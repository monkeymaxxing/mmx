package node

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"mmx/internal/durable"
	"mmx/internal/modules"
	"mmx/internal/protocol"
)

func (s Server) systemdDirectory() string {
	if s.unitDirectory != "" {
		return s.unitDirectory
	}
	return "/etc/systemd/system"
}
func (s Server) systemControl(ctx context.Context, args ...string) (string, error) {
	if s.systemctl != nil {
		return s.systemctl(ctx, args...)
	}
	cmd := exec.CommandContext(ctx, "systemctl", args...)
	cmd.Env = workerEnv
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("systemctl %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

// Runner binaries are installed at immutable absolute paths. Quote systemd's
// argument syntax, including its percent and dollar expansion, not shell syntax.
func runnerUnitArgument(path string) (string, error) {
	if !filepath.IsAbs(path) || strings.ContainsAny(path, "\x00\n\r") {
		return "", fmt.Errorf("change service requires an absolute node runner executable path without line breaks")
	}
	path = strings.NewReplacer("\\", "\\\\", "\"", "\\\"", "%", "%%", "$", "$$").Replace(path)
	return "\"" + path + "\"", nil
}
func (s Server) changeUnits(r owedRestoreRecord) (map[string]string, error) {
	binary, err := runnerUnitArgument(s.Binary)
	if err != nil {
		return nil, err
	}
	id := r.Outcome.OwedRestoreID
	after := "network.target"
	if r.RestoreAfterUnit != "" {
		// Ordering alone neither starts nor enables the module's service.
		after += " " + r.RestoreAfterUnit
	}
	deadline := r.RestoreDeadline.Truncate(time.Second)
	if deadline.Before(r.RestoreDeadline) {
		deadline = deadline.Add(time.Second)
	}
	service := fmt.Sprintf(`[Unit]
Description=MMX owed restore for change %s
After=%s

[Service]
Type=oneshot
User=root
Group=root
UMask=0077
ExecStart=%s run-owed-restore --owed-restore-id %s
NotifyAccess=main
TimeoutStartSec=330s
TimeoutStopSec=5s
KillMode=control-group
SendSIGKILL=yes
Restart=no
StandardInput=null

[Install]
WantedBy=multi-user.target
`, id, after, binary, id)
	timer := fmt.Sprintf(`[Unit]
Description=MMX restore deadline for change %s
DefaultDependencies=no
Before=timers.target shutdown.target
Conflicts=shutdown.target

[Timer]
OnCalendar=%s
AccuracySec=1s
RandomizedDelaySec=0
Unit=%s

[Install]
WantedBy=timers.target
`, id, deadline.UTC().Format("2006-01-02 15:04:05 UTC"), restoreServiceUnit(id))
	activation := fmt.Sprintf(`[Unit]
Description=MMX configuration activation for change %s
After=%s

[Service]
Type=oneshot
User=root
Group=root
UMask=0077
ExecStart=%s activate-change --owed-restore-id %s
TimeoutStartSec=infinity
TimeoutStopSec=5s
KillMode=control-group
SendSIGKILL=yes
Restart=no
StandardInput=null
`, id, after, binary, id)
	return map[string]string{restoreServiceUnit(id): service, restoreTimerUnit(id): timer, activationServiceUnit(id): activation}, nil
}

// armRestore returns only after units and enablement are durable, and a live
// absolute timer still has a next firing. systemctl enable --now alone is NOT
// evidence of that: systemd accepts an already elapsed OnCalendar time.
func (s Server) armRestore(ctx context.Context, r owedRestoreRecord) error {
	units, err := s.changeUnits(r)
	if err != nil {
		return err
	}
	id := r.Outcome.OwedRestoreID
	for _, name := range []string{restoreServiceUnit(id), activationServiceUnit(id), restoreTimerUnit(id)} {
		if err := durable.Write(filepath.Join(s.systemdDirectory(), name), []byte(units[name]), 0644); err != nil {
			return err
		}
	}
	if _, err := s.systemControl(ctx, "daemon-reload"); err != nil {
		return err
	}
	if _, err := s.systemControl(ctx, "enable", restoreServiceUnit(id), restoreTimerUnit(id)); err != nil {
		return err
	}
	for _, directory := range []string{"multi-user.target.wants", "timers.target.wants", "."} {
		if err := syncDirectory(filepath.Join(s.systemdDirectory(), directory)); err != nil {
			return err
		}
	}
	if _, err := s.systemControl(ctx, "start", restoreTimerUnit(id)); err != nil {
		return err
	}
	for _, name := range []string{restoreServiceUnit(id), restoreTimerUnit(id)} {
		enabled, err := s.systemControl(ctx, "is-enabled", name)
		if err != nil {
			return err
		}
		if enabled != "enabled" {
			return fmt.Errorf("owed restore unit is not enabled: %s", name)
		}
	}
	for _, name := range []string{restoreServiceUnit(id), activationServiceUnit(id)} {
		loaded, err := s.systemControl(ctx, "show", name, "--property=LoadState", "--value")
		if err != nil {
			return err
		}
		if loaded != "loaded" {
			return fmt.Errorf("Change activation or owed restore service is not loaded: %s", name)
		}
	}
	state, err := s.systemControl(ctx, "show", restoreTimerUnit(id), "--property=LoadState", "--property=ActiveState", "--property=NextElapseUSecRealtime")
	if err != nil {
		return err
	}
	properties := systemdProperties(state)
	if properties["LoadState"] != "loaded" || properties["ActiveState"] != "active" || properties["NextElapseUSecRealtime"] == "" {
		return fmt.Errorf("owed restore timer %s is not loaded, is not active, or has no future restore scheduled; the proposed change was not applied", restoreTimerUnit(id))
	}
	if !s.timeNow().Before(r.RestoreDeadline) {
		return fmt.Errorf("owed restore deadline passed while arming %s; the proposed change was not applied", restoreTimerUnit(id))
	}
	return nil
}
func systemdProperties(text string) map[string]string {
	result := map[string]string{}
	for _, line := range strings.Split(text, "\n") {
		key, value, ok := strings.Cut(line, "=")
		if ok {
			result[key] = value
		}
	}
	return result
}
func (s Server) startRestore(ctx context.Context, id string) error {
	_, err := s.systemControl(ctx, "start", "--no-block", restoreServiceUnit(id))
	return err
}

// Only a live wait extends systemd's timeout. A restore attempt never does.
// systemd accepts EXTEND_TIMEOUT_USEC from the main process of a oneshot
// service with NotifyAccess=main. Direct administrator invocations have no
// NOTIFY_SOCKET and use the same in-process restore budget.
func (s Server) extendRestoreServiceWait() error {
	if s.extendRestoreWait != nil {
		return s.extendRestoreWait()
	}
	socket := os.Getenv("NOTIFY_SOCKET")
	if socket == "" {
		return nil
	}
	connection, err := net.DialUnix("unixgram", nil, &net.UnixAddr{Name: socket, Net: "unixgram"})
	if err != nil {
		return fmt.Errorf("extending the owed restore service wait: %w", err)
	}
	defer connection.Close()
	if err := connection.SetWriteDeadline(time.Now().Add(time.Second)); err != nil {
		return err
	}
	_, err = connection.Write([]byte("EXTEND_TIMEOUT_USEC=330000000"))
	return err
}

// RunOwedRestore is the ONLY restore path. The lock order is restore_execution,
// short owed_restores sections, then (with owed_restores released) stop the
// activation service and acquire change_activation. It never needs the
// worker's execution.lock or admission.lock.
func (s Server) RunOwedRestore(ctx context.Context, id string) error {
	if err := protocol.ValidateOwedRestoreID(id); err != nil {
		return err
	}
	if _, err := os.Stat(s.restoreDirectory(id)); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	held, err := lockContext(ctx, filepath.Join(s.restoreDirectory(id), "restore_execution.lock"))
	if err != nil {
		return err
	}
	defer unlock(held)
	var record owedRestoreRecord
	var nextExtension time.Time
	for {
		err = s.withOwedRestores(ctx, func() error {
			var err error
			record, err = s.readOwedRestore(id)
			if err != nil {
				return err
			}
			if record.resolved() {
				return nil
			}
			running, err := s.changeWorkerRunning(record)
			if err != nil {
				return err
			}
			expired := !record.RestoreDeadline.IsZero() && !s.timeNow().Before(record.RestoreDeadline)
			required := record.Phase == restorationRequired || record.Phase == restoringConfig || record.Phase == restoreFailed
			if !running || expired || required {
				if !record.ChangeStarted {
					return s.requireRestore(&record, "The change ended before activation.")
				}
				if record.Outcome.OriginalFailureReason == "" {
					record.Outcome.OriginalFailureReason = missingReachabilityReason(record)
					if !running {
						record.Outcome.OriginalFailureReason = "node job worker process ended before reachability was confirmed"
					}
				}
				record.Phase = restoringConfig
				record.Outcome.RestoreFailureReason = ""
				record.Outcome.PreviousFileRestored, record.Outcome.PreviousConfigActivated, record.Outcome.ObsoleteVirtualDevicesRemoved = false, false, false
				record.Outcome.PreviousMembershipRestored = false
				return s.writeOwedRestore(&record)
			}
			return nil
		})
		if err != nil {
			return err
		}
		if record.resolved() {
			// Persist again before destroying recovery data: a prior writer may have
			// renamed the accepted record but failed the directory fsync.
			if !record.CleanupComplete {
				if err := s.withOwedRestores(ctx, func() error { return s.writeOwedRestore(&record) }); err != nil {
					return err
				}
			}
			return s.cleanupOwedRestore(ctx, &record)
		}
		if record.Phase == restoringConfig {
			break
		}
		// A one-shot timer that fired early never fires again. Stay alive and
		// re-evaluate the NODE'S wall clock and durable decision until resolved.
		if time.Now().Before(nextExtension) {
			if err := waitForRecord(ctx); err != nil {
				return err
			}
			continue
		}
		nextExtension = time.Now().Add(30 * time.Second)
		if err := s.extendRestoreServiceWait(); err != nil {
			if writeErr := s.withOwedRestores(ctx, func() error {
				current, readErr := s.readOwedRestore(id)
				if readErr != nil {
					return readErr
				}
				return s.requireRestore(&current, err.Error())
			}); writeErr != nil {
				return errors.Join(err, writeErr)
			}
		}
		if err := waitForRecord(ctx); err != nil {
			return err
		}
	}
	// Reset the service allowance once at the start of work, then stop extending.
	// The command budget below includes stopping activation and taking its lock.
	budget := restoreAttemptLimit
	if err := s.extendRestoreServiceWait(); err != nil {
		// A notification failure must not prevent restoration. The previous
		// extension is at most 30 seconds old; leave bookkeeping time before
		// its 330-second limit, even if this final extension cannot be sent.
		budget -= 5 * time.Second
	}
	attempt, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	if err := s.performRestore(attempt, &record); err != nil {
		return s.restoreAttemptFailed(record, err)
	}
	if err := s.withOwedRestores(attempt, func() error {
		record.Phase = configRestored

		return s.writeOwedRestore(&record)
	}); err != nil {
		return s.restoreAttemptFailed(record, err)
	}
	return s.cleanupOwedRestore(attempt, &record)
}

func (s Server) performRestore(ctx context.Context, record *owedRestoreRecord) error {
	id := record.Outcome.OwedRestoreID
	if _, err := s.systemControl(ctx, "stop", activationServiceUnit(id)); err != nil {
		return err
	}
	state, err := s.systemControl(ctx, "show", activationServiceUnit(id), "--property=ActiveState", "--value")
	if err != nil {
		return err
	}
	if state != "inactive" && state != "failed" {
		return fmt.Errorf("Cannot restore %s: change activation service %s did not stop (state %s); the saved previous state and owed restore remain.", record.subject(), activationServiceUnit(id), state)
	}
	activation, err := lockContext(ctx, filepath.Join(s.Root, "change_activation.lock"))
	if err != nil {
		return err
	}
	defer unlock(activation)
	if record.Outcome.DestinationFile != "" {
		if err := modules.RemoveFileChangeCandidates(record.Outcome.DestinationFile); err != nil {
			return err
		}
	}
	adapter, err := s.newChangeActivation(*record)
	if err != nil {
		return err
	}
	s.handSavedFiles(adapter, *record)
	runtime := modules.Runtime{PrivateTaskDirectoryRoot: s.privateTaskDirectoryRoot}
	if record.Outcome.DestinationFile != "" {
		result := runtime.RestorePreviousFile(ctx, record.Outcome.DestinationFile, filepath.Join(s.restoreDirectory(id), "previous_file_contents"), *record.PreviousFile)
		if result.Status != "succeeded" {
			return errors.New(result.Reason)
		}
		record.Outcome.PreviousFileRestored = true
		if err := s.saveRestoreProgress(ctx, record); err != nil {
			return err
		}
	}
	if err := adapter.PrepareActivation(ctx, runtime, true); err != nil {
		return err
	}
	if err := adapter.Activate(ctx, runtime, true); err != nil {
		return err
	}
	if err := adapter.RemoveObsoleteVirtualDevices(ctx, runtime, true); err != nil {
		return err
	}
	record.Outcome.ObsoleteVirtualDevicesRemoved = record.Outcome.DestinationFile != ""
	if err := s.saveRestoreProgress(ctx, record); err != nil {
		return err
	}
	if err := adapter.CheckActivatedConfiguration(ctx, runtime, true); err != nil {
		return err
	}
	if record.Outcome.DestinationFile != "" {
		record.Outcome.PreviousConfigActivated = true
	} else {
		record.Outcome.PreviousMembershipRestored = true
	}
	return s.saveRestoreProgress(ctx, record)
}
func (s Server) saveRestoreProgress(ctx context.Context, record *owedRestoreRecord) error {
	return s.withOwedRestores(ctx, func() error { return s.writeOwedRestore(record) })
}
func (s Server) restoreAttemptFailed(record owedRestoreRecord, cause error) error {
	// Saving the failure has a separate small budget when the native work's
	// context expired. The external service still has its 330-second bound.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := s.withOwedRestores(ctx, func() error {
		record.Phase = restoreFailed
		record.Outcome.RestoreFailureReason = cause.Error()
		return s.writeOwedRestore(&record)
	})
	return errors.Join(fmt.Errorf("owed restore %s failed: %s; correct the cause and run systemctl start %s", record.Outcome.OwedRestoreID, record.Outcome.Safe().RestoreFailureReason, record.Outcome.RestoreServiceUnit), err)
}

// Called only by the restore_execution holder, after a durable terminal
// decision. CleanupComplete prevents a late service invocation from deleting
// candidates belonging to a later change of the same destination.
func (s Server) cleanupOwedRestore(ctx context.Context, r *owedRestoreRecord) error {
	if r.CleanupComplete {
		return nil
	}
	if !r.resolved() {
		return fmt.Errorf("cannot clean up an unresolved owed restore")
	}
	// A crash can lose the last cleanup-complete write after unit removal.
	// Later inspection may then retry cleanup beside a newer restore of the
	// same destination. Never remove that restore process's live candidate.
	activation, err := lockContext(ctx, filepath.Join(s.Root, "change_activation.lock"))
	if err != nil {
		return err
	}
	defer unlock(activation)
	id := r.Outcome.OwedRestoreID
	// Some failures precede unit creation. Explicitly remove only our known
	// enablement links; absence is already clean and needs no systemctl call.
	timerPath := filepath.Join(s.systemdDirectory(), restoreTimerUnit(id))
	if _, err := os.Lstat(timerPath); err == nil {
		if _, err := s.systemControl(ctx, "stop", restoreTimerUnit(id)); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	for _, entry := range []struct{ directory, name string }{
		{"timers.target.wants", restoreTimerUnit(id)}, {"multi-user.target.wants", restoreServiceUnit(id)},
	} {
		path := filepath.Join(s.systemdDirectory(), entry.directory, entry.name)
		if err := removeAndSync(path); err != nil {
			return err
		}
	}
	if r.Outcome.DestinationFile != "" {
		if err := modules.RemoveFileChangeCandidates(r.Outcome.DestinationFile); err != nil {
			return err
		}
	}
	for _, saved := range []string{"previous_file_contents", "proposed_file_contents"} {
		if err := removeAndSync(filepath.Join(s.restoreDirectory(id), saved)); err != nil {
			return err
		}
	}
	hadUnits := false
	for _, name := range []string{restoreTimerUnit(id), activationServiceUnit(id), restoreServiceUnit(id)} {
		path := filepath.Join(s.systemdDirectory(), name)
		if _, err := os.Lstat(path); err == nil {
			hadUnits = true
		}
		if err := removeAndSync(path); err != nil {
			return err
		}
	}
	// Also reload on a repeated cleanup after an interrupted reload. Without
	// unit files this is harmless, but the initial pre-arming failure needs none.
	if hadUnits || !r.RestoreDeadline.IsZero() {
		if _, err := s.systemControl(ctx, "daemon-reload"); err != nil {
			return err
		}
	}
	return s.withOwedRestores(ctx, func() error { r.CleanupComplete = true; return s.writeOwedRestore(r) })
}
func removeAndSync(path string) error {
	err := os.Remove(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	// Repeat a failed sync even when a preceding process already unlinked it.
	err = syncDirectory(filepath.Dir(path))
	if os.IsNotExist(err) {
		return nil
	}
	return err
}
func (s Server) refuseRunnerReplacement() error {
	names, err := filepath.Glob(filepath.Join(s.systemdDirectory(), "mmx-owed-restore-*.service"))
	if err != nil {
		return err
	}
	if len(names) != 0 {
		return fmt.Errorf("cannot replace the node runner executable while owed restore unit %s exists; resolve the owed restore first", filepath.Base(names[0]))
	}
	return nil
}
