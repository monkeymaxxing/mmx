package modules

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"

	"mmx/internal/protocol"
	"mmx/internal/value"
)

// NpmGlobalPackage installs command-line tools for the whole node with
// npm install --global.
type NpmGlobalPackage struct {
	Packages         []InstallerPackage `yaml:"packages,omitempty" json:"packages,omitempty"`
	State            string             `yaml:"state,omitempty" json:"state,omitempty"`
	NpmProgramPath   string             `yaml:"npm_program_path,omitempty" json:"npm_program_path,omitempty"`
	RunAsUserAccount string             `yaml:"run_as_user_account,omitempty" json:"run_as_user_account,omitempty"`
	Env              map[string]any     `yaml:"env,omitempty" json:"env,omitempty"`
}

var npmGlobalPackageDescription = Description{
	Name: "npm_global_package", Topic: TopicLanguageAndToolInstallers, Schema: reflect.TypeOf(NpmGlobalPackage{}),
	Summary: "command-line tools installed for the whole node with npm install --global; value lists each package that changed as {name, from, to}; an application's own packages are npm_project_dependencies",
	Fields: []Field{
		{Name: "packages", Required: true, Notes: "each entry requires name and version; version is an exact version; " + newestVersionWordsNote + "; with state: absent, name only"},
		{Name: "state", Values: presenceStates, Default: "present", Notes: "present follows each entry's version choice; absent uninstalls"},
		{Name: "npm_program_path", Default: "found on the standard path", Notes: "a written value must be absolute; the npm program to run when it is not npm on the standard path, such as one from Node.js unpacked under /opt; its directory comes first in PATH, so npm finds the node beside it"},
		{Name: "run_as_user_account", Required: true, Notes: "local account that runs npm, as shell's run_as_user_account; an ordinary account needs its own global directory, such as env NPM_CONFIG_PREFIX"},
		{Name: "env", Notes: "environment variables for npm, such as NPM_CONFIG_REGISTRY; values may be secrets"},
	},
	Example: "- npm_global_package:\n    packages: [{name: pm2, version: \"5.4.3\"}]\n    run_as_user_account: root\n",
	Results: []string{"status", "change", "value"},
	Dryrun:  "lists {name, from, to}; for newest_on_every_run it asks npm outdated --global",
}

var npmVersionPattern = regexp.MustCompile(`^v?[0-9]+\.[0-9]+\.[0-9]+([+-][0-9A-Za-z.+-]+)?$`)

var npmExactVersion = rule("version must be an exact npm version or a newest version keyword", func(text string) bool {
	return npmVersionPattern.MatchString(text)
})

// Prepare checks the inputs when the project loads.
func (n *NpmGlobalPackage) Prepare(base string) error {
	if err := checkInstallerPackages(n.Packages, n.State, n.RunAsUserAccount, false, "an exact version"); err != nil {
		return err
	}
	return validateEnvNames(n.Env)
}

// Arguments lists the inputs that may hold a reference, with their rules.
func (n *NpmGlobalPackage) Arguments(arguments *ArgumentList) {
	installerPackageArguments(arguments, n.Packages, n.State, npmPackageName("name"), npmExactVersion)
	installerArguments(arguments, &n.RunAsUserAccount, "npm_program_path", &n.NpmProgramPath, n.Env)
}

// npmPackageName is a package's name without a version: npm reads a name
// with @version after it as a version, which the version field says.
func npmPackageName(field string) func(any) error {
	return rule(field+" must be an npm package name, such as pm2 or @angular/cli, without a version; the version goes in version", func(s string) bool {
		return installerNames(field)(s) == nil && !strings.Contains(strings.TrimPrefix(s, "@"), "@")
	})
}

// CleanPaths cleans the resolved program path.
func (n *NpmGlobalPackage) CleanPaths() {
	if n.NpmProgramPath != "" {
		n.NpmProgramPath = filepath.Clean(n.NpmProgramPath)
	}
}

func (n *NpmGlobalPackage) settings(s value.Scope) installerSettings {
	return installerSettings{program: "npm", programPath: n.NpmProgramPath, programField: "npm_program_path",
		account: n.RunAsUserAccount, variables: n.Env, scope: s}
}

func (n *NpmGlobalPackage) names() []string { return installerPackageNames(n.Packages) }

// Run does the module's work on the node.
func (n *NpmGlobalPackage) Run(ctx context.Context, task NodeTask) protocol.Result {
	return task.Runtime.NpmGlobalPackage(ctx, *n, task.Scope)
}

// TrackedInputs lists the named packages, which count by their versions.
func (n *NpmGlobalPackage) TrackedInputs() []TrackedInput {
	return []TrackedInput{{Kind: "installed", Path: "npm_global_package " + strings.Join(n.names(), ", "), State: n}}
}

// ReadState lists the installed versions of the named packages, with the
// task's env.
func (n *NpmGlobalPackage) ReadState(ctx context.Context, s value.Scope) (string, error) {
	return namedStateReader(ctx, n.settings(s), func(ctx context.Context, i *installer) (map[string]string, error) {
		return npmGlobalInstalled(ctx, i, n.names())
	})
}

// StateAfter is what a dryrun predicts for the named packages.
func (n *NpmGlobalPackage) StateAfter(r protocol.Result) (string, bool) {
	return installerStateAfter(r)
}

// npmGlobalInstalled lists the installed versions of names, from
// npm ls --global --json. npm exits 1 for problems elsewhere in the tree,
// such as a missing peer dependency, and still prints the listing.
func npmGlobalInstalled(ctx context.Context, i *installer, names []string) (map[string]string, error) {
	out, err := i.read(ctx, "", i.program, "ls", "--global", "--json", "--depth=0")
	var listing struct {
		Dependencies map[string]struct {
			Version string `json:"version"`
		} `json:"dependencies"`
	}
	if jsonErr := json.Unmarshal([]byte(out), &listing); jsonErr != nil {
		if err == nil {
			err = fmt.Errorf("npm ls --global printed a listing MMX cannot read: %w", jsonErr)
		}
		return nil, err
	}
	installed := map[string]string{}
	for _, name := range names {
		if dependency, ok := listing.Dependencies[name]; ok {
			installed[name] = dependency.Version
		}
	}
	return installed, nil
}

// NpmGlobalPackage installs, upgrades, or uninstalls the named packages.
func (run Runtime) NpmGlobalPackage(ctx context.Context, n NpmGlobalPackage, s value.Scope) protocol.Result {
	i, stop := run.newInstaller("npm_global_package", n.settings(s))
	if stop != nil {
		return *stop
	}
	names := n.names()
	before, err := npmGlobalInstalled(ctx, i, names)
	if err != nil {
		return protocol.Failed(err)
	}
	outdated := map[string]struct {
		Latest string `json:"latest"`
	}{}
	for _, entry := range n.Packages {
		if entry.Version != newestOnEveryRun {
			continue
		}
		out, err := i.read(ctx, "", i.program, "outdated", "--global", "--json")
		switch printed := strings.TrimSpace(out); {
		case printed == "" && err != nil:
			return protocol.Failed(err)
		case printed != "":
			if jsonErr := json.Unmarshal([]byte(printed), &outdated); jsonErr != nil {
				return protocol.Failed(fmt.Errorf("npm outdated --global printed a listing MMX cannot read: %w", jsonErr))
			}
		}
		break
	}
	var planned []any
	var arguments []string
	command := "install"
	if n.State == "absent" {
		command = "uninstall"
	}
	for _, entry := range n.Packages {
		name, version := entry.Name, entry.Version
		current, installed := before[name]
		switch {
		case n.State == "absent":
			if installed {
				planned = append(planned, installedChange(name, current, nil))
				arguments = append(arguments, name)
			}
		case isVersionRule(version):
			// npm lists v5.4.3, which the input rule accepts, as 5.4.3.
			if current != strings.TrimPrefix(version, "v") {
				planned = append(planned, installedChange(name, nilWhenEmpty(current), version))
				arguments = append(arguments, name+"@"+version)
			}
		case !installed:
			planned = append(planned, installedChange(name, nil, newestVersion))
			if version == newestOnEveryRun {
				arguments = append(arguments, name+"@latest")
			} else {
				arguments = append(arguments, name)
			}
		case version == newestOnEveryRun && outdated[name].Latest != "" && outdated[name].Latest != current:
			planned = append(planned, installedChange(name, current, outdated[name].Latest))
			arguments = append(arguments, name+"@latest")
		}
	}

	return installNamed(ctx, run, before, planned, func() error {
		_, err := i.change(ctx, command, "", i.program, append([]string{command, "--global", "--no-fund", "--no-audit"}, arguments...)...)
		return err
	}, func(ctx context.Context) (map[string]string, error) {
		return npmGlobalInstalled(ctx, i, names)
	})
}
