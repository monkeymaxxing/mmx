package config

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"mmx/internal/modules"
	"mmx/internal/value"
)

const wholeValueKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl admin@example.com"

// wholeValueTasks use one whole-value reference for an integer, a boolean, a
// list, and a mapping field.
const wholeValueTasks = `[
  {update_alternatives: {name: editor, program_path: /usr/bin/vim, generic_command_path: /usr/bin/editor, priority_when_registered: "{{ .vars.alternatives_priority }}"}},
  {user: {name: app, create_home_directory: false, system: "{{ .vars.system_account }}"}},
  {authorized_keys: {for_user_account: root, keys: "{{ .vars.keys }}", keys_not_listed: keep}},
  {mysql_user: {name: shop, may_connect_from: "10.0.%", allow_creating_account_without_password: true, privileges: ["{{ .vars.privileges }}"], privileges_not_listed: remove}}]`

func wholeValueProject(t *testing.T, vars string) string {
	t.Helper()
	return fixture(t, "vars: {"+vars+"}\nfleet_stages: [{id: setup, nodes: all, node_tasks: "+wholeValueTasks+"}]\n")
}

// A whole-value reference keeps its type: a field of any type takes one, and
// the value it resolves to fills the field, on the controller machine and on
// the node alike.
func TestWholeValueReferencesFillTypedFields(t *testing.T) {
	path := wholeValueProject(t, "alternatives_priority: 512, system_account: true, keys: ['"+wholeValueKey+"'], privileges: {grant_on: 'shop.*', grant: [SELECT]}")
	p, inv, err := Load(path, "example", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	scope := localScope(p, inv, "web1", InventoryValues(inv))
	tasks := make([]Task, len(p.FleetStages[0].NodeTasks))
	for i, task := range p.FleetStages[0].NodeTasks {
		// The node gets the task as JSON.
		data, err := json.Marshal(task)
		if err != nil {
			t.Fatal(err)
		}
		var sent Task
		if err := json.Unmarshal(data, &sent); err != nil {
			t.Fatal(err)
		}
		if len(sent.WholeValueReferences) != 1 {
			t.Fatal(i, sent.WholeValueReferences)
		}
		resolved, err := ResolveArguments(sent, scope)
		if err != nil {
			t.Fatal(i, err)
		}
		tasks[i] = resolved
	}
	if size := *InputsOf[modules.UpdateAlternatives](tasks[0]).PriorityWhenRegistered; size != 512 {
		t.Fatal(size)
	}
	if system := InputsOf[modules.User](tasks[1]).System; system == nil || !*system {
		t.Fatal(system)
	}
	if keys := InputsOf[modules.AuthorizedKeys](tasks[2]).Keys; len(keys) != 1 || keys[0] != wholeValueKey {
		t.Fatal(keys)
	}
	if privileges := InputsOf[modules.MySQLUser](tasks[3]).Privileges; len(privileges) != 1 || privileges[0].GrantOn != "shop.*" || privileges[0].Grant[0] != "SELECT" {
		t.Fatal(privileges)
	}
	// The definition keeps its references.
	if *InputsOf[modules.UpdateAlternatives](p.FleetStages[0].NodeTasks[0]).PriorityWhenRegistered != 0 {
		t.Fatal("resolving changed the definition")
	}

	// A value known only on the node, such as a fact read from JSON, is
	// checked against the field's type there.
	fact := Task{Module: "update_alternatives", ModuleInputs: &modules.UpdateAlternatives{Name: "editor", ProgramPath: "/usr/bin/vim", GenericCommandPath: "/usr/bin/editor"}, WholeValueReferences: []WholeValueReference{{Field: "priority_when_registered", Steps: []string{"priority_when_registered"}, Reference: "{{ .node.alternatives_priority }}"}}}
	resolved, err := ResolveArguments(fact, value.Scope{"node": map[string]any{"alternatives_priority": float64(2048)}})
	if err != nil || *InputsOf[modules.UpdateAlternatives](resolved).PriorityWhenRegistered != 2048 {
		t.Fatal(resolved, err)
	}
	if _, err := ResolveArguments(fact, value.Scope{"node": map[string]any{"alternatives_priority": "a lot"}}); err == nil || !strings.Contains(err.Error(), `update_alternatives.priority_when_registered: {{ .node.alternatives_priority }} resolved to the text "a lot", but the module input requires a whole number`) {
		t.Fatal(err)
	}
}

// A value of the wrong type fails where it resolves, naming the field, the
// reference, and the type the field needs. The module's own rules apply to
// the resolved value. A reference within other text stays text.
// An optional field whose absence a module's own check counts, such as
// wait_for.tcp_port_accepts_connections beside wait_for's other forms, counts as written while its
// reference waits, and gets the module's range check once it resolves.
func TestWholeValueReferenceInAnOptionalField(t *testing.T) {
	for _, c := range []struct{ port, want string }{{"6379", ""}, {"70000", "wait_for.tcp_port_accepts_connections must be between 1 and 65535"}} {
		path := fixture(t, "vars: {port: "+c.port+"}\nfleet_stages: [{id: setup, nodes: all, node_tasks: [{wait_for: {tcp_port_accepts_connections: '{{ .vars.port }}', wait_at_most: 5s}}]}]\n")
		p, _, err := Load(path, "example", "", nil)
		if c.want == "" && err == nil {
			// On the node, the job's JSON gives the value as a json.Number.
			sent := Task{Module: "wait_for", ModuleInputs: modules.CopyInputs(p.FleetStages[0].NodeTasks[0].ModuleInputs), WholeValueReferences: p.FleetStages[0].NodeTasks[0].WholeValueReferences}
			resolved, err := ResolveArguments(sent, value.Scope{"vars": map[string]any{"port": json.Number(c.port)}})
			if err != nil || *InputsOf[modules.WaitFor](resolved).TCPPortAcceptsConnections != 6379 {
				t.Fatal(err)
			}
		}
		if c.want == "" && err != nil || c.want != "" && (err == nil || !strings.Contains(err.Error(), c.want)) {
			t.Fatal(c.port, err)
		}
	}
}

// A list that a module's own check counts as written or left out, such as
// pip_package's packages, counts as written while its reference
// waits.
func TestWholeValueReferenceInAListThatAModuleCounts(t *testing.T) {
	path := fixture(t, "vars: {packages: [{name: celery, version: newest_on_every_run}, {name: redis, version: newest_on_every_run}]}\nfleet_stages: [{id: setup, nodes: all, node_tasks: [{pip_package: {packages: '{{ .vars.packages }}', run_as_user_account: root, virtual_environment: /srv/venv}}]}]\n")
	p, inv, err := Load(path, "example", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := ResolveArguments(p.FleetStages[0].NodeTasks[0], localScope(p, inv, "web1", InventoryValues(inv)))
	if err != nil {
		t.Fatal(err)
	}
	if got := InputsOf[modules.PipPackage](resolved).Packages; len(got) != 2 || got[0].Name != "celery" || got[1].Name != "redis" || got[0].Version != "newest_on_every_run" {
		t.Fatal(got)
	}
}

func TestWholeValueReferencesRejectAWrongType(t *testing.T) {
	good := map[string]string{"alternatives_priority": "512", "system_account": "true", "keys": "['" + wholeValueKey + "']", "privileges": "{grant_on: 'shop.*', grant: [SELECT]}"}
	for _, tc := range []struct{ name, value, want string }{
		{"alternatives_priority", "'512MB'", `update_alternatives.priority_when_registered): {{ .vars.alternatives_priority }} resolved to the text "512MB", but the module input requires a whole number`},
		{"system_account", "'yes'", `user.system): {{ .vars.system_account }} resolved to the text "yes", but the module input requires true or false`},
		{"keys", "'" + wholeValueKey + "'", "authorized_keys.keys): {{ .vars.keys }} resolved to the text"},
		{"privileges", "[SELECT]", "mysql_user.privileges[0]): {{ .vars.privileges }} resolved to a list, but the module input requires a mapping"},
		// The module's own rule for the field, with the resolved value.
		{"keys", "['not a key']", "authorized_keys.keys): must be one OpenSSH key line"},
	} {
		vars := []string{}
		for _, name := range []string{"alternatives_priority", "system_account", "keys", "privileges"} {
			v := good[name]
			if name == tc.name {
				v = tc.value
			}
			vars = append(vars, name+": "+v)
		}
		_, _, err := Load(wholeValueProject(t, strings.Join(vars, ", ")), "example", "", nil)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatal(tc.name, tc.value, err)
		}
	}
	embedded := fixture(t, "vars: {alternatives_priority: 512}\nfleet_stages: [{id: setup, nodes: all, node_tasks: [{update_alternatives: {name: editor, program_path: /usr/bin/vim, generic_command_path: /usr/bin/editor, priority_when_registered: '{{ .vars.alternatives_priority }}0'}}]}]\n")
	if _, _, err := Load(embedded, "example", "", nil); err == nil || !strings.Contains(err.Error(), "expected int, got !!str") {
		t.Fatal(err)
	}
	secret := fixture(t, "secrets_from_env: {size: MMX_SIZE}\nfleet_stages: [{id: setup, nodes: all, node_tasks: [{update_alternatives: {name: editor, program_path: /usr/bin/vim, generic_command_path: /usr/bin/editor, priority_when_registered: '{{ .secrets.size }}'}}]}]\n")
	p, inv, err := Load(secret, "example", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	scope := value.Scope{"secrets": map[string]any{"size": value.Sensitive(512)}}
	if _, err := ResolveArguments(p.FleetStages[0].NodeTasks[0], scope); err == nil || !strings.Contains(err.Error(), "must not contain a secret") {
		t.Fatal(inv.Name, err)
	}
}

// A whole value is data, substituted once: text in it that looks like a
// reference stays text, and the rules of the module's own arguments below the
// field check the values as they are.
func TestWholeValuesAreNotReadAgainAsReferences(t *testing.T) {
	load := func(vars, task string) (Project, Inventory, error) {
		path := fixture(t, "vars: {"+vars+"}\nfleet_stages: [{id: setup, nodes: all, node_tasks: [{"+task+"}]}]\n")
		return Load(path, "example", "", nil)
	}
	const keys = `authorized_keys: {for_user_account: root, keys: "{{ .vars.keys }}", keys_not_listed: keep}`
	literal := wholeValueKey + " {{ .vars.other }}"
	p, inv, err := load("other: replaced, keys: ['"+literal+"']", keys)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := ResolveArguments(p.FleetStages[0].NodeTasks[0], localScope(p, inv, "web1", InventoryValues(inv)))
	if err != nil {
		t.Fatal(err)
	}
	if got := InputsOf[modules.AuthorizedKeys](resolved).Keys; len(got) != 1 || got[0] != literal {
		t.Fatal("a whole value was read again:", got)
	}
	// An item that is only reference-like text fails the key rule as text,
	// although the variable it names holds a valid key.
	if _, _, err := load("other: '"+wholeValueKey+"', keys: ['{{ .vars.other }}']", keys); err == nil || !strings.Contains(err.Error(), "must be one OpenSSH key line") {
		t.Fatal(err)
	}
	const members = `group: {name: team, supplementary_members: "{{ .vars.members }}", supplementary_members_not_listed: keep}`
	p, inv, err = load("members: [alice, bob]", members)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err = ResolveArguments(p.FleetStages[0].NodeTasks[0], localScope(p, inv, "web1", InventoryValues(inv)))
	if err != nil || strings.Join(InputsOf[modules.Group](resolved).SupplementaryMembers, ",") != "alice,bob" {
		t.Fatal(resolved, err)
	}
	if _, _, err := load("members: [alice, '-x']", members); err == nil || !strings.Contains(err.Error(), "group.supplementary_members") || !strings.Contains(err.Error(), "invalid account name") {
		t.Fatal(err)
	}

}

// Key files are read on the controller machine as the project loads. A
// whole-value reference there would reach the node with the file names but
// without their keys, and keys_not_listed: remove would then remove every
// key: such a field is refused as the project loads.
func TestWholeValueReferenceRefusedWhereFilesAreReadOnTheControllerMachine(t *testing.T) {
	for field, written := range map[string]string{"key_files_on_controller_machine": "files/admin.pub", "key_directories_on_controller_machine": "files/admin"} {
		path := fixture(t, "vars: {admin_keys: ["+written+"]}\nfleet_stages: [{id: setup, nodes: all, node_tasks: [{authorized_keys: {for_user_account: root, "+field+": '{{ .vars.admin_keys }}', keys_not_listed: remove}}]}]\n")
		write(t, filepath.Join(filepath.Dir(path), "files/admin.pub"), wholeValueKey+"\n")
		write(t, filepath.Join(filepath.Dir(path), "files/admin/admin.pub"), wholeValueKey+"\n")
		_, _, err := Load(path, "example", "", nil)
		if err == nil || !strings.Contains(err.Error(), "authorized_keys."+field+"): "+field+" names files read on the controller machine while loading the project") {
			t.Fatal(field, err)
		}
	}
}

// Package files must be found and saved while the project loads. A list or
// entry supplied only by a whole-value reference cannot carry prepared assets.
func TestWholeValuePackageEntriesCannotSupplyControllerFiles(t *testing.T) {
	for _, manager := range []string{"apt", "dnf"} {
		for _, fileInputs := range []string{
			"package_file_source: application.pkg",
			"package_file_url: https://example.com/application.pkg, package_file_sha256: " + strings.Repeat("a", 64),
		} {
			for _, entryReference := range []bool{false, true} {
				field, entries := "packages", "'{{ .vars.package_entries }}'"
				if entryReference {
					field, entries = "packages[0]", "['{{ .vars.package_entries }}']"
				}
				declaration := "[{name: application, version: '1', " + fileInputs + "}]"
				if entryReference {
					declaration = "{name: application, version: '1', " + fileInputs + "}"
				}
				path := fixture(t, "vars: {package_entries: "+declaration+"}\nfleet_stages: [{id: setup, nodes: all, node_tasks: [{"+manager+"_package: {packages: "+entries+"}}]}]\n")
				write(t, filepath.Join(filepath.Dir(path), "application.pkg"), "application bytes")
				_, _, err := Load(path, "example", "", nil)
				if err == nil || !strings.Contains(err.Error(), manager+"_package."+field+"): "+field+" names files read on the controller machine while loading the project") {
					t.Fatal(manager, fileInputs, entryReference, err)
				}
			}
		}
	}
}

// A check_new_file may be a whole-value reference. Once it resolves, loading
// checks the command as it checks one written in the task: .new_file.path
// waits for the node, and a reference to a value that does not exist, a
// reference in a script, or a check without shell is refused.
func TestWholeValueReferenceToACheckOfANewFile(t *testing.T) {
	project := func(check string) string {
		return fixture(t, "vars: {check: "+check+"}\nfleet_stages: [{id: setup, nodes: all, node_tasks: [{line: {insert_at_end_of_file: true, path: /etc/app.conf, line: a=1, check_new_file: '{{ .vars.check }}'}}]}]\n")
	}
	path := project(`{shell: {script: 'test -s "$CANDIDATE"', env: {CANDIDATE: '{{ .new_file.path }}'}}}`)
	p, inv, err := Load(path, "example", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := ResolveArguments(p.FleetStages[0].NodeTasks[0], localScope(p, inv, "web1", InventoryValues(inv)))
	if err != nil {
		t.Fatal(err)
	}
	check := InputsOf[modules.Line](resolved).CheckNewFile
	if check == nil || check.Shell == nil || check.Shell.Script != `test -s "$CANDIDATE"` || check.Shell.Env["CANDIDATE"] != "{{ .new_file.path }}" {
		t.Fatalf("check_new_file did not resolve: %#v", check)
	}
	if _, err := SecretUsageOf(path, ""); err != nil {
		t.Fatal(err)
	}
	for check, want := range map[string][]string{
		`{shell: {script: "true", changed_when: {equals: ["{{ .this_node_task.unknown }}", true]}}}`: {"reference .this_node_task.unknown names a value that is not declared or is declared later"},
		`{shell: {script: 'true', env: {VALUE: '{{ .vars.missing }}'}}}`:                             {"line.check_new_file", ".vars.missing", "Correct the command value", "value supplied at"},
		`{shell: {script: 'check {{ .new_file.path }}'}}`:                                            {"check_new_file.shell.script runs as written", "The shell script comes from {{ .vars.check }}"},
		`{}`: {"check_new_file requires shell"},
	} {
		_, _, err := Load(project(check), "example", "", nil)
		for _, part := range want {
			if err == nil || !strings.Contains(err.Error(), part) {
				t.Fatalf("%s: want %q in %v", check, part, err)
			}
		}
	}
}

func TestGemRemovalReferencesRequireAnExactVersion(t *testing.T) {
	for _, version := range []string{"3.1.8", "newest_when_first_installed", "newest_on_every_run"} {
		path := fixture(t, "vars: {version: "+version+"}\nfleet_stages: [{id: setup, nodes: all, node_tasks: [{gem_package: {packages: [{name: rack, version: '{{ .vars.version }}'}], state: absent, run_as_user_account: root}}]}]\n")
		_, _, err := Load(path, "example", "", nil)
		if version == "3.1.8" {
			if err != nil {
				t.Fatal(err)
			}
		} else if err == nil || !strings.Contains(err.Error(), "version must be an exact RubyGems version") {
			t.Fatalf("%s: %v", version, err)
		}
	}
}

func TestPipVersionRefusesEnvironmentMarkers(t *testing.T) {
	for _, version := range []string{`==23.0.0; python_version >= "3.9"`, `{{ .vars.version }}`} {
		path := fixture(t, "vars: {version: '>=2.32,<3; sys_platform == \"linux\"'}\nfleet_stages: [{id: setup, nodes: all, node_tasks: [{pip_package: {packages: [{name: requests, version: '"+version+"'}], virtual_environment: /srv/app/venv, run_as_user_account: root}}]}]\n")
		_, _, err := Load(path, "example", "", nil)
		if err == nil || !strings.Contains(err.Error(), `version takes a version specifier such as "==23.0.0" or ">=2.32,<3"`) || !strings.Contains(err.Error(), "Use the task's when to set a condition") {
			t.Fatalf("%s: %v", version, err)
		}
	}
}
