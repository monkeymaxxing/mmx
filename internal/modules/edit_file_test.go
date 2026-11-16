package modules

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"mmx/internal/value"
)

func filter(script string) *ContentFilter {
	return &ContentFilter{Shell: &FilterShell{Script: script}}
}

// edit_file installs the filter's output through the file installer: a
// preview with a diff, an atomic replacement that keeps the file's metadata,
// and an unchanged rerun.
func TestEditFileInstallsTheFilteredText(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "fstab")
	original := "UUID=1 / ext4 defaults 0 1\n/swap.img none swap sw 0 0\n"
	if err := os.WriteFile(path, []byte(original), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0604); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	e := EditFile{Path: path, ContentFilter: &ContentFilter{Shell: &FilterShell{
		Script: `sed -E "s/^([^#].*[[:space:]]$KIND[[:space:]].*)$/#\1/"`,
		Env:    map[string]any{"KIND": "{{ .vars.kind }}"},
	}}}
	s := value.Scope{"vars": map[string]any{"kind": "swap"}}
	preview := (Runtime{Dryrun: true}).EditFile(context.Background(), e, s)
	if preview.Status != "succeeded" || preview.Change != "would_change" || preview.FileChange == nil ||
		!strings.Contains(preview.FileChange.Diff, "-/swap.img none swap sw 0 0\n+#/swap.img none swap sw 0 0\n") {
		t.Fatal(preview, preview.FileChange)
	}
	if read(t, path) != original {
		t.Fatal("dryrun changed the file")
	}
	for _, want := range []string{"changed", "unchanged"} {
		if r := (Runtime{}).EditFile(context.Background(), e, s); r.Status != "succeeded" || r.Change != want {
			t.Fatal(want, r)
		}
	}
	if got := read(t, path); got != "UUID=1 / ext4 defaults 0 1\n#/swap.img none swap sw 0 0\n" {
		t.Fatalf("%q", got)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	was, is := before.Sys().(*syscall.Stat_t), after.Sys().(*syscall.Stat_t)
	if after.Mode().Perm() != 0604 || was.Uid != is.Uid || was.Gid != is.Gid {
		t.Fatal("metadata not kept", after.Mode(), is.Uid, is.Gid)
	}
	if r := (Runtime{Dryrun: true}).EditFile(context.Background(), e, s); r.Change != "unchanged" {
		t.Fatal(r)
	}
}

// A failing filter, a filter that prints nothing for a file that is not
// empty, a filter that would change the file on every run, and a
// rejected new file each fail the task and leave the file as it was; another
// try after the cause is fixed installs the change.
func TestEditFileFailuresChangeNothing(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "app.ini")
	original := "[main]\nport=80\n"
	if err := os.WriteFile(path, []byte(original), 0644); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, script, want string
		check              *Check
	}{
		{name: "nonzero exit", script: "echo broken >&2; exit 3", want: "exit code 3: broken"},
		// A filter that edits the file in place, such as sed -i, prints
		// nothing: installing its output would empty the file.
		{name: "prints nothing", script: "cat >/dev/null", want: "edit_file.content_filter printed nothing"},
		{name: "changes its own output", script: `sed 's/^port=.*/&0/'`, want: "edit_file.content_filter would change the file on every run"},
		{name: "rejected new file", script: `sed 's/^port=80$/port=8080/'`, check: &Check{Shell: &Shell{Script: "exit 1"}}, want: "check_new_file failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := EditFile{Path: path, ContentFilter: filter(tc.script), CheckNewFile: tc.check}
			for _, dryrun := range []bool{true, false} {
				r := (Runtime{Dryrun: dryrun}).EditFile(context.Background(), e, value.Scope{})
				if tc.check != nil && dryrun {
					// A check without its own dryrun command is not run in dryrun.
					if r.Change != "would_change" || r.CheckNewFile == nil || r.CheckNewFile.Status != "skipped" {
						t.Fatal(r)
					}
					continue
				}
				if r.Status != "failed" || !strings.Contains(r.Reason, tc.want) {
					t.Fatal(dryrun, r)
				}
			}
			if read(t, path) != original {
				t.Fatal("changed the file")
			}
		})
	}
	e := EditFile{Path: path, ContentFilter: filter(`sed 's/^port=80$/port=8080/'`), CheckNewFile: &Check{Shell: &Shell{Script: "exit 0"}}}
	if r := (Runtime{}).EditFile(context.Background(), e, value.Scope{}); r.Status != "succeeded" || r.Change != "changed" {
		t.Fatal(r)
	}
	if got := read(t, path); got != "[main]\nport=8080\n" {
		t.Fatalf("%q", got)
	}
}

// edit_file changes only an existing regular file; dryrun leaves a missing one
// to an earlier task, such as the package that installs it.
func TestEditFileNeedsAnExistingRegularFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "directory"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "target"), []byte("a=1\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("target", filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"missing", "missing-dir/file", "directory", "link"} {
		e := EditFile{Path: filepath.Join(dir, name), ContentFilter: filter("sed s/1/2/")}
		if r := (Runtime{}).EditFile(context.Background(), e, value.Scope{}); r.Status != "failed" {
			t.Fatal(name, r)
		}
		r := (Runtime{Dryrun: true}).EditFile(context.Background(), e, value.Scope{})
		if strings.HasPrefix(name, "missing") != (r.Status == "not_checked") {
			t.Fatal(name, r)
		}
	}
	if read(t, filepath.Join(dir, "target")) != "a=1\n" {
		t.Fatal("followed the link")
	}
}
