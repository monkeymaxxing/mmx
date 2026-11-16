package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"mmx/internal/access"
	"mmx/internal/config"
	"mmx/internal/facts"
	"mmx/internal/modules"
	"mmx/internal/protocol"
)

func TestStopPreventsAdmissionAndRetryKeepsCompletedFleetStages(t *testing.T) {
	for _, interrupt := range []bool{false, true} {
		c, r := fleet(t, config.FleetStage{ID: "setup", Nodes: "all", MaxNodesAtOnce: "1"}, config.FleetStage{ID: "later", Nodes: "all", MaxNodesAtOnce: "1"})
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		counts := map[string]int{}
		c.executeNode = func(_ context.Context, _ *Run, fleetStage config.FleetStage, en Entry, _ bool, save func(Entry) error) (Entry, error) {
			counts[fleetStage.ID+"/"+en.Node]++
			if fleetStage.ID == "setup" && en.Node == "a" && counts["setup/a"] == 1 {
				if interrupt {
					cancel()
				} else if err := c.requestStop(r.ID); err != nil {
					return en, err
				}
			}
			en.NodeJobAttempt++
			en.Response = protocol.Response{Status: "succeeded"}
			return en, nil
		}
		err := c.Execute(ctx, r, false)
		if (interrupt && !errors.Is(err, context.Canceled)) || (!interrupt && err != nil) {
			t.Fatal(err)
		}
		if r.Status != "stopped" || len(counts) != 1 {
			t.Fatalf("stop did not prevent work: %s %v", r.Status, counts)
		}
		stored, err := c.Load(r.ID, false)
		if err != nil {
			t.Fatal(err)
		}
		if !stored.StopRequested {
			t.Fatal("stop request was not saved")
		}
		if err := c.Execute(context.Background(), stored, true); err != nil {
			t.Fatal(err)
		}
		if stored.Status != "succeeded" || len(counts) != 6 {
			t.Fatal(stored.Status, counts)
		}
		for work, count := range counts {
			if count != 1 {
				t.Fatalf("repeated %s: %d", work, count)
			}
		}
	}
}

// Nodes a stop keeps from starting are saved as stopped and never attempted:
// not started, rather than failed or blocked by a prerequisite.
func TestStopRecordsUnadmittedNodesAsNotStarted(t *testing.T) {
	c, r := fleet(t, config.FleetStage{ID: "setup", Nodes: "all", MaxNodesAtOnce: "1"})
	c.executeNode = func(_ context.Context, _ *Run, _ config.FleetStage, en Entry, _ bool, _ func(Entry) error) (Entry, error) {
		en.NodeJobAttempt, en.JobID = 1, "x"
		en.Response = protocol.Response{Status: "stopped", StopRequested: true}
		return en, c.requestStop(r.ID)
	}
	execute(t, c, r, false)
	saved, err := c.Load(r.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Status != "stopped" || saved.Entries[0].NodeJobAttempt != 1 {
		t.Fatal(saved.Status, saved.Entries[0])
	}
	for _, en := range saved.Entries[1:] {
		if en.NodeJobAttempt != 0 || en.JobID != "" || en.Response.Status != "stopped" || en.Response.Error != "not started: stop requested" {
			t.Fatalf("%s: attempt %d %+v", en.Node, en.NodeJobAttempt, en.Response)
		}
	}
}

// A stop that arrives while the last node finishes its last task leaves no
// unfinished work: the run succeeded and there is nothing to resume.
func TestStopAfterAllWorkCompletedIsNotAStoppedRun(t *testing.T) {
	c, r := fleet(t, config.FleetStage{ID: "setup", Nodes: "a"})
	c.executeNode = func(_ context.Context, _ *Run, _ config.FleetStage, en Entry, _ bool, _ func(Entry) error) (Entry, error) {
		en.NodeJobAttempt++
		en.Response = protocol.Response{Status: "succeeded", StopRequested: true}
		return en, c.requestStop(r.ID) // mmx stop from another terminal
	}
	execute(t, c, r, false)
	if r.Status != "succeeded" || r.FleetStagesAwaitingNodeSelection {
		t.Fatalf("every selected node fleet stage succeeded, but the run is %q (pending=%v)", r.Status, r.FleetStagesAwaitingNodeSelection)
	}
	if _, err := c.Load("", true); !errors.Is(err, ErrNothingToResume) {
		t.Fatalf("retry offered for completed work: %v", err)
	}
}

func TestStatusDoesNotAdvanceWorkOrWriteWhileControllerOwnsRun(t *testing.T) {
	c, r := fleet(t, config.FleetStage{ID: "setup", Nodes: "all"})
	lock, err := c.lockRun(r.ID)
	if err != nil || lock == nil {
		t.Fatal(err)
	}
	if err := c.Inspect(context.Background(), r, false); err != nil {
		t.Fatal(err)
	}
	if r.Status != "running" {
		t.Fatal(r.Status)
	}
	for _, en := range r.Entries {
		if en.JobID != "" || en.NodeJobAttempt != 0 {
			t.Fatal("status admitted work")
		}
	}
	if err := c.Execute(context.Background(), r, true); err == nil {
		t.Fatal("two schedulers own the same run")
	}
	if err := c.Inspect(context.Background(), r, true); err != nil {
		t.Fatal(err)
	}
	if r.Status != "stopping" {
		t.Fatal("claimed stop while the scheduler still owns admission", r.Status)
	}
	if err := os.Remove(filepath.Join(c.runDir(r.ID), "stop")); err != nil {
		t.Fatal(err)
	}
	unlockRun(lock)
	if err := c.Inspect(context.Background(), r, false); err != nil {
		t.Fatal(err)
	}
	if r.Status != "interrupted" {
		t.Fatal("lost controller was not recognized", r.Status)
	}
	if err := c.Inspect(context.Background(), r, true); err != nil {
		t.Fatal(err)
	}
	if r.Status != "stopped" {
		t.Fatal(r.Status)
	}
}

func TestPartialProgressReachesObserverBeforeCompletion(t *testing.T) {
	c, r := fleet(t, config.FleetStage{ID: "setup", Nodes: "a"})
	seen := make(chan struct{})
	c.Progress = func(r *Run) {
		en := r.Entries[0]
		if en.Response.Status == "running" && len(en.Response.Results) == 1 {
			select {
			case <-seen:
			default:
				close(seen)
			}
		}
	}
	c.executeNode = func(_ context.Context, _ *Run, _ config.FleetStage, en Entry, _ bool, save func(Entry) error) (Entry, error) {
		en.Response = protocol.Response{Status: "running", Results: []protocol.Result{{Task: "first", Status: "succeeded"}}}
		if err := save(en); err != nil {
			return en, err
		}
		<-seen
		en.Response.Status = "succeeded"
		return en, nil
	}
	if err := c.Execute(context.Background(), r, false); err != nil {
		t.Fatal(err)
	}
}

func TestMergeProgressAppendsNewerResults(t *testing.T) {
	facts := &facts.Snapshot{Values: map[string]any{"boot_id": "b"}}
	held := protocol.Response{ID: "job", Status: "running", Facts: facts, Results: []protocol.Result{{Task: "one"}}}
	merged, err := mergeProgress(held, protocol.Response{ID: "job", Status: "running", CurrentTask: "three", ResultsAfter: 1, Results: []protocol.Result{{Task: "two"}}})
	if err != nil || len(merged.Results) != 2 || merged.Results[1].Task != "two" || merged.Facts != facts || merged.ResultsAfter != 0 || merged.CurrentTask != "three" || len(held.Results) != 1 {
		t.Fatal(merged, err)
	}
	complete := protocol.Response{ID: "job", Status: "succeeded", Results: []protocol.Result{{Task: "only"}}}
	if merged, err := mergeProgress(held, complete); err != nil || !reflect.DeepEqual(merged, complete) {
		t.Fatal(merged, err)
	}
	if _, err := mergeProgress(held, protocol.Response{ResultsAfter: 3}); err == nil {
		t.Fatal("merged progress that skips results")
	}
}

// progressHost answers progress requests from answers in order and records
// each request; stop requests are answered with stop acknowledged.
func progressNode(t *testing.T, answer func(protocol.ProgressRequest, *http.Request) protocol.Response) (*Connection, *[]protocol.ProgressRequest, *atomic.Int32) {
	var requests []protocol.ProgressRequest
	var mu sync.Mutex
	stops := &atomic.Int32{}
	client := fakeEndpoint(t, func(r *http.Request) (*http.Response, error) {
		var out protocol.Response
		switch {
		case r.URL.Path == "/v1/progress":
			var request protocol.ProgressRequest
			body, _ := io.ReadAll(r.Body)
			if err := json.Unmarshal(body, &request); err != nil {
				t.Error(err)
			}
			mu.Lock()
			requests = append(requests, request)
			mu.Unlock()
			out = answer(request, r)
			if r.Context().Err() != nil {
				return nil, r.Context().Err()
			}
		case strings.HasPrefix(r.URL.Path, "/v1/stop/"):
			stops.Add(1)
			out = protocol.Response{ID: "job", Status: "running", StopRequested: true}
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
		}
		b, _ := json.Marshal(out)
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(b)), Header: make(http.Header)}, nil
	})
	return &Connection{Transport: "https", direct: client}, &requests, stops
}

// Each progress request says what the entry holds, and newer results from the
// node are merged into it.
func TestWaitEntryAsksForProgressPastHeldResults(t *testing.T) {
	result := func(task string) protocol.Result {
		return protocol.Result{Task: task, Status: "succeeded", Change: "unchanged"}
	}
	// The job finishes on the third request whatever it asks, so a wrong merge
	// fails the request comparison instead of looping.
	asked := 0
	conn, requests, _ := progressNode(t, func(request protocol.ProgressRequest, _ *http.Request) protocol.Response {
		asked++
		switch {
		case asked > 2:
			return protocol.Response{ID: "job", Status: "succeeded", Results: []protocol.Result{result("one"), result("two")}}
		case request.Results == 0:
			return protocol.Response{ID: "job", Status: "running", CurrentTask: "two", Results: []protocol.Result{result("one")}}
		default:
			return protocol.Response{ID: "job", Status: "running", ResultsAfter: 1, Results: []protocol.Result{result("two")}}
		}
	})
	c := Controller{State: t.TempDir()}
	en := Entry{JobID: "job", Response: protocol.Response{ID: "job", Status: "running"}}
	en, err := c.waitEntry(context.Background(), conn, &Run{ID: "run"}, config.FleetStage{}, en, func(Entry) error { return nil })
	want := []protocol.ProgressRequest{{ID: "job"}, {ID: "job", Results: 1, CurrentTask: "two"}, {ID: "job", Results: 2}}
	if err != nil || en.Response.Status != "succeeded" || len(en.Response.Results) != 2 || !reflect.DeepEqual(*requests, want) {
		t.Fatalf("%v %+v %+v", err, en.Response, *requests)
	}
}

// A stop interrupts a waiting progress request, so the node hears the stop
// without waiting for the job to change.
func TestStopInterruptsWaitingProgress(t *testing.T) {
	started := make(chan struct{})
	var stops *atomic.Int32
	conn, _, stopCount := progressNode(t, func(_ protocol.ProgressRequest, r *http.Request) protocol.Response {
		if stops.Load() == 0 {
			close(started)
			<-r.Context().Done() // nothing changes until the controller gives up waiting
			return protocol.Response{}
		}
		return protocol.Response{ID: "job", Status: "stopped", StopRequested: true}
	})
	stops = stopCount
	c := Controller{State: t.TempDir()}
	r := &Run{ID: "run", stopped: make(chan struct{})}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	done := make(chan struct{})
	var en Entry
	var err error
	go func() {
		defer close(done)
		en, err = c.waitEntry(ctx, conn, r, config.FleetStage{}, Entry{JobID: "job", Response: protocol.Response{ID: "job", Status: "running"}}, func(Entry) error { return nil })
	}()
	defer func() { cancel(); <-done }()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("blocking progress request did not start")
	}
	close(r.stopped)
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("stop did not interrupt the waiting progress request")
	}
	if err != nil || stops.Load() != 1 || en.Response.Status != "stopped" {
		t.Fatal(err, stops.Load(), en.Response)
	}
}

// A node can go down before the controller's next progress request hears of
// its reboot. A fleet stage that ends in a reboot then asks for the job's
// status until the node answers on its new boot; any other fleet stage, and a
// definite error, still end the wait.
func TestLostContactInARebootingFleetStageFollowsTheReboot(t *testing.T) {
	poll := rebootPoll
	rebootPoll = time.Millisecond
	t.Cleanup(func() { rebootPoll = poll })
	dir := t.TempDir()
	statusAsks, progressExit := filepath.Join(dir, "status-asks"), filepath.Join(dir, "progress-exit")
	fakeSSH(t, `for command; do :; done
case "$command" in
*progress*) echo 'ssh: connect to host fixture port 22: Connection refused' >&2; exit "$(cat `+quote(progressExit)+`)";;
*status*) echo ask >> `+quote(statusAsks)+`
	if [ "$(wc -l < `+quote(statusAsks)+`)" -lt 3 ]; then echo 'ssh: connect to host fixture port 22: Connection refused' >&2; exit 255; fi
	echo '{"id":"job","status":"succeeded","results":[{"task":"restart","status":"succeeded","change":"changed"}]}';;
esac
`)
	asks := func() int {
		b, _ := os.ReadFile(statusAsks)
		return strings.Count(string(b), "ask")
	}
	conn := &Connection{Transport: "ssh", native: &sshSession{Node: config.Node{Address: "fixture"}, Runner: "/runner", Credentials: &access.Credentials{}}}
	c := Controller{State: t.TempDir()}
	running := Entry{JobID: "job", Response: protocol.Response{ID: "job", Status: "running"}}
	save := func(Entry) error { return nil }
	rebooting := config.FleetStage{NodeTasks: []config.Task{{Module: "reboot", ModuleInputs: &modules.Reboot{}}}}
	for _, check := range []struct {
		fleetStage   config.FleetStage
		progressExit string
		succeeds     bool
	}{{rebooting, "255", true}, {config.FleetStage{}, "255", false}, {rebooting, "1", false}} {
		os.Remove(statusAsks)
		if err := os.WriteFile(progressExit, []byte(check.progressExit), 0600); err != nil {
			t.Fatal(err)
		}
		en, err := c.waitEntry(context.Background(), conn, &Run{ID: "run"}, check.fleetStage, running, save)
		if check.succeeds && (err != nil || en.Response.Status != "succeeded" || asks() < 3) || !check.succeeds && (err == nil || asks() != 0) {
			t.Fatal(check.progressExit, err, en.Response, asks())
		}
	}
}
