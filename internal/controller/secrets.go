package controller

import (
	"fmt"

	"mmx/internal/access"
	"mmx/internal/config"
	"mmx/internal/secrets"
	"mmx/internal/value"
)

func (c *Controller) bindSecrets(r *Run) {
	if c.Secrets == nil {
		c.Secrets = &secrets.Store{}
	}
	c.Secrets.Vault, c.Secrets.Inputs = r.Project.Vault, r.Project.Secrets
	if c.Credentials == nil {
		c.Credentials = &access.Credentials{}
	}
	c.Credentials.Secret = func(name string) (string, error) {
		v, err := c.Secrets.Get(name)
		if err != nil {
			return "", err
		}
		text, ok := v.(string)
		if !ok {
			return "", fmt.Errorf("connection secret %s must be a string", name)
		}
		return text, nil
	}
}

// A run or resume loads every named secret that the fleet stages it runs read
// (config.SecretNamesARunReads), with the run's limit; delivery sends each job
// only what its own references reach.
func (c Controller) secretScope(r *Run, limit config.Limit) (map[string]any, error) {
	out := map[string]any{}
	for _, name := range config.SecretNamesARunReads(r.Project, r.Inventory, limit) {
		raw, err := c.Secrets.Get(name)
		if err != nil {
			return nil, err
		}
		out[name] = &value.Secret{Name: name, Data: raw, Available: true}
	}
	return out, nil
}

// assignments lists the variables node tasks may set on the node, including within
// roles and task block phases.
func assignments(nodeTasks []config.Task, names map[string]bool) {
	for _, t := range nodeTasks {
		for name := range t.SetVars {
			names[name] = true
		}
		for _, list := range t.Children() {
			assignments(list.NodeTasks, names)
		}
	}
}

// A job carries only the secret values its assigned definition's references
// reach. The controller follows a dynamic selector it can resolve exactly as
// the node will: the node's name, earlier outputs, fleet stage outcomes,
// inventory, fleet stage loop items, and variables no task in the job
// reassigns. If such a selector cannot resolve, the node fails the read, so
// nothing is delivered for it. A selector never reads a named secret: the
// loader rejects such a reference (config.checkPath). Facts, results, task
// loop items, role inputs, and reassigned variables exist only on the node,
// which selects within what it receives. Other exported values may be present
// as references, but never as plaintext inputs.
func scopeJobSecrets(job config.Job) (config.Job, error) {
	keys := value.Scope(merge(job.Scope))
	delete(keys, "node_tasks") // The controller's scope has no task results.
	keys["node"] = map[string]any{"name": job.Node}
	keys["vars"] = merge(job.Vars)
	reassigned := map[string]bool{}
	assignments(job.NodeTasks, reassigned)
	for path, reason := range job.Unavailable {
		keys.Mark(path, reason)
	}
	nodeOnly := func(parts []value.Selector) bool {
		field := "" // A fixed field name; a dynamic or index selector has none.
		if len(parts) > 1 {
			field = parts[1].Name
		}
		switch parts[0].Name {
		case "node":
			return field != "name"
		case "vars":
			return field == "" || reassigned[field]
		}
		_, known := keys[parts[0].Name]
		return !known
	}
	allowed := map[*value.Secret]*value.Secret{}
	for _, path := range config.References([]any{job.NodeTasks, job.Outputs}) {
		v, err := value.Scope(job.Scope).Within(path, keys, nodeOnly)
		if err != nil {
			return job, err
		}
		value.CollectSecrets(v, allowed)
	}
	job.Scope = value.KeepSecrets(job.Scope, allowed).(map[string]any)
	job.PrivateInputs = value.HasPrivateData(job.Scope) || value.HasPrivateData(job.Vars)
	return job, nil
}
