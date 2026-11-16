package config

import (
	"fmt"

	"mmx/internal/modules"
)

// Action is a task's kind and its default retry policy: a core module from the
// registry, or one of the node task forms that are not modules.
type Action struct {
	Name           string
	RetryByDefault bool
}

// ModuleCall returns the core module a task calls and its inputs, or "" when
// the task calls no core module. Until loading prepares the task, a package
// or repository task, or a download, has its written form.
func (t Task) ModuleCall() (string, any) {
	if _, core := modules.LookUpCoreModule(t.Module); !core {
		return "", nil
	}
	return t.Module, t.ModuleInputs
}

// NonCoreModuleInputs returns the inputs of a module of the project's own,
// or nil when the task calls none.
func (t Task) NonCoreModuleInputs() map[string]any {
	if _, core := modules.LookUpCoreModule(t.Module); core {
		return nil
	}
	inputs, _ := t.ModuleInputs.(map[string]any)
	return inputs
}

// CallsModuleThatIsNotCore reports a task that calls a module of the project's own.
func (t Task) CallsModuleThatIsNotCore() bool {
	_, core := modules.LookUpCoreModule(t.Module)
	return t.Module != "" && !core
}

func (t Task) Action() (Action, error) {
	var selected Action
	count := 0
	if name, _ := t.ModuleCall(); name != "" {
		m, ok := modules.LookUpCoreModule(name)
		if !ok {
			return Action{}, fmt.Errorf("internal error: no core module %s in the registry", name)
		}
		selected = Action{m.Name, m.RetryByDefault}
		count++
	}
	for _, entry := range []struct {
		present bool
		action  Action
	}{
		{t.CallsModuleThatIsNotCore(), Action{t.Module, true}},
		{t.Show != nil, Action{"show", false}},
		{t.SetVars != nil, Action{"set_vars", false}},
		{t.Role != "", Action{"role", false}},
		{t.TaskBlock != nil, Action{"task_block", false}},
	} {
		if entry.present {
			selected = entry.action
			count++
		}
	}
	if count > 1 && t.CallsModuleThatIsNotCore() {
		return Action{}, fmt.Errorf("%s is not a node task setting; a module call cannot share a node task with role, task_block, show, or set_vars", t.Module)
	}
	if count != 1 {
		return Action{}, fmt.Errorf("a node task must specify exactly one action: a module call, role, task_block, show, or set_vars")
	}
	return selected, nil
}

// CoreModule is the registry entry of the core module a task calls; its Name
// is empty when the task calls none.
func (t Task) CoreModule() modules.CoreModule {
	name, _ := t.ModuleCall()
	m, _ := modules.LookUpCoreModule(name)
	return m
}

// InputsOf returns a task's module inputs as T, or nil when the task's
// module keeps its inputs in another type.
func InputsOf[T any](t Task) *T {
	inputs, _ := t.ModuleInputs.(*T)
	return inputs
}
