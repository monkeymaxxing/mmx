package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"

	"mmx/internal/modules"
	"mmx/internal/value"
)

const originsKey = "\x00origins"
const secretOriginsKey = "\x00secret_origins"

type Origins map[string]Location

func (o Origins) At(ref string) Location {
	parts, err := value.Path(ref)
	if err != nil {
		return Location{}
	}
	path := ""
	var found Location
	for _, part := range value.StaticPrefix(parts) {
		if part.Index != nil {
			path += fmt.Sprintf("[%d]", *part.Index)
		} else {
			path += "." + part.Name
		}
		if at, ok := o[path]; ok {
			found = at
		}
	}
	return found
}
func ScopeOrigins(s value.Scope) Origins  { o, _ := s[originsKey].(Origins); return o }
func SetOrigins(s value.Scope, o Origins) { s[originsKey] = o }

// varLayer is one source of a node's variables, with their locations.
type varLayer struct {
	vars    map[string]any
	sources Sources
}

// varLayers lists a node's variable layers in value order: project,
// inventory, its groups (which never set the same variable), its own vars,
// and the command-line variables, whose names no other layer sets. An empty
// node gives the layers every node shares.
func varLayers(p Project, inv Inventory, managedNode string) []varLayer {
	layers := []varLayer{{p.Vars, p.Sources}, {inv.Vars, inv.Sources}}
	layers = append(layers, nodeLayers(inv, managedNode)...)
	return append(layers, commandLineLayer(p))
}

// hostLayers are the layers that differ by node: its groups' vars, then its
// own vars.
func nodeLayers(inv Inventory, managedNode string) []varLayer {
	var layers []varLayer
	for _, group := range NodeGroups(inv, managedNode) {
		layers = append(layers, varLayer{inv.Groups[group].Vars, inv.Groups[group].Sources})
	}
	return append(layers, varLayer{inv.Nodes[managedNode].Vars, inv.Nodes[managedNode].Sources})
}

// everyHostLayer lists the layers any node may add to the shared ones: every
// group's vars, then every node's, in name order.
func everyNodeLayer(inv Inventory) []varLayer {
	var layers []varLayer
	for _, group := range sortedKeys(inv.Groups) {
		layers = append(layers, varLayer{inv.Groups[group].Vars, inv.Groups[group].Sources})
	}
	for _, managedNode := range sortedKeys(inv.Nodes) {
		layers = append(layers, varLayer{inv.Nodes[managedNode].Vars, inv.Nodes[managedNode].Sources})
	}
	return layers
}

// commandLineLayer holds each command-line variable's --var value, else its
// default; a variable with neither is absent. Its origin is the --var option or
// the default's location.
func commandLineLayer(p Project) varLayer {
	layer := varLayer{map[string]any{}, Sources{}}
	for name, declared := range p.CommandLineVars {
		if v, ok := p.CommandLineValues[name]; ok {
			layer.vars[name], layer.sources["vars."+name] = v, Location{Field: "--var " + name}
		} else if declared.Default != nil {
			layer.vars[name], layer.sources["vars."+name] = declared.Default, declared.Sources.At("default")
		}
	}
	return layer
}

// NodeGroups lists the groups that list node, by name.
func NodeGroups(inv Inventory, managedNode string) []string {
	var groups []string
	for _, name := range sortedKeys(inv.Groups) {
		if _, found := slices.BinarySearch(inv.Groups[name].Nodes, managedNode); found {
			groups = append(groups, name)
		}
	}
	return groups
}

// NodeVars and InventoryValues build a node's variables and the .inventory
// values for validation, explain, and the controller's jobs alike. An empty
// node gives the variables every node shares.
func NodeVars(p Project, inv Inventory, managedNode string) map[string]any {
	return layeredVars(varLayers(p, inv, managedNode))
}
func layeredVars(layers []varLayer) map[string]any {
	vars := map[string]any{}
	for _, layer := range layers {
		for k, v := range layer.vars {
			vars[k] = v
		}
	}
	return vars
}

// InventoryValues gives each node's name, address, and the vars that differ by
// node (its groups' and its own), and each group's sorted node list. The
// shared layers (project, inventory, command line) are read through .vars, so
// jobs do not carry a copy of them per node.
func InventoryValues(inv Inventory) map[string]any {
	groups := map[string]any{}
	for name, g := range inv.Groups {
		items := make([]any, len(g.Nodes))
		for i, h := range g.Nodes {
			items[i] = h
		}
		groups[name] = items
	}
	nodes := map[string]any{}
	for name, h := range inv.Nodes {
		nodes[name] = map[string]any{"name": name, "address": h.Address, "vars": layeredVars(nodeLayers(inv, name))}
	}
	return map[string]any{"nodes": nodes, "groups": groups}
}

func VariableOrigins(p Project, inv Inventory, managedNode string) Origins {
	out := Origins{}
	for _, layer := range varLayers(p, inv, managedNode) {
		for key := range layer.vars {
			prefix := ".vars." + key
			for old := range out {
				if old == prefix || strings.HasPrefix(old, prefix+".") || strings.HasPrefix(old, prefix+"[") {
					delete(out, old)
				}
			}
			var remember func(string, string, any)
			remember = func(ref, field string, v any) {
				out[ref] = layer.sources.At(field)
				switch x := v.(type) {
				case map[string]any:
					for name, child := range x {
						remember(ref+"."+name, field+"."+name, child)
					}
				case []any:
					for i, child := range x {
						suffix := fmt.Sprintf("[%d]", i)
						remember(ref+suffix, field+suffix, child)
					}
				}
			}
			remember(prefix, "vars."+key, layer.vars[key])
		}
	}
	return out
}

func argumentError(t Task, field string, raw any, s value.Scope, err error) error {
	return SourceError(t.Sources, field, raw, s, err)
}

// SourceError is the one form of a value error: it names the consuming field
// in sources and, when the value raw read came from elsewhere, where it was
// supplied. Validation, the engine, and the controller's fleet stages all use it.
func SourceError(sources Sources, field string, raw any, s value.Scope, err error) error {
	for _, ref := range References(raw) {
		if at := ScopeOrigins(s).At(ref); at != (Location{}) && at != sources.At(field) {
			err = fmt.Errorf("%w; value supplied at %s", err, at)
			break
		}
	}
	return sources.Error(field, err)
}

func InputType(v any, kind string) bool {
	v = value.Plain(v)
	switch kind {
	case "string":
		_, ok := v.(string)
		return ok
	case "boolean":
		_, ok := v.(bool)
		return ok
	case "integer":
		switch x := v.(type) {
		case int, int64, uint64:
			return true
		case json.Number:
			_, err := x.Int64()
			return err == nil
		case float64:
			return x == float64(int64(x))
		}
	case "list":
		_, ok := v.([]any)
		return ok
	case "mapping":
		_, ok := v.(map[string]any)
		return ok
	}
	return false
}

// roleInput is what a role call gives input k: its supplied input value, else the
// default. ok is false when the call gives neither.
func roleInput(t Task, k string) (raw any, provided, ok bool) {
	spec := t.RoleInputs[k]
	raw, provided = t.Inputs[k]
	if !provided {
		raw = spec.Default
	}
	return raw, provided, provided || spec.Required || spec.Default != nil
}

// RoleScope is a role call's own scope before its inputs resolve: the node,
// each input's origin and secret names, and nothing else from the caller.
// It also keeps the inventory the node job carries, which a module such as
// ufw's from_inventory_group reads; validation still refuses a reference to
// .inventory in a role. Inspection walks an inactive call's node tasks in it
// without resolving inputs.
func RoleScope(t Task, parent value.Scope) value.Scope {
	child := value.Scope{"vars": map[string]any{}, "inputs": map[string]any{}, "node_tasks": map[string]any{}, "node": parent["node"], "inventory": parent["inventory"]}
	child.InheritTrace(parent)
	child.InheritOutputLines(parent)
	origins := Origins{}
	SetOrigins(child, origins)
	secretOrigins := map[string][]string{}
	child[secretOriginsKey] = secretOrigins
	for path, reason := range parent.Missing() {
		if path == "/node" || strings.HasPrefix(path, "/node/") {
			child.Mark(path, reason)
		}
	}
	if reason, partly := parent.PartlyKnown()[value.Pointer("node")]; partly {
		child.MarkPartlyKnown(value.Pointer("node"), reason)
	}
	for _, k := range sortedKeys(t.RoleInputs) {
		raw, provided, ok := roleInput(t, k)
		if !ok {
			continue
		}
		origins[".inputs."+k] = t.Sources.At("inputs." + k)
		if !provided {
			origins[".inputs."+k] = t.RoleInputs[k].Sources.At("default")
		}
		if t.RoleInputs[k].Secret {
			secretOrigins[".inputs."+k] = referenceSecrets(parent, raw)
			if len(secretOrigins[".inputs."+k]) == 0 {
				secretOrigins[".inputs."+k] = []string{k}
			}
		}
		if refs := References(raw); len(refs) > 0 {
			if at := ScopeOrigins(parent).At(refs[0]); at != (Location{}) {
				origins[".inputs."+k] = at
			}
		}
	}
	return child
}

// ResolveRoleInputs is shared by local inspection and real execution. Unknown
// runtime inputs stay unavailable during local inspection, never become defaults.
//
// A role call only passes its inputs on, so a secret in an input marks no
// trace here and does not hide the role's node tasks by itself. The input
// keeps its secrecy: the node task inside the role that reads it marks its
// own trace and, through it, the trace of every role call around it. From
// then on the role's later node tasks are hidden too, as in a node task
// block (value.Scope.Trace).
func ResolveRoleInputs(t Task, parent value.Scope, partial bool) (value.Scope, error) {
	child := RoleScope(t, parent)
	inputs := child["inputs"].(map[string]any)
	for _, k := range sortedKeys(t.RoleInputs) {
		spec := t.RoleInputs[k]
		raw, _, ok := roleInput(t, k)
		if !ok {
			continue
		}
		v, err := parent.ResolveWithoutMarkingATrace(raw)
		if err != nil {
			var unavailable *value.UnavailableError
			if partial && errors.As(err, &unavailable) {
				child.Mark(value.Pointer("inputs", k), unavailable.Reason)
				continue
			}
			return nil, argumentError(t, "inputs."+k, raw, parent, err)
		}
		if spec.Secret && !value.HasSecrets(v) {
			return nil, argumentError(t, "inputs."+k, raw, parent, fmt.Errorf("input %s requires a named secret or secret-derived value", k))
		}
		if !spec.Secret && value.HasSecrets(v) {
			return nil, argumentError(t, "inputs."+k, raw, parent, fmt.Errorf("input %s receives a secret; declare secret: true", k))
		}
		if !InputType(v, spec.Type) {
			return nil, argumentError(t, "inputs."+k, raw, parent, fmt.Errorf("input %s must be %s", k, spec.Type))
		}
		inputs[k] = v
	}
	return child, nil
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

type Argument struct {
	Field     string     `json:"field"`
	Value     any        `json:"value"`
	Reference any        `json:"reference,omitempty"`
	Status    string     `json:"status"`
	Source    Location   `json:"source"`
	Origins   []Location `json:"origins,omitempty"`
	Secrets   []string   `json:"secrets,omitempty"`
}

// A public identifier (path, name, owner, version, URL, ID) has a target, the
// field ResolveArguments writes; private values have none.
type taskArgument struct {
	field  string
	raw    any
	rule   func(any) error
	target any // *string, *Account, or *any
	// loginHandedOnlyToATool: a private value that the module hands only to
	// its tool as a login (modules.ArgumentList.LoginHandedOnlyToATool).
	loginHandedOnlyToATool bool
	// wholeSecretReference: the value must be one whole reference to a named
	// secret or to a role input declared secret: true
	// (modules.ArgumentList.WholeSecretReference).
	wholeSecretReference bool
}

// taskArguments is the one rule table for reference-capable arguments. Load
// applies each rule to literal values in every branch; inspection and the
// engine apply it to resolved values. Static schema fields and literal
// scripts/sources do not gain another expression language here.
func taskArguments(t Task) []taskArgument {
	var a []taskArgument
	public := func(field string, target any, rule func(any) error) {
		arg := taskArgument{field: field, rule: rule, target: target}
		switch p := target.(type) {
		case *string:
			arg.raw = *p
		case *any:
			arg.raw = *p
		}
		a = append(a, arg)
	}
	private := func(field string, raw any, rule func(any) error) {
		a = append(a, taskArgument{field: field, raw: raw, rule: rule})
	}
	// A variable source field resolves like any identifier; the node then picks
	// the file the controller prepared for the resolved value.
	for _, s := range taskSources(&t) {
		if !s.Dynamic() {
			continue
		}
		if s.Kind == modules.SourceKeys {
			for _, p := range modules.KeyPaths(s.Keys) {
				if modules.Dynamic(*p.Name) {
					public("authorized_keys."+p.Field, p.Name, modules.NonEmptyScalar)
				}
			}
		} else if s.Kind == modules.SourceDownload {
			public(s.Field, s.URL, modules.Scalar)
			if *s.SHA256 != "" {
				public(strings.TrimSuffix(s.Field, "url")+"sha256", s.SHA256, modules.Scalar)
			}
		} else {
			public(s.Field, s.Path, modules.NonEmptyScalar)
		}
	}
	if name, inputs := t.ModuleCall(); inputs != nil {
		a = append(a, wholeValueArguments(t, name, inputs)...)
		if m, ok := inputs.(interface{ Arguments(*modules.ArgumentList) }); ok {
			list := modules.ArgumentList{Resolved: t.Resolved}
			m.Arguments(&list)
			for _, arg := range list.List {
				if underWholeValue(t, arg.Field) {
					continue // its rules apply once the reference resolved
				}
				a = append(a, taskArgument{field: name + "." + arg.Field, raw: arg.Raw, rule: arg.Rule, target: arg.Target, loginHandedOnlyToATool: arg.LoginHandedOnlyToATool, wholeSecretReference: arg.WholeSecretReference})
			}
		}
	}
	if t.Show != nil {
		private("show", t.Show, nil)
	}
	for _, k := range sortedKeys(t.SetVars) {
		private("set_vars."+k, t.SetVars[k], nil)
	}
	// An input that the module's description marks private_value is handed
	// only to a tool, as a core module's login is.
	inputs := t.NonCoreModuleInputs()
	for _, k := range sortedKeys(inputs) {
		arg := taskArgument{field: t.Module + "." + k, raw: inputs[k]}
		arg.loginHandedOnlyToATool = t.NonCoreModule != nil && t.NonCoreModule.Description.PrivateValue(k)
		a = append(a, arg)
	}
	return a
}

// checkLiterals applies the argument rules to values written without references.
func checkLiterals(t Task) error {
	for _, arg := range taskArguments(t) {
		if arg.rule != nil && !modules.Dynamic(arg.raw) {
			if err := arg.rule(arg.raw); err != nil {
				return t.Sources.Error(arg.field, err)
			}
		}
	}
	return nil
}

// InspectArguments resolves and checks a task's arguments in s. On a Resolved
// task it checks only the private values.
func InspectArguments(t Task, s value.Scope, partial bool) ([]Argument, error) {
	return inspectArguments(t, taskArguments(t), s, partial)
}
func inspectArguments(t Task, args []taskArgument, s value.Scope, partial bool) ([]Argument, error) {
	var out []Argument
	for _, arg := range args {
		if t.Resolved && arg.target != nil {
			continue
		}
		view := Argument{Field: arg.field, Source: t.Sources.At(arg.field), Status: "resolved", Secrets: referenceSecrets(s, arg.raw)}
		if modules.Dynamic(arg.raw) {
			view.Reference = arg.raw
		}
		for _, ref := range References(arg.raw) {
			if at := ScopeOrigins(s).At(ref); at != (Location{}) {
				view.Origins = append(view.Origins, at)
			}
		}
		v, err := resolveArgument(t, arg, s)
		var unavailable *value.UnavailableError
		if err != nil && partial && errors.As(err, &unavailable) {
			view.Status = "runtime"
			if len(view.Secrets) > 0 {
				view.Status = "secret"
			}
			out = append(out, view)
			continue
		}
		if err != nil {
			return nil, err
		}
		view.Value = value.Display(v)
		if value.HasSecrets(v) {
			view.Status = "secret"
			view.Value = nil
		}
		out = append(out, view)
	}
	return out, nil
}

// identifierSecretError is the error of an identifier field whose value
// holds a secret. It names the reference that reads the secret and, for the
// output of a hidden node task, why that node task was hidden, and "value
// supplied at" follows that reference, never another value the field reads.
func identifierSecretError(t Task, arg taskArgument, s value.Scope) error {
	err := errors.New("resource identifier must not contain a secret")
	for _, ref := range References(arg.raw) {
		read, resolveErr := s.ResolveWithoutMarkingATrace("{{ " + ref + " }}")
		if resolveErr != nil || !value.HasSecrets(read) {
			continue
		}
		err = fmt.Errorf("%w; it reads %s, which holds a secret", err, ref)
		if because := value.HiddenBecause(read); because != "" {
			err = fmt.Errorf("%w: the result of that node task is hidden because %s", err, because)
		}
		return argumentError(t, arg.field, "{{ "+ref+" }}", s, err)
	}
	return argumentError(t, arg.field, arg.raw, s, err)
}

// filledValueWithParentPart refuses a public identifier, such as a path,
// into which a reference fills text with a .. part. MMX cleans a resolved
// path, so /srv/sites/{{ .outputs.list.web1.site }} with ../../etc would name
// /etc: another node would choose where this node writes. Every reference
// counts, since vars, role inputs, and loop items can all carry an output
// (set_vars, inputs, loop). A .. written in the project's own text stays.
func filledValueWithParentPart(t Task, arg taskArgument, s value.Scope) error {
	for _, ref := range References(arg.raw) {
		read, err := s.ResolveWithoutMarkingATrace("{{ " + ref + " }}")
		if err != nil {
			continue
		}
		if text, found := textWithParentPart(value.Plain(read)); found {
			return argumentError(t, arg.field, "{{ "+ref+" }}", s, fmt.Errorf("%s is %q, which has a .. part; a value filled into a path cannot climb out of the directory written around it", ref, text))
		}
	}
	return nil
}

// textWithParentPart finds a text in v, or in v's lists and mappings, with a
// .. part between slashes.
func textWithParentPart(v any) (string, bool) {
	switch x := v.(type) {
	case string:
		return x, slices.Contains(strings.Split(x, "/"), "..")
	case []any:
		for _, item := range x {
			if text, found := textWithParentPart(item); found {
				return text, true
			}
		}
	case map[string]any:
		for _, item := range x {
			if text, found := textWithParentPart(item); found {
				return text, true
			}
		}
	}
	return "", false
}

// resolveArgument resolves one argument in s and applies its rule. A login
// that the module hands only to its tool is resolved as the module resolves
// it, without a mark in s's trace, so that the check of its rule does not
// hide the task's result.
func resolveArgument(t Task, arg taskArgument, s value.Scope) (any, error) {
	var v any
	var err error
	switch {
	case t.SetVars != nil:
		v, err = s.Evaluate(arg.raw)
	case arg.loginHandedOnlyToATool:
		v, err = s.LoginHandedOnlyToATool(arg.raw)
	default:
		v, err = s.Resolve(arg.raw)
	}
	if err == nil && arg.target != nil && value.HasSecrets(v) {
		return nil, identifierSecretError(t, arg, s)
	}
	if err == nil && arg.rule != nil {
		err = arg.rule(v)
	}
	if err != nil {
		return nil, argumentError(t, arg.field, arg.raw, s, err)
	}
	if arg.target != nil {
		if err := filledValueWithParentPart(t, arg, s); err != nil {
			return nil, err
		}
	}
	return v, nil
}

// ResolveArguments returns a copy of t whose public identifiers are resolved
// in s, checked by their rules, and stored as literal data (paths cleaned, IDs
// as integers). Private values (content, env, show, set_vars, with) stay
// references for the resource to resolve in its own scope, so the task's trace
// sees any secret they read. A Resolved task is returned as it is: its
// identifiers are never resolved again.
func ResolveArguments(t Task, s value.Scope) (Task, error) {
	if t.Resolved {
		return t, nil
	}
	t.copyModuleInputs()
	// Whole-value references come first, so that the module's own rules
	// below see the values they stand for.
	for _, arg := range taskArguments(t) {
		target, ok := arg.target.(*wholeValueTarget)
		if !ok {
			continue
		}
		v, err := resolveArgument(t, arg, s)
		if err != nil {
			return t, err
		}
		if err := setWholeValue(t.ModuleInputs, target.ref, v); err != nil {
			return t, argumentError(t, arg.field, arg.raw, s, err)
		}
	}
	// A field a whole value filled holds data, which is substituted once and
	// never read again as references: its own rules check it as it is.
	filled := t
	t.WholeValueReferences = nil
	name, _ := t.ModuleCall()
	for _, arg := range taskArguments(t) {
		if arg.target == nil {
			continue
		}
		if underWholeValue(filled, strings.TrimPrefix(arg.field, name+".")) {
			if arg.rule != nil {
				if err := arg.rule(arg.raw); err != nil {
					return t, argumentError(t, arg.field, arg.raw, s, err)
				}
			}
			continue
		}
		v, err := resolveArgument(t, arg, s)
		if err != nil {
			return t, err
		}
		switch p := arg.target.(type) {
		case *string:
			*p, _ = value.Scalar(v) // every public string rule requires a scalar
		case *modules.Account:
			text, _ := value.Scalar(v)
			*p = modules.Account(text)
		case *any:
			id, _ := modules.AccountID(v)
			*p = int64(id)
		}
	}
	if _, inputs := t.ModuleCall(); inputs != nil {
		if paths, ok := inputs.(interface{ CleanPaths() }); ok {
			paths.CleanPaths()
		}
	}
	// After the loop above, so that an error names the rule as written.
	if err := expandInventoryGroups(&t, s, filled); err != nil {
		return t, err
	}
	if err := selectSources(&t); err != nil {
		return t, err
	}
	t.Resolved = true
	return t, nil
}

// Literal argument display includes schema settings and scripts as written;
// reference-bearing fields above replace these with their effective values.
func authoredArguments(t Task) []Argument {
	name, inputs := t.ModuleCall()
	if inputs == nil {
		return nil
	}
	written := modules.WrittenArguments(inputs)
	if own, ok := inputs.(interface {
		WrittenArguments() []modules.WrittenArgument
	}); ok {
		written = own.WrittenArguments()
	}
	var out []Argument
	for _, w := range written {
		field := name + "." + w.Field
		out = append(out, Argument{Field: field, Value: w.Value, Status: "literal", Source: t.Sources.At(field)})
	}
	return out
}

func referenceSecrets(s value.Scope, raw any) []string {
	names := SecretNames(raw)
	inherited, _ := s[secretOriginsKey].(map[string][]string)
	for _, ref := range References(raw) {
		for prefix, list := range inherited {
			if ref == prefix || strings.HasPrefix(ref, prefix+".") || strings.HasPrefix(ref, prefix+"[") {
				names = append(names, list...)
			}
		}
	}
	return names
}

// AssignVars applies one set_vars task's values together, for inspection and
// the engine alike. Each variable's origin becomes its assignment, so a later
// error names the set_vars field that supplied the value.
func AssignVars(s value.Scope, t Task, values map[string]any) {
	for key, v := range values {
		s["vars"].(map[string]any)[key] = v //nolint:nilaway // Scope construction allocates vars; assigning a nil variable value does not dereference it.
		s.Clear(value.Pointer("vars", key))
		SetOrigin(s, ".vars."+key, t.Sources.At("set_vars."+key))
	}
}

func SetOrigin(s value.Scope, ref string, at Location) {
	origins := Origins{}
	for k, v := range ScopeOrigins(s) {
		if k != ref && !strings.HasPrefix(k, ref+".") && !strings.HasPrefix(k, ref+"[") {
			origins[k] = v
		}
	}
	origins[ref] = at
	SetOrigins(s, origins)
}

// copyModuleInputs gives t a deep copy of its module's inputs, so resolving
// them never changes the definition t came from.
func (t *Task) copyModuleInputs() {
	if t.ModuleInputs != nil {
		t.ModuleInputs = modules.CopyInputs(t.ModuleInputs)
	}
}
