package main

import (
	"os"
	"path/filepath"
	"testing"

	"mmx/internal/config"
)

// Every example project in examples/ validates with each of its playbooks and
// each of its inventories, through the code that mmx validate runs. A
// directory of examples/ without project settings, such as
// examples/external-modules, holds no project.
func TestEveryExampleProjectValidatesWithEachPlaybookAndEachInventory(t *testing.T) {
	repository, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	examples := filepath.Join(repository, "examples")
	if _, err := os.Stat(examples); err != nil {
		t.Skip("the test binary runs outside the repository, as on a test node")
	}
	// An example that calls an official extra module finds the repository's
	// official_extra_modules/, as mmx finds the directory beside itself.
	t.Setenv("MMX_RUNNER_DIR", repository)
	// Validation saves no run; no command of this test may write into an example.
	t.Setenv("MMX_STATE_DIR", t.TempDir())
	settingsFiles, err := filepath.Glob(filepath.Join(examples, "*", "mmx_project_settings.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(settingsFiles) == 0 {
		t.Fatal("no example project in " + examples)
	}
	for _, settingsFile := range settingsFiles {
		project := filepath.Dir(settingsFile)
		example := filepath.Base(project)
		t.Run(example, func(t *testing.T) {
			playbooks, err := config.ProjectNames(settingsFile, "playbooks")
			if err != nil {
				t.Fatal(err)
			}
			inventories, err := config.ProjectNames(settingsFile, "inventories")
			if err != nil {
				t.Fatal(err)
			}
			if len(playbooks) == 0 || len(inventories) == 0 {
				t.Fatalf("example %s: needs at least one playbook and one inventory; playbooks %v, inventories %v", example, playbooks, inventories)
			}
			t.Chdir(project)
			for _, playbook := range playbooks {
				for _, inventory := range inventories {
					if _, code, err := captureCommand(t, "validate", playbook, "--inventory", inventory, "--json"); code != 0 || err != nil {
						t.Errorf("example %s, playbook %s, inventory %s: mmx validate exits %d: %v", example, playbook, inventory, code, err)
					}
				}
			}
		})
	}
}
