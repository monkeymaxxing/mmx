package modules

import (
	"context"
	"path/filepath"
	"reflect"
	"strings"

	"mmx/internal/protocol"
	"mmx/internal/value"
)

// ComposerProjectDependencies installs what an application's composer.lock
// lists, with composer install.
type ComposerProjectDependencies struct {
	LockFileDir                    string         `yaml:"lock_file_dir" json:"lock_file_dir"`
	RunAsUserAccount               string         `yaml:"run_as_user_account" json:"run_as_user_account"`
	IncludeDevelopmentDependencies *bool          `yaml:"include_development_dependencies" json:"include_development_dependencies"`
	ComposerProgramPath            string         `yaml:"composer_program_path,omitempty" json:"composer_program_path,omitempty"`
	Env                            map[string]any `yaml:"env,omitempty" json:"env,omitempty"`
}

var composerProjectDependenciesDescription = Description{
	Name: "composer_project_dependencies", Topic: TopicLanguageAndToolInstallers, Schema: reflect.TypeOf(ComposerProjectDependencies{}),
	Summary: "what an application's composer.lock lists, installed into its vendor directory with composer install, without prompts; a record .mmx-composer-project-dependencies in the directory makes a rerun unchanged",
	Fields: []Field{
		{Name: "lock_file_dir", Required: true, Notes: "the directory on the node that holds composer.json and composer.lock, usually a release directory or a checkout"},
		{Name: "run_as_user_account", Required: true, Notes: "the application's account, which runs Composer: Composer runs the packages' scripts"},
		{Name: "include_development_dependencies", Required: true, Notes: "false leaves out what composer.json lists under require-dev; true installs it"},
		{Name: "composer_program_path", Default: "found on the standard path", Notes: "a written value must be absolute; the composer program to run when it is not composer on the standard path; its directory comes first in PATH"},
		{Name: "env", Notes: "environment variables for Composer, such as COMPOSER_AUTH for a private repository; values may be secrets"},
	},
	Example: "- composer_project_dependencies:\n    lock_file_dir: /srv/myapp/releases/v1.4.2\n    run_as_user_account: myapp\n    include_development_dependencies: false\n",
	Results: []string{"status", "change", "value"},
	Dryrun:  "compares the record and reports unchanged or would change with the reason, and the operations composer install --dry-run lists in value.operations; a lock file directory that does not exist yet, or a lock file or manifest that an earlier node task would change, is not checked",
}

// Prepare checks the inputs when the project loads.
func (c *ComposerProjectDependencies) Prepare(base string) error {
	if err := checkProjectDependencies(c.LockFileDir, c.RunAsUserAccount, c.Env); err != nil {
		return err
	}
	return requiredBool("include_development_dependencies", c.IncludeDevelopmentDependencies)
}

// Arguments lists the inputs that may hold a reference, with their rules.
func (c *ComposerProjectDependencies) Arguments(arguments *ArgumentList) {
	projectDependencyArguments(arguments, &c.LockFileDir, &c.RunAsUserAccount, "composer_program_path", &c.ComposerProgramPath, c.Env)
}

// CleanPaths cleans the resolved paths.
func (c *ComposerProjectDependencies) CleanPaths() {
	c.LockFileDir = filepath.Clean(c.LockFileDir)
	if c.ComposerProgramPath != "" {
		c.ComposerProgramPath = filepath.Clean(c.ComposerProgramPath)
	}
}

func (c *ComposerProjectDependencies) task(s value.Scope) projectDependencies {
	development := c.IncludeDevelopmentDependencies != nil && *c.IncludeDevelopmentDependencies
	install := []string{"install", "--no-interaction", "--no-progress"}
	if !development {
		install = append(install, "--no-dev")
	}
	return projectDependencies{
		module: "composer_project_dependencies", directory: c.LockFileDir,
		settings: installerSettings{program: "composer", programPath: c.ComposerProgramPath, programField: "composer_program_path",
			providedBy: "PHP and Composer (the package composer, or composer.phar from getcomposer.org with download)", account: c.RunAsUserAccount, variables: c.Env, scope: s},
		manifest: "composer.json", lockFiles: []string{"composer.lock"}, installedPaths: []string{"vendor"},
		chosen:  map[string]any{"include_development_dependencies": development},
		runtime: "php",
		install: func(string) []string { return install },
		preview: func(ctx context.Context, i *installer, v map[string]any) error {
			// composer lists the operations on standard error.
			out, err := i.readOutput(ctx, c.LockFileDir, i.program, append(install, "--dry-run")...)
			if err != nil {
				return err
			}
			operations := []any{}
			for _, line := range strings.Split(out, "\n") {
				if operation, ok := strings.CutPrefix(strings.TrimSpace(line), "- "); ok {
					operations = append(operations, operation)
				}
			}
			v["operations"] = operations
			return nil
		},
	}
}

// Run does the module's work on the node.
func (c *ComposerProjectDependencies) Run(ctx context.Context, task NodeTask) protocol.Result {
	return task.Runtime.installProjectDependencies(ctx, c.task(task.Scope))
}

// TrackedInputs lists the installation, which counts by its record.
func (c *ComposerProjectDependencies) TrackedInputs() []TrackedInput {
	return []TrackedInput{{Kind: "installed", Path: "composer_project_dependencies in " + c.LockFileDir, State: c}}
}

// ReadState is the record of the last installation.
func (c *ComposerProjectDependencies) ReadState(ctx context.Context, _ value.Scope) (string, error) {
	return dependencyStateReader(c.task(nil))
}

// StateAfter is what a dryrun predicts for the installation.
func (c *ComposerProjectDependencies) StateAfter(r protocol.Result) (string, bool) {
	return installerStateAfter(r)
}
