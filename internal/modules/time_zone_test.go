package modules

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// A stand-in timedatectl links $dir/localtime to the zone's file, as the
// real one links /etc/localtime. A file "fail" makes set-timezone fail as
// timedated does when SELinux keeps it from writing the link: it still
// answers "show" with the new name. "stuck" makes it change nothing.
const fakeTimedatectl = `
case "$1" in
show) cat "$dir/zone";;
set-timezone)
  if [ -e "$dir/fail" ]; then echo "$2" > "$dir/zone"; echo "Failed to set time zone: Access denied" >&2; exit 1; fi
  [ -e "$dir/stuck" ] || { echo "$2" > "$dir/zone"; ln -sfn "../zoneinfo/$2" "$dir/etc/localtime"; };;
esac`

func timeZoneFixture(t *testing.T, zone string) string {
	t.Helper()
	dir := standInPrograms(t, map[string]string{"timedatectl": fakeTimedatectl})
	writeState(t, filepath.Join(dir, "zone"), zone+"\n")
	for _, name := range []string{"Europe/Berlin", "Etc/UTC", "UTC"} {
		writeState(t, filepath.Join(dir, "zoneinfo", name), "TZif2")
	}
	if err := os.MkdirAll(filepath.Join(dir, "etc"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../zoneinfo/"+zone, filepath.Join(dir, "etc", "localtime")); err != nil {
		t.Fatal(err)
	}
	previousDatabase, previousLink := timeZoneDatabase, localTimeLink
	timeZoneDatabase, localTimeLink = filepath.Join(dir, "zoneinfo"), filepath.Join(dir, "etc", "localtime")
	t.Cleanup(func() { timeZoneDatabase, localTimeLink = previousDatabase, previousLink })
	return dir
}

func TestTimeZoneIsSetThroughTimedatectl(t *testing.T) {
	ctx := testContext(t)
	dir := timeZoneFixture(t, "Etc/UTC")
	z := TimeZone{Name: "Europe/Berlin"}
	want := map[string]any{"time_zone_before": "Etc/UTC", "time_zone_after": "Europe/Berlin"}
	preview := noMutation(t).TimeZone(ctx, z)
	if preview.Status != "succeeded" || preview.Change != "would_change" || !reflect.DeepEqual(preview.Value, want) {
		t.Fatalf("%+v", preview)
	}
	if calls := changing(takeCalls(t, dir), "timedatectl show"); calls != nil {
		t.Fatal("dryrun changed the time zone", calls)
	}
	made := (Runtime{}).TimeZone(ctx, z)
	if made.Status != "succeeded" || made.Change != "changed" || !reflect.DeepEqual(made.Value, want) || readState(t, filepath.Join(dir, "zone")) != "Europe/Berlin\n" {
		t.Fatalf("%+v", made)
	}
	if calls := changing(takeCalls(t, dir), "timedatectl show"); !reflect.DeepEqual(calls, []string{"timedatectl set-timezone Europe/Berlin"}) {
		t.Fatal(calls)
	}
	for _, run := range []Runtime{{}, noMutation(t)} {
		if r := run.TimeZone(ctx, z); r.Status != "succeeded" || r.Change != "unchanged" || r.Value != nil {
			t.Fatalf("%+v", r)
		}
	}
	// A node without /etc/localtime, such as a container, uses UTC.
	os.Remove(localTimeLink)
	if r := noMutation(t).TimeZone(ctx, z); r.Change != "would_change" || r.Value.(map[string]any)["time_zone_before"] != "UTC" {
		t.Fatalf("%+v", r)
	}
	// A node whose /etc/localtime is a copy, not a link, has no name yet.
	writeState(t, localTimeLink, "TZif2")
	if r := (Runtime{}).TimeZone(ctx, z); r.Change != "changed" || r.Value.(map[string]any)["time_zone_before"] != "" {
		t.Fatalf("%+v", r)
	}
}

func TestTimeZoneFailures(t *testing.T) {
	ctx := testContext(t)
	dir := timeZoneFixture(t, "Etc/UTC")
	// An unknown name, or a directory of the database, fails before any
	// change, in a dryrun too.
	for _, name := range []string{"Europe/Berlinn", "Europe"} {
		for _, run := range []Runtime{{}, {Dryrun: true}} {
			r := run.TimeZone(ctx, TimeZone{Name: name})
			if r.Status != "failed" || r.FailureKind != "config" || !strings.Contains(r.Reason, "is not a time zone") || !strings.Contains(r.Reason, "timedatectl list-timezones") {
				t.Fatalf("%+v", r)
			}
		}
	}
	if calls := changing(takeCalls(t, dir), "timedatectl show"); calls != nil {
		t.Fatal(calls)
	}
	z := TimeZone{Name: "Europe/Berlin"}
	writeState(t, filepath.Join(dir, "fail"), "")
	if r := (Runtime{}).TimeZone(ctx, z); r.Status != "failed" || r.Change != "unknown" || !strings.Contains(r.Reason, "Access denied") {
		t.Fatalf("%+v", r)
	}
	// timedated now answers with the new name, but the link still names
	// the old zone: a second try is no success.
	for _, run := range []Runtime{{}, {Dryrun: true}} {
		if r := run.TimeZone(ctx, z); r.Change == "unchanged" {
			t.Fatalf("%+v", r)
		}
	}
	os.Remove(filepath.Join(dir, "fail"))
	writeState(t, filepath.Join(dir, "zone"), "Etc/UTC\n")
	writeState(t, filepath.Join(dir, "stuck"), "")
	if r := (Runtime{}).TimeZone(ctx, z); r.Status != "failed" || r.Change != "changed" || !strings.Contains(r.Reason, `is "Etc/UTC", not "Europe/Berlin"`) {
		t.Fatalf("%+v", r)
	}
	os.Remove(filepath.Join(dir, "stuck"))
	if r := (Runtime{}).TimeZone(ctx, z); r.Status != "succeeded" || r.Change != "changed" {
		t.Fatalf("%+v", r)
	}
	// Without the time zone database, or without timedatectl, dryrun cannot
	// check and a run fails, naming the package.
	os.RemoveAll(timeZoneDatabase)
	for run, status := range map[*Runtime]string{{Dryrun: true}: "not_checked", {}: "failed"} {
		if r := run.TimeZone(ctx, z); r.Status != status || !strings.Contains(r.Reason, "install the package tzdata") {
			t.Fatalf("%+v", r)
		}
	}
	setWorkerEnvironment(t, "PATH", t.TempDir())
	for run, status := range map[*Runtime]string{{Dryrun: true}: "not_checked", {}: "failed"} {
		if r := run.TimeZone(ctx, z); r.Status != status || !strings.Contains(r.Reason, "timedatectl is not installed; install the package systemd") {
			t.Fatalf("%+v", r)
		}
	}
}

func TestTimeZoneRule(t *testing.T) {
	t.Parallel()
	for name, ok := range map[string]bool{
		"Europe/Berlin": true, "UTC": true, "America/Argentina/Buenos_Aires": true, "Etc/GMT+5": true, "Etc/GMT-14": true,
		"../etc/passwd": false, "/Europe/Berlin": false, "Europe/": false, "Europe//Berlin": false, "Europe/Berlin ": false, "": false,
	} {
		if err := timeZoneRule(name); (err == nil) != ok {
			t.Error(name, err)
		}
	}
}
