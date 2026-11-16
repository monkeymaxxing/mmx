package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mmx/internal/config"
)

// The date in a new playbook's name is the controller machine's date when
// the command ran: the day before the call, or the day after it at midnight.
func createdOneTimePlaybook(t *testing.T, root, whatItDoes string, before time.Time) string {
	t.Helper()
	for _, day := range []time.Time{before, time.Now()} {
		name := day.Format("2006-01-02") + "_" + whatItDoes
		if _, err := os.Stat(filepath.Join(root, "one_time_playbooks", name+".yml")); err == nil {
			return name
		}
	}
	entries, _ := os.ReadDir(filepath.Join(root, "one_time_playbooks"))
	t.Fatal("no one-time playbook dated today", entries)
	return ""
}

func TestNewOneTimePlaybookCreatesADatedPlaybookThatIsValidAsItIs(t *testing.T) {
	root := initializedProject(t)
	// The command finds the project root from a subdirectory and names the
	// file as that directory reaches it.
	t.Chdir(filepath.Join(root, "playbooks"))
	before := time.Now()
	out, code, err := captureCommand(t, "new-one-time-playbook", "remove_old_ruby_manager")
	if code != 0 || err != nil {
		t.Fatal(code, err)
	}
	name := createdOneTimePlaybook(t, root, "remove_old_ruby_manager", before)
	if want := "Created ../one_time_playbooks/" + name + ".yml. Replace its example node task with the work, then preview it with mmx dryrun " + name + ".\n"; out != want {
		t.Fatalf("got %q, want %q", out, want)
	}
	text, err := os.ReadFile(filepath.Join(root, "one_time_playbooks", name+".yml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"# A one-time playbook holds work that must happen once on every node it\n# selects and never again",
		"It runs only\n# when a command names it.",
		// The four commands of the playbook's life, one on each line.
		"\n# Preview what it would change:      mmx dryrun " + name + "\n" +
			"# Run it:                            mmx run " + name + "\n" +
			"# Check which nodes still need it:   mmx check-one-time-playbooks\n" +
			"# Complete it when no node needs it: mmx complete-one-time-playbook " + name + "\n",
		"\nfleet_stages:\n  - id: remove_old_ruby_manager\n    nodes: all\n\n    node_tasks:\n",
	} {
		if !strings.Contains(string(text), want) {
			t.Fatalf("missing %q in\n%s", want, text)
		}
	}
	// A refusal names the file as the success did: as the working directory
	// reaches it.
	_, code, err = captureCommand(t, "new-one-time-playbook", "remove_old_ruby_manager")
	if want := "../one_time_playbooks/" + name + ".yml already exists; a one-time playbook name cannot be reused after completion; choose different words for WHAT_IT_DOES"; code != 4 || err == nil || err.Error() != want {
		t.Fatal(code, err)
	}
	// mmx validate accepts the file as it is, alone and with the whole project.
	for _, args := range [][]string{{"validate"}, {"validate", name}} {
		if _, code, err := captureCommand(t, args...); code != 0 || err != nil {
			t.Fatal(args, code, err)
		}
	}
	// Its one fleet stage has the ID WHAT_IT_DOES, selects every node, and
	// holds one node task that only shows a text.
	out, code, err = captureCommand(t, "explain", name, "--json")
	var explained struct {
		FleetStages []struct {
			Path      string
			Nodes     []string
			NodeTasks []struct{ Action string } `json:"node_tasks"`
		} `json:"fleet_stages"`
	}
	if code != 0 || err != nil || json.Unmarshal([]byte(out), &explained) != nil {
		t.Fatal(code, err, out)
	}
	if stages := explained.FleetStages; len(stages) != 1 || stages[0].Path != "remove_old_ruby_manager" || strings.Join(stages[0].Nodes, ",") != "example" ||
		len(stages[0].NodeTasks) != 1 || stages[0].NodeTasks[0].Action != "show" {
		t.Fatal(out)
	}
	// It is an outstanding one-time playbook like one written by hand.
	names, err := config.OutstandingOneTimePlaybooks(filepath.Join(root, "mmx_project_settings.yml"))
	if err != nil || len(names) != 1 || names[0] != name {
		t.Fatal(names, err)
	}
	if _, code, err := captureCommand(t, "run"); code != 4 || err == nil || !strings.Contains(err.Error(), "one-time playbooks: "+name) {
		t.Fatal(code, err)
	}
}

func TestNewOneTimePlaybookRefusesANameThatExistsAndWordsThatAreNoName(t *testing.T) {
	root := initializedProject(t)
	before := time.Now()
	if _, code, err := captureCommand(t, "new-one-time-playbook", "stop_old_metrics_agent"); code != 0 || err != nil {
		t.Fatal(code, err)
	}
	name := createdOneTimePlaybook(t, root, "stop_old_metrics_agent", before)
	file := filepath.Join(root, "one_time_playbooks", name+".yml")
	if err := os.WriteFile(file, []byte("the user's own work\n"), 0644); err != nil {
		t.Fatal(err)
	}
	// The same name on the same day, in one_time_playbooks/ and in completed/.
	_, code, err := captureCommand(t, "new-one-time-playbook", "stop_old_metrics_agent")
	if want := "one_time_playbooks/" + name + ".yml already exists; a one-time playbook name cannot be reused after completion; choose different words for WHAT_IT_DOES"; code != 4 || err == nil || err.Error() != want {
		t.Fatal(code, err)
	}
	if text, _ := os.ReadFile(file); string(text) != "the user's own work\n" {
		t.Fatal("the existing playbook was replaced")
	}
	completed := filepath.Join(root, "one_time_playbooks/completed", name+".yml")
	if err := os.MkdirAll(filepath.Dir(completed), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(file, completed); err != nil {
		t.Fatal(err)
	}
	_, code, err = captureCommand(t, "new-one-time-playbook", "stop_old_metrics_agent")
	if code != 4 || err == nil || !strings.HasPrefix(err.Error(), "one_time_playbooks/completed/"+name+".yml already exists;") {
		t.Fatal(code, err)
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Fatal("a playbook was created beside its completed namesake", err)
	}
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"Remove_old"}, `WHAT_IT_DOES must be lowercase words joined by underscores, such as remove_old_ruby_manager; got "Remove_old"`},
		{[]string{"remove-old"}, "WHAT_IT_DOES must be lowercase words joined by underscores"},
		{[]string{"remove__old"}, "WHAT_IT_DOES must be lowercase words joined by underscores"},
		{[]string{"remove old"}, "WHAT_IT_DOES must be lowercase words joined by underscores"},
		{[]string{"2026-10-05_remove_old"}, "WHAT_IT_DOES must be lowercase words joined by underscores"},
		{[]string{"../remove"}, "WHAT_IT_DOES must be lowercase words joined by underscores"},
		{[]string{""}, "WHAT_IT_DOES must be lowercase words joined by underscores"},
		{[]string{"7zip_removal"}, `WHAT_IT_DOES must start with a letter, because it becomes the ID of the new playbook's fleet stage; got "7zip_removal"`},
		{nil, "usage: mmx new-one-time-playbook WHAT_IT_DOES"},
		{[]string{"remove_old", "and_more"}, "usage: mmx new-one-time-playbook WHAT_IT_DOES"},
		// The command writes a file and prints its path: it has no --json.
		{[]string{"--json", "remove_old"}, "flag provided but not defined: -json"},
		{[]string{"--inventory", "example", "remove_old"}, "flag provided but not defined: -inventory"},
	} {
		_, code, err := captureCommand(t, append([]string{"new-one-time-playbook"}, tc.args...)...)
		if code != 4 || err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%q: want %q, got %d %v", tc.args, tc.want, code, err)
		}
	}
	entries, err := os.ReadDir(filepath.Join(root, "one_time_playbooks"))
	if err != nil || len(entries) != 1 || entries[0].Name() != "completed" {
		t.Fatal("a refused command created a file", entries, err)
	}
	// Help creates nothing, and outside a project there is nowhere to create it.
	out, code, err := captureCommand(t, "new-one-time-playbook", "--help")
	if code != 0 || err != nil || !strings.Contains(out, "usage: mmx new-one-time-playbook WHAT_IT_DOES") {
		t.Fatal(code, err, out)
	}
	t.Chdir(t.TempDir())
	if _, code, err := captureCommand(t, "new-one-time-playbook", "remove_old"); code != 4 || err == nil || !strings.Contains(err.Error(), "no mmx_project_settings.yml found") {
		t.Fatal(code, err)
	}
	if entries, _ := os.ReadDir("."); len(entries) != 0 {
		t.Fatal("a file was created outside a project", entries)
	}
}
