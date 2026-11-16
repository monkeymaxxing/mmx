package controller

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"mmx/internal/config"
	"mmx/internal/modules"
	"mmx/internal/protocol"
	"mmx/internal/release"
)

func fleet(t *testing.T, fleetStages ...config.FleetStage) (Controller, *Run) {
	t.Helper()
	c := Controller{State: t.TempDir()}
	p := config.Project{Project: "fleet", MaxNodesAtOnce: 2, FleetStages: fleetStages}
	inv := config.Inventory{Nodes: map[string]config.Node{"a": {Address: "a"}, "b": {Address: "b"}, "c": {Address: "c"}}}
	r, err := c.New(context.Background(), p, inv, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	return c, r
}
func receive(t *testing.T, c <-chan string) string {
	t.Helper()
	select {
	case s := <-c:
		return s
	case <-time.After(30 * time.Second):
		t.Fatal("scheduler did not start node")
		return ""
	}
}
func TestStreamingAndFixedBatches(t *testing.T) {
	for _, batch := range []bool{false, true} {
		t.Run(fmt.Sprint(batch), func(t *testing.T) {
			c, r := fleet(t, config.FleetStage{ID: "setup", Nodes: "all", OnNodeFailure: "continue", WaitForAllInBatch: batch})
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			started := make(chan string, 3)
			a, b := make(chan struct{}), make(chan struct{})
			c.executeNode = func(ctx context.Context, _ *Run, _ config.FleetStage, en Entry, _ bool, save func(Entry) error) (Entry, error) {
				started <- en.Node
				en.Response.Status = "running"
				if err := save(en); err != nil {
					return en, err
				}
				var wait <-chan struct{}
				if en.Node == "a" {
					wait = a
				}
				if en.Node == "b" {
					wait = b
				}
				if wait != nil {
					select {
					case <-wait:
					case <-ctx.Done():
						return en, ctx.Err()
					}
				}
				en.Response.Status = "succeeded"
				return en, nil
			}
			done := make(chan error, 1)
			go func() { done <- c.Execute(ctx, r, false) }()
			first, second := receive(t, started), receive(t, started)
			if first == second || first == "c" || second == "c" {
				t.Fatalf("first group: %s %s", first, second)
			}
			close(a)
			if batch {
				select {
				case h := <-started:
					t.Fatalf("batch admitted %s before b completed", h)
				case <-time.After(50 * time.Millisecond):
				}
				close(b)
				if receive(t, started) != "c" {
					t.Fatal("missing next batch")
				}
			} else {
				if receive(t, started) != "c" {
					t.Fatal("did not fill freed slot")
				}
				close(b)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			if r.Status != "succeeded" {
				t.Fatal(r.Status)
			}
		})
	}
}

func TestFailurePoliciesDependenciesAndRecordedRetry(t *testing.T) {
	for _, policy := range []string{"continue", "stop_fleet_stage", "stop_run"} {
		t.Run(policy, func(t *testing.T) {
			c, r := fleet(t,
				config.FleetStage{ID: "setup", Nodes: "all", MaxNodesAtOnce: "1", OnNodeFailure: policy},
				config.FleetStage{ID: "dependent", Nodes: "all", Requires: config.Requires{AllNodes: []string{"setup"}}, OnNodeFailure: "continue"},
				config.FleetStage{ID: "independent", Nodes: "all", OnNodeFailure: "continue"},
			)
			fail := true
			counts := map[string]int{}
			var mu sync.Mutex
			c.executeNode = func(_ context.Context, _ *Run, s config.FleetStage, en Entry, _ bool, _ func(Entry) error) (Entry, error) {
				mu.Lock()
				counts[s.ID+"/"+en.Node]++
				mu.Unlock()
				en.Response.Status = "succeeded"
				if s.ID == "setup" && en.Node == "a" && fail {
					en.Response.Status = "failed"
				}
				return en, nil
			}
			if err := c.Execute(context.Background(), r, false); err != nil {
				t.Fatal(err)
			}
			if r.Status != "failed" {
				t.Fatal("failure hidden")
			}
			if counts["dependent/a"] != 0 {
				t.Fatal("dependency admitted despite failed prerequisite")
			}
			if policy == "continue" && counts["setup/b"] != 1 {
				t.Fatal("independent node stopped")
			}
			if policy != "continue" && counts["setup/b"] != 0 {
				t.Fatal("stop did not prevent new admission")
			}
			if policy == "stop_run" && counts["independent/a"] != 0 {
				t.Fatal("later fleet stage admitted after stop_run")
			}
			if counts["independent/a"] != 0 {
				t.Fatal("failed node resumed at an independent later fleet_stage")
			}
			if policy != "stop_run" && counts["independent/b"] != 1 {
				t.Fatal("independent later fleet stage was blocked on an eligible node")
			}
			// Only unmet prerequisites are blocked; work a policy kept from
			// starting is stopped with its cause and was never attempted.
			unstarted := map[string]bool{"setup/b": policy != "continue", "dependent/a": policy == "stop_run", "independent/a": true}
			for key, stopped := range unstarted {
				fleetStage, managedNode, _ := strings.Cut(key, "/")
				en := entry(t, r, fleetStage, managedNode)
				want, reason := "succeeded", ""
				switch {
				case stopped:
					want, reason = "stopped", "not started: on_node_failure "+policy+" after setup failed on a"
					if fleetStage == "independent" && policy != "stop_run" {
						reason = "not started: earlier fleet stage setup on a is failed"
					}
				case fleetStage == "dependent":
					want, reason = "blocked", "requires all_nodes fleet stage setup to succeed (a is failed)"
				}
				if en.NodeJobAttempt != 0 && stopped || en.Response.Status != want || en.Response.Error != reason {
					t.Fatalf("%s: attempt %d %+v", key, en.NodeJobAttempt, en.Response)
				}
			}
			// Retry must work after loading the separate on-disk records too.
			var err error
			r, err = c.Load(r.ID, true)
			if err != nil {
				t.Fatal(err)
			}
			fail = false
			if err := c.Execute(context.Background(), r, true); err != nil {
				t.Fatal(err)
			}
			if r.Status != "succeeded" {
				t.Fatal(r.Status)
			}
			if counts["setup/a"] != 2 || counts["setup/b"] != 1 || counts["dependent/a"] != 1 || counts["independent/a"] != 1 {
				t.Fatalf("retry repeated completed work or retained old stop: %v", counts)
			}
		})
	}
}

// failed_nodes_allowed lets the fleet stage continue until one more node than it
// allows has failed; then the policy stops it as without an allowance. A
// percentage counts the fleet stage's selected nodes, rounded down, and retry counts
// only its own failures against the same allowance.
func TestFailedNodesAllowedDelaysTheStopPolicy(t *testing.T) {
	for _, allowance := range []config.FailureAllowance{"1", "50%"} {
		t.Run(string(allowance), func(t *testing.T) {
			c, r := fleet(t,
				config.FleetStage{ID: "patch", Nodes: "all", MaxNodesAtOnce: "1", OnNodeFailure: "stop_run", FailedNodesAllowed: allowance},
				config.FleetStage{ID: "report", Nodes: "all", OnNodeFailure: "continue"},
			)
			failing := map[string]bool{"a": true, "b": true}
			var mu sync.Mutex
			counts := map[string]int{}
			c.executeNode = func(_ context.Context, _ *Run, s config.FleetStage, en Entry, _ bool, _ func(Entry) error) (Entry, error) {
				mu.Lock()
				defer mu.Unlock()
				counts[s.ID+"/"+en.Node]++
				en.NodeJobAttempt++
				en.Response.Status = "succeeded"
				if s.ID == "patch" && failing[en.Node] {
					en.Response.Status = "failed"
				}
				return en, nil
			}
			execute(t, c, r, false)
			if counts["patch/b"] != 1 {
				t.Fatal("the first failure within the allowance stopped the fleet_stage", counts)
			}
			cause := "not started: on_node_failure stop_run after patch failed on b (2 failed nodes; failed_nodes_allowed " + string(allowance) + " allows 1)"
			for _, key := range []string{"patch/c", "report/a", "report/b", "report/c"} {
				fleetStage, managedNode, _ := strings.Cut(key, "/")
				if en := entry(t, r, fleetStage, managedNode); en.NodeJobAttempt != 0 || en.Response.Status != "stopped" || en.Response.Error != cause {
					t.Fatalf("%s: %+v", key, en.Response)
				}
			}
			loaded, err := c.Load(r.ID, true)
			if err != nil {
				t.Fatal(err)
			}
			failing["b"] = false
			execute(t, c, loaded, true)
			if counts["patch/c"] != 1 || counts["report/b"] != 1 || counts["report/c"] != 1 || counts["report/a"] != 0 {
				t.Fatal("retry counted the first attempt's failures against the allowance", counts)
			}
			if a := entry(t, loaded, "report", "a").Response; a.Status != "stopped" || !strings.Contains(a.Error, "earlier fleet stage patch on a is failed") {
				t.Fatalf("%+v", a)
			}
		})
	}
}

// Work a retry does not admit keeps an earlier job's last real outcome, so
// status and a later retry still reconcile it.
func TestRetryKeepsUncertainEntryWhenNotAdmitted(t *testing.T) {
	for _, variant := range []string{"stop", "stop_fleet_stage"} {
		t.Run(variant, func(t *testing.T) {
			fleetStages := []config.FleetStage{
				{ID: "prep", Nodes: "all", OnNodeFailure: "continue"},
				{ID: "deploy", Nodes: "all", MaxNodesAtOnce: "1", OnNodeFailure: "continue", Requires: config.Requires{SameNode: []string{"prep"}}},
			}
			if variant == "stop_fleet_stage" {
				fleetStages[1].OnNodeFailure = "stop_fleet_stage"
			}
			c, r := fleet(t, fleetStages...)
			retrying := false
			c.executeNode = func(_ context.Context, _ *Run, s config.FleetStage, en Entry, _ bool, _ func(Entry) error) (Entry, error) {
				en.Response.Status = "succeeded"
				switch {
				case !retrying && s.ID == "prep" && en.Node == "a":
					en.Response.Status = "failed"
				case !retrying && s.ID == "deploy" && en.Node == "b":
					// The controller lost contact after publishing this job.
					en.JobID, en.NodeJobAttempt = "J1", 1
					en.Response = protocol.Response{ID: "J1", Status: "unknown", Error: "connection lost"}
				case retrying && s.ID == "deploy" && en.Node == "a":
					if variant == "stop" {
						if err := c.requestStop(r.ID); err != nil {
							t.Error(err)
						}
					}
					en.Response.Status = "failed"
				case retrying && s.ID == "deploy":
					t.Errorf("admitted %s after the stop", en.Node)
				}
				return en, nil
			}
			execute(t, c, r, false)
			loaded, err := c.Load(r.ID, true)
			if err != nil {
				t.Fatal(err)
			}
			retrying = true
			execute(t, c, loaded, true)
			if loaded, err = c.Load(r.ID, false); err != nil {
				t.Fatal(err)
			}
			// The unanswered job keeps the run unfinished and active.
			want := map[string]string{"stop": "stopping", "stop_fleet_stage": "running"}[variant]
			if b := entry(t, loaded, "deploy", "b"); b.JobID != "J1" || b.Response.Status != "unknown" || loaded.Status != want {
				t.Fatalf("uncertain job J1 was overwritten: %+v; run %s", b.Response, loaded.Status)
			}
		})
	}
}

// A new stop_run failure during retry leaves a later fleet stage's uncertain entry
// for status and retry to reconcile.
func TestRetryPolicyStopKeepsUncertainEntry(t *testing.T) {
	c, r := fleet(t,
		config.FleetStage{ID: "base", Nodes: []any{"a", "b"}, OnNodeFailure: "continue"},
		config.FleetStage{ID: "gate", Nodes: []any{"a", "b"}, OnNodeFailure: "stop_run", Requires: config.Requires{SameNode: []string{"base"}}},
		config.FleetStage{ID: "deploy", Nodes: []any{"b", "c"}},
	)
	// Original run: base failed on a, gate was blocked on a and succeeded on b,
	// deploy on b was admitted, and the controller was killed.
	set := func(fleetStage, managedNode, job string, response protocol.Response) {
		for _, en := range r.Entries {
			if en.FleetStage == fleetStage && en.Node == managedNode {
				en.Response, en.JobID = response, job
				if job != "" {
					en.NodeJobAttempt = 1
				}
				if err := c.saveEntry(r.ID, en); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	set("base", "a", "j1", protocol.Response{Status: "failed"})
	set("base", "b", "j2", protocol.Response{Status: "succeeded"})
	set("gate", "a", "", protocol.Response{Status: "blocked", Error: "requires same_node fleet stage base to succeed on a"})
	set("gate", "b", "j3", protocol.Response{Status: "succeeded"})
	set("deploy", "b", "j4", protocol.Response{ID: "j4", Status: "running"})
	c.executeNode = func(_ context.Context, _ *Run, s config.FleetStage, en Entry, _ bool, _ func(Entry) error) (Entry, error) {
		if s.ID == "deploy" {
			t.Errorf("admitted deploy on %s after stop_run", en.Node)
		}
		en.Response.Status = "succeeded"
		if s.ID == "gate" {
			en.Response.Status = "failed" // a new failure under stop_run
		}
		return en, nil
	}
	loaded, err := c.Load(r.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	execute(t, c, loaded, true)
	b, cc := entry(t, loaded, "deploy", "b"), entry(t, loaded, "deploy", "c")
	if b.JobID != "j4" || b.Response.Status != "running" || loaded.Status != "running" {
		t.Fatalf("uncertain job j4 was overwritten: %+v; run %s", b.Response, loaded.Status)
	}
	if cc.Response.Status != "stopped" || cc.Response.Error != "not started: on_node_failure stop_run after gate failed on a" {
		t.Fatalf("unstarted later fleet_stage: %+v", cc.Response)
	}
}

// stop_fleet stage stops admission when the controller observes the required failure,
// before the failed node's failure handling and cleanup finish.
func TestStopFleetStageTriggersOnObservedFailureBeforeCleanup(t *testing.T) {
	c, r := fleet(t, config.FleetStage{ID: "setup", Nodes: "all", MaxNodesAtOnce: "2", OnNodeFailure: "stop_fleet_stage"})
	cleanup, bDone := make(chan struct{}), make(chan struct{})
	started := make(chan string, 3)
	c.executeNode = func(_ context.Context, _ *Run, _ config.FleetStage, en Entry, _ bool, save func(Entry) error) (Entry, error) {
		started <- en.Node
		switch en.Node {
		case "a":
			// The operation's verify failed; on_failure and cleanup still run.
			en.JobID, en.NodeJobAttempt = "job-a", 1
			en.Response = protocol.Response{ID: "job-a", Status: "running", Results: []protocol.Result{{Task: "deploy/verify/health", Status: "failed", Reason: "unhealthy"}}}
			if err := save(en); err != nil {
				return en, err
			}
			close(bDone) // b finishes while a is still cleaning up
			<-cleanup
			en.Response.Status = "failed"
		case "b":
			<-bDone
			en.Response.Status = "succeeded"
		default:
			en.Response.Status = "succeeded"
		}
		return en, nil
	}
	done := make(chan error, 1)
	go func() { done <- c.Execute(context.Background(), r, false) }()
	receive(t, started)
	receive(t, started)
	select {
	case h := <-started:
		t.Errorf("admitted %s after a's required failure was observed", h)
	case <-time.After(300 * time.Millisecond):
	}
	close(cleanup)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := entry(t, r, "setup", "c").Response; got.Status != "stopped" || got.Error != "not started: on_node_failure stop_fleet_stage after setup failed on a" {
		t.Fatalf("%+v", got)
	}
}

// Retry reconciles an earlier job still in cleanup after a failure the first
// run already observed and stopped on; that failure must not stop the retry.
func TestRetryReconcilingEarlierFailedJobDoesNotStopFleetStage(t *testing.T) {
	c, r := fleet(t, config.FleetStage{ID: "setup", Nodes: []any{"a", "b"}, MaxNodesAtOnce: "1", OnNodeFailure: "stop_fleet_stage"})
	old := protocol.Response{ID: "J1", Status: "running", Results: []protocol.Result{{Task: "deploy/verify", Status: "failed"}}}
	retrying := false
	c.executeNode = func(_ context.Context, _ *Run, _ config.FleetStage, en Entry, _ bool, save func(Entry) error) (Entry, error) {
		if en.Node == "a" {
			en.JobID, en.NodeJobAttempt, en.Response = "J1", 1, old
			if err := save(en); err != nil { // old job observed in cleanup (waitEntry's first save)
				return en, err
			}
			if !retrying {
				en.Response.Status = "unknown" // contact lost in the first run
				return en, nil
			}
			// Old job finished failed; the retry starts and completes a new attempt.
			en.PreviousNodeJobAttempts = append(en.PreviousNodeJobAttempts, Attempt{JobID: "J1", Response: protocol.Response{ID: "J1", Status: "failed"}})
			en.JobID, en.NodeJobAttempt, en.Response = "J2", 2, protocol.Response{ID: "J2", Status: "succeeded"}
			return en, nil
		}
		en.NodeJobAttempt++
		en.Response.Status = "succeeded"
		return en, nil
	}
	execute(t, c, r, false)
	if b := entry(t, r, "setup", "b").Response; b.Status != "stopped" {
		t.Fatalf("setup: first run should stop admission, b=%+v", b)
	}
	loaded, err := c.Load(r.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	retrying = true
	execute(t, c, loaded, true)
	if b := entry(t, loaded, "setup", "b").Response; b.Status != "succeeded" {
		t.Fatalf("retry was stopped by the historical failure: b=%+v run=%s", b, loaded.Status)
	}
}

func TestFailedNodeDoesNotResumeAtNextFleetStage(t *testing.T) {
	dir := t.TempDir()
	fail := filepath.Join(dir, "fail")
	if err := os.WriteFile(fail, nil, 0600); err != nil {
		t.Fatal(err)
	}
	c, r := fleet(t,
		config.FleetStage{ID: "fail", Nodes: []any{"a", "b"}, OnNodeFailure: "continue", NodeTasks: []config.Task{{Module: "shell", ModuleInputs: &modules.Shell{Env: map[string]any{"MMX_TEST_HOST": "{{ .node.name }}"}, Script: "if [ \"$MMX_TEST_HOST\" = a ] && [ -f " + quote(fail) + " ]; then exit 1; fi"}}}},
		config.FleetStage{ID: "later", Nodes: []any{"a", "b"}, NodeTasks: []config.Task{{Module: "shell", ModuleInputs: &modules.Shell{Env: map[string]any{"MMX_TEST_HOST": "{{ .node.name }}"}, Script: "touch " + quote(dir) + "/\"$MMX_TEST_HOST\""}}}},
	)
	counts := map[string]int{}
	var mu sync.Mutex
	localNodes(&c, func(fleetStage config.FleetStage, en Entry) *protocol.Response {
		mu.Lock()
		counts[fleetStage.ID+"/"+en.Node]++
		mu.Unlock()
		return nil
	})
	execute(t, c, r, false)
	if entry(t, r, "fail", "a").Response.Status != "failed" {
		t.Fatal("fixture did not fail the first node fleet_stage")
	}
	if _, err := os.Stat(filepath.Join(dir, "a")); !os.IsNotExist(err) {
		t.Fatalf("failed node resumed normal work in the next fleet_stage: later=%+v, stat=%v", entry(t, r, "later", "a").Response, err)
	}
	if response := entry(t, r, "later", "a").Response; response.Status != "stopped" || !strings.Contains(response.Error, "fail") {
		t.Fatal("later work must name the earlier failure", response)
	}
	if _, err := os.Stat(filepath.Join(dir, "b")); err != nil {
		t.Fatal("healthy node's independent later work was blocked", err)
	}
	if err := os.Remove(fail); err != nil {
		t.Fatal(err)
	}
	loaded, err := c.Load(r.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	execute(t, c, loaded, true)
	if loaded.Status != "succeeded" || counts["fail/a"] != 2 || counts["later/a"] != 1 || counts["fail/b"] != 1 || counts["later/b"] != 1 {
		t.Fatal("retry did not clear the resolved failure while preserving successful work", loaded.Status, counts)
	}
}

// A lost answer does not establish completion of the earlier node fleet stage. Its
// original job remains available to status and retry before later work starts.
func TestUnconfirmedNodeJobStopsLaterWorkUntilReconciled(t *testing.T) {
	c, r := fleet(t,
		config.FleetStage{ID: "first", Nodes: []any{"a", "b"}},
		config.FleetStage{ID: "later", Nodes: []any{"a", "b"}},
	)
	retrying := false
	counts := map[string]int{}
	var mu sync.Mutex
	c.executeNode = func(_ context.Context, _ *Run, s config.FleetStage, en Entry, _ bool, _ func(Entry) error) (Entry, error) {
		mu.Lock()
		counts[s.ID+"/"+en.Node]++
		mu.Unlock()
		if s.ID == "first" && en.Node == "a" {
			if retrying && (en.JobID != "original" || en.Response.Status != "unknown") {
				t.Error("lost the original job to reconcile", en)
			}
			en.JobID = "original"
		}
		en.Response = protocol.Response{ID: en.JobID, Status: "succeeded"}
		if !retrying && s.ID == "first" && en.Node == "a" {
			en.Response.Status = "unknown"
		}
		return en, nil
	}
	execute(t, c, r, false)
	if r.Status != "running" || counts["later/a"] != 0 || counts["later/b"] != 1 || entry(t, r, "later", "a").Response.Status != "stopped" {
		t.Fatal("unconfirmed earlier work did not hold later admission", r.Status, counts, r.Entries)
	}
	loaded, err := c.Load(r.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	retrying = true
	execute(t, c, loaded, true)
	if loaded.Status != "succeeded" || counts["first/a"] != 2 || counts["later/a"] != 1 || counts["first/b"] != 1 || counts["later/b"] != 1 {
		t.Fatal("reconciliation failed to release later work", loaded.Status, counts)
	}
}

// A saved run started by another MMX release is refused before any work and
// before any node is contacted; a run from this release loads for resume.
func TestResumeRefusesARunStartedByAnotherRelease(t *testing.T) {
	c, r := fleet(t, config.FleetStage{ID: "setup", Nodes: "all"})
	if _, err := c.Load(r.ID, true); err != nil {
		t.Fatalf("a run from this release must resume: %v", err)
	}
	current := release.Version
	defer func() { release.Version = current }()
	release.Version = "0.0.9"
	other, err := c.New(context.Background(), config.Project{Project: "fleet", MaxNodesAtOnce: 2, FleetStages: []config.FleetStage{{ID: "setup", Nodes: "all"}}}, config.Inventory{Nodes: map[string]config.Node{"a": {Address: "a"}}}, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	release.Version = current
	_, err = c.Load(other.ID, true)
	want := "this playbook run was started by MMX 0.0.9, but this mmx program is version " + current +
		". A run finished by two MMX releases can leave a node's last-successful record skipped. " +
		"Resume the run with mmx of release 0.0.9 (such as the previous install, if you kept it), or start a new run with mmx run"
	if err == nil || err.Error() != want {
		t.Fatalf("got %v, want %q", err, want)
	}
}
