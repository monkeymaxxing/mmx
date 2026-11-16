package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// completedFirstLine is the line the command adds, dated with the day of
// the call, or the day before it at midnight.
func completedFirstLine(t *testing.T, text string, before time.Time, done string) string {
	t.Helper()
	for _, day := range []time.Time{before, time.Now()} {
		if line := "# Completed " + day.Format("2006-01-02") + ": " + done + ".\n"; strings.HasPrefix(text, line) {
			return line
		}
	}
	t.Fatalf("the completed playbook does not start with its completion line for %q:\n%s", done, text)
	return ""
}

func TestCompleteOneTimePlaybookMovesAPlaybookThatNoNodeNeeds(t *testing.T) {
	root := oneTimeProject(t)
	standIn := useNodesStandIn(t, nil)
	file, completed := "one_time_playbooks/"+removeOldRubyManager+".yml", "one_time_playbooks/completed/"+removeOldRubyManager+".yml"
	original, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(file, 0640); err != nil {
		t.Fatal(err)
	}
	before := time.Now()
	out, code, err := captureCommand(t, "complete-one-time-playbook", removeOldRubyManager)
	want := "Inventory: example (inventories/example.yml)\n\n" +
		removeOldRubyManager + "\n" +
		"  done on all 4 servers\n" +
		"\nCompleted: moved " + file + " to " + completed + ".\n"
	if code != 0 || err != nil || out != want {
		t.Fatalf("%d %v, got\n%s\nwant\n%s", code, err, out, want)
	}
	// The file is in completed/ with its new first line and its unchanged
	// text and permissions, and no longer in one_time_playbooks/.
	text, err := os.ReadFile(completed)
	if err != nil {
		t.Fatal(err)
	}
	if line := completedFirstLine(t, string(text), before, "done on all 4 servers"); string(text) != line+string(original) {
		t.Fatalf("the playbook's text changed:\n%s", text)
	}
	if info, err := os.Stat(completed); err != nil || info.Mode().Perm() != 0640 {
		t.Fatal(info, err)
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Fatal("the playbook is still in one_time_playbooks/", err)
	}
	if got := standIn.playbooks(); len(got) != 1 || got[0] != removeOldRubyManager {
		t.Fatal("the command inspects only the playbook it names:", got)
	}
	// The project stays valid, the check ignores the completed playbook, and
	// no command runs, dry-runs, or completes it again.
	if _, code, err := captureCommand(t, "validate"); code != 0 || err != nil {
		t.Fatal(code, err)
	}
	if out, code, err := captureCommand(t, "check-one-time-playbooks"); code != 0 || err != nil || strings.Contains(out, removeOldRubyManager) || !strings.Contains(out, stopOldMetricsAgent+"\n  done on all 2 servers\n") {
		t.Fatal(code, err, out)
	}
	for _, command := range []string{"complete-one-time-playbook", "dryrun", "run", "validate"} {
		_, code, err := captureCommand(t, command, removeOldRubyManager)
		if want := "one-time playbook " + removeOldRubyManager + " is completed: its file is " + completed + ", and a completed one-time playbook cannot be run, dry-run, or checked; move the file back to one_time_playbooks/ to make it runnable again"; code != 4 || err == nil || err.Error() != want {
			t.Fatal(command, code, err)
		}
	}
	if len(standIn.dryruns) != 2 {
		t.Fatal("a completed playbook was inspected")
	}

	// One server is written in the singular, and the name may follow the options.
	writeProjectFile(t, "inventories/single.yml", "nodes:\n  only01: {address: only01.example.net}\n")
	writeProjectFile(t, "one_time_playbooks/2026-10-06_remove_one.yml", "fleet_stages: [{id: remove_one, nodes: all, node_tasks: [{file: {path: /etc/one, state: absent}}]}]\n")
	before = time.Now()
	out, code, err = captureCommand(t, "complete-one-time-playbook", "--inventory", "single", "--json", "2026-10-06_remove_one")
	var completion oneTimePlaybookCompletion
	if code != 0 || err != nil || json.Unmarshal([]byte(out), &completion) != nil {
		t.Fatal(code, err, out)
	}
	// JSON names the new file by its absolute path.
	if completion.SchemaVersion != 1 || completion.Inventory != "single" || completion.Status != "completed" || completion.CompletedPlaybookFile != filepath.Join(root, "one_time_playbooks/completed/2026-10-06_remove_one.yml") ||
		strings.Join(completion.InventoriesNotInspected, ",") != "example" || len(completion.OneTimePlaybooks) != 1 || completion.OneTimePlaybooks[0].RunID != standIn.dryruns[2].ID ||
		len(completion.OneTimePlaybooks[0].Done) != 1 || completion.OneTimePlaybooks[0].Done[0].Node != "only01" {
		t.Fatal(out)
	}
	text, err = os.ReadFile("one_time_playbooks/completed/2026-10-06_remove_one.yml")
	if err != nil {
		t.Fatal(err)
	}
	completedFirstLine(t, string(text), before, "done on the 1 server")
}

func TestCompleteOneTimePlaybookRefusesWhileANodeNeedsItOrIsNotKnown(t *testing.T) {
	oneTimeProject(t)
	file := "one_time_playbooks/" + removeOldRubyManager + ".yml"
	original, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	unchangedOnDisk := func() {
		t.Helper()
		if text, err := os.ReadFile(file); err != nil || string(text) != string(original) {
			t.Fatal("a refused command changed the playbook", err)
		}
		if _, err := os.Stat("one_time_playbooks/completed"); !os.IsNotExist(err) {
			t.Fatal("a refused command wrote into completed/", err)
		}
	}
	for _, tc := range []struct {
		name          string
		answers       map[string]nodeAnswer
		code          int
		report, cause string
	}{
		{"a node still needs it", map[string]nodeAnswer{"app02": wouldChange}, 2,
			"  done on 3 of 4 servers\n  still needed on 1 server: app02\n",
			"1 node still needs the one-time playbook; run the one-time playbook with mmx run " + removeOldRubyManager + "; mmx results "},
		{"two nodes still need it", map[string]nodeAnswer{"app02": wouldChange, "app04": wouldChange}, 2,
			"  done on 2 of 4 servers\n  still needed on 2 servers: app02, app04\n",
			"2 nodes still need the one-time playbook; run the one-time playbook with mmx run " + removeOldRubyManager + "; mmx results "},
		{"a node is not known", map[string]nodeAnswer{"app03": serverIsDown("app03")}, 3,
			"  done on 3 of 4 servers\n  not known on 1 server: app03 (" + serverIsDownReason("app03") + ")\n",
			"the state of 1 server is not known; mmx results "},
		{"a node still needs it and a node is not known", map[string]nodeAnswer{"app01": wouldChange, "app02": serverIsDown("app02"), "app03": serverIsDown("app03")}, 3,
			"  done on 1 of 4 servers\n  still needed on 1 server: app01\n  not known on 2 servers: app02 (" + serverIsDownReason("app02") + "), app03 (" + serverIsDownReason("app03") + ")\n",
			"1 node still needs the one-time playbook, and the state of 2 servers is not known; run the one-time playbook with mmx run " + removeOldRubyManager + "; mmx results "},
	} {
		standIn := useNodesStandIn(t, map[string]map[string]nodeAnswer{removeOldRubyManager: tc.answers})
		out, code, err := captureCommand(t, "complete-one-time-playbook", removeOldRubyManager)
		if want := "Inventory: example (inventories/example.yml)\n\n" + removeOldRubyManager + "\n" + tc.report; code != tc.code || out != want {
			t.Fatalf("%s: exit code %d, got\n%s\nwant\n%s", tc.name, code, out, want)
		}
		if want := "one-time playbook " + removeOldRubyManager + " is not completed and stays in one_time_playbooks/: " + tc.cause; err == nil || !strings.HasPrefix(err.Error(), want) ||
			!strings.HasSuffix(err.Error(), "; mmx results "+shortRunID(standIn)+" --diff shows what the check found on each server") {
			t.Fatalf("%s: %v", tc.name, err)
		}
		unchangedOnDisk()
		out, code, err = captureCommand(t, "complete-one-time-playbook", removeOldRubyManager, "--json")
		var completion oneTimePlaybookCompletion
		if code != tc.code || err == nil || json.Unmarshal([]byte(out), &completion) != nil || completion.Status != "refused" || completion.CompletedPlaybookFile != "" || strings.Contains(out, "completed_playbook_file") ||
			len(completion.OneTimePlaybooks) != 1 || len(completion.OneTimePlaybooks[0].StillNeeded)+len(completion.OneTimePlaybooks[0].NotKnown) != len(tc.answers) {
			t.Fatalf("%s: %d %v\n%s", tc.name, code, err, out)
		}
		unchangedOnDisk()
	}
}

func shortRunID(standIn *nodesStandIn) string {
	id := standIn.dryruns[len(standIn.dryruns)-1].ID
	return id[strings.Index(id, "-")+1:][:8]
}

// The command inspects only the nodes of the inventory it was given, and
// names the project's other inventories in a note.
func TestCompleteOneTimePlaybookNamesTheInventoriesItDidNotInspect(t *testing.T) {
	oneTimeProject(t)
	writeProjectFile(t, "inventories/production.yml", "nodes:\n  production01: {address: production01.example.net}\n  production02: {address: production02.example.net}\n")
	writeProjectFile(t, "inventories/staging.yml", "nodes:\n  staging01: {address: staging01.example.net}\n")
	useNodesStandIn(t, map[string]map[string]nodeAnswer{removeOldRubyManager: {"production02": wouldChange}})
	out, code, err := captureCommand(t, "complete-one-time-playbook", removeOldRubyManager, "--inventory", "production")
	note := "\nNote: MMX inspected nodes only in inventory production. MMX did not inspect nodes in other inventories, example and staging. Before completing a one-time playbook, check every inventory with mmx check-one-time-playbooks --inventory NAME.\n"
	if want := "Inventory: production (inventories/production.yml)\n\n" + removeOldRubyManager + "\n  done on 1 of 2 servers\n  still needed on 1 server: production02\n" + note; code != 2 || err == nil || out != want {
		t.Fatalf("%d %v, got\n%s\nwant\n%s", code, err, out, want)
	}
	out, code, err = captureCommand(t, "complete-one-time-playbook", removeOldRubyManager, "--inventory", "production", "--json")
	var completion oneTimePlaybookCompletion
	if code != 2 || err == nil || json.Unmarshal([]byte(out), &completion) != nil || completion.Inventory != "production" || strings.Join(completion.InventoriesNotInspected, ",") != "example,staging" {
		t.Fatal(code, err, out)
	}
	// The same check with another inventory completes the playbook, and the
	// note then says how to make it runnable again for the others.
	out, code, err = captureCommand(t, "complete-one-time-playbook", removeOldRubyManager, "--inventory", "staging")
	want := "Inventory: staging (inventories/staging.yml)\n\n" + removeOldRubyManager + "\n  done on the 1 server\n" +
		"\nCompleted: moved one_time_playbooks/" + removeOldRubyManager + ".yml to one_time_playbooks/completed/" + removeOldRubyManager + ".yml.\n" +
		"\nNote: MMX inspected nodes only in inventory staging. MMX did not inspect nodes in other inventories, example and production. Before completing a one-time playbook, check every inventory with mmx check-one-time-playbooks --inventory NAME.\n" +
		"If a node in another inventory still needs the completed one-time playbook, move the playbook file back to one_time_playbooks/ and run the playbook with the inventory that contains the node.\n"
	if code != 0 || err != nil || out != want {
		t.Fatalf("%d %v, got\n%s\nwant\n%s", code, err, out, want)
	}
	// With one other inventory the note names it; with none there is no note.
	if err := os.Remove("inventories/production.yml"); err != nil {
		t.Fatal(err)
	}
	out, code, err = captureCommand(t, "complete-one-time-playbook", stopOldMetricsAgent)
	if code != 0 || err != nil || !strings.Contains(out, "MMX did not inspect nodes in another inventory, staging.") {
		t.Fatal(code, err, out)
	}
	if err := os.Remove("inventories/staging.yml"); err != nil {
		t.Fatal(err)
	}
	writeProjectFile(t, "one_time_playbooks/2026-10-07_remove_more.yml", "fleet_stages: [{id: remove_more, nodes: all, node_tasks: [{file: {path: /etc/more, state: absent}}]}]\n")
	out, code, err = captureCommand(t, "complete-one-time-playbook", "2026-10-07_remove_more", "--json")
	if code != 0 || err != nil || strings.Contains(out, "inventories_not_inspected") {
		t.Fatal(code, err, out)
	}
	if out, code, err = captureCommand(t, "check-one-time-playbooks"); code != 0 || err != nil || !strings.Contains(out, "No outstanding one-time playbook") {
		t.Fatal(code, err, out)
	}
}

// From a subdirectory of the project, the command names each file as the
// working directory reaches it, in a success and in a refusal alike. JSON
// names the completed file by its absolute path.
func TestCompleteOneTimePlaybookNamesFilesFromASubdirectory(t *testing.T) {
	root := oneTimeProject(t)
	useNodesStandIn(t, nil)
	t.Chdir(filepath.Join(root, "playbooks"))
	out, code, err := captureCommand(t, "complete-one-time-playbook", removeOldRubyManager)
	want := "Inventory: example (../inventories/example.yml)\n\n" + removeOldRubyManager + "\n  done on all 4 servers\n" +
		"\nCompleted: moved ../one_time_playbooks/" + removeOldRubyManager + ".yml to ../one_time_playbooks/completed/" + removeOldRubyManager + ".yml.\n"
	if code != 0 || err != nil || out != want {
		t.Fatalf("%d %v, got\n%s\nwant\n%s", code, err, out, want)
	}
	// The same name written again: MMX cannot move it over the completed file.
	writeProjectFile(t, "../one_time_playbooks/"+removeOldRubyManager+".yml", "fleet_stages: [{id: again, nodes: all, node_tasks: [{show: again}]}]\n")
	_, code, err = captureCommand(t, "complete-one-time-playbook", removeOldRubyManager)
	if want := "../one_time_playbooks/completed/" + removeOldRubyManager + ".yml already exists, so MMX cannot move ../one_time_playbooks/" + removeOldRubyManager + ".yml into the completed directory: MMX never replaces a completed one-time playbook, and a one-time playbook's file is in only one of the two directories. Remove or rename one of the two files"; code != 4 || err == nil || err.Error() != want {
		t.Fatal(code, err)
	}
	out, code, err = captureCommand(t, "complete-one-time-playbook", stopOldMetricsAgent, "--json")
	var completion oneTimePlaybookCompletion
	if code != 0 || err != nil || json.Unmarshal([]byte(out), &completion) != nil || completion.CompletedPlaybookFile != filepath.Join(root, "one_time_playbooks/completed", stopOldMetricsAgent+".yml") {
		t.Fatal(code, err, out)
	}
	if _, err := os.Stat(completion.CompletedPlaybookFile); err != nil || !filepath.IsAbs(completion.CompletedPlaybookFile) {
		t.Fatal(completion.CompletedPlaybookFile, err)
	}
}

func TestCompleteOneTimePlaybookRefusesWhatItCannotDo(t *testing.T) {
	oneTimeProject(t)
	standIn := useNodesStandIn(t, nil)
	file, completed := "one_time_playbooks/"+stopOldMetricsAgent+".yml", "one_time_playbooks/completed/"+stopOldMetricsAgent+".yml"
	// In the inventory staging the group that the older playbook selects is empty.
	writeProjectFile(t, "inventories/staging.yml", "nodes:\n  staging01: {address: staging01.example.net}\ngroups:\n  metrics: {nodes: []}\n")
	// A playbook whose fleet stages find no node during the run has
	// inspected nothing, so the command refuses.
	standIn.selectsNoNode = removeOldRubyManager
	out, code, err := captureCommand(t, "complete-one-time-playbook", removeOldRubyManager)
	if want := "Inventory: example (inventories/example.yml)\n\n" + removeOldRubyManager + "\n  inspected nothing: the playbook selects no node of inventory example\n\nNote: "; code != 3 || err == nil || !strings.HasPrefix(out, want) ||
		!strings.HasPrefix(err.Error(), "one-time playbook "+removeOldRubyManager+" is not completed and stays in one_time_playbooks/: inspected nothing: the playbook selects no node of inventory example; mmx results ") {
		t.Fatalf("%d %v\n%s", code, err, out)
	}
	if _, err := os.Stat("one_time_playbooks/completed"); !os.IsNotExist(err) {
		t.Fatal("a refused command wrote into completed/", err)
	}
	standIn.selectsNoNode = ""
	inspected := len(standIn.dryruns)
	for _, tc := range []struct {
		args []string
		want string
	}{
		{nil, "usage: mmx complete-one-time-playbook NAME [--inventory NAME]"},
		{[]string{removeOldRubyManager, stopOldMetricsAgent}, "complete-one-time-playbook takes one playbook name"},
		{[]string{"site"}, `"site" is not a valid one-time playbook name; expected YYYY-MM-DD_what_it_does`},
		{[]string{"one_time_playbooks/" + stopOldMetricsAgent + ".yml"}, "is not a valid one-time playbook name"},
		{[]string{"2026-01-01_never_written"}, "there is no one-time playbook 2026-01-01_never_written: one_time_playbooks/2026-01-01_never_written.yml does not exist; the one-time playbooks outside completed/ are: " + stopOldMetricsAgent + ", " + removeOldRubyManager},
		// MMX cannot load the playbook with this inventory: a command error.
		{[]string{stopOldMetricsAgent, "--inventory", "staging"}, "node selection is empty"},
		{[]string{stopOldMetricsAgent, "--inventory", "production"}, "inventory production"},
		{[]string{stopOldMetricsAgent, "--inventory", "inventories/example.yml"}, "--inventory must be a name, not a path"},
		{[]string{stopOldMetricsAgent, "--vault-password-file", ""}, "--vault-password-file requires a nonempty path"},
		// The command always names the nodes that are not done.
		{[]string{stopOldMetricsAgent, "--output", "summary"}, "flag provided but not defined: -output"},
		{[]string{stopOldMetricsAgent, "--limit", "app01"}, "flag provided but not defined: -limit"},
	} {
		if _, code, err := captureCommand(t, append([]string{"complete-one-time-playbook"}, tc.args...)...); code != 4 || err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%v: want %q, got %d %v", tc.args, tc.want, code, err)
		}
	}
	// completed/ already has the name: MMX never replaces a completed
	// playbook, and contacts no node before it says so.
	writeProjectFile(t, completed, "# Completed 2026-09-20: done on all 2 servers.\nan earlier text\n")
	_, code, err = captureCommand(t, "complete-one-time-playbook", stopOldMetricsAgent)
	if want := completed + " already exists, so MMX cannot move " + file + " into the completed directory: MMX never replaces a completed one-time playbook, and a one-time playbook's file is in only one of the two directories. Remove or rename one of the two files"; code != 4 || err == nil || err.Error() != want {
		t.Fatal(code, err)
	}
	if text, _ := os.ReadFile(completed); string(text) != "# Completed 2026-09-20: done on all 2 servers.\nan earlier text\n" {
		t.Fatal("the completed playbook was replaced")
	}
	if _, err := os.Stat(file); err != nil {
		t.Fatal("the outstanding playbook is gone", err)
	}
	if len(standIn.dryruns) != inspected {
		t.Fatal("a refused command inspected nodes")
	}
	// A file that cannot be written into completed/ loses nothing: the
	// playbook stays where it is.
	if err := os.RemoveAll("one_time_playbooks/completed"); err != nil {
		t.Fatal(err)
	}
	writeProjectFile(t, "one_time_playbooks/completed", "not a directory\n")
	_, code, err = captureCommand(t, "complete-one-time-playbook", removeOldRubyManager)
	if want := "cannot write the completed playbook file one_time_playbooks/completed/" + removeOldRubyManager + ".yml: not a directory; the playbook file one_time_playbooks/" + removeOldRubyManager + ".yml stays where it is"; code != 4 || err == nil || err.Error() != want {
		t.Fatal(code, err)
	}
	if _, err := os.Stat("one_time_playbooks/" + removeOldRubyManager + ".yml"); err != nil {
		t.Fatal("a failed move lost the playbook", err)
	}
}
