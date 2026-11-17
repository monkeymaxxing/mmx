package main

import (
	"context"
	"errors"
	"fmt"
	"os"

	"mmx/internal/secrets"
)

// secretsAddRandom adds a key with a generated value to the vault, or with
// --replace gives an existing key a new one. It never displays the value:
// it names the key, the vault, and the value's length, and with --json it
// prints only the vault's path and the status, as edit and passwd do.
func secretsAddRandom(ctx context.Context, o secretsOptions) (int, error) {
	if len(o.arguments) == 0 {
		return 4, fmt.Errorf("usage: mmx secrets add-random KEY [NAME ...] [--length N] [--replace]; KEY is the name of the secret, such as session_cookie_secret, and each NAME is one level inside it, such as mmx secrets add-random web_app session_cookie_secret")
	}
	keyPath := o.arguments
	if err := secrets.CheckKeyPath(keyPath); err != nil {
		return 4, err
	}
	key, place := keyPath[0], secrets.KeyPathText(keyPath)
	if o.length < 1 {
		return 4, fmt.Errorf("--length must be a positive integer, such as --length 48; got %d", o.length)
	}
	if err := o.refuseKeyFromEnvironment(key); err != nil {
		return 4, err
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
	replaced, err := secrets.AddRandomKey(vault, password, keyPath, o.length, o.replace)
	if err != nil {
		return secretsFailure(err)
	}
	if replaced {
		return o.saved(vault, fmt.Sprintf("Replaced the value of the key %s in the vault %s with a random value of %d letters and digits. MMX does not show the value.", place, displayPath(vault), o.length))
	}
	return o.saved(vault, fmt.Sprintf("Added the key %s to the vault %s, with a random value of %d letters and digits. MMX does not show the value.", place, displayPath(vault), o.length))
}
