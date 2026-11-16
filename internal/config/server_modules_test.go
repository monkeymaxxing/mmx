package config

import (
	"encoding/json"
	"strings"
	"testing"

	"mmx/internal/modules"
	"mmx/internal/value"
)

func TestServerModuleConfiguration(t *testing.T) {
	for _, task := range []string{
		`directory: {path: /srv/app, permissions_mode: "0750"}`,
		`directory: {path: /srv/app, also_apply_to_everything_inside: true, permissions_mode: "0750", file_permissions_mode: "0640"}`,
		`group: {name: app, system: true}`,
		`group: {name: app, gid: 0}`,
		`group: {name: app, state: absent}`,
		`user: {create_home_directory: true, name: app, supplementary_groups: [], supplementary_groups_not_listed: remove}`,
		`user: {create_home_directory: true, name: app, uid: 4294967294, supplementary_groups: [], supplementary_groups_not_listed: keep}`,
		`user: {name: app, state: absent}`,
		`systemd_unit: {name: app.service, content: "[Service]\nExecStart=/bin/true\n"}`,
		`systemd_unit: {name: app.service, drop_in: 20-limits.conf, content: "[Service]\nMemoryMax=512M\n"}`,
		`systemd_unit: {name: app.service, state: absent}`,
		`systemd_unit: {name: app.service, drop_in: 20-limits.conf, state: absent}`,
		`systemd_unit: {name: backup.timer, content: "[Timer]\nOnCalendar=daily\n"}`,
		`systemd_unit: {name: app@.timer, drop_in: 20-delay.conf, content: "[Timer]\nRandomizedDelaySec=5m\n"}`,
		`systemd_unit: {name: restart.path, content: "[Path]\nPathExists=/run/request\n"}`,
		`systemd_unit: {name: app.socket, state: absent}`,
		`systemd_unit: {name: 'srv-my\x2ddata.mount', content: "[Mount]\nWhat=tmpfs\nWhere=/srv/my-data\nType=tmpfs\n"}`,
		`systemd_service: {name: backup.timer, start_at_boot: true, state: running}`,
		`systemd_service: {name: 'srv-my\x2ddata.mount', state: running}`,
		`systemd_service: {name: app, refresh: reload, state: running}`,
		`systemd_service: {name: app.v2, refresh: reload, state: running}`,
	} {
		p := fixture(t, "fleet_stages: [{id: configure, nodes: all, node_tasks: [{"+task+"}]}]\n")
		project, _, err := Load(p, "example", "", nil)
		if err != nil {
			t.Fatal(task, err)
		}
		task := project.FleetStages[0].NodeTasks[0]
		// A start that failed fails again, so systemd_service tries once.
		if task.Module == "systemd_service" {
			if task.Retry != nil {
				t.Fatal("systemd_service got a retry policy that its task did not ask for")
			}
		} else if task.Retry == nil || task.Retry.Disabled {
			t.Fatal("resource did not inherit retry policy")
		}
		b, err := json.Marshal(task)
		if err != nil {
			t.Fatal(err)
		}
		var saved Task
		if err := json.Unmarshal(b, &saved); err != nil {
			t.Fatal(err)
		}
		if InputsOf[modules.Directory](task) != nil && (InputsOf[modules.Directory](saved).Recurse != InputsOf[modules.Directory](task).Recurse || InputsOf[modules.Directory](saved).FileMode != InputsOf[modules.Directory](task).FileMode) {
			t.Fatal("recursive settings lost in saved task")
		}
		if InputsOf[modules.User](task) != nil && InputsOf[modules.User](task).SupplementaryGroups != nil && InputsOf[modules.User](saved).SupplementaryGroups == nil {
			t.Fatal("explicit empty membership list lost during saved retry")
		}
	}
	for _, task := range []string{
		`directory: {path: /srv/app, file_permissions_mode: "0640"}`,
		`directory: {path: /srv/app, also_apply_to_everything_inside: true, file_permissions_mode: "0999"}`,
		`directory: {path: /srv/app, also_apply_to_everything_inside: yes}`,
		`directory: {path: /srv/app, also_apply_to_everything_inside: true, file_permissions_mode: 640}`,
		`directory: {path: relative}`, `directory: {path: /srv/app, permissions_mode: "0999"}`,
		`user: {create_home_directory: true, name: "-option"}`, `user: {create_home_directory: true, name: app, supplementary_groups_not_listed: remove}`,
		`user: {create_home_directory: true, name: app, home: "relative"}`, `group: {name: "x:y"}`,
		`user: {create_home_directory: true, name: app, uid: -1}`, `group: {name: app, gid: 4294967295}`,
		`user: {create_home_directory: true, name: app, uid: "1200"}`, `user: {create_home_directory: true, name: app, uid: true}`,
		`user: {create_home_directory: true, name: app, uid: 1.5}`, `group: {name: app, gid: [1200]}`,
		`user: {create_home_directory: true, name: app, uid: "id-{{ .facts.cpu_count }}"}`,
		`group: {name: app, state: invalid}`,
		`user: {name: app, state: absent, uid: 1200}`,
		`user: {name: app, state: absent, system: false}`,
		`user: {name: app, state: absent, supplementary_groups: []}`,
		`group: {name: app, state: absent, gid: 1200}`,
		`group: {name: app, state: absent, system: false}`,
		`systemd_unit: {name: "../../tmp/x.service", content: x}`,
		`systemd_unit: {name: app.service}`, `systemd_unit: {name: app.service, content: x, template: x}`,
		`systemd_unit: {name: app.service, drop_in: ../escape.conf, content: x}`,
		`systemd_unit: {name: app.service, drop_in: /escape.conf, content: x}`,
		`systemd_unit: {name: app.service, drop_in: wrong.service, content: x}`,
		`systemd_unit: {name: app.service, state: absent, content: x}`,
		`systemd_unit: {name: app.device, content: x}`, `systemd_unit: {name: app.conf, content: x}`,
		`systemd_unit: {name: backup.timer.d, content: x}`,
	} {
		p := fixture(t, "fleet_stages: [{id: configure, nodes: all, node_tasks: [{"+task+"}]}]\n")
		if _, _, err := Load(p, "example", "", nil); err == nil {
			t.Fatal("accepted", task)
		}
	}
	p := fixture(t, `fleet_stages: [{id: configure, nodes: all, node_tasks: [{directory: {path: "{{ .vars.missing }}"}}]}]`)
	if _, _, err := Load(p, "example", "", nil); err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatal(err)
	}
}

func TestAccountIDReferences(t *testing.T) {
	p := fixture(t, `vars: {uid: 1200}
fleet_stages: [{id: configure, nodes: all, node_tasks: [{user: {create_home_directory: true, name: app, uid: "{{ .vars.uid }}"}}]}]`)
	project, _, err := Load(p, "example", "", nil)
	if err != nil || InputsOf[modules.User](project.FleetStages[0].NodeTasks[0]).UID != "{{ .vars.uid }}" {
		t.Fatal(project, err)
	}
	for _, task := range []string{
		`user: {create_home_directory: true, name: app, uid: "{{ .vars.missing }}"}`,
		`group: {name: app, gid: "{{ .vars.missing }}"}`,
		`systemd_unit: {name: app.service, drop_in: "{{ .vars.missing }}", state: absent}`,
	} {
		p := fixture(t, "fleet_stages: [{id: configure, nodes: all, node_tasks: [{"+task+"}]}]")
		if _, _, err := Load(p, "example", "", nil); err == nil {
			t.Fatal("accepted undeclared reference", task)
		}
	}
}

const aliceKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIORZZg3NlYdxJ28janK/DsJ1O0O2YgF7KN2aiEPskegs alice@laptop"
const bobKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIEhTsh+JVq7mk31K1Fmmykip66tKN98E/piQ6A6iE7gx bob@laptop"

func TestAuthorizedKeysAndUFWConfiguration(t *testing.T) {
	load := func(task string) (Task, error) {
		p := fixture(t, "vars: {key: '"+bobKey+"', port: 8080, office: 192.0.2.0/24, bridge: incusbr0}\nfleet_stages: [{id: configure, nodes: all, node_tasks: [{"+task+"}]}]\n")
		project, _, err := Load(p, "example", "", nil)
		if err != nil {
			return Task{}, err
		}
		return project.FleetStages[0].NodeTasks[0], nil
	}
	for _, task := range []string{
		`authorized_keys: {for_user_account: deploy, keys: ["` + aliceKey + `"], keys_not_listed: keep}`,
		`authorized_keys: {for_user_account: deploy, keys: ['from="10.0.0.0/8",no-agent-forwarding ` + aliceKey + `', "{{ .vars.key }}"], keys_not_listed: keep}`,
		`authorized_keys: {for_user_account: deploy, keys: [], keys_not_listed: remove}`,
		`authorized_keys: {for_user_account: deploy, keys: ["` + aliceKey + `"], state: absent}`,
		`ufw: {default_incoming: deny, default_outgoing: allow, incoming_rules: [{action: limit, port: 22, protocol: tcp}], enabled: true, incoming_rules_not_listed: keep}`,
		`ufw: {incoming_rules: [{action: allow, port: "8000:8010", protocol: udp}, {action: allow, port: 5432, protocol: tcp, from: 10.0.0.0/8}, {protocol: tcp_and_udp, action: deny, port: 25, from: "2001:db8::/32"}, {protocol: tcp_and_udp, action: reject, port: 23, from: 192.0.2.7}], incoming_rules_not_listed: keep}`,
		`ufw: {incoming_rules: [{action: allow, port: "{{ .vars.port }}", protocol: tcp, from: "{{ .vars.office }}"}], incoming_rules_not_listed: keep}`,
		`ufw: {incoming_rules: [], incoming_rules_not_listed: remove}`,
		`ufw: {incoming_rules: [{port: 22, protocol: tcp}], state: absent}`,
		`ufw: {outgoing_rules: [{action: allow, port: 53, protocol: udp}, {protocol: tcp_and_udp, action: allow, to: 10.1.0.0/16}, {protocol: tcp_and_udp, action: reject, outgoing_interface: eth1}], outgoing_rules_not_listed: keep}`,
		`ufw: {outgoing_rules: [], outgoing_rules_not_listed: remove}`,
		`ufw: {outgoing_rules: [{port: 443, protocol: tcp}], state: absent}`,
		`ufw: {outgoing_rules: [{protocol: tcp_and_udp, action: allow, port: "{{ .vars.port }}", to: "{{ .vars.office }}", outgoing_interface: "{{ .vars.bridge }}"}], outgoing_rules_not_listed: keep}`,
		`ufw: {enabled: false}`,
		`ufw: {default_routed: deny, incoming_rules: [{protocol: tcp_and_udp, action: allow, incoming_interface: incusbr0}, {action: allow, incoming_interface: eth1, port: 22, protocol: tcp}], incoming_rules_not_listed: keep, routed_rules: [{protocol: tcp_and_udp, action: allow, incoming_interface: incusbr0}, {protocol: tcp_and_udp, action: allow, outgoing_interface: incusbr0}, {action: deny, incoming_interface: eth1, outgoing_interface: eth0, from: 10.0.0.0/8, to: 192.0.2.7, port: 80, protocol: tcp}, {action: allow, port: 443, protocol: tcp}], routed_rules_not_listed: remove}`,
		`ufw: {routed_rules: [{protocol: tcp_and_udp, incoming_interface: incusbr0}], state: absent}`,
		`ufw: {routed_rules: [{protocol: tcp_and_udp, action: allow, incoming_interface: "{{ .vars.bridge }}", to: "{{ .vars.office }}"}], routed_rules_not_listed: keep}`,
		`ufw: {incoming_rules: [{protocol: udp, action: allow, from: 10.0.0.0/8}, {action: allow, from: 10.0.0.0/8, protocol: tcp}], incoming_rules_not_listed: keep}`,
	} {
		loaded, err := load(task)
		if err != nil {
			t.Fatal(task, err)
		}
		if loaded.Retry == nil || loaded.Retry.Disabled {
			t.Fatal("resource did not inherit retry policy", task)
		}
		b, _ := json.Marshal(loaded)
		var saved Task
		if err := json.Unmarshal(b, &saved); err != nil {
			t.Fatal(err)
		}
		if (InputsOf[modules.AuthorizedKeys](loaded) != nil && InputsOf[modules.AuthorizedKeys](saved).Keys == nil) || (InputsOf[modules.UFW](loaded) != nil && InputsOf[modules.UFW](loaded).IncomingRules != nil && InputsOf[modules.UFW](saved).IncomingRules == nil) {
			t.Fatal("explicit empty list lost during saved retry", task)
		}
		if inputs := InputsOf[modules.UFW](loaded); inputs != nil && inputs.OutgoingRules != nil && InputsOf[modules.UFW](saved).OutgoingRules == nil {
			t.Fatal("explicit outgoing_rules list lost during saved retry", task)
		}
	}
	for task, want := range map[string]string{
		`authorized_keys: {for_user_account: deploy}`:                                                                                "needs keys, key_files_on_controller_machine, or key_directories_on_controller_machine",
		`authorized_keys: {for_user_account: "-x", keys: [], keys_not_listed: keep}`:                                                 "invalid account name",
		`authorized_keys: {for_user_account: deploy, keys: ["ssh-ed25519 not-base64"], keys_not_listed: keep}`:                       "OpenSSH key line",
		`authorized_keys: {for_user_account: deploy, keys: ["# comment"], keys_not_listed: keep}`:                                    "OpenSSH key line",
		`authorized_keys: {for_user_account: deploy, keys: ["` + aliceKey + `\n` + bobKey + `"], keys_not_listed: keep}`:             "OpenSSH key line",
		`authorized_keys: {for_user_account: deploy, keys: ["` + aliceKey + `", "no-pty ` + aliceKey + `2"], keys_not_listed: keep}`: "listed twice",
		`authorized_keys: {for_user_account: deploy, keys: [], keys_not_listed: merge}`:                                              "keys_not_listed",
		`authorized_keys: {for_user_account: deploy, keys: [], keys_not_listed: keep, state: absent}`:                                "accepts only",
		`authorized_keys: {for_user_account: deploy, keys: []}`:                                                                      "keys_not_listed is required",
		`ufw: {}`: "needs incoming_rules, routed_rules",
		`ufw: {incoming_rules: [{protocol: tcp_and_udp, action: allow, port: 22}]}`:                                                     "incoming_rules_not_listed is required",
		`ufw: {incoming_rules: [], incoming_rules_not_listed: replace}`:                                                                 "incoming_rules_not_listed must be remove or keep",
		`ufw: {incoming_rules: [{protocol: tcp_and_udp, action: permit, port: 22}], incoming_rules_not_listed: keep}`:                   "action must be",
		`ufw: {incoming_rules: [{protocol: tcp_and_udp, port: 22}], incoming_rules_not_listed: keep}`:                                   "action must be",
		`ufw: {incoming_rules: [{protocol: tcp_and_udp, action: allow}], incoming_rules_not_listed: keep}`:                              "requires at least one of port, from, and incoming_interface",
		`ufw: {incoming_rules: [{protocol: tcp_and_udp, action: allow, port: 0}], incoming_rules_not_listed: keep}`:                     "1 to 65535",
		`ufw: {incoming_rules: [{protocol: tcp_and_udp, action: allow, port: 70000}], incoming_rules_not_listed: keep}`:                 "1 to 65535",
		`ufw: {incoming_rules: [{protocol: tcp_and_udp, action: allow, port: "22/tcp"}], incoming_rules_not_listed: keep}`:              "1 to 65535",
		`ufw: {incoming_rules: [{action: allow, port: "8000:8010"}], incoming_rules_not_listed: keep}`:                                  "protocol is required",
		`ufw: {incoming_rules: [{action: allow, port: "10:5", protocol: tcp}], incoming_rules_not_listed: keep}`:                        "first below last",
		`ufw: {incoming_rules: [{action: allow, port: 22, protocol: icmp}], incoming_rules_not_listed: keep}`:                           "protocol must be",
		`ufw: {incoming_rules: [{protocol: tcp_and_udp, action: allow, port: 22, from: 10.0.0.5/8}], incoming_rules_not_listed: keep}`:  "network such as",
		`ufw: {incoming_rules: [{protocol: tcp_and_udp, action: allow, port: 22, from: example.com}], incoming_rules_not_listed: keep}`: "network such as",
		`ufw: {default_incoming: drop}`: "default_incoming must be",
		`ufw: {incoming_rules: [{protocol: tcp_and_udp, action: allow, port: 22}, {protocol: tcp_and_udp, action: deny, port: "022"}], incoming_rules_not_listed: keep}`: "listed twice",
		`ufw: {incoming_rules: [{protocol: tcp_and_udp, action: allow, port: 22}], enabled: true, state: absent}`:                                                        "accepts only incoming_rules, routed_rules, and outgoing_rules",
		`ufw: {incoming_rules: [], state: absent}`:                                                                                                                       "accepts only incoming_rules, routed_rules, and outgoing_rules",
		`ufw: {enabled: true, incoming_rules_not_listed: remove}`:                                                                                                        "requires incoming_rules",
		`ufw: {incoming_rules: [{action: allow, port: "{{ .vars.missing }}", protocol: tcp}], incoming_rules_not_listed: keep}`:                                          "missing",
		`ufw: {routed_rules: [{protocol: tcp_and_udp, action: allow, incoming_interface: incusbr0}]}`:                                                                    "routed_rules_not_listed is required",
		`ufw: {routed_rules: [{protocol: tcp_and_udp, action: allow}], routed_rules_not_listed: keep}`:                                                                   "at least one of incoming_interface",
		`ufw: {routed_rules: [{protocol: tcp_and_udp, action: allow, to: 10.0.0.5/8}], routed_rules_not_listed: keep}`:                                                   "to must be an address",
		`ufw: {routed_rules: [{protocol: tcp_and_udp, action: allow, incoming_interface: "eth0:1"}], routed_rules_not_listed: keep}`:                                     "network interface name",
		`ufw: {incoming_rules: [{protocol: tcp_and_udp, action: allow, incoming_interface: "a-very-long-interface"}], incoming_rules_not_listed: keep}`:                  "network interface name",
		`ufw: {routed_rules: [{action: allow, port: 443, protocol: tcp}, {action: deny, port: 443, protocol: tcp}], routed_rules_not_listed: keep}`:                      "routed rule listed twice",
		`ufw: {outgoing_rules: [{protocol: tcp_and_udp, action: allow, port: 53}]}`:                                                                                      "outgoing_rules_not_listed is required",
		`ufw: {outgoing_rules: [], outgoing_rules_not_listed: merge}`:                                                                                                    "outgoing_rules_not_listed must be remove or keep",
		`ufw: {enabled: true, outgoing_rules_not_listed: remove}`:                                                                                                        "requires outgoing_rules",
		`ufw: {outgoing_rules: [{protocol: tcp_and_udp, action: allow}], outgoing_rules_not_listed: keep}`:                                                               "needs port, to, outgoing_interface",
		`ufw: {outgoing_rules: [{protocol: tcp_and_udp, port: 53}], outgoing_rules_not_listed: keep}`:                                                                    "action must be",
		`ufw: {outgoing_rules: [{protocol: tcp_and_udp, action: permit, port: 53}], outgoing_rules_not_listed: keep}`:                                                    "action must be",
		`ufw: {outgoing_rules: [{action: allow, port: 53, protocol: icmp}], outgoing_rules_not_listed: keep}`:                                                            "protocol must be",
		`ufw: {outgoing_rules: [{protocol: tcp_and_udp, action: allow, port: 0}], outgoing_rules_not_listed: keep}`:                                                      "1 to 65535",
		`ufw: {outgoing_rules: [{action: allow, port: "8000:8100"}], outgoing_rules_not_listed: keep}`:                                                                   "protocol is required",
		`ufw: {outgoing_rules: [{action: allow, port: "10:5", protocol: tcp}], outgoing_rules_not_listed: keep}`:                                                         "first below last",
		`ufw: {outgoing_rules: [{protocol: tcp_and_udp, action: allow, to: example.com}], outgoing_rules_not_listed: keep}`:                                              "to must be an address",
		`ufw: {outgoing_rules: [{protocol: tcp_and_udp, action: allow, outgoing_interface: "eth0:1"}], outgoing_rules_not_listed: keep}`:                                 "network interface name",
		`ufw: {outgoing_rules: [{protocol: tcp_and_udp, action: allow, port: 53}, {protocol: tcp_and_udp, action: deny, port: "053"}], outgoing_rules_not_listed: keep}`: "outgoing rule listed twice",
		`ufw: {outgoing_rules: [{protocol: tcp_and_udp, port: 53}], outgoing_rules_not_listed: keep, state: absent}`:                                                     "accepts only",
		`ufw: {outgoing_rules: [], state: absent}`:                                                                                                                       "needs at least one rule",
		`ufw: {outgoing_rules: [{protocol: tcp_and_udp, action: allow, from: 10.0.0.5}], outgoing_rules_not_listed: keep}`:                                               "unknown",
		`ufw: {outgoing_rules: [{action: allow, from_inventory_group: {group: app, address_var: private_address}}], outgoing_rules_not_listed: keep}`:                    "unknown",
		`ufw: {default_routed: drop}`: "default_routed must be",
		`ufw: {routed_rules: [{protocol: tcp_and_udp, incoming_interface: incusbr0}], default_routed: allow, state: absent}`: "accepts only incoming_rules, routed_rules, and outgoing_rules",
	} {
		if _, err := load(task); err == nil || !strings.Contains(err.Error(), want) {
			t.Error(task, err)
		}
	}
}

func TestUFWRuleTargetsMatchUFW(t *testing.T) {
	// ufw show added prints rules in these forms (ufw 0.36).
	for rule, want := range map[modules.UFWRule]string{
		{Port: "22", Proto: "tcp"}:                            "22/tcp",
		{Port: "80"}:                                          "80",
		{Port: "0443"}:                                        "443",
		{Port: "8000:8010", Proto: "udp"}:                     "8000:8010/udp",
		{Port: "5432", Proto: "tcp", From: "10.0.0.0/8"}:      "from 10.0.0.0/8 to any port 5432 proto tcp",
		{Port: "53", From: "10.0.0.1/32"}:                     "from 10.0.0.1 to any port 53",
		{Port: "22", Proto: "tcp", From: "2001:DB8::1"}:       "from 2001:db8::1 to any port 22 proto tcp",
		{From: "10.0.0.0/8"}:                                  "from 10.0.0.0/8",
		{From: "192.0.2.0/24", Proto: "udp"}:                  "from 192.0.2.0/24 proto udp",
		{IncomingInterface: "incusbr0"}:                       "in on incusbr0",
		{IncomingInterface: "eth9", Proto: "tcp"}:             "in on eth9 proto tcp",
		{IncomingInterface: "eth9", Port: "22", Proto: "tcp"}: "in on eth9 to any port 22 proto tcp",
		{IncomingInterface: "eth9", Port: "5432", Proto: "tcp", From: "10.0.0.0/8"}: "in on eth9 from 10.0.0.0/8 to any port 5432 proto tcp",
	} {
		if got, err := rule.Target(); err != nil || got != want {
			t.Errorf("%+v: %q %v", rule, got, err)
		}
	}
	// ufw show added prints routed rules in these forms, after "route ACTION" (ufw 0.36).
	for rule, want := range map[modules.UFWRoutedRule]string{
		{IncomingInterface: "incusbr0"}:                        "in on incusbr0",
		{OutgoingInterface: "incusbr0"}:                        "out on incusbr0",
		{IncomingInterface: "eth9", OutgoingInterface: "eth8"}: "in on eth9 out on eth8",
		{IncomingInterface: "eth9", OutgoingInterface: "eth8", To: "10.1.2.3", Port: "80", Proto: "tcp"}: "in on eth9 out on eth8 to 10.1.2.3 port 80 proto tcp",
		{IncomingInterface: "eth9", OutgoingInterface: "eth8", From: "10.0.0.0/8", Port: "80"}:           "in on eth9 out on eth8 from 10.0.0.0/8 to any port 80",
		{From: "10.0.0.0/8", To: "10.1.0.0/16"}:                                                          "from 10.0.0.0/8 to 10.1.0.0/16",
		{From: "10.0.0.0/8", To: "10.1.0.0/16", Port: "80", Proto: "tcp"}:                                "from 10.0.0.0/8 to 10.1.0.0/16 port 80 proto tcp",
		{From: "10.0.0.0/8"}: "from 10.0.0.0/8",
		{To: "10.1.0.0/16"}:  "to 10.1.0.0/16",
		{OutgoingInterface: "eth8", To: "192.168.0.0/16", Port: "53"}: "out on eth8 to 192.168.0.0/16 port 53",
		{IncomingInterface: "eth9", From: "10.0.0.5"}:                 "in on eth9 from 10.0.0.5",
		{Port: "443", Proto: "tcp"}:                                   "443/tcp",
	} {
		if got, err := rule.Target(); err != nil || got != want {
			t.Errorf("%+v: %q %v", rule, got, err)
		}
	}
}

func TestResolvedKeysAndPorts(t *testing.T) {
	task := Task{Module: "authorized_keys", ModuleInputs: &modules.AuthorizedKeys{ForUserAccount: "deploy", Keys: []string{"{{ .vars.key }}"}}}
	resolved, err := ResolveArguments(task, value.Scope{"vars": map[string]any{"key": bobKey}})
	if err != nil || InputsOf[modules.AuthorizedKeys](resolved).Keys[0] != bobKey || InputsOf[modules.AuthorizedKeys](task).Keys[0] != "{{ .vars.key }}" {
		t.Fatal(InputsOf[modules.AuthorizedKeys](resolved), err)
	}
	if _, err := ResolveArguments(task, value.Scope{"vars": map[string]any{"key": "not a key"}}); err == nil || !strings.Contains(err.Error(), "authorized_keys.keys[0]") {
		t.Fatal(err)
	}
	ufw := Task{Module: "ufw", ModuleInputs: &modules.UFW{IncomingRules: []modules.UFWRule{{Proto: "tcp_and_udp", Action: "allow", Port: "{{ .vars.port }}"}}}}
	resolved, err = ResolveArguments(ufw, value.Scope{"vars": map[string]any{"port": 8080}})
	if err != nil || InputsOf[modules.UFW](resolved).IncomingRules[0].Port != "8080" || InputsOf[modules.UFW](ufw).IncomingRules[0].Port != "{{ .vars.port }}" {
		t.Fatal(InputsOf[modules.UFW](resolved), err)
	}
	// The explicit tcp_and_udp choice also accepts a resolved range.
	if _, err := ResolveArguments(ufw, value.Scope{"vars": map[string]any{"port": "8000:8010"}}); err != nil {
		t.Fatal(err)
	}
}

func TestUFWOutgoingArguments(t *testing.T) {
	task := Task{Module: "ufw", ModuleInputs: &modules.UFW{OutgoingRulesNotListed: "keep", OutgoingRules: []modules.UFWOutgoingRule{{Action: "allow", Port: "{{ .vars.port }}", To: "{{ .vars.destination }}", OutgoingInterface: "{{ .vars.interface }}", Proto: "tcp"}}}}
	scope := value.Scope{"vars": map[string]any{"port": 443, "destination": "10.0.0.5", "interface": "eth0"}}
	resolved, err := ResolveArguments(task, scope)
	if err != nil {
		t.Fatal(err)
	}
	rule := InputsOf[modules.UFW](resolved).OutgoingRules[0]
	if rule.Port != "443" || rule.To != "10.0.0.5" || rule.OutgoingInterface != "eth0" || InputsOf[modules.UFW](task).OutgoingRules[0].To != "{{ .vars.destination }}" {
		t.Fatal(rule)
	}
	for field, invalid := range map[string]any{"port": 70000, "destination": "example.com", "interface": "eth0:1"} {
		scope["vars"].(map[string]any)[field] = invalid
		if _, err := ResolveArguments(task, scope); err == nil {
			t.Fatalf("accepted invalid %s", field)
		}
		scope["vars"].(map[string]any)[field] = map[string]any{"port": 443, "destination": "10.0.0.5", "interface": "eth0"}[field]
	}
}
