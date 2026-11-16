package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"os"
	"slices"
	"strings"
	"text/tabwriter"

	"mmx/internal/config"
	"mmx/internal/modules"
	"mmx/internal/release"
)

// moduleUsageCommand shows which modules the project's playbooks use, of which
// kind, where their files are, and every place that calls them. It is local:
// it reads the project as validate does and contacts no node.
func moduleUsageCommand(args []string) (int, error) {
	fs := flag.NewFlagSet("module-usage", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "structured output")
	only := fs.String("module", "", "show where the named module is used")
	var playbooks []string
	for {
		if err := fs.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return 0, nil
			}
			return 4, err
		}
		if fs.NArg() == 0 {
			break
		}
		playbooks, args = append(playbooks, fs.Arg(0)), fs.Args()[1:]
	}
	if len(playbooks) > 1 {
		return 4, fmt.Errorf("module-usage takes at most one playbook name")
	}
	project, err := config.FindProject(".")
	if err != nil {
		return 4, err
	}
	if err := config.CheckOfficialExtraModulesVersion(config.OfficialExtraModulesDirectory()); err != nil {
		return 4, err
	}
	usage, err := config.ModuleUsageOf(project, playbooks)
	if err != nil {
		return 4, err
	}
	if *only != "" {
		var kept []config.UsedModule
		for _, m := range usage.Modules {
			if m.Name == *only {
				kept = append(kept, m)
			}
		}
		if len(kept) == 0 {
			return 4, fmt.Errorf("no node task calls the module %s", *only)
		}
		usage = config.ModuleUsage{Modules: kept}
	}
	if *asJSON {
		return encode(struct {
			SchemaVersion int `json:"schema_version"`
			config.ModuleUsage
		}{1, usage})
	}
	w := bufio.NewWriter(os.Stdout)
	defer w.Flush()
	groups := []struct {
		title string
		kinds []string
	}{
		{"Core modules (built into MMX)", []string{modules.KindCore}},
		{"Official extra modules (Starlark, shipped with MMX " + release.Version + ")", []string{modules.KindOfficialExtra}},
		{"The project's own modules", []string{modules.KindProjectStarlark, modules.KindProjectExecutable}},
	}
	first := true
	for _, group := range groups {
		table := tabwriter.NewWriter(w, 0, 0, 3, ' ', 0)
		printed := false
		for _, m := range usage.Modules {
			if !slices.Contains(group.kinds, m.Kind) {
				continue
			}
			if !printed {
				if !first {
					fmt.Fprintln(table)
				}
				fmt.Fprintln(table, group.title)
				printed, first = true, false
			}
			places := make([]string, len(m.Calls))
			for i, call := range m.Calls {
				places[i] = call.Place()
			}
			switch m.Kind {
			case modules.KindCore, modules.KindOfficialExtra:
				fmt.Fprintf(table, "  %s\t%s\n", m.Name, strings.Join(places, ", "))
			default:
				kind := "Starlark"
				if m.Kind == modules.KindProjectExecutable {
					kind = "executable (" + m.Interpreter + ")"
				}
				fmt.Fprintf(table, "  %s\t%s   %s\n", m.Name, m.Directory, kind)
				fmt.Fprintf(table, "  \t%s\n", strings.Join(places, ", "))
				if !m.DryrunCheckedByMMX {
					fmt.Fprintln(table, "  \tMMX cannot check the executable module's dryrun.")
					fmt.Fprintln(table, "  \tMMX cannot see what the executable changes, so the module")
					fmt.Fprintln(table, "  \titself must keep the dryrun promise.")
				}
			}
		}
		table.Flush()
	}
	if len(usage.NotUsed) > 0 {
		fmt.Fprintf(w, "\nThe project's own modules that no node task calls: %s\n", strings.Join(usage.NotUsed, ", "))
	}
	return 0, nil
}
