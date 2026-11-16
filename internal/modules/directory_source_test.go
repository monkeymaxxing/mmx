package modules

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"mmx/internal/assets"
	"mmx/internal/value"
)

func copyTask(t *testing.T, destination string, files map[string]string) Directory {
	t.Helper()
	d := Directory{Path: destination, Source: "frozen-source"}
	entries := map[string]DirectoryEntry{".": {Path: ".", Directory: true, Mode: 0755}, "empty": {Path: "empty", Directory: true, Mode: 0750}}
	for name, data := range files {
		source := filepath.Join(t.TempDir(), "source")
		if err := os.WriteFile(source, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		ref, err := assets.Snapshot(context.Background(), t.TempDir(), source)
		if err != nil {
			t.Fatal(err)
		}
		entries[name] = DirectoryEntry{Path: name, Mode: 0644, Asset: &ref}
		for parent := filepath.Dir(name); parent != "."; parent = filepath.Dir(parent) {
			entries[parent] = DirectoryEntry{Path: parent, Directory: true, Mode: 0755}
		}
	}
	names := []string{}
	for name := range entries {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		d.Entries = append(d.Entries, entries[name])
	}
	return d
}

func TestDirectoryCopyEmptySource(t *testing.T) {
	t.Parallel()
	d := Directory{Path: filepath.Join(t.TempDir(), "empty"), Source: "frozen",
		Entries: []DirectoryEntry{{Path: ".", Directory: true, Mode: 0751}}}
	r := (Runtime{Dryrun: true}).Directory(context.Background(), d, value.Scope{})
	if r.Status != "succeeded" || r.Change != "would_change" {
		t.Fatal(r)
	}
	if _, err := os.Stat(d.Path); !os.IsNotExist(err) {
		t.Fatal("preview created the empty root", err)
	}
	r = (Runtime{}).Directory(context.Background(), d, value.Scope{})
	if r.Status != "succeeded" || r.DirectoryChange.Applied != 1 {
		t.Fatal(r)
	}
	info, err := os.Stat(d.Path)
	if err != nil || info.Mode().Perm() != 0751 {
		t.Fatal(info, err)
	}
	if r := (Runtime{}).Directory(context.Background(), d, value.Scope{}); r.Change != "unchanged" {
		t.Fatal(r)
	}
	// The filesystem root is also a valid existing destination. Preview only.
	d.Path = "/"
	r = (Runtime{Dryrun: true}).Directory(context.Background(), d, value.Scope{})
	if r.Status != "succeeded" || r.Change != "unchanged" || r.DirectoryChange.Directories != 1 {
		t.Fatal(r)
	}
}

// An empty source, such as a missing one with if_source_directory_is_missing:
// copy_nothing, leaves an existing destination and everything in it alone.
func TestDirectoryCopyEmptySourceKeepsWhatTheDestinationHolds(t *testing.T) {
	t.Parallel()
	d := Directory{Path: filepath.Join(t.TempDir(), "modules"), Source: "frozen",
		Entries: []DirectoryEntry{{Path: ".", Directory: true, Mode: 0755}}}
	if err := os.Mkdir(d.Path, 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d.Path, "earlier.sh"), []byte("kept\n"), 0644); err != nil {
		t.Fatal(err)
	}
	for _, dryrun := range []bool{true, false} {
		if r := (Runtime{Dryrun: dryrun}).Directory(context.Background(), d, value.Scope{}); r.Status != "succeeded" || r.Change != "unchanged" {
			t.Fatal(dryrun, r)
		}
	}
	info, err := os.Stat(d.Path)
	b, readErr := os.ReadFile(filepath.Join(d.Path, "earlier.sh"))
	if err != nil || info.Mode().Perm() != 0750 || readErr != nil || string(b) != "kept\n" {
		t.Fatal(info, err, readErr, string(b))
	}
}

func TestDirectoryCopyPreviewApplyAndUnchanged(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "target")
	literal := "{{ .secrets.literal }}"
	files := map[string]string{".hidden": "text\n", "nested/run": "#!/bin/sh\n", literal: "\x00\xffbytes"}
	d := copyTask(t, root, files)
	for i := range d.Entries {
		if d.Entries[i].Path == "nested/run" {
			d.Entries[i].Mode = 0751
		}
	}
	preview := (Runtime{Dryrun: true}).Directory(context.Background(), d, value.Scope{})
	if preview.Status != "succeeded" || preview.Change != "would_change" || preview.DirectoryChange.Files != 3 || preview.DirectoryChange.Directories != 3 || preview.DirectoryChange.Changes != 6 {
		t.Fatalf("%+v %+v", preview, preview.DirectoryChange)
	}
	if _, err := os.Stat(root); !os.IsNotExist(err) {
		t.Fatal("preview created target", err)
	}
	r := (Runtime{}).Directory(context.Background(), d, value.Scope{})
	if r.Status != "succeeded" || r.Change != "changed" || r.DirectoryChange.Applied != 6 {
		t.Fatalf("%+v %+v", r, r.DirectoryChange)
	}
	before := map[string]os.FileInfo{}
	for name, data := range files {
		p := filepath.Join(root, name)
		b, err := os.ReadFile(p)
		if err != nil || string(b) != data {
			t.Fatal(name, err)
		}
		before[name], _ = os.Stat(p)
	}
	if before["nested/run"].Mode().Perm() != 0751 {
		t.Fatal("lost source executable mode")
	}
	if err := os.WriteFile(filepath.Join(root, "unlisted"), []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	calls := 0
	run := Runtime{BeforeMutation: func(string) error { calls++; return nil }}
	r = run.Directory(context.Background(), d, value.Scope{})
	if r.Change != "unchanged" || r.DirectoryChange.Changes != 0 || calls != 0 {
		t.Fatal(r, calls)
	}
	for name, old := range before {
		now, _ := os.Stat(filepath.Join(root, name))
		if !os.SameFile(old, now) || !old.ModTime().Equal(now.ModTime()) {
			t.Fatal("unchanged copy rewrote", name)
		}
	}
	d.Mode, d.FileMode = "0700", "0640"
	r = run.Directory(context.Background(), d, value.Scope{})
	if r.Status != "succeeded" || r.DirectoryChange.Applied != 6 {
		t.Fatalf("%+v", r)
	}
	for name, old := range before {
		now, _ := os.Stat(filepath.Join(root, name))
		if !os.SameFile(old, now) || now.Mode().Perm() != 0640 {
			t.Fatal("metadata-only replacement", name)
		}
	}
	unlisted, _ := os.Stat(filepath.Join(root, "unlisted"))
	if unlisted.Mode().Perm() != 0600 {
		t.Fatal("changed unlisted metadata")
	}
	// Source-list removal is not destination pruning; omitted metadata is retained.
	kept := d.Entries[:0]
	for _, e := range d.Entries {
		if e.Path != ".hidden" {
			kept = append(kept, e)
		}
	}
	d.Entries = kept
	d.Mode, d.FileMode = "", ""
	r = run.Directory(context.Background(), d, value.Scope{})
	if r.Status != "succeeded" || r.Change != "unchanged" {
		t.Fatal(r)
	}
	if b, _ := os.ReadFile(filepath.Join(root, ".hidden")); string(b) != "text\n" {
		t.Fatal("pruned old source")
	}
}

func TestDirectoryCopyPreflightAndRaces(t *testing.T) {
	t.Parallel()
	for _, conflict := range []string{"root link", "directory link", "file link", "file directory", "directory file"} {
		t.Run(conflict, func(t *testing.T) {
			base := t.TempDir()
			root := filepath.Join(base, "target")
			outside := filepath.Join(base, "outside")
			os.Mkdir(root, 0755)
			os.Mkdir(outside, 0755)
			os.WriteFile(filepath.Join(outside, "victim"), []byte("keep"), 0600)
			switch conflict {
			case "root link":
				os.Remove(root)
				os.Symlink(outside, root)
			case "directory link":
				os.Symlink(outside, filepath.Join(root, "nested"))
			case "file link":
				os.Mkdir(filepath.Join(root, "nested"), 0755)
				os.Symlink(filepath.Join(outside, "victim"), filepath.Join(root, "nested/file"))
			case "file directory":
				os.MkdirAll(filepath.Join(root, "nested/file"), 0755)
			case "directory file":
				os.WriteFile(filepath.Join(root, "nested"), nil, 0600)
			}
			d := copyTask(t, root, map[string]string{"a": "new", "nested/file": "new"})
			d.AlsoDeleteEntriesNotInSource = true
			calls := 0
			r := (Runtime{BeforeMutation: func(string) error { calls++; return nil }}).Directory(context.Background(), d, value.Scope{})
			if r.Status != "failed" || calls != 0 {
				t.Fatal(r, calls)
			}
			b, _ := os.ReadFile(filepath.Join(outside, "victim"))
			if string(b) != "keep" {
				t.Fatal("followed symlink")
			}
		})
	}
	for _, replace := range []string{"file", "ancestor"} {
		t.Run("replace "+replace, func(t *testing.T) {
			base := t.TempDir()
			root := filepath.Join(base, "target")
			outside := filepath.Join(base, "outside")
			os.MkdirAll(filepath.Join(root, "nested"), 0755)
			os.Mkdir(outside, 0755)
			target := filepath.Join(root, "nested/file")
			os.WriteFile(target, []byte("old"), 0644)
			os.WriteFile(filepath.Join(outside, "file"), []byte("keep"), 0600)
			d := copyTask(t, root, map[string]string{"nested/file": "new"})
			run := Runtime{BeforeMutation: func(name string) error {
				if name != target {
					return nil
				}
				if replace == "file" {
					if err := os.Remove(target); err != nil {
						return err
					}
					return os.Symlink(filepath.Join(outside, "file"), target)
				}
				if err := os.Rename(filepath.Join(root, "nested"), filepath.Join(base, "moved")); err != nil {
					return err
				}
				return os.Symlink(outside, filepath.Join(root, "nested"))
			}}
			r := run.Directory(context.Background(), d, value.Scope{})
			if r.Status != "failed" {
				t.Fatal(r)
			}
			b, _ := os.ReadFile(filepath.Join(outside, "file"))
			if string(b) != "keep" {
				t.Fatal("redirected mutation")
			}
		})
	}
}

func TestDirectoryCopyPartialFailureAndBoundedPrivatePreviews(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "target")
	files := map[string]string{}
	for i := 0; i < 25; i++ {
		files[fmt.Sprintf("file-%02d", i)] = "visible\n"
	}
	d := copyTask(t, root, files)
	fail := filepath.Join(root, "file-01")
	run := Runtime{BeforeMutation: func(name string) error {
		if name == fail {
			return fmt.Errorf("injected copy failure")
		}
		return nil
	}}
	r := run.Directory(context.Background(), d, value.Scope{})
	if r.Status != "failed" || r.Change != "changed" || r.DirectoryChange.Applied != 3 {
		t.Fatalf("%+v %+v", r, r.DirectoryChange)
	}
	info, err := os.Stat(root)
	if err != nil || info.Mode().Perm() != 0755 {
		t.Fatal("partial copy left temporary root permissions", info, err)
	}
	r = (Runtime{}).Directory(context.Background(), d, value.Scope{})
	if r.Status != "succeeded" || r.DirectoryChange.Applied != 24 || len(r.DirectoryChange.Examples) != 10 {
		t.Fatalf("%+v %+v", r, r.DirectoryChange)
	}
	for i := range d.Entries {
		if d.Entries[i].Asset != nil {
			copy := copyTask(t, root, map[string]string{"new": "private-value"})
			d.Entries[i].Asset = copy.Entries[2].Asset
		}
	}
	// Use a private destination; even a public desired mode cannot expose its old bytes.
	private := filepath.Join(root, "file-00")
	os.Chmod(private, 0600)
	d.FileMode = "0644"
	r = (Runtime{Dryrun: true}).Directory(context.Background(), d, value.Scope{})
	if r.Status != "succeeded" {
		t.Fatal(r)
	}
	for _, e := range r.DirectoryChange.Examples {
		if e.Path == private && (e.Diff != "" || !strings.Contains(e.DiffOmitted, "private")) {
			t.Fatal(e)
		}
	}
	r.Secret = true
	b, err := json.Marshal(r)
	if err != nil || strings.Contains(string(b), "private-value") {
		t.Fatal("secret directory diff retained", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	calls := 0
	r = (Runtime{BeforeMutation: func(string) error { calls++; return nil }}).Directory(ctx, d, value.Scope{})
	if r.Status != "failed" || calls != 0 {
		t.Fatal("ignored cancellation", r)
	}
}

func TestDirectoryCopyDeletesEntriesNotInSource(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "target")
	d := copyTask(t, root, map[string]string{"keep": "kept", "nested/keep": "nested bytes"})
	if r := (Runtime{}).Directory(context.Background(), d, value.Scope{}); r.Status != "succeeded" {
		t.Fatal(r)
	}
	kept := map[string]os.FileInfo{}
	for _, name := range []string{"keep", "nested/keep", "empty"} {
		kept[name], _ = os.Stat(filepath.Join(root, name))
	}
	tree(t, root, "extra", "nested/extra", "old/deep/file", ".mmx-staging-interrupted/data")
	outside := t.TempDir()
	tree(t, outside, "untouched")
	if err := os.Symlink(outside, filepath.Join(root, "nested/link")); err != nil {
		t.Fatal(err)
	}
	before := names(t, root)
	// The default still preserves everything that was not copied.
	if r := (Runtime{}).Directory(context.Background(), d, value.Scope{}); r.Change != "unchanged" {
		t.Fatal(r)
	}
	d.AlsoDeleteEntriesNotInSource = true
	preview := (Runtime{Dryrun: true}).Directory(context.Background(), d, value.Scope{})
	deleted(t, preview, "succeeded", "would_change", 8)
	if preview.DirectoryChange.Changes != 8 || preview.DirectoryChange.Applied != 0 || len(preview.DirectoryChange.Examples) != 8 || preview.DirectoryChange.Directories+preview.DirectoryChange.Files-preview.DirectoryChange.Changes != 5 {
		t.Fatalf("%+v", preview.DirectoryChange)
	}
	if found := names(t, root); !reflect.DeepEqual(found, before) {
		t.Fatal("dryrun deleted entries", found)
	}
	for _, example := range preview.DirectoryChange.Examples {
		if example.Action != "delete" || example.Before == nil || example.After != nil {
			t.Fatal(example)
		}
	}
	refused := (Runtime{BeforeMutation: func(string) error { return fmt.Errorf("injected deletion refusal") }}).Directory(context.Background(), d, value.Scope{})
	deleted(t, refused, "failed", "unknown", 0)
	if !reflect.DeepEqual(names(t, root), before) {
		t.Fatal("deleted before mutation was allowed")
	}
	applied := (Runtime{}).Directory(context.Background(), d, value.Scope{})
	deleted(t, applied, "succeeded", "changed", 8)
	if applied.DirectoryChange.Changes != 8 || applied.DirectoryChange.Applied != 8 {
		t.Fatalf("%+v", applied.DirectoryChange)
	}
	if found := names(t, root); !reflect.DeepEqual(found, []string{".", "empty", "keep", "nested", "nested/keep"}) {
		t.Fatal(found)
	}
	for name, old := range kept {
		now, err := os.Stat(filepath.Join(root, name))
		if err != nil || !os.SameFile(old, now) || old.Mode() != now.Mode() || !old.ModTime().Equal(now.ModTime()) {
			t.Fatal("changed a source-owned entry", name, err)
		}
	}
	if data, err := os.ReadFile(filepath.Join(outside, "untouched")); err != nil || string(data) != "untouched" {
		t.Fatal("followed a link", string(data), err)
	}
	for _, dryrun := range []bool{true, false} {
		r := (Runtime{Dryrun: dryrun}).Directory(context.Background(), d, value.Scope{})
		deleted(t, r, "succeeded", "unchanged", 0)
	}
}

func TestDirectoryCopyDeletionPreviewIsBounded(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "target")
	d := copyTask(t, root, nil)
	if r := (Runtime{}).Directory(context.Background(), d, value.Scope{}); r.Status != "succeeded" {
		t.Fatal(r)
	}
	for i := 0; i < 15; i++ {
		tree(t, root, fmt.Sprintf("extra-%02d", i))
	}
	d.AlsoDeleteEntriesNotInSource = true
	r := (Runtime{Dryrun: true}).Directory(context.Background(), d, value.Scope{})
	deleted(t, r, "succeeded", "would_change", 15)
	if r.DirectoryChange.Changes != 15 || len(r.DirectoryChange.Examples) != 10 || len(names(t, root)) != 17 {
		t.Fatalf("%+v", r.DirectoryChange)
	}
}

func TestDirectoryCopyDeletionValidation(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		d      Directory
		reason string
	}{
		{Directory{AlsoDeleteEntriesNotInSource: true}, "directory.also_delete_entries_not_in_source requires source"},
		{Directory{Source: "files", AlsoDeleteEntriesNotInSource: true, IfSourceDirectoryIsMissing: "copy_nothing"}, "a missing source directory on the controller machine would delete everything inside path"},
		{Directory{Source: "files", AlsoDeleteEntriesNotInSource: true, Recurse: true}, "directory.source cannot use also_apply_to_everything_inside: true"},
		{Directory{Source: "files", AlsoDeleteEntriesNotInSource: true, State: "absent"}, "directory with state: absent accepts only path, state, and also_delete_everything_inside"},
		{Directory{Source: "files", AlsoDeleteEntriesNotInSource: true, AlsoDeleteEverythingInside: true}, "directory.also_delete_everything_inside goes with state: absent"},
		{Directory{Source: "files", AlsoDeleteEntriesNotInSource: true, DeleteEverythingInsideExcept: []string{}}, "directory.delete_everything_inside_except accepts path, create_missing_parent_directories, owner, group, and permissions_mode"},
	} {
		if err := ValidateDirectory(tc.d); err == nil || !strings.Contains(err.Error(), tc.reason) {
			t.Fatal(tc.d, err)
		}
	}
}

func TestDirectoryCopyDeletionPreservesSourceOwnedStagingName(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "target")
	d := copyTask(t, root, map[string]string{".mmx-staging-old/keep": "kept"})
	if r := (Runtime{}).Directory(context.Background(), d, value.Scope{}); r.Status != "succeeded" {
		t.Fatal(r)
	}
	tree(t, root, "old/data")
	d.AlsoDeleteEntriesNotInSource = true
	before := names(t, root)
	for _, dryrun := range []bool{true, false} {
		r := (Runtime{Dryrun: dryrun}).Directory(context.Background(), d, value.Scope{})
		if r.Status != "failed" || !strings.Contains(r.Reason, "belongs to the source") || !reflect.DeepEqual(names(t, root), before) {
			t.Fatal(r)
		}
	}
}

func TestDirectoryCopyDeletionUsesSafePathRule(t *testing.T) {
	t.Parallel()
	for _, path := range []string{"/", "/srv", "/srv/../etc"} {
		d := Directory{Path: path, Source: "files", AlsoDeleteEntriesNotInSource: true}
		arguments := ArgumentList{}
		d.Arguments(&arguments)
		if err := arguments.List[0].Rule(path); err == nil || !strings.Contains(err.Error(), "at least two levels deep") {
			t.Fatal(path, err)
		}
	}
}
