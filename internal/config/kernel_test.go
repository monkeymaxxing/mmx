package config

import (
	"encoding/json"
	"strings"
	"testing"

	"mmx/internal/modules"
	"mmx/internal/value"
)

func TestKernelSettingsAndModulesConfiguration(t *testing.T) {
	for _, tasks := range []string{
		// The design's example: a mapping loop gives each setting's name and value.
		`{kernel_module: {name: br_netfilter}}, {id: kernel_settings, loop: {net.ipv4.ip_forward: "1", net.bridge.bridge-nf-call-iptables: "1"}, as: setting, sysctl_setting: {name: "{{ .setting.key }}", value: "{{ .setting.value }}"}}`,
		`{sysctl_setting: {name: net/ipv4/conf/eth0.10/rp_filter, value: "2"}}`,
		`{sysctl_setting: {name: net.ipv4.ip_local_port_range, value: "32768 60999"}}`,
		`{sysctl_setting: {name: net.ipv4.ip_forward, state: boot_setting_removed_live_value_kept}}`,
		`{kernel_module: {name: nf_conntrack, options: {hashsize: "65536", nf_conntrack_helper: "Y"}}}`,
		`{kernel_module: {name: "{{ .vars.module }}", options: {hashsize: "{{ .vars.size }}"}}}`,
		`{kernel_module: {name: br_netfilter, state: unloaded}}`,
	} {
		p := fixture(t, "vars: {module: nf_conntrack, size: \"65536\"}\nfleet_stages: [{id: configure, nodes: all, node_tasks: ["+tasks+"]}]\n")
		project, _, err := Load(p, "example", "", nil)
		if err != nil {
			t.Fatal(tasks, err)
		}
		if loaded := project.FleetStages[0].NodeTasks[0]; loaded.Retry == nil || loaded.Retry.Disabled {
			t.Fatal("resource did not inherit retry policy", tasks)
		}
	}
	for task, want := range map[string]string{
		`sysctl_setting: {value: "1"}`:                                                                         "sysctl_setting.name is required",
		`sysctl_setting: {name: net.ipv4.ip_forward}`:                                                          "sysctl_setting.value is required",
		`sysctl_setting: {name: net.ipv4.ip_forward, value: "1", state: gone}`:                                 "state must be present or boot_setting_removed_live_value_kept",
		`sysctl_setting: {name: net.ipv4.ip_forward, value: "1", state: boot_setting_removed_live_value_kept}`: "boot_setting_removed_live_value_kept sysctl_setting accepts only name and state",
		`sysctl_setting: {name: /proc/sys/net/ipv4/ip_forward, value: "1"}`:                                    "sysctl_setting.name must name a kernel setting",
		`sysctl_setting: {name: net.ipv4/conf/eth0.10/rp_filter, value: "1"}`:                                  "separate all name components with slashes",
		`sysctl_setting: {name: net.ipv4.ip_forward, value: "1\n2"}`:                                           "value must be one line of text",
		`sysctl_setting: {name: net.ipv4.ip_forward, value: "1", other: x}`:                                    "sysctl_setting.other): unknown field",
		`sysctl_setting: {name: net.ipv4.ip_forward, value: 1}`:                                                "expected string, got !!int",
		`kernel_module: {name: vxlan, options: {"{{ .vars.x }}": "1"}}`:                                        "must be an option name",
		`kernel_module: {options: {hashsize: "1"}}`:                                                            "kernel_module.name is required",
		`kernel_module: {name: "br netfilter"}`:                                                                "name must be a kernel module's name",
		`kernel_module: {name: vxlan, state: absent}`:                                                          "kernel_module.state must be loaded or unloaded",
		`kernel_module: {name: vxlan, state: unloaded, options: {udp_port: "1"}}`:                              "accepts only name and state",
		`kernel_module: {name: vxlan, options: {udp_port: "a b"}}`:                                             "one word without spaces or quotes",
		`kernel_module: {name: vxlan, options: {udp_port: }}`:                                                  "needs a value",
		`kernel_module: {name: vxlan, options: {"udp port": "1"}}`:                                             "must be an option name",
	} {
		p := fixture(t, "fleet_stages: [{id: configure, nodes: all, node_tasks: [{"+task+"}]}]\n")
		if _, _, err := Load(p, "example", "", nil); err == nil || !strings.Contains(err.Error(), want) {
			t.Error(task, err)
		}
	}
	// References resolve with the same rules, and the definition keeps them.
	scope := value.Scope{"vars": map[string]any{"name": "net.ipv4.ip_forward", "size": "65536"}}
	setting := Task{Module: "sysctl_setting", ModuleInputs: &modules.SysctlSetting{Name: "{{ .vars.name }}", Value: "{{ .vars.size }}"}}
	resolved, err := ResolveArguments(setting, scope)
	if err != nil || *InputsOf[modules.SysctlSetting](resolved) != (modules.SysctlSetting{Name: "net.ipv4.ip_forward", Value: "65536"}) || InputsOf[modules.SysctlSetting](setting).Name != "{{ .vars.name }}" {
		t.Fatal(InputsOf[modules.SysctlSetting](resolved), err)
	}
	size := "{{ .vars.size }}"
	module := Task{Module: "kernel_module", ModuleInputs: &modules.KernelModule{Name: "nf_conntrack", Options: map[string]*string{"hashsize": &size}}}
	resolved, err = ResolveArguments(module, scope)
	if err != nil || *InputsOf[modules.KernelModule](resolved).Options["hashsize"] != "65536" || size != "{{ .vars.size }}" {
		t.Fatal(err, size)
	}
	if _, err := ResolveArguments(module, value.Scope{"vars": map[string]any{"size": "1 2"}}); err == nil || !strings.Contains(err.Error(), "kernel_module.options.hashsize") {
		t.Fatal(err)
	}
	if _, err := ResolveArguments(setting, value.Scope{"vars": map[string]any{"name": "net..ipv4", "size": "1"}}); err == nil || !strings.Contains(err.Error(), "sysctl_setting.name") {
		t.Fatal(err)
	}
}

func TestKernelModuleEmptyOptionsSurviveSavedTask(t *testing.T) {
	for _, written := range []map[string]*string{nil, {}} {
		task := Task{Module: "kernel_module", ModuleInputs: &modules.KernelModule{Name: "vxlan", Options: written}}
		encoded, err := json.Marshal(task)
		if err != nil {
			t.Fatal(err)
		}
		var saved Task
		if err := json.Unmarshal(encoded, &saved); err != nil {
			t.Fatal(err)
		}
		if (InputsOf[modules.KernelModule](saved).Options == nil) != (written == nil) {
			t.Fatalf("options lost in %s", encoded)
		}
	}
}
