package config

import (
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"mmx/internal/modules"
	"mmx/internal/value"
)

// FailureAllowance is failed_nodes_allowed: a count of failed nodes ("5") or
// a whole-number percentage of the fleet stage's selected nodes ("2%"). YAML writes
// a count as an integer; decoding keeps its text.
type FailureAllowance string

var allowancePattern = regexp.MustCompile(`^([0-9]{1,9})(%?)$`)

// Nodes is how many of a fleet stage's selected nodes may fail before its
// on_node_failure policy stops it; a percentage rounds down.
func (a FailureAllowance) Nodes(selected int) (int, error) {
	if a == "" {
		return 0, nil
	}
	m := allowancePattern.FindStringSubmatch(string(a))
	if m == nil {
		return 0, fmt.Errorf("failed_nodes_allowed must be a count or a whole-number percentage such as \"2%%\"")
	}
	n, _ := strconv.Atoi(m[1])
	if m[2] == "" {
		return n, nil
	}
	if n > 100 {
		return 0, fmt.Errorf("failed_nodes_allowed percentage must be at most 100%%")
	}
	return selected * n / 100, nil
}

// NodesAtOnce is max_nodes_at_once on a fleet stage or a for_each_node loop: a
// count ("5") or a whole-number percentage of the selected nodes ("34%"). YAML
// writes a count as an integer; decoding keeps its text.
type NodesAtOnce string

// Nodes is how many of selected nodes may work at once; unwritten is
// unset's count. A percentage rounds down and is never less than one node.
func (n NodesAtOnce) Nodes(selected, unset int) (int, error) {
	if n == "" {
		return unset, nil
	}
	count := 0
	m := allowancePattern.FindStringSubmatch(string(n))
	if m != nil {
		count, _ = strconv.Atoi(m[1])
	}
	if m == nil || count < 1 || m[2] != "" && count > 100 {
		return 0, fmt.Errorf("max_nodes_at_once must be a positive count or a whole-number percentage from 1%% to 100%%, such as \"34%%\"")
	}
	if m[2] == "" {
		return count, nil
	}
	return max(1, selected*count/100), nil
}

// checkOutputs returns an error for the outputs.<name> field of the fleet stage or
// role file declaring them.
func checkOutputs(outputs map[string]any, d declarations) error {
	for _, k := range sortedKeys(outputs) {
		if !modules.Name.MatchString(k) {
			return modules.Invalid("outputs."+k, fmt.Sprintf("invalid output name %q", k))
		}
		if err := checkDeclared(outputs[k], d); err != nil {
			return &modules.FieldError{Field: "outputs." + k, Err: fmt.Errorf("output %s: %w", k, err)}
		}
	}
	return nil
}

// checkDeclared checks an output or set_vars value as value.Scope.Evaluate
// computes it: a predicate form, else ordinary data.
func checkDeclared(v any, d declarations) error {
	if value.IsPredicate(v) {
		return checkPredicate(v, d)
	}
	return checkValue(v, d)
}

// checkLoop returns an error for the loop, as, or key field it concerns.
func checkLoop(loop any, as string, key any, d declarations, requireKey bool) error {
	switch collection := loop.(type) {
	case []any, map[string]any:
	case string:
		if simpleReference.FindString(collection) != collection || collection == "" {
			return modules.Invalid("loop", "loop requires a list, a mapping, or a whole-value reference that resolves to a list or mapping")
		}
	default:
		return modules.Invalid("loop", "loop requires a list, a mapping, or a whole-value reference that resolves to a list or mapping")
	}
	if err := checkValue(loop, d); err != nil {
		return &modules.FieldError{Field: "loop", Err: err}
	}
	if err := checkLoopVariable(as, d); err != nil {
		return err
	}
	if requireKey && key == nil {
		return modules.Invalid("key", "fleet stage loops and node task block loops require key to identify each iteration")
	}
	local := copyDeclarations(d)
	local[as] = nil
	if err := checkValue(key, local); err != nil {
		return &modules.FieldError{Field: "key", Err: err}
	}
	return nil
}

// checkLoopVariable returns an error for a loop's as field: a name that is not
// a reserved scope name and shadows no enclosing name.
func checkLoopVariable(as string, d declarations) error {
	if !modules.Name.MatchString(as) || contains([]string{"vars", "inputs", "node", "inventory", "this_node_task", "node_tasks", "outputs", "fleet_stages", "new_file", "secrets"}, as) {
		return modules.Invalid("as", "as must be a name of letters, digits, underscores, and hyphens that starts with a letter or underscore, other than vars, inputs, node, inventory, this_node_task, node_tasks, outputs, fleet_stages, new_file, and secrets")
	}
	if _, exists := d[as]; exists {
		return modules.Invalid("as", fmt.Sprintf("loop variable %s conflicts with a name already declared outside the loop; choose a different as name", as))
	}
	return nil
}

// checkRollout checks and completes the rollout settings of a fleet stage or
// a for_each_node loop; kind names which one in messages.
func checkRollout(s *FleetStage, kind string) error {
	if _, err := s.MaxNodesAtOnce.Nodes(0, 1); err != nil {
		return s.Sources.Error("max_nodes_at_once", fmt.Errorf("%s %s: %w", kind, s.ID, err))
	}
	if s.OnNodeFailure == "" {
		s.OnNodeFailure = "continue"
	}
	if !contains([]string{"continue", "stop_fleet_stage", "stop_run"}, s.OnNodeFailure) {
		return s.Sources.Error("on_node_failure", fmt.Errorf("%s %s: on_node_failure must be continue, stop_fleet_stage, or stop_run", kind, s.ID))
	}
	if _, err := s.FailedNodesAllowed.Nodes(0); err != nil {
		return s.Sources.Error("failed_nodes_allowed", fmt.Errorf("%s %s: %w", kind, s.ID, err))
	}
	if s.FailedNodesAllowed != "" && s.OnNodeFailure == "continue" {
		return s.Sources.Error("failed_nodes_allowed", fmt.Errorf("%s %s: failed_nodes_allowed requires on_node_failure: stop_fleet_stage or on_node_failure: stop_run; on_node_failure: continue does not stop after node failures", kind, s.ID))
	}
	return nil
}

// ReadsNodeFacts reports whether a fleet stage's or for_each_node loop's when
// reads the node's facts, any .node field but its name. The controller
// process then asks that node for its facts just before deciding.
func ReadsNodeFacts(when any) bool {
	for _, ref := range References(when) {
		//nolint:nilaway // Successful Path parsing appends a first selector before returning.
		if parts, err := value.Path(ref); err == nil && parts[0].Name == "node" && (len(parts) < 2 || parts[1].Name != "name") {
			return true
		}
	}
	return false
}

// prepareForEachNode checks a for_each_node loop: its node selection, its
// variable, its rollout settings, which count the loop's nodes, and its
// enclosed fleet stages, which take no failure policy of their own.
func prepareForEachNode(s *FleetStage, p Project, inv Inventory, base string, d declarations) error {
	if s.Loop != nil || s.Key != nil || s.Nodes != nil || len(s.NodeTasks) > 0 || len(s.Requires.SameNode)+len(s.Requires.AllNodes) > 0 {
		return s.Sources.Error("for_each_node", fmt.Errorf("for_each_node loop %s does not accept loop, key, nodes, node_tasks, or requires; each selected node's id identifies an iteration, and the enclosed fleet stages select the nodes to run on", s.ID))
	}
	if len(s.FleetStages) == 0 {
		return s.Sources.Error("for_each_node", fmt.Errorf("for_each_node loop %s needs fleet_stages to run for each node", s.ID))
	}
	if err := checkValue(s.ForEachNode, d); err != nil {
		return s.Sources.Error("for_each_node", explainUndeclaredVariable(err, p, inv, "", variablesOfNoSingleNode))
	}
	if err := checkValue(s.ExceptNodes, d); err != nil {
		return s.Sources.Error("except_nodes", explainUndeclaredVariable(err, p, inv, "", variablesOfNoSingleNode))
	}
	var selected []string
	static := !modules.Dynamic(s.ForEachNode) && !modules.Dynamic(s.ExceptNodes)
	if static {
		var err error
		if selected, err = SelectNodes(inv, value.Scope{}, *s); err != nil {
			return fmt.Errorf("for_each_node loop %s: %w", s.ID, err)
		}
	}
	if s.As == "" {
		return s.Sources.Error("as", fmt.Errorf("for_each_node loop %s needs as: the name of the variable that holds the current node's ID", s.ID))
	}
	if err := checkLoopVariable(s.As, d); err != nil {
		return s.Sources.Error("", err)
	}
	if err := checkRollout(s, "for_each_node loop"); err != nil {
		return err
	}
	if err := WalkFleetStages(s.FleetStages, func(path string, child FleetStage) error {
		field := ""
		switch {
		case child.OnNodeFailure != "":
			field = "on_node_failure"
		case child.FailedNodesAllowed != "":
			field = "failed_nodes_allowed"
		default:
			return nil
		}
		return child.Sources.Error(field, fmt.Errorf("fleet stage %s is inside for_each_node loop %s, which counts failures by the loop's nodes; write %s on the loop", path, s.ID, field))
	}); err != nil {
		return err
	}
	local := copyDeclarations(d)
	declareLoopItems(local, s.As, nil) // node names from the inventory
	// The loop's when is checked for each loop node and reads that node as
	// .node: its name, and its facts, which the node reports just before.
	condition := copyDeclarations(local)
	condition["node"] = nil
	if err := checkPredicate(s.When, condition); err != nil {
		return s.Sources.Error("when", explainUndeclaredVariable(err, p, inv, "", variablesOfNoSingleNode))
	}
	if err := prepareFleetStages(s.FleetStages, p, inv, base, local); err != nil {
		return err
	}
	// An enclosed fleet stage selects its nodes from the loop's node, such
	// as nodes: "{{ .server }}", so the load above did not know them. For
	// each loop node, the nodes it selects then are known: their durations
	// are checked as a fleet stage's outside a loop. A selection that reads
	// anything else waits for the run.
	for _, loopNode := range selected {
		if err := WalkFleetStages(s.FleetStages, func(_ string, child FleetStage) error {
			if len(child.NodeTasks) == 0 {
				return nil
			}
			nodes, err := SelectNodes(inv, value.Scope{s.As: loopNode}, child)
			if err != nil {
				return nil
			}
			for _, managedNode := range nodes {
				if err := checkDurationsOnNode(child, p, inv, managedNode); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			return err
		}
	}
	// Child names become visible only for this iteration's declared exports.
	for _, child := range s.FleetStages {
		local["outputs"][child.ID] = true //nolint:nilaway // Fleet-stage declarations allocate outputs and copies preserve that reserved namespace.
		local["fleet_stages"][child.ID] = true
		local["outputs."+child.ID] = names(child.Outputs)
	}
	if err := checkOutputs(s.Outputs, local); err != nil {
		return s.Sources.Error("", explainUndeclaredVariable(err, p, inv, "", variablesOfNoSingleNode))
	}
	// A known selection, also an empty one, names the nodes whose outputs
	// later fleet stages may read.
	if static {
		d["nodes."+s.ID] = map[string]bool{}
		for _, h := range selected {
			d["nodes."+s.ID][h] = true
		}
	}
	return nil
}

// checkDurationsOnNode checks, as the project loads and so before any node
// job, the node's value of each var that a duration of the fleet stage
// reads (ResolveDurations), naming the setting and where the var is set. A
// var that only the command line sets waits for the run, which requires it.
func checkDurationsOnNode(s FleetStage, p Project, inv Inventory, managedNode string) error {
	scope := localScope(p, inv, managedNode, nil)
	SetOrigins(scope, VariableOrigins(p, inv, managedNode))
	if _, err := ResolveDurations(s.NodeTasks, scope); err != nil && !unknown(err) {
		return fmt.Errorf("fleet stage %s, node %s: %w", s.ID, managedNode, err)
	}
	return nil
}

// variableNames lists the variables a node's node tasks start with: every value
// layer's names, and each declared command-line variable, even one validate
// has no value for. An empty node gives the names every node shares.
func variableNames(p Project, inv Inventory, managedNode string) map[string]bool {
	vars := names(p.CommandLineVars)
	for _, layer := range varLayers(p, inv, managedNode) {
		for k := range layer.vars {
			vars[k] = true
		}
	}
	return vars
}
func prepareFleetStages(fleetStages []FleetStage, p Project, inv Inventory, base string, outer declarations) error {
	seen := map[string]FleetStage{}
	d := copyDeclarations(outer)
	d["secrets"] = nil
	d["vars"] = variableNames(p, inv, "")
	d["inventory"] = map[string]bool{"nodes": true, "groups": true}
	for i := range fleetStages {
		s := &fleetStages[i]
		if !modules.Name.MatchString(s.ID) {
			return s.Sources.Error("id", fmt.Errorf("invalid fleet stage id %q", s.ID))
		}
		if _, ok := seen[s.ID]; ok {
			return s.Sources.Error("id", fmt.Errorf("duplicate fleet stage id %s", s.ID))
		}
		if d["fleet_stages"][s.ID] {
			return s.Sources.Error("id", fmt.Errorf("fleet stage id %s conflicts with an enclosing fleet stage id", s.ID))
		}
		if s.ForEachNode != nil {
			if err := prepareForEachNode(s, p, inv, base, d); err != nil {
				return err
			}
		} else if s.Loop != nil {
			if len(s.FleetStages) == 0 || s.Nodes != nil || s.ExceptNodes != nil || s.SkipWhenNoNodesAreSelected || len(s.NodeTasks) > 0 || s.MaxNodesAtOnce != "" || s.WaitForAllInBatch || s.OnNodeFailure != "" || s.FailedNodesAllowed != "" || len(s.Requires.SameNode)+len(s.Requires.AllNodes) > 0 {
				return s.Sources.Error("loop", fmt.Errorf("fleet stage loop %s contains fleet_stages; selection and failure policies belong to those fleet_stages", s.ID))
			}
			if err := checkLoop(s.Loop, s.As, s.Key, d, true); err != nil {
				return s.Sources.Error("", explainUndeclaredVariable(err, p, inv, "", variablesOfNoSingleNode))
			}
			local := copyDeclarations(d)
			declareLoopItems(local, s.As, s.Loop)
			if err := checkPredicate(s.When, local); err != nil {
				return s.Sources.Error("when", explainUndeclaredVariable(err, p, inv, "", variablesOfNoSingleNode))
			}
			if err := prepareFleetStages(s.FleetStages, p, inv, base, local); err != nil {
				return err
			}
			// Child names become visible only for this iteration's declared exports.
			for _, child := range s.FleetStages {
				local["outputs"][child.ID] = true //nolint:nilaway // Fleet-stage declarations allocate outputs and copies preserve that reserved namespace.
				local["fleet_stages"][child.ID] = true
				local["outputs."+child.ID] = names(child.Outputs)
			}
			if err := checkOutputs(s.Outputs, local); err != nil {
				return s.Sources.Error("", explainUndeclaredVariable(err, p, inv, "", variablesOfNoSingleNode))
			}
		} else {
			if s.As != "" || s.Key != nil || len(s.FleetStages) > 0 {
				return s.Sources.Error("", fmt.Errorf("fleet stage %s: as, key, and fleet_stages require loop or for_each_node", s.ID))
			}
			for _, req := range append(append([]string{}, s.Requires.SameNode...), s.Requires.AllNodes...) {
				if !d["fleet_stages"][req] {
					return s.Sources.Error("requires", fmt.Errorf("fleet stage %s: requirement %s must name an earlier fleet stage", s.ID, req))
				}
			}
			if err := checkRollout(s, "fleet stage"); err != nil {
				return err
			}
			if err := checkValue(s.Nodes, d); err != nil {
				return s.Sources.Error("nodes", explainUndeclaredVariable(err, p, inv, "", variablesOfNoSingleNode))
			}
			if err := checkValue(s.ExceptNodes, d); err != nil {
				return s.Sources.Error("except_nodes", explainUndeclaredVariable(err, p, inv, "", variablesOfNoSingleNode))
			}
			var selected []string
			if staticSelection(*s) {
				var err error
				selected, err = SelectNodes(inv, value.Scope{}, *s)
				if err != nil {
					return fmt.Errorf("fleet stage %s: %w", s.ID, err)
				}
			}
			for _, req := range s.Requires.SameNode {
				before, ok := seen[req]
				prerequisite := before.Nodes
				if before.ForEachNode != nil {
					prerequisite = before.ForEachNode // a for_each_node loop's iterations are its nodes
				}
				if !ok || before.Loop != nil || modules.Dynamic(prerequisite) || modules.Dynamic(before.ExceptNodes) || !staticSelection(*s) {
					continue
				}
				prev, _ := SelectNodes(inv, value.Scope{}, before)
				for _, h := range selected {
					if !contains(prev, h) {
						return s.Sources.Error("requires.same_node", fmt.Errorf("fleet stage %s: the required fleet stage %s in requires.same_node does not select node %s", s.ID, req, h))
					}
				}
			}
			if len(s.NodeTasks) == 0 && len(s.Outputs) == 0 {
				return s.Sources.Error("", fmt.Errorf("fleet stage %s requires node_tasks or outputs", s.ID))
			}
			if len(s.NodeTasks) > 0 {
				if err := prepare(s.NodeTasks, "", base, base, p.AllowNested, p.Defaults, nil, map[string]bool{}); err != nil {
					return fmt.Errorf("fleet stage %s: %w", s.ID, err)
				}
				if err := checkReboot(s.NodeTasks, true); err != nil {
					return fmt.Errorf("fleet stage %s: %w", s.ID, err)
				}
				if err := checkDurationReferences(s.NodeTasks, varsSetBySetVars(s.NodeTasks, map[string]bool{}), false); err != nil {
					return fmt.Errorf("fleet stage %s: %w", s.ID, err)
				}
			}
			validationNodes := selected
			if len(validationNodes) == 0 {
				validationNodes = []string{""}
			}
			for _, managedNode := range validationNodes {
				local := copyDeclarations(d)
				local["vars"] = variableNames(p, inv, managedNode)
				if managedNode == "" {
					for _, layer := range everyNodeLayer(inv) {
						for k := range layer.vars {
							local["vars"][k] = true
						}
					}
				}
				local[variablesSetLater] = varsSetBySetVars(s.NodeTasks, map[string]bool{})
				local["node"] = nil
				local["inputs"] = map[string]bool{}
				local["node_tasks"] = map[string]bool{}
				if err := checkPredicate(s.When, local); err != nil {
					return s.Sources.Error("when", explainUndeclaredVariable(err, p, inv, managedNode, variablesOfOneNodeBeforeSetVars))
				}
				if err := validateReferences(s.NodeTasks, local); err != nil {
					return fmt.Errorf("fleet stage %s, node %s: %w", s.ID, managedNode, explainUndeclaredVariable(err, p, inv, managedNode, variablesOfOneNode))
				}
				if managedNode != "" {
					if err := checkDurationsOnNode(*s, p, inv, managedNode); err != nil {
						return err
					}
				}
				if err := checkOutputs(s.Outputs, local); err != nil {
					return s.Sources.Error("", explainUndeclaredVariable(err, p, inv, managedNode, variablesOfOneNode))
				}
			}
			// A known selection, also an empty one, names the nodes whose
			// outputs later fleet stages may read.
			if staticSelection(*s) {
				nodes := map[string]bool{}
				for _, h := range selected {
					nodes[h] = true
				}
				d["nodes."+s.ID] = nodes
			}
		}
		seen[s.ID] = *s
		d["outputs"][s.ID] = true //nolint:nilaway // Fleet-stage declarations allocate outputs and copies preserve that reserved namespace.
		d["fleet_stages"][s.ID] = true
		d["outputs."+s.ID] = names(s.Outputs)
		if err := prepareControllerMachineFiles(s, p, inv, base, d); err != nil {
			return err
		}
	}
	return nil
}

// checkReboot allows a reboot only as the last task of a fleet stage's own list,
// where it ends the node fleet phase: not in a role, a task block, or a loop.
func checkReboot(nodeTasks []Task, fleetStageLevel bool) error {
	for i, t := range nodeTasks {
		if m := t.CoreModule(); m.EndsTheNodeJob && (!fleetStageLevel || i != len(nodeTasks)-1 || t.Loop != nil) {
			return t.Sources.Error(m.Name, fmt.Errorf("%s must be the last node task in a fleet stage's node_tasks list, outside roles, node task blocks, and loops", m.Name))
		}
		for _, list := range t.Children() {
			if err := checkReboot(list.NodeTasks, false); err != nil {
				return err
			}
		}
	}
	return nil
}

// WalkFleetStages visits every fleet stage before its nested fleet stages. The path names the fleet stage
// through its enclosing loops without iteration keys, such as rollout/update.
func WalkFleetStages(fleetStages []FleetStage, visit func(path string, s FleetStage) error) error {
	var walk func([]FleetStage, string) error
	walk = func(fleetStages []FleetStage, prefix string) error {
		for _, s := range fleetStages {
			if err := visit(prefix+s.ID, s); err != nil {
				return err
			}
			if err := walk(s.FleetStages, prefix+s.ID+"/"); err != nil {
				return err
			}
		}
		return nil
	}
	return walk(fleetStages, "")
}

// ValidStepPath accepts the concrete path of a node fleet stage inside nested loops.
func ValidFleetStagePath(path string) bool {
	parts := strings.Split(path, "/")
	for i, part := range parts {
		name, key, loop := strings.Cut(part, "[")
		if !modules.Name.MatchString(name) {
			return false
		}
		if loop {
			if i == len(parts)-1 || !strings.HasSuffix(key, "]") {
				return false
			}
			encoded := strings.TrimSuffix(key, "]")
			decoded, err := url.PathUnescape(encoded)
			if err != nil || decoded == "" || url.PathEscape(decoded) != encoded {
				return false
			}
		} else if i != len(parts)-1 {
			return false
		}
	}
	return true
}
