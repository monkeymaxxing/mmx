package endpoint

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mmx/internal/assets"
	"mmx/internal/node"

	"golang.org/x/crypto/ssh"
)

func TestDifferenceDownloadAuthenticatesAndChecksBytes(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("native Linux root test")
	}
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	identity, err := LoadIdentity(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	denied := false
	s := &Server{Node: node.Server{Root: root, RuntimeRoot: t.TempDir()}, Info: Info{TLSPin: identity.Pin}, Authorize: func(context.Context, string, ssh.PublicKey, string, string) error {
		if denied {
			return fmt.Errorf("key removed")
		}
		return nil
	}}
	server := serveTLS(t, identity, s, nil)
	client := NewClient(server.URL, "admin", identity.Pin, []ssh.Signer{testSigner(t)})
	defer client.Close()
	id := strings.Repeat("b", 64)
	text := "+retained\n"
	ref, _ := assets.Hash(context.Background(), strings.NewReader(text))
	dir := filepath.Join(root, "jobs", id, "diffs")
	if err := assets.Receive(context.Background(), dir, ref, strings.NewReader(text)); err != nil {
		t.Fatal(err)
	}
	if err := client.DownloadDiff(context.Background(), id, ref, t.TempDir()); err != nil {
		t.Fatal(err)
	}
	// A removed key is refused on the next connection.
	denied = true
	client.Close()
	if err := client.DownloadDiff(context.Background(), id, ref, t.TempDir()); err == nil {
		t.Fatal("revoked key read artifact")
	}
	denied = false
	if err := client.DownloadDiff(context.Background(), "../escape", ref, t.TempDir()); err == nil {
		t.Fatal("unsafe job path accepted")
	}
	if err := os.WriteFile(filepath.Join(dir, ref.SHA256), []byte(strings.Repeat("x", len(text))), 0600); err != nil {
		t.Fatal(err)
	}
	if err := client.DownloadDiff(context.Background(), id, ref, t.TempDir()); err == nil {
		t.Fatal("corrupt bytes accepted")
	}
}
