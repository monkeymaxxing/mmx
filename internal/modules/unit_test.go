package modules

import (
	"context"
	"crypto/rand"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"mmx/internal/protocol"
	"mmx/internal/value"
)

func TestUnitIdentityAndManagerInspection(t *testing.T) {
	content := "[Service]\nExecStart=/bin/true\n"
	u := Unit{Name: "app.service", Content: &content}
	if file := u.File(); file.Path != "/etc/systemd/system/app.service" {
		t.Fatal(file)
	}
	u.DropIn, u.State, u.Content = "20-limits.conf", "absent", nil
	if file := u.File(); file.Path != "/etc/systemd/system/app.service.d/20-limits.conf" || file.State != "absent" || file.HasContents() {
		t.Fatal(file)
	}
	dir := t.TempDir()
	script := `#!/bin/sh
if [ "$1" = show ]; then
 printf 'Id=app.service\nLoadState=loaded\nActiveState=active\nUnitFileState=enabled\nInvocationID=instance\nNeedDaemonReload=yes\n'
else
 printf '%s\n' "$*" >> "$MMX_MANAGER_CALLS"
fi
`
	writeStandInProgram(t, filepath.Join(dir, "systemctl"), script)
	calls := filepath.Join(dir, "calls")
	putProgramsFirst(t, dir)
	t.Setenv("MMX_MANAGER_CALLS", calls)
	observed, err := InspectService(testContext(t), "app")
	if err != nil || !observed.NeedDaemonReload {
		t.Fatal(observed, err)
	}
	if r := (Runtime{Dryrun: true}).ReloadManager(testContext(t)); r.Status != "failed" {
		t.Fatal("dryrun allowed manager mutation", r)
	}
	if _, err := os.Stat(calls); !os.IsNotExist(err) {
		t.Fatal("dryrun invoked systemctl")
	}
	if r := (Runtime{}).ReloadManager(testContext(t)); r.Status != "succeeded" || r.Change != "changed" {
		t.Fatal(r)
	}
	b, err := os.ReadFile(calls)
	if err != nil || string(b) != "daemon-reload\n" {
		t.Fatal(string(b), err)
	}
}

// Timers, paths, sockets, and mounts are inspected as themselves, never as a
// .service of the same name; systemd's identity must be one of those kinds.
func TestUnitKindsAreInspectedByTheirOwnName(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	script := `#!/bin/sh
for unit; do :; done
[ "$unit" != other.service ] || unit=other.device
printf 'Id=%s\nNames=%s\nLoadState=loaded\nActiveState=inactive\n' "$unit" "$unit"
`
	writeStandInProgram(t, filepath.Join(dir, "systemctl"), script)
	putProgramsFirst(t, dir)
	for name, want := range map[string]string{"app": "app.service", "backup.timer": "backup.timer", "restart.path": "restart.path",
		"app.socket": "app.socket", `srv-my\x2ddata.mount`: `srv-my\x2ddata.mount`,
		"workers.target": "workers.target", "swapfile.swap": "swapfile.swap", "batch.slice": "batch.slice", "srv-data.automount": "srv-data.automount"} {
		observed, err := InspectService(testContext(t), name)
		if err != nil || observed.Unit != want {
			t.Fatal(name, observed, err)
		}
	}
	if _, err := InspectService(testContext(t), "other"); err == nil || !strings.Contains(err.Error(), "invalid systemd unit name") {
		t.Fatal(err)
	}
	for name, want := range map[string]bool{"app@.service": true, "app@.timer": true, "app@1.timer": false, "app.service": false, `srv-my\x2ddata.mount`: false} {
		if template(name) != want {
			t.Fatal(name)
		}
	}
}

func TestUnitRemovalRecoversManagerReload(t *testing.T) {
	// Not parallel: the test works on the node's systemd manager, whose
	// reloads and pending reloads every such test shares.
	for _, dropIn := range []string{"", "20-limits.conf"} {
		t.Run("drop_in="+dropIn, func(t *testing.T) { testUnitRemoval(t, dropIn) })
	}
}

func testUnitRemoval(t *testing.T, dropIn string) {
	if runtime.GOOS != "linux" || os.Geteuid() != 0 {
		t.Skip("requires a root Linux systemd test guest")
	}
	if _, err := os.Stat("/run/systemd/system"); err != nil {
		t.Skip("requires running systemd")
	}
	name := "mmx-code-" + rand.Text() + ".service"
	path := filepath.Join("/etc/systemd/system", name)
	ctx := testContext(t)
	defer func() {
		Command(ctx, nil, "", "systemctl", "stop", name)
		os.Remove(path)
		os.RemoveAll(path + ".d")
		Command(ctx, nil, "", "systemctl", "daemon-reload")
		Command(ctx, nil, "", "systemctl", "reset-failed", name)
	}()
	content := "[Service]\nExecStart=/bin/sleep infinity\n"
	u := Unit{Name: name, Content: &content}
	if r := (Runtime{}).Unit(ctx, u, value.Scope{}); r.Status != "succeeded" {
		t.Fatal(r)
	}
	if dropIn != "" {
		limits := "[Service]\nMemoryMax=64M\n"
		u.DropIn, u.Content = dropIn, &limits
		if r := (Runtime{}).Unit(ctx, u, value.Scope{}); r.Status != "succeeded" {
			t.Fatal(r)
		}
	}
	if r := Command(ctx, nil, "", "systemctl", "start", name); r.Status != "succeeded" {
		t.Fatal(r)
	}
	before, err := InspectService(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	u.State, u.Content = "absent", nil
	run := Runtime{BeforeMutation: func(resource string) error {
		if resource == "systemd manager" {
			return context.Canceled
		}
		return nil
	}}
	r := run.Unit(ctx, u, value.Scope{})
	if r.Status != "failed" || r.Change != "changed" || r.FileChange == nil || r.FileChange.Action != "remove" {
		t.Fatal(r)
	}
	removed := path
	if dropIn != "" {
		removed = filepath.Join(path+".d", dropIn)
	}
	if _, err := os.Stat(removed); !os.IsNotExist(err) {
		t.Fatal("unit file was not removed", err)
	}
	pending, err := InspectService(ctx, name)
	if err != nil || !pending.NeedDaemonReload || pending.Instance != before.Instance {
		t.Fatal("lost pending reload or stopped process", pending, err)
	}
	// A dryrun reports the reload that the run would make, and reloads
	// nothing.
	r = noMutation(t).Unit(ctx, u, value.Scope{})
	if r.Status != "succeeded" || r.Change != "would_change" || r.Reason != "would reload systemd, which has not yet read the changed unit definitions" {
		t.Fatal(r)
	}
	if still, err := InspectService(ctx, name); err != nil || !still.NeedDaemonReload {
		t.Fatal("the dryrun reloaded systemd", still, err)
	}
	r = (Runtime{}).Unit(ctx, u, value.Scope{})
	if r.Status != "succeeded" || r.Change != "changed" {
		t.Fatal(r)
	}
	after, err := InspectService(ctx, name)
	if err != nil || after.NeedDaemonReload || after.Instance != before.Instance {
		t.Fatal(after, err)
	}
	for _, run := range []Runtime{{}, noMutation(t)} {
		if r := run.Unit(ctx, u, value.Scope{}); r.Status != "succeeded" || r.Change != "unchanged" {
			t.Fatal(r)
		}
	}
}

// systemd reports generated for a unit that one of its generators makes
// from another file, and names that file. It can neither enable nor disable
// such a unit, except the unit of an init script: there systemctl hands over
// to the distribution's own tool, and is-enabled says what that tool did.
func TestInspectionOfGeneratedUnits(t *testing.T) {
	t.Parallel()
	dir := standInPrograms(t, map[string]string{"systemctl": `
for unit; do :; done
case "$1" in
show) printf 'Id=%s\nNames=%s\nLoadState=loaded\nActiveState=inactive\nUnitFileState=generated\nSourcePath=%s\n' "$unit" "$unit" "$(cat "$dir/source")";;
is-enabled) cat "$dir/init-script-enabled"; [ "$(cat "$dir/init-script-enabled")" = enabled ];;
esac`})
	for source, enabled := range map[string]string{
		"/etc/containers/systemd/shop-web.container": "generated",
		"/etc/init.d/legacy":                         "disabled",
		"/etc/rc.d/init.d/legacy":                    "disabled",
		"":                                           "generated",
	} {
		writeState(t, filepath.Join(dir, "source"), source)
		writeState(t, filepath.Join(dir, "init-script-enabled"), "disabled\n")
		observed, err := InspectService(testContext(t), "shop-web")
		if err != nil || observed.Enabled != enabled || observed.GeneratedFrom != source {
			t.Fatalf("%s: %+v %v", source, observed, err)
		}
	}
	writeState(t, filepath.Join(dir, "source"), "/etc/init.d/legacy")
	writeState(t, filepath.Join(dir, "init-script-enabled"), "enabled\n")
	if observed, err := InspectService(testContext(t), "legacy"); err != nil || observed.Enabled != "enabled" {
		t.Fatalf("%+v %v", observed, err)
	}
	for source, want := range map[string]string{
		"/home/shop/.config/containers/systemd/shop-web.container": "unit shop-web.service is generated from the Quadlet file /home/shop/.config/containers/systemd/shop-web.container, so systemd can neither enable nor disable it; remove start_at_boot from this task. Whether the unit starts at boot is the [Install] section of that file: WantedBy=multi-user.target for the system's systemd, WantedBy=default.target for an account's",
		"/etc/crypttab": "unit shop-web.service is generated from /etc/crypttab, so systemd can neither enable nor disable it; remove start_at_boot from this task",
		"":              "unit shop-web.service is generated from another file, such as a Podman Quadlet file, so systemd can neither enable nor disable it; remove start_at_boot from this task. Whether the unit starts at boot is the [Install] section of that file: WantedBy=multi-user.target for the system's systemd, WantedBy=default.target for an account's",
	} {
		if err := EnabledOnGeneratedUnit("shop-web.service", source); err.Error() != want {
			t.Errorf("%q: %v", source, err)
		}
	}
}

// Systemd cannot show a template itself; its drop-ins need the template's unit
// file, and a changed drop-in reaches its loaded instance.
func TestTemplateUnitAndDropIn(t *testing.T) {
	// Not parallel: the test works on the node's systemd manager, whose
	// reloads and pending reloads every such test shares.
	if runtime.GOOS != "linux" || os.Geteuid() != 0 {
		t.Skip("requires a root Linux systemd test guest")
	}
	if _, err := os.Stat("/run/systemd/system"); err != nil {
		t.Skip("requires running systemd")
	}
	prefix := "mmx-code-" + rand.Text()
	name, instance := prefix+"@.service", prefix+"@1.service"
	path := filepath.Join("/etc/systemd/system", name)
	ctx := testContext(t)
	defer func() {
		Command(ctx, nil, "", "systemctl", "stop", instance)
		os.Remove(path)
		os.RemoveAll(path + ".d")
		Command(ctx, nil, "", "systemctl", "daemon-reload")
		Command(ctx, nil, "", "systemctl", "reset-failed", instance)
	}()
	limits := "[Service]\nMemoryMax=64M\n"
	dropIn := Unit{Name: name, DropIn: "20-limits.conf", Content: &limits}
	if r := (Runtime{}).Unit(ctx, dropIn, value.Scope{}); r.Status != "failed" || r.FailureKind != "missing_prerequisite" {
		t.Fatal("drop-in without its template", r)
	}
	if _, err := os.Stat(path + ".d"); !os.IsNotExist(err) {
		t.Fatal("created a drop-in directory without its template", err)
	}
	content := "[Service]\nExecStart=/bin/sleep infinity\n"
	if r := (Runtime{}).Unit(ctx, Unit{Name: name, Content: &content}, value.Scope{}); r.Status != "succeeded" || r.Change != "changed" {
		t.Fatal(r)
	}
	if r := (Runtime{}).Unit(ctx, dropIn, value.Scope{}); r.Status != "succeeded" || r.Change != "changed" {
		t.Fatal(r)
	}
	if r := Command(ctx, nil, "", "systemctl", "start", instance); r.Status != "succeeded" {
		t.Fatal(r)
	}
	limits = "[Service]\nMemoryMax=80M\n"
	if r := (Runtime{}).Unit(ctx, dropIn, value.Scope{}); r.Status != "succeeded" || r.Change != "changed" {
		t.Fatal(r)
	}
	if after, err := InspectService(ctx, instance); err != nil || after.NeedDaemonReload {
		t.Fatal("instance does not see the changed drop-in", after, err)
	}
	if r := (Runtime{}).Unit(ctx, dropIn, value.Scope{}); r.Status != "succeeded" || r.Change != "unchanged" {
		t.Fatal(r)
	}
}

func TestUnitCandidateFailureDoesNotReplaceLiveContents(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "20-limits.conf")
	if err := os.WriteFile(path, []byte("live"), 0644); err != nil {
		t.Fatal(err)
	}
	parent, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	next := "invalid"
	r := (Runtime{}).fileAt(testContext(t), File{Mode: "0644", Path: path, Content: &next}, value.Scope{}, parent, 0644, func(candidate string) protocol.Result {
		bytes, err := os.ReadFile(candidate)
		if err != nil || string(bytes) != next {
			t.Fatal(string(bytes), err)
		}
		return protocol.Failed(fmt.Errorf("invalid combined unit"))
	})
	if r.Status != "failed" || r.FailureKind != "validation" || r.Change != "unchanged" || !strings.Contains(r.Reason, "invalid combined") {
		t.Fatal(r)
	}
	bytes, err := os.ReadFile(path)
	if err != nil || string(bytes) != "live" {
		t.Fatal(string(bytes), err)
	}
}

// Unit validation needs installed prerequisites, so dryrun previews without
// writing a candidate under /etc/systemd/system and reports validation as not
// checked only when the unit would change.
func TestUnitDryrunWritesNoCandidate(t *testing.T) {
	// Not parallel: the test works on the node's systemd manager, whose
	// reloads and pending reloads every such test shares.
	if runtime.GOOS != "linux" || os.Geteuid() != 0 {
		t.Skip("requires a root Linux systemd test guest")
	}
	if _, err := os.Stat("/run/systemd/system"); err != nil {
		t.Skip("requires running systemd")
	}
	name := "mmx-code-" + rand.Text() + ".service"
	path := filepath.Join("/etc/systemd/system", name)
	ctx := testContext(t)
	defer func() {
		os.Remove(path)
		Command(ctx, nil, "", "systemctl", "daemon-reload")
	}()
	content := "[Service]\nExecStart=/bin/true\n"
	u := Unit{Name: name, Content: &content}
	if r := (Runtime{}).Unit(ctx, u, value.Scope{}); r.Status != "succeeded" || r.Change != "changed" {
		t.Fatal(r)
	}
	modified := func() time.Time {
		info, err := os.Stat("/etc/systemd/system")
		if err != nil {
			t.Fatal(err)
		}
		return info.ModTime()
	}
	before := modified()
	r := (Runtime{Dryrun: true}).Unit(ctx, u, value.Scope{})
	if r.Status != "succeeded" || r.Change != "unchanged" || r.CheckNewFile != nil {
		t.Fatal(r)
	}
	next := "[Service]\nExecStart=/bin/false\n"
	u.Content = &next
	r = (Runtime{Dryrun: true}).Unit(ctx, u, value.Scope{})
	if r.Status != "succeeded" || r.Change != "would_change" || r.CheckNewFile == nil || r.CheckNewFile.Status != "skipped" || !strings.Contains(r.CheckNewFile.Reason, "not checked") {
		t.Fatal(r)
	}
	if after := modified(); !after.Equal(before) {
		t.Fatal("dryrun wrote under /etc/systemd/system", before, after)
	}
	if bytes, err := os.ReadFile(path); err != nil || string(bytes) != content {
		t.Fatal(string(bytes), err)
	}
}

// Units and directory copies share entryInfoAt; its conflict names the entry
// and the type it needs, not one caller's resource.
func TestEntryConflictNamesWantedType(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "app.service.d"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "app.conf"), 0700); err != nil {
		t.Fatal(err)
	}
	parent, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	for name, want := range map[string]string{"app.service.d": "app.service.d has a conflicting type; expected a directory", "app.conf": "app.conf has a conflicting type; expected a regular file"} {
		if _, err := entryInfoAt(parent, name, name == "app.service.d"); err == nil || err.Error() != want {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

// A failed check of a new unit file names the module whose task installs
// the file. systemd_unit and systemd_scheduled_job have no check_new_file
// field, so their failure does not name one.
func TestUnitCheckFailureNamesItsModule(t *testing.T) {
	t.Parallel()
	rejected := protocol.Failed(fmt.Errorf("exit status 1"))
	rejected.Stderr = "app.service: Service has no ExecStart=\n"
	for module, want := range map[string]string{
		"systemd_unit":          "systemd_unit: systemd's check of the new unit file failed: exit status 1",
		"systemd_scheduled_job": "systemd_scheduled_job: systemd's check of the new unit file failed: exit status 1",
	} {
		if r := unitCheckFailure(module, rejected); r.Status != "failed" || r.Reason != want || r.Stderr != rejected.Stderr {
			t.Errorf("%s: %+v", module, r)
		}
	}
	if r := unitCheckFailure("systemd_unit", protocol.Success("unchanged")); r.Status != "succeeded" || r.Reason != "" {
		t.Fatalf("%+v", r)
	}
}

// A stand-in systemd-analyze for the check of an account's unit. It notes
// who runs it, its arguments, the two settings of its environment that the
// check gives it, and the new file as it finds it. It rejects a file that
// holds the word INVALID with a line that names the file, and while
// $dir/verify-waits exists it does not end.
const fakeUnitVerification = `
/usr/bin/id -u > "$dir/verify-user"
echo "$SYSTEMD_UNIT_PATH" > "$dir/verify-unit-path"
echo "$XDG_RUNTIME_DIR" > "$dir/verify-runtime-directory"
[ ! -e "$dir/verify-waits" ] || /bin/sleep 20
[ -d "$XDG_RUNTIME_DIR" ] && [ -w "$XDG_RUNTIME_DIR" ] || { echo "no runtime directory" >&2; exit 1; }
new=$(find "${SYSTEMD_UNIT_PATH%%:*}" -type f)
cat "$new" > "$dir/verify-new-file"
if grep -q INVALID "$new"; then echo "$new:2: Unknown key name 'INVALID' in section 'Service'" >&2; exit 1; fi`

// The check of a unit of an account's own systemd runs as that account, on
// a copy of the new file in a private directory that is the account's for
// the check: root reads nothing in the account's unit directory for it and
// does nothing there by a path. This test's own account stands in for the
// ordinary account.
func TestUnitCheckOfAnAccountRunsAsTheAccount(t *testing.T) {
	ctx := testContext(t)
	dir := standInPrograms(t, map[string]string{"systemd-analyze": fakeUnitVerification})
	previousLimit := unitCheckTimeLimit
	t.Cleanup(func() { unitCheckTimeLimit = previousLimit })
	privateTaskDirectoryRoot := t.TempDir()
	run := withPrivateTaskDirectoryRoot(Runtime{}, privateTaskDirectoryRoot)
	home := t.TempDir()
	uid, gid := os.Geteuid(), os.Getegid()
	m := UnitManager{Account: "shop", Home: home, UID: uid, GID: gid,
		credential: &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid), NoSetGroups: true}, env: map[string]string{"HOME": home, "XDG_RUNTIME_DIR": "/run/user/" + strconv.Itoa(uid)}}
	text, limits := "[Service]\nExecStart=/bin/true\n", "[Service]\nMemoryMax=64M\n"
	// candidate puts the new file where the file module leaves it for the
	// check and returns the pinned directory and the file's path.
	candidate := func(directory, name, contents string) (*os.Root, string) {
		t.Helper()
		path := filepath.Join(directory, ".mmx-candidate-test", name)
		writeState(t, path, contents)
		parent, err := os.OpenRoot(directory)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { parent.Close() })
		return parent, path
	}
	nothingLeft := func(what string) {
		t.Helper()
		if left, _ := os.ReadDir(privateTaskDirectoryRoot); len(left) != 0 {
			t.Fatal(what, "left the check's private directory behind", left)
		}
	}
	noted := func(name string) string { return strings.TrimSpace(readState(t, filepath.Join(dir, name))) }

	u := LocateUnit(Unit{Name: "app.service", Content: &text, InSystemdOfUserAccount: "shop"}, m)
	parent, path := candidate(m.UnitDirectory(), "app.service", text)
	if r := run.validateUnitCandidate(ctx, "systemd_unit", m, u, parent, path); r.Status != "succeeded" {
		t.Fatalf("%+v", r)
	}
	private := filepath.Dir(noted("verify-runtime-directory"))
	if filepath.Dir(private) != privateTaskDirectoryRoot || !strings.HasPrefix(filepath.Base(private), "systemd_unit-") || noted("verify-runtime-directory") != filepath.Join(private, "runtime") {
		t.Fatal(private)
	}
	if got := noted("verify-unit-path"); got != filepath.Join(private, "unit-files")+":"+m.UnitDirectory()+":" {
		t.Fatal(got)
	}
	if calls := takeCalls(t, dir); !reflect.DeepEqual(calls, []string{"systemd-analyze --user verify --man=no --recursive-errors=no " + filepath.Join(private, "unit-files", "app.service")}) {
		t.Fatal(calls)
	}
	if noted("verify-user") != strconv.Itoa(uid) || readState(t, filepath.Join(dir, "verify-new-file")) != text {
		t.Fatal(noted("verify-user"), readState(t, filepath.Join(dir, "verify-new-file")))
	}
	nothingLeft("the check")

	// A drop-in is checked as part of its unit: systemd is asked about the
	// unit's name and finds the new drop-in in the private directory.
	dropIn := LocateUnit(Unit{Name: "app.service", DropIn: "20-limits.conf", Content: &limits, InSystemdOfUserAccount: "shop"}, m)
	parent, path = candidate(filepath.Join(m.UnitDirectory(), "app.service.d"), "20-limits.conf", limits)
	if r := run.validateUnitCandidate(ctx, "systemd_unit", m, dropIn, parent, path); r.Status != "succeeded" {
		t.Fatalf("%+v", r)
	}
	if calls := takeCalls(t, dir); !reflect.DeepEqual(calls, []string{"systemd-analyze --user verify --man=no --recursive-errors=no app.service"}) {
		t.Fatal(calls)
	}
	if readState(t, filepath.Join(dir, "verify-new-file")) != limits {
		t.Fatal(readState(t, filepath.Join(dir, "verify-new-file")))
	}
	nothingLeft("the check of a drop-in")

	// A file that systemd rejects: its message names the file where it goes.
	parent, path = candidate(filepath.Join(m.UnitDirectory(), "app.service.d"), "20-limits.conf", "[Service]\nINVALID=1\n")
	r := run.validateUnitCandidate(ctx, "systemd_unit", m, dropIn, parent, path)
	if r.Status != "failed" || r.Stderr != filepath.Join(m.UnitDirectory(), "app.service.d", "20-limits.conf")+":2: Unknown key name 'INVALID' in section 'Service'\n" {
		t.Fatalf("%+v", r)
	}
	nothingLeft("the check of a rejected file")
	takeCalls(t, dir)

	// A new file that is not a regular file is not read, and nothing runs.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(dir, "verify-user"), path); err != nil {
		t.Fatal(err)
	}
	r = run.validateUnitCandidate(ctx, "systemd_unit", m, dropIn, parent, path)
	if r.Status != "failed" || !strings.HasPrefix(r.Reason, "prepare private files for systemd unit validation: ") {
		t.Fatalf("%+v", r)
	}
	if calls := takeCalls(t, dir); calls != nil {
		t.Fatal(calls)
	}

	// A check that does not end is ended after its time limit, with what
	// can make it wait.
	unitCheckTimeLimit = 300 * time.Millisecond
	writeState(t, filepath.Join(dir, "verify-waits"), "")
	parent, path = candidate(m.UnitDirectory(), "app.service", text)
	r = run.validateUnitCandidate(ctx, "systemd_unit", m, u, parent, path)
	want := "systemd-analyze verify did not finish within 300ms; systemd-analyze reads unit files in " + m.UnitDirectory() + ", where a non-regular file, such as a named pipe, can make systemd-analyze wait indefinitely"
	if r.Status != "failed" || r.Reason != want {
		t.Fatalf("%+v", r)
	}
	nothingLeft("the check that did not end")
}
