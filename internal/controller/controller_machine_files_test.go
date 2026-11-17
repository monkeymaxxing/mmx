package controller

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mmx/internal/config"
	"mmx/internal/modules"
	"mmx/internal/value"
)

// saveFixture is a run whose collect fleet stage publishes each node's report
// and saves it on the controller machine, one file per node and one summary.
func saveFixture(t *testing.T, root string, entries ...config.SaveOnControllerMachine) (Controller, *Run) {
	t.Helper()
	for i := range entries {
		entries[i].ProjectRoot = root
	}
	c, r := fleet(t, config.FleetStage{ID: "collect", Nodes: []any{"a", "b"}, OnNodeFailure: "continue",
		Outputs:                 map[string]any{"report": "report of {{ .vars.site }}"},
		SaveOnControllerMachine: entries})
	r.Project.Vars = map[string]any{"site": "frankfurt"}
	// The fixture created the run before these variables.
	if err := c.createRun(r); err != nil {
		t.Fatal(err)
	}
	return c, r
}

var (
	perNode = config.SaveOnControllerMachine{Output: "report", Path: "server_details/{{ .node.name }}.txt", PermissionsMode: "0644"}
	summary = config.SaveOnControllerMachine{Template: "summary.tmpl", Path: "summary.txt", PermissionsMode: "0640",
		TemplateText: "{{ range $node, $o := .outputs.collect }}{{ $node }}: {{ $o.report }}\n{{ end }}"}
)

func read(t *testing.T, path string) (string, os.FileMode) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(path)
	return string(b), info.Mode().Perm()
}

func files(r *Run) []ControllerMachineFile { return r.Selections["collect"].ControllerMachineFiles }

func TestSaveOnControllerMachineWritesOutputsAndATemplate(t *testing.T) {
	root := t.TempDir()
	c, r := saveFixture(t, root, perNode, summary)
	runLocally(&c, map[string]int{}, nil)
	execute(t, c, r, false)
	if r.Status != "succeeded" {
		t.Fatal(r.Status, files(r))
	}
	for _, node := range []string{"a", "b"} {
		if text, mode := read(t, filepath.Join(root, "server_details", node+".txt")); text != "report of frankfurt" || mode != 0644 {
			t.Fatal(node, text, mode)
		}
	}
	if text, mode := read(t, filepath.Join(root, "summary.txt")); text != "a: report of frankfurt\nb: report of frankfurt\n" || mode != 0640 {
		t.Fatal(text, mode)
	}
	saved := files(r)
	if len(saved) != 3 || saved[0].Node != "a" || saved[2].Node != "" || saved[0].Result.Change != "changed" || saved[0].Result.FileChange.Action != "create" {
		t.Fatal(saved)
	}
	// A new run that finds the files as wanted changes nothing.
	c, r = saveFixture(t, root, perNode, summary)
	runLocally(&c, map[string]int{}, nil)
	execute(t, c, r, false)
	for _, file := range files(r) {
		if file.Result.Status != "succeeded" || file.Result.Change != "unchanged" {
			t.Fatal(file)
		}
	}
}

func TestSaveOnControllerMachineDryrunWritesNothing(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "server_details"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "server_details/a.txt"), []byte("old report\n"), 0644); err != nil {
		t.Fatal(err)
	}
	c, r := saveFixture(t, root, perNode)
	r.Dryrun = true
	runLocally(&c, map[string]int{}, nil)
	execute(t, c, r, false)
	saved := files(r)
	if r.Status != "succeeded" || len(saved) != 2 {
		t.Fatal(r.Status, saved)
	}
	change := saved[0].Result.FileChange
	if saved[0].Result.Change != "would_change" || change.Action != "update" || !strings.Contains(change.Diff, "-old report") || !strings.Contains(change.Diff, "+report of frankfurt") {
		t.Fatal(saved[0].Result, change)
	}
	if saved[1].Result.Change != "would_change" || saved[1].Result.FileChange.Action != "create" {
		t.Fatal(saved[1].Result)
	}
	if text, _ := read(t, filepath.Join(root, "server_details/a.txt")); text != "old report\n" {
		t.Fatal("dryrun changed a file")
	}
	if _, err := os.Stat(filepath.Join(root, "server_details/b.txt")); !os.IsNotExist(err) {
		t.Fatal("dryrun created a file")
	}
}

// A secret value needs explicit read permission; it reaches the file and
// nothing else: no result, difference, or saved history holds it.
func TestSaveOnControllerMachineKeepsSecretsOutOfHistory(t *testing.T) {
	root := t.TempDir()
	c, r := fleet(t, config.FleetStage{ID: "collect", Nodes: "a",
		Outputs: map[string]any{"token": "{{ .secrets.token }}-derived"},
		SaveOnControllerMachine: []config.SaveOnControllerMachine{
			{Output: "token", Path: "public/{{ .node.name }}", PermissionsMode: "0644", ProjectRoot: root},
			{Output: "token", Path: "private/{{ .node.name }}", PermissionsMode: "0600", ProjectRoot: root},
		}})
	r.Project.Secrets = map[string]string{"token": "MMX_TEST_SAVED_SECRET"}
	if err := c.createRun(r); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MMX_TEST_SAVED_SECRET", "saved-secret-sentinel")
	runLocally(&c, map[string]int{}, nil)
	execute(t, c, r, false)
	saved := files(r)
	if r.Status != "failed" || saved[0].Result.Status != "failed" || !strings.Contains(saved[0].Result.Safe().Reason, `dangerously_let_every_account_read_secret: true`) {
		t.Fatal(r.Status, saved)
	}
	if _, err := os.Stat(filepath.Join(root, "public/a")); !os.IsNotExist(err) {
		t.Fatal("a secret was written to a readable file")
	}
	if text, mode := read(t, filepath.Join(root, "private/a")); text != "saved-secret-sentinel-derived" || mode != 0600 {
		t.Fatal(text, mode)
	}
	if !saved[1].Result.Secret || saved[1].Result.Safe().FileChange.Diff != "" {
		t.Fatal(saved[1].Result)
	}
	noSecretIn(t, c.State, "saved-secret-sentinel")
}

// mmx resume writes the files that are still missing: a node that failed
// first, the template that needs every node, and a file that could not be
// written. Files the run already wrote are kept.
func TestResumeWritesTheControllerMachineFilesStillMissing(t *testing.T) {
	root := t.TempDir()
	c, r := saveFixture(t, root, perNode, summary)
	attempts := map[string]int{}
	failing := true
	runLocally(&c, attempts, func(_, node string) bool { return node == "b" && failing })
	execute(t, c, r, false)
	saved := files(r)
	if r.Status != "failed" || len(saved) != 2 || saved[0].Node != "a" || saved[1].Result.Status != "blocked" || !strings.Contains(saved[1].Result.Reason, "collect on b is failed") {
		t.Fatal(r.Status, saved)
	}
	if _, err := os.Stat(filepath.Join(root, "server_details/b.txt")); !os.IsNotExist(err) {
		t.Fatal("a file was written for a failed node")
	}
	// Someone edits a's file before the resume: it was written, so it stays.
	if err := os.WriteFile(filepath.Join(root, "server_details/a.txt"), []byte("kept"), 0644); err != nil {
		t.Fatal(err)
	}
	r, err := c.Load(r.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	failing = false
	execute(t, c, r, true)
	if r.Status != "succeeded" || attempts["collect/a"] != 1 || attempts["collect/b"] != 2 {
		t.Fatal(r.Status, attempts, entry(t, r, "collect", "b").Response)
	}
	if text, _ := read(t, filepath.Join(root, "server_details/a.txt")); text != "kept" {
		t.Fatal("resume rewrote a file the run had written")
	}
	if text, _ := read(t, filepath.Join(root, "server_details/b.txt")); text != "report of frankfurt" {
		t.Fatal(text)
	}
	if text, _ := read(t, filepath.Join(root, "summary.txt")); !strings.Contains(text, "b: report of frankfurt") {
		t.Fatal(text)
	}
	if _, err := c.Load(r.ID, true); err != ErrNothingToResume {
		t.Fatal(err)
	}
}

// A file that cannot be written fails the run; once the cause is fixed,
// mmx resume writes it without running any node job again.
func TestResumeWritesAControllerMachineFileThatFailed(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "server_details"), []byte("a file in the way"), 0644); err != nil {
		t.Fatal(err)
	}
	c, r := saveFixture(t, root, perNode)
	attempts := map[string]int{}
	runLocally(&c, attempts, nil)
	execute(t, c, r, false)
	if r.Status != "failed" || files(r)[0].Result.Status != "failed" {
		t.Fatal(r.Status, files(r))
	}
	if err := os.Remove(filepath.Join(root, "server_details")); err != nil {
		t.Fatal(err)
	}
	r, err := c.Load(r.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Execute(context.Background(), r, true); err != nil {
		t.Fatal(err)
	}
	if r.Status != "succeeded" || attempts["collect/a"] != 1 || attempts["collect/b"] != 1 {
		t.Fatal(r.Status, attempts)
	}
	if text, _ := read(t, filepath.Join(root, "server_details/b.txt")); text != "report of frankfurt" {
		t.Fatal(text)
	}
}

// A relative path that a reference makes leave the project is refused when it
// resolves.
func TestSaveOnControllerMachineRefusesAPathLeavingTheProject(t *testing.T) {
	root := t.TempDir()
	c, r := saveFixture(t, root, config.SaveOnControllerMachine{Output: "report", Path: "{{ .vars.site }}/../../{{ .node.name }}", PermissionsMode: "0644"})
	runLocally(&c, map[string]int{}, nil)
	execute(t, c, r, false)
	if file := files(r)[0]; r.Status != "failed" || !strings.Contains(file.Result.Reason, "leaves the project root") {
		t.Fatal(r.Status, file)
	}
}

// A value that came from a node fills part of one file name in a path: a
// node cannot steer the controller machine's write to another directory,
// such as the project's playbooks, or to an absolute path.
func TestSaveOnControllerMachineRefusesANodeValueThatLeavesItsFileName(t *testing.T) {
	name := "{{ .outputs.collect[.node.name].name }}"
	for _, tc := range []struct{ name, path, refused string }{
		{"../playbooks/deploy", "hosts/" + name + ".yml", `.outputs.collect[.node.name].name is "../playbooks/deploy"`},
		{"/tmp/elsewhere", name + "-{{ .node.name }}", `.outputs.collect[.node.name].name is "/tmp/elsewhere"`},
		{"..", "hosts/" + name + "/{{ .node.name }}", `has a ".." part`},
		// Two values that are each one file name can still make "..".
		{".", "hosts/" + name + "." + "{{ .outputs.collect[.node.name].empty }}/{{ .node.name }}", `has a ".." part`},
	} {
		root := t.TempDir()
		if err := os.Mkdir(filepath.Join(root, "hosts"), 0755); err != nil {
			t.Fatal(err)
		}
		c, r := fleet(t, config.FleetStage{ID: "collect", Nodes: "a",
			Outputs: map[string]any{"name": tc.name, "empty": "", "report": "report"},
			SaveOnControllerMachine: []config.SaveOnControllerMachine{
				{Output: "report", Path: tc.path, PermissionsMode: "0644", ProjectRoot: root},
			}})
		runLocally(&c, map[string]int{}, nil)
		execute(t, c, r, false)
		file := files(r)[0]
		if r.Status != "failed" || file.Result.FileChange != nil || !strings.Contains(file.Result.Reason, tc.refused) {
			t.Fatal(tc.name, r.Status, file)
		}
		if entries, _ := os.ReadDir(filepath.Join(root, "hosts")); len(entries) != 0 {
			t.Fatal(tc.name, "a file was written in hosts")
		}
		if entries, _ := os.ReadDir(root); len(entries) != 1 {
			t.Fatal(tc.name, "a file was written outside hosts", entries)
		}
	}
	// A node value that is one file name writes as before.
	root := t.TempDir()
	c, r := fleet(t, config.FleetStage{ID: "collect", Nodes: "a",
		Outputs: map[string]any{"name": "web-1.example", "report": "report"},
		SaveOnControllerMachine: []config.SaveOnControllerMachine{
			{Output: "report", Path: "hosts/" + name + ".yml", PermissionsMode: "0644", ProjectRoot: root},
		}})
	runLocally(&c, map[string]int{}, nil)
	execute(t, c, r, false)
	if text, _ := read(t, filepath.Join(root, "hosts", "web-1.example.yml")); r.Status != "succeeded" || text != "report" {
		t.Fatal(r.Status, files(r))
	}
}

// A fleet stage that --only-fleet-stages leaves out runs nowhere, so it saves
// no file on the controller machine; a kept one saves its files.
func TestOnlyFleetStagesSavesOnlyTheKeptFleetStagesFiles(t *testing.T) {
	root := t.TempDir()
	stage := func(id string) config.FleetStage {
		return config.FleetStage{ID: id, Nodes: "a", Outputs: map[string]any{"report": "report of " + id},
			SaveOnControllerMachine: []config.SaveOnControllerMachine{{Output: "report", Path: id + "/{{ .node.name }}.txt", PermissionsMode: "0644", ProjectRoot: root}}}
	}
	c := Controller{State: t.TempDir()}
	p := config.Project{Project: "fleet", MaxNodesAtOnce: 2, OnlyFleetStages: []string{"kept"}, FleetStages: []config.FleetStage{stage("left_out"), stage("kept")}}
	r, err := c.New(context.Background(), p, config.Inventory{Nodes: map[string]config.Node{"a": {Address: "a"}}}, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	attempts := map[string]int{}
	runLocally(&c, attempts, nil)
	execute(t, c, r, false)
	if r.Status != "succeeded" || attempts["left_out/a"] != 0 {
		t.Fatal(r.Status, attempts)
	}
	if _, err := os.Stat(filepath.Join(root, "left_out")); !os.IsNotExist(err) {
		t.Fatal("a left-out fleet stage saved a file")
	}
	if text, _ := read(t, filepath.Join(root, "kept/a.txt")); text != "report of kept" {
		t.Fatal(text)
	}
}

// A path that a secret value reaches only when it renders is refused there,
// naming the entry; neither the path nor the value reaches saved history.
func TestSaveOnControllerMachineRefusesASecretInAPath(t *testing.T) {
	root := t.TempDir()
	c, r := fleet(t, config.FleetStage{ID: "collect", Nodes: "a",
		NodeTasks: []config.Task{{ID: "token", Module: "shell", ModuleInputs: &modules.Shell{Script: "echo cGF0aC1zZWNyZXQtc2VudGluZWw= | base64 -d", OutputIsSecret: true, ChangedWhen: false}}},
		Outputs:   map[string]any{"token": "{{ .node_tasks.token.stdout }}", "report": "report"},
		SaveOnControllerMachine: []config.SaveOnControllerMachine{
			{Output: "report", Path: "details/{{ .outputs.collect.a.token }}/{{ .node.name }}", PermissionsMode: "0644", ProjectRoot: root},
		}})
	runLocally(&c, map[string]int{}, nil)
	execute(t, c, r, false)
	file := files(r)[0]
	if r.Status != "failed" || file.Path != "" || file.Result.FileChange != nil || !strings.Contains(file.Result.Safe().Reason, "save_on_controller_machine[0].path: the destination file path would contain a secret value") {
		t.Fatal(r.Status, file)
	}
	if entries, _ := os.ReadDir(root); len(entries) != 0 {
		t.Fatal("a file was written")
	}
	noSecretIn(t, c.State, "path-secret-sentinel")
}

// Inside a loop, a file that could not be written leaves its iteration
// unfinished, so mmx resume writes it and the run succeeds.
func TestResumeWritesAControllerMachineFileThatFailedInALoopIteration(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "updated/a.txt"), 0755); err != nil {
		t.Fatal(err)
	}
	c, r := fleet(t, config.FleetStage{ID: "each", Loop: []any{"first"}, As: "item", Key: "{{ .item }}", FleetStages: []config.FleetStage{
		{ID: "update", Nodes: "a", Outputs: map[string]any{"report": "updated"},
			SaveOnControllerMachine: []config.SaveOnControllerMachine{{Output: "report", Path: "updated/{{ .node.name }}.txt", PermissionsMode: "0644", ProjectRoot: root}}},
	}})
	attempts := map[string]int{}
	runLocally(&c, attempts, nil)
	execute(t, c, r, false)
	if iteration := r.Selections["each"].Iterations["first"]; r.Status != "failed" || iteration.Status != "failed" || !strings.Contains(iteration.Error, "save_on_controller_machine[0]") {
		t.Fatal(r.Status, iteration)
	}
	if err := os.Remove(filepath.Join(root, "updated/a.txt")); err != nil {
		t.Fatal(err)
	}
	r, err := c.Load(r.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	execute(t, c, r, true)
	if r.Status != "succeeded" || attempts["each[first]/update/a"] != 1 {
		t.Fatal(r.Status, attempts, r.Selections["each"].Iterations)
	}
	if text, _ := read(t, filepath.Join(root, "updated/a.txt")); text != "updated" {
		t.Fatal(text)
	}
}

func TestControllerSecretFileReadPermissions(t *testing.T) {
	for _, tc := range []struct {
		mode            string
		group, everyone bool
		refusal         string
	}{
		{"0700", false, false, ""}, {"0622", false, false, ""},
		{"0640", false, false, "secret_readable_by_group: true"},
		{"0640", true, false, ""},
		{"0644", true, false, "dangerously_let_every_account_read_secret: true"},
		{"0644", false, true, ""},
	} {
		t.Run(tc.mode+tc.refusal, func(t *testing.T) {
			root := t.TempDir()
			w := fleetStageWalker{run: &Run{}}
			entry := config.SaveOnControllerMachine{Path: "secret", ProjectRoot: root, PermissionsMode: tc.mode, Template: "secret.tmpl", TemplateText: "{{ .secrets.password }}", SecretReadableByGroup: tc.group, DangerouslyLetEveryAccountReadSecret: tc.everyone}
			_, r := w.saveFile("collect", entry, 0, "", value.Scope{"secrets": map[string]any{"password": value.Sensitive("saved-secret")}})
			if tc.refusal != "" {
				if r.Status != "failed" || !strings.Contains(r.Reason, tc.refusal) {
					t.Fatal(r)
				}
				if !strings.Contains(r.Reason, filepath.Join(root, "secret")+" holds a secret, and permissions_mode "+tc.mode) {
					t.Fatal("refusal must name the controller-machine file and mode", r)
				}
				if _, err := os.Stat(filepath.Join(root, "secret")); !os.IsNotExist(err) {
					t.Fatal("refused write changed path", err)
				}
			} else {
				if r.Status != "succeeded" || !r.Secret {
					t.Fatal(r)
				}
				if text, mode := read(t, filepath.Join(root, "secret")); text != "saved-secret" || fmt.Sprintf("%04o", mode) != tc.mode {
					t.Fatal(text, mode)
				}
			}
		})
	}
}
