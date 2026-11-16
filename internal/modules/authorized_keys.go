package modules

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"reflect"
	"strings"

	"mmx/internal/protocol"

	"golang.org/x/crypto/ssh"
)

// lookupUser finds the account whose keys a task manages; tests replace it to
// give the current account a temporary home.
var lookupUser = user.Lookup

// AuthorizedKeys manages <home>/.ssh/authorized_keys. The account controls its
// home, so .ssh and the file are inspected and replaced within pinned
// directories without following links. The endpoint reads the same file for
// each HTTPS connection, so these keys also grant and revoke MMX access.
func (run Runtime) AuthorizedKeys(ctx context.Context, k AuthorizedKeys) (result protocol.Result) {
	if err := ValidateAuthorizedKeys(k); err != nil {
		return protocol.ConfigurationError(err)
	}
	// Written keys arrive resolved and checked by their argument rule; keys
	// from files were checked when they were read. A reference may still
	// repeat a key.
	wanted, err := AuthorizedKeyList(k, k.Keys, nil)
	if err != nil {
		return protocol.ConfigurationError(err)
	}
	// A task that only takes keys away has nothing to do for an account or
	// home that does not exist, so it does not fail after the account is gone.
	removing := k.State == "absent" || (k.KeysNotListed == "remove" && len(wanted) == 0)
	u, err := lookupUser(k.ForUserAccount)
	var unknown user.UnknownUserError
	if removing && errors.As(err, &unknown) {
		return protocol.Success("unchanged")
	}
	if err != nil {
		return run.accountFailure(err)
	}
	group, err := user.LookupGroupId(u.Gid)
	if err != nil {
		return protocol.Failed(fmt.Errorf("primary group of %s: %w", k.ForUserAccount, err))
	}
	dirMetadata, err := metadataFor(nil, 0700, "", Account(u.Username), Account(group.Name))
	if err != nil {
		return protocol.Failed(err)
	}
	home, err := os.OpenRoot(u.HomeDir)
	if os.IsNotExist(err) && removing {
		return protocol.Success("unchanged")
	}
	if os.IsNotExist(err) {
		return run.notYet(fmt.Errorf("home directory %s of account %s does not exist", u.HomeDir, k.ForUserAccount))
	}
	if err != nil {
		return protocol.Failed(err)
	}
	defer home.Close()
	dirInfo, err := entryInfoAt(home, ".ssh", true)
	if err != nil {
		return protocol.Failed(err)
	}
	var dir *os.Root
	var fileInfo os.FileInfo
	current := ""
	if dirInfo != nil {
		if dir, err = openDirectoryAt(home, ".ssh", dirInfo); err != nil {
			return protocol.Failed(err)
		}
		defer dir.Close()
		if fileInfo, err = entryInfoAt(dir, "authorized_keys", false); err != nil {
			return protocol.Failed(err)
		}
		if fileInfo != nil {
			if current, err = readTextAt(dir, "authorized_keys"); err != nil {
				return protocol.Failed(err)
			}
		}
	}
	if k.State == "absent" && fileInfo == nil {
		return protocol.Success("unchanged")
	}
	next, changes, err := planAuthorizedKeys(current, wanted, k.KeysNotListed == "remove", k.State == "absent", k.ReplaceOptionsOfListedKeys)
	if err != nil {
		result := protocol.ConfigurationError(fmt.Errorf("%s: %w", filepath.Join(u.HomeDir, ".ssh", "authorized_keys"), err))
		result.Change = "unchanged"
		return result
	}
	dirPath := filepath.Join(u.HomeDir, ".ssh")
	f := File{Path: filepath.Join(dirPath, "authorized_keys"), Literal: &next, Owner: Account(u.Username), Group: Account(group.Name), Mode: "0600"}
	dirChanged := !dirMetadata.matches(dirInfo)
	if run.Dryrun {
		result = run.fileAt(ctx, f, nil, dir, 0600, nil)
		if dirChanged && result.Status == "succeeded" {
			result.Change = "would_change"
		}
		if len(changes) > 0 && result.Status == "succeeded" {
			result.Value = changes
		}
		return result
	}
	if dirChanged {
		if err := run.Mutation(dirPath); err != nil {
			return protocol.Failed(err)
		}
		if dirInfo == nil {
			if err := createLabeled(ctx, dirPath, selinuxDirectory, func() error { return home.Mkdir(".ssh", 0700) }); err != nil {
				return protocol.Failed(err)
			}
			if dirInfo, err = home.Lstat(".ssh"); err != nil {
				return protocol.Failed(err)
			}
			if dir, err = openDirectoryAt(home, ".ssh", dirInfo); err != nil {
				return protocol.Failed(err)
			}
			defer dir.Close()
		}
		handle, err := dir.Open(".")
		if err == nil {
			_, err = dirMetadata.apply(handle)
			handle.Close()
		}
		if err != nil {
			r := protocol.Failed(err)
			r.Change = "changed"
			return r
		}
	}
	result = run.fileAt(ctx, f, nil, dir, 0600, nil)
	// The keys are in the file once fileAt wrote it, also when a check after
	// the write failed; a file that was not written holds none of them.
	if len(changes) > 0 && result.Change == "changed" {
		result.Value = changes
	}
	if dirChanged && result.Change != "changed" {
		result.Change = "changed"
	}
	return result
}

// planAuthorizedKeys returns the new file text and the keys added, updated,
// and removed. List ownership and replacement of listed keys' options are
// independent. Without replacement, explicit conflicting options refuse the
// whole plan, while plain keys preserve all their existing lines.
func planAuthorizedKeys(current string, wanted []AuthorizedKey, replace, absent, replaceOptions bool) (string, map[string]any, error) {
	listed := map[string]AuthorizedKey{}
	for _, w := range wanted {
		listed[w.ID] = w
	}
	changes := map[string]any{}
	note := func(kind string, k AuthorizedKey) {
		list, _ := changes[kind].([]any)
		changes[kind] = append(list, k.Name())
	}
	found := map[string]bool{}
	preserved := map[string][]string{}
	var lines []string
	if current != "" {
		lines = strings.Split(strings.TrimSuffix(current, "\n"), "\n")
	}
	var out []string
	for _, line := range lines {
		key, err := ParseAuthorizedKey(line)
		w, isListed := listed[key.ID]
		switch {
		case err != nil: // comments, blank lines, and lines this parser does not accept
			if !replace {
				out = append(out, line)
			}
		case !isListed && replace:
			note("removed", key)
		case !isListed:
			out = append(out, line)
		case absent:
			note("removed", key)
		case !replaceOptions:
			if w.Options != "" && w.Options != key.Options {
				name := w.Comment
				if name == "" {
					name = key.Comment
				}
				if name == "" {
					name = w.Type + " " + base64.StdEncoding.EncodeToString([]byte(w.ID))[:32]
				}
				return "", nil, fmt.Errorf("authorized_keys: SSH key %q has options [%s] on the node, but the node task requests options [%s]; to replace the SSH key options, add replace_options_of_listed_keys: true", name, key.Options, w.Options)
			}
			found[key.ID] = true
			preserved[key.ID] = append(preserved[key.ID], line)
			if !replace {
				out = append(out, line)
			}
		case found[key.ID]:
			note("updated", w)
		default:
			found[key.ID] = true
			if strings.TrimSpace(line) != w.Line {
				note("updated", w)
			}
			if !replace {
				out = append(out, w.Line)
			}
		}
	}
	if replace {
		out = nil
	}
	for _, w := range wanted {
		if absent {
			break
		}
		if !found[w.ID] {
			note("added", w)
		}
		if replace && len(preserved[w.ID]) > 0 {
			out = append(out, preserved[w.ID]...)
		} else if replace || !found[w.ID] {
			out = append(out, w.Line)
		}
	}
	text := strings.Join(out, "\n")
	if len(out) > 0 {
		text += "\n"
	}
	return text, changes, nil
}

type AuthorizedKeys struct {
	ReplaceOptionsOfListedKeys bool     `yaml:"replace_options_of_listed_keys,omitempty" json:"replace_options_of_listed_keys,omitempty"`
	ForUserAccount             string   `yaml:"for_user_account" json:"for_user_account"`
	Keys                       []string `yaml:"keys" json:"keys"`
	KeysNotListed              string   `yaml:"keys_not_listed,omitempty" json:"keys_not_listed,omitempty"`
	State                      string   `yaml:"state,omitempty" json:"state,omitempty"`

	// The key files and directories are read on the controller machine as the
	// project loads (ControllerMachineFiles).
	KeyFilesOnControllerMachine       []string `yaml:"key_files_on_controller_machine,omitempty" json:"key_files_on_controller_machine,omitempty" mmx:"controller_machine_files"`
	KeyDirectoriesOnControllerMachine []string `yaml:"key_directories_on_controller_machine,omitempty" json:"key_directories_on_controller_machine,omitempty" mmx:"controller_machine_files"`
	// FileKeys are the key lines read from KeyFilesOnControllerMachine and KeyDirectoriesOnControllerMachine when the
	// project loaded, or, for paths with references, from the node's variant.
	// A run saves them, so mmx resume uses the same keys.
	FileKeys []FileKey `yaml:"-" json:"file_keys,omitempty"`
	// SourceBase and Variants serve paths with references (variable_sources.go).
	SourceBase string                    `yaml:"-" json:"-"`
	Variants   map[string]*SourceVariant `yaml:"-" json:"variants,omitempty"`
}

// FileKey is one key line from a key file, with the place it was read from
// ("key_files_on_controller_machine[0] files/keys/alice.pub line 2") for error messages.
type FileKey struct {
	Line  string `json:"line"`
	Place string `json:"place"`
}

func ValidateAuthorizedKeys(k AuthorizedKeys) error {
	if err := validatePresence(k.State); err != nil {
		return err
	}
	if k.State == "absent" && (k.KeysNotListed != "" || k.ReplaceOptionsOfListedKeys) {
		return fmt.Errorf("authorized_keys with state: absent accepts only for_user_account, keys, key_files_on_controller_machine, key_directories_on_controller_machine, and state")
	}
	if k.Keys == nil && k.KeyFilesOnControllerMachine == nil && k.KeyDirectoriesOnControllerMachine == nil {
		return invalid("keys", "authorized_keys needs keys, key_files_on_controller_machine, or key_directories_on_controller_machine (use keys: [] with keys_not_listed: remove to empty the file)")
	}
	if k.State != "absent" {
		if err := validateNotListed("keys", true, k.KeysNotListed); err != nil {
			return err
		}
	}
	// Written keys are checked by their argument rule; this adds repeats.
	// Keys read from files are checked when they are read.
	_, err := AuthorizedKeyList(k, k.Keys, func(i int, line string) bool { return Dynamic(line) })
	return err
}

// AuthorizedKeyList parses the task's keys, given resolved, and the keys read
// from its files, in that order. A key that appears twice fails, naming both
// places. skip leaves out a written key that is not known yet.
func AuthorizedKeyList(k AuthorizedKeys, keys []string, skip func(int, string) bool) ([]AuthorizedKey, error) {
	var out []AuthorizedKey
	places := map[string]string{}
	add := func(line, place string) error {
		key, err := ParseAuthorizedKey(line)
		if err != nil {
			return err
		}
		if earlier, seen := places[key.ID]; seen {
			return fmt.Errorf("key listed twice: %s, in %s and in %s", key.Name(), earlier, place)
		}
		places[key.ID] = place
		out = append(out, key)
		return nil
	}
	for i, line := range keys {
		if skip != nil && skip(i, line) {
			continue
		}
		if err := add(line, fmt.Sprintf("keys[%d]", i)); err != nil {
			return nil, invalid(fmt.Sprintf("keys[%d]", i), err.Error())
		}
	}
	for _, f := range k.FileKeys {
		if err := add(f.Line, f.Place); err != nil {
			return nil, invalid(strings.Fields(f.Place)[0], err.Error())
		}
	}
	return out, nil
}

// keyPath is one entry of key_files_on_controller_machine or key_directories_on_controller_machine.
type keyPath struct {
	Field     string // key_files_on_controller_machine[0]
	Name      *string
	Directory bool
}

func keyPaths(k *AuthorizedKeys) []keyPath {
	var out []keyPath
	for i := range k.KeyFilesOnControllerMachine {
		out = append(out, keyPath{fmt.Sprintf("key_files_on_controller_machine[%d]", i), &k.KeyFilesOnControllerMachine[i], false})
	}
	for i := range k.KeyDirectoriesOnControllerMachine {
		out = append(out, keyPath{fmt.Sprintf("key_directories_on_controller_machine[%d]", i), &k.KeyDirectoriesOnControllerMachine[i], true})
	}
	return out
}

func keyPathsDynamic(k AuthorizedKeys) bool {
	for _, p := range keyPaths(&k) {
		if Dynamic(*p.Name) {
			return true
		}
	}
	return false
}

// AuthorizedKey is one OpenSSH authorized_keys line. Its identity is the key
// type and base64 body; options and comment belong to the line.
type AuthorizedKey struct {
	Line, ID, Type, Fingerprint, Comment, Options string
}

// Name identifies a key in results: type, SHA-256 fingerprint, and comment.
func (k AuthorizedKey) Name() string {
	return strings.TrimSpace(k.Type + " " + k.Fingerprint + " " + k.Comment)
}

// ParseAuthorizedKey parses one key line with the same parser MMX's HTTPS
// authorization uses.
func ParseAuthorizedKey(line string) (AuthorizedKey, error) {
	line = strings.TrimSpace(line)
	if line == "" || line[0] == '#' || strings.ContainsAny(line, "\r\n") {
		return AuthorizedKey{}, errors.New("must be one OpenSSH key line")
	}
	key, comment, options, rest, err := ssh.ParseAuthorizedKey([]byte(line))
	if err != nil || len(rest) != 0 {
		return AuthorizedKey{}, errors.New("must be one OpenSSH key line: [options] type base64-key [comment]")
	}
	return AuthorizedKey{Line: line, ID: string(key.Marshal()), Type: key.Type(), Fingerprint: ssh.FingerprintSHA256(key), Comment: comment, Options: strings.Join(options, ",")}, nil
}

// authorizedKeysDescription is what mmx describe-module shows for authorized_keys.
var authorizedKeysDescription = Description{
	Name: "authorized_keys", Topic: TopicSSHKeysAndFirewalls, Schema: reflect.TypeOf(AuthorizedKeys{}),
	Summary: "one user's SSH authorized keys in <home>/.ssh/authorized_keys; they also authorize MMX's HTTPS access",
	Fields: []Field{
		{Name: "for_user_account", Required: true, Notes: "the account and its home must exist; with state: absent, or with keys_not_listed: remove and no keys, a missing account is unchanged"},
		{Name: "keys", Notes: "OpenSSH key lines: [options] type base64-key [comment]; a key's identity is its type and base64 key; no key twice; at least one of keys, key_files_on_controller_machine, or key_directories_on_controller_machine"},
		{Name: "key_files_on_controller_machine", Notes: "files on the controller machine, relative to the project root, or to the role's directory inside a role like file.source, each with one or more key lines; blank lines and lines starting with # are skipped; read when the run starts, so mmx resume uses the same keys"},
		{Name: "key_directories_on_controller_machine", Notes: "directories on the controller machine; every file directly inside whose name ends in .pub, in name order, read like key_files_on_controller_machine; a directory without a .pub file fails, since an empty list could lock everyone out (write keys: [] to remove every key); like file.source, key_files_on_controller_machine and key_directories_on_controller_machine may read the node's name, vars, role inputs, and loop items"},
		{Name: "replace_options_of_listed_keys", Default: "false", Notes: "with keys_not_listed: keep or remove, true replaces existing listed lines with the task's lines, including removing restrictions absent from them; without it, plain keys preserve existing lines and different explicit options fail before changes; omit with state: absent"},
		{Name: "keys_not_listed", Values: notListed, Required: true, Notes: "keep adds missing keys and keeps unlisted lines; remove keeps only listed keys, and keys: [] empties the file; both preserve existing lines for plain keys and refuse different explicit options unless replace_options_of_listed_keys: true; omit with absent"},
		{Name: "state", Values: presenceStates, Default: "present", Notes: "absent removes the listed keys whatever their options; accepts only for_user_account, keys, key_files_on_controller_machine, and key_directories_on_controller_machine"},
	},
	Example: "- authorized_keys:\n    for_user_account: deploy\n    keys:\n      - ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIORZZg3NlYdxJ28janK/DsJ1O0O2YgF7KN2aiEPskegs alice@laptop\n    keys_not_listed: keep\n",
	Results: []string{"status", "change", "value.added", "value.updated", "value.removed"},
	Dryrun:  "supported; lists keys to add, update, or remove by type, fingerprint, and comment",
}

// readKeyFiles reads the task's key_files_on_controller_machine and key_directories_on_controller_machine on the
// controller machine into FileKeys when the project loads. Paths with
// references resolve for each node instead (registerSources): they may read
// only what the controller machine knows before it connects.
func readKeyFiles(k *AuthorizedKeys, base string) error {
	k.FileKeys, k.SourceBase = nil, base
	if keyPathsDynamic(*k) {
		for _, p := range keyPaths(k) {
			if Dynamic(*p.Name) {
				if err := CheckSourceReferences(p.Field, *p.Name); err != nil {
					return err
				}
			}
		}
		return nil
	}
	var err error
	k.FileKeys, err = readKeys(base, keyPaths(k))
	return err
}

// readKeys reads key files, relative to base like file.source, in the order
// given; a directory gives the files directly inside whose name ends in
// .pub, in name order. Each file holds key lines; blank lines and lines
// starting with # are skipped.
func readKeys(base string, paths []keyPath) ([]FileKey, error) {
	var keys []FileKey
	read := func(field, name, path string) error {
		b, err := readTemplate(path)
		if err != nil {
			return &FieldError{field, fmt.Errorf("key file %s: %w", name, err)}
		}
		found := false
		for n, line := range strings.Split(string(b), "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			place := fmt.Sprintf("%s %s line %d", field, name, n+1)
			if _, err := ParseAuthorizedKey(line); err != nil {
				return &FieldError{field, fmt.Errorf("%s: %w", place, err)}
			}
			keys = append(keys, FileKey{Line: line, Place: place})
			found = true
		}
		if !found {
			return &FieldError{field, fmt.Errorf("key file %s holds no key line", name)}
		}
		return nil
	}
	for _, p := range paths {
		name := *p.Name
		if !p.Directory {
			path, err := localSource(base, p.Field, name)
			if err != nil {
				return nil, err
			}
			if err := read(p.Field, name, path); err != nil {
				return nil, err
			}
			continue
		}
		directory, err := controllerPath(base, p.Field, name)
		if err != nil {
			return nil, err
		}
		entries, err := os.ReadDir(directory) // sorted by name
		if err != nil {
			return nil, &FieldError{p.Field, fmt.Errorf("key directory %s: %w", name, err)}
		}
		found := false
		for _, entry := range entries {
			if !strings.HasSuffix(entry.Name(), ".pub") {
				continue
			}
			file := filepath.Join(name, entry.Name())
			if !entry.Type().IsRegular() {
				return nil, invalid(p.Field, "key file "+file+" is not a regular file")
			}
			if err := read(p.Field, file, filepath.Join(directory, entry.Name())); err != nil {
				return nil, err
			}
			found = true
		}
		// An empty result would empty authorized_keys with keys_not_listed:
		// remove, and lock out every administrator, MMX's own key included.
		if !found {
			return nil, invalid(p.Field, fmt.Sprintf("key directory %s holds no file ending in .pub; MMX reads only files whose name ends in .pub, such as alice.pub (to remove every key, write keys: [] instead)", name))
		}
	}
	return keys, nil
}

// Prepare checks the inputs when the project loads and reads the key files
// on the controller machine, relative to base.
func (k *AuthorizedKeys) Prepare(base string) error {
	if err := ValidateAuthorizedKeys(*k); err != nil {
		return err
	}
	if err := readKeyFiles(k, base); err != nil {
		return err
	}
	// A key in a file may repeat a written key or a key of another file.
	return ValidateAuthorizedKeys(*k)
}

// Arguments lists the inputs that may hold a reference, with their rules.
func (k *AuthorizedKeys) Arguments(arguments *ArgumentList) {
	arguments.Public("for_user_account", &k.ForUserAccount, accountNameRule)
	for i := range k.Keys {
		arguments.Public(fmt.Sprintf("keys[%d]", i), &k.Keys[i], authorizedKey)
	}
}

// ControllerFiles lists the key files and key directories together.
func (k *AuthorizedKeys) ControllerFiles() []ControllerFile {
	if len(k.KeyFilesOnControllerMachine)+len(k.KeyDirectoriesOnControllerMachine) == 0 {
		return nil
	}
	return []ControllerFile{{Field: "key_files_on_controller_machine", Kind: SourceKeys, Base: k.SourceBase, Variants: &k.Variants, Keys: k}}
}

// Run does the module's work on the node.
func (k *AuthorizedKeys) Run(ctx context.Context, task NodeTask) protocol.Result {
	return task.Runtime.AuthorizedKeys(ctx, *k)
}
