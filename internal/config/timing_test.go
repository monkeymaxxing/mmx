package config

import (
	"testing"
)

func TestTimingInheritanceNullAndOptIn(t *testing.T) {
	path := fixture(t, `node_task_defaults:
  time_limit: 1m
  time_limit_per_try: 5s
  retry: {max_tries: 8, pause_between_tries: 0s}
fleet_stages:
- id: setup
  nodes: all
  node_tasks:
  - show: once
  - show: repeat
    retry: {}
  - show: bounded_by_time
    time_limit_per_try: null
    retry: {max_tries: unlimited}
  - show: bounded_by_count
    time_limit: null
    retry: {max_tries: 2, pause_between_tries: 1s}
`)
	p, _, err := Load(path, "example", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	ts := p.FleetStages[0].NodeTasks
	if ts[0].Retry != nil {
		t.Fatal("defaults enabled retries")
	}
	if ts[1].Retry.MaxTries != 8 || ts[1].Retry.PauseBetweenTries != "0s" {
		t.Fatal("per-setting inheritance failed")
	}
	if ts[2].Retry.MaxTries != 0 || ts[2].TimeLimitPerTry != nil || *ts[2].TimeLimit != "1m" {
		t.Fatal("explicit null did not remove limit")
	}
	if ts[3].TimeLimit != nil || ts[3].Retry.MaxTries != 2 {
		t.Fatal("override failed")
	}
}
func TestUnboundedOrInvalidRetriesRejected(t *testing.T) {
	for _, fields := range []string{"retry: {max_tries: null}", "retry: {max_tries: 0}", "retry: {pause_between_tries: -1s}", "time_limit: 0s", "retry: {until: true, magic: yes}"} {
		path := fixture(t, "fleet_stages:\n- id: setup\n  nodes: all\n  node_tasks:\n  - show: hi\n    "+fields+"\n")
		if _, _, err := Load(path, "example", "", nil); err == nil {
			t.Fatal("accepted", fields)
		}
	}
}

func TestBuiltinRetriesAndExplicitOptOut(t *testing.T) {
	path := fixture(t, `fleet_stages:
- id: setup
  nodes: all
  node_tasks:
  - file: {permissions_mode: '0644', path: /tmp/example, content: hello}
  - apt_package: {packages: [{name: nginx, version: newest_when_first_installed}]}
  - systemd_service: {name: nginx, state: running}
  - http: {url: 'http://localhost/'}
  - shell: {script: 'true'}
  - file: {permissions_mode: '0644', path: /tmp/example, content: hello}
    retry: false
  - shell: {script: 'true'}
    retry: true
  - systemd_service: {name: nginx, state: running}
    retry: true
`)
	p, _, err := Load(path, "example", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	nodeTasks := p.FleetStages[0].NodeTasks
	for _, i := range []int{0, 1, 6, 7} {
		r := nodeTasks[i].Retry
		if r == nil || r.Disabled || r.MaxTries == 0 || r.MaxTries != 3 || r.PauseBetweenTries != "2s" {
			t.Fatalf("task %d: %+v", i, r)
		}
	}
	// A start that failed fails again, so systemd_service tries once, like a
	// request and a command.
	if nodeTasks[2].Retry != nil || nodeTasks[3].Retry != nil || nodeTasks[4].Retry != nil || nodeTasks[5].Retry == nil || !nodeTasks[5].Retry.Disabled {
		t.Fatal("retry opt-in/opt-out changed")
	}
}
