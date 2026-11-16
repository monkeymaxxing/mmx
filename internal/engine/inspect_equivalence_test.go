package engine

import (
	"context"
	"embed"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mmx/internal/config"
	"mmx/internal/facts"
	"mmx/internal/protocol"
	"mmx/internal/value"
)

// Validation and explain walk the definitions with node values unavailable;
// the engine walks them on the node. These fixtures run both for every node
// and check that inspection never claims what execution does not do.
var equivalenceFacts = map[string]map[string]any{
	"web1": {"hostname": "web1", "cpu_count": 2, "addresses": []any{"10.0.0.1"}},
	"web2": {"hostname": "web2", "cpu_count": 4, "addresses": []any{"10.0.0.2", "10.0.0.3"}},
}

// stepScope is the controller's outer scope before any fleet stage runs.
func fleetStageScope(p config.Project, inv config.Inventory) value.Scope {
	secrets := map[string]any{}
	for name := range p.Secrets {
		secrets[name] = &value.Secret{Name: name, Data: "equivalence-" + name, Available: true}
	}
	return value.Scope{"secrets": secrets, "vars": config.NodeVars(p, inv, ""), "outputs": map[string]any{}, "fleet_stages": map[string]any{}, "inventory": config.InventoryValues(inv)}
}

// dryrunJob builds the job as the controller's hostJob does and runs it in
// dryrun with fixed facts.
func dryrunJob(t *testing.T, p config.Project, inv config.Inventory, managedNode, path string, fleetStage config.FleetStage, scope value.Scope) protocol.Response {
	t.Helper()
	data := scope.Data()
	delete(data, "vars")
	j := config.Job{ID: "equivalence", Project: p.Project, FleetStage: path, Node: managedNode, Dryrun: true, Vars: config.NodeVars(p, inv, managedNode), Origins: config.VariableOrigins(p, inv, managedNode), NodeTasks: fleetStage.NodeTasks, Outputs: fleetStage.Outputs, Sources: fleetStage.Sources, Scope: data, Unavailable: scope.Missing()}
	e := Engine{RuntimeRoot: t.TempDir(), collectFacts: func(string) facts.Snapshot { return facts.Snapshot{Values: equivalenceFacts[managedNode]} }}
	return e.Run(context.Background(), j)
}

// dryrunHost walks the fleet stages as the controller does and indexes every result
// by its full path. As in the controller process, a fleet stage's when is
// checked for the node before its job; a fleet stage it skips is recorded as a
// result skipped by when.
func dryrunNode(t *testing.T, p config.Project, inv config.Inventory, managedNode string) map[string]protocol.Result {
	t.Helper()
	results := map[string]protocol.Result{}
	var walk func([]config.FleetStage, value.Scope, string)
	walk = func(fleetStages []config.FleetStage, scope value.Scope, prefix string) {
		for _, fleetStage := range fleetStages {
			path := prefix + fleetStage.ID
			if fleetStage.Loop != nil {
				items, err := scope.Iterations(fleetStage.Loop, fleetStage.As, fleetStage.Key)
				if err != nil {
					t.Fatal(path, err)
				}
				for _, item := range items {
					local := scope.Local()
					local[fleetStage.As] = item.Value
					if ok, err := local.Predicate(fleetStage.When); err != nil || !ok {
						continue
					}
					walk(fleetStage.FleetStages, local, path+"["+value.EscapeKey(item.Key)+"]/")
				}
				continue
			}
			requested, err := scope.Resolve(fleetStage.Nodes)
			if err != nil {
				t.Fatal(path, err)
			}
			nodes, err := config.Select(inv, requested)
			if err != nil {
				t.Fatal(path, err)
			}
			if !contains(nodes, managedNode) {
				continue
			}
			condition := scope.Local()
			condition["vars"] = config.NodeVars(p, inv, managedNode)
			condition["node"] = map[string]any{"name": managedNode}
			if ok, err := condition.Predicate(fleetStage.When); err != nil {
				t.Fatal(path, err)
			} else if !ok {
				results[path] = protocol.Result{Status: "skipped", Reason: "when is false"}
				continue
			}
			r := dryrunJob(t, p, inv, managedNode, path, fleetStage, scope)
			switch r.Status {
			case "succeeded":
			default:
				t.Fatalf("%s on %s: %s: %s", path, managedNode, r.Status, r.Error)
			}
			for _, result := range r.Results {
				results[path+"/"+result.Task] = result
			}
		}
	}
	walk(p.FleetStages, fleetStageScope(p, inv), "")
	return results
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// skippedByWhen reports whether the task or an enclosing task or fleet stage was
// skipped because its condition was false. A secret result hides its reason;
// these fixtures have no other kind of skip.
func skippedByWhen(results map[string]protocol.Result, path string) bool {
	for {
		if r, ok := results[path]; ok && r.Status == "skipped" && (r.Reason == "when is false" || r.Secret) {
			return true
		}
		i := strings.LastIndex(path, "/")
		if i < 0 {
			return false
		}
		path = path[:i]
	}
}

// A runtime argument must wait for a node value or for an assignment or role
// input that does; a project or inventory variable is known locally.
func runtimeSource(a config.Argument) bool {
	for _, ref := range config.References(a.Reference) {
		parts, err := value.Path(ref)
		if err == nil && parts[0].Name != "vars" && parts[0].Name != "inputs" {
			return true
		}
	}
	for _, at := range a.Origins {
		if strings.Contains(at.Field, "set_vars.") || strings.Contains(at.Field, "inputs.") {
			return true
		}
	}
	return false
}

func jsonText(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// Embedded so the native Linux test executables carry their fixtures.
//
//go:embed testdata/equivalence
var equivalenceFixtures embed.FS

func TestInspectionAgreesWithDryrun(t *testing.T) {
	root := t.TempDir()
	tree, err := fs.Sub(equivalenceFixtures, "testdata/equivalence")
	if err == nil {
		err = os.CopyFS(root, tree)
	}
	if err != nil {
		t.Fatal(err)
	}
	fixtures, err := filepath.Glob(filepath.Join(root, "*/mmx_project_settings.yml"))
	if err != nil || len(fixtures) < 4 {
		t.Fatal(fixtures, err)
	}
	for _, fixture := range fixtures {
		t.Run(filepath.Base(filepath.Dir(fixture)), func(t *testing.T) {
			p, inv, err := config.Load(fixture, "example", "", nil)
			if err != nil {
				t.Fatal(err)
			}
			for managedNode := range inv.Nodes {
				results := dryrunNode(t, p, inv, managedNode)
				view, err := config.Inspect(p, inv, []string{managedNode})
				if err != nil {
					t.Fatalf("%s: dryrun succeeded but inspection failed: %v", managedNode, err)
				}
				ready := 0
				for _, fleetStage := range view.FleetStages {
					for _, task := range fleetStage.NodeTasks {
						r, ran := results[task.Path]
						skipped := skippedByWhen(results, task.Path)
						switch task.Status {
						case "skipped":
							if !skipped {
								t.Errorf("%s %s: inspection skipped, engine %+v", managedNode, task.Path, r)
							}
						case "ready":
							ready++
							if !ran || r.Status != "succeeded" {
								t.Errorf("%s %s: inspection ready, engine ran=%v %+v", managedNode, task.Path, ran, r)
								continue
							}
						}
						for _, a := range task.Arguments {
							switch {
							case a.Status == "runtime" && !runtimeSource(a):
								t.Errorf("%s %s: %s waits for execution but reads only local values: %+v", managedNode, task.Path, a.Field, a)
							// A role given a secret input keeps its tasks' results
							// secret, so the engine redacts values explain can show.
							case task.Status == "ready" && a.Field == "show" && a.Status == "resolved" && !r.Secret:
								if got, want := jsonText(t, value.Display(r.Value)), jsonText(t, a.Value); got != want {
									t.Errorf("%s %s: explain shows %s, engine shows %s", managedNode, task.Path, want, got)
								}
							}
						}
					}
				}
				if ready == 0 {
					t.Fatalf("%s: fixture has no ready node_tasks", managedNode)
				}
			}
		})
	}
}

// A value known only on the node fails with its field and, when it came from
// elsewhere, the location that supplied it, as validation reports local values.
func TestRuntimeErrorsNameTheAssignment(t *testing.T) {
	assign := "  node_tasks:\n  - set_vars: {dest: '{{ .node.hostname }}', list: '{{ .node.addresses }}'}\n"
	for _, tc := range []struct {
		fleetStage string
		want       []string
	}{
		{assign + "  - file: {permissions_mode: '0644', path: '{{ .vars.dest }}', content: x}\n", []string{"node_tasks[1].file.path", "path must be absolute", "value supplied at", "node_tasks[0].set_vars.dest"}},
		{assign + "  - {show: x, when: '{{ .vars.dest }}'}\n", []string{"playbooks/example.yml:8:", "node_tasks[1].when", "value supplied at", "node_tasks[0].set_vars.dest"}},
		{assign + "  - {show: x, loop: '{{ .vars.dest }}', as: item}\n", []string{"playbooks/example.yml:8:", "node_tasks[1].loop", "value supplied at", "node_tasks[0].set_vars.dest"}},
		{assign + "  - {show: x, retry: {until: '{{ .vars.dest }}'}}\n", []string{"node_tasks[1].retry.until", "value supplied at", "node_tasks[0].set_vars.dest"}},
		{assign + "  outputs: {text: 'x{{ .vars.list }}'}\n", []string{"fleet_stages[0].outputs.text", "expected a string, boolean, number, or binary content", "value supplied at", "node_tasks[0].set_vars.list"}},
		{"  node_tasks: [{role: app}]\n", []string{"roles/app/main.yml:2:", "outputs.text", "expected a string, boolean, number, or binary content"}},
	} {
		dir := t.TempDir()
		for name, text := range map[string]string{
			"inventories/example.yml":  "nodes: {web1: {address: web1.invalid}}\n",
			"roles/app/main.yml":       "node_tasks: [{show: x}]\noutputs: {text: 'x{{ .node.addresses }}'}\n",
			"mmx_project_settings.yml": "project_name: origins\ndefault_inventory: example\n",
			"playbooks/example.yml":    "\n\nfleet_stages:\n- id: setup\n  nodes: all\n" + tc.fleetStage,
		} {
			if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0600); err != nil {
				t.Fatal(err)
			}
		}
		p, inv, err := config.Load(filepath.Join(dir, "mmx_project_settings.yml"), "example", "", nil)
		if err != nil {
			t.Fatal(err)
		}
		r := dryrunJob(t, p, inv, "web1", "setup", p.FleetStages[0], fleetStageScope(p, inv))
		for _, want := range tc.want {
			if r.Status != "failed" || !strings.Contains(r.Error, want) {
				t.Fatalf("want %s: %s %s", want, r.Status, r.Error)
			}
		}
	}
}

// A service task block fixes its tracked arguments before its stages run; a bad
// value fails there with the same location as the same task outside one.
func TestServiceTaskBlockErrorsNameTheirField(t *testing.T) {
	file := "{file: {permissions_mode: '0644', path: '{{ .vars.dest }}', content: x}}"
	for _, tc := range []struct{ task, field string }{
		{file, "node_tasks[1].file.path"},
		{"{task_block: {id: app , changes: [" + (file) + "], activate: [{systemd_service: {name: app, state: running}}]}}", "node_tasks[1].task_block.changes[0].file.path"},
	} {
		dir := t.TempDir()
		for name, text := range map[string]string{
			"inventories/example.yml":  "nodes: {web1: {address: web1.invalid}}\n",
			"mmx_project_settings.yml": "project_name: origins\ndefault_inventory: example\n",
			"playbooks/example.yml":    "\n\nfleet_stages:\n- id: setup\n  nodes: all\n  node_tasks:\n  - set_vars: {dest: '{{ .node.hostname }}'}\n  - " + tc.task + "\n",
		} {
			if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0600); err != nil {
				t.Fatal(err)
			}
		}
		p, inv, err := config.Load(filepath.Join(dir, "mmx_project_settings.yml"), "example", "", nil)
		if err != nil {
			t.Fatal(err)
		}
		r := dryrunJob(t, p, inv, "web1", "setup", p.FleetStages[0], fleetStageScope(p, inv))
		for _, want := range []string{"playbooks/example.yml:8:", tc.field, "path must be absolute", "value supplied at", "node_tasks[0].set_vars.dest"} {
			if r.Status != "failed" || !strings.Contains(r.Error, want) {
				t.Fatalf("want %s: %s %s", want, r.Status, r.Error)
			}
		}
	}
}
