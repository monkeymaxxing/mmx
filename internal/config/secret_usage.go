package config

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	"mmx/internal/modules"
	"mmx/internal/value"
)

// SecretUsage is what mmx secrets usage reads from the project's files:
// where each secret key comes from and every place that uses it. It holds
// keys and places, never a value.
type SecretUsage struct {
	// Vault is the vault file that the selected inventory, else the project,
	// selects.
	Vault string
	// FromEnvironment maps each key that secrets_from_env binds to
	// the name of its environment variable.
	FromEnvironment map[string]string
	// Uses maps each key that the project reads to the places that use it,
	// ordered by file and line.
	Uses map[string][]SecretUse
	// OnlyReadByFleetStagesLeftOutBeforeTheRun holds each key of Uses that no
	// run with the inventory reads (SecretNamesARunReads): only fleet stages
	// that the run leaves out before it starts read the key, because they
	// select no nodes in it or their when is false on every node, and no
	// node's connection reads it.
	OnlyReadByFleetStagesLeftOutBeforeTheRun map[string]bool
}

// SecretUse is one place that uses a secret key.
type SecretUse struct {
	File string `json:"file"`
	Line int    `json:"line"`
	// UsedBy names what uses the key at that place, such as
	// "role database, input root_password" or "file /root/.my.cnf (content)".
	UsedBy string `json:"used_by"`
	// UsesInsideTheRole are, for a role call that passes the key to a role
	// input, the places inside the role that read that input.
	UsesInsideTheRole []SecretUse `json:"uses_inside_the_role,omitempty"`
}

// Place is a use's place as file:line.
func (u SecretUse) Place() string {
	return fmt.Sprintf("%s:%d", u.File, u.Line)
}

// SecretUsageOf reads every playbook of the project, with the one-time
// playbooks that are not completed, with the named inventory (the project's
// default inventory when the name is empty), and lists where each secret key
// is used and which keys no run with that inventory reads. It covers every
// place where the loader accepts a .secrets reference: the fields of node
// tasks, role inputs, the settings of fleet stages, and the connection
// secrets of the inventory's nodes. Project, inventory, group, and node vars
// are literal data and hold no reference. Where a role call passes a key to
// a role input, it follows the input to the places inside the role that read
// it.
func SecretUsageOf(settingsFile, inventory string) (SecretUsage, error) {
	playbooks, err := ProjectNames(settingsFile, "playbooks")
	if err != nil {
		return SecretUsage{}, err
	}
	oneTime, err := OutstandingOneTimePlaybooks(settingsFile)
	if err != nil {
		return SecretUsage{}, err
	}
	p, inv, err := Load(settingsFile, "", inventory, nil)
	if err != nil {
		return SecretUsage{}, err
	}
	usage := SecretUsage{Vault: p.Vault, FromEnvironment: p.Secrets, Uses: map[string][]SecretUse{}, OnlyReadByFleetStagesLeftOutBeforeTheRun: map[string]bool{}}
	var found []namedUse
	read := map[string]bool{}
	for _, name := range sortedKeys(inv.Nodes) {
		node := inv.Nodes[name]
		for _, connection := range []struct{ field, reference string }{{"ssh_password", node.SSHPassword}, {"sudo_password", node.SudoPassword}} {
			if key := modules.ConnectionSecretName(connection.reference); key != "" {
				found = append(found, namedUse{key, secretUseAt(node.Sources.At(connection.field), "node "+name+", "+connection.field)})
				read[key] = true
			}
		}
	}
	for _, playbook := range append(playbooks, oneTime...) {
		// Read each playbook as mmx validate does: a source field that
		// reads a command-line value given only at run time names no secret.
		p, inv, err := load(settingsFile, playbook, inventory, nil, nil, true)
		if err != nil {
			return SecretUsage{}, err
		}
		for _, key := range SecretNamesARunReads(p, inv, nil) {
			read[key] = true
		}
		if err := WalkFleetStages(p.FleetStages, func(_ string, s FleetStage) error {
			found = append(found, usesInFleetStageSettings(s)...)
			found = append(found, usesInNodeTasks(s.NodeTasks, secretKeysRead)...)
			return nil
		}); err != nil {
			return SecretUsage{}, err
		}
	}
	for _, use := range found {
		usage.Uses[use.name] = append(usage.Uses[use.name], use.use)
	}
	for key := range usage.Uses {
		usage.Uses[key] = orderedUses(usage.Uses[key])
		if !read[key] {
			usage.OnlyReadByFleetStagesLeftOutBeforeTheRun[key] = true
		}
	}
	return usage, nil
}

// namedUse is a use of one name: a secret key, or inside a role the name of
// a role input.
type namedUse struct {
	name string
	use  SecretUse
}

func secretUseAt(at Location, usedBy string) SecretUse {
	return SecretUse{File: at.File, Line: at.Line, UsedBy: usedBy}
}

// orderedUses orders uses by file and line and drops a repeated one, at
// every level.
func orderedUses(uses []SecretUse) []SecretUse {
	sort.SliceStable(uses, func(i, j int) bool {
		if uses[i].File != uses[j].File {
			return uses[i].File < uses[j].File
		}
		return uses[i].Line < uses[j].Line
	})
	uses = slices.CompactFunc(uses, func(a, b SecretUse) bool {
		return a.File == b.File && a.Line == b.Line && a.UsedBy == b.UsedBy
	})
	for i := range uses {
		uses[i].UsesInsideTheRole = orderedUses(uses[i].UsesInsideTheRole)
	}
	return uses
}

// secretKeysRead lists the secret keys that the references in a value name.
func secretKeysRead(raw any) []string { return namesReadBelow(raw, "secrets") }

// roleInputsRead lists the role inputs that the references in a value name.
func roleInputsRead(raw any) []string { return namesReadBelow(raw, "inputs") }

// namesReadBelow lists the names that the references in a value read below
// root, such as KEY in .secrets.KEY. It need not look into selectors: the
// loader accepts neither a secret key nor a role input declared secret: true
// as the selector of another value.
func namesReadBelow(raw any, root string) []string {
	found := map[string]bool{}
	for _, path := range References(raw) {
		if parts, err := value.Path(path); err == nil && len(parts) > 1 && parts[0].Name == root && parts[1].Name != "" {
			found[parts[1].Name] = true
		}
	}
	return sortedKeys(found)
}

// usesInFleetStageSettings lists the secret keys that a fleet stage's own
// settings read: its selection, condition, loop, outputs, and the templates
// of the files it saves on the controller machine.
func usesInFleetStageSettings(s FleetStage) []namedUse {
	var found []namedUse
	attributed := map[string]bool{}
	add := func(field, words string, raw any) {
		for _, key := range secretKeysRead(raw) {
			found = append(found, namedUse{key, secretUseAt(s.Sources.At(field), "fleet stage "+s.ID+", "+words)})
			attributed[key] = true
		}
	}
	for _, setting := range []struct {
		field string
		raw   any
	}{{"nodes", s.Nodes}, {"except_nodes", s.ExceptNodes}, {"for_each_node", s.ForEachNode}, {"when", s.When}, {"loop", s.Loop}, {"key", s.Key}} {
		add(setting.field, setting.field, setting.raw)
	}
	for _, output := range sortedKeys(s.Outputs) {
		add("outputs."+output, "output "+output, s.Outputs[output])
	}
	for i, file := range s.SaveOnControllerMachine {
		field := ControllerMachineFileField(i)
		add(field+".path", field+".path", file.Path)
		add(field+".template", field+".template", file.TemplateText)
	}
	// Whatever else of the fleet stage reads a key is listed at the fleet
	// stage, so that no use is left out.
	own := s
	own.NodeTasks, own.FleetStages = nil, nil
	for _, key := range secretKeysRead(own) {
		if !attributed[key] {
			found = append(found, namedUse{key, secretUseAt(s.Sources.At(""), "fleet stage "+s.ID)})
		}
	}
	return found
}

// usesInNodeTasks lists, in written order, the uses of every name that reads
// reports in the node tasks: with secretKeysRead the secret keys that the
// node tasks of a fleet stage read, and with roleInputsRead the role inputs
// that the node tasks of a role read.
func usesInNodeTasks(nodeTasks []Task, reads func(any) []string) []namedUse {
	var found []namedUse
	for _, t := range nodeTasks {
		attributed := map[string]bool{}
		for _, read := range referenceFields(t) {
			for _, name := range reads(read.raw) {
				found = append(found, namedUse{name, secretUseAt(t.Sources.At(read.field), secretUsedBy(t, read.field))})
				attributed[name] = true
			}
		}
		if t.Role != "" {
			// A role sees only its inputs. A name that the call passes to an
			// input is followed to the places inside the role that read
			// that input.
			inside := usesInNodeTasks(t.RoleNodeTasks, roleInputsRead)
			for _, output := range sortedKeys(t.RoleOutputs) {
				for _, input := range roleInputsRead(t.RoleOutputs[output]) {
					inside = append(inside, namedUse{input, secretUseAt(t.RoleSources.At("outputs."+output), "role "+t.Role+", output "+output)})
				}
			}
			for _, input := range sortedKeys(t.Inputs) {
				names := reads(t.Inputs[input])
				if len(names) == 0 {
					continue
				}
				use := secretUseAt(t.Sources.At("inputs."+input), "role "+t.Role+", input "+input)
				for _, read := range inside {
					if read.name == input {
						use.UsesInsideTheRole = append(use.UsesInsideTheRole, read.use)
					}
				}
				for _, name := range names {
					found = append(found, namedUse{name, use})
					attributed[name] = true
				}
			}
		}
		// Whatever else of the node task reads a name is listed at the node
		// task, so that no use is left out: for example the template of a
		// file whose template path is chosen for each node.
		own := t
		own.Inputs, own.RoleNodeTasks, own.RoleOutputs, own.TaskBlock = nil, nil, nil, nil
		for _, name := range reads(own) {
			if !attributed[name] {
				found = append(found, namedUse{name, secretUseAt(t.Sources.At(""), secretUsedBy(t, ""))})
			}
		}
		for _, list := range t.Children() {
			if list.Phase != "" {
				found = append(found, usesInNodeTasks(list.NodeTasks, reads)...)
			}
		}
	}
	return found
}

// referenceField is one field of a node task that can hold a reference,
// named as mmx explain names it, such as file.content or when.
type referenceField struct {
	field string
	raw   any
}

// referenceFields lists the fields of one node task that can hold a
// reference. A role call's inputs and the node tasks below the task are not
// among them.
func referenceFields(t Task) []referenceField {
	reads := []referenceField{{"when", t.When}, {"loop", t.Loop}, {"key", t.Key}}
	for _, argument := range taskArguments(t) {
		reads = append(reads, referenceField{argument.field, argument.raw})
	}
	name, module := moduleReads(t)
	for _, field := range sortedKeys(module.Templates) {
		reads = append(reads, referenceField{name + "." + field, module.Templates[field]})
	}
	for _, conditions := range []map[string]any{module.Conditions, module.ResultConditions} {
		for _, field := range sortedKeys(conditions) {
			reads = append(reads, referenceField{name + "." + field, conditions[field]})
		}
	}
	for _, field := range sortedKeys(module.Commands) {
		if field == "" {
			continue // the module's own command: its arguments are the task's
		}
		// A nested command, such as the check of a new file, is a shell
		// task below the field that holds it.
		command := Task{Module: "shell", ModuleInputs: module.Commands[field]}
		for _, read := range referenceFields(command) {
			if read.raw != nil {
				reads = append(reads, referenceField{name + "." + strings.TrimSuffix(field, "shell") + read.field, read.raw})
			}
		}
	}
	if t.Retry != nil {
		reads = append(reads, referenceField{"retry.until", t.Retry.Until}, referenceField{"retry.retry_when", t.Retry.RetryWhen})
	}
	return reads
}

// secretUseSubjectFields are the inputs that say which thing a node task
// works on, in the order in which secretUsedBy looks for one.
var secretUseSubjectFields = []string{"path", "name", "image", "url"}

// secretUsedBy names a node task and the field of it that reads a secret
// key, in the words that mmx explain prints for both: the task's action and
// the field's name. A role call reads "role database, when". A node task
// with a path, name, image, or url reads "file /root/.my.cnf (content)",
// with that input as it is written; any other reads
// "shell env DATABASE_PASSWORD".
func secretUsedBy(t Task, field string) string {
	action, err := t.Action()
	if err != nil {
		return strings.TrimSpace("node task " + field)
	}
	// A module's input is named below the module: file.content is content.
	if field == action.Name {
		field = ""
	}
	field = strings.TrimPrefix(field, action.Name+".")
	if t.Role != "" {
		if field == "" {
			return "role " + t.Role
		}
		return "role " + t.Role + ", " + field
	}
	subject := ""
	if t.TaskBlock != nil {
		subject = t.TaskBlock.ID
	}
	arguments := taskArguments(t)
	for _, name := range secretUseSubjectFields {
		for _, argument := range arguments {
			if written, ok := argument.raw.(string); ok && subject == "" && argument.target != nil && argument.field == action.Name+"."+name {
				subject = written
			}
		}
	}
	switch {
	case subject == "" && field == "":
		return action.Name
	case subject == "":
		return action.Name + " " + strings.ReplaceAll(field, ".", " ")
	case field == "":
		return action.Name + " " + subject
	}
	return action.Name + " " + subject + " (" + field + ")"
}
