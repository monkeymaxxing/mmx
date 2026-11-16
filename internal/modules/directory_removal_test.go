package modules

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"

	"mmx/internal/protocol"
	"mmx/internal/value"
)

// tree creates directories and files below root; a name ending in / is a
// directory.
func tree(t *testing.T, root string, entries ...string) {
	t.Helper()
	for _, entry := range entries {
		path := filepath.Join(root, entry)
		var err error
		if strings.HasSuffix(entry, "/") {
			err = os.MkdirAll(path, 0755)
		} else if err = os.MkdirAll(filepath.Dir(path), 0755); err == nil {
			err = os.WriteFile(path, []byte(entry), 0644)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
}

func deleted(t *testing.T, r protocol.Result, status, change string, entries int, names ...string) {
	t.Helper()
	v, _ := r.Value.(map[string]any)
	got, _ := v["deleted_names"].([]string)
	if r.Status != status || r.Change != change || v["deleted_entries"] != entries || !slices.Equal(got, names) {
		t.Fatalf("got %+v, want %s %s, %d entries %v", r, status, change, entries, names)
	}
}

func TestDirectoryAbsentDeletesOnlyAnEmptyDirectoryUnlessTold(t *testing.T) {
	t.Parallel()
	parent := t.TempDir()
	path := filepath.Join(parent, "old-api")
	tree(t, parent, "old-api/")
	d := Directory{Path: path, State: "absent"}
	deleted(t, (Runtime{Dryrun: true}).Directory(context.Background(), d, value.Scope{}), "succeeded", "would_change", 1)
	if _, err := os.Stat(path); err != nil {
		t.Fatal("dryrun deleted", err)
	}
	deleted(t, (Runtime{}).Directory(context.Background(), d, value.Scope{}), "succeeded", "changed", 1)
	if found := names(t, parent); !slices.Equal(found, []string{"."}) {
		t.Fatal(found)
	}
	if r := (Runtime{}).Directory(context.Background(), d, value.Scope{}); r.Status != "succeeded" || r.Change != "unchanged" {
		t.Fatal(r)
	}
	// A directory with entries stays, in dryrun and run alike.
	tree(t, parent, "old-api/config/app.conf")
	for _, run := range []Runtime{{Dryrun: true}, {}} {
		if r := run.Directory(context.Background(), d, value.Scope{}); r.Status != "failed" || !strings.Contains(r.Reason, "is not empty (2 entries inside); write also_delete_everything_inside: true") {
			t.Fatal(r)
		}
	}
	if found := names(t, parent); len(found) != 4 {
		t.Fatal(found)
	}
	// A missing parent means a missing path.
	missing := Directory{Path: filepath.Join(parent, "missing/child"), State: "absent", AlsoDeleteEverythingInside: true}
	if r := (Runtime{}).Directory(context.Background(), missing, value.Scope{}); r.Status != "succeeded" || r.Change != "unchanged" {
		t.Fatal(r)
	}
}

func TestDirectoryAbsentDeletesTheWholeTreeWithoutFollowingLinks(t *testing.T) {
	t.Parallel()
	parent := t.TempDir()
	outside := t.TempDir()
	tree(t, outside, "keep")
	path := filepath.Join(parent, "old-api")
	tree(t, parent, "old-api/bin/app", "old-api/data/one", "old-api/data/two", "old-api/empty/", "beside")
	if err := os.Symlink(outside, filepath.Join(path, "data/outside")); err != nil {
		t.Fatal(err)
	}
	d := Directory{Path: path, State: "absent", AlsoDeleteEverythingInside: true}
	// The directory, bin, bin/app, data, its two files and link, and empty.
	deleted(t, (Runtime{Dryrun: true}).Directory(context.Background(), d, value.Scope{}), "succeeded", "would_change", 8)
	refused := Runtime{BeforeMutation: func(string) error { return errors.New("stop here") }}
	if r := refused.Directory(context.Background(), d, value.Scope{}); r.Status != "failed" {
		t.Fatal(r)
	}
	if found := names(t, parent); len(found) != 10 {
		t.Fatal("deleted before the mutation boundary", found)
	}
	deleted(t, (Runtime{}).Directory(context.Background(), d, value.Scope{}), "succeeded", "changed", 8)
	if found := names(t, parent); !slices.Equal(found, []string{".", "beside"}) {
		t.Fatal(found)
	}
	if found := names(t, outside); !slices.Equal(found, []string{".", "keep"}) {
		t.Fatal("followed a link", found)
	}
	for _, run := range []Runtime{{Dryrun: true}, {}} {
		if r := run.Directory(context.Background(), d, value.Scope{}); r.Status != "succeeded" || r.Change != "unchanged" {
			t.Fatal(r)
		}
	}
}

// A deletion that stopped half way leaves only the staging directory beside
// the path; the next run deletes it, also when the path exists again.
func TestDirectoryAbsentFinishesAnInterruptedDeletion(t *testing.T) {
	t.Parallel()
	parent := t.TempDir()
	path := filepath.Join(parent, "old-api")
	d := Directory{Path: path, State: "absent", AlsoDeleteEverythingInside: true}
	if os.Geteuid() == 0 {
		t.Skip("root deletes from a directory it cannot write")
	}
	tree(t, parent, "old-api/data/one", "old-api/data/two")
	// A directory that cannot be written stops the deletion after the rename.
	if err := os.Chmod(filepath.Join(path, "data"), 0500); err != nil {
		t.Fatal(err)
	}
	r := (Runtime{}).Directory(context.Background(), d, value.Scope{})
	if r.Status != "failed" || r.Change != "changed" {
		t.Fatal(r)
	}
	staging := filepath.Join(parent, ".mmx-staging-old-api")
	if found := names(t, parent); !slices.Contains(found, ".mmx-staging-old-api/data/one") || slices.Contains(found, "old-api") {
		t.Fatal(found)
	}
	if err := os.Chmod(filepath.Join(staging, "data"), 0755); err != nil {
		t.Fatal(err)
	}
	// The path itself is gone, but the task still has work: the leftover.
	deleted(t, (Runtime{Dryrun: true}).Directory(context.Background(), d, value.Scope{}), "succeeded", "would_change", 4)
	tree(t, parent, "old-api/again")
	deleted(t, (Runtime{}).Directory(context.Background(), d, value.Scope{}), "succeeded", "changed", 6)
	if found := names(t, parent); !slices.Equal(found, []string{"."}) {
		t.Fatal(found)
	}
	// Without also_delete_everything_inside, a task does not touch the
	// leftover of another task's deletion.
	tree(t, parent, ".mmx-staging-old-api/data/one")
	if r := (Runtime{}).Directory(context.Background(), Directory{Path: path, State: "absent"}, value.Scope{}); r.Change != "unchanged" {
		t.Fatal(r)
	}
}

func TestDirectoryDeletesEverythingInsideExceptTheListedNames(t *testing.T) {
	t.Parallel()
	releases := filepath.Join(t.TempDir(), "releases")
	tree(t, releases, "1.0/bin/app", "1.1/bin/app", "1.2/", "notes.txt", ".hidden")
	if err := os.Symlink("1.2", filepath.Join(releases, "current")); err != nil {
		t.Fatal(err)
	}
	d := Directory{Path: releases, DeleteEverythingInsideExcept: []string{"1.2", "1.1", "1.3"}}
	deleted(t, (Runtime{Dryrun: true}).Directory(context.Background(), d, value.Scope{}), "succeeded", "would_change", 6, ".hidden", "1.0", "current", "notes.txt")
	if found := names(t, releases); len(found) != 11 {
		t.Fatal("dryrun deleted", found)
	}
	deleted(t, (Runtime{}).Directory(context.Background(), d, value.Scope{}), "succeeded", "changed", 6, ".hidden", "1.0", "current", "notes.txt")
	if found := names(t, releases); !slices.Equal(found, []string{".", "1.1", "1.1/bin", "1.1/bin/app", "1.2"}) {
		t.Fatal(found)
	}
	for _, run := range []Runtime{{Dryrun: true}, {}} {
		if r := run.Directory(context.Background(), d, value.Scope{}); r.Status != "succeeded" || r.Change != "unchanged" {
			t.Fatal(r)
		}
	}
	// An empty list empties the directory and keeps it.
	d.DeleteEverythingInsideExcept = []string{}
	deleted(t, (Runtime{}).Directory(context.Background(), d, value.Scope{}), "succeeded", "changed", 4, "1.1", "1.2")
	if found := names(t, releases); !slices.Equal(found, []string{"."}) {
		t.Fatal(found)
	}
}

// With state: present, the directory exists: a missing one is created with
// the task's owner, group, and permissions_mode, as any present directory
// is, and holds none of the names to delete. The names are not created.
func TestDeleteEverythingInsideExceptCreatesAMissingDirectory(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	uid, gid := os.Geteuid(), os.Getegid()
	d := Directory{Path: filepath.Join(base, "releases"), Owner: Account(fmt.Sprint(uid)), Group: Account(fmt.Sprint(gid)), Mode: "0750", DeleteEverythingInsideExcept: []string{"1.0"}}
	preview := (Runtime{Dryrun: true}).Directory(context.Background(), d, value.Scope{})
	if preview.Status != "succeeded" || preview.Change != "would_change" || preview.DirectoryChange == nil || len(preview.DirectoryChange.Examples) != 1 {
		t.Fatalf("%+v", preview)
	}
	if example := preview.DirectoryChange.Examples[0]; example.Path != d.Path || example.Action != "create" || *example.After != (protocol.FileState{Kind: "directory", Mode: "0750", UID: uid, GID: gid}) {
		t.Fatalf("%+v %+v", example, example.After)
	}
	if _, err := os.Lstat(d.Path); !os.IsNotExist(err) {
		t.Fatal("the dryrun created the directory", err)
	}
	if r := (Runtime{}).Directory(context.Background(), d, value.Scope{}); r.Status != "succeeded" || r.Change != "changed" {
		t.Fatalf("%+v", r)
	}
	info, err := os.Lstat(d.Path)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0750 || info.Sys().(*syscall.Stat_t).Uid != uint32(uid) || info.Sys().(*syscall.Stat_t).Gid != uint32(gid) {
		t.Fatal(info, err)
	}
	if found := names(t, d.Path); !slices.Equal(found, []string{"."}) {
		t.Fatal("created a listed name", found)
	}
	for _, run := range []Runtime{{Dryrun: true}, {}} {
		if r := run.Directory(context.Background(), d, value.Scope{}); r.Status != "succeeded" || r.Change != "unchanged" {
			t.Fatalf("%+v", r)
		}
	}
	// Without permissions_mode, a new directory gets 0755.
	plain := Directory{Path: filepath.Join(base, "cache"), DeleteEverythingInsideExcept: []string{}}
	if r := (Runtime{}).Directory(context.Background(), plain, value.Scope{}); r.Status != "succeeded" || r.Change != "changed" {
		t.Fatalf("%+v", r)
	}
	if info, err := os.Lstat(plain.Path); err != nil || !info.IsDir() || info.Mode().Perm() != 0755 {
		t.Fatal(info, err)
	}
}

// An entry whose deletion stopped half way is left under its staging name
// inside the directory, which is not listed, so the next run deletes it.
func TestDeleteEverythingInsideExceptFinishesAnInterruptedDeletion(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("root deletes from a directory it cannot write")
	}
	releases := filepath.Join(t.TempDir(), "releases")
	tree(t, releases, "1.0/lib/a", "1.0/lib/b", "2.0/")
	if err := os.Chmod(filepath.Join(releases, "1.0/lib"), 0500); err != nil {
		t.Fatal(err)
	}
	d := Directory{Path: releases, DeleteEverythingInsideExcept: []string{"2.0"}}
	if r := (Runtime{}).Directory(context.Background(), d, value.Scope{}); r.Status != "failed" || r.Change != "changed" {
		t.Fatal(r)
	}
	if found := names(t, releases); !slices.Contains(found, ".mmx-staging-1.0/lib/a") || slices.Contains(found, "1.0") {
		t.Fatal(found)
	}
	if err := os.Chmod(filepath.Join(releases, ".mmx-staging-1.0/lib"), 0755); err != nil {
		t.Fatal(err)
	}
	deleted(t, (Runtime{}).Directory(context.Background(), d, value.Scope{}), "succeeded", "changed", 4, ".mmx-staging-1.0")
	if found := names(t, releases); !slices.Equal(found, []string{".", "2.0"}) {
		t.Fatal(found)
	}
}

func TestDirectoryDeletionRefusesLinksAndFiles(t *testing.T) {
	t.Parallel()
	parent := t.TempDir()
	target := filepath.Join(parent, "target")
	tree(t, parent, "target/keep", "file")
	if err := os.Symlink(target, filepath.Join(parent, "link")); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		d      Directory
		reason string
	}{
		{Directory{Path: filepath.Join(parent, "link"), State: "absent", AlsoDeleteEverythingInside: true}, "is a symlink"},
		{Directory{Path: filepath.Join(parent, "link"), DeleteEverythingInsideExcept: []string{}}, "is a symlink"},
		{Directory{Path: filepath.Join(parent, "file"), State: "absent"}, "is not a directory"},
		{Directory{Path: filepath.Join(parent, "file"), DeleteEverythingInsideExcept: []string{}}, "is not a directory"},
	} {
		for _, run := range []Runtime{{Dryrun: true}, {}} {
			if r := run.Directory(context.Background(), tc.d, value.Scope{}); r.Status != "failed" || !strings.Contains(r.Reason, tc.reason) {
				t.Fatal(tc.d, r)
			}
		}
	}
	if found := names(t, parent); !slices.Equal(found, []string{".", "file", "link", "target", "target/keep"}) {
		t.Fatal(found)
	}
}

// A node task block hashes whether the directory exists and, for
// delete_everything_inside_except, its names, so every deletion and the
// creation of a missing directory change the tracked input, and a dryrun
// predicts the result.
func TestDeletionInput(t *testing.T) {
	t.Parallel()
	releases := filepath.Join(t.TempDir(), "releases")
	tree(t, releases, "2.0/", "1.0/", "notes")
	except := Directory{Path: releases, DeleteEverythingInsideExcept: []string{"2.0", "3.0"}}
	absent := Directory{Path: releases, State: "absent", AlsoDeleteEverythingInside: true}
	missing := Directory{Path: filepath.Join(releases, "missing"), DeleteEverythingInsideExcept: []string{"2.0"}}
	for _, tc := range []struct {
		d       Directory
		applied bool
		exists  bool
		names   []string
	}{
		{except, false, true, []string{"1.0", "2.0", "notes"}},
		{except, true, true, []string{"2.0"}},
		{absent, false, true, nil},
		{absent, true, false, nil},
		// delete_everything_inside_except creates a missing directory, empty.
		{missing, false, false, nil},
		{missing, true, true, nil},
	} {
		exists, names, err := DeletionInput(tc.d, tc.applied)
		if err != nil || exists != tc.exists || !slices.Equal(names, tc.names) {
			t.Fatal(tc, exists, names, err)
		}
	}
	if exists, _, err := DeletionInput(Directory{Path: filepath.Join(releases, "missing"), State: "absent"}, false); exists || err != nil {
		t.Fatal(exists, err)
	}
}

func TestDirectoryCleanupAppliesOnlyItsOwnMetadata(t *testing.T) {
	path := filepath.Join(t.TempDir(), "releases")
	tree(t, path, "keep/data", "remove/data")
	if err := os.Chmod(path, 0755); err != nil {
		t.Fatal(err)
	}
	kept, err := os.Stat(filepath.Join(path, "keep"))
	if err != nil {
		t.Fatal(err)
	}
	d := Directory{Path: path, Mode: "0700", Owner: Account(fmt.Sprint(os.Geteuid())), Group: Account(fmt.Sprint(os.Getegid())), DeleteEverythingInsideExcept: []string{"keep"}}
	for _, dryrun := range []bool{true, false} {
		r := (Runtime{Dryrun: dryrun}).Directory(context.Background(), d, value.Scope{})
		if r.Status != "succeeded" || (dryrun && r.Change != "would_change") || (!dryrun && r.Change != "changed") {
			t.Fatal(r)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		want := os.FileMode(0700)
		if dryrun {
			want = 0755
		}
		if info.Mode().Perm() != want {
			t.Fatal(info.Mode())
		}
	}
	info, _ := os.Stat(filepath.Join(path, "keep"))
	if info.Mode() != kept.Mode() {
		t.Fatal("changed kept directory metadata")
	}
	if r := (Runtime{}).Directory(context.Background(), d, value.Scope{}); r.Change != "unchanged" {
		t.Fatal(r)
	}
	// Metadata alone still changes the directory when there is nothing to delete.
	d.Mode = "0750"
	if r := (Runtime{}).Directory(context.Background(), d, value.Scope{}); r.Change != "changed" {
		t.Fatal(r)
	}
}
