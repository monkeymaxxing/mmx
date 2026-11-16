package main

import (
	"context"
	"errors"
	"fmt"

	"mmx/internal/config"
	"mmx/internal/controller"
)

// removeMMXFromNode removes MMX from one node and from the controller
// machine (controller.RemoveMMXFromNode), prints one line for each thing it
// removed, and reminds the reader of the step it does not do: taking the node
// out of the inventory and the playbooks. It exits 2 when it refuses because a
// node job runs or an owed restore waits on the node.
func removeMMXFromNode(ctx context.Context, c controller.Controller, p config.Project, inv config.Inventory, name string) (int, error) {
	h, ok := inv.Nodes[name]
	if !ok {
		return 4, fmt.Errorf("remove-mmx-from-node takes the name of one node of inventory %s, not %q", inv.Name, name)
	}
	removed, err := c.RemoveMMXFromNode(ctx, p, name, h)
	for _, line := range removed {
		fmt.Println(escapeControlCharacters(line))
	}
	if errors.Is(err, controller.ErrNodeNotIdle) {
		return 2, err
	}
	if err != nil {
		return 4, err
	}
	if len(removed) == 0 {
		fmt.Printf("Nothing of MMX is left on %s or on the controller machine.\n", name)
	}
	fmt.Printf("MMX did not change the inventory or the playbooks. Take %s out of the inventory, its groups, and the playbooks; until then, a run that selects %s sets MMX up on it again.\n", name, name)
	return 0, nil
}
