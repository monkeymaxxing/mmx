package config

import (
	"context"
	"encoding/json"
	"fmt"
	"mmx/internal/modules"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

func TestConnectionInputsAreValidatedWithoutLoadingCredentials(t *testing.T) {
	path := fixture(t, "secrets_from_env: {login: MMX_TEST_LOGIN, privilege: MMX_TEST_SUDO}\nfleet_stages: [{id: setup, nodes: all, node_tasks: [{show: ready}]}]\n")
	t.Setenv("MMX_TEST_LOGIN", "must-not-enter-inventory")
	for _, managedNode := range []string{
		"{address: example.com, ssh_password: '{{ .secrets.login }}', sudo_password: '{{ .secrets.privilege }}'}",
		"{address: example.com, connection: ssh_only}",
		"{address: example.com, ssh_password: '{{ .secrets.vault_login }}', sudo_password: '{{ .secrets.vault_privilege }}'}",
		"{address: example.com, connection: https_with_ssh_fallback, https_address: 'https://[::1]:7443/'}",
		"{address: example.com, mmx_runner_download_url: 'http://mirror.example.com:8081/mmx/'}",
		"{address: example.com, mmx_runner_download_url: 'https://mirror.example.com'}",
	} {
		write(t, filepath.Join(filepath.Dir(path), "inventories/example.yml"), "nodes: {web: "+managedNode+"}\n")
		p, inv, err := Load(path, "example", "", nil)
		if err != nil {
			t.Fatal(err)
		}
		data, err := json.Marshal([]any{p, inv})
		if err != nil || strings.Contains(string(data), "must-not-enter-inventory") {
			t.Fatal("configuration evaluated connection credentials", err)
		}
	}
	for _, field := range []string{
		"ssh_password: bad/name", "sudo_password: bad/name", "connection: https",
		"https_address: 'http://example.com'", "https_address: 'https://user:password@example.com'",
		"https_address: 'https://example.com/path'", "https_address: 'https://example.com?query=1'",
		"https_address: 'https://example.com#fragment'",
		"mmx_runner_download_url: 'ftp://mirror.example.com'", "mmx_runner_download_url: 'mirror.example.com/mmx'",
		"mmx_runner_download_url: 'https://user:password@mirror.example.com'", "mmx_runner_download_url: 'https://mirror.example.com?token=1'",
		"mmx_runner_download_url: 'https://mirror.example.com/#fragment'", "mmx_runner_download_url: 'https://mirror.example.com/a b'",
	} {
		write(t, filepath.Join(filepath.Dir(path), "inventories/example.yml"), "nodes: {web: {address: example.com, "+field+"}}\n")
		if _, _, err := Load(path, "example", "", nil); err == nil {
			t.Fatalf("accepted invalid connection field %s", field)
		}
	}
}

func TestStrictYAML(t *testing.T) {
	cases := map[string]string{
		"duplicate":                   "project_name: one\nproject_name: two\n",
		"unknown":                     "project_name: one\nmagic: true\n",
		"anchor":                      "vars: &v {x: 1}\n",
		"alias":                       "vars: {a: &x hi, b: *x}\n",
		"merge":                       "vars: {<<: {x: 1}}\n",
		"tag":                         "project_name: !!str hi\n",
		"multiple":                    "project_name: one\n---\nproject_name: two\n",
		"legacy boolean":              "allow_role_calls_within_roles: yes\n",
		"numeric string":              "project_name: 123\n",
		"numeric mode":                "fleet_stages: [{node_tasks: [{file: {path: /tmp/a, content: hi, permissions_mode: 0644}}]}]\n",
		"null boolean":                "allow_role_calls_within_roles: null\n",
		"nonstring key":               "vars: {1: x}\n",
		"guard unknown":               "fleet_stages: [{node_tasks: [{shell: {script: true, run_only_if_this_command_succeeds: {script: check, magic: 1}}}]}]\n",
		"task block scalar":           "fleet_stages: [{node_tasks: [{task_block: configure}]}]\n",
		"task block unknown phase":    "fleet_stages: [{node_tasks: [{task_block: {id: configure, changes: [{show: ready}], magic: []}}]}]\n",
		"task block nested condition": "fleet_stages: [{node_tasks: [{task_block: {id: configure, when: false, changes: [{show: ready}]}}]}]\n",
		"task block duplicate phase":  "fleet_stages: [{node_tasks: [{task_block: {id: configure, changes: [{show: first}], changes: [{show: second}]}}]}]\n",
	}
	for name, text := range cases {
		t.Run(name, func(t *testing.T) {
			var definition any = &Project{}
			if strings.HasPrefix(text, "fleet_stages:") {
				definition = &Playbook{}
			}
			if err := Decode("fixture", []byte(text), definition); err == nil {
				t.Fatal("accepted invalid configuration")
			}
		})
	}
	var p Project
	if err := Decode("valid", []byte("project_name: yes\nvars: {word: on, count: 3, enabled: true}\n"), &p); err != nil {
		t.Fatal(err)
	}
	if p.Project != "yes" || p.Vars["word"] != "on" {
		t.Fatalf("unexpected YAML coercion: %#v", p)
	}
}

// Every load error names the project-relative file, line, column, and field
// that caused it, never a controller path.
func TestLoadErrorsNameTheirFileAndField(t *testing.T) {
	fleetStage := func(nodeTasks string) string {
		return "fleet_stages:\n- id: s\n  nodes: all\n  node_tasks:\n  - " + nodeTasks + "\n"
	}
	role := func(name, text string) map[string]string {
		return map[string]string{"roles/" + name + "/main.yml": text}
	}
	for _, tc := range []struct {
		name, project string
		files         map[string]string
		file, field   string
	}{
		{"project name", "project_name: 1x\n" + fleetStage("show: a"), nil, "mmx_project_settings.yml", "project_name"},
		{"history retention", "run_history_retention: soon\n" + fleetStage("show: a"), nil, "mmx_project_settings.yml", "run_history_retention"},
		{"secret input", "secrets_from_env: {token: bad-name}\n" + fleetStage("show: a"), nil, "mmx_project_settings.yml", "secrets_from_env.token"},
		{"nodes at once", "max_nodes_at_once: 0\n" + fleetStage("show: a"), nil, "mmx_project_settings.yml", "max_nodes_at_once"},
		{"no fleet_stages", "fleet_stages: []\n", nil, "playbooks/example.yml", "fleet_stages"},
		{"node address", fleetStage("show: a"), map[string]string{"inventories/example.yml": "nodes: {web1: {address: '-x'}}\n"}, "inventories/example.yml", "nodes.web1.address"},
		{"node port", fleetStage("show: a"), map[string]string{"inventories/example.yml": "nodes: {web1: {address: a, ssh_port: 70000}}\n"}, "inventories/example.yml", "nodes.web1.ssh_port"},
		{"node secret", fleetStage("show: a"), map[string]string{"inventories/example.yml": "nodes: {web1: {address: a, ssh_password: bad/name}}\n"}, "inventories/example.yml", "nodes.web1.ssh_password"},
		{"node transport", fleetStage("show: a"), map[string]string{"inventories/example.yml": "nodes: {web1: {address: a, connection: https}}\n"}, "inventories/example.yml", "nodes.web1.connection"},
		{"no nodes", fleetStage("show: a"), map[string]string{"inventories/example.yml": "nodes: {}\n"}, "inventories/example.yml", "nodes"},
		{"group name", fleetStage("show: a"), map[string]string{"inventories/example.yml": "nodes: {web1: {address: a}}\ngroups: {web1: {nodes: [web1]}}\n"}, "inventories/example.yml", "groups.web1"},
		{"group member", fleetStage("show: a"), map[string]string{"inventories/example.yml": "nodes: {web1: {address: a}}\ngroups: {web: {nodes: [web1, nothere]}}\n"}, "inventories/example.yml", "groups.web.nodes[1]"},
		{"repeated group member", fleetStage("show: a"), map[string]string{"inventories/example.yml": "nodes: {web1: {address: a}}\ngroups: {web: {nodes: [web1, web1]}}\n"}, "inventories/example.yml", "groups.web.nodes[1]"},
		{"plain group list", fleetStage("show: a"), map[string]string{"inventories/example.yml": "nodes: {web1: {address: a}}\ngroups: {web: [web1]}\n"}, "inventories/example.yml", "groups.web"},
		{"group var overlap", fleetStage("show: a"), map[string]string{"inventories/example.yml": "nodes: {web1: {address: a}}\ngroups:\n  a: {nodes: [web1], vars: {port: 80}}\n  b: {nodes: [web1], vars: {port: 80}}\n"}, "inventories/example.yml", "groups.b.vars.port"},
		{"command-line name", "command_line_vars: {1x: {type: string}}\n" + fleetStage("show: a"), nil, "playbooks/example.yml", "command_line_vars.1x"},
		{"command-line type", "command_line_vars: {release: {type: list}}\n" + fleetStage("show: a"), nil, "playbooks/example.yml", "command_line_vars.release.type"},
		{"command-line default type", "command_line_vars: {count: {type: integer, default: '4'}}\n" + fleetStage("show: a"), nil, "playbooks/example.yml", "command_line_vars.count.default"},
		{"command-line default reference", "command_line_vars: {release: {type: string, default: '{{ .secrets.token }}'}}\n" + fleetStage("show: a"), nil, "playbooks/example.yml", "command_line_vars.release.default"},
		{"command-line secret", "command_line_vars: {release: {type: string, secret: true}}\n" + fleetStage("show: a"), nil, "playbooks/example.yml", "command_line_vars.release.secret"},
		{"command-line also in vars", "command_line_vars: {release: {type: string}}\n" + fleetStage("show: a"), map[string]string{"inventories/example.yml": "nodes: {web1: {address: a}}\ngroups: {web: {nodes: [web1], vars: {release: x}}}\n"}, "playbooks/example.yml", "command_line_vars.release"},
		{"missing inventory", fleetStage("show: a"), map[string]string{"inventories/example.yml": ""}, "mmx_project_settings.yml", "default_inventory"},
		{"fleet stage id", "fleet_stages:\n- id: 1s\n  nodes: all\n  node_tasks: [{show: a}]\n", nil, "playbooks/example.yml", "fleet_stages[0].id"},
		{"duplicate fleet_stage", "fleet_stages:\n- {id: s, nodes: all, node_tasks: [{show: a}]}\n- {id: s, nodes: all, node_tasks: [{show: a}]}\n", nil, "playbooks/example.yml", "fleet_stages[1].id"},
		{"node failure policy", "fleet_stages:\n- id: s\n  nodes: all\n  on_node_failure: stop\n  node_tasks: [{show: a}]\n", nil, "playbooks/example.yml", "fleet_stages[0].on_node_failure"},
		{"allowance without a stopping policy", "fleet_stages:\n- id: s\n  nodes: all\n  failed_nodes_allowed: 2\n  node_tasks: [{show: a}]\n", nil, "playbooks/example.yml", "fleet_stages[0].failed_nodes_allowed"},
		{"fractional allowance", "fleet_stages:\n- id: s\n  nodes: all\n  on_node_failure: stop_fleet_stage\n  failed_nodes_allowed: 2.5%\n  node_tasks: [{show: a}]\n", nil, "playbooks/example.yml", "fleet_stages[0].failed_nodes_allowed"},
		{"allowance over 100%", "fleet_stages:\n- id: s\n  nodes: all\n  on_node_failure: stop_fleet_stage\n  failed_nodes_allowed: 101%\n  node_tasks: [{show: a}]\n", nil, "playbooks/example.yml", "fleet_stages[0].failed_nodes_allowed"},
		{"negative allowance", "fleet_stages:\n- id: s\n  nodes: all\n  on_node_failure: stop_run\n  failed_nodes_allowed: -1\n  node_tasks: [{show: a}]\n", nil, "playbooks/example.yml", "fleet_stages[0].failed_nodes_allowed"},
		{"fleet stage loop allowance", "fleet_stages:\n- id: s\n  loop: [a]\n  as: x\n  key: x\n  failed_nodes_allowed: 1\n  fleet_stages: [{id: t, nodes: all, node_tasks: [{show: a}]}]\n", nil, "playbooks/example.yml", "fleet_stages[0].loop"},
		{"fleet stage nodes", "fleet_stages:\n- id: s\n  nodes: [web1, nothere]\n  node_tasks: [{show: a}]\n", nil, "playbooks/example.yml", "fleet_stages[0].nodes"},
		{"fleet stage requirement", "fleet_stages:\n- id: s\n  nodes: all\n  requires: {same_node: [later]}\n  node_tasks: [{show: a}]\n", nil, "playbooks/example.yml", "fleet_stages[0].requires"},
		{"fleet stage without work", "fleet_stages:\n- id: s\n  nodes: all\n", nil, "playbooks/example.yml", "fleet_stages[0]"},
		{"fleet stage key", "fleet_stages:\n- id: s\n  nodes: all\n  key: x\n  node_tasks: [{show: a}]\n", nil, "playbooks/example.yml", "fleet_stages[0]"},
		{"fleet stage loop name", "fleet_stages:\n- id: s\n  loop: [a]\n  as: vars\n  key: x\n  fleet_stages: [{id: t, nodes: all, node_tasks: [{show: a}]}]\n", nil, "playbooks/example.yml", "fleet_stages[0].as"},
		{"fleet stage loop policy", "fleet_stages:\n- id: s\n  loop: [a]\n  as: x\n  key: x\n  nodes: all\n  fleet_stages: [{id: t, nodes: all, node_tasks: [{show: a}]}]\n", nil, "playbooks/example.yml", "fleet_stages[0].loop"},
		{"fleet stage output", "fleet_stages:\n- id: s\n  nodes: all\n  outputs: {x: '{{ .node_tasks.nope.value }}'}\n", nil, "playbooks/example.yml", "fleet_stages[0].outputs.x"},
		{"inputs without call", fleetStage("show: a\n    inputs: {x: 1}"), nil, "playbooks/example.yml", "fleet_stages[0].node_tasks[0].inputs"},
		{"module name", fleetStage("external_module: bad.name"), nil, "playbooks/example.yml", "fleet_stages[0].node_tasks[0].external_module"},
		{"missing module", fleetStage("external_module: absent"), nil, "playbooks/example.yml", "fleet_stages[0].node_tasks[0].external_module"},
		{"as without loop", fleetStage("show: a\n    as: x"), nil, "playbooks/example.yml", "fleet_stages[0].node_tasks[0]"},
		{"loop name", fleetStage("show: a\n    loop: [1, 2]\n    as: vars"), nil, "playbooks/example.yml", "fleet_stages[0].node_tasks[0].as"},
		{"task_block loop key", fleetStage("task_block:\n      id: op\n      changes: [{show: a}]\n    loop: [1]\n    as: x\n"), nil, "playbooks/example.yml", "fleet_stages[0].node_tasks[0]"},
		{"duplicate identity", fleetStage("show: a\n  - show: b\n    id: task_1"), nil, "playbooks/example.yml", "fleet_stages[0].node_tasks[1].id"},
		{"task_block missing id", fleetStage("task_block: {changes: [{show: a}]}"), nil, "playbooks/example.yml", "fleet_stages[0].node_tasks[0].task_block"},
		{"task_block name", fleetStage("task_block:\n      id: bad.name\n      changes: [{show: a}]"), nil, "playbooks/example.yml", "fleet_stages[0].node_tasks[0].task_block.id"},
		{"task_block changes", fleetStage("task_block:\n      id: op\n      changes: []\n      activate: [{show: a}]"), nil, "playbooks/example.yml", "fleet_stages[0].node_tasks[0].task_block.changes"},
		{"stage without task_block", fleetStage("show: a\n    verify: [{show: b}]"), nil, "playbooks/example.yml", "fleet_stages[0].node_tasks[0].verify"},
		{"role in stage", fleetStage("task_block:\n      id: op\n      changes: [{role: app}]"), role("app", "node_tasks: [{show: a}]\n"), "playbooks/example.yml", "fleet_stages[0].node_tasks[0].task_block.changes[0]"},
		{"task_block validator", fleetStage("role: b"), role("b", "node_tasks:\n- task_block:\n    id: op\n    changes: [{show: x}]\n    validate: [{file: {permissions_mode: '0644', path: /x, content: y}}]\n"), "roles/b/main.yml", "node_tasks[0].task_block.validate[0]"},
		{"variable name", fleetStage("set_vars: {1x: y}"), nil, "playbooks/example.yml", "fleet_stages[0].node_tasks[0].set_vars.1x"},
		{"missing role", fleetStage("role: absent"), nil, "playbooks/example.yml", "fleet_stages[0].node_tasks[0].role"},
		{"role name", fleetStage("role: bad.name"), nil, "playbooks/example.yml", "fleet_stages[0].node_tasks[0].role"},
		{"nested role", fleetStage("role: outer"), map[string]string{"roles/outer/main.yml": "node_tasks: [{role: inner}]\n", "roles/inner/main.yml": "node_tasks: [{show: a}]\n"}, "roles/outer/main.yml", "node_tasks[0].role"},
		{"role without node_tasks", fleetStage("role: app"), role("app", "node_tasks: []\n"), "roles/app/main.yml", "node_tasks"},
		{"role input type", fleetStage("role: app"), role("app", "inputs: {port: {type: number}}\nnode_tasks: [{show: a}]\n"), "roles/app/main.yml", "inputs.port.type"},
		{"role input name", fleetStage("role: app"), role("app", "inputs: {1port: {type: string}}\nnode_tasks: [{show: a}]\n"), "roles/app/main.yml", "inputs.1port"},
		{"secret default", fleetStage("role: app"), role("app", "inputs: {token: {type: string, secret: true, default: x}}\nnode_tasks: [{show: a}]\n"), "roles/app/main.yml", "inputs.token.default"},
		{"plaintext secret input", fleetStage("role: app\n    inputs: {token: x}"), role("app", "inputs: {token: {type: string, secret: true}}\nnode_tasks: [{show: a}]\n"), "playbooks/example.yml", "fleet_stages[0].node_tasks[0].inputs.token"},
		{"role output", fleetStage("role: app"), role("app", "node_tasks: [{show: a}]\noutputs: {x: '{{ .node_tasks.nope.value }}'}\n"), "roles/app/main.yml", "outputs.x"},
		{"missing source", fleetStage("file: {permissions_mode: '0644', path: /etc/x, source: files/nothere}"), nil, "playbooks/example.yml", "fleet_stages[0].node_tasks[0].file.source"},
		{"missing template", fleetStage("file: {permissions_mode: '0644', path: /etc/x, template: templates/nothere.tmpl}"), nil, "playbooks/example.yml", "fleet_stages[0].node_tasks[0].file.template"},
		{"missing unit template", fleetStage("systemd_unit: {name: app.service, template: nothere.tmpl}"), nil, "playbooks/example.yml", "fleet_stages[0].node_tasks[0].systemd_unit.template"},
		{"missing directory source", fleetStage("directory: {path: /srv/app, source: nothere}"), nil, "playbooks/example.yml", "fleet_stages[0].node_tasks[0].directory.source"},
		{"assert without that", fleetStage("assert: {message: m}"), nil, "playbooks/example.yml", "fleet_stages[0].node_tasks[0].assert"},
		{"assert predicate", fleetStage("assert: {that: {magic: true}}"), nil, "playbooks/example.yml", "fleet_stages[0].node_tasks[0].assert.that"},
		{"assert message", fleetStage("assert: {that: true, message: '{{ .vars.nope }}'}"), nil, "playbooks/example.yml", "fleet_stages[0].node_tasks[0].assert.message"},
		{"assert retry", fleetStage("assert: {that: true}\n    retry: false"), nil, "playbooks/example.yml", "fleet_stages[0].node_tasks[0]"},
		{"reboot before another task", fleetStage("reboot: {}\n  - show: a"), nil, "playbooks/example.yml", "fleet_stages[0].node_tasks[0].reboot"},
		{"reboot in role", fleetStage("role: app"), role("app", "node_tasks: [{reboot: {}}]\n"), "roles/app/main.yml", "node_tasks[0].reboot"},
		{"reboot in task_block", fleetStage("task_block:\n      id: op\n      changes: [{reboot: {}}]"), nil, "playbooks/example.yml", "fleet_stages[0].node_tasks[0].task_block.changes[0].reboot"},
		{"reboot loop", fleetStage("reboot: {}\n    loop: [1]\n    as: x"), nil, "playbooks/example.yml", "fleet_stages[0].node_tasks[0].reboot"},
		{"reboot retry", fleetStage("reboot: {}\n    retry: false"), nil, "playbooks/example.yml", "fleet_stages[0].node_tasks[0]"},
		{"reboot task timeout", fleetStage("reboot: {}\n    time_limit: 1m"), nil, "playbooks/example.yml", "fleet_stages[0].node_tasks[0]"},
		{"reboot timeout", fleetStage("reboot: {wait_at_most: soon}"), nil, "playbooks/example.yml", "fleet_stages[0].node_tasks[0].reboot.wait_at_most"},
		{"reboot guard", fleetStage("reboot: {run_only_if_this_command_succeeds: {script: check, true_exit_codes: [1]}}"), nil, "playbooks/example.yml", "fleet_stages[0].node_tasks[0].reboot"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			project := tc.project
			if !strings.HasPrefix(project, "project_name:") {
				project = "project_name: test\n" + project
			}
			dir := t.TempDir()
			write(t, filepath.Join(dir, "inventories/example.yml"), "nodes:\n  web1: {address: example.com}\n")
			write(t, filepath.Join(dir, "mmx_project_settings.yml"), project+"default_inventory: example\n")
			for name, text := range tc.files {
				if name == "inventories/example.yml" && text == "" {
					os.Remove(filepath.Join(dir, name))
					continue
				}
				write(t, filepath.Join(dir, name), text)
			}
			_, _, err := Load(filepath.Join(dir, "mmx_project_settings.yml"), "example", "", nil)
			if err == nil || !strings.Contains(err.Error(), tc.file+":") || !strings.Contains(err.Error(), "("+tc.field+")") {
				t.Fatalf("want %s (%s): %v", tc.file, tc.field, err)
			}
		})
	}
}

// mmx run-command keeps the project's settings and inventory and replaces the
// playbook, which it neither needs nor validates, with one shell fleet stage.
func TestLoadRunCommandBuildsOneShellFleetStage(t *testing.T) {
	path := fixture(t, "max_nodes_at_once: 7\nnode_task_defaults: {time_limit: 5m}\nfleet_stages: [{id: app, nodes: all, node_tasks: [{role: missing}]}]\n")
	if _, _, err := Load(path, "example", "", nil); err == nil {
		t.Fatal("fixture playbook should not load")
	}
	p, inv, err := LoadRunCommand(path, "", []string{"web1", "web"}, "uptime && id", "app")
	if err != nil {
		t.Fatal(err)
	}
	if len(inv.Nodes) != 2 || p.MaxNodesAtOnce != 7 || len(p.FleetStages) != 1 {
		t.Fatal(p, inv)
	}
	fleetStage := p.FleetStages[0]
	task := fleetStage.NodeTasks[0]
	if fleetStage.ID != "run-command" || fleetStage.OnNodeFailure != "continue" || len(fleetStage.NodeTasks) != 1 || task.ID != "command" ||
		InputsOf[modules.Shell](task).Script != "uptime && id" || InputsOf[modules.Shell](task).User != "app" || task.TimeLimit == nil || *task.TimeLimit != "5m" || task.Retry != nil {
		t.Fatalf("%+v %+v", fleetStage, task)
	}
	if nodes, err := Select(inv, fleetStage.Nodes); err != nil || strings.Join(nodes, ",") != "web1,web2" {
		t.Fatal(nodes, err)
	}
	if _, _, err := LoadRunCommand(path, "", []string{"db"}, "true", ""); err == nil || !strings.Contains(err.Error(), "--nodes") {
		t.Fatal(err)
	}
	if _, _, err := LoadRunCommand(path, "", []string{"all"}, "true", "bad user"); err == nil || !strings.Contains(err.Error(), "shell.run_as_user_account") {
		t.Fatal(err)
	}
}

func TestShellUserIsAnAccountName(t *testing.T) {
	for user, valid := range map[string]bool{"app": true, "'{{ .vars.app_user }}'": true, "'bad user'": false, "'-app'": false} {
		path := fixture(t, "vars: {app_user: app}\nfleet_stages: [{id: app, nodes: all, node_tasks: [{shell: {script: id, run_as_user_account: "+user+"}}]}]\n")
		if _, _, err := Load(path, "example", "", nil); (err == nil) != valid {
			t.Fatal(user, err)
		}
	}
}

func fixture(t *testing.T, project string) string {
	t.Helper()
	dir := t.TempDir()
	write(t, filepath.Join(dir, "inventories/example.yml"), "nodes:\n  web1: {address: example.com}\n  web2: {address: other.example.com}\ngroups:\n  web: {nodes: [web1, web2]}\n")
	path := filepath.Join(dir, "mmx_project_settings.yml")
	write(t, path, "project_name: test\ndefault_inventory: example\n"+project)
	return path
}
func write(t *testing.T, path, contents string) {
	t.Helper()
	if filepath.Base(path) == "mmx_project_settings.yml" {
		settings, playbook := splitFixture(contents)
		if playbook != "" {
			write(t, filepath.Join(filepath.Dir(path), "playbooks/example.yml"), playbook)
		}
		contents = settings
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
}

// Saved definitions and node jobs repeat every task's field locations, so they
// name files relative to the project and store each file once per definition.
func TestSavedLocationsAreCompactAndProjectRelative(t *testing.T) {
	samples := []string{"binary18", "binary64", "small1", "small2", "small3"}
	var nodeTasks []string
	for _, name := range samples {
		nodeTasks = append(nodeTasks, fmt.Sprintf(`{"id": %q, "file": {"permissions_mode": "0644", "path": "/var/tmp/mmx-streamed-assets/%s", "source": "SOURCE%s"}}`, name, name, name))
	}
	nodeTasks = append(nodeTasks, `{"id": "loop", "loop": ["one", "two"], "as": "item", "file": {"permissions_mode": "0644", "path": "/var/tmp/mmx-streamed-assets/{{ .item }}", "source": "SOURCEsmall1"}}`,
		`{"id": "template", "file": {"permissions_mode": "0644", "path": "/var/tmp/mmx-streamed-assets/config", "template": "SOURCEconfig.tmpl"}}`)
	list := strings.Join(nodeTasks, ", ")
	path := fixture(t, "\"vars\": {\"setting\": \"frozen\"}\n\"fleet_stages\": [{\"id\": \"copy\", \"nodes\": \"all\", \"node_tasks\": [{\"role\": \"copy\", \"inputs\": {\"setting\": \"{{ .vars.setting }}\"}}, {task_block: {id: \"copy_assets\" , \"changes\": [{\"file\": {\"permissions_mode\": \"0644\", \"path\": \"/var/tmp/mmx-streamed-assets/task_block\", \"source\": \"roles/copy/small2\"}}]}}]}]\n")
	dir := filepath.Dir(path)
	// Keep a role self-contained when possible.
	for _, name := range samples {
		write(t, filepath.Join(dir, "roles/copy", name), name)
	}
	write(t, filepath.Join(dir, "roles/copy/config.tmpl"), "value={{ .inputs.setting }}\n")
	write(t, filepath.Join(dir, "roles/copy/main.yml"), `{"inputs": {"setting": {"type": "string", "required": true}}, "node_tasks": [`+strings.ReplaceAll(list, "SOURCE", "")+`]}`)
	p, _, err := Load(path, "example", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := FreezeAssets(context.Background(), &p, t.TempDir(), nil); err != nil {
		t.Fatal(err)
	}
	saved, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(saved) > 10*1024 || strings.Contains(string(saved), `"file":"/`) {
		t.Fatalf("%d bytes: %s", len(saved), saved)
	}
	var restored Project
	if err := json.Unmarshal(saved, &restored); err != nil {
		t.Fatal(err)
	}
	role := restored.FleetStages[0].NodeTasks[0].RoleNodeTasks[5]
	block := restored.FleetStages[0].NodeTasks[1].TaskBlock
	originalBlock := p.FleetStages[0].NodeTasks[1].TaskBlock
	if !reflect.DeepEqual(block.Sources, originalBlock.Sources) || !reflect.DeepEqual(block.Changes[0].Sources, originalBlock.Changes[0].Sources) {
		t.Fatal("saved task block lost nested source locations", block)
	}
	if got := block.Changes[0].Sources.At("file.path").Field; got != "fleet_stages[0].node_tasks[1].task_block.changes[0].file.path" {
		t.Fatal(got)
	}
	if !reflect.DeepEqual(role.Sources, p.FleetStages[0].NodeTasks[0].RoleNodeTasks[5].Sources) || !reflect.DeepEqual(restored.FleetStages[0].NodeTasks[1].Sources, p.FleetStages[0].NodeTasks[1].Sources) {
		t.Fatal(role.Sources)
	}
	if got := role.Sources.At("file.path").String(); !strings.HasPrefix(got, "roles/copy/main.yml:1:") || !strings.HasSuffix(got, " (node_tasks[5].file.path)") {
		t.Fatal(got)
	}
	write(t, filepath.Join(dir, "roles/copy/main.yml"), "inputs: {setting: {type: string}}\nnode_tasks:\n- file: {permissions_mode: '0644', path: relative, content: x}\n")
	if _, _, err := Load(path, "example", "", nil); err == nil || !strings.Contains(err.Error(), "roles/copy/main.yml:3:42 (node_tasks[0].file.path): path must be absolute") {
		t.Fatal(err)
	}
}

func TestRoleInputsAndNestedOptIn(t *testing.T) {
	path := fixture(t, "fleet_stages:\n- id: setup\n  nodes: web\n  node_tasks:\n  - role: outer\n    inputs: {port: 8080}\n")
	dir := filepath.Dir(path)
	write(t, filepath.Join(dir, "roles/outer/main.yml"), "inputs:\n  port: {type: integer, required: true}\nnode_tasks:\n- role: inner\n")
	write(t, filepath.Join(dir, "roles/inner/main.yml"), "node_tasks:\n- show: ready\n")
	if _, _, err := Load(path, "example", "", nil); err == nil || !strings.Contains(err.Error(), "allow_role_calls_within_roles") {
		t.Fatalf("expected opt-in error, got %v", err)
	}
	b, _ := os.ReadFile(path)
	write(t, path, "allow_role_calls_within_roles: true\n"+string(b))
	p, inv, err := Load(path, "example", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.FleetStages[0].NodeTasks[0].RoleNodeTasks) != 1 {
		t.Fatal("role not expanded")
	}
	nodes, err := Select(inv, []any{"web2", "web"})
	if err != nil || strings.Join(nodes, ",") != "web1,web2" {
		t.Fatalf("selection %v: %v", nodes, err)
	}
	write(t, filepath.Join(dir, "roles/inner/main.yml"), "node_tasks:\n- role: outer\n  inputs: {port: 1}\n")
	if _, _, err := Load(path, "example", "", nil); err == nil || !strings.Contains(err.Error(), "recursive") {
		t.Fatalf("expected cycle rejection, got %v", err)
	}
}

// A literal role input is known locally, so its type is checked in every
// branch: ready, conditional, skipped, and inside a runtime loop.
func TestLiteralRoleInputsCheckedInEveryBranch(t *testing.T) {
	for name, policy := range map[string]string{
		"ready":       "",
		"conditional": "    when: {equals: ['{{ .node.arch }}', arm64]}\n",
		"runtime":     "    when: '{{ .node.virtual }}'\n",
		"skipped":     "    when: false\n",
		"loop":        "    loop: '{{ .node.addresses }}'\n    as: address\n",
	} {
		path := fixture(t, "fleet_stages:\n- id: setup\n  nodes: web\n  node_tasks:\n  - role: app\n"+policy+"    inputs: {port: not-a-number}\n")
		write(t, filepath.Join(filepath.Dir(path), "roles/app/main.yml"), "inputs:\n  port: {type: integer, required: true}\nnode_tasks:\n- show: '{{ .inputs.port }}'\n")
		_, _, err := Load(path, "example", "", nil)
		if err == nil || !strings.Contains(err.Error(), "input port must be integer") || !strings.Contains(err.Error(), "node_tasks[0].inputs.port") {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestRequirementsCheckedBeforeConnecting(t *testing.T) {
	for _, body := range []string{
		"- id: first\n  nodes: web1\n  requires: {same_node: [later]}\n  node_tasks: [{show: hi}]\n",
		"- id: first\n  nodes: web1\n  node_tasks: [{show: hi}]\n- id: second\n  nodes: web2\n  requires: {same_node: [first]}\n  node_tasks: [{show: hi}]\n",
		"- id: first\n  nodes: web1\n  node_tasks: [{shell: {script: check, run_only_if_this_command_succeeds: {script: guard, true_exit_codes: [0, 2], false_exit_codes: [2]}}}]\n",
	} {
		path := fixture(t, "fleet_stages:\n"+body)
		if _, _, err := Load(path, "example", "", nil); err == nil {
			t.Fatal("accepted invalid dependency or guard")
		}
	}
}

func TestPackageRemovalConfiguration(t *testing.T) {
	for _, pkg := range []string{"{packages: [{name: demo}], state: absent}", "{packages: [{name: demo, version: '1.0'}], state: present}"} {
		if _, _, err := Load(fixture(t, "fleet_stages: [{id: setup, nodes: all, node_tasks: [{apt_package: "+pkg+"}]}]\n"), "example", "", nil); err != nil {
			t.Fatal(err)
		}
	}
	for _, pkg := range []string{"{packages: [{name: demo, version: '1.0'}], state: absent}", "{packages: [{name: demo}], state: purge}"} {
		if _, _, err := Load(fixture(t, "fleet_stages: [{id: setup, nodes: all, node_tasks: [{apt_package: "+pkg+"}]}]\n"), "example", "", nil); err == nil {
			t.Fatalf("accepted %s", pkg)
		}
	}
}

// Fixtures keep settings and playbook snippets together for compact test cases;
// write emits separate files, preserving lines for source-location assertions.
func splitFixture(contents string) (string, string) {
	var document yaml.Node
	if err := yaml.Unmarshal([]byte(contents), &document); err != nil {
		return contents, ""
	}
	if len(document.Content) == 0 || document.Content[0].Kind != yaml.MappingNode {
		return contents, ""
	}
	lines := strings.SplitAfter(contents, "\n")
	settings, playbook := append([]string{}, lines...), make([]string, len(lines))
	for i, line := range lines {
		if strings.HasSuffix(line, "\n") {
			playbook[i] = "\n"
		}
	}
	fields := document.Content[0].Content
	found := false
	for i := 0; i < len(fields); i += 2 {
		if fields[i].Value != "fleet_stages" && fields[i].Value != "command_line_vars" {
			continue
		}
		found = true
		end := len(lines)
		if i+2 < len(fields) {
			end = fields[i+2].Line - 1
		}
		for line := fields[i].Line - 1; line < end; line++ {
			settings[line], playbook[line] = playbook[line], settings[line]
		}
	}
	if !found {
		return contents, ""
	}
	return strings.Join(settings, ""), strings.Join(playbook, "")
}

// A variable, or a value that a node gets in its scope such as a fleet
// stage's output, must not use a mapping key that MMX reserves for its own
// records of secrets, shortened text, and binary content: the node would read
// the mapping as such a record. Loading refuses it, naming the key and the file.
func TestLoadRefusesKeysReservedForMMXRecords(t *testing.T) {
	const stage = "fleet_stages: [{id: setup, nodes: all, node_tasks: [{show: ready}]}]\n"
	for _, tc := range []struct {
		name, project, inventory string
		want                     []string
	}{
		{"the only project variable", `vars: {"$mmx_secret": {}}` + "\n" + stage, "",
			[]string{"mmx_project_settings.yml:", "$mmx_secret", "Rename the key"}},
		{"nested in a project variable", `vars: {app: {banner: {"$mmx_truncated": partial}}}` + "\n" + stage, "",
			[]string{"mmx_project_settings.yml:", "$mmx_truncated"}},
		{"in a group variable", stage, "nodes:\n  web1: {address: example.com}\n  web2: {address: other.example.com}\ngroups:\n  web: {nodes: [web1, web2], vars: {banner: {\"$mmx_truncated\": partial}}}\n",
			[]string{"inventories/example.yml:", "$mmx_truncated"}},
		{"in a list in a node variable", stage, "nodes:\n  web1: {address: example.com, vars: {files: [{\"$mmx_binary\": aGk=}]}}\n  web2: {address: other.example.com}\n",
			[]string{"inventories/example.yml:", "$mmx_binary"}},
		{"in a fleet stage output", "fleet_stages: [{id: setup, nodes: all, node_tasks: [{show: ready}], outputs: {banner: {\"$mmx_truncated\": partial}}}]\n", "",
			[]string{"playbooks/example.yml:", "$mmx_truncated", "Rename the key"}},
	} {
		path := fixture(t, tc.project)
		if tc.inventory != "" {
			write(t, filepath.Join(filepath.Dir(path), "inventories/example.yml"), tc.inventory)
		}
		_, _, err := Load(path, "example", "", nil)
		for _, part := range tc.want {
			if err == nil || !strings.Contains(err.Error(), part) {
				t.Fatalf("%s: want %q in %v", tc.name, part, err)
			}
		}
	}
}

// A path that starts with ~/ in an SSH setting starts at the home directory,
// as OpenSSH reads it; another relative path starts at the project directory.
func TestSSHPathsExpandHomeDirectory(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	path := fixture(t, "fleet_stages: [{id: setup, nodes: all, node_tasks: [{show: hi}]}]\n")
	write(t, filepath.Join(filepath.Dir(path), "inventories/example.yml"), "nodes:\n  web1: {address: example.com, ssh_identity_file: ~/.ssh/web1_key, ssh_known_hosts: ssh/known_hosts, ssh_config: ~/.ssh/config}\n")
	_, inv, err := Load(path, "example", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	node := inv.Nodes["web1"]
	if want := filepath.Join(home, ".ssh/web1_key"); node.IdentityFile != want {
		t.Errorf("ssh_identity_file = %q, want %q", node.IdentityFile, want)
	}
	if want := filepath.Join(home, ".ssh/config"); node.SSHConfig != want {
		t.Errorf("ssh_config = %q, want %q", node.SSHConfig, want)
	}
	if want := filepath.Join(filepath.Dir(path), "ssh/known_hosts"); node.KnownHosts != want {
		t.Errorf("ssh_known_hosts = %q, want %q", node.KnownHosts, want)
	}
}
