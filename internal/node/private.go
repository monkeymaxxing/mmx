package node

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"mmx/internal/durable"
	"mmx/internal/protocol"
	"mmx/internal/value"
)

// RuntimeRoot is the runner's volatile state directory on the /run tmpfs.
const RuntimeRoot = "/run/mmx"

// privateTaskDirectoriesName is the directory in the runner's volatile
// directory that holds the private directories of the running node job's
// node tasks. A node task makes one for what it hands a tool for as long as
// the task runs: a login for git, docker, or podman, a copy of a new unit
// file or Quadlet file for a check that an ordinary account runs, and the
// scratch files of Docker Compose's dry run. The modules make a task's
// directory there through one helper, which owns the rule for it
// (internal/modules/private_task_directory.go).
const privateTaskDirectoriesName = "private_task_directories"

// passThroughMode is the mode of the runner's volatile directory and of the
// directory privateTaskDirectoriesName in it. Both belong to root. A node
// task can run git, podman, or a check as an ordinary account, and that
// account must reach the private directory of its own task, so every account
// may pass through these two (the search bit). No account but root can list them or create, rename, or
// remove anything in them. What else the volatile directory holds is closed
// by its own mode 0700: private and services.
const passThroughMode = 0711

// runtimeDir makes the runner's volatile directory with its mode. Whatever
// mode an earlier runner gave it, MMX sets its own. A node job worker
// process calls it before its first node task, and so does whatever writes a
// secret there.
func (s Server) runtimeDir() (string, error) {
	if !filepath.IsAbs(s.RuntimeRoot) {
		return "", fmt.Errorf("internal error: the mmx-runner runtime directory requires an absolute path")
	}
	return s.RuntimeRoot, passThroughDirectory(s.RuntimeRoot)
}

// passThroughDirectory makes dir when it is missing, requires it to be a
// directory of the runner's own account, which is root (Initialize refuses
// to admit or run a job as any other), and gives it passThroughMode. A new directory needs the mode set too: the node
// job worker process runs with umask 077.
func passThroughDirectory(dir string) error {
	if err := os.Mkdir(dir, passThroughMode); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	owner, ok := info.Sys().(*syscall.Stat_t)
	if !info.IsDir() || !ok || int(owner.Uid) != os.Geteuid() {
		return fmt.Errorf("%s must be a directory that the node runner's account, root, owns", dir)
	}
	if info.Mode().Perm() == passThroughMode {
		return nil
	}
	return os.Chmod(dir, passThroughMode)
}

// requireMemoryBacked refuses a node whose /run is on a disk: what MMX keeps
// in the runner's volatile directory holds passwords and keys. Tests use
// another directory, which is not checked.
func (s Server) requireMemoryBacked() error {
	if s.RuntimeRoot != RuntimeRoot {
		return nil
	}
	return requireMemoryFilesystem(filepath.Dir(RuntimeRoot))
}

// privateTaskDirectoryRoot returns the directory in which each node task's
// private directory is made, itself made and checked. The node job worker
// process hands this function to the node task execution engine, which asks
// when a node task needs a private directory.
func (s Server) privateTaskDirectoryRoot() (string, error) {
	root, err := s.runtimeDir()
	if err == nil {
		err = s.requireMemoryBacked()
	}
	if err != nil {
		return "", err
	}
	dir := filepath.Join(root, privateTaskDirectoriesName)
	return dir, passThroughDirectory(dir)
}

// removeLeftoverPrivateTaskDirectories removes every node task's private
// directory. A node task removes its own when it ends, so what is here while
// no node job runs was left by a node job worker process that was killed. The caller holds
// admission.lock and found no active job: only a running job's worker makes
// these directories, and none can start meanwhile, so nothing here is in use.
// A task's directory belongs to the account its tool ran as, which may have
// put anything into it; os.RemoveAll follows no link.
func (s Server) removeLeftoverPrivateTaskDirectories() error {
	dir := filepath.Join(s.RuntimeRoot, privateTaskDirectoriesName)
	info, err := os.Lstat(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", dir)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	var failures []error
	for _, entry := range entries {
		failures = append(failures, os.RemoveAll(filepath.Join(dir, entry.Name())))
	}
	return errors.Join(failures...)
}

// Secret handoff and computed exports live only in the node's root-private
// runtime filesystem. Persistent job records contain references and statuses.
func (s Server) privateDir() (string, error) {
	root, err := s.runtimeDir()
	if err == nil {
		err = s.requireMemoryBacked()
	}
	if err != nil {
		return "", err
	}
	dir := filepath.Join(root, "private")
	if err := os.Mkdir(dir, 0700); err != nil && !errors.Is(err, os.ErrExist) {
		return "", err
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return "", err
	}
	owner, ok := info.Sys().(*syscall.Stat_t)
	if !info.IsDir() || info.Mode().Perm() != 0700 || !ok || owner.Uid != 0 {
		return "", fmt.Errorf("private node job storage must be a directory owned by root with mode 0700")
	}
	return dir, nil
}
func (s Server) privatePath(id, kind string) string {
	return filepath.Join(s.RuntimeRoot, "private", id+"-"+kind+".json")
}
func (s Server) removePrivate(id, kind string) error {
	if err := os.Remove(s.privatePath(id, kind)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
func (s Server) writePrivate(id, kind string, b []byte) error {
	if _, err := s.privateDir(); err != nil {
		return err
	}
	return durable.Write(s.privatePath(id, kind), b, 0600)
}

// hiddenDetailsPath is the file in the private runtime storage that keeps,
// for root, what the hidden results of a node job's failed node tasks leave
// out: the reason, the exit code, and the output, which may hold a secret.
// It goes with the node job's records, as the job's secret outputs do.
func (s Server) hiddenDetailsPath(id string) string {
	return filepath.Join(s.RuntimeRoot, "private", id+"-hidden-details-of-failed-node-tasks.txt")
}

// keepHiddenDetails adds the details of one failed node task to the node
// job's hidden details file and returns the file.
func (s Server) keepHiddenDetails(id string, r protocol.Result) (string, error) {
	if _, err := s.privateDir(); err != nil {
		return "", err
	}
	var text strings.Builder
	fmt.Fprintf(&text, "Node task: %s\nStatus: %s\nExit code: %d\nReason: %s\n", r.Task, r.Status, r.ExitCode, r.Reason)
	for _, output := range []struct {
		name, text string
		shortened  bool
	}{{"Standard error", r.Stderr, r.StderrTruncated}, {"Standard output", r.Stdout, r.StdoutTruncated}} {
		if output.text == "" {
			continue
		}
		if output.shortened {
			output.name += ", shortened"
		}
		fmt.Fprintf(&text, "%s:\n%s", output.name, output.text)
		if !strings.HasSuffix(output.text, "\n") {
			text.WriteString("\n")
		}
	}
	text.WriteString("\n")
	path := s.hiddenDetailsPath(id)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0600)
	if err != nil {
		return "", err
	}
	if _, err := f.WriteString(text.String()); err != nil {
		f.Close()
		return "", err
	}
	return path, f.Close()
}

func (s Server) privateOutputs(r protocol.Response) (protocol.Response, error) {
	if !value.HasSecrets(r.Outputs) {
		return r, nil
	}
	b, err := os.ReadFile(s.privatePath(r.ID, "outputs"))
	if os.IsNotExist(err) {
		return r, nil
	}
	if err != nil {
		return r, err
	}
	var outputs map[string]any
	if err := protocol.Decode(b, &outputs); err != nil {
		return r, fmt.Errorf("invalid private output record")
	}
	r.Outputs = value.Restore(outputs).(map[string]any)
	return r, nil
}
func (s Server) savePrivateOutputs(r protocol.Response) error {
	if !value.HasSecrets(r.Outputs) {
		return nil
	}
	b, err := json.Marshal(value.Transport(r.Outputs))
	if err != nil {
		return err
	}
	return s.writePrivate(r.ID, "outputs", b)
}
