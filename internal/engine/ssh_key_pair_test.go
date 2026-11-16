package engine

import (
	"context"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"testing"

	"mmx/internal/config"
	"mmx/internal/modules"
)

// A later task and the fleet stage's outputs read an ssh_key_pair task's
// public_key. A dryrun that would create the pair reports them not checked;
// the run gives them the key, with the default comment <account>@<node>.
func TestSSHKeyPairPublicKeyReachesLaterTasksAndOutputs(t *testing.T) {
	if _, err := exec.LookPath("ssh-keygen"); err != nil {
		t.Skip("ssh-keygen is not installed")
	}
	me, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	copied := filepath.Join(dir, "copied.pub")
	key := config.Task{ID: "backup_key", Module: "ssh_key_pair", ModuleInputs: &modules.SSHKeyPair{ForUserAccount: me.Username, PrivateKeyPath: filepath.Join(dir, ".ssh", "id_ed25519"), KeyType: "ed25519"}}
	j := job(key, taskFile(copied, "{{ .node_tasks.backup_key.public_key }}\n"))
	j.Outputs = map[string]any{"backup_public_key": "{{ .node_tasks.backup_key.public_key }}"}
	e := Engine{RuntimeRoot: filepath.Join(dir, "state")}

	j.Dryrun = true
	r := e.Run(context.Background(), j)
	requireStatus(t, r, "not_checked")
	if r.Results[0].Change != "would_change" || r.Results[1].Status != "not_checked" || !strings.Contains(r.Results[1].Reason, "public key is not known in a dryrun") {
		t.Fatalf("%+v", r.Results)
	}
	if _, err := os.Lstat(filepath.Join(dir, ".ssh")); !os.IsNotExist(err) {
		t.Fatal("dryrun made the key directory")
	}

	j.Dryrun = false
	r = e.Run(context.Background(), j)
	requireStatus(t, r, "succeeded")
	public := r.Results[0].PublicKey
	if !strings.HasPrefix(public, "ssh-ed25519 ") || !strings.HasSuffix(public, " "+me.Username+"@test") {
		t.Fatalf("%+v", r.Results[0])
	}
	if read(t, copied) != public+"\n" || r.Outputs["backup_public_key"] != public {
		t.Fatal(read(t, copied), r.Outputs)
	}

	j.Dryrun = true
	r = e.Run(context.Background(), j)
	requireStatus(t, r, "succeeded")
	if r.Change != "unchanged" || r.Outputs["backup_public_key"] != public {
		t.Fatalf("%+v %v", r.Results, r.Outputs)
	}

	// Without ssh-keygen, the dryrun does not check the pair, and a later
	// task that reads its public key is not checked either.
	t.Setenv("PATH", t.TempDir())
	r = e.Run(context.Background(), j)
	if r.Results[0].Status != "not_checked" || r.Results[1].Status != "not_checked" || !strings.Contains(r.Results[1].Reason, "ssh-keygen is not installed") {
		t.Fatalf("without ssh-keygen: %+v", r.Results)
	}
}
