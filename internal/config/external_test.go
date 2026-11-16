package config

import (
	"context"
	"encoding/json"
	"mmx/internal/modules"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"
)

// testModuleDescription takes one optional input of any type, text.
const testModuleDescription = "summary: a test module\ninputs: {text: {type: any, required: false, description: Any value.}}\nexamples: [{description: x, config: 'pair: {}'}]\n"

func moduleFile(t *testing.T, base, name, file, contents string, mode os.FileMode) string {
	t.Helper()
	description := filepath.Join(base, "external_modules", name, modules.ModuleDescriptionFile)
	if _, err := os.Stat(description); os.IsNotExist(err) {
		write(t, description, strings.ReplaceAll(testModuleDescription, "pair", name))
	}
	local := filepath.Join(base, "external_modules", name, file)
	write(t, local, contents)
	if err := os.Chmod(local, mode); err != nil {
		t.Fatal(err)
	}
	return local
}

func TestModuleDiscoveryFreezeAndSelection(t *testing.T) {
	project := fixture(t, "secrets_from_env: {token: MODULE_TEST_TOKEN}\nfleet_stages:\n- id: setup\n  nodes: all\n  node_tasks:\n  - pair: {text: \"{{ .secrets.token }}\"}\n  - role: use_pair\n  - task_block:\n      id: task_block\n      changes: [{pair: {}}]\n      cleanup: [{pair: {}}]\n")
	base := filepath.Dir(project)
	marker := filepath.Join(base, "must-not-run")
	moduleFile(t, base, "pair", "module_executable_linux_arm64", "#!/bin/sh\ntouch "+marker+"\n", 0700)
	moduleFile(t, base, "pair", "module_executable_linux_amd64", "amd64-binary", 0755)
	literal := "{{ .secrets.not_an_input }}"
	moduleFile(t, base, "pair", "support/"+literal, literal, 0640)
	write(t, filepath.Join(base, "roles/use_pair/main.yml"), "node_tasks: [{pair: {text: '{{ .item }}'}, loop: [one, two], as: item}]\n")
	p, _, err := Load(project, "example", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("discovery executed module code")
	}
	if len(p.Modules) != 1 || len(p.Modules["pair"].Files) != 4 || p.FleetStages[0].NodeTasks[0].Retry == nil {
		t.Fatal("module catalog or resource retry default missing")
	}
	if names := SecretNames(p); !reflect.DeepEqual(names, []string{"token"}) {
		t.Fatalf("module files were scanned as references: %v", names)
	}
	frozen := t.TempDir()
	if err := FreezeAssets(context.Background(), &p, frozen, nil); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	moduleJSON, _ := json.Marshal(p.Modules)
	if strings.Contains(string(moduleJSON), base) || strings.Contains(string(moduleJSON), "touch ") {
		t.Fatal("module bytes or controller paths embedded in job metadata")
	}
	if err := os.RemoveAll(filepath.Join(base, "external_modules")); err != nil {
		t.Fatal(err)
	}
	var saved Project
	if err := json.Unmarshal(raw, &saved); err != nil {
		t.Fatal(err)
	}
	if err := BindProjectAssets(&saved, frozen); err != nil {
		t.Fatal(err)
	}
	for _, arch := range []string{"arm64", "amd64"} {
		selected, err := SelectModules(saved.Modules, saved.FleetStages[0].NodeTasks, arch)
		if err != nil || selected["pair"].Executable != "module_executable_linux_"+arch || len(selected["pair"].Files) != 3 {
			t.Fatalf("selection %s: %+v %v", arch, selected, err)
		}
		refs, err := RequiredAssets(saved.FleetStages[0].NodeTasks, selected)
		if err != nil || len(refs) != 3 {
			t.Fatalf("deduplicated assets: %v %v", refs, err)
		}
		for _, file := range selected["pair"].Files {
			data, err := os.ReadFile(file.Asset.Path)
			if err != nil || len(data) == 0 {
				t.Fatal("lost frozen module file", err)
			}
		}
	}
	if len(saved.Modules["pair"].Files) != 4 {
		t.Fatal("node selection modified saved definitions")
	}
	if _, err := SelectModules(saved.Modules, saved.FleetStages[0].NodeTasks, "unsupported"); err == nil {
		t.Fatal("accepted missing build")
	}
}

func TestInvalidLocalModules(t *testing.T) {
	for _, kind := range []string{"missing", "not executable", "mixed builds", "root symlink", "module symlink", "file symlink", "directory symlink", "fifo", "unsupported build"} {
		t.Run(kind, func(t *testing.T) {
			project := fixture(t, "fleet_stages: [{id: setup, nodes: all, node_tasks: [{pair: {}}]}]\n")
			base := filepath.Dir(project)
			entry := moduleFile(t, base, "pair", "module_executable", "#!/bin/sh\nexit 0\n", 0700)
			switch kind {
			case "missing":
				os.Remove(entry)
			case "not executable":
				os.Chmod(entry, 0600)
			case "mixed builds":
				moduleFile(t, base, "pair", "module_executable_linux_arm64", "program", 0700)
			case "unsupported build":
				os.Rename(entry, filepath.Join(filepath.Dir(entry), "module_executable_linux_other"))
			case "root symlink", "module symlink":
				path := filepath.Dir(entry)
				if kind == "root symlink" {
					path = filepath.Dir(path)
				}
				if err := os.Rename(path, path+"-real"); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(path+"-real", path); err != nil {
					t.Fatal(err)
				}
			case "file symlink":
				if err := os.Symlink(entry, filepath.Join(filepath.Dir(entry), "support")); err != nil {
					t.Fatal(err)
				}
			case "directory symlink":
				if err := os.Symlink(t.TempDir(), filepath.Join(filepath.Dir(entry), "support")); err != nil {
					t.Fatal(err)
				}
			case "fifo":
				if err := syscall.Mkfifo(filepath.Join(filepath.Dir(entry), "pipe"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if _, _, err := Load(project, "example", "", nil); err == nil {
				t.Fatal("accepted invalid local module")
			}
		})
	}
}

func TestModuleTaskValidation(t *testing.T) {
	for _, task := range []string{
		`{"../escape": {}}`, `{"{{ .vars.module }}": {}}`, `{pair: 123}`,
		`{pair: {}, shell: {script: "true"}}`, `{pair: [one]}`,
		`{pair: {text: "{{ .vars.missing }}"}}`,
		"{task_block: {id: op , changes: [{show: before}], validate: [{pair: {}}]}}",
		`{show: ready, inputs: {}}`, `{pair: {}, files: []}`, `{Pair: {}}`,
	} {
		project := fixture(t, "fleet_stages: [{id: setup, nodes: all, node_tasks: ["+task+"]}]\n")
		moduleFile(t, filepath.Dir(project), "pair", "module_executable", "#!/bin/sh\nexit 0\n", 0700)
		if _, _, err := Load(project, "example", "", nil); err == nil {
			t.Fatalf("accepted %s", task)
		}
	}
}

// The error for a required input that a node task leaves out names the
// input: its location is the module's, since the input is not written.
func TestMissingModuleInputIsNamed(t *testing.T) {
	project := fixture(t, "fleet_stages: [{id: setup, nodes: all, node_tasks: [{greeting: {path: /tmp/greeting.txt}}]}]\n")
	base := filepath.Dir(project)
	write(t, filepath.Join(base, "external_modules/greeting", modules.ModuleDescriptionFile), "summary: x\ninputs:\n  path: {type: string, required: true, description: The file.}\n  person: {type: string, required: true, description: The person to greet.}\nexamples: [{description: x, config: 'greeting: {path: /tmp/greeting.txt, person: Barbara}'}]\n")
	moduleFile(t, base, "greeting", "module_executable", "#!/bin/sh\nexit 0\n", 0700)
	_, _, err := Load(project, "example", "", nil)
	if err == nil || !strings.Contains(err.Error(), "the input person is required: The person to greet.") {
		t.Fatalf("error: %v", err)
	}
}

func TestModuleBundlePaths(t *testing.T) {
	description := modules.ModuleFile{Path: modules.ModuleDescriptionFile, Mode: 0600}
	executable := modules.ModuleFile{Path: modules.ModuleExecutableFile, Mode: 0700}
	for _, name := range []string{"../escape", "/absolute", "", ".", "a/../b", "a//b", "a\\b", "a\x00b", "a\xff"} {
		m := modules.Module{Kind: modules.KindProjectExecutable, Executable: modules.ModuleExecutableFile, Files: []modules.ModuleFile{executable, description, {Path: name, Mode: 0600}}}
		if err := m.Validate(true); err == nil {
			t.Fatalf("accepted %q", name)
		}
	}
	for _, extra := range []modules.ModuleFile{executable, {Path: "module_executable/child", Mode: 0600}, {Path: "support", Mode: 04755}} {
		m := modules.Module{Kind: modules.KindProjectExecutable, Executable: modules.ModuleExecutableFile, Files: []modules.ModuleFile{executable, description, extra}}
		if err := m.Validate(true); err == nil {
			t.Fatalf("accepted malformed file %+v", extra)
		}
	}
	for _, files := range [][]modules.ModuleFile{{executable}, {description}, {executable, description, {Path: modules.ModuleScriptFile, Mode: 0644}}} {
		m := modules.Module{Kind: modules.KindProjectExecutable, Executable: modules.ModuleExecutableFile, Files: files}
		if err := m.Validate(true); err == nil {
			t.Fatalf("accepted a module of the files %+v", files)
		}
	}
}
