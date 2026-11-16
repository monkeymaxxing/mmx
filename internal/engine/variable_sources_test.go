package engine

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"mmx/internal/config"
)

// A role call whose when the node answers carries, in each node's job, the
// template its inputs name for that node. The node checks the when and runs
// the role only when it is true, in a dryrun and in a run.
func TestConditionalRoleCallWritesEachNodesTemplate(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "marker")
	files := map[string]string{
		"mmx_project_settings.yml": "project_name: conditional_role\ndefault_inventory: example\n",
		"inventories/example.yml":  "nodes:\n  web1: {address: web1.invalid, vars: {check_file: a.yml}}\n  web2: {address: web2.invalid, vars: {check_file: b.yml}}\n",
		"checks/a.yml":             "check a\n",
		"checks/b.yml":             "check b\n",
		"marker":                   "",
		"roles/r/main.yml": `inputs: {check_file: {type: string, required: true}, out: {type: string, required: true}}
node_tasks:
  - file: {path: "{{ .inputs.out }}/{{ .inputs.check_file }}", template: "../../checks/{{ .inputs.check_file }}", permissions_mode: "0644"}
`,
		"playbooks/example.yml": `fleet_stages:
  - id: setup
    nodes: all
    node_tasks:
      - id: present
        role: r
        when: {path_exists: ` + marker + `}
        inputs: {check_file: "{{ .vars.check_file }}", out: "` + dir + `/{{ .node.name }}/present"}
      - id: absent
        role: r
        when: {path_exists: ` + filepath.Join(dir, "missing") + `}
        inputs: {check_file: "{{ .vars.check_file }}", out: "` + dir + `/{{ .node.name }}/absent"}
`,
	}
	for name, text := range files {
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
	assets := t.TempDir()
	if err := config.FreezeAssets(context.Background(), &p, assets, nil); err != nil {
		t.Fatal(err)
	}
	for node, want := range map[string]string{"web1": "check a\n", "web2": "check b\n"} {
		for _, sub := range []string{"present", "absent"} {
			if err := os.MkdirAll(filepath.Join(dir, node, sub), 0700); err != nil {
				t.Fatal(err)
			}
		}
		tasks, err := config.NodeTasksFor(p.FleetStages[0].NodeTasks, node, assets)
		if err != nil {
			t.Fatal(err)
		}
		j := config.Job{ID: "conditional-role-" + node, Project: p.Project, FleetStage: "setup", Node: node, Vars: config.NodeVars(p, inv, node), NodeTasks: tasks, Scope: map[string]any{"node": map[string]any{"name": node}}}
		e := Engine{RuntimeRoot: filepath.Join(dir, "state", node)}
		for _, dryrun := range []bool{true, false} {
			j.Dryrun = dryrun
			r := e.Run(context.Background(), j)
			requireStatus(t, r, "succeeded")
			if r.Results[0].Status != "succeeded" || r.Results[len(r.Results)-1].Status != "skipped" {
				t.Fatalf("%s dryrun=%v: %+v", node, dryrun, r.Results)
			}
		}
		written := filepath.Join(dir, node, "present", map[string]string{"web1": "a.yml", "web2": "b.yml"}[node])
		if got := read(t, written); got != want {
			t.Fatalf("%s: wrote %q, want %q", node, got, want)
		}
		if entries, _ := os.ReadDir(filepath.Join(dir, node, "absent")); len(entries) != 0 {
			t.Fatalf("%s: the role whose when is false wrote %v", node, entries)
		}
	}
}
