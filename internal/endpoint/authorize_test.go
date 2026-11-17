package endpoint

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

func TestAuthorizedKeysKeepRestrictions(t *testing.T) {
	key := testSigner(t).PublicKey()
	text := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key)))
	for _, tt := range []struct {
		options string
		allow   bool
	}{
		{"", true}, {"restrict", true}, {"no-pty,no-agent-forwarding", true},
		{`command="/usr/bin/true"`, false}, {"cert-authority", false},
		{`from="127.0.0.0/8,!127.0.0.2"`, true}, {`from="!127.0.0.1,*"`, false},
		{`from="192.0.2.0/24"`, false}, {`expiry-time="20000101Z"`, false},
		{`from="*,!127.0.0.1/33"`, false},
		{`expiry-time="29991231Z"`, false}, {`unknown-option="yes"`, false},
		{`permitopen="invalid"`, false},
	} {
		t.Run(tt.options, func(t *testing.T) {
			allowed, _, err := keyAllowed([]byte("# admin\n\n"+tt.options+" "+text+"\n"), key, "127.0.0.1")
			if err != nil || allowed != tt.allow {
				t.Fatal(allowed, err)
			}
		})
	}
	if ok, _, err := keyAllowed([]byte(text+"\ninvalid key entry\n"), key, "127.0.0.1"); err == nil || ok {
		t.Fatal("invalid authorization source accepted")
	}
	if ok, _, err := keyAllowed([]byte(text), testSigner(t).PublicKey(), "127.0.0.1"); err != nil || ok {
		t.Fatal("unlisted key accepted")
	}
}

// A key whose authorized_keys line has a forced command is refused, and the
// refusal names the option so the reader knows to look at that line.
func TestRefusedKeyNamesTheOptionsOfItsAuthorizedKeysLine(t *testing.T) {
	key := testSigner(t).PublicKey()
	text := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key)))
	allowed, refused, err := keyAllowed([]byte(`no-pty,command="/usr/bin/true",from="192.0.2.0/24" `+text+"\n"), key, "127.0.0.1")
	if err != nil || allowed {
		t.Fatal(allowed, err)
	}
	got := notAuthorizedError("admin", refused).Error()
	want := `key is not authorized for unrestricted MMX access as admin: its line in authorized_keys has options MMX does not accept (command, from); remove them, or use connection: ssh_only for other SSH authorization mechanisms`
	if got != want {
		t.Fatal(got)
	}
	if got := notAuthorizedError("admin", nil).Error(); got != `key is not authorized for unrestricted MMX access as admin; check current SSH keys or use connection: ssh_only for other SSH authorization mechanisms` {
		t.Fatal(got)
	}
}

func TestAccountExpiryAndRestrictedShell(t *testing.T) {
	now := time.Unix(20000*86400, 0)
	for _, tt := range []struct {
		shadow string
		allow  bool
	}{
		{"admin:!:19000:0:99999:7:::", true},
		{"admin:!:19000:0:99999:7::20001:", true},
		{"admin:!:19000:0:99999:7::20000:", false},
		{"admin:!:19000:0:99999:7::0:", false},
		{"admin:!:19000:0:99999:7::invalid:", false},
	} {
		if accountCurrent(tt.shadow, "admin", now) != tt.allow {
			t.Fatal(tt)
		}
	}
	if !ordinaryShell("/bin/bash") || ordinaryShell("/bin/rbash") || ordinaryShell("/usr/sbin/nologin") {
		t.Fatal("restricted shell admitted")
	}
}

func TestOnlyUnrestrictedSudoIsAdministrator(t *testing.T) {
	for _, tt := range []struct {
		entry string
		allow bool
	}{
		{"RunAsUsers: ALL\nOptions: !authenticate\nCommands:\n ALL", true},
		{"RunAsUsers: root\nOptions: authenticate\nCommands:\n ALL", true},
		{"RunAsUsers: app\nCommands:\n ALL", false},
		{"RunAsUsers: ALL\nCommands:\n /bin/systemctl restart app", false},
		{"RunAsUsers: root\nCommands:\n ALL\n !/bin/sh", false},
		{"RunAsUsers: ALL, !root\nCommands:\n ALL", false},
		{"RunAsUsers: root\nOptions: noexec\nCommands:\n ALL", false},
		{"RunAsUsers: ALL\nCommands:\n ALL\nSudoers entry:\nRunAsUsers: root\nCommands:\n !/bin/sh", false},
	} {
		if unrestrictedSudo("Sudoers entry:\n"+tt.entry) != tt.allow {
			t.Fatal(tt)
		}
	}
	// Real `sudo -ll -U dev` output from Rocky Linux 9, and the same output
	// with a command-specific Defaults entry: the Defaults sections contain
	// "!" and grant nothing.
	rocky := "Matching Defaults entries for dev on lima-rocky9:\n    !visiblepw, always_set_home, match_group_by_gid, env_reset\n\nUser dev may run the following commands on lima-rocky9:\n\n"
	commandSpecific := "Matching Defaults entries for dev on node1:\n    secure_path=/usr/local/sbin\\:/usr/local/bin\\:/usr/bin\n\nRunas and Command-specific defaults for dev:\n    Defaults!/usr/bin/visudo env_keep+=\"SUDO_EDITOR EDITOR VISUAL\"\n\nUser dev may run the following commands on node1:\n\n"
	full := "Sudoers entry: /etc/sudoers.d/90-cloud-init-users\n    RunAsUsers: ALL\n    Options: !authenticate\n    Commands:\n\tALL\n"
	narrowed := "Sudoers entry: /etc/sudoers\n    RunAsUsers: ALL\n    Commands:\n\tALL\n\t!/bin/sh\n"
	for _, defaults := range []string{rocky, commandSpecific} {
		if !unrestrictedSudo(defaults+full) || unrestrictedSudo(defaults+narrowed) || unrestrictedSudo(defaults) {
			t.Fatal("stock sudo defaults changed the decision", defaults)
		}
	}
	settings := sshdConfig{"allowusers": {"deploy@127.0.0.0/8"}, "denygroups": {"blocked"}}
	if !allowedAccount(settings, "deploy", "127.0.0.1", []string{"sudo"}) || allowedAccount(settings, "deploy", "192.0.2.1", []string{"sudo"}) || allowedAccount(settings, "deploy", "127.0.0.1", []string{"blocked"}) {
		t.Fatal("SSH account restrictions lost")
	}
}

// sshdLists is an excerpt of real `sshd -T` output (OpenSSH 10.3) for:
//
//	DenyUsers alice bob@10.0.0.0/8
//	AllowUsers carol dave
//	AllowGroups sudo wheel
//	DenyGroups blocked
//
// sshd prints one line per list entry.
const sshdLists = `permitrootlogin prohibit-password
pubkeyauthentication yes
usedns no
refuseconnection no
forcecommand none
chrootdirectory none
authorizedkeyscommand none
revokedkeys none
authorizedkeysfile .ssh/authorized_keys .ssh/authorized_keys2
allowusers carol
allowusers dave
denyusers alice
denyusers bob@10.0.0.0/8
allowgroups sudo
allowgroups wheel
denygroups blocked
authenticationmethods any
`

func TestSSHListsApplyEveryEntry(t *testing.T) {
	settings := parseSSHD(sshdLists)
	if err := sshdPolicy(settings, "carol", false); err != nil {
		t.Fatal(err)
	}
	// OpenSSH 10.5 prints the keys with capital letters and leaves out an
	// option that is not set, such as RevokedKeys.
	capitals := "PermitRootLogin prohibit-password\nPubkeyAuthentication yes\nUseDNS no\nRefuseConnection no\nForceCommand none\nChrootDirectory none\nAuthorizedKeysFile .ssh/authorized_keys\nAuthenticationMethods any\nAllowUsers carol\n"
	settings = parseSSHD(capitals)
	if err := sshdPolicy(settings, "root", true); err != nil || !allowedAccount(settings, "carol", "10.0.0.1", nil) || allowedAccount(settings, "dave", "10.0.0.1", nil) {
		t.Fatal("OpenSSH 10.5 sshd -T output was not understood", err, settings)
	}
	for _, restriction := range []string{"RevokedKeys /etc/ssh/revoked\n", "ForceCommand /bin/false\n", "UseDNS yes\n"} {
		if sshdPolicy(parseSSHD(strings.Replace(capitals, "UseDNS no\n", "", 1)+restriction), "root", true) == nil {
			t.Fatal("a restriction was ignored:", restriction)
		}
	}
	settings = parseSSHD(sshdLists)
	for _, tt := range []struct {
		name, address string
		groups        []string
		allow         bool
	}{
		{"carol", "10.0.0.1", []string{"sudo"}, true},
		{"dave", "10.0.0.1", []string{"staff", "wheel"}, true},
		{"alice", "10.0.0.1", []string{"sudo"}, false},
		{"carol", "10.0.0.1", []string{"sudo", "blocked"}, false},
		{"carol", "10.0.0.1", []string{"staff"}, false},
		{"erin", "10.0.0.1", []string{"sudo"}, false},
	} {
		if allowedAccount(settings, tt.name, tt.address, tt.groups) != tt.allow {
			t.Fatal(tt)
		}
	}
	// A deny entry's host part applies only to matching addresses.
	settings["allowusers"] = append(settings["allowusers"], "bob")
	if allowedAccount(settings, "bob", "10.0.0.1", []string{"sudo"}) || !allowedAccount(settings, "bob", "192.0.2.1", []string{"sudo"}) {
		t.Fatal("DenyUsers host restriction lost")
	}
	// Every entry, not only the last, is checked for patterns sshd evaluates
	// differently from this endpoint.
	for _, field := range []string{"allowusers", "denyusers", "allowgroups", "denygroups"} {
		for _, pattern := range []string{"!mallory", "carol@10.0.0.1,10.0.0.2"} {
			complex := parseSSHD(sshdLists)
			complex[field] = append([]string{pattern}, complex[field]...)
			if err := sshdPolicy(complex, "carol", false); err == nil || !strings.Contains(err.Error(), "connection: ssh_only") {
				t.Fatal(field, pattern, err)
			}
		}
	}
}

// The installed sshd reports list options one entry per line.
func TestSSHDReportsEveryListEntry(t *testing.T) {
	if _, err := exec.LookPath("ssh-keygen"); err != nil {
		t.Skip("ssh-keygen unavailable", err)
	}
	if _, err := os.Stat("/usr/sbin/sshd"); os.IsNotExist(err) {
		t.Skip("sshd unavailable", err)
	} else if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	key := filepath.Join(dir, "host_key")
	if out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", key).CombinedOutput(); err != nil {
		t.Fatal("generate temporary host key", err, string(out))
	}
	config := filepath.Join(dir, "sshd_config")
	if err := os.WriteFile(config, []byte("HostKey "+key+"\nDenyUsers alice bob\nAllowUsers carol dave\nMatch Host 10.0.0.1\n PasswordAuthentication no\n"), 0600); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("/usr/sbin/sshd", "-T", "-f", config, "-C", sshdConnectionSpec("x", "10.0.0.1", "10.0.0.2", 22)).CombinedOutput()
	if err != nil {
		t.Fatal("sshd rejected the connection specification", err, string(out))
	}
	settings := parseSSHD(string(out))
	if settings.get("passwordauthentication") != "no" {
		t.Fatal("sshd did not apply the matching host policy", settings)
	}
	if allowedAccount(settings, "alice", "10.0.0.1", nil) || !allowedAccount(settings, "carol", "10.0.0.1", nil) {
		t.Fatalf("denyusers %q, allowusers %q", settings["denyusers"], settings["allowusers"])
	}
}

func TestSSHPatternsDoNotInterpretShellCharacterClasses(t *testing.T) {
	if !matches("deploy*", "deploy-web") || !matches("web?", "web1") || matches("web[12]", "web1") {
		t.Fatal("incorrect SSH wildcard matching")
	}
}

// With no allow lists, a user or group matched only by an earlier deny line is
// still denied: sshd reads every line, so HTTPS must too.
func TestEarlierDenyEntriesDenyWithoutAllowLists(t *testing.T) {
	settings := parseSSHD("denyusers alice\ndenyusers bob\ndenygroups blocked\ndenygroups other\n")
	for _, tt := range []struct {
		name   string
		groups []string
		allow  bool
	}{
		{"alice", []string{"sudo"}, false},
		{"bob", []string{"sudo"}, false},
		{"carol", []string{"sudo", "blocked"}, false},
		{"carol", []string{"other"}, false},
		{"carol", []string{"sudo"}, true},
	} {
		if allowedAccount(settings, tt.name, "10.0.0.1", tt.groups) != tt.allow {
			t.Fatal(tt)
		}
	}
}

// A refused SSH authorization path says what is unsafe, why, and what to run,
// and names the file or the directory that is the problem.
func TestUnsafeAuthorizationPathSaysWhatIsUnsafeAndTheFix(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "authorized_keys")
	if err := os.WriteFile(file, nil, 0600); err != nil {
		t.Fatal(err)
	}
	uid := strconv.Itoa(os.Getuid())
	check := func(path string, uid string) string {
		st, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		return unsafeAuthorizationPath(path, st, uid)
	}
	if got := check(file, uid); got != "" {
		t.Fatalf("a private file is safe: %q", got)
	}
	for _, tt := range []struct{ path, mode, want string }{
		{file, "664", "unsafe SSH authorization path " + file + ": group or other accounts can write this file, so sshd and MMX do not trust it; run chmod go-w " + file},
		{dir, "775", "unsafe SSH authorization path " + dir + ": group or other accounts can write this directory, so sshd and MMX do not trust it; run chmod go-w " + dir},
	} {
		mode, _ := strconv.ParseUint(tt.mode, 8, 32)
		if err := os.Chmod(tt.path, os.FileMode(mode)); err != nil {
			t.Fatal(err)
		}
		if got := check(tt.path, uid); got != tt.want {
			t.Fatalf("got %q, want %q", got, tt.want)
		}
	}
	if os.Getuid() != 0 {
		// A file owned by this account is not safe for another administrator account.
		got := check(file, "0")
		if !strings.Contains(got, "belongs to the account with user ID "+uid) || !strings.Contains(got, "neither root nor the administrator account") || !strings.Contains(got, "chown") {
			t.Fatalf("owner message %q", got)
		}
	}
}

// Before the key is proved, a refusal is one that the client must not read:
// an unknown account, or a key the account does not list.
func TestRefusalsBeforeTheKeyIsProvedAreMarked(t *testing.T) {
	key := testSigner(t).PublicKey()
	var before *refusedBeforeKeyProof
	if err := (Authorizer{}).Authorize(context.Background(), "no_such_mmx_account", key, "127.0.0.1", "127.0.0.1"); !errors.As(err, &before) {
		t.Fatal(err)
	}
	dir := t.TempDir()
	file := filepath.Join(dir, "authorized_keys")
	other := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(testSigner(t).PublicKey())))
	listed := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key)))
	account := &user.User{Username: "admin", Uid: strconv.Itoa(os.Getuid()), HomeDir: dir}
	settings := sshdConfig{"authorizedkeysfile": {file}, "pubkeyacceptedalgorithms": {"ssh-ed25519"}}
	for _, tt := range []struct {
		line   string
		before bool
	}{
		{other, true},
		// The key's own line proves the key: its options are named.
		{`command="/usr/bin/true" ` + listed, false},
	} {
		if err := os.WriteFile(file, []byte(tt.line+"\n"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(dir, 0700); err != nil {
			t.Fatal(err)
		}
		err := authorizeKey(account, settings, key, "127.0.0.1")
		if err == nil || errors.As(err, &before) != tt.before {
			t.Fatal(tt.line, err)
		}
	}
}

// sshd built without PAM, or with UsePAM no, refuses an account whose
// password is locked, so the node HTTPS API does too. With UsePAM yes, sshd
// leaves the account check to PAM, which accepts a key for a locked password.
func TestLockedPasswordFollowsUsePAM(t *testing.T) {
	for _, tt := range []struct {
		shadow, usePAM string
		locked         bool
	}{
		{"admin:!$6$salt$hash:19000:0:99999:7:::", "no", true},
		{"admin:!:19000:0:99999:7:::", "", true},
		{"admin:!:19000:0:99999:7:::", "yes", false},
		{"admin:*:19000:0:99999:7:::", "no", false},
		{"admin:$6$salt$hash:19000:0:99999:7:::", "no", false},
	} {
		settings := sshdConfig{}
		if tt.usePAM != "" {
			settings["usepam"] = []string{tt.usePAM}
		}
		if lockedForSSHD(tt.shadow, settings) != tt.locked {
			t.Fatal(tt)
		}
	}
}
