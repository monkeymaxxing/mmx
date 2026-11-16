package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"mmx/internal/modules"
	"mmx/internal/value"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"
)

func Read(path string, dst any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return Decode(path, b, dst)
}
func Decode(path string, b []byte, dst any) error {
	var node yaml.Node
	if err := yaml.Unmarshal(b, &node); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if err := checkNode(&node); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if err := checkTypes(&node, reflect.TypeOf(dst), ""); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	d := yaml.NewDecoder(bytes.NewReader(b))
	d.KnownFields(true)
	if err := d.Decode(dst); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return fmt.Errorf("%s: exactly one YAML document is required", path)
	}
	captureSources(reflect.ValueOf(dst), &node, path, "")
	return nil
}

// yaml.v3 intentionally supports some YAML 1.1 coercions. MMX does not: a
// numeric mode or a string such as "yes" must not silently change its type.
func checkTypes(n *yaml.Node, t reflect.Type, field string) error {
	return checkTypesOf(n, t, field, false)
}

// checkTypesOf checks a node against a Go type. With wholeValueReferences, as
// in a core module's inputs, text that is exactly one reference fits any
// type: it is checked against the field's type when it resolves
// (WholeValueReference).
func checkTypesOf(n *yaml.Node, t reflect.Type, field string, wholeValueReferences bool) error {
	if wholeValueReferences && n.Kind == yaml.ScalarNode && n.Tag == "!!str" && value.IsWholeReference(n.Value) {
		return nil
	}
	at := func(message string) error {
		return fmt.Errorf("line %d column %d (%s): %s", n.Line, n.Column, field, message)
	}
	if n.Kind == yaml.DocumentNode {
		if len(n.Content) == 0 {
			return fmt.Errorf("empty YAML document")
		}
		return checkTypesOf(n.Content[0], t, field, wholeValueReferences)
	}
	if n.Tag == "!!null" && (t == reflect.TypeOf((*string)(nil)) || t == reflect.TypeOf((*int)(nil))) {
		return nil
	}
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if n.Tag == "!!null" {
		if t.Kind() != reflect.Interface && t.Kind() != reflect.Map && t.Kind() != reflect.Slice {
			return at("the field does not accept null")
		}
		return nil
	}
	if t == reflect.TypeOf(TryLimit(0)) && n.Tag == "!!str" && n.Value == "unlimited" {
		return nil
	}
	if t == reflect.TypeOf(Retry{}) && n.Kind == yaml.ScalarNode && n.Tag == "!!bool" {
		return nil
	}
	if t == reflect.TypeOf(modules.Guard{}) && n.Kind == yaml.ScalarNode && n.Tag == "!!str" {
		return nil
	}
	// A port, failure count, or node count may be written as an integer; decoding keeps its text.
	if (t == reflect.TypeOf(modules.PortOrRange("")) || t == reflect.TypeOf(FailureAllowance("")) || t == reflect.TypeOf(NodesAtOnce(""))) && n.Tag == "!!int" {
		return nil
	}
	// An owner or group is a name as text or a numeric ID as an integer.
	if t == reflect.TypeOf(modules.Account("")) && n.Kind == yaml.ScalarNode {
		var written any
		if err := n.Decode(&written); err != nil {
			return at(err.Error())
		}
		if err := modules.AccountRule(written); err != nil {
			return at(err.Error())
		}
		return nil
	}
	if t == reflect.TypeOf(Task{}) && n.Kind == yaml.MappingNode {
		return checkTaskTypes(n, field)
	}
	switch t.Kind() {
	case reflect.Interface:
		return nil
	case reflect.Struct:
		if n.Kind != yaml.MappingNode {
			return at("expected a mapping")
		}
		fields := yamlFields(t)
		for i := 0; i < len(n.Content); i += 2 {
			if ft, ok := fields[n.Content[i].Value]; ok {
				if e := checkTypesOf(n.Content[i+1], ft, fieldPath(field, n.Content[i].Value), wholeValueReferences); e != nil {
					return e
				}
			} else {
				return fmt.Errorf("line %d column %d (%s): unknown field", n.Content[i].Line, n.Content[i].Column, fieldPath(field, n.Content[i].Value))
			}
		}
		return nil
	case reflect.Map:
		if n.Kind != yaml.MappingNode {
			return at("expected a mapping")
		}
		for i := 1; i < len(n.Content); i += 2 {
			if e := checkTypesOf(n.Content[i], t.Elem(), fieldPath(field, n.Content[i-1].Value), wholeValueReferences); e != nil {
				return e
			}
		}
		return nil
	case reflect.Slice:
		if n.Kind != yaml.SequenceNode {
			return at("expected a list")
		}
		for i, c := range n.Content {
			if e := checkTypesOf(c, t.Elem(), fmt.Sprintf("%s[%d]", field, i), wholeValueReferences); e != nil {
				return e
			}
		}
		return nil
	case reflect.String:
		if n.Tag == "!!str" {
			return nil
		}
	case reflect.Bool:
		if n.Tag == "!!bool" {
			return nil
		}
	case reflect.Int:
		if n.Tag == "!!int" {
			return nil
		}
	default:
		// Non-schema numeric and non-YAML kinds have no supported input form and reach the error below.
	}
	return at(fmt.Sprintf("expected %s, got %s", t.Kind(), n.Tag))
}

// yamlFields maps a struct's YAML field names to their types: the fields
// strict decoding accepts, which mmx describe-module also lists.
func yamlFields(t reflect.Type) map[string]reflect.Type {
	fields := map[string]reflect.Type{}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if name := strings.Split(f.Tag.Get("yaml"), ",")[0]; name != "-" {
			fields[name] = f.Type
		}
	}
	return fields
}
func checkNode(n *yaml.Node) error {
	if n.Kind == yaml.AliasNode || n.Anchor != "" || n.Style&yaml.TaggedStyle != 0 {
		return fmt.Errorf("line %d: aliases, anchors, and explicit tags are not supported", n.Line)
	}
	if n.Kind == yaml.MappingNode {
		seen := map[string]bool{}
		for i := 0; i < len(n.Content); i += 2 {
			k := n.Content[i]
			if k.Tag != "!!str" || k.Value == "<<" {
				return fmt.Errorf("line %d: mapping keys must be plain strings; merge keys are not supported", k.Line)
			}
			if value.HasReservedKey(map[string]any{k.Value: nil}) {
				return fmt.Errorf("line %d: mapping key %q is reserved for internal MMX records. Rename the key before loading the YAML file again.", k.Line, k.Value)
			}
			if seen[k.Value] {
				return fmt.Errorf("line %d: duplicate key %q", k.Line, k.Value)
			}
			seen[k.Value] = true
		}
	}
	if n.Tag == "!!bool" && n.Value != "true" && n.Value != "false" {
		return fmt.Errorf("line %d: spell booleans true or false", n.Line)
	}
	for _, c := range n.Content {
		if err := checkNode(c); err != nil {
			return err
		}
	}
	return nil
}

// FindProject searches the current directory and its parents for project settings.
func FindProject(start string) (string, error) {
	directory, err := filepath.Abs(start)
	if err != nil {
		return "", err
	}
	for {
		file := filepath.Join(directory, "mmx_project_settings.yml")
		info, err := os.Stat(file)
		if err == nil {
			if !info.Mode().IsRegular() {
				return "", fmt.Errorf("%s must be a regular file", file)
			}
			return file, nil
		}
		if !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			return "", fmt.Errorf("no mmx_project_settings.yml found in the current directory or its parents")
		}
		directory = parent
	}
}

// ProjectNames lists named YAML files in a project's playbooks or inventories directory.
func ProjectNames(settingsFile, directory string) ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(filepath.Dir(settingsFile), directory))
	if os.IsNotExist(err) {
		return []string{}, nil
	}
	if err != nil {
		return nil, err
	}
	names := []string{}
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".yml") {
			name := strings.TrimSuffix(entry.Name(), ".yml")
			if !modules.Name.MatchString(name) {
				return nil, fmt.Errorf("invalid %s name %q", directory, name)
			}
			names = append(names, name)
		}
	}
	return names, nil
}

func ReadSettings(path string) (Project, error) {
	count := 3
	p := Project{HistoryRetention: DefaultHistoryRetention, MaxNodesAtOnce: 100, Defaults: Defaults{Retry: RetryDefaults{MaxTries: TryLimit(count), PauseBetweenTries: "2s"}}}
	abs, err := filepath.Abs(path)
	if err != nil {
		return p, err
	}
	if err = readProjectFile(filepath.Dir(abs), abs, &p); err != nil {
		return p, err
	}
	p.SettingsFile = abs
	if _, err := HistoryRetention(p.HistoryRetention); err != nil {
		return p, p.Sources.Error("run_history_retention", err)
	}
	if p.Project == "" {
		return p, p.Sources.Error("project_name", fmt.Errorf("project_name is required, such as project_name: shop"))
	}
	if !modules.Name.MatchString(p.Project) {
		return p, p.Sources.Error("project_name", fmt.Errorf("project_name must start with a letter or underscore and contain only letters, digits, underscores, or hyphens"))
	}
	if p.Inventory == "" {
		return p, p.Sources.Error("default_inventory", fmt.Errorf("default_inventory is required: the inventory used when --inventory is omitted, such as default_inventory: production for inventories/production.yml"))
	}
	if !modules.Name.MatchString(p.Inventory) {
		return p, p.Sources.Error("default_inventory", fmt.Errorf("default_inventory must be an inventory name, not a path"))
	}
	for _, name := range sortedKeys(p.Secrets) {
		if env := p.Secrets[name]; !modules.Name.MatchString(name) || !modules.Name.MatchString(env) || strings.Contains(env, "-") {
			return p, p.Sources.Error("secrets_from_env."+name, fmt.Errorf("secret key %q must start with a letter or underscore and contain only letters, digits, underscores, or hyphens; the secrets_from_env value must name an environment variable using letters, digits, or underscores and starting with a letter or underscore", name))
		}
	}
	if p.MaxNodesAtOnce < 1 {
		return p, p.Sources.Error("max_nodes_at_once", fmt.Errorf("max_nodes_at_once must be a positive integer"))
	}
	return p, nil
}

// Load reads project settings, a named playbook, and a named inventory.
// An empty playbook loads only settings and inventory, for mmx nodes.
func Load(path, playbook, inventory string, commandLine []string) (Project, Inventory, error) {
	p, inv, err := load(path, playbook, inventory, commandLine, nil, false)
	if err != nil || playbook == "" {
		return p, inv, err
	}
	return p, inv, checkCommandRecordsAcrossPlaybooks(path, p, inv.Name)
}

// checkCommandRecordsAcrossPlaybooks rejects a command of the loaded
// playbook that would share a command record with a different command of
// another playbook, as checkCommandRecords does within one playbook: a
// record's key names the project and the task's result path, not the
// playbook. A playbook directory that cannot be listed, and another playbook
// that does not load, are left to mmx validate, which reports the error.
func checkCommandRecordsAcrossPlaybooks(path string, p Project, inventory string) error {
	playbooks, err := projectPlaybooks(path)
	if err != nil {
		return nil
	}
	seen := map[string]Location{}
	if err := checkCommandRecords(p.FleetStages, seen); err != nil {
		return err
	}
	for _, playbook := range playbooks {
		if playbook == p.PlaybookName {
			continue
		}
		other, _, err := load(path, playbook, inventory, nil, nil, true)
		if err != nil {
			continue
		}
		if err := checkCommandRecords(other.FleetStages, seen); err != nil {
			return err
		}
	}
	return nil
}

// projectPlaybooks lists the playbooks and the outstanding one-time
// playbooks.
func projectPlaybooks(path string) ([]string, error) {
	playbooks, err := ProjectNames(path, "playbooks")
	if err != nil {
		return nil, err
	}
	oneTime, err := OutstandingOneTimePlaybooks(path)
	if err != nil {
		return nil, err
	}
	return append(playbooks, oneTime...), nil
}

// LoadRunCommand creates an ordinary fleet stage for a one-off command.
func LoadRunCommand(path, inventory string, nodes []string, script, user string) (Project, Inventory, error) {
	selection := make([]any, len(nodes))
	for i, node := range nodes {
		selection[i] = node
	}
	return load(path, "", inventory, nil, &FleetStage{ID: "run-command", Nodes: selection, NodeTasks: []Task{{ID: "command", Module: "shell", ModuleInputs: &modules.Shell{Script: script, User: user}}}}, false)
}

func load(path, playbook, inventory string, commandLine []string, command *FleetStage, wholeProjectValidation bool) (p Project, inv Inventory, err error) {
	// A one-time playbook names a node that the inventory no longer has
	// when the node was removed after the playbook's work: then the
	// playbook belongs in completed/.
	var unknown *UnknownNodeError
	defer func() {
		if IsOneTimePlaybookName(playbook) && errors.As(err, &unknown) {
			err = fmt.Errorf("%w; a one-time playbook that no node needs any more goes to %s/", err, CompletedOneTimePlaybooksDirectory)
		}
	}()
	p, err = ReadSettings(path)
	if err != nil {
		return p, inv, err
	}
	p.WholeProjectValidation = wholeProjectValidation
	base := filepath.Dir(p.SettingsFile)
	if playbook != "" {
		if !modules.Name.MatchString(playbook) && !IsOneTimePlaybookName(playbook) {
			return p, inv, fmt.Errorf("playbook must be a name, not a path: %q", playbook)
		}
		file, err := playbookFile(p.SettingsFile, playbook)
		if err != nil {
			return p, inv, err
		}
		var book Playbook
		if err := readProjectFile(base, file, &book); err != nil {
			if os.IsNotExist(err) {
				return p, inv, NoSuchPlaybookError(p.SettingsFile, playbook)
			}
			return p, inv, err
		}
		if IsOneTimePlaybookName(playbook) {
			if err := checkOneTimePlaybook(book); err != nil {
				return p, inv, err
			}
		}
		p.FleetStages, p.CommandLineVars, p.PlaybookName = book.FleetStages, book.CommandLineVars, playbook
		p.Sources["fleet_stages"], p.Sources["command_line_vars"] = book.Sources.At("fleet_stages"), book.Sources.At("command_line_vars")
		if len(p.FleetStages) == 0 {
			return p, inv, book.Sources.Error("fleet_stages", fmt.Errorf("fleet_stages must not be empty"))
		}
	} else if len(commandLine) > 0 {
		return p, inv, fmt.Errorf("--var requires a playbook name")
	}
	// The inventory's name comes from the caller, such as --inventory, or
	// from the project's default_inventory; an error names the one it was.
	named := inventory != ""
	if !named {
		inventory = p.Inventory
	}
	if !modules.Name.MatchString(inventory) {
		return p, inv, fmt.Errorf("--inventory must be a name, not a path: %q", inventory)
	}
	inventoryPath := InventoryPath(p.SettingsFile, inventory)
	if err := readProjectFile(base, inventoryPath, &inv); err != nil {
		if os.IsNotExist(err) {
			names, _ := ProjectNames(p.SettingsFile, "inventories")
			err = fmt.Errorf("there is no inventory %s: inventories/%s.yml does not exist; the project's inventories are: %s", inventory, inventory, namesOrNone(names))
		} else {
			err = fmt.Errorf("inventory %s: %w", inventory, err)
		}
		if !named {
			return p, inv, p.Sources.Error("default_inventory", err)
		}
		return p, inv, err
	}
	inv.Path, inv.Name = inventoryPath, inventory
	p.Vault = VaultPath(p.SettingsFile, p, inventoryPath, inv)
	if len(inv.Nodes) == 0 {
		return p, inv, inv.Sources.Error("nodes", fmt.Errorf("inventory requires at least one node"))
	}
	for _, id := range sortedKeys(inv.Nodes) {
		h := inv.Nodes[id]
		if h.Transport == "" {
			h.Transport = "https_with_ssh_fallback"
		}
		if err := checkManagedNode(id, h); err != nil {
			return p, inv, h.Sources.Error("", err)
		}
		for _, v := range []*string{&h.SSHConfig, &h.KnownHosts, &h.IdentityFile} {
			if *v == "" {
				continue
			}
			// A path written as ~/.ssh/key, as people write it for OpenSSH,
			// starts at the home directory, as it does for OpenSSH.
			if rest, ok := strings.CutPrefix(*v, "~/"); ok {
				home, err := os.UserHomeDir()
				if err != nil {
					return p, inv, h.Sources.Error("", fmt.Errorf("node %s: cannot expand ~ in %s: %w", id, *v, err))
				}
				*v = filepath.Join(home, rest)
				continue
			}
			*v = join(base, *v)
		}
		inv.Nodes[id] = h
	}
	for _, group := range sortedKeys(inv.Groups) {
		g := inv.Groups[group]
		if _, found := inv.Nodes[group]; found || group == "all" || !modules.Name.MatchString(group) {
			return p, inv, g.Sources.Error("", fmt.Errorf("inventory group %q must start with a letter or underscore and contain only letters, digits, underscores, or hyphens; the group name must differ from 'all' and every node name", group))
		}
		for i, h := range g.Nodes {
			if _, ok := inv.Nodes[h]; !ok || slices.Contains(g.Nodes[:i], h) {
				return p, inv, g.Sources.Error(fmt.Sprintf("nodes[%d]", i), fmt.Errorf("group %s: unknown or repeated node %s", group, h))
			}
		}
		slices.Sort(g.Nodes)
		inv.Groups[group] = g
	}
	if err := checkGroupVars(inv); err != nil {
		return p, inv, err
	}
	if err := checkCommandLineVars(p, inv); err != nil {
		return p, inv, err
	}
	if p.CommandLineValues, err = commandLineValues(p, commandLine); err != nil {
		return p, inv, err
	}
	if p.MaxNodesAtOnce < 1 {
		return p, inv, p.Sources.Error("max_nodes_at_once", fmt.Errorf("max_nodes_at_once must be a positive integer"))
	}
	if command != nil {
		if _, err := Select(inv, command.Nodes); err != nil {
			return p, inv, fmt.Errorf("--nodes: %w", err)
		}
		p.FleetStages = []FleetStage{*command}
		return p, inv, prepareFleetStages(p.FleetStages, p, inv, base, declarations{"outputs": {}, "fleet_stages": {}})
	}
	if err := prepareFleetStages(p.FleetStages, p, inv, base, declarations{"outputs": {}, "fleet_stages": {}}); err != nil {
		return p, inv, err
	}
	if err := checkCommandRecords(p.FleetStages, map[string]Location{}); err != nil {
		return p, inv, err
	}
	if err := checkEnabledOnQuadletServices(p.FleetStages); err != nil {
		return p, inv, err
	}
	if err := checkWithinFleetStages(p.FleetStages); err != nil {
		return p, inv, err
	}
	if err := checkInventoryGroupRules(p.FleetStages, inv); err != nil {
		return p, inv, err
	}
	if err := loadModules(&p, base); err != nil {
		return p, inv, err
	}
	if _, err := Inspect(p, inv, nil); err != nil {
		return p, inv, err
	}
	return p, inv, nil
}

// checkGroupVars rejects two groups of one node that set the same variable,
// even to equal values: group order never decides a value.
func checkGroupVars(inv Inventory) error {
	setBy := map[string]map[string]string{} // node → variable → group
	for _, group := range sortedKeys(inv.Groups) {
		g, keys := inv.Groups[group], sortedKeys(inv.Groups[group].Vars)
		for _, managedNode := range g.Nodes {
			if len(keys) > 0 && setBy[managedNode] == nil {
				setBy[managedNode] = map[string]string{}
			}
			for _, key := range keys {
				if first, ok := setBy[managedNode][key]; ok {
					return g.Sources.Error("vars."+key, fmt.Errorf("node %s belongs to groups %s and %s, which set the same variable %s; define the variable in inventory vars, in only one group, or in each node's vars", managedNode, first, group, key))
				}
				setBy[managedNode][key] = group //nolint:nilaway // setBy[managedNode] is allocated whenever this group has keys to iterate.
			}
		}
	}
	return nil
}

// checkCommandLineVars checks each declaration, and that no project,
// inventory, group, or node vars set a declared name.
func checkCommandLineVars(p Project, inv Inventory) error {
	if len(p.CommandLineVars) == 0 {
		return nil
	}
	layers := append([]varLayer{{p.Vars, p.Sources}, {inv.Vars, inv.Sources}}, everyNodeLayer(inv)...)
	for _, name := range sortedKeys(p.CommandLineVars) {
		v := p.CommandLineVars[name]
		switch {
		case !modules.Name.MatchString(name):
			return v.Sources.Error("", fmt.Errorf("invalid command-line variable name %q", name))
		case !contains([]string{"string", "integer", "boolean"}, v.Type):
			return v.Sources.Error("type", fmt.Errorf("command-line variable %s needs type string, integer, or boolean", name))
		case v.Default != nil && (modules.Dynamic(v.Default) || !InputType(v.Default, v.Type)):
			return v.Sources.Error("default", fmt.Errorf("command-line variable %s default must be a literal %s", name, v.Type))
		}
		for _, layer := range layers {
			if _, ok := layer.vars[name]; ok {
				return v.Sources.Error("", fmt.Errorf("command-line variable %s must not also be set in vars (%s)", name, layer.sources.At("vars."+name)))
			}
		}
	}
	return nil
}

// CommandLineError reports a rejected --var value rather than invalid configuration.
type CommandLineError struct{ error }

// commandLineValues types each --var NAME=VALUE by its declaration. The text
// after the first = is the value; a string is taken literally.
func commandLineValues(p Project, args []string) (map[string]any, error) {
	values := map[string]any{}
	for _, arg := range args {
		name, text, ok := strings.Cut(arg, "=")
		v, declared := p.CommandLineVars[name]
		_, repeated := values[name]
		var err error
		switch {
		case !ok:
			err = fmt.Errorf("expected NAME=VALUE")
		case !declared:
			err = fmt.Errorf("%s is not declared in command_line_vars", name)
		case repeated:
			err = fmt.Errorf("%s is set more than once", name)
		case v.Type == "integer":
			var n int64
			if n, err = strconv.ParseInt(text, 10, 64); err != nil {
				err = fmt.Errorf("%s must be a decimal integer", name)
			}
			values[name] = int(n)
		case v.Type == "boolean":
			if text != "true" && text != "false" {
				err = fmt.Errorf("%s must be true or false", name)
			}
			values[name] = text == "true"
		case text == "" && v.Default == nil:
			// An unset CI environment variable becomes --var name=, and deploying
			// "Release ." must not pass silently; a default says empty is intended.
			return nil, &CommandLineError{fmt.Errorf("--var %s= gives an empty value; %s has no default, so MMX refuses an empty value, which most often comes from an unset environment variable; give a value, or declare default: \"\" for %s in the playbook's command_line_vars", name, name, name)}
		default:
			values[name] = text
		}
		if err != nil {
			names := "no command-line variables are declared"
			if len(p.CommandLineVars) > 0 {
				names = "declared: " + strings.Join(sortedKeys(p.CommandLineVars), ", ")
			}
			return nil, &CommandLineError{fmt.Errorf("--var %s: %w (%s)", arg, err, names)}
		}
	}
	return values, nil
}

// RequireCommandLineValues rejects a command-line variable with neither a --var
// value nor a default: explain, dryrun, and run need every value.
func RequireCommandLineValues(p Project) error {
	var missing []string
	for _, name := range sortedKeys(p.CommandLineVars) {
		if _, set := p.CommandLineValues[name]; !set && p.CommandLineVars[name].Default == nil {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("set %s with --var NAME=VALUE; command_line_vars gives no default", strings.Join(missing, ", "))
	}
	return nil
}

// checkHost checks one inventory node's name and connection fields.
func checkManagedNode(id string, h Node) error {
	if !modules.Name.MatchString(id) {
		return fmt.Errorf("invalid node name %q", id)
	}
	if id == "all" {
		// A selection of "all" means every node of the inventory, so a node
		// with that name could never be selected alone.
		return fmt.Errorf("node name %q is reserved: selecting all selects every node of the inventory; give the node another name", id)
	}
	if h.Address == "" || strings.HasPrefix(h.Address, "-") || strings.ContainsAny(h.Address, "\n\r\x00 \t") {
		return modules.Invalid("address", "node address must not be empty, start with '-', or contain spaces, tabs, line breaks, or a NUL byte")
	}
	if h.User != "" && !modules.Name.MatchString(h.User) {
		return modules.Invalid("ssh_user", "ssh_user must start with a letter or underscore and contain only letters, digits, underscores, or hyphens")
	}
	if h.Port < 0 || h.Port > 65535 {
		return modules.Invalid("ssh_port", "ssh_port must be a port number from 1 to 65535")
	}
	for field, name := range map[string]string{"ssh_password": h.SSHPassword, "sudo_password": h.SudoPassword} {
		if name != "" && modules.ConnectionSecretName(name) == "" {
			return modules.Invalid(field, `must be a whole-value secret reference such as "{{ .secrets.ssh_password }}"`)
		}
	}
	if h.Transport != "" && h.Transport != "https_with_ssh_fallback" && h.Transport != "ssh_only" {
		return modules.Invalid("connection", "connection must be https_with_ssh_fallback or ssh_only")
	}
	if h.Endpoint != "" {
		u, err := url.Parse(h.Endpoint)
		if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
			return modules.Invalid("https_address", "https_address must contain https:// and a host name, with an optional port and trailing slash, but no credentials, other path, query, or fragment")
		}
	}
	if h.RunnerURL != "" {
		u, err := url.Parse(h.RunnerURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || strings.ContainsAny(h.RunnerURL, "\n\r\x00 \t") {
			return modules.Invalid("mmx_runner_download_url", "mmx_runner_download_url must be an HTTP or HTTPS URL without credentials, query, or fragment")
		}
	}
	return nil
}

// readProjectFile names YAML locations relative to the project directory, so
// saved definitions, jobs, and messages say roles/app/main.yml, not a local path.
func readProjectFile(base, path string, dst any) error {
	name, err := filepath.Rel(base, path)
	if err != nil {
		return err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return Decode(name, b, dst)
}

// InventoryPath resolves a project's inventory from its project file.
func InventoryPath(projectFile, inventory string) string {
	return filepath.Join(filepath.Dir(projectFile), "inventories", inventory+".yml")
}

// VaultPath chooses the inventory's vault, else the project's, else secrets_vault.age
// beside the project file. A relative vault path starts at the file naming it.
func VaultPath(projectFile string, p Project, inventoryFile string, inv Inventory) string {
	switch {
	case inv.Vault != "":
		return join(filepath.Dir(inventoryFile), inv.Vault)
	case p.Vault != "":
		return join(filepath.Dir(projectFile), p.Vault)
	}
	return filepath.Join(filepath.Dir(projectFile), "secrets_vault.age")
}

func join(base, path string) string {
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	return filepath.Join(base, path)
}
func contains(xs []string, x string) bool {
	for _, a := range xs {
		if a == x {
			return true
		}
	}
	return false
}
func Select(inv Inventory, selector any) ([]string, error) {
	nodes, err := selectMaybeNone(inv, selector)
	if err == nil && len(nodes) == 0 {
		return nil, fmt.Errorf("node selection is empty")
	}
	return nodes, err
}

// selectMaybeNone resolves a selector like Select but accepts one that selects
// no node, as except_nodes may: an empty group or list removes nothing.
func selectMaybeNone(inv Inventory, selector any) ([]string, error) {
	var selectors []string
	switch v := selector.(type) {
	case string:
		selectors = []string{v}
	case []any:
		for _, x := range v {
			s, ok := x.(string)
			if !ok {
				return nil, fmt.Errorf("nodes selector must be a string or list of strings")
			}
			selectors = append(selectors, s)
		}
	case []string:
		selectors = v
	default:
		return nil, fmt.Errorf("nodes selector must be a string or list of strings")
	}
	selected := map[string]bool{}
	for _, s := range selectors {
		if s == "all" {
			for h := range inv.Nodes {
				selected[h] = true
			}
		} else if _, ok := inv.Nodes[s]; ok {
			selected[s] = true
		} else if group, ok := inv.Groups[s]; ok {
			for _, h := range group.Nodes {
				selected[h] = true
			}
		} else {
			return nil, &UnknownNodeError{Name: s}
		}
	}
	var result []string
	for h := range selected {
		result = append(result, h)
	}
	sort.Strings(result)
	return result, nil
}

// UnknownNodeError says that a selector names no node or group of the
// inventory.
type UnknownNodeError struct{ Name string }

func (e *UnknownNodeError) Error() string {
	return fmt.Sprintf("no node or inventory group named %q", e.Name)
}

// Limit is the set of nodes a --limit keeps; nil keeps every node.
type Limit map[string]bool

// NewLimit resolves --limit selectors against the inventory.
func NewLimit(inv Inventory, selectors []string) (Limit, error) {
	if len(selectors) == 0 {
		return nil, nil
	}
	nodes, err := Select(inv, selectors)
	if err != nil {
		return nil, fmt.Errorf("--limit: %w", err)
	}
	limit := Limit{}
	for _, h := range nodes {
		limit[h] = true
	}
	return limit, nil
}

// Narrow splits a fleet stage's selected nodes into those the limit keeps and those
// it leaves out.
func (l Limit) Narrow(selected []string) (kept, leftOut []string) {
	if l == nil {
		return selected, nil
	}
	for _, h := range selected {
		if l[h] {
			kept = append(kept, h)
		} else {
			leftOut = append(leftOut, h)
		}
	}
	return kept, leftOut
}

// prepare checks and completes one task list. phase is the task block phase
// the list is, or "" for a fleet stage's or a role's node tasks.
func prepare(nodeTasks []Task, phase, base, projectBase string, allowNested bool, defaults Defaults, roleStack []string, ids map[string]bool) error {
	for i := range nodeTasks {
		t := &nodeTasks[i]
		// Every check below reads the module's own input type.
		if err := convertWrittenInputs(t); err != nil {
			return t.Sources.Error(t.Module, err)
		}
		if command, _ := TrackedInputFollower(*t); command != nil {
			if phase != "activate" {
				return t.Sources.Error("shell.only_when_tracked_inputs_changed", fmt.Errorf("only_when_tracked_inputs_changed requires a shell node task in a node task block's activate phase; the files, packages, and checkouts in changes are the block's tracked inputs"))
			}
			if t.ID == "" {
				return t.Sources.Error("shell.only_when_tracked_inputs_changed", fmt.Errorf("a shell node task with only_when_tracked_inputs_changed needs an id; MMX stores the command record on the node using the node task block id and the shell node task id"))
			}
		}
		if phase == ChecksPhase {
			if err := checkChangesNothing(*t); err != nil {
				return t.Sources.Error("", err)
			}
		}
		// A module input may belong in some node task block phases only.
		if _, inputs := t.ModuleCall(); inputs != nil {
			if checked, ok := inputs.(interface{ CheckPhase(phase string) error }); ok {
				if err := checked.CheckPhase(phase); err != nil {
					return t.Sources.Error(t.Module, err)
				}
			}
		}
		if err := checkWholeValueReferences(*t); err != nil {
			return err
		}
		if t.CallsModuleThatIsNotCore() {
			module, err := findModule(projectBase, OfficialExtraModulesDirectory(), t.Module)
			if err != nil {
				return t.Sources.Error(t.Module, err)
			}
			if problem := module.Description.InputsProblem(t.Module, t.NonCoreModuleInputs()); problem != nil {
				return t.Sources.Error(t.Module, problem)
			}
			t.NonCoreModule = &module
		}
		if err := resolveTiming(t, defaults); err != nil {
			return t.Sources.Error("", err)
		}
		if t.ID != "" {
			if !modules.Name.MatchString(t.ID) || ids[t.ID] {
				return t.Sources.Error("id", fmt.Errorf("invalid or duplicate node task id %q", t.ID))
			}
			ids[t.ID] = true
		}
		if t.Role == "" && t.Inputs != nil {
			return t.Sources.Error("inputs", fmt.Errorf("inputs belongs to a role call; a module's inputs are written below the module's name"))
		}
		if t.CallsModuleThatIsNotCore() && !modules.ModuleName.MatchString(t.Module) {
			return t.Sources.Error(t.Module, fmt.Errorf("%s is not a node task setting or a core module; a module of the project's own must have a name that starts with a lowercase letter, contains only lowercase letters, digits, or underscores, and has at most 64 characters", t.Module))
		}
		if t.Role != "" {
			if !modules.Name.MatchString(t.Role) {
				return t.Sources.Error("role", fmt.Errorf("invalid role name %q", t.Role))
			}
			if len(roleStack) > 0 && !allowNested {
				return t.Sources.Error("role", fmt.Errorf("role calls within roles require allow_role_calls_within_roles: true"))
			}
			if contains(roleStack, t.Role) {
				return t.Sources.Error("role", fmt.Errorf("recursive role call: %s", t.Role))
			}
			var r Role
			dir := filepath.Join(projectBase, "roles", t.Role)
			if err := readProjectFile(projectBase, filepath.Join(dir, "main.yml"), &r); err != nil {
				return t.Sources.Error("role", err)
			}
			if len(r.NodeTasks) == 0 {
				return r.Sources.Error("node_tasks", fmt.Errorf("role %s: node_tasks must not be empty", t.Role))
			}
			for _, k := range sortedKeys(r.Inputs) {
				in := r.Inputs[k]
				// A default is literal: a reference would resolve in the caller's scope.
				if modules.Dynamic(in.Default) {
					return in.Sources.Error("default", fmt.Errorf("role %s: input %s default must be a literal value; pass references through the role call's inputs mapping", t.Role, k))
				}
				if in.Default != nil && !InputType(in.Default, in.Type) {
					return in.Sources.Error("default", fmt.Errorf("input %s must be %s", k, in.Type))
				}
				if in.Secret && in.Default != nil {
					return in.Sources.Error("default", fmt.Errorf("role %s: secret input %s must come from an explicit input, not a plaintext default", t.Role, k))
				}
				if !modules.Name.MatchString(k) {
					return in.Sources.Error("", fmt.Errorf("role %s: invalid input name %s", t.Role, k))
				}
				if !contains([]string{"string", "integer", "boolean", "list", "mapping"}, in.Type) {
					return in.Sources.Error("type", fmt.Errorf("role %s: input %s needs type string, integer, boolean, list, or mapping", t.Role, k))
				}
				if in.Required && in.Default == nil {
					if _, ok := t.Inputs[k]; !ok {
						return t.Sources.Error("inputs", fmt.Errorf("role %s: missing input %s", t.Role, k))
					}
				}
			}
			// A literal input is checked here, in every branch; references
			// are checked where they resolve.
			for k, v := range t.Inputs {
				spec, ok := r.Inputs[k]
				if !ok {
					return t.Sources.Error("inputs."+k, fmt.Errorf("role %s: unknown input %s", t.Role, k))
				}
				if !modules.Dynamic(v) && !InputType(v, spec.Type) {
					return t.Sources.Error("inputs."+k, fmt.Errorf("input %s must be %s", k, spec.Type))
				}
			}
			if err := prepare(r.NodeTasks, "", dir, projectBase, allowNested, defaults, append(append([]string{}, roleStack...), t.Role), map[string]bool{}); err != nil {
				return err
			}
			t.RoleNodeTasks = r.NodeTasks
			t.RoleInputs = r.Inputs
			t.RoleOutputs = r.Outputs
			t.RoleSources = r.Sources
		}
		t.ModuleBase = base
		err := prepareModule(t, base)
		if err != nil && waitsForWholeValue(*t, err) && len(taskSources(t)) > 0 {
			// The check stopped Prepare before it read the task's files on
			// the controller machine; the node would get none of them.
			err = fmt.Errorf("%w; the node task uses files read on the controller machine while loading the project; write the failing field's value directly in the node task", err)
		} else if waitsForWholeValue(*t, err) {
			err = nil
		}
		if err != nil {
			action, _ := t.Action()
			return t.Sources.Error(action.Name, err)
		}
		if err := checkLiterals(*t); err != nil {
			return err
		}
		if t.TaskBlock != nil {
			if !modules.Name.MatchString(t.TaskBlock.ID) {
				return t.TaskBlock.Sources.Error("id", fmt.Errorf("invalid node task block id"))
			}
			for _, validator := range t.TaskBlock.Validate {
				if !validator.CoreModule().ChecksAConfiguration {
					return validator.Sources.Error("", fmt.Errorf("the node task block's validate phase accepts only shell or http node tasks"))
				}
			}
			if len(t.TaskBlock.Changes) == 0 {
				return t.TaskBlock.Sources.Error("changes", fmt.Errorf("the node task block requires at least one node task in changes"))
			}
			if len(t.TaskBlock.ChecksBeforeRestartOrReload) > 0 && !TrackingTaskBlock(*t) {
				return t.TaskBlock.Sources.Error(ChecksPhase, fmt.Errorf("checks_before_restart_or_reload would never run: the node task block's activate phase has no systemd_service node task or shell node task with only_when_tracked_inputs_changed"))
			}
		}
		for _, list := range t.Children() {
			if list.Phase == "" || len(list.NodeTasks) == 0 {
				continue // Role tasks were prepared from the role's directory above.
			}
			for _, child := range list.NodeTasks {
				if child.TaskBlock != nil || child.Role != "" {
					return child.Sources.Error("", fmt.Errorf("node task block phases accept individual node tasks; put role calls and nested node task blocks in the fleet stage's or role's node_tasks list"))
				}
			}
			if err := prepare(list.NodeTasks, list.Phase, base, projectBase, allowNested, defaults, roleStack, ids); err != nil {
				return err
			}
		}
	}
	return nil
}

// checkCommandRecords rejects two different commands that would share one
// record on a node. A command with only_when_tracked_inputs_changed keeps its
// record under the project's name and its result path: the ids of the role
// calls around it, its task block, and the task, and in a loop the
// iteration's key. The same task reached twice, as when two fleet stages or
// two playbooks call one role, is one command with one record. seen holds the
// paths found so far, also those of other playbooks.
func checkCommandRecords(stages []FleetStage, seen map[string]Location) error {
	var walk func(nodeTasks []Task, prefix string) error
	walk = func(nodeTasks []Task, prefix string) error {
		for i, t := range nodeTasks {
			name := prefix + TaskName(t, i)
			if t.Loop != nil && t.Key == nil && holdsCommandWithRecord(t) {
				return t.Sources.Error("loop", fmt.Errorf("a loop whose iterations run a shell command with only_when_tracked_inputs_changed requires key: the node keeps each iteration's command record under the iteration's key, and without key a list item is named by its position, so adding or removing an item would hand one item's record, and a command still owed, to another item"))
			}
			if command, _ := TrackedInputFollower(t); command != nil {
				at := t.Sources.At("")
				if first, ok := seen[name]; ok && first != at {
					return t.Sources.Error("id", fmt.Errorf("the shell command and the shell command at %s would share a command record on the node because the commands have the task path %s; change a node task block id or shell node task id to give each command a distinct path", first, name))
				}
				seen[name] = at
			}
			for _, list := range t.Children() {
				nested := name + "/"
				if list.Phase != "" {
					nested += list.Phase + "/"
				}
				if err := walk(list.NodeTasks, nested); err != nil {
					return err
				}
			}
		}
		return nil
	}
	for _, s := range stages {
		if err := walk(s.NodeTasks, ""); err != nil {
			return err
		}
		if err := checkCommandRecords(s.FleetStages, seen); err != nil {
			return err
		}
	}
	return nil
}

// holdsCommandWithRecord reports whether a task is, or contains, a shell
// command with only_when_tracked_inputs_changed.
func holdsCommandWithRecord(t Task) bool {
	if command, _ := TrackedInputFollower(t); command != nil {
		return true
	}
	for _, list := range t.Children() {
		if slices.ContainsFunc(list.NodeTasks, holdsCommandWithRecord) {
			return true
		}
	}
	return false
}

// checkEnabledOnQuadletServices rejects start_at_boot on a systemd_service task
// whose unit is the generated service of a podman_quadlet task among the
// node tasks of the same fleet stage, in the same systemd. systemd can
// neither enable nor disable a generated unit. On the node only a run could
// say so, after it installed the Quadlet files: before that, systemd has no
// such unit, and a dryrun has nothing to look at. Names that hold a
// reference are compared on the node.
func checkEnabledOnQuadletServices(stages []FleetStage) error {
	// service names the generated service within its systemd.
	service := func(account, unit string) string { return account + "\x00" + unit }
	var walk func(nodeTasks []Task, visit func(Task) error) error
	walk = func(nodeTasks []Task, visit func(Task) error) error {
		for _, t := range nodeTasks {
			if err := visit(t); err != nil {
				return err
			}
			for _, list := range t.Children() {
				if err := walk(list.NodeTasks, visit); err != nil {
					return err
				}
			}
		}
		return nil
	}
	for _, s := range stages {
		quadlets := map[string]Task{}
		_ = walk(s.NodeTasks, func(t Task) error {
			_, inputs := t.ModuleCall()
			if q, ok := inputs.(*modules.PodmanQuadlet); ok && (q.State == "" || q.State == modules.QuadletPresent) && !modules.Dynamic(q.Name) {
				quadlets[service(q.InSystemdOfUserAccount, q.GeneratedService())] = t
			}
			return nil
		})
		err := walk(s.NodeTasks, func(t Task) error {
			_, inputs := t.ModuleCall()
			sv, ok := inputs.(*modules.Service)
			if !ok || sv.StartAtBoot == nil || modules.Dynamic(sv.Name) {
				return nil
			}
			unit := modules.UnitName(sv.Name)
			quadlet, generated := quadlets[service(sv.InSystemdOfUserAccount, unit)]
			if !generated {
				return nil
			}
			_, inputs = quadlet.ModuleCall()
			file := inputs.(*modules.PodmanQuadlet).Name
			return t.Sources.Error("systemd_service.start_at_boot", fmt.Errorf("%w; the podman_quadlet node task at %s installs the Quadlet file", modules.EnabledOnGeneratedUnit(unit, file), quadlet.Sources.At("")))
		})
		if err != nil {
			return err
		}
		if err := checkEnabledOnQuadletServices(s.FleetStages); err != nil {
			return err
		}
	}
	return nil
}

// checkChangesNothing accepts the node tasks a checks_before_restart_or_reload
// phase may hold: those that only read.
func checkChangesNothing(t Task) error {
	if t.Show != nil || t.CoreModule().OnlyReads {
		return nil
	}
	if _, inputs := t.ModuleCall(); inputs != nil {
		if when, ok := inputs.(modules.OnlyReadsWhen); ok {
			return when.OnlyReads()
		}
	}
	return fmt.Errorf("checks_before_restart_or_reload accepts only tasks that change nothing: assert, http with GET or HEAD, wait_for, show, and shell with changed_when: false")
}

// ValidateProject checks every inventory and every playbook/inventory
// pairing, with the one-time playbooks that are not completed. It takes no
// --var values, so a command-line variable without a default is not known
// (Project.WholeProjectValidation).
func ValidateProject(path string) (Project, error) {
	p, err := ReadSettings(path)
	if err != nil {
		return p, err
	}
	inventories, err := ProjectNames(path, "inventories")
	if err != nil {
		return p, err
	}
	if !slices.Contains(inventories, p.Inventory) {
		return p, p.Sources.Error("default_inventory", fmt.Errorf("there is no inventory %s: inventories/%s.yml does not exist; the project's inventories are: %s", p.Inventory, p.Inventory, namesOrNone(inventories)))
	}
	playbooks, err := projectPlaybooks(path)
	if err != nil {
		return p, err
	}
	for _, inventory := range inventories {
		if _, _, err := Load(path, "", inventory, nil); err != nil {
			return p, err
		}
		commands := map[string]Location{}
		for _, playbook := range playbooks {
			book, _, err := load(path, playbook, inventory, nil, nil, true)
			if err != nil {
				return p, err
			}
			if err := checkCommandRecords(book.FleetStages, commands); err != nil {
				return p, err
			}
		}
	}
	return p, nil
}

// convertWrittenInputs turns a module's inputs from the form the task wrote
// them in into the module's input type, such as a download into the file it
// installs.
func convertWrittenInputs(t *Task) error {
	written, ok := t.ModuleInputs.(modules.WrittenInputs)
	if !ok {
		return nil
	}
	inputs, err := written.ModuleInputs()
	if err != nil {
		return err
	}
	t.ModuleInputs = inputs
	return nil
}

// checkTaskTypes checks a node task's settings by their types, and the one key
// that is no setting as the name of a module: a core module's inputs by the
// module's input type, and a module of the project's own's inputs as a
// mapping that the project's module checks.
func checkTaskTypes(n *yaml.Node, field string) error {
	settings := yamlFields(reflect.TypeOf(Task{}))
	for i := 0; i < len(n.Content); i += 2 {
		key, value := n.Content[i].Value, n.Content[i+1]
		path := fieldPath(field, key)
		if t, ok := settings[key]; ok {
			if err := checkTypes(value, t, path); err != nil {
				return err
			}
			continue
		}
		if m, core := modules.LookUpCoreModule(key); core {
			if err := checkTypesOf(value, reflect.TypeOf(m.NewWritten()), path, true); err != nil {
				return err
			}
			continue
		}
		if value.Kind != yaml.MappingNode && value.Tag != "!!null" {
			return fmt.Errorf("line %d column %d (%s): %s is not a node task setting or a core module; inputs for a module of the project's own must be a YAML mapping", value.Line, value.Column, path, key)
		}
	}
	return nil
}
