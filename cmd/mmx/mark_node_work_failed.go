package main

import (
	"context"
	"errors"
	"fmt"
	"os"

	"mmx/internal/controller"
)

// markNodeWorkFailed records, in each run, the node's unconfirmed node jobs
// as failed because the team says the server is gone
// (controller.MarkNodeWorkFailed), and prints what it recorded and, for each
// run it did not record, why. Every run is tried, so one run that cannot be
// recorded does not keep the others unconfirmed. The exit code is the most
// serious of the runs' outcomes: 4 for an error such as a run that its mmx
// process still holds, 3 when MMX could not tell whether the node is gone,
// 2 when MMX reached the node and refused, and 0 when every run's work was
// recorded or none was unconfirmed.
func markNodeWorkFailed(ctx context.Context, c controller.Controller, runs []*controller.Run, node string, asJSON bool) (int, error) {
	type recorded struct {
		ID                       string `json:"id"`
		ShortID                  string `json:"short_id"`
		NodeJobsRecordedAsFailed int    `json:"node_jobs_recorded_as_failed"`
		Status                   string `json:"status"`
	}
	type notRecorded struct {
		ID       string `json:"id"`
		ShortID  string `json:"short_id"`
		Reason   string `json:"reason"`
		ExitCode int    `json:"exit_code"`
	}
	view := struct {
		SchemaVersion   int           `json:"schema_version"`
		Node            string        `json:"node"`
		Reason          string        `json:"reason"`
		Runs            []recorded    `json:"runs"`
		RunsNotRecorded []notRecorded `json:"runs_not_recorded"`
	}{1, node, controller.ReasonServerIsGone, []recorded{}, []notRecorded{}}
	code := 0
	for _, r := range runs {
		count, err := c.MarkNodeWorkFailed(ctx, r, node)
		if err != nil {
			runCode := 4
			switch {
			case errors.Is(err, controller.ErrNodeAnswered):
				runCode = 2
			case errors.Is(err, controller.ErrNodeStateUnknown):
				runCode = 3
			}
			view.RunsNotRecorded = append(view.RunsNotRecorded, notRecorded{r.ID, controller.ShortID(r.ID), err.Error(), runCode})
			if runCode > code {
				code = runCode
			}
			if ctx.Err() != nil {
				break
			}
			continue
		}
		if count > 0 {
			view.Runs = append(view.Runs, recorded{r.ID, controller.ShortID(r.ID), count, r.Status})
		}
	}
	if asJSON {
		if _, err := encode(view); err != nil {
			return 4, err
		}
		return code, nil
	}
	if len(view.Runs)+len(view.RunsNotRecorded) == 0 {
		fmt.Printf("No saved run has unconfirmed work on node %s.\n", node)
	}
	for _, run := range view.Runs {
		fmt.Printf("Run %s: recorded %d node jobs on %s as failed: %s. The run is %s now.\n", run.ShortID, run.NodeJobsRecordedAsFailed, node, controller.ReasonServerIsGone, run.Status)
	}
	for _, run := range view.RunsNotRecorded {
		fmt.Fprintf(os.Stderr, "Run %s: recorded nothing: %s\n", run.ShortID, escapeControlCharacters(run.Reason))
	}
	return code, nil
}
