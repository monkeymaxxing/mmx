package node

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"mmx/internal/modules"
	"mmx/internal/protocol"
)

var errRestoreServiceWaitExpired = errors.New("restore service wait expired")

// A service is bound to the task by the engine, not by module-supplied JSON.
// Activation factories are compiled into the registry;
// the saved record contains data for that factory, never command text.
type owedRestoreService struct {
	server                              Server
	nodeJobID, nodeTaskPath, moduleName string
	connectionKinds                     []protocol.ConnectionKind
	secret                              func() bool
	runtime                             modules.Runtime
}

func (s Server) changeService(jobID, taskPath, moduleName string, kinds []protocol.ConnectionKind, secret func() bool, runtime modules.Runtime) modules.OwedRestoreService {
	return owedRestoreService{s, jobID, taskPath, moduleName, kinds, secret, runtime}
}
func (service owedRestoreService) CheckChangeAllowed(ctx context.Context, destination string) error {
	return service.server.CheckChangeAllowed(ctx, destination)
}
func (s Server) newChangeActivation(r owedRestoreRecord) (modules.ChangeActivation, error) {
	if s.activationFactory != nil {
		return s.activationFactory(r.Outcome.ModuleName, r.Outcome.DestinationFile, r.ModuleRestoreData)
	}
	module, ok := modules.LookUpCoreModule(r.Outcome.ModuleName)
	if !ok || module.NewChangeActivation == nil {
		return nil, fmt.Errorf("module %s does not provide configuration change activation", r.Outcome.ModuleName)
	}
	return module.NewChangeActivation(r.Outcome.DestinationFile, r.ModuleRestoreData)
}

func (service owedRestoreService) ApplyChange(ctx context.Context, change modules.ChangeWithOwedRestore) protocol.Result {
	s := service.server
	if service.runtime.Dryrun || change.RestoreConfirmationWait <= 0 {
		return protocol.Failed(fmt.Errorf("a configuration change with an owed restore requires a positive reachability confirmation wait and cannot run during a dryrun."))
	}
	destination := ""
	if change.PreparedFile != nil {
		destination = change.PreparedFile.DestinationFile()
	}
	boot, err := s.machine.CurrentBootID()
	if err != nil {
		return protocol.Failed(err)
	}
	now := s.timeNow()
	record := owedRestoreRecord{
		Outcome:   protocol.OwedRestore{NodeTaskPath: service.nodeTaskPath, ModuleName: service.moduleName, DestinationFile: destination, ChangeSubject: change.ChangeSubject},
		NodeJobID: service.nodeJobID, NodeBootIDBeforeChange: boot, Phase: waitingBeforeChange,
		ConfirmationWaitDuration:              change.RestoreConfirmationWait.String(),
		BeforeChangeConfirmationDeadline:      now.Add(change.RestoreConfirmationWait),
		LatestAfterChangeConfirmationDeadline: now.Add(change.RestoreConfirmationWait).Add(armingAllowance).Add(change.RestoreConfirmationWait),
		ConnectionKindsToConfirm:              service.connectionKinds, ModuleRestoreData: change.ModuleRestoreData, Change: "unchanged",
		RestoreAfterUnit: change.RestoreAfterUnit, RestoreSuccessSentence: change.RestoreSuccessSentence,
	}
	if service.secret != nil {
		record.Outcome.Secret = service.secret()
	}
	checkedActivation, err := s.newChangeActivation(record)
	if err != nil {
		return protocol.Failed(err)
	}
	_, usesSavedFiles := checkedActivation.(modules.ChangeActivationWithSavedFiles)
	if err := s.withOwedRestores(ctx, func() error {
		if err := s.checkChangeAllowed(record.Outcome.DestinationFile); err != nil {
			return err
		}
		return s.createOwedRestore(&record)
	}); err != nil {
		return protocol.Failed(err)
	}
	id := record.Outcome.OwedRestoreID
	installed := protocol.Success("unchanged")
	fail := func(cause error) protocol.Result {
		// Cancellation and stop do not cancel recovery. Give the service time to
		// report one bounded attempt, while its durable record remains authoritative.
		recovery, cancel := context.WithTimeout(context.Background(), restoreServiceLimit)
		defer cancel()
		err := s.withOwedRestores(recovery, func() error {
			var err error
			record, err = s.readOwedRestore(id)
			if err != nil {
				return err
			}
			return s.requireRestore(&record, cause.Error())
		})
		if err != nil {
			return changeResult(record, installed, errors.Join(cause, err))
		}
		if record.Phase == changeAccepted || record.Phase == configRestored || record.Phase == restoreFailed {
			return changeResult(record, installed, cause)
		}
		if !record.ChangeStarted {
			err = s.RunOwedRestore(recovery, id)
		} else {
			record, err = s.waitForChange(recovery, id, false)
		}
		return changeResult(record, installed, errors.Join(cause, err))
	}
	record, err = s.waitBeforeChange(ctx, id)
	if err != nil {
		return fail(err)
	}
	var previous *modules.PreviousFileMetadata
	if change.PreparedFile != nil {
		saved, saveErr := change.PreparedFile.SavePreviousFile(ctx, filepath.Join(s.restoreDirectory(id), "previous_file_contents"))
		if saveErr != nil {
			return fail(saveErr)
		}
		previous = &saved
		if usesSavedFiles && change.PreparedFile.InstallsAFile() {
			if err := change.PreparedFile.SaveProposedFile(ctx, filepath.Join(s.restoreDirectory(id), "proposed_file_contents")); err != nil {
				return fail(fmt.Errorf("Cannot save a copy of the proposed file %s; the destination file was not changed: %w.", destination, err))
			}
		}
	}
	err = s.withOwedRestores(ctx, func() error {
		var err error
		record, err = s.readOwedRestore(id)
		if err != nil {
			return err
		}
		if record.Phase != preparingRestore || s.timeNow().After(record.BeforeChangeCompletedAt.Add(armingAllowance)) {
			return fmt.Errorf("Cannot change %s: restore preparation is no longer active or exceeded the allowed setup time; %s.", record.subject(), record.notChanged())
		}
		record.PreviousFile = previous
		record.RestoreDeadline = s.timeNow().Add(change.RestoreConfirmationWait)
		return s.writeOwedRestore(&record)
	})
	if err != nil {
		return fail(err)
	}
	if err := s.armRestore(ctx, record); err != nil {
		return fail(fmt.Errorf("Cannot schedule the owed restore for %s; %s: %w.", record.subject(), record.notChanged(), err))
	}
	activation, err := lockContext(ctx, filepath.Join(s.Root, "change_activation.lock"))
	if err != nil {
		return fail(err)
	}
	err = s.withOwedRestores(ctx, func() error {
		var err error
		record, err = s.readOwedRestore(id)
		if err != nil {
			return err
		}
		running, err := s.changeWorkerRunning(record)
		if err != nil {
			return err
		}
		if record.Phase != preparingRestore || !running || !s.timeNow().Before(record.RestoreDeadline) {
			return fmt.Errorf("Cannot activate the file change: restore preparation is no longer active, the node job worker process is not confirmed running, or the restore deadline has been reached.")
		}
		record.Phase, record.ChangeStarted, record.Change = applyingChange, true, "unknown"
		if err := s.writeOwedRestore(&record); err != nil {
			return err
		}
		// This short filesystem operation is the only mutation inside the record
		// lock. A restore cannot decide from an old phase while installation runs.
		if change.PreparedFile != nil {
			installed = change.PreparedFile.Install(ctx)
		} else {
			// Activation owns every mutation; until it completes the result is unknown.
			installed = protocol.Success("unknown")
		}
		record.Change = installed.Change
		if installed.Status != "succeeded" {
			return s.requireRestore(&record, installed.Reason)
		}
		record.Phase = activatingConfig
		return s.writeOwedRestore(&record)
	})
	unlock(activation)
	if err != nil {
		return fail(err)
	}
	if installed.Status != "succeeded" {
		return fail(errors.New(installed.Reason))
	}
	if _, err := s.systemControl(ctx, "start", "--no-block", activationServiceUnit(id)); err != nil {
		return fail(err)
	}
	record, err = s.waitForChange(ctx, id, true)
	if err != nil {
		if errors.Is(err, errRestoreServiceWaitExpired) {
			// The service has already had its allowance. Keep the durable obligation
			// without starting another attempt or waiting through another allowance.
			return changeResult(record, installed, err)
		}
		return fail(err)
	}
	if record.Phase != changeAccepted {
		return changeResult(record, installed, nil)
	}
	// An accepted record is safe even if cleanup fails. A late timer start reads
	// that decision and only cleans up; it never reverses the accepted change.
	cleanup, cancel := context.WithTimeout(context.Background(), restoreServiceLimit)
	defer cancel()
	err = s.RunOwedRestore(cleanup, id)
	if err != nil {
		installed.Reason = "The change was accepted; owed restore cleanup remains: " + err.Error()
	}
	return changeResult(record, installed, nil)
}

// handSavedFiles gives an activation that asks for them the paths of the
// change's saved previous and proposed files, "" for a file that does not
// exist.
func (s Server) handSavedFiles(activation modules.ChangeActivation, r owedRestoreRecord) {
	withSavedFiles, ok := activation.(modules.ChangeActivationWithSavedFiles)
	if !ok {
		return
	}
	previous, proposed := "", filepath.Join(s.restoreDirectory(r.Outcome.OwedRestoreID), "proposed_file_contents")
	if r.PreviousFile != nil && r.PreviousFile.Existed {
		previous = filepath.Join(s.restoreDirectory(r.Outcome.OwedRestoreID), "previous_file_contents")
	}
	if _, err := os.Lstat(proposed); err != nil {
		proposed = ""
	}
	withSavedFiles.UseSavedFiles(previous, proposed)
}

func changeResult(r owedRestoreRecord, installed protocol.Result, cause error) protocol.Result {
	result := installed
	result.Change = r.Change
	outcome := r.Outcome.Safe()
	result.OwedRestore, result.Secret = &outcome, r.Outcome.Secret
	if r.Phase != changeAccepted {
		result.Status, result.NoOtherTry = "failed", true
		result.Reason = r.Outcome.OriginalFailureReason
		if result.Reason == "" && cause != nil {
			result.Reason = cause.Error()
		} else if errors.Is(cause, errRestoreServiceWaitExpired) {
			result.Reason += " " + cause.Error() //nolint:nilaway // errors.Is with errRestoreServiceWaitExpired cannot match a nil cause.
		}
		if result.Reason != "" && !strings.HasSuffix(result.Reason, ".") {
			result.Reason += "."
		}
		if r.Phase == configRestored {
			if r.Outcome.DestinationFile == "" {
				result.Change = "changed"
				result.Reason += " " + r.RestoreSuccessSentence
			} else if r.PreviousFile != nil && r.PreviousFile.Existed {
				if r.ChangeActivationFailed {
					result.Reason += " The previous file and its running configuration are restored."
				} else {
					result.Reason += fmt.Sprintf(" The previous file %s and its running configuration are restored.", r.Outcome.DestinationFile)
				}
			} else {
				result.Reason += fmt.Sprintf(" The new file %s and its declared virtual devices were removed; the previous configuration is restored.", r.Outcome.DestinationFile)
			}
		} else if r.Outcome.RestoreFailureReason != "" {
			retained := "The owed restore remains"
			if r.PreviousFile != nil && r.PreviousFile.Existed {
				retained = "The owed restore and saved previous file remain"
			}
			result.Reason += fmt.Sprintf(" Restoring %s failed: %s. %s; the previous running configuration is not confirmed restored.", r.subject(), strings.TrimSuffix(outcome.RestoreFailureReason, "."), retained)
		}
		if result.Reason == "" {
			result.Reason = "The change was not accepted."
		}
	}
	return result
}
func (s Server) waitBeforeChange(ctx context.Context, id string) (record owedRestoreRecord, err error) {
	for {
		err = s.withOwedRestores(ctx, func() error {
			var err error
			record, err = s.readOwedRestore(id)
			if err != nil {
				return err
			}
			if record.Phase != waitingBeforeChange {
				return fmt.Errorf("The file change is no longer waiting for reachability confirmation before activation.")
			}
			if err := s.changeStop(ctx, record.NodeJobID); err != nil {
				return err
			}
			running, err := s.changeWorkerRunning(record)
			if err != nil {
				return err
			}
			if !running {
				return fmt.Errorf("The node job worker process ended before the %s.", record.changeName())
			}
			if len(record.BeforeChangeConfirmedConnectionKinds) == len(record.ConnectionKindsToConfirm) || !s.timeNow().Before(record.BeforeChangeConfirmationDeadline) {
				if len(record.BeforeChangeConfirmedConnectionKinds) == 0 {
					if len(record.ConnectionKindsToConfirm) == 1 {
						return fmt.Errorf("No new %s connection confirmed node reachability before the %s; %s was not changed.", strings.ToUpper(string(record.ConnectionKindsToConfirm[0])), record.changeName(), record.subject())
					}
					return fmt.Errorf("No new SSH or HTTPS connection confirmed node reachability before the %s; %s was not changed.", record.changeName(), record.subject())
				}
				record.Phase, record.BeforeChangeCompletedAt = preparingRestore, s.timeNow()
				return s.writeOwedRestore(&record)
			}
			return nil
		})
		if err != nil || record.Phase == preparingRestore {
			return record, err
		}
		if err := waitForRecord(ctx); err != nil {
			return record, err
		}
	}
}
func (s Server) changeStop(ctx context.Context, jobID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	stop, err := s.stopRequested(jobID)
	if err != nil {
		return err
	}
	if stop {
		return fmt.Errorf("The node job was asked to stop before the file change was accepted.")
	}
	return nil
}
func (s Server) waitForChange(ctx context.Context, id string, observeStop bool) (record owedRestoreRecord, err error) {
	requestedRestore := false
	workerContext := ctx
	allowance := restoreServiceLimit
	if s.restoreWait > 0 {
		allowance = s.restoreWait
	}
	var restoreWait context.Context
	defer func() {
		// A task deadline still enters the caller's independent recovery wait.
		// Only exhaustion of that wait, or this allowance, ends recovery waiting.
		if err != nil && restoreWait != nil && (!observeStop || workerContext.Err() == nil) && errors.Is(restoreWait.Err(), context.DeadlineExceeded) {
			err = fmt.Errorf("%w: %s did not record completion within %s; the owed restore and saved previous state remain", errRestoreServiceWaitExpired, restoreServiceUnit(id), allowance)
		}
	}()
	for {
		start := false
		err = s.withOwedRestores(ctx, func() error {
			var err error
			record, err = s.readOwedRestore(id)
			if err != nil {
				return err
			}
			if record.resolved() || record.Phase == restoreFailed {
				return nil
			}
			if observeStop {
				// Confirmation may accept before this worker observes stop: every
				// required new connection proved reachability, and the job stops after this task.
				if err := s.changeStop(ctx, record.NodeJobID); err != nil {
					return err
				}
			}
			if record.Phase != restoringConfig && record.Phase != restorationRequired && !s.timeNow().Before(record.RestoreDeadline) {
				if err := s.requireRestore(&record, missingReachabilityReason(record)); err != nil {
					return err
				}
			}
			start = record.Phase == restorationRequired && !requestedRestore
			return nil
		})
		if err != nil || record.resolved() || record.Phase == restoreFailed {
			return record, err
		}
		if restoreWait == nil && (record.Phase == restorationRequired || record.Phase == restoringConfig) {
			var cancel context.CancelFunc
			restoreWait, cancel = context.WithTimeout(ctx, allowance)
			defer cancel()
			ctx = restoreWait
		}
		if record.Phase == restoringConfig {
			if _, err := s.reconcileInterruptedRestore(ctx, id); err != nil {
				return record, err
			}
		}
		if start {
			requestedRestore = true
			if err := s.startRestore(ctx, id); err != nil {
				return record, err
			}
		}
		if err := waitForRecord(ctx); err != nil {
			return record, err
		}
	}
}

// ActivateChange is entered in its own systemd service. The activation
// lock covers native commands and all their completion checks. Restoring first
// stops this entire service control group, then takes that same lock.
func (s Server) ActivateChange(ctx context.Context, id string) error {
	if err := protocol.ValidateOwedRestoreID(id); err != nil {
		return err
	}
	activation, err := lockContext(ctx, filepath.Join(s.Root, "change_activation.lock"))
	if err != nil {
		return err
	}
	defer unlock(activation)
	var record owedRestoreRecord
	run := false
	err = s.withOwedRestores(ctx, func() error {
		var err error
		record, err = s.readOwedRestore(id)
		if err != nil {
			return err
		}
		if record.Phase != activatingConfig {
			return nil
		}
		running, err := s.changeWorkerRunning(record)
		if err != nil {
			return err
		}
		if !running || !s.timeNow().Before(record.RestoreDeadline) {
			return s.requireRestore(&record, "Activation could not complete before reachability confirmation expired.")
		}
		run = true
		return nil
	})
	if err != nil {
		return err
	}
	if !run {
		if record.Phase == restorationRequired {
			return s.startRestore(ctx, id)
		}
		return nil
	}
	adapter, err := s.newChangeActivation(record)
	runtime := modules.Runtime{PrivateTaskDirectoryRoot: s.privateTaskDirectoryRoot, CheckFileDestination: s.CheckChangeAllowed}
	if err == nil {
		s.handSavedFiles(adapter, record)
		err = adapter.PrepareActivation(ctx, runtime, false)
	}
	if err == nil {
		err = adapter.Activate(ctx, runtime, false) //nolint:nilaway // The activation factory returns an allocated adapter when err == nil.
	}
	if err == nil {
		err = adapter.RemoveObsoleteVirtualDevices(ctx, runtime, false)
	}
	if err == nil {
		err = adapter.CheckActivatedConfiguration(ctx, runtime, false)
	}
	activationErr := err
	err = s.withOwedRestores(ctx, func() error {
		var err error
		record, err = s.readOwedRestore(id)
		if err != nil {
			return err
		}
		if record.Phase != activatingConfig {
			return nil
		}
		if activationErr != nil {
			record.ChangeActivationFailed = true
			return s.requireRestore(&record, activationErr.Error())
		}
		running, err := s.changeWorkerRunning(record)
		if err != nil {
			return err
		}
		if !running || !s.timeNow().Before(record.RestoreDeadline) {
			return s.requireRestore(&record, "Activation used the reachability confirmation budget.")
		}
		if record.Outcome.DestinationFile == "" {
			record.Change = "changed"
		}
		record.Phase = waitingAfterChange
		return s.writeOwedRestore(&record)
	})
	if err != nil {
		return err
	}
	if record.Phase == restorationRequired {
		return s.startRestore(ctx, id)
	}
	return nil
}

// changeName names the change an owed restore protects: a file change, or the
// change of a module that holds no file.
func (r owedRestoreRecord) changeName() string {
	if r.Outcome.DestinationFile == "" {
		return "change"
	}
	return "file change"
}

// notChanged says what stayed as it was when a change did not start.
func (r owedRestoreRecord) notChanged() string {
	if r.Outcome.DestinationFile == "" {
		return "the proposed change was not applied"
	}
	return "the destination file was not changed"
}
