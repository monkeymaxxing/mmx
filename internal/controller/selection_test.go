package controller

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"mmx/internal/config"
	"mmx/internal/protocol"
	"mmx/internal/secrets"
)

// playbookRun creates a run of the playbook site for the inventory
// production, whose nodes are a, b, and c.
func playbookRun(t *testing.T, p config.Project, limit []string, dryrun bool) (Controller, *Run) {
	t.Helper()
	c := Controller{State: t.TempDir(), Selection: Identity{Inventory: "production"}}
	p.Project, p.PlaybookName, p.MaxNodesAtOnce = "fleet", "site", 2
	p.Vars = map[string]any{"skip": []any{"c"}}
	inv := config.Inventory{Name: "production", Nodes: map[string]config.Node{"a": {Address: "a"}, "b": {Address: "b"}, "c": {Address: "c"}},
		Groups: map[string]config.NodeGroup{"virtual_machines": {Nodes: []string{"b"}}}}
	r, err := c.New(context.Background(), p, inv, limit, dryrun)
	if err != nil {
		t.Fatal(err)
	}
	return c, r
}

// recorder collects the last successful run records the run asks nodes to write.
type recorder struct {
	mu      sync.Mutex
	records map[string]protocol.LastSuccessfulRunRequest
	refuse  string // a node whose record cannot be written
}

func (rec *recorder) attach(c *Controller) {
	c.recordNode = func(_ context.Context, _ *Run, en Entry, request protocol.LastSuccessfulRunRequest) error {
		rec.mu.Lock()
		defer rec.mu.Unlock()
		if en.Node == rec.refuse {
			return errors.New("node unreachable")
		}
		rec.records[en.Node] = request
		return nil
	}
}
func (rec *recorder) nodes() []string {
	var out []string
	for node := range rec.records {
		out = append(out, node)
	}
	sort.Strings(out)
	return out
}

// except_nodes removes nodes from a static selection and from one the walker
// resolves; --only-fleet-stages leaves the other fleet stages out of the run,
// blocks work that reads them, naming the option, and stays with the run for
// resume.
func TestExceptNodesAndOnlyFleetStagesSelectTheRun(t *testing.T) {
	c, r := playbookRun(t, config.Project{OnlyFleetStages: []string{"network", "app", "report"}, FleetStages: []config.FleetStage{
		{ID: "ruby", Nodes: "all"},
		{ID: "network", Nodes: "all", ExceptNodes: []any{"virtual_machines"}},
		{ID: "app", Nodes: "all", ExceptNodes: "{{ .vars.skip }}"},
		{ID: "report", Nodes: "c", NodeTasks: []config.Task{{Show: "{{ .fleet_stages.ruby.c.status }}"}}},
	}}, nil, false)
	fail := true
	var calls []string
	var mu sync.Mutex
	localNodes(&c, func(s config.FleetStage, en Entry) *protocol.Response {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, s.ID+"/"+en.Node)
		if s.ID == "app" && en.Node == "b" && fail {
			return &protocol.Response{Status: "failed", Error: "app failed"}
		}
		return nil
	})
	execute(t, c, r, false)
	if r.Selections["ruby"] != nil || !reflect.DeepEqual(r.Selections["network"].Nodes, []string{"a", "c"}) || !reflect.DeepEqual(r.Selections["app"].Nodes, []string{"a", "b"}) {
		t.Fatalf("%+v %+v %+v", r.Selections["ruby"], r.Selections["network"], r.Selections["app"])
	}
	if report := entry(t, r, "report", "c").Response; report.Status != "blocked" || !strings.Contains(report.Error, "fleet stage ruby is not selected: --only-fleet-stages network,app,report leaves it out") {
		t.Fatal(report)
	}
	loaded, err := c.Load(r.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(loaded.Project.OnlyFleetStages, []string{"network", "app", "report"}) {
		t.Fatal(loaded.Project.OnlyFleetStages)
	}
	fail, calls = false, nil
	execute(t, c, loaded, true)
	sort.Strings(calls)
	if want := []string{"app/b", "report/c"}; !reflect.DeepEqual(calls, want) || loaded.Selections["ruby"] != nil {
		t.Fatal(calls)
	}
}

// A run loads only the secrets of the fleet stages --only-fleet-stages keeps,
// so a left-out fleet stage's secret need not be available.
func TestOnlyFleetStagesLoadsOnlyTheKeptSecrets(t *testing.T) {
	t.Setenv("MMX_TEST_MISSING_SECRET", "")
	c, r := playbookRun(t, config.Project{OnlyFleetStages: []string{"first"}, Secrets: map[string]string{"missing": "MMX_TEST_MISSING_SECRET"}, FleetStages: []config.FleetStage{
		{ID: "first", Nodes: "a", NodeTasks: []config.Task{{Show: "ordinary work"}}},
		{ID: "second", Nodes: "a", NodeTasks: []config.Task{{Show: "{{ .secrets.missing }}"}}},
	}}, nil, false)
	localNodes(&c, nil)
	execute(t, c, r, false)
	if r.Status != "succeeded" || len(r.Entries) != 1 {
		t.Fatal(r.Status, r.Entries)
	}
}

// presentOnlyVault makes a vault that holds only the key present, and one
// store for the runs of a test, which unlocks the vault once.
func presentOnlyVault(t *testing.T) (string, *secrets.Store) {
	t.Helper()
	vault := filepath.Join(t.TempDir(), "secrets_vault.age")
	if err := secrets.Write(vault, "vault-pass", []byte("present: present-value\n")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MMX_VAULT_PASSWORD", "vault-pass")
	return vault, &secrets.Store{}
}

// readsSecret is a node task that reads the secret key.
func readsSecret(key string) []config.Task {
	return []config.Task{{Show: "{{ .secrets." + key + " }}"}}
}

// A fleet stage or for_each_node loop whose selection resolves before the run
// and selects no nodes, through skip_when_no_nodes_are_selected or the limit,
// or a top-level fleet stage whose when is false before the run on every
// node it selects, needs none of its secrets, and neither do the fleet stages inside such a
// loop or such a fleet stage inside a loop that runs: a vault without them is
// no error. A fleet stage that selects no nodes still writes its
// save_on_controller_machine template, with the secret the template reads.
func TestALeftOutFleetStageNeedsNoSecrets(t *testing.T) {
	vault, store := presentOnlyVault(t)
	for _, tc := range []struct {
		name  string
		limit []string
		// Each of these fleet stages reads a key that the vault lacks.
		leftOut []config.FleetStage
	}{
		{"skip_when_no_nodes_are_selected", nil, []config.FleetStage{
			{ID: "staging", Nodes: "virtual_machines", ExceptNodes: "b", SkipWhenNoNodesAreSelected: true, NodeTasks: readsSecret("staging_password")},
			{ID: "staging_rollout", ForEachNode: "virtual_machines", ExceptNodes: "b", As: "server", SkipWhenNoNodesAreSelected: true, FleetStages: []config.FleetStage{
				{ID: "update", Nodes: "{{ .server }}", NodeTasks: readsSecret("staging_rollout_password")},
			}},
		}},
		{"a when that is false before the run", nil, []config.FleetStage{
			{ID: "relay", Nodes: "a", When: false, NodeTasks: readsSecret("relay_password")},
			{ID: "on_another_node", Nodes: "a", When: map[string]any{"equals": []any{"{{ .node.name }}", "b"}}, NodeTasks: readsSecret("another_node_password")},
		}},
		{"limit", []string{"a"}, []config.FleetStage{
			{ID: "on_c", Nodes: "c", NodeTasks: readsSecret("c_password")},
			{ID: "rollout_of_c", ForEachNode: "c", As: "server", FleetStages: []config.FleetStage{
				{ID: "update", Nodes: "{{ .server }}", NodeTasks: readsSecret("c_rollout_password")},
			}},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			p := config.Project{Vault: vault}
			p.FleetStages = append([]config.FleetStage{
				{ID: "web", Nodes: "a", NodeTasks: readsSecret("present")},
				{ID: "sites", Loop: []any{"one"}, As: "site", Key: "{{ .site }}", FleetStages: []config.FleetStage{
					{ID: "deploy", Nodes: "a", NodeTasks: readsSecret("present")},
					{ID: "staging_site", Nodes: "virtual_machines", ExceptNodes: "b", SkipWhenNoNodesAreSelected: true, NodeTasks: readsSecret("staging_site_password")},
				}},
				{ID: "report", Nodes: "virtual_machines", ExceptNodes: "b", SkipWhenNoNodesAreSelected: true, NodeTasks: readsSecret("staging_report_password"),
					SaveOnControllerMachine: []config.SaveOnControllerMachine{{Template: "report.tmpl", TemplateText: "{{ .secrets.present }}", Path: "report.txt", PermissionsMode: "0600", ProjectRoot: root}}},
			}, tc.leftOut...)
			c, r := playbookRun(t, p, tc.limit, false)
			c.Secrets = store
			var calls []string
			var mu sync.Mutex
			c.executeNode = func(ctx context.Context, r *Run, s config.FleetStage, en Entry, _ bool, _ func(Entry) error) (Entry, error) {
				mu.Lock()
				calls = append(calls, s.ID+"/"+en.Node)
				mu.Unlock()
				return runOnNode(ctx, r, s, en)
			}
			execute(t, c, r, false)
			sort.Strings(calls)
			if want := []string{"sites[one]/deploy/a", "web/a"}; r.Status != "succeeded" || !reflect.DeepEqual(calls, want) {
				t.Fatal(r.Status, calls)
			}
			if text, err := os.ReadFile(filepath.Join(root, "report.txt")); err != nil || string(text) != "present-value" {
				t.Fatalf("the template of a fleet stage that selects no nodes holds %q: %v", text, err)
			}
		})
	}
}

// A fleet stage that may select nodes needs its secrets, and a missing one
// fails the run before any node job, with the vault's message: a fleet stage
// that selects nodes, one whose when is true on a node or is decided only
// during the run, one whose selection resolves during the run from outputs, the template of a fleet stage that selects no nodes, and, inside a
// for_each_node iteration the limit keeps, a fleet stage on a helper node
// outside the limit.
func TestAFleetStageThatMaySelectNodesNeedsItsSecrets(t *testing.T) {
	vault, store := presentOnlyVault(t)
	for _, tc := range []struct {
		name       string
		limit      []string
		fleetStage config.FleetStage
	}{
		{"selects nodes", nil, config.FleetStage{ID: "web", Nodes: "a", NodeTasks: readsSecret("missing_password")}},
		{"a when that is true on a node", nil, config.FleetStage{ID: "web", Nodes: "a", When: map[string]any{"equals": []any{"{{ .node.name }}", "a"}}, NodeTasks: readsSecret("missing_password")}},
		{"a when decided during the run", nil, config.FleetStage{ID: "web", Nodes: "a", When: map[string]any{"equals": []any{"{{ .outputs.gate.a.none }}", "x"}}, NodeTasks: readsSecret("missing_password")}},
		{"resolves during the run", nil, config.FleetStage{ID: "resolved_empty", Nodes: "{{ .outputs.gate.a.none }}", SkipWhenNoNodesAreSelected: true, NodeTasks: readsSecret("missing_password")}},
		{"template", nil, config.FleetStage{ID: "staging", Nodes: "virtual_machines", ExceptNodes: "b", SkipWhenNoNodesAreSelected: true,
			SaveOnControllerMachine: []config.SaveOnControllerMachine{{Template: "report.tmpl", TemplateText: "{{ .secrets.missing_password }}", Path: "report.txt", PermissionsMode: "0600"}}}},
		{"helper node", []string{"a"}, config.FleetStage{ID: "rollout", ForEachNode: "all", As: "server", FleetStages: []config.FleetStage{
			{ID: "take_out_of_load_balancer", Nodes: "c", NodeTasks: readsSecret("missing_password")},
		}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			for i := range tc.fleetStage.SaveOnControllerMachine {
				tc.fleetStage.SaveOnControllerMachine[i].ProjectRoot = root
			}
			p := config.Project{Vault: vault, FleetStages: []config.FleetStage{{ID: "gate", Nodes: "a", Outputs: map[string]any{"none": []any{}}}, tc.fleetStage}}
			c, r := playbookRun(t, p, tc.limit, false)
			c.Secrets = store
			c.executeNode = func(context.Context, *Run, config.FleetStage, Entry, bool, func(Entry) error) (Entry, error) {
				t.Error("a node job started without a secret it needs")
				return Entry{}, nil
			}
			if err := c.Execute(context.Background(), r, false); err == nil || err.Error() != "vault is missing required secret missing_password" || r.Status != "failed" {
				t.Fatal(err, r.Status)
			}
		})
	}
}

// After a run or resume of a playbook, each node on which every fleet stage
// that selected it succeeded, or was skipped by its when, records the run with
// its inventory, limit, and fleet stage selection. A failed node keeps its
// earlier record until a resume succeeds there; a record that cannot be
// written is housekeeping. A dryrun records nothing.
func TestLastSuccessfulRunIsRecordedWhereEveryFleetStageSucceeded(t *testing.T) {
	project := config.Project{FleetStages: []config.FleetStage{
		{ID: "setup", Nodes: "all"},
		{ID: "deploy", Nodes: []any{"a", "b"}},
		{ID: "optional", Nodes: "c", When: false},
	}}
	c, r := playbookRun(t, project, nil, false)
	rec := &recorder{records: map[string]protocol.LastSuccessfulRunRequest{}, refuse: "c"}
	rec.attach(&c)
	fail := true
	localNodes(&c, func(s config.FleetStage, en Entry) *protocol.Response {
		if s.ID == "deploy" && en.Node == "b" && fail {
			return &protocol.Response{Status: "failed", Error: "deploy failed"}
		}
		return nil
	})
	execute(t, c, r, false)
	if r.Status != "failed" || !reflect.DeepEqual(rec.nodes(), []string{"a"}) {
		t.Fatal(r.Status, rec.nodes())
	}
	want := protocol.LastSuccessfulRunRequest{Project: "fleet", Playbook: "site", Record: protocol.LastSuccessfulRun{RunID: r.ID, Inventory: "production", FinishedAt: r.Finished.Truncate(time.Second)}}
	if got := rec.records["a"]; !reflect.DeepEqual(got, want) {
		t.Fatalf("%+v\n%+v", got, want)
	}
	loaded, err := c.Load(r.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if got := entry(t, loaded, "optional", "c").Housekeeping; len(got) != 1 || got[0] != "recording the last successful run on the node: node unreachable" {
		t.Fatal(got)
	}
	fail, rec.refuse, rec.records = false, "", map[string]protocol.LastSuccessfulRunRequest{}
	execute(t, c, loaded, true)
	if loaded.Status != "succeeded" || !reflect.DeepEqual(rec.nodes(), []string{"a", "b", "c"}) || rec.records["b"].Record.RunID != r.ID {
		t.Fatal(loaded.Status, rec.nodes())
	}

	// The record names a run's limit and fleet stage selection.
	project.OnlyFleetStages = []string{"setup", "deploy"}
	c, r = playbookRun(t, project, []string{"a", "c"}, false)
	rec = &recorder{records: map[string]protocol.LastSuccessfulRunRequest{}}
	rec.attach(&c)
	localNodes(&c, nil)
	execute(t, c, r, false)
	if got := rec.records["a"].Record; !reflect.DeepEqual(rec.nodes(), []string{"a", "c"}) || got.Limit != "a,c" || got.OnlyFleetStages != "setup,deploy" {
		t.Fatal(rec.nodes(), got)
	}

	c, r = playbookRun(t, project, nil, true)
	rec = &recorder{records: map[string]protocol.LastSuccessfulRunRequest{}}
	rec.attach(&c)
	localNodes(&c, nil)
	execute(t, c, r, false)
	if r.Status != "succeeded" || len(rec.records) != 0 {
		t.Fatal(r.Status, rec.nodes())
	}
}

// A run that stopped before it resolved a selection records nothing, since
// that fleet stage may have selected any node; its resume does. A run-command
// run never records.
func TestLastSuccessfulRunNeedsEverySelectionResolved(t *testing.T) {
	c, r := playbookRun(t, config.Project{FleetStages: []config.FleetStage{
		{ID: "probe", Nodes: []any{"a", "b"}, MaxNodesAtOnce: "1"},
		{ID: "follow", Loop: "{{ .fleet_stages.probe }}", As: "h", Key: "{{ .h.key }}", FleetStages: []config.FleetStage{{ID: "act", Nodes: "{{ .h.key }}"}}},
	}}, nil, false)
	rec := &recorder{records: map[string]protocol.LastSuccessfulRunRequest{}}
	rec.attach(&c)
	first := true
	localNodes(&c, func(s config.FleetStage, en Entry) *protocol.Response {
		if first && s.ID == "probe" && en.Node == "a" {
			if err := c.requestStop(r.ID); err != nil {
				t.Error(err)
			}
		}
		return nil
	})
	execute(t, c, r, false)
	if entry(t, r, "probe", "a").Response.Status != "succeeded" || r.Selections["follow"].Resolved || len(rec.records) != 0 {
		t.Fatal(r.Selections["follow"], rec.nodes())
	}
	loaded, err := c.Load(r.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	first = false
	execute(t, c, loaded, true)
	if loaded.Status != "succeeded" || !reflect.DeepEqual(rec.nodes(), []string{"a", "b"}) {
		t.Fatal(loaded.Status, rec.nodes())
	}

	c, r = playbookRun(t, config.Project{FleetStages: []config.FleetStage{{ID: "run-command", Nodes: "all"}}}, nil, false)
	r.EntryPoint = RunCommandEntryPoint
	rec = &recorder{records: map[string]protocol.LastSuccessfulRunRequest{}}
	rec.attach(&c)
	localNodes(&c, nil)
	execute(t, c, r, false)
	if r.Status != "succeeded" || len(rec.records) != 0 {
		t.Fatal(r.Status, rec.nodes())
	}
}

// A for_each_node iteration can fail although all its node jobs succeeded, as
// when the loop's own outputs fail. Neither its loop node nor its helper node
// records the run then; the loop node of a successful iteration does.
func TestLastSuccessfulRunNeedsEveryIterationANodeTookPartIn(t *testing.T) {
	c, r := playbookRun(t, config.Project{FleetStages: []config.FleetStage{
		{ID: "roll", ForEachNode: []any{"a", "b"}, As: "app", Outputs: map[string]any{"name": "{{ .vars.names[.app] }}"}, FleetStages: []config.FleetStage{
			{ID: "drain", Nodes: "c"}, {ID: "update", Nodes: "{{ .app }}"},
		}},
	}}, nil, false)
	r.Project.Vars["names"] = map[string]any{"a": "first"} // b's loop output cannot be evaluated
	if err := c.createRun(r); err != nil {
		t.Fatal(err)
	}
	rec := &recorder{records: map[string]protocol.LastSuccessfulRunRequest{}}
	rec.attach(&c)
	localNodes(&c, nil)
	execute(t, c, r, false)
	for _, en := range r.Entries {
		if en.Response.Status != "succeeded" {
			t.Fatalf("%s/%s: %+v", en.FleetStage, en.Node, en.Response)
		}
	}
	if r.Status != "failed" || r.Selections["roll"].Iterations["b"].Status != "failed" || !reflect.DeepEqual(rec.nodes(), []string{"a"}) {
		t.Fatal(r.Status, r.Selections["roll"].Iterations, rec.nodes())
	}
}

// With skip_when_no_nodes_are_selected, a fleet stage or for_each_node loop
// whose selection is empty, known before the run or resolved from outputs
// during it, is not selected: it gets no node job, a requirement on it is
// met, its whole output collection is an empty mapping, and reading one
// named node's output from it fails as for any node it does not select.
// Without the setting, a selection that resolves empty during the run fails.
// A fleet stage that selects nodes ignores the setting.
func TestAFleetStageThatSelectsNoNodesIsSkippedByTheRun(t *testing.T) {
	c, r := playbookRun(t, config.Project{FleetStages: []config.FleetStage{
		{ID: "empty_group", Nodes: "virtual_machines", ExceptNodes: "b", SkipWhenNoNodesAreSelected: true, Outputs: map[string]any{"port": 80}},
		{ID: "gate", Nodes: "a", Outputs: map[string]any{"none": []any{}}},
		{ID: "resolved_empty", Nodes: "{{ .outputs.gate.a.none }}", SkipWhenNoNodesAreSelected: true, Outputs: map[string]any{"port": 81}},
		{ID: "roll", ForEachNode: "{{ .outputs.gate.a.none }}", ExceptNodes: "c", As: "server", SkipWhenNoNodesAreSelected: true, FleetStages: []config.FleetStage{
			{ID: "update", Nodes: "{{ .server }}", NodeTasks: []config.Task{{Show: "hi"}}},
		}},
		{ID: "selects", Nodes: "a", SkipWhenNoNodesAreSelected: true, Requires: config.Requires{AllNodes: []string{"empty_group", "resolved_empty", "roll"}},
			NodeTasks: []config.Task{{Show: "{{ .outputs.empty_group }}"}, {Show: "{{ .outputs.resolved_empty }}"}, {Show: "{{ .outputs.roll }}"}}},
		{ID: "named_node", Nodes: "c", NodeTasks: []config.Task{{Show: "{{ .outputs.resolved_empty.a.port }}"}}},
		{ID: "not_skipped", Nodes: "{{ .outputs.gate.a.none }}", NodeTasks: []config.Task{{Show: "hi"}}},
	}}, nil, false)
	var calls []string
	var mu sync.Mutex
	localNodes(&c, func(s config.FleetStage, en Entry) *protocol.Response {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, s.ID+"/"+en.Node)
		return nil
	})
	execute(t, c, r, false)
	sort.Strings(calls)
	if want := []string{"gate/a", "named_node/c", "selects/a"}; !reflect.DeepEqual(calls, want) {
		t.Fatal(calls)
	}
	for _, path := range []string{"empty_group", "resolved_empty", "roll"} {
		if sel := r.Selections[path]; sel == nil || !sel.Resolved || !sel.LeftOutBecauseItSelectsNoNodes || len(sel.Nodes)+len(sel.Items)+len(sel.LeftOut) != 0 || sel.Response.Status != "" {
			t.Fatalf("%s: %+v", path, sel)
		}
	}
	if sel := r.Selections["selects"]; sel.LeftOutBecauseItSelectsNoNodes || !reflect.DeepEqual(sel.Nodes, []string{"a"}) {
		t.Fatalf("%+v", sel)
	}
	selects := entry(t, r, "selects", "a").Response
	if selects.Status != "succeeded" || len(selects.Results) != 3 {
		t.Fatal(selects)
	}
	for _, result := range selects.Results {
		if outputs, ok := result.Value.(map[string]any); !ok || len(outputs) != 0 {
			t.Fatalf("a whole output collection is %#v", result.Value)
		}
	}
	if named := entry(t, r, "named_node", "c").Response; named.Status == "succeeded" || !strings.Contains(named.Error, "resolved_empty") {
		t.Fatal(named)
	}
	if sel := r.Selections["not_skipped"]; sel.Resolved || sel.Response.Status != "failed" || !strings.Contains(sel.Response.Error, "node selection is empty") {
		t.Fatalf("%+v", sel)
	}
	// The record says why the fleet stages got no node job.
	loaded, err := c.Load(r.ID, false)
	if err != nil || !loaded.Selections["empty_group"].LeftOutBecauseItSelectsNoNodes || !loaded.Selections["roll"].LeftOutBecauseItSelectsNoNodes {
		t.Fatal(err)
	}
}
