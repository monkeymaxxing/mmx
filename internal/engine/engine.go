// Package engine reconciles one frozen node job. Both transports use this engine.
package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"strconv"
	"strings"
	"syscall"

	"mmx/internal/config"
	"mmx/internal/differences"
	"mmx/internal/facts"
	"mmx/internal/modules"
	"mmx/internal/protocol"
	"mmx/internal/value"
)

type Engine struct {
	CheckFileDestination func(context.Context, string) error
	OwedRestoresForTask  func(string, string, func() bool, modules.Runtime) modules.OwedRestoreService
	currentTaskPath      string
	currentTaskSecret    func() bool
	DiffDir              string
	diffs                differences.Store
	capture              *differences.Capture
	ModuleHelper         string
	job                  config.Job
	jobSecretTexts       []string
	results              []protocol.Result
	retained             int
	lastResult           protocol.Result
	assigned             map[string]any
	ran                  bool // the current task reached its resource
	RuntimeRoot          string
	// ControllerConnection comes from the node admission record.
	ControllerConnection modules.ControllerConnection
	// PrivateTaskDirectoryRoot returns the node's directory in which a module
	// makes the private directory of a node task that hands a tool a login
	// or a copy of a file for a check. The node makes and checks the
	// directory when a task first asks.
	PrivateTaskDirectoryRoot func() (string, error)
	// KeepHiddenDetails keeps, for root on the node, what the hidden result
	// of a failed node task leaves out, and returns the file that holds it.
	KeepHiddenDetails func(protocol.Result) (string, error)
	// hiddenDetailsFile is that file, once a node task of the job kept its
	// details there, and hiddenDetailsKept counts those node tasks.
	hiddenDetailsFile string
	hiddenDetailsKept int
	// StateRoot is the runner's state directory that survives a reboot; the
	// records of commands that follow a change live under it.
	StateRoot    string
	active       *taskBlockState
	checking     bool
	BeforeRecord func(string) error
	packages     modules.PackageManager
	newPackages  func() modules.PackageManager // tests replace the node's manager
	// BeforeMutation is also the fault-injection boundary exercised by tests.
	BeforeMutation func(string) error
	Report         func(protocol.Response) error
	facts          *facts.Snapshot
	collectFacts   func(string) facts.Snapshot
	// Machine reads the node's boot ID, the hash of its machine ID, and
	// whether it is a container, for the reboot task.
	Machine    NodeMachine
	rebootFrom string // the boot a reboot task leaves
	// previewPaths are the paths that earlier node tasks of this dryrun would
	// create or change; a path_exists condition cannot check them yet.
	previewPaths modules.PreviewPaths
	// earlierChange names the first node task of this dryrun that reported
	// would_change or unknown. A later check reads the node as it is now,
	// before that change, so its failure says nothing about the run.
	earlierChange string
	Starting      func(string) error
	StopRequested func() (bool, error)
}

var errStopped = errors.New("stop requested")

// errWhenTurnedTrueInItsBlock is the error of a tracked task whose when was
// false as its node task block started, with arguments that did not resolve
// then, and is true when the task comes.
var errWhenTurnedTrueInItsBlock = errors.New("the tracked node task's when condition was false when the node task block started, so the block did not resolve the task's " +
	"arguments or track the task's changes; when is now true because an earlier task in the block changed a value the condition reads. Set the condition's inputs before the node task block")

func (e *Engine) Run(ctx context.Context, j config.Job) protocol.Response {
	e.job = j
	e.jobSecretTexts = value.SecretValueTexts(j.Scope)
	e.diffs = differences.Store{Dir: e.DiffDir}
	// A dryrun's earlier fleet stages on the node count as earlier node tasks.
	e.results, e.retained, e.rebootFrom, e.previewPaths, e.earlierChange = nil, 0, "", nil, j.EarlierChangeInThisDryrun
	collect := e.collectFacts
	if collect == nil {
		collect = facts.Collect
	}
	observation := collect(j.Node)
	e.facts = &observation
	r := protocol.Response{ID: j.ID, Status: "succeeded", Facts: e.facts}
	manager, _ := e.facts.Values["package_manager"].(string)
	e.packages = modules.NewPackageManager(manager)
	if e.newPackages != nil {
		e.packages = e.newPackages()
	}
	if manager == "apt" && e.newPackages == nil && !j.Dryrun {
		if err := modules.RemoveLeftoverServicePolicy(ctx, e.runtime()); err != nil {
			r.Housekeeping = append(r.Housekeeping, "removing the apt service policy an interrupted run left: "+err.Error())
		}
	}
	e.active = nil
	e.checking = false
	scope := value.Scope(clone(j.Scope))
	scope["vars"], scope["inputs"], scope["node_tasks"], scope["node"] = clone(j.Vars), map[string]any{}, map[string]any{}, e.facts.Values
	config.SetOrigins(scope, j.Origins)
	scope.GuardOutputLines()
	for path, reason := range e.facts.Unavailable {
		scope.Mark(path, reason)
	}
	if e.Report != nil {
		if err := e.report(); err != nil {
			r.Status, r.Error = "failed", "saving node facts: "+err.Error()
			return r
		}
	}
	for path, reason := range j.Unavailable {
		scope.Mark(path, reason)
	}
	for path, reason := range j.OmittedEntries {
		scope.MarkOmittedEntry(path, reason)
	}
	err := e.nodeTasks(ctx, j.NodeTasks, scope, "")
	if err == nil {
		r.Outputs, err = config.ExportOutputs(j.Outputs, scope, j.Sources)
		// A secret output records where it came from, since only that is saved.
		r.Outputs, _ = value.FromNode(r.Outputs, value.NodeOrigin{FleetStage: j.FleetStage, Node: j.Node}).(map[string]any)
	}
	if err != nil {
		r.Outputs = nil
		failure := e.failure(err)
		r.Status, r.Error = failure.Status, failure.Reason
		if errors.Is(err, errStopped) {
			r.Status, r.Error = "stopped", ""
		}
	}
	if e.rebootFrom != "" {
		if r.Status == "succeeded" {
			r.Status, r.RebootFrom = "rebooting", e.rebootFrom
		} else { // the fleet stage's outputs failed after its reboot task
			last := &e.results[len(e.results)-1] //nolint:nilaway // Only a reboot task sets rebootFrom and its result is recorded before nodeTasks returns.
			last.Status, last.Change, last.Reason, last.Value = "skipped", "unchanged", "not rebooted: "+r.Error, nil
		}
	}
	r.Change = childChanges(e.results)
	r.Results = e.results
	return r
}

// pathExists answers a path_exists condition from the node's filesystem. A
// link counts as a file, whatever it points to.
func (e *Engine) pathExists(path string) (bool, error) {
	_, err := os.Lstat(path)
	if err == nil {
		return true, nil
	}
	if !errors.Is(err, os.ErrNotExist) && !errors.Is(err, syscall.ENOTDIR) {
		return false, fmt.Errorf("path_exists %s: %w", path, err)
	}
	if e.previewPaths.WouldChange(path) {
		return false, &value.UnavailableError{Path: path, Reason: "an earlier node task of this dryrun would create or change it or a path inside it, so the path is not yet checkable"}
	}
	return false, nil
}

// checkInADryrun answers for a check that a dryrun ran on the node as it is
// now: a check_new_file or a module's own check of a new file (failure_kind
// validation), a check in a node task block's validate phase, and a node
// task that only reads the node (wait_for, http with GET or HEAD, shell with
// changed_when: false). When an earlier node task of the dryrun would change
// the node, the run checks a node that differs, so a failure is not checked,
// and the check's output stays in the result. With nothing earlier to
// change, the failure stands. A missing prerequisite after an earlier
// change, such as a run_from_directory that does not exist yet, is not
// checked either: that change may provide it. An assert is no such check: it judges values,
// such as the approval that checks_before_restart_or_reload asks for.
//
// A validate check that the dryrun cannot run, such as a shell task without
// a dryrun command, reports no change while nothing earlier would change: a
// run then checks the configuration that is on the node now and changes
// nothing, so such a dryrun can still end without proposed changes.
func (e *Engine) checkInADryrun(t config.Task, r protocol.Result) protocol.Result {
	if t.Loop != nil || t.Role != "" || t.TaskBlock != nil {
		return r // each node task inside answers for itself
	}
	if e.checking && r.Status == "skipped" && r.Change == "unknown" && e.earlierChange == "" {
		r.Change = "unchanged"
	}
	if r.Status != "failed" || e.earlierChange == "" {
		return r
	}
	if r.FailureKind == "missing_prerequisite" {
		r.Status, r.FailureKind = "not_checked", ""
		r.Reason = modules.EarlierChangeMayProvide(e.earlierChange, r.Reason)
		return r
	}
	if r.FailureKind != "validation" && (r.FailureKind != "" || !(e.checking || onlyReadsTheNode(t))) {
		return r
	}
	r.Status, r.FailureKind = "not_checked", ""
	r.Reason = "an earlier node task, " + e.earlierChange + ", would change the node before the run reaches the check; the check failed against the node's current state: " + r.Reason
	return r
}

// onlyReadsTheNode is a node task that reads the node and changes nothing.
func onlyReadsTheNode(t config.Task) bool {
	if t.CoreModule().OnlyReads && t.Module != "assert" {
		return true
	}
	if _, inputs := t.ModuleCall(); inputs != nil {
		if reads, ok := inputs.(modules.OnlyReadsWhen); ok {
			return reads.OnlyReads() == nil
		}
	}
	return false
}

// taskPaths lists the paths a node task manages or says it creates, as far as
// its fields name them.
func taskPaths(t config.Task, r protocol.Result, scope value.Scope) []string {
	var named []string
	if _, inputs := t.ModuleCall(); inputs != nil {
		if m, ok := inputs.(interface{ ManagedPaths() []string }); ok {
			named = m.ManagedPaths()
		}
	}
	var paths []string
	for _, path := range named {
		if resolved, err := scope.Text(path); err == nil && strings.HasPrefix(resolved, "/") {
			paths = append(paths, resolved)
		}
	}
	if r.FileChange != nil {
		paths = append(paths, r.FileChange.Path)
	}
	return paths
}

func clone(m map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range m {
		out[k] = v
	}
	return out
}
func (e *Engine) nodeTasks(ctx context.Context, nodeTasks []config.Task, scope value.Scope, prefix string) error {
	for i, t := range nodeTasks {
		name := prefix + config.TaskName(t, i)
		local := boundScope(scope, t)
		var r protocol.Result
		e.ran = false
		keptBefore := e.hiddenDetailsKept
		if t.Loop != nil {
			r = e.loopTask(ctx, t, local, prefix, config.TaskName(t, i))
		} else {
			r = e.task(ctx, t, local, name)
		}
		if r.Status == "blocked" && e.job.Dryrun {
			r.Status = "not_checked"
		}
		if e.job.Dryrun {
			r = e.checkInADryrun(t, r)
		}
		if e.checking && !e.job.Dryrun {
			r.Change = "unchanged"
			// A check that its when or a shell condition skips is not a
			// failed check, as in a dryrun.
			if r.Status != "succeeded" && r.Status != "skipped" {
				r.Status, r.FailureKind = "failed", "validation"
				r.Reason = "required validation did not pass: " + r.Reason
			}
		}
		r.Task = name
		if r.HidesFailureDetails() {
			e.keepHiddenDetails(t, &r, keptBefore)
		}
		e.lastResult = r
		// Later tasks read the actual result; every message uses its safe form.
		safe := r.Safe()
		e.record(safe)
		if t.ID != "" {
			scope.Clear(value.Pointer("node_tasks", t.ID))
			scope["node_tasks"].(map[string]any)[t.ID] = value.FromNode(protocol.ResultValue(r), value.NodeOrigin{FleetStage: e.job.FleetStage, Node: e.job.Node, NodeTask: name})
			config.SetOrigin(scope, ".node_tasks."+t.ID, t.Sources.At(""))
			// A failed command, request, or module keeps what it captured for
			// failure handlers. Work that did not run, and any value, stays unavailable.
			if r.Status != "succeeded" {
				captured := []string{"exit_code", "stdout", "stdout_json", "stderr", "http_status_code", "response_body", "response_body_json"}
				unavailable := append(captured, "value", "outputs", "items", "content", "public_key")
				if r.Status == "failed" && e.ran && t.Loop == nil && (t.CoreModule().KeepsOutputOfAFailure || t.CallsModuleThatIsNotCore()) {
					unavailable = unavailable[len(captured):]
				}
				for _, field := range unavailable {
					scope.Mark(value.Pointer("node_tasks", t.ID, field), name+" is "+r.Status+": "+safe.Reason)
				}
			}
			// A module that is not core leaves out an output it does not know:
			// it stopped before it, or a dryrun cannot know it yet.
			if module := e.job.Modules[t.Module]; t.CallsModuleThatIsNotCore() && module.Description != nil && t.Loop == nil {
				for _, output := range module.Description.Outputs {
					if _, reported := r.ModuleOutputs[output.Name]; reported {
						continue
					}
					if r.Status != "succeeded" {
						scope.Mark(value.Pointer("node_tasks", t.ID, output.Name), name+" is "+r.Status+": "+safe.Reason)
					} else if e.job.Dryrun {
						scope.Mark(value.Pointer("node_tasks", t.ID, output.Name), name+" reports no "+output.Name+" in this dryrun; the run reports it")
					}
				}
			}
			// A dryrun cannot know some results of what it would create.
			_, inputs := t.ModuleCall()
			if unknown, ok := inputs.(interface {
				UnknownInADryrun(protocol.Result) map[string]string
			}); ok && e.job.Dryrun {
				for field, reason := range unknown.UnknownInADryrun(r) {
					if t.Loop == nil {
						scope.Mark(value.Pointer("node_tasks", t.ID, field), name+" "+reason)
					}
				}
				for i, item := range r.Items {
					for field, reason := range unknown.UnknownInADryrun(item) {
						scope.Mark(value.Pointer("node_tasks", t.ID, "items", strconv.Itoa(i), field), name+"["+item.IterationKey+"] "+reason)
					}
				}
			}
			// A dryrun predicts a change; only the run knows the change that
			// the node task reports.
			if e.job.Dryrun && (r.Change == "would_change" || r.Change == "unknown") {
				scope.Mark(value.Pointer("node_tasks", t.ID, "change"), name+" reports "+r.Change+" in a dryrun; the run knows whether it changes the node")
			}
		}
		if t.SetVars != nil && t.Loop == nil && r.Status == "succeeded" {
			config.AssignVars(scope, t, e.assigned)
		} else if t.SetVars != nil && t.Loop == nil && r.Status == "not_checked" {
			// A failed set_vars assigns nothing and keeps the earlier values.
			// After one that a dryrun did not check, a later read gets the
			// reason instead of a value that the run would replace.
			for key := range t.SetVars {
				scope.Mark(value.Pointer("vars", key), name+" is "+r.Status+": "+safe.Reason)
			}
		}
		if e.active != nil && e.job.Dryrun && (r.Change == "would_change" || r.Change == "unknown") {
			e.active.previewChanged = true
		}
		if e.job.Dryrun && (r.Change == "would_change" || r.Change == "unknown") {
			e.previewPaths = append(e.previewPaths, taskPaths(t, r, scope)...)
			if e.earlierChange == "" {
				e.earlierChange = name
			}
		}
		if e.Report != nil {
			if err := e.report(); err != nil {
				return fmt.Errorf("saving task results: %w", err)
			}
		}
		switch r.Status {
		case "stopped":
			return errStopped
		case "blocked":
			return &value.UnavailableError{Path: name, Reason: safe.Reason}
		case "failed":
			err := fmt.Errorf("%s: %s", name, safe.Reason)
			if r.ReasonReadsNoSecret {
				return failureWhoseReasonReadsNoSecret{err}
			}
			return err
		}
	}
	return nil
}

// failureWhoseReasonReadsNoSecret is the error of a node task that failed
// with a reason that reads no secret (protocol.Result.ReasonReadsNoSecret).
// The role call, node task block, or node task that fails with it keeps
// its reason too (Engine.failure): all it adds is MMX's own text and the
// safe reasons of other node tasks.
type failureWhoseReasonReadsNoSecret struct{ error }

func (e *Engine) task(ctx context.Context, t config.Task, scope value.Scope, name string) (result protocol.Result) {
	scope, trace := scope.Trace()
	trace.Task = name
	previousPath, previousSecret := e.currentTaskPath, e.currentTaskSecret
	e.currentTaskPath, e.currentTaskSecret = name, func() bool { return trace.Used }
	defer func() { e.currentTaskPath, e.currentTaskSecret = previousPath, previousSecret }()
	var capture *differences.Capture
	defer func() {
		if trace.Used {
			result.Secret, result.SecretBecause = true, trace.HiddenBecause()
		}
		// A module that runs on the node may have left a secret it read
		// there for a later node task to read back; assert only decides.
		if t.Module != "" && t.Module != "assert" {
			trace.HidesLaterTasks()
		}
		texts := append(scope.SecretTexts(), e.jobSecretTexts...)
		result = result.WithoutSecretCommandOutput(texts)
		if capture != nil {
			result = capture.Finish(result)
		}
	}()
	if err := ctx.Err(); err != nil {
		return e.failure(err)
	}
	// A grouped change finishes activation and cleanup before stopping.
	if e.active == nil && e.StopRequested != nil {
		stop, err := e.StopRequested()
		if err != nil {
			return e.failure(err)
		}
		if stop {
			return e.failure(errStopped)
		}
	}
	if e.Starting != nil {
		if err := e.Starting(name); err != nil {
			return e.failure(err)
		}
	}
	ok, err := scope.PredicateOnNode(t.When, e.pathExists)
	if err != nil {
		return e.failure(config.SourceError(t.Sources, "when", t.When, scope, err))
	}
	if !ok {
		return protocol.Result{Status: "skipped", Change: "unchanged", Reason: "when is false"}
	}
	if t.WhenWasFalseAtBlockStart {
		r := e.failure(config.SourceError(t.Sources, "when", t.When, scope, errWhenTurnedTrueInItsBlock))
		r.FailureKind = "config"
		return r
	}
	start := len(e.results)
	switch {
	case t.Role != "":
		child, err := config.ResolveRoleInputs(t, scope, false)
		if err != nil {
			r := e.failure(err)
			r.FailureKind = "config"
			return r
		}
		err = e.nodeTasks(ctx, t.RoleNodeTasks, child, name+"/")
		var outputs map[string]any
		if err == nil {
			outputs, err = config.ExportOutputs(t.RoleOutputs, child, t.RoleSources)
		}
		r := protocol.Success("unchanged")
		if err != nil {
			r = e.failure(err)
		}
		r.Outputs, r.Change = outputs, childChanges(e.results[start:])
		return r
	case t.TaskBlock != nil:
		err := e.taskBlock(ctx, t, scope, name)
		r := protocol.Success("unchanged")
		if err != nil {
			r = e.failure(err)
		}
		r.Change = childChanges(e.results[start:])
		return r
	default:
		capture = e.diffs.Task()
		previous := e.capture
		e.capture = capture
		defer func() { e.capture = previous }()
		return e.timed(ctx, t, scope)
	}
}

func (e *Engine) loopTask(ctx context.Context, t config.Task, scope value.Scope, prefix, name string) protocol.Result {
	var items []value.Iteration
	if t.Prepared == nil {
		var err error
		items, err = scope.Iterations(t.Loop, t.As, t.Key)
		if err != nil {
			return e.failure(config.SourceError(t.Sources, "loop", t.Loop, scope, err))
		}
	}
	if t.Prepared != nil {
		items = make([]value.Iteration, len(t.Prepared))
		for i, child := range t.Prepared {
			items[i] = value.Iteration{Key: child.Key.(string), Value: child.Bindings[t.As]}
		}
	}
	r := protocol.Success("unchanged")
	for i, item := range items {
		local := scope.Local()
		local[t.As] = item.Value
		child := t
		if t.Prepared != nil {
			child = t.Prepared[i]
		}
		child.Loop, child.Prepared = nil, nil
		child.ID = name + "[" + value.EscapeKey(item.Key) + "]"
		err := e.nodeTasks(ctx, []config.Task{child}, local, prefix)
		result := e.lastResult
		result.IterationKey = item.Key
		r.Items = append(r.Items, result)
		r.Change = protocol.CombineChange(r.Change, result.Change)
		if err != nil || protocol.Rank(result.Status) > protocol.Rank(r.Status) {
			r.Status, r.Reason, r.ReasonReadsNoSecret = result.Status, result.Safe().Reason, result.ReasonReadsNoSecret
		}
		if err != nil {
			return r
		}
	}
	return r
}
func (e *Engine) failure(err error) protocol.Result {
	r := protocol.Failed(err)
	r.ReasonReadsNoSecret = errors.As(err, new(failureWhoseReasonReadsNoSecret))
	if errors.Is(err, errStopped) {
		r.Status, r.Reason = "stopped", "stop requested"
	}
	if r.Status == "blocked" && e.job.Dryrun {
		r.Status = "not_checked"
	}
	return r
}

func childChanges(results []protocol.Result) string {
	change := "unchanged"
	for _, r := range results {
		change = protocol.CombineChange(change, r.Change)
	}
	return change
}

// Resources receive their public identifiers resolved and checked; they
// resolve only private values, through s.
func (e *Engine) execute(ctx context.Context, t config.Task, s value.Scope) protocol.Result {
	// The loader's link from a task to its module that is not core does not
	// travel in the node job; the job's own modules restore it, so that the
	// checks below know which inputs the module's description marks private.
	if t.CallsModuleThatIsNotCore() && t.NonCoreModule == nil {
		if module, sent := e.job.Modules[t.Module]; sent {
			t.NonCoreModule = &module
		}
	}
	t, err := config.ResolveArguments(t, s)
	if err == nil {
		_, err = config.InspectArguments(t, s, false)
	}
	if err != nil {
		return protocol.ConfigurationError(err)
	}
	e.preview(ctx, t, s)
	e.ran = true
	// A login that the module handed only to its tool leaves the result
	// here, before anything reads it: a retry's condition, a later node task,
	// the job's records, the error of the node job, and the controller.
	r := e.resource(ctx, t, s).WithoutLoginsHandedOnlyToATool(s.LoginsHandedOnlyToATool())
	e.recordFingerprint(t, &r)
	_, inputs := t.ModuleCall()
	// In dryrun, a service or command of the block compares with the commit
	// a checkout would move to, what an installer would install, and the
	// versions a package task would install.
	if e.job.Dryrun && e.active != nil && e.active.states != nil {
		if stated, ok := inputs.(interface {
			StateAfter(protocol.Result) (string, bool)
		}); ok {
			if state, known := stated.StateAfter(r); known {
				for _, in := range config.TrackedInputs(t) {
					e.active.states[in.Key()] = state
				}
			}
		}
		if packages, ok := inputs.(interface {
			ExpectedAfter(protocol.Result) map[string]modules.PackageExpectation
		}); ok {
			maps.Copy(e.active.installed, packages.ExpectedAfter(r))
		}
	}
	// A changed file or link can be a package source; the manager decides.
	if r.Change == "changed" {
		for _, in := range config.TrackedInputs(t) {
			if in.Kind == "config" || in.Kind == "unit" {
				e.packages.PathChanged(in.Path)
			}
		}
	}
	return r
}

// nodeTask is what the core module that t names gets when it runs.
func (e *Engine) nodeTask(t config.Task, s value.Scope) modules.NodeTask {
	task := modules.NodeTask{
		Runtime:        e.runtime(),
		Scope:          s,
		NodeName:       e.job.Node,
		Packages:       e.packages,
		Checking:       e.checking,
		PreviewChanged: e.active != nil && e.active.previewChanged,
		InputError: func(field string, written any, err error) error {
			return config.SourceError(t.Sources, t.Module+"."+field, written, s, err)
		},
		TaskBlock: taskBlockServices{e, t, s},
	}
	if e.OwedRestoresForTask != nil {
		task.OwedRestores = e.OwedRestoresForTask(e.currentTaskPath, t.Module, e.currentTaskSecret, task.Runtime)
	}
	if e.active != nil {
		task.PreviewPathsOfTaskBlock = e.previewPaths[e.active.previewPathsBefore:]
	}
	return task
}

func (e *Engine) resource(ctx context.Context, t config.Task, s value.Scope) protocol.Result {
	if _, inputs := t.ModuleCall(); inputs != nil {
		return inputs.(modules.Inputs).Run(ctx, e.nodeTask(t, s))
	}
	switch {
	case t.CallsModuleThatIsNotCore():
		module := e.job.Modules[t.Module]
		var r protocol.Result
		if module.IsStarlark() {
			r = e.runtime().Starlark(ctx, t.Module, module, t.NonCoreModuleInputs(), s)
		} else {
			r = e.runtime().External(ctx, t.Module, module, t.NonCoreModuleInputs(), s, e.ModuleHelper)
		}
		r.ModuleKind = module.Kind
		return r
	case t.Show != nil:
		v, err := s.Resolve(t.Show)
		if err != nil {
			return protocol.Failed(err)
		}
		r := protocol.Success("unchanged")
		r.Value = v
		return r
	case t.SetVars != nil:
		// tasks assigns the values beside the result, outside this task's trace copy.
		assigned := map[string]any{}
		for _, key := range slices.Sorted(maps.Keys(t.SetVars)) {
			v, err := s.Evaluate(t.SetVars[key])
			if err != nil {
				return protocol.Failed(t.Sources.Error("set_vars."+key, err))
			}
			assigned[key] = v
		}
		e.assigned = assigned
		return protocol.Success("unchanged")
	default:
		return protocol.Failed(fmt.Errorf("unsupported node task"))
	}
}

// taskBlockServices gives a module the records of the node task block it runs
// in, and the node job's reboot.
type taskBlockServices struct {
	e     *Engine
	t     config.Task
	scope value.Scope
}

func (b taskBlockServices) RefreshService(ctx context.Context, s modules.Service) protocol.Result {
	return b.e.service(ctx, s)
}

func (b taskBlockServices) RunCommandThatFollowsTrackedInputs(ctx context.Context, recordKey string, run func() protocol.Result) protocol.Result {
	return b.e.followingCommand(ctx, recordKey, run)
}

func (b taskBlockServices) Reboot(ctx context.Context, r modules.Reboot) protocol.Result {
	return b.e.reboot(ctx, r)
}

// reboot requests a reboot as its fleet stage's last task. Its guards run as a shell
// task's do. A node that is the machine that runs the controller process
// then refuses, in a dryrun as in a run: the controller process asked the
// same question before the node job, and this is the check just before the
// reboot. Dryrun reports would_change. A run records the boot the node
// leaves; the job then ends rebooting, and its worker reboots once that
// result is durable.
func (e *Engine) reboot(ctx context.Context, r modules.Reboot) protocol.Result {
	if stop := modules.PathGuards(r.SkipIfThisPathExists, r.RunOnlyIfThisPathExists, ""); stop != nil {
		return *stop
	}
	if stop := modules.Guards(ctx, r.OnlyIf, r.OnlyIfNot, nil, "", nil); stop != nil {
		return *stop
	}
	if e.job.ControllerMachine != nil {
		refusal, err := e.Machine.RebootRefusal(*e.job.ControllerMachine)
		if err != nil {
			err = RebootNotChecked(err)
		} else if refusal != "" {
			err = errors.New(refusal)
		}
		if err != nil {
			refused := protocol.Failed(err)
			refused.Change = "unchanged"
			return refused
		}
	}
	if e.job.Dryrun {
		return protocol.Result{Status: "succeeded", Change: "would_change", Reason: "the node would reboot"}
	}
	if err := e.runtime().Mutation("reboot"); err != nil {
		return protocol.Failed(err)
	}
	boot, err := e.Machine.CurrentBootID()
	if err != nil {
		return protocol.Failed(err)
	}
	e.rebootFrom = boot
	return protocol.Result{Status: "rebooting", Change: "changed", Reason: "waiting for the node to return on a new boot", Value: map[string]any{"boot_id_before": boot}}
}

// Resource implementations receive execution policy and a bounded diagnostic sink.
func (e *Engine) runtime() modules.Runtime {
	r := modules.Runtime{ControllerConnection: e.ControllerConnection, CheckFileDestination: e.CheckFileDestination, Dryrun: e.job.Dryrun, BeforeMutation: e.BeforeMutation, ResolveShell: resolveShell, PrivateTaskDirectoryRoot: e.PrivateTaskDirectoryRoot, PreviewPaths: e.previewPaths, EarlierChange: e.earlierChange}
	if e.capture != nil {
		r.RetainDiff = e.capture.Add
	}
	return r
}

// One budget bounds a job's retained result records, measured as the JSON of
// the status answer (escaping included), so that answer stays well under
// protocol.ResponseLimit (64 MiB) beside fleet stage outputs and the facts snapshot.
// A record keeps its text while the job has used under resultBudget. Otherwise
// it keeps its outcome, exit code, and reason and states what it omits.
// Evaluation never reads these records: later node tasks, predicates, and outputs
// use the actual result.
const (
	resultBudget  = 16 << 20
	reasonLimit   = 4 << 10
	retainedTries = 10
)

// keepHiddenDetails gives the node what the hidden result of a failed node
// task leaves out, so that root can read the reason there. A role call, a
// node task block, or a loop that failed because a node task inside kept
// its details names the same file.
func (e *Engine) keepHiddenDetails(t config.Task, r *protocol.Result, keptBefore int) {
	if t.Role != "" || t.TaskBlock != nil || t.Loop != nil {
		if e.hiddenDetailsKept > keptBefore {
			r.HiddenDetailsFile = e.hiddenDetailsFile
		}
		return
	}
	if e.KeepHiddenDetails == nil {
		return
	}
	// When the node cannot write the file, the reason names none, and the
	// details stay hidden.
	if file, err := e.KeepHiddenDetails(*r); err == nil {
		r.HiddenDetailsFile, e.hiddenDetailsFile = file, file
		e.hiddenDetailsKept++
	}
}

func (e *Engine) record(safe protocol.Result) {
	if b, err := json.Marshal(safe); err != nil || e.retained+len(b) > resultBudget {
		safe = omitDetails(safe)
	} else {
		e.retained += len(b)
	}
	e.results = append(e.results, safe)
}

func omitDetails(r protocol.Result) protocol.Result {
	reason := r.Reason
	if len(reason) > reasonLimit {
		reason = strings.ToValidUTF8(reason[:reasonLimit], "") + " [reason shortened]"
	}
	brief := protocol.Result{OwedRestore: r.OwedRestore, Task: r.Task, IterationKey: r.IterationKey, Status: r.Status, Change: r.Change, ExitCode: r.ExitCode,
		FailureKind: r.FailureKind, Secret: r.Secret, ReasonReadsNoSecret: r.ReasonReadsNoSecret, OutputHoldsNoSecret: r.OutputHoldsNoSecret, Reason: reason, TriesOmitted: r.TriesOmitted + len(r.Tries),
		Omitted: fmt.Sprintf("output, values, and examples omitted: %d MiB job result budget exhausted", resultBudget>>20)}
	brief.Items = omittedRestoreResults(r.Items)
	brief.Tries = omittedRestoreResults(r.Tries)
	brief.TriesOmitted -= len(brief.Tries)
	if r.CheckNewFile != nil {
		v := omitDetails(*r.CheckNewFile)
		brief.CheckNewFile = &v
	}
	if r.FileChange != nil {
		c := *r.FileChange
		if c.Diff != "" {
			c.Diff, c.DiffOmitted = "", brief.Omitted
		}
		brief.FileChange = &c
	}
	if r.DirectoryChange != nil {
		d := *r.DirectoryChange
		d.Examples = nil
		brief.DirectoryChange = &d
	}
	return brief
}

func (e *Engine) report() error {
	return e.Report(protocol.Response{ID: e.job.ID, Status: "running", Results: e.results, Facts: e.facts})
}

// resolveShell resolves a nested shell task's public arguments, such as
// check_new_file's.
func resolveShell(sh modules.Shell, s value.Scope) (modules.Shell, error) {
	t, err := config.ResolveArguments(config.Task{Module: "shell", ModuleInputs: &sh}, s)
	if err != nil {
		return sh, err
	}
	return *t.ModuleInputs.(*modules.Shell), nil
}

// recordFingerprint keeps the fingerprint a module that is not core reported
// in a node task block whose services or commands follow it. Such a module
// must report one.
func (e *Engine) recordFingerprint(t config.Task, r *protocol.Result) {
	if e.active == nil || !t.CallsModuleThatIsNotCore() {
		return
	}
	key, tracked := e.active.moduleKeys[config.ModuleFingerprintKey(t)]
	if !tracked {
		return
	}
	// A try that failed after it changed something counts too: the next
	// try, which succeeds, finds nothing left to change.
	if r.Change != "unchanged" {
		if e.active.changedByModule == nil {
			e.active.changedByModule = map[string]bool{}
		}
		e.active.changedByModule[key] = true
	}
	if r.Status != "succeeded" {
		return
	}
	if r.Fingerprint == "" {
		r.Status, r.Reason = "failed", "module "+t.Module+" is in a node task block's changes phase; the block has service actions or commands that depend on changed inputs, so the module result must include a fingerprint"
		return
	}
	if e.active.fingerprints == nil {
		e.active.fingerprints = map[string]string{}
	}
	e.active.fingerprints[key] += r.Fingerprint + "\n"
}

// Grouped tasks may own the only task-tree copy of a restore outcome. Keep
// those small branches when the ordinary output/details budget is exhausted.
func omittedRestoreResults(results []protocol.Result) []protocol.Result {
	var kept []protocol.Result
	for _, result := range results {
		brief := omitDetails(result)
		if brief.OwedRestore != nil || len(brief.Items) > 0 || len(brief.Tries) > 0 {
			kept = append(kept, brief)
		}
	}
	return kept
}
