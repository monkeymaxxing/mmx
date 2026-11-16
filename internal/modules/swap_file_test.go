package modules

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

func swapFixture(t *testing.T) (storageFixture, SwapFile) {
	t.Helper()
	f := newStorageFixture(t)
	return f, SwapFile{Path: filepath.Join(f.dir, "swapfile"), Size: "2MiB"}
}

// requireSwapFile checks the file itself: its size written in full, mode
// 0600, its swap signature, and that /proc/swaps and fstab list it.
func requireSwapFile(t *testing.T, f storageFixture, s SwapFile) {
	t.Helper()
	info, err := os.Lstat(s.Path)
	if err != nil {
		t.Fatal(err)
	}
	size, _ := wholeBinarySize(s.Size)
	if info.Size() != size || info.Sys().(*syscall.Stat_t).Blocks*512 < size || info.Mode().Perm() != 0600 {
		t.Fatalf("size %d, blocks %d, mode %v", info.Size(), info.Sys().(*syscall.Stat_t).Blocks, info.Mode())
	}
	parent, _ := os.OpenRoot(filepath.Dir(s.Path))
	defer parent.Close()
	if ok, err := hasSwapSignature(parent, filepath.Base(s.Path)); !ok || err != nil {
		t.Fatal("no swap signature", err)
	}
	if active, _ := swapActive(s.Path); !active {
		t.Fatal("not switched on")
	}
	if !strings.Contains(f.read(t, "fstab"), "\n"+s.Path+" none swap defaults 0 0\n") {
		t.Fatalf("%q", f.read(t, "fstab"))
	}
}

func TestSwapFileIsWrittenSwitchedOnAndKeptInFstab(t *testing.T) {
	ctx := context.Background()
	f, s := swapFixture(t)
	before := f.read(t, "fstab")

	preview := (Runtime{Dryrun: true}).SwapFile(ctx, s)
	requireResult(t, preview, "succeeded", "would_change", "would write "+s.Path+" with 2 MiB, mode 0600, owned by root, and give it a swap signature, then switch on "+s.Path+", then add the swap file's line to "+fstabPath)
	if preview.FileChange == nil || !strings.Contains(preview.FileChange.Diff, "+"+s.Path+" none swap defaults 0 0") {
		t.Fatalf("%+v", preview.FileChange)
	}
	if _, err := os.Lstat(s.Path); !os.IsNotExist(err) || f.read(t, "fstab") != before || len(f.calls(t)) != 0 {
		t.Fatal("dryrun changed the node")
	}

	made := (Runtime{}).SwapFile(ctx, s)
	requireResult(t, made, "succeeded", "changed", "wrote "+s.Path+" with 2 MiB and its swap signature, switched on "+s.Path+", added the swap file's line to "+fstabPath)
	requireSwapFile(t, f, s)
	// The comments and the other lines stay as they were.
	if !strings.HasPrefix(f.read(t, "fstab"), before) {
		t.Fatal(f.read(t, "fstab"))
	}
	temporary := filepath.Join(f.dir, ".swapfile.mmx-new")
	if want := []string{"mkswap " + temporary, "swapon " + s.Path, "findmnt --verify", "systemctl daemon-reload"}; !callsStartWith(f.calls(t), want) {
		t.Fatal("calls")
	}
	for _, dryrun := range []bool{false, true} {
		requireResult(t, (Runtime{Dryrun: dryrun}).SwapFile(ctx, s), "succeeded", "unchanged", "")
	}
	if calls := f.calls(t); len(calls) != 0 {
		t.Fatal(calls)
	}
}

// Each step is found by inspection: a run that was cut off after any of
// them is finished by the next, which does only what is missing.
func TestSwapFileFindsEachMissingStep(t *testing.T) {
	ctx := context.Background()
	f, s := swapFixture(t)
	requireResult(t, (Runtime{}).SwapFile(ctx, s), "succeeded", "changed", "")
	written := f.read(t, "fstab")
	f.calls(t)
	for _, tc := range []struct {
		name  string
		undo  func()
		calls []string
	}{
		{"switched off", func() { f.set(t, "swaps", "Filename Type Size Used Priority\n") }, []string{"swapon " + s.Path}},
		{"line removed", func() { f.set(t, "fstab", strings.Replace(written, s.Path+" none swap defaults 0 0\n", "", 1)) }, []string{"findmnt --verify", "systemctl daemon-reload"}},
		{"line of another type", func() { f.set(t, "fstab", strings.Replace(written, " swap defaults", " ext4 defaults", 1)) }, []string{"findmnt --verify", "systemctl daemon-reload"}},
		{"mode", func() { os.Chmod(s.Path, 0644) }, nil},
		{"unfinished file of an interrupted run", func() { os.WriteFile(filepath.Join(f.dir, ".swapfile.mmx-new"), []byte("partial"), 0600) }, nil},
	} {
		tc.undo()
		requireResult(t, (Runtime{}).SwapFile(ctx, s), "succeeded", "changed", "")
		if calls := f.calls(t); !callsStartWith(calls, tc.calls) {
			t.Errorf("%s: %v", tc.name, calls)
		}
		requireSwapFile(t, f, s)
		if f.read(t, "fstab") != written {
			t.Fatalf("%s: %q", tc.name, f.read(t, "fstab"))
		}
		if _, err := os.Lstat(filepath.Join(f.dir, ".swapfile.mmx-new")); !os.IsNotExist(err) {
			t.Fatal("the unfinished file is still there")
		}
	}
	// A line written in another form, such as Ubuntu's, is kept as it is.
	f.set(t, "fstab", strings.Replace(written, s.Path+" none swap defaults 0 0", s.Path+"   swap   swap   sw   0   0", 1))
	requireResult(t, (Runtime{}).SwapFile(ctx, s), "succeeded", "unchanged", "")
}

// A swap file of another size is switched off, deleted, written again, and
// switched on; a file with holes counts as one of another size.
func TestSwapFileReplacesASwapFileOfAnotherSize(t *testing.T) {
	ctx := context.Background()
	f, s := swapFixture(t)
	requireResult(t, (Runtime{}).SwapFile(ctx, s), "succeeded", "changed", "")
	f.calls(t)
	s.Size = "3MiB"
	requireResult(t, (Runtime{Dryrun: true}).SwapFile(ctx, s), "succeeded", "would_change", "would switch off "+s.Path+", then delete "+s.Path+", which is not 3 MiB written in full, then write")
	requireResult(t, (Runtime{}).SwapFile(ctx, s), "succeeded", "changed", "switched off "+s.Path+", deleted the old "+s.Path+", wrote "+s.Path+" with 3 MiB")
	if calls := f.calls(t); !callsStartWith(calls, []string{"swapoff " + s.Path, "mkswap ", "swapon " + s.Path}) {
		t.Fatal(calls)
	}
	requireSwapFile(t, f, s)
	if strings.Count(f.read(t, "fstab"), s.Path) != 1 {
		t.Fatal(f.read(t, "fstab"))
	}
}

// A file at the path without a swap signature may hold data: the task never
// replaces, formats, or deletes it.
func TestSwapFileRefusesAFileThatIsNotASwapFile(t *testing.T) {
	ctx := context.Background()
	f, s := swapFixture(t)
	data := strings.Repeat("customer records\n", 1000)
	if err := os.WriteFile(s.Path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	for _, task := range []SwapFile{s, {Path: s.Path, State: "absent"}} {
		for _, dryrun := range []bool{true, false} {
			requireResult(t, (Runtime{Dryrun: dryrun}).SwapFile(ctx, task), "failed", "unchanged", s.Path+" has no swap signature and may hold data")
		}
	}
	if b, _ := os.ReadFile(s.Path); string(b) != data || len(f.calls(t)) != 0 {
		t.Fatal("the data file was touched")
	}
}

func TestSwapFileAbsent(t *testing.T) {
	ctx := context.Background()
	f, s := swapFixture(t)
	before := f.read(t, "fstab")
	requireResult(t, (Runtime{}).SwapFile(ctx, s), "succeeded", "changed", "")
	f.calls(t)
	gone := SwapFile{Path: s.Path, State: "absent"}
	requireResult(t, (Runtime{Dryrun: true}).SwapFile(ctx, gone), "succeeded", "would_change", "would switch off "+s.Path+", then remove the swap file's line from "+fstabPath+", then delete "+s.Path)
	requireResult(t, (Runtime{}).SwapFile(ctx, gone), "succeeded", "changed", "switched off "+s.Path+", removed the swap file's line from "+fstabPath+", deleted "+s.Path)
	if _, err := os.Lstat(s.Path); !os.IsNotExist(err) {
		t.Fatal("the file is still there")
	}
	if active, _ := swapActive(s.Path); active || f.read(t, "fstab") != before {
		t.Fatal("still active or listed")
	}
	requireResult(t, (Runtime{}).SwapFile(ctx, gone), "succeeded", "unchanged", "")
	// A directory that does not exist holds no swap file.
	requireResult(t, (Runtime{}).SwapFile(ctx, SwapFile{Path: filepath.Join(f.dir, "missing", "swapfile"), State: "absent"}), "succeeded", "unchanged", "")
}

// A failure keeps what was done and names it; the next run does the rest.
// A swap file that could not be finished is deleted.
func TestSwapFileAfterFailures(t *testing.T) {
	ctx := context.Background()
	f, s := swapFixture(t)
	f.set(t, "mkswap-fails", "")
	requireResult(t, (Runtime{}).SwapFile(ctx, s), "failed", "unknown", "mkswap "+filepath.Join(f.dir, ".swapfile.mmx-new")+" failed: mkswap: injected failure")
	for _, name := range []string{"swapfile", ".swapfile.mmx-new"} {
		if _, err := os.Lstat(filepath.Join(f.dir, name)); !os.IsNotExist(err) {
			t.Fatal(name, "is left")
		}
	}
	f.remove(t, "mkswap-fails")

	f.set(t, "swapon-fails", "")
	requireResult(t, (Runtime{}).SwapFile(ctx, s), "failed", "changed", "swapon "+s.Path+" failed: swapon: injected failure; already done: wrote "+s.Path)
	f.remove(t, "swapon-fails")
	f.calls(t)
	requireResult(t, (Runtime{}).SwapFile(ctx, s), "succeeded", "changed", "switched on "+s.Path+", added the swap file's line")
	if calls := f.calls(t); !callsStartWith(calls, []string{"swapon " + s.Path, "findmnt --verify", "systemctl daemon-reload"}) {
		t.Fatal(calls)
	}
	requireSwapFile(t, f, s)

	// The test boundary refuses the first change of a run.
	refusing := Runtime{BeforeMutation: func(string) error { return os.ErrPermission }}
	s.Size = "1MiB"
	requireResult(t, refusing.SwapFile(ctx, s), "failed", "unknown", "permission denied")
}

func TestSwapFileWaitsForItsDirectoryAndPrograms(t *testing.T) {
	ctx := context.Background()
	f, s := swapFixture(t)
	missing := SwapFile{Path: filepath.Join(f.dir, "missing", "swapfile"), Size: "1MiB"}
	requireResult(t, (Runtime{Dryrun: true}).SwapFile(ctx, missing), "not_checked", "unknown", "the directory "+filepath.Dir(missing.Path)+" does not exist yet")
	requireResult(t, (Runtime{}).SwapFile(ctx, missing), "failed", "unknown", "does not exist yet")
	t.Setenv("PATH", t.TempDir())
	requireResult(t, (Runtime{Dryrun: true}).SwapFile(ctx, s), "not_checked", "unknown", missingProgramError("mkswap").Error())
	requireResult(t, (Runtime{}).SwapFile(ctx, s), "failed", "unknown", "mkswap is not installed")
}

func TestSwapFileRejectsInvalidInput(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		s    SwapFile
		want string
	}{
		{SwapFile{Path: "/swapfile"}, "size is required"},
		{SwapFile{Path: "/swapfile", Size: "-1MiB"}, "a whole number above 0"},
		{SwapFile{Path: "/swapfile", Size: "1MiB", State: "off"}, "state must be present or absent"},
		{SwapFile{Path: "/swapfile", Size: "1MiB", State: "absent"}, "accepts only path and state"},
	} {
		r := (Runtime{}).SwapFile(context.Background(), tc.s)
		if r.Status != "failed" || r.FailureKind != "config" || !strings.Contains(r.Reason, tc.want) {
			t.Errorf("%+v: %+v", tc.s, r)
		}
	}
	for _, path := range []string{"/", "swapfile", "/swap\nfile"} {
		if swapFilePath(path) == nil {
			t.Errorf("%q passed", path)
		}
	}
}

func TestSwapFileBinarySizeUnits(t *testing.T) {
	for text, expected := range map[string]int64{"2MiB": 2097152, "4GiB": 4294967296, "1TiB": 1099511627776} {
		got, err := wholeBinarySize(text)
		if err != nil || got != expected {
			t.Fatalf("%s: %d %v", text, got, err)
		}
	}
	for _, text := range []string{"0GiB", "4G", "1.5GiB", "-1MiB", "4GB"} {
		if _, err := wholeBinarySize(text); err == nil {
			t.Fatalf("accepted %s", text)
		}
	}
}
