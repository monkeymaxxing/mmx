package main

import (
	"context"
	"os"
	"strings"
	"testing"
)

func TestInitHelpDoesNotCreateFilesAndInitPreservesExistingWork(t *testing.T) {
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(old)
	code, err := run(context.Background(), []string{"init", "--help"})
	if code != 0 || err != nil {
		t.Fatal(code, err)
	}
	entries, _ := os.ReadDir(".")
	if len(entries) != 0 {
		t.Fatal("help created project files")
	}
	if err := os.WriteFile(".gitignore", []byte("existing-rule"), 0644); err != nil {
		t.Fatal(err)
	}
	code, err = run(context.Background(), []string{"init"})
	if code != 0 || err != nil {
		t.Fatal(code, err)
	}
	// The example node task has a descriptive ID, so the output names it
	// instead of task_1.
	if playbook, _ := os.ReadFile("playbooks/example.yml"); !strings.Contains(string(playbook), "    node_tasks:\n      - id: create_example_file\n        file:\n") {
		t.Fatalf("the example node task has no descriptive ID:\n%s", playbook)
	}
	ignore, _ := os.ReadFile(".gitignore")
	if !strings.Contains(string(ignore), "existing-rule\n/.mmx/\n") {
		t.Fatal("ignore rule did not preserve existing content")
	}
	if err := os.WriteFile("mmx_project_settings.yml", []byte("user edits"), 0644); err != nil {
		t.Fatal(err)
	}
	code, err = run(context.Background(), []string{"init"})
	if code == 0 || err == nil {
		t.Fatal("init overwrote existing project")
	}
	b, _ := os.ReadFile("mmx_project_settings.yml")
	if string(b) != "user edits" {
		t.Fatal("existing work lost")
	}
}
