package config

import (
	"path/filepath"
	"strings"
	"testing"
)

const hardwareStage = `fleet_stages:
- id: collect
  nodes: web
  node_tasks: [{id: hardware, shell: {script: lsblk, changed_when: false}}]
  outputs: {report: '{{ .node_tasks.hardware.stdout }}'}
  save_on_controller_machine:
`

func TestSaveOnControllerMachineLoads(t *testing.T) {
	path := fixture(t, hardwareStage+`  - {output: report, path: 'server_details/{{ .node.name }}.txt', permissions_mode: "0644"}
  - {template: summary.tmpl, path: /tmp/mmx-summary.txt, permissions_mode: "0600"}
`)
	write(t, filepath.Join(filepath.Dir(path), "summary.tmpl"), "{{ range $node, $o := .outputs.collect }}{{ $node }}\n{{ end }}")
	p, _, err := Load(path, "example", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	entries := p.FleetStages[0].SaveOnControllerMachine
	if entries[0].ProjectRoot != filepath.Dir(path) || entries[1].TemplateText == "" {
		t.Fatal(entries)
	}
}

func TestSaveOnControllerMachineRejectsInvalidEntries(t *testing.T) {
	for _, tc := range []struct{ name, entry, field, message string }{
		{"no path", `{output: report, permissions_mode: "0644"}`, "", "path is required"},
		{"no mode", `{output: report, path: 'a/{{ .node.name }}'}`, "", "permissions_mode is required"},
		{"bad mode", `{output: report, path: 'a/{{ .node.name }}', permissions_mode: rw}`, "permissions_mode", "quoted octal"},
		{"both", `{output: report, template: summary.tmpl, path: 'a/{{ .node.name }}', permissions_mode: "0644"}`, "", "exactly one of output"},
		{"neither", `{path: a, permissions_mode: "0644"}`, "", "exactly one of output"},
		{"unknown output", `{output: other, path: 'a/{{ .node.name }}', permissions_mode: "0644"}`, "output", "not one of fleet stage collect's outputs"},
		{"one path for every node", `{output: report, path: a.txt, permissions_mode: "0644"}`, "path", "must contain {{ .node.name }}"},
		{"leaves the project", `{output: report, path: '../{{ .node.name }}/../../a', permissions_mode: "0644"}`, "path", "leaves the project root"},
		{"static path leaves the project", `{template: summary.tmpl, path: ../a.txt, permissions_mode: "0644"}`, "path", "leaves the project root"},
		{"secret in path", `{output: report, path: '{{ .secrets.token }}/{{ .node.name }}', permissions_mode: "0644"}`, "path", "cannot read a secret"},
		{"node value in path", `{output: report, path: '{{ .node.os.id }}/{{ .node.name }}', permissions_mode: "0644"}`, "path", "names a value that is not declared or is declared later"},
		{"node in a template entry's path", `{template: summary.tmpl, path: '{{ .node.name }}', permissions_mode: "0644"}`, "path", "unknown reference root .node"},
		{"missing template", `{template: missing.tmpl, path: a.txt, permissions_mode: "0644"}`, "template", "no such file"},
		{"template reads the node", `{template: node.tmpl, path: a.txt, permissions_mode: "0644"}`, "template", "unknown reference root .node"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := fixture(t, hardwareStage+"  - "+tc.entry+"\n")
			write(t, filepath.Join(filepath.Dir(path), "summary.tmpl"), "{{ .outputs.collect }}")
			write(t, filepath.Join(filepath.Dir(path), "node.tmpl"), "{{ .node.name }}")
			_, _, err := Load(path, "example", "", nil)
			field := "fleet_stages[0].save_on_controller_machine[0]"
			if tc.field != "" {
				field += "." + tc.field
			}
			if err == nil || !strings.Contains(err.Error(), "("+field+")") || !strings.Contains(err.Error(), tc.message) {
				t.Fatalf("want %s: %s, got %v", field, tc.message, err)
			}
		})
	}
	loop := fixture(t, `fleet_stages:
- id: each
  loop: [a]
  as: item
  key: '{{ .item }}'
  fleet_stages:
  - {id: inner, nodes: web, node_tasks: [{show: hi}]}
  save_on_controller_machine:
  - {template: summary.tmpl, path: a.txt, permissions_mode: "0644"}
`)
	if _, _, err := Load(loop, "example", "", nil); err == nil || !strings.Contains(err.Error(), "belongs to a fleet stage that selects nodes") {
		t.Fatal(err)
	}
}

// A template entry reads the fleet stage's whole output collection, so a limit
// that narrows the fleet stage is rejected, as for any reader of a whole
// collection. An output entry writes the kept nodes' files.
func TestSaveOnControllerMachineTemplateNeedsEveryNode(t *testing.T) {
	path := fixture(t, hardwareStage+`  - {template: summary.tmpl, path: a.txt, permissions_mode: "0644"}
`)
	write(t, filepath.Join(filepath.Dir(path), "summary.tmpl"), "{{ len .outputs.collect }}")
	p, inv, err := Load(path, "example", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Inspect(p, inv, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := Inspect(p, inv, []string{"web"}); err != nil {
		t.Fatal(err)
	}
	_, err = Inspect(p, inv, []string{"web1"})
	if err == nil || !strings.Contains(err.Error(), "save_on_controller_machine") || !strings.Contains(err.Error(), "--limit excludes some nodes from fleet stage collect, whose whole output collection fleet stage") {
		t.Fatal(err)
	}
	path = fixture(t, hardwareStage+`  - {output: report, path: 'details/{{ .node.name }}.txt', permissions_mode: "0644"}
`)
	p, inv, err = Load(path, "example", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Inspect(p, inv, []string{"web1"}); err != nil {
		t.Fatal(err)
	}
}

func TestReadFileAndOutputIsSecretConfiguration(t *testing.T) {
	stage := func(task string) string {
		return "fleet_stages:\n- id: s\n  nodes: all\n  node_tasks:\n  - " + task + "\n"
	}
	p, _, err := Load(fixture(t, stage("{id: token, read_file: {path: /var/lib/token, content_is_secret: true}}")+"  outputs: {token: '{{ .node_tasks.token.content }}'}\n"), "example", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if action, _ := p.FleetStages[0].NodeTasks[0].Action(); action.Name != "read_file" || action.RetryByDefault {
		t.Fatal(action)
	}
	if _, _, err := Load(fixture(t, stage("{shell: {script: make-token, output_is_secret: true}}")), "example", "", nil); err != nil {
		t.Fatal(err)
	}
	for task, message := range map[string]string{
		"{read_file: {path: /var/lib/token}}":                             "content_is_secret is required",
		"{read_file: {content_is_secret: false}}":                         "read_file.path is required",
		"{read_file: {path: relative/token, content_is_secret: false}}":   "path must be absolute",
		"{read_file: {path: /a, content_is_secret: false, mode: '0600'}}": "read_file.mode): unknown field",
	} {
		if _, _, err := Load(fixture(t, stage(task)), "example", "", nil); err == nil || !strings.Contains(err.Error(), message) {
			t.Fatal(task, err)
		}
	}
}

// A path that reads an output visibly holding a secret is refused at load.
func TestSaveOnControllerMachinePathRefusesAVisiblySecretOutput(t *testing.T) {
	for _, outputs := range []string{
		"{token: '{{ .node_tasks.hardware.stdout }}'}",
		"{token: '{{ .secrets.token }}'}",
	} {
		secret := "  node_tasks: [{id: hardware, shell: {script: make-token, output_is_secret: true}}]\n"
		path := fixture(t, "fleet_stages:\n- id: make\n  nodes: web\n"+secret+"  outputs: "+outputs+"\n"+
			"- id: collect\n  nodes: web\n  node_tasks: [{show: hi}]\n  outputs: {report: hi}\n  save_on_controller_machine:\n"+
			"  - {output: report, path: 'details/{{ .outputs.make.web1.token }}/{{ .node.name }}', permissions_mode: \"0644\"}\n")
		_, _, err := Load(path, "example", "", nil)
		if err == nil || !strings.Contains(err.Error(), "(fleet_stages[1].save_on_controller_machine[0].path)") || !strings.Contains(err.Error(), "cannot read a secret") {
			t.Fatal(outputs, err)
		}
	}
}

func TestOutputHoldsNoSecretConfiguration(t *testing.T) {
	for _, task := range []string{
		"{shell: {script: check-settings, output_holds_no_secret: true, output_is_secret: true}}",
		"{file: {path: /etc/myapp.conf, content: example, check_new_file: {shell: {script: check-settings, output_holds_no_secret: true, output_is_secret: true}}}}",
	} {
		playbook := "fleet_stages:\n- id: settings\n  nodes: all\n  node_tasks:\n  - " + task + "\n"
		_, _, err := Load(fixture(t, playbook), "example", "", nil)
		if err == nil || !strings.Contains(err.Error(), "output_holds_no_secret") || !strings.Contains(err.Error(), "output_is_secret") {
			t.Fatalf("contradictory command output declarations accepted: %v", err)
		}
	}
}
