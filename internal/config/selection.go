package config

import (
	"fmt"
	"slices"
	"strings"

	"mmx/internal/modules"
	"mmx/internal/value"
)

// SelectNodes is the one resolution of a fleet stage's selection: the nodes
// that nodes, or a for_each_node loop's for_each_node, selects, minus the
// nodes its except_nodes selects (nil removes none). Both resolve in scope,
// from values the controller process knows before it connects. A selection
// that is empty is an error, so that a group emptied by mistake never makes
// its work vanish without a word; with skip_when_no_nodes_are_selected it is
// no nodes, and the fleet stage is not selected. Validation, inspection, the
// run, and the fleet stage walker all select through it, so each shows the
// selection the others use.
func SelectNodes(inv Inventory, scope value.Scope, s FleetStage) ([]string, error) {
	field, selector := "nodes", s.Nodes
	if s.ForEachNode != nil {
		field, selector = "for_each_node", s.ForEachNode
	}
	resolve := func(field string, raw any) ([]string, error) {
		requested, err := scope.Resolve(raw)
		var nodes []string
		switch {
		case err != nil:
		case field == "except_nodes" || s.SkipWhenNoNodesAreSelected:
			// An empty group or list in except_nodes removes nothing; in
			// nodes it selects no node of a fleet stage that may be skipped.
			nodes, err = selectMaybeNone(inv, requested)
		default:
			if nodes, err = selectMaybeNone(inv, requested); err == nil && len(nodes) == 0 {
				err = selectsNoNode("node selection is empty")
			}
		}
		if err != nil {
			return nil, SourceError(s.Sources, field, raw, scope, err)
		}
		return nodes, nil
	}
	nodes, err := resolve(field, selector)
	if err != nil || s.ExceptNodes == nil {
		return nodes, err
	}
	excluded, err := resolve("except_nodes", s.ExceptNodes)
	if err != nil {
		return nil, err
	}
	nodes = slices.DeleteFunc(nodes, func(node string) bool { return slices.Contains(excluded, node) })
	if len(nodes) == 0 && !s.SkipWhenNoNodesAreSelected {
		return nil, s.Sources.Error("except_nodes", selectsNoNode("except_nodes leaves no node of "+field))
	}
	return nodes, nil
}

// selectsNoNode is the error of a selection that is empty, which names the
// setting that leaves such a fleet stage out instead.
func selectsNoNode(problem string) error {
	return fmt.Errorf("%s; to leave the fleet stage out of the run when it selects no node, set skip_when_no_nodes_are_selected: true", problem)
}

// staticSelection reports whether a fleet stage's selection resolves without
// any value, so validation can select its nodes.
func staticSelection(s FleetStage) bool {
	return !modules.Dynamic(s.Nodes) && !modules.Dynamic(s.ExceptNodes)
}

// KeepsFleetStage reports whether --only-fleet-stages keeps the playbook's
// top-level fleet stage or fleet stage loop id. Without the option every
// fleet stage is kept.
func (p Project) KeepsFleetStage(id string) bool {
	return len(p.OnlyFleetStages) == 0 || slices.Contains(p.OnlyFleetStages, id)
}

// NotSelectedReason is why work that reads a fleet stage --only-fleet-stages
// left out is blocked.
func (p Project) NotSelectedReason(id string) string {
	return fmt.Sprintf("fleet stage %s is not selected: --only-fleet-stages %s leaves it out", id, strings.Join(p.OnlyFleetStages, ","))
}

// SecretNamesARunReads lists the named secrets that the fleet stages of a run
// with the inventory and limit read. It leaves out what the walker never
// reads, so that a team which leaves an optional group empty needs no
// placeholder secret for it: a fleet stage that --only-fleet-stages leaves
// out, and a fleet stage or for_each_node loop whose selection resolves
// before the run, from node and group names, vars, and the inventory, and
// selects no node through skip_when_no_nodes_are_selected or the limit,
// with the fleet stages inside such a loop, and a top-level fleet stage whose
// when is false before the run on every node it selects
// (whenIsFalseBeforeTheRun). Of such a fleet stage it keeps
// the save_on_controller_machine templates, which the walker still writes
// from the empty output collection. A selection that resolves only during
// the run, from outputs, outcomes, or a loop's item, may select nodes, so its
// fleet stage keeps all its secrets. A loop's enclosed fleet stages are
// checked once for all iterations; inside a for_each_node loop the limit does
// not narrow them, as in the walker. The inventory's connection secrets are
// read when a connection needs them and are not listed here.
func SecretNamesARunReads(p Project, inv Inventory, limit Limit) []string {
	var read []any
	var walk func(fleetStages []FleetStage, scope value.Scope, narrow Limit, topLevel bool)
	walk = func(fleetStages []FleetStage, scope value.Scope, narrow Limit, topLevel bool) {
		for _, s := range fleetStages {
			if topLevel && !p.KeepsFleetStage(s.ID) {
				continue
			}
			if s.Loop == nil {
				if nodes, err := SelectNodes(inv, scope, s); err == nil {
					if kept, _ := narrow.Narrow(nodes); len(kept) == 0 || (topLevel && s.ForEachNode == nil && whenIsFalseBeforeTheRun(p, inv, s, kept)) {
						for _, file := range s.SaveOnControllerMachine {
							if file.Output == "" {
								read = append(read, file)
							}
						}
						continue
					}
				}
			}
			if len(s.FleetStages) > 0 {
				enclosed, enclosedNarrow := scope.Local(), narrow
				enclosed.Mark(value.Pointer(s.As), "the loop's item is resolved during the run")
				if s.ForEachNode != nil {
					enclosedNarrow = nil
				}
				walk(s.FleetStages, enclosed, enclosedNarrow, false)
				s.FleetStages = nil
			}
			read = append(read, s)
		}
	}
	walk(p.FleetStages, localScope(p, inv, "", InventoryValues(inv)), limit, true)
	return SecretNames(read)
}

// whenIsFalseBeforeTheRun reports that a top-level fleet stage's when is
// false on every node it selects, from what the controller process knows
// before the run: each node's vars and name, as the walker evaluates it
// when the node would start. A when that reads anything else, such as a
// fact, a secret, or an earlier fleet stage's output, cannot be decided
// before the run, and the fleet stage keeps its secrets. Inside a fleet
// stage loop a when may read the loop's item, so enclosed fleet stages are
// not checked.
func whenIsFalseBeforeTheRun(p Project, inv Inventory, s FleetStage, nodes []string) bool {
	if s.When == nil {
		return false
	}
	for _, node := range nodes {
		if starts, err := localScope(p, inv, node, InventoryValues(inv)).Predicate(s.When); err != nil || starts {
			return false
		}
	}
	return true
}

// checkOnlyFleetStages rejects an --only-fleet-stages value that names no
// top-level fleet stage of the playbook, names a fleet stage enclosed in a
// loop, or keeps a fleet stage that requires, or reads the outputs of, one it
// leaves out. MMX never adds a fleet stage itself: the rejection prints the
// option value that also keeps every fleet stage the named ones need.
func checkOnlyFleetStages(p Project) error {
	if len(p.OnlyFleetStages) == 0 {
		return nil
	}
	var ids []string
	enclosing := map[string]string{} // an enclosed fleet stage's ID → its top-level loop
	for _, s := range p.FleetStages {
		ids = append(ids, s.ID)
		_ = WalkFleetStages(s.FleetStages, func(_ string, child FleetStage) error {
			if _, seen := enclosing[child.ID]; !seen {
				enclosing[child.ID] = s.ID
			}
			return nil
		})
	}
	for _, id := range p.OnlyFleetStages {
		if slices.Contains(ids, id) {
			continue
		}
		if loop, ok := enclosing[id]; ok {
			return fmt.Errorf("--only-fleet-stages: fleet stage %s is inside the fleet stage loop %s and cannot be named alone; name %s to keep the whole loop", id, loop, loop)
		}
		return fmt.Errorf("--only-fleet-stages: the playbook has no fleet stage %q; its fleet stages are %s", id, strings.Join(ids, ", "))
	}
	// Requirements and output reads name earlier fleet stages only, so one pass
	// backwards adds everything the kept fleet stages need, directly or not.
	suggested := map[string]bool{}
	for _, id := range p.OnlyFleetStages {
		suggested[id] = true
	}
	for i := len(p.FleetStages) - 1; i >= 0; i-- {
		if suggested[p.FleetStages[i].ID] {
			for _, need := range fleetStageNeeds(p.FleetStages, i) {
				suggested[need.id] = true
			}
		}
	}
	var suggestion []string
	for _, id := range ids {
		if suggested[id] {
			suggestion = append(suggestion, id)
		}
	}
	for i, s := range p.FleetStages {
		if !p.KeepsFleetStage(s.ID) {
			continue
		}
		for _, need := range fleetStageNeeds(p.FleetStages, i) {
			if !p.KeepsFleetStage(need.id) {
				return fmt.Errorf("--only-fleet-stages %s leaves out fleet stage %s, %s; use --only-fleet-stages %s", strings.Join(p.OnlyFleetStages, ","), need.id, need.reason, strings.Join(suggestion, ","))
			}
		}
	}
	return nil
}

// fleetStageNeed is an earlier top-level fleet stage that a fleet stage needs,
// and why, such as "which fleet stage configure requires".
type fleetStageNeed struct{ id, reason string }

// fleetStageNeeds lists the earlier top-level fleet stages that fleetStages[i],
// or any fleet stage it encloses, requires or reads the outputs of. A loop's
// enclosed fleet stages cannot share an earlier fleet stage's ID, so such a
// name always means the earlier fleet stage.
func fleetStageNeeds(fleetStages []FleetStage, i int) []fleetStageNeed {
	earlier := map[string]bool{}
	for _, s := range fleetStages[:i] {
		earlier[s.ID] = true
	}
	var needs []fleetStageNeed
	seen := map[string]bool{}
	add := func(id, reason string) {
		if earlier[id] && !seen[id] {
			seen[id] = true
			needs = append(needs, fleetStageNeed{id, reason})
		}
	}
	_ = WalkFleetStages(fleetStages[i:i+1], func(path string, s FleetStage) error {
		for _, req := range append(append([]string{}, s.Requires.SameNode...), s.Requires.AllNodes...) {
			add(req, "which fleet stage "+path+" requires")
		}
		return nil
	})
	for _, ref := range References(fleetStages[i]) {
		parts, _ := value.Path(ref)
		if len(parts) >= 2 && parts[0].Name == "outputs" {
			add(parts[1].Name, "whose outputs fleet stage "+fleetStages[i].ID+" reads")
		}
	}
	return needs
}
