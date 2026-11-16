package config

import "testing"

func TestNetworkManagerConnectionFileRejectsTaskTimingAndIgnoresDefaults(t *testing.T) {
	for _, policy := range []string{"retry: true", "retry: false", "retry: {}", "time_limit: 1s", "time_limit: null", "time_limit_per_try: 1s", "time_limit_per_try: null"} {
		path := fixture(t, "fleet_stages:\n- id: network\n  nodes: all\n  node_tasks:\n  - networkmanager_connection_file: {name: 'System eth1.nmconnection', content: example}\n    "+policy+"\n")
		if _, _, err := Load(path, "example", "", nil); err == nil {
			t.Fatal("accepted", policy)
		}
	}
	path := fixture(t, "node_task_defaults:\n  time_limit: 1ms\n  time_limit_per_try: 1ms\n  retry: {max_tries: 3}\nfleet_stages:\n- id: network\n  nodes: all\n  node_tasks:\n  - networkmanager_connection_file: {name: example.nmconnection, content: example}\n")
	project, _, err := Load(path, "example", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	task := project.FleetStages[0].NodeTasks[0]
	if task.Retry != nil || task.TimeLimit != nil || task.TimeLimitPerTry != nil {
		t.Fatal(task)
	}
}
