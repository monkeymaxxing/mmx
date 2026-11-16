package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"

	"mmx/internal/config"
)

// nodesView is mmx nodes: every inventory node and group, and each fleet stage's
// selection after the limit, taken from the inspection explain and run use.
type nodesView struct {
	SchemaVersion int    `json:"schema_version"`
	Playbook      string `json:"playbook,omitempty"`
	// Inventory is the inventory's name, as in the results of a run.
	Inventory     string              `json:"inventory"`
	InventoryFile string              `json:"inventory_file"`
	Limit         []string            `json:"limit,omitempty"`
	Nodes         []nodeRow           `json:"nodes"`
	Groups        map[string][]string `json:"groups"`
	FleetStages   []fleetStageRow     `json:"fleet_stages,omitempty"`

	OnlyFleetStages []string            `json:"only_fleet_stages,omitempty"`
	HelperNodes     []config.HelperNode `json:"helper_nodes,omitempty"`
}
type nodeRow struct {
	Name    string   `json:"name"`
	Address string   `json:"address"`
	User    string   `json:"ssh_user,omitempty"`
	Groups  []string `json:"groups,omitempty"`
}
type fleetStageRow struct {
	FleetStage string   `json:"fleet_stage"`
	Selection  any      `json:"selection"`
	Runtime    bool     `json:"resolved_during_run,omitempty"`
	Nodes      []string `json:"nodes,omitempty"`
	LeftOut    []string `json:"left_out,omitempty"`

	ExceptNodes              any  `json:"except_nodes,omitempty"`
	LeftOutByOnlyFleetStages bool `json:"left_out_by_only_fleet_stages,omitempty"`
	// LeftOutBecauseItSelectsNoNodes: its own selection is empty, and
	// skip_when_no_nodes_are_selected leaves it out of the run.
	LeftOutBecauseItSelectsNoNodes bool `json:"left_out_because_it_selects_no_nodes,omitempty"`
	// NarrowedByWhen: a when, of the fleet stage or of its for_each_node
	// loop, is checked for each node during the run.
	NarrowedByWhen bool `json:"narrowed_during_run_by_when,omitempty"`
}

func newNodesView(playbook string, inv config.Inventory, inspection config.Inspection) nodesView {
	view := nodesView{SchemaVersion: 1, Playbook: playbook, Inventory: inv.Name, InventoryFile: inv.Path, Limit: inspection.Limit, OnlyFleetStages: inspection.OnlyFleetStages, Groups: map[string][]string{}}
	for name, group := range inv.Groups {
		view.Groups[name] = group.Nodes
	}
	for name, h := range inv.Nodes {
		view.Nodes = append(view.Nodes, nodeRow{Name: name, Address: h.Address, User: h.User, Groups: config.NodeGroups(inv, name)})
	}
	sort.Slice(view.Nodes, func(i, j int) bool { return view.Nodes[i].Name < view.Nodes[j].Name })
	for _, fleetStage := range inspection.FleetStages {
		// A for_each_node loop is shown through its iterations' fleet stages;
		// one that selects no nodes has none, so it is shown itself.
		if fleetStage.ForEachNodeLoop && !fleetStage.LeftOutBecauseItSelectsNoNodes {
			continue
		}
		view.FleetStages = append(view.FleetStages, fleetStageRow{FleetStage: fleetStage.Path, Selection: fleetStage.Selection, Runtime: fleetStage.Runtime, Nodes: fleetStage.Nodes, LeftOut: fleetStage.LeftOut, ExceptNodes: fleetStage.ExceptNodes, LeftOutByOnlyFleetStages: fleetStage.LeftOutByOnlyFleetStages, LeftOutBecauseItSelectsNoNodes: fleetStage.LeftOutBecauseItSelectsNoNodes, NarrowedByWhen: fleetStage.NarrowedByWhen})
	}
	view.HelperNodes = inspection.HelperNodes
	return view
}

// showNodes prints the view. Summary output, the automatic choice above ten
// nodes, gives counts per group and fleet stage instead of node lists.
func showNodes(out io.Writer, view nodesView, mode string, asJSON bool) (int, error) {
	if asJSON {
		return encode(view)
	}
	out = terminalSafe(out)
	if mode == "auto" {
		mode = "nodes"
		if len(view.Nodes) > 10 {
			mode = "summary"
		}
	}
	list := func(nodes []string) string {
		if mode == "summary" {
			return nodeCount(len(nodes))
		}
		return strings.Join(nodes, ", ")
	}
	w := tabwriter.NewWriter(out, 0, 0, 3, ' ', 0)
	if view.Playbook != "" {
		fmt.Fprintf(w, "Playbook: %s   ", view.Playbook)
	}
	fmt.Fprintf(w, "Inventory: %s (%s, %s)\n", view.Inventory, displayPath(view.InventoryFile), nodeCount(len(view.Nodes)))
	if len(view.Limit) > 0 {
		fmt.Fprintf(w, "Limit: %s\n", strings.Join(view.Limit, ","))
	}
	if len(view.OnlyFleetStages) > 0 {
		fmt.Fprintf(w, "Only fleet stages: %s\n", strings.Join(view.OnlyFleetStages, ","))
	}
	if mode != "summary" {
		fmt.Fprintln(w, "\nNodes")
		for _, h := range view.Nodes {
			user := h.User
			if user == "" {
				user = "-"
			}
			fmt.Fprintf(w, "  %s\t%s\t%s\t%s\n", h.Name, h.Address, user, strings.Join(h.Groups, ", "))
		}
	}
	if len(view.Groups) > 0 {
		fmt.Fprintln(w, "\nGroups")
		names := make([]string, 0, len(view.Groups))
		for name := range view.Groups {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			fmt.Fprintf(w, "  %s\t%s\n", name, list(view.Groups[name]))
		}
	}
	if len(view.FleetStages) == 0 {
		return 0, w.Flush()
	}
	heading := "\nFleet stages"
	if len(view.Limit) > 0 {
		heading += " (limit applied)"
	}
	fmt.Fprintln(w, heading)
	for _, fleetStage := range view.FleetStages {
		selected := list(fleetStage.Nodes)
		written := selector(fleetStage.Selection)
		if fleetStage.ExceptNodes != nil {
			written += " except " + selector(fleetStage.ExceptNodes)
		}
		switch {
		case fleetStage.LeftOutByOnlyFleetStages:
			selected = "none; left out by --only-fleet-stages"
		case fleetStage.Runtime:
			selected = "resolved during the run"
		case fleetStage.LeftOutBecauseItSelectsNoNodes:
			selected = "none; " + selectsNoNodes
		case len(fleetStage.Nodes) == 0:
			selected = "none; left out by the limit"
		}
		if fleetStage.NarrowedByWhen {
			selected += "; narrowed during the run by `when`"
		}
		fmt.Fprintf(w, "  %s\t%s\t→ %s\n", fleetStage.FleetStage, written, selected)
	}
	showHelperNodes(w, view.HelperNodes)
	return 0, w.Flush()
}

// showHelperNodes lists the nodes outside the limit that kept for_each_node
// iterations work on, with those fleet stages. nodes, explain, dryrun, and
// run print it before any connection.
func showHelperNodes(out io.Writer, helpers []config.HelperNode) {
	if len(helpers) == 0 {
		return
	}
	w := tabwriter.NewWriter(out, 0, 0, 3, ' ', 0)
	fmt.Fprintln(w, "\nNodes outside --limit that help selected nodes")
	for _, helper := range helpers {
		fmt.Fprintf(w, "  %s\t%s\n", helper.Node, strings.Join(helper.FleetStages, ", "))
	}
	w.Flush()
}

// selector shows a written nodes field: a name, a reference, or a list.
func selector(v any) string {
	if v == nil {
		return "-"
	}
	if list, ok := v.([]any); ok {
		names := make([]string, len(list))
		for i, name := range list {
			names[i] = fmt.Sprint(name)
		}
		return strings.Join(names, ", ")
	}
	if s, ok := v.(string); ok {
		return s
	}
	b, _ := json.Marshal(v)
	return string(b)
}

func nodeCount(n int) string {
	if n == 1 {
		return "1 node"
	}
	return fmt.Sprintf("%d nodes", n)
}

// displayPath shows a path relative to the current directory when it can.
func displayPath(path string) string {
	if dir, err := os.Getwd(); err == nil {
		if rel, err := filepath.Rel(dir, path); err == nil {
			return rel
		}
	}
	return path
}
