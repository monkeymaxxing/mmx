package modules

import (
	"context"
	"path/filepath"
	"reflect"

	"mmx/internal/protocol"
	"mmx/internal/value"
)

// NpmProjectDependencies installs what an application's package-lock.json
// lists, with npm ci.
type NpmProjectDependencies struct {
	LockFileDir                    string         `yaml:"lock_file_dir" json:"lock_file_dir"`
	RunAsUserAccount               string         `yaml:"run_as_user_account" json:"run_as_user_account"`
	IncludeDevelopmentDependencies *bool          `yaml:"include_development_dependencies" json:"include_development_dependencies"`
	NpmProgramPath                 string         `yaml:"npm_program_path,omitempty" json:"npm_program_path,omitempty"`
	Env                            map[string]any `yaml:"env,omitempty" json:"env,omitempty"`
}

var npmProjectDependenciesDescription = Description{
	Name: "npm_project_dependencies", Topic: TopicLanguageAndToolInstallers, Schema: reflect.TypeOf(NpmProjectDependencies{}),
	Summary: "what an application's package-lock.json (or npm-shrinkwrap.json) lists, installed into its node_modules with npm ci, which never rewrites the lock file; a record .mmx-npm-project-dependencies in the directory makes a rerun unchanged",
	Fields: []Field{
		{Name: "lock_file_dir", Required: true, Notes: "the directory on the node that holds package.json and the lock file, usually a release directory or a checkout"},
		{Name: "run_as_user_account", Required: true, Notes: "the application's account, which runs npm: npm runs code from the packages it installs"},
		{Name: "include_development_dependencies", Required: true, Notes: "false omits the packages listed under devDependencies in package.json. true installs development dependencies, which a build step on the node often needs."},
		{Name: "npm_program_path", Default: "found on the standard path", Notes: "a written value must be absolute; the npm program to run when it is not npm on the standard path; its directory comes first in PATH, so npm finds the node beside it"},
		{Name: "env", Notes: "environment variables for npm, such as NPM_CONFIG_REGISTRY or a token for a private registry; values may be secrets"},
	},
	Example: "- npm_project_dependencies:\n    lock_file_dir: /srv/myapp/releases/v1.4.2\n    run_as_user_account: myapp\n    include_development_dependencies: false\n",
	Results: []string{"status", "change", "value"},
	Dryrun:  "compares the record and reports unchanged or would change with the reason; a lock file directory that does not exist yet, or a lock file or manifest that an earlier node task would change, is not checked",
}

// Prepare checks the inputs when the project loads.
func (n *NpmProjectDependencies) Prepare(base string) error {
	if err := checkProjectDependencies(n.LockFileDir, n.RunAsUserAccount, n.Env); err != nil {
		return err
	}
	return requiredBool("include_development_dependencies", n.IncludeDevelopmentDependencies)
}

// Arguments lists the inputs that may hold a reference, with their rules.
func (n *NpmProjectDependencies) Arguments(arguments *ArgumentList) {
	projectDependencyArguments(arguments, &n.LockFileDir, &n.RunAsUserAccount, "npm_program_path", &n.NpmProgramPath, n.Env)
}

// CleanPaths cleans the resolved paths.
func (n *NpmProjectDependencies) CleanPaths() {
	n.LockFileDir = filepath.Clean(n.LockFileDir)
	if n.NpmProgramPath != "" {
		n.NpmProgramPath = filepath.Clean(n.NpmProgramPath)
	}
}

func (n *NpmProjectDependencies) task(s value.Scope) projectDependencies {
	development := n.IncludeDevelopmentDependencies != nil && *n.IncludeDevelopmentDependencies
	return projectDependencies{
		module: "npm_project_dependencies", directory: n.LockFileDir,
		settings: installerSettings{program: "npm", programPath: n.NpmProgramPath, programField: "npm_program_path", account: n.RunAsUserAccount, variables: n.Env, scope: s},
		manifest: "package.json", lockFiles: []string{"package-lock.json", "npm-shrinkwrap.json"}, installedPaths: []string{"node_modules"},
		chosen:  map[string]any{"include_development_dependencies": development},
		runtime: "node",
		install: func(string) []string {
			dependencies := "--omit=dev"
			if development {
				dependencies = "--include=dev"
			}
			return []string{"ci", dependencies, "--no-fund", "--no-audit"}
		},
	}
}

// Run does the module's work on the node.
func (n *NpmProjectDependencies) Run(ctx context.Context, task NodeTask) protocol.Result {
	return task.Runtime.installProjectDependencies(ctx, n.task(task.Scope))
}

// TrackedInputs lists the installation, which counts by its record.
func (n *NpmProjectDependencies) TrackedInputs() []TrackedInput {
	return []TrackedInput{{Kind: "installed", Path: "npm_project_dependencies in " + n.LockFileDir, State: n}}
}

// ReadState is the record of the last installation.
func (n *NpmProjectDependencies) ReadState(ctx context.Context, _ value.Scope) (string, error) {
	return dependencyStateReader(n.task(nil))
}

// StateAfter is what a dryrun predicts for the installation.
func (n *NpmProjectDependencies) StateAfter(r protocol.Result) (string, bool) {
	return installerStateAfter(r)
}
