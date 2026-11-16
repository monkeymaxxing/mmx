package config

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const oneTimeStage = "fleet_stages: [{id: remove, nodes: all, node_tasks: [{file: {path: /etc/old, state: absent}}]}]\n"

func TestOneTimePlaybookNamesAreADateAndLowercaseWords(t *testing.T) {
	for name, valid := range map[string]bool{
		"2026-10-05_remove_old_ruby_manager": true,
		"2026-10-05_remove_php7":             true,
		"2026-02-30_not_a_date":              false,
		"2026-10-05":                         false,
		"2026-10-05_":                        false,
		"2026-10-05_Remove":                  false,
		"2026-10-05_remove__twice":           false,
		"2026-10-05_remove-old":              false,
		"26-10-05_remove":                    false,
		"remove_old_ruby_manager":            false,
		"../2026-10-05_remove":               false,
	} {
		if IsOneTimePlaybookName(name) != valid {
			t.Errorf("%s: valid=%v, want %v", name, !valid, valid)
		}
	}
}

func TestCommandsSelectAOneTimePlaybookByItsName(t *testing.T) {
	path := fixture(t, "")
	root := filepath.Dir(path)
	write(t, filepath.Join(root, "one_time_playbooks/2026-10-05_remove_old_ruby_manager.yml"), oneTimeStage)
	p, _, err := Load(path, "2026-10-05_remove_old_ruby_manager", "", nil)
	if err != nil || p.PlaybookName != "2026-10-05_remove_old_ruby_manager" || len(p.FleetStages) != 1 {
		t.Fatal(p.PlaybookName, err)
	}
	// The same name in playbooks/ is not a one-time playbook, and the loader
	// never looks for it there.
	if _, _, err := Load(path, "2026-10-06_not_there", "", nil); err == nil || !strings.Contains(err.Error(), "one_time_playbooks") {
		t.Fatal(err)
	}
	for _, name := range []string{"../playbooks/example", "2026-10-05_Remove", "one_time_playbooks/2026-10-05_remove_old_ruby_manager"} {
		if _, _, err := Load(path, name, "", nil); err == nil || !strings.Contains(err.Error(), "playbook must be a name, not a path") {
			t.Fatal(name, err)
		}
	}
}

func TestACompletedOneTimePlaybookCannotBeLoaded(t *testing.T) {
	path := fixture(t, "")
	root := filepath.Dir(path)
	write(t, filepath.Join(root, "one_time_playbooks/completed/2026-09-12_stop_old_metrics_agent.yml"), "# Completed 2026-10-19: done on all 12 servers.\n"+oneTimeStage)
	_, _, err := Load(path, "2026-09-12_stop_old_metrics_agent", "", nil)
	var completed *CompletedOneTimePlaybookError
	if !errors.As(err, &completed) || completed.Name != "2026-09-12_stop_old_metrics_agent" {
		t.Fatal(err)
	}
	want := "one-time playbook 2026-09-12_stop_old_metrics_agent is completed: its file is one_time_playbooks/completed/2026-09-12_stop_old_metrics_agent.yml, and a completed one-time playbook cannot be run, dry-run, or checked; move the file back to one_time_playbooks/ to make it runnable again"
	if err.Error() != want {
		t.Fatal(err)
	}
	// Moved back, it is an ordinary one-time playbook again.
	if err := os.Rename(CompletedOneTimePlaybookPath(path, completed.Name), OneTimePlaybookPath(path, completed.Name)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Load(path, completed.Name, "", nil); err != nil {
		t.Fatal(err)
	}
}

func TestOutstandingOneTimePlaybooksAreListedOldestFirstWithoutCompleted(t *testing.T) {
	path := fixture(t, "")
	root := filepath.Dir(path)
	if names, err := OutstandingOneTimePlaybooks(path); err != nil || len(names) != 0 {
		t.Fatal(names, err)
	}
	write(t, filepath.Join(root, "one_time_playbooks/2026-10-05_remove_old_ruby_manager.yml"), oneTimeStage)
	write(t, filepath.Join(root, "one_time_playbooks/2026-09-12_stop_old_metrics_agent.yml"), oneTimeStage)
	write(t, filepath.Join(root, "one_time_playbooks/completed/2026-08-01_done_long_ago.yml"), "not even YAML: [")
	write(t, filepath.Join(root, "one_time_playbooks/completed/anything.txt"), "")
	// What a file manager or Git leaves there is no playbook.
	write(t, filepath.Join(root, "one_time_playbooks/.DS_Store"), "")
	write(t, filepath.Join(root, "one_time_playbooks/.gitkeep"), "")
	names, err := OutstandingOneTimePlaybooks(path)
	if err != nil || !slices.Equal(names, []string{"2026-09-12_stop_old_metrics_agent", "2026-10-05_remove_old_ruby_manager"}) {
		t.Fatal(names, err)
	}
	if _, err := ValidateProject(path); err != nil {
		t.Fatal(err)
	}
	for _, other := range []string{"remove_old_ruby_manager.yml", "2026-10-05_remove.yaml", "2026-13-01_no_such_month.yml", "notes.txt", "drafts/keep"} {
		file := filepath.Join(root, "one_time_playbooks", other)
		write(t, file, oneTimeStage)
		first, _, _ := strings.Cut(other, "/")
		want := "one_time_playbooks/" + first + ": one_time_playbooks/ accepts only one-time playbook files and the completed/ directory; name each one-time playbook file using YYYY-MM-DD_what_it_does"
		if _, err := OutstandingOneTimePlaybooks(path); err == nil || !strings.HasPrefix(err.Error(), want) {
			t.Fatal(other, err)
		}
		if _, err := ValidateProject(path); err == nil || !strings.HasPrefix(err.Error(), want) {
			t.Fatal(other, err)
		}
		if err := os.RemoveAll(filepath.Join(root, "one_time_playbooks", first)); err != nil {
			t.Fatal(err)
		}
	}
}

func TestValidateChecksEveryOutstandingOneTimePlaybook(t *testing.T) {
	path := fixture(t, "")
	root := filepath.Dir(path)
	file := filepath.Join(root, "one_time_playbooks/2026-10-05_remove_old_ruby_manager.yml")
	write(t, file, "fleet_stages: [{id: remove, nodes: no_such_group, node_tasks: [{file: {path: /etc/old, state: absent}}]}]\n")
	// A node that the inventory no longer has, such as one removed after
	// the playbook's work: the reason says where a playbook that no node
	// needs goes. An ordinary playbook's reason does not.
	hint := `no node or inventory group named "no_such_group"; a one-time playbook that no node needs any more goes to one_time_playbooks/completed/`
	if _, err := ValidateProject(path); err == nil || !strings.HasSuffix(err.Error(), hint) {
		t.Fatal(err)
	}
	write(t, file, oneTimeStage)
	write(t, filepath.Join(root, "playbooks/deploy.yml"), "fleet_stages: [{id: remove, nodes: no_such_group, node_tasks: [{show: hi}]}]\n")
	if _, err := ValidateProject(path); err == nil || !strings.HasSuffix(err.Error(), `no node or inventory group named "no_such_group"`) {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "playbooks/deploy.yml")); err != nil {
		t.Fatal(err)
	}
	// A one-time playbook is checked on the nodes without --var.
	write(t, file, "command_line_vars: {release: {type: string}}\n"+oneTimeStage)
	want := "command-line variable release of a one-time playbook needs a default: mmx check-one-time-playbooks inspects the nodes without --var"
	if _, err := ValidateProject(path); err == nil || !strings.Contains(err.Error(), want) {
		t.Fatal(err)
	}
	write(t, file, "command_line_vars: {release: {type: string, default: \"1.4.3\"}}\n"+oneTimeStage)
	if _, err := ValidateProject(path); err != nil {
		t.Fatal(err)
	}
	// An ordinary playbook keeps its command-line variables without defaults.
	write(t, filepath.Join(root, "playbooks/deploy.yml"), "command_line_vars: {release: {type: string}}\n"+oneTimeStage)
	if _, err := ValidateProject(path); err != nil {
		t.Fatal(err)
	}
}

func TestModuleUsageReadsOutstandingOneTimePlaybooks(t *testing.T) {
	path := fixture(t, "")
	root := filepath.Dir(path)
	write(t, filepath.Join(root, "one_time_playbooks/2026-10-05_remove_old_ruby_manager.yml"), oneTimeStage)
	usage, err := ModuleUsageOf(path, nil)
	if err != nil || len(usage.Modules) != 1 || usage.Modules[0].Name != "file" || usage.Modules[0].Calls[0].File != "one_time_playbooks/2026-10-05_remove_old_ruby_manager.yml" {
		t.Fatal(usage, err)
	}
}

func TestAMissingPlaybookOrInventoryNamesItsFileAndWhatTheProjectHas(t *testing.T) {
	path := fixture(t, oneTimeStage)
	root := filepath.Dir(path)
	write(t, filepath.Join(root, "one_time_playbooks/2026-10-05_remove_old_ruby_manager.yml"), oneTimeStage)
	write(t, filepath.Join(root, "inventories/staging.yml"), "nodes:\n  web1: {address: staging.example.com}\n")
	for name, want := range map[string]string{
		"deploy":               "there is no playbook deploy: playbooks/deploy.yml does not exist; the project's playbooks are: example",
		"2026-10-06_not_there": "there is no one-time playbook 2026-10-06_not_there: one_time_playbooks/2026-10-06_not_there.yml does not exist; the one-time playbooks outside completed/ are: 2026-10-05_remove_old_ruby_manager",
	} {
		if _, _, err := Load(path, name, "", nil); err == nil || err.Error() != want {
			t.Fatal(name, err)
		}
	}
	// A name from --inventory is not the settings file's default_inventory.
	want := "there is no inventory production: inventories/production.yml does not exist; the project's inventories are: example, staging"
	if _, _, err := Load(path, "example", "production", nil); err == nil || err.Error() != want {
		t.Fatal(err)
	}
	write(t, filepath.Join(root, "inventories/staging.yml"), "nodes: {web1: {address: staging.example.com, no_such_field: 1}}\n")
	if _, _, err := Load(path, "example", "staging", nil); err == nil || !strings.HasPrefix(err.Error(), "inventory staging: inventories/staging.yml:") || strings.Contains(err.Error(), "default_inventory") {
		t.Fatal(err)
	}
	// The default inventory's error names the setting that chose it.
	write(t, path, "project_name: test\ndefault_inventory: gone\n")
	if _, _, err := Load(path, "example", "", nil); err == nil || !strings.HasSuffix(err.Error(), "(default_inventory): there is no inventory gone: inventories/gone.yml does not exist; the project's inventories are: example, staging") {
		t.Fatal(err)
	}
}
