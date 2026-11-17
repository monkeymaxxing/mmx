package engine

import (
	"context"
	"encoding/json"
	"mmx/internal/facts"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mmx/internal/config"
	"mmx/internal/modules"
	"mmx/internal/protocol"
	"mmx/internal/value"
)

func text(s string) *string { return &s }
func taskFile(path, contents string) config.Task {
	return config.Task{Module: "file", ModuleInputs: &modules.File{Path: path, Content: text(contents), Mode: "0644"}}
}
func job(nodeTasks ...config.Task) config.Job {
	return config.Job{ID: "test", Project: "test", FleetStage: "setup", Node: "test", NodeTasks: nodeTasks}
}
func scope() value.Scope {
	return value.Scope{"vars": map[string]any{}, "node_tasks": map[string]any{}}
}
func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
func put(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
}
func requireStatus(t *testing.T, r protocol.Response, status string) {
	t.Helper()
	if r.Status != status {
		t.Fatalf("status %s: %s; %+v", r.Status, r.Error, r.Results)
	}
}

func TestFileDryrunNoopAndCandidateRejection(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "managed")
	e := Engine{RuntimeRoot: filepath.Join(dir, "state")}
	j := job(taskFile(path, "hello\n"))
	j.Dryrun = true
	r := e.Run(context.Background(), j)
	requireStatus(t, r, "succeeded")
	if r.Results[0].Change != "would_change" {
		t.Fatal(r)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("dryrun wrote managed file")
	}
	j.Dryrun = false
	r = e.Run(context.Background(), j)
	requireStatus(t, r, "succeeded")
	if read(t, path) != "hello\n" {
		t.Fatal("wrong content")
	}
	r = e.Run(context.Background(), j)
	if r.Results[0].Change != "unchanged" {
		t.Fatal(r)
	}
	config.InputsOf[modules.File](j.NodeTasks[0]).Content = text("bad")
	config.InputsOf[modules.File](j.NodeTasks[0]).Mode = "0600"
	config.InputsOf[modules.File](j.NodeTasks[0]).CheckNewFile = &modules.Check{Shell: &modules.Shell{Script: "exit 1"}}
	r = e.Run(context.Background(), j)
	requireStatus(t, r, "failed")
	if read(t, path) != "hello\n" {
		t.Fatal("rejected candidate replaced live file")
	}
	st, _ := os.Stat(path)
	if st.Mode().Perm() != 0644 {
		t.Fatal("rejected candidate changed metadata")
	}
	os.Remove(path)
	os.Symlink(filepath.Join(dir, "target"), path)
	r = e.Run(context.Background(), j)
	requireStatus(t, r, "failed")
	if !strings.Contains(r.Error, "is a symbolic link, and file manages only regular files") {
		t.Fatal(r.Error)
	}
}

// A long poll and many capped outputs keep the status answer within the job's
// result budget. Omitted records keep their outcome and say what they omit;
// later node tasks and fleet stage outputs still evaluate the actual captured text.
func TestResultBudgetBoundsTheStatusResponse(t *testing.T) {
	count := 50
	counter := filepath.Join(t.TempDir(), "count")
	zeros := config.Task{Module: "shell", ModuleInputs: &modules.Shell{Script: "head -c 1048576 /dev/zero"}}
	nodeTasks := []config.Task{{Module: "shell", ModuleInputs: &modules.Shell{Script: `n=$(( $(cat "$F" 2>/dev/null || echo 0) + 1 )); echo $n > "$F"; head -c 1048576 /dev/zero; echo "poll $n" >&2`, Env: map[string]any{"F": counter}},
		Retry: &config.Retry{MaxTries: config.TryLimit(count), PauseBetweenTries: "0s", Until: map[string]any{"contains": []any{"{{ .this_node_task.stderr }}", "poll 20"}}}}}
	for range 4 {
		nodeTasks = append(nodeTasks, zeros)
	}
	ended := map[string]any{"contains": []any{"{{ .node_tasks.big.stdout }}", "END"}}
	nodeTasks = append(nodeTasks, config.Task{ID: "big", Module: "shell", ModuleInputs: &modules.Shell{Script: "head -c 1000000 /dev/zero; printf END"}},
		config.Task{ID: "after", When: ended, Module: "shell", ModuleInputs: &modules.Shell{Script: "true"}})
	j := job(nodeTasks...)
	j.Outputs = map[string]any{"ended": ended}
	r := (&Engine{}).Run(context.Background(), j)
	requireStatus(t, r, "succeeded")
	b, err := json.Marshal(r)
	if err != nil || len(b) > resultBudget+2<<20 {
		t.Fatalf("status response is %d bytes: %v", len(b), err)
	}
	full := 0
	for _, x := range r.Results {
		if x.Status != "succeeded" || x.Change != "changed" || x.Task == "" {
			t.Fatalf("record lost its outcome: %+v", x)
		}
		if x.Omitted == "" {
			full++
		} else if x.Stdout != "" || len(x.Tries) != 0 || !strings.Contains(x.Omitted, "budget") {
			t.Fatalf("omitted record kept text: %+v", x)
		}
	}
	poll, big, after := r.Results[0], r.Results[len(r.Results)-2], r.Results[len(r.Results)-1]
	if full < 2 || poll.Omitted == "" || poll.TriesOmitted != 20 || big.Task != "big" || big.Omitted == "" || after.Task != "after" || r.Outputs["ended"] != true || r.Change != "changed" {
		t.Fatalf("full %d, poll %+v, big %+v, after %+v, outputs %v", full, poll.Omitted, big.Omitted, after, r.Outputs)
	}
}

func TestSetVarsIsAtomicAndRoleHasExplicitInputs(t *testing.T) {
	e := Engine{}
	s := scope()
	s["vars"].(map[string]any)["old"] = "value"
	err := e.nodeTasks(context.Background(), []config.Task{{SetVars: map[string]any{"old": "changed", "other": "{{ .vars.missing }}"}}}, s, "")
	if err == nil || s["vars"].(map[string]any)["old"] != "value" {
		t.Fatal("partial assignment")
	}
	child, err := config.ResolveRoleInputs(config.Task{RoleInputs: map[string]config.Input{"value": {Type: "string", Required: true}}, Inputs: map[string]any{"value": "{{ .vars.old }}"}}, s, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := child.Get(".vars.old"); err == nil {
		t.Fatal("role leaked caller vars")
	}
	if v, _ := child.Get(".inputs.value"); v != "value" {
		t.Fatal(v)
	}
}

// set_vars evaluates the predicate forms of when to booleans, sensitive when
// they read a secret; any other value, including a mapping, resolves as data.
func TestSetVarsPredicatesAssignBooleans(t *testing.T) {
	j := job(
		config.Task{ID: "app_config", Show: "x"},
		config.Task{SetVars: map[string]any{
			"update_needed": map[string]any{"any": []any{
				map[string]any{"equals": []any{"{{ .node_tasks.app_config.change }}", "changed"}},
				map[string]any{"matches": []any{"{{ .node_tasks.app_config.status }}", "^succ"}}}},
			"quiet":    map[string]any{"not": map[string]any{"contains": []any{"{{ .node_tasks.app_config.value }}", "x"}}},
			"password": map[string]any{"equals": []any{"{{ .secrets.password }}", "sentinel"}},
			"settings": map[string]any{"mode": "safe", "any": "data"},
		}},
		config.Task{ID: "flag", Show: "{{ .vars.update_needed }}"},
		config.Task{ID: "gated", When: "{{ .vars.update_needed }}", Show: "ran"},
		config.Task{ID: "quiet", Show: "{{ .vars.quiet }}"},
		config.Task{ID: "private", Show: "{{ .vars.password }}"},
		config.Task{ID: "data", Show: "{{ .vars.settings.mode }}"},
	)
	j.Scope = map[string]any{"secrets": map[string]any{"password": &value.Secret{Name: "password", Data: "sentinel", Available: true}}}
	r := (&Engine{}).Run(context.Background(), j)
	requireStatus(t, r, "succeeded")
	byTask := map[string]protocol.Result{}
	for _, x := range r.Results {
		byTask[x.Task] = x
	}
	if byTask["flag"].Value != true || byTask["gated"].Status != "succeeded" || byTask["quiet"].Value != false || byTask["data"].Value != "safe" {
		t.Fatalf("%+v", r.Results)
	}
	if !byTask["private"].Secret || !byTask["task_2"].Secret {
		t.Fatalf("a boolean computed from a secret lost its sensitivity: %+v", r.Results)
	}
}

// assert succeeds unchanged, or fails with its resolved message, or the
// condition as written, before later node tasks run. A value not known yet blocks
// it, which dryrun reports as not checked. A condition that reads a secret
// makes its result secret, and a message that reads none still shows.
func TestAssertPassesOrFailsWithItsMessage(t *testing.T) {
	version := config.Task{ID: "version", Module: "shell", ModuleInputs: &modules.Shell{Script: "echo 1.2.3"}}
	that := func(want string) map[string]any {
		return map[string]any{"contains": []any{"{{ .node_tasks.version.stdout }}", want}}
	}
	later := filepath.Join(t.TempDir(), "later")
	run := func(j config.Job) protocol.Response { return (&Engine{}).Run(context.Background(), j) }

	r := run(job(version, config.Task{Module: "assert", ModuleInputs: &modules.Assert{That: that("1.2")}}, taskFile(later, "x")))
	requireStatus(t, r, "succeeded")
	if a := r.Results[1]; a.Status != "succeeded" || a.Change != "unchanged" || read(t, later) != "x" {
		t.Fatalf("%+v", a)
	}
	os.Remove(later)

	failing := config.Task{Module: "assert", ModuleInputs: &modules.Assert{That: that("2.0"), Message: "version {{ .node_tasks.version.stdout }} is not 2.0"}}
	r = run(job(version, failing, taskFile(later, "x")))
	requireStatus(t, r, "failed")
	if a := r.Results[1]; len(r.Results) != 2 || a.Status != "failed" || a.Change != "unchanged" || a.Reason != "version 1.2.3\n is not 2.0" || !strings.Contains(r.Error, "is not 2.0") {
		t.Fatalf("%s %+v", r.Error, r.Results)
	}
	if _, err := os.Stat(later); !os.IsNotExist(err) {
		t.Fatal("a task after a failed assert ran")
	}
	config.InputsOf[modules.Assert](failing).Message = ""
	r = run(job(version, failing))
	if a := r.Results[1]; a.Reason != `assert.that is false: {"contains":["{{ .node_tasks.version.stdout }}","2.0"]}` {
		t.Fatal(a.Reason)
	}

	// Dryrun skips the shell without a dryrun command, so its output is unknown.
	j := job(version, failing)
	j.Dryrun = true
	if r = run(j); r.Results[1].Status != "not_checked" {
		t.Fatalf("%s %+v", r.Status, r.Results)
	}

	j = job(config.Task{Module: "assert", ModuleInputs: &modules.Assert{That: map[string]any{"equals": []any{"{{ .secrets.token }}", "other"}}, Message: "token mismatch"}})
	j.Scope = map[string]any{"secrets": map[string]any{"token": value.Sensitive("assert-secret-value")}}
	r = run(j)
	b, _ := json.Marshal(r)
	if r.Status != "failed" || !r.Results[0].Secret || r.Results[0].Reason != "token mismatch" || strings.Contains(string(b), "assert-secret-value") {
		t.Fatalf("%s", b)
	}
}

// A reboot the guards allow ends the job rebooting with the boot it leaves;
// the worker reboots afterwards. A guard that disallows it skips it, dryrun
// previews it, a stop keeps it from running, and a fleet stage whose outputs fail
// does not reboot.
func TestRebootEndsTheJobRebooting(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "patched")
	patch := config.Task{ID: "patch", Module: "shell", ModuleInputs: &modules.Shell{Script: "touch " + marker}}
	reboot := func(guard string) config.Task {
		return config.Task{ID: "restart", Module: "reboot", ModuleInputs: &modules.Reboot{OnlyIf: &modules.Guard{Script: guard}}}
	}
	run := func(e *Engine, j config.Job) protocol.Response {
		e.Machine.BootID = func() (string, error) { return "boot-1", nil }
		return e.Run(context.Background(), j)
	}
	r := run(&Engine{}, job(patch, reboot("test -f "+marker)))
	last := r.Results[len(r.Results)-1]
	if r.Status != "rebooting" || r.RebootFrom != "boot-1" || r.Change != "changed" || last.Task != "restart" || last.Status != "rebooting" ||
		last.Change != "changed" || last.Value.(map[string]any)["boot_id_before"] != "boot-1" {
		t.Fatalf("%+v", r)
	}

	r = run(&Engine{}, job(reboot("test -f "+marker+".missing")))
	if r.Status != "succeeded" || r.RebootFrom != "" || r.Results[0].Status != "skipped" || r.Results[0].Reason != "node task skipped by a command condition" {
		t.Fatalf("%+v", r)
	}
	r = run(&Engine{}, job(config.Task{Module: "reboot", ModuleInputs: &modules.Reboot{OnlyIf: &modules.Guard{Script: "exit 7"}}}))
	if r.Status != "failed" || r.Results[0].Reason != "node task command condition exited with code 7, which true_exit_codes and false_exit_codes do not accept" {
		t.Fatalf("%+v", r)
	}

	j := job(reboot("true"))
	j.Dryrun = true
	r = run(&Engine{}, j)
	if r.Status != "succeeded" || r.RebootFrom != "" || r.Results[0].Change != "would_change" {
		t.Fatalf("%+v", r)
	}

	requested := false
	e := &Engine{StopRequested: func() (bool, error) { return requested, nil }}
	e.Report = func(r protocol.Response) error {
		requested = len(r.Results) > 0
		return nil
	}
	r = run(e, job(patch, reboot("true")))
	if r.Status != "stopped" || r.RebootFrom != "" || r.Results[1].Status != "stopped" {
		t.Fatalf("%+v", r)
	}

	j = job(patch, reboot("true"))
	j.Outputs = map[string]any{"missing": "{{ .node_tasks.nope.value }}"}
	r = run(&Engine{}, j)
	if last := r.Results[1]; r.Status != "failed" || r.RebootFrom != "" || last.Status != "skipped" || !strings.HasPrefix(last.Reason, "not rebooted: ") || last.Value != nil {
		t.Fatalf("%+v", r)
	}
}

// Failure handlers read what a failed command, request, or module captured.
// Work that never ran, and a failed task's value, stay unavailable.
// path_exists is answered when the node task is reached. In dryrun, a path an
// earlier node task would create cannot be checked yet, and the result says so.
func TestPathExistsConditionInspectsTheNodeWhenTheTaskIsReached(t *testing.T) {
	dir := t.TempDir()
	marker, managed := filepath.Join(dir, "installed"), filepath.Join(dir, "app", "config")
	guarded := func(path string, negate bool) config.Task {
		var when any = map[string]any{"path_exists": path}
		if negate {
			when = map[string]any{"not": when}
		}
		return config.Task{ID: "guarded", When: when, Show: "ran"}
	}
	last := func(j config.Job) protocol.Result {
		r := (&Engine{RuntimeRoot: filepath.Join(dir, "state")}).Run(context.Background(), j)
		return r.Results[len(r.Results)-1]
	}
	if got := last(job(guarded(marker, false))); got.Status != "skipped" {
		t.Fatalf("a missing path counted as present: %+v", got)
	}
	// The earlier task creates the path, and the condition sees it.
	create := config.Task{Module: "shell", ModuleInputs: &modules.Shell{Script: "touch " + marker}}
	if got := last(job(create, guarded(marker, false))); got.Status != "succeeded" || got.Value != "ran" {
		t.Fatalf("%+v", got)
	}
	// A link that points nowhere still exists as a file.
	link := filepath.Join(dir, "link")
	os.Symlink(filepath.Join(dir, "nowhere"), link)
	if got := last(job(guarded(link, false))); got.Status != "succeeded" {
		t.Fatalf("%+v", got)
	}
	// A path below a regular file does not exist.
	if got := last(job(guarded(marker+"/below", true))); got.Status != "succeeded" {
		t.Fatalf("%+v", got)
	}

	preview := job(config.Task{Module: "directory", ModuleInputs: &modules.Directory{Path: filepath.Dir(managed)}}, guarded(managed, false))
	preview.Dryrun = true
	if got := last(preview); got.Status != "not_checked" || !strings.Contains(got.Reason, "not yet checkable") || !strings.Contains(got.Reason, filepath.Dir(managed)) {
		t.Fatalf("dryrun guessed about a path an earlier task would create: %+v", got)
	}
	// A path no earlier task touches is checked in dryrun as in a run.
	preview = job(config.Task{Module: "directory", ModuleInputs: &modules.Directory{Path: filepath.Dir(managed)}}, guarded(filepath.Join(dir, "elsewhere"), true))
	preview.Dryrun = true
	if got := last(preview); got.Status != "succeeded" {
		t.Fatalf("%+v", got)
	}
}

// A command that prints JSON is read by field; other output says it is not JSON.
func TestCommandOutputIsReadAsJSONByField(t *testing.T) {
	status := config.Task{ID: "balancer", Module: "shell", ModuleInputs: &modules.Shell{Script: `echo '{"active": true, "mode": "upmap"}'`, ChangedWhen: false}}
	r := (&Engine{}).Run(context.Background(), job(status,
		config.Task{When: "{{ .node_tasks.balancer.stdout_json.active }}", Show: "{{ .node_tasks.balancer.stdout_json.mode }}"}))
	requireStatus(t, r, "succeeded")
	if r.Results[1].Value != "upmap" {
		t.Fatalf("%+v", r.Results[1])
	}
	config.InputsOf[modules.Shell](status).Script = "echo HEALTH_OK"
	r = (&Engine{}).Run(context.Background(), job(status, config.Task{When: "{{ .node_tasks.balancer.stdout_json.active }}", Show: "unreachable"}))
	if r.Status != "failed" || !strings.Contains(r.Error, "stdout is not valid JSON") {
		t.Fatalf("%+v", r)
	}
}

func TestFailureHandlersReadCapturedOutput(t *testing.T) {
	failing := config.Task{ID: "migrate", Module: "shell", ModuleInputs: &modules.Shell{Script: "echo partial; echo migration-broke >&2; exit 3"}}
	report := func(field string) config.Task {
		return config.Task{ID: "report", Show: "{{ .node_tasks.migrate." + field + " }}"}
	}
	taskBlock := func(change config.Task, handler config.Task) protocol.Response {
		return (&Engine{}).Run(context.Background(), job(config.Task{TaskBlock: &config.TaskBlock{ID: "app", Changes: []config.Task{change}, OnFailure: []config.Task{handler}}}))
	}
	handler := func(r protocol.Response) protocol.Result {
		for _, x := range r.Results {
			if x.Task == "app/on_failure/report" {
				return x
			}
		}
		t.Fatalf("handler did not run: %+v", r.Results)
		return protocol.Result{}
	}
	for field, want := range map[string]any{"exit_code": 3, "stdout": "partial\n", "stderr": "migration-broke\n"} {
		if got := handler(taskBlock(failing, report(field))); got.Status != "succeeded" || got.Value != want {
			t.Errorf("%s: %+v", field, got)
		}
	}
	if got := handler(taskBlock(failing, report("value"))); got.Status != "blocked" || !strings.Contains(got.Reason, "migrate is failed") {
		t.Errorf("a failed task's value was readable: %+v", got)
	}
	// An argument error fails the task before its command runs.
	unrun := config.Task{ID: "migrate", Module: "shell", ModuleInputs: &modules.Shell{Script: "true", Cwd: "{{ .vars.missing }}"}}
	if got := handler(taskBlock(unrun, report("exit_code"))); got.Status != "blocked" {
		t.Errorf("a command that never ran supplied an exit code: %+v", got)
	}
	// A truncated stream remains truncated for predicates.
	noisy := config.Task{ID: "migrate", Module: "shell", ModuleInputs: &modules.Shell{Script: "head -c 1100000 /dev/zero | tr '\\0' x; exit 3"}}
	guarded := config.Task{ID: "report", When: map[string]any{"contains": []any{"{{ .node_tasks.migrate.stdout }}", "x"}}, Show: "read"}
	if got := handler(taskBlock(noisy, guarded)); got.Status != "failed" || !strings.Contains(got.Reason, "truncated") {
		t.Errorf("a predicate trusted truncated output: %+v", got)
	}
	skipped := job(config.Task{ID: "migrate", When: false, Module: "shell", ModuleInputs: &modules.Shell{Script: "true"}}, report("stdout"))
	if r := (&Engine{}).Run(context.Background(), skipped); r.Status != "blocked" {
		t.Errorf("a skipped task supplied output: %s %+v", r.Status, r.Results)
	}
}

func TestRebootPathGuards(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "patched")
	put(t, marker, "ready")
	for _, dryrun := range []bool{false, true} {
		for _, test := range []struct {
			skip, only string
			allowed    bool
		}{
			{marker, "", false},
			{marker + ".missing", "", true},
			{"", marker, true},
			{"", marker + ".missing", false},
		} {
			e := Engine{Machine: NodeMachine{BootID: func() (string, error) { return "boot-1", nil }}}
			j := job(config.Task{Module: "reboot", ModuleInputs: &modules.Reboot{SkipIfThisPathExists: test.skip, RunOnlyIfThisPathExists: test.only}})
			j.Dryrun = dryrun
			r := e.Run(context.Background(), j)
			want := "skipped"
			if test.allowed {
				want = "rebooting"
				if dryrun {
					want = "succeeded"
				}
			}
			if len(r.Results) != 1 || r.Results[0].Status != want {
				t.Fatalf("dryrun=%t skip=%q only=%q: %+v", dryrun, test.skip, test.only, r)
			}
		}
	}
}

// A template that fails the same way on every try, such as percent outside 0 to
// 100, fails at once and is not tried again.
func TestTemplateErrorThatNoRetryCanFixFailsAtOnce(t *testing.T) {
	dir := t.TempDir()
	task := config.Task{ID: "share", Module: "file", ModuleInputs: &modules.File{Mode: "0644", Path: filepath.Join(dir, "cache.conf"), Template: "sizing", TemplateText: `cache={{ percent .node.memory.total_mib 150 }}`},
		Retry: &config.Retry{MaxTries: config.TryLimit(3), PauseBetweenTries: "0s"}}
	e := &Engine{RuntimeRoot: filepath.Join(dir, "state"), collectFacts: func(string) facts.Snapshot {
		return facts.Snapshot{CollectedAt: time.Now(), Values: map[string]any{"name": "test", "memory": map[string]any{"total_mib": int64(4096)}}}
	}}
	r := e.Run(context.Background(), job(task))
	requireStatus(t, r, "failed")
	got := r.Results[0]
	if len(got.Tries) > 1 || got.FailureKind != "config" || !strings.Contains(got.Reason, "percent requires a percentage between 0 and 100") || strings.Contains(got.Reason, "3 tries") {
		t.Fatalf("%+v", got)
	}
}

// A reading shell task with run_script_in_dryrun is checked in a dryrun like
// one with a dryrun command: its result stands on an unchanged node, and a
// failure after an earlier change that the dryrun proposes is not_checked.
func TestDryrunChecksAReadingShellTaskThatRunsItsScript(t *testing.T) {
	dir := t.TempDir()
	reads := func(script string) config.Task {
		return config.Task{Module: "shell", ModuleInputs: &modules.Shell{Script: script, RunScriptInDryrun: true, ChangedWhen: false}}
	}
	dryrun := func(tasks ...config.Task) protocol.Response {
		j := job(tasks...)
		j.Dryrun = true
		return (&Engine{}).Run(context.Background(), j)
	}
	r := dryrun(reads("echo ok"))
	if r.Results[0].Status != "succeeded" || r.Results[0].Change != "unchanged" {
		t.Fatalf("%+v", r)
	}
	r = dryrun(reads("exit 1"))
	if r.Results[0].Status != "failed" {
		t.Fatalf("%+v", r)
	}
	r = dryrun(taskFile(filepath.Join(dir, "new"), "x"), reads("exit 1"))
	if r.Results[1].Status != "not_checked" {
		t.Fatalf("%+v", r)
	}
}

// A fleet stage output never carries MMX's record keys from a command's JSON
// to the controller process, whether the output reads the whole JSON or a
// part that holds such a key.
func TestOutputsCarryNoRecordKeysFromCommandJSON(t *testing.T) {
	api := config.Task{ID: "api", Module: "shell", ModuleInputs: &modules.Shell{Script: `echo '{"config": {"token": {"$mmx_secret": {"name": "production_database_root_password"}}}}'`, ChangedWhen: false}}
	for _, output := range []string{"{{ .node_tasks.api.stdout_json }}", "{{ .node_tasks.api.stdout_json.config }}"} {
		j := job(api)
		j.Outputs = map[string]any{"config": output}
		r := (&Engine{}).Run(context.Background(), j)
		if r.Status != "failed" || !strings.Contains(r.Error, "$mmx_secret, which MMX reserves for its own records") || r.Outputs != nil {
			t.Fatalf("%s: %s %s %v", output, r.Status, r.Error, r.Outputs)
		}
	}
	// The command's text itself passes on.
	j := job(api)
	j.Outputs = map[string]any{"text": "{{ .node_tasks.api.stdout }}"}
	if r := (&Engine{}).Run(context.Background(), j); r.Status != "succeeded" {
		t.Fatal(r.Status, r.Error)
	}
}
