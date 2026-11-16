package modules

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Every module that is not core has a module description: its inputs, its
// outputs, and examples, in the file module_description.yml beside its
// script or its executable. The configuration loader checks every node task
// that calls the module against it before anything runs, and
// mmx describe-module prints it. docs/changing-mmx/design/modules.md owns the rules
// ("The module description").

// ModuleDescriptionFile is the name of a module's description file, and
// moduleDescriptionLimit the most bytes it holds.
const (
	ModuleDescriptionFile  = "module_description.yml"
	moduleDescriptionLimit = 256 * 1024
)

// ModuleDescription says what a module does and what a node task can write
// for it.
type ModuleDescription struct {
	Summary  string          `json:"summary"`
	Inputs   []ModuleInput   `json:"inputs,omitempty"`
	Outputs  []ModuleOutput  `json:"outputs,omitempty"`
	Examples []ModuleExample `json:"examples"`
}

// ModuleInput is one input of a module.
type ModuleInput struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Required bool   `json:"required"`
	// AllowedValues are the values the input may have, when it may not have
	// every value of its type.
	AllowedValues []any `json:"allowed_values,omitempty"`
	// PrivateValue marks an input whose value is private, such as a password
	// that the module hands only to a program through its standard input. A
	// secret in it does not hide the task's result: MMX shows [redacted]
	// wherever a text of the result holds the value.
	PrivateValue bool   `json:"private_value,omitempty"`
	Description  string `json:"description"`
}

// ModuleOutput is one named output, which later node tasks read as
// .node_tasks.<id>.<name>.
type ModuleOutput struct {
	Name        string `json:"name"`
	Type        string `json:"type"`
	Description string `json:"description"`
}

// ModuleExample is one example call: what it shows, and its YAML.
type ModuleExample struct {
	Description string `json:"description"`
	Config      string `json:"config"`
}

// ModuleValueTypes are the types an input or an output can have. any takes
// lists and mappings of any shape, which the module checks itself.
var ModuleValueTypes = []string{"string", "integer", "number", "boolean", "list_of_strings", "mapping_of_strings", "any"}

// reservedOutputNames are result fields that every result or a role call
// already has, so no output can take their names.
var reservedOutputNames = []string{"status", "change", "reason", "items", "outputs"}

// ReadModuleDescription reads and checks the description file of the module
// named module in directory. Every error names the file, and the line where
// the file has one.
func ReadModuleDescription(directory, module string) (*ModuleDescription, error) {
	path := filepath.Join(directory, ModuleDescriptionFile)
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, moduleDescriptionLimit+1))
	if err != nil {
		return nil, err
	}
	if len(b) > moduleDescriptionLimit {
		return nil, fmt.Errorf("%s holds more than 256 KiB", path)
	}
	d, err := ParseModuleDescription(b, module)
	if err != nil {
		return nil, fmt.Errorf("%s:%w", path, err)
	}
	return d, nil
}

// ParseModuleDescription reads a description from the text of its file and
// checks its examples. An error starts with the line it is about.
func ParseModuleDescription(text []byte, module string) (*ModuleDescription, error) {
	var document yaml.Node
	if err := yaml.Unmarshal(text, &document); err != nil {
		return nil, fmt.Errorf(" %w", err)
	}
	if len(document.Content) != 1 {
		return nil, fmt.Errorf(" expected a mapping in module_description.yml")
	}
	d, err := parseModuleDescription(document.Content[0])
	if err == nil {
		err = d.checkExamples(module)
	}
	return d, err
}

func descriptionError(n *yaml.Node, format string, values ...any) error {
	return fmt.Errorf("%d: %s", n.Line, fmt.Sprintf(format, values...)) //nolint:nilaway // Name lookups use keys inserted with parsed YAML nodes; type lookups follow successful descriptionText checks.
}

// descriptionEntries returns the entries of a mapping in the order written.
// It refuses a node that is no mapping, a key written twice, and a key that is
// not among known.
func descriptionEntries(n *yaml.Node, what string, known ...string) ([]string, map[string]*yaml.Node, error) {
	if n.Kind != yaml.MappingNode {
		return nil, nil, descriptionError(n, "%s: expected a mapping", what)
	}
	keys, values := []string{}, map[string]*yaml.Node{}
	for i := 0; i+1 < len(n.Content); i += 2 {
		key, value := n.Content[i], n.Content[i+1]
		if key.Tag != "!!str" || values[key.Value] != nil {
			return nil, nil, descriptionError(key, "%s: expected each key once, as a plain string: %q", what, key.Value)
		}
		if len(known) > 0 && !slices.Contains(known, key.Value) {
			return nil, nil, descriptionError(key, "%s: unknown field %q; the fields are %s", what, key.Value, strings.Join(known, ", "))
		}
		keys, values[key.Value] = append(keys, key.Value), value
	}
	return keys, values, nil
}

// descriptionText returns the text of a required field: one string that is
// not empty.
func descriptionText(parent *yaml.Node, values map[string]*yaml.Node, what, field string) (string, error) {
	n := values[field]
	if n == nil {
		return "", descriptionError(parent, "%s: %s is required", what, field)
	}
	if n.Kind != yaml.ScalarNode || n.Tag != "!!str" || strings.TrimSpace(n.Value) == "" {
		return "", descriptionError(n, "%s: %s: expected a nonempty string", what, field)
	}
	return strings.TrimSpace(n.Value), nil
}

func parseModuleDescription(n *yaml.Node) (*ModuleDescription, error) {
	_, top, err := descriptionEntries(n, "the description", "summary", "inputs", "outputs", "examples")
	if err != nil {
		return nil, err
	}
	d := &ModuleDescription{}
	if d.Summary, err = descriptionText(n, top, "the description", "summary"); err != nil {
		return nil, err
	}
	if inputs := top["inputs"]; inputs != nil {
		names, entries, err := descriptionEntries(inputs, "inputs")
		if err != nil {
			return nil, err
		}
		for _, name := range names {
			input, err := parseModuleInput(name, entries[name])
			if err != nil {
				return nil, err
			}
			d.Inputs = append(d.Inputs, input)
		}
	}
	if outputs := top["outputs"]; outputs != nil {
		names, entries, err := descriptionEntries(outputs, "outputs")
		if err != nil {
			return nil, err
		}
		for _, name := range names {
			what := "outputs." + name
			if !ModuleName.MatchString(name) {
				return nil, descriptionError(entries[name], "%s: the name of an output uses lower-case letters, digits, and underscores, and starts with a letter", what)
			}
			if slices.Contains(reservedOutputNames, name) {
				return nil, descriptionError(entries[name], "%s: every result already has %s, so no output can take that name", what, name)
			}
			_, fields, err := descriptionEntries(entries[name], what, "type", "description")
			if err != nil {
				return nil, err
			}
			output := ModuleOutput{Name: name}
			if output.Type, err = descriptionText(entries[name], fields, what, "type"); err != nil {
				return nil, err
			}
			if !slices.Contains(ModuleValueTypes, output.Type) {
				return nil, descriptionError(fields["type"], "%s: type: expected one of %s", what, strings.Join(ModuleValueTypes, ", "))
			}
			if output.Description, err = descriptionText(entries[name], fields, what, "description"); err != nil {
				return nil, err
			}
			d.Outputs = append(d.Outputs, output)
		}
	}
	examples := top["examples"]
	if examples == nil || examples.Kind != yaml.SequenceNode || len(examples.Content) == 0 {
		return nil, descriptionError(n, "examples is required: a list of at least one example, each with a description and a config")
	}
	for i, example := range examples.Content {
		what := fmt.Sprintf("examples.%d", i)
		_, fields, err := descriptionEntries(example, what, "description", "config")
		if err != nil {
			return nil, err
		}
		parsed := ModuleExample{}
		if parsed.Description, err = descriptionText(example, fields, what, "description"); err != nil {
			return nil, err
		}
		if _, err = descriptionText(example, fields, what, "config"); err != nil {
			return nil, err
		}
		parsed.Config = fields["config"].Value //nolint:nilaway // Successful descriptionText for config guarantees that the map entry exists and is non-nil.
		d.Examples = append(d.Examples, parsed)
	}
	return d, nil
}

func parseModuleInput(name string, n *yaml.Node) (ModuleInput, error) {
	what := "inputs." + name
	input := ModuleInput{Name: name}
	if !ModuleName.MatchString(name) {
		return input, descriptionError(n, "%s: the name of an input uses lower-case letters, digits, and underscores, and starts with a letter", what)
	}
	_, fields, err := descriptionEntries(n, what, "type", "required", "allowed_values", "private_value", "description")
	if err != nil {
		return input, err
	}
	if input.Type, err = descriptionText(n, fields, what, "type"); err != nil {
		return input, err
	}
	if !slices.Contains(ModuleValueTypes, input.Type) {
		return input, descriptionError(fields["type"], "%s: type: expected one of %s", what, strings.Join(ModuleValueTypes, ", "))
	}
	required := fields["required"]
	if required == nil {
		return input, descriptionError(n, "%s: the required field must be set: true if every module call must supply the input, false if the input can be omitted", what)
	}
	if required.Kind != yaml.ScalarNode || required.Tag != "!!bool" || (required.Value != "true" && required.Value != "false") {
		return input, descriptionError(required, "%s: required: expected true or false", what)
	}
	input.Required = required.Value == "true"
	if input.Description, err = descriptionText(n, fields, what, "description"); err != nil {
		return input, err
	}
	if private := fields["private_value"]; private != nil {
		if private.Kind != yaml.ScalarNode || private.Tag != "!!bool" || (private.Value != "true" && private.Value != "false") {
			return input, descriptionError(private, "%s: private_value: expected true or false", what)
		}
		if input.PrivateValue = private.Value == "true"; input.PrivateValue && input.Type != "string" {
			return input, descriptionError(private, "%s: private_value: only an input of the type string can be a private value", what)
		}
	}
	if allowed := fields["allowed_values"]; allowed != nil {
		if input.Type != "string" && input.Type != "integer" {
			return input, descriptionError(allowed, "%s: allowed_values: only an input of the type string or integer takes a list of allowed values", what)
		}
		var values []any
		if allowed.Kind != yaml.SequenceNode || allowed.Decode(&values) != nil || len(values) == 0 {
			return input, descriptionError(allowed, "%s: allowed_values: expected a list of at least one value", what)
		}
		for _, v := range values {
			if problem := ValueTypeProblem(input.Type, v); problem != "" {
				return input, descriptionError(allowed, "%s: allowed_values: %s", what, problem)
			}
		}
		input.AllowedValues = values
	}
	return input, nil
}

// checkExamples checks each example against the description's own inputs:
// the example is the YAML of one call that names the module.
func (d *ModuleDescription) checkExamples(module string) error {
	for i, example := range d.Examples {
		what := fmt.Sprintf("examples.%d.config", i)
		var config map[string]any
		if err := yaml.Unmarshal([]byte(example.Config), &config); err != nil || config == nil {
			return fmt.Errorf(" %s: expected the YAML of one node task that names the module %s", what, module)
		}
		stated, names := config[module]
		if !names {
			return fmt.Errorf(" %s: the example does not name the module %s", what, module)
		}
		inputs, _ := stated.(map[string]any)
		if stated != nil && inputs == nil {
			return fmt.Errorf(" %s: %s: expected a mapping of the module's inputs", what, module)
		}
		if err := d.InputsProblem(module, inputs); err != nil {
			return fmt.Errorf(" %s: %s.%s: %s", what, module, err.Field, err.Err)
		}
	}
	return nil
}

// InputsProblem checks a call's inputs against the description: every input
// is one the module has, every required input is written, and every value
// has the type of its input and is one of its allowed values. A value that
// holds a reference ({{ ... }}) is checked once it resolves, on the node.
func (d *ModuleDescription) InputsProblem(module string, inputs map[string]any) *FieldError {
	names := make([]string, 0, len(inputs))
	for name := range inputs {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		input, exists := d.input(name)
		if !exists {
			has := "the module " + module + " takes no input"
			if len(d.Inputs) > 0 {
				has = "the inputs of the module " + module + " are " + strings.Join(d.inputNames(), ", ")
			}
			return &FieldError{name, fmt.Errorf("unknown input %q: %s", name, has)}
		}
		v := inputs[name]
		if Dynamic(v) {
			continue
		}
		if problem := ValueTypeProblem(input.Type, v); problem != "" {
			return &FieldError{name, fmt.Errorf("%s", problem)}
		}
		if len(input.AllowedValues) > 0 && !slices.ContainsFunc(input.AllowedValues, func(allowed any) bool { return sameValue(allowed, v) }) {
			var allowed []string
			for _, a := range input.AllowedValues {
				allowed = append(allowed, fmt.Sprint(a))
			}
			return &FieldError{name, fmt.Errorf("expected one of %s", strings.Join(allowed, ", "))}
		}
	}
	for _, input := range d.Inputs {
		if _, written := inputs[input.Name]; input.Required && !written {
			// The location of an input that is not written is the
			// module's, so the text names the input.
			return &FieldError{input.Name, fmt.Errorf("the input %s is required: %s", input.Name, input.Description)}
		}
	}
	return nil
}

// OutputsProblem checks a result's outputs against the description: every
// output is listed, with its type.
func (d *ModuleDescription) OutputsProblem(module string, outputs map[string]any) error {
	names := make([]string, 0, len(outputs))
	for name := range outputs {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		output, listed := d.output(name)
		if !listed {
			return fmt.Errorf("the module %s returned the output %s, which its description does not list", module, name)
		}
		if problem := ValueTypeProblem(output.Type, outputs[name]); problem != "" {
			return fmt.Errorf("the output %s of the module %s: %s", name, module, problem)
		}
	}
	return nil
}

// PrivateValue reports whether the description marks the input with that
// name private_value. A module without a description has no such input.
func (d *ModuleDescription) PrivateValue(name string) bool {
	if d == nil {
		return false
	}
	input, _ := d.input(name)
	return input.PrivateValue
}

func (d *ModuleDescription) input(name string) (ModuleInput, bool) {
	for _, input := range d.Inputs {
		if input.Name == name {
			return input, true
		}
	}
	return ModuleInput{}, false
}

func (d *ModuleDescription) output(name string) (ModuleOutput, bool) {
	for _, output := range d.Outputs {
		if output.Name == name {
			return output, true
		}
	}
	return ModuleOutput{}, false
}

func (d *ModuleDescription) inputNames() []string {
	var names []string
	for _, input := range d.Inputs {
		names = append(names, input.Name)
	}
	return names
}

// ValueTypeProblem says what is wrong with a value for a type, or "". The
// value is as YAML or JSON gives it.
func ValueTypeProblem(valueType string, v any) string {
	switch valueType {
	case "string":
		if _, ok := v.(string); !ok {
			return "expected a string"
		}
	case "integer":
		if _, ok := asWholeNumber(v); !ok {
			return "expected a whole number"
		}
	case "number":
		if _, ok := asNumber(v); !ok {
			return "expected a number"
		}
	case "boolean":
		if _, ok := v.(bool); !ok {
			return "expected true or false"
		}
	case "list_of_strings":
		list, ok := v.([]any)
		for _, item := range list {
			if _, text := item.(string); !text {
				ok = false
			}
		}
		if !ok {
			return "expected a list of strings"
		}
	case "mapping_of_strings":
		mapping, ok := v.(map[string]any)
		for _, item := range mapping {
			if _, text := item.(string); !text {
				ok = false
			}
		}
		if !ok {
			return "expected a mapping of strings"
		}
	}
	return ""
}

func asNumber(v any) (float64, bool) {
	switch n := v.(type) {
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case uint64:
		return float64(n), true
	case float64:
		return n, !math.IsNaN(n) && !math.IsInf(n, 0)
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	}
	return 0, false
}

func asWholeNumber(v any) (int64, bool) {
	switch n := v.(type) {
	case int:
		return int64(n), true
	case int64:
		return n, true
	case json.Number:
		i, err := n.Int64()
		return i, err == nil
	case float64:
		return int64(n), n == math.Trunc(n) && math.Abs(n) < 1<<53
	}
	return 0, false
}

func sameValue(allowed, written any) bool {
	if a, ok := asWholeNumber(allowed); ok {
		w, ok := asWholeNumber(written)
		return ok && a == w
	}
	return allowed == written
}
