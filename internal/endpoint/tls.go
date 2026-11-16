package endpoint

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"sync"
	"time"

	"mmx/internal/durable"
)

type Identity struct {
	mu          sync.Mutex
	root        string
	key         ed25519.PrivateKey
	certificate *tls.Certificate
	Pin         string
}

func publicPin(key any) (string, error) {
	der, err := x509.MarshalPKIXPublicKey(key)
	if err != nil {
		return "", err
	}
	hash := sha256.Sum256(der)
	return hex.EncodeToString(hash[:]), nil
}

func LoadIdentity(root string) (*Identity, error) {
	path := filepath.Join(root, "tls.key")
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		_, key, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return nil, err
		}
		der, err := x509.MarshalPKCS8PrivateKey(key)
		if err != nil {
			return nil, err
		}
		data = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})
		if err := durable.Write(path, data, 0600); err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}
	st, err := os.Lstat(path)
	if err != nil || !st.Mode().IsRegular() || st.Mode().Perm() != 0600 {
		return nil, fmt.Errorf("TLS private key must be a regular file with mode 0600")
	}
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("invalid TLS private key")
	}
	raw, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	key, ok := raw.(ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("unexpected TLS key type")
	}
	pin, err := publicPin(key.Public())
	if err != nil {
		return nil, err
	}
	i := &Identity{root: root, key: key, Pin: pin}
	if cert, err := os.ReadFile(filepath.Join(root, "tls.crt")); err == nil {
		if pair, err := tls.X509KeyPair(cert, data); err == nil {
			i.certificate = &pair
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	_, err = i.GetCertificate(nil)
	return i, err
}

func (i *Identity) GetCertificate(_ *tls.ClientHelloInfo) (*tls.Certificate, error) {
	i.mu.Lock()
	defer i.mu.Unlock()
	now := time.Now()
	if i.certificate != nil && i.certificate.Leaf != nil && now.After(i.certificate.Leaf.NotBefore) && i.certificate.Leaf.NotAfter.After(now.Add(24*time.Hour)) {
		return i.certificate, nil
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, err
	}
	template := &x509.Certificate{SerialNumber: serial, Subject: pkix.Name{CommonName: "MMX node"}, DNSNames: []string{"mmx"}, NotBefore: now.Add(-5 * time.Minute), NotAfter: now.Add(90 * 24 * time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, i.key.Public(), i.key)
	if err != nil {
		return nil, err
	}
	if err := durable.Write(filepath.Join(i.root, "tls.crt"), pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0600); err != nil {
		return nil, err
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	i.certificate = &tls.Certificate{Certificate: [][]byte{der}, PrivateKey: i.key, Leaf: leaf}
	return i.certificate, nil
}

type IdentityError struct{ Reason string }

func (e *IdentityError) Error() string { return "HTTPS node identity: " + e.Reason }

func ClientTLS(pin string) *tls.Config {
	return &tls.Config{MinVersion: tls.VersionTLS13, ServerName: "mmx",
		// Trust is the public key obtained through verified SSH, not a public CA
		// or a hostname. VerifyConnection checks that identity on every handshake.
		InsecureSkipVerify: true,
		VerifyConnection: func(state tls.ConnectionState) error {
			if len(state.PeerCertificates) != 1 {
				return &IdentityError{"expected one pinned node certificate"}
			}
			cert := state.PeerCertificates[0]
			got, err := publicPin(cert.PublicKey)
			if err != nil || got != pin {
				return &IdentityError{"server key differs from the key verified through SSH"}
			}
			if err := cert.CheckSignature(cert.SignatureAlgorithm, cert.RawTBSCertificate, cert.Signature); err != nil {
				return &IdentityError{"invalid node certificate signature"}
			}
			if now := time.Now(); now.Before(cert.NotBefore) || now.After(cert.NotAfter) {
				return &IdentityError{"node certificate is outside its validity period"}
			}
			return nil
		},
	}
}
