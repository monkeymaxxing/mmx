package node

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"mmx/internal/assets"
	"mmx/internal/config"
	"mmx/internal/engine"
	"mmx/internal/facts"
	"mmx/internal/modules"
	"mmx/internal/protocol"
)

func TestStatusDoesNotFinalizeARequestDuringAdmission(t *testing.T) {
	s := Server{Root: t.TempDir(), RuntimeRoot: t.TempDir()}
	id := strings.Repeat("a", 64)
	dir := s.jobDir(id)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "request.json"), []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	l, err := lock(filepath.Join(s.Root, "admission.lock"))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan protocol.Response, 1)
	failures := make(chan error, 1)
	go func() {
		r, err := s.Status(id)
		if err != nil {
			failures <- err
		}
		done <- r
	}()
	select {
	case <-done:
		t.Fatal("status bypassed the admission lock")
	case <-time.After(30 * time.Millisecond):
	}
	// Admission has completed and the job finished while status was waiting.
	if _, err := s.finish(protocol.Response{ID: id, Status: "succeeded"}); err != nil {
		unlock(l)
		t.Fatal(err)
	}
	unlock(l)
	select {
	case r := <-done:
		if r.Status != "succeeded" {
			t.Fatal(r)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("status did not resume")
	}
	select {
	case err := <-failures:
		t.Fatal(err)
	default:
	}

}

// jobWithWorker saves an admitted job; while running, the test holds its
// worker.lock as the job's worker would.
func jobWithWorker(t *testing.T, s Server, id string, running bool, journal string) {
	t.Helper()
	dir := s.jobDir(id)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{"request.json": `{}`, "progress.jsonl": journal} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if running {
		held, err := lock(filepath.Join(dir, "worker.lock"))
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return // this test already holds it
		}
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { unlock(held) })
	}
}

// fakeWorker is a runner binary whose started worker holds its job's lock,
// as a running job does, until release; release returns once it has exited.
func fakeWorker(t *testing.T) (string, func(s Server, id string)) {
	t.Helper()
	dir := t.TempDir()
	gate, bin := filepath.Join(dir, "release"), filepath.Join(dir, "runner")
	script := "#!/bin/sh\nenv > " + filepath.Join(dir, "env") + "\nwhile [ ! -e " + gate + " ]; do sleep 0.02; done\n"
	writeProgramFileThroughShellChild(t, bin, script)
	t.Cleanup(func() { _ = os.WriteFile(gate, nil, 0600) })
	return bin, func(s Server, id string) {
		t.Helper()
		if err := os.WriteFile(gate, nil, 0600); err != nil {
			t.Fatal(err)
		}
		exited, err := waitLock(filepath.Join(s.jobDir(id), "worker.lock"))
		if err != nil {
			t.Fatal(err)
		}
		unlock(exited)
	}
}

const journalFacts = `{"facts":{"collected_at":"2026-09-25T12:00:00Z","values":{"boot_id":"observed-boot"}}}` + "\n"

// A live read or a crash can leave a torn final line; it is dropped.
func TestStatusFoldsTheLiveJournalAndStopAcknowledgment(t *testing.T) {
	s := Server{Root: t.TempDir(), RuntimeRoot: t.TempDir()}
	id := strings.Repeat("b", 64)
	for _, torn := range []string{"", `{"result":{"task":"configure/`, "{not json}\n"} {
		jobWithWorker(t, s, id, true, journalFacts+
			`{"current_task":"configure/changes"}`+"\n"+
			`{"result":{"task":"configure/changes","status":"succeeded","change":"changed"}}`+"\n"+
			`{"current_task":"configure/restart"}`+"\n"+
			`{"result":{"task":"configure/restart","status":"succeeded","change":"changed"}}`+"\n"+
			`{"current_task":"configure/activate"}`+"\n"+torn)
		if err := os.WriteFile(filepath.Join(s.jobDir(id), "stop"), nil, 0600); err != nil {
			t.Fatal(err)
		}
		r, err := s.Status(id)
		if err != nil || r.Status != "running" || r.Facts == nil || r.Facts.Values["boot_id"] != "observed-boot" || !r.StopRequested ||
			r.CurrentTask != "configure/activate" || len(r.Results) != 2 || r.Results[1].Task != "configure/restart" {
			t.Fatal(torn, r, err)
		}
	}
}

func TestStatusReadsAJobWithoutJournalAsRunning(t *testing.T) {
	s := Server{Root: t.TempDir(), RuntimeRoot: t.TempDir()}
	id := strings.Repeat("d", 64)
	jobWithWorker(t, s, id, true, "")
	if err := os.Remove(filepath.Join(s.jobDir(id), "progress.jsonl")); err != nil {
		t.Fatal(err)
	}
	r, err := s.Status(id)
	if err != nil || r.Status != "running" || len(r.Results) != 0 || r.Facts != nil || r.CurrentTask != "" {
		t.Fatal(r, err)
	}
}

// Crash damage inside the journal ends the partial record; it must not block
// status from answering or an interrupted job from being finalized.
func TestStatusStopsAtADamagedJournalLine(t *testing.T) {
	for _, running := range []bool{true, false} {
		s := Server{Root: t.TempDir(), RuntimeRoot: t.TempDir()}
		id := strings.Repeat("e", 64)
		jobWithWorker(t, s, id, running, journalFacts+"\x00\x00\x00\n"+`{"result":{"task":"configure/changes","status":"succeeded","change":"changed"}}`+"\n")
		r, err := s.Status(id)
		if err != nil || r.Facts == nil || len(r.Results) != 0 || running != (r.Status == "running") {
			t.Fatal(running, r, err)
		}
	}
}

func TestInterruptedWorkerKeepsJournalResults(t *testing.T) {
	s := Server{Root: t.TempDir(), RuntimeRoot: t.TempDir()}
	id := strings.Repeat("f", 64)
	jobWithWorker(t, s, id, false, journalFacts+
		`{"result":{"task":"configure/changes","status":"succeeded","change":"changed"}}`+"\n"+
		`{"current_task":"configure/activate"}`+"\n"+`{"resu`)
	r, err := s.Status(id)
	if err != nil || r.Status != "failed" || !strings.Contains(r.Error, "interrupted") || r.Facts == nil || r.Facts.Values["boot_id"] != "observed-boot" ||
		len(r.Results) != 1 || r.Results[0].Task != "configure/changes" || r.CurrentTask != "" || r.Finished == nil {
		t.Fatal(r, err)
	}
	if saved, err := s.storedResult(id); err != nil || len(saved.Results) != 1 || saved.Facts == nil {
		t.Fatal(saved, err)
	}
}

// Each report appends only the newly retained result, so a task's write does
// not grow with the results already retained.
func TestJournalAppendsOnlyNewResults(t *testing.T) {
	s := Server{Root: t.TempDir()}
	id := strings.Repeat("a", 64)
	if err := os.MkdirAll(s.jobDir(id), 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(s.jobDir(id), "progress.jsonl")
	journal, err := openJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	defer journal.file.Close()
	size := func() int {
		st, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		return int(st.Size())
	}
	observation := protocol.Response{ID: id, Status: "running", Facts: &facts.Snapshot{Values: map[string]any{"boot_id": "b"}}}
	if err := journal.report(observation); err != nil {
		t.Fatal(err)
	}
	reason := strings.Repeat("x", 64<<10)
	for i := range 20 {
		before := size()
		task := fmt.Sprintf("task%d", i)
		if err := journal.starting(task); err != nil {
			t.Fatal(err)
		}
		result := protocol.Result{Task: task, Status: "failed", Change: "unchanged", Reason: reason}
		observation.Results = append(observation.Results, result)
		line, _ := json.Marshal(progressLine{Result: &result})
		want := len(`{"current_task":"`+task+`"}`) + 1 + len(line) + 1
		if err := journal.report(observation); err != nil {
			t.Fatal(err)
		}
		if got := size() - before; got != want {
			t.Fatalf("task %d appended %d bytes, want %d", i, got, want)
		}
	}
	r, err := s.progress(id)
	if err != nil || len(r.Results) != 20 || r.Results[19].Reason != reason || r.Facts.Values["boot_id"] != "b" || r.CurrentTask != "" {
		t.Fatal(len(r.Results), err)
	}
}

// Every caller waits for the node locks; only an actually active job is busy.
func TestNodeLocksWaitAndOnlyAnActiveJobIsBusy(t *testing.T) {
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
	pending := func(done chan error, what string) {
		t.Helper()
		select {
		case err := <-done:
			t.Fatal(what+" did not wait for its lock", err)
		case <-time.After(30 * time.Millisecond):
		}
	}
	resumed := func(done chan error, what string) {
		t.Helper()
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(what, err)
			}
		// A ceiling, not a measure: a busy test machine may take seconds.
		case <-time.After(10 * time.Second):
			t.Fatal(what + " did not resume")
		}
	}
	admission, err := lock(filepath.Join(s.Root, "admission.lock"))
	if err != nil {
		t.Fatal(err)
	}
	first, raw := job(1)
	submitted, uploaded := make(chan error, 1), make(chan error, 1)
	go func() {
		r, err := s.Submit(raw, modules.ControllerConnection{})
		if err == nil && r.Status != "running" {
			err = fmt.Errorf("unexpected admission %v", r)
		}
		submitted <- err
	}()
	data := []byte("asset")
	ref, _ := assets.Hash(context.Background(), bytes.NewReader(data))
	go func() {
		uploaded <- s.Upload(context.Background(), fmt.Sprintf("%064x", 2), ref, bytes.NewReader(data))
	}()
	pending(submitted, "submit")
	pending(uploaded, "upload")
	unlock(admission)
	resumed(submitted, "submit")
	resumed(uploaded, "upload")

	// The admitted worker is running: a definite busy answer, not contention.
	_, other := job(3)
	if _, err := s.Submit(other, modules.ControllerConnection{}); err == nil || !strings.Contains(err.Error(), "node busy with job "+first) {
		t.Fatal("admitted a second active job", err)
	}
	if err := s.Install(func() error { return nil }); err == nil || !strings.Contains(err.Error(), "node busy with job "+first) {
		t.Fatal("installed during an active job", err)
	}

	// The job finished; its worker still holds execution.lock while cleaning up.
	if _, err := s.finish(protocol.Response{ID: first, Status: "succeeded"}); err != nil {
		t.Fatal(err)
	}
	release(s, first)
	admission, err = lock(filepath.Join(s.Root, "admission.lock"))
	if err != nil {
		t.Fatal(err)
	}
	execution, err := lock(filepath.Join(s.Root, "execution.lock"))
	if err != nil {
		t.Fatal(err)
	}
	installed, worked := make(chan error, 1), make(chan error, 1)
	go func() { installed <- s.Install(func() error { return nil }) }()
	pending(installed, "install")
	unlock(admission)
	pending(installed, "install")
	// A late duplicate worker start waits instead of failing "host busy".
	go func() { worked <- s.Worker(first, nil) }()
	pending(worked, "worker")
	unlock(execution)
	resumed(installed, "install")
	resumed(worked, "worker")
}

func TestStatusAcknowledgesStopBeforeAdmission(t *testing.T) {
	s := Server{Root: t.TempDir(), RuntimeRoot: t.TempDir()}
	id := strings.Repeat("c", 64)
	if err := os.MkdirAll(s.jobDir(id), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.jobDir(id), "stop"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	r, err := s.Status(id)
	if err != nil || r.Status != "stopped" || !r.StopRequested {
		t.Fatal(r, err)
	}
}

// A started worker holds its job's lock from before it runs until it exits,
// with a fixed environment and its output in the job's log; once it has exited
// without a result, status reports the job interrupted.
func TestStartedWorkerHoldsItsJobLockUntilItExits(t *testing.T) {
	runner, release := fakeWorker(t)
	s := Server{Root: t.TempDir(), RuntimeRoot: t.TempDir(), Binary: runner}
	id := strings.Repeat("f", 64)
	jobWithWorker(t, s, id, false, journalFacts)
	t.Setenv("MMX_TEST_CALLER_ONLY", "leak")
	if err := s.startWorker(id); err != nil {
		t.Fatal(err)
	}
	if r, err := s.Status(id); err != nil || r.Status != "running" {
		t.Fatal("started worker not running", r, err)
	}
	release(s, id)
	env, err := os.ReadFile(filepath.Join(filepath.Dir(runner), "env"))
	if err != nil || !strings.Contains(string(env), "LANG=C.UTF-8\n") || !strings.Contains(string(env), "LOGNAME=root\n") || !strings.Contains(string(env), "\nHOME=/") || strings.Contains(string(env), "MMX_TEST_CALLER_ONLY") {
		t.Fatal("worker environment", string(env), err)
	}
	if _, err := os.Stat(filepath.Join(s.jobDir(id), "worker.log")); err != nil {
		t.Fatal(err)
	}
	if r, err := s.Status(id); err != nil || r.Status != "failed" || !strings.Contains(r.Error, "interrupted") {
		t.Fatal("exited worker still running", r, err)
	}
}

// A worker keeps its job's lock out of task processes: a daemon a task leaves
// running must not make the job look alive after the worker exits.
func TestTaskProcessesDoNotHoldTheJobLock(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("native Linux root test")
	}
	runner, release := fakeWorker(t)
	s := Server{Root: t.TempDir(), RuntimeRoot: t.TempDir(), Binary: runner}
	if err := os.Chmod(s.Root, 0700); err != nil {
		t.Fatal(err)
	}
	id := strings.Repeat("9", 64)
	raw, err := json.Marshal(config.Job{Protocol: protocol.Version, ID: id, Project: "test", FleetStage: "configure", Node: "test",
		NodeTasks: []config.Task{{ID: "daemon", Module: "shell", ModuleInputs: &modules.Shell{Script: "sleep 30 >/dev/null 2>&1 &"}}}})
	if err != nil {
		t.Fatal(err)
	}
	if r, err := s.Submit(raw, modules.ControllerConnection{}); err != nil || r.Status != "running" {
		t.Fatal(r, err)
	}
	release(s, id)
	held, err := lock(filepath.Join(s.jobDir(id), "worker.lock"))
	if err != nil {
		t.Fatal(err)
	}
	// A started worker inherits the lock without close-on-exec.
	if _, _, errno := syscall.Syscall(syscall.SYS_FCNTL, held.Fd(), syscall.F_SETFD, 0); errno != 0 {
		t.Fatal(errno)
	}
	if err := s.Worker(id, held); err != nil {
		t.Fatal(err)
	}
	after, err := lock(filepath.Join(s.jobDir(id), "worker.lock"))
	if err != nil {
		t.Fatal("a task process kept the job lock", err)
	}
	unlock(after)
	if r, err := s.Status(id); err != nil || r.Status != "succeeded" {
		t.Fatal(r, err)
	}
}

const runningJournal = journalFacts +
	`{"current_task":"configure/one"}` + "\n" +
	`{"result":{"task":"configure/one","status":"succeeded","change":"changed"}}` + "\n" +
	`{"current_task":"configure/two"}` + "\n"

// Progress answers once the node has moved past the request and the spacing
// has passed, and a running answer carries only the results the controller
// does not hold.
func TestProgressSpacesNewerRunningResults(t *testing.T) {
	s := Server{Root: t.TempDir(), RuntimeRoot: t.TempDir(), progressWait: 5 * time.Second}
	id := strings.Repeat("a", 64)
	jobWithWorker(t, s, id, true, runningJournal+`{"result":{"task":"configure/two","status":"succeeded","change":"unchanged"}}`+"\n")
	started := time.Now()
	fresh, err := s.Progress(protocol.ProgressRequest{ID: id})
	if waited := time.Since(started); err != nil || len(fresh.Results) != 2 || fresh.ResultsAfter != 0 || fresh.Facts == nil || waited < progressSpacing || waited > 3*time.Second {
		t.Fatal(fresh, err, waited)
	}
	later, err := s.Progress(protocol.ProgressRequest{ID: id, Results: 1, CurrentTask: "configure/two"})
	if err != nil || len(later.Results) != 1 || later.Results[0].Task != "configure/two" || later.ResultsAfter != 1 || later.Facts != nil {
		t.Fatal(later, err)
	}
}

// A job that finishes while a moved answer waits for its spacing answers as
// soon as its result is written.
func TestProgressAnswersAFinishingJobAtOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := Server{Root: t.TempDir(), RuntimeRoot: t.TempDir(), progressWait: 30 * time.Second, spacing: 5 * time.Second}
		id := strings.Repeat("d", 64)
		jobWithWorker(t, s, id, true, runningJournal)
		var response protocol.Response
		var progressError error
		done := make(chan struct{})
		go func() {
			response, progressError = s.Progress(protocol.ProgressRequest{ID: id})
			close(done)
		}()
		synctest.Wait() // Progress has inspected the running job and is waiting for spacing.
		select {
		case <-done:
			t.Fatal("progress answered before the job finished or spacing passed", response, progressError)
		default:
		}
		// The worker writes its result atomically, as durable.Write does.
		temporary := filepath.Join(s.jobDir(id), "result.json.new")
		if err := os.WriteFile(temporary, []byte(`{"id":"`+id+`","status":"succeeded","finished":"2026-09-27T00:00:00Z"}`), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Rename(temporary, filepath.Join(s.jobDir(id), "result.json")); err != nil {
			t.Fatal(err)
		}
		select {
		case <-done:
			if progressError != nil || response.Status != "succeeded" {
				t.Fatal(response, progressError)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("progress did not answer after the job finished")
		}
	})
}

// With nothing new, progress waits without a lock until the journal changes,
// then answers; with no change at all it answers when its wait ends.
func TestProgressWaitsForTheNextChange(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := Server{Root: t.TempDir(), RuntimeRoot: t.TempDir(), progressWait: 30 * time.Second}
		id := strings.Repeat("b", 64)
		jobWithWorker(t, s, id, true, runningJournal)
		held := protocol.ProgressRequest{ID: id, Results: 1, CurrentTask: "configure/two"}
		var response protocol.Response
		var progressError error
		done := make(chan struct{})
		go func() {
			response, progressError = s.Progress(held)
			close(done)
		}()
		synctest.Wait() // Progress has observed the unchanged journal and is waiting.
		select {
		case <-done:
			t.Fatal("progress answered before the journal changed", response, progressError)
		default:
		}
		if _, err := s.Status(id); err != nil { // the admission lock stays free while progress waits
			t.Fatal(err)
		}
		file, err := os.OpenFile(filepath.Join(s.jobDir(id), "progress.jsonl"), os.O_APPEND|os.O_WRONLY, 0600)
		if err != nil {
			t.Fatal(err)
		}
		_, writeError := file.WriteString(`{"result":{"task":"configure/two","status":"succeeded","change":"unchanged"}}` + "\n")
		closeError := file.Close()
		if err := errors.Join(writeError, closeError); err != nil {
			t.Fatal(err)
		}
		select {
		case <-done:
			if progressError != nil || len(response.Results) != 1 || response.ResultsAfter != 1 {
				t.Fatal(response, progressError)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("progress did not answer after the journal changed")
		}
		s.progressWait = 300 * time.Millisecond
		held.Results = 2
		held.CurrentTask = ""
		started := time.Now()
		response, err = s.Progress(held)
		if waited := time.Since(started); err != nil || waited < 300*time.Millisecond || len(response.Results) != 0 || response.ResultsAfter != 2 || response.Status != "running" {
			t.Fatal(response, err, waited)
		}
	})
}

// A job that is no longer running answers in full at once.
func TestProgressOfAFinishedJobIsComplete(t *testing.T) {
	s := Server{Root: t.TempDir(), RuntimeRoot: t.TempDir(), progressWait: 5 * time.Second}
	id := strings.Repeat("c", 64)
	jobWithWorker(t, s, id, false, runningJournal)
	started := time.Now()
	r, err := s.Progress(protocol.ProgressRequest{ID: id, Results: 1, CurrentTask: "configure/two"})
	if err != nil || r.Status != "failed" || len(r.Results) != 1 || r.ResultsAfter != 0 || time.Since(started) > 3*time.Second {
		t.Fatal(r, err)
	}
	if _, err := s.Progress(protocol.ProgressRequest{ID: "../escape"}); err == nil {
		t.Fatal("invalid job id accepted")
	}
}

// putPrivate leaves a job's private file as admission or the worker writes it.
func putPrivate(t *testing.T, s Server, id, kind string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(s.privatePath(id, kind)), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.privatePath(id, kind), []byte(`{"secret":"sentinel"}`), 0600); err != nil {
		t.Fatal(err)
	}
}

// A rebooting job's result is durable before its reboot request. The node
// answers rebooting while it runs on the boot the job left; the first answer
// on another boot records the reboot task's success with both boot IDs. A
// refused request fails the reboot task at once.
func TestRebootingJobSucceedsOnANewBoot(t *testing.T) {
	boot, requests := "boot-1", 0
	s := Server{Root: t.TempDir(), RuntimeRoot: t.TempDir(), machine: engine.NodeMachine{BootID: func() (string, error) { return boot, nil }}, announceWait: time.Millisecond}
	s.reboot = func() error { requests++; return nil }
	rebooting := func(id string) protocol.Response {
		jobWithWorker(t, s, id, false, "")
		r, err := s.finish(protocol.Response{ID: id, Status: "rebooting", Change: "changed", RebootFrom: "boot-1", Results: []protocol.Result{
			{Task: "patch", Status: "succeeded", Change: "unchanged"},
			{Task: "restart", Status: "rebooting", Change: "changed", Value: map[string]any{"boot_id_before": "boot-1"}}}})
		if err != nil || r.Status != "rebooting" {
			t.Fatal(r, err)
		}
		return r
	}
	id := strings.Repeat("d", 64)
	if err := s.requestReboot(rebooting(id)); err != nil || requests != 1 {
		t.Fatal(requests, err)
	}
	if r, err := s.Status(id); err != nil || r.Status != "rebooting" || r.RebootFrom != "boot-1" {
		t.Fatal(r, err)
	}
	boot = "boot-2"
	r, err := s.Status(id)
	task := r.Results[1]
	if err != nil || r.Status != "succeeded" || r.RebootFrom != "" || r.Change != "changed" || task.Status != "succeeded" || task.Reason != "" ||
		task.Value.(map[string]any)["boot_id_before"] != "boot-1" || task.Value.(map[string]any)["boot_id_after"] != "boot-2" {
		t.Fatal(r, err)
	}
	boot = "boot-3"
	if again, err := s.Status(id); err != nil || again.Results[1].Value.(map[string]any)["boot_id_after"] != "boot-2" {
		t.Fatal("a later boot rewrote the completed reboot", again, err)
	}

	boot = "boot-1"
	s.reboot = func() error { return fmt.Errorf("systemctl reboot: exit status 1: Access denied") }
	refused := strings.Repeat("e", 64)
	if err := s.requestReboot(rebooting(refused)); err != nil {
		t.Fatal(err)
	}
	r, err = s.Status(refused)
	if task := r.Results[1]; err != nil || r.Status != "failed" || r.RebootFrom != "" || r.Change != "unchanged" || task.Status != "failed" ||
		r.Error != "restart: systemctl reboot: exit status 1: Access denied" {
		t.Fatal(r, err)
	}
}

// A rebooting job's worker keeps execution.lock until its reboot request
// returns, so the node stays busy while that worker lives, and no longer.
func TestRebootingJobKeepsTheNodeBusyWhileItsWorkerLives(t *testing.T) {
	s := Server{Root: t.TempDir(), RuntimeRoot: t.TempDir(), machine: engine.NodeMachine{BootID: func() (string, error) { return "boot-1", nil }}}
	id := strings.Repeat("d", 64)
	jobWithWorker(t, s, id, false, "")
	if _, err := s.finish(protocol.Response{ID: id, Status: "rebooting", Change: "changed", RebootFrom: "boot-1", Results: []protocol.Result{
		{Task: "restart", Status: "rebooting", Change: "changed", Value: map[string]any{"boot_id_before": "boot-1"}}}}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.Root, "active"), []byte(id), 0600); err != nil {
		t.Fatal(err)
	}
	held, err := lock(filepath.Join(s.jobDir(id), "worker.lock"))
	if err != nil {
		t.Fatal(err)
	}
	if active, err := s.activeJob(); err != nil || active != id {
		t.Fatal("a rebooting job whose worker lives left the node free", active, err)
	}
	unlock(held)
	if active, err := s.activeJob(); err != nil || active != "" {
		t.Fatal("a rebooting job whose worker ended kept the node busy", active, err)
	}
}

// The worker reboots once an answer saying rebooting has gone out, so a
// connected controller hears of the reboot; without one it reboots after
// announceWait.
func TestRebootWaitsUntilItIsAnnounced(t *testing.T) {
	for _, announced := range []bool{true, false} {
		t.Run(fmt.Sprint(announced), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				s := Server{Root: t.TempDir(), RuntimeRoot: t.TempDir(), machine: engine.NodeMachine{BootID: func() (string, error) { return "boot-1", nil }},
					announceWait: 30 * time.Second, announceGrace: 10 * time.Millisecond}
				var rebooted time.Time
				s.reboot = func() error { rebooted = time.Now(); return nil }
				id := strings.Repeat("f", 64)
				jobWithWorker(t, s, id, false, "")
				response, err := s.finish(protocol.Response{ID: id, Status: "rebooting", Change: "changed", RebootFrom: "boot-1", Results: []protocol.Result{
					{Task: "restart", Status: "rebooting", Change: "changed", Value: map[string]any{"boot_id_before": "boot-1"}}}})
				if err != nil {
					t.Fatal(err)
				}
				started := time.Now()
				done := make(chan error, 1)
				go func() { done <- s.requestReboot(response) }()
				synctest.Wait() // The request has checked for an announcement and is waiting.
				select {
				case err := <-done:
					t.Fatal("reboot did not wait for its announcement", err)
				default:
				}
				if announced {
					if answer, err := s.Status(id); err != nil || answer.Status != "rebooting" {
						t.Fatal(answer, err)
					}
					select {
					case err := <-done:
						if err != nil {
							t.Fatal(err)
						}
					case <-time.After(3 * time.Second):
						t.Fatal("reboot did not follow its announcement")
					}
				} else {
					if err := <-done; err != nil {
						t.Fatal(err)
					}
					if waited := rebooted.Sub(started); waited < s.announceWait {
						t.Fatal("unannounced reboot did not wait for its fallback", waited)
					}
				}
			})
		})
	}
}

// A worker that ended before reading its private input leaves it behind;
// ending the job through status removes it with the job's execution assets.
func TestInterruptedJobReleasesPrivateInputAndAssets(t *testing.T) {
	s := Server{Root: t.TempDir(), RuntimeRoot: t.TempDir()}
	id := strings.Repeat("c", 64)
	jobWithWorker(t, s, id, false, journalFacts)
	putPrivate(t, s, id, "input")
	if err := os.MkdirAll(s.assetDir(id), 0700); err != nil {
		t.Fatal(err)
	}
	r, err := s.Status(id)
	if err != nil || r.Status != "failed" || !strings.Contains(r.Error, "interrupted") {
		t.Fatal(r, err)
	}
	if exists(s.privatePath(id, "input")) || exists(s.assetDir(id)) {
		t.Fatal("ended job kept its private input or execution assets")
	}
}

// While /etc/mmx/do_not_manage exists, a job that could change the node fails
// before its first node task, naming the file and its text; a dryrun inspects
// as usual and fails as the run would. Removing the file lets the next job run.
func TestDoNotManageRefusesJobsThatCouldChangeTheNode(t *testing.T) {
	dir := t.TempDir()
	s := Server{Root: t.TempDir(), RuntimeRoot: t.TempDir(), doNotManage: filepath.Join(dir, "do_not_manage")}
	managed := filepath.Join(dir, "managed")
	content := "hello\n"
	n := 0
	work := func(dryrun bool) protocol.Response {
		t.Helper()
		n++
		id := fmt.Sprintf("%064x", n)
		raw, err := json.Marshal(config.Job{Protocol: protocol.Version, ID: id, Project: "test", FleetStage: "configure", Node: "test", Dryrun: dryrun,
			Outputs:   map[string]any{"written": "yes"},
			NodeTasks: []config.Task{{ID: "managed", Module: "file", ModuleInputs: &modules.File{Path: managed, Content: &content, Mode: "0644"}}}})
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(s.jobDir(id), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(s.jobDir(id), "request.json"), raw, 0600); err != nil {
			t.Fatal(err)
		}
		r, err := s.work(id)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	notice := "alice: kernel debugging until 2026-10-08\n\x1b[31mcall +49 30 1234\n"
	if err := os.WriteFile(s.doNotManage, []byte(notice), 0644); err != nil {
		t.Fatal(err)
	}
	said := `the file says "alice: kernel debugging until 2026-10-08 / [31mcall +49 30 1234"`
	r := work(false)
	if r.Status != "failed" || !strings.HasPrefix(r.Error, "this node refuses management because "+s.doNotManage+" exists; "+said+". MMX never creates or removes this file") || len(r.Results) != 0 || r.Outputs != nil {
		t.Fatalf("%+v", r)
	}
	if exists(managed) {
		t.Fatal("a refused job changed the node")
	}
	r = work(true)
	if r.Status != "failed" || !strings.HasPrefix(r.Error, "a run would be refused: this node refuses management because") || !strings.Contains(r.Error, said) ||
		len(r.Results) != 1 || r.Results[0].Change != "would_change" || r.Outputs != nil {
		t.Fatalf("%+v", r)
	}
	for _, tc := range []struct{ text, said string }{
		{"", "the file is empty"},
		{" \n\n", "the file is empty"},
		{strings.Repeat("x", 3000), `the file says "` + strings.Repeat("x", 1024) + `" (only its first 1024 bytes are shown)`},
	} {
		if err := os.WriteFile(s.doNotManage, []byte(tc.text), 0644); err != nil {
			t.Fatal(err)
		}
		if r := work(false); r.Status != "failed" || !strings.Contains(r.Error, "exists; "+tc.said+". MMX never") {
			t.Fatalf("%q: %+v", tc.text, r)
		}
	}
	if err := os.Remove(s.doNotManage); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(s.doNotManage, 0700); err != nil {
		t.Fatal(err)
	}
	if r := work(false); r.Status != "failed" || !strings.Contains(r.Error, "it is not a regular file") {
		t.Fatalf("%+v", r)
	}
	if err := os.Remove(s.doNotManage); err != nil {
		t.Fatal(err)
	}
	// A symbolic link refuses: one to a file shows that file's text, and one
	// whose target is gone still refuses.
	notes := filepath.Join(t.TempDir(), "notes")
	if err := os.Symlink(notes, s.doNotManage); err != nil {
		t.Fatal(err)
	}
	if r := work(false); r.Status != "failed" || !strings.Contains(r.Error, "exists; it is a symbolic link whose target cannot be read") {
		t.Fatalf("%+v", r)
	}
	if err := os.WriteFile(notes, []byte("alice until Friday\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if r := work(false); r.Status != "failed" || !strings.Contains(r.Error, `the file says "alice until Friday"`) {
		t.Fatalf("%+v", r)
	}
	if err := os.Remove(s.doNotManage); err != nil {
		t.Fatal(err)
	}
	if r := work(false); r.Status != "succeeded" || r.Outputs["written"] != "yes" || !exists(managed) {
		t.Fatalf("%+v", r)
	}
}

func TestBusyMessageNamesWhoStartedTheNodeJob(t *testing.T) {
	s := Server{Root: t.TempDir()}
	id := fmt.Sprintf("%064x", 7)
	if err := os.MkdirAll(s.jobDir(id), 0700); err != nil {
		t.Fatal(err)
	}
	if got := s.startedBy(id); got != "" {
		t.Fatalf("an older node job record named %q", got)
	}
	if text := startedByText(config.Job{StartedByAccount: "you", StartedFromHostName: "laptop\n"}); text != "you@laptop" {
		t.Fatalf("started by text %q", text)
	}
	if err := os.WriteFile(filepath.Join(s.jobDir(id), "started_by"), []byte("you@laptop"), 0600); err != nil {
		t.Fatal(err)
	}
	if got := s.startedBy(id); got != ", started by you@laptop" {
		t.Fatalf("busy suffix %q", got)
	}
	if got := startedByText(config.Job{}); got != "" {
		t.Fatalf("empty job named %q", got)
	}
}
