package modules

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"mmx/internal/assets"
	"mmx/internal/moduleio"
	"mmx/internal/protocol"
	"mmx/internal/value"
)

// resolveModuleInputs resolves the inputs of a node task that calls a module
// that is not core, and returns them as plain values. An input that the
// module's description marks private_value is a login handed only to a tool
// (value.Scope.LoginHandedOnlyToATool): a secret in it does not hide the
// task's result, and the node task execution engine replaces its text in
// what the task reports. hidesResult says that another input holds a secret.
// Validation checked each input that holds a reference only by its name, so
// the resolved values get the rest of the description's check here, before
// any kind of module runs: a value that breaks it fails the task as a
// configuration error. failure is the task's result when resolving or that
// check failed.
func resolveModuleInputs(module string, scope value.Scope, description *ModuleDescription, inputs map[string]any) (plain map[string]any, hidesResult bool, failure *protocol.Result) {
	names := make([]string, 0, len(inputs))
	for name := range inputs {
		names = append(names, name)
	}
	sort.Strings(names)
	plain = map[string]any{}
	for _, name := range names {
		private := description.PrivateValue(name)
		resolve := scope.Resolve
		if private {
			resolve = scope.LoginHandedOnlyToATool
		}
		resolved, err := resolve(inputs[name])
		if err != nil {
			r := protocol.Failed(err)
			return nil, false, &r
		}
		hidesResult = hidesResult || (!private && value.HasSecrets(resolved))
		plain[name] = value.Plain(resolved)
	}
	if description != nil {
		if problem := description.InputsProblem(module, plain); problem != nil {
			r := protocol.ConfigurationError(fmt.Errorf("%s.%s: %w", module, problem.Field, problem.Err))
			r.Secret = hidesResult
			return nil, hidesResult, &r
		}
	}
	return plain, hidesResult, nil
}

func (run Runtime) External(ctx context.Context, name string, module Module, inputs map[string]any, scope value.Scope, helper string) (result protocol.Result) {
	plain, private, failure := resolveModuleInputs(name, scope, module.Description, inputs)
	if failure != nil {
		return *failure
	}
	defer func() { result.Secret = result.Secret || private }()
	mode := "apply"
	if run.Dryrun {
		mode = "check"
	}
	request, err := json.Marshal(moduleio.Request{Protocol: moduleio.Version, Mode: mode, Inputs: plain})
	if err != nil {
		return protocol.ConfigurationError(fmt.Errorf("the inputs of module %s must be JSON values", name))
	}
	// Use the same request checks as the authoring helper, before launching code.
	if _, err := moduleio.ReadRequest(bytes.NewReader(request)); err != nil {
		return protocol.ConfigurationError(err)
	}
	if err := module.Validate(true); err != nil {
		return protocol.ConfigurationError(err)
	}
	if !filepath.IsAbs(module.Directory) || !filepath.IsAbs(helper) {
		return protocol.ConfigurationError(fmt.Errorf("module %s requires absolute paths on the node for the module directory and the JSON helper executable", name))
	}
	if !run.Dryrun {
		if err := run.Mutation("module " + name); err != nil {
			return protocol.Failed(err)
		}
	}
	environment := map[string]string{"MMX_MODULE_HELPER": helper}
	for _, entry := range RootLoginEnvironment() {
		name, value, _ := strings.Cut(entry, "=")
		environment[name] = value
	}
	process := runProcess(ctx, environment, module.Directory,
		bytes.NewReader(request), nil, filepath.Join(module.Directory, module.Executable))
	r := process.Result
	// Stdout is a protocol message, never a second unvalidated diagnostic stream.
	r.Stdout = ""
	r.Change = "unknown"
	if !process.Started {
		r.Change = "unchanged"
		return r
	}
	// runProcess failed the result already: the part of the output that
	// came is not the module's result.
	if process.OutputReadFailure != nil {
		return r
	}
	var reported moduleio.Result
	if process.Truncated {
		err = fmt.Errorf("the result of module %s exceeds 1 MiB", name)
	} else {
		reported, err = moduleio.ReadResult(bytes.NewReader(process.Output), mode, r.ExitCode)
	}
	if err != nil {
		r.Status, r.Reason = "failed", err.Error()
		if ctx.Err() != nil {
			r.Reason = "the executable of module " + name + " ended: " + ctx.Err().Error()
		}
		return r
	}
	r.Status, r.Change, r.Reason, r.Secret = reported.Status, reported.Change, reported.Reason, reported.Secret
	r.Diff = reported.Diff
	// The node task execution engine only compares fingerprints, and the
	// text that a module reports can hold a secret input: the result keeps
	// its SHA-256, as a Starlark script's fingerprint is one.
	if reported.Fingerprint != "" {
		r.Fingerprint = run.SHA256(reported.Fingerprint)
	}
	if reported.Outputs != nil {
		if err := protocol.Decode(reported.Outputs, &r.ModuleOutputs); err != nil {
			return protocol.Failed(fmt.Errorf("invalid module outputs"))
		}
		if value.HasReservedKey(r.ModuleOutputs) {
			r.Status, r.Reason, r.ModuleOutputs = "failed", "module outputs use a reserved MMX record key ($mmx_secret, $mmx_truncated, or $mmx_binary); use secret: true for private data", nil
			return r
		}
		if module.Description != nil {
			if err := module.Description.OutputsProblem(name, r.ModuleOutputs); err != nil {
				r.Status, r.Reason, r.ModuleOutputs = "failed", err.Error(), nil
				return r
			}
		}
	}
	if ctx.Err() != nil {
		r.Status, r.Reason = "failed", "the executable of module "+name+" ended: "+ctx.Err().Error()
	}
	return r
}

// Module is stored once per name, independently of task calls and loop counts.
// Executable is chosen for a node job; Directory is a private node-side binding.
type Module struct {
	// Kind is official_extra, project_starlark, or project_executable.
	Kind        string             `json:"kind"`
	Description *ModuleDescription `json:"description"`
	Executable  string             `json:"executable,omitempty"`
	Files       []ModuleFile       `json:"files"`
	Directory   string             `json:"-"`
}

// The files of a module that is not core.
const (
	ModuleScriptFile     = "module.star"
	ModuleExecutableFile = "module_executable"
)

// The kinds of a module, as results and mmx module-usage name them.
const (
	KindCore              = "core"
	KindOfficialExtra     = "official_extra"
	KindProjectStarlark   = "project_starlark"
	KindProjectExecutable = "project_executable"
)

// IsStarlark reports a module whose code is a Starlark script.
func (m Module) IsStarlark() bool { return m.Kind != KindProjectExecutable }

type ModuleFile struct {
	Path      string     `json:"path"`
	Mode      uint32     `json:"mode"`
	Asset     assets.Ref `json:"asset"`
	LocalPath string     `json:"-"`
}

func moduleEntry(name string) bool {
	return name == ModuleExecutableFile || name == ModuleExecutableFile+"_linux_amd64" || name == ModuleExecutableFile+"_linux_arm64"
}

// Validate applies to local discovery and received bundles. Validate references
// separately: discovery has paths but no frozen content digests yet.
func (m Module) Validate(selected bool) error {
	files := map[string]uint32{}
	for _, file := range m.Files {
		if file.Path == "." || !fs.ValidPath(file.Path) || !utf8.ValidString(file.Path) || strings.ContainsAny(file.Path, "\\\x00") {
			return fmt.Errorf("module file path must be relative and stay inside the module directory")
		}
		if _, exists := files[file.Path]; exists {
			return fmt.Errorf("duplicate module file path")
		}
		if file.Mode & ^uint32(0777) != 0 {
			return fmt.Errorf("module file permissions must contain only read, write, and execute bits (no setuid, setgid, or sticky bits)")
		}
		files[file.Path] = file.Mode
		if strings.HasPrefix(file.Path, ModuleExecutableFile+"_") && !strings.Contains(file.Path, "/") && !moduleEntry(file.Path) {
			return fmt.Errorf("the builds of a module_executable are module_executable_linux_amd64 and module_executable_linux_arm64")
		}
	}
	for file := range files {
		for parent := path.Dir(file); parent != "."; parent = path.Dir(parent) {
			if _, exists := files[parent]; exists {
				return fmt.Errorf("module file conflicts with a parent directory")
			}
		}
	}
	if _, described := files[ModuleDescriptionFile]; !described {
		return fmt.Errorf("the module needs its description in %s beside its code", ModuleDescriptionFile)
	}
	_, script := files[ModuleScriptFile]
	_, common := files[ModuleExecutableFile]
	_, amd64 := files[ModuleExecutableFile+"_linux_amd64"]
	_, arm64 := files[ModuleExecutableFile+"_linux_arm64"]
	switch {
	case script && (common || amd64 || arm64):
		return fmt.Errorf("a module is a Starlark script (module.star) or an executable (module_executable), not both")
	case script:
		if !m.IsStarlark() {
			return fmt.Errorf("the module's files hold a Starlark script, but its kind is %s", m.Kind)
		}
		return nil
	case common && (amd64 || arm64):
		return fmt.Errorf("use module_executable or the builds module_executable_linux_amd64 and module_executable_linux_arm64, not both")
	case !common && !amd64 && !arm64:
		return fmt.Errorf("the module needs module.star (a Starlark script) or module_executable")
	}
	if m.IsStarlark() {
		return fmt.Errorf("an official extra module is a Starlark script")
	}
	for name, mode := range files {
		if moduleEntry(name) && mode&0100 == 0 {
			return fmt.Errorf("the module's %s must be executable by its owner", name)
		}
	}
	if selected {
		if !moduleEntry(m.Executable) || files[m.Executable]&0100 == 0 {
			return fmt.Errorf("the module has no valid executable for this node")
		}
		for name := range files {
			if moduleEntry(name) && name != m.Executable {
				return fmt.Errorf("the module sent to the node includes a build for another processor")
			}
		}
	}
	return nil
}
