package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"mmx/internal/secrets"
)

// secretsDiffKeys compares the vault file in the working directory with the
// same file in the last Git commit and prints which keys were added,
// removed, or changed, and never a value. It only reads the vault. It exits
// 0 whether or not a key differs: the three lists are its answer.
func secretsDiffKeys(ctx context.Context, o secretsOptions) (int, error) {
	if len(o.arguments) != 0 {
		return 4, fmt.Errorf("mmx secrets diff-keys takes no arguments")
	}
	vault, password, current, err := o.existingVault(ctx)
	if err != nil {
		return secretsFailure(err)
	}
	encrypted, err := committedVault(ctx, vault)
	if err != nil {
		return secretsFailure(err)
	}
	committed, err := secrets.ReadEncrypted(encrypted, password)
	if err != nil {
		return 4, fmt.Errorf("the last committed version of vault %s cannot be opened with the supplied passphrase (%w). The committed vault needs a different passphrase or is invalid. Commit the current vault so mmx secrets diff-keys can compare later edits with the new commit", displayPath(vault), err)
	}
	added, removed, changed := secrets.CompareKeys(committed, current)
	if o.asJSON {
		return encode(struct {
			SchemaVersion int      `json:"schema_version"`
			Vault         string   `json:"vault"`
			Added         []string `json:"added"`
			Removed       []string `json:"removed"`
			Changed       []string `json:"changed"`
		}{1, vault, added, removed, changed})
	}
	fmt.Printf("Vault: %s, compared with the last commit\n\n", displayPath(vault))
	for _, list := range []struct {
		title string
		keys  []string
	}{{"Added:", added}, {"Removed:", removed}, {"Changed:", changed}} {
		keys := "(none)"
		if len(list.keys) > 0 {
			keys = strings.Join(list.keys, ", ")
		}
		fmt.Printf("%-9s %s\n", list.title, keys)
	}
	return 0, nil
}

// committedVault returns the encrypted bytes of the vault file as the last
// Git commit holds it. It runs the git program in the vault's directory, so
// it reads the repository that the vault is in. Each reason for which there
// is no committed version has its own message.
func committedVault(ctx context.Context, vault string) ([]byte, error) {
	program, err := exec.LookPath("git")
	if err != nil {
		return nil, fmt.Errorf("mmx secrets diff-keys reads the vault of the last Git commit with the git program, and git is not installed on this machine (no git in PATH); install git")
	}
	directory, file, name := filepath.Dir(vault), filepath.Base(vault), displayPath(vault)
	// git answers in English, so that its message for "not a repository" is known.
	git := func(arguments ...string) (stdout []byte, stderr string, err error) {
		command := exec.CommandContext(ctx, program, arguments...)
		command.Dir = directory
		command.Env = append(os.Environ(), "LC_ALL=C")
		var captured bytes.Buffer
		command.Stderr = &captured
		stdout, err = command.Output()
		if ctx.Err() != nil {
			return nil, "", ctx.Err()
		}
		return stdout, strings.TrimSpace(captured.String()), err
	}
	failure := func(err error, said string) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("git could not read the repository of the vault %s: %v: %s", name, err, said)
	}
	if inside, said, err := git("rev-parse", "--is-inside-work-tree"); err != nil || strings.TrimSpace(string(inside)) != "true" {
		if err != nil && !strings.Contains(said, "not a git repository") {
			return nil, failure(err, said)
		}
		return nil, fmt.Errorf("the vault %s is not in a Git repository, so there is no last commit to compare with", name)
	}
	if _, _, err := git("rev-parse", "--verify", "--quiet", "HEAD"); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("the Git repository of the vault %s has no commit yet, so there is no last commit to compare with; commit the vault, and mmx secrets diff-keys then compares later edits with that commit", name)
	}
	committed := "HEAD:./" + file
	if _, _, err := git("cat-file", "-e", committed); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("the last Git commit does not contain the vault %s, so the vault has no committed version to compare with; commit the vault first", name)
	}
	encrypted, said, err := git("show", committed)
	if err != nil {
		return nil, failure(err, said)
	}
	return encrypted, nil
}
