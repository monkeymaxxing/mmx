package config

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestLoopAndOutputDeclarations(t *testing.T) {
	valid := `fleet_stages:
- id: inspect
  nodes: web
  node_tasks: [{id: ready, show: true}]
  outputs: {ready: "{{ .node_tasks.ready.value }}"}
- id: rollout
  loop: [web2, web1]
  as: app
  key: "{{ .app }}"
  fleet_stages:
  - id: check
    nodes: "{{ .app }}"
    when: "{{ .outputs.inspect[.app].ready }}"
    node_tasks:
    - id: role_call
      role: sample
      inputs: {ready: "{{ .outputs.inspect[.app].ready }}"}
    outputs: {ready: "{{ .node_tasks.role_call.outputs.ready }}"}
  outputs: {ready: "{{ .outputs.check[.app].ready }}"}
- id: report
  nodes: web1
  node_tasks: [{show: "{{ .outputs.rollout.web1.ready }}"}]
`
	path := fixture(t, valid)
	write(t, filepath.Join(filepath.Dir(path), "roles/sample/main.yml"), "inputs: {ready: {type: boolean, required: true}}\nnode_tasks: [{id: private, show: '{{ .inputs.ready }}'}]\noutputs: {ready: '{{ .node_tasks.private.value }}'}\n")
	if _, _, err := Load(path, "example", "", nil); err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"scalar loop":  strings.Replace(valid, "loop: [web2, web1]", "loop: 42", 1),
		"string loop":  strings.Replace(valid, "loop: [web2, web1]", "loop: web1", 1),
		"output typo":  strings.Replace(valid, ".outputs.rollout.web1.ready", ".outputs.rollout.web1.typo", 1),
		"node typo":    strings.Replace(valid, ".outputs.inspect[.app].ready", ".outputs.inspect.missing.ready", 1),
		"forward":      strings.Replace(valid, ".outputs.inspect[.app].ready", ".outputs.report.web1.ready", 1),
		"private":      strings.Replace(valid, ".node_tasks.role_call.outputs.ready", ".node_tasks.private.value", 1),
		"role typo":    strings.Replace(valid, ".node_tasks.role_call.outputs.ready", ".node_tasks.role_call.outputs.typo", 1),
		"shadow":       strings.Replace(valid, "as: app", "as: node", 1),
		"no key":       strings.Replace(valid, "  key: \"{{ .app }}\"\n", "", 1),
		"inner leak":   strings.Replace(valid, ".outputs.rollout.web1.ready", ".outputs.check.web1.ready", 1),
		"hidden error": strings.Replace(valid, "  node_tasks: [{show: \"{{ .outputs.rollout.web1.ready }}\"}]", "  when: false\n  node_tasks: [{show: \"{{ .outputs.rollout.web1.typo }}\"}]", 1),
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			write(t, path, "project_name: test\ndefault_inventory: example\n"+body)
			if _, _, err := Load(path, "example", "", nil); err == nil {
				t.Fatal("accepted invalid reference")
			}
		})
	}
}

// An error in a value read from a loop item names the iteration that failed.
func TestLoopValueErrorsNameTheIteration(t *testing.T) {
	vars := "vars:\n  paths: [/etc/a, relative/b]\n  files: {a: /etc/a, b: relative/b}\n"
	task := func(loop, path string) string {
		return "  node_tasks:\n  - file: {permissions_mode: '0644', path: '" + path + "', content: x}\n    loop: " + loop + "\n    as: p\n"
	}
	for name, tc := range map[string]struct{ body, iteration string }{
		"list":        {"- id: setup\n  nodes: all\n" + task("'{{ .vars.paths }}'", "{{ .p }}"), "setup/task_1[1]"},
		"mapping":     {"- id: setup\n  nodes: all\n" + task("'{{ .vars.files }}'", "{{ .p.value }}"), "setup/task_1[b]"},
		"literal":     {"- id: setup\n  nodes: all\n" + task("[/etc/a, relative/b]", "{{ .p }}"), "setup/task_1[1]"},
		"fleet_stage": {"- id: rollout\n  loop: '{{ .vars.paths }}'\n  as: p\n  key: '{{ .p }}'\n  fleet_stages:\n  - id: copy\n    nodes: all\n    node_tasks: [{file: {permissions_mode: '0644', path: '{{ .p }}', content: x}}]\n", "rollout[relative%2Fb]/copy"},
	} {
		_, _, err := Load(fixture(t, vars+"fleet_stages:\n"+tc.body), "example", "", nil)
		for _, want := range []string{"path must be absolute", tc.iteration} {
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Errorf("%s: want %s: %v", name, want, err)
			}
		}
	}
}

func TestTemplateRootReferencesAreValidatedInsideLoops(t *testing.T) {
	d := declarations{"outputs": {"inspect": true}, "outputs.inspect": {"port": true}, "nodes.inspect": {"web1": true}}
	for _, source := range []string{`{{ range .outputs.missing }}{{ .port }}{{ end }}`, `{{ range .outputs.inspect }}{{ $.outputs.inspect.web1.typo }}{{ end }}`} {
		if err := checkTemplate(source, d); err == nil {
			t.Fatal("accepted", source)
		}
	}
	if err := checkTemplate(`{{ range .outputs.inspect }}{{ .port }}{{ end }}`, d); err != nil {
		t.Fatal(err)
	}
}

func TestConcreteFleetStagePaths(t *testing.T) {
	for _, path := range []string{"setup", "rollout[app01]/update", "outer[a%2Fb]/inner[%E2%98%83]/check"} {
		if !ValidFleetStagePath(path) {
			t.Fatal("rejected", path)
		}
	}
	for _, path := range []string{"../setup", "rollout[]/update", "rollout[a/b]/update", "rollout[a]/../update", "rollout[a]/", "rollout[a]", "rollout[%0x]/fleet_stage", "rollout[a]\n/fleet_stage"} {
		if ValidFleetStagePath(path) {
			t.Fatal("accepted", path)
		}
	}
}

func TestFleetStageCanPublishFactsWithoutNodeTasks(t *testing.T) {
	if _, _, err := Load(fixture(t, "fleet_stages: [{id: facts, nodes: web, outputs: {name: '{{ .node.name }}'}}]\n"), "example", "", nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Load(fixture(t, "fleet_stages: [{id: empty, nodes: web}]\n"), "example", "", nil); err == nil {
		t.Fatal("accepted empty fleet_stage")
	}
}
