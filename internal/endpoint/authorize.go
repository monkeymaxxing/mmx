package endpoint

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rsa"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/crypto/ssh"

	"mmx/internal/modules"
)

var accountName = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_-]*[$]?$`)

type Authorizer struct{ SSHPort int }

// refusedBeforeKeyProof is a refusal reached before the client proved that
// the account lists its key: anyone can sign with a key made for the
// purpose, so such a refusal must not tell the client which accounts exist or
// how sudo and sshd are set up. The node HTTPS API answers it with one fixed
// text and writes the reason to the node's journal.
type refusedBeforeKeyProof struct{ error }

func (e *refusedBeforeKeyProof) Unwrap() error { return e.error }

// account checks the account's whole SSH and sudo access, for setup over
// verified SSH.
func (a Authorizer) account(ctx context.Context, name, remote, local string) (*user.User, sshdConfig, error) {
	u, settings, err := a.keyFiles(ctx, name, remote, local)
	if err != nil {
		return nil, nil, err
	}
	if err := a.access(ctx, u, settings, remote); err != nil {
		return nil, nil, err
	}
	return u, settings, nil
}

// keyFiles finds what decides which keys the account accepts: the account,
// and sshd's settings for this connection.
func (a Authorizer) keyFiles(ctx context.Context, name, remote, local string) (*user.User, sshdConfig, error) {
	if !accountName.MatchString(name) {
		return nil, nil, fmt.Errorf("invalid administrator account")
	}
	u, err := user.Lookup(name)
	if err != nil {
		return nil, nil, fmt.Errorf("administrator account is unavailable")
	}
	if a.SSHPort == 0 {
		a.SSHPort = 22
	}
	out, err := modules.SSHDSettings(ctx, "/usr/sbin/sshd", "-C", sshdConnectionSpec(name, remote, local, a.SSHPort))
	if err != nil {
		return nil, nil, fmt.Errorf("cannot read effective SSH authorization for %s: %w", name, err)
	}
	return u, parseSSHD(out), nil
}

// access checks the rest of what sshd and sudo decide for the account: an
// ordinary shell, an account that is current and not locked as sshd sees it,
// unrestricted root sudo, and the sshd policy this endpoint evaluates.
func (a Authorizer) access(ctx context.Context, u *user.User, settings sshdConfig, remote string) error {
	name := u.Username
	passwd, err := exec.CommandContext(ctx, "getent", "passwd", name).Output()
	if err != nil {
		return fmt.Errorf("cannot inspect administrator account")
	}
	fields := strings.Split(strings.TrimSpace(string(passwd)), ":")
	if len(fields) != 7 || fields[0] != name || fields[2] != u.Uid || !ordinaryShell(fields[6]) {
		return fmt.Errorf("administrator account details or shell could not be verified for HTTPS access; use connection: ssh_only")
	}
	shadow, err := exec.CommandContext(ctx, "getent", "shadow", name).Output()
	if err != nil || !accountCurrent(string(shadow), name, time.Now()) {
		return fmt.Errorf("administrator account is expired or its account policy is unavailable")
	}
	if lockedForSSHD(string(shadow), settings) {
		return fmt.Errorf("administrator account %s has a locked password, and sshd refuses a locked account unless UsePAM is yes; give the account a password, or no password with usermod -p '*' %s", name, name)
	}
	if u.Uid != "0" {
		cmd := exec.CommandContext(ctx, "sudo", "-n", "-ll", "-U", name)
		cmd.Env = append(os.Environ(), "LC_ALL=C")
		out, err := cmd.Output()
		if err != nil || !unrestrictedSudo(string(out)) {
			return fmt.Errorf("account %s does not have unrestricted root sudo access", name)
		}
	}
	if err := sshdPolicy(settings, name, u.Uid == "0"); err != nil {
		return err
	}
	groups, err := u.GroupIds()
	if err != nil {
		return err
	}
	var groupNames []string
	for _, id := range groups {
		group, err := user.LookupGroupId(id)
		if err != nil {
			return fmt.Errorf("cannot read administrator group %s: %w", id, err)
		}
		groupNames = append(groupNames, group.Name)
	}
	if !allowedAccount(settings, name, remote, groupNames) {
		return fmt.Errorf("SSH account policy denies %s", name)
	}
	return nil
}

// sshdConnectionSpec uses OpenSSH's connection-test keywords. Its host field
// is the remote host, regardless of MMX's term for a managed node.
func sshdConnectionSpec(name, remote, local string, port int) string {
	return fmt.Sprintf("user=%s,host=%s,addr=%s,laddr=%s,lport=%d", name, remote, remote, local, port)
}

// sshdConfig is `sshd -T` output. List options such as AllowUsers print one
// line per entry, so each key keeps every value in order. Keys are stored in
// lower case: OpenSSH up to 10.2 prints "pubkeyauthentication", and 10.5
// prints "PubkeyAuthentication".
type sshdConfig map[string][]string

func parseSSHD(out string) sshdConfig {
	settings := sshdConfig{}
	for _, line := range strings.Split(out, "\n") {
		if key, value, ok := strings.Cut(line, " "); ok {
			key = strings.ToLower(key)
			settings[key] = append(settings[key], value)
		}
	}
	return settings
}

// get reads a single-valued option.
func (c sshdConfig) get(key string) string {
	if values := c[key]; len(values) > 0 {
		return values[len(values)-1]
	}
	return ""
}

// unset reports whether a single-valued option has no value. OpenSSH up to
// 10.2 prints "none" for it; 10.5 leaves the line out.
func (c sshdConfig) unset(key string) bool {
	value := c.get(key)
	return value == "" || value == "none"
}

// sshdPolicy admits only the SSH policy this endpoint evaluates itself.
func sshdPolicy(settings sshdConfig, name string, root bool) error {
	login := settings.get("permitrootlogin")
	if settings.get("pubkeyauthentication") != "yes" || (root && login != "yes" && login != "without-password" && login != "prohibit-password") {
		return fmt.Errorf("SSH public-key access is disabled for %s", name)
	}
	if methods := settings.get("authenticationmethods"); !settings.unset("forcecommand") || !settings.unset("chrootdirectory") || (methods != "" && methods != "any" && methods != "publickey") {
		return fmt.Errorf("SSH access restrictions are not supported by the node HTTPS API; use connection: ssh_only")
	}
	// Delegate policies outside this endpoint's key-file contract to OpenSSH.
	// In particular, never ignore a revocation list or resolve Match Host using
	// an address when sshd would have used a verified DNS name.
	if settings.get("usedns") == "yes" || !settings.unset("revokedkeys") || settings.get("refuseconnection") == "yes" {
		return fmt.Errorf("SSH DNS, revocation-list, or connection policy is not supported by the node HTTPS API; use connection: ssh_only")
	}
	for _, field := range []string{"allowusers", "denyusers", "allowgroups", "denygroups"} {
		for _, pattern := range settings[field] {
			if strings.ContainsAny(pattern, "!, \t") {
				return fmt.Errorf("complex SSH %s patterns are not supported by the node HTTPS API; use connection: ssh_only", field)
			}
		}
	}
	return nil
}

func ordinaryShell(path string) bool {
	switch path {
	case "/bin/sh", "/bin/bash", "/bin/dash", "/bin/zsh", "/bin/ksh", "/bin/fish",
		"/usr/bin/sh", "/usr/bin/bash", "/usr/bin/dash", "/usr/bin/zsh", "/usr/bin/ksh", "/usr/bin/fish":
		return true
	}
	return false
}

// lockedForSSHD reports a locked password ("!" first in the shadow
// password field) that sshd refuses: OpenSSH on Linux refuses a locked
// account itself unless UsePAM is yes, which leaves the account check to PAM.
func lockedForSSHD(shadow string, settings sshdConfig) bool {
	fields := strings.Split(strings.TrimSpace(shadow), ":")
	return settings.get("usepam") != "yes" && len(fields) == 9 && strings.HasPrefix(fields[1], "!")
}

func accountCurrent(shadow, name string, now time.Time) bool {
	fields := strings.Split(strings.TrimSpace(shadow), ":")
	if len(fields) != 9 || fields[0] != name {
		return false
	}
	if fields[7] == "" || fields[7] == "-1" {
		return true
	}
	expires, err := strconv.ParseInt(fields[7], 10, 64)
	return err == nil && expires > now.Unix()/86400
}

func unrestrictedSudo(output string) bool {
	// Only the entries grant rights. The Defaults sections that sudo prints
	// above them are settings, and they can contain "!": "!visiblepw" on the
	// Red Hat family, or a command-specific "Defaults!/usr/bin/visudo".
	_, entries, found := strings.Cut(output, "Sudoers entry:")
	if !found {
		return false
	}
	// Negated commands or run-as accounts can narrow another ALL entry. Avoid
	// inventing a second sudoers evaluator; those policies use native SSH/sudo.
	for _, line := range strings.Split(entries, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "Options:") && strings.Contains(line, "!") {
			return false
		}
	}
	for _, entry := range strings.Split(output, "Sudoers entry:") {
		root, commands := false, false
		var allowed []string
		for _, line := range strings.Split(entry, "\n") {
			line = strings.TrimSpace(line)
			if options, ok := strings.CutPrefix(line, "Options:"); ok {
				for _, option := range strings.Split(options, ",") {
					switch strings.TrimSpace(option) {
					case "authenticate", "!authenticate", "setenv", "!setenv":
					default:
						return false
					}
				}
			}
			if strings.HasPrefix(line, "RunAsUsers:") {
				for _, who := range strings.Split(strings.TrimPrefix(line, "RunAsUsers:"), ",") {
					who = strings.TrimSpace(who)
					root = root || who == "ALL" || who == "root" || who == "#0"
				}
			}
			if line == "Commands:" {
				commands = true
				continue
			}
			if commands && line != "" {
				allowed = append(allowed, line)
			}
		}
		if root && len(allowed) == 1 && allowed[0] == "ALL" {
			return true
		}
	}
	return false
}

func matches(pattern, value string) bool {
	// OpenSSH patterns have only '*' and '?'; shell character classes are literal.
	expression := strings.NewReplacer(`\*`, ".*", `\?`, ".").Replace(regexp.QuoteMeta(pattern))
	ok, err := regexp.MatchString("^"+expression+"$", value)
	return err == nil && ok
}

// matchesUser follows sshd's match_user: a node part follows the last '@'.
func matchesUser(pattern, name, address string) bool {
	at := strings.LastIndex(pattern, "@")
	if at < 0 {
		return matches(pattern, name)
	}
	return matches(pattern[:at], name) && matchAddress(pattern[at+1:], address)
}

// allowedAccount applies sshd's order (DenyUsers, AllowUsers, DenyGroups,
// AllowGroups) to every entry sshd reports.
func allowedAccount(settings sshdConfig, name, address string, groups []string) bool {
	for _, pattern := range settings["denyusers"] {
		if matchesUser(pattern, name, address) {
			return false
		}
	}
	if allow := settings["allowusers"]; len(allow) > 0 {
		ok := false
		for _, pattern := range allow {
			ok = ok || matchesUser(pattern, name, address)
		}
		if !ok {
			return false
		}
	}
	for _, pattern := range settings["denygroups"] {
		for _, group := range groups {
			if matches(pattern, group) {
				return false
			}
		}
	}
	if allow := settings["allowgroups"]; len(allow) > 0 {
		ok := false
		for _, pattern := range allow {
			for _, group := range groups {
				ok = ok || matches(pattern, group)
			}
		}
		if !ok {
			return false
		}
	}
	return true
}

func authorizedPaths(u *user.User, settings sshdConfig) ([]string, error) {
	var paths []string
	for _, path := range strings.Fields(settings.get("authorizedkeysfile")) {
		if path == "none" {
			continue
		}
		path = strings.NewReplacer("%%", "\x00", "%h", u.HomeDir, "%u", u.Username, "%U", u.Uid).Replace(path)
		if strings.Contains(path, "%") {
			return nil, fmt.Errorf("unsupported AuthorizedKeysFile expansion; use connection: ssh_only")
		}
		path = strings.ReplaceAll(path, "\x00", "%")
		if !filepath.IsAbs(path) {
			path = filepath.Join(u.HomeDir, path)
		}
		paths = append(paths, filepath.Clean(path))
	}
	return paths, nil
}

// unsafeAuthorizationPath says why sshd and MMX do not trust path, a file or a
// directory on the way to the authorization file, and what to run; it returns
// "" when path is safe. Another local account that could write a path or own
// it could grant itself access. A symlink's own mode is ignored, as its target
// is checked too.
func unsafeAuthorizationPath(path string, st os.FileInfo, uid string) string {
	kind := "file"
	if st.IsDir() {
		kind = "directory"
	}
	owner := st.Sys().(*syscall.Stat_t).Uid
	switch {
	case owner != 0 && strconv.FormatUint(uint64(owner), 10) != uid:
		return fmt.Sprintf("unsafe SSH authorization path %s: this %s belongs to the account with user ID %d, which is neither root nor the administrator account, so sshd and MMX do not trust it; give it to the administrator account or to root with chown", path, kind, owner)
	case st.Mode()&os.ModeSymlink == 0 && st.Mode().Perm()&0022 != 0:
		return fmt.Sprintf("unsafe SSH authorization path %s: group or other accounts can write this %s, so sshd and MMX do not trust it; run chmod go-w %s", path, kind, path)
	}
	return ""
}

// Check owners and writable ancestors, including symlink targets, before reading
// an authorization file. Another local account must not be able to grant access.
func readKeys(path, uid string) ([]byte, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	for _, start := range []string{filepath.Clean(path), resolved} {
		for current := start; ; current = filepath.Dir(current) {
			st, err := os.Lstat(current)
			if err != nil {
				return nil, err
			}
			if reason := unsafeAuthorizationPath(current, st, uid); reason != "" {
				return nil, errors.New(reason)
			}
			if start == resolved && current == resolved && !st.Mode().IsRegular() {
				return nil, fmt.Errorf("SSH key source is not a regular file")
			}
			if current == "/" {
				break
			}
		}
	}
	return os.ReadFile(resolved)
}

// Authorize checks key for the account first, so that a client whose key the
// account does not list gets a refusedBeforeKeyProof, and only then the rest
// of the account's access.
func (a Authorizer) Authorize(ctx context.Context, name string, key ssh.PublicKey, remote, local string) error {
	u, settings, err := a.keyFiles(ctx, name, remote, local)
	if err != nil {
		return &refusedBeforeKeyProof{err}
	}
	if err := authorizeKey(u, settings, key, remote); err != nil {
		return err
	}
	return a.access(ctx, u, settings, remote)
}

// authorizeKey checks key against the account's current key files and
// policy. The key files come first: until a line lists the key, every refusal
// is a refusedBeforeKeyProof.
func authorizeKey(u *user.User, settings sshdConfig, key ssh.PublicKey, remote string) error {
	name := u.Username
	paths, err := authorizedPaths(u, settings)
	if err != nil {
		return &refusedBeforeKeyProof{err}
	}
	matched := false
	var refused []string
	for _, path := range paths {
		data, err := readKeys(path, u.Uid)
		if err != nil {
			return &refusedBeforeKeyProof{err}
		}
		ok, refusedHere, err := keyAllowed(data, key, remote)
		if err != nil {
			return &refusedBeforeKeyProof{err}
		}
		matched = matched || ok
		refused = append(refused, refusedHere...)
	}
	switch {
	case !matched && len(refused) == 0:
		return &refusedBeforeKeyProof{notAuthorizedError(name, nil)}
	case !matched:
		return notAuthorizedError(name, refused)
	}
	algorithm := key.Type()
	switch algorithm {
	case ssh.KeyAlgoED25519, ssh.KeyAlgoRSA, ssh.KeyAlgoECDSA256, ssh.KeyAlgoECDSA384, ssh.KeyAlgoECDSA521:
	default:
		return fmt.Errorf("this SSH key type requires connection: ssh_only")
	}
	if algorithm == ssh.KeyAlgoRSA {
		algorithm = ssh.KeyAlgoRSASHA256
		if public, ok := key.(ssh.CryptoPublicKey); ok {
			if rsaKey, ok := public.CryptoPublicKey().(*rsa.PublicKey); ok {
				minimum, _ := strconv.Atoi(settings.get("requiredrsasize"))
				if rsaKey.N.BitLen() < minimum {
					return fmt.Errorf("RSA key is smaller than the SSH policy allows")
				}
			}
		}
	}
	accepted := false
	for _, allowed := range strings.Split(settings.get("pubkeyacceptedalgorithms"), ",") {
		accepted = accepted || allowed == algorithm
	}
	if !accepted {
		return fmt.Errorf("key algorithm is not permitted by current SSH policy")
	}
	return nil
}

// notAuthorizedError is the refusal of a key. refused names the options of
// the key's own authorized_keys line that MMX does not accept, such as a
// forced command; with none, MMX found no line for the key.
func notAuthorizedError(name string, refused []string) error {
	if len(refused) > 0 {
		return fmt.Errorf("key is not authorized for unrestricted MMX access as %s: its line in authorized_keys has options MMX does not accept (%s); remove them, or use connection: ssh_only for other SSH authorization mechanisms", name, strings.Join(refused, ", "))
	}
	return fmt.Errorf("key is not authorized for unrestricted MMX access as %s; check current SSH keys or use connection: ssh_only for other SSH authorization mechanisms", name)
}

// keyAllowed reports whether a line of data lists wanted with options MMX
// accepts. refused names the options of the lines for wanted that MMX does not
// accept, such as command.
func keyAllowed(data []byte, wanted ssh.PublicKey, remote string) (allowed bool, refused []string, err error) {
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 8192), 64*1024)
	matched := false
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 || line[0] == '#' {
			continue
		}
		key, _, options, _, err := ssh.ParseAuthorizedKey(line)
		if err != nil {
			return false, nil, fmt.Errorf("invalid SSH authorized-key entry")
		}
		if !bytes.Equal(key.Marshal(), wanted.Marshal()) {
			continue
		}
		if allowedKeyOptions(options, remote) {
			matched = true
			continue
		}
		for _, option := range options {
			if !allowedKeyOptions([]string{option}, remote) {
				name, _, _ := strings.Cut(option, "=")
				refused = append(refused, strings.ToLower(name))
			}
		}
	}
	return matched, refused, scanner.Err()
}

func matchAddress(pattern, address string) bool {
	if _, network, err := net.ParseCIDR(pattern); err == nil {
		return network.Contains(net.ParseIP(address))
	}
	return matches(pattern, address)
}

func allowedKeyOptions(options []string, remote string) bool {
	for _, option := range options {
		key, raw, hasValue := strings.Cut(option, "=")
		key = strings.ToLower(key)
		if !hasValue {
			switch key {
			case "restrict", "no-agent-forwarding", "no-port-forwarding", "no-x11-forwarding", "no-pty", "no-user-rc", "agent-forwarding", "port-forwarding", "x11-forwarding", "pty", "user-rc":
				continue
			default:
				return false
			}
		}
		value, err := strconv.Unquote(raw)
		if err != nil {
			return false
		}
		switch key {
		case "from":
			positive := false
			for _, pattern := range strings.Split(value, ",") {
				negative := strings.HasPrefix(pattern, "!")
				pattern = strings.TrimPrefix(pattern, "!")
				if strings.Contains(pattern, "/") {
					if _, _, err := net.ParseCIDR(pattern); err != nil {
						return false
					}
				}
				if matchAddress(pattern, remote) {
					if negative {
						return false
					}
					positive = true
				}
			}
			if !positive {
				return false
			}
		default:
			return false
		}
	}
	return true
}
