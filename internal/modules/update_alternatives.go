package modules

import (
	"context"
	"fmt"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"mmx/internal/protocol"
)

// alternativesDirectories is where the alternatives system keeps one file for
// each generic name it has choices for, by package manager: dpkg's
// update-alternatives on Debian and Ubuntu, and alternatives on the Red Hat
// family, where update-alternatives is the same program.
// Tests point them elsewhere.
var alternativesDirectories = map[string]string{"apt": "/var/lib/dpkg/alternatives", "dnf": "/var/lib/alternatives"}

// alternativesState is what update-alternatives --display shows for a name.
type alternativesState struct {
	registered bool
	mode       string // automatic or manual
	current    string // the program the generic name runs now
	best       string // the choice with the highest priority
	choices    []string
}

// The lines of update-alternatives --display, in dpkg's form and in the Red
// Hat family's:
//
//	editor - manual mode                  editor - status is manual.
//	  link currently points to X           link currently points to X
//	  link best version is Y              Current `best' version is Y.
//	/usr/bin/vim.basic - priority 30      /usr/bin/vim - family vim priority 30
var (
	alternativesModeLine    = regexp.MustCompile(`^\S+ - (?:status is )?(auto|manual)(?: mode|\.)$`)
	alternativesCurrentLine = regexp.MustCompile(`link currently points to (\S+)$`)
	alternativesBestLine    = regexp.MustCompile(`best'? version is (\S+?)\.?$`)
	alternativesChoiceLine  = regexp.MustCompile(`^(/\S+) - (?:family \S+ )?priority -?\d+$`)
)

// UpdateAlternatives sets which registered program a generic name runs, in
// manual mode, or returns the name to automatic mode. With generic_command_path
// and priority_when_registered it first registers program_path as a choice
// when it is not one.
func (run Runtime) UpdateAlternatives(ctx context.Context, a UpdateAlternatives, packageManager string) (result protocol.Result) {
	if err := ValidateUpdateAlternatives(a); err != nil {
		return protocol.ConfigurationError(err)
	}
	directory, known := alternativesDirectories[packageManager]
	if !known {
		return protocol.ConfigurationError(fmt.Errorf("update_alternatives supports nodes with apt or dnf; the node's package manager is %q", packageManager))
	}
	if _, err := lookPath(ctx, "update-alternatives"); err != nil {
		return run.notYet(missingProgramError("update-alternatives"))
	}
	before, err := run.readAlternatives(ctx, directory, a.Name)
	if err != nil {
		return protocol.Failed(err)
	}
	register := false
	switch {
	case a.Automatic && !before.registered:
		// An earlier package task may register the name's choices.
		return run.notYet(fmt.Errorf("the alternatives system has no choices for %s yet; the packages that provide %s register them", a.Name, a.Name))
	case !a.Automatic && !slices.Contains(before.choices, a.ProgramPath) && a.GenericCommandPath == "":
		choices := "none"
		if len(before.choices) > 0 {
			choices = strings.Join(before.choices, ", ")
		}
		return run.notYet(fmt.Errorf("%s is not a registered choice for %s; the registered choices are: %s. The package that provides %s registers it, or give generic_command_path and priority_when_registered to register it", a.ProgramPath, a.Name, choices, a.ProgramPath))
	case !a.Automatic && !slices.Contains(before.choices, a.ProgramPath):
		register = true
	}
	mode := map[bool]string{true: "automatic", false: "manual"}[a.Automatic]
	if !register && before.mode == mode && (a.Automatic || before.current == a.ProgramPath) {
		return protocol.Success("unchanged")
	}
	changes := map[string]any{"program_path_before": before.current, "program_path_after": a.ProgramPath, "automatic_after": a.Automatic}
	// A name that is not registered yet has no mode before.
	if before.mode != "" {
		changes["automatic_before"] = before.mode == "automatic"
	}
	if a.Automatic {
		changes["program_path_after"] = before.best
	}
	if run.Dryrun {
		r := protocol.Success("would_change")
		r.Value = changes
		return r
	}
	// The value says what the program path became, so it is there once
	// update-alternatives --set or --auto worked.
	programPathSet := false
	defer func() {
		if programPathSet {
			result.Value = changes
		}
	}()
	changed := false
	failed := func(err error) protocol.Result {
		r := protocol.Failed(err)
		if changed {
			r.Change = "changed"
		}
		return r
	}
	if register {
		arguments := []string{"update-alternatives", "--install", a.GenericCommandPath, a.Name, a.ProgramPath, strconv.Itoa(*a.PriorityWhenRegistered)}
		if err := commandFailure(run.RunCommandThatChangesTheNode(ctx, arguments, ""), strings.Join(arguments, " ")); err != nil {
			return failed(err)
		}
		changed = true
	}
	arguments := []string{"update-alternatives", "--set", a.Name, a.ProgramPath}
	if a.Automatic {
		arguments = []string{"update-alternatives", "--auto", a.Name}
	}
	if err := commandFailure(run.RunCommandThatChangesTheNode(ctx, arguments, ""), strings.Join(arguments, " ")); err != nil {
		return failed(err)
	}
	changed, programPathSet = true, true
	after, err := run.readAlternatives(ctx, directory, a.Name)
	if err == nil {
		changes["program_path_after"] = after.current
	}
	if err == nil && (after.mode != mode || (!a.Automatic && after.current != a.ProgramPath)) {
		err = fmt.Errorf("after %s, %s is in %s mode and runs %s", strings.Join(arguments, " "), a.Name, after.mode, after.current)
	}
	if err != nil {
		return failed(err)
	}
	return protocol.Success("changed")
}

// readAlternatives reads a name's mode, current program, and choices. A name
// without a file in the alternatives system's directory has no choices.
func (run Runtime) readAlternatives(ctx context.Context, directory, name string) (alternativesState, error) {
	record := run.InspectPath(directory + "/" + name)
	if record.Error != "" {
		return alternativesState{}, fmt.Errorf("inspect %s/%s: %s", directory, name, record.Error)
	}
	if !record.Exists {
		return alternativesState{}, nil
	}
	out := run.RunCommand(ctx, []string{"update-alternatives", "--display", name}, "")
	if err := commandFailure(out, "update-alternatives --display "+name); err != nil {
		return alternativesState{}, err
	}
	state := alternativesState{registered: true}
	for _, line := range strings.Split(out.StandardOutput, "\n") {
		line = strings.TrimRight(line, " \t")
		if m := alternativesModeLine.FindStringSubmatch(line); m != nil {
			state.mode = map[string]string{"auto": "automatic", "manual": "manual"}[m[1]]
		} else if m := alternativesCurrentLine.FindStringSubmatch(line); m != nil {
			state.current = m[1]
		} else if m := alternativesBestLine.FindStringSubmatch(line); m != nil {
			state.best = m[1]
		} else if m := alternativesChoiceLine.FindStringSubmatch(line); m != nil {
			state.choices = append(state.choices, m[1])
		}
	}
	if state.mode == "" {
		return alternativesState{}, fmt.Errorf("update-alternatives --display %s did not show the mode of %s: %s", name, name, strings.TrimSpace(out.StandardOutput))
	}
	return state, nil
}

// UpdateAlternatives is which installed program a generic command runs.
type UpdateAlternatives struct {
	Name                   string `yaml:"name" json:"name"`
	ProgramPath            string `yaml:"program_path,omitempty" json:"program_path,omitempty"`
	Automatic              bool   `yaml:"automatic,omitempty" json:"automatic,omitempty"`
	GenericCommandPath     string `yaml:"generic_command_path,omitempty" json:"generic_command_path,omitempty"`
	PriorityWhenRegistered *int   `yaml:"priority_when_registered,omitempty" json:"priority_when_registered,omitempty"`
}

// alternativesName is a generic name of the alternatives system, such as
// editor or java: a file name in its directory.
var alternativesName = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.+-]*$`)

var alternativesNameRule = rule("update_alternatives.name must be a generic name such as editor or java: letters, digits, '_', '.', '+', and '-', without /", alternativesName.MatchString)

func ValidateUpdateAlternatives(a UpdateAlternatives) error {
	switch {
	case a.Name == "":
		return invalid("name", "update_alternatives.name is required: the generic name, such as editor or java")
	case a.Automatic && a.ProgramPath != "":
		return invalid("program_path", "update_alternatives takes no program_path with automatic: true, which lets the registered choice with the highest priority win")
	case a.Automatic && (a.GenericCommandPath != "" || a.PriorityWhenRegistered != nil):
		return invalid("generic_command_path", "update_alternatives.generic_command_path and priority_when_registered register program_path as a choice, so they need program_path, not automatic: true")
	case !a.Automatic && a.ProgramPath == "":
		return invalid("program_path", "update_alternatives.program_path is required unless automatic: true: the program the generic name runs, such as /usr/bin/vim.basic")
	case a.GenericCommandPath != "" && a.PriorityWhenRegistered == nil:
		return invalid("priority_when_registered", "update_alternatives.priority_when_registered is required with generic_command_path: a whole number; in automatic mode the highest wins")
	case a.GenericCommandPath == "" && a.PriorityWhenRegistered != nil:
		return invalid("generic_command_path", "update_alternatives.generic_command_path is required with priority_when_registered: the generic command's path, such as /usr/bin/java")
	}
	return nil
}

// updateAlternativesDescription is what mmx describe-module shows for update_alternatives.
var updateAlternativesDescription = Description{
	Name: "update_alternatives", Topic: TopicHostNameTimeZoneLocaleAndAlternatives, Schema: reflect.TypeOf(UpdateAlternatives{}),
	Summary: "which installed program a generic command, such as editor or java, runs, through the node's update-alternatives (the same program as alternatives on the Red Hat family)",
	Fields: []Field{
		{Name: "name", Required: true, Notes: "the generic name, such as editor"},
		{Name: "program_path", Notes: "required unless automatic: true; the program the generic name runs, set in manual mode so a package upgrade does not switch it back; it must be one of the name's registered choices, or generic_command_path and priority_when_registered register it"},
		{Name: "automatic", Default: "false", Notes: "true takes no program_path and returns the name to automatic mode, in which the registered choice with the highest priority wins"},
		{Name: "generic_command_path", Notes: "with priority_when_registered: the generic command's path, such as /usr/bin/java; MMX then registers program_path as a choice for name when it is not one (update-alternatives --install)"},
		{Name: "priority_when_registered", Notes: "with generic_command_path: a whole number; in automatic mode the highest wins"},
	},
	Example: "- update_alternatives:\n    name: editor\n    program_path: /usr/bin/vim.basic\n",
	Results: []string{"status", "change", "value.program_path_before", "value.program_path_after", "value.automatic_before", "value.automatic_after"},
	Dryrun:  "supported; reads the name's mode, program, and choices and reports what would be set; a path that is not a registered choice is not checked, because an earlier package task may register it",
}

// Prepare checks the inputs when the project loads.
func (a *UpdateAlternatives) Prepare(base string) error {
	return ValidateUpdateAlternatives(*a)
}

// Arguments lists the inputs that may hold a reference, with their rules.
func (a *UpdateAlternatives) Arguments(arguments *ArgumentList) {
	arguments.Public("name", &a.Name, alternativesNameRule)
	if a.ProgramPath != "" {
		arguments.Public("program_path", &a.ProgramPath, absolute)
	}
	if a.GenericCommandPath != "" {
		arguments.Public("generic_command_path", &a.GenericCommandPath, absolute)
	}
}

// CleanPaths cleans the resolved paths.
func (a *UpdateAlternatives) CleanPaths() {
	if a.ProgramPath != "" {
		a.ProgramPath = filepath.Clean(a.ProgramPath)
	}
	if a.GenericCommandPath != "" {
		a.GenericCommandPath = filepath.Clean(a.GenericCommandPath)
	}
}

// Run does the module's work on the node, by the node's package manager.
func (a *UpdateAlternatives) Run(ctx context.Context, task NodeTask) protocol.Result {
	packageManager := ""
	if task.Packages != nil {
		packageManager = task.Packages.Name()
	}
	return task.Runtime.UpdateAlternatives(ctx, *a, packageManager)
}
