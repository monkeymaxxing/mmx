package modules

import (
	"context"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"

	"mmx/internal/protocol"
	"mmx/internal/value"
)

// GemPackage installs Ruby gems with RubyGems' gem command.
type GemPackage struct {
	Packages         []InstallerPackage `yaml:"packages,omitempty" json:"packages,omitempty"`
	State            string             `yaml:"state,omitempty" json:"state,omitempty"`
	GemProgramPath   string             `yaml:"gem_program_path,omitempty" json:"gem_program_path,omitempty"`
	RunAsUserAccount string             `yaml:"run_as_user_account,omitempty" json:"run_as_user_account,omitempty"`
	Env              map[string]any     `yaml:"env,omitempty" json:"env,omitempty"`
}

var gemPackageDescription = Description{
	Name: "gem_package", Topic: TopicLanguageAndToolInstallers, Schema: reflect.TypeOf(GemPackage{}),
	Summary: "Ruby gems installed with gem; value lists each gem that changed as {name, from, to}",
	Fields: []Field{
		{Name: "packages", Required: true, Notes: "each entry requires name and version; version is an exact version; " + newestVersionWordsNote + "; with state: absent, name only; gem_package also accepts an exact version"},
		{Name: "state", Values: presenceStates, Default: "present", Notes: "present follows each entry's version choice; absent removes every version, or with an exact version only that one"},
		{Name: "gem_program_path", Default: "found on the standard path", Notes: "a written value must be absolute; the gem program to run when it is not gem on the standard path, such as one that rbenv provides"},
		{Name: "run_as_user_account", Required: true, Notes: "local account that runs gem, as shell's run_as_user_account; RubyGems installs into the account's own directory when it cannot write the system's"},
		{Name: "env", Notes: "environment variables for gem, such as GEM_HOME; values may be secrets"},
	},
	Example: "- gem_package:\n    packages: [{name: bundler, version: \"2.5.23\"}]\n    run_as_user_account: root\n",
	Results: []string{"status", "change", "value"},
	Dryrun:  "lists the gems it would install or remove; for newest_on_every_run it asks gem outdated",
}

var gemNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
var gemVersionPattern = regexp.MustCompile(`^[0-9][0-9A-Za-z._-]*$`)
var gemPackageName = rule("name must be a RubyGems package name without a version", func(text string) bool { return gemNamePattern.MatchString(text) })

var gemExactVersion = rule("version must be an exact RubyGems version", func(text string) bool {
	return gemVersionPattern.MatchString(text)
})

// Prepare checks the inputs when the project loads.
func (g *GemPackage) Prepare(base string) error {
	if err := checkInstallerPackages(g.Packages, g.State, g.RunAsUserAccount, true, "an exact version"); err != nil {
		return err
	}
	return validateEnvNames(g.Env)
}

// Arguments lists the inputs that may hold a reference, with their rules.
func (g *GemPackage) Arguments(arguments *ArgumentList) {
	installerPackageArguments(arguments, g.Packages, g.State, gemPackageName, gemExactVersion)
	installerArguments(arguments, &g.RunAsUserAccount, "gem_program_path", &g.GemProgramPath, g.Env)
}

// CleanPaths cleans the resolved program path.
func (g *GemPackage) CleanPaths() {
	if g.GemProgramPath != "" {
		g.GemProgramPath = filepath.Clean(g.GemProgramPath)
	}
}

func (g *GemPackage) settings(s value.Scope) installerSettings {
	return installerSettings{program: "gem", programPath: g.GemProgramPath, programField: "gem_program_path",
		account: g.RunAsUserAccount, variables: g.Env, scope: s}
}

func (g *GemPackage) names() []string { return installerPackageNames(g.Packages) }

// Run does the module's work on the node.
func (g *GemPackage) Run(ctx context.Context, task NodeTask) protocol.Result {
	return task.Runtime.GemPackage(ctx, *g, task.Scope)
}

// TrackedInputs lists the named gems, which count by their versions.
func (g *GemPackage) TrackedInputs() []TrackedInput {
	return []TrackedInput{{Kind: "installed", Path: "gem_package " + strings.Join(g.names(), ", "), State: g}}
}

// ReadState lists the installed versions of the named gems, with the
// task's env.
func (g *GemPackage) ReadState(ctx context.Context, s value.Scope) (string, error) {
	return namedStateReader(ctx, g.settings(s), func(ctx context.Context, i *installer) (map[string]string, error) {
		return gemsInstalled(ctx, i, g.names())
	})
}

// StateAfter is what a dryrun predicts for the named gems.
func (g *GemPackage) StateAfter(r protocol.Result) (string, bool) { return installerStateAfter(r) }

// gemListLine is one gem in gem list: its name and its versions, such as
// "rake (13.2.1, default: 13.0.6)" or "nokogiri (1.16.0 x86_64-linux)".
var gemListLine = regexp.MustCompile(`^(\S+) \((.*)\)$`)

// gemsInstalled lists the installed versions of names, each name's
// versions sorted and joined by ", ".
func gemsInstalled(ctx context.Context, i *installer, names []string) (map[string]string, error) {
	out, err := i.read(ctx, "", i.program, append([]string{"list", "--local", "--exact"}, names...)...)
	if err != nil {
		return nil, err
	}
	installed := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		m := gemListLine.FindStringSubmatch(strings.TrimSpace(line))
		if m == nil || !slices.Contains(names, m[1]) {
			continue
		}
		var versions []string
		for _, v := range strings.Split(m[2], ", ") {
			if fields := strings.Fields(strings.TrimPrefix(v, "default: ")); len(fields) > 0 {
				versions = append(versions, fields[0])
			}
		}
		slices.Sort(versions)
		installed[m[1]] = strings.Join(versions, ", ")
	}
	return installed, nil
}

// gemOutdatedLine is one gem in gem outdated: "rake (13.0.6 < 13.2.1)".
var gemOutdatedLine = regexp.MustCompile(`^(\S+) \((\S+) < (\S+)\)$`)

// GemPackage installs, upgrades, or removes the named gems.
func (run Runtime) GemPackage(ctx context.Context, g GemPackage, s value.Scope) protocol.Result {
	i, stop := run.newInstaller("gem_package", g.settings(s))
	if stop != nil {
		return *stop
	}
	names := g.names()
	before, err := gemsInstalled(ctx, i, names)
	if err != nil {
		return protocol.Failed(err)
	}
	has := func(name, version string) bool {
		return slices.Contains(strings.Split(before[name], ", "), version)
	}
	newest := map[string]string{}
	for _, entry := range g.Packages {
		if entry.Version != newestOnEveryRun {
			continue
		}
		out, err := i.read(ctx, "", i.program, "outdated") //nolint:nilaway // newInstaller returns a nil installer only with a non-nil stop, which returned above.
		if err != nil {
			return protocol.Failed(err)
		}
		for _, line := range strings.Split(out, "\n") {
			if match := gemOutdatedLine.FindStringSubmatch(strings.TrimSpace(line)); match != nil {
				newest[match[1]] = match[3]
			}
		}
		break
	}
	var planned []any
	var install []string
	var removals [][]string
	for _, entry := range g.Packages {
		name, version := entry.Name, entry.Version
		current, installed := before[name]
		if g.State == "absent" {
			if !installed || (version != "" && !has(name, version)) {
				continue
			}
			if version != "" {
				rest := slices.DeleteFunc(strings.Split(current, ", "), func(v string) bool { return v == version })
				planned = append(planned, installedChange(name, current, nilWhenEmpty(strings.Join(rest, ", "))))
				removals = append(removals, []string{"uninstall", name, "--version", version, "--executables"})
			} else {
				planned = append(planned, installedChange(name, current, nil))
				removals = append(removals, []string{"uninstall", name, "--all", "--executables"})
			}
			continue
		}
		switch {
		case isVersionRule(version):
			if has(name, version) {
				continue
			}
			planned = append(planned, installedChange(name, nilWhenEmpty(current), strings.Trim(current+", "+version, ", ")))
			install = append(install, name+":"+version)
		case !installed:
			planned = append(planned, installedChange(name, nil, newestVersion))
			install = append(install, name)
		case version == newestOnEveryRun && newest[name] != "":
			planned = append(planned, installedChange(name, current, newest[name]))
			install = append(install, name)
		}
	}
	apply := func() error {
		if g.State == "absent" {
			for _, args := range removals {
				if _, err := i.change(ctx, "uninstall", "", i.program, args...); err != nil {
					return err
				}
			}
			return nil
		}
		_, err := i.change(ctx, "install", "", i.program, append([]string{"install", "--no-document"}, install...)...)
		return err
	}

	return installNamed(ctx, run, before, planned, apply, func(ctx context.Context) (map[string]string, error) {
		return gemsInstalled(ctx, i, names)
	})
}

// nilWhenEmpty is text, or null for none.
func nilWhenEmpty(text string) any {
	if text == "" {
		return nil
	}
	return text
}
