package engine

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mmx/internal/config"
	"mmx/internal/modules"
)

func TestFailedCleanupDoesNotRestrictNewWork(t *testing.T) {
	dir := t.TempDir()
	old := filepath.Join(dir, "old")
	next := filepath.Join(dir, "next")
	e := Engine{RuntimeRoot: filepath.Join(dir, "state")}
	j := job(config.Task{TaskBlock: &config.TaskBlock{ID: "server", Changes: []config.Task{taskFile(old, "old")}, Cleanup: []config.Task{{Module: "shell", ModuleInputs: &modules.Shell{Script: "exit 1"}}}}})
	requireStatus(t, e.Run(context.Background(), j), "failed")
	j.NodeTasks = []config.Task{taskFile(next, "next")}
	requireStatus(t, e.Run(context.Background(), j), "succeeded")
	if read(t, old) != "old" || read(t, next) != "next" {
		t.Fatal("old cleanup was replayed or new work was lost")
	}
}

func TestCustomActivationRunsOnEachAttemptAndCleanupFollowsFailure(t *testing.T) {
	dir := t.TempDir()
	count := filepath.Join(dir, "count")
	e := Engine{RuntimeRoot: filepath.Join(dir, "state")}
	op := config.Task{TaskBlock: &config.TaskBlock{ID: "server", Changes: []config.Task{{Module: "shell", ModuleInputs: &modules.Shell{Script: "true", ChangedWhen: false}}}, Activate: []config.Task{{Module: "shell", ModuleInputs: &modules.Shell{Script: "echo action >> \"$COUNT\"", Env: map[string]any{"COUNT": count}}}}}}
	for i := 0; i < 2; i++ {
		requireStatus(t, e.Run(context.Background(), job(op)), "succeeded")
	}
	if read(t, count) != "action\naction\n" {
		t.Fatal("custom action was silently skipped")
	}
	op.TaskBlock.Changes = []config.Task{{Module: "shell", ModuleInputs: &modules.Shell{Script: "exit 2"}}}
	op.TaskBlock.OnFailure = []config.Task{{Module: "shell", ModuleInputs: &modules.Shell{Script: "exit 3"}}}
	op.TaskBlock.Cleanup = []config.Task{taskFile(filepath.Join(dir, "cleaned"), "yes")}
	r := e.Run(context.Background(), job(op))
	requireStatus(t, r, "failed")
	if !strings.Contains(r.Error, "on_failure") || read(t, filepath.Join(dir, "cleaned")) != "yes" {
		t.Fatal(r)
	}
}

func TestDryrunDoesNotVerifyOldStateAfterCustomChanges(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++; w.WriteHeader(200) }))
	defer server.Close()
	j := job(config.Task{TaskBlock: &config.TaskBlock{ID: "server", Changes: []config.Task{{Module: "shell", ModuleInputs: &modules.Shell{Script: "exit 1", Dryrun: "true"}}}, Verify: []config.Task{{Module: "http", ModuleInputs: &modules.HTTP{URL: server.URL}}}}})
	j.Dryrun = true
	e := Engine{RuntimeRoot: t.TempDir()}
	r := e.Run(context.Background(), j)
	requireStatus(t, r, "succeeded")
	if calls != 0 || r.Results[1].Status != "skipped" {
		t.Fatal("dryrun certified old running state", r)
	}
}

// A record directory is closed to every other account. Its parent may let
// other accounts pass through, as the runner's volatile directory does for
// the private directories of node tasks, and nothing more.
func TestRecordDirectoryIsClosedBelowAParentOthersOnlyPassThrough(t *testing.T) {
	parent := filepath.Join(t.TempDir(), "mmx")
	dir := filepath.Join(parent, "services")
	if err := privateDirectory(dir); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		path     string
		mode     os.FileMode
		accepted bool
	}{{parent, 0711, true}, {parent, 0755, false}, {parent, 0733, false}, {parent, 0700, true}, {dir, 0711, false}, {dir, 0700, true}} {
		if err := os.Chmod(c.path, c.mode); err != nil {
			t.Fatal(err)
		}
		if err := privateDirectory(dir); (err == nil) != c.accepted {
			t.Fatalf("%s with mode %o: %v", c.path, c.mode, err)
		}
	}
}
