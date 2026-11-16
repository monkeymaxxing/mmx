package modules

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// oldFilesTree writes files below root, each with its age, and returns the
// bytes of each.
func oldFilesTree(t *testing.T, root string, files map[string]time.Duration) {
	t.Helper()
	now := time.Now()
	for name, age := range files {
		path := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(name), 0644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(path, now.Add(-age), now.Add(-age)); err != nil {
			t.Fatal(err)
		}
	}
}

// realTempDir is a new temporary directory named by a path without a link,
// which delete_old_files requires: on macOS the temporary directories are
// below /var, a link to /private/var.
func realTempDir(t *testing.T) string {
	t.Helper()
	path, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func oldFilesValue(t *testing.T, v any) (files int, bytes int64, examples []string) {
	t.Helper()
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("value %#v", v)
	}
	return m["deleted_files"].(int), m["deleted_bytes"].(int64), m["deleted_file_examples"].([]string)
}

func remainingFiles(t *testing.T, root string) []string {
	t.Helper()
	var found []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			relative, _ := filepath.Rel(root, path)
			found = append(found, relative)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(found)
	return found
}

// A dryrun lists the old files whose names match and deletes nothing; a run
// deletes exactly those, also in the directories below with
// include_subdirectories; then the task is unchanged. A newer file, a file
// of another name, and the directories stay.
func TestDeleteOldFiles(t *testing.T) {
	t.Parallel()
	day := 24 * time.Hour
	for _, include := range []bool{true, false} {
		root := filepath.Join(realTempDir(t), "var/log/shop")
		oldFilesTree(t, root, map[string]time.Duration{
			"shop.log":         20 * day, // another name
			"shop.log.1":       20 * day,
			"shop.log.2.gz":    9 * day,
			"shop.log.3.gz":    3 * day, // too new
			".hidden.gz":       20 * day,
			"shop.log.x":       20 * day, // [0-9] does not match x
			"workers/a.log.gz": 30 * day,
			"workers/a.log":    30 * day,
		})
		d := DeleteOldFiles{Directory: root, NamePatterns: []string{"*.gz", "*.[0-9]"}, OlderThan: "7d", IncludeSubdirectories: &include}
		want := []string{".hidden.gz", "shop.log.1", "shop.log.2.gz"}
		if include {
			want = append(want, "workers/a.log.gz")
		}
		var bytes int64
		var examples []string
		for _, name := range want {
			bytes += int64(len(name))
			examples = append(examples, filepath.Join(root, name))
		}
		before := remainingFiles(t, root)
		r := Runtime{Dryrun: true}.DeleteOldFiles(testContext(t), d)
		if files, b, e := oldFilesValue(t, r.Value); r.Status != "succeeded" || r.Change != "would_change" || files != len(want) || b != bytes || !slices.Equal(e, examples) {
			t.Fatalf("include %t: %+v; want %v", include, r, examples)
		}
		if got := remainingFiles(t, root); !slices.Equal(got, before) {
			t.Fatalf("a dryrun deleted files: %v", got)
		}
		r = Runtime{}.DeleteOldFiles(testContext(t), d)
		if files, b, e := oldFilesValue(t, r.Value); r.Status != "succeeded" || r.Change != "changed" || files != len(want) || b != bytes || !slices.Equal(e, examples) {
			t.Fatalf("include %t: %+v", include, r)
		}
		if !strings.HasPrefix(r.Reason, fmt.Sprintf("deleted %d files totaling ", len(want))) {
			t.Fatalf("%q", r.Reason)
		}
		left := slices.DeleteFunc(before, func(name string) bool { return slices.Contains(want, name) })
		if got := remainingFiles(t, root); !slices.Equal(got, left) {
			t.Fatalf("include %t: left %v, want %v", include, got, left)
		}
		for _, dryrun := range []bool{true, false} {
			r = Runtime{Dryrun: dryrun}.DeleteOldFiles(testContext(t), d)
			if files, _, _ := oldFilesValue(t, r.Value); r.Change != "unchanged" || files != 0 || r.Reason != "no file in "+root+" matches name_patterns and is older than 7d" {
				t.Fatalf("dryrun %t: %+v", dryrun, r)
			}
		}
	}
}

// delete_old_files follows no link below the directory: a link whose name
// matches stays, and so does the old file it points to; a directory reached
// through a link is not walked. A missing directory is unchanged.
func TestDeleteOldFilesFollowsNoLink(t *testing.T) {
	t.Parallel()
	base := realTempDir(t)
	elsewhere := filepath.Join(base, "elsewhere")
	oldFilesTree(t, elsewhere, map[string]time.Duration{"target.gz": 100 * 24 * time.Hour, "deep/inner.gz": 100 * 24 * time.Hour})
	root := filepath.Join(base, "var/log/shop")
	oldFilesTree(t, root, map[string]time.Duration{"old.gz": 100 * 24 * time.Hour})
	for link, target := range map[string]string{"link.gz": "../../../elsewhere/target.gz", "linked-directory": "../../../elsewhere/deep"} {
		if err := os.Symlink(target, filepath.Join(root, link)); err != nil {
			t.Fatal(err)
		}
	}
	include := true
	d := DeleteOldFiles{Directory: root, NamePatterns: []string{"*.gz"}, OlderThan: "1d", IncludeSubdirectories: &include}
	r := Runtime{}.DeleteOldFiles(testContext(t), d)
	if files, _, _ := oldFilesValue(t, r.Value); r.Status != "succeeded" || files != 1 || !strings.HasSuffix(r.Reason, "; left alone 1 symlinks whose names match and 0 mount points, which delete_old_files never follows") {
		t.Fatalf("%+v", r)
	}
	if got := remainingFiles(t, elsewhere); !slices.Equal(got, []string{"deep/inner.gz", "target.gz"}) {
		t.Fatalf("deleted through a link: %v", got)
	}
	for _, name := range []string{"link.gz", "linked-directory"} {
		if _, err := os.Lstat(filepath.Join(root, name)); err != nil {
			t.Fatalf("the link %s is gone: %v", name, err)
		}
	}
	d.Directory = filepath.Join(base, "var/log/gone")
	if r := (Runtime{}).DeleteOldFiles(testContext(t), d); r.Status != "succeeded" || r.Change != "unchanged" || r.Reason != "the directory "+d.Directory+" does not exist" {
		t.Fatalf("%+v", r)
	}
	d.Directory = filepath.Join(base, "var/gone/shop")
	if r := (Runtime{}).DeleteOldFiles(testContext(t), d); r.Change != "unchanged" || r.Reason != "the directory "+d.Directory+" does not exist: "+filepath.Join(base, "var/gone")+" does not exist" {
		t.Fatalf("%+v", r)
	}
}

// A directory whose path has a symlink in any component fails the task and
// names that component, the last one or one in the middle, so that the task
// deletes only below the directory as written: neither the old files where
// the link points nor anything else changes.
func TestDeleteOldFilesRefusesALinkAnywhereOnTheDirectorysPath(t *testing.T) {
	t.Parallel()
	base := realTempDir(t)
	day := 24 * time.Hour
	database := filepath.Join(base, "var/lib/postgresql")
	oldFilesTree(t, database, map[string]time.Duration{"16/main/base.gz": 100 * day, "old.gz": 100 * day})
	if err := os.MkdirAll(filepath.Join(base, "var/log"), 0755); err != nil {
		t.Fatal(err)
	}
	// The last component is a link: /var/log/postgresql -> /var/lib/postgresql.
	if err := os.Symlink(database, filepath.Join(base, "var/log/postgresql")); err != nil {
		t.Fatal(err)
	}
	// A middle component is a link: /srv/logs -> /var/lib, so
	// /srv/logs/postgresql is the database's directory too.
	if err := os.MkdirAll(filepath.Join(base, "srv"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(base, "var/lib"), filepath.Join(base, "srv/logs")); err != nil {
		t.Fatal(err)
	}
	include := true
	for directory, link := range map[string]string{
		filepath.Join(base, "var/log/postgresql"):  filepath.Join(base, "var/log/postgresql"),
		filepath.Join(base, "srv/logs/postgresql"): filepath.Join(base, "srv/logs"),
	} {
		d := DeleteOldFiles{Directory: directory, NamePatterns: []string{"*.gz"}, OlderThan: "1d", IncludeSubdirectories: &include}
		for _, run := range []Runtime{noMutation(t), {}} {
			r := run.DeleteOldFiles(testContext(t), d)
			want := link + " is a symlink; delete_old_files deletes only below the directory as written and follows no link on its path"
			if r.Status != "failed" || r.Change != "unchanged" || r.FailureKind != "config" || !strings.HasPrefix(r.Reason, want) {
				t.Fatalf("%s, dryrun %t: %+v; want %q", directory, run.Dryrun, r, want)
			}
		}
	}
	if got := remainingFiles(t, database); !slices.Equal(got, []string{"16/main/base.gz", "old.gz"}) {
		t.Fatalf("deleted through a link on the path: %v", got)
	}
}

// A file that cannot be deleted fails the task, which still reports the
// files deleted before it.
func TestDeleteOldFilesReportsWhatItDeletedBeforeAFailure(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("root deletes from a directory it cannot write")
	}
	root := filepath.Join(realTempDir(t), "var/log/shop")
	oldFilesTree(t, root, map[string]time.Duration{"a.gz": 48 * time.Hour, "b/c.gz": 48 * time.Hour})
	if err := os.Chmod(filepath.Join(root, "b"), 0500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(filepath.Join(root, "b"), 0755) })
	include := true
	r := Runtime{}.DeleteOldFiles(testContext(t), DeleteOldFiles{Directory: root, NamePatterns: []string{"*.gz"}, OlderThan: "1d", IncludeSubdirectories: &include})
	if r.Status != "failed" || r.Change != "changed" || !strings.Contains(r.Reason, "delete "+filepath.Join(root, "b/c.gz")) {
		t.Fatalf("%+v", r)
	}
	if files, _, _ := oldFilesValue(t, r.Value); files != 1 {
		t.Fatalf("%+v", r)
	}
}

func TestDeleteOldFilesRejectsInputs(t *testing.T) {
	t.Parallel()
	yes := true
	valid := DeleteOldFiles{Directory: "/var/log/shop", NamePatterns: []string{"*.gz"}, OlderThan: "7d", IncludeSubdirectories: &yes}
	if err := valid.Prepare(""); err != nil {
		t.Fatal(err)
	}
	for message, change := range map[string]func(*DeleteOldFiles){
		"delete_old_files.directory is required":                              func(d *DeleteOldFiles) { d.Directory = "" },
		"delete_old_files.name_patterns is required":                          func(d *DeleteOldFiles) { d.NamePatterns = nil },
		"delete_old_files.older_than is required":                             func(d *DeleteOldFiles) { d.OlderThan = "" },
		"delete_old_files.include_subdirectories is required":                 func(d *DeleteOldFiles) { d.IncludeSubdirectories = nil },
		"older_than: must be a duration above zero":                           func(d *DeleteOldFiles) { d.OlderThan = "a week" },
		"name_patterns[1]: a file name pattern must be a nonempty shell glob": func(d *DeleteOldFiles) { d.NamePatterns = []string{"*.gz", "old/*.gz"} },
		"name_patterns[0]: \"[a\" is not a glob":                              func(d *DeleteOldFiles) { d.NamePatterns = []string{"[a"} },
	} {
		d := valid
		change(&d)
		if err := d.Prepare(""); err == nil || !strings.Contains(err.Error(), message) {
			t.Errorf("%s: %v", message, err)
		}
	}
	// The directory's rule is the one of directory's deletions.
	for path, ok := range map[string]bool{"/var/log": true, "/var": false, "/": false, "var/log/shop": false} {
		if err := deletablePath(path); (err == nil) != ok {
			t.Errorf("%s: %v", path, err)
		}
	}
	if !(&oldFilesWalk{patterns: []string{goGlob("*.[!0-9]")}}).matches("app.log.x") || (&oldFilesWalk{patterns: []string{goGlob("*.[!0-9]")}}).matches("app.log.1") {
		t.Error("[!0-9] does not match as a shell glob")
	}
}
