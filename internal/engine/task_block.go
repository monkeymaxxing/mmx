package engine

import (
	"context"
	"fmt"

	"mmx/internal/config"
	"mmx/internal/value"
)

// An task block groups ordinary ordered work. Only service input records survive
// attempts; custom commands and cleanup have no separate recovery ledger.
func (e *Engine) taskBlock(ctx context.Context, t config.Task, s value.Scope, name string) error {
	state, planned, err := e.prepareServices(ctx, t, s, name)
	if err != nil {
		return err
	}
	previous := e.active
	state.previewPathsBefore = len(e.previewPaths)
	e.active = state
	defer func() { e.active = previous }()
	// The checks run after the baselines are saved and before the first
	// write, and only when the block would restart or reload something.
	var normalErr error
	if checks := planned.TaskBlock.ChecksBeforeRestartOrReload; len(checks) > 0 {
		state.checks = &blockChecks{tasks: checks, scope: s, prefix: name + "/" + config.ChecksPhase + "/"}
		var needed bool
		if needed, normalErr = e.wouldRefresh(ctx, state, s); normalErr == nil && needed {
			normalErr = e.runChecks(ctx, state.checks)
		}
	}
	if normalErr == nil {
		normalErr = e.nodeTasks(ctx, planned.TaskBlock.Changes, s, name+"/changes/")
	}
	if normalErr == nil {
		e.checking = true
		normalErr = e.nodeTasks(ctx, planned.TaskBlock.Validate, s, name+"/validate/")
		e.checking = false
	}
	if normalErr == nil {
		normalErr = e.nodeTasks(ctx, planned.TaskBlock.Activate, s, name+"/activate/")
	}
	if normalErr == nil {
		normalErr = e.clearMarks(state)
	}
	if normalErr == nil && state.checks != nil && !state.checks.ran {
		normalErr = e.skipChecks(state.checks)
	}
	if normalErr == nil {
		normalErr = e.nodeTasks(ctx, planned.TaskBlock.Verify, s, name+"/verify/")
	}
	if !e.job.Dryrun && normalErr != nil && len(planned.TaskBlock.OnFailure) > 0 {
		if err := e.nodeTasks(ctx, planned.TaskBlock.OnFailure, s, name+"/on_failure/"); err != nil {
			normalErr = fmt.Errorf("%w; on_failure: %v", normalErr, err)
		}
	}
	if err := e.nodeTasks(ctx, planned.TaskBlock.Cleanup, s, name+"/cleanup/"); err != nil {
		if normalErr != nil {
			return fmt.Errorf("%w; cleanup: %v", normalErr, err)
		}
		return err
	}
	return normalErr
}
