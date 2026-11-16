package config

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"mmx/internal/modules"
	"mmx/internal/value"
)

// SaveOnControllerMachine is one entry of a fleet stage's
// save_on_controller_machine list: after the fleet stage's nodes finish, the
// controller process writes the fleet stage's outputs to a file on the
// controller machine. It runs no command.
type SaveOnControllerMachine struct {
	SecretReadableByGroup                bool   `yaml:"secret_readable_by_group,omitempty" json:"secret_readable_by_group,omitempty"`
	DangerouslyLetEveryAccountReadSecret bool   `yaml:"dangerously_let_every_account_read_secret,omitempty" json:"dangerously_let_every_account_read_secret,omitempty"`
	Path                                 string `yaml:"path" json:"path"`
	PermissionsMode                      string `yaml:"permissions_mode" json:"permissions_mode"`
	// Output names one of the fleet stage's outputs: one file for each node.
	Output string `yaml:"output,omitempty" json:"output,omitempty"`
	// Template is a template file rendered once from the fleet stage's whole
	// output collection: one file.
	Template     string `yaml:"template,omitempty" json:"template,omitempty"`
	TemplateText string `yaml:"-" json:"template_text,omitempty"`
	// TemplateFile is the template's file on the controller machine, which
	// explain shows.
	TemplateFile string `yaml:"-" json:"-"`
	// ProjectRoot is where a relative path starts. The run saves it, so a
	// resume writes to the same place.
	ProjectRoot string `yaml:"-" json:"project_root"`
}

// ControllerMachineFileField names entry i of save_on_controller_machine.
func ControllerMachineFileField(i int) string {
	return fmt.Sprintf("save_on_controller_machine[%d]", i)
}

// prepareControllerMachineFiles checks a fleet stage's save_on_controller_machine
// entries and reads their templates. d declares what the fleet stage and the
// fleet stages before it publish.
func prepareControllerMachineFiles(s *FleetStage, p Project, inv Inventory, base string, d declarations) error {
	if len(s.SaveOnControllerMachine) > 0 && (s.Loop != nil || s.ForEachNode != nil) {
		return s.Sources.Error("save_on_controller_machine", fmt.Errorf("fleet stage loop %s: save_on_controller_machine belongs to a fleet stage that selects nodes; put it on a fleet stage inside the loop", s.ID))
	}
	// Later paths refuse the outputs known here to hold a secret.
	d["secret_outputs."+s.ID] = secretOutputs(*s)
	// A file is written on the controller machine: no node values, and no
	// node task results.
	controller := copyDeclarations(d)
	for i := range s.SaveOnControllerMachine {
		e := &s.SaveOnControllerMachine[i]
		field := ControllerMachineFileField(i)
		fail := func(err error) error { return s.Sources.Error(field, err) }
		e.ProjectRoot = base
		if e.Path == "" {
			return fail(modules.Invalid("path", "path is required: where on the controller machine to write the file"))
		}
		if !modules.PermissionsModePattern.MatchString(e.PermissionsMode) {
			return fail(modules.Invalid("permissions_mode", "permissions_mode is required: a quoted octal string, such as \"0644\", or \"0600\" for a file that holds a secret"))
		}
		if (e.Output == "") == (e.Template == "") {
			return fail(fmt.Errorf("set exactly one of output or template; output writes one file per node, and template writes one file from the whole output collection"))
		}
		pathScope := copyDeclarations(controller)
		if e.Output != "" {
			if _, ok := s.Outputs[e.Output]; !ok {
				return fail(modules.Invalid("output", fmt.Sprintf("output %s is not one of fleet stage %s's outputs", e.Output, s.ID)))
			}
			if !slices.Contains(References(e.Path), ".node.name") {
				return fail(modules.Invalid("path", "path must contain {{ .node.name }}: an output entry writes one file for each node, such as server_details/{{ .node.name }}.txt"))
			}
			pathScope["node"] = map[string]bool{"name": true}
		}
		if err := checkValue(e.Path, pathScope); err != nil {
			return fail(&modules.FieldError{Field: "path", Err: explainUndeclaredVariable(err, p, inv, "", variablesOfNoSingleNode)})
		}
		if len(SecretNames(e.Path)) > 0 || readsSecretOutput(e.Path, d) {
			return fail(modules.Invalid("path", "path is public: it cannot read a secret"))
		}
		if !modules.Dynamic(e.Path) {
			if _, err := ControllerMachinePath(base, e.Path); err != nil {
				return fail(&modules.FieldError{Field: "path", Err: err})
			}
		}
		if e.Template != "" {
			if modules.Dynamic(e.Template) {
				return fail(modules.Invalid("template", "template names a file in the project; it takes no references"))
			}
			path, err := modules.LocalSource(base, "template", e.Template)
			if err != nil {
				return fail(err)
			}
			b, err := modules.ReadTemplate(path)
			if err != nil {
				return fail(&modules.FieldError{Field: "template", Err: err})
			}
			e.TemplateText, e.TemplateFile = string(b), path
			if err := checkTemplate(e.TemplateText, controller); err != nil {
				return fail(&modules.FieldError{Field: "template", Err: explainUndeclaredVariable(err, p, inv, "", variablesOfNoSingleNode)})
			}
		}
	}
	return nil
}

// ControllerMachinePath is where a save_on_controller_machine path writes: an
// absolute path as written, a relative one from the project root, which it
// cannot leave.
func ControllerMachinePath(root, path string) (string, error) {
	if filepath.IsAbs(path) {
		return filepath.Clean(path), nil
	}
	full := filepath.Join(root, path)
	if relative, err := filepath.Rel(root, full); err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("path %q leaves the project root %s; a relative path stays inside the project, an absolute path may name any place on the controller machine", path, root)
	}
	return full, nil
}

// controllerMachineFileReads is what a fleet stage's
// save_on_controller_machine entries read, for the limit check. A template
// entry reads the fleet stage's whole output collection, whatever its text
// names.
func controllerMachineFileReads(s FleetStage) []any {
	var reads []any
	for _, e := range s.SaveOnControllerMachine {
		reads = append(reads, e.Path)
		if e.Template != "" {
			reads = append(reads, e.TemplateText, "{{ .outputs."+s.ID+" }}")
		}
	}
	return reads
}

// secretOutputs lists the outputs of a fleet stage that visibly hold a secret:
// those that read a named secret, or the result of one of its node tasks that
// marks its value secret (read_file with content_is_secret, shell with
// output_is_secret). Others may still hold one; the controller process
// refuses a secret in a path when it renders the path.
func secretOutputs(s FleetStage) map[string]bool {
	secretTasks := map[string]bool{}
	for _, t := range s.NodeTasks {
		_, inputs := t.ModuleCall()
		if secret, ok := inputs.(interface{ ResultIsSecret() bool }); ok && t.ID != "" && secret.ResultIsSecret() {
			secretTasks[t.ID] = true
		}
	}
	out := map[string]bool{}
	for name, expression := range s.Outputs {
		if len(SecretNames(expression)) > 0 {
			out[name] = true
		}
		for _, ref := range References(expression) {
			if parts, _ := value.Path(ref); len(parts) >= 2 && parts[0].Name == "node_tasks" && secretTasks[parts[1].Name] {
				out[name] = true
			}
		}
	}
	return out
}

// readsSecretOutput reports a path that reads an output secretOutputs found.
func readsSecretOutput(path string, d declarations) bool {
	for _, ref := range References(path) {
		parts, _ := value.Path(ref)
		if len(parts) >= 4 && parts[0].Name == "outputs" && parts[3].Dynamic == "" && d["secret_outputs."+parts[1].Name][parts[3].Name] {
			return true
		}
	}
	return false
}
