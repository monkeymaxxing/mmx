package node

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mmx/internal/protocol"
)

func TestReadFileAnswersAsReadFileReads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(path, []byte("token"), 0600); err != nil {
		t.Fatal(err)
	}
	s := Server{}
	if answer, err := s.ReadFile(protocol.ReadFileRequest{Path: path}); err != nil || answer.Content != "token" {
		t.Fatal(answer, err)
	}
	if _, err := s.ReadFile(protocol.ReadFileRequest{Path: "token"}); err == nil || !strings.Contains(err.Error(), "absolute file path") {
		t.Fatal(err)
	}
	if _, err := s.ReadFile(protocol.ReadFileRequest{Path: path + ".gone"}); err == nil || !strings.Contains(err.Error(), "file does not exist") {
		t.Fatal(err)
	}
}
