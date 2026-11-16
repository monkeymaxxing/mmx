package durable

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReplacementIsPrivateAndFailedWriteKeepsPriorFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	if err := JSON(path, map[string]any{"state": "old"}); err != nil {
		t.Fatal(err)
	}
	if err := JSON(path, make(chan int)); err == nil {
		t.Fatal("unsupported value encoded")
	}
	b, err := os.ReadFile(path)
	if err != nil || string(b) != "{\n  \"state\": \"old\"\n}\n" {
		t.Fatalf("failed write changed state: %s %v", b, err)
	}
	st, _ := os.Stat(path)
	if st.Mode().Perm() != 0600 {
		t.Fatal("state is not private")
	}
	if err := JSON(path, map[string]any{"state": "new"}); err != nil {
		t.Fatal(err)
	}
	files, _ := os.ReadDir(dir)
	if len(files) != 1 {
		t.Fatal("temporary files leaked")
	}
}

func TestReplaceIsAtomicAndPrivate(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "progress.json")
	for _, content := range []string{"first", "second"} {
		if err := Replace(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	b, err := os.ReadFile(path)
	st, _ := os.Stat(path)
	files, _ := os.ReadDir(dir)
	if err != nil || string(b) != "second" || st.Mode().Perm() != 0600 || len(files) != 1 {
		t.Fatalf("%q %v %v %d files", b, err, st.Mode(), len(files))
	}
}
