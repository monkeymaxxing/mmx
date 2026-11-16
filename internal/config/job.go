package config

import (
	"encoding/json"
	"errors"
	"mmx/internal/assets"
	"mmx/internal/modules"
	"mmx/internal/protocol"
	"mmx/internal/value"
)

// The frozen node job: one fleet stage for one node, as the controller process
// sends it and the node job worker process runs it.

type Job struct {
	ReachabilityConfirmationConnectionKinds []protocol.ConnectionKind `json:"reachability_confirmation_connection_kinds,omitempty"`
	Origins                                 Origins                   `json:"origins,omitempty"`
	Modules                                 map[string]modules.Module `json:"modules,omitempty"`
	PrivateInputs                           bool                      `json:"private_inputs,omitempty"`
	Scope                                   map[string]any            `json:"scope,omitempty"`
	Unavailable                             map[string]string         `json:"unavailable,omitempty"`
	OmittedEntries                          map[string]string         `json:"omitted_entries,omitempty"`
	// EarlierChangeInThisDryrun names a node task of an earlier fleet stage on
	// the node that this dryrun found would change it.
	EarlierChangeInThisDryrun string `json:"earlier_change_in_this_dryrun,omitempty"`
	// KeptFiles names the files of the job that the node keeps for later
	// runs (KeptFilesOfANodeJob). The controller process names them: the job
	// carries no web address that a template gives.
	KeptFiles []assets.Ref   `json:"kept_files,omitempty"`
	Outputs   map[string]any `json:"outputs,omitempty"`
	// Sources locates the fleet stage's outputs for their errors. The
	// controller process checks the fleet stage's when before the job.
	Sources Sources `json:"sources,omitempty"`
	// ControllerMachine is set for a fleet stage that ends with a reboot
	// task: the node compares it with itself before the reboot.
	ControllerMachine *protocol.ControllerMachine `json:"controller_machine,omitempty"`
	// StartedByAccount and StartedFromHostName name the local account and the
	// controller machine that started the node job. The node names them when
	// it refuses another job as busy. They are not secrets. An older node job
	// record lacks them.
	StartedByAccount    string `json:"started_by_account,omitempty"`
	StartedFromHostName string `json:"started_from_host_name,omitempty"`

	Protocol   int            `json:"protocol"`
	ID         string         `json:"id"`
	Project    string         `json:"project"`
	FleetStage string         `json:"fleet_stage"`
	Node       string         `json:"node"`
	Dryrun     bool           `json:"dryrun"`
	Vars       map[string]any `json:"vars,omitempty"`
	NodeTasks  []Task         `json:"node_tasks"`
}

func (j Job) Transport() ([]byte, error) {
	j.PrivateInputs = value.HasPrivateData(j.Scope) || value.HasPrivateData(j.Vars)
	scope, scopeIsMapping := value.Transport(j.Scope).(map[string]any)
	vars, varsAreMapping := value.Transport(j.Vars).(map[string]any)
	if !scopeIsMapping || !varsAreMapping {
		return nil, errors.New("The node job scope or variables could not be encoded as mappings of names. Check the scope and variable values before starting the playbook run again.")
	}
	j.Scope, j.Vars = scope, vars
	return json.Marshal(j)
}

func (j *Job) UnmarshalJSON(b []byte) error {
	type plain Job
	if err := protocol.Decode(b, (*plain)(j)); err != nil {
		return err
	}
	if err := protocol.ValidateConfirmationConnectionKinds(j.ReachabilityConfirmationConnectionKinds); err != nil {
		return err
	}
	scope, scopeIsMapping := value.Restore(j.Scope).(map[string]any)
	vars, varsAreMapping := value.Restore(j.Vars).(map[string]any)
	if !scopeIsMapping || !varsAreMapping {
		return errors.New("The node job scope or variables contain an internal MMX record instead of a mapping of names. Rename reserved mapping keys before starting the playbook run again.")
	}
	j.Scope, j.Vars = scope, vars
	return nil
}
