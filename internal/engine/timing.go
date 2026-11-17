package engine

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"mmx/internal/config"
	"mmx/internal/protocol"
	"mmx/internal/value"
	"time"
)

func (e *Engine) timed(ctx context.Context, t config.Task, s value.Scope) protocol.Result {
	if t.TimeLimit != nil {
		duration, err := time.ParseDuration(*t.TimeLimit)
		if err != nil {
			return protocol.Failed(err)
		}
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, duration)
		defer cancel()
	}
	// Keep the first and latest tries and count the others, so polling
	// cannot grow the record. Predicates read each try's own result.
	var tries []protocol.Result
	omitted := 0
	change := "unchanged"
	finish := func(r protocol.Result) protocol.Result {
		r.Change = change
		if t.Retry != nil && !t.Retry.Disabled {
			r.Tries, r.TriesOmitted = tries, omitted
		}
		return r
	}
	// A module that is not core reports its fingerprint in each try that
	// succeeds. Only the last try's counts, as in a run whose first try is
	// the last: each further try starts from the fingerprints that the node
	// task block had before this task.
	var fingerprintsBeforeTheTask map[string]string
	if e.active != nil {
		fingerprintsBeforeTheTask = maps.Clone(e.active.fingerprints)
	}
	for n := 1; ; n++ {
		if n > 1 && e.active != nil {
			e.active.fingerprints = maps.Clone(fingerprintsBeforeTheTask)
		}
		tryContext := ctx
		cancel := func() {}
		if t.TimeLimitPerTry != nil {
			duration, err := time.ParseDuration(*t.TimeLimitPerTry)
			if err != nil {
				return protocol.Failed(err)
			}
			tryContext, cancel = context.WithTimeout(ctx, duration)
		}
		r := e.execute(tryContext, t, s)
		tryErr := tryContext.Err()
		cancel()
		limit, err := "time_limit exceeded", ctx.Err()
		if err == nil && errors.Is(tryErr, context.DeadlineExceeded) {
			limit, err = "time_limit_per_try exceeded", tryErr
		}
		if err != nil {
			// A time limit has ended, or the node job was stopped. A try
			// that failed then with a reason of its own keeps that reason
			// behind the words that say which of them happened: a module
			// that waited says what it waited for.
			what, why := limit, err.Error()
			if errors.Is(err, context.Canceled) {
				what, why = "the node job was stopped", ""
			}
			if r.Status == "failed" && r.Reason != "" {
				why = r.Reason
			}
			r.Status, r.Reason = "failed", what
			if why != "" {
				r.Reason += ": " + why
			}
		}
		tries = append(tries, r)
		if len(tries) > retainedTries {
			tries = append(tries[:1], tries[2:]...)
			omitted++
		}
		change = protocol.CombineChange(change, r.Change)
		// Skipped, blocked, and unchecked tries did not run and do not repeat.
		ran := r.Status == "succeeded" || r.Status == "failed"
		if t.Retry == nil || t.Retry.Disabled || !ran || r.NoOtherTry || r.FailureKind == "config" || r.FailureKind == "validation" || r.FailureKind == "missing_prerequisite" {
			return finish(r)
		}
		local := value.Scope(clone(s))
		local["this_node_task"] = protocol.ResultValue(r)
		if r.Status == "succeeded" {
			done, err := local.Predicate(t.Retry.Until)
			if err != nil {
				r.Status = "failed"
				r.FailureKind = "config"
				r.Reason = config.SourceError(t.Sources, "retry.until", t.Retry.Until, local, err).Error()
				return finish(r)
			}
			if done {
				return finish(r)
			}
		}
		r.Status = "failed"
		allowed, err := local.Predicate(t.Retry.RetryWhen)
		if err != nil {
			r.FailureKind = "config"
			r.Reason = config.SourceError(t.Sources, "retry.retry_when", t.Retry.RetryWhen, local, err).Error()
			return finish(r)
		}
		if !allowed {
			r.Reason = "retry_when is false; " + r.Reason
			return finish(r)
		}
		if ctx.Err() != nil {
			return finish(r)
		}
		if t.Retry.MaxTries != 0 && config.TryLimit(n) >= t.Retry.MaxTries {
			r.Reason = fmt.Sprintf("node task did not finish after %d tries, because each try failed or left retry.until false: %s", n, r.Reason)
			return finish(r)
		}
		pause, err := time.ParseDuration(t.Retry.PauseBetweenTries)
		if err != nil {
			r.Reason = err.Error()
			return finish(r)
		}
		timer := time.NewTimer(pause)
		select {
		case <-ctx.Done():
			timer.Stop()
			r.Reason = "time_limit exceeded during retry.pause_between_tries"
			return finish(r)
		case <-timer.C:
		}
	}
}
