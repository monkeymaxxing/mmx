// Package secrets owns controller-side secret sources and the age project file.
package secrets

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"mmx/internal/config"
	"mmx/internal/durable"
	"mmx/internal/modules"

	"filippo.io/age"
	"go.yaml.in/yaml/v3"
)

// MaxVaultBytes is the most text a vault holds: 4 MiB.
const MaxVaultBytes = 4 * 1024 * 1024

type Store struct {
	Vault        string
	Inputs       map[string]string
	PasswordFile string
	Interactive  bool
	Prompt       func(string) (string, error)
	// Warn receives warnings; nil means standard error.
	Warn     io.Writer
	mu       sync.Mutex
	password string
	values   map[string]any
	loaded   bool
}

func passwordFile(path string, warn io.Writer) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("read vault password file %s: %w", path, err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("vault password file must be a regular file")
	}
	// A warning, not a refusal: a warning is a default that a team may ignore,
	// and a refusal would need a stronger reason, such as a shared secrets
	// volume whose mode MMX cannot change.
	if info.Mode().Perm()&0o077 != 0 {
		if warn == nil {
			warn = os.Stderr
		}
		fmt.Fprintf(warn, "mmx: warning: the vault password file %s is readable by group or others; run: chmod 600 %s\n", path, path)
	}
	b, err := io.ReadAll(io.LimitReader(f, 16385))
	if err != nil {
		return "", err
	}
	if len(b) > 16384 {
		return "", fmt.Errorf("vault password file exceeds 16 KiB")
	}
	password := string(b)
	if strings.HasSuffix(password, "\n") {
		password = strings.TrimSuffix(strings.TrimSuffix(password, "\n"), "\r")
	}
	if password == "" || strings.ContainsAny(password, "\r\n\x00") {
		return "", fmt.Errorf("vault password file must contain one nonempty passphrase")
	}
	return password, nil
}

// newVaultPassphrasePrompt asks for the passphrase of a vault that does not
// exist yet. The person chooses it now, and a lost one cannot be recovered.
const newVaultPassphrasePrompt = "You are creating a new vault. Choose its passphrase now, and keep it safe, such as in a password manager.\n" +
	"Nobody can open the vault without the passphrase, and MMX cannot recover a lost one.\n" +
	"New vault passphrase: "

// Password returns the vault passphrase; confirm is for a new vault, whose
// passphrase the prompt asks for twice.
func (s *Store) Password(confirm bool) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.password != "" {
		return s.password, nil
	}
	path := s.PasswordFile
	if path == "" {
		if env, ok := os.LookupEnv("MMX_VAULT_PASSWORD_FILE"); ok {
			if env == "" {
				return "", fmt.Errorf("MMX_VAULT_PASSWORD_FILE names an empty path")
			}
			path = env
		}
	}
	var password string
	var err error
	switch {
	case path != "":
		password, err = passwordFile(path, s.Warn)
	default:
		if v, ok := os.LookupEnv("MMX_VAULT_PASSWORD"); ok {
			password = v
		} else {
			if !s.Interactive || s.Prompt == nil {
				return "", fmt.Errorf("vault passphrase required; use --vault-password-file, MMX_VAULT_PASSWORD_FILE, or MMX_VAULT_PASSWORD (prompts are disabled)")
			}
			label := "Vault passphrase: "
			if confirm {
				label = newVaultPassphrasePrompt
			}
			password, err = s.Prompt(label)
			if err == nil && confirm {
				var again string
				again, err = s.Prompt("Confirm new vault passphrase: ")
				if err == nil && password != again {
					err = fmt.Errorf("vault passphrases do not match")
				}
			}
		}
	}
	if err != nil {
		return "", err
	}
	if password == "" || strings.ContainsAny(password, "\r\n\x00") {
		return "", fmt.Errorf("vault passphrase must be nonempty and contain no line breaks")
	}
	s.password = password
	return password, nil
}
func (s *Store) NewPassword(path string) (string, error) {
	if path != "" {
		return passwordFile(path, s.Warn)
	}
	if !s.Interactive || s.Prompt == nil {
		return "", fmt.Errorf("new vault passphrase required; use --new-vault-password-file (prompts are disabled)")
	}
	p, err := s.Prompt("New vault passphrase: ")
	if err != nil {
		return "", err
	}
	q, err := s.Prompt("Confirm new vault passphrase: ")
	if err != nil {
		return "", err
	}
	if p == "" || strings.ContainsAny(p, "\r\n\x00") || p != q {
		return "", fmt.Errorf("new vault passphrases are empty, invalid, or do not match")
	}
	return p, nil
}
func (s *Store) Get(name string) (any, error) {
	if input, ok := s.Inputs[name]; ok {
		v, ok := os.LookupEnv(input)
		if !ok || v == "" {
			return nil, fmt.Errorf("secret %s requires a nonempty environment variable %s", name, input)
		}
		return v, nil
	}
	if _, err := os.Stat(s.Vault); err != nil {
		return nil, fmt.Errorf("read vault: %w", err)
	}
	password, err := s.Password(false)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.loaded {
		values, err := Read(s.Vault, password)
		if err != nil {
			return nil, err
		}
		s.values = values
		s.loaded = true
	}
	v, ok := s.values[name]
	if !ok {
		return nil, fmt.Errorf("vault is missing required secret %s", name)
	}
	return v, nil
}

// withoutYAMLDocument reports a text that holds no YAML document, such as one
// of only comments: a vault without a secret key.
func withoutYAMLDocument(plaintext []byte) bool {
	var document yaml.Node
	return yaml.Unmarshal(plaintext, &document) == nil && document.Kind == 0
}
func decode(plaintext []byte) (map[string]any, error) {
	if withoutYAMLDocument(plaintext) {
		return map[string]any{}, nil
	}
	var values map[string]any
	// YAML decoder diagnostics can quote values. Keep secret document errors out
	// of ordinary diagnostics, including errors from duplicate mapping keys.
	if err := config.Decode("vault", plaintext, &values); err != nil || values == nil {
		return nil, fmt.Errorf("vault must contain one YAML mapping with unique secret names")
	}
	for name := range values {
		if !modules.Name.MatchString(name) {
			return nil, fmt.Errorf("vault contains an invalid secret name")
		}
	}
	return values, nil
}
func decrypt(path, password string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("read vault: %w", err)
	}
	defer f.Close()
	return decryptFrom(f, password)
}

// decryptFrom decrypts a vault's encrypted bytes, from the vault file or
// from another copy of it, such as the version in the last Git commit.
func decryptFrom(encrypted io.Reader, password string) ([]byte, error) {
	id, err := age.NewScryptIdentity(password)
	if err != nil {
		return nil, fmt.Errorf("invalid vault passphrase")
	}
	reader, err := age.Decrypt(io.LimitReader(encrypted, MaxVaultBytes+64*1024), id)
	if err != nil {
		return nil, fmt.Errorf("cannot decrypt vault: wrong passphrase or invalid age file")
	}
	b, err := io.ReadAll(io.LimitReader(reader, MaxVaultBytes+1))
	if err != nil {
		return nil, fmt.Errorf("cannot decrypt vault: damaged or truncated age file")
	}
	if len(b) > MaxVaultBytes {
		return nil, fmt.Errorf("vault exceeds 4 MiB")
	}
	return b, nil
}
func Read(path, password string) (map[string]any, error) {
	b, err := decrypt(path, password)
	if err != nil {
		return nil, err
	}
	return decode(b)
}
func Write(path, password string, plaintext []byte) error {
	if len(plaintext) > MaxVaultBytes {
		return fmt.Errorf("vault exceeds 4 MiB")
	}
	if _, err := decode(plaintext); err != nil {
		return err
	}
	recipient, err := age.NewScryptRecipient(password)
	if err != nil {
		return fmt.Errorf("invalid vault passphrase")
	}
	var encrypted bytes.Buffer
	writer, err := age.Encrypt(&encrypted, recipient)
	if err != nil {
		return fmt.Errorf("encrypt vault: %w", err)
	}
	if _, err = writer.Write(plaintext); err != nil {
		return fmt.Errorf("encrypt vault: %w", err)
	}
	if err = writer.Close(); err != nil {
		return fmt.Errorf("finish encrypting vault: %w", err)
	}
	return durable.Write(path, encrypted.Bytes(), 0600)
}
func ChangePassword(path, old, new string) error {
	plaintext, err := decrypt(path, old)
	if err != nil {
		return err
	}
	return Write(path, new, plaintext)
}

// newVaultText is what the editor shows for a vault that does not exist yet.
// Its first line is an uncommented example, so a line typed below it is not
// turned into a comment by an editor that continues comments, as vim does
// for YAML. Saved unchanged, the example is a secret key that nothing reads:
// harmless, and mmx secrets usage lists it as never referenced.
const newVaultText = "example_secret_key: \"replace this line with your own secret key and value\"\n" +
	"# Easier for one secret: close this editor and run mmx secrets set KEY. It asks for the value and hides what you type.\n" +
	"# To save and close: in vim, press Esc, type :wq, and press Enter. In other editors, save and close.\n"

// Edit opens the vault's text in an editor. When the edited text is rejected,
// editAgain (nil: never) is told why and says whether to reopen the editor
// with that text, as visudo does; the text stays in the editor's temporary file.
func Edit(ctx context.Context, path, password, editor string, editAgain func(rejection error) bool) error {
	plaintext, err := decrypt(path, password)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		plaintext = []byte(newVaultText)
	}
	dir, err := os.MkdirTemp("", "mmx-vault-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	temporary := filepath.Join(dir, "secrets.yml")
	if err := os.WriteFile(temporary, plaintext, 0600); err != nil {
		return err
	}
	if editor == "" {
		editor = os.Getenv("VISUAL")
	}
	if editor == "" {
		editor = os.Getenv("EDITOR")
	}
	if editor == "" {
		editor = "vi"
	}
	for {
		// Keep the editor in the foreground terminal group so interactive editors
		// can read input. Exec replaces the shell, letting cancellation kill the
		// editor itself before the private temporary directory is removed.
		command := exec.CommandContext(ctx, "/bin/sh", "-c", "exec "+editor+` "$1"`, "mmx-editor", temporary)
		command.Stdin, command.Stdout, command.Stderr = os.Stdin, os.Stdout, os.Stderr
		command.WaitDelay = time.Second
		if err := command.Run(); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("vault editor did not finish successfully; the encrypted vault file is unchanged")
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		plaintext, err = os.ReadFile(temporary)
		if err != nil {
			return fmt.Errorf("read edited vault: %w", err)
		}
		err := Write(path, password, plaintext)
		if err == nil {
			return nil
		}
		if editAgain != nil && editAgain(err) {
			continue
		}
		return fmt.Errorf("%w; nothing was saved and the encrypted vault file is unchanged", err)
	}
}
