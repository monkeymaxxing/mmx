package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// secretsOutputAtTerminal runs one mmx secrets command whose standard
// output is a terminal, as when a person runs it, and returns what the
// terminal shows, standard error with the error that main prints there,
// and the exit code.
func secretsOutputAtTerminal(t *testing.T, args ...string) (shown, stderr string, code int) {
	t.Helper()
	controller, terminal := openPseudoTerminal(t)
	defer controller.Close()
	read := make(chan []byte)
	go func() {
		b, _ := io.ReadAll(controller)
		read <- b
	}()
	errors, err := os.CreateTemp(t.TempDir(), "stderr")
	if err != nil {
		t.Fatal(err)
	}
	savedStdout, savedStderr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = terminal, errors
	code, err = run(context.Background(), append([]string{"secrets"}, args...))
	os.Stdout, os.Stderr = savedStdout, savedStderr
	terminal.Close()
	errors.Close()
	b, _ := os.ReadFile(errors.Name())
	stderr = string(b)
	if err != nil {
		stderr += "mmx: " + err.Error() + "\n"
	}
	// A terminal shows each line ending as a carriage return and a line feed.
	return strings.ReplaceAll(string(<-read), "\r\n", "\n"), stderr, code
}

const showVault = "database_password: sentinel-database-value\nfiles:\n  user: sentinel-nested-value\n  tls:\n    ca.pem: |\n      sentinel line one\n      sentinel line two\n"

// show prints to a terminal only: in a test, as for a coding agent, standard
// output is a file, so the command refuses before it opens the vault.
func TestSecretsShowRefusesWithoutATerminal(t *testing.T) {
	root, passwordFile := secretsProject(t)
	quickVault(t, filepath.Join(root, "secrets_vault.age"), vaultTestPassphrase, showVault)
	const refusal = "mmx: mmx secrets show prints a secret only to a terminal. Run it yourself in a terminal, not through your coding agent\n"
	stdout, stderr, code := secretsOutput(t, "show", "database_password", "--vault-password-file", passwordFile)
	if code != 4 || stdout != "" || stderr != refusal {
		t.Fatalf("exit code %d\n%s%s", code, stdout, stderr)
	}
	// --json is refused even at a terminal.
	shown, stderr, code := secretsOutputAtTerminal(t, "show", "database_password", "--json", "--vault-password-file", passwordFile)
	if code != 4 || shown != "" || stderr != refusal {
		t.Fatalf("exit code %d\n%s%s", code, shown, stderr)
	}
}

func TestSecretsShowPrintsOneValueAtATerminal(t *testing.T) {
	root, passwordFile := secretsProject(t)
	quickVault(t, filepath.Join(root, "secrets_vault.age"), vaultTestPassphrase, showVault)
	for name, tc := range map[string]struct {
		args []string
		want string
	}{
		"a top-level key": {[]string{"database_password"}, "sentinel-database-value\nClear your terminal afterwards with `clear`.\n"},
		"a nested name":   {[]string{"files", "user"}, "sentinel-nested-value\nClear your terminal afterwards with `clear`.\n"},
		"a text block at depth 3, with its own final line ending": {[]string{"files", "tls", "ca.pem"},
			"sentinel line one\nsentinel line two\nClear your terminal afterwards with `clear`.\n"},
	} {
		t.Run(name, func(t *testing.T) {
			shown, stderr, code := secretsOutputAtTerminal(t, append(append([]string{"show"}, tc.args...), "--vault-password-file", passwordFile)...)
			if code != 0 || stderr != "" || shown != tc.want {
				t.Fatalf("exit code %d\n%q\n%s", code, shown, stderr)
			}
		})
	}
}

// A path that does not end at one value says what the vault has there, so
// the person can pick the right words, and shows no value.
func TestSecretsShowNamesWhatTheVaultHasInstead(t *testing.T) {
	root, passwordFile := secretsProject(t)
	quickVault(t, filepath.Join(root, "secrets_vault.age"), vaultTestPassphrase, showVault)
	for name, tc := range map[string]struct {
		args []string
		want string
	}{
		"a missing key":             {[]string{"api_token"}, "mmx: the vault has no secret key api_token; mmx secrets list-keys lists the secret keys\n"},
		"a missing name":            {[]string{"files", "password"}, "mmx: the vault's files has no name password; its names are tls, user\n"},
		"a path that ends at names": {[]string{"files"}, "mmx: the vault's files holds names, not one value: tls, user. Give one more word for the name to read, such as files tls\n"},
		"a name inside a value":     {[]string{"database_password", "user"}, "mmx: the vault's database_password holds a value, not names; to read the value, give only database_password\n"},
		"no key":                    {nil, "mmx: usage: mmx secrets show KEY [NAME ...]"},
	} {
		t.Run(name, func(t *testing.T) {
			shown, stderr, code := secretsOutputAtTerminal(t, append(append([]string{"show"}, tc.args...), "--vault-password-file", passwordFile)...)
			if code != 4 || shown != "" || !strings.HasPrefix(stderr, tc.want) || strings.Contains(stderr, "sentinel") {
				t.Fatalf("exit code %d\n%q\n%s", code, shown, stderr)
			}
		})
	}
}

// A run reads a key that secrets_from_env binds from its environment
// variable, so the vault's value for it is not the one a run uses.
func TestSecretsShowRefusesAKeyThatTheProjectReadsFromTheEnvironment(t *testing.T) {
	root, passwordFile := secretsProject(t)
	quickVault(t, filepath.Join(root, "secrets_vault.age"), vaultTestPassphrase, showVault)
	os.WriteFile(filepath.Join(root, "mmx_project_settings.yml"), []byte("project_name: shop\ndefault_inventory: production\nsecrets_from_env:\n  database_password: SHOP_DATABASE_PASSWORD\n"), 0644)
	shown, stderr, code := secretsOutputAtTerminal(t, "show", "database_password", "--vault-password-file", passwordFile)
	if code != 4 || shown != "" || !strings.HasPrefix(stderr, "mmx: secret key database_password is bound to environment variable SHOP_DATABASE_PASSWORD by secrets_from_env") {
		t.Fatalf("exit code %d\n%q\n%s", code, shown, stderr)
	}
}
