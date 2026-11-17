package controller

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"

	"mmx/internal/protocol"
)

// ReasonServerIsGone is the reason that mmx mark-node-work-failed gives the
// node jobs it records as failed.
const ReasonServerIsGone = "the team says this server is gone"

// ErrNodeAnswered refuses to mark a node's work failed while MMX can reach
// the node: the node itself can say how its work ended.
var ErrNodeAnswered = errors.New("MMX reached the node")

// ErrNodeStateUnknown means MMX could not tell whether the node is gone,
// such as when its login was refused.
var ErrNodeStateUnknown = errors.New("MMX could not tell whether the node is gone")

// RunsWithUnconfirmedWorkOn lists the saved runs, newest first, that have a
// node job on the node whose outcome is unconfirmed.
func (c Controller) RunsWithUnconfirmedWorkOn(node string) ([]*Run, error) {
	lease, err := c.lockHistory(false)
	if err != nil {
		return nil, err
	}
	defer unlockRun(lease)
	files, err := filepath.Glob(filepath.Join(c.State, "runs", "*", "run.json"))
	if err != nil {
		return nil, err
	}
	sort.Sort(sort.Reverse(sort.StringSlice(files)))
	var runs []*Run
	for _, f := range files {
		r, err := c.readRunWithProgress(f)
		if err != nil {
			return nil, err
		}
		if len(unconfirmedWorkOn(r, node)) > 0 {
			runs = append(runs, r)
		}
	}
	return runs, nil
}

// unconfirmedWorkOn lists the indexes of the run's admitted node jobs on the
// node whose outcome is unconfirmed.
func unconfirmedWorkOn(r *Run, node string) []int {
	var found []int
	for i, en := range r.Entries {
		if en.Node == node && en.JobID != "" && protocol.Uncertain(en.Response.Status) {
			found = append(found, i)
		}
	}
	return found
}

// MarkNodeWorkFailed records each node job of the run on the node whose
// outcome is unconfirmed as failed, with ReasonServerIsGone, and returns how
// many it recorded. It first tries to reach the node, as mmx status does: a
// node that answers is refused (ErrNodeAnswered), since it can say how its
// work ended; an address that now reaches another machine records the work
// as replaced, as mmx status does; and a node that MMX cannot reach at all,
// or cannot even try to reach because OpenSSH cannot read the run's SSH
// configuration, has its work recorded as gone. Another failure, such as a
// refused login, leaves the work as it was (ErrNodeStateUnknown). A run that its own
// mmx process still holds is refused too: that process may yet hear from
// the node.
func (c Controller) MarkNodeWorkFailed(ctx context.Context, r *Run, node string) (int, error) {
	c.bindSecrets(r)
	lease, err := c.lockHistory(false)
	if err != nil {
		return 0, err
	}
	defer unlockRun(lease)
	lock, err := c.lockRun(r.ID)
	if err != nil {
		return 0, err
	}
	if lock == nil {
		return 0, fmt.Errorf("run %s is still running in an mmx process; end that process first, with mmx stop %s or Ctrl-C", ShortID(r.ID), ShortID(r.ID))
	}
	defer unlockRun(lock)
	if err := c.loadProgress(r); err != nil {
		return 0, err
	}
	work := unconfirmedWorkOn(r, node)
	if len(work) == 0 {
		return 0, nil
	}
	first := r.Entries[work[0]]
	conn, err := c.connect(ctx, r, node, first.RunnerHash, machine{first.NodeID, first.Architecture})
	var replaced *replacedError
	var configuration *sshConfigurationError
	switch {
	case err == nil:
		conn.Close()
		return 0, fmt.Errorf("%w %s, so its work is not lost; mmx status %s shows how the work ended", ErrNodeAnswered, node, ShortID(r.ID))
	case errors.As(err, &replaced):
		for _, i := range work {
			r.Entries[i] = onReplacedMachine(r.Entries[i], replaced)
		}
	case transportFailure(err), errors.As(err, &configuration):
		// A run whose SSH configuration OpenSSH cannot read, such as an
		// ssh_config file that is gone with a temporary project, cannot
		// reach the node either, now or later: no command could report
		// that work, so the team's word decides, as for a node MMX tried.
		how := "MMX could not reach it: "
		if configuration != nil {
			how = "MMX could not try to reach it: "
		}
		for _, i := range work {
			en := r.Entries[i]
			en.PreviousNodeJobAttempts = append(en.PreviousNodeJobAttempts, Attempt{JobID: en.JobID, Response: en.Response})
			en.JobID, en.Response = "", protocol.Response{Status: "failed", Error: ReasonServerIsGone + "; " + how + err.Error()}
			r.Entries[i] = en
		}
	default:
		return 0, fmt.Errorf("%w %s: %v", ErrNodeStateUnknown, node, err)
	}
	for _, i := range work {
		if err := c.saveEntry(r.ID, r.Entries[i]); err != nil {
			return 0, err
		}
	}
	r.Status = runStatus(r, false)
	return len(work), c.saveStatus(r)
}
