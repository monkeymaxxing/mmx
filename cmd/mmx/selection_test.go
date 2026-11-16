package main

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// mmx nodes and explain show except_nodes and the fleet stages
// --only-fleet-stages leaves out, with the same rejections run would print;
// resume and the commands that do not select fleet stages reject the option.
func TestNodesAndExplainShowExceptNodesAndOnlyFleetStages(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	writeCLIProject(t, dir, "inventories/example.yml", "nodes:\n  web01: {address: web01.example.net}\n  db01: {address: db01.example.net}\n  build01: {address: build01.example.net}\ngroups:\n  virtual_machines: {nodes: [build01]}\n")
	writeCLIProject(t, dir, "mmx_project_settings.yml", "project_name: test\ndefault_inventory: example\nfleet_stages:\n- {id: configure_network_interfaces, nodes: all, except_nodes: [virtual_machines], node_tasks: [{show: hi}]}\n- {id: ruby, nodes: web01, node_tasks: [{show: hi}]}\n- {id: sites, nodes: web01, requires: {same_node: [ruby]}, node_tasks: [{show: hi}]}\n")
	command := func(args ...string) (string, int, error) {
		t.Helper()
		stdout, err := os.CreateTemp(dir, "stdout")
		if err != nil {
			t.Fatal(err)
		}
		saved := os.Stdout
		os.Stdout = stdout
		code, err := run(context.Background(), args)
		os.Stdout = saved
		stdout.Close()
		b, _ := os.ReadFile(stdout.Name())
		return string(b), code, err
	}
	out, code, err := command("nodes", "example", "--only-fleet-stages", "configure_network_interfaces")
	if code != 0 || err != nil {
		t.Fatal(code, err)
	}
	for _, want := range []string{"Only fleet stages: configure_network_interfaces", "configure_network_interfaces   all except virtual_machines   → db01, web01", "ruby                           web01                         → none; left out by --only-fleet-stages"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in\n%s", want, out)
		}
	}
	out, code, err = command("nodes", "example", "--only-fleet-stages", "ruby,sites", "--json")
	var view nodesView
	if code != 0 || err != nil || json.Unmarshal([]byte(out), &view) != nil {
		t.Fatal(code, err, out)
	}
	// "inventory" is the inventory's name, as in a run's results, and
	// "inventory_file" is its file.
	if !strings.Contains(out, `"inventory": "example"`) || !strings.Contains(out, `/inventories/example.yml"`) || !strings.HasSuffix(view.InventoryFile, "/inventories/example.yml") || strings.Contains(out, "inventory_name") {
		t.Fatal(out)
	}
	if first := view.FleetStages[0]; !first.LeftOutByOnlyFleetStages || first.ExceptNodes == nil || strings.Join(view.OnlyFleetStages, ",") != "ruby,sites" || strings.Join(view.FleetStages[2].Nodes, ",") != "web01" {
		t.Fatal(out)
	}
	if out, code, err = command("explain", "example", "--only-fleet-stages", "ruby"); code != 0 || err != nil || !strings.Contains(out, "Only fleet stages ruby") || !strings.Contains(out, "sites → not selected; left out by --only-fleet-stages") {
		t.Fatal(code, err, out)
	}
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"explain", "example", "--only-fleet-stages", "sites"}, "--only-fleet-stages sites leaves out fleet stage ruby, which fleet stage sites requires; use --only-fleet-stages ruby,sites"},
		{[]string{"nodes", "example", "--only-fleet-stages", "nope"}, `the playbook has no fleet stage "nope"; its fleet stages are configure_network_interfaces, ruby, sites`},
		{[]string{"dryrun", "example", "--only-fleet-stages", "sites"}, "use --only-fleet-stages ruby,sites"},
		{[]string{"nodes", "--only-fleet-stages", "ruby"}, "--only-fleet-stages requires a playbook name"},
		{[]string{"resume", "--only-fleet-stages", "ruby"}, "resume repeats the recorded run's fleet stages and accepts no --only-fleet-stages; start a new mmx run <playbook> --only-fleet-stages"},
		{[]string{"resume", "--limit", "web01"}, "does not accept --limit; start a new mmx run <playbook> --limit"},
		{[]string{"validate", "example", "--only-fleet-stages", "ruby"}, "validate does not accept --only-fleet-stages"},
		{[]string{"run-command", "--nodes", "all", "--only-fleet-stages", "ruby", "--", "true"}, "run-command does not accept --only-fleet-stages"},
		// --node, --fleet-stage, and --task filter only what mmx results
		// shows; every other command would act on its whole selection.
		{[]string{"run", "example", "--node", "web01"}, "run does not accept --node; narrow the selected nodes with --limit"},
		{[]string{"dryrun", "example", "--fleet-stage", "ruby"}, "dryrun does not accept --fleet-stage; keep only some fleet stages with --only-fleet-stages"},
		{[]string{"explain", "example", "--node", "web01"}, "explain does not accept --node; narrow the selected nodes with --limit"},
		{[]string{"nodes", "example", "--fleet-stage", "ruby"}, "nodes does not accept --fleet-stage; keep only some fleet stages with --only-fleet-stages"},
		{[]string{"run", "example", "--task", "check"}, "run does not accept --task; use mmx results to filter the results of a saved run"},
		{[]string{"resume", "--node", "web01"}, "resume does not accept --node; resume uses the recorded run's node selection; start a new mmx run <playbook> --limit NODE_OR_GROUP[,...] to select different nodes"},
		{[]string{"resume", "--fleet-stage", "ruby"}, "resume does not accept --fleet-stage; resume repeats the recorded run's fleet stages; start a new mmx run <playbook> --only-fleet-stages for other fleet stages"},
		{[]string{"stop", "--node", "web01"}, "stop does not accept --node; use mmx results to filter the results of a saved run"},
		{[]string{"status", "--task", "check"}, "status does not accept --task; use mmx results to filter the results of a saved run"},
		{[]string{"validate", "--fleet-stage", "ruby"}, "validate does not accept --fleet-stage; use mmx results to filter the results of a saved run"},
		{[]string{"run-command", "--nodes", "all", "--node", "web01", "--", "true"}, "run-command does not accept --node; choose the nodes with --nodes"},
	} {
		if _, code, err := command(tc.args...); code != 4 || err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%v: want %q, got %d %v", tc.args, tc.want, code, err)
		}
	}
}

// A fleet stage or for_each_node loop that selects no nodes, which
// skip_when_no_nodes_are_selected allows, is reported as not selected by
// nodes, explain, and a run, with the reason, and gets no node job; a fleet
// stage beside it that selects nodes is unaffected.
func TestNodesExplainAndRunReportAFleetStageThatSelectsNoNodes(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("MMX_RUNNER_DIR", t.TempDir())
	writeCLIProject(t, dir, "inventories/example.yml", "nodes:\n  web01: {address: web01.example.net}\ngroups:\n  web: {nodes: [web01]}\n  staging: {nodes: []}\n")
	writeCLIProject(t, dir, "mmx_project_settings.yml", "project_name: test\ndefault_inventory: example\nfleet_stages:\n- {id: configure_staging, nodes: staging, skip_when_no_nodes_are_selected: true, node_tasks: [{show: hi}]}\n- {id: roll_staging, for_each_node: staging, as: server, skip_when_no_nodes_are_selected: true, fleet_stages: [{id: update, nodes: '{{ .server }}', node_tasks: [{show: hi}]}]}\n- {id: configure_web, nodes: web, skip_when_no_nodes_are_selected: true, node_tasks: [{show: hi}]}\n")
	out, code, err := captureCommand(t, "nodes", "example")
	if code != 0 || err != nil {
		t.Fatal(code, err)
	}
	for _, want := range []string{"configure_staging   staging   → none; selection contains no nodes (skip_when_no_nodes_are_selected)\n", "roll_staging        staging   → none; selection contains no nodes (skip_when_no_nodes_are_selected)\n", "configure_web       web       → web01\n"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in\n%s", want, out)
		}
	}
	out, code, err = captureCommand(t, "nodes", "example", "--json")
	var view nodesView
	if code != 0 || err != nil || json.Unmarshal([]byte(out), &view) != nil || len(view.FleetStages) != 3 {
		t.Fatal(code, err, out)
	}
	if !view.FleetStages[0].LeftOutBecauseItSelectsNoNodes || !view.FleetStages[1].LeftOutBecauseItSelectsNoNodes || view.FleetStages[2].LeftOutBecauseItSelectsNoNodes || !strings.Contains(out, `"left_out_because_it_selects_no_nodes": true`) {
		t.Fatal(out)
	}
	out, code, err = captureCommand(t, "explain", "example")
	if code != 0 || err != nil || !strings.Contains(out, "configure_staging → not selected; selection contains no nodes (skip_when_no_nodes_are_selected)\n") ||
		!strings.Contains(out, "roll_staging → for each node: not selected; selection contains no nodes (skip_when_no_nodes_are_selected)\n") || !strings.Contains(out, "configure_web → web01\n") {
		t.Fatal(code, err, out)
	}
	// A run with only such fleet stages contacts no node and succeeds.
	writeCLIProject(t, dir, "mmx_project_settings.yml", "project_name: test\ndefault_inventory: example\nfleet_stages:\n- {id: configure_staging, nodes: staging, skip_when_no_nodes_are_selected: true, node_tasks: [{show: hi}]}\n- {id: configure_web_staging, nodes: web, except_nodes: web01, skip_when_no_nodes_are_selected: true, node_tasks: [{show: hi}]}\n")
	out, code, err = captureCommand(t, "run", "example")
	if code != 0 || err != nil || !strings.Contains(out, "  configure_staging: not selected — selection contains no nodes (skip_when_no_nodes_are_selected)\n  configure_web_staging: not selected — selection contains no nodes (skip_when_no_nodes_are_selected)\n") || !strings.Contains(out, ": succeeded\n") {
		t.Fatal(code, err, out)
	}
	out, code, err = captureCommand(t, "results", "--json")
	if code != 0 || err != nil || !strings.Contains(out, `"left_out_because_it_selects_no_nodes": true`) {
		t.Fatal(code, err, out)
	}
}
