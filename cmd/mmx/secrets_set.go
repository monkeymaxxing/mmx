package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"mmx/internal/access"
	"mmx/internal/secrets"
)

// secretsSet sets a key of the vault to a value that a command produces,
// so that the value passes through no conversation, command line, or
// editor. It reads the value from standard input, or from a prompt without
// echo when standard input is a terminal. It never prints the value, also
// not in an error: on success it prints nothing, and with --json only the
// vault's path and the status.
func secretsSet(ctx context.Context, o secretsOptions) (int, error) {
	if len(o.arguments) == 0 {
		return 4, fmt.Errorf("usage: some_command | mmx secrets set KEY [NAME ...] [--replace]; KEY is the name of the secret, such as cloudflare_tunnel_token, and each NAME is one level inside it, such as mmx secrets set mmwatchdog_secret_files smtp-password")
	}
	keyPath := o.arguments
	if err := secrets.CheckKeyPath(keyPath); err != nil {
		return 4, err
	}
	key, place := keyPath[0], secrets.KeyPathText(keyPath)
	// A run reads a key that the project binds to an environment variable
	// from that variable, so a value in the vault would never be read.
	bindings, err := o.environmentBindings()
	if err != nil {
		return 4, err
	}
	if variable, bound := bindings[key]; bound {
		return 4, fmt.Errorf("secret key %s is bound to environment variable %s by secrets_from_env in mmx_project_settings.yml. MMX reads %s from the environment instead of the vault. Remove the secrets_from_env entry or choose a different secret key", key, variable, key)
	}
	// A prompt for the value needs a terminal on standard input, and
	// --non-interactive and --json never prompt.
	prompt := access.IsTerminal(int(os.Stdin.Fd()))
	if prompt && (o.nonInteractive || o.asJSON) {
		option := "--non-interactive"
		if o.asJSON {
			option = "--json"
		}
		return 4, fmt.Errorf("standard input is a terminal, and %s turns off the prompt for the value; give the value on standard input, such as some_command | mmx secrets set %s", option, place)
	}
	vault, err := o.vaultFile()
	if err != nil {
		return 4, err
	}
	_, statErr := os.Stat(vault)
	if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return 4, statErr
	}
	// A project that has no vault yet gets one, with a new passphrase taken
	// as mmx secrets edit takes it.
	password, err := o.store(ctx).Password(errors.Is(statErr, os.ErrNotExist))
	if err != nil {
		return secretsFailure(err)
	}
	value := func() (string, error) {
		if prompt {
			credentials := &access.Credentials{Context: ctx}
			typed, err := credentials.ReadPassword("Value of " + place + ": ")
			if err != nil {
				return "", err
			}
			return valueOf(typed, place)
		}
		read, err := io.ReadAll(io.LimitReader(os.Stdin, secrets.MaxVaultBytes+1))
		if err != nil {
			return "", fmt.Errorf("read the value for %s from standard input: %w", place, err)
		}
		if len(read) > secrets.MaxVaultBytes {
			return "", fmt.Errorf("the value for %s on standard input is larger than the vault, which holds at most 4 MiB", place)
		}
		return valueOf(string(read), place)
	}
	if err := secrets.SetKey(vault, password, keyPath, o.replace, value); err != nil {
		return secretsFailure(err)
	}
	return o.saved(vault, "")
}

// valueOf is the value that input gives, for the secret that place names: without exactly one final line
// ending, "\n" or "\r\n", so that echo and printf %s give the same value.
// An empty value is refused: it is almost always a command that printed
// nothing.
func valueOf(input, place string) (string, error) {
	value, cut := strings.CutSuffix(input, "\n")
	if cut {
		value = strings.TrimSuffix(value, "\r")
	}
	if value == "" {
		return "", fmt.Errorf("the value for %s is empty; mmx secrets set needs a value on standard input, such as some_command | mmx secrets set %s", place, place)
	}
	return value, nil
}
