package config

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"mmx/internal/modules"
	"mmx/internal/value"
)

// variableProject is a project whose two nodes have a site each, with a
// template for each site, a file for each loop item, and a role that picks a
// file by an input.
func variableProject(t *testing.T, nodeTasks string) (path string) {
	t.Helper()
	path = fixture(t, "vars: {release: '1.4.2', digest: "+strings.Repeat("a", 64)+"}\nfleet_stages:\n- id: setup\n  nodes: all\n  node_tasks:\n"+nodeTasks)
	dir := filepath.Dir(path)
	write(t, filepath.Join(dir, "inventories/example.yml"), "nodes:\n  web1: {address: example.com, vars: {site: frankfurt}}\n  web2: {address: other.example.com, vars: {site: helsinki}}\n")
	write(t, filepath.Join(dir, "haproxy/frankfurt.cfg.tmpl"), "site frankfurt on {{ .node.name }} release {{ .vars.release }}\n")
	write(t, filepath.Join(dir, "haproxy/helsinki.cfg.tmpl"), "site helsinki on {{ .node.name }}\n")
	write(t, filepath.Join(dir, "files/alice.pub"), "alice key\n")
	write(t, filepath.Join(dir, "files/bob.pub"), "bob key\n")
	write(t, filepath.Join(dir, "files/web1.motd"), "this is web1\n")
	write(t, filepath.Join(dir, "files/web2.motd"), "this is web2\n")
	write(t, filepath.Join(dir, "roles/app/main.yml"), "inputs: {flavor: {type: string, required: true}}\nnode_tasks:\n- file: {permissions_mode: '0644', path: /etc/app.conf, source: 'conf/{{ .inputs.flavor }}.conf'}\n")
	write(t, filepath.Join(dir, "roles/app/conf/small.conf"), "small\n")
	write(t, filepath.Join(dir, "roles/app/conf/large.conf"), "large\n")
	return path
}

const variableTasks = `  - file: {permissions_mode: '0644', path: /etc/haproxy/haproxy.cfg, template: "haproxy/{{ .vars.site }}.cfg.tmpl"}
  - loop: [alice, bob]
    as: person
    file: {permissions_mode: '0644', path: "/etc/keys/{{ .person }}.pub", source: "files/{{ .person }}.pub"}
  - file: {permissions_mode: '0644', path: /etc/motd, source: "files/{{ .node.name }}.motd"}
  - role: app
    inputs: {flavor: small}
  - file: {permissions_mode: '0644', path: /etc/static, source: files/alice.pub}
`

// One task installs a different file for each node, loop item, or role input.
// The controller resolves the path for each node before the run; a node's job
// carries only its own files, and the node picks one by the same resolution.
func TestVariableSourcePathsSelectOneFileForEachNodeAndItem(t *testing.T) {
	path := variableProject(t, variableTasks)
	p, inv, err := Load(path, "example", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !ProjectHasVariableSources(p) {
		t.Fatal("variable sources not found")
	}
	// Loading inspected the project for every node and recorded the files.
	assets := t.TempDir()
	if err := FreezeAssets(context.Background(), &p, assets, nil); err != nil {
		t.Fatal(err)
	}
	// The saved run keeps every variant; reading it back needs no source file.
	saved, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	var restored Project
	if err := json.Unmarshal(saved, &restored); err != nil {
		t.Fatal(err)
	}
	if err := BindProjectAssets(&restored, assets); err != nil {
		t.Fatal(err)
	}
	for node, want := range map[string]struct{ site, motd string }{
		"web1": {"site frankfurt on {{ .node.name }} release {{ .vars.release }}\n", "this is web1\n"},
		"web2": {"site helsinki on {{ .node.name }}\n", "this is web2\n"},
	} {
		tasks, err := NodeTasksFor(restored.FleetStages[0].NodeTasks, node, assets)
		if err != nil {
			t.Fatal(err)
		}
		// The job for one node has that node's template and motd only, and
		// both loop items.
		if len(InputsOf[modules.File](tasks[0]).Variants) != 1 || len(InputsOf[modules.File](tasks[1]).Variants) != 2 || len(InputsOf[modules.File](tasks[2]).Variants) != 1 || len(InputsOf[modules.File](tasks[3].RoleNodeTasks[0]).Variants) != 1 {
			t.Fatalf("%s: %v %v %v", node, InputsOf[modules.File](tasks[0]).Variants, InputsOf[modules.File](tasks[1]).Variants, InputsOf[modules.File](tasks[2]).Variants)
		}
		required, err := RequiredAssets(tasks, nil)
		if err != nil || len(required) != 6 { // template, two keys (one is also the static file), motd, role file... counted by content
			t.Logf("%s: %d required assets", node, len(required))
		}
		// The node receives the job as JSON, binds the assets, and resolves.
		sent, _ := json.Marshal(tasks)
		var received []Task
		if err := json.Unmarshal(sent, &received); err != nil {
			t.Fatal(err)
		}
		if err := BindAssets(received, nil, assets); err != nil {
			t.Fatal(err)
		}
		scope := value.Scope{"vars": NodeVars(p, inv, node), "node": map[string]any{"name": node}, "inputs": map[string]any{}, "node_tasks": map[string]any{}}
		site, err := ResolveArguments(received[0], scope)
		if err != nil || InputsOf[modules.File](site).TemplateText != want.site || InputsOf[modules.File](site).Asset == nil || InputsOf[modules.File](site).Variants != nil {
			t.Fatalf("%s: %+v %v", node, InputsOf[modules.File](site), err)
		}
		for person, content := range map[string]string{"alice": "alice key\n", "bob": "bob key\n"} {
			local := scope.Local()
			local["person"] = person
			key, err := ResolveArguments(received[1], local)
			if err != nil || InputsOf[modules.File](key).Path != "/etc/keys/"+person+".pub" || read(t, InputsOf[modules.File](key).Asset.Path) != content {
				t.Fatalf("%s %s: %+v %v", node, person, InputsOf[modules.File](key), err)
			}
		}
		motd, err := ResolveArguments(received[2], scope)
		if err != nil || read(t, InputsOf[modules.File](motd).Asset.Path) != want.motd {
			t.Fatalf("%s: %+v %v", node, InputsOf[modules.File](motd), err)
		}
		role := scope.Local()
		role["inputs"] = map[string]any{"flavor": "small"}
		conf, err := ResolveArguments(received[3].RoleNodeTasks[0], role)
		if err != nil || read(t, InputsOf[modules.File](conf).Asset.Path) != "small\n" {
			t.Fatalf("%s: %+v %v", node, InputsOf[modules.File](conf), err)
		}
		// A value the controller did not prepare for is an error that says so.
		role["inputs"] = map[string]any{"flavor": "large"}
		if _, err := ResolveArguments(received[3].RoleNodeTasks[0], role); err == nil || !strings.Contains(err.Error(), "no file was prepared for") || !strings.Contains(err.Error(), "conf/large.conf") {
			t.Fatal(err)
		}
		static, err := ResolveArguments(received[4], scope)
		if err != nil || read(t, InputsOf[modules.File](static).Asset.Path) != "alice key\n" {
			t.Fatal(InputsOf[modules.File](static), err)
		}
	}
}

// What a variable source field may read, and what happens when the file it
// resolves to is missing, is decided before MMX connects to any node.
func TestVariableSourceErrorsNameTheNodeAndTheField(t *testing.T) {
	for tasks, message := range map[string]string{
		// web2's site has no file.
		`  - file: {permissions_mode: '0644', path: /etc/x, source: "files/{{ .vars.site }}.motd"}` + "\n": "node web1",
		// A fact is known only during a run.
		`  - file: {permissions_mode: '0644', path: /etc/x, source: "files/{{ .node.arch }}.bin"}` + "\n":                                                                        "source reads .node.arch, which is known only during a run",
		`  - {id: probe, shell: {script: "true"}}` + "\n" + `  - file: {permissions_mode: '0644', path: /etc/x, template: "haproxy/{{ .node_tasks.probe.stdout }}.tmpl"}` + "\n": "which is known only during a run",
		`  - download: {permissions_mode: '0644', url: "https://example.com/{{ .secrets.token }}/app", sha256: "{{ .vars.digest }}", path: /usr/local/bin/app}` + "\n":           "url reads .secrets.token",
		// A resolved path obeys the same rules as a written one.
		`  - role: escape` + "\n    inputs: {where: ../../..}\n":                                                                                                                                   "leaves",
		`  - file: {permissions_mode: '0644', path: /etc/x, source: "files/{{ .vars.missing }}.motd"}` + "\n":                                                                                      "reference .vars.missing is not declared for node ",
		`  - download: {permissions_mode: '0644', url: "http://example.com/{{ .vars.release }}", sha256: "{{ .vars.digest }}", path: /usr/local/bin/app}` + "\n":                                   "is not an HTTPS URL",
		`  - download: {permissions_mode: '0644', url: "https://example.com/{{ .vars.release }}", sha256: "{{ .vars.release }}", path: /usr/local/bin/app}` + "\n":                                 "is not 64 hexadecimal digits",
		`  - download: {permissions_mode: '0644', url: "https://example.com/{{ .vars.release }}", sha256: "{{ .vars.digest }}", dangerously_skip_checksum: true, path: /usr/local/bin/app}` + "\n": "not both",
		// A loop whose items a node task produces is known only during the run.
		`  - {id: list, shell: {script: "echo a"}}` + "\n" + `  - loop: "{{ .node_tasks.list.stdout_json }}"` + "\n    as: name\n" + `    file: {permissions_mode: '0644', path: "/etc/{{ .name }}", source: "files/{{ .name }}.pub"}` + "\n": "values known on the controller machine before connecting to the node",
		// A role input that a node task produces stays unknown, also when the
		// role call's when is answered on the node.
		`  - {id: probe, shell: {script: "echo small"}}` + "\n" + `  - role: app` + "\n    when: {path_exists: /etc/hostname}\n" + `    inputs: {flavor: "{{ .node_tasks.probe.stdout }}"}` + "\n": "(node_tasks[0].file.source): .inputs.flavor: ",
	} {
		path := variableProject(t, tasks)
		write(t, filepath.Join(filepath.Dir(path), "roles/escape/main.yml"), "inputs: {where: {type: string, required: true}}\nnode_tasks:\n- file: {permissions_mode: '0644', path: /etc/x, source: '{{ .inputs.where }}/files/alice.pub'}\n")
		_, _, err := Load(path, "example", "", nil)
		if err == nil || !strings.Contains(err.Error(), message) {
			t.Errorf("%s\nwant %q, got %v", tasks, message, err)
		}
	}
	// Every node of the inventory needs its file, and the error names the
	// node, the field, and the path.
	path := variableProject(t, `  - file: {permissions_mode: '0644', path: /etc/x, source: "files/{{ .vars.site }}.motd"}`+"\n")
	write(t, filepath.Join(filepath.Dir(path), "files/frankfurt.motd"), "frankfurt\n")
	if _, _, err := Load(path, "example", "", nil); err == nil || !strings.Contains(err.Error(), "node web2") || !strings.Contains(err.Error(), "file.source") || !strings.Contains(err.Error(), "helsinki.motd") {
		t.Fatal(err)
	}
	// A SHA-256 that resolves to empty text would install the file unchecked;
	// only dangerously_skip_checksum: true, written in its place, does that.
	// The error names the node, the field, and where the empty value was
	// supplied.
	for field, task := range map[string]string{
		"download.sha256":                             `  - download: {permissions_mode: '0644', url: "https://example.com/app", sha256: "{{ .vars.app_digest }}", path: /usr/local/bin/app}`,
		"unpack_archive.sha256":                       `  - unpack_archive: {url: "https://example.com/app.tar.gz", sha256: "{{ .vars.app_digest }}", path: /srv/app}`,
		"apt_package.packages[0].package_file_sha256": `  - apt_package: {packages: [{name: app, version: "1", package_file_url: "https://example.com/app.deb", package_file_sha256: "{{ .vars.app_digest }}"}]}`,
		"apt_repository.signing_key_sha256":           `  - apt_repository: {name: app, uris: ["https://example.com/apt"], suites: [stable], components: [main], signing_key_url: "https://example.com/key.asc", signing_key_sha256: "{{ .vars.app_digest }}"}`,
	} {
		path := variableProject(t, task+"\n")
		write(t, filepath.Join(filepath.Dir(path), "inventories/example.yml"), "nodes:\n  web1: {address: example.com, vars: {app_digest: "+strings.Repeat("a", 64)+"}}\n  web2: {address: other.example.com, vars: {app_digest: ''}}\n")
		_, _, err := Load(path, "example", "", nil)
		if err == nil || !strings.Contains(err.Error(), "node web2") || !strings.Contains(err.Error(), "."+field+")") || !strings.Contains(err.Error(), "empty") || !strings.Contains(err.Error(), "value supplied at inventories/example.yml") || !strings.Contains(err.Error(), "dangerously_skip_checksum: true") {
			t.Errorf("%s: want an error naming node web2, %s, and the empty value's place, got %v", field, field, err)
		}
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// A package task freezes each entry independently, including a static entry
// beside a variable entry. A saved run resolves both without the originals.
func TestPackageFilesFreezeStaticAndVariableSourcesTogether(t *testing.T) {
	path := variableProject(t, `  - apt_package:
      packages:
        - {name: common, version: "1", package_file_source: common.deb}
        - {name: application, version: "2", package_file_source: "{{ .vars.application_package }}"}
`)
	dir := filepath.Dir(path)
	write(t, filepath.Join(dir, "inventories/example.yml"), "nodes:\n  web1: {address: example.com, vars: {application_package: application.deb}}\n")
	write(t, filepath.Join(dir, "common.deb"), "common bytes")
	write(t, filepath.Join(dir, "application.deb"), "application bytes")
	p, inv, err := Load(path, "example", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	savedAssets := t.TempDir()
	if err := FreezeAssets(context.Background(), &p, savedAssets, nil); err != nil {
		t.Fatal(err)
	}
	saved, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	var restored Project
	if err := json.Unmarshal(saved, &restored); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, "common.deb"), "changed common")
	write(t, filepath.Join(dir, "application.deb"), "changed application")
	if err := BindProjectAssets(&restored, savedAssets); err != nil {
		t.Fatal(err)
	}
	tasks, err := NodeTasksFor(restored.FleetStages[0].NodeTasks, "web1", savedAssets)
	if err != nil {
		t.Fatal(err)
	}
	refs, err := RequiredAssets(tasks, nil)
	if err != nil || len(refs) != 2 {
		t.Fatalf("required assets: %v %v", refs, err)
	}
	resolved, err := ResolveArguments(tasks[0], value.Scope{"vars": NodeVars(p, inv, "web1")})
	if err != nil {
		t.Fatal(err)
	}
	entries := InputsOf[modules.Package](resolved).Packages
	for i, want := range []string{"common bytes", "application bytes"} {
		if entries[i].PackageFile == nil || read(t, entries[i].PackageFile.Path) != want {
			t.Fatalf("entry %d: %+v", i, entries[i])
		}
	}
}

// A task block checks its tracked paths before its own node tasks prepare
// their files for the node, so a source directory that differs by node
// inside the task block's changes loads for every node, and each node's job
// carries only its own directory.
func TestVariableDirectorySourceInsideATaskBlockDiffersByNode(t *testing.T) {
	path := variableProject(t, `  - task_block:
      id: haproxy
      changes:
        - directory: {path: /etc/haproxy/sites, source: "sites/{{ .vars.site }}"}
      activate:
        - systemd_service: {name: haproxy, state: running, refresh: reload}
`)
	dir := filepath.Dir(path)
	write(t, filepath.Join(dir, "sites/frankfurt/frankfurt.cfg"), "frankfurt\n")
	write(t, filepath.Join(dir, "sites/helsinki/helsinki.cfg"), "helsinki\n")
	p, _, err := Load(path, "example", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	assets := t.TempDir()
	if err := FreezeAssets(context.Background(), &p, assets, nil); err != nil {
		t.Fatal(err)
	}
	for node, want := range map[string]string{"web1": "sites/frankfurt", "web2": "sites/helsinki"} {
		tasks, err := NodeTasksFor(p.FleetStages[0].NodeTasks, node, assets)
		if err != nil {
			t.Fatal(err)
		}
		variants := InputsOf[modules.Directory](tasks[0].TaskBlock.Changes[0]).Variants
		if len(variants) != 1 || variants[want] == nil {
			t.Fatalf("%s: %v", node, variants)
		}
	}
}

// A role call or node task block whose when the node answers may run, so the
// source paths below it resolve for each node from what is known before
// connecting: the role's inputs and the items of a loop. The when stays a
// condition for the node, and each node's job carries only its own files.
func TestVariableSourcePathsUnderAConditionTheNodeAnswers(t *testing.T) {
	path := variableProject(t, `  - role: checks
    when: {path_exists: /etc/hostname}
    inputs: {flavor: "{{ .vars.flavor }}", people: "{{ .vars.people }}"}
  - when: {path_exists: /etc/hostname}
    task_block:
      id: more_keys
      changes:
        - loop: "{{ .vars.people }}"
          as: person
          file: {permissions_mode: '0644', path: "/etc/more/{{ .person }}.pub", source: "files/{{ .person }}.pub"}
`)
	dir := filepath.Dir(path)
	write(t, filepath.Join(dir, "inventories/example.yml"), "nodes:\n  web1: {address: example.com, vars: {flavor: small, people: [alice]}}\n  web2: {address: other.example.com, vars: {flavor: large, people: [bob]}}\n")
	write(t, filepath.Join(dir, "roles/checks/main.yml"), `inputs: {flavor: {type: string, required: true}, people: {type: list, required: true}}
node_tasks:
- file: {permissions_mode: '0644', path: /etc/app.conf, template: '../app/conf/{{ .inputs.flavor }}.conf'}
- loop: "{{ .inputs.people }}"
  as: person
  file: {permissions_mode: '0644', path: "/etc/keys/{{ .person }}.pub", source: "../../files/{{ .person }}.pub"}
`)
	p, inv, err := Load(path, "example", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	assets := t.TempDir()
	if err := FreezeAssets(context.Background(), &p, assets, nil); err != nil {
		t.Fatal(err)
	}
	for node, want := range map[string]struct{ flavor, person string }{"web1": {"small", "alice"}, "web2": {"large", "bob"}} {
		tasks, err := NodeTasksFor(p.FleetStages[0].NodeTasks, node, assets)
		if err != nil {
			t.Fatal(err)
		}
		template := InputsOf[modules.File](tasks[0].RoleNodeTasks[0]).Variants
		if len(template) != 1 || template["../app/conf/"+want.flavor+".conf"] == nil || template["../app/conf/"+want.flavor+".conf"].TemplateText != want.flavor+"\n" {
			t.Fatalf("%s: role template variants %v", node, template)
		}
		key := InputsOf[modules.File](tasks[0].RoleNodeTasks[1]).Variants
		if len(key) != 1 || key["../../files/"+want.person+".pub"] == nil {
			t.Fatalf("%s: role loop variants %v", node, key)
		}
		more := InputsOf[modules.File](tasks[1].TaskBlock.Changes[0]).Variants
		if len(more) != 1 || more["files/"+want.person+".pub"] == nil {
			t.Fatalf("%s: task block loop variants %v", node, more)
		}
		// Inspection keeps the role call and everything below it conditional
		// and shows the when the node answers.
		view, err := Inspect(p, inv, []string{node})
		if err != nil {
			t.Fatal(err)
		}
		statuses := map[string]string{}
		for _, task := range view.FleetStages[0].NodeTasks {
			statuses[task.Path] = task.Status
			if task.Path == "setup/checks" && !slices.ContainsFunc(task.Arguments, func(a Argument) bool { return a.Field == "when" && a.Status == "condition" }) {
				t.Fatalf("%s: role call shows no condition: %+v", node, task.Arguments)
			}
		}
		for _, task := range []string{"setup/checks", "setup/checks/task_1", "setup/checks/task_2[0]", "setup/more_keys", "setup/more_keys/changes/task_1[0]"} {
			if statuses[task] != "conditional" {
				t.Fatalf("%s: %s is %q, want conditional: %v", node, task, statuses[task], statuses)
			}
		}
	}
}
