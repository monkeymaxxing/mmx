package config

import (
	"strings"
	"testing"

	"mmx/internal/modules"
	"mmx/internal/value"
)

func TestNodeSettingsConfiguration(t *testing.T) {
	for _, task := range []string{
		`hostname: {name: web1}`,
		`hostname: {name: "{{ .node.name }}.example.com"}`,
		`time_zone: {name: Europe/Berlin}`,
		`time_zone: {name: "{{ .vars.zone }}"}`,
		`locale: {name: en_US.UTF-8, use_as_system_default: true}`,
		`locale: {name: sr_RS@latin, use_as_system_default: false}`,
		`update_alternatives: {name: editor, program_path: /usr/bin/vim.basic}`,
		`update_alternatives: {name: editor, automatic: true}`,
		`update_alternatives: {name: java, program_path: /opt/jdk/bin/java, generic_command_path: /usr/bin/java, priority_when_registered: 100}`,
	} {
		p := fixture(t, "vars: {zone: UTC}\nfleet_stages: [{id: configure, nodes: all, node_tasks: [{"+task+"}]}]\n")
		project, _, err := Load(p, "example", "", nil)
		if err != nil {
			t.Fatal(task, err)
		}
		if loaded := project.FleetStages[0].NodeTasks[0]; loaded.Retry == nil || loaded.Retry.Disabled {
			t.Fatal("resource did not inherit retry policy", task)
		}
	}
	for task, want := range map[string]string{
		`hostname: {}`:                          "hostname.name is required",
		`hostname: {name: web_1}`:               "hostname.name must be a host name",
		`hostname: {name: web1.}`:               "hostname.name must be a host name",
		`hostname: {name: web1, aliases: [a]}`:  "aliases",
		`time_zone: {}`:                         "time_zone.name is required",
		`time_zone: {name: ../../etc/passwd}`:   "time_zone.name must be a name from the time zone database",
		`locale: {name: en_US.UTF-8}`:           "locale.use_as_system_default is required",
		`locale: {use_as_system_default: true}`: "locale.name is required",
		`locale: {name: "en_US.UTF-8 UTF-8", use_as_system_default: true}`:                                                                       "locale.name must be a locale name",
		`locale: {name: en_US.UTF-8, use_as_system_default: "yes"}`:                                                                              "expected bool",
		`update_alternatives: {program_path: /usr/bin/vim}`:                                                                                      "update_alternatives.name is required",
		`update_alternatives: {name: editor}`:                                                                                                    "update_alternatives.program_path is required unless automatic: true",
		`update_alternatives: {name: editor, program_path: usr/bin/vim}`:                                                                         "path must be absolute",
		`update_alternatives: {name: editor, automatic: true, program_path: /usr/bin/vim}`:                                                       "takes no program_path with automatic: true",
		`update_alternatives: {name: editor, program_path: /usr/bin/vim, generic_command_path: /usr/bin/editor}`:                                 "priority_when_registered is required with generic_command_path",
		`update_alternatives: {name: editor, program_path: /usr/bin/vim, priority_when_registered: 5}`:                                           "generic_command_path is required with priority_when_registered",
		`update_alternatives: {name: editor, program_path: /usr/bin/vim, generic_command_path: /usr/bin/editor, priority_when_registered: high}`: "priority",
		`update_alternatives: {name: a/b, program_path: /usr/bin/vim}`:                                                                           "update_alternatives.name must be a generic name",
	} {
		p := fixture(t, "fleet_stages: [{id: configure, nodes: all, node_tasks: [{"+task+"}]}]\n")
		if _, _, err := Load(p, "example", "", nil); err == nil || !strings.Contains(err.Error(), want) {
			t.Error(task, err)
		}
	}
	// References resolve with the same rules, and resolved paths are clean.
	task := Task{Module: "hostname", ModuleInputs: &modules.Hostname{Name: "{{ .node.name }}.example.com"}}
	resolved, err := ResolveArguments(task, value.Scope{"node": map[string]any{"name": "web1"}})
	if err != nil || InputsOf[modules.Hostname](resolved).Name != "web1.example.com" || InputsOf[modules.Hostname](task).Name != "{{ .node.name }}.example.com" {
		t.Fatal(InputsOf[modules.Hostname](resolved), err)
	}
	if _, err := ResolveArguments(task, value.Scope{"node": map[string]any{"name": "web_1"}}); err == nil || !strings.Contains(err.Error(), "hostname.name") {
		t.Fatal(err)
	}
	alternatives := Task{Module: "update_alternatives", ModuleInputs: &modules.UpdateAlternatives{Name: "editor", ProgramPath: "/usr/bin/../bin/vim"}}
	resolved, err = ResolveArguments(alternatives, value.Scope{})
	if err != nil || InputsOf[modules.UpdateAlternatives](resolved).ProgramPath != "/usr/bin/vim" {
		t.Fatal(InputsOf[modules.UpdateAlternatives](resolved), err)
	}
}
