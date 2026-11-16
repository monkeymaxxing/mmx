package config

import (
	"fmt"
	"mmx/internal/modules"
)

// These types are shared by the strict project loader and the frozen job bundle.
type Project struct {
	PlaybookName     string                    `yaml:"-" json:"playbook_name,omitempty"`
	SettingsFile     string                    `yaml:"-" json:"-"`
	Sources          Sources                   `yaml:"-" json:"sources,omitempty"`
	Modules          map[string]modules.Module `yaml:"-" json:"external_modules,omitempty"`
	Vault            string                    `yaml:"vault_file,omitempty" json:"vault_file,omitempty"`
	Secrets          map[string]string         `yaml:"secrets_from_env,omitempty" json:"secrets_from_env,omitempty"`
	HistoryRetention string                    `yaml:"run_history_retention,omitempty" json:"run_history_retention"`
	MaxNodesAtOnce   int                       `yaml:"max_nodes_at_once,omitempty" json:"max_nodes_at_once"`
	Defaults         Defaults                  `yaml:"node_task_defaults,omitempty" json:"node_task_defaults"`
	Project          string                    `yaml:"project_name" json:"project_name"`
	Inventory        string                    `yaml:"default_inventory" json:"default_inventory"`
	AllowNested      bool                      `yaml:"allow_role_calls_within_roles" json:"allow_role_calls_within_roles"`
	Vars             map[string]any            `yaml:"vars,omitempty" json:"vars,omitempty"`
	FleetStages      []FleetStage              `yaml:"-" json:"fleet_stages"`

	CommandLineVars map[string]CommandLineVar `yaml:"-" json:"command_line_vars,omitempty"`
	// CommandLineValues are the --var values given to the invocation. A run saves
	// them with the project, so resume reuses them.
	CommandLineValues map[string]any `yaml:"-" json:"command_line_values,omitempty"`
	// WholeProjectValidation marks a playbook that mmx validate checks without
	// a playbook name, and so without --var values: a command-line variable
	// without a default is not known, and a field that must be known before
	// the run starts is not checked when it reads one.
	WholeProjectValidation bool `yaml:"-" json:"-"`
	// OnlyFleetStages are the --only-fleet-stages IDs given to the invocation;
	// empty keeps every fleet stage. A run saves them with the project, so
	// resume repeats the same fleet stages.
	OnlyFleetStages []string `yaml:"-" json:"only_fleet_stages,omitempty"`
}

// CommandLineVar declares a variable set with --var NAME=VALUE.
type CommandLineVar struct {
	Sources Sources `yaml:"-" json:"sources,omitempty"`
	Type    string  `yaml:"type" json:"type"`
	Default any     `yaml:"default,omitempty" json:"default,omitempty"`
}
type Playbook struct {
	Sources         Sources                   `yaml:"-" json:"-"`
	FleetStages     []FleetStage              `yaml:"fleet_stages" json:"fleet_stages"`
	CommandLineVars map[string]CommandLineVar `yaml:"command_line_vars,omitempty" json:"command_line_vars,omitempty"`
}

type Inventory struct {
	Name string `yaml:"-" json:"name"`
	// Path is the inventory file Load read: the project's, or --inventory.
	Path    string               `yaml:"-" json:"-"`
	Sources Sources              `yaml:"-" json:"sources,omitempty"`
	Vault   string               `yaml:"vault_file,omitempty" json:"vault_file,omitempty"`
	Vars    map[string]any       `yaml:"vars,omitempty" json:"vars,omitempty"`
	Nodes   map[string]Node      `yaml:"nodes" json:"nodes"`
	Groups  map[string]NodeGroup `yaml:"groups,omitempty" json:"groups,omitempty"`
}

// NodeGroup lists nodes and the vars it sets for them. Load sorts Nodes, and
// membership lookups rely on that order.
type NodeGroup struct {
	Sources Sources        `yaml:"-" json:"sources,omitempty"`
	Nodes   []string       `yaml:"nodes" json:"nodes"`
	Vars    map[string]any `yaml:"vars,omitempty" json:"vars,omitempty"`
}
type Node struct {
	Sources      Sources        `yaml:"-" json:"sources,omitempty"`
	SSHPassword  string         `yaml:"ssh_password,omitempty" json:"ssh_password,omitempty"`
	SudoPassword string         `yaml:"sudo_password,omitempty" json:"sudo_password,omitempty"`
	Transport    string         `yaml:"connection,omitempty" json:"connection,omitempty"`
	Endpoint     string         `yaml:"https_address,omitempty" json:"https_address,omitempty"`
	RunnerURL    string         `yaml:"mmx_runner_download_url,omitempty" json:"mmx_runner_download_url,omitempty"`
	Address      string         `yaml:"address" json:"address"`
	User         string         `yaml:"ssh_user,omitempty" json:"ssh_user,omitempty"`
	Port         int            `yaml:"ssh_port,omitempty" json:"ssh_port,omitempty"`
	SSHConfig    string         `yaml:"ssh_config,omitempty" json:"ssh_config,omitempty"`
	KnownHosts   string         `yaml:"ssh_known_hosts,omitempty" json:"ssh_known_hosts,omitempty"`
	IdentityFile string         `yaml:"ssh_identity_file,omitempty" json:"ssh_identity_file,omitempty"`
	Vars         map[string]any `yaml:"vars,omitempty" json:"vars,omitempty"`
}
type FleetStage struct {
	Sources     Sources        `yaml:"-" json:"sources,omitempty"`
	Loop        any            `yaml:"loop,omitempty" json:"loop,omitempty"`
	As          string         `yaml:"as,omitempty" json:"as,omitempty"`
	Key         any            `yaml:"key,omitempty" json:"key,omitempty"`
	FleetStages []FleetStage   `yaml:"fleet_stages,omitempty" json:"fleet_stages,omitempty"`
	When        any            `yaml:"when,omitempty" json:"when,omitempty"`
	Outputs     map[string]any `yaml:"outputs,omitempty" json:"outputs,omitempty"`
	// ForEachNode selects the nodes a for_each_node loop runs its fleet_stages
	// for, with the same selectors as nodes; each node is one iteration.
	ForEachNode any `yaml:"for_each_node,omitempty" json:"for_each_node,omitempty"`

	MaxNodesAtOnce    NodesAtOnce `yaml:"max_nodes_at_once,omitempty" json:"max_nodes_at_once,omitempty"`
	WaitForAllInBatch bool        `yaml:"wait_for_all_in_batch,omitempty" json:"wait_for_all_in_batch,omitempty"`
	ID                string      `yaml:"id" json:"id"`
	Nodes             any         `yaml:"nodes" json:"nodes"`
	ExceptNodes       any         `yaml:"except_nodes,omitempty" json:"except_nodes,omitempty"`
	NodeTasks         []Task      `yaml:"node_tasks" json:"node_tasks"`
	Requires          Requires    `yaml:"requires,omitempty" json:"requires,omitempty"`
	OnNodeFailure     string      `yaml:"on_node_failure,omitempty" json:"on_node_failure,omitempty"`

	// SkipWhenNoNodesAreSelected leaves a fleet stage or a for_each_node loop
	// out of the run when its selection, nodes or for_each_node minus
	// except_nodes, is empty; without it an empty selection is an error.
	SkipWhenNoNodesAreSelected bool `yaml:"skip_when_no_nodes_are_selected,omitempty" json:"skip_when_no_nodes_are_selected,omitempty"`

	FailedNodesAllowed FailureAllowance `yaml:"failed_nodes_allowed,omitempty" json:"failed_nodes_allowed,omitempty"`

	SaveOnControllerMachine []SaveOnControllerMachine `yaml:"save_on_controller_machine,omitempty" json:"save_on_controller_machine,omitempty"`
}
type Requires struct {
	SameNode []string `yaml:"same_node,omitempty" json:"same_node,omitempty"`
	AllNodes []string `yaml:"all_nodes,omitempty" json:"all_nodes,omitempty"`
}
type Task struct {
	Sources     Sources        `yaml:"-" json:"sources,omitempty"`
	Loop        any            `yaml:"loop,omitempty" json:"loop,omitempty"`
	As          string         `yaml:"as,omitempty" json:"as,omitempty"`
	Key         any            `yaml:"key,omitempty" json:"key,omitempty"`
	RoleOutputs map[string]any `yaml:"-" json:"role_outputs,omitempty"`
	Prepared    []Task         `yaml:"-" json:"-"`
	Bindings    map[string]any `yaml:"-" json:"-"`
	// Resolved: ResolveArguments made the public identifiers literal data.
	Resolved bool `yaml:"-" json:"-"`
	// WhenWasFalseAtBlockStart marks a tracked task of a node task block
	// whose when was false as the block started and whose arguments did not
	// resolve then: the block fixed none of them and follows no change of
	// the task, so the task must not run.
	WhenWasFalseAtBlockStart bool `yaml:"-" json:"-"`

	TimeLimit          *string `yaml:"time_limit,omitempty" json:"time_limit"`
	TimeLimitPerTry    *string `yaml:"time_limit_per_try,omitempty" json:"time_limit_per_try"`
	Retry              *Retry  `yaml:"retry,omitempty" json:"retry,omitempty"`
	TimeLimitSet       bool    `yaml:"-" json:"-"`
	TimeLimitPerTrySet bool    `yaml:"-" json:"-"`

	ID     string         `yaml:"id,omitempty" json:"id,omitempty"`
	Role   string         `yaml:"role,omitempty" json:"role,omitempty"`
	Inputs map[string]any `yaml:"inputs,omitempty" json:"inputs,omitempty"` // a role call's inputs
	When   any            `yaml:"when,omitempty" json:"when,omitempty"`
	// Module is the name of the module the task calls, and ModuleInputs the
	// inputs written below that name: a core module's input type, or a
	// mapping for a module of the project's own. A role call, task block,
	// show, or set_vars calls no module.
	Module       string `yaml:"-" json:"module,omitempty"`
	ModuleInputs any    `yaml:"-" json:"-"`
	// WholeValueReferences are a core module's inputs written as one
	// whole-value reference in place of a value of the input's type, such as
	// priority_when_registered: "{{ .vars.alternatives_priority }}". ResolveArguments puts their values in.
	WholeValueReferences []WholeValueReference `yaml:"-" json:"whole_value_references,omitempty"`
	// ModuleBase is the directory the module's Prepare read the project's
	// files from: the role's directory inside a role, else the project root.
	ModuleBase string `yaml:"-" json:"-"`
	// NonCoreModule is the module that is not core the task calls, as
	// loading found it, for its description; the project's catalog
	// (Project.Modules) carries it to the nodes.
	NonCoreModule *modules.Module `yaml:"-" json:"-"`
	Show          any             `yaml:"show,omitempty" json:"show,omitempty"`
	SetVars       map[string]any  `yaml:"set_vars,omitempty" json:"set_vars,omitempty"`
	TaskBlock     *TaskBlock      `yaml:"task_block,omitempty" json:"task_block,omitempty"`

	RoleNodeTasks []Task           `yaml:"-" json:"role_node_tasks,omitempty"`
	RoleInputs    map[string]Input `yaml:"-" json:"role_inputs,omitempty"`
	RoleSources   Sources          `yaml:"-" json:"role_sources,omitempty"` // the role file's locations, for output errors
}

// TaskBlock groups related node tasks into phases on one node.
type TaskBlock struct {
	Sources Sources `yaml:"-" json:"sources,omitempty"`
	ID      string  `yaml:"id" json:"id"`
	Changes []Task  `yaml:"changes" json:"changes"`
	// ChecksBeforeRestartOrReload must pass before the block restarts or
	// reloads a running service or runs a command that follows a change.
	ChecksBeforeRestartOrReload []Task `yaml:"checks_before_restart_or_reload,omitempty" json:"checks_before_restart_or_reload,omitempty"`
	Validate                    []Task `yaml:"validate,omitempty" json:"validate,omitempty"`
	Activate                    []Task `yaml:"activate,omitempty" json:"activate,omitempty"`
	Verify                      []Task `yaml:"verify,omitempty" json:"verify,omitempty"`
	OnFailure                   []Task `yaml:"on_failure,omitempty" json:"on_failure,omitempty"`
	Cleanup                     []Task `yaml:"cleanup,omitempty" json:"cleanup,omitempty"`
}

// ChecksPhase is the phase that runs before a task block's first write, and
// only when the block would restart or reload something.
const ChecksPhase = "checks_before_restart_or_reload"

// TaskList is one nested list: a role's node tasks (Phase "") or a task block phase.
type TaskList struct {
	Phase     string
	NodeTasks []Task
}

// Children lists a task's nested task lists in written order. Every static
// walker uses it, so a new stage or child list is added here once.
func (t Task) Children() []TaskList {
	lists := []TaskList{{"", t.RoleNodeTasks}}
	if b := t.TaskBlock; b != nil {
		lists = append(lists, TaskList{ChecksPhase, b.ChecksBeforeRestartOrReload}, TaskList{"changes", b.Changes}, TaskList{"validate", b.Validate}, TaskList{"activate", b.Activate}, TaskList{"verify", b.Verify}, TaskList{"on_failure", b.OnFailure}, TaskList{"cleanup", b.Cleanup})
	}
	return lists
}

// TaskName is a task's identity within its list and in result paths.
func TaskName(t Task, i int) string {
	if t.ID != "" {
		return t.ID
	}
	if t.TaskBlock != nil {
		return t.TaskBlock.ID
	}
	if t.Role != "" {
		return t.Role
	}
	return fmt.Sprintf("task_%d", i+1)
}

type Defaults struct {
	Sources         Sources       `yaml:"-" json:"sources,omitempty"`
	TimeLimit       *string       `yaml:"time_limit,omitempty" json:"time_limit"`
	TimeLimitPerTry *string       `yaml:"time_limit_per_try,omitempty" json:"time_limit_per_try"`
	Retry           RetryDefaults `yaml:"retry,omitempty" json:"retry"`
}
type RetryDefaults struct {
	Sources           Sources  `yaml:"-" json:"sources,omitempty"`
	MaxTries          TryLimit `yaml:"max_tries,omitempty" json:"max_tries"`
	PauseBetweenTries string   `yaml:"pause_between_tries,omitempty" json:"pause_between_tries"`
}
type Retry struct {
	Disabled          bool     `yaml:"-" json:"disabled,omitempty"`
	MaxTries          TryLimit `yaml:"max_tries,omitempty" json:"max_tries"`
	PauseBetweenTries string   `yaml:"pause_between_tries,omitempty" json:"pause_between_tries"`
	Until             any      `yaml:"until,omitempty" json:"until,omitempty"`
	RetryWhen         any      `yaml:"retry_when,omitempty" json:"retry_when,omitempty"`
	MaxSet            bool     `yaml:"-" json:"-"`
	PauseSet          bool     `yaml:"-" json:"-"`
}
type Input struct {
	Sources  Sources `yaml:"-" json:"sources,omitempty"`
	Secret   bool    `yaml:"secret,omitempty" json:"secret,omitempty"`
	Type     string  `yaml:"type" json:"type"`
	Required bool    `yaml:"required,omitempty" json:"required,omitempty"`
	Default  any     `yaml:"default,omitempty" json:"default,omitempty"`
}
type Role struct {
	Sources Sources        `yaml:"-" json:"sources,omitempty"`
	Outputs map[string]any `yaml:"outputs,omitempty" json:"outputs,omitempty"`

	Inputs    map[string]Input `yaml:"inputs,omitempty" json:"inputs,omitempty"`
	NodeTasks []Task           `yaml:"node_tasks" json:"node_tasks"`
}
