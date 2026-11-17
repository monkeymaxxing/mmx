package main

import (
	"strings"
	"testing"
)

// mmx remove-mmx-from-node takes the name of one node of the inventory, and
// nothing else changes what the command does.
func TestRemoveMMXFromNodeTakesOneNodeOfTheInventory(t *testing.T) {
	initializedProject(t)
	for _, args := range [][]string{
		{"remove-mmx-from-node"},
		{"remove-mmx-from-node", "web01", "--json"},
		{"remove-mmx-from-node", "web01", "--limit", "web01"},
		{"remove-mmx-from-node", "web01", "--connection", "ssh_only"},
	} {
		if _, code, err := captureCommand(t, args...); code != 4 || err == nil {
			t.Fatalf("%v: %d %v", args, code, err)
		}
	}
	if _, code, err := captureCommand(t, "remove-mmx-from-node", "nonexistent"); code != 4 || err == nil || !strings.Contains(err.Error(), "nonexistent") {
		t.Fatal(code, err)
	}
}

// An unknown node is reported by the argument the user typed, not by the
// --nodes option of run-command, whose loader the command reuses.
func TestRemoveMMXFromNodeNamesTheNodeArgument(t *testing.T) {
	initializedProject(t)
	_, code, err := captureCommand(t, "remove-mmx-from-node", "web09")
	if code != 4 || err == nil || strings.Contains(err.Error(), "--nodes") || !strings.HasPrefix(err.Error(), `remove-mmx-from-node: no node named "web09" in inventory `) {
		t.Fatal(code, err)
	}
}

// The last line starts from the reader's goal, to stop managing the node,
// so it does not read as an order to delete the server.
func TestRemoveMMXFromNodeEndsWithTheStepToStopManagingTheNode(t *testing.T) {
	const want = "To stop managing app01 with MMX, also take app01 out of the inventory, its groups, and the playbooks. MMX did not change those files, so until then, a run that selects app01 sets MMX up on it again."
	if got := stopManagingTheNode("app01"); got != want {
		t.Fatalf("got %q", got)
	}
}
