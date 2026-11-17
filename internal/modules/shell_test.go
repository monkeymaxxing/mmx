package modules

import (
	"context"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"mmx/internal/value"
)

func TestShellReportingAndGuardErrors(t *testing.T) {
	t.Parallel()
	run := Runtime{}
	s := value.Scope{}
	r := run.Shell(context.Background(), Shell{Script: "printf hi; exit 2", SuccessExitCodes: []int{0, 2}, ChangedWhen: map[string]any{"equals": []any{"{{ .this_node_task.exit_code }}", 2}}}, s, false)
	if r.Status != "succeeded" || r.Change != "changed" || r.Stdout != "hi" {
		t.Fatal(r)
	}
	r = run.Shell(context.Background(), Shell{Script: "exit 0", SuccessExitCodes: []int{2}}, s, false)
	if r.Status != "failed" {
		t.Fatal("exit zero ignored the declared success codes", r)
	}
	r = run.Shell(context.Background(), Shell{Script: "exit 5", FailedWhen: false}, s, false)
	if r.Status != "failed" {
		t.Fatal("failed_when hid bad exit")
	}
	r = run.Shell(context.Background(), Shell{Script: "echo SHOULD_NOT_RUN", OnlyIfNot: &Guard{Script: "exit 7"}}, s, false)
	if r.Status != "failed" || r.Stdout != "" {
		t.Fatal("skip_if_this_command_succeeds inverted guard failure", r)
	}
	r = run.Shell(context.Background(), Shell{Script: "echo SHOULD_NOT_RUN", OnlyIfNot: &Guard{Script: "exit 0"}}, s, false)
	if r.Status != "skipped" {
		t.Fatal(r)
	}
	// A command that is not installed says so, not only "exit status 127".
	r = run.Shell(context.Background(), Shell{Script: "a-program-that-is-not-installed --version"}, s, false)
	if r.Status != "failed" || r.ExitCode != 127 || r.Reason != "shell command exited with code 127: bash could not find a command in it, usually because its program is not installed yet" {
		t.Fatal("a missing program's failure does not say what is missing", r)
	}
}

func TestShellOutputPredicatesAndTruncation(t *testing.T) {
	t.Parallel()
	contains := func(ref, pattern string) any { return map[string]any{"contains": []any{ref, pattern}} }
	matches := func(ref, pattern string) any { return map[string]any{"matches": []any{ref, pattern}} }
	run := Runtime{}
	for _, tc := range []struct {
		name           string
		shell          Shell
		status, change string
		truncated      bool
	}{
		{"zero exit additional failure", Shell{Script: "echo applied; echo ERROR:bad >&2", ChangedWhen: contains("{{ .this_node_task.stdout }}", "applied"), FailedWhen: matches("{{ .this_node_task.stderr }}", "^ERROR:")}, "failed", "changed", false},
		{"unchanged", Shell{Script: "echo unchanged", ChangedWhen: contains("{{ .this_node_task.stdout }}", "applied")}, "succeeded", "unchanged", false},
		{"truncated consumed", Shell{Script: "head -c 1048577 /dev/zero", ChangedWhen: contains("{{ .this_node_task.stdout }}", "anything")}, "failed", "changed", true},
		{"truncated unused", Shell{Script: "head -c 1048577 /dev/zero", ChangedWhen: map[string]any{"any": []any{true, contains("{{ .this_node_task.stdout }}", "anything")}}}, "succeeded", "changed", true},
		{"stderr truncated", Shell{Script: "head -c 1048577 /dev/zero >&2", FailedWhen: matches("{{ .this_node_task.stderr }}", "anything")}, "failed", "changed", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := run.Shell(context.Background(), tc.shell, value.Scope{}, false)
			if r.Status != tc.status || r.Change != tc.change || r.ExitCode != 0 || (r.StdoutTruncated || r.StderrTruncated) != tc.truncated {
				t.Fatalf("%s / %s exit %d: %s", r.Status, r.Change, r.ExitCode, r.Reason)
			}
			if tc.truncated && tc.status == "failed" && r.FailureKind != "config" {
				t.Fatal(r.FailureKind, r.Reason)
			}
		})
	}
}

// An env value that is a list or a mapping reaches the script as JSON text,
// and its rule accepts it. A secret inside the value still makes the task's
// result secret.
func TestShellEnvPassesAListOrAMappingAsJSONText(t *testing.T) {
	t.Parallel()
	scope, trace := value.Scope{
		"vars":    map[string]any{"ports": []any{80, 443}, "site": map[string]any{"city": "Frankfurt"}},
		"secrets": map[string]any{"database": &value.Secret{Name: "database", Data: map[string]any{"user": "app", "password": "pw"}, Available: true}},
	}.Trace()
	sh := Shell{Script: `printf '%s\n' "$PORTS" "$SITE"`, Env: map[string]any{"PORTS": "{{ .vars.ports }}", "SITE": "{{ .vars.site }}"}}
	if r := (Runtime{}).Shell(context.Background(), sh, scope, false); r.Status != "succeeded" || r.Stdout != "[80,443]\n{\"city\":\"Frankfurt\"}\n" || trace.Used {
		t.Fatalf("%s %q %s; secret read: %v", r.Status, r.Stdout, r.Reason, trace.Used)
	}
	secret := Shell{Script: `printf '%s' "$DATABASE"`, Env: map[string]any{"DATABASE": "{{ .secrets.database }}"}}
	if r := (Runtime{}).Shell(context.Background(), secret, scope, false); r.Status != "succeeded" || r.Stdout != `{"password":"pw","user":"app"}` || !trace.Used {
		t.Fatalf("%s %q %s; secret read: %v", r.Status, r.Stdout, r.Reason, trace.Used)
	}
	var arguments ArgumentList
	sh.Arguments(&arguments)
	for _, argument := range arguments.List {
		resolved, err := scope.Resolve(argument.Raw)
		if err != nil {
			t.Fatal(err)
		}
		if err := argument.Rule(resolved); err != nil {
			t.Errorf("%s: %v", argument.Field, err)
		}
	}
}

// A missing account fails the task before anything runs; dryrun cannot check
// an account an earlier task would create.
func TestShellUserMustExist(t *testing.T) {
	t.Parallel()
	sh := Shell{Script: "echo ran", Dryrun: "echo ran", User: "mmx-no-such-account"}
	r := Runtime{}.Shell(context.Background(), sh, value.Scope{}, false)
	if r.Status != "failed" || r.Stdout != "" || !strings.Contains(r.Reason, "shell.run_as_user_account") {
		t.Fatal(r)
	}
	r = Runtime{Dryrun: true}.Shell(context.Background(), sh, value.Scope{}, false)
	if r.Status != "not_checked" || r.Change != "unknown" || r.Stdout != "" {
		t.Fatal(r)
	}
}

// The script, its guards, and its dryrun command run with the account's IDs and
// environment, without a login shell; env still overrides that environment.
func TestShellRunsAsApplicationUser(t *testing.T) {
	t.Parallel()
	if runtime.GOOS != "linux" || os.Geteuid() != 0 {
		t.Skip("requires root on Linux")
	}
	account, err := user.Lookup("nobody")
	if err != nil {
		t.Skip("requires the nobody account")
	}
	groups, err := account.GroupIds()
	if err != nil {
		t.Fatal(err)
	}
	slices.Sort(groups)
	script := `id -u; id -g; id -G | tr ' ' '\n' | sort -u | paste -sd ' '; echo "$HOME $USER $LOGNAME"; pwd; echo "$EXTRA"`
	want := strings.Join([]string{account.Uid, account.Gid, strings.Join(slices.Compact(groups), " "),
		account.HomeDir + " nobody nobody", "/tmp", "extra", ""}, "\n")
	sh := Shell{Script: script, Dryrun: script, User: "nobody", Cwd: "/tmp", Env: map[string]any{"EXTRA": "extra"},
		OnlyIf: &Guard{Script: `test "$(id -un)" = nobody`}}
	for _, run := range []Runtime{{}, {Dryrun: true}} {
		r := run.Shell(context.Background(), sh, value.Scope{}, false)
		if r.Status != "succeeded" || r.Stdout != want {
			t.Fatalf("dryrun %v: %s %q, want %q: %s", run.Dryrun, r.Status, r.Stdout, want, r.Stderr)
		}
	}
	home := Shell{Script: "echo $HOME", User: "nobody", Env: map[string]any{"HOME": "/srv/other"}}
	if r := (Runtime{}).Shell(context.Background(), home, value.Scope{}, false); r.Stdout != "/srv/other\n" {
		t.Fatal("env did not override the account's HOME", r)
	}
	private := Shell{Script: "echo ran", User: "nobody", Cwd: "/root"}
	if r := (Runtime{}).Shell(context.Background(), private, value.Scope{}, false); r.Status != "failed" || r.Stdout != "" {
		t.Fatal("the account entered a directory it cannot open", r)
	}
}

// The marker file is written only after the command succeeded; while it
// exists the command is skipped, and dryrun reports from the file.
func TestRunOnlyOnceMarkerFile(t *testing.T) {
	t.Parallel()
	marker := filepath.Join(t.TempDir(), "commands_run_once", "import")
	count := filepath.Join(t.TempDir(), "count")
	sh := Shell{Script: "echo ran >> " + count + "; exit 3", RunOnlyOnceMarkerFile: marker}
	if r := (Runtime{}).Shell(context.Background(), sh, value.Scope{}, false); r.Status != "failed" {
		t.Fatal(r)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("a failed command left its marker file", err)
	}
	sh.Script = "echo ran >> " + count
	r := Runtime{Dryrun: true}.Shell(context.Background(), sh, value.Scope{}, false)
	if r.Status != "succeeded" || r.Change != "would_change" || !strings.Contains(r.Reason, "would run: the marker file") {
		t.Fatal(r)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("dryrun wrote the marker file", err)
	}
	if r := (Runtime{}).Shell(context.Background(), sh, value.Scope{}, false); r.Status != "succeeded" || r.Change != "changed" {
		t.Fatal(r)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatal(err)
	}
	for _, run := range []Runtime{{}, {Dryrun: true}} {
		r := run.Shell(context.Background(), sh, value.Scope{}, false)
		if r.Status != "skipped" || r.Change != "unchanged" || r.Reason != "the shell command has already run; delete the marker file "+marker+" to run the shell command again" {
			t.Fatal(r)
		}
	}
	if b, _ := os.ReadFile(count); string(b) != "ran\nran\n" {
		t.Fatalf("the command ran %q", b)
	}
}

// A secret command output that a changed_when cannot compare is not quoted in
// the reason, which configuration failures keep visible.
func TestSecretOutputStaysOutOfConditionErrors(t *testing.T) {
	t.Parallel()
	sh := Shell{Script: "printf s3cr3t", OutputIsSecret: true, ChangedWhen: map[string]any{"number_at_least": []any{"{{ .this_node_task.stdout }}", 1}}}
	r := (Runtime{}).Shell(context.Background(), sh, value.Scope{}, false)
	if r.Status != "failed" || r.FailureKind != "config" || strings.Contains(r.Reason, "s3cr3t") || strings.Contains(r.Safe().Reason, "s3cr3t") || !strings.Contains(r.Reason, "a secret value") {
		t.Fatal(r.Reason)
	}
}

func TestShellPathGuardsUseRunFromDirectory(t *testing.T) {
	t.Parallel()
	directory := t.TempDir()
	directory, err := filepath.EvalSymlinks(directory)
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(directory, "marker")
	if err := os.WriteFile(marker, nil, 0600); err != nil {
		t.Fatal(err)
	}
	for _, dryrun := range []bool{false, true} {
		for _, test := range []struct {
			name, skip, only, status string
		}{
			{"skip existing", "marker", "", "skipped"},
			{"skip missing", "missing", "", "succeeded"},
			{"require existing", "", "marker", "succeeded"},
			{"require missing", "", "missing", "skipped"},
		} {
			t.Run(fmt.Sprintf("%s dryrun=%t", test.name, dryrun), func(t *testing.T) {
				sh := Shell{Script: "pwd", Dryrun: "pwd", Cwd: directory, SkipIfThisPathExists: test.skip, RunOnlyIfThisPathExists: test.only}
				r := (Runtime{Dryrun: dryrun}).Shell(context.Background(), sh, nil, false)
				if r.Status != test.status {
					t.Fatalf("%+v", r)
				}
				if test.status == "succeeded" && strings.TrimSpace(r.Stdout) != directory {
					t.Fatalf("command ran from %q, want %q", r.Stdout, directory)
				}
			})
		}
	}
}

func TestShellPathGuardsResolveParentThroughWorkingDirectorySymlink(t *testing.T) {
	t.Parallel()
	application := t.TempDir()
	release := filepath.Join(application, "releases", "v2")
	if err := os.MkdirAll(release, 0755); err != nil {
		t.Fatal(err)
	}
	current := filepath.Join(application, "current")
	if err := os.Symlink(release, current); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Join(application, "releases", ".migrated"), filepath.Join(application, ".not-migrated")} {
		if err := os.WriteFile(path, nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	for _, test := range []struct {
		path, command string
		exists        bool
	}{
		{"../.migrated", "test -f ../.migrated && printf guard-passed", true},
		{"../.not-migrated", "test ! -e ../.not-migrated && printf guard-passed", false},
	} {
		for _, dryrun := range []bool{false, true} {
			for _, skipIfExists := range []bool{false, true} {
				sh := Shell{Cwd: current, Script: test.command, Dryrun: test.command}
				if skipIfExists {
					sh.SkipIfThisPathExists = test.path
				} else {
					sh.RunOnlyIfThisPathExists = test.path
				}
				r := (Runtime{Dryrun: dryrun}).Shell(context.Background(), sh, nil, false)
				want := "succeeded"
				if test.exists == skipIfExists {
					want = "skipped"
				}
				if r.Status != want || (want == "succeeded" && r.Stdout != "guard-passed") {
					t.Errorf("path=%s dryrun=%t skipIfExists=%t: %+v; want %s", test.path, dryrun, skipIfExists, r, want)
				}
			}
		}
		sh := Shell{Cwd: current, SkipIfThisPathExists: test.path}
		_, err := os.Stat(sh.ManagedPaths()[0])
		if (err == nil) != test.exists || (err != nil && !os.IsNotExist(err)) {
			t.Errorf("managed path %s: %v; want exists=%t", sh.ManagedPaths()[0], err, test.exists)
		}
	}
}

// run_script_in_dryrun is refused with each input that already answers what a
// dryrun does, and the error names the input and the way out.
func TestRunScriptInDryrunIsRefusedWithInputsThatAnswerTheSameQuestion(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		sh   Shell
		want []string
	}{
		"dryrun":                           {Shell{Script: "true", RunScriptInDryrun: true, Dryrun: "true"}, []string{"dryrun", "remove dryrun"}},
		"run_only_once_marker_file":        {Shell{Script: "true", RunScriptInDryrun: true, RunOnlyOnceMarkerFile: "/var/lib/mmx/commands_run_once/x"}, []string{"run_only_once_marker_file", "meant to run once", "remove run_script_in_dryrun"}},
		"only_when_tracked_inputs_changed": {Shell{Script: "true", RunScriptInDryrun: true, OnlyWhenTrackedInputsChanged: true}, []string{"only_when_tracked_inputs_changed", "only after a change", "remove run_script_in_dryrun"}},
	}
	for name, c := range cases {
		err := ValidateShell(&c.sh)
		if err == nil {
			t.Fatalf("%s: not refused", name)
		}
		for _, w := range c.want {
			if !strings.Contains(err.Error(), w) {
				t.Errorf("%s: error %q lacks %q", name, err, w)
			}
		}
	}
	ok := Shell{Script: "true", RunScriptInDryrun: true}
	if err := ValidateShell(&ok); err != nil {
		t.Fatal(err)
	}
}

// A dryrun runs script as written and applies the result rules; a run is not
// affected; a guard still skips the task in a dryrun.
func TestRunScriptInDryrunRunsTheScript(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	count := filepath.Join(dir, "count")
	sh := Shell{Script: "echo ran >> " + count + "; echo hello", RunScriptInDryrun: true, ChangedWhen: false}
	r := Runtime{Dryrun: true}.Shell(context.Background(), sh, value.Scope{}, false)
	if r.Status != "succeeded" || r.Change != "unchanged" || r.Stdout != "hello\n" {
		t.Fatal(r)
	}
	sh.ChangedWhen = nil
	r = Runtime{Dryrun: true}.Shell(context.Background(), sh, value.Scope{}, false)
	if r.Status != "succeeded" || r.Change != "would_change" {
		t.Fatal(r)
	}
	r = Runtime{}.Shell(context.Background(), sh, value.Scope{}, false)
	if r.Status != "succeeded" || r.Change != "changed" {
		t.Fatal(r)
	}
	guarded := Shell{Script: "echo ran >> " + count, RunScriptInDryrun: true, SkipIfThisPathExists: dir}
	r = Runtime{Dryrun: true}.Shell(context.Background(), guarded, value.Scope{}, false)
	if r.Status != "skipped" {
		t.Fatal(r)
	}
	if b, _ := os.ReadFile(count); string(b) != "ran\nran\nran\n" {
		t.Fatalf("the script ran %q", b)
	}
	// Without the setting, a dryrun still skips the task with change unknown.
	plain := Shell{Script: "echo ran >> " + count}
	if r := (Runtime{Dryrun: true}).Shell(context.Background(), plain, value.Scope{}, false); r.Status != "skipped" || r.Change != "unknown" {
		t.Fatal(r)
	}
}
