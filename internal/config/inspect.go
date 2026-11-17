package config

import (
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"

	"mmx/internal/modules"
	"mmx/internal/value"
)

type Inspection struct {
	Project string   `json:"project"`
	Limit   []string `json:"limit,omitempty"`
	// OnlyFleetStages are the --only-fleet-stages IDs; the fleet stages they
	// leave out are listed as not selected.
	OnlyFleetStages []string `json:"only_fleet_stages,omitempty"`
	// CommandLineVars shows each command-line variable's value and whether it
	// came from --var or the default.
	CommandLineVars []Argument            `json:"command_line_vars,omitempty"`
	FleetStages     []ExplainedFleetStage `json:"fleet_stages"`
	// HelperNodes are the nodes outside the limit that a kept for_each_node
	// iteration's fleet stages run on.
	HelperNodes []HelperNode `json:"helper_nodes,omitempty"`
}

// HelperNode is a node outside the limit and the fleet stages that do work on
// it for a kept for_each_node iteration.
type HelperNode struct {
	Node        string   `json:"node"`
	FleetStages []string `json:"fleet_stages"`
}

// ExplainedStep is one node fleet stage: its written selection, and the nodes it
// selects after the limit unless the selection resolves during the run. A
// for_each_node loop has a view of its own, with its loop nodes as Nodes.
type ExplainedFleetStage struct {
	Path      string          `json:"path"`
	Selection any             `json:"selection"`
	Runtime   bool            `json:"resolved_during_run,omitempty"`
	Nodes     []string        `json:"nodes,omitempty"`
	LeftOut   []string        `json:"left_out,omitempty"`
	Condition any             `json:"when,omitempty"`
	Source    Location        `json:"source"`
	NodeTasks []ExplainedTask `json:"node_tasks"`
	// SaveOnControllerMachine lists what the fleet stage writes on the
	// controller machine after its nodes finish.
	SaveOnControllerMachine []ExplainedControllerMachineFile `json:"save_on_controller_machine,omitempty"`

	// FailedHostsAllowed is the written allowance; AllowedFailures is its
	// count for the selected nodes, unknown for a runtime selection.
	FailedNodesAllowed FailureAllowance `json:"failed_nodes_allowed,omitempty"`
	AllowedFailures    *int             `json:"allowed_failures,omitempty"`

	// ExceptNodes is the written except_nodes, which Nodes already excludes.
	ExceptNodes any `json:"except_nodes,omitempty"`
	// LeftOutByOnlyFleetStages: --only-fleet-stages does not name this
	// top-level fleet stage or loop, so the command leaves it out.
	LeftOutByOnlyFleetStages bool `json:"left_out_by_only_fleet_stages,omitempty"`
	// LeftOutBecauseItSelectsNoNodes: its own selection is empty, and
	// skip_when_no_nodes_are_selected leaves it out of the run.
	LeftOutBecauseItSelectsNoNodes bool `json:"left_out_because_it_selects_no_nodes,omitempty"`

	// ForEachNodeLoop marks a for_each_node loop's own view. MaxNodesAtOnce is
	// the written setting and ResolvedMaxNodesAtOnce its count for the
	// selected nodes. NarrowedByWhen marks a fleet stage whose when, or whose
	// for_each_node loop's when, is checked for each node during the run.
	ForEachNodeLoop        bool        `json:"for_each_node_loop,omitempty"`
	MaxNodesAtOnce         NodesAtOnce `json:"max_nodes_at_once,omitempty"`
	ResolvedMaxNodesAtOnce *int        `json:"resolved_max_nodes_at_once,omitempty"`
	NarrowedByWhen         bool        `json:"narrowed_during_run_by_when,omitempty"`
}
type ExplainedTask struct {
	Path   string `json:"path"`
	Node   string `json:"node,omitempty"`
	Action string `json:"action"`
	// ModuleKind is the kind of the module the task calls: core,
	// official_extra, project_starlark, or project_executable.
	ModuleKind string     `json:"module_kind,omitempty"`
	Status     string     `json:"status"`
	Source     Location   `json:"source"`
	Arguments  []Argument `json:"arguments,omitempty"`
	Timing     []Argument `json:"timing,omitempty"`
	// RenderedTemplates and ExpandedInventoryGroupRules are what MMX derives
	// from the task's arguments for its node. They stay outside Arguments,
	// which hold what the playbook wrote, and only explain shows them
	// (InspectForExplain).
	RenderedTemplates           []RenderedTemplate            `json:"rendered_templates,omitempty"`
	ExpandedInventoryGroupRules []ExpandedInventoryGroupRules `json:"expanded_inventory_group_rules,omitempty"`
}

// RenderedTemplate is a template file of a node task, rendered on the
// controller machine as the task's node gets it, or why explain did not
// render it: the template reads a value that only the node or the run knows,
// or it fails. The node renders it when the task runs.
type RenderedTemplate struct {
	Field              string  `json:"field"`
	Text               *string `json:"text,omitempty"`
	NotRenderedBecause string  `json:"not_rendered_because,omitempty"`
}

// ExpandedInventoryGroupRules is a rule list as the task's node gets it, with
// one rule for each node of each from_inventory_group.
type ExpandedInventoryGroupRules struct {
	Field string `json:"field"`
	Rules []any  `json:"rules"`
}

// renderTemplates renders a node task's template files with the values the
// controller machine knows for its node.
func renderTemplates(t Task, scope value.Scope) []RenderedTemplate {
	name, reads := moduleReads(t)
	var out []RenderedTemplate
	for _, field := range sortedKeys(reads.Templates) {
		rendered := RenderedTemplate{Field: name + "." + field}
		var unavailable *value.UnavailableError
		if text, err := scope.Template(reads.Templates[field]); errors.As(err, &unavailable) {
			rendered.NotRenderedBecause = "the template reads " + unavailable.Path + ", " + unavailable.Reason
		} else if err != nil {
			rendered.NotRenderedBecause = "template rendering failed: " + err.Error()
		} else {
			rendered.Text = &text
		}
		out = append(out, rendered)
	}
	return out
}

// expandInventoryGroupRules gives the rule lists of a task whose rules name
// from_inventory_group as its node gets them: resolving the arguments expands
// the groups from the inventory. Rules that read a value known only during
// the run are left to the node.
func expandInventoryGroupRules(t Task, scope value.Scope) ([]ExpandedInventoryGroupRules, error) {
	if _, inputs := t.ModuleCall(); !namesInventoryGroups(inputs) {
		return nil, nil
	}
	resolved, err := ResolveArguments(withoutSourceVariants(t), scope)
	if unknown(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	name, inputs := resolved.ModuleCall()
	lists := inputs.(inventoryGroupRules).InventoryGroupRuleLists()
	var out []ExpandedInventoryGroupRules
	for _, field := range sortedKeys(lists) {
		out = append(out, ExpandedInventoryGroupRules{Field: name + "." + field, Rules: lists[field]})
	}
	return out, nil
}

func namesInventoryGroups(inputs any) bool {
	rules, ok := inputs.(inventoryGroupRules)
	return ok && rules.NamesInventoryGroups()
}

// ExplainedControllerMachineFile is one save_on_controller_machine entry of a
// fleet stage. Path is the path as written; Files are the files it names on
// the controller machine: one for each selected node for an output entry, one
// for a template entry. FilesResolvedDuringRun marks a path that reads a value
// known only during the run, or an output entry of a fleet stage whose nodes
// are selected during the run: explain cannot name its files.
type ExplainedControllerMachineFile struct {
	Output       string `json:"output,omitempty"`
	Template     string `json:"template,omitempty"`
	TemplateFile string `json:"template_file,omitempty"`

	Path                   string               `json:"path"`
	Files                  []ExplainedSavedFile `json:"files,omitempty"`
	FilesResolvedDuringRun bool                 `json:"files_resolved_during_run,omitempty"`

	PermissionsMode                      string   `json:"permissions_mode"`
	SecretReadableByGroup                bool     `json:"secret_readable_by_group,omitempty"`
	DangerouslyLetEveryAccountReadSecret bool     `json:"dangerously_let_every_account_read_secret,omitempty"`
	Source                               Location `json:"source"`
}

// ExplainedSavedFile is a file a save_on_controller_machine entry writes, and
// the node whose output it holds; a template entry's file has no node.
type ExplainedSavedFile struct {
	Node string `json:"node,omitempty"`
	File string `json:"file"`
}

// explainControllerMachineFiles lists a fleet stage's save_on_controller_machine
// entries with the files each writes. scope is the fleet stage's, nodes its
// selected nodes, and runtime says that they are selected during the run.
func explainControllerMachineFiles(s FleetStage, scope value.Scope, nodes []string, runtime bool) ([]ExplainedControllerMachineFile, error) {
	var out []ExplainedControllerMachineFile
	for i, e := range s.SaveOnControllerMachine {
		field := ControllerMachineFileField(i)
		view := ExplainedControllerMachineFile{Output: e.Output, Template: e.Template, TemplateFile: e.TemplateFile, Path: e.Path,
			PermissionsMode: e.PermissionsMode, SecretReadableByGroup: e.SecretReadableByGroup,
			DangerouslyLetEveryAccountReadSecret: e.DangerouslyLetEveryAccountReadSecret, Source: s.Sources.At(field)}
		writers := []string{""}
		if e.Output != "" {
			writers = nodes
			view.FilesResolvedDuringRun = runtime
		}
		for _, node := range writers {
			if view.FilesResolvedDuringRun {
				break
			}
			// As when the run writes it: the fleet stage's values, and the
			// node's name for an output entry.
			local := scope.Local()
			if node != "" {
				delete(local.Missing(), value.Pointer("node"))
				local["node"] = map[string]any{"name": node}
			}
			resolved, err := local.Resolve(e.Path)
			if unknown(err) {
				view.Files, view.FilesResolvedDuringRun = nil, true
				break
			}
			var written, file string
			if err == nil {
				written, err = value.Scalar(resolved)
			}
			if err == nil {
				err = NodeValuesStayInOneFileName(e.Path, written, local)
			}
			if err == nil {
				file, err = ControllerMachinePath(e.ProjectRoot, written)
			}
			if err != nil {
				return nil, s.Sources.Error(field+".path", err)
			}
			view.Files = append(view.Files, ExplainedSavedFile{Node: node, File: file})
		}
		out = append(out, view)
	}
	return out, nil
}

func unknown(err error) bool {
	var unavailable *value.UnavailableError
	return errors.As(err, &unavailable)
}
func localScope(p Project, inv Inventory, managedNode string, inventory any) value.Scope {
	s := value.Scope{"vars": NodeVars(p, inv, managedNode), "inputs": map[string]any{}, "node_tasks": map[string]any{}, "inventory": inventory}
	for _, root := range []string{"node", "node_tasks", "outputs", "fleet_stages", "secrets"} {
		s.Mark(value.Pointer(root), "resolved during execution")
	}
	// A named node's name is known before MMX connects to it; its facts are not.
	if managedNode != "" {
		delete(s.Missing(), value.Pointer("node"))
		s["node"] = map[string]any{"name": managedNode}
		s.MarkPartlyKnown(value.Pointer("node"), "a fact that the node reports when MMX connects to it")
	}
	// validate and nodes accept a missing command-line value; the other
	// commands require one (RequireCommandLineValues).
	for name := range p.CommandLineVars {
		if _, ok := s["vars"].(map[string]any)[name]; !ok {
			reason := "set with --var " + name + "=VALUE"
			if p.WholeProjectValidation {
				reason += notGivenToWholeProjectValidation
			}
			s.Mark(value.Pointer("vars", name), reason)
		}
	}
	SetOrigins(s, VariableOrigins(p, inv, managedNode))
	return s
}

// Inspect uses the ordinary resolver with explicit unavailable runtime roots.
// It neither executes resources nor loads credentials, secrets, or downloads.
// It is the one local view of each fleet stage's selection: explain, nodes, and a
// limited dryrun or run use it, and it rejects a limit or an
// --only-fleet-stages value that leaves out work a kept fleet stage needs.
func Inspect(p Project, inv Inventory, limitSelectors []string) (Inspection, error) {
	return inspect(p, inv, limitSelectors, false)
}

// InspectForExplain is Inspect with each node task's rendered templates and
// expanded inventory group rules, which explain shows. Inspect leaves them
// out: every run inspects, and rendering each template for each node would
// make it hold every node's files in memory.
func InspectForExplain(p Project, inv Inventory, limitSelectors []string) (Inspection, error) {
	return inspect(p, inv, limitSelectors, true)
}

func inspect(p Project, inv Inventory, limitSelectors []string, derived bool) (Inspection, error) {
	result := Inspection{Project: p.Project, Limit: limitSelectors, OnlyFleetStages: p.OnlyFleetStages}
	if err := checkOnlyFleetStages(p); err != nil {
		return result, err
	}
	limit, err := NewLimit(inv, limitSelectors)
	if err != nil {
		return result, err
	}
	kept := false
	helpers := map[string][]string{}
	// limited maps a visible fleet stage ID to what the limit did to it: leftOut or
	// narrowed. Loop iterations extend a copy, as their fleet stage names stay local.
	// narrow is the limit that selections take: the run's limit, or none inside a
	// kept for_each_node iteration, whose fleet stages run on the nodes they name.
	// narrowedByWhen says that an enclosing for_each_node loop has a when.
	var fleetStages func([]FleetStage, value.Scope, string, string, map[string]string, Limit, bool) error
	fleetStages = func(list []FleetStage, outer value.Scope, prefix, inherited string, limited map[string]string, narrow Limit, narrowedByWhen bool) error {
		for _, fleetStage := range list {
			path := prefix + fleetStage.ID
			if prefix == "" && !p.KeepsFleetStage(fleetStage.ID) {
				// A left-out for_each_node loop shows its loop selection as a
				// plain row, so mmx nodes lists it as left out.
				written := fleetStage.Nodes
				if fleetStage.ForEachNode != nil {
					written = fleetStage.ForEachNode
				}
				result.FleetStages = append(result.FleetStages, ExplainedFleetStage{Path: path, Selection: written, ExceptNodes: fleetStage.ExceptNodes, LeftOutByOnlyFleetStages: true, Source: fleetStage.Sources.At("")})
				continue
			}
			if fleetStage.ForEachNode != nil {
				nodes, err := SelectNodes(inv, outer, fleetStage)
				if err != nil && !unknown(err) {
					return err
				}
				view := ExplainedFleetStage{Path: path, Selection: fleetStage.ForEachNode, ExceptNodes: fleetStage.ExceptNodes, Condition: fleetStage.When, Source: fleetStage.Sources.At(""), ForEachNodeLoop: true, NarrowedByWhen: narrowedByWhen || fleetStage.When != nil}
				view.Nodes, view.LeftOut = narrow.Narrow(nodes)
				view.Runtime = unknown(err)
				view.LeftOutBecauseItSelectsNoNodes = err == nil && len(nodes) == 0
				inspectRollout(&view, fleetStage)
				result.FleetStages = append(result.FleetStages, view)
				if !view.Runtime && len(view.Nodes) == 0 {
					// A loop that selects no nodes leaves nothing out that later
					// work needs: its outputs are an empty mapping.
					if len(view.LeftOut) > 0 {
						limited[fleetStage.ID] = leftOut
					}
					continue
				}
				kept = true
				if err := checkLimit(path, nil, []any{fleetStage.ForEachNode, fleetStage.ExceptNodes, fleetStage.When}, limited); err != nil {
					return err
				}
				if len(view.LeftOut) > 0 {
					limited[fleetStage.ID] = narrowed
				}
				iteration := func(child value.Scope, prefix, status string) error {
					local := maps.Clone(limited)
					if err := fleetStages(fleetStage.FleetStages, child, prefix, status, local, nil, view.NarrowedByWhen); err != nil {
						return err
					}
					return checkLimit(path, nil, fleetStage.Outputs, local)
				}
				if view.Runtime || inherited != "ready" {
					child := outer.Local()
					child.Mark(value.Pointer(fleetStage.As), "loop node resolved during execution")
					if err := iteration(child, path+"[runtime]/", "conditional"); err != nil {
						return err
					}
					continue
				}
				for _, node := range view.Nodes {
					child := bindItem(outer, fleetStage.As, value.Iteration{Key: node, Value: node})
					ok, err := child.Predicate(fleetStage.When)
					if err != nil && !unknown(err) {
						return SourceError(fleetStage.Sources, "when", fleetStage.When, child, err)
					}
					if err == nil && !ok {
						continue
					}
					status := inherited
					if unknown(err) {
						status = "conditional"
					}
					if err := iteration(child, path+"["+value.EscapeKey(node)+"]/", status); err != nil {
						return err
					}
				}
				continue
			}
			if fleetStage.Loop != nil {
				if err := checkLimit(path, nil, []any{fleetStage.Loop, fleetStage.When}, limited); err != nil {
					return err
				}
				iteration := func(child value.Scope, prefix, status string) error {
					local := maps.Clone(limited)
					if err := fleetStages(fleetStage.FleetStages, child, prefix, status, local, narrow, narrowedByWhen); err != nil {
						return err
					}
					return checkLimit(path, nil, fleetStage.Outputs, local)
				}
				var items []value.Iteration
				var err error
				if inherited == "ready" {
					items, err = outer.Iterations(fleetStage.Loop, fleetStage.As, fleetStage.Key)
				}
				if err != nil && !unknown(err) {
					return SourceError(fleetStage.Sources, "loop", fleetStage.Loop, outer, err)
				}
				if unknown(err) || inherited != "ready" {
					child := outer.Local()
					child.Mark(value.Pointer(fleetStage.As), loopItemReason(err))
					if err := iteration(child, path+"[runtime]/", "conditional"); err != nil {
						return err
					}
					continue
				}
				for _, item := range items {
					child := bindItem(outer, fleetStage.As, item)
					ok, err := child.Predicate(fleetStage.When)
					if err != nil && !unknown(err) {
						return SourceError(fleetStage.Sources, "when", fleetStage.When, child, err)
					}
					if err == nil && !ok {
						continue
					}
					status := inherited
					if unknown(err) {
						status = "conditional"
					}
					if err := iteration(child, path+"["+value.EscapeKey(item.Key)+"]/", status); err != nil {
						return err
					}
				}
				continue
			}
			nodes, err := SelectNodes(inv, outer, fleetStage)
			if err != nil && !unknown(err) {
				return err
			}
			view := ExplainedFleetStage{Path: path, Selection: fleetStage.Nodes, ExceptNodes: fleetStage.ExceptNodes, Condition: fleetStage.When, Source: fleetStage.Sources.At(""), NarrowedByWhen: narrowedByWhen || fleetStage.When != nil}
			if resolved, err := outer.Resolve(fleetStage.Nodes); err == nil {
				view.Selection = resolved // a loop variable shows the node it names, such as app02
			}
			view.Nodes, view.LeftOut = narrow.Narrow(nodes)
			view.Runtime = unknown(err)
			view.LeftOutBecauseItSelectsNoNodes = err == nil && len(nodes) == 0
			inspectRollout(&view, fleetStage)
			// A fleet stage that selects no nodes still writes a template
			// entry's file, from an empty output collection.
			files, filesErr := explainControllerMachineFiles(fleetStage, outer, view.Nodes, view.Runtime)
			if filesErr != nil {
				return filesErr
			}
			view.SaveOnControllerMachine = files
			// Inside a kept for_each_node iteration, a node outside the limit is a helper node.
			if narrow == nil && limit != nil {
				for _, node := range view.Nodes {
					if !limit[node] {
						helpers[node] = append(helpers[node], path)
					}
				}
			}
			if !unknown(err) && len(view.Nodes) == 0 {
				// A fleet stage that selects no nodes leaves nothing out that
				// later work needs: a requirement on it is met, and its outputs
				// are an empty mapping.
				if len(view.LeftOut) > 0 {
					limited[fleetStage.ID] = leftOut
				}
				result.FleetStages = append(result.FleetStages, view)
				continue
			}
			kept = true
			requires := append(append([]string{}, fleetStage.Requires.SameNode...), fleetStage.Requires.AllNodes...)
			if err := checkLimit(path, requires, []any{fleetStage.Nodes, fleetStage.ExceptNodes, fleetStage.When, fleetStage.NodeTasks, fleetStage.Outputs}, limited); err != nil {
				return err
			}
			if len(view.LeftOut) > 0 {
				limited[fleetStage.ID] = narrowed
			}
			if err := checkLimit(path, nil, controllerMachineFileReads(fleetStage), limited); err != nil {
				return fmt.Errorf("save_on_controller_machine: %w", err)
			}
			nodes = view.Nodes
			if unknown(err) {
				view.Runtime = true
				nodes = []string{""}
			}
			for _, managedNode := range nodes {
				scope := localScope(p, inv, managedNode, outer["inventory"])
				for k, v := range outer {
					if k != "vars" && k != "" && !strings.HasPrefix(k, "\x00") {
						scope[k] = v
					}
				}
				for key, reason := range outer.Missing() {
					if !strings.HasPrefix(key, "/vars/") && (managedNode == "" || key != value.Pointer("node")) {
						scope.Mark(key, reason)
					}
				}
				// A runtime-selected node's groups and own vars may override
				// any project or inventory variable.
				if managedNode == "" {
					for _, layer := range everyNodeLayer(inv) {
						for key := range layer.vars {
							scope.Mark(value.Pointer("vars", key), "node selection resolved during execution")
						}
					}
				}
				status := inherited
				ok := true
				var err error
				if status == "ready" {
					ok, err = scope.Predicate(fleetStage.When)
				}
				if err != nil && !unknown(err) {
					return SourceError(fleetStage.Sources, "when", fleetStage.When, scope, err)
				}
				if unknown(err) {
					status = "conditional"
				} else if !ok {
					status = "skipped"
				}
				nodeTasks, err := inspectNodeTasks(fleetStage.NodeTasks, scope, path+"/", managedNode, status, derived)
				if err != nil {
					return resultError(path, managedNode, err)
				}
				view.NodeTasks = append(view.NodeTasks, nodeTasks...)
			}
			result.FleetStages = append(result.FleetStages, view)
		}
		return nil
	}
	layer := commandLineLayer(p)
	for _, name := range sortedKeys(layer.vars) {
		result.CommandLineVars = append(result.CommandLineVars, Argument{Field: name, Value: layer.vars[name], Status: "resolved", Source: layer.sources.At("vars." + name)})
	}
	if err := fleetStages(p.FleetStages, localScope(p, inv, "", InventoryValues(inv)), "", "ready", map[string]string{}, limit, false); err != nil {
		return result, err
	}
	if limit != nil && !kept {
		return result, fmt.Errorf("--limit %s selects no node in any fleet stage", strings.Join(limitSelectors, ","))
	}
	for _, node := range sortedKeys(helpers) {
		result.HelperNodes = append(result.HelperNodes, HelperNode{Node: node, FleetStages: helpers[node]})
	}
	// A module that is not core gets its kind from the project's catalog.
	for i := range result.FleetStages {
		for j := range result.FleetStages[i].NodeTasks {
			task := &result.FleetStages[i].NodeTasks[j]
			if module, found := p.Modules[task.Action]; found && task.ModuleKind == "" {
				task.ModuleKind = module.Kind
			}
		}
	}
	return result, nil
}

// inspectRollout shows a fleet stage's or for_each_node loop's rollout
// settings, with their node counts when the selection is known before the run.
func inspectRollout(view *ExplainedFleetStage, s FleetStage) {
	view.FailedNodesAllowed, view.MaxNodesAtOnce = s.FailedNodesAllowed, s.MaxNodesAtOnce
	if view.Runtime {
		return
	}
	if s.FailedNodesAllowed != "" {
		allowed, _ := s.FailedNodesAllowed.Nodes(len(view.Nodes))
		view.AllowedFailures = &allowed
	}
	if s.MaxNodesAtOnce != "" {
		resolved, _ := s.MaxNodesAtOnce.Nodes(len(view.Nodes), 0)
		view.ResolvedMaxNodesAtOnce = &resolved
	}
}

const leftOut, narrowed = "left out", "narrowed"

// checkLimit rejects a fleet stage the limit keeps when it needs work the limit
// leaves out: a requirement or output read of a fleet stage left out entirely, or
// the whole output collection of a fleet stage the limit narrowed, which would
// silently lose nodes.
func checkLimit(path string, requires []string, reads any, limited map[string]string) error {
	for _, req := range requires {
		if limited[req] == leftOut {
			return fmt.Errorf("--limit excludes fleet stage %s, which fleet stage %s requires; include the required nodes in --limit or omit --limit", req, path)
		}
	}
	for _, ref := range References(reads) {
		parts, _ := value.Path(ref)
		if len(parts) < 2 || parts[0].Name != "outputs" {
			continue
		}
		switch producer := parts[1].Name; {
		case limited[producer] == leftOut:
			return fmt.Errorf("--limit excludes fleet stage %s, whose outputs fleet stage %s reads; include the output-producing nodes in --limit or omit --limit", producer, path)
		case limited[producer] == narrowed && len(parts) == 2:
			return fmt.Errorf("--limit excludes some nodes from fleet stage %s, whose whole output collection fleet stage %s reads; include all output-producing nodes in --limit or omit --limit", producer, path)
		}
	}
	return nil
}

// bindItem binds a loop item in a copy of s.
func bindItem(s value.Scope, as string, item value.Iteration) value.Scope {
	local := s.Local()
	local[as] = item.Value
	return local
}
func resultError(fleetStage, managedNode string, err error) error {
	if managedNode != "" {
		return fmt.Errorf("fleet stage %s, node %s: %w", fleetStage, managedNode, err)
	}
	return fmt.Errorf("fleet stage %s: %w", fleetStage, err)
}

// ExportOutputs computes a fleet stage's or role's declared outputs during execution;
// an error names the outputs field and the value's source, as inspection does.
func ExportOutputs(outputs map[string]any, scope value.Scope, sources Sources) (map[string]any, error) {
	out := map[string]any{}
	for _, key := range sortedKeys(outputs) {
		v, err := scope.Evaluate(outputs[key])
		if err != nil {
			return nil, SourceError(sources, "outputs."+key, outputs[key], scope, err)
		}
		out[key] = v
	}
	return out, nil
}

// derived adds each active task's rendered templates and expanded inventory
// group rules.
func inspectNodeTasks(nodeTasks []Task, scope value.Scope, prefix, managedNode, inherited string, derived bool) ([]ExplainedTask, error) {
	var out []ExplainedTask
	for i, t := range nodeTasks {
		path := prefix + TaskName(t, i)
		// A loop below a when that the node answers may run too, so its items
		// known before connecting expand and each item's source paths
		// resolve. The engine reads the loop only after that when, so a loop
		// error fails the inspection only of a loop that is sure to run; a
		// conditional one that fails stays one unexpanded node task.
		if t.Loop != nil && inherited != "skipped" {
			items, err := scope.Iterations(t.Loop, t.As, t.Key)
			if err != nil && !unknown(err) && inherited == "ready" {
				return nil, argumentError(t, "loop", t.Loop, scope, err)
			}
			if unknown(err) {
				local := scope.Local()
				local.Mark(value.Pointer(t.As), loopItemReason(err))
				child := t
				child.Loop = nil
				child.ID = TaskName(t, i) + "[runtime]"
				nested, err := inspectNodeTasks([]Task{child}, local, prefix, managedNode, "conditional", derived)
				if err != nil {
					return nil, err
				}
				out = append(out, nested...)
				continue
			}
			if err == nil {
				for _, item := range items {
					local := bindItem(scope, t.As, item)
					child := t
					child.Loop = nil
					child.ID = TaskName(t, i) + "[" + value.EscapeKey(item.Key) + "]"
					nested, err := inspectNodeTasks([]Task{child}, local, prefix, managedNode, inherited, derived)
					if err != nil {
						return nil, fmt.Errorf("iteration %s: %w", prefix+child.ID, err)
					}
					out = append(out, nested...)
				}
				continue
			}
		}
		action, err := t.Action()
		if err != nil {
			return nil, t.Sources.Error("", err)
		}
		view := ExplainedTask{Path: path, Node: managedNode, Action: action.Name, Status: inherited, Source: t.Sources.At(""), Timing: timingView(t)}
		if _, core := modules.LookUpCoreModule(t.Module); core {
			view.ModuleKind = modules.KindCore
		}
		if inherited == "ready" {
			// A path on the node is known only when the node task is reached.
			ok, err := scope.PredicateOnNode(t.When, func(path string) (bool, error) {
				return false, &value.UnavailableError{Path: path, Reason: "resolved during execution"}
			})
			if err != nil && !unknown(err) {
				return nil, argumentError(t, "when", t.When, scope, err)
			}
			if unknown(err) {
				view.Status = "conditional"
			} else if !ok {
				view.Status = "skipped"
			}
		}
		if t.When != nil {
			view.Arguments = append(view.Arguments, Argument{Field: "when", Reference: t.When, Status: "condition", Source: t.Sources.At("when"), Secrets: referenceSecrets(scope, t.When)})
		}
		if name, reads := moduleReads(t); len(reads.Conditions) > 0 {
			for _, field := range sortedKeys(reads.Conditions) {
				condition := reads.Conditions[field]
				view.Arguments = append(view.Arguments, Argument{Field: name + "." + field, Reference: condition, Status: "condition", Source: t.Sources.At(name + "." + field), Secrets: referenceSecrets(scope, condition)})
			}
		}
		if t.Loop != nil {
			view.Arguments = append(view.Arguments, Argument{Field: "loop", Reference: t.Loop, Status: view.Status, Source: t.Sources.At("loop")})
		}
		// A task that may run needs the file its variable source field names.
		if view.Status != "skipped" {
			if err := registerSources(t, scope, managedNode); err != nil {
				return nil, err
			}
		}
		active := view.Status == "ready"
		if active {
			args, err := InspectArguments(t, scope, true)
			if err != nil {
				return nil, err
			}
			if err := checkModuleWithWholeValues(t, scope); err != nil {
				return nil, err
			}
			byField := map[string]Argument{}
			for _, a := range authoredArguments(t) {
				byField[a.Field] = a
			}
			for _, a := range args {
				byField[a.Field] = a
			}
			for _, field := range sortedKeys(byField) {
				view.Arguments = append(view.Arguments, byField[field])
			}
			if err := inspectPolicies(t, scope); err != nil {
				return nil, err
			}
			if derived {
				view.RenderedTemplates = renderTemplates(t, scope)
				if view.ExpandedInventoryGroupRules, err = expandInventoryGroupRules(t, scope); err != nil {
					return nil, err
				}
			}
		} else {
			for _, a := range taskArguments(t) {
				view.Arguments = append(view.Arguments, Argument{Field: a.field, Reference: a.raw, Status: view.Status, Source: t.Sources.At(a.field), Secrets: referenceSecrets(scope, a.raw)})
			}
		}
		child := scope
		if t.Role != "" {
			// A role call that may run passes on the inputs known before
			// connecting, whether or not its when needs the node, so the
			// source paths of its node tasks resolve. The engine checks
			// inputs only after the when, so an input that fails here fails
			// the inspection only of an active call.
			child, err = ResolveRoleInputs(t, scope, true)
			if err != nil && active {
				return nil, err
			}
			if err != nil {
				child = RoleScope(t, scope)
			}
			child.Mark("/node_tasks", "resolved during execution")
		}
		if t.Role != "" && active {
			for _, key := range sortedKeys(t.RoleInputs) {
				raw, provided, _ := roleInput(t, key)
				source := t.Sources.At("inputs." + key)
				if !provided {
					source = t.RoleInputs[key].Sources.At("default")
				}
				arg := Argument{Field: "inputs." + key, Source: source, Reference: raw, Status: "resolved", Origins: []Location{ScopeOrigins(child).At(".inputs." + key)}, Secrets: referenceSecrets(scope, raw)}
				v, err := child.Get(".inputs." + key)
				if err != nil {
					arg.Status = "runtime"
				} else {
					arg.Value = value.Display(v)
				}
				if t.RoleInputs[key].Secret {
					arg.Status = "secret"
					arg.Value = nil
				}
				view.Arguments = append(view.Arguments, arg)
			}
		}
		var fixed map[string]Argument
		if active && t.TaskBlock != nil {
			if fixed, err = fixedInputs(t, scope, path+"/"); err != nil {
				return nil, err
			}
		}
		out = append(out, view)
		// As in the engine, a role's tasks use its own scope while task block
		// stages share the task's scope, so a stage's assignments reach later
		// tasks. on_failure runs only after a failure; cleanup always runs with
		// its task block, including dryrun, so it shares the task block's status.
		for _, list := range t.Children() {
			local, prefix, status := scope, path+"/"+list.Phase+"/", view.Status
			switch list.Phase {
			case "":
				local, prefix = child, path+"/"
			case "on_failure", ChecksPhase:
				if status == "ready" {
					status = "conditional"
				}
			}
			nested, err := inspectNodeTasks(list.NodeTasks, local, prefix, managedNode, status, derived)
			if err != nil {
				return nil, err
			}
			if err := checkFixed(nested, fixed, TaskName(t, 0)); err != nil {
				return nil, err
			}
			out = append(out, nested...)
		}
		if t.ID != "" {
			SetOrigin(scope, ".node_tasks."+t.ID, t.Sources.At(""))
		}
		if t.SetVars != nil && view.Status != "skipped" {
			// Every value resolves before any is assigned. A value known only
			// during execution stays unavailable; its origin is still this task.
			values, later := map[string]any{}, []string{}
			for _, key := range sortedKeys(t.SetVars) {
				if !active {
					values[key], later = nil, append(later, key)
					continue
				}
				v, err := scope.Evaluate(t.SetVars[key])
				if unknown(err) {
					values[key], later = nil, append(later, key)
					continue
				}
				if err != nil {
					return nil, argumentError(t, "set_vars."+key, t.SetVars[key], scope, err)
				}
				values[key] = v
			}
			AssignVars(scope, t, values)
			for _, key := range later {
				scope.Mark(value.Pointer("vars", key), "assignment resolved during execution")
			}
		}
	}
	return out, nil
}

// TrackedInputFollower returns the shell command with
// only_when_tracked_inputs_changed, which keeps a command record, or the
// service that a task is. In a node task block's activate phase, either one
// acts on the block's tracked inputs. Both are nil for any other task.
func TrackedInputFollower(t Task) (command *modules.Shell, service *modules.Service) {
	if command := InputsOf[modules.Shell](t); command != nil && command.OnlyWhenTrackedInputsChanged {
		return command, nil
	}
	return nil, InputsOf[modules.Service](t)
}

// TrackingTaskBlock reports whether a task block's activate phase has
// something that follows the block's tracked inputs: a service, or a command
// with only_when_tracked_inputs_changed. Only then do its stages fix tracked
// node tasks when it starts.
func TrackingTaskBlock(t Task) bool {
	if t.TaskBlock == nil {
		return false
	}
	return slices.ContainsFunc(t.TaskBlock.Activate, func(a Task) bool { return Tracked(a, "activate") })
}

// Tracked reports whether a task in a tracking task block's phase is fixed
// when the task block starts. In changes, that is a module of the project's
// own, by the fingerprint it reports, and a core module whose inputs list
// tracked inputs and do not opt out (modules.TrackedInput says how). In
// activate, it is a task that follows the tracked inputs.
func Tracked(t Task, stage string) bool {
	switch stage {
	case "changes":
		if t.CallsModuleThatIsNotCore() {
			return true
		}
		_, inputs := t.ModuleCall()
		if _, lists := inputs.(interface{ TrackedInputs() []modules.TrackedInput }); !lists {
			return false
		}
		optOut, ok := inputs.(interface{ TrackedInChanges() bool })
		return !ok || optOut.TrackedInChanges()
	case "activate":
		command, service := TrackedInputFollower(t)
		return command != nil || service != nil
	}
	return false
}

// EntryTask is one task a service task block fixes when it starts: its phase,
// its position there, and a loop's items in the entry scope.
type EntryTask struct {
	Phase string
	Index int
	Task  Task
	Items []value.Iteration
}

// Item is the task for loop item i, bound as the engine runs it.
func (e EntryTask) Item(i int) Task {
	child := e.Task
	child.Loop, child.Prepared = nil, nil
	child.Key = e.Items[i].Key
	child.Bindings = map[string]any{e.Task.As: e.Items[i].Value}
	return child
}

// EntryNodeTasks lists, in written order, the node tasks a service task block fixes in
// the scope it was entered with, before any stage runs. The engine resolves
// exactly these once and runs them with those values; validation checks exactly
// these against the written-order view. Other node tasks, including their loops,
// evaluate when they run. An task block without a service fixes nothing. With
// partial, a loop known only on the node is listed without items.
func EntryNodeTasks(op Task, entry value.Scope, partial bool) ([]EntryTask, error) {
	if !TrackingTaskBlock(op) {
		return nil, nil
	}
	var out []EntryTask
	for _, list := range []TaskList{{"changes", op.TaskBlock.Changes}, {"activate", op.TaskBlock.Activate}} {
		for i, t := range list.NodeTasks {
			if !Tracked(t, list.Phase) {
				continue
			}
			e := EntryTask{Phase: list.Phase, Index: i, Task: t}
			if t.Loop != nil {
				items, err := entry.Iterations(t.Loop, t.As, t.Key)
				if err != nil && !(partial && unknown(err)) {
					return nil, argumentError(t, "loop", t.Loop, entry, err)
				}
				if err == nil {
					e.Items = items
				}
			}
			out = append(out, e)
		}
	}
	return out, nil
}

// TrackedInputs lists what a resolved task adds to its task block's tracked
// inputs: files, links, directory deletions, unit files, packages, and
// checkouts, and installations, as its module says.
func TrackedInputs(t Task) []modules.TrackedInput {
	if t.CallsModuleThatIsNotCore() {
		return []modules.TrackedInput{{Kind: "module", Path: ModuleFingerprintKey(t)}}
	}
	_, inputs := t.ModuleCall()
	if tracked, ok := inputs.(interface{ TrackedInputs() []modules.TrackedInput }); ok {
		return tracked.TrackedInputs()
	}
	return nil
}

// TrackedPaths holds one service task block's tracked file, link, and unit
// paths, for TrackPath.
type TrackedPaths map[string]*trackedPath

type trackedPath struct {
	// mayRun: a node task that writes the path may run, because its when was
	// not known to be false when the task block started.
	mayRun       bool
	editsInPlace bool
	// owner describes the node task that first tracked the path, for the
	// message about a duplicate path.
	owner string
}

// TrackPath adds the path of a tracked file, link, or unit to one service
// task block's tracked paths and reports whether it is new. Several line and
// edit_file node tasks may edit one file, which counts once; any other repeat
// is an error, since a file, link, or unit owns the whole path. A node task
// whose when is false as the task block starts (mayRun false) still tracks
// its path but conflicts with nothing, so tasks for different nodes, such as
// one download per processor architecture, may write the same path.
func TrackPath(paths TrackedPaths, in modules.TrackedInput, mayRun bool, owner string) (bool, error) {
	prior, seen := paths[in.Path]
	switch {
	case !seen:
		paths[in.Path] = &trackedPath{mayRun: mayRun, editsInPlace: in.EditsInPlace, owner: owner}
		return true, nil
	case !mayRun:
		return false, nil
	case !prior.mayRun:
		prior.mayRun, prior.editsInPlace, prior.owner = true, in.EditsInPlace, owner
		return false, nil
	case prior.editsInPlace && in.EditsInPlace:
		return false, nil
	}
	return false, fmt.Errorf("duplicate service configuration path %s: two node tasks that may run on the node write to the same path: %s and %s", in.Path, prior.owner, owner)
}

// TrackedOwner describes a node task for the message about a duplicate path:
// its name, where the playbook or role defines it, and the project file its
// content comes from, so the reader sees which node task to remove.
func TrackedOwner(name string, t Task) string {
	owner := "node task " + name
	if at := t.Sources.At(""); at.File != "" {
		owner += fmt.Sprintf(" (%s:%d)", at.File, at.Line)
	}
	if _, inputs := t.ModuleCall(); inputs != nil {
		if v := reflect.Indirect(reflect.ValueOf(inputs)); v.Kind() == reflect.Struct {
			for _, field := range []string{"Source", "Template"} {
				if f := v.FieldByName(field); f.IsValid() && f.Kind() == reflect.String && f.String() != "" {
					owner += ", content from " + f.String()
					break
				}
			}
		}
	}
	return owner
}

// fixedInputs checks what a service task block fixes in the scope it was
// entered with (EntryNodeTasks), each tracked argument by task path and field;
// checkFixed then compares it with the written-order view. Two tracked paths
// that resolve alike are rejected when their values, and the when of both
// node tasks, are known locally; the node checks them again.
func fixedInputs(t Task, entry value.Scope, prefix string) (map[string]Argument, error) {
	fixed := map[string]Argument{}
	early := func(err error) error {
		return fmt.Errorf("%w; node task block %s resolves the arguments of the block's node tasks before any block phase runs", err, t.TaskBlock.ID)
	}
	nodeTasks, err := EntryNodeTasks(t, entry, true)
	if err != nil {
		return fixed, early(err)
	}
	paths := TrackedPaths{}
	for _, e := range nodeTasks {
		name := prefix + e.Phase + "/" + TaskName(e.Task, e.Index)
		type bound struct {
			path  string
			task  Task
			scope value.Scope
		}
		list := []bound{{name, e.Task, entry}}
		if e.Task.Loop != nil {
			list = nil
			for n, item := range e.Items {
				path := name + "[" + value.EscapeKey(item.Key) + "]"
				list = append(list, bound{path, e.Item(n), bindItem(entry, e.Task.As, item)})
			}
		}
		for _, b := range list {
			resolved, err := inspectArguments(b.task, publicArguments(b.task), b.scope, true)
			if err != nil && knownToBeFalse(b.task, b.scope) {
				// The node leaves it out of what the block fixes too.
				continue
			}
			if err != nil {
				return fixed, early(err)
			}
			for _, a := range resolved {
				fixed[b.path+"\x00"+a.Field] = a
			}
			// The task block's own node tasks prepare their source files
			// for this node only after this check, which needs their
			// tracked paths and none of their files.
			a, err := ResolveArguments(withoutSourceVariants(b.task), b.scope)
			if unknown(err) {
				continue
			}
			if err != nil {
				return fixed, early(err)
			}
			for _, in := range TrackedInputs(a) {
				if !in.OwnsPath() {
					continue
				}
				if _, err := TrackPath(paths, in, knownToRun(b.task, b.scope), TrackedOwner(b.path, b.task)); err != nil {
					return fixed, b.task.Sources.Error("", fmt.Errorf("%w; both are in task_block %s, so remove one of them", err, t.TaskBlock.ID))
				}
			}
		}
	}
	return fixed, nil
}

// knownToBeFalse reports whether a tracked node task has a when known locally
// to be false, as the node finds it when its node task block starts.
func knownToBeFalse(t Task, entry value.Scope) bool {
	ok, err := entry.PredicateOnNode(t.When, func(path string) (bool, error) {
		return false, &value.UnavailableError{Path: path, Reason: "resolved during execution"}
	})
	return err == nil && !ok
}

// knownToRun reports whether a tracked node task has no when, or one known
// locally to be true. A when that reads the node's facts is known only on the
// node, where the engine decides it as the task block starts.
func knownToRun(t Task, entry value.Scope) bool {
	ok, err := entry.PredicateOnNode(t.When, func(path string) (bool, error) {
		return false, &value.UnavailableError{Path: path, Reason: "resolved during execution"}
	})
	return err == nil && ok
}

// publicArguments are a task's identifiers, the arguments a service task block
// fixes for its tracked node tasks.
func publicArguments(t Task) []taskArgument {
	var out []taskArgument
	for _, a := range taskArguments(t) {
		if a.target != nil {
			out = append(out, a)
		}
	}
	return out
}

// A tracked argument that reads differently in written order depends on an
// assignment inside its task block; execution.md rejects that dependency.
func checkFixed(views []ExplainedTask, fixed map[string]Argument, taskBlock string) error {
	for _, view := range views {
		for _, a := range view.Arguments {
			want, ok := fixed[view.Path+"\x00"+a.Field]
			if ok && view.Status == "ready" && (a.Status != want.Status || !reflect.DeepEqual(a.Value, want.Value)) {
				err := fmt.Errorf("%s: %s is fixed when node task block %s starts, so the field cannot depend on a value assigned inside the node task block", a.Source, a.Field, taskBlock)
				for _, at := range a.Origins {
					err = fmt.Errorf("%w; value supplied at %s", err, at)
				}
				return err
			}
		}
	}
	return nil
}
func inspectPolicies(t Task, s value.Scope) error {
	local := s.Local()
	local.Mark("/this_node_task", "command result resolved during execution")
	local.Mark("/new_file", "new_file.path resolved on the node")
	checks := map[string]any{}
	name, reads := moduleReads(t)
	for field, condition := range reads.ResultConditions {
		checks[name+"."+field] = condition
	}
	for field, condition := range reads.Conditions {
		checks[name+"."+field] = condition
	}
	if t.Retry != nil {
		checks["retry.until"] = t.Retry.Until
		checks["retry.retry_when"] = t.Retry.RetryWhen
	}
	for _, field := range sortedKeys(checks) {
		if _, err := local.Predicate(checks[field]); err != nil && !unknown(err) {
			return argumentError(t, field, checks[field], s, err)
		}
	}
	// New file checks are shell tasks with new_file.path unavailable locally.
	for field, command := range reads.Commands {
		if field == "" {
			continue // the module's own command: its arguments are the task's
		}
		check := Task{Module: "shell", ModuleInputs: command, Sources: t.Sources.Sub(name + "." + strings.TrimSuffix(field, ".shell"))}
		if _, err := InspectArguments(check, local, true); err != nil {
			return err
		}
	}
	return nil
}
func timingView(t Task) []Argument {
	if t.Role != "" || t.TaskBlock != nil || t.CoreModule().WaitsWithItsOwnLimit {
		return nil
	}
	fields := []Argument{
		{Field: "time_limit", Value: t.TimeLimit},
		{Field: "time_limit_per_try", Value: t.TimeLimitPerTry},
		{Field: "retry.enabled", Value: t.Retry != nil && !t.Retry.Disabled},
	}
	if t.Retry != nil && !t.Retry.Disabled {
		fields = append(fields,
			Argument{Field: "retry.max_tries", Value: t.Retry.MaxTries},
			Argument{Field: "retry.pause_between_tries", Value: t.Retry.PauseBetweenTries})
		for _, condition := range []struct {
			field string
			raw   any
		}{{"retry.until", t.Retry.Until}, {"retry.retry_when", t.Retry.RetryWhen}} {
			if condition.raw != nil {
				fields = append(fields, Argument{Field: condition.field, Reference: condition.raw, Status: "condition"})
			}
		}
	}
	for i := range fields {
		if fields[i].Status == "" {
			fields[i].Status = "resolved"
		}
		fields[i].Source = t.Sources.At(fields[i].Field)
	}
	return fields
}

// ModuleFingerprintKey names the fingerprint of a module that is not core
// within its node task block: the module's name. Several tasks of one module
// in a node task block, and the items of a loop, add their fingerprints up.
// In a record the node task execution engine puts the project, the fleet
// stage, and the block before the name.
func ModuleFingerprintKey(t Task) string {
	return t.Module
}
