package controller

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"mmx/internal/config"
	"mmx/internal/modules"
	"mmx/internal/protocol"
)

func historyRun(t *testing.T, c Controller, id, status string, dry bool, completed time.Time, hash string) *Run {
	t.Helper()
	r := &Run{ID: id, Status: status, Dryrun: dry, Created: completed.Add(-time.Hour), Finished: &completed,
		EntryPoint: "/project/mmx.yml", InventoryName: "/project/nodes.yml", Project: config.Project{Project: "test"},
		Entries: []Entry{{FleetStage: "setup", Node: "test", RunnerHash: hash, Response: protocol.Response{Status: status}}}}
	if err := c.createRun(r); err != nil {
		t.Fatal(err)
	}
	if err := c.saveStatus(r); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestHistoryPrunesCompletedRunsAndUnusedRunners(t *testing.T) {
	c := Controller{State: t.TempDir()}
	now := time.Now().UTC()
	old, recent := now.Add(-48*time.Hour), now.Add(-time.Hour)
	retainedHash, expiredHash, otherHash := strings.Repeat("a", 64), strings.Repeat("b", 64), strings.Repeat("c", 64)
	for _, state := range []string{"succeeded", "failed", "stopped", "interrupted"} {
		historyRun(t, c, "01-"+state, state, false, old, expiredHash)
	}
	historyRun(t, c, "02-dryrun", "succeeded", true, old, expiredHash)
	latest := historyRun(t, c, "03-latest-normal", "succeeded", false, old, retainedHash)
	historyRun(t, c, "04-recent", "succeeded", true, recent, retainedHash)
	for _, state := range []string{"running", "unknown", "not_found"} {
		r := historyRun(t, c, "05-"+state, "failed", true, old, retainedHash)
		r.Entries[0].Response.Status = state
		if err := c.saveEntry(r.ID, r.Entries[0]); err != nil {
			t.Fatal(err)
		}
	}
	for _, key := range []string{"project", "entrypoint", "inventory"} {
		r := historyRun(t, c, "06-other-"+key, "succeeded", true, old, otherHash)
		switch key {
		case "project":
			r.Project.Project = "other"
		case "entrypoint":
			r.EntryPoint = "/other/mmx.yml"
		case "inventory":
			r.InventoryName = "/project/other.yml"
		}
		if err := c.createRun(r); err != nil {
			t.Fatal(err)
		}
	}
	dir := filepath.Join(c.State, "runners")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	for _, hash := range []string{retainedHash, expiredHash, otherHash, "unrelated"} {
		if err := os.WriteFile(filepath.Join(dir, hash), []byte("runner"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.Prune(latest, "24h", now); err != nil {
		t.Fatal(err)
	}
	files, err := os.ReadDir(filepath.Join(c.State, "runs"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 8 {
		t.Fatalf("retained %d runs: %v", len(files), files)
	}
	for _, f := range files {
		if strings.HasPrefix(f.Name(), "01-") || strings.HasPrefix(f.Name(), "02-") {
			t.Fatalf("expired run retained: %s", f.Name())
		}
	}
	if _, err := os.Stat(filepath.Join(dir, expiredHash)); !os.IsNotExist(err) {
		t.Fatal("unused runner retained", err)
	}
	for _, hash := range []string{retainedHash, otherHash, "unrelated"} {
		if _, err := os.Stat(filepath.Join(dir, hash)); err != nil {
			t.Fatal(err)
		}
	}
	// The latest normal success remains the default retry selection, even when older than the policy.
	if _, err := c.Load("", true); !errors.Is(err, ErrNothingToResume) {
		t.Fatal(err)
	}
}

func TestHistoryForeverAndActiveUseDeferCleanup(t *testing.T) {
	c := Controller{State: t.TempDir()}
	now := time.Now().UTC()
	old := historyRun(t, c, "01-old", "failed", false, now.Add(-48*time.Hour), "")
	latest := historyRun(t, c, "02-latest", "succeeded", false, now, "")
	if err := c.Prune(latest, "forever", now); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Load(old.ID, false); err != nil {
		t.Fatal(err)
	}
	release, err := c.UseHistory()
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Prune(latest, "24h", now); err != nil {
		t.Fatal(err)
	}
	release()
	if _, err := c.Load(old.ID, false); err != nil {
		t.Fatal("cleanup raced a history user", err)
	}
	if err := c.Prune(latest, "24h", now); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Load(old.ID, false); err == nil {
		t.Fatal("cleanup did not resume")
	}
}

// A retry stopped before a queued node is admitted keeps that node's earlier
// result instead of replacing it with the stop.
func TestStoppedRetryKeepsUnstartedNodesResult(t *testing.T) {
	c, r := fleet(t, config.FleetStage{ID: "setup", Nodes: "all", MaxNodesAtOnce: "1", OnNodeFailure: "continue"})
	// First run: every host fails.
	c.executeNode = func(_ context.Context, _ *Run, _ config.FleetStage, en Entry, _ bool, _ func(Entry) error) (Entry, error) {
		en.NodeJobAttempt, en.JobID = 1, strings.Repeat(en.Node, 64)
		en.Response = protocol.Response{ID: en.JobID, Status: "failed", Error: "real failure"}
		return en, nil
	}
	execute(t, c, r, false)
	loaded, err := c.Load(r.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	before := loaded.Entries[1]
	if before.Node != "b" || before.JobID == "" {
		t.Fatal("setup: b should hold its failed job")
	}
	// Retry: a is admitted, then the operator stops the run.
	c.executeNode = func(_ context.Context, run *Run, _ config.FleetStage, en Entry, _ bool, _ func(Entry) error) (Entry, error) {
		if en.Node != "a" {
			t.Errorf("admitted %s after the stop", en.Node)
		}
		en.Response.Status = "stopped"
		return en, c.requestStop(run.ID)
	}
	execute(t, c, loaded, true)
	after, err := c.Load(r.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(after.Entries[1], before) {
		t.Fatalf("stopped retry replaced b's result: %+v", after.Entries[1].Response)
	}
}

func TestCompletionAgeSurvivesInspectionAndResetsOnRetry(t *testing.T) {
	c, r := fleet(t, config.FleetStage{ID: "setup", Nodes: "all"})
	old := time.Now().UTC().Add(-48 * time.Hour)
	r.Status, r.Finished = "failed", &old
	for i := range r.Entries {
		r.Entries[i].Response.Status = "failed"
		if err := c.saveEntry(r.ID, r.Entries[i]); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.saveStatus(r); err != nil {
		t.Fatal(err)
	}
	if err := c.Inspect(context.Background(), r, false); err != nil {
		t.Fatal(err)
	}
	if r.Finished == nil || !r.Finished.Equal(old) {
		t.Fatal("inspection extended retention", r.Finished)
	}
	c.Progress = func(r *Run) {
		if r.Status == "running" && r.Finished != nil {
			t.Error("retry kept prior completion time")
		}
	}
	c.executeNode = func(_ context.Context, _ *Run, _ config.FleetStage, en Entry, _ bool, _ func(Entry) error) (Entry, error) {
		en.Response.Status = "succeeded"
		return en, nil
	}
	before := time.Now()
	if err := c.Execute(context.Background(), r, true); err != nil {
		t.Fatal(err)
	}
	if r.Finished == nil || r.Finished.Before(before) {
		t.Fatal("retry did not set new completion time", r.Finished)
	}
	saved, err := c.Load(r.ID, false)
	if err != nil || saved.Finished == nil || !saved.Finished.Equal(*r.Finished) {
		t.Fatal("completion not persisted", saved, err)
	}
}

func TestLatestSuccessfulRunDoesNotReviveOlderFailure(t *testing.T) {
	c := Controller{State: t.TempDir(), Selection: Identity{"/project/mmx.yml", "example", "/project/nodes.yml", ""}}
	content := "v1"
	p := config.Project{Project: "example", MaxNodesAtOnce: 1, FleetStages: []config.FleetStage{{ID: "setup", Nodes: "all", NodeTasks: []config.Task{{ID: "config", Module: "file", ModuleInputs: &modules.File{Mode: "0644", Path: "/etc/example", Content: &content}}}}}}
	inv := config.Inventory{Nodes: map[string]config.Node{"node": {Address: "node"}}}
	old, err := c.New(context.Background(), p, inv, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	old.Status = "failed"
	old.Entries[0].Response = protocol.Response{Status: "failed", Error: "original failure", Results: []protocol.Result{{Task: "config", Status: "failed"}}}
	if err := c.saveEntry(old.ID, old.Entries[0]); err != nil {
		t.Fatal(err)
	}
	if err := c.saveStatus(old); err != nil {
		t.Fatal(err)
	}
	current, err := c.New(context.Background(), p, inv, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	current.Status = "succeeded"
	current.Entries[0].Response = protocol.Response{Status: "succeeded", Results: []protocol.Result{{Task: "config", Status: "succeeded"}}}
	if err := c.saveEntry(current.ID, current.Entries[0]); err != nil {
		t.Fatal(err)
	}
	if err := c.saveStatus(current); err != nil {
		t.Fatal(err)
	}
	if _, err := c.New(context.Background(), p, inv, nil, true); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Load("", true); !errors.Is(err, ErrNothingToResume) {
		t.Fatalf("stale failed run remains eligible: %v", err)
	}
	prior, err := c.Load(old.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if prior.Status != "failed" || prior.Entries[0].Response.Error != "original failure" {
		t.Fatal("historical failure was changed")
	}
}
func TestLatestRetryIsScopedToEntryPointProjectAndInventory(t *testing.T) {
	c := Controller{State: t.TempDir(), Selection: Identity{"/project/a.yml", "a", "/project/production.yml", ""}}
	p := config.Project{Project: "a", MaxNodesAtOnce: 1, FleetStages: []config.FleetStage{{ID: "setup", Nodes: "all"}}}
	inv := config.Inventory{Nodes: map[string]config.Node{"node": {Address: "node"}}}
	wanted, err := c.New(context.Background(), p, inv, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	other := c
	other.Selection.Inventory = "/project/staging.yml"
	if _, err := other.New(context.Background(), p, inv, nil, false); err != nil {
		t.Fatal(err)
	}
	other = c
	other.Selection.EntryPoint = "/project/b.yml"
	if _, err := other.New(context.Background(), p, inv, nil, false); err != nil {
		t.Fatal(err)
	}
	other = c
	p.Project = "b"
	if _, err := other.New(context.Background(), p, inv, nil, false); err != nil {
		t.Fatal(err)
	}
	selected, err := c.Load("", true)
	if err != nil || selected.ID != wanted.ID {
		t.Fatalf("wrong retry context: %v %v", selected, err)
	}
}

// A run whose controller was lost while its job ran stays kept, however old,
// so `mmx status` or `mmx resume` can still confirm its work.
func TestPruneKeepsRunsWithUnconfirmedWork(t *testing.T) {
	c := Controller{State: t.TempDir()}
	now := time.Now().UTC()
	old := now.Add(-48 * time.Hour)
	for _, status := range []string{"running", "unknown", "not_found"} {
		r := historyRun(t, c, "01-"+status, "running", false, old, "")
		r.Entries[0].JobID = "job"
		r.Entries[0].Response = protocol.Response{ID: "job", Status: status}
		if err := c.saveEntry(r.ID, r.Entries[0]); err != nil {
			t.Fatal(err)
		}
	}
	latest := historyRun(t, c, "02-latest", "succeeded", false, now, "")
	if err := c.Prune(latest, "24h", now.Add(365*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"01-running", "01-unknown", "01-not_found"} {
		if _, err := os.Stat(c.runDir(id)); err != nil {
			t.Fatal("unconfirmed run expired", err)
		}
	}
}

// A job left unconfirmed on a machine since replaced can never be answered for:
// status fails the node fleet stage with the replacement as its reason and keeps the
// job as an earlier attempt, so the run finishes and can expire.
func TestStatusFailsUnconfirmedJobOnReplacedNode(t *testing.T) {
	for _, status := range []string{"running", "unknown", "not_found"} {
		t.Run(status, func(t *testing.T) {
			trace := filepath.Join(t.TempDir(), "trace")
			c, r := sshNode(t, `printf '%s\n' "$command" >> `+quote(trace)+`; echo unexpected runner command >&2; exit 1`)
			job := strings.Repeat("a", 64)
			en := r.Entries[0]
			en.JobID, en.NodeJobAttempt = job, 1
			en.NodeID, en.Architecture, en.Transport = strings.Repeat("0", 64), "amd64", "ssh"
			uncertain := protocol.Response{ID: job, Status: status}
			en.Response = uncertain
			if err := c.saveEntry(r.ID, en); err != nil {
				t.Fatal(err)
			}
			time.Sleep(time.Millisecond) // a later run ID
			latest, err := c.New(context.Background(), r.Project, r.Inventory, nil, false)
			if err != nil {
				t.Fatal(err)
			}
			latest.Status = "succeeded"
			if err := c.saveStatus(latest); err != nil {
				t.Fatal(err)
			}
			loaded, err := c.Load(r.ID, false)
			if err != nil {
				t.Fatal(err)
			}
			if err := c.Inspect(context.Background(), loaded, false); err != nil {
				t.Fatal(err)
			}
			saved, err := c.Load(r.ID, false)
			if err != nil {
				t.Fatal(err)
			}
			got := saved.Entries[0]
			if saved.Status != "failed" || got.JobID != "" || got.NodeJobAttempt != 1 || got.Response.Status != "failed" || !strings.Contains(got.Response.Error, "node was replaced") ||
				!reflect.DeepEqual(got.PreviousNodeJobAttempts, []Attempt{{JobID: job, Response: uncertain}}) || lines(t, trace) != nil {
				t.Fatalf("%s %+v, attempts %+v", saved.Status, got.Response, got.PreviousNodeJobAttempts)
			}
			if err := c.Prune(latest, "24h", time.Now().Add(48*time.Hour)); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(c.runDir(r.ID)); !os.IsNotExist(err) {
				t.Fatal("finished run on a replaced node was kept", err)
			}
		})
	}
}

// A file system that refuses file permissions, as a Windows drive under WSL
// can, refuses chmod with "operation not permitted". A lock file that links
// to /dev/null, which root owns, gets the same refusal on any system.
func TestHistoryNamesAFileSystemThatRefusesFilePermissions(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("as root, the chmod would change the permissions of /dev/null")
	}
	c := Controller{State: t.TempDir()}
	if err := os.Symlink("/dev/null", filepath.Join(c.State, "history.lock")); err != nil {
		t.Fatal(err)
	}
	want := "MMX cannot save files in " + c.State + ": the file system there refuses file permissions, " +
		"and MMX sets the permissions of every file it saves. A Windows drive under WSL, such as /mnt/c, can do this. " +
		"Move the project directory to a Linux file system, such as your home directory in WSL, then run again."
	release, err := c.UseHistory()
	if err == nil {
		release()
	}
	if err == nil || err.Error() != want {
		t.Fatalf("got %v\nwant %s", err, want)
	}
}

// A saved run that this MMX cannot read, such as one with a field that an
// earlier version wrote, stops cleanup until the person removes it: every
// such run is named, and nothing is removed meanwhile.
func TestPruneNamesUnreadableRunsAndRemovesNothing(t *testing.T) {
	c := Controller{State: t.TempDir()}
	now := time.Now().UTC()
	old := now.Add(-48 * time.Hour)
	expired := historyRun(t, c, "01-expired", "succeeded", false, old, "")
	var unreadable []string
	for _, id := range []string{"02-other-version", "03-other-version"} {
		historyRun(t, c, id, "succeeded", false, old, "")
		path := filepath.Join(c.runDir(id), "run.json")
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(strings.Replace(string(b), "{", `{"order":[0],`, 1)), 0600); err != nil {
			t.Fatal(err)
		}
		unreadable = append(unreadable, c.runDir(id))
	}
	latest := historyRun(t, c, "04-latest", "succeeded", false, now, "")
	field := ` (json: unknown field "order")`
	reasons := " History cleanup removes nothing while a saved run cannot be read, because that run may use a cached runner binary. "
	want := "MMX cannot read these saved runs, which another version of MMX may have written: " + unreadable[1] + field + "; " + unreadable[0] + field + "." +
		reasons + "Remove those run directories, then run again."
	if err := c.Prune(latest, "24h", now); err == nil || err.Error() != want {
		t.Fatalf("got %v\nwant %s", err, want)
	}
	if _, err := os.Stat(c.runDir(expired.ID)); err != nil {
		t.Fatal("cleanup removed a run while another could not be read", err)
	}
	if err := os.RemoveAll(unreadable[1]); err != nil {
		t.Fatal(err)
	}
	want = "MMX cannot read this saved run, which another version of MMX may have written: " + unreadable[0] + field + "." +
		reasons + "Remove that run directory, then run again."
	if err := c.Prune(latest, "24h", now); err == nil || err.Error() != want {
		t.Fatalf("got %v\nwant %s", err, want)
	}
	if err := os.RemoveAll(unreadable[0]); err != nil {
		t.Fatal(err)
	}
	if err := c.Prune(latest, "24h", now); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(c.runDir(expired.ID)); !os.IsNotExist(err) {
		t.Fatal("the expired run stayed after the unreadable runs were removed", err)
	}
}
