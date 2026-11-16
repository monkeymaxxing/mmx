package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"mmx/internal/config"
	"mmx/internal/engine"
	"mmx/internal/modules"
	"mmx/internal/protocol"
	"mmx/internal/value"
)

// runLocally runs each node job with the node task execution engine in this
// process, through the same secret delivery and private transport as a node
// job. fail interrupts the named entries before they run.
func runLocally(c *Controller, attempts map[string]int, fail func(fleetStage, node string) bool) {
	var mu sync.Mutex
	c.executeNode = func(ctx context.Context, r *Run, fleetStage config.FleetStage, en Entry, _ bool, _ func(Entry) error) (Entry, error) {
		mu.Lock()
		attempts[fleetStage.ID+"/"+en.Node]++
		mu.Unlock()
		if fail != nil && fail(fleetStage.ID, en.Node) {
			en.Response = protocol.Response{Status: "failed", Error: "fixture interruption"}
			return en, nil
		}
		j, err := scopeJobSecrets(mustJob(nodeJob(r, fleetStage, en)))
		if err != nil {
			en.Response = protocol.Response{Status: preparationFailure(err), Error: err.Error()}
			return en, nil
		}
		en.privateInputs = j.PrivateInputs
		wire, err := j.Transport()
		if err != nil {
			return en, err
		}
		var received config.Job
		if err := protocol.Decode(wire, &received); err != nil {
			return en, err
		}
		response := (&engine.Engine{}).Run(ctx, received)
		// The controller receives private outputs on the authenticated link.
		answer, err := json.Marshal(response.Transport())
		if err != nil {
			return en, err
		}
		if err := protocol.Decode(answer, &en.Response); err != nil {
			return en, err
		}
		return en, nil
	}
}

// readFileNode makes node a reachable through a fake verified SSH connection
// whose runner answers read-file requests with reply, and logs each request.
// "<log> connections" logs every command the fake ssh receives.
func readFileNode(t *testing.T, c *Controller, r *Run, reply string) string {
	t.Helper()
	binary := []byte("runner")
	sum := sha256.Sum256(binary)
	log := filepath.Join(t.TempDir(), "read-file requests")
	dir := fakeSSH(t, `case "$1" in -G) printf 'hostname fixture\nuser root\nport 22\n'; exit 0;; -F) exit 0;; esac
for command; do :; done
echo "$command" >> `+quote(log+" connections")+`
case "$command" in
*machine-id*) printf 'Linux\nx86_64\n0\nroot\n0b0b0b0b-0000-4000-8000-000000000000\nsystemd\n0123456789abcdef0123456789abcdef\n\n10.0.0.1 5000 10.0.0.2 22\n';;
*sha256sum*) echo `+hex.EncodeToString(sum[:])+`;;
*read-file*) cat >> `+quote(log)+`; echo >> `+quote(log)+`; `+reply+`;;
*) echo "unexpected runner command" >&2; exit 1;;
esac
`)
	if err := os.WriteFile(filepath.Join(dir, "mmx-runner-linux-amd64"), binary, 0700); err != nil {
		t.Fatal(err)
	}
	c.RunnerDir = dir
	r.Inventory.Nodes["a"] = config.Node{Address: "a", Transport: "ssh_only"}
	// The fixture created the run before this inventory change.
	if err := c.createRun(r); err != nil {
		t.Fatal(err)
	}
	return log
}

func noSecretIn(t *testing.T, dir string, values ...string) {
	t.Helper()
	filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, _ := os.ReadFile(path)
		for _, v := range values {
			if strings.Contains(string(b), v) {
				t.Errorf("secret %s in %s", v, path)
			}
		}
		return nil
	})
}

func boolean(b bool) *bool { return &b }

// The design's join token: read on one node with read_file, consumed by other
// nodes. The value passes only through the controller process's memory; a
// resume reads the file again from its node, once, before the work that
// consumes it, instead of running the producer again.
func TestResumeReadsAReadFileSecretAgainFromItsNode(t *testing.T) {
	token := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(token, []byte("first-join-token"), 0600); err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	content := "{{ .outputs.start_first_server.a.join_token }}"
	c, r := fleet(t,
		config.FleetStage{ID: "start_first_server", Nodes: "a", NodeTasks: []config.Task{{ID: "join_token", Module: "read_file", ModuleInputs: &modules.ReadFile{Path: token, ContentIsSecret: boolean(true)}}},
			Outputs: map[string]any{"join_token": "{{ .node_tasks.join_token.content }}"}},
		config.FleetStage{ID: "join_other_servers", Nodes: []any{"b", "c"}, NodeTasks: []config.Task{{Module: "file", ModuleInputs: &modules.File{Path: filepath.Join(out, "{{ .node.name }}"), Content: &content, Mode: "0600"}}}},
	)
	attempts := map[string]int{}
	failing := true
	runLocally(&c, attempts, func(fleetStage, _ string) bool { return fleetStage == "join_other_servers" && failing })
	log := readFileNode(t, &c, r, `printf '{"content":"%s"}\n' "$(cat `+quote(token)+`)"`)
	execute(t, c, r, false)
	if r.Status != "failed" {
		t.Fatal(r.Status)
	}
	if display, _ := json.Marshal(value.Display(entry(t, r, "start_first_server", "a").Response.Outputs)); string(display) != `{"join_token":"[redacted]"}` {
		t.Fatal(string(display))
	}
	// The node generated a new token since: the resume reads the current file.
	if err := os.WriteFile(token, []byte("current-join-token"), 0600); err != nil {
		t.Fatal(err)
	}
	r, err := c.Load(r.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	failing = false
	execute(t, c, r, true)
	if r.Status != "succeeded" || attempts["start_first_server/a"] != 1 {
		t.Fatal(r.Status, attempts, r.Entries)
	}
	for _, node := range []string{"b", "c"} {
		if b, err := os.ReadFile(filepath.Join(out, node)); err != nil || string(b) != "current-join-token" {
			t.Fatal(node, string(b), err)
		}
	}
	requests, _ := os.ReadFile(log)
	if strings.Count(string(requests), `"path":"`+token+`"`) != 1 {
		t.Fatal("the file was not read again exactly once:", string(requests))
	}
	noSecretIn(t, c.State, "first-join-token", "current-join-token")
}

// When the node cannot give the file again, the work that consumes it is
// blocked with the reason, and no other work is affected.
func TestResumeBlocksConsumersWhenAReadFileSecretCannotBeReadAgain(t *testing.T) {
	token := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(token, []byte("lost-join-token"), 0600); err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	content := "{{ .outputs.start_first_server.a.join_token }}"
	c, r := fleet(t,
		config.FleetStage{ID: "start_first_server", Nodes: "a", NodeTasks: []config.Task{{ID: "join_token", Module: "read_file", ModuleInputs: &modules.ReadFile{Path: token, ContentIsSecret: boolean(true)}}},
			Outputs: map[string]any{"join_token": "{{ .node_tasks.join_token.content }}"}},
		config.FleetStage{ID: "join_other_servers", Nodes: "b", NodeTasks: []config.Task{{Module: "file", ModuleInputs: &modules.File{Path: filepath.Join(out, "joined"), Content: &content, Mode: "0600"}}}},
		config.FleetStage{ID: "unrelated", Nodes: "c", NodeTasks: []config.Task{{Show: "independent"}}},
	)
	attempts := map[string]int{}
	failing := true
	runLocally(&c, attempts, func(fleetStage, _ string) bool { return fleetStage != "start_first_server" && failing })
	readFileNode(t, &c, r, `echo "file does not exist: `+token+`" >&2; exit 1`)
	execute(t, c, r, false)
	r, err := c.Load(r.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	failing = false
	execute(t, c, r, true)
	consumer := entry(t, r, "join_other_servers", "b").Response
	if consumer.Status != "blocked" || !strings.Contains(consumer.Error, "secret from read_file task join_token of fleet stage start_first_server") || !strings.Contains(consumer.Error, "could not read "+token+" on node a again") || !strings.Contains(consumer.Error, "file does not exist") {
		t.Fatal(consumer)
	}
	if entry(t, r, "unrelated", "c").Response.Status != "succeeded" || attempts["start_first_server/a"] != 1 {
		t.Fatal(r.Entries)
	}
	if _, err := os.Stat(filepath.Join(out, "joined")); !os.IsNotExist(err) {
		t.Fatal("the consumer ran without its secret")
	}
	noSecretIn(t, c.State, "lost-join-token")
}

// A shell task's secret output cannot be read again: on resume, the work that
// consumes it is blocked, naming the producing fleet stage and task, and the
// completed command does not run again.
func TestResumeBlocksConsumersOfAShellSecret(t *testing.T) {
	out := t.TempDir()
	content := "{{ .outputs.create_token.a.token }}"
	c, r := fleet(t,
		config.FleetStage{ID: "create_token", Nodes: "a", NodeTasks: []config.Task{{ID: "new_token", Module: "shell", ModuleInputs: &modules.Shell{Script: "echo c2hlbGwtdG9rZW4tc2VudGluZWw= | base64 -d", OutputIsSecret: true, ChangedWhen: false}}},
			Outputs: map[string]any{"token": "{{ .node_tasks.new_token.stdout }}"}},
		config.FleetStage{ID: "use_token", Nodes: "b", NodeTasks: []config.Task{{Module: "file", ModuleInputs: &modules.File{Path: filepath.Join(out, "token"), Content: &content, Mode: "0600"}}}},
	)
	attempts := map[string]int{}
	failing := true
	runLocally(&c, attempts, func(fleetStage, _ string) bool { return fleetStage == "use_token" && failing })
	execute(t, c, r, false)
	if r.Status != "failed" {
		t.Fatal(r.Status)
	}
	if results := entry(t, r, "create_token", "a").Response.Results; results[0].Safe().Stdout != "[redacted]" {
		t.Fatal(results)
	}
	r, err := c.Load(r.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	failing = false
	execute(t, c, r, true)
	consumer := entry(t, r, "use_token", "b").Response
	if consumer.Status != "blocked" || !strings.Contains(consumer.Error, "node task new_token of fleet stage create_token on node a") || !strings.Contains(consumer.Error, "start a new mmx run") {
		t.Fatal(consumer)
	}
	if attempts["create_token/a"] != 1 {
		t.Fatal("the completed command ran again", attempts)
	}
	if _, err := os.Stat(filepath.Join(out, "token")); !os.IsNotExist(err) {
		t.Fatal("the consumer ran without its secret")
	}
	noSecretIn(t, c.State, "shell-token-sentinel")
}

// A file saved on the controller machine that is still missing on resume
// gets a read_file secret by reading the file again from its node.
func TestResumeSavesAReadFileSecretOnTheControllerMachineFromANewRead(t *testing.T) {
	token := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(token, []byte("kubeconfig-sentinel"), 0600); err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "credentials"), []byte("a file in the way"), 0644); err != nil {
		t.Fatal(err)
	}
	c, r := fleet(t, config.FleetStage{ID: "fetch", Nodes: "a",
		NodeTasks: []config.Task{{ID: "kubeconfig", Module: "read_file", ModuleInputs: &modules.ReadFile{Path: token, ContentIsSecret: boolean(true)}}},
		Outputs:   map[string]any{"kubeconfig": "{{ .node_tasks.kubeconfig.content }}"},
		SaveOnControllerMachine: []config.SaveOnControllerMachine{
			{Output: "kubeconfig", Path: "credentials/{{ .node.name }}.yaml", PermissionsMode: "0600", ProjectRoot: root},
		}})
	attempts := map[string]int{}
	runLocally(&c, attempts, nil)
	log := readFileNode(t, &c, r, `printf '{"content":"%s"}\n' "$(cat `+quote(token)+`)"`)
	execute(t, c, r, false)
	if r.Status != "failed" {
		t.Fatal(r.Status)
	}
	if err := os.Remove(filepath.Join(root, "credentials")); err != nil {
		t.Fatal(err)
	}
	r, err := c.Load(r.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	execute(t, c, r, true)
	if r.Status != "succeeded" || attempts["fetch/a"] != 1 {
		t.Fatal(r.Status, attempts)
	}
	b, err := os.ReadFile(filepath.Join(root, "credentials/a.yaml"))
	info, _ := os.Stat(filepath.Join(root, "credentials/a.yaml"))
	if err != nil || string(b) != "kubeconfig-sentinel" || info.Mode().Perm() != 0600 {
		t.Fatal(string(b), err)
	}
	if requests, _ := os.ReadFile(log); strings.Count(string(requests), token) != 1 {
		t.Fatal(string(requests))
	}
	noSecretIn(t, c.State, "kubeconfig-sentinel")
}

// A fleet stage's when that the controller process checks never quotes a
// secret in its error.
func TestWhenErrorsNeverQuoteASecret(t *testing.T) {
	c, r := fleet(t, config.FleetStage{ID: "check", Nodes: "a", When: map[string]any{"number_at_least": []any{"{{ .secrets.token }}", 1}}, NodeTasks: []config.Task{{Show: "hi"}}})
	r.Project.Secrets = map[string]string{"token": "MMX_TEST_WHEN_SECRET"}
	if err := c.createRun(r); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MMX_TEST_WHEN_SECRET", "when-secret-sentinel")
	runLocally(&c, map[string]int{}, nil)
	execute(t, c, r, false)
	if en := entry(t, r, "check", "a"); en.Response.Status != "failed" || !strings.Contains(en.Response.Error, "a secret value is not a number") {
		t.Fatal(en.Response)
	}
	noSecretIn(t, c.State, "when-secret-sentinel")
}

// A resume reads read_file secrets again before it schedules anything, and a
// stop ends a read that hangs instead of waiting out its time limit.
func TestStopEndsAReadFileSecretReadAgain(t *testing.T) {
	out := t.TempDir()
	token := filepath.Join(out, "token")
	if err := os.WriteFile(token, []byte("hanging-token"), 0600); err != nil {
		t.Fatal(err)
	}
	content := "{{ .outputs.start_first_server.a.join_token }}"
	c, r := fleet(t,
		config.FleetStage{ID: "start_first_server", Nodes: "a", NodeTasks: []config.Task{{ID: "join_token", Module: "read_file", ModuleInputs: &modules.ReadFile{Path: token, ContentIsSecret: boolean(true)}}},
			Outputs: map[string]any{"join_token": "{{ .node_tasks.join_token.content }}"}},
		config.FleetStage{ID: "join_other_servers", Nodes: "b", NodeTasks: []config.Task{{Module: "file", ModuleInputs: &modules.File{Path: filepath.Join(out, "joined"), Content: &content, Mode: "0600"}}}},
	)
	failing := true
	runLocally(&c, map[string]int{}, func(fleetStage, _ string) bool { return fleetStage == "join_other_servers" && failing })
	// exec: the fake ssh is this shell; ending it must end the read, as
	// ending a real ssh does.
	readStarted := filepath.Join(out, "secret-read-started")
	readFileNode(t, &c, r, `touch `+quote(readStarted)+`; exec sleep 60`)
	execute(t, c, r, false)
	r, err := c.Load(r.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	failing = false
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	var executionError error
	go func() {
		defer close(done)
		executionError = c.Execute(ctx, r, true)
	}()
	defer func() { cancel(); <-done }()
	// Resume clears an old stop request before rereading secrets. Do not send
	// the new stop until the node's secret reader has actually started.
	startupDeadline := time.NewTimer(30 * time.Second)
	defer startupDeadline.Stop()
	readPoll := time.NewTicker(10 * time.Millisecond)
	defer readPoll.Stop()
	for {
		if _, err := os.Stat(readStarted); err == nil {
			break
		} else if !os.IsNotExist(err) {
			t.Fatal(err)
		}
		select {
		case <-done:
			t.Fatal("resume ended before the secret read started", executionError)
		case <-startupDeadline.C:
			t.Fatal("secret read did not start")
		case <-readPoll.C:
		}
	}
	if err := c.requestStop(r.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
		if executionError != nil {
			t.Fatal(executionError)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the stop waited for the secret read")
	}
	if consumer := entry(t, r, "join_other_servers", "b").Response; consumer.Status != "stopped" || r.Status != "stopped" {
		t.Fatal(r.Status, consumer)
	}
}

// A read_file secret published by a completed loop iteration is read again
// only from the machine its node job ran on: a replaced node is refused.
func TestResumeRefusesAReplacedNodeForALoopIterationsReadFileSecret(t *testing.T) {
	token := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(token, []byte("loop-token-sentinel"), 0600); err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	content := "{{ .outputs.each.first.token }}"
	c, r := fleet(t,
		config.FleetStage{ID: "each", Loop: []any{"first"}, As: "item", Key: "{{ .item }}", Outputs: map[string]any{"token": "{{ .outputs.produce.a.token }}"}, FleetStages: []config.FleetStage{
			{ID: "produce", Nodes: "a", NodeTasks: []config.Task{{ID: "token", Module: "read_file", ModuleInputs: &modules.ReadFile{Path: token, ContentIsSecret: boolean(true)}}},
				Outputs: map[string]any{"token": "{{ .node_tasks.token.content }}"}},
		}},
		config.FleetStage{ID: "consume", Nodes: "b", NodeTasks: []config.Task{{Module: "file", ModuleInputs: &modules.File{Path: filepath.Join(out, "token"), Content: &content, Mode: "0600"}}}},
	)
	failing := true
	runLocally(&c, map[string]int{}, func(fleetStage, _ string) bool { return fleetStage == "consume" && failing })
	readFileNode(t, &c, r, `printf '{"content":"%s"}\n' "$(cat `+quote(token)+`)"`)
	execute(t, c, r, false)
	// The producing node job ran on another machine than the one the node's
	// address reaches now.
	for _, en := range r.Entries {
		if en.FleetStage == "each[first]/produce" {
			en.NodeID = "fedcba9876543210fedcba9876543210"
			if err := c.saveEntry(r.ID, en); err != nil {
				t.Fatal(err)
			}
		}
	}
	r, err := c.Load(r.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	failing = false
	execute(t, c, r, true)
	consumer := entry(t, r, "consume", "b").Response
	if consumer.Status != "blocked" || !strings.Contains(consumer.Error, "node was replaced") {
		t.Fatal(consumer)
	}
	if _, err := os.Stat(filepath.Join(out, "token")); !os.IsNotExist(err) {
		t.Fatal("the secret was read from a replaced node")
	}
}

// joinFixture is the join token example with a third fleet stage, on node c,
// that fails until failing is false. The fake SSH connection to node a hangs
// on any read-file request.
func joinFixture(t *testing.T, failing *bool, failOn func(fleetStage, node string) bool, extra ...config.FleetStage) (Controller, *Run, string, string) {
	t.Helper()
	token := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(token, []byte("join-token"), 0600); err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	content := "{{ .outputs.start_first_server.a.join_token }}"
	stages := append([]config.FleetStage{
		{ID: "start_first_server", Nodes: "a", NodeTasks: []config.Task{{ID: "join_token", Module: "read_file", ModuleInputs: &modules.ReadFile{Path: token, ContentIsSecret: boolean(true)}}},
			Outputs: map[string]any{"join_token": "{{ .node_tasks.join_token.content }}"}},
		{ID: "join_other_servers", Nodes: "b", NodeTasks: []config.Task{{Module: "file", ModuleInputs: &modules.File{Path: filepath.Join(out, "joined"), Content: &content, Mode: "0600"}}}},
	}, extra...)
	stages = append(stages, config.FleetStage{ID: "unrelated", Nodes: "c", NodeTasks: []config.Task{{Show: "independent"}}})
	c, r := fleet(t, stages...)
	runLocally(&c, map[string]int{}, func(fleetStage, node string) bool { return *failing && failOn(fleetStage, node) })
	log := readFileNode(t, &c, r, `exec sleep 60`)
	return c, r, log, out
}

// resumeFast resumes r and fails if a node read made it wait.
func resumeFast(t *testing.T, c Controller, r *Run) *Run {
	t.Helper()
	r, err := c.Load(r.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	execute(t, c, r, true)
	if elapsed := time.Since(started); elapsed > 10*time.Second {
		t.Fatal("the resume waited for a node:", elapsed)
	}
	return r
}

// A resume whose unfinished work reads no read_file secret connects to no
// node for one: the producer's node may be gone for good.
func TestResumeReadsNoSecretThatNoUnfinishedWorkNeeds(t *testing.T) {
	failing := true
	c, r, log, out := joinFixture(t, &failing, func(fleetStage, _ string) bool { return fleetStage == "unrelated" })
	execute(t, c, r, false)
	if r.Status != "failed" || entry(t, r, "join_other_servers", "b").Response.Status != "succeeded" {
		t.Fatal(r.Status, r.Entries)
	}
	failing = false
	r = resumeFast(t, c, r)
	if r.Status != "succeeded" {
		t.Fatal(r.Status, r.Entries)
	}
	if b, _ := os.ReadFile(log + " connections"); len(b) != 0 {
		t.Fatal("the resume connected to the producer's node:", string(b))
	}
	if b, _ := os.ReadFile(filepath.Join(out, "joined")); string(b) != "join-token" {
		t.Fatal(string(b))
	}
}

// A loop iteration that is not completed reads the secret; completed
// iterations do not need it.
func TestResumeReadsASecretForAnUnfinishedLoopIterationOnly(t *testing.T) {
	content := "{{ .outputs.start_first_server.a.join_token }}"
	loop := func(out string) config.FleetStage {
		return config.FleetStage{ID: "each", Loop: []any{"x", "y"}, As: "item", Key: "{{ .item }}", FleetStages: []config.FleetStage{
			{ID: "use", Nodes: "b", NodeTasks: []config.Task{{Module: "file", ModuleInputs: &modules.File{Path: filepath.Join(out, "{{ .item }}"), Content: &content, Mode: "0600"}}}},
		}}
	}
	for _, tc := range []struct {
		name       string
		failOn     string
		wantReads  int
		wantStatus string
	}{
		// The stop keeps iteration y's earlier failure.
		{"iteration y unfinished", "each[y]/use", 1, "failed"},
		{"every iteration completed", "unrelated", 0, "succeeded"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			failing, out := true, t.TempDir()
			c, r, log, _ := joinFixture(t, &failing, func(fleetStage, _ string) bool { return fleetStage == tc.failOn }, loop(out))
			execute(t, c, r, false)
			failing = false
			if tc.wantReads > 0 {
				// The read hangs, so a stop ends this resume once it has asked.
				// Stop the run when the node was asked for the secret, which it
				// never answers; a fixed pause was too short on a busy machine.
				go func() {
					for deadline := time.Now().Add(time.Minute); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
						if requests, _ := os.ReadFile(log); strings.Contains(string(requests), `"path"`) {
							break
						}
					}
					c.requestStop(r.ID)
				}()
			}
			r = resumeFast(t, c, r)
			requests, _ := os.ReadFile(log)
			if reads := strings.Count(string(requests), `"path"`); reads != tc.wantReads || r.Status != tc.wantStatus {
				t.Fatal(reads, r.Status)
			}
		})
	}
}

// A file save_on_controller_machine must write again reads the secret it
// holds; a file the run wrote and that still exists needs no read.
func TestResumeReadsASecretForAControllerMachineFileStillMissing(t *testing.T) {
	for _, deleted := range []bool{true, false} {
		t.Run(fmt.Sprint("deleted ", deleted), func(t *testing.T) {
			root := t.TempDir()
			failing := true
			c, r, log, _ := joinFixture(t, &failing, func(fleetStage, _ string) bool { return fleetStage == "unrelated" })
			r.Project.FleetStages[0].SaveOnControllerMachine = []config.SaveOnControllerMachine{{Output: "join_token", Path: "credentials/{{ .node.name }}", PermissionsMode: "0600", ProjectRoot: root}}
			if err := c.createRun(r); err != nil {
				t.Fatal(err)
			}
			execute(t, c, r, false)
			saved := filepath.Join(root, "credentials/a")
			if b, err := os.ReadFile(saved); err != nil || string(b) != "join-token" {
				t.Fatal(string(b), err)
			}
			if deleted {
				os.Remove(saved)
				// The stop must come after the resume sent its read, however
				// slow the machine is: wait for the read in the request log,
				// not for a fixed time.
				id := r.ID
				go func() {
					for deadline := time.Now().Add(time.Minute); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
						if requests, _ := os.ReadFile(log); strings.Contains(string(requests), `"path"`) {
							break
						}
					}
					c.requestStop(id)
				}()
			}
			failing = false
			r = resumeFast(t, c, r)
			requests, _ := os.ReadFile(log)
			if reads := strings.Count(string(requests), `"path"`); (reads == 1) != deleted {
				t.Fatal(reads)
			}
			if deleted {
				// The read never answered: the file is blocked, not written.
				if file := r.Selections["start_first_server"].ControllerMachineFiles[0]; file.Result.Status != "blocked" {
					t.Fatal(file)
				}
			} else if r.Status != "succeeded" {
				t.Fatal(r.Status)
			}
		})
	}
}
