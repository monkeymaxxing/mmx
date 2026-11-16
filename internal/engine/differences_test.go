package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mmx/internal/config"
	"mmx/internal/differences"
	"mmx/internal/modules"
	"mmx/internal/value"
)

func TestSecretValidatorSuppressesArtifactBeforeItIsStored(t *testing.T) {
	root := t.TempDir()
	e := Engine{RuntimeRoot: root, DiffDir: filepath.Join(root, "diffs")}
	content := "public proposed contents\n"
	j := config.Job{ID: "test", Node: "test", Scope: map[string]any{"secrets": map[string]any{"token": value.Sensitive("password")}}, NodeTasks: []config.Task{{Module: "file", ModuleInputs: &modules.File{Path: filepath.Join(root, "config"), Content: &content, Mode: "0644", CheckNewFile: &modules.Check{Shell: &modules.Shell{Script: `test "$TOKEN" = password`, Env: map[string]any{"TOKEN": "{{ .secrets.token }}"}}}}}}}
	r := e.Run(context.Background(), j)
	if r.Status != "succeeded" {
		t.Fatal(r)
	}
	refs, err := differences.Refs(r)
	if err != nil || len(refs) != 0 {
		t.Fatal(refs, err)
	}
	if files, _ := os.ReadDir(e.DiffDir); len(files) != 0 {
		t.Fatal("secret validator caused persisted difference")
	}
	if c := r.Results[0].FileChange; c.FullDiff != nil || !strings.Contains(c.FullDiffOmitted, "secrets") {
		t.Fatal(c)
	}
}
