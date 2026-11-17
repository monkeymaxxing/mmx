package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"mmx/internal/secrets"
)

// secretsOutputFromInput runs one mmx secrets command with input on its
// standard input, as some_command | mmx secrets ... gives it, and returns
// what secretsOutput returns. A file is no terminal, so nothing prompts.
func secretsOutputFromInput(t *testing.T, input string, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	file := filepath.Join(t.TempDir(), "input")
	if err := os.WriteFile(file, []byte(input), 0600); err != nil {
		t.Fatal(err)
	}
	stdin, err := os.Open(file)
	if err != nil {
		t.Fatal(err)
	}
	defer stdin.Close()
	saved := os.Stdin
	os.Stdin = stdin
	defer func() { os.Stdin = saved }()
	return secretsOutput(t, args...)
}

func TestSecretsSetTakesTheValueFromStandardInputAndNamesWhatItSaved(t *testing.T) {
	root, passwordFile := secretsProject(t)
	// A project that has no vault yet gets one: here the vault that the
	// selected inventory names.
	stagingVault := filepath.Join(root, "inventories/staging_vault.age")
	stdout, stderr, code := secretsOutputFromInput(t, "sentinel-token-value\n", "set", "cloudflare_tunnel_token", "--inventory", "staging", "--vault-password-file", passwordFile)
	if code != 0 || !strings.HasPrefix(stdout, "Saved the key ") || strings.Contains(stdout, "sentinel") || stderr != "" {
		t.Fatalf("exit code %d\n%s%s", code, stdout, stderr)
	}
	if want := "Saved the key cloudflare_tunnel_token in the vault inventories/staging_vault.age. MMX does not show the value.\n"; stdout != want {
		t.Fatalf("stdout %q, want %q", stdout, want)
	}
	values, err := secrets.Read(stagingVault, vaultTestPassphrase)
	if err != nil || !reflect.DeepEqual(values, map[string]any{"cloudflare_tunnel_token": "sentinel-token-value"}) {
		t.Fatal(len(values), err)
	}
	if info, err := os.Stat(stagingVault); err != nil || info.Mode().Perm() != 0600 {
		t.Fatal(info, err)
	}
	if _, err := os.Stat(filepath.Join(root, "secrets_vault.age")); !os.IsNotExist(err) {
		t.Fatal("the project's vault was written for another inventory", err)
	}

	// Exactly one final line ending goes: echo and printf %s give the same
	// value, and a second line ending stays part of the value.
	vault := filepath.Join(root, "secrets_vault.age")
	quickVault(t, vault, vaultTestPassphrase, "# The shop\ndatabase_password: sentinel-database-value\n")
	for key, input := range map[string]string{
		"from_echo":                  "sentinel-token-value\n",
		"from_printf":                "sentinel-token-value",
		"from_a_windows_line":        "sentinel-token-value\r\n",
		"with_a_second_line_ending":  "sentinel-token-value\n\n",
		"with_a_carriage_return_end": "sentinel-token-value\r",
	} {
		if stdout, stderr, code := secretsOutputFromInput(t, input, "set", key, "--vault-password-file", passwordFile); code != 0 || !strings.HasPrefix(stdout, "Saved the key ") || strings.Contains(stdout, "sentinel") || stderr != "" {
			t.Fatalf("%s: exit code %d\n%s%s", key, code, stdout, stderr)
		}
	}
	values, err = secrets.Read(vault, vaultTestPassphrase)
	want := map[string]any{"database_password": "sentinel-database-value", "from_echo": "sentinel-token-value", "from_printf": "sentinel-token-value", "from_a_windows_line": "sentinel-token-value",
		"with_a_second_line_ending": "sentinel-token-value\n", "with_a_carriage_return_end": "sentinel-token-value\r"}
	if err != nil || !reflect.DeepEqual(values, want) {
		t.Fatal(len(values), err)
	}

	// JSON prints only the vault's path and the status.
	stdout, stderr, code = secretsOutputFromInput(t, "sentinel-nested-value", "set", "api_token", "--json", "--vault-password-file", passwordFile)
	var answer map[string]any
	if code != 0 || stderr != "" || json.Unmarshal([]byte(stdout), &answer) != nil || !reflect.DeepEqual(answer, map[string]any{"vault": vault, "status": "succeeded"}) {
		t.Fatalf("exit code %d\n%s%s", code, stdout, stderr)
	}
}

func TestSecretsSetReplacesAValueOnlyWithReplace(t *testing.T) {
	root, passwordFile := secretsProject(t)
	vault := filepath.Join(root, "secrets_vault.age")
	quickVault(t, vault, vaultTestPassphrase, "database_password: sentinel-database-value\napi_token: sentinel-token-value\n")
	original, _ := os.ReadFile(vault)
	stdout, stderr, code := secretsOutputFromInput(t, "sentinel-changed-value\n", "set", "database_password", "--vault-password-file", passwordFile)
	if code != 4 || stdout != "" || stderr != "mmx: the vault already has the key database_password; to replace its value, add --replace\n" {
		t.Fatalf("exit code %d\n%s%s", code, stdout, stderr)
	}
	if now, _ := os.ReadFile(vault); !bytes.Equal(now, original) {
		t.Fatal("the refused command changed the vault")
	}
	// --replace changes the value and keeps the other key; with a key the
	// vault does not have, it adds the key.
	if stdout, stderr, code := secretsOutputFromInput(t, "sentinel-changed-value\n", "set", "--replace", "database_password", "--vault-password-file", passwordFile); code != 0 || !strings.HasPrefix(stdout, "Saved the key ") || strings.Contains(stdout, "sentinel") || stderr != "" {
		t.Fatalf("exit code %d\n%s%s", code, stdout, stderr)
	}
	if stdout, stderr, code := secretsOutputFromInput(t, "sentinel-nested-value\n", "set", "session_cookie_secret", "--replace", "--vault-password-file", passwordFile); code != 0 || !strings.HasPrefix(stdout, "Saved the key ") || strings.Contains(stdout, "sentinel") || stderr != "" {
		t.Fatalf("exit code %d\n%s%s", code, stdout, stderr)
	}
	values, err := secrets.Read(vault, vaultTestPassphrase)
	if err != nil || !reflect.DeepEqual(values, map[string]any{"database_password": "sentinel-changed-value", "api_token": "sentinel-token-value", "session_cookie_secret": "sentinel-nested-value"}) {
		t.Fatal(len(values), err)
	}
}

func TestSecretsSetRefusalsKeepTheVaultAndShowNoValue(t *testing.T) {
	root, passwordFile := secretsProject(t)
	vault := filepath.Join(root, "secrets_vault.age")
	quickVault(t, vault, vaultTestPassphrase, "database_password: sentinel-database-value\n")
	original, _ := os.ReadFile(vault)
	wrong := filepath.Join(t.TempDir(), "wrong")
	os.WriteFile(wrong, []byte("another passphrase\n"), 0600)
	for name, tc := range map[string]struct {
		input string
		args  []string
		want  string
	}{
		"an empty value":                   {"", []string{"set", "api_token", "--vault-password-file", passwordFile}, "mmx: the value for api_token is empty; mmx secrets set needs a value on standard input, such as some_command | mmx secrets set api_token\n"},
		"only a line ending":               {"\n", []string{"set", "api_token", "--vault-password-file", passwordFile}, "mmx: the value for api_token is empty"},
		"a value that is not UTF-8 text":   {"sentinel-token-value\xff\n", []string{"set", "api_token", "--vault-password-file", passwordFile}, "mmx: the value for api_token is not UTF-8 text, and the vault holds text; encode other data first, for example with base64\n"},
		"no key":                           {"sentinel-token-value", []string{"set", "--vault-password-file", passwordFile}, "usage: some_command | mmx secrets set KEY [NAME ...] [--replace]"},
		"a name inside a key with a value": {"sentinel-token-value", []string{"set", "database_password", "smtp-password", "--vault-password-file", passwordFile}, "mmx: the vault's database_password holds a value, not names, so smtp-password cannot go inside it. To keep the value, choose another name; to replace the value with names, run mmx secrets edit and remove the value of database_password\n"},
		"a name that YAML reads":           {"sentinel-token-value", []string{"set", "web_app", "null", "--vault-password-file", passwordFile}, "the name null cannot be added: YAML reads null as a value instead of a name; choose a different name"},
		"a key that is no name":            {"sentinel-token-value", []string{"set", "9lives", "--vault-password-file", passwordFile}, `"9lives" cannot be a secret key`},
		"a wrong passphrase":               {"sentinel-token-value", []string{"set", "api_token", "--vault-password-file", wrong}, "cannot decrypt vault: wrong passphrase or invalid age file"},
		"no passphrase":                    {"sentinel-token-value", []string{"set", "api_token", "--non-interactive"}, "vault passphrase required; use --vault-password-file"},
		"an existing key, wrong password":  {"sentinel-token-value", []string{"set", "database_password", "--replace", "--vault-password-file", wrong}, "cannot decrypt vault"},
		"the option of add-random":         {"sentinel-token-value", []string{"set", "api_token", "--length", "5", "--vault-password-file", passwordFile}, "flag provided but not defined: -length"},
	} {
		t.Run(name, func(t *testing.T) {
			stdout, stderr, code := secretsOutputFromInput(t, tc.input, tc.args...)
			if code != 4 || stdout != "" || !strings.Contains(stderr, tc.want) {
				t.Fatalf("exit code %d\n%s%s", code, stdout, stderr)
			}
			if now, _ := os.ReadFile(vault); !bytes.Equal(now, original) {
				t.Fatal("the refused command changed the vault")
			}
		})
	}
	if entries, _ := os.ReadDir(root); len(entries) != 3 {
		t.Fatalf("a refused command left a file in the project: %v", entries)
	}
}

// A vault that holds the inventory's list by mistake, as
// mmwatchdog_secret_files: [], refuses a name inside it and says what to do.
func TestSecretsSetRefusesANameInsideAList(t *testing.T) {
	root, passwordFile := secretsProject(t)
	vault := filepath.Join(root, "secrets_vault.age")
	quickVault(t, vault, vaultTestPassphrase, "mmwatchdog_secret_files: []\n")
	original, _ := os.ReadFile(vault)
	stdout, stderr, code := secretsOutputFromInput(t, "sentinel-token-value\n", "set", "mmwatchdog_secret_files", "smtp-password", "--vault-password-file", passwordFile)
	if code != 4 || stdout != "" || stderr != "mmx: the vault's mmwatchdog_secret_files holds a list, not names, so smtp-password cannot go inside it. To keep the list, choose another secret key; to give mmwatchdog_secret_files names instead, run mmx secrets edit and replace the list with the names, one per line below mmwatchdog_secret_files, each indented by two spaces\n" {
		t.Fatalf("exit code %d\n%s%s", code, stdout, stderr)
	}
	if now, _ := os.ReadFile(vault); !bytes.Equal(now, original) {
		t.Fatal("the refused command changed the vault")
	}
}

// A run reads a key that secrets_from_env binds from the environment
// variable, so a value for it in the vault would never be read. The refusal
// comes before the vault is unlocked: it needs no passphrase.
func TestSecretsSetRefusesAKeyThatTheProjectReadsFromTheEnvironment(t *testing.T) {
	root, _ := secretsProject(t)
	os.WriteFile(filepath.Join(root, "mmx_project_settings.yml"), []byte("project_name: shop\ndefault_inventory: production\nsecrets_from_env:\n  api_token: SHOP_API_TOKEN\n"), 0644)
	stdout, stderr, code := secretsOutputFromInput(t, "sentinel-token-value\n", "set", "api_token", "--replace", "--non-interactive")
	if code != 4 || stdout != "" || stderr != "mmx: secret key api_token is bound to environment variable SHOP_API_TOKEN by secrets_from_env in mmx_project_settings.yml. MMX reads api_token from the environment instead of the vault. Remove the secrets_from_env entry or choose a different secret key\n" {
		t.Fatalf("exit code %d\n%s%s", code, stdout, stderr)
	}
	if _, err := os.Stat(filepath.Join(root, "secrets_vault.age")); !os.IsNotExist(err) {
		t.Fatal("the refused command made a vault", err)
	}
}

// Each word after the key is one level inside it. The new name goes into
// its mapping, list-keys still shows the top-level key, and JSON keeps its
// shape. Messages name the place as the words joined by spaces.
func TestSecretsSetSetsANameInsideAKey(t *testing.T) {
	root, passwordFile := secretsProject(t)
	vault := filepath.Join(root, "secrets_vault.age")
	quickVault(t, vault, vaultTestPassphrase, "# The shop\ndatabase_password: sentinel-database-value\nmmwatchdog_secret_files:\n  # Mail\n  smtp-user: sentinel-nested-value\n")
	if stdout, stderr, code := secretsOutputFromInput(t, "sentinel-token-value\n", "set", "mmwatchdog_secret_files", "smtp-password", "--vault-password-file", passwordFile); code != 0 || !strings.HasPrefix(stdout, "Saved the key ") || strings.Contains(stdout, "sentinel") || stderr != "" {
		t.Fatalf("exit code %d\n%s%s", code, stdout, stderr)
	}
	// A certificate, two levels down, in a mapping that does not exist yet.
	const certificate = "-----BEGIN CERTIFICATE-----\nMIIBszCCAVmgAwIBAgIU\n-----END CERTIFICATE-----\n"
	stdout, stderr, code := secretsOutputFromInput(t, certificate+"\n", "set", "mmwatchdog_secret_files", "tls", "ca.pem", "--json", "--vault-password-file", passwordFile)
	var answer map[string]any
	if code != 0 || stderr != "" || json.Unmarshal([]byte(stdout), &answer) != nil || !reflect.DeepEqual(answer, map[string]any{"vault": vault, "status": "succeeded"}) {
		t.Fatalf("exit code %d\n%s%s", code, stdout, stderr)
	}
	values, err := secrets.Read(vault, vaultTestPassphrase)
	want := map[string]any{"database_password": "sentinel-database-value", "mmwatchdog_secret_files": map[string]any{
		"smtp-user": "sentinel-nested-value", "smtp-password": "sentinel-token-value", "tls": map[string]any{"ca.pem": certificate}}}
	if err != nil || !reflect.DeepEqual(values, want) {
		t.Fatal(len(values), err)
	}
	stdout, stderr, code = secretsOutputFromInput(t, "sentinel-changed-value\n", "set", "mmwatchdog_secret_files", "smtp-password", "--vault-password-file", passwordFile)
	if code != 4 || stdout != "" || stderr != "mmx: the vault already has the key mmwatchdog_secret_files smtp-password; to replace its value, add --replace\n" {
		t.Fatalf("exit code %d\n%s%s", code, stdout, stderr)
	}
	stdout, stderr, code = secretsOutput(t, "list-keys", "--vault-password-file", passwordFile)
	if code != 0 || stdout != "database_password\nmmwatchdog_secret_files\n" || stderr != "" {
		t.Fatalf("exit code %d\n%s%s", code, stdout, stderr)
	}
}
