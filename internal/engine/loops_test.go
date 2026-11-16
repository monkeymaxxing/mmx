package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mmx/internal/config"
	"mmx/internal/facts"
	"mmx/internal/modules"
	"mmx/internal/value"
)

func TestLoopRoleOutputsAndVariableIsolation(t *testing.T) {
	nodeTasks := []config.Task{
		{ID: "each", Loop: []any{"b", "a"}, As: "item", Key: "{{ .item }}", Role: "sample", Inputs: map[string]any{"name": "{{ .item }}"}, RoleInputs: map[string]config.Input{"name": {Type: "string", Required: true}}, RoleNodeTasks: []config.Task{{SetVars: map[string]any{"private": "{{ .inputs.name }}"}}, {ID: "read", Show: "{{ .vars.private }}"}}, RoleOutputs: map[string]any{"name": "{{ .node_tasks.read.value }}"}},
		{ID: "inspect", Show: "{{ .node_tasks.each.items[1].outputs.name }}"},
	}
	j := job(nodeTasks...)
	j.Outputs = map[string]any{"name": "{{ .node_tasks.inspect.value }}"}
	r := (&Engine{}).Run(context.Background(), j)
	if r.Status != "succeeded" || r.Outputs["name"] != "a" {
		t.Fatalf("%+v", r)
	}
	loop := r.Results[len(r.Results)-2]
	if len(loop.Items) != 2 || loop.Items[0].IterationKey != "b" || loop.Items[1].IterationKey != "a" {
		t.Fatalf("%+v", loop)
	}
	for _, task := range r.Results {
		if strings.Contains(task.Task, "each[b]") && strings.Contains(task.Task, "each[a]") {
			t.Fatal("bad loop path")
		}
	}
}

func TestDuplicateLoopKeyRejectsBeforeFirstMutation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "written")
	text := "hello"
	j := job(config.Task{Loop: []any{"a", "a"}, As: "item", Key: "{{ .item }}", Module: "file", ModuleInputs: &modules.File{Mode: "0644", Path: path, Content: &text}})
	r := (&Engine{}).Run(context.Background(), j)
	if r.Status != "failed" || !strings.Contains(r.Error, "repeated") {
		t.Fatal(r)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("mutated before validating keys", err)
	}
}

func TestUnknownOutputsAndSkippedConditions(t *testing.T) {
	for _, dryrun := range []bool{false, true} {
		j := job(config.Task{ID: "producer", When: false, Show: "never"}, config.Task{ID: "disabled", When: false, Show: "{{ .node_tasks.producer.value }}"}, config.Task{ID: "consumer", Show: "{{ .node_tasks.producer.value }}"})
		j.Dryrun = dryrun
		j.Outputs = map[string]any{"value": "{{ .node_tasks.consumer.value }}"}
		r := (&Engine{}).Run(context.Background(), j)
		want := "blocked"
		if dryrun {
			want = "not_checked"
		}
		if r.Status != want || r.Results[1].Status != "skipped" || r.Results[2].Status != want {
			t.Fatalf("dryrun=%v: %+v", dryrun, r)
		}
	}
}

// A stage task that did not run (when false; a dryrun shell) stays unavailable
// after its task block, as it does outside one; dryrun consumers stay not_checked.
func TestSkippedStageResultsStayUnavailableAfterTaskBlock(t *testing.T) {
	for _, dryrun := range []bool{false, true} {
		// A healthy host reports no unavailable facts.
		e := Engine{RuntimeRoot: t.TempDir(), collectFacts: func(name string) facts.Snapshot {
			return facts.Snapshot{Values: map[string]any{"name": name}}
		}}
		op := config.Task{TaskBlock: &config.TaskBlock{ID: "op", Changes: []config.Task{
			{ID: "probe", When: dryrun, Module: "shell", ModuleInputs: &modules.Shell{Script: "exit 7"}},
			{ID: "derived", Show: "{{ .node_tasks.probe.stdout }}"},
		}}}
		if !dryrun {
			op.TaskBlock.Changes = op.TaskBlock.Changes[:1]
		}
		j := job(op, config.Task{ID: "after", Show: "{{ .node_tasks.probe.exit_code }}"})
		j.Dryrun = dryrun
		want := "blocked"
		if dryrun {
			want = "not_checked"
			j.NodeTasks = append(j.NodeTasks, config.Task{Show: "{{ .node_tasks.derived.value }}"})
		}
		r := e.Run(context.Background(), j)
		for _, res := range r.Results {
			if !strings.HasPrefix(res.Task, "op") && res.Status != want {
				t.Errorf("dryrun=%v: %s is %s (%s), want %s", dryrun, res.Task, res.Status, res.Reason, want)
			}
		}
		if !dryrun && r.Status != "blocked" {
			t.Errorf("job %s: %s", r.Status, r.Error)
		}
	}
}

func TestOutputFailureRetainsCompletedChanges(t *testing.T) {
	path := filepath.Join(t.TempDir(), "written")
	content := "new"
	j := job(config.Task{Module: "file", ModuleInputs: &modules.File{Mode: "0644", Path: path, Content: &content}})
	j.Outputs = map[string]any{"bad": "{{ .vars.missing }}"}
	r := (&Engine{}).Run(context.Background(), j)
	if r.Status != "failed" || len(r.Results) != 1 || r.Results[0].Change != "changed" {
		t.Fatal(r)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != "new" {
		t.Fatal(string(got), err)
	}
}

func TestPreparedFileLoopFreezesPathsAndHashesEveryItem(t *testing.T) {
	dir := t.TempDir()
	content := "value={{ .item }}"
	nodeTasks := []config.Task{{ID: "files", Loop: []any{"b", "a"}, As: "item", Module: "file", ModuleInputs: &modules.File{Mode: "0644", Path: dir + "/{{ .item }}", Content: &content}}}
	var files []modules.TrackedInput
	applied := map[string]appliedFile{}
	op := config.Task{TaskBlock: &config.TaskBlock{ID: "files", Changes: nodeTasks, Activate: []config.Task{{Module: "systemd_service", ModuleInputs: &modules.Service{State: "running", Name: "demo"}}}}}
	fixed, err := fixTracked(&op, scope(), func(config.Task, value.Scope) bool { return false })
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range fixed["changes"] {
		files = append(files, config.TrackedInputs(*a)...)
		applied[config.InputsOf[modules.File](*a).Path] = appliedFile{content: config.InputsOf[modules.File](*a), scope: boundScope(scope(), *a).Local()}
	}
	if len(files) != 2 || len(op.TaskBlock.Changes[0].Prepared) != 2 {
		t.Fatal("loop items not fixed", files)
	}
	before, err := configurationHash(context.Background(), files, nil)
	if err != nil {
		t.Fatal(err)
	}
	desired, err := configurationHash(context.Background(), files, applied)
	if err != nil {
		t.Fatal(err)
	}
	r := (&Engine{}).Run(context.Background(), job(nodeTasks...))
	actual, err := configurationHash(context.Background(), files, nil)
	if err != nil || r.Status != "succeeded" || desired != actual || before == actual {
		t.Fatal(r, desired, actual, err)
	}
	if len(r.Results[len(r.Results)-1].Items) != 2 {
		t.Fatal("loop results not grouped")
	}
}

func TestTaskBlockLoopsIsolateAssignments(t *testing.T) {
	dir := t.TempDir()
	content := "{{ .vars.local }}"
	j := job(config.Task{ID: "task_blocks", Loop: []any{"b", "a"}, As: "item", Key: "{{ .item }}", TaskBlock: &config.TaskBlock{ID: "configure", Changes: []config.Task{{SetVars: map[string]any{"local": "{{ .item }}"}}, {Module: "file", ModuleInputs: &modules.File{Mode: "0644", Path: dir + "/{{ .item }}", Content: &content}}}}},
		config.Task{Show: "{{ .vars.local }}"})
	j.Vars = map[string]any{"local": "outside"}
	r := (&Engine{}).Run(context.Background(), j)
	if r.Status != "succeeded" || r.Results[len(r.Results)-1].Value != "outside" {
		t.Fatal(r)
	}
	if read(t, dir+"/a") != "a" || read(t, dir+"/b") != "b" {
		t.Fatal("iteration variables leaked")
	}
}

func TestSetVarsPredicatesResolveOnlyNeededValues(t *testing.T) {
	for _, tc := range []struct {
		name, reference, status string
		first, secret           bool
	}{
		{"unused unavailable", "{{ .node_tasks.skipped.value }}", "succeeded", true, false},
		{"needed unavailable", "{{ .node_tasks.skipped.value }}", "blocked", false, false},
		{"unused secret", "{{ .secrets.flag }}", "succeeded", true, false},
		{"needed secret", "{{ .secrets.flag }}", "succeeded", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			j := job(config.Task{ID: "skipped", When: false, Show: "unused"},
				config.Task{SetVars: map[string]any{"ok": map[string]any{"any": []any{tc.first, tc.reference}}}})
			j.Scope = map[string]any{"secrets": map[string]any{"flag": value.Sensitive(true)}}
			j.Outputs = map[string]any{"ok": "{{ .vars.ok }}"}
			r := (&Engine{}).Run(context.Background(), j)
			if r.Status != tc.status {
				t.Fatalf("status=%s error=%s", r.Status, r.Error)
			}
			if tc.status == "succeeded" && (value.Plain(r.Outputs["ok"]) != true || value.HasSecrets(r.Outputs["ok"]) != tc.secret || r.Results[1].Secret != tc.secret) {
				t.Fatalf("wrong assignment value or sensitivity: %+v", r)
			}
		})
	}
}
