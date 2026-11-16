package access

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"syscall"

	"mmx/internal/durable"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

func KeyPath(destination string) (string, error) {
	home, err := os.UserHomeDir()
	return filepath.Join(home, ".ssh", "mmx", destination), err
}

// Signers uses the agent where available and reads local keys only when needed.
// Explicit identity files restrict agent selection just as IdentitiesOnly does.
func Signers(paths []string, socket string, identitiesOnly bool, credentials *Credentials) ([]ssh.Signer, func(), error) {
	var signers, offered []ssh.Signer
	closeAgent := func() {}
	if socket != "" && socket != "none" {
		if conn, err := net.Dial("unix", socket); err == nil {
			closeAgent = func() { conn.Close() }
			offered, _ = agent.NewClient(conn).Signers()
		}
	}
	seen := map[string]bool{}
	add := func(s ssh.Signer) {
		key := string(s.PublicKey().Marshal())
		if !seen[key] {
			seen[key] = true
			signers = append(signers, s)
		}
	}
	if !identitiesOnly {
		for _, signer := range offered {
			add(signer)
		}
	}
	var keyError error
	for _, path := range paths {
		b, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			keyError = err
			continue
		}
		var public ssh.PublicKey
		if pub, err := os.ReadFile(path + ".pub"); err == nil {
			public, _, _, _, _ = ssh.ParseAuthorizedKey(pub)
		}
		if public == nil {
			public, _, _, _, _ = ssh.ParseAuthorizedKey(b)
		}
		matched := false
		if public != nil {
			for _, signer := range offered {
				if string(signer.PublicKey().Marshal()) == string(public.Marshal()) {
					add(signer)
					matched = true
				}
			}
		}
		if matched {
			continue
		}
		st, err := os.Stat(path)
		if err != nil {
			keyError = err
			continue
		}
		if st.Mode().Perm()&0077 != 0 {
			keyError = fmt.Errorf("SSH private key must not allow group or other access: %s", path)
			continue
		}
		signer, err := ssh.ParsePrivateKey(b)
		var encrypted *ssh.PassphraseMissingError
		if errors.As(err, &encrypted) {
			if public == nil {
				public = encrypted.PublicKey
			}
			if public != nil {
				add(&lockedSigner{public: public, key: b, path: path, credentials: credentials})
				continue
			}
			passphrase, e := credentials.Get("SSH key passphrase", path, "")
			if e != nil {
				keyError = e
				continue
			}
			signer, err = ssh.ParsePrivateKeyWithPassphrase(b, []byte(passphrase))
		}
		if err != nil {
			keyError = fmt.Errorf("read SSH identity %s: %w", path, err)
			continue
		}
		add(signer)
	}
	if len(signers) == 0 && keyError != nil {
		closeAgent()
		return nil, func() {}, keyError
	}
	return signers, closeAgent, nil
}

// An encrypted key with a readable public half needs no prompt until selected
// for signing. An already-authorized agent key can therefore complete the run.
type lockedSigner struct {
	mu          sync.Mutex
	public      ssh.PublicKey
	key         []byte
	path        string
	credentials *Credentials
	signer      ssh.Signer
}

func (s *lockedSigner) PublicKey() ssh.PublicKey { return s.public }
func (s *lockedSigner) Sign(random io.Reader, data []byte) (*ssh.Signature, error) {
	return s.SignWithAlgorithm(random, data, "")
}
func (s *lockedSigner) SignWithAlgorithm(random io.Reader, data []byte, algorithm string) (*ssh.Signature, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.signer == nil {
		passphrase, err := s.credentials.Get("SSH key passphrase", s.path, "")
		if err != nil {
			return nil, err
		}
		s.signer, err = ssh.ParsePrivateKeyWithPassphrase(s.key, []byte(passphrase))
		if err != nil {
			return nil, fmt.Errorf("unlock SSH key %s: %w", s.path, err)
		}
	}
	if algorithm != "" {
		signer, ok := s.signer.(ssh.AlgorithmSigner)
		if !ok {
			return nil, fmt.Errorf("SSH key cannot sign with %s", algorithm)
		}
		return signer.SignWithAlgorithm(random, data, algorithm)
	}
	return s.signer.Sign(random, data)
}

func EnsureKey(path string) (ssh.Signer, error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	st, err := os.Lstat(dir)
	if err != nil {
		return nil, err
	}
	if !st.IsDir() || st.Mode().Perm() != 0700 {
		return nil, fmt.Errorf("managed SSH key directory must be a directory with mode 0700: %s", dir)
	}
	f, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return nil, err
	}
	// Closing the descriptor releases the lock even if the explicit unlock fails.
	defer func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }()
	if b, err := os.ReadFile(path); err == nil {
		st, err := os.Lstat(path)
		if err != nil || !st.Mode().IsRegular() || st.Mode().Perm() != 0600 {
			return nil, fmt.Errorf("managed SSH key must be a regular file with mode 0600: %s", path)
		}
		return ssh.ParsePrivateKey(b)
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	block, err := ssh.MarshalPrivateKey(key, "MMX administrator access")
	if err != nil {
		return nil, err
	}
	if err := durable.Write(path, pem.EncodeToMemory(block), 0600); err != nil {
		return nil, err
	}
	signer, err := ssh.NewSignerFromKey(key)
	if err == nil {
		err = durable.Write(path+".pub", ssh.MarshalAuthorizedKey(signer.PublicKey()), 0600)
	}
	return signer, err
}
