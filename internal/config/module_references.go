package config

import (
	"fmt"
	"reflect"
	"regexp"
	"strings"

	"mmx/internal/modules"
	"mmx/internal/value"
)

// moduleReads is what a task's module reads beyond its argument list.
func moduleReads(t Task) (string, modules.Reads) {
	name, inputs := t.ModuleCall()
	if m, ok := inputs.(interface{ Reads() modules.Reads }); ok {
		reads := m.Reads()
		for field := range reads.Commands {
			if underWholeValue(t, field) {
				// The command does not exist yet. All readers wait until
				// checkModuleWithWholeValues resolves and checks it.
				delete(reads.Commands, field)
			}
		}
		return name, reads
	}
	return name, modules.Reads{}
}

// Check references before node values are available, retaining the consuming field.
func checkModuleReferences(t Task, d declarations) error {
	for _, arg := range taskArguments(t) {
		if err := checkValue(arg.raw, d); err != nil {
			return t.Sources.Error(arg.field, err)
		}
		// A role input stands for a secret only when it is declared so.
		if text, ok := arg.raw.(string); ok && arg.wholeSecretReference {
			if input := modules.RoleInputName(text); input != "" && !d[secretRoleInputs][input] {
				return t.Sources.Error(arg.field, fmt.Errorf("%s reads role input %s, which must be declared secret: true so the role call supplies a secret reference instead of a plaintext YAML value", arg.field, input))
			}
		}
	}
	name, reads := moduleReads(t)
	for _, field := range sortedKeys(reads.Templates) {
		if err := checkTemplate(reads.Templates[field], d); err != nil {
			return t.Sources.Error(name+"."+field, err)
		}
	}
	for _, field := range sortedKeys(reads.Conditions) {
		if err := checkPredicate(reads.Conditions[field], d); err != nil {
			return t.Sources.Error(name+"."+field, err)
		}
	}
	for _, field := range sortedKeys(reads.Commands) {
		local := d
		if field != "" {
			local = copyDeclarations(d)
			local["new_file"] = map[string]bool{"path": true}
		}
		if err := checkShell(reads.Commands[field], local); err != nil {
			return t.Sources.Error(fieldPath(name, field), err)
		}
		if field == "" {
			continue // the module's own command: its scripts are below
		}
		nested := reads.Commands[field].Reads().ShellScripts
		for _, script := range sortedKeys(nested) {
			if err := checkLiteralScript(fieldPath(name, fieldPath(field, script)), nested[script], local, true); err != nil {
				return t.Sources.Error(fieldPath(name, fieldPath(field, script)), err)
			}
		}
	}
	takesEnv := moduleTakesEnv(t)
	for _, field := range sortedKeys(reads.ShellScripts) {
		if err := checkLiteralScript(fieldPath(name, field), reads.ShellScripts[field], d, takesEnv); err != nil {
			return t.Sources.Error(fieldPath(name, field), err)
		}
	}
	_, inputs := t.ModuleCall()
	if check, ok := inputs.(interface {
		CheckSecretInputs(func(any) bool, func(string) bool) error
	}); ok {
		if err := check.CheckSecretInputs(func(raw any) bool {
			return len(secretKeysRead(raw)) > 0 || readsASecretRoleInput(raw, d)
		}, func(field string) bool { return underWholeValue(t, field) }); err != nil {
			return t.Sources.Error(name, err)
		}
	}
	return nil
}

// mmxRoots are the values a reference in a task can read, beside the names a
// task can see in its place, such as a loop's as name.
var mmxRoots = []string{"vars", "inputs", "secrets", "node", "outputs", "node_tasks", "new_file", "fleet_stages", "inventory", "this_node_task"}

// templateRoot finds, inside {{ }}, a name that starts with a dot, as a
// reference or a Go template reads it: {{ .vars.port }}, {{index .vars "x"}}.
var templateRoot = regexp.MustCompile(`(?:^|[\s(|,])\.([A-Za-z_][A-Za-z0-9_]*)`)

// checkLiteralScript refuses, in a script that runs as written, a reference
// to a value MMX knows there. MMX fills in no reference in such a script, so
// the shell would get the braces themselves: a habit from Ansible that would
// otherwise fail only on the node. A template for another tool, such as
// docker's {{.State.Status}}, names no such value and stays; so does the
// back-quoted literal form. The advice names env where the module takes it;
// a module without env, such as reboot, gets its node task's when instead.
func checkLiteralScript(field, script string, d declarations, takesEnv bool) error {
	if !strings.Contains(script, "{{") {
		return nil
	}
	roots := map[string]bool{}
	for _, root := range mmxRoots {
		roots[root] = true
	}
	for name := range d {
		if !strings.Contains(name, ".") {
			roots[name] = true
		}
	}
	for _, match := range value.Reference.FindAllStringSubmatch(value.WithoutLiterals(script), -1) {
		body := strings.TrimSpace(strings.Trim(match[1], "-"))
		for _, root := range templateRoot.FindAllStringSubmatch(body, -1) {
			if !roots[root[1]] {
				continue
			}
			if !takesEnv {
				return fmt.Errorf("%s runs as written, so the shell would receive the literal text %s; MMX does not replace references in %s, and the module has no env input; use the node task's when condition, which accepts references, or pass the value through env to an earlier shell node task and check the shell result in when", field, match[0], field)
			}
			reference := "{{ " + body + " }}"
			name := environmentName(body)
			return fmt.Errorf("%s runs as written, so the shell would receive the literal text %s; MMX does not replace references in %s; pass the value through env and read the environment variable in the script, such as env: {%s: %q} and $%s", field, match[0], field, name, reference, name)
		}
	}
	return nil
}

// moduleTakesEnv reports whether the task's module has an env input, the way
// a value reaches a script that runs as written.
func moduleTakesEnv(t Task) bool {
	_, inputs := t.ModuleCall()
	inputType := reflect.TypeOf(inputs)
	if inputType == nil || inputType.Kind() != reflect.Pointer || inputType.Elem().Kind() != reflect.Struct {
		return false
	}
	_, takesEnv := yamlFields(inputType.Elem())["env"]
	return takesEnv
}

// environmentName suggests an environment variable for a reference: the last
// name it reads, in capitals.
func environmentName(body string) string {
	fields := strings.FieldsFunc(body, func(r rune) bool {
		return !(r == '_' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9')
	})
	if len(fields) == 0 {
		return "VALUE"
	}
	name := strings.ToUpper(fields[len(fields)-1])
	// A name the shell or the system already sets, such as PATH, would
	// replace that variable for the script and break it: .new_file.path
	// becomes NEW_FILE_PATH.
	if shellOwnVariables[name] {
		if len(fields) > 1 {
			name = strings.ToUpper(fields[len(fields)-2]) + "_" + name
		} else {
			name = "VALUE_" + name
		}
	}
	if name[0] >= '0' && name[0] <= '9' {
		name = "VALUE_" + name
	}
	return name
}

// shellOwnVariables are environment variables that the shell, the login,
// or common programs set and read, so a suggested name avoids them.
var shellOwnVariables = map[string]bool{
	"PATH": true, "HOME": true, "USER": true, "LOGNAME": true, "SHELL": true,
	"PWD": true, "OLDPWD": true, "IFS": true, "LANG": true, "TERM": true,
	"HOSTNAME": true, "TMPDIR": true, "UID": true, "PS1": true, "LC_ALL": true,
}
