package node

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"mmx/internal/durable"
	"mmx/internal/engine"
	"mmx/internal/modules"
	"mmx/internal/protocol"
	"mmx/internal/value"
)

type restoreTestClock struct {
	sync.Mutex
	now time.Time
}

func (c *restoreTestClock) read() time.Time         { c.Lock(); defer c.Unlock(); return c.now }
func (c *restoreTestClock) advance(d time.Duration) { c.Lock(); defer c.Unlock(); c.now = c.now.Add(d) }

type activationForTest struct {
	activate func(bool) error
	check    func(bool) error
	calls    atomic.Int64
}

func (a *activationForTest) PrepareActivation(context.Context, modules.Runtime, bool) error {
	return nil
}
func (a *activationForTest) Activate(_ context.Context, _ modules.Runtime, restoring bool) error {
	a.calls.Add(1)
	if a.activate != nil {
		return a.activate(restoring)
	}
	return nil
}
func (a *activationForTest) RemoveObsoleteVirtualDevices(context.Context, modules.Runtime, bool) error {
	return nil
}
func (a *activationForTest) CheckActivatedConfiguration(_ context.Context, _ modules.Runtime, restoring bool) error {
	if a.check != nil {
		return a.check(restoring)
	}
	return nil
}

type restoreFixture struct {
	server             Server
	clock              *restoreTestClock
	adapter            *activationForTest
	jobID, destination string
	worker             *os.File
	stopOnce           sync.Once
	systemMu           sync.Mutex
	commands           []string
	timerNext          string
	activationState    string
	failCommand        string
}

func newRestoreFixture(t *testing.T) *restoreFixture {
	t.Helper()
	f := &restoreFixture{jobID: strings.Repeat("a", 64), destination: filepath.Join(t.TempDir(), "network.conf"), timerNext: "tomorrow", activationState: "inactive"}
	f.clock = &restoreTestClock{now: time.Date(2030, 1, 2, 3, 4, 5, 600000000, time.UTC)}
	f.adapter = &activationForTest{}
	f.server = Server{Root: t.TempDir(), RuntimeRoot: t.TempDir(), Binary: "/opt/mmx/bin/example/mmx-runner", unitDirectory: t.TempDir(), now: f.clock.read,
		machine:           engine.NodeMachine{BootID: func() (string, error) { return "original-boot", nil }},
		activationFactory: func(string, string, json.RawMessage) (modules.ChangeActivation, error) { return f.adapter, nil }, extendRestoreWait: func() error { return nil }}
	f.server.systemctl = f.systemctl
	for _, dir := range []string{f.server.jobDir(f.jobID), filepath.Join(f.server.systemdDirectory(), "multi-user.target.wants"), filepath.Join(f.server.systemdDirectory(), "timers.target.wants")} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(f.server.jobDir(f.jobID), "request.json"), []byte(`{}`), 0600); err != nil {
		t.Fatal(err)
	}
	var err error
	f.worker, err = lock(filepath.Join(f.server.jobDir(f.jobID), "worker.lock"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(f.stopWorker)
	return f
}
func (f *restoreFixture) stopWorker() { f.stopOnce.Do(func() { unlock(f.worker) }) }
func (f *restoreFixture) systemctl(_ context.Context, args ...string) (string, error) {
	f.systemMu.Lock()
	defer f.systemMu.Unlock()
	command := strings.Join(args, " ")
	f.commands = append(f.commands, command)
	if f.failCommand != "" && strings.Contains(command, f.failCommand) {
		return "", fmt.Errorf("test systemctl failure: %s", command)
	}
	if args[0] == "is-enabled" {
		return "enabled", nil
	}
	if args[0] == "show" && strings.HasSuffix(args[1], ".timer") {
		return "LoadState=loaded\nActiveState=active\nNextElapseUSecRealtime=" + f.timerNext, nil
	}
	if args[0] == "show" && slices.Contains(args, "--property=LoadState") {
		return "loaded", nil
	}
	if args[0] == "show" {
		return f.activationState, nil
	}
	return "", nil
}
func (f *restoreFixture) record(t *testing.T, phase string, kinds ...protocol.ConnectionKind) owedRestoreRecord {
	t.Helper()
	now := f.clock.read()
	r := owedRestoreRecord{Outcome: protocol.OwedRestore{NodeTaskPath: "network.configure", ModuleName: "file", DestinationFile: f.destination},
		NodeJobID: f.jobID, NodeBootIDBeforeChange: "original-boot", Phase: phase, ConfirmationWaitDuration: "1m0s", BeforeChangeConfirmationDeadline: now.Add(time.Minute), LatestAfterChangeConfirmationDeadline: now.Add(150 * time.Second), ConnectionKindsToConfirm: kinds, ModuleRestoreData: json.RawMessage(`{}`), Change: "unchanged"}
	if phase != waitingBeforeChange {
		r.BeforeChangeCompletedAt = now
	}
	if phase != waitingBeforeChange && phase != preparingRestore {
		r.BeforeChangeConfirmedConnectionKinds = slices.Clone(kinds)
		if phase == changeAccepted {
			r.AfterChangeConfirmedConnectionKinds = slices.Clone(kinds)
		}
		r.RestoreDeadline = now.Add(time.Minute)
		r.PreviousFile = &modules.PreviousFileMetadata{}
		r.ChangeStarted = true
		r.Change = "changed"
	}
	if phase == restoreFailed {
		r.Outcome.RestoreFailureReason = "native activation refused previous configuration"
	}
	if err := f.server.withOwedRestores(context.Background(), func() error { return f.server.createOwedRestore(&r) }); err != nil {
		t.Fatal(err)
	}
	return r
}
func readRestore(t *testing.T, s Server, id string) owedRestoreRecord {
	t.Helper()
	var r owedRestoreRecord
	if err := s.withOwedRestores(context.Background(), func() error { var err error; r, err = s.readOwedRestore(id); return err }); err != nil {
		t.Fatal(err)
	}
	return r
}
func confirm(t *testing.T, s Server, r owedRestoreRecord, phase protocol.ConfirmationPhase, kind protocol.ConnectionKind) protocol.ReachabilityConfirmationResponse {
	t.Helper()
	answer, err := s.ConfirmReachability(context.Background(), protocol.ReachabilityConfirmationRequest{NodeJobID: r.NodeJobID, OwedRestoreID: r.Outcome.OwedRestoreID, ConfirmationPhase: phase}, kind)
	if err != nil {
		t.Fatal(err)
	}
	return answer
}

func TestBeforeChangeFreezesExactlyConfirmedConnections(t *testing.T) {
	for _, test := range []struct {
		name                string
		eligible, confirmed []protocol.ConnectionKind
		expired, success    bool
	}{
		{"ssh", []protocol.ConnectionKind{protocol.ConnectionSSH, protocol.ConnectionHTTPS}, []protocol.ConnectionKind{protocol.ConnectionSSH}, true, true},
		{"https", []protocol.ConnectionKind{protocol.ConnectionSSH, protocol.ConnectionHTTPS}, []protocol.ConnectionKind{protocol.ConnectionHTTPS}, true, true},
		{"both", []protocol.ConnectionKind{protocol.ConnectionSSH, protocol.ConnectionHTTPS}, []protocol.ConnectionKind{protocol.ConnectionSSH, protocol.ConnectionHTTPS}, false, true},
		{"neither", []protocol.ConnectionKind{protocol.ConnectionSSH, protocol.ConnectionHTTPS}, nil, true, false},
		{"ssh_only", []protocol.ConnectionKind{protocol.ConnectionSSH}, []protocol.ConnectionKind{protocol.ConnectionSSH}, false, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newRestoreFixture(t)
			r := f.record(t, waitingBeforeChange, test.eligible...)
			for _, kind := range test.confirmed {
				if a := confirm(t, f.server, r, protocol.ConfirmationBeforeChange, kind); !a.ConfirmationCounted {
					t.Fatal(a)
				}
			}
			if test.expired {
				f.clock.advance(time.Minute)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			result, err := f.server.waitBeforeChange(ctx, r.Outcome.OwedRestoreID)
			if (err == nil) != test.success {
				t.Fatal(result, err)
			}
			if test.success && (result.Phase != preparingRestore || !slices.Equal(result.BeforeChangeConfirmedConnectionKinds, test.confirmed)) {
				t.Fatal(result)
			}
		})
	}
}

func TestConfirmationCannotSelectAnotherJobChangePhaseOrConnection(t *testing.T) {
	for _, test := range []struct {
		name   string
		phase  string
		mutate func(*protocol.ReachabilityConfirmationRequest)
		kind   protocol.ConnectionKind
		reason string
	}{
		{"other_job", waitingBeforeChange, func(r *protocol.ReachabilityConfirmationRequest) { r.NodeJobID = strings.Repeat("b", 64) }, protocol.ConnectionSSH, "node_job_does_not_match"},
		{"other_change", waitingBeforeChange, func(r *protocol.ReachabilityConfirmationRequest) { r.OwedRestoreID = "ffffffffffffffff" }, protocol.ConnectionSSH, "owed_restore_not_found"},
		{"after_before_ready", activatingConfig, func(r *protocol.ReachabilityConfirmationRequest) {
			r.ConfirmationPhase = protocol.ConfirmationAfterChange
		}, protocol.ConnectionSSH, "confirmation_phase_not_ready"},
		{"before_after_phase", waitingAfterChange, func(*protocol.ReachabilityConfirmationRequest) {}, protocol.ConnectionSSH, "confirmation_phase_not_ready"},
		{"https_on_ssh_only", waitingBeforeChange, func(*protocol.ReachabilityConfirmationRequest) {}, protocol.ConnectionHTTPS, "connection_kind_not_required"},
	} {
		t.Run(test.name, func(t *testing.T) {
			f := newRestoreFixture(t)
			r := f.record(t, test.phase, protocol.ConnectionSSH)
			req := protocol.ReachabilityConfirmationRequest{NodeJobID: r.NodeJobID, OwedRestoreID: r.Outcome.OwedRestoreID, ConfirmationPhase: protocol.ConfirmationBeforeChange}
			test.mutate(&req)
			answer, err := f.server.ConfirmReachability(context.Background(), req, test.kind)
			if err != nil || answer.ConfirmationCounted || answer.ConfirmationReason != test.reason {
				t.Fatal(answer, err)
			}
			if got := readRestore(t, f.server, r.Outcome.OwedRestoreID); got.Revision != r.Revision {
				t.Fatal("rejected request changed record", got)
			}
		})
	}
}

func TestRepeatedConfirmationSurvivesLostResponseAndAcceptance(t *testing.T) {
	f := newRestoreFixture(t)
	r := f.record(t, waitingAfterChange, protocol.ConnectionSSH, protocol.ConnectionHTTPS)
	for _, kind := range []protocol.ConnectionKind{protocol.ConnectionSSH, protocol.ConnectionSSH, protocol.ConnectionHTTPS, protocol.ConnectionHTTPS} {
		if a := confirm(t, f.server, r, protocol.ConfirmationAfterChange, kind); !a.ConfirmationCounted {
			t.Fatal(a)
		}
	}
	got := readRestore(t, f.server, r.Outcome.OwedRestoreID)
	if got.Phase != changeAccepted || len(got.AfterChangeConfirmedConnectionKinds) != 2 {
		t.Fatal(got)
	}
	if a := confirm(t, f.server, r, protocol.ConfirmationBeforeChange, protocol.ConnectionSSH); a.ConfirmationCounted {
		t.Fatal(a)
	}
}

// lockWaitBarrier observes a blocked lock acquisition without sleeping or
// adding hooks to production locking. The uncontended restore-execution lock
// does not call Done; the contended record lock does.
type lockWaitBarrier struct {
	context.Context
	waiting chan struct{}
	once    sync.Once
}

func (c *lockWaitBarrier) Done() <-chan struct{} {
	c.once.Do(func() { close(c.waiting) })
	return c.Context.Done()
}

func TestConfirmationAndExpiryHaveOneDurableWinner(t *testing.T) {
	for _, expiryFirst := range []bool{false, true} {
		t.Run(fmt.Sprint(expiryFirst), func(t *testing.T) {
			f := newRestoreFixture(t)
			r := f.record(t, waitingAfterChange, protocol.ConnectionSSH)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			entered, release := make(chan struct{}), make(chan struct{})
			var releaseOnce sync.Once
			unblock := func() { releaseOnce.Do(func() { close(release) }) }
			defer unblock()
			first, second := f.server, f.server
			first.machine.BootID = func() (string, error) {
				close(entered) // Already read the record, still inside its decision lock.
				select {
				case <-release:
					return "original-boot", nil
				case <-ctx.Done():
					return "", ctx.Err()
				}
			}
			secondDecided := make(chan struct{}, 1)
			second.machine.BootID = func() (string, error) {
				select {
				case secondDecided <- struct{}{}:
				default:
				}
				return "original-boot", nil
			}
			blocked := &lockWaitBarrier{Context: ctx, waiting: make(chan struct{})}
			var answer protocol.ReachabilityConfirmationResponse
			confirmationDone, restoreDone := make(chan error, 1), make(chan error, 1)
			confirmChange := func(ctx context.Context, server Server) {
				var err error
				answer, err = server.ConfirmReachability(ctx, protocol.ReachabilityConfirmationRequest{NodeJobID: r.NodeJobID, OwedRestoreID: r.Outcome.OwedRestoreID, ConfirmationPhase: protocol.ConfirmationAfterChange}, protocol.ConnectionSSH)
				confirmationDone <- err
			}
			if expiryFirst {
				f.clock.advance(time.Minute)
				go func() { restoreDone <- first.RunOwedRestore(ctx, r.Outcome.OwedRestoreID) }()
			} else {
				go confirmChange(ctx, first)
			}
			select {
			case <-entered:
			case <-ctx.Done():
				t.Fatal("first decision did not reach its barrier")
			}
			if expiryFirst {
				go confirmChange(blocked, second)
			} else {
				go func() { restoreDone <- second.RunOwedRestore(blocked, r.Outcome.OwedRestoreID) }()
			}
			select {
			case <-blocked.waiting:
				// Both invocations are in flight; only the first can decide.
			case <-secondDecided:
				t.Error("competing invocation read an undecided record without its lock")
			case <-ctx.Done():
				t.Error("competing invocation did not contend for the decision lock")
			}
			unblock()
			confirmationErr, restoreErr := <-confirmationDone, <-restoreDone
			if confirmationErr != nil || restoreErr != nil {
				t.Fatal(confirmationErr, restoreErr)
			}
			if answer.ConfirmationCounted == expiryFirst {
				t.Fatal(answer)
			}
			got := readRestore(t, f.server, r.Outcome.OwedRestoreID)
			if expiryFirst && got.Phase != configRestored || !expiryFirst && got.Phase != changeAccepted {
				t.Fatal(got)
			}
			wantCalls := int64(0)
			if expiryFirst {
				wantCalls = 1
			}
			if calls := f.adapter.calls.Load(); calls != wantCalls {
				t.Fatal("wrong native restore count", calls)
			}
		})
	}
}

func TestDeadWorkerAndRebootCannotConfirm(t *testing.T) {
	for _, reboot := range []bool{false, true} {
		t.Run(fmt.Sprint(reboot), func(t *testing.T) {
			f := newRestoreFixture(t)
			r := f.record(t, waitingAfterChange, protocol.ConnectionSSH)
			if reboot {
				f.server.machine.BootID = func() (string, error) { return "new-boot", nil }
			} else {
				f.stopWorker()
			}
			a := confirm(t, f.server, r, protocol.ConfirmationAfterChange, protocol.ConnectionSSH)
			if a.ConfirmationCounted || a.ConfirmationReason != "node_job_worker_not_running_on_original_boot" {
				t.Fatal(a)
			}
			if got := readRestore(t, f.server, r.Outcome.OwedRestoreID); got.Phase != restorationRequired {
				t.Fatal(got)
			}
		})
	}
}

func TestDeadWorkerRestoresFromEveryMutationPhase(t *testing.T) {
	for _, phase := range []string{waitingBeforeChange, preparingRestore, applyingChange, activatingConfig, waitingAfterChange} {
		t.Run(phase, func(t *testing.T) {
			f := newRestoreFixture(t)
			r := f.record(t, phase, protocol.ConnectionSSH)
			if r.ChangeStarted {
				if err := os.WriteFile(f.destination, []byte("proposed"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			f.stopWorker()
			if err := f.server.RunOwedRestore(context.Background(), r.Outcome.OwedRestoreID); err != nil {
				t.Fatal(err)
			}
			got := readRestore(t, f.server, r.Outcome.OwedRestoreID)
			want := configRestored
			if !r.ChangeStarted {
				want = abandonedChange
			}
			if got.Phase != want || !got.CleanupComplete {
				t.Fatal(got)
			}
			if _, err := os.Lstat(f.destination); !os.IsNotExist(err) {
				t.Fatal("previous absence not restored", err)
			}
		})
	}
}

func TestEarlyRestoreStartWaitsForDeadlineOrAcceptance(t *testing.T) {
	for _, accept := range []bool{false, true} {
		t.Run(fmt.Sprint(accept), func(t *testing.T) {
			f := newRestoreFixture(t)
			r := f.record(t, waitingAfterChange, protocol.ConnectionSSH)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- f.server.RunOwedRestore(ctx, r.Outcome.OwedRestoreID) }()
			select {
			case err := <-done:
				t.Fatal("early service exited", err)
			case <-time.After(40 * time.Millisecond):
			}
			if accept {
				if a := confirm(t, f.server, r, protocol.ConfirmationAfterChange, protocol.ConnectionSSH); !a.ConfirmationCounted {
					t.Fatal(a)
				}
			} else {
				f.clock.advance(time.Minute)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			if calls := f.adapter.calls.Load(); (calls == 0) != accept {
				t.Fatal(calls)
			}
		})
	}
}

func TestDuplicateRestoreIsSerializedAndDoesNotRepeatSuccessfulActivation(t *testing.T) {
	f := newRestoreFixture(t)
	r := f.record(t, waitingAfterChange, protocol.ConnectionSSH)
	f.stopWorker()
	entered, release := make(chan struct{}), make(chan struct{})
	f.adapter.activate = func(bool) error { close(entered); <-release; return nil }
	done := make(chan error, 2)
	go func() { done <- f.server.RunOwedRestore(context.Background(), r.Outcome.OwedRestoreID) }()
	<-entered
	go func() { done <- f.server.RunOwedRestore(context.Background(), r.Outcome.OwedRestoreID) }()
	close(release)
	for i := 0; i < 2; i++ {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	if f.adapter.calls.Load() != 1 {
		t.Fatal("restored twice")
	}
}

func TestDelayedActivationAfterRestoreClaimDoesNothing(t *testing.T) {
	f := newRestoreFixture(t)
	r := f.record(t, restorationRequired, protocol.ConnectionSSH)
	if err := f.server.ActivateChange(context.Background(), r.Outcome.OwedRestoreID); err != nil {
		t.Fatal(err)
	}
	if f.adapter.calls.Load() != 0 {
		t.Fatal("late activation ran")
	}
}

func TestRestoreFailureKeepsPreviousBytesAndBlocksSameDestination(t *testing.T) {
	f := newRestoreFixture(t)
	r := f.record(t, waitingAfterChange, protocol.ConnectionSSH)
	previous := "previous"
	proposed := "proposed"
	if err := os.WriteFile(f.destination, []byte(previous), 0640); err != nil {
		t.Fatal(err)
	}
	prepared, result := (modules.Runtime{}).PrepareFileChange(context.Background(), modules.File{Path: f.destination, Literal: &proposed}, value.Scope{}, nil)
	if prepared == nil {
		t.Fatal(result)
	}
	defer prepared.Close()
	metadata, err := prepared.SavePreviousFile(context.Background(), filepath.Join(f.server.restoreDirectory(r.Outcome.OwedRestoreID), "previous_file_contents"))
	if err != nil {
		t.Fatal(err)
	}
	r.PreviousFile = &metadata
	if err := f.server.withOwedRestores(context.Background(), func() error { return f.server.writeOwedRestore(&r) }); err != nil {
		t.Fatal(err)
	}
	if result := prepared.Install(context.Background()); result.Status != "succeeded" {
		t.Fatal(result)
	}
	f.adapter.activate = func(bool) error { return fmt.Errorf("native activation refused previous configuration") }
	f.stopWorker()
	if err := f.server.RunOwedRestore(context.Background(), r.Outcome.OwedRestoreID); err == nil {
		t.Fatal("restore should fail")
	}
	got := readRestore(t, f.server, r.Outcome.OwedRestoreID)
	if got.Phase != restoreFailed || !got.Outcome.PreviousFileRestored || got.Outcome.PreviousConfigActivated {
		t.Fatal(got)
	}
	if bytes, err := os.ReadFile(f.destination); err != nil || string(bytes) != previous {
		t.Fatal(string(bytes), err)
	}
	for _, dryrun := range []bool{false, true} {
		runtime := modules.Runtime{Dryrun: dryrun, CheckFileDestination: f.server.CheckChangeAllowed}
		result := runtime.File(context.Background(), modules.File{Path: f.destination, Literal: &previous}, value.Scope{})
		if result.Status != "failed" || !strings.Contains(result.Reason, f.jobID) || !strings.Contains(result.Reason, "native activation refused") {
			t.Fatal(result)
		}
		link := runtime.Symlink(context.Background(), modules.Symlink{Path: f.destination, Target: "other.conf", ReplaceExistingFile: true})
		if link.Status != "failed" || !strings.Contains(link.Reason, r.Outcome.RestoreServiceUnit) {
			t.Fatal(link)
		}
		other := filepath.Join(filepath.Dir(f.destination), "other.conf")
		if r := runtime.File(context.Background(), modules.File{Path: other, Literal: &proposed}, value.Scope{}); r.Status != "succeeded" {
			t.Fatal(r)
		}
	}
	// A new worker for another job still sees the same on-disk refusal.
	if err := f.server.CheckChangeAllowed(context.Background(), f.destination); err == nil {
		t.Fatal("forgot failed restore")
	}
	f.adapter.activate = nil
	if err := f.server.RunOwedRestore(context.Background(), r.Outcome.OwedRestoreID); err != nil {
		t.Fatal(err)
	}
	if err := f.server.CheckChangeAllowed(context.Background(), f.destination); err != nil {
		t.Fatal(err)
	}
}

func TestTimerReadinessAndPastDeadlineRefuseArming(t *testing.T) {
	for _, failure := range []string{"elapsed", "expired", "enable", "start"} {
		t.Run(failure, func(t *testing.T) {
			f := newRestoreFixture(t)
			r := f.record(t, waitingAfterChange, protocol.ConnectionSSH)
			if failure == "elapsed" {
				f.timerNext = ""
			}
			if failure == "expired" {
				f.clock.advance(time.Minute)
			}
			if failure == "enable" || failure == "start" {
				f.failCommand = failure + " "
			}
			if err := f.server.armRestore(context.Background(), r); err == nil {
				t.Fatal("armed unusable timer")
			}
			if _, err := os.Lstat(f.destination); !os.IsNotExist(err) {
				t.Fatal("changed destination", err)
			}
		})
	}
}

func TestRestoreUnitsRoundDeadlineUpAndKeepBootAndStopRules(t *testing.T) {
	f := newRestoreFixture(t)
	r := f.record(t, waitingAfterChange, protocol.ConnectionSSH)
	units, err := f.server.changeUnits(r)
	if err != nil {
		t.Fatal(err)
	}
	timer := units[restoreTimerUnit(r.Outcome.OwedRestoreID)]
	for _, required := range []string{"OnCalendar=2030-01-02 03:05:06 UTC", "AccuracySec=1s", "DefaultDependencies=no", "Conflicts=shutdown.target", "WantedBy=timers.target"} {
		if !strings.Contains(timer, required) {
			t.Fatal(timer)
		}
	}
	if strings.Contains(timer, "Persistent=") {
		t.Fatal(timer)
	}
	restore := units[r.Outcome.RestoreServiceUnit]
	for _, required := range []string{"After=network.target", "WantedBy=multi-user.target", "TimeoutStartSec=330s", "NotifyAccess=main", "TimeoutStopSec=5s", "KillMode=control-group", "run-owed-restore --owed-restore-id " + r.Outcome.OwedRestoreID} {
		if !strings.Contains(restore, required) {
			t.Fatal(restore)
		}
	}
	if strings.Contains(restore, "DefaultDependencies=no") {
		t.Fatal(restore)
	}
	if err := f.server.armRestore(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	for name := range units {
		info, err := os.Stat(filepath.Join(f.server.systemdDirectory(), name))
		if err != nil || info.Mode().Perm() != 0644 {
			t.Fatal(info, err)
		}
	}
}

func TestRestoreAcceptsInactiveAndFailedButNeverRunningActivation(t *testing.T) {
	for _, state := range []string{"inactive", "failed", "active", "deactivating"} {
		t.Run(state, func(t *testing.T) {
			f := newRestoreFixture(t)
			r := f.record(t, waitingAfterChange, protocol.ConnectionSSH)
			f.stopWorker()
			f.activationState = state
			err := f.server.RunOwedRestore(context.Background(), r.Outcome.OwedRestoreID)
			allowed := state == "inactive" || state == "failed"
			if (err == nil) != allowed {
				t.Fatal(err)
			}
			if !allowed && f.adapter.calls.Load() != 0 {
				t.Fatal("restored beside live activation")
			}
		})
	}
}

func TestAcceptedChangeSurvivesCleanupFailureAndLateTimer(t *testing.T) {
	f := newRestoreFixture(t)
	r := f.record(t, waitingAfterChange, protocol.ConnectionSSH)
	if err := f.server.armRestore(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	if a := confirm(t, f.server, r, protocol.ConfirmationAfterChange, protocol.ConnectionSSH); !a.ConfirmationCounted {
		t.Fatal(a)
	}
	f.failCommand = "stop "
	if err := f.server.RunOwedRestore(context.Background(), r.Outcome.OwedRestoreID); err == nil {
		t.Fatal("cleanup should fail")
	}
	if got := readRestore(t, f.server, r.Outcome.OwedRestoreID); got.Phase != changeAccepted || got.CleanupComplete {
		t.Fatal(got)
	}
	f.failCommand = ""
	f.clock.advance(time.Hour)
	f.stopWorker()
	if err := f.server.RunOwedRestore(context.Background(), r.Outcome.OwedRestoreID); err != nil {
		t.Fatal(err)
	}
	if f.adapter.calls.Load() != 0 {
		t.Fatal("restored accepted change")
	}
}

func TestInterruptedJobAcquiresLaterRestoreOutcomeAndHistoryKeepsIt(t *testing.T) {
	f := newRestoreFixture(t)
	r := f.record(t, waitingAfterChange, protocol.ConnectionSSH)
	f.stopWorker()
	first, err := f.server.Status(f.jobID)
	if err != nil {
		t.Fatal(err)
	}
	if first.Status != "failed" || len(first.OwedRestores) != 1 || first.OwedRestores[0].RestoreStatus != "required" {
		t.Fatal(first)
	}
	finished := *first.Finished
	old := f.clock.read().Add(-nodeRetention - time.Hour)
	path := filepath.Join(f.server.jobDir(f.jobID), "result.json")
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	if err := f.server.expireJob(f.jobID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("expired unresolved job", err)
	}
	if err := f.server.RunOwedRestore(context.Background(), r.Outcome.OwedRestoreID); err != nil {
		t.Fatal(err)
	}
	later, err := f.server.Status(f.jobID)
	if err != nil {
		t.Fatal(err)
	}
	if later.Status != "failed" || !later.Finished.Equal(finished) || later.OwedRestores[0].RestoreStatus != "succeeded" {
		t.Fatal(later)
	}
	stored, err := f.server.storedResult(f.jobID)
	if err != nil || stored.OwedRestores[0].RestoreStatus != "succeeded" {
		t.Fatal(stored, err)
	}
}

func TestOwedRestoreRecordRejectsInvalidRecoveryState(t *testing.T) {
	for _, edit := range []func(map[string]any){
		func(m map[string]any) { m["unknown_field"] = true },
		func(m map[string]any) { m["node_job_id"] = "../../job" },
		func(m map[string]any) { m["change_phase"] = "future_phase" },
		func(m map[string]any) { m["previous_file_metadata"] = nil },
		func(m map[string]any) { m["owed_restore_cleanup_complete"] = true },
		func(m map[string]any) { m["connection_kinds_to_confirm"] = []string{"ssh", "ssh"} },
	} {
		f := newRestoreFixture(t)
		r := f.record(t, waitingAfterChange, protocol.ConnectionSSH)
		path := filepath.Join(f.server.restoreDirectory(r.Outcome.OwedRestoreID), "owed_restore.json")
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var fields map[string]any
		if err := json.Unmarshal(b, &fields); err != nil {
			t.Fatal(err)
		}
		edit(fields)
		if err := durable.JSON(path, fields); err != nil {
			t.Fatal(err)
		}
		if err := f.server.RunOwedRestore(context.Background(), r.Outcome.OwedRestoreID); err == nil {
			t.Fatal("accepted invalid recovery state", fields)
		}
		if f.adapter.calls.Load() != 0 {
			t.Fatal("ran native code from invalid record")
		}
	}
}

func TestRunnerReplacementRefusesRestoreUnitMarker(t *testing.T) {
	f := newRestoreFixture(t)
	marker := filepath.Join(f.server.systemdDirectory(), "mmx-owed-restore-0123456789abcdef.service")
	if err := os.WriteFile(marker, []byte("partial arming"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := f.server.refuseRunnerReplacement(); err == nil || !strings.Contains(err.Error(), filepath.Base(marker)) {
		t.Fatal(err)
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	if err := f.server.refuseRunnerReplacement(); err != nil {
		t.Fatal(err)
	}
}

func TestProgressWakesWhenOnlyConfirmationRecordChanges(t *testing.T) {
	f := newRestoreFixture(t)
	r := f.record(t, waitingBeforeChange, protocol.ConnectionSSH, protocol.ConnectionHTTPS)
	f.server.spacing = time.Millisecond
	f.server.progressWait = 30 * time.Second
	pending := r.pending(f.clock.read())
	done := make(chan protocol.Response, 1)
	fail := make(chan error, 1)
	go func() {
		response, err := f.server.Progress(protocol.ProgressRequest{ID: f.jobID, KnownReachabilityConfirmation: pending})
		done <- response
		fail <- err
	}()
	if a := confirm(t, f.server, r, protocol.ConfirmationBeforeChange, protocol.ConnectionSSH); !a.ConfirmationCounted {
		t.Fatal(a)
	}
	select {
	case response := <-done:
		if err := <-fail; err != nil {
			t.Fatal(err)
		}
		if response.PendingReachabilityConfirmation == nil || len(response.PendingReachabilityConfirmation.ConfirmedConnectionKinds) != 1 {
			t.Fatal(response)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("progress did not wake for confirmation")
	}
}

func TestActivationFailureNeverAdvertisesAfterChangeConfirmation(t *testing.T) {
	f := newRestoreFixture(t)
	r := f.record(t, activatingConfig, protocol.ConnectionSSH)
	entered, release := make(chan struct{}), make(chan struct{})
	f.adapter.activate = func(bool) error { close(entered); <-release; return errors.New("activation rejected") }
	done := make(chan error, 1)
	go func() { done <- f.server.ActivateChange(context.Background(), r.Outcome.OwedRestoreID) }()
	<-entered
	if a := confirm(t, f.server, r, protocol.ConfirmationAfterChange, protocol.ConnectionSSH); a.ConfirmationCounted {
		t.Fatal(a)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if a := confirm(t, f.server, r, protocol.ConfirmationAfterChange, protocol.ConnectionSSH); a.ConfirmationCounted {
		t.Fatal(a)
	}
	if got := readRestore(t, f.server, r.Outcome.OwedRestoreID); got.Phase != restorationRequired || !got.ChangeActivationFailed || got.Outcome.OriginalFailureReason != "activation rejected" {
		t.Fatal(got)
	}
}

func waitForRestorePhase(t *testing.T, s Server, phase string) owedRestoreRecord {
	t.Helper()
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
		var records []owedRestoreRecord
		if err := s.withOwedRestores(context.Background(), func() error { var err error; records, err = s.owedRestoreRecords(); return err }); err != nil {
			t.Fatal(err)
		}
		for _, r := range records {
			if r.Phase == phase {
				return r
			}
		}
	}
	t.Fatalf("did not reach phase %s", phase)
	return owedRestoreRecord{}
}

func TestWorkerInstallsOnlyAfterBeforeConfirmationAndLiveTimer(t *testing.T) {
	for _, elapsed := range []bool{false, true} {
		t.Run(fmt.Sprint(elapsed), func(t *testing.T) {
			f := newRestoreFixture(t)
			previous, proposed := "previous", "proposed"
			if err := os.WriteFile(f.destination, []byte(previous), 0600); err != nil {
				t.Fatal(err)
			}
			prepared, result := (modules.Runtime{CheckFileDestination: f.server.CheckChangeAllowed}).PrepareFileChange(context.Background(), modules.File{Path: f.destination, Literal: &proposed}, value.Scope{}, nil)
			if prepared == nil {
				t.Fatal(result)
			}
			defer prepared.Close()
			if elapsed {
				f.timerNext = ""
			}
			service := f.server.changeService(f.jobID, "configure", "file", []protocol.ConnectionKind{protocol.ConnectionSSH}, nil, modules.Runtime{})
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			done := make(chan protocol.Result, 1)
			go func() {
				done <- service.ApplyChange(ctx, modules.ChangeWithOwedRestore{PreparedFile: prepared, RestoreConfirmationWait: time.Minute, ModuleRestoreData: json.RawMessage(`{}`)})
			}()
			r := waitForRestorePhase(t, f.server, waitingBeforeChange)
			if contents, err := os.ReadFile(f.destination); err != nil || string(contents) != previous {
				t.Fatal("changed before confirmation", err)
			}
			if a := confirm(t, f.server, r, protocol.ConfirmationBeforeChange, protocol.ConnectionSSH); !a.ConfirmationCounted {
				t.Fatal(a)
			}
			if !elapsed {
				r = waitForRestorePhase(t, f.server, activatingConfig)
				if contents, err := os.ReadFile(f.destination); err != nil || string(contents) != proposed {
					t.Fatal("did not install before activation", err)
				}
				if err := f.server.ActivateChange(ctx, r.Outcome.OwedRestoreID); err != nil {
					t.Fatal(err)
				}
				if a := confirm(t, f.server, r, protocol.ConfirmationAfterChange, protocol.ConnectionSSH); !a.ConfirmationCounted {
					t.Fatal(a)
				}
			}
			result = <-done
			if elapsed {
				if result.Status != "failed" || result.Change != "unchanged" {
					t.Fatal(result)
				}
				if contents, err := os.ReadFile(f.destination); err != nil || string(contents) != previous {
					t.Fatal("installed with elapsed timer", err)
				}
			} else if result.Status != "succeeded" || result.Change != "changed" {
				t.Fatal(result)
			}
			got := readRestore(t, f.server, r.Outcome.OwedRestoreID)
			if !got.CleanupComplete {
				t.Fatal("cleanup not complete", got)
			}
		})
	}
}

func TestRestoreWaitUsesNodeWallClockAndExtendsOnlyBeforeAttempt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newRestoreFixture(t)
		r := f.record(t, waitingAfterChange, protocol.ConnectionSSH)
		var extensions atomic.Int64
		firstExtension := make(chan struct{})
		f.server.extendRestoreWait = func() error {
			if extensions.Add(1) == 1 {
				close(firstExtension)
			}
			return nil
		}
		// A slow native activation must not renew the attempt allowance.
		f.adapter.activate = func(bool) error {
			time.Sleep(time.Minute)
			return nil
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		defer cancel()
		f.clock.advance(-time.Hour)
		done := make(chan error, 1)
		go func() { done <- f.server.RunOwedRestore(ctx, r.Outcome.OwedRestoreID) }()
		select {
		case <-firstExtension:
		case <-ctx.Done():
			t.Fatal("restore service did not extend its wait")
		}
		synctest.Wait() // The service has observed the node clock and is waiting.
		select {
		case err := <-done:
			t.Fatal("clock setback ended wait", err)
		default:
		}
		f.clock.advance(time.Hour + time.Minute)
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		if extensions.Load() != 2 {
			t.Fatal("expected one wait extension and one attempt allowance", extensions.Load())
		}
	})
}

func TestNotificationFailureDoesNotPreventRestore(t *testing.T) {
	f := newRestoreFixture(t)
	r := f.record(t, waitingAfterChange, protocol.ConnectionSSH)
	f.server.extendRestoreWait = func() error { return fmt.Errorf("notification socket unavailable") }
	if err := f.server.RunOwedRestore(context.Background(), r.Outcome.OwedRestoreID); err != nil {
		t.Fatal(err)
	}
	got := readRestore(t, f.server, r.Outcome.OwedRestoreID)
	if got.Phase != configRestored || f.adapter.calls.Load() != 1 {
		t.Fatal(got)
	}
}

type advanceClockAfterSave struct {
	context.Context
	fixture *restoreFixture
	once    sync.Once
}

func (c *advanceClockAfterSave) Err() error {
	saved, _ := filepath.Glob(filepath.Join(c.fixture.server.Root, "owed_restores", "*", "previous_file_contents"))
	if len(saved) > 0 {
		c.once.Do(func() { c.fixture.clock.advance(armingAllowance + time.Second) })
	}
	return c.Context.Err()
}
func TestWorkerRefusesArmingAfterItsAllowance(t *testing.T) {
	f := newRestoreFixture(t)
	previous, proposed := "previous", "proposed"
	if err := os.WriteFile(f.destination, []byte(previous), 0600); err != nil {
		t.Fatal(err)
	}
	prepared, result := (modules.Runtime{}).PrepareFileChange(context.Background(), modules.File{Path: f.destination, Literal: &proposed}, value.Scope{}, nil)
	if prepared == nil {
		t.Fatal(result)
	}
	defer prepared.Close()
	service := f.server.changeService(f.jobID, "configure", "file", []protocol.ConnectionKind{protocol.ConnectionSSH}, nil, modules.Runtime{})
	bounded, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	ctx := &advanceClockAfterSave{Context: bounded, fixture: f}
	done := make(chan protocol.Result, 1)
	go func() {
		done <- service.ApplyChange(ctx, modules.ChangeWithOwedRestore{PreparedFile: prepared, RestoreConfirmationWait: time.Minute, ModuleRestoreData: json.RawMessage(`{}`)})
	}()
	r := waitForRestorePhase(t, f.server, waitingBeforeChange)
	if a := confirm(t, f.server, r, protocol.ConfirmationBeforeChange, protocol.ConnectionSSH); !a.ConfirmationCounted {
		t.Fatal(a)
	}
	result = <-done
	if result.Status != "failed" || result.Change != "unchanged" || !strings.Contains(result.Reason, "allowed setup time") {
		t.Fatal(result)
	}
	if contents, err := os.ReadFile(f.destination); err != nil || string(contents) != previous {
		t.Fatal("installed too late", err)
	}
}

// The controller process reads only restore_status, so every write derives it
// from the phase: a write that names only the phase still reports a required
// or failed restore.
func TestRestoreStatusFollowsThePhase(t *testing.T) {
	for phase, want := range map[string]string{
		waitingAfterChange:  "not_started",
		changeAccepted:      "not_needed",
		restorationRequired: "required",
		restoringConfig:     "running",
		restoreFailed:       "failed",
		configRestored:      "succeeded",
	} {
		f := newRestoreFixture(t)
		r := f.record(t, phase, protocol.ConnectionSSH)
		r.Outcome.RestoreStatus = "not_started"
		if phase == waitingAfterChange || phase == changeAccepted {
			r.Outcome.RestoreStatus = "failed"
		}
		if err := f.server.withOwedRestores(context.Background(), func() error { return f.server.writeOwedRestore(&r) }); err != nil {
			t.Fatal(phase, err)
		}
		if got := readRestore(t, f.server, r.Outcome.OwedRestoreID).Outcome.RestoreStatus; got != want {
			t.Errorf("%s: restore_status %q, want %q", phase, got, want)
		}
	}
}

func TestInterruptedRestoreIsReportedFailedWithoutRepeatingIt(t *testing.T) {
	f := newRestoreFixture(t)
	r := f.record(t, restoringConfig, protocol.ConnectionSSH)
	r.Outcome.PreviousFileRestored = true
	if err := f.server.withOwedRestores(context.Background(), func() error { return f.server.writeOwedRestore(&r) }); err != nil {
		t.Fatal(err)
	}
	response, err := f.server.Status(f.jobID)
	if err != nil {
		t.Fatal(err)
	}
	if len(response.OwedRestores) != 1 {
		t.Fatal(response)
	}
	outcome := response.OwedRestores[0]
	if outcome.RestoreStatus != "failed" || !outcome.PreviousFileRestored || !strings.Contains(outcome.RestoreFailureReason, "ended before recording completion") {
		t.Fatal(outcome)
	}
	if f.adapter.calls.Load() != 0 {
		t.Fatal("inspection repeated failed restore")
	}
}

func TestLiveRestoreIsNotMistakenForInterruptedAttempt(t *testing.T) {
	f := newRestoreFixture(t)
	r := f.record(t, restoringConfig, protocol.ConnectionSSH)
	if err := f.server.withOwedRestores(context.Background(), func() error { return f.server.writeOwedRestore(&r) }); err != nil {
		t.Fatal(err)
	}
	held, err := lock(filepath.Join(f.server.restoreDirectory(r.Outcome.OwedRestoreID), "restore_execution.lock"))
	if err != nil {
		t.Fatal(err)
	}
	defer unlock(held)
	response, err := f.server.Status(f.jobID)
	if err != nil {
		t.Fatal(err)
	}
	if len(response.OwedRestores) != 1 || response.OwedRestores[0].RestoreStatus != "running" {
		t.Fatal(response)
	}
}

func TestInspectionFinishesCleanupAfterUnitsAreAlreadyRemoved(t *testing.T) {
	f := newRestoreFixture(t)
	r := f.record(t, waitingAfterChange, protocol.ConnectionSSH)
	if a := confirm(t, f.server, r, protocol.ConfirmationAfterChange, protocol.ConnectionSSH); !a.ConfirmationCounted {
		t.Fatal(a)
	}
	if err := f.server.RunOwedRestore(context.Background(), r.Outcome.OwedRestoreID); err != nil {
		t.Fatal(err)
	}
	// Model a restart that lost the last cleanup marker after durable unit
	// removal. No unit remains that could arrange another cleanup invocation.
	r = readRestore(t, f.server, r.Outcome.OwedRestoreID)
	r.CleanupComplete = false
	if err := f.server.withOwedRestores(context.Background(), func() error { return f.server.writeOwedRestore(&r) }); err != nil {
		t.Fatal(err)
	}
	if _, err := f.server.Status(f.jobID); err != nil {
		t.Fatal(err)
	}
	if got := readRestore(t, f.server, r.Outcome.OwedRestoreID); !got.CleanupComplete || got.Phase != changeAccepted {
		t.Fatal(got)
	}
	if f.adapter.calls.Load() != 0 {
		t.Fatal("inspection restored accepted file")
	}
	if err := f.server.CheckChangeAllowed(context.Background(), f.destination); err != nil {
		t.Fatal(err)
	}
}

func TestExpiredConfirmationNamesOnlyMissingRequiredConnection(t *testing.T) {
	f := newRestoreFixture(t)
	r := f.record(t, waitingAfterChange, protocol.ConnectionSSH, protocol.ConnectionHTTPS)
	if answer := confirm(t, f.server, r, protocol.ConfirmationAfterChange, protocol.ConnectionSSH); !answer.ConfirmationCounted {
		t.Fatal(answer)
	}
	f.clock.advance(time.Minute)
	if answer := confirm(t, f.server, r, protocol.ConfirmationAfterChange, protocol.ConnectionHTTPS); answer.ConfirmationCounted {
		t.Fatal(answer)
	}
	got := readRestore(t, f.server, r.Outcome.OwedRestoreID)
	reason := got.Outcome.OriginalFailureReason
	if !strings.Contains(reason, "HTTPS") || strings.Contains(reason, "SSH") || !strings.Contains(reason, "within 1m.") {
		t.Fatal(reason)
	}
}

func TestRestoredTaskStillFailsAndExplainsPreviousPresenceOrAbsence(t *testing.T) {
	for _, existed := range []bool{false, true} {
		record := owedRestoreRecord{Phase: configRestored, Change: "changed", PreviousFile: &modules.PreviousFileMetadata{Existed: existed}, Outcome: protocol.OwedRestore{DestinationFile: "/etc/example.conf", RestoreStatus: "succeeded", OriginalFailureReason: "reachability confirmation failed"}}
		result := changeResult(record, protocol.Success("changed"), nil)
		if result.Status != "failed" || !result.NoOtherTry || !strings.Contains(result.Reason, "/etc/example.conf") || !strings.Contains(result.Reason, "configuration is restored.") && !strings.Contains(result.Reason, "configuration are restored.") {
			t.Fatal(result)
		}
		if strings.Contains(result.Reason, "virtual devices") == existed {
			t.Fatal(result.Reason)
		}
	}
}

func TestWorkerBoundsWaitWhenRestoreServiceNeverRecordsCompletion(t *testing.T) {
	f := newRestoreFixture(t)
	f.server.restoreWait = 80 * time.Millisecond
	previous, proposed := "previous", "proposed"
	if err := os.WriteFile(f.destination, []byte(previous), 0600); err != nil {
		t.Fatal(err)
	}
	prepared, result := (modules.Runtime{}).PrepareFileChange(context.Background(), modules.File{Path: f.destination, Literal: &proposed}, value.Scope{}, nil)
	if prepared == nil {
		t.Fatal(result)
	}
	defer prepared.Close()
	service := f.server.changeService(f.jobID, "configure", "file", []protocol.ConnectionKind{protocol.ConnectionSSH}, nil, modules.Runtime{})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	done := make(chan protocol.Result, 1)
	go func() {
		done <- service.ApplyChange(ctx, modules.ChangeWithOwedRestore{PreparedFile: prepared, RestoreConfirmationWait: time.Minute, ModuleRestoreData: json.RawMessage(`{}`)})
	}()
	r := waitForRestorePhase(t, f.server, waitingBeforeChange)
	confirm(t, f.server, r, protocol.ConfirmationBeforeChange, protocol.ConnectionSSH)
	r = waitForRestorePhase(t, f.server, activatingConfig)
	if err := f.server.ActivateChange(ctx, r.Outcome.OwedRestoreID); err != nil {
		t.Fatal(err)
	}
	f.clock.advance(time.Minute)
	// systemctl acknowledges the start, but the service never publishes any
	// restore decision or completion. The worker's caller still has time left.
	select {
	case result = <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("worker exceeded its restore-service allowance")
	}
	if result.Status != "failed" || !result.NoOtherTry || !strings.Contains(result.Reason, r.Outcome.RestoreServiceUnit) || !strings.Contains(result.Reason, "did not record completion") {
		t.Fatal(result)
	}
	if ctx.Err() != nil || f.adapter.calls.Load() != 1 {
		t.Fatal("caller timed out or worker repeated native activation", ctx.Err(), f.adapter.calls.Load())
	}
	got := readRestore(t, f.server, r.Outcome.OwedRestoreID)
	if got.Phase != restorationRequired || got.CleanupComplete || got.Outcome.RestoreStatus != "required" {
		t.Fatal(got)
	}
	saved := filepath.Join(f.server.restoreDirectory(r.Outcome.OwedRestoreID), "previous_file_contents")
	if contents, err := os.ReadFile(saved); err != nil || string(contents) != previous {
		t.Fatal("lost saved previous file", string(contents), err)
	}
	if contents, err := os.ReadFile(f.destination); err != nil || string(contents) != proposed {
		t.Fatal("worker restored the file itself", string(contents), err)
	}
	f.systemMu.Lock()
	defer f.systemMu.Unlock()
	starts := 0
	for _, command := range f.commands {
		if command == "start --no-block "+r.Outcome.RestoreServiceUnit {
			starts++
		}
	}
	if starts != 1 {
		t.Fatalf("requested restore %d times, want one", starts)
	}
}

func TestUnpublishedRestoreDebrisDoesNotBlockOtherDestinations(t *testing.T) {
	for _, leftover := range []string{"empty", "temporary record", "previous_file_contents", "restore_execution.lock", "unknown", "temporary directory", "temporary symlink", "public temporary file", "restore service", "restore timer", "activation service", "enabled restore service", "enabled restore timer"} {
		t.Run(leftover, func(t *testing.T) {
			f := newRestoreFixture(t)
			f.record(t, waitingAfterChange, protocol.ConnectionSSH)
			id := "0123456789abcdef"
			dir := f.server.restoreDirectory(id)
			if err := os.Mkdir(dir, 0700); err != nil {
				t.Fatal(err)
			}
			if leftover != "empty" {
				file, err := os.CreateTemp(dir, ".mmx-")
				if err != nil {
					t.Fatal(err)
				}
				if _, err := file.WriteString(`{"owed_restore":`); err != nil {
					t.Fatal(err)
				}
				if err := file.Close(); err != nil {
					t.Fatal(err)
				}
				switch leftover {
				case "temporary directory", "temporary symlink":
					if err := os.Remove(file.Name()); err != nil {
						t.Fatal(err)
					}
					if leftover == "temporary directory" {
						err = os.Mkdir(file.Name(), 0700)
					} else {
						err = os.Symlink("missing", file.Name())
					}
				case "public temporary file":
					err = os.Chmod(file.Name(), 0644)
				case "previous_file_contents", "restore_execution.lock", "unknown":
					err = os.WriteFile(filepath.Join(dir, leftover), []byte("preserve"), 0600)
				case "restore service", "restore timer", "activation service", "enabled restore service", "enabled restore timer":
					unit := map[string]string{
						"restore service": restoreServiceUnit(id), "restore timer": restoreTimerUnit(id), "activation service": activationServiceUnit(id),
						"enabled restore service": filepath.Join("multi-user.target.wants", restoreServiceUnit(id)), "enabled restore timer": filepath.Join("timers.target.wants", restoreTimerUnit(id)),
					}[leftover]
					err = os.WriteFile(filepath.Join(f.server.systemdDirectory(), unit), []byte("partial arming"), 0644)
				}
				if err != nil {
					t.Fatal(err)
				}
			}
			allowed := leftover == "empty" || leftover == "temporary record"
			err := f.server.CheckChangeAllowed(context.Background(), filepath.Join(filepath.Dir(f.destination), "unrelated.conf"))
			if (err == nil) != allowed {
				t.Fatalf("unrelated destination: %v; safe unpublished directory: %v", err, allowed)
			}
			if allowed {
				if err := f.server.CheckChangeAllowed(context.Background(), f.destination); err == nil || !strings.Contains(err.Error(), "still owes a restore") {
					t.Fatal("lost the published reservation", err)
				}
			}
		})
	}
}

func TestRestoreActivationRequiresSuccessfulAcceptanceCheck(t *testing.T) {
	f := newRestoreFixture(t)
	r := f.record(t, waitingAfterChange, protocol.ConnectionSSH)
	f.stopWorker()
	f.adapter.check = func(restoring bool) error {
		if !restoring || f.adapter.calls.Load() != 1 {
			t.Fatal("check did not follow successful restore activation")
		}
		if got := readRestore(t, f.server, r.Outcome.OwedRestoreID); got.Outcome.PreviousConfigActivated {
			t.Fatal("reported activation before its check", got)
		}
		return errors.New("native tool ignored the previous configuration")
	}
	if err := f.server.RunOwedRestore(context.Background(), r.Outcome.OwedRestoreID); err == nil {
		t.Fatal("accepted rejected configuration")
	}
	got := readRestore(t, f.server, r.Outcome.OwedRestoreID)
	if got.Phase != restoreFailed || !got.Outcome.PreviousFileRestored || !got.Outcome.ObsoleteVirtualDevicesRemoved || got.Outcome.PreviousConfigActivated || !strings.Contains(got.Outcome.RestoreFailureReason, "native tool ignored") {
		t.Fatal(got)
	}
}

func TestInstallRefusesRestoreUnitBeforeReplacingRunner(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("Server.Install requires root; do not bypass its initialization checks")
	}
	f := newRestoreFixture(t)
	marker := filepath.Join(f.server.systemdDirectory(), "mmx-owed-restore-0123456789abcdef.service")
	if err := os.WriteFile(marker, []byte("partial arming"), 0644); err != nil {
		t.Fatal(err)
	}
	called := false
	replace := func() error { called = true; return nil }
	if err := f.server.Install(replace); err == nil || !strings.Contains(err.Error(), filepath.Base(marker)) || called {
		t.Fatal("installation did not refuse owed restore", err, called)
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	if err := f.server.Install(replace); err != nil || !called {
		t.Fatal("resolved restore still blocks installation", err, called)
	}
}

func TestFailedDurableDecisionDoesNotAcknowledgeOrRunRestore(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission fault injection requires a non-root process")
	}
	for _, decision := range []string{"accept", "restore"} {
		for _, fault := range []string{"temporary write", "directory sync"} {
			t.Run(decision+"/"+fault, func(t *testing.T) {
				f := newRestoreFixture(t)
				r := f.record(t, waitingAfterChange, protocol.ConnectionSSH)
				if err := os.WriteFile(f.destination, []byte("proposed"), 0600); err != nil {
					t.Fatal(err)
				}
				dir := f.server.restoreDirectory(r.Outcome.OwedRestoreID)
				t.Cleanup(func() { _ = os.Chmod(dir, 0700) })
				if decision == "restore" {
					f.clock.advance(time.Minute)
				}
				// The record has been read when the decision checks the node clock.
				// 0500 refuses the temporary write; 0300 permits write and rename,
				// but refuses opening the directory for the final durability sync.
				f.server.now = func() time.Time {
					mode := os.FileMode(0500)
					if fault == "directory sync" {
						mode = 0300
					}
					if err := os.Chmod(dir, mode); err != nil {
						t.Fatal(err)
					}
					return f.clock.read()
				}
				var err error
				if decision == "accept" {
					var answer protocol.ReachabilityConfirmationResponse
					answer, err = f.server.ConfirmReachability(context.Background(), protocol.ReachabilityConfirmationRequest{NodeJobID: r.NodeJobID, OwedRestoreID: r.Outcome.OwedRestoreID, ConfirmationPhase: protocol.ConfirmationAfterChange}, protocol.ConnectionSSH)
					if answer.ConfirmationCounted {
						t.Fatal("acknowledged a failed durable write", answer)
					}
				} else {
					err = f.server.RunOwedRestore(context.Background(), r.Outcome.OwedRestoreID)
				}
				if !os.IsPermission(err) {
					t.Fatal("expected filesystem permission failure", err)
				}
				if err := os.Chmod(dir, 0700); err != nil {
					t.Fatal(err)
				}
				f.server.now = f.clock.read
				if contents, err := os.ReadFile(f.destination); err != nil || string(contents) != "proposed" || f.adapter.calls.Load() != 0 {
					t.Fatal("restored before durable decision", string(contents), err, f.adapter.calls.Load())
				}
				got := readRestore(t, f.server, r.Outcome.OwedRestoreID)
				wantPhase := waitingAfterChange
				if fault == "directory sync" {
					wantPhase = changeAccepted
					if decision == "restore" {
						wantPhase = restoringConfig
					}
				}
				if got.Phase != wantPhase || got.CleanupComplete {
					t.Fatal(got)
				}
				if decision == "accept" {
					if answer := confirm(t, f.server, r, protocol.ConfirmationAfterChange, protocol.ConnectionSSH); !answer.ConfirmationCounted {
						t.Fatal(answer)
					}
				}
				// An explicit repeated invocation settles the durable decision.
				if err := f.server.RunOwedRestore(context.Background(), r.Outcome.OwedRestoreID); err != nil {
					t.Fatal(err)
				}
				got = readRestore(t, f.server, r.Outcome.OwedRestoreID)
				if !got.CleanupComplete || (decision == "accept" && got.Phase != changeAccepted) || (decision == "restore" && got.Phase != configRestored) {
					t.Fatal(got)
				}
			})
		}
	}
}

func TestTaskDeadlineDoesNotConsumeIndependentRestoreAllowance(t *testing.T) {
	f := newRestoreFixture(t)
	r := f.record(t, restorationRequired, protocol.ConnectionSSH)
	f.server.restoreWait = time.Second
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	_, err := f.server.waitForChange(ctx, r.Outcome.OwedRestoreID, true)
	if !errors.Is(err, context.DeadlineExceeded) || errors.Is(err, errRestoreServiceWaitExpired) {
		t.Fatal("task deadline skipped the caller's recovery wait", err)
	}
}

func TestNetworkFileResultSentencesUseTheRecordedFailureKind(t *testing.T) {
	const path = "/etc/systemd/network/20-second.network"
	for _, test := range []struct {
		name                        string
		activation, existed, failed bool
		want                        string
	}{
		{"activation", true, true, false, "systemd-networkd did not accept " + path + ": Unknown key. The previous file and its running configuration are restored."},
		{"expired", false, true, false, "No reachability confirmation arrived over a new SSH connection within 2m. The previous file " + path + " and its running configuration are restored."},
		{"new file", false, false, false, "No reachability confirmation arrived over a new SSH connection within 2m. The new file " + path + " and its declared virtual devices were removed; the previous configuration is restored."},
		{"restore failed without a previous file", false, false, true, "No reachability confirmation arrived over a new SSH connection within 2m. Restoring " + path + " failed: networkctl exited with 1. The owed restore remains; the previous running configuration is not confirmed restored."},
		{"restore failed", false, true, true, "No reachability confirmation arrived over a new SSH connection within 2m. Restoring " + path + " failed: networkctl exited with 1. The owed restore and saved previous file remain; the previous running configuration is not confirmed restored."},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := owedRestoreRecord{Phase: configRestored, Change: "changed", ChangeActivationFailed: test.activation, PreviousFile: &modules.PreviousFileMetadata{Existed: test.existed}, Outcome: protocol.OwedRestore{DestinationFile: path, RestoreStatus: "succeeded", OriginalFailureReason: "No reachability confirmation arrived over a new SSH connection within 2m."}}
			if test.activation {
				r.Outcome.OriginalFailureReason = "systemd-networkd did not accept " + path + ": Unknown key."
			}
			if test.failed {
				r.Phase = restoreFailed
				r.Outcome.RestoreStatus, r.Outcome.RestoreFailureReason = "failed", "networkctl exited with 1"
			}
			result := changeResult(r, protocol.Success("changed"), nil)
			if result.Reason != test.want || result.Status != "failed" || result.Change != "changed" || !result.NoOtherTry {
				t.Fatal(result)
			}
		})
	}
}

func TestNetworkdNativeActivationFailureSuppressesSecretsInResultAndSavedRecord(t *testing.T) {
	for _, secret := range []bool{false, true} {
		t.Run(fmt.Sprint(secret), func(t *testing.T) {
			f := newRestoreFixture(t)
			// Use the production registry and adapter. Only the native executables
			// are stand-ins; no network or managed file on this machine is changed.
			f.server.activationFactory = nil
			f.destination = "/etc/systemd/network/99-mmx-secret-test.network"
			r := f.record(t, activatingConfig, protocol.ConnectionSSH)
			r.Outcome.ModuleName, r.Outcome.Secret = "systemd_networkd_file", secret
			if err := f.server.writeOwedRestore(&r); err != nil {
				t.Fatal(err)
			}
			directory := t.TempDir()
			const diagnostic = "native-diagnostic-secret-sentinel"
			for name, contents := range map[string]string{
				"journalctl": "#!/bin/sh\necho '-- cursor: before-reload'\n",
				"networkctl": "#!/bin/sh\n[ \"$*\" = reload ] || exit 90\necho '" + diagnostic + "' >&2\nexit 7\n",
			} {
				writeProgramFileThroughShellChild(t, filepath.Join(directory, name), contents)
			}
			t.Setenv("PATH", directory)
			if err := f.server.ActivateChange(context.Background(), r.Outcome.OwedRestoreID); err != nil {
				t.Fatal(err)
			}
			got := readRestore(t, f.server, r.Outcome.OwedRestoreID)
			if got.Phase != restorationRequired || !got.ChangeActivationFailed || got.Outcome.Secret != secret {
				t.Fatal(got)
			}
			result := changeResult(got, protocol.Success("changed"), nil)
			encoded, err := json.Marshal(result)
			if err != nil {
				t.Fatal(err)
			}
			saved, err := os.ReadFile(filepath.Join(f.server.restoreDirectory(r.Outcome.OwedRestoreID), "owed_restore.json"))
			if err != nil {
				t.Fatal(err)
			}
			for _, output := range []string{string(encoded), string(saved)} {
				if strings.Contains(output, diagnostic) == secret {
					t.Fatalf("secret=%t: diagnostic visibility is wrong: %s", secret, output)
				}
			}
			if !secret && !strings.Contains(got.Outcome.OriginalFailureReason, "systemd-networkd did not accept "+f.destination) {
				t.Fatal(got)
			}
		})
	}
}
