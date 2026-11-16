package config

import (
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// rollingInventory has three app servers and the load balancer lb, which the
// rolling playbooks below use as a helper node.
const rollingInventory = "nodes:\n  app1: {address: a}\n  app2: {address: b}\n  app3: {address: c}\n  lb: {address: d}\ngroups:\n  app: {nodes: [app1, app2, app3]}\n"

// rollingPlaybook takes each app server out of lb, updates it, and puts it
// back; loopFields are extra fields of the loop.
func rollingPlaybook(loopFields string) string {
	return `fleet_stages:
- id: rollout
  for_each_node: app
  as: app_server
` + loopFields + `  fleet_stages:
  - id: drain
    nodes: lb
    node_tasks: [{show: '{{ .app_server }}'}]
  - id: update
    nodes: '{{ .app_server }}'
    node_tasks: [{show: update}]
    outputs: {healthy: true}
  - id: rejoin
    nodes: lb
    node_tasks: [{show: '{{ .app_server }}'}]
  outputs: {healthy: '{{ .outputs.update[.app_server].healthy }}'}
`
}

func loadRolling(t *testing.T, playbook string) (Project, Inventory, error) {
	t.Helper()
	path := fixture(t, "")
	write(t, filepath.Join(filepath.Dir(path), "inventories/example.yml"), rollingInventory)
	write(t, path, "project_name: test\ndefault_inventory: example\n"+playbook)
	return Load(path, "example", "", nil)
}

func TestForEachNodeValidation(t *testing.T) {
	p, _, err := loadRolling(t, rollingPlaybook("  max_nodes_at_once: \"34%\"\n  on_node_failure: stop_run\n  failed_nodes_allowed: 1\n")+`- id: report
  nodes: lb
  node_tasks: [{show: '{{ .outputs.rollout.app2.healthy }}'}]
`)
	if err != nil {
		t.Fatal(err)
	}
	if loop := p.FleetStages[0]; loop.MaxNodesAtOnce != "34%" || loop.OnNodeFailure != "stop_run" || loop.FailedNodesAllowed != "1" {
		t.Fatalf("%+v", loop)
	}
	for _, tc := range []struct{ playbook, want string }{
		{rollingPlaybook("  key: '{{ .app_server }}'\n"), "for_each_node loop rollout does not accept loop, key, nodes, node_tasks, or requires"},
		{rollingPlaybook("  nodes: lb\n"), "does not accept loop, key, nodes"},
		{strings.Replace(rollingPlaybook(""), "  as: app_server\n", "", 1), "for_each_node loop rollout needs as: the name of the variable that holds the current node's ID"},
		{strings.Replace(rollingPlaybook(""), "for_each_node: app", "for_each_node: [app, nope]", 1), `(fleet_stages[0].for_each_node): no node or inventory group named "nope"`},
		{rollingPlaybook("  max_nodes_at_once: 0\n"), "max_nodes_at_once must be a positive count or a whole-number percentage"},
		{rollingPlaybook("  max_nodes_at_once: \"0%\"\n"), "max_nodes_at_once must be a positive count"},
		{rollingPlaybook("  max_nodes_at_once: \"101%\"\n"), "from 1% to 100%"},
		{rollingPlaybook("  failed_nodes_allowed: 1\n"), "for_each_node loop rollout: failed_nodes_allowed requires on_node_failure: stop_fleet_stage or on_node_failure: stop_run"},
		{strings.Replace(rollingPlaybook(""), "    nodes: lb\n    node_tasks: [{show: '{{ .app_server }}'}]\n  - id: update", "    nodes: lb\n    on_node_failure: stop_run\n    node_tasks: [{show: '{{ .app_server }}'}]\n  - id: update", 1), "fleet stage drain is inside for_each_node loop rollout, which counts failures by the loop's nodes; write on_node_failure on the loop"},
		{strings.Replace(rollingPlaybook(""), "    outputs: {healthy: true}\n", "    outputs: {healthy: true}\n    failed_nodes_allowed: 1\n", 1), "write failed_nodes_allowed on the loop"},
		{rollingPlaybook("") + "- id: report\n  nodes: lb\n  node_tasks: [{show: '{{ .outputs.rollout.lb.healthy }}'}]\n", "reference .outputs.rollout.lb.healthy names a node that the referenced fleet stage does not select"},
		{rollingPlaybook("  when: '{{ .inputs.arch }}'\n"), "unknown reference root .inputs"},
	} {
		if _, _, err := loadRolling(t, tc.playbook); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("want %q, got %v", tc.want, err)
		}
	}
}

// A fleet stage's when, and a for_each_node loop's, may read the node's name,
// earlier outputs, and the node's facts; ReadsNodeFacts tells the controller
// process when it must ask the node for its facts first.
func TestFleetStageConditionMayReadNodeFacts(t *testing.T) {
	stage := func(when string) string {
		return "fleet_stages:\n- id: find\n  nodes: app\n  outputs: {leader: false}\n- id: update\n  nodes: app\n  when: " + when + "\n  node_tasks: [{show: hi}]\n"
	}
	for _, when := range []string{`{not: '{{ .outputs.find[.node.name].leader }}'}`, `{equals: ['{{ .node.distribution.id }}', debian]}`} {
		if _, _, err := loadRolling(t, stage(when)); err != nil {
			t.Fatal(when, err)
		}
	}
	if _, _, err := loadRolling(t, rollingPlaybook("  when: {equals: ['{{ .node.package_manager }}', apt]}\n")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := loadRolling(t, "fleet_stages:\n- id: plain\n  loop: [x]\n  as: item\n  key: '{{ .item }}'\n  when: '{{ .node.arch }}'\n  fleet_stages: [{id: inner, nodes: app, node_tasks: [{show: hi}]}]\n"); err == nil || !strings.Contains(err.Error(), "unknown reference root .node") {
		t.Fatal("a plain loop has no node", err)
	}
	for when, reads := range map[string]bool{
		"{{ .node.name }}": false, "{{ .vars.enabled }}": false, "{{ .node.distribution.id }}": true,
		`{"all": ["{{ .outputs.find.app1.leader }}", {"equals": ["{{ .node.arch }}", "arm64"]}]}`: true,
	} {
		var condition any = when
		if strings.HasPrefix(when, "{\"") {
			if err := json.Unmarshal([]byte(when), &condition); err != nil {
				t.Fatal(err)
			}
		}
		if ReadsNodeFacts(condition) != reads {
			t.Errorf("%s reads facts: %v", when, !reads)
		}
	}
}

// A limit picks a for_each_node loop's iterations; the enclosed fleet stages
// of a kept iteration keep every node they name, and the nodes outside the
// limit among them are listed as helper nodes.
func TestLimitPicksForEachNodeIterationsAndShowsHelperNodes(t *testing.T) {
	_, _, err := loadRolling(t, rollingPlaybook("  max_nodes_at_once: \"67%\"\n  when: {not: '{{ .outputs.inspect[.app_server].current }}'}\n")+"- id: after\n  nodes: app\n  node_tasks: [{show: hi}]\n")
	if err == nil || !strings.Contains(err.Error(), "reference .outputs.inspect[.app_server].current names a value that is not declared or is declared later") {
		t.Fatal("a when may read only earlier fleet stages", err)
	}
	p, inv, err := loadRolling(t, "fleet_stages:\n- id: inspect\n  nodes: app\n  outputs: {current: false}\n"+strings.TrimPrefix(rollingPlaybook("  max_nodes_at_once: \"67%\"\n  when: {not: '{{ .outputs.inspect[.app_server].current }}'}\n"), "fleet_stages:\n"))
	if err != nil {
		t.Fatal(err)
	}
	view, err := Inspect(p, inv, []string{"app2"})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, fleetStage := range view.FleetStages {
		got[fleetStage.Path] = strings.Join(fleetStage.Nodes, ",") + "|" + strings.Join(fleetStage.LeftOut, ",")
		if strings.HasPrefix(fleetStage.Path, "rollout") != fleetStage.NarrowedByWhen {
			t.Errorf("%s: narrowed by when %v", fleetStage.Path, fleetStage.NarrowedByWhen)
		}
	}
	want := map[string]string{"inspect": "app2|app1,app3", "rollout": "app2|app1,app3", "rollout[app2]/drain": "lb|", "rollout[app2]/update": "app2|", "rollout[app2]/rejoin": "lb|"}
	if !reflect.DeepEqual(got, want) {
		t.Fatal(got)
	}
	if loop := view.FleetStages[1]; !loop.ForEachNodeLoop || loop.ResolvedMaxNodesAtOnce == nil || *loop.ResolvedMaxNodesAtOnce != 1 {
		t.Fatalf("%+v", loop)
	}
	if helpers := []HelperNode{{Node: "lb", FleetStages: []string{"rollout[app2]/drain", "rollout[app2]/rejoin"}}}; !reflect.DeepEqual(view.HelperNodes, helpers) {
		t.Fatal(view.HelperNodes)
	}
	// Without a limit there are no helper nodes, and 67% of three nodes is two.
	if view, err = Inspect(p, inv, nil); err != nil || len(view.HelperNodes) != 0 || *view.FleetStages[1].ResolvedMaxNodesAtOnce != 2 || len(view.FleetStages) != 11 {
		t.Fatal(err, view.HelperNodes, len(view.FleetStages))
	}
	// A limit that leaves out the whole loop leaves out a reader of its outputs.
	p, inv, err = loadRolling(t, rollingPlaybook("")+"- id: report\n  nodes: lb\n  node_tasks: [{show: '{{ .outputs.rollout.app1.healthy }}'}]\n")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Inspect(p, inv, []string{"lb"}); err == nil || !strings.Contains(err.Error(), "--limit excludes fleet stage rollout, whose outputs fleet stage report reads") {
		t.Fatal(err)
	}
	// A reader of the whole output collection of a narrowed loop is rejected.
	p, inv, err = loadRolling(t, rollingPlaybook("")+"- id: report\n  nodes: lb\n  node_tasks: [{show: '{{ .outputs.rollout }}'}]\n")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Inspect(p, inv, []string{"app1", "lb"}); err == nil || !strings.Contains(err.Error(), "excludes some nodes from fleet stage rollout, whose whole output collection fleet stage report reads") {
		t.Fatal(err)
	}
}

func TestNodesAtOnceCountsTheSelection(t *testing.T) {
	for _, tc := range []struct {
		written        NodesAtOnce
		selected, want int
	}{{"", 6, 100}, {"5", 6, 5}, {"34%", 6, 2}, {"34%", 2, 1}, {"100%", 6, 6}, {"1%", 0, 1}} {
		if got, err := tc.written.Nodes(tc.selected, 100); err != nil || got != tc.want {
			t.Errorf("%q of %d: %d %v", tc.written, tc.selected, got, err)
		}
	}
	for _, bad := range []NodesAtOnce{"0", "0%", "101%", "-1", "two", "2.5%"} {
		if _, err := bad.Nodes(6, 100); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

// except_nodes works on a for_each_node loop as on a fleet stage, and a plain
// loop still rejects it. --only-fleet-stages keeps or leaves out the loop whole.
// save_on_controller_machine belongs on the loop's fleet stages, not the loop.
func TestForEachNodeExceptNodesOnlyFleetStagesAndSavedFiles(t *testing.T) {
	p, inv, err := loadRolling(t, rollingPlaybook("  except_nodes: [app2]\n"))
	if err != nil {
		t.Fatal(err)
	}
	view, err := Inspect(p, inv, nil)
	if err != nil || !reflect.DeepEqual(view.FleetStages[0].Nodes, []string{"app1", "app3"}) || view.FleetStages[1].Path != "rollout[app1]/drain" {
		t.Fatal(err, view.FleetStages[0])
	}
	if _, _, err := loadRolling(t, rollingPlaybook("  except_nodes: [app]\n")); err == nil || !strings.Contains(err.Error(), "except_nodes leaves no node of for_each_node") {
		t.Fatal(err)
	}
	if _, _, err := loadRolling(t, "fleet_stages:\n- id: plain\n  loop: [x]\n  as: item\n  key: '{{ .item }}'\n  except_nodes: [app2]\n  fleet_stages: [{id: inner, nodes: app, node_tasks: [{show: hi}]}]\n"); err == nil || !strings.Contains(err.Error(), "fleet stage loop plain contains fleet_stages") {
		t.Fatal(err)
	}
	if _, _, err := loadRolling(t, rollingPlaybook("  save_on_controller_machine: [{output: healthy, path: 'a/{{ .node.name }}', permissions_mode: '0644'}]\n")); err == nil || !strings.Contains(err.Error(), "save_on_controller_machine belongs to a fleet stage that selects nodes") {
		t.Fatal(err)
	}
	saved := strings.Replace(rollingPlaybook(""), "    outputs: {healthy: true}\n", "    outputs: {healthy: true}\n    save_on_controller_machine: [{output: healthy, path: 'health/{{ .node.name }}.txt', permissions_mode: '0644'}]\n", 1)
	if _, _, err := loadRolling(t, saved); err != nil {
		t.Fatal(err)
	}
	p, inv, err = loadRolling(t, rollingPlaybook("")+"- id: report\n  nodes: lb\n  node_tasks: [{show: hi}]\n")
	if err != nil {
		t.Fatal(err)
	}
	p.OnlyFleetStages = []string{"drain"}
	if _, err := Inspect(p, inv, nil); err == nil || !strings.Contains(err.Error(), "fleet stage drain is inside the fleet stage loop rollout and cannot be named alone") {
		t.Fatal(err)
	}
	p.OnlyFleetStages = []string{"report"}
	view, err = Inspect(p, inv, nil)
	if err != nil || len(view.FleetStages) != 2 || !view.FleetStages[0].LeftOutByOnlyFleetStages || view.FleetStages[0].Selection != "app" {
		t.Fatal(err, view.FleetStages)
	}
}
