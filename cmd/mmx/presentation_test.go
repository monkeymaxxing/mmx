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

	"mmx/internal/assets"
	"mmx/internal/controller"
	"mmx/internal/differences"
	"mmx/internal/protocol"
)

func TestAutoOutputBoundaryAndStableChoice(t *testing.T) {
	for _, n := range []int{10, 11} {
		r := &controller.Run{}
		for i := 0; i < n; i++ {
			r.Entries = append(r.Entries, controller.Entry{Node: fmt.Sprint(i)})
		}
		p := presentation{mode: "auto"}
		p.choose(r)
		want := "nodes"
		if n > 10 {
			want = "summary"
		}
		if p.mode != want {
			t.Fatal(n, p.mode)
		}
		r.Entries = nil
		p.choose(r)
		if p.mode != want {
			t.Fatal("display changed mid-run")
		}
		for _, override := range []string{"nodes", "summary"} {
			p := presentation{mode: override}
			p.choose(r)
			if p.mode != override {
				t.Fatal(p)
			}
		}
	}
}

// An accepted change needs no restore, so the output must not claim that a
// restore has not started or name a restore service that no longer exists.
func TestHumanPrintsNoRestoreLineForAnAcceptedChange(t *testing.T) {
	restore := protocol.OwedRestore{OwedRestoreID: "0123456789abcdef", DestinationFile: "/etc/mmx/example-network.conf",
		RestoreServiceUnit: "mmx-owed-restore-0123456789abcdef.service", RestoreStatus: "not_needed"}
	response := protocol.Response{Status: "succeeded", OwedRestores: []protocol.OwedRestore{restore}}
	r := &controller.Run{Entries: []controller.Entry{{FleetStage: "network", Node: "web01", Response: response}}}
	for _, mode := range []string{"nodes", "summary"} {
		var out bytes.Buffer
		if err := (&presentation{mode: mode}).human(&out, r, true); err != nil {
			t.Fatal(err)
		}
		for _, banned := range []string{"restore", "0123456789abcdef"} {
			if strings.Contains(out.String(), banned) {
				t.Fatalf("%s: accepted change printed %q:\n%s", mode, banned, out.String())
			}
		}
	}
}

func TestHumanShowsCurrentRestoreAndOriginalTaskFailure(t *testing.T) {
	for _, fixture := range []struct{ scenario, outcome string }{
		{"succeeded", "/etc/mmx/example-network.conf was restored to its previous state"},
		{"running", "the restore of /etc/mmx/example-network.conf is still running"},
		{"not_started", "the restore of /etc/mmx/example-network.conf has not started yet"},
		{"required", "the restore of /etc/mmx/example-network.conf has not started yet"},
		{"failed", "the restore of /etc/mmx/example-network.conf failed"},
		{"repaired", "/etc/mmx/example-network.conf was restored to its previous state"},
	} {
		scenario := fixture.scenario
		t.Run(scenario, func(t *testing.T) {
			restore := protocol.OwedRestore{OwedRestoreID: "0123456789abcdef", DestinationFile: "/etc/mmx/example-network.conf",
				RestoreServiceUnit: "mmx-owed-restore-0123456789abcdef.service", RestoreStatus: scenario, OriginalFailureReason: "reachability confirmation expired"}
			if scenario == "succeeded" || scenario == "repaired" {
				restore.RestoreStatus, restore.PreviousFileRestored, restore.PreviousConfigActivated, restore.ObsoleteVirtualDevicesRemoved = "succeeded", true, true, true
			}
			if scenario == "failed" {
				restore.PreviousFileRestored, restore.ObsoleteVirtualDevicesRemoved = true, true
				restore.RestoreFailureReason = "previous configuration activation failed"
			}
			response := protocol.Response{Status: "failed", Error: "node job worker interrupted", OwedRestores: []protocol.OwedRestore{restore}}
			if scenario == "repaired" {
				old := restore
				old.RestoreStatus, old.PreviousConfigActivated = "failed", false
				old.RestoreFailureReason = "previous configuration activation failed"
				response.Results = []protocol.Result{{Task: "network file", Status: "failed", Change: "changed", Reason: "reachability confirmation expired; previous configuration activation failed", OwedRestore: &old}}
			}
			r := &controller.Run{Entries: []controller.Entry{{FleetStage: "network", Node: "web01", Response: response}}}
			for _, mode := range []string{"nodes", "summary"} {
				for _, details := range []bool{false, true} {
					var out bytes.Buffer
					if err := (&presentation{mode: mode}).human(&out, r, details); err != nil {
						t.Fatal(err)
					}
					node := "web01"
					if mode == "summary" {
						node = "1 node [web01]"
					}
					location := fmt.Sprintf("  network / %s / file change 0123456789abcdef: ", node)
					want := location + fixture.outcome + "\n" +
						"    why the change was not kept: reachability confirmation expired\n"
					if scenario == "failed" {
						want += "    why the restore failed: previous configuration activation failed\n" +
							"    previous file put back: yes; previous configuration activated: no; virtual devices of the new file removed: yes\n" +
							"    after you corrected the cause, run on the node: systemctl start mmx-owed-restore-0123456789abcdef.service\n"
					} else if scenario != "succeeded" && scenario != "repaired" {
						want += "    restore service on the node: mmx-owed-restore-0123456789abcdef.service\n"
					}
					start := strings.Index(out.String(), location)
					if start < 0 || out.String()[start:] != want || strings.Count(out.String(), location) != 1 || !strings.Contains(out.String(), "network / "+node+": failed\n") {
						t.Fatalf("%s (details %v):\n%s\nwant current outcome:\n%s", mode, details, out.String(), want)
					}
					if scenario == "repaired" && !strings.Contains(out.String(), "network file: failed, changed — reachability confirmation expired; previous configuration activation failed") {
						t.Fatal("repair hid the original task failure", out.String())
					}
					if mode == "nodes" && !details {
						t.Logf("rendered %s outcome:\n%s", scenario, out.String()[start:])
					}
				}
			}
			r.Entries[0].Response.OwedRestores[0].OriginalFailureReason = ""
			var withoutReason bytes.Buffer
			if err := (&presentation{mode: "nodes"}).human(&withoutReason, r, false); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(withoutReason.String(), "why the change was not kept:") {
				t.Fatal("printed a reason the node did not supply", withoutReason.String())
			}
		})
	}
}

func TestHumanRestoreGroupingKeepsEachNodeAndFileChange(t *testing.T) {
	r := &controller.Run{}
	for _, fixture := range []struct{ node, change, status string }{
		{"web01", "0123456789abcdef", "running"},
		{"web02", "0123456789abcdef", "failed"},
		{"web03", "fedcba9876543210", "running"},
	} {
		r.Entries = append(r.Entries, controller.Entry{FleetStage: "network", Node: fixture.node, Response: protocol.Response{Status: "failed", OwedRestores: []protocol.OwedRestore{{OwedRestoreID: fixture.change, DestinationFile: "/etc/mmx/example-network.conf", RestoreStatus: fixture.status}}}})
	}
	var out bytes.Buffer
	if err := (&presentation{mode: "summary"}).human(&out, r, false); err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{
		"network / 1 node [web01] / file change 0123456789abcdef: the restore of /etc/mmx/example-network.conf is still running",
		"network / 1 node [web02] / file change 0123456789abcdef: the restore of /etc/mmx/example-network.conf failed",
		"network / 1 node [web03] / file change fedcba9876543210: the restore of /etc/mmx/example-network.conf is still running",
	} {
		if !strings.Contains(out.String(), text) {
			t.Fatal("grouping lost current restore membership", text, out.String())
		}
	}
}

func TestHumanRestoreFromTaskResultsIncludingNestedTasks(t *testing.T) {
	first := protocol.OwedRestore{OwedRestoreID: "0123456789abcdef", DestinationFile: "/etc/mmx/first.conf", RestoreStatus: "succeeded"}
	second := protocol.OwedRestore{OwedRestoreID: "fedcba9876543210", DestinationFile: "/etc/mmx/second.conf", RestoreStatus: "failed"}
	response := protocol.Response{Status: "failed", Results: []protocol.Result{{Task: "network files", Items: []protocol.Result{{OwedRestore: &first}}, Tries: []protocol.Result{{OwedRestore: &second}}}}}
	var out bytes.Buffer
	r := &controller.Run{Entries: []controller.Entry{{FleetStage: "network", Node: "web01", Response: response}}}
	if err := (&presentation{mode: "nodes"}).human(&out, r, false); err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{
		"file change 0123456789abcdef: /etc/mmx/first.conf was restored to its previous state",
		"file change fedcba9876543210: the restore of /etc/mmx/second.conf failed",
	} {
		if !strings.Contains(out.String(), text) {
			t.Fatal("nested restore outcome missing", out.String())
		}
	}
}

func TestHumanCurrentRestoreSuppressesSecretsFromResponseAndTask(t *testing.T) {
	for _, marking := range []string{"response", "task"} {
		t.Run(marking, func(t *testing.T) {
			restore := protocol.OwedRestore{OwedRestoreID: "0123456789abcdef", DestinationFile: "/etc/mmx/example-network.conf", RestoreStatus: "failed", OriginalFailureReason: "original-secret", RestoreFailureReason: "restore-secret"}
			response := protocol.Response{Status: "failed", OwedRestores: []protocol.OwedRestore{restore}}
			if marking == "response" {
				response.OwedRestores[0].Secret = true
			} else {
				response.Results = []protocol.Result{{Task: "network", Secret: true, Reason: "task-secret", OwedRestore: &restore}}
			}
			for _, mode := range []string{"nodes", "summary"} {
				var out bytes.Buffer
				r := &controller.Run{Entries: []controller.Entry{{FleetStage: "network", Node: "web01", Response: response}}}
				if err := (&presentation{mode: mode}).human(&out, r, true); err != nil {
					t.Fatal(err)
				}
				for _, secret := range []string{"original-secret", "restore-secret", "task-secret"} {
					if strings.Contains(out.String(), secret) {
						t.Fatal("restore output leaked a secret", out.String())
					}
				}
				for _, text := range []string{"file change 0123456789abcdef: the restore of /etc/mmx/example-network.conf failed", "why the change was not kept: details suppressed for a task using secrets", "why the restore failed: details suppressed for a task using secrets"} {
					if !strings.Contains(out.String(), text) {
						t.Fatal("secret suppression hid restore identity or outcome", text, out.String())
					}
				}
			}
		})
	}
}
func TestGroupedOutcomesPreserveMembershipDifferencesAndContext(t *testing.T) {
	r := &controller.Run{}
	for i := 0; i < 12; i++ {
		result := protocol.Result{Task: "role/files[config]", Status: "failed", Change: "unchanged", Reason: "validation failed", FileChange: &protocol.FileChange{Diff: "-old\n+same\n"}}
		if i == 10 {
			result.FileChange.Diff = "-old\n+distinct\n"
		}
		if i == 11 {
			result.Task = "role/files[other]"
		}
		r.Entries = append(r.Entries, controller.Entry{FleetStage: "configure[app]", Node: fmt.Sprintf("node%02d", i), Response: protocol.Response{Results: []protocol.Result{result}}})
	}
	groups := outcomeGroups(r, true)
	if len(groups) != 3 || len(groups[0].nodes) != 10 || groups[0].nodes[9] != "node09" {
		t.Fatal(groups)
	}
	if len(outcomeGroups(r, false)) != 12 {
		t.Fatal("node detail lost")
	}
	var out bytes.Buffer
	p := presentation{mode: "summary", diff: "preview"}
	if err := p.human(&out, r, false); err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"10 nodes [node00", "node09]", "+distinct", "role/files[other]"} {
		if !strings.Contains(out.String(), text) {
			t.Fatal(text, out.String())
		}
	}
}

func TestResultBudgetOmissionsAreVisible(t *testing.T) {
	result := protocol.Result{Task: "wait", Status: "succeeded", Change: "unchanged", Omitted: "output omitted: budget exhausted"}
	r := &controller.Run{Entries: []controller.Entry{{FleetStage: "poll", Node: "web", Response: protocol.Response{Status: "succeeded", Results: []protocol.Result{result}}}}}
	var out bytes.Buffer
	if err := (&presentation{mode: "nodes"}).human(&out, r, false); err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"    output omitted: budget exhausted"} {
		if !strings.Contains(out.String(), text) {
			t.Fatal(text, out.String())
		}
	}
}

// The human view shows a failed command's output, its truncation marker, and
// its exit code.
func TestHumanShowsTruncatedOutputAndExitCode(t *testing.T) {
	failed := protocol.Result{Task: "poll", Status: "failed", Change: "unchanged", Reason: "shell command exited with unaccepted code 3", ExitCode: 3,
		Stdout: "partial output" + protocol.TruncationMarker, StdoutTruncated: true, Stderr: "complete error",
		Tries: make([]protocol.Result, 10), TriesOmitted: 5}
	passed := protocol.Result{Task: "wait", Status: "succeeded", Change: "unchanged", Stdout: "ready", Tries: make([]protocol.Result, 2)}
	r := &controller.Run{ID: "x", Entries: []controller.Entry{{FleetStage: "check", Node: "web", Response: protocol.Response{Status: "failed", Results: []protocol.Result{failed, passed}}}}}
	var out bytes.Buffer
	if err := (&presentation{mode: "nodes"}).human(&out, r, false); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"    partial output\n    [MMX output truncated at 1 MiB]\n", "    complete error\n", "    exit code: 3\n"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("missing %q in:\n%s", want, out.String())
		}
	}
	for _, unwanted := range []string{"ready", "tries"} {
		if strings.Contains(out.String(), unwanted) {
			t.Errorf("unexpected %q in:\n%s", unwanted, out.String())
		}
	}
}

// A dryrun that rejects a candidate saves the validator's output under
// result.check_new_file, as JSON shows. Human dryrun and logs views show that
// cause too, with truncation marked and a secret task's text suppressed.
func TestHumanShowsRejectedCandidateValidatorOutput(t *testing.T) {
	rejected := func(task string, secret bool) protocol.Result {
		return protocol.Result{Task: task, Status: "failed", FailureKind: "validation", Change: "would_change", Secret: secret,
			Reason:     "check_new_file failed: shell command exited with unaccepted code 1",
			FileChange: &protocol.FileChange{Path: "/etc/nginx/" + task, Action: "update"},
			CheckNewFile: &protocol.Result{Status: "failed", Change: "unchanged", ExitCode: 1, Reason: "shell command exited with unaccepted code 1",
				Stdout: "checked" + protocol.TruncationMarker, StdoutTruncated: true, Stderr: "nginx: [emerg] unknown directive \"servr\" (" + task + ")"}}
	}
	skipped := protocol.Result{Task: "unchecked", Status: "succeeded", Change: "would_change",
		CheckNewFile: &protocol.Result{Status: "skipped", Change: "unchanged", Reason: "not checked: check_new_file has no inspection command for a dryrun"}}
	r := &controller.Run{ID: "x", Status: "failed", Dryrun: true, Entries: []controller.Entry{{FleetStage: "configure", Node: "web", NodeJobAttempt: 1,
		Response: protocol.Response{Status: "failed", Results: []protocol.Result{rejected("public.conf", false), rejected("private.conf", true), skipped}}}}}
	b, err := json.Marshal(r)
	if err != nil || !strings.Contains(string(b), "unknown directive") || strings.Contains(string(b), "private.conf)") {
		t.Fatal("fixture: JSON does not carry the public validator output alone", err)
	}
	for _, mode := range []string{"nodes", "summary"} {
		for _, details := range []bool{false, true} {
			var out bytes.Buffer
			if err := (&presentation{mode: mode}).human(&out, r, details); err != nil {
				t.Fatal(err)
			}
			for _, want := range []string{"      exit code: 1\n", "      checked\n      [MMX output truncated at 1 MiB]\n",
				"      nginx: [emerg] unknown directive \"servr\" (public.conf)\n", "      [redacted]\n"} {
				if !strings.Contains(out.String(), want) {
					t.Errorf("%s (details %v) missing %q in:\n%s", mode, details, want, out.String())
				}
			}
			if strings.Contains(out.String(), "private.conf)") {
				t.Errorf("%s (details %v) exposed suppressed or raw text:\n%s", mode, details, out.String())
			}
		}
	}
}

func TestFullDiffJSONAndMissingOrCorruptLocalArtifacts(t *testing.T) {
	p := presentation{state: t.TempDir(), diff: "all"}
	r := &controller.Run{ID: "saved"}
	text := strings.Repeat("+line\n", 300)
	ref, _ := assets.Hash(context.Background(), strings.NewReader(text))
	dir := filepath.Join(p.runDir(r), "diffs")
	if err := assets.Receive(context.Background(), dir, ref, strings.NewReader(text)); err != nil {
		t.Fatal(err)
	}
	// A running job's partial result names an artifact that is not local yet.
	pending, _ := assets.Hash(context.Background(), strings.NewReader("+pending\n"))
	r.Entries = []controller.Entry{
		{Node: "a", FleetStage: "setup", Response: protocol.Response{Status: "succeeded", Results: []protocol.Result{{Task: "config", FileChange: &protocol.FileChange{Path: "/etc/a", FullDiff: &ref}}}}},
		{Node: "b", FleetStage: "setup", Response: protocol.Response{Status: "running", Results: []protocol.Result{{Task: "config", FileChange: &protocol.FileChange{Path: "/etc/b", FullDiff: &pending}}}}},
	}
	var out bytes.Buffer
	if err := p.verifyDiffs(r); err != nil {
		t.Fatal(err)
	}
	if err := p.encodeRun(&out, map[string]any{"entries": r.Entries}, r); err != nil {
		t.Fatal(err)
	}
	var result struct {
		Entries []controller.Entry
		Full    map[string]string `json:"full_differences"`
		Missing []string          `json:"full_differences_unavailable"`
	}
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatal(err, out.String())
	}
	if len(result.Entries) != 2 || len(result.Full) != 1 || result.Full[ref.SHA256] != text || len(result.Missing) != 1 || result.Missing[0] != pending.SHA256 {
		t.Fatal("lossy JSON", out.String())
	}
	out.Reset()
	p.mode = "nodes"
	if err := p.human(&out, r, true); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"setup / a / config: full difference for \"/etc/a\"\n+line\n", "setup / b / config: full difference for \"/etc/b\" is not available locally\n"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("missing %q in:\n%s", want, out.String())
		}
	}
	// A corrupt or missing local artifact is shown as unavailable; the saved
	// task outcomes still display.
	path := differences.Local(dir, ref).Path
	if err := os.WriteFile(path, []byte(strings.Repeat("z", len(text))), 0600); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := p.verifyDiffs(r); err != nil || !p.unavailable[ref.SHA256] {
			t.Fatal("damaged artifact not reported unavailable", err)
		}
		os.Remove(path)
	}
}

func TestFullDiffInspectionIncludesPreviousAttempts(t *testing.T) {
	p := presentation{state: t.TempDir(), mode: "nodes", diff: "all"}
	r := &controller.Run{ID: "attempts"}
	store := differences.Store{Dir: filepath.Join(p.runDir(r), "diffs")}
	capture := store.Task()
	ref, _ := capture.Add("+earlier candidate\n")
	previous := capture.Finish(protocol.Result{Task: "role/file[item]", Status: "failed", FileChange: &protocol.FileChange{Path: "/etc/app", FullDiff: ref}})
	r.Entries = []controller.Entry{{Node: "app", FleetStage: "configure", Response: protocol.Response{Status: "succeeded"}, PreviousNodeJobAttempts: []controller.Attempt{{Response: protocol.Response{Results: []protocol.Result{previous}}}}}}
	var out bytes.Buffer
	if err := p.human(&out, r, true); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"previous attempt 1", "role/file[item]", "+earlier candidate"} {
		if !strings.Contains(out.String(), want) {
			t.Fatal(want, out.String())
		}
	}
	refs, err := p.refs(r)
	if err != nil || len(refs) != 1 {
		t.Fatal(refs, err)
	}
}

// An mmx run-command run shows every node's output, not only failures; summary
// output groups identical output and keeps distinct output apart.
func TestShellRunShowsEachNodesOutput(t *testing.T) {
	r := &controller.Run{EntryPoint: controller.RunCommandEntryPoint}
	for i, stdout := range []string{"same\n", "same\n", "different\n"} {
		result := protocol.Result{Task: "command", Status: "succeeded", Change: "changed", Stdout: stdout}
		r.Entries = append(r.Entries, controller.Entry{FleetStage: "shell", Node: fmt.Sprintf("web%d", i+1), Response: protocol.Response{Status: "succeeded", Results: []protocol.Result{result}}})
	}
	var out bytes.Buffer
	if err := (&presentation{mode: "summary"}).human(&out, r, false); err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{"shell / 2 nodes [web1, web2] / command: succeeded, changed\n    same\n", "shell / 1 node [web3] / command: succeeded, changed\n    different\n"} {
		if !strings.Contains(out.String(), text) {
			t.Fatal(text, out.String())
		}
	}
	r.EntryPoint = "/project/mmx_project_settings.yml"
	out.Reset()
	if err := (&presentation{mode: "summary"}).human(&out, r, false); err != nil || strings.Contains(out.String(), "same") {
		t.Fatal("a playbook run showed successful command output", err, out.String())
	}
}

func TestHumanHTTPResponseUsesExplicitFieldNames(t *testing.T) {
	var output bytes.Buffer
	commandOutput(&output, protocol.Result{HTTPStatusCode: 503, ResponseBody: "starting\n"}, "    ")
	if output.String() != "    http_status_code: 503\n    response_body: starting\n" {
		t.Fatal(output.String())
	}
}

// Files saved on the controller machine appear under their fleet stage, with
// their action and path; a secret file shows neither content nor difference.
func TestControllerMachineFilesAreShown(t *testing.T) {
	change := func(path, diff string) *protocol.FileChange {
		return &protocol.FileChange{Path: path, Action: "create", After: &protocol.FileState{Kind: "file", Size: 4, Mode: "0644"}, Diff: diff}
	}
	r := &controller.Run{Selections: map[string]*controller.Selection{"collect": {Path: "collect", Resolved: true, ControllerMachineFiles: []controller.ControllerMachineFile{
		{SaveOnControllerMachineIndex: 0, Node: "web01", Path: "/p/server_details/web01.txt", Result: protocol.Result{Task: "save_on_controller_machine[0]", Status: "succeeded", Change: "changed", FileChange: change("/p/server_details/web01.txt", "+report-text\n")}},
		{SaveOnControllerMachineIndex: 1, Path: "/p/token", Result: protocol.Result{Task: "save_on_controller_machine[1]", Status: "succeeded", Change: "changed", Secret: true, FileChange: change("/p/token", "+secret-text\n")}},
	}}}}
	var out bytes.Buffer
	p := presentation{mode: "nodes", diff: "preview"}
	if err := p.human(&out, r, false); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	for _, want := range []string{"collect / controller machine / save_on_controller_machine[0] from web01: succeeded, changed", `created "/p/server_details/web01.txt"`, "+report-text", "save_on_controller_machine[1]: succeeded, changed"} {
		if !strings.Contains(text, want) {
			t.Fatal(want, "\n", text)
		}
	}
	if strings.Contains(text, "secret-text") {
		t.Fatal("a secret file's difference was shown")
	}
}
