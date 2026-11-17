package controller

import (
	"encoding/json"
	"fmt"
	"slices"
	"strconv"

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

// A node job's outputs are data the node chose. MMX's own secret records
// in them are instructions to the controller process: a name loads that
// secret again, and a read_file origin makes mmx resume read that file on
// that node. A node runner an attacker controls, or a playbook exporting
// someone else's JSON, could write such a record. So the controller process
// takes a name only for a part of a named secret it sent that node job whole
// (nodeJobSecrets), and a read_file origin only for that node job's own fleet
// stage and node (ownOrigins). Every other record keeps the node's data, as a
// secret the node job produced.

// trustNodeRecords returns a node job's outputs with only the records the
// node job could have made. sent gives the named secrets delivered to that
// node job, computed only when a record names one.
func (w *fleetStageWalker) trustNodeRecords(outputs map[string]any, origin value.NodeOrigin, sent func() []*value.Secret) map[string]any {
	var trust func(any) any
	trust = func(v any) any {
		switch x := v.(type) {
		case *value.Secret:
			if x.Name == "" || w.wasSent(x, sent()) {
				return ownOrigins(x, origin)
			}
			return &value.Secret{Data: x.Data, Available: x.Available, FromNode: &value.NodeOrigin{FleetStage: origin.FleetStage, Node: origin.Node}, HiddenBecause: x.HiddenBecause}
		case map[string]any:
			if x == nil {
				return x
			}
			out := map[string]any{}
			for k, child := range x {
				out[k] = trust(child)
			}
			return out
		case []any:
			out := make([]any, len(x))
			for i, child := range x {
				out[i] = trust(child)
			}
			return out
		default:
			return v
		}
	}
	trusted, _ := trust(outputs).(map[string]any)
	return trusted
}

// ownOrigins replaces a secret's origin that names another fleet stage or
// node than the node job origin, which sent it, with origin itself: such a
// secret is unavailable on resume, and nothing reads a file for it.
func ownOrigins(v any, origin value.NodeOrigin) any {
	switch x := v.(type) {
	case *value.Secret:
		if x.FromNode == nil || x.FromNode.FleetStage == origin.FleetStage && x.FromNode.Node == origin.Node {
			return x
		}
		copy := *x
		copy.FromNode = &value.NodeOrigin{FleetStage: origin.FleetStage, Node: origin.Node}
		return &copy
	case map[string]any:
		if x == nil {
			return x
		}
		out := map[string]any{}
		for k, child := range x {
			out[k] = ownOrigins(child, origin)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, child := range x {
			out[i] = ownOrigins(child, origin)
		}
		return out
	default:
		return v
	}
}

// wasSent reports whether a node job received the whole part of a named
// secret that record names: a secret it received only in part, such as one
// field, names only that field.
func (w *fleetStageWalker) wasSent(record *value.Secret, sent []*value.Secret) bool {
	full, err := w.lookupSecret(record.Name)
	if err != nil {
		return false
	}
	whole, ok := selectSecretPart(full, record.Path)
	if !ok {
		return false
	}
	for _, s := range sent {
		if s.Name != record.Name || len(s.Path) > len(record.Path) || !slices.Equal(s.Path, record.Path[:len(s.Path)]) {
			continue
		}
		part, ok := selectSecretPart(s.Data, record.Path[len(s.Path):])
		if ok && sameJSON(part, whole) {
			return true
		}
	}
	return false
}

// selectSecretPart follows path into a secret's data, as value.Refresh does.
func selectSecretPart(data any, path []string) (any, bool) {
	for _, part := range path {
		switch c := data.(type) {
		case map[string]any:
			var ok bool
			if data, ok = c[part]; !ok {
				return nil, false
			}
		case []any:
			n, err := strconv.Atoi(part)
			if err != nil || n < 0 || n >= len(c) {
				return nil, false
			}
			data = c[n]
		default:
			return nil, false
		}
	}
	return data, true
}

// sameJSON compares two secret values as JSON, so a number read from the
// vault equals the same number decoded from a node's answer.
func sameJSON(a, b any) bool {
	x, errX := json.Marshal(value.Transport(a))
	y, errY := json.Marshal(value.Transport(b))
	return errX == nil && errY == nil && string(x) == string(y)
}

// nodeJobSecrets lists the named secrets a node job of the fleet stage on the
// node received, as delivery computes them from the scope the node job was
// built from.
func nodeJobSecrets(r *Run, fleetStage config.FleetStage, node string, scope value.Scope) []*value.Secret {
	job, err := nodeJob(r, fleetStage, Entry{Node: node, scope: scope})
	if err == nil {
		job, err = scopeJobSecrets(job)
	}
	if err != nil {
		return nil
	}
	var sent []*value.Secret
	var collect func(any)
	collect = func(v any) {
		switch x := v.(type) {
		case *value.Secret:
			if x.Name != "" && x.Available {
				sent = append(sent, x)
			}
		case map[string]any:
			for _, child := range x {
				collect(child)
			}
		case []any:
			for _, child := range x {
				collect(child)
			}
		}
	}
	collect(job.Scope)
	return sent
}
