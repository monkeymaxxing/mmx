package modules

import (
	"context"
	"path/filepath"
	"reflect"

	"mmx/internal/protocol"
	"mmx/internal/value"
)

// UvProjectDependencies installs what a Python application's uv.lock lists,
// with uv sync.
type UvProjectDependencies struct {
	LockFileDir                    string         `yaml:"lock_file_dir" json:"lock_file_dir"`
	RunAsUserAccount               string         `yaml:"run_as_user_account" json:"run_as_user_account"`
	IncludeDevelopmentDependencies *bool          `yaml:"include_development_dependencies" json:"include_development_dependencies"`
	UvProgramPath                  string         `yaml:"uv_program_path,omitempty" json:"uv_program_path,omitempty"`
	PythonProgramPath              string         `yaml:"python_program_path,omitempty" json:"python_program_path,omitempty"`
	Env                            map[string]any `yaml:"env,omitempty" json:"env,omitempty"`
}

var uvProjectDependenciesDescription = Description{
	Name: "uv_project_dependencies", Topic: TopicLanguageAndToolInstallers, Schema: reflect.TypeOf(UvProjectDependencies{}),
	Summary: "what a Python application's uv.lock lists, installed into its .venv with uv sync --locked, which never rewrites the lock file and never downloads a Python; a record .mmx-uv-project-dependencies in the directory makes a rerun unchanged",
	Fields: []Field{
		{Name: "lock_file_dir", Required: true, Notes: "the directory on the node that holds pyproject.toml and uv.lock, usually a release directory or a checkout; the virtual environment is its .venv"},
		{Name: "run_as_user_account", Required: true, Notes: "the application's account, which runs uv: uv runs code from the packages it builds"},
		{Name: "include_development_dependencies", Required: true, Notes: "false passes --no-dev; true passes --dev; other groups follow the application's uv settings"},
		{Name: "uv_program_path", Default: "found on the standard path", Notes: "a written value must be absolute; the uv program to run when it is not uv on the standard path, such as uv unpacked under /opt"},
		{Name: "python_program_path", Notes: "absolute path to the Python executable for the virtual environment, such as a Python installed under /opt with uv python install. When python_program_path is omitted, uv chooses an installed Python version accepted by the project"},
		{Name: "env", Notes: "environment variables for uv, such as UV_INDEX_URL or a token for a private package index; values may be secrets"},
	},
	Example: "- uv_project_dependencies:\n    lock_file_dir: /srv/myapp/releases/v1.4.2\n    run_as_user_account: myapp\n    include_development_dependencies: false\n",
	Results: []string{"status", "change", "value"},
	Dryrun:  "compares the record and reports unchanged or would change with the reason; a lock file directory that does not exist yet, a lock file that an earlier node task would change, or a python_program_path that is not installed yet, is not checked",
}

// Prepare checks the inputs when the project loads.
func (u *UvProjectDependencies) Prepare(base string) error {
	if err := checkProjectDependencies(u.LockFileDir, u.RunAsUserAccount, u.Env); err != nil {
		return err
	}
	return requiredBool("include_development_dependencies", u.IncludeDevelopmentDependencies)
}

// Arguments lists the inputs that may hold a reference, with their rules.
func (u *UvProjectDependencies) Arguments(arguments *ArgumentList) {
	projectDependencyArguments(arguments, &u.LockFileDir, &u.RunAsUserAccount, "uv_program_path", &u.UvProgramPath, u.Env)
	if u.PythonProgramPath != "" {
		arguments.Public("python_program_path", &u.PythonProgramPath, absolute)
	}
}

// CleanPaths cleans the resolved paths.
func (u *UvProjectDependencies) CleanPaths() {
	u.LockFileDir = filepath.Clean(u.LockFileDir)
	if u.UvProgramPath != "" {
		u.UvProgramPath = filepath.Clean(u.UvProgramPath)
	}
	if u.PythonProgramPath != "" {
		u.PythonProgramPath = filepath.Clean(u.PythonProgramPath)
	}
}

func (u *UvProjectDependencies) task(s value.Scope) projectDependencies {
	development := u.IncludeDevelopmentDependencies != nil && *u.IncludeDevelopmentDependencies
	chosen := map[string]any{"include_development_dependencies": development}
	if u.PythonProgramPath != "" {
		chosen["python_program_path"] = u.PythonProgramPath
	}
	return projectDependencies{
		module: "uv_project_dependencies", directory: u.LockFileDir,
		settings: installerSettings{program: "uv", programPath: u.UvProgramPath, programField: "uv_program_path", account: u.RunAsUserAccount, variables: u.Env, scope: s},
		manifest: "pyproject.toml", lockFiles: []string{"uv.lock"}, installedPaths: []string{".venv"},
		chosen:  chosen,
		runtime: "python3", runtimePath: u.PythonProgramPath, runtimeField: "python_program_path",
		install: func(string) []string {
			// uv would otherwise download a Python of its own into the
			// account's home directory, outside every node task.
			arguments := []string{"sync", "--locked", "--no-python-downloads"}
			if development {
				arguments = append(arguments, "--dev")
			} else {
				arguments = append(arguments, "--no-dev")
			}
			if u.PythonProgramPath != "" {
				arguments = append(arguments, "--python", u.PythonProgramPath)
			}
			return arguments
		},
	}
}

// Run does the module's work on the node.
func (u *UvProjectDependencies) Run(ctx context.Context, task NodeTask) protocol.Result {
	return task.Runtime.installProjectDependencies(ctx, u.task(task.Scope))
}

// TrackedInputs lists the installation, which counts by its record.
func (u *UvProjectDependencies) TrackedInputs() []TrackedInput {
	return []TrackedInput{{Kind: "installed", Path: "uv_project_dependencies in " + u.LockFileDir, State: u}}
}

// ReadState is the record of the last installation.
func (u *UvProjectDependencies) ReadState(ctx context.Context, _ value.Scope) (string, error) {
	return dependencyStateReader(u.task(nil))
}

// StateAfter is what a dryrun predicts for the installation.
func (u *UvProjectDependencies) StateAfter(r protocol.Result) (string, bool) {
	return installerStateAfter(r)
}
