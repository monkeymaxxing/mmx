package config

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"mmx/internal/modules"
)

// ModuleUsage is what mmx module-usage shows: which modules a project's
// playbooks use, of which kind, where their files are, and every place that
// calls them. It is read locally, from what validate resolves.
type ModuleUsage struct {
	Modules []UsedModule `json:"modules"`
	// NotUsed are the project's own modules that no node task calls.
	NotUsed []string `json:"not_used,omitempty"`
}

// UsedModule is one module and the places that call it.
type UsedModule struct {
	Name string `json:"name"`
	// Kind is core, official_extra, project_starlark, or project_executable.
	Kind string `json:"kind"`
	// Directory is where an official extra module's or the project's own
	// module's files are.
	Directory string `json:"directory,omitempty"`
	// Interpreter is the program an executable's first line names, or
	// compiled for a binary.
	Interpreter string `json:"interpreter,omitempty"`
	// DryrunCheckedByMMX is false for an executable: MMX cannot see what it
	// changes, so it must keep its own dryrun promise.
	DryrunCheckedByMMX bool       `json:"dryrun_checked_by_mmx"`
	Calls              []Location `json:"calls"`
}

// ModuleUsageOf reads the named playbooks, or every playbook of the project
// when none is named, with the one-time playbooks that are not completed,
// with the project's default inventory, and lists the modules their node
// tasks and the roles they call use.
func ModuleUsageOf(projectFile string, playbooks []string) (ModuleUsage, error) {
	if len(playbooks) == 0 {
		var err error
		if playbooks, err = ProjectNames(projectFile, "playbooks"); err != nil {
			return ModuleUsage{}, err
		}
		oneTime, err := OutstandingOneTimePlaybooks(projectFile)
		if err != nil {
			return ModuleUsage{}, err
		}
		playbooks = append(playbooks, oneTime...)
	}
	used := map[string]*UsedModule{}
	seen := map[Location]bool{}
	for _, playbook := range playbooks {
		p, _, err := Load(projectFile, playbook, "", nil)
		if err != nil {
			return ModuleUsage{}, err
		}
		var walk func(nodeTasks []Task)
		walk = func(nodeTasks []Task) {
			for _, t := range nodeTasks {
				if t.Module != "" {
					m := used[t.Module]
					if m == nil {
						m = usedModule(projectFile, t.Module, p.Modules)
						used[t.Module] = m
					}
					if at := t.Sources.At(t.Module); !seen[at] {
						seen[at] = true
						m.Calls = append(m.Calls, Location{File: at.File, Line: at.Line, Column: at.Column})
					}
				}
				for _, list := range t.Children() {
					walk(list.NodeTasks)
				}
			}
		}
		if err := WalkFleetStages(p.FleetStages, func(_ string, s FleetStage) error {
			walk(s.NodeTasks)
			return nil
		}); err != nil {
			return ModuleUsage{}, err
		}
	}
	var usage ModuleUsage
	for _, m := range used {
		sort.Slice(m.Calls, func(i, j int) bool {
			a, b := m.Calls[i], m.Calls[j]
			if a.File != b.File {
				return a.File < b.File
			}
			return a.Line < b.Line
		})
		usage.Modules = append(usage.Modules, *m)
	}
	order := map[string]int{modules.KindCore: 0, modules.KindOfficialExtra: 1, modules.KindProjectStarlark: 2, modules.KindProjectExecutable: 2}
	sort.Slice(usage.Modules, func(i, j int) bool {
		a, b := usage.Modules[i], usage.Modules[j]
		if order[a.Kind] != order[b.Kind] {
			return order[a.Kind] < order[b.Kind]
		}
		return a.Name < b.Name
	})
	entries, _ := os.ReadDir(filepath.Join(filepath.Dir(projectFile), "external_modules"))
	for _, e := range entries {
		if e.IsDir() && used[e.Name()] == nil && modules.ModuleName.MatchString(e.Name()) {
			usage.NotUsed = append(usage.NotUsed, e.Name())
		}
	}
	slices.Sort(usage.NotUsed)
	return usage, nil
}

func usedModule(projectFile, name string, catalog map[string]modules.Module) *UsedModule {
	if _, core := modules.LookUpCoreModule(name); core {
		return &UsedModule{Name: name, Kind: modules.KindCore, DryrunCheckedByMMX: true}
	}
	module := catalog[name]
	m := &UsedModule{Name: name, Kind: module.Kind, DryrunCheckedByMMX: module.Kind != modules.KindProjectExecutable}
	switch module.Kind {
	case modules.KindOfficialExtra:
		m.Directory = filepath.Join(OfficialExtraModulesDirectory(), name) + "/"
	default:
		m.Directory = filepath.Join("external_modules", name) + "/"
	}
	if module.Kind == modules.KindProjectExecutable {
		m.Interpreter = interpreterOf(filepath.Join(filepath.Dir(projectFile), "external_modules", name), module)
	}
	return m
}

// interpreterOf names the program an executable's first line runs, such as
// Python for #!/usr/bin/env python3, or compiled for a binary.
func interpreterOf(directory string, module modules.Module) string {
	for _, f := range module.Files {
		if !modules.IsModuleEntry(f.Path) {
			continue
		}
		b := make([]byte, 128)
		file, err := os.Open(filepath.Join(directory, f.Path))
		if err != nil {
			return "unknown"
		}
		n, _ := file.Read(b)
		file.Close()
		line, _, _ := strings.Cut(string(b[:n]), "\n")
		if !strings.HasPrefix(line, "#!") {
			return "compiled"
		}
		for _, known := range []struct{ word, name string }{{"python", "Python"}, {"bash", "Bash"}, {"perl", "Perl"}, {"ruby", "Ruby"}, {"node", "Node.js"}, {"sh", "shell"}} {
			if strings.Contains(line, known.word) {
				return known.name
			}
		}
		return "script"
	}
	return "unknown"
}

// Place is a call's place as file:line.
func (l Location) Place() string {
	return fmt.Sprintf("%s:%d", l.File, l.Line)
}
