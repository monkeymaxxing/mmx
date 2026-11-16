package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"testing"

	"mmx/internal/config"
	"mmx/internal/modules"
	"mmx/internal/protocol"
	"mmx/internal/value"
)

// commandFixture is a task block with one tracked file and one command that
// follows it. The command appends a line to count each time it runs.
type commandFixture struct {
	dir, path, count string
	e                Engine
}

func newCommandFixture(t *testing.T) *commandFixture {
	t.Helper()
	dir := t.TempDir()
	f := &commandFixture{dir: dir, path: filepath.Join(dir, "virtual"), count: filepath.Join(dir, "count"),
		e: Engine{RuntimeRoot: filepath.Join(dir, "run"), StateRoot: filepath.Join(dir, "state")}}
	put(t, f.path, "A")
	if err := os.Chmod(f.path, 0644); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *commandFixture) op(content string) config.Task {
	return config.Task{TaskBlock: &config.TaskBlock{ID: "postfix_virtual_map", Changes: []config.Task{taskFile(f.path, content)},
		Activate: []config.Task{{ID: "rebuild_virtual_map", Module: "shell", ModuleInputs: &modules.Shell{Script: "echo ran >> " + f.count, OnlyWhenTrackedInputsChanged: true}}}}}
}

func (f *commandFixture) runs(t *testing.T) int {
	t.Helper()
	b, err := os.ReadFile(f.count)
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return strings.Count(string(b), "ran\n")
}

func (f *commandFixture) run(t *testing.T, op config.Task, status string, runs int) protocol.Response {
	t.Helper()
	r := f.e.Run(context.Background(), job(op))
	requireStatus(t, r, status)
	if got := f.runs(t); got != runs {
		t.Fatalf("the command ran %d times, want %d: %+v", got, runs, r.Results)
	}
	return r
}

func commandResult(t *testing.T, r protocol.Response) protocol.Result {
	t.Helper()
	for _, result := range r.Results {
		if result.Task == "postfix_virtual_map/activate/rebuild_virtual_map" {
			return result
		}
	}
	t.Fatalf("no result for the command: %+v", r.Results)
	return protocol.Result{}
}

// A missing record takes a baseline and does not run the command. A changed
// input runs it once, an unchanged rerun skips it, and the record survives
// the loss of the runtime directory, as after a reboot.
func TestCommandRunsOnlyWhenTrackedInputsChanged(t *testing.T) {
	f := newCommandFixture(t)
	first := f.run(t, f.op("A"), "succeeded", 0)
	if r := commandResult(t, first); r.Status != "skipped" || r.Change != "unchanged" {
		t.Fatal(r)
	}
	changed := f.run(t, f.op("B"), "succeeded", 1)
	if r := commandResult(t, changed); r.Change != "changed" || r.Reason != "ran because tracked input "+f.path+" changed" {
		t.Fatal(r)
	}
	if r := f.run(t, f.op("B"), "succeeded", 1); r.Change != "unchanged" {
		t.Fatal(r)
	}
	if err := os.RemoveAll(f.e.RuntimeRoot); err != nil {
		t.Fatal(err)
	}
	f.run(t, f.op("B"), "succeeded", 1)
}

// Dryrun reads the record without writing it and says why the command would run.
func TestCommandDryrunReportsFromTheRecord(t *testing.T) {
	f := newCommandFixture(t)
	f.run(t, f.op("A"), "succeeded", 0)
	record := read(t, f.e.commandPath("", "postfix_virtual_map/activate/rebuild_virtual_map"))
	for content, want := range map[string]string{"A": "skipped", "B": "succeeded"} {
		j := job(f.op(content))
		j.Dryrun = true
		r := commandResult(t, f.e.Run(context.Background(), j))
		if r.Status != want || (content == "B" && (r.Change != "would_change" || r.Reason != "would run: tracked input "+f.path+" would change")) {
			t.Fatal(content, r)
		}
	}
	if f.runs(t) != 0 || read(t, f.e.commandPath("", "postfix_virtual_map/activate/rebuild_virtual_map")) != record || read(t, f.path) != "A" {
		t.Fatal("dryrun ran the command or wrote")
	}
	// A dryrun before the first run compares with the files on disk.
	fresh := newCommandFixture(t)
	j := job(fresh.op("B"))
	j.Dryrun = true
	if r := commandResult(t, fresh.e.Run(context.Background(), j)); r.Change != "would_change" {
		t.Fatal(r)
	}
	if _, err := os.Stat(fresh.e.StateRoot); !os.IsNotExist(err) {
		t.Fatal("dryrun wrote a record", err)
	}
}

// A node task block can track one more input than its command's record
// holds. A dryrun takes that input's baseline as a run does, without writing
// it: neither says that the command would run for an input that is as
// declared.
func TestCommandDryrunTakesTheBaselineOfANewTrackedInput(t *testing.T) {
	f := newCommandFixture(t)
	f.run(t, f.op("A"), "succeeded", 0)
	path := f.e.commandPath("", "postfix_virtual_map/activate/rebuild_virtual_map")
	record := read(t, path)
	second := filepath.Join(f.dir, "second")
	put(t, second, "S")
	op := f.op("A")
	op.TaskBlock.Changes = append(op.TaskBlock.Changes, taskFile(second, "S"))
	j := job(op)
	j.Dryrun = true
	if r := commandResult(t, f.e.Run(context.Background(), j)); r.Status != "skipped" || r.Change != "unchanged" {
		t.Fatal("the dryrun predicts a command that the run does not run: ", r)
	}
	if read(t, path) != record {
		t.Fatal("dryrun wrote the record")
	}
	if r := commandResult(t, f.run(t, op, "succeeded", 0)); r.Status != "skipped" {
		t.Fatal(r)
	}
}

// A run that wrote the file and stopped before the command, and a command
// that failed, both leave the command owed to the next run.
func TestCommandStillRunsAfterAnInterruptedOrFailedRun(t *testing.T) {
	f := newCommandFixture(t)
	f.run(t, f.op("A"), "succeeded", 0)
	stopped := f.op("B")
	stopped.TaskBlock.Validate = []config.Task{{Module: "shell", ModuleInputs: &modules.Shell{Script: "exit 1"}}}
	f.run(t, stopped, "failed", 0)
	if read(t, f.path) != "B" {
		t.Fatal("the file was not written")
	}
	f.run(t, f.op("B"), "succeeded", 1)
	failing := f.op("C")
	config.InputsOf[modules.Shell](failing.TaskBlock.Activate[0]).Script = "exit 1"
	f.run(t, failing, "failed", 1)
	recovered := f.run(t, f.op("C"), "succeeded", 2)
	if r := commandResult(t, recovered); r.Reason != "ran because the last run of this command did not finish" {
		t.Fatal(r)
	}
	f.run(t, f.op("C"), "succeeded", 2)
}

// A first run that is interrupted has already saved the baseline, so the
// next run still sees the change.
func TestCommandBaselineIsSavedBeforeTheFirstWrite(t *testing.T) {
	f := newCommandFixture(t)
	stopped := f.op("B")
	stopped.TaskBlock.Validate = []config.Task{{Module: "shell", ModuleInputs: &modules.Shell{Script: "exit 1"}}}
	f.run(t, stopped, "failed", 0)
	f.run(t, f.op("B"), "succeeded", 1)
}

// A command follows a module that is not core by the module's fingerprint,
// which only the module's own task reports. A new command record holds none:
// a module that changes nothing gives the baseline and the command does not
// run, and a module that changes something runs it once. A dryrun predicts
// the same and writes no record.
func TestCommandFollowsAModuleFingerprint(t *testing.T) {
	f := newCommandFixture(t)
	module, writer := writerModule(t, f.path)
	block := func(text string) config.Task {
		op := f.op(text)
		op.TaskBlock.Changes = []config.Task{writer(text)}
		return op
	}
	run := func(block config.Task, dryrun bool, status string, runs int) protocol.Response {
		t.Helper()
		j := job(block)
		j.Modules, j.Dryrun = module, dryrun
		r := f.e.Run(context.Background(), j)
		requireStatus(t, r, status)
		if got := f.runs(t); got != runs {
			t.Fatalf("the command ran %d times, want %d: %+v", got, runs, r.Results)
		}
		return r
	}
	if r := commandResult(t, run(block("A"), true, "succeeded", 0)); r.Status != "skipped" || r.Change != "unchanged" {
		t.Fatal(r)
	}
	if r := commandResult(t, run(block("B"), true, "succeeded", 0)); r.Change != "would_change" || r.Reason != "would run: tracked input module writer would change" {
		t.Fatal(r)
	}
	if _, err := os.Stat(f.e.StateRoot); !os.IsNotExist(err) {
		t.Fatal("dryrun wrote a record", err)
	}
	for range 2 {
		if r := commandResult(t, run(block("A"), false, "succeeded", 0)); r.Status != "skipped" || r.Reason != "tracked inputs are unchanged since this command last ran" {
			t.Fatal(r)
		}
	}
	if r := commandResult(t, run(block("B"), false, "succeeded", 1)); r.Reason != "ran because tracked input module writer changed" {
		t.Fatal(r)
	}
	run(block("B"), false, "succeeded", 1)

	// Without a record, a module that changes something runs the command,
	// also when the run with the change ended before the command.
	for _, interrupted := range []bool{false, true} {
		if err := os.RemoveAll(f.e.StateRoot); err != nil {
			t.Fatal(err)
		}
		runs := f.runs(t)
		text := map[bool]string{false: "C", true: "D"}[interrupted]
		if interrupted {
			stopped := block(text)
			stopped.TaskBlock.Validate = []config.Task{{Module: "shell", ModuleInputs: &modules.Shell{Script: "exit 1"}}}
			run(stopped, false, "failed", runs)
		}
		for range 2 {
			run(block(text), false, "succeeded", runs+1)
		}
	}

	// A module task that its condition skips changes nothing: nothing is
	// owed for it, and its fingerprint is the baseline when it next runs.
	if err := os.RemoveAll(f.e.StateRoot); err != nil {
		t.Fatal(err)
	}
	runs := f.runs(t)
	skipped := block("D")
	skipped.TaskBlock.Changes[0].When = false
	run(skipped, false, "succeeded", runs)
	run(block("D"), false, "succeeded", runs)
	run(block("E"), false, "succeeded", runs+1)
}

// Each loop item has its own record, and a corrupt record fails the task
// block before any change.
func TestCommandRecordsPerLoopItemAndCorruptRecord(t *testing.T) {
	f := newCommandFixture(t)
	op := f.op("A")
	command := &op.TaskBlock.Activate[0]
	command.Loop, command.As = []any{"one", "two"}, "name"
	f.run(t, op, "succeeded", 0)
	// Without a key, a list item is named by its position, as in results.
	for _, key := range []string{"0", "1"} {
		if _, err := os.Stat(f.e.commandPath("", "postfix_virtual_map/activate/rebuild_virtual_map["+key+"]")); err != nil {
			t.Fatal(err)
		}
	}
	op = f.op("B")
	op.TaskBlock.Activate[0].Loop, op.TaskBlock.Activate[0].As = []any{"one", "two"}, "name"
	f.run(t, op, "succeeded", 2)
	put(t, f.e.commandPath("", "postfix_virtual_map/activate/rebuild_virtual_map[0]"), "{")
	op = f.op("C")
	op.TaskBlock.Activate[0].Loop, op.TaskBlock.Activate[0].As = []any{"one", "two"}, "name"
	r := f.run(t, op, "failed", 2)
	if !strings.Contains(r.Error, "invalid command record") || read(t, f.path) != "B" {
		t.Fatal(r.Error)
	}
}

// iterationCommand is a node task block of one fleet stage loop iteration on
// a helper node, such as a load balancer drained for each application node:
// the block writes the iteration's own file, and its command follows the
// file. The command appends "ran" to count, or fails.
func (f *commandFixture) iterationCommand(t *testing.T, key, content string, fails bool) config.Task {
	t.Helper()
	path := filepath.Join(f.dir, key)
	if _, err := os.Stat(path); os.IsNotExist(err) {
		put(t, path, "A")
		if err := os.Chmod(path, 0644); err != nil {
			t.Fatal(err)
		}
	}
	script := "echo ran >> " + f.count
	if fails {
		script = "exit 1"
	}
	return config.Task{TaskBlock: &config.TaskBlock{ID: "postfix_virtual_map", Changes: []config.Task{taskFile(path, content)},
		Activate: []config.Task{{ID: "rebuild_virtual_map", Module: "shell", ModuleInputs: &modules.Shell{Script: script, OnlyWhenTrackedInputsChanged: true}}}}}
}

// runIn runs a node job of the fleet stage at the path fleetStage, as the
// controller process names it: rollout[web1]/drain inside a loop.
func (f *commandFixture) runIn(t *testing.T, fleetStage string, op config.Task, status string, runs int) protocol.Response {
	t.Helper()
	j := job(op)
	j.FleetStage = fleetStage
	r := f.e.Run(context.Background(), j)
	requireStatus(t, r, status)
	if got := f.runs(t); got != runs {
		t.Fatalf("the command ran %d times, want %d: %+v", got, runs, r.Results)
	}
	return r
}

// release010CommandRecordPath is where the node runner executable of
// release 0.1.0 keeps a command record: under the project and the task path
// alone, also inside a fleet stage loop.
func release010CommandRecordPath(f *commandFixture, task string) string {
	sum := sha256.Sum256([]byte("test\x00" + task))
	return filepath.Join(f.e.StateRoot, "command_records", hex.EncodeToString(sum[:])+".json")
}

// Two iterations of a fleet stage loop that reach one node keep one command
// record each: a command that failed in one iteration stays owed after the
// other iteration's command succeeded. A fleet stage outside loops keeps the
// record key of release 0.1.0.
func TestCommandRecordsPerFleetStageLoopIteration(t *testing.T) {
	f := newCommandFixture(t)
	web1, web2 := "rollout[web1]/drain", "for_each_app[web2]/drain"
	f.runIn(t, web1, f.iterationCommand(t, "web1", "A", false), "succeeded", 0)
	f.runIn(t, web2, f.iterationCommand(t, "web2", "A", false), "succeeded", 0)
	f.runIn(t, web1, f.iterationCommand(t, "web1", "B", true), "failed", 0)
	f.runIn(t, web2, f.iterationCommand(t, "web2", "B", false), "succeeded", 1)
	owed := f.runIn(t, web1, f.iterationCommand(t, "web1", "B", false), "succeeded", 2)
	if r := commandResult(t, owed); r.Reason != "ran because the last run of this command did not finish" {
		t.Fatal(r)
	}
	f.runIn(t, web1, f.iterationCommand(t, "web1", "B", false), "succeeded", 2)
	f.runIn(t, web2, f.iterationCommand(t, "web2", "B", false), "succeeded", 2)

	outside := newCommandFixture(t)
	outside.runIn(t, "drain", outside.iterationCommand(t, "web1", "A", false), "succeeded", 0)
	if _, err := os.Stat(release010CommandRecordPath(outside, "postfix_virtual_map/activate/rebuild_virtual_map")); err != nil {
		t.Fatal(err)
	}
}

// A command inside a fleet stage loop that the node runner executable of
// release 0.1.0 left owed, under the record key without the iteration, runs
// once in the iteration after the upgrade. A command that the old record says
// ran with the current inputs does not run. The old record is made as release
// 0.1.0 made it for every command: by a fleet stage outside loops, where the
// key has not changed.
func TestCommandRecordOfRelease010IsReadInAFleetStageLoop(t *testing.T) {
	for _, owed := range []bool{true, false} {
		f := newCommandFixture(t)
		f.runIn(t, "drain", f.iterationCommand(t, "web1", "A", false), "succeeded", 0)
		f.runIn(t, "drain", f.iterationCommand(t, "web1", "B", owed), map[bool]string{true: "failed", false: "succeeded"}[owed], map[bool]int{true: 0, false: 1}[owed])
		old := release010CommandRecordPath(f, "postfix_virtual_map/activate/rebuild_virtual_map")
		if _, err := os.Stat(old); err != nil {
			t.Fatal(err)
		}
		f.runIn(t, "rollout[web1]/drain", f.iterationCommand(t, "web1", "B", false), "succeeded", 1)
		f.runIn(t, "rollout[web1]/drain", f.iterationCommand(t, "web1", "B", false), "succeeded", 1)
		f.runIn(t, "rollout[web1]/drain", f.iterationCommand(t, "web1", "C", false), "succeeded", 2)
	}
}

func assertTask(ok bool) config.Task {
	return config.Task{Module: "assert", ModuleInputs: &modules.Assert{That: ok, Message: "Redis must restart to load this change; run again with --var restart_approved=true"}}
}

// The checks run only when a running service would be refreshed, before the
// first write; a failed check leaves the node unchanged.
func TestChecksBeforeRestartOrReload(t *testing.T) {
	f := newServiceFixture(t)
	op := func(content string, approved bool) config.Task {
		task := f.op(content)
		task.TaskBlock.ChecksBeforeRestartOrReload = []config.Task{assertTask(approved)}
		return task
	}
	check := func(r protocol.Response) protocol.Result {
		t.Helper()
		for _, result := range r.Results {
			if result.Task == "server/checks_before_restart_or_reload/task_1" {
				return result
			}
		}
		t.Fatalf("no result for the check: %+v", r.Results)
		return protocol.Result{}
	}
	unchanged := f.e.Run(context.Background(), job(op("A", false)))
	requireStatus(t, unchanged, "succeeded")
	if r := check(unchanged); r.Status != "skipped" {
		t.Fatal(r)
	}
	for _, dryrun := range []bool{true, false} {
		j := job(op("B", false))
		j.Dryrun = dryrun
		refused := f.e.Run(context.Background(), j)
		requireStatus(t, refused, "failed")
		if !strings.Contains(refused.Error, "run again with --var restart_approved=true") || read(t, f.path) != "A" {
			t.Fatal(refused.Error, read(t, f.path))
		}
		f.assert(t, "A", "")
	}
	approved := f.e.Run(context.Background(), job(op("B", true)))
	requireStatus(t, approved, "succeeded")
	if r := check(approved); r.Status != "succeeded" {
		t.Fatal(r)
	}
	f.assert(t, "B", "reload\n")
	// A restart still owed from an interrupted run needs the checks again.
	interrupted := op("C", true)
	interrupted.TaskBlock.Validate = []config.Task{{Module: "shell", ModuleInputs: &modules.Shell{Script: "exit 1"}}}
	f.run(t, interrupted, "failed")
	f.run(t, op("C", false), "failed")
	f.assert(t, "B", "reload\n")
	f.run(t, op("C", true), "succeeded")
	f.assert(t, "C", "reload\nreload\n")
	// A stopped service starts without the checks.
	put(t, filepath.Join(f.dir, "demo.service/active"), "inactive\n")
	f.run(t, op("D", false), "succeeded")
	f.assert(t, "D", "reload\nreload\nstart\n")
}

// A command that follows a change counts like a service for the checks.
func TestChecksGuardACommandThatFollowsAChange(t *testing.T) {
	f := newCommandFixture(t)
	op := func(content string, approved bool) config.Task {
		task := f.op(content)
		task.TaskBlock.ChecksBeforeRestartOrReload = []config.Task{assertTask(approved)}
		return task
	}
	f.run(t, op("A", false), "succeeded", 0)
	f.run(t, op("B", false), "failed", 0)
	if read(t, f.path) != "A" {
		t.Fatal("a failed check did not keep the file")
	}
	f.run(t, op("B", true), "succeeded", 1)
}

// Before a node task block's first write, the engine asks a dryrun of the
// block's git task which commit the checkout would move to. That dryrun is
// told where a node task's private directory goes, as the task itself is, so
// git reaches a private repository with the task's password; the place is
// empty again afterwards.
func TestPreviewOfACheckoutUsesTheTasksCredentials(t *testing.T) {
	f := newCommandFixture(t)
	privateTaskDirectoryRoot := filepath.Join(f.dir, "private_task_directories")
	if err := os.Mkdir(privateTaskDirectoryRoot, 0711); err != nil {
		t.Fatal(err)
	}
	f.e.PrivateTaskDirectoryRoot = func() (string, error) { return privateTaskDirectoryRoot, nil }
	// A stand-in git that answers only the question of a dryrun, which commit
	// a branch names, and notes how it was asked for the password.
	asked := filepath.Join(f.dir, "asked")
	if err := os.Mkdir(filepath.Join(f.dir, "bin"), 0755); err != nil {
		t.Fatal(err)
	}
	writeProgramFileThroughShellChild(t, filepath.Join(f.dir, "bin", "git"), `#!/bin/sh
[ "$1" = ls-remote ] || exit 1
{ echo "$GIT_ASKPASS"; "$GIT_ASKPASS" "Password for 'https://deploy@example.com': "; } > `+asked+`
printf '1111111111111111111111111111111111111111\trefs/heads/main\n'
`)
	t.Setenv("PATH", filepath.Join(f.dir, "bin")+string(os.PathListSeparator)+os.Getenv("PATH"))
	op := config.Task{TaskBlock: &config.TaskBlock{ID: "shop_code",
		Changes:                     []config.Task{{Module: "git", ModuleInputs: &modules.Git{Repo: "https://example.com/shop.git", FollowBranch: "main", Path: filepath.Join(f.dir, "checkout"), Username: "deploy", Password: "s3cret-token"}}},
		ChecksBeforeRestartOrReload: []config.Task{assertTask(false)},
		Activate:                    []config.Task{{ID: "restart_shop", Module: "shell", ModuleInputs: &modules.Shell{Script: "echo ran >> " + f.count, OnlyWhenTrackedInputsChanged: true}}}}}
	r := f.run(t, op, "failed", 0)
	if !strings.Contains(r.Error, "run again with --var restart_approved=true") {
		t.Fatal(r.Error)
	}
	if got := read(t, asked); !strings.HasPrefix(got, privateTaskDirectoryRoot+"/git-") || !strings.HasSuffix(got, "/askpass\ns3cret-token") {
		t.Fatalf("the preview's git did not get the task's credentials: %q", got)
	}
	if left, err := os.ReadDir(privateTaskDirectoryRoot); err != nil || len(left) != 0 {
		t.Fatal("the preview left the task's credentials behind", left, err)
	}
}

// A checkout's commit is a tracked input: a new commit makes the service
// load it with the task's refresh, and runs a command that follows it.
func TestCheckoutIsATrackedInput(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	f := newServiceFixture(t)
	f.e.StateRoot = filepath.Join(f.dir, "persistent")
	source, checkout, count := filepath.Join(f.dir, "source"), filepath.Join(f.dir, "checkout"), filepath.Join(f.dir, "count")
	git := func(args ...string) {
		t.Helper()
		if out, err := exec.Command("git", append([]string{"-C", source, "-c", "user.name=MMX", "-c", "user.email=mmx@example.com"}, args...)...).CombinedOutput(); err != nil {
			t.Fatal(args, string(out), err)
		}
	}
	commit := func(text string) {
		t.Helper()
		put(t, filepath.Join(source, "app.txt"), text)
		git("add", "app.txt")
		git("commit", "-q", "-m", text)
	}
	if err := os.Mkdir(source, 0755); err != nil {
		t.Fatal(err)
	}
	git("init", "-q", "-b", "main")
	commit("one")
	op := config.Task{TaskBlock: &config.TaskBlock{ID: "shop_code",
		Changes: []config.Task{{Module: "git", ModuleInputs: &modules.Git{Repo: source, FollowBranch: "main", Path: checkout}}},
		Activate: []config.Task{
			{Module: "systemd_service", ModuleInputs: &modules.Service{State: "running", Name: "demo", Refresh: "reload"}},
			{ID: "tell_passenger_to_restart", Module: "shell", ModuleInputs: &modules.Shell{Script: "echo ran >> " + count, OnlyWhenTrackedInputsChanged: true}},
		}}}
	runs := func() int { b, _ := os.ReadFile(count); return strings.Count(string(b), "ran\n") }
	// The first checkout differs from the baseline, which had no checkout.
	f.run(t, op, "succeeded")
	f.assert(t, "A", "reload\n")
	if runs() != 1 {
		t.Fatal(runs())
	}
	f.run(t, op, "succeeded")
	f.assert(t, "A", "reload\n")
	commit("two")
	j := job(op)
	j.Dryrun = true
	preview := f.e.Run(context.Background(), j)
	requireStatus(t, preview, "succeeded")
	if r := preview.Results[2]; preview.Change != "would_change" || r.Reason != "would run: tracked input checkout "+checkout+" would change" {
		t.Fatalf("%+v", preview.Results)
	}
	f.run(t, op, "succeeded")
	f.assert(t, "A", "reload\nreload\n")
	if runs() != 2 {
		t.Fatal(runs())
	}
	f.run(t, op, "succeeded")
	f.assert(t, "A", "reload\nreload\n")
	if runs() != 2 {
		t.Fatal(runs())
	}
}

// What an installer installed is a tracked input: a stand-in python3 keeps
// the virtual environment's packages in a file, and a new version runs the
// command that follows it once.
func TestInstalledPackagesAreATrackedInput(t *testing.T) {
	account, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	f := newCommandFixture(t)
	installed := filepath.Join(f.dir, "installed")
	python := filepath.Join(f.dir, "python3")
	writeProgramFileThroughShellChild(t, python, `#!/bin/sh
if [ "$1" = -m ] && [ "$2" = venv ]; then mkdir -p "$3/bin"; cp "$0" "$3/bin/python"; exit 0; fi
shift 4
touch `+installed+`
case "$1" in
--version) echo "pip 23.0.1 from /usr/lib/python3/dist-packages/pip (python 3.11)";;
freeze) cat `+installed+`;;
install)
  shift
  dryrun=no; entries=
  for r in "$@"; do
    case "$r" in --dry-run) dryrun=yes; continue;; --*|-) continue;; esac
    entries="$entries{\"metadata\": {\"name\": \"${r%%==*}\", \"version\": \"${r#*==}\"}}"
    [ $dryrun = yes ] || { grep -v "^${r%%==*}==" `+installed+` > `+installed+`.kept || true; mv `+installed+`.kept `+installed+`; echo "$r" >> `+installed+`; }
  done
  [ $dryrun = no ] || echo "{\"install\": [$entries]}";;
esac
`)
	venv := filepath.Join(f.dir, "venv")
	op := func(requirement string) config.Task {
		return config.Task{TaskBlock: &config.TaskBlock{ID: "myapp",
			Changes:  []config.Task{{Module: "pip_package", ModuleInputs: &modules.PipPackage{RunAsUserAccount: account.Username, Packages: []modules.InstallerPackage{{Name: "gunicorn", Version: strings.TrimPrefix(requirement, "gunicorn")}}, VirtualEnvironment: venv, PythonProgramPath: python}}},
			Activate: []config.Task{{ID: "restart_myapp", Module: "shell", ModuleInputs: &modules.Shell{Script: "echo ran >> " + f.count, OnlyWhenTrackedInputsChanged: true}}}}}
	}
	// The first installation differs from the baseline, which had no
	// virtual environment.
	f.run(t, op("gunicorn==22.0.0"), "succeeded", 1)
	f.run(t, op("gunicorn==22.0.0"), "succeeded", 1)
	j := job(op("gunicorn==23.0.0"))
	j.Dryrun = true
	preview := f.e.Run(context.Background(), j)
	requireStatus(t, preview, "succeeded")
	if r := preview.Results[1]; r.Task != "myapp/activate/restart_myapp" || r.Change != "would_change" || r.Reason != "would run: tracked input installation of pip_package in "+venv+" would change" {
		t.Fatalf("%+v", preview.Results)
	}
	f.run(t, op("gunicorn==23.0.0"), "succeeded", 2)
	f.run(t, op("gunicorn==23.0.0"), "succeeded", 2)
}

// An installer's tracked input is read with the task's env, resolved in the
// task's scope: a stand-in gem keeps its gems in GEM_HOME, and a new version
// there runs the command that follows it once. A secret in env reaches only
// gem: neither a result nor a record on the node holds it, even when the
// listing fails and gem prints it.
func TestInstalledGemsInGemHomeAreATrackedInput(t *testing.T) {
	account, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	f := newCommandFixture(t)
	gem := filepath.Join(f.dir, "gem")
	writeProgramFileThroughShellChild(t, gem, `#!/bin/sh
store="$GEM_HOME/installed"
touch "$store"
case "$1" in
list) [ ! -e "$GEM_HOME/fail" ] || { echo "cannot list with key $GEM_HOST_API_KEY" >&2; exit 1; }
  shift 3; for n in "$@"; do v=$(grep "^$n " "$store" | cut -d' ' -f2); [ -z "$v" ] || echo "$n ($v)"; done;;
install) shift 2; n=${1%%:*}; v=${1#*:}; grep -v "^$n " "$store" > "$store.kept"; mv "$store.kept" "$store"; echo "$n $v" >> "$store";;
esac
`)
	gems := filepath.Join(f.dir, "gems")
	if err := os.Mkdir(gems, 0755); err != nil {
		t.Fatal(err)
	}
	run := func(version, status string, runs int) protocol.Response {
		t.Helper()
		j := job(config.Task{TaskBlock: &config.TaskBlock{ID: "myapp",
			Changes: []config.Task{{Module: "gem_package", ModuleInputs: &modules.GemPackage{RunAsUserAccount: account.Username, Packages: []modules.InstallerPackage{{Name: "rack", Version: version}}, GemProgramPath: gem,
				Env: map[string]any{"GEM_HOME": "{{ .vars.gem_home }}", "GEM_HOST_API_KEY": "{{ .secrets.gem_key }}"}}}},
			Activate: []config.Task{{ID: "restart_myapp", Module: "shell", ModuleInputs: &modules.Shell{Script: "echo ran >> " + f.count, OnlyWhenTrackedInputsChanged: true}}}}})
		j.Vars = map[string]any{"gem_home": gems}
		j.Scope = map[string]any{"secrets": map[string]any{"gem_key": value.Sensitive("private-marker")}}
		r := f.e.Run(context.Background(), j)
		requireStatus(t, r, status)
		if got := f.runs(t); got != runs {
			t.Fatalf("the command ran %d times, want %d: %+v", got, runs, r.Results)
		}
		return r
	}
	run("3.1.7", "succeeded", 1)
	run("3.1.7", "succeeded", 1)
	run("3.1.8", "succeeded", 2)
	run("3.1.8", "succeeded", 2)
	put(t, filepath.Join(gems, "fail"), "")
	failed := run("3.1.8", "failed", 2)
	data, err := json.Marshal(failed)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "private-marker") {
		t.Fatalf("the result holds the secret: %s", data)
	}
	err = filepath.WalkDir(f.dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil || entry.IsDir() || path == gem {
			return err
		}
		if content, err := os.ReadFile(path); err != nil || strings.Contains(string(content), "private-marker") {
			return fmt.Errorf("%s holds the secret (%v)", path, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
