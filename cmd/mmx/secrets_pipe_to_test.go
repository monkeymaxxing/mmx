package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const pipeToVault = "database_password: sentinel-database-value\ncertificate: |\n  sentinel line one\n  sentinel line two\nkey_without_final_line_ending: |-\n  sentinel line one\n  sentinel line two\nfiles:\n  ssh:\n    id_ed25519: |\n      -----BEGIN OPENSSH PRIVATE KEY-----\n      sentinel key line\n      -----END OPENSSH PRIVATE KEY-----\n"

// pipeToVaultProject makes a project whose vault is pipeToVault, dated in
// the past, and returns the vault's path and the password file.
func pipeToVaultProject(t *testing.T) (vault, passwordFile string) {
	t.Helper()
	root, passwordFile := secretsProject(t)
	vault = filepath.Join(root, "secrets_vault.age")
	quickVault(t, vault, vaultTestPassphrase, pipeToVault)
	past := time.Now().Add(-time.Hour).Truncate(time.Second)
	if err := os.Chtimes(vault, past, past); err != nil {
		t.Fatal(err)
	}
	return vault, passwordFile
}

// The program receives the stored value, with one final line ending added
// only when the value has none, and nothing else. MMX prints nothing, and
// the vault file keeps its bytes and its time.
func TestSecretsPipeToWritesTheValueToTheProgramsStandardInput(t *testing.T) {
	vault, passwordFile := pipeToVaultProject(t)
	before, _ := os.ReadFile(vault)
	beforeInfo, _ := os.Stat(vault)
	for name, tc := range map[string]struct {
		keyPath []string
		want    string
	}{
		"a value of one line gets a line ending":            {[]string{"database_password"}, "sentinel-database-value\n"},
		"a text block keeps its line ending":                {[]string{"certificate"}, "sentinel line one\nsentinel line two\n"},
		"a text block without a final line ending gets one": {[]string{"key_without_final_line_ending"}, "sentinel line one\nsentinel line two\n"},
		"a private key at depth 3, byte for byte": {[]string{"files", "ssh", "id_ed25519"},
			"-----BEGIN OPENSSH PRIVATE KEY-----\nsentinel key line\n-----END OPENSSH PRIVATE KEY-----\n"},
	} {
		t.Run(name, func(t *testing.T) {
			received := filepath.Join(t.TempDir(), "received")
			args := append(append([]string{"pipe-to"}, tc.keyPath...), "--vault-password-file", passwordFile, "--", "sh", "-c", `cat > "$0"`, received)
			stdout, stderr, code := secretsOutput(t, args...)
			if code != 0 || stdout != "" || stderr != "" {
				t.Fatalf("exit code %d\n%s%s", code, stdout, stderr)
			}
			if got, _ := os.ReadFile(received); string(got) != tc.want {
				t.Fatalf("the program received %q", got)
			}
		})
	}
	after, _ := os.ReadFile(vault)
	afterInfo, _ := os.Stat(vault)
	if !bytes.Equal(after, before) || !afterInfo.ModTime().Equal(beforeInfo.ModTime()) {
		t.Fatal("mmx secrets pipe-to changed the vault file")
	}
}

// The program's output passes through, and MMX exits with its exit code.
func TestSecretsPipeToPassesTheProgramsOutputAndExitCodeThrough(t *testing.T) {
	_, passwordFile := pipeToVaultProject(t)
	stdout, stderr, code := secretsOutput(t, "pipe-to", "database_password", "--vault-password-file", passwordFile, "--", "sh", "-c", `read -r line; echo "read ${#line} characters"; echo "a warning" >&2; exit 7`)
	if code != 7 || stdout != "read 23 characters\n" || stderr != "a warning\n" {
		t.Fatalf("exit code %d\n%s%s", code, stdout, stderr)
	}
}

func TestSecretsPipeToRefusalsSayWhatToDo(t *testing.T) {
	vault, passwordFile := pipeToVaultProject(t)
	before, _ := os.ReadFile(vault)
	const usage = "mmx: usage: mmx secrets pipe-to KEY [NAME ...] -- COMMAND [ARGUMENT ...]; MMX writes the value of the secret to COMMAND's standard input, such as mmx secrets pipe-to backup_passphrase -- openssl"
	for name, tc := range map[string]struct {
		args []string
		want string
	}{
		"no --":             {[]string{"pipe-to", "database_password", "--vault-password-file", passwordFile, "sh"}, usage},
		"no COMMAND":        {[]string{"pipe-to", "database_password", "--vault-password-file", passwordFile, "--"}, usage},
		"no key":            {[]string{"pipe-to", "--vault-password-file", passwordFile, "--", "sh"}, usage},
		"a missing program": {[]string{"pipe-to", "database_password", "--vault-password-file", passwordFile, "--", "mmx-test-no-such-program"}, "mmx: cannot start mmx-test-no-such-program: no such program; check the program's name, or the PATH environment variable\n"},
		"a missing key":     {[]string{"pipe-to", "api_token", "--vault-password-file", passwordFile, "--", "sh", "-c", "exit 0"}, "mmx: the vault has no secret key api_token; mmx secrets list-keys lists the secret keys\n"},
		"a path that ends at names": {[]string{"pipe-to", "files", "ssh", "--vault-password-file", passwordFile, "--", "sh", "-c", "exit 0"},
			"mmx: the vault's files ssh holds names, not one value: id_ed25519. Give one more word for the name to read, such as files ssh id_ed25519\n"},
		"--json": {[]string{"pipe-to", "database_password", "--json", "--vault-password-file", passwordFile, "--", "sh"}, "mmx: mmx secrets pipe-to has no --json: the output is the output of sh\n"},
	} {
		t.Run(name, func(t *testing.T) {
			stdout, stderr, code := secretsOutput(t, tc.args...)
			if code != 4 || stdout != "" || !strings.HasPrefix(stderr, tc.want) {
				t.Fatalf("exit code %d\n%s%s", code, stdout, stderr)
			}
		})
	}
	if after, _ := os.ReadFile(vault); !bytes.Equal(after, before) {
		t.Fatal("a refused command changed the vault file")
	}
}
