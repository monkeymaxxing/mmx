package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"mmx/internal/secrets"

	"filippo.io/age"
)

// These values stand in the test vaults. No command may show one of them, or
// a part of one that these texts would match.
var vaultSentinels = []string{"sentinel-database-value", "sentinel-token-value", "sentinel-changed-value", "sentinel-nested-value"}

const vaultTestPassphrase = "keys test passphrase"

// secretsOutput runs one mmx secrets command and returns everything a user
// would see: standard output, then standard error with the error that main
// prints there, and the exit code.
func secretsOutput(t *testing.T, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	files := [2]*os.File{}
	for i := range files {
		file, err := os.CreateTemp(t.TempDir(), "output")
		if err != nil {
			t.Fatal(err)
		}
		files[i] = file
	}
	savedStdout, savedStderr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = files[0], files[1]
	code, err := run(context.Background(), append([]string{"secrets"}, args...))
	os.Stdout, os.Stderr = savedStdout, savedStderr
	texts := [2]string{}
	for i, file := range files {
		file.Close()
		b, _ := os.ReadFile(file.Name())
		texts[i] = string(b)
	}
	if err != nil {
		texts[1] += "mmx: " + err.Error() + "\n"
	}
	for _, sentinel := range vaultSentinels {
		if strings.Contains(texts[0]+texts[1], sentinel) {
			t.Fatalf("mmx secrets %s shows a secret's value:\n%s%s", strings.Join(args, " "), texts[0], texts[1])
		}
	}
	return texts[0], texts[1], code
}

// quickVault writes a vault whose passphrase takes little work to derive a
// key from, so that a test of a command that only reads does not wait.
func quickVault(t *testing.T, path, password, plaintext string) {
	t.Helper()
	recipient, err := age.NewScryptRecipient(password)
	if err != nil {
		t.Fatal(err)
	}
	recipient.SetWorkFactor(10)
	var encrypted bytes.Buffer
	writer, err := age.Encrypt(&encrypted, recipient)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte(plaintext)); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, encrypted.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
}

// secretsProject makes a project with two inventories, of which staging has
// a vault of its own, and makes it the working directory. It returns the
// project directory and a password file outside it.
func secretsProject(t *testing.T) (root, passwordFile string) {
	t.Helper()
	t.Setenv("MMX_RUNNER_DIR", t.TempDir())
	for _, name := range []string{"MMX_VAULT_PASSWORD_FILE", "MMX_VAULT_PASSWORD"} {
		t.Setenv(name, "")
		os.Unsetenv(name)
	}
	// TempDir may name a link; the commands see the directory it points to.
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for name, text := range map[string]string{
		"mmx_project_settings.yml":   "project_name: shop\ndefault_inventory: production\n",
		"inventories/production.yml": "nodes:\n  web01: {address: 192.0.2.10}\n",
		"inventories/staging.yml":    "vault_file: staging_vault.age\nnodes:\n  web01: {address: 192.0.2.20}\n",
	} {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, name)), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, name), []byte(text), 0644); err != nil {
			t.Fatal(err)
		}
	}
	passwordFile = filepath.Join(t.TempDir(), "vault-password")
	if err := os.WriteFile(passwordFile, []byte(vaultTestPassphrase+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Chdir(root)
	return root, passwordFile
}

func TestSecretsAddRandomAddsAKeyWithoutShowingItsValue(t *testing.T) {
	root, passwordFile := secretsProject(t)
	// A project that has no vault yet gets one: here the vault that the
	// selected inventory names.
	stagingVault := filepath.Join(root, "inventories/staging_vault.age")
	stdout, stderr, code := secretsOutput(t, "add-random", "session_cookie_secret", "--length", "48", "--inventory", "staging", "--vault-password-file", passwordFile)
	if code != 0 || stderr != "" || stdout != "Added the key session_cookie_secret to the vault inventories/staging_vault.age, with a random value of 48 letters and digits. MMX does not show the value.\n" {
		t.Fatal(code, stdout, stderr)
	}
	values, err := secrets.Read(stagingVault, vaultTestPassphrase)
	generated, _ := values["session_cookie_secret"].(string)
	if err != nil || len(values) != 1 || !regexp.MustCompile(`^[A-Za-z0-9]{48}$`).MatchString(generated) {
		t.Fatal(len(values), err)
	}
	if _, err := os.Stat(filepath.Join(root, "secrets_vault.age")); !os.IsNotExist(err) {
		t.Fatal("the project's vault was written for another inventory", err)
	}

	// An existing vault keeps its keys; the key may stand before or after
	// the options, and the default length is 32. JSON prints only the
	// vault's path and the status.
	vault := filepath.Join(root, "secrets_vault.age")
	quickVault(t, vault, vaultTestPassphrase, "# The shop\ndatabase_password: sentinel-database-value\n")
	stdout, stderr, code = secretsOutput(t, "add-random", "--json", "--vault-password-file", passwordFile, "api_token")
	var answer map[string]any
	if code != 0 || stderr != "" || json.Unmarshal([]byte(stdout), &answer) != nil || !reflect.DeepEqual(answer, map[string]any{"vault": vault, "status": "succeeded"}) {
		t.Fatal(code, stdout, stderr)
	}
	values, err = secrets.Read(vault, vaultTestPassphrase)
	second, _ := values["api_token"].(string)
	if err != nil || len(values) != 2 || values["database_password"] != "sentinel-database-value" || !regexp.MustCompile(`^[A-Za-z0-9]{32}$`).MatchString(second) {
		t.Fatal(len(values), err)
	}
	if strings.Contains(stdout, second) || strings.Contains(stdout, generated) {
		t.Fatal("add-random printed the value it generated")
	}
	if info, err := os.Stat(vault); err != nil || info.Mode().Perm() != 0600 {
		t.Fatal(info, err)
	}
}

// --replace gives a key that the vault already has a new random value, as
// mmx secrets set --replace gives it the value on standard input; the other
// keys stay. A key the vault does not have is added.
func TestSecretsAddRandomReplaceGivesAnExistingKeyANewValue(t *testing.T) {
	root, passwordFile := secretsProject(t)
	vault := filepath.Join(root, "secrets_vault.age")
	quickVault(t, vault, vaultTestPassphrase, "# The shop\ndatabase_password: sentinel-database-value\napi_token: sentinel-token-value\n")
	stdout, stderr, code := secretsOutput(t, "add-random", "database_password", "--replace", "--length", "40", "--vault-password-file", passwordFile)
	if code != 0 || stderr != "" || stdout != "Replaced the value of the key database_password in the vault secrets_vault.age with a random value of 40 letters and digits. MMX does not show the value.\n" {
		t.Fatal(code, stdout, stderr)
	}
	stdout, stderr, code = secretsOutput(t, "add-random", "--replace", "session_cookie_secret", "--vault-password-file", passwordFile)
	if code != 0 || stderr != "" || stdout != "Added the key session_cookie_secret to the vault secrets_vault.age, with a random value of 32 letters and digits. MMX does not show the value.\n" {
		t.Fatal(code, stdout, stderr)
	}
	values, err := secrets.Read(vault, vaultTestPassphrase)
	replaced, _ := values["database_password"].(string)
	added, _ := values["session_cookie_secret"].(string)
	if err != nil || len(values) != 3 || values["api_token"] != "sentinel-token-value" || !regexp.MustCompile(`^[A-Za-z0-9]{40}$`).MatchString(replaced) ||
		!regexp.MustCompile(`^[A-Za-z0-9]{32}$`).MatchString(added) {
		t.Fatal(len(values), err)
	}
}

func TestSecretsAddRandomRefusalsKeepTheVault(t *testing.T) {
	root, passwordFile := secretsProject(t)
	vault := filepath.Join(root, "secrets_vault.age")
	quickVault(t, vault, vaultTestPassphrase, "database_password: sentinel-database-value\n")
	original, _ := os.ReadFile(vault)
	wrong := filepath.Join(t.TempDir(), "wrong")
	os.WriteFile(wrong, []byte("another passphrase\n"), 0600)
	for name, tc := range map[string]struct {
		args []string
		want string
	}{
		"the key exists":                   {[]string{"add-random", "database_password", "--vault-password-file", passwordFile}, "the vault already has the key database_password; to give it a new random value, add --replace"},
		"no key":                           {[]string{"add-random", "--vault-password-file", passwordFile}, "usage: mmx secrets add-random KEY [NAME ...] [--length N] [--replace]"},
		"a name inside a key with a value": {[]string{"add-random", "database_password", "smtp-password", "--vault-password-file", passwordFile}, "the vault's database_password holds a value, not names, so smtp-password cannot go inside it. To keep the value, choose another name; to replace the value with names, run mmx secrets edit"},
		"an empty name":                    {[]string{"add-random", "web_app", "", "--vault-password-file", passwordFile}, "an empty name cannot be a name inside the secret key web_app"},
		"a key that is no name":            {[]string{"add-random", "9lives", "--vault-password-file", passwordFile}, `"9lives" cannot be a secret key`},
		"a key that YAML reads":            {[]string{"add-random", "true", "--vault-password-file", passwordFile}, "YAML reads true as a value instead of a name"},
		"a length of zero":                 {[]string{"add-random", "new_key", "--length", "0", "--vault-password-file", passwordFile}, "--length must be a positive integer, such as --length 48; got 0"},
		"a negative length":                {[]string{"add-random", "new_key", "--length=-3", "--vault-password-file", passwordFile}, "--length must be a positive integer, such as --length 48; got -3"},
		"a length that is no number":       {[]string{"add-random", "new_key", "--length", "long", "--vault-password-file", passwordFile}, `invalid value "long" for flag -length`},
		"a length beyond the vault's size": {[]string{"add-random", "new_key", "--length", "99999999", "--vault-password-file", passwordFile}, "a value of 99999999 characters does not fit into the vault, which holds at most 4 MiB"},
		"a wrong passphrase":               {[]string{"add-random", "new_key", "--vault-password-file", wrong}, "cannot decrypt vault: wrong passphrase or invalid age file"},
		"no passphrase":                    {[]string{"add-random", "new_key", "--non-interactive"}, "vault passphrase required; use --vault-password-file"},
		"an option of passwd":              {[]string{"add-random", "new_key", "--new-vault-password-file", passwordFile}, "flag provided but not defined: -new-vault-password-file"},
		"an inventory that is not there":   {[]string{"add-random", "new_key", "--inventory", "missing", "--vault-password-file", passwordFile}, "inventory missing"},
	} {
		t.Run(name, func(t *testing.T) {
			stdout, stderr, code := secretsOutput(t, tc.args...)
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

// A run reads a key that secrets_from_env binds from the environment
// variable, so a value for it in the vault would never be read.
func TestSecretsAddRandomRefusesAKeyThatTheProjectReadsFromTheEnvironment(t *testing.T) {
	root, passwordFile := secretsProject(t)
	settings := filepath.Join(root, "mmx_project_settings.yml")
	os.WriteFile(settings, []byte("project_name: shop\ndefault_inventory: production\nsecrets_from_env:\n  api_token: SHOP_API_TOKEN\n"), 0644)
	vault := filepath.Join(root, "secrets_vault.age")
	quickVault(t, vault, vaultTestPassphrase, "database_password: sentinel-database-value\n")
	original, _ := os.ReadFile(vault)
	const refusal = "mmx: secret key api_token is bound to environment variable SHOP_API_TOKEN by secrets_from_env in mmx_project_settings.yml. MMX reads api_token from the environment instead of the vault. Remove the secrets_from_env entry or choose a different secret key\n"
	// The refusal comes before the vault is unlocked: it needs no passphrase.
	for name, args := range map[string][]string{
		"the project's vault":              {"add-random", "api_token", "--non-interactive"},
		"another inventory's vault":        {"add-random", "api_token", "--inventory", "staging", "--non-interactive"},
		"another vault inside the project": {"add-random", "api_token", "--vault", "other.age", "--vault-password-file", passwordFile},
		"as JSON":                          {"add-random", "api_token", "--json"},
	} {
		t.Run(name, func(t *testing.T) {
			if stdout, stderr, code := secretsOutput(t, args...); code != 4 || stdout != "" || stderr != refusal {
				t.Fatalf("exit code %d\n%s%s", code, stdout, stderr)
			}
		})
	}
	t.Chdir(filepath.Join(root, "inventories"))
	if stdout, stderr, code := secretsOutput(t, "add-random", "api_token", "--non-interactive"); code != 4 || stdout != "" || stderr != refusal {
		t.Fatalf("exit code %d\n%s%s", code, stdout, stderr)
	}
	t.Chdir(root)
	if now, _ := os.ReadFile(vault); !bytes.Equal(now, original) {
		t.Fatal("the refused command changed the vault")
	}
	for _, created := range []string{"other.age", "inventories/staging_vault.age"} {
		if _, err := os.Stat(filepath.Join(root, created)); !os.IsNotExist(err) {
			t.Fatalf("the refused command made %s: %v", created, err)
		}
	}
	// Settings that do not load are an error, also with --vault: MMX does
	// not skip the check.
	os.WriteFile(settings, []byte("project_name: [shop\n"), 0644)
	if stdout, stderr, code := secretsOutput(t, "add-random", "api_token", "--vault", "other.age", "--vault-password-file", passwordFile); code != 4 || stdout != "" || !strings.Contains(stderr, "mmx_project_settings.yml") {
		t.Fatalf("exit code %d\n%s%s", code, stdout, stderr)
	}
	// A vault that --vault names outside a project has no project to ask.
	t.Chdir(t.TempDir())
	stdout, stderr, code := secretsOutput(t, "add-random", "api_token", "--vault", "other.age", "--vault-password-file", passwordFile)
	if code != 0 || stderr != "" || stdout != "Added the key api_token to the vault other.age, with a random value of 32 letters and digits. MMX does not show the value.\n" {
		t.Fatalf("exit code %d\n%s%s", code, stdout, stderr)
	}
}

// edit and passwd name the vault as every secrets command names a file for
// people: relative to the working directory.
func TestSecretsEditAndPasswdNameTheVaultFromTheWorkingDirectory(t *testing.T) {
	root, passwordFile := secretsProject(t)
	next := filepath.Join(t.TempDir(), "next")
	os.WriteFile(next, []byte("next passphrase\n"), 0600)
	// passwd needs a vault that is there, and says how to make one.
	for vault, args := range map[string][]string{
		"secrets_vault.age":             {"passwd", "--vault-password-file", passwordFile, "--new-vault-password-file", next},
		"inventories/staging_vault.age": {"passwd", "--inventory", "staging", "--vault-password-file", passwordFile, "--new-vault-password-file", next},
		"missing/other.age":             {"passwd", "--vault", "missing/other.age", "--vault-password-file", passwordFile, "--new-vault-password-file", next, "--json"},
	} {
		stdout, stderr, code := secretsOutput(t, args...)
		if code != 4 || stdout != "" || stderr != "mmx: the vault "+vault+" does not exist; create it with mmx secrets set KEY, which asks for one secret's value, or mmx secrets add-random KEY\n" {
			t.Fatalf("exit code %d\n%s%s", code, stdout, stderr)
		}
	}
	if entries, _ := os.ReadDir(root); len(entries) != 2 {
		t.Fatalf("passwd without a vault left a file in the project: %v", entries)
	}
	vault := filepath.Join(root, "secrets_vault.age")
	quickVault(t, vault, vaultTestPassphrase, "database_password: sentinel-database-value\n")
	editor := filepath.Join(t.TempDir(), "editor")
	writeProgramFileThroughShellChild(t, editor, "#!/bin/sh\nprintf 'token: sentinel-token-value\\n' > \"$1\"\n")
	t.Setenv("EDITOR", editor)
	t.Setenv("VISUAL", "")
	t.Chdir(filepath.Join(root, "inventories"))
	if stdout, stderr, code := secretsOutput(t, "edit", "--vault-password-file", passwordFile); code != 0 || stderr != "" || stdout != "Saved encrypted vault: ../secrets_vault.age\n" {
		t.Fatalf("exit code %d\n%s%s", code, stdout, stderr)
	}
	t.Chdir(root)
	quickVault(t, vault, vaultTestPassphrase, "database_password: sentinel-database-value\n")
	if stdout, stderr, code := secretsOutput(t, "passwd", "--vault-password-file", passwordFile, "--new-vault-password-file", next); code != 0 || stderr != "" || stdout != "Saved encrypted vault: secrets_vault.age\n" {
		t.Fatalf("exit code %d\n%s%s", code, stdout, stderr)
	}
	if values, err := secrets.Read(vault, "next passphrase"); err != nil || values["database_password"] != "sentinel-database-value" {
		t.Fatal(len(values), err)
	}
}

func TestSecretsListKeysPrintsKeysAndNeverWrites(t *testing.T) {
	root, passwordFile := secretsProject(t)
	// No vault yet: the message says how to make one.
	_, stderr, code := secretsOutput(t, "list-keys", "--vault-password-file", passwordFile)
	if code != 4 || !strings.Contains(stderr, "the vault secrets_vault.age does not exist; create it with mmx secrets set KEY, which asks for one secret's value, or mmx secrets add-random KEY") {
		t.Fatal(code, stderr)
	}
	vault := filepath.Join(root, "secrets_vault.age")
	quickVault(t, vault, vaultTestPassphrase, "# The shop\nsession_cookie_secret: sentinel-token-value\ndatabase:\n  password: sentinel-nested-value\napi_token: sentinel-database-value\n")
	original, _ := os.ReadFile(vault)
	before, _ := os.Stat(vault)
	stdout, stderr, code := secretsOutput(t, "list-keys", "--vault-password-file", passwordFile)
	if code != 0 || stderr != "" || stdout != "api_token\ndatabase\nsession_cookie_secret\n" {
		t.Fatal(code, stdout, stderr)
	}
	// MMX_VAULT_PASSWORD is a passphrase source too, and JSON never prompts.
	t.Setenv("MMX_VAULT_PASSWORD", vaultTestPassphrase)
	stdout, stderr, code = secretsOutput(t, "list-keys", "--json")
	var answer struct {
		SchemaVersion int `json:"schema_version"`
		Vault         string
		Keys          []string
	}
	if code != 0 || stderr != "" || json.Unmarshal([]byte(stdout), &answer) != nil || answer.SchemaVersion != 1 || answer.Vault != vault ||
		!reflect.DeepEqual(answer.Keys, []string{"api_token", "database", "session_cookie_secret"}) {
		t.Fatal(code, stdout, stderr)
	}
	os.Unsetenv("MMX_VAULT_PASSWORD")

	// Another vault by --vault, from a directory that is no project.
	other := filepath.Join(t.TempDir(), "other.age")
	quickVault(t, other, vaultTestPassphrase, "{}\n")
	t.Chdir(t.TempDir())
	if stdout, stderr, code = secretsOutput(t, "list-keys", "--vault", other, "--vault-password-file", passwordFile); code != 0 || stdout != "" || stderr != "" {
		t.Fatal(code, stdout, stderr)
	}
	if stdout, _, code = secretsOutput(t, "list-keys", "--vault", other, "--vault-password-file", passwordFile, "--json"); code != 0 || !strings.Contains(stdout, `"keys": []`) {
		t.Fatal(code, stdout)
	}
	if _, stderr, code = secretsOutput(t, "list-keys", "--vault-password-file", passwordFile); code != 4 || !strings.Contains(stderr, "no mmx_project_settings.yml found") {
		t.Fatal(code, stderr)
	}
	t.Chdir(root)

	wrong := filepath.Join(t.TempDir(), "wrong")
	os.WriteFile(wrong, []byte("another passphrase\n"), 0600)
	broken := filepath.Join(t.TempDir(), "broken.age")
	quickVault(t, broken, vaultTestPassphrase, "database_password: [sentinel-database-value\n")
	for name, tc := range map[string]struct {
		args []string
		want string
	}{
		"a wrong passphrase":       {[]string{"list-keys", "--vault-password-file", wrong}, "cannot decrypt vault: wrong passphrase or invalid age file"},
		"no passphrase":            {[]string{"list-keys", "--non-interactive"}, "vault passphrase required; use --vault-password-file"},
		"a vault that is no YAML":  {[]string{"list-keys", "--vault", broken, "--vault-password-file", passwordFile}, "vault must contain one YAML mapping with unique secret names"},
		"a file that is no vault":  {[]string{"list-keys", "--vault", passwordFile, "--vault-password-file", passwordFile}, "cannot decrypt vault: wrong passphrase or invalid age file"},
		"an argument":              {[]string{"list-keys", "database", "--vault-password-file", passwordFile}, "mmx secrets list-keys takes no arguments"},
		"the option of add-random": {[]string{"list-keys", "--length", "5", "--vault-password-file", passwordFile}, "flag provided but not defined: -length"},
	} {
		t.Run(name, func(t *testing.T) {
			if stdout, stderr, code := secretsOutput(t, tc.args...); code != 4 || stdout != "" || !strings.Contains(stderr, tc.want) {
				t.Fatalf("exit code %d\n%s%s", code, stdout, stderr)
			}
		})
	}
	after, _ := os.Stat(vault)
	if now, _ := os.ReadFile(vault); !bytes.Equal(now, original) || !after.ModTime().Equal(before.ModTime()) {
		t.Fatal("list-keys wrote the vault")
	}
}

// gitForTests runs git in directory with a configuration of its own, so
// that the user's settings, such as commit signing, play no part.
func gitForTests(t *testing.T, directory string, args ...string) {
	t.Helper()
	command := exec.Command("git", args...)
	command.Dir = directory
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
	}
}

func TestSecretsDiffKeysComparesWithTheLastCommit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	root, passwordFile := secretsProject(t)
	for name, value := range map[string]string{"GIT_CONFIG_GLOBAL": os.DevNull, "GIT_CONFIG_SYSTEM": os.DevNull, "GIT_AUTHOR_NAME": "Test", "GIT_AUTHOR_EMAIL": "test@example.net", "GIT_COMMITTER_NAME": "Test", "GIT_COMMITTER_EMAIL": "test@example.net"} {
		t.Setenv(name, value)
	}
	vault := filepath.Join(root, "secrets_vault.age")
	quickVault(t, vault, vaultTestPassphrase, "database_password: sentinel-database-value\nold_cdn_token: sentinel-token-value\nunchanged:\n  user: app\n  password: sentinel-nested-value\n")
	refused := func(want string, args ...string) {
		t.Helper()
		stdout, stderr, code := secretsOutput(t, append([]string{"diff-keys", "--vault-password-file", passwordFile}, args...)...)
		if code != 4 || stdout != "" || !strings.Contains(stderr, want) {
			t.Fatalf("exit code %d, want %q\n%s%s", code, want, stdout, stderr)
		}
	}
	// Each reason for which there is no committed vault has its own message.
	refused("the vault secrets_vault.age is not in a Git repository, so there is no last commit to compare with")
	gitForTests(t, root, "init", "--quiet")
	refused("the Git repository of the vault secrets_vault.age has no commit yet")
	gitForTests(t, root, "add", "mmx_project_settings.yml")
	gitForTests(t, root, "commit", "--quiet", "--message", "settings")
	refused("the last Git commit does not contain the vault secrets_vault.age, so the vault has no committed version to compare with; commit the vault first")
	gitForTests(t, root, "add", "secrets_vault.age")
	gitForTests(t, root, "commit", "--quiet", "--message", "vault")

	// Nothing differs: the three lines are still there, and the exit code is 0.
	stdout, stderr, code := secretsOutput(t, "diff-keys", "--vault-password-file", passwordFile)
	if code != 0 || stderr != "" || stdout != "Vault: secrets_vault.age, compared with the last commit\n\nAdded:    (none)\nRemoved:  (none)\nChanged:  (none)\n" {
		t.Fatal(code, stdout, stderr)
	}
	// An edit: two keys added, one removed, one changed, one written in
	// another way with the same value.
	quickVault(t, vault, vaultTestPassphrase, "# Edited\nunchanged: {password: sentinel-nested-value, user: app}\nsession_cookie_secret: sentinel-token-value\ndatabase_password: sentinel-changed-value\napi_token: sentinel-token-value\n")
	original, _ := os.ReadFile(vault)
	before, _ := os.Stat(vault)
	stdout, stderr, code = secretsOutput(t, "diff-keys", "--vault-password-file", passwordFile)
	if code != 0 || stderr != "" || stdout != "Vault: secrets_vault.age, compared with the last commit\n\nAdded:    api_token, session_cookie_secret\nRemoved:  old_cdn_token\nChanged:  database_password\n" {
		t.Fatal(code, stdout, stderr)
	}
	// From a directory below the project root, the vault is named from
	// there; JSON keeps the whole path.
	t.Chdir(filepath.Join(root, "inventories"))
	if stdout, _, code = secretsOutput(t, "diff-keys", "--vault-password-file", passwordFile); code != 0 || !strings.HasPrefix(stdout, "Vault: ../secrets_vault.age, compared with the last commit\n\nAdded:    api_token, session_cookie_secret\n") {
		t.Fatal(code, stdout)
	}
	stdout, stderr, code = secretsOutput(t, "diff-keys", "--vault-password-file", passwordFile, "--json")
	var answer struct {
		SchemaVersion           int `json:"schema_version"`
		Vault                   string
		Added, Removed, Changed []string
	}
	if code != 0 || stderr != "" || json.Unmarshal([]byte(stdout), &answer) != nil || answer.SchemaVersion != 1 || answer.Vault != vault ||
		!reflect.DeepEqual(answer.Added, []string{"api_token", "session_cookie_secret"}) || !reflect.DeepEqual(answer.Removed, []string{"old_cdn_token"}) || !reflect.DeepEqual(answer.Changed, []string{"database_password"}) {
		t.Fatal(code, stdout, stderr)
	}
	t.Chdir(root)
	after, _ := os.Stat(vault)
	if now, _ := os.ReadFile(vault); !bytes.Equal(now, original) || !after.ModTime().Equal(before.ModTime()) {
		t.Fatal("diff-keys wrote the vault")
	}

	// The vault of another inventory is read in its own directory.
	stagingVault := filepath.Join(root, "inventories/staging_vault.age")
	quickVault(t, stagingVault, vaultTestPassphrase, "staging_password: sentinel-database-value\n")
	refused("the last Git commit does not contain the vault inventories/staging_vault.age", "--inventory", "staging")
	gitForTests(t, root, "add", "inventories/staging_vault.age")
	gitForTests(t, root, "commit", "--quiet", "--message", "staging vault")
	quickVault(t, stagingVault, vaultTestPassphrase, "staging_password: sentinel-changed-value\n")
	stdout, _, code = secretsOutput(t, "diff-keys", "--vault-password-file", passwordFile, "--vault", "inventories/staging_vault.age")
	if code != 0 || stdout != "Vault: inventories/staging_vault.age, compared with the last commit\n\nAdded:    (none)\nRemoved:  (none)\nChanged:  staging_password\n" {
		t.Fatal(code, stdout)
	}

	// The committed version has another passphrase.
	other := filepath.Join(t.TempDir(), "other")
	os.WriteFile(other, []byte("another passphrase\n"), 0600)
	quickVault(t, vault, "another passphrase", "database_password: sentinel-database-value\n")
	stdout, stderr, code = secretsOutput(t, "diff-keys", "--vault-password-file", other)
	if code != 4 || stdout != "" || !strings.Contains(stderr, "the last committed version of vault secrets_vault.age cannot be opened with the supplied passphrase") {
		t.Fatal(code, stdout, stderr)
	}
	// The working file does not open, takes an argument, or is not there.
	refused("cannot decrypt vault: wrong passphrase or invalid age file")
	refused("mmx secrets diff-keys takes no arguments", "database_password")
	os.Remove(vault)
	refused("the vault secrets_vault.age does not exist")

	// A machine without the git program.
	quickVault(t, vault, vaultTestPassphrase, "database_password: sentinel-database-value\n")
	t.Setenv("PATH", t.TempDir())
	refused("git is not installed on this machine (no git in PATH)")
}
