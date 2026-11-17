package secrets

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func cleanEnvironment(t *testing.T) {
	t.Helper()
	for _, k := range []string{"MMX_VAULT_PASSWORD_FILE", "MMX_VAULT_PASSWORD"} {
		t.Setenv(k, "")
		os.Unsetenv(k)
	}
}

// A new vault opens with an uncommented example line, so an editor that
// continues comments does not turn a typed secret into a comment. The
// person replaces the example or adds a line below; saved unchanged, the
// vault holds only the example key, which nothing reads.
func TestANewVaultTakesALineAddedBelowItsStartingText(t *testing.T) {
	const password = "test editor passphrase"
	const example = "replace this line with your own secret key and value"
	for name, tc := range map[string]struct {
		editorScript string
		want         map[string]any
	}{
		"a line added below":     {"#!/bin/sh\nprintf 'join_code: \"added-by-a-person\"\\n' >> \"$1\"\n", map[string]any{"example_secret_key": example, "join_code": "added-by-a-person"}},
		"the example replaced":   {"#!/bin/sh\nsed 's/^example_secret_key: .*/join_code: \"typed\"/' \"$1\" > \"$1.new\" && mv \"$1.new\" \"$1\"\n", map[string]any{"join_code": "typed"}},
		"saved without a change": {"#!/bin/sh\nexit 0\n", map[string]any{"example_secret_key": example}},
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "secrets.age")
			editor := filepath.Join(t.TempDir(), "editor")
			writeProgramFileThroughShellChild(t, editor, tc.editorScript)
			if err := Edit(context.Background(), path, password, editor, nil); err != nil {
				t.Fatal(err)
			}
			if name == "saved without a change" {
				text, err := decrypt(path, password)
				if err != nil || !strings.Contains(string(text), "\n# Easier for one secret: close this editor and run mmx secrets set KEY.") ||
					!strings.Contains(string(text), "\n# To save and close: in vim, press Esc, type :wq, and press Enter. In other editors, save and close.\n") {
					t.Fatalf("the starting text does not point to mmx secrets set or say how to save: %q, %v", text, err)
				}
			}
			values, err := Read(path, password)
			if err != nil || !reflect.DeepEqual(values, tc.want) {
				t.Fatalf("got %v, %v", values, err)
			}
		})
	}
}

func TestInterruptedEditRemovesPlaintextAndPreservesVault(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secrets.age")
	if err := Write(path, "test editor passphrase", []byte("token: private-value\n")); err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "editor-path")
	t.Setenv("MMX_TEST_EDITOR_PATH", marker)
	editor := filepath.Join(t.TempDir(), "editor")
	writeProgramFileThroughShellChild(t, editor, "#!/bin/sh\nprintf '%s' \"$1\" > \"$MMX_TEST_EDITOR_PATH\"\nexec sleep 60\n")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- Edit(ctx, path, "test editor passphrase", editor, nil) }()
	// Edit derives the key from the passphrase before it starts the editor; that
	// is slow by design and much slower under the race detector on a busy host.
	deadline := time.After(90 * time.Second)
	var temporary string
	for temporary == "" {
		if b, err := os.ReadFile(marker); err == nil {
			temporary = string(b)
		}
		select {
		case err := <-done:
			t.Fatal("editor exited before cancellation", err)
		case <-deadline:
			t.Fatal("editor did not start")
		case <-time.After(10 * time.Millisecond):
		}
	}
	for path, mode := range map[string]os.FileMode{temporary: 0600, filepath.Dir(temporary): 0700} {
		info, err := os.Stat(path)
		if err != nil || info.Mode().Perm() != mode {
			t.Fatal("editor file is not private", err)
		}
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("interrupted editor did not stop")
	}
	if _, err := os.Stat(filepath.Dir(temporary)); !os.IsNotExist(err) {
		t.Fatal("editor plaintext directory remains")
	}
	if b, err := os.ReadFile(path); err != nil || string(b) != string(original) {
		t.Fatal("interrupted edit changed vault", err)
	}
}
func file(t *testing.T, name, contents string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
	return p
}
func TestPasswordSourcesAndNoFallback(t *testing.T) {
	cleanEnvironment(t)
	first := file(t, "password", "  selected password  \r\n")
	second := file(t, "password", "second")
	t.Setenv("MMX_VAULT_PASSWORD_FILE", second)
	t.Setenv("MMX_VAULT_PASSWORD", "third")
	prompted := 0
	s := Store{PasswordFile: first, Interactive: true, Prompt: func(string) (string, error) { prompted++; return "prompted", nil }}
	for i := 0; i < 2; i++ {
		v, err := s.Password(false)
		if err != nil || v != "  selected password  " {
			t.Fatal(v, err)
		}
	}
	if prompted != 0 {
		t.Fatal("configured file prompted")
	}
	for _, text := range []string{"", "one\ntwo\n", "one\r", "one\x00two"} {
		p := file(t, "invalid", text)
		s := Store{PasswordFile: p, Interactive: true, Prompt: s.Prompt}
		if _, err := s.Password(false); err == nil {
			t.Fatalf("accepted invalid password file %q", text)
		}
	}
	if _, err := (&Store{PasswordFile: filepath.Join(t.TempDir(), "missing"), Interactive: true, Prompt: s.Prompt}).Password(false); err == nil {
		t.Fatal("missing file fell through")
	}
	if prompted != 0 {
		t.Fatal("bad file fell back to a prompt")
	}
	v, err := (&Store{}).Password(false)
	if err != nil || v != "second" {
		t.Fatal(v, err)
	}
	os.Unsetenv("MMX_VAULT_PASSWORD_FILE")
	v, err = (&Store{}).Password(false)
	if err != nil || v != "third" {
		t.Fatal(v, err)
	}
	os.Unsetenv("MMX_VAULT_PASSWORD")
	var labels []string
	v, err = (&Store{Interactive: true, Prompt: func(label string) (string, error) { labels = append(labels, label); return s.Prompt(label) }}).Password(true)
	if err != nil || v != "prompted" || prompted != 2 {
		t.Fatal(v, err, prompted)
	}
	// A new vault's prompt says that the person chooses the passphrase now
	// and must keep it.
	if len(labels) != 2 || labels[0] != "You are creating a new vault. Choose its passphrase now, and keep it safe, such as in a password manager.\nNobody can open the vault without the passphrase, and MMX cannot recover a lost one.\nNew vault passphrase: " ||
		labels[1] != "Confirm new vault passphrase: " {
		t.Fatalf("new vault prompts: %q", labels)
	}
	if _, err = (&Store{}).Password(false); err == nil || !strings.Contains(err.Error(), "--vault-password-file") {
		t.Fatal(err)
	}
}
func TestAgeVaultEditAndPasswordChange(t *testing.T) {
	cleanEnvironment(t)
	path := filepath.Join(t.TempDir(), "secrets.age")
	const password = "test shared vault passphrase"
	const plaintext = "database_password: sentinel-database-value\nnumber: 42\n"
	if err := Write(path, password, []byte(plaintext)); err != nil {
		t.Fatal(err)
	}
	encrypted, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(encrypted), "age-encryption.org/v1\n") || strings.Contains(string(encrypted), "sentinel-database-value") {
		t.Fatal("not an encrypted age file")
	}
	got, err := Read(path, password)
	if err != nil || got["database_password"] != "sentinel-database-value" || got["number"] != 42 {
		t.Fatal(got, err)
	}
	if _, err := Read(path, "wrong"); err == nil {
		t.Fatal("wrong passphrase succeeded")
	}
	bad := file(t, "damaged.age", string(encrypted[:len(encrypted)-8]))
	if _, err := Read(bad, password); err == nil {
		t.Fatal("truncated ciphertext succeeded")
	}
	for _, invalid := range []string{"token: one\ntoken: secret-value-never-in-errors\n", "[secret-value-never-in-errors]", "token: [secret-value-never-in-errors"} {
		if err := Write(path, password, []byte(invalid)); err == nil || strings.Contains(err.Error(), "secret-value-never-in-errors") {
			t.Fatal(err)
		}
	}
	if now, _ := os.ReadFile(path); string(now) != string(encrypted) {
		t.Fatal("invalid write replaced vault")
	}
	editor := filepath.Join(t.TempDir(), "editor")
	writeProgramFileThroughShellChild(t, editor, "#!/bin/sh\nprintf '%s' \"$1\" > \"$MMX_TEST_EDITOR_PATH\"\nprintf 'token: [invalid' > \"$1\"\n")
	marker := filepath.Join(t.TempDir(), "path")
	t.Setenv("MMX_TEST_EDITOR_PATH", marker)
	if err := Edit(context.Background(), path, password, editor, nil); err == nil {
		t.Fatal("bad edit succeeded")
	}
	tmp, _ := os.ReadFile(marker)
	if _, err := os.Stat(filepath.Dir(string(tmp))); !os.IsNotExist(err) {
		t.Fatal("editor plaintext directory remains")
	}
	if now, _ := os.ReadFile(path); string(now) != string(encrypted) {
		t.Fatal("failed edit replaced vault")
	}
	if err := ChangePassword(path, password, "new test passphrase"); err != nil {
		t.Fatal(err)
	}
	if _, err := Read(path, password); err == nil {
		t.Fatal("old passphrase still works")
	}
	if got, err := Read(path, "new test passphrase"); err != nil || got["database_password"] != "sentinel-database-value" {
		t.Fatal(got, err)
	}
}

// A rejected edit is offered back to the editor with the person's own text, not
// the old vault, and nothing but the editor's temporary file holds that text.
func TestRejectedEditReopensTheEditorWithTheEditedText(t *testing.T) {
	const password = "test editor passphrase"
	path := filepath.Join(t.TempDir(), "secrets.age")
	if err := Write(path, password, []byte("token: old-value\n")); err != nil {
		t.Fatal(err)
	}
	original, _ := os.ReadFile(path)
	marker := filepath.Join(t.TempDir(), "editor-path")
	t.Setenv("MMX_TEST_EDITOR_PATH", marker)
	editor := filepath.Join(t.TempDir(), "editor")
	// First run: add a duplicate key. Second run: remove the duplicate line and
	// record the text the editor was given.
	writeProgramFileThroughShellChild(t, editor, "#!/bin/sh\nprintf '%s' \"$1\" > \"$MMX_TEST_EDITOR_PATH\"\nif grep -q 'token: second' \"$1\"; then\n  cp \"$1\" \"$1.seen\"; sed -i.bak '/token: second/d' \"$1\"; rm \"$1.bak\"\nelse\n  printf 'token: second\\n' >> \"$1\"\nfi\n")
	var asked []string
	again := func(err error) bool { asked = append(asked, err.Error()); return len(asked) == 1 }
	if err := Edit(context.Background(), path, password, editor, again); err != nil {
		t.Fatal(err)
	}
	if len(asked) != 1 || !strings.Contains(asked[0], "unique secret names") {
		t.Fatalf("asked %q", asked)
	}
	values, err := Read(path, password)
	if err != nil || !reflect.DeepEqual(values, map[string]any{"token": "old-value"}) {
		t.Fatalf("got %v, %v", values, err)
	}
	if now, _ := os.ReadFile(path); string(now) == string(original) {
		t.Fatal("the corrected edit was not saved")
	}
	tmp, _ := os.ReadFile(marker)
	if _, err := os.Stat(filepath.Dir(string(tmp))); !os.IsNotExist(err) {
		t.Fatal("editor plaintext directory remains")
	}
}

// Without a way to ask, a rejected edit fails and says nothing was saved.
func TestRejectedEditWithoutAskingSaysNothingWasSaved(t *testing.T) {
	const password = "test editor passphrase"
	path := filepath.Join(t.TempDir(), "secrets.age")
	if err := Write(path, password, []byte("token: old-value\n")); err != nil {
		t.Fatal(err)
	}
	original, _ := os.ReadFile(path)
	editor := filepath.Join(t.TempDir(), "editor")
	writeProgramFileThroughShellChild(t, editor, "#!/bin/sh\nprintf 'token: one\\ntoken: two\\n' > \"$1\"\n")
	err := Edit(context.Background(), path, password, editor, nil)
	if err == nil || !strings.Contains(err.Error(), "nothing was saved") {
		t.Fatal(err)
	}
	if now, _ := os.ReadFile(path); string(now) != string(original) {
		t.Fatal("failed edit replaced vault")
	}
}

// A passphrase file that group or others can read gets a warning on standard
// error that names the file and the chmod command; the passphrase still works.
func TestPassphraseFileReadableByOthersWarns(t *testing.T) {
	for mode, wantWarning := range map[os.FileMode]bool{0600: false, 0400: false, 0640: true, 0604: true, 0644: true} {
		path := filepath.Join(t.TempDir(), "passphrase")
		if err := os.WriteFile(path, []byte("test passphrase\n"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		var warnings strings.Builder
		store := &Store{PasswordFile: path, Warn: &warnings}
		got, err := store.Password(false)
		if err != nil || got != "test passphrase" {
			t.Fatalf("mode %o: %q, %v", mode, got, err)
		}
		if has := strings.Contains(warnings.String(), "chmod 600 "+path); has != wantWarning || (wantWarning && !strings.Contains(warnings.String(), "readable by group or others")) {
			t.Errorf("mode %o: warning %q", mode, warnings.String())
		}
	}
}
