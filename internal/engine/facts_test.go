package engine

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mmx/internal/config"
	"mmx/internal/facts"
	"mmx/internal/modules"
	"mmx/internal/protocol"
)

func TestFactsStayFixedDuringAJobAndRefreshForTheNextJob(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config")
	memory, calls := int64(4096), 0
	e := Engine{collectFacts: func(name string) facts.Snapshot {
		calls++
		return facts.Snapshot{CollectedAt: time.Now(), Values: map[string]any{"name": name, "memory": map[string]any{"total_mib": memory}}}
	}}
	e.BeforeMutation = func(string) error { memory = 8192; return nil }
	reports := 0
	e.Report = func(r protocol.Response) error {
		reports++
		if r.Facts == nil || r.Facts.Values["memory"].(map[string]any)["total_mib"] != int64(4096) {
			t.Fatal("lost initial snapshot", r.Facts)
		}
		return nil
	}
	j := job(config.Task{Module: "file", ModuleInputs: &modules.File{Mode: "0644", Path: path, Template: "sizing", TemplateText: `cache={{ percent .node.memory.total_mib 25 }}`}}, config.Task{Show: "{{ .node.memory.total_mib }}"})
	first := e.Run(context.Background(), j)
	requireStatus(t, first, "succeeded")
	if calls != 1 || reports < 3 || read(t, path) != "cache=1024" || first.Results[1].Value != int64(4096) {
		t.Fatal(calls, reports, first)
	}
	e.Report = nil
	second := e.Run(context.Background(), j)
	requireStatus(t, second, "succeeded")
	if calls != 2 || read(t, path) != "cache=2048" || second.Results[0].Change != "changed" {
		t.Fatal(second)
	}
	if first.Facts.Values["memory"].(map[string]any)["total_mib"] != int64(4096) {
		t.Fatal("mutated an earlier observation")
	}
	j.Dryrun = true
	third := e.Run(context.Background(), j)
	if calls != 3 || third.Results[0].Change != "unchanged" {
		t.Fatal(third)
	}
}
func TestUnavailableFactsOnlyBlockConsumersIncludingRoles(t *testing.T) {
	e := Engine{collectFacts: func(string) facts.Snapshot {
		return facts.Snapshot{Values: map[string]any{"hostname": "known", "memory": map[string]any{}}, Unavailable: map[string]string{"/node/memory/total_mib": "memory inspection failed"}}
	}}
	path := filepath.Join(t.TempDir(), "config")
	j := job(config.Task{When: false, Show: "{{ .node.memory.total_mib }}"}, taskFile(path, "unrelated"), config.Task{Show: "{{ .node.hostname }}"})
	r := e.Run(context.Background(), j)
	requireStatus(t, r, "succeeded")
	if read(t, path) != "unrelated" {
		t.Fatal(r)
	}
	j = job(config.Task{Role: "sized", RoleNodeTasks: []config.Task{{Module: "file", ModuleInputs: &modules.File{Mode: "0644", Path: path, Template: "sizing", TemplateText: `cache={{ percent .node.memory.total_mib 25 }}`}}}})
	r = e.Run(context.Background(), j)
	if r.Status != "blocked" || !strings.Contains(r.Error, "memory inspection failed") || read(t, path) != "unrelated" {
		t.Fatal(r)
	}
	j.Dryrun = true
	preview := e.Run(context.Background(), j)
	if preview.Results[0].Status != "not_checked" || read(t, path) != "unrelated" {
		t.Fatal(preview)
	}
	e.Report = func(protocol.Response) error { return errors.New("save failed") }
	r = e.Run(context.Background(), job(taskFile(path, "must not write")))
	if r.Status != "failed" || read(t, path) != "unrelated" {
		t.Fatal(r)
	}
}
