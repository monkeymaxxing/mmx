package modules

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// withPrivateTaskDirectoryRoot is run with directory as the place where a
// node task's private directory goes, as the node task execution engine
// hands a module its Runtime.
func withPrivateTaskDirectoryRoot(run Runtime, directory string) Runtime {
	run.PrivateTaskDirectoryRoot = func() (string, error) { return directory, nil }
	return run
}

// owner is the user and group that own path itself, not what a link at path
// points to.
func owner(t *testing.T, path string) (uid, gid int) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	stat := info.Sys().(*syscall.Stat_t)
	return int(stat.Uid), int(stat.Gid)
}

// A node task's private directory is made in the place that the engine
// hands the module, belongs to the account its tool runs as with mode 0700,
// and is gone after its remove function.
func TestPrivateTaskDirectoryBelongsToTheToolsAccountAlone(t *testing.T) {
	t.Parallel()
	privateTaskDirectoryRoot := t.TempDir()
	run := withPrivateTaskDirectoryRoot(Runtime{}, privateTaskDirectoryRoot)
	// As root, the account is another one, as for a git task with run_as_user_account.
	// Without root, a process can give files only to itself.
	account := &syscall.Credential{Uid: uint32(os.Geteuid()), Gid: uint32(os.Getegid())}
	if os.Geteuid() == 0 {
		account = &syscall.Credential{Uid: 12345, Gid: 23456}
	}
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("kept"), 0600); err != nil {
		t.Fatal(err)
	}
	outsideUID, outsideGID := owner(t, outside)

	directory, remove, err := run.makePrivateTaskDirectory("git", account, func(directory string) error {
		// While the task's files are written, the directory is still the
		// runner's own and closed to every other account.
		if uid, _ := owner(t, directory); uid != os.Geteuid() {
			t.Errorf("the directory belonged to user %d while its files were written", uid)
		}
		if info, err := os.Lstat(directory); err != nil || info.Mode().Perm() != 0700 {
			t.Error("the directory was open while its files were written", info, err)
		}
		if err := os.WriteFile(filepath.Join(directory, "password"), []byte("s3cret"), 0600); err != nil {
			return err
		}
		if err := os.Mkdir(filepath.Join(directory, "nested"), 0700); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(directory, "nested", "key"), []byte("key"), 0600); err != nil {
			return err
		}
		return os.Symlink(outside, filepath.Join(directory, "link"))
	})
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(directory) != privateTaskDirectoryRoot || !strings.HasPrefix(filepath.Base(directory), "git-") {
		t.Fatal("the directory is not in the handed place under its module's name", directory)
	}
	if info, err := os.Lstat(directory); err != nil || !info.IsDir() || info.Mode().Perm() != 0700 {
		t.Fatal("the directory is open to other accounts", info, err)
	}
	for _, name := range []string{"", "password", "nested", "nested/key", "link"} {
		if uid, gid := owner(t, filepath.Join(directory, name)); uid != int(account.Uid) || gid != int(account.Gid) {
			t.Fatalf("%q belongs to %d:%d, not to the tool's account %d:%d", name, uid, gid, account.Uid, account.Gid)
		}
	}
	// A link is given to the account itself; what it points to is not.
	if uid, gid := owner(t, outside); uid != outsideUID || gid != outsideGID {
		t.Fatal("the owner of a file outside the directory changed", uid, gid)
	}

	// The account can put anything into its directory before the task
	// ends. Removing the directory follows no link out of it.
	kept := t.TempDir()
	if err := os.WriteFile(filepath.Join(kept, "file"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(kept, filepath.Join(directory, "planted")); err != nil {
		t.Fatal(err)
	}
	remove()
	if _, err := os.Lstat(directory); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("the directory outlived its task", err)
	}
	if left := names(t, privateTaskDirectoryRoot); len(left) != 1 {
		t.Fatal("the place of the private task directories is not empty", left)
	}
	for _, path := range []string{outside, filepath.Join(kept, "file")} {
		if _, err := os.Stat(path); err != nil {
			t.Fatal("removing the directory followed a link", err)
		}
	}
}

// Root's own task, such as a docker task, keeps the directory for root. A
// task whose files cannot be written leaves no directory, and a Runtime that
// was not told where private task directories go makes none anywhere else.
func TestPrivateTaskDirectoryOfRootAndItsFailures(t *testing.T) {
	t.Parallel()
	privateTaskDirectoryRoot := t.TempDir()
	run := withPrivateTaskDirectoryRoot(Runtime{}, privateTaskDirectoryRoot)
	directory, remove, err := run.makePrivateTaskDirectory("docker_compose_project", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if uid, _ := owner(t, directory); uid != os.Geteuid() {
		t.Fatal("the directory does not belong to the runner's account", uid)
	}
	if info, err := os.Lstat(directory); err != nil || info.Mode().Perm() != 0700 || !strings.HasPrefix(filepath.Base(directory), "docker_compose_project-") {
		t.Fatal(directory, info, err)
	}
	remove()

	failed := errors.New("the disk is full")
	if _, _, err := run.makePrivateTaskDirectory("git", nil, func(string) error { return failed }); !errors.Is(err, failed) {
		t.Fatal(err)
	}
	if left := names(t, privateTaskDirectoryRoot); len(left) != 1 {
		t.Fatal("a task that failed to write its files left a directory", left)
	}

	if _, _, err := (Runtime{}).makePrivateTaskDirectory("git", nil, nil); err == nil || !strings.Contains(err.Error(), "internal error") {
		t.Fatal("a Runtime that was not told where private task directories go made one", err)
	}
	unavailable := errors.New("/run is not a memory filesystem")
	refusing := Runtime{PrivateTaskDirectoryRoot: func() (string, error) { return "", unavailable }}
	if _, _, err := refusing.makePrivateTaskDirectory("git", nil, nil); !errors.Is(err, unavailable) {
		t.Fatal(err)
	}
}
