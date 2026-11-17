package controller

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"mmx/internal/access"
	"mmx/internal/config"
	"mmx/internal/engine"
	"mmx/internal/facts"
	"mmx/internal/modules"
	"mmx/internal/protocol"
	"mmx/internal/release"
	"mmx/internal/secrets"
	"mmx/internal/value"
)

type Entry struct {
	// The node cannot report controller-machine probes. Keep these diagnoses by
	// owed restore ID so status refreshes retain the failed task's explanation.
	ControllerMachineReachabilityFailures map[string]string `json:"controller_machine_reachability_failures,omitempty"`

	responseRequestedAt time.Time // conservative clock anchor for a received confirmation deadline
	Housekeeping        []string  `json:"housekeeping,omitempty"`

	// FactsChanged is derived for display from saved observations.
	FactsChanged []string          `json:"facts_changed,omitempty"`
	Transfer     *TransferProgress `json:"transfer,omitempty"`
	scope        value.Scope
	// privateInputs tells the scheduler this attempt's job carries secret values.
	privateInputs bool
	// recorded is the machine this run's saved work reached on the node; resume
	// holds work that never contacted the node to it as well.
	recorded machine
	// earlierChange names, in a dryrun, the first node task of an earlier
	// fleet stage on the node that would change it (earlierChangeOnTheNode).
	earlierChange string

	Transport string `json:"connection_used,omitempty"`
	// ConnectionFallbackReason is set with connection_used: ssh_fallback.
	ConnectionFallbackReason string            `json:"connection_fallback_reason,omitempty"`
	Architecture             string            `json:"architecture,omitempty"`
	NodeID                   string            `json:"node_id,omitempty"`
	PreviousNodeJobAttempts  []Attempt         `json:"previous_node_job_attempts,omitempty"`
	FleetStage               string            `json:"fleet_stage"`
	Node                     string            `json:"node"`
	JobID                    string            `json:"job_id,omitempty"`
	NodeJobAttempt           int               `json:"node_job_attempt"`
	RunnerHash               string            `json:"runner_hash,omitempty"`
	Response                 protocol.Response `json:"response"`
	// WhenWasTrue records that the fleet stage's when let this node start, so
	// resume keeps that result instead of checking the condition again.
	WhenWasTrue bool `json:"when_was_true,omitempty"`
}
type Attempt struct {
	JobID    string            `json:"job_id"`
	Response protocol.Response `json:"response"`
}
type Run struct {
	// nodeIdentities snapshots saved entries, then records verified connections.
	// Probe goroutines never read Entries while the fleet scheduler updates Entries.
	nodeIdentities *sync.Map // inventory name -> node identity hash
	// machines records which inventory name's connection reached each machine
	// in its current boot first in this controller process.
	machines *sync.Map // bootedMachine -> inventory name

	// stopped closes when the scheduler first sees a stop request; node workers
	// read it instead of each checking the stop file.
	stopped chan struct{}

	SecretsRefreshed bool                  `json:"secret_inputs_refreshed,omitempty"`
	Selections       map[string]*Selection `json:"selections,omitempty"`
	// FleetStagesAwaitingNodeSelection is set while a fleet stage's nodes or
	// loop items are not resolved yet, or the walk has not reached them.
	FleetStagesAwaitingNodeSelection bool `json:"fleet_stages_awaiting_node_selection,omitempty"`

	Finished      *time.Time `json:"finished,omitempty"`
	StopRequested bool       `json:"stop_requested,omitempty"`
	EntryPoint    string     `json:"entry_point,omitempty"`
	InventoryName string     `json:"inventory_name,omitempty"`
	// Limit is the run's --limit selectors; it narrows every selection, and
	// resume reuses it.
	Limit []string `json:"limit,omitempty"`
	// HelperNodes are the nodes outside the limit that the run showed before
	// connecting, with the for_each_node iterations' fleet stages that work on
	// them. Only these fleet stages reach a node outside the limit.
	HelperNodes []config.HelperNode `json:"helper_nodes,omitempty"`
	// MMXVersion is the MMX release that started the run; Load refuses to
	// resume the run with another release. A run saved before this field
	// existed has none, and resumes as before.
	MMXVersion string           `json:"mmx_version,omitempty"`
	ID         string           `json:"id"`
	Created    time.Time        `json:"created"`
	Status     string           `json:"status"`
	Dryrun     bool             `json:"dryrun"`
	Project    config.Project   `json:"project"`
	Inventory  config.Inventory `json:"inventory"`
	Entries    []Entry          `json:"entries"`
}

// stopping reports whether the scheduler has seen a stop request.
func (r *Run) stopping() bool {
	select {
	case <-r.stopped:
		return true
	default:
		return false
	}
}

// Identity records the project settings file, project, inventory name, and
// playbook name. Resume, status, results, and pruning select saved runs by it.
type Identity struct{ EntryPoint, Project, Inventory, Playbook string }

// RunCommandEntryPoint records an mmx run-command run in place of a project file, so the
// default selection for resume, status, stop, and results stays with playbooks.
const RunCommandEntryPoint = "run-command"

func (r *Run) Identity() Identity {
	return Identity{r.EntryPoint, r.Project.Project, r.InventoryName, r.Project.PlaybookName}
}

// Selects reports whether a saved run belongs to this selection. An empty
// field matches any run.
func (i Identity) Selects(r *Run) bool {
	own := r.Identity()
	return (i.Playbook == "" || i.Playbook == own.Playbook) && (i.EntryPoint == "" || i.EntryPoint == own.EntryPoint) && (i.Project == "" || i.Project == own.Project) && (i.Inventory == "" || i.Inventory == own.Inventory)
}

type Controller struct {
	Secrets     *secrets.Store
	Credentials *access.Credentials
	Selection   Identity
	executeNode func(context.Context, *Run, config.FleetStage, Entry, bool, func(Entry) error) (Entry, error)
	// nodeFacts replaces factsOfNode in tests.
	nodeFacts func(context.Context, *Run, Entry, bool) (facts.Snapshot, error)
	State     string
	RunnerDir string
	Progress  func(*Run)

	// recordNode replaces recordOnNode, which writes a node's last successful
	// run record, in tests.
	recordNode func(context.Context, *Run, Entry, protocol.LastSuccessfulRunRequest) error
	// probeInventoryNode replaces the read-only fresh SSH probe in tests.
	probeInventoryNode func(context.Context, *Run, string) (bootedMachine, error)
	// controllerMachine replaces thisControllerMachine in tests.
	controllerMachine func() (protocol.ControllerMachine, error)
}

// thisControllerMachine reads, once for the process, what tells a node that
// it is the machine this controller process runs on.
var thisControllerMachine = sync.OnceValues(func() (protocol.ControllerMachine, error) {
	return readControllerMachine(runtime.GOOS, facts.BootID, facts.MachineIDHash)
})

// readControllerMachine reads the kernel's boot ID of the controller machine
// and the hash of its systemd machine ID. A machine that does not run Linux,
// such as a Mac, has no boot ID; its empty boot ID matches no node. Every
// Linux machine has one, so a failed read there is an error: with an empty
// boot ID no node would recognize the controller machine, and nothing would
// say that the guard is off. A machine ID that cannot be read stays empty,
// because a container from an image without /etc/machine-id has none, and
// its server still recognizes it by the boot ID.
func readControllerMachine(system string, bootID, machineIDHash func() (string, error)) (protocol.ControllerMachine, error) {
	boot, err := bootID()
	if err != nil {
		if system == "linux" {
			return protocol.ControllerMachine{}, fmt.Errorf("reading the boot ID of the controller machine: %w", err)
		}
		boot = ""
	}
	machine, _ := machineIDHash()
	return protocol.ControllerMachine{BootID: boot, MachineIDHash: machine}, nil
}

// New creates a run of the current project. The caller has checked the limit
// and the project's --only-fleet-stages value with config.Inspect.
func (c Controller) New(ctx context.Context, p config.Project, inv config.Inventory, limit []string, dryrun bool) (*Run, error) {
	lease, err := c.lockHistory(false)
	if err != nil {
		return nil, err
	}
	defer unlockRun(lease)
	var nonce [8]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, err
	}
	kept, err := config.NewLimit(inv, limit)
	if err != nil {
		return nil, err
	}
	r := &Run{ID: time.Now().UTC().Format("20060102T150405.000000000") + "-" + hex.EncodeToString(nonce[:]), Created: time.Now().UTC(), MMXVersion: release.Version, Status: "running", Dryrun: dryrun, Project: p, Inventory: inv, EntryPoint: c.Selection.EntryPoint, InventoryName: c.Selection.Inventory, Limit: limit}
	if kept != nil {
		// The same local view the caller checked the limit with; a limit it
		// rejects shows no helper node, so none is reached.
		if inspection, err := config.Inspect(p, inv, limit); err == nil {
			r.HelperNodes = inspection.HelperNodes
		}
	}
	r.Selections = map[string]*Selection{}
	for i, s := range p.FleetStages {
		if !p.KeepsFleetStage(s.ID) {
			continue
		}
		if s.Loop != nil || s.ForEachNode != nil || modules.Dynamic(s.Nodes) || modules.Dynamic(s.ExceptNodes) {
			r.FleetStagesAwaitingNodeSelection = true
			continue
		}
		nodes, err := config.SelectNodes(inv, value.Scope{}, s)
		if err != nil {
			return nil, err
		}
		selection := &Selection{Path: s.ID, Resolved: true, PlaybookPosition: []int{i}, LeftOutBecauseItSelectsNoNodes: len(nodes) == 0}
		selection.Nodes, selection.LeftOut = kept.Narrow(nodes)
		r.Selections[s.ID] = selection
		for _, h := range selection.Nodes {
			r.Entries = append(r.Entries, Entry{FleetStage: s.ID, Node: h, Response: protocol.Response{Status: "pending"}})
		}
	}
	// Earlier runs' asset directories, newest first, offer their pinned
	// downloads for reuse; a run that history removed offers nothing.
	earlierAssetDirs, _ := filepath.Glob(filepath.Join(c.State, "runs", "*", "assets"))
	slices.Reverse(earlierAssetDirs)
	if err := config.FreezeAssets(ctx, &r.Project, filepath.Join(c.runDir(r.ID), "assets"), earlierAssetDirs); err != nil {
		os.RemoveAll(c.runDir(r.ID))
		return nil, err
	}
	return r, c.createRun(r)
}

// NotStartedError reports that a new run failed before any node job started.
// Execute has removed the run's record.
type NotStartedError struct{ error }

func (e *NotStartedError) Unwrap() error { return e.error }

var ErrNothingToResume = errors.New("nothing to resume")

func hasUnfinished(r *Run) bool {
	if r.FleetStagesAwaitingNodeSelection {
		return true
	}
	for _, sel := range r.Selections {
		if (!sel.Resolved || sel.Response.Status != "") && !protocol.Completed(sel.Response.Status) {
			return true
		}
		if sel.controllerMachineFilesUnfinished() {
			return true
		}
		for _, item := range sel.Items {
			if !protocol.Completed(sel.Iterations[item.Key].Status) {
				return true
			}
		}
	}
	for _, en := range r.Entries {
		if !protocol.Completed(en.Response.Status) {
			return true
		}
	}
	return false
}
func merge(ms ...map[string]any) map[string]any {
	out := map[string]any{}
	for _, m := range ms {
		for k, v := range m {
			out[k] = v
		}
	}
	return out
}
func (c Controller) Execute(ctx context.Context, r *Run, resume bool) error {
	lease, err := c.lockHistory(false)
	if err != nil {
		return err
	}
	defer unlockRun(lease)
	lock, err := c.lockRun(r.ID)
	if err != nil {
		return err
	}
	if lock == nil {
		return fmt.Errorf("run is already active; use mmx status %s or mmx stop %s", ShortID(r.ID), ShortID(r.ID))
	}
	defer unlockRun(lock)
	if err := c.loadProgress(r); err != nil {
		return err
	}
	limit, err := config.NewLimit(r.Inventory, r.Limit)
	if err != nil {
		return err
	}
	recorded := map[string]machine{}
	r.nodeIdentities, r.machines = &sync.Map{}, &sync.Map{}
	for _, en := range r.Entries {
		if en.NodeID != "" {
			r.nodeIdentities.Store(en.Node, en.NodeID)
		}
		if _, ok := recorded[en.Node]; resume && !ok && en.NodeID != "" {
			recorded[en.Node] = machine{en.NodeID, en.Architecture}
		}
	}
	if err := config.BindProjectAssets(&r.Project, filepath.Join(c.runDir(r.ID), "assets")); err != nil {
		return err
	}
	c.bindSecrets(r)
	resolvedSecrets, err := c.secretScope(r, limit)
	if err != nil {
		r.Status = "failed"
		if !resume {
			// A new run stopped here contacted no node, so nothing is left to
			// resume or start again: save no run and give no such advice.
			return &NotStartedError{errors.Join(err, os.RemoveAll(c.runDir(r.ID)))}
		}
		return errors.Join(err, c.saveStatus(r))
	}
	if resume {
		if err := os.Remove(filepath.Join(c.runDir(r.ID), "stop")); err != nil && !os.IsNotExist(err) {
			return err
		}
		r.StopRequested = false
	}
	r.stopped = make(chan struct{})
	if r.StopRequested {
		close(r.stopped)
	}
	r.Status = "running"
	if err := c.saveStatus(r); err != nil {
		return err
	}
	progress := func() {
		if c.Progress != nil {
			c.Progress(r)
		}
	}
	progress()
	var fatal error
	checkStop := func() {
		if ctx.Err() != nil && !r.StopRequested {
			if err := c.requestStop(r.ID); err != nil {
				fatal = err
				return
			}
		}
		stop, err := c.stopRequested(r.ID)
		if err != nil {
			fatal = err
			return
		}
		if stop && !r.StopRequested {
			r.StopRequested, r.Status = true, "stopping"
			close(r.stopped)
			progress()
		}
	}
	// The first interrupt requests a boundary stop. Admitted jobs and result
	// collection continue; the CLI's second interrupt can leave the display.
	workCtx := context.WithoutCancel(ctx)
	interrupted := ctx.Done()
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	// notAdmitted records work that did not start. An entry that already has a
	// job keeps its last real outcome, so status and resume reconcile or
	// collect it.
	notAdmitted := func(i int, status, reason string) error {
		en := r.Entries[i]
		if en.JobID != "" {
			return nil
		}
		en.Response = protocol.Response{Status: status, Error: reason}
		r.Entries[i] = en
		return c.saveEntry(r.ID, en)
	}
	stopRun := "" // the on_node_failure stop_run cause, once triggered
	// A failed or unconfirmed job stops later normal work on its node. Work
	// blocked by another node's missing output did not fail here, so the node
	// may still read good values later. This attempt rebuilds the map in fleet stage
	// order, so a successful recorded resume of the earlier fleet stage permits its
	// previously unstarted work to continue. It is keyed by the for_each_node
	// iteration (its path prefix, "" outside such a loop) and the node: inside
	// an iteration a failure ends only that loop node's sequence, so a helper
	// node that failed for one loop node still works for the others. When the
	// loop ends, its iterations' halts move to the enclosing level.
	haltedNodes := map[[2]string]string{}
	promoteHalts := func(iteration, enclosing string) {
		for key, cause := range haltedNodes {
			if outer := [2]string{enclosing, key[1]}; key[0] == iteration && haltedNodes[outer] == "" {
				haltedNodes[outer] = cause
			}
		}
	}
	// notStarted names what keeps new work from starting: a stop request or a
	// triggered stop_run. The walk continues so that work is recorded with it.
	notStarted := func() string {
		switch {
		case r.StopRequested:
			return "not started: stop requested"
		case stopRun != "":
			return "not started: " + stopRun
		}
		return ""
	}
	// mu guards the scheduler state: the run's entries and selections, these
	// closures' variables, and the display. The walk holds it, and lets go only
	// while it waits for node jobs or for_each_node iterations, which is when the
	// concurrent iterations of a for_each_node loop take their turns.
	var mu sync.Mutex
	// busy holds each node with an admitted node job. A for_each_node
	// iteration's fleet stage waits for a node another iteration is using, so a
	// helper node works for one loop node at a time.
	busy := map[string]bool{}
	nodeReleased := make(chan struct{})
	release := func(managedNode string) {
		delete(busy, managedNode)
		close(nodeReleased)
		nodeReleased = make(chan struct{})
	}
	// askFacts asks a node for its facts, for a when that reads them. It runs
	// without the scheduler lock, so requests to several nodes overlap, and a
	// stop request or Ctrl-C interrupts it.
	askFacts := func(en Entry) (facts.Snapshot, error) {
		factsCtx, cancel := context.WithCancel(ctx)
		defer cancel()
		go func() {
			select {
			case <-r.stopped:
				cancel()
			case <-factsCtx.Done():
			}
		}()
		en.recorded = recorded[en.Node]
		ask := c.nodeFacts
		if ask == nil {
			ask = c.factsOfNode
		}
		return ask(factsCtx, r, en, resume)
	}
	executeFleetStage := func(fleetStage config.FleetStage, scope value.Scope, notStarted func() string, iteration string) error {
		checkStop()
		if fatal != nil {
			return fatal
		}
		var queue []int
		selected := r.Selections[fleetStage.ID]
		for _, i := range r.selectionEntries(selected.Path) {
			en := r.Entries[i]
			if resume && protocol.Completed(en.Response.Status) {
				continue
			}
			status, reason := blocked(r, fleetStage, en.Node)
			if slices.Contains(selected.BlockedByLimit, en.Node) {
				status, reason = "blocked", fmt.Sprintf("%s selected %s during the run; the limit %s leaves %s out, and the run did not show it before connecting as a node outside the limit that does work for it. Name %s in the fleet stage's nodes in the playbook, or widen the limit", fleetStage.ID, en.Node, strings.Join(r.Limit, ","), en.Node, en.Node)
			}
			cause := haltedNodes[[2]string{"", en.Node}]
			if cause == "" {
				cause = haltedNodes[[2]string{iteration, en.Node}]
			}
			if status == "" && cause != "" {
				status, reason = "stopped", cause
			}
			if cause := notStarted(); cause != "" {
				status, reason = "stopped", cause
			}
			if status == "" {
				queue = append(queue, i)
			} else if err := notAdmitted(i, status, reason); err != nil {
				return err
			}
		}
		limit, err := fleetStage.MaxNodesAtOnce.Nodes(len(selected.Nodes), r.Project.MaxNodesAtOnce)
		if err != nil {
			return err
		}
		if limit < 1 {
			return fmt.Errorf("invalid saved limit on the number of nodes running at once")
		}
		type update struct {
			index    int
			entry    Entry
			finished bool
			err      error
		}
		updates := make(chan update, limit)
		active, next := 0, 0
		stopFleetStage := "" // the on_node_failure cause that stopped this fleet stage's admission
		// The allowance counts this attempt's failed nodes against the fleet stage's
		// recorded selection; the policy stops the fleet stage at one more.
		allowed, err := fleetStage.FailedNodesAllowed.Nodes(len(selected.Nodes))
		if err != nil {
			return err
		}
		failedNodes := map[int]bool{}
		// A resume first reconciles an entry's earlier job; its failure was observed
		// by the run that admitted it and must not stop this resume again.
		earlierJob := map[int]string{}
		// A when that reads node facts needs each node's facts first. They are
		// asked for ahead, for as many queued nodes as there are free slots, so
		// the requests overlap; each answer is used when its node's turn comes.
		readsFacts := fleetStage.When != nil && config.ReadsNodeFacts(fleetStage.When)
		type factsAnswer struct {
			index    int
			snapshot facts.Snapshot
			err      error
		}
		answers, asked := map[int]*factsAnswer{}, map[int]bool{}
		factsAnswers := make(chan factsAnswer, len(queue))
		askAhead := func(from int) {
			for k := from; k < len(queue) && k < from+max(1, limit-active); k++ {
				i := queue[k]
				if en := r.Entries[i]; !asked[i] && !en.WhenWasTrue {
					asked[i] = true
					go func() {
						snapshot, err := askFacts(en)
						factsAnswers <- factsAnswer{i, snapshot, err}
					}()
				}
			}
		}
		// observe takes in a node's progress or outcome: from its job, or from a
		// when that kept it from starting.
		observe := func(index int, en Entry, finished bool, err error) {
			// Workers persist independently; the scheduler owns the display
			// and in-memory entries. Each worker sends updates in order.
			r.Entries[index] = en
			if resume && en.privateInputs {
				r.SecretsRefreshed = true
			}
			if finished {
				if status := en.Response.Status; status == "failed" || protocol.Uncertain(status) {
					haltedNodes[[2]string{iteration, en.Node}] = fmt.Sprintf("not started: earlier fleet stage %s on %s is %s", fleetStage.ID, en.Node, en.Response.Status)
				}
				if err != nil && fatal == nil {
					fatal = err
				}
			}
			// A stopping policy triggers once a required failure is
			// observed, without waiting for that node's cleanup.
			stopping := fleetStage.OnNodeFailure == "stop_fleet_stage" || fleetStage.OnNodeFailure == "stop_run"
			current := finished || en.JobID != earlierJob[index]
			if stopping && stopFleetStage == "" && current && nodeFailed(en.Response, finished) {
				failedNodes[index] = true
			}
			if stopFleetStage == "" && len(failedNodes) > allowed {
				stopFleetStage = fmt.Sprintf("on_node_failure %s after %s failed on %s", fleetStage.OnNodeFailure, fleetStage.ID, en.Node)
				if allowed > 0 {
					stopFleetStage += fmt.Sprintf(" (%d failed nodes; failed_nodes_allowed %s allows %d)", len(failedNodes), fleetStage.FailedNodesAllowed, allowed)
				}
				if fleetStage.OnNodeFailure == "stop_run" {
					stopRun = stopFleetStage
				}
			}
		}
		for next < len(queue) || active > 0 {
			checkStop()
			// released is set while the next node works for another
			// for_each_node iteration; admission waits for it.
			var released chan struct{}
			waitingForFacts := false
			mayFill := !fleetStage.WaitForAllInBatch || active == 0
			if mayFill && stopFleetStage == "" && !r.StopRequested && fatal == nil {
				for next < len(queue) && active < limit {
					checkStop()
					if r.StopRequested || fatal != nil || stopFleetStage != "" {
						break
					}
					i := queue[next]
					en := r.Entries[i]
					// The when is checked as the node would start; a node it
					// skips takes no slot. Resume keeps a when found true.
					if fleetStage.When != nil && !en.WhenWasTrue {
						var snapshot *facts.Snapshot
						if readsFacts {
							answer := answers[i]
							if answer == nil {
								askAhead(next)
								waitingForFacts = true
								break
							}
							snapshot = &answer.snapshot
							if answer.err != nil {
								snapshot = nil
							}
						}
						response, starts := whenAtStart(r, fleetStage, scope, en.Node, snapshot)
						if readsFacts && answers[i].err != nil {
							// An unreachable node fails as its node job would have.
							response, starts = protocol.Response{Status: "failed", Error: "reading the node's facts for the fleet stage's when: " + answers[i].err.Error()}, false
						}
						if !starts {
							next++
							en.Response = response
							r.Entries[i] = en
							if err := c.saveEntry(r.ID, en); err != nil {
								fatal = err
								break
							}
							// A condition that cannot be evaluated fails the node;
							// a skipped or blocked node is no failure.
							if response.Status == "failed" {
								observe(i, en, true, nil)
							}
							continue
						}
						en.WhenWasTrue = true
						r.Entries[i] = en
					}
					if busy[en.Node] {
						released = nodeReleased
						break
					}
					next++
					active++
					busy[en.Node] = true
					en.scope, en.recorded = scope, recorded[en.Node]
					if r.Dryrun {
						en.earlierChange = earlierChangeOnTheNode(r, en.Node)
					}
					earlierJob[i] = en.JobID
					r.Entries[i].Response.Status = "running"
					go func() {
						saveEntry, finishEntry := c.entrySaver(r.ID, en)
						save := func(en Entry) error {
							if err := saveEntry(en); err != nil {
								return err
							}
							en.Response.Results = append([]protocol.Result(nil), en.Response.Results...)
							updates <- update{index: i, entry: en}
							return nil
						}
						execute := c.executeNode
						if execute == nil {
							execute = c.executeEntry
						}
						result, err := execute(workCtx, r, fleetStage, en, resume, save)
						err = errors.Join(err, finishEntry(result))
						updates <- update{i, result, true, err}
					}()
				}
				progress()
			}
			if active == 0 && released == nil && !waitingForFacts {
				break
			}
			interrupt, event := interrupted, ""
			var result update
			var answer factsAnswer
			mu.Unlock()
			select {
			case <-interrupt:
				event = "interrupted"
			case <-ticker.C:
				event = "tick"
			case <-released: // a nil channel never fires
			case result = <-updates:
				event = "update"
			case answer = <-factsAnswers:
				event = "facts"
			}
			mu.Lock()
			switch event {
			case "facts":
				answers[answer.index] = &answer
			case "interrupted":
				interrupted = nil
				checkStop()
			case "tick":
				checkStop()
				progress()
			case "update":
				if result.finished {
					active--
					release(result.entry.Node)
				}
				observe(result.index, result.entry, result.finished, result.err)
				progress()
			}
		}
		// Work left queued by a controller failure stays pending.
		for ; next < len(queue) && fatal == nil; next++ {
			reason := "not started: " + stopFleetStage
			if r.StopRequested {
				reason = "not started: stop requested"
			}
			fatal = notAdmitted(queue[next], "stopped", reason)
		}
		return fatal
	}
	// A resume reads read_file secrets again before it schedules anything.
	nodeFiles := &nodeFileReads{}
	if resume {
		nodeFiles = c.readSecretsAgain(ctx, r)
	}
	walk := &fleetStageWalker{controller: c, run: r, limit: limit, resume: resume, execute: executeFleetStage, notStarted: notStarted, failed: func() bool { checkStop(); return fatal != nil }, nodeFiles: nodeFiles,
		lock: &mu, stopRun: func(cause string) { stopRun = cause }, promoteHalts: promoteHalts, askFacts: askFacts}
	scope := value.Scope{"secrets": resolvedSecrets, "vars": config.NodeVars(r.Project, r.Inventory, ""), "outputs": map[string]any{}, "fleet_stages": map[string]any{}, "inventory": config.InventoryValues(r.Inventory)}
	config.SetOrigins(scope, config.VariableOrigins(r.Project, r.Inventory, ""))
	mu.Lock()
	done, err := walk.fleetStages(r.Project.FleetStages, scope, "", map[string]string{}, nil)
	mu.Unlock()
	r.FleetStagesAwaitingNodeSelection = !done
	if err != nil {
		fatal = err
	}

	checkStop()
	r.Status = runStatus(r, false)
	if err := c.saveStatus(r); err != nil {
		return err
	}
	if fatal != nil {
		return fatal
	}
	if err := c.recordLastSuccessfulRuns(workCtx, r); err != nil {
		return err
	}
	return ctx.Err()
}

func (c Controller) executeEntry(ctx context.Context, r *Run, fleetStage config.FleetStage, en Entry, resume bool, checkpoint func(Entry) error) (Entry, error) {
	expected, recorded := "", machine{}
	if resume {
		expected, recorded = en.RunnerHash, machine{en.NodeID, en.Architecture}
		if recorded == (machine{}) {
			recorded = en.recorded
		}
	}
	conn, err := c.connect(ctx, r, en.Node, expected, recorded)
	var replaced *replacedError
	if errors.As(err, &replaced) {
		return onReplacedMachine(en, replaced), nil
	}
	if err == nil {
		defer conn.Close()
		en.Architecture = conn.Architecture
		en.NodeID = conn.NodeID
		err = r.recordConnection(en.Node, conn)
		en.Transport, en.ConnectionFallbackReason = conn.ConnectionUsed(), conn.FallbackReason
		en.RunnerHash = conn.Hash
		// An uncertain job, or a reboot not yet seen on a new boot, is asked about first.
		if err == nil && resume && en.JobID != "" && (protocol.Uncertain(en.Response.Status) || en.Response.RebootFrom != "") {
			var response protocol.Response
			en.responseRequestedAt = time.Now()
			response, err = conn.Status(ctx, en.JobID)
			if err == nil {
				en.Response = response
				en, err = c.waitEntry(ctx, conn, r, fleetStage, en, checkpoint)
			}
		}
		stop, stopErr := c.stopRequested(r.ID)
		if stopErr != nil {
			return en, stopErr
		}
		if stop && err == nil {
			return en, nil
		}
		if err == nil && !stop && !protocol.Completed(en.Response.Status) {
			if en.JobID != "" {
				en.PreviousNodeJobAttempts = append(en.PreviousNodeJobAttempts, Attempt{JobID: en.JobID, Response: en.Response})
			}
			en.NodeJobAttempt++
			hash := sha256.Sum256([]byte(fmt.Sprintf("%s/%s/%s/%d", r.ID, fleetStage.ID, en.Node, en.NodeJobAttempt)))
			en.JobID = hex.EncodeToString(hash[:])
			en.ControllerMachineReachabilityFailures = nil
			en.Response = protocol.Response{ID: en.JobID, Status: "running"}
			job, prepareErr := nodeJob(r, fleetStage, en)
			if endsInReboot(fleetStage) {
				// MMX never reboots the machine that runs this controller
				// process. The node says whether it is that machine, before
				// anything of the job reaches it, in a dryrun as in a run and
				// whatever the reboot task's guards would say. The job carries
				// the same values, so the reboot task asks once more. A
				// controller machine that cannot say which machine it is asks
				// nothing and sends nothing.
				controllerMachine := thisControllerMachine
				if c.controllerMachine != nil {
					controllerMachine = c.controllerMachine
				}
				machine, err := controllerMachine()
				refusal := ""
				if err == nil {
					refusal, err = conn.RebootRefusal(ctx, machine)
				}
				if err != nil {
					refusal = engine.RebootNotChecked(err).Error()
				}
				if refusal != "" {
					// The job was never sent, so its outcome is certain.
					en.Response.Status, en.Response.Error = "failed", refusal
					return en, nil
				}
				job.ControllerMachine = &machine
			}
			if prepareErr == nil && config.HasVariableSources(job.NodeTasks) {
				// This node's job carries only the files its own values select.
				job.NodeTasks, prepareErr = config.NodeTasksFor(job.NodeTasks, en.Node, filepath.Join(c.runDir(r.ID), "assets"))
			}
			if prepareErr == nil {
				job, prepareErr = scopeJobSecrets(job)
			}
			if prepareErr == nil {
				job.Modules, prepareErr = config.SelectModules(r.Project.Modules, fleetStage.NodeTasks, en.Architecture)
			}
			if prepareErr == nil {
				job.KeptFiles, prepareErr = config.KeptFilesOfANodeJob(fleetStage.NodeTasks, job)
			}
			if prepareErr != nil {
				en.Response.Status, en.Response.Error = preparationFailure(prepareErr), prepareErr.Error()
				return en, nil
			}
			en.privateInputs = job.PrivateInputs
			raw, encodeErr := job.Transport()
			if encodeErr != nil {
				return en, encodeErr
			}
			if len(raw) > protocol.RequestLimit {
				en.Response.Status = "failed"
				en.Response.Error = fmt.Sprintf("job instructions exceed %d MiB", protocol.RequestLimit>>20)
				return en, nil
			}
			if err := checkpoint(en); err != nil {
				return en, err
			}
			if err := c.deliverAssets(ctx, r, job, &en, checkpoint, conn); err != nil {
				en.Response.Status, en.Response.Error = "failed", err.Error()
				if errors.Is(err, context.Canceled) {
					en.Response.Status = "stopped"
				}
				en.Transfer = nil
				return en, nil
			}
			// Publishing the job ID makes a concurrent stop able to reach it.
			// Recheck after that save in case stop inspected the earlier snapshot.
			stop, err = c.stopRequested(r.ID)
			if err != nil {
				return en, err
			}
			if stop {
				en.Response.Status, en.Response.StopRequested = "stopped", true
				return en, nil
			}
			en.responseRequestedAt = time.Now()
			response, submitErr := conn.submitBytes(ctx, raw, en.JobID)
			err = submitErr
			if err == nil {
				en.Response = response
				en, err = c.waitEntry(ctx, conn, r, fleetStage, en, checkpoint)
			}
		}
	}
	if err != nil {
		// A published job stays uncertain unless the node definitely rejected it.
		status := "failed"
		var rejected *rejectedError
		if en.JobID != "" && protocol.Uncertain(en.Response.Status) && !errors.As(err, &rejected) {
			status = "unknown"
		}
		if en.JobID == "" {
			if stop, _ := c.stopRequested(r.ID); stop || errors.Is(err, context.Canceled) {
				status, en.Response.StopRequested = "stopped", true
			}
		}
		en.Response.Status, en.Response.Error = status, err.Error()
	}
	return en, nil
}

// recordConnection records which machine a verified connection for the
// inventory name reached. Two inventory names that reach one machine in one
// boot, such as after a typo in an address, would run both names' node jobs
// on one server and never touch the server the other name means, so the
// later name fails. Virtual machines cloned from one image share a machine
// ID, but each boots its own kernel with its own boot ID.
func (r *Run) recordConnection(name string, conn *Connection) error {
	if r.nodeIdentities != nil {
		r.nodeIdentities.Store(name, conn.NodeID)
	}
	if r.machines == nil {
		return nil
	}
	first, _ := r.machines.LoadOrStore(bootedMachine{conn.NodeID, conn.BootID}, name)
	if first == name {
		return nil
	}
	// Two names with the same address and port are a visible choice in the
	// inventory, such as a load balancer that runs on an application
	// server; only different addresses that reach one machine are a mistake.
	if a, b := r.Inventory.Nodes[first.(string)], r.Inventory.Nodes[name]; a.Address == b.Address && a.Port == b.Port {
		return nil
	}
	return fmt.Errorf("inventory nodes %[1]s and %[2]s reach the same machine: both answered with the same systemd machine ID and the same boot ID. "+
		"MMX ran nothing for %[2]s on that machine, because the machine is %[1]s's in this run. "+
		"Check the addresses of %[1]s and %[2]s in the inventory: one of them probably names the wrong server; two names for one server need the same address. "+
		"When an address is a Host alias in an SSH configuration file (~/.ssh/config, or the node's ssh_config), the wrong value may be that alias's HostName. "+
		"Some containers share the boot ID of their host, so each container needs its own /etc/machine-id", first, name)
}

// whenAtStart checks a fleet stage's when for one node just before the node
// would start, in the controller process, with the walk's values, that
// node's vars, and its name. starts reports that the node starts; otherwise
// response records it skipped, with the condition as the reason, or records
// why the condition could not be evaluated.
func whenAtStart(r *Run, fleetStage config.FleetStage, scope value.Scope, managedNode string, snapshot *facts.Snapshot) (response protocol.Response, starts bool) {
	local := scope.Local()
	local["vars"] = config.NodeVars(r.Project, r.Inventory, managedNode)
	nodeScope(local, managedNode, snapshot)
	config.SetOrigins(local, config.VariableOrigins(r.Project, r.Inventory, managedNode))
	ok, err := local.Predicate(fleetStage.When)
	if err != nil {
		response = failureResponse(r.Dryrun, config.SourceError(fleetStage.Sources, "when", fleetStage.When, local, err))
	} else if !ok {
		response = protocol.Response{Status: "skipped", Error: conditionFalse(fleetStage.When)}
	} else {
		return protocol.Response{}, true
	}
	// The facts the decision read stay with the node's record.
	response.Facts = snapshot
	return response, false
}

// nodeScope sets .node for a condition the controller process checks: the
// node's name, and its facts when the node reported them for this check.
func nodeScope(s value.Scope, managedNode string, snapshot *facts.Snapshot) {
	node := map[string]any{"name": managedNode}
	if snapshot != nil {
		for name, v := range snapshot.Values {
			node[name] = v
		}
		node["name"] = managedNode
		for path, reason := range snapshot.Unavailable {
			s.Mark(path, reason)
		}
	}
	s["node"] = node
}

// factsOfNode connects to a node, setting it up as for any node job, and asks
// for its facts, measured as a node job measures them.
func (c Controller) factsOfNode(ctx context.Context, r *Run, en Entry, resume bool) (facts.Snapshot, error) {
	expected, recorded := "", machine{}
	if resume {
		expected, recorded = en.RunnerHash, machine{en.NodeID, en.Architecture}
		if recorded == (machine{}) {
			recorded = en.recorded
		}
	}
	conn, err := c.connect(ctx, r, en.Node, expected, recorded)
	if err != nil {
		var replaced *replacedError
		if errors.As(err, &replaced) {
			return facts.Snapshot{}, fmt.Errorf("%s", onReplacedMachine(en, replaced).Response.Error)
		}
		return facts.Snapshot{}, err
	}
	defer conn.Close()
	if err := r.recordConnection(en.Node, conn); err != nil {
		return facts.Snapshot{}, err
	}
	return conn.Facts(ctx, en.Node)
}

// conditionFalse is the reason recorded for work a false when skipped.
func conditionFalse(when any) string {
	text, ok := when.(string)
	if !ok {
		b, _ := json.Marshal(when)
		text = string(b)
	}
	return "when is false: " + text
}

// failureResponse records work that could not be selected or evaluated: an
// unavailable value blocks it, or in dryrun leaves it not_checked; any other
// error fails it.
func failureResponse(dryrun bool, err error) protocol.Response {
	status := "failed"
	var unavailable *value.UnavailableError
	if errors.As(err, &unavailable) {
		status = "blocked"
		if dryrun {
			status = "not_checked"
		}
	}
	return protocol.Response{Status: status, Error: err.Error()}
}

// nodeFailed reports a required node failure: a finished job that needs resume,
// or a running job that recorded a failed task and is now in failure handling
// or cleanup. A task result is recorded only after its final repetition.
func nodeFailed(response protocol.Response, finished bool) bool {
	if finished {
		return !protocol.Completed(response.Status)
	}
	if response.Status != "running" {
		return false
	}
	for _, result := range response.Results {
		if protocol.Failure(result.Status) {
			return true
		}
	}
	return false
}

// blocked returns the status and reason for work whose prerequisites did not
// succeed. When dryrun could not check every unmet prerequisite, the work is
// not_checked with the same reason; any other unmet prerequisite blocks it.
func blocked(r *Run, s config.FleetStage, managedNode string) (string, string) {
	status, reason := "", ""
	unmet := func(prerequisite, why string) {
		if prerequisite == "not_checked" && status == "" {
			status, reason = "not_checked", why
		} else if prerequisite != "not_checked" && status != "blocked" {
			status, reason = "blocked", why
		}
	}
	responses := func(path string) map[string]protocol.Response {
		out := map[string]protocol.Response{}
		sel := r.Selections[path]
		if sel != nil && sel.Loop {
			for _, item := range sel.Items {
				out[item.Key] = sel.Iterations[item.Key]
			}
		} else {
			for _, entry := range r.Entries {
				if entry.FleetStage == path {
					out[entry.Node] = entry.Response
				}
			}
		}
		return out
	}
	// A selection that could not be resolved has no nodes or items to check.
	for _, req := range s.Requires.SameNode {
		if sel := r.Selections[req]; sel != nil && !sel.Resolved {
			unmet(sel.Response.Status, fmt.Sprintf("requires same_node fleet stage %s to succeed: %s", req, sel.Response.Error))
		} else if response := responses(req)[managedNode]; response.Status != "succeeded" {
			unmet(response.Status, fmt.Sprintf("requires same_node fleet stage %s to succeed on %s", req, managedNode))
		}
	}
	for _, req := range s.Requires.AllNodes {
		if sel := r.Selections[req]; sel != nil && !sel.Resolved {
			unmet(sel.Response.Status, fmt.Sprintf("requires all_nodes fleet stage %s to succeed: %s", req, sel.Response.Error))
			continue
		} else if sel != nil && len(sel.Nodes) == 0 && len(sel.LeftOut) > 0 {
			unmet("blocked", fmt.Sprintf("requires all_nodes fleet stage %s, which the limit %s leaves out", req, strings.Join(r.Limit, ",")))
			continue
		}
		statuses := responses(req)
		nodes := make([]string, 0, len(statuses))
		for h := range statuses {
			nodes = append(nodes, h)
		}
		sort.Strings(nodes)
		for _, h := range nodes {
			if statuses[h].Status != "succeeded" {
				unmet(statuses[h].Status, fmt.Sprintf("requires all_nodes fleet stage %s to succeed (%s is %s)", req, h, statuses[h].Status))
			}
		}
	}
	return status, reason
}
