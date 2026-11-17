package engine

import (
	"context"
	"fmt"
	"maps"
	"os"
	"strings"

	"mmx/internal/modules"
	"mmx/internal/protocol"
)

func (e *Engine) service(ctx context.Context, sv modules.Service) (result protocol.Result) {
	// wanted: the task starts or enables the unit.
	wanted := sv.State == "running" || (sv.StartAtBoot != nil && *sv.StartAtBoot)
	mask := sv.Masked != nil && *sv.Masked
	m, err := modules.SystemdOf(sv.InSystemdOfUserAccount)
	if err != nil && !wanted && !mask && modules.AccountIsGone(err) {
		r := protocol.Success("unchanged")
		r.Reason = "account " + sv.InSystemdOfUserAccount + " does not exist, so it has no unit that runs or is enabled"
		return r
	}
	if err != nil {
		return e.runtime().AccountFailure(fmt.Errorf("systemd_service.in_systemd_of_user_account: %w", err))
	}
	managerChange, notice := "unchanged", ""
	defer func() {
		if managerChange == "changed" {
			result.Change = "changed"
		}
		if managerChange == "would_change" && result.Change == "unchanged" {
			result.Change = "would_change"
		}
		if notice != "" && result.Status == "succeeded" && result.Reason == "" {
			result.Reason = notice
		}
	}()
	// A unit of an account that this task wants started or enabled must not
	// stop when the account logs out, and the account's systemd must run
	// before the unit can be started. A task that only stops, disables, or
	// masks leaves lingering alone: without a running systemd the unit is
	// stopped, and Inspect and Action read and change its unit files offline.
	if wanted && !m.Lingering() {
		if e.job.Dryrun {
			managerChange, notice = "would_change", "would enable lingering for account "+m.Account+", so its services start at boot and keep running after logout"
		} else {
			if r := e.runtime().EnableLinger(ctx, m); r.Status != "succeeded" {
				return r
			}
			managerChange, notice = "changed", m.LingerNotice()
		}
	}
	observed, err := m.Inspect(ctx, sv.Name)
	if err != nil {
		return protocol.Failed(err)
	}
	name := observed.Unit
	key := m.Key(name)
	if observed.NeedDaemonReload && e.job.Dryrun {
		managerChange = "would_change"
	}
	if observed.NeedDaemonReload && !e.job.Dryrun {
		r := e.runtime().ReloadManagerOf(ctx, m)
		if r.Status != "succeeded" {
			return r
		}
		managerChange = "changed"
		observed, err = m.Inspect(ctx, name)
		if err != nil {
			return protocol.Failed(err)
		}
	}
	state := sv.State
	// masked: false unmasks first: nothing else can be read from or done to a
	// masked unit.
	if sv.Masked != nil && !mask && observed.Masked() {
		if e.job.Dryrun {
			return protocol.Result{Status: "succeeded", Change: "would_change", Reason: "would unmask the systemd unit before checking the unit state"}
		}
		if err := e.runtime().Mutation("service " + name); err != nil {
			return protocol.Failed(err)
		}
		action := "unmask"
		if observed.Enabled == "masked-runtime" {
			action = "unmask-runtime"
		}
		if r := m.Action(ctx, action, name); r.Status != "succeeded" {
			r.Change = "unknown"
			return r
		}
		managerChange = "changed"
		if observed, err = m.Inspect(ctx, name); err != nil {
			return protocol.Failed(err)
		}
	}
	// A removal playbook can be rerun after its unit has gone. Stopping,
	// disabling, or masking that unit is still meaningful, and do_not_start_or_stop
	// asks nothing of it; starting or enabling needs a unit.
	absent := observed.Load == "not-found"
	enable := sv.StartAtBoot != nil && *sv.StartAtBoot
	var plan *servicePlan
	if e.active != nil {
		plan = e.active.services[key]
	}
	if absent && state == "do_not_start_or_stop" && !enable && !mask {
		if !running(observed) {
			if err := e.oweNothing(plan, key); err != nil {
				return protocol.Failed(fmt.Errorf("remove the record of unit %s, which does not exist: %w", name, err))
			}
		}
		return protocol.Success("unchanged")
	}
	// A masked unit can still be stopped, and a task that neither starts nor
	// enables it leaves the mask alone.
	usable := observed.Load == "loaded" || (absent && state == "stopped" && !enable) || (mask && (absent || observed.Load == "masked")) ||
		(observed.Load == "masked" && state != "running" && !enable)
	if !usable {
		if e.job.Dryrun && e.active != nil && e.active.previewChanged {
			return protocol.Result{Status: "skipped", Change: "unknown", Reason: "service state can be checked after the node task block's proposed changes are applied"}
		}
		// An earlier node task, such as a package task, may install the unit.
		if e.job.Dryrun && absent {
			return protocol.Result{Status: "not_checked", Change: "unknown", Reason: fmt.Sprintf("unit %s is not installed yet; an earlier node task may install it", name)}
		}
		if observed.Load == "masked" {
			return protocol.Failed(fmt.Errorf("unit %s is masked, so it cannot be started or enabled; write masked: false on this task to unmask it", name))
		}
		return protocol.Failed(fmt.Errorf("service %s is not loaded (%s)", name, observed.Load))
	}
	// systemd makes a generated unit anew at each reload, from another file,
	// and refuses to enable or disable it. A unit from an init script is
	// not reported as generated here: systemctl enables and disables it.
	if sv.StartAtBoot != nil && observed.Enabled == "generated" {
		return protocol.ConfigurationError(modules.EnabledOnGeneratedUnit(name, observed.GeneratedFrom))
	}
	if state == "running" && observed.RunsOnceAndEnds {
		return protocol.Failed(fmt.Errorf("unit %s runs once and then ends (Type=oneshot without RemainAfterExit=yes), so it can never be found started; run its command with a shell task or let a timer start it, or write state: do_not_start_or_stop to manage only whether it is enabled", name))
	}
	var current map[string]string
	var record *serviceRecord
	if plan != nil {
		record = plan.record
		var preview inputPreview
		if e.job.Dryrun {
			preview = e.active.inputPreview
		}
		current, err = e.inputHashes(ctx, plan, preview)
		if err != nil {
			return protocol.Failed(err)
		}

	}
	// restart_now restarts the unit once: while its marker file is missing.
	// With the file there, the task keeps the unit running as any other.
	restartNow := false
	if sv.RestartNow {
		done, err := modules.RunOnlyOnceMarkerFileExists(sv.RunOnlyOnceMarkerFile)
		if err != nil {
			return protocol.Failed(fmt.Errorf("systemd_service.run_only_once_marker_file: %w", err))
		}
		restartNow = !done
	}
	var actions []string
	if sv.StartAtBoot != nil {
		switch {
		case *sv.StartAtBoot && observed.Enabled != "enabled":
			actions = append(actions, "enable")
		case !*sv.StartAtBoot && observed.Enabled == "enabled":
			actions = append(actions, "disable")
		case !*sv.StartAtBoot && observed.Enabled == "enabled-runtime":
			actions = append(actions, "disable-runtime")
		}
	}
	loadAction, reason := "", ""
	if state == "stopped" && !stopped(observed) {
		actions = append(actions, "stop")
	}
	// do_not_start_or_stop refreshes a running unit as running does and never starts a
	// stopped one. A unit this task masks is not refreshed.
	if state == "running" || (state == "do_not_start_or_stop" && !mask) {
		if restartNow {
			// systemctl restart starts a unit that does not run.
			loadAction = "restart"
			reason = restartNowReason(e.job.Dryrun, running(observed), name, sv.RunOnlyOnceMarkerFile)
		} else if !running(observed) {
			if state == "running" {
				loadAction = "start"
				reason = "service is not running"
			} else if err := e.oweNothing(plan, key); err != nil {
				return protocol.Failed(fmt.Errorf("remove the record of stopped unit %s: %w", name, err))
			}
		} else if plan != nil {
			// A module that changed nothing reported what the unit has loaded.
			if record != nil && e.active.takeFingerprintBaselines(record.Inputs, plan.unreported, current) && !e.job.Dryrun {
				if err := e.saveServiceRecord(record, "baseline"); err != nil {
					return protocol.Failed(fmt.Errorf("save initial service state and tracked inputs: %w", err))
				}
			}
			loadAction, reason = refreshAction(sv, plan, observed, current)
			// The comparison before the block's first write did not see this
			// restart or reload coming; its checks must still pass first.
			if loadAction != "" {
				if stop := e.requireChecks(ctx); stop != nil {
					return *stop
				}
			}
		}
		// A unit that systemd still starts, or starts again after a start
		// that failed, is not started yet. systemctl start waits for the
		// start in progress and gives its result.
		if state == "running" && loadAction == "" && observed.Active == "activating" {
			loadAction, reason = "start", "service is still starting"
		}
	}
	if loadAction != "" {
		actions = append(actions, loadAction)
	}
	if mask && observed.Enabled != "masked" {
		actions = append(actions, "mask")
		if reason == "" {
			reason = "unit is not masked"
		}
	}
	if len(actions) == 0 {
		if state == "stopped" {
			if err := e.oweNothing(plan, key); err != nil {
				return protocol.Failed(fmt.Errorf("remove the record of stopped unit %s: %w", name, err))
			}
		}
		return protocol.Success("unchanged")
	}
	result = protocol.Success("changed")
	result.Reason = reason
	if e.job.Dryrun {
		result.Change = "would_change"
		return result
	}
	if err := e.runtime().Mutation("service " + name); err != nil {
		return protocol.Failed(err)
	}
	asked := modules.JournalNow()
	for _, action := range actions {
		if plan != nil && action == loadAction {
			// A start or restart loads everything on disk, so a hash that
			// another task block recorded earlier does not say what it
			// loaded: the record keeps only this block's inputs, and the
			// other block takes a baseline for its own when it next begins.
			// A module's fingerprint stays, because it has no baseline:
			// only the module's own run reports it. A reload keeps every
			// hash of the other blocks: it does not acknowledge their units
			// or packages.
			inputs := map[string]string{}
			if record != nil {
				for k, v := range record.Inputs {
					if action == "reload" || strings.HasPrefix(k, "module:") {
						inputs[k] = v
					}
				}
			}
			maps.Copy(inputs, current)
			record = &serviceRecord{Version: 1, Unit: key, Instance: observed.Instance, Inputs: inputs, Action: action, Job: e.job.ID}
			if err := e.saveServiceRecord(record, "unconfirmed"); err != nil {
				return protocol.Failed(fmt.Errorf("record the service action before running the action: %w", err))
			}
			plan.record = record
		}
		r := m.Action(ctx, action, name)
		if r.Status != "succeeded" {
			r.Change = "unknown"
			if action == loadAction {
				r.Reason += m.JournalAfterFailedAction(ctx, action, name, asked)
			}
			return r
		}
	}
	after, err := m.Inspect(ctx, name)
	if err != nil {
		r := protocol.Failed(err)
		r.Change = "changed"
		return r
	}
	if loadAction != "" && state == "do_not_start_or_stop" && !started(after) {
		r := protocol.Failed(fmt.Errorf("service %s is not running after its %s (ActiveState=%s)%s", name, loadAction, after.Active, m.JournalAfterFailedAction(ctx, loadAction, name, asked)))
		r.Change = "changed"
		return r
	}
	if mask && after.Enabled != "masked" {
		r := protocol.Failed(fmt.Errorf("unit %s is not masked after systemctl mask (UnitFileState=%s)", name, after.Enabled))
		r.Change = "changed"
		return r
	}
	if (state == "running" && !started(after)) || (state == "stopped" && !stopped(after)) {
		r := protocol.Failed(fmt.Errorf("service %s did not reach state %s (ActiveState=%s)", name, state, after.Active))
		if state == "running" && loadAction != "" {
			r.Reason += m.JournalAfterFailedAction(ctx, loadAction, name, asked)
		}
		r.Change = "changed"
		return r
	}
	// A stopped unit loads the files on disk when it next starts, even when
	// something outside MMX starts it.
	if state == "stopped" {
		if err := e.oweNothing(plan, key); err != nil {
			r := protocol.Failed(fmt.Errorf("remove the record of stopped unit %s: %w", name, err))
			r.Change = "changed"
			return r
		}
	}
	if sv.StartAtBoot != nil && ((*sv.StartAtBoot && after.Enabled != "enabled") || (!*sv.StartAtBoot && (after.Enabled == "enabled" || after.Enabled == "enabled-runtime"))) {
		r := protocol.Failed(fmt.Errorf("service %s did not reach start_at_boot=%t (UnitFileState=%s)", name, *sv.StartAtBoot, after.Enabled))
		r.Change = "changed"
		return r
	}
	// The marker file is written only after the unit runs again, so a
	// restart that failed is tried again on the next run.
	if restartNow {
		if err := e.runtime().WriteRunOnlyOnceMarkerFile(ctx, sv.RunOnlyOnceMarkerFile, "The restart of "+name, "restarts the unit again"); err != nil {
			r := protocol.Failed(fmt.Errorf("unit %s was restarted, but its marker file %s was not written, so the next run would restart it again: %w", name, sv.RunOnlyOnceMarkerFile, err))
			r.Change = "changed"
			return r
		}
	}
	if plan != nil && loadAction != "" {
		record.Instance = after.Instance //nolint:nilaway // A nonempty loadAction with a plan means the action loop allocated record.
		record.Confirmed = true
		record.Action = ""
		if err := e.saveServiceRecord(record, "confirmed"); err != nil {
			r := protocol.Failed(fmt.Errorf("save the service record after activation: %w", err))
			r.Change = "changed"
			return r
		}
		plan.record = record
	}
	result.Reason = reasonAfterTheRun(result.Reason, loadAction)
	return result
}

// reasonAfterTheRun turns a reason that describes the unit as a dryrun finds
// it, such as "service is not running", into what a run found and did:
// "service was not running; started". A reason that names a change, such
// as a tracked input that changed, already reads true after the run.
func reasonAfterTheRun(reason, loadAction string) string {
	before, ok := map[string]string{
		"service is not running":    "service was not running",
		"service is still starting": "service was still starting",
		"unit is not masked":        "unit was not masked",
		noConfirmedRecordOfInputs:   "MMX had no confirmed record of the inputs that the running service loaded",
	}[reason]
	if !ok {
		return reason
	}
	done, ok := map[string]string{"start": "started", "restart": "restarted", "reload": "reloaded", "": "masked"}[loadAction]
	if !ok {
		return reason
	}
	return before + "; " + done
}

// restartNowReason is the reason of a task whose restart_now restarts the
// unit: the restart, and the marker file that keeps it to one.
func restartNowReason(dryrun, wasRunning bool, unit, marker string) string {
	switch {
	case dryrun && wasRunning:
		return "would restart " + unit + " once: the marker file " + marker + " does not exist"
	case dryrun:
		return "would restart " + unit + ", which is not running, so the restart starts it: the marker file " + marker + " does not exist"
	case wasRunning:
		return "restarted " + unit + " once and wrote the marker file " + marker
	}
	return "started " + unit + ", which was not running, with its one restart, and wrote the marker file " + marker
}

// oweNothing removes the record of a unit that a stopped task leaves stopped,
// or that do_not_start_or_stop finds not running, stopped or not there at all. Nothing is owed to such a unit: whenever it
// starts, it loads the files on disk then.
func (e *Engine) oweNothing(plan *servicePlan, key string) error {
	if plan == nil || e.job.Dryrun {
		return nil
	}
	if err := os.Remove(e.servicePath(key)); err != nil && !os.IsNotExist(err) {
		return err
	}
	plan.record = nil
	return nil
}

// refreshes reports whether a service task refreshes a unit that is running:
// running and do_not_start_or_stop do, except on a unit the task masks.
func refreshes(sv modules.Service) bool {
	return sv.State == "running" || (sv.State == "do_not_start_or_stop" && (sv.Masked == nil || !*sv.Masked))
}

// noConfirmedRecordOfInputs is refreshAction's reason when the service
// record does not say what the running service loaded.
const noConfirmedRecordOfInputs = "MMX has no confirmed record of the inputs that the running service loaded"

// refreshAction decides what a running unit needs so that it has loaded the
// input hashes in current: "restart", "reload", or "" for nothing, with the
// reason. A changed unit definition, package, or installation (what a
// language or tool installer installed, or an image ID) restarts; changed
// configuration or a new checked-out commit uses the task's refresh.
func refreshAction(sv modules.Service, plan *servicePlan, observed modules.ServiceState, current map[string]string) (action, reason string) {
	record := plan.record
	// Without a record, compare with the inputs before the task block.
	base := plan.before
	if record != nil {
		base = record.Inputs
	}
	switch {
	case record != nil && !record.Confirmed && (record.Action == "restart" || record.Action == "start"):
		return "restart", "previous service start was not confirmed"
	case changedInputs("unit:", base, current):
		return "restart", "service definition changed"
	case changedInputs("package:", base, current) || changedInputs("installed:", base, current):
		return "restart", "installed software version changed"
	// A process started during the task block loaded the recorded inputs
	// only if they were already on disk when the task block began.
	case record == nil || !record.Confirmed || changedInputs("config:", base, current) || changedInputs("git:", base, current) || changedInputs("module:", base, current) || (record.Instance != observed.Instance && changedInputs("", base, plan.before)):
		action = sv.Refresh
		if action == "" {
			action = "restart"
		}
		reason = noConfirmedRecordOfInputs
		if changed := changedInputNames(base, current, "config", "git", "module"); changed != "" {
			reason = changed + " changed after the service last started or reloaded"
		}
		if record != nil && !record.Confirmed && record.Action != "" {
			reason = "previous service refresh was not confirmed"
		}
	}
	return action, reason
}
