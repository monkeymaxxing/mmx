package controller

import (
	"context"
	"fmt"
	"slices"

	"mmx/internal/assets"
	"mmx/internal/differences"
	"mmx/internal/protocol"
)

// fetchDiffs downloads the full differences a terminal result names.
// A failed download is housekeeping; logs show that difference as unavailable.
func (c Controller) fetchDiffs(ctx context.Context, r *Run, en Entry, conn *Connection) Entry {
	if !protocol.Terminal(en.Response.Status) {
		return en
	}
	refs, err := differences.Refs(en.Response)
	for _, ref := range refs {
		if err != nil {
			break
		}
		if assets.Verify(ctx, differences.Local(c.DiffDir(r.ID), ref)) != nil {
			err = conn.downloadDiff(ctx, en.JobID, ref, c.DiffDir(r.ID))
		}
	}
	if err != nil {
		en.Housekeeping = append(en.Housekeeping, fmt.Sprintf("full differences of job %s: %v", en.JobID, err))
	}
	return en
}

// onReplacedMachine ends the entry's job on a machine the address no longer
// reaches, a definite answer that the job's machine is gone: the job becomes an
// earlier attempt and the node fleet stage fails with the replacement as its reason.
// A new mmx run sets the new machine up.
func onReplacedMachine(en Entry, replaced *replacedError) Entry {
	if en.JobID != "" {
		en.PreviousNodeJobAttempts = append(en.PreviousNodeJobAttempts, Attempt{JobID: en.JobID, Response: en.Response})
	}
	en.JobID, en.Response = "", protocol.Response{Status: "failed", Error: replaced.Error()}
	return en
}

// Housekeeping observed while polling remains visible with the final outcome.
func withHousekeeping(current, next protocol.Response) protocol.Response {
	for _, warning := range current.Housekeeping {
		if !slices.Contains(next.Housekeeping, warning) {
			next.Housekeeping = append(next.Housekeeping, warning)
		}
	}
	return next
}
