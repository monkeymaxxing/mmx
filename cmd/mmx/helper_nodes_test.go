package main

import (
	"context"
	"os"
	"strings"
	"testing"
)

// Under a limit, nodes and explain show a for_each_node iteration's fleet
// stages on the nodes they name, and list the helper nodes outside the limit
// before any connection. A when marks the selection as narrowed during the run.
func TestNodesAndExplainShowHelperNodesOutsideTheLimit(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	writeCLIProject(t, dir, "inventories/example.yml", "nodes:\n  app01: {address: app01.example.net}\n  app02: {address: app02.example.net}\n  lb01: {address: lb01.example.net}\ngroups:\n  app: {nodes: [app01, app02]}\n")
	writeCLIProject(t, dir, "mmx_project_settings.yml", `project_name: test
default_inventory: example
fleet_stages:
- id: inspect
  nodes: app
  outputs: {current: false}
- id: rollout
  for_each_node: app
  as: app_server
  max_nodes_at_once: "34%"
  when: {not: '{{ .outputs.inspect[.app_server].current }}'}
  fleet_stages:
  - {id: take_out_of_load_balancer, nodes: lb01, node_tasks: [{show: '{{ .app_server }}'}]}
  - {id: update, nodes: '{{ .app_server }}', node_tasks: [{show: update}]}
  - {id: put_back_into_load_balancer, nodes: lb01, node_tasks: [{show: '{{ .app_server }}'}]}
`)
	command := func(args ...string) string {
		t.Helper()
		stdout, err := os.CreateTemp(dir, "stdout")
		if err != nil {
			t.Fatal(err)
		}
		saved := os.Stdout
		os.Stdout = stdout
		code, err := run(context.Background(), append([]string{args[0], "example"}, args[1:]...))
		os.Stdout = saved
		stdout.Close()
		if code != 0 || err != nil {
			t.Fatal(args, code, err)
		}
		b, _ := os.ReadFile(stdout.Name())
		return string(b)
	}
	// Columns are aligned with spaces; compare with single spaces.
	out := strings.Join(strings.Fields(strings.ReplaceAll(command("nodes", "--limit", "app02"), "\n", " | ")), " ")
	for _, want := range []string{
		"| inspect app → app02 |",
		"| rollout[app02]/take_out_of_load_balancer lb01 → lb01; narrowed during the run by `when` |",
		"| rollout[app02]/update app02 → app02; narrowed during the run by `when` |",
		"| | Nodes outside --limit that help selected nodes | lb01 rollout[app02]/take_out_of_load_balancer, rollout[app02]/put_back_into_load_balancer |",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in\n%s", want, out)
		}
	}
	if strings.Contains(out, "| rollout ") || strings.Contains(out, "app01]") {
		t.Fatal("nodes shows the loop through its kept iterations only\n", out)
	}
	out = command("explain", "--limit", "app02")
	for _, want := range []string{"\nrollout → for each node: app02\n", "  max_nodes_at_once: 34% = 1 of 1 node\n", "\nrollout[app02]/take_out_of_load_balancer → lb01\n", "\nNodes outside --limit that help selected nodes\n  lb01"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in\n%s", want, out)
		}
	}
	if out := command("nodes"); strings.Contains(out, "Nodes outside --limit that help selected nodes") || !strings.Contains(out, "rollout[app01]/update") {
		t.Fatal("without a limit there are no helper nodes\n", out)
	}
}
