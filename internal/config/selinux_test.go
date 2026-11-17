package config

import (
	"strings"
	"testing"

	"mmx/internal/modules"
	"mmx/internal/value"
)

func TestSELinuxConfiguration(t *testing.T) {
	for _, task := range []string{
		`selinux_boolean: {name: httpd_can_network_connect, enabled: true}`,
		`selinux_boolean: {name: "{{ .vars.boolean }}", enabled: false}`,
		`selinux_file_context: {path: /srv/www, also_apply_to_everything_inside: true, selinux_type: httpd_sys_content_t}`,
		`selinux_file_context: {path: /srv/www/index.html, also_apply_to_everything_inside: false, selinux_type: "{{ .vars.label_type }}"}`,
		`selinux_file_context: {path: /srv/www, also_apply_to_everything_inside: true, state: absent}`,
		`selinux_port: {port: 8443, protocol: tcp, selinux_type: http_port_t}`,
		`selinux_port: {port: "8100:8102", protocol: udp, selinux_type: http_port_t}`,
		`selinux_port: {port: 8443, protocol: tcp, state: absent}`,
		`selinux_mode: {mode: enforcing}`,
		`selinux_mode: {mode: permissive}`,
	} {
		p := fixture(t, "vars: {boolean: httpd_can_network_connect, label_type: httpd_sys_content_t}\nfleet_stages: [{id: configure, nodes: all, node_tasks: [{"+task+"}]}]\n")
		project, _, err := Load(p, "example", "", nil)
		if err != nil {
			t.Fatal(task, err)
		}
		if loaded := project.FleetStages[0].NodeTasks[0]; loaded.Retry == nil || loaded.Retry.Disabled {
			t.Fatal("resource did not inherit retry policy", task)
		}
	}
	for task, want := range map[string]string{
		`selinux_boolean: {name: httpd_can_network_connect}`:                                                                              "enabled is required",
		`selinux_boolean: {name: "httpd can", enabled: true}`:                                                                             "name must be a name as SELinux writes it",
		`selinux_boolean: {name: httpd_can_network_connect, enabled: "on"}`:                                                               "expected bool",
		`selinux_file_context: {path: srv/www, also_apply_to_everything_inside: true, selinux_type: httpd_sys_content_t}`:                 "path must be absolute",
		`selinux_file_context: {path: /srv/www, selinux_type: httpd_sys_content_t}`:                                                       "also_apply_to_everything_inside is required",
		`selinux_file_context: {path: /srv/www, also_apply_to_everything_inside: true}`:                                                   "selinux_type is required",
		`selinux_file_context: {path: /srv/www, also_apply_to_everything_inside: true, selinux_type: "a:b"}`:                              "selinux_type must be a name as SELinux writes it",
		`selinux_file_context: {path: /srv/www, also_apply_to_everything_inside: true, selinux_type: httpd_sys_content_t, state: absent}`: "accepts only path",
		`selinux_port: {port: 8443, selinux_type: http_port_t}`:                                                                           "protocol must be tcp or udp",
		`selinux_port: {port: 8443, protocol: sctp, selinux_type: http_port_t}`:                                                           "protocol must be tcp or udp",
		`selinux_port: {protocol: tcp, selinux_type: http_port_t}`:                                                                        "1 to 65535",
		`selinux_port: {port: "10:5", protocol: tcp, selinux_type: http_port_t}`:                                                          "first below last",
		`selinux_port: {port: 8443, protocol: tcp}`:                                                                                       "selinux_type is required",
		`selinux_port: {port: 8443, protocol: tcp, selinux_type: http_port_t, state: absent}`:                                             "accepts only port, protocol, and state",
		`selinux_mode: {mode: disabled}`:                                                                                                  "enforcing or permissive; MMX does not disable SELinux",
		`selinux_mode: {}`:                                                                                                                "enforcing or permissive",
	} {
		p := fixture(t, "fleet_stages: [{id: configure, nodes: all, node_tasks: [{"+task+"}]}]\n")
		if _, _, err := Load(p, "example", "", nil); err == nil || !strings.Contains(err.Error(), want) {
			t.Error(task, err)
		}
	}
	// Resolved paths are clean, and references take their rules.
	task := Task{Module: "selinux_file_context", ModuleInputs: &modules.SELinuxFileContext{Path: "/srv/./web/", SELinuxType: "{{ .vars.label_type }}"}}
	resolved, err := ResolveArguments(task, value.Scope{"vars": map[string]any{"label_type": "httpd_sys_content_t"}})
	if err != nil || InputsOf[modules.SELinuxFileContext](resolved).Path != "/srv/web" || InputsOf[modules.SELinuxFileContext](resolved).SELinuxType != "httpd_sys_content_t" || InputsOf[modules.SELinuxFileContext](task).SELinuxType != "{{ .vars.label_type }}" {
		t.Fatal(InputsOf[modules.SELinuxFileContext](resolved), err)
	}
	if _, err := ResolveArguments(task, value.Scope{"vars": map[string]any{"label_type": "system_u:object_r:httpd_sys_content_t:s0"}}); err == nil || !strings.Contains(err.Error(), "selinux_file_context.selinux_type") {
		t.Fatal(err)
	}
}
