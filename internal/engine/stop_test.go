package engine

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"mmx/internal/config"
	"mmx/internal/modules"
	"mmx/internal/protocol"
)

func TestStopFinishesGroupedChangeInsideRole(t *testing.T) {
	for _, validationFails := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "validation failure"}[validationFails], func(t *testing.T) {
			dir := t.TempDir()
			requested := false
			e := Engine{RuntimeRoot: dir, StopRequested: func() (bool, error) { return requested, nil }}
			e.Report = func(r protocol.Response) error {
				if len(r.Results) > 0 {
					requested = true
				}
				return nil
			}
			op := config.Task{TaskBlock: &config.TaskBlock{ID: "configure", Changes: []config.Task{taskFile(filepath.Join(dir, "config"), "new")},
				Activate:  []config.Task{taskFile(filepath.Join(dir, "activated"), "yes")},
				Verify:    []config.Task{taskFile(filepath.Join(dir, "verified"), "yes")},
				OnFailure: []config.Task{taskFile(filepath.Join(dir, "on_failure"), "yes")},
				Cleanup:   []config.Task{taskFile(filepath.Join(dir, "cleanup"), "yes")}},
			}
			if validationFails {
				op.TaskBlock.Validate = []config.Task{{Module: "shell", ModuleInputs: &modules.Shell{Script: "exit 1"}}}
			}
			j := job(config.Task{Role: "web", RoleNodeTasks: []config.Task{op, taskFile(filepath.Join(dir, "later"), "no")}})
			r := e.Run(context.Background(), j)
			want := "stopped"
			if validationFails {
				want = "failed"
			}
			requireStatus(t, r, want)
			if read(t, filepath.Join(dir, "config")) != "new" || read(t, filepath.Join(dir, "cleanup")) != "yes" {
				t.Fatal("group did not finish")
			}
			if validationFails {
				if read(t, filepath.Join(dir, "on_failure")) != "yes" {
					t.Fatal("failure handler skipped")
				}
				if _, err := os.Stat(filepath.Join(dir, "activated")); !os.IsNotExist(err) {
					t.Fatal("activated invalid config")
				}
			} else {
				if read(t, filepath.Join(dir, "activated")) != "yes" || read(t, filepath.Join(dir, "verified")) != "yes" {
					t.Fatal("activation/verification skipped")
				}
			}
			if _, err := os.Stat(filepath.Join(dir, "later")); !os.IsNotExist(err) {
				t.Fatal("started work after stop")
			}
		})
	}
}

func TestStopBetweenStandaloneNodeTasksAndBeforeFirstTask(t *testing.T) {
	for _, initial := range []bool{false, true} {
		dir := t.TempDir()
		requested := initial
		e := Engine{RuntimeRoot: dir, StopRequested: func() (bool, error) { return requested, nil }}
		e.Report = func(r protocol.Response) error {
			if len(r.Results) > 0 {
				requested = true
			}
			return nil
		}
		r := e.Run(context.Background(), job(taskFile(filepath.Join(dir, "first"), "yes"), taskFile(filepath.Join(dir, "second"), "no")))
		requireStatus(t, r, "stopped")
		if _, err := os.Stat(filepath.Join(dir, "first")); os.IsNotExist(err) != initial {
			t.Fatal("wrong first-task boundary")
		}
		if _, err := os.Stat(filepath.Join(dir, "second")); !os.IsNotExist(err) {
			t.Fatal("second task started")
		}
	}
}
