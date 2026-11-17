package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"mmx/internal/access"
	"mmx/internal/secrets"
)

// secretsShow prints one secret's value, for a person who needs to read it
// once, such as to store a passphrase in a password manager. It prints only
// to a terminal, so that the value goes to a person's screen and not into a
// file, a pipe, or a coding agent's conversation.
func secretsShow(ctx context.Context, o secretsOptions) (int, error) {
	if len(o.arguments) == 0 {
		return 4, fmt.Errorf("usage: mmx secrets show KEY [NAME ...]; KEY is the name of the secret, such as database_password, and each NAME is one level inside it, such as mmx secrets show mmwatchdog_secret_files smtp-password")
	}
	if o.asJSON || !access.IsTerminal(int(os.Stdout.Fd())) {
		return 4, fmt.Errorf("mmx secrets show prints a secret only to a terminal. Run it yourself in a terminal, not through your coding agent")
	}
	if err := o.refuseKeyFromEnvironment(o.arguments[0]); err != nil {
		return 4, err
	}
	_, _, values, err := o.existingVault(ctx)
	if err != nil {
		return secretsFailure(err)
	}
	value, err := secrets.ValueAt(values, o.arguments)
	if err != nil {
		return 4, err
	}
	if !strings.HasSuffix(value, "\n") {
		value += "\n"
	}
	fmt.Print(value)
	fmt.Println("Clear your terminal afterwards with `clear`.")
	return 0, nil
}
