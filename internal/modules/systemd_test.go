package modules

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// fakeJournalctl answers with the entries in $dir/journal/<unit>, one file
// for each unit, and with $dir/journal/printed-by-account for the query of
// everything an account printed. It prints the newest --lines entries, as
// journalctl does. An account's own read (--user) fails while
// $dir/account-cannot-read exists, every read while $dir/journal-unreadable
// exists, and a read never ends while $dir/journal-hangs exists.
const fakeJournalctl = `
lines=10
of=
for argument; do
	case "$argument" in
	--lines=*) lines=${argument#--lines=} ;;
	--unit=*) of=${argument#--unit=} ;;
	_SYSTEMD_USER_UNIT=*) of=${argument#_SYSTEMD_USER_UNIT=} ;;
	_TRANSPORT=stdout) of=printed-by-account ;;
	--user)
		if [ -e "$dir/account-cannot-read" ]; then
			echo "No journal files were opened due to insufficient permissions." >&2
			exit 1
		fi ;;
	esac
done
[ ! -e "$dir/journal-hangs" ] || exec sleep 30
[ ! -e "$dir/journal-unreadable" ] || exit 1
[ ! -f "$dir/journal/$of" ] || tail -n "$lines" "$dir/journal/$of"
`

// fakeSystemctlShow answers systemctl show: the properties of the unit that
// needs others from $dir/needs, and the state of the needed units from
// $dir/states, as systemctl prints them.
const fakeSystemctlShow = `
[ "$1" != --user ] || shift
case "$2" in
--property=Requires,Requisite,BindsTo) cat "$dir/needs" ;;
--property=Id,ActiveState) cat "$dir/states" ;;
esac
`

// journalEntryText is one entry as journalctl --output=json prints it. The
// journal received an entry of time 2000 after the action was asked for at
// 1000, and one of time 500 before.
func journalEntryText(received int, fields string) string {
	return fmt.Sprintf(`{"__MONOTONIC_TIMESTAMP":"%d","_BOOT_ID":"b59443ac1bdf439393687f8eac4e24e2",%s}`+"\n", received, fields)
}

func journalFixture(t *testing.T, journal map[string]string) string {
	t.Helper()
	dir := standInPrograms(t, map[string]string{"journalctl": fakeJournalctl, "systemctl": fakeSystemctlShow})
	for unit, entries := range journal {
		writeState(t, filepath.Join(dir, "journal", unit), entries)
	}
	return dir
}

const askedAt = JournalTime(1000)

func TestJournalClockGoesForward(t *testing.T) {
	t.Parallel()
	first := JournalNow()
	time.Sleep(2 * time.Millisecond)
	if second := JournalNow(); second <= first || second-first > JournalTime(time.Minute/time.Microsecond) {
		t.Fatal("the journal's clock is not the monotonic clock in microseconds", first, second)
	}
}

func TestFailedActionGainsTheUnitsJournalLinesSinceTheAction(t *testing.T) {
	t.Parallel()
	dir := journalFixture(t, map[string]string{"shop.service": journalEntryText(500, `"MESSAGE":"an older start: listening on port 8080","_SYSTEMD_UNIT":"shop.service"`) +
		journalEntryText(2000, `"MESSAGE":"Starting shop.service...","UNIT":"shop.service","JOB_TYPE":"start"`) +
		// A message with a color sequence, a tab, and a line break comes as byte values.
		journalEntryText(2001, `"MESSAGE":[27,91,51,49,109,99,97,110,110,111,116,9,114,101,97,100,10,99,111,110,102,105,103],"_SYSTEMD_UNIT":"shop.service"`) +
		journalEntryText(2002, `"MESSAGE":null,"_SYSTEMD_UNIT":"shop.service"`) +
		journalEntryText(2003, `"MESSAGE":"   ","_SYSTEMD_UNIT":"shop.service"`) +
		journalEntryText(2004, `"MESSAGE":"shop.service: Main process exited, code=exited, status=1/FAILURE","UNIT":"shop.service"`) +
		journalEntryText(2005, `"MESSAGE":"Failed to start shop.service.","UNIT":"shop.service","JOB_TYPE":"start","JOB_RESULT":"failed"`) +
		"not an entry\n"})
	got := UnitManager{}.JournalAfterFailedAction(testContext(t), "start", "shop.service", askedAt)
	want := "\nLast lines of the systemd journal for shop.service during this start:" +
		"\n  Starting shop.service..." +
		"\n  [31mcannot read config" +
		"\n  shop.service: Main process exited, code=exited, status=1/FAILURE" +
		"\n  Failed to start shop.service."
	if got != want {
		t.Fatalf("got %q\nwant %q", got, want)
	}
	if calls := readState(t, filepath.Join(dir, "calls")); calls != "journalctl --boot --all --no-pager --quiet --output=json --lines=10 --unit=shop.service\n" {
		t.Fatal(calls)
	}
}

func TestJournalLinesAreLimited(t *testing.T) {
	t.Parallel()
	entries := ""
	for n := range 25 {
		entries += journalEntryText(2000+n, fmt.Sprintf(`"MESSAGE":"line %02d: %s","_SYSTEMD_UNIT":"shop.service"`, n, strings.Repeat("é", 400)))
	}
	journalFixture(t, map[string]string{"shop.service": entries})
	got := UnitManager{}.JournalAfterFailedAction(testContext(t), "restart", "shop.service", askedAt)
	lines := strings.Split(got, "\n")
	if len(lines) != 2+journalLinesShown || lines[1] != "Last lines of the systemd journal for shop.service during this restart:" {
		t.Fatalf("%d lines: %q", len(lines), got)
	}
	// The newest ten, each cut inside the limit and never inside a
	// character: the limit falls on the second byte of an é.
	for n, line := range lines[2:] {
		if !strings.HasPrefix(line, fmt.Sprintf("  line %02d: é", 15+n)) || !strings.HasSuffix(line, "é ...") || len(line) > 2+journalLineLimit+len(" ...") || strings.ContainsRune(line, '�') {
			t.Fatalf("line %d: %d bytes: %q", n, len(line), line)
		}
	}
	if len(got) > journalAdditionLimit {
		t.Fatal(len(got))
	}
}

func TestJournalThatGivesNothingLeavesTheFailureAsItIs(t *testing.T) {
	t.Parallel()
	entry := journalEntryText(2000, `"MESSAGE":"shop: cannot read config","_SYSTEMD_UNIT":"shop.service"`)
	for name, prepare := range map[string]func(dir string){
		"only older lines": func(dir string) {
			writeState(t, filepath.Join(dir, "journal/shop.service"), journalEntryText(999, `"MESSAGE":"older"`))
		},
		"no line of the unit":   func(dir string) { os.Remove(filepath.Join(dir, "journal/shop.service")) },
		"journalctl fails":      func(dir string) { writeState(t, filepath.Join(dir, "journal-unreadable"), "") },
		"journalctl is missing": func(dir string) { os.Remove(filepath.Join(dir, "journalctl")); setWorkerEnvironment(t, "PATH", dir) },
		"a line without a time": func(dir string) {
			writeState(t, filepath.Join(dir, "journal/shop.service"), `{"MESSAGE":"when?"}`+"\n")
		},
		"a line without message": func(dir string) {
			writeState(t, filepath.Join(dir, "journal/shop.service"), journalEntryText(2000, `"PRIORITY":"6"`))
		},
	} {
		t.Run(name, func(t *testing.T) {
			dir := journalFixture(t, map[string]string{"shop.service": entry})
			prepare(dir)
			if got := (UnitManager{}).JournalAfterFailedAction(testContext(t), "start", "shop.service", askedAt); got != "" {
				t.Fatalf("%q", got)
			}
		})
	}
}

func TestJournalReadHasItsOwnTimeLimit(t *testing.T) {
	dir := journalFixture(t, map[string]string{"shop.service": journalEntryText(2000, `"MESSAGE":"shop: cannot read config"`)})
	writeState(t, filepath.Join(dir, "journal-hangs"), "")
	previous := journalReadTimeLimit
	journalReadTimeLimit = 200 * time.Millisecond
	t.Cleanup(func() { journalReadTimeLimit = previous })
	started := time.Now()
	if got := (UnitManager{}).JournalAfterFailedAction(testContext(t), "start", "shop.service", askedAt); got != "" {
		t.Fatalf("%q", got)
	}
	if took := time.Since(started); took > 10*time.Second {
		t.Fatal("the journal read did not end at its time limit", took)
	}
	// A task whose own time is over reads nothing.
	os.Remove(filepath.Join(dir, "journal-hangs"))
	ended, cancel := context.WithCancel(testContext(t))
	cancel()
	if got := (UnitManager{}).JournalAfterFailedAction(ended, "start", "shop.service", askedAt); got != "" {
		t.Fatalf("%q", got)
	}
}

func TestFailedStartNamesTheNeededUnitThatIsNotRunning(t *testing.T) {
	t.Parallel()
	gaveUp := journalEntryText(2003, `"MESSAGE":"Dependency failed for shop.service.","UNIT":"shop.service","JOB_TYPE":"start","JOB_RESULT":"dependency"`) +
		journalEntryText(2004, `"MESSAGE":"shop.service: Job shop.service/start failed with result 'dependency'.","UNIT":"shop.service","JOB_RESULT":"dependency"`)
	dir := journalFixture(t, map[string]string{"shop.service": gaveUp,
		"shop-pod.service": journalEntryText(700, `"MESSAGE":"an older failure","_SYSTEMD_UNIT":"shop-pod.service"`) +
			journalEntryText(2001, `"MESSAGE":"Error: building local pause image: finding pause binary: exec: \"catatonit\": executable file not found in $PATH","_SYSTEMD_UNIT":"shop-pod.service"`) +
			journalEntryText(2002, `"MESSAGE":"Failed to start shop-pod.service.","UNIT":"shop-pod.service","JOB_RESULT":"failed"`)})
	writeState(t, filepath.Join(dir, "needs"), "Requires=sysinit.target system.slice\nRequisite=shop-data.mount\nBindsTo=shop-pod.service\n")
	writeState(t, filepath.Join(dir, "states"), "Id=sysinit.target\nActiveState=active\n\nActiveState=reloading\nId=system.slice\n\nId=shop-data.mount\nActiveState=inactive\n\nId=shop-pod.service\nActiveState=failed\n")
	got := UnitManager{}.JournalAfterFailedAction(testContext(t), "start", "shop.service", askedAt)
	want := "\nsystemd did not start shop.service, because shop-data.mount, which it needs, is not running (ActiveState=inactive)." +
		"\nsystemd did not start shop.service, because shop-pod.service, which it needs, failed." +
		"\nLast lines of the systemd journal for shop-pod.service during this start:" +
		"\n  Error: building local pause image: finding pause binary: exec: \"catatonit\": executable file not found in $PATH" +
		"\n  Failed to start shop-pod.service."
	if got != want {
		t.Fatalf("got %q\nwant %q", got, want)
	}
	calls := readState(t, filepath.Join(dir, "calls"))
	for _, call := range []string{"systemctl show --property=Requires,Requisite,BindsTo -- shop.service\n",
		"systemctl show --property=Id,ActiveState -- sysinit.target system.slice shop-data.mount shop-pod.service\n"} {
		if !strings.Contains(calls, call) {
			t.Fatal(calls)
		}
	}

	// When every needed unit runs again by now, or systemd does not answer,
	// the unit's own lines say that a dependency failed.
	own := "\nLast lines of the systemd journal for shop.service during this start:" +
		"\n  Dependency failed for shop.service." +
		"\n  shop.service: Job shop.service/start failed with result 'dependency'."
	writeState(t, filepath.Join(dir, "states"), "Id=sysinit.target\nActiveState=active\n\nId=shop-pod.service\nActiveState=active\n")
	if got := (UnitManager{}).JournalAfterFailedAction(testContext(t), "start", "shop.service", askedAt); got != own {
		t.Fatalf("%q", got)
	}
	writeState(t, filepath.Join(dir, "needs"), "Requires=\nRequisite=\nBindsTo=\n")
	if got := (UnitManager{}).JournalAfterFailedAction(testContext(t), "start", "shop.service", askedAt); got != own {
		t.Fatalf("%q", got)
	}

	// A unit that failed by itself is not explained by a needed unit, and
	// systemd is not asked about one.
	writeState(t, filepath.Join(dir, "calls"), "")
	writeState(t, filepath.Join(dir, "journal/shop.service"), journalEntryText(2000, `"MESSAGE":"Failed to start shop.service.","JOB_RESULT":"failed"`))
	if got := (UnitManager{}).JournalAfterFailedAction(testContext(t), "start", "shop.service", askedAt); got != "\nLast lines of the systemd journal for shop.service during this start:\n  Failed to start shop.service." {
		t.Fatalf("%q", got)
	}
	if calls := readState(t, filepath.Join(dir, "calls")); strings.Contains(calls, "systemctl") {
		t.Fatal(calls)
	}
}

func TestMoreNeededUnitsThanTheLimitAreCut(t *testing.T) {
	t.Parallel()
	needs, states, journal := "Requires=", "", map[string]string{
		"shop.service": journalEntryText(2000, `"MESSAGE":"Dependency failed for shop.service.","JOB_RESULT":"dependency"`)}
	for n := range 6 {
		unit := fmt.Sprintf("needed-%d.service", n)
		needs += unit + " "
		states += "Id=" + unit + "\nActiveState=failed\n\n"
		for line := range journalLinesShown {
			journal[unit] += journalEntryText(2000+line, fmt.Sprintf(`"MESSAGE":"%s"`, strings.Repeat("x", journalLineLimit)))
		}
	}
	dir := journalFixture(t, journal)
	writeState(t, filepath.Join(dir, "needs"), needs+"\n")
	writeState(t, filepath.Join(dir, "states"), states)
	got := UnitManager{}.JournalAfterFailedAction(testContext(t), "start", "shop.service", askedAt)
	if strings.Count(got, "which it needs, failed.") != 2 || strings.Contains(got, "needed-2.service") || len(got) != journalAdditionLimit+len(" ...") || !strings.HasSuffix(got, "x ...") {
		t.Fatalf("%d bytes: %q", len(got), got)
	}
	// Without the long lines, three needed units are named and no more.
	for unit := range journal {
		if unit != "shop.service" {
			writeState(t, filepath.Join(dir, "journal", unit), "")
		}
	}
	got = UnitManager{}.JournalAfterFailedAction(testContext(t), "start", "shop.service", askedAt)
	if strings.Count(got, "which it needs, failed.") != neededUnitsShown || !strings.Contains(got, "needed-2.service") || strings.Contains(got, "needed-3.service") || strings.Contains(got, "Last lines") {
		t.Fatalf("%q", got)
	}
}

// An account's unit: the account reads its own journal where it may, and
// root reads the same entries, filtered by the account's UID, where it may
// not. The lines of the account without a unit come under their own heading.
func TestFailedActionOfAnAccountsUnit(t *testing.T) {
	t.Parallel()
	uid, gid := os.Geteuid(), os.Getegid()
	home := t.TempDir()
	m := UnitManager{Account: "shop", Home: home, UID: uid, GID: gid,
		credential: &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid), NoSetGroups: true}, env: map[string]string{"HOME": home, "XDG_RUNTIME_DIR": "/run/user/" + strconv.Itoa(uid)}}
	dir := journalFixture(t, map[string]string{
		"shop-web.service": journalEntryText(2000, `"MESSAGE":"Starting shop-web.service...","USER_UNIT":"shop-web.service","_SYSTEMD_UNIT":"user@1002.service","_SYSTEMD_CGROUP":"/user.slice/user-1002.slice/user@1002.service/init.scope"`) +
			journalEntryText(2002, `"MESSAGE":"shop-web.service: Main process exited, code=exited, status=3/NOTIMPLEMENTED","USER_UNIT":"shop-web.service","_SYSTEMD_UNIT":"user@1002.service","_SYSTEMD_CGROUP":"/user.slice/user-1002.slice/user@1002.service/init.scope"`),
		"printed-by-account": journalEntryText(900, `"MESSAGE":"an older line without a unit","_TRANSPORT":"stdout"`) +
			journalEntryText(2001, `"MESSAGE":"shop-web: cannot read /home/shop/config.yml","_TRANSPORT":"stdout"`) +
			journalEntryText(2001, `"MESSAGE":"a line of another unit of the account","_TRANSPORT":"stdout","_SYSTEMD_UNIT":"user@1002.service","_SYSTEMD_CGROUP":"/user.slice/user-1002.slice/user@1002.service/app.slice/other.service"`) +
			journalEntryText(2001, `"MESSAGE":"a line of a system unit that runs as the account","_TRANSPORT":"stdout","_SYSTEMD_UNIT":"shop-backup.service"`)})
	want := "\nLast lines of the systemd journal for shop-web.service of account shop during this start:" +
		"\n  Starting shop-web.service..." +
		"\n  shop-web.service: Main process exited, code=exited, status=3/NOTIMPLEMENTED" +
		"\nLines that programs of account shop printed during this start, for which the systemd journal recorded no unit because the program ended at once:" +
		"\n  shop-web: cannot read /home/shop/config.yml"
	id := strconv.Itoa(uid)
	unit := " _SYSTEMD_USER_UNIT=shop-web.service _UID=" + id + " + USER_UNIT=shop-web.service _UID=" + id + "\n"
	const options = "journalctl --boot --all --no-pager --quiet --output=json --lines="
	if got := m.JournalAfterFailedAction(testContext(t), "start", "shop-web.service", askedAt); got != want {
		t.Fatalf("got %q\nwant %q", got, want)
	}
	if calls := readState(t, filepath.Join(dir, "calls")); calls != options+"10 --user"+unit+options+"100 --user _UID="+id+" _TRANSPORT=stdout\n" {
		t.Fatal(calls)
	}

	writeState(t, filepath.Join(dir, "calls"), "")
	writeState(t, filepath.Join(dir, "account-cannot-read"), "")
	if got := m.JournalAfterFailedAction(testContext(t), "start", "shop-web.service", askedAt); got != want {
		t.Fatalf("got %q\nwant %q", got, want)
	}
	if calls := readState(t, filepath.Join(dir, "calls")); calls != options+"10 --user"+unit+options+"10"+unit+options+"100 _UID="+id+" _TRANSPORT=stdout\n" {
		t.Fatal(calls)
	}

	// A needed unit of the account, asked of the account's systemd.
	writeState(t, filepath.Join(dir, "calls"), "")
	writeState(t, filepath.Join(dir, "journal/shop-web.service"), journalEntryText(2003, `"MESSAGE":"Dependency failed for shop-web.service.","USER_UNIT":"shop-web.service","JOB_RESULT":"dependency"`))
	writeState(t, filepath.Join(dir, "journal/shop-pod.service"), journalEntryText(2002, `"MESSAGE":"Failed to start shop-pod.service.","USER_UNIT":"shop-pod.service","JOB_RESULT":"failed"`))
	writeState(t, filepath.Join(dir, "needs"), "Requires=app.slice basic.target\nRequisite=\nBindsTo=shop-pod.service\n")
	writeState(t, filepath.Join(dir, "states"), "Id=app.slice\nActiveState=active\n\nId=basic.target\nActiveState=active\n\nId=shop-pod.service\nActiveState=failed\n")
	want = "\nThe systemd of account shop did not start shop-web.service, because shop-pod.service, which it needs, failed." +
		"\nLast lines of the systemd journal for shop-pod.service of account shop during this start:" +
		"\n  Failed to start shop-pod.service." +
		"\nLines that programs of account shop printed during this start, for which the systemd journal recorded no unit because the program ended at once:" +
		"\n  shop-web: cannot read /home/shop/config.yml"
	if got := m.JournalAfterFailedAction(testContext(t), "start", "shop-web.service", askedAt); got != want {
		t.Fatalf("got %q\nwant %q", got, want)
	}
	if calls := readState(t, filepath.Join(dir, "calls")); !strings.Contains(calls, "systemctl --user show --property=Requires,Requisite,BindsTo -- shop-web.service\n") ||
		!strings.Contains(calls, " _SYSTEMD_USER_UNIT=shop-pod.service _UID="+id+" + USER_UNIT=shop-pod.service _UID="+id+"\n") {
		t.Fatal(calls)
	}
}

// An account's systemd that is not running (no lingering, nobody logged in)
// runs none of the account's units and answers nothing. Whether a unit is
// there, enabled, or masked is in the account's unit files, which systemctl
// reads and changes itself with SYSTEMD_OFFLINE=1, starting nothing: the
// account's systemd and its units stay stopped. A verb that would run
// something still asks that systemd, and fails. The stand-in answers only
// offline, as systemctl --user does then, and keeps the state of each unit
// file in $dir/unit-files.
func TestUnitFilesOfAnAccountWhoseSystemdIsNotRunning(t *testing.T) {
	dir := standInPrograms(t, map[string]string{"systemctl": `
[ "$1" != --user ] || shift
if [ "${SYSTEMD_OFFLINE:-}" != 1 ]; then echo "Failed to connect to bus: No medium found" >&2; exit 1; fi
for unit; do :; done
state="$dir/unit-files/$unit"
case "$1" in
is-enabled)
	if [ ! -f "$state" ]; then echo "Failed to get unit file state for $unit: No such file or directory" >&2; exit 1; fi
	cat "$state"
	case "$(cat "$state")" in enabled|static) exit 0 ;; not-found) exit 4 ;; *) exit 1 ;; esac ;;
enable) echo enabled > "$state" ;;
disable) [ "$(cat "$state")" = masked ] || echo disabled > "$state" ;;
mask) echo masked > "$state" ;;
unmask) echo disabled > "$state" ;;
*) echo "Running in chroot, ignoring command '$1'" >&2 ;;
esac`})
	before := accountRuntimeDirectories
	accountRuntimeDirectories = filepath.Join(dir, "run-user")
	t.Cleanup(func() { accountRuntimeDirectories = before })
	uid, gid := os.Geteuid(), os.Getegid()
	home := t.TempDir()
	m := UnitManager{Account: "shop", Home: home, UID: uid, GID: gid,
		credential: &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid), NoSetGroups: true},
		env:        map[string]string{"HOME": home, "XDG_RUNTIME_DIR": filepath.Join(dir, "run-user", strconv.Itoa(uid))}}
	ctx := testContext(t)
	inspect := func(name string) ServiceState {
		t.Helper()
		state, err := m.Inspect(ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		return state
	}
	writeState(t, filepath.Join(dir, "unit-files/report-worker.service"), "enabled\n")
	if got := inspect("report-worker"); got.Unit != "report-worker.service" || got.Load != "loaded" || got.Active != "inactive" || got.Enabled != "enabled" {
		t.Fatalf("%+v", got)
	}
	for _, step := range []struct{ action, enabled, load string }{{"disable", "disabled", "loaded"}, {"mask", "masked", "masked"}, {"unmask", "disabled", "loaded"}, {"enable", "enabled", "loaded"}} {
		if r := m.Action(ctx, step.action, "report-worker.service"); r.Status != "succeeded" {
			t.Fatalf("%s: %+v", step.action, r)
		}
		if got := inspect("report-worker.service"); got.Enabled != step.enabled || got.Load != step.load || got.Active != "inactive" {
			t.Fatalf("after %s: %+v", step.action, got)
		}
	}
	// A missing unit, as systemd 252 says it and as newer ones say it.
	writeState(t, filepath.Join(dir, "unit-files/gone-newer.service"), "not-found\n")
	for _, name := range []string{"never-installed.service", "gone-newer.service"} {
		if got := inspect(name); got.Load != "not-found" || got.Enabled != "" || got.Active != "inactive" {
			t.Fatalf("%s: %+v", name, got)
		}
	}
	// Offline, systemctl would ignore a start and succeed; MMX never asks so.
	if r := m.Action(ctx, "start", "report-worker.service"); r.Status != "failed" || !strings.Contains(r.Stderr, "Failed to connect to bus") {
		t.Fatalf("%+v", r)
	}
	for _, call := range takeCalls(t, dir) {
		if strings.Contains(call, "user@") || strings.Contains(call, " show ") {
			t.Fatal("asked the account's systemd, or started it:", call)
		}
	}
	if m.Running() {
		t.Fatal("the account's systemd runs")
	}
	// A systemd that runs is asked.
	writeState(t, filepath.Join(dir, "run-user", strconv.Itoa(uid), "systemd/private"), "")
	if _, err := m.Inspect(ctx, "report-worker.service"); err == nil || !strings.Contains(err.Error(), "Failed to connect to bus") {
		t.Fatal(err)
	}
	if calls := takeCalls(t, dir); len(calls) != 1 || !strings.HasPrefix(calls[0], "systemctl --user show ") {
		t.Fatal(calls)
	}
}
