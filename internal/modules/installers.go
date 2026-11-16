package modules

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"

	"mmx/internal/protocol"
	"mmx/internal/value"
)

// The code the language and tool installers share: pip_package,
// gem_package, npm_global_package, snap_package, and the five
// ..._project_dependencies modules. Each runs the installer program on
// the node and links no library into MMX.

// standardPath is the PATH an installer starts with, as a shell task's;
// tests replace it.
var standardPath = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

// installer is one task's installer program, with the account and
// environment it runs with.
type installer struct {
	run Runtime
	// module names the task's module in failures.
	module string
	// program is the absolute path of the installer program.
	program string
	env     map[string]string
	account *syscall.Credential
}

// installerSettings is what a task says about how its installer runs.
type installerSettings struct {
	// program is the installer's name on the standard path, such as pip or
	// npm; programPath is the task's <tool>_program_path, written as
	// programField, which replaces it.
	program, programPath, programField string
	// providedBy says what to install for a missing program that
	// programPackages does not know, such as Yarn, which is no package of
	// every distribution.
	providedBy string
	// account is run_as_user_account; "" runs the installer as root.
	account string
	// variables is the task's env, resolved in scope.
	variables map[string]any
	scope     value.Scope
}

// newInstaller finds the installer program and the account. A missing
// program or account is something an earlier task may still provide.
func (run Runtime) newInstaller(module string, settings installerSettings) (*installer, *protocol.Result) {
	env, account, stop := run.commandEnvironment(module, settings.account, settings.variables, settings.scope)
	if stop != nil {
		return nil, stop
	}
	// The account that already runs MMX needs no switch.
	if account != nil && int(account.Uid) == os.Geteuid() {
		account = nil
	}
	path := standardPath
	if written, ok := env["PATH"]; ok {
		path = written
	}
	program := settings.programPath
	if program == "" {
		program = lookUpProgram(settings.program, standardPath)
		if program == "" {
			message := fmt.Sprintf("%s: %s", module, missingProgramError(settings.program))
			if _, known := programPackages[settings.program]; !known {
				message = fmt.Sprintf("%s: the program %s is not installed; install %s in an earlier node task", module, settings.program, settings.providedBy)
			}
			if settings.programField != "" {
				message += ", or name the program in " + settings.programField
			}
			r := run.notYet(errors.New(message))
			return nil, &r
		}
	} else {
		if !executableFile(program) {
			r := run.notYet(fmt.Errorf("%s: %s %s is not an executable program; an earlier node task may install the program", module, settings.programField, program))
			return nil, &r
		}
		// The program's own directory comes first, so that npm finds the
		// node beside it.
		path = filepath.Dir(program) + string(os.PathListSeparator) + path
	}
	env["PATH"] = path
	if _, set := env["LANG"]; !set {
		env["LANG"] = "C.UTF-8" //nolint:nilaway // Every nil environment return carries a non-nil stop; successful commandEnvironment returns an allocated map.
	}
	return &installer{run: run, module: module, program: program, env: env, account: account}, nil
}

// lookUpProgram finds name in the directories of path.
func lookUpProgram(name, path string) string {
	for _, directory := range filepath.SplitList(path) {
		candidate := filepath.Join(directory, name)
		if executableFile(candidate) {
			return candidate
		}
	}
	return ""
}

func executableFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular() && info.Mode()&0111 != 0 //nolint:nilaway // err == nil short-circuits both method calls; successful os.Stat supplies non-nil file information.
}

// read runs a program that only reads, such as a listing of what is
// installed, in directory, and returns its standard output.
func (i *installer) read(ctx context.Context, directory string, program string, args ...string) (string, error) {
	if directory == "" {
		directory = "/"
	}
	r := runProcess(ctx, i.env, directory, nil, i.account, program, args...).Result
	if r.Status != "succeeded" || ctx.Err() != nil {
		return r.Stdout, installerCommandFailure(program, args, r)
	}
	if r.StdoutTruncated {
		return "", fmt.Errorf("%s %s printed more than 1 MiB", filepath.Base(program), strings.Join(args, " "))
	}
	return r.Stdout, nil
}

// readOutput runs a program that only reads and returns what it printed on
// both streams, for tools that print their report to standard error, as
// composer does.
func (i *installer) readOutput(ctx context.Context, directory string, program string, args ...string) (string, error) {
	if directory == "" {
		directory = "/"
	}
	r := runProcess(ctx, i.env, directory, nil, i.account, program, args...).Result
	if r.Status != "succeeded" || ctx.Err() != nil {
		return "", installerCommandFailure(program, args, r)
	}
	return r.Stdout + "\n" + r.Stderr, nil
}

// change runs a program that changes the node, after the mutation boundary;
// resource names what changes, in the boundary's words.
func (i *installer) change(ctx context.Context, resource, directory, program string, args ...string) (string, error) {
	if err := i.run.Mutation(i.module + " " + resource); err != nil {
		return "", err
	}
	return i.read(ctx, directory, program, args...)
}

// installerCommandFailure is a failed command with the end of what it printed, where
// installers explain their failures.
func installerCommandFailure(program string, args []string, r protocol.Result) error {
	detail := lastLines(strings.TrimSpace(r.Stderr), 20)
	if detail == "" {
		detail = lastLines(strings.TrimSpace(r.Stdout), 20)
	}
	if detail == "" {
		detail = r.Reason
	}
	return fmt.Errorf("%s %s: %s", filepath.Base(program), strings.Join(args, " "), detail)
}

func lastLines(text string, count int) string {
	lines := strings.Split(text, "\n")
	if len(lines) > count {
		lines = append([]string{"..."}, lines[len(lines)-count:]...)
	}
	return strings.Join(lines, "\n")
}

// installedChange is one entry of an installer task's result value: a
// package whose installed version differs, from null when it was missing,
// to null when it is gone.
func installedChange(name string, from, to any) map[string]any {
	return map[string]any{"name": name, "from": from, "to": to}
}

// installedDifferences compares two listings of installed versions by name.
// A name with several versions lists them joined by ", ".
func installedDifferences(before, after map[string]string) []any {
	names := map[string]bool{}
	for name := range before {
		names[name] = true
	}
	for name := range after {
		names[name] = true
	}
	var out []any
	for _, name := range slices.Sorted(maps.Keys(names)) {
		was, had := before[name]
		now, has := after[name]
		if had && has && was == now {
			continue
		}
		var from, to any
		if had {
			from = was
		}
		if has {
			to = now
		}
		out = append(out, installedChange(name, from, to))
	}
	return out
}

// installedListing is a tracked input's text: one "name version" line for
// each entry, sorted, so that the same installation hashes the same.
func installedListing(installed map[string]string) string {
	var b strings.Builder
	for _, name := range slices.Sorted(maps.Keys(installed)) {
		b.WriteString(name + " " + installed[name] + "\n")
	}
	return b.String()
}

// installerResult is a named-thing installer's result: the differences in
// its value, changed when there are any.
func installerResult(run Runtime, differences []any) protocol.Result {
	change := "unchanged"
	if len(differences) > 0 {
		change = "changed"
		if run.Dryrun {
			change = "would_change"
		}
	}
	r := protocol.Success(change)
	if differences == nil {
		differences = []any{}
	}
	r.Value, r.ValueSet = differences, true
	return r
}

// installerFailure keeps what an installer changed before it failed.
func installerFailure(err error, differences []any) protocol.Result {
	r := protocol.Failed(err)
	if len(differences) > 0 {
		r.Change = "changed"
		r.Value, r.ValueSet = differences, true
	}
	return r
}

// stateWouldChange is the state a dryrun predicts for an installation that
// would change. No listing of a tool reads like it, so a service compares it
// as changed.
const stateWouldChange = "\x00would change"

// installerStateAfter is what a dryrun predicts for an installer's tracked
// input: changed when the task would change, and also when it cannot tell,
// such as with a pip older than 22.2: a dryrun that predicts a restart that
// does not come is safer than one that misses a restart. An unchanged task
// leaves the input as the node has it.
func installerStateAfter(r protocol.Result) (string, bool) {
	if (r.Status == "succeeded" && r.Change == "would_change") || r.Status == "not_checked" {
		return stateWouldChange, true
	}
	return "", false
}

// installerNames is the rule for one name an installer task lists: a word
// that cannot be read as an option of the installer.
func installerNames(field string) func(any) error {
	return rule(field+" must be one name without spaces that does not start with -", func(s string) bool {
		return s != "" && !strings.HasPrefix(s, "-") && !strings.ContainsAny(s, " \t\r\n\x00")
	})
}

// InstallerPackage is one named language package and its version choice.
type InstallerPackage struct {
	Name    string `yaml:"name" json:"name"`
	Version string `yaml:"version,omitempty" json:"version,omitempty"`
}

func installerPackageNames(packages []InstallerPackage) []string {
	names := make([]string, len(packages))
	for index, entry := range packages {
		names[index] = entry.Name
	}
	return names
}

func checkInstallerPackages(packages []InstallerPackage, state, account string, gemRemoval bool, versionRule string) error {
	if len(packages) == 0 {
		return invalid("packages", "packages is required: list the packages this task manages")
	}
	if state != "" && !slices.Contains(presenceStates, state) {
		return invalid("state", "state must be present or absent")
	}
	seen := map[string]bool{}
	for index, entry := range packages {
		field := fmt.Sprintf("packages[%d]", index)
		if entry.Name == "" {
			return invalid(field+".name", "name is required for each package")
		}
		if seen[entry.Name] {
			return invalid(field+".name", "the name "+entry.Name+" is listed twice")
		}
		seen[entry.Name] = true
		if state == "absent" {
			if entry.Version != "" && (!gemRemoval || !isVersionRule(entry.Version)) {
				if gemRemoval {
					return invalid(field+".version", "state: absent takes name only or an exact version; remove the newest version keyword")
				}
				return invalid(field+".version", "state: absent takes name only; remove version from the package entry")
			}
		} else if err := checkEntryVersion(field+".version", entry.Version, versionRule); err != nil {
			return err
		}
	}
	if account == "" {
		return invalid("run_as_user_account", "run_as_user_account is required: name the account that runs the installer, including root")
	}
	return nil
}

func installerPackageArguments(arguments *ArgumentList, packages []InstallerPackage, state string, nameRule, versionRule func(any) error) {
	for index := range packages {
		field := fmt.Sprintf("packages[%d]", index)
		arguments.Public(field+".name", &packages[index].Name, nameRule)
		if packages[index].Version != "" {
			arguments.Public(field+".version", &packages[index].Version, func(v any) error {
				if text, ok := v.(string); state != "absent" && ok && !isVersionRule(text) {
					return nil
				}
				return versionRule(v)
			})
		}
	}
}

// installerArguments adds the arguments every installer shares:
// run_as_user_account, its program path, and env.
func installerArguments(arguments *ArgumentList, account *string, programField string, programPath *string, env map[string]any) {
	if *account != "" {
		arguments.Public("run_as_user_account", account, accountNameRule)
	}
	if *programPath != "" {
		arguments.Public(programField, programPath, absolute)
	}
	for _, k := range slices.Sorted(maps.Keys(env)) {
		arguments.Private("env."+k, env[k], environmentValue)
	}
}

// installerNameArguments adds a list of names, each with rule.
func installerNameArguments(arguments *ArgumentList, field string, names []string, rule func(any) error) {
	for i := range names {
		arguments.Public(fmt.Sprintf("%s[%d]", field, i), &names[i], rule)
	}
}

// newestVersion stands for the version an installer chooses, which a
// dryrun cannot know before it installs.
const newestVersion = "the newest version"

// installNamed finishes a named-thing installer task: planned holds the
// differences the task would make, which a dryrun reports; apply makes
// them, and the result compares inspect's listing after it with before.
func installNamed(ctx context.Context, run Runtime, before map[string]string, planned []any, apply func() error, inspect func(context.Context) (map[string]string, error)) protocol.Result {
	if len(planned) == 0 || run.Dryrun {
		return installerResult(run, planned)
	}
	applied := apply()
	after, err := inspect(ctx)
	if err != nil {
		r := protocol.Failed(errors.Join(applied, err))
		r.Change = "unknown"
		return r
	}
	differences := installedDifferences(before, after)
	if applied != nil {
		return installerFailure(applied, differences)
	}
	return installerResult(run, differences)
}

// namedStateReader reads a named-thing installer's tracked input: the
// listing of the named things, "" when the installer is not there yet.
// settings hold the task's env and scope, so the installer lists what the
// task installed, such as the gems in GEM_HOME. A secret in env reaches
// only the installer's environment: the read marks the traces of the
// scope, as the task's own run does, and the listing goes into a record
// only as a hash.
func namedStateReader(ctx context.Context, settings installerSettings, inspect func(context.Context, *installer) (map[string]string, error)) (string, error) {
	i, stop := Runtime{}.newInstaller("", settings)
	if stop != nil {
		return "", nil
	}
	installed, err := inspect(ctx, i)
	if err != nil {
		return "", err
	}
	return installedListing(installed), nil
}
