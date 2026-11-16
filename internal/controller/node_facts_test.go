package controller

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"mmx/internal/config"
	"mmx/internal/facts"
	"mmx/internal/protocol"
)

// distribution is the fleet's facts: a and c run Debian, b runs Rocky Linux.
var distribution = map[string]string{"a": "debian", "b": "rocky", "c": "debian", "lb": "debian"}

// onDebian is a when that reads the node's facts.
var onDebian = map[string]any{"equals": []any{"{{ .node.distribution.id }}", "debian"}}

// answerFacts answers facts requests from distribution and counts them; hold,
// when set, runs before a node answers.
func answerFacts(c *Controller, hold func(ctx context.Context, node string) error) func() map[string]int {
	var mu sync.Mutex
	asked := map[string]int{}
	c.nodeFacts = func(ctx context.Context, _ *Run, en Entry, _ bool) (facts.Snapshot, error) {
		mu.Lock()
		asked[en.Node]++
		mu.Unlock()
		if hold != nil {
			if err := hold(ctx, en.Node); err != nil {
				return facts.Snapshot{}, err
			}
		}
		return facts.Snapshot{Values: map[string]any{"name": en.Node, "distribution": map[string]any{"id": distribution[en.Node]}}}, nil
	}
	return func() map[string]int {
		mu.Lock()
		defer mu.Unlock()
		out := map[string]int{}
		for node, n := range asked {
			out[node] = n
		}
		return out
	}
}

// A when on a fleet stage or a for_each_node loop that reads node facts asks
// each node for its facts just before deciding: b is skipped, with the facts
// it was decided on, and starts no node job. A when without facts asks no
// node. Dryrun decides the same way.
func TestWhenReadingFactsAsksEachNodeFirst(t *testing.T) {
	for _, dryrun := range []bool{false, true} {
		c, r := loopFleet(t, nil,
			config.FleetStage{ID: "debian_only", Nodes: "app", When: onDebian},
			config.FleetStage{ID: "by_name", Nodes: "app", When: map[string]any{"not": map[string]any{"equals": []any{"{{ .node.name }}", "c"}}}},
			config.FleetStage{ID: "roll", ForEachNode: "app", As: "app", When: onDebian, FleetStages: []config.FleetStage{{ID: "update", Nodes: "{{ .app }}"}}},
		)
		r.Dryrun = dryrun
		if err := c.createRun(r); err != nil {
			t.Fatal(err)
		}
		asked := answerFacts(&c, nil)
		calls := recordCalls(&c, nil)
		execute(t, c, r, false)
		// Nodes of one fleet stage run at once, so compare without order.
		want := []string{"by_name/a", "by_name/b", "debian_only/a", "debian_only/c", "roll[a]/update/a", "roll[c]/update/c"}
		ran := calls()
		sort.Strings(ran)
		if r.Status != "succeeded" || !reflect.DeepEqual(ran, want) {
			t.Fatal(dryrun, r.Status, calls())
		}
		// One request for each node and when that reads facts; none for by_name.
		if got := asked(); !reflect.DeepEqual(got, map[string]int{"a": 2, "b": 2, "c": 2}) {
			t.Fatal(got)
		}
		skipped := entry(t, r, "debian_only", "b").Response
		if skipped.Status != "skipped" || !strings.HasPrefix(skipped.Error, "when is false: ") || skipped.Facts == nil || skipped.Facts.Values["distribution"].(map[string]any)["id"] != "rocky" {
			t.Fatalf("%+v", skipped)
		}
		if loop := r.Selections["roll"].Iterations["b"]; loop.Status != "skipped" || loop.Facts == nil {
			t.Fatalf("%+v", loop)
		}
	}
}

// A node that cannot answer the facts request fails as its node job would
// have: the failure policy applies, and the node starts none of its later
// fleet stages.
func TestUnreachableNodeFailsItsFactsReadingWhen(t *testing.T) {
	c, r := loopFleet(t, nil,
		config.FleetStage{ID: "debian_only", Nodes: "app", When: onDebian, MaxNodesAtOnce: "1", OnNodeFailure: "stop_fleet_stage"},
		config.FleetStage{ID: "later", Nodes: "app"},
	)
	answerFacts(&c, func(_ context.Context, node string) error {
		if node == "a" {
			return errors.New("connection refused")
		}
		return nil
	})
	calls := recordCalls(&c, nil)
	execute(t, c, r, false)
	a := entry(t, r, "debian_only", "a").Response
	if r.Status != "failed" || a.Status != "failed" || a.Error != "reading the node's facts for the fleet stage's when: connection refused" {
		t.Fatal(r.Status, a)
	}
	if c := entry(t, r, "debian_only", "c").Response; c.Status != "stopped" || !strings.Contains(c.Error, "on_node_failure stop_fleet_stage after debian_only failed on a") {
		t.Fatalf("%+v", c)
	}
	ran := calls()
	sort.Strings(ran)
	if later := entry(t, r, "later", "a").Response; later.Status != "stopped" || !reflect.DeepEqual(ran, []string{"later/b", "later/c"}) {
		t.Fatal(later, calls())
	}
}

// A for_each_node loop checks a loop node's when when that node's turn
// comes, once a slot is free, as a plain fleet stage does; only its facts
// are asked for ahead. With one loop node at a time, a's failure stops the
// loop before b's turn comes, so b is recorded stopped, not skipped by a
// when checked too early, and a resume checks b's when.
func TestForEachNodeChecksItsWhenOnceASlotIsFree(t *testing.T) {
	c, r := loopFleet(t, nil, config.FleetStage{ID: "roll", ForEachNode: []any{"a", "b"}, As: "app", When: onDebian, OnNodeFailure: "stop_fleet_stage",
		FleetStages: []config.FleetStage{{ID: "update", Nodes: "{{ .app }}"}}})
	asked := answerFacts(&c, nil)
	fail := true
	calls := recordCalls(&c, func(_, node string) *protocol.Response {
		if fail && node == "a" {
			return &protocol.Response{Status: "failed", Error: "broken"}
		}
		return nil
	})
	execute(t, c, r, false)
	if a := r.Selections["roll"].Iterations["a"]; a.Status != "failed" {
		t.Fatalf("%+v", a)
	}
	if b := r.Selections["roll"].Iterations["b"]; b.Status != "stopped" || b.Error != "not started: on_node_failure stop_fleet_stage after roll failed on a" {
		t.Fatalf("b's when was checked before its turn came: %+v", b)
	}
	if !reflect.DeepEqual(calls(), []string{"roll[a]/update/a"}) {
		t.Fatal(calls())
	}
	loaded, err := c.Load(r.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	fail = false
	execute(t, c, loaded, true)
	if b := loaded.Selections["roll"].Iterations["b"]; loaded.Status != "succeeded" || b.Status != "skipped" || b.Facts == nil || asked()["b"] < 1 {
		t.Fatalf("%s %+v %v", loaded.Status, b, asked())
	}
}

// The facts requests run without the scheduler lock: they overlap, and a
// request that hangs keeps no other for_each_node iteration from going on.
// A stop request interrupts a hanging request, and the node does not start.
func TestFactsRequestsOverlapAndAStopInterruptsThem(t *testing.T) {
	// All three requests wait for each other: they must be in flight at once.
	c, r := loopFleet(t, nil, config.FleetStage{ID: "debian_only", Nodes: "app", When: onDebian, MaxNodesAtOnce: "3"})
	var started sync.WaitGroup
	started.Add(3)
	answerFacts(&c, func(context.Context, string) error {
		started.Done()
		started.Wait()
		return nil
	})
	recordCalls(&c, nil)
	finished := make(chan error, 1)
	go func() { finished <- c.Execute(context.Background(), r, false) }()
	select {
	case err := <-finished:
		if err != nil || r.Status != "succeeded" {
			t.Fatal(err, r.Status)
		}
	case <-time.After(time.Minute):
		t.Fatal("facts requests waited for each other")
	}

	// a's facts request hangs until b's iteration ran its node job.
	c, r = loopFleet(t, nil, config.FleetStage{ID: "roll", ForEachNode: []any{"a", "b"}, As: "app", MaxNodesAtOnce: "2",
		FleetStages: []config.FleetStage{{ID: "check", Nodes: "{{ .app }}", When: onDebian}, {ID: "update", Nodes: "{{ .app }}"}}})
	distribution["b"] = "debian"
	defer func() { distribution["b"] = "rocky" }()
	bUpdated := make(chan struct{})
	answerFacts(&c, func(_ context.Context, node string) error {
		if node == "a" {
			<-bUpdated
		}
		return nil
	})
	calls := recordCalls(&c, func(fleetStage, _ string) *protocol.Response {
		if fleetStage == "roll[b]/update" {
			close(bUpdated)
		}
		return nil
	})
	execute(t, c, r, false)
	if r.Status != "succeeded" || len(calls()) != 4 {
		t.Fatal(r.Status, calls())
	}

	// A stop request interrupts a facts request that does not answer.
	c, r = loopFleet(t, nil, config.FleetStage{ID: "debian_only", Nodes: "app", When: onDebian})
	asking := make(chan struct{}, 3)
	interrupted := make(chan error, 3)
	answerFacts(&c, func(ctx context.Context, _ string) error {
		asking <- struct{}{}
		<-ctx.Done()
		interrupted <- ctx.Err()
		return ctx.Err()
	})
	calls = recordCalls(&c, nil)
	go func() {
		<-asking
		if err := c.requestStop(r.ID); err != nil {
			t.Error(err)
		}
	}()
	execute(t, c, r, false)
	if err := <-interrupted; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if a := entry(t, r, "debian_only", "a").Response; r.Status != "stopped" || a.Status != "stopped" || a.Error != "not started: stop requested" || len(calls()) != 0 {
		t.Fatal(r.Status, a, calls())
	}
}

// Resume keeps a when that already let a node start: the node is not asked
// for its facts again, even when they would now decide otherwise.
func TestResumeKeepsAWhenThatReadFacts(t *testing.T) {
	c, r := loopFleet(t, nil, config.FleetStage{ID: "debian_only", Nodes: "app", When: onDebian})
	asked := answerFacts(&c, nil)
	fail := true
	calls := recordCalls(&c, func(fleetStage, node string) *protocol.Response {
		if fail && node == "a" {
			return &protocol.Response{Status: "failed", Error: "broken"}
		}
		return nil
	})
	execute(t, c, r, false)
	loaded, err := c.Load(r.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	distribution["a"] = "rocky"
	defer func() { distribution["a"] = "debian" }()
	fail = false
	execute(t, c, loaded, true)
	if loaded.Status != "succeeded" || calls()[len(calls())-1] != "debian_only/a" || asked()["a"] != 1 {
		t.Fatal(loaded.Status, calls(), asked())
	}
}
