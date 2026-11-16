package controller

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"mmx/internal/config"
	"mmx/internal/modules"
	"mmx/internal/protocol"
)

// loopFleet creates a run on app servers a, b, and c and the load balancer
// lb, the helper node of the rolling loops below.
func loopFleet(t *testing.T, limit []string, fleetStages ...config.FleetStage) (Controller, *Run) {
	t.Helper()
	c := Controller{State: t.TempDir()}
	p := config.Project{Project: "fleet", MaxNodesAtOnce: 100, FleetStages: fleetStages}
	inv := config.Inventory{Nodes: map[string]config.Node{"a": {Address: "a"}, "b": {Address: "b"}, "c": {Address: "c"}, "lb": {Address: "lb"}},
		Groups: map[string]config.NodeGroup{"app": {Nodes: []string{"a", "b", "c"}}}}
	r, err := c.New(context.Background(), p, inv, limit, false)
	if err != nil {
		t.Fatal(err)
	}
	return c, r
}

// rollingLoop takes each app server out of lb, updates it, and puts it back,
// with the loop's rollout settings in loop.
func rollingLoop(loop config.FleetStage) config.FleetStage {
	loop.ID, loop.ForEachNode, loop.As = "roll", "app", "app"
	loop.FleetStages = []config.FleetStage{
		{ID: "drain", Nodes: "lb"},
		{ID: "update", Nodes: "{{ .app }}", Outputs: map[string]any{"name": "{{ .app }}"}},
		{ID: "rejoin", Nodes: "lb"},
	}
	loop.Outputs = map[string]any{"name": "{{ .outputs.update[.app].name }}"}
	return loop
}

// recordCalls runs node jobs locally and records each fleet stage and node in
// order; fail returns a response for a job that should not run.
func recordCalls(c *Controller, fail func(fleetStage, node string) *protocol.Response) func() []string {
	var mu sync.Mutex
	var calls []string
	localNodes(c, func(s config.FleetStage, en Entry) *protocol.Response {
		mu.Lock()
		calls = append(calls, s.ID+"/"+en.Node)
		mu.Unlock()
		if fail != nil {
			return fail(s.ID, en.Node)
		}
		return nil
	})
	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), calls...)
	}
}

// Each loop node runs its fleet stages in order, one node after the other by
// default; outputs are keyed by the loop's node, and a new run repeats the work.
func TestForEachNodeRunsEachNodesFleetStagesInOrder(t *testing.T) {
	for attempt := 0; attempt < 2; attempt++ {
		c, r := loopFleet(t, nil, rollingLoop(config.FleetStage{}),
			config.FleetStage{ID: "report", Nodes: "lb", NodeTasks: []config.Task{{Show: "{{ .outputs.roll.b.name }}"}}})
		calls := recordCalls(&c, nil)
		execute(t, c, r, false)
		want := []string{"roll[a]/drain/lb", "roll[a]/update/a", "roll[a]/rejoin/lb", "roll[b]/drain/lb", "roll[b]/update/b", "roll[b]/rejoin/lb", "roll[c]/drain/lb", "roll[c]/update/c", "roll[c]/rejoin/lb", "report/lb"}
		if r.Status != "succeeded" || !reflect.DeepEqual(calls(), want) {
			t.Fatal(attempt, r.Status, calls())
		}
		if got := entry(t, r, "report", "lb").Response.Results[0].Value; got != "b" {
			t.Fatal(got)
		}
		loaded, err := c.Load(r.ID, false)
		if err != nil || hasUnfinished(loaded) {
			t.Fatal(loaded, err)
		}
		if sel := loaded.Selections["roll"]; !reflect.DeepEqual(sel.Nodes, []string{"a", "b", "c"}) || sel.Iterations["c"].Outputs["name"] != "c" {
			t.Fatalf("%+v", sel)
		}
	}
}

// A limit picks the loop's iterations; a kept iteration still works on lb,
// which is outside the limit, and reading a left-out loop node is blocked.
func TestLimitPicksForEachNodeIterationsAndReachesHelperNodes(t *testing.T) {
	c, r := loopFleet(t, []string{"b"}, rollingLoop(config.FleetStage{}),
		config.FleetStage{ID: "report", Nodes: "b", NodeTasks: []config.Task{{Show: "{{ .outputs.roll.a.name }}"}}})
	calls := recordCalls(&c, nil)
	execute(t, c, r, false)
	// The node job of report finds the left-out node's output unavailable.
	if want := []string{"roll[b]/drain/lb", "roll[b]/update/b", "roll[b]/rejoin/lb", "report/b"}; !reflect.DeepEqual(calls(), want) {
		t.Fatal(calls())
	}
	if sel := r.Selections["roll"]; !reflect.DeepEqual(sel.Nodes, []string{"b"}) || !reflect.DeepEqual(sel.LeftOut, []string{"a", "c"}) {
		t.Fatalf("%+v", sel)
	}
	if report := entry(t, r, "report", "b").Response; report.Status != "blocked" || !strings.Contains(report.Error, "roll on a is left out by the limit b") {
		t.Fatal(report)
	}
}

// A failure ends that loop node's sequence: lb does not rejoin the failed
// server. stop_run on the loop keeps the next loop node and later fleet stages
// from starting; resume repeats the failed loop node from its first unfinished
// fleet stage and then continues.
func TestForEachNodeFailureEndsTheSequenceAndResumeContinues(t *testing.T) {
	c, r := loopFleet(t, nil, rollingLoop(config.FleetStage{OnNodeFailure: "stop_run"}), config.FleetStage{ID: "report", Nodes: "lb"})
	fail := true
	calls := recordCalls(&c, func(fleetStage, _ string) *protocol.Response {
		if fail && fleetStage == "roll[b]/update" {
			return &protocol.Response{Status: "failed", Error: "unhealthy"}
		}
		return nil
	})
	execute(t, c, r, false)
	if want := []string{"roll[a]/drain/lb", "roll[a]/update/a", "roll[a]/rejoin/lb", "roll[b]/drain/lb", "roll[b]/update/b"}; r.Status != "failed" || !reflect.DeepEqual(calls(), want) {
		t.Fatal(r.Status, calls())
	}
	loaded, err := c.Load(r.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if rejoin := entry(t, loaded, "roll[b]/rejoin", "lb"); rejoin.NodeJobAttempt != 0 || rejoin.Response.Status != "stopped" || rejoin.Response.Error != "not started: roll[b]/update on b is failed: unhealthy" {
		t.Fatalf("%+v", rejoin.Response)
	}
	cause := "not started: on_node_failure stop_run after roll failed on b"
	if got := loaded.Selections["roll"].Iterations["c"]; got.Status != "stopped" || got.Error != cause {
		t.Fatalf("%+v", got)
	}
	if got := loaded.Selections["roll"].Iterations["b"]; got.Status != "failed" {
		t.Fatalf("%+v", got)
	}
	if report := entry(t, loaded, "report", "lb").Response; report.Status != "stopped" || report.Error != cause {
		t.Fatalf("%+v", report)
	}
	fail = false
	execute(t, c, loaded, true)
	want := []string{"roll[b]/update/b", "roll[b]/rejoin/lb", "roll[c]/drain/lb", "roll[c]/update/c", "roll[c]/rejoin/lb", "report/lb"}
	if got := calls()[5:]; loaded.Status != "succeeded" || !reflect.DeepEqual(got, want) {
		t.Fatal(loaded.Status, got)
	}
}

// A stop request ends the loop at the next fleet stage boundary; resume
// finishes the stopped loop node and the ones after it.
func TestStopRequestDuringForEachNodeKeepsTheRestResumable(t *testing.T) {
	c, r := loopFleet(t, nil, rollingLoop(config.FleetStage{}))
	calls := recordCalls(&c, func(fleetStage, _ string) *protocol.Response {
		if fleetStage == "roll[a]/drain" {
			if err := c.requestStop(r.ID); err != nil {
				t.Error(err)
			}
		}
		return nil
	})
	execute(t, c, r, false)
	if r.Status != "stopped" || !reflect.DeepEqual(calls(), []string{"roll[a]/drain/lb"}) {
		t.Fatal(r.Status, calls())
	}
	if got := r.Selections["roll"].Iterations["b"]; got.Status != "stopped" || got.Error != "not started: stop requested" {
		t.Fatalf("%+v", got)
	}
	loaded, err := c.Load(r.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	execute(t, c, loaded, true)
	if got := calls(); loaded.Status != "succeeded" || len(got) != 9 || got[1] != "roll[a]/update/a" {
		t.Fatal(loaded.Status, got)
	}
}

// With max_nodes_at_once 2, two loop nodes are in their sequence at once. lb
// works for one loop node at a time. When one loop node fails, the other one
// already in its sequence finishes it whole, and stop_run keeps the third from
// starting.
func TestConcurrentForEachNodeIterationsShareHelperNodesOneAtATime(t *testing.T) {
	c, r := loopFleet(t, nil, rollingLoop(config.FleetStage{MaxNodesAtOnce: "2", OnNodeFailure: "stop_run"}))
	var mu sync.Mutex
	onHelper, mostOnHelper := 0, 0
	aFailed := make(chan struct{})
	calls := recordCalls(&c, func(fleetStage, node string) *protocol.Response {
		if node == "lb" {
			mu.Lock()
			onHelper++
			mostOnHelper = max(mostOnHelper, onHelper)
			mu.Unlock()
			time.Sleep(20 * time.Millisecond)
			mu.Lock()
			onHelper--
			mu.Unlock()
		}
		switch fleetStage {
		case "roll[a]/update":
			close(aFailed)
			return &protocol.Response{Status: "failed", Error: "unhealthy"}
		case "roll[b]/update":
			// b is still in its sequence when the loop records a's failure.
			<-aFailed
			waitForIteration(t, c, r, "roll", "a", "failed")
		}
		return nil
	})
	execute(t, c, r, false)
	got := map[string]bool{}
	for _, call := range calls() {
		got[call] = true
	}
	if r.Status != "failed" || !got["roll[b]/rejoin/lb"] || got["roll[a]/rejoin/lb"] || got["roll[c]/drain/lb"] || !got["roll[b]/drain/lb"] {
		t.Fatal(r.Status, calls())
	}
	if mostOnHelper != 1 {
		t.Fatalf("lb worked for %d loop nodes at once", mostOnHelper)
	}
	if iteration := r.Selections["roll"].Iterations["c"]; iteration.Status != "stopped" || !strings.Contains(iteration.Error, "on_node_failure stop_run after roll failed on a") {
		t.Fatalf("%+v", iteration)
	}
	if iteration := r.Selections["roll"].Iterations["b"]; iteration.Status != "succeeded" {
		t.Fatalf("%+v", iteration)
	}
}

// A percentage counts the loop's nodes, and with wait_for_all_in_batch the
// next loop node starts only after the whole batch finished.
func TestForEachNodePercentageAndBatches(t *testing.T) {
	for _, batch := range []bool{false, true} {
		t.Run(map[bool]string{false: "streaming", true: "batch"}[batch], func(t *testing.T) {
			c, r := loopFleet(t, nil, config.FleetStage{ID: "roll", ForEachNode: "app", As: "app", MaxNodesAtOnce: "67%", WaitForAllInBatch: batch,
				FleetStages: []config.FleetStage{{ID: "update", Nodes: "{{ .app }}"}}})
			// Every node job waits until the test releases its node, so
			// admission alone decides which jobs have started.
			started := make(chan string, 3)
			release := map[string]chan struct{}{"a": make(chan struct{}), "b": make(chan struct{}), "c": make(chan struct{})}
			c.executeNode = func(_ context.Context, _ *Run, _ config.FleetStage, en Entry, _ bool, _ func(Entry) error) (Entry, error) {
				started <- en.Node
				<-release[en.Node]
				en.Response.Status = "succeeded"
				return en, nil
			}
			done := make(chan error, 1)
			go func() { done <- c.Execute(context.Background(), r, false) }()
			// 67% of three loop nodes is two: a and b start, in either order.
			if first, second := receiveSoon(t, started), receiveSoon(t, started); first == second || first == "c" || second == "c" {
				t.Fatal(first, second)
			}
			close(release["b"])
			if batch {
				// b's slot is free, but c belongs to the next batch.
				waitForIteration(t, c, r, "roll", "b", "succeeded")
				select {
				case node := <-started:
					t.Fatalf("started %s before the batch finished", node)
				default:
				}
				close(release["a"])
				if receiveSoon(t, started) != "c" {
					t.Fatal("missing the next batch")
				}
			} else {
				// c takes b's slot while a still holds its own.
				if receiveSoon(t, started) != "c" {
					t.Fatal("did not fill the freed slot")
				}
				close(release["a"])
			}
			close(release["c"])
			if err := <-done; err != nil || r.Status != "succeeded" {
				t.Fatal(err, r.Status)
			}
		})
	}
}

// receiveSoon is receive with room for a loaded test machine.
func receiveSoon(t *testing.T, c <-chan string) string {
	t.Helper()
	select {
	case s := <-c:
		return s
	case <-time.After(time.Minute):
		t.Fatal("scheduler did not start a node")
		return ""
	}
}

// waitForIteration waits until the run's saved selection of loop records key's
// iteration with status: the scheduler saves it when the iteration has ended.
func waitForIteration(t *testing.T, c Controller, r *Run, loop, key, status string) {
	t.Helper()
	path := filepath.Join(c.runDir(r.ID), "selections", fmt.Sprintf("%x.json", sha256.Sum256([]byte(loop))))
	for deadline := time.Now().Add(time.Minute); time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		var sel Selection
		if b, err := os.ReadFile(path); err == nil && protocol.Decode(b, &sel) == nil && sel.Iterations[key].Status == status {
			return
		}
	}
	t.Fatalf("iteration %s of %s never became %s", key, loop, status)
}

// The controller process checks a when on a fleet stage or a for_each_node
// loop for each node just before it would start: a skipped node starts no
// node job and records the condition. Resume keeps a when found true.
func TestWhenIsCheckedForEachNodeAsItStarts(t *testing.T) {
	notLeader := map[string]any{"not": "{{ .outputs.find[.node.name].leader }}"}
	c, r := loopFleet(t, nil,
		config.FleetStage{ID: "find", Nodes: "app", Outputs: map[string]any{"leader": map[string]any{"equals": []any{"{{ .node.name }}", "b"}}}},
		config.FleetStage{ID: "followers", Nodes: "app", When: notLeader, MaxNodesAtOnce: "1"},
		config.FleetStage{ID: "roll", ForEachNode: "app", As: "db", When: map[string]any{"not": "{{ .outputs.find[.db].leader }}"},
			FleetStages: []config.FleetStage{{ID: "update", Nodes: "{{ .db }}"}}},
	)
	fail := true
	calls := recordCalls(&c, func(fleetStage, node string) *protocol.Response {
		if fail && fleetStage == "followers" && node == "c" {
			return &protocol.Response{Status: "failed", Error: "broken"}
		}
		return nil
	})
	execute(t, c, r, false)
	// c failed in followers, so its loop node does not start either.
	want := []string{"find/a", "find/b", "find/c", "followers/a", "followers/c", "roll[a]/update/a"}
	got := calls()
	if len(got) != len(want) {
		t.Fatal(got)
	}
	for _, call := range want {
		if !strings.Contains(strings.Join(got, " "), call) {
			t.Fatal(call, got)
		}
	}
	if skipped := entry(t, r, "followers", "b").Response; skipped.Status != "skipped" || skipped.Error != `when is false: {"not":"{{ .outputs.find[.node.name].leader }}"}` || len(skipped.Results) != 0 {
		t.Fatalf("%+v", skipped)
	}
	if skipped := r.Selections["roll"].Iterations["b"]; skipped.Status != "skipped" || !strings.HasPrefix(skipped.Error, "when is false: ") {
		t.Fatalf("%+v", skipped)
	}
	loaded, err := c.Load(r.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if !entry(t, loaded, "followers", "c").WhenWasTrue {
		t.Fatal("the when that let c start was not recorded")
	}
	// Even a condition that would now be false keeps its recorded result.
	loaded.Project.FleetStages[1].When = false
	fail = false
	execute(t, c, loaded, true)
	if resumed := calls()[len(got):]; loaded.Status != "succeeded" || !reflect.DeepEqual(resumed, []string{"followers/c", "roll[c]/update/c"}) {
		t.Fatal(loaded.Status, calls())
	}
}

// A dryrun runs the same loop and changes nothing: a shell task without a
// dryrun command is not run.
func TestForEachNodeDryrunChangesNothing(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "drained")
	loop := rollingLoop(config.FleetStage{})
	loop.FleetStages[0].NodeTasks = []config.Task{{Module: "shell", ModuleInputs: &modules.Shell{Script: "touch " + quote(marker)}}}
	c, r := loopFleet(t, nil, loop)
	r.Dryrun = true
	if err := c.createRun(r); err != nil {
		t.Fatal(err)
	}
	calls := recordCalls(&c, nil)
	execute(t, c, r, false)
	if r.Status != "succeeded" || len(calls()) != 9 {
		t.Fatal(r.Status, calls())
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("dryrun drained", err)
	}
}

// except_nodes on a for_each_node loop leaves those nodes without an
// iteration; --only-fleet-stages that leaves the loop out gives it no
// selection; save_on_controller_machine on an enclosed fleet stage writes the
// file of each iteration's node.
func TestForEachNodeTakesExceptNodesOnlyFleetStagesAndSavedFiles(t *testing.T) {
	root := t.TempDir()
	loop := rollingLoop(config.FleetStage{ExceptNodes: []any{"b"}})
	loop.FleetStages[1].SaveOnControllerMachine = []config.SaveOnControllerMachine{{Output: "name", Path: "updated/{{ .node.name }}.txt", PermissionsMode: "0644", ProjectRoot: root}}
	c, r := loopFleet(t, nil, loop)
	calls := recordCalls(&c, nil)
	execute(t, c, r, false)
	if want := []string{"roll[a]/drain/lb", "roll[a]/update/a", "roll[a]/rejoin/lb", "roll[c]/drain/lb", "roll[c]/update/c", "roll[c]/rejoin/lb"}; r.Status != "succeeded" || !reflect.DeepEqual(calls(), want) {
		t.Fatal(r.Status, calls())
	}
	for _, node := range []string{"a", "c"} {
		if text, _ := read(t, filepath.Join(root, "updated", node+".txt")); text != node {
			t.Fatal(node, text)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "updated", "b.txt")); !os.IsNotExist(err) {
		t.Fatal("b has no iteration", err)
	}

	c, r = loopFleet(t, nil, rollingLoop(config.FleetStage{}), config.FleetStage{ID: "report", Nodes: "lb"})
	r.Project.OnlyFleetStages = []string{"report"}
	if err := c.createRun(r); err != nil {
		t.Fatal(err)
	}
	calls = recordCalls(&c, nil)
	execute(t, c, r, false)
	if r.Status != "succeeded" || !reflect.DeepEqual(calls(), []string{"report/lb"}) || r.Selections["roll"] != nil {
		t.Fatal(r.Status, calls(), r.Selections["roll"])
	}
}

// A helper node's failure for one loop node ends only that loop node's
// sequence: the other loop node already in its sequence still rejoins lb, and
// under continue the next loop node runs on lb too. The failure counts for the
// loop node it worked for. After the loop, lb starts none of its later fleet
// stages.
func TestHelperFailureEndsOnlyItsLoopNodesSequence(t *testing.T) {
	for _, policy := range []string{"continue", "stop_fleet_stage"} {
		t.Run(policy, func(t *testing.T) {
			c, r := loopFleet(t, nil, rollingLoop(config.FleetStage{MaxNodesAtOnce: "2", OnNodeFailure: policy}),
				config.FleetStage{ID: "report", Nodes: []any{"a", "lb"}})
			bDrained := make(chan struct{})
			calls := recordCalls(&c, func(fleetStage, _ string) *protocol.Response {
				switch fleetStage {
				case "roll[b]/drain":
					defer close(bDrained)
					return &protocol.Response{Status: "failed", Error: "lb refused"}
				case "roll[a]/rejoin":
					// a is still in its sequence when the loop records b's failure.
					<-bDrained
					waitForIteration(t, c, r, "roll", "b", "failed")
				}
				return nil
			})
			execute(t, c, r, false)
			got := map[string]bool{}
			for _, call := range calls() {
				got[call] = true
			}
			if !got["roll[a]/rejoin/lb"] || got["roll[b]/update/b"] || got["roll[b]/rejoin/lb"] || got["report/lb"] || !got["report/a"] {
				t.Fatal(calls())
			}
			if got["roll[c]/drain/lb"] != (policy == "continue") {
				t.Fatal("the next loop node", calls())
			}
			iterations := r.Selections["roll"].Iterations
			if iterations["a"].Status != "succeeded" || iterations["b"].Status != "failed" {
				t.Fatalf("%+v", iterations)
			}
			if policy == "stop_fleet_stage" && (iterations["c"].Status != "stopped" || iterations["c"].Error != "not started: on_node_failure stop_fleet_stage after roll failed on b") {
				t.Fatalf("%+v", iterations["c"])
			}
			if report := entry(t, r, "report", "lb").Response; report.Status != "stopped" || !strings.Contains(report.Error, "roll[b]/drain on lb is failed") {
				t.Fatal(report)
			}
		})
	}
}

// Under a limit, a kept iteration reaches the helper nodes the run showed
// before connecting. A fleet stage whose nodes resolve during the run, from an
// earlier output, cannot reach a node outside the limit that was not shown:
// that work is blocked, naming the limit, and the iteration ends there.
func TestLimitBlocksHelperNodesSelectedDuringTheRun(t *testing.T) {
	loop := rollingLoop(config.FleetStage{})
	loop.FleetStages = append([]config.FleetStage{{ID: "inspect", Nodes: "{{ .app }}", Outputs: map[string]any{"balancer": "lb"}}}, loop.FleetStages...)
	loop.FleetStages[1].Nodes = "{{ .outputs.inspect[.app].balancer }}" // drain: resolved during the run
	c, r := loopFleet(t, []string{"b"}, loop)
	if want := []config.HelperNode{{Node: "lb", FleetStages: []string{"roll[b]/rejoin"}}}; !reflect.DeepEqual(r.HelperNodes, want) {
		t.Fatal(r.HelperNodes)
	}
	calls := recordCalls(&c, nil)
	execute(t, c, r, false)
	if !reflect.DeepEqual(calls(), []string{"roll[b]/inspect/b"}) {
		t.Fatal(calls())
	}
	drain := entry(t, r, "roll[b]/drain", "lb").Response
	if drain.Status != "blocked" || !strings.Contains(drain.Error, "the limit b leaves lb out") || !strings.Contains(drain.Error, "Name lb in the fleet stage's nodes in the playbook, or widen the limit") {
		t.Fatal(drain)
	}
	if rejoin := entry(t, r, "roll[b]/rejoin", "lb").Response; rejoin.Status != "stopped" {
		t.Fatal(rejoin)
	}
	// Without a limit nothing is narrowed.
	c, r = loopFleet(t, nil, loop)
	calls = recordCalls(&c, nil)
	execute(t, c, r, false)
	if r.Status != "succeeded" || len(calls()) != 12 {
		t.Fatal(r.Status, calls())
	}
}
