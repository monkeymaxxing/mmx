package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"mmx/internal/config"
	"mmx/internal/durable"
	"mmx/internal/modules"
	"mmx/internal/protocol"
	"mmx/internal/value"
)

// A commandRecord is what a shell task with only_when_tracked_inputs_changed
// last ran with: one hash per tracked input of its task block, keyed as in a
// service record. Unlike a service record it survives a reboot, because boot
// does not run the command, and it has no running instance to compare.
//
// Confirmed is false from just before the command starts until it succeeded,
// so a command whose outcome was lost runs again.
//
// A record is keyed by the project, the fleet stage loop iteration, and the
// task's result path. FleetStageIteration is empty outside fleet stage loops,
// and in every record of release 0.1.0, whose key had no iteration.
type commandRecord struct {
	Version             int               `json:"version"`
	Project             string            `json:"project"`
	FleetStageIteration string            `json:"fleet_stage_iteration,omitempty"`
	Task                string            `json:"task"`
	Inputs              map[string]string `json:"inputs"`
	Confirmed           bool              `json:"confirmed"`
}

type commandPlan struct {
	task   string // the task's result path, which with the project is the record's key
	record *commandRecord
	// unreported are the module inputs whose fingerprint the record lacked
	// when the task block began, as in a servicePlan.
	unreported map[string]bool
}

// blockChecks is a task block's checks_before_restart_or_reload phase. It
// runs at most once in an attempt.
type blockChecks struct {
	tasks  []config.Task
	scope  value.Scope
	prefix string
	ran    bool
}

// fleetStageIteration names the fleet stage loop iteration that a node job
// runs in: the job's fleet stage path up to its innermost iteration key, such
// as rollout[web1] for rollout[web1]/drain, or "" outside fleet stage loops.
// Iterations that reach one node, such as a helper node, need a command
// record each, or one iteration's success would hide another's owed command.
// A fleet stage id holds no "]", and an iteration key's "]" is escaped.
func fleetStageIteration(fleetStage string) string {
	return fleetStage[:strings.LastIndex(fleetStage, "]")+1]
}

// commandPath is the file of a command record. Outside fleet stage loops the
// key is the project and the task path, as in release 0.1.0.
func (e *Engine) commandPath(iteration, task string) string {
	key := e.job.Project + "\x00" + task
	if iteration != "" {
		key = e.job.Project + "\x00" + iteration + "\x00" + task
	}
	sum := sha256.Sum256([]byte(key))
	return filepath.Join(e.StateRoot, "command_records", hex.EncodeToString(sum[:])+".json")
}

func (e *Engine) readCommandRecord(iteration, task string) (*commandRecord, error) {
	if !filepath.IsAbs(e.StateRoot) {
		return nil, fmt.Errorf("internal error: the command record state directory requires an absolute path")
	}
	b, err := os.ReadFile(e.commandPath(iteration, task))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var r commandRecord
	if err := protocol.Decode(b, &r); err != nil {
		return nil, fmt.Errorf("invalid command record for %s: %w", task, err)
	}
	if r.Version != 1 || r.Project != e.job.Project || r.FleetStageIteration != iteration || r.Task != task || !validInputs(r.Inputs) {
		return nil, fmt.Errorf("invalid command record for %s", task)
	}
	if r.Inputs == nil {
		r.Inputs = map[string]string{}
	}
	return &r, nil
}

func (e *Engine) saveCommandRecord(r *commandRecord, phase string) error {
	if e.job.Dryrun {
		return fmt.Errorf("internal error: command record write during dryrun")
	}
	if e.BeforeRecord != nil {
		if err := e.BeforeRecord("command " + phase); err != nil {
			return err
		}
	}
	path := e.commandPath(r.FleetStageIteration, r.Task)
	if err := privateDirectory(filepath.Dir(path)); err != nil {
		return err
	}
	return durable.JSON(path, r)
}

// prepareCommand reads the record of one command that follows tracked inputs
// and, before the task block's first write, saves the inputs as they are now
// for every input the record lacks. A missing record therefore never runs the
// command by itself, and a run that stops after a write leaves a record the
// next run compares with. For a module that is not core the record gets
// unreportedFingerprint, as a service record does. A dryrun makes the same
// record and does not save it.
//
// In a fleet stage loop iteration without its own record, the record that
// release 0.1.0 kept under the key without the iteration is the starting
// record, so a command that release left owed still runs. That record cannot
// say which iteration owed the command, so each iteration that reaches the
// node starts from it.
func (e *Engine) prepareCommand(state *taskBlockState, task *config.Task, command *modules.Shell, blockName string) error {
	path := blockName + "/activate/" + task.ID
	if key, loop := task.Key.(string); loop {
		path += "[" + value.EscapeKey(key) + "]"
	}
	if _, exists := state.commands[path]; exists {
		return fmt.Errorf("duplicate command record %s", path)
	}
	iteration := fleetStageIteration(e.job.FleetStage)
	record, err := e.readCommandRecord(iteration, path)
	if err != nil {
		return err
	}
	added := record == nil
	if record == nil && iteration != "" {
		if record, err = e.readCommandRecord("", path); err != nil {
			return err
		}
		if record != nil {
			record.FleetStageIteration = iteration
		}
	}
	if record == nil {
		record = &commandRecord{Version: 1, Project: e.job.Project, FleetStageIteration: iteration, Task: path, Inputs: map[string]string{}, Confirmed: true}
	}
	for k, v := range state.before {
		if _, ok := record.Inputs[k]; !ok {
			record.Inputs[k], added = v, true
		}
	}
	unreported := awaitFingerprints(record.Inputs, state.inputs.moduleInputs)
	if (added || len(unreported) > 0) && !e.job.Dryrun {
		if err := e.saveCommandRecord(record, "baseline"); err != nil {
			return fmt.Errorf("save initial tracked inputs for the command: %w", err)
		}
	}
	command.CommandRecordKey = path
	state.commands[path] = &commandPlan{task: path, record: record, unreported: unreported}
	return nil
}

// commandNeeded reports whether a command must run for these input hashes,
// and why.
func commandNeeded(plan *commandPlan, current map[string]string, dryrun bool) (bool, string) {
	if !plan.record.Confirmed {
		return true, "the last run of this command did not finish"
	}
	reason := changedInputNames(plan.record.Inputs, current)
	if reason == "" {
		return false, ""
	}
	if dryrun {
		return true, reason + " would change"
	}
	return true, reason + " changed"
}

// changedInputNames names the tracked inputs whose hash in current differs
// from base, in the words of a result reason: "tracked input /etc/app.yml" or
// "tracked inputs /etc/app.yml and 2 more", and "" when none differs. With
// kinds, such as "config", only inputs of those kinds count.
func changedInputNames(base, current map[string]string, kinds ...string) string {
	var changed []string
	for k, v := range current {
		kind, _, _ := strings.Cut(k, ":")
		if base[k] != v && (len(kinds) == 0 || slices.Contains(kinds, kind)) {
			changed = append(changed, inputName(k))
		}
	}
	if len(changed) == 0 {
		return ""
	}
	slices.Sort(changed)
	if len(changed) == 1 {
		return "tracked input " + changed[0]
	}
	return fmt.Sprintf("tracked inputs %s and %d more", changed[0], len(changed)-1)
}

// inputName is a record key in the words of a result reason.
func inputName(key string) string {
	kind, name, _ := strings.Cut(key, ":")
	switch kind {
	case "package":
		return "package " + name
	case "git":
		return "checkout " + name
	case "installed":
		return "installation of " + name
	case "module": // the name ends with the module, after its project, fleet stage, and block
		return "module " + name[strings.LastIndex(name, "\x00")+1:]
	}
	return name
}

// followingCommand runs a shell task with only_when_tracked_inputs_changed
// when its task block's tracked inputs differ from its record. The record is
// saved as unconfirmed before the command and confirmed after its success.
func (e *Engine) followingCommand(ctx context.Context, recordKey string, run func() protocol.Result) protocol.Result {
	var plan *commandPlan
	if e.active != nil {
		plan = e.active.commands[recordKey]
	}
	if plan == nil {
		return protocol.Failed(fmt.Errorf("internal error: a command with only_when_tracked_inputs_changed has no record plan"))
	}
	var preview inputPreview
	if e.job.Dryrun {
		preview = e.active.inputPreview
	}
	current, err := e.inputHashes(ctx, e.active.inputs, preview)
	if err != nil {
		return protocol.Failed(err)
	}
	// All changes of the task block have run. A module that changed nothing
	// reported what the command last ran with, and a module whose tasks did
	// not run leaves nothing owed.
	settled := e.active.takeFingerprintBaselines(plan.record.Inputs, plan.unreported, current)
	settled = e.active.forgetUnreportedFingerprints(plan.record.Inputs, plan.unreported) || settled
	if settled && !e.job.Dryrun {
		if err := e.saveCommandRecord(plan.record, "baseline"); err != nil {
			return protocol.Failed(fmt.Errorf("save initial tracked inputs for the command: %w", err))
		}
	}
	needed, reason := commandNeeded(plan, current, e.job.Dryrun)
	if !needed {
		return protocol.Result{Status: "skipped", Change: "unchanged", Reason: "tracked inputs are unchanged since this command last ran"}
	}
	if stop := e.requireChecks(ctx); stop != nil {
		return *stop
	}
	if e.job.Dryrun {
		r := run()
		// Without a dryrun command the script says nothing; the record says
		// that it would run.
		if r.Status == "skipped" && r.Change == "unknown" {
			r = protocol.Success("would_change")
		}
		if r.Status == "succeeded" {
			r.Reason = "would run: " + reason
		}
		return r
	}
	inputs := maps.Clone(plan.record.Inputs)
	maps.Copy(inputs, current)
	record := &commandRecord{Version: 1, Project: e.job.Project, FleetStageIteration: plan.record.FleetStageIteration, Task: plan.task, Inputs: inputs}
	if err := e.saveCommandRecord(record, "unconfirmed"); err != nil {
		return protocol.Failed(fmt.Errorf("record the command attempt before running the command: %w", err))
	}
	plan.record = record
	r := run()
	if r.Status != "succeeded" {
		return r
	}
	r.Reason = "ran because " + reason
	record.Confirmed = true
	if err := e.saveCommandRecord(record, "confirmed"); err != nil {
		r.Status, r.Reason = "failed", fmt.Sprintf("the command succeeded, but its record was not saved, so the next run runs it again: %v", err)
	}
	return r
}

// wouldRefresh reports, before a task block's first write, whether applying
// the block would restart or reload a service that is running now or run a
// command that follows tracked inputs. It makes the comparison a dryrun
// makes: what the tracked node tasks would install against each record. When
// that cannot be told yet, the answer is yes, so the checks run.
func (e *Engine) wouldRefresh(ctx context.Context, state *taskBlockState, s value.Scope) (bool, error) {
	preview := newInputPreview()
	for _, task := range state.tracked {
		t := *task
		scope := boundScope(s, t)
		applies, err := scope.PredicateOnNode(t.When, e.pathExists)
		if err != nil {
			return true, nil
		}
		if !applies {
			continue
		}
		preview.add(ctx, t, scope)
		_, inputs := t.ModuleCall()
		// The state that a checkout, an image, or an installation would have
		// is what a dryrun of its node task reports. The dryrun gets what the
		// task itself will get, so a git task reaches a private repository
		// with its credentials.
		if stated, ok := inputs.(interface {
			modules.Inputs
			StateAfter(protocol.Result) (string, bool)
		}); ok {
			dryrun := e.nodeTask(t, scope)
			dryrun.Runtime.Dryrun, dryrun.Runtime.RetainDiff = true, nil
			state, known := stated.StateAfter(stated.Run(ctx, dryrun))
			if !known {
				return true, nil
			}
			for _, in := range config.TrackedInputs(t) {
				preview.states[in.Key()] = state
			}
		}
	}
	for _, plan := range state.services {
		observed, err := plan.manager.Inspect(ctx, plan.unit)
		if err != nil {
			return false, err
		}
		if !running(observed) || !refreshes(plan.task) {
			continue
		}
		current, err := e.inputHashes(ctx, plan, preview)
		if err != nil {
			return true, nil
		}
		if action, _ := refreshAction(plan.task, plan, observed, current); action != "" {
			return true, nil
		}
	}
	if len(state.commands) == 0 {
		return false, nil
	}
	current, err := e.inputHashes(ctx, state.inputs, preview)
	if err != nil {
		return true, nil
	}
	for _, plan := range state.commands {
		if needed, _ := commandNeeded(plan, current, true); needed {
			return true, nil
		}
	}
	return false, nil
}

// requireChecks runs the task block's checks_before_restart_or_reload when a
// service task or command is about to act and they have not run in this
// attempt: the comparison before the first write could not see this action
// coming. It returns the acting task's failure when a check fails.
func (e *Engine) requireChecks(ctx context.Context) *protocol.Result {
	if e.active == nil || e.active.checks == nil || e.active.checks.ran {
		return nil
	}
	if err := e.runChecks(ctx, e.active.checks); err != nil {
		r := e.failure(err)
		r.Change = "unchanged"
		return &r
	}
	return nil
}

func (e *Engine) runChecks(ctx context.Context, checks *blockChecks) error {
	checks.ran = true
	return e.nodeTasks(ctx, checks.tasks, checks.scope, checks.prefix)
}

// skipChecks records each check as skipped when the task block restarted and
// reloaded nothing that was running.
func (e *Engine) skipChecks(checks *blockChecks) error {
	for i, t := range checks.tasks {
		e.record(protocol.Result{Task: checks.prefix + config.TaskName(t, i), Status: "skipped", Change: "unchanged",
			Reason: "not needed: this task block restarted or reloaded no running service and ran no command that follows a change"})
	}
	if e.Report != nil {
		return e.report()
	}
	return nil
}
