package main

import (
	"context"
	"mmx/internal/config"
	"mmx/internal/durable"
	"path/filepath"
	"strings"
	"testing"

	"mmx/internal/controller"
	"mmx/internal/protocol"
)

func TestRunExitCodes(t *testing.T) {
	for _, tc := range []struct {
		name, status    string
		dryrun, pending bool
		results         []protocol.Result
		want            int
	}{
		{name: "run", status: "succeeded", want: 0},
		{name: "failed", status: "failed", want: 1},
		{name: "stopped", status: "stopped", want: 1},
		{name: "unfinished", status: "running", want: 1},
		{name: "unchanged", status: "succeeded", dryrun: true, want: 0},
		{name: "changed", status: "succeeded", dryrun: true, results: []protocol.Result{{Status: "succeeded", Change: "would_change"}}, want: 2},
		{name: "unknown beats changed", status: "succeeded", dryrun: true, results: []protocol.Result{{Status: "succeeded", Change: "would_change"}, {Status: "skipped", Change: "unknown"}}, want: 3},
		{name: "failed beats unknown", status: "failed", dryrun: true, results: []protocol.Result{{Status: "not_checked", Change: "unknown"}}, want: 1},
		{name: "nested loop unknown", status: "succeeded", dryrun: true, results: []protocol.Result{{Status: "succeeded", Change: "would_change", Items: []protocol.Result{{Status: "not_checked"}}}}, want: 3},
		{name: "unperformed candidate check", status: "succeeded", dryrun: true, results: []protocol.Result{{Status: "succeeded", Change: "would_change", CheckNewFile: &protocol.Result{Status: "skipped"}}}, want: 3},
		{name: "candidate check failed after an earlier change", status: "succeeded", dryrun: true, results: []protocol.Result{{Status: "succeeded", Change: "would_change"}, {Status: "not_checked", Change: "unknown", CheckNewFile: &protocol.Result{Status: "failed", Change: "unchanged", ExitCode: 1}}}, want: 3},
		{name: "candidate check failed with nothing earlier", status: "succeeded", dryrun: true, results: []protocol.Result{{Status: "failed", Change: "unchanged", CheckNewFile: &protocol.Result{Status: "failed", Change: "unchanged", ExitCode: 1}}}, want: 1},
		{name: "pending fleet_stages", status: "succeeded", dryrun: true, pending: true, want: 3},
		{name: "stopped dryrun", status: "stopped", dryrun: true, want: 1},
		{name: "failed beats unconfirmed", status: "failed", dryrun: true, results: []protocol.Result{{Status: "unknown", Change: "would_change"}}, want: 1},
		{name: "unconfirmed node job", status: "not_found", dryrun: true, want: 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &controller.Run{Status: tc.status, Dryrun: tc.dryrun, FleetStagesAwaitingNodeSelection: tc.pending, Entries: []controller.Entry{{Response: protocol.Response{Status: tc.status, Results: tc.results}}}}
			if got := runExitCode(r); got != tc.want {
				t.Fatalf("got %d, want %d", got, tc.want)
			}
		})
	}
	r := &controller.Run{Status: "succeeded", Dryrun: true, Selections: map[string]*controller.Selection{"loop": {Iterations: map[string]protocol.Response{"one": {Status: "not_checked"}}}}}
	if runExitCode(r) != 3 {
		t.Fatal("lost incomplete controller loop result")
	}
}

func TestUsageConfigurationAndCanceledPreparationCodes(t *testing.T) {
	for _, args := range [][]string{{"unknown"}, {"run", "--invalid"}, {"validate", "--file", t.TempDir() + "/missing.yml"}, {"init", "unexpected"}, {"version", "--invalid"}, {"module-json", "get", "--invalid"}} {
		code, err := run(context.Background(), args)
		if code != 4 || err == nil {
			t.Fatal(args, code, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	code, _ := run(ctx, []string{"run", "--file", t.TempDir() + "/missing.yml"})
	if code != 130 {
		t.Fatal(code)
	}
}

func TestPlaybookLoadExitCodes(t *testing.T) {
	initializedProject(t)
	writeProjectFile(t, "playbooks/broken.yml", "fleet_stages: invalid\n")
	for _, name := range []string{"broken", "missing"} {
		for _, command := range []string{"run", "dryrun", "explain", "nodes", "validate"} {
			want := 4
			if command == "validate" {
				want = 1
			}
			if _, code, err := captureCommand(t, command, name); code != want || err == nil {
				t.Fatal(command, name, code, err)
			}
		}
	}
}

func TestNoMatchingRunExitCode(t *testing.T) {
	initializedProject(t)
	for _, command := range []string{"resume", "results", "status", "stop"} {
		for _, id := range []string{"", "missing"} {
			args := []string{command}
			if id != "" {
				args = append(args, id)
			}
			// A default resume, too, finds no saved run here: that is a
			// command error, unlike a matching run with nothing unfinished.
			_, code, err := captureCommand(t, args...)
			if code != 4 || err == nil || !strings.Contains(err.Error(), "no matching saved run") {
				t.Fatal(args, code, err)
			}
		}
	}
}

func TestInspectionExitCodes(t *testing.T) {
	root := initializedProject(t)
	state := filepath.Join(root, ".mmx")
	c := controller.Controller{State: state}
	for _, status := range []string{"failed", "stopped", "running", "unknown", "not_found"} {
		for _, command := range []string{"results", "status", "stop"} {
			r, err := c.New(context.Background(), config.Project{HistoryRetention: "forever"}, config.Inventory{}, nil, false)
			if err != nil {
				t.Fatal(err)
			}
			r.Status = status
			r.Entries = []controller.Entry{{FleetStage: "setup", Node: "example", Response: protocol.Response{Status: status}}}
			if err := durable.JSON(filepath.Join(state, "runs", r.ID, "run.json"), r); err != nil {
				t.Fatal(err)
			}
			want := 0
			if command != "results" && protocol.Unconfirmed(status) {
				want = 3
			}
			if _, code, err := captureCommand(t, command, r.ID, "--json"); code != want || err != nil {
				t.Fatal(command, status, code, err)
			}
		}
	}
}

func TestMissingSecretBeforeRunExitCode(t *testing.T) {
	initializedProject(t)
	t.Setenv("MMX_EXIT_TEST_SECRET", "")
	writeProjectFile(t, "mmx_project_settings.yml", "project_name: test\ndefault_inventory: example\nsecrets_from_env: {token: MMX_EXIT_TEST_SECRET}\n")
	writeProjectFile(t, "playbooks/example.yml", "fleet_stages: [{id: setup, nodes: all, node_tasks: [{show: '{{ .secrets.token }}'}]}]\n")
	// No resume: a run stopped before any node job starts is not saved.
	for _, command := range []string{"run", "dryrun"} {
		args := []string{command, "--json", "--non-interactive", "example"}
		if _, code, err := captureCommand(t, args...); code != 4 || err == nil || !strings.Contains(err.Error(), "MMX_EXIT_TEST_SECRET") {
			t.Fatal(command, code, err)
		}
	}
}

func TestHelpExitCodeTable(t *testing.T) {
	out, code, err := captureCommand(t, "--help")
	if code != 0 || err != nil || !strings.Contains(out, exitCodeHelp) {
		t.Fatal(code, err, out)
	}
}

// A run stopped before any node job starts, here by a missing secret, is not
// saved and gets no advice to resume it or start it again.
func TestRunStoppedBeforeAnyNodeJobSavesNothingAndAdvisesNothing(t *testing.T) {
	initializedProject(t)
	t.Setenv("MMX_EXIT_TEST_SECRET", "")
	writeProjectFile(t, "mmx_project_settings.yml", "project_name: test\ndefault_inventory: example\nsecrets_from_env: {token: MMX_EXIT_TEST_SECRET}\n")
	writeProjectFile(t, "playbooks/example.yml", "fleet_stages: [{id: setup, nodes: all, node_tasks: [{show: '{{ .secrets.token }}'}]}]\n")
	out, code, err := captureCommand(t, "run", "example", "--non-interactive")
	if code != 4 || err == nil || !strings.Contains(err.Error(), "MMX_EXIT_TEST_SECRET") {
		t.Fatal(code, err)
	}
	for _, advice := range []string{"Resume unfinished work", "Start a new run", "Saved:"} {
		if strings.Contains(out, advice) {
			t.Errorf("output has %q:\n%s", advice, out)
		}
	}
	if saved, _ := filepath.Glob(filepath.Join(".mmx", "runs", "*")); len(saved) != 0 {
		t.Errorf("saved runs: %v", saved)
	}
}

// mmx resume refuses a dryrun, so the end of a failed or unfinished dryrun
// does not offer it; a run that is not a dryrun still does.
func TestUnfinishedDryrunDoesNotOfferResume(t *testing.T) {
	root := initializedProject(t)
	state := filepath.Join(root, ".mmx")
	c := controller.Controller{State: state}
	for _, tc := range []struct {
		dryrun bool
		status string
	}{{true, "failed"}, {true, "stopped"}, {false, "failed"}} {
		r, err := c.New(context.Background(), config.Project{HistoryRetention: "forever"}, config.Inventory{}, nil, tc.dryrun)
		if err != nil {
			t.Fatal(err)
		}
		r.Status = tc.status
		r.Entries = []controller.Entry{{FleetStage: "setup", Node: "example", Response: protocol.Response{Status: tc.status}}}
		if err := durable.JSON(filepath.Join(state, "runs", r.ID, "run.json"), r); err != nil {
			t.Fatal(err)
		}
		out, _, err := captureCommand(t, "results", r.ID)
		if err != nil || !strings.Contains(out, "Inspect:") {
			t.Fatal(err, out)
		}
		if offers := strings.Contains(out, "Resume unfinished work: mmx resume "); offers == tc.dryrun {
			t.Errorf("dryrun %v, status %s: resume offered %v\n%s", tc.dryrun, tc.status, offers, out)
		}
	}
}

// The help text names every case of exit codes 2, 3, and 4 that the code uses.
func TestHelpExitCodeTableNamesRefusedFleetStageFilter(t *testing.T) {
	for _, want := range []string{"--only-fleet-stages", "inspected no nodes"} {
		if !strings.Contains(exitCodeHelp, want) {
			t.Errorf("exit code help lacks %q", want)
		}
	}
}
