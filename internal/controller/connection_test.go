package controller

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"mmx/internal/access"
	"mmx/internal/assets"
	"mmx/internal/config"
	"mmx/internal/differences"
	"mmx/internal/endpoint"
	"mmx/internal/modules"
	"mmx/internal/protocol"
	"mmx/internal/release"
	"mmx/internal/value"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// fakeSSH puts a stand-in for ssh that runs script first on PATH. Every
// stand-in answers the node runner executable's version request itself, with
// this build's version or the content of the file runner-version beside it,
// so that script sees only the requests its test is about.
func fakeSSH(t *testing.T, script string) string {
	t.Helper()
	dir := t.TempDir()
	version := `for last; do :; done
case "$last" in */mmx-runner*" version' sh "*)
  if test -e "$(dirname "$0")/runner-version"; then version=$(cat "$(dirname "$0")/runner-version"); else version=` + release.Version + `; fi
  printf '{"protocol":1,"version":"%s"}\n' "$version"; exit 0;;
esac
`
	writeProgramFileThroughShellChild(t, filepath.Join(dir, "ssh"), "#!/bin/sh\n"+version+script)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("HOME", dir)
	return dir
}

// The test binary is ssh's askpass program, as the mmx binary is in use, so a
// fake ssh can ask for a password through the session's askpass socket.
func TestMain(m *testing.M) {
	if socket := os.Getenv("MMX_ASKPASS_SOCKET"); socket != "" {
		if err := access.RunAskpass(socket, os.Args[len(os.Args)-1]); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// sshNode answers a verified SSH connection to node a with a fixed identity and
// installed runner; runner is the shell reply to every runner command.
func sshNode(t *testing.T, runner string) (Controller, *Run) {
	t.Helper()
	binary := []byte("runner")
	sum := sha256.Sum256(binary)
	dir := fakeSSH(t, `case "$1" in -G) printf 'hostname fixture\nuser root\nport 22\n'; exit 0;; -F) exit 0;; esac
for command; do :; done
case "$command" in
*confirm-reachability*) `+runner+`;;
*machine-id*) printf 'Linux\nx86_64\n0\nroot\n0b0b0b0b-0000-4000-8000-000000000000\nsystemd\n0123456789abcdef0123456789abcdef\n\n10.0.0.1 5000 10.0.0.2 22\n';;
*sha256sum*) echo `+hex.EncodeToString(sum[:])+`;;
*) `+runner+`;;
esac
`)
	if err := os.WriteFile(filepath.Join(dir, "mmx-runner-linux-amd64"), binary, 0700); err != nil {
		t.Fatal(err)
	}
	c, r := fleet(t, config.FleetStage{ID: "setup", Nodes: "a"})
	c.RunnerDir = dir
	r.Inventory.Nodes["a"] = config.Node{Address: "a", Transport: "ssh_only"}
	return c, r
}

// A managed node reaches askpass through any prompt ssh forwards. Only
// OpenSSH's own passphrase prompt for a key file this session gave ssh gets a
// local key passphrase, and ssh does not forward server-written prompts.
func TestAskpassGivesKeyPassphraseOnlyToOpenSSHPromptForSessionKey(t *testing.T) {
	trace := filepath.Join(t.TempDir(), "arguments")
	fakeSSH(t, `printf '%s\n' "$@" >> `+quote(trace)+`
for command; do :; done
case "$command" in *machine-id*) printf 'Linux\nx86_64\n0\nroot\n0b0b0b0b-0000-4000-8000-000000000000\nsystemd\n0123456789abcdef0123456789abcdef\n\n10.0.0.1 5000 10.0.0.2 22\n';; esac
`)
	key := "/home/admin/.ssh/id_ed25519"
	credentials := &access.Credentials{Interactive: true, Prompt: func(string) (string, error) { return "local-key-passphrase", nil }}
	// An earlier connection unlocked the administrator's encrypted keys.
	for _, path := range []string{key, "/home/admin/.ssh/other"} {
		if _, err := credentials.Get("SSH key passphrase", path, ""); err != nil {
			t.Fatal(err)
		}
	}
	credentials.Prompt = func(label string) (string, error) {
		if strings.HasPrefix(label, "SSH password") {
			return "target-password", nil
		}
		return "", fmt.Errorf("unexpected prompt %s", label)
	}
	s, err := openSSH(context.Background(), config.Node{Address: "fixture"}, credentials, nil, "", []string{key}, "admin@fixture:22")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for prompt, want := range map[string]string{
		"Enter passphrase for key '" + key + "': ":                  "local-key-passphrase",
		"(admin@fixture) Enter passphrase for key '" + key + "': ":  "",
		"Enter passphrase for key '/home/admin/.ssh/other': ":       "",
		"Server passphrase check: Enter passphrase for key '" + key: "",
		// A ProxyJump host's password prompt must not receive the target's password.
		"jump@bastion's password: ":  "",
		"admin@fixture's password: ": "target-password",
	} {
		conn, err := net.Dial("unix", s.askpassPath)
		if err != nil {
			t.Fatal(err)
		}
		var reply struct{ Value, Error string }
		if err := json.NewEncoder(conn).Encode(prompt); err != nil || json.NewDecoder(conn).Decode(&reply) != nil {
			t.Fatal(err)
		}
		conn.Close()
		if reply.Value != want || (want == "") != (reply.Error != "") {
			t.Fatalf("%q: %+v", prompt, reply)
		}
	}
	arguments, _ := os.ReadFile(trace)
	if !strings.Contains(string(arguments), "KbdInteractiveAuthentication=no") {
		t.Fatal("ssh may forward keyboard-interactive prompts")
	}
}

func TestNodeRejectionFailsAttemptButLostTransportStaysUncertain(t *testing.T) {
	for _, tc := range []struct{ name, runner, want string }{
		{"rejected", `echo 'node busy with job other' >&2; exit 1`, "failed"},
		{"transport lost", `echo 'Connection reset by peer' >&2; exit 255`, "unknown"},
		{"runner crashed", `echo 'panic: runtime error' >&2; exit 2`, "unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, r := sshNode(t, tc.runner)
			en := r.Entries[0]
			en.scope = value.Scope{}
			en, err := c.executeEntry(context.Background(), r, r.Project.FleetStages[0], en, false, func(Entry) error { return nil })
			if err != nil || en.JobID == "" || en.NodeJobAttempt != 1 || en.Response.Status != tc.want {
				t.Fatal(en.Response, err)
			}
		})
	}
}

func TestRetryKeepsUnansweredJobUncertainWhenNodeIsUnreachable(t *testing.T) {
	fakeSSH(t, "echo 'ssh: connect to node a port 22: Connection refused' >&2\nexit 255\n")
	c, r := fleet(t, config.FleetStage{ID: "setup", Nodes: "a"})
	id := strings.Repeat("a", 64)
	en := r.Entries[0]
	en.JobID, en.NodeJobAttempt, en.Response = id, 1, protocol.Response{ID: id, Status: "not_found"}
	en, err := c.executeEntry(context.Background(), r, r.Project.FleetStages[0], en, true, func(Entry) error { return nil })
	if err != nil || en.JobID != id || en.NodeJobAttempt != 1 || en.Response.Status != "unknown" {
		t.Fatal(en.JobID, en.Response, err)
	}
}

// A failed ProxyJump or ProxyCommand ends ssh with status 255 and no text. The
// error says where to look and does not claim that a proxy failed.
func TestSSHFailureWithoutTextSaysWhereToLook(t *testing.T) {
	_, session := fakeRunner(t, strings.Repeat("a", 64), "", 1, "exit 255")
	_, err := session.ssh(context.Background(), "true")
	want := "SSH fixture: exit status 255: OpenSSH printed no reason. If this node's SSH configuration uses ProxyJump or ProxyCommand, OpenSSH prints nothing about a failure there while a shared connection is being set up. Run ssh -v to this node to see the reason"
	var failure *sshError
	if err == nil || err.Error() != want || !errors.As(err, &failure) || !failure.transient {
		t.Fatal(err)
	}
}

// Refusing a changed machine keeps the previous job's response, with its
// results, as an earlier attempt.
func TestRetryRefusingChangedNodeKeepsPreviousAttempt(t *testing.T) {
	for _, changed := range []string{"identity", "architecture"} {
		t.Run(changed, func(t *testing.T) {
			c, r := sshNode(t, `echo unexpected runner call >&2; exit 1`)
			previous := protocol.Response{ID: "old", Status: "failed", Results: []protocol.Result{{Task: "check", Status: "failed"}}}
			en := r.Entries[0]
			en.JobID, en.NodeJobAttempt, en.Response = "old", 1, previous
			if changed == "identity" {
				en.NodeID = "another machine"
			} else {
				en.Architecture = "arm64"
			}
			en, err := c.executeEntry(context.Background(), r, r.Project.FleetStages[0], en, true, func(Entry) error { return nil })
			if err != nil || en.JobID != "" || en.Response.Status != "failed" || !strings.Contains(en.Response.Error, changed+" changed") {
				t.Fatal(en.JobID, en.Response, err)
			}
			if !reflect.DeepEqual(en.PreviousNodeJobAttempts, []Attempt{{JobID: "old", Response: previous}}) {
				t.Fatalf("previous attempt lost: %+v", en.PreviousNodeJobAttempts)
			}
		})
	}
}

// retryFixture is a saved run whose node a lost contact after job old, the
// second attempt, was published; the first attempt failed and was saved.
// The verified SSH runner appends each command to trace, answers the nth
// status or progress request with the nth answer (the last repeats), answers
// that it does not refuse a reboot, and admits the next attempt's job as
// succeeded.
type retryFixture struct {
	c          Controller
	run        *Run
	trace      string
	old, next  string
	first      Attempt
	uncertain  protocol.Response
	nextResult protocol.Response
}

func newRetryFixture(t *testing.T, saved string, answers ...string) *retryFixture {
	t.Helper()
	dir := t.TempDir()
	f := &retryFixture{trace: filepath.Join(dir, "trace"), old: strings.Repeat("a", 64)}
	f.c, f.run = sshNode(t, `printf '%s\n' "$command" >> `+quote(f.trace)+`
case "$command" in
*" status "*|*" progress"*) cat >/dev/null; cat `+quote(dir)+`/status-$(grep -c -E ' (status |progress)' `+quote(f.trace)+`) 2>/dev/null || cat `+quote(dir)+`/status-last;;
*" reboot-refusal"*) cat >/dev/null; echo '{"refusal":""}';;
*" submit"*) cat >/dev/null; cat `+quote(dir)+`/submit;;
*) echo unexpected runner command >&2; exit 1;;
esac`)
	write := func(name string, response protocol.Response) {
		data, err := json.Marshal(response)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	for i, status := range answers {
		answer := protocol.Response{ID: f.old, Status: status}
		switch {
		case status == "not_found":
			answer.Error = "node history is unavailable for job " + f.old
		case status == "rebooting":
			answer.RebootFrom = "boot-1"
		case protocol.Terminal(status):
			answer.Change = "changed"
		}
		write(fmt.Sprintf("status-%d", i+1), answer)
		write("status-last", answer)
	}
	next := sha256.Sum256([]byte(f.run.ID + "/setup/a/3"))
	f.next = hex.EncodeToString(next[:])
	f.nextResult = protocol.Response{ID: f.next, Status: "succeeded", Change: "unchanged"}
	write("submit", f.nextResult)
	node := sha256.Sum256([]byte("0123456789abcdef0123456789abcdef"))
	f.first = Attempt{JobID: "first", Response: protocol.Response{ID: "first", Status: "failed"}}
	f.uncertain = protocol.Response{ID: f.old, Status: saved, Error: "SSH a: connection lost"}
	en := f.run.Entries[0]
	en.PreviousNodeJobAttempts = []Attempt{f.first}
	en.JobID, en.NodeJobAttempt, en.Response = f.old, 2, f.uncertain
	en.NodeID, en.Architecture, en.Transport = hex.EncodeToString(node[:]), "amd64", "ssh"
	f.run.Entries[0] = en
	if err := f.c.saveEntry(f.run.ID, en); err != nil {
		t.Fatal(err)
	}
	return f
}

// retry runs mmx resume through the scheduler and returns the saved entry.
func (f *retryFixture) retry(t *testing.T) Entry {
	t.Helper()
	execute(t, f.c, f.run, true)
	saved, err := f.c.Load(f.run.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Status != f.run.Status {
		t.Fatalf("saved run is %s, scheduler reported %s", saved.Status, f.run.Status)
	}
	return saved.Entries[0]
}

// task blocks names the runner commands the node received, in order.
func (f *retryFixture) taskBlocks(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, line := range lines(t, f.trace) {
		switch {
		case strings.Contains(line, " status ") && strings.Contains(line, f.old):
			out = append(out, "status old")
		case strings.Contains(line, " progress"):
			out = append(out, "progress")
		case strings.Contains(line, " reboot-refusal"):
			out = append(out, "reboot-refusal")
		case strings.Contains(line, " submit"):
			out = append(out, "submit")
		default:
			out = append(out, line)
		}
	}
	return out
}

// A retry asks the node about an uncertain job before anything else. When
// that job succeeded, the entry takes its result: no new attempt,
// no submit, and the earlier attempts stay as they were.
func TestRetryReconcilesUncertainJobThatSucceeded(t *testing.T) {
	for _, saved := range []string{"unknown", "running", "not_found"} {
		t.Run(saved, func(t *testing.T) {
			f := newRetryFixture(t, saved, "succeeded")
			en := f.retry(t)
			if ops := f.taskBlocks(t); !reflect.DeepEqual(ops, []string{"status old"}) {
				t.Fatalf("node task_blocks %q", ops)
			}
			if en.JobID != f.old || en.NodeJobAttempt != 2 || en.Response.Status != "succeeded" || en.Response.Change != "changed" || !reflect.DeepEqual(en.PreviousNodeJobAttempts, []Attempt{f.first}) {
				t.Fatalf("%s attempt %d %+v, attempts %+v", en.JobID, en.NodeJobAttempt, en.Response, en.PreviousNodeJobAttempts)
			}
			if f.run.Status != "succeeded" {
				t.Fatal(f.run.Status)
			}
		})
	}
}

// A job the node is still running is polled to its outcome, which is saved;
// the retry never submits a second job beside it.
func TestRetryPollsStillRunningJobToItsOutcome(t *testing.T) {
	f := newRetryFixture(t, "unknown", "running", "running", "succeeded")
	en := f.retry(t)
	if ops := f.taskBlocks(t); !reflect.DeepEqual(ops, []string{"status old", "progress", "progress"}) {
		t.Fatalf("node task_blocks %q", ops)
	}
	if en.JobID != f.old || en.NodeJobAttempt != 2 || en.Response.Status != "succeeded" || !reflect.DeepEqual(en.PreviousNodeJobAttempts, []Attempt{f.first}) {
		t.Fatalf("%s attempt %d %+v, attempts %+v", en.JobID, en.NodeJobAttempt, en.Response, en.PreviousNodeJobAttempts)
	}
	if f.run.Status != "succeeded" {
		t.Fatal(f.run.Status)
	}
}

// rebootFixture is a retry fixture whose fleet stage ends with a reboot and whose old
// job requested it; the reboot wait polls quickly.
func rebootFixture(t *testing.T, saved, timeout string, answers ...string) *retryFixture {
	t.Helper()
	poll := rebootPoll
	rebootPoll = time.Millisecond
	t.Cleanup(func() { rebootPoll = poll })
	f := newRetryFixture(t, saved, answers...)
	f.run.Project.FleetStages[0].NodeTasks = []config.Task{{ID: "restart", Module: "reboot", ModuleInputs: &modules.Reboot{WaitAtMost: timeout}}}
	f.uncertain.RebootFrom = "boot-1"
	f.run.Entries[0].Response = f.uncertain
	if err := f.c.saveEntry(f.run.ID, f.run.Entries[0]); err != nil {
		t.Fatal(err)
	}
	return f
}

// Retry of a job that requested a reboot asks the node about it. While the
// node answers on the same boot, retry waits as the run did; once it answers
// on a new boot the fleet stage succeeds without running again. A fleet stage whose wait
// timed out is asked about the same way, so a node that returned late is
// not rebooted twice.
func TestRetryRecognizesACompletedReboot(t *testing.T) {
	for _, saved := range []string{"rebooting", "unknown", "failed"} {
		t.Run(saved, func(t *testing.T) {
			f := rebootFixture(t, saved, "1m", "rebooting", "rebooting", "succeeded")
			en := f.retry(t)
			if ops := f.taskBlocks(t); !reflect.DeepEqual(ops, []string{"status old", "status old", "status old"}) {
				t.Fatalf("node task_blocks %q", ops)
			}
			if en.JobID != f.old || en.NodeJobAttempt != 2 || en.Response.Status != "succeeded" || f.run.Status != "succeeded" {
				t.Fatalf("%s attempt %d %+v", en.JobID, en.NodeJobAttempt, en.Response)
			}
		})
	}
}

// A node still on the boot its job left when the wait's timeout ends has not
// rebooted: the job fails with that reason, and retry runs the fleet stage again,
// which requests the reboot again, after it asked the node whether it
// refuses the reboot.
func TestRebootWaitTimeoutFailsAndRetryRunsTheFleetStageAgain(t *testing.T) {
	f := rebootFixture(t, "rebooting", "50ms", "rebooting")
	en := f.retry(t)
	ops := f.taskBlocks(t)
	if len(ops) < 3 || ops[len(ops)-2] != "reboot-refusal" || ops[len(ops)-1] != "submit" || slices.Contains(ops[:len(ops)-1], "submit") {
		t.Fatalf("node task_blocks %q", ops)
	}
	timedOut := en.PreviousNodeJobAttempts[len(en.PreviousNodeJobAttempts)-1].Response
	if en.JobID != f.next || en.Response.Status != "succeeded" || timedOut.Status != "failed" || timedOut.RebootFrom != "boot-1" ||
		timedOut.Error != "node did not return on a new boot within 50ms" {
		t.Fatalf("%s %+v, timed out %+v", en.JobID, en.Response, timedOut)
	}
}

// mmx status records a reboot the node completed after the controller left.
func TestStatusRecordsACompletedReboot(t *testing.T) {
	f := rebootFixture(t, "rebooting", "1m", "succeeded")
	if err := f.c.Inspect(context.Background(), f.run, false); err != nil {
		t.Fatal(err)
	}
	saved, err := f.c.Load(f.run.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if en := saved.Entries[0]; en.Response.Status != "succeeded" || saved.Status != "succeeded" || !reflect.DeepEqual(f.taskBlocks(t), []string{"status old"}) {
		t.Fatalf("%s %+v %q", saved.Status, en.Response, f.taskBlocks(t))
	}
}

// not_found is the node's answer that it has no record of the job. Only after
// that answer does retry start a new attempt with its own job ID, keeping the
// answer as the earlier attempt; the node admits one job at a time, so a late
// admission of the old job cannot overlap it. An unreachable node leaves the
// job uncertain instead (TestRetryKeepsUnansweredJobUncertainWhenHostIsUnreachable).
func TestRetryStartsNewAttemptAfterNodeHasNoRecordOfJob(t *testing.T) {
	f := newRetryFixture(t, "unknown", "not_found")
	en := f.retry(t)
	if ops := f.taskBlocks(t); !reflect.DeepEqual(ops, []string{"status old", "submit"}) {
		t.Fatalf("node task_blocks %q", ops)
	}
	answer := protocol.Response{ID: f.old, Status: "not_found", Error: "node history is unavailable for job " + f.old}
	if en.JobID != f.next || en.NodeJobAttempt != 3 || !reflect.DeepEqual(en.Response, f.nextResult) || !reflect.DeepEqual(en.PreviousNodeJobAttempts, []Attempt{f.first, {JobID: f.old, Response: answer}}) {
		t.Fatalf("%s attempt %d %+v, attempts %+v", en.JobID, en.NodeJobAttempt, en.Response, en.PreviousNodeJobAttempts)
	}
	if f.run.Status != "succeeded" {
		t.Fatal(f.run.Status)
	}
}

// On a machine with another identity or architecture, the uncertain job cannot
// be asked about or replaced: retry refuses before any runner command, keeps
// the uncertain response as an earlier attempt, and fails the node fleet stage.
func TestRetryRefusesUncertainJobOnChangedNode(t *testing.T) {
	for _, changed := range []string{"identity", "architecture"} {
		t.Run(changed, func(t *testing.T) {
			f := newRetryFixture(t, "unknown", "succeeded")
			en := f.run.Entries[0]
			if changed == "identity" {
				en.NodeID = strings.Repeat("0", 64)
			} else {
				en.Architecture = "arm64"
			}
			if err := f.c.saveEntry(f.run.ID, en); err != nil {
				t.Fatal(err)
			}
			en = f.retry(t)
			if ops := f.taskBlocks(t); ops != nil {
				t.Fatalf("node task_blocks %q", ops)
			}
			if en.JobID != "" || en.NodeJobAttempt != 2 || en.Response.Status != "failed" || !strings.Contains(en.Response.Error, changed+" changed") || !reflect.DeepEqual(en.PreviousNodeJobAttempts, []Attempt{f.first, {JobID: f.old, Response: f.uncertain}}) {
				t.Fatalf("%s attempt %d %+v, attempts %+v", en.JobID, en.NodeJobAttempt, en.Response, en.PreviousNodeJobAttempts)
			}
			if f.run.Status != "failed" {
				t.Fatal(f.run.Status)
			}
		})
	}
}

// Work that never contacted the node still belongs to the machine the run's
// earlier fleet stages changed there: retry refuses a replacement instead of running
// later fleet stages on a machine that lacks the earlier ones.
func TestRetryRefusesUnstartedWorkOnReplacedNode(t *testing.T) {
	trace := filepath.Join(t.TempDir(), "trace")
	c, _ := sshNode(t, `printf '%s\n' "$command" >> `+quote(trace)+`; echo unexpected runner command >&2; exit 1`)
	p := config.Project{Project: "fleet", MaxNodesAtOnce: 1, FleetStages: []config.FleetStage{{ID: "base", Nodes: "a"}, {ID: "app", Nodes: "a"}}}
	r, err := c.New(context.Background(), p, config.Inventory{Nodes: map[string]config.Node{"a": {Address: "a", Transport: "ssh_only"}}}, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	base := r.Entries[0]
	base.JobID, base.NodeJobAttempt, base.NodeID, base.Architecture = "base", 1, strings.Repeat("9", 64), "amd64"
	base.Response = protocol.Response{ID: "base", Status: "succeeded", Change: "changed"}
	if err := c.saveEntry(r.ID, base); err != nil {
		t.Fatal(err)
	}
	execute(t, c, r, true)
	app := entry(t, r, "app", "a")
	if r.Status != "failed" || app.Response.Status != "failed" || !strings.Contains(app.Response.Error, "node was replaced") || lines(t, trace) != nil {
		t.Fatalf("%s %+v", r.Status, app.Response)
	}
}

func TestUncertainHTTPSSubmissionInspectsSameJobWithoutResubmitting(t *testing.T) {
	answers := map[string]int{"denied": 403, "busy": 409}
	for _, state := range []string{"running", "succeeded", "not_found", "denied", "busy"} {
		t.Run(state, func(t *testing.T) {
			trace := filepath.Join(t.TempDir(), "ssh-arguments")
			id := strings.Repeat("a", 64)
			fakeSSH(t, "printf '%s\\n' \"$@\" > "+quote(trace)+"\nprintf '%s\\n' "+quote(`{"id":"`+id+`","status":"`+state+`"}`)+"\n")
			submits, fallbacks := 0, 0
			client := fakeEndpoint(t, func(r *http.Request) (*http.Response, error) {
				if r.Method != "POST" || r.URL.Path != "/v1/submit" {
					t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
				}
				submits++
				if code := answers[state]; code != 0 {
					return &http.Response{StatusCode: code, Body: io.NopCloser(strings.NewReader(state)), Header: make(http.Header)}, nil
				}
				// The request reached the endpoint, but no admission response reached us.
				return nil, &net.OpError{Op: "read", Net: "tcp", Err: io.EOF}
			})
			conn := &Connection{Transport: "https", direct: client, fallback: func(context.Context) (*sshSession, error) {
				fallbacks++
				return &sshSession{Node: config.Node{Address: "fixture"}, Runner: "/runner", Credentials: &access.Credentials{}}, nil
			}}
			defer conn.Close()
			result, err := conn.submitBytes(context.Background(), []byte(`{"id":"`+id+`"}`), id)
			// The node's answer is a definite rejection; it never falls back.
			if answers[state] != 0 {
				var rejected *rejectedError
				if !errors.As(err, &rejected) || fallbacks != 0 {
					t.Fatal("node answer was not a rejection or allowed fallback", err, fallbacks)
				}
				return
			}
			if err != nil || result.Status != state || submits != 1 || fallbacks != 1 {
				t.Fatal(result, err, submits, fallbacks)
			}
			data, err := os.ReadFile(trace)
			if err != nil || !strings.Contains(string(data), "status") || !strings.Contains(string(data), id) || strings.Contains(string(data), "submit") {
				t.Fatalf("did not inspect original job: %s: %v", data, err)
			}
		})
	}
}

// fakeEndpoint is an HTTPS client whose requests get answer's reply; its fake
// transport opens no connection to authenticate.
func fakeEndpoint(t *testing.T, answer func(*http.Request) (*http.Response, error)) *endpoint.Client {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(key)
	if err != nil {
		t.Fatal(err)
	}
	client := endpoint.NewClient("https://fixture", "admin", "pin", []ssh.Signer{signer})
	client.HTTP.Transport = transportFunc(answer)
	return client
}

// fakeRunner fakes verified SSH to an installed runner that answers every
// task block for job id. Each runner command is appended to the returned trace;
// the first failures of them run fail (a shell snippet) instead.
func fakeRunner(t *testing.T, id, diff string, failures int, fail string) (string, *sshSession) {
	t.Helper()
	trace := filepath.Join(t.TempDir(), "trace")
	fakeSSH(t, `for command; do :; done
printf '%s\n' "$command" >> `+quote(trace)+`
if [ "$(wc -l < `+quote(trace)+`)" -le `+fmt.Sprint(failures)+` ]; then `+fail+`; fi
case "$command" in
*" upload "*) cat >/dev/null; echo '{"status":"uploaded"}';;
*" diff "*) printf '%s' `+quote(diff)+`;;
*" kept-files"*) cat >/dev/null; echo '{"files_to_send":[]}';;
*" reboot-refusal"*) cat >/dev/null; echo '{"refusal":""}';;
*" facts"*) cat >/dev/null; echo '{}';;
*" read-file"*) cat >/dev/null; echo '{"content":"text"}';;
*"last-successful-run"*) cat >/dev/null; echo '{"status":"recorded"}';;
*) echo '{"id":"`+id+`","status":"running"}';;
esac
`)
	return trace, &sshSession{Node: config.Node{Address: "fixture"}, Runner: "/runner", Credentials: &access.Credentials{}}
}

type nodeTaskBlock struct {
	name, https, ssh string
	call             func(*Connection) error
}

// nodeTaskBlocks are every controller task block on job id, each with the
// start of its HTTPS request line and the runner command its SSH form sends.
func nodeTaskBlocks(t *testing.T, id, diff string) []nodeTaskBlock {
	ctx := context.Background()
	asset := filepath.Join(t.TempDir(), "asset")
	if err := os.WriteFile(asset, []byte("asset bytes"), 0600); err != nil {
		t.Fatal(err)
	}
	upload, _ := assets.Hash(ctx, strings.NewReader("asset bytes"))
	upload.Path = asset
	difference, _ := assets.Hash(ctx, strings.NewReader(diff))
	return []nodeTaskBlock{
		{"status", "GET /v1/status/" + id, " status ", func(c *Connection) error { _, err := c.Status(ctx, id); return err }},
		{"stop", "POST /v1/stop/" + id, " stop ", func(c *Connection) error { _, err := c.Stop(ctx, id); return err }},
		{"progress", "POST /v1/progress", " progress", func(c *Connection) error {
			_, err := c.Progress(ctx, protocol.ProgressRequest{ID: id})
			return err
		}},
		{"upload", "PUT /v1/assets/", " upload ", func(c *Connection) error { return c.upload(ctx, id, upload, nil) }},
		{"kept-files", "POST /v1/kept-files", " kept-files", func(c *Connection) error {
			_, err := c.UseKeptFiles(ctx, id, []assets.Ref{upload})
			return err
		}},
		{"reboot-refusal", "POST /v1/reboot-refusal", " reboot-refusal", func(c *Connection) error {
			_, err := c.RebootRefusal(ctx, protocol.ControllerMachine{BootID: "boot-1", MachineIDHash: "hash-1"})
			return err
		}},
		{"facts", "POST /v1/facts", " facts", func(c *Connection) error { _, err := c.Facts(ctx, "web01"); return err }},
		{"read-file", "POST /v1/read-file", " read-file", func(c *Connection) error { _, err := c.ReadFile(ctx, "/etc/hostname"); return err }},
		{"last-successful-run", "POST /v1/last-successful-run", " last-successful-run", func(c *Connection) error {
			return c.RecordLastSuccessfulRun(ctx, protocol.LastSuccessfulRunRequest{Project: "shop", Playbook: "site"})
		}},
		{"diff", "GET /v1/diffs/", " diff ", func(c *Connection) error {
			dir := t.TempDir()
			if err := c.downloadDiff(ctx, id, difference, dir); err != nil {
				return err
			}
			return assets.Verify(ctx, differences.Local(dir, difference))
		}},
		{"submit", "POST /v1/submit", " submit", func(c *Connection) error {
			_, err := c.submitBytes(ctx, []byte(`{"id":"`+id+`"}`), id)
			return err
		}},
	}
}

func TestNodeTaskBlocksFallBackToSSHOnlyWhenHTTPSIsUnreachable(t *testing.T) {
	id, diff := strings.Repeat("a", 64), "-old\n+new\n"
	answers := map[string]func() (*http.Response, error){
		"unreachable": func() (*http.Response, error) {
			return nil, &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
		},
		"identity": func() (*http.Response, error) { return nil, &endpoint.IdentityError{Reason: "changed endpoint key"} },
		"denied": func() (*http.Response, error) {
			return &http.Response{StatusCode: 403, Body: io.NopCloser(strings.NewReader("key removed")), Header: make(http.Header)}, nil
		},
		"answered": func() (*http.Response, error) {
			return &http.Response{StatusCode: 409, Body: io.NopCloser(strings.NewReader("node busy")), Header: make(http.Header)}, nil
		},
	}
	for _, op := range nodeTaskBlocks(t, id, diff) {
		for name, answer := range answers {
			t.Run(op.name+"/"+name, func(t *testing.T) {
				trace, session := fakeRunner(t, id, diff, 0, ":")
				fallbacks := 0
				var requestLines []string
				conn := &Connection{Transport: "https", httpsURL: "https://fixture", direct: fakeEndpoint(t, func(r *http.Request) (*http.Response, error) {
					requestLines = append(requestLines, r.Method+" "+r.URL.Path)
					return answer()
				}), fallback: func(context.Context) (*sshSession, error) {
					fallbacks++
					return session, nil
				}}
				defer conn.Close()
				err := op.call(conn)
				commands, _ := os.ReadFile(trace)
				if len(requestLines) == 0 || !strings.HasPrefix(requestLines[0], op.https) {
					t.Fatalf("HTTPS requests %q, want %q first", requestLines, op.https)
				}
				if name == "unreachable" {
					// Idempotent operations repeat over SSH; submit inspects its job.
					want := op.ssh
					if op.name == "submit" {
						want = " status "
					}
					// The first SSH command asks the node about its side of HTTPS, for the notice.
					_, afterCheck, _ := strings.Cut(string(commands), "echo ufw_without_rule")
					if err != nil || fallbacks != 1 || conn.Transport != "ssh" || strings.Count(string(commands), "ufw status | grep -q") != 1 || strings.Count(string(commands), "firewall-cmd --state") != 1 || !strings.Contains(afterCheck, want) {
						t.Fatalf("%v, %d fallbacks, %s: %s", err, fallbacks, conn.Transport, commands)
					}
					// Submit returns before the fallback; the notice still gives its failure.
					if conn.ConnectionUsed() != "ssh_fallback" || !strings.HasPrefix(conn.FallbackReason, "MMX could not connect to https://fixture (connection refused)") || !strings.Contains(conn.FallbackReason, "MMX is using SSH for this node") || !strings.Contains(conn.FallbackReason, "connection: ssh_only") || !strings.Contains(conn.FallbackReason, "--connection ssh_only") {
						t.Fatal(conn.ConnectionUsed(), conn.FallbackReason)
					}
					return
				}
				// Only the node's answer to submit is a rejection of that job.
				var rejected *rejectedError
				if err == nil || fallbacks != 0 || len(commands) != 0 || errors.As(err, &rejected) != (op.name == "submit" && name != "identity") {
					t.Fatalf("%v, %d fallbacks: %s", err, fallbacks, commands)
				}
			})
		}
	}
}

func TestNodeTaskBlocksRepeatOnlyAfterSSHConnectionLoss(t *testing.T) {
	id, diff := strings.Repeat("a", 64), "-old\n+new\n"
	for _, op := range nodeTaskBlocks(t, id, diff) {
		for _, tc := range []struct{ name, fail string }{
			{"connection lost", `echo 'Connection reset by peer' >&2; exit 255`},
			{"authentication", `echo 'Permission denied (publickey)' >&2; exit 255`},
			{"host key", `echo 'Host key verification failed.' >&2; exit 255`},
			{"node answer", `echo 'node busy with job other' >&2; exit 1`},
		} {
			t.Run(op.name+"/"+tc.name, func(t *testing.T) {
				trace, session := fakeRunner(t, id, diff, 1, tc.fail)
				err := op.call(&Connection{Transport: "ssh", native: session})
				data, _ := os.ReadFile(trace)
				commands := strings.Split(strings.TrimSpace(string(data)), "\n")
				var rejected *rejectedError
				switch {
				case tc.name != "connection lost":
					// Neither is repeated; only the runner's own error rejects a submit.
					if err == nil || len(commands) != 1 || errors.As(err, &rejected) != (op.name == "submit" && tc.name == "node answer") {
						t.Fatalf("%v: %q", err, commands)
					}
					// An access failure names its remedy.
					remedy := map[string]string{"authentication": "ssh_password", "host key": "known_hosts"}[tc.name]
					if !strings.Contains(err.Error(), remedy) {
						t.Fatal("no remedy:", err)
					}
				case op.name == "submit":
					// The lost submit may have been admitted: inspect it, never resend.
					if err != nil || len(commands) != 2 || !strings.Contains(commands[1], " status ") {
						t.Fatalf("%v: %q", err, commands)
					}
				default:
					if err != nil || len(commands) != 2 || !strings.Contains(commands[1], op.ssh) {
						t.Fatalf("%v: %q", err, commands)
					}
				}
			})
		}
	}
}

func TestOversizedResponseFailsAlikeOverHTTPSAndSSH(t *testing.T) {
	id := strings.Repeat("a", 64)
	trace := filepath.Join(t.TempDir(), "trace")
	fakeSSH(t, "echo >> "+quote(trace)+"\nhead -c "+fmt.Sprint(protocol.ResponseLimit+1)+" /dev/zero\n")
	session := &sshSession{Node: config.Node{Address: "fixture"}, Runner: "/runner", Credentials: &access.Credentials{}}
	fallbacks := 0
	https := &Connection{Transport: "https", fallback: func(context.Context) (*sshSession, error) { fallbacks++; return session, nil }}
	https.direct = fakeEndpoint(t, func(*http.Request) (*http.Response, error) {
		body := `{"id":"` + id + `","status":"running","error":"` + strings.Repeat("x", protocol.ResponseLimit) + `"}`
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})
	_, err := https.Status(context.Background(), id)
	if !errors.Is(err, protocol.ErrResponseLimit) || fallbacks != 0 {
		t.Fatal("HTTPS", err, fallbacks)
	}
	// An oversized answer is not a lost connection, so it is not repeated.
	_, err = (&Connection{Transport: "ssh", native: session}).Status(context.Background(), id)
	commands, _ := os.ReadFile(trace)
	if !errors.Is(err, protocol.ErrResponseLimit) || len(commands) != 1 {
		t.Fatal("SSH", err, len(commands))
	}
}

// connectFixture is one node for Controller.connect. Verified SSH reports a
// fixed identity and installed runner and answers setup; with password set, SSH
// accepts only the password "secret" through askpass. With needKey set, setup
// accepts no offered key until the request adds one. The HTTPS endpoint has its
// own TLS key and answers info with answer.
type connectFixture struct {
	c                               Controller
	run                             *Run
	managedNode                     config.Node
	info                            endpoint.Info // the node's current identity
	server                          *httptest.Server
	ssh, sshClosed, requests, saved string
	// runnerVersion holds the MMX version the node runner executable reports
	// over SSH.
	runnerVersion string
	// httpsCheck is the file whose content the node answers its HTTPS service
	// check with.
	httpsCheck string
	// dropConnectionsBeforeSetup makes the HTTPS endpoint close every request
	// until setup has run, as a node whose HTTPS service is not running does.
	dropConnectionsBeforeSetup bool
}

func newConnectFixture(t *testing.T, password, needKey bool, answer func(info endpoint.Info, setupRan bool) (int, endpoint.Info)) *connectFixture {
	t.Helper()
	t.Setenv("SSH_AUTH_SOCK", "")
	identity, err := endpoint.LoadIdentity(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	binary := []byte("runner")
	runner, node := sha256.Sum256(binary), sha256.Sum256([]byte("0123456789abcdef0123456789abcdef"))
	f := &connectFixture{info: endpoint.Info{NodeID: hex.EncodeToString(node[:]), Architecture: "amd64", RunnerHash: hex.EncodeToString(runner[:]), TLSPin: identity.Pin, BootID: "0b0b0b0b-0000-4000-8000-000000000000", RunnerVersion: release.Version}}
	work := t.TempDir()
	f.ssh, f.requests = filepath.Join(work, "ssh"), filepath.Join(work, "requests")
	f.sshClosed = filepath.Join(work, "ssh-closed")
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	block, err := ssh.MarshalPrivateKey(private, "")
	if err != nil {
		t.Fatal(err)
	}
	key := filepath.Join(work, "id_ed25519")
	if err := os.WriteFile(key, pem.EncodeToMemory(block), 0600); err != nil {
		t.Fatal(err)
	}
	auth := ""
	if password {
		auth = `case "$*" in *BatchMode=yes*) echo 'Permission denied (publickey,password).' >&2; exit 255;; esac
test "$("$SSH_ASKPASS" "admin@fixture's password: ")" = secret || { echo 'Permission denied (password).' >&2; exit 255; }
`
	}
	info, _ := json.Marshal(f.info)
	reply := `printf '{"info":%s}\n' ` + quote(string(info))
	if needKey {
		reply = `case "$request" in *'"add_key"'*) ` + reply + `;; *) echo '{"need_key":true,"reason":"keys supplied by AuthorizedKeysCommand require transport: ssh"}';; esac`
	}
	dir := fakeSSH(t, `case "$1" in -G) printf 'hostname fixture\nuser admin\nport %s\nidentityfile %s\n' "$(cat "$(dirname "$0")/port" 2>/dev/null || echo 22)" `+quote(key)+`; exit 0;; -F) echo closed >> `+quote(f.sshClosed)+`; exit 0;; esac
echo ssh >> `+quote(f.ssh)+`
key=; previous=; for argument; do test "$previous" = -i && key=$argument; previous=$argument; done
if test -e "$(dirname "$0")/authorized-key"; then cmp -s "$key" "$(dirname "$0")/authorized-key" 2>/dev/null || { echo 'Permission denied (publickey).' >&2; exit 255; }; fi
`+auth+`for command; do :; done
case "$command" in
"cat /etc/machine-id /proc/"*) printf '0123456789abcdef0123456789abcdef\n0b0b0b0b-0000-4000-8000-000000000000\n';;
*machine-id*) printf 'Linux\nx86_64\n0\nroot\n0b0b0b0b-0000-4000-8000-000000000000\nsystemd\n0123456789abcdef0123456789abcdef\n\n10.0.0.1 5000 10.0.0.2 22\n';;
*sha256sum*) echo `+f.info.RunnerHash+`;;
*'systemctl start '*) echo "Failed to start mmx-https-api.service" >&2; exit 1;;
*mmx-https-api.service*) if test -e "$(dirname "$0")/https-check"; then cat "$(dirname "$0")/https-check"; else echo "unexpected command" >&2; exit 1; fi;;
*" setup"*) request=$(cat); printf '%s\n' "$request" >> `+quote(f.requests)+`; `+reply+`;;
*) echo "unexpected command" >&2; exit 1;;
esac
`)
	if err := os.WriteFile(filepath.Join(dir, "mmx-runner-linux-amd64"), binary, 0700); err != nil {
		t.Fatal(err)
	}
	f.runnerVersion = filepath.Join(dir, "runner-version")
	f.httpsCheck = filepath.Join(dir, "https-check")
	f.server = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/session" {
			_, _ = w.Write([]byte(`{"status":"authenticated"}`))
			return
		}
		_, err := os.Stat(f.requests)
		if f.dropConnectionsBeforeSetup && err != nil {
			if conn, _, hijackErr := w.(http.Hijacker).Hijack(); hijackErr == nil {
				conn.Close()
			}
			return
		}
		code, info := answer(f.info, err == nil)
		if code != 200 {
			http.Error(w, "fixture answer", code)
			return
		}
		_ = json.NewEncoder(w).Encode(info)
	}))
	f.server.TLS = &tls.Config{MinVersion: tls.VersionTLS13, GetCertificate: identity.GetCertificate}
	f.server.StartTLS()
	t.Cleanup(f.server.Close)
	f.c, f.run = fleet(t, config.FleetStage{ID: "setup", Nodes: "a"})
	f.c.RunnerDir = dir
	f.c.Credentials = &access.Credentials{Interactive: true, Prompt: func(label string) (string, error) {
		if password && strings.Contains(label, "SSH password") {
			return "secret", nil
		}
		return "", fmt.Errorf("unexpected prompt %s", label)
	}}
	f.managedNode = config.Node{Address: "a", Endpoint: f.server.URL}
	destination := sha256.Sum256([]byte("fixture\x0022\x00admin"))
	f.saved = filepath.Join(f.c.State, "nodes", hex.EncodeToString(destination[:])+".json")
	return f
}

// reportRunnerVersion makes the node runner executable report version over SSH.
func (f *connectFixture) reportRunnerVersion(t *testing.T, version string) {
	t.Helper()
	if err := os.WriteFile(f.runnerVersion, []byte(version), 0600); err != nil {
		t.Fatal(err)
	}
}

func (f *connectFixture) save(t *testing.T, info endpoint.Info) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(f.saved), 0700); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(info)
	if err := os.WriteFile(f.saved, data, 0600); err != nil {
		t.Fatal(err)
	}
}
func (f *connectFixture) connect(t *testing.T) (*Connection, error) {
	t.Helper()
	conn, err := f.c.connect(context.Background(), f.run, f.managedNode, "", machine{})
	if conn != nil {
		t.Cleanup(conn.Close)
	}
	return conn, err
}

// lines reads a fixture trace; nil means nothing was recorded.
func lines(t *testing.T, path string) []string {
	t.Helper()
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

func currentInfo(info endpoint.Info, _ bool) (int, endpoint.Info) { return 200, info }

// A node runner executable of another MMX release than this mmx program is
// refused, whether the node reports its version over HTTPS or over SSH: the
// node would otherwise run modules without the fixes of this release. The
// failure names both versions, the file, and how to put matching files in
// place. A recorded node job keeps the node runner executable it started with,
// so for a resumed run the failure says to resume with that release instead.
func TestConnectRefusesRunnerOfAnotherRelease(t *testing.T) {
	path := func(f *connectFixture) string { return filepath.Join(f.c.RunnerDir, "mmx-runner-linux-amd64") }
	other := func(f *connectFixture, version string) string {
		return "the node runner executable " + path(f) + " reports MMX version " + version + ", but this mmx program is version " + release.Version +
			". Install mmx together with the mmx-runner files of the same MMX release; a release archive holds both"
	}
	t.Run("https", func(t *testing.T) {
		f := newConnectFixture(t, false, false, func(info endpoint.Info, _ bool) (int, endpoint.Info) {
			info.RunnerVersion = "0.0.9"
			return 200, info
		})
		f.save(t, f.info)
		if _, err := f.connect(t); err == nil || err.Error() != other(f, "0.0.9") {
			t.Fatal(err)
		}
	})
	t.Run("https-without-version", func(t *testing.T) {
		f := newConnectFixture(t, false, false, func(info endpoint.Info, _ bool) (int, endpoint.Info) {
			info.RunnerVersion = ""
			return 200, info
		})
		f.save(t, f.info)
		want := strings.Replace(other(f, ""), "reports MMX version ,", "reports no MMX version,", 1)
		if _, err := f.connect(t); err == nil || err.Error() != want {
			t.Fatal(err)
		}
	})
	t.Run("ssh", func(t *testing.T) {
		f := newConnectFixture(t, false, false, currentInfo)
		f.managedNode.Transport = "ssh_only"
		f.reportRunnerVersion(t, "0.0.9")
		if _, err := f.connect(t); err == nil || err.Error() != other(f, "0.0.9") {
			t.Fatal(err)
		}
		f.reportRunnerVersion(t, release.Version)
		if _, err := f.connect(t); err != nil {
			t.Fatal("the matching node runner executable was refused", err)
		}
	})
	t.Run("recorded", func(t *testing.T) {
		f := newConnectFixture(t, false, false, currentInfo)
		f.managedNode.Transport = "ssh_only"
		f.reportRunnerVersion(t, "0.0.9")
		retained := filepath.Join(f.c.State, "runners", f.info.RunnerHash)
		if err := os.MkdirAll(filepath.Dir(retained), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(retained, []byte("runner"), 0600); err != nil {
			t.Fatal(err)
		}
		_, err := f.c.connect(context.Background(), f.run, f.managedNode, f.info.RunnerHash, machine{})
		want := "this playbook run's recorded node job uses the node runner executable " + filepath.Join(f.c.State, "runners", f.info.RunnerHash) +
			", which reports MMX version 0.0.9, but this mmx program is version " + release.Version +
			". A recorded node job keeps the node runner executable it started with. Resume the run with mmx of that release, or start a new run with mmx run"
		if err == nil || err.Error() != want {
			t.Fatal(err)
		}
	})
}

// An administrator refused over HTTPS is final: it never reaches SSH or setup.
func TestConnectRejectionWithSavedIdentityIsFinal(t *testing.T) {
	f := newConnectFixture(t, false, false, func(endpoint.Info, bool) (int, endpoint.Info) { return 403, endpoint.Info{} })
	f.save(t, f.info)
	var denied *endpoint.HTTPError
	if _, err := f.connect(t); !errors.As(err, &denied) || denied.Status != 403 || lines(t, f.ssh) != nil {
		t.Fatal("HTTPS rejection", err)
	}
}

// notices collects the connection's user messages.
func (f *connectFixture) notices() *[]string {
	var got []string
	f.c.Credentials.Notify = func(message string) { got = append(got, message) }
	return &got
}

// oldMachine is the saved identity of the server this node's address reached
// before it was replaced.
func (f *connectFixture) oldMachine() endpoint.Info {
	old := f.info
	old.NodeID, old.TLSPin = strings.Repeat("9", 64), strings.Repeat("1", 64)
	return old
}

// The saved identity only records what verified SSH reported. When the node no
// longer matches it, whether a new server with its endpoint already running,
// a fresh server without one, or the same machine with a new endpoint key,
// verified SSH decides again: the node is set up, its identity saved, and the
// user told why.
func TestConnectSetsUpChangedNodeTrustedBySSH(t *testing.T) {
	for _, tt := range []struct {
		name, notice, transport string
		fresh, sameMachine      bool
	}{
		{"replaced", "node address now reaches a different machine", "https", false, false},
		{"replaced-same-key", "node address now reaches a different machine", "https", false, false},
		{"replaced-fresh", "node address now reaches a different machine", "ssh", true, false},
		{"new-endpoint-key", "node HTTPS API key changed", "https", false, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newConnectFixture(t, false, false, currentInfo)
			saved := f.oldMachine()
			if tt.sameMachine {
				saved.NodeID = f.info.NodeID
			}
			if tt.name == "replaced-same-key" {
				// The endpoint key passes the pin but reports another machine.
				saved.TLSPin = f.info.TLSPin
			}
			f.save(t, saved)
			if tt.fresh {
				f.server.Close()
			}
			notices := f.notices()
			conn, err := f.connect(t)
			var now endpoint.Info
			data, _ := os.ReadFile(f.saved)
			if err != nil || conn.Transport != tt.transport || conn.NodeID != f.info.NodeID || len(lines(t, f.requests)) != 1 || json.Unmarshal(data, &now) != nil || now != f.info {
				t.Fatal(conn, err, now)
			}
			if !strings.Contains(strings.Join(*notices, "\n"), tt.notice) {
				t.Fatalf("notices %q", *notices)
			}
		})
	}
}

// Once verified SSH confirms the saved endpoint key, an HTTPS server presenting
// another key is not the node: the identity error is final.
func TestConnectRejectsInterceptedHTTPSAfterSSHConfirmsKey(t *testing.T) {
	f := newConnectFixture(t, false, false, currentInfo)
	f.save(t, f.info)
	other, err := endpoint.LoadIdentity(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f.server.TLS.GetCertificate = other.GetCertificate
	_, err = f.connect(t)
	var now endpoint.Info
	data, _ := os.ReadFile(f.saved)
	var identity *endpoint.IdentityError
	if !errors.As(err, &identity) || len(lines(t, f.requests)) != 1 || json.Unmarshal(data, &now) != nil || now != f.info {
		t.Fatal(err, now)
	}
}

// Recorded work belongs to the machine it ran on. A replaced node is refused
// before setup or any runner install, whether or not a later run already
// saved the new machine's identity.
func TestConnectRefusesReplacedNodeForRecordedWork(t *testing.T) {
	for _, updated := range []bool{false, true} {
		f := newConnectFixture(t, false, false, currentInfo)
		old := f.oldMachine()
		saved := old
		if updated {
			saved = f.info
		}
		f.save(t, saved)
		before, _ := os.ReadFile(f.saved)
		_, err := f.c.connect(context.Background(), f.run, f.managedNode, "", machine{old.NodeID, old.Architecture})
		after, _ := os.ReadFile(f.saved)
		var replaced *replacedError
		if !errors.As(err, &replaced) || lines(t, f.requests) != nil || string(before) != string(after) {
			t.Fatal(updated, err)
		}
	}
}

// Retry of an uncertain job on a replaced HTTPS node fails the node fleet stage and
// keeps the job as an earlier attempt, like SSH (TestRetryRefusesUncertainJobOnChangedHost).
func TestRetryFailsUncertainJobOnReplacedHTTPSNode(t *testing.T) {
	f := newConnectFixture(t, false, false, currentInfo)
	old := f.oldMachine()
	f.save(t, old)
	if err := os.MkdirAll(filepath.Join(f.c.State, "runners"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.c.State, "runners", f.info.RunnerHash), []byte("runner"), 0600); err != nil {
		t.Fatal(err)
	}
	f.run.Inventory.Nodes["a"] = f.managedNode
	en := f.run.Entries[0]
	uncertain := protocol.Response{ID: "old", Status: "unknown", Error: "connection lost"}
	en.JobID, en.NodeJobAttempt, en.Response = "old", 1, uncertain
	en.NodeID, en.Architecture, en.RunnerHash, en.Transport = old.NodeID, old.Architecture, f.info.RunnerHash, "https"
	f.run.Entries[0] = en
	if err := f.c.saveEntry(f.run.ID, en); err != nil {
		t.Fatal(err)
	}
	execute(t, f.c, f.run, true)
	en = f.run.Entries[0]
	if f.run.Status != "failed" || en.JobID != "" || en.Response.Status != "failed" || !strings.Contains(en.Response.Error, "node was replaced") ||
		!reflect.DeepEqual(en.PreviousNodeJobAttempts, []Attempt{{JobID: "old", Response: uncertain}}) || lines(t, f.requests) != nil {
		t.Fatalf("%s %+v, attempts %+v", f.run.Status, en.Response, en.PreviousNodeJobAttempts)
	}
}

// Unreachable HTTPS with the retained runner uses verified SSH without setup.
func TestConnectUsesSSHWhenHTTPSIsUnreachable(t *testing.T) {
	f := newConnectFixture(t, false, false, currentInfo)
	f.save(t, f.info)
	f.server.Close()
	conn, err := f.connect(t)
	if err != nil || conn.Transport != "ssh" || lines(t, f.ssh) == nil || lines(t, f.requests) != nil {
		t.Fatal(conn, err)
	}
}

// A node whose HTTPS service unit no longer exists (the node was removed from
// MMX's management and added back) is set up again through verified SSH,
// replacing the saved identity, so that HTTPS comes back. A unit that exists
// but does not run, or a blocked port, keeps today's path: SSH, no setup.
func TestConnectSetsUpNodeWhoseHTTPSServiceUnitIsMissing(t *testing.T) {
	for _, tt := range []struct {
		name, check, transport string
		setups                 int
	}{
		{"unit missing", "not-found\n-\n", "https", 1},
		{"unit failed", "failed\n-\n", "ssh", 0},
		{"unit running, port blocked", "active\nufw_without_rule\n", "ssh", 0},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newConnectFixture(t, false, false, currentInfo)
			f.dropConnectionsBeforeSetup = tt.setups == 1
			old := f.info
			old.BootID = "00000000-0000-4000-8000-000000000000" // what an earlier setup saved
			f.save(t, old)
			if tt.setups == 0 {
				f.server.Close()
			}
			if err := os.WriteFile(f.httpsCheck, []byte(tt.check), 0600); err != nil {
				t.Fatal(err)
			}
			notices := f.notices()
			conn, err := f.connect(t)
			var now endpoint.Info
			data, _ := os.ReadFile(f.saved)
			if err != nil || conn.Transport != tt.transport || len(lines(t, f.requests)) != tt.setups || json.Unmarshal(data, &now) != nil {
				t.Fatal(conn, err, *notices)
			}
			if tt.setups == 1 && (now != f.info || !strings.Contains(strings.Join(*notices, "\n"), "does not exist")) {
				t.Fatal(now, *notices)
			}
			if tt.setups == 0 && now != old {
				t.Fatal("saved identity changed", now)
			}
		})
	}
}

// An endpoint serving another runner is set up again through SSH, then used.
func TestConnectSetsUpEndpointRunningAnotherRunner(t *testing.T) {
	f := newConnectFixture(t, false, false, func(info endpoint.Info, setupRan bool) (int, endpoint.Info) {
		if !setupRan {
			info.RunnerHash = strings.Repeat("0", 64)
		}
		return 200, info
	})
	old := f.info
	old.RunnerHash = strings.Repeat("0", 64)
	f.save(t, old)
	conn, err := f.connect(t)
	var saved endpoint.Info
	data, _ := os.ReadFile(f.saved)
	if err != nil || conn.Transport != "https" || len(lines(t, f.requests)) != 1 || json.Unmarshal(data, &saved) != nil || saved != f.info {
		t.Fatal(conn, err, saved)
	}
}

// Password-only access enrolls a managed key; key-only access that no key file
// authorizes says why instead.
func TestConnectEnrollsKeyOnlyAfterPasswordLogin(t *testing.T) {
	f := newConnectFixture(t, true, true, currentInfo)
	conn, err := f.connect(t)
	if err != nil || conn.Transport != "https" {
		t.Fatal(conn, err)
	}
	requests := lines(t, f.requests)
	var enroll endpoint.SetupRequest
	if len(requests) != 2 || json.Unmarshal([]byte(requests[1]), &enroll) != nil {
		t.Fatal(requests)
	}
	managed, err := access.EnsureKey(filepath.Join(os.Getenv("HOME"), ".ssh", "mmx", strings.TrimSuffix(filepath.Base(f.saved), ".json")))
	if err != nil || string(enroll.AddKey) != string(managed.PublicKey().Marshal()) {
		t.Fatal("managed key not enrolled", err)
	}
	f = newConnectFixture(t, false, true, currentInfo)
	if _, err := f.connect(t); err == nil || !strings.Contains(err.Error(), "AuthorizedKeysCommand require transport: ssh") || len(lines(t, f.requests)) != 1 {
		t.Fatal("key-only access without an authorized key file", err)
	}
}

// `ssh -G` prints IdentityFile tokens unexpanded while OpenSSH's passphrase
// prompt names the expanded path, so a token-named key must still match.
func TestIdentityTokensMatchOpenSSHPassphrasePrompt(t *testing.T) {
	if _, err := exec.LookPath("ssh"); err != nil {
		t.Skip("needs the OpenSSH client")
	}
	cfg := filepath.Join(t.TempDir(), "config")
	if err := os.WriteFile(cfg, []byte("Host x\n  IdentityFile ~/.ssh/%u_key\n"), 0600); err != nil {
		t.Fatal(err)
	}
	s, err := resolveSSH(context.Background(), config.Node{Address: "x", SSHConfig: cfg})
	if err != nil {
		t.Fatal(err)
	}
	u, _ := user.Current()
	home, _ := os.UserHomeDir()
	keys := map[string]bool{}
	for _, p := range s.Identities {
		keys[p] = true
	}
	prompt := "Enter passphrase for key '" + filepath.Join(home, ".ssh", u.Username+"_key") + "': "
	if keyPrompt(prompt, keys) == "" {
		t.Fatalf("OpenSSH's own prompt refused; identities %v", s.Identities)
	}
}

// A runner file is read and hashed once per process: later nodes share the
// first read even if the file changes, so one command uses one runner build.
func TestRunnerArtifactIsReadOncePerProcess(t *testing.T) {
	c := Controller{State: t.TempDir()}
	path := filepath.Join(t.TempDir(), "mmx-runner-linux-amd64")
	if err := os.WriteFile(path, []byte("first build"), 0700); err != nil {
		t.Fatal(err)
	}
	first, err := c.runnerArtifact(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("second build"), 0700); err != nil {
		t.Fatal(err)
	}
	again, err := c.runnerArtifact(path)
	sum := sha256.Sum256([]byte("first build"))
	retained, _ := os.ReadFile(filepath.Join(c.State, "runners", hex.EncodeToString(sum[:])))
	if err != nil || again != first || first.hash != hex.EncodeToString(sum[:]) || string(retained) != "first build" {
		t.Fatal(err, first.hash)
	}
	// SSH setup sends the same bytes compressed, compressed once.
	packed := first.gzipped()
	r, err := gzip.NewReader(bytes.NewReader(packed))
	if err != nil {
		t.Fatal(err)
	}
	unpacked, err := io.ReadAll(r)
	if err != nil || string(unpacked) != "first build" || &again.gzipped()[0] != &packed[0] {
		t.Fatal("compressed runner", err, string(unpacked))
	}
}

type countingAgent struct {
	agent.Agent
	lists atomic.Int32
}

func (a *countingAgent) List() ([]*agent.Key, error) {
	a.lists.Add(1)
	return a.Agent.List()
}

// agentSocket serves an agent holding one key and counts its key listings.
func agentSocket(t *testing.T) (*countingAgent, string) {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "mmx-keyring-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	ring := &countingAgent{Agent: agent.NewKeyring()}
	if err := ring.Add(agent.AddedKey{PrivateKey: key}); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", filepath.Join(dir, "socket"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() { defer conn.Close(); _ = agent.ServeAgent(ring, conn) }()
		}
	}()
	return ring, listener.Addr().String()
}

// Nodes that connect at once share one key load, so the agent is asked once.
func TestAdministratorKeysLoadOncePerProcess(t *testing.T) {
	ring, socket := agentSocket(t)
	dir := t.TempDir()
	credentials := &access.Credentials{}
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			signers, err := loadSigners(nil, socket, false, credentials)
			if err != nil || len(signers) != 1 {
				t.Error(signers, err)
				return
			}
			if _, err := signers[0].Sign(rand.Reader, []byte("data")); err != nil {
				t.Error("shared agent signer", err)
			}
		}()
	}
	wg.Wait()
	if lists := ring.lists.Load(); lists != 1 {
		t.Fatal("agent listed", lists, "times")
	}
	// A load that finds no key is not kept.
	empty := filepath.Join(dir, "missing")
	if signers, err := loadSigners([]string{empty}, "", true, credentials); err != nil || len(signers) != 0 {
		t.Fatal(signers, err)
	}
	_, other, _ := ed25519.GenerateKey(rand.Reader)
	block, err := ssh.MarshalPrivateKey(other, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(empty, pem.EncodeToMemory(block), 0600); err != nil {
		t.Fatal(err)
	}
	if signers, err := loadSigners([]string{empty}, "", true, credentials); err != nil || len(signers) != 1 {
		t.Fatal("key created later was not found", signers, err)
	}
}

// Nodes without identity_file each have their own managed key, yet they share
// one load of the administrator's other keys: the agent is asked once. A
// managed key that setup creates later is still found for its node.
func TestNodesWithoutIdentityFileShareOneAgentListing(t *testing.T) {
	ring, socket := agentSocket(t)
	t.Setenv("SSH_AUTH_SOCK", socket)
	binary := []byte("runner")
	sum := sha256.Sum256(binary)
	machineID := "0123456789abcdef0123456789abcdef"
	node := sha256.Sum256([]byte(machineID))
	info := fmt.Sprintf(`{"info":{"node_id":"%x","architecture":"amd64","runner_hash":"%x","tls_pin":"%s"}}`, node, sum, strings.Repeat("c", 64))
	dir := fakeSSH(t, `for command; do :; done
case "$1" in -G) printf 'hostname %s\nuser root\nport 22\n' "$command"; exit 0;; -F) exit 0;; esac
case "$command" in
*machine-id*) printf 'Linux\nx86_64\n0\nroot\n0b0b0b0b-0000-4000-8000-000000000000\nsystemd\n`+machineID+`\n\n10.0.0.1 5000 10.0.0.2 22\n';;
*sha256sum*) echo `+hex.EncodeToString(sum[:])+`;;
*setup*) cat >/dev/null; echo `+quote(info)+`;;
esac
`)
	if err := os.WriteFile(filepath.Join(dir, "mmx-runner-linux-amd64"), binary, 0700); err != nil {
		t.Fatal(err)
	}
	credentials := &access.Credentials{}
	c := Controller{State: t.TempDir(), RunnerDir: dir, Credentials: credentials}
	for _, address := range []string{"first", "second"} {
		// The endpoint is unreachable, so each host continues over SSH after setup.
		conn, err := c.connect(context.Background(), &Run{}, config.Node{Address: address, Endpoint: "https://127.0.0.1:1"}, "", machine{})
		if err != nil {
			t.Fatal(address, err)
		}
		conn.Close()
	}
	if lists := ring.lists.Load(); lists != 1 {
		t.Fatal("agent listed", lists, "times for two nodes")
	}
	settings := sshSettings{Agent: socket}
	managed := filepath.Join(t.TempDir(), "mmx", "managed")
	if signers, err := nodeSigners(config.Node{}, settings, managed, credentials); err != nil || len(signers) != 1 {
		t.Fatal(signers, err)
	}
	created, err := access.EnsureKey(managed)
	if err != nil {
		t.Fatal(err)
	}
	signers, err := nodeSigners(config.Node{}, settings, managed, credentials)
	if err != nil || len(signers) != 2 || !bytes.Equal(signers[1].PublicKey().Marshal(), created.PublicKey().Marshal()) {
		t.Fatal("managed key created later was not found", signers, err)
	}
	if lists := ring.lists.Load(); lists != 1 {
		t.Fatal("agent listed", lists, "times")
	}
}

// The controller asks a node which of the files that the job names to keep it
// keeps, and sends only the ones it lacks. A file from the project is not
// asked about: it is always sent.
func TestDeliverySendsOnlyKeptFilesTheNodeLacks(t *testing.T) {
	ctx := context.Background()
	id := strings.Repeat("a", 64)
	c := Controller{State: t.TempDir()}
	r := &Run{ID: "20261008T120000.000000000-0123456789abcdef"}
	dir := filepath.Join(c.runDir(r.ID), "assets")
	refs := map[string]assets.Ref{}
	for _, text := range []string{"kept module file", "module file to send", "kept download", "file from the project"} {
		source := filepath.Join(t.TempDir(), "file")
		if err := os.WriteFile(source, []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
		ref, err := assets.Snapshot(ctx, dir, source)
		if err != nil {
			t.Fatal(err)
		}
		refs[text] = ref
	}
	kept, sent, download, project := refs["kept module file"], refs["module file to send"], refs["kept download"], refs["file from the project"]
	trace, request := filepath.Join(t.TempDir(), "trace"), filepath.Join(t.TempDir(), "request")
	fakeSSH(t, `for command; do :; done
printf '%s\n' "$command" >> `+quote(trace)+`
case "$command" in
*" upload "*) cat >/dev/null; echo '{"status":"uploaded"}';;
*" kept-files"*) cat > `+quote(request)+`; echo '{"files_to_send":["`+sent.SHA256+`"]}';;
esac
`)
	conn := &Connection{Transport: "ssh", native: &sshSession{Node: config.Node{Address: "fixture"}, Runner: "/runner", Credentials: &access.Credentials{}}}
	defer conn.Close()
	job := config.Job{ID: id, Modules: map[string]modules.Module{"greeting": {Kind: modules.KindProjectStarlark, Files: []modules.ModuleFile{
		{Path: modules.ModuleScriptFile, Mode: 0644, Asset: kept}, {Path: modules.ModuleDescriptionFile, Mode: 0644, Asset: sent}}}},
		NodeTasks: []config.Task{
			{Module: "download", ModuleInputs: &modules.File{URL: "https://downloads.example.com/app", SHA256: download.SHA256, Path: "/usr/local/bin/app", Mode: "0755", Asset: &download}},
			{Module: "file", ModuleInputs: &modules.File{Source: "files/app.conf", LocalPath: "/project/files/app.conf", Path: "/etc/app.conf", Mode: "0644", Asset: &project}},
		}}
	// The job names the files to keep, as its preparation does.
	var err error
	if job.KeptFiles, err = config.KeptFileAssets(job.NodeTasks, job.Modules); err != nil {
		t.Fatal(err)
	}
	if err := c.deliverAssets(ctx, r, job, &Entry{Node: "fixture"}, func(Entry) error { return nil }, conn); err != nil {
		t.Fatal(err)
	}
	asked, _ := os.ReadFile(request)
	for _, ref := range []assets.Ref{kept, sent, download} {
		if !strings.Contains(string(asked), ref.SHA256) {
			t.Errorf("did not ask whether the node keeps %s:\n%s", ref.SHA256, asked)
		}
	}
	if strings.Contains(string(asked), project.SHA256) {
		t.Errorf("asked whether the node keeps a file from the project:\n%s", asked)
	}
	commands, _ := os.ReadFile(trace)
	uploaded := []string{}
	for _, ref := range []assets.Ref{kept, sent, download, project} {
		if strings.Contains(string(commands), ref.SHA256) {
			uploaded = append(uploaded, ref.SHA256)
		}
	}
	if strings.Count(string(commands), " upload ") != 2 || !slices.Equal(uploaded, []string{sent.SHA256, project.SHA256}) {
		t.Fatalf("sent a file the node keeps, or not the ones it lacks:\n%s", commands)
	}
}

// The notice for a node reached over SSH after HTTPS failed gives the HTTPS
// failure and what the node, reached over SSH, says about its side: a stopped
// mmx-runner HTTPS service, ufw without a rule for the port, or neither. Its
// advice names the host and port and the likely causes without choosing one.
func TestHTTPSFallbackReasonSaysWhatTheNodeFound(t *testing.T) {
	address, timedOut := "https://10.20.0.2:7443", &url.Error{Op: "Get", URL: "https://10.20.0.2:7443/v1/info", Err: &net.OpError{Op: "dial", Net: "tcp", Err: os.ErrDeadlineExceeded}}
	failed := "MMX could not connect to https://10.20.0.2:7443 (connection timed out)"
	advice := "To use HTTPS, check that the controller machine can reach 10.20.0.2 on TCP port 7443: the network between them may be down, or a firewall may block the port. To make SSH the stated choice instead, set connection: ssh_only on the node in the inventory, or pass --connection ssh_only when this controller machine cannot reach the node's HTTPS address."
	for _, tc := range []struct {
		name  string
		state nodeHTTPSState
		want  string
	}{
		{"stopped", nodeHTTPSState{apiState: "inactive"},
			failed + ". mmx-runner, MMX's HTTPS service on this node, is not running: its systemd unit mmx-https-api.service is inactive. MMX is using SSH for this node; journalctl -u mmx-https-api.service on this node says why mmx-runner does not run."},
		{"start failed", nodeHTTPSState{apiState: "failed", startErr: errors.New("exit status 1: start request repeated too quickly")},
			failed + ". mmx-runner, MMX's HTTPS service on this node, is not running: its systemd unit mmx-https-api.service is failed, and starting it failed: exit status 1: start request repeated too quickly. MMX is using SSH for this node;"},
		{"firewalld without a rule", nodeHTTPSState{apiState: "active", firewallWithoutRule: "firewalld"},
			failed + ". mmx-runner, MMX's HTTPS service on this node, is running, and firewalld's default zone on this node has no rule for port 7443. MMX is using SSH for this node. " + advice},
		{"ufw without a rule", nodeHTTPSState{apiState: "active", firewallWithoutRule: "ufw"},
			failed + ". mmx-runner, MMX's HTTPS service on this node, is running, and ufw on this node is active with no rule that names port 7443. MMX is using SSH for this node. " + advice},
		{"process runs", nodeHTTPSState{apiState: "active"},
			failed + ". mmx-runner, MMX's HTTPS service on this node, is running. MMX is using SSH for this node. " + advice},
		{"unknown", nodeHTTPSState{checkErr: errors.New("sudo: a password is required")},
			failed + ", and MMX could not check this node over SSH: sudo: a password is required. MMX is using SSH for this node. " + advice},
	} {
		got := tc.state.fallbackReason(address, timedOut)
		if !strings.HasPrefix(got, tc.want) {
			t.Errorf("%s:\n%s\nwant\n%s", tc.name, got, tc.want)
		}
		for _, guess := range []string{"blocks", "something between"} {
			if strings.Contains(got, guess) {
				t.Errorf("%s names a cause MMX did not check (%q): %s", tc.name, guess, got)
			}
		}
	}
	// An HTTPS address without a port is reached on 443.
	if got := (nodeHTTPSState{apiState: "active"}).fallbackReason("https://app02.example.net", timedOut); !strings.Contains(got, "reach app02.example.net on TCP port 443:") {
		t.Error(got)
	}
}

// The HTTPS failure names the address MMX tried and, for a connection that
// never opened, the reason in plain words rather than Go's error text.
func TestHTTPSFailureSaysWhatMMXSaw(t *testing.T) {
	address := "https://10.20.0.2:7443"
	request := func(err error) error { return &url.Error{Op: "Get", URL: address + "/v1/info", Err: err} }
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"timed out", request(&net.OpError{Op: "dial", Net: "tcp", Err: os.ErrDeadlineExceeded}),
			"MMX could not connect to https://10.20.0.2:7443 (connection timed out)"},
		{"refused", request(&net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}),
			"MMX could not connect to https://10.20.0.2:7443 (connection refused)"},
		{"no answer", request(context.DeadlineExceeded),
			"MMX got no answer in time from https://10.20.0.2:7443"},
		{"closed", request(io.EOF),
			"The connection to https://10.20.0.2:7443 closed before an answer came"},
		{"lost", request(&net.OpError{Op: "read", Net: "tcp", Err: os.NewSyscallError("read", syscall.ECONNRESET)}),
			"A request to https://10.20.0.2:7443 failed (read tcp: read: connection reset by peer)"},
	} {
		if got := httpsFailure(address, tc.err); got != tc.want {
			t.Errorf("%s:\n%s\nwant\n%s", tc.name, got, tc.want)
		}
	}
}

func TestSavedSSHOnlyInventoryConnectsOverSSHOnResume(t *testing.T) {
	c, original := sshNode(t, `echo unexpected runner call >&2; exit 1`)
	r, err := c.New(context.Background(), original.Project, original.Inventory, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	// Mutating the caller's inventory cannot change the recorded choice.
	original.Inventory.Nodes["a"] = config.Node{Address: "changed.example.net"}
	saved, err := c.Load(r.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := c.connect(context.Background(), saved, saved.Inventory.Nodes["a"], "", machine{})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if conn.ConnectionUsed() != "ssh" || conn.FallbackReason != "" {
		t.Fatal(conn.ConnectionUsed(), conn.FallbackReason)
	}
}

// The probe reads the machine ID and the boot ID over one fresh, read-only SSH
// connection. A saved identity of another machine is a replacement. A saved
// identity of the controller machine's machine ID is probed too: a clone of the
// controller machine's image shares the machine ID, and only its boot ID tells
// it from the controller machine.
func TestInventorySSHProbeUsesFreshConnectionAndSavedIdentity(t *testing.T) {
	const machineID, bootID = "0123456789abcdef0123456789abcdef", "0b0b0b0b-0000-4000-8000-000000000000"
	hash := sha256.Sum256([]byte(machineID))
	nodeID := hex.EncodeToString(hash[:])
	for _, saved := range []string{"", nodeID, strings.Repeat("b", 64), testControllerMachine.MachineIDHash} {
		t.Run("saved="+saved, func(t *testing.T) {
			trace := filepath.Join(t.TempDir(), "ssh-arguments")
			fakeSSH(t, `case "$1" in
-G) printf 'hostname fixture\nuser root\nport 22\n'; exit 0;;
-F) exit 0;;
esac
printf '%s\n' "$@" >> `+quote(trace)+`
for command; do :; done
if test "$command" != 'cat /etc/machine-id /proc/sys/kernel/random/boot_id'; then echo 'probe ran a setup or sudo command' >&2; exit 1; fi
printf '%s\n%s\n' `+quote(machineID)+` `+quote(bootID))
			c := Controller{State: t.TempDir()}
			destination := sha256.Sum256([]byte("fixture\x0022\x00root"))
			if saved != "" && saved != testControllerMachine.MachineIDHash {
				if err := os.MkdirAll(filepath.Join(c.State, "nodes"), 0700); err != nil {
					t.Fatal(err)
				}
				raw, _ := json.Marshal(endpoint.Info{NodeID: saved})
				if err := os.WriteFile(filepath.Join(c.State, "nodes", hex.EncodeToString(destination[:])+".json"), raw, 0600); err != nil {
					t.Fatal(err)
				}
			}
			r := &Run{Inventory: config.Inventory{Nodes: map[string]config.Node{"db01": {Address: "fixture", KnownHosts: "/known-hosts"}}}}
			if saved == testControllerMachine.MachineIDHash {
				// SSH-only nodes have saved run identities without a saved HTTPS pin.
				r.nodeIdentities = &sync.Map{}
				r.nodeIdentities.Store("db01", saved)
			}
			got, err := c.probeInventorySSH(context.Background(), r, "db01")
			var replaced *replacedError
			switch saved {
			case strings.Repeat("b", 64), testControllerMachine.MachineIDHash:
				if !errors.As(err, &replaced) {
					t.Fatal("accepted a replacement node", got, err)
				}
			default:
				if err != nil || got != (bootedMachine{nodeID, bootID}) {
					t.Fatal(got, err)
				}
			}
			raw, err := os.ReadFile(trace)
			if err != nil {
				t.Fatal("the probe did not reach the node", err)
			}
			for _, option := range []string{"ControlMaster=no", "ControlPath=none", "ControlPersist=no", "StrictHostKeyChecking=yes", "NoHostAuthenticationForLocalhost=no", "UserKnownHostsFile=/known-hosts"} {
				if strings.Count(string(raw), option+"\n") != 1 {
					t.Fatalf("probe did not use exactly one ordinary, fresh SSH connection with %s: %s", option, raw)
				}
			}
		})
	}
}

// portChange sets up the node as a server that MMX first reached on SSH port
// 22 with a password: MMX made its key, and the saved node identity belongs to
// that name. The server then moved sshd to port 2222 and accepts only MMX's
// key. The node's key is the only key it accepts, as with password login off.
func (f *connectFixture) portChange(t *testing.T) (oldKey, newKey string) {
	t.Helper()
	oldKey, err := access.KeyPath(nodeDestination(sshSettings{Hostname: "fixture", Port: "22", User: "admin"}))
	if err != nil {
		t.Fatal(err)
	}
	newKey, err = access.KeyPath(nodeDestination(sshSettings{Hostname: "fixture", Port: "2222", User: "admin"}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := access.EnsureKey(oldKey); err != nil {
		t.Fatal(err)
	}
	private, _ := os.ReadFile(oldKey)
	dir := f.c.RunnerDir
	for file, content := range map[string][]byte{"authorized-key": private, "port": []byte("2222")} {
		if err := os.WriteFile(filepath.Join(dir, file), content, 0600); err != nil {
			t.Fatal(err)
		}
	}
	f.saved = filepath.Join(f.c.State, "nodes", nodeDestination(sshSettings{Hostname: "fixture", Port: "2222", User: "admin"})+".json")
	return oldKey, newKey
}

// After the SSH port changes, MMX uses its own key again once verified SSH
// shows the same machine as the saved node identity; the key is copied, so
// the old name keeps its key.
func TestConnectAdoptsManagedKeyAfterSSHPortChange(t *testing.T) {
	f := newConnectFixture(t, false, false, currentInfo)
	notices := f.notices()
	oldKey, newKey := f.portChange(t)
	f.saveAt(t, nodeDestination(sshSettings{Hostname: "fixture", Port: "22", User: "admin"}), f.info)
	conn, err := f.connect(t)
	if err != nil || conn.Transport != "https" {
		t.Fatal(conn, err, *notices)
	}
	for _, suffix := range []string{"", ".pub"} {
		first, _ := os.ReadFile(oldKey + suffix)
		second, err := os.ReadFile(newKey + suffix)
		if err != nil || len(first) == 0 || !bytes.Equal(first, second) {
			t.Fatalf("key file %q was not copied: %v", suffix, err)
		}
	}
	if st, err := os.Stat(newKey); err != nil || st.Mode().Perm() != 0600 {
		t.Fatal("private key mode", st, err)
	}
	if got := strings.Join(*notices, "\n"); !strings.Contains(got, "SSH port 22") || !strings.Contains(got, "SSH port 2222") {
		t.Fatal("the notice names both ports:", got)
	}
	if _, err := os.Stat(f.saved); err != nil {
		t.Fatal("the new saved node identity was not written", err)
	}
}

// Two servers behind one address can differ only by port. A key that works
// at another port but reaches another machine, or a port with no saved node
// identity, is never adopted: MMX behaves as before and cannot log in.
func TestConnectAdoptsNoKeyWithoutProofOfTheSameMachine(t *testing.T) {
	for _, tt := range []struct{ name, savedMachine string }{
		{"another machine", strings.Repeat("9", 64)},
		{"no saved node identity", ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newConnectFixture(t, false, false, currentInfo)
			_, newKey := f.portChange(t)
			if tt.savedMachine != "" {
				saved := f.info
				saved.NodeID = tt.savedMachine
				f.saveAt(t, nodeDestination(sshSettings{Hostname: "fixture", Port: "22", User: "admin"}), saved)
			}
			if _, err := f.connect(t); err == nil {
				t.Fatal("connect succeeded without an authorized key")
			}
			if _, err := os.Stat(newKey); !os.IsNotExist(err) {
				t.Fatal("a key was adopted without proof of the same machine", err)
			}
		})
	}
}

func (f *connectFixture) saveAt(t *testing.T, destination string, info endpoint.Info) {
	t.Helper()
	path := filepath.Join(f.c.State, "nodes", destination+".json")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(info)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}
