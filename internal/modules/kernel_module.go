package modules

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"

	"mmx/internal/protocol"
	"mmx/internal/value"
)

// The kernel lists its loaded modules in /proc/modules and the options of a
// module under /sys/module/<name>/parameters/. The modules built into the
// running kernel are listed in /lib/modules/<release>/modules.builtin.
// systemd-modules-load loads the modules named in /etc/modules-load.d at
// boot, and modprobe reads the options lines of /etc/modprobe.d whenever it
// loads a module. Tests point these paths elsewhere.
var (
	procModulesFile           = "/proc/modules"
	moduleParametersDirectory = "/sys/module"
	kernelModulesDirectory    = "/lib/modules"
	modulesLoadDirectory      = "/etc/modules-load.d"
	modprobeDirectory         = "/etc/modprobe.d"
)

// KernelModule loads one kernel module now and at every boot, with its
// options, or unloads it.
func (run Runtime) KernelModule(ctx context.Context, m KernelModule) (result protocol.Result) {
	if err := ValidateKernelModule(m); err != nil {
		return protocol.ConfigurationError(err)
	}
	if _, err := lookPath(ctx, "modprobe"); err != nil {
		return run.notYet(missingProgramError("modprobe"))
	}
	// The kernel writes a module's name with underscores; modprobe takes a
	// dash for an underscore.
	name := strings.ReplaceAll(m.Name, "-", "_")
	builtIn, err := builtIntoKernel(name)
	if err != nil {
		return protocol.Failed(err)
	}
	loaded, err := moduleLoaded(name)
	if err != nil {
		return protocol.Failed(err)
	}
	loadFile := filepath.Join(modulesLoadDirectory, "mmx-"+name+".conf")
	optionsFile := filepath.Join(modprobeDirectory, "mmx-"+name+".conf")
	files := kernelModuleFiles{run: run}
	defer func() {
		if files.changed && result.Change != "changed" {
			result.Change = map[bool]string{true: "would_change", false: "changed"}[run.Dryrun]
		}
		if len(files.paths) > 0 || result.Value != nil {
			v, _ := result.Value.(map[string]any)
			if v == nil {
				v = map[string]any{}
			}
			if len(files.paths) > 0 {
				v["files_changed"] = files.paths
			}
			result.Value = v
		}
		result.Diff = files.diff.String()
	}()

	if m.State == "unloaded" {
		if builtIn {
			return protocol.ConfigurationError(fmt.Errorf("the kernel module %s is built into the kernel and cannot be unloaded", name))
		}
		for _, path := range []string{optionsFile, loadFile} {
			if r := files.write(ctx, File{Path: path, State: "absent"}); r != nil {
				return *r
			}
		}
		if !loaded {
			return protocol.Success("unchanged")
		}
		change := map[string]any{"loaded_before": true, "loaded_after": false}
		if run.Dryrun {
			r := protocol.Success("would_change")
			r.Value = change
			return r
		}
		if r := run.modprobe(ctx, "-r", name); r != nil {
			return *r
		}
		if loaded, err = moduleLoaded(name); err == nil && loaded {
			err = fmt.Errorf("modprobe -r %s reported success, but %s still lists the module", name, procModulesFile)
		}
		if err != nil {
			return protocol.Failed(err)
		}
		r := protocol.Success("changed")
		r.Value = change
		return r
	}

	if builtIn && len(m.Options) > 0 {
		return protocol.ConfigurationError(fmt.Errorf("the kernel module %s is built into the kernel, so modprobe cannot set the module options; set the options on the kernel command line instead, as %s", name, strings.Join(kernelCommandLineOptions(name, m.Options), " ")))
	}
	if !builtIn && !loaded {
		// Before any change, so that a misspelled name is never kept in a file
		// that fails systemd-modules-load at boot. A module that does not exist
		// yet may come with a package an earlier task installs.
		if out := run.RunCommand(ctx, []string{"modprobe", "--dry-run", name}, ""); out.Error != "" || out.ExitCode != 0 {
			return run.notYet(fmt.Errorf("modprobe cannot load the kernel module %s for the running kernel: %s", name, modprobeMessage(strings.TrimSpace(out.StandardError), strings.TrimSpace(out.StandardOutput), out.Error)))
		}
	}
	// A module already loaded keeps the options it was loaded with.
	var different []string
	if loaded {
		different = moduleOptionsDifferent(name, m.Options)
	}
	loadLine := name + "\n"
	if r := files.write(ctx, File{Path: loadFile, Literal: &loadLine, Mode: "0644"}); r != nil {
		return *r
	}
	options := File{Path: optionsFile, State: "absent"}
	if len(m.Options) > 0 {
		optionsLine := "options " + name + " " + strings.Join(moduleOptionWords(m.Options), " ") + "\n"
		options = File{Path: optionsFile, Literal: &optionsLine, Mode: "0644"}
	}
	if m.Options != nil {
		if r := files.write(ctx, options); r != nil {
			return *r
		}
	}
	if len(different) > 0 {
		// Another try would find the module loaded with the same options.
		return protocol.ConfigurationError(fmt.Errorf("the kernel module %s is loaded with options that differ from the node task (%s); MMX leaves the loaded module in place because the module may be in use; MMX %s the requested options to %s for the next module load; unload the module with modprobe -r %s or a kernel_module node task with state: unloaded, then run the playbook again, or reboot the node", name, strings.Join(different, "; "), map[bool]string{true: "would write", false: "wrote"}[run.Dryrun], optionsFile, name))
	}
	if builtIn || loaded {
		return protocol.Success("unchanged")
	}
	change := map[string]any{"loaded_before": false, "loaded_after": true}
	if run.Dryrun {
		r := protocol.Success("would_change")
		r.Value = change
		return r
	}
	if r := run.modprobe(ctx, name); r != nil {
		return *r
	}
	if loaded, err = moduleLoaded(name); err == nil && !loaded {
		err = fmt.Errorf("modprobe %s reported success, but %s does not list the module; a line such as \"install %s /bin/true\" in /etc/modprobe.d may keep it from loading", name, procModulesFile, name)
	}
	if err == nil {
		if different = moduleOptionsDifferent(name, m.Options); len(different) > 0 {
			err = fmt.Errorf("the kernel module %s was loaded, but the loaded options differ from the node task (%s); another options line in /etc/modprobe.d may override the requested options", name, strings.Join(different, "; "))
		}
	}
	if err != nil {
		r := protocol.Failed(err)
		r.Change = "changed"
		return r
	}
	r := protocol.Success("changed")
	r.Value = change
	return r
}

// modprobeMessage is the first of modprobe's messages that is not empty.
func modprobeMessage(texts ...string) string {
	for _, text := range texts {
		if text != "" {
			return text
		}
	}
	return ""
}

// modprobe runs modprobe with arguments and returns a failed result with its
// message, or nil when it succeeded.
func (run Runtime) modprobe(ctx context.Context, arguments ...string) *protocol.Result {
	out := run.RunCommandThatChangesTheNode(ctx, append([]string{"modprobe"}, arguments...), "")
	if out.Error == "" && out.ExitCode == 0 {
		return nil
	}
	r := protocol.Failed(fmt.Errorf("modprobe %s failed: %s", strings.Join(arguments, " "), modprobeMessage(strings.TrimSpace(out.StandardError), strings.TrimSpace(out.StandardOutput), out.Error)))
	return &r
}

// kernelModuleFiles writes a kernel_module task's files in turn and gathers
// what changed: the paths, and one difference that names each file.
type kernelModuleFiles struct {
	run     Runtime
	changed bool
	paths   []string
	diff    strings.Builder
}

// write installs or removes one file and returns a failed result, or nil.
func (f *kernelModuleFiles) write(ctx context.Context, file File) *protocol.Result {
	r := f.run.File(ctx, file, value.Scope{})
	if r.Status != "succeeded" {
		if f.changed {
			r.Change = "changed"
		}
		return &r
	}
	if r.Change == "unchanged" {
		return nil
	}
	f.changed = true
	f.paths = append(f.paths, file.Path)
	if c := r.FileChange; c != nil && c.Diff != "" {
		f.diff.WriteString(strings.Replace(c.Diff, "--- before\n+++ after\n", "--- "+file.Path+"\n+++ "+file.Path+"\n", 1))
	} else if c != nil {
		f.diff.WriteString(c.Action + " " + file.Path + "\n")
	}
	return nil
}

// moduleLoaded reads /proc/modules.
func moduleLoaded(name string) (bool, error) {
	f, err := os.Open(procModulesFile)
	if err != nil {
		return false, err
	}
	defer f.Close()
	lines := bufio.NewScanner(f)
	for lines.Scan() {
		if fields := strings.Fields(lines.Text()); len(fields) > 0 && fields[0] == name {
			return true, nil
		}
	}
	return false, lines.Err()
}

// builtIntoKernel reads the running kernel's modules.builtin. Without the
// file, such as in a container, no module counts as built in.
func builtIntoKernel(name string) (bool, error) {
	release, err := os.ReadFile(filepath.Join(kernelSettingsDirectory, "kernel", "osrelease"))
	if err != nil {
		return false, err
	}
	f, err := os.Open(filepath.Join(kernelModulesDirectory, strings.TrimSpace(string(release)), "modules.builtin"))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer f.Close()
	lines := bufio.NewScanner(f)
	for lines.Scan() {
		// kernel/net/bridge/br_netfilter.ko
		if strings.ReplaceAll(strings.TrimSuffix(filepath.Base(strings.TrimSpace(lines.Text())), ".ko"), "-", "_") == name {
			return true, nil
		}
	}
	return false, lines.Err()
}

// moduleOptionsDifferent compares the options with the values the loaded
// module shows under /sys/module/<name>/parameters/, and describes each one
// that differs. An option the module does not show there is not compared.
func moduleOptionsDifferent(name string, options map[string]*string) []string {
	var different []string
	for _, option := range slices.Sorted(maps.Keys(options)) {
		b, err := os.ReadFile(filepath.Join(moduleParametersDirectory, name, "parameters", strings.ReplaceAll(option, "-", "_")))
		if err != nil {
			continue
		}
		shown, want := strings.TrimSpace(string(b)), *options[option] //nolint:nilaway // KernelModule calls ValidateKernelModule, which rejects every nil option value before comparison.
		if !sameModuleOption(shown, want) {
			different = append(different, fmt.Sprintf("%s is %s, not %s", option, shown, want))
		}
	}
	return different
}

// sameModuleOption compares an option's value with the one the kernel shows:
// a yes-or-no option shows Y or N whichever of the kernel's spellings set it.
func sameModuleOption(shown, want string) bool {
	if shown == "Y" || shown == "N" {
		if yes, ok := kernelBool(want); ok {
			return yes == (shown == "Y")
		}
	}
	return slices.Equal(strings.Fields(shown), strings.Fields(want))
}

// kernelBool reads a yes-or-no value as the kernel does (kstrtobool).
func kernelBool(s string) (yes, ok bool) {
	switch {
	case s == "":
		return false, false
	case strings.ContainsRune("yY1", rune(s[0])):
		return true, true
	case strings.ContainsRune("nN0", rune(s[0])):
		return false, true
	case len(s) > 1 && strings.ContainsRune("oO", rune(s[0])) && strings.ContainsRune("nN", rune(s[1])):
		return true, true
	case len(s) > 1 && strings.ContainsRune("oO", rune(s[0])) && strings.ContainsRune("fF", rune(s[1])):
		return false, true
	}
	return false, false
}

// moduleOptionWords are the options as modprobe's options line writes them,
// in name order.
func moduleOptionWords(options map[string]*string) []string {
	var words []string
	for _, option := range slices.Sorted(maps.Keys(options)) {
		words = append(words, option+"="+*options[option]) //nolint:nilaway // ValidateKernelModule rejects nil option values before either option-formatting caller runs.
	}
	return words
}

func kernelCommandLineOptions(name string, options map[string]*string) []string {
	words := moduleOptionWords(options)
	for i, word := range words {
		words[i] = name + "." + word
	}
	return words
}

// KernelModule is one loadable kernel module.
type KernelModule struct {
	Name string `yaml:"name" json:"name"`
	// Options are the module's options by name, such as hashsize: "65536".
	Options map[string]*string `yaml:"options,omitempty" json:"options"`
	State   string             `yaml:"state,omitempty" json:"state,omitempty"`
}

var (
	kernelModuleName   = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
	kernelModuleOption = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
)

func ValidateKernelModule(m KernelModule) error {
	if m.State != "" && !slices.Contains(kernelModuleStates, m.State) {
		return invalid("state", "kernel_module.state must be loaded or unloaded")
	}
	if m.Name == "" {
		return invalid("name", "kernel_module.name is required, such as br_netfilter")
	}
	if m.State == "unloaded" && m.Options != nil {
		return fmt.Errorf("kernel_module with state: unloaded accepts only name and state")
	}
	for _, option := range slices.Sorted(maps.Keys(m.Options)) {
		if !kernelModuleOption.MatchString(option) {
			return invalid("options", fmt.Sprintf("kernel_module option %q must be an option name as modinfo lists it: letters, digits, '_', and '-'", option))
		}
		if m.Options[option] == nil {
			return invalid("options."+option, "kernel_module option "+option+" needs a value, such as \"65536\"")
		}
	}
	return nil
}

var (
	kernelModuleNameRule = rule("name must be a kernel module's name as lsmod or modinfo shows it, such as br_netfilter: letters, digits, '_', and '-'", kernelModuleName.MatchString)
	// One word of modprobe's options line.
	kernelModuleOptionRule = rule(`an option's value must be one word without spaces or quotes, as modinfo describes it, such as "65536" or "1"`, func(s string) bool {
		return s != "" && !strings.ContainsFunc(s, func(r rune) bool { return r <= ' ' || r == 0x7f || r == '"' || r == '\'' })
	})
)

// kernelModuleDescription is what mmx describe-module shows for kernel_module.
var kernelModuleDescription = Description{
	Name: "kernel_module", Topic: TopicKernelSettingsAndKernelModules, Schema: reflect.TypeOf(KernelModule{}),
	Summary: "one loadable kernel module, loaded now with modprobe and at every boot from /etc/modules-load.d/mmx-<name>.conf, with its options in /etc/modprobe.d/mmx-<name>.conf; or unloaded",
	Fields: []Field{
		{Name: "name", Required: true, Notes: "as lsmod shows it, such as br_netfilter; a module that does not exist for the running kernel fails the task with modprobe's message; a module built into the kernel counts as loaded"},
		{Name: "options", Type: "mapping of option name to text", Notes: `omitted keeps the saved options file; {} deletes the saved options file. For example: {hashsize: "65536"}. MMX saves the options in one options line for modprobe and compares the requested values with /sys/module/<name>/parameters/. A loaded module keeps the current option values until the module is loaded again. If the loaded values differ, the node task writes the files and fails with instructions to unload the module or reboot. A module built into the kernel refuses options because built-in module options belong on the kernel command line.`},
		{Name: "state", Values: kernelModuleStates, Default: "loaded", Notes: "unloaded accepts only name; deletes MMX's two files and unloads the module with modprobe -r; a module in use fails the task with modprobe's message"},
	},
	Example: "- kernel_module:\n    name: nf_conntrack\n    options:\n      hashsize: \"65536\"\n",
	Results: []string{"status", "change", "value.loaded_before", "value.loaded_after", "value.files_changed"},
	Dryrun:  "supported; reports whether the module would be loaded or unloaded, and the difference of each file; a module modprobe cannot find yet is not checked, because an earlier task may install it; needs modprobe from the package kmod",
}

// Prepare checks the inputs when the project loads.
func (m *KernelModule) Prepare(base string) error {
	return ValidateKernelModule(*m)
}

// Arguments lists the inputs that may hold a reference, with their rules.
func (m *KernelModule) Arguments(arguments *ArgumentList) {
	arguments.Public("name", &m.Name, kernelModuleNameRule)
	for _, option := range slices.Sorted(maps.Keys(m.Options)) {
		if m.Options[option] != nil {
			arguments.Public("options."+option, m.Options[option], kernelModuleOptionRule)
		}
	}
}

// WrittenArguments lists the inputs for mmx explain, each option by its name.
func (m *KernelModule) WrittenArguments() []WrittenArgument {
	written := WrittenArguments(m)
	for _, option := range slices.Sorted(maps.Keys(m.Options)) {
		if m.Options[option] != nil {
			written = append(written, WrittenArgument{Field: "options." + option, Value: *m.Options[option]})
		}
	}
	return written
}

// Run does the module's work on the node.
func (m *KernelModule) Run(ctx context.Context, task NodeTask) protocol.Result {
	return task.Runtime.KernelModule(ctx, *m)
}
