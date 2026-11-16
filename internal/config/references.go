package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"mmx/internal/modules"
	"mmx/internal/value"
	"regexp"
	"strings"
)

var simpleReference = value.Reference

type declarations map[string]map[string]bool

// secretRoleInputs is the declarations key that lists a role's inputs
// declared secret: true. The dot keeps it apart from every reference root.
const secretRoleInputs = "inputs.declared_secret"

// readsASecretRoleInput says whether a value reads an input that the
// enclosing role declares secret: true.
func readsASecretRoleInput(v any, d declarations) bool {
	for _, input := range roleInputsRead(v) {
		if d[secretRoleInputs][input] {
			return true
		}
	}
	return false
}

// variablesSetLater is the declarations key that lists the variables a
// set_vars in the fleet stage sets, so that a reference before the set_vars is
// told apart from a variable that nothing declares.
const variablesSetLater = "vars.set_in_fleet_stage"

// undeclaredVariableError is a reference to a variable that the node's layers
// and the earlier set_vars do not declare. inRole says the reference is in a
// role, which reads no variable of the project, the inventory, or the command
// line.
type undeclaredVariableError struct {
	path, name       string
	setLater, inRole bool
}

func (e *undeclaredVariableError) Error() string {
	return fmt.Sprintf("reference %s names a value that is not declared or is declared later", e.path)
}

// variablePlace is the place of a reference to a variable, which decides where
// the variable can be declared.
type variablePlace int

const (
	// variablesOfOneNode: a node task or fleet stage output, read for one
	// node after that fleet stage's earlier set_vars ran.
	variablesOfOneNode variablePlace = iota
	// variablesOfOneNodeBeforeSetVars: a fleet stage's when, read for one node
	// before any of its node tasks run, so its set_vars cannot help.
	variablesOfOneNodeBeforeSetVars
	// variablesOfNoSingleNode: a place the controller process reads before
	// it chooses a node, or that no node owns: nodes, except_nodes, loop,
	// for_each_node, a loop's when and outputs, save_on_controller_machine.
	// Only the variables every node shares are declared here.
	variablesOfNoSingleNode
)

// explainUndeclaredVariable replaces, in the text of err, the message of an
// undeclaredVariableError with one that says where the variable can be
// declared at the place of the reference, for managedNode ("" for every
// node). The wrapping errors above it have already copied its text, so the
// text is replaced, not the error.
func explainUndeclaredVariable(err error, p Project, inv Inventory, managedNode string, place variablePlace) error {
	var undeclared *undeclaredVariableError
	if !errors.As(err, &undeclared) || undeclared.setLater {
		return err
	}
	var where string
	switch {
	case undeclared.inRole:
		where = "is not available in a role: a role reads only its inputs and the variables its own set_vars set earlier; declare an input in the role file and pass the value in the role call's inputs"
	case place == variablesOfNoSingleNode:
		where = "is not declared where this place can read it: no single node is chosen here, so declare it under vars in mmx_project_settings.yml, under vars at the top level of inventories/" + inv.Name + ".yml (not inside a group or a node), or in command_line_vars of the playbook"
	default:
		places := "vars in mmx_project_settings.yml or in inventories/" + inv.Name + ".yml (the inventory, a group, or the node), in command_line_vars of the playbook"
		setVars := ", or set it earlier in this fleet stage with set_vars"
		if place == variablesOfOneNodeBeforeSetVars {
			setVars = ". set_vars cannot set it here, because a fleet stage's when is read before the fleet stage's node tasks run"
			places = strings.Replace(places, ", in command_line_vars", ", or in command_line_vars", 1)
		}
		where = "is not declared for any node: declare it under " + places + setVars
		if managedNode != "" {
			var have []string
			for _, other := range sortedKeys(inv.Nodes) {
				if other != managedNode && variableNames(p, inv, other)[undeclared.name] {
					have = append(have, other)
				}
			}
			if len(have) > 0 {
				where = fmt.Sprintf("is declared for node %s but not for node %s: declare it for node %s under vars in a group the node belongs to or in the node's own vars in inventories/%s.yml%s", strings.Join(have, ", "), managedNode, managedNode, inv.Name, setVars)
			} else {
				where = fmt.Sprintf("is not declared for node %s: declare it under %s%s", managedNode, places, setVars)
			}
		}
	}
	return errors.New(strings.Replace(err.Error(), undeclared.Error(), "reference "+undeclared.path+" "+where, 1))
}

func copyDeclarations(d declarations) declarations {
	out := declarations{}
	for root, names := range d {
		if names == nil {
			out[root] = nil
			continue
		}
		out[root] = map[string]bool{}
		for n, b := range names {
			out[root][n] = b
		}
	}
	return out
}
func names[V any](m map[string]V) map[string]bool {
	out := map[string]bool{}
	for n := range m {
		out[n] = true
	}
	return out
}
func checkPath(path string, d declarations) error {
	parts, err := value.Path(path)
	if err != nil {
		return err
	}
	known, ok := d[parts[0].Name]
	if !ok {
		return fmt.Errorf("unknown reference root .%s; %s", parts[0].Name, value.InvalidReference)
	}
	if parts[0].Name == "secrets" && (len(parts) < 2 || parts[1].Dynamic != "" || !modules.Name.MatchString(parts[1].Name)) {
		return fmt.Errorf("secret references must name a fixed secret, such as .secrets.database_password")
	}
	if len(parts) > 1 && known != nil && parts[1].Dynamic == "" && !known[parts[1].Name] {
		if parts[0].Name == "vars" {
			// The fleet stage check adds where to declare the variable.
			return &undeclaredVariableError{path: path, name: parts[1].Name, setLater: d[variablesSetLater][parts[1].Name], inRole: d[secretRoleInputs] != nil}
		}
		return fmt.Errorf("reference %s names a value that is not declared or is declared later", path)
	}
	for _, part := range parts {
		if part.Dynamic != "" {
			if err := checkPath(part.Dynamic, d); err != nil {
				return err
			}
			// A node gets a secret only as a value to use, never as the key
			// of a lookup: delivery sends nothing for such a reference, and
			// the node job would end blocked.
			selector, _ := value.Path(part.Dynamic)
			if len(selector) > 0 && selector[0].Name == "secrets" {
				return fmt.Errorf("%s uses the secret %s as a selector, but a secret cannot select another value. Read the secret directly as {{ %s }}, or use a non-secret value, such as an inventory variable, to select the value on the controller machine", strings.TrimSpace(path), part.Dynamic, part.Dynamic)
			}
			// Inside a role, an input declared secret: true holds a secret.
			if len(selector) > 1 && selector[0].Name == "inputs" && selector[1].Dynamic == "" && d[secretRoleInputs][selector[1].Name] {
				input := ".inputs." + selector[1].Name
				return fmt.Errorf("%s uses the role input %s as a selector, but the input is declared secret: true, and a secret cannot select another value. Read the input directly as {{ %s }}, or select with a role input that is not declared secret: true, and have the role call set the input from a value that is no secret, such as an inventory variable", strings.TrimSpace(path), input, part.Dynamic)
			}
		}
	}
	if len(parts) > 1 && (parts[0].Name == "outputs" || parts[0].Name == "fleet_stages") && parts[1].Dynamic != "" {
		return fmt.Errorf("the fleet stage name in %s must be written directly, without a reference or lookup", path)
	}
	if len(parts) >= 3 && (parts[0].Name == "outputs" || parts[0].Name == "fleet_stages") {
		if fields, ok := d["outputs."+parts[1].Name]; parts[0].Name == "outputs" && len(parts) >= 4 && ok && parts[3].Dynamic == "" && !fields[parts[3].Name] {
			return fmt.Errorf("unknown output in %s", path)
		}
		if nodes := d["nodes."+parts[1].Name]; nodes != nil && parts[2].Dynamic == "" && !nodes[parts[2].Name] {
			return fmt.Errorf("reference %s names a node that the referenced fleet stage does not select", path)
		}
	}
	if len(parts) >= 4 && parts[0].Name == "fleet_stages" && !contains([]string{"status", "change", "reason"}, parts[3].Name) {
		return fmt.Errorf("unknown fleet stage outcome field in %s", path)
	}
	if parts[0].Name == "node_tasks" {
		fields := []string{"outputs", "items", "iteration_key", "status", "change", "exit_code", "stdout", "stdout_json", "stderr", "reason", "http_status_code", "response_body", "response_body_json", "value", "public_key", "content"}
		// A module that is not core adds the outputs its description lists.
		if len(parts) > 1 && parts[1].Dynamic == "" {
			for output := range d["node_tasks."+parts[1].Name+".module_outputs"] {
				fields = append(fields, output)
			}
		}
		for i := 2; i < len(parts); i += 2 {
			part := parts[i]
			if part.Dynamic != "" {
				break
			}
			if !contains(fields, part.Name) {
				return fmt.Errorf("unknown node task result field in %s; valid fields: %s", path, strings.Join(fields, ", "))
			}
			if part.Name != "items" {
				break
			}
		}
	}
	if len(parts) >= 3 && parts[0].Name == "node_tasks" && parts[1].Dynamic == "" && parts[2].Name == "outputs" && d["node_tasks."+parts[1].Name+".items"] != nil {
		return fmt.Errorf("role call %s runs in a loop; read its outputs through .node_tasks.%s.items[n].outputs", parts[1].Name, parts[1].Name)
	}
	if len(parts) >= 4 && parts[0].Name == "node_tasks" && parts[2].Name == "outputs" {
		if fields := d["node_tasks."+parts[1].Name+".outputs"]; fields != nil && parts[3].Dynamic == "" && !fields[parts[3].Name] {
			return fmt.Errorf("unknown role output in %s", path)
		}
	}
	return nil
}

func checkValue(v any, d declarations) error {
	switch v := v.(type) {
	case string:
		references := value.WithoutLiterals(v)
		for _, m := range simpleReference.FindAllStringSubmatch(references, -1) {
			if err := checkPath(m[1], d); err != nil {
				return err
			}
		}
		rest := simpleReference.ReplaceAllString(references, "")
		// A }} on its own is ordinary text.
		if strings.Contains(rest, "{{") {
			return fmt.Errorf("invalid value reference %q: %s", v, value.InvalidReference)
		}
	case []any:
		for _, x := range v {
			if err := checkValue(x, d); err != nil {
				return err
			}
		}
	case map[string]any:
		for _, x := range v {
			if err := checkValue(x, d); err != nil {
				return err
			}
		}
	}
	return nil
}

// checkPredicate checks a condition that does not inspect the node.
func checkPredicate(v any, d declarations) error { return checkCondition(v, d, false) }

// checkCondition also accepts path_exists where onNode says the condition is
// the when of a node task or node task block.
func checkCondition(v any, d declarations, onNode bool) error {
	if v == nil {
		return nil
	}
	switch v := v.(type) {
	case bool:
		return nil
	case string:
		if !simpleReference.MatchString(v) || simpleReference.FindString(v) != v {
			return fmt.Errorf("condition must be true, false, or a whole-value reference that resolves to a boolean")
		}
		return checkValue(v, d)
	case map[string]any:
		if len(v) != 1 {
			return fmt.Errorf("predicate requires one operator")
		}
		for op, arg := range v {
			switch op {
			case "equals", "contains", "matches":
				a, ok := arg.([]any)
				if !ok || len(a) != 2 {
					return fmt.Errorf("%s requires two values", op)
				}
				if op == "contains" || op == "matches" {
					for _, x := range a {
						if _, ok := x.(string); !ok {
							return fmt.Errorf("%s requires two strings", op)
						}
					}
					if op == "matches" && !modules.Dynamic(a[1]) {
						if _, err := regexp.Compile(a[1].(string)); err != nil {
							return fmt.Errorf("matches requires a valid Go regular expression")
						}
					}
				}
				return checkValue(a, d)
			case "all", "any":
				a, ok := arg.([]any)
				if !ok {
					return fmt.Errorf("%s requires a list", op)
				}
				for _, x := range a {
					if err := checkCondition(x, d, onNode); err != nil {
						return err
					}
				}
				return nil
			case "not":
				return checkCondition(arg, d, onNode)
			case "path_exists":
				if !onNode {
					return fmt.Errorf("path_exists checks a path on the node and is allowed only in a node task's or node task block's when condition")
				}
				path, ok := arg.(string)
				if !ok || (!modules.Dynamic(path) && !strings.HasPrefix(path, "/")) {
					return fmt.Errorf("path_exists requires an absolute path")
				}
				return checkValue(path, d)
			default:
				if value.IsComparison(op) {
					a, ok := arg.([]any)
					if !ok || len(a) != 2 {
						return fmt.Errorf("%s requires two values", op)
					}
					for _, x := range a {
						switch x.(type) {
						case string, int, int64, float64, json.Number:
						default:
							return fmt.Errorf("%s compares numbers or text, not %T", op, x)
						}
					}
					return checkValue(a, d)
				}
				return fmt.Errorf("unknown predicate %q", op)
			}
		}
	}
	return fmt.Errorf("invalid predicate")
}
func checkShell(s *modules.Shell, d declarations) error {
	//nolint:nilaway // moduleReads removes unresolved whole-value commands; validation requires shell for every remaining check.
	if err := checkValue(s.Env, d); err != nil {
		return err
	}
	for _, x := range []string{s.Cwd, s.User, s.SkipIfThisPathExists, s.RunOnlyIfThisPathExists} {
		if err := checkValue(x, d); err != nil {
			return err
		}
	}
	local := copyDeclarations(d)
	local["this_node_task"] = map[string]bool{"status": true, "change": true, "exit_code": true, "stdout": true, "stdout_json": true, "stderr": true, "http_status_code": true, "response_body": true, "response_body_json": true, "value": true, "reason": true}
	if err := checkPredicate(s.ChangedWhen, local); err != nil {
		return &modules.FieldError{Field: "changed_when", Err: err}
	}
	if err := checkPredicate(s.FailedWhen, local); err != nil {
		return &modules.FieldError{Field: "failed_when", Err: err}
	}
	return nil
}
func checkTemplate(source string, d declarations) error {
	reads, err := value.TemplateReads(source)
	if err != nil {
		return err
	}
	for _, path := range reads {
		if path == "." {
			return fmt.Errorf("template reads the whole value scope; read specific values such as .vars.port")
		}
		if err := checkPath(path, d); err != nil {
			return err
		}
	}
	return nil
}

func validateReferences(nodeTasks []Task, d declarations) error {
	identities := map[string]bool{}
	for i, t := range nodeTasks {
		identity := TaskName(t, i)
		if identities[identity] {
			field := ""
			if t.ID != "" {
				field = "id"
			}
			return t.Sources.Error(field, fmt.Errorf("duplicate node task name %q: MMX names a node task by its id, else by its node task block's id or its role, else task_N for its place in the list; give repeated role calls distinct ids", identity))
		}
		identities[identity] = true
		parent := d
		if t.Loop != nil {
			if err := checkLoop(t.Loop, t.As, t.Key, d, t.TaskBlock != nil); err != nil {
				return t.Sources.Error("", err)
			}
			d = copyDeclarations(d)
			d[t.As] = nil
		} else if t.As != "" || t.Key != nil {
			return t.Sources.Error("", fmt.Errorf("as and key require loop on the node task"))
		}
		check := func() error {
			if err := checkCondition(t.When, d, true); err != nil {
				return t.Sources.Error("when", err)
			}
			var role declarations
			if t.Role != "" {
				for _, k := range sortedKeys(t.Inputs) {
					if err := checkValue(t.Inputs[k], d); err != nil {
						return t.Sources.Error("inputs."+k, err)
					}
				}
				inputNames, secretInputNames := map[string]bool{}, map[string]bool{}
				for _, k := range sortedKeys(t.RoleInputs) {
					spec := t.RoleInputs[k]
					if supplied, ok := t.Inputs[k]; ok {
						if spec.Secret && !modules.Dynamic(supplied) {
							return t.Sources.Error("inputs."+k, fmt.Errorf("role input %s requires a secret reference, not a plaintext value", k))
						}
						if !spec.Secret && (len(SecretNames(supplied)) > 0 || readsASecretRoleInput(supplied, d)) {
							return t.Sources.Error("inputs."+k, fmt.Errorf("role input %s receives a secret; declare secret: true", k))
						}
					}
					inputNames[k] = true
					if spec.Secret {
						secretInputNames[k] = true
					}
				}
				role = declarations{"vars": {}, "inputs": inputNames, secretRoleInputs: secretInputNames, "node_tasks": {}, "node": nil}
			}
			// Role tasks see only their inputs; task block phases share the caller's names.
			for _, list := range t.Children() {
				names := d
				if list.Phase == "" {
					names = role
				}
				if err := validateReferences(list.NodeTasks, names); err != nil {
					return err
				}
			}
			if t.Role != "" {
				if err := checkOutputs(t.RoleOutputs, role); err != nil {
					return t.RoleSources.Error("", err)
				}
			}
			if err := checkModuleReferences(t, d); err != nil {
				return err
			}
			if err := checkValue(t.Show, d); err != nil {
				return t.Sources.Error("show", err)
			}
			for _, k := range sortedKeys(t.SetVars) {
				if !modules.Name.MatchString(k) {
					return t.Sources.Error("set_vars."+k, fmt.Errorf("invalid variable name %s", k))
				}
				if err := checkDeclared(t.SetVars[k], d); err != nil {
					return t.Sources.Error("set_vars."+k, err)
				}
			}
			for k := range t.SetVars {
				d["vars"][k] = true
			}
			if t.Retry != nil {
				local := copyDeclarations(d)
				local["this_node_task"] = nil
				if err := checkPredicate(t.Retry.Until, local); err != nil {
					return t.Sources.Error("retry.until", err)
				}
				if err := checkPredicate(t.Retry.RetryWhen, local); err != nil {
					return t.Sources.Error("retry.retry_when", err)
				}
			}
			return nil
		}
		if err := check(); err != nil {
			return err
		}
		d = parent
		if t.ID != "" {
			d["node_tasks"][t.ID] = true //nolint:nilaway // Fleet-stage and role declarations allocate node_tasks before reference checking.
			if t.NonCoreModule != nil && t.NonCoreModule.Description != nil {
				outputs := map[string]bool{}
				for _, output := range t.NonCoreModule.Description.Outputs {
					outputs[output.Name] = true
				}
				d["node_tasks."+t.ID+".module_outputs"] = outputs
			}
			if t.Role != "" && t.Loop == nil {
				d["node_tasks."+t.ID+".outputs"] = names(t.RoleOutputs)
			} else if t.Role != "" {
				d["node_tasks."+t.ID+".items"] = map[string]bool{} // outputs are per iteration
			}
		}
	}
	return nil
}
