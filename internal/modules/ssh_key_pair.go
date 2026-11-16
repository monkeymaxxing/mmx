package modules

import (
	"context"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"syscall"

	"mmx/internal/protocol"
)

// sshKeyTypes maps key_type to the type name that starts a public key line.
var sshKeyLinePrefixes = map[string]string{"ed25519": "ssh-ed25519", "rsa": "ssh-rsa"}

// SSHKeyPair makes an account's SSH key pair once and returns its public key.
// The node's ssh-keygen runs as the account, so it writes nothing the account
// could not write itself. MMX never reads the private key: ssh-keygen -y
// derives the public key from it, and an existing key is never replaced.
func (run Runtime) SSHKeyPair(ctx context.Context, k SSHKeyPair) (result protocol.Result) {
	if err := ValidateSSHKeyPair(k); err != nil {
		return protocol.ConfigurationError(err)
	}
	if _, err := lookPath(ctx, "ssh-keygen"); err != nil {
		return run.notYet(missingProgramError("ssh-keygen"))
	}
	u, err := lookupUser(k.ForUserAccount)
	if err != nil {
		return run.accountFailure(err)
	}
	group, err := user.LookupGroupId(u.Gid)
	if err != nil {
		return protocol.Failed(fmt.Errorf("look up the primary group of account %s: %w", k.ForUserAccount, err))
	}
	keys := keyFiles{SSHKeyPair: k, owner: Account(u.Username), group: Account(group.Name)}
	if u.Uid != strconv.Itoa(os.Geteuid()) {
		keys.env = map[string]string{}
		if keys.account, err = shellAccount(u.Username, keys.env); err != nil {
			return run.accountFailure(err)
		}
	}
	keyDir := filepath.Dir(k.PrivateKeyPath)
	above, err := os.OpenRoot(filepath.Dir(keyDir))
	if os.IsNotExist(err) {
		return run.notYet(fmt.Errorf("directory %s does not exist yet", filepath.Dir(keyDir)))
	}
	if err != nil {
		return protocol.Failed(err)
	}
	defer above.Close()
	dirInfo, err := entryInfoAt(above, filepath.Base(keyDir), true)
	if err != nil {
		return protocol.Failed(err)
	}
	var privateInfo, publicInfo os.FileInfo
	if dirInfo != nil {
		if keys.dir, err = openDirectoryAt(above, filepath.Base(keyDir), dirInfo); err != nil {
			return protocol.Failed(err)
		}
		defer keys.dir.Close()
		if privateInfo, err = entryInfoAt(keys.dir, filepath.Base(k.PrivateKeyPath), false); err != nil {
			return protocol.Failed(err)
		}
		if publicInfo, err = entryInfoAt(keys.dir, filepath.Base(k.PublicKeyPath()), false); err != nil {
			return protocol.Failed(err)
		}
	}
	if privateInfo != nil {
		return run.existingKeyPair(ctx, keys, publicInfo != nil)
	}
	if run.Dryrun {
		r := protocol.Success("would_change")
		r.Reason = "would create the SSH key pair; the public key is unknown until the SSH key pair exists"
		return r
	}
	if err := run.Mutation(k.PrivateKeyPath); err != nil {
		return protocol.Failed(err)
	}
	changed := func(err error) protocol.Result {
		r := protocol.Failed(err)
		r.Change = "changed"
		return r
	}
	if keys.dir == nil {
		metadata, err := metadataFor(nil, 0700, "", keys.owner, keys.group)
		if err != nil {
			return protocol.Failed(err)
		}
		name := filepath.Base(keyDir)
		if err := createLabeled(ctx, keyDir, selinuxDirectory, func() error { return above.Mkdir(name, 0700) }); err != nil {
			return protocol.Failed(err)
		}
		if dirInfo, err = above.Lstat(name); err != nil {
			return changed(err)
		}
		if keys.dir, err = openDirectoryAt(above, name, dirInfo); err != nil {
			return changed(err)
		}
		defer keys.dir.Close()
		handle, err := keys.dir.Open(".")
		if err == nil {
			_, err = metadata.apply(handle)
			handle.Close()
		}
		if err != nil {
			return changed(err)
		}
	}
	// The key has no passphrase: a service uses it unattended.
	made := runProcess(ctx, keys.env, "/", nil, keys.account, "ssh-keygen", "-q", "-t", k.KeyType, "-N", "", "-C", k.KeyCommentWhenCreated, "-f", k.PrivateKeyPath).Result
	if made.Status != "succeeded" {
		return changed(fmt.Errorf("ssh-keygen could not create the key pair as account %s: %s: %s", k.ForUserAccount, made.Reason, strings.TrimSpace(made.Stderr)))
	}
	// The public key is readable by everyone, like the one ssh-keygen makes in
	// a login shell; MMX sets that mode itself rather than rely on the umask.
	text, err := readTextAt(keys.dir, filepath.Base(k.PublicKeyPath()))
	if err != nil {
		return changed(err)
	}
	if r := run.writePublicKey(ctx, keys, text); r.Status != "succeeded" {
		r.Change = "changed"
		return r
	}
	result = protocol.Success("changed")
	result.PublicKey = firstLine(text)
	return result
}

// keyFiles is an ssh_key_pair task with its account and the pinned directory
// that holds, or will hold, the key files.
type keyFiles struct {
	SSHKeyPair
	owner, group Account
	account      *syscall.Credential // nil when the runner is the account
	env          map[string]string
	dir          *os.Root
}

// existingKeyPair checks an existing private key's type and returns its public
// key: the .pub file's line, or, when the .pub file is missing, the key
// ssh-keygen derives, which it writes again.
func (run Runtime) existingKeyPair(ctx context.Context, keys keyFiles, havePublic bool) protocol.Result {
	read := runProcess(ctx, keys.env, "/", nil, keys.account, "ssh-keygen", "-y", "-P", "", "-f", keys.PrivateKeyPath).Result
	derived := firstLine(read.Stdout)
	key := strings.Fields(derived)
	if read.Status != "succeeded" || len(key) < 2 {
		return protocol.Failed(fmt.Errorf("ssh-keygen could not read the private key %s as account %s: %s: %s", keys.PrivateKeyPath, keys.ForUserAccount, read.Reason, strings.TrimSpace(read.Stderr)))
	}
	if key[0] != sshKeyLinePrefixes[keys.KeyType] {
		return protocol.Failed(fmt.Errorf("the private key %s is a %s key, not %s as key_type says; MMX never replaces a key: to make a new one, remove %s and %s with file tasks (state: absent) first", keys.PrivateKeyPath, key[0], keys.KeyType, keys.PrivateKeyPath, keys.PublicKeyPath()))
	}
	if !havePublic {
		r := run.writePublicKey(ctx, keys, derived+"\n")
		if r.Status == "succeeded" {
			r.PublicKey = derived
		}
		return r
	}
	text, err := readTextAt(keys.dir, filepath.Base(keys.PublicKeyPath()))
	if err != nil {
		return protocol.Failed(err)
	}
	line := firstLine(text)
	// A key's identity is its type and base64 text; the comment may differ.
	if fields := strings.Fields(line); len(fields) < 2 || fields[0] != key[0] || fields[1] != key[1] {
		return protocol.Failed(fmt.Errorf("public key file %s does not match private key file %s; remove the public key file, and the next run writes the public key file again from the private key file", keys.PublicKeyPath(), keys.PrivateKeyPath))
	}
	r := protocol.Success("unchanged")
	r.PublicKey = line
	return r
}

// writePublicKey installs the public key file, owned by the account and
// readable by everyone.
func (run Runtime) writePublicKey(ctx context.Context, keys keyFiles, text string) protocol.Result {
	f := File{Path: keys.PublicKeyPath(), Literal: &text, Owner: keys.owner, Group: keys.group, Mode: "0644"}
	return run.fileAt(ctx, f, nil, keys.dir, 0644, nil)
}

func firstLine(text string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(text), "\n")
	return strings.TrimSpace(line)
}

// SSHKeyPair is an SSH key pair for an account, made once with the node's
// ssh-keygen. The public key is PrivateKeyPath with .pub added.
type SSHKeyPair struct {
	ForUserAccount        string `yaml:"for_user_account" json:"for_user_account"`
	PrivateKeyPath        string `yaml:"private_key_path" json:"private_key_path"`
	KeyType               string `yaml:"key_type" json:"key_type"`
	KeyCommentWhenCreated string `yaml:"key_comment_when_created,omitempty" json:"key_comment_when_created,omitempty"`
}

// PublicKeyPath is where ssh-keygen writes the public key of the pair.
func (k SSHKeyPair) PublicKeyPath() string { return k.PrivateKeyPath + ".pub" }

func ValidateSSHKeyPair(k SSHKeyPair) error {
	if k.ForUserAccount == "" {
		return invalid("for_user_account", "ssh_key_pair.for_user_account is required: the account that owns the key files")
	}
	if k.PrivateKeyPath == "" {
		return invalid("private_key_path", "ssh_key_pair.private_key_path is required, such as /home/backup/.ssh/id_ed25519; the public key is the same path with .pub")
	}
	if !slices.Contains(sshKeyTypes, k.KeyType) {
		return invalid("key_type", "ssh_key_pair.key_type is required: "+strings.Join(sshKeyTypes, " or "))
	}
	return nil
}

// sshKeyPairDescription is what mmx describe-module shows for ssh_key_pair.
var sshKeyPairDescription = Description{
	Name: "ssh_key_pair", Topic: TopicSSHKeysAndFirewalls, Schema: reflect.TypeOf(SSHKeyPair{}),
	Summary: "an SSH key pair for an account, without a passphrase, created once with the node's ssh-keygen (package openssh-client on Debian and Ubuntu, openssh on the Red Hat family) and never replaced",
	Fields: []Field{
		{Name: "for_user_account", Required: true, Notes: "the account that owns the key files and runs ssh-keygen; it must exist"},
		{Name: "private_key_path", Required: true, Notes: "absolute private key path, such as /home/backup/.ssh/id_ed25519; the public key path adds .pub. MMX creates a missing parent directory with permissions_mode 0700, owned by for_user_account"},
		{Name: "key_type", Values: sshKeyTypes, Required: true, Notes: "an existing private key of another type fails the task"},
		{Name: "key_comment_when_created", Default: "<for_user_account>@<node name>", Notes: "one line, written at the end of the public key"},
	},
	Example: "- ssh_key_pair:\n    for_user_account: backup\n    private_key_path: /home/backup/.ssh/id_ed25519\n    key_type: ed25519\n",
	Results: []string{"status", "change", "public_key"},
	Dryrun:  "supported; reads an existing key's public key, or reports that the pair would be created; public_key is then not known, and tasks that read it report not checked",
}

// Prepare checks the inputs when the project loads.
func (k *SSHKeyPair) Prepare(base string) error {
	return ValidateSSHKeyPair(*k)
}

// Arguments lists the inputs that may hold a reference, with their rules.
func (k *SSHKeyPair) Arguments(arguments *ArgumentList) {
	arguments.Public("for_user_account", &k.ForUserAccount, accountNameRule)
	arguments.Public("private_key_path", &k.PrivateKeyPath, absolute)
	if k.KeyCommentWhenCreated != "" {
		arguments.Public("key_comment_when_created", &k.KeyCommentWhenCreated, keyComment)
	}
}

// Run does the module's work on the node. The comment of the public key is
// <account>@<node name> unless the task gives one.
func (k *SSHKeyPair) Run(ctx context.Context, task NodeTask) protocol.Result {
	pair := *k
	if pair.KeyCommentWhenCreated == "" {
		pair.KeyCommentWhenCreated = pair.ForUserAccount + "@" + task.NodeName
	}
	return task.Runtime.SSHKeyPair(ctx, pair)
}

// ManagedPaths lists the paths the task manages or says it creates, as
// written; a dryrun cannot check a later task's path_exists on them.
func (k *SSHKeyPair) ManagedPaths() []string {
	return []string{k.PrivateKeyPath, k.PublicKeyPath()}
}

// UnknownInADryrun lists the result fields a dryrun cannot know: the public
// key of a pair the task would create.
func (k *SSHKeyPair) UnknownInADryrun(r protocol.Result) map[string]string {
	if r.Status != "succeeded" || r.PublicKey != "" {
		return nil
	}
	return map[string]string{"public_key": "would create the key pair, so its public key is not known in a dryrun"}
}
