package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"mmx/internal/config"
	"mmx/internal/controller"
	"mmx/internal/protocol"
	"mmx/internal/value"
)

// Count servers, not node/fleet stage entries, by their highest-ranked status. A
// server is finished only when all of its recorded fleet stages completed; failed and
// prerequisite-blocked servers are counted apart. Work a stop or stopping
// policy kept from starting counts as not started.
func summary(r *controller.Run) string {
	byNode := map[string][]controller.Entry{}
	for _, en := range r.Entries {
		byNode[en.Node] = append(byNode[en.Node], en)
	}
	counts := map[string]int{}
	for _, entries := range byNode {
		worst, begun := "succeeded", false
		for _, en := range entries {
			status := en.Response.Status
			if en.NodeJobAttempt > 0 || en.JobID != "" || len(en.Response.Results) > 0 || protocol.Completed(status) {
				begun = true
			} else if !protocol.Failure(status) && !protocol.Uncertain(status) {
				status = "pending"
			}
			if protocol.Rank(status) > protocol.Rank(worst) {
				worst = status
			}
		}
		state := "waiting"
		switch {
		case protocol.Completed(worst):
			state = "finished"
		case protocol.Unconfirmed(worst):
			state = "unconfirmed"
		case protocol.Uncertain(worst):
			state = "running"
		case protocol.Failure(worst):
			state = worst // failed or blocked
		case protocol.Terminal(worst) || begun && (protocol.Terminal(r.Status) || r.StopRequested):
			state = "stopped partway through"
		case !begun:
			state = "not started"
		}
		counts[state]++
	}
	var parts []string
	for _, state := range []string{"finished", "running", "failed", "blocked", "stopped partway through", "waiting", "not started", "unconfirmed"} {
		if counts[state] > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", counts[state], state))
		}
	}
	awaiting := r.FleetStagesAwaitingNodeSelection
	for _, sel := range r.Selections {
		awaiting = awaiting || !sel.Resolved
	}
	if awaiting {
		parts = append(parts, "more fleet stages awaiting node or loop item selection")
	}
	return strings.Join(parts, " · ")
}

// selectionLine names what a run targets before it contacts any server.
func selectionLine(r *controller.Run) string {
	entry := "Playbook: " + r.Project.PlaybookName
	if r.EntryPoint == controller.RunCommandEntryPoint {
		entry = "Entry point: run-command"
	}
	line := fmt.Sprintf("Project: %s   %s\nInventory: %s (%s)", r.Project.Project, entry, r.InventoryName, nodeCount(len(r.Inventory.Nodes)))
	if len(r.Limit) > 0 {
		line += "   Limit: " + strings.Join(r.Limit, ",")
	}
	if len(r.Project.OnlyFleetStages) > 0 {
		line += "   Only fleet stages: " + strings.Join(r.Project.OnlyFleetStages, ",")
	}
	count, bound := selectedNodes(r)
	if bound {
		return line + "   Selected: up to " + nodeCount(count)
	}
	return line + "   Selected: " + nodeCount(count)
}

// selectedNodes counts the nodes a run selected. While selections still
// resolve during the run, the nodes its limit keeps, or the whole inventory,
// bound the count instead.
func selectedNodes(r *controller.Run) (count int, bound bool) {
	if r.FleetStagesAwaitingNodeSelection {
		// A for_each_node iteration the limit keeps also works on helper
		// nodes outside the limit.
		helpers := false
		// The traversal callback always returns nil and traversal has no other error source.
		_ = config.WalkFleetStages(r.Project.FleetStages, func(_ string, s config.FleetStage) error {
			helpers = helpers || s.ForEachNode != nil
			return nil
		})
		if limit, err := config.NewLimit(r.Inventory, r.Limit); err == nil && limit != nil && !helpers {
			return len(limit), true
		}
		return len(r.Inventory.Nodes), true
	}
	nodes := map[string]bool{}
	for _, en := range r.Entries {
		nodes[en.Node] = true
	}
	return len(nodes), false
}

func activeDetails(w io.Writer, r *controller.Run) {
	for _, en := range r.Entries {
		var activity string
		switch {
		case en.Response.Status == "running" && en.Transfer != nil:
			p := en.Transfer
			activity = fmt.Sprintf("transferring files — %.1f / %.1f MiB (%d/%d files)", float64(p.Bytes)/(1024*1024), float64(p.Total)/(1024*1024), p.Completed, p.Files)
		case en.Response.Status == "running":
			activity = "running"
			if en.JobID == "" {
				activity = "connecting"
				if r.StopRequested {
					activity = "canceling the connection; no node job accepted"
				}
			} else if r.StopRequested {
				activity = "stop unconfirmed"
				if en.Response.StopRequested {
					activity = "finishing current work; stop acknowledged"
				}
			}
			if en.Response.CurrentTask != "" {
				activity += " (" + en.Response.CurrentTask + ")"
			}
		case en.Response.Status == "rebooting":
			activity = "node is rebooting; waiting for the node to return after restart"
		case protocol.Unconfirmed(en.Response.Status):
			activity = "unconfirmed; work may still be running"
			if en.Response.Error != "" {
				activity += " — " + en.Response.Error
			}
		default:
			continue
		}
		fmt.Fprintf(w, "  %s / %s: %s\n", en.FleetStage, en.Node, activity)
	}
}

func liveProgress(w io.Writer, jsonOutput bool, display ...*presentation) func(*controller.Run) {
	// Progress shows node errors and the current task's name.
	w = terminalSafe(w)
	p := &presentation{mode: "auto"}
	if len(display) > 0 {
		p = display[0]
	}
	var last time.Time
	var previous string
	started, stopping := false, false
	return func(r *controller.Run) {
		if !started {
			started = true
			if !jsonOutput {
				p.choose(r)
				fmt.Fprintf(w, "Run %s\n%s\n", controller.ShortID(r.ID), selectionLine(r))
				p.describe(w)
			}
		}
		if r.StopRequested && !stopping {
			stopping = true
			fmt.Fprintln(w, "Stop requested. No new work will start.\nEach node will finish the current node task or node task block.\nWaiting for the nodes to stop. Press Ctrl-C again to leave the display.")
			last = time.Time{}
		}
		if jsonOutput || time.Since(last) < time.Second {
			return
		}
		var view strings.Builder
		fmt.Fprintln(&view, "Progress: "+summary(r))
		if p.mode == "nodes" {
			activeDetails(&view, r)
		}
		if view.String() != previous && time.Since(last) >= time.Second {
			fmt.Fprint(w, view.String())
			previous, last = view.String(), time.Now()
		}
	}
}

func output(r *controller.Run, jsonOutput, details bool, p *presentation, err error) (int, error) {
	p.choose(r)
	if p.diff == "all" {
		if e := p.verifyDiffs(r); e != nil {
			return 4, e
		}
	}
	if jsonOutput {
		entries := r.OrderedEntries()
		view := map[string]any{"schema_version": 1, "id": r.ID, "short_id": controller.ShortID(r.ID), "limit": r.Limit, "only_fleet_stages": r.Project.OnlyFleetStages, "created": r.Created, "finished": r.Finished, "status": r.Status, "stop_requested": r.StopRequested, "dryrun": r.Dryrun, "project": r.Project.Project, "entries": entries, "inventory": r.InventoryName, "playbook": r.Project.PlaybookName, "entry_point": r.EntryPoint, "selections": r.Selections, "fleet_stages_awaiting_node_selection": r.FleetStagesAwaitingNodeSelection, "secret_inputs_refreshed": r.SecretsRefreshed}
		if e := p.encodeRun(os.Stdout, view, r); e != nil {
			return 4, e
		}
	} else {
		// Text from nodes reaches a terminal here; see terminalSafe.
		out := terminalSafe(os.Stdout)
		if r.SecretsRefreshed {
			fmt.Fprintln(out, "Secret inputs refreshed for unfinished work.")
		}
		p.describe(out)
		if e := p.human(out, r, details); e != nil {
			return 4, e
		}
		selectionOutput(out, r, details)
		id := controller.ShortID(r.ID)
		fmt.Fprintf(out, "Run %s: %s\n%s\nSaved: %s\n", id, r.Status, summary(r), p.runDir(r))
		if p.mode == "nodes" {
			activeDetails(out, r)
		}
		if !protocol.Terminal(r.Status) {
			fmt.Fprintf(out, "Check progress: mmx status %s\nRequest stop:   mmx stop %s\n", id, id)
		} else if r.Status != "succeeded" {
			fmt.Fprintf(out, "Inspect:                mmx results %s\n", id)
			// mmx resume refuses a dryrun.
			if !r.Dryrun {
				fmt.Fprintf(out, "Resume unfinished work: mmx resume %s\n", id)
			}
			if r.EntryPoint != controller.RunCommandEntryPoint {
				fmt.Fprintf(out, "Start a new run:        mmx run %s\n", r.Project.PlaybookName)
			}
		}
		// A node that MMX could not reach may be gone for good; then mmx
		// resume cannot finish its work, and this command records it.
		unreachable := map[string]bool{}
		for _, en := range r.Entries {
			if en.Response.Status == "unknown" && en.Response.Error != "" && !unreachable[en.Node] {
				unreachable[en.Node] = true
				fmt.Fprintf(out, "If server %s is gone for good: mmx mark-node-work-failed --node %s %s\n", en.Node, en.Node, id)
			}
		}
	}
	if errors.Is(err, context.Canceled) {
		return 130, nil
	}
	if err != nil {
		return 4, err
	}
	return runExitCode(r), nil
}

// selectsNoNodes is why nodes, explain, and a run's output report a fleet
// stage or for_each_node loop as not selected when its own selection is
// empty.
const selectsNoNodes = "selection contains no nodes (skip_when_no_nodes_are_selected)"

func selectionOutput(w io.Writer, r *controller.Run, details bool) {
	for _, fleetStage := range r.Project.FleetStages {
		if !r.Project.KeepsFleetStage(fleetStage.ID) {
			fmt.Fprintf(w, "  %s: not selected — left out by --only-fleet-stages\n", fleetStage.ID)
		}
	}
	paths := make([]string, 0, len(r.Selections))
	for path := range r.Selections {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		sel := r.Selections[path]
		//nolint:nilaway // Selection paths come from entries allocated by scheduling or loading and retained throughout display.
		if sel.Response.Error != "" {
			fmt.Fprintf(w, "  %s: %s — %s\n", path, sel.Response.Status, sel.Response.Error)
		}
		//nolint:nilaway // Selection paths come from entries allocated by scheduling or loading and retained throughout display.
		if sel.LeftOutBecauseItSelectsNoNodes {
			fmt.Fprintf(w, "  %s: not selected — %s\n", path, selectsNoNodes)
		} else if sel.Resolved && !sel.Loop && len(sel.Nodes) == 0 || sel.Resolved && sel.Loop && len(sel.Items) == 0 && len(sel.LeftOut) > 0 {
			fmt.Fprintf(w, "  %s: not selected — left out by the limit\n", path)
		}
		for _, item := range sel.Items {
			response := sel.Iterations[item.Key] //nolint:nilaway // Selection paths come from entries allocated by scheduling or loading and retained throughout display.
			if response.Error != "" {
				fmt.Fprintf(w, "  %s[%s]: %s — %s\n", path, item.Key, response.Status, response.Error)
			} else if details && len(response.Outputs) > 0 {
				b, _ := json.Marshal(value.Display(response.Outputs))
				fmt.Fprintf(w, "  %s[%s] outputs: %s\n", path, item.Key, b)
			}
		}
	}
}

func fileChanges(w io.Writer, result protocol.Result, location string, diff bool) {
	if c := result.FileChange; c != nil {
		action := map[string]string{"create": "created", "update": "updated", "remove": "removed", "replace": "replaced"}[c.Action]
		if result.Change == "would_change" {
			action = "would " + c.Action
		}
		if protocol.Failure(result.Status) {
			action = "requested " + c.Action
		}
		fmt.Fprintf(w, "  %s / %s: %s %q\n", location, result.Task, action, c.Path)
		describe := func(s *protocol.FileState) string {
			if s == nil {
				return "absent"
			}
			if s.Kind == "symlink" {
				return fmt.Sprintf("link to %q", s.Target)
			}
			if s.Kind == "directory" {
				return fmt.Sprintf("directory, permissions_mode %s, uid %d, gid %d", s.Mode, s.UID, s.GID)
			}
			if s.SHA256 != "" {
				return fmt.Sprintf("%d bytes, sha256 %s, permissions_mode %s, uid %d, gid %d", s.Size, s.SHA256, s.Mode, s.UID, s.GID)
			}
			return fmt.Sprintf("%d bytes, permissions_mode %s, uid %d, gid %d", s.Size, s.Mode, s.UID, s.GID)
		}
		fmt.Fprintf(w, "    %s -> %s\n", describe(c.Before), describe(c.After))
		if c.OriginalFileCopiedTo != "" {
			verb := "copied the original file to"
			if result.Change == "would_change" {
				verb = "would copy the original file to"
			}
			fmt.Fprintf(w, "    %s %q\n", verb, c.OriginalFileCopiedTo)
		}
		if diff {
			for _, line := range strings.Split(strings.TrimSuffix(c.Diff, "\n"), "\n") {
				if c.Diff != "" {
					fmt.Fprintln(w, "    "+line)
				}
			}
			if c.DiffOmitted != "" {
				fmt.Fprintln(w, "    "+c.DiffOmitted)
			}
		}
	}
}

func directoryChanges(w io.Writer, result protocol.Result, location string, details bool) {
	d := result.DirectoryChange
	if d == nil {
		return
	}
	fmt.Fprintf(w, "  %s / %s: directory %q\n", location, result.Task, d.Path)
	fmt.Fprintf(w, "    checked %d directories and %d files; ", d.Directories, d.Files)
	if result.Change == "would_change" {
		fmt.Fprintf(w, "%d entries would change", d.Changes)
	} else {
		updates := d.Applied
		if len(result.Tries) > 1 {
			updates = 0
			for _, attempt := range result.Tries {
				if attempt.DirectoryChange != nil {
					updates += attempt.DirectoryChange.Applied
				}
			}
		}
		if d.Copy {
			fmt.Fprintf(w, "%d entries updated; %d unchanged", updates, d.Directories+d.Files-d.Changes)
		} else {
			fmt.Fprintf(w, "%d metadata updates", updates)
		}
	}
	if result.Status == "failed" {
		fmt.Fprint(w, " before failure; inspection may be incomplete")
	}
	fmt.Fprintln(w)
	if len(d.CreatedParentDirectories) > 0 {
		verb := "created"
		if result.Change == "would_change" {
			verb = "would create"
		}
		fmt.Fprintf(w, "    %s the missing parent directories %s, owner root, permissions_mode 0755\n", verb, strings.Join(d.CreatedParentDirectories, ", "))
	}
	if d.SkippedSymlinks+d.SkippedSpecial+d.SkippedMounts > 0 {
		fmt.Fprintf(w, "    skipped %d symlinks, %d special files, %d different-device mount points\n", d.SkippedSymlinks, d.SkippedSpecial, d.SkippedMounts)
	}
	if details {
		for i := range d.Examples {
			sample := result
			sample.FileChange = &d.Examples[i]
			// Each example is a requested change, not proof of its completion.
			if d.Applied < d.Changes && result.Change != "would_change" {
				sample.Status = "failed"
			}
			fileChanges(w, sample, location, d.Copy)
		}
		if omitted := d.Changes - len(d.Examples); omitted > 0 {
			fmt.Fprintf(w, "    %d further entries omitted from examples\n", omitted)
		}
	}
}
