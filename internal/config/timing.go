package config

import (
	"encoding/json"
	"fmt"
	"mmx/internal/modules"
	"mmx/internal/protocol"
	"reflect"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// UnmarshalYAML reads a node task: its settings, and the one module it names
// with the module's inputs below the name. A name that is no core module is a
// module of the project's own, which loading looks up in external_modules/.
func (t *Task) UnmarshalYAML(n *yaml.Node) error {
	type plain Task
	if err := n.Decode((*plain)(t)); err != nil {
		return err
	}
	settings := yamlFields(reflect.TypeOf(Task{}))
	for i := 0; i < len(n.Content); i += 2 {
		key, value := n.Content[i].Value, n.Content[i+1]
		switch key {
		case "time_limit":
			t.TimeLimitSet = true
		case "time_limit_per_try":
			t.TimeLimitPerTrySet = true
		}
		if _, setting := settings[key]; setting {
			continue
		}
		if t.Module != "" {
			return fmt.Errorf("line %d: a node task must name exactly one module, but the node task names %s and %s", n.Content[i].Line, t.Module, key)
		}
		t.Module = key
		if m, core := modules.LookUpCoreModule(key); core {
			inputs := m.NewWritten()
			written := copyNode(value)
			t.WholeValueReferences = takeWholeValueReferences(written, reflect.TypeOf(inputs), "", nil)
			if err := written.Decode(inputs); err != nil {
				return err
			}
			t.ModuleInputs = inputs
			continue
		}
		inputs := map[string]any{}
		if err := value.Decode(&inputs); err != nil {
			return err
		}
		t.ModuleInputs = inputs
	}
	return nil
}

// MarshalJSON writes the module's inputs beside the task's settings.
func (t Task) MarshalJSON() ([]byte, error) {
	type plain Task
	return json.Marshal(struct {
		plain
		ModuleInputs any `json:"module_inputs,omitempty"`
	}{plain(t), t.ModuleInputs})
}

// UnmarshalJSON reads a saved task strictly, with its module's inputs in the
// module's input type.
func (t *Task) UnmarshalJSON(b []byte) error {
	type plain Task
	var saved struct {
		plain
		ModuleInputs json.RawMessage `json:"module_inputs,omitempty"`
	}
	if err := protocol.Decode(b, &saved); err != nil {
		return err
	}
	*t = Task(saved.plain)
	if t.Module == "" {
		return nil
	}
	if m, core := modules.LookUpCoreModule(t.Module); core {
		inputs := m.NewInputs()
		if err := protocol.Decode(saved.ModuleInputs, inputs); err != nil {
			return fmt.Errorf("the inputs of %s: %w", t.Module, err)
		}
		t.ModuleInputs = inputs
		return nil
	}
	inputs := map[string]any{}
	if len(saved.ModuleInputs) > 0 {
		if err := protocol.Decode(saved.ModuleInputs, &inputs); err != nil {
			return fmt.Errorf("the inputs of %s: %w", t.Module, err)
		}
	}
	t.ModuleInputs = inputs
	return nil
}

func (r *Retry) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode && n.Tag == "!!bool" {
		var enabled bool
		if err := n.Decode(&enabled); err != nil {
			return err
		}
		r.Disabled = !enabled
		return nil
	}
	type plain Retry
	if err := n.Decode((*plain)(r)); err != nil {
		return err
	}
	for i := 0; i < len(n.Content); i += 2 {
		switch n.Content[i].Value {
		case "max_tries":
			r.MaxSet = true
		case "pause_between_tries":
			r.PauseSet = true
		}
	}
	return nil
}
func resolveTiming(t *Task, d Defaults) error {
	if t.Sources == nil {
		t.Sources = Sources{}
	}
	// Project defaults do not apply to a reboot: it runs once per attempt, and
	// its own timeout bounds the wait.
	name, _ := t.ModuleCall()
	module, _ := modules.LookUpCoreModule(name)
	if module.WaitsWithItsOwnLimit {
		action, _ := t.Action()
		if t.Retry != nil || t.TimeLimitSet || t.TimeLimitPerTrySet || t.TimeLimit != nil || t.TimeLimitPerTry != nil {
			return fmt.Errorf("%s does not accept retry, time_limit, or time_limit_per_try; %s", action.Name, module.Description.WaitLimit)
		}
		return nil
	}
	inherit := func(field string, sources Sources, sourceField string) {
		at, ok := sources[sourceField]
		if !ok {
			at = Location{Field: "MMX default"}
		}
		t.Sources[field] = at
	}
	if !t.TimeLimitSet {
		inherit("time_limit", d.Sources, "time_limit")
	}
	if !t.TimeLimitPerTrySet {
		inherit("time_limit_per_try", d.Sources, "time_limit_per_try")
	}
	if t.Retry == nil {
		t.Sources["retry.enabled"] = Location{Field: "module default"}
	} else {
		t.Sources["retry.enabled"] = t.Sources.At("retry")
	}
	action, err := t.Action()
	if err != nil {
		return err
	}
	if !t.TimeLimitSet {
		t.TimeLimit = d.TimeLimit
	}
	if !t.TimeLimitPerTrySet {
		t.TimeLimitPerTry = d.TimeLimitPerTry
	}
	for _, field := range []struct {
		name  string
		limit *string
	}{{"time_limit", t.TimeLimit}, {"time_limit_per_try", t.TimeLimitPerTry}} {
		if limit := field.limit; limit != nil && !strings.Contains(*limit, "{{") {
			if err := positiveTimeLimit(field.name)(*limit); err != nil {
				return err
			}
		}
	}
	if t.Retry == nil && action.RetryByDefault {
		t.Retry = &Retry{}
	}
	if t.Retry != nil && (t.TaskBlock != nil || t.Role != "") {
		return fmt.Errorf("retry must be set on an individual node task inside the role or node task block")
	}
	if t.Retry != nil && module.EvaluatesOnce {
		return fmt.Errorf("%s evaluates once and rejects retry; retry the task that observes the value", module.Name)
	}
	if t.Retry == nil || t.Retry.Disabled {
		return nil
	}
	r := t.Retry
	if !r.MaxSet {
		inherit("retry.max_tries", d.Retry.Sources, "max_tries")
		r.MaxTries = d.Retry.MaxTries
	}
	if !r.PauseSet {
		inherit("retry.pause_between_tries", d.Retry.Sources, "pause_between_tries")
		r.PauseBetweenTries = d.Retry.PauseBetweenTries
	}
	if r.MaxTries != 0 && r.MaxTries < 1 {
		return modules.Invalid("retry.max_tries", "must be a positive integer or unlimited")
	}
	if r.MaxTries == 0 && t.TimeLimit == nil {
		return fmt.Errorf("retry.max_tries: unlimited requires time_limit")
	}
	if !strings.Contains(r.PauseBetweenTries, "{{") {
		return pauseBetweenTries(r.PauseBetweenTries)
	}
	return nil
}

// positiveTimeLimit is the check of time_limit and time_limit_per_try.
func positiveTimeLimit(field string) func(string) error {
	return func(text string) error {
		if v, err := time.ParseDuration(text); err != nil || v <= 0 {
			return modules.Invalid(field, "time limit must be a positive duration or null")
		}
		return nil
	}
}

// pauseBetweenTries is the check of retry.pause_between_tries.
func pauseBetweenTries(text string) error {
	if v, err := time.ParseDuration(text); err != nil || v < 0 {
		return modules.Invalid("retry.pause_between_tries", "must be a nonnegative duration")
	}
	return nil
}

// TryLimit is a positive count, or zero for explicitly unlimited tries.
// The public YAML and JSON spelling of an unlimited count is "unlimited".
type TryLimit int

func (limit *TryLimit) UnmarshalYAML(n *yaml.Node) error {
	if n.Tag == "!!str" && n.Value == "unlimited" {
		*limit = 0
		return nil
	}
	var count int
	if n.Tag != "!!int" {
		return fmt.Errorf("max_tries must be a positive integer or unlimited")
	}
	if err := n.Decode(&count); err != nil {
		return err
	}
	if count < 1 {
		return fmt.Errorf("max_tries must be a positive integer or unlimited")
	}
	*limit = TryLimit(count)
	return nil
}
func (limit TryLimit) MarshalJSON() ([]byte, error) {
	if limit == 0 {
		return []byte(`"unlimited"`), nil
	}
	return json.Marshal(int(limit))
}
func (limit *TryLimit) UnmarshalJSON(data []byte) error {
	if string(data) == `"unlimited"` {
		*limit = 0
		return nil
	}
	var count int
	if err := json.Unmarshal(data, &count); err != nil {
		return err
	}
	if count < 1 {
		return fmt.Errorf("max_tries must be a positive integer or unlimited")
	}
	*limit = TryLimit(count)
	return nil
}
