package endpoint

import (
	"context"
	"errors"
	"io"
	"mmx/internal/node"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mmx/internal/assets"

	"golang.org/x/crypto/ssh"
)

type unreadableBody struct{ t *testing.T }

func (r unreadableBody) Read([]byte) (int, error) {
	r.t.Error("read upload body before authenticating metadata")
	return 0, io.EOF
}
func (r unreadableBody) Close() error { return nil }
func TestUploadRequiresAuthenticatedConnectionBeforeBody(t *testing.T) {
	ref := assets.Ref{SHA256: strings.Repeat("a", 64), Size: 64 * 1024 * 1024}
	s := &Server{Info: Info{TLSPin: "pin"}, Authorize: func(context.Context, string, ssh.PublicKey, string, string) error { return nil }}
	r := httptest.NewRequestWithContext(s.ConnContext(context.Background(), nil), "PUT", assetPath(strings.Repeat("b", 64), ref), unreadableBody{t})
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 403 {
		t.Fatal(w.Code, w.Body.String())
	}
}
func TestProgressingUploadOutlivesControlRequestDeadline(t *testing.T) {
	// Keep this above the ordinary 30-second request deadline. No VM or root is
	// required to exercise the actual client streaming and timeout behavior.
	if testing.Short() {
		t.Skip("32-second upload")
	}
	signer := testSigner(t)
	size := int64(32)
	identity, err := LoadIdentity(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	fake := &Server{}
	handler := http.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/session" {
			io.WriteString(w, `{"status":"authenticated"}`)
			return
		}
		n, err := io.Copy(io.Discard, r.Body)
		if err != nil || n != size {
			t.Errorf("upload %d: %v", n, err)
		}
		io.WriteString(w, `{"status":"uploaded"}`)
	}))
	// Native root execution exercises the real server, including its ordinary
	// request deadlines, admission staging, authentication and stream verification.
	if os.Geteuid() == 0 {
		root := t.TempDir()
		if err := os.Chmod(root, 0700); err != nil {
			t.Fatal(err)
		}
		fake = &Server{Node: node.Server{Root: root, RuntimeRoot: t.TempDir()}, Info: Info{TLSPin: identity.Pin}, Authorize: func(context.Context, string, ssh.PublicKey, string, string) error { return nil }}
		handler = fake
	}
	server := tlsServer(t, identity, fake, handler)
	if os.Geteuid() == 0 {
		server.Config.ReadTimeout = 30 * time.Second
		server.Config.WriteTimeout = 30 * time.Second
	}
	server.StartTLS()
	client := NewClient(server.URL, "admin", identity.Pin, []ssh.Signer{signer})
	defer client.Close()
	input := &slowSource{Reader: strings.NewReader(strings.Repeat("x", int(size)))}
	ref, _ := assets.Hash(context.Background(), strings.NewReader(strings.Repeat("x", int(size))))
	if err := client.Upload(context.Background(), strings.Repeat("a", 64), ref, input, nil); err != nil {
		t.Fatal(err)
	}
}

type slowSource struct{ *strings.Reader }

func (r *slowSource) Read(p []byte) (int, error) {
	if r.Len() == 0 {
		return 0, io.EOF
	}
	time.Sleep(time.Second)
	return r.Reader.Read(p[:min(1, len(p))])
}

func TestAuthenticatedUploadVerifiesAndRejectsCorruption(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("native Linux root test")
	}
	signer := testSigner(t)
	identity, err := LoadIdentity(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	handler := &Server{Node: node.Server{Root: t.TempDir(), RuntimeRoot: t.TempDir()}, Info: Info{TLSPin: identity.Pin}, Authorize: func(context.Context, string, ssh.PublicKey, string, string) error { return nil }}
	if err := os.Chmod(handler.Node.Root, 0700); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{}, 8)
	server := tlsServer(t, identity, handler, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler.ServeHTTP(w, r)
		if r.Method == "PUT" {
			done <- struct{}{}
		}
	}))
	server.Config.ReadTimeout = 30 * time.Second
	server.Config.WriteTimeout = 30 * time.Second
	server.StartTLS()
	wait := func() {
		t.Helper()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("upload handler did not finish")
		}
	}
	client := NewClient(server.URL, "admin", identity.Pin, []ssh.Signer{signer})
	defer client.Close()
	data := strings.Repeat("x", 2*1024*1024)
	ref, _ := assets.Hash(context.Background(), strings.NewReader(data))
	id := strings.Repeat("e", 64)
	if err := client.Upload(context.Background(), id, ref, strings.NewReader("short"), nil); err == nil {
		t.Fatal("accepted short body")
	}
	wait()
	if err := client.Upload(context.Background(), id, ref, strings.NewReader(strings.Repeat("z", len(data))), nil); err == nil {
		t.Fatal("accepted wrong digest")
	}
	wait()
	if err := client.Upload(context.Background(), id, ref, strings.NewReader(data), nil); err != nil {
		t.Fatal(err)
	}
	wait()
	ref.Path = filepath.Join(handler.Node.Root, "uploads", id, ref.SHA256)
	if err := assets.Verify(context.Background(), ref); err != nil {
		t.Fatal(err)
	}
	// Lost upload confirmation can resend a whole file. It cannot replace one
	// after the job's instructions have been admitted.
	if err := client.Upload(context.Background(), id, ref, strings.NewReader(data), nil); err != nil {
		t.Fatal(err)
	}
	wait()
	cancelID := strings.Repeat("f", 64)
	ctx, cancel := context.WithCancel(context.Background())
	err = client.Upload(ctx, cancelID, ref, strings.NewReader(data), func(int64) error { cancel(); return nil })
	if !errors.Is(err, context.Canceled) {
		t.Fatal("cancellation", err)
	}
	wait()
	entries, _ := os.ReadDir(filepath.Join(handler.Node.Root, "uploads", cancelID))
	if len(entries) != 0 {
		t.Fatal("canceled upload left files", entries)
	}
	if _, err := os.Stat(filepath.Join(handler.Node.Root, "jobs", cancelID, "request.json")); !os.IsNotExist(err) {
		t.Fatal("upload admitted work", err)
	}
	if err := os.WriteFile(filepath.Join(handler.Node.Root, "jobs", id, "request.json"), []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := client.Upload(context.Background(), id, ref, strings.NewReader(data), nil); err == nil {
		t.Fatal("replaced admitted file")
	}
}
