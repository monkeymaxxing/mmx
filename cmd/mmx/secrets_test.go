package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"mmx/internal/secrets"
)

func TestVaultCLIUsesPasswordFilesAndPreservesFailedEdits(t *testing.T) {
	dir := t.TempDir()
	password := filepath.Join(dir, "password")
	os.WriteFile(password, []byte("cli test passphrase\n"), 0600)
	next := filepath.Join(dir, "next")
	os.WriteFile(next, []byte("next cli passphrase\n"), 0600)
	editor := filepath.Join(dir, "editor")
	writeProgramFileThroughShellChild(t, editor, "#!/bin/sh\nprintf 'token: cli-secret-sentinel\\n' > \"$1\"\n")
	t.Setenv("EDITOR", editor)
	t.Setenv("VISUAL", "")
	t.Setenv("MMX_VAULT_PASSWORD", "wrong-lower-priority")
	vault := filepath.Join(dir, "secrets.age")
	invoke := func(args ...string) (int, error) {
		t.Helper()
		return run(context.Background(), append([]string{"secrets"}, args...))
	}
	code, err := invoke("edit", "--vault", vault, "--vault-password-file", password, "--json")
	if err != nil || code != 0 {
		t.Fatal(code, err)
	}
	got, err := secrets.Read(vault, "cli test passphrase")
	if err != nil || got["token"] != "cli-secret-sentinel" {
		t.Fatal(err)
	}
	code, err = invoke("passwd", "--vault", vault, "--vault-password-file", password, "--new-vault-password-file", next, "--non-interactive")
	if err != nil || code != 0 {
		t.Fatal(code, err)
	}
	original, _ := os.ReadFile(vault)
	code, err = invoke("edit", "--vault", vault, "--vault-password-file", password, "--json")
	if err == nil || code == 0 || strings.Contains(err.Error(), "cli-secret-sentinel") {
		t.Fatal(code, err)
	}
	if b, _ := os.ReadFile(vault); string(b) != string(original) {
		t.Fatal("wrong password changed vault")
	}
	if code, err = invoke("edit", "--vault", vault, "--vault-password-file", "", "--json"); code != 4 || err == nil {
		t.Fatal("empty password-file option accepted")
	}
}

// The test executable also runs mmx's real main, so tests can signal it.
func TestMainProcessFixture(t *testing.T) {
	if os.Getenv("MMX_MAIN_FIXTURE") != "1" {
		return
	}
	os.Args = append([]string{"mmx"}, strings.Split(os.Getenv("MMX_MAIN_ARGS"), "\n")...)
	main()
}

// Closing the terminal (SIGHUP) or repeated interrupts during an edit must not
// leave the decrypted vault in the temporary directory.
func TestSignalsDuringVaultEditRemovePlaintext(t *testing.T) {
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	// Pending signals of one kind coalesce; distinct kinds make a second signal.
	// Run serially: concurrent scrypt fixtures and child processes exhaust 2 GiB guests.
	for _, signals := range []string{"HUP", "INT", "HUP INT", "INT TERM", "TERM HUP"} {
		t.Run(signals, func(t *testing.T) {
			dir := t.TempDir()
			temporary := filepath.Join(dir, "tmp")
			if err := os.Mkdir(temporary, 0700); err != nil {
				t.Fatal(err)
			}
			password := filepath.Join(dir, "password")
			if err := os.WriteFile(password, []byte("signal test passphrase\n"), 0600); err != nil {
				t.Fatal(err)
			}
			vault := filepath.Join(dir, "secrets.age")
			if err := secrets.Write(vault, "signal test passphrase", []byte("token: signal-secret-sentinel\n")); err != nil {
				t.Fatal(err)
			}
			original, _ := os.ReadFile(vault)
			marker := filepath.Join(dir, "editor-path")
			// The editor's parent is mmx: the editor shell execs in place.
			editor := filepath.Join(dir, "editor")
			script := "#!/bin/sh\nprintf '%s' \"$1\" > \"$MMX_TEST_EDITOR_PATH\"\nfor s in $MMX_TEST_SIGNALS; do kill -$s $PPID; done\nexec sleep 30\n"
			writeProgramFileThroughShellChild(t, editor, script)
			log, err := os.Create(filepath.Join(dir, "output"))
			if err != nil {
				t.Fatal(err)
			}
			defer log.Close()
			command := exec.Command(binary, "-test.run=^TestMainProcessFixture$")
			command.Env = append(os.Environ(), "MMX_MAIN_FIXTURE=1", "GORACE=atexit_sleep_ms=0", "TMPDIR="+temporary, "EDITOR="+editor, "VISUAL=",
				"MMX_TEST_EDITOR_PATH="+marker, "MMX_TEST_SIGNALS="+signals,
				"MMX_MAIN_ARGS="+strings.Join([]string{"secrets", "edit", "--vault", vault, "--vault-password-file", password, "--json"}, "\n"))
			command.Stdout, command.Stderr = log, log
			err = command.Run()
			output, _ := os.ReadFile(log.Name())
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 130 {
				t.Fatalf("mmx did not exit 130 after cleanup: %v\n%s", err, output)
			}
			if b, err := os.ReadFile(marker); err != nil || !strings.HasPrefix(string(b), temporary) {
				t.Fatal("editor did not open the temporary plaintext", err)
			}
			if entries, _ := os.ReadDir(temporary); len(entries) != 0 {
				t.Fatalf("decrypted vault left in the temporary directory: %v", entries)
			}
			if b, _ := os.ReadFile(vault); string(b) != string(original) {
				t.Fatal("interrupted edit changed the vault")
			}
		})
	}
}

// At a terminal, a rejected edit prints the reason and asks to edit again;
// Enter and y say yes, anything else says no. Without a terminal it never asks.
func TestEditAgainPrompt(t *testing.T) {
	for answer, want := range map[string]bool{"\n": true, "y\n": true, "Y\n": true, "yes\n": true, "n\n": false, "no\n": false, "": false} {
		var out strings.Builder
		again := editAgainAtTerminal(true, strings.NewReader(answer), &out)
		if got := again(errors.New("duplicate key")); got != want {
			t.Errorf("answer %q: got %v", answer, got)
		}
		if !strings.Contains(out.String(), "duplicate key") || !strings.Contains(out.String(), "Edit again? [Y/n]") {
			t.Errorf("answer %q: prompt %q", answer, out.String())
		}
	}
	var out strings.Builder
	if editAgainAtTerminal(false, strings.NewReader("\n"), &out) != nil || out.Len() != 0 {
		t.Error("without a terminal there is nothing to ask")
	}
}
