package modules

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/user"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"mmx/internal/protocol"
	"mmx/internal/value"

	"go.yaml.in/yaml/v3"
)

func (run Runtime) Shell(ctx context.Context, sh Shell, s value.Scope, inspection bool) protocol.Result {
	env, account, stop := run.commandEnvironment("shell", sh.User, sh.Env, s)
	if stop != nil {
		return *stop
	}
	if stop := PathGuards(sh.SkipIfThisPathExists, sh.RunOnlyIfThisPathExists, sh.Cwd); stop != nil {
		return *stop
	}
	marker := sh.RunOnlyOnceMarkerFile
	if marker != "" {
		done, err := RunOnlyOnceMarkerFileExists(marker)
		if err != nil {
			return protocol.Failed(err)
		}
		if done {
			return protocol.Result{Status: "skipped", Change: "unchanged", Reason: "the shell command has already run; delete the marker file " + marker + " to run the shell command again"}
		}
	}
	if missing := run.missingRunFromDirectory(sh.Cwd); missing != nil {
		return *missing
	}
	if stop := Guards(ctx, sh.OnlyIf, sh.OnlyIfNot, env, sh.Cwd, account); stop != nil {
		return *stop
	}
	script := sh.Script
	if run.Dryrun {
		if sh.Dryrun == "" && !sh.RunScriptInDryrun && marker != "" {
			return protocol.Result{Status: "succeeded", Change: "would_change", Reason: "would run: the marker file " + marker + " does not exist"}
		}
		if sh.Dryrun == "" && !sh.RunScriptInDryrun {
			return protocol.Result{Status: "skipped", Change: "unknown", Reason: "the shell node task has no dryrun command; set dryrun to a read-only command, or run_script_in_dryrun: true for a script that changes nothing, to check the task"}
		}
		script = sh.Dryrun
		if sh.RunScriptInDryrun {
			script = sh.Script
		}
	}
	if !inspection && !run.Dryrun {
		if err := run.Mutation("shell"); err != nil {
			return protocol.Failed(err)
		}
	}
	p := runProcess(ctx, env, sh.Cwd, nil, account, "/bin/bash", "-c", script)
	r := p.Result
	// Before the predicates read them, so a value derived from them is secret too.
	r.Secret = r.Secret || sh.OutputIsSecret
	r.OutputHoldsNoSecret = sh.OutputHoldsNoSecret
	accepted := sh.SuccessExitCodes
	if accepted == nil || inspection {
		accepted = []int{0}
	}
	// Output that could not be read whole fails the task whatever its exit
	// code, and the result rules, which may read the part of the output that
	// came, keep runProcess's reason.
	incomplete := p.OutputReadFailure != nil
	if !incomplete && hasCode(accepted, r.ExitCode) && r.ExitCode >= 0 && ctx.Err() == nil {
		r.Status = "succeeded"
		r.Reason = ""
	} else {
		r.Status = "failed"
	}
	r.Change = "changed"
	local := s.Local()
	local["this_node_task"] = protocol.ResultValue(r)
	changed, err := local.Predicate(sh.ChangedWhen)
	if err != nil && !incomplete {
		r.Status = "failed"
		r.Reason = err.Error()
		r.FailureKind = "config"
	} else if err == nil && !changed {
		r.Change = "unchanged"
	}
	if sh.FailedWhen != nil && !incomplete {
		b, err := local.Predicate(sh.FailedWhen)
		if err != nil {
			r.Status = "failed"
			r.Reason = err.Error()
			r.FailureKind = "config"
		} else if b {
			r.Status = "failed"
			r.Reason = "failed_when is true"
		}
	}
	if inspection {
		r.Change = "unchanged"
	} else if run.Dryrun {
		if r.Change == "changed" {
			r.Change = "would_change"
		}
	}
	if r.Status == "failed" && r.Reason == "" {
		r.Reason = fmt.Sprintf("shell command exited with unaccepted code %d", r.ExitCode)
	}
	// The marker is written only after success, so a failed or interrupted
	// command runs again on the next run.
	if marker != "" && r.Status == "succeeded" && !inspection && !run.Dryrun {
		if err := run.WriteRunOnlyOnceMarkerFile(ctx, marker, "This command", "runs it again"); err != nil {
			r.Status, r.Reason = "failed", fmt.Sprintf("the command succeeded, but its marker file was not written, so the next run would run it again: %v", err)
		}
	}
	return r
}

// commandEnvironment prepares what a task's command runs with: the account in
// run_as_user_account, looked up now so that an earlier task can create it,
// and the resolved env values. action names the task in a failure.
func (run Runtime) commandEnvironment(action, account string, variables map[string]any, s value.Scope) (map[string]string, *syscall.Credential, *protocol.Result) {
	env := map[string]string{}
	var credential *syscall.Credential
	if account != "" {
		var err error
		if credential, err = shellAccount(account, env); err != nil {
			r := run.accountFailure(fmt.Errorf("%s.run_as_user_account: %w", action, err))
			return nil, nil, &r
		}
	}
	for k, v := range variables {
		if !envName.MatchString(k) {
			r := protocol.Failed(fmt.Errorf("invalid environment variable name %q", k))
			return nil, nil, &r
		}
		x, err := s.Resolve(v)
		if err != nil {
			r := protocol.Failed(err)
			return nil, nil, &r
		}
		text, err := value.EnvironmentText(x)
		if err != nil {
			r := protocol.Failed(err)
			return nil, nil, &r
		}
		env[k] = text
	}
	return env, credential, nil
}

// environmentValue is the rule of an env value.
func environmentValue(v any) error { _, err := value.EnvironmentText(v); return err }

// RunOnlyOnceMarkerFileExists reports whether the marker file of a task
// that does its work once (shell's run_only_once_marker_file, and
// systemd_service's restart_now) is there, in which case the work is done.
// Any entry at the path counts, also a link.
func RunOnlyOnceMarkerFileExists(marker string) (bool, error) {
	_, err := os.Lstat(marker)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

// WriteRunOnlyOnceMarkerFile records that the work of a task with a
// run_only_once_marker_file succeeded: done names the work, such as "This
// command", and again what MMX does once the file is deleted, such as "runs
// it again". It creates the file's missing directories, as for
// /var/lib/mmx/commands_run_once/. The marker is MMX's own record, not part
// of the task's change, so it keeps no difference in the task's result.
func (run Runtime) WriteRunOnlyOnceMarkerFile(ctx context.Context, marker, done, again string) error {
	run.RetainDiff = nil
	var missing []string
	for directory := filepath.Dir(marker); directory != "/"; directory = filepath.Dir(directory) {
		if _, err := os.Stat(directory); !errors.Is(err, os.ErrNotExist) {
			break
		}
		missing = append([]string{directory}, missing...)
	}
	for _, directory := range missing {
		if made := run.MakeDirectory(ctx, directory, "", "", "0755"); made.Error != "" {
			return errors.New(made.Error)
		}
	}
	text := done + " succeeded at " + time.Now().UTC().Format(time.RFC3339) + ". MMX " + again + " only after this file is deleted.\n"
	if written := run.WriteFile(ctx, marker, text, "", "", "0644"); written.Error != "" {
		return errors.New(written.Error)
	}
	return nil
}

// PathGuards checks shell and reboot path guards from the command's directory.
// An omitted directory means /, as for the command itself.
func PathGuards(skipIfThisPathExists, runOnlyIfThisPathExists, directory string) *protocol.Result {
	if directory == "" {
		directory = "/"
	}
	for _, guard := range []struct {
		path         string
		skipIfExists bool
	}{{skipIfThisPathExists, true}, {runOnlyIfThisPathExists, false}} {
		if guard.path == "" {
			continue
		}
		path := guard.path
		if !filepath.IsAbs(path) {
			// Let the kernel resolve .. after following any directory symlink.
			path = strings.TrimSuffix(directory, "/") + "/" + path
		}
		_, err := os.Stat(path)
		exists := err == nil
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			r := protocol.Failed(err)
			return &r
		}
		if exists == guard.skipIfExists {
			return &protocol.Result{Status: "skipped", Change: "unchanged", Reason: "node task skipped by a path condition for: " + path}
		}
	}
	return nil
}

// Guards runs a task's run_only_if_this_command_succeeds and
// skip_if_this_command_succeeds checks. It returns the task's
// result when a check skips or fails the task, and nil when both allow it.
func Guards(ctx context.Context, onlyIf, onlyIfNot *Guard, env map[string]string, cwd string, account *syscall.Credential) *protocol.Result {
	for _, g := range []struct {
		guard  *Guard
		invert bool
	}{{onlyIf, false}, {onlyIfNot, true}} {
		if g.guard == nil {
			continue
		}
		p := runProcess(ctx, env, cwd, nil, account, "/bin/bash", "-c", g.guard.Script)
		r := p.Result
		if p.OutputReadFailure != nil {
			r.Status = "failed"
			r.Change = "unchanged"
			r.Reason = "node task command condition failed: " + r.Reason
			return &r
		}
		yes, no := g.guard.TrueExitCodes, g.guard.FalseExitCodes
		if yes == nil {
			yes = []int{0}
		}
		if no == nil {
			no = []int{1}
		}
		truth := hasCode(yes, r.ExitCode)
		if !truth && !hasCode(no, r.ExitCode) {
			r.Status = "failed"
			r.Change = "unchanged"
			r.Reason = fmt.Sprintf("node task command condition exited with code %d, which true_exit_codes and false_exit_codes do not accept", r.ExitCode)
			return &r
		}
		if truth == g.invert {
			return &protocol.Result{Status: "skipped", Change: "unchanged", Reason: "node task skipped by a command condition"}
		}
	}
	return nil
}

// shellAccount looks up a shell task's account when the task runs, so an
// earlier task can create it. Its UID, primary GID, and groups start the
// task's processes; its home and name set HOME, USER, and LOGNAME in env.
func shellAccount(name string, env map[string]string) (*syscall.Credential, error) {
	u, err := user.Lookup(name)
	if err != nil {
		return nil, err
	}
	groups, err := u.GroupIds()
	if err != nil {
		return nil, err
	}
	ids := make([]uint32, 0, len(groups)+2)
	for _, id := range append([]string{u.Uid, u.Gid}, groups...) {
		n, err := strconv.ParseUint(id, 10, 32)
		if err != nil {
			return nil, fmt.Errorf("account %s has invalid ID %q", name, id)
		}
		ids = append(ids, uint32(n))
	}
	env["HOME"], env["USER"], env["LOGNAME"] = u.HomeDir, u.Username, u.Username
	return &syscall.Credential{Uid: ids[0], Gid: ids[1], Groups: ids[2:]}, nil
}

var envName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func hasCode(codes []int, code int) bool {
	for _, x := range codes {
		if x == code {
			return true
		}
	}
	return false
}

type Shell struct {
	Script string `yaml:"script" json:"script"`
	Dryrun string `yaml:"dryrun,omitempty" json:"dryrun,omitempty"`
	// RunScriptInDryrun makes a dryrun run script as written, where it would
	// run the dryrun command. For a script that changes nothing.
	RunScriptInDryrun       bool           `yaml:"run_script_in_dryrun,omitempty" json:"run_script_in_dryrun,omitempty"`
	Env                     map[string]any `yaml:"env,omitempty" json:"env,omitempty"`
	Cwd                     string         `yaml:"run_from_directory,omitempty" json:"run_from_directory,omitempty"`
	User                    string         `yaml:"run_as_user_account,omitempty" json:"run_as_user_account,omitempty"`
	SuccessExitCodes        []int          `yaml:"success_exit_codes,omitempty" json:"success_exit_codes"`
	ChangedWhen             any            `yaml:"changed_when,omitempty" json:"changed_when,omitempty"`
	FailedWhen              any            `yaml:"failed_when,omitempty" json:"failed_when,omitempty"`
	SkipIfThisPathExists    string         `yaml:"skip_if_this_path_exists,omitempty" json:"skip_if_this_path_exists,omitempty"`
	RunOnlyIfThisPathExists string         `yaml:"run_only_if_this_path_exists,omitempty" json:"run_only_if_this_path_exists,omitempty"`
	OnlyIf                  *Guard         `yaml:"run_only_if_this_command_succeeds,omitempty" json:"run_only_if_this_command_succeeds,omitempty"`
	OnlyIfNot               *Guard         `yaml:"skip_if_this_command_succeeds,omitempty" json:"skip_if_this_command_succeeds,omitempty"`
	// RunOnlyOnceMarkerFile is a file the task creates after the script
	// succeeded; while it exists the script does not run.
	RunOnlyOnceMarkerFile string `yaml:"run_only_once_marker_file,omitempty" json:"run_only_once_marker_file,omitempty"`
	// OnlyWhenTrackedInputsChanged makes a command in a task block's activate
	// phase follow the block's tracked inputs, as a service does.
	OnlyWhenTrackedInputsChanged bool `yaml:"only_when_tracked_inputs_changed,omitempty" json:"only_when_tracked_inputs_changed,omitempty"`
	// OutputIsSecret marks the command's stdout and stderr secret, as a
	// task that reads a secret through env already has them.
	OutputIsSecret bool `yaml:"output_is_secret,omitempty" json:"output_is_secret,omitempty"`
	// OutputHoldsNoSecret promises that the command prints no secret, so its
	// stdout, stderr, and failure reason are shown although the task read a
	// secret, with each secret value that the task read replaced first.
	OutputHoldsNoSecret bool `yaml:"output_holds_no_secret,omitempty" json:"output_holds_no_secret,omitempty"`
	// CommandRecordKey names the command's record on the node; the engine
	// sets it when the task block starts.
	CommandRecordKey string `yaml:"-" json:"-"`
}

type Guard struct {
	Script         string `yaml:"script" json:"script"`
	TrueExitCodes  []int  `yaml:"true_exit_codes,omitempty" json:"true_exit_codes"`
	FalseExitCodes []int  `yaml:"false_exit_codes,omitempty" json:"false_exit_codes"`
}

func ValidateShell(s *Shell) error {
	if s.OutputHoldsNoSecret && s.OutputIsSecret {
		return invalid("output_holds_no_secret", "output_holds_no_secret: true says that the command prints no secret, and output_is_secret: true says that its output is secret; set only one of them")
	}
	for _, code := range s.SuccessExitCodes {
		if code < 0 || code > 255 {
			return invalid("success_exit_codes", "exit codes must be between 0 and 255")
		}
	}
	if err := validateEnvNames(s.Env); err != nil {
		return err
	}
	if s.Script == "" {
		return fmt.Errorf("shell.script is required")
	}
	if s.RunScriptInDryrun {
		switch {
		case s.Dryrun != "":
			return invalid("run_script_in_dryrun", "run_script_in_dryrun: true and dryrun both say what a dryrun runs; remove dryrun to run script in a dryrun, or remove run_script_in_dryrun to run the dryrun command")
		case s.RunOnlyOnceMarkerFile != "":
			return invalid("run_script_in_dryrun", "run_script_in_dryrun: true cannot be used with run_only_once_marker_file: a dryrun already reports whether the script would run, and running the script there would do work that is meant to run once; remove run_script_in_dryrun")
		case s.OnlyWhenTrackedInputsChanged:
			return invalid("run_script_in_dryrun", "run_script_in_dryrun: true cannot be used with only_when_tracked_inputs_changed: a dryrun already reports whether the script would run, and running the script there would do work that is meant to run only after a change; remove run_script_in_dryrun")
		}
	}
	return validateGuards(s.OnlyIf, s.OnlyIfNot)
}

func validateEnvNames(env map[string]any) error {
	for key := range env {
		if !Name.MatchString(key) || strings.Contains(key, "-") {
			return invalid("env."+key, "invalid environment variable name")
		}
	}
	return nil
}

func validateGuards(onlyIf, onlyIfNot *Guard) error {
	for _, g := range []*Guard{onlyIf, onlyIfNot} {
		if g == nil {
			continue
		}
		if g.Script == "" {
			return fmt.Errorf("guard script is required")
		}
		yes, no := g.TrueExitCodes, g.FalseExitCodes
		if yes == nil {
			yes = []int{0}
		}
		if no == nil {
			no = []int{1}
		}
		for _, c := range append(append([]int{}, yes...), no...) {
			if c < 0 || c > 255 {
				return fmt.Errorf("invalid guard exit code %s", strconv.Itoa(c))
			}
		}
		for _, a := range yes {
			for _, b := range no {
				if a == b {
					return fmt.Errorf("command condition true_exit_codes and false_exit_codes must not contain the same exit code")
				}
			}
		}
	}
	return nil
}

func (g *Guard) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode && n.Tag == "!!str" {
		g.Script = n.Value
		return nil
	}
	if n.Kind != yaml.MappingNode {
		return fmt.Errorf("line %d: guard must be a command string or mapping", n.Line)
	}
	for i := 0; i < len(n.Content); i += 2 {
		switch n.Content[i].Value {
		case "script", "true_exit_codes", "false_exit_codes":
		default:
			return fmt.Errorf("line %d: unknown guard field %q", n.Content[i].Line, n.Content[i].Value)
		}
	}
	type plain Guard
	return n.Decode((*plain)(g))
}

// shellDescription is what mmx describe-module shows for shell.
var shellDescription = Description{
	Name: "shell", Topic: TopicCommandsChecksAndReboot, Schema: reflect.TypeOf(Shell{}),
	Summary: "a Bash script run as root or an application user, with optional guards, dryrun command, and result rules",
	Fields: []Field{
		{Name: "script", Required: true, Notes: "run with bash -c; no profile files"},
		{Name: "dryrun", Notes: "read-only command run instead during dryrun"},
		{Name: "run_script_in_dryrun", Default: "false", Notes: "true runs script as written during a dryrun, with the same guards, run_as_user_account, run_from_directory, env, and result rules; for a script that changes nothing, because a dryrun must not change a node: MMX does not check that the script only reads; cannot be used with dryrun, run_only_once_marker_file, or only_when_tracked_inputs_changed"},
		{Name: "env", Notes: "environment variables; pass values here, not in script"},
		{Name: "run_from_directory", Default: "/", Notes: "the directory the script, guards, and dryrun command run from; a directory that does not exist is a missing prerequisite: a run fails without another try, and a dryrun reports the node task not checked when an earlier node task would change the node"},
		{Name: "run_as_user_account", Default: "root", Notes: "local account for the script, guards, and dryrun command: its UID, primary GID, and groups, with HOME, USER, and LOGNAME from it; no sudo or login shell"},
		{Name: "skip_if_this_path_exists", Notes: "skip when this path exists; relative to run_from_directory"},
		{Name: "run_only_if_this_path_exists", Notes: "run only when this path exists; relative to run_from_directory"},
		{Name: "run_only_if_this_command_succeeds", Type: "command or mapping", Notes: "run only when the check exits 0; 1 skips; {script, true_exit_codes, false_exit_codes}"},
		{Name: "skip_if_this_command_succeeds", Type: "command or mapping", Notes: "run only when the check exits 1; 0 skips"},
		{Name: "run_only_once_marker_file", Notes: "absolute path of a file MMX creates after the script succeeded; while the file exists the script is skipped; delete the file to run the script again; keep such files in /var/lib/mmx/commands_run_once/"},
		{Name: "only_when_tracked_inputs_changed", Default: "false", Notes: "only in a task_block's activate phase, on a task with an id: the command runs when the block's tracked inputs (the files, units, packages, and Git checkouts in its changes) differ from what it last ran with, also when an earlier run stopped before the command"},
		{Name: "success_exit_codes", Default: "[0]"},
		{Name: "changed_when", Type: "boolean or predicate", Default: "true", Notes: "reads .this_node_task.exit_code, .this_node_task.stdout, .this_node_task.stderr"},
		{Name: "failed_when", Type: "boolean or predicate", Default: "false", Notes: "an additional failure condition"},
		{Name: "output_holds_no_secret", Default: "false", Notes: "true promises that the program prints no secret: a run and a dryrun show stdout, stderr, and the failure reason although the task read a secret, with every secret value of the node job replaced by [redacted] first; diffs and outputs stay hidden; cannot be true with output_is_secret"},
		{Name: "output_is_secret", Default: "false", Notes: "true marks stdout and stderr secret: never printed, never saved; an output made from them is secret too; on mmx resume, work that reads such an output is blocked until a new mmx run produces it again"},
	},
	Example: "- shell:\n    script: /usr/local/bin/migrate\n    skip_if_this_path_exists: /srv/app/.migrated\n",
	Results: []string{"status", "change", "exit_code", "stdout", "stdout_json", "stderr"},
	Dryrun:  "runs only the dryrun command, or script as written with run_script_in_dryrun: true; without either, guards may skip it, otherwise it is skipped with change unknown, or unchanged in a task_block's validate while no earlier node task of the dryrun would change the node; a check (in validate, or with changed_when: false) that fails after an earlier node task of the dryrun would change the node is not_checked, with its output kept; with run_only_once_marker_file or only_when_tracked_inputs_changed it reports whether the command would run",
}

// Prepare checks the inputs when the project loads.
func (s *Shell) Prepare(base string) error {
	return ValidateShell(s)
}

// Arguments lists the inputs that may hold a reference, with their rules.
func (s *Shell) Arguments(arguments *ArgumentList) {
	arguments.Text("run_from_directory", &s.Cwd)
	arguments.Text("skip_if_this_path_exists", &s.SkipIfThisPathExists)
	arguments.Text("run_only_if_this_path_exists", &s.RunOnlyIfThisPathExists)
	if s.RunOnlyOnceMarkerFile != "" {
		arguments.Public("run_only_once_marker_file", &s.RunOnlyOnceMarkerFile, absolute)
	}
	if s.User != "" {
		arguments.Public("run_as_user_account", &s.User, accountNameRule)
	}
	for _, k := range slices.Sorted(maps.Keys(s.Env)) {
		arguments.Private("env."+k, s.Env[k], environmentValue)
	}
}

// Reads lists the command itself and its result rules, which read the
// command's own result.
func (s *Shell) Reads() Reads {
	scripts := map[string]string{"script": s.Script, "dryrun": s.Dryrun}
	if s.OnlyIf != nil {
		scripts["run_only_if_this_command_succeeds.script"] = s.OnlyIf.Script
	}
	if s.OnlyIfNot != nil {
		scripts["skip_if_this_command_succeeds.script"] = s.OnlyIfNot.Script
	}
	return Reads{
		Commands:         map[string]*Shell{"": s},
		ResultConditions: map[string]any{"changed_when": s.ChangedWhen, "failed_when": s.FailedWhen}, //nolint:nilaway // validateCheck requires nested shells; unresolved whole-value commands are removed, and resolved commands are prepared before Reads.
		ShellScripts:     scripts,
	}
}

// OnlyReads accepts a command in checks_before_restart_or_reload only when it
// says changed_when: false.
func (s *Shell) OnlyReads() error {
	if changed, literal := s.ChangedWhen.(bool); literal && !changed {
		return nil
	}
	return fmt.Errorf("a shell task in checks_before_restart_or_reload must say changed_when: false: the checks run before the task_block's first write and must change nothing")
}

// ResultIsSecret reports a command whose output is secret whatever it
// prints: output_is_secret: true.
func (s *Shell) ResultIsSecret() bool {
	return s.OutputIsSecret
}

// Run runs the command. A command that follows its node task block's
// tracked inputs runs through the node task block's record of them.
func (s *Shell) Run(ctx context.Context, task NodeTask) protocol.Result {
	if s.OnlyWhenTrackedInputsChanged {
		return task.TaskBlock.RunCommandThatFollowsTrackedInputs(ctx, s.CommandRecordKey, func() protocol.Result {
			return task.Runtime.Shell(ctx, *s, task.Scope, false)
		})
	}
	return task.Runtime.Shell(ctx, *s, task.Scope, task.Checking)
}

// ManagedPaths lists the paths the task manages or says it creates, as
// resolved from run_from_directory; a dryrun cannot check a later wait on them.
func (s *Shell) ManagedPaths() []string {
	path := s.SkipIfThisPathExists
	if path != "" && !filepath.IsAbs(path) {
		directory := s.Cwd
		if directory == "" {
			directory = "/"
		}
		// Let the kernel resolve .. after following any directory symlink.
		path = strings.TrimSuffix(directory, "/") + "/" + path
	}
	return []string{path, s.RunOnlyOnceMarkerFile}
}
