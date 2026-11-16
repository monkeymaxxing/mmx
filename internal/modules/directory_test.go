package modules

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"mmx/internal/protocol"
	"mmx/internal/value"
)

func TestDirectoryLifecycle(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "app")
	d := Directory{Path: path, Mode: "0750"}
	if r := (Runtime{Dryrun: true}).Directory(context.Background(), d, value.Scope{}); r.Change != "would_change" {
		t.Fatal(r)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("dryrun created directory")
	}
	if r := (Runtime{}).Directory(context.Background(), d, value.Scope{}); r.Status != "succeeded" || r.Change != "changed" {
		t.Fatal(r)
	}
	if r := (Runtime{}).Directory(context.Background(), d, value.Scope{}); r.Change != "unchanged" {
		t.Fatal(r)
	}
	child := filepath.Join(path, "keep")
	if err := os.WriteFile(child, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	before, _ := os.Stat(path)
	d.Mode = "0700"
	if r := (Runtime{}).Directory(context.Background(), d, value.Scope{}); r.Change != "changed" {
		t.Fatal(r)
	}
	after, err := os.Stat(path)
	if err != nil || !os.SameFile(before, after) || after.Mode().Perm() != 0700 {
		t.Fatal(after, err)
	}
	data, err := os.ReadFile(child)
	if err != nil || string(data) != "keep" {
		t.Fatal("changed child contents", err)
	}
	link := path + "-link"
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	d.Path = link
	if r := (Runtime{}).Directory(context.Background(), d, value.Scope{}); r.Status != "failed" {
		t.Fatal(r)
	}
}

// A numeric owner or group is used as the ID itself, with no account lookup.
func TestNumericOwnerAndGroupNeedNoAccount(t *testing.T) {
	t.Parallel()
	uid, gid := Account(strconv.Itoa(os.Getuid())), Account(strconv.Itoa(os.Getgid()))
	d := Directory{Path: filepath.Join(t.TempDir(), "data"), Owner: uid, Group: gid}
	if r := (Runtime{}).Directory(context.Background(), d, value.Scope{}); r.Status != "succeeded" || r.Change != "changed" {
		t.Fatal(r)
	}
	if r := (Runtime{}).Directory(context.Background(), d, value.Scope{}); r.Status != "succeeded" || r.Change != "unchanged" {
		t.Fatal(r)
	}
	// An ID with no account resolves; only the kernel may refuse the change.
	m, err := metadataFor(nil, 0755, "", "4000000123", "4000000124")
	if err != nil || m.uid != 4000000123 || m.gid != 4000000124 {
		t.Fatal(m, err)
	}
	// A name is looked up as before.
	if _, err := metadataFor(nil, 0755, "", "mmx_missing_fixture_account", ""); err == nil {
		t.Fatal("a missing account name was accepted")
	}
}

func TestDirectoryOwnershipPreviewWaitsForAccount(t *testing.T) {
	t.Parallel()
	d := Directory{Path: filepath.Join(t.TempDir(), "app"), Owner: "mmx_missing_fixture_account"}
	r := (Runtime{Dryrun: true}).Directory(context.Background(), d, value.Scope{})
	if r.Status != "not_checked" {
		t.Fatal(r)
	}
	r = (Runtime{}).Directory(context.Background(), d, value.Scope{})
	if r.Status != "failed" {
		t.Fatal(r)
	}
}

// A missing parent directory is a missing prerequisite: a run fails without
// another try, naming it; a dryrun reports the node task not checked, and
// previews it as new when an earlier node task of the dryrun would create the
// parent.
func TestMissingParentDirectoryIsAMissingPrerequisite(t *testing.T) {
	t.Parallel()
	parent := filepath.Join(t.TempDir(), "backups")
	content := "x\n"
	for name, write := range map[string]func(Runtime, string) protocol.Result{
		"file": func(run Runtime, path string) protocol.Result {
			return run.File(context.Background(), File{Mode: "0644", Path: path, Content: &content}, value.Scope{})
		},
		"directory": func(run Runtime, path string) protocol.Result {
			return run.Directory(context.Background(), Directory{Path: path}, value.Scope{})
		},
		"directory with delete_everything_inside_except": func(run Runtime, path string) protocol.Result {
			return run.Directory(context.Background(), Directory{Path: path, DeleteEverythingInsideExcept: []string{}}, value.Scope{})
		},
		"symlink": func(run Runtime, path string) protocol.Result {
			return run.Symlink(context.Background(), Symlink{Path: path, Target: "/etc/hostname"})
		},
	} {
		path := filepath.Join(parent, "ledger")
		want := "the parent directory " + parent + " does not exist; create it with an earlier directory node task"
		if strings.HasPrefix(name, "directory") {
			want += "; or set create_missing_parent_directories: true on this directory node task"
		}
		if r := write(Runtime{}, path); r.Status != "failed" || r.FailureKind != "missing_prerequisite" || r.Reason != want {
			t.Errorf("%s in a run: %+v", name, r)
		}
		if r := write(Runtime{Dryrun: true}, path); r.Status != "not_checked" || r.Change != "unknown" || r.Reason != want {
			t.Errorf("%s in a dryrun: %+v", name, r)
		}
		if r := write(Runtime{Dryrun: true, PreviewPaths: []string{parent}}, path); r.Status != "succeeded" || r.Change != "would_change" {
			t.Errorf("%s in a dryrun after a node task that would create the parent: %+v", name, r)
		}
		if _, err := os.Stat(parent); !os.IsNotExist(err) {
			t.Fatalf("%s created the parent", name)
		}
	}
}

// create_missing_parent_directories creates the missing parents as mkdir -p
// does, each with the node program's account and permissions_mode 0755, and
// lists them; a dryrun lists them and creates nothing. It works the same for
// a directory that delete_everything_inside_except names.
func TestDirectoryCreatesMissingParentDirectories(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	parents := []string{filepath.Join(base, "srv"), filepath.Join(base, "srv", "app")}
	for name, d := range map[string]Directory{
		"directory": {Path: filepath.Join(parents[1], "data"), Mode: "0750", CreateMissingParentDirectories: true},
		"cleanup":   {Path: filepath.Join(parents[1], "releases"), Mode: "0750", DeleteEverythingInsideExcept: []string{"1.0"}},
		"copy":      copyTask(t, filepath.Join(parents[1], "conf.d"), map[string]string{"app.conf": "x\n"}),
	} {
		d.CreateMissingParentDirectories = true
		r := (Runtime{Dryrun: true}).Directory(context.Background(), d, value.Scope{})
		if r.Status != "succeeded" || r.Change != "would_change" || !slices.Equal(r.DirectoryChange.CreatedParentDirectories, parents) {
			t.Fatalf("%s in a dryrun: %+v %+v", name, r, r.DirectoryChange)
		}
		if _, err := os.Stat(parents[0]); !os.IsNotExist(err) {
			t.Fatalf("%s: the dryrun created %s", name, parents[0])
		}
		r = (Runtime{}).Directory(context.Background(), d, value.Scope{})
		if r.Status != "succeeded" || r.Change != "changed" || !slices.Equal(r.DirectoryChange.CreatedParentDirectories, parents) {
			t.Fatalf("%s in a run: %+v %+v", name, r, r.DirectoryChange)
		}
		for _, parent := range parents {
			info, err := os.Stat(parent)
			if err != nil || info.Mode().Perm() != 0755 || info.Sys().(*syscall.Stat_t).Uid != uint32(os.Geteuid()) {
				t.Fatalf("%s: %s: %v %v", name, parent, info, err)
			}
		}
		if info, err := os.Stat(d.Path); err != nil || !info.IsDir() {
			t.Fatalf("%s: %s: %v", name, d.Path, err)
		}
		r = (Runtime{}).Directory(context.Background(), d, value.Scope{})
		if r.Status != "succeeded" || r.Change != "unchanged" || (r.DirectoryChange != nil && r.DirectoryChange.CreatedParentDirectories != nil) {
			t.Fatalf("%s in a second run: %+v %+v", name, r, r.DirectoryChange)
		}
		if err := os.RemoveAll(parents[0]); err != nil {
			t.Fatal(err)
		}
	}
	// A file where a parent directory belongs fails and creates nothing.
	file := filepath.Join(base, "file")
	if err := os.WriteFile(file, nil, 0644); err != nil {
		t.Fatal(err)
	}
	d := Directory{Path: filepath.Join(file, "app", "data"), CreateMissingParentDirectories: true}
	if r := (Runtime{}).Directory(context.Background(), d, value.Scope{}); r.Status != "failed" || !strings.Contains(r.Reason, "not a directory") {
		t.Fatal(r)
	}
	// A directory that is deleted needs no parents.
	d = Directory{Path: "/srv/app", State: "absent", CreateMissingParentDirectories: true}
	if err := ValidateDirectory(d); err == nil || !strings.Contains(err.Error(), "directory with state: absent accepts only path, state, and also_delete_everything_inside") {
		t.Fatal(err)
	}
}
