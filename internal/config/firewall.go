package config

import (
	"errors"
	"fmt"
	"strings"

	"mmx/internal/modules"
	"mmx/internal/value"
)

// inventoryGroupRules is a module whose rules may name an inventory group:
// one rule for each node of the group, with that node's address.
type inventoryGroupRules interface {
	modules.Inputs
	ExpandInventoryGroups(inventory any) error
	NamesInventoryGroups() bool
	InventoryGroupRuleLists() map[string][]any
}

// expandInventoryGroups expands a resolved task's from_inventory_group rules.
// It reads .inventory in s, which every node job carries. A rule list that
// written held as one whole-value reference was not checked when the project
// loaded, so its resolved rules are checked here, before the module runs.
func expandInventoryGroups(t *Task, s value.Scope, written Task) error {
	name, inputs := t.ModuleCall()
	if rules, ok := inputs.(inventoryGroupRules); ok {
		if err := rules.ExpandInventoryGroups(s["inventory"]); err != nil {
			return t.Sources.Error(name, err)
		}
		if ruleListIsAReference(written, rules) {
			if err := rules.Prepare(""); err != nil {
				return t.Sources.Error(name, fmt.Errorf("with the rules that the reference gave: %w", err))
			}
		}
	}
	return nil
}

// checkInventoryGroupRules checks every rule with from_inventory_group before
// MMX connects, whatever its conditions: the group exists, each of its nodes
// gives an address under the variable, and the expanded rules are not listed
// twice.
func checkInventoryGroupRules(stages []FleetStage, inv Inventory) error {
	inventory := InventoryValues(inv)
	var walk func(nodeTasks []Task) error
	walk = func(nodeTasks []Task) error {
		for _, t := range nodeTasks {
			name, inputs := t.ModuleCall()
			if rules, ok := inputs.(inventoryGroupRules); ok && !ruleListIsAReference(t, rules) {
				expanded := modules.CopyInputs(rules)
				if err := expanded.ExpandInventoryGroups(inventory); err != nil {
					return t.Sources.Error(name, err)
				}
				// The expanded rules are checked as the module checks
				// written ones; their positions are not the written ones.
				if err := expanded.Prepare(""); err != nil {
					var specific *modules.FieldError
					if errors.As(err, &specific) {
						err = specific.Err
					}
					return t.Sources.Error(name+".rules", fmt.Errorf("with one rule for each node of the inventory group: %w", err))
				}
			}
			for _, list := range t.Children() {
				if err := walk(list.NodeTasks); err != nil {
					return err
				}
			}
		}
		return nil
	}
	return WalkFleetStages(stages, func(_ string, s FleetStage) error { return walk(s.NodeTasks) })
}

// ruleListIsAReference reports a rule list written as one whole-value
// reference, such as the rules an earlier node task works out. Its rules are
// known only once the reference has a value, so ResolveArguments expands and
// checks them then, on the node, before the module changes anything.
func ruleListIsAReference(t Task, rules inventoryGroupRules) bool {
	for field := range rules.InventoryGroupRuleLists() {
		for _, ref := range t.WholeValueReferences {
			if ref.Field == field || strings.HasPrefix(ref.Field, field+"[") || strings.HasPrefix(ref.Field, field+".") {
				return true
			}
		}
	}
	return false
}
