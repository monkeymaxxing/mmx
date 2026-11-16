package config

import (
	"encoding/json"
	"mmx/internal/modules"
	"strings"
	"testing"
	"time"
)

func TestUnlimitedTriesRequireResolvedTimeLimit(t *testing.T) {
	for _, tc := range []struct {
		name, defaults, fields string
		valid                  bool
	}{
		{"task limit", "", "time_limit: 1s\nretry: {max_tries: unlimited}", true},
		{"inherited limit", "time_limit: 1s", "retry: {max_tries: unlimited}", true},
		{"inherited unlimited", "retry: {max_tries: unlimited}", "time_limit: 1s\nretry: true", true},
		{"no limit", "", "retry: {max_tries: unlimited}", false},
		{"removed limit", "time_limit: 1s", "time_limit: null\nretry: {max_tries: unlimited}", false},
		{"inherited unbounded", "retry: {max_tries: unlimited}", "retry: true", false},
		{"null task count", "", "time_limit: 1s\nretry: {max_tries: null}", false},
		{"null default count", "retry: {max_tries: null}", "time_limit: 1s\nretry: true", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			defaults := ""
			if tc.defaults != "" {
				defaults = "node_task_defaults:\n  " + tc.defaults + "\n"
			}
			path := fixture(t, defaults+"fleet_stages:\n- id: setup\n  nodes: all\n  node_tasks:\n  - show: ready\n    "+strings.ReplaceAll(tc.fields, "\n", "\n    ")+"\n")
			p, _, err := Load(path, "example", "", nil)
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v: %v", tc.valid, err)
			}
			if !tc.valid {
				return
			}
			task := p.FleetStages[0].NodeTasks[0]
			data, err := json.Marshal(task)
			if err != nil || !strings.Contains(string(data), `"max_tries":"unlimited"`) {
				t.Fatalf("%s: %v", data, err)
			}
			var restored Task
			if err := json.Unmarshal(data, &restored); err != nil || restored.Retry.MaxTries != 0 || restored.TimeLimit == nil {
				t.Fatalf("round trip: %+v %v", restored, err)
			}
		})
	}
}

func TestOldNodeTaskSettingsAreUnknownFields(t *testing.T) {
	for _, task := range []string{
		"show: x\ntask_timeout: 1s", "show: x\nexecution_timeout: 1s",
		"show: x\nretry: {max_executions: 2}", "show: x\nretry: {pause: 1s}",
		"http: {url: 'http://localhost', status: 200}", "reboot: {timeout: 1m}",
		"file: {permissions_mode: '0644', path: /tmp/x, content: x, validate: {shell: {script: true}}}",
		"line: {insert_at_end_of_file: true, path: /tmp/x, line: x, validate: {}}",
		"download: {permissions_mode: '0644', url: 'https://example.com/a', path: /tmp/x, validate: {}}",
	} {
		t.Run(task, func(t *testing.T) {
			path := fixture(t, "fleet_stages:\n- id: setup\n  nodes: all\n  node_tasks:\n  - "+strings.ReplaceAll(task, "\n", "\n    ")+"\n")
			if _, _, err := Load(path, "example", "", nil); err == nil || !strings.Contains(err.Error(), "unknown field") && !strings.Contains(err.Error(), "is not a node task setting or a core module") {
				t.Fatal(err)
			}
		})
	}
	for _, field := range []string{"task_timeout: 1s", "execution_timeout: 1s", "retry: {max_executions: 2}", "retry: {pause: 1s}"} {
		path := fixture(t, "node_task_defaults:\n  "+field+"\nfleet_stages: []\n")
		if _, _, err := Load(path, "example", "", nil); err == nil || !strings.Contains(err.Error(), "unknown field") {
			t.Fatal(field, err)
		}
	}
}

func TestWaitForValidationAndTiming(t *testing.T) {
	for _, fields := range []string{
		"{tcp_port_refuses_connections: 0}", "{tcp_port_refuses_connections: 65536}",
		"{path_exists: /tmp/x, trusted_certificate_authority_file: /tmp/ca.pem}",
		"{tcp_port_accepts_connections: 443, dangerously_skip_server_certificate_check: true}",
		"{}", "{tcp_port_accepts_connections: 80, path_exists: /tmp/x}", "{tcp_port_accepts_connections: 80, tcp_port_refuses_connections: 80}", "{path_exists: /tmp/x, path_is_missing: /tmp/x}",
		"{url_responds: 'http://localhost', command_succeeds: true}", "{path_exists: /tmp/x, tcp_host: localhost}",
		"{tcp_port_accepts_connections: 80, expected_http_status_codes: [200]}", "{path_exists: /tmp/x, response_body_contains: ''}",
		"{tcp_port_accepts_connections: 0}", "{tcp_port_accepts_connections: 65536}", "{path_exists: relative}", "{url_responds: 'ftp://localhost'}", "{tcp_port_accepts_connections: 80, wait_at_most: 0s}",
		"{url_responds: 'http://localhost', expected_http_status_codes: [99]}",
		"{url_responds: 'http://localhost', expected_http_status_codes: [200, 600]}", "{url_responds: 'http://localhost', expected_http_status_codes: []}",
		"{url_responds: 'http://localhost', expected_http_status_codes: 200}",
	} {
		path := fixture(t, "fleet_stages:\n- id: setup\n  nodes: all\n  node_tasks:\n  - wait_for: "+fields+"\n")
		if _, _, err := Load(path, "example", "", nil); err == nil {
			t.Fatal("accepted", fields)
		}
	}
	for _, module := range []string{"wait_for: {tcp_port_accepts_connections: 80}", "reboot: {}", "systemd_networkd_file: {name: 20-second.network, content: example}"} {
		for _, policy := range []string{"retry: true", "retry: false", "retry: {}", "time_limit: 1s", "time_limit: null", "time_limit_per_try: 1s", "time_limit_per_try: null"} {
			path := fixture(t, "fleet_stages:\n- id: setup\n  nodes: all\n  node_tasks:\n  - "+module+"\n    "+policy+"\n")
			if _, _, err := Load(path, "example", "", nil); err == nil {
				t.Fatal("accepted", module, policy)
			}
		}
	}
	path := fixture(t, `node_task_defaults:
  time_limit: 1ms
  time_limit_per_try: 1ms
  retry: {max_tries: unlimited, pause_between_tries: 1s}
fleet_stages:
- id: setup
  nodes: all
  node_tasks:
  - wait_for: {tcp_port_accepts_connections: 80}
  - systemd_networkd_file: {name: 20-second.network, content: example}
  - reboot: {wait_at_most: 2m}
`)
	p, _, err := Load(path, "example", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, task := range p.FleetStages[0].NodeTasks {
		if task.Retry != nil || task.TimeLimit != nil || task.TimeLimitPerTry != nil {
			t.Fatal(task)
		}
	}
	if wait, err := (modules.Reboot{}).Wait(); err != nil || wait != 10*time.Minute {
		t.Fatal(wait, err)
	}
	if wait, err := InputsOf[modules.Reboot](p.FleetStages[0].NodeTasks[2]).Wait(); err != nil || wait != 2*time.Minute {
		t.Fatal(wait, err)
	}
	if wait, err := (modules.WaitFor{}).Wait(); err != nil || wait != time.Minute {
		t.Fatal(wait, err)
	}
	for _, phase := range []string{"verify", "activate", "on_failure", "cleanup"} {
		path := fixture(t, "fleet_stages:\n- id: setup\n  nodes: all\n  node_tasks:\n  - task_block:\n      id: configure\n      changes: [{show: x}]\n      "+phase+": [{wait_for: {path_exists: /tmp/ready}}]\n")
		if _, _, err := Load(path, "example", "", nil); err != nil {
			t.Fatal(phase, err)
		}
	}
}

func TestRenamedScopesAndReservedNames(t *testing.T) {
	for _, name := range []string{"node_tasks", "this_node_task"} {
		path := fixture(t, "fleet_stages:\n- id: setup\n  nodes: all\n  node_tasks:\n  - show: x\n    loop: [x]\n    as: "+name+"\n")
		if _, _, err := Load(path, "example", "", nil); err == nil {
			t.Fatal("accepted reserved scope", name)
		}
	}
	for _, task := range []string{
		"show: '{{ .results.first.value }}'",
		"shell: {script: 'true', changed_when: {equals: ['{{ .result.exit_code }}', 0]}}",
		"file: {permissions_mode: '0644', path: /tmp/x, content: x, check_new_file: {shell: {script: 'true', env: {NEW_FILE: '{{ .candidate.path }}'}}}}",
	} {
		path := fixture(t, "fleet_stages:\n- id: setup\n  nodes: all\n  node_tasks:\n  - id: first\n    show: ready\n  - "+task+"\n")
		if _, _, err := Load(path, "example", "", nil); err == nil {
			t.Fatal("accepted old scope", task)
		}
	}
}

func TestRoleAndExternalModuleRejectUnknownCallFields(t *testing.T) {
	for _, action := range []string{"role: greeting", "greeting: {}"} {
		path := fixture(t, "fleet_stages:\n- id: setup\n  nodes: all\n  node_tasks:\n  - "+action+"\n    with: {person: Barbara}\n")
		if _, _, err := Load(path, "example", "", nil); err == nil || !strings.Contains(err.Error(), "with is not a node task setting") && !strings.Contains(err.Error(), "names greeting and with") {
			t.Fatal(action, err)
		}
	}
}
