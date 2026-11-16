package node

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mmx/internal/assets"
	"mmx/internal/config"
	"mmx/internal/modules"
	"mmx/internal/protocol"
)

func TestUploadAdmissionAndImmutability(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("native Linux root test")
	}
	s := Server{Root: t.TempDir(), RuntimeRoot: t.TempDir()}
	if err := os.Chmod(s.Root, 0700); err != nil {
		t.Fatal(err)
	}
	id := strings.Repeat("a", 64)
	data := []byte("verified file")
	ref, _ := assets.Hash(context.Background(), bytes.NewReader(data))
	job := config.Job{Protocol: protocol.Version, ID: id, Project: "test", FleetStage: "copy", Node: "test", NodeTasks: []config.Task{{Module: "file", ModuleInputs: &modules.File{Mode: "0644", Path: "/tmp/destination", Source: "file", Asset: &ref}}}}
	raw, _ := json.Marshal(job)
	if _, err := s.Submit(raw, modules.ControllerConnection{}); err == nil || !strings.Contains(err.Error(), "required asset") {
		t.Fatal("admitted missing asset", err)
	}
	if _, err := os.Stat(filepath.Join(s.jobDir(id), "request.json")); !os.IsNotExist(err) {
		t.Fatal("admitted incomplete job", err)
	}
	if err := s.Upload(context.Background(), id, ref, bytes.NewReader(data[:3])); err == nil {
		t.Fatal("accepted incomplete upload")
	}
	if err := s.Upload(context.Background(), id, ref, bytes.NewReader(data)); err != nil {
		t.Fatal(err)
	}
	if err := s.verifyAssets(job.NodeTasks, job.Modules, id); err != nil {
		t.Fatal(err)
	}
	// Exercise the same exclusion used by admission without starting a system job.
	l, err := s.assetLock(id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Submit(raw, modules.ControllerConnection{}); err == nil {
		t.Fatal("admitted while upload owns assets")
	}
	if err := s.Upload(context.Background(), id, ref, bytes.NewReader(data)); err == nil {
		t.Fatal("second upload shared the asset lock")
	}
	unlock(l)
	if err := os.WriteFile(filepath.Join(s.jobDir(id), "request.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.Upload(context.Background(), id, ref, bytes.NewReader(data)); err == nil || !strings.Contains(err.Error(), "admitted") {
		t.Fatal("changed admitted assets", err)
	}
}

// The history sweep removes an unsubmitted upload with its job directory once
// it is older than hostRetention; admitted, still transferring, and recent
// uploads stay.
func TestUploadCleanupProtectsAdmittedAndInProgressAssets(t *testing.T) {
	s := Server{Root: t.TempDir(), RuntimeRoot: t.TempDir()}
	ids := []string{strings.Repeat("a", 64), strings.Repeat("b", 64), strings.Repeat("c", 64), strings.Repeat("d", 64)}
	for _, id := range ids {
		l, err := s.assetLock(id)
		if err != nil {
			t.Fatal(err)
		}
		unlock(l)
		os.MkdirAll(s.assetDir(id), 0700)
		os.WriteFile(filepath.Join(s.assetDir(id), ".upload-partial"), []byte("partial"), 0600)
	}
	// b is queued/running, c is still transferring, d is recent.
	os.WriteFile(filepath.Join(s.jobDir(ids[1]), "request.json"), []byte("{}"), 0600)
	l, err := s.assetLock(ids[2])
	if err != nil {
		t.Fatal(err)
	}
	defer unlock(l)
	old := time.Now().Add(-nodeRetention - time.Hour)
	for _, id := range ids[:3] {
		os.Chtimes(s.jobDir(id), old, old)
	}
	if warnings := sweep(t, s, ""); len(warnings) != 0 {
		t.Fatal(warnings)
	}
	if _, err := os.Stat(s.assetDir(ids[0])); !os.IsNotExist(err) {
		t.Fatal("abandoned upload retained", err)
	}
	if _, err := os.Stat(s.jobDir(ids[0])); !os.IsNotExist(err) {
		t.Fatal("abandoned upload's job directory retained", err)
	}
	for _, id := range ids[1:] {
		if _, err := os.Stat(s.assetDir(id)); err != nil {
			t.Fatal("removed protected assets", id, err)
		}
	}
}

func TestDirectorySourceAdmission(t *testing.T) {
	s := Server{Root: t.TempDir(), RuntimeRoot: t.TempDir()}
	id := strings.Repeat("a", 64)
	ref, err := assets.Hash(context.Background(), strings.NewReader("source"))
	if err != nil {
		t.Fatal(err)
	}
	d := modules.Directory{Path: "/target", Source: "files", Entries: []modules.DirectoryEntry{{Path: ".", Directory: true, Mode: 0755}, {Path: "a", Mode: 0644, Asset: &ref}}}
	nodeTasks := []config.Task{{Module: "directory", ModuleInputs: &d}}
	if err := s.verifyAssets(nodeTasks, nil, id); err == nil {
		t.Fatal("accepted missing source asset")
	}
	if err := assets.Receive(context.Background(), s.assetDir(id), ref, strings.NewReader("source")); err != nil {
		t.Fatal(err)
	}
	if err := s.verifyAssets(nodeTasks, nil, id); err != nil {
		t.Fatal(err)
	}
	d.Entries[1].Path = "../escape"
	if err := s.verifyAssets(nodeTasks, nil, id); err == nil {
		t.Fatal("accepted traversal")
	}
	d.Entries[1].Path = "a"
	if err := os.WriteFile(filepath.Join(s.assetDir(id), ref.SHA256), []byte("tampered"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.verifyAssets(nodeTasks, nil, id); err == nil {
		t.Fatal("accepted corrupt source asset")
	}
}
