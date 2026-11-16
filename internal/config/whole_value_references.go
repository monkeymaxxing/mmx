package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"sort"
	"strconv"
	"strings"

	"mmx/internal/modules"
	"mmx/internal/value"

	"go.yaml.in/yaml/v3"
)

// A whole-value reference keeps its type, so text that is exactly one
// reference may stand for a core module's input of any type: an integer, a
// boolean, a list, or a mapping. Loading takes such a reference out of the
// inputs and keeps it here; the field holds its zero value until the
// reference resolves. It resolves as one of the task's public arguments
// (taskArguments): validation checks it where the value is known locally,
// and the node resolves it before the module runs. The resolved value must
// fit the field's type.

// WholeValueReference is one module input that holds a whole-value reference
// in place of a value of the input's type.
type WholeValueReference struct {
	// Field names the input as errors name it, below the module's name:
	// priority_when_registered, keys, or ports[1].
	Field string `json:"field"`
	// Steps lead from the module's inputs to the field: a YAML key for a
	// mapping, a position for a list.
	Steps     []string `json:"steps"`
	Reference string   `json:"reference"`
}

// takeWholeValueReferences takes each whole-value reference that the module
// inputs' type would not accept as written out of n, leaving null in its
// place, and returns them. A reference in a text field stays where it is.
func takeWholeValueReferences(n *yaml.Node, t reflect.Type, field string, steps []string) []WholeValueReference {
	if n.Kind == yaml.ScalarNode && n.Tag == "!!str" && value.IsWholeReference(n.Value) {
		if checkTypes(n, t, field) == nil {
			return nil
		}
		ref := WholeValueReference{Field: field, Steps: append([]string(nil), steps...), Reference: n.Value}
		*n = zeroNode(t)
		return []WholeValueReference{ref}
	}
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t == reflect.TypeOf(Task{}) {
		return nil
	}
	var out []WholeValueReference
	switch {
	case n.Kind == yaml.MappingNode && t.Kind() == reflect.Struct:
		fields := yamlFields(t)
		for i := 0; i+1 < len(n.Content); i += 2 {
			key := n.Content[i].Value
			if ft, ok := fields[key]; ok {
				out = append(out, takeWholeValueReferences(n.Content[i+1], ft, fieldPath(field, key), append(steps, key))...)
			}
		}
	case n.Kind == yaml.MappingNode && t.Kind() == reflect.Map:
		for i := 0; i+1 < len(n.Content); i += 2 {
			key := n.Content[i].Value
			out = append(out, takeWholeValueReferences(n.Content[i+1], t.Elem(), fieldPath(field, key), append(steps, key))...)
		}
	case n.Kind == yaml.SequenceNode && t.Kind() == reflect.Slice:
		for i, c := range n.Content {
			out = append(out, takeWholeValueReferences(c, t.Elem(), fmt.Sprintf("%s[%d]", field, i), append(steps, strconv.Itoa(i)))...)
		}
	}
	return out
}

// zeroNode is the YAML of a type's zero value, which a field holds until its
// whole-value reference resolves. A list keeps the position of an item.
func zeroNode(t reflect.Type) yaml.Node {
	// An optional field, such as wait_for.tcp_port_accepts_connections, counts as written: the zero
	// of its type, not null, which would read as a field left out.
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.Struct, reflect.Map:
		return yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	case reflect.Slice:
		// One item, so that a module's check of which fields are written,
		// such as pip_package's packages, sees the list as written.
		// A rule on that item names the field and waits for the reference.
		item := zeroNode(t.Elem())
		return yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq", Content: []*yaml.Node{&item}}
	case reflect.String:
		return yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: ""}
	case reflect.Int, reflect.Int64, reflect.Int32, reflect.Uint, reflect.Uint32, reflect.Uint64:
		return yaml.Node{Kind: yaml.ScalarNode, Tag: "!!int", Value: "0"}
	case reflect.Bool:
		return yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: "false"}
	default:
		// Non-schema numeric, interface, and non-YAML kinds use the null placeholder below.
	}
	return yaml.Node{Kind: yaml.ScalarNode, Tag: "!!null", Value: "null"}
}

// copyNode copies a YAML node and everything below it.
func copyNode(n *yaml.Node) *yaml.Node {
	c := *n
	c.Content = make([]*yaml.Node, len(n.Content))
	for i, child := range n.Content {
		c.Content[i] = copyNode(child)
	}
	return &c
}

// wholeValueField finds the field that steps lead to in a module's inputs, and
// calls set with it. A field of a mapping is set back into the mapping.
func wholeValueField(v reflect.Value, steps []string, set func(reflect.Value) error) error {
	if len(steps) == 0 {
		return set(v)
	}
	for v.Kind() == reflect.Pointer || v.Kind() == reflect.Interface {
		if v.IsNil() {
			if v.Kind() == reflect.Interface {
				return fmt.Errorf("no field %s", steps[0])
			}
			v.Set(reflect.New(v.Type().Elem()))
		}
		v = v.Elem()
	}
	switch v.Kind() {
	case reflect.Struct:
		// A written form's YAML name, or the name of the same input in a
		// module's input type that only JSON reads, such as a package's names.
		for _, tag := range []string{"yaml", "json"} {
			for i := 0; i < v.NumField(); i++ {
				if name := strings.Split(v.Type().Field(i).Tag.Get(tag), ",")[0]; name == steps[0] {
					return wholeValueField(v.Field(i), steps[1:], set)
				}
			}
		}
	case reflect.Map:
		key := reflect.ValueOf(steps[0]).Convert(v.Type().Key())
		elem := reflect.New(v.Type().Elem()).Elem()
		if existing := v.MapIndex(key); existing.IsValid() {
			elem.Set(existing)
		}
		if err := wholeValueField(elem, steps[1:], set); err != nil {
			return err
		}
		if v.IsNil() {
			v.Set(reflect.MakeMap(v.Type()))
		}
		v.SetMapIndex(key, elem)
		return nil
	case reflect.Slice:
		if i, err := strconv.Atoi(steps[0]); err == nil && i < v.Len() {
			return wholeValueField(v.Index(i), steps[1:], set)
		}
	default:
		// Scalars and non-YAML kinds have no nested fields and reach the existing error below.
	}
	return fmt.Errorf("no field %s", steps[0])
}

// wholeValueType is the type of the field a whole-value reference stands for.
func wholeValueType(inputs any, ref WholeValueReference) (reflect.Type, error) {
	var t reflect.Type
	err := wholeValueField(reflect.ValueOf(modules.CopyInputs(inputs)), ref.Steps, func(v reflect.Value) error {
		t = v.Type()
		return nil
	})
	return t, err
}

// checkWholeValueReferences checks, as the project loads, that each
// whole-value reference stands for a field of the module's inputs, also after
// the written form became the module's input type.
func checkWholeValueReferences(t Task) error {
	name, inputs := t.ModuleCall()
	for _, ref := range t.WholeValueReferences {
		if _, err := wholeValueType(inputs, ref); err != nil {
			return t.Sources.Error(name+"."+ref.Field, fmt.Errorf("%s does not accept a whole-value reference; write the value directly in the node task", ref.Field))
		}
		if namesControllerMachineFiles(reflect.TypeOf(inputs), ref.Steps) {
			return t.Sources.Error(name+"."+ref.Field, fmt.Errorf("%s names files read on the controller machine while loading the project, before a whole-value reference can be resolved for a node; write the list directly in the node task, or use a separate reference for each list item, such as [\"{{ .vars.admin_key_file }}\"]", ref.Field))
		}
	}
	return nil
}

// namesControllerMachineFiles reports a field that is, or holds, an input
// tagged mmx:"controller_machine_files".
func namesControllerMachineFiles(t reflect.Type, steps []string) bool {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if len(steps) == 0 {
		return holdsControllerMachineFiles(t, map[reflect.Type]bool{})
	}
	switch t.Kind() {
	case reflect.Struct:
		for _, tag := range []string{"yaml", "json"} {
			for i := 0; i < t.NumField(); i++ {
				f := t.Field(i)
				if strings.Split(f.Tag.Get(tag), ",")[0] == steps[0] {
					return f.Tag.Get("mmx") == modules.ControllerMachineFiles || namesControllerMachineFiles(f.Type, steps[1:])
				}
			}
		}
	case reflect.Map, reflect.Slice:
		return namesControllerMachineFiles(t.Elem(), steps[1:])
	default:
		// Scalars and non-YAML kinds have no nested controller-machine file fields.
	}
	return false
}

func holdsControllerMachineFiles(t reflect.Type, seen map[reflect.Type]bool) bool {
	for t.Kind() == reflect.Pointer || t.Kind() == reflect.Slice || t.Kind() == reflect.Map {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct || seen[t] {
		return false
	}
	seen[t] = true
	for i := 0; i < t.NumField(); i++ {
		if t.Field(i).Tag.Get("mmx") == modules.ControllerMachineFiles || holdsControllerMachineFiles(t.Field(i).Type, seen) {
			return true
		}
	}
	return false
}

// waitsForWholeValue reports an error of the module's own checks that names a
// field a whole-value reference stands for: the field holds its zero value as
// the project loads, so the check is made when the reference resolves.
func waitsForWholeValue(t Task, err error) bool {
	var field *modules.FieldError
	return errors.As(err, &field) && underWholeValue(t, field.Field)
}

// underWholeValue reports a module input, named below the module's name, that
// a whole-value reference stands for or contains.
func underWholeValue(t Task, field string) bool {
	for _, ref := range t.WholeValueReferences {
		if field == ref.Field || strings.HasPrefix(field, ref.Field+".") || strings.HasPrefix(field, ref.Field+"[") {
			return true
		}
	}
	return false
}

// checkModuleWithWholeValues makes the module's own checks, which loading left
// to this point, with the values that the task's whole-value references
// stand for, when validation knows them all on the controller machine. A
// value known only on the node is checked against the field's type there.
func checkModuleWithWholeValues(t Task, s value.Scope) error {
	if len(t.WholeValueReferences) == 0 {
		return nil
	}
	resolved, err := ResolveArguments(t, s)
	if unknown(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := prepareModule(&resolved, t.ModuleBase); err != nil {
		action, _ := t.Action()
		return t.Sources.Error(action.Name, err)
	}
	// A whole value can supply a nested command with its own references.
	// Validate it through the ordinary shell argument and policy checks.
	local := s.Local()
	local.Mark("/new_file", "new_file.path resolved on the node")
	commandDeclarations := declarations{}
	for _, root := range mmxRoots {
		commandDeclarations[root] = nil
	}
	for root := range s {
		commandDeclarations[root] = nil // includes names supplied by a loop
	}
	commandDeclarations["new_file"] = map[string]bool{"path": true}
	name, reads := moduleReads(resolved)
	for _, field := range sortedKeys(reads.Commands) {
		for _, ref := range t.WholeValueReferences {
			if field != ref.Field && !strings.HasPrefix(field, ref.Field+".") && !strings.HasPrefix(ref.Field, field+".") {
				continue
			}
			command := reads.Commands[field]
			check := Task{Module: "shell", ModuleInputs: command, Sources: t.Sources.Sub(name + "." + strings.TrimSuffix(field, ".shell"))}
			err := checkShell(command, commandDeclarations)
			if err == nil {
				_, err = InspectArguments(check, local, true)
			}
			if err == nil {
				err = inspectPolicies(check, local)
			}
			if err != nil {
				return SourceError(t.Sources, fieldPath(name, field), ref.Reference, s, fmt.Errorf("The command supplied by %s is invalid: %w. Correct the command value before loading the project again.", ref.Reference, err))
			}
			scripts := command.Reads().ShellScripts
			for _, script := range sortedKeys(scripts) {
				if err := checkLiteralScript(fieldPath(name, fieldPath(field, script)), scripts[script], commandDeclarations, true); err != nil {
					return SourceError(t.Sources, fieldPath(name, field), ref.Reference, s, fmt.Errorf("%w. The shell script comes from %s. Correct the script value before loading the project again.", err, ref.Reference))
				}
			}
			break
		}
	}
	return nil
}

// wholeValueArguments lists a task's whole-value references as public
// arguments whose rule is the field's type.
func wholeValueArguments(t Task, name string, inputs any) []taskArgument {
	refs := append([]WholeValueReference(nil), t.WholeValueReferences...)
	sort.Slice(refs, func(i, j int) bool { return refs[i].Field < refs[j].Field })
	var out []taskArgument
	for _, ref := range refs {
		fieldType, err := wholeValueType(inputs, ref)
		if err != nil {
			continue // checkWholeValueReferences reports it
		}
		out = append(out, taskArgument{field: name + "." + ref.Field, raw: ref.Reference, target: &wholeValueTarget{ref}, rule: func(v any) error {
			_, err := wholeValueNode(v, fieldType, ref)
			return err
		}})
	}
	return out
}

// wholeValueTarget is where ResolveArguments puts a whole-value reference's
// value: the field of the module's inputs it stands for.
type wholeValueTarget struct{ ref WholeValueReference }

// wholeValueNode turns a resolved value into YAML and checks it against the
// field's type with the loader's own rules.
func wholeValueNode(v any, fieldType reflect.Type, ref WholeValueReference) (*yaml.Node, error) {
	v = wholeNumbers(value.Plain(v))
	var n yaml.Node
	err := n.Encode(v)
	if err == nil {
		err = checkTypes(&n, fieldType, ref.Field)
	}
	if err != nil {
		return nil, fmt.Errorf("%s resolved to %s, but the module input requires %s", ref.Reference, describeWholeValue(v), typeInWords(fieldType))
	}
	return &n, nil
}

// wholeNumbers turns a number without a fraction, such as a fact read from
// JSON, into an integer.
func wholeNumbers(v any) any {
	switch v := v.(type) {
	case json.Number:
		// The node reads its job's JSON with UseNumber.
		if i, err := v.Int64(); err == nil {
			return i
		}
		if f, err := v.Float64(); err == nil {
			return f
		}
	case float64:
		if v == math.Trunc(v) && math.Abs(v) < 1<<53 {
			return int64(v)
		}
	case []any:
		out := make([]any, len(v))
		for i, x := range v {
			out[i] = wholeNumbers(x)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(v))
		for k, x := range v {
			out[k] = wholeNumbers(x)
		}
		return out
	}
	return v
}

func describeWholeValue(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case string:
		return fmt.Sprintf("the text %q", v)
	case bool:
		return fmt.Sprintf("the boolean %v", v)
	case int, int64, float64:
		return fmt.Sprintf("the number %v", v)
	case []any:
		return "a list"
	case map[string]any:
		return "a mapping"
	}
	return fmt.Sprintf("%v", v)
}

func typeInWords(t reflect.Type) string {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.Int, reflect.Int64, reflect.Int32, reflect.Uint, reflect.Uint32, reflect.Uint64:
		return "a whole number"
	case reflect.Bool:
		return "true or false"
	case reflect.Slice:
		return "a list"
	case reflect.Map, reflect.Struct:
		return "a mapping"
	case reflect.String:
		return "text"
	default:
		// Other numeric, interface, and non-YAML kinds use their Go type name below.
	}
	return t.String()
}

// setWholeValue puts a resolved whole-value reference into the module's
// inputs.
func setWholeValue(inputs any, ref WholeValueReference, v any) error {
	return wholeValueField(reflect.ValueOf(inputs), ref.Steps, func(field reflect.Value) error {
		n, err := wholeValueNode(v, field.Type(), ref)
		if err != nil {
			return err
		}
		target := reflect.New(field.Type())
		if err := n.Decode(target.Interface()); err != nil {
			return err
		}
		field.Set(target.Elem())
		return nil
	})
}
