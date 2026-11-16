package modules

import (
	"context"
	"path/filepath"
	"reflect"
	"strings"

	"mmx/internal/protocol"
	"mmx/internal/value"
)

// YarnProjectDependencies installs what an application's yarn.lock lists.
type YarnProjectDependencies struct {
	LockFileDir                    string         `yaml:"lock_file_dir" json:"lock_file_dir"`
	RunAsUserAccount               string         `yaml:"run_as_user_account" json:"run_as_user_account"`
	IncludeDevelopmentDependencies *bool          `yaml:"include_development_dependencies" json:"include_development_dependencies"`
	YarnProgramPath                string         `yaml:"yarn_program_path,omitempty" json:"yarn_program_path,omitempty"`
	Env                            map[string]any `yaml:"env,omitempty" json:"env,omitempty"`
}

var yarnProjectDependenciesDescription = Description{
	Name: "yarn_project_dependencies", Topic: TopicLanguageAndToolInstallers, Schema: reflect.TypeOf(YarnProjectDependencies{}),
	Summary: "what an application's yarn.lock lists, installed with the Yarn install that refuses to change yarn.lock: yarn install --frozen-lockfile with Yarn 1, --immutable with Yarn 2 and newer; a record .mmx-yarn-project-dependencies in the directory makes a rerun unchanged",
	Fields: []Field{
		{Name: "lock_file_dir", Required: true, Notes: "the directory on the node that holds package.json and yarn.lock, usually a release directory or a checkout"},
		{Name: "run_as_user_account", Required: true, Notes: "the application's account, which runs Yarn: Yarn runs code from the packages it installs"},
		{Name: "include_development_dependencies", Required: true, Notes: "false leaves out devDependencies (Yarn 2 and newer use yarn workspaces focus --all --production); true installs them"},
		{Name: "yarn_program_path", Default: "found on the standard path", Notes: "absolute path to the yarn executable to use instead of yarn from the standard PATH; the executable's directory is placed first in PATH"},
		{Name: "env", Notes: "environment variables for Yarn, such as a token for a private registry; values may be secrets"},
	},
	Example: "- yarn_project_dependencies:\n    lock_file_dir: /srv/myapp/releases/v1.4.2\n    run_as_user_account: myapp\n    include_development_dependencies: false\n",
	Results: []string{"status", "change", "value"},
	Dryrun:  "compares the record and reports unchanged or would change with the reason; a lock file directory that does not exist yet, or a lock file or manifest that an earlier node task would change, is not checked",
}

// Prepare checks the inputs when the project loads.
func (y *YarnProjectDependencies) Prepare(base string) error {
	if err := checkProjectDependencies(y.LockFileDir, y.RunAsUserAccount, y.Env); err != nil {
		return err
	}
	return requiredBool("include_development_dependencies", y.IncludeDevelopmentDependencies)
}

// Arguments lists the inputs that may hold a reference, with their rules.
func (y *YarnProjectDependencies) Arguments(arguments *ArgumentList) {
	projectDependencyArguments(arguments, &y.LockFileDir, &y.RunAsUserAccount, "yarn_program_path", &y.YarnProgramPath, y.Env)
}

// CleanPaths cleans the resolved paths.
func (y *YarnProjectDependencies) CleanPaths() {
	y.LockFileDir = filepath.Clean(y.LockFileDir)
	if y.YarnProgramPath != "" {
		y.YarnProgramPath = filepath.Clean(y.YarnProgramPath)
	}
}

func (y *YarnProjectDependencies) task(s value.Scope) projectDependencies {
	development := y.IncludeDevelopmentDependencies != nil && *y.IncludeDevelopmentDependencies
	return projectDependencies{
		module: "yarn_project_dependencies", directory: y.LockFileDir,
		settings: installerSettings{program: "yarn", programPath: y.YarnProgramPath, programField: "yarn_program_path",
			providedBy: "Yarn, such as with npm_global_package with a yarn packages entry", account: y.RunAsUserAccount, variables: y.Env, scope: s},
		manifest: "package.json", lockFiles: []string{"yarn.lock"},
		// Yarn 2 and newer install into a Plug'n'Play file unless the
		// project chooses node_modules.
		installedPaths: []string{"node_modules", ".pnp.cjs"},
		chosen:         map[string]any{"include_development_dependencies": development},
		runtime:        "node",
		// Yarn 2 and newer refuse to change yarn.lock with this setting,
		// also in workspaces focus.
		env: map[string]string{"YARN_ENABLE_IMMUTABLE_INSTALLS": "true"},
		install: func(version string) []string {
			if strings.HasPrefix(strings.TrimSpace(version), "1.") {
				production := "--production=true"
				if development {
					production = "--production=false"
				}
				return []string{"install", "--frozen-lockfile", "--non-interactive", production}
			}
			if development {
				return []string{"install", "--immutable"}
			}
			return []string{"workspaces", "focus", "--all", "--production"}
		},
	}
}

// Run does the module's work on the node.
func (y *YarnProjectDependencies) Run(ctx context.Context, task NodeTask) protocol.Result {
	return task.Runtime.installProjectDependencies(ctx, y.task(task.Scope))
}

// TrackedInputs lists the installation, which counts by its record.
func (y *YarnProjectDependencies) TrackedInputs() []TrackedInput {
	return []TrackedInput{{Kind: "installed", Path: "yarn_project_dependencies in " + y.LockFileDir, State: y}}
}

// ReadState is the record of the last installation.
func (y *YarnProjectDependencies) ReadState(ctx context.Context, _ value.Scope) (string, error) {
	return dependencyStateReader(y.task(nil))
}

// StateAfter is what a dryrun predicts for the installation.
func (y *YarnProjectDependencies) StateAfter(r protocol.Result) (string, bool) {
	return installerStateAfter(r)
}
