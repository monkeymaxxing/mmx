package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"

	"mmx/internal/assets"
	"mmx/internal/modules"
	"mmx/internal/value"
)

// A source or template path, and a download's web address and SHA-256, may
// contain references to values the controller machine knows before it
// connects: the node's name, vars, role inputs, and loop items. One task then
// installs a different file for each node or loop item. Before a run starts,
// the controller resolves the field for every node and iteration that can
// reach the task and saves one SourceVariant for each result: loading a
// project inspects it for every node of the inventory, and inspection records
// the variants. A node's job carries the variants of that node, and the node
// picks one by resolving the same field.

// keysVariantKey names a key variant by every resolved key path.
func keysVariantKey(k *modules.AuthorizedKeys) string {
	var parts []string
	for _, p := range modules.KeyPaths(k) {
		parts = append(parts, p.Field+"="+*p.Name)
	}
	return variantKey(parts...)
}

// taskSources lists each source field a task uses.
func taskSources(t *Task) []modules.ControllerFile {
	name, inputs := t.ModuleCall()
	m, ok := inputs.(interface {
		ControllerFiles() []modules.ControllerFile
	})
	if !ok {
		return nil
	}
	files := m.ControllerFiles()
	for i := range files {
		files[i].Field = name + "." + files[i].Field
	}
	return files
}

// variantKey names a variant by what the field resolved to.
func variantKey(resolved ...string) string { return strings.Join(resolved, " ") }

// notGivenToWholeProjectValidation ends the reason that a command-line
// variable without a default is unknown in mmx validate without a playbook
// name.
const notGivenToWholeProjectValidation = ", which mmx validate without a playbook name does not take"

// readsCommandLineValueNotGiven reports an error that only a command-line
// value missing from mmx validate without a playbook name causes. That
// validation checks the source field as far as it can, as it checks other
// values it cannot know yet; the commands that name the playbook require the
// value and check the field.
func readsCommandLineValueNotGiven(err error) bool {
	var unavailable *value.UnavailableError
	return errors.As(err, &unavailable) && strings.HasSuffix(unavailable.Reason, notGivenToWholeProjectValidation)
}

// loopItemReason is why a loop item is unknown during inspection, given the
// loop's error. An item over a command-line value missing from mmx validate
// without a playbook name keeps that reason, so the validation skips the
// source fields that read the item.
func loopItemReason(err error) string {
	var unavailable *value.UnavailableError
	if readsCommandLineValueNotGiven(err) && errors.As(err, &unavailable) {
		return unavailable.Reason
	}
	return "loop value resolved during execution"
}

// registerSources resolves a task's variable source fields for one node, in the
// scope inspection built for it, and records the file each resolves to. The
// error for a missing file, or for a value known only during a run, names the
// field; the caller names the node.
func registerSources(t Task, scope value.Scope, node string) error {
sources:
	for _, s := range taskSources(&t) {
		if !s.Dynamic() {
			continue
		}
		local := scope.Local()
		resolve := func(text string) (string, error) {
			resolved, err := local.Text(text)
			if err != nil {
				return "", t.Sources.Error(s.Field, fmt.Errorf("%w; the source field may use only values known on the controller machine before connecting to the node: the node's name, vars, role inputs, and loop items", err))
			}
			return resolved, nil
		}
		variant := &modules.SourceVariant{}
		var key string
		if s.Kind == modules.SourceKeys {
			k := *s.Keys
			k.KeyFilesOnControllerMachine, k.KeyDirectoriesOnControllerMachine = slices.Clone(k.KeyFilesOnControllerMachine), slices.Clone(k.KeyDirectoriesOnControllerMachine)
			paths := modules.KeyPaths(&k)
			for _, p := range paths {
				field := "authorized_keys." + p.Field
				resolved, err := local.Text(*p.Name)
				if readsCommandLineValueNotGiven(err) {
					continue sources
				}
				if err != nil {
					return t.Sources.Error(field, fmt.Errorf("%w; the source field may use only values known on the controller machine before connecting to the node: the node's name, vars, role inputs, and loop items", err))
				}
				*p.Name = resolved
			}
			keys, err := modules.ReadKeys(s.Base, paths)
			var located *modules.FieldError
			if errors.As(err, &located) {
				return t.Sources.Error("authorized_keys."+located.Field, located.Err)
			}
			if err != nil {
				return t.Sources.Error(s.Field, err)
			}
			// A key may repeat a written key or a key of another file.
			k.FileKeys = keys
			if _, err := modules.AuthorizedKeyList(k, k.Keys, func(_ int, line string) bool { return modules.Dynamic(line) }); err != nil {
				return t.Sources.Error("authorized_keys", err)
			}
			key, variant.Keys = keysVariantKey(&k), keys
		} else if s.Kind == modules.SourceDownload {
			url, err := resolve(*s.URL)
			if readsCommandLineValueNotGiven(err) {
				continue sources
			}
			if err != nil {
				return err
			}
			digest, err := resolve(*s.SHA256)
			if readsCommandLineValueNotGiven(err) {
				continue sources
			}
			if err != nil {
				return err
			}
			digest = strings.ToLower(digest)
			if assets.ValidateDownloadURL(url) != nil {
				return t.Sources.Error(s.Field, fmt.Errorf("%q is not an HTTPS URL without credentials or a fragment", url))
			}
			// Only dangerously_skip_checksum: true skips the check, and
			// loading refuses it beside a SHA-256 reference: a reference that
			// resolves to empty text would skip the check unwritten.
			if digestField := strings.TrimSuffix(s.Field, "url") + "sha256"; digest == "" && modules.Dynamic(*s.SHA256) {
				return SourceError(t.Sources, digestField, *s.SHA256, local, fmt.Errorf("%s resolves to empty text, so the download from %s would have no checksum check; provide a SHA-256 checksum as 64 hexadecimal digits, or explicitly skip the checksum check by replacing %s with dangerously_skip_checksum: true", digestField, url, digestField[strings.LastIndex(digestField, ".")+1:]))
			}
			if digest != "" && assets.ValidateDownload(url, digest) != nil {
				return t.Sources.Error(s.Field, fmt.Errorf("the SHA-256 for %s resolves to %q, which is not 64 hexadecimal digits", url, digest))
			}
			key, variant.URL, variant.SHA256 = variantKey(url, digest), url, digest
		} else {
			name, err := resolve(*s.Path)
			if readsCommandLineValueNotGiven(err) {
				continue sources
			}
			if err != nil {
				return err
			}
			key = variantKey(name)
			field := s.Field[strings.LastIndex(s.Field, ".")+1:]
			path, err := modules.ControllerPath(s.Base, field, name)
			if err != nil {
				return t.Sources.Error(s.Field, err)
			}
			variant.LocalPath = path
			switch s.Kind {
			case modules.SourceDirectory:
				if variant.Entries, err = s.ReadTree(path, name); err != nil {
					return t.Sources.Error(s.Field, err)
				}
			default:
				info, err := os.Lstat(path)
				if err != nil {
					return t.Sources.Error(s.Field, err)
				}
				if !info.Mode().IsRegular() {
					return t.Sources.Error(s.Field, fmt.Errorf("file source is not a regular file: %s", name))
				}
			}
			if s.Kind == modules.SourceTemplate {
				b, err := modules.ReadTemplate(path)
				if err != nil {
					return t.Sources.Error(s.Field, err)
				}
				variant.TemplateText = string(b)
				reads, err := value.TemplateReads(variant.TemplateText)
				if err != nil {
					return t.Sources.Error(s.Field, fmt.Errorf("%s: %w", name, err))
				}
				for _, read := range reads {
					if read == "." {
						continue
					}
					if _, err := scope.Get(read); err != nil && !unknown(err) {
						return t.Sources.Error(s.Field, fmt.Errorf("%s: %w", name, err))
					}
				}
			}
		}
		if *s.Variants == nil {
			*s.Variants = map[string]*modules.SourceVariant{}
		}
		if known := (*s.Variants)[key]; known != nil {
			variant = known
		} else {
			(*s.Variants)[key] = variant //nolint:nilaway // The preceding nil check allocates Variants before insertion.
		}
		if !slices.Contains(variant.Nodes, node) {
			variant.Nodes = append(variant.Nodes, node)
		}
	}
	return nil
}

// selectSources gives a task, whose arguments are resolved, the variant its
// variable source field resolved to. A node job runs it in the node's scope.
func selectSources(t *Task) error {
	for _, s := range taskSources(t) {
		if len(*s.Variants) == 0 {
			continue
		}
		key := ""
		if s.Kind == modules.SourceKeys {
			key = keysVariantKey(s.Keys)
		} else if s.Kind == modules.SourceDownload {
			key = variantKey(*s.URL, strings.ToLower(*s.SHA256))
		} else {
			key = variantKey(*s.Path)
		}
		variant := (*s.Variants)[key]
		if variant == nil {
			return t.Sources.Error(s.Field, fmt.Errorf("no file was prepared for %q before the playbook run started; source fields require values known on the controller machine before connecting to the node and cannot use values changed by an earlier node task", key))
		}
		if s.Asset != nil {
			*s.Asset = variant.Asset
		}
		if s.Template != nil {
			*s.Template = variant.TemplateText
		}
		if s.Entries != nil {
			*s.Entries = variant.Entries
		}
		if s.Keys != nil {
			s.Keys.FileKeys = variant.Keys
		}
		*s.Variants = nil
	}
	return nil
}

// withoutSourceVariants returns a copy of t whose variable source fields
// select no file when its arguments are resolved.
func withoutSourceVariants(t Task) Task {
	t.copyModuleInputs()
	for _, s := range taskSources(&t) {
		*s.Variants = nil
	}
	return t
}

// HasVariableSources reports whether any task below uses a variable source field.
func HasVariableSources(nodeTasks []Task) bool {
	for i := range nodeTasks {
		for _, s := range taskSources(&nodeTasks[i]) {
			if s.Dynamic() {
				return true
			}
		}
		for _, list := range nodeTasks[i].Children() {
			if HasVariableSources(list.NodeTasks) {
				return true
			}
		}
	}
	return false
}

// KeepSourcesOf leaves in nodeTasks, a copy made for one node's job, only the
// variants that node can use, so its job carries and uploads no other node's files.
func KeepSourcesOf(nodeTasks []Task, node string) {
	for i := range nodeTasks {
		for _, s := range taskSources(&nodeTasks[i]) {
			for key, variant := range *s.Variants {
				if !slices.Contains(variant.Nodes, node) && !slices.Contains(variant.Nodes, "") {
					delete(*s.Variants, key)
				} else {
					variant.Nodes = nil
				}
			}
		}
		for _, list := range nodeTasks[i].Children() {
			KeepSourcesOf(list.NodeTasks, node)
		}
	}
}

// ProjectHasVariableSources reports whether any fleet stage uses a variable source field.
func ProjectHasVariableSources(p Project) bool {
	found := false
	// The fleet-stage callback always returns nil and traversal has no other error source.
	_ = WalkFleetStages(p.FleetStages, func(_ string, s FleetStage) error {
		found = found || HasVariableSources(s.NodeTasks)
		return nil
	})
	return found
}

// NodeTasksFor returns a copy of a fleet stage's node tasks for one node's job:
// each variable source field keeps only the files that node can select, bound
// to the run's saved assets in dir.
func NodeTasksFor(nodeTasks []Task, node, dir string) ([]Task, error) {
	saved, err := json.Marshal(nodeTasks)
	if err != nil {
		return nil, err
	}
	var own []Task
	if err := json.Unmarshal(saved, &own); err != nil {
		return nil, err
	}
	KeepSourcesOf(own, node)
	return own, BindAssets(own, nil, dir)
}
