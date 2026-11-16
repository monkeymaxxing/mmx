package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"mmx/internal/config"
	"mmx/internal/engine"
	"mmx/internal/modules"
	"mmx/internal/protocol"
)

// Run real node node tasks without the transport so scheduler tests can inspect
// observable effects and the same encoded job/output types used by the runner.
func localNodes(c *Controller, before func(config.FleetStage, Entry) *protocol.Response) {
	c.executeNode = func(ctx context.Context, r *Run, s config.FleetStage, en Entry, _ bool, _ func(Entry) error) (Entry, error) {
		if before != nil {
			if response := before(s, en); response != nil {
				en.Response = *response
				return en, nil
			}
		}
		raw, err := json.Marshal(mustJob(nodeJob(r, s, en)))
		if err != nil {
			return en, err
		}
		var j config.Job
		if err := protocol.Decode(raw, &j); err != nil {
			return en, err
		}
		en.Response = (&engine.Engine{}).Run(ctx, j)
		return en, nil
	}
}

func execute(t *testing.T, c Controller, r *Run, retry bool) {
	t.Helper()
	if err := c.Execute(context.Background(), r, retry); err != nil {
		t.Fatal(err)
	}
}
func entry(t *testing.T, r *Run, fleetStage, managedNode string) Entry {
	t.Helper()
	for _, en := range r.Entries {
		if en.FleetStage == fleetStage && en.Node == managedNode {
			return en
		}
	}
	t.Fatalf("missing %s/%s", fleetStage, managedNode)
	return Entry{}
}

func TestOutputsAvailabilityAndRetry(t *testing.T) {
	c, r := fleet(t,
		config.FleetStage{ID: "inspect", Nodes: []any{"a", "b"}, Outputs: map[string]any{"port": 8080}},
		config.FleetStage{ID: "one", Nodes: "c", NodeTasks: []config.Task{{Show: "{{ .outputs.inspect.a.port }}"}}},
		config.FleetStage{ID: "all", Nodes: "c", NodeTasks: []config.Task{{Show: "{{ .outputs.inspect }}"}}},
		config.FleetStage{ID: "outcome", Nodes: "c", NodeTasks: []config.Task{{Show: "{{ .fleet_stages.inspect.b.status }}"}}},
		config.FleetStage{ID: "disabled", Nodes: "c", When: false, NodeTasks: []config.Task{{Show: "{{ .outputs.inspect.b.port }}"}}},
	)
	fail := true
	counts := map[string]int{}
	var mu sync.Mutex
	localNodes(&c, func(s config.FleetStage, en Entry) *protocol.Response {
		mu.Lock()
		counts[s.ID+"/"+en.Node]++
		mu.Unlock()
		if s.ID == "inspect" && en.Node == "b" && fail {
			return &protocol.Response{Status: "failed", Error: "inspection failed"}
		}
		return nil
	})
	execute(t, c, r, false)
	if r.Status != "failed" || entry(t, r, "one", "c").Response.Status != "succeeded" || entry(t, r, "all", "c").Response.Status != "blocked" || entry(t, r, "disabled", "c").Response.Status != "skipped" {
		t.Fatalf("%+v", r.Entries)
	}
	if got := entry(t, r, "outcome", "c").Response.Results[0].Value; got != "failed" {
		t.Fatal(got)
	}
	var err error
	r, err = c.Load(r.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	fail = false
	execute(t, c, r, true)
	// The controller process skipped the disabled fleet stage: no node job ran.
	if r.Status != "succeeded" || counts["inspect/a"] != 1 || counts["inspect/b"] != 2 || counts["all/c"] != 2 || counts["disabled/c"] != 0 || counts["outcome/c"] != 1 {
		t.Fatal(r.Status, counts)
	}
	if disabled := entry(t, r, "disabled", "c").Response; disabled.Error != "when is false: false" {
		t.Fatal(disabled)
	}
	if got := entry(t, r, "all", "c").Response.Results[0].Value.(map[string]any); len(got) != 2 {
		t.Fatal(got)
	}
}

// A node that a fleet stage's when skipped is left out of the fleet stage's
// whole output collection, as a loop iteration that its when skipped is left
// out of the loop's. A read of the skipped node's own entry is blocked and
// names the condition, and .fleet_stages still lists the node as skipped.
func TestWholeOutputCollectionLeavesOutWhatAWhenSkipped(t *testing.T) {
	notB := map[string]any{"not": map[string]any{"equals": []any{"{{ .node.name }}", "b"}}}
	c, r := fleet(t,
		config.FleetStage{ID: "inspect", Nodes: "all", When: notB, Outputs: map[string]any{"name": "{{ .node.name }}"}},
		config.FleetStage{ID: "all", Nodes: "c", NodeTasks: []config.Task{{Show: "{{ .outputs.inspect }}"}, {Show: "{{ .fleet_stages.inspect.b.status }}"}}},
		config.FleetStage{ID: "one", Nodes: "c", NodeTasks: []config.Task{{Show: "{{ .outputs.inspect.b.name }}"}}},
		config.FleetStage{ID: "each", Loop: []any{"x", "y"}, As: "item", Key: "{{ .item }}",
			When:        map[string]any{"equals": []any{"{{ .item }}", "x"}},
			FleetStages: []config.FleetStage{{ID: "report", Nodes: "a"}}, Outputs: map[string]any{"item": "{{ .item }}"}},
		config.FleetStage{ID: "iterations", Nodes: "c", NodeTasks: []config.Task{{Show: "{{ .outputs.each }}"}}},
	)
	localNodes(&c, nil)
	execute(t, c, r, false)
	if all := entry(t, r, "all", "c").Response; all.Status != "succeeded" || len(all.Results) != 2 ||
		fmt.Sprint(all.Results[0].Value) != "map[a:map[name:a] c:map[name:c]]" || all.Results[1].Value != "skipped" {
		t.Fatalf("%+v", all)
	}
	if one := entry(t, r, "one", "c").Response; one.Status != "blocked" ||
		!strings.Contains(one.Error, `inspect on b is skipped: when is false: {"not":{"equals":["{{ .node.name }}","b"]}}`) {
		t.Fatalf("%+v", one)
	}
	if iterations := entry(t, r, "iterations", "c").Response; iterations.Status != "succeeded" || fmt.Sprint(iterations.Results[0].Value) != "map[x:map[item:x]]" {
		t.Fatalf("%+v", iterations)
	}
}

// A fleet stage whose nodes resolve during the run gets its entries after
// those of later fleet stages whose nodes were known at the start, and one
// that resolves only on resume gets them after a fleet stage later in the
// playbook. Each fleet stage still reads its own nodes' entries, during the
// run and after the run is saved and loaded again.
func TestSelectionsResolvedDuringTheRunKeepTheirOwnEntries(t *testing.T) {
	c, r := fleet(t,
		config.FleetStage{ID: "gate", Nodes: "a", Outputs: map[string]any{"target": "b"}},
		config.FleetStage{ID: "source", Nodes: "c", Outputs: map[string]any{"target": "c"}},
		config.FleetStage{ID: "on_resume", Nodes: "{{ .outputs.gate.a.target }}", Outputs: map[string]any{"name": "selected on resume"}},
		config.FleetStage{ID: "during_run", Nodes: "{{ .outputs.source.c.target }}", Outputs: map[string]any{"name": "selected during the run"}},
		config.FleetStage{ID: "report", Nodes: "c", NodeTasks: []config.Task{{Show: "{{ .outputs.on_resume.b.name }}"}, {Show: "{{ .outputs.during_run.c.name }}"}, {Show: "{{ .fleet_stages.on_resume.b.status }}"}}},
	)
	fail := true
	localNodes(&c, func(s config.FleetStage, en Entry) *protocol.Response {
		if s.ID == "gate" && fail {
			return &protocol.Response{Status: "failed", Error: "gate closed"}
		}
		return nil
	})
	execute(t, c, r, false)
	if sel := r.Selections["on_resume"]; sel.Resolved {
		t.Fatalf("on_resume resolved without gate's output: %+v", sel)
	}
	if during := entry(t, r, "during_run", "c").Response; during.Status != "succeeded" || during.Outputs["name"] != "selected during the run" {
		t.Fatal(during)
	}
	loaded, err := c.Load(r.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	fail = false
	execute(t, c, loaded, true)
	// The scenario: during_run's entry now precedes on_resume's, although
	// on_resume comes first in the playbook.
	at := map[string]int{}
	for i, en := range loaded.Entries {
		at[en.FleetStage] = i
	}
	if at["during_run"] > at["on_resume"] {
		t.Fatal("the entries are not in the order of resolution", loaded.Entries)
	}
	for _, view := range []func() *Run{
		func() *Run { return loaded },
		func() *Run {
			saved, err := c.Load(r.ID, false)
			if err != nil {
				t.Fatal(err)
			}
			return saved
		},
	} {
		run := view()
		if len(run.Entries) != 5 || run.Status != "succeeded" {
			t.Fatal(run.Status, run.Entries)
		}
		if onResume := entry(t, run, "on_resume", "b").Response; onResume.Outputs["name"] != "selected on resume" {
			t.Fatal(onResume)
		}
		if during := entry(t, run, "during_run", "c").Response; during.Outputs["name"] != "selected during the run" {
			t.Fatal(during)
		}
		var shown []any
		for _, result := range entry(t, run, "report", "c").Response.Results {
			shown = append(shown, result.Value)
		}
		if want := []any{"selected on resume", "selected during the run", "succeeded"}; !reflect.DeepEqual(shown, want) {
			t.Fatal(shown)
		}
	}
}

// A limit narrows static and runtime selections, blocks work that reads a node
// it left out, and stays with the run for retry.
func TestLimitNarrowsEverySelectionAndRetryKeepsIt(t *testing.T) {
	c := Controller{State: t.TempDir()}
	inv := config.Inventory{Nodes: map[string]config.Node{"a": {Address: "a"}, "b": {Address: "b"}, "c": {Address: "c"}}}
	p := config.Project{Project: "fleet", MaxNodesAtOnce: 2, FleetStages: []config.FleetStage{
		{ID: "inspect", Nodes: "all", Outputs: map[string]any{"port": 8080}},
		{ID: "db", Nodes: "c"},
		{ID: "roll", Loop: []any{"a", "b"}, As: "app", Key: "{{ .app }}", FleetStages: []config.FleetStage{
			{ID: "update", Nodes: "{{ .app }}", NodeTasks: []config.Task{{Show: "{{ .outputs.inspect.a.port }}"}}},
			{ID: "peer", Nodes: "{{ .app }}", NodeTasks: []config.Task{{Show: "{{ .outputs.inspect.b.port }}"}}},
		}},
		{ID: "after", Nodes: "a", Requires: config.Requires{AllNodes: []string{"db"}}},
	}}
	r, err := c.New(context.Background(), p, inv, []string{"a"}, false)
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	var mu sync.Mutex
	localNodes(&c, func(s config.FleetStage, en Entry) *protocol.Response {
		mu.Lock()
		counts[s.ID+"/"+en.Node]++
		mu.Unlock()
		return nil
	})
	for attempt, retry := range []bool{false, true} {
		if attempt == 1 {
			if r, err = c.Load(ShortID(r.ID), true); err != nil {
				t.Fatal(err)
			}
		}
		execute(t, c, r, retry)
		// Retry repeats only the blocked host step, within the recorded limit.
		want := map[string]int{"inspect/a": 1, "roll[a]/update/a": 1, "roll[a]/peer/a": attempt + 1}
		if !reflect.DeepEqual(counts, want) || len(r.Entries) != 4 || !reflect.DeepEqual(r.Limit, []string{"a"}) || r.Status != "failed" {
			t.Fatal(attempt, counts, r.Entries, r.Limit)
		}
		if sel := r.Selections["db"]; len(sel.Nodes) != 0 || !reflect.DeepEqual(sel.LeftOut, []string{"c"}) {
			t.Fatalf("%+v", sel)
		}
		if sel := r.Selections["roll[b]/update"]; !sel.Resolved || len(sel.Nodes) != 0 || !reflect.DeepEqual(sel.LeftOut, []string{"b"}) {
			t.Fatalf("%+v", sel)
		}
		if peer := entry(t, r, "roll[a]/peer", "a").Response; peer.Status != "blocked" || !strings.Contains(peer.Error, "inspect on b is left out by the limit a") {
			t.Fatal(peer)
		}
		if after := entry(t, r, "after", "a").Response; after.Status != "blocked" || after.Error != "requires all_nodes fleet stage db, which the limit a leaves out" {
			t.Fatal(after)
		}
	}
}

// Command-line values and group vars reach node jobs, and the -e values saved
// with the run are the ones retry uses.
func TestRetryReusesSavedCommandLineValues(t *testing.T) {
	c := Controller{State: t.TempDir()}
	inv := config.Inventory{Nodes: map[string]config.Node{"a": {Address: "a"}, "b": {Address: "b"}},
		Groups: map[string]config.NodeGroup{"web": {Nodes: []string{"a", "b"}, Vars: map[string]any{"port": 9090}}}}
	p := config.Project{Project: "fleet", MaxNodesAtOnce: 2,
		CommandLineVars:   map[string]config.CommandLineVar{"release": {Type: "string", Default: "1.0"}},
		CommandLineValues: map[string]any{"release": "1.4.3"},
		FleetStages:       []config.FleetStage{{ID: "deploy", Nodes: "web", NodeTasks: []config.Task{{Show: "{{ .vars.release }}:{{ .vars.port }}"}}}}}
	r, err := c.New(context.Background(), p, inv, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	fail := true
	localNodes(&c, func(s config.FleetStage, en Entry) *protocol.Response {
		if en.Node == "b" && fail {
			return &protocol.Response{Status: "failed", Error: "unreachable"}
		}
		return nil
	})
	execute(t, c, r, false)
	if got := entry(t, r, "deploy", "a").Response.Results[0].Value; got != "1.4.3:9090" {
		t.Fatal(got)
	}
	if r, err = c.Load(r.ID, true); err != nil {
		t.Fatal(err)
	}
	fail = false
	execute(t, c, r, true)
	if got := entry(t, r, "deploy", "b").Response; got.Status != "succeeded" || got.Results[0].Value != "1.4.3:9090" {
		t.Fatal(got)
	}
}

// A short run ID selects its run unless another retained run shares it.
func TestShortRunIDSelectsAnUnambiguousRun(t *testing.T) {
	c, r := fleet(t, config.FleetStage{ID: "setup", Nodes: "all"})
	loaded, err := c.Load(ShortID(r.ID), false)
	if err != nil || loaded.ID != r.ID || len(ShortID(r.ID)) != 8 {
		t.Fatal(ShortID(r.ID), err)
	}
	twin := *r
	_, random, _ := strings.Cut(r.ID, "-")
	twin.ID = "20000101T000000.000000000-" + random[:8] + "00000000"
	if err := c.createRun(&twin); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Load(ShortID(r.ID), false); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatal(err)
	}
	if loaded, err := c.Load(r.ID, false); err != nil || loaded.ID != r.ID {
		t.Fatal(err)
	}
}

// A skipped prerequisite does not satisfy requires, and retry keeps it skipped.
func TestSkippedPrerequisiteBlocks(t *testing.T) {
	c, r := fleet(t,
		config.FleetStage{ID: "optional", Nodes: []any{"a", "b"}, When: false},
		config.FleetStage{ID: "same", Nodes: "a", Requires: config.Requires{SameNode: []string{"optional"}}},
		config.FleetStage{ID: "all", Nodes: "c", Requires: config.Requires{AllNodes: []string{"optional"}}},
	)
	counts := map[string]int{}
	var mu sync.Mutex
	localNodes(&c, func(s config.FleetStage, en Entry) *protocol.Response {
		mu.Lock()
		counts[s.ID+"/"+en.Node]++
		mu.Unlock()
		return nil
	})
	for attempt, retry := range []bool{false, true} {
		execute(t, c, r, retry)
		if r.Status != "failed" || counts["optional/a"] != 0 || counts["same/a"]+counts["all/c"] != 0 {
			t.Fatal(attempt, r.Status, counts)
		}
		for _, en := range []Entry{entry(t, r, "same", "a"), entry(t, r, "all", "c")} {
			if en.Response.Status != "blocked" {
				t.Fatal(attempt, en.Response)
			}
		}
		var err error
		if r, err = c.Load(r.ID, true); err != nil {
			t.Fatal(err)
		}
	}
}

func TestFleetStageLoopRetryPreservesOutputsAndSelection(t *testing.T) {
	c, r := fleet(t,
		config.FleetStage{ID: "choose", Nodes: "c", Outputs: map[string]any{"nodes": []any{"b", "a"}}},
		config.FleetStage{ID: "rollout", Loop: "{{ .outputs.choose.c.nodes }}", As: "app", Key: "{{ .app }}", FleetStages: []config.FleetStage{
			{ID: "inspect", Nodes: "{{ .app }}", Outputs: map[string]any{"name": "{{ .app }}"}},
			{ID: "update", Nodes: "{{ .app }}", OnNodeFailure: "stop_run", NodeTasks: []config.Task{{Show: "{{ .outputs.inspect[.app].name }}"}}, Outputs: map[string]any{"name": "{{ .app }}"}},
		}, Outputs: map[string]any{"name": "{{ .outputs.update[.app].name }}"}},
		config.FleetStage{ID: "report", Nodes: "c", NodeTasks: []config.Task{{Show: "{{ .outputs.rollout }}"}}},
	)
	fail := true
	calls := []string{}
	localNodes(&c, func(s config.FleetStage, en Entry) *protocol.Response {
		calls = append(calls, s.ID+"/"+en.Node)
		if s.ID == "rollout[b]/update" && fail {
			return &protocol.Response{Status: "failed", Error: "unhealthy"}
		}
		return nil
	})
	execute(t, c, r, false)
	if r.Status != "failed" || !hasUnfinished(r) {
		t.Fatal(r.Status)
	}
	for _, call := range calls {
		if strings.Contains(call, "rollout[a]") {
			t.Fatal("next server ran after stop_run", calls)
		}
	}
	r, err := c.Load(r.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	fail = false
	execute(t, c, r, true)
	want := []string{"choose/c", "rollout[b]/inspect/b", "rollout[b]/update/b", "rollout[b]/update/b", "rollout[a]/inspect/a", "rollout[a]/update/a", "report/c"}
	if r.Status != "succeeded" || !reflect.DeepEqual(calls, want) {
		t.Fatal(r.Status, calls)
	}
	got := entry(t, r, "report", "c").Response.Results[0].Value.(map[string]any)
	if got["a"].(map[string]any)["name"] != "a" || got["b"].(map[string]any)["name"] != "b" {
		t.Fatal("iteration output leaked", got)
	}
	loaded, err := c.Load(r.ID, false)
	if err != nil || hasUnfinished(loaded) {
		t.Fatal(loaded, err)
	}
	if loaded.Selections["rollout"].Iterations["a"].Outputs["name"] != "a" {
		t.Fatal("loop exports not persisted")
	}
}

func TestDuplicateFleetStageKeysAndLoopOutputFailuresAreVisible(t *testing.T) {
	for _, duplicate := range []bool{false, true} {
		t.Run(fmt.Sprint(duplicate), func(t *testing.T) {
			items := []any{"a", "b"}
			if duplicate {
				items[1] = "a"
			}
			c, r := fleet(t, config.FleetStage{ID: "roll", Loop: items, As: "app", Key: "{{ .app }}", FleetStages: []config.FleetStage{{ID: "write", Nodes: "{{ .app }}", Outputs: map[string]any{"value": true}}}, Outputs: map[string]any{"bad": "{{ .vars.missing }}"}})
			calls := 0
			localNodes(&c, func(config.FleetStage, Entry) *protocol.Response { calls++; return nil })
			execute(t, c, r, false)
			if r.Status != "failed" || !hasUnfinished(r) {
				t.Fatal(r.Status)
			}
			if duplicate && calls != 0 {
				t.Fatal("started before key validation")
			}
			loaded, err := c.Load(r.ID, true)
			if err != nil || !hasUnfinished(loaded) {
				t.Fatal(loaded, err)
			}
		})
	}
}

// A selection, condition, loop, or output known only from an earlier fleet stage's
// output fails with the YAML field that consumed it.
func TestFleetStageSelectionErrorsNameTheirField(t *testing.T) {
	at := func(fleetStage int, fields ...string) config.Sources {
		sources := config.Sources{"": {File: "mmx.yml", Line: fleetStage + 4, Column: 3, Field: fmt.Sprintf("fleet_stages[%d]", fleetStage)}}
		for _, field := range fields {
			sources[field] = config.Location{File: "mmx.yml", Line: fleetStage + 4, Column: 10, Field: fmt.Sprintf("fleet_stages[%d].%s", fleetStage, field)}
		}
		return sources
	}
	inner := []config.FleetStage{{ID: "inside", Nodes: "a", NodeTasks: []config.Task{{Show: "x"}}}}
	c, r := fleet(t,
		config.FleetStage{ID: "producer", Nodes: "a", Outputs: map[string]any{"text": "nohost"}},
		config.FleetStage{ID: "nodes", Nodes: "{{ .outputs.producer.a.text }}", NodeTasks: []config.Task{{Show: "x"}}, Sources: at(1, "nodes")},
		config.FleetStage{ID: "loop", Loop: "{{ .outputs.producer.a.text }}", As: "item", Key: "{{ .item }}", FleetStages: inner, Sources: at(2, "loop")},
		config.FleetStage{ID: "when", Loop: []any{"one"}, As: "item", Key: "{{ .item }}", When: "{{ .outputs.producer.a.text }}", FleetStages: inner, Sources: at(3, "when")},
		config.FleetStage{ID: "outputs", Loop: []any{"one"}, As: "item", Key: "{{ .item }}", FleetStages: inner, Outputs: map[string]any{"list": "x{{ .vars.none }}"}, Sources: at(4, "outputs.list")},
	)
	localNodes(&c, nil)
	execute(t, c, r, false)
	for path, want := range map[string]string{
		"nodes":   r.Selections["nodes"].Response.Error,
		"loop":    r.Selections["loop"].Response.Error,
		"when":    r.Selections["when"].Iterations["one"].Error,
		"outputs": r.Selections["outputs"].Iterations["one"].Error,
	} {
		field := map[string]string{"nodes": "fleet_stages[1].nodes", "loop": "fleet_stages[2].loop", "when": "fleet_stages[3].when", "outputs": "fleet_stages[4].outputs.list"}[path]
		if !strings.Contains(want, "mmx.yml:") || !strings.Contains(want, field) {
			t.Errorf("%s: %q", path, want)
		}
	}
}

// In a dryrun, a node task of an earlier fleet stage on the node that would
// change it counts as an earlier node task: a later fleet stage's check that
// fails on the node as it is now is not checked, and the fleet stages after
// it still run. Without that change, the check's failure stands.
func TestDryrunCheckAfterAnEarlierFleetStageChangeIsNotChecked(t *testing.T) {
	for _, earlier := range []bool{true, false} {
		t.Run(fmt.Sprint(earlier), func(t *testing.T) {
			release := filepath.Join(t.TempDir(), "release")
			// A shell task without a dryrun inspection may change the node;
			// one with an inspection that passes changes nothing.
			install := config.Task{ID: "unpack", Module: "shell", ModuleInputs: &modules.Shell{Script: "touch " + release}}
			if !earlier {
				install.ModuleInputs = &modules.Shell{Script: "true", Dryrun: "true", ChangedWhen: false}
			}
			check := config.Task{ID: "check", Module: "shell", ModuleInputs: &modules.Shell{Script: "true", Dryrun: "test -e " + release, ChangedWhen: false}}
			later := config.Task{ID: "later", Module: "shell", ModuleInputs: &modules.Shell{Script: "true", Dryrun: "true"}}
			c, r := fleet(t,
				config.FleetStage{ID: "install", Nodes: "a", NodeTasks: []config.Task{install}},
				config.FleetStage{ID: "candidate", Nodes: "a", NodeTasks: []config.Task{check}},
				config.FleetStage{ID: "roles", Nodes: "a", NodeTasks: []config.Task{later}},
			)
			r.Dryrun = true
			if err := c.createRun(r); err != nil {
				t.Fatal(err)
			}
			localNodes(&c, nil)
			execute(t, c, r, false)
			got := entry(t, r, "candidate", "a").Response
			if !earlier {
				if got.Status != "failed" {
					t.Fatalf("%+v", got)
				}
				return
			}
			if got.Status != "succeeded" || len(got.Results) != 1 || got.Results[0].Status != "not_checked" ||
				!strings.Contains(got.Results[0].Reason, "an earlier node task, unpack in the fleet stage install, would change the node before the run reaches the check") {
				t.Fatalf("%+v", got)
			}
			if roles := entry(t, r, "roles", "a").Response; roles.Status != "succeeded" {
				t.Fatalf("a later fleet stage did not run: %+v", roles)
			}
		})
	}
}

func TestDryrunOutputUnknownDoesNotBecomeFalse(t *testing.T) {
	path := filepath.Join(t.TempDir(), "must-not-exist")
	c, r := fleet(t,
		config.FleetStage{ID: "producer", Nodes: "a", NodeTasks: []config.Task{{ID: "command", Module: "shell", ModuleInputs: &modules.Shell{Script: "printf ready"}}}, Outputs: map[string]any{"value": "{{ .node_tasks.command.stdout }}"}},
		config.FleetStage{ID: "consumer", Nodes: "a", When: map[string]any{"equals": []any{"{{ .outputs.producer.a.value }}", "ready"}}, NodeTasks: []config.Task{{Module: "shell", ModuleInputs: &modules.Shell{Script: "touch " + path}}}},
		config.FleetStage{ID: "loop", Loop: "{{ .outputs.producer.a.value }}", As: "item", Key: "{{ .item }}", FleetStages: []config.FleetStage{{ID: "inside", Nodes: "a"}}},
	)
	r.Dryrun = true
	if err := c.createRun(r); err != nil {
		t.Fatal(err)
	}
	localNodes(&c, nil)
	execute(t, c, r, false)
	if r.Status != "succeeded" || entry(t, r, "producer", "a").Response.Status != "not_checked" || entry(t, r, "consumer", "a").Response.Status != "not_checked" || r.Selections["loop"].Response.Status != "not_checked" {
		t.Fatal(r.Status, r.Entries)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("preview mutated", err)
	}
}

// In dryrun, work whose unmet prerequisites could not be checked (an entry, a
// selection, or a loop iteration) is not_checked; any other unmet one blocks it.
func TestDryrunRequiresNotCheckedPrerequisite(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprint(fail), func(t *testing.T) {
			probe := func(nodes any) config.FleetStage {
				return config.FleetStage{ID: "producer", Nodes: nodes, NodeTasks: []config.Task{{ID: "command", Module: "shell", ModuleInputs: &modules.Shell{Script: "printf ready"}}}, Outputs: map[string]any{"value": "{{ .node_tasks.command.stdout }}"}}
			}
			inner := probe("{{ .app }}")
			c, r := fleet(t,
				probe("a"),
				config.FleetStage{ID: "consumer", Nodes: "a", Requires: config.Requires{SameNode: []string{"producer"}}},
				config.FleetStage{ID: "dynamic", Nodes: "{{ .outputs.producer.a.value }}"},
				config.FleetStage{ID: "after_dynamic", Nodes: "a", Requires: config.Requires{SameNode: []string{"dynamic"}}},
				config.FleetStage{ID: "roll", Loop: []any{"a"}, As: "app", Key: "{{ .app }}", FleetStages: []config.FleetStage{inner}},
				config.FleetStage{ID: "after_roll", Nodes: "c", Requires: config.Requires{AllNodes: []string{"roll"}}},
				config.FleetStage{ID: "checked", Nodes: "b"},
				config.FleetStage{ID: "mixed", Nodes: "b", Requires: config.Requires{SameNode: []string{"checked"}, AllNodes: []string{"producer"}}},
			)
			r.Dryrun = true
			if err := c.createRun(r); err != nil {
				t.Fatal(err)
			}
			localNodes(&c, func(s config.FleetStage, _ Entry) *protocol.Response {
				if s.ID == "checked" && fail {
					return &protocol.Response{Status: "failed", Error: "inspection failed"}
				}
				return nil
			})
			execute(t, c, r, false)
			if r.Selections["dynamic"].Response.Status != "not_checked" || r.Selections["roll"].Iterations["a"].Status != "not_checked" {
				t.Fatal("setup", r.Selections["dynamic"].Response, r.Selections["roll"].Iterations)
			}
			for fleetStage, managedNode := range map[string]string{"consumer": "a", "after_dynamic": "a", "after_roll": "c"} {
				if got := entry(t, r, fleetStage, managedNode).Response; got.Status != "not_checked" || !strings.HasPrefix(got.Error, "requires ") {
					t.Fatalf("%s: %+v", fleetStage, got)
				}
			}
			mixed, want, status := entry(t, r, "mixed", "b").Response, "not_checked", "succeeded"
			if fail {
				want, status = "blocked", "failed"
			}
			if mixed.Status != want || r.Status != status {
				t.Fatalf("mixed %+v; run %s", mixed, r.Status)
			}
		})
	}
}

func TestStopBetweenIterationsKeepsFutureWorkRetryable(t *testing.T) {
	c, r := fleet(t, config.FleetStage{ID: "roll", Loop: []any{"a", "b"}, As: "app", Key: "{{ .app }}", FleetStages: []config.FleetStage{{ID: "setup", Nodes: "{{ .app }}"}}})
	localNodes(&c, func(s config.FleetStage, en Entry) *protocol.Response {
		if s.ID == "roll[a]/setup" {
			if err := c.requestStop(r.ID); err != nil {
				t.Error(err)
			}
		}
		return nil
	})
	execute(t, c, r, false)
	if r.Status != "stopped" || !hasUnfinished(r) {
		t.Fatal(r.Status)
	}
	var err error
	r, err = c.Load(r.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	calls := []string{}
	localNodes(&c, func(s config.FleetStage, en Entry) *protocol.Response { calls = append(calls, s.ID); return nil })
	execute(t, c, r, true)
	if r.Status != "succeeded" || !reflect.DeepEqual(calls, []string{"roll[b]/setup"}) {
		t.Fatal(r.Status, calls)
	}
}

func TestNestedAndEmptyFleetStageLoops(t *testing.T) {
	c, r := fleet(t,
		config.FleetStage{ID: "outer", Loop: []any{"a", "b"}, As: "app", Key: "{{ .app }}", FleetStages: []config.FleetStage{
			{ID: "inner", Loop: map[string]any{"z": 3, "a": 1}, As: "port", Key: "{{ .port.key }}", FleetStages: []config.FleetStage{
				{ID: "inspect", Nodes: "{{ .app }}", NodeTasks: []config.Task{{ID: "local", SetVars: map[string]any{"private": "{{ .port.value }}"}}}, Outputs: map[string]any{"port": "{{ .vars.private }}"}},
			}, Outputs: map[string]any{"port": "{{ .outputs.inspect[.app].port }}"}},
		}, Outputs: map[string]any{"ports": "{{ .outputs.inner }}"}},
		config.FleetStage{ID: "empty", Loop: []any{}, As: "unused", Key: "{{ .unused }}", FleetStages: []config.FleetStage{{ID: "never", Nodes: "c"}}},
		config.FleetStage{ID: "report", Nodes: "c", NodeTasks: []config.Task{{Show: "{{ .outputs.outer }}"}, {Show: "{{ .outputs.empty }}"}, {Show: "{{ .fleet_stages.outer.a.change }}"}}},
	)
	var calls []string
	localNodes(&c, func(s config.FleetStage, _ Entry) *protocol.Response { calls = append(calls, s.ID); return nil })
	execute(t, c, r, false)
	want := []string{"outer[a]/inner[a]/inspect", "outer[a]/inner[z]/inspect", "outer[b]/inner[a]/inspect", "outer[b]/inner[z]/inspect", "report"}
	if r.Status != "succeeded" || !reflect.DeepEqual(calls, want) {
		t.Fatal(r.Status, calls)
	}
	for _, item := range []string{"a", "b"} {
		ports := r.Selections["outer"].Iterations[item].Outputs["ports"].(map[string]any)
		if fmt.Sprint(ports["z"].(map[string]any)["port"]) != "3" {
			t.Fatal(ports)
		}
	}
	loaded, err := c.Load(r.ID, false)
	if err != nil || hasUnfinished(loaded) {
		t.Fatal(loaded, err)
	}
}

func TestRetryAfterDisconnectBeforeFirstLoopCompletion(t *testing.T) {
	c, r := fleet(t, config.FleetStage{ID: "roll", Loop: []any{"a", "b"}, As: "app", Key: "{{ .app }}", FleetStages: []config.FleetStage{{ID: "inspect", Nodes: "{{ .app }}", Outputs: map[string]any{"name": "{{ .app }}"}}}, Outputs: map[string]any{"name": "{{ .outputs.inspect[.app].name }}"}})
	localNodes(&c, nil)
	executeNode := c.executeNode
	c.executeNode = func(ctx context.Context, r *Run, s config.FleetStage, en Entry, retry bool, save func(Entry) error) (Entry, error) {
		result, err := executeNode(ctx, r, s, en, retry, save)
		if err != nil {
			return result, err
		}
		return result, fmt.Errorf("controller disconnected after collecting the node result")
	}
	if err := c.Execute(context.Background(), r, false); err == nil {
		t.Fatal("injected disconnect missing")
	}
	loaded, err := c.Load(r.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Selections["roll"].Iterations) != 0 {
		t.Fatal("loop completion unexpectedly saved")
	}
	var calls []string
	localNodes(&c, func(s config.FleetStage, _ Entry) *protocol.Response { calls = append(calls, s.ID); return nil })
	execute(t, c, loaded, true)
	if loaded.Status != "succeeded" || !reflect.DeepEqual(calls, []string{"roll[b]/inspect"}) {
		t.Fatal(loaded.Status, calls)
	}
	if loaded.Selections["roll"].Iterations["a"].Outputs["name"] != "a" {
		t.Fatal("lost completed producer output")
	}
}

func TestLoopIterationWithUnansweredNodeJobStaysUncertain(t *testing.T) {
	c, r := fleet(t, config.FleetStage{ID: "roll", Loop: []any{"a"}, As: "app", Key: "{{ .app }}", FleetStages: []config.FleetStage{{ID: "inspect", Nodes: "{{ .app }}"}}})
	answered := false
	var calls []string
	localNodes(&c, func(s config.FleetStage, _ Entry) *protocol.Response {
		calls = append(calls, s.ID)
		if !answered {
			return &protocol.Response{Status: "not_found", Error: "node history is unavailable"}
		}
		return nil
	})
	execute(t, c, r, false)
	if iteration := r.Selections["roll"].Iterations["a"]; iteration.Status != "not_found" || r.Status != "running" || !hasUnfinished(r) {
		t.Fatal(iteration, r.Status)
	}
	loaded, err := c.Load(r.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	answered = true
	execute(t, c, loaded, true)
	if loaded.Status != "succeeded" || !reflect.DeepEqual(calls, []string{"roll[a]/inspect", "roll[a]/inspect"}) {
		t.Fatal(loaded.Status, calls)
	}
}

// rollout is the rolling-deployment shape: every enclosed fleet stage uses stop_run;
// a and b are the apps and c is the load balancer.
func rollout(t *testing.T) (Controller, *Run) {
	fleetStage := func(id string, nodes any) config.FleetStage {
		return config.FleetStage{ID: id, Nodes: nodes, OnNodeFailure: "stop_run"}
	}
	return fleet(t,
		config.FleetStage{ID: "roll", Loop: []any{"a", "b"}, As: "app", Key: "{{ .app }}", FleetStages: []config.FleetStage{
			fleetStage("drain", "c"), fleetStage("update", "{{ .app }}"), fleetStage("rejoin", "c"),
		}},
		config.FleetStage{ID: "report", Nodes: "c"},
	)
}

// A stop or stopping policy that ends a fleet stage loop records the work it kept
// from starting, with its cause, so output shows it and retry selects it.
func TestStoppedLoopRecordsUnstartedWork(t *testing.T) {
	for _, variant := range []string{"stop_run", "stop"} {
		t.Run(variant, func(t *testing.T) {
			c, r := rollout(t)
			cause := "not started: stop requested"
			if variant == "stop_run" {
				cause = "not started: on_node_failure stop_run after roll[a]/update failed on a"
			}
			c.executeNode = func(_ context.Context, _ *Run, s config.FleetStage, en Entry, _ bool, _ func(Entry) error) (Entry, error) {
				en.NodeJobAttempt++
				en.Response.Status = "succeeded"
				if variant == "stop_run" && s.ID == "roll[a]/update" {
					en.Response.Status = "failed"
				}
				if variant == "stop" && s.ID == "roll[a]/drain" {
					if err := c.requestStop(r.ID); err != nil {
						t.Error(err)
					}
				}
				return en, nil
			}
			execute(t, c, r, false)
			loaded, err := c.Load(r.ID, true)
			if err != nil {
				t.Fatal(err)
			}
			if got := loaded.Selections["roll"].Iterations["b"]; got.Status != "stopped" || got.Error != cause {
				t.Fatalf("unstarted iteration: %+v", got)
			}
			later := []Entry{entry(t, loaded, "roll[a]/rejoin", "c"), entry(t, loaded, "report", "c")}
			if variant == "stop" {
				later = append(later, entry(t, loaded, "roll[a]/update", "a"))
			}
			for _, en := range later {
				if en.NodeJobAttempt != 0 || en.Response.Status != "stopped" || en.Response.Error != cause {
					t.Fatalf("%s/%s: %+v", en.FleetStage, en.Node, en.Response)
				}
			}
			var calls []string
			c.executeNode = func(_ context.Context, _ *Run, s config.FleetStage, en Entry, _ bool, _ func(Entry) error) (Entry, error) {
				calls = append(calls, s.ID+"/"+en.Node)
				en.NodeJobAttempt++
				en.Response.Status = "succeeded"
				return en, nil
			}
			execute(t, c, loaded, true)
			want := []string{"roll[a]/update/a", "roll[a]/rejoin/c", "roll[b]/drain/c", "roll[b]/update/b", "roll[b]/rejoin/c", "report/c"}
			if loaded.Status != "succeeded" || !reflect.DeepEqual(calls, want) {
				t.Fatal(loaded.Status, calls)
			}
		})
	}
}

// After a stop or stop_run, a later selection that reads .fleet stages outcomes stays
// unselected, so retry resolves it from the outcomes current then instead of
// acting on (or skipping) stop-time values.
func TestSelectionReadingOutcomesWaitsForRetryAfterStop(t *testing.T) {
	for _, variant := range []string{"stop", "stop_run"} {
		t.Run(variant, func(t *testing.T) {
			c, r := fleet(t,
				config.FleetStage{ID: "probe", Nodes: []any{"a", "b"}, MaxNodesAtOnce: "1", OnNodeFailure: map[string]string{"stop": "continue", "stop_run": "stop_run"}[variant]},
				config.FleetStage{ID: "follow", Loop: "{{ .fleet_stages.probe }}", As: "h", Key: "{{ .h.key }}",
					When:        map[string]any{"equals": []any{"{{ .h.value.status }}", "succeeded"}},
					FleetStages: []config.FleetStage{{ID: "act", Nodes: "{{ .h.key }}"}}},
			)
			var calls []string
			first := true
			c.executeNode = func(_ context.Context, _ *Run, s config.FleetStage, en Entry, _ bool, _ func(Entry) error) (Entry, error) {
				calls = append(calls, s.ID+"/"+en.Node)
				en.NodeJobAttempt++
				en.Response.Status = "succeeded"
				if first && s.ID == "probe" && en.Node == "a" {
					if variant == "stop" {
						if err := c.requestStop(r.ID); err != nil {
							t.Error(err)
						}
					} else {
						en.Response.Status = "failed"
					}
				}
				return en, nil
			}
			execute(t, c, r, false)
			loaded, err := c.Load(r.ID, true)
			if err != nil {
				t.Fatal(err)
			}
			if sel := loaded.Selections["follow"]; sel.Resolved || sel.Response.Status != "stopped" || !strings.HasPrefix(sel.Response.Error, "not started: ") {
				t.Fatalf("follow was selected from stop-time outcomes: %+v", sel)
			}
			first, calls = false, nil
			execute(t, c, loaded, true)
			want := []string{"probe/b", "follow[a]/act/a", "follow[b]/act/b"}
			if variant == "stop_run" {
				want = []string{"probe/a", "probe/b", "follow[a]/act/a", "follow[b]/act/b"}
			}
			if loaded.Status != "succeeded" || !reflect.DeepEqual(calls, want) {
				t.Fatal(loaded.Status, calls)
			}
		})
	}
}

// mustJob is a node job that a test's playbook always gives.
func mustJob(job config.Job, err error) config.Job {
	if err != nil {
		panic(err)
	}
	return job
}

func TestNodeJobNamesTheAccountAndControllerMachineThatStartedIt(t *testing.T) {
	account, hostName := startedBy()
	if account == "" || hostName == "" {
		t.Skip("account or host name unknown")
	}
	r := &Run{Project: config.Project{Project: "p"}, Inventory: config.Inventory{Nodes: map[string]config.Node{"a": {}}}}
	j, err := nodeJob(r, config.FleetStage{ID: "s"}, Entry{Node: "a", JobID: "x"})
	if err != nil || j.StartedByAccount != account || j.StartedFromHostName != hostName {
		t.Fatal(j.StartedByAccount, j.StartedFromHostName, err)
	}
}
