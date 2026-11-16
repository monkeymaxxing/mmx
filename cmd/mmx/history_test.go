package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mmx/internal/config"
	"mmx/internal/controller"
	"mmx/internal/durable"
)

// Without an ID, logs, status, stop, and retry select the latest run.
func TestCommandsSelectTheLatestRunByDefault(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("MMX_STATE_DIR", filepath.Join(dir, "state"))
	entry := filepath.Join(dir, "mmx_project_settings.yml")
	if err := os.WriteFile(entry, []byte("project_name: test\ndefault_inventory: example\nrun_history_retention: forever\n"), 0600); err != nil {
		t.Fatal(err)
	}
	c := controller.Controller{State: filepath.Join(dir, "state"), Selection: controller.Identity{EntryPoint: entry, Project: "test", Inventory: "example"}}
	var latest *controller.Run
	for range 2 {
		r, err := c.New(context.Background(), config.Project{Project: "test", Inventory: "example"}, config.Inventory{}, nil, false)
		if err != nil {
			t.Fatal(err)
		}
		latest = r
	}
	for _, command := range []string{"results", "status", "stop", "resume"} {
		stdout, err := os.CreateTemp(dir, "stdout")
		if err != nil {
			t.Fatal(err)
		}
		saved := os.Stdout
		os.Stdout = stdout
		code, err := run(context.Background(), append([]string{command}, "--json"))
		os.Stdout = saved
		stdout.Close()
		if code != 0 || err != nil {
			t.Fatal(command, code, err)
		}
		b, _ := os.ReadFile(stdout.Name())
		var view struct{ ID, Status string }
		if err := json.Unmarshal(b, &view); err != nil {
			t.Fatal(command, err, string(b))
		}
		if command == "resume" && view.Status != "nothing_to_resume" || command != "resume" && view.ID != latest.ID {
			t.Fatal(command, "selected", string(b))
		}
	}
	if code, err := run(context.Background(), []string{"run", "last"}); code != 4 || err == nil {
		t.Fatal("run accepted a run id", code, err)
	}
}

// Each inventory keeps its own default run; a short ID selects any run, and
// retry reuses the recorded limit and --var values instead of accepting them.
func TestInventoryDefaultsAndShortRunIDs(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("MMX_STATE_DIR", filepath.Join(dir, "state"))
	entry := filepath.Join(dir, "mmx_project_settings.yml")
	if err := os.WriteFile(entry, []byte("project_name: test\ndefault_inventory: example\nrun_history_retention: forever\n"), 0600); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(dir, "state")
	runs := map[string]*controller.Run{}
	for _, inventory := range []string{"example", "staging"} {
		c := controller.Controller{State: state, Selection: controller.Identity{EntryPoint: entry, Project: "test", Inventory: inventory}}
		r, err := c.New(context.Background(), config.Project{Project: "test", Inventory: "example"}, config.Inventory{}, nil, false)
		if err != nil {
			t.Fatal(err)
		}
		runs[inventory] = r
	}
	logs := func(args ...string) string {
		t.Helper()
		stdout, err := os.CreateTemp(dir, "stdout")
		if err != nil {
			t.Fatal(err)
		}
		saved := os.Stdout
		os.Stdout = stdout
		code, err := run(context.Background(), append([]string{"results", "--json"}, args...))
		os.Stdout = saved
		stdout.Close()
		if code != 0 || err != nil {
			t.Fatal(args, code, err)
		}
		b, _ := os.ReadFile(stdout.Name())
		var view struct {
			ID      string
			ShortID string `json:"short_id"`
		}
		if err := json.Unmarshal(b, &view); err != nil || view.ShortID != controller.ShortID(view.ID) {
			t.Fatal(err, string(b))
		}
		return view.ID
	}
	staging := runs["staging"].ID
	if logs() != runs["example"].ID || logs("--inventory", "staging") != staging || logs(controller.ShortID(staging)) != staging {
		t.Fatal("wrong run selected")
	}
	for _, command := range []string{"resume", "status", "results", "validate"} {
		if code, err := run(context.Background(), []string{command, "--limit", "web"}); code != 4 || err == nil || !strings.Contains(err.Error(), "--limit") {
			t.Fatal(command, code, err)
		}
	}
	for _, command := range []string{"resume", "status", "results"} {
		if code, err := run(context.Background(), []string{command, "--var", "release=1"}); code != 4 || err == nil || !strings.Contains(err.Error(), "--var") {
			t.Fatal(command, code, err)
		}
	}
}

// A shell run is recorded under its own entry point: default logs, status,
// and retry keep selecting the playbook's runs, and its ID selects it.
func TestShellRunsAreSelectedOnlyByID(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("MMX_STATE_DIR", filepath.Join(dir, "state"))
	entry := filepath.Join(dir, "mmx_project_settings.yml")
	if err := os.WriteFile(entry, []byte("project_name: test\ndefault_inventory: example\nrun_history_retention: forever\n"), 0600); err != nil {
		t.Fatal(err)
	}
	state := filepath.Join(dir, "state")
	inventory := "example"
	runs := map[string]*controller.Run{}
	for _, entryPoint := range []string{entry, controller.RunCommandEntryPoint} {
		c := controller.Controller{State: state, Selection: controller.Identity{EntryPoint: entryPoint, Project: "test", Inventory: inventory}}
		r, err := c.New(context.Background(), config.Project{Project: "test", Inventory: "example", HistoryRetention: "forever"}, config.Inventory{}, nil, false)
		if err != nil {
			t.Fatal(err)
		}
		runs[entryPoint] = r
	}
	selected := func(args ...string) string {
		t.Helper()
		stdout, err := os.CreateTemp(dir, "stdout")
		if err != nil {
			t.Fatal(err)
		}
		saved := os.Stdout
		os.Stdout = stdout
		code, err := run(context.Background(), append(args, "--json"))
		os.Stdout = saved
		stdout.Close()
		if code != 0 || err != nil {
			t.Fatal(args, code, err)
		}
		b, _ := os.ReadFile(stdout.Name())
		var view struct{ ID, Status string }
		if err := json.Unmarshal(b, &view); err != nil {
			t.Fatal(err, string(b))
		}
		return view.ID + view.Status
	}
	shell := runs[controller.RunCommandEntryPoint].ID
	if selected("results") != runs[entry].ID+"running" || selected("resume") != runs[entry].ID+"nothing_to_resume" || selected("results", controller.ShortID(shell)) != shell+"running" {
		t.Fatal("a default command selected the shell run, or its ID did not")
	}
}

func TestShellArguments(t *testing.T) {

	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"run-command", "--", "true"}, "usage: mmx run-command"},
		{[]string{"run-command", "--nodes", "all"}, "usage: mmx run-command"},
		{[]string{"run-command", "--nodes", "all", "--limit", "web", "--", "true"}, "does not accept --limit"},
		{[]string{"run-command", "--nodes", "all", "--var", "release=1", "--", "true"}, "does not accept --var"},
		{[]string{"results", "--nodes", "all"}, "only mmx run-command"},
		{[]string{"run", "--run-as-node-user-account", "app"}, "only mmx run-command"},
	} {
		if code, err := run(context.Background(), tc.args); code != 4 || err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatal(tc.args, code, err)
		}
	}
}

func TestCommandsUseCurrentRetentionOrSavedPolicyWithoutProject(t *testing.T) {
	for _, tt := range []struct {
		name, command, saved, current string
		keep                          bool
	}{
		{"current-default", "results", "forever", "", false},
		{"current-forever", "results", "24h", "run_history_retention: forever\n", true},
		{"missing-project", "results", "24h", "missing", false},
		{"nothing-to-retry", "resume", "forever", "", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Chdir(dir)
			t.Setenv("MMX_STATE_DIR", filepath.Join(dir, "state"))
			entry := filepath.Join(dir, "mmx_project_settings.yml")
			c := controller.Controller{State: filepath.Join(dir, "state"), Selection: controller.Identity{EntryPoint: entry, Inventory: "example"}}
			p := config.Project{Project: "test", Inventory: "example", HistoryRetention: tt.saved}
			if tt.current != "missing" {
				if err := os.WriteFile(entry, []byte("project_name: test\ndefault_inventory: example\n"+tt.current), 0600); err != nil {
					t.Fatal(err)
				}
			}
			save := func(status string) *controller.Run {
				r, err := c.New(context.Background(), p, config.Inventory{}, nil, false)
				if err != nil {
					t.Fatal(err)
				}
				old := time.Now().UTC().Add(-48 * time.Hour)
				r.Status, r.Finished = status, &old
				if err := durable.JSON(filepath.Join(c.State, "runs", r.ID, "run.json"), r); err != nil {
					t.Fatal(err)
				}
				return r
			}
			old, latest := save("failed"), save("succeeded")
			code, err := run(context.Background(), []string{tt.command, latest.ID, "--json"})
			if code != 0 || err != nil {
				t.Fatal(code, err)
			}
			_, err = os.Stat(filepath.Join(c.State, "runs", old.ID))
			if tt.keep && err != nil {
				t.Fatal("retained record disappeared", err)
			}
			if !tt.keep && !os.IsNotExist(err) {
				t.Fatal("expired record was not removed", err)
			}
			if _, err := os.Stat(filepath.Join(c.State, "runs", latest.ID)); err != nil {
				t.Fatal("latest normal run disappeared", err)
			}
		})
	}
}

// A project moved with .mmx/ copied: the saved runs name a settings file at
// the old path. When that path exists but cannot be read, history cleanup
// treats it like a missing path and uses the saved policy, without a message.
func TestHistoryCleanupIgnoresAnUnreadableOldSettingsFile(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads every file")
	}
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("MMX_STATE_DIR", filepath.Join(dir, "state"))
	entry := filepath.Join(dir, "mmx_project_settings.yml")
	if err := os.WriteFile(entry, []byte("project_name: test\ndefault_inventory: example\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := controller.Controller{State: filepath.Join(dir, "state"), Selection: controller.Identity{EntryPoint: entry, Inventory: "example"}}
	r, err := c.New(context.Background(), config.Project{Project: "test", Inventory: "example", HistoryRetention: "24h"}, config.Inventory{}, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(entry, 0); err != nil {
		t.Fatal(err)
	}
	stderr, err := os.CreateTemp(dir, "stderr")
	if err != nil {
		t.Fatal(err)
	}
	saved := os.Stderr
	os.Stderr = stderr
	cleanupHistory(c, r)
	os.Stderr = saved
	stderr.Close()
	if b, _ := os.ReadFile(stderr.Name()); len(b) != 0 {
		t.Fatalf("history cleanup printed %q", b)
	}
}
