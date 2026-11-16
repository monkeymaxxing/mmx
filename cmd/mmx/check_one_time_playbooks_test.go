package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"mmx/internal/access"
	"mmx/internal/config"
	"mmx/internal/controller"
	"mmx/internal/engine"
	"mmx/internal/protocol"
	"mmx/internal/secrets"
	"mmx/internal/value"
)

const (
	removeOldRubyManager = "2026-10-05_remove_old_ruby_manager"
	stopOldMetricsAgent  = "2026-09-12_stop_old_metrics_agent"
)

// oneTimeProject is a project with the four nodes app01 to app04 and two
// outstanding one-time playbooks: the newer one selects every node, the older
// one the two nodes of the group metrics.
func oneTimeProject(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	t.Chdir(root)
	t.Setenv("MMX_STATE_DIR", "")
	writeProjectFile(t, "mmx_project_settings.yml", "project_name: shop\ndefault_inventory: example\nrun_history_retention: forever\n")
	writeProjectFile(t, "inventories/example.yml", "nodes:\n  app01: {address: app01.example.net}\n  app02: {address: app02.example.net}\n  app03: {address: app03.example.net}\n  app04: {address: app04.example.net}\ngroups:\n  metrics: {nodes: [app01, app02]}\n")
	writeProjectFile(t, "playbooks/site.yml", "fleet_stages: [{id: site, nodes: all, node_tasks: [{show: site}]}]\n")
	writeProjectFile(t, "one_time_playbooks/"+removeOldRubyManager+".yml", "fleet_stages:\n  - id: remove_old_ruby_manager\n    nodes: all\n\n    node_tasks:\n      - directory:\n          path: /opt/rbenv\n          state: absent\n          also_delete_everything_inside: true\n\n      - file:\n          path: /etc/profile.d/rbenv.sh\n          state: absent\n")
	writeProjectFile(t, "one_time_playbooks/"+stopOldMetricsAgent+".yml", "fleet_stages: [{id: stop_old_metrics_agent, nodes: metrics, node_tasks: [{file: {path: /etc/old-metrics-agent.conf, state: absent}}]}]\n")
	return root
}

// nodeAnswer is what one node answers in the stand-in for the nodes: the
// response of each of its node jobs, and whether the controller process got
// a connection to the node at all.
type nodeAnswer struct {
	protocol.Response
	noConnection bool
}

var (
	nothingToChange = nodeAnswer{Response: protocol.Response{Status: "succeeded", Change: "unchanged", Results: []protocol.Result{{Task: "task_1", Status: "succeeded", Change: "unchanged"}, {Task: "task_2", Status: "succeeded", Change: "unchanged"}}}}
	wouldChange     = nodeAnswer{Response: protocol.Response{Status: "succeeded", Change: "would_change", Results: []protocol.Result{{Task: "task_1", Status: "succeeded", Change: "unchanged"}, {Task: "task_2", Status: "succeeded", Change: "would_change"}}}}
)

// notConnected is a node job that failed with this error before the
// controller process had a connection to the node.
func notConnected(err string) nodeAnswer {
	return nodeAnswer{Response: protocol.Response{Status: "failed", Error: err}, noConnection: true}
}

// serverIsDownError is what the controller process records for a node whose
// SSH port does not answer, and serverIsDownReason what the check says of it.
func serverIsDownError(node string) string {
	return "SSH " + node + ".example.net: exit status 255: ssh: connect to host " + node + ".example.net port 22: Connection timed out"
}
func serverIsDownReason(node string) string {
	return "MMX could not connect: " + serverIsDownError(node)
}
func serverIsDown(node string) nodeAnswer { return notConnected(serverIsDownError(node)) }

// nodesStandIn answers the dryruns of the one-time playbook commands in
// place of real nodes. The real controller code still creates each dryrun
// in the run history and selects its nodes; the stand-in then gives every
// node job the response its node has in answers, or nothingToChange.
type nodesStandIn struct {
	answers map[string]map[string]nodeAnswer // playbook name, then node
	// selectsNoNode names a playbook whose fleet stages select their nodes
	// during the run and find none.
	selectsNoNode string
	// after runs when a playbook's dryrun has its answers, before it returns.
	// It may give the dryrun what the answers cannot say, such as a fleet
	// stage whose nodes the dryrun could not find out.
	after       func(r *controller.Run)
	dryruns     []*controller.Run
	secrets     []*secrets.Store
	credentials []*access.Credentials
}

func useNodesStandIn(t *testing.T, answers map[string]map[string]nodeAnswer) *nodesStandIn {
	t.Helper()
	standIn := &nodesStandIn{answers: answers}
	original := dryrunOfOneTimePlaybook
	t.Cleanup(func() { dryrunOfOneTimePlaybook = original })
	dryrunOfOneTimePlaybook = func(ctx context.Context, c controller.Controller, p config.Project, inv config.Inventory) (*controller.Run, error) {
		r, err := c.New(ctx, p, inv, nil, true)
		if err != nil {
			return nil, err
		}
		r.Status = "succeeded"
		if p.PlaybookName == standIn.selectsNoNode {
			r.Entries = nil
		}
		for i := range r.Entries {
			en := &r.Entries[i]
			answer, ok := standIn.answers[p.PlaybookName][en.Node]
			if !ok {
				answer = nothingToChange
			}
			if !answer.noConnection {
				en.Transport = "https"
			}
			if en.Response = answer.Response; protocol.Failure(answer.Status) {
				r.Status = "failed"
			}
		}
		standIn.dryruns = append(standIn.dryruns, r)
		standIn.secrets, standIn.credentials = append(standIn.secrets, c.Secrets), append(standIn.credentials, c.Credentials)
		if standIn.after != nil {
			standIn.after(r)
		}
		return r, ctx.Err()
	}
	return standIn
}

func (s *nodesStandIn) playbooks() []string {
	names := make([]string, len(s.dryruns))
	for i, r := range s.dryruns {
		names[i] = r.Project.PlaybookName
	}
	return names
}

func TestCheckOneTimePlaybooksReportsEachNodesStateInThreeOutputForms(t *testing.T) {
	oneTimeProject(t)
	standIn := useNodesStandIn(t, map[string]map[string]nodeAnswer{
		removeOldRubyManager: {"app02": wouldChange, "app03": serverIsDown("app03")},
	})
	out, code, err := captureCommand(t, "check-one-time-playbooks")
	if code != 3 || err != nil {
		t.Fatal(code, err)
	}
	// The report lists the newer playbook first, and the states in the
	// order done, still needed, not known.
	want := "Inventory: example (inventories/example.yml)\n\n" +
		removeOldRubyManager + "\n" +
		"  done on 2 of 4 servers\n" +
		"  still needed on 1 server: app02\n" +
		"  not known on 1 server: app03 (" + serverIsDownReason("app03") + ")\n" +
		stopOldMetricsAgent + "\n" +
		"  done on all 2 servers\n"
	if out != want {
		t.Fatalf("got\n%s\nwant\n%s", out, want)
	}
	if got := standIn.playbooks(); !slices.Equal(got, []string{removeOldRubyManager, stopOldMetricsAgent}) {
		t.Fatal(got)
	}
	// Each inspection is a dryrun in the run history, of that playbook, with
	// this inventory, which mmx results reads by its ID.
	ids := map[string]string{}
	for _, r := range standIn.dryruns {
		saved, code, err := captureCommand(t, "results", r.ID, "--json")
		var view struct {
			ID, Playbook, Inventory string
			Dryrun                  bool
		}
		if code != 0 || err != nil || json.Unmarshal([]byte(saved), &view) != nil || view.ID != r.ID || !view.Dryrun || view.Playbook != r.Project.PlaybookName || view.Inventory != "example" {
			t.Fatal(code, err, saved)
		}
		ids[view.Playbook] = r.ID
	}

	// Nodes output names every node and adds each dryrun's run ID.
	standIn.dryruns = nil
	out, code, err = captureCommand(t, "check-one-time-playbooks", "--output", "nodes")
	if code != 3 || err != nil || len(standIn.dryruns) != 2 {
		t.Fatal(code, err)
	}
	first, second := controller.ShortID(standIn.dryruns[0].ID), controller.ShortID(standIn.dryruns[1].ID)
	want = "Inventory: example (inventories/example.yml)\n\n" +
		removeOldRubyManager + "\n" +
		"  done on 2 of 4 servers: app01, app04\n" +
		"  still needed on 1 server: app02\n" +
		"  not known on 1 server: app03 (" + serverIsDownReason("app03") + ")\n" +
		"  inspected by dryrun " + first + "; mmx results " + first + " --diff shows what a server still needs\n" +
		stopOldMetricsAgent + "\n" +
		"  done on all 2 servers: app01, app02\n" +
		"  inspected by dryrun " + second + "; mmx results " + second + " --diff shows what a server still needs\n"
	if out != want {
		t.Fatalf("got\n%s\nwant\n%s", out, want)
	}

	// Summary output prints only the counts.
	out, code, err = captureCommand(t, "check-one-time-playbooks", "--output=summary")
	want = "Inventory: example (inventories/example.yml)\n\n" +
		removeOldRubyManager + "\n" +
		"  done on 2 of 4 servers\n" +
		"  still needed on 1 server\n" +
		"  not known on 1 server\n" +
		stopOldMetricsAgent + "\n" +
		"  done on all 2 servers\n"
	if code != 3 || err != nil || out != want {
		t.Fatalf("%d %v, got\n%s\nwant\n%s", code, err, out, want)
	}

	// JSON carries every node with its reason, and each dryrun's full run ID.
	standIn.dryruns = nil
	out, code, err = captureCommand(t, "check-one-time-playbooks", "--json", "--output", "summary")
	var check oneTimePlaybookCheck
	if code != 3 || err != nil || json.Unmarshal([]byte(out), &check) != nil {
		t.Fatal(code, err, out)
	}
	wantCheck := oneTimePlaybookCheck{SchemaVersion: 1, Inventory: "example", OneTimePlaybooks: []checkedOneTimePlaybook{
		{Name: removeOldRubyManager, RunID: standIn.dryruns[0].ID,
			Done:        []nodeState{{"app01", "no node task would change the node"}, {"app04", "no node task would change the node"}},
			StillNeeded: []nodeState{{"app02", "node task task_2 of fleet stage remove_old_ruby_manager would change the node"}},
			NotKnown:    []nodeState{{"app03", serverIsDownReason("app03")}}},
		{Name: stopOldMetricsAgent, RunID: standIn.dryruns[1].ID,
			Done:        []nodeState{{"app01", "no node task would change the node"}, {"app02", "no node task would change the node"}},
			StillNeeded: []nodeState{}, NotKnown: []nodeState{}},
	}}
	if got, want := fmt.Sprintf("%+v", check), fmt.Sprintf("%+v", wantCheck); got != want {
		t.Fatalf("got\n%s\nwant\n%s", got, want)
	}
	// An empty list is written as a list, and a playbook that was inspected
	// has no not_inspected_reason.
	if !strings.Contains(out, `"still_needed": [],`) || !strings.Contains(out, `"not_known": []`) || strings.Contains(out, "not_inspected_reason") || !strings.Contains(out, `"schema_version": 1,`) {
		t.Fatal(out)
	}
}

func TestCheckOneTimePlaybooksExitCodes(t *testing.T) {
	oneTimeProject(t)
	for _, tc := range []struct {
		name    string
		answers map[string]map[string]nodeAnswer
		want    int
	}{
		{"every node is done", nil, 0},
		{"a node still needs a playbook", map[string]map[string]nodeAnswer{stopOldMetricsAgent: {"app01": wouldChange}}, 2},
		{"a node is not known", map[string]map[string]nodeAnswer{stopOldMetricsAgent: {"app01": serverIsDown("app01")}}, 3},
		// Incomplete goes before proposed changes, also across playbooks.
		{"not known in one playbook and still needed in another", map[string]map[string]nodeAnswer{removeOldRubyManager: {"app04": wouldChange}, stopOldMetricsAgent: {"app02": serverIsDown("app02")}}, 3},
	} {
		useNodesStandIn(t, tc.answers)
		for _, form := range [][]string{nil, {"--json"}} {
			if _, code, err := captureCommand(t, append([]string{"check-one-time-playbooks"}, form...)...); code != tc.want || err != nil {
				t.Errorf("%s %v: exit code %d, want %d (%v)", tc.name, form, code, tc.want, err)
			}
		}
	}
}

// A node's state comes from all of its node jobs of the dryrun, and a not
// known node's reason carries the cause that the dryrun recorded.
func TestANodesStateComesFromAllOfItsNodeJobs(t *testing.T) {
	unchanged := protocol.Result{Task: "remove_file", Status: "succeeded", Change: "unchanged"}
	proposed := protocol.Result{Task: "remove_directory", Status: "succeeded", Change: "would_change"}
	// A shell task without a guard, a dryrun command, or a marker file.
	notInspected := protocol.Result{Task: "raw_command", Status: "skipped", Change: "unknown", Reason: "the shell node task has no dryrun command; set dryrun to a read-only command, or run_script_in_dryrun: true for a script that changes nothing, to check the task"}
	notChecked := protocol.Result{Task: "read_token", Status: "not_checked", Change: "unknown", Reason: "/etc/token does not exist"}
	connected := func(fleetStage, node string, response protocol.Response) controller.Entry {
		return controller.Entry{FleetStage: fleetStage, Node: node, Transport: "https", Response: response}
	}
	// A node job without connection_used: the controller process had no
	// connection to the node, or the node job never started.
	withoutConnection := func(fleetStage, node, status, err string) controller.Entry {
		return controller.Entry{FleetStage: fleetStage, Node: node, Response: protocol.Response{Status: status, Error: err}}
	}
	succeeded := func(results ...protocol.Result) protocol.Response {
		return protocol.Response{Status: "succeeded", Results: results}
	}
	// The texts that the controller process and the node record, as they
	// write them.
	notLinux := `this machine does not run Linux: it answered "Darwin" (macOS) to the command uname -s. MMX manages Linux nodes only, and a macOS controller machine cannot be a node. Remove this node from the inventory, or leave it out of the fleet stage with except_nodes`
	refusedKey := "SSH app01.example.net: exit status 255: deploy@app01.example.net: Permission denied (publickey); check the node's SSH user and its key, agent, or ssh_password"
	_, sudoPassword := (&access.Credentials{}).Get("sudo password", "deploy@app01.example.net:22", "")
	if sudoPassword == nil {
		t.Fatal("a missing sudo password is no error")
	}
	doNotManage := `a run would be refused: this node refuses management because /etc/mmx/do_not_manage exists; the file says "Barbara: debugging the kernel until 2026-10-08". MMX never creates or removes this file: whoever created it removes it when MMX may manage the node again`
	for _, tc := range []struct {
		name        string
		entries     []controller.Entry
		state, want string
	}{
		{"nothing to change in two fleet stages", []controller.Entry{connected("first", "app01", succeeded(unchanged)), connected("second", "app01", succeeded(unchanged))},
			"done", "no node task would change the node"},
		{"a fleet stage's when skips the node", []controller.Entry{withoutConnection("first", "app01", "skipped", "when is false: {{ .vars.old }}")},
			"done", "no node task would change the node"},
		{"a skipped node task and an unchanged one", []controller.Entry{connected("first", "app01", succeeded(protocol.Result{Task: "marker", Status: "skipped", Change: "unchanged", Reason: "already run"}, unchanged))},
			"done", "no node task would change the node"},
		{"a proposed change in the second fleet stage", []controller.Entry{connected("first", "app01", succeeded(unchanged)), connected("second", "app01", succeeded(unchanged, proposed))},
			"still needed", "node task remove_directory of fleet stage second would change the node"},
		{"a proposed change in a loop item", []controller.Entry{connected("first", "app01", succeeded(protocol.Result{Task: "remove_each", Status: "succeeded", Change: "unchanged", Items: []protocol.Result{unchanged, proposed}}))},
			"still needed", "node task remove_each of fleet stage first would change the node"},
		// Still needed goes before not known for one node.
		{"a proposed change beside a node task that was not checked", []controller.Entry{connected("first", "app01", succeeded(notInspected)), connected("second", "app01", succeeded(proposed))},
			"still needed", "node task remove_directory of fleet stage second would change the node"},
		{"a proposed change on a node whose later fleet stage failed", []controller.Entry{connected("first", "app01", succeeded(proposed)), connected("second", "app01", protocol.Response{Status: "failed", Error: "node busy"})},
			"still needed", "node task remove_directory of fleet stage first would change the node"},
		{"a shell task that a dryrun cannot inspect", []controller.Entry{connected("first", "app01", succeeded(unchanged, notInspected))},
			"not known", "the dryrun of fleet stage first could not check node task raw_command"},
		{"a node task that was not checked, in the second fleet stage", []controller.Entry{connected("first", "app01", succeeded(unchanged)), connected("second", "app01", succeeded(notChecked, notInspected))},
			"not known", "the dryrun of fleet stage second could not check node task read_token"},
		{"a new file whose check a dryrun skipped", []controller.Entry{connected("first", "app01", succeeded(protocol.Result{Task: "write_file", Status: "succeeded", Change: "unchanged", CheckNewFile: &protocol.Result{Status: "skipped"}}))},
			"not known", "the dryrun of fleet stage first could not check node task write_file"},
		{"a node task that failed in a node job that went on", []controller.Entry{connected("first", "app01", succeeded(protocol.Result{Task: "probe", Status: "failed", Change: "unchanged"}))},
			"not known", "node task probe failed in the dryrun of fleet stage first"},

		// A failure before a connection says that MMX could not connect, and
		// then what the controller process recorded. The node's first such
		// reason counts, so the fleet stage that then did not start adds nothing.
		{"a server that is down", []controller.Entry{withoutConnection("first", "app01", "failed", serverIsDownError("app01")), withoutConnection("second", "app01", "stopped", "not started: earlier fleet stage first on app01 is failed")},
			"not known", "MMX could not connect: SSH app01.example.net: exit status 255: ssh: connect to host app01.example.net port 22: Connection timed out"},
		{"a node that is not Linux", []controller.Entry{withoutConnection("first", "app01", "failed", notLinux)},
			"not known", "MMX could not connect: " + notLinux},
		{"an SSH key that the node refuses", []controller.Entry{withoutConnection("first", "app01", "failed", refusedKey)},
			"not known", "MMX could not connect: " + refusedKey},
		{"a sudo password that MMX does not have", []controller.Entry{withoutConnection("first", "app01", "failed", sudoPassword.Error())},
			"not known", "MMX could not connect: sudo password for deploy@app01.example.net:22 is required; configure an environment variable for the credential or use an SSH key or ssh-agent (prompts are disabled)"},
		{"an error of several lines", []controller.Entry{withoutConnection("first", "app01", "failed", "SSH app01.example.net: exit status 255: kex_exchange_identification: read: Connection reset by peer\r\nConnection reset by 192.0.2.10 port 22\n")},
			"not known", "MMX could not connect: SSH app01.example.net: exit status 255: kex_exchange_identification: read: Connection reset by peer Connection reset by 192.0.2.10 port 22"},

		// A node job that did not succeed names its fleet stage, and then
		// what the node or the controller process recorded.
		{"the refusal to reboot the controller machine", []controller.Entry{connected("first", "app01", succeeded(unchanged)), connected("patch", "app01", protocol.Response{Status: "failed", Error: engine.RebootOfControllerMachineRefused})},
			"not known", "fleet stage patch: MMX does not reboot the controller machine, the machine where this mmx command runs. Run the playbook from another machine, or reboot this machine by hand after MMX finishes."},
		{"a node that refuses management", []controller.Entry{connected("first", "app01", protocol.Response{Status: "failed", Error: doNotManage, Results: []protocol.Result{unchanged}})},
			"not known", "fleet stage first: " + doNotManage},
		{"a node job whose outcome the controller process could not confirm", []controller.Entry{connected("first", "app01", protocol.Response{Status: "unknown", Error: "connection lost"})},
			"not known", "fleet stage first: connection lost"},
		// Work that another node's failure blocked had no connection either,
		// and MMX did not try to connect.
		{"a node job that another node's failure blocked", []controller.Entry{connected("first", "app01", succeeded(unchanged)), withoutConnection("second", "app01", "blocked", "requires all_nodes fleet stage first to succeed (app02 is failed)")},
			"not known", "fleet stage second: requires all_nodes fleet stage first to succeed (app02 is failed)"},
		{"a node job that a prerequisite left not checked", []controller.Entry{withoutConnection("second", "app01", "not_checked", "requires same_node fleet stage first to succeed on app01")},
			"not known", "fleet stage second: requires same_node fleet stage first to succeed on app01"},
		{"a node job that a stop kept from starting", []controller.Entry{withoutConnection("first", "app01", "stopped", "not started: stop requested")},
			"not known", "fleet stage first: not started: stop requested"},
		{"a node job that failed without a text", []controller.Entry{connected("first", "app01", protocol.Response{Status: "failed", Results: []protocol.Result{unchanged}})},
			"not known", "the dryrun of fleet stage first ended with status failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &controller.Run{ID: "20261005T142233.104233000-7f3a9c1e4b2d6a08", Dryrun: true, Status: "succeeded", InventoryName: "example", Entries: tc.entries}
			r.Project.PlaybookName = removeOldRubyManager
			checked := checkOfDryrun(r)
			states := map[string][]nodeState{"done": checked.Done, "still needed": checked.StillNeeded, "not known": checked.NotKnown}
			if got := states[tc.state]; len(got) != 1 || got[0] != (nodeState{"app01", tc.want}) || len(checked.Done)+len(checked.StillNeeded)+len(checked.NotKnown) != 1 {
				t.Fatalf("want %s: %s, got %+v", tc.state, tc.want, checked)
			}
			if checked.Name != removeOldRubyManager || checked.RunID != r.ID || checked.NotInspectedReason != "" {
				t.Fatalf("%+v", checked)
			}
			// A reason is one line.
			if strings.ContainsAny(tc.want, "\r\n") {
				t.Fatal("the wanted reason has more than one line")
			}
			// The check never calls a node done that the dryrun's exit code
			// does not call unchanged and complete.
			if tc.state == "done" && runExitCode(r) != 0 {
				t.Fatal("done, but the dryrun's exit code is", runExitCode(r))
			}
		})
	}
}

// What the dryrun did not inspect makes the whole playbook count as not
// known, whatever its node jobs say: the nodes of a fleet stage that it could
// not select, and of a loop iteration that started no node job, are in none
// of the three lists.
func TestAPlaybookWhoseDryrunDidNotInspectEveryNodeCountsAsNotKnown(t *testing.T) {
	done := controller.Entry{FleetStage: "first", Node: "app01", Transport: "https", Response: nothingToChange.Response}
	// A node job that failed after one of its node tasks proposed a change:
	// its node still needs the playbook.
	failedAfterAProposedChange := controller.Entry{FleetStage: "first", Node: "app02", Transport: "https", Response: protocol.Response{Status: "failed", Error: "node task task_2 failed",
		Results: []protocol.Result{{Task: "task_1", Status: "succeeded", Change: "would_change"}, {Task: "task_2", Status: "failed", Change: "unchanged"}}}}
	secondNotSelected := &controller.Selection{Path: "second", Response: protocol.Response{Status: "not_checked", Error: "nodes: .outputs.first is not available in a dryrun"}}
	// A for_each_node loop over app01 and app02: the node job of app01
	// failed without a connection, and with it its iteration.
	downInItsIteration := controller.Entry{FleetStage: "rollout[app01]/update", Node: "app01", Response: serverIsDown("app01").Response}
	rollout := func(app02 protocol.Response) map[string]*controller.Selection {
		return map[string]*controller.Selection{"rollout": {Path: "rollout", Loop: true, Resolved: true, Items: []value.Iteration{{Key: "app01"}, {Key: "app02"}},
			Iterations: map[string]protocol.Response{"app01": {Status: "failed", Error: "rollout[app01]/update on app01 is failed: " + serverIsDownError("app01")}, "app02": app02}}}
	}
	for _, tc := range []struct {
		name                        string
		run                         controller.Run
		done, stillNeeded, notKnown int
		want                        string
	}{
		{"no node selected during the run", controller.Run{Status: "succeeded", Selections: map[string]*controller.Selection{"first": {Path: "first", Resolved: true}}}, 0, 0, 0,
			"inspected nothing: the playbook selects no node of inventory example"},
		{"a fleet stage whose nodes the dryrun could not find out", controller.Run{Status: "succeeded", Entries: []controller.Entry{done}, Selections: map[string]*controller.Selection{
			"first": {Path: "first", Resolved: true, Nodes: []string{"app01"}}, "second": secondNotSelected}}, 1, 0, 0,
			"the dryrun did not inspect every node: fleet stage second ended with status not_checked: nodes: .outputs.first is not available in a dryrun"},
		// The node job that failed does not hide the fleet stage after it,
		// whose nodes nobody knows; without it the exit code would be 2.
		{"a fleet stage whose nodes the dryrun could not find out, after a node job that failed", controller.Run{Status: "failed", Entries: []controller.Entry{done, failedAfterAProposedChange}, Selections: map[string]*controller.Selection{
			"first": {Path: "first", Resolved: true, Nodes: []string{"app01", "app02"}}, "second": secondNotSelected}}, 1, 1, 0,
			"the dryrun did not inspect every node: fleet stage second ended with status not_checked: nodes: .outputs.first is not available in a dryrun"},
		{"a loop iteration that did not start", controller.Run{Status: "failed", Entries: []controller.Entry{done}, Selections: map[string]*controller.Selection{
			"rollout": {Path: "rollout", Loop: true, Resolved: true, Items: []value.Iteration{{Key: "app01"}, {Key: "app02"}},
				Iterations: map[string]protocol.Response{"app01": {Status: "succeeded"}, "app02": {Status: "failed", Error: "reading the node's facts for the loop's when: SSH app02: exit status 255"}}}}}, 1, 0, 0,
			"the dryrun did not inspect every node: fleet stage rollout[app02] ended with status failed: reading the node's facts for the loop's when: SSH app02: exit status 255"},
		// The node of the iteration that never started is in no list, so the
		// report names that iteration beside the node whose node job failed.
		{"a loop iteration that never started, after one whose node job failed", controller.Run{Status: "failed", Entries: []controller.Entry{downInItsIteration},
			Selections: rollout(protocol.Response{Status: "stopped", Error: "not started: on_node_failure stop_fleet_stage after rollout failed on app01"})}, 0, 0, 1,
			"the dryrun did not inspect every node: fleet stage rollout[app02] ended with status stopped: not started: on_node_failure stop_fleet_stage after rollout failed on app01"},
		{"a loop iteration without a record", controller.Run{Status: "failed", Entries: []controller.Entry{downInItsIteration}, Selections: rollout(protocol.Response{})}, 0, 0, 1,
			"the dryrun did not inspect every node: fleet stage rollout[app02] ended with status pending"},
		// An iteration whose own node job failed adds nothing: its node is in
		// the report, with the reason.
		{"a loop iteration whose node job failed", controller.Run{Status: "failed", Entries: []controller.Entry{downInItsIteration, {FleetStage: "rollout[app02]/update", Node: "app02", Transport: "https", Response: nothingToChange.Response}},
			Selections: rollout(protocol.Response{Status: "succeeded"})}, 1, 0, 1, ""},
		{"fleet stages that the dryrun did not reach", controller.Run{Status: "interrupted", FleetStagesAwaitingNodeSelection: true, Entries: []controller.Entry{done}}, 1, 0, 0,
			"the dryrun did not inspect every node: it ended with status interrupted"},
		// An iteration can fail although its node jobs succeeded, such as
		// when the loop's own outputs fail. No node job explains that.
		{"a loop iteration that failed although its node job succeeded", controller.Run{Status: "failed", Entries: []controller.Entry{{FleetStage: "rollout[app01]/update", Node: "app01", Transport: "https", Response: nothingToChange.Response}},
			Selections: map[string]*controller.Selection{"rollout": {Path: "rollout", Loop: true, Resolved: true, Items: []value.Iteration{{Key: "app01"}},
				Iterations: map[string]protocol.Response{"app01": {Status: "failed", Error: "outputs.version: node_tasks.read_version is not available"}}}}}, 1, 0, 0,
			"the dryrun did not inspect every node: fleet stage rollout[app01] ended with status failed: outputs.version: node_tasks.read_version is not available"},
		// A dryrun that did not succeed, and nothing that says why.
		{"a dryrun that ended failed without a node job that failed", controller.Run{Status: "failed", Entries: []controller.Entry{done}}, 1, 0, 0,
			"the dryrun did not inspect every node: it ended with status failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := tc.run
			r.ID, r.Dryrun, r.InventoryName = "20261005T142233.104233000-7f3a9c1e4b2d6a08", true, "example"
			checked := checkOfDryrun(&r)
			if checked.NotInspectedReason != tc.want || len(checked.Done) != tc.done || len(checked.StillNeeded) != tc.stillNeeded || len(checked.NotKnown) != tc.notKnown {
				t.Fatalf("%+v", checked)
			}
			if tc.want == "" {
				return
			}
			if !checked.countsAsNotKnown() || oneTimePlaybookExitCode([]checkedOneTimePlaybook{checked}) != 3 {
				t.Fatal("the playbook does not count as not known")
			}
			var out strings.Builder
			showCheckedOneTimePlaybook(&out, checked, "summary")
			if !strings.HasSuffix(out.String(), "\n  "+tc.want+"\n") {
				t.Fatal(out.String())
			}
		})
	}
}

// Through the commands: a fleet stage whose nodes the dryrun could not find
// out, and a loop iteration that never started, end the check with exit
// code 3 and keep the playbook from being completed, whatever the node jobs
// of the other fleet stages say.
func TestCheckOneTimePlaybooksNeverLosesNodesThatTheDryrunCouldNotFindOut(t *testing.T) {
	oneTimeProject(t)
	header := "Inventory: example (inventories/example.yml)\n\n" + removeOldRubyManager + "\n"
	older := stopOldMetricsAgent + "\n  done on all 2 servers\n"

	// The node job of app02 proposes a change and then fails. The fleet
	// stage after it selects its nodes from an output of the first, so the
	// dryrun cannot say which nodes it has.
	notSelected := "the dryrun did not inspect every node: fleet stage restart_workers ended with status not_checked: one_time_playbooks/" + removeOldRubyManager + ".yml:16:12 (fleet_stages[1].nodes): outputs.remove_old_ruby_manager.app02 is unavailable: remove_old_ruby_manager on app02 is failed"
	standIn := useNodesStandIn(t, map[string]map[string]nodeAnswer{removeOldRubyManager: {"app02": {Response: protocol.Response{Status: "failed", Error: "node task task_2 failed",
		Results: []protocol.Result{{Task: "task_1", Status: "succeeded", Change: "would_change"}, {Task: "task_2", Status: "failed", Change: "unchanged"}}}}}})
	standIn.after = func(r *controller.Run) {
		if r.Project.PlaybookName == removeOldRubyManager {
			r.Selections["restart_workers"] = &controller.Selection{Path: "restart_workers", Response: protocol.Response{Status: "not_checked", Error: strings.TrimPrefix(notSelected, "the dryrun did not inspect every node: fleet stage restart_workers ended with status not_checked: ")}}
		}
	}
	out, code, err := captureCommand(t, "check-one-time-playbooks")
	if want := header + "  done on 3 of 4 servers\n  still needed on 1 server: app02\n  " + notSelected + "\n" + older; code != 3 || err != nil || out != want {
		t.Fatalf("%d %v, got\n%s\nwant\n%s", code, err, out, want)
	}
	out, code, err = captureCommand(t, "check-one-time-playbooks", "--json")
	var check oneTimePlaybookCheck
	if code != 3 || err != nil || json.Unmarshal([]byte(out), &check) != nil || check.OneTimePlaybooks[0].NotInspectedReason != notSelected || len(check.OneTimePlaybooks[0].NotKnown) != 0 {
		t.Fatal(code, err, out)
	}
	_, code, err = captureCommand(t, "complete-one-time-playbook", removeOldRubyManager)
	if want := "one-time playbook " + removeOldRubyManager + " is not completed and stays in one_time_playbooks/: 1 node still needs the one-time playbook, and " + notSelected + "; run the one-time playbook with mmx run " + removeOldRubyManager + "; mmx results "; code != 3 || err == nil || !strings.HasPrefix(err.Error(), want) {
		t.Fatal(code, err)
	}

	// A for_each_node loop over the four nodes, one at a time: app01 is down,
	// the loop stops, and the iterations of the other nodes never start.
	neverStarted := "the dryrun did not inspect every node: fleet stage rollout[app02] ended with status stopped: not started: on_node_failure stop_fleet_stage after rollout failed on app01"
	standIn = useNodesStandIn(t, nil)
	standIn.after = func(r *controller.Run) {
		if r.Project.PlaybookName != removeOldRubyManager {
			return
		}
		r.Status = "failed"
		r.Entries = []controller.Entry{{FleetStage: "rollout[app01]/update", Node: "app01", Response: serverIsDown("app01").Response}}
		rollout := &controller.Selection{Path: "rollout", Loop: true, Resolved: true, Iterations: map[string]protocol.Response{"app01": {Status: "failed", Error: "rollout[app01]/update on app01 is failed: " + serverIsDownError("app01")}}}
		for _, node := range []string{"app01", "app02", "app03", "app04"} {
			rollout.Items = append(rollout.Items, value.Iteration{Key: node, Value: node})
			if node != "app01" {
				rollout.Iterations[node] = protocol.Response{Status: "stopped", Error: "not started: on_node_failure stop_fleet_stage after rollout failed on app01"}
			}
		}
		r.Selections = map[string]*controller.Selection{"rollout": rollout}
	}
	out, code, err = captureCommand(t, "check-one-time-playbooks")
	if want := header + "  not known on 1 server: app01 (" + serverIsDownReason("app01") + ")\n  " + neverStarted + "\n" + older; code != 3 || err != nil || out != want {
		t.Fatalf("%d %v, got\n%s\nwant\n%s", code, err, out, want)
	}
	if _, code, err = captureCommand(t, "complete-one-time-playbook", removeOldRubyManager); code != 3 || err == nil || !strings.Contains(err.Error(), "the state of 1 server is not known, and "+neverStarted) {
		t.Fatal(code, err)
	}
	if _, err := os.Stat("one_time_playbooks/completed"); !os.IsNotExist(err) {
		t.Fatal("a refused command wrote into completed/", err)
	}
}

func TestCheckOneTimePlaybooksWithoutAnOutstandingPlaybookSucceeds(t *testing.T) {
	root := oneTimeProject(t)
	standIn := useNodesStandIn(t, nil)
	// completed/ and files that start with a dot are no outstanding playbook.
	if err := os.MkdirAll("one_time_playbooks/completed", 0755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{removeOldRubyManager, stopOldMetricsAgent} {
		if err := os.Rename("one_time_playbooks/"+name+".yml", "one_time_playbooks/completed/"+name+".yml"); err != nil {
			t.Fatal(err)
		}
	}
	writeProjectFile(t, "one_time_playbooks/.gitkeep", "")
	out, code, err := captureCommand(t, "check-one-time-playbooks")
	if want := "Inventory: example (inventories/example.yml)\n\nNo outstanding one-time playbook: one_time_playbooks/ holds none outside completed/.\n"; code != 0 || err != nil || out != want {
		t.Fatal(code, err, out)
	}
	out, code, err = captureCommand(t, "check-one-time-playbooks", "--json")
	var check oneTimePlaybookCheck
	if code != 0 || err != nil || json.Unmarshal([]byte(out), &check) != nil || check.SchemaVersion != 1 || check.Inventory != "example" || !strings.Contains(out, `"one_time_playbooks": []`) {
		t.Fatal(code, err, out)
	}
	// A project that has no one_time_playbooks/ directory at all.
	if err := os.RemoveAll("one_time_playbooks"); err != nil {
		t.Fatal(err)
	}
	if _, code, err := captureCommand(t, "check-one-time-playbooks"); code != 0 || err != nil {
		t.Fatal(code, err)
	}
	if len(standIn.dryruns) != 0 {
		t.Fatal("a dryrun ran without a playbook")
	}
	if _, err := os.Stat(filepath.Join(root, "one_time_playbooks")); !os.IsNotExist(err) {
		t.Fatal("the check created the directory", err)
	}
}

// --inventory selects the inventory whose nodes are checked. A playbook that
// MMX cannot load with it has inspected nothing and counts as not known, and
// the other playbooks are still inspected.
func TestCheckOneTimePlaybooksWithASecondInventory(t *testing.T) {
	oneTimeProject(t)
	// The inventory staging has no group metrics, which the older playbook selects.
	writeProjectFile(t, "inventories/staging.yml", "nodes:\n  staging01: {address: staging01.example.net}\n")
	standIn := useNodesStandIn(t, nil)
	out, code, err := captureCommand(t, "check-one-time-playbooks", "--inventory", "staging")
	want := "Inventory: staging (inventories/staging.yml)\n\n" +
		removeOldRubyManager + "\n" +
		"  done on the 1 server\n" +
		stopOldMetricsAgent + "\n" +
		"  inspected nothing: MMX cannot load the playbook with inventory staging: fleet stage stop_old_metrics_agent: one_time_playbooks/" + stopOldMetricsAgent + ".yml:1:52 (fleet_stages[0].nodes): no node or inventory group named \"metrics\"; a one-time playbook that no node needs any more goes to one_time_playbooks/completed/\n"
	if code != 3 || err != nil || out != want {
		t.Fatalf("%d %v, got\n%s\nwant\n%s", code, err, out, want)
	}
	if len(standIn.dryruns) != 1 || standIn.dryruns[0].InventoryName != "staging" || len(standIn.dryruns[0].Entries) != 1 || standIn.dryruns[0].Entries[0].Node != "staging01" {
		t.Fatalf("%+v", standIn.dryruns)
	}
	out, code, err = captureCommand(t, "check-one-time-playbooks", "--inventory=staging", "--json")
	var check oneTimePlaybookCheck
	if code != 3 || err != nil || json.Unmarshal([]byte(out), &check) != nil || check.Inventory != "staging" || len(check.OneTimePlaybooks) != 2 {
		t.Fatal(code, err, out)
	}
	if notLoaded := check.OneTimePlaybooks[1]; notLoaded.Name != stopOldMetricsAgent || notLoaded.RunID != "" || len(notLoaded.Done)+len(notLoaded.StillNeeded)+len(notLoaded.NotKnown) != 0 ||
		!strings.HasPrefix(notLoaded.NotInspectedReason, "inspected nothing: MMX cannot load the playbook with inventory staging: ") || !strings.Contains(out, `"run_id": "",`) {
		t.Fatal(out)
	}
	// The default inventory still answers for its own nodes.
	if out, code, err = captureCommand(t, "check-one-time-playbooks", "--output", "summary"); code != 0 || err != nil || !strings.Contains(out, "Inventory: example (inventories/example.yml)\n\n"+removeOldRubyManager+"\n  done on all 4 servers\n") {
		t.Fatal(code, err, out)
	}
}

// The command's own dryrun code, without the stand-in: a fleet stage whose
// when skips every node contacts no node, and such a node has nothing to do.
// Every dryrun of one command reads the vault through one store. The when
// reads the secret, so it is decided only during the run and the fleet
// stage needs its secret; a when that is false before the run would leave
// the fleet stage out with no vault to unlock.
func TestCheckOneTimePlaybooksRunsOrdinaryDryrunsAndUnlocksTheVaultOnce(t *testing.T) {
	root := oneTimeProject(t)
	for _, name := range []string{removeOldRubyManager, stopOldMetricsAgent} {
		writeProjectFile(t, "one_time_playbooks/"+name+".yml", "fleet_stages:\n  - id: revoke_token\n    nodes: all\n    when: {equals: ['{{ .secrets.token }}', not-the-token]}\n    node_tasks:\n      - shell: {script: revoke, env: {TOKEN: '{{ .secrets.token }}'}}\n")
	}
	password := filepath.Join(t.TempDir(), "password")
	if err := os.WriteFile(password, []byte("check test passphrase\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := secrets.Write(filepath.Join(root, "secrets_vault.age"), "check test passphrase", []byte("token: one-time-secret-sentinel\n")); err != nil {
		t.Fatal(err)
	}
	// No passphrase comes from the environment of the test itself.
	for _, name := range []string{"MMX_VAULT_PASSWORD", "MMX_VAULT_PASSWORD_FILE"} {
		t.Setenv(name, "")
		os.Unsetenv(name)
	}
	out, code, err := captureCommand(t, "check-one-time-playbooks", "--json", "--vault-password-file", password)
	var check oneTimePlaybookCheck
	if code != 0 || err != nil || json.Unmarshal([]byte(out), &check) != nil || len(check.OneTimePlaybooks) != 2 || strings.Contains(out, "one-time-secret-sentinel") {
		t.Fatal(code, err, out)
	}
	for _, playbook := range check.OneTimePlaybooks {
		if len(playbook.Done) != 4 || len(playbook.StillNeeded)+len(playbook.NotKnown) != 0 || playbook.NotInspectedReason != "" {
			t.Fatalf("%+v", playbook)
		}
		saved, code, err := captureCommand(t, "results", playbook.RunID, "--json")
		var view struct {
			Status, Playbook string
			Dryrun           bool
			Entries          []struct{ Response struct{ Status string } }
		}
		if code != 0 || err != nil || json.Unmarshal([]byte(saved), &view) != nil || !view.Dryrun || view.Status != "succeeded" || view.Playbook != playbook.Name || len(view.Entries) != 4 || view.Entries[0].Response.Status != "skipped" {
			t.Fatal(code, err, saved)
		}
	}
	// A vault that cannot be unlocked is a command error, before any node work.
	if _, code, err := captureCommand(t, "check-one-time-playbooks", "--json", "--non-interactive"); code != 4 || err == nil || !strings.Contains(err.Error(), "vault passphrase required") {
		t.Fatal(code, err)
	}
	if _, code, err := captureCommand(t, "check-one-time-playbooks", "--vault-password-file", ""); code != 4 || err == nil || !strings.Contains(err.Error(), "--vault-password-file requires a nonempty path") {
		t.Fatal(code, err)
	}
	// Ctrl-C before the first dryrun: that dryrun starts no node job and is
	// saved as stopped, like an interrupted mmx dryrun, and the second
	// playbook gets no dryrun at all.
	saved := func() int {
		runs, err := filepath.Glob(filepath.Join(root, ".mmx/runs/*/run.json"))
		if err != nil {
			t.Fatal(err)
		}
		return len(runs)
	}
	before := saved()
	interrupted, cancel := context.WithCancel(context.Background())
	cancel()
	if code, err := run(interrupted, []string{"check-one-time-playbooks", "--json", "--vault-password-file", password}); code != 130 || err != nil {
		t.Fatal(code, err)
	}
	if saved() != before+1 {
		t.Fatal("after Ctrl-C the command made", saved()-before, "dryruns")
	}
	if out, code, err := captureCommand(t, "results", "--json"); code != 0 || err != nil || !strings.Contains(out, `"status": "stopped"`) || !strings.Contains(out, `"playbook": "`+removeOldRubyManager+`"`) {
		t.Fatal(code, err, out)
	}
	// Each playbook's dryrun gets the command's one vault store and its one
	// set of credentials, so nothing is unlocked or asked for twice.
	standIn := useNodesStandIn(t, nil)
	if _, code, err := captureCommand(t, "check-one-time-playbooks", "--vault-password-file", password); code != 0 || err != nil {
		t.Fatal(code, err)
	}
	if len(standIn.secrets) != 2 || standIn.secrets[0] == nil || standIn.secrets[0] != standIn.secrets[1] || standIn.secrets[0].PasswordFile != password ||
		standIn.credentials[0] == nil || standIn.credentials[0] != standIn.credentials[1] {
		t.Fatal("the dryruns of one command do not share one vault store and one set of credentials")
	}
}

// Ctrl-C stops the dryrun that runs, as in mmx dryrun, and no later playbook
// is inspected.
func TestCheckOneTimePlaybooksStopsAtCtrlC(t *testing.T) {
	oneTimeProject(t)
	standIn := useNodesStandIn(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	standIn.after = func(*controller.Run) { cancel() }
	if code, err := run(ctx, []string{"check-one-time-playbooks", "--json"}); code != 130 || err != nil {
		t.Fatal(code, err)
	}
	if got := standIn.playbooks(); !slices.Equal(got, []string{removeOldRubyManager}) {
		t.Fatal("inspected after Ctrl-C:", got)
	}
}

func TestCheckOneTimePlaybooksRefusesWhatItCannotDo(t *testing.T) {
	oneTimeProject(t)
	standIn := useNodesStandIn(t, nil)
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{removeOldRubyManager}, "check-one-time-playbooks takes no playbook name: it checks every one-time playbook outside one_time_playbooks/completed/"},
		{[]string{"--output", "wide"}, "--output must be auto, nodes, or summary"},
		{[]string{"--inventory", "inventories/example.yml"}, `--inventory must be a name, not a path: "inventories/example.yml"`},
		{[]string{"--inventory", "production"}, "inventory production"},
		// The check inspects every node a playbook selects, with its defaults.
		{[]string{"--limit", "app01"}, "flag provided but not defined: -limit"},
		{[]string{"--var", "release=1"}, "flag provided but not defined: -var"},
		{[]string{"--only-fleet-stages", "site"}, "flag provided but not defined: -only-fleet-stages"},
		{[]string{"--diff"}, "flag provided but not defined: -diff"},
	} {
		if _, code, err := captureCommand(t, append([]string{"check-one-time-playbooks"}, tc.args...)...); code != 4 || err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%v: want %q, got %d %v", tc.args, tc.want, code, err)
		}
	}
	// Anything else in one_time_playbooks/ is an error, as for mmx validate.
	writeProjectFile(t, "one_time_playbooks/remove_old_ruby_manager.yml", "fleet_stages: []\n")
	if _, code, err := captureCommand(t, "check-one-time-playbooks"); code != 4 || err == nil || !strings.HasPrefix(err.Error(), "one_time_playbooks/remove_old_ruby_manager.yml: one_time_playbooks/ accepts only one-time playbook files and the completed/ directory") {
		t.Fatal(code, err)
	}
	if len(standIn.dryruns) != 0 {
		t.Fatal("a refused command inspected nodes")
	}
	t.Chdir(t.TempDir())
	if _, code, err := captureCommand(t, "check-one-time-playbooks"); code != 4 || err == nil || !strings.Contains(err.Error(), "no mmx_project_settings.yml found") {
		t.Fatal(code, err)
	}
}

func TestHelpNamesTheOneTimePlaybookCommands(t *testing.T) {
	out, code, err := captureCommand(t, "help")
	if code != 0 || err != nil {
		t.Fatal(code, err)
	}
	for _, want := range []string{
		" | new-one-time-playbook WHAT_IT_DOES | check-one-time-playbooks | complete-one-time-playbook NAME | ",
		"check-one-time-playbooks: a node still needs an outstanding one-time playbook.",
		"complete-one-time-playbook: refused, because a node still needs the playbook.",
		"check-one-time-playbooks and complete-one-time-playbook: a node could not be inspected, or the check inspected no nodes.",
		"Also a command that names a completed one-time playbook.",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("missing %q in\n%s", want, out)
		}
	}
}
