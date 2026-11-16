package access

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

func TestCredentialsAreLazyAndNoninteractive(t *testing.T) {
	called := 0
	c := &Credentials{Prompt: func(string) (string, error) { called++; return "interactive-value", nil }}
	if _, err := c.Get("SSH password", "node", ""); err == nil || called != 0 {
		t.Fatal("noninteractive prompt")
	}
	t.Setenv("MMX_TEST_LOGIN", "separate-login-value")
	t.Setenv("MMX_TEST_SUDO", "separate-sudo-value")
	login, err := c.Get("SSH password", "node", "MMX_TEST_LOGIN")
	if err != nil || login != "separate-login-value" {
		t.Fatal(err)
	}
	sudo, err := c.Get("sudo password", "node", "MMX_TEST_SUDO")
	if err != nil || sudo != "separate-sudo-value" {
		t.Fatal(err)
	}
	if text := c.Redact(login + " " + sudo); strings.Contains(text, login) || strings.Contains(text, sudo) {
		t.Fatal("credential retained in diagnostic")
	}
	c.Interactive = true
	if _, err := c.Get("sudo password", "another", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Get("sudo password", "another", ""); err != nil || called != 1 {
		t.Fatal("password requested again")
	}
	if _, err := c.Get("SSH password", "node", "MMX_TEST_MISSING_INPUT"); err == nil || called != 1 {
		t.Fatal("missing configured input prompted")
	}
}

func TestManagedKeySurvivesRepeatedSetup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys", "node")
	first, err := EnsureKey(path)
	if err != nil {
		t.Fatal(err)
	}
	second, err := EnsureKey(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first.PublicKey().Marshal(), second.PublicKey().Marshal()) {
		t.Fatal("setup replaced the key")
	}
	for _, file := range []string{path, path + ".pub"} {
		st, err := os.Stat(file)
		if err != nil || st.Mode().Perm() != 0600 {
			t.Fatal("key permissions", err)
		}
	}
}

func TestAgentAndEncryptedKeySelection(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "mmx-agent-test-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	public, err := ssh.NewPublicKey(key.Public())
	if err != nil {
		t.Fatal(err)
	}
	publicPath := filepath.Join(dir, "agent.pub")
	if err := os.WriteFile(publicPath, ssh.MarshalAuthorizedKey(public), 0644); err != nil {
		t.Fatal(err)
	}
	ring := agent.NewKeyring()
	if err := ring.Add(agent.AddedKey{PrivateKey: key}); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", filepath.Join(dir, "socket"))
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() { defer conn.Close(); _ = agent.ServeAgent(ring, conn) }()
		}
	}()
	credentials := &Credentials{}
	signers, closeAgent, err := Signers([]string{publicPath}, listener.Addr().String(), true, credentials)
	if err != nil {
		t.Fatal(err)
	}
	defer closeAgent()
	if len(signers) != 1 {
		t.Fatal("public identity did not select the SSH-agent key")
	}
	signature, err := signers[0].Sign(rand.Reader, []byte("challenge"))
	if err != nil || public.Verify([]byte("challenge"), signature) != nil {
		t.Fatal("agent signing failed", err)
	}
	block, err := ssh.MarshalPrivateKeyWithPassphrase(key, "test", []byte("unlock-value"))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "encrypted")
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0600); err != nil {
		t.Fatal(err)
	}
	calls := 0
	credentials = &Credentials{Interactive: true, Prompt: func(string) (string, error) { calls++; return "unlock-value", nil }}
	locked, closeLocked, err := Signers([]string{path}, "", true, credentials)
	if err != nil {
		t.Fatal(err)
	}
	defer closeLocked()
	if calls != 0 || len(locked) != 1 {
		t.Fatal("key loading prompted before it was needed")
	}
	for i := 0; i < 2; i++ {
		signature, err := locked[0].Sign(rand.Reader, []byte("challenge"))
		if err != nil || public.Verify([]byte("challenge"), signature) != nil {
			t.Fatal(err)
		}
	}
	if calls != 1 {
		t.Fatal("key passphrase was not reused")
	}
}
