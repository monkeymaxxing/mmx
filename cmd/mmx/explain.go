package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"mmx/internal/config"
	"mmx/internal/modules"
)

func explain(w io.Writer, view config.Inspection) error {
	buffer := bufio.NewWriter(w)
	w = buffer
	fmt.Fprintf(w, "Project %s\n", view.Project)
	if len(view.Limit) > 0 {
		fmt.Fprintf(w, "Limit %s\n", strings.Join(view.Limit, ","))
	}
	if len(view.OnlyFleetStages) > 0 {
		fmt.Fprintf(w, "Only fleet stages %s\n", strings.Join(view.OnlyFleetStages, ","))
	}
	for _, v := range view.CommandLineVars {
		b, _ := json.Marshal(v.Value)
		fmt.Fprintf(w, "Command-line variable %s = %s\n  from %s\n", v.Field, b, v.Source)
	}
	for _, fleetStage := range view.FleetStages {
		nodes := strings.Join(fleetStage.Nodes, ", ")
		switch {
		case fleetStage.LeftOutByOnlyFleetStages:
			nodes = "not selected; left out by --only-fleet-stages"
		case fleetStage.Runtime:
			b, _ := json.Marshal(fleetStage.Selection)
			nodes = "node selection resolved during the run: " + string(b)
		case fleetStage.LeftOutBecauseItSelectsNoNodes:
			nodes = "not selected; " + selectsNoNodes
		case len(fleetStage.Nodes) == 0:
			nodes = "not selected; left out by the limit"
		}
		if fleetStage.ForEachNodeLoop {
			nodes = "for each node: " + nodes
		}
		fmt.Fprintf(w, "\n%s → %s\n", fleetStage.Path, nodes)
		if fleetStage.Condition != nil {
			condition, _ := json.Marshal(fleetStage.Condition)
			fmt.Fprintf(w, "  when: %s\n", condition)
		}
		if limit := string(fleetStage.MaxNodesAtOnce); limit != "" {
			if fleetStage.ResolvedMaxNodesAtOnce != nil && strings.HasSuffix(limit, "%") {
				limit += fmt.Sprintf(" = %d of %s", *fleetStage.ResolvedMaxNodesAtOnce, nodeCount(len(fleetStage.Nodes)))
			}
			fmt.Fprintf(w, "  max_nodes_at_once: %s\n", limit)
		}
		if allowance := string(fleetStage.FailedNodesAllowed); allowance != "" {
			if fleetStage.AllowedFailures != nil && strings.HasSuffix(allowance, "%") {
				allowance += fmt.Sprintf(" = %d of %s", *fleetStage.AllowedFailures, nodeCount(len(fleetStage.Nodes)))
			}
			fmt.Fprintf(w, "  failed_nodes_allowed: %s\n", allowance)
		}
		for _, task := range fleetStage.NodeTasks {
			action := task.Action
			if label := moduleKindLabels[task.ModuleKind]; label != "" {
				action += " (" + label + ")"
			}
			fmt.Fprintf(w, "  %s / %s: %s (%s)\n", task.Node, task.Path, action, task.Status)
			fmt.Fprintf(w, "    at %s\n", task.Source)
			for _, arg := range append(append([]config.Argument{}, task.Arguments...), task.Timing...) {
				v := arg.Value
				if arg.Status == "condition" {
					v = arg.Reference
				}
				if arg.Status == "secret" {
					// A vault secret has a name; a secret output from another
					// node has only its reference.
					v = "secret: " + strings.Join(arg.Secrets, ", ")
					if len(arg.Secrets) == 0 {
						v = fmt.Sprint("secret: ", arg.Reference)
					}
				}
				if arg.Status == "runtime" || arg.Status == "conditional" || arg.Status == "skipped" {
					v = arg.Status + " (not evaluated)"
				}
				b, _ := json.Marshal(v)
				text := string(b)
				if len(text) > 240 {
					text = text[:237] + "..."
				}
				fmt.Fprintf(w, "    %s = %s\n", arg.Field, text)
				fmt.Fprintf(w, "      from %s\n", arg.Source)
				seen := map[config.Location]bool{arg.Source: true}
				for _, at := range arg.Origins {
					if at != (config.Location{}) && !seen[at] {
						fmt.Fprintf(w, "      value from %s\n", at)
						seen[at] = true
					}
				}
				if arg.Reference != nil && (arg.Status == "runtime" || arg.Status == "conditional" || arg.Status == "skipped") {
					reference, _ := json.Marshal(arg.Reference)
					fmt.Fprintf(w, "      reference %s\n", reference)
				}
			}
			for _, rendered := range task.RenderedTemplates {
				if rendered.Text == nil {
					fmt.Fprintf(w, "    %s, not rendered by mmx explain: %s\n", rendered.Field, rendered.NotRenderedBecause)
					continue
				}
				fmt.Fprintf(w, "    %s, rendered%s:\n", rendered.Field, forNode(task.Node))
				lines := strings.Split(strings.TrimSuffix(*rendered.Text, "\n"), "\n")
				if *rendered.Text == "" {
					lines = []string{"(empty)"}
				}
				for _, line := range lines {
					fmt.Fprintln(w, strings.TrimRight("      "+line, " "))
				}
			}
			for _, expanded := range task.ExpandedInventoryGroupRules {
				fmt.Fprintf(w, "    %s%s, with one rule for each node of each from_inventory_group:\n", expanded.Field, forNode(task.Node))
				for _, rule := range expanded.Rules {
					b, _ := json.Marshal(rule)
					fmt.Fprintf(w, "      %s\n", b)
				}
			}
		}
		for i, file := range fleetStage.SaveOnControllerMachine {
			from := "the output " + file.Output
			if file.Template != "" {
				from = fmt.Sprintf("the template %s (%s)", file.Template, file.TemplateFile)
			}
			fmt.Fprintf(w, "  %s: writes on the controller machine from %s, permissions_mode %s\n", config.ControllerMachineFileField(i), from, file.PermissionsMode)
			fmt.Fprintf(w, "    at %s\n", file.Source)
			for _, saved := range file.Files {
				if saved.Node != "" {
					fmt.Fprintf(w, "    %s: %s\n", saved.Node, saved.File)
				} else {
					fmt.Fprintf(w, "    %s\n", saved.File)
				}
			}
			if file.FilesResolvedDuringRun {
				path, _ := json.Marshal(file.Path)
				fmt.Fprintf(w, "    files resolved during the run, from the path %s\n", path)
			}
			if file.SecretReadableByGroup {
				fmt.Fprintf(w, "    secret_readable_by_group: true\n")
			}
			if file.DangerouslyLetEveryAccountReadSecret {
				fmt.Fprintf(w, "    dangerously_let_every_account_read_secret: true\n")
			}
		}
	}
	showHelperNodes(w, view.HelperNodes)
	return buffer.Flush()
}

// forNode names the node that a rendered view is for; a node selected during
// the run has no name yet.
func forNode(node string) string {
	if node == "" {
		return ""
	}
	return " for " + node
}

// moduleKindLabels name the kind beside a node task whose module is not core.
var moduleKindLabels = map[string]string{
	modules.KindOfficialExtra:     "official extra",
	modules.KindProjectStarlark:   "project's own, Starlark",
	modules.KindProjectExecutable: "project's own, executable",
}
