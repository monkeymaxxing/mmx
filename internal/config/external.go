package config

import (
	"errors"
	"fmt"
	"io/fs"
	"mmx/internal/modules"
	"mmx/internal/release"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
)

func ModuleNames(nodeTasks []Task) []string {
	names := map[string]bool{}
	var walk func([]Task)
	walk = func(nodeTasks []Task) {
		for _, t := range nodeTasks {
			if t.CallsModuleThatIsNotCore() {
				names[t.Module] = true
			}
			for _, list := range t.Children() {
				walk(list.NodeTasks)
			}
		}
	}
	walk(nodeTasks)
	out := make([]string, 0, len(names))
	for name := range names {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// loadModules reads each module that is not core once, at the first task that
// calls it, so an error names that call: an official extra module from the
// directory beside mmx, then a module of the project's own from
// external_modules/. It checks every call's inputs against the module's
// description, and refuses a module of the project's own whose name another
// module or a node task setting has, also when nothing calls it.
func loadModules(p *Project, base string) error {
	extras := OfficialExtraModulesDirectory()
	if err := checkProjectModuleNames(base, extras); err != nil {
		return err
	}
	var load func([]Task) error
	load = func(nodeTasks []Task) error {
		for _, t := range nodeTasks {
			if !t.CallsModuleThatIsNotCore() {
				for _, list := range t.Children() {
					if err := load(list.NodeTasks); err != nil {
						return err
					}
				}
				continue
			}
			if _, found := p.Modules[t.Module]; !found && t.NonCoreModule != nil {
				if p.Modules == nil {
					p.Modules = map[string]modules.Module{}
				}
				p.Modules[t.Module] = *t.NonCoreModule
			}
		}
		return nil
	}
	return WalkFleetStages(p.FleetStages, func(_ string, fleetStage FleetStage) error { return load(fleetStage.NodeTasks) })
}

// ReleaseDirectory is the directory that holds the files of this MMX release
// beside mmx: the node runner executables and official_extra_modules/. It is
// the directory of the mmx program itself, also when a symlink to mmx
// started it, or MMX_RUNNER_DIR in a development environment.
func ReleaseDirectory() (string, error) {
	if directory := os.Getenv("MMX_RUNNER_DIR"); directory != "" {
		return directory, nil
	}
	executable, err := os.Executable()
	if err != nil {
		return "", err
	}
	// On macOS, os.Executable names the symlink that started the program.
	executable, err = filepath.EvalSymlinks(executable)
	if err != nil {
		return "", err
	}
	return filepath.Dir(executable), nil
}

// OfficialExtraModulesDirectory is where the official extra modules of this
// MMX release are: beside the node runner executables, in ReleaseDirectory.
func OfficialExtraModulesDirectory() string {
	directory, _ := ReleaseDirectory()
	return filepath.Join(directory, "official_extra_modules")
}

// OfficialExtraModulesVersionFile holds the version of the release the
// official extra modules were shipped with.
const OfficialExtraModulesVersionFile = "shipped_with_mmx_version"

// CheckOfficialExtraModulesVersion refuses official extra modules that came
// with another MMX release than this mmx program. A controller machine
// without the directory passes: only a call of an official extra module needs
// it.
func CheckOfficialExtraModulesVersion(directory string) error {
	b, err := os.ReadFile(filepath.Join(directory, OfficialExtraModulesVersionFile))
	if errors.Is(err, fs.ErrNotExist) {
		if _, statErr := os.Stat(directory); errors.Is(statErr, fs.ErrNotExist) {
			return nil
		}
	}
	if err != nil {
		return fmt.Errorf("the official extra modules in %s have no readable %s: %w", directory, OfficialExtraModulesVersionFile, err)
	}
	if shipped := strings.TrimSpace(string(b)); shipped != release.Version {
		return fmt.Errorf("the official extra modules in %s were shipped with MMX %s, but the mmx program is version %s; reinstall the mmx program and official_extra_modules/ from a single MMX release", directory, shipped, release.Version)
	}
	return nil
}

// findModule looks a module that is not core up: among the official extra
// modules, then among the project's own.
func findModule(base, extras, name string) (modules.Module, error) {
	if !modules.ModuleName.MatchString(name) {
		return modules.Module{}, fmt.Errorf("%s is not a node task setting or a core module; a module of the project's own must have a name that starts with a lowercase letter, contains only lowercase letters, digits, or underscores, and has at most 64 characters", name)
	}
	if _, err := os.Stat(filepath.Join(extras, name)); err == nil {
		module, err := readModuleDirectory(filepath.Join(extras, name), name, modules.KindOfficialExtra)
		if err != nil {
			return module, fmt.Errorf("the official extra module %s in %s: %w", name, extras, err)
		}
		return module, nil
	}
	directory := filepath.Join(base, "external_modules", name)
	if _, err := os.Stat(directory); errors.Is(err, fs.ErrNotExist) {
		where := "the official extra modules in " + extras
		if _, err := os.Stat(extras); err != nil {
			where = "the official extra modules (" + extras + " does not exist)"
		}
		return modules.Module{}, fmt.Errorf("%s is not a node task setting or a known module; MMX checked the core modules (listed by mmx describe-module), %s, and the project's own modules in external_modules/", name, where)
	}
	kind := modules.KindProjectExecutable
	if _, err := os.Stat(filepath.Join(directory, modules.ModuleScriptFile)); err == nil {
		kind = modules.KindProjectStarlark
	}
	module, err := readModuleDirectory(directory, name, kind)
	if err != nil {
		return module, fmt.Errorf("the module %s in external_modules/: %w", name, err)
	}
	return module, nil
}

// checkProjectModuleNames refuses a module of the project's own whose name is
// no valid module name, or the name of a core module, an official extra
// module, or a node task setting.
func checkProjectModuleNames(base, extras string) error {
	entries, err := os.ReadDir(filepath.Join(base, "external_modules"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	settings := yamlFields(reflect.TypeOf(Task{}))
	for _, entry := range entries {
		name := entry.Name()
		where := "external_modules/" + name
		switch _, setting := settings[name]; {
		case !entry.IsDir():
			continue
		case !modules.ModuleName.MatchString(name):
			return fmt.Errorf("%s: a module's name uses lower-case letters, digits, and underscores, starts with a letter, and has at most 64 characters; give the module another name", where)
		case setting:
			return fmt.Errorf("%s: %s is a node task setting; give the module another name", where, name)
		}
		if _, core := modules.LookUpCoreModule(name); core {
			return fmt.Errorf("%s: %s is a core module of MMX; give the module another name", where, name)
		}
		if _, err := os.Stat(filepath.Join(extras, name)); err == nil {
			return fmt.Errorf("%s: %s is an official extra module of MMX; give the module another name", where, name)
		}
	}
	return nil
}

// readModuleDirectory reads the files and the description of a module that is
// not core.
func readModuleDirectory(root, name, kind string) (modules.Module, error) {
	module := modules.Module{Kind: kind}
	for _, dir := range []string{filepath.Dir(root), root} {
		info, err := os.Lstat(dir)
		if err != nil {
			return module, err
		}
		if !info.IsDir() {
			return module, fmt.Errorf("module path %s must be a directory, not a symbolic link or another file type", dir)
		}
	}
	err := filepath.WalkDir(root, func(local string, entry fs.DirEntry, err error) error {
		relative, relErr := filepath.Rel(root, local)
		if relErr != nil {
			return relErr
		}
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
			return fmt.Errorf("module file %s must be a regular file without setuid, setgid, or sticky permission bits", filepath.Join(name, relative))
		}
		module.Files = append(module.Files, modules.ModuleFile{Path: filepath.ToSlash(relative), Mode: uint32(info.Mode().Perm()), LocalPath: local})
		return nil
	})
	if err == nil {
		err = module.Validate(false)
	}
	if err == nil {
		module.Description, err = modules.ReadModuleDescription(root, name)
	}
	return module, err
}

// SelectModules builds one catalog for a node fleet stage without mutating the saved
// project. Retry uses its frozen files and the same verified node architecture.
func SelectModules(all map[string]modules.Module, nodeTasks []Task, arch string) (map[string]modules.Module, error) {
	selected := map[string]modules.Module{}
	for _, name := range ModuleNames(nodeTasks) {
		module, found := all[name]
		if !found || !modules.ModuleName.MatchString(name) {
			return nil, fmt.Errorf("the module %s is not in the frozen project", name)
		}
		if err := module.Validate(false); err != nil {
			return nil, fmt.Errorf("the module %s: %w", name, err)
		}
		if module.IsStarlark() {
			selected[name] = module
			continue
		}
		executable := modules.ModuleExecutableFile + "_linux_" + arch
		for _, file := range module.Files {
			if file.Path == modules.ModuleExecutableFile {
				executable = modules.ModuleExecutableFile
			}
		}
		chosen := modules.Module{Kind: module.Kind, Description: module.Description, Executable: executable}
		for _, file := range module.Files {
			if !modules.IsModuleEntry(file.Path) || file.Path == executable {
				chosen.Files = append(chosen.Files, file)
			}
		}
		if err := chosen.Validate(true); err != nil {
			return nil, fmt.Errorf("the module %s has no executable for Linux %s", name, arch)
		}
		selected[name] = chosen
	}
	return selected, nil
}
