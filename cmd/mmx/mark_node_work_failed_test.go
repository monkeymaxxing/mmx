package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"mmx/internal/config"
	"mmx/internal/controller"
	"mmx/internal/durable"
	"mmx/internal/protocol"
)

// A run whose node MMX can no longer reach: status names mmx
// mark-node-work-failed beside resume, and the command records the node's
// unconfirmed work as failed, in that run or, without a run ID, in every
// saved run, and says so in JSON and in words.
func TestMarkNodeWorkFailedCommand(t *testing.T) {
	root := initializedProject(t)
	state := filepath.Join(root, ".mmx")
	c := controller.Controller{State: state}
	// The address does not resolve, so SSH fails at once with exit status 255.
	inv := config.Inventory{Nodes: map[string]config.Node{"gone01": {Address: "gone01.invalid"}}}
	r, err := c.New(context.Background(), config.Project{HistoryRetention: "forever"}, inv, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	r.Status = "unknown"
	r.Entries = []controller.Entry{{FleetStage: "restore", Node: "gone01", JobID: "job-on-gone01",
		Response: protocol.Response{Status: "unknown", Error: "ssh: connect to host port 22: Connection timed out"}}}
	if err := durable.JSON(filepath.Join(state, "runs", r.ID, "run.json"), r); err != nil {
		t.Fatal(err)
	}
	short := controller.ShortID(r.ID)

	out, code, err := captureCommand(t, "status", r.ID)
	if code != 3 || err != nil || !strings.Contains(out, "If server gone01 is gone for good: mmx mark-node-work-failed --node gone01 "+short+"\n") {
		t.Fatalf("%d %v\n%s", code, err, out)
	}
	if _, code, err := captureCommand(t, "mark-node-work-failed", r.ID); code != 4 || err == nil || !strings.Contains(err.Error(), "--node") {
		t.Fatal(code, err)
	}

	out, code, err = captureCommand(t, "mark-node-work-failed", "--node", "gone01", "--json")
	var view struct {
		Node, Reason string
		Runs         []struct {
			ID                       string
			NodeJobsRecordedAsFailed int `json:"node_jobs_recorded_as_failed"`
			Status                   string
		}
	}
	if code != 0 || err != nil || json.Unmarshal([]byte(out), &view) != nil || view.Node != "gone01" || view.Reason != controller.ReasonServerIsGone ||
		len(view.Runs) != 1 || view.Runs[0].ID != r.ID || view.Runs[0].NodeJobsRecordedAsFailed != 1 || view.Runs[0].Status != "failed" {
		t.Fatalf("%d %v\n%s", code, err, out)
	}
	saved, code, err := captureCommand(t, "results", r.ID, "--json")
	if code != 0 || err != nil || !strings.Contains(saved, `"error": "`+controller.ReasonServerIsGone+`; MMX could not reach it: `) {
		t.Fatalf("%d %v\n%s", code, err, saved)
	}
	if out, code, err = captureCommand(t, "mark-node-work-failed", "--node", "gone01"); code != 0 || err != nil || out != "No saved run has unconfirmed work on node gone01.\n" {
		t.Fatalf("%d %v %q", code, err, out)
	}
}

// Every run is tried: a run whose saved SSH configuration file is gone, with
// a temporary project, is recorded because no command can reach its node,
// and saying so; a run that its mmx process holds is not recorded. The JSON
// names both, and the exit code is the more serious outcome.
func TestMarkNodeWorkFailedTriesEveryRun(t *testing.T) {
	root := initializedProject(t)
	state := filepath.Join(root, ".mmx")
	c := controller.Controller{State: state}
	saveRun := func(node config.Node) *controller.Run {
		t.Helper()
		r, err := c.New(context.Background(), config.Project{HistoryRetention: "forever"}, config.Inventory{Nodes: map[string]config.Node{"gone01": node}}, nil, false)
		if err != nil {
			t.Fatal(err)
		}
		r.Status = "unknown"
		r.Entries = []controller.Entry{{FleetStage: "restore", Node: "gone01", JobID: "job-" + r.ID,
			Response: protocol.Response{Status: "unknown", Error: "ssh: connect to host port 22: Connection timed out"}}}
		if err := durable.JSON(filepath.Join(state, "runs", r.ID, "run.json"), r); err != nil {
			t.Fatal(err)
		}
		return r
	}
	configurationGone := saveRun(config.Node{Address: "10.255.255.1", SSHConfig: filepath.Join(root, "removed-with-the-temporary-project", "ssh_config")})
	held := saveRun(config.Node{Address: "gone01.invalid"})
	lock, err := os.OpenFile(filepath.Join(state, "runs", held.ID, "control.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}

	out, code, err := captureCommand(t, "mark-node-work-failed", "--node", "gone01", "--json")
	var view struct {
		Runs []struct {
			ID                       string
			NodeJobsRecordedAsFailed int `json:"node_jobs_recorded_as_failed"`
		}
		RunsNotRecorded []struct {
			ID       string
			Reason   string
			ExitCode int `json:"exit_code"`
		} `json:"runs_not_recorded"`
	}
	if code != 4 || err != nil || json.Unmarshal([]byte(out), &view) != nil ||
		len(view.Runs) != 1 || view.Runs[0].ID != configurationGone.ID || view.Runs[0].NodeJobsRecordedAsFailed != 1 ||
		len(view.RunsNotRecorded) != 1 || view.RunsNotRecorded[0].ID != held.ID || view.RunsNotRecorded[0].ExitCode != 4 ||
		!strings.Contains(view.RunsNotRecorded[0].Reason, "still running in an mmx process") {
		t.Fatalf("%d %v\n%s", code, err, out)
	}
	saved, _, err := captureCommand(t, "results", configurationGone.ID, "--json")
	if err != nil || !strings.Contains(saved, controller.ReasonServerIsGone+"; MMX could not try to reach it: resolve SSH configuration for 10.255.255.1: exit status 255: ") ||
		!strings.Contains(saved, "Can't open user config file") {
		t.Fatal(err, saved)
	}
}

// mark-node-work-failed scans every saved run, so it refuses --inventory like
// the other options it does not use.
func TestMarkNodeWorkFailedRefusesInventory(t *testing.T) {
	initializedProject(t)
	_, code, err := captureCommand(t, "mark-node-work-failed", "--node", "gone01", "--inventory", "example")
	if code != 4 || err == nil || err.Error() != "mark-node-work-failed does not accept --inventory; it scans every saved run" {
		t.Fatal(code, err)
	}
}
