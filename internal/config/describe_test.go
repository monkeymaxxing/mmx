package config

import (
	"fmt"
	"maps"
	"mmx/internal/modules"
	"mmx/internal/release"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

// Every name Task.Action can return needs an entry, so a new action cannot
// ship without its description.
func TestEveryTaskActionHasADescription(t *testing.T) {
	described := map[string]bool{}
	for _, d := range allDescriptions() {
		described[d.Name] = true
	}
	topicsInUse := map[string]bool{}
	for _, m := range modules.CoreModules() {
		if m.Description == nil || m.Description.Name != m.Name || m.NewInputs == nil {
			t.Errorf("core module %s has no description or input type in the registry", m.Name)
			continue
		}
		if !slices.Contains(modules.CoreModuleTopics, m.Description.Topic) {
			t.Errorf("core module %s has the topic %q, which is not one of modules.CoreModuleTopics", m.Name, m.Description.Topic)
		}
		topicsInUse[m.Description.Topic] = true
	}
	for _, topic := range modules.CoreModuleTopics {
		if !topicsInUse[topic] {
			t.Errorf("no core module has the topic %q", topic)
		}
	}
	for _, form := range []string{"show", "set_vars", "role", "task_block", "task"} {
		if !described[form] {
			t.Errorf("task form %s has no describe entry", form)
		}
	}
}

// Each entry lists exactly its schema's fields, its example loads through
// validation as a task of that action, and a field is required exactly when
// validation rejects the example without it.
func TestDescriptionsMatchValidation(t *testing.T) {
	taskFields := map[string]bool{}
	for _, entry := range allDescriptions() {
		d, err := Describe(entry.Name)
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, f := range d.Fields {
			if f.Type == "" {
				t.Errorf("%s.%s needs a type", d.Name, f.Name)
			}
			names = append(names, f.Name)
		}
		if entry.Schema == taskSchema {
			for _, name := range names {
				taskFields[name] = true
			}
		} else {
			taskFields[d.Name] = true
			want := slices.Sorted(maps.Keys(yamlFields(entry.Schema)))
			slices.Sort(names)
			if !slices.Equal(names, want) {
				t.Errorf("%s describes %v; its schema has %v", d.Name, names, want)
			}
		}
		if err := loadExample(t, d.Example); err != nil {
			t.Errorf("%s example: %v", d.Name, err)
			continue
		}
		task, err := exampleTask(d)
		if err != nil {
			t.Fatal(err)
		}
		if action, err := task.Action(); d.Name != "task" && (err != nil || action.Name != d.Name) {
			t.Errorf("%s example is a %s task: %v", d.Name, action.Name, err)
		}
		if d.Retry == "" || d.Dryrun == "" || len(d.Results) == 0 || d.Summary == "" {
			t.Errorf("%s entry is incomplete", d.Name)
		}
		for _, f := range d.Fields {
			without, present := removeField(t, d, entry.Schema == taskSchema, f.Name)
			if !present {
				if f.Required {
					t.Errorf("%s example lacks required field %s", d.Name, f.Name)
				}
				continue
			}
			err := loadExample(t, without)
			if f.Required && err == nil {
				t.Errorf("%s.%s is marked required, but the example validates without it", d.Name, f.Name)
			}
			if !f.Required && err != nil && f.Notes == "" {
				t.Errorf("%s.%s is marked optional, but the example fails without it: %v", d.Name, f.Name, err)
			}
		}
	}
	for name := range yamlFields(taskSchema) {
		if !taskFields[name] {
			t.Errorf("task field %s is not described", name)
		}
	}
}

// A core module's retry text says what validation does with its node task:
// it refuses retry, or retry and both time limits; it retries by default; or
// it runs once unless the node task sets retry.
func TestCoreModuleRetryTextMatchesValidation(t *testing.T) {
	for _, m := range modules.CoreModules() {
		d, err := Describe(m.Name)
		if err != nil {
			t.Fatal(err)
		}
		p, err := loadExampleProject(t, d.Example)
		if err != nil {
			t.Fatal(m.Name, err)
		}
		loaded := p.FleetStages[0].NodeTasks[0]
		retriesByDefault := loaded.Retry != nil && !loaded.Retry.Disabled
		retryRefused := loadExample(t, withTaskSetting(t, d, "retry", true)) != nil
		timeLimitRefused := loadExample(t, withTaskSetting(t, d, "time_limit", "5s")) != nil
		want := "runs once unless the task sets retry"
		switch {
		case retryRefused && timeLimitRefused:
			want = "rejects retry, time_limit, and time_limit_per_try"
		case retryRefused:
			want = "does not accept retry"
		case retriesByDefault:
			want = "bounded automatic retries by default"
		}
		if !strings.Contains(d.Retry, want) {
			t.Errorf("%s: retry refused %v, time_limit refused %v, retries by default %v; its retry text %q lacks %q", m.Name, retryRefused, timeLimitRefused, retriesByDefault, d.Retry, want)
		}
		if m.WaitsWithItsOwnLimit != (m.Description.WaitLimit != "") {
			t.Errorf("%s: only a module that waits with its own limit, and every such module, names the input that bounds the wait in WaitLimit", m.Name)
		}
	}
}

func TestDescribeRejectsUnknownNames(t *testing.T) {
	if _, err := Describe("lineinfile"); err == nil || !strings.Contains(err.Error(), "run mmx describe-module to list available modules") {
		t.Fatal(err)
	}
}

func TestModulesThatAreNotCoreAreDescribedFromTheirFiles(t *testing.T) {
	t.Setenv("MMX_RUNNER_DIR", t.TempDir())
	dir := t.TempDir()
	project := filepath.Join(dir, "mmx_project_settings.yml")
	if found, err := NonCoreModules(project); err != nil || found != nil {
		t.Fatal("a project without external_modules/ lists modules", found, err)
	}
	description := func(name string) string {
		return "summary: Says hello.\ninputs: {person: {type: string, required: true, description: The person.}}\nexamples: [{description: x, config: '" + name + ": {person: Barbara}'}]\n"
	}
	write(t, filepath.Join(dir, "external_modules/greeting/module.star"), "def run_node_task(inputs):\n    return None\n")
	write(t, filepath.Join(dir, "external_modules/greeting/module_description.yml"), description("greeting"))
	executable(t, filepath.Join(dir, "external_modules/compiled/module_executable_linux_amd64"))
	executable(t, filepath.Join(dir, "external_modules/compiled/module_executable_linux_arm64"))
	write(t, filepath.Join(dir, "external_modules/compiled/module_description.yml"), description("compiled"))
	found, err := NonCoreModules(project)
	if err != nil || len(found) != 2 || found[0].Name != "compiled" || found[1].Name != "greeting" {
		t.Fatal(found, err)
	}
	d, err := DescribeNonCoreModule(project, "compiled")
	if err != nil || d.Kind != modules.KindProjectExecutable || d.Directory != "external_modules/compiled" ||
		!slices.Equal(d.Builds, []string{"module_executable_linux_amd64", "module_executable_linux_arm64"}) || d.Summary != "Says hello." || d.Inputs[0].Name != "person" {
		t.Fatal(d, err)
	}
	if d, err := DescribeNonCoreModule(project, "greeting"); err != nil || d.Kind != modules.KindProjectStarlark {
		t.Fatal(d, err)
	}
	write(t, filepath.Join(dir, "external_modules/broken/README"), "no code\n")
	for _, name := range []string{"broken", "missing", "../greeting"} {
		if _, err := DescribeNonCoreModule(project, name); err == nil {
			t.Fatal("described invalid module", name)
		}
	}
}

// An official extra module is found before a module of the project's own, a
// module of the project's own cannot take its name, and the version beside
// mmx must be this release's.
func TestOfficialExtraModulesAndTheirVersion(t *testing.T) {
	releaseDirectory := t.TempDir()
	t.Setenv("MMX_RUNNER_DIR", releaseDirectory)
	extras := filepath.Join(releaseDirectory, "official_extra_modules")
	if err := CheckOfficialExtraModulesVersion(extras); err != nil {
		t.Fatal("a controller machine without the directory was refused", err)
	}
	write(t, filepath.Join(extras, "incus_instance/module.star"), "def run_node_task(inputs):\n    return None\n")
	write(t, filepath.Join(extras, "incus_instance/module_description.yml"), "summary: An Incus instance.\nexamples: [{description: x, config: 'incus_instance: {}'}]\n")
	otherVersion := release.Version + "-different"
	write(t, filepath.Join(extras, OfficialExtraModulesVersionFile), otherVersion+"\n")
	if err := CheckOfficialExtraModulesVersion(extras); err == nil || !strings.Contains(err.Error(), "were shipped with MMX "+otherVersion+", but the mmx program is version "+release.Version) {
		t.Fatal(err)
	}
	write(t, filepath.Join(extras, OfficialExtraModulesVersionFile), release.Version+"\n")
	if err := CheckOfficialExtraModulesVersion(extras); err != nil {
		t.Fatal(err)
	}
	project := fixture(t, "fleet_stages: [{id: setup, nodes: all, node_tasks: [{incus_instance: {}}]}]\n")
	p, _, err := Load(project, "example", "", nil)
	if err != nil || p.Modules["incus_instance"].Kind != modules.KindOfficialExtra {
		t.Fatal(p.Modules, err)
	}
	for name, want := range map[string]string{"incus_instance": "is an official extra module of MMX", "file": "is a core module of MMX", "when": "is a node task setting"} {
		base := filepath.Dir(project)
		write(t, filepath.Join(base, "external_modules", name, "module.star"), "def run_node_task(inputs):\n    return None\n")
		if _, _, err := Load(project, "example", "", nil); err == nil || !strings.Contains(err.Error(), want) {
			t.Error(name, err)
		}
		if err := os.RemoveAll(filepath.Join(base, "external_modules", name)); err != nil {
			t.Fatal(err)
		}
	}
}

// loadExample validates a task list in a project with the role and module its
// examples call.
func loadExample(t *testing.T, nodeTasks string) error {
	t.Helper()
	_, err := loadExampleProject(t, nodeTasks)
	return err
}

// loadExampleProject loads a task list as loadExample does and returns the
// loaded project.
func loadExampleProject(t *testing.T, nodeTasks string) (Project, error) {
	t.Helper()
	dir := t.TempDir()
	write(t, filepath.Join(dir, "inventories/example.yml"), "nodes:\n  web: {address: example.com}\n")
	write(t, filepath.Join(dir, "roles/web_server/main.yml"), "inputs:\n  port: {type: integer, default: 80}\nnode_tasks:\n  - show: \"{{ .inputs.port }}\"\n")
	executable(t, filepath.Join(dir, "external_modules/greeting/run"))
	path := filepath.Join(dir, "mmx_project_settings.yml")
	indented := "      " + strings.ReplaceAll(strings.TrimSuffix(nodeTasks, "\n"), "\n", "\n      ")
	write(t, path, "project_name: example\ndefault_inventory: example\nfleet_stages:\n  - id: example\n    nodes: all\n    node_tasks:\n"+indented+"\n")
	p, _, err := Load(path, "example", "", nil)
	return p, err
}

// exampleTask decodes an entry's example as a playbook task, with a download
// turned into its file, as loading does.
func exampleTask(d modules.Description) (Task, error) {
	var nodeTasks []Task
	if err := Decode(d.Name+" example", []byte(d.Example), &nodeTasks); err != nil {
		return Task{}, err
	}
	if len(nodeTasks) != 1 {
		return Task{}, fmt.Errorf("%s example needs exactly one task", d.Name)
	}
	t := nodeTasks[0]
	return t, convertWrittenInputs(&t)
}

// withTaskSetting returns an entry's example with one node task setting
// added beside the module.
func withTaskSetting(t *testing.T, d modules.Description, name string, value any) string {
	t.Helper()
	var nodeTasks []map[string]any
	if err := yaml.Unmarshal([]byte(d.Example), &nodeTasks); err != nil {
		t.Fatal(err)
	}
	nodeTasks[0][name] = value
	b, err := yaml.Marshal(nodeTasks)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// removeField returns the entry's example without one field, and whether the
// example had it.
func removeField(t *testing.T, d modules.Description, taskLevel bool, name string) (string, bool) {
	t.Helper()
	var nodeTasks []map[string]any
	if err := yaml.Unmarshal([]byte(d.Example), &nodeTasks); err != nil {
		t.Fatal(err)
	}
	fields := nodeTasks[0]
	if !taskLevel {
		fields = nodeTasks[0][d.Name].(map[string]any)
	}
	if _, ok := fields[name]; !ok {
		return "", false
	}
	delete(fields, name)
	b, err := yaml.Marshal(nodeTasks)
	if err != nil {
		t.Fatal(err)
	}
	return string(b), true
}

func executable(t *testing.T, path string) {
	t.Helper()
	write(t, path, "#!/bin/sh\n")
	if err := os.Chmod(path, 0755); err != nil {
		t.Fatal(err)
	}
}
