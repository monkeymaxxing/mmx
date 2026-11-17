package controller

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"syscall"
	"time"

	"mmx/internal/config"
	"mmx/internal/durable"
	"mmx/internal/protocol"
)

// One process writes a run's progress. Inspection during execution is a live
// view; the executing controller remains responsible for saving its entries.
// A held lock returns nil.
func (c Controller) lockRun(id string) (*os.File, error) {
	f, err := os.OpenFile(filepath.Join(c.runDir(id), "control.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, nil
		}
		return nil, err
	}
	return f, nil
}
func unlockRun(f *os.File) {
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	_ = f.Close()
}
func (c Controller) requestStop(id string) error {
	return durable.Write(filepath.Join(c.runDir(id), "stop"), nil, 0600)
}
func (c Controller) stopRequested(id string) (bool, error) {
	_, err := os.Stat(filepath.Join(c.runDir(id), "stop"))
	if os.IsNotExist(err) {
		return false, nil
	}
	return err == nil, err
}

// Inspect refreshes admitted jobs only. It never submits a job or advances a
// fleet stage. A stop marker also reaches an active local scheduler and admissions
// that race this command's snapshot.
func (c Controller) Inspect(ctx context.Context, r *Run, stop bool) error {
	c.bindSecrets(r)
	lease, err := c.lockHistory(false)
	if err != nil {
		return err
	}
	defer unlockRun(lease)
	if stop {
		if err := c.requestStop(r.ID); err != nil {
			return err
		}
	}
	lock, err := c.lockRun(r.ID)
	if err != nil {
		return err
	}
	if lock != nil {
		defer unlockRun(lock)
	}
	if err := c.loadProgress(r); err != nil {
		return err
	}
	type result struct {
		index int
		entry Entry
		err   error
	}
	results := make(chan result)
	limit := r.Project.MaxNodesAtOnce
	if limit < 1 {
		limit = 1
	}
	next, active := 0, 0
	var failure error
	for next < len(r.Entries) || active > 0 {
		for next < len(r.Entries) && active < limit {
			i := next
			next++
			en := r.Entries[i]
			if en.JobID == "" || !protocol.Uncertain(en.Response.Status) && !hasUnresolvedRestore(en.Response) {
				continue
			}
			active++
			go func() {
				var save func(Entry) error
				if lock != nil {
					save = func(en Entry) error { return c.saveEntry(r.ID, en) }
				}
				en, err := c.inspectEntry(ctx, r, en, stop, save)
				results <- result{i, en, err}
			}()
		}
		if active == 0 {
			break
		}
		got := <-results
		active--
		r.Entries[got.index] = got.entry
		failure = errors.Join(failure, got.err)
	}
	r.Status = runStatus(r, lock == nil)
	if lock != nil {
		failure = errors.Join(failure, c.saveStatus(r))
	}
	return failure
}

func (c Controller) inspectEntry(ctx context.Context, r *Run, en Entry, stop bool, save func(Entry) error) (Entry, error) {
	conn, err := c.connect(ctx, r, en.Node, en.RunnerHash, machine{en.NodeID, en.Architecture})
	if err == nil {
		defer conn.Close()
		var response protocol.Response
		if stop {
			response, err = conn.Stop(ctx, en.JobID)
		} else {
			response, err = conn.Status(ctx, en.JobID)
		}
		en.Transport, en.ConnectionFallbackReason = conn.ConnectionUsed(), conn.FallbackReason
		if err == nil {
			en.Response = withHousekeeping(en.Response, response)
			addInventoryFailures(&en.Response, en.ControllerMachineReachabilityFailures)
			if save != nil {
				en = c.fetchDiffs(ctx, r, en, conn)
			}
		}
	}
	if err != nil && protocol.Terminal(en.Response.Status) && hasUnresolvedRestore(en.Response) {
		// A failed refresh cannot erase the node job's known failure or its saved
		// recovery outcome. Report the inspection failure separately to the caller.
		failure := fmt.Errorf("cannot refresh owed restore outcomes for node job %s: %w", en.JobID, err)
		if save != nil {
			failure = errors.Join(failure, save(en))
		}
		return en, failure
	}
	var replaced *replacedError
	switch {
	case errors.As(err, &replaced):
		en = onReplacedMachine(en, replaced)
	case err != nil:
		en.Response.Status, en.Response.Error = "unknown", err.Error()
	}
	// Remote contact failures are part of the result, not a reason to omit the
	// other nodes. Their saved partial results remain available.
	if save != nil {
		return en, save(en)
	}
	return en, nil
}

func runStatus(r *Run, controllerActive bool) string {
	if !hasUnfinished(r) {
		return "succeeded"
	}
	for _, en := range r.Entries {
		controllerActive = controllerActive || protocol.Uncertain(en.Response.Status)
	}
	if controllerActive {
		if r.StopRequested {
			return "stopping"
		}
		return "running"
	}
	// A node failure outranks a stop; blocked work and unresolved selections do not.
	for _, en := range r.Entries {
		if en.Response.Status == "failed" {
			return "failed"
		}
	}
	if r.StopRequested {
		return "stopped"
	}
	for _, sel := range r.Selections {
		if protocol.Failure(sel.Response.Status) || sel.controllerMachineFileFailed() {
			return "failed"
		}
		for _, response := range sel.Iterations {
			if protocol.Failure(response.Status) {
				return "failed"
			}
		}
	}
	for _, en := range r.Entries {
		if protocol.Failure(en.Response.Status) {
			return "failed"
		}
	}
	return "interrupted"
}

// waitEntry follows a running job until it finishes. Each progress request says
// what the entry holds, and the node answers with newer results once something
// changes; the node spaces its running answers, so the next request follows at
// once and a finished job is seen as soon as it finishes. A stop interrupts a
// waiting request so the node hears it promptly; the stop uses a separate
// runner command, and cancellation of the terminal never kills a package
// install or shell task. Only changes are saved. A job that ends rebooting is
// followed through its reboot.
func (c Controller) waitEntry(ctx context.Context, conn *Connection, r *Run, fleetStage config.FleetStage, en Entry, save func(Entry) error) (Entry, error) {
	var confirmations *confirmationFollower
	var window confirmationWindow
	defer func() {
		if confirmations != nil {
			confirmations.close()
		}
	}()
	observe := func(response *protocol.Response, started time.Time, failed bool) confirmationWindow {
		if confirmations == nil && response != nil && response.PendingReachabilityConfirmation != nil {
			inventory, err := c.controllerInventoryReachability(r, en.Node, bootedMachine{conn.NodeID, conn.BootID})
			if err != nil {
				return confirmationWindow{err: err}
			}
			confirmations = followConfirmations(ctx, r.stopped, en.JobID, r.Inventory.Nodes[en.Node].Transport == "ssh_only", conn.ConfirmReachability, conn.StatusOnNewConnection, inventory)
		}
		if confirmations != nil {
			window = confirmations.observe(response, started, failed)
			if len(window.inventoryFailures) > 0 {
				en.ControllerMachineReachabilityFailures = maps.Clone(en.ControllerMachineReachabilityFailures)
				if en.ControllerMachineReachabilityFailures == nil {
					en.ControllerMachineReachabilityFailures = map[string]string{}
				}
				maps.Copy(en.ControllerMachineReachabilityFailures, window.inventoryFailures)
			}
			if response != nil {
				addInventoryFailures(response, en.ControllerMachineReachabilityFailures)
			}
		} else if response != nil && restoreInProgress(*response) && window.deadline.IsZero() {
			window.deadline = time.Now().Add(restoreCollectionAllowance)
		}
		return window
	}
	started := en.responseRequestedAt
	if started.IsZero() {
		started = time.Now()
	}
	window = observe(&en.Response, started, false)
	if window.err != nil {
		return en, window.err
	}
	var saved protocol.Response
	lostContact := func(cause error) error {
		owedRestoreID := window.owedRestoreID
		if owedRestoreID == "" {
			for _, restore := range en.Response.OwedRestores {
				if restore.RestoreStatus == "required" || restore.RestoreStatus == "running" {
					owedRestoreID = restore.OwedRestoreID
					break
				}
			}
		}
		changeLabel := "change"
		for _, restore := range en.Response.OwedRestores {
			if restore.OwedRestoreID == owedRestoreID {
				if restore.DestinationFile != "" {
					changeLabel = "file change"
				} else {
					changeLabel = restore.ChangeSubject + " change"
				}
			}
		}
		if reason := en.ControllerMachineReachabilityFailures[owedRestoreID]; reason != "" {
			cause = fmt.Errorf("%s %w", reason, cause)
		}
		return fmt.Errorf("cannot collect node job %s after %s %s; restore completion is unconfirmed; run mmx status %s to inspect it again: %w", en.JobID, changeLabel, owedRestoreID, r.ID, cause)
	}
	pause := func() error {
		wait := reachabilityProgressPoll
		if !window.deadline.IsZero() {
			wait = min(wait, max(0, time.Until(window.deadline)))
		}
		timer := time.NewTimer(wait)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return nil
		}
	}
	for {
		en.Transport, en.ConnectionFallbackReason = conn.ConnectionUsed(), conn.FallbackReason
		if r.stopping() && !en.Response.StopRequested && protocol.Uncertain(en.Response.Status) {
			started := time.Now()
			stopContext, cancel := context.WithCancel(ctx)
			if !window.deadline.IsZero() {
				cancel()
				stopContext, cancel = context.WithDeadline(ctx, minTime(window.deadline, started.Add(confirmationAttemptLimit)))
			}
			response, err := conn.Stop(stopContext, en.JobID)
			if err != nil && stopContext.Err() != nil {
				err = stopContext.Err()
			}
			cancel()
			if err != nil {
				window = observe(nil, started, true)
				if transportFailure(err) && time.Now().Before(window.deadline) {
					if err := pause(); err != nil {
						return en, err
					}
					continue
				}
				if window.owedRestoreID != "" || restoreInProgress(en.Response) {
					return en, lostContact(err)
				}
				return en, err
			}
			en.Response = withHousekeeping(en.Response, response)
			window = observe(&en.Response, started, false)
			if window.err != nil {
				return en, window.err
			}
		}
		if !reflect.DeepEqual(saved, en.Response) {
			if err := save(en); err != nil {
				return en, err
			}
			saved = en.Response
		}
		if en.Response.Status == "rebooting" {
			return c.awaitReboot(ctx, conn, r, fleetStage, en, save)
		}
		if en.Response.Status != "running" && !restoreInProgress(en.Response) {
			if protocol.Terminal(en.Response.Status) {
				en = c.fetchDiffs(ctx, r, en, conn)
				return en, save(en)
			}
			return en, nil
		}
		if err := ctx.Err(); err != nil {
			return en, err
		}
		wait, cancel := context.WithCancel(ctx)
		if !window.deadline.IsZero() {
			cancel()
			deadline := minTime(window.deadline, time.Now().Add(confirmationAttemptLimit))
			if time.Now().Before(window.confirmationDeadline) {
				deadline = minTime(deadline, window.confirmationDeadline)
			}
			wait, cancel = context.WithDeadline(ctx, deadline)
		}
		if !en.Response.StopRequested && protocol.Uncertain(en.Response.Status) {
			go func() {
				select {
				case <-r.stopped:
					cancel()
				case <-wait.Done():
				}
			}()
		}
		request := protocol.ProgressRequest{ID: en.JobID, Results: len(en.Response.Results), CurrentTask: en.Response.CurrentTask, KnownReachabilityConfirmation: en.Response.PendingReachabilityConfirmation}
		started := time.Now()
		var response protocol.Response
		var err error
		if protocol.Terminal(en.Response.Status) {
			response, err = conn.Status(wait, en.JobID)
		} else {
			response, err = conn.Progress(wait, request)
		}
		if err != nil && wait.Err() != nil {
			err = wait.Err()
		}
		cancel()
		if err != nil && ctx.Err() == nil && r.stopping() && !en.Response.StopRequested && protocol.Uncertain(en.Response.Status) {
			continue // the stop interrupted this request; deliver it
		}
		if err != nil && ctx.Err() == nil && transportFailure(err) {
			window = observe(nil, started, true)
			if time.Now().Before(window.deadline) {
				if err := pause(); err != nil {
					return en, err
				}
				continue
			}
			if window.owedRestoreID != "" || restoreInProgress(en.Response) {
				return en, lostContact(err)
			}
		}
		if err != nil && ctx.Err() == nil && endsInReboot(fleetStage) && transportFailure(err) {
			response, err = statusAfterLostContact(ctx, conn, rebootWaitLimit(r, fleetStage, en.Node), en.JobID, err)
		}
		if err != nil {
			return en, err
		}
		next, err := mergeProgress(en.Response, response)
		if err != nil {
			return en, err
		}
		en.Response = withHousekeeping(en.Response, next)
		window = observe(&en.Response, started, false)
		if window.err != nil {
			return en, window.err
		}
		if restoreInProgress(en.Response) && protocol.Terminal(en.Response.Status) {
			if !time.Now().Before(window.deadline) {
				return en, lostContact(fmt.Errorf("restore service has not recorded completion"))
			}
			if err := pause(); err != nil {
				return en, err
			}
		}
	}
}

// rebootPoll spaces the status requests of a reboot wait.
var rebootPoll = time.Second

func endsInReboot(fleetStage config.FleetStage) bool {
	return len(fleetStage.NodeTasks) > 0 && fleetStage.NodeTasks[len(fleetStage.NodeTasks)-1].CoreModule().EndsTheNodeJob
}

// statusAfterLostContact asks again for the status of a job whose node stopped
// answering, until the node answers, gives a definite error, or the reboot
// task's timeout passes: a node that reboots can go down before the
// controller's next progress request hears of the reboot, for example when the
// network drops that request.
func statusAfterLostContact(ctx context.Context, conn *Connection, timeout time.Duration, jobID string, lost error) (protocol.Response, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return protocol.Response{}, ctx.Err()
		case <-time.After(rebootPoll):
		}
		response, err := conn.Status(ctx, jobID)
		if err == nil || !transportFailure(err) {
			return response, err
		}
		lost = err
	}
	return protocol.Response{}, fmt.Errorf("node stopped answering and did not answer again within the reboot task's %s: %w", timeout, lost)
}

// awaitReboot follows a rebooting job until its node answers on a new boot,
// which the node records as the reboot task's success. The node keeps its
// slot meanwhile. Contact fails while the node restarts, so the wait asks
// again until the reboot task's timeout, when the node fleet stage fails; the
// response keeps RebootFrom, so a retry asks the node again. Only a fleet stage's
// last task reboots, and load checked its timeout.
func (c Controller) awaitReboot(ctx context.Context, conn *Connection, r *Run, fleetStage config.FleetStage, en Entry, save func(Entry) error) (Entry, error) {
	timeout := rebootWaitLimit(r, fleetStage, en.Node)
	deadline := time.Now().Add(timeout)
	var contact error
	for en.Response.Status == "rebooting" {
		if !time.Now().Before(deadline) {
			en.Response.Status = "failed"
			en.Response.Error = fmt.Sprintf("node did not return on a new boot within %s", timeout)
			if contact != nil {
				en.Response.Error += "; last contact: " + contact.Error()
			}
			return en, save(en)
		}
		select {
		case <-ctx.Done():
			return en, ctx.Err()
		case <-time.After(rebootPoll):
		}
		response, err := conn.Status(ctx, en.JobID)
		if contact = err; err == nil {
			en.Response = withHousekeeping(en.Response, response)
		}
	}
	en.Transport, en.ConnectionFallbackReason = conn.ConnectionUsed(), conn.FallbackReason
	if protocol.Terminal(en.Response.Status) {
		en = c.fetchDiffs(ctx, r, en, conn)
	}
	return en, save(en)
}

// mergeProgress completes a progress answer that omits results the entry
// already holds.
func mergeProgress(held, answer protocol.Response) (protocol.Response, error) {
	if answer.ResultsAfter == 0 {
		return answer, nil
	}
	if answer.ResultsAfter != len(held.Results) {
		return held, fmt.Errorf("node progress starts after %d results, but the controller process has %d results", answer.ResultsAfter, len(held.Results))
	}
	answer.Results = append(append([]protocol.Result(nil), held.Results...), answer.Results...)
	if answer.Facts == nil {
		answer.Facts = held.Facts
	}
	answer.ResultsAfter = 0
	return answer, nil
}

// rebootWaitLimit is the wait_at_most of the reboot that ends the fleet
// stage's node job on the node, with the node's value of a var it reads
// (config.ResolveDurations). Loading the project and preparing the node job
// checked it, so a duration that fails here waits no time.
func rebootWaitLimit(r *Run, fleetStage config.FleetStage, managedNode string) time.Duration {
	reboot, err := config.ResolveDurations(fleetStage.NodeTasks[len(fleetStage.NodeTasks)-1:], config.DurationScope(r.Project, r.Inventory, managedNode))
	if err != nil {
		return 0
	}
	waits, ok := reboot[0].ModuleInputs.(interface{ Wait() (time.Duration, error) })
	if !ok {
		return 0
	}
	timeout, _ := waits.Wait()
	return timeout
}
