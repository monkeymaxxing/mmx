package config

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// selectionProject writes an inventory of two web nodes, a database, and a
// build machine in the group virtual_machines, and a playbook.
func selectionProject(t *testing.T) func(fleetStages string) (Project, Inventory, error) {
	path := fixture(t, "")
	write(t, filepath.Join(filepath.Dir(path), "inventories/example.yml"), "vars: {skip: [db1]}\nnodes:\n  web1: {address: a}\n  web2: {address: b}\n  db1: {address: c}\n  build1: {address: d}\ngroups:\n  web: {nodes: [web1, web2]}\n  canary: {nodes: [web1]}\n  virtual_machines: {nodes: [build1]}\n  retired: {nodes: []}\n")
	return func(fleetStages string) (Project, Inventory, error) {
		t.Helper()
		write(t, path, "project_name: test\ndefault_inventory: example\nfleet_stages:\n"+fleetStages)
		return Load(path, "example", "", nil)
	}
}

func selections(view Inspection) map[string]string {
	got := map[string]string{}
	for _, fleetStage := range view.FleetStages {
		got[fleetStage.Path] = strings.Join(fleetStage.Nodes, ",") + "|" + strings.Join(fleetStage.LeftOut, ",")
		if fleetStage.LeftOutByOnlyFleetStages {
			got[fleetStage.Path] = "not selected"
		}
	}
	return got
}

// An except_nodes that selects no node, such as an empty group or an empty
// list, removes nothing; it is not an error the way an empty nodes is.
func TestExceptNodesThatSelectsNoNodeRemovesNothing(t *testing.T) {
	load := selectionProject(t)
	p, inv, err := load(`- id: emptied_group
  nodes: web
  except_nodes: [retired]
  node_tasks: [{show: hi}]
- id: empty_list
  nodes: web
  except_nodes: []
  node_tasks: [{show: hi}]
`)
	if err != nil {
		t.Fatal(err)
	}
	view, err := Inspect(p, inv, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"emptied_group": "web1,web2|", "empty_list": "web1,web2|"}
	if got := selections(view); !reflect.DeepEqual(got, want) {
		t.Fatal(got)
	}
	if _, _, err := load("- id: nothing\n  nodes: retired\n  node_tasks: [{show: hi}]\n"); err == nil || !strings.Contains(err.Error(), "node selection is empty") {
		t.Fatalf("an empty nodes selection must still fail: %v", err)
	}
}

// except_nodes removes the union of its selectors from the union of nodes,
// from the inventory or a value known before connecting; the limit then
// narrows the rest. An unknown name, a selection left empty, and except_nodes
// on a loop over fleet stages are rejected.
func TestExceptNodesRemovesNodesFromTheSelection(t *testing.T) {
	load := selectionProject(t)
	p, inv, err := load(`- id: network
  nodes: all
  except_nodes: [virtual_machines]
  node_tasks: [{show: hi}]
- id: app
  nodes: [web, db1]
  except_nodes: '{{ .vars.skip }}'
  node_tasks: [{show: hi}]
- id: configure
  nodes: web1
  requires: {same_node: [network]}
  node_tasks: [{show: hi}]
`)
	if err != nil {
		t.Fatal(err)
	}
	view, err := Inspect(p, inv, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"network": "db1,web1,web2|", "app": "web1,web2|", "configure": "web1|"}
	if got := selections(view); !reflect.DeepEqual(got, want) || !reflect.DeepEqual(view.FleetStages[0].ExceptNodes, []any{"virtual_machines"}) {
		t.Fatal(got, view.FleetStages[0].ExceptNodes)
	}
	if view, err = Inspect(p, inv, []string{"canary", "build1"}); err != nil {
		t.Fatal(err)
	}
	want = map[string]string{"network": "web1|db1,web2", "app": "web1|web2", "configure": "web1|"}
	if got := selections(view); !reflect.DeepEqual(got, want) {
		t.Fatal(got)
	}
	for _, tc := range []struct{ fleetStages, wants string }{
		{"- id: network\n  nodes: all\n  except_nodes: [nope]\n  node_tasks: [{show: hi}]\n", `except_nodes): no node or inventory group named "nope"`},
		{"- id: network\n  nodes: web\n  except_nodes: [web1, web2]\n  node_tasks: [{show: hi}]\n", "except_nodes leaves no node of nodes"},
		{"- id: network\n  nodes: all\n  except_nodes: '{{ .vars.missing }}'\n  node_tasks: [{show: hi}]\n", "except_nodes"},
		{"- id: network\n  nodes: all\n  except_nodes: [db1]\n  node_tasks: [{show: hi}]\n- id: configure\n  nodes: all\n  requires: {same_node: [network]}\n  node_tasks: [{show: hi}]\n", "the required fleet stage network in requires.same_node does not select node db1"},
		{"- id: roll\n  loop: [web1]\n  as: app\n  key: '{{ .app }}'\n  except_nodes: [db1]\n  fleet_stages: [{id: update, nodes: '{{ .app }}', node_tasks: [{show: hi}]}]\n", "selection and failure policies belong to those fleet_stages"},
	} {
		if _, _, err := load(tc.fleetStages); err == nil || !strings.Contains(err.Error(), tc.wants) {
			t.Errorf("want %q, got %v", tc.wants, err)
		}
	}
}

// --only-fleet-stages keeps the named top-level fleet stages and loops, in
// written order, and lists the others as not selected. It is rejected before
// any connection when a kept fleet stage needs one it leaves out, printing
// the value that keeps everything needed; an unknown ID lists the playbook's
// fleet stage IDs, and an enclosed fleet stage cannot be named alone.
func TestOnlyFleetStagesKeepsWholeFleetStagesInWrittenOrder(t *testing.T) {
	load := selectionProject(t)
	p, inv, err := load(`- id: ruby
  nodes: web
  node_tasks: [{show: ruby}]
- id: nginx_passenger
  nodes: web
  outputs: {port: 80}
- id: platform_api_sites
  nodes: web
  requires: {same_node: [nginx_passenger]}
  node_tasks: [{show: sites}]
- id: monitoring
  nodes: all
  node_tasks: [{show: '{{ .outputs.nginx_passenger.web1.port }}'}]
- id: roll
  loop: [web1, web2]
  as: app
  key: '{{ .app }}'
  fleet_stages:
    - {id: update, nodes: '{{ .app }}', node_tasks: [{show: hi}]}
    - {id: check, nodes: '{{ .app }}', requires: {same_node: [update, ruby]}, node_tasks: [{show: hi}]}
`)
	if err != nil {
		t.Fatal(err)
	}
	p.OnlyFleetStages = []string{"platform_api_sites", "nginx_passenger"}
	view, err := Inspect(p, inv, []string{"canary"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"ruby": "not selected", "nginx_passenger": "web1|web2", "platform_api_sites": "web1|web2", "monitoring": "not selected", "roll": "not selected"}
	if got := selections(view); !reflect.DeepEqual(got, want) || !reflect.DeepEqual(view.OnlyFleetStages, p.OnlyFleetStages) || view.FleetStages[1].Path != "nginx_passenger" {
		t.Fatal(got, view.OnlyFleetStages)
	}
	p.OnlyFleetStages = []string{"ruby", "roll"}
	if view, err = Inspect(p, inv, nil); err != nil {
		t.Fatal(err)
	}
	if got := selections(view); got["roll[web1]/update"] != "web1|" || got["roll[web2]/check"] != "web2|" || got["monitoring"] != "not selected" {
		t.Fatal(got)
	}
	for _, tc := range []struct{ only, wants string }{
		{"platform_api_sites", "--only-fleet-stages platform_api_sites leaves out fleet stage nginx_passenger, which fleet stage platform_api_sites requires; use --only-fleet-stages nginx_passenger,platform_api_sites"},
		{"monitoring", "leaves out fleet stage nginx_passenger, whose outputs fleet stage monitoring reads; use --only-fleet-stages nginx_passenger,monitoring"},
		{"roll", "leaves out fleet stage ruby, which fleet stage roll/check requires; use --only-fleet-stages ruby,roll"},
		{"nope", `--only-fleet-stages: the playbook has no fleet stage "nope"; its fleet stages are ruby, nginx_passenger, platform_api_sites, monitoring, roll`},
		{"update", "fleet stage update is inside the fleet stage loop roll and cannot be named alone; name roll to keep the whole loop"},
	} {
		p.OnlyFleetStages = strings.Split(tc.only, ",")
		if _, err := Inspect(p, inv, nil); err == nil || err.Error() != tc.wants && !strings.Contains(err.Error(), tc.wants) {
			t.Errorf("--only-fleet-stages %s: want %q, got %v", tc.only, tc.wants, err)
		}
	}
	// With a limit, the kept fleet stages still need to keep a node.
	p.OnlyFleetStages = []string{"ruby"}
	if _, err := Inspect(p, inv, []string{"db1"}); err == nil || !strings.Contains(err.Error(), "--limit db1 selects no node in any fleet stage") {
		t.Fatal(err)
	}
}

// A fleet stage or for_each_node loop whose selection is empty, through an
// empty group or an except_nodes that removes every node, fails when the
// project loads, naming the setting that allows it. With
// skip_when_no_nodes_are_selected it is not selected, also under a limit: a
// requirement on it is met and its whole output collection may be read, but
// a named node's output may not. A fleet stage that selects nodes ignores
// the setting, and an unknown name stays an error.
func TestAFleetStageThatSelectsNoNodesIsLeftOutOnlyWithTheSetting(t *testing.T) {
	load := selectionProject(t)
	for _, tc := range []struct{ fleetStages, wants string }{
		{"- id: staging\n  nodes: retired\n  node_tasks: [{show: hi}]\n", "node selection is empty; to leave the fleet stage out of the run when it selects no node, set skip_when_no_nodes_are_selected: true"},
		{"- id: staging\n  nodes: web\n  except_nodes: web\n  node_tasks: [{show: hi}]\n", "except_nodes leaves no node of nodes; to leave the fleet stage out of the run when it selects no node, set skip_when_no_nodes_are_selected: true"},
		{"- id: roll\n  for_each_node: retired\n  as: app\n  fleet_stages: [{id: update, nodes: '{{ .app }}', node_tasks: [{show: hi}]}]\n", "node selection is empty"},
		{"- id: staging\n  nodes: [retired, nope]\n  skip_when_no_nodes_are_selected: true\n  node_tasks: [{show: hi}]\n", `no node or inventory group named "nope"`},
		{"- id: staging\n  nodes: retired\n  skip_when_no_nodes_are_selected: true\n  outputs: {port: 80}\n- id: report\n  nodes: web1\n  node_tasks: [{show: '{{ .outputs.staging.web1.port }}'}]\n", "reference .outputs.staging.web1.port names a node that the referenced fleet stage does not select"},
		{"- id: roll\n  loop: [web1]\n  as: app\n  key: '{{ .app }}'\n  skip_when_no_nodes_are_selected: true\n  fleet_stages: [{id: update, nodes: '{{ .app }}', node_tasks: [{show: hi}]}]\n", "selection and failure policies belong to those fleet_stages"},
	} {
		if _, _, err := load(tc.fleetStages); err == nil || !strings.Contains(err.Error(), tc.wants) {
			t.Errorf("want %q, got %v", tc.wants, err)
		}
	}
	p, inv, err := load(`- id: staging
  nodes: retired
  skip_when_no_nodes_are_selected: true
  outputs: {port: 80}
- id: emptied
  nodes: web
  except_nodes: web
  skip_when_no_nodes_are_selected: true
  node_tasks: [{show: hi}]
- id: roll
  for_each_node: retired
  as: app
  skip_when_no_nodes_are_selected: true
  fleet_stages: [{id: update, nodes: '{{ .app }}', node_tasks: [{show: hi}]}]
- id: web_servers
  nodes: web
  skip_when_no_nodes_are_selected: true
  requires: {all_nodes: [staging, emptied]}
  node_tasks: [{show: '{{ .outputs.staging }}'}]
`)
	if err != nil {
		t.Fatal(err)
	}
	for _, limit := range [][]string{nil, {"canary"}} {
		view, err := Inspect(p, inv, limit)
		if err != nil {
			t.Fatal(limit, err)
		}
		notSelected := map[string]bool{}
		for _, fleetStage := range view.FleetStages {
			notSelected[fleetStage.Path] = fleetStage.LeftOutBecauseItSelectsNoNodes
		}
		web := "web1,web2|"
		if limit != nil {
			web = "web1|web2"
		}
		if got := selections(view); !reflect.DeepEqual(got, map[string]string{"staging": "|", "emptied": "|", "roll": "|", "web_servers": web}) ||
			!reflect.DeepEqual(notSelected, map[string]bool{"staging": true, "emptied": true, "roll": true, "web_servers": false}) {
			t.Fatal(limit, got, notSelected)
		}
	}
	// A limit must still keep a node of some fleet stage.
	if _, err := Inspect(p, inv, []string{"db1"}); err == nil || !strings.Contains(err.Error(), "--limit db1 selects no node in any fleet stage") {
		t.Fatal(err)
	}
}
