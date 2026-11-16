package modules

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"syscall"

	"mmx/internal/protocol"
	"mmx/internal/value"
)

// Git checks out one ref of a repository into path with the node's git
// command, as the task's owner when it names one. A new checkout is built
// through installStaged.
func (run Runtime) Git(ctx context.Context, g Git, s value.Scope) protocol.Result {
	if _, err := lookPath(ctx, "git"); err != nil {
		return run.notYet(missingProgramError("git"))
	}
	// No prompts, no credential helper that could store a token, and no
	// Git LFS downloads during checkout.
	env := map[string]string{"GIT_TERMINAL_PROMPT": "0", "GIT_LFS_SKIP_SMUDGE": "1",
		"GIT_CONFIG_COUNT": "1", "GIT_CONFIG_KEY_0": "credential.helper", "GIT_CONFIG_VALUE_0": ""}
	var account *syscall.Credential
	uid, gid := os.Geteuid(), os.Getegid()
	if g.RunAsUserAccount != "" {
		var err error
		if account, err = shellAccount(g.RunAsUserAccount, env); err != nil {
			return run.accountFailure(fmt.Errorf("git.run_as_user_account: %w", err))
		}
		uid, gid = int(account.Uid), int(account.Gid)
	}
	removeCredentials, err := run.gitCredentials(g, s, env, account)
	if err != nil {
		return protocol.Failed(err)
	}
	defer removeCredentials()
	git := func(dir string, args ...string) (string, error) {
		r := runProcess(ctx, env, dir, nil, account, "git", args...).Result
		if r.Status != "succeeded" {
			detail := strings.TrimSpace(r.Stderr)
			if detail == "" {
				detail = r.Reason
			}
			return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), detail)
		}
		// Only the line ends: status output starts with a space.
		return strings.TrimRight(r.Stdout, "\r\n"), nil
	}
	checkout := gitCheckout{g: g, git: git}

	parent, info, err := openParent(g.Path, true)
	if err != nil {
		return protocol.Failed(err)
	}
	if parent == nil {
		if r := run.missingParent(g.Path); r != nil {
			return *r
		}
	}
	if parent != nil {
		defer parent.Close()
	}
	var current any // the checked-out commit; null before a clone
	var edited []string
	if info != nil {
		head, changed, err := checkout.existing(info)
		if err != nil {
			return protocol.Failed(err)
		}
		current, edited = head, changed
	}
	target, ref, err := checkout.resolve()
	if err != nil {
		return protocol.Failed(err)
	}
	result := func(change string, commit any) protocol.Result {
		r := protocol.Success(change)
		v := map[string]any{"commit": commit, "previous_commit": current}
		if g.FilesChangedInCheckoutOnNode == GitDiscardTheChanges {
			restored := make([]any, len(edited))
			for i, file := range edited {
				restored[i] = file
			}
			v["restored_files"] = restored
		}
		r.Value = v
		return r
	}
	needsHistory := false
	if info != nil && g.FullHistory {
		shallow, err := git(g.Path, "rev-parse", "--is-shallow-repository")
		if err != nil {
			return protocol.Failed(err)
		}
		needsHistory = shallow == "true"
	}
	if current == target && len(edited) == 0 && !needsHistory {
		return result("unchanged", target)
	}
	if run.Dryrun {
		return result("would_change", target)
	}
	if err := ctx.Err(); err != nil {
		return protocol.Failed(err)
	}
	if err := run.Mutation(g.Path); err != nil {
		return protocol.Failed(err)
	}
	if info != nil {
		// Fetch and validate the target before discarding any edited files.
		commit := target
		if current != target || needsHistory {
			commit, err = checkout.fetch(g.Path, ref)
			if err != nil {
				return protocol.Failed(err)
			}
		}
		// Restore edits before moving to the new commit; untracked files stay.
		if len(edited) > 0 {
			if _, err := git(g.Path, "reset", "-q", "--hard", "HEAD"); err != nil {
				r := protocol.Failed(err)
				r.Change = "unknown"
				return r
			}
		}
		if current == target && !needsHistory {
			return result("changed", target)
		}
		if err := checkout.checkOut(g.Path, commit); err != nil {
			r := protocol.Failed(err)
			if len(edited) > 0 {
				r.Change = "changed"
			}
			return r
		}
		return result("changed", commit)
	}
	commit := ""
	installed, err := installStaged(ctx, parent, g.Path, func(staging string) error {
		handle, err := parent.Open(staging)
		if err != nil {
			return err
		}
		_, err = fileMetadata{0755, uid, gid}.apply(handle)
		handle.Close()
		dir := filepath.Join(filepath.Dir(g.Path), staging)
		if err == nil {
			_, err = git(dir, "init", "-q")
		}
		if err == nil {
			_, err = git(dir, "remote", "add", "origin", g.Repo)
		}
		if err == nil {
			commit, err = checkout.fetch(dir, ref)
		}
		if err == nil {
			err = checkout.checkOut(dir, commit)
		}
		return err
	})
	if !installed {
		return protocol.Failed(err)
	}
	r := result("changed", commit)
	if err != nil {
		r.Status, r.Reason = "failed", err.Error()
	}
	return r
}

type gitCheckout struct {
	g   Git
	git func(dir string, args ...string) (string, error)
}

// existing checks that path is a checkout of the task's repository, and
// returns its commit. Tracked files edited on the node fail the task, or with
// discard_the_changes are returned for the task to restore.
func (c gitCheckout) existing(info os.FileInfo) (string, []string, error) {
	path := c.g.Path
	dotGit, err := os.Lstat(filepath.Join(path, ".git"))
	if !info.IsDir() || err != nil || !dotGit.IsDir() {
		return "", nil, fmt.Errorf("%s exists and is not a Git checkout", path)
	}
	origin, err := c.git(path, "config", "--get", "remote.origin.url")
	if err != nil {
		return "", nil, fmt.Errorf("%s is not a usable checkout: %w", path, err)
	}
	if origin != c.g.Repo {
		return "", nil, fmt.Errorf("%s is a checkout of %s, not %s", path, origin, c.g.Repo)
	}
	status, err := c.git(path, "--no-optional-locks", "status", "--porcelain=v1", "-z", "--untracked-files=no")
	if err != nil {
		return "", nil, err
	}
	edited := gitStatusFiles(status)
	if len(edited) > 0 && c.g.FilesChangedInCheckoutOnNode != GitDiscardTheChanges {
		return "", nil, fmt.Errorf("tracked files in %s have local changes on the node: %s; restore the tracked files, commit the local changes to the repository, or set files_changed_in_checkout_on_node: discard_the_changes", path, strings.Join(edited, ", "))
	}
	head, err := c.git(path, "rev-parse", "--verify", "HEAD^{commit}")
	return head, edited, err
}

// gitStatusFiles lists the paths in git status --porcelain=v1 -z output.
// Each entry is two status letters, a space, and the path; a rename or copy
// is followed by its original path, which is listed too.
func gitStatusFiles(status string) []string {
	var files []string
	fields := strings.Split(status, "\x00")
	for i := 0; i < len(fields); i++ {
		entry := fields[i]
		if len(entry) < 4 {
			continue
		}
		files = append(files, entry[3:])
		if (entry[0] == 'R' || entry[0] == 'C') && i+1 < len(fields) {
			i++
			files = append(files, fields[i])
		}
	}
	return files
}

// resolve looks up only the selected namespace. A full commit ID needs no
// branch or tag lookup, even when either has the same name.
func (c gitCheckout) resolve() (commit, fetch string, err error) {
	if c.g.CheckOutCommit != "" {
		return c.g.CheckOutCommit, c.g.CheckOutCommit, nil
	}
	ref, kind := "refs/heads/"+c.g.FollowBranch, "branch"
	names := []string{ref}
	if c.g.CheckOutTag != "" {
		ref, kind = "refs/tags/"+c.g.CheckOutTag, "tag"
		names = []string{ref + "^{}", ref}
	}
	out, err := c.git("/", append([]string{"ls-remote", c.g.Repo}, names...)...)
	if err != nil {
		return "", "", err
	}
	found := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if id, name, ok := strings.Cut(line, "\t"); ok {
			found[name] = id
		}
	}
	for _, name := range names {
		if id := found[name]; id != "" {
			return id, ref, nil
		}
	}
	return "", "", fmt.Errorf("%s %s does not exist in %s", kind, strings.TrimPrefix(strings.TrimPrefix(ref, "refs/heads/"), "refs/tags/"), c.g.Repo)
}

// fetch downloads ref into the checkout at dir, only its commit unless the
// task wants full history, and validates the selected commit. With full
// history a commit ID comes with every branch, since not every server serves
// a commit by its ID.
func (c gitCheckout) fetch(dir, ref string) (string, error) {
	args := []string{"fetch", "-q", "--no-tags"}
	if !c.g.FullHistory {
		args = append(args, "--depth", "1")
	} else if shallow, err := c.git(dir, "rev-parse", "--is-shallow-repository"); err != nil {
		return "", err
	} else if shallow == "true" {
		args = append(args, "--unshallow")
	}
	args, head := append(args, "origin"), "FETCH_HEAD"
	if c.g.FullHistory && GitCommit.MatchString(ref) {
		head = ref
	} else {
		args = append(args, ref)
	}
	if _, err := c.git(dir, args...); err != nil {
		return "", err
	}
	if c.g.CheckOutCommit != "" {
		kind, err := c.git(dir, "cat-file", "-t", c.g.CheckOutCommit)
		if err != nil {
			return "", err
		}
		if kind != "commit" {
			return "", fmt.Errorf("check_out_commit %s does not name a commit", c.g.CheckOutCommit)
		}
	}
	return c.git(dir, "rev-parse", "--verify", head+"^{commit}")
}

func (c gitCheckout) checkOut(dir, commit string) error {
	_, err := c.git(dir, "-c", "advice.detachedHead=false", "checkout", "-q", "--detach", commit)
	return err
}

// gitCredentials writes the task's private values to files only the account
// running git can read, in a private directory of the task
// (makePrivateTaskDirectory), and points git at them through its environment, so
// they never appear in a URL, a command line, or .git/config. account is the
// account that runs git, nil for root. The returned function removes the
// files.
//
// The user name, the password, and the SSH key are logins that MMX hands
// only to a tool: a secret in them does not hide the task's result, and MMX
// replaces them in what the task reports. git and ssh get each of them as
// its plain text, keep none, and name a user name as its plain text in a
// failure, so there is no other form to replace. ssh_known_hosts_lines is no login; a
// secret in it hides the result like any other secret.
func (run Runtime) gitCredentials(g Git, s value.Scope, env map[string]string, account *syscall.Credential) (func(), error) {
	files := map[string]string{}
	for _, private := range []struct {
		name, raw              string
		loginHandedOnlyToATool bool
	}{{"username", g.Username, true}, {"password", g.Password, true}, {"ssh_private_key", g.SSHPrivateKey, true}, {"ssh_known_hosts_lines", g.SSHKnownHostsLines, false}} {
		if private.raw == "" {
			continue
		}
		resolve := s.Resolve
		if private.loginHandedOnlyToATool {
			resolve = s.LoginHandedOnlyToATool
		}
		v, err := resolve(private.raw)
		if err == nil {
			files[private.name], err = value.Scalar(v)
		}
		if err != nil {
			return nil, fmt.Errorf("git.%s: %w", private.name, err)
		}
	}
	if len(files) == 0 {
		return func() {}, nil
	}
	_, remove, err := run.makePrivateTaskDirectory("git", account, func(directory string) error {
		path := func(name string) string { return filepath.Join(directory, name) }
		if g.Username != "" || g.Password != "" {
			// git asks for both; an omitted one answers empty.
			username, password := files["username"], files["password"]
			files["username"], files["password"] = username, password
			files["askpass"] = fmt.Sprintf("#!/bin/sh\ncase \"$1\" in\nUsername*) cat %s ;;\n*) cat %s ;;\nesac\n", path("username"), path("password"))
			env["GIT_ASKPASS"] = path("askpass")
		}
		if g.SSHKnownHostsLines != "" {
			command := "ssh -F /dev/null -o BatchMode=yes -o IdentityAgent=none -o StrictHostKeyChecking=yes -o GlobalKnownHostsFile=/dev/null -o UserKnownHostsFile=" + path("ssh_known_hosts_lines")
			if key := files["ssh_private_key"]; key != "" {
				if !strings.HasSuffix(key, "\n") {
					files["ssh_private_key"] = key + "\n" // OpenSSH rejects a key without its final newline
				}
				command += " -o IdentitiesOnly=yes -i " + path("ssh_private_key")
			}
			env["GIT_SSH_COMMAND"] = command
		}
		for name, text := range files {
			if name == "askpass" {
				// A shell writes the program that git runs, so that this
				// process never holds it open for writing: a process that
				// another goroutine starts meanwhile would inherit that
				// open file until it runs its own program, and git's run
				// of askpass would fail with "text file busy" until then.
				write := exec.Command("/bin/sh", "-c", `umask 077 && cat > "$1" && chmod 700 "$1"`, "sh", path(name))
				write.Stdin = strings.NewReader(text)
				if output, err := write.CombinedOutput(); err != nil {
					return fmt.Errorf("writing %s: %w: %s", path(name), err, strings.TrimSpace(string(output)))
				}
				continue
			}
			if err := os.WriteFile(path(name), []byte(text), 0600); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("git: the private directory for the credentials: %w", err)
	}
	return remove, nil
}

// GitHead returns the commit checked out at a git task's path, or "" when
// there is no checkout there yet. It runs git as run_as_user_account, as the
// task does, so that git accepts the tree's ownership.
func GitHead(ctx context.Context, g Git) (string, error) {
	if _, err := os.Lstat(filepath.Join(g.Path, ".git")); err != nil {
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
			return "", nil
		}
		return "", err
	}
	env := map[string]string{"GIT_TERMINAL_PROMPT": "0"}
	var account *syscall.Credential
	if g.RunAsUserAccount != "" {
		var err error
		if account, err = shellAccount(g.RunAsUserAccount, env); err != nil {
			return "", fmt.Errorf("git.run_as_user_account: %w", err)
		}
	}
	r := runProcess(ctx, env, g.Path, nil, account, "git", "rev-parse", "--verify", "HEAD^{commit}").Result
	if r.Status != "succeeded" {
		return "", nil // a checkout without a commit yet
	}
	return strings.TrimSpace(r.Stdout), nil
}

// Git is a directory checked out from a repository at a branch, tag, or
// commit. The credential fields are private values the module resolves;
// username, password, and ssh_private_key are logins that it hands only to git
// (gitCredentials).
type Git struct {
	Repo               string `yaml:"repo" json:"repo"`
	FollowBranch       string `yaml:"follow_branch,omitempty" json:"follow_branch,omitempty"`
	CheckOutTag        string `yaml:"check_out_tag,omitempty" json:"check_out_tag,omitempty"`
	CheckOutCommit     string `yaml:"check_out_commit,omitempty" json:"check_out_commit,omitempty"`
	Path               string `yaml:"path" json:"path"`
	RunAsUserAccount   string `yaml:"run_as_user_account,omitempty" json:"run_as_user_account,omitempty"`
	FullHistory        bool   `yaml:"full_history,omitempty" json:"full_history,omitempty"`
	Username           string `yaml:"username,omitempty" json:"username,omitempty"`
	Password           string `yaml:"password,omitempty" json:"password,omitempty"`
	SSHPrivateKey      string `yaml:"ssh_private_key,omitempty" json:"ssh_private_key,omitempty"`
	SSHKnownHostsLines string `yaml:"ssh_known_hosts_lines,omitempty" json:"ssh_known_hosts_lines,omitempty"`
	// FilesChangedInCheckoutOnNode says what happens to tracked files that were
	// edited in the checkout on the node.
	FilesChangedInCheckoutOnNode string `yaml:"files_changed_in_checkout_on_node,omitempty" json:"files_changed_in_checkout_on_node,omitempty"`
}

// What a git task does with tracked files edited on the node.
const (
	GitStopWithAnError   = "stop_with_an_error"
	GitDiscardTheChanges = "discard_the_changes"
)

var gitFilesChangedInCheckoutOnNode = []string{GitStopWithAnError, GitDiscardTheChanges}

// GitCommit is the full commit ID accepted by check_out_commit.
var GitCommit = regexp.MustCompile(`^[0-9a-f]{40}$`)

var gitRefName = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._/+@-]*$`)

var gitRefRule = rule("Git branch and tag names must name one branch or tag", func(s string) bool {
	return gitRefName.MatchString(s) && !strings.Contains(s, "..") && !strings.HasSuffix(s, "/")
})

// GitRepoKind classifies a repository location: "https" or "http" for those
// URLs, "ssh" for ssh:// and scp-like user@server:path, and "local" for a path
// or file:// URL.
func GitRepoKind(repo string) string {
	if scheme, _, ok := strings.Cut(repo, "://"); ok {
		switch scheme {
		case "https", "http", "file":
			if scheme == "file" {
				return "local"
			}
			return scheme
		}
		return "ssh"
	}
	if colon := strings.Index(repo, ":"); colon > 0 && !strings.Contains(repo[:colon], "/") {
		return "ssh"
	}
	return "local"
}

// gitRepoRule checks a resolved repository against the credentials the task
// sets: an SSH repository needs ssh_known_hosts_lines, username and password need
// HTTPS, and ssh_private_key needs SSH. A URL never carries credentials.
func gitRepoRule(g Git) func(any) error {
	return parsed(func(s string) error {
		if s == "" || strings.HasPrefix(s, "-") || strings.ContainsAny(s, " \t\r\n\x00") {
			return errors.New("git.repo must be a repository URL or path")
		}
		kind := GitRepoKind(s)
		if strings.Contains(s, "://") {
			u, err := url.Parse(s)
			if err != nil {
				return errors.New("git.repo must be a repository URL or path")
			}
			if _, set := u.User.Password(); set || (u.User != nil && kind != "ssh") {
				return errors.New("git.repo must not contain credentials; use username and password")
			}
		}
		switch {
		case kind == "ssh" && g.SSHKnownHostsLines == "":
			return errors.New("an SSH repository requires ssh_known_hosts_lines")
		case (g.Username != "" || g.Password != "") && kind != "https":
			return errors.New("username and password require an https:// repository")
		case (g.SSHPrivateKey != "" || g.SSHKnownHostsLines != "") && kind != "ssh":
			return errors.New("ssh_private_key and ssh_known_hosts_lines require an SSH repository")
		}
		return nil
	})
}

// gitDescription is what mmx describe-module shows for git.
var gitDescription = Description{
	Name: "git", Topic: TopicDirectoriesDownloadsAndReleaseDirectories, Schema: reflect.TypeOf(Git{}),
	Summary: "a directory checked out from a Git repository at a tag, branch, or commit",
	Fields: []Field{
		{Name: "repo", Required: true, Notes: "https://, ssh:// or user@server:path, or a local path; no credentials in the URL"},
		{Name: "follow_branch", Notes: "branch followed on each run; exactly one of follow_branch, check_out_tag, or check_out_commit is required"},
		{Name: "check_out_tag", Notes: "tag to check out; looked up only among tags"},
		{Name: "check_out_commit", Notes: "full 40-character hexadecimal commit ID"},
		{Name: "path", Required: true, Notes: "absolute; the parent must exist; install the git package first"},
		{Name: "run_as_user_account", Default: "root", Notes: "local account every git command runs as, which owns the checkout"},
		{Name: "full_history", Default: "false", Notes: "fetch the whole history instead of only the target commit"},
		{Name: "username", Notes: "for an https:// repo; a private value"},
		{Name: "password", Notes: "token for an https:// repo; a private value, normally a secret; a secret in username, password, or ssh_private_key does not hide the task's result: MMX shows [redacted] wherever a text of the result holds the secret"},
		{Name: "ssh_private_key", Notes: "private key for an SSH repo; normally a secret"},
		{Name: "ssh_known_hosts_lines", Notes: "the SSH server's host-key lines; required for an SSH repo"},
		{Name: "files_changed_in_checkout_on_node", Values: gitFilesChangedInCheckoutOnNode, Default: GitStopWithAnError, Notes: "what happens when files Git tracks were edited in the checkout on the node: stop_with_an_error fails without changing anything and lists the files; discard_the_changes restores them to the checked-out commit and lists them in value.restored_files; untracked and ignored files stay in both cases"},
	},
	Example: "- git:\n    repo: https://github.com/example/app.git\n    check_out_tag: v1.4.2\n    path: /srv/app/releases/v1.4.2\n    run_as_user_account: app\n",
	Results: []string{"status", "change", "value.commit", "value.previous_commit", "value.restored_files"},
	Dryrun:  "supported; resolves the target commit on the remote and reports it with the current one, and lists the files discard_the_changes would restore, writing nothing; not_checked while the parent directory is missing, unless an earlier node task of the dryrun would create it",
}

// Prepare checks the inputs when the project loads; the argument rules
// check git's fields, also after references resolve.
func (g *Git) Prepare(base string) error {
	count := 0
	for _, selector := range []string{g.FollowBranch, g.CheckOutTag, g.CheckOutCommit} {
		if selector != "" {
			count++
		}
	}
	if count != 1 {
		return fmt.Errorf("git requires exactly one of follow_branch, check_out_tag, or check_out_commit")
	}

	if g.FilesChangedInCheckoutOnNode != "" && !slices.Contains(gitFilesChangedInCheckoutOnNode, g.FilesChangedInCheckoutOnNode) {
		return invalid("files_changed_in_checkout_on_node", "files_changed_in_checkout_on_node must be "+strings.Join(gitFilesChangedInCheckoutOnNode, " or "))
	}
	return nil
}

// Arguments lists the inputs that may hold a reference, with their rules.
func (g *Git) Arguments(arguments *ArgumentList) {
	arguments.Public("repo", &g.Repo, gitRepoRule(*g))
	if g.FollowBranch != "" {
		arguments.Public("follow_branch", &g.FollowBranch, gitRefRule)
	}
	if g.CheckOutTag != "" {
		arguments.Public("check_out_tag", &g.CheckOutTag, gitRefRule)
	}
	if g.CheckOutCommit != "" {
		arguments.Public("check_out_commit", &g.CheckOutCommit, rule("git.check_out_commit must be a full 40-character hexadecimal commit ID", GitCommit.MatchString))
	}
	arguments.Public("path", &g.Path, absolute)
	if g.RunAsUserAccount != "" {
		arguments.Public("run_as_user_account", &g.RunAsUserAccount, accountNameRule)
	}
	for _, login := range []struct{ field, raw string }{{"username", g.Username}, {"password", g.Password}, {"ssh_private_key", g.SSHPrivateKey}} {
		if login.raw != "" {
			arguments.LoginHandedOnlyToATool(login.field, login.raw, scalar)
		}
	}
	if g.SSHKnownHostsLines != "" {
		arguments.Private("ssh_known_hosts_lines", g.SSHKnownHostsLines, scalar)
	}
}

// CleanPaths cleans the resolved path.
func (g *Git) CleanPaths() {
	g.Path = filepath.Clean(g.Path)
}

// Run does the module's work on the node.
func (g *Git) Run(ctx context.Context, task NodeTask) protocol.Result {
	return task.Runtime.Git(ctx, *g, task.Scope)
}

// ManagedPaths lists the paths the task manages or says it creates, as
// written; a dryrun cannot check a later task's path_exists on them.
func (g *Git) ManagedPaths() []string {
	return []string{g.Path}
}

// TrackedInputs lists the checkout, which counts by its commit.
func (g *Git) TrackedInputs() []TrackedInput {
	return []TrackedInput{{Kind: "git", Path: g.Path, State: g}}
}

// ReadState is the commit checked out now.
func (g *Git) ReadState(ctx context.Context, _ value.Scope) (string, error) {
	return GitHead(ctx, *g)
}

// StateAfter is the commit a successful result says the checkout is at, or
// would be at in a dryrun.
func (g *Git) StateAfter(r protocol.Result) (string, bool) {
	if r.Status != "succeeded" {
		return "", false
	}
	v, _ := r.Value.(map[string]any)
	commit, ok := v["commit"].(string)
	return commit, ok
}
