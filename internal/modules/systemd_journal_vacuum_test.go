package modules

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"mmx/internal/protocol"

	"golang.org/x/sys/unix"
)

// journalFilesFixture is a journal with one root directory and this machine's
// directory in it, and a stand-in journalctl that deletes the files that
// the test names in $dir/delete and notes its call.
type journalFilesFixture struct {
	t                  *testing.T
	machine, programs  string
	sequence           string
	nextSequenceNumber int
}

const journalTestMachineID = "0123456789abcdef0123456789abcdef"

// newJournalFilesFixture points the module at the fixture's directories, which
// are package variables, so its tests do not run in parallel.
func newJournalFilesFixture(t *testing.T) *journalFilesFixture {
	root := t.TempDir()
	machineFile := filepath.Join(root, "machine-id")
	if err := os.WriteFile(machineFile, []byte(journalTestMachineID+"\n"), 0644); err != nil {
		t.Fatal(err)
	}
	journal := filepath.Join(root, "var/log/journal")
	if err := os.MkdirAll(filepath.Join(journal, journalTestMachineID), 0755); err != nil {
		t.Fatal(err)
	}
	roots, machineID := journalRootDirectories, machineIDFile
	journalRootDirectories, machineIDFile = []string{filepath.Join(root, "run/log/journal"), journal}, machineFile
	t.Cleanup(func() { journalRootDirectories, machineIDFile = roots, machineID })
	programs := standInPrograms(t, map[string]string{
		"journalctl": `[ ! -f "$dir/delete" ] || while read -r file; do rm -f "$file"; done < "$dir/delete"
[ ! -f "$dir/fail" ] || { echo "Failed to vacuum" >&2; exit 1; }`,
	})
	return &journalFilesFixture{t: t, machine: filepath.Join(journal, journalTestMachineID), programs: programs, sequence: strings.Repeat("ab", 16), nextSequenceNumber: 1}
}

// archive writes an archived file of the fixture's sequence whose first
// entry has the time first, with entries entries and kibibytes of data,
// and returns its path and the bytes it takes on disk.
func (f *journalFilesFixture) archive(first time.Time, entries uint64, kibibytes int) (string, int64) {
	f.t.Helper()
	name := "system@" + f.sequence + "-" + hex16(uint64(f.nextSequenceNumber)) + "-" + hex16(uint64(first.UnixMicro())) + ".journal"
	f.nextSequenceNumber++
	return f.write(name, first, entries, kibibytes)
}

func (f *journalFilesFixture) write(name string, modified time.Time, entries uint64, kibibytes int) (string, int64) {
	f.t.Helper()
	path := filepath.Join(f.machine, name)
	data := make([]byte, kibibytes*1024)
	if len(data) >= 160 {
		copy(data, "LPKSHHRH")
		binary.LittleEndian.PutUint64(data[152:], entries)
		// Not all zeros, so the filesystem allocates every block.
		for i := 160; i < len(data); i++ {
			data[i] = byte(i)
		}
	}
	if err := os.WriteFile(path, data, 0640); err != nil {
		f.t.Fatal(err)
	}
	if err := os.Chtimes(path, modified, modified); err != nil {
		f.t.Fatal(err)
	}
	var st unix.Stat_t
	if err := unix.Lstat(path, &st); err != nil {
		f.t.Fatal(err)
	}
	return path, 512 * st.Blocks
}

func (f *journalFilesFixture) deleteInRun(paths ...string) {
	f.t.Helper()
	if err := os.WriteFile(filepath.Join(f.programs, "delete"), []byte(strings.Join(paths, "\n")+"\n"), 0644); err != nil {
		f.t.Fatal(err)
	}
}

func (f *journalFilesFixture) calls() string {
	text, _ := os.ReadFile(filepath.Join(f.programs, "calls"))
	return string(text)
}

func hex16(n uint64) string {
	text := strconv.FormatUint(n, 16)
	return strings.Repeat("0", 16-len(text)) + text
}

func exists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

func journalValue(t *testing.T, r protocol.Result) (freed int64, deleted int) {
	t.Helper()
	v, ok := r.Value.(map[string]any)
	if !ok {
		t.Fatalf("value %#v", r.Value)
	}
	return v["freed_bytes"].(int64), v["deleted_archived_journal_files"].(int)
}

// keep_at_most_size deletes the oldest archived files of a directory until
// its files, the active one included, fit; a dryrun predicts exactly the
// files that the run's journalctl deletes, and deletes nothing itself.
func TestSystemdJournalVacuumBySize(t *testing.T) {
	f := newJournalFilesFixture(t)
	now := time.Now()
	_, active := f.write("system.journal", now, 5, 1024)
	oldest, oldestBytes := f.archive(now.Add(-3*time.Hour), 5, 1024)
	older, olderBytes := f.archive(now.Add(-2*time.Hour), 5, 1024)
	newest, newestBytes := f.archive(now.Add(-time.Hour), 5, 1024)
	// Room for the active file and the newest archived file only.
	limit := active + newestBytes
	limitMiB := (limit + mebibyte - 1) / mebibyte
	task := SystemdJournalVacuum{KeepAtMostSize: strconv.FormatInt(limitMiB, 10) + "MiB"}
	limitBytes := limitMiB * mebibyte
	if limitBytes >= active+olderBytes+newestBytes {
		t.Fatalf("the files are too small for the test: %d bytes", active+olderBytes+newestBytes)
	}
	r := Runtime{Dryrun: true}.SystemdJournalVacuum(testContext(t), task)
	if r.Status != "succeeded" || r.Change != "would_change" {
		t.Fatalf("%+v", r)
	}
	if freed, deleted := journalValue(t, r); freed != oldestBytes+olderBytes || deleted != 2 {
		t.Fatalf("predicted %d bytes in %d files, want %d in 2", freed, deleted, oldestBytes+olderBytes)
	}
	if !strings.Contains(r.Reason, "would delete 2 archived journal files and free ") || f.calls() != "" || !exists(oldest) {
		t.Fatalf("%q; calls %q", r.Reason, f.calls())
	}
	f.deleteInRun(oldest, older)
	r = Runtime{}.SystemdJournalVacuum(testContext(t), task)
	if r.Status != "succeeded" || r.Change != "changed" || !strings.HasPrefix(r.Reason, "deleted 2 archived journal files and freed ") {
		t.Fatalf("%+v", r)
	}
	if freed, deleted := journalValue(t, r); freed != oldestBytes+olderBytes || deleted != 2 {
		t.Fatalf("freed %d bytes in %d files, want %d in 2", freed, deleted, oldestBytes+olderBytes)
	}
	if want := "journalctl --vacuum-size=" + strconv.FormatInt(limitBytes, 10) + "\n"; f.calls() != want {
		t.Fatalf("calls %q, want %q", f.calls(), want)
	}
	if !exists(newest) {
		t.Fatal("the newest archived file is gone")
	}
	// Now the files fit: a dryrun and a run report unchanged.
	f.deleteInRun()
	for _, dryrun := range []bool{true, false} {
		r = Runtime{Dryrun: dryrun}.SystemdJournalVacuum(testContext(t), task)
		if freed, deleted := journalValue(t, r); r.Status != "succeeded" || r.Change != "unchanged" || freed != 0 || deleted != 0 {
			t.Fatalf("dryrun %t: %+v", dryrun, r)
		}
		if want := "no archived journal file is beyond the limits; the journal's files take " + bytesText(active+newestBytes); r.Reason != want {
			t.Fatalf("%q, want %q", r.Reason, want)
		}
	}
}

// keep_newer_than deletes each archived file whose first entry is older,
// also when the file's own times say it is older than its name does; an
// empty archived file goes whatever the limits; a damaged file counts by
// the time in its name; the active file and files of other names stay.
func TestSystemdJournalVacuumByAge(t *testing.T) {
	f := newJournalFilesFixture(t)
	now := time.Now()
	f.write("system.journal", now, 5, 64)
	f.write("notes.txt", now.Add(-100*24*time.Hour), 0, 64)
	old, oldBytes := f.archive(now.Add(-20*24*time.Hour), 5, 64)
	// Its name says one hour, its modification time 30 days.
	touched, touchedBytes := f.archive(now.Add(-time.Hour), 5, 64)
	if err := os.Chtimes(touched, now.Add(-30*24*time.Hour), now.Add(-30*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	recent, _ := f.archive(now.Add(-2*time.Hour), 5, 64)
	empty, emptyBytes := f.archive(now.Add(-time.Minute), 0, 64)
	damaged, damagedBytes := f.write("system@"+hex16(uint64(now.Add(-15*24*time.Hour).UnixMicro()))+"-00000000000000ff.journal~", now, 5, 64)
	task := SystemdJournalVacuum{KeepNewerThan: "14d"}
	r := Runtime{Dryrun: true}.SystemdJournalVacuum(testContext(t), task)
	want := oldBytes + touchedBytes + emptyBytes + damagedBytes
	if freed, deleted := journalValue(t, r); r.Change != "would_change" || freed != want || deleted != 4 {
		t.Fatalf("predicted %d bytes in %d files, want %d in 4: %+v", freed, deleted, want, r)
	}
	f.deleteInRun(old, touched, empty, damaged)
	r = Runtime{}.SystemdJournalVacuum(testContext(t), task)
	if freed, deleted := journalValue(t, r); r.Change != "changed" || freed != want || deleted != 4 {
		t.Fatalf("freed %d bytes in %d files, want %d in 4: %+v", freed, deleted, want, r)
	}
	if want := "journalctl --vacuum-time=1209600s\n"; f.calls() != want {
		t.Fatalf("calls %q, want %q", f.calls(), want)
	}
	if !exists(recent) || !exists(filepath.Join(f.machine, "system.journal")) {
		t.Fatal("a file that the limits keep is gone")
	}
}

// Both limits go to one journalctl; a file that either limit deletes goes.
// A journalctl that fails fails the task, with what it deleted before.
func TestSystemdJournalVacuumWithBothLimitsAndAFailure(t *testing.T) {
	f := newJournalFilesFixture(t)
	now := time.Now()
	old, oldBytes := f.archive(now.Add(-30*24*time.Hour), 5, 64)
	f.archive(now.Add(-time.Hour), 5, 64)
	task := SystemdJournalVacuum{KeepAtMostSize: "1GiB", KeepNewerThan: "7d"}
	if freed, deleted := journalValue(t, Runtime{Dryrun: true}.SystemdJournalVacuum(testContext(t), task)); freed != oldBytes || deleted != 1 {
		t.Fatalf("predicted %d bytes in %d files", freed, deleted)
	}
	f.deleteInRun(old)
	if err := os.WriteFile(filepath.Join(f.programs, "fail"), nil, 0644); err != nil {
		t.Fatal(err)
	}
	r := Runtime{}.SystemdJournalVacuum(testContext(t), task)
	if r.Status != "failed" || r.Change != "changed" || !strings.Contains(r.Reason, "Failed to vacuum") || !strings.Contains(r.Reason, "deleted 1 archived journal files") {
		t.Fatalf("%+v", r)
	}
	if want := "journalctl --vacuum-size=1073741824 --vacuum-time=604800s\n"; f.calls() != want {
		t.Fatalf("calls %q, want %q", f.calls(), want)
	}
}

// A node without journalctl, which comes with systemd, cannot be checked yet.
func TestSystemdJournalVacuumNeedsJournalctl(t *testing.T) {
	t.Parallel()
	setWorkerEnvironment(t, "PATH", t.TempDir())
	task := SystemdJournalVacuum{KeepNewerThan: "14d"}
	if r := (Runtime{Dryrun: true}).SystemdJournalVacuum(testContext(t), task); r.Status != "not_checked" || !strings.Contains(r.Reason, "journalctl is not installed") {
		t.Fatalf("%+v", r)
	}
	if r := (Runtime{}).SystemdJournalVacuum(testContext(t), task); r.Status != "failed" || r.FailureKind != "missing_prerequisite" {
		t.Fatalf("%+v", r)
	}
}

func TestSystemdJournalVacuumRejectsInputs(t *testing.T) {
	t.Parallel()
	for task, want := range map[SystemdJournalVacuum]string{
		{}:                        "systemd_journal_vacuum requires keep_at_most_size, keep_newer_than, or a value for each setting",
		{KeepAtMostSize: "500MB"}: "keep_at_most_size: size must be a whole number above 0 and MiB, GiB, or TiB",
		{KeepAtMostSize: "0MiB"}:  "keep_at_most_size: size must be a whole number above 0",
		{KeepNewerThan: "2w"}:     "keep_newer_than: must be a duration above zero, such as 14d, 12h, or 30m",
		{KeepNewerThan: "0d"}:     "keep_newer_than: must be a duration above zero",
	} {
		if err := task.Prepare(""); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%+v: %v, want %q", task, err, want)
		}
	}
	for _, task := range []SystemdJournalVacuum{{KeepAtMostSize: "500MiB"}, {KeepNewerThan: "36h"}, {KeepAtMostSize: "2GiB", KeepNewerThan: "14d"}} {
		if err := task.Prepare(""); err != nil {
			t.Errorf("%+v: %v", task, err)
		}
	}
}
