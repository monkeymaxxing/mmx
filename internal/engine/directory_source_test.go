package engine

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"mmx/internal/config"
	"mmx/internal/modules"
)

func TestDirectoryCopyServiceRecovery(t *testing.T) {
	f := newServiceFixture(t)
	d := modules.Directory{Path: f.dir, Source: "frozen", Entries: []modules.DirectoryEntry{
		{Path: ".", Directory: true, Mode: 0755},
		{Path: "config", Mode: 0644, Asset: fileAsset(t, []byte("A"))},
	}}
	op := f.op("A")
	op.TaskBlock.Changes = []config.Task{{ID: "tree", Module: "directory", ModuleInputs: &d}}
	f.run(t, op, "succeeded")
	f.assert(t, "A", "")
	d.Entries[1].Asset = fileAsset(t, []byte("B"))
	preview := job(op)
	preview.Dryrun = true
	response := f.e.Run(context.Background(), preview)
	requireStatus(t, response, "succeeded")
	if response.Change != "would_change" {
		t.Fatal(response)
	}
	f.assert(t, "A", "")
	op.TaskBlock.Validate = []config.Task{{Module: "shell", ModuleInputs: &modules.Shell{Script: "exit 1"}}}
	f.run(t, op, "failed")
	f.assert(t, "A", "")
	if read(t, f.path) != "B" {
		t.Fatal("fixture did not fail after copying")
	}
	op.TaskBlock.Validate = nil
	response = f.e.Run(context.Background(), job(op))
	requireStatus(t, response, "succeeded")
	if response.Results[0].Change != "unchanged" || response.Results[1].Change != "changed" {
		t.Fatal(response)
	}
	f.assert(t, "B", "reload\n")
	d.FileMode = "0640"
	f.run(t, op, "succeeded")
	f.assert(t, "B", "reload\n")
	f.run(t, op, "succeeded")
	f.assert(t, "B", "reload\n")
	// A false copy condition observes current files rather than desired source.
	d.Entries[1].Asset = fileAsset(t, []byte("C"))
	op.TaskBlock.Changes[0].When = false
	f.run(t, op, "succeeded")
	f.assert(t, "B", "reload\n")
	op.TaskBlock.Changes[0].When = nil
	op.TaskBlock.Changes = append(op.TaskBlock.Changes, taskFile(f.path, "conflict"))
	r := f.e.Run(context.Background(), job(op))
	if r.Status != "failed" || !strings.Contains(r.Error, "duplicate") {
		t.Fatal(r)
	}
	f.assert(t, "B", "reload\n")
}

func TestDirectoryCopyLoopAndLiteralNames(t *testing.T) {
	root := t.TempDir()
	literal := "{{ .secrets.not_an_input }}"
	d := modules.Directory{Path: root + "/{{ .item }}", Source: "files", Entries: []modules.DirectoryEntry{
		{Path: ".", Directory: true, Mode: 0755},
		{Path: literal, Mode: 0644, Asset: fileAsset(t, []byte("literal bytes"))},
	}}
	task := config.Task{ID: "copy", Loop: []any{"a", "b"}, As: "item", Module: "directory", ModuleInputs: &d}
	r := (&Engine{}).Run(context.Background(), job(task))
	requireStatus(t, r, "succeeded")
	if len(r.Results) != 3 || len(r.Results[2].Items) != 2 {
		t.Fatal(r)
	}
	for _, dir := range []string{"a", "b"} {
		if read(t, filepath.Join(root, dir, literal)) != "literal bytes" {
			t.Fatal(dir)
		}
	}
	// Desired hashes must use the literal source path and retained loop bindings.
	f := newServiceFixture(t)
	op := f.op("A")
	op.TaskBlock.Changes = []config.Task{task}
	f.run(t, op, "succeeded")
}
