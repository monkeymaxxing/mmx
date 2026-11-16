package differences

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mmx/internal/assets"
	"mmx/internal/protocol"
)

func TestBudgetDeduplicationAndSuppressionBeforeStorage(t *testing.T) {
	s := Store{Dir: filepath.Join(t.TempDir(), "diffs")}
	c := s.Task()
	ref, omission := c.Add(strings.Repeat("x", Budget))
	if ref == nil || omission != "" {
		t.Fatal(ref, omission)
	}
	if _, err := os.Stat(s.Dir); !os.IsNotExist(err) {
		t.Fatal("staged text reached disk", err)
	}
	if duplicate, _ := c.Add(strings.Repeat("x", Budget)); duplicate == nil {
		t.Fatal("duplicate charged twice")
	}
	if extra, reason := c.Add("z"); extra != nil || !strings.Contains(reason, "8 MiB limit") {
		t.Fatal(extra, reason)
	}
	r := c.Finish(protocol.Result{Secret: true, FileChange: &protocol.FileChange{FullDiff: ref}})
	if r.FileChange.FullDiff != nil || s.used != 0 {
		t.Fatal("secret reference retained")
	}
	if _, err := os.Stat(s.Dir); !os.IsNotExist(err) {
		t.Fatal("secret artifact stored", err)
	}
	c = s.Task()
	ref, _ = c.Add(strings.Repeat("y", Budget))
	r = c.Finish(protocol.Result{FileChange: &protocol.FileChange{FullDiff: ref}})
	if err := assets.Verify(context.Background(), Local(s.Dir, *r.FileChange.FullDiff)); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(filepath.Join(s.Dir, ref.SHA256)); st.Mode().Perm() != 0600 {
		t.Fatal(st.Mode())
	}
	c = s.Task()
	if ref, _ := c.Add(strings.Repeat("y", Budget)); ref == nil {
		t.Fatal("retained duplicate charged twice")
	}
	if ref, reason := c.Add("new"); ref != nil || !strings.Contains(reason, "8 MiB limit") {
		t.Fatal(ref, reason)
	}
}

func TestFailedArtifactSaveDoesNotChangeTaskOutcome(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(dir, []byte("occupied"), 0600); err != nil {
		t.Fatal(err)
	}
	s := Store{Dir: dir}
	c := s.Task()
	ref, _ := c.Add("+new\n")
	r := c.Finish(protocol.Result{Status: "succeeded", Change: "changed", FileChange: &protocol.FileChange{FullDiff: ref}})
	if r.Status != "succeeded" || r.FileChange.FullDiff != nil || !strings.Contains(r.FileChange.FullDiffOmitted, "could not be saved") {
		t.Fatal(r)
	}
}

func TestReferencesIncludeTriesAndRejectUnboundedMetadata(t *testing.T) {
	ref := &assets.Ref{SHA256: strings.Repeat("a", 64), Size: Budget}
	r := protocol.Result{FileChange: &protocol.FileChange{FullDiff: ref}}
	r.Tries = []protocol.Result{r}
	response := protocol.Response{Results: []protocol.Result{r}}
	refs, err := Refs(response)
	if err != nil || len(refs) != 1 {
		t.Fatal(refs, err)
	}
	ref.Size++
	if _, err := Refs(response); err == nil {
		t.Fatal("oversized reference accepted")
	}
	ref.Size = 1
	ref.SHA256 = "../escape"
	if _, err := Refs(response); err == nil {
		t.Fatal("unsafe reference accepted")
	}
}
