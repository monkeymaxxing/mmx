package config

import (
	"errors"
	"fmt"
	"strings"

	"mmx/internal/modules"
	"mmx/internal/value"
)

// A duration setting may be written out, such as 15m, or be one whole
// reference to a var, such as "{{ .vars.reboot_wait_at_most }}". MMX puts each
// node's value in before the run (ResolveDurations), where the controller
// process can still check it: the controller process waits for a reboot,
// and the node sets a node task's time limits before the node task resolves
// anything. So such a reference reads only a var that MMX knows before the
// run, never an output, a fact, or a secret, and a var that set_vars sets
// during the run is refused as well.

// durationSettings calls visit for each duration setting of the node task t
// that is not empty: time_limit, time_limit_per_try,
// retry.pause_between_tries, and its module's duration inputs
// (modules.DurationInputs), each with the field name errors use and the
// check the duration must pass.
func durationSettings(t *Task, visit func(field string, text *string, check func(string) error)) {
	if t.TimeLimit != nil {
		visit("time_limit", t.TimeLimit, positiveTimeLimit("time_limit"))
	}
	if t.TimeLimitPerTry != nil {
		visit("time_limit_per_try", t.TimeLimitPerTry, positiveTimeLimit("time_limit_per_try"))
	}
	if t.Retry != nil && t.Retry.PauseBetweenTries != "" {
		visit("retry.pause_between_tries", &t.Retry.PauseBetweenTries, pauseBetweenTries)
	}
	if inputs, ok := t.ModuleInputs.(modules.DurationInputs); ok {
		inputs.DurationInputs(func(field string, text *string, check func(string) error) {
			if *text != "" {
				visit(t.Module+"."+field, text, check)
			}
		})
	}
}

// durationReferenceVar is the var that a duration written as one whole
// reference to a var reads, such as reboot_wait_at_most; ok is false for any
// other text that holds a reference.
func durationReferenceVar(text string) (name string, ok bool) {
	if !value.IsWholeReference(text) {
		return "", false
	}
	parts, err := value.Path(strings.TrimSpace(value.Reference.FindStringSubmatch(text)[1]))
	if err != nil || len(parts) < 2 || parts[0].Name != "vars" || parts[1].Name == "" || parts[1].Dynamic != "" {
		return "", false
	}
	return parts[1].Name, true
}

// checkDurationReferences checks, as the project loads, every duration
// setting of a fleet stage's node tasks that is written with a reference.
// assigned holds the vars that the fleet stage's set_vars may set outside
// roles. A node task in a role refuses such a reference: a role's vars are
// only those its own set_vars sets during the run.
func checkDurationReferences(nodeTasks []Task, assigned map[string]bool, inRole bool) error {
	for i := range nodeTasks {
		t := &nodeTasks[i]
		var err error
		durationSettings(t, func(field string, text *string, _ func(string) error) {
			if err != nil || !strings.Contains(*text, "{{") {
				return
			}
			name, ok := durationReferenceVar(*text)
			switch {
			case !ok:
				err = fmt.Errorf("%s must be a duration, such as 15m, or one whole reference to a var, such as \"{{ .vars.reboot_wait_at_most }}\"; MMX takes the duration before the run, when outputs, facts, and secrets are not known yet", field)
			case inRole:
				err = fmt.Errorf("%s in a role must be a duration written out, such as 15m: a role's vars are only those its set_vars sets during the run, and MMX takes the duration before the run", field)
			case assigned[name]:
				err = fmt.Errorf("%s reads the var %s, which a set_vars node task of this fleet stage sets during the run; MMX takes the duration before the run, so read a var that set_vars does not set", field, name)
			}
			if err != nil {
				err = t.Sources.Error(field, err)
			}
		})
		if err != nil {
			return err
		}
		for _, list := range t.Children() {
			if err := checkDurationReferences(list.NodeTasks, assigned, inRole || t.Role != ""); err != nil {
				return err
			}
		}
	}
	return nil
}

// varsSetBySetVars lists the vars that set_vars node tasks set outside roles.
func varsSetBySetVars(nodeTasks []Task, names map[string]bool) map[string]bool {
	for _, t := range nodeTasks {
		for name := range t.SetVars {
			names[name] = true
		}
		if t.Role != "" {
			continue
		}
		for _, list := range t.Children() {
			varsSetBySetVars(list.NodeTasks, names)
		}
	}
	return names
}

// DurationScope is the scope in which a node's duration references resolve:
// the node's vars, with where each one is set, for "value supplied at".
func DurationScope(p Project, inv Inventory, managedNode string) value.Scope {
	s := value.Scope{"vars": NodeVars(p, inv, managedNode)}
	SetOrigins(s, VariableOrigins(p, inv, managedNode))
	return s
}

// ResolveDurations returns nodeTasks with each duration setting that reads
// a var holding the var's value in scope, checked as that setting's
// duration; the error names the setting. A node task without such a
// reference is returned as it is, and one with a reference is a copy, so
// the playbook's node tasks keep their references for the next node.
func ResolveDurations(nodeTasks []Task, scope value.Scope) ([]Task, error) {
	out := append([]Task(nil), nodeTasks...)
	for i := range out {
		t := &out[i]
		if hasDurationReference(t) {
			if t.TimeLimit != nil {
				text := *t.TimeLimit
				t.TimeLimit = &text
			}
			if t.TimeLimitPerTry != nil {
				text := *t.TimeLimitPerTry
				t.TimeLimitPerTry = &text
			}
			if t.Retry != nil {
				retry := *t.Retry
				t.Retry = &retry
			}
			t.copyModuleInputs()
			var err error
			durationSettings(t, func(field string, text *string, check func(string) error) {
				if err != nil || !value.IsWholeReference(*text) {
					return
				}
				raw := *text
				err = resolveDuration(text, scope, check)
				if err != nil {
					err = SourceError(t.Sources, field, raw, scope, err)
				}
			})
			if err != nil {
				return nil, err
			}
		}
		if t.TaskBlock != nil {
			block := *t.TaskBlock
			for _, list := range []*[]Task{&block.Changes, &block.ChecksBeforeRestartOrReload, &block.Validate, &block.Activate, &block.Verify, &block.OnFailure, &block.Cleanup} {
				resolved, err := ResolveDurations(*list, scope)
				if err != nil {
					return nil, err
				}
				*list = resolved
			}
			t.TaskBlock = &block
		}
	}
	return out, nil
}

// resolveDuration puts the value of the var that text reads in its place,
// after check accepts it.
func resolveDuration(text *string, scope value.Scope, check func(string) error) error {
	v, err := scope.Resolve(*text)
	if err != nil {
		return err
	}
	if value.HasSecrets(v) {
		return fmt.Errorf("a duration must not hold a secret")
	}
	written, ok := v.(string)
	if !ok {
		return fmt.Errorf("the var holds %v, which is no duration; write it as text, such as 15m", value.Display(v))
	}
	if err := check(written); err != nil {
		// The check's own message, so that the error keeps where the var
		// is set (SourceError).
		var field *modules.FieldError
		if errors.As(err, &field) {
			return field.Err
		}
		return err
	}
	*text = written
	return nil
}

func hasDurationReference(t *Task) bool {
	found := false
	durationSettings(t, func(_ string, text *string, _ func(string) error) {
		found = found || value.IsWholeReference(*text)
	})
	return found
}
