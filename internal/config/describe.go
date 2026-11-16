package config

import (
	"errors"
	"fmt"
	"io/fs"
	"mmx/internal/modules"
	"os"
	"path/filepath"
	"reflect"
)

var (
	taskSchema  = reflect.TypeOf(Task{})
	resultBasic = modules.ResultBasic
)

// descriptions lists every task action, then the settings every task accepts.
var descriptions = []modules.Description{
	{
		Name: "show", Schema: taskSchema,
		Summary: "displays a message or structured value",
		Fields: []modules.Field{
			{Name: "show", Type: "text or value", Required: true, Notes: "references resolve; secrets are redacted"},
		},
		Example: "- show: \"Configuring {{ .node.name }}\"\n",
		Results: []string{"status", "change", "value"},
		Dryrun:  "runs; values not known yet are marked, not guessed",
	},
	{
		Name: "set_vars", Schema: taskSchema,
		Summary: "assigns local variables for later node tasks",
		Fields: []modules.Field{
			{Name: "set_vars", Required: true, Notes: "each variable gets a data value or the boolean result of a condition; all variable assignments apply together"},
		},
		Example: "- set_vars:\n    greeting: \"Hello from {{ .node.name }}\"\n",
		Results: resultBasic,
		Dryrun:  "runs",
	},
	{
		Name: "role", Schema: taskSchema,
		Summary: "runs roles/<name>/main.yml with its declared inputs",
		Fields: []modules.Field{
			{Name: "role", Required: true, Notes: "a directory name under roles/ beside the project file"},
			{Name: "inputs", Notes: "values passed to the role; declarations are in the role file's inputs mapping"},
		},
		Example: "- role: web_server\n  inputs:\n    port: 8080\n",
		Results: []string{"status", "change", "outputs"},
		Dryrun:  "each node task uses the dryrun behavior of the action the node task calls",
		Retry:   "does not accept retry; set retry on individual node tasks inside the role or node task block",
	},
	{
		Name: "task_block", Schema: reflect.TypeOf(TaskBlock{}),
		Summary: "groups changes with their validation, activation, verification, and cleanup",
		Fields: []modules.Field{
			{Name: "id", Required: true, Notes: "the node task block's name"},
			{Name: "changes", Required: true, Notes: "node tasks that make changes, such as file, systemd_unit, apt_package, and git; the changes node tasks supply the node task block's tracked inputs"},
			{Name: "checks_before_restart_or_reload", Notes: "checks using assert, http, wait_for, show, or shell with changed_when: false; they run only when the block would restart or reload a running service or run a command with only_when_tracked_inputs_changed: before the block writes any changes when MMX can foresee that, or else just before the restart, reload, or command; a failed check fails the node task block before the restart, reload, or command, and before any change is written when the checks ran first"},
			{Name: "validate", Notes: "shell or http checks of the combined configuration before activation"},
			{Name: "activate", Notes: "systemd_service node tasks restart or reload services when required by tracked inputs; shell node tasks run on every playbook run unless only_when_tracked_inputs_changed: true is set"},
			{Name: "verify", Notes: "health checks after activation"},
			{Name: "on_failure", Notes: "runs after a failed phase"},
			{Name: "cleanup", Notes: "runs last, even after a failure"},
		},
		Example: "- task_block:\n    id: nginx_config\n    changes:\n      - file:\n          permissions_mode: \"0644\"\n          path: /etc/nginx/conf.d/app.conf\n          content: \"server { listen 8080; }\\n\"\n    validate:\n      - shell:\n          script: nginx -t\n    activate:\n      - systemd_service:\n          name: nginx\n          refresh: reload\n          state: running\n",
		Results: resultBasic,
		Dryrun:  "phases preview their node tasks; validation runs only declared dryrun commands; no service action runs",
		Retry:   "does not accept retry; set retry on individual node tasks inside the role or node task block",
	},
	{
		Name: "task", Schema: taskSchema,
		Summary: "settings accepted by every node task",
		Fields: []modules.Field{
			{Name: "id", Notes: "names the result, read as .node_tasks.<id>"},
			{Name: "when", Type: "boolean or predicate", Notes: "false skips the task without resolving its arguments"},
			{Name: "loop", Type: "list, mapping, or reference", Notes: "runs the task once per item"},
			{Name: "as", Notes: "the loop variable name used to read each item; required with loop"},
			{Name: "key", Type: "string", Notes: "a stable key identifying each loop iteration; required for node task block loops"},
			{Name: "time_limit", Type: "duration or null", Notes: "time limit for the whole node task, including retries; project default in node_task_defaults.time_limit"},
			{Name: "time_limit_per_try", Type: "duration or null", Notes: "time limit for one attempt at the node task; project default in node_task_defaults.time_limit_per_try"},
			{Name: "retry", Type: "boolean or mapping", Notes: "true, false, or {max_tries, pause_between_tries, until, retry_when}; max_tries is a positive integer or unlimited (requires time_limit), never null; project defaults in node_task_defaults.retry"},
		},
		Example: "- id: health\n  when:\n    equals: [\"{{ .node.name }}\", web]\n  http:\n    url: http://127.0.0.1:8080/health\n  time_limit: 1m\n  retry:\n    max_tries: 10\n    pause_between_tries: 3s\n",
		Results: []string{"status", "change", "reason", "items"},
		Dryrun:  "when and loops are evaluated; each action previews by its own rule",
		Retry:   "per action; node_task_defaults.retry sets max_tries (3) and pause_between_tries (2s)",
	},
}

// Descriptions returns every entry in display order.
func Descriptions() ([]modules.Description, error) {
	var out []modules.Description
	for _, d := range allDescriptions() {
		described, err := describe(d)
		if err != nil {
			return nil, err
		}
		out = append(out, described)
	}
	return out, nil
}

// allDescriptions lists the core modules from the registry, then the node task
// forms that are not modules and the settings every task accepts.
func allDescriptions() []modules.Description {
	var all []modules.Description
	for _, m := range modules.CoreModules() {
		all = append(all, *m.Description)
	}
	return append(all, descriptions...)
}

// Describe returns one entry by name.
func Describe(name string) (modules.Description, error) {
	for _, d := range allDescriptions() {
		if d.Name == name {
			return describe(d)
		}
	}
	return modules.Description{}, fmt.Errorf("unknown module %q; run mmx describe-module to list available modules", name)
}

func describe(d modules.Description) (modules.Description, error) {
	fields := yamlFields(d.Schema)
	d.Fields = append([]modules.Field{}, d.Fields...)
	for i, f := range d.Fields {
		t, ok := fields[f.Name]
		if !ok {
			return d, fmt.Errorf("%s: no field %s", d.Name, f.Name)
		}
		if f.Type == "" {
			d.Fields[i].Type = yamlType(t)
		}
	}
	if m, core := modules.LookUpCoreModule(d.Name); core {
		d.Retry = coreModuleRetryText(m)
	} else if d.Retry == "" {
		d.Retry = "runs once unless the task sets retry"
	}
	return d, nil
}

// coreModuleRetryText states a core module's retry policy from the registry
// flags that validation reads, in the order resolveTiming checks them, so
// the description cannot disagree with validation.
func coreModuleRetryText(m modules.CoreModule) string {
	switch {
	case m.WaitsWithItsOwnLimit:
		return "runs once; rejects retry, time_limit, and time_limit_per_try, and ignores node_task_defaults; " + m.Description.WaitLimit
	case m.EvaluatesOnce:
		return "evaluates once and does not accept retry"
	case m.RetryByDefault:
		return "bounded automatic retries by default; retry: false turns them off"
	}
	return "runs once unless the task sets retry"
}

// yamlType names a Go field type as YAML; it is empty for any.
func yamlType(t reflect.Type) string {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.String:
		return "string"
	case reflect.Bool:
		return "boolean"
	case reflect.Int:
		return "integer"
	case reflect.Map, reflect.Struct:
		return "mapping"
	case reflect.Slice:
		if t.Elem() == taskSchema {
			return "list of node tasks"
		}
		return "list of " + yamlType(t.Elem()) + "s"
	default:
		// Interfaces have no fixed YAML type; non-schema kinds have no YAML type name.
	}
	return ""
}

// NonCoreModule is what mmx describe-module shows for a module that is not
// core: its kind, where its files are, and its description.
type NonCoreModule struct {
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	Directory string `json:"directory"`
	// Builds are an executable's files for the node's processor.
	Builds []string `json:"builds,omitempty"`
	modules.ModuleDescription
}

// DescribeNonCoreModule reads the module that is not core with that name, as
// loading finds it: among the official extra modules, then in the project's
// external_modules/ when projectFile names a project.
func DescribeNonCoreModule(projectFile, name string) (NonCoreModule, error) {
	base := ""
	if projectFile != "" {
		base = filepath.Dir(projectFile)
	}
	module, err := findModule(base, OfficialExtraModulesDirectory(), name)
	if err != nil {
		return NonCoreModule{}, err
	}
	directory := filepath.Join(OfficialExtraModulesDirectory(), name)
	if module.Kind != modules.KindOfficialExtra {
		directory = filepath.Join("external_modules", name)
	}
	d := NonCoreModule{Name: name, Kind: module.Kind, Directory: directory, ModuleDescription: *module.Description}
	for _, f := range module.Files {
		if modules.IsModuleEntry(f.Path) {
			d.Builds = append(d.Builds, f.Path)
		}
	}
	return d, nil
}

// NonCoreModules lists the official extra modules and, inside a project, the
// project's own modules, each read as loading reads it.
func NonCoreModules(projectFile string) ([]NonCoreModule, error) {
	var names []string
	for _, directory := range []string{OfficialExtraModulesDirectory(), filepath.Join(filepath.Dir(projectFile), "external_modules")} {
		if projectFile == "" && directory != OfficialExtraModulesDirectory() {
			continue
		}
		entries, err := os.ReadDir(directory)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			if e.IsDir() && modules.ModuleName.MatchString(e.Name()) {
				names = append(names, e.Name())
			}
		}
	}
	var out []NonCoreModule
	for _, name := range names {
		d, err := DescribeNonCoreModule(projectFile, name)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, nil
}
