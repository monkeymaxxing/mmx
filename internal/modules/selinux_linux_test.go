//go:build linux

package modules

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// A directory of the staged tree that becomes a symlink while labels are
// being set must not redirect a label to the link's target.
func TestSetLabelBeneathRefusesSymlinkedDirectory(t *testing.T) {
	base := t.TempDir()
	outside := filepath.Join(base, "outside")
	tree := filepath.Join(base, "tree")
	for _, dir := range []string{outside, filepath.Join(tree, "a", "b")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(outside, "b-file"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	rootFd, err := unix.Open(tree, unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(rootFd)

	var labeled []string
	record := func(path, label string) error {
		resolved, err := filepath.EvalSymlinks(path)
		if err != nil {
			return err
		}
		labeled = append(labeled, resolved)
		return nil
	}
	if err := setLabelBeneath(rootFd, "a/b", "x", record); err != nil {
		t.Fatalf("labeling a real directory: %v", err)
	}
	if err := setLabelBeneath(rootFd, ".", "x", record); err != nil {
		t.Fatalf("labeling the tree itself: %v", err)
	}
	for _, path := range labeled {
		if !strings.HasPrefix(path, tree) {
			t.Fatalf("label went to %s, outside the tree", path)
		}
	}

	// The account swaps a for a link to a directory outside the tree.
	if err := os.RemoveAll(filepath.Join(tree, "a")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(tree, "a")); err != nil {
		t.Fatal(err)
	}
	called := false
	err = setLabelBeneath(rootFd, "a/b-file", "x", func(string, string) error { called = true; return nil })
	if err == nil || called {
		t.Fatalf("a path through a symlinked directory was labeled (err %v, labeled %v)", err, called)
	}
}
