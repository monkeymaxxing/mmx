package modules

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"

	"mmx/internal/protocol"
	"mmx/internal/value"
)

// PipPackage installs Python packages with pip, into a virtual environment
// or the system's Python.
type PipPackage struct {
	Packages               []InstallerPackage `yaml:"packages,omitempty" json:"packages,omitempty"`
	RequirementsFileOnNode string             `yaml:"requirements_file_on_node,omitempty" json:"requirements_file_on_node,omitempty"`
	State                  string             `yaml:"state,omitempty" json:"state,omitempty"`
	VirtualEnvironment     string             `yaml:"virtual_environment,omitempty" json:"virtual_environment,omitempty"`
	InstallSystemWide      bool               `yaml:"install_system_wide,omitempty" json:"install_system_wide,omitempty"`
	PythonProgramPath      string             `yaml:"python_program_path,omitempty" json:"python_program_path,omitempty"`
	RunAsUserAccount       string             `yaml:"run_as_user_account,omitempty" json:"run_as_user_account,omitempty"`
	Env                    map[string]any     `yaml:"env,omitempty" json:"env,omitempty"`
}

var pipPackageDescription = Description{
	Name: "pip_package", Topic: TopicLanguageAndToolInstallers, Schema: reflect.TypeOf(PipPackage{}),
	Summary: "Python packages installed with pip into a virtual environment or the system's Python; value lists each package that changed as {name, from, to}",
	Fields: []Field{
		{Name: "packages", Notes: "one of packages and requirements_file_on_node; each entry requires name and version; version is a pip version specifier, such as ==23.0.0 or >=2.32,<3; environment markers (;) are refused; use the task's when for a condition; " + newestVersionWordsNote + "; with state: absent, name only"},
		{Name: "requirements_file_on_node", Notes: "absolute path on the node of a pip requirements file, usually in the application's checkout"},
		{Name: "state", Values: presenceStates, Default: "present", Notes: "only with packages; present follows each entry's version choice; absent uninstalls the named packages"},
		{Name: "virtual_environment", Notes: "absolute path of the virtual environment, created with python -m venv as the task's account when the directory does not exist; on Debian and Ubuntu that needs the package python3-venv; one of virtual_environment and install_system_wide"},
		{Name: "install_system_wide", Default: "false", Notes: "true installs into the node's system Python. Debian 12, Ubuntu 24.04, and newer refuse system-wide pip installation unless env sets PIP_BREAK_SYSTEM_PACKAGES: \"1\", because pip can replace packages managed by the distribution."},
		{Name: "python_program_path", Default: "found on the standard path", Notes: "a written value must be absolute; the Python that creates the virtual environment or receives a system-wide installation, when it is not python3 on the standard path"},
		{Name: "run_as_user_account", Required: true, Notes: "local account that runs Python and pip, as shell's run_as_user_account; it owns the virtual environment"},
		{Name: "env", Notes: "environment variables for pip, such as PIP_INDEX_URL; values may be secrets"},
	},
	Example: "- pip_package:\n    packages: [{name: gunicorn, version: \"==23.0.0\"}, {name: requests, version: newest_when_first_installed}]\n    virtual_environment: /srv/myapp/venv\n    run_as_user_account: myapp\n",
	Results: []string{"status", "change", "value"},
	Dryrun:  "asks pip what it would install (pip install --dry-run, pip 22.2 or newer) and lists it; a virtual environment that does not exist yet reports would change, since the run creates it; with an older pip, or a requirements file that does not exist yet or that an earlier node task would change, the task is not checked",
}

// pipRequirement is one name or requirement: one line that pip cannot read
// as an option.
var pipRequirement = rule("a pip requirement must be one line that does not start with -", func(s string) bool {
	return s != "" && !strings.HasPrefix(s, "-") && !strings.ContainsAny(s, "\r\n\x00")
})

var pipNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

var pipPackageName = rule("name must be a Python package name without a version", func(text string) bool {
	return pipNamePattern.MatchString(text)
})

func pipVersionRequirement(v any) error {
	if text, ok := v.(string); ok && strings.Contains(text, ";") {
		return fmt.Errorf(`version takes a version specifier such as "==23.0.0" or ">=2.32,<3". Use the task's when to set a condition`)
	}
	return rule("version must be a pip version specifier such as ==23.0.0 or >=2.32,<3, or a newest version keyword", func(text string) bool {
		return text != "" && strings.ContainsAny(text[:1], "<>=!~") && pipRequirement(text) == nil
	})(v)
}

// Prepare checks the inputs when the project loads.
func (p *PipPackage) Prepare(base string) error {
	switch {
	case (len(p.Packages) > 0) == (p.RequirementsFileOnNode != ""):
		return invalid("packages", "pip_package needs exactly one of packages and requirements_file_on_node")
	case (p.VirtualEnvironment == "") == !p.InstallSystemWide:
		return invalid("virtual_environment", "pip_package needs exactly one of virtual_environment and install_system_wide: true; there is no default place")
	case p.State != "" && len(p.Packages) == 0:
		return invalid("state", "state goes with packages")
	}
	if len(p.Packages) > 0 {
		if err := checkInstallerPackages(p.Packages, p.State, p.RunAsUserAccount, false, "a pip version specifier, such as ==23.0.0 or >=2.32,<3"); err != nil {
			return err
		}
	} else if p.RunAsUserAccount == "" {
		return invalid("run_as_user_account", "run_as_user_account is required: name the account that runs pip, including root")
	}

	return validateEnvNames(p.Env)
}

// Arguments lists the inputs that may hold a reference, with their rules.
func (p *PipPackage) Arguments(arguments *ArgumentList) {
	installerPackageArguments(arguments, p.Packages, p.State, pipPackageName, pipVersionRequirement)
	if p.RequirementsFileOnNode != "" {
		arguments.Public("requirements_file_on_node", &p.RequirementsFileOnNode, absolute)
	}
	if p.VirtualEnvironment != "" {
		arguments.Public("virtual_environment", &p.VirtualEnvironment, absolute)
	}
	installerArguments(arguments, &p.RunAsUserAccount, "python_program_path", &p.PythonProgramPath, p.Env)
}

// CleanPaths cleans the resolved paths.
func (p *PipPackage) CleanPaths() {
	for _, path := range []*string{&p.RequirementsFileOnNode, &p.VirtualEnvironment, &p.PythonProgramPath} {
		if *path != "" {
			*path = filepath.Clean(*path)
		}
	}
}

// Run does the module's work on the node.
func (p *PipPackage) Run(ctx context.Context, task NodeTask) protocol.Result {
	return task.Runtime.PipPackage(ctx, *p, task.Scope)
}

// TrackedInputs lists the environment, which counts by its installed
// packages and versions.
func (p *PipPackage) TrackedInputs() []TrackedInput {
	where := "the system's Python"
	if p.VirtualEnvironment != "" {
		where = p.VirtualEnvironment
	}
	return []TrackedInput{{Kind: "installed", Path: "pip_package in " + where, State: p}}
}

// ReadState lists the packages and versions installed in the environment
// now, "" when it does not exist yet, with the task's env, as
// namedStateReader reads.
func (p *PipPackage) ReadState(ctx context.Context, s value.Scope) (string, error) {
	pip, stop := Runtime{}.pip(*p, s, true)
	//nolint:nilaway // Every nil pip return carries a non-nil stop; || short-circuits before accessing pip.exists.
	if stop != nil || !pip.exists {
		return "", nil
	}
	installed, err := pip.freeze(ctx)
	if err != nil {
		return "", err
	}
	return installedListing(installed), nil
}

// StateAfter is what a dryrun predicts for the environment.
func (p *PipPackage) StateAfter(r protocol.Result) (string, bool) { return installerStateAfter(r) }

// pipEnvironment is the Python a task installs into, and the installer
// that runs it.
type pipEnvironment struct {
	*installer
	// python runs pip: the virtual environment's own, or the system's.
	python string
	// exists: the virtual environment exists, or the installation is
	// system-wide.
	exists bool
}

// pip prepares the task's Python. Reading needs no python_program_path once
// the virtual environment exists.
func (run Runtime) pip(p PipPackage, s value.Scope, reading bool) (*pipEnvironment, *protocol.Result) {
	settings := installerSettings{program: "python3", programPath: p.PythonProgramPath, programField: "python_program_path",
		account: p.RunAsUserAccount, variables: p.Env, scope: s}
	if p.VirtualEnvironment != "" {
		venvPython := filepath.Join(p.VirtualEnvironment, "bin", "python")
		if executableFile(venvPython) {
			settings.programPath = venvPython
			i, stop := run.newInstaller("pip_package", settings)
			if stop != nil {
				return nil, stop
			}
			return &pipEnvironment{installer: i, python: venvPython, exists: true}, nil
		}
		if _, err := os.Lstat(p.VirtualEnvironment); err == nil {
			r := protocol.Failed(fmt.Errorf("%s exists and is not a Python virtual environment: it has no bin/python", p.VirtualEnvironment))
			return nil, &r
		}
		if reading {
			return &pipEnvironment{}, nil
		}
	}
	i, stop := run.newInstaller("pip_package", settings)
	if stop != nil {
		return nil, stop
	}
	return &pipEnvironment{installer: i, python: i.program, exists: p.InstallSystemWide}, nil
}

// pipArguments are the arguments of python -m pip that every call shares.
func (e *pipEnvironment) pipArguments(args ...string) []string {
	return append([]string{"-m", "pip", "--disable-pip-version-check", "--no-input"}, args...)
}

// freeze lists the installed packages by their normalized name, with the
// version or, for a package installed from elsewhere, what pip prints.
func (e *pipEnvironment) freeze(ctx context.Context) (map[string]string, error) {
	out, err := e.read(ctx, "", e.python, e.pipArguments("freeze", "--all")...)
	if err != nil {
		return nil, err
	}
	installed := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if name, version, ok := strings.Cut(line, "=="); ok {
			installed[pipName(name)] = version
		} else if name, source, ok := strings.Cut(line, " @ "); ok {
			installed[pipName(name)] = "@ " + source
		} else {
			installed[line] = ""
		}
	}
	return installed, nil
}

// pipName is a package name as Python's packaging rules compare it:
// lowercase, with each run of -, _, and . as one -.
func pipName(name string) string {
	return strings.ToLower(pipSeparators.ReplaceAllString(strings.TrimSpace(name), "-"))
}

var pipSeparators = regexp.MustCompile(`[-_.]+`)

// pipRequirementSatisfied uses the version specifier parser shipped with pip, not
// MMX's own version ordering, to preserve already-satisfied version rules.
const pipRequirementSatisfied = `import sys
from pip._vendor.packaging.specifiers import SpecifierSet
print("satisfied" if SpecifierSet(sys.argv[1]).contains(sys.argv[2], prereleases=True) else "unsatisfied")
`

// PipPackage installs, upgrades, or uninstalls, and reports the difference
// pip freeze shows.
func (run Runtime) PipPackage(ctx context.Context, p PipPackage, s value.Scope) protocol.Result {
	env, stop := run.pip(p, s, false)
	if stop != nil {
		return *stop
	}
	if _, set := env.env["PIP_DISABLE_PIP_VERSION_CHECK"]; !set {
		env.env["PIP_DISABLE_PIP_VERSION_CHECK"] = "1"
	}
	if p.RequirementsFileOnNode != "" {
		if _, err := os.Stat(p.RequirementsFileOnNode); err != nil {
			return run.notYet(fmt.Errorf("the requirements file %s does not exist; an earlier task, such as a git checkout, may create it", p.RequirementsFileOnNode))
		}
		if run.EarlierNodeTaskWouldChange(p.RequirementsFileOnNode) {
			return run.notYet(fmt.Errorf("an earlier node task would change the requirements file %s, so a dryrun cannot tell what pip would install", p.RequirementsFileOnNode))
		}
	}
	absent := p.State == "absent"
	if !env.exists {
		if absent {
			return installerResult(run, nil)
		}
		if run.Dryrun {
			r := protocol.Success("would_change")
			r.Reason = "would create the virtual environment " + p.VirtualEnvironment + " and install into it"
			return r
		}
		if _, err := env.change(ctx, "virtual environment "+p.VirtualEnvironment, "", env.python, "-m", "venv", p.VirtualEnvironment); err != nil {
			return protocol.Failed(err)
		}
		created, stop := run.pip(p, s, false)
		if stop != nil {
			r := *stop
			r.Change = "changed"
			return r
		}
		created.env = env.env
		env = created
	}
	before, err := env.freeze(ctx)
	if err != nil {
		return protocol.Failed(err)
	}
	if absent {
		var remove []string
		for _, entry := range p.Packages {
			name := entry.Name
			if _, installed := before[pipName(name)]; installed {
				remove = append(remove, pipName(name))
			}
		}
		if len(remove) == 0 || run.Dryrun {
			after := maps.Clone(before)
			for _, name := range remove {
				delete(after, name)
			}
			return installerResult(run, installedDifferences(before, after))
		}
		_, err := env.change(ctx, "uninstall", "", env.python, env.pipArguments(append([]string{"uninstall", "--yes"}, remove...)...)...)
		return env.finish(ctx, before, err)
	}
	install := []string{"install"}
	if p.RequirementsFileOnNode != "" {
		install = append(install, "--requirement", p.RequirementsFileOnNode)
	} else {
		upgrade := false
		for _, entry := range p.Packages {
			upgrade = upgrade || entry.Version == newestOnEveryRun
		}
		if upgrade {
			install = append(install, "--upgrade")
		}
		for _, entry := range p.Packages {
			version, installed := before[pipName(entry.Name)]
			if installed && strings.HasPrefix(version, "@ ") && entry.Version != newestOnEveryRun {
				// freeze records a direct URL instead of the distribution's version.
				out, err := env.read(ctx, "", env.python, "-c", "import sys; from importlib.metadata import version; print(version(sys.argv[1]))", entry.Name)
				if err != nil {
					return protocol.Failed(err)
				}
				version = strings.TrimSpace(out)
			}
			switch {
			case isVersionRule(entry.Version):
				requirement := entry.Name + entry.Version
				if upgrade && installed {
					// --upgrade applies to every direct requirement. Ask pip's own
					// specifier parser whether this entry can keep its installed
					// version, so another entry's upgrade cannot upgrade it too.
					satisfied, err := env.read(ctx, "", env.python, "-c", pipRequirementSatisfied, entry.Version, version)
					if err != nil {
						return protocol.Failed(err)
					}
					if strings.TrimSpace(satisfied) == "satisfied" {
						requirement = entry.Name + "==" + version
					}
				}
				install = append(install, requirement)
			case entry.Version == newestWhenFirstInstalled && installed:
				// Keep this package in the transaction so dependency resolution cannot
				// upgrade it when a neighbouring entry requests the newest version.
				install = append(install, entry.Name+"=="+version)
			default:
				install = append(install, entry.Name)
			}
		}
	}

	if run.Dryrun {
		return env.preview(ctx, before, install)
	}
	_, err = env.change(ctx, "install", "", env.python, env.pipArguments(install...)...)
	return env.finish(ctx, before, err)
}

// finish compares the environment after pip ran with before.
func (e *pipEnvironment) finish(ctx context.Context, before map[string]string, ran error) protocol.Result {
	after, err := e.freeze(ctx)
	if err != nil {
		return protocol.Failed(errors.Join(ran, err))
	}
	differences := installedDifferences(before, after)
	if ran != nil {
		return installerFailure(ran, differences)
	}
	return installerResult(e.run, differences)
}

// pipDryrunVersion is the first pip that can say what it would install.
var pipDryrunVersion = []int{22, 2}

// preview asks pip what it would install.
func (e *pipEnvironment) preview(ctx context.Context, before map[string]string, install []string) protocol.Result {
	out, err := e.read(ctx, "", e.python, e.pipArguments("--version")...)
	if err != nil {
		return protocol.Failed(err)
	}
	version := strings.Fields(out)
	if len(version) < 2 || !versionAtLeast(version[1], pipDryrunVersion) {
		return protocol.Result{Status: "not_checked", Change: "unknown",
			Reason: fmt.Sprintf("pip %s cannot say what it would install; that needs pip 22.2 or newer", strings.Join(version[1:2], ""))}
	}
	out, err = e.read(ctx, "", e.python, e.pipArguments(append(install, "--dry-run", "--quiet", "--report", "-")...)...)
	if err != nil {
		return protocol.Failed(err)
	}
	var report struct {
		Install []struct {
			Metadata struct {
				Name    string `json:"name"`
				Version string `json:"version"`
			} `json:"metadata"`
		} `json:"install"`
	}
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		return protocol.Failed(fmt.Errorf("pip printed a report MMX cannot read: %w", err))
	}
	after := maps.Clone(before)
	for _, item := range report.Install {
		after[pipName(item.Metadata.Name)] = item.Metadata.Version
	}
	return installerResult(e.run, installedDifferences(before, after))
}

// versionAtLeast compares a dotted version's leading numbers with want.
func versionAtLeast(version string, want []int) bool {
	parts := strings.Split(version, ".")
	for i, w := range want {
		if i >= len(parts) {
			return false
		}
		n, err := strconv.Atoi(parts[i])
		if err != nil {
			return false
		}
		if n != w {
			return n > w
		}
	}
	return true
}
