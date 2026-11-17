package controller

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"mmx/internal/access"
	"mmx/internal/assets"
	"mmx/internal/config"
	"mmx/internal/durable"
	"mmx/internal/endpoint"
	"mmx/internal/facts"
	"mmx/internal/protocol"
	"mmx/internal/release"

	"golang.org/x/crypto/ssh"
)

type Connection struct {
	NodeID, Hash, Transport, Architecture string
	// BootID is the kernel's ID of the node's current boot, as the node
	// reported it when this connection was made.
	BootID string
	// FallbackReason says why a node that may use HTTPS is reached over SSH.
	FallbackReason string
	// httpsURL is the address of the node's HTTPS service, and lostHTTPS the
	// failure that moved this connection to SSH, for FallbackReason.
	httpsURL  string
	lostHTTPS error
	direct    *endpoint.Client
	native    *sshSession
	fallback  func(context.Context) (*sshSession, error)
	// Confirmation owns separate request clients. Only its SSH attempt goroutine
	// uses this preparation session; ordinary request fallback never changes it.
	confirmationSSH        *sshSession
	prepareConfirmationSSH func(context.Context) (*sshSession, error)
	newConfirmationHTTPS   func() *endpoint.Client
}

// ConnectionUsed is the node job result's record of how the node was reached:
// https; ssh for a node whose inventory says ssh_only, or for a dryrun on a
// node where MMX's HTTPS service is not set up; or ssh_fallback.
func (c *Connection) ConnectionUsed() string {
	if c.Transport == "ssh" && c.FallbackReason != "" {
		return "ssh_fallback"
	}
	return c.Transport
}

// httpsCheck prints two lines about the node's side of HTTPS. The first is
// systemd's state of the node HTTPS API process, "active" when it runs, or
// "not-found" when the unit does not exist on the node (systemctl is-active
// prints "inactive" for a missing unit, so the load state is asked first). The
// second is "firewalld_without_rule" when firewalld is running and its default
// zone has neither a port rule nor a rich rule that names port 7443,
// "ufw_without_rule" when ufw is active and none of its rules names the port,
// and "-" otherwise. A rule that names the port can still be limited to an
// interface or a source, so the check claims no more than that.
const httpsCheck = `if [ "$(systemctl show --property=LoadState --value ` + endpoint.HTTPSAPIUnit + ` 2>/dev/null)" = not-found ]; then
 echo not-found
else
 systemctl is-active ` + endpoint.HTTPSAPIUnit + ` || true
fi
if command -v firewall-cmd >/dev/null 2>&1 && firewall-cmd --state >/dev/null 2>&1; then
 zone=$(firewall-cmd --get-default-zone)
 if ! firewall-cmd --zone="$zone" --query-port=7443/tcp >/dev/null 2>&1 && ! firewall-cmd --zone="$zone" --list-rich-rules | grep -Eq '(^|[^0-9])7443([^0-9]|$)'; then
  echo firewalld_without_rule
 else
  echo -
 fi
elif command -v ufw >/dev/null 2>&1 && ufw status | grep -q '^Status: active' && ! ufw status | grep -Eq '(^|[^0-9])7443([^0-9]|$)'; then
 echo ufw_without_rule
else
 echo -
fi
`

// nodeHTTPSState is what the node, reached over SSH, says about its side of
// HTTPS after HTTPS failed.
type nodeHTTPSState struct {
	// apiState is systemd's state of the node HTTPS API process.
	apiState string
	// firewallWithoutRule names the active firewall, "ufw" or "firewalld", when
	// none of its rules names port 7443; it is empty otherwise.
	firewallWithoutRule string
	// checkErr says why the node could not answer; startErr why starting the
	// node HTTPS API process failed, when MMX tried.
	checkErr, startErr error
}

func (s *sshSession) checkHTTPS(ctx context.Context) nodeHTTPSState {
	out, err := s.privileged(ctx, httpsCheck, nil)
	if err != nil {
		return nodeHTTPSState{checkErr: err}
	}
	apiState, firewall, _ := strings.Cut(strings.TrimSpace(string(out)), "\n")
	firewalls := map[string]string{"ufw_without_rule": "ufw", "firewalld_without_rule": "firewalld", "-": ""}
	name, known := firewalls[firewall]
	if apiState == "" || !known {
		return nodeHTTPSState{checkErr: fmt.Errorf("unexpected answer to the node HTTPS service and firewall check over SSH: %q", out)}
	}
	return nodeHTTPSState{apiState: apiState, firewallWithoutRule: name}
}

// httpsFailure says what MMX saw when a request to the node's HTTPS service at
// endpointURL failed with err, a reachability failure, in plain words where
// Go's error has a common form.
func httpsFailure(endpointURL string, err error) string {
	var request *url.Error
	if errors.As(err, &request) {
		err = request.Err
	}
	var network net.Error
	var dial *net.OpError
	switch {
	case errors.As(err, &dial) && dial.Op == "dial":
		why := dial.Err.Error()
		var system *os.SyscallError
		switch {
		case dial.Timeout():
			why = "connection timed out"
		case errors.As(dial.Err, &system):
			why = system.Err.Error()
		}
		return "MMX could not connect to " + endpointURL + " (" + why + ")"
	case errors.As(err, &network) && network.Timeout():
		return "MMX got no answer in time from " + endpointURL
	case errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF):
		return "The connection to " + endpointURL + " closed before an answer came"
	}
	return "A request to " + endpointURL + " failed (" + err.Error() + ")"
}

// httpsService names the node HTTPS API process for a reader who has not met
// it, in every message that mentions it.
const httpsService = "mmx-runner, MMX's HTTPS service on this node,"

// fallbackReason words the notice for a node that MMX reaches over SSH after
// HTTPS to endpointURL failed with err. It gives what MMX saw and what the node
// reports about itself, and names no cause that MMX did not check: the advice
// lists the likely ones without choosing. MMX never opens the port.
func (state nodeHTTPSState) fallbackReason(endpointURL string, err error) string {
	saw := httpsFailure(endpointURL, err)
	host, port := endpointURL, "443"
	if u, parseErr := url.Parse(endpointURL); parseErr == nil {
		host = u.Hostname()
		if u.Port() != "" {
			port = u.Port()
		}
	}
	choices := "To use HTTPS, check that the controller machine can reach " + host + " on TCP port " + port + ": the network between them may be down, or a firewall may block the port. To make SSH the stated choice instead, set connection: ssh_only on the node in the inventory, or pass --connection ssh_only when this controller machine cannot reach the node's HTTPS address."
	switch {
	case state.checkErr != nil:
		return saw + ", and MMX could not check this node over SSH: " + state.checkErr.Error() + ". MMX is using SSH for this node. " + choices
	case state.apiState != "active":
		stopped := saw + ". " + httpsService + " is not running: its systemd unit " + endpoint.HTTPSAPIUnit + " is " + state.apiState
		if state.startErr != nil {
			stopped += ", and starting it failed: " + state.startErr.Error()
		}
		return stopped + ". MMX is using SSH for this node; journalctl -u " + endpoint.HTTPSAPIUnit + " on this node says why mmx-runner does not run."
	case state.firewallWithoutRule == "firewalld":
		return saw + ". " + httpsService + " is running, and firewalld's default zone on this node has no rule for port 7443. MMX is using SSH for this node. " + choices
	case state.firewallWithoutRule != "":
		return saw + ". " + httpsService + " is running, and " + state.firewallWithoutRule + " on this node is active with no rule that names port 7443. MMX is using SSH for this node. " + choices
	default:
		return saw + ". " + httpsService + " is running. MMX is using SSH for this node. " + choices
	}
}

func (c *Connection) Close() {
	if c.direct != nil {
		c.direct.Close()
	}
	if c.native != nil {
		c.native.Close()
	}
	if c.confirmationSSH != nil && c.confirmationSSH != c.native {
		c.confirmationSSH.Close()
	}
}

// bootedMachine is one machine in one boot: the SHA-256 of its systemd machine
// ID and the kernel's boot ID. Virtual machines cloned from one image can share
// a machine ID, but each boots its own kernel with its own boot ID.
type bootedMachine struct{ NodeID, BootID string }

// machine is the node a recorded job ran on; the zero value accepts any node.
type machine struct{ NodeID, Architecture string }

func (m machine) check(node, arch string) error {
	if m.NodeID != "" && node != m.NodeID {
		return &replacedError{"machine identity"}
	}
	if m.Architecture != "" && arch != m.Architecture {
		return &replacedError{"architecture"}
	}
	return nil
}

// replacedError means the address now reaches another machine than the one a
// recorded job ran on. Its saved progress describes that machine, so retry and
// status refuse before installing anything; a new run sets the new one up.
type replacedError struct{ what string }

func (e *replacedError) Error() string {
	return "node was replaced since this run (" + e.what + " changed); its recorded work belongs to the previous machine; use mmx run"
}

type artifact struct {
	once   sync.Once
	path   string
	binary []byte
	hash   string
	err    error
	packed sync.Once
	gz     []byte
	// versionMatched is set once a node running these bytes reported this mmx
	// program's release version; the same bytes report the same version.
	versionMatched atomic.Bool
}

// checkVersion refuses a node runner executable of another MMX release than
// this mmx program: the node would run modules without this release's fixes.
// Development builds all report release.DefaultVersion and match one another.
// recorded says that a recorded node job resumes with this executable.
func (a *artifact) checkVersion(reported string, recorded bool) error {
	if reported == release.Version {
		a.versionMatched.Store(true) //nolint:nilaway // Each caller first loads the runner: the HTTPS check in connect runs only for a saved node identity, whose runner loadArtifact loaded.
		return nil
	}
	found := "reports MMX version " + reported
	if reported == "" {
		found = "reports no MMX version"
	}
	if recorded {
		return fmt.Errorf("this playbook run's recorded node job uses the node runner executable %s, which %s, but this mmx program is version %s. "+
			"A recorded node job keeps the node runner executable it started with. Resume the run with mmx of that release, or start a new run with mmx run", a.path, found, release.Version)
	}
	return fmt.Errorf("the node runner executable %s %s, but this mmx program is version %s. "+
		"Install mmx together with the mmx-runner files of the same MMX release; a release archive holds both", a.path, found, release.Version)
}

// gzipped is the runner as it travels over SSH, compressed once per process: a
// stripped Go binary still shrinks to about 40%. The node checks the hash of
// the decompressed bytes.
func (a *artifact) gzipped() []byte {
	a.packed.Do(func() {
		var b bytes.Buffer
		w := gzip.NewWriter(&b)
		_, _ = w.Write(a.binary)
		_ = w.Close()
		a.gz = b.Bytes()
	})
	return a.gz
}

// artifacts holds each runner file read by this process. A command uses one
// runner build throughout, and every node connection shares its bytes and hash
// instead of reading and hashing the file again.
var artifacts sync.Map

// runnerArtifact reads and hashes a runner file once per process and retains a
// copy under its hash for recorded retry.
func (c Controller) runnerArtifact(path string) (*artifact, error) {
	value, _ := artifacts.LoadOrStore(c.State+"\x00"+path, &artifact{})
	a := value.(*artifact)
	a.once.Do(func() {
		binary, err := os.ReadFile(path)
		if err != nil {
			a.err = fmt.Errorf("required node runner executable could not be read: %w", err)
			return
		}
		sum := sha256.Sum256(binary)
		a.path, a.binary, a.hash = path, binary, hex.EncodeToString(sum[:])
		if a.err = os.MkdirAll(filepath.Join(c.State, "runners"), 0700); a.err == nil {
			a.err = writeRetained(filepath.Join(c.State, "runners", a.hash), binary)
		}
	})
	return a, a.err
}

// keyring is one set of administrator keys loaded by this process.
type keyring struct {
	mu      sync.Mutex
	signers []ssh.Signer
}

// keyrings lets every node connection of a command share one key load: one
// agent listing, one parse of each key file, and one passphrase prompt for an
// encrypted key. Asking a forwarded agent once per node cost about 0.6 s per
// node at 100 nodes at once. A node's managed key is not shared; nodeSigners
// reads it for each node.
var keyrings sync.Map

// loadSigners returns the keys for paths, loading them at most once. Only a
// load that found a key is kept. The agent connection of a kept load stays
// open for the process.
func loadSigners(paths []string, socket string, only bool, credentials *access.Credentials) ([]ssh.Signer, error) {
	value, _ := keyrings.LoadOrStore(fmt.Sprintf("%q %q %v %p", paths, socket, only, credentials), &keyring{})
	k := value.(*keyring)
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.signers == nil {
		signers, closeAgent, err := access.Signers(paths, socket, only, credentials)
		if err != nil || len(signers) == 0 {
			closeAgent()
			return signers, err
		}
		k.signers = signers
	}
	return k.signers, nil
}

// nodeSigners returns a node's administrator keys: the keys every node shares,
// loaded once per process, and without identity_file the node's own managed
// key, read on each call because setup may create it during this process.
func nodeSigners(h config.Node, settings sshSettings, managed string, credentials *access.Credentials) ([]ssh.Signer, error) {
	shared, err := loadSigners(settings.Identities, settings.Agent, settings.IdentitiesOnly, credentials)
	if h.IdentityFile != "" {
		return shared, err
	}
	own, _, ownErr := access.Signers([]string{managed}, "", true, credentials)
	signers := append([]ssh.Signer(nil), shared...)
	for _, signer := range own {
		if !slices.ContainsFunc(signers, func(s ssh.Signer) bool { return bytes.Equal(s.PublicKey().Marshal(), signer.PublicKey().Marshal()) }) {
			signers = append(signers, signer)
		}
	}
	if len(signers) > 0 {
		return signers, nil
	}
	return nil, errors.Join(err, ownErr)
}

// probeInventorySSH opens exactly one fresh, read-only SSH connection and reads
// the node's machine ID and boot ID. A saved HTTPS identity is also the machine
// identity for SSH; no service setup is needed. Every inventory name is probed,
// also one saved with the controller machine's machine ID: a clone of the
// controller machine's image shares the machine ID, and only the boot ID that
// the probe reads tells the clone from the controller machine.
func (c Controller) probeInventorySSH(ctx context.Context, run *Run, name string) (bootedMachine, error) {
	h := run.Inventory.Nodes[name]
	settings, err := resolveSSH(ctx, h)
	if err != nil {
		return bootedMachine{}, err
	}
	destination := nodeDestination(settings)
	var known endpoint.Info
	raw, err := os.ReadFile(filepath.Join(c.State, "nodes", destination+".json"))
	if err == nil {
		if err := protocol.Decode(raw, &known); err != nil {
			return bootedMachine{}, fmt.Errorf("Invalid saved node identity for %s: %w.", name, err)
		}
	} else if !os.IsNotExist(err) {
		return bootedMachine{}, err
	}
	if known.NodeID != "" && !runnerHash.MatchString(known.NodeID) {
		return bootedMachine{}, fmt.Errorf("Invalid saved node identity for %s.", name)
	}
	if run.nodeIdentities != nil {
		if identity, ok := run.nodeIdentities.Load(name); ok {
			known.NodeID = identity.(string)
		}
	}
	keyPath, err := access.KeyPath(destination)
	if err != nil {
		return bootedMachine{}, err
	}
	credentials := c.Credentials
	if credentials == nil {
		credentials = &access.Credentials{}
	}
	c.adoptManagedKeyAfterPortChange(ctx, run, h, settings, keyPath, credentials)
	session, err := newSSHSession(h, credentials, run.Project.Secrets, keyPath, settings.Identities, settings.User+"@"+net.JoinHostPort(settings.Hostname, settings.Port))
	if err != nil {
		return bootedMachine{}, err
	}
	defer session.Close()
	session.allowAskpass = true
	nodeID, bootID, err := session.probeMachine(ctx, name)
	if err != nil {
		return bootedMachine{}, err
	}
	if err := (machine{NodeID: known.NodeID}).check(nodeID, ""); err != nil {
		return bootedMachine{}, err
	}
	return bootedMachine{nodeID, bootID}, nil
}

// connect reaches the inventory node name over HTTPS with its saved identity,
// or sets MMX up through verified SSH. The saved identity only records what
// verified SSH reported, so a mismatch sends the node back to SSH: OpenSSH's
// host-key check decides whether a replaced server is trusted, then its
// identity is saved again. A dryrun never sets up MMX's HTTPS service on a node
// that does not have its systemd unit: it uses verified SSH there, as
// connection: ssh_only does, and the first run sets the service up.
// recorded, when set, is the machine the node must be.
func (c Controller) connect(ctx context.Context, run *Run, name string, expected string, recorded machine) (*Connection, error) {
	h := run.Inventory.Nodes[name]
	settings, err := resolveSSH(ctx, h)
	if err != nil {
		return nil, err
	}
	destination := nodeDestination(settings)
	keyPath, err := access.KeyPath(destination)
	if err != nil {
		return nil, err
	}
	pinPath := filepath.Join(c.State, "nodes", destination+".json")
	credentials := c.Credentials
	if credentials == nil {
		credentials = &access.Credentials{}
	}
	c.adoptManagedKeyAfterPortChange(ctx, run, h, settings, keyPath, credentials)
	notify := func(message string) {
		if credentials.Notify != nil {
			credentials.Notify(h.Address + ": " + message)
		}
	}
	conn := &Connection{}
	complete := false
	defer func() {
		if !complete {
			conn.Close()
			return
		}
		// Ordinary SSH already needs this session. HTTPS setup closes its session
		// below; only a pending confirmation can open a new preparation session.
		ordinarySSH := conn.native
		want, hash := machine{conn.NodeID, conn.Architecture}, conn.Hash
		conn.prepareConfirmationSSH = func(ctx context.Context) (*sshSession, error) {
			if conn.confirmationSSH != nil {
				return conn.confirmationSSH, nil
			}
			if ordinarySSH != nil {
				conn.confirmationSSH = ordinarySSH
				return ordinarySSH, nil
			}
			// Resolve SSH authentication and sudo exactly as ordinary requests do,
			// once. This read-only preparation never installs or sets up anything.
			session, err := openSSH(ctx, h, credentials, run.Project.Secrets, keyPath, settings.Identities, settings.User+"@"+net.JoinHostPort(settings.Hostname, settings.Port))
			if err != nil {
				return nil, err
			}
			if err := want.check(session.NodeID, session.Arch); err != nil {
				session.Close()
				return nil, err
			}
			session.Runner, session.Hash = runnerDirectory+"/"+hash+"/mmx-runner", hash
			conn.confirmationSSH = session
			return session, nil
		}
	}()
	var runner *artifact
	binaryArch := ""
	loadArtifact := func(arch string) error {
		path := filepath.Join(c.RunnerDir, "mmx-runner-linux-"+arch)
		if expected != "" {
			if !runnerHash.MatchString(expected) {
				return fmt.Errorf("invalid SHA-256 hash for the saved node runner executable")
			}
			path = filepath.Join(c.State, "runners", expected)
		}
		a, err := c.runnerArtifact(path)
		if err != nil {
			return err
		}
		runner, conn.Hash = a, a.hash
		if expected != "" && conn.Hash != expected {
			return fmt.Errorf("saved node runner executable does not match the recorded SHA-256 hash")
		}
		binaryArch = arch
		return nil
	}
	// open reaches the node through verified SSH and installs the runner; a node
	// that is not the wanted machine is refused first.
	open := func(ctx context.Context, want machine) (*sshSession, error) {
		native, err := openSSH(ctx, h, credentials, run.Project.Secrets, keyPath, settings.Identities, settings.User+"@"+net.JoinHostPort(settings.Hostname, settings.Port))
		if err != nil {
			return nil, err
		}
		if err := want.check(native.NodeID, native.Arch); err != nil {
			native.Close()
			return nil, err
		}
		conn.Architecture = native.Arch
		if binaryArch != native.Arch {
			if err := loadArtifact(native.Arch); err != nil {
				native.Close()
				return nil, err
			}
		}
		if err := native.installRunner(ctx, runner); err != nil {
			native.Close()
			return nil, err
		}
		if !runner.versionMatched.Load() {
			var answer struct {
				Protocol int    `json:"protocol"`
				Version  string `json:"version"`
			}
			err := native.runner(ctx, "version", nil, &answer)
			if err == nil {
				err = runner.checkVersion(answer.Version, expected != "")
			}
			if err != nil {
				native.Close()
				return nil, err
			}
		}
		return native, nil
	}
	// A connection that loses HTTPS continues over SSH to the same machine only.
	conn.fallback = func(ctx context.Context) (*sshSession, error) {
		return open(ctx, machine{conn.NodeID, conn.Architecture})
	}
	if h.Transport == "ssh_only" {
		conn.native, err = open(ctx, recorded)
		if err != nil {
			return nil, err
		}
		conn.NodeID, conn.BootID, conn.Transport = conn.native.NodeID, conn.native.BootID, "ssh"
		complete = true
		return conn, nil
	}
	var known endpoint.Info
	raw, err := os.ReadFile(pinPath)
	if err == nil {
		if err := protocol.Decode(raw, &known); err != nil {
			return nil, fmt.Errorf("invalid saved node HTTPS API identity: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	if known.NodeID != "" {
		if !runnerHash.MatchString(known.NodeID) || !runnerHash.MatchString(known.TLSPin) || (known.Architecture != "arm64" && known.Architecture != "amd64") {
			return nil, fmt.Errorf("invalid saved node HTTPS API identity")
		}
		conn.NodeID = known.NodeID
		if err := loadArtifact(known.Architecture); err != nil {
			return nil, err
		}
	}
	signers, keyErr := nodeSigners(h, settings, keyPath, credentials)
	httpsURL := h.Endpoint
	if httpsURL == "" {
		httpsURL = "https://" + net.JoinHostPort(settings.Hostname, "7443")
	}
	conn.httpsURL = httpsURL
	var current endpoint.Info
	stale := "" // why the saved identity no longer describes this node
	// apiState is systemd's state of the node HTTPS API process once verified
	// SSH has asked, "not-found" when its unit does not exist.
	apiState := ""
	if known.NodeID != "" && len(signers) > 0 {
		// started: this connect has started the node HTTPS API process, so
		// HTTPS gets one more check and the process no second start.
		for started := false; ; started = true {
			conn.direct = endpoint.NewClient(httpsURL, settings.User, known.TLSPin, signers)
			conn.retainConfirmationHTTPS(conn.direct)
			err := conn.direct.Call(ctx, "GET", "/v1/info", nil, &current)
			var identity *endpoint.IdentityError
			switch {
			case errors.As(err, &identity):
				stale = identity.Reason
			case err == nil && (current.NodeID != known.NodeID || current.TLSPin != known.TLSPin || current.Architecture != known.Architecture):
				stale = "the node HTTPS API reports a different node identity"
			case err == nil:
				if err := recorded.check(current.NodeID, current.Architecture); err != nil {
					return nil, err
				}
				if current.RunnerHash == conn.Hash {
					if err := runner.checkVersion(current.RunnerVersion, expected != ""); err != nil {
						return nil, err
					}
					if conn.native != nil {
						conn.native.Close()
						conn.native = nil
					}
					conn.Architecture, conn.BootID = current.Architecture, current.BootID
					conn.Transport = "https"
					complete = true
					return conn, nil
				}
			case !endpoint.ReachabilityFailure(err):
				return nil, err
			}
			conn.direct.Close()
			conn.direct = nil
			if err == nil || stale != "" || known.RunnerHash != conn.Hash {
				break
			}
			if conn.native == nil {
				native, openErr := open(ctx, recorded)
				if openErr != nil {
					return nil, openErr
				}
				conn.native = native
			}
			if conn.native.NodeID != known.NodeID || conn.native.Arch != known.Architecture {
				stale = "verified SSH reached another machine"
				break
			}
			state := conn.native.checkHTTPS(ctx)
			apiState = state.apiState
			if state.checkErr == nil && state.apiState == "not-found" {
				// The node was removed from MMX's management and added back:
				// its unit is gone, so only setup brings the HTTPS service
				// back. The session is verified SSH to the saved machine, as
				// for a new node.
				stale = "its systemd unit " + endpoint.HTTPSAPIUnit + " does not exist on the node"
				break
			}
			if !started && state.checkErr == nil && state.apiState != "active" {
				// MMX's own process is down. With the identity and the runner
				// matching, starting its unit, as at boot, is enough; setup
				// would wait for a running node job.
				if _, state.startErr = conn.native.privileged(ctx, "systemctl start "+endpoint.HTTPSAPIUnit, nil); state.startErr == nil {
					notify(httpsService + " was not running (its systemd unit " + endpoint.HTTPSAPIUnit + " was " + state.apiState + "); MMX started it again through verified SSH")
					continue
				}
			}
			conn.Transport, conn.BootID = "ssh", conn.native.BootID
			conn.FallbackReason = state.fallbackReason(httpsURL, err)
			notify(conn.FallbackReason)
			complete = true
			return conn, nil
		}
	}
	if conn.native == nil {
		conn.native, err = open(ctx, recorded)
		if err != nil {
			return nil, err
		}
	}
	conn.NodeID, conn.BootID = conn.native.NodeID, conn.native.BootID
	if apiState == "" {
		state := conn.native.checkHTTPS(ctx)
		if state.checkErr != nil {
			return nil, fmt.Errorf("checking over SSH whether MMX's HTTPS service is set up on the node: %w", state.checkErr)
		}
		apiState = state.apiState
	}
	// Say so only once verified SSH passed its first check: a node that fails
	// it is not set up.
	switch {
	case apiState == "not-found" && run.Dryrun:
		// A dryrun changes nothing that a run would have to undo: the node
		// keeps only the node runner executable, and the first run starts the
		// HTTPS service.
		conn.Transport = "ssh"
		complete = true
		return conn, nil
	case apiState == "not-found":
		remove := "mmx remove-mmx-from-node " + name
		if run.Inventory.Name != "" {
			remove += " --inventory " + run.Inventory.Name
		}
		// Two plain sentences say who can use the port and how to have none.
		notify("setting up MMX's HTTPS service on " + name + " (" + endpoint.HTTPSAPIUnit + ", TCP port 7443 on every network address). " +
			"Only SSH keys that can already log in to " + name + " can use the service. " +
			"For no port, set connection: ssh_only on " + name + " in the inventory and run " + remove)
	case stale != "":
		notify("saved node HTTPS API identity is out of date (" + stale + "); setting up MMX again through verified SSH")
	default:
		notify("setting up MMX through verified SSH")
	}
	// SSH can unlock an encrypted key through askpass; reuse that passphrase for
	// HTTPS signing. No passwords are included in this setup payload.
	if keyErr != nil || len(signers) == 0 {
		signers, keyErr = nodeSigners(h, settings, keyPath, credentials)
		if keyErr != nil {
			return nil, keyErr
		}
	}
	request := endpoint.SetupRequest{User: conn.native.User, Remote: conn.native.Remote, Local: conn.native.Local, SSHPort: conn.native.SSHPort}
	for _, signer := range signers {
		request.Keys = append(request.Keys, signer.PublicKey().Marshal())
	}
	setup := func() (endpoint.SetupResult, error) {
		raw, err := json.Marshal(request)
		if err != nil {
			return endpoint.SetupResult{}, err
		}
		var result endpoint.SetupResult
		if err := conn.native.runner(ctx, "setup", bytes.NewReader(raw), &result); err != nil {
			return endpoint.SetupResult{}, fmt.Errorf("automatic setup of mmx-runner, MMX's HTTPS service on this node: %w", err)
		}
		return result, nil
	}
	result, err := setup()
	if err != nil {
		return nil, err
	}
	if result.NeedKey {
		conn.native.askpassMu.Lock()
		passwordUsed := conn.native.passwordUsed
		conn.native.askpassMu.Unlock()
		if !passwordUsed {
			reason := ""
			if result.Reason != "" {
				reason = " (" + result.Reason + ")"
			}
			return nil, fmt.Errorf("no SSH signing key on the controller machine matches the administrator account's SSH access%s; load the account's SSH key into ssh-agent, set ssh_identity_file, or select connection: ssh_only", reason)
		}
		signer, err := access.EnsureKey(keyPath)
		if err != nil {
			return nil, err
		}
		notify("establishing ordinary SSH key access; private key: " + keyPath)
		request.AddKey = signer.PublicKey().Marshal()
		signers = []ssh.Signer{signer}
		result, err = setup()
		if err != nil {
			return nil, err
		}
	}
	if result.NeedKey || result.Info.NodeID != conn.NodeID || result.Info.RunnerHash != conn.Hash || result.Info.Architecture != conn.Architecture {
		return nil, fmt.Errorf("node HTTPS API setup did not confirm SSH key authorization or returned a different node ID, architecture, or runner executable hash than expected")
	}
	switch {
	case known.NodeID != "" && result.Info.NodeID != known.NodeID:
		notify("node address now reaches a different machine with a trusted SSH host key; saved node identity updated")
	case known.TLSPin != "" && result.Info.TLSPin != known.TLSPin:
		notify("node HTTPS API key changed, verified through SSH; saved node identity updated")
	}
	if err := os.MkdirAll(filepath.Dir(pinPath), 0700); err != nil {
		return nil, err
	}
	if err := durable.JSON(pinPath, result.Info); err != nil {
		return nil, err
	}
	conn.direct = endpoint.NewClient(httpsURL, conn.native.User, result.Info.TLSPin, signers)
	conn.retainConfirmationHTTPS(conn.direct)
	for attempt := 0; ; attempt++ {
		err = conn.direct.Call(ctx, "GET", "/v1/info", nil, &current)
		if err == nil || !endpoint.ReachabilityFailure(err) || attempt == 2 {
			break
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
	if err != nil {
		if !endpoint.ReachabilityFailure(err) {
			return nil, err
		}
		conn.direct.Close()
		conn.direct = nil
		conn.Transport = "ssh"
		conn.FallbackReason = conn.native.checkHTTPS(ctx).fallbackReason(httpsURL, err)
		notify(conn.FallbackReason)
	} else {
		if current.NodeID != conn.NodeID || current.RunnerHash != conn.Hash || current.Architecture != conn.Architecture {
			return nil, fmt.Errorf("node HTTPS API readiness check returned a different node or runner executable identity than setup returned")
		}
		conn.native.Close()
		conn.native = nil
		conn.Transport = "https"
	}
	complete = true
	return conn, nil
}

// call is every node request's one path: over HTTPS while the connection
// uses it, otherwise over verified SSH. Only a reachability failure moves the
// connection to SSH; an identity, authentication or authorization rejection
// or an HTTP error answer is final. After a transport failure an idempotent
// request is repeated, at most three attempts in all. Submit is not
// repeated; its caller inspects the job instead.
func (c *Connection) call(ctx context.Context, idempotent bool, https func(*endpoint.Client) error, ssh func(*sshSession) error) error {
	for attempt := 1; ; attempt++ {
		var err error
		if c.direct != nil {
			err = https(c.direct)
			if endpoint.ReachabilityFailure(err) && ctx.Err() == nil {
				c.lostHTTPS = err
				c.direct.Close()
				c.direct = nil
			}
		} else {
			if c.native == nil {
				if c.native, err = c.fallback(ctx); err != nil {
					return err
				}
			}
			if c.Transport != "ssh" {
				c.Transport, c.FallbackReason = "ssh", c.native.checkHTTPS(ctx).fallbackReason(c.httpsURL, c.lostHTTPS)
			}
			err = ssh(c.native)
		}
		if !idempotent || attempt == 3 || !transportFailure(err) || ctx.Err() != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// transportFailure means the node was not reached or the connection was lost,
// so the request's outcome is unknown.
func transportFailure(err error) bool {
	var remote *sshError
	return endpoint.ReachabilityFailure(err) || (errors.As(err, &remote) && remote.transient)
}

// rejectedError is the node's definite answer that it did not admit a job: an
// HTTPS error response or the runner exiting with its error. Transport failures
// leave the job uncertain instead.
type rejectedError struct{ error }

func (e *rejectedError) Unwrap() error { return e.error }

// send makes the JSON node request called name, one of protocol's Request
// names: POST /v1/<name> over HTTPS or mmx-runner <name> over SSH, with the
// same body either way.
func (c *Connection) send(ctx context.Context, name string, idempotent bool, body []byte, answer any) error {
	return c.call(ctx, idempotent, func(d *endpoint.Client) error {
		return d.Call(ctx, "POST", "/v1/"+name, body, answer)
	}, func(s *sshSession) error {
		return s.runner(ctx, name, bytes.NewReader(body), answer)
	})
}

// ask makes a JSON node request that is safe to repeat after a lost
// connection, which every request except submit is.
func (c *Connection) ask(ctx context.Context, name string, request, answer any) error {
	body, err := json.Marshal(request)
	if err != nil {
		return err
	}
	return c.send(ctx, name, true, body, answer)
}

func (c *Connection) submitBytes(ctx context.Context, body []byte, id string) (protocol.Response, error) {
	var result protocol.Response
	err := c.send(ctx, protocol.RequestSubmit, false, body, &result)
	var answered *endpoint.HTTPError
	var remote *sshError
	if transportFailure(err) && ctx.Err() == nil {
		// The node may have admitted the job: inspect it by ID, never resubmit.
		return c.Status(ctx, id)
	} else if errors.As(err, &answered) || (errors.As(err, &remote) && remote.rejected) {
		err = &rejectedError{err}
	}
	return result, checkJobID(result, id, err)
}
func (c *Connection) Status(ctx context.Context, id string) (protocol.Response, error) {
	return c.inspect(ctx, "GET", "status", id)
}

// Progress waits on the node for a running job to move past what request holds.
func (c *Connection) Progress(ctx context.Context, request protocol.ProgressRequest) (protocol.Response, error) {
	var result protocol.Response
	err := c.ask(ctx, protocol.RequestProgress, request, &result)
	return result, checkJobID(result, request.ID, err)
}
func (c *Connection) Stop(ctx context.Context, id string) (protocol.Response, error) {
	return c.inspect(ctx, "POST", "stop", id)
}
func (c *Connection) inspect(ctx context.Context, method, action, id string) (protocol.Response, error) {
	var result protocol.Response
	err := c.call(ctx, true, func(d *endpoint.Client) error {
		return d.Call(ctx, method, "/v1/"+action+"/"+id, nil, &result)
	}, func(s *sshSession) error {
		return s.runner(ctx, action+" "+quote(id), nil, &result)
	})
	return result, checkJobID(result, id, err)
}
func checkJobID(r protocol.Response, id string, err error) error {
	if err == nil && r.ID != id {
		return fmt.Errorf("mmx-runner returned a node job ID different from the requested ID")
	}
	return err
}

// RecordLastSuccessfulRun asks the node to replace its record of the
// playbook's last successful run. Repeating it writes the same record.
func (c *Connection) RecordLastSuccessfulRun(ctx context.Context, request protocol.LastSuccessfulRunRequest) error {
	var result struct {
		Status string `json:"status"`
	}
	err := c.ask(ctx, protocol.RequestLastSuccessfulRun, request, &result)
	if err == nil && result.Status != "recorded" {
		err = fmt.Errorf("node did not confirm saving the last successful run record")
	}
	return err
}

// Facts asks the node for its facts, measured as a node job measures them,
// for a fleet stage's when that reads node facts.
func (c *Connection) Facts(ctx context.Context, managedNode string) (facts.Snapshot, error) {
	var result facts.Snapshot
	err := c.ask(ctx, protocol.RequestFacts, protocol.FactsRequest{Node: managedNode}, &result)
	return result, err
}

// RebootRefusal asks the node whether it refuses a reboot task because it is
// the machine that runs this controller process. It returns the node's
// refusal message, or "" when the node would reboot.
func (c *Connection) RebootRefusal(ctx context.Context, controllerMachine protocol.ControllerMachine) (string, error) {
	var result protocol.RebootRefusalResponse
	err := c.ask(ctx, protocol.RequestRebootRefusal, protocol.RebootRefusalRequest{ControllerMachine: controllerMachine}, &result)
	return result.Refusal, err
}

// UseKeptFiles asks the node to use the files it keeps for this job and
// returns the SHA-256 of each file it must be sent. The request only links
// files the node already keeps, so a repeat is safe.
func (c *Connection) UseKeptFiles(ctx context.Context, jobID string, files []assets.Ref) (map[string]bool, error) {
	var result protocol.KeptFilesResponse
	err := c.ask(ctx, protocol.RequestKeptFiles, protocol.KeptFilesRequest{JobID: jobID, Files: files}, &result)
	toSend := map[string]bool{}
	for _, digest := range result.FilesToSend {
		toSend[digest] = true
	}
	return toSend, err
}

// ReadFile asks the node for one file's content, as read_file reads it. A
// resume uses it to read a read_file secret again.
func (c *Connection) ReadFile(ctx context.Context, path string) (any, error) {
	var result protocol.ReadFileResponse
	err := c.ask(ctx, protocol.RequestReadFile, protocol.ReadFileRequest{Path: path}, &result)
	return result.Content, err
}

func (c *Connection) retainConfirmationHTTPS(client *endpoint.Client) {
	url, user, pin, signers := client.URL, client.User, client.Pin, client.Signers
	c.newConfirmationHTTPS = func() *endpoint.Client {
		return endpoint.NewConfirmationClient(url, user, pin, signers)
	}
}

// ConfirmReachability attempts exactly the requested connection kind. Progress
// may fall back to SSH, but that cannot replace an HTTPS confirmation. These
// requests do not use call's retries, fallback, installation, or mutable state.
func (c *Connection) ConfirmReachability(ctx context.Context, request protocol.ReachabilityConfirmationRequest, kind protocol.ConnectionKind) (answer protocol.ReachabilityConfirmationResponse, err error) {
	if err := request.Validate(); err != nil {
		return answer, err
	}
	body, err := json.Marshal(request)
	if err != nil {
		return answer, err
	}
	err = c.callOnNewConnection(ctx, kind, "POST", "/v1/"+protocol.RequestConfirmReachability, protocol.RequestConfirmReachability, body, &answer)
	if err != nil {
		return answer, err
	}
	if answer.NodeJobID != request.NodeJobID || answer.OwedRestoreID != request.OwedRestoreID || answer.ConfirmationPhase != request.ConfirmationPhase || answer.ReceivedConnectionKind != kind {
		return answer, fmt.Errorf("reachability confirmation answer does not match its node job, change, phase, or connection kind")
	}
	if pending := answer.PendingReachabilityConfirmation; pending != nil && (pending.NodeJobID != request.NodeJobID || pending.OwedRestoreID != request.OwedRestoreID) {
		return answer, fmt.Errorf("pending reachability confirmation does not match the requested node job and change")
	}
	return answer, nil
}

// StatusOnNewConnection discovers readiness without confirming any change. A
// later confirmation must open another connection, after this answer arrives.
func (c *Connection) StatusOnNewConnection(ctx context.Context, id string, kind protocol.ConnectionKind) (protocol.Response, error) {
	var answer protocol.Response
	err := c.callOnNewConnection(ctx, kind, "GET", "/v1/status/"+id, "status "+quote(id), nil, &answer)
	return answer, checkJobID(answer, id, err)
}

func (c *Connection) callOnNewConnection(ctx context.Context, kind protocol.ConnectionKind, method, path, command string, body []byte, answer any) (err error) {
	defer func() {
		// Killing SSH, including read-only preparation, reports a process error.
		// Preserve cancellation so a timeout can be tried again within the wait.
		if err != nil && ctx.Err() != nil {
			err = ctx.Err()
		}
	}()
	switch kind {
	case protocol.ConnectionSSH:
		if c.prepareConfirmationSSH == nil {
			return fmt.Errorf("SSH reachability confirmation is unavailable for this node")
		}
		var session *sshSession
		session, err = c.prepareConfirmationSSH(ctx)
		if err != nil {
			return err
		}
		err = session.runnerOnNewConnection(ctx, command, bytes.NewReader(body), answer)
	case protocol.ConnectionHTTPS:
		if c.newConfirmationHTTPS == nil {
			return fmt.Errorf("HTTPS reachability confirmation is unavailable for this node")
		}
		client := c.newConfirmationHTTPS()
		defer client.Close()
		err = client.Call(ctx, method, path, body, answer)
	default:
		return fmt.Errorf("reachability confirmation requires SSH or HTTPS")
	}
	return err
}

// probeMachine opens one fresh, read-only SSH connection and reads the
// node's machine ID and boot ID. It returns the node ID, which is the
// SHA-256 of the machine ID, and the boot ID. name only words the error.
func (s *sshSession) probeMachine(ctx context.Context, name string) (nodeID, bootID string, err error) {
	var output responseBuffer
	if err := s.rawSSHRequest(ctx, "cat /etc/machine-id "+bootIDFile, nil, &output, true); err != nil {
		return "", "", err
	}
	if output.exceeded {
		return "", "", protocol.ErrResponseLimit
	}
	lines := strings.Fields(output.data.String())
	if len(lines) != 2 || !bootIDPattern.MatchString(lines[1]) {
		return "", "", fmt.Errorf("The SSH probe of node %s did not return one machine ID and one boot ID: %s.", name, quoteShortened(output.data.String()))
	}
	identity := sha256.Sum256([]byte(lines[0]))
	return hex.EncodeToString(identity[:]), lines[1], nil
}
