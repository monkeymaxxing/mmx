package config

import (
	"path/filepath"
	"strings"
	"testing"

	"mmx/internal/modules"
)

// durationProject writes a project whose two nodes give different values to
// the vars the playbook's durations read.
func durationProject(t *testing.T, settings, web1Vars, web2Vars string) string {
	t.Helper()
	path := fixture(t, settings)
	write(t, filepath.Join(filepath.Dir(path), "inventories/example.yml"),
		"nodes:\n  web1: {address: example.com, vars: {"+web1Vars+"}}\n  web2: {address: other.example.com, vars: {"+web2Vars+"}}\n")
	return path
}

// Every duration setting may be one whole reference to a var. Each node gets
// its own value before the run, inside a node task block too, and the
// playbook's node tasks keep the reference for the next node.
func TestADurationReadsEachNodesVar(t *testing.T) {
	path := durationProject(t, `fleet_stages:
  - id: app
    nodes: all
    node_tasks:
      - shell: {script: work}
        time_limit: '{{ .vars.limit }}'
        time_limit_per_try: '{{ .vars.limit }}'
        retry: {max_tries: 2, pause_between_tries: '{{ .vars.pause }}'}
      - task_block:
          id: op
          changes:
            - wait_for: {path_exists: /ready, wait_at_most: '{{ .vars.wait }}'}
      - systemd_scheduled_job: {name: backup, script: run, cron_schedule: '30 4 * * *', run_as_user_account: root, random_delay: '{{ .vars.wait }}', stop_a_run_after: '{{ .vars.limit }}'}
      - reboot: {wait_at_most: '{{ .vars.reboot_wait }}'}
`, "limit: 2m, pause: 5s, wait: 1m, reboot_wait: 30s", "limit: 1h, pause: 0s, wait: 3m, reboot_wait: 10m")
	p, inv, err := Load(path, "example", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	tasks := p.FleetStages[0].NodeTasks
	for node, want := range map[string][6]string{"web1": {"2m", "5s", "1m", "1m", "2m", "30s"}, "web2": {"1h", "0s", "3m", "3m", "1h", "10m"}} {
		resolved, err := ResolveDurations(tasks, DurationScope(p, inv, node))
		if err != nil {
			t.Fatal(node, err)
		}
		shell, job := resolved[0], InputsOf[modules.SystemdScheduledJob](resolved[2])
		got := [6]string{*shell.TimeLimit, shell.Retry.PauseBetweenTries, InputsOf[modules.WaitFor](resolved[1].TaskBlock.Changes[0]).WaitAtMost, job.RandomDelay, job.StopARunAfter, InputsOf[modules.Reboot](resolved[3]).WaitAtMost}
		if got != want || *shell.TimeLimitPerTry != want[0] {
			t.Fatalf("%s: %v, want %v", node, got, want)
		}
	}
	if *tasks[0].TimeLimit != "{{ .vars.limit }}" || InputsOf[modules.Reboot](tasks[3]).WaitAtMost != "{{ .vars.reboot_wait }}" || InputsOf[modules.WaitFor](tasks[1].TaskBlock.Changes[0]).WaitAtMost != "{{ .vars.wait }}" {
		t.Fatal("resolving a node's durations changed the playbook's node tasks")
	}
}

// A duration reference that MMX cannot give before the run, or a var whose
// value is no such duration, fails as the project loads, naming the
// setting, and for a value also the node and where the var is set.
func TestADurationThatIsNoVarsDurationFailsAsTheProjectLoads(t *testing.T) {
	reboot := func(wait string) string {
		return "fleet_stages: [{id: app, nodes: all, node_tasks: [{reboot: {wait_at_most: '" + wait + "'}}]}]\n"
	}
	for _, tc := range []struct {
		name, settings, web2Vars, files, want string
	}{
		{"an output", reboot("{{ .outputs.setup.web1.wait }}"), "", "", "(fleet_stages[0].node_tasks[0].reboot.wait_at_most): reboot.wait_at_most must be a duration, such as 15m, or one whole reference to a var"},
		{"a fact", reboot("{{ .node.facts.wait }}"), "", "", "one whole reference to a var"},
		{"a secret", reboot("{{ .secrets.wait }}"), "", "", "one whole reference to a var"},
		{"text around the reference", reboot("{{ .vars.wait }}s"), "", "", "one whole reference to a var"},
		{"a var that set_vars sets", "fleet_stages: [{id: app, nodes: all, node_tasks: [{set_vars: {wait: 5m}}, {reboot: {wait_at_most: '{{ .vars.wait }}'}}]}]\n", "", "", "(fleet_stages[0].node_tasks[1].reboot.wait_at_most): reboot.wait_at_most reads the var wait, which a set_vars node task of this fleet stage sets during the run"},
		{"in a role", "fleet_stages: [{id: app, nodes: all, node_tasks: [{role: app}]}]\n", "", "roles/app/main.yml", "(node_tasks[0].time_limit): time_limit in a role must be a duration written out"},
		{"a value that is no duration", reboot("{{ .vars.wait }}"), "wait: soon", "", "fleet stage app, node web2: playbooks/example.yml"},
		{"a value that is no text", reboot("{{ .vars.wait }}"), "wait: 30", "", "the var holds 30, which is no duration"},
		{"a NetworkManager restore deadline", "fleet_stages: [{id: app, nodes: all, node_tasks: [{networkmanager_connection_file: {name: eth1.nmconnection, content: \"[connection]\\nid=eth1\\n\", restore_unless_controller_confirms_within: '{{ .vars.wait }}'}}]}]\n", "wait: soon", "", "networkmanager_connection_file.restore_unless_controller_confirms_within"},
		{"a value in a fleet stage inside a for_each_node loop", "fleet_stages:\n  - id: rollout\n    for_each_node: all\n    as: server\n    fleet_stages:\n      - id: restart\n        nodes: '{{ .server }}'\n        node_tasks: [{reboot: {wait_at_most: '{{ .vars.wait }}'}}]\n", "wait: soon", "", "fleet stage restart, node web2: playbooks/example.yml:10:46 (fleet_stages[0].fleet_stages[0].node_tasks[0].reboot.wait_at_most): reboot.wait_at_most must be a positive duration; value supplied at inventories/example.yml"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := durationProject(t, tc.settings, "wait: 1m", tc.web2Vars)
			if tc.files != "" {
				write(t, filepath.Join(filepath.Dir(path), tc.files), "node_tasks: [{shell: {script: work}, time_limit: '{{ .vars.limit }}'}]\n")
			}
			_, _, err := Load(path, "example", "", nil)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v\nwant %s", err, tc.want)
			}
		})
	}
	// The value's error names the setting, the rule, and where the var is set.
	path := durationProject(t, reboot("{{ .vars.wait }}"), "wait: 1m", "wait: soon")
	_, _, err := Load(path, "example", "", nil)
	if err == nil || !strings.Contains(err.Error(), "(fleet_stages[0].node_tasks[0].reboot.wait_at_most): reboot.wait_at_most must be a positive duration; value supplied at inventories/example.yml") {
		t.Fatal(err)
	}
}
