package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"mmx/internal/config"
	"mmx/internal/controller"
	"mmx/internal/durable"
	"mmx/internal/protocol"
	"mmx/internal/value"
)

func TestProgressExplainsStopAndSummaryCountsNodesAcrossFleetStages(t *testing.T) {
	r := &controller.Run{ID: "example", Entries: []controller.Entry{
		{Node: "a", FleetStage: "first", Response: protocol.Response{Status: "succeeded"}},
		{Node: "a", FleetStage: "second", Response: protocol.Response{Status: "pending"}},
		{Node: "b", FleetStage: "first", Response: protocol.Response{Status: "pending"}},
	}}
	if summary(r) != "1 waiting · 1 not started" {
		t.Fatal(summary(r))
	}
	var out bytes.Buffer
	report := liveProgress(&out, false)
	report(r)
	r.StopRequested = true
	report(r)
	if summary(r) != "1 stopped partway through · 1 not started" {
		t.Fatal(summary(r))
	}
	for _, text := range []string{"Stop requested", "No new work will start", "current node task or node task block", "Press Ctrl-C again"} {
		if !strings.Contains(out.String(), text) {
			t.Fatal("missing instruction", text, out.String())
		}
	}
	out.Reset()
	report(r)
	if strings.Contains(out.String(), "Stop requested") {
		t.Fatal("repeated stop instructions")
	}
}

// A run's live progress and its final report come from one command: the
// output view is named once, and the live counts are labeled as progress,
// so a recorded run does not read "1 not started" as part of the outcome.
func TestARunNamesItsOutputViewOnceAndLabelsItsProgress(t *testing.T) {
	r := &controller.Run{ID: "20261028T000000.000000000-0123456789abcdef", Status: "running", Entries: []controller.Entry{{Node: "a", FleetStage: "site", Response: protocol.Response{Status: "pending"}}}}
	r.Project.PlaybookName = "site"
	p := &presentation{mode: "auto"}
	var progress bytes.Buffer
	liveProgress(&progress, false, p)(r)
	r.Status, r.Entries[0].NodeJobAttempt, r.Entries[0].Response.Status = "succeeded", 1, "succeeded"
	stdout, err := os.CreateTemp(t.TempDir(), "stdout")
	if err != nil {
		t.Fatal(err)
	}
	saved := os.Stdout
	os.Stdout = stdout
	_, err = output(r, false, false, p, nil)
	os.Stdout = saved
	stdout.Close()
	report, _ := os.ReadFile(stdout.Name())
	if err != nil {
		t.Fatal(err)
	}
	recorded := progress.String() + string(report)
	if strings.Count(recorded, "Output: nodes") != 1 || !strings.Contains(progress.String(), "Output: nodes (change with --output=nodes or --output=summary)\nProgress: 1 not started\n") ||
		!strings.Contains(string(report), "Run 01234567: succeeded\n1 finished\n") {
		t.Fatalf("progress:\n%s\nreport:\n%s", progress.String(), report)
	}
}

// A run's report starts with one plain line for each server, before the
// detail lines. A dryrun says in that line why work is not checked yet; no
// separate note repeats it, and a run says nothing about it.
func TestARunReportStartsWithAPlainSummaryForEachServer(t *testing.T) {
	report := func(dryrun bool, results ...protocol.Result) string {
		t.Helper()
		r := &controller.Run{ID: "20261028T000000.000000000-0123456789abcdef", Status: "succeeded", Dryrun: dryrun, Entries: []controller.Entry{
			{Node: "web01", FleetStage: "set_up_nginx", NodeJobAttempt: 1, Response: protocol.Response{Status: "succeeded", Results: results}},
			{Node: "web02", FleetStage: "set_up_nginx", NodeJobAttempt: 1, Response: protocol.Response{Status: "succeeded", Results: results}},
		}}
		r.Project.PlaybookName = "set_up_web_servers"
		stdout, err := os.CreateTemp(t.TempDir(), "stdout")
		if err != nil {
			t.Fatal(err)
		}
		saved := os.Stdout
		os.Stdout = stdout
		_, err = output(r, false, false, &presentation{mode: "auto"}, nil)
		os.Stdout = saved
		stdout.Close()
		text, _ := os.ReadFile(stdout.Name())
		if err != nil {
			t.Fatal(err)
		}
		return string(text)
	}
	notChecked := protocol.Result{Task: "check_nginx_config", Status: "not_checked", Change: "unknown", Reason: "an earlier node task, install_nginx, would change the node before the run reaches the check; the check failed against the node's current state: shell command exited with code 127"}
	checked := protocol.Result{Task: "write_home_page", Status: "succeeded", Change: "would_change"}
	const want = "Output: nodes (change with --output=nodes or --output=summary)\n" +
		"web01: 1 node task would change (write_home_page). 2 not checked yet: they usually check what earlier node tasks install or create; mmx run checks them for real. 0 failed.\n" +
		"web02: 1 node task would change (write_home_page). 2 not checked yet: they usually check what earlier node tasks install or create; mmx run checks them for real. 0 failed.\n" +
		"set_up_nginx / web01: succeeded\n"
	if text := report(true, checked, notChecked, notChecked); !strings.HasPrefix(text, want) || strings.Count(text, "earlier node tasks install") != 2 {
		t.Fatalf("the dryrun report does not start with one summary line for each server:\n%s", text)
	}
	checked.Change = "changed"
	if text := report(false, checked); !strings.Contains(text, "\nweb01: 1 node task changed (write_home_page). 0 failed.\nweb02: ") || strings.Contains(text, "not checked") {
		t.Fatalf("the run report:\n%s", text)
	}
}

func TestCanceledConnectionDoesNotImplyRemoteWork(t *testing.T) {
	r := &controller.Run{StopRequested: true, Entries: []controller.Entry{{Node: "web", FleetStage: "setup", Response: protocol.Response{Status: "running"}}}}
	var out bytes.Buffer
	activeDetails(&out, r)
	if !strings.Contains(out.String(), "canceling the connection; no node job accepted") {
		t.Fatal(out.String())
	}
	r.Entries[0].Response.Status = "stopped"
	if summary(r) != "1 not started" {
		t.Fatal(summary(r))
	}
}

func TestFileChangesShowSummaryDiffAndLoopLocation(t *testing.T) {
	r := protocol.Result{Task: "files[config]", Change: "would_change", FileChange: &protocol.FileChange{
		Path: "/etc/app", Action: "update", Before: &protocol.FileState{Kind: "file", Size: 4, Mode: "0644"}, After: &protocol.FileState{Kind: "file", Size: 4, Mode: "0640"}, Diff: "--- before\n+++ after\n-old\n+new\n",
	}}
	for _, diff := range []bool{false, true} {
		var out bytes.Buffer
		fileChanges(&out, r, "setup / node", diff)
		for _, want := range []string{"files[config]", "/etc/app", "would update", "0644", "0640"} {
			if !strings.Contains(out.String(), want) {
				t.Fatal(out.String())
			}
		}
		if strings.Contains(out.String(), "+new") != diff {
			t.Fatal(out.String())
		}
	}
	r.Secret = true
	var out bytes.Buffer
	fileChanges(&out, r.Safe(), "setup / node", true)
	if strings.Contains(out.String(), "+new") || !strings.Contains(out.String(), "suppressed") {
		t.Fatal(out.String())
	}
}

// A file from node paths shows SHA-256 values, a kept original shows where it
// goes, and a link that replaces a file says so.
func TestFileChangesShowDigestsCopiesAndReplacedFiles(t *testing.T) {
	r := protocol.Result{Task: "certificate", Change: "would_change", FileChange: &protocol.FileChange{
		Path: "/etc/haproxy/site.pem", Action: "update", OriginalFileCopiedTo: "/etc/haproxy/site.pem.original",
		Before: &protocol.FileState{Kind: "file", Size: 4, Mode: "0640", SHA256: "aaaa"}, After: &protocol.FileState{Kind: "file", Size: 9, Mode: "0640", SHA256: "bbbb"},
	}}
	var out bytes.Buffer
	fileChanges(&out, r, "setup / node", true)
	for _, want := range []string{"sha256 aaaa", "sha256 bbbb", `would copy the original file to "/etc/haproxy/site.pem.original"`} {
		if !strings.Contains(out.String(), want) {
			t.Fatal(out.String())
		}
	}
	r = protocol.Result{Task: "current", Change: "changed", FileChange: &protocol.FileChange{
		Path: "/srv/app/current", Action: "replace", Before: &protocol.FileState{Kind: "file", Size: 4, Mode: "0644"}, After: &protocol.FileState{Kind: "symlink", Target: "releases/2"},
	}}
	out.Reset()
	fileChanges(&out, r, "setup / node", false)
	if !strings.Contains(out.String(), `replaced "/srv/app/current"`) || !strings.Contains(out.String(), `-> link to "releases/2"`) {
		t.Fatal(out.String())
	}
}

func TestDirectorySummaryAndBoundedExamples(t *testing.T) {
	r := protocol.Result{Task: "permissions", Status: "succeeded", Change: "would_change", DirectoryChange: &protocol.DirectoryChange{
		Path: "/srv/app", Directories: 2, Files: 30, Changes: 32, SkippedSymlinks: 1,
		Examples: []protocol.FileChange{{Path: "/srv/app", Action: "update", Before: &protocol.FileState{Kind: "directory", Mode: "0755"}, After: &protocol.FileState{Kind: "directory", Mode: "0750"}}},
	}}
	var out bytes.Buffer
	directoryChanges(&out, r, "configure / app", true)
	for _, text := range []string{"checked 2 directories and 30 files", "32 entries would change", "skipped 1 symlinks", "mode 0755", "mode 0750", "31 further entries"} {
		if !strings.Contains(out.String(), text) {
			t.Fatal(text, out.String())
		}
	}
	r.Change = "changed"
	r.Status = "failed"
	r.DirectoryChange.Applied = 3
	out.Reset()
	directoryChanges(&out, r, "configure / app", false)
	if !strings.Contains(out.String(), "3 metadata updates before failure") {
		t.Fatal(out.String())
	}
	r.Change = "would_change"
	out.Reset()
	directoryChanges(&out, r, "configure / app", false)
	if !strings.Contains(out.String(), "32 entries would change before failure; inspection may be incomplete") {
		t.Fatal(out.String())
	}
	r.Status = "succeeded"
	r.DirectoryChange.CreatedParentDirectories = []string{"/srv", "/srv/app"}
	out.Reset()
	directoryChanges(&out, r, "configure / app", false)
	if !strings.Contains(out.String(), "    would create the missing parent directories /srv, /srv/app, owner root, permissions_mode 0755\n") {
		t.Fatal(out.String())
	}
	r.Change = "changed"
	out.Reset()
	directoryChanges(&out, r, "configure / app", false)
	if !strings.Contains(out.String(), "    created the missing parent directories /srv, /srv/app,") {
		t.Fatal(out.String())
	}
	r.DirectoryChange.CreatedParentDirectories = nil
	r.Change, r.Status = "changed", "succeeded"
	r.Tries = []protocol.Result{{DirectoryChange: &protocol.DirectoryChange{Applied: 3}}, {DirectoryChange: &protocol.DirectoryChange{Applied: 29}}}
	r.DirectoryChange.Applied = 29
	out.Reset()
	directoryChanges(&out, r, "configure / app", false)
	if !strings.Contains(out.String(), "32 metadata updates\n") {
		t.Fatal(out.String())
	}
}

func TestActiveDetailsShowsAggregateTransferProgress(t *testing.T) {
	var out strings.Builder
	r := &controller.Run{Entries: []controller.Entry{{FleetStage: "copy", Node: "web", JobID: "job", Response: protocol.Response{Status: "running"}, Transfer: &controller.TransferProgress{Bytes: 18 * 1024 * 1024, Total: 64 * 1024 * 1024, Completed: 1, Files: 2}}}}
	activeDetails(&out, r)
	if !strings.Contains(out.String(), "transferring files — 18.0 / 64.0 MiB (1/2 files)") {
		t.Fatal(out.String())
	}
}

// Failed, prerequisite-blocked, and unstarted servers are counted apart; a
// finished run never calls a server waiting.
func TestSummarySeparatesFailedBlockedAndUnstarted(t *testing.T) {
	r := &controller.Run{ID: "x", Status: "failed", Entries: []controller.Entry{
		{FleetStage: "migrate", Node: "db", NodeJobAttempt: 1, Response: protocol.Response{Status: "failed"}},
	}}
	for i := 0; i < 3; i++ {
		r.Entries = append(r.Entries, controller.Entry{FleetStage: "app", Node: fmt.Sprintf("app%d", i), Response: protocol.Response{Status: "blocked", Error: "requires all_nodes fleet stage migrate to succeed (db is failed)"}})
	}
	if s := summary(r); s != "1 failed · 3 blocked" {
		t.Fatal(s)
	}
	cause := "not started: on_node_failure stop_run after setup failed on a"
	r = &controller.Run{ID: "x", Status: "failed", Entries: []controller.Entry{
		{FleetStage: "setup", Node: "a", NodeJobAttempt: 1, Response: protocol.Response{Status: "failed"}},
		{FleetStage: "setup", Node: "b", NodeJobAttempt: 1, Response: protocol.Response{Status: "succeeded"}},
		{FleetStage: "independent", Node: "a", Response: protocol.Response{Status: "stopped", Error: cause}},
		{FleetStage: "independent", Node: "b", Response: protocol.Response{Status: "stopped", Error: cause}},
	}}
	if s := summary(r); s != "1 failed · 1 stopped partway through" {
		t.Fatal(s)
	}
}

// A rollout loop ended by stop_run or a stop, as the scheduler records it
// (controller TestStoppedLoopRecordsUnstartedWork): the drained app is not
// finished, and the iteration that never started is listed with its cause.
func TestSummaryShowsWorkAStoppedLoopKeptFromStarting(t *testing.T) {
	for _, variant := range []string{"stop_run", "stop"} {
		t.Run(variant, func(t *testing.T) {
			cause, status, update := "not started: stop requested", "stopped", protocol.Response{Status: "stopped", Error: "not started: stop requested"}
			if variant == "stop_run" {
				cause, status, update = "not started: on_node_failure stop_run after roll[a]/update failed on a", "failed", protocol.Response{Status: "failed", Error: "health check failed"}
			}
			updateAttempt := 0
			if variant == "stop_run" {
				updateAttempt = 1
			}
			r := &controller.Run{ID: "x", Status: status, StopRequested: variant == "stop", Entries: []controller.Entry{
				{FleetStage: "migrate", Node: "a", NodeJobAttempt: 1, Response: protocol.Response{Status: "succeeded"}},
				{FleetStage: "report", Node: "c", Response: protocol.Response{Status: "stopped", Error: cause}},
				{FleetStage: "roll[a]/drain", Node: "c", NodeJobAttempt: 1, Response: protocol.Response{Status: "succeeded"}},
				{FleetStage: "roll[a]/update", Node: "a", NodeJobAttempt: updateAttempt, Response: update},
				{FleetStage: "roll[a]/rejoin", Node: "c", Response: protocol.Response{Status: "stopped", Error: cause}},
			}, Selections: map[string]*controller.Selection{"roll": {Path: "roll", Loop: true, Resolved: true,
				Items:      []value.Iteration{{Key: "a"}, {Key: "b"}},
				Iterations: map[string]protocol.Response{"a": {Status: status, Error: "roll[a]/update on a is " + update.Status}, "b": {Status: "stopped", Error: cause}},
			}}}
			want := "2 stopped partway through"
			if variant == "stop_run" {
				want = "1 failed · 1 stopped partway through"
			}
			if s := summary(r); s != want {
				t.Fatalf("got %q, want %q", s, want)
			}
			var out bytes.Buffer
			selectionOutput(&out, r, false)
			if !strings.Contains(out.String(), "  roll[b]: stopped — "+cause+"\n") {
				t.Fatal(out.String())
			}
		})
	}
}

// The run record that mmx results --json prints names each field of the
// fleet stage selections and the node job attempts by what it holds.
func TestResultsJSONNamesTheRunRecordFields(t *testing.T) {
	root := initializedProject(t)
	state := filepath.Join(root, ".mmx")
	r, err := controller.Controller{State: state}.New(context.Background(), config.Project{HistoryRetention: "forever"}, config.Inventory{}, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	failed := protocol.Response{Status: "failed", Error: "the first try failed"}
	r.Status, r.FleetStagesAwaitingNodeSelection = "failed", true
	r.Selections = map[string]*controller.Selection{"setup": {Path: "setup", PlaybookPosition: []int{0}, Resolved: true, Nodes: []string{"web01"}}}
	r.Entries = []controller.Entry{{FleetStage: "setup", Node: "web01", JobID: "second", NodeJobAttempt: 2, Response: failed, PreviousNodeJobAttempts: []controller.Attempt{{JobID: "first", Response: failed}}}}
	if err := durable.JSON(filepath.Join(state, "runs", r.ID, "run.json"), r); err != nil {
		t.Fatal(err)
	}
	out, code, err := captureCommand(t, "results", r.ID, "--json")
	if code != 0 || err != nil {
		t.Fatal(code, err, out)
	}
	var view struct {
		FleetStagesAwaitingNodeSelection *bool `json:"fleet_stages_awaiting_node_selection"`
		Selections                       map[string]map[string]any
		Entries                          []map[string]any
	}
	if err := json.Unmarshal([]byte(out), &view); err != nil {
		t.Fatal(err, out)
	}
	if view.FleetStagesAwaitingNodeSelection == nil || !*view.FleetStagesAwaitingNodeSelection {
		t.Fatal("no fleet_stages_awaiting_node_selection: true", out)
	}
	selection := view.Selections["setup"]
	if !reflect.DeepEqual(selection["playbook_position"], []any{0.0}) || selection["resolved"] != true {
		t.Fatal("selection", selection)
	}
	entry := view.Entries[0]
	previous, _ := entry["previous_node_job_attempts"].([]any)
	if entry["node_job_attempt"] != 2.0 || len(previous) != 1 || previous[0].(map[string]any)["job_id"] != "first" {
		t.Fatal("entry", entry)
	}
	for _, old := range []string{`"pending"`, `"order"`, `"start"`, `"expanded"`, `"attempt"`, `"previous_attempts"`, `"loop_position"`} {
		if strings.Contains(out, old+":") {
			t.Fatal("the old field name", old, out)
		}
	}
}

// mmx results --json prints the entries in playbook order. A fleet stage whose
// nodes resolved during the run has its entry saved after those of later fleet
// stages, so the saved order differs. A reader finds a selection's entries by
// fleet_stage and node; no field of a selection indexes the printed entries.
func TestResultsJSONSelectionsFindTheirEntriesByFleetStageAndNode(t *testing.T) {
	root := initializedProject(t)
	state := filepath.Join(root, ".mmx")
	r, err := controller.Controller{State: state}.New(context.Background(), config.Project{HistoryRetention: "forever"}, config.Inventory{}, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	r.Status = "failed"
	// pick_backup_node selected its node during the run, from an output of an
	// earlier fleet stage; report's node was known when the run started.
	r.Selections = map[string]*controller.Selection{
		"pick_backup_node": {Path: "pick_backup_node", PlaybookPosition: []int{0}, Resolved: true, Nodes: []string{"web02"}},
		"report":           {Path: "report", PlaybookPosition: []int{1}, Resolved: true, Nodes: []string{"web01"}},
	}
	r.Entries = []controller.Entry{
		{FleetStage: "report", Node: "web01", Response: protocol.Response{Status: "succeeded"}},
		{FleetStage: "pick_backup_node", Node: "web02", Response: protocol.Response{Status: "failed", Error: "backup failed"}},
	}
	if err := durable.JSON(filepath.Join(state, "runs", r.ID, "run.json"), r); err != nil {
		t.Fatal(err)
	}
	out, code, err := captureCommand(t, "results", r.ID, "--json")
	if code != 0 || err != nil {
		t.Fatal(code, err, out)
	}
	var view struct {
		Selections map[string]map[string]any
		Entries    []struct {
			FleetStage string            `json:"fleet_stage"`
			Node       string            `json:"node"`
			Response   protocol.Response `json:"response"`
		}
	}
	if err := json.Unmarshal([]byte(out), &view); err != nil {
		t.Fatal(err, out)
	}
	if len(view.Entries) != 2 || view.Entries[0].FleetStage != "pick_backup_node" || view.Entries[1].FleetStage != "report" {
		t.Fatal("entries are not in playbook order", out)
	}
	for path, selection := range view.Selections {
		if index, ok := selection["first_entry_index"].(float64); ok {
			at := view.Entries[int(index)]
			t.Errorf("selection %s indexes the printed entries: first_entry_index %v is the entry of %s on %s", path, index, at.FleetStage, at.Node)
		}
		for _, node := range selection["nodes"].([]any) {
			found := 0
			for _, entry := range view.Entries {
				if entry.FleetStage == path && entry.Node == node {
					found++
				}
			}
			if found != 1 {
				t.Errorf("selection %s has %d entries on %s", path, found, node)
			}
		}
	}
	for _, entry := range view.Entries {
		if entry.FleetStage == "pick_backup_node" && entry.Response.Error != "backup failed" {
			t.Fatal("pick_backup_node lost its own outcome", entry)
		}
	}
}
