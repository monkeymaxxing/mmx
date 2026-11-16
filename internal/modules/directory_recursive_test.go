package modules

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"mmx/internal/value"
)

func directoryFixture(t *testing.T) (string, map[string]os.FileInfo) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "tree")
	for _, path := range []string{root, filepath.Join(root, "nested")} {
		if err := os.Mkdir(path, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, 0755); err != nil {
			t.Fatal(err)
		}
	}
	files := map[string]os.FileInfo{}
	for _, name := range []string{"first", "nested/second"} {
		path := filepath.Join(root, name)
		if err := os.WriteFile(path, []byte("preserved contents"), 0644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, 0644); err != nil {
			t.Fatal(err)
		}
		files[path], _ = os.Stat(path)
	}
	return root, files
}
func requireMode(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	st, err := os.Stat(path)
	if err != nil || permissionMode(st.Mode()) != mode {
		t.Fatalf("%s: expected %v, got %v (%v)", path, mode, st, err)
	}
}
func TestRecursiveDirectoryMetadataAndPreview(t *testing.T) {
	t.Parallel()
	root, files := directoryFixture(t)
	d := Directory{Path: root, Recurse: true, Mode: "0750", FileMode: "0640"}
	r := (Runtime{Dryrun: true, BeforeMutation: func(string) error { t.Fatal("preview mutated"); return nil }}).Directory(context.Background(), d, value.Scope{})
	if r.Status != "succeeded" || r.Change != "would_change" || r.DirectoryChange.Changes != 4 || r.DirectoryChange.Applied != 0 {
		t.Fatal(r)
	}
	for path := range files {
		requireMode(t, path, 0644)
	}
	requireMode(t, root, 0755)
	requireMode(t, filepath.Join(root, "nested"), 0755)
	r = (Runtime{}).Directory(context.Background(), d, value.Scope{})
	if r.Status != "succeeded" || r.Change != "changed" || r.DirectoryChange.Applied != 4 {
		t.Fatal(r)
	}
	for path, before := range files {
		after, err := os.Stat(path)
		data, readErr := os.ReadFile(path)
		if err != nil || readErr != nil || !os.SameFile(before, after) || !before.ModTime().Equal(after.ModTime()) || string(data) != "preserved contents" {
			t.Fatal("rewrote contents", path, err, readErr)
		}
		requireMode(t, path, 0640)
	}
	requireMode(t, root, 0750)
	requireMode(t, filepath.Join(root, "nested"), 0750)
	r = (Runtime{BeforeMutation: func(string) error { t.Fatal("correct tree requested a mutation"); return nil }}).Directory(context.Background(), d, value.Scope{})
	if r.Change != "unchanged" || r.DirectoryChange.Files != 2 || r.DirectoryChange.Directories != 2 || r.DirectoryChange.Changes != 0 {
		t.Fatal(r)
	}
}
func TestRecursiveDirectoryPreservesOmittedModes(t *testing.T) {
	t.Parallel()
	root, _ := directoryFixture(t)
	executable := filepath.Join(root, "first")
	if err := os.Chmod(executable, 0751|os.ModeSetgid); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(root, 0755|os.ModeSticky); err != nil {
		t.Fatal(err)
	}
	d := Directory{Path: root, Recurse: true, Mode: "0700"}
	if r := (Runtime{}).Directory(context.Background(), d, value.Scope{}); r.Status != "succeeded" {
		t.Fatal(r)
	}
	requireMode(t, executable, 0751|os.ModeSetgid)
	requireMode(t, filepath.Join(root, "nested/second"), 0644)
	requireMode(t, root, 0700)
	if err := os.Chmod(root, 0700|os.ModeSticky); err != nil {
		t.Fatal(err)
	}
	d.Mode = ""
	d.FileMode = "0600"
	if r := (Runtime{}).Directory(context.Background(), d, value.Scope{}); r.Status != "succeeded" {
		t.Fatal(r)
	}
	requireMode(t, root, 0700|os.ModeSticky)
	requireMode(t, executable, 0600)
}
func TestRecursiveDirectorySkipsLinksAndSpecialEntries(t *testing.T) {
	t.Parallel()
	root, _ := directoryFixture(t)
	outside := t.TempDir()
	target := filepath.Join(outside, "file")
	if err := os.WriteFile(target, []byte("outside"), 0644); err != nil {
		t.Fatal(err)
	}
	before, _ := os.Stat(target)
	for name, target := range map[string]string{"external": outside, "internal": "nested", "dangling": "missing", "file-link": target} {
		if err := os.Symlink(target, filepath.Join(root, name)); err != nil {
			t.Fatal(err)
		}
	}
	fifo := filepath.Join(root, "fifo")
	if err := syscall.Mkfifo(fifo, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(fifo, 0644); err != nil {
		t.Fatal(err)
	}
	d := Directory{Path: root, Recurse: true, Mode: "0700", FileMode: "0600"}
	for _, dryrun := range []bool{true, false} {
		r := (Runtime{Dryrun: dryrun}).Directory(context.Background(), d, value.Scope{})
		if r.Status != "succeeded" || r.DirectoryChange.SkippedSymlinks != 4 || r.DirectoryChange.SkippedSpecial != 1 {
			t.Fatal(r)
		}
	}
	after, _ := os.Stat(target)
	if !os.SameFile(before, after) || permissionMode(before.Mode()) != permissionMode(after.Mode()) {
		t.Fatal("changed outside target")
	}
	requireMode(t, fifo, 0644)
	for _, name := range []string{"external", "internal", "dangling", "file-link"} {
		if _, err := os.Readlink(filepath.Join(root, name)); err != nil {
			t.Fatal(err)
		}
	}
}
func TestRecursiveDirectoryBatchesAndBoundsExamples(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	for i := 0; i < 300; i++ {
		if err := os.WriteFile(filepath.Join(root, fmt.Sprint(i)), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	d := Directory{Path: root, Recurse: true, FileMode: "0644"}
	for _, dryrun := range []bool{true, false} {
		r := (Runtime{Dryrun: dryrun}).Directory(context.Background(), d, value.Scope{})
		if r.Status != "succeeded" || r.DirectoryChange.Files != 300 || r.DirectoryChange.Directories != 1 || r.DirectoryChange.Changes != 300 || len(r.DirectoryChange.Examples) != 10 {
			t.Fatal(r)
		}
	}
}
func TestRecursiveDirectoryPartialFailureAndCancellation(t *testing.T) {
	t.Parallel()
	for _, cancelTask := range []bool{false, true} {
		root, _ := directoryFixture(t)
		ctx, cancel := context.WithCancel(context.Background())
		calls := 0
		runtime := Runtime{BeforeMutation: func(string) error {
			calls++
			if calls == 2 {
				if cancelTask {
					cancel()
					return ctx.Err()
				}
				return errors.New("injected metadata failure")
			}
			return nil
		}}
		d := Directory{Path: root, Recurse: true, Mode: "0700", FileMode: "0600"}
		r := runtime.Directory(ctx, d, value.Scope{})
		cancel()
		if r.Status != "failed" || r.Change != "changed" || r.DirectoryChange.Applied != 1 {
			t.Fatal(r)
		}
		r = (Runtime{}).Directory(context.Background(), d, value.Scope{})
		if r.Status != "succeeded" || r.DirectoryChange.Applied != 3 {
			t.Fatal(r)
		}
		if r = (Runtime{}).Directory(context.Background(), d, value.Scope{}); r.Change != "unchanged" {
			t.Fatal(r)
		}
	}
}
func TestRecursiveDirectoryCannotFollowReplacementLinks(t *testing.T) {
	t.Parallel()
	for _, replaceParent := range []bool{false, true} {
		base := t.TempDir()
		root := filepath.Join(base, "tree")
		nested := filepath.Join(root, "nested")
		if err := os.MkdirAll(nested, 0755); err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(nested, "file")
		if err := os.WriteFile(target, nil, 0644); err != nil {
			t.Fatal(err)
		}
		outside := filepath.Join(base, "outside")
		if err := os.Mkdir(outside, 0755); err != nil {
			t.Fatal(err)
		}
		outsideFile := filepath.Join(outside, "file")
		if err := os.WriteFile(outsideFile, nil, 0644); err != nil {
			t.Fatal(err)
		}
		before, _ := os.Stat(outsideFile)
		runtime := Runtime{BeforeMutation: func(path string) error {
			if path != target {
				return nil
			}
			if replaceParent {
				if err := os.Rename(nested, nested+"-moved"); err != nil {
					return err
				}
				return os.Symlink(outside, nested)
			}
			if err := os.Rename(target, target+"-moved"); err != nil {
				return err
			}
			return os.Symlink(outsideFile, target)
		}}
		r := runtime.Directory(context.Background(), Directory{Path: root, Recurse: true, FileMode: "0600"}, value.Scope{})
		if !replaceParent && r.Status != "failed" {
			t.Fatal("accepted replaced file", r)
		}
		after, _ := os.Stat(outsideFile)
		if !os.SameFile(before, after) || permissionMode(before.Mode()) != permissionMode(after.Mode()) {
			t.Fatal("followed replacement into outside tree")
		}
	}
}
func TestDirectoryAppliesRestrictiveModesAfterChildren(t *testing.T) {
	t.Parallel()
	root, files := directoryFixture(t)
	defer func() {
		os.Chmod(root, 0700)
		os.Chmod(filepath.Join(root, "nested"), 0700)
	}()
	r := (Runtime{}).Directory(context.Background(), Directory{Path: root, Recurse: true, Mode: "0000", FileMode: "0600"}, value.Scope{})
	if r.Status != "succeeded" {
		t.Fatal(r)
	}
	os.Chmod(root, 0700)
	os.Chmod(filepath.Join(root, "nested"), 0700)
	for path := range files {
		requireMode(t, path, 0600)
	}
}
func TestDirectoryDryrunMissingRootAndExplicitModes(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "missing")
	d := Directory{Path: path, Recurse: true, FileMode: "0600"}
	r := (Runtime{Dryrun: true}).Directory(context.Background(), d, value.Scope{})
	if r.DirectoryChange.Changes != 1 || r.DirectoryChange.Directories != 1 || r.DirectoryChange.Files != 0 || r.DirectoryChange.Examples[0].After.Mode != "0755" {
		t.Fatal(r)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if r = (Runtime{}).Directory(context.Background(), d, value.Scope{}); r.Status != "succeeded" {
		t.Fatal(r)
	}
	requireMode(t, path, 0755)
	d.Path = path + "-link"
	if err := os.Symlink(path, d.Path); err != nil {
		t.Fatal(err)
	}
	if r = (Runtime{}).Directory(context.Background(), d, value.Scope{}); r.Status != "failed" {
		t.Fatal(r)
	}
}
