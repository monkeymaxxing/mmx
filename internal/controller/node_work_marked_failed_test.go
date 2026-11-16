package controller

import (
	"context"
	"strings"
	"testing"

	"mmx/internal/config"
	"mmx/internal/protocol"
)

// mmx mark-node-work-failed records the unconfirmed work of a node that MMX
// cannot reach as failed, because the team says the server is gone, and
// keeps the job as an earlier attempt. Another node's unconfirmed work stays
// as it was. Once no work is unconfirmed, the run is finished, so history
// retention can end it. A run that its own mmx process still holds is
// refused.
func TestMarkNodeWorkFailedRecordsTheWorkOfANodeMMXCannotReach(t *testing.T) {
	c, r := fleet(t, config.FleetStage{ID: "setup", Nodes: []any{"a", "b"}})
	for i := range r.Entries {
		r.Entries[i].JobID = "job-on-" + r.Entries[i].Node
		r.Entries[i].Response = protocol.Response{Status: "unknown", Error: "ssh: connect to host port 22: Connection timed out"}
		if err := c.saveEntry(r.ID, r.Entries[i]); err != nil {
			t.Fatal(err)
		}
	}
	r.Status = runStatus(r, false)
	if err := c.saveStatus(r); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	lock, err := c.lockRun(r.ID)
	if err != nil || lock == nil {
		t.Fatal(err)
	}
	if _, err := c.MarkNodeWorkFailed(ctx, r, "a"); err == nil || !strings.Contains(err.Error(), "still running in an mmx process") {
		t.Fatal(err)
	}
	unlockRun(lock)

	if runs, err := c.RunsWithUnconfirmedWorkOn("a"); err != nil || len(runs) != 1 || runs[0].ID != r.ID {
		t.Fatal(runs, err)
	}
	if count, err := c.MarkNodeWorkFailed(ctx, r, "a"); err != nil || count != 1 {
		t.Fatal(count, err)
	}
	saved, err := c.Load(r.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	byNode := map[string]Entry{}
	for _, en := range saved.Entries {
		byNode[en.Node] = en
	}
	if a := byNode["a"]; a.Response.Status != "failed" || !strings.HasPrefix(a.Response.Error, ReasonServerIsGone+"; MMX could not reach it: ") ||
		a.JobID != "" || len(a.PreviousNodeJobAttempts) != 1 || a.PreviousNodeJobAttempts[0].JobID != "job-on-a" {
		t.Fatalf("%+v", a)
	}
	if b := byNode["b"]; b.Response.Status != "unknown" || b.JobID != "job-on-b" {
		t.Fatalf("the other node's work changed: %+v", b)
	}
	if saved.Finished != nil {
		t.Fatal("the run is finished while node b's work is unconfirmed")
	}
	if runs, err := c.RunsWithUnconfirmedWorkOn("a"); err != nil || len(runs) != 0 {
		t.Fatal(runs, err)
	}
	if count, err := c.MarkNodeWorkFailed(ctx, saved, "a"); err != nil || count != 0 {
		t.Fatal("a second call recorded something", count, err)
	}

	if count, err := c.MarkNodeWorkFailed(ctx, saved, "b"); err != nil || count != 1 {
		t.Fatal(count, err)
	}
	if saved, err = c.Load(r.ID, false); err != nil || saved.Status != "failed" || saved.Finished == nil {
		t.Fatalf("the run is %q, finished %v: %v", saved.Status, saved.Finished, err)
	}
}
