package controller

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"mmx/internal/access"
	"mmx/internal/config"
	"mmx/internal/durable"
	"mmx/internal/modules"
	"mmx/internal/protocol"
)

type sshError struct {
	message                   string
	transient, authentication bool
	// rejected means the remote command ran and exited with its own error.
	rejected bool
	// silent means the command printed nothing on standard error.
	silent bool
}

func (e *sshError) Error() string { return e.message }
func quote(s string) string       { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }

type sshSettings struct {
	Hostname, User, Port, Agent string
	Identities                  []string
	IdentitiesOnly              bool
}

func sshOptions(h config.Node) []string {
	// Keyboard-interactive prompts are written by the server. Password and key
	// passphrase prompts are OpenSSH's own, so askpass answers only those.
	args := []string{"-T", "-o", "StrictHostKeyChecking=yes", "-o", "NoHostAuthenticationForLocalhost=no", "-o", "ForwardAgent=no", "-o", "KbdInteractiveAuthentication=no", "-o", "ConnectTimeout=10", "-o", "ServerAliveInterval=15", "-o", "ServerAliveCountMax=3", "-o", "LogLevel=ERROR"}
	if h.SSHConfig != "" {
		args = append(args, "-F", h.SSHConfig)
	}
	if h.KnownHosts != "" {
		args = append(args, "-o", "UserKnownHostsFile="+h.KnownHosts)
	}
	if h.IdentityFile != "" {
		args = append(args, "-i", h.IdentityFile, "-o", "IdentitiesOnly=yes")
	}
	if h.Port != 0 {
		args = append(args, "-p", strconv.Itoa(h.Port))
	}
	if h.User != "" {
		args = append(args, "-l", h.User)
	}
	return args
}

// sshConfigurationError means OpenSSH could not read the SSH configuration
// that the node's settings name, such as an ssh_config file that does not
// exist: MMX could not try to reach the node at all.
type sshConfigurationError struct{ address, reason string }

func (e *sshConfigurationError) Error() string {
	return "resolve SSH configuration for " + e.address + ": " + e.reason
}

func resolveSSH(ctx context.Context, h config.Node) (sshSettings, error) {
	args := append([]string{"-G"}, sshOptions(h)...)
	args = append(args, "--", h.Address)
	b, err := exec.CommandContext(ctx, "ssh", args...).Output()
	if err != nil && ctx.Err() != nil {
		return sshSettings{}, ctx.Err()
	}
	if err != nil {
		// OpenSSH's own words say what it could not read.
		reason := err.Error()
		var exit *exec.ExitError
		if errors.As(err, &exit) && len(bytes.TrimSpace(exit.Stderr)) > 0 {
			reason = err.Error() + ": " + string(bytes.TrimSpace(exit.Stderr))
		}
		return sshSettings{}, &sshConfigurationError{h.Address, reason}
	}
	s := sshSettings{Agent: os.Getenv("SSH_AUTH_SOCK")}
	for _, line := range strings.Split(string(b), "\n") {
		key, value, _ := strings.Cut(line, " ")
		switch key {
		case "hostname":
			s.Hostname = value
		case "user":
			s.User = value
		case "port":
			s.Port = value
		case "identityfile":
			s.Identities = append(s.Identities, value)
		case "identitiesonly":
			s.IdentitiesOnly = value == "yes"
		case "identityagent":
			if value == "SSH_AUTH_SOCK" || strings.HasPrefix(value, "$") {
				s.Agent = os.Getenv(strings.TrimPrefix(value, "$"))
			} else {
				s.Agent = value
			}
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return s, err
	}
	local, err := user.Current()
	if err != nil {
		return s, err
	}
	hostname, err := os.Hostname()
	if err != nil {
		return s, err
	}
	short, _, _ := strings.Cut(hostname, ".")
	// `ssh -G` prints these OpenSSH tokens unexpanded; passphrase prompts name the
	// expanded path. An unsupported token leaves a path no prompt can match.
	tokens := strings.NewReplacer("%%", "%", "%d", home, "%h", s.Hostname, "%n", h.Address, "%r", s.User, "%p", s.Port,
		"%u", local.Username, "%i", local.Uid, "%l", hostname, "%L", short)
	expand := func(path string) string {
		if strings.HasPrefix(path, "~/") {
			path = filepath.Join(home, path[2:])
		}
		return tokens.Replace(path)
	}
	for i := range s.Identities {
		s.Identities[i] = expand(s.Identities[i])
	}
	s.Agent = expand(s.Agent)
	return s, nil
}

type sshSession struct {
	Node                                                         config.Node
	NodeID, Arch, User, Remote, Local, Runner, Hash, ControlPath string
	// BootID is the kernel's ID of the node's current boot.
	BootID       string
	SSHPort      int
	Credentials  *access.Credentials
	Secrets      map[string]string
	sudo         bool
	sudoPassword string
	allowAskpass bool
	passwordUsed bool
	askpassPath  string
	stopAskpass  func()
	askpassMu    sync.Mutex
	askpassError error
}

// openSSH opens a verified session. identities are the key files ssh resolved
// for this node; with the managed key, they are the only files whose passphrase
// askpass supplies.
func openSSH(ctx context.Context, h config.Node, credentials *access.Credentials, secrets map[string]string, managedKey string, identities []string, destination string) (*sshSession, error) {
	s, err := newSSHSession(h, credentials, secrets, managedKey, identities, destination)
	if err != nil {
		return nil, err
	}
	// The SSH connection details come from the verified remote session, before sudo
	// strips SSH_CONNECTION from the environment.
	probe, err := s.inspect(ctx, "uname -s; uname -m; id -u; id -un; cat "+bootIDFile+"; "+systemdProbe+"; cat /etc/machine-id; printf '\\n%s\\n' \"$SSH_CONNECTION\"")
	if err != nil {
		s.Close()
		var failure *sshError
		if errors.As(err, &failure) && failure.rejected && failure.silent {
			// ssh exits 1 without a word when the account's login shell
			// refuses the command, as /usr/sbin/nologin and /bin/false do.
			failure.message += "the first command of MMX printed nothing and exited with status 1. " +
				"The likely cause is that the login shell of the SSH account refuses commands, as /usr/sbin/nologin and /bin/false do. " +
				"Give the account a shell such as /bin/sh, or use another ssh_user"
		}
		return nil, err
	}
	parts, err := probeAnswer(string(probe))
	if err != nil {
		s.Close()
		return nil, err
	}
	id := sha256.Sum256([]byte(parts[5]))
	s.NodeID, s.BootID = hex.EncodeToString(id[:]), parts[4]
	switch parts[1] {
	case "aarch64", "arm64":
		s.Arch = "arm64"
	case "x86_64":
		s.Arch = "amd64"
	default:
		s.Close()
		return nil, fmt.Errorf("unsupported node architecture %s", parts[1])
	}
	s.User, s.Remote, s.Local = parts[3], parts[6], parts[8]
	s.SSHPort, err = strconv.Atoi(parts[9])
	if err != nil {
		s.Close()
		return nil, err
	}
	s.sudo = parts[2] != "0"
	if s.sudo {
		if _, err := s.ssh(ctx, "LC_ALL=C sudo -n true"); err != nil {
			if !sudoAsksForPassword(err.Error()) {
				s.Close()
				return nil, err
			}
			s.sudoPassword, err = credentials.Named("sudo password", destination, modules.ConnectionSecretName(h.SudoPassword), secrets[modules.ConnectionSecretName(h.SudoPassword)])
			if err != nil {
				s.Close()
				return nil, err
			}
			if _, err := s.privileged(ctx, "true", nil); err != nil {
				s.Close()
				return nil, err
			}
		}
	}
	return s, nil
}

// newSSHSession prepares ordinary SSH authentication without connecting or sudo.
func newSSHSession(h config.Node, credentials *access.Credentials, secrets map[string]string, managedKey string, identities []string, destination string) (*sshSession, error) {
	s := &sshSession{Node: h, Credentials: credentials, Secrets: secrets}
	if managedKey != "" && h.IdentityFile == "" {
		if _, err := os.Stat(managedKey); err == nil {
			s.Node.IdentityFile = managedKey
		}
	}
	keys := map[string]bool{}
	for _, path := range identities {
		keys[path] = true
	}
	if s.Node.IdentityFile != "" {
		keys[s.Node.IdentityFile] = true
	}
	dir, err := os.MkdirTemp("/tmp", "mmx-ssh-")
	if err != nil {
		return nil, err
	}
	s.ControlPath = filepath.Join(dir, "control")
	s.askpassPath, s.stopAskpass, err = access.Askpass(dir, func(prompt string) (string, error) {
		kind, target, name, env := "SSH password", destination, modules.ConnectionSecretName(h.SSHPassword), secrets[modules.ConnectionSecretName(h.SSHPassword)]
		var value string
		var err error
		if strings.Contains(strings.ToLower(prompt), "passphrase") {
			kind, name, env = "SSH key passphrase", "", ""
			if target = keyPrompt(prompt, keys); target == "" {
				err = fmt.Errorf("refusing an SSH passphrase prompt because the prompt does not match a key file MMX supplied to OpenSSH: %q", prompt)
			}
		} else if !passwordPrompt(prompt, destination) {
			err = fmt.Errorf("refusing an SSH password prompt that is not OpenSSH's prompt for %s: %q", destination, prompt)
		}
		if err == nil {
			value, err = credentials.Named(kind, target, name, env)
		}
		s.askpassMu.Lock()
		if err != nil {
			s.askpassError = err
		}
		if kind == "SSH password" && err == nil {
			s.passwordUsed = true
		}
		s.askpassMu.Unlock()
		return value, err
	})
	if err != nil {
		s.Close()
		return nil, err
	}
	return s, nil
}

// bootIDFile holds the kernel's ID of the current boot.
const bootIDFile = "/proc/sys/kernel/random/boot_id"

// bootIDPattern is the form of a kernel boot ID, a random UUID.
var bootIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// systemdProbe prints whether systemd runs the node, by sd_booted's rule:
// systemd makes the directory /run/systemd/system when it starts the
// machine, and no other init system makes it.
const systemdProbe = "if test -d /run/systemd/system; then echo systemd; else echo no_systemd; fi"

// probeAnswer reads what a node answered to the first SSH command of a
// session: the eleven words of a Linux node, which are the system, the
// architecture, the user ID, the user name, the boot ID, the answer of
// systemdProbe, the machine ID, and the four words of the SSH connection. It
// returns the ten words without the answer of systemdProbe. Any other answer
// is an error that says what MMX found and, where MMX knows it, what to do.
// A node that systemd does not run is refused before its machine ID is read:
// MMX manages only servers that run systemd, and the advice for a missing
// machine ID is for a node that does.
func probeAnswer(output string) ([]string, error) {
	first := ""
	for _, line := range strings.Split(output, "\n") {
		if first = strings.TrimSpace(line); first != "" {
			break
		}
	}
	switch words := len(strings.Fields(first)); {
	case words == 0:
		return nil, fmt.Errorf("this node answered nothing to the command uname -s, and MMX expects the one word Linux there. The SSH account must run the commands that MMX sends")
	case words > 1:
		// MMX knows only the text, not where it comes from.
		return nil, fmt.Errorf("the answer of this node to the command uname -s started with %s, and MMX expects the one word Linux there. If a login script of the SSH account prints this text, make it print nothing when the session has no terminal", quoteShortened(first))
	case first != "Linux":
		return nil, notLinuxError(first)
	}
	parts := strings.Fields(output)
	unexpected := fmt.Errorf("unexpected answer of this node to the first SSH command of MMX, which asks for the system, the architecture, the user ID, the user name, the boot ID, whether systemd runs the node, the machine ID, and the SSH connection: %s", quoteShortened(strings.Join(parts, " ")))
	switch {
	case len(parts) < 10:
		return nil, unexpected
	case !bootIDPattern.MatchString(parts[4]):
		return nil, fmt.Errorf("this node did not answer with the kernel's boot ID from %s, which MMX reads to tell a machine from a clone of it: it answered %s", bootIDFile, quoteShortened(parts[4]))
	case parts[5] == "no_systemd":
		return nil, errors.New("this node does not run systemd: the directory /run/systemd/system, which exists on every machine that systemd started, is not there. " +
			"MMX manages only Linux servers that run systemd. Remove this node from the inventory, or leave it out of the fleet stage with except_nodes")
	case parts[5] != "systemd":
		return nil, unexpected
	case len(parts) == 10:
		// cat printed no word: the file is not there, or it holds nothing.
		return nil, fmt.Errorf("this node has no systemd machine ID: its file /etc/machine-id is missing or empty. %s Run systemd-machine-id-setup as root on the node: it writes a new ID into the file. Then run MMX again", machineIDUse)
	}
	parts = append(parts[:5:5], parts[6:]...)
	// The words between the boot ID and the SSH connection are the file's.
	machineID := strings.Join(parts[5:len(parts)-4], " ")
	if raw, err := hex.DecodeString(machineID); err != nil || len(raw) != 16 {
		// systemd-machine-id-setup leaves a file that holds the word
		// "uninitialized" as it is (measured with systemd 257 and 262), and
		// creates a file that is not there with every version.
		return nil, fmt.Errorf("this node's systemd machine ID is not valid: its file /etc/machine-id holds %s, not 32 hexadecimal digits. %s As root on the node, remove the file and run systemd-machine-id-setup: it creates the file with a new ID. Then run MMX again", quoteShortened(machineID), machineIDUse)
	}
	return parts, nil
}

// machineIDUse says in a failure why a node needs its machine ID.
const machineIDUse = "MMX identifies a node by the systemd machine ID."

// quoteShortened quotes text that a node sent for a failure, and at most its
// first 80 characters.
func quoteShortened(text string) string {
	if runes := []rune(text); len(runes) > 80 {
		return strconv.Quote(string(runes[:80])) + " and more"
	}
	return strconv.Quote(text)
}

// notLinuxError refuses a machine whose one-word answer to uname -s is not
// Linux. The node runner executable is built for Linux only, so such a
// machine cannot be a node. Only the answer Darwin tells MMX that the
// machine is a Mac, which is what a macOS controller machine in its own
// inventory answers.
func notLinuxError(system string) error {
	name, only := strconv.Quote(system), "MMX manages Linux nodes only."
	if system == "Darwin" {
		name, only = name+" (macOS)", "MMX manages Linux nodes only, and a macOS controller machine cannot be a node."
	}
	return fmt.Errorf("this machine does not run Linux: it answered %s to the command uname -s. %s Remove this node from the inventory, or leave it out of the fleet stage with except_nodes", name, only)
}

// keyPrompt returns the key file named by OpenSSH's own passphrase prompt, or
// "" for any other text or a file this session did not give ssh. A local key's
// passphrase must never answer a prompt a managed node wrote.
func keyPrompt(prompt string, keys map[string]bool) string {
	path, local := strings.CutPrefix(prompt, "Enter passphrase for key '")
	path, closed := strings.CutSuffix(path, "': ")
	if local && closed && keys[path] {
		return path
	}
	return ""
}

// passwordPrompt reports whether prompt is OpenSSH's own password prompt for
// the resolved destination user@node:port. A jump host's prompt names another
// node, so the target's password never answers it.
func passwordPrompt(prompt, destination string) bool {
	user, address, _ := strings.Cut(destination, "@")
	managedNode, _, err := net.SplitHostPort(address)
	return err == nil && prompt == user+"@"+managedNode+"'s password: "
}

// ssh runs an unprivileged command while opening the session. Its first
// authentication failure enables askpass prompts and repeats the command, which
// settles how this session authenticates.
func (s *sshSession) ssh(ctx context.Context, command string) ([]byte, error) {
	out, err := s.output(ctx, command, nil)
	var failure *sshError
	if err != nil && !s.allowAskpass && errors.As(err, &failure) && failure.authentication {
		s.allowAskpass = true
		return s.output(ctx, command, nil)
	}
	return out, err
}

// sudoAsksForPassword reports whether sudo -n failed because it wants a
// password, as opposed to refusing the account. sudo says "a password is
// required" (or "a terminal is required"); sudo-rs, the sudo of Ubuntu 26.04,
// says "interactive authentication is required".
func sudoAsksForPassword(message string) bool {
	for _, wording := range []string{"password is required", "terminal is required", "interactive authentication is required"} {
		if strings.Contains(message, wording) {
			return true
		}
	}
	return false
}

// asRoot is the one place that gives a command root privilege: through sudo when
// the SSH user is not root, with sudo's password first on stdin when it needs one.
func (s *sshSession) asRoot(command string, input io.Reader) (string, io.Reader) {
	if input == nil {
		input = strings.NewReader("")
	}
	prefix := ""
	if s.sudo {
		prefix = "sudo -n "
		if s.sudoPassword != "" {
			prefix = "sudo -k -S -p '' "
			input = io.MultiReader(strings.NewReader(s.sudoPassword+"\n"), input)
		}
	}
	// Expand SSH_CONNECTION in the remote login shell before sudo clears the
	// environment. The value comes from this SSH session, not a job body.
	return "LC_ALL=C " + prefix + "/bin/sh -c " + quote("SSH_CONNECTION=$1; export SSH_CONNECTION; shift; "+command) + ` sh "$SSH_CONNECTION"`, input
}
func (s *sshSession) privileged(ctx context.Context, command string, input io.Reader) ([]byte, error) {
	command, input = s.asRoot(command, input)
	return s.output(ctx, command, input)
}

// runner runs one runner request and decodes its JSON answer.
func (s *sshSession) runner(ctx context.Context, args string, input io.Reader, out any) error {
	raw, err := s.privileged(ctx, quote(s.Runner)+" "+args, input)
	if err != nil {
		return err
	}
	return protocol.Decode(raw, out)
}

func (s *sshSession) runnerOnNewConnection(ctx context.Context, args string, input io.Reader, answer any) error {
	command, input := s.asRoot(quote(s.Runner)+" "+args, input)
	architecture := "x86_64"
	if s.Arch == "arm64" {
		architecture = "aarch64|arm64"
	}
	// Recheck identity on the very connection carrying confirmation, even if
	// read-only SSH preparation used an older shared connection.
	command = "node_machine_id=$(cat /etc/machine-id) && " +
		"test \"$(printf '%s' \"$node_machine_id\" | sha256sum | cut -d ' ' -f 1)\" = " + quote(s.NodeID) +
		" && test \"$(uname -s)\" = Linux && case $(uname -m) in " + architecture + ") true;; *) false;; esac" +
		" || { echo 'node identity changed before reachability confirmation' >&2; exit 1; }; " + command
	var output responseBuffer
	err := s.rawSSHRequest(ctx, command, input, &output, true)
	if output.exceeded {
		return protocol.ErrResponseLimit
	}
	if err != nil {
		return err
	}
	return protocol.Decode(output.data.Bytes(), answer)
}

// responseBuffer holds a command's answer and refuses one larger than an HTTPS
// response may be. The refused write closes the command's output. (Embedding
// bytes.Buffer would let io.Copy bypass Write through ReadFrom.)
type responseBuffer struct {
	data     bytes.Buffer
	exceeded bool
}

func (b *responseBuffer) Write(p []byte) (int, error) {
	if b.data.Len()+len(p) > protocol.ResponseLimit {
		b.exceeded = true
		return 0, protocol.ErrResponseLimit
	}
	return b.data.Write(p)
}
func (s *sshSession) output(ctx context.Context, command string, input io.Reader) ([]byte, error) {
	var out responseBuffer
	err := s.rawSSHTo(ctx, command, input, &out)
	if out.exceeded {
		return nil, fmt.Errorf("SSH %s: %w", s.Node.Address, protocol.ErrResponseLimit)
	}
	return out.data.Bytes(), err
}
func (s *sshSession) rawSSHTo(ctx context.Context, command string, input io.Reader, output io.Writer) error {
	return s.rawSSHRequest(ctx, command, input, output, false)
}

func (s *sshSession) requestOptions(ctx context.Context, fresh bool) []string {
	args := sshOptions(s.Node)
	if fresh {
		// Command-line values precede SSH configuration, whose ControlMaster,
		// ControlPath and ControlPersist must never re-enable connection sharing.
		seconds := 10
		if deadline, ok := ctx.Deadline(); ok {
			seconds = max(1, min(seconds, int((time.Until(deadline)+time.Second-1)/time.Second)))
		}
		for i, option := range args {
			if option == "ConnectTimeout=10" {
				args[i] = "ConnectTimeout=" + strconv.Itoa(seconds)
			}
		}
		args = append(args, "-o", "ControlMaster=no", "-o", "ControlPath=none", "-o", "ControlPersist=no")
	} else {
		args = append(args, "-o", "ControlMaster=auto", "-o", "ControlPersist=10s", "-o", "ControlPath="+s.ControlPath)
	}
	batch := "yes"
	if s.allowAskpass {
		batch = "no"
	}
	return append(args, "-o", "BatchMode="+batch, "-o", "NumberOfPasswordPrompts=1")
}

func (s *sshSession) rawSSHRequest(ctx context.Context, command string, input io.Reader, output io.Writer, fresh bool) error {
	args := append(s.requestOptions(ctx, fresh), "--", s.Node.Address, command)
	cmd := exec.CommandContext(ctx, "ssh", args...)
	cmd.Stdin = input
	// Declared connection secrets are read only by the parent. Even a user's
	// SendEnv rule cannot forward these variables through the child SSH process.
	excluded := map[string]bool{"SSH_ASKPASS": true, "SSH_ASKPASS_REQUIRE": true, "MMX_ASKPASS_SOCKET": true, "LC_ALL": true, "MMX_VAULT_PASSWORD": true, "MMX_VAULT_PASSWORD_FILE": true}
	for _, input := range s.Secrets {
		excluded[input] = true
	}
	for _, variable := range os.Environ() {
		key, _, _ := strings.Cut(variable, "=")
		if !excluded[key] {
			cmd.Env = append(cmd.Env, variable)
		}
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	cmd.Env = append(cmd.Env, "LC_ALL=C", "SSH_ASKPASS="+exe, "SSH_ASKPASS_REQUIRE=force", "MMX_ASKPASS_SOCKET="+s.askpassPath)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.Stdout = output
	err = cmd.Run()
	if err == nil {
		return nil
	}
	s.askpassMu.Lock()
	promptError := s.askpassError
	s.askpassMu.Unlock()
	if promptError != nil {
		return promptError
	}
	message := s.Credentials.Redact(strings.TrimSpace(stderr.String()))
	var exit *exec.ExitError
	auth := strings.Contains(message, "Permission denied")
	identity := strings.Contains(message, "Host key verification failed") || strings.Contains(message, "REMOTE HOST IDENTIFICATION HAS CHANGED")
	transient := errors.As(err, &exit) && exit.ExitCode() == 255 && !auth && !identity
	// The runner and sudo report their own errors with exit status 1. SSH itself
	// exits 255, and a crash or signal after admission leaves the job uncertain.
	rejected := errors.As(err, &exit) && exit.ExitCode() == 1
	switch {
	case identity:
		message = strings.TrimSuffix(message, ".") + "; trust the server's current host key in known_hosts first, as for any SSH tool"
	case auth:
		message = strings.TrimSuffix(message, ".") + "; check the node's SSH user and its key, agent, or ssh_password"
	case message == "" && transient:
		// OpenSSH discards what a ProxyJump or ProxyCommand prints while a
		// shared connection is being set up, so such a failure has no text.
		// MMX does not know that one failed, so the text says only where to look.
		message = "OpenSSH printed no reason. If this node's SSH configuration uses ProxyJump or ProxyCommand, OpenSSH prints nothing about a failure there while a shared connection is being set up. Run ssh -v to this node to see the reason"
	}
	return &sshError{message: fmt.Sprintf("SSH %s: %v: %s", s.Node.Address, err, message), transient: transient, authentication: auth && !identity, rejected: rejected, silent: message == ""}
}
func (s *sshSession) inspect(ctx context.Context, command string) ([]byte, error) {
	for n := 0; ; n++ {
		out, err := s.ssh(ctx, command)
		var failure *sshError
		if err == nil || n == 2 || !errors.As(err, &failure) || !failure.transient {
			return out, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}
func (s *sshSession) Close() {
	if s.stopAskpass != nil {
		s.stopAskpass()
	}
	if s.ControlPath == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, _ = exec.CommandContext(ctx, "ssh", "-F", "/dev/null", "-S", s.ControlPath, "-O", "exit", s.Node.Address).CombinedOutput()
	_ = os.RemoveAll(filepath.Dir(s.ControlPath))
}

// runnerDirectory holds one directory per runner build. SELinux policies label
// /opt/<name>/bin and everything in it as programs (bin_t), which systemd may
// start as a service; a path such as /usr/local/lib/mmx is labelled as a
// library, and a service started from it may not run sudo.
const runnerDirectory = "/opt/mmx/bin"

func (s *sshSession) installRunner(ctx context.Context, runner *artifact) error {
	hash := runner.hash
	s.Hash, s.Runner = hash, runnerDirectory+"/"+hash+"/mmx-runner"
	check := secureRunnerParents + "\nfor dir in /opt/mmx " + runnerDirectory + " " + quote(filepath.Dir(s.Runner)) + "; do owned_directory \"$dir\"; done\n" + "test ! -L " + quote(s.Runner) + " && test -f " + quote(s.Runner) + " && test \"$(stat -c '%u:%a' " + quote(s.Runner) + ")\" = 0:700 && sha256sum " + quote(s.Runner)
	existing, err := s.privileged(ctx, check, nil)
	if fields := strings.Fields(string(existing)); err == nil && len(fields) > 0 && fields[0] == hash {
		return nil
	}
	var transport *sshError
	if errors.As(err, &transport) && transport.transient {
		return err
	}
	script := secureRunnerParents + `
umask 077
for dir in /opt/mmx ` + runnerDirectory + ` /var/lib/mmx; do
 test ! -L "$dir"
 mkdir -p "$dir"
 owned_directory "$dir"
 chmod 700 "$dir"
done
# The same lock order and active-job check as node.Server.Install.
exec 9>/var/lib/mmx/admission.lock
flock 9
if test -f /var/lib/mmx/active; then
 job=$(cat /var/lib/mmx/active)
 lock="/var/lib/mmx/jobs/$job/worker.lock"
 if test ! -f "/var/lib/mmx/jobs/$job/result.json" && test -f "$lock" && ! flock -n "$lock" true; then
  by=$(cat "/var/lib/mmx/jobs/$job/started_by" 2>/dev/null || true)
  echo "node busy with job $job${by:+, started by $by}" >&2; exit 1
 fi
fi
for unit in /etc/systemd/system/mmx-owed-restore-*.service; do
  if [ -e "$unit" ] || [ -L "$unit" ]; then
    echo "cannot replace the node runner executable while owed restore unit ${unit##*/} exists; resolve the owed restore first" >&2; exit 1
  fi
done
exec 8>/var/lib/mmx/execution.lock
flock 8
` + "dir=" + quote(filepath.Dir(s.Runner)) + "\n" + `test ! -L "$dir"
mkdir -p "$dir"
owned_directory "$dir"
chmod 700 "$dir"
tmp=$(mktemp "$dir/.upload.XXXXXX")
trap 'rm -f "$tmp"' EXIT HUP INT TERM
`
	// A runner_url node fetches the same bytes from a nearby mirror instead of
	// receiving them compressed over SSH; the hash comes from the controller
	// either way. The script holds the node's admission lock, so a download that
	// stalls for 30 seconds on each of curl's three attempts fails.
	var input io.Reader
	source := ""
	if s.Node.RunnerURL != "" {
		url := strings.TrimSuffix(s.Node.RunnerURL, "/") + "/mmx-runner-" + hash
		script += "curl -fsSL --retry 2 --connect-timeout 10 --speed-limit 1 --speed-time 30 -o \"$tmp\" -- " + quote(url) + "\n"
		source = " from " + url
	} else {
		script += "gzip -dc > \"$tmp\"\n"
		input = bytes.NewReader(runner.gzipped())
	}
	script += "printf '%s  %s\\n' " + quote(hash) + ` "$tmp" | sha256sum -c - >/dev/null
chmod 700 "$tmp"
mv "$tmp" "$dir/mmx-runner"
sync -f "$dir"
# With SELinux, new files take their directory's label; give them the labels
# the node's policy has for these paths. SELinux runs when selinuxfs is
# mounted and a policy is loaded: without a policy every process has the
# context "kernel", as on images that mount selinuxfs only.
if test -e /sys/fs/selinux/enforce && test "$(tr -d '\000' < /proc/self/attr/current)" != kernel; then
	command -v restorecon >/dev/null || { echo "SELinux runs on this node, but restorecon is not installed; install the package policycoreutils" >&2; exit 1; }
	restorecon -R /opt/mmx
fi
`
	if _, err := s.privileged(ctx, script, input); err != nil {
		return fmt.Errorf("automatic setup of the node runner executable%s: %w", source, err)
	}
	return nil
}

const secureRunnerParents = `set -eu
owned_directory() {
 test ! -L "$1"
 test -d "$1"
 test "$(stat -c %u "$1")" = 0
 mode=$(stat -c %a "$1")
 test "$((0$mode & 022))" = 0
}
for dir in / /opt /var /var/lib; do owned_directory "$dir"; done
`

func writeRetained(path string, b []byte) error {
	if old, err := os.ReadFile(path); err == nil {
		if bytes.Equal(old, b) {
			return nil
		}
		return fmt.Errorf("saved node runner executable changed: %s", path)
	} else if !os.IsNotExist(err) {
		return err
	}
	return durable.Write(path, b, 0600)
}
