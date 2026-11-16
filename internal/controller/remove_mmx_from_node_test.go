package controller

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"mmx/internal/access"
	"mmx/internal/config"
)

// removeFixture is a node reached through a fake ssh whose answers to the
// three scripts of mmx remove-mmx-from-node come from files beside it: what
// the node says about running node jobs and owed restores, what the removal
// script printed, and what the authorized_keys script printed.
type removeFixture struct {
	c            Controller
	dir, trace   string
	identityFile string
	keyFile      string
}

func newRemoveFixture(t *testing.T) *removeFixture {
	t.Helper()
	trace := filepath.Join(t.TempDir(), "commands")
	dir := fakeSSH(t, `case "$1" in -G) printf 'hostname fixture\nuser root\nport 22\n'; exit 0;; -F) exit 0;; esac
for command; do :; done
case "$command" in
*machine-id*) printf 'Linux\nx86_64\n0\nroot\n0b0b0b0b-0000-4000-8000-000000000000\nsystemd\n0123456789abcdef0123456789abcdef\n\n10.0.0.1 5000 10.0.0.2 22\n';;
*flock*) echo flock >> `+quote(trace)+`; cat "$(dirname "$0")/idle-answer";;
*"awk -v key"*) echo authorized_keys >> `+quote(trace)+`; printf '%s\n' "$command" > "$(dirname "$0")/authorized-keys-command"; cat "$(dirname "$0")/key-answer";;
*rmdir*) echo removal >> `+quote(trace)+`; cat "$(dirname "$0")/removal-answer";;
*) echo "unexpected command: $command" >&2; exit 1;;
esac
`)
	f := &removeFixture{c: Controller{State: t.TempDir()}, dir: dir, trace: trace}
	for _, name := range []string{"idle-answer", "removal-answer", "key-answer"} {
		if err := os.WriteFile(filepath.Join(dir, name), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	destination := nodeDestination(sshSettings{Hostname: "fixture", Port: "22", User: "root"})
	f.identityFile = filepath.Join(f.c.State, "nodes", destination+".json")
	keyPath, err := access.KeyPath(destination)
	if err != nil {
		t.Fatal(err)
	}
	f.keyFile = keyPath
	if err := os.MkdirAll(filepath.Dir(f.identityFile), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.identityFile, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := access.EnsureKey(keyPath); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *removeFixture) answer(t *testing.T, script, text string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(f.dir, script), []byte(text), 0600); err != nil {
		t.Fatal(err)
	}
}

func (f *removeFixture) run() ([]string, error) {
	return f.c.RemoveMMXFromNode(context.Background(), config.Project{}, "web03", config.Node{Address: "fixture"})
}

func (f *removeFixture) commands(t *testing.T) string {
	t.Helper()
	data, _ := os.ReadFile(f.trace)
	return string(data)
}

func (f *removeFixture) controllerFilesExist() bool {
	for _, file := range []string{f.identityFile, f.keyFile, f.keyFile + ".pub", f.keyFile + ".lock"} {
		if _, err := os.Stat(file); err != nil {
			return false
		}
	}
	return true
}

// While a node job runs, MMX stays: the error says which node job and how to
// finish it first, and nothing is removed on the node or the controller machine.
func TestRemoveMMXFromNodeRefusesWhileANodeJobRuns(t *testing.T) {
	f := newRemoveFixture(t)
	f.answer(t, "idle-answer", "running_job 2f5c1e\n")
	removed, err := f.run()
	if !errors.Is(err, ErrNodeNotIdle) || len(removed) != 0 {
		t.Fatal(removed, err)
	}
	for _, want := range []string{"node job 2f5c1e is running", "mmx status", "mmx stop", "mmx resume", "mmx remove-mmx-from-node web03 again"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the error lacks %q: %v", want, err)
		}
	}
	if got := f.commands(t); got != "flock\n" || !f.controllerFilesExist() {
		t.Fatalf("something was removed: commands %q", got)
	}
}

// An owed restore keeps MMX on the node; the error names the restore's unit
// and the command that starts it again.
func TestRemoveMMXFromNodeRefusesWhileAnOwedRestoreWaits(t *testing.T) {
	f := newRemoveFixture(t)
	unit := "mmx-owed-restore-0123456789abcdef.service"
	f.answer(t, "idle-answer", "owed_restore "+unit+"\n")
	removed, err := f.run()
	if !errors.Is(err, ErrNodeNotIdle) || len(removed) != 0 {
		t.Fatal(removed, err)
	}
	for _, want := range []string{"an owed restore waits as " + unit, "systemctl start " + unit, "mmx status"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("the error lacks %q: %v", want, err)
		}
	}
	if got := f.commands(t); got != "flock\n" || !f.controllerFilesExist() {
		t.Fatalf("something was removed: commands %q", got)
	}
}

// The removal reports each line the node printed, then the managed key's
// line, then the controller machine's files; a second run finds nothing.
func TestRemoveMMXFromNodeRemovesEverythingAndRunsAgain(t *testing.T) {
	f := newRemoveFixture(t)
	f.answer(t, "removal-answer", "stopped and disabled mmx-https-api.service\nremoved /opt/mmx\nkept /etc/mmx: it still holds systemd_scheduled_jobs because MMX removes neither the secrets of a scheduled job nor do_not_manage\n")
	f.answer(t, "key-answer", "removed 1 from /root/.ssh/authorized_keys\n")
	managed, _ := os.ReadFile(f.keyFile + ".pub")
	removed, err := f.run()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"web03: stopped and disabled mmx-https-api.service",
		"web03: removed /opt/mmx",
		"web03: kept /etc/mmx: it still holds systemd_scheduled_jobs because MMX removes neither the secrets of a scheduled job nor do_not_manage",
		"web03: removed 1 from /root/.ssh/authorized_keys",
		"controller machine: removed " + f.identityFile,
		"controller machine: removed " + f.keyFile,
		"controller machine: removed " + f.keyFile + ".pub",
		"controller machine: removed " + f.keyFile + ".lock",
	}
	if strings.Join(removed, "\n") != strings.Join(want, "\n") {
		t.Fatalf("got\n%s\nwant\n%s", strings.Join(removed, "\n"), strings.Join(want, "\n"))
	}
	if f.controllerFilesExist() {
		t.Fatal("the node's identity or the managed key's files are still there")
	}
	sent, _ := os.ReadFile(filepath.Join(f.dir, "authorized-keys-command"))
	if fields := strings.Fields(string(managed)); len(fields) < 2 || !strings.Contains(string(sent), fields[1]) {
		t.Fatalf("the authorized_keys command does not name the managed key: %s", sent)
	}
	if got := f.commands(t); got != "flock\nremoval\nauthorized_keys\n" {
		t.Fatalf("commands %q", got)
	}

	// The managed key is gone, so a second run sends no authorized_keys
	// command, and the nothing it finds is no error.
	f.answer(t, "removal-answer", "")
	removed, err = f.run()
	if err != nil || len(removed) != 0 {
		t.Fatal(removed, err)
	}
	if got := f.commands(t); got != "flock\nremoval\nauthorized_keys\nflock\nremoval\n" {
		t.Fatalf("commands %q", got)
	}
}

// A key that MMX copied to another SSH port's name is the same key. Removal
// leaves no copy of it on the controller machine, and keeps a different key.
func TestRemoveMMXFromNodeRemovesCopiesOfTheManagedKey(t *testing.T) {
	f := newRemoveFixture(t)
	dir := filepath.Dir(f.keyFile)
	copyName := filepath.Join(dir, strings.Repeat("c", 64))
	otherName := filepath.Join(dir, strings.Repeat("d", 64))
	for _, suffix := range []string{"", ".pub"} {
		data, err := os.ReadFile(f.keyFile + suffix)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(copyName+suffix, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := access.EnsureKey(otherName); err != nil {
		t.Fatal(err)
	}
	copyIdentity := filepath.Join(f.c.State, "nodes", strings.Repeat("c", 64)+".json")
	otherIdentity := filepath.Join(f.c.State, "nodes", strings.Repeat("d", 64)+".json")
	for _, identity := range []string{copyIdentity, otherIdentity} {
		if err := os.MkdirAll(filepath.Dir(identity), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(identity, []byte("{}"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.run(); err != nil {
		t.Fatal(err)
	}
	for _, gone := range []string{copyName, copyName + ".pub", copyIdentity} {
		if _, err := os.Stat(gone); !os.IsNotExist(err) {
			t.Fatal("a copy of the managed key is still there:", gone, err)
		}
	}
	if _, err := os.Stat(otherName); err != nil {
		t.Fatal("another key was removed", err)
	}
	if _, err := os.Stat(otherIdentity); err != nil {
		t.Fatal("another node's saved identity was removed", err)
	}
}

// The authorized_keys script removes the managed key's line with the comment
// and the older line without it, and leaves every other line, such as another
// controller machine's managed key line.
func TestAuthorizedKeyRemovalScriptRemovesOnlyTheManagedKey(t *testing.T) {
	home := t.TempDir()
	if err := os.MkdirAll(filepath.Join(home, ".ssh"), 0700); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(home, ".ssh", "authorized_keys")
	before := strings.Join([]string{
		"ssh-ed25519 AAAAmine mmx managed key",
		"ssh-ed25519 AAAAyours you@laptop",
		"ssh-ed25519 AAAAmine",
		"ssh-ed25519 AAAAother mmx managed key",
		"command=\"false\" ssh-ed25519 AAAAmine",
	}, "\n") + "\n"
	if err := os.WriteFile(file, []byte(before), 0600); err != nil {
		t.Fatal(err)
	}
	run := func() string {
		out, err := exec.Command("/bin/sh", "-c", strings.Replace(authorizedKeyRemovalScript, "%[1]s", "AAAAmine", -1)).Output()
		if err != nil {
			t.Fatal(err)
		}
		return string(out)
	}
	t.Setenv("HOME", home)
	if out := run(); out != "removed 2 from "+file+"\n" {
		t.Fatalf("output %q", out)
	}
	after, _ := os.ReadFile(file)
	if want := "ssh-ed25519 AAAAyours you@laptop\nssh-ed25519 AAAAother mmx managed key\ncommand=\"false\" ssh-ed25519 AAAAmine\n"; string(after) != want {
		t.Fatalf("authorized_keys is now\n%s", after)
	}
	if out := run(); out != "" {
		t.Fatalf("a second run printed %q", out)
	}
}
