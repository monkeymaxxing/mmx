package main

import (
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"mmx/internal/controller"
	"mmx/internal/protocol"
	"mmx/internal/value"
)

type diffMode string

func (d *diffMode) String() string   { return string(*d) }
func (d *diffMode) IsBoolFlag() bool { return true }
func (d *diffMode) Set(s string) error {
	switch s {
	case "true":
		*d = "preview"
	case "false":
		*d = ""
	case "all":
		*d = "all"
	default:
		return fmt.Errorf("use --diff for previews or --diff=all for complete saved differences")
	}
	return nil
}

type presentation struct {
	mode, state string
	// described: this command has named its output view, in its live
	// progress or its report.
	described bool
	diff      diffMode
	// Artifacts not available locally, found by verifyDiffs.
	unavailable map[string]bool
}

func (p *presentation) choose(r *controller.Run) {
	if p.mode != "auto" && p.mode != "" {
		return
	}
	// Runtime node selections cannot be known at admission. Use the upper
	// bound then, and keep the chosen view for this command's lifetime.
	p.mode = "nodes"
	if count, _ := selectedNodes(r); count > 10 {
		p.mode = "summary"
	}
}

// describe names the output view, once for a command.
func (p *presentation) describe(w io.Writer) {
	if !p.described {
		fmt.Fprintln(w, "Output: "+p.mode+" (change with --output=nodes or --output=summary)")
		p.described = true
	}
}
func (p *presentation) runDir(r *controller.Run) string { return filepath.Join(p.state, "runs", r.ID) }

type outcomeGroup struct {
	fleetStage string
	nodes      []string
	result     protocol.Result
}

func outcomeGroups(r *controller.Run, grouped bool) []outcomeGroup {
	var groups []outcomeGroup
	indices := map[string]int{}
	for _, en := range r.OrderedEntries() {
		for _, result := range en.Response.Results {
			result = result.Safe()
			b, _ := json.Marshal(result)
			key := en.FleetStage + "\x00" + string(b)
			if index, ok := indices[key]; grouped && ok {
				groups[index].nodes = append(groups[index].nodes, en.Node) //nolint:nilaway // The index is stored when its group is appended and read only after a successful lookup.
			} else {
				indices[key] = len(groups)
				groups = append(groups, outcomeGroup{en.FleetStage, []string{en.Node}, result})
			}
		}
	}
	return groups
}
func nodeLabel(nodes []string, grouped bool) string {
	if !grouped {
		return nodes[0]
	}
	return fmt.Sprintf("%s [%s]", nodeCount(len(nodes)), strings.Join(nodes, ", "))
}

// Current response outcomes supersede copies in task results. Keep a task's
// secret marking even when its later restore outcome arrives on its own.
func currentOwedRestores(response protocol.Response) []protocol.OwedRestore {
	byChange := map[string]protocol.OwedRestore{}
	remember := func(restore protocol.OwedRestore) {
		restore.Secret = restore.Secret || byChange[restore.OwedRestoreID].Secret
		byChange[restore.OwedRestoreID] = restore.Safe()
	}
	var collect func([]protocol.Result)
	collect = func(results []protocol.Result) {
		for _, result := range results {
			result = result.Safe()
			collect(result.Tries)
			collect(result.Items)
			if result.OwedRestore != nil {
				remember(*result.OwedRestore)
			}
		}
	}
	collect(response.Results)
	for _, restore := range response.OwedRestores {
		remember(restore)
	}
	var restores []protocol.OwedRestore
	for _, id := range slices.Sorted(maps.Keys(byChange)) {
		restores = append(restores, byChange[id])
	}
	return restores
}

func (p *presentation) human(w io.Writer, r *controller.Run, details bool) error {
	// Text from nodes reaches a terminal here; see terminalSafe.
	w = terminalSafe(w)
	grouped := p.mode == "summary"
	// A plain summary for each server comes first; the detail lines follow.
	summarizeServers(w, r, grouped)
	// Group entry outcomes too, so a common connection failure or unstarted
	// fleet stage remains one visible outcome rather than thousands of repeated lines.
	type entryGroup struct {
		en       controller.Entry
		nodes    []string
		restores []protocol.OwedRestore
	}
	var entries []entryGroup
	indices := map[string]int{}
	for _, en := range r.OrderedEntries() {
		restores := currentOwedRestores(en.Response)
		b, _ := json.Marshal([]any{en.Response.Status, en.Response.Error, en.Housekeeping, en.Response.Housekeeping, en.Response.Outputs, restores})
		key := en.FleetStage + "\x00" + string(b)
		if i, ok := indices[key]; grouped && ok {
			entries[i].nodes = append(entries[i].nodes, en.Node) //nolint:nilaway // The index is stored when its entry is appended and read only after a successful lookup.
		} else {
			indices[key] = len(entries)
			entries = append(entries, entryGroup{en, []string{en.Node}, restores})
		}
	}
	for _, group := range entries {
		en := group.en
		location := en.FleetStage + " / " + nodeLabel(group.nodes, grouped)
		fmt.Fprintf(w, "%s: %s\n", location, en.Response.Status)
		for _, warning := range append(append([]string(nil), en.Housekeeping...), en.Response.Housekeeping...) {
			fmt.Fprintf(w, "  housekeeping: %s\n", warning)
		}
		if en.Response.Error != "" {
			fmt.Fprintf(w, "  %s\n", en.Response.Error)
		}
		if details && len(en.Response.Outputs) > 0 {
			b, _ := json.Marshal(value.Display(en.Response.Outputs))
			fmt.Fprintf(w, "  outputs: %s\n", b)
		}
	}
	if details && !grouped {
		for _, en := range r.OrderedEntries() {
			if en.Response.Facts == nil {
				continue
			}
			fmt.Fprintf(w, "  %s / %s: facts collected at %s\n", en.FleetStage, en.Node, en.Response.Facts.CollectedAt.Format(time.RFC3339Nano))
			if len(en.FactsChanged) > 0 {
				fmt.Fprintf(w, "    facts changed since the previous reading in the run: %s\n", strings.Join(en.FactsChanged, ", "))
			}
			paths := make([]string, 0, len(en.Response.Facts.Unavailable))
			for path := range en.Response.Facts.Unavailable {
				paths = append(paths, path)
			}
			sort.Strings(paths)
			for _, path := range paths {
				fmt.Fprintf(w, "    %s unavailable: %s\n", path, en.Response.Facts.Unavailable[path])
			}
		}
	}
	for _, group := range outcomeGroups(r, grouped) {
		result := group.result
		location := group.fleetStage + " / " + nodeLabel(group.nodes, grouped)
		fmt.Fprintf(w, "  %s / %s: %s, %s", location, result.Task, result.Status, result.Change)
		if result.Reason != "" {
			fmt.Fprint(w, " — ", result.Reason)
		}
		fmt.Fprintln(w)
		if result.Omitted != "" {
			fmt.Fprintf(w, "    %s\n", result.Omitted)
		}
		if v := result.CheckNewFile; v != nil {
			fmt.Fprintf(w, "    check_new_file: %s", v.Status)
			if v.Reason != "" {
				fmt.Fprint(w, " — ", v.Reason)
			}
			fmt.Fprintln(w)
			// A rejected candidate's cause is the validator's own output.
			if details || v.Status == "failed" {
				commandOutput(w, *v, "      ")
			}
		}
		if result.Value != nil || result.ValueSet {
			b, _ := json.Marshal(value.Display(result.Value))
			fmt.Fprintf(w, "    %s\n", b)
		}
		if result.PublicKey != "" {
			fmt.Fprintf(w, "    public_key: %s\n", result.PublicKey)
		}
		for _, name := range slices.Sorted(maps.Keys(result.ModuleOutputs)) {
			b, _ := json.Marshal(value.Display(result.ModuleOutputs[name]))
			fmt.Fprintf(w, "    %s: %s\n", name, b)
		}
		// A module's own difference, shown like a file's.
		if result.Diff != "" && p.diff != "" {
			fmt.Fprintln(w, "    "+strings.ReplaceAll(strings.TrimSuffix(result.Diff, "\n"), "\n", "\n    "))
		}
		// An mmx run-command command's output is its result.
		if details || result.Status == "failed" || r.EntryPoint == controller.RunCommandEntryPoint {
			commandOutput(w, result, "    ")
		}
		fileChanges(w, result, location, p.diff == "preview")
		directoryChanges(w, result, location, p.diff == "preview")
		if p.diff == "all" {
			if err := p.resultDiffs(w, r, result, location); err != nil {
				return err
			}
		}
	}
	// Print the current recovery state after the original task failures, whose
	// reasons can still describe an earlier, failed restore attempt.
	for _, group := range entries {
		location := group.en.FleetStage + " / " + nodeLabel(group.nodes, grouped)
		for _, restore := range group.restores {
			var outcome string
			subject, changeLabel := restore.DestinationFile, "file change"
			if subject == "" {
				subject, changeLabel = restore.ChangeSubject, "membership change"
			}
			switch restore.RestoreStatus {
			case "succeeded":
				outcome = subject + " was restored to its previous state"
			case "running":
				outcome = "the restore of " + subject + " is still running"
			case "not_started", "required":
				outcome = "the restore of " + subject + " has not started yet"
			case "failed":
				outcome = "the restore of " + subject + " failed"
			default:
				continue
			}
			fmt.Fprintf(w, "  %s / %s %s: %s\n", location, changeLabel, restore.OwedRestoreID, outcome)
			if restore.OriginalFailureReason != "" {
				fmt.Fprintf(w, "    why the change was not kept: %s\n", restore.OriginalFailureReason)
			}
			if restore.RestoreStatus == "failed" {
				if restore.RestoreFailureReason != "" {
					fmt.Fprintf(w, "    why the restore failed: %s\n", restore.RestoreFailureReason)
				}
				yesNo := func(done bool) string {
					if done {
						return "yes"
					}
					return "no"
				}
				if restore.DestinationFile == "" {
					fmt.Fprintf(w, "    Previous membership restored in both firewall layers: %s.\n", yesNo(restore.PreviousMembershipRestored))
				} else {
					fmt.Fprintf(w, "    previous file put back: %s; previous configuration activated: %s; virtual devices of the new file removed: %s\n", yesNo(restore.PreviousFileRestored), yesNo(restore.PreviousConfigActivated), yesNo(restore.ObsoleteVirtualDevicesRemoved))
				}
				if restore.RestoreServiceUnit != "" {
					fmt.Fprintf(w, "    after you corrected the cause, run on the node: systemctl start %s\n", restore.RestoreServiceUnit)
				}
			} else if restore.RestoreStatus != "succeeded" && restore.RestoreServiceUnit != "" {
				fmt.Fprintf(w, "    restore service on the node: %s\n", restore.RestoreServiceUnit)
			}
		}
	}
	// Files that save_on_controller_machine wrote on the controller machine.
	paths := make([]string, 0, len(r.Selections))
	for path := range r.Selections {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	for _, path := range paths {
		//nolint:nilaway // Selection paths come from entries allocated by scheduling or loading and retained throughout display.
		for _, file := range r.Selections[path].ControllerMachineFiles {
			result := file.Result.Safe()
			if file.Node != "" {
				result.Task += " from " + file.Node
			}
			location := path + " / controller machine"
			fmt.Fprintf(w, "  %s / %s: %s, %s", location, result.Task, result.Status, result.Change)
			if result.Reason != "" {
				fmt.Fprint(w, " — ", result.Reason)
			}
			fmt.Fprintln(w)
			fileChanges(w, result, location, p.diff != "")
		}
	}
	if p.diff == "all" {
		for _, en := range r.OrderedEntries() {
			for n, a := range en.PreviousNodeJobAttempts {
				for _, result := range a.Response.Results {
					location := fmt.Sprintf("%s / %s / previous attempt %d", en.FleetStage, en.Node, n+1)
					if err := p.resultDiffs(w, r, result.Safe(), location); err != nil {
						return err
					}
				}
			}
		}
	}
	return nil
}

// commandOutput prints a safe result's exit code and captured streams, marking
// text cut at the capture limit.
func commandOutput(w io.Writer, result protocol.Result, indent string) {
	if result.HTTPStatusCode != 0 {
		fmt.Fprintf(w, "%shttp_status_code: %d\n", indent, result.HTTPStatusCode)
	}
	if result.ResponseBody != "" {
		fmt.Fprintln(w, indent+"response_body: "+strings.ReplaceAll(strings.TrimSuffix(result.ResponseBody, "\n"), "\n", "\n"+indent))
	}
	if result.ExitCode != 0 {
		fmt.Fprintf(w, "%sexit code: %d\n", indent, result.ExitCode)
	}
	for _, text := range []string{result.Stdout, result.Stderr} {
		if text != "" {
			fmt.Fprintln(w, indent+strings.ReplaceAll(strings.TrimSuffix(text, "\n"), "\n", "\n"+indent))
		}
	}
}
