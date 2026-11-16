package config

import (
	"strings"
	"testing"
)

// A wireguard_interface file that wg-quick could not read, or that holds
// what MMX owns, fails when the project loads, before any connection.
func TestWireguardInterfaceFileIsCheckedWhenTheProjectLoads(t *testing.T) {
	task := func(name, content string) string {
		return "fleet_stages:\n- id: network\n  nodes: all\n  node_tasks:\n  - wireguard_interface:\n      name: " + name + "\n      content: |\n" +
			"        " + strings.ReplaceAll(strings.TrimSuffix(content, "\n"), "\n", "\n        ") + "\n"
	}
	valid := "[Interface]\nAddress = 10.0.0.1/24\n[Peer]\nPublicKey = xntON0r+ip4v53R2PDYOEKeq0RH91Y9x307uPnUQOTE=\nAllowedIPs = 10.0.0.2/32\n"
	if _, _, err := Load(fixture(t, task("wg0", valid)), "example", "", nil); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ name, content, refusal string }{
		{"wg0", "[Interface]\nPrivateKey = xntON0r+ip4v53R2PDYOEKeq0RH91Y9x307uPnUQOTE=\n", "line 2: PrivateKey is refused"},
		{"wg0", "[Interface]\nSaveConfig = true\n", "line 2: SaveConfig = true is refused"},
		{"wg0", "[Peer]\nPublicKey = xntON0r+ip4v53R2PDYOEKeq0RH91Y9x307uPnUQOTE=\n", "the file has no [Interface] section"},
		{"wg+0", valid, "wireguard_interface.name must be 1 to 15 of the characters"},
	} {
		_, _, err := Load(fixture(t, task(c.name, c.content)), "example", "", nil)
		if err == nil || !strings.Contains(err.Error(), c.refusal) {
			t.Errorf("%s %q: %v", c.name, c.content, err)
		}
	}
}

func TestWireguardInterfaceRejectsTaskTimingAndIgnoresTimingDefaults(t *testing.T) {
	for _, policy := range []string{"retry: true", "retry: false", "retry: {}", "time_limit: 1s", "time_limit: null", "time_limit_per_try: 1s", "time_limit_per_try: null"} {
		path := fixture(t, "fleet_stages:\n- id: network\n  nodes: all\n  node_tasks:\n  - wireguard_interface: {name: wg0, content: '[Interface]'}\n    "+policy+"\n")
		if _, _, err := Load(path, "example", "", nil); err == nil {
			t.Fatal("accepted", policy)
		}
	}
	path := fixture(t, "node_task_defaults:\n  retry: {max_tries: unlimited}\n  time_limit: 1ms\n  time_limit_per_try: 1ms\nfleet_stages:\n- id: network\n  nodes: all\n  node_tasks:\n  - wireguard_interface: {name: wg0, content: '[Interface]'}\n")
	project, _, err := Load(path, "example", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	task := project.FleetStages[0].NodeTasks[0]
	if task.Retry != nil || task.TimeLimit != nil || task.TimeLimitPerTry != nil {
		t.Fatal(task)
	}
}
