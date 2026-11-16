package modules

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"testing"

	"mmx/internal/protocol"
)

// keyPairFixture asks for a key pair of the current account in a directory
// that does not exist yet.
func keyPairFixture(t *testing.T, keyType string) SSHKeyPair {
	t.Helper()
	if _, err := exec.LookPath("ssh-keygen"); err != nil {
		t.Skip("ssh-keygen is not installed")
	}
	me, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	return SSHKeyPair{ForUserAccount: me.Username, PrivateKeyPath: filepath.Join(t.TempDir(), ".ssh", "id_"+keyType), KeyType: keyType, KeyCommentWhenCreated: "backup@backup01"}
}

// noPrivateKey fails when the result, in any form it is saved or shown,
// holds a line of the private key's text.
func noPrivateKey(t *testing.T, k SSHKeyPair, results ...protocol.Result) {
	t.Helper()
	private, err := os.ReadFile(k.PrivateKeyPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range results {
		saved, _ := json.Marshal(r)
		for _, line := range strings.Split(string(private), "\n") {
			if len(line) > 20 && !strings.HasPrefix(line, "-----") && strings.Contains(string(saved), line) {
				t.Fatalf("result holds the private key: %s", saved)
			}
		}
	}
}

func mode(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode().Perm()
}

func TestSSHKeyPairIsCreatedOnceAndReturnsTheSameKey(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for _, keyType := range []string{"ed25519", "rsa"} {
		k := keyPairFixture(t, keyType)
		preview := (Runtime{Dryrun: true}).SSHKeyPair(ctx, k)
		if preview.Status != "succeeded" || preview.Change != "would_change" || preview.PublicKey != "" || !strings.Contains(preview.Reason, "unknown") {
			t.Fatalf("%+v", preview)
		}
		if _, err := os.Lstat(filepath.Dir(k.PrivateKeyPath)); !os.IsNotExist(err) {
			t.Fatal("dryrun created the key directory")
		}
		made := (Runtime{}).SSHKeyPair(ctx, k)
		prefix := map[string]string{"ed25519": "ssh-ed25519 ", "rsa": "ssh-rsa "}[keyType]
		if made.Status != "succeeded" || made.Change != "changed" || !strings.HasPrefix(made.PublicKey, prefix) || !strings.HasSuffix(made.PublicKey, " backup@backup01") {
			t.Fatalf("%+v", made)
		}
		for path, want := range map[string]os.FileMode{filepath.Dir(k.PrivateKeyPath): 0700, k.PrivateKeyPath: 0600, k.PublicKeyPath(): 0644} {
			if got := mode(t, path); got != want {
				t.Fatal(path, got)
			}
		}
		if b, _ := os.ReadFile(k.PublicKeyPath()); strings.TrimSpace(string(b)) != made.PublicKey {
			t.Fatalf("%q", b)
		}
		again := (Runtime{}).SSHKeyPair(ctx, k)
		checked := (Runtime{Dryrun: true}).SSHKeyPair(ctx, k)
		for _, r := range []protocol.Result{again, checked} {
			if r.Status != "succeeded" || r.Change != "unchanged" || r.PublicKey != made.PublicKey {
				t.Fatalf("%+v", r)
			}
		}
		noPrivateKey(t, k, preview, made, again, checked)
	}
}

// A missing .pub file is written again from the private key, in dryrun
// shown and not written; the private key stays as it was.
func TestSSHKeyPairWritesAMissingPublicKeyAgain(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	k := keyPairFixture(t, "ed25519")
	made := (Runtime{}).SSHKeyPair(ctx, k)
	private, _ := os.ReadFile(k.PrivateKeyPath)
	if err := os.Remove(k.PublicKeyPath()); err != nil {
		t.Fatal(err)
	}
	key := strings.Join(strings.Fields(made.PublicKey)[:2], " ")
	preview := (Runtime{Dryrun: true}).SSHKeyPair(ctx, k)
	if preview.Change != "would_change" || !strings.HasPrefix(preview.PublicKey, key) || preview.FileChange == nil || preview.FileChange.Action != "create" {
		t.Fatalf("%+v", preview)
	}
	if _, err := os.Lstat(k.PublicKeyPath()); !os.IsNotExist(err) {
		t.Fatal("dryrun wrote the public key")
	}
	written := (Runtime{}).SSHKeyPair(ctx, k)
	if written.Status != "succeeded" || written.Change != "changed" || !strings.HasPrefix(written.PublicKey, key) {
		t.Fatalf("%+v", written)
	}
	if b, _ := os.ReadFile(k.PublicKeyPath()); !strings.HasPrefix(string(b), key) || mode(t, k.PublicKeyPath()) != 0644 {
		t.Fatalf("%q", b)
	}
	if after, _ := os.ReadFile(k.PrivateKeyPath); string(after) != string(private) {
		t.Fatal("the private key changed")
	}
	if r := (Runtime{}).SSHKeyPair(ctx, k); r.Change != "unchanged" || r.PublicKey != written.PublicKey {
		t.Fatalf("%+v", r)
	}
	noPrivateKey(t, k, preview, written)
}

// An existing key is never replaced: a key of another type, or a .pub file
// that holds another key, fails the task in dryrun and run alike.
func TestSSHKeyPairNeverReplacesAKey(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	k := keyPairFixture(t, "ed25519")
	(Runtime{}).SSHKeyPair(ctx, k)
	private, _ := os.ReadFile(k.PrivateKeyPath)
	public, _ := os.ReadFile(k.PublicKeyPath())
	wrongType := k
	wrongType.KeyType = "rsa"
	for _, dryrun := range []bool{true, false} {
		r := (Runtime{Dryrun: dryrun}).SSHKeyPair(ctx, wrongType)
		if r.Status != "failed" || !strings.Contains(r.Reason, "is a ssh-ed25519 key, not rsa as key_type says") || !strings.Contains(r.Reason, "state: absent") {
			t.Fatalf("%+v", r)
		}
		noPrivateKey(t, k, r)
	}
	other := keyPairFixture(t, "ed25519")
	(Runtime{}).SSHKeyPair(ctx, other)
	otherPublic, _ := os.ReadFile(other.PublicKeyPath())
	if err := os.WriteFile(k.PublicKeyPath(), otherPublic, 0644); err != nil {
		t.Fatal(err)
	}
	if r := (Runtime{}).SSHKeyPair(ctx, k); r.Status != "failed" || !strings.Contains(r.Reason, "does not match private key file") {
		t.Fatalf("%+v", r)
	}
	if after, _ := os.ReadFile(k.PrivateKeyPath); string(after) != string(private) {
		t.Fatal("the private key changed")
	}
	if err := os.WriteFile(k.PublicKeyPath(), public, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(k.PrivateKeyPath, filepath.Join(filepath.Dir(k.PrivateKeyPath), "id_link")); err != nil {
		t.Fatal(err)
	}
	link := k
	link.PrivateKeyPath = filepath.Join(filepath.Dir(k.PrivateKeyPath), "id_link")
	if r := (Runtime{}).SSHKeyPair(ctx, link); r.Status != "failed" || !strings.Contains(r.Reason, "expected a regular file") {
		t.Fatalf("%+v", r)
	}
}

// What an earlier task may still provide (ssh-keygen, the account, the
// directory above .ssh) is not checked in dryrun and fails a run.
func TestSSHKeyPairWaitsForWhatEarlierTasksProvide(t *testing.T) {
	ctx := context.Background()
	k := keyPairFixture(t, "ed25519")
	missingDir := k
	missingDir.PrivateKeyPath = filepath.Join(t.TempDir(), "home", "backup", ".ssh", "id_ed25519")
	missingAccount := k
	missingAccount.ForUserAccount = "mmx-missing-user"
	for _, tc := range []struct {
		task SSHKeyPair
		path string
		want string
	}{
		{missingDir, os.Getenv("PATH"), "does not exist yet"},
		{missingAccount, os.Getenv("PATH"), "unknown user"},
		{k, t.TempDir(), missingProgramError("ssh-keygen").Error()},
	} {
		t.Setenv("PATH", tc.path)
		if r := (Runtime{Dryrun: true}).SSHKeyPair(ctx, tc.task); r.Status != "not_checked" || !strings.Contains(r.Reason, tc.want) {
			t.Fatalf("%+v", r)
		}
		if r := (Runtime{}).SSHKeyPair(ctx, tc.task); r.Status != "failed" || !strings.Contains(r.Reason, tc.want) {
			t.Fatalf("%+v", r)
		}
	}
	if _, err := os.Lstat(filepath.Dir(k.PrivateKeyPath)); !os.IsNotExist(err) {
		t.Fatal("a failed task created the key directory")
	}
}

func TestSSHKeyPairRejectsInvalidInput(t *testing.T) {
	t.Parallel()
	k := keyPairFixture(t, "ed25519")
	for _, tc := range []struct {
		change func(*SSHKeyPair)
		want   string
	}{
		{func(k *SSHKeyPair) { k.ForUserAccount = "" }, "for_user_account is required"},
		{func(k *SSHKeyPair) { k.PrivateKeyPath = "" }, "private_key_path is required"},
		{func(k *SSHKeyPair) { k.KeyType = "dsa" }, "key_type is required: ed25519 or rsa"},
	} {
		task := k
		tc.change(&task)
		if r := (Runtime{}).SSHKeyPair(context.Background(), task); r.Status != "failed" || r.FailureKind != "config" || !strings.Contains(r.Reason, tc.want) {
			t.Fatalf("%+v", r)
		}
	}
}

// ssh-keygen failing after the key directory was made leaves the task failed
// and changed; the next try makes the pair in that directory.
func TestSSHKeyPairAfterAFailedCreation(t *testing.T) {
	ctx := context.Background()
	k := keyPairFixture(t, "ed25519")
	shim := t.TempDir()
	writeStandInProgram(t, filepath.Join(shim, "ssh-keygen"), "#!/bin/sh\necho 'no space left on device' >&2\nexit 1\n")
	path := os.Getenv("PATH")
	t.Setenv("PATH", shim+string(os.PathListSeparator)+path)
	r := (Runtime{}).SSHKeyPair(ctx, k)
	if r.Status != "failed" || r.Change != "changed" || !strings.Contains(r.Reason, "no space left on device") {
		t.Fatalf("%+v", r)
	}
	if mode(t, filepath.Dir(k.PrivateKeyPath)) != 0700 {
		t.Fatal("the key directory was not made")
	}
	t.Setenv("PATH", path)
	if r := (Runtime{}).SSHKeyPair(ctx, k); r.Status != "succeeded" || r.Change != "changed" || r.PublicKey == "" {
		t.Fatalf("%+v", r)
	}
}
