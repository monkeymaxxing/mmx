package controller

import (
	"context"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"mmx/internal/config"
	"mmx/internal/protocol"
	"mmx/internal/value"
)

// recordLastSuccessfulRuns asks each node on which every fleet stage that
// selected it succeeded to record this run as the playbook's last successful
// run, so monitoring can see a node that has not been configured for a long
// time. A dryrun and a run-command run record nothing. Neither does a run
// that left a selection unresolved, such as after a stop: a fleet stage may
// then have selected a node without the run knowing. A skipped fleet stage,
// whose when was false on the node, counts as success. A failed, stopped, or
// refused node keeps its earlier record. A record that cannot be written is
// housekeeping on the node's last entry; it changes no outcome.
func (c Controller) recordLastSuccessfulRuns(ctx context.Context, r *Run) error {
	if r.Dryrun || r.EntryPoint == RunCommandEntryPoint || r.Project.PlaybookName == "" || r.FleetStagesAwaitingNodeSelection || !selectionsResolved(r, r.Project.FleetStages, "") {
		return nil
	}
	last, unfinished := map[string]int{}, map[string]bool{}
	for i, en := range r.Entries {
		// A node job's entry names the machine to record on; an entry that a
		// fleet stage's when skipped in the controller process reached no node.
		if previous, seen := last[en.Node]; !seen || en.JobID != "" || r.Entries[previous].JobID == "" {
			last[en.Node] = i
		}
		if !protocol.Completed(en.Response.Status) {
			unfinished[en.Node] = true
		}
	}
	// A loop iteration can fail although each of its entries completed, such
	// as when the loop's own outputs fail. Every node that took part in it,
	// the for_each_node loop's node and its helper nodes, keeps its record.
	for _, sel := range r.Selections {
		for _, item := range sel.Items {
			if protocol.Completed(sel.Iterations[item.Key].Status) {
				continue
			}
			if slices.Contains(sel.Nodes, item.Key) {
				unfinished[item.Key] = true
			}
			prefix := sel.Path + "[" + value.EscapeKey(item.Key) + "]/"
			for _, en := range r.Entries {
				if strings.HasPrefix(en.FleetStage, prefix) {
					unfinished[en.Node] = true
				}
			}
		}
	}
	var nodes []string
	for node := range last {
		if !unfinished[node] {
			nodes = append(nodes, node)
		}
	}
	sort.Strings(nodes)
	finished := time.Now().UTC()
	if r.Finished != nil {
		finished = r.Finished.UTC()
	}
	request := protocol.LastSuccessfulRunRequest{Project: r.Project.Project, Playbook: r.Project.PlaybookName, Record: protocol.LastSuccessfulRun{
		RunID: r.ID, Inventory: r.InventoryName, Limit: strings.Join(r.Limit, ","), OnlyFleetStages: strings.Join(r.Project.OnlyFleetStages, ","), FinishedAt: finished.Truncate(time.Second),
	}}
	record := c.recordNode
	if record == nil {
		record = c.recordOnNode
	}
	limit := max(r.Project.MaxNodesAtOnce, 1)
	slots := make(chan struct{}, limit)
	var mu sync.Mutex
	var saveErr error
	var wg sync.WaitGroup
	for _, node := range nodes {
		i := last[node]
		en := r.Entries[i]
		wg.Add(1)
		slots <- struct{}{}
		go func() {
			defer func() { <-slots; wg.Done() }()
			err := record(ctx, r, en, request)
			if err == nil {
				return
			}
			mu.Lock()
			defer mu.Unlock()
			en.Housekeeping = append(en.Housekeeping, "recording the last successful run on the node: "+err.Error())
			r.Entries[i] = en
			if err := c.saveEntry(r.ID, en); err != nil && saveErr == nil {
				saveErr = err
			}
		}()
	}
	wg.Wait()
	return saveErr
}

// recordOnNode connects to the machine the node's last entry ran on, with the
// runner build that entry used, and asks it to write the record.
func (c Controller) recordOnNode(ctx context.Context, r *Run, en Entry, request protocol.LastSuccessfulRunRequest) error {
	conn, err := c.connect(ctx, r, r.Inventory.Nodes[en.Node], en.RunnerHash, machine{en.NodeID, en.Architecture})
	if err != nil {
		return err
	}
	defer conn.Close()
	return conn.RecordLastSuccessfulRun(ctx, request)
}

// selectionsResolved reports whether the run resolved the selection of every
// fleet stage it keeps, through every loop iteration that was not skipped, so
// its entries name every fleet stage that selected each node.
func selectionsResolved(r *Run, fleetStages []config.FleetStage, prefix string) bool {
	for _, s := range fleetStages {
		if prefix == "" && !r.Project.KeepsFleetStage(s.ID) {
			continue
		}
		sel := r.Selections[prefix+s.ID]
		if sel == nil || !sel.Resolved {
			return false
		}
		for _, item := range sel.Items {
			if sel.Iterations[item.Key].Status == "skipped" {
				continue
			}
			if !selectionsResolved(r, s.FleetStages, sel.Path+"["+value.EscapeKey(item.Key)+"]/") {
				return false
			}
		}
	}
	return true
}
