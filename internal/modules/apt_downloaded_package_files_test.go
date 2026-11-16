package modules

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// aptCacheFixture is apt's archives directory with stand-ins of apt-config,
// which names it, and of apt-get, whose clean deletes what the real one
// deletes, while $dir/held-times says how many of its calls still find the
// lock held.
func aptCacheFixture(t *testing.T) (archives, programs string) {
	t.Helper()
	archives = filepath.Join(t.TempDir(), "var/cache/apt/archives")
	if err := os.MkdirAll(filepath.Join(archives, "partial"), 0755); err != nil {
		t.Fatal(err)
	}
	put := func(name, text string) {
		if err := os.WriteFile(filepath.Join(archives, name), []byte(text), 0644); err != nil {
			t.Fatal(err)
		}
	}
	put("lock", "")
	programs = standInPrograms(t, map[string]string{
		"apt-config": `echo "ARCHIVES='` + archives + `/'"`,
		"apt-get": `[ "$1" = clean ] || exit 64
held=$(cat "$dir/held-times" 2>/dev/null || echo 0)
if [ "$held" -gt 0 ]; then
	echo $((held - 1)) > "$dir/held-times"
	echo 'E: Could not get lock ` + archives + `/lock. It is held by process 2016 (apt-get)' >&2
	echo 'E: Unable to lock directory ` + archives + `/' >&2
	exit 100
fi
find "` + archives + `" "` + archives + `/partial" -maxdepth 1 ! -type d ! -name lock -exec rm -f {} +`,
	})
	return archives, programs
}

func aptCacheValue(t *testing.T, v any) (files int, bytes int64) {
	t.Helper()
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("value %#v", v)
	}
	return m["deleted_package_files"].(int), m["deleted_bytes"].(int64)
}

// A dryrun counts the package files without deleting them; a run deletes
// them with apt-get clean and reports what is gone; then it is unchanged and
// runs nothing. apt's lock and its partial/ directory stay.
func TestAptDownloadedPackageFiles(t *testing.T) {
	t.Parallel()
	archives, programs := aptCacheFixture(t)
	for name, size := range map[string]int{"hello_2.10-3_amd64.deb": 53080, "git_2.39.5_amd64.deb": 7264380, "partial/curl_7.88.1_amd64.deb": 1000} {
		if err := os.WriteFile(filepath.Join(archives, name), make([]byte, size), 0644); err != nil {
			t.Fatal(err)
		}
	}
	const total = 53080 + 7264380 + 1000
	ctx := testContext(t)
	r := Runtime{Dryrun: true}.AptDownloadedPackageFiles(ctx)
	if files, bytes := aptCacheValue(t, r.Value); r.Status != "succeeded" || r.Change != "would_change" || files != 3 || bytes != total {
		t.Fatalf("%+v", r)
	}
	if want := "would delete 3 package files totaling 7.0 MiB (7318460 bytes) from " + archives + "/"; r.Reason != want {
		t.Fatalf("%q, want %q", r.Reason, want)
	}
	if calls, _ := os.ReadFile(filepath.Join(programs, "calls")); strings.Contains(string(calls), "apt-get") {
		t.Fatalf("a dryrun ran apt-get: %s", calls)
	}
	r = Runtime{}.AptDownloadedPackageFiles(ctx)
	if files, bytes := aptCacheValue(t, r.Value); r.Status != "succeeded" || r.Change != "changed" || files != 3 || bytes != total {
		t.Fatalf("%+v", r)
	}
	left, _ := filepath.Glob(filepath.Join(archives, "*.deb"))
	partial, _ := filepath.Glob(filepath.Join(archives, "partial/*"))
	if len(left)+len(partial) != 0 || !exists(filepath.Join(archives, "lock")) {
		t.Fatalf("left %v %v", left, partial)
	}
	for _, dryrun := range []bool{true, false} {
		r = Runtime{Dryrun: dryrun}.AptDownloadedPackageFiles(ctx)
		if files, _ := aptCacheValue(t, r.Value); r.Change != "unchanged" || files != 0 || r.Reason != "apt's archives directory "+archives+"/ holds no downloaded package file" {
			t.Fatalf("dryrun %t: %+v", dryrun, r)
		}
	}
	if calls, _ := os.ReadFile(filepath.Join(programs, "calls")); strings.Count(string(calls), "apt-get clean") != 1 {
		t.Fatalf("calls %s", calls)
	}
}

// apt-get clean fails at once while another apt holds the lock of the
// downloaded files: the task tries again until the lock is free, as a
// refresh of the package lists does, and names the lock when it stays held.
func TestAptDownloadedPackageFilesWaitsForAptsLock(t *testing.T) {
	archives, programs := aptCacheFixture(t)
	pause, wait := aptLockPause, aptLockWait
	aptLockPause, aptLockWait = 10*time.Millisecond, 5*time.Second
	t.Cleanup(func() { aptLockPause, aptLockWait = pause, wait })
	held := func(times string) {
		if err := os.WriteFile(filepath.Join(programs, "held-times"), []byte(times), 0644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(archives, "hello_2.10-3_amd64.deb"), []byte("deb"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	held("2")
	ctx := testContext(t)
	if r := (Runtime{}).AptDownloadedPackageFiles(ctx); r.Status != "succeeded" || r.Change != "changed" {
		t.Fatalf("%+v", r)
	}
	if calls, _ := os.ReadFile(filepath.Join(programs, "calls")); strings.Count(string(calls), "apt-get clean") != 3 {
		t.Fatalf("calls %s", calls)
	}
	held("1000")
	aptLockWait = time.Second
	r := Runtime{}.AptDownloadedPackageFiles(ctx)
	if r.Status != "failed" || r.Change != "unchanged" || !strings.Contains(r.Reason, "another apt process still holds the lock on the downloaded package files after 1s") ||
		!strings.Contains(r.Reason, "Could not get lock") {
		t.Fatalf("%+v", r)
	}
}

// A node without apt-get cannot be checked yet; a run fails without another
// try. Validation requires state: absent.
func TestAptDownloadedPackageFilesNeedsAptAndState(t *testing.T) {
	t.Parallel()
	setWorkerEnvironment(t, "PATH", t.TempDir())
	if r := (Runtime{Dryrun: true}).AptDownloadedPackageFiles(testContext(t)); r.Status != "not_checked" || !strings.Contains(r.Reason, "apt-get is not installed") {
		t.Fatalf("%+v", r)
	}
	if r := (Runtime{}).AptDownloadedPackageFiles(testContext(t)); r.Status != "failed" || r.FailureKind != "missing_prerequisite" {
		t.Fatalf("%+v", r)
	}
	for _, state := range []string{"", "present"} {
		if err := (&AptDownloadedPackageFiles{State: state}).Prepare(""); err == nil || !strings.Contains(err.Error(), "apt_downloaded_package_files.state is required and must be absent") {
			t.Fatalf("%q: %v", state, err)
		}
	}
}
