package endpoint

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	"mmx/internal/facts"
	"mmx/internal/modules"
	"mmx/internal/node"
	"mmx/internal/protocol"
	"mmx/internal/release"

	"golang.org/x/crypto/ssh"
)

type Info struct {
	NodeID       string `json:"node_id"`
	Architecture string `json:"architecture"`
	RunnerHash   string `json:"runner_hash"`
	TLSPin       string `json:"tls_pin"`
	// BootID is the kernel's ID of the node's current boot. With NodeID it
	// tells one running machine from a clone that shares its machine ID.
	BootID string `json:"boot_id,omitempty"`
	// RunnerVersion is the MMX release version of this node runner
	// executable, which the controller process compares with its own.
	RunnerVersion string `json:"runner_version,omitempty"`
}

// sessionProof authenticates one TLS connection: an administrator's SSH
// signature over the node's pinned key, the account, and the connection's
// exported keying material, which no other connection shares.
type sessionProof struct {
	User           string
	Key, Signature []byte
}

const sessionLabel = "EXPORTER-MMX-session"

func signedSession(pin, user string, binding []byte) []byte {
	b, _ := json.Marshal([]string{"mmx-session-v1", pin, user, hex.EncodeToString(binding)})
	return b
}

type Server struct {
	Node      node.Server
	Info      Info
	Authorize func(context.Context, string, ssh.PublicKey, string, string) error
}

// session is one TLS connection's authentication. As with an SSH login, the
// connection is authorized once, when it authenticates, and its requests run
// with that authority until it closes. Every new connection is checked again
// against the node's current keys, sudo, and SSH policy.
type session struct {
	mu         sync.Mutex
	authorized bool
}
type sessionKey struct{}

func (c *session) active() bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.authorized
}
func (c *session) set(authorized bool) {
	c.mu.Lock()
	c.authorized = authorized
	c.mu.Unlock()
}

// ConnContext gives each connection its own session; http.Server calls it.
func (s *Server) ConnContext(ctx context.Context, _ net.Conn) context.Context {
	return context.WithValue(ctx, sessionKey{}, &session{})
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.URL.RawQuery != "" {
		http.Error(w, "query parameters are not supported", 400)
		return
	}
	current, _ := r.Context().Value(sessionKey{}).(*session)
	if r.Method == "POST" && r.URL.Path == "/v1/session" {
		s.authenticate(w, r, current)
		return
	}
	if !current.active() {
		http.Error(w, "MMX authentication required", http.StatusForbidden)
		return
	}
	var result any
	var err error
	switch {
	case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/v1/diffs/"):
		s.downloadDiff(w, r)
		return
	case r.Method == "PUT" && strings.HasPrefix(r.URL.Path, "/v1/assets/"):
		err = s.upload(w, r)
		result = map[string]string{"status": "uploaded"}
	case r.Method == "GET" && r.URL.Path == "/v1/info":
		result = s.Info
	case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/v1/status/"):
		result, err = s.Node.Status(strings.TrimPrefix(r.URL.Path, "/v1/status/"))
	case r.Method == "POST" && strings.HasPrefix(r.URL.Path, "/v1/stop/"):
		result, err = s.Node.Stop(strings.TrimPrefix(r.URL.Path, "/v1/stop/"))
	case r.Method == "POST" && strings.HasPrefix(r.URL.Path, "/v1/"):
		remote, _, _ := net.SplitHostPort(r.RemoteAddr)
		connection := modules.ControllerConnection{Kind: protocol.ConnectionHTTPS, SourceAddress: remote}
		if local, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr); ok {
			connection.NodeLocalAddress, _, _ = net.SplitHostPort(local.String())
		}
		result, err = s.Node.Answer(r.Context(), strings.TrimPrefix(r.URL.Path, "/v1/"), r.Body, connection)
	default:
		err = node.ErrUnknownRequest
	}
	switch {
	case errors.Is(err, node.ErrUnknownRequest):
		http.Error(w, "unknown node HTTPS API endpoint", 404)
		return
	case errors.Is(err, protocol.ErrRequestLimit):
		http.Error(w, err.Error(), http.StatusRequestEntityTooLarge) //nolint:nilaway // errors.Is matched a non-nil target, so err is not nil.
		return
	case err != nil:
		http.Error(w, err.Error(), http.StatusConflict)
		return
	}
	if response, ok := result.(protocol.Response); ok {
		result = response.Transport()
	}
	_ = json.NewEncoder(w).Encode(result)
}

// refusedBeforeKeyProofAnswer is the node HTTPS API's whole answer to a
// client whose key the account does not list, whatever the reason.
const refusedBeforeKeyProofAnswer = "key is not authorized for unrestricted MMX access as this account; check current SSH keys or use connection: ssh_only for other SSH authorization mechanisms; journalctl -u " + HTTPSAPIUnit + " on the node gives the reason"

// refusalJournal receives the reason for each refused authentication. The
// node HTTPS API process's standard error goes to the systemd journal.
var refusalJournal io.Writer = os.Stderr

// authorizations holds one entry for each authentication being checked. Each
// check starts sshd -C, and for a proved key getent and sudo, as root; ten
// at once, the start of sshd's own MaxStartups, bounds the processes a flood
// of tries can start. A try waits its turn within the check's time limit.
var authorizations = make(chan struct{}, 10)

// authenticate verifies a signature over this connection's keying material,
// then authorizes the key against the node's current SSH and sudo settings.
func (s *Server) authenticate(w http.ResponseWriter, r *http.Request, current *session) {
	if current == nil || r.TLS == nil {
		http.Error(w, "MMX authentication requires a TLS connection", http.StatusForbidden)
		return
	}
	current.set(false)
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 32*1024))
	var p sessionProof
	if err != nil || protocol.Decode(body, &p) != nil {
		http.Error(w, "invalid authentication proof", 400)
		return
	}
	binding, err := r.TLS.ExportKeyingMaterial(sessionLabel, nil, 32)
	if err != nil {
		http.Error(w, "connection keying material unavailable", http.StatusForbidden)
		return
	}
	key, err := ssh.ParsePublicKey(p.Key)
	var signature ssh.Signature
	//lint:ignore SA1019 The DSA constant is named only to reject that algorithm.
	if err != nil || ssh.Unmarshal(p.Signature, &signature) != nil || signature.Format == ssh.KeyAlgoRSA || signature.Format == ssh.InsecureKeyAlgoDSA || key.Verify(signedSession(s.Info.TLSPin, p.User, binding), &signature) != nil {
		http.Error(w, "invalid authentication signature", http.StatusForbidden)
		return
	}
	remote, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		http.Error(w, "invalid controller connection address", 400)
		return
	}
	local := "127.0.0.1"
	if address, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr); ok {
		local, _, _ = net.SplitHostPort(address.String())
	}
	if s.Authorize == nil {
		http.Error(w, "node HTTPS API authorization is unavailable", http.StatusForbidden)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	select {
	case authorizations <- struct{}{}:
		defer func() { <-authorizations }()
	case <-ctx.Done():
		http.Error(w, "the node HTTPS API is busy checking other connections; try again", http.StatusServiceUnavailable)
		return
	}
	if err := s.Authorize(ctx, p.User, key, remote, local); err != nil {
		fmt.Fprintf(refusalJournal, "MMX authentication refused for account %q from %s: %v\n", p.User, remote, err)
		var before *refusedBeforeKeyProof
		if errors.As(err, &before) {
			http.Error(w, refusedBeforeKeyProofAnswer, http.StatusForbidden)
			return
		}
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	current.set(true)
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "authenticated"})
}

func NodeInfo(binary string, identity *Identity) (Info, error) {
	node, err := facts.MachineIDHash()
	if err != nil {
		return Info{}, err
	}
	b, err := os.ReadFile(binary)
	if err != nil {
		return Info{}, err
	}
	boot, err := facts.BootID()
	if err != nil {
		return Info{}, err
	}
	hash := sha256.Sum256(b)
	return Info{NodeID: node, Architecture: runtime.GOARCH, RunnerHash: hex.EncodeToString(hash[:]), TLSPin: identity.Pin, BootID: boot, RunnerVersion: release.Version}, nil
}

// httpsAPIServer configures the node HTTPS API process's server on port 7443.
func httpsAPIServer(handler *Server, identity *Identity) *http.Server {
	return &http.Server{Addr: ":7443", Handler: handler, ConnContext: handler.ConnContext, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 32 * 1024,
		TLSConfig: &tls.Config{MinVersion: tls.VersionTLS13, GetCertificate: identity.GetCertificate},
		// The controller process speaks only HTTP/1.1. HTTP/2 would only add
		// attack surface before MMX authentication.
		TLSNextProto: map[string]func(*http.Server, *tls.Conn, http.Handler){}}
}

func Serve(node node.Server, sshPort int) error {
	if err := node.Initialize(); err != nil {
		return err
	}
	identity, err := LoadIdentity(node.Root)
	if err != nil {
		return err
	}
	info, err := NodeInfo(node.Binary, identity)
	if err != nil {
		return err
	}
	handler := &Server{Node: node, Info: info, Authorize: (Authorizer{SSHPort: sshPort}).Authorize}
	server := httpsAPIServer(handler, identity)
	listener, err := net.Listen("tcp", server.Addr)
	if err != nil {
		return err
	}
	if err := notifyReady(); err != nil {
		listener.Close()
		return err
	}
	return server.ServeTLS(listener, "", "")
}

// notifyReady tells systemd that the HTTPS API listens. Its unit is
// Type=notify, so systemd counts it as started, and systemctl start or restart
// returns, only then: a controller that connects right after setup or after a
// boot finds the port open. Outside systemd NOTIFY_SOCKET is unset.
func notifyReady() error {
	socket := os.Getenv("NOTIFY_SOCKET")
	if socket == "" {
		return nil
	}
	conn, err := net.DialUnix("unixgram", nil, &net.UnixAddr{Name: socket, Net: "unixgram"})
	if err != nil {
		return fmt.Errorf("notify systemd that the node HTTPS API process is ready: %w", err)
	}
	defer conn.Close()
	_, err = conn.Write([]byte("READY=1"))
	return err
}
