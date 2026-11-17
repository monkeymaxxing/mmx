package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"mmx/internal/access"
	"mmx/internal/config"
	"mmx/internal/durable"
	"mmx/internal/secrets"
)

// secretsWritePassphraseFile asks a person for the vault passphrase and
// writes it to a file outside the project, so that the person's coding agent
// can run mmx with MMX_VAULT_PASSWORD_FILE and the passphrase never appears
// in the agent's conversation. It runs only at a terminal, where the person
// types the passphrase, and it never prints the passphrase.
//
// For a vault that exists, it writes the file only after the passphrase
// opens the vault. With no vault yet, it creates an empty vault with the new
// passphrase, so that the first mmx secrets set uses the file's passphrase
// instead of asking for another new one.
func secretsWritePassphraseFile(ctx context.Context, o secretsOptions) (int, error) {
	if len(o.arguments) != 0 {
		return 4, fmt.Errorf("mmx secrets write-passphrase-file takes no arguments")
	}
	if !access.IsTerminal(int(os.Stdin.Fd())) || !access.IsTerminal(int(os.Stdout.Fd())) {
		return 4, fmt.Errorf("mmx secrets write-passphrase-file asks for the vault passphrase at a terminal. Run it yourself in your own terminal, not through your coding agent")
	}
	vault, file, err := o.passphraseFile()
	if err != nil {
		return 4, err
	}
	_, statErr := os.Stat(vault)
	if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return 4, statErr
	}
	newVault := errors.Is(statErr, os.ErrNotExist)
	credentials := &access.Credentials{Context: ctx}
	password, err := secrets.AskForPassphrase(credentials.ReadPassword, newVault)
	if err != nil {
		return secretsFailure(err)
	}
	if password == "" || strings.ContainsAny(password, "\r\n\x00") {
		return 4, fmt.Errorf("the vault passphrase must not be empty or contain line breaks. Nothing was written")
	}
	if !newVault {
		if _, err := secrets.Read(vault, password); err != nil {
			return 4, fmt.Errorf("that passphrase does not open the vault %s. Nothing was written. Run mmx secrets write-passphrase-file again and type the vault's passphrase", displayPath(vault))
		}
	}
	_, existsErr := os.Lstat(file)
	replaced := existsErr == nil
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		return 4, err
	}
	if newVault {
		if err := secrets.Write(vault, password, nil); err != nil {
			return secretsFailure(err)
		}
	}
	if err := durable.Write(file, []byte(password+"\n"), 0o600); err != nil {
		return 4, err
	}
	shown := homeRelative(file)
	if newVault {
		fmt.Printf("Created the vault %s with this passphrase. The vault holds no secrets yet.\n", displayPath(vault))
	}
	if replaced {
		fmt.Printf("Replaced the old %s.\n", shown)
	}
	fmt.Printf("Your coding agent can now run mmx with MMX_VAULT_PASSWORD_FILE=%s.\n", shown)
	fmt.Printf("When you are done, delete the file: rm %s\n", shown)
	return 0, nil
}

// passphraseFile returns the selected vault and the file that holds its
// passphrase: ~/.config/mmx/vault-passphrase-PROJECT_NAME for the project's
// own vault. Another vault, such as one that an inventory's vault_file
// names, can have its own passphrase, so its file name adds the vault's file
// name without .age: vault-passphrase-shop-staging_vault.
func (o secretsOptions) passphraseFile() (vault, file string, err error) {
	settings, err := config.FindProject(".")
	if err != nil {
		return "", "", fmt.Errorf("mmx secrets write-passphrase-file names the file after the project; run it in the project directory: %w", err)
	}
	project, inventory, err := config.Load(settings, "", o.inventory, nil)
	if err != nil {
		return "", "", err
	}
	vault = config.VaultPath(settings, project, inventory.Path, inventory)
	if o.vault != "" {
		if vault, err = filepath.Abs(o.vault); err != nil {
			return "", "", err
		}
	}
	// Load replaces the project's vault_file with the selected vault; the
	// settings file alone gives the project's own vault.
	own, err := config.ReadSettings(settings)
	if err != nil {
		return "", "", err
	}
	name := "vault-passphrase-" + project.Project
	if vault != config.VaultPath(settings, own, "", config.Inventory{}) {
		name += "-" + strings.TrimSuffix(filepath.Base(vault), ".age")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", "", err
	}
	return vault, filepath.Join(home, ".config", "mmx", name), nil
}

// homeRelative writes a path in the home directory with ~, as people type it.
func homeRelative(path string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		return path
	}
	if rel, err := filepath.Rel(home, path); err == nil && !strings.HasPrefix(rel, "..") {
		return "~/" + rel
	}
	return path
}
