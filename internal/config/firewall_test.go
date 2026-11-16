package config

import (
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"mmx/internal/modules"
	"mmx/internal/value"
)

// groupInventory has the inventory of the design's example: two application
// servers with a private address each, and a database server.
const groupInventory = `nodes:
  app01: {address: 203.0.113.11, vars: {private_address: 10.0.1.11}}
  app02: {address: 203.0.113.12, vars: {private_address: 10.0.1.12}}
  db01: {address: 203.0.113.20, vars: {private_address: 10.0.1.20}}
  bare: {address: 203.0.113.30}
  cache01: {address: 203.0.113.41}
  cache02: {address: 203.0.113.42}
groups:
  app: {nodes: [app02, app01]}
  database: {nodes: [db01]}
  with_bare: {nodes: [app01, bare]}
  shared: {nodes: [cache01, cache02], vars: {private_address: 10.0.1.99}}
`

func loadFirewallTask(t *testing.T, inventory, task string) (Project, Inventory, error) {
	t.Helper()
	p := fixture(t, "vars: {port: 5432, office: 192.0.2.0/24, zone: public}\nfleet_stages: [{id: configure, nodes: all, node_tasks: [{"+task+"}]}]\n")
	if inventory != "" {
		write(t, filepath.Join(filepath.Dir(p), "inventories/example.yml"), inventory)
	}
	return Load(p, "example", "", nil)
}

func TestFirewalldConfiguration(t *testing.T) {
	for _, task := range []string{
		`firewalld: {zone: public, rules: [{action: allow, firewalld_service: ssh}, {action: allow, port: 7443, protocol: tcp}], rules_not_listed: remove}`,
		`firewalld: {zone: public, rules: [{action: allow, port: "8000:8010", protocol: udp, from: 10.0.0.0/8}, {action: reject, port: 23, protocol: tcp}, {action: drop, firewalld_service: https, from: "2001:db8::/32"}, {action: allow, from: 192.0.2.7}], rules_not_listed: keep}`,
		`firewalld: {zone: "{{ .vars.zone }}", rules: [{action: allow, port: "{{ .vars.port }}", protocol: tcp, from: "{{ .vars.office }}"}], rules_not_listed: keep}`,
		`firewalld: {zone: public, rules: [], rules_not_listed: remove}`,
		`firewalld: {zone: public, rules: [{firewalld_service: ssh}, {port: 22, protocol: tcp, from: 10.0.0.0/8}], state: absent}`,
	} {
		project, _, err := loadFirewallTask(t, "", task)
		if err != nil {
			t.Fatal(task, err)
		}
		loaded := project.FleetStages[0].NodeTasks[0]
		if loaded.Retry == nil || loaded.Retry.Disabled {
			t.Fatal("resource did not inherit retry policy", task)
		}
		b, _ := json.Marshal(loaded)
		var saved Task
		if err := json.Unmarshal(b, &saved); err != nil || InputsOf[modules.Firewalld](saved).Rules == nil {
			t.Fatal("explicit empty list lost during saved retry", task, err)
		}
	}
	for task, want := range map[string]string{
		`firewalld: {rules: [], rules_not_listed: remove}`:                                                                                                   "zone must be a name as firewalld writes it",
		`firewalld: {zone: "public zone", rules: [], rules_not_listed: remove}`:                                                                              "zone must be a name as firewalld writes it",
		`firewalld: {zone: public, rules_not_listed: remove}`:                                                                                                "firewalld.rules is required",
		`firewalld: {zone: public, rules: [{action: allow, firewalld_service: ssh}]}`:                                                                        "rules_not_listed is required",
		`firewalld: {zone: public, rules: [{action: limit, firewalld_service: ssh}], rules_not_listed: keep}`:                                                "action must be allow, reject, or drop",
		`firewalld: {zone: public, rules: [{firewalld_service: ssh}], rules_not_listed: keep}`:                                                               "action must be allow, reject, or drop",
		`firewalld: {zone: public, rules: [{action: allow, firewalld_service: ssh, port: 22, protocol: tcp}], rules_not_listed: keep}`:                       "firewalld_service or port, not both",
		`firewalld: {zone: public, rules: [{action: allow, port: 22}], rules_not_listed: keep}`:                                                              "needs protocol",
		`firewalld: {zone: public, rules: [{action: allow, firewalld_service: ssh, protocol: tcp}], rules_not_listed: keep}`:                                 "protocol goes with port",
		`firewalld: {zone: public, rules: [{action: allow}], rules_not_listed: keep}`:                                                                        "needs firewalld_service, port, from, or from_inventory_group",
		`firewalld: {zone: public, rules: [{action: allow, firewalld_service: "ssh;reboot"}], rules_not_listed: keep}`:                                       "firewalld_service must be a name as firewalld writes it",
		`firewalld: {zone: public, rules: [{action: allow, port: 70000, protocol: tcp}], rules_not_listed: keep}`:                                            "1 to 65535",
		`firewalld: {zone: public, rules: [{action: allow, port: 22, protocol: tcp, from: 10.0.0.5/8}], rules_not_listed: keep}`:                             "network such as",
		`firewalld: {zone: public, rules: [{action: allow, firewalld_service: ssh}, {action: drop, firewalld_service: ssh}], rules_not_listed: keep}`:        "rule listed twice: service ssh",
		`firewalld: {zone: public, rules: [{action: allow, port: 22, protocol: tcp}, {action: reject, port: "022", protocol: tcp}], rules_not_listed: keep}`: "rule listed twice: port 22/tcp",
		`firewalld: {zone: public, rules: [{firewalld_service: ssh}], rules_not_listed: keep, state: absent}`:                                                "accepts only zone and rules",
		`firewalld: {zone: public, rules: [], state: absent}`:                                                                                                "needs at least one rule",
		`firewalld: {zone: public, rules: [{action: allow, port: "{{ .vars.missing }}", protocol: tcp}], rules_not_listed: keep}`:                            "missing",
	} {
		if _, _, err := loadFirewallTask(t, "", task); err == nil || !strings.Contains(err.Error(), want) {
			t.Error(task, err)
		}
	}
}

func TestFirewalldIdentity(t *testing.T) {
	for want, rule := range map[string]modules.FirewalldRule{
		"service ssh":                        {FirewalldService: "ssh"},
		"port 7443/tcp":                      {Port: "7443", Proto: "tcp"},
		"port 8000-8010/udp from 10.0.0.0/8": {Port: "8000:8010", Proto: "udp", From: "10.0.0.0/8"},
		"port 22/tcp from 10.0.0.1":          {Port: "022", Proto: "tcp", From: "10.0.0.1/32"},
		"service https from 2001:db8::/32":   {FirewalldService: "https", From: "2001:DB8::/32"},
		"from 192.0.2.7":                     {From: "192.0.2.7"},
	} {
		if got, err := rule.Identity(); err != nil || got != want {
			t.Errorf("%+v: %q %v", rule, got, err)
		}
	}
}

func TestRulesForEachNodeOfAnInventoryGroup(t *testing.T) {
	group := `from_inventory_group: {group: app, address_var: private_address}`
	for _, task := range []string{
		`ufw: {incoming_rules: [{action: limit, port: 22, protocol: tcp}, {action: allow, port: 5432, protocol: tcp, ` + group + `}], incoming_rules_not_listed: remove}`,
		`ufw: {incoming_rules: [{protocol: tcp_and_udp, action: allow, ` + group + `}], incoming_rules_not_listed: keep}`,
		`ufw: {incoming_rules: [{protocol: tcp_and_udp, ` + group + `}], state: absent}`,
		`firewalld: {zone: public, rules: [{action: allow, firewalld_service: postgresql, ` + group + `}], rules_not_listed: keep}`,
		`firewalld: {zone: public, rules: [{action: allow, ` + group + `}], rules_not_listed: keep}`,
	} {
		if _, _, err := loadFirewallTask(t, groupInventory, task); err != nil {
			t.Fatal(task, err)
		}
	}
	// Checked before MMX connects, also for a task that may not run.
	for task, want := range map[string]string{
		`ufw: {incoming_rules: [{action: allow, port: 5432, protocol: tcp, from_inventory_group: {group: web, address_var: private_address}}], incoming_rules_not_listed: keep}`:                          "ufw.incoming_rules[0].from_inventory_group): the inventory has no group web",
		`ufw: {incoming_rules: [{action: allow, port: 5432, protocol: tcp, from_inventory_group: {group: with_bare, address_var: private_address}}], incoming_rules_not_listed: keep}`:                    "node bare in inventory group with_bare has no variable private_address in vars",
		`when: {equals: ["{{ .node.name }}", db01]}, firewalld: {zone: public, rules: [{action: allow, from_inventory_group: {group: with_bare, address_var: private_address}}], rules_not_listed: keep}`: "node bare in inventory group with_bare has no variable private_address",
		`ufw: {incoming_rules: [{action: allow, port: 5432, protocol: tcp, from_inventory_group: {group: app, address_var: address}}], incoming_rules_not_listed: keep}`:                                  "node app01 in inventory group app has no variable address",
		`ufw: {incoming_rules: [{action: allow, port: 5432, protocol: tcp, from_inventory_group: {group: shared, address_var: private_address}}], incoming_rules_not_listed: keep}`:                       "with one rule for each node of the inventory group: rule listed twice: from 10.0.1.99 to any port 5432 proto tcp",
		`ufw: {incoming_rules: [{action: allow, port: 5432, protocol: tcp, from: 10.0.1.11}, {action: deny, port: 5432, protocol: tcp, ` + group + `}], incoming_rules_not_listed: keep}`:                 "rule listed twice: from 10.0.1.11 to any port 5432 proto tcp",
		`ufw: {incoming_rules: [{protocol: tcp_and_udp, action: allow, port: 5432, from: 10.0.0.0/8, ` + group + `}], incoming_rules_not_listed: keep}`:                                                   "from or from_inventory_group, not both",
		`firewalld: {zone: public, rules: [{action: allow, firewalld_service: ssh, from: 10.0.0.0/8, ` + group + `}], rules_not_listed: keep}`:                                                            "from or from_inventory_group, not both",
		`ufw: {incoming_rules: [{protocol: tcp_and_udp, action: allow, from_inventory_group: {group: app}}], incoming_rules_not_listed: keep}`:                                                            "address_var must name a variable",
		`firewalld: {zone: public, rules: [{action: allow, from_inventory_group: {address_var: private_address}}], rules_not_listed: keep}`:                                                               "group must name an inventory group",
	} {
		if _, _, err := loadFirewallTask(t, groupInventory, task); err == nil || !strings.Contains(err.Error(), want) {
			t.Error(task, err)
		}
	}
	addresses := strings.ReplaceAll(groupInventory, "app01: {address: 203.0.113.11, vars: {private_address: 10.0.1.11}}", "app01: {address: 203.0.113.11, vars: {private_address: app01.internal}}")
	if _, _, err := loadFirewallTask(t, addresses, `ufw: {incoming_rules: [{protocol: tcp_and_udp, action: allow, `+group+`}], incoming_rules_not_listed: keep}`); err == nil || !strings.Contains(err.Error(), "node app01 of group app: vars.private_address must be an address") {
		t.Error(err)
	}

	// The node expands the rules from the .inventory values every job carries,
	// in node name order, after the rule's own references resolve.
	project, inv, err := loadFirewallTask(t, groupInventory, `ufw: {incoming_rules: [{action: limit, port: 22, protocol: tcp}, {action: allow, port: "{{ .vars.port }}", protocol: tcp, `+group+`}, {action: allow, port: 443, protocol: tcp}], incoming_rules_not_listed: remove}`)
	if err != nil {
		t.Fatal(err)
	}
	scope := value.Scope{"vars": NodeVars(project, inv, "db01"), "inventory": InventoryValues(inv)}
	written := project.FleetStages[0].NodeTasks[0]
	resolved, err := ResolveArguments(written, scope)
	if err != nil {
		t.Fatal(err)
	}
	want := []modules.UFWRule{{Action: "limit", Port: "22", Proto: "tcp"}, {Action: "allow", Port: "5432", Proto: "tcp", From: "10.0.1.11"},
		{Action: "allow", Port: "5432", Proto: "tcp", From: "10.0.1.12"}, {Action: "allow", Port: "443", Proto: "tcp"}}
	if !reflect.DeepEqual(InputsOf[modules.UFW](resolved).IncomingRules, want) || len(InputsOf[modules.UFW](written).IncomingRules) != 3 || InputsOf[modules.UFW](written).IncomingRules[1].FromInventoryGroup == nil {
		t.Fatalf("%+v\n%+v", InputsOf[modules.UFW](resolved).IncomingRules, InputsOf[modules.UFW](written).IncomingRules)
	}
	project, inv, err = loadFirewallTask(t, groupInventory, `firewalld: {zone: public, rules: [{action: allow, firewalld_service: postgresql, `+group+`}], rules_not_listed: remove}`)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err = ResolveArguments(project.FleetStages[0].NodeTasks[0], value.Scope{"inventory": InventoryValues(inv)})
	if err != nil || !reflect.DeepEqual(InputsOf[modules.Firewalld](resolved).Rules, []modules.FirewalldRule{{Action: "allow", FirewalldService: "postgresql", From: "10.0.1.11"}, {Action: "allow", FirewalldService: "postgresql", From: "10.0.1.12"}}) {
		t.Fatal(InputsOf[modules.Firewalld](resolved), err)
	}
	// A group that has lost its nodes gives no rules; a missing inventory fails.
	empty := InventoryValues(inv)
	empty["groups"].(map[string]any)["app"] = []any{}
	if resolved, err = ResolveArguments(project.FleetStages[0].NodeTasks[0], value.Scope{"inventory": empty}); err != nil || InputsOf[modules.Firewalld](resolved).Rules == nil || len(InputsOf[modules.Firewalld](resolved).Rules) != 0 {
		t.Fatal(InputsOf[modules.Firewalld](resolved), err)
	}
	if _, err := ResolveArguments(project.FleetStages[0].NodeTasks[0], value.Scope{}); err == nil || !strings.Contains(err.Error(), "firewalld.rules[0].from_inventory_group") {
		t.Fatal(err)
	}
}

func TestRulesFromTheInventoryAddressesOfAGroup(t *testing.T) {
	group := `from_inventory_group: {group: app, use_node_address: true}`
	project, inv, err := loadFirewallTask(t, groupInventory, `ufw: {incoming_rules: [{action: allow, port: 51820, protocol: udp, `+group+`}], incoming_rules_not_listed: remove}`)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := ResolveArguments(project.FleetStages[0].NodeTasks[0], value.Scope{"vars": NodeVars(project, inv, "db01"), "inventory": InventoryValues(inv)})
	if err != nil {
		t.Fatal(err)
	}
	want := []modules.UFWRule{{Action: "allow", Port: "51820", Proto: "udp", From: "203.0.113.11"}, {Action: "allow", Port: "51820", Proto: "udp", From: "203.0.113.12"}}
	if got := InputsOf[modules.UFW](resolved).IncomingRules; !reflect.DeepEqual(got, want) {
		t.Fatalf("%+v", got)
	}
	for task, want := range map[string]string{
		`ufw: {incoming_rules: [{action: allow, port: 51820, protocol: udp, from_inventory_group: {group: app, address_var: private_address, use_node_address: true}}], incoming_rules_not_listed: keep}`: "address_var or use_node_address: true, not both",
	} {
		if _, _, err := loadFirewallTask(t, groupInventory, task); err == nil || !strings.Contains(err.Error(), want) {
			t.Error(task, err)
		}
	}
	// A host name cannot be a rule's source; validation says where to put the IP addresses.
	names := strings.ReplaceAll(groupInventory, "app01: {address: 203.0.113.11,", "app01: {address: app01.example.net,")
	if _, _, err := loadFirewallTask(t, names, `ufw: {incoming_rules: [{action: allow, port: 51820, protocol: udp, `+group+`}], incoming_rules_not_listed: keep}`); err == nil || !strings.Contains(err.Error(), `node app01 of group app: its address "app01.example.net" is not an IP address`) {
		t.Error(err)
	}
}

// A rule inside a role reads the inventory its node job carries, as it does in
// a fleet stage; the role's own references still see only its inputs.
func TestRulesForAnInventoryGroupInsideARole(t *testing.T) {
	p := fixture(t, "fleet_stages: [{id: configure, nodes: all, node_tasks: [{role: firewall}]}]\n")
	write(t, filepath.Join(filepath.Dir(p), "inventories/example.yml"), groupInventory)
	write(t, filepath.Join(filepath.Dir(p), "roles/firewall/main.yml"), "inputs: {}\nnode_tasks: [{ufw: {incoming_rules: [{action: allow, port: 51820, protocol: udp, from_inventory_group: {group: app, use_node_address: true}}], incoming_rules_not_listed: remove}}]\n")
	project, inv, err := Load(p, "example", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	call := project.FleetStages[0].NodeTasks[0]
	child, err := ResolveRoleInputs(call, value.Scope{"vars": NodeVars(project, inv, "db01"), "inventory": InventoryValues(inv)}, false)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := ResolveArguments(call.RoleNodeTasks[0], child)
	if err != nil {
		t.Fatal(err)
	}
	want := []modules.UFWRule{{Action: "allow", Port: "51820", Proto: "udp", From: "203.0.113.11"}, {Action: "allow", Port: "51820", Proto: "udp", From: "203.0.113.12"}}
	if got := InputsOf[modules.UFW](resolved).IncomingRules; !reflect.DeepEqual(got, want) {
		t.Fatalf("%+v", got)
	}
	write(t, filepath.Join(filepath.Dir(p), "roles/firewall/main.yml"), "inputs: {}\nnode_tasks: [{show: '{{ .inventory.groups.app }}'}]\n")
	if _, _, err := Load(p, "example", "", nil); err == nil || !strings.Contains(err.Error(), "unknown reference root .inventory") {
		t.Fatal(err)
	}
}

// A rule list may be one whole-value reference, such as the rules an earlier
// node task works out. Its rules are checked once the reference has a value,
// with from_inventory_group expanded then.
func TestRuleListFromOneWholeValueReference(t *testing.T) {
	p := fixture(t, "vars: {rules: []}\nfleet_stages: [{id: configure, nodes: all, node_tasks: [{ufw: {incoming_rules: \"{{ .vars.rules }}\", incoming_rules_not_listed: remove}}]}]\n")
	write(t, filepath.Join(filepath.Dir(p), "inventories/example.yml"), groupInventory)
	project, inv, err := Load(p, "example", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	written := project.FleetStages[0].NodeTasks[0]
	resolve := func(rules ...map[string]any) (Task, error) {
		list := make([]any, len(rules))
		for i, r := range rules {
			list[i] = r
		}
		return ResolveArguments(written, value.Scope{"vars": map[string]any{"rules": list}, "inventory": InventoryValues(inv)})
	}
	resolved, err := resolve(map[string]any{"action": "limit", "port": 22, "protocol": "tcp"},
		map[string]any{"action": "allow", "port": 5432, "protocol": "tcp", "from_inventory_group": map[string]any{"group": "app", "address_var": "private_address"}})
	if err != nil {
		t.Fatal(err)
	}
	want := []modules.UFWRule{{Action: "limit", Port: "22", Proto: "tcp"}, {Action: "allow", Port: "5432", Proto: "tcp", From: "10.0.1.11"}, {Action: "allow", Port: "5432", Proto: "tcp", From: "10.0.1.12"}}
	if got := InputsOf[modules.UFW](resolved).IncomingRules; !reflect.DeepEqual(got, want) {
		t.Fatalf("%+v", got)
	}
	if _, err := resolve(map[string]any{"action": "permit", "port": 22, "protocol": "tcp"}); err == nil || !strings.Contains(err.Error(), "action must be allow, deny, reject, or limit") {
		t.Fatal(err)
	}
}
