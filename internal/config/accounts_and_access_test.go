package config

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"mmx/internal/modules"
	"mmx/internal/value"
)

func loadTask(t *testing.T, task string) (Task, error) {
	t.Helper()
	p := fixture(t, "secrets_from_env: {alice_hash: MMX_TEST_ALICE_HASH}\nfleet_stages: [{id: configure, nodes: all, node_tasks: [{"+task+"}]}]\n")
	project, _, err := Load(p, "example", "", nil)
	if err != nil {
		return Task{}, err
	}
	return project.FleetStages[0].NodeTasks[0], nil
}

func TestAccountFieldsValidate(t *testing.T) {
	for _, task := range []string{
		`user: {name: alice, full_name: Alice Example, create_home_directory: false, password_locked: true}`,
		`user: {create_home_directory: true, name: alice, password_hash: "{{ .secrets.alice_hash }}", apply_password_hash: only_when_account_has_no_password}`,
		`user: {create_home_directory: true, name: alice, password_hash: "{{.secrets.alice_hash}}", apply_password_hash: on_every_run, password_locked: false}`,
		`user: {name: alice, state: absent, also_delete_home_directory_and_mail: true}`,
		`group: {name: team, supplementary_members: [alice, bob], supplementary_members_not_listed: remove}`,
		`group: {name: team, supplementary_members: [], supplementary_members_not_listed: remove}`,
		`group: {name: team, supplementary_members: ["{{ .node.name }}"], supplementary_members_not_listed: keep}`,
	} {
		loaded, err := loadTask(t, task)
		if err != nil {
			t.Fatal(task, err)
		}
		b, _ := json.Marshal(loaded)
		var saved Task
		if err := json.Unmarshal(b, &saved); err != nil || !reflect.DeepEqual(InputsOf[modules.User](saved), InputsOf[modules.User](loaded)) || (InputsOf[modules.Group](loaded) != nil && !reflect.DeepEqual(InputsOf[modules.Group](saved).SupplementaryMembers, InputsOf[modules.Group](loaded).SupplementaryMembers)) {
			t.Fatal("a saved task lost a field", task, err)
		}
	}
	for task, want := range map[string]string{
		`user: {create_home_directory: true, name: alice, password_hash: "$6$salt$hash", apply_password_hash: on_every_run}`:                    "one whole secret reference",
		`user: {create_home_directory: true, name: alice, password_hash: "prefix{{ .secrets.alice_hash }}", apply_password_hash: on_every_run}`: "one whole secret reference",
		`user: {create_home_directory: true, name: alice, password_hash: "{{ .vars.alice_hash }}", apply_password_hash: on_every_run}`:          "one whole secret reference",
		`user: {create_home_directory: true, name: alice, password_hash: "{{ .secrets.alice_hash }}"}`:                                          "apply_password_hash is required",
		`user: {create_home_directory: true, name: alice, password_hash: "{{ .secrets.alice_hash }}", apply_password_hash: always}`:             "only_when_account_has_no_password or on_every_run",
		`user: {create_home_directory: true, name: alice, apply_password_hash: on_every_run}`:                                                   "requires password_hash",
		`user: {create_home_directory: true, name: alice, also_delete_home_directory_and_mail: true}`:                                           "only for state: absent",
		`user: {name: alice, state: absent, full_name: Alice}`:                                                                                  "accepts only name, state, and also_delete_home_directory_and_mail",
		`user: {name: alice, state: absent, password_locked: true}`:                                                                             "accepts only",
		`user: {create_home_directory: true, name: alice, full_name: "Alice: Example"}`:                                                         "full_name must not contain",
		`user: {create_home_directory: true, name: alice, full_name: "Alice\nExample"}`:                                                         "full_name must not contain",
		`group: {name: team, supplementary_members: [alice]}`:                                                                                   "supplementary_members_not_listed is required",
		`group: {name: team, supplementary_members_not_listed: keep}`:                                                                           "requires supplementary_members",
		`group: {name: team, supplementary_members: [alice, alice], supplementary_members_not_listed: keep}`:                                    "listed twice",
		`group: {name: team, supplementary_members: ["-x"], supplementary_members_not_listed: keep}`:                                            "invalid account name",
		`group: {name: team, state: absent, supplementary_members: [], supplementary_members_not_listed: remove}`:                               "accepts only name and state",
	} {
		if _, err := loadTask(t, task); err == nil || !strings.Contains(err.Error(), want) {
			t.Error(task, err)
		}
	}
}

// Where MMX requires one whole secret reference, a role may pass on a role
// input declared secret: true: the role call must fill it from a secret, so
// the value is never written in the YAML either. An input not declared so is
// refused, and so is a reference within other text.
func TestWholeSecretReferenceTakesASecretRoleInput(t *testing.T) {
	const repository = "dnf_repository: {name: r, description: R, baseurl: ['https://example.com'], signing_key_url: 'https://example.com/key', dangerously_skip_checksum: true, username: u, password: "
	load := func(secret, task string) error {
		path := fixture(t, "secrets_from_env: {admin_hash: MMX_TEST_ADMIN_HASH}\nfleet_stages: [{id: configure, nodes: all, node_tasks: [{role: r, inputs: {hash: '{{ .secrets.admin_hash }}'}}]}]\n")
		write(t, filepath.Join(filepath.Dir(path), "roles/r/main.yml"), "inputs:\n  hash: {type: string, required: true, secret: "+secret+"}\nnode_tasks: [{"+task+"}]\n")
		_, _, err := Load(path, "example", "", nil)
		return err
	}
	for _, task := range []string{
		`user: {create_home_directory: true, name: admin, password_hash: "{{ .inputs.hash }}", apply_password_hash: on_every_run}`,
		repository + `'{{ .inputs.hash }}'}`,
	} {
		if err := load("true", task); err != nil {
			t.Fatal(task, err)
		}
	}
	for _, tc := range []struct{ secret, task, want string }{
		// Without secret: true, the role call's secret is refused first.
		{"false", `user: {create_home_directory: true, name: admin, password_hash: "{{ .inputs.hash }}", apply_password_hash: on_every_run}`, "role input hash receives a secret; declare secret: true"},
		{"true", `user: {create_home_directory: true, name: admin, password_hash: "x{{ .inputs.hash }}", apply_password_hash: on_every_run}`, "or, in a role, a reference to an input declared secret: true"},
		{"true", repository + `'{{ .inputs.hash }}x'}`, "dnf_repository.password must be a whole-value secret reference"},
	} {
		if err := load(tc.secret, tc.task); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Error(tc.task, err)
		}
	}
	// A role input that is not declared secret: true and gets no secret.
	path := fixture(t, "vars: {hash: plain}\nfleet_stages: [{id: configure, nodes: all, node_tasks: [{role: r, inputs: {hash: '{{ .vars.hash }}'}}]}]\n")
	write(t, filepath.Join(filepath.Dir(path), "roles/r/main.yml"), "inputs:\n  hash: {type: string, required: true}\nnode_tasks: [{user: {create_home_directory: true, name: admin, password_hash: '{{ .inputs.hash }}', apply_password_hash: on_every_run}}]\n")
	if _, _, err := Load(path, "example", "", nil); err == nil || !strings.Contains(err.Error(), "user.password_hash reads role input hash, which must be declared secret: true") {
		t.Fatal(err)
	}
	// Outside a role, .inputs names nothing.
	if _, err := loadTask(t, `user: {create_home_directory: true, name: alice, password_hash: "{{ .inputs.hash }}", apply_password_hash: on_every_run}`); err == nil {
		t.Fatal("a reference to a role input outside a role was accepted")
	}
}

// The password hash rule checks the resolved secret and never repeats it.
func TestPasswordHashRuleKeepsTheValueSecret(t *testing.T) {
	task := Task{Module: "user", ModuleInputs: &modules.User{Name: "alice", PasswordHash: "{{ .secrets.alice_hash }}", ApplyPasswordHash: "on_every_run"}}
	scope := func(hash string) value.Scope {
		return value.Scope{"secrets": map[string]any{"alice_hash": value.Sensitive(hash)}}
	}
	if _, err := InspectArguments(task, scope("$y$j9T$salt$hashhash"), false); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"plain-password-sentinel", "$6$salt$hash:with-colon", "$6$salt$hash\nsecond"} {
		_, err := InspectArguments(task, scope(bad), false)
		if err == nil || !strings.Contains(err.Error(), "crypt format") || strings.Contains(err.Error(), "sentinel") || strings.Contains(err.Error(), "with-colon") {
			t.Fatal(bad, err)
		}
	}
}

func TestKeyFilesAreReadWhenTheProjectLoads(t *testing.T) {
	const carolKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIJ6+dYwF2LfqNPAMrlXWfcNIkI5GE8sJjZoEw4M0VNYL carol@laptop"
	p := fixture(t, "fleet_stages: [{id: configure, nodes: all, node_tasks: [{authorized_keys: {for_user_account: deploy, keys: ['"+bobKey+"'], key_files_on_controller_machine: [files/ci.keys], key_directories_on_controller_machine: [files/team], keys_not_listed: remove}}]}]\n")
	dir := filepath.Dir(p)
	write(t, filepath.Join(dir, "files/ci.keys"), "# the CI machine\n\n"+carolKey+"\n")
	write(t, filepath.Join(dir, "files/team/b.pub"), aliceKey+"\n")
	write(t, filepath.Join(dir, "files/team/README"), "not a key\n")
	project, _, err := Load(p, "example", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	k := InputsOf[modules.AuthorizedKeys](project.FleetStages[0].NodeTasks[0])
	want := []modules.FileKey{{Line: carolKey, Place: "key_files_on_controller_machine[0] files/ci.keys line 3"}, {Line: aliceKey, Place: "key_directories_on_controller_machine[0] files/team/b.pub line 1"}}
	if !reflect.DeepEqual(k.FileKeys, want) {
		t.Fatal(k.FileKeys)
	}
	// A run saves the keys it read, so mmx resume does not read the files again.
	b, _ := json.Marshal(project.FleetStages[0].NodeTasks[0])
	var saved Task
	if err := json.Unmarshal(b, &saved); err != nil || !reflect.DeepEqual(InputsOf[modules.AuthorizedKeys](saved).FileKeys, want) {
		t.Fatal(InputsOf[modules.AuthorizedKeys](saved), err)
	}
	keys, err := modules.AuthorizedKeyList(*k, k.Keys, nil)
	if err != nil || len(keys) != 3 || keys[0].Comment != "bob@laptop" || keys[1].Comment != "carol@laptop" || keys[2].Comment != "alice@laptop" {
		t.Fatal(keys, err)
	}

	for _, tc := range []struct{ file, text, want string }{
		{"files/team/a.pub", aliceKey + "\nssh-ed25519 not-base64\n", "key_directories_on_controller_machine[0] files/team/a.pub line 2: must be one OpenSSH key line"},
		{"files/team/a.pub", "# nobody yet\n", "key file files/team/a.pub holds no key line"},
		{"files/team/a.pub", "no-pty " + bobKey + "\n", "key listed twice: ssh-ed25519 SHA256:"},
		{"files/team/c.pub", "no-pty " + aliceKey + "\n", "in key_directories_on_controller_machine[0] files/team/b.pub line 1 and in key_directories_on_controller_machine[0] files/team/c.pub line 1"},
	} {
		write(t, filepath.Join(dir, tc.file), tc.text)
		_, _, err := Load(p, "example", "", nil)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Error(tc.file, err)
		}
		if err := os.Remove(filepath.Join(dir, tc.file)); err != nil {
			t.Fatal(err)
		}
	}
	for task, want := range map[string]string{
		`authorized_keys: {for_user_account: deploy, key_files_on_controller_machine: [files/missing.keys], keys_not_listed: keep}`:           "key_files_on_controller_machine[0]",
		`authorized_keys: {for_user_account: deploy, key_directories_on_controller_machine: [files/missing], keys_not_listed: keep}`:          "key directory files/missing",
		`authorized_keys: {for_user_account: deploy, key_files_on_controller_machine: ["{{ .vars.file }}"], keys_not_listed: keep}`:           "reference .vars.file is not declared for node ",
		`authorized_keys: {for_user_account: deploy, key_files_on_controller_machine: [../outside.keys], keys_not_listed: keep}`:              "leaves",
		`authorized_keys: {for_user_account: deploy, keys_not_listed: keep}`:                                                                  "needs keys, key_files_on_controller_machine, or key_directories_on_controller_machine",
		`authorized_keys: {for_user_account: deploy, key_files_on_controller_machine: [files/ci.keys], state: absent, keys_not_listed: keep}`: "accepts only for_user_account, keys, key_files_on_controller_machine, key_directories_on_controller_machine, and state",
	} {
		if _, err := loadTask(t, task); err == nil || !strings.Contains(err.Error(), want) {
			t.Error(task, err)
		}
	}
}

func TestAccessControlAndCapabilityConfiguration(t *testing.T) {
	for _, task := range []string{
		`file_access_control_list: {path: /var/log/app, entries: [{user_account: promtail, permissions: r-x}, {group: support, permissions: r--}], entries_not_listed: remove, also_apply_to_everything_inside: true, also_use_for_new_files_inside: true}`,
		`file_access_control_list: {path: /srv/data, entries: [], entries_not_listed: remove}`,
		`file_access_control_list: {path: "{{ .vars.path }}", entries: [{user_account: "{{ .node.name }}", permissions: rwx}], entries_not_listed: keep}`,
		`file_access_control_list: {path: /srv/data, state: absent, also_apply_to_everything_inside: true}`,
		`linux_capability_on_program_file: {capabilities_not_listed: remove, path: /usr/local/bin/pinger, capabilities: [cap_net_raw, cap_net_bind_service]}`,
		`linux_capability_on_program_file: {path: /usr/local/bin/pinger, state: absent}`,
	} {
		p := fixture(t, "vars: {path: /srv/data}\nfleet_stages: [{id: configure, nodes: all, node_tasks: [{"+task+"}]}]\n")
		project, _, err := Load(p, "example", "", nil)
		if err != nil {
			t.Fatal(task, err)
		}
		if action, _ := project.FleetStages[0].NodeTasks[0].Action(); !action.RetryByDefault {
			t.Fatal("resource did not take the bounded retry default", task)
		}
	}
	for task, want := range map[string]string{
		`file_access_control_list: {path: /srv, entries: [{user_account: a, permissions: r-x}]}`:                                                                "entries_not_listed is required",
		`file_access_control_list: {path: /srv, entries_not_listed: keep}`:                                                                                      "entries is required",
		`file_access_control_list: {path: /srv, entries: [{permissions: r-x}], entries_not_listed: keep}`:                                                       "exactly one of user_account or group",
		`file_access_control_list: {path: /srv, entries: [{user_account: a, group: b, permissions: r-x}], entries_not_listed: keep}`:                            "exactly one of user_account or group",
		`file_access_control_list: {path: /srv, entries: [{user_account: a, permissions: rx}], entries_not_listed: keep}`:                                       "three characters",
		`file_access_control_list: {path: /srv, entries: [{user_account: a, permissions: x-r}], entries_not_listed: keep}`:                                      "three characters",
		`file_access_control_list: {path: /srv, entries: [{user_account: a, permissions: r--}, {user_account: a, permissions: rw-}], entries_not_listed: keep}`: "listed twice",
		`file_access_control_list: {path: /srv, entries: [], entries_not_listed: keep, state: absent}`:                                                          "accepts only path",
		`file_access_control_list: {path: srv, entries: [], entries_not_listed: keep}`:                                                                          "absolute",
		`file_access_control_list: {path: /srv, entries: [{group: "-x", permissions: r--}], entries_not_listed: keep}`:                                          "invalid account name",
		`linux_capability_on_program_file: {capabilities_not_listed: remove, path: /usr/local/bin/pinger}`:                                                      "capabilities is required",
		`linux_capability_on_program_file: {capabilities_not_listed: remove, path: /usr/local/bin/pinger, capabilities: [CAP_NET_RAW]}`:                         "not a Linux capability",
		`linux_capability_on_program_file: {capabilities_not_listed: remove, path: /usr/local/bin/pinger, capabilities: [cap_net_raw, cap_net_raw]}`:            "listed twice",
		`linux_capability_on_program_file: {path: /usr/local/bin/pinger, capabilities: [cap_net_raw], state: absent}`:                                           "accepts only path and state",
		`linux_capability_on_program_file: {capabilities_not_listed: remove, path: pinger, capabilities: [cap_net_raw]}`:                                        "absolute",
	} {
		if _, err := loadTask(t, task); err == nil || !strings.Contains(err.Error(), want) {
			t.Error(task, err)
		}
	}
}

// Key paths may read what the controller machine knows before it connects,
// such as a node's vars: each node gets the keys of its own directory. The
// keys are saved with the run, so a resume needs none of the files.
func TestVariableKeyPathsGiveEachNodeItsKeys(t *testing.T) {
	path := variableProject(t, "  - authorized_keys: {for_user_account: deploy, key_files_on_controller_machine: [keys/ci.keys], key_directories_on_controller_machine: [\"keys/{{ .vars.site }}\"], keys_not_listed: remove}\n")
	dir := filepath.Dir(path)
	write(t, filepath.Join(dir, "keys/ci.keys"), bobKey+"\n")
	write(t, filepath.Join(dir, "keys/frankfurt/alice.pub"), aliceKey+"\n")
	const carolKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIJ6+dYwF2LfqNPAMrlXWfcNIkI5GE8sJjZoEw4M0VNYL carol@laptop"
	write(t, filepath.Join(dir, "keys/helsinki/carol.pub"), carolKey+"\n")
	p, inv, err := Load(path, "example", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !ProjectHasVariableSources(p) {
		t.Fatal("variable key paths not found")
	}
	assets := t.TempDir()
	if err := FreezeAssets(context.Background(), &p, assets, nil); err != nil {
		t.Fatal(err)
	}
	saved, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	// The files change after the run started; its saved keys do not.
	if err := os.RemoveAll(filepath.Join(dir, "keys")); err != nil {
		t.Fatal(err)
	}
	var restored Project
	if err := json.Unmarshal(saved, &restored); err != nil {
		t.Fatal(err)
	}
	if err := BindProjectAssets(&restored, assets); err != nil {
		t.Fatal(err)
	}
	for node, want := range map[string][]modules.FileKey{
		"web1": {{Line: bobKey, Place: "key_files_on_controller_machine[0] keys/ci.keys line 1"}, {Line: aliceKey, Place: "key_directories_on_controller_machine[0] keys/frankfurt/alice.pub line 1"}},
		"web2": {{Line: bobKey, Place: "key_files_on_controller_machine[0] keys/ci.keys line 1"}, {Line: carolKey, Place: "key_directories_on_controller_machine[0] keys/helsinki/carol.pub line 1"}},
	} {
		tasks, err := NodeTasksFor(restored.FleetStages[0].NodeTasks, node, assets)
		if err != nil {
			t.Fatal(err)
		}
		if len(InputsOf[modules.AuthorizedKeys](tasks[0]).Variants) != 1 {
			t.Fatalf("%s: the job carries another node's keys: %v", node, InputsOf[modules.AuthorizedKeys](tasks[0]).Variants)
		}
		sent, _ := json.Marshal(tasks)
		var received []Task
		if err := json.Unmarshal(sent, &received); err != nil {
			t.Fatal(err)
		}
		scope := value.Scope{"vars": NodeVars(p, inv, node), "node": map[string]any{"name": node}, "node_tasks": map[string]any{}}
		resolved, err := ResolveArguments(received[0], scope)
		if err != nil || !reflect.DeepEqual(InputsOf[modules.AuthorizedKeys](resolved).FileKeys, want) || InputsOf[modules.AuthorizedKeys](resolved).KeyDirectoriesOnControllerMachine[0] != "keys/"+NodeVars(p, inv, node)["site"].(string) {
			t.Fatalf("%s: %+v %v", node, InputsOf[modules.AuthorizedKeys](resolved), err)
		}
		if InputsOf[modules.AuthorizedKeys](received[0]).KeyDirectoriesOnControllerMachine[0] != "keys/{{ .vars.site }}" {
			t.Fatal("resolving changed the job's task")
		}
	}
}

func TestVariableKeyPathErrorsNameTheNodeAndThePath(t *testing.T) {
	for tasks, message := range map[string]string{
		// web2's site has no directory.
		"  - authorized_keys: {for_user_account: deploy, key_directories_on_controller_machine: [\"keys/{{ .vars.site }}\"], keys_not_listed: remove}\n":                                                         "node web2",
		"  - authorized_keys: {for_user_account: deploy, key_files_on_controller_machine: [\"keys/{{ .node.arch }}.keys\"], keys_not_listed: remove}\n":                                                          "key_files_on_controller_machine[0] reads .node.arch, which is known only during a run",
		"  - {id: probe, shell: {script: \"true\"}}\n  - authorized_keys: {for_user_account: deploy, key_directories_on_controller_machine: [\"keys/{{ .node_tasks.probe.stdout }}\"], keys_not_listed: keep}\n": "known only during a run",
		// A key in the node's directory repeats a written key.
		"  - authorized_keys: {for_user_account: deploy, keys: ['" + aliceKey + "'], key_directories_on_controller_machine: [\"keys/{{ .vars.site }}\"], keys_not_listed: keep}\n": "key listed twice",
	} {
		path := variableProject(t, tasks)
		write(t, filepath.Join(filepath.Dir(path), "keys/frankfurt/alice.pub"), aliceKey+"\n")
		_, _, err := Load(path, "example", "", nil)
		if err == nil || !strings.Contains(err.Error(), message) {
			t.Errorf("%s\nwant %q, got %v", tasks, message, err)
		}
	}
	path := variableProject(t, "  - authorized_keys: {for_user_account: deploy, key_directories_on_controller_machine: [\"keys/{{ .vars.site }}\"], keys_not_listed: remove}\n")
	write(t, filepath.Join(filepath.Dir(path), "keys/frankfurt/alice.pub"), aliceKey+"\n")
	if _, _, err := Load(path, "example", "", nil); err == nil || !strings.Contains(err.Error(), "node web2") ||
		!strings.Contains(err.Error(), "authorized_keys.key_directories_on_controller_machine[0]") || !strings.Contains(err.Error(), "keys/helsinki") {
		t.Fatal(err)
	}
}

// A key directory without a .pub file is an error, never an empty list: with
// keys_not_listed: remove it would empty authorized_keys and lock out every
// administrator. Someone who means "no keys" writes keys: [].
func TestKeyDirectoryWithoutPubFilesFails(t *testing.T) {
	p := fixture(t, "fleet_stages: [{id: configure, nodes: all, node_tasks: [{authorized_keys: {for_user_account: deploy, key_directories_on_controller_machine: [files/production_deploy], keys_not_listed: remove}}]}]\n")
	dir := filepath.Dir(p)
	write(t, filepath.Join(dir, "files/production_deploy/alice_ed25519"), aliceKey+"\n")
	write(t, filepath.Join(dir, "files/production_deploy/bob.key"), bobKey+"\n")
	_, _, err := Load(p, "example", "", nil)
	if err == nil || !strings.Contains(err.Error(), "authorized_keys.key_directories_on_controller_machine[0]") ||
		!strings.Contains(err.Error(), "key directory files/production_deploy holds no file ending in .pub") || !strings.Contains(err.Error(), "keys: []") {
		t.Fatal(err)
	}
	// For a path chosen by a var, the error names the node whose directory is empty.
	p = variableProject(t, "  - authorized_keys: {for_user_account: deploy, key_directories_on_controller_machine: [\"keys/{{ .vars.site }}\"], keys_not_listed: remove}\n")
	dir = filepath.Dir(p)
	write(t, filepath.Join(dir, "keys/frankfurt/alice.pub"), aliceKey+"\n")
	write(t, filepath.Join(dir, "keys/helsinki/bob.key"), bobKey+"\n")
	_, _, err = Load(p, "example", "", nil)
	if err == nil || !strings.Contains(err.Error(), "node web2") || !strings.Contains(err.Error(), "authorized_keys.key_directories_on_controller_machine[0]") ||
		!strings.Contains(err.Error(), "key directory keys/helsinki holds no file ending in .pub") {
		t.Fatal(err)
	}
	// keys: [] still empties the file.
	if _, err := loadTask(t, `authorized_keys: {for_user_account: deploy, keys: [], keys_not_listed: remove}`); err != nil {
		t.Fatal(err)
	}
}
