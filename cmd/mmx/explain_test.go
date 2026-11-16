package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mmx/internal/config"
)

// mmx nodes shows the inventory, groups, and each fleet stage's selection after the
// limit, and rejects a limit the same way run does.
func TestNodesShowsTheLimitedSelection(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	write := func(name, content string) {
		writeCLIProject(t, dir, name, content)
	}
	write("inventories/example.yml", "nodes:\n  web01: {address: web01.example.net, ssh_user: deploy}\n  web02: {address: web02.example.net}\n  db01: {address: db01.example.net}\ngroups:\n  web: {nodes: [web02, web01]}\n  canary: {nodes: [web01]}\n  db: {nodes: [db01]}\n")
	write("mmx_project_settings.yml", "project_name: test\ndefault_inventory: example\nfleet_stages:\n- {id: deploy_app, nodes: web, node_tasks: [{show: hi}]}\n- {id: backup_database, nodes: db, node_tasks: [{show: hi}]}\n")
	nodes := func(args ...string) (string, int, error) {
		t.Helper()
		stdout, err := os.CreateTemp(dir, "stdout")
		if err != nil {
			t.Fatal(err)
		}
		saved := os.Stdout
		os.Stdout = stdout
		code, err := run(context.Background(), append([]string{"nodes", "example"}, args...))
		os.Stdout = saved
		stdout.Close()
		b, _ := os.ReadFile(stdout.Name())
		return string(b), code, err
	}
	out, code, err := nodes("--limit", "canary")
	if code != 0 || err != nil {
		t.Fatal(code, err)
	}
	for _, want := range []string{"Playbook: example   Inventory: example (inventories/example.yml, 3 nodes)\nLimit: canary", "web01   web01.example.net   deploy   canary, web", "web02   web02.example.net   -", "canary   web01", "Fleet stages (limit applied)", "deploy_app        web   → web01", "backup_database   db    → none; left out by the limit"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in\n%s", want, out)
		}
	}
	if out, _, _ := nodes("--output", "summary"); !strings.Contains(out, "web      2 nodes") || !strings.Contains(out, "deploy_app        web   → 2 nodes") || strings.Contains(out, "web01.example.net") {
		t.Fatal(out)
	}
	write("mmx_project_settings.yml", "project_name: test\ndefault_inventory: example\nfleet_stages:\n- {id: backup_database, nodes: db, node_tasks: [{show: hi}]}\n- {id: deploy_app, nodes: web, requires: {all_nodes: [backup_database]}, node_tasks: [{show: hi}]}\n")
	if _, code, err := nodes("--limit", "canary"); code != 4 || err == nil || !strings.Contains(err.Error(), "excludes fleet stage backup_database, which fleet stage deploy_app requires") {
		t.Fatal(code, err)
	}
}

// explain names the group and the --var option that supplied a value; it needs a
// value for every command-line variable, while validate and nodes do not.
func TestExplainShowsGroupAndCommandLineOrigins(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	write := func(name, content string) {
		writeCLIProject(t, dir, name, content)
	}
	write("inventories/example.yml", "nodes:\n  web01: {address: web01.example.net}\ngroups:\n  web: {nodes: [web01], vars: {port: 8080}}\n")
	write("mmx_project_settings.yml", "project_name: test\ndefault_inventory: example\ncommand_line_vars:\n  release: {type: string}\nfleet_stages:\n- {id: deploy, nodes: web, node_tasks: [{show: '{{ .vars.release }}:{{ .vars.port }}'}]}\n")
	command := func(args ...string) (string, int, error) {
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
		b, _ := os.ReadFile(stdout.Name())
		return string(b), code, err
	}
	for _, name := range []string{"validate", "nodes"} {
		if _, code, err := command(name); code != 0 || err != nil {
			t.Fatal(name, code, err)
		}
	}
	if _, code, err := command("explain"); code != 4 || err == nil || !strings.Contains(err.Error(), "set release with --var NAME=VALUE") {
		t.Fatal(code, err)
	}
	out, code, err := command("explain", "--var", "release=1.4.3")
	if code != 0 || err != nil {
		t.Fatal(code, err)
	}
	for _, want := range []string{"Command-line variable release = \"1.4.3\"\n  from --var release", "show = \"1.4.3:8080\"", "value from inventories/example.yml:4:38 (groups.web.vars.port)", "value from --var release"} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in\n%s", want, out)
		}
	}
	if _, code, err := command("explain", "--var", "version=1"); code != 4 || err == nil || !strings.Contains(err.Error(), "version is not declared in command_line_vars (declared: release)") {
		t.Fatal(code, err)
	}
}

// The JSON of validate and explain carries schema_version, as every JSON
// result of mmx does. Explain's is 2 since it lists save_on_controller_machine.
func TestValidateAndExplainJSONCarryTheSchemaVersion(t *testing.T) {
	initializedProject(t)
	for args, version := range map[string]int{"validate --json": 1, "validate example --json": 1, "explain example --json": 2} {
		out, code, err := captureCommand(t, strings.Fields(args)...)
		var view struct {
			SchemaVersion int `json:"schema_version"`
		}
		if code != 0 || err != nil || json.Unmarshal([]byte(out), &view) != nil || view.SchemaVersion != version {
			t.Fatal(args, code, err, out)
		}
	}
}

// explain lists what each fleet stage writes on the controller machine: for
// each save_on_controller_machine entry, its output or template, the file of
// each node, and the permissions mode. A path that reads a value known only
// during the run names no file.
func TestExplainListsTheFilesAFleetStageWritesOnTheControllerMachine(t *testing.T) {
	t.Setenv("MMX_RUNNER_DIR", t.TempDir())
	dir := t.TempDir()
	t.Chdir(dir)
	elsewhere := filepath.Join(dir, "elsewhere", "summary.txt")
	writeCLIProject(t, dir, "inventories/example.yml", "nodes:\n  web01: {address: web01.example.net}\n  db01: {address: db01.example.net}\n")
	writeCLIProject(t, dir, "files/summary.tmpl", "{{ range $node, $outputs := .outputs.collect }}{{ $node }}\n{{ end }}")
	writeCLIProject(t, dir, "mmx_project_settings.yml", `project_name: test
default_inventory: example
fleet_stages:
- id: collect
  nodes: all
  outputs: {report: hello}
  save_on_controller_machine:
  - {output: report, path: 'server_details/{{ .node.name }}.txt', permissions_mode: '0644'}
  - {template: files/summary.tmpl, path: '`+elsewhere+`', permissions_mode: '0600'}
- id: where
  nodes: web01
  outputs: {directory: reports}
- id: follow
  nodes: web01
  outputs: {report: hello}
  save_on_controller_machine:
  - {output: report, path: '{{ .outputs.where.web01.directory }}/{{ .node.name }}.txt', permissions_mode: '0644'}
`)
	out, code, err := captureCommand(t, "explain", "example", "--json")
	var view config.Inspection
	if code != 0 || err != nil || json.Unmarshal([]byte(out), &view) != nil || len(view.FleetStages) != 3 {
		t.Fatal(code, err, out)
	}
	collect, where, follow := view.FleetStages[0].SaveOnControllerMachine, view.FleetStages[1].SaveOnControllerMachine, view.FleetStages[2].SaveOnControllerMachine
	if len(collect) != 2 || len(where) != 0 || len(follow) != 1 {
		t.Fatal(out)
	}
	perNode := collect[0]
	files := map[string]string{}
	for _, file := range perNode.Files {
		files[file.Node] = file.File
	}
	if perNode.Output != "report" || perNode.PermissionsMode != "0644" || perNode.FilesResolvedDuringRun || len(files) != 2 ||
		!filepath.IsAbs(files["web01"]) || !strings.HasSuffix(files["web01"], "/server_details/web01.txt") || !strings.HasSuffix(files["db01"], "/server_details/db01.txt") {
		t.Fatalf("%+v", perNode)
	}
	summary := collect[1]
	if summary.Template != "files/summary.tmpl" || !strings.HasSuffix(summary.TemplateFile, "/files/summary.tmpl") || !filepath.IsAbs(summary.TemplateFile) ||
		summary.PermissionsMode != "0600" || len(summary.Files) != 1 || summary.Files[0] != (config.ExplainedSavedFile{File: elsewhere}) {
		t.Fatalf("%+v", summary)
	}
	if follow[0].Path != "{{ .outputs.where.web01.directory }}/{{ .node.name }}.txt" || !follow[0].FilesResolvedDuringRun || len(follow[0].Files) != 0 {
		t.Fatalf("%+v", follow[0])
	}
	human, code, err := captureCommand(t, "explain", "example")
	if code != 0 || err != nil {
		t.Fatal(code, err)
	}
	for _, want := range []string{
		"save_on_controller_machine[0]: writes on the controller machine from the output report, permissions_mode 0644",
		"web01: " + files["web01"],
		"save_on_controller_machine[1]: writes on the controller machine from the template files/summary.tmpl (" + summary.TemplateFile + "), permissions_mode 0600",
		"    " + elsewhere + "\n",
		`files resolved during the run, from the path "{{ .outputs.where.web01.directory }}/{{ .node.name }}.txt"`,
	} {
		if !strings.Contains(human, want) {
			t.Fatalf("missing %q in\n%s", want, human)
		}
	}
}

// explain shows what MMX derives for each node from a task's arguments,
// beside the arguments as written: a template rendered with the values the
// controller machine knows for the node, or why it is not, and ufw rules with
// one rule for each node of a from_inventory_group. The node's name is known,
// so an argument that reads it is resolved.
func TestExplainRendersTemplatesAndInventoryGroupRulesForEachNode(t *testing.T) {
	t.Setenv("MMX_RUNNER_DIR", t.TempDir())
	dir := t.TempDir()
	t.Chdir(dir)
	writeCLIProject(t, dir, "inventories/example.yml", "nodes:\n  web01: {address: web01.example.net, vars: {wireguard_address: 10.8.0.10}}\n  db01: {address: db01.example.net, vars: {wireguard_address: 10.8.0.20}}\ngroups:\n  web: {nodes: [web01]}\n")
	writeCLIProject(t, dir, "files/peers.tmpl", "{{ range $name := .inventory.groups.web }}[Peer] # {{ $name }}\n{{ end }}host={{ .node.name }}\n")
	writeCLIProject(t, dir, "files/facts.tmpl", "os={{ .node.distribution.id }}\n")
	writeCLIProject(t, dir, "files/token.tmpl", "token={{ .secrets.api_token }}\n")
	writeCLIProject(t, dir, "mmx_project_settings.yml", `project_name: test
default_inventory: example
fleet_stages:
- id: database
  nodes: db01
  node_tasks:
  - id: firewall
    ufw:
      default_incoming: deny
      incoming_rules_not_listed: remove
      incoming_rules:
      - {action: allow, port: 22, protocol: tcp}
      - {action: allow, port: 5432, protocol: tcp, from_inventory_group: {group: web, address_var: wireguard_address}}
  - id: peers
    file: {path: '/etc/peers-{{ .node.name }}.conf', template: files/peers.tmpl, owner: root, group: root, permissions_mode: '0644'}
  - id: facts
    file: {path: /etc/facts.conf, template: files/facts.tmpl, owner: root, group: root, permissions_mode: '0644'}
  - id: token
    file: {path: /etc/token, template: files/token.tmpl, owner: root, group: root, permissions_mode: '0600'}
`)
	out, code, err := captureCommand(t, "explain", "example", "--json")
	var view config.Inspection
	if code != 0 || err != nil || json.Unmarshal([]byte(out), &view) != nil {
		t.Fatal(code, err, out)
	}
	tasks := map[string]config.ExplainedTask{}
	for _, task := range view.FleetStages[0].NodeTasks {
		tasks[task.Path] = task
	}
	firewall := tasks["database/firewall"]
	if len(firewall.ExpandedInventoryGroupRules) != 1 || firewall.ExpandedInventoryGroupRules[0].Field != "ufw.incoming_rules" ||
		fmt.Sprint(firewall.ExpandedInventoryGroupRules[0].Rules) != "[map[action:allow port:22 protocol:tcp] map[action:allow from:10.8.0.10 port:5432 protocol:tcp]]" {
		t.Fatalf("%+v", firewall.ExpandedInventoryGroupRules)
	}
	for _, arg := range firewall.Arguments {
		if arg.Field == "ufw.incoming_rules" && !strings.Contains(fmt.Sprint(arg.Value), "from_inventory_group") {
			t.Fatalf("the argument keeps the rules as written: %+v", arg)
		}
	}
	peers := tasks["database/peers"]
	if len(peers.RenderedTemplates) != 1 || peers.RenderedTemplates[0].Field != "file.template" || peers.RenderedTemplates[0].Text == nil ||
		*peers.RenderedTemplates[0].Text != "[Peer] # web01\nhost=db01\n" {
		t.Fatalf("%+v", peers.RenderedTemplates)
	}
	for _, arg := range peers.Arguments {
		if arg.Field == "file.path" && (arg.Status != "resolved" || arg.Value != "/etc/peers-db01.conf") {
			t.Fatalf("%+v", arg)
		}
	}
	if facts := tasks["database/facts"].RenderedTemplates; len(facts) != 1 || facts[0].Text != nil ||
		facts[0].NotRenderedBecause != "the template reads .node.distribution.id, a fact that the node reports when MMX connects to it" {
		t.Fatalf("%+v", facts)
	}
	if token := tasks["database/token"].RenderedTemplates; len(token) != 1 || token[0].Text != nil || !strings.HasPrefix(token[0].NotRenderedBecause, "the template reads .secrets.api_token") {
		t.Fatalf("%+v", token)
	}
	human, code, err := captureCommand(t, "explain", "example")
	if code != 0 || err != nil {
		t.Fatal(code, err)
	}
	for _, want := range []string{
		"    file.template, rendered for db01:\n      [Peer] # web01\n      host=db01\n",
		"    ufw.incoming_rules for db01, with one rule for each node of each from_inventory_group:\n      {\"action\":\"allow\",\"port\":\"22\",\"protocol\":\"tcp\"}\n      {\"action\":\"allow\",\"port\":\"5432\",\"protocol\":\"tcp\",\"from\":\"10.8.0.10\"}\n",
		"    file.template, not rendered by mmx explain: the template reads .node.distribution.id, a fact that the node reports when MMX connects to it\n",
	} {
		if !strings.Contains(human, want) {
			t.Fatalf("missing %q in\n%s", want, human)
		}
	}
}

// explain labels a node task whose module is not core with the module's kind,
// and its JSON names the module_kind of every node task that calls a module.
func TestExplainShowsTheModuleKind(t *testing.T) {
	t.Setenv("MMX_RUNNER_DIR", t.TempDir())
	dir := t.TempDir()
	t.Chdir(dir)
	description := "summary: Says hello.\nexamples: [{description: x, config: 'greeting: {}'}]\n"
	writeCLIProject(t, dir, "inventories/example.yml", "nodes:\n  web01: {address: web01.example.net}\n")
	writeCLIProject(t, dir, "external_modules/greeting/module.star", "def run_node_task(inputs):\n    return None\n")
	writeCLIProject(t, dir, "external_modules/greeting/module_description.yml", description)
	writeCLIProject(t, dir, "external_modules/compiled/module_description.yml", strings.ReplaceAll(description, "greeting", "compiled"))
	for _, architecture := range []string{"amd64", "arm64"} {
		executable := "external_modules/compiled/module_executable_linux_" + architecture
		writeCLIProject(t, dir, executable, "#!/bin/sh\n")
		if err := os.Chmod(filepath.Join(dir, executable), 0755); err != nil {
			t.Fatal(err)
		}
	}
	writeCLIProject(t, dir, "mmx_project_settings.yml", "project_name: test\ndefault_inventory: example\nfleet_stages:\n- {id: deploy, nodes: all, node_tasks: [{id: hello, greeting: {}}, {id: built, compiled: {}}, {id: motd, file: {path: /etc/motd, content: hi, owner: root, group: root, permissions_mode: '0644'}}]}\n")
	human, code, err := captureCommand(t, "explain", "example")
	if code != 0 || err != nil {
		t.Fatal(code, err)
	}
	for _, want := range []string{"web01 / deploy/hello: greeting (project's own, Starlark) (ready)", "web01 / deploy/built: compiled (project's own, executable) (ready)", "web01 / deploy/motd: file (ready)"} {
		if !strings.Contains(human, want) {
			t.Fatalf("missing %q in\n%s", want, human)
		}
	}
	out, code, err := captureCommand(t, "explain", "example", "--json")
	var view config.Inspection
	if code != 0 || err != nil || json.Unmarshal([]byte(out), &view) != nil {
		t.Fatal(code, err, out)
	}
	kinds := map[string]string{}
	for _, task := range view.FleetStages[0].NodeTasks {
		kinds[task.Action] = task.ModuleKind
	}
	if kinds["greeting"] != "project_starlark" || kinds["compiled"] != "project_executable" || kinds["file"] != "core" {
		t.Fatalf("module kinds %v in\n%s", kinds, out)
	}
}

func TestHumanExplainShowsAllOriginsAndDeferredReferences(t *testing.T) {
	source := config.Location{File: "role.yml", Line: 10, Column: 3, Field: "file.content"}
	view := config.Inspection{Project: "example", FleetStages: []config.ExplainedFleetStage{{Path: "configure", Nodes: []string{"app"}, NodeTasks: []config.ExplainedTask{{Path: "config", Node: "app", Action: "file", Status: "ready", Source: source, Arguments: []config.Argument{
		{Field: "file.content", Value: "resolved", Source: source, Origins: []config.Location{{File: "project.yml", Line: 4, Column: 7, Field: "vars.port"}, {File: "inventories/example.yml", Line: 6, Column: 7, Field: "vars.greeting"}}},
		{Field: "show", Status: "runtime", Reference: "{{ .node_tasks.check.stdout }}", Source: source},
	}}}}}}
	var out bytes.Buffer
	if err := explain(&out, view); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"role.yml:10:3", "project.yml:4:7", "inventories/example.yml:6:7", "runtime (not evaluated)", ".node_tasks.check.stdout"} {
		if !strings.Contains(out.String(), want) {
			t.Fatal(want, out.String())
		}
	}
}

func TestHumanExplainShowsTheFailureAllowance(t *testing.T) {
	ten := 10
	view := config.Inspection{Project: "example", FleetStages: []config.ExplainedFleetStage{
		{Path: "patch", Nodes: make([]string, 500), FailedNodesAllowed: "2%", AllowedFailures: &ten},
		{Path: "later", Runtime: true, FailedNodesAllowed: "5"},
	}}
	var out bytes.Buffer
	if err := explain(&out, view); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"failed_nodes_allowed: 2% = 10 of 500 nodes", "failed_nodes_allowed: 5\n"} {
		if !strings.Contains(out.String(), want) {
			t.Fatal(want, out.String())
		}
	}
}

func writeCLIProject(t *testing.T, root, name, content string) {
	t.Helper()
	if name == "mmx_project_settings.yml" {
		// These fixtures always start with project_name and default_inventory.
		lines := strings.SplitN(content, "\n", 3)
		writeCLIProject(t, root, "playbooks/example.yml", "\n\n"+lines[2])
		content = lines[0] + "\n" + lines[1] + "\n"
	}
	path := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}
