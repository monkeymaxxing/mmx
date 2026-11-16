package controller

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"mmx/internal/protocol"
)

const confirmationInterval = 10 * time.Second

// confirmationAttemptLimit bounds one confirmation attempt, and one progress
// or stop request while a reachability confirmation is pending.
var confirmationAttemptLimit = 10 * time.Second

// Allow the node's 330-second restore allowance and one more collection poll.
var restoreCollectionAllowance = 340 * time.Second

var reachabilityProgressPoll = 10 * time.Second

type confirmationAttempt struct {
	request           protocol.ReachabilityConfirmationRequest
	kind              protocol.ConnectionKind
	started, deadline time.Time
	askReadiness      bool
}
type confirmationAnswer struct {
	attempt confirmationAttempt
	answer  protocol.ReachabilityConfirmationResponse
	status  protocol.Response
	err     error
}
type confirmationPhaseState struct {
	confirmed   map[protocol.ConnectionKind]bool
	refused     map[protocol.ConnectionKind]bool
	lastStarted map[protocol.ConnectionKind]time.Time
}
type confirmationSchedule struct {
	inventory                                            *inventoryReachability
	jobID, owedRestoreID                                 string
	sshOnly                                              bool
	beforeKinds, afterKinds                              []protocol.ConnectionKind
	beforeDeadline, afterDeadline, collectionDeadline    time.Time
	afterKnown, beforeCompleted, progressFailed, stopped bool
	phases                                               map[protocol.ConfirmationPhase]*confirmationPhaseState
	readiness                                            *confirmationPhaseState
}

func newConfirmationPhaseState() *confirmationPhaseState {
	return &confirmationPhaseState{confirmed: map[protocol.ConnectionKind]bool{}, refused: map[protocol.ConnectionKind]bool{}, lastStarted: map[protocol.ConnectionKind]time.Time{}}
}

func earlierDeadline(held, next time.Time) time.Time {
	if held.IsZero() || next.Before(held) {
		return next
	}
	return held
}

func (s *confirmationSchedule) observe(p *protocol.PendingReachabilityConfirmation, started, received time.Time) error {
	if p == nil {
		return nil
	}
	if err := p.Validate(); err != nil {
		return err
	}
	if p.NodeJobID != s.jobID {
		return fmt.Errorf("pending reachability confirmation belongs to another node job")
	}
	if s.sshOnly && slices.Contains(p.ConnectionKindsToConfirm, protocol.ConnectionHTTPS) {
		return fmt.Errorf("node with connection: ssh_only requested HTTPS reachability confirmation")
	}
	if s.owedRestoreID != p.OwedRestoreID {
		*s = confirmationSchedule{jobID: s.jobID, sshOnly: s.sshOnly, stopped: s.stopped, inventory: s.inventory.newChange(), owedRestoreID: p.OwedRestoreID,
			phases: map[protocol.ConfirmationPhase]*confirmationPhaseState{}}
		for _, phase := range []protocol.ConfirmationPhase{protocol.ConfirmationBeforeChange, protocol.ConfirmationAfterChange} {
			s.phases[phase] = newConfirmationPhaseState()
		}
		s.readiness = newConfirmationPhaseState()
	}
	// Request start is conservative for confirmations; receipt leaves the full
	// node restore allowance for result collection. Neither deadline is extended.
	deadline := started.Add(p.ConfirmationDeadline.Sub(p.NodeTime))
	latest := started.Add(p.LatestAfterChangeConfirmationDeadline.Sub(p.NodeTime))
	collection := received.Add(p.LatestAfterChangeConfirmationDeadline.Sub(p.NodeTime)).Add(restoreCollectionAllowance)
	if p.ConfirmationPhase == protocol.ConfirmationAfterChange {
		if !s.afterKnown && s.inventory != nil && !s.inventory.ready(false) {
			// Resume can observe confirmations counted by an earlier process.
			// Never form a before-change set from probes that finish after activation.
			s.inventory.beforeObserved = false
		}
		if s.afterKnown && !sameConnectionKinds(s.afterKinds, p.ConnectionKindsToConfirm) {
			return fmt.Errorf("node changed which connections require reachability confirmation after the file change")
		}
		if len(s.beforeKinds) > 0 {
			for _, kind := range p.ConnectionKindsToConfirm {
				if !slices.Contains(s.beforeKinds, kind) {
					return fmt.Errorf("node added a connection kind after the before-change confirmation phase")
				}
			}
		}
		s.afterKnown, s.beforeCompleted = true, true
		s.afterKinds = slices.Clone(p.ConnectionKindsToConfirm)
		s.afterDeadline = earlierDeadline(s.afterDeadline, deadline)
		collection = received.Add(p.ConfirmationDeadline.Sub(p.NodeTime)).Add(restoreCollectionAllowance)
	} else if !s.afterKnown {
		s.beforeKinds = slices.Clone(p.ConnectionKindsToConfirm)
		s.beforeDeadline = earlierDeadline(s.beforeDeadline, deadline)
		s.afterDeadline = earlierDeadline(s.afterDeadline, latest)
	}
	s.collectionDeadline = earlierDeadline(s.collectionDeadline, collection)
	phase := s.phases[p.ConfirmationPhase]
	for _, kind := range p.ConfirmedConnectionKinds {
		phase.confirmed[kind] = true //nolint:nilaway // Validated restore phases allocate schedule state; serialized updates reject stale restore IDs.
	}
	if p.ConfirmationPhase == protocol.ConfirmationBeforeChange {
		s.beforeCompleted = s.beforeCompleted || s.allConfirmed(protocol.ConfirmationBeforeChange, s.beforeKinds)
	}
	return nil
}
func sameConnectionKinds(a, b []protocol.ConnectionKind) bool {
	if len(a) != len(b) {
		return false
	}
	for _, kind := range a {
		if !slices.Contains(b, kind) {
			return false
		}
	}
	return true
}
func (s *confirmationSchedule) allConfirmed(phase protocol.ConfirmationPhase, kinds []protocol.ConnectionKind) bool {
	if len(kinds) == 0 {
		return false
	}
	for _, kind := range kinds {
		//nolint:nilaway // Validated restore phases allocate schedule state; serialized updates reject stale restore IDs.
		if !s.phases[phase].confirmed[kind] {
			return false
		}
	}
	return true
}

// due returns the next phase's eligible attempts and the next scheduling time.
// A phase's first attempt is immediate; its repeats are ten seconds apart.
// The caller keeps one in-flight attempt per kind, across phase/change switches.
func (s *confirmationSchedule) due(now time.Time, busy map[protocol.ConnectionKind]bool) ([]confirmationAttempt, time.Time) {
	if s.stopped || s.owedRestoreID == "" {
		return nil, time.Time{}
	}
	phase, kinds, deadline := protocol.ConfirmationBeforeChange, s.beforeKinds, s.beforeDeadline
	askReadiness := !s.afterKnown && s.progressFailed && (s.beforeCompleted || !now.Before(s.beforeDeadline))
	if s.afterKnown || askReadiness {
		phase, deadline = protocol.ConfirmationAfterChange, s.afterDeadline
		kinds = s.afterKinds
		if !s.afterKnown {
			kinds = s.beforeKinds
		}
	}
	var next time.Time
	if phase == protocol.ConfirmationBeforeChange && s.progressFailed && now.Before(s.beforeDeadline) {
		next = s.beforeDeadline
	}
	var attempts []confirmationAttempt
	if !now.Before(deadline) {
		return attempts, next
	}
	if !askReadiness && !s.inventory.ready(phase == protocol.ConfirmationAfterChange) {
		return nil, next
	}
	state := s.phases[phase]
	if askReadiness {
		state = s.readiness
	}
	for _, kind := range kinds {
		//nolint:nilaway // Validated restore phases allocate schedule state; serialized updates reject stale restore IDs.
		if busy[kind] || state.confirmed[kind] || state.refused[kind] {
			continue
		}
		due := state.lastStarted[kind].Add(confirmationInterval) //nolint:nilaway // Validated restore phases allocate schedule state; serialized updates reject stale restore IDs.
		if now.Before(due) {
			if due.Before(deadline) {
				next = earlierDeadline(next, due)
			}
			continue
		}
		attempts = append(attempts, confirmationAttempt{request: protocol.ReachabilityConfirmationRequest{NodeJobID: s.jobID, OwedRestoreID: s.owedRestoreID, ConfirmationPhase: phase}, kind: kind, started: now, deadline: minTime(deadline, now.Add(confirmationAttemptLimit)), askReadiness: askReadiness})
		state.lastStarted[kind] = now
	}
	return attempts, next
}
func minTime(a, b time.Time) time.Time {
	if a.Before(b) {
		return a
	}
	return b
}

func (s *confirmationSchedule) answered(result confirmationAnswer, received time.Time) {
	a := result.attempt
	if a.request.OwedRestoreID != s.owedRestoreID {
		return
	}
	state := s.phases[a.request.ConfirmationPhase]
	if a.askReadiness {
		state = s.readiness
	}
	if result.err != nil {
		// An unanswered connection may retry; trust/authorization/protocol
		// rejection is final for this kind. Other kinds can still confirm.
		if !transportFailure(result.err) && !errors.Is(result.err, context.DeadlineExceeded) && !errors.Is(result.err, context.Canceled) {
			state.refused[a.kind] = true //nolint:nilaway // Validated restore phases allocate schedule state; serialized updates reject stale restore IDs.
		}
		return
	}
	if a.askReadiness {
		response := result.status
		if response.ID != s.jobID {
			state.refused[a.kind] = true
			return
		}
		if pending := response.PendingReachabilityConfirmation; pending != nil {
			if pending.OwedRestoreID != s.owedRestoreID || s.observe(pending, a.started, received) != nil {
				state.refused[a.kind] = true
			}
		}
		if response.StopRequested || protocol.Terminal(response.Status) {
			s.stopped = true
		}
		return
	}
	answer := result.answer
	if answer.NodeJobID != a.request.NodeJobID || answer.OwedRestoreID != a.request.OwedRestoreID || answer.ConfirmationPhase != a.request.ConfirmationPhase || answer.ReceivedConnectionKind != a.kind {
		state.refused[a.kind] = true
		return
	}
	if pending := answer.PendingReachabilityConfirmation; pending != nil {
		if pending.OwedRestoreID != s.owedRestoreID || s.observe(pending, a.started, received) != nil {
			state.refused[a.kind] = true
			return
		}
	}
	if answer.ConfirmationCounted {
		state.confirmed[answer.ReceivedConnectionKind] = true //nolint:nilaway // Validated restore phases allocate schedule state; serialized updates reject stale restore IDs.
	}
	if a.request.ConfirmationPhase == protocol.ConfirmationBeforeChange {
		s.beforeCompleted = s.beforeCompleted || s.allConfirmed(protocol.ConfirmationBeforeChange, s.beforeKinds)
	}
}

// inventoryReachability belongs to one change. Before-change answers select the
// required nodes; only fresh after-change answers can release confirmation.
type inventoryReachability struct {
	controllerMachine bootedMachine
	nodes             []string
	limit             int
	probe             func(context.Context, string) (bootedMachine, error)
	states            map[string]*inventoryProbeState
	beforeObserved    bool
}

type inventoryProbeState struct {
	beforeDone, afterDone bool
	// nodeID is the machine that answered before the change; set, it makes
	// the node required after the change.
	nodeID      string
	lastStarted time.Time
}

type inventoryProbeAttempt struct {
	name, owedRestoreID string
	phase               protocol.ConfirmationPhase
	deadline            time.Time
}

type inventoryProbeAnswer struct {
	attempt inventoryProbeAttempt
	machine bootedMachine
	err     error
}

func (g *inventoryReachability) newChange() *inventoryReachability {
	if g == nil {
		return nil
	}
	next := *g
	next.beforeObserved = false
	next.states = make(map[string]*inventoryProbeState, len(g.nodes))
	for _, name := range g.nodes {
		next.states[name] = &inventoryProbeState{}
	}
	return &next
}

func (g *inventoryReachability) ready(after bool) bool {
	if g == nil {
		return true
	}
	if !g.beforeObserved {
		return false // A resumed after-change wait has no trustworthy before-change set.
	}
	for _, state := range g.states {
		if !state.beforeDone || after && state.nodeID != "" && !state.afterDone {
			return false
		}
	}
	return true
}

func (s *confirmationSchedule) inventoryDue(now time.Time, busy map[string]bool) ([]inventoryProbeAttempt, time.Time) {
	g := s.inventory
	if g == nil || s.stopped || s.owedRestoreID == "" {
		return nil, time.Time{}
	}
	phase, deadline := protocol.ConfirmationBeforeChange, s.beforeDeadline
	if s.afterKnown {
		phase, deadline = protocol.ConfirmationAfterChange, s.afterDeadline
	} else {
		g.beforeObserved = true
	}
	if !now.Before(deadline) || !g.beforeObserved {
		return nil, time.Time{}
	}
	var attempts []inventoryProbeAttempt
	var next time.Time
	active := len(busy)
	//nolint:nilaway // A schedule with an owed restore got its inventory from newChange, which gives every name in g.nodes a state.
	for _, name := range g.nodes {
		state := g.states[name]
		if busy[name] || !s.afterKnown && state.beforeDone || s.afterKnown && (state.nodeID == "" || state.afterDone) {
			continue
		}
		if s.afterKnown {
			due := state.lastStarted.Add(confirmationInterval)
			if now.Before(due) {
				if due.Before(deadline) {
					next = earlierDeadline(next, due)
				}
				continue
			}
		}
		if active >= g.limit {
			continue
		}
		active++
		state.lastStarted = now
		attempts = append(attempts, inventoryProbeAttempt{name: name, owedRestoreID: s.owedRestoreID, phase: phase, deadline: minTime(deadline, now.Add(confirmationAttemptLimit))})
	}
	return attempts, next
}

func (s *confirmationSchedule) inventoryAnswered(answer inventoryProbeAnswer, now time.Time) {
	g, a := s.inventory, answer.attempt
	if g == nil || a.owedRestoreID != s.owedRestoreID {
		return
	}
	state := g.states[a.name]
	//nolint:nilaway // Attempts name only nodes in g.nodes, and newChange gives each a state; an attempt of an earlier change has another owed restore ID.
	if a.phase == protocol.ConfirmationBeforeChange {
		state.beforeDone = true
		state.lastStarted = time.Time{} // The after-change phase starts a new spacing interval.
		if answer.err == nil && now.Before(s.beforeDeadline) && answer.machine != g.controllerMachine {
			state.nodeID = answer.machine.NodeID
		}
	} else if answer.err == nil && now.Before(s.afterDeadline) && answer.machine.NodeID == state.nodeID {
		state.afterDone = true
	}
}

func (s *confirmationSchedule) inventoryFailure(now time.Time) string {
	g := s.inventory
	if g == nil || !s.afterKnown || now.Before(s.afterDeadline) || g.ready(true) {
		return ""
	}
	if !g.beforeObserved {
		return "The changed node is the controller machine. The controller process did not complete the before-change inventory probes, so MMX did not confirm the change."
	}
	var missing []string
	//nolint:nilaway // afterKnown means a change was observed, and its newChange gave every name in g.nodes a state.
	for _, name := range g.nodes {
		state := g.states[name]
		if state.nodeID != "" && !state.afterDone {
			missing = append(missing, name)
		}
	}
	if len(missing) == 0 {
		return ""
	}
	return "The changed node is the controller machine. After the change, the controller machine could not reach these nodes over new SSH connections: " + strings.Join(missing, ", ") + ". MMX did not confirm the change."
}

func (c Controller) controllerInventoryReachability(r *Run, node string, changed bootedMachine) (*inventoryReachability, error) {
	read := thisControllerMachine
	if c.controllerMachine != nil {
		read = c.controllerMachine
	}
	machine, err := read()
	if err != nil {
		return nil, err
	}
	// The controller machine is the changed node only in the same boot: a
	// clone of its image shares the machine ID but boots its own kernel.
	controllerMachine := bootedMachine{machine.MachineIDHash, machine.BootID}
	if machine.MachineIDHash == "" || controllerMachine != changed {
		return nil, nil
	}
	g := &inventoryReachability{controllerMachine: controllerMachine, limit: max(1, r.Project.MaxNodesAtOnce)}
	for name := range r.Inventory.Nodes {
		if name != node {
			g.nodes = append(g.nodes, name)
		}
	}
	slices.Sort(g.nodes)
	probe := c.probeInventorySSH
	if c.probeInventoryNode != nil {
		probe = c.probeInventoryNode
	}
	g.probe = func(ctx context.Context, name string) (bootedMachine, error) {
		return probe(ctx, r, name)
	}
	return g, nil
}

// addInventoryFailures adds controller observations to the node's failed task,
// retaining only the already-safe form of any secret-bearing node diagnostic.
func addInventoryFailures(response *protocol.Response, failures map[string]string) {
	if len(failures) == 0 {
		return
	}
	var visit func(*[]protocol.Result, bool) []string
	visit = func(results *[]protocol.Result, inheritedSecret bool) []string {
		// Earlier progress snapshots may still be displayed or saved concurrently.
		*results = slices.Clone(*results)
		var failuresBelow []string
		for i := range *results {
			r := &(*results)[i]
			reasons := visit(&r.Items, false)
			reasons = append(reasons, visit(&r.Tries, r.Secret || inheritedSecret)...)
			if r.OwedRestore != nil && protocol.Failure(r.Status) {
				if reason := failures[r.OwedRestore.OwedRestoreID]; reason != "" {
					reasons = append(reasons, reason)
				}
			}
			if protocol.Failure(r.Status) && len(reasons) > 0 {
				safe := *r
				safe.Secret = safe.Secret || inheritedSecret
				r.Reason = safe.Safe().Reason
				for _, reason := range reasons {
					if !strings.Contains(r.Reason, reason) {
						r.Reason = strings.TrimSpace(r.Reason + " " + reason)
					}
				}
				r.ReasonReadsNoSecret = true
			}
			failuresBelow = append(failuresBelow, reasons...)
		}
		return failuresBelow
	}
	visit(&response.Results, false)
}

type confirmationObservation struct {
	response *protocol.Response
	started  time.Time
	failed   bool
	reply    chan confirmationWindow
}
type confirmationWindow struct {
	inventoryFailures    map[string]string
	owedRestoreID        string
	deadline             time.Time
	confirmationDeadline time.Time
	err                  error
}

// confirmationFollower owns the schedule in one goroutine. Its request clients
// are independent of the ordinary Connection's progress/fallback state.
type confirmationFollower struct {
	updates chan confirmationObservation
	cancel  context.CancelFunc
	done    chan struct{}
}

func followConfirmations(ctx context.Context, stopped <-chan struct{}, jobID string, sshOnly bool,
	confirm func(context.Context, protocol.ReachabilityConfirmationRequest, protocol.ConnectionKind) (protocol.ReachabilityConfirmationResponse, error),
	status func(context.Context, string, protocol.ConnectionKind) (protocol.Response, error), inventory *inventoryReachability) *confirmationFollower {
	ctx, cancel := context.WithCancel(ctx)
	f := &confirmationFollower{updates: make(chan confirmationObservation), cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(f.done)
		confirmationContext, cancelConfirmations := context.WithCancel(ctx)
		defer cancelConfirmations()
		watchDone := make(chan struct{})
		defer close(watchDone)
		go func() {
			select {
			case <-stopped:
				cancelConfirmations()
			case <-watchDone:
			case <-ctx.Done():
			}
		}()
		var requests sync.WaitGroup
		defer requests.Wait()
		results := make(chan confirmationAnswer, 2)
		busy := map[protocol.ConnectionKind]bool{}
		cancels := map[protocol.ConnectionKind]context.CancelFunc{}
		inFlight := map[protocol.ConnectionKind]confirmationAttempt{}
		probeResults := make(chan inventoryProbeAnswer)
		probeBusy := map[string]bool{}
		probeCancels := map[string]context.CancelFunc{}
		failures := map[string]string{}
		schedule := confirmationSchedule{jobID: jobID, sshOnly: sshOnly, inventory: inventory}
		for {
			if confirmationContext.Err() != nil {
				schedule.stopped = true
			}
			now := time.Now()
			if reason := schedule.inventoryFailure(now); reason != "" {
				failures[schedule.owedRestoreID] = reason
			}
			probes, probeNext := schedule.inventoryDue(now, probeBusy)
			for _, attempt := range probes {
				probeContext, cancel := context.WithDeadline(confirmationContext, attempt.deadline)
				probeBusy[attempt.name], probeCancels[attempt.name] = true, cancel
				probe := schedule.inventory.probe
				requests.Add(1)
				go func() {
					defer requests.Done()
					defer cancel()
					machine, err := probe(probeContext, attempt.name)
					select {
					case probeResults <- inventoryProbeAnswer{attempt: attempt, machine: machine, err: err}:
					case <-ctx.Done():
					}
				}()
			}
			attempts, next := schedule.due(now, busy)
			if !probeNext.IsZero() {
				next = earlierDeadline(next, probeNext)
			}
			if schedule.inventory != nil && schedule.afterKnown && now.Before(schedule.afterDeadline) && !schedule.inventory.ready(true) {
				next = earlierDeadline(next, schedule.afterDeadline)
			}
			for _, attempt := range attempts {
				if confirmationContext.Err() != nil {
					break
				}
				attemptContext, cancel := context.WithDeadline(confirmationContext, attempt.deadline)
				busy[attempt.kind], cancels[attempt.kind] = true, cancel
				inFlight[attempt.kind] = attempt
				requests.Add(1)
				go func() {
					defer requests.Done()
					defer cancel()
					result := confirmationAnswer{attempt: attempt}
					if attempt.askReadiness {
						result.status, result.err = status(attemptContext, attempt.request.NodeJobID, attempt.kind)
					} else {
						result.answer, result.err = confirm(attemptContext, attempt.request, attempt.kind)
					}
					select {
					case results <- result:
					case <-ctx.Done():
					}
				}()
			}
			var timer *time.Timer
			var wake <-chan time.Time
			if !next.IsZero() {
				timer = time.NewTimer(max(0, time.Until(next)))
				wake = timer.C
			}
			select {
			case <-ctx.Done():
				if timer != nil {
					timer.Stop()
				}
				return
			case update := <-f.updates:
				if reason := schedule.inventoryFailure(time.Now()); reason != "" {
					failures[schedule.owedRestoreID] = reason
				}
				oldID := schedule.owedRestoreID
				var err error
				if update.response != nil {
					response := update.response
					schedule.progressFailed = false
					err = schedule.observe(response.PendingReachabilityConfirmation, update.started, time.Now())
					if response.PendingReachabilityConfirmation == nil && schedule.owedRestoreID != "" {
						schedule.beforeCompleted = true
						if changeFinished(response.Results, schedule.owedRestoreID) {
							schedule = confirmationSchedule{jobID: jobID, sshOnly: sshOnly, stopped: schedule.stopped, inventory: inventory}
						}
					}
					if response.StopRequested || protocol.Terminal(response.Status) {
						schedule.stopped = true
						cancelConfirmations()
					}
					if restoreInProgress(*response) && schedule.collectionDeadline.IsZero() {
						schedule.collectionDeadline = time.Now().Add(restoreCollectionAllowance)
					}
				} else if update.failed {
					schedule.progressFailed = true
				}
				if oldID != schedule.owedRestoreID {
					for _, cancel := range probeCancels {
						cancel()
					}
					for _, cancel := range cancels {
						cancel()
					}
				} else if update.response != nil && !schedule.afterKnown && update.response.PendingReachabilityConfirmation != nil {
					// Recovered progress supplies readiness again; stop asking on
					// separate connections while it still reports before-change.
					for kind, attempt := range inFlight {
						if attempt.askReadiness {
							cancels[kind]()
						}
					}
				}
				phaseDeadline := schedule.beforeDeadline
				if schedule.afterKnown {
					phaseDeadline = schedule.afterDeadline
				}
				update.reply <- confirmationWindow{owedRestoreID: schedule.owedRestoreID, deadline: schedule.collectionDeadline, confirmationDeadline: phaseDeadline, inventoryFailures: maps.Clone(failures), err: err}
			case result := <-probeResults:
				delete(probeBusy, result.attempt.name)
				delete(probeCancels, result.attempt.name)
				schedule.inventoryAnswered(result, time.Now())
			case result := <-results:
				busy[result.attempt.kind] = false
				delete(cancels, result.attempt.kind)
				delete(inFlight, result.attempt.kind)
				schedule.answered(result, time.Now())
				if schedule.stopped {
					cancelConfirmations()
				}
			case <-wake:
			}
			if timer != nil {
				timer.Stop()
			}
		}
	}()
	return f
}
func (f *confirmationFollower) observe(response *protocol.Response, started time.Time, failed bool) confirmationWindow {
	update := confirmationObservation{response: response, started: started, failed: failed, reply: make(chan confirmationWindow, 1)}
	select {
	case f.updates <- update:
	case <-f.done:
		return confirmationWindow{err: context.Canceled}
	}
	select {
	case window := <-update.reply:
		return window
	case <-f.done:
		return confirmationWindow{err: context.Canceled}
	}
}
func (f *confirmationFollower) close() { f.cancel(); <-f.done }

func changeFinished(results []protocol.Result, id string) bool {
	for _, result := range results {
		if restore := result.OwedRestore; restore != nil && restore.OwedRestoreID == id && restore.RestoreStatus != "required" && restore.RestoreStatus != "running" {
			return true
		}
		if changeFinished(result.Items, id) || changeFinished(result.Tries, id) {
			return true
		}
	}
	return false
}

func restoreInProgress(response protocol.Response) bool {
	for _, restore := range response.OwedRestores {
		if restore.RestoreStatus == "required" || restore.RestoreStatus == "running" {
			return true
		}
	}
	return false
}
func hasUnresolvedRestore(response protocol.Response) bool {
	if restoreInProgress(response) {
		return true
	}
	for _, restore := range response.OwedRestores {
		if restore.RestoreStatus == "failed" {
			return true
		}
	}
	return false
}
