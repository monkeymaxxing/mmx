package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"

	"mmx/internal/secrets"
)

// secretsPipeTo writes one secret's value to the standard input of a
// program, such as openssl or ssh, so that the value goes from the vault to
// the program without passing through a terminal, a file, or a coding
// agent's conversation. MMX only reads the vault. The program's output
// passes through, and MMX exits with the program's exit code.
func secretsPipeTo(ctx context.Context, o secretsOptions) (int, error) {
	if len(o.arguments) == 0 {
		return 4, pipeToUsage()
	}
	if o.asJSON {
		return 4, fmt.Errorf("mmx secrets pipe-to has no --json: the output is the output of %s", o.command[0])
	}
	if err := o.refuseKeyFromEnvironment(o.arguments[0]); err != nil {
		return 4, err
	}
	// A program that is not there fails before the vault is opened, so
	// nobody types a passphrase for nothing.
	program, err := exec.LookPath(o.command[0])
	if errors.Is(err, exec.ErrNotFound) || errors.Is(err, os.ErrNotExist) {
		return 4, fmt.Errorf("cannot start %s: no such program; check the program's name, or the PATH environment variable", o.command[0])
	}
	if err != nil {
		return 4, fmt.Errorf("cannot start %s: %w", o.command[0], err)
	}
	_, _, values, err := o.existingVault(ctx)
	if err != nil {
		return secretsFailure(err)
	}
	value, err := secrets.ValueAt(values, o.arguments)
	if err != nil {
		return 4, err
	}
	// The value ends with one line ending, as a person types it, unless it
	// has one: a program such as openssl -pass stdin reads one line, and a
	// private key keeps its bytes.
	if !strings.HasSuffix(value, "\n") {
		value += "\n"
	}
	command := exec.CommandContext(ctx, program, o.command[1:]...)
	command.Args[0] = o.command[0]
	command.Stdin, command.Stdout, command.Stderr = strings.NewReader(value), os.Stdout, os.Stderr
	err = command.Run()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return 0, nil
	case ctx.Err() != nil:
		return 130, nil
	case errors.As(err, &exit):
		// A program that a signal ended exits as a shell reports it: 128
		// and the signal's number.
		if status, ok := exit.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			return 128 + int(status.Signal()), nil
		}
		return exit.ExitCode(), nil
	}
	return 4, fmt.Errorf("cannot start %s: %w", o.command[0], err)
}

func pipeToUsage() error {
	return fmt.Errorf("usage: mmx secrets pipe-to KEY [NAME ...] -- COMMAND [ARGUMENT ...]; MMX writes the value of the secret to COMMAND's standard input, such as mmx secrets pipe-to backup_passphrase -- openssl enc -d -aes-256-cbc -pbkdf2 -pass stdin -in backup.tar.enc -out backup.tar")
}
