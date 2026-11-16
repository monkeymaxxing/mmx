package node

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"mmx/internal/assets"
	"mmx/internal/protocol"
)

// A node keeps the files of an admitted job that the job names to keep, and
// gives them to a later job, so the controller sends only the files the node
// lacks. A file that the job does not name, such as one from the project, is
// not kept, nor is a name that is no SHA-256.
func TestNodeKeepsFilesForLaterJobs(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	s := Server{Root: t.TempDir(), RuntimeRoot: t.TempDir(), now: func() time.Time { return now }}
	first, second := strings.Repeat("a", 64), strings.Repeat("b", 64)
	contents := map[string]string{}
	saved := func(text string) assets.Ref {
		ref, _ := assets.Hash(context.Background(), bytes.NewReader([]byte(text)))
		contents[ref.SHA256] = text
		if err := assets.Receive(context.Background(), s.assetDir(first), ref, strings.NewReader(text)); err != nil {
			t.Fatal(err)
		}
		return ref
	}
	script, description := saved("def run_node_task(inputs):\n    return None\n"), saved("summary: x\n")
	program, key := saved("the program's bytes"), saved("a private key from the project")

	// Nothing is kept before a job with these files is admitted.
	if s.useKeptFile(second, script) {
		t.Fatal("used a file the node never kept")
	}
	s.keepFiles([]assets.Ref{script, description, program, {SHA256: "../escape", Size: 1}}, first)
	if err := os.MkdirAll(s.assetDir(second), 0700); err != nil {
		t.Fatal(err)
	}
	for _, ref := range []assets.Ref{script, description, program} {
		if !s.useKeptFile(second, ref) {
			t.Fatalf("did not use the kept file %q", contents[ref.SHA256])
		}
		ref.Path = filepath.Join(s.assetDir(second), ref.SHA256)
		if err := assets.Verify(context.Background(), ref); err != nil {
			t.Fatal("the later job lacks a kept file", err)
		}
	}
	if s.useKeptFile(second, key) {
		t.Fatal("kept a file that the job did not name")
	}
	if _, err := os.Lstat(filepath.Join(s.Root, "escape")); !os.IsNotExist(err) {
		t.Fatal("kept a file under a name that is no SHA-256", err)
	}

	// A kept file whose bytes changed is removed and sent again.
	kept := filepath.Join(s.keptFilesDir(), description.SHA256)
	if err := os.Remove(kept); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(kept, []byte("summary: changed\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if s.useKeptFile(strings.Repeat("c", 64), description) {
		t.Fatal("used a kept file whose bytes changed")
	}
	if _, err := os.Stat(kept); !os.IsNotExist(err) {
		t.Fatal("kept a file whose bytes changed", err)
	}

	// A file no run used for 30 days is removed; a recently used one stays.
	now = now.Add(29 * 24 * time.Hour)
	if err := os.MkdirAll(s.assetDir(strings.Repeat("d", 64)), 0700); err != nil {
		t.Fatal(err)
	}
	if !s.useKeptFile(strings.Repeat("d", 64), script) {
		t.Fatal("did not use a kept file")
	}
	unused := filepath.Join(s.keptFilesDir(), strings.Repeat("e", 64))
	if err := os.WriteFile(unused, []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	old := now.Add(-31 * 24 * time.Hour)
	if err := os.Chtimes(unused, old, old); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * 24 * time.Hour)
	if warnings := s.removeUnusedKeptFiles(); len(warnings) > 0 {
		t.Fatal(warnings)
	}
	if _, err := os.Stat(unused); !os.IsNotExist(err) {
		t.Fatal("kept a file no run used for 30 days", err)
	}
	if _, err := os.Stat(filepath.Join(s.keptFilesDir(), script.SHA256)); err != nil {
		t.Fatal("removed a file a run used recently", err)
	}
}

// The request answers with the files to send and refuses an admitted job.
func TestKeptFilesRequest(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("native Linux root test")
	}
	s := Server{Root: t.TempDir(), RuntimeRoot: t.TempDir()}
	if err := os.Chmod(s.Root, 0700); err != nil {
		t.Fatal(err)
	}
	id := strings.Repeat("a", 64)
	kept, _ := assets.Hash(context.Background(), strings.NewReader("kept"))
	missing, _ := assets.Hash(context.Background(), strings.NewReader("missing"))
	if err := assets.Receive(context.Background(), s.keptFilesDir(), kept, strings.NewReader("kept")); err != nil {
		t.Fatal(err)
	}
	response, err := s.UseKeptFiles(protocol.KeptFilesRequest{JobID: id, Files: []assets.Ref{kept, missing}})
	if err != nil || !slices.Equal(response.FilesToSend, []string{missing.SHA256}) {
		t.Fatal(response, err)
	}
	if err := os.WriteFile(filepath.Join(s.jobDir(id), "request.json"), []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.UseKeptFiles(protocol.KeptFilesRequest{JobID: id, Files: []assets.Ref{kept}}); err == nil {
		t.Fatal("added kept files to an admitted job")
	}
}
