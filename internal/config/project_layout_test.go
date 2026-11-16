package config

import (
	"mmx/internal/modules"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestFindProjectFromNestedDirectory(t *testing.T) {
	root := t.TempDir()
	settings := filepath.Join(root, "mmx_project_settings.yml")
	write(t, settings, "project_name: shop\ndefault_inventory: example\n")
	nested := filepath.Join(root, "playbooks", "nested")
	if err := os.MkdirAll(nested, 0700); err != nil {
		t.Fatal(err)
	}
	found, err := FindProject(nested)
	if err != nil || found != settings {
		t.Fatal(found, err)
	}
	write(t, filepath.Join(nested, "mmx_project_settings.yml"), "project_name: nested\ndefault_inventory: example\n")
	found, err = FindProject(nested)
	if err != nil || found != filepath.Join(nested, "mmx_project_settings.yml") {
		t.Fatal(found, err)
	}
	if _, err := FindProject(t.TempDir()); err == nil {
		t.Fatal("accepted directory without settings")
	}
}

func TestSettingsAndPlaybooksRejectEachOthersFieldsAndOldKeys(t *testing.T) {
	for _, key := range []string{"project", "inventory", "vault", "secrets", "defaults", "history_retention", "fleet_stages", "command_line_vars", "unknown"} {
		var project Project
		err := Decode("mmx_project_settings.yml", []byte(key+": ignored\n"), &project)
		if err == nil || !strings.Contains(err.Error(), "unknown field") {
			t.Fatalf("%s: %v", key, err)
		}
	}
	for _, key := range []string{"project_name", "default_inventory", "vault_file", "vars", "node_task_defaults", "max_nodes_at_once"} {
		var book Playbook
		err := Decode("playbooks/deploy.yml", []byte(key+": ignored\n"), &book)
		if err == nil || !strings.Contains(err.Error(), "unknown field") {
			t.Fatalf("%s: %v", key, err)
		}
	}
	var project Project
	err := Decode("settings", []byte("project_name: shop\ndefault_inventory: example\nnode_task_defaults: {time_limit: 1m, time_limit_per_try: 10s, retry: {max_tries: 2, pause_between_tries: 1s}}\nsecrets_from_env: {database_password: SHOP_DATABASE_PASSWORD}\n"), &project)
	if err != nil || project.Secrets["database_password"] != "SHOP_DATABASE_PASSWORD" || project.Defaults.Retry.MaxTries != 2 {
		t.Fatal(project, err)
	}
}

func TestInventoryRejectsOldConnectionFieldsAndBarePasswords(t *testing.T) {
	for _, key := range []string{"user", "port", "identity_file", "known_hosts", "transport", "endpoint", "runner_url"} {
		var inv Inventory
		err := Decode("inventories/example.yml", []byte("nodes: {web: {address: example.com, "+key+": old}}\n"), &inv)
		if err == nil || !strings.Contains(err.Error(), "unknown field") {
			t.Fatalf("%s: %v", key, err)
		}
	}
	var inv Inventory
	if err := Decode("inventory", []byte("vault: old.age\nnodes: {}\n"), &inv); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatal(err)
	}
	for _, password := range []string{"ssh_password", "sudo_password"} {
		for _, reference := range []string{"login", "prefix {{ .secrets.login }}", "{{ .secrets.login }} suffix", "{{ .secrets.login.value }}", "{{ .vars.login }}"} {
			path := fixture(t, "fleet_stages: [{id: setup, nodes: all, node_tasks: [{show: ready}]}]\n")
			write(t, filepath.Join(filepath.Dir(path), "inventories/example.yml"), "nodes: {web: {address: example.com, "+password+": '"+reference+"'}}\n")
			_, _, err := Load(path, "example", "", nil)
			if err == nil || !strings.Contains(err.Error(), "whole-value secret reference") {
				t.Fatalf("%s: %v", reference, err)
			}
		}
	}
	for _, reference := range []string{"{{ .secrets.login }}", "{{.secrets.login}}"} {
		if modules.ConnectionSecretName(reference) != "login" {
			t.Fatal(reference)
		}
	}
}

func TestProjectPathsAndInventoryNames(t *testing.T) {
	path := fixture(t, "fleet_stages: [{id: setup, nodes: all, node_tasks: [{file: {permissions_mode: '0644', path: /etc/app, source: files/app}}, {role: app}]}]\n")
	root := filepath.Dir(path)
	write(t, filepath.Join(root, "files/app"), "project file")
	write(t, filepath.Join(root, "roles/app/files/app"), "role file")
	write(t, filepath.Join(root, "roles/app/main.yml"), "node_tasks: [{file: {permissions_mode: '0644', path: /etc/role, source: files/app}}]\n")
	p, inv, err := Load(path, "example", "example", nil)
	if err != nil {
		t.Fatal(err)
	}
	tasks := p.FleetStages[0].NodeTasks
	if InputsOf[modules.File](tasks[0]).LocalPath != filepath.Join(root, "files/app") || InputsOf[modules.File](tasks[1].RoleNodeTasks[0]).LocalPath != filepath.Join(root, "roles/app/files/app") {
		t.Fatal(tasks)
	}
	if p.Vault != filepath.Join(root, "secrets_vault.age") || inv.Name != "example" || inv.Nodes["web1"].Transport != "https_with_ssh_fallback" {
		t.Fatal(p.Vault, inv)
	}
	// A relative path starts in the role and may reach other project files.
	// Leaving the project is refused. An absolute path may name any file.
	write(t, filepath.Join(root, "roles/shared/files/common"), "shared")
	write(t, filepath.Join(root, "roles/shared/tree/a"), "a")
	write(t, filepath.Join(filepath.Dir(root), "outside"), "outside")
	for task, reaches := range map[string]string{
		"file: {permissions_mode: '0644', path: /etc/x, source: ../shared/files/common}":                                   "",
		"file: {permissions_mode: '0644', path: /etc/x, template: files/../../shared/files/common}":                        "",
		"directory: {path: /etc/x, source: ../shared/tree}":                                                                "",
		"unpack_archive: {path: /srv/x, source: ../../../outside}":                                                         filepath.Dir(root),
		"file: {permissions_mode: '0644', path: /etc/x, source: files/../files/app}":                                       "",
		"file: {permissions_mode: '0644', path: /etc/x, source: " + filepath.Join(root, "roles/shared/files/common") + "}": "",
	} {
		write(t, filepath.Join(root, "roles/app/main.yml"), "node_tasks: [{"+task+"}]\n")
		_, _, err := Load(path, "example", "example", nil)
		if reaches == "" && err != nil || reaches != "" && (err == nil || !strings.Contains(err.Error(), "reaches into "+reaches) || !strings.Contains(err.Error(), "leaves "+root)) {
			t.Errorf("%s\nwant reaching into %q, got %v", task, reaches, err)
		}
	}
	for _, name := range []string{"inventories/example.yml", "../example", "/tmp/example", "example.yml"} {
		if _, _, err := Load(path, "example", name, nil); err == nil || !strings.Contains(err.Error(), "name, not a path") {
			t.Fatal(name, err)
		}
	}
	if _, _, err := Load(path, "playbooks/example.yml", "", nil); err == nil {
		t.Fatal("accepted playbook path")
	}
}

func TestOldExternalModuleKeyAndConnectionValuesAreRejected(t *testing.T) {
	var task Task
	if err := Decode("task", []byte("module: example\n"), &task); err == nil || !strings.Contains(err.Error(), "module is not a node task setting or a core module") {
		t.Fatal(err)
	}
	for _, connection := range []string{"auto", "ssh"} {
		err := checkManagedNode("web", Node{Address: "example.com", Transport: connection})
		if err == nil || !strings.Contains(err.Error(), "connection must be https_with_ssh_fallback or ssh_only") {
			t.Fatal(connection, err)
		}
	}
}

// A selection of "all" means every node, so no node may be named all: selecting
// it alone would run on every node.
func TestNodeNameAllIsRefused(t *testing.T) {
	if err := checkManagedNode("all", Node{Address: "example.com"}); err == nil || !strings.Contains(err.Error(), `node name "all" is reserved`) {
		t.Fatal(err)
	}
}

// A required project setting that is missing is named as missing.
func TestProjectSettingsSayWhichRequiredSettingIsMissing(t *testing.T) {
	for text, want := range map[string]string{
		"default_inventory: example\n": "project_name is required",
		"project_name: shop\n":         "default_inventory is required",
	} {
		path := filepath.Join(t.TempDir(), "mmx_project_settings.yml")
		write(t, path, text)
		if _, err := ReadSettings(path); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: want %q, got %v", text, want, err)
		}
	}
}
