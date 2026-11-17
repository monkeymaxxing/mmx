package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"mmx/internal/modules"
	"mmx/internal/value"
)

func TestLocalRoleTypesAndSourceLocations(t *testing.T) {
	path := fixture(t, `vars: {port: 80}
fleet_stages:
  - id: setup
    nodes: all
    node_tasks:
      - role: app
        inputs:
          port: "{{ .vars.port }}"
`)
	root := filepath.Dir(path)
	role := filepath.Join(root, "roles/app/main.yml")
	write(t, role, "inputs:\n  port:\n    type: integer\n    default: 8080\nnode_tasks: [{show: '{{ .inputs.port }}'}]\n")
	write(t, filepath.Join(root, "inventories/example.yml"), "nodes:\n  web:\n    address: unused\n    vars:\n      port: wrong\n")
	_, _, err := Load(path, "example", "", nil)
	for _, want := range []string{"input port must be integer", "inputs.port", "playbooks/example.yml:10:", "inventories/example.yml:5:", "value supplied at"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("want %s: %v", want, err)
		}
	}
	write(t, filepath.Join(root, "inventories/example.yml"), "nodes: {web: {address: unused, vars: {port: 443}}}\n")
	p, inv, err := Load(path, "example", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	report, err := Inspect(p, inv, []string{"web"})
	if err != nil {
		t.Fatal(err)
	}
	arg := report.FleetStages[0].NodeTasks[0].Arguments[0]
	if arg.Value != 443 || arg.Origins[0].File != "inventories/example.yml" {
		t.Fatal(arg)
	}
	write(t, role, "inputs:\n  port:\n    type: integer\n    default: wrong\nnode_tasks: [{show: '{{ .inputs.port }}'}]\n")
	_, _, err = Load(path, "example", "", nil)
	if err == nil || !strings.Contains(err.Error(), "main.yml:4:") || !strings.Contains(err.Error(), "inputs.port.default") {
		t.Fatal(err)
	}
}

// A role default is literal: a reference would read the caller's scope and
// escape validation, even on a call whose condition is known only on the node.
func TestRoleInputDefaultsRejectReferences(t *testing.T) {
	path := fixture(t, `vars: {region: eu-west}
fleet_stages:
  - id: setup
    nodes: all
    node_tasks:
      - role: app
        when: {equals: ['{{ .node.arch }}', arm64]}
`)
	role := filepath.Join(filepath.Dir(path), "roles/app/main.yml")
	for _, reference := range []string{"'{{ .vars.region }}'", "'{{ .vars.typo }}'", "'region {{ .node.arch }}'"} {
		write(t, role, "inputs:\n  region:\n    type: string\n    default: "+reference+"\nnode_tasks: [{show: '{{ .inputs.region }}'}]\n")
		_, _, err := Load(path, "example", "", nil)
		if err == nil || !strings.Contains(err.Error(), "main.yml:4:") || !strings.Contains(err.Error(), "inputs.region.default") || !strings.Contains(err.Error(), "pass references through the role call's inputs mapping") {
			t.Fatalf("%s: %v", reference, err)
		}
	}
}

func TestStaticChecksKeepUnknownAndSkippedArgumentsUnevaluated(t *testing.T) {
	path := fixture(t, `fleet_stages:
- id: setup
  nodes: all
  node_tasks:
  - when: false
    role: app
    inputs: {port: '{{ .vars.bad }}'}
  - id: runtime
    role: app
    inputs: {port: '{{ .node.cpu_count }}'}
  - when: false
    file: {permissions_mode: '0644', path: '{{ .vars.bad }}', content: x}
vars: {bad: [not, a, path]}
`)
	write(t, filepath.Join(filepath.Dir(path), "roles/app/main.yml"), "inputs: {port: {type: integer, required: true}}\nnode_tasks: [{show: '{{ .inputs.port }}'}]\n")
	p, inv, err := Load(path, "example", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	report, err := Inspect(p, inv, nil)
	if err != nil {
		t.Fatal(err)
	}
	if report.FleetStages[0].NodeTasks[0].Status != "skipped" {
		t.Fatal(report)
	}
	b, _ := json.Marshal(report)
	if !strings.Contains(string(b), `"status":"runtime"`) {
		t.Fatal(string(b))
	}
}

func TestShallowOverridesLiteralValuesAndTimingOrigins(t *testing.T) {
	path := fixture(t, `vars: {settings: {project: true}, message: '{{ .not.reinterpreted }}'}
node_task_defaults:
  time_limit: 1m
  retry: {max_tries: 5, pause_between_tries: 0s}
fleet_stages:
- id: setup
  nodes: all
  node_tasks:
  - show: '{{ .vars.settings }}'
  - show: '{{ .vars.message }}'
  - file: {permissions_mode: '0644', path: /tmp/config, content: x}
    time_limit: null
    retry: {pause_between_tries: 1s}
`)
	root := filepath.Dir(path)
	write(t, filepath.Join(root, "inventories/example.yml"), "vars: {settings: {inventory: true}}\nnodes:\n  web:\n    address: unused\n    vars: {settings: {node: true}}\n")
	p, inv, err := Load(path, "example", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	report, err := Inspect(p, inv, nil)
	if err != nil {
		t.Fatal(err)
	}
	nodeTasks := report.FleetStages[0].NodeTasks
	v := nodeTasks[0].Arguments[0].Value.(map[string]any)
	if len(v) != 1 || v["node"] != true {
		t.Fatal(v)
	}
	if nodeTasks[1].Arguments[0].Value != "{{ .not.reinterpreted }}" {
		t.Fatal(nodeTasks[1])
	}
	for _, arg := range nodeTasks[2].Timing {
		switch arg.Field {
		case "time_limit":
			if arg.Source.Field != "fleet_stages[0].node_tasks[2].time_limit" {
				t.Fatal(arg)
			}
		case "retry.max_tries":
			if arg.Source.Field != "node_task_defaults.retry.max_tries" {
				t.Fatal(arg)
			}
		case "retry.pause_between_tries":
			if arg.Source.Field != "fleet_stages[0].node_tasks[2].retry.pause_between_tries" {
				t.Fatal(arg)
			}
		case "time_limit_per_try":
			if arg.Source.Field != "MMX default" {
				t.Fatal(arg)
			}
		}
	}
}

func TestResolvedResourceArgumentsAndSchemaLocations(t *testing.T) {
	for _, tc := range []struct{ vars, task, field, want string }{
		{"destination: relative", "file: {permissions_mode: '0644', path: '{{ .vars.destination }}', content: x}", "file.path", "path must be absolute"},
		{"uid: wrong", "user: {create_home_directory: true, name: app, uid: '{{ .vars.uid }}'}", "user.uid", "integer"},
		{"group: bad:name", "group: {name: '{{ .vars.group }}'}", "group.name", "account name"},
		{"items: [a, b]", "shell: {script: 'true', env: {ITEMS: 'items: {{ .vars.items }}'}}", "shell.env.ITEMS", "expected a string, boolean, number, or binary content"},
	} {
		path := fixture(t, "vars: {"+tc.vars+"}\nfleet_stages: [{id: setup, nodes: all, node_tasks: [{"+tc.task+"}]}]\n")
		_, _, err := Load(path, "example", "", nil)
		if err == nil || !strings.Contains(err.Error(), tc.field) || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), ":4:") {
			t.Fatal(tc, err)
		}
	}
	// A shell env value that is one whole reference to a list or a mapping
	// is valid: the script gets it as JSON text.
	whole := fixture(t, "vars: {ports: [80, 443], site: {city: Frankfurt}}\nfleet_stages: [{id: setup, nodes: all, node_tasks: [{shell: {script: 'true', env: {PORTS: '{{ .vars.ports }}', SITE: '{{ .vars.site }}'}}}]}]\n")
	if _, _, err := Load(whole, "example", "", nil); err != nil {
		t.Fatal(err)
	}
	path := fixture(t, "fleet_stages:\n- id: setup\n  nodes: all\n  node_tasks:\n  - file:\n      path: /tmp/x\n      permissions_mode: 644\n      content: x\n")
	_, _, err := Load(path, "example", "", nil)
	if err == nil || !strings.Contains(err.Error(), "line 9 column 25 (fleet_stages[0].node_tasks[0].file.permissions_mode)") {
		t.Fatal(err)
	}
}

func TestArgumentRulesRunBeforeTheNode(t *testing.T) {
	// A resolved value gets the rule the host would apply.
	for _, tc := range []struct{ vars, task, field, want string }{
		{`target: "a\0b"`, "symlink: {path: /etc/link, target: '{{ .vars.target }}'}", "symlink.target", "null byte"},
		{"unit: 'bad name'", "systemd_service: {name: '{{ .vars.unit }}', state: running}", "systemd_service.name", "invalid service name"},
		// A value, such as another node's output, cannot climb out of the
		// directory a path names.
		{"site: ../../etc/cron.d", "file: {path: '/srv/sites/{{ .vars.site }}/index', permissions_mode: '0644', content: x}", "file.path", "without null bytes or .. parts"},
		{"site: ../..", "directory: {path: '/srv/sites/{{ .vars.site }}', state: absent}", "directory.path", "without .. parts"},
		{"site: ../../etc", "delete_old_files: {directory: '/srv/sites/{{ .vars.site }}', name_patterns: ['*.gz'], older_than: 7d, include_subdirectories: false}", "delete_old_files.directory", "without .. parts"},
		{"site: ../../root", "user: {create_home_directory: true, name: app, home: '/srv/{{ .vars.site }}'}", "user.home", "newlines, or .. parts"},
	} {
		path := fixture(t, "vars: {"+tc.vars+"}\nfleet_stages: [{id: setup, nodes: all, node_tasks: [{"+tc.task+"}]}]\n")
		_, _, err := Load(path, "example", "", nil)
		if err == nil || !strings.Contains(err.Error(), tc.field) || !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "playbooks/example.yml:4:") {
			t.Fatal(tc, err)
		}
	}
	// A literal value is checked in every branch, including skipped ones.
	for _, tc := range []struct{ task, field string }{
		{"{when: false, systemd_service: {name: 'bad name', state: running}}", "systemd_service.name"},
		{"{when: false, http: {url: 'ftp://example.com'}}", "http.url"},
		{"{when: false, group: {name: app, gid: 4294967295}}", "group.gid"},
		{"{when: '{{ .node.cpu_count }}', user: {create_home_directory: true, name: app, home: relative}}", "user.home"},
	} {
		for _, fleetStage := range []string{"{id: setup, nodes: all, node_tasks: [" + tc.task + "]}", "{id: setup, nodes: all, when: false, node_tasks: [" + tc.task + "]}"} {
			_, _, err := Load(fixture(t, "fleet_stages: ["+fleetStage+"]\n"), "example", "", nil)
			if err == nil || !strings.Contains(err.Error(), tc.field) {
				t.Fatal(fleetStage, err)
			}
		}
	}
}

// A login that a module hands only to its tool is a private value: its
// rule is checked, and explain never shows a secret in it. Checking it does
// not mark the task's trace, so it does not hide the task's result; every
// other private value that reads a secret still marks the trace.
func TestInspectArgumentsChecksALoginHandedOnlyToAToolWithoutMarkingTheTrace(t *testing.T) {
	secrets := value.Scope{"secrets": map[string]any{"registry_password": &value.Secret{Name: "registry_password", Data: "s3cret-sentinel", Available: true},
		"deploy_key": &value.Secret{Name: "deploy_key", Data: "key-sentinel", Available: true}, "nested": &value.Secret{Name: "nested", Data: map[string]any{"user": "deploy"}, Available: true}}}
	inspect := func(task Task) ([]Argument, *value.Trace, value.Scope, error) {
		t.Helper()
		s, trace := secrets.Trace()
		resolved, err := ResolveArguments(task, s)
		if err != nil {
			t.Fatal(err)
		}
		args, err := InspectArguments(resolved, s, false)
		return args, trace, s, err
	}
	byField := func(args []Argument) map[string]Argument {
		out := map[string]Argument{}
		for _, a := range args {
			out[a.Field] = a
		}
		return out
	}
	image := Task{Module: "docker_image", ModuleInputs: &modules.DockerImage{Image: "registry.example.com/shop/web:2.4", PullImage: "always",
		RegistryUsername: "deploy", RegistryPassword: "{{ .secrets.registry_password }}"}}
	args, trace, s, err := inspect(image)
	if err != nil || trace.Used {
		t.Fatal("checking a login marked the task's trace", err)
	}
	if password := byField(args)["docker_image.registry_password"]; password.Status != "secret" || password.Value != nil || len(password.Secrets) != 1 || password.Secrets[0] != "registry_password" {
		t.Fatalf("explain must name the secret and never show it: %+v", password)
	}
	if username := byField(args)["docker_image.registry_username"]; username.Status != "resolved" || username.Value != "deploy" {
		t.Fatalf("%+v", username)
	}
	if got := s.LoginsHandedOnlyToATool(); len(got) != 1 || got[0] != "s3cret-sentinel" {
		t.Fatal("the checked login was not remembered for the replacement", got)
	}
	if b, _ := json.Marshal(args); strings.Contains(string(b), "s3cret-sentinel") {
		t.Fatal("the inspected arguments hold the password")
	}
	// The login's rule still holds: a login is one text.
	wrong := map[string]Task{
		"docker_image.registry_username":                              {Module: "docker_image", ModuleInputs: &modules.DockerImage{Image: "registry.example.com/shop/web:2.4", PullImage: "always", RegistryUsername: "{{ .secrets.nested }}", RegistryPassword: "x"}},
		"podman_image.registry_password":                              {Module: "podman_image", ModuleInputs: &modules.PodmanImage{Image: "registry.example.com/shop/web:2.4", PullImage: "always", RegistryUsername: "deploy", RegistryPassword: "{{ .secrets.nested }}"}},
		"docker_compose_project.registry_logins[0].registry_password": {Module: "docker_compose_project", ModuleInputs: &modules.DockerComposeProject{ComposeProjectName: "shop", ComposeProjectDir: "/opt/shop", State: "running", PullImages: "always", ContainersOfRemovedServices: "remove", RegistryLogins: []modules.DockerRegistryLogin{{Registry: "registry.example.com", RegistryUsername: "deploy", RegistryPassword: "{{ .secrets.nested }}"}}}},
		"git.password":        {Module: "git", ModuleInputs: &modules.Git{Repo: "https://example.com/app.git", FollowBranch: "main", Path: "/srv/app", Username: "deploy", Password: "{{ .secrets.nested }}"}},
		"git.ssh_private_key": {Module: "git", ModuleInputs: &modules.Git{Repo: "git@example.com:app.git", FollowBranch: "main", Path: "/srv/app", SSHPrivateKey: "{{ .secrets.nested }}", SSHKnownHostsLines: "example.com ssh-ed25519 AAAA"}},
	}
	for field, task := range wrong {
		_, trace, _, err := inspect(task)
		if err == nil || !strings.Contains(err.Error(), field) || !strings.Contains(err.Error(), "expected a string, boolean, number, or binary content") || trace.Used {
			t.Fatal(field, err, trace.Used)
		}
	}
	// git's ssh_known_hosts_lines is no login: a secret in it marks the trace, as a
	// secret in any other private value does.
	_, trace, s, err = inspect(Task{Module: "git", ModuleInputs: &modules.Git{Repo: "git@example.com:app.git", FollowBranch: "main", Path: "/srv/app",
		SSHPrivateKey: "{{ .secrets.deploy_key }}", SSHKnownHostsLines: "{{ .secrets.registry_password }}"}})
	if err != nil || !trace.Used {
		t.Fatal("a secret in a private value that is no login must mark the trace", err)
	}
	if got := s.LoginsHandedOnlyToATool(); len(got) != 1 || got[0] != "key-sentinel" {
		t.Fatal(got)
	}
	// A secret in the image reference is refused, as in every identifier.
	identifier := Task{Module: "docker_image", ModuleInputs: &modules.DockerImage{Image: "registry.example.com/shop/{{ .secrets.registry_password }}:2.4", PullImage: "always"}}
	if _, err := ResolveArguments(identifier, secrets); err == nil || !strings.Contains(err.Error(), "resource identifier must not contain a secret") {
		t.Fatal(err)
	}
}

// ResolveArguments makes public identifiers literal data once, with the rules
// and field names validation uses, and leaves private values as references.
func TestResolveArgumentsMakesIdentifiersLiteralOnce(t *testing.T) {
	content := "{{ .vars.body }}"
	s := value.Scope{"vars": map[string]any{"dir": "/srv/{{ .vars.x }}/", "x": "a", "name": "app", "group": "workers", "id": 1201, "body": value.Sensitive("private")}}
	f := Task{Module: "file", ModuleInputs: &modules.File{Mode: "0644", Path: "{{ .vars.dir }}", Content: &content}}
	r, err := ResolveArguments(f, s)
	if err != nil || !r.Resolved || InputsOf[modules.File](r).Path != "/srv/{{ .vars.x }}" || *InputsOf[modules.File](r).Content != content || InputsOf[modules.File](f).Path != "{{ .vars.dir }}" {
		t.Fatal(InputsOf[modules.File](r), err)
	}
	if again, err := ResolveArguments(r, s); err != nil || InputsOf[modules.File](again).Path != InputsOf[modules.File](r).Path {
		t.Fatal("resolved an identifier twice", InputsOf[modules.File](again), err)
	}
	if args, err := InspectArguments(r, s, false); err != nil || len(args) != 1 || args[0].Field != "file.content" || args[0].Status != "secret" {
		t.Fatal("resolved task's private values were not checked alone", args, err)
	}
	u := Task{Module: "user", ModuleInputs: &modules.User{Name: "{{ .vars.name }}", UID: "{{ .vars.id }}", SupplementaryGroups: []string{"{{ .vars.group }}"}}}
	r, err = ResolveArguments(u, s)
	if err != nil || InputsOf[modules.User](r).Name != "app" || InputsOf[modules.User](r).UID != int64(1201) || InputsOf[modules.User](r).SupplementaryGroups[0] != "workers" || InputsOf[modules.User](u).SupplementaryGroups[0] != "{{ .vars.group }}" {
		t.Fatal(InputsOf[modules.User](r), err)
	}
	for _, tc := range []struct {
		task        Task
		v           any
		field, want string
	}{
		{Task{Module: "systemd_unit", ModuleInputs: &modules.Unit{Name: "{{ .vars.v }}", Content: &content}}, "../../tmp/escape.service", "unit.name", "unit name must be"},
		{Task{Module: "systemd_unit", ModuleInputs: &modules.Unit{Name: "app.service", DropIn: "{{ .vars.v }}", Content: &content}}, "../escape.conf", "systemd_unit.drop_in", "single .conf filename"},
		{Task{Module: "symlink", ModuleInputs: &modules.Symlink{Path: "/etc/link", Target: "{{ .vars.v }}"}}, "", "symlink.target", "non-empty"},
		{Task{Module: "user", ModuleInputs: &modules.User{Name: "{{ .vars.v }}"}}, value.Sensitive("private"), "user.name", "must not contain a secret"},
		{Task{Module: "apt_package", ModuleInputs: &modules.Package{Manager: "apt", Packages: []modules.PackageEntry{{Name: "{{ .vars.v }}", Version: "newest_when_first_installed"}}}}, value.Sensitive("private"), "apt_package.packages[0].name", "must not contain a secret"},
	} {
		if _, err := ResolveArguments(tc.task, value.Scope{"vars": map[string]any{"v": tc.v}}); err == nil || !strings.Contains(err.Error(), tc.field+": ") || !strings.Contains(err.Error(), tc.want) {
			t.Fatal(tc.field, err)
		}
	}
	for _, id := range []any{"1200", true, 1.5, value.Sensitive(1200)} {
		if _, err := ResolveArguments(Task{Module: "group", ModuleInputs: &modules.Group{Name: "app", GID: "{{ .vars.id }}"}}, value.Scope{"vars": map[string]any{"id": id}}); err == nil || !strings.Contains(err.Error(), "group.gid") {
			t.Fatal("accepted invalid resolved ID", id, err)
		}
	}
}

func TestServiceTaskBlockRejectsDuplicateTrackedPaths(t *testing.T) {
	for _, changes := range []string{
		"[{file: {permissions_mode: '0644', path: /etc/app.conf, content: x}}, {symlink: {path: '{{ .vars.dest }}', target: y}}]",
		"[{file: {permissions_mode: '0644', path: /etc/app.conf, content: x}}, {when: true, file: {path: /etc//app.conf, state: absent}}]",
		// Two whens known true locally: both tasks run.
		"[{when: {equals: ['{{ .vars.dest }}', /etc/app.conf]}, file: {permissions_mode: '0644', path: /etc/app.conf, content: x}}, {when: {not: {equals: ['{{ .vars.dest }}', /x]}}, file: {permissions_mode: '0644', path: /etc/app.conf, content: y}}]",
		"[{systemd_unit: {name: app.service, content: x}}, {file: {permissions_mode: '0644', path: /etc/systemd/system/app.service, content: x}}]",
		"[{line: {insert_at_end_of_file: true, path: /etc/app.conf, line: x}}, {file: {permissions_mode: '0644', path: /etc/app.conf, content: x}}]",
		"[{file: {permissions_mode: '0644', path: /etc/app.conf, content: x}}, {line: {insert_at_end_of_file: true, path: '{{ .vars.dest }}', line: x}}]",
		"[{edit_file: {path: /etc/app.conf, content_filter: {shell: {script: cat}}}}, {file: {permissions_mode: '0644', path: /etc/app.conf, content: x}}]",
		"[{file: {permissions_mode: '0644', path: '{{ .vars.dest }}', content: x}}, {edit_file: {path: /etc/app.conf, content_filter: {shell: {script: cat}}}}]",
	} {
		path := fixture(t, "vars: {dest: /etc/app.conf}\nfleet_stages: [{id: setup, nodes: all, node_tasks: [{task_block: {id: app , changes: "+(changes)+", activate: [{systemd_service: {name: app.service, state: running}}]}}]}]\n")
		_, _, err := Load(path, "example", "", nil)
		if err == nil || !strings.Contains(err.Error(), "duplicate service configuration path") {
			t.Fatal(changes, err)
		}
	}
	for _, changes := range []string{
		"[{file: {permissions_mode: '0644', path: /etc/a, content: x}}, {file: {permissions_mode: '0644', path: /etc/b, content: x}}]",
		// Several line edits of one file count once.
		"[{line: {insert_at_end_of_file: true, path: /etc/a, match: '^x=', line: x=1}}, {line: {insert_at_end_of_file: true, path: /etc//a, match: '^y=', line: y=1}}]",
		// So do edit_file edits, beside line edits of the same file.
		"[{line: {insert_at_end_of_file: true, path: /etc/a, line: x=1}}, {edit_file: {path: /etc/a, content_filter: {shell: {script: cat}}}}]",
		// A file that keeps its existing content is no tracked input.
		"[{file: {permissions_mode: '0644', path: /etc/a, content: x, if_file_exists: keep_existing_content}}, {line: {insert_at_end_of_file: true, path: /etc/a, line: x=1}}]",
		// A task whose when is false shares its path; the node decides a
		// when on its facts, such as one download per processor architecture.
		"[{file: {permissions_mode: '0644', path: /etc/a, content: x}}, {when: false, file: {path: /etc//a, state: absent}}]",
		"[{when: {equals: ['{{ .node.arch }}', amd64]}, download: {url: 'https://example.com/k3s', sha256: '" + strings.Repeat("a", 64) + "', path: /usr/local/bin/k3s, permissions_mode: '0755'}}, {when: {equals: ['{{ .node.arch }}', arm64]}, download: {url: 'https://example.com/k3s-arm64', sha256: '" + strings.Repeat("b", 64) + "', path: /usr/local/bin/k3s, permissions_mode: '0755'}}]",
	} {
		path := fixture(t, "fleet_stages: [{id: setup, nodes: all, node_tasks: [{task_block: {id: app , changes: "+(changes)+", activate: [{shell: {script: 'true'}}, {systemd_service: {name: app.service, state: running}}]}}]}]\n")
		if _, _, err := Load(path, "example", "", nil); err != nil {
			t.Fatal(changes, err)
		}
	}
}

func TestExplainDoesNotReadSecretsOrFetch(t *testing.T) {
	t.Setenv("MMX_TEST_SECRET", "private-sentinel")
	path := fixture(t, `secrets_from_env: {token: MMX_TEST_SECRET}
fleet_stages: [{id: setup, nodes: all, node_tasks: [{shell: {script: 'printf never', env: {TOKEN: '{{ .secrets.token }}'}}}, {download: {permissions_mode: '0644', url: 'https://127.0.0.1:1/unreachable', sha256: 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa', path: /tmp/artifact}}]}]
`)
	p, inv, err := Load(path, "example", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	report, err := Inspect(p, inv, nil)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(report)
	if strings.Contains(string(b), os.Getenv("MMX_TEST_SECRET")) || !strings.Contains(string(b), `"secrets":["token"]`) {
		t.Fatal(string(b))
	}
}

func TestConditionalRolesAndLoopsDoNotEvaluateTheirArguments(t *testing.T) {
	path := fixture(t, `vars: {bad: [not, a, path]}
fleet_stages:
- id: setup
  nodes: all
  node_tasks:
  - when: {equals: ['{{ .node.cpu_count }}', 1]}
    role: app
    inputs: {port: '{{ .vars.bad }}'}
  - loop: '{{ .node.addresses }}'
    as: address
    file: {permissions_mode: '0644', path: '{{ .vars.bad }}', content: x}
`)
	write(t, filepath.Join(filepath.Dir(path), "roles/app/main.yml"), "inputs: {port: {type: integer, required: true}}\nnode_tasks:\n- when: {equals: ['{{ .inputs.port }}', 80]}\n  show: '{{ .inputs.port }}'\n")
	p, inv, err := Load(path, "example", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	view, err := Inspect(p, inv, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, task := range view.FleetStages[0].NodeTasks {
		if task.Status != "conditional" {
			t.Fatal(task)
		}
	}
}

func TestTimingFailurePointsAtTheInheritedValue(t *testing.T) {
	path := fixture(t, "node_task_defaults:\n  time_limit: invalid\nfleet_stages:\n- id: setup\n  nodes: all\n  node_tasks: [{show: hi}]\n")
	_, _, err := Load(path, "example", "", nil)
	if err == nil || !strings.Contains(err.Error(), "node_task_defaults.time_limit") || !strings.Contains(err.Error(), "mmx_project_settings.yml:4:15") {
		t.Fatal(err)
	}
}

// FleetStage selections, conditions, and loops name their field and the variable
// that supplied the value, like task arguments.
func TestSelectionConditionAndLoopErrorsNameTheSuppliedValue(t *testing.T) {
	for _, tc := range []struct{ fleetStage, field string }{
		{"- id: setup\n  nodes: '{{ .vars.name }}'\n  node_tasks: [{show: hi}]\n", "fleet_stages[0].nodes"},
		{"- id: setup\n  nodes: all\n  when: '{{ .vars.name }}'\n  node_tasks: [{show: hi}]\n", "fleet_stages[0].when"},
		{"- id: setup\n  nodes: all\n  node_tasks: [{show: hi, loop: '{{ .vars.name }}', as: x}]\n", "fleet_stages[0].node_tasks[0].loop"},
		{"- id: roll\n  loop: '{{ .vars.name }}'\n  as: x\n  key: '{{ .x }}'\n  fleet_stages: [{id: inner, nodes: all, node_tasks: [{show: hi}]}]\n", "fleet_stages[0].loop"},
		{"- id: roll\n  loop: [a]\n  as: x\n  key: '{{ .x }}'\n  when: '{{ .vars.name }}'\n  fleet_stages: [{id: inner, nodes: all, node_tasks: [{show: hi}]}]\n", "fleet_stages[0].when"},
	} {
		_, _, err := Load(fixture(t, "vars: {name: web9}\nfleet_stages:\n"+tc.fleetStage), "example", "", nil)
		if err == nil || !strings.Contains(err.Error(), tc.field) || !strings.Contains(err.Error(), "value supplied at mmx_project_settings.yml:3:14 (vars.name)") {
			t.Fatal(tc.field, err)
		}
	}
}

func TestAssignmentReplacesNestedOrigins(t *testing.T) {
	path := fixture(t, `vars: {settings: {port: 80}}
fleet_stages:
- id: setup
  nodes: all
  node_tasks:
  - set_vars: {settings: {port: 443}}
  - show: '{{ .vars.settings.port }}'
`)
	p, inv, err := Load(path, "example", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	view, err := Inspect(p, inv, nil)
	if err != nil {
		t.Fatal(err)
	}
	arg := view.FleetStages[0].NodeTasks[1].Arguments[0]
	if arg.Value != 443 || len(arg.Origins) != 1 || arg.Origins[0].Field != "fleet_stages[0].node_tasks[0].set_vars.settings" {
		t.Fatal(arg)
	}
}

func TestNestedSecretInputsShowOnlyNames(t *testing.T) {
	path := fixture(t, `allow_role_calls_within_roles: true
secrets_from_env: {token: UNUSED_TOKEN}
fleet_stages: [{id: setup, nodes: all, node_tasks: [{role: outer, inputs: {password: '{{ .secrets.token }}'}}]}]
`)
	root := filepath.Dir(path)
	write(t, filepath.Join(root, "roles/outer/main.yml"), "inputs: {password: {type: string, secret: true, required: true}}\nnode_tasks: [{role: inner, inputs: {credential: '{{ .inputs.password }}'}}]\n")
	write(t, filepath.Join(root, "roles/inner/main.yml"), "inputs: {credential: {type: string, secret: true, required: true}}\nnode_tasks: [{shell: {script: 'true', env: {TOKEN: '{{ .inputs.credential }}'}}}]\n")
	p, inv, err := Load(path, "example", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	view, err := Inspect(p, inv, nil)
	if err != nil {
		t.Fatal(err)
	}
	args := view.FleetStages[0].NodeTasks[2].Arguments
	found := false
	for _, arg := range args {
		if arg.Field == "shell.env.TOKEN" {
			found = true
			if arg.Status != "secret" || arg.Value != nil || len(arg.Secrets) != 1 || arg.Secrets[0] != "token" {
				t.Fatal(arg)
			}
		}
	}
	if !found {
		t.Fatal(args)
	}
}

func TestLocalAssignmentsUseThePreTaskScope(t *testing.T) {
	path := fixture(t, `vars: {a: original}
fleet_stages:
- id: setup
  nodes: all
  node_tasks:
  - set_vars: {a: replacement, b: '{{ .vars.a }}'}
  - show: '{{ .vars.b }}'
`)
	p, inv, err := Load(path, "example", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	view, err := Inspect(p, inv, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := view.FleetStages[0].NodeTasks[1].Arguments[0].Value; got != "original" {
		t.Fatal(got)
	}
}

func TestCleanupIsInspectedWithItsTaskBlock(t *testing.T) {
	path := fixture(t, "vars: {relative: etc/app}\nfleet_stages:\n- id: setup\n  nodes: all\n  node_tasks:\n  - task_block:\n      id: app\n      changes: [{show: hi}]\n      on_failure: [{show: failed}]\n      cleanup: [{file: {permissions_mode: '0644', path: '{{ .vars.relative }}', content: x}}]\n")
	_, _, err := Load(path, "example", "", nil)
	if err == nil || !strings.Contains(err.Error(), "cleanup[0].file.path") || !strings.Contains(err.Error(), "path must be absolute") {
		t.Fatal(err)
	}
	write(t, path, "project_name: test\ndefault_inventory: example\nfleet_stages: [{id: setup, nodes: all, node_tasks: [{task_block: {id: app , changes: [{show: hi}], on_failure: [{show: failed}], cleanup: [{show: done}]}}]}]\n")
	p, inv, err := Load(path, "example", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	view, err := Inspect(p, inv, []string{"web1"})
	if err != nil {
		t.Fatal(err)
	}
	status := map[string]string{}
	for _, task := range view.FleetStages[0].NodeTasks {
		status[task.Path] = task.Status
	}
	if status["setup/app/on_failure/task_1"] != "conditional" || status["setup/app/cleanup/task_1"] != "ready" {
		t.Fatal(status)
	}
}

func TestServiceTaskBlockChecksTrackedPathsAsTheTaskBlockStarts(t *testing.T) {
	for _, tc := range []struct{ vars, changes, activate, field, want string }{
		{"{}", "{file: {permissions_mode: '0644', path: '{{ .vars.dest }}', content: x}}", "{systemd_service: {name: app.service, state: running}}", "changes[1].file.path", "value is unavailable at /vars/dest"},
		{"{dest: /etc/early}", "{file: {permissions_mode: '0644', path: '{{ .vars.dest }}', content: x}}", "{systemd_service: {name: app.service, state: running}}", "changes[1].file.path", "is fixed when node task block app starts, so the field cannot depend on a value assigned inside the node task block; value supplied at"},
		{"{}", "{loop: '{{ .vars.paths }}', as: p, file: {permissions_mode: '0644', path: '{{ .p }}', content: x}}", "{systemd_service: {name: app.service, state: running}}", "changes[1].loop", "value is unavailable at /vars/paths"},
		{"{}", "{show: hi}", "{systemd_service: {name: '{{ .vars.unit }}', state: running}}", "activate[0].systemd_service.name", "value is unavailable at /vars/unit"},
		// A tracked task's other public arguments are fixed with its path.
		{"{owner: root}", "{file: {permissions_mode: '0644', path: /etc/app.conf, owner: '{{ .vars.owner }}', content: x}}", "{systemd_service: {name: app.service, state: running}}", "changes[1].file.owner", "is fixed when node task block app starts"},
	} {
		path := fixture(t, "vars: "+(tc.vars)+"\nfleet_stages:\n- id: setup\n  nodes: all\n  node_tasks:\n  - task_block:\n      id: app\n      changes:\n      - set_vars: {dest: /etc/app.conf, paths: [/etc/a], unit: app.service, owner: app}\n      - "+(tc.changes)+"\n      activate: ["+(tc.activate)+"]\n")
		_, _, err := Load(path, "example", "", nil)
		if err == nil || !strings.Contains(err.Error(), tc.field) || !strings.Contains(err.Error(), tc.want) {
			t.Fatal(tc, err)
		}
	}
	// An assignment that does not move a tracked path is fine, and without a
	// service nothing is fixed in advance.
	for _, nodeTasks := range []string{
		"changes: [{file: {permissions_mode: '0644', path: '{{ .vars.dest }}', content: x}}, {set_vars: {dest: /etc/later}}]\n      activate: [{systemd_service: {name: app.service, state: running}}]",
		"changes: [{set_vars: {dest: /etc/app.conf}}, {file: {permissions_mode: '0644', path: '{{ .vars.dest }}', content: x}}]\n      activate: [{shell: {script: 'true'}}]",
	} {
		path := fixture(t, "vars: {dest: /etc/early}\nfleet_stages:\n- id: setup\n  nodes: all\n  node_tasks:\n  - task_block:\n      id: app\n      "+(nodeTasks)+"\n")
		if _, _, err := Load(path, "example", "", nil); err != nil {
			t.Fatal(nodeTasks, err)
		}
	}
}

// Tracked and untracked loops in a service task block validate wherever the
// task block sits, including under an enclosing fleet stage loop, looped task block, or
// looped role call.
func TestServiceTaskBlockLoopsValidate(t *testing.T) {
	loop := "{loop: '{{ .vars.sites }}', as: s, file: {permissions_mode: '0644', path: '/etc/app/{{ .s.key }}.conf', content: x}}"
	project := "vars: {sites: {a: 1}, names: [old]}\nfleet_stages:\n- id: setup\n  nodes: all\n  node_tasks:\n  - task_block:\n      id: app\n      changes: [" + (loop) + "]\n      activate: [{set_vars: {names: [new]}}, {loop: '{{ .vars.names }}', as: n, file: {permissions_mode: '0644', path: '/var/www/{{ .n }}', content: y}}, {systemd_service: {name: app, state: running}}]\n"
	if _, _, err := Load(fixture(t, project), "example", "", nil); err != nil {
		t.Fatal(err)
	}
	op := "task_block:\n      id: app\n      changes: [" + (loop) + "]\n      activate: [{systemd_service: {name: app, state: running}}]\n"
	for name, project := range map[string]string{
		"fleet stage loop": "vars: {sites: {a: 1}}\nfleet_stages:\n- id: rollout\n  loop: [one]\n  as: pass\n  key: '{{ .pass }}'\n  fleet_stages:\n  - id: setup\n    nodes: all\n    node_tasks:\n    - task_block:\n        id: app\n        changes: [" + (loop) + "]\n        activate: [{systemd_service: {name: app, state: running}}]\n",
		"task_block loop":  "vars: {sites: {a: 1}}\nfleet_stages:\n- id: setup\n  nodes: all\n  node_tasks:\n  - loop: [one]\n    as: pass\n    key: '{{ .pass }}'\n    " + op,
		"role loop":        "fleet_stages:\n- id: setup\n  nodes: all\n  node_tasks:\n  - {role: app, loop: [one], as: pass}\n",
	} {
		path := fixture(t, project)
		write(t, filepath.Join(filepath.Dir(path), "roles/app/main.yml"), "node_tasks:\n- task_block:\n    id: app\n    changes: [{loop: {a: 1}, as: s, file: {permissions_mode: '0644', path: '/etc/app/{{ .s.key }}.conf', content: x}}]\n    activate: [{systemd_service: {name: app, state: running}}]\n")
		if _, _, err := Load(path, "example", "", nil); err != nil {
			t.Fatal(name, err)
		}
	}
}

func TestInactiveRoleUsesItsOwnScope(t *testing.T) {
	path := fixture(t, `secrets_from_env: {token: UNUSED_TOKEN}
fleet_stages:
- id: setup
  nodes: all
  node_tasks:
  - when: {equals: ['{{ .node.cpu_count }}', 1]}
    role: app
    inputs: {password: '{{ .secrets.token }}'}
`)
	write(t, filepath.Join(filepath.Dir(path), "roles/app/main.yml"), "inputs: {password: {type: string, secret: true, required: true}}\nnode_tasks: [{shell: {script: 'true', env: {TOKEN: '{{ .inputs.password }}'}}}]\n")
	p, inv, err := Load(path, "example", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	view, err := Inspect(p, inv, []string{"web1"})
	if err != nil {
		t.Fatal(err)
	}
	task := view.FleetStages[0].NodeTasks[1]
	if task.Status != "conditional" || len(task.Arguments) != 1 || len(task.Arguments[0].Secrets) != 1 || task.Arguments[0].Secrets[0] != "token" {
		t.Fatal(task)
	}
}

// A limit narrows every fleet stage's selection, including inside fleet stage loops, and
// is rejected, naming the fleet stages, when a kept fleet stage needs work it leaves out.
func TestLimitNarrowsSelectionsAndRejectsLeftOutWork(t *testing.T) {
	path := fixture(t, "")
	write(t, filepath.Join(filepath.Dir(path), "inventories/example.yml"), "nodes:\n  web1: {address: a}\n  web2: {address: b}\n  db1: {address: c}\ngroups:\n  web: {nodes: [web1, web2]}\n  canary: {nodes: [web1]}\n")
	load := func(fleetStages string) (Project, Inventory) {
		t.Helper()
		write(t, path, "project_name: test\ndefault_inventory: example\nfleet_stages:\n"+fleetStages)
		p, inv, err := Load(path, "example", "", nil)
		if err != nil {
			t.Fatal(err)
		}
		return p, inv
	}
	p, inv := load(`- id: migrate
  nodes: db1
  node_tasks: [{show: migrate}]
- id: inspect
  nodes: web
  outputs: {name: '{{ .node.name }}'}
- id: configure
  nodes: web
  requires: {same_node: [inspect]}
  node_tasks: [{show: '{{ .outputs.inspect[.node.name].name }}'}]
- id: roll
  loop: [web1, web2]
  as: app
  key: '{{ .app }}'
  fleet_stages: [{id: update, nodes: '{{ .app }}', node_tasks: [{show: hi}]}]
`)
	view, err := Inspect(p, inv, []string{"canary"})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, fleetStage := range view.FleetStages {
		got[fleetStage.Path] = strings.Join(fleetStage.Nodes, ",") + "|" + strings.Join(fleetStage.LeftOut, ",")
	}
	want := map[string]string{"migrate": "|db1", "inspect": "web1|web2", "configure": "web1|web2", "roll[web1]/update": "web1|", "roll[web2]/update": "|web2"}
	if !reflect.DeepEqual(got, want) || strings.Join(view.Limit, ",") != "canary" {
		t.Fatal(got, view.Limit)
	}
	for _, tc := range []struct{ limit, fleetStages, wants string }{
		{"nope", "- id: setup\n  nodes: all\n  node_tasks: [{show: hi}]\n", `--limit: no node or inventory group named "nope"`},
		{"db1", "- id: setup\n  nodes: web\n  node_tasks: [{show: hi}]\n", "--limit db1 selects no node in any fleet stage"},
		{"canary", "- id: migrate\n  nodes: db1\n  node_tasks: [{show: hi}]\n- id: configure\n  nodes: web\n  requires: {all_nodes: [migrate]}\n  node_tasks: [{show: hi}]\n", "excludes fleet stage migrate, which fleet stage configure requires"},
		{"canary", "- id: migrate\n  nodes: db1\n  outputs: {version: 2}\n- id: configure\n  nodes: web\n  node_tasks: [{show: '{{ .outputs.migrate.db1.version }}'}]\n", "excludes fleet stage migrate, whose outputs fleet stage configure reads"},
		{"canary,db1", "- id: inspect\n  nodes: web\n  outputs: {name: '{{ .node.name }}'}\n- id: balance\n  nodes: db1\n  node_tasks: [{show: '{{ .outputs.inspect }}'}]\n", "excludes some nodes from fleet stage inspect, whose whole output collection fleet stage balance reads"},
		{"web1", "- id: inspect\n  nodes: web\n  outputs: {name: '{{ .node.name }}'}\n- id: roll\n  loop: [web1]\n  as: app\n  key: '{{ .app }}'\n  fleet_stages: [{id: update, nodes: '{{ .app }}', node_tasks: [{show: hi}]}]\n  outputs: {all: '{{ .outputs.inspect }}'}\n", "whose whole output collection fleet stage roll reads"},
	} {
		p, inv := load(tc.fleetStages)
		if _, err := Inspect(p, inv, strings.Split(tc.limit, ",")); err == nil || !strings.Contains(err.Error(), tc.wants) {
			t.Errorf("--limit %s: want %q, got %v", tc.limit, tc.wants, err)
		}
	}
}

// --inventory replaces the project's inventory, and its vault setting, for
// one invocation.
func TestInventoryOverrideReplacesTheProjectInventory(t *testing.T) {
	path := fixture(t, "fleet_stages: [{id: setup, nodes: all, node_tasks: [{show: hi}]}]\n")
	staging := filepath.Join(filepath.Dir(path), "inventories/staging.yml")
	write(t, staging, "vault_file: staging.age\nnodes:\n  stage1: {address: stage.example.com}\n")
	p, inv, err := Load(path, "example", "staging", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(inv.Nodes) != 1 || inv.Nodes["stage1"].Address != "stage.example.com" || p.Vault != filepath.Join(filepath.Dir(staging), "staging.age") {
		t.Fatal(inv.Nodes, p.Vault)
	}
	if _, _, err := Load(path, "example", filepath.Join(filepath.Dir(path), "missing.yml"), nil); err == nil || !strings.Contains(err.Error(), "--inventory") {
		t.Fatal(err)
	}
}

// A node's vars come from project, inventory, its groups, then its own vars;
// command-line variables take their --var value, else their default. Origins and
// explain name the group, the --var option, or the default.
func TestGroupAndCommandLineVariables(t *testing.T) {
	path := fixture(t, "")
	write(t, filepath.Join(filepath.Dir(path), "inventories/example.yml"), `vars: {region: north, port: 80}
nodes:
  web1: {address: a, vars: {port: 8080}}
  web2: {address: b}
  db1: {address: c}
groups:
  web: {nodes: [web2, web1], vars: {port: 9090, role: web}}
  canary: {nodes: [web1], vars: {canary: true}}
`)
	write(t, path, `project_name: test
default_inventory: example
vars: {region: project, tier: small}
command_line_vars:
  release: {type: string}
  workers: {type: integer, default: 4}
  migrate: {type: boolean, default: true}
fleet_stages:
- id: deploy
  nodes: all
  node_tasks: [{show: "{{ .vars.release }} on port {{ .vars.port }}"}]
`)
	p, inv, err := Load(path, "example", "", []string{"release={{ .secrets.x }}=1", "migrate=false"})
	if err != nil {
		t.Fatal(err)
	}
	// .inventory.hosts.<name>.vars holds only what differs by host: its
	// groups' and its own vars. The shared layers are read through .vars.
	shared := map[string]any{"region": "north", "port": 80, "tier": "small", "release": "{{ .secrets.x }}=1", "workers": 4, "migrate": false}
	for managedNode, own := range map[string]map[string]any{
		"web1": {"port": 8080, "role": "web", "canary": true},
		"web2": {"port": 9090, "role": "web"},
		"db1":  {},
	} {
		want := maps.Clone(shared)
		maps.Copy(want, own)
		if got := NodeVars(p, inv, managedNode); !reflect.DeepEqual(got, want) {
			t.Errorf("%s: %v, want %v", managedNode, got, want)
		}
		if got := InventoryValues(inv)["nodes"].(map[string]any)[managedNode].(map[string]any)["vars"]; !reflect.DeepEqual(got, own) {
			t.Errorf(".inventory.nodes.%s.vars: %v, want %v", managedNode, got, own)
		}
	}
	if got := InventoryValues(inv)["groups"].(map[string]any)["web"]; !reflect.DeepEqual(got, []any{"web1", "web2"}) {
		t.Fatal(got)
	}
	origins := VariableOrigins(p, inv, "web2")
	for ref, field := range map[string]string{".vars.port": "groups.web.vars.port", ".vars.region": "vars.region", ".vars.release": "--var release", ".vars.workers": "command_line_vars.workers.default"} {
		if origins[ref].Field != field {
			t.Errorf("%s from %+v, want %s", ref, origins[ref], field)
		}
	}
	view, err := Inspect(p, inv, []string{"web2"})
	if err != nil {
		t.Fatal(err)
	}
	var fromCommandLine []string
	for _, v := range view.CommandLineVars {
		fromCommandLine = append(fromCommandLine, fmt.Sprintf("%s=%v %s", v.Field, v.Value, v.Source.Field))
	}
	if want := []string{"migrate=false --var migrate", "release={{ .secrets.x }}=1 --var release", "workers=4 command_line_vars.workers.default"}; !reflect.DeepEqual(fromCommandLine, want) {
		t.Fatal(fromCommandLine)
	}
	show := view.FleetStages[0].NodeTasks[0].Arguments[0]
	if show.Value != "{{ .secrets.x }}=1 on port 9090" || fmt.Sprint(show.Origins) != "[inventories/example.yml:7:43 (groups.web.vars.port) --var release]" {
		t.Fatalf("%+v", show)
	}

	// validate accepts a missing value; explain, dryrun, and run require it.
	p, _, err = Load(path, "example", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := RequireCommandLineValues(p); err == nil || !strings.Contains(err.Error(), "set release with --var") {
		t.Fatal(err)
	}
	for _, tc := range []struct{ arg, want string }{
		{"nope=1", "--var nope=1: nope is not declared in command_line_vars (declared: migrate, release, workers)"},
		{"workers=four", "workers must be a decimal integer"},
		{"migrate=yes", "migrate must be true or false"},
		{"release", "expected NAME=VALUE"},
	} {
		if _, _, err := Load(path, "example", "", []string{tc.arg}); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("--var %s: want %q, got %v", tc.arg, tc.want, err)
		}
	}
	if _, _, err := Load(path, "example", "", []string{"release=1", "release=2"}); err == nil || !strings.Contains(err.Error(), "release is set more than once") {
		t.Fatal(err)
	}

	// Two groups of one host never both set a variable, even to equal values.
	write(t, filepath.Join(filepath.Dir(path), "inventories/example.yml"), "nodes: {web1: {address: a}}\ngroups:\n  web: {nodes: [web1], vars: {port: 80}}\n  canary: {nodes: [web1], vars: {port: 80}}\n")
	if _, _, err := Load(path, "example", "", []string{"release=1"}); err == nil || !strings.Contains(err.Error(), "node web1 belongs to groups canary and web, which set the same variable port") {
		t.Fatal(err)
	}
}

// failed_nodes_allowed takes a count or a percentage of the fleet stage's selected
// nodes after the limit, rounded down; explain shows both.
func TestFailedNodesAllowedCountsTheSelection(t *testing.T) {
	path := fixture(t, `fleet_stages:
- id: patch
  nodes: web
  on_node_failure: stop_fleet_stage
  failed_nodes_allowed: "50%"
  node_tasks: [{show: hi}]
- id: count
  nodes: all
  on_node_failure: stop_run
  failed_nodes_allowed: 3
  node_tasks: [{show: hi}]
`)
	p, inv, err := Load(path, "example", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if p.FleetStages[0].FailedNodesAllowed != "50%" || p.FleetStages[1].FailedNodesAllowed != "3" {
		t.Fatalf("%+v", p.FleetStages)
	}
	for limit, want := range map[string][2]int{"": {1, 3}, "web1": {0, 3}} {
		var selectors []string
		if limit != "" {
			selectors = []string{limit}
		}
		view, err := Inspect(p, inv, selectors)
		if err != nil {
			t.Fatal(err)
		}
		for i, fleetStage := range view.FleetStages {
			if fleetStage.AllowedFailures == nil || *fleetStage.AllowedFailures != want[i] {
				t.Fatalf("limit %q: %s allows %v", limit, fleetStage.Path, fleetStage.AllowedFailures)
			}
		}
	}
}

// Only explain renders each node's templates: Inspect, which every run uses,
// leaves them out, so a run does not hold every node's files in memory.
func TestOnlyInspectForExplainRendersTemplates(t *testing.T) {
	path := fixture(t, "fleet_stages:\n- id: setup\n  nodes: web\n  node_tasks:\n  - file: {path: /etc/app.conf, template: app.conf.tmpl, owner: root, group: root, permissions_mode: '0644'}\n")
	write(t, filepath.Join(filepath.Dir(path), "app.conf.tmpl"), "name={{ .node.name }}\n")
	p, inv, err := Load(path, "example", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, explain := range []bool{false, true} {
		inspect := Inspect
		if explain {
			inspect = InspectForExplain
		}
		view, err := inspect(p, inv, nil)
		if err != nil {
			t.Fatal(err)
		}
		rendered := []string{}
		for _, task := range view.FleetStages[0].NodeTasks {
			for _, template := range task.RenderedTemplates {
				rendered = append(rendered, task.Node+": "+*template.Text)
			}
		}
		if want := map[bool]string{false: "[]", true: "[web1: name=web1\n web2: name=web2\n]"}[explain]; fmt.Sprint(rendered) != want {
			t.Fatal(explain, rendered)
		}
	}
}

// A node task block fixes the arguments of its tracked tasks as it starts,
// but not of one whose when is known to be false then: such a task writes
// nothing, so a value that only it reads need not exist.
func TestATaskBlockFixesNoArgumentsOfATaskWhoseWhenIsFalse(t *testing.T) {
	project := func(hasJobsServers string) string {
		return "vars: {has_jobs_servers: " + hasJobsServers + ", login_files: {app_servers: /etc/app/logins.conf}}\nfleet_stages:\n- id: setup\n  nodes: web\n  node_tasks:\n  - task_block:\n      id: logins\n" +
			"      changes:\n      - file: {path: /etc/app/logins.conf, content: x, owner: root, group: root, permissions_mode: '0644'}\n" +
			"      - when: '{{ .vars.has_jobs_servers }}'\n        file: {path: '{{ .vars.login_files.jobs_servers }}', content: x, owner: root, group: root, permissions_mode: '0644'}\n" +
			"      activate:\n      - systemd_service: {name: app, state: running, start_at_boot: true}\n"
	}
	if _, _, err := Load(fixture(t, project("false")), "example", "", nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Load(fixture(t, project("true")), "example", "", nil); err == nil || !strings.Contains(err.Error(), "node task block logins resolves the arguments of the block's node tasks before any block phase runs") {
		t.Fatal(err)
	}
}

// Which core modules' node tasks a node task block tracks: in changes, every
// module that lists tracked inputs, except a file that keeps its existing
// content and a directory that neither copies a source tree nor deletes; in
// activate, a service and a shell command that follows the tracked inputs.
func TestTrackedNodeTasksOfEachCoreModule(t *testing.T) {
	trackedInChanges := []string{"file", "line", "edit_file", "symlink", "download", "git", "apt_package", "dnf_package", "systemd_unit", "cron_job", "pip_package", "gem_package", "npm_global_package", "snap_package", "npm_project_dependencies", "yarn_project_dependencies", "bundler_project_dependencies", "composer_project_dependencies", "uv_project_dependencies", "docker_image", "podman_image", "podman_quadlet"}
	for _, m := range modules.CoreModules() {
		task := Task{Module: m.Name, ModuleInputs: m.NewInputs()}
		if got, want := Tracked(task, "changes"), slices.Contains(trackedInChanges, m.Name); got != want {
			t.Errorf("%s in changes: tracked %v, want %v", m.Name, got, want)
		}
		if got := Tracked(task, "activate"); got != (m.Name == "systemd_service") {
			t.Errorf("%s in activate: tracked %v", m.Name, got)
		}
	}
	for _, c := range []struct {
		module         string
		inputs         modules.Inputs
		changes, block bool
	}{
		{"file", &modules.File{IfFileExists: "keep_existing_content"}, false, false},
		{"directory", &modules.Directory{Source: "app"}, true, false},
		{"directory", &modules.Directory{State: "absent"}, true, false},
		{"directory", &modules.Directory{DeleteEverythingInsideExcept: []string{}}, true, false},
		{"shell", &modules.Shell{OnlyWhenTrackedInputsChanged: true}, false, true},
		{"systemd_service", &modules.Service{}, false, true},
	} {
		task := Task{Module: c.module, ModuleInputs: c.inputs}
		block := Task{TaskBlock: &TaskBlock{Activate: []Task{task}}}
		if Tracked(task, "changes") != c.changes || Tracked(task, "activate") != c.block || TrackingTaskBlock(block) != c.block {
			t.Errorf("%T %+v: tracked in changes %v, in activate %v, tracking block %v", c.inputs, c.inputs, Tracked(task, "changes"), Tracked(task, "activate"), TrackingTaskBlock(block))
		}
	}
	external := Task{Module: "greeting", ModuleInputs: map[string]any{}}
	if !Tracked(external, "changes") || Tracked(external, "activate") {
		t.Error("a module of the project's own is tracked in changes only")
	}
}

// An empty --var value for a command-line variable without a default is
// refused; a variable that declares default: "" may be set empty.
func TestEmptyCommandLineValueNeedsADefault(t *testing.T) {
	path := fixture(t, `command_line_vars:
  release: {type: string}
  note: {type: string, default: ""}
fleet_stages:
- id: deploy
  nodes: all
  node_tasks: [{show: "Release {{ .vars.release }}."}]
`)
	_, _, err := Load(path, "example", "", []string{"release="})
	var commandLineErr *CommandLineError
	if !errors.As(err, &commandLineErr) {
		t.Fatalf("want a CommandLineError, got %v", err)
	}
	for _, want := range []string{"--var release= gives an empty value", "release has no default", "unset environment variable", `declare default: "" for release`} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q lacks %q", err, want)
		}
	}
	if _, _, err := Load(path, "example", "", []string{"release=1", "note="}); err != nil {
		t.Fatalf("a variable declared with default: \"\" accepts an empty value: %v", err)
	}
}

// A reader who sees a duplicate path needs both node tasks and the source
// files, to know which one to remove.
func TestDuplicateServicePathNamesBothNodeTasksAndTheirSources(t *testing.T) {
	path := fixture(t, "fleet_stages: [{id: setup, nodes: all, node_tasks: [{task_block: {id: app , changes: [{id: first_copy, file: {permissions_mode: '0644', path: /etc/app/alerts.yml, source: files/first.yml}}, {id: second_copy, file: {permissions_mode: '0644', path: /etc/app/alerts.yml, template: templates/second.tmpl}}], activate: [{systemd_service: {name: app.service, state: running}}]}}]}]\n")
	dir := filepath.Dir(path)
	write(t, filepath.Join(dir, "files/first.yml"), "a")
	write(t, filepath.Join(dir, "templates/second.tmpl"), "b")
	_, _, err := Load(path, "example", "", nil)
	if err == nil {
		t.Fatal("accepted")
	}
	for _, want := range []string{"duplicate service configuration path /etc/app/alerts.yml", "first_copy", "second_copy", "files/first.yml", "templates/second.tmpl", "task_block app"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("missing %q in %v", want, err)
		}
	}
}
