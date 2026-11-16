package config

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestNodeTaskResultFieldsAreCheckedBeforeDispatch(t *testing.T) {
	cases := map[string]bool{
		".node_tasks.check.exitcode":                       false,
		".node_tasks.check.items[0].exitcode":              false,
		".node_tasks.check.items[0].items[1].exitcode":     false,
		".node_tasks.check.items[.vars.position].exitcode": false,
		".node_tasks[.vars.task].exitcode":                 false,
		".node_tasks.check[.vars.field]":                   true,
		".node_tasks.check.items[0][.vars.field]":          true,
		".node_tasks.check.items[.vars.position].stdout":   true,
		".node_tasks[.vars.task].stdout":                   true,
		".node_tasks.check.outputs.custom":                 true,
		".node_tasks.check.items[0].outputs.custom":        true,
		".node_tasks.check.value.custom":                   true,
	}
	// The complete field set from protocol.ResultValue, including optional fields.
	for _, field := range []string{"outputs", "items", "iteration_key", "status", "change", "exit_code", "stdout", "stdout_json", "stderr", "reason", "http_status_code", "response_body", "response_body_json", "value"} {
		cases[".node_tasks.check."+field] = true
		cases[".node_tasks.check.items[0]."+field] = true
	}
	for reference, valid := range cases {
		t.Run(reference, func(t *testing.T) {
			path := fixture(t, "vars: {task: check, field: exitcode, position: 0}\nfleet_stages:\n- id: setup\n  nodes: all\n  node_tasks:\n  - id: check\n    shell: {script: 'true', changed_when: false}\n  - show: '{{ "+reference+" }}'\n")
			_, _, err := Load(path, "example", "", nil)
			if valid {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), reference) || !strings.Contains(err.Error(), "valid fields:") || !strings.Contains(err.Error(), "exit_code") || !strings.Contains(err.Error(), "node_tasks[1].show") {
				t.Fatalf("expected result field error naming the path, valid fields, and source; got %v", err)
			}
		})
	}
}

func TestStaticReferencesAndPredicatesFailBeforeDispatch(t *testing.T) {
	for _, nodeTasks := range []string{
		"[{show: first}, {id: task_1, show: second}]",
		"[{show: '{{ .node_tasks.later.stdout }}'}, {id: later, show: hi}]",
		"[{show: '{{ .vars.missing }}', when: false}]",
		"[{show: ok, when: {magic: true}}]",
		"[{show: ok, when: 'false'}]",
		"[{set_vars: {x: '{{ .vars.x }}'}}]",
		"[{show: ok, inputs: {x: value}}]",
		"[{task_block: {id: same , changes: [{show: hi}]}}, {task_block: {id: same , changes: [{show: hi}]}}]",
	} {
		path := fixture(t, "fleet_stages:\n- id: setup\n  nodes: all\n  node_tasks: "+nodeTasks+"\n")
		if _, _, err := Load(path, "example", "", nil); err == nil {
			t.Fatal("accepted", nodeTasks)
		}
	}
}

// Number and version comparisons are conditions everywhere; path_exists
// inspects the node and is accepted only in a node task's or task block's when.
func TestComparisonAndPathConditionsAreCheckedBeforeDispatch(t *testing.T) {
	stage := func(nodeTasks string) string {
		return "fleet_stages:\n- id: setup\n  nodes: all\n  node_tasks: " + nodeTasks + "\n"
	}
	for source, message := range map[string]string{
		stage(`[{id: lag, shell: {script: lag, changed_when: false}}, {show: ok, when: {number_at_most: ["{{ .node_tasks.lag.stdout }}", 30]}}]`):         "",
		stage(`[{id: lag, shell: {script: lag, changed_when: false}}, {assert: {that: {version_at_least: ["{{ .node_tasks.lag.stdout }}", "1.30.2"]}}}]`): "",
		stage(`[{id: lag, shell: {script: lag, changed_when: false}}, {set_vars: {slow: {number_greater_than: ["{{ .node_tasks.lag.stdout }}", 30]}}}]`):  "",
		stage(`[{id: status, shell: {script: status, changed_when: false}}, {show: ok, when: "{{ .node_tasks.status.stdout_json.active }}"}]`):            "",
		stage(`[{show: ok, when: {path_exists: /etc/app/installed}}]`):                                                                                    "",
		stage(`[{show: ok, when: {not: {path_exists: "{{ .vars.marker }}"}}}]`):                                                                           "reference .vars.marker is not declared for node web1",
		stage(`[{task_block: {id: app, changes: [{show: ok}]}, when: {all: [{path_exists: /etc/app}, true]}}]`):                                           "",
		stage(`[{show: ok, when: {path_exists: etc/app}}]`):                                                                                               "path_exists requires an absolute path",
		stage(`[{show: ok, when: {path_exists: [/etc/app]}}]`):                                                                                            "path_exists requires an absolute path",
		stage(`[{show: ok, when: {number_at_most: [1]}}]`):                                                                                                "number_at_most requires two values",
		stage(`[{show: ok, when: {number_at_most: [[1], 2]}}]`):                                                                                           "compares numbers or text",
		stage(`[{show: ok, when: {version_newer: ["1", "2"]}}]`):                                                                                          `unknown predicate "version_newer"`,
		stage(`[{shell: {script: "true", changed_when: {path_exists: /etc/app}}}]`):                                                                       "allowed only in a node task's or node task block's when condition",
		stage(`[{assert: {that: {path_exists: /etc/app}}}]`):                                                                                              "allowed only in a node task's or node task block's when condition",
		"fleet_stages:\n- id: setup\n  nodes: all\n  when: {path_exists: /etc/app}\n  node_tasks: [{show: ok}]\n":                                         "allowed only in a node task's or node task block's when condition",
	} {
		_, _, err := Load(fixture(t, source), "example", "", nil)
		if message == "" && err != nil || message != "" && (err == nil || !strings.Contains(err.Error(), message)) {
			t.Errorf("%s\nwant %q, got %v", source, message, err)
		}
	}
}

// A literal {{ for another tool is written in back quotes inside braces; a {{
// MMX cannot read is an error that names this form.
func TestLiteralBracesInTaskValues(t *testing.T) {
	stage := func(content string) string {
		return "vars: {image: app}\nfleet_stages:\n- id: setup\n  nodes: all\n  node_tasks:\n  - systemd_unit:\n      name: app.service\n      content: |\n        [Service]\n        ExecStart=/usr/bin/docker run " + content + "\n"
	}
	if _, _, err := Load(fixture(t, stage("--log-opt tag={{`{{.Name}}`}} {{ .vars.image }}")), "example", "", nil); err != nil {
		t.Fatal(err)
	}
	for _, content := range []string{"--log-opt tag={{.Name}} app", "--log-opt tag={{ {{.Name}} }} app", "--format {{json .}} app"} {
		_, _, err := Load(fixture(t, stage(content)), "example", "", nil)
		if err == nil || !strings.Contains(err.Error(), "{{`{{.Name}}`}}") || !strings.Contains(err.Error(), "systemd_unit.content") {
			t.Errorf("%s: %v", content, err)
		}
	}
}

func TestTemplateReferencesAreCheckedThroughChains(t *testing.T) {
	for source, valid := range map[string]bool{
		"{{ (.vars).port }}":          true,
		"{{ (.vars).nope }}":          false,
		"{{ ((.vars).nope).deeper }}": false,
		// A template call reads its argument; the called body sees only that value.
		`{{ define "x" }}{{ . }}{{ end }}{{ template "x" .vars.port }}`: true,
		`{{ define "x" }}{{ . }}{{ end }}{{ template "x" .vars.nope }}`: false,
		`{{ define "x" }}{{ .port }}{{ end }}{{ template "x" .vars }}`:  true,
		`{{ define "x" }}{{ .vars.port }}{{ end }}{{ template "x" . }}`: false,
		"{{ . }}":                           false,
		"{{ range . }}{{ . }}{{ end }}":     false,
		"{{ range .vars }}{{ . }}{{ end }}": true,
	} {
		path := fixture(t, "vars: {port: 80}\nfleet_stages: [{id: setup, nodes: all, node_tasks: [{file: {permissions_mode: '0644', path: /etc/app, template: app.tmpl}}]}]\n")
		write(t, filepath.Join(filepath.Dir(path), "app.tmpl"), source)
		if _, _, err := Load(path, "example", "", nil); (err == nil) != valid {
			t.Fatal(source, err)
		}
	}
}

// The engine shares vars across an task block's stages, so later node tasks may read
// an assignment made in cleanup or on_failure.
func TestTaskBlockStageAssignmentsReachLaterNodeTasks(t *testing.T) {
	for _, stage := range []string{"verify", "on_failure", "cleanup"} {
		path := fixture(t, "fleet_stages:\n- id: setup\n  nodes: all\n  node_tasks:\n  - task_block:\n      id: app\n      changes: [{show: change}]\n      "+(stage)+": [{set_vars: {marker: done}}]\n  - show: \"{{ .vars.marker }}\"\n")
		if _, _, err := Load(path, "example", "", nil); err != nil {
			t.Fatal(stage, err)
		}
	}
}
func TestScriptTextIsLiteralAndResultReferencesAreExplicit(t *testing.T) {
	path := fixture(t, `fleet_stages:
- id: setup
  nodes: all
  node_tasks:
  - id: inspect
    shell:
      script: 'echo "{{ not interpreted }}"'
      changed_when: false
  - set_vars: {output: "{{ .node_tasks.inspect.stdout }}"}
  - show: "{{ .vars.output }}"
    when: {equals: ["{{ .node.arch }}", arm64]}
`)
	if _, _, err := Load(path, "example", "", nil); err != nil {
		t.Fatal(err)
	}
}

// set_vars predicate forms are checked as predicates and assign booleans, as
// the engine evaluates them; any other value is data.
func TestSetVarsPredicatesAreBooleans(t *testing.T) {
	path := fixture(t, `vars: {x: 1, name: web}
fleet_stages:
- id: setup
  nodes: all
  node_tasks:
  - set_vars:
      flag: {all: [{equals: ["{{ .vars.x }}", 1]}, {not: {matches: ["{{ .vars.name }}", "^db"]}}]}
      runtime: {equals: ["{{ .node.arch }}", arm64]}
  - show: ok
    when: "{{ .vars.flag }}"
  - show: "{{ .vars.flag }}"
  - show: later
    when: "{{ .vars.runtime }}"
`)
	p, inv, err := Load(path, "example", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	view, err := Inspect(p, inv, []string{"web1"})
	if err != nil {
		t.Fatal(err)
	}
	nodeTasks := view.FleetStages[0].NodeTasks
	if nodeTasks[1].Status != "ready" || nodeTasks[2].Arguments[0].Value != true || nodeTasks[3].Status != "conditional" {
		t.Fatalf("%+v", nodeTasks)
	}
	for field, text := range map[string]string{
		"set_vars.flag": "set_vars: {flag: {equals: [1]}}",
		"set_vars.bad":  "set_vars: {bad: {any: [{magic: 1}]}}",
		"set_vars.re":   "set_vars: {re: {matches: [text, '(']}}",
	} {
		path := fixture(t, "fleet_stages:\n- id: setup\n  nodes: all\n  node_tasks:\n  - "+text+"\n")
		if _, _, err := Load(path, "example", "", nil); err == nil || !strings.Contains(err.Error(), "playbooks/example.yml:") || !strings.Contains(err.Error(), field) {
			t.Errorf("%s: %v", text, err)
		}
	}
	// path_exists inspects the node, so only a when takes it: at the top of
	// a set_vars or outputs value it is refused as it is inside all.
	for field, fleetStage := range map[string]string{
		"set_vars.exists":        "node_tasks:\n  - set_vars: {exists: {path_exists: /etc/app.conf}}\n",
		"set_vars.nested":        "node_tasks:\n  - set_vars: {nested: {all: [{path_exists: /etc/app.conf}]}}\n",
		"outputs.config_present": "node_tasks:\n  - show: ok\n  outputs: {config_present: {path_exists: /etc/app.conf}}\n",
	} {
		path := fixture(t, "fleet_stages:\n- id: setup\n  nodes: all\n  "+fleetStage)
		if _, _, err := Load(path, "example", "", nil); err == nil || !strings.Contains(err.Error(), field) || !strings.Contains(err.Error(), "path_exists checks a path on the node") {
			t.Errorf("%s: %v", field, err)
		}
	}
}

// assert.that is checked like when: locally known values before any node, a
// runtime value when it runs. Explain shows the condition as written and the
// resolved message.
func TestAssertIsCheckedLikeACondition(t *testing.T) {
	path := fixture(t, `vars: {name: web}
fleet_stages:
- id: setup
  nodes: all
  node_tasks:
  - assert:
      that: {equals: ["{{ .node.os.id }}", debian]}
      message: "{{ .vars.name }} needs Debian"
`)
	p, inv, err := Load(path, "example", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	view, err := Inspect(p, inv, []string{"web1"})
	if err != nil {
		t.Fatal(err)
	}
	task := view.FleetStages[0].NodeTasks[0]
	args := map[string]Argument{}
	for _, a := range task.Arguments {
		args[a.Field] = a
	}
	if task.Action != "assert" || task.Status != "ready" || args["assert.that"].Status != "condition" || args["assert.that"].Reference == nil || args["assert.message"].Value != "web needs Debian" {
		t.Fatalf("%+v", task)
	}
	_, _, err = Load(fixture(t, "vars: {name: web9}\nfleet_stages:\n- id: setup\n  nodes: all\n  node_tasks: [{assert: {that: '{{ .vars.name }}'}}]\n"), "example", "", nil)
	if err == nil || !strings.Contains(err.Error(), "assert.that") || !strings.Contains(err.Error(), "value supplied at mmx_project_settings.yml:3:14 (vars.name)") {
		t.Fatal(err)
	}
}

// A looped role call's outputs belong to its iterations.
func TestLoopedRoleOutputsAreReadThroughItems(t *testing.T) {
	project := `fleet_stages:
- id: setup
  nodes: all
  node_tasks:
  - id: each
    role: app
    loop: [a, b]
    as: item
  - show: "{{ .node_tasks.each.OUTPUT.name }}"
`
	path := fixture(t, strings.Replace(project, "OUTPUT", "outputs", 1))
	write(t, filepath.Join(filepath.Dir(path), "roles/app/main.yml"), "node_tasks: [{show: hi}]\noutputs: {name: fixed}\n")
	_, _, err := Load(path, "example", "", nil)
	if err == nil || !strings.Contains(err.Error(), ".node_tasks.each.items[n].outputs") || !strings.Contains(err.Error(), "node_tasks[1].show") {
		t.Fatal(err)
	}
	write(t, path, "project_name: test\ndefault_inventory: example\n"+strings.Replace(project, "OUTPUT", "items[1].outputs", 1))
	if _, _, err := Load(path, "example", "", nil); err != nil {
		t.Fatal(err)
	}
}

// A secret cannot be the selector of another value: a node gets a secret only
// as a value, so such a reference would block its node job in every run.
func TestASecretCannotSelectAnotherValue(t *testing.T) {
	const advice = "Read the secret directly as {{ .secrets.which_port }}, or use a non-secret value, such as an inventory variable, to select the value on the controller machine"
	project := func(nodeTasks, rest string) string {
		return "vars: {ports: {web: 443}, tables: {a: {web: 1}, web1: {'443': 1}, web2: {'443': 2}}}\nfleet_stages:\n- id: setup\n  nodes: all\n  node_tasks:\n" + nodeTasks + rest
	}
	for name, tc := range map[string]struct{ project, want string }{
		"a module input": {project("  - show: '{{ .vars.ports[.secrets.which_port] }}'\n", ""),
			"playbooks/example.yml:8:11 (fleet_stages[0].node_tasks[0].show): .vars.ports[.secrets.which_port] uses the secret .secrets.which_port as a selector, but a secret cannot select another value. " + advice},
		"inside other text": {project("  - file: {path: /etc/app.conf, permissions_mode: '0600', content: 'port={{ .vars.ports[.secrets.which_port] }}'}\n", ""),
			"(fleet_stages[0].node_tasks[0].file.content): .vars.ports[.secrets.which_port] uses the secret .secrets.which_port as a selector, but a secret cannot select another value. " + advice},
		"a selector inside a selector": {project("  - show: '{{ .vars.tables[.vars.ports[.secrets.which_port]].web }}'\n", ""),
			"(fleet_stages[0].node_tasks[0].show): .vars.ports[.secrets.which_port] uses the secret .secrets.which_port as a selector, but a secret cannot select another value. " + advice},
		"a selector that selects inside a secret": {project("  - show: '{{ .vars.ports[.secrets.which_port[.node.name]] }}'\n", ""),
			"(fleet_stages[0].node_tasks[0].show): .vars.ports[.secrets.which_port[.node.name]] uses the secret .secrets.which_port[.node.name] as a selector, but a secret cannot select another value. Read the secret directly as {{ .secrets.which_port[.node.name] }}, or use a non-secret value"},
		"a condition": {project("  - show: hello\n    when: {equals: ['{{ .vars.ports[.secrets.which_port] }}', 443]}\n", ""),
			"(fleet_stages[0].node_tasks[0].when): .vars.ports[.secrets.which_port] uses the secret .secrets.which_port as a selector, but a secret cannot select another value. " + advice},
		"a variable assignment": {project("  - set_vars: {port: '{{ .vars.ports[.secrets.which_port] }}'}\n", ""),
			"(fleet_stages[0].node_tasks[0].set_vars.port): .vars.ports[.secrets.which_port] uses the secret .secrets.which_port as a selector, but a secret cannot select another value. " + advice},
		"a shell environment": {project("  - shell: {script: 'true', env: {PORT: '{{ .vars.ports[.secrets.which_port] }}'}}\n", ""),
			"(fleet_stages[0].node_tasks[0].shell.env.PORT): .vars.ports[.secrets.which_port] uses the secret .secrets.which_port as a selector, but a secret cannot select another value. " + advice},
		"a loop": {project("  - show: hello\n    loop: '{{ .vars.tables[.secrets.which_port] }}'\n    as: entry\n", ""),
			".vars.tables[.secrets.which_port] uses the secret .secrets.which_port as a selector, but a secret cannot select another value. " + advice},
		"an output of a fleet stage": {project("  - show: hello\n", "  outputs:\n    port: '{{ .vars.ports[.secrets.which_port] }}'\n"),
			".vars.ports[.secrets.which_port] uses the secret .secrets.which_port as a selector, but a secret cannot select another value. " + advice},
		"the condition of a fleet stage": {project("  - show: hello\n", "  when: {equals: ['{{ .vars.ports[.secrets.which_port] }}', 443]}\n"),
			"(fleet_stages[0].when): .vars.ports[.secrets.which_port] uses the secret .secrets.which_port as a selector, but a secret cannot select another value. " + advice},
	} {
		t.Run(name, func(t *testing.T) {
			_, _, err := Load(fixture(t, tc.project), "example", "", nil)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v\nwant %s", err, tc.want)
			}
		})
	}
	// A template file reads values with the same rule, also through index.
	for _, text := range []string{"port={{ index .vars.ports .secrets.which_port }}\n", "port={{ (index .vars.tables .secrets.which_port).web }}\n"} {
		path := fixture(t, project("  - file: {path: /etc/app.conf, permissions_mode: '0600', template: app.conf.tmpl}\n", ""))
		write(t, filepath.Join(filepath.Dir(path), "app.conf.tmpl"), text)
		if _, _, err := Load(path, "example", "", nil); err == nil || !strings.Contains(err.Error(), "(fleet_stages[0].node_tasks[0].file.template): ") ||
			!strings.Contains(err.Error(), "uses the secret .secrets.which_port as a selector, but a secret cannot select another value. "+advice) {
			t.Fatalf("%s: %v", text, err)
		}
	}
	// A secret is still read as a value, a selector may still choose inside
	// a secret, and a value that is no secret may still select.
	for _, nodeTasks := range []string{
		"  - show: '{{ .secrets.which_port }}'\n",
		"  - show: '{{ .secrets.passwords[.node.name] }}'\n",
		"  - show: '{{ .secrets.passwords[.vars.ports.web].value }}'\n",
		"  - show: '{{ .vars.tables[.node.name][.vars.ports.web] }}'\n",
	} {
		if _, _, err := Load(fixture(t, project(nodeTasks, "")), "example", "", nil); err != nil {
			t.Fatal(nodeTasks, err)
		}
	}
}

// Inside a role, an input declared secret: true holds a secret, so it cannot
// select another value either.
func TestASecretRoleInputCannotSelectAnotherValue(t *testing.T) {
	const advice = "Read the input directly as {{ .inputs.which_port }}, or select with a role input that is not declared secret: true, and have the role call set the input from a value that is no secret, such as an inventory variable"
	const refused = ".inputs.ports[.inputs.which_port] uses the role input .inputs.which_port as a selector, but the input is declared secret: true, and a secret cannot select another value. " + advice
	load := func(t *testing.T, roleBody, template string) error {
		t.Helper()
		path := fixture(t, "fleet_stages:\n- id: setup\n  nodes: all\n  node_tasks:\n  - role: app\n    inputs: {ports: {web: 443}, tables: {a: {web: web}}, which_port: '{{ .secrets.which_port }}', name: web}\n")
		base := filepath.Dir(path)
		write(t, filepath.Join(base, "roles/app/main.yml"), "inputs:\n  ports: {type: mapping, required: true}\n  tables: {type: mapping, required: true}\n  which_port: {type: string, secret: true, required: true}\n  name: {type: string, required: true}\n"+roleBody)
		if template != "" {
			write(t, filepath.Join(base, "roles/app/app.conf.tmpl"), template)
		}
		_, _, err := Load(path, "example", "", nil)
		return err
	}
	for name, tc := range map[string]struct{ roleBody, want string }{
		"a module input": {"node_tasks:\n- show: '{{ .inputs.ports[.inputs.which_port] }}'\n",
			"(node_tasks[0].show): " + refused},
		"inside other text": {"node_tasks:\n- file: {path: /etc/app.conf, permissions_mode: '0600', content: 'port={{ .inputs.ports[.inputs.which_port] }}'}\n",
			"(node_tasks[0].file.content): " + refused},
		"a selector inside a selector": {"node_tasks:\n- show: '{{ .inputs.tables[.inputs.ports[.inputs.which_port]].web }}'\n",
			"(node_tasks[0].show): " + refused},
		"a selector that selects inside the input": {"node_tasks:\n- show: '{{ .inputs.ports[.inputs.which_port[.node.name]] }}'\n",
			"(node_tasks[0].show): .inputs.ports[.inputs.which_port[.node.name]] uses the role input .inputs.which_port as a selector, but the input is declared secret: true, and a secret cannot select another value. Read the input directly as {{ .inputs.which_port[.node.name] }}, or select with a role input"},
		"a condition": {"node_tasks:\n- show: hello\n  when: {equals: ['{{ .inputs.ports[.inputs.which_port] }}', 443]}\n",
			"(node_tasks[0].when): " + refused},
		"a variable assignment": {"node_tasks:\n- set_vars: {port: '{{ .inputs.ports[.inputs.which_port] }}'}\n",
			"(node_tasks[0].set_vars.port): " + refused},
		"a shell environment": {"node_tasks:\n- shell: {script: 'true', env: {PORT: '{{ .inputs.ports[.inputs.which_port] }}'}}\n",
			"(node_tasks[0].shell.env.PORT): " + refused},
		"a loop": {"node_tasks:\n- show: hello\n  loop: '{{ .inputs.tables[.inputs.which_port] }}'\n  as: entry\n",
			".inputs.tables[.inputs.which_port] uses the role input .inputs.which_port as a selector, but the input is declared secret: true, and a secret cannot select another value. " + advice},
		"a phase of a node task block": {"node_tasks:\n- task_block:\n    id: configure\n    changes:\n    - show: '{{ .inputs.ports[.inputs.which_port] }}'\n",
			refused},
		"an output of the role": {"node_tasks:\n- show: hello\noutputs:\n  port: '{{ .inputs.ports[.inputs.which_port] }}'\n",
			refused},
	} {
		t.Run(name, func(t *testing.T) {
			if err := load(t, tc.roleBody, ""); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v\nwant %s", err, tc.want)
			}
		})
	}
	// A template file of the role reads values with the same rule.
	for _, text := range []string{"port={{ index .inputs.ports .inputs.which_port }}\n", "port={{ (index .inputs.tables .inputs.which_port).web }}\n"} {
		err := load(t, "node_tasks:\n- file: {path: /etc/app.conf, permissions_mode: '0600', template: app.conf.tmpl}\n", text)
		if err == nil || !strings.Contains(err.Error(), "(node_tasks[0].file.template): ") ||
			!strings.Contains(err.Error(), "uses the role input .inputs.which_port as a selector, but the input is declared secret: true, and a secret cannot select another value. "+advice) {
			t.Fatalf("%s: %v", text, err)
		}
	}
	// The secret input is still read as a value, a selector may still choose
	// inside it, and an input that is not declared secret may still select.
	for _, roleBody := range []string{
		"node_tasks:\n- show: '{{ .inputs.which_port }}'\n",
		"node_tasks:\n- show: '{{ .inputs.which_port[.inputs.name] }}'\n",
		"node_tasks:\n- show: '{{ .inputs.ports[.inputs.name] }}'\n",
		"node_tasks:\n- show: '{{ .inputs.ports[.inputs.tables.a[.inputs.name]] }}'\n",
	} {
		if err := load(t, roleBody, ""); err != nil {
			t.Fatal(roleBody, err)
		}
	}
}

// A role that passes its secret input on to another role must pass it to an
// input that the other role declares secret: true, as with a .secrets
// reference.
func TestASecretRoleInputPassedOnNeedsASecretInput(t *testing.T) {
	const refused = "role input x receives a secret; declare secret: true"
	load := func(t *testing.T, nodeTasks string) error {
		t.Helper()
		path := fixture(t, "allow_role_calls_within_roles: true\nfleet_stages:\n- id: setup\n  nodes: all\n  node_tasks:\n  - role: app\n    inputs: {secret_in: '{{ .secrets.password }}', plain_in: web}\n")
		base := filepath.Dir(path)
		write(t, filepath.Join(base, "roles/app/main.yml"), "inputs:\n  secret_in: {type: string, secret: true, required: true}\n  plain_in: {type: string, required: true}\nnode_tasks:\n"+nodeTasks)
		write(t, filepath.Join(base, "roles/inner/main.yml"), "inputs:\n  x: {type: string}\n  y: {type: string, secret: true}\nnode_tasks: [{show: hello}]\n")
		_, _, err := Load(path, "example", "", nil)
		return err
	}
	for name, tc := range map[string]struct{ nodeTasks, want string }{
		"a role call": {"- role: inner\n  inputs: {x: '{{ .inputs.secret_in }}'}\n",
			"(node_tasks[0].inputs.x): " + refused},
		"inside other text": {"- role: inner\n  inputs: {x: 'password={{ .inputs.secret_in }}'}\n",
			"(node_tasks[0].inputs.x): " + refused},
		"a loop": {"- role: inner\n  id: each\n  loop: [a, b]\n  as: entry\n  inputs: {x: '{{ .inputs.secret_in }}-{{ .entry }}'}\n",
			"(node_tasks[0].inputs.x): " + refused},
		"after a node task block": {"- task_block:\n    id: configure\n    changes: [{show: hello}]\n- role: inner\n  inputs: {x: '{{ .inputs.secret_in }}'}\n",
			"(node_tasks[1].inputs.x): " + refused},
	} {
		t.Run(name, func(t *testing.T) {
			if err := load(t, tc.nodeTasks); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v\nwant %s", err, tc.want)
			}
		})
	}
	// An input that is no secret may be passed on to any input, and a secret
	// input to an input declared secret: true.
	for _, nodeTasks := range []string{
		"- role: inner\n  inputs: {x: '{{ .inputs.plain_in }}'}\n",
		"- role: inner\n  inputs: {y: '{{ .inputs.secret_in }}'}\n",
		"- role: inner\n  id: each\n  loop: [a, b]\n  as: entry\n  inputs: {x: '{{ .inputs.plain_in }}-{{ .entry }}', y: '{{ .inputs.secret_in }}'}\n",
	} {
		if err := load(t, nodeTasks); err != nil {
			t.Fatal(nodeTasks, err)
		}
	}
}

// One rule gives one message: every place that reads a variable which no layer
// declares says where to declare it. A fleet stage's when says that set_vars
// cannot help; a place with no single node names only the project and the
// inventory's top level; a role says to use an input.
func TestUndeclaredVariableMessageIsTheSameInEveryPlace(t *testing.T) {
	const (
		oneNode         = "is not declared for node web1: declare it under vars in mmx_project_settings.yml"
		noSingleNode    = "is not declared where this place can read it: no single node is chosen here, so declare it under vars in mmx_project_settings.yml, under vars at the top level of inventories/example.yml (not inside a group or a node), or in command_line_vars of the playbook"
		inner           = "    nodes: all\n    node_tasks: [{show: a}]\n"
		stage           = "fleet_stages:\n- id: s\n  nodes: all\n  node_tasks: [{show: a}]\n"
		forEachNode     = "fleet_stages:\n- id: l\n  for_each_node: all\n  as: i\n"
		loop            = "fleet_stages:\n- id: l\n  loop: [a]\n  as: i\n  key: '{{ .i }}'\n"
		child           = "  fleet_stages:\n  - id: s\n" + inner
		setVarsAdvice   = "or set it earlier in this fleet stage with set_vars"
		noSetVarsAdvice = "set_vars cannot set it here, because a fleet stage's when is read before the fleet stage's node tasks run"
	)
	for name, tc := range map[string]struct{ playbook, want, notWant string }{
		"fleet stage when":                {"fleet_stages:\n- id: s\n  nodes: all\n  when: {not: {equals: ['{{ .vars.x }}', []]}}\n  node_tasks: [{show: a}]\n", oneNode + " or in inventories/example.yml (the inventory, a group, or the node), or in command_line_vars of the playbook. " + noSetVarsAdvice, setVarsAdvice},
		"fleet stage nodes":               {"fleet_stages:\n- id: s\n  nodes: '{{ .vars.x }}'\n  node_tasks: [{show: a}]\n", noSingleNode, ""},
		"fleet stage except_nodes":        {"fleet_stages:\n- id: s\n  nodes: all\n  except_nodes: '{{ .vars.x }}'\n  node_tasks: [{show: a}]\n", noSingleNode, ""},
		"fleet stage outputs":             {stage + "  outputs: {o: '{{ .vars.x }}'}\n", oneNode + " or in inventories/example.yml (the inventory, a group, or the node), in command_line_vars of the playbook, " + setVarsAdvice, ""},
		"fleet stage loop":                {"fleet_stages:\n- id: l\n  loop: '{{ .vars.x }}'\n  as: i\n  key: '{{ .i }}'\n" + child, noSingleNode, ""},
		"fleet stage loop key":            {"fleet_stages:\n- id: l\n  loop: [a]\n  as: i\n  key: '{{ .vars.x }}'\n" + child, noSingleNode, ""},
		"fleet stage loop when":           {loop + "  when: '{{ .vars.x }}'\n" + child, noSingleNode, ""},
		"fleet stage loop outputs":        {loop + "  outputs: {o: '{{ .vars.x }}'}\n" + child, noSingleNode, ""},
		"for_each_node":                   {"fleet_stages:\n- id: l\n  for_each_node: '{{ .vars.x }}'\n  as: i\n" + child, noSingleNode, ""},
		"for_each_node except_nodes":      {forEachNode + "  except_nodes: '{{ .vars.x }}'\n" + child, noSingleNode, ""},
		"for_each_node when":              {forEachNode + "  when: '{{ .vars.x }}'\n" + child, noSingleNode, ""},
		"for_each_node outputs":           {forEachNode + "  outputs: {o: '{{ .vars.x }}'}\n" + child, noSingleNode, ""},
		"save_on_controller_machine path": {stage + "  outputs: {o: 1}\n  save_on_controller_machine: [{output: o, path: '{{ .vars.x }}/{{ .node.name }}', permissions_mode: '0644'}]\n", noSingleNode, ""},
		"role body":                       {"fleet_stages:\n- id: s\n  nodes: all\n  node_tasks: [{role: app}]\n", "reference .vars.x is not available in a role: a role reads only its inputs and the variables its own set_vars set earlier; declare an input in the role file and pass the value in the role call's inputs", "declare it under vars in mmx_project_settings.yml"},
	} {
		t.Run(name, func(t *testing.T) {
			path := fixture(t, tc.playbook)
			write(t, filepath.Join(filepath.Dir(path), "roles/app/main.yml"), "node_tasks: [{show: '{{ .vars.x }}'}]\n")
			_, _, err := Load(path, "example", "", nil)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want %q, got %v", tc.want, err)
			}
			if strings.Contains(err.Error(), "declared later") || tc.notWant != "" && strings.Contains(err.Error(), tc.notWant) {
				t.Fatalf("wrong advice in %v", err)
			}
		})
	}
	t.Run("fleet stage when reads a variable that its set_vars sets", func(t *testing.T) {
		path := fixture(t, "fleet_stages:\n- id: s\n  nodes: all\n  when: '{{ .vars.x }}'\n  node_tasks: [{set_vars: {x: 1}}]\n")
		_, _, err := Load(path, "example", "", nil)
		if err == nil || !strings.Contains(err.Error(), "reference .vars.x names a value that is not declared or is declared later") {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("fleet stage when reads a variable that only some nodes get", func(t *testing.T) {
		path := fixture(t, "fleet_stages:\n- id: s\n  nodes: all\n  when: '{{ .vars.x }}'\n  node_tasks: [{show: a}]\n")
		write(t, filepath.Join(filepath.Dir(path), "inventories/example.yml"), "nodes:\n  web1: {address: a.example.com}\n  web2: {address: b.example.com, vars: {x: 1}}\n")
		_, _, err := Load(path, "example", "", nil)
		if err == nil || !strings.Contains(err.Error(), "is declared for node web2 but not for node web1") || !strings.Contains(err.Error(), noSetVarsAdvice) || strings.Contains(err.Error(), setVarsAdvice) {
			t.Fatalf("got %v", err)
		}
	})
}

// A variable that no layer declares is fixed by declaring it, so the message
// names the places to declare it and the inventory in use; a variable that a
// set_vars sets only after the reference keeps the "declared later" message;
// a variable that only some nodes get names the nodes.
func TestUndeclaredVariableMessageNamesWhereToDeclareIt(t *testing.T) {
	load := func(t *testing.T, inventory, project string) error {
		t.Helper()
		path := fixture(t, project)
		write(t, filepath.Join(filepath.Dir(path), "inventories/example.yml"), inventory)
		_, _, err := Load(path, "example", "", nil)
		return err
	}
	stage := func(nodes, nodeTasks string) string {
		return "fleet_stages:\n- id: setup\n  nodes: " + nodes + "\n  node_tasks: " + nodeTasks + "\n"
	}
	both := "nodes:\n  web1: {address: a.example.com}\n  web2: {address: b.example.com}\ngroups:\n  web: {nodes: [web1, web2]}\n"
	t.Run("declared nowhere", func(t *testing.T) {
		err := load(t, both, stage("all", "[{show: '{{ .vars.secret_files }}'}]"))
		if err == nil {
			t.Fatal("accepted")
		}
		for _, want := range []string{"reference .vars.secret_files is not declared for node web1", "vars in mmx_project_settings.yml", "inventories/example.yml", "a group, or the node", "command_line_vars", "set_vars"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("missing %q in %v", want, err)
			}
		}
		if strings.Contains(err.Error(), "declared later") {
			t.Errorf("declared-later wording for a variable declared nowhere: %v", err)
		}
	})
	t.Run("set later in the fleet stage", func(t *testing.T) {
		err := load(t, both, stage("all", "[{show: '{{ .vars.later }}'}, {set_vars: {later: x}}]"))
		if err == nil || !strings.Contains(err.Error(), "reference .vars.later names a value that is not declared or is declared later") {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("declared for some nodes", func(t *testing.T) {
		inventory := both + "  db: {nodes: [web2]}\n"
		inventory = strings.Replace(inventory, "web2: {address: b.example.com}", "web2: {address: b.example.com, vars: {port: 80}}", 1)
		err := load(t, inventory, stage("all", "[{show: '{{ .vars.port }}'}]"))
		if err == nil || !strings.Contains(err.Error(), "node web1") || !strings.Contains(err.Error(), "declared for node web2") {
			t.Fatalf("got %v", err)
		}
	})
}
