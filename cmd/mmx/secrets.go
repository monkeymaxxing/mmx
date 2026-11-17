package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"mmx/internal/access"
	"mmx/internal/config"
	"mmx/internal/secrets"
)

const secretsHelp = `mmx secrets edit | passwd | add-random KEY [NAME ...] | set KEY [NAME ...] | show KEY [NAME ...] | pipe-to KEY [NAME ...] -- COMMAND [ARGUMENT ...] | list-keys | diff-keys | usage [KEY]

Options: --inventory NAME  --vault PATH (not for usage)  --vault-password-file PATH
Passphrase change: --new-vault-password-file PATH
Length of the value that add-random generates: --length N (default 32)
Value for set: standard input, such as some_command | mmx secrets set KEY
A name inside a key: one word for each level, such as mmx secrets set mmwatchdog_secret_files smtp-password
A key the vault already has: set and add-random change its value only with --replace
show prints one value, only to a terminal; clear the terminal afterwards
pipe-to writes one value to COMMAND's standard input; never use a COMMAND that prints its input, such as cat
Automation: --non-interactive  --json`

// secretsOptions are the options and arguments of one mmx secrets command.
type secretsOptions struct {
	action          string
	vault           string
	inventory       string
	passwordFile    string
	newPasswordFile string
	nonInteractive  bool
	asJSON          bool
	// length is the length of the value that add-random generates.
	length int
	// replace lets set and add-random change the value of a key the vault
	// already has.
	replace bool
	// arguments are the words that are not options, such as add-random's KEY
	// and the NAME words inside it.
	arguments []string
	// command is pipe-to's COMMAND and its arguments, the words after --.
	command []string
}

func secretCommand(ctx context.Context, args []string) (int, error) {
	if len(args) == 0 || args[0] == "--help" || args[0] == "help" {
		fmt.Println(secretsHelp)
		return 0, nil
	}
	o := secretsOptions{action: args[0]}
	switch o.action {
	case "edit", "passwd", "add-random", "set", "show", "pipe-to", "list-keys", "diff-keys", "usage":
	default:
		return 4, fmt.Errorf("unknown secrets command %q", o.action)
	}
	fs := flag.NewFlagSet("secrets "+o.action, flag.ContinueOnError)
	fs.StringVar(&o.inventory, "inventory", "", "inventory whose vault setting applies, replacing the project's")
	fs.StringVar(&o.passwordFile, "vault-password-file", "", "file on the controller machine containing the vault passphrase")
	fs.BoolVar(&o.nonInteractive, "non-interactive", false, "do not prompt")
	fs.BoolVar(&o.asJSON, "json", false, "structured output; do not prompt")
	if o.action != "usage" {
		fs.StringVar(&o.vault, "vault", "", "encrypted vault file (default: project selection or secrets_vault.age)")
	}
	if o.action == "edit" || o.action == "passwd" {
		fs.StringVar(&o.newPasswordFile, "new-vault-password-file", "", "file containing the new passphrase, for passwd")
	}
	if o.action == "add-random" {
		fs.IntVar(&o.length, "length", secrets.DefaultRandomValueLength, "length of the generated value, in characters")
	}
	if o.action == "set" || o.action == "add-random" {
		fs.BoolVar(&o.replace, "replace", false, "replace the value of a key the vault already has")
	}
	options := args[1:]
	// pipe-to's COMMAND comes after --, with options of its own.
	if o.action == "pipe-to" {
		separator := slices.Index(options, "--")
		if separator < 0 || separator == len(options)-1 {
			return 4, pipeToUsage()
		}
		options, o.command = options[:separator], options[separator+1:]
	}
	// Accept a key before, between, or after the options.
	for {
		if err := fs.Parse(options); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return 0, nil
			}
			return 4, err
		}
		if fs.NArg() == 0 {
			break
		}
		o.arguments, options = append(o.arguments, fs.Arg(0)), fs.Args()[1:]
	}
	if err := checkPasswordFileOptions(fs); err != nil {
		return 4, err
	}
	switch o.action {
	case "add-random":
		return secretsAddRandom(ctx, o)
	case "set":
		return secretsSet(ctx, o)
	case "show":
		return secretsShow(ctx, o)
	case "pipe-to":
		return secretsPipeTo(ctx, o)
	case "list-keys":
		return secretsListKeys(ctx, o)
	case "diff-keys":
		return secretsDiffKeys(ctx, o)
	case "usage":
		return secretsUsage(ctx, o)
	}
	if len(o.arguments) != 0 {
		return 4, fmt.Errorf("mmx secrets edit and mmx secrets passwd take no positional arguments")
	}
	if o.action == "edit" && o.newPasswordFile != "" {
		return 4, fmt.Errorf("--new-vault-password-file is only accepted by mmx secrets passwd")
	}
	vault, err := o.vaultFile()
	if err != nil {
		return 4, err
	}
	store := o.store(ctx)
	_, statErr := os.Stat(vault)
	if statErr != nil && !errors.Is(statErr, os.ErrNotExist) {
		return 4, statErr
	}
	if o.action == "passwd" && statErr != nil {
		return 4, vaultDoesNotExist(vault)
	}
	password, err := store.Password(errors.Is(statErr, os.ErrNotExist))
	if err != nil {
		return secretsFailure(err)
	}
	if o.action == "edit" {
		err = secrets.Edit(ctx, vault, password, "", editAgainAtTerminal(store.Interactive, os.Stdin, os.Stderr))
	} else {
		var next string
		next, err = store.NewPassword(o.newPasswordFile)
		if err == nil {
			err = secrets.ChangePassword(vault, password, next)
		}
	}
	if err != nil {
		return secretsFailure(err)
	}
	return o.saved(vault, "Saved encrypted vault: "+displayPath(vault))
}

// vaultFile is the vault the command works on: --vault, else the vault that
// the selected inventory or the project selects.
func (o secretsOptions) vaultFile() (string, error) {
	if o.vault != "" {
		return o.vault, nil
	}
	abs, err := config.FindProject(".")
	if err != nil {
		return "", err
	}
	project, inventory, err := config.Load(abs, "", o.inventory, nil)
	if err != nil {
		return "", err
	}
	return config.VaultPath(abs, project, inventory.Path, inventory), nil
}

// store reads the vault passphrase from the sources every vault command
// shares. JSON and noninteractive commands never prompt.
func (o secretsOptions) store(ctx context.Context) *secrets.Store {
	credentials := &access.Credentials{Context: ctx}
	return &secrets.Store{PasswordFile: o.passwordFile, Interactive: !o.nonInteractive && !o.asJSON && access.IsTerminal(int(os.Stdin.Fd())), Prompt: credentials.ReadPassword}
}

// saved reports a vault that edit, passwd, add-random, or set replaced. JSON
// holds only the vault's path and the status; people read line, and set,
// whose line is empty, prints nothing.
func (o secretsOptions) saved(vault, line string) (int, error) {
	if o.asJSON {
		return encode(map[string]any{"vault": vault, "status": "succeeded"})
	}
	if line != "" {
		fmt.Println(line)
	}
	return 0, nil
}

// vaultDoesNotExist says so for a command that needs a vault that is there.
// A file that a secrets command names in output for people goes through
// displayPath; JSON keeps the path as the command has it.
func vaultDoesNotExist(vault string) error {
	return fmt.Errorf("the vault %s does not exist; create it with mmx secrets edit, mmx secrets add-random KEY, or mmx secrets set KEY", displayPath(vault))
}

// environmentBindings returns the project's secrets_from_env: for
// each bound key the name of its environment variable. A command that names
// its vault with --vault outside a project has no project to ask.
func (o secretsOptions) environmentBindings() (map[string]string, error) {
	settings, err := config.FindProject(".")
	if err != nil {
		if o.vault != "" {
			return nil, nil
		}
		return nil, err
	}
	project, err := config.ReadSettings(settings)
	if err != nil {
		return nil, err
	}
	return project.Secrets, nil
}

// refuseKeyFromEnvironment refuses a secret key that secrets_from_env binds
// to an environment variable: a run reads that key from the variable, so
// the vault's value for it would never be read.
func (o secretsOptions) refuseKeyFromEnvironment(key string) error {
	bindings, err := o.environmentBindings()
	if err != nil {
		return err
	}
	if variable, bound := bindings[key]; bound {
		return fmt.Errorf("secret key %s is bound to environment variable %s by secrets_from_env in mmx_project_settings.yml. MMX reads %s from the environment instead of the vault. Remove the secrets_from_env entry or choose a different secret key", key, variable, key)
	}
	return nil
}

// existingVault opens the vault of a command that only reads: it returns the
// vault's file and its decoded keys and values. The caller prints keys only.
func (o secretsOptions) existingVault(ctx context.Context) (string, string, map[string]any, error) {
	vault, err := o.vaultFile()
	if err != nil {
		return "", "", nil, err
	}
	if _, err := os.Stat(vault); errors.Is(err, os.ErrNotExist) {
		return "", "", nil, vaultDoesNotExist(vault)
	} else if err != nil {
		return "", "", nil, err
	}
	password, err := o.store(ctx).Password(false)
	if err != nil {
		return "", "", nil, err
	}
	values, err := secrets.Read(vault, password)
	return vault, password, values, err
}

// secretsFailure is a secrets command that could not do its job: exit code
// 4, or 130 after Ctrl-C.
func secretsFailure(err error) (int, error) {
	if errors.Is(err, context.Canceled) {
		return 130, nil
	}
	return 4, err
}

func checkPasswordFileOptions(fs *flag.FlagSet) error {
	var err error
	fs.Visit(func(f *flag.Flag) {
		if (f.Name == "vault-password-file" || f.Name == "new-vault-password-file") && f.Value.String() == "" {
			err = fmt.Errorf("--%s requires a nonempty path", f.Name)
		}
	})
	return err
}

// editAgainAtTerminal asks, as visudo does, whether to reopen the editor with
// the text the vault rejected, so a typing mistake does not lose the edit. It
// returns nil without a terminal: nobody can answer, and the command fails.
func editAgainAtTerminal(interactive bool, in io.Reader, out io.Writer) func(error) bool {
	if !interactive {
		return nil
	}
	reader := bufio.NewReader(in)
	return func(rejection error) bool {
		fmt.Fprintf(out, "mmx: %v\nEdit again? [Y/n] ", rejection)
		answer, _ := reader.ReadString('\n')
		switch strings.ToLower(strings.TrimSpace(answer)) {
		case "", "y", "yes":
			return answer != ""
		}
		return false
	}
}
