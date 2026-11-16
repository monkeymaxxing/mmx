package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestModuleUsageShowsKindsFilesAndCalls(t *testing.T) {
	t.Setenv("MMX_RUNNER_DIR", t.TempDir())
	dir := t.TempDir()
	t.Chdir(dir)
	description := func(name string) string {
		return "summary: x\nexamples: [{description: x, config: '" + name + ": {}'}]\n"
	}
	for path, text := range map[string]string{
		"mmx_project_settings.yml":                              "project_name: test\ndefault_inventory: example\n",
		"inventories/example.yml":                               "nodes:\n  web01: {address: 192.0.2.10}\n",
		"playbooks/deploy.yml":                                  "fleet_stages:\n  - id: web\n    nodes: all\n    node_tasks:\n      - file: {permissions_mode: '0644', path: /tmp/a, content: a}\n      - greeting: {}\n      - role: backup\n",
		"roles/backup/main.yml":                                 "node_tasks:\n  - legacy_backup: {}\n  - file: {permissions_mode: '0644', path: /tmp/b, content: b}\n",
		"external_modules/greeting/module.star":                 "def run_node_task(inputs):\n    return None\n",
		"external_modules/greeting/module_description.yml":      description("greeting"),
		"external_modules/legacy_backup/module_executable":      "#!/usr/bin/env python3\n",
		"external_modules/legacy_backup/module_description.yml": description("legacy_backup"),
		"external_modules/unused/module.star":                   "def run_node_task(inputs):\n    return None\n",
		"external_modules/unused/module_description.yml":        description("unused"),
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(text), 0755); err != nil {
			t.Fatal(err)
		}
	}
	human, err := captureStdout(t, "module-usage", "deploy")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Core modules (built into MMX)\n  file ",
		"playbooks/deploy.yml:5, roles/backup/main.yml:3",
		"The project's own modules\n  greeting ",
		"external_modules/greeting/   Starlark",
		"external_modules/legacy_backup/   executable (Python)",
		"MMX cannot check the executable module's dryrun",
		"no node task calls: unused",
	} {
		if !strings.Contains(human, want) {
			t.Fatalf("missing %q in\n%s", want, human)
		}
	}
	out, err := captureStdout(t, "module-usage", "--module", "legacy_backup", "--json")
	var usage struct {
		Modules []struct {
			Name, Kind         string
			DryrunCheckedByMMX bool `json:"dryrun_checked_by_mmx"`
			Calls              []struct{ File string }
		}
	}
	if err != nil || json.Unmarshal([]byte(out), &usage) != nil || len(usage.Modules) != 1 || usage.Modules[0].Kind != "project_executable" ||
		usage.Modules[0].DryrunCheckedByMMX || usage.Modules[0].Calls[0].File != "roles/backup/main.yml" {
		t.Fatal(out, err)
	}
}

// captureStdout runs one mmx command and returns what it printed.
func captureStdout(t *testing.T, args ...string) (string, error) {
	t.Helper()
	stdout, err := os.CreateTemp(t.TempDir(), "stdout")
	if err != nil {
		t.Fatal(err)
	}
	saved := os.Stdout
	os.Stdout = stdout
	code, err := run(context.Background(), args)
	os.Stdout = saved
	stdout.Close()
	b, _ := os.ReadFile(stdout.Name())
	if err == nil && code != 0 {
		err = fmt.Errorf("exit code %d", code)
	}
	return string(b), err
}
