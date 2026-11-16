package node

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"syscall"
	"time"

	"mmx/internal/durable"
	"mmx/internal/modules"
	"mmx/internal/protocol"
)

const (
	waitingBeforeChange = "waiting_before_change"
	preparingRestore    = "preparing_restore"
	applyingChange      = "applying_change"
	activatingConfig    = "activating_config"
	waitingAfterChange  = "waiting_after_change"
	changeAccepted      = "change_accepted"
	restorationRequired = "restoration_required"
	restoringConfig     = "restoring_previous_config"
	restoreFailed       = "restore_failed"
	configRestored      = "previous_config_restored"
	abandonedChange     = "abandoned_before_change"
	armingAllowance     = 30 * time.Second
	restoreAttemptLimit = 5 * time.Minute
	restoreServiceLimit = 330 * time.Second
)

// The record is outside job history. Every phase change holds
// owed_restores.lock. No native command runs with that lock held.
// ChangeStarted is durable BEFORE applying a change: after a crash it means
// that the configuration may have changed, even if no result was saved.
type owedRestoreRecord struct {
	Outcome                               protocol.OwedRestore          `json:"owed_restore"`
	NodeJobID                             string                        `json:"node_job_id"`
	NodeBootIDBeforeChange                string                        `json:"node_boot_id_before_change"`
	Phase                                 string                        `json:"change_phase"`
	Revision                              uint64                        `json:"record_revision"`
	ConfirmationWaitDuration              string                        `json:"reachability_confirmation_wait_duration"`
	BeforeChangeConfirmationDeadline      time.Time                     `json:"before_change_confirmation_deadline"`
	LatestAfterChangeConfirmationDeadline time.Time                     `json:"latest_after_change_confirmation_deadline"`
	BeforeChangeCompletedAt               time.Time                     `json:"before_change_confirmation_wait_ended_at,omitzero"`
	RestoreDeadline                       time.Time                     `json:"restore_deadline,omitzero"`
	ConnectionKindsToConfirm              []protocol.ConnectionKind     `json:"connection_kinds_to_confirm"`
	BeforeChangeConfirmedConnectionKinds  []protocol.ConnectionKind     `json:"before_change_confirmed_connection_kinds"`
	AfterChangeConfirmedConnectionKinds   []protocol.ConnectionKind     `json:"after_change_confirmed_connection_kinds"`
	PreviousFile                          *modules.PreviousFileMetadata `json:"previous_file_metadata,omitempty"`
	ModuleRestoreData                     json.RawMessage               `json:"module_restore_data"`
	RestoreAfterUnit                      string                        `json:"restore_after_unit,omitempty"`
	RestoreSuccessSentence                string                        `json:"restore_success_sentence,omitempty"`
	ChangeActivationFailed                bool                          `json:"change_activation_failed,omitempty"`
	ChangeStarted                         bool                          `json:"change_started"`
	Change                                string                        `json:"change"`
	CleanupComplete                       bool                          `json:"owed_restore_cleanup_complete"`
}

func (r owedRestoreRecord) resolved() bool {
	return r.Phase == changeAccepted || r.Phase == configRestored || r.Phase == abandonedChange
}
func restoreServiceUnit(id string) string          { return "mmx-owed-restore-" + id + ".service" }
func restoreTimerUnit(id string) string            { return "mmx-owed-restore-" + id + ".timer" }
func activationServiceUnit(id string) string       { return "mmx-activate-change-" + id + ".service" }
func (s Server) restoreDirectory(id string) string { return filepath.Join(s.Root, "owed_restores", id) }

func (r owedRestoreRecord) validate() error {
	if err := protocol.ValidateOwedRestoreID(r.Outcome.OwedRestoreID); err != nil {
		return err
	}
	if !validID.MatchString(r.NodeJobID) || r.NodeBootIDBeforeChange == "" || r.Outcome.NodeTaskPath == "" || !modules.Name.MatchString(r.Outcome.ModuleName) {
		return fmt.Errorf("owed restore record requires a valid node job ID, a node boot ID, a node task path, and a module name")
	}
	if r.Outcome.DestinationFile != "" && (!filepath.IsAbs(r.Outcome.DestinationFile) || filepath.Clean(r.Outcome.DestinationFile) != r.Outcome.DestinationFile || r.Outcome.DestinationFile == "/") {
		return fmt.Errorf("owed restore destination_file must be an absolute file path without redundant components")
	}
	if r.Outcome.DestinationFile == "" {
		if strings.TrimSpace(r.Outcome.ChangeSubject) == "" || strings.ContainsAny(r.Outcome.ChangeSubject, "\x00\r\n") ||
			strings.TrimSpace(r.RestoreSuccessSentence) == "" || strings.ContainsAny(r.RestoreSuccessSentence, "\x00\r\n") {
			return fmt.Errorf("A restore without a destination file requires a plain change subject and a successful restore sentence.")
		}
		if !modules.ServiceName.MatchString(r.RestoreAfterUnit) || !strings.HasSuffix(r.RestoreAfterUnit, ".service") {
			return fmt.Errorf("A restore without a destination file requires one valid service unit to run after.")
		}
		if r.PreviousFile != nil || r.Outcome.PreviousFileRestored || r.Outcome.PreviousConfigActivated || r.Outcome.ObsoleteVirtualDevicesRemoved {
			return fmt.Errorf("A restore without a destination file must not carry file metadata or file restoration steps.")
		}
	} else if r.Outcome.ChangeSubject != "" || r.RestoreAfterUnit != "" || r.RestoreSuccessSentence != "" || r.Outcome.PreviousMembershipRestored {
		return fmt.Errorf("A file restore must not carry module-data-only restore descriptions or membership restoration steps.")
	}
	if r.Outcome.RestoreServiceUnit != restoreServiceUnit(r.Outcome.OwedRestoreID) {
		return fmt.Errorf("owed restore restore_service_unit does not match owed_restore_id")
	}
	switch r.Phase {
	case waitingBeforeChange, preparingRestore, applyingChange, activatingConfig, waitingAfterChange, changeAccepted, restorationRequired, restoringConfig, restoreFailed, configRestored, abandonedChange:
	default:
		return fmt.Errorf("invalid owed restore change_phase %q", r.Phase)
	}
	duration, err := time.ParseDuration(r.ConfirmationWaitDuration)
	if err != nil || duration <= 0 || r.Revision == 0 {
		return fmt.Errorf("owed restore requires a positive reachability_confirmation_wait_duration and record_revision")
	}
	p := r.pendingForPhase(protocol.ConfirmationBeforeChange, r.BeforeChangeConfirmationDeadline)
	if err := p.Validate(); err != nil {
		return fmt.Errorf("owed restore: %w", err)
	}
	if !r.LatestAfterChangeConfirmationDeadline.Equal(r.BeforeChangeConfirmationDeadline.Add(armingAllowance).Add(duration)) {
		return fmt.Errorf("owed restore latest_after_change_confirmation_deadline does not match the before-change deadline plus the restore setup allowance and confirmation wait duration")
	}
	if len(r.AfterChangeConfirmedConnectionKinds) > 0 || r.ChangeStarted {
		p = r.pendingForPhase(protocol.ConfirmationAfterChange, r.RestoreDeadline)
		if err := p.Validate(); err != nil {
			return fmt.Errorf("owed restore: %w", err)
		}
	}
	if !r.RestoreDeadline.IsZero() {
		_, offset := r.RestoreDeadline.Zone()
		if offset != 0 || r.RestoreDeadline.After(r.LatestAfterChangeConfirmationDeadline) {
			return fmt.Errorf("owed restore restore_deadline must be UTC and no later than latest_after_change_confirmation_deadline")
		}
	}
	if r.Phase != waitingBeforeChange && r.Phase != abandonedChange {
		_, offset := r.BeforeChangeCompletedAt.Zone()
		if r.BeforeChangeCompletedAt.IsZero() || offset != 0 {
			return fmt.Errorf("owed restore requires a UTC before_change_confirmation_wait_ended_at after the before-change wait")
		}
	}
	mutationPhase := r.Phase != waitingBeforeChange && r.Phase != preparingRestore && r.Phase != abandonedChange
	if mutationPhase != r.ChangeStarted {
		return fmt.Errorf("owed restore change_started does not match its change_phase")
	}
	if r.Phase == changeAccepted && len(r.AfterChangeConfirmedConnectionKinds) != len(r.BeforeChangeConfirmedConnectionKinds) {
		return fmt.Errorf("accepted change requires every after-change confirmation")
	}
	if r.ChangeStarted && r.Outcome.DestinationFile != "" && r.PreviousFile == nil {
		return fmt.Errorf("owed restore installation requires saved previous file metadata")
	}
	if r.PreviousFile != nil {
		if err := r.PreviousFile.Validate(); err != nil {
			return err
		}
	}
	if r.CleanupComplete && !r.resolved() {
		return fmt.Errorf("unresolved owed restore cannot have owed_restore_cleanup_complete")
	}
	if r.Change != "unchanged" && r.Change != "changed" && r.Change != "unknown" {
		return fmt.Errorf("invalid owed restore change %q", r.Change)
	}
	if !json.Valid(r.ModuleRestoreData) {
		return fmt.Errorf("owed restore module_restore_data must be valid JSON")
	}
	return nil
}

func (r owedRestoreRecord) pendingForPhase(phase protocol.ConfirmationPhase, now time.Time) protocol.PendingReachabilityConfirmation {
	p := protocol.PendingReachabilityConfirmation{NodeJobID: r.NodeJobID, OwedRestoreID: r.Outcome.OwedRestoreID, ConfirmationPhase: phase, NodeTime: now, LatestAfterChangeConfirmationDeadline: r.LatestAfterChangeConfirmationDeadline}
	if phase == protocol.ConfirmationBeforeChange {
		p.ConfirmationDeadline = r.BeforeChangeConfirmationDeadline
		p.ConnectionKindsToConfirm = r.ConnectionKindsToConfirm
		p.ConfirmedConnectionKinds = r.BeforeChangeConfirmedConnectionKinds
	} else {
		p.ConfirmationDeadline = r.RestoreDeadline
		p.ConnectionKindsToConfirm = r.BeforeChangeConfirmedConnectionKinds
		p.ConfirmedConnectionKinds = r.AfterChangeConfirmedConnectionKinds
	}
	return p
}
func (r owedRestoreRecord) pending(now time.Time) *protocol.PendingReachabilityConfirmation {
	var phase protocol.ConfirmationPhase
	switch r.Phase {
	case waitingBeforeChange:
		phase = protocol.ConfirmationBeforeChange
	case waitingAfterChange:
		phase = protocol.ConfirmationAfterChange
	default:
		return nil
	}
	p := r.pendingForPhase(phase, now)
	return &p
}

func syncDirectory(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	return errors.Join(f.Sync(), f.Close())
}
func privateDirectory(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode().Perm() != 0700 || info.Sys().(*syscall.Stat_t).Uid != uint32(os.Geteuid()) {
		return fmt.Errorf("owed restore directory must belong to root and have mode 0700: %s", path)
	}
	return nil
}
func (s Server) withOwedRestores(ctx context.Context, fn func() error) error {
	held, err := lockContext(ctx, filepath.Join(s.Root, "owed_restores.lock"))
	if err != nil {
		return err
	}
	defer unlock(held)
	return fn()
}
func lockContext(ctx context.Context, path string) (*os.File, error) {
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		held, err := lock(path)
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			return held, err
		}
		if err := waitForRecord(ctx); err != nil {
			return nil, err
		}
	}
}
func waitForRecord(ctx context.Context) error {
	timer := time.NewTimer(progressPoll)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// All callers of record IO hold owed_restores.lock. Directory creation and
// collision detection share that lock, so another reader cannot see half a record.
func (s Server) createOwedRestore(r *owedRestoreRecord) error {
	r.Outcome.OwedRestoreID, r.Outcome.RestoreServiceUnit, r.Revision = "0000000000000000", restoreServiceUnit("0000000000000000"), 1
	if err := r.validate(); err != nil {
		return err
	}
	r.Revision = 0
	root := filepath.Join(s.Root, "owed_restores")
	if err := os.Mkdir(root, 0700); err != nil && !os.IsExist(err) {
		return err
	}
	if err := privateDirectory(root); err != nil {
		return err
	}
	if err := syncDirectory(s.Root); err != nil {
		return err
	}
	for {
		var random [8]byte
		if _, err := rand.Read(random[:]); err != nil {
			return err
		}
		id := hex.EncodeToString(random[:])
		dir := s.restoreDirectory(id)
		if err := os.Mkdir(dir, 0700); os.IsExist(err) {
			continue
		} else if err != nil {
			return err
		}
		r.Outcome.OwedRestoreID, r.Outcome.RestoreServiceUnit = id, restoreServiceUnit(id)
		if err := syncDirectory(root); err != nil {
			return err
		}
		return s.writeOwedRestore(r)
	}
}

// restoreStatus is the restore_status that the controller process reads,
// derived from the phase at every write, so no phase change can leave the
// controller process seeing a required or failed restore as not started.
// "not_needed" means the change was accepted and no restore remains.
func restoreStatus(phase string) string {
	switch phase {
	case restorationRequired:
		return "required"
	case restoringConfig:
		return "running"
	case restoreFailed:
		return "failed"
	case configRestored:
		return "succeeded"
	case changeAccepted:
		// The change was kept, so the owed restore was cancelled and its
		// restore service no longer exists.
		return "not_needed"
	}
	return "not_started"
}

func (s Server) writeOwedRestore(r *owedRestoreRecord) error {
	r.Revision++
	r.Outcome.RestoreStatus = restoreStatus(r.Phase)
	if err := r.validate(); err != nil {
		return err
	}
	return durable.JSON(filepath.Join(s.restoreDirectory(r.Outcome.OwedRestoreID), "owed_restore.json"), r)
}
func (s Server) readOwedRestore(id string) (owedRestoreRecord, error) {
	var r owedRestoreRecord
	if err := protocol.ValidateOwedRestoreID(id); err != nil {
		return r, err
	}
	dir := s.restoreDirectory(id)
	if err := privateDirectory(dir); err != nil {
		return r, err
	}
	path := filepath.Join(dir, "owed_restore.json")
	info, err := os.Lstat(path)
	if err != nil {
		return r, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Sys().(*syscall.Stat_t).Uid != uint32(os.Geteuid()) {
		return r, fmt.Errorf("owed restore record must be a private regular file: %s", path)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return r, err
	}
	if err := protocol.Decode(b, &r); err != nil {
		return r, fmt.Errorf("reading owed restore %s: %w", id, err)
	}
	if r.Outcome.OwedRestoreID != id {
		return r, fmt.Errorf("owed restore owed_restore_id does not match its directory: %s", id)
	}
	return r, r.validate()
}
func (s Server) owedRestoreRecords() ([]owedRestoreRecord, error) {
	if err := privateDirectory(filepath.Join(s.Root, "owed_restores")); os.IsNotExist(err) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(filepath.Join(s.Root, "owed_restores"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var records []owedRestoreRecord
	for _, entry := range entries {
		if err := protocol.ValidateOwedRestoreID(entry.Name()); err != nil {
			return nil, fmt.Errorf("unexpected entry in owed_restores: %s", entry.Name())
		}
		r, err := s.readOwedRestore(entry.Name())
		if os.IsNotExist(err) {
			unpublished, checkErr := s.unpublishedOwedRestore(entry.Name())
			if checkErr != nil {
				return nil, checkErr
			}
			if unpublished {
				continue
			}
		}
		if err != nil {
			return nil, err
		}
		records = append(records, r)
	}
	return records, nil
}

// Called under owed_restores.lock after a missing record. Only an empty
// directory or private durable.Write temporary files can precede publication.
// Saved contents, a lock, or any installed unit require a real record.
func (s Server) unpublishedOwedRestore(id string) (bool, error) {
	contents, err := os.ReadDir(s.restoreDirectory(id))
	if err != nil {
		return false, err
	}
	for _, entry := range contents {
		suffix, temporary := strings.CutPrefix(entry.Name(), ".mmx-")
		if !temporary || suffix == "" || strings.Trim(suffix, "0123456789") != "" {
			return false, nil
		}
		info, err := entry.Info()
		if err != nil {
			return false, err
		}
		if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Sys().(*syscall.Stat_t).Uid != uint32(os.Geteuid()) {
			return false, nil
		}
	}
	for _, unit := range []string{restoreServiceUnit(id), restoreTimerUnit(id), activationServiceUnit(id),
		filepath.Join("multi-user.target.wants", restoreServiceUnit(id)), filepath.Join("timers.target.wants", restoreTimerUnit(id))} {
		if _, err := os.Lstat(filepath.Join(s.systemdDirectory(), unit)); err == nil {
			return false, nil
		} else if !os.IsNotExist(err) {
			return false, err
		}
	}
	return true, nil
}

func (s Server) changeWorkerRunning(r owedRestoreRecord) (bool, error) {
	boot, err := s.machine.CurrentBootID()
	if err != nil || boot != r.NodeBootIDBeforeChange {
		return false, err
	}
	return s.workerRunning(r.NodeJobID)
}
func (s Server) requireRestore(r *owedRestoreRecord, reason string) error {
	if r.resolved() || r.Phase == restorationRequired || r.Phase == restoringConfig || r.Phase == restoreFailed {
		return nil
	}
	if r.Outcome.OriginalFailureReason == "" {
		r.Outcome.OriginalFailureReason = reason
	}
	if !r.ChangeStarted {
		r.Phase = abandonedChange
	} else {
		r.Phase = restorationRequired
	}
	return s.writeOwedRestore(r)
}

// ConfirmReachability receives the connection kind from the request process,
// never from JSON. Acceptance and expiry use this same short locked decision.
func (s Server) ConfirmReachability(ctx context.Context, request protocol.ReachabilityConfirmationRequest, kind protocol.ConnectionKind) (answer protocol.ReachabilityConfirmationResponse, err error) {
	answer = protocol.ReachabilityConfirmationResponse{NodeJobID: request.NodeJobID, OwedRestoreID: request.OwedRestoreID, ConfirmationPhase: request.ConfirmationPhase, ReceivedConnectionKind: kind}
	if err := request.Validate(); err != nil {
		return answer, err
	}
	if kind != protocol.ConnectionSSH && kind != protocol.ConnectionHTTPS {
		return answer, fmt.Errorf("received connection kind must be ssh or https")
	}
	err = s.withOwedRestores(ctx, func() error {
		r, err := s.readOwedRestore(request.OwedRestoreID)
		if os.IsNotExist(err) {
			answer.ConfirmationReason = "owed_restore_not_found"
			return nil
		}
		if err != nil {
			return err
		}
		answer.ConfirmationReason = "confirmation_phase_not_ready"
		if r.NodeJobID != request.NodeJobID {
			answer.ConfirmationReason = "node_job_does_not_match"
			return nil
		}
		// A lost response can repeat after durable acceptance. It acknowledges only
		// the same after-change connection kind, never a different phase or kind.
		if r.Phase == changeAccepted && request.ConfirmationPhase == protocol.ConfirmationAfterChange && slices.Contains(r.AfterChangeConfirmedConnectionKinds, kind) {
			// A preceding write may have renamed the record but failed its
			// directory sync. Repeat the durable write before acknowledging it.
			if err := s.writeOwedRestore(&r); err != nil {
				return err
			}
			answer.ConfirmationCounted, answer.ConfirmationReason = true, "already_confirmed"
			return nil
		}
		if r.resolved() {
			answer.ConfirmationReason = "owed_restore_resolved"
			return nil
		}
		if r.Phase == restorationRequired || r.Phase == restoringConfig || r.Phase == restoreFailed {
			answer.ConfirmationReason = "restoration_already_required"
			return nil
		}
		running, err := s.changeWorkerRunning(r)
		if err != nil {
			return err
		}
		if !running {
			answer.ConfirmationReason = "node_job_worker_not_running_on_original_boot"
			return s.requireRestore(&r, "node job worker process ended before reachability was confirmed")
		}
		now := s.timeNow()
		pending := r.pending(now)
		answer.PendingReachabilityConfirmation = pending
		if pending == nil || pending.ConfirmationPhase != request.ConfirmationPhase {
			return nil
		}
		if !now.Before(pending.ConfirmationDeadline) {
			answer.ConfirmationReason = "confirmation_deadline_passed"
			// Before-change expiry freezes the kinds in the worker, not in a request.
			if request.ConfirmationPhase == protocol.ConfirmationAfterChange {
				return s.requireRestore(&r, missingReachabilityReason(r))
			}
			return nil
		}
		if !slices.Contains(pending.ConnectionKindsToConfirm, kind) {
			answer.ConfirmationReason = "connection_kind_not_required"
			return nil
		}
		confirmed := &r.BeforeChangeConfirmedConnectionKinds
		if request.ConfirmationPhase == protocol.ConfirmationAfterChange {
			confirmed = &r.AfterChangeConfirmedConnectionKinds
		}
		if slices.Contains(*confirmed, kind) {
			// A preceding write may have renamed the record but failed its
			// directory sync. Repeat the durable write before acknowledging it.
			if err := s.writeOwedRestore(&r); err != nil {
				return err
			}
			answer.ConfirmationCounted, answer.ConfirmationReason = true, "already_confirmed"
			return nil
		}
		*confirmed = append(*confirmed, kind)
		if request.ConfirmationPhase == protocol.ConfirmationAfterChange && len(*confirmed) == len(pending.ConnectionKindsToConfirm) {
			r.Phase = changeAccepted
		}
		if err := s.writeOwedRestore(&r); err != nil {
			return err
		}
		answer.ConfirmationCounted, answer.ConfirmationReason = true, "confirmation_recorded"
		answer.PendingReachabilityConfirmation = r.pending(now)
		return nil
	})
	return answer, err
}

func (s Server) CheckChangeAllowed(ctx context.Context, destination string) error {
	return s.withOwedRestores(ctx, func() error { return s.checkChangeAllowed(destination) })
}
func (s Server) checkChangeAllowed(destination string) error {
	records, err := s.owedRestoreRecords()
	if err != nil {
		return err
	}
	for _, r := range records {
		if r.Outcome.DestinationFile == destination && !r.CleanupComplete {
			if destination == "" {
				reason := fmt.Sprintf("Cannot change %s: node job %s still owes a restore or its cleanup. Restore service: %s.", r.subject(), r.NodeJobID, r.Outcome.RestoreServiceUnit)
				if r.Outcome.RestoreFailureReason != "" {
					reason += " The earlier restore failed: " + r.Outcome.Safe().RestoreFailureReason
				}
				return errors.New(reason)
			}
			reason := fmt.Sprintf("Cannot change %s: node job %s still owes a restore of this file. Restore service: %s.", destination, r.NodeJobID, r.Outcome.RestoreServiceUnit)
			if r.resolved() {
				reason = fmt.Sprintf("Cannot change %s: file change %s of node job %s still needs cleanup; restore service: %s.", destination, r.Outcome.OwedRestoreID, r.NodeJobID, r.Outcome.RestoreServiceUnit)
			}
			if r.Outcome.RestoreFailureReason != "" {
				reason = fmt.Sprintf("Cannot change %s: the earlier file restore failed: %s; node job: %s; restore service: %s.", destination, r.Outcome.Safe().RestoreFailureReason, r.NodeJobID, r.Outcome.RestoreServiceUnit)
			}
			return errors.New(reason)
		}
	}
	return nil
}

func samePendingConfirmation(a, b *protocol.PendingReachabilityConfirmation) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	left, right := *a, *b
	left.NodeTime, right.NodeTime = time.Time{}, time.Time{}
	return reflect.DeepEqual(left, right)
}
func (s Server) owedRestoreMark(id string) string {
	var mark strings.Builder
	err := s.withOwedRestores(context.Background(), func() error {
		records, err := s.owedRestoreRecords()
		for _, r := range records {
			if r.NodeJobID == id {
				fmt.Fprintf(&mark, "%s:%d;", r.Outcome.OwedRestoreID, r.Revision)
			}
		}
		return err
	})
	if err != nil {
		return err.Error()
	}
	return mark.String()
}

// attachOwedRestores projects recovery state into ordinary job answers. The
// independent array survives a killed worker's incomplete task-result tree.
// A terminal result is refreshed in place without changing its finish time.
func (s Server) attachOwedRestores(response protocol.Response, persist bool) (protocol.Response, error) {
	before := response
	var start, abandon, inspectRestore []string
	err := s.withOwedRestores(context.Background(), func() error {
		records, err := s.owedRestoreRecords()
		if err != nil {
			return err
		}
		for _, record := range records {
			if record.NodeJobID != response.ID {
				continue
			}
			if record.Phase == restoringConfig {
				inspectRestore = append(inspectRestore, record.Outcome.OwedRestoreID)
			}
			if record.resolved() && !record.CleanupComplete {
				abandon = append(abandon, record.Outcome.OwedRestoreID)
			}
			if !record.resolved() && record.Phase != restoreFailed && record.Phase != restoringConfig {
				running, err := s.changeWorkerRunning(record)
				if err != nil {
					return err
				}
				if !running {
					if err := s.requireRestore(&record, "node job worker process ended before reachability was confirmed"); err != nil {
						return err
					}
					if record.ChangeStarted {
						start = append(start, record.Outcome.OwedRestoreID)
					} else {
						abandon = append(abandon, record.Outcome.OwedRestoreID)
					}
				}
			}
			found := false
			// Preserve outcomes already copied into history when their records expire.
			outcomes := slices.Clone(response.OwedRestores)
			for i := range outcomes {
				if outcomes[i].OwedRestoreID == record.Outcome.OwedRestoreID {
					outcomes[i], found = record.Outcome.Safe(), true
				}
			}
			if !found {
				outcomes = append(outcomes, record.Outcome.Safe())
			}
			response.OwedRestores = outcomes
			if pending := record.pending(s.timeNow()); pending != nil {
				response.PendingReachabilityConfirmation = pending
			}
		}
		return nil
	})
	if err != nil {
		return response, err
	}
	for _, id := range inspectRestore {
		interrupted, err := s.reconcileInterruptedRestore(context.Background(), id)
		if err != nil {
			return response, err
		}
		if interrupted != nil {
			for i := range response.OwedRestores {
				if response.OwedRestores[i].OwedRestoreID == id {
					response.OwedRestores[i] = interrupted.Safe()
				}
			}
		}
	}
	response.Results = refreshRestoreResults(response.Results, response.OwedRestores)
	if persist && response.Finished != nil && !reflect.DeepEqual(before, response) {
		if err := durable.JSON(filepath.Join(s.jobDir(response.ID), "result.json"), response); err != nil {
			return response, err
		}
	}
	for _, id := range start {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := s.startRestore(ctx, id)
		cancel()
		if err != nil {
			response.Housekeeping = append(response.Housekeeping, "starting owed restore: "+err.Error())
		}
	}
	for _, id := range abandon {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		err := s.RunOwedRestore(ctx, id)
		cancel()
		if err != nil {
			response.Housekeeping = append(response.Housekeeping, "cleaning up a resolved change: "+err.Error())
		}
	}
	return response, nil
}
func refreshRestoreResults(results []protocol.Result, outcomes []protocol.OwedRestore) []protocol.Result {
	results = slices.Clone(results)
	for i := range results {
		r := &results[i]
		if r.OwedRestore != nil {
			for _, outcome := range outcomes {
				if r.OwedRestore.OwedRestoreID == outcome.OwedRestoreID {
					r.OwedRestore = &outcome
					break
				}
			}
		}
		r.Items = refreshRestoreResults(r.Items, outcomes)
		r.Tries = refreshRestoreResults(r.Tries, outcomes)
	}
	return results
}

// A job cannot expire until every referenced restore is resolved, cleaned up,
// and copied into its terminal result. Lock files are deleted only while their
// restore execution lock is held, never while holding owed_restores.lock first.
func (s Server) expireOwedRestores(jobID string) (bool, error) {
	var records []owedRestoreRecord
	if err := s.withOwedRestores(context.Background(), func() error {
		var err error
		records, err = s.owedRestoreRecords()
		return err
	}); err != nil {
		return false, err
	}
	for _, record := range records {
		if record.NodeJobID == jobID && !record.CleanupComplete {
			return false, nil
		}
	}
	for _, record := range records {
		if record.NodeJobID != jobID {
			continue
		}
		held, err := lock(filepath.Join(s.restoreDirectory(record.Outcome.OwedRestoreID), "restore_execution.lock"))
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		err = s.withOwedRestores(context.Background(), func() error {
			if err := os.RemoveAll(s.restoreDirectory(record.Outcome.OwedRestoreID)); err != nil {
				return err
			}
			return syncDirectory(filepath.Join(s.Root, "owed_restores"))
		})
		unlock(held)
		if err != nil {
			return false, err
		}
	}
	return true, nil
}

// A killed or timed-out restore cannot write its own failure. The process
// holds restore_execution.lock BEFORE publishing the running phase. A free
// lock and that phase therefore prove an interrupted attempt. Never repeat
// it from inspection; an administrator or the next boot chooses the retry.
func (s Server) reconcileInterruptedRestore(ctx context.Context, id string) (*protocol.OwedRestore, error) {
	held, err := lock(filepath.Join(s.restoreDirectory(id), "restore_execution.lock"))
	if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer unlock(held)
	var outcome *protocol.OwedRestore
	err = s.withOwedRestores(ctx, func() error {
		record, err := s.readOwedRestore(id)
		if err != nil {
			return err
		}
		if record.Phase != restoringConfig {
			return nil
		}
		record.Phase = restoreFailed
		record.Outcome.RestoreFailureReason = "previous configuration restore process ended before recording completion"
		if err := s.writeOwedRestore(&record); err != nil {
			return err
		}
		outcome = &record.Outcome
		return nil
	})
	return outcome, err
}

func missingReachabilityReason(record owedRestoreRecord) string {
	var missing []string
	for _, kind := range record.BeforeChangeConfirmedConnectionKinds {
		if !slices.Contains(record.AfterChangeConfirmedConnectionKinds, kind) {
			missing = append(missing, strings.ToUpper(string(kind)))
		}
	}
	var connections []string
	for _, kind := range missing {
		connections = append(connections, "a new "+kind+" connection")
	}
	duration := record.ConfirmationWaitDuration
	if strings.HasSuffix(duration, "m0s") {
		duration = strings.TrimSuffix(duration, "0s")
	}
	return fmt.Sprintf("No reachability confirmation arrived over %s within %s.", strings.Join(connections, " or "), duration)
}

// A module-data-only record names its subject without a destination file.
func (r owedRestoreRecord) subject() string {
	if r.Outcome.DestinationFile == "" {
		return r.Outcome.ChangeSubject
	}
	return r.Outcome.DestinationFile
}
