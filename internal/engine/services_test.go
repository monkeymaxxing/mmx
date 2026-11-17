package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mmx/internal/config"
	"mmx/internal/modules"
	"mmx/internal/value"
)

// The fake service keeps its loaded contents separately from both the live file
// and MMX's record. Tests assert what the service loaded, not just stored hashes.
type serviceFixture struct {
	dir, path string
	e         Engine
}

func newServiceFixture(t *testing.T) *serviceFixture {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin")
	if err := os.Mkdir(bin, 0700); err != nil {
		t.Fatal(err)
	}
	script := `#!/bin/sh
set -eu
for unit; do :; done
[ "$unit" != alias.service ] || unit=demo.service
root="$MMX_TEST_SERVICE/$unit"
case "$1" in
daemon-reload)
 echo no > "$MMX_TEST_SERVICE/demo.service/need-reload"
 ;;
show)
 if [ ! -d "$root" ]; then printf 'Id=%s\nNames=%s\nLoadState=not-found\nActiveState=inactive\nUnitFileState=\nInvocationID=\n' "$unit" "$unit"; exit 0; fi
 names=$unit
 [ "$unit" != demo.service ] || names="demo.service alias.service"
 load=loaded
 [ "$(cat "$root/enabled")" != masked ] || load=masked
 printf 'Id=%s\nNames=%s\nLoadState='$load'\nActiveState=%s\nUnitFileState=%s\nInvocationID=%s\n' "$unit" "$names" "$(cat "$root/active")" "$(cat "$root/enabled")" "$(cat "$root/instance")"
 [ ! -f "$root/type" ] || printf 'Type=%s\nRemainAfterExit=no\n' "$(cat "$root/type")"
 [ ! -f "$root/source" ] || printf 'SourcePath=%s\n' "$(cat "$root/source")"
 printf 'NeedDaemonReload=%s\n' "$(cat "$root/need-reload" 2>/dev/null || echo no)"
 # A queued state becomes current after one inspection.
 [ ! -f "$root/next-active" ] || mv "$root/next-active" "$root/active"
 ;;
*)
 echo "$1" >> "$root/actions"
 [ ! -f "$root/fail" ] || exit 1
 # Starting a running unit only waits for its start or reload to finish.
 if [ "$1" = start ] && [ "$(cat "$root/active")" != inactive ] && [ "$(cat "$root/active")" != failed ]; then echo active > "$root/active"; exit 0; fi
 case "$1" in
 start|restart) echo active > "$root/active"; echo "$(($(cat "$root/instance")+1))" > "$root/instance"
  # A unit that ends at once: systemd starts it again when the start is done.
  [ ! -f "$root/active-after-start" ] || cat "$root/active-after-start" > "$root/active" ;;
 reload) echo active > "$root/active" ;;
 stop) echo inactive > "$root/active" ;;
 enable) echo enabled > "$root/enabled" ;;
 mask) echo masked > "$root/enabled" ;;
 unmask) echo disabled > "$root/enabled" ;;
 disable)
  if [ "$(cat "$root/enabled")" != enabled-runtime ] || [ "$2" = --runtime ]; then echo disabled > "$root/enabled"; fi ;;
 esac
 case "$1" in start|restart|reload) cat "$MMX_TEST_SERVICE/config" > "$root/loaded" ;; esac
 ;;
esac
`
	writeProgramFileThroughShellChild(t, filepath.Join(bin, "systemctl"), script)
	query := `#!/bin/sh
for name; do :; done
[ -f "$MMX_TEST_SERVICE/package-$name" ] || exit 1
cat "$MMX_TEST_SERVICE/package-$name"
`
	writeProgramFileThroughShellChild(t, filepath.Join(bin, "dpkg-query"), query)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("MMX_TEST_SERVICE", dir)
	// apt's own files (its service policy program, version rules) go under root.
	root := filepath.Join(dir, "root")
	for _, d := range []string{"usr/sbin", "etc/apt/preferences.d"} {
		if err := os.MkdirAll(filepath.Join(root, d), 0755); err != nil {
			t.Fatal(err)
		}
	}
	f := &serviceFixture{dir: dir, path: filepath.Join(dir, "config"), e: Engine{RuntimeRoot: filepath.Join(dir, "state"),
		newPackages: func() modules.PackageManager { return &modules.APT{Root: root} }}}
	put(t, f.path, "A")
	if err := os.Chmod(f.path, 0644); err != nil {
		t.Fatal(err)
	}
	f.addService(t, "demo.service")
	return f
}
func (f *serviceFixture) addService(t *testing.T, name string) {
	t.Helper()
	dir := filepath.Join(f.dir, name)
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	for k, v := range map[string]string{"active": "active\n", "enabled": "enabled\n", "instance": "1\n", "loaded": "A", "actions": ""} {
		put(t, filepath.Join(dir, k), v)
	}
}
func (f *serviceFixture) op(content string) config.Task {
	return config.Task{TaskBlock: &config.TaskBlock{ID: "server", Changes: []config.Task{taskFile(f.path, content)}, Activate: []config.Task{{Module: "systemd_service", ModuleInputs: &modules.Service{State: "running", Name: "demo", Refresh: "reload"}}}}}
}
func (f *serviceFixture) run(t *testing.T, op config.Task, status string) {
	t.Helper()
	requireStatus(t, f.e.Run(context.Background(), job(op)), status)
}
func (f *serviceFixture) assert(t *testing.T, loaded, actions string) {
	t.Helper()
	if got := read(t, filepath.Join(f.dir, "demo.service/loaded")); got != loaded {
		t.Fatalf("loaded %q, want %q", got, loaded)
	}
	if got := read(t, filepath.Join(f.dir, "demo.service/actions")); got != actions {
		t.Fatalf("actions %q, want %q", got, actions)
	}
}

func TestServiceConfigurationAndHealthRecovery(t *testing.T) {
	f := newServiceFixture(t)
	f.run(t, f.op("A"), "succeeded")
	f.assert(t, "A", "")
	op := f.op("B")
	op.TaskBlock.Validate = []config.Task{{Module: "shell", ModuleInputs: &modules.Shell{Script: "exit 1"}}}
	f.run(t, op, "failed")
	if read(t, f.path) != "B" {
		t.Fatal("file change did not survive failure")
	}
	f.assert(t, "A", "")
	op.TaskBlock.Validate = []config.Task{{Module: "shell", ModuleInputs: &modules.Shell{Script: "true"}}}
	op.TaskBlock.Verify = []config.Task{{Module: "shell", ModuleInputs: &modules.Shell{Script: "exit 1", ChangedWhen: false}}}
	f.run(t, op, "failed")
	f.assert(t, "B", "reload\n")
	op.TaskBlock.Verify = nil
	f.run(t, op, "succeeded")
	f.assert(t, "B", "reload\n")
	f.run(t, f.op("A"), "succeeded")
	f.assert(t, "A", "reload\nreload\n")
}

func TestStoppedServiceAfterUnitRemoval(t *testing.T) {
	f := newServiceFixture(t)
	script := filepath.Join(f.dir, "bin/systemctl")
	writeProgramFileThroughShellChild(t, script, strings.ReplaceAll(read(t, script), "load=loaded", "load=not-found"))
	put(t, filepath.Join(f.dir, "demo.service/active"), "inactive")
	put(t, filepath.Join(f.dir, "demo.service/enabled"), "")
	disabled := false
	task := config.Task{Module: "systemd_service", ModuleInputs: &modules.Service{Name: "demo", State: "stopped", StartAtBoot: &disabled}}
	for _, dryrun := range []bool{true, false} {
		j := job(task)
		j.Dryrun = dryrun
		r := f.e.Run(context.Background(), j)
		requireStatus(t, r, "succeeded")
		if r.Change != "unchanged" {
			t.Fatal(r)
		}
	}
	f.assert(t, "A", "")
	// A cached process can still be stopped after its unit file is removed.
	put(t, filepath.Join(f.dir, "demo.service/active"), "active")
	f.run(t, task, "succeeded")
	f.assert(t, "A", "stop\n")
	config.InputsOf[modules.Service](task).State = "running"
	f.run(t, task, "failed")
	config.InputsOf[modules.Service](task).State, disabled = "stopped", true
	f.run(t, task, "failed")
	f.assert(t, "A", "stop\n")
}

// A decommission playbook stops and disables an account's unit, removes its
// files, and then the account. A rerun after the account is gone is
// unchanged: an account that does not exist has no unit that runs or is
// enabled. A task that starts, enables, or masks the unit, or installs a unit
// file, still needs the account, which an earlier node task may create.
func TestUnitOfAnAccountThatIsGone(t *testing.T) {
	f := newServiceFixture(t)
	yes, no := true, false
	gone := "mmx-test-account-that-is-gone"
	content := "[Service]\nExecStart=/bin/true\n"
	for _, test := range []struct {
		task                         config.Task
		runStatus, dryrunStatus, why string
	}{
		{config.Task{Module: "systemd_service", ModuleInputs: &modules.Service{Name: "report-worker", State: "stopped", StartAtBoot: &no, InSystemdOfUserAccount: gone}}, "succeeded", "succeeded", "account " + gone + " does not exist"},
		{config.Task{Module: "systemd_service", ModuleInputs: &modules.Service{Name: "report-worker", State: "stopped", InSystemdOfUserAccount: gone}}, "succeeded", "succeeded", "account " + gone + " does not exist"},
		{config.Task{Module: "systemd_service", ModuleInputs: &modules.Service{Name: "report-worker", State: "do_not_start_or_stop", StartAtBoot: &no, InSystemdOfUserAccount: gone}}, "succeeded", "succeeded", "account " + gone + " does not exist"},
		{config.Task{Module: "systemd_unit", ModuleInputs: &modules.Unit{Name: "report-worker.service", State: "absent", InSystemdOfUserAccount: gone}}, "succeeded", "succeeded", "account " + gone + " does not exist"},
		{config.Task{Module: "systemd_unit", ModuleInputs: &modules.Unit{Name: "report-worker.service", DropIn: "10-extra.conf", State: "absent", InSystemdOfUserAccount: gone}}, "succeeded", "succeeded", "account " + gone + " does not exist"},
		{config.Task{Module: "systemd_service", ModuleInputs: &modules.Service{State: "running", Name: "report-worker", InSystemdOfUserAccount: gone}}, "failed", "succeeded", "unknown user"},
		{config.Task{Module: "systemd_service", ModuleInputs: &modules.Service{Name: "report-worker", State: "stopped", StartAtBoot: &yes, InSystemdOfUserAccount: gone}}, "failed", "succeeded", "unknown user"},
		{config.Task{Module: "systemd_service", ModuleInputs: &modules.Service{Name: "report-worker", State: "stopped", StartAtBoot: &no, Masked: &yes, InSystemdOfUserAccount: gone}}, "failed", "succeeded", "unknown user"},
		{config.Task{Module: "systemd_unit", ModuleInputs: &modules.Unit{Name: "report-worker.service", Content: &content, InSystemdOfUserAccount: gone}}, "failed", "succeeded", "unknown user"},
	} {
		for _, dryrun := range []bool{true, false} {
			j := job(test.task)
			j.Dryrun = dryrun
			r := f.e.Run(context.Background(), j)
			got := r.Results[0]
			want, wantChange, wantStatus := test.runStatus, "unchanged", "succeeded"
			if dryrun {
				want = test.dryrunStatus
			}
			if want == "failed" || (dryrun && test.runStatus == "failed") {
				wantChange, wantStatus = "unknown", "failed"
				if dryrun {
					wantStatus = "not_checked"
				}
			}
			if r.Status != want || got.Status != wantStatus || got.Change != wantChange || !strings.Contains(got.Reason, test.why) ||
				(wantStatus == "failed" && got.FailureKind != "missing_prerequisite") {
				t.Errorf("%s %+v, dryrun %t: %s %+v", test.task.Module, test.task.ModuleInputs, dryrun, r.Status, got)
			}
		}
	}
	f.assert(t, "A", "")
}

func TestStopWaitsForDeactivatingService(t *testing.T) {
	f := newServiceFixture(t)
	active := filepath.Join(f.dir, "demo.service/active")
	put(t, active, "deactivating\n")
	j := job(config.Task{Module: "systemd_service", ModuleInputs: &modules.Service{Name: "demo", State: "stopped"}})
	j.Dryrun = true
	r := f.e.Run(context.Background(), j)
	requireStatus(t, r, "succeeded")
	if r.Change != "would_change" || read(t, active) != "deactivating\n" {
		t.Fatal("preview did not preserve the pending stop", r)
	}
	f.assert(t, "A", "")
	j.Dryrun = false
	r = f.e.Run(context.Background(), j)
	requireStatus(t, r, "succeeded")
	if r.Change != "changed" || read(t, active) != "inactive\n" {
		t.Fatal("service task completed before reaching the stopped state", r)
	}
	f.assert(t, "A", "stop\n")
	for _, state := range []string{"inactive", "failed"} {
		put(t, active, state+"\n")
		for _, dryrun := range []bool{true, false} {
			j.Dryrun = dryrun
			r = f.e.Run(context.Background(), j)
			requireStatus(t, r, "succeeded")
			if r.Change != "unchanged" {
				t.Fatalf("already stopped service (%s) requested work: %+v", state, r)
			}
			f.assert(t, "A", "stop\n")
		}
	}
}

// A start that systemd refuses, and a unit that is not running after the
// start or restart that MMX asked for, report the lines that the journal has
// of the unit since the action. A task that read a secret hides them with
// the rest of its reason.
func TestFailedServiceActionReportsTheUnitsJournalLines(t *testing.T) {
	f := newServiceFixture(t)
	journalctl := filepath.Join(f.dir, "bin/journalctl")
	writeProgramFileThroughShellChild(t, journalctl, "#!/bin/sh\ncat <<'ENTRIES'\n"+
		`{"__MONOTONIC_TIMESTAMP":"1","MESSAGE":"demo: a line from before the action"}`+"\n"+
		`{"__MONOTONIC_TIMESTAMP":"18446744073709551615","MESSAGE":"demo: cannot read /etc/demo.conf"}`+"\nENTRIES\n")
	lines := func(action string) string {
		return "\nLast lines of the systemd journal for demo.service during this " + action + ":\n  demo: cannot read /etc/demo.conf"
	}
	active, refuse := filepath.Join(f.dir, "demo.service/active"), filepath.Join(f.dir, "demo.service/fail")
	start := config.Task{Module: "systemd_service", ModuleInputs: &modules.Service{State: "running", Name: "demo"}}

	put(t, active, "inactive\n")
	put(t, refuse, "")
	r := f.e.Run(context.Background(), job(start))
	requireStatus(t, r, "failed")
	if got := r.Results[0].Reason; got != "exit status 1"+lines("start") {
		t.Fatalf("%q", got)
	}
	// The same failure of a task that read a secret.
	private := start
	private.When = map[string]any{"equals": []any{"{{ .secrets.start_demo }}", "yes"}}
	j := job(private)
	j.Scope = map[string]any{"secrets": map[string]any{"start_demo": value.Sensitive("yes")}}
	r = f.e.Run(context.Background(), j)
	requireStatus(t, r, "failed")
	if b, _ := json.Marshal(r); !r.Results[0].Secret || strings.Contains(string(b), "demo.conf") || !strings.Contains(string(b), "details suppressed for a task using secrets") {
		t.Fatalf("%s", b)
	}

	// systemd accepts the start, and the unit has failed when MMX looks.
	if err := os.Remove(refuse); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(f.dir, "bin/systemctl")
	writeProgramFileThroughShellChild(t, script, strings.ReplaceAll(read(t, script), "start|restart) echo active", "start|restart) echo failed"))
	r = f.e.Run(context.Background(), job(start))
	requireStatus(t, r, "failed")
	if got := r.Results[0].Reason; got != "service demo.service did not reach state running (ActiveState=failed)"+lines("start") {
		t.Fatalf("%q", got)
	}
	// do_not_start_or_stop restarts a running unit for a changed file.
	put(t, active, "active\n")
	block := f.op("B")
	block.TaskBlock.Activate = []config.Task{{Module: "systemd_service", ModuleInputs: &modules.Service{Name: "demo", State: "do_not_start_or_stop"}}}
	r = f.e.Run(context.Background(), job(block))
	requireStatus(t, r, "failed")
	if got := r.Results[len(r.Results)-1].Reason; got != "server/activate/task_1: service demo.service is not running after its restart (ActiveState=failed)"+lines("restart") {
		t.Fatalf("%q", got)
	}
	// A unit that does not stop gets no journal lines: MMX asked for no start.
	writeProgramFileThroughShellChild(t, script, strings.ReplaceAll(read(t, script), "stop) echo inactive", "stop) echo active"))
	put(t, active, "active\n")
	r = f.e.Run(context.Background(), job(config.Task{Module: "systemd_service", ModuleInputs: &modules.Service{Name: "demo", State: "stopped"}}))
	requireStatus(t, r, "failed")
	if got := r.Results[0].Reason; got != "service demo.service did not reach state stopped (ActiveState=active)" {
		t.Fatalf("%q", got)
	}
}

func TestUnconfirmedReloadCannotMistakeRestoredFileForLoadedFile(t *testing.T) {
	f := newServiceFixture(t)
	f.run(t, f.op("A"), "succeeded")
	f.e.BeforeRecord = func(phase string) error {
		if phase == "confirmed" {
			return errors.New("lost completion")
		}
		return nil
	}
	f.run(t, f.op("B"), "failed")
	f.assert(t, "B", "reload\n")
	f.e.BeforeRecord = nil
	f.run(t, f.op("A"), "succeeded")
	f.assert(t, "A", "reload\nreload\n")
}

func TestServiceRecordWritesPrecedeTheirEffects(t *testing.T) {
	for _, phase := range []string{"baseline", "unconfirmed"} {
		t.Run(phase, func(t *testing.T) {
			f := newServiceFixture(t)
			f.e.BeforeRecord = func(at string) error {
				if at == phase {
					return errors.New("disk full")
				}
				return nil
			}
			f.run(t, f.op("B"), "failed")
			f.assert(t, "A", "")
			want := "A"
			if phase == "unconfirmed" {
				want = "B"
			}
			if read(t, f.path) != want {
				t.Fatal("write boundary did not protect its effect")
			}
			f.e.BeforeRecord = nil
			f.run(t, f.op("B"), "succeeded")
			f.assert(t, "B", "reload\n")
		})
	}
}

func TestMissingRecordAndNewInstanceDoNotCauseExtraReload(t *testing.T) {
	f := newServiceFixture(t)
	f.run(t, f.op("B"), "succeeded")
	f.assert(t, "B", "reload\n")
	if err := os.Remove(f.e.servicePath("demo.service")); err != nil {
		t.Fatal(err)
	}
	put(t, filepath.Join(f.dir, "demo.service/instance"), "2\n")
	f.run(t, f.op("B"), "succeeded")
	f.assert(t, "B", "reload\n")
	put(t, filepath.Join(f.dir, "demo.service/instance"), "3\n")
	f.run(t, f.op("B"), "succeeded")
	f.assert(t, "B", "reload\n")
}

func TestStoppedServiceStartsOnceAndRuntimeEnableIsRemoved(t *testing.T) {
	f := newServiceFixture(t)
	put(t, filepath.Join(f.dir, "demo.service/active"), "inactive\n")
	put(t, filepath.Join(f.dir, "demo.service/enabled"), "enabled-runtime\n")
	op := f.op("B")
	disabled := false
	config.InputsOf[modules.Service](op.TaskBlock.Activate[0]).StartAtBoot = &disabled
	f.run(t, op, "succeeded")
	f.run(t, op, "succeeded")
	f.assert(t, "B", "disable\nstart\n")
}

func TestIndependentServiceCompletionAndAliases(t *testing.T) {
	f := newServiceFixture(t)
	f.addService(t, "second.service")
	op := f.op("B")
	op.TaskBlock.Activate = append(op.TaskBlock.Activate, config.Task{Module: "systemd_service", ModuleInputs: &modules.Service{State: "running", Name: "second", Refresh: "reload"}})
	put(t, filepath.Join(f.dir, "second.service/fail"), "")
	f.run(t, op, "failed")
	f.assert(t, "B", "reload\n")
	if err := os.Remove(filepath.Join(f.dir, "second.service/fail")); err != nil {
		t.Fatal(err)
	}
	config.InputsOf[modules.Service](op.TaskBlock.Activate[0]).Name = "alias"
	f.run(t, op, "succeeded")
	f.assert(t, "B", "reload\n")
	if read(t, filepath.Join(f.dir, "second.service/loaded")) != "B" {
		t.Fatal("second service not recovered")
	}
}

func TestCandidateGuardCannotSkipRequiredValidation(t *testing.T) {
	f := newServiceFixture(t)
	op := f.op("B")
	op.TaskBlock.Changes = append([]config.Task{taskFile(filepath.Join(f.dir, "another"), "changed")}, op.TaskBlock.Changes...)
	config.InputsOf[modules.File](op.TaskBlock.Changes[1]).CheckNewFile = &modules.Check{Shell: &modules.Shell{Script: "true", OnlyIf: &modules.Guard{Script: "exit 1"}}}
	f.run(t, op, "failed")
	if read(t, f.path) != "A" {
		t.Fatal("rejected candidate replaced live file")
	}
	f.assert(t, "A", "")
}

func TestTaskBlockValidatorDoesNotReportAChange(t *testing.T) {
	f := newServiceFixture(t)
	op := f.op("A")
	op.TaskBlock.Validate = []config.Task{{Module: "shell", ModuleInputs: &modules.Shell{Script: "true"}}}
	r := f.e.Run(context.Background(), job(op))
	requireStatus(t, r, "succeeded")
	for _, result := range r.Results {
		if result.Change != "unchanged" {
			t.Fatal(result)
		}
	}
	f.assert(t, "A", "")
}

func TestServiceDryrunDoesNotWriteRecordsOrConfirmOldState(t *testing.T) {
	f := newServiceFixture(t)
	j := job(f.op("B"))
	j.Dryrun = true
	r := f.e.Run(context.Background(), j)
	requireStatus(t, r, "succeeded")
	if r.Results[1].Change != "would_change" {
		t.Fatal(r)
	}
	if _, err := os.Stat(f.e.servicePath("demo.service")); !os.IsNotExist(err) {
		t.Fatal("dryrun created a baseline")
	}
	if read(t, f.path) != "A" {
		t.Fatal("dryrun wrote configuration")
	}
	f.assert(t, "A", "")
	f.run(t, f.op("A"), "succeeded")
	record := read(t, f.e.servicePath("demo.service"))
	requireStatus(t, f.e.Run(context.Background(), j), "succeeded")
	if read(t, f.e.servicePath("demo.service")) != record {
		t.Fatal("dryrun changed record")
	}
}

func TestPackageVersionRequiresRestart(t *testing.T) {
	f := newServiceFixture(t)
	put(t, filepath.Join(f.dir, "package-demo"), "1\tinstalled\n")
	op := f.op("A")
	op.TaskBlock.Changes = append(op.TaskBlock.Changes, config.Task{Module: "apt_package", ModuleInputs: &modules.Package{Manager: "apt", Packages: []modules.PackageEntry{{Name: "demo", Version: "newest_when_first_installed"}}}})
	f.run(t, op, "succeeded")
	f.assert(t, "A", "")
	put(t, filepath.Join(f.dir, "package-demo"), "2\tinstalled\n")
	f.run(t, op, "succeeded")
	f.assert(t, "A", "restart\n")
	f.run(t, op, "succeeded")
	f.assert(t, "A", "restart\n")
}

// A package manager that upgrades a tracked package and restarts its service
// between MMX runs started the new version: the next run takes a fresh
// baseline for that instance and does not restart it again.
func TestOutsideUpgradeWithRestartNeedsNoAction(t *testing.T) {
	f := newServiceFixture(t)
	put(t, filepath.Join(f.dir, "package-demo"), "1\tinstalled\n")
	op := f.op("A")
	op.TaskBlock.Changes = append(op.TaskBlock.Changes, config.Task{Module: "apt_package", ModuleInputs: &modules.Package{Manager: "apt", Packages: []modules.PackageEntry{{Name: "demo", Version: "newest_when_first_installed"}}}})
	f.run(t, op, "succeeded")
	f.assert(t, "A", "")
	put(t, filepath.Join(f.dir, "package-demo"), "2\tinstalled\n")
	put(t, filepath.Join(f.dir, "demo.service/instance"), "9\n")
	f.run(t, op, "succeeded")
	f.run(t, op, "succeeded")
	f.assert(t, "A", "")
}

func TestConfigurationHashTracksBytesAndPaths(t *testing.T) {
	dir := t.TempDir()
	a := taskFile(filepath.Join(dir, "a"), "")
	b := taskFile(filepath.Join(dir, "b"), "")
	put(t, config.InputsOf[modules.File](a).Path, "ab")
	put(t, config.InputsOf[modules.File](b).Path, "c")
	h := func(ts ...config.Task) string {
		t.Helper()
		var tracked []modules.TrackedInput
		for _, task := range ts {
			tracked = append(tracked, config.TrackedInputs(task)...)
		}
		v, err := configurationHash(context.Background(), tracked, nil)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	initial := h(a, b)
	if h(b, a) != initial {
		t.Fatal("order changed hash")
	}
	put(t, config.InputsOf[modules.File](a).Path, "a")
	put(t, config.InputsOf[modules.File](b).Path, "bc")
	if h(a, b) == initial {
		t.Fatal("ambiguous concatenation")
	}
	put(t, config.InputsOf[modules.File](a).Path, "")
	empty := h(a)
	if err := os.Remove(config.InputsOf[modules.File](a).Path); err != nil {
		t.Fatal(err)
	}
	if h(a) == empty {
		t.Fatal("absence matched empty file")
	}
	if err := os.Symlink(config.InputsOf[modules.File](b).Path, config.InputsOf[modules.File](a).Path); err != nil {
		t.Fatal(err)
	}
	if _, err := configurationHash(context.Background(), config.TrackedInputs(a), nil); err == nil {
		t.Fatal("accepted symlink")
	}
}

func TestCorruptRecordAndDuplicateUnitFailBeforeFileChanges(t *testing.T) {
	for _, problem := range []string{"corrupt", "duplicate"} {
		t.Run(problem, func(t *testing.T) {
			f := newServiceFixture(t)
			f.run(t, f.op("A"), "succeeded")
			op := f.op("B")
			if problem == "corrupt" {
				put(t, f.e.servicePath("demo.service"), "{")
			} else {
				op.TaskBlock.Activate = append(op.TaskBlock.Activate, config.Task{Module: "systemd_service", ModuleInputs: &modules.Service{State: "running", Name: "alias"}})
			}
			f.run(t, op, "failed")
			if read(t, f.path) != "A" {
				t.Fatal("invalid preflight allowed file change")
			}
			f.assert(t, "A", "")
		})
	}
}

// Only tracked node tasks are fixed as a service task block starts. Other node tasks in
// its stages, loops included, run in written order, so an activate loop sees an
// assignment or result made earlier in activate.
func TestServiceTaskBlockRunsUntrackedLoopsInWrittenOrder(t *testing.T) {
	f := newServiceFixture(t)
	out := filepath.Join(f.dir, "out")
	if err := os.Mkdir(out, 0700); err != nil {
		t.Fatal(err)
	}
	written := func(dir string) config.Task {
		return config.Task{Module: "file", ModuleInputs: &modules.File{Mode: "0644", Path: dir + "/{{ .n }}", Content: text("y")}, Loop: "{{ .vars.names }}", As: "n"}
	}
	op := f.op("A")
	op.TaskBlock.Activate = append([]config.Task{{SetVars: map[string]any{"names": []any{"new"}}}, written(out)}, op.TaskBlock.Activate...)
	j := job(op)
	j.Vars = map[string]any{"names": []any{"old"}}
	requireStatus(t, f.e.Run(context.Background(), j), "succeeded")
	if entries, err := os.ReadDir(out); err != nil || len(entries) != 1 || entries[0].Name() != "new" {
		t.Fatal("activate loop did not use the written-order assignment", entries, err)
	}
	loop := written(out)
	loop.Loop = "{{ .node_tasks.list.value }}"
	op = f.op("A")
	op.TaskBlock.Activate = append([]config.Task{{ID: "list", Show: []any{"listed"}}, loop}, op.TaskBlock.Activate...)
	requireStatus(t, f.e.Run(context.Background(), job(op)), "succeeded")
	if _, err := os.Stat(filepath.Join(out, "listed")); err != nil {
		t.Fatal("activate loop over an earlier result did not run", err)
	}
}

func TestUnchangedFileStillRequiresItsValidator(t *testing.T) {
	f := newServiceFixture(t)
	f.run(t, f.op("A"), "succeeded")
	put(t, f.path, "B")
	op := f.op("B")
	config.InputsOf[modules.File](op.TaskBlock.Changes[0]).CheckNewFile = &modules.Check{Shell: &modules.Shell{Script: "exit 1"}}
	r := f.e.Run(context.Background(), job(op))
	requireStatus(t, r, "failed")
	if !strings.Contains(r.Error, "check_new_file") {
		t.Fatal(r.Error)
	}
	f.assert(t, "A", "")
}

func TestSkippedPackageDoesNotPredictRestart(t *testing.T) {
	f := newServiceFixture(t)
	put(t, filepath.Join(f.dir, "package-demo"), "1\tinstalled\n")
	op := f.op("A")
	op.TaskBlock.Changes = append(op.TaskBlock.Changes, config.Task{When: false, Module: "apt_package", ModuleInputs: &modules.Package{Manager: "apt", Packages: []modules.PackageEntry{{Name: "demo", Version: "2"}}}})
	j := job(op)
	j.Dryrun = true
	r := f.e.Run(context.Background(), j)
	requireStatus(t, r, "succeeded")
	if r.Results[2].Change != "unchanged" {
		t.Fatal("skipped package caused a predicted restart", r)
	}
}

func TestPartialPackageAndPinnedVersionRequireWork(t *testing.T) {
	for _, state := range []string{"unpacked", "half-configured", "installed"} {
		t.Run(state, func(t *testing.T) {
			f := newServiceFixture(t)
			put(t, filepath.Join(f.dir, "package-demo"), "1\t"+state+"\n")
			reached := false
			f.e.BeforeMutation = func(string) error { reached = true; return errors.New("before package mutation") }
			p := modules.Package{Manager: "apt", Packages: []modules.PackageEntry{{Name: "demo", Version: "newest_when_first_installed"}}}
			writeProgramFileThroughShellChild(t, filepath.Join(f.dir, "bin/apt-get"), "#!/bin/sh\nexit 0\n")
			if state == "installed" {
				p.Packages[0].Version = "2"
				// The index offers version 2.
				for name, script := range map[string]string{"apt-cache": "echo ' demo | 2 | https://deb.example stable/main amd64 Packages'\n",
					"dpkg": "[ \"$2\" \\> \"$4\" ]\n"} {
					writeProgramFileThroughShellChild(t, filepath.Join(f.dir, "bin", name), "#!/bin/sh\n"+script)
				}
			}
			r := f.e.runtime().Package(context.Background(), &modules.APT{Root: filepath.Join(f.dir, "root")}, p)
			if r.Status != "failed" || !reached {
				t.Fatal("incorrectly treated package as already installed", r)
			}
		})
	}
}

func TestServiceInstanceChangeDuringWritesCannotHideChanges(t *testing.T) {
	f := newServiceFixture(t)
	f.e.BeforeMutation = func(resource string) error {
		if resource == f.path {
			put(t, filepath.Join(f.dir, "demo.service/instance"), "2\n")
		}
		return nil
	}
	f.run(t, f.op("B"), "succeeded")
	f.assert(t, "B", "reload\n")
}

func TestServiceCompletesPendingManagerReload(t *testing.T) {
	f := newServiceFixture(t)
	pending := filepath.Join(f.dir, "demo.service/need-reload")
	put(t, pending, "yes")
	task := config.Task{Module: "systemd_service", ModuleInputs: &modules.Service{State: "running", Name: "demo"}}
	preview := job(task)
	preview.Dryrun = true
	r := f.e.Run(context.Background(), preview)
	requireStatus(t, r, "succeeded")
	if r.Change != "would_change" || read(t, pending) != "yes" {
		t.Fatal(r)
	}
	r = f.e.Run(context.Background(), job(task))
	requireStatus(t, r, "succeeded")
	if r.Change != "changed" || strings.TrimSpace(read(t, pending)) != "no" {
		t.Fatal(r)
	}
	r = f.e.Run(context.Background(), job(task))
	if r.Change != "unchanged" {
		t.Fatal(r)
	}
}

func TestRecursivePermissionsDoNotReloadService(t *testing.T) {
	f := newServiceFixture(t)
	tree := filepath.Join(t.TempDir(), "data")
	if err := os.Mkdir(tree, 0755); err != nil {
		t.Fatal(err)
	}
	put(t, filepath.Join(tree, "file"), "application data")
	if err := os.Chmod(filepath.Join(tree, "file"), 0644); err != nil {
		t.Fatal(err)
	}
	op := f.op("A")
	op.TaskBlock.Changes = append(op.TaskBlock.Changes, config.Task{Module: "directory", ModuleInputs: &modules.Directory{Path: tree, Recurse: true, FileMode: "0600"}})
	op.TaskBlock.Activate[0].ID = "reload"
	for _, dryrun := range []bool{true, false, false} {
		j := job(op)
		j.Dryrun = dryrun
		r := f.e.Run(context.Background(), j)
		requireStatus(t, r, "succeeded")
		found := false
		for _, activation := range r.Results {
			if activation.Task == "server/activate/reload" {
				found = true
				if activation.Change != "unchanged" {
					t.Fatalf("metadata caused service activation (dryrun=%v): %+v", dryrun, activation)
				}
			}
		}
		if !found {
			t.Fatal("missing service result", r.Results)
		}
		f.assert(t, "A", "")
		st, err := os.Stat(filepath.Join(tree, "file"))
		want := os.FileMode(0600)
		if dryrun {
			want = 0644
		}
		if err != nil || st.Mode().Perm() != want {
			t.Fatal("unexpected file mode", st, err)
		}
	}
}

// A resolved value is literal data: a tracked path whose value contains "{{"
// is hashed and written at that literal path, so its change reloads once.
func TestServiceTracksTheLiteralPathItWrites(t *testing.T) {
	f := newServiceFixture(t)
	literal := filepath.Join(f.dir, "{{ .vars.x }}")
	for _, dir := range []string{literal, filepath.Join(f.dir, "sub")} {
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	// The fake service loads f.path; point it at the literal destination.
	put(t, filepath.Join(literal, "config"), "A")
	if err := os.Remove(f.path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(literal, "config"), f.path); err != nil {
		t.Fatal(err)
	}
	op := f.op("B")
	config.InputsOf[modules.File](op.TaskBlock.Changes[0]).Path = "{{ .vars.dir }}/config"
	j := job(op)
	j.Vars = map[string]any{"dir": literal, "x": "sub"}
	for range 2 {
		requireStatus(t, f.e.Run(context.Background(), j), "succeeded")
		if read(t, filepath.Join(literal, "config")) != "B" {
			t.Fatal("did not write the literal path")
		}
		if _, err := os.Stat(filepath.Join(f.dir, "sub", "config")); !os.IsNotExist(err) {
			t.Fatal("resolved the path's value again", err)
		}
		f.assert(t, "B", "reload\n")
	}
}

// Dryrun predicts each tracked file with the scope it has when its own task
// runs; a later assignment before the service does not change the prediction.
// The real run then does what dryrun predicted.
func TestServiceDryrunUsesEachFileNodeTasksScope(t *testing.T) {
	for _, tc := range []struct {
		name, before, after string
		when                any
		actions             string
	}{
		{"changed then reassigned", "B", "A", nil, "reload\n"},
		{"unchanged then reassigned", "A", "B", nil, ""},
		{"condition reassigned", "B", "B", "{{ .vars.apply }}", "reload\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newServiceFixture(t)
			f.run(t, f.op("A"), "succeeded")
			op := f.op("{{ .vars.conf }}")
			op.TaskBlock.Changes[0].When = tc.when
			op.TaskBlock.Activate = append([]config.Task{{SetVars: map[string]any{"conf": tc.after, "apply": false}}}, op.TaskBlock.Activate...)
			j := job(op)
			j.Vars = map[string]any{"conf": tc.before, "apply": true}
			j.Dryrun = true
			r := f.e.Run(context.Background(), j)
			requireStatus(t, r, "succeeded")
			want := "unchanged"
			if tc.actions != "" {
				want = "would_change"
			}
			if got := r.Results[len(r.Results)-2].Change; got != want {
				t.Fatalf("predicted %s, want %s: %+v", got, want, r.Results)
			}
			j.Dryrun = false
			requireStatus(t, f.e.Run(context.Background(), j), "succeeded")
			f.assert(t, tc.before, tc.actions)
		})
	}
}

// Identifiers must not be secret inside a service task block either, and
// private content fixed there still marks its task secret.
func TestServiceTaskBlockKeepsSecretRules(t *testing.T) {
	f := newServiceFixture(t)
	op := f.op("{{ .secrets.password }}")
	config.InputsOf[modules.File](op.TaskBlock.Changes[0]).Mode = "0600"
	j := job(op)
	j.Scope = map[string]any{"secrets": map[string]any{"password": value.Sensitive("task_block-secret")}}
	r := f.e.Run(context.Background(), j)
	requireStatus(t, r, "succeeded")
	if !r.Results[0].Secret || r.Results[1].Change != "changed" {
		t.Fatal("private content did not mark its task secret", r.Results)
	}
	f.assert(t, "task_block-secret", "reload\n")
	config.InputsOf[modules.File](op.TaskBlock.Changes[0]).Path = f.dir + "/{{ .secrets.password }}"
	j.NodeTasks = []config.Task{op}
	r = f.e.Run(context.Background(), j)
	requireStatus(t, r, "failed")
	b, _ := json.Marshal(r)
	if _, err := os.Stat(filepath.Join(f.dir, "task_block-secret")); !os.IsNotExist(err) || strings.Contains(string(b), "task_block-secret") {
		t.Fatal("secret identifier accepted or leaked", r.Error)
	}
}

// A service still starting or reloading when its task block begins is running:
// it gets a baseline, so an unchanged run after reboot refreshes nothing. A
// unit that still starts when its service task runs is not started yet: the
// task waits for the start with systemctl start.
func TestStartingOrReloadingServiceGetsABaseline(t *testing.T) {
	for _, entry := range []string{"activating", "reloading"} {
		for _, next := range []string{"active", ""} {
			t.Run(entry+"-then-"+next, func(t *testing.T) {
				f := newServiceFixture(t)
				put(t, filepath.Join(f.dir, "demo.service/active"), entry)
				if next != "" {
					put(t, filepath.Join(f.dir, "demo.service/next-active"), next)
				}
				f.run(t, f.op("A"), "succeeded")
				actions := ""
				if entry == "activating" && next == "" {
					actions = "start\n"
				}
				f.assert(t, "A", actions)
			})
		}
	}
}

// systemd reports a unit as activating while it starts, and also between
// the tries of a unit with Restart= whose start failed. Such a unit is not
// started: state: running waits for the start and fails when the start
// fails, so another try after a failed start cannot report success.
func TestStartedWaitsForAUnitThatStillStarts(t *testing.T) {
	task := config.Task{Module: "systemd_service", ModuleInputs: &modules.Service{State: "running", Name: "demo"}}
	t.Run("the start succeeds", func(t *testing.T) {
		f := newServiceFixture(t)
		active := filepath.Join(f.dir, "demo.service/active")
		put(t, active, "activating\n")
		j := job(task)
		j.Dryrun = true
		r := f.e.Run(context.Background(), j)
		requireStatus(t, r, "succeeded")
		if r.Change != "would_change" || r.Results[0].Reason != "service is still starting" || read(t, active) != "activating\n" {
			t.Fatalf("dryrun of a unit that still starts: %+v", r.Results)
		}
		f.assert(t, "A", "")
		if r := f.e.Run(context.Background(), job(task)); r.Status != "succeeded" || r.Results[0].Reason != "service was still starting; started" {
			t.Fatalf("run of a unit that still starts: %+v", r)
		}
		f.assert(t, "A", "start\n")
		f.run(t, task, "succeeded")
		f.assert(t, "A", "start\n")
	})
	t.Run("the start fails", func(t *testing.T) {
		f := newServiceFixture(t)
		put(t, filepath.Join(f.dir, "demo.service/active"), "activating\n")
		put(t, filepath.Join(f.dir, "demo.service/fail"), "")
		for range 2 {
			f.run(t, task, "failed")
		}
		f.assert(t, "A", "start\nstart\n")
	})
	// A start that systemctl reports as done and a unit that is activating
	// right after it: the unit ended at once and systemd starts it again.
	t.Run("the unit ends at once after its start", func(t *testing.T) {
		for _, state := range []string{"running", "do_not_start_or_stop"} {
			f := newServiceFixture(t)
			put(t, filepath.Join(f.dir, "demo.service/active-after-start"), "activating\n")
			block := f.op("B")
			service := config.InputsOf[modules.Service](block.TaskBlock.Activate[0])
			service.State, service.Refresh = state, "restart"
			r := f.e.Run(context.Background(), job(block))
			requireStatus(t, r, "failed")
			if !strings.Contains(r.Error, "ActiveState=activating") {
				t.Fatalf("state %s: %s", state, r.Error)
			}
		}
	})
}

// An outside reload that began before MMX's write cannot confirm the new
// configuration; the service task refreshes after it.
func TestReloadInProgressDoesNotConfirmNewConfiguration(t *testing.T) {
	f := newServiceFixture(t)
	f.e.BeforeMutation = func(resource string) error {
		if resource == f.path {
			put(t, filepath.Join(f.dir, "demo.service/active"), "reloading")
		}
		return nil
	}
	f.run(t, f.op("B"), "succeeded")
	f.assert(t, "B", "reload\n")
}

// Operations in different fleet stages can manage one service with different inputs.
// Each compares only its own inputs, so unchanged runs take no action.
func TestServiceManagedByFleetStagesWithDifferentInputs(t *testing.T) {
	f := newServiceFixture(t)
	put(t, filepath.Join(f.dir, "package-demo"), "1\tinstalled\n")
	site := filepath.Join(f.dir, "site")
	put(t, site, "S")
	first := f.op("A")
	first.TaskBlock.Changes = append(first.TaskBlock.Changes, config.Task{Module: "apt_package", ModuleInputs: &modules.Package{Manager: "apt", Packages: []modules.PackageEntry{{Name: "demo", Version: "newest_when_first_installed"}}}})
	second := config.Task{TaskBlock: &config.TaskBlock{ID: "sites", Changes: []config.Task{taskFile(site, "S")}, Activate: []config.Task{{Module: "systemd_service", ModuleInputs: &modules.Service{State: "running", Name: "demo", Refresh: "reload"}}}}}
	fleetStages := func() {
		t.Helper()
		for range 2 {
			f.run(t, first, "succeeded")
			f.run(t, second, "succeeded")
		}
	}
	fleetStages()
	f.assert(t, "A", "")
	second.TaskBlock.Changes[0] = taskFile(site, "T")
	fleetStages()
	f.assert(t, "A", "reload\n")
	put(t, filepath.Join(f.dir, "package-demo"), "2\tinstalled\n")
	fleetStages()
	f.assert(t, "A", "reload\nrestart\n")
}

// Two task blocks in one job may also give one service different inputs.
func TestOneJobManagesAServiceWithDifferentInputs(t *testing.T) {
	f := newServiceFixture(t)
	put(t, filepath.Join(f.dir, "package-demo"), "1\tinstalled\n")
	site := filepath.Join(f.dir, "site")
	put(t, site, "S")
	first := f.op("A")
	first.TaskBlock.Changes = append(first.TaskBlock.Changes, config.Task{Module: "apt_package", ModuleInputs: &modules.Package{Manager: "apt", Packages: []modules.PackageEntry{{Name: "demo", Version: "newest_when_first_installed"}}}})
	second := config.Task{TaskBlock: &config.TaskBlock{ID: "sites", Changes: []config.Task{taskFile(site, "S")}, Activate: []config.Task{{Module: "systemd_service", ModuleInputs: &modules.Service{State: "running", Name: "demo", Refresh: "reload"}}}}}
	both := func() {
		t.Helper()
		for range 2 {
			requireStatus(t, f.e.Run(context.Background(), job(first, second)), "succeeded")
		}
	}
	both()
	f.assert(t, "A", "")
	second.TaskBlock.Changes[0] = taskFile(site, "T")
	both()
	f.assert(t, "A", "reload\n")
	first.TaskBlock.Changes[0] = taskFile(f.path, "B")
	both()
	f.assert(t, "B", "reload\nreload\n")
	put(t, filepath.Join(f.dir, "package-demo"), "2\tinstalled\n")
	both()
	f.assert(t, "B", "reload\nreload\nrestart\n")
}

// twoBlocks is one service in two task blocks, as a deployment has it: one
// block writes the service's configuration and reloads the service, the other
// writes the service's definition and starts or restarts it.
type twoBlocks struct {
	f                *serviceFixture
	unit, definition string
}

func newTwoBlocks(t *testing.T, service string) twoBlocks {
	f := newServiceFixture(t)
	return twoBlocks{f: f, unit: filepath.Join(f.dir, service+".service"), definition: filepath.Join(f.dir, "definition")}
}

// configuration is the block that writes the configuration and names the
// service in the given state with refresh: reload.
func (b twoBlocks) configuration(content, state string) config.Task {
	return config.Task{TaskBlock: &config.TaskBlock{ID: "configuration", Changes: []config.Task{taskFile(b.f.path, content)},
		Activate: []config.Task{{Module: "systemd_service", ModuleInputs: &modules.Service{Name: filepath.Base(b.unit), State: state, Refresh: "reload"}}}}}
}

// install is the block that installs the unit where it is missing, stopped,
// writes the definition, and names the service started with refresh: restart.
func (b twoBlocks) install(definition string) config.Task {
	missing := "[ -d " + b.unit + " ] || { mkdir " + b.unit + " && cd " + b.unit + " && echo inactive > active && echo disabled > enabled && : > instance && : > loaded && : > actions; }"
	return config.Task{TaskBlock: &config.TaskBlock{ID: "install", Changes: []config.Task{{Module: "shell", ModuleInputs: &modules.Shell{Script: missing, ChangedWhen: false}}, taskFile(b.definition, definition)},
		Activate: []config.Task{{Module: "systemd_service", ModuleInputs: &modules.Service{State: "running", Name: filepath.Base(b.unit)}}}}}
}

func (b twoBlocks) run(t *testing.T, status string, blocks ...config.Task) {
	t.Helper()
	requireStatus(t, b.f.e.Run(context.Background(), job(blocks...)), status)
}

// assert checks what the service has loaded and every action it was given.
func (b twoBlocks) assert(t *testing.T, loaded, actions string) {
	t.Helper()
	if got := read(t, filepath.Join(b.unit, "loaded")); got != loaded {
		t.Fatalf("loaded %q, want %q", got, loaded)
	}
	if got := read(t, filepath.Join(b.unit, "actions")); got != actions {
		t.Fatalf("actions %q, want %q", got, actions)
	}
}

// The first block refreshes the service only where it runs (do_not_start_or_stop), and
// the second block installs the unit and starts it. The start loads the
// configuration that the first block wrote, so an unchanged run does nothing;
// a changed configuration then reloads once.
func TestStartInALaterTaskBlockLoadsTheConfigurationOfAnEarlierOne(t *testing.T) {
	for _, stopped := range []bool{false, true} {
		t.Run(map[bool]string{false: "one run", true: "run stopped between the blocks"}[stopped], func(t *testing.T) {
			b := newTwoBlocks(t, "late")
			if stopped {
				b.run(t, "succeeded", b.configuration("B", "do_not_start_or_stop"))
			}
			b.run(t, "succeeded", b.configuration("B", "do_not_start_or_stop"), b.install("D"))
			b.assert(t, "B", "start\n")
			dryrun := job(b.configuration("B", "do_not_start_or_stop"), b.install("D"))
			dryrun.Dryrun = true
			for _, r := range b.f.e.Run(context.Background(), dryrun).Results {
				if strings.Contains(r.Task, "/activate/") && r.Change != "unchanged" {
					t.Errorf("dryrun of the unchanged blocks predicts %s: %s", r.Change, r.Reason)
				}
			}
			b.run(t, "succeeded", b.configuration("B", "do_not_start_or_stop"), b.install("D"))
			b.assert(t, "B", "start\n")
			for range 2 {
				b.run(t, "succeeded", b.configuration("C", "do_not_start_or_stop"), b.install("D"))
				b.assert(t, "C", "start\nreload\n")
			}
		})
	}
}

// Both blocks name a unit that exists. The second block's definition was not
// there when the service last started, so the second block restarts it. The
// restart loads the configuration too: an unchanged run does nothing, a
// changed configuration reloads once, and a changed definition restarts once.
func TestRestartInALaterTaskBlockLoadsTheConfigurationOfAnEarlierOne(t *testing.T) {
	for _, tc := range []struct {
		name, state, actions string
		running              bool
	}{
		{"both blocks say running and the unit is stopped", "running", "start\nrestart\n", false},
		{"the first block says do_not_start_or_stop and the unit runs", "do_not_start_or_stop", "reload\nrestart\n", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := newTwoBlocks(t, "demo")
			if !tc.running {
				put(t, filepath.Join(b.unit, "active"), "inactive\n")
			}
			actions := tc.actions
			for _, step := range []struct{ configuration, definition, action string }{{"B", "D", ""}, {"C", "D", "reload\n"}, {"C", "E", "restart\n"}} {
				actions += step.action
				for range 2 {
					b.run(t, "succeeded", b.configuration(step.configuration, tc.state), b.install(step.definition))
					b.assert(t, step.configuration, actions)
				}
			}
		})
	}
}

// A block wrote the configuration and ended before its activate phase, so a
// reload is owed. Another block's start or restart of the service, in a run
// that does not hold the first block (another fleet stage), loads the
// configuration: nothing is owed after it.
func TestStartOrRestartByAnotherTaskBlockLoadsAnOwedConfiguration(t *testing.T) {
	for _, action := range []string{"start", "restart"} {
		t.Run(action, func(t *testing.T) {
			b := newTwoBlocks(t, "demo")
			actions := ""
			if action == "start" {
				put(t, filepath.Join(b.unit, "active"), "inactive\n")
			} else {
				b.run(t, "succeeded", b.configuration("A", "running"), b.install("D"))
				actions = "restart\n"
			}
			rejected := b.configuration("B", "running")
			rejected.TaskBlock.Validate = []config.Task{{Module: "shell", ModuleInputs: &modules.Shell{Script: "exit 1"}}}
			b.run(t, "failed", rejected)
			b.assert(t, "A", actions)
			b.run(t, "succeeded", b.install("E"))
			actions += action + "\n"
			b.assert(t, "B", actions)
			for range 2 {
				b.run(t, "succeeded", b.configuration("B", "running"), b.install("E"))
				b.assert(t, "B", actions)
			}
		})
	}
}

// What is still owed after a run that ended early happens on the next run,
// once: a reload when the first block ended before its activate phase, and a
// restart when the second block's restart failed.
func TestOwedRefreshOfOneOfTwoTaskBlocksHappensOnTheNextRun(t *testing.T) {
	b := newTwoBlocks(t, "late")
	b.run(t, "succeeded", b.configuration("B", "do_not_start_or_stop"), b.install("D"))
	b.assert(t, "B", "start\n")
	rejected := b.configuration("C", "do_not_start_or_stop")
	rejected.TaskBlock.Validate = []config.Task{{Module: "shell", ModuleInputs: &modules.Shell{Script: "exit 1"}}}
	b.run(t, "failed", rejected, b.install("D"))
	b.assert(t, "B", "start\n")
	for range 2 {
		b.run(t, "succeeded", b.configuration("C", "do_not_start_or_stop"), b.install("D"))
		b.assert(t, "C", "start\nreload\n")
	}
	put(t, filepath.Join(b.unit, "fail"), "")
	b.run(t, "failed", b.configuration("C", "do_not_start_or_stop"), b.install("E"))
	b.assert(t, "C", "start\nreload\nrestart\n")
	if err := os.Remove(filepath.Join(b.unit, "fail")); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		b.run(t, "succeeded", b.configuration("C", "do_not_start_or_stop"), b.install("E"))
		b.assert(t, "C", "start\nreload\nrestart\nrestart\n")
	}
}

// A start whose confirmation was lost still happened: the service runs as a
// new instance, which loaded everything on disk. The next run takes that
// instance as the baseline for both blocks, as it does for one block, and
// neither restarts nor reloads.
func TestStartWithLostConfirmationLoadedTheConfigurationOfAnEarlierTaskBlock(t *testing.T) {
	b := newTwoBlocks(t, "late")
	b.f.e.BeforeRecord = func(phase string) error {
		if phase == "confirmed" {
			return errors.New("lost completion")
		}
		return nil
	}
	b.run(t, "failed", b.configuration("B", "do_not_start_or_stop"), b.install("D"))
	b.assert(t, "B", "start\n")
	b.f.e.BeforeRecord = nil
	for range 2 {
		b.run(t, "succeeded", b.configuration("B", "do_not_start_or_stop"), b.install("D"))
		b.assert(t, "B", "start\n")
	}
}

// do_not_start_or_stop owes nothing to a unit that does not exist, as to a stopped
// one: when the unit is installed and started later, by a task outside the
// block, it loads the files on disk, and the next run has nothing to do.
func TestLeaveAsIsOwesNothingToAUnitThatDoesNotExistYet(t *testing.T) {
	b := newTwoBlocks(t, "late")
	b.run(t, "succeeded", b.configuration("B", "do_not_start_or_stop"))
	start := b.install("D").TaskBlock
	b.run(t, "succeeded", start.Changes[0], start.Activate[0])
	b.assert(t, "B", "start\n")
	b.run(t, "succeeded", b.configuration("B", "do_not_start_or_stop"))
	b.assert(t, "B", "start\n")
	for range 2 {
		b.run(t, "succeeded", b.configuration("C", "do_not_start_or_stop"))
		b.assert(t, "C", "start\nreload\n")
	}
}

// Editing an task block's inputs keeps an earlier unloaded change pending.
func TestEditedInputsKeepPendingActivation(t *testing.T) {
	f := newServiceFixture(t)
	f.run(t, f.op("A"), "succeeded")
	op := f.op("B")
	op.TaskBlock.Validate = []config.Task{{Module: "shell", ModuleInputs: &modules.Shell{Script: "exit 1"}}}
	f.run(t, op, "failed")
	site := filepath.Join(f.dir, "site")
	put(t, site, "S")
	op.TaskBlock.Validate = nil
	op.TaskBlock.Changes = append(op.TaskBlock.Changes, taskFile(site, "S"))
	f.run(t, op, "succeeded")
	f.assert(t, "B", "reload\n")
}

// Systemd merges a service's unit files from its names and aliases and their
// templates; a unit task under any of them is part of the service definition.
func TestUnitsAreTrackedThroughTheNamesSystemdReads(t *testing.T) {
	for _, tc := range []struct {
		unit, service, canonical string
		tracked                  bool
	}{
		{"demo.service", "alias", "demo.service", true},
		{"alias.service", "demo", "demo.service", true},
		{"demo@.service", "demo@1", "demo@1.service", true},
		{"demo-web@.service", "demo-web@1", "demo-web@1.service", true},
		{"demo-web.service", "demo", "demo.service", false},
		{"demo@1.service", "demo@2", "demo@2.service", false},
		{"demo.timer", "demo.timer", "demo.timer", true},
		{"demo@.timer", "demo@1.timer", "demo@1.timer", true},
		{"demo@.service", "demo@1.timer", "demo@1.timer", false},
		// The service a timer activates is not the timer's definition.
		{"demo.service", "demo.timer", "demo.timer", false},
	} {
		t.Run(tc.unit+"-"+tc.service, func(t *testing.T) {
			f := newServiceFixture(t)
			if tc.canonical != "demo.service" {
				f.addService(t, tc.canonical)
			}
			op := config.Task{TaskBlock: &config.TaskBlock{ID: "server", Changes: []config.Task{{Module: "systemd_unit", ModuleInputs: &modules.Unit{Name: tc.unit, DropIn: "10-x.conf", Content: text("[Service]\n")}}},
				Activate: []config.Task{{Module: "systemd_service", ModuleInputs: &modules.Service{State: "running", Name: tc.service}}}}}
			f.e.job = job(op)
			state, _, err := f.e.prepareServices(context.Background(), op, scope(), "server")
			if err != nil {
				t.Fatal(err)
			}
			if got := len(state.services[tc.canonical].units) == 1; got != tc.tracked {
				t.Fatalf("unit %s tracked for %s: %v", tc.unit, tc.canonical, got)
			}
		})
	}
}

// A timer named in activate is its own record and action target: a tracked
// change restarts the timer, not the service of the same name.
func TestTimerIsRefreshedAsItself(t *testing.T) {
	f := newServiceFixture(t)
	f.addService(t, "demo.timer")
	enabled := true
	op := func(content string) config.Task {
		return config.Task{TaskBlock: &config.TaskBlock{ID: "server", Changes: []config.Task{taskFile(f.path, content)},
			Activate: []config.Task{{Module: "systemd_service", ModuleInputs: &modules.Service{State: "running", Name: "demo.timer", StartAtBoot: &enabled}}}}}
	}
	f.run(t, op("A"), "succeeded")
	f.run(t, op("B"), "succeeded")
	f.run(t, op("B"), "succeeded")
	if got := read(t, filepath.Join(f.dir, "demo.timer/actions")); got != "restart\n" {
		t.Fatalf("timer actions %q", got)
	}
	if _, err := os.Stat(f.e.servicePath("demo.timer")); err != nil {
		t.Fatal("timer record missing", err)
	}
	f.assert(t, "A", "")
}

// An operator restart during an unchanged run loaded the recorded inputs, so
// MMX does not restart the service again.
func TestOperatorRestartDuringUnchangedRun(t *testing.T) {
	f := newServiceFixture(t)
	f.run(t, f.op("A"), "succeeded")
	op := f.op("A")
	config.InputsOf[modules.Service](op.TaskBlock.Activate[0]).Refresh = ""
	bump := "echo 7 > " + filepath.Join(f.dir, "demo.service/instance")
	op.TaskBlock.Changes = append(op.TaskBlock.Changes, config.Task{Module: "shell", ModuleInputs: &modules.Shell{Script: bump, ChangedWhen: false}})
	f.run(t, op, "succeeded")
	f.assert(t, "A", "")
}

// When the inputs at entry differ from the recorded ones, a restart during the
// run could have loaded either version, so the service is refreshed.
func TestRestartBeforeRestoringLoadedInputsRefreshes(t *testing.T) {
	f := newServiceFixture(t)
	f.run(t, f.op("A"), "succeeded")
	op := f.op("B")
	op.TaskBlock.Validate = []config.Task{{Module: "shell", ModuleInputs: &modules.Shell{Script: "exit 1"}}}
	f.run(t, op, "failed")
	f.e.BeforeMutation = func(resource string) error {
		if resource == f.path {
			put(t, filepath.Join(f.dir, "demo.service/instance"), "7\n")
			put(t, filepath.Join(f.dir, "demo.service/loaded"), "B")
		}
		return nil
	}
	f.run(t, f.op("A"), "succeeded")
	f.assert(t, "A", "reload\n")
}

// A service that starts or restarts partway through an attempt (a package's
// maintainer script) may have loaded the files before that attempt wrote them.
// When the attempt fails before its service task, the next attempt must not
// adopt the written files as that instance's baseline.
func TestInstanceStartedDuringFailedAttemptLoadsWrittenFiles(t *testing.T) {
	for _, running := range []bool{false, true} {
		t.Run(map[bool]string{false: "stopped", true: "recorded"}[running], func(t *testing.T) {
			f := newServiceFixture(t)
			unit := filepath.Join(f.dir, "demo.service")
			if running {
				f.run(t, f.op("A"), "succeeded")
			} else {
				put(t, filepath.Join(unit, "active"), "inactive\n")
				put(t, filepath.Join(unit, "instance"), "\n")
				put(t, filepath.Join(unit, "loaded"), "")
			}
			f.e.BeforeMutation = func(resource string) error {
				if resource == f.path {
					put(t, filepath.Join(unit, "active"), "active\n")
					put(t, filepath.Join(unit, "instance"), "7\n")
					put(t, filepath.Join(unit, "loaded"), read(t, f.path))
				}
				return nil
			}
			op := f.op("B")
			op.TaskBlock.Validate = []config.Task{{Module: "shell", ModuleInputs: &modules.Shell{Script: "exit 1"}}}
			f.run(t, op, "failed")
			f.e.BeforeMutation = nil
			f.assert(t, "A", "")
			op.TaskBlock.Validate = nil
			f.run(t, op, "succeeded")
			f.assert(t, "B", "reload\n")
			f.run(t, op, "succeeded")
			f.assert(t, "B", "reload\n")
		})
	}
}

// A service task for the target in changes runs before the writes, so it
// cannot end the attempt's mark; only a successful activate stage does.
func TestServiceTaskBeforeWritesKeepsTheAttemptMark(t *testing.T) {
	f := newServiceFixture(t)
	unit := filepath.Join(f.dir, "demo.service")
	f.run(t, f.op("A"), "succeeded")
	f.e.BeforeMutation = func(resource string) error {
		if resource == f.path {
			put(t, filepath.Join(unit, "instance"), "7\n")
			put(t, filepath.Join(unit, "loaded"), read(t, f.path))
		}
		return nil
	}
	op := f.op("B")
	op.TaskBlock.Changes = append([]config.Task{{Module: "systemd_service", ModuleInputs: &modules.Service{State: "running", Name: "demo", Refresh: "reload"}}}, op.TaskBlock.Changes...)
	op.TaskBlock.Validate = []config.Task{{Module: "shell", ModuleInputs: &modules.Shell{Script: "exit 1"}}}
	f.run(t, op, "failed")
	f.e.BeforeMutation = nil
	f.run(t, f.op("B"), "succeeded")
	f.assert(t, "B", "reload\n")
}

// An attempt that failed before writing leaves nothing for a later start to
// miss: that start loaded the same files, so no action follows.
func TestServiceStartedAfterAttemptWithoutWritesNeedsNoAction(t *testing.T) {
	f := newServiceFixture(t)
	unit := filepath.Join(f.dir, "demo.service")
	put(t, filepath.Join(unit, "active"), "inactive\n")
	put(t, filepath.Join(unit, "instance"), "\n")
	op := f.op("A")
	op.TaskBlock.Changes = append([]config.Task{{Module: "shell", ModuleInputs: &modules.Shell{Script: "exit 1"}}}, op.TaskBlock.Changes...)
	f.run(t, op, "failed")
	put(t, filepath.Join(unit, "active"), "active\n")
	put(t, filepath.Join(unit, "instance"), "7\n")
	f.run(t, f.op("A"), "succeeded")
	f.assert(t, "A", "")
}

// Several line edits of one file in a service task block count once in its
// hash: either edit reloads the service once, an unchanged rerun does
// nothing, and dryrun predicts from all the edits in turn.
func TestLineEditsOfOneFileReloadOnce(t *testing.T) {
	f := newServiceFixture(t)
	put(t, f.path, "a=1\nb=1\n")
	op := func(a, b string) config.Task {
		edit := func(key, v string) config.Task {
			return config.Task{Module: "line", ModuleInputs: &modules.Line{InsertAtEndOfFile: true, Path: f.path, Match: "^" + key + "=", Text: key + "=" + v}}
		}
		return config.Task{TaskBlock: &config.TaskBlock{ID: "server", Changes: []config.Task{edit("a", a), edit("b", b)}, Activate: []config.Task{{Module: "systemd_service", ModuleInputs: &modules.Service{State: "running", Name: "demo", Refresh: "reload"}}}}}
	}
	f.run(t, op("1", "1"), "succeeded")
	f.assert(t, "A", "")
	j := job(op("2", "1"))
	j.Dryrun = true
	r := f.e.Run(context.Background(), j)
	requireStatus(t, r, "succeeded")
	if service := r.Results[len(r.Results)-2]; !strings.Contains(service.Task, "/activate/") || service.Change != "would_change" {
		t.Fatalf("dryrun did not predict the first edit's reload: %+v", r.Results)
	}
	f.run(t, op("2", "1"), "succeeded")
	f.assert(t, "a=2\nb=1\n", "reload\n")
	f.run(t, op("2", "2"), "succeeded")
	f.run(t, op("2", "2"), "succeeded")
	f.assert(t, "a=2\nb=2\n", "reload\nreload\n")

	// A file task owns the whole path, so it cannot share it with a line.
	both := op("3", "3")
	both.TaskBlock.Changes = append(both.TaskBlock.Changes, taskFile(f.path, "c=1\n"))
	f.run(t, both, "failed")
	if read(t, f.path) != "a=2\nb=2\n" {
		t.Fatal("duplicate path changed the file")
	}
}

// do_not_start_or_stop refreshes a running unit as state: running does. It never starts a
// stopped unit, and owes that unit nothing when it starts later.
func TestLeaveAsIsRefreshesARunningUnitAndNeverStartsAStoppedOne(t *testing.T) {
	f := newServiceFixture(t)
	op := func(content string) config.Task {
		task := f.op(content)
		config.InputsOf[modules.Service](task.TaskBlock.Activate[0]).State = "do_not_start_or_stop"
		return task
	}
	f.run(t, op("B"), "succeeded")
	f.assert(t, "B", "reload\n")
	put(t, filepath.Join(f.dir, "demo.service/active"), "inactive\n")
	enabled := false
	stopped := op("C")
	config.InputsOf[modules.Service](stopped.TaskBlock.Activate[0]).StartAtBoot = &enabled
	f.run(t, stopped, "succeeded")
	f.assert(t, "B", "reload\ndisable\n")
	if _, err := os.Stat(f.e.servicePath("demo.service")); !os.IsNotExist(err) {
		t.Fatal("a stopped unit kept a record of something owed", err)
	}
	// Someone starts the unit: it loads C, and the next run has nothing to do.
	put(t, filepath.Join(f.dir, "demo.service/active"), "active\n")
	put(t, filepath.Join(f.dir, "demo.service/instance"), "7\n")
	put(t, filepath.Join(f.dir, "demo.service/loaded"), "C")
	f.run(t, op("C"), "succeeded")
	f.assert(t, "C", "reload\ndisable\n")
	// A unit that does not exist is unchanged.
	script := filepath.Join(f.dir, "bin/systemctl")
	writeProgramFileThroughShellChild(t, script, strings.ReplaceAll(read(t, script), "load=loaded", "load=not-found"))
	r := f.e.Run(context.Background(), job(config.Task{Module: "systemd_service", ModuleInputs: &modules.Service{Name: "demo", State: "do_not_start_or_stop"}}))
	requireStatus(t, r, "succeeded")
	if r.Change != "unchanged" {
		t.Fatal(r)
	}
}

// masked: true masks after stopping; a masked unit cannot be started until a
// task says masked: false, which unmasks before it starts.
func TestMaskedUnit(t *testing.T) {
	f := newServiceFixture(t)
	yes, no := true, false
	mask := config.Task{Module: "systemd_service", ModuleInputs: &modules.Service{Name: "demo", State: "stopped", StartAtBoot: &no, Masked: &yes}}
	dryrun := job(mask)
	dryrun.Dryrun = true
	if r := f.e.Run(context.Background(), dryrun); r.Status != "succeeded" || r.Change != "would_change" {
		t.Fatal(r)
	}
	f.assert(t, "A", "")
	f.run(t, mask, "succeeded")
	f.assert(t, "A", "disable\nstop\nmask\n")
	if r := f.e.Run(context.Background(), job(mask)); r.Status != "succeeded" || r.Change != "unchanged" {
		t.Fatal(r)
	}
	r := f.e.Run(context.Background(), job(config.Task{Module: "systemd_service", ModuleInputs: &modules.Service{State: "running", Name: "demo"}}))
	if r.Status != "failed" || !strings.Contains(r.Error, "is masked, so it cannot be started or enabled; write masked: false") {
		t.Fatal(r)
	}
	// A task that neither starts nor enables the unit, without masked, leaves
	// the mask alone: a stopped masked unit is as stopped wants it, and a
	// masked unit that still runs is stopped.
	for _, task := range []*modules.Service{{Name: "demo", State: "stopped"}, {Name: "demo", State: "stopped", StartAtBoot: &no}, {Name: "demo", State: "do_not_start_or_stop"}, {Name: "demo", State: "do_not_start_or_stop", StartAtBoot: &no}} {
		if r := f.e.Run(context.Background(), job(config.Task{Module: "systemd_service", ModuleInputs: task})); r.Status != "succeeded" || r.Change != "unchanged" {
			t.Fatalf("state %s, enabled %v: %+v", task.State, task.StartAtBoot, r)
		}
	}
	f.assert(t, "A", "disable\nstop\nmask\n")
	put(t, filepath.Join(f.dir, "demo.service/active"), "active\n")
	f.run(t, config.Task{Module: "systemd_service", ModuleInputs: &modules.Service{Name: "demo", State: "stopped", StartAtBoot: &no}}, "succeeded")
	f.assert(t, "A", "disable\nstop\nmask\nstop\n")
	f.run(t, config.Task{Module: "systemd_service", ModuleInputs: &modules.Service{State: "running", Name: "demo", Masked: &no}}, "succeeded")
	f.assert(t, "A", "disable\nstop\nmask\nstop\nunmask\nstart\n")
}

// A oneshot unit that does not stay active can never be found started; the
// task says so before it runs the unit.
func TestUnitThatRunsOnceCannotBeStarted(t *testing.T) {
	f := newServiceFixture(t)
	put(t, filepath.Join(f.dir, "demo.service/type"), "oneshot\n")
	put(t, filepath.Join(f.dir, "demo.service/active"), "inactive\n")
	r := f.e.Run(context.Background(), job(config.Task{Module: "systemd_service", ModuleInputs: &modules.Service{State: "running", Name: "demo"}}))
	if r.Status != "failed" || !strings.Contains(r.Error, "runs once and then ends") {
		t.Fatal(r)
	}
	f.assert(t, "A", "")
	enabled := true
	f.run(t, config.Task{Module: "systemd_service", ModuleInputs: &modules.Service{Name: "demo", State: "do_not_start_or_stop", StartAtBoot: &enabled}}, "succeeded")
	f.assert(t, "A", "")
}

// writerModule is a module of the project's own that writes text to the file
// at path, and the task that calls it. With the input fail_after_a_change, a
// try that wrote the file fails.
func writerModule(t *testing.T, path string) (map[string]modules.Module, func(text string) config.Task) {
	t.Helper()
	dir := t.TempDir()
	put(t, filepath.Join(dir, modules.ModuleScriptFile), "def run_node_task(inputs):\n    written = write_file(inputs[\"path\"], inputs[\"text\"], owner = inputs[\"owner\"], group = inputs[\"group\"])\n    if written.error:\n        return {\"status\": \"failed\", \"reason\": written.error}\n    if written.changed and inputs.get(\"fail_after_a_change\"):\n        return {\"status\": \"failed\", \"reason\": \"failed after the change\"}\n    return None\n")
	description, err := modules.ParseModuleDescription([]byte("summary: x\ninputs: {path: {type: string, required: true, description: d}, text: {type: string, required: true, description: d}, owner: {type: string, required: true, description: d}, group: {type: string, required: true, description: d}, fail_after_a_change: {type: boolean, required: false, description: d}}\nexamples: [{description: x, config: 'writer: {path: /x, text: x, owner: root, group: root}'}]\n"), "writer")
	if err != nil {
		t.Fatal(err)
	}
	info := modules.Runtime{}.InspectPath(path)
	return map[string]modules.Module{"writer": {Kind: modules.KindProjectStarlark, Description: description, Directory: dir}}, func(text string) config.Task {
		return config.Task{Module: "writer", ModuleInputs: map[string]any{"path": path, "text": text, "owner": info.Owner, "group": info.Group}}
	}
}

// moduleFollower is the service fixture with the writer module: a node task
// block calls the module in its changes and names the service demo in its
// activate phase with refresh: reload.
type moduleFollower struct {
	f      *serviceFixture
	module map[string]modules.Module
	writer func(text string) config.Task
}

func newModuleFollower(t *testing.T) moduleFollower {
	t.Helper()
	f := newServiceFixture(t)
	module, writer := writerModule(t, f.path)
	return moduleFollower{f: f, module: module, writer: writer}
}

func (m moduleFollower) block(text string) config.Task {
	return config.Task{TaskBlock: &config.TaskBlock{ID: "server", Changes: []config.Task{m.writer(text)},
		Activate: []config.Task{{Module: "systemd_service", ModuleInputs: &modules.Service{State: "running", Name: "demo", Refresh: "reload"}}}}}
}

// rejected is the block with a validate phase that fails: the attempt ends
// after the module's task and before the service's.
func (m moduleFollower) rejected(text string) config.Task {
	block := m.block(text)
	block.TaskBlock.Validate = []config.Task{{Module: "shell", ModuleInputs: &modules.Shell{Script: "exit 1"}}}
	return block
}

func (m moduleFollower) job(dryrun bool, nodeTasks ...config.Task) config.Job {
	j := job(nodeTasks...)
	j.Modules, j.Dryrun = m.module, dryrun
	return j
}

// run runs the node tasks as one node job and checks every action that the
// service has been given since the fixture began.
func (m moduleFollower) run(t *testing.T, status, actions string, nodeTasks ...config.Task) {
	t.Helper()
	requireStatus(t, m.f.e.Run(context.Background(), m.job(false, nodeTasks...)), status)
	if got := read(t, filepath.Join(m.f.dir, "demo.service/actions")); got != actions {
		t.Fatalf("actions %q, want %q", got, actions)
	}
}

// reboot leaves the node as a reboot does: MMX's records of services are
// gone, and the service runs as a new instance that loaded what is on disk.
func (m moduleFollower) reboot(t *testing.T) {
	t.Helper()
	if err := os.RemoveAll(m.f.e.RuntimeRoot); err != nil {
		t.Fatal(err)
	}
	m.restartByOtherMeans(t)
}

// restartByOtherMeans gives the service a new instance without MMX, as an
// operator's systemctl restart does: it loads what is on disk.
func (m moduleFollower) restartByOtherMeans(t *testing.T) {
	t.Helper()
	unit := filepath.Join(m.f.dir, "demo.service")
	put(t, filepath.Join(unit, "instance"), strings.TrimSpace(read(t, filepath.Join(unit, "instance")))+"0\n")
	put(t, filepath.Join(unit, "loaded"), read(t, m.f.path))
}

// A module that is not core in a node task block's changes counts by its
// fingerprint, which only the module's own task reports. Where the service's
// record holds none (the first run, after a reboot, after a restart by other
// means), a module that changes nothing gives the baseline and the service
// is left alone; a module that changes something reloads the service once.
func TestServiceFollowsAModuleFingerprint(t *testing.T) {
	m := newModuleFollower(t)
	for range 2 {
		m.run(t, "succeeded", "", m.block("A"))
	}
	for range 2 {
		m.run(t, "succeeded", "reload\n", m.block("B"))
	}
	m.f.assert(t, "B", "reload\n")
	m.reboot(t)
	for range 2 {
		m.run(t, "succeeded", "reload\n", m.block("B"))
	}
	m.restartByOtherMeans(t)
	for range 2 {
		m.run(t, "succeeded", "reload\n", m.block("B"))
	}
	// Without a record, a module that changes something still reloads.
	for _, text := range []string{"C", "D"} {
		m.reboot(t)
		if text == "D" {
			m.restartByOtherMeans(t)
		}
		before := read(t, filepath.Join(m.f.dir, "demo.service/actions"))
		for range 2 {
			m.run(t, "succeeded", before+"reload\n", m.block(text))
		}
		m.f.assert(t, text, before+"reload\n")
	}
}

// A dryrun without a record predicts what the run does: no reload for a
// module that would change nothing, a reload for one that would, and it
// writes no record.
func TestDryrunOfAModuleFingerprintWithoutARecord(t *testing.T) {
	m := newModuleFollower(t)
	for text, want := range map[string]string{"A": "unchanged", "B": "would_change"} {
		r := m.f.e.Run(context.Background(), m.job(true, m.block(text)))
		requireStatus(t, r, "succeeded")
		if service := r.Results[len(r.Results)-2]; !strings.Contains(service.Task, "/activate/") || service.Change != want {
			t.Fatalf("dryrun of %s predicts %+v, want %s", text, service, want)
		}
	}
	if _, err := os.Stat(m.f.e.RuntimeRoot); !os.IsNotExist(err) {
		t.Fatal("dryrun wrote a record", err)
	}
	m.f.assert(t, "A", "")
}

// A run that ends after a module's change and before the service's refresh
// owes the reload, also when the service's record was new in that run and so
// held no fingerprint. The next run, in which the module finds nothing left
// to change, reloads once. Another node task block of the service that runs
// in between, in its own node job, does not take the owed reload away.
func TestOwedReloadAfterAModuleChangeHappensOnTheNextRun(t *testing.T) {
	for _, recorded := range []bool{false, true} {
		for _, between := range []bool{false, true} {
			t.Run(fmt.Sprintf("record held a fingerprint: %t, another block in between: %t", recorded, between), func(t *testing.T) {
				m := newModuleFollower(t)
				site := filepath.Join(m.f.dir, "site")
				put(t, site, "S")
				sites := config.Task{TaskBlock: &config.TaskBlock{ID: "sites", Changes: []config.Task{taskFile(site, "S")},
					Activate: []config.Task{{Module: "systemd_service", ModuleInputs: &modules.Service{State: "running", Name: "demo", Refresh: "reload"}}}}}
				if recorded {
					m.run(t, "succeeded", "", m.block("A"))
				}
				m.run(t, "failed", "", m.rejected("B"))
				m.f.assert(t, "A", "")
				if between {
					m.run(t, "succeeded", "", sites)
				}
				for range 2 {
					m.run(t, "succeeded", "reload\n", m.block("B"))
				}
				m.f.assert(t, "B", "reload\n")
			})
		}
	}
}

// A try of the module's task that fails after it changed something leaves
// the next try nothing to change. The change of the failed try still counts:
// the service reloads, also without an earlier fingerprint in its record.
func TestAModuleChangeInAFailedTryReloadsTheService(t *testing.T) {
	m := newModuleFollower(t)
	block := m.block("B")
	write := &block.TaskBlock.Changes[0]
	write.ModuleInputs.(map[string]any)["fail_after_a_change"] = true
	write.Retry = &config.Retry{MaxTries: 2, PauseBetweenTries: "0s"}
	for range 2 {
		m.run(t, "succeeded", "reload\n", block)
	}
	m.f.assert(t, "B", "reload\n")
}

// A module task that tries again after a try that succeeded reports its
// fingerprint in each try. Only the last try's counts: the next run, whose
// first try is the last, reports the same and reloads nothing.
func TestOnlyTheLastTryOfAModuleTaskGivesItsFingerprint(t *testing.T) {
	m := newModuleFollower(t)
	block := m.block("B")
	block.TaskBlock.Changes[0].Retry = &config.Retry{MaxTries: 3, PauseBetweenTries: "0s", Until: map[string]any{"equals": []any{"{{ .this_node_task.change }}", "unchanged"}}}
	for range 3 {
		m.run(t, "succeeded", "reload\n", block)
	}
	m.f.assert(t, "B", "reload\n")
}

// A service that starts again partway through an attempt loaded what the
// module kept before the attempt's change. When the attempt ends before its
// service task, the next attempt must not take the module's fingerprint as
// that instance's baseline: it reloads once.
func TestInstanceStartedDuringAFailedAttemptHasNotLoadedAModuleChange(t *testing.T) {
	for _, recorded := range []bool{false, true} {
		t.Run(fmt.Sprintf("record held a fingerprint: %t", recorded), func(t *testing.T) {
			m := newModuleFollower(t)
			if recorded {
				m.run(t, "succeeded", "", m.block("A"))
			}
			m.f.e.BeforeMutation = func(resource string) error {
				if resource == m.f.path {
					m.restartByOtherMeans(t)
				}
				return nil
			}
			m.run(t, "failed", "", m.rejected("B"))
			m.f.e.BeforeMutation = nil
			m.f.assert(t, "A", "")
			for range 2 {
				m.run(t, "succeeded", "reload\n", m.block("B"))
			}
			m.f.assert(t, "B", "reload\n")
		})
	}
}

// A module task that its condition skips reports nothing and changes
// nothing, so nothing is owed for it. When it runs in a later run and finds
// nothing to change, its fingerprint is the baseline then.
func TestSkippedModuleTaskOwesNothing(t *testing.T) {
	m := newModuleFollower(t)
	skipped := m.block("A")
	skipped.TaskBlock.Changes[0].When = false
	m.run(t, "succeeded", "", skipped)
	for range 2 {
		m.run(t, "succeeded", "", m.block("A"))
	}
	for range 2 {
		m.run(t, "succeeded", "reload\n", m.block("B"))
	}
}

// A service task for the same unit in changes runs before the module's task
// has reported. It leaves the record waiting for the fingerprint, so a run
// that ends after the module's change still owes the reload.
func TestServiceTaskBeforeAModuleTaskKeepsTheReloadOwed(t *testing.T) {
	m := newModuleFollower(t)
	early := func(block config.Task) config.Task {
		block.TaskBlock.Changes = append([]config.Task{{Module: "systemd_service", ModuleInputs: &modules.Service{State: "running", Name: "demo", Refresh: "reload"}}}, block.TaskBlock.Changes...)
		return block
	}
	m.run(t, "succeeded", "", early(m.block("A")))
	m.reboot(t)
	m.run(t, "failed", "", early(m.rejected("B")))
	for range 2 {
		m.run(t, "succeeded", "reload\n", early(m.block("B")))
	}
	m.f.assert(t, "B", "reload\n")
}

// One service can be in a node task block with a module and in another
// node task block, in one node job or in two (two fleet stages). After a
// reboot the block that runs first makes the service's record, without the
// module's fingerprint. Neither order reloads an unchanged service, and a
// changed module reloads it once.
func TestModuleFingerprintInOneOfTwoTaskBlocks(t *testing.T) {
	for _, order := range []string{"module block first", "module block second"} {
		for _, jobs := range []int{1, 2} {
			t.Run(fmt.Sprintf("%s, %d node jobs", order, jobs), func(t *testing.T) {
				m := newModuleFollower(t)
				site := filepath.Join(m.f.dir, "site")
				put(t, site, "S")
				sites := config.Task{TaskBlock: &config.TaskBlock{ID: "sites", Changes: []config.Task{taskFile(site, "S")},
					Activate: []config.Task{{Module: "systemd_service", ModuleInputs: &modules.Service{State: "running", Name: "demo", Refresh: "reload"}}}}}
				run := func(text, actions string) {
					t.Helper()
					blocks := []config.Task{m.block(text), sites}
					if order == "module block second" {
						blocks = []config.Task{sites, m.block(text)}
					}
					for range 2 {
						if jobs == 1 {
							m.run(t, "succeeded", actions, blocks...)
							continue
						}
						requireStatus(t, m.f.e.Run(context.Background(), m.job(false, blocks[0])), "succeeded")
						m.run(t, "succeeded", actions, blocks[1])
					}
				}
				run("A", "")
				m.reboot(t)
				run("A", "")
				run("B", "reload\n")
				m.reboot(t)
				run("B", "reload\n")
				m.f.assert(t, "B", "reload\n")
			})
		}
	}
}

// Two node task blocks of one service may call the same module, as two calls
// of one role do. Each block follows its own calls: an unchanged run reloads
// nothing, also after a reboot, and a change in one block reloads once. The
// two blocks may have the same name when they are in two fleet stages or in
// two projects.
func TestTwoTaskBlocksOfOneServiceCallTheSameModule(t *testing.T) {
	for _, layout := range []string{"one node job", "two fleet stages", "two projects"} {
		t.Run(layout, func(t *testing.T) {
			m := newModuleFollower(t)
			site := filepath.Join(m.f.dir, "site")
			put(t, site, "S")
			if err := os.Chmod(site, 0644); err != nil {
				t.Fatal(err)
			}
			run := func(text, siteText, actions string) {
				t.Helper()
				sites := m.block(siteText)
				sites.TaskBlock.Changes[0].ModuleInputs.(map[string]any)["path"] = site
				for range 2 {
					if layout == "one node job" {
						sites.TaskBlock.ID = "sites"
						m.run(t, "succeeded", actions, m.block(text), sites)
						continue
					}
					second := m.job(false, sites)
					if layout == "two fleet stages" {
						second.FleetStage = "sites"
					} else {
						second.Project = "sites"
					}
					requireStatus(t, m.f.e.Run(context.Background(), m.job(false, m.block(text))), "succeeded")
					requireStatus(t, m.f.e.Run(context.Background(), second), "succeeded")
					if got := read(t, filepath.Join(m.f.dir, "demo.service/actions")); got != actions {
						t.Fatalf("actions %q, want %q", got, actions)
					}
				}
			}
			run("A", "S", "")
			run("A", "T", "reload\n")
			m.reboot(t)
			run("A", "T", "reload\n")
			run("B", "T", "reload\nreload\n")
			m.f.assert(t, "B", "reload\nreload\n")
		})
	}
}

// Only a module's own run reports its fingerprint; there is nothing on disk
// to take as its baseline. A restart by another task block therefore keeps
// the fingerprint in the service's record, and the module's task block does
// not refresh the service again.
func TestRestartByAnotherTaskBlockKeepsAModuleFingerprint(t *testing.T) {
	b := newTwoBlocks(t, "demo")
	module, writer := writerModule(t, b.f.path)
	run := func(text, definition string) string {
		t.Helper()
		j := job(config.Task{TaskBlock: &config.TaskBlock{ID: "settings",
			Changes:  []config.Task{writer(text)},
			Activate: []config.Task{{Module: "systemd_service", ModuleInputs: &modules.Service{State: "running", Name: "demo", Refresh: "reload"}}}}},
			b.install(definition))
		j.Modules = module
		requireStatus(t, b.f.e.Run(context.Background(), j), "succeeded")
		return read(t, filepath.Join(b.unit, "actions"))
	}
	// The definition was not there when the service last started.
	for range 2 {
		if got := run("A", "D"); got != "restart\n" {
			t.Fatalf("actions %q, want one restart for the new definition", got)
		}
	}
	for range 2 {
		if got := run("A", "E"); got != "restart\nrestart\n" {
			t.Fatalf("actions %q, want one more restart for the changed definition", got)
		}
	}
	for range 2 {
		if got := run("B", "E"); got != "restart\nrestart\nreload\n" {
			t.Fatalf("actions %q, want one reload after the restarts", got)
		}
	}
}

// A dryrun of a service whose unit an earlier node task would install reports
// the task not checked; a run fails.
func TestDryrunOfAServiceNotInstalledYetIsNotChecked(t *testing.T) {
	f := newServiceFixture(t)
	op := config.Task{Module: "systemd_service", ModuleInputs: &modules.Service{Name: "cron", State: "running"}}
	dryrun := job(op)
	dryrun.Dryrun = true
	requireStatus(t, f.e.Run(context.Background(), dryrun), "succeeded")
	results := f.e.Run(context.Background(), dryrun).Results
	if len(results) != 1 || results[0].Status != "not_checked" || !strings.Contains(results[0].Reason, "not installed yet") {
		t.Fatalf("%+v", results)
	}
	requireStatus(t, f.e.Run(context.Background(), job(op)), "failed")
}

// A refresh names the tracked inputs that changed, as a command's record
// does, and says so when MMX has no confirmed record of what the service loaded.
func TestServiceRefreshReasonNamesTheChangedInputs(t *testing.T) {
	record := &serviceRecord{Confirmed: true, Inputs: map[string]string{"config:/etc/app/app.yml": "old", "config:/etc/app/other.yml": "same"}}
	current := map[string]string{"config:/etc/app/app.yml": "new", "config:/etc/app/other.yml": "same"}
	action, reason := refreshAction(modules.Service{Refresh: "reload"}, &servicePlan{record: record}, modules.ServiceState{}, current)
	if action != "reload" || reason != "tracked input /etc/app/app.yml changed after the service last started or reloaded" {
		t.Errorf("%s: %s", action, reason)
	}
	action, reason = refreshAction(modules.Service{}, &servicePlan{before: map[string]string{}}, modules.ServiceState{}, map[string]string{})
	if action != "restart" || reason != "MMX has no confirmed record of the inputs that the running service loaded" {
		t.Errorf("%s: %s", action, reason)
	}
}

// restart_now restarts a running unit once and then writes its marker file;
// while the file is there the task only keeps the unit running. A dryrun
// reports the restart while the file is missing, which is how
// check-one-time-playbooks sees that a node still needs it.
func TestRestartNowRestartsOnceUntilItsMarkerFileExists(t *testing.T) {
	f := newServiceFixture(t)
	marker := filepath.Join(f.dir, "commands_run_once", "restart_demo")
	task := config.Task{Module: "systemd_service", ModuleInputs: &modules.Service{State: "running", Name: "demo", RestartNow: true, RunOnlyOnceMarkerFile: marker}}
	unit := filepath.Join(f.dir, "demo.service")
	attempt := func(dryrun bool, change, reason string) {
		t.Helper()
		j := job(task)
		j.Dryrun = dryrun
		r := f.e.Run(context.Background(), j)
		requireStatus(t, r, "succeeded")
		if got := r.Results[0]; got.Change != change || !strings.Contains(got.Reason, reason) {
			t.Fatalf("dryrun %t: %s %q, want %s with %q", dryrun, got.Change, got.Reason, change, reason)
		}
	}
	attempt(true, "would_change", "would restart demo.service once: the marker file "+marker+" does not exist")
	if read(t, filepath.Join(unit, "actions")) != "" {
		t.Fatal("a dryrun restarted the unit")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("a dryrun wrote the marker file")
	}
	attempt(false, "changed", "restarted demo.service once and wrote the marker file "+marker)
	if got := read(t, filepath.Join(unit, "actions")); got != "restart\n" {
		t.Fatalf("actions %q", got)
	}
	if got := read(t, filepath.Join(unit, "instance")); got != "2\n" {
		t.Fatalf("instance %q: the unit did not restart", got)
	}
	if got := read(t, marker); !strings.HasPrefix(got, "The restart of demo.service succeeded at ") {
		t.Fatalf("marker file %q", got)
	}
	attempt(true, "unchanged", "")
	attempt(false, "unchanged", "")
	if got := read(t, filepath.Join(unit, "actions")); got != "restart\n" {
		t.Fatalf("the unit was restarted again: actions %q", got)
	}
	// With the marker file there, a stopped unit is started as by any task
	// with state: running, not restarted.
	// A dryrun describes the unit as it is; a run says what it found and did.
	put(t, filepath.Join(unit, "active"), "inactive\n")
	attempt(true, "would_change", "service is not running")
	attempt(false, "changed", "service was not running; started")
	if got := read(t, filepath.Join(unit, "actions")); got != "restart\nstart\n" {
		t.Fatalf("actions %q", got)
	}
}

// A unit that does not run is started by its one restart; a restart that
// fails writes no marker file, so the next run restarts the unit again.
func TestRestartNowStartsAStoppedUnitAndRetriesAFailedRestart(t *testing.T) {
	f := newServiceFixture(t)
	marker := filepath.Join(f.dir, "restart_demo")
	task := config.Task{Module: "systemd_service", ModuleInputs: &modules.Service{State: "running", Name: "demo", RestartNow: true, RunOnlyOnceMarkerFile: marker}}
	unit := filepath.Join(f.dir, "demo.service")
	put(t, filepath.Join(unit, "active"), "inactive\n")
	put(t, filepath.Join(unit, "fail"), "")
	r := f.e.Run(context.Background(), job(task))
	requireStatus(t, r, "failed")
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("a failed restart wrote the marker file")
	}
	if err := os.Remove(filepath.Join(unit, "fail")); err != nil {
		t.Fatal(err)
	}
	r = f.e.Run(context.Background(), job(task))
	requireStatus(t, r, "succeeded")
	if got := r.Results[0]; got.Change != "changed" || !strings.HasPrefix(got.Reason, "started demo.service, which was not running, with its one restart") {
		t.Fatalf("%s %q", got.Change, got.Reason)
	}
	if got := read(t, filepath.Join(unit, "actions")); got != "restart\nrestart\n" {
		t.Fatalf("actions %q", got)
	}
	if got := read(t, filepath.Join(unit, "active")); got != "active\n" {
		t.Fatalf("the unit is %q", got)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatal(err)
	}
}

// A unit that a node task block keeps stopped loads its files only when it
// starts. Started outside MMX after the block changed its configuration, as
// for a failover to a standby, it has loaded the new configuration, and the
// next run neither reloads nor restarts it.
func TestStoppedServiceStartedOutsideMMXIsNotRefreshed(t *testing.T) {
	f := newServiceFixture(t)
	unit := filepath.Join(f.dir, "demo.service")
	put(t, filepath.Join(unit, "active"), "inactive\n")
	stopped := f.op("B")
	stopped.TaskBlock.Activate[0].ModuleInputs = &modules.Service{State: "stopped", Name: "demo", Refresh: "reload"}
	f.run(t, stopped, "succeeded")
	f.assert(t, "A", "")
	// Started outside MMX: a new instance that loaded B.
	put(t, filepath.Join(unit, "active"), "active\n")
	put(t, filepath.Join(unit, "instance"), "7\n")
	put(t, filepath.Join(unit, "loaded"), "B")
	f.run(t, f.op("B"), "succeeded")
	f.assert(t, "B", "")
}

// A run's reason says what the run found and did; a reason that names a
// change already reads true after the run and stays as it is.
func TestARunSaysWhatItFoundAndDidToAService(t *testing.T) {
	for _, tc := range []struct{ reason, loadAction, want string }{
		{"service is not running", "start", "service was not running; started"},
		{"service is still starting", "start", "service was still starting; started"},
		{"unit is not masked", "", "unit was not masked; masked"},
		{noConfirmedRecordOfInputs, "reload", "MMX had no confirmed record of the inputs that the running service loaded; reloaded"},
		{"service definition changed", "restart", "service definition changed"},
	} {
		if got := reasonAfterTheRun(tc.reason, tc.loadAction); got != tc.want {
			t.Errorf("%q after %q: got %q, want %q", tc.reason, tc.loadAction, got, tc.want)
		}
	}
}
