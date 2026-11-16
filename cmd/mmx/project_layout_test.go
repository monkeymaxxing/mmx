package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"mmx/internal/config"
	"mmx/internal/controller"
	"mmx/internal/release"
)

func captureCommand(t *testing.T, args ...string) (string, int, error) {
	t.Helper()
	file, err := os.CreateTemp(t.TempDir(), "stdout")
	if err != nil {
		t.Fatal(err)
	}
	original := os.Stdout
	os.Stdout = file
	defer func() { os.Stdout = original; file.Close() }()
	code, commandErr := run(context.Background(), args)
	data, err := os.ReadFile(file.Name())
	if err != nil {
		t.Fatal(err)
	}
	return string(data), code, commandErr
}

func initializedProject(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	t.Chdir(root)
	if _, code, err := captureCommand(t, "init"); code != 0 || err != nil {
		t.Fatal(code, err)
	}
	return root
}

func writeProjectFile(t *testing.T, name, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(name), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestInitFilesAndCommandsFromSubdirectory(t *testing.T) {
	root := initializedProject(t)
	var files []string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() {
			relative, _ := filepath.Rel(root, path)
			files = append(files, relative)
		}
		return nil
	})
	if err != nil || strings.Join(files, ",") != ".gitignore,inventories/example.yml,mmx_project_settings.yml,playbooks/example.yml" {
		t.Fatal(files, err)
	}
	settings, err := config.ReadSettings(filepath.Join(root, "mmx_project_settings.yml"))
	if err != nil || settings.Project != "mmx_example" || settings.Inventory != "example" {
		t.Fatal(settings, err)
	}
	t.Chdir(filepath.Join(root, "playbooks"))
	for _, args := range [][]string{{"validate"}, {"validate", "example"}, {"nodes"}, {"nodes", "example"}, {"explain", "example"}} {
		if _, code, err := captureCommand(t, args...); code != 0 || err != nil {
			t.Fatal(args, code, err)
		}
	}
	for _, command := range []string{"run", "dryrun", "explain"} {
		if _, code, err := captureCommand(t, command); code != 4 || err == nil || !strings.Contains(err.Error(), "available playbooks: example") {
			t.Fatal(command, code, err)
		}
	}
}

func TestValidateAllAndNamedPlaybookAndVariables(t *testing.T) {
	initializedProject(t)
	writeProjectFile(t, "playbooks/deploy.yml", "command_line_vars:\n  release: {type: string}\nfleet_stages: [{id: deploy, nodes: all, node_tasks: [{show: '{{ .vars.release }}'}]}]\n")
	writeProjectFile(t, "inventories/staging.yml", "nodes: {staging: {address: staging.invalid, ssh_user: deploy}}\n")
	for _, args := range [][]string{{"validate"}, {"validate", "deploy"}, {"validate", "deploy", "--var", "release=v1"}, {"validate", "--inventory", "staging", "deploy", "--var", "release=v1"}, {"nodes", "deploy", "--var", "release=v1"}, {"nodes", "--inventory", "staging"}} {
		out, code, err := captureCommand(t, args...)
		if code != 0 || err != nil {
			t.Fatal(args, code, err)
		}
		if args[0] == "nodes" && args[1] == "--inventory" && !strings.Contains(out, "staging.invalid") {
			t.Fatal(out)
		}
	}
	for _, args := range [][]string{{"validate", "--var", "release=v1"}, {"nodes", "--var", "release=v1"}, {"validate", "deploy", "--var", "undeclared=x"}, {"explain", "deploy"}, {"validate", "deploy", "--inventory", "inventories/staging.yml"}, {"run", "playbooks/deploy.yml"}} {
		if _, code, err := captureCommand(t, args...); code != 4 || err == nil {
			t.Fatal(args, code, err)
		}
	}
	writeProjectFile(t, "inventories/broken.yml", "nodes: {bad: {address: broken.invalid, user: old}}\n")
	if _, code, err := captureCommand(t, "validate"); code != 1 || err == nil || !strings.Contains(err.Error(), "inventories/broken.yml") {
		t.Fatal(code, err)
	}
	if _, code, err := captureCommand(t, "validate", "deploy"); code != 0 || err != nil {
		t.Fatal(code, err)
	}
	if err := os.Remove("inventories/broken.yml"); err != nil {
		t.Fatal(err)
	}
	writeProjectFile(t, "playbooks/broken.yml", "vars: {wrong: true}\nfleet_stages: []\n")
	if _, code, err := captureCommand(t, "validate"); code != 1 || err == nil || !strings.Contains(err.Error(), "playbooks/broken.yml") {
		t.Fatal(code, err)
	}
}

// mmx validate without a playbook name takes no --var, so a command-line
// variable without a default is not known there, also in a download's URL,
// which must be known before a run starts. Every command that names the
// playbook requires the value; a URL that reads the node's facts stays an
// error.
func TestWholeProjectValidateLeavesACommandLineVariableUnknown(t *testing.T) {
	initializedProject(t)
	writeProjectFile(t, "roles/k3s_server/main.yml", `inputs:
  k3s_version: {type: string, required: true}
node_tasks:
  - task_block:
      id: k3s_binary
      changes:
        - when: {equals: ["{{ .node.arch }}", amd64]}
          download: {url: "https://example.com/{{ .inputs.k3s_version }}/k3s", sha256: "`+strings.Repeat("a", 64)+`", path: /usr/local/bin/k3s, permissions_mode: "0755"}
        - when: {equals: ["{{ .node.arch }}", arm64]}
          download: {url: "https://example.com/{{ .inputs.k3s_version }}/k3s-arm64", sha256: "`+strings.Repeat("b", 64)+`", path: /usr/local/bin/k3s, permissions_mode: "0755"}
      activate:
        - systemd_service: {name: k3s, state: running}
`)
	writeProjectFile(t, "playbooks/upgrade.yml", "command_line_vars:\n  k3s_version: {type: string}\nfleet_stages: [{id: upgrade, nodes: all, node_tasks: [{role: k3s_server, inputs: {k3s_version: '{{ .vars.k3s_version }}'}}]}]\n")
	for _, args := range [][]string{{"validate"}, {"validate", "upgrade", "--var", "k3s_version=v1"}, {"explain", "upgrade", "--var", "k3s_version=v1"}} {
		if _, code, err := captureCommand(t, args...); code != 0 || err != nil {
			t.Fatal(args, code, err)
		}
	}
	for _, args := range [][]string{{"validate", "upgrade"}, {"explain", "upgrade"}, {"dryrun", "upgrade"}, {"run", "upgrade"}} {
		if _, code, err := captureCommand(t, args...); code == 0 || err == nil || !strings.Contains(err.Error(), "--var k3s_version=VALUE") {
			t.Fatal(args, code, err)
		}
	}
	writeProjectFile(t, "playbooks/facts.yml", "fleet_stages: [{id: upgrade, nodes: all, node_tasks: [{download: {url: 'https://example.com/k3s-{{ .node.arch }}', sha256: '"+strings.Repeat("a", 64)+"', path: /usr/local/bin/k3s}}]}]\n")
	if _, code, err := captureCommand(t, "validate"); code != 1 || err == nil || !strings.Contains(err.Error(), "playbooks/facts.yml") {
		t.Fatal(code, err)
	}
}

// A loop item that waits for a command-line variable leaves a source path
// unknown in mmx validate without a playbook name too: in a node task loop
// inside a role call whose when the node answers, and in a fleet stage loop.
// Given the value, the playbook's source paths resolve for each node.
func TestWholeProjectValidateLeavesALoopOverACommandLineVariableUnknown(t *testing.T) {
	initializedProject(t)
	writeProjectFile(t, "checks/every_server.yml", "every server\n")
	writeProjectFile(t, "checks/ops.yml", "ops desk\n")
	writeProjectFile(t, "roles/checks/main.yml", `inputs:
  check_files: {type: list, required: true}
node_tasks:
  - loop: "{{ .inputs.check_files }}"
    as: check_file
    file: {path: "/etc/checks/{{ .check_file }}", template: "../../checks/{{ .check_file }}", permissions_mode: "0644"}
`)
	writeProjectFile(t, "playbooks/checks.yml", `command_line_vars:
  desk_name: {type: string}
fleet_stages:
  - id: checks
    nodes: all
    node_tasks:
      - role: checks
        when: {path_exists: /etc/hostname}
        inputs: {check_files: [every_server.yml, "{{ .vars.desk_name }}.yml"]}
  - id: each_desk
    loop: ["{{ .vars.desk_name }}"]
    as: desk
    key: "{{ .desk }}"
    fleet_stages:
      - id: desk_checks
        nodes: all
        node_tasks:
          - file: {path: "/etc/desk/{{ .desk }}.yml", template: "checks/{{ .desk }}.yml", permissions_mode: "0644"}
`)
	for _, args := range [][]string{{"validate"}, {"validate", "checks", "--var", "desk_name=ops"}, {"explain", "checks", "--var", "desk_name=ops"}} {
		if _, code, err := captureCommand(t, args...); code != 0 || err != nil {
			t.Fatal(args, code, err)
		}
	}
	if _, code, err := captureCommand(t, "validate", "checks", "--var", "desk_name=missing"); code == 0 || err == nil || !strings.Contains(err.Error(), "missing.yml") {
		t.Fatal(code, err)
	}
}

func TestRemovedCommandsAndFlagsAreRejected(t *testing.T) {
	initializedProject(t)
	for _, command := range []string{"retry", "logs", "shell", "describe"} {
		if _, code, err := captureCommand(t, command); code != 4 || err == nil || !strings.Contains(err.Error(), "unknown command") {
			t.Fatal(command, code, err)
		}
	}
	for _, flag := range []string{"--file", "--state-dir", "--runner-dir", "-e"} {
		if _, code, err := captureCommand(t, "validate", flag, "ignored"); code != 4 || err == nil || !strings.Contains(err.Error(), "flag provided but not defined") {
			t.Fatal(flag, code, err)
		}
	}
	help, code, err := captureCommand(t, "help")
	if code != 0 || err != nil {
		t.Fatal(code, err)
	}
	if !strings.Contains(help, "\nFor executable modules: mmx module-json") || strings.Contains(help, "--state-dir") || strings.Contains(help, "--runner-dir") || strings.Contains(help, "--file") {
		t.Fatal(help)
	}
}

func TestSavedSelectionAcrossPlaybooksInventoriesAndDryruns(t *testing.T) {
	root := initializedProject(t)
	state := filepath.Join(t.TempDir(), "history")
	t.Setenv("MMX_STATE_DIR", state)
	settings := filepath.Join(root, "mmx_project_settings.yml")
	c := controller.Controller{State: state, Selection: controller.Identity{EntryPoint: settings, Project: "mmx_example", Inventory: "example"}}
	save := func(playbook, inventory string, dryrun bool, entryPoint string) *controller.Run {
		t.Helper()
		c.Selection.Inventory, c.Selection.EntryPoint = inventory, entryPoint
		p := config.Project{Project: "mmx_example", PlaybookName: playbook, Inventory: inventory, HistoryRetention: "forever"}
		saved, err := c.New(context.Background(), p, config.Inventory{}, nil, dryrun)
		if err != nil {
			t.Fatal(err)
		}
		return saved
	}
	save("bootstrap", "example", false, settings)
	normal := save("deploy", "example", false, settings)
	preview := save("preview", "example", true, settings)
	staging := save("deploy", "staging", false, settings)
	command := save("", "example", false, controller.RunCommandEntryPoint)
	cases := []struct {
		args []string
		want *controller.Run
	}{
		{[]string{"resume"}, normal}, {[]string{"status"}, preview}, {[]string{"results"}, preview},
		{[]string{"results", "--inventory", "staging"}, staging},
		{[]string{"results", controller.ShortID(command.ID)}, command},
		{[]string{"results", normal.ID}, normal},
	}
	for _, tc := range cases {
		out, code, err := captureCommand(t, append(tc.args, "--json")...)
		if code != 0 || err != nil {
			t.Fatal(tc.args, code, err)
		}
		var selected struct{ ID, Playbook string }
		if err := json.Unmarshal([]byte(out), &selected); err != nil || selected.ID != tc.want.ID || selected.Playbook != tc.want.Project.PlaybookName {
			t.Fatal(tc.args, out, err)
		}
	}
	if command.EntryPoint != "run-command" {
		t.Fatal(command.EntryPoint)
	}
	if _, err := os.Stat(filepath.Join(root, ".mmx")); !os.IsNotExist(err) {
		t.Fatal("ignored MMX_STATE_DIR", err)
	}
	if _, code, err := captureCommand(t, "resume", preview.ID); code != 4 || err == nil || !strings.Contains(err.Error(), "dryrun") {
		t.Fatal(code, err)
	}
}

// A package manager may install mmx as a symlink in a directory on PATH.
// mmx started through the symlink still finds the official extra modules
// beside the program itself, not beside the symlink.
func TestSymlinkedMMXFindsTheOfficialExtraModulesBesideItself(t *testing.T) {
	program, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(program)
	if err != nil {
		t.Fatal(err)
	}
	releaseDirectory := t.TempDir()
	write := func(name, content string, mode os.FileMode) {
		t.Helper()
		path := filepath.Join(releaseDirectory, name)
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
	}
	// The test program runs mmx's main through TestMainProcessFixture.
	writeProgramFileThroughShellChild(t, filepath.Join(releaseDirectory, "mmx"), string(contents))
	write("official_extra_modules/"+config.OfficialExtraModulesVersionFile, release.Version+"\n", 0600)
	write("official_extra_modules/greeting/module.star", "def run_node_task(inputs):\n    return None\n", 0600)
	write("official_extra_modules/greeting/module_description.yml", "summary: Says hello.\nexamples: [{description: x, config: 'greeting: {}'}]\n", 0600)
	symlink := filepath.Join(t.TempDir(), "mmx")
	if err := os.Symlink(filepath.Join(releaseDirectory, "mmx"), symlink); err != nil {
		t.Fatal(err)
	}
	command := exec.Command(symlink, "-test.run=^TestMainProcessFixture$")
	command.Dir = t.TempDir()
	command.Env = append(os.Environ(), "MMX_MAIN_FIXTURE=1", "MMX_RUNNER_DIR=", "MMX_MAIN_ARGS=describe-module\n--json")
	output, err := command.Output()
	var listing struct {
		Entries []struct{ Name, Kind string }
	}
	if err != nil || json.Unmarshal(output, &listing) != nil {
		t.Fatal(err, string(output))
	}
	for _, entry := range listing.Entries {
		if entry.Name == "greeting" && entry.Kind == "official_extra" {
			return
		}
	}
	t.Fatalf("mmx started through a symlink did not list the official extra module beside the program:\n%s", output)
}

func TestControllerDirectoryEnvironmentVariables(t *testing.T) {
	root := initializedProject(t)
	settings := filepath.Join(root, "mmx_project_settings.yml")
	t.Chdir(filepath.Join(root, "playbooks"))
	t.Setenv("MMX_STATE_DIR", "")
	t.Setenv("MMX_RUNNER_DIR", "")
	state, runner, err := controllerDirectories(settings)
	executable, executableErr := os.Executable()
	if executableErr == nil {
		executable, executableErr = filepath.EvalSymlinks(executable)
	}
	if err != nil || executableErr != nil || state != filepath.Join(root, ".mmx") || runner != filepath.Dir(executable) {
		t.Fatal(state, runner, err, executableErr)
	}
	customState, customRunner := filepath.Join(t.TempDir(), "state"), filepath.Join(t.TempDir(), "runners")
	t.Setenv("MMX_STATE_DIR", customState)
	t.Setenv("MMX_RUNNER_DIR", customRunner)
	state, runner, err = controllerDirectories(settings)
	if err != nil || state != customState || runner != customRunner {
		t.Fatal(state, runner, err)
	}
}
