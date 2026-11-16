package main

import (
	"mmx/internal/controller"
	"mmx/internal/protocol"
)

func runExitCode(r *controller.Run) int {
	if !r.Dryrun {
		if r.Status != "succeeded" {
			return 1
		}
		return 0
	}
	f := dryrunFindings{incomplete: r.FleetStagesAwaitingNodeSelection}
	for _, e := range r.Entries {
		f.response(e.Response)
	}
	f.selections(r)
	f.outcome(r.Status, "")
	if f.failed {
		return 1
	}
	if f.incomplete || r.Status != "succeeded" {
		return 3
	}
	if f.changed {
		return 2
	}
	return 0
}

// dryrunFindings is what the statuses and changes of a dryrun add up to:
// failed work, work the dryrun could not check, and a proposed change. A
// dryrun's exit code reads them for the whole run, and the check of a
// one-time playbook reads them for each node.
type dryrunFindings struct{ changed, incomplete, failed bool }

func (f *dryrunFindings) outcome(status, change string) {
	switch {
	case protocol.Failure(status) || status == "stopped":
		f.failed = true
	case protocol.Unconfirmed(status) || !protocol.Terminal(status) || status == "not_checked":
		f.incomplete = true
	}
	if change == "unknown" {
		f.incomplete = true
	}
	if change == "would_change" || change == "changed" {
		f.changed = true
	}
}

func (f *dryrunFindings) result(v protocol.Result) {
	f.outcome(v.Status, v.Change)
	if v.CheckNewFile != nil {
		switch {
		case v.Status == "not_checked":
			// The node task is not checked: its new file's check ran against a
			// node that an earlier node task would change first, so the check's
			// failure is not the run's. The dryrun is incomplete, not failed.
			f.incomplete = true
		case v.CheckNewFile.Status == "skipped":
			f.incomplete = true
			f.result(*v.CheckNewFile)
		default:
			f.result(*v.CheckNewFile)
		}
	}
	for _, item := range v.Items {
		f.result(item)
	}
}

func (f *dryrunFindings) response(v protocol.Response) {
	f.outcome(v.Status, v.Change)
	for _, v := range v.Results {
		f.result(v)
	}
}

// selections takes in what a run's entries cannot say: a selection that did
// not resolve, and the outcome of each loop iteration.
func (f *dryrunFindings) selections(r *controller.Run) {
	for _, s := range r.Selections {
		if s.Response.Status != "" {
			f.response(s.Response)
		}
		for _, r := range s.Iterations {
			f.response(r)
		}
	}
}

const exitCodeHelp = `
Exit codes:
  0    Success, including an unchanged complete dryrun or successful results/status inspection of a failed run.
  1    Failed work: a node task, run, stopped unfinished run, run-command, invalid configuration found by validate,
       or a referenced secret key that is not defined, found by secrets usage.
  2    Dryrun: something would change.
       check-one-time-playbooks: a node still needs an outstanding one-time playbook.
       complete-one-time-playbook: refused, because a node still needs the playbook.
       mark-node-work-failed: refused, because MMX reached the node.
       remove-mmx-from-node: refused, because a node job runs or an owed restore waits on the node.
  3    Unknown or incomplete dryrun, or status/stop cannot confirm a node's current state.
       check-one-time-playbooks and complete-one-time-playbook: a node could not be inspected, or the check inspected no nodes.
       mark-node-work-failed: MMX could not tell whether the node is gone, such as after a refused login.
  4    Command error: bad arguments or flags, unknown command or module, missing playbook name,
       rejected --var, --limit, or --only-fleet-stages, configuration that cannot load (except validate),
       unavailable vault or credentials before a run, no matching saved run, a short run ID that matches
       several saved runs, resume of a run that another MMX release started, or controller storage/output failure.
       Also a command that names a completed one-time playbook.
  130  Ctrl-C, including preparation or leaving the live display.
Dryrun precedence: failure, then incomplete, then proposed changes.
`
