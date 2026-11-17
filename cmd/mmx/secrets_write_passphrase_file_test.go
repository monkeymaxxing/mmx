package main

import (
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"mmx/internal/secrets"
)

// writePassphraseFileAtTerminal runs mmx secrets write-passphrase-file as a
// person does: in its own session, whose controlling terminal is a
// pseudo-terminal, so the hidden prompt reads from it. It types each answer
// after the prompt for it appears and returns what the terminal showed and
// the exit code. HOME is home, so the file goes to home/.config/mmx/.
func writePassphraseFileAtTerminal(t *testing.T, home string, answers []string, args ...string) (shown string, code int) {
	t.Helper()
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	controller, terminal := openPseudoTerminal(t)
	defer controller.Close()
	command := exec.Command(binary, "-test.run=^TestMainProcessFixture$")
	command.Env = append(os.Environ(), "MMX_MAIN_FIXTURE=1", "HOME="+home,
		"MMX_MAIN_ARGS="+strings.Join(append([]string{"secrets", "write-passphrase-file"}, args...), "\n"))
	command.Stdin, command.Stdout, command.Stderr = terminal, terminal, terminal
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	terminal.Close()
	var mu sync.Mutex
	var seen bytes.Buffer
	go func() {
		buffer := make([]byte, 4096)
		for {
			n, err := controller.Read(buffer)
			mu.Lock()
			seen.Write(buffer[:n])
			mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	text := func() string {
		mu.Lock()
		defer mu.Unlock()
		return strings.ReplaceAll(seen.String(), "\r\n", "\n")
	}
	deadline := time.Now().Add(60 * time.Second)
	for i, answer := range answers {
		// Type only after the prompt turned off echo, as a person does.
		for strings.Count(text(), "passphrase: ") <= i {
			if time.Now().After(deadline) {
				command.Process.Kill()
				t.Fatalf("no prompt for answer %d:\n%s", i+1, text())
			}
			time.Sleep(10 * time.Millisecond)
		}
		if _, err := io.WriteString(controller, answer+"\n"); err != nil {
			t.Fatal(err)
		}
	}
	err = command.Wait()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		code = exit.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	// The reader stops when the terminal closes.
	time.Sleep(50 * time.Millisecond)
	return text(), code
}

const typedPassphrase = "typed sentinel passphrase"

// What a person sees: never the passphrase, and the two lines that say what
// to tell the coding agent and how to remove the file.
func checkShownLines(t *testing.T, shown string, wantLines ...string) {
	t.Helper()
	if strings.Contains(shown, typedPassphrase) {
		t.Fatalf("the terminal shows the passphrase:\n%s", shown)
	}
	if !strings.HasSuffix(shown, strings.Join(wantLines, "\n")+"\n") {
		t.Fatalf("the terminal does not end with\n%s\ngot:\n%s", strings.Join(wantLines, "\n"), shown)
	}
}

func TestSecretsWritePassphraseFileRefusesWithoutATerminal(t *testing.T) {
	root, _ := secretsProject(t)
	quickVault(t, filepath.Join(root, "secrets_vault.age"), typedPassphrase, "database_password: sentinel-database-value\n")
	home := t.TempDir()
	t.Setenv("HOME", home)
	stdout, stderr, code := secretsOutput(t, "write-passphrase-file")
	if code != 4 || stdout != "" || stderr != "mmx: mmx secrets write-passphrase-file asks for the vault passphrase at a terminal. Run it yourself in your own terminal, not through your coding agent\n" {
		t.Fatalf("exit code %d\n%s%s", code, stdout, stderr)
	}
	if _, err := os.Stat(filepath.Join(home, ".config")); !os.IsNotExist(err) {
		t.Fatal("the refused command wrote in the home directory", err)
	}
}

func TestSecretsWritePassphraseFileWritesTheCheckedPassphraseForAnAgent(t *testing.T) {
	root, _ := secretsProject(t)
	vault := filepath.Join(root, "secrets_vault.age")
	quickVault(t, vault, typedPassphrase, "database_password: sentinel-database-value\n")
	home := t.TempDir()
	file := filepath.Join(home, ".config/mmx/vault-passphrase-shop")

	// A wrong passphrase writes nothing.
	shown, code := writePassphraseFileAtTerminal(t, home, []string{"a wrong passphrase"})
	if code != 4 || !strings.Contains(shown, "Vault passphrase: ") || !strings.Contains(shown, "mmx: that passphrase does not open the vault secrets_vault.age. Nothing was written.") {
		t.Fatalf("exit code %d\n%s", code, shown)
	}
	if _, err := os.Stat(filepath.Join(home, ".config")); !os.IsNotExist(err) {
		t.Fatal("a wrong passphrase wrote in the home directory", err)
	}

	// The right one, asked once, goes to the file as one line.
	shown, code = writePassphraseFileAtTerminal(t, home, []string{typedPassphrase})
	if code != 0 || strings.Count(shown, "passphrase: ") != 1 {
		t.Fatalf("exit code %d\n%s", code, shown)
	}
	checkShownLines(t, shown,
		"Your coding agent can now run mmx with MMX_VAULT_PASSWORD_FILE=~/.config/mmx/vault-passphrase-shop.",
		"When you are done, delete the file: rm ~/.config/mmx/vault-passphrase-shop")
	if b, err := os.ReadFile(file); err != nil || string(b) != typedPassphrase+"\n" {
		t.Fatalf("file holds %q, %v", b, err)
	}
	for path, mode := range map[string]os.FileMode{file: 0o600, filepath.Dir(file): 0o700} {
		if info, err := os.Stat(path); err != nil || info.Mode().Perm() != mode {
			t.Fatalf("%s: %v, %v; want %v", path, info.Mode().Perm(), err, mode)
		}
	}

	// A later command, as the coding agent runs it, opens the vault without
	// a prompt.
	t.Setenv("MMX_VAULT_PASSWORD_FILE", file)
	if stdout, stderr, code := secretsOutput(t, "list-keys", "--non-interactive"); code != 0 || stdout != "database_password\n" {
		t.Fatalf("exit code %d\n%s%s", code, stdout, stderr)
	}
	os.Unsetenv("MMX_VAULT_PASSWORD_FILE")

	// Running it again replaces the file and says so; a wrong passphrase
	// keeps the old file.
	if err := os.WriteFile(file, []byte("an old passphrase\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, code := writePassphraseFileAtTerminal(t, home, []string{"a wrong passphrase"}); code != 4 {
		t.Fatal(code)
	}
	if b, _ := os.ReadFile(file); string(b) != "an old passphrase\n" {
		t.Fatalf("a wrong passphrase changed the file: %q", b)
	}
	shown, code = writePassphraseFileAtTerminal(t, home, []string{typedPassphrase})
	if code != 0 {
		t.Fatalf("exit code %d\n%s", code, shown)
	}
	checkShownLines(t, shown,
		"Replaced the old ~/.config/mmx/vault-passphrase-shop.",
		"Your coding agent can now run mmx with MMX_VAULT_PASSWORD_FILE=~/.config/mmx/vault-passphrase-shop.",
		"When you are done, delete the file: rm ~/.config/mmx/vault-passphrase-shop")
	if b, _ := os.ReadFile(file); string(b) != typedPassphrase+"\n" {
		t.Fatalf("file holds %q", b)
	}
}

// With no vault yet, the person chooses a new passphrase, asked twice with
// the new-vault advice. The command creates an empty vault with it, so the
// first mmx secrets set uses the file and asks for no other new passphrase.
// An inventory with a vault of its own gets a file of its own.
func TestSecretsWritePassphraseFileForANewVault(t *testing.T) {
	root, _ := secretsProject(t)
	home := t.TempDir()
	vault := filepath.Join(root, "inventories/staging_vault.age")
	file := filepath.Join(home, ".config/mmx/vault-passphrase-shop-staging_vault")

	shown, code := writePassphraseFileAtTerminal(t, home, []string{typedPassphrase, "a different passphrase"}, "--inventory", "staging")
	if code != 4 || !strings.Contains(shown, "mmx: vault passphrases do not match") {
		t.Fatalf("exit code %d\n%s", code, shown)
	}
	if _, err := os.Stat(vault); !os.IsNotExist(err) {
		t.Fatal("mismatched passphrases created the vault", err)
	}
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Fatal("mismatched passphrases wrote the file", err)
	}

	shown, code = writePassphraseFileAtTerminal(t, home, []string{typedPassphrase, typedPassphrase}, "--inventory", "staging")
	if code != 0 || !strings.Contains(shown, "make it new, not a password you use anywhere else") || !strings.Contains(shown, "Confirm new vault passphrase: ") {
		t.Fatalf("exit code %d\n%s", code, shown)
	}
	checkShownLines(t, shown,
		"Created the vault inventories/staging_vault.age with this passphrase. The vault holds no secrets yet.",
		"Your coding agent can now run mmx with MMX_VAULT_PASSWORD_FILE=~/.config/mmx/vault-passphrase-shop-staging_vault.",
		"When you are done, delete the file: rm ~/.config/mmx/vault-passphrase-shop-staging_vault")
	if values, err := secrets.Read(vault, typedPassphrase); err != nil || len(values) != 0 {
		t.Fatal(values, err)
	}
	if _, err := os.Stat(filepath.Join(root, "secrets_vault.age")); !os.IsNotExist(err) {
		t.Fatal("the project's vault was created for another inventory", err)
	}

	t.Setenv("MMX_VAULT_PASSWORD_FILE", file)
	if stdout, stderr, code := secretsOutputFromInput(t, "sentinel-token-value\n", "set", "api_token", "--inventory", "staging", "--non-interactive"); code != 0 || stderr != "" {
		t.Fatalf("exit code %d\n%s%s", code, stdout, stderr)
	}
	if values, err := secrets.Read(vault, typedPassphrase); err != nil || values["api_token"] != "sentinel-token-value" {
		t.Fatal(len(values), err)
	}
}
