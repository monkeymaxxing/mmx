package modules

import (
	"context"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"

	"mmx/internal/protocol"
	"mmx/internal/value"
)

// BundlerProjectDependencies installs what an application's Gemfile.lock
// lists, with Bundler in its deployment mode.
type BundlerProjectDependencies struct {
	LockFileDir          string         `yaml:"lock_file_dir" json:"lock_file_dir"`
	RunAsUserAccount     string         `yaml:"run_as_user_account" json:"run_as_user_account"`
	WithoutGemfileGroups []string       `yaml:"without_gemfile_groups" json:"without_gemfile_groups"`
	GemsDir              string         `yaml:"gems_dir,omitempty" json:"gems_dir,omitempty"`
	BundleProgramPath    string         `yaml:"bundle_program_path,omitempty" json:"bundle_program_path,omitempty"`
	Env                  map[string]any `yaml:"env,omitempty" json:"env,omitempty"`
}

// defaultGemsDirectory is where Bundler's deployment mode installs gems,
// inside the lock file directory.
const defaultGemsDirectory = "vendor/bundle"

var bundlerProjectDependenciesDescription = Description{
	Name: "bundler_project_dependencies", Topic: TopicLanguageAndToolInstallers, Schema: reflect.TypeOf(BundlerProjectDependencies{}),
	Summary: "what an application's Gemfile.lock lists, installed with bundle install in Bundler's deployment mode, which fails when Gemfile.lock does not fit the Gemfile; a record .mmx-bundler-project-dependencies in the directory makes a rerun unchanged; value.bundler_settings lists the settings the application must run with",
	Fields: []Field{
		{Name: "lock_file_dir", Required: true, Notes: "the directory on the node that holds the Gemfile and Gemfile.lock, usually a release directory or a checkout"},
		{Name: "run_as_user_account", Required: true, Notes: "the application's account, which runs Bundler: gems can run code when they are installed"},
		{Name: "without_gemfile_groups", Required: true, Notes: "the Gemfile groups to leave out, such as [development, test]; [] installs every group"},
		{Name: "gems_dir", Default: defaultGemsDirectory, Notes: "where the gems are installed, relative to lock_file_dir or absolute; one outside the lock file directory lets releases share their gems, at the price that installing for the next release changes a directory the running release reads"},
		{Name: "bundle_program_path", Default: "found on the standard path", Notes: "a written value must be absolute; the bundle program to run when it is not bundle on the standard path, such as one that rbenv provides; its directory comes first in PATH"},
		{Name: "env", Notes: "environment variables for Bundler, such as a token for a private gem server; values may be secrets"},
	},
	Example: "- bundler_project_dependencies:\n    lock_file_dir: /srv/myapp/releases/v1.4.2\n    run_as_user_account: myapp\n    without_gemfile_groups: [development, test]\n",
	Results: []string{"status", "change", "value"},
	Dryrun:  "compares the record and reports unchanged or would change with the reason, and the gems bundle check reports missing; a lock file directory that does not exist yet, or a lock file or manifest that an earlier node task would change, is not checked",
}

// Prepare checks the inputs when the project loads.
func (b *BundlerProjectDependencies) Prepare(base string) error {
	if err := checkProjectDependencies(b.LockFileDir, b.RunAsUserAccount, b.Env); err != nil {
		return err
	}
	if b.WithoutGemfileGroups == nil {
		return invalid("without_gemfile_groups", "without_gemfile_groups is required: the Gemfile groups to leave out, such as [development, test], or [] for every group")
	}
	return nil
}

// Arguments lists the inputs that may hold a reference, with their rules.
func (b *BundlerProjectDependencies) Arguments(arguments *ArgumentList) {
	projectDependencyArguments(arguments, &b.LockFileDir, &b.RunAsUserAccount, "bundle_program_path", &b.BundleProgramPath, b.Env)
	installerNameArguments(arguments, "without_gemfile_groups", b.WithoutGemfileGroups, installerNames("without_gemfile_groups"))
	if b.GemsDir != "" {
		arguments.Public("gems_dir", &b.GemsDir, rule("gems_dir must be a path without null bytes", func(s string) bool {
			return s != "" && !strings.ContainsRune(s, 0)
		}))
	}
}

// CleanPaths cleans the resolved paths.
func (b *BundlerProjectDependencies) CleanPaths() {
	b.LockFileDir = filepath.Clean(b.LockFileDir)
	for _, path := range []*string{&b.BundleProgramPath, &b.GemsDir} {
		if *path != "" {
			*path = filepath.Clean(*path)
		}
	}
}

// bundlerSettings are the environment variables through which the task's
// settings reach Bundler, so MMX writes no Bundler configuration file.
func (b *BundlerProjectDependencies) bundlerSettings() map[string]string {
	gems := b.GemsDir
	if gems == "" {
		gems = defaultGemsDirectory
	}
	return map[string]string{"BUNDLE_DEPLOYMENT": "true", "BUNDLE_PATH": gems, "BUNDLE_WITHOUT": strings.Join(b.WithoutGemfileGroups, ":")}
}

func (b *BundlerProjectDependencies) task(s value.Scope) projectDependencies {
	settings := b.bundlerSettings()
	listed := map[string]any{}
	for k, v := range settings {
		listed[k] = v
	}
	return projectDependencies{
		module: "bundler_project_dependencies", directory: b.LockFileDir,
		settings: installerSettings{program: "bundle", programPath: b.BundleProgramPath, programField: "bundle_program_path",
			providedBy: "Bundler (gem_package with a bundler packages entry, or the package ruby-bundler or rubygem-bundler)", account: b.RunAsUserAccount, variables: b.Env, scope: s},
		manifest: "Gemfile", lockFiles: []string{"Gemfile.lock"}, installedPaths: []string{settings["BUNDLE_PATH"]},
		chosen: listed, runtime: "ruby", env: settings,
		install: func(string) []string { return []string{"install"} },
		value:   map[string]any{"bundler_settings": listed},
		preview: func(ctx context.Context, i *installer, v map[string]any) error {
			// bundle check exits 1 for missing gems. Keep its exit status so
			// other failures cannot masquerade as a successful dependency check.
			r := runProcess(ctx, i.env, b.LockFileDir, nil, i.account, i.program, "check").Result
			report := r.Stdout + "\n" + r.Stderr
			missing := []any{}
			for _, line := range strings.Split(report, "\n") {
				if gem, ok := strings.CutPrefix(strings.TrimSpace(line), "* "); ok {
					missing = append(missing, gem)
				}
			}
			reportsMissingGems := r.ExitCode == 1 && (len(missing) > 0 || strings.Contains(report, "The following gems are missing"))
			if r.Status != "succeeded" && !reportsMissingGems {
				return fmt.Errorf("The bundle check command failed in %s with exit status %d; fix the reported problem and run the dryrun again: %w", b.LockFileDir, r.ExitCode, installerCommandFailure(i.program, []string{"check"}, r))
			}
			v["missing_gems"] = missing
			return nil
		},
	}
}

// Run does the module's work on the node.
func (b *BundlerProjectDependencies) Run(ctx context.Context, task NodeTask) protocol.Result {
	return task.Runtime.installProjectDependencies(ctx, b.task(task.Scope))
}

// TrackedInputs lists the installation, which counts by its record.
func (b *BundlerProjectDependencies) TrackedInputs() []TrackedInput {
	return []TrackedInput{{Kind: "installed", Path: "bundler_project_dependencies in " + b.LockFileDir, State: b}}
}

// ReadState is the record of the last installation.
func (b *BundlerProjectDependencies) ReadState(ctx context.Context, _ value.Scope) (string, error) {
	return dependencyStateReader(b.task(nil))
}

// StateAfter is what a dryrun predicts for the installation.
func (b *BundlerProjectDependencies) StateAfter(r protocol.Result) (string, bool) {
	return installerStateAfter(r)
}
