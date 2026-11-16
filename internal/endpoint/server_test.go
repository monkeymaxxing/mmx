package endpoint

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func TestTimeoutAllowsFallbackButAuthorizationDoesNot(t *testing.T) {
	if !ReachabilityFailure(os.ErrDeadlineExceeded) || ReachabilityFailure(context.Canceled) || ReachabilityFailure(&HTTPError{Status: 403}) || ReachabilityFailure(&IdentityError{Reason: "wrong key"}) {
		t.Fatal("incorrect SSH fallback classification")
	}
}

func testSigner(t *testing.T) ssh.Signer {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return signer
}

// tlsServer prepares handler (s itself when nil) with the endpoint's TLS
// identity and per-connection sessions, as Serve does.
func tlsServer(t *testing.T, identity *Identity, s *Server, handler http.Handler) *httptest.Server {
	t.Helper()
	if handler == nil {
		handler = s
	}
	server := httptest.NewUnstartedServer(handler)
	server.TLS = &tls.Config{MinVersion: tls.VersionTLS13, GetCertificate: identity.GetCertificate}
	server.Config.ConnContext = s.ConnContext
	t.Cleanup(server.Close)
	return server
}
func serveTLS(t *testing.T, identity *Identity, s *Server, handler http.Handler) *httptest.Server {
	t.Helper()
	server := tlsServer(t, identity, s, handler)
	server.StartTLS()
	return server
}

// exchange sends one request directly on conn and returns its status.
func exchange(t *testing.T, conn net.Conn, in *bufio.Reader, method, path string, body []byte) int {
	t.Helper()
	r, err := http.NewRequest(method, "https://mmx"+path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Write(conn); err != nil {
		t.Fatal(err)
	}
	response, err := http.ReadResponse(in, r)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, response.Body)
	return response.StatusCode
}

func TestSessionProofBindsNodeUserAndConnection(t *testing.T) {
	identity, err := LoadIdentity(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	signer := testSigner(t)
	calls := 0
	s := &Server{Info: Info{NodeID: "node", TLSPin: identity.Pin}, Authorize: func(_ context.Context, user string, key ssh.PublicKey, remote, local string) error {
		calls++
		if user != "admin" {
			return errors.New("not an administrator")
		}
		return nil
	}}
	server := serveTLS(t, identity, s, nil)
	dial := func() (*tls.Conn, *bufio.Reader, []byte) {
		conn, err := tls.Dial("tcp", server.Listener.Addr().String(), ClientTLS(identity.Pin))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { conn.Close() })
		state := conn.ConnectionState()
		binding, err := state.ExportKeyingMaterial(sessionLabel, nil, 32)
		if err != nil {
			t.Fatal(err)
		}
		return conn, bufio.NewReader(conn), binding
	}
	proof := func(pin, signedUser, claimedUser string, binding []byte) []byte {
		signature, err := sign(signer, signedSession(pin, signedUser, binding))
		if err != nil {
			t.Fatal(err)
		}
		body, _ := json.Marshal(sessionProof{User: claimedUser, Key: signer.PublicKey().Marshal(), Signature: ssh.Marshal(signature)})
		return body
	}
	_, _, otherBinding := dial()
	for _, test := range []string{"node", "user", "connection", "none", "valid"} {
		t.Run(test, func(t *testing.T) {
			calls = 0
			conn, in, binding := dial()
			var body []byte
			switch test {
			case "node":
				body = proof("another-node", "admin", "admin", binding)
			case "user":
				body = proof(identity.Pin, "other", "admin", binding)
			case "connection":
				body = proof(identity.Pin, "admin", "admin", otherBinding)
			case "valid":
				body = proof(identity.Pin, "admin", "admin", binding)
			}
			if body != nil {
				want := 403
				if test == "valid" {
					want = 200
				}
				if code := exchange(t, conn, in, "POST", "/v1/session", body); code != want {
					t.Fatal("session", code)
				}
			}
			want := 403
			if test == "valid" {
				want = 200
			}
			for range 3 {
				if code := exchange(t, conn, in, "GET", "/v1/info", nil); code != want {
					t.Fatal("info", code)
				}
			}
			if (test == "valid") != (calls == 1) || calls > 1 {
				t.Fatal("authorization calls", calls)
			}
		})
	}
}

// Like an SSH login, a connection is authorized once. Removing a key refuses
// the next connection; requests already on an open connection continue.
func TestHTTPSAuthorizesEachConnectionOnceAndPinsServer(t *testing.T) {
	identity, err := LoadIdentity(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	first, second, stranger := testSigner(t), testSigner(t), testSigner(t)
	var mu sync.Mutex
	authorized := map[string]bool{string(first.PublicKey().Marshal()): true, string(second.PublicKey().Marshal()): true}
	calls, connections := 0, 0
	s := &Server{Info: Info{NodeID: "node", TLSPin: identity.Pin}, Authorize: func(_ context.Context, user string, key ssh.PublicKey, remote, local string) error {
		mu.Lock()
		defer mu.Unlock()
		calls++
		if user != "admin" || !authorized[string(key.Marshal())] {
			return errors.New("key removed")
		}
		return nil
	}}
	server := tlsServer(t, identity, s, nil)
	server.Config.ConnState = func(_ net.Conn, state http.ConnState) {
		if state == http.StateNew {
			mu.Lock()
			connections++
			mu.Unlock()
		}
	}
	server.StartTLS()
	counts := func() (int, int) {
		mu.Lock()
		defer mu.Unlock()
		return calls, connections
	}
	c := NewClient(server.URL, "admin", identity.Pin, []ssh.Signer{first})
	defer c.Close()
	var got Info
	for range 3 {
		if err := c.Call(context.Background(), "GET", "/v1/info", nil, &got); err != nil || got.NodeID != "node" {
			t.Fatal(got, err)
		}
	}
	if calls, connections := counts(); calls != 1 || connections != 1 {
		t.Fatalf("expected one authorized connection, got %d authorizations on %d connections", calls, connections)
	}
	mu.Lock()
	delete(authorized, string(first.PublicKey().Marshal()))
	mu.Unlock()
	if err := c.Call(context.Background(), "GET", "/v1/info", nil, &got); err != nil {
		t.Fatal("open connection lost its authority", err)
	}
	c.Close()
	err = c.Call(context.Background(), "GET", "/v1/info", nil, &got)
	var denied *HTTPError
	if !errors.As(err, &denied) || denied.Status != 403 || ReachabilityFailure(err) {
		t.Fatal("removed key or fallback policy", err)
	}
	// The key that worked is tried first on later connections.
	other := NewClient(server.URL, "admin", identity.Pin, []ssh.Signer{stranger, second})
	defer other.Close()
	if err := other.Call(context.Background(), "GET", "/v1/info", nil, &got); err != nil {
		t.Fatal("other administrator stopped working", err)
	}
	other.Close()
	before, _ := counts()
	if err := other.Call(context.Background(), "GET", "/v1/info", nil, &got); err != nil {
		t.Fatal(err)
	}
	if after, _ := counts(); after != before+1 {
		t.Fatal("working key was not tried first", after-before)
	}
	wrong := NewClient(server.URL, "admin", strings.Repeat("0", 64), []ssh.Signer{second})
	defer wrong.Close()
	err = wrong.Call(context.Background(), "GET", "/v1/info", nil, &got)
	var identityError *IdentityError
	if !errors.As(err, &identityError) || ReachabilityFailure(err) {
		t.Fatal("wrong server identity permitted fallback", err)
	}
	server.Close()
	if err := other.Call(context.Background(), "GET", "/v1/info", nil, &got); !ReachabilityFailure(err) {
		t.Fatal("stopped endpoint should permit SSH fallback", err)
	}
}

// Only a TLS connection can authenticate, and nothing else is served first.
func TestUnauthenticatedConnectionsReachNoTaskBlock(t *testing.T) {
	s := &Server{Info: Info{TLSPin: "pin"}, Authorize: func(context.Context, string, ssh.PublicKey, string, string) error { return nil }}
	for _, path := range []string{"/v1/info", "/v1/submit", "/v1/progress", "/v1/session"} {
		method := "POST"
		if path == "/v1/info" {
			method = "GET"
		}
		for _, ctx := range []context.Context{context.Background(), s.ConnContext(context.Background(), nil)} {
			r := httptest.NewRequestWithContext(ctx, method, path, strings.NewReader("{}"))
			w := httptest.NewRecorder()
			s.ServeHTTP(w, r)
			if w.Code != 403 {
				t.Fatal(path, w.Code, w.Body.String())
			}
		}
	}
}

func TestCertificateRenewalPreservesPinnedKey(t *testing.T) {
	identity, err := LoadIdentity(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	before := append([]byte{}, identity.certificate.Certificate[0]...)
	identity.certificate.Leaf.NotAfter = time.Now().Add(time.Hour)
	certificate, err := identity.GetCertificate(nil)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(before, certificate.Certificate[0]) {
		t.Fatal("certificate did not renew")
	}
	if err := ClientTLS(identity.Pin).VerifyConnection(tls.ConnectionState{PeerCertificates: []*x509.Certificate{certificate.Leaf}}); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadIdentity(identity.root)
	if err != nil || loaded.Pin != identity.Pin {
		t.Fatal("identity changed on reopen", err)
	}
}

// The HTTPS API tells systemd it listens through NOTIFY_SOCKET, so its
// Type=notify unit counts as started only then; outside systemd it says nothing.
func TestNotifyReadyTellsSystemd(t *testing.T) {
	t.Setenv("NOTIFY_SOCKET", "")
	if err := notifyReady(); err != nil {
		t.Fatal(err)
	}
	path := t.TempDir() + "/notify"
	socket, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: path, Net: "unixgram"})
	if err != nil {
		t.Fatal(err)
	}
	defer socket.Close()
	t.Setenv("NOTIFY_SOCKET", path)
	if err := notifyReady(); err != nil {
		t.Fatal(err)
	}
	message := make([]byte, 64)
	n, err := socket.Read(message)
	if err != nil || string(message[:n]) != "READY=1" {
		t.Fatal(err, string(message[:n]))
	}
}

// The node HTTPS API process serves only HTTP/1.1: the controller process
// never speaks HTTP/2, and HTTP/2 framing would run before MMX authentication.
func TestHTTPSAPIServerNegotiatesOnlyHTTP1(t *testing.T) {
	identity, err := LoadIdentity(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	server := httpsAPIServer(&Server{Info: Info{TLSPin: identity.Pin}}, identity)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = server.ServeTLS(listener, "", "") }()
	t.Cleanup(func() { server.Close() })
	config := ClientTLS(identity.Pin)
	config.NextProtos = []string{"h2", "http/1.1"}
	conn, err := tls.Dial("tcp", listener.Addr().String(), config)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if negotiated := conn.ConnectionState().NegotiatedProtocol; negotiated != "http/1.1" {
		t.Fatalf("node HTTPS API negotiated %q for a client that offered h2 and http/1.1; want http/1.1", negotiated)
	}
	if code := exchange(t, conn, bufio.NewReader(conn), "GET", "/v1/info", nil); code != 403 {
		t.Fatal("unauthenticated HTTP/1.1 request on the negotiated connection", code)
	}
}
