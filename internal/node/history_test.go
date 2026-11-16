package node

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mmx/internal/config"
	"mmx/internal/durable"
	"mmx/internal/protocol"
	"mmx/internal/value"
)

func historyJob(t *testing.T, s Server, n int, status string) protocol.Response {
	t.Helper()
	id := fmt.Sprintf("%064x", n)
	if err := os.MkdirAll(s.jobDir(id), 0700); err != nil {
		t.Fatal(err)
	}
	if err := durable.JSON(filepath.Join(s.jobDir(id), "request.json"), config.Job{ID: id}); err != nil {
		t.Fatal(err)
	}
	r := protocol.Response{ID: id, Status: status}
	if protocol.Terminal(status) {
		var err error
		if r, err = s.finish(r); err != nil {
			t.Fatal(err)
		}
	}
	return r
}
func sweep(t *testing.T, s Server, preserve string) []string {
	t.Helper()
	l, err := lock(filepath.Join(s.Root, "admission.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer unlock(l)
	return s.sweepHistory(preserve)
}
func exists(path string) bool { _, err := os.Stat(path); return err == nil }

// A finished job expires hostRetention after its result was written, whatever
// its outcome; reading it does not extend that. Running work and the job being
// answered stay.
func TestNodeRetentionExpiresFinishedJobsByAge(t *testing.T) {
	now := time.Now()
	s := Server{Root: t.TempDir(), RuntimeRoot: t.TempDir(), now: func() time.Time { return now }}
	succeeded := historyJob(t, s, 1, "succeeded")
	failed := historyJob(t, s, 2, "failed")
	stopped := historyJob(t, s, 3, "stopped")
	queued := historyJob(t, s, 4, "running")
	protected := historyJob(t, s, 5, "succeeded")
	now = now.Add(nodeRetention - time.Minute)
	if r, err := s.Status(succeeded.ID); err != nil || r.Status != "succeeded" {
		t.Fatal(r, err)
	}
	if warnings := sweep(t, s, protected.ID); len(warnings) != 0 {
		t.Fatal(warnings)
	}
	for _, id := range []string{succeeded.ID, failed.ID, stopped.ID} {
		if !exists(s.jobDir(id)) {
			t.Fatal("removed a job before its retention ended", id)
		}
	}
	now = now.Add(2 * time.Minute)
	if warnings := sweep(t, s, protected.ID); len(warnings) != 0 {
		t.Fatal(warnings)
	}
	for _, id := range []string{succeeded.ID, failed.ID, stopped.ID} {
		if exists(s.jobDir(id)) {
			t.Fatal("expired job retained", id)
		}
	}
	for _, id := range []string{queued.ID, protected.ID} {
		if !exists(s.jobDir(id)) {
			t.Fatal("removed running or answered job", id)
		}
	}
}

func TestHousekeepingFollowsTheActiveJobNotExecutionLock(t *testing.T) {
	now := time.Now()
	s := Server{Root: t.TempDir(), RuntimeRoot: t.TempDir(), now: func() time.Time { return now }}
	r := historyJob(t, s, 1, "succeeded")
	now = now.Add(nodeRetention + time.Minute)
	// This started worker holds its job's lock but not execution.lock yet.
	queued := historyJob(t, s, 2, "running")
	started, err := lock(filepath.Join(s.jobDir(queued.ID), "worker.lock"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.Root, "active"), []byte(queued.ID), 0600); err != nil {
		t.Fatal(err)
	}
	if warnings := sweep(t, s, ""); len(warnings) != 0 || !exists(s.jobDir(r.ID)) {
		t.Fatal("expired history while a worker was active", warnings)
	}
	// A finished worker may still hold execution.lock while it removes its files.
	if _, err := s.finish(protocol.Response{ID: queued.ID, Status: "succeeded"}); err != nil {
		t.Fatal(err)
	}
	unlock(started)
	worker, err := lock(filepath.Join(s.Root, "execution.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer unlock(worker)
	if warnings := sweep(t, s, ""); len(warnings) != 0 || exists(s.jobDir(r.ID)) {
		t.Fatal("housekeeping consulted execution.lock", warnings)
	}
}

func TestStopBeforeAdmissionExpiresAfterRetention(t *testing.T) {
	s := Server{Root: t.TempDir(), RuntimeRoot: t.TempDir()}
	old := time.Now().Add(-nodeRetention - time.Hour)
	stopOnly := func(n int) string {
		id := fmt.Sprintf("%064x", n)
		if err := os.MkdirAll(s.jobDir(id), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(s.jobDir(id), "stop"), nil, 0600); err != nil {
			t.Fatal(err)
		}
		return id
	}
	expired, recent, uploading := stopOnly(1), stopOnly(2), stopOnly(3)
	// A runner stopped between saving the private input and the request.
	putPrivate(t, s, expired, "input")
	upload, err := s.assetLock(uploading)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock(upload)
	admitted := historyJob(t, s, 4, "running")
	for _, id := range []string{expired, uploading, admitted.ID} {
		if err := os.Chtimes(s.jobDir(id), old, old); err != nil {
			t.Fatal(err)
		}
	}
	if warnings := sweep(t, s, ""); len(warnings) != 0 {
		t.Fatal(warnings)
	}
	if exists(s.jobDir(expired)) {
		t.Fatal("stop marker for a never-admitted job was kept forever")
	}
	if exists(s.privatePath(expired, "input")) {
		t.Fatal("private input of a never-admitted job outlived its expiry")
	}
	for _, id := range []string{recent, uploading, admitted.ID} {
		if !exists(s.jobDir(id)) {
			t.Fatal("removed protected job directory", id)
		}
	}
	// A recent marker still refuses a late admission of that job.
	if r, err := s.Status(recent); err != nil || r.Status != "stopped" {
		t.Fatal(r, err)
	}
}

func TestExpiryDefersForUploadsAndRetriesFailedDeletion(t *testing.T) {
	now := time.Now()
	s := Server{Root: t.TempDir(), RuntimeRoot: t.TempDir(), now: func() time.Time { return now }}
	r := historyJob(t, s, 1, "succeeded")
	now = now.Add(nodeRetention + time.Minute)
	upload, err := s.assetLock(r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if warnings := sweep(t, s, ""); len(warnings) != 0 || !exists(s.jobDir(r.ID)) {
		t.Fatal(warnings)
	}
	unlock(upload)
	if err := os.MkdirAll(s.assetDir(r.ID), 0700); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"input", "outputs"} {
		if err := os.MkdirAll(filepath.Dir(s.privatePath(r.ID, kind)), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(s.privatePath(r.ID, kind), []byte("private"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(s.hiddenDetailsPath(r.ID), []byte("private"), 0600); err != nil {
		t.Fatal(err)
	}
	service := filepath.Join(s.RuntimeRoot, "service-record")
	runner := filepath.Join(s.Root, "runner")
	for _, path := range []string{service, runner} {
		if err := os.WriteFile(path, []byte("keep"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	s.removeAll = func(path string) error {
		if strings.Contains(path, "/expired/") {
			// Even deleting some files before the failure must permit later cleanup.
			_ = os.Remove(filepath.Join(path, "request.json"))
			return errors.New("injected deletion failure")
		}
		return os.RemoveAll(path)
	}
	kept := historyJob(t, s, 2, "succeeded")
	response, err := s.Status(kept.ID)
	if err != nil || response.Status != "succeeded" || len(response.Housekeeping) != 1 || !strings.Contains(response.Housekeeping[0], "injected deletion failure") {
		t.Fatal(response, err)
	}
	if exists(s.jobDir(r.ID)) || !exists(filepath.Join(s.Root, "expired", r.ID)) {
		t.Fatal("expiry did not stage deletion")
	}
	s.removeAll = nil
	if warnings := sweep(t, s, ""); len(warnings) != 0 {
		t.Fatal(warnings)
	}
	for _, path := range []string{filepath.Join(s.Root, "expired", r.ID), s.assetDir(r.ID), s.privatePath(r.ID, "outputs"), s.hiddenDetailsPath(r.ID)} {
		if exists(path) {
			t.Fatal("left expired artifact", path)
		}
	}
	if !exists(service) || !exists(runner) {
		t.Fatal("removed service or runner state")
	}
}

// The node keeps the details of each failed node task whose result is
// hidden in one file of the node job, in its private runtime storage, which
// only root can read.
func TestHiddenDetailsOfFailedNodeTasksStayInPrivateStorage(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("private storage requires root; exercised by the native Linux suite")
	}
	s := Server{Root: t.TempDir(), RuntimeRoot: t.TempDir()}
	id := fmt.Sprintf("%064x", 1)
	for _, r := range []protocol.Result{
		{Task: "load/insert_rows", Status: "failed", ExitCode: 1, Reason: "exit code 1", Stderr: "ERROR:  invalid input syntax: secret-row\n", Stdout: "COPY 0"},
		{Task: "load", Status: "failed", Reason: "load/insert_rows: exit code 1", Stdout: "partial", StdoutTruncated: true},
	} {
		if path, err := s.keepHiddenDetails(id, r); err != nil || path != filepath.Join(s.RuntimeRoot, "private", id+"-hidden-details-of-failed-node-tasks.txt") {
			t.Fatal(path, err)
		}
	}
	want := "Node task: load/insert_rows\nStatus: failed\nExit code: 1\nReason: exit code 1\nStandard error:\nERROR:  invalid input syntax: secret-row\nStandard output:\nCOPY 0\n\n" +
		"Node task: load\nStatus: failed\nExit code: 0\nReason: load/insert_rows: exit code 1\nStandard output, shortened:\npartial\n\n"
	info, err := os.Stat(s.hiddenDetailsPath(id))
	if got, _ := os.ReadFile(s.hiddenDetailsPath(id)); err != nil || string(got) != want || info.Mode().Perm() != 0600 {
		t.Fatalf("%q\n%v %v", got, info, err)
	}
	if info, err := os.Stat(filepath.Join(s.RuntimeRoot, "private")); err != nil || info.Mode().Perm() != 0700 {
		t.Fatal(info, err)
	}
}

func TestTerminalRecordExcludesVolatileSecretExports(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("private storage requires root; exercised by the native Linux suite")
	}
	s := Server{Root: t.TempDir(), RuntimeRoot: t.TempDir()}
	r := historyJob(t, s, 1, "running")
	r.Status, r.Outputs = "succeeded", map[string]any{"token": value.Sensitive("secret-token")}
	if err := s.savePrivateOutputs(r); err != nil {
		t.Fatal(err)
	}
	r, err := s.finish(r)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(s.jobDir(r.ID), "result.json"))
	if bytes.Contains(raw, []byte("secret-token")) {
		t.Fatal("secret in terminal record")
	}
	if err := os.Remove(s.privatePath(r.ID, "outputs")); err != nil {
		t.Fatal(err)
	}
	missing, err := s.result(r.ID)
	if err != nil || value.RequireAvailable(missing.Outputs) == nil {
		t.Fatal(missing, err)
	}
	wire, err := json.Marshal(missing)
	if err != nil || bytes.Contains(wire, []byte("secret-token")) {
		t.Fatal(string(wire), err)
	}
}
