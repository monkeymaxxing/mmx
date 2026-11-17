package controller

import (
	"cmp"
	"fmt"
	"os"
	"os/user"
	"slices"
	"sort"
	"strings"
	"sync"

	"mmx/internal/config"
	"mmx/internal/facts"
	"mmx/internal/protocol"
	"mmx/internal/value"
)

// A selection fixes the nodes or loop items before admitting work. Node progress
// stays in the existing entry records; loops save only their declared exports.
// LeftOut lists the nodes the run's limit removed from a node selection; with
// no Nodes left, the fleet stage is not selected. A fleet stage or
// for_each_node loop whose own selection is empty, which
// skip_when_no_nodes_are_selected allows, is not selected either
// (LeftOutBecauseItSelectsNoNodes). Resolved is set once the
// nodes or loop items are fixed. A node selection's entries are those with its
// Path, one for each of its Nodes (Run.selectionEntries).
type Selection struct {
	// The indexes of each enclosing loop's fleet stage and iteration, then of
	// this fleet stage in its list: [2] is a third top-level fleet stage.
	PlaybookPosition []int                        `json:"playbook_position"`
	Path             string                       `json:"path"`
	Nodes            []string                     `json:"nodes,omitempty"`
	LeftOut          []string                     `json:"left_out,omitempty"`
	Loop             bool                         `json:"loop,omitempty"`
	Resolved         bool                         `json:"resolved"`
	Items            []value.Iteration            `json:"items,omitempty"`
	Iterations       map[string]protocol.Response `json:"iterations,omitempty"`
	Response         protocol.Response            `json:"response"`
	// BlockedByLimit lists the nodes outside the limit that a for_each_node
	// iteration's selection reached during the run without the run showing
	// them before connecting; their work is blocked.
	BlockedByLimit []string `json:"blocked_by_limit,omitempty"`
	// LeftOutBecauseItSelectsNoNodes: nodes or for_each_node, minus
	// except_nodes, selects no node, and skip_when_no_nodes_are_selected
	// leaves the fleet stage or loop out of the run.
	LeftOutBecauseItSelectsNoNodes bool `json:"left_out_because_it_selects_no_nodes,omitempty"`
	// ControllerMachineFiles are the files save_on_controller_machine wrote.
	ControllerMachineFiles []ControllerMachineFile `json:"controller_machine_files,omitempty"`
}

type fleetStageWalker struct {
	controller Controller
	run        *Run
	// limit narrows selections: the run's limit, or none inside a
	// for_each_node iteration, whose fleet stages run on the nodes they name.
	limit  config.Limit
	resume bool
	// execute runs one fleet stage's node jobs; its notStarted names what
	// keeps them from starting, and iteration is the for_each_node iteration
	// whose failed nodes it halts ("" outside such a loop).
	execute func(config.FleetStage, value.Scope, func() string, string) error
	// failed reports a controller failure: the walk ends and unwalked work
	// stays pending. notStarted names a stop or stop_run cause: the walk goes
	// on, recording each remaining selection, entry, and iteration as stopped.
	failed     func() bool
	notStarted func() string
	// nodeFiles holds the read_file secrets a resume read again before the
	// walk; the walkers of concurrent for_each_node iterations share it.
	nodeFiles *nodeFileReads
	// lock guards the scheduler state; a for_each_node loop lets go of it
	// while it waits for its iterations. stopRun triggers stop_run for the
	// fleet stages after a for_each_node loop.
	lock    *sync.Mutex
	stopRun func(string)
	// A for_each_node iteration ends its sequence at a fleet stage that did
	// not complete: ended then names the cause for its remaining work.
	endOnFailure bool
	ended        string
	// iterationPath is the path prefix of the for_each_node iteration this walker
	// runs, such as rollout[app02]/; promoteHalts moves an iteration's halted
	// nodes to the enclosing level when its loop ends.
	iterationPath string
	promoteHalts  func(iteration, enclosing string)
	// askFacts asks a node for its facts; the walker calls it without the lock.
	askFacts func(Entry) (facts.Snapshot, error)
}

// cause names what keeps new work from starting: a stop request, stop_run, or
// the end of this for_each_node iteration's sequence.
func (w *fleetStageWalker) cause() string {
	if cause := w.notStarted(); cause != "" {
		return cause
	}
	return w.ended
}

func branchScope(parent value.Scope) value.Scope {
	child := parent.Local()
	for _, root := range []string{"outputs", "fleet_stages"} {
		child[root] = merge(parent[root].(map[string]any))
	}
	return child
}

// earlierChangeOnTheNode names the first node task, in playbook order, of a
// fleet stage that the node has completed in this dryrun and that would
// change the node, or may, as the engine counts them, or the empty text. A node runs its fleet stages in
// order, so each completed one comes before the fleet stage it starts now:
// a check of that one may fail only because the dryrun made no change.
func earlierChangeOnTheNode(r *Run, node string) string {
	for _, en := range r.OrderedEntries() {
		if en.Node != node || !protocol.Completed(en.Response.Status) {
			continue
		}
		for _, result := range en.Response.Results {
			if result.Change == "would_change" || result.Change == "unknown" {
				return result.Task + " in the fleet stage " + en.FleetStage
			}
		}
	}
	return ""
}

// nodeJob is the job of the fleet stage on the entry's node. Its node tasks
// hold the node's value of each var that a duration reads
// (config.ResolveDurations).
func nodeJob(r *Run, fleetStage config.FleetStage, en Entry) (config.Job, error) {
	nodeTasks, err := config.ResolveDurations(fleetStage.NodeTasks, config.DurationScope(r.Project, r.Inventory, en.Node))
	if err != nil {
		return config.Job{}, err
	}
	scope := en.scope.Data()
	delete(scope, "vars")
	kinds := []protocol.ConnectionKind{protocol.ConnectionSSH}
	if r.Inventory.Nodes[en.Node].Transport != "ssh_only" {
		kinds = append(kinds, protocol.ConnectionHTTPS)
	}
	account, hostName := startedBy()
	return config.Job{StartedByAccount: account, StartedFromHostName: hostName, ReachabilityConfirmationConnectionKinds: kinds, Origins: config.VariableOrigins(r.Project, r.Inventory, en.Node), Protocol: protocol.Version, ID: en.JobID, Project: r.Project.Project, FleetStage: fleetStage.ID, Node: en.Node, Dryrun: r.Dryrun, Vars: config.NodeVars(r.Project, r.Inventory, en.Node), NodeTasks: nodeTasks, Outputs: fleetStage.Outputs, Sources: fleetStage.Sources, Scope: scope, Unavailable: en.scope.Missing(), OmittedEntries: en.scope.OmittedEntries(), EarlierChangeInThisDryrun: en.earlierChange}, nil
}

// startedBy names the local account and the host name of the controller
// machine, for the node's busy message. Either is empty when unknown.
func startedBy() (account, hostName string) {
	if u, err := user.Current(); err == nil {
		account = u.Username
	}
	hostName, _ = os.Hostname()
	return account, hostName
}

// unselected records a selection that could not resolve; after a stop or
// stop_run it is work that did not start.
func (w *fleetStageWalker) unselected(err error) protocol.Response {
	if cause := w.cause(); cause != "" {
		return protocol.Response{Status: "stopped", Error: cause}
	}
	return w.failure(err)
}

func (w *fleetStageWalker) failure(err error) protocol.Response {
	return failureResponse(w.run.Dryrun, err)
}

func (w *fleetStageWalker) fleetStages(fleetStages []config.FleetStage, scope value.Scope, prefix string, parentPaths map[string]string, order []int) (bool, error) {
	paths := map[string]string{}
	for key, path := range parentPaths {
		paths[key] = path
	}
	for i, fleetStage := range fleetStages {
		if w.failed() {
			return false, nil
		}
		path := prefix + fleetStage.ID
		// A fleet stage --only-fleet-stages leaves out gets no selection; work
		// that reads it is blocked, naming the option.
		if prefix == "" && !w.run.Project.KeepsFleetStage(fleetStage.ID) {
			reason := w.run.Project.NotSelectedReason(fleetStage.ID)
			scope.Mark(value.Pointer("outputs", fleetStage.ID), reason)
			scope.Mark(value.Pointer("fleet_stages", fleetStage.ID), reason)
			continue
		}
		sel := w.run.Selections[path]
		if sel == nil {
			sel = &Selection{Path: path, Loop: fleetStage.Loop != nil || fleetStage.ForEachNode != nil, PlaybookPosition: append(append([]int(nil), order...), i)}
			w.run.Selections[path] = sel
		}
		if !sel.Resolved {
			sel.Response = protocol.Response{}
			// After a stop or stop_run, .fleet stages outcomes can still change before
			// resume, so a selection reading them stays unselected; resume
			// resolves it from the outcomes current then.
			resolve := scope
			if cause := w.cause(); cause != "" {
				resolve = scope.Local()
				resolve.Mark(value.Pointer("fleet_stages"), cause)
			}
			if fleetStage.ForEachNode != nil {
				// The loop's nodes resolve like a fleet stage's nodes; each kept
				// node is one iteration, keyed by its ID.
				nodes, err := config.SelectNodes(w.run.Inventory, resolve, fleetStage)
				if err != nil {
					sel.Response = w.unselected(err)
				} else {
					sel.Nodes, sel.LeftOut = w.limit.Narrow(nodes)
					sel.LeftOutBecauseItSelectsNoNodes = len(nodes) == 0
					for _, managedNode := range sel.Nodes {
						sel.Items = append(sel.Items, value.Iteration{Key: managedNode, Value: managedNode})
					}
					sel.Iterations = map[string]protocol.Response{}
					sel.Resolved = true
				}
			} else if sel.Loop {
				items, err := resolve.Iterations(fleetStage.Loop, fleetStage.As, fleetStage.Key)
				if err != nil {
					sel.Response = w.unselected(config.SourceError(fleetStage.Sources, "loop", fleetStage.Loop, scope, err))
				} else {
					sel.Items = items
					sel.Iterations = map[string]protocol.Response{}
					sel.Resolved = true
				}
			} else {
				nodes, err := config.SelectNodes(w.run.Inventory, resolve, fleetStage)
				if err != nil {
					sel.Response = w.unselected(err)
				} else {
					sel.Nodes, sel.LeftOut = w.limit.Narrow(nodes)
					sel.LeftOutBecauseItSelectsNoNodes = len(nodes) == 0
					sel.BlockedByLimit = w.notShown(path, sel.Nodes)
					sel.Resolved = true
				}
			}
			if err := w.controller.saveSelection(w.run.ID, sel); err != nil {
				return false, err
			}
			if sel.Resolved && !sel.Loop {
				for _, managedNode := range sel.Nodes {
					w.run.Entries = append(w.run.Entries, Entry{FleetStage: path, Node: managedNode, Response: protocol.Response{Status: "pending"}})
				}
			}
		}
		if !sel.Resolved {
			scope.Mark(value.Pointer("outputs", fleetStage.ID), path+": "+sel.Response.Error)
			scope.Mark(value.Pointer("fleet_stages", fleetStage.ID), path+": "+sel.Response.Error)
			paths[fleetStage.ID] = path
			w.endSequence(fleetStage, prefix)
			continue
		}
		switch {
		case fleetStage.ForEachNode != nil:
			if err := w.forEachNode(fleetStage, sel, scope, paths); err != nil {
				return false, err
			}
		case sel.Loop:
			if err := w.loop(fleetStage, sel, scope, paths); err != nil {
				return false, err
			}
		default:
			concrete := fleetStage
			concrete.ID = path
			concrete.Requires.SameNode = resolveRequirements(fleetStage.Requires.SameNode, paths)
			concrete.Requires.AllNodes = resolveRequirements(fleetStage.Requires.AllNodes, paths)
			if err := w.execute(concrete, scope, w.cause, w.iterationPath); err != nil {
				return false, err
			}
		}
		w.publish(fleetStage, sel, scope)
		if err := w.saveOnControllerMachine(fleetStage, sel, scope); err != nil {
			return false, err
		}
		paths[fleetStage.ID] = path
		w.endSequence(fleetStage, prefix)
	}
	// Every fleet stage was walked; a later stop leaves no fleet stage unselected.
	return true, nil
}

// notShown lists the nodes a for_each_node iteration's fleet stage selected
// that the run's limit leaves out and that the run did not show before
// connecting as helper nodes of that fleet stage: those of a selection that
// resolved during the run, from outputs or outcomes of earlier fleet stages.
func (w *fleetStageWalker) notShown(path string, nodes []string) []string {
	if w.iterationPath == "" || len(w.run.Limit) == 0 {
		return nil
	}
	limit, err := config.NewLimit(w.run.Inventory, w.run.Limit)
	if err != nil {
		return nodes
	}
	shown := map[string]bool{}
	for _, helper := range w.run.HelperNodes {
		if slices.Contains(helper.FleetStages, path) {
			shown[helper.Node] = true
		}
	}
	var out []string
	for _, node := range nodes {
		if !limit[node] && !shown[node] {
			out = append(out, node)
		}
	}
	return out
}

// endSequence ends a for_each_node iteration's sequence after a fleet stage
// that did not complete for it, on the loop's node or on a helper node.
func (w *fleetStageWalker) endSequence(fleetStage config.FleetStage, prefix string) {
	if !w.endOnFailure || w.ended != "" {
		return
	}
	if outcome := w.sequenceOutcome([]config.FleetStage{fleetStage}, prefix); !protocol.Completed(outcome.Status) {
		w.ended = "not started: " + outcome.Error
	}
}

func resolveRequirements(names []string, paths map[string]string) []string {
	out := make([]string, len(names))
	for i, name := range names {
		out[i] = paths[name]
	}
	return out
}

func (w *fleetStageWalker) loop(fleetStage config.FleetStage, sel *Selection, scope value.Scope, paths map[string]string) error {
	if sel.Iterations == nil {
		sel.Iterations = map[string]protocol.Response{}
	}
	for index, item := range sel.Items {
		if w.failed() {
			break
		}
		previous, recorded := sel.Iterations[item.Key]
		if recorded && w.resume && protocol.Completed(previous.Status) {
			continue
		}
		if cause := w.cause(); cause != "" {
			// An earlier attempt's failed or uncertain outcome stays visible.
			if !recorded || !(protocol.Failure(previous.Status) || protocol.Uncertain(previous.Status)) {
				sel.Iterations[item.Key] = protocol.Response{Status: "stopped", Error: cause}
				if err := w.controller.saveSelection(w.run.ID, sel); err != nil {
					return err
				}
			}
			continue
		}
		local := branchScope(scope)
		local[fleetStage.As] = value.ReadAgain(value.Deferred(item.Value, w.lookupSecret, w.resume), w.readAgain)
		prefix := sel.Path + "[" + value.EscapeKey(item.Key) + "]/"
		response, starts := w.loopCondition(fleetStage, local, prefix)
		if starts {
			var err error
			response, err = w.iteration(fleetStage, local, prefix, paths, append(append([]int(nil), sel.PlaybookPosition...), index))
			if err != nil {
				return err
			}
		}
		sel.Iterations[item.Key] = response
		if err := w.controller.saveSelection(w.run.ID, sel); err != nil {
			return err
		}
	}
	return nil
}

// loopCondition checks a loop's when for one iteration just before it would
// start. An iteration that already started keeps its when's result, so resume
// does not check it again. starts reports that the iteration starts;
// otherwise response records it skipped, with the condition as the reason, or
// why the condition could not be evaluated.
func (w *fleetStageWalker) loopCondition(fleetStage config.FleetStage, local value.Scope, prefix string) (response protocol.Response, starts bool) {
	if w.run.Selections[prefix+fleetStage.FleetStages[0].ID] != nil {
		return protocol.Response{}, true
	}
	ok, err := local.Predicate(fleetStage.When)
	switch {
	case err != nil:
		return w.failure(config.SourceError(fleetStage.Sources, "when", fleetStage.When, local, err)), false
	case !ok:
		return protocol.Response{Status: "skipped", Error: conditionFalse(fleetStage.When)}, false
	}
	return protocol.Response{}, true
}

// iteration walks one loop iteration's enclosed fleet stages and returns its
// outcome, with its declared outputs when it succeeded.
func (w *fleetStageWalker) iteration(fleetStage config.FleetStage, local value.Scope, prefix string, paths map[string]string, order []int) (protocol.Response, error) {
	done, err := w.fleetStages(fleetStage.FleetStages, local, prefix, paths, order)
	if err != nil {
		return protocol.Response{}, err
	}
	response := w.sequenceOutcome(fleetStage.FleetStages, prefix)
	if !done && response.Status == "succeeded" {
		response.Status = "pending"
	}
	if response.Status == "succeeded" {
		response.Outputs, err = config.ExportOutputs(fleetStage.Outputs, local, fleetStage.Sources)
		if err != nil {
			change := response.Change
			response = w.failure(err)
			response.Change = change
		}
	}
	return response, nil
}

// forEachNode runs a for_each_node loop. Each of the loop's nodes is one
// iteration; up to max_nodes_at_once of them (one by default) are in their
// sequence of enclosed fleet stages at once, each in its own goroutine. The
// walker's lock keeps them to one at a time in the scheduler: an iteration
// holds it except while it waits for node jobs, and this loop holds it
// except while it waits for an iteration to end. A failure ends that
// iteration's sequence; iterations already in their sequence finish it whole,
// and the loop's on_node_failure and failed_nodes_allowed, which count the
// loop's nodes, decide whether further loop nodes start.
func (w *fleetStageWalker) forEachNode(fleetStage config.FleetStage, sel *Selection, scope value.Scope, paths map[string]string) error {
	if sel.Iterations == nil {
		sel.Iterations = map[string]protocol.Response{}
	}
	limit, err := fleetStage.MaxNodesAtOnce.Nodes(len(sel.Items), 1)
	if err != nil {
		return err
	}
	allowed, err := fleetStage.FailedNodesAllowed.Nodes(len(sel.Items))
	if err != nil {
		return err
	}
	type ended struct {
		key      string
		response protocol.Response
		err      error
	}
	ends := make(chan ended)
	active, batch, failedNodes := 0, 0, 0
	stopLoop := "" // the loop's on_node_failure cause, once triggered
	var walkErr error
	record := func(key string, response protocol.Response) {
		sel.Iterations[key] = response
		if err := w.controller.saveSelection(w.run.ID, sel); err != nil && walkErr == nil {
			walkErr = err
		}
	}
	// settle records a loop node's outcome and counts a failure, on the loop's
	// node or on a helper node, against the loop's policy.
	settle := func(key string, response protocol.Response) {
		if protocol.Failure(response.Status) || protocol.Uncertain(response.Status) {
			failedNodes++
		}
		stopping := fleetStage.OnNodeFailure == "stop_fleet_stage" || fleetStage.OnNodeFailure == "stop_run"
		if stopping && stopLoop == "" && failedNodes > allowed {
			stopLoop = fmt.Sprintf("on_node_failure %s after %s failed on %s", fleetStage.OnNodeFailure, sel.Path, key)
			if allowed > 0 {
				stopLoop += fmt.Sprintf(" (%d failed nodes; failed_nodes_allowed %s allows %d)", failedNodes, fleetStage.FailedNodesAllowed, allowed)
			}
		}
		record(key, response)
	}
	wait := func() {
		w.lock.Unlock()
		end := <-ends
		w.lock.Lock()
		active--
		if end.err != nil {
			if walkErr == nil {
				walkErr = end.err
			}
			return
		}
		settle(end.key, end.response)
	}
	notStarted := func() string {
		if cause := w.cause(); cause != "" || stopLoop == "" {
			return cause
		}
		return "not started: " + stopLoop
	}
	// stopped records a loop node that a stop keeps from starting; an
	// earlier attempt's failed or uncertain outcome stays visible.
	stopped := func(key, cause string) {
		previous, recorded := sel.Iterations[key]
		if !recorded || !(protocol.Failure(previous.Status) || protocol.Uncertain(previous.Status)) {
			record(key, protocol.Response{Status: "stopped", Error: cause})
		}
	}
	// A when that reads node facts needs each loop node's facts first: they
	// are asked for ahead, for as many loop nodes as may be in their sequence
	// at once, so the requests overlap.
	readsFacts := fleetStage.When != nil && config.ReadsNodeFacts(fleetStage.When)
	type factsAnswer struct {
		snapshot facts.Snapshot
		err      error
	}
	answers := map[string]chan factsAnswer{}
	askAhead := func(from int) {
		for k := from; k < len(sel.Items) && k < from+limit; k++ {
			key := sel.Items[k].Key
			if answers[key] == nil {
				answer := make(chan factsAnswer, 1)
				answers[key] = answer
				go func() {
					snapshot, err := w.askFacts(Entry{Node: key})
					answer <- factsAnswer{snapshot, err}
				}()
			}
		}
	}
	for index, item := range sel.Items {
		if w.failed() || walkErr != nil {
			break
		}
		if previous, recorded := sel.Iterations[item.Key]; recorded && w.resume && protocol.Completed(previous.Status) {
			continue
		}
		if cause := notStarted(); cause != "" {
			stopped(item.Key, cause)
			continue
		}
		local := branchScope(scope)
		local[fleetStage.As] = item.Value
		prefix := sel.Path + "[" + value.EscapeKey(item.Key) + "]/"
		needsFacts := readsFacts && w.run.Selections[prefix+fleetStage.FleetStages[0].ID] == nil
		if needsFacts {
			askAhead(index)
		}
		// Wait for a free slot, or with wait_for_all_in_batch, for the whole
		// current batch to finish.
		for active > 0 && (active >= limit || fleetStage.WaitForAllInBatch && batch >= limit) {
			wait()
		}
		if active == 0 {
			batch = 0
		}
		if w.failed() || walkErr != nil {
			break
		}
		if cause := notStarted(); cause != "" {
			stopped(item.Key, cause)
			continue
		}
		// The loop's when is checked now that the loop node would start, as
		// on a plain fleet stage. It reads the loop node as .node: its name,
		// and its facts when the condition reads them.
		var snapshot *facts.Snapshot
		var factsErr error
		if needsFacts {
			w.lock.Unlock()
			answer := <-answers[item.Key]
			w.lock.Lock()
			snapshot, factsErr = &answer.snapshot, answer.err
			if cause := notStarted(); cause != "" {
				stopped(item.Key, cause)
				continue
			}
		}
		condition := local.Local()
		nodeScope(condition, item.Key, snapshot)
		// A loop node its when skips takes no slot.
		response, starts := w.loopCondition(fleetStage, condition, prefix)
		if factsErr != nil {
			// An unreachable loop node fails as its node job would have.
			response, starts = protocol.Response{Status: "failed", Error: "reading the node's facts for the loop's when: " + factsErr.Error()}, false
		} else if !starts {
			response.Facts = snapshot
		}
		if !starts {
			settle(item.Key, response)
			continue
		}
		active++
		batch++
		iteration := *w
		iteration.limit, iteration.endOnFailure, iteration.ended, iteration.iterationPath = nil, true, "", prefix
		order := append(append([]int(nil), sel.PlaybookPosition...), index)
		go func() {
			w.lock.Lock()
			response, err := iteration.iteration(fleetStage, local, prefix, paths, order)
			w.lock.Unlock()
			ends <- ended{item.Key, response, err}
		}()
	}
	for active > 0 {
		wait()
	}
	// After the loop, a node that failed in an iteration starts none of its
	// later fleet stages.
	for _, item := range sel.Items {
		w.promoteHalts(sel.Path+"["+value.EscapeKey(item.Key)+"]/", w.iterationPath)
	}
	if stopLoop != "" && fleetStage.OnNodeFailure == "stop_run" {
		w.stopRun(stopLoop)
	}
	return walkErr
}

func outcome(response protocol.Response) map[string]any {
	change := protocol.CombineChange("unchanged", response.Change)
	for _, r := range response.Results {
		change = protocol.CombineChange(change, r.Change)
	}
	return map[string]any{"status": response.Status, "reason": response.Error, "change": change}
}

// lookupSecret loads a named secret that saved fleet stage state carries only by name:
// a completed output or a fleet stage-loop item. A resume refreshes them.
func (w *fleetStageWalker) lookupSecret(name string) (any, error) {
	if w.controller.Secrets == nil {
		return nil, fmt.Errorf("secret %s is unavailable", name)
	}
	return w.controller.Secrets.Get(name)
}

func (w *fleetStageWalker) publish(fleetStage config.FleetStage, sel *Selection, scope value.Scope) {
	outputs, states := map[string]any{}, map[string]any{}
	// The scope the node jobs were built from, before this fleet stage's own
	// outputs join it.
	defer func() {
		scope["outputs"].(map[string]any)[fleetStage.ID] = outputs
		scope["fleet_stages"].(map[string]any)[fleetStage.ID] = states
	}()
	publish := func(key string, response protocol.Response, origin *value.NodeOrigin) {
		if response.Status == "" {
			response.Status = "pending"
		}
		states[key] = outcome(response)
		if response.Status == "succeeded" {
			exported := response.Outputs
			if origin != nil {
				var sent []*value.Secret
				computed := false
				exported = w.trustNodeRecords(exported, *origin, func() []*value.Secret {
					if !computed {
						sent, computed = nodeJobSecrets(w.run, fleetStage, origin.Node, scope), true
					}
					return sent
				})
			}
			outputs[key] = value.ReadAgain(value.Deferred(exported, w.lookupSecret, w.resume), w.readAgain)
			if response.Outputs == nil {
				outputs[key] = map[string]any{}
			}
		} else if reason := fmt.Sprintf("%s on %s is %s: %s", sel.Path, key, response.Status, response.Error); response.Status == "skipped" {
			// A node or loop iteration that a when skipped is the
			// playbook's choice, not a failure that must not disappear from
			// the whole collection: the collection leaves it out.
			scope.MarkOmittedEntry(value.Pointer("outputs", fleetStage.ID, key), reason)
		} else {
			scope.Mark(value.Pointer("outputs", fleetStage.ID, key), reason)
		}
	}
	if sel.Loop {
		for _, item := range sel.Items {
			// The controller process computed a loop iteration's outputs.
			publish(item.Key, sel.Iterations[item.Key], nil)
		}
	} else {
		for _, i := range w.run.selectionEntries(sel.Path) {
			en := w.run.Entries[i]
			publish(en.Node, en.Response, &value.NodeOrigin{FleetStage: en.FleetStage, Node: en.Node})
		}
	}
	// Work that reads a node the limit left out, from a fleet stage or a
	// for_each_node loop, is blocked, naming the limit.
	for _, managedNode := range sel.LeftOut {
		reason := fmt.Sprintf("%s on %s is left out by the limit %s", sel.Path, managedNode, strings.Join(w.run.Limit, ","))
		scope.Mark(value.Pointer("outputs", fleetStage.ID, managedNode), reason)
		scope.Mark(value.Pointer("fleet_stages", fleetStage.ID, managedNode), reason)
	}
}

// Reduce only this iteration's direct children. Nested loops already carry
// their own aggregate result; unrelated nodes and iterations need no scan.
func (w *fleetStageWalker) sequenceOutcome(fleetStages []config.FleetStage, prefix string) protocol.Response {
	responses := []protocol.Response{}
	for _, fleetStage := range fleetStages {
		sel := w.run.Selections[prefix+fleetStage.ID]
		if sel == nil {
			responses = append(responses, protocol.Response{Status: "pending"})
			continue
		}
		if !sel.Resolved {
			responses = append(responses, sel.Response)
			continue
		}
		if sel.Loop {
			for _, item := range sel.Items {
				responses = append(responses, sel.Iterations[item.Key])
			}
		} else {
			for _, i := range w.run.selectionEntries(sel.Path) {
				response := w.run.Entries[i].Response
				response.Error = fmt.Sprintf("%s on %s is %s: %s", sel.Path, w.run.Entries[i].Node, response.Status, response.Error)
				responses = append(responses, response)
			}
		}
		// A file save_on_controller_machine could not write keeps the
		// iteration unfinished, so resume writes it.
		for _, file := range sel.ControllerMachineFiles {
			if result := file.Result.Safe(); !protocol.Completed(result.Status) {
				responses = append(responses, protocol.Response{Status: result.Status, Error: fmt.Sprintf("%s / controller machine / %s is %s: %s", sel.Path, result.Task, result.Status, result.Reason)})
			}
		}
	}
	// The iteration takes the highest-ranked status; success carries no error.
	result := protocol.Response{Status: "succeeded", Change: "unchanged"}
	for _, response := range responses {
		result.Change = protocol.CombineChange(result.Change, outcome(response)["change"].(string))
		if protocol.Rank(response.Status) > protocol.Rank(result.Status) {
			result.Status, result.Error = response.Status, response.Error
		}
	}
	if result.Status == "" {
		result.Status = "pending"
	}
	return result
}

// selectionEntries returns the indexes in r.Entries of a node selection's
// entries: those of its fleet stage path, one for each of its Nodes, in that
// order. A path names one fleet stage, or one fleet stage of one loop
// iteration, and a selection lists each node once, so a fleet stage path and a
// node identify one entry. The entries of a selection resolved during the run
// follow those of later fleet stages: an index says nothing about its entry.
func (r *Run) selectionEntries(path string) []int {
	var indexes []int
	for i, en := range r.Entries {
		if en.FleetStage == path {
			indexes = append(indexes, i)
		}
	}
	return indexes
}

// sortedSelections lists the selections in playbook order.
func sortedSelections(selections map[string]*Selection) []*Selection {
	out := make([]*Selection, 0, len(selections))
	for _, sel := range selections {
		out = append(out, sel)
	}
	slices.SortFunc(out, func(a, b *Selection) int {
		return cmp.Or(slices.Compare(a.PlaybookPosition, b.PlaybookPosition), strings.Compare(a.Path, b.Path))
	})
	return out
}

// OrderedEntries lists the entries in playbook and iteration order, for display.
func (r *Run) OrderedEntries() []Entry {
	entries := append([]Entry(nil), r.Entries...)
	sort.SliceStable(entries, func(i, j int) bool {
		a, b := r.Selections[entries[i].FleetStage], r.Selections[entries[j].FleetStage]
		if a == nil || b == nil {
			return false
		}
		return slices.Compare(a.PlaybookPosition, b.PlaybookPosition) < 0
	})
	explainFactChanges(entries)
	return entries
}
