package engine

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"mmx/internal/config"
	"mmx/internal/modules"
	"mmx/internal/protocol"
	"mmx/internal/value"
)

// A user task with password_hash reads a secret, so its whole result is
// suppressed in the job's response and in progress records, in dryrun and in
// a run; the hash reaches chpasswd on standard input alone. The stand-ins for
// the native account tools are the modules package's.
func TestUserPasswordHashResultIsSecret(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("account management runs on Linux nodes")
	}
	tools, err := filepath.Abs("../modules/testdata/accounts")
	if err != nil {
		t.Fatal(err)
	}
	// Without the stand-ins, as when the test binary runs on a test node, the
	// node's real account tools would come first on PATH and change real accounts.
	if _, err := os.Stat(filepath.Join(tools, "chpasswd")); err != nil {
		t.Skip("needs the stand-in account tools in internal/modules/testdata/accounts")
	}
	accounts := t.TempDir()
	for name, text := range map[string]string{"passwd": "root:x:0:0:root:/root:/bin/sh\n", "group": "root:x:0:\n", "shadow": "root:*:20000:0:99999:7:::\n", "calls": ""} {
		if err := os.WriteFile(filepath.Join(accounts, name), []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("MMX_FAKE_ACCOUNTS", accounts)
	t.Setenv("PATH", tools+string(os.PathListSeparator)+os.Getenv("PATH"))
	const hash = "$6$enginesalt$enginehashsentinel"
	create := false
	task := config.Task{ID: "alice", Module: "user", ModuleInputs: &modules.User{Name: "alice", Home: "/home/alice", CreateHomeDirectory: &create,
		PasswordHash: "{{ .secrets.alice_hash }}", ApplyPasswordHash: "only_when_account_has_no_password"}}
	for _, dryrun := range []bool{true, false} {
		j := job(task)
		j.Dryrun = dryrun
		j.Scope = map[string]any{"secrets": map[string]any{"alice_hash": &value.Secret{Name: "alice_hash", Data: hash, Available: true}}}
		e := Engine{Report: func(observation protocol.Response) error {
			if b, _ := json.Marshal(observation); strings.Contains(string(b), "enginehashsentinel") {
				t.Error("a password hash in a progress record")
			}
			return nil
		}}
		r := e.Run(context.Background(), j)
		requireStatus(t, r, "succeeded")
		b, _ := json.Marshal(r)
		if !r.Results[0].Secret || strings.Contains(string(b), "enginehashsentinel") || strings.Contains(string(b), "fields_changed") {
			t.Fatalf("the result of a task with password_hash was not suppressed: %s", b)
		}
	}
	if !strings.Contains(read(t, filepath.Join(accounts, "shadow")), "alice:"+hash+":") || strings.Contains(read(t, filepath.Join(accounts, "calls")), "sentinel") {
		t.Fatal("the hash was not set through standard input alone")
	}
}
