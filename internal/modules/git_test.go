package modules

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"mmx/internal/value"
)

// gitRepository creates a repository with an annotated tag v1 on its first
// commit and main one commit later. The checkout under test reads only the
// test's own Git configuration.
// gitConfigurationOfTests keeps the configuration of the machine's account
// and system away from git, in the node tasks of the tests and in the tests'
// own git commands.
var gitConfigurationOfTests = map[string]string{"GIT_CONFIG_GLOBAL": os.DevNull, "GIT_CONFIG_NOSYSTEM": "1"}

// gitCommandOfTest is a git command of the test itself, with
// gitConfigurationOfTests.
func gitCommandOfTest(args ...string) *exec.Cmd {
	c := exec.Command("git", args...)
	c.Env = os.Environ()
	for name, value := range gitConfigurationOfTests {
		c.Env = append(c.Env, name+"="+value)
	}
	return c
}

func gitRepository(t *testing.T) (dir string, git func(...string) string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	for name, value := range gitConfigurationOfTests {
		setWorkerEnvironment(t, name, value)
	}
	dir = filepath.Join(t.TempDir(), "source")
	git = func(args ...string) string {
		t.Helper()
		out, err := gitCommandOfTest(append([]string{"-C", dir, "-c", "user.name=MMX", "-c", "user.email=mmx@example.com"}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatal(args, string(out), err)
		}
		return strings.TrimSpace(string(out))
	}
	commit := func(text string) {
		if err := os.WriteFile(filepath.Join(dir, "app.txt"), []byte(text), 0644); err != nil {
			t.Fatal(err)
		}
		git("add", "app.txt")
		git("commit", "-q", "-m", text)
	}
	if err := os.Mkdir(dir, 0755); err != nil {
		t.Fatal(err)
	}
	git("init", "-q", "-b", "main")
	commit("one\n")
	git("tag", "-a", "v1", "-m", "release one")
	commit("two\n")
	return dir, git
}

func gitResult(t *testing.T, r any) (commit, previous any) {
	t.Helper()
	v, ok := r.(map[string]any)
	if !ok {
		t.Fatal("no result value", r)
	}
	return v["commit"], v["previous_commit"]
}

func TestGitClonesUpdatesAndFollowsABranch(t *testing.T) {
	t.Parallel()
	source, git := gitRepository(t)
	first, second := git("rev-parse", "v1^{commit}"), git("rev-parse", "main")
	releases := t.TempDir()
	path := filepath.Join(releases, "app")
	g := Git{Repo: source, CheckOutTag: "v1", Path: path}
	checkout := func(run Runtime, ref, status, change string, commit, previous any) {
		t.Helper()
		g.FollowBranch, g.CheckOutTag, g.CheckOutCommit = "", "", ""
		if GitCommit.MatchString(ref) {
			g.CheckOutCommit = ref
		} else if ref == "main" {
			g.FollowBranch = ref
		} else {
			g.CheckOutTag = ref
		}
		r := run.Git(testContext(t), g, value.Scope{})
		if r.Status != status || r.Change != change {
			t.Fatal(ref, r)
		}
		if got, was := gitResult(t, r.Value); got != commit || was != previous {
			t.Fatal(ref, got, was)
		}
	}
	content := func() string {
		data, _ := os.ReadFile(filepath.Join(path, "app.txt"))
		return string(data)
	}
	checkout(Runtime{Dryrun: true}, "v1", "succeeded", "would_change", first, nil)
	if found := names(t, releases); !slices.Equal(found, []string{"."}) {
		t.Fatal("dryrun wrote", found)
	}
	checkout(Runtime{}, "v1", "succeeded", "changed", first, nil)
	if content() != "one\n" || slices.Contains(names(t, releases), ".mmx-staging-app") {
		t.Fatal(names(t, releases))
	}
	if shallow, err := gitCommandOfTest("-C", path, "rev-parse", "--is-shallow-repository").Output(); err != nil || strings.TrimSpace(string(shallow)) != "true" {
		t.Fatal("fetched more than the target commit", string(shallow), err)
	}
	checkout(Runtime{}, "v1", "succeeded", "unchanged", first, first)

	// Untracked build output stays through an update.
	if err := os.WriteFile(filepath.Join(path, "build.out"), []byte("built\n"), 0644); err != nil {
		t.Fatal(err)
	}
	checkout(Runtime{Dryrun: true}, "main", "succeeded", "would_change", second, first)
	if content() != "one\n" {
		t.Fatal("dryrun checked out", content())
	}
	checkout(Runtime{}, "main", "succeeded", "changed", second, first)
	if content() != "two\n" {
		t.Fatal(content())
	}
	if _, err := os.Stat(filepath.Join(path, "build.out")); err != nil {
		t.Fatal("update removed untracked output", err)
	}
	// A branch follows its remote head on each run.
	if err := os.WriteFile(filepath.Join(source, "app.txt"), []byte("three\n"), 0644); err != nil {
		t.Fatal(err)
	}
	git("commit", "-q", "-am", "three")
	third := git("rev-parse", "main")
	checkout(Runtime{}, "main", "succeeded", "changed", third, second)

	// A commit ID is fetched directly, and an unchanged rerun of one needs
	// no network: the repository can be gone.
	checkout(Runtime{}, first, "succeeded", "changed", first, third)
	if err := os.Rename(source, source+".moved"); err != nil {
		t.Fatal(err)
	}
	checkout(Runtime{}, first, "succeeded", "unchanged", first, first)
	if err := os.Rename(source+".moved", source); err != nil {
		t.Fatal(err)
	}

	// Local changes to tracked files fail without changing anything.
	if err := os.WriteFile(filepath.Join(path, "app.txt"), []byte("edited\n"), 0644); err != nil {
		t.Fatal(err)
	}
	g.FollowBranch, g.CheckOutTag, g.CheckOutCommit = "main", "", ""
	for _, run := range []Runtime{{}, {Dryrun: true}} {
		if r := run.Git(testContext(t), g, value.Scope{}); r.Status != "failed" || !strings.Contains(r.Reason, "have local changes on the node: app.txt;") || content() != "edited\n" {
			t.Fatal(r)
		}
	}
	// discard_the_changes restores them, lists them, and then moves on;
	// untracked files stay. A dryrun lists them and restores nothing.
	g.FilesChangedInCheckoutOnNode = GitDiscardTheChanges
	restored := func(r any) any { return r.(map[string]any)["restored_files"] }
	if r := (Runtime{Dryrun: true}).Git(testContext(t), g, value.Scope{}); r.Change != "would_change" || !slices.Equal(restored(r.Value).([]any), []any{"app.txt"}) || content() != "edited\n" {
		t.Fatal(r)
	}
	r := (Runtime{}).Git(testContext(t), g, value.Scope{})
	if r.Status != "succeeded" || r.Change != "changed" || !slices.Equal(restored(r.Value).([]any), []any{"app.txt"}) || content() != "three\n" {
		t.Fatal(r, content())
	}
	if _, err := os.Stat(filepath.Join(path, "build.out")); err != nil {
		t.Fatal("discarding removed untracked output", err)
	}
	// The same commit with an edited file: restored, and so changed.
	if err := os.WriteFile(filepath.Join(path, "app.txt"), []byte("edited\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if r := (Runtime{}).Git(testContext(t), g, value.Scope{}); r.Change != "changed" || content() != "three\n" {
		t.Fatal(r)
	}
	if r := (Runtime{}).Git(testContext(t), g, value.Scope{}); r.Change != "unchanged" || len(restored(r.Value).([]any)) != 0 {
		t.Fatal(r)
	}
	g.FollowBranch, g.CheckOutTag, g.CheckOutCommit = "", "v9", ""
	if r := (Runtime{}).Git(testContext(t), g, value.Scope{}); r.Status != "failed" || !strings.Contains(r.Reason, "does not exist") {
		t.Fatal(r)
	}
}

func TestGitStatusFiles(t *testing.T) {
	t.Parallel()
	status := " M app.txt\x00D  gone.txt\x00R  new name.txt\x00old name.txt\x00"
	if got := gitStatusFiles(status); !slices.Equal(got, []string{"app.txt", "gone.txt", "new name.txt", "old name.txt"}) {
		t.Fatal(got)
	}
	if err := (&Git{FollowBranch: "main", FilesChangedInCheckoutOnNode: "keep"}).Prepare(""); err == nil {
		t.Fatal("accepted keep")
	}
}

func TestGitFullHistoryFetchesEveryBranchForACommitID(t *testing.T) {
	t.Parallel()
	source, git := gitRepository(t)
	first := git("rev-parse", "v1^{commit}")
	path := filepath.Join(t.TempDir(), "app")
	g := Git{Repo: source, FollowBranch: "main", Path: path}
	if r := (Runtime{}).Git(testContext(t), g, value.Scope{}); r.Status != "succeeded" {
		t.Fatal(r)
	}
	// A shallow checkout switched to full history is deepened.
	g.FollowBranch, g.CheckOutCommit, g.FullHistory = "", first, true
	if r := (Runtime{}).Git(testContext(t), g, value.Scope{}); r.Status != "succeeded" || r.Change != "changed" {
		t.Fatal(r)
	}
	out, err := gitCommandOfTest("-C", path, "rev-list", "--count", "origin/main").Output()
	if err != nil || strings.TrimSpace(string(out)) != "2" {
		t.Fatal("history", string(out), err)
	}
	if head, _ := gitCommandOfTest("-C", path, "rev-parse", "HEAD").Output(); strings.TrimSpace(string(head)) != first {
		t.Fatal(string(head))
	}
}

func TestGitRefusesPathsThatAreNotItsCheckout(t *testing.T) {
	t.Parallel()
	source, _ := gitRepository(t)
	other, _ := gitRepository(t)
	dir := t.TempDir()
	clone := filepath.Join(dir, "other")
	if r := (Runtime{}).Git(testContext(t), Git{Repo: other, FollowBranch: "main", Path: clone}, value.Scope{}); r.Status != "succeeded" {
		t.Fatal(r)
	}
	if err := os.Mkdir(filepath.Join(dir, "plain"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "file"), nil, 0644); err != nil {
		t.Fatal(err)
	}
	for name, reason := range map[string]string{"plain": "not a Git checkout", "file": "not a Git checkout", "other": "is a checkout of " + other} {
		for _, run := range []Runtime{{Dryrun: true}, {}} {
			r := run.Git(testContext(t), Git{Repo: source, FollowBranch: "main", Path: filepath.Join(dir, name)}, value.Scope{})
			if r.Status != "failed" || !strings.Contains(r.Reason, reason) {
				t.Fatal(name, r)
			}
		}
	}
}

func TestGitNeedsTheGitCommand(t *testing.T) {
	t.Parallel()
	setWorkerEnvironment(t, "PATH", t.TempDir())
	g := Git{Repo: "https://example.com/app.git", FollowBranch: "main", Path: filepath.Join(t.TempDir(), "app")}
	if r := (Runtime{Dryrun: true}).Git(testContext(t), g, value.Scope{}); r.Status != "not_checked" {
		t.Fatal(r)
	}
	if r := (Runtime{}).Git(testContext(t), g, value.Scope{}); r.Status != "failed" || !strings.Contains(r.Reason, missingProgramError("git").Error()) {
		t.Fatal(r)
	}
}

// Credentials reach git only through files the account running it owns, named
// in its environment, and are removed after the task.
func TestGitCredentialsAreTaskFiles(t *testing.T) {
	t.Parallel()
	privateTaskDirectoryRoot := t.TempDir()
	run := withPrivateTaskDirectoryRoot(Runtime{}, privateTaskDirectoryRoot)
	s := value.Scope{"vars": map[string]any{"token": "s3cret-token", "key": "-----BEGIN OPENSSH PRIVATE KEY-----\nb3Blbg==\n-----END OPENSSH PRIVATE KEY-----"}}
	env := map[string]string{}
	cleanup, err := run.gitCredentials(Git{Username: "deploy", Password: "{{ .vars.token }}"}, s, env, nil)
	if err != nil {
		t.Fatal(err)
	}
	for prompt, want := range map[string]string{"Username for 'https://example.com': ": "deploy", "Password for 'https://deploy@example.com': ": "s3cret-token"} {
		out, err := exec.Command(env["GIT_ASKPASS"], prompt).Output()
		if err != nil || string(out) != want {
			t.Fatal(prompt, string(out), err)
		}
	}
	dir := filepath.Dir(env["GIT_ASKPASS"])
	for _, name := range []string{"username", "password"} {
		if info, err := os.Stat(filepath.Join(dir, name)); err != nil || info.Mode().Perm() != 0600 {
			t.Fatal(name, info, err)
		}
	}
	if info, err := os.Stat(dir); err != nil || info.Mode().Perm() != 0700 {
		t.Fatal(info, err)
	}
	cleanup()
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("credentials outlived the task", err)
	}

	env = map[string]string{}
	cleanup, err = run.gitCredentials(Git{SSHPrivateKey: "{{ .vars.key }}", SSHKnownHostsLines: "github.com ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl"}, s, env, nil)
	if err != nil {
		t.Fatal(err)
	}
	command := env["GIT_SSH_COMMAND"]
	fields := strings.Fields(command)
	key := fields[len(fields)-1]
	data, err := os.ReadFile(key)
	if err != nil || !strings.HasSuffix(string(data), "-----END OPENSSH PRIVATE KEY-----\n") || strings.Contains(command, "b3Blbg") {
		t.Fatal(command, string(data), err)
	}
	for _, option := range []string{"-F /dev/null", "IdentityAgent=none", "StrictHostKeyChecking=yes", "UserKnownHostsFile=" + filepath.Join(filepath.Dir(key), "ssh_known_hosts_lines"), "IdentitiesOnly=yes"} {
		if !strings.Contains(command, option) {
			t.Fatal(option, command)
		}
	}
	if env["GIT_ASKPASS"] != "" {
		t.Fatal("an SSH repository got an askpass program")
	}
	cleanup()
	if _, err := run.gitCredentials(Git{}, s, map[string]string{}, nil); err != nil || !slices.Equal(names(t, privateTaskDirectoryRoot), []string{"."}) {
		t.Fatal("wrote a directory without credentials", err)
	}
}

// A stand-in git that asks for its login as git does, and fails telling what
// it got: the user name and the password that its askpass program answers,
// or the key in the file that its SSH command names.
const fakeGitThatTellsItsLogin = `
if [ -n "$GIT_ASKPASS" ]; then
  user=$("$GIT_ASKPASS" "Username for 'https://example.com': ")
  password=$("$GIT_ASKPASS" "Password for 'https://$user@example.com': ")
  echo "remote: the login $user:$password is refused" >&2
  echo "fatal: Authentication failed for 'https://$user@example.com/app.git/'" >&2
else
  echo "Load key: $(cat "${GIT_SSH_COMMAND##* -i }")" >&2
  echo "git@example.com: Permission denied (publickey)." >&2
fi
exit 128`

// git hands its user name, password, and SSH key only to git and ssh. A
// secret in them marks no trace, so the task's result shows, and a failure
// that tells one of them reports [redacted] in its place and keeps the rest
// of its reason. ssh_known_hosts_lines is no login: a secret in it marks the trace.
func TestGitLoginHandedOnlyToGitLeavesTheResultReadable(t *testing.T) {
	t.Parallel()
	ctx := testContext(t)
	standInPrograms(t, map[string]string{"git": fakeGitThatTellsItsLogin})
	privateTaskDirectoryRoot := t.TempDir()
	run := withPrivateTaskDirectoryRoot(Runtime{}, privateTaskDirectoryRoot)
	const user, password = "robot$app+deploy", "sentinel p/w&4711"
	const key = "-----BEGIN OPENSSH PRIVATE KEY-----\nc2VudGluZWw=\n-----END OPENSSH PRIVATE KEY-----"
	const hostKey = "example.com ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl"
	secrets := map[string]string{"user": user, "token": password, "deploy_key": key, "host_key": hostKey}
	path := filepath.Join(t.TempDir(), "app")
	const lsRemote = " refs/heads/main: "

	// HTTPS with a user name and a password.
	https := Git{Repo: "https://example.com/app.git", FollowBranch: "main", Path: path, Username: "{{ .secrets.user }}", Password: "{{ .secrets.token }}"}
	s, trace := taskScope(secrets)
	told := run.Git(ctx, https, s)
	if !strings.Contains(told.Reason, user+":"+password) {
		t.Fatalf("the stand-in git did not tell the login: %s", told.Reason)
	}
	r := reportedByTheEngine(told, s)
	if want := "git ls-remote https://example.com/app.git" + lsRemote + "remote: the login [redacted]:[redacted] is refused\nfatal: Authentication failed for 'https://[redacted]@example.com/app.git/'"; r.Status != "failed" || r.Reason != want || trace.Used {
		t.Fatalf("%+v\nwant reason %s\nthe login marked the trace: %v", r, want, trace.Used)
	}
	// A user name that is written in the playbook is no secret and stays.
	https.Username = user
	s, trace = taskScope(secrets)
	r = reportedByTheEngine(run.Git(ctx, https, s), s)
	if want := "git ls-remote https://example.com/app.git" + lsRemote + "remote: the login " + user + ":[redacted] is refused\nfatal: Authentication failed for 'https://" + user + "@example.com/app.git/'"; r.Reason != want || trace.Used {
		t.Fatalf("%+v\nwant reason %s", r, want)
	}
	// SSH with a key. MMX gives ssh the key with a last line end; the key
	// as the secret holds it is replaced.
	ssh := Git{Repo: "git@example.com:app.git", FollowBranch: "main", Path: path, SSHPrivateKey: "{{ .secrets.deploy_key }}", SSHKnownHostsLines: hostKey}
	s, trace = taskScope(secrets)
	told = run.Git(ctx, ssh, s)
	if !strings.Contains(told.Reason, key) {
		t.Fatalf("the stand-in git did not tell the key: %s", told.Reason)
	}
	r = reportedByTheEngine(told, s)
	if want := "git ls-remote git@example.com:app.git" + lsRemote + "Load key: [redacted]\ngit@example.com: Permission denied (publickey)."; r.Status != "failed" || r.Reason != want || trace.Used {
		t.Fatalf("%+v\nwant reason %s\nthe login marked the trace: %v", r, want, trace.Used)
	}
	// A secret in ssh_known_hosts_lines is another read of a secret: it marks the
	// trace, and the engine then hides the whole result as before.
	ssh.SSHKnownHostsLines = "{{ .secrets.host_key }}"
	s, trace = taskScope(secrets)
	if r = run.Git(ctx, ssh, s); r.Status != "failed" || !trace.Used {
		t.Fatalf("a secret in ssh_known_hosts_lines did not mark the trace: %+v", r)
	}
	if left, _ := os.ReadDir(privateTaskDirectoryRoot); len(left) != 0 {
		t.Fatal("a login directory is left behind", left)
	}
}

func TestGitSelectorsResolveOnlyTheirOwnNamespace(t *testing.T) {
	source, git := gitRepository(t)
	first, second := git("rev-parse", "v1^{commit}"), git("rev-parse", "main")
	git("tag", "main", first)
	git("branch", "branch-only", second)
	for _, tc := range []struct{ branch, tag, commit, want string }{
		{branch: "main", want: second}, {tag: "main", want: first}, {commit: second, want: second},
	} {
		g := Git{Repo: source, Path: filepath.Join(t.TempDir(), "checkout"), FollowBranch: tc.branch, CheckOutTag: tc.tag, CheckOutCommit: tc.commit}
		r := (Runtime{}).Git(testContext(t), g, value.Scope{})
		if r.Status != "succeeded" {
			t.Fatal(r)
		}
		commit, _ := gitResult(t, r.Value)
		if commit != tc.want {
			t.Fatal(commit, tc.want)
		}
	}
	for _, g := range []Git{{FollowBranch: "v1"}, {CheckOutTag: "branch-only"}} {
		g.Repo, g.Path = source, filepath.Join(t.TempDir(), "checkout")
		if r := (Runtime{}).Git(testContext(t), g, value.Scope{}); r.Status != "failed" || !strings.Contains(r.Reason, "does not exist") {
			t.Fatal(r)
		}
	}
	// An annotated tag's object ID is a tag, even though Git can peel it.
	tagObject := git("rev-parse", "refs/tags/v1")
	g := Git{Repo: source, Path: filepath.Join(t.TempDir(), "tag-object"), CheckOutCommit: tagObject}
	if r := (Runtime{}).Git(testContext(t), g, value.Scope{}); r.Status != "failed" || !strings.Contains(r.Reason, "does not name a commit") {
		t.Fatal(r)
	}
	// Refusing the same object in an existing checkout must preserve edits,
	// even when the task allows discarding them for a valid target.
	g.CheckOutCommit, g.FollowBranch = "", "main"
	if r := (Runtime{}).Git(testContext(t), g, value.Scope{}); r.Status != "succeeded" {
		t.Fatal(r)
	}
	path := filepath.Join(g.Path, "app.txt")
	const edited = "uncommitted changes on the node\n"
	if err := os.WriteFile(path, []byte(edited), 0644); err != nil {
		t.Fatal(err)
	}
	g.FollowBranch, g.CheckOutCommit = "", tagObject
	g.FilesChangedInCheckoutOnNode = GitDiscardTheChanges
	if r := (Runtime{}).Git(testContext(t), g, value.Scope{}); r.Status != "failed" || !strings.Contains(r.Reason, "does not name a commit") {
		t.Fatal(r)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != edited {
		t.Fatalf("refusing a tag object changed checkout contents: %q (%v)", data, err)
	}
}

func TestGitFullHistoryDeepensAnUnchangedCommit(t *testing.T) {
	source, _ := gitRepository(t)
	path := filepath.Join(t.TempDir(), "checkout")
	g := Git{Repo: source, Path: path, FollowBranch: "main"}
	if r := (Runtime{}).Git(testContext(t), g, value.Scope{}); r.Status != "succeeded" {
		t.Fatal(r)
	}
	g.FullHistory = true
	if r := (Runtime{Dryrun: true}).Git(testContext(t), g, value.Scope{}); r.Change != "would_change" {
		t.Fatal(r)
	}
	if output, err := gitCommandOfTest("-C", path, "rev-parse", "--is-shallow-repository").Output(); err != nil || strings.TrimSpace(string(output)) != "true" {
		t.Fatal(string(output), err)
	}
	if r := (Runtime{}).Git(testContext(t), g, value.Scope{}); r.Change != "changed" {
		t.Fatal(r)
	}
	if output, err := gitCommandOfTest("-C", path, "rev-list", "--count", "HEAD").Output(); err != nil || strings.TrimSpace(string(output)) != "2" {
		t.Fatal(string(output), err)
	}
	if r := (Runtime{}).Git(testContext(t), g, value.Scope{}); r.Change != "unchanged" {
		t.Fatal(r)
	}
}
