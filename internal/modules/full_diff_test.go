package modules

import (
	"path/filepath"
	"strings"
	"testing"

	"mmx/internal/assets"
	"mmx/internal/differences"
	"mmx/internal/protocol"
)

func TestFullDifferenceInputAndPreviewBoundaries(t *testing.T) {
	t.Parallel()
	// A directory others can pass through, so the file is not private by
	// where it is: the test's working directory may be a private home directory.
	path := filepath.Join(traversableTempDir(t), "app.conf")
	for _, size := range []int{64 * 1024, 64*1024 + 1, differences.InputLimit, differences.InputLimit + 1} {
		for _, before := range []bool{false, true} {
			old, next := []byte{}, []byte(strings.Repeat("x", size-1)+"\n")
			if before {
				old, next = next, []byte("new\n")
			}
			c := &protocol.FileChange{Path: path, After: &protocol.FileState{Mode: "0644"}}
			var full string
			r := Runtime{RetainDiff: func(text string) (*assets.Ref, string) { full = text; return &assets.Ref{}, "" }}
			r.textDiff(c, old, next)
			if (full != "") != (size <= differences.InputLimit) {
				t.Fatal(size, len(full), c.FullDiffOmitted)
			}
			if size > differences.InputLimit && !strings.Contains(c.FullDiffOmitted, "1 MiB") {
				t.Fatal(c)
			}
			if size > 64*1024 && !strings.Contains(c.DiffOmitted, "64 KiB") {
				t.Fatal(c)
			}
			if len(c.Diff) > 16*1024 || strings.Count(c.Diff, "\n") > 200 {
				t.Fatal("unbounded preview")
			}
			if full != "" && !strings.Contains(full, strings.Repeat("x", size-1)) {
				t.Fatal("full difference truncated")
			}
		}
	}
	for _, mode := range []string{"0600", "0644"} {
		c := &protocol.FileChange{Path: path, After: &protocol.FileState{Mode: mode}}
		r := Runtime{RetainDiff: func(string) (*assets.Ref, string) { t.Fatal("suppressed content offered to storage"); return nil, "" }}
		r.textDiff(c, nil, []byte("binary\x00"))
		if c.FullDiffOmitted == "" {
			t.Fatal(c)
		}
	}
}
