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
	"mmx/internal/protocol"
	"mmx/internal/value"
)

// A dryrun checks the node as it is now. A check that fails after an earlier
// node task would change the node is not checked, and keeps its output; with
// nothing earlier to change, the failure stands; a run is unchanged.
func TestDryrunCheckAfterAnEarlierChangeIsNotChecked(t *testing.T) {
	dir := t.TempDir()
	earlier := taskFile(filepath.Join(dir, "account"), "web\n") // stands in for a user task
	checked := func() config.Task {
		task := taskFile(filepath.Join(dir, "wordpress-sites"), "su web web\n")
		config.InputsOf[modules.File](task).CheckNewFile = &modules.Check{Shell: &modules.Shell{
			Script: "echo \"unknown user 'web'\" >&2; exit 1",
			Dryrun: "echo \"unknown user 'web'\" >&2; exit 1",
		}}
		return task
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusServiceUnavailable) }))
	defer server.Close()
	request := config.Task{Module: "http", ModuleInputs: &modules.HTTP{URL: server.URL}}
	block := func(changes ...config.Task) config.Task {
		return config.Task{TaskBlock: &config.TaskBlock{ID: "nginx", Changes: changes,
			Validate: []config.Task{{Module: "shell", ModuleInputs: &modules.Shell{Script: "echo 'nginx: command not found' >&2; exit 1", Dryrun: "echo 'nginx: command not found' >&2; exit 1"}}}}}
	}
	run := func(t *testing.T, dryrun bool, tasks ...config.Task) protocol.Response {
		t.Helper()
		os.Remove(filepath.Join(dir, "account"))
		j := job(tasks...)
		j.Dryrun = dryrun
		return (&Engine{RuntimeRoot: filepath.Join(dir, "state")}).Run(context.Background(), j)
	}
	t.Run("check_new_file", func(t *testing.T) {
		r := run(t, true, earlier, checked())
		requireStatus(t, r, "succeeded")
		got := r.Results[1]
		if got.Status != "not_checked" || got.FailureKind != "" || !strings.Contains(got.Reason, "an earlier node task, task_1, would change the node before the run reaches the check") {
			t.Fatalf("%+v", got)
		}
		if got.Change != "would_change" || got.CheckNewFile == nil || !strings.Contains(got.CheckNewFile.Stderr, "unknown user 'web'") || !strings.Contains(got.Reason, "check_new_file failed") {
			t.Fatalf("the check's output is lost: %+v", got)
		}
		// Nothing earlier would change the node: the failure stands.
		r = run(t, true, checked())
		requireStatus(t, r, "failed")
		if got := r.Results[0]; got.Status != "failed" || got.FailureKind != "validation" {
			t.Fatalf("%+v", got)
		}
		// The run checks after the earlier change, and fails as before.
		r = run(t, false, earlier, checked())
		requireStatus(t, r, "failed")
		if got := r.Results[1]; got.Status != "failed" || got.FailureKind != "validation" {
			t.Fatalf("%+v", got)
		}
	})
	t.Run("validate", func(t *testing.T) {
		r := run(t, true, block(earlier))
		requireStatus(t, r, "succeeded")
		got := r.Results[1]
		if !strings.Contains(got.Task, "/validate/") || got.Status != "not_checked" || !strings.Contains(got.Reason, "would change the node before the run reaches the check") || !strings.Contains(got.Stderr, "nginx: command not found") {
			t.Fatalf("%+v", r.Results)
		}
		put(t, filepath.Join(dir, "account"), "web\n")
		os.Chmod(filepath.Join(dir, "account"), 0644)
		j := job(block(earlier))
		j.Dryrun = true
		r = (&Engine{RuntimeRoot: filepath.Join(dir, "state")}).Run(context.Background(), j)
		requireStatus(t, r, "failed")
		r = run(t, false, block(earlier))
		requireStatus(t, r, "failed")
	})
	t.Run("read-only http", func(t *testing.T) {
		r := run(t, true, earlier, request)
		requireStatus(t, r, "succeeded")
		if got := r.Results[1]; got.Status != "not_checked" || got.HTTPStatusCode != http.StatusServiceUnavailable || !strings.Contains(got.Reason, "would change the node before the run reaches the check") {
			t.Fatalf("%+v", got)
		}
		r = run(t, true, request)
		requireStatus(t, r, "failed")
	})
	t.Run("assert", func(t *testing.T) {
		// An assert judges values, such as an approval; it is no check of the node.
		r := run(t, true, earlier, config.Task{Module: "assert", ModuleInputs: &modules.Assert{That: false, Message: "not approved"}})
		requireStatus(t, r, "failed")
	})
}

// A missing parent directory fails a file task once, with the reason, and is
// not tried again: another try cannot create it.
func TestMissingParentDirectoryIsTriedOnce(t *testing.T) {
	dir := t.TempDir()
	parent := filepath.Join(dir, "backups")
	r := (&Engine{RuntimeRoot: filepath.Join(dir, "state")}).Run(context.Background(), job(taskFile(filepath.Join(parent, "ledger"), "x")))
	requireStatus(t, r, "failed")
	got := r.Results[0]
	if got.FailureKind != "missing_prerequisite" || len(got.Tries) > 1 || got.Reason != "the parent directory "+parent+" does not exist; create it with an earlier directory node task" {
		t.Fatalf("%+v", got)
	}
}

// A dryrun cannot know that a package creates the directory a later file
// goes in. After an earlier node task that would change the node, the file
// task's reason names that node task after what is missing, so an agent does not add a
// directory task for a directory the package provides. With nothing earlier
// to change, the module's own advice stands alone.
func TestDryrunMissingParentNamesTheEarlierChange(t *testing.T) {
	dir := t.TempDir()
	parent := filepath.Join(dir, "nginx", "conf.d")
	site := taskFile(filepath.Join(parent, "shop.conf"), "server {}\n")
	earlier := taskFile(filepath.Join(dir, "elsewhere"), "x")
	earlier.ID = "install_nginx"
	missing := "the parent directory " + parent + " does not exist; create it with an earlier directory node task"
	dryrun := func(tasks ...config.Task) protocol.Response {
		j := job(tasks...)
		j.Dryrun = true
		return (&Engine{RuntimeRoot: filepath.Join(dir, "state")}).Run(context.Background(), j)
	}
	r := dryrun(earlier, site)
	requireStatus(t, r, "succeeded")
	want := missing + ". The earlier node task install_nginx would change this node first and may provide what is missing; if the run then succeeds, change nothing."
	if got := r.Results[1]; got.Status != "not_checked" || got.Change != "unknown" || got.Reason != want {
		t.Fatalf("%+v", got)
	}
	r = dryrun(site)
	if got := r.Results[0]; got.Status != "not_checked" || got.Reason != missing {
		t.Fatalf("%+v", got)
	}
}

// A file task without content changes only an existing file's metadata. A
// missing file is a missing prerequisite, since an earlier node task, such as
// a package, may create it: a run fails once with the reason, and a dryrun
// leaves the task not checked.
func TestMetadataOfAMissingFileIsAMissingPrerequisite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "myapp.conf")
	permissions := config.Task{Module: "file", ModuleInputs: &modules.File{Path: path, Mode: "0640"},
		Retry: &config.Retry{MaxTries: config.TryLimit(3), PauseBetweenTries: "0s"}}
	missing := "cannot change metadata of missing file " + path
	r := (&Engine{RuntimeRoot: filepath.Join(dir, "state")}).Run(context.Background(), job(permissions))
	requireStatus(t, r, "failed")
	if got := r.Results[0]; got.FailureKind != "missing_prerequisite" || len(got.Tries) > 1 || got.Reason != missing {
		t.Fatalf("%+v", got)
	}
	j := job(permissions)
	j.Dryrun = true
	r = (&Engine{RuntimeRoot: filepath.Join(dir, "state")}).Run(context.Background(), j)
	requireStatus(t, r, "succeeded")
	if got := r.Results[0]; got.Status != "not_checked" || got.Change != "unknown" || got.Reason != missing {
		t.Fatalf("%+v", got)
	}
}

// A validate check without a dryrun command reports no change while nothing
// would change, so such a dryrun ends without proposed or unknown changes.
func TestDryrunValidateWithoutDryrunCommandReportsNoChangeWhenNothingChanges(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nginx.conf")
	put(t, path, "live")
	os.Chmod(path, 0644)
	block := func(content string) config.Task {
		return config.Task{TaskBlock: &config.TaskBlock{ID: "nginx", Changes: []config.Task{taskFile(path, content)},
			Validate: []config.Task{{Module: "shell", ModuleInputs: &modules.Shell{Script: "true"}}}}}
	}
	j := job(block("live"))
	j.Dryrun = true
	r := (&Engine{RuntimeRoot: filepath.Join(dir, "state")}).Run(context.Background(), j)
	requireStatus(t, r, "succeeded")
	if r.Change != "unchanged" || r.Results[1].Status != "skipped" || r.Results[1].Change != "unchanged" {
		t.Fatalf("%s %+v", r.Change, r.Results)
	}
	j = job(block("proposed"))
	j.Dryrun = true
	r = (&Engine{RuntimeRoot: filepath.Join(dir, "state")}).Run(context.Background(), j)
	requireStatus(t, r, "succeeded")
	if r.Change != "would_change" || r.Results[1].Change != "unknown" {
		t.Fatalf("%s %+v", r.Change, r.Results)
	}
}

// A validate check whose when is false is skipped and does not fail the
// node task block, in a run as in a dryrun: a role checks TLS only when TLS
// is on.
func TestSkippedValidateCheckDoesNotFailTheBlock(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nginx.conf")
	block := config.Task{TaskBlock: &config.TaskBlock{ID: "nginx", Changes: []config.Task{taskFile(path, "proposed")},
		Validate: []config.Task{{When: false, Module: "shell", ModuleInputs: &modules.Shell{Script: "exit 1"}}}}}
	for _, dryrun := range []bool{true, false} {
		j := job(block)
		j.Dryrun = dryrun
		r := (&Engine{RuntimeRoot: filepath.Join(dir, "state")}).Run(context.Background(), j)
		requireStatus(t, r, "succeeded")
		if r.Results[1].Status != "skipped" {
			t.Fatalf("dryrun %v: %+v", dryrun, r.Results)
		}
	}
}

// A run_from_directory that does not exist is a missing prerequisite, as in
// the OpenWISP deployment: the shell task that loads the initial data runs
// from the installation directory, which an earlier directory node task
// creates. A run fails once with MMX's own reason, which a task that reads a
// secret shows. A dryrun leaves the task not checked after an earlier node
// task that would create the directory, and with nothing earlier the
// missing prerequisite stands.
func TestMissingRunFromDirectoryIsAMissingPrerequisite(t *testing.T) {
	dir := t.TempDir()
	installation := filepath.Join(dir, "openwisp2")
	loadInitialData := func() config.Task {
		return config.Task{Module: "shell", ModuleInputs: &modules.Shell{
			Script:           "env/bin/python load_initial_data.py",
			Dryrun:           "env/bin/python load_initial_data.py --check",
			Cwd:              installation,
			OnlyIfNot:        &modules.Guard{Script: "test -x env/bin/python && env/bin/python load_initial_data.py --check"},
			Env:              map[string]any{"ADMINISTRATOR_PASSWORD": "{{ .secrets.administrator_password }}"},
			SuccessExitCodes: []int{0},
		}}
	}
	createInstallation := config.Task{Module: "directory", ModuleInputs: &modules.Directory{Path: installation, Mode: "0755"}}
	run := func(t *testing.T, dryrun bool, tasks ...config.Task) protocol.Response {
		t.Helper()
		j := job(tasks...)
		j.Dryrun = dryrun
		j.Scope = map[string]any{"secrets": map[string]any{"administrator_password": value.Sensitive("openwisp-secret")}}
		return (&Engine{RuntimeRoot: filepath.Join(dir, "state")}).Run(context.Background(), j)
	}
	missing := "run_from_directory " + installation + " does not exist; create it with an earlier directory node task"
	t.Run("run", func(t *testing.T) {
		task := loadInitialData()
		task.Retry = &config.Retry{MaxTries: config.TryLimit(3), PauseBetweenTries: "0s"}
		r := run(t, false, task)
		requireStatus(t, r, "failed")
		got := r.Results[0]
		if got.FailureKind != "missing_prerequisite" || len(got.Tries) > 1 || got.Reason != missing || !got.Secret {
			t.Fatalf("%+v", got)
		}
	})
	t.Run("dryrun after an earlier directory node task", func(t *testing.T) {
		r := run(t, true, createInstallation, loadInitialData())
		requireStatus(t, r, "succeeded")
		if got := r.Results[1]; got.Status != "not_checked" {
			t.Fatalf("%+v", got)
		}
	})
	t.Run("dryrun with nothing earlier", func(t *testing.T) {
		r := run(t, true, loadInitialData())
		requireStatus(t, r, "failed")
		if got := r.Results[0]; got.Status != "failed" || got.FailureKind != "missing_prerequisite" || got.Reason != missing {
			t.Fatalf("%+v", got)
		}
	})
}

// A dryrun predicts a change, and only the run knows the change that a node
// task reports. A condition on that change is not checked in a dryrun; the
// run decides it.
func TestDryrunConditionOnAPredictedChangeIsNotChecked(t *testing.T) {
	dir := t.TempDir()
	changing := taskFile(filepath.Join(dir, "app.yml"), "new\n")
	changing.ID = "app_config"
	guarded := config.Task{When: map[string]any{"equals": []any{"{{ .node_tasks.app_config.change }}", "changed"}}, Show: "restarted"}
	for _, dryrun := range []bool{true, false} {
		j := job(changing, guarded)
		j.Dryrun = dryrun
		r := (&Engine{RuntimeRoot: filepath.Join(dir, "state")}).Run(context.Background(), j)
		got := r.Results[1]
		if dryrun && (got.Status != "not_checked" || !strings.Contains(got.Reason, "app_config")) {
			t.Errorf("dryrun: %+v", got)
		}
		if !dryrun && (got.Status != "succeeded" || got.Value != "restarted") {
			t.Errorf("run: %+v", got)
		}
	}
}

// A dryrun cannot know an output that a module task did not report: the
// module was not checked, or a dryrun cannot know the output yet, such as
// the state of a node it would create. A node task that reads such an
// output is not checked, and so is one that reads a variable set from it.
// The run reads the output that the module reports.
func TestDryrunOfAnOutputThatAModuleDidNotReportIsNotChecked(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, modules.ModuleScriptFile), []byte(`def run_node_task(inputs):
    if not inspect_path(inputs["program"]).exists:
        return {"status": "missing_prerequisite", "reason": "the program " + inputs["program"] + " is not installed"}
    if is_dryrun():
        return {"reason": "would join the formation", "outputs": {"created": True}}
    return {"outputs": {"created": True, "is_primary": True}}
`), 0600); err != nil {
		t.Fatal(err)
	}
	description, err := modules.ParseModuleDescription([]byte("summary: x\ninputs:\n  program: {type: string, required: true, description: d}\n"+
		"outputs:\n  created: {type: boolean, description: d}\n  is_primary: {type: boolean, description: d}\n"+
		"examples: [{description: x, config: 'member: {program: /usr/bin/x}'}]\n"), "member")
	if err != nil {
		t.Fatal(err)
	}
	module := modules.Module{Kind: modules.KindProjectStarlark, Directory: dir, Description: description,
		Files: []modules.ModuleFile{{Path: modules.ModuleScriptFile, Mode: 0600}, {Path: modules.ModuleDescriptionFile, Mode: 0600}}}
	program := filepath.Join(t.TempDir(), "program")
	tasks := []config.Task{
		{ID: "member", Module: "member", ModuleInputs: map[string]any{"program": program}},
		{ID: "name_the_primary", SetVars: map[string]any{"is_the_primary": "{{ .node_tasks.member.is_primary }}"}},
		{ID: "only_on_the_primary", When: "{{ .vars.is_the_primary }}", Show: "primary"},
	}
	run := func(dryrun bool, nodeTasks ...config.Task) []protocol.Result {
		j := job(nodeTasks...)
		j.Modules, j.Dryrun = map[string]modules.Module{"member": module}, dryrun
		return (&Engine{RuntimeRoot: filepath.Join(dir, "state")}).Run(context.Background(), j).Results
	}
	notChecked := func(r protocol.Result, names ...string) bool {
		for _, name := range names {
			if !strings.Contains(r.Reason, name) {
				return false
			}
		}
		return r.Status == "not_checked"
	}

	// An earlier node task would install the program, so the module is not checked.
	got := run(true, append([]config.Task{taskFile(program, "x\n")}, tasks...)...)
	if got[0].Change != "would_change" || !notChecked(got[1], "is not installed") || !notChecked(got[2], "member is not_checked: "+modules.EarlierChangeMayProvide("task_1", "the program "+program+" is not installed")) ||
		!notChecked(got[3], "name_the_primary is not_checked: ", "member is not_checked") {
		t.Fatalf("a module that is not checked: %+v", got)
	}

	// The program is there, and the dryrun leaves out is_primary.
	if err := os.WriteFile(program, []byte("x\n"), 0600); err != nil {
		t.Fatal(err)
	}
	got = run(true, tasks...)
	if got[0].Status != "succeeded" || !notChecked(got[1], "member reports no is_primary in this dryrun") || !notChecked(got[2], "name_the_primary is not_checked: ") {
		t.Fatalf("an output the dryrun leaves out: %+v", got)
	}
	got = run(false, tasks...)
	if got[2].Status != "succeeded" || got[2].Value != "primary" {
		t.Fatalf("run: %+v", got)
	}
}
