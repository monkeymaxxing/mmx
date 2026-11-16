package node

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"mmx/internal/config"
	"mmx/internal/modules"
	"mmx/internal/protocol"
	"mmx/internal/value"
)

func TestPrivateAdmissionAndWorkerHandoff(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("root-owned runtime boundary is exercised by native Linux code tests")
	}
	runner, release := fakeWorker(t)
	s := Server{Root: t.TempDir(), RuntimeRoot: t.TempDir(), Binary: runner}
	if err := os.Chmod(s.Root, 0700); err != nil {
		t.Fatal(err)
	}
	j := config.Job{Protocol: protocol.Version, ID: strings.Repeat("e", 64), Project: "secret_test", FleetStage: "configure", Node: "a", Scope: map[string]any{"secrets": map[string]any{"token": &value.Secret{Name: "token", Data: "node-private-sentinel", Available: true}}}, NodeTasks: []config.Task{{ID: "derive", Module: "shell", ModuleInputs: &modules.Shell{Script: `printf '%s-derived' "$TOKEN"`, Env: map[string]any{"TOKEN": "{{ .secrets.token }}"}}}}, Outputs: map[string]any{"derived": "{{ .node_tasks.derive.stdout }}"}}
	raw, err := j.Transport()
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.Submit(raw, modules.ControllerConnection{})
	if err != nil || r.Status != "running" {
		t.Fatal(r, err)
	}
	saved, err := os.ReadFile(filepath.Join(s.jobDir(j.ID), "request.json"))
	if err != nil || strings.Contains(string(saved), "node-private-sentinel") {
		t.Fatal("plaintext persisted at admission", err)
	}
	if _, err := os.Stat(s.privatePath(j.ID, "input")); err != nil {
		t.Fatal("missing private handoff", err)
	}
	j.Scope["secrets"].(map[string]any)["token"] = &value.Secret{Name: "token", Data: "rotated-after-admission", Available: true}
	duplicate, _ := j.Transport()
	if _, err := s.Submit(duplicate, modules.ControllerConnection{}); err != nil {
		t.Fatal(err)
	}
	private, _ := os.ReadFile(s.privatePath(j.ID, "input"))
	if strings.Contains(string(private), "rotated-after-admission") {
		t.Fatal("duplicate changed an admitted input")
	}
	release(s, j.ID)
	if err := s.Worker(j.ID, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.privatePath(j.ID, "input")); !os.IsNotExist(err) {
		t.Fatal("handoff retained after worker read")
	}
	r, err = s.Status(j.ID)
	if err != nil || r.Status != "succeeded" || value.Plain(r.Outputs["derived"]) != "node-private-sentinel-derived" {
		t.Fatal(r.Status, err)
	}
	saved, _ = json.Marshal(r)
	if strings.Contains(string(saved), "node-private-sentinel") {
		t.Fatal("status persists private exports")
	}
	filepath.WalkDir(s.Root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			b, _ := os.ReadFile(path)
			if strings.Contains(string(b), "node-private-sentinel") {
				t.Error("secret in persistent node state", path)
			}
		}
		return nil
	})
	if err := os.Remove(s.privatePath(j.ID, "outputs")); err != nil {
		t.Fatal(err)
	}
	r, err = s.Status(j.ID)
	if err != nil || r.Status != "succeeded" {
		t.Fatal("losing volatile outputs replayed or failed completed work", err)
	}
	if err := value.RequireAvailable(r.Outputs); err == nil {
		t.Fatal("lost computed secret appears available")
	}
}

// Once a worker holds its job lock, its own failure ends the job through
// finish: the result records the cause, and the private input and execution
// assets are released.
func TestWorkerFailureEndsTheJobThroughFinish(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("root-owned runtime boundary is exercised by native Linux code tests")
	}
	s := Server{Root: t.TempDir(), RuntimeRoot: t.TempDir()}
	if err := os.Chmod(s.Root, 0700); err != nil {
		t.Fatal(err)
	}
	id := strings.Repeat("9", 64)
	request, err := json.Marshal(config.Job{Protocol: protocol.Version, ID: id, Project: "test", FleetStage: "configure", Node: "a", PrivateInputs: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(s.assetDir(id), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(s.jobDir(id), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.jobDir(id), "request.json"), request, 0600); err != nil {
		t.Fatal(err)
	}
	putPrivate(t, s, id, "input") // not a job: the worker cannot decode it
	if err := s.Worker(id, nil); err != nil {
		t.Fatal(err)
	}
	r, err := s.Status(id)
	if err != nil || r.Status != "failed" || r.Error != "invalid private node job input" || r.Finished == nil {
		t.Fatal(r, err)
	}
	if exists(s.privatePath(id, "input")) || exists(s.assetDir(id)) {
		t.Fatal("failed worker kept its private input or execution assets")
	}
}

// mode is the permission bits of the entry at path itself.
func mode(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode().Perm()
}

// The directory of the node tasks' private directories and the runner's
// volatile directory above it belong to the runner's account and have mode
// 0711: an ordinary account passes through them to the private directory of
// its own node task and can neither list them nor create anything in them.
// The node sets the mode itself, also on a volatile directory that an
// earlier runner made closed.
func TestRootOfPrivateTaskDirectoriesLetsAccountsOnlyPassThrough(t *testing.T) {
	s := Server{Root: t.TempDir(), RuntimeRoot: filepath.Join(t.TempDir(), "mmx")}
	if err := os.Mkdir(s.RuntimeRoot, 0700); err != nil {
		t.Fatal(err)
	}
	// The node job worker process makes its directories with umask 077.
	defer syscall.Umask(syscall.Umask(0077))
	dir, err := s.privateTaskDirectoryRoot()
	if err != nil || dir != filepath.Join(s.RuntimeRoot, "private_task_directories") {
		t.Fatal(dir, err)
	}
	for _, path := range []string{s.RuntimeRoot, dir} {
		info, err := os.Lstat(path)
		if err != nil || !info.IsDir() || info.Mode().Perm() != 0711 || int(info.Sys().(*syscall.Stat_t).Uid) != os.Geteuid() {
			t.Fatal("not the runner's own directory with mode 0711", path, info, err)
		}
	}
	// A second task finds the same directory.
	if again, err := s.privateTaskDirectoryRoot(); err != nil || again != dir || mode(t, dir) != 0711 {
		t.Fatal(again, err)
	}
	// What is not a directory in its place is refused and never followed.
	elsewhere := t.TempDir()
	for name, put := range map[string]func(string) error{
		"a link to a directory": func(path string) error { return os.Symlink(elsewhere, path) },
		"a file":                func(path string) error { return os.WriteFile(path, nil, 0600) },
	} {
		if err := os.Remove(dir); err != nil {
			t.Fatal(err)
		}
		if err := put(dir); err != nil {
			t.Fatal(err)
		}
		if _, err := s.privateTaskDirectoryRoot(); err == nil || !strings.Contains(err.Error(), "must be a directory") {
			t.Fatal(name, "was accepted as the directory of the private task directories", err)
		}
		if err := s.removeLeftoverPrivateTaskDirectories(); err == nil {
			t.Fatal(name, "was emptied as the directory of the private task directories")
		}
	}
	if mode(t, elsewhere) != 0700 {
		t.Fatal("the mode of a link's target changed")
	}
	if _, err := (Server{RuntimeRoot: "relative"}).privateTaskDirectoryRoot(); err == nil {
		t.Fatal("a relative volatile directory was accepted")
	}
}

// A private task directory stays while its node job's worker runs. A worker
// that is killed leaves it behind, with the password in it. The node removes
// it as soon as a request finds no running job, and admission asks first, so
// it is gone before the next node job starts. What the tool's account linked
// from its directory is not followed.
func TestPrivateTaskDirectoriesOfAKilledWorkerAreRemovedBeforeTheNextJob(t *testing.T) {
	s := Server{Root: t.TempDir(), RuntimeRoot: t.TempDir()}
	root, err := s.privateTaskDirectoryRoot()
	if err != nil {
		t.Fatal(err)
	}
	id := strings.Repeat("d", 64)
	jobWithWorker(t, s, id, false, journalFacts)
	worker, err := lock(filepath.Join(s.jobDir(id), "worker.lock"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.Root, "active"), []byte(id), 0600); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside")
	password := filepath.Join(root, "git-1", "password")
	if err := os.Mkdir(filepath.Dir(password), 0700); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{password, outside} {
		if err := os.WriteFile(path, []byte("s3cret"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(filepath.Dir(outside), filepath.Join(root, "git-1", "planted")); err != nil {
		t.Fatal(err)
	}

	// The job runs: status, stop, and housekeeping leave its task's files.
	if r, err := s.Status(id); err != nil || r.Status != "running" {
		t.Fatal(r, err)
	}
	if warnings := sweep(t, s, ""); len(warnings) != 0 || !exists(password) {
		t.Fatal("removed the private task directory of a running node job", warnings)
	}

	// The worker is killed: its lock is free and it wrote no result.
	unlock(worker)
	next := fmt.Sprintf("%064x", 2)
	if warnings := sweep(t, s, next); len(warnings) != 0 {
		t.Fatal(warnings)
	}
	if left, err := os.ReadDir(root); err != nil || len(left) != 0 {
		t.Fatal("a killed worker's private task directories are left for the next node job", left, err)
	}
	if !exists(outside) {
		t.Fatal("removing the private task directories followed a link")
	}
	if r, err := s.Status(id); err != nil || r.Status != "failed" || !strings.Contains(r.Error, "interrupted") {
		t.Fatal(r, err)
	}
}

// Admission of the next node job is where the node removes them at the
// latest: a second job is refused while the first runs, and its admission
// after the first worker died starts with nothing left.
func TestAdmissionRemovesThePrivateTaskDirectoriesOfAKilledWorker(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("native Linux root test")
	}
	runner, release := fakeWorker(t)
	s := Server{Root: t.TempDir(), RuntimeRoot: t.TempDir(), Binary: runner}
	if err := os.Chmod(s.Root, 0700); err != nil {
		t.Fatal(err)
	}
	job := func(n int) (string, []byte) {
		id := fmt.Sprintf("%064x", n)
		raw, err := json.Marshal(config.Job{Protocol: protocol.Version, ID: id, Project: "test", FleetStage: "configure", Node: "test"})
		if err != nil {
			t.Fatal(err)
		}
		return id, raw
	}
	first, raw := job(1)
	if r, err := s.Submit(raw, modules.ControllerConnection{}); err != nil || r.Status != "running" {
		t.Fatal(r, err)
	}
	root, err := s.privateTaskDirectoryRoot()
	if err != nil {
		t.Fatal(err)
	}
	// The directory of a git task that runs git as an ordinary account.
	password := filepath.Join(root, "git-1", "password")
	if err := os.Mkdir(filepath.Dir(password), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(password, []byte("s3cret"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{password, filepath.Dir(password)} {
		if err := os.Chown(path, 12345, 23456); err != nil {
			t.Fatal(err)
		}
	}
	second, other := job(2)
	if _, err := s.Submit(other, modules.ControllerConnection{}); err == nil || !strings.Contains(err.Error(), "node busy with job "+first) || !exists(password) {
		t.Fatal("a second node job was admitted, or the running job's private task directory was removed", err)
	}
	// The first worker ends without a result, as a killed one does.
	release(s, first)
	if r, err := s.Submit(other, modules.ControllerConnection{}); err != nil || r.Status != "running" {
		t.Fatal(r, err)
	}
	if left, err := os.ReadDir(root); err != nil || len(left) != 0 {
		t.Fatal("the next node job started beside a killed worker's private task directories", left, err)
	}
	release(s, second)
}
