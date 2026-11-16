package modules

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mmx/internal/assets"
	"mmx/internal/protocol"
	"mmx/internal/value"
)

func TestFileSourceCopiesExactBytesAndValidates(t *testing.T) {
	t.Parallel()
	for _, data := range [][]byte{nil, []byte("{{ .not.a.reference }}\n"), {0, 255, 254, 128, 10}} {
		path := filepath.Join(t.TempDir(), "file")
		f := File{Path: path, Source: "frozen", Asset: fileAsset(t, data), Mode: "0644"}
		r := (Runtime{Dryrun: true}).File(context.Background(), f, value.Scope{})
		if r.Status != "succeeded" || r.Change != "would_change" || r.FileChange.After.Size != int64(len(data)) {
			t.Fatal(r)
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatal("dryrun wrote", err)
		}
		for _, want := range []string{"changed", "unchanged"} {
			r = (Runtime{}).File(context.Background(), f, value.Scope{})
			actual, err := os.ReadFile(path)
			if r.Status != "succeeded" || r.Change != want || err != nil || !bytes.Equal(data, actual) {
				t.Fatal(r, actual, err)
			}
		}
		f.Asset = fileAsset(t, []byte("new"))
		f.CheckNewFile = &Check{Shell: &Shell{Script: "exit 1"}}
		r = (Runtime{}).File(context.Background(), f, value.Scope{})
		actual, err := os.ReadFile(path)
		if r.FailureKind != "validation" || err != nil || !bytes.Equal(data, actual) {
			t.Fatal(r, actual, err)
		}
	}
}

func TestFileRemovalAndPathTypes(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "file")
	if err := os.WriteFile(path, []byte("deleted-private-content"), 0644); err != nil {
		t.Fatal(err)
	}
	f := File{Path: path, State: "absent"}
	r := (Runtime{Dryrun: true}).File(context.Background(), f, value.Scope{})
	if r.Change != "would_change" || r.FileChange.Diff != "" {
		t.Fatal(r)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"changed", "unchanged"} {
		r = (Runtime{}).File(context.Background(), f, value.Scope{})
		if r.Status != "succeeded" || r.Change != want {
			t.Fatal(r)
		}
	}
	if err := os.Symlink("missing", path); err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{path, dir} { // paths arrive cleaned (config.ResolveArguments)
		f.Path = target
		r = (Runtime{}).File(context.Background(), f, value.Scope{})
		if r.Status != "failed" {
			t.Fatal("removed non-file", r)
		}
	}
	if _, err := os.Readlink(path); err != nil {
		t.Fatal("link removed", err)
	}
}

func TestSymlinkLifecycleLeavesTargetsAlone(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "link")
	target := filepath.Join(dir, "data")
	if err := os.WriteFile(target, []byte("unchanged"), 0600); err != nil {
		t.Fatal(err)
	}
	link := Symlink{Path: path, Target: "data"}
	for _, desired := range []string{"data", "missing", target} {
		link.Target = desired
		before, _ := os.Readlink(path)
		r := (Runtime{Dryrun: true}).Symlink(context.Background(), link)
		after, _ := os.Readlink(path)
		if r.Change != "would_change" || before != after {
			t.Fatal(r)
		}
		for _, want := range []string{"changed", "unchanged"} {
			r = (Runtime{}).Symlink(context.Background(), link)
			after, err := os.Readlink(path)
			if r.Status != "succeeded" || r.Change != want || err != nil || after != desired {
				t.Fatal(r, err)
			}
		}
	}
	link.State = "absent"
	link.Target = ""
	if r := (Runtime{Dryrun: true}).Symlink(context.Background(), link); r.Change != "would_change" {
		t.Fatal(r)
	}
	for _, want := range []string{"changed", "unchanged"} {
		r := (Runtime{}).Symlink(context.Background(), link)
		if r.Change != want {
			t.Fatal(r)
		}
	}
	for _, bad := range []string{target, dir, target + "/"} {
		link.Path = bad
		if r := (Runtime{}).Symlink(context.Background(), link); r.Status != "failed" {
			t.Fatal("removed non-link", r)
		}
	}
	actual, err := os.ReadFile(target)
	if err != nil || string(actual) != "unchanged" {
		t.Fatal("modified target", err)
	}
}

func TestLinkAndRemovalDetectReplacedDestination(t *testing.T) {
	t.Parallel()
	for _, remove := range []bool{false, true} {
		dir := t.TempDir()
		path := filepath.Join(dir, "path")
		if remove {
			os.WriteFile(path, []byte("old"), 0600)
		} else {
			os.Symlink("old", path)
		}
		run := Runtime{BeforeMutation: func(string) error {
			if err := os.Rename(path, path+".before"); err != nil {
				return err
			}
			return os.WriteFile(path, []byte("replacement"), 0600)
		}}
		var r protocol.Result
		if remove {
			r = run.File(context.Background(), File{Path: path, State: "absent"}, value.Scope{})
		} else {
			r = run.Symlink(context.Background(), Symlink{Path: path, Target: "new"})
		}
		actual, err := os.ReadFile(path)
		if r.Status != "failed" || err != nil || string(actual) != "replacement" {
			t.Fatal(r, err)
		}
	}
}

func TestFileTextPreview(t *testing.T) {
	t.Parallel()
	cases := []struct{ old, next, contains, omitted string }{
		{"old\n", "new\n", "-old\n+new\n", ""},
		{"", "new\n", "@@ -0,0 +1,1 @@", ""},
		{"old", "old\n", "\\ No newline at end of file", ""},
		{"same\nold\nend\n", "same\nnew\nend\n", " same\n-old\n+new\n end\n", ""},
		{"\x00", "new", "", "binary"},
		{"", "\x1b[31m", "", "binary"},
		{"", strings.Repeat("x", 65537), "", "exceeds"},
		{"", strings.Repeat("line\n", 300), "+line", "truncated"},
	}
	for _, c := range cases {
		change := &protocol.FileChange{After: &protocol.FileState{Mode: "0644"}}
		(Runtime{}).textDiff(change, []byte(c.old), []byte(c.next))
		if !strings.Contains(change.Diff, c.contains) || !strings.Contains(change.DiffOmitted, c.omitted) || len(change.Diff) > 16*1024 || strings.Count(change.Diff, "\n") > 200 {
			t.Fatalf("%+v", change)
		}
		if c.omitted == "" && change.DiffOmitted != "" {
			t.Fatal(change)
		}
	}
	for _, modes := range [][2]string{{"0600", "0644"}, {"0644", "0640"}} {
		change := &protocol.FileChange{Before: &protocol.FileState{Mode: modes[0]}, After: &protocol.FileState{Mode: modes[1]}}
		(Runtime{}).textDiff(change, []byte("old-secret"), []byte("new-secret"))
		if change.Diff != "" || !strings.Contains(change.DiffOmitted, "private") {
			t.Fatal(change)
		}
	}
}

func fileAsset(t *testing.T, data []byte) *assets.Ref {
	t.Helper()
	source := filepath.Join(t.TempDir(), "source")
	if err := os.WriteFile(source, data, 0600); err != nil {
		t.Fatal(err)
	}
	ref, err := assets.Snapshot(context.Background(), t.TempDir(), source)
	if err != nil {
		t.Fatal(err)
	}
	return &ref
}

// A link is installed and removed within the parent directory it inspected,
// like a file: a directory moved into its place meanwhile is left alone.
func TestSymlinkStaysInItsPinnedParent(t *testing.T) {
	t.Parallel()
	for _, remove := range []bool{false, true} {
		base := t.TempDir()
		dir := filepath.Join(base, "etc")
		if err := os.Mkdir(dir, 0700); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, "link")
		if err := os.Symlink("old", path); err != nil {
			t.Fatal(err)
		}
		run := Runtime{BeforeMutation: func(string) error {
			if err := os.Rename(dir, dir+".inspected"); err != nil {
				return err
			}
			if err := os.Mkdir(dir, 0700); err != nil {
				return err
			}
			return os.Symlink("replacement", path)
		}}
		link := Symlink{Path: path, Target: "new"}
		if remove {
			link = Symlink{Path: path, State: "absent"}
		}
		r := run.Symlink(context.Background(), link)
		if r.Status != "succeeded" || r.Change != "changed" {
			t.Fatal(remove, r)
		}
		if target, err := os.Readlink(path); err != nil || target != "replacement" {
			t.Fatal("changed the directory moved into place", remove, target, err)
		}
		target, err := os.Readlink(filepath.Join(dir+".inspected", "link"))
		if remove && !os.IsNotExist(err) || !remove && (err != nil || target != "new") {
			t.Fatal("inspected directory did not receive the change", remove, target, err)
		}
		if entries, _ := os.ReadDir(dir + ".inspected"); len(entries) != 1 && !remove {
			t.Fatal("candidate left behind", entries)
		}
	}
}
