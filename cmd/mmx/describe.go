package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"text/tabwriter"

	modulepages "mmx/docs/reference/modules"
	"mmx/internal/access"
	"mmx/internal/config"
	"mmx/internal/modules"
	"mmx/internal/release"
)

// moduleReadmeFile is the page of a module that is not core, in its
// directory.
const moduleReadmeFile = "README.md"

// nodeTaskFormSections names, for each node task form that is not a module,
// the section of the configuration specification that explains it.
var nodeTaskFormSections = map[string]string{
	"task":       `"Node tasks and task blocks", docs/changing-mmx/design/configuration.md#node-tasks-and-task-blocks`,
	"task_block": `"Node tasks and task blocks", docs/changing-mmx/design/configuration.md#node-tasks-and-task-blocks`,
	"role":       `"Roles describe reusable server configuration", docs/changing-mmx/design/configuration.md#roles-describe-reusable-server-configuration`,
	"show":       `"Show messages and runtime values", docs/changing-mmx/design/configuration.md#show-messages-and-runtime-values`,
	"set_vars":   `"Task results and runtime variables", docs/changing-mmx/design/configuration.md#task-results-and-runtime-variables`,
}

// describeCommand lists the modules, or prints one module's page or its
// reference: kind, inputs, example, results, dryrun, and retry rule. It reads
// no inventory and contacts no node; project discovery locates
// external_modules/.
func describeCommand(args []string) (int, error) {
	fs := flag.NewFlagSet("describe-module", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "structured output")
	referenceOnly := fs.Bool("reference-only", false, "print only the module's kind, inputs, example, results, dryrun, and retry rule, without its page")
	// Accept the name before, between, or after options.
	var names []string
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
		names, args = append(names, fs.Arg(0)), fs.Args()[1:]
	}
	if len(names) > 1 {
		return 4, fmt.Errorf("describe-module takes at most one module name")
	}
	name := strings.Join(names, "")
	if name == "" && *referenceOnly {
		return 4, fmt.Errorf("--reference-only needs a module name: mmx describe-module NAME --reference-only")
	}
	project, projectErr := config.FindProject(".")
	if projectErr != nil {
		project = ""
	}
	if err := config.CheckOfficialExtraModulesVersion(config.OfficialExtraModulesDirectory()); err != nil {
		return 4, err
	}
	if name == "" {
		return listModules(project, *asJSON)
	}
	if _, core := modules.LookUpCoreModule(name); !core {
		if _, err := config.Describe(name); err != nil {
			return describeNonCoreModule(project, name, *asJSON, *referenceOnly)
		}
	}
	d, err := config.Describe(name)
	if err != nil {
		return 4, err
	}
	_, core := modules.LookUpCoreModule(d.Name)
	page := ""
	if core && !*referenceOnly {
		page, _ = modulepages.CoreModulePage(d.Name)
	}
	if *asJSON {
		return encode(struct {
			SchemaVersion int    `json:"schema_version"`
			Kind          string `json:"kind"`
			modules.Description
			Page string `json:"page,omitempty"`
		}{1, describedKind(d.Name), d, page})
	}
	if *referenceOnly {
		return printText(coreModuleReference(d), false)
	}
	if page != "" {
		return printText(page, true)
	}
	text := coreModuleReference(d)
	if section, form := nodeTaskFormSections[d.Name]; form {
		text += "\nThe configuration specification explains " + d.Name + " in its section " + section + ".\n"
	}
	return printText(text, true)
}

// describedKind is the kind that the JSON of mmx describe-module gives an
// entry that config.Describe describes: a core module or a node task form.
func describedKind(name string) string {
	if _, core := modules.LookUpCoreModule(name); core {
		return modules.KindCore
	}
	return "node_task_form"
}

// listModules prints every module, one line each: the core modules by
// topic, the official extra modules, the project's own modules, and the node
// task forms that are not modules.
func listModules(project string, asJSON bool) (int, error) {
	entries, err := config.Descriptions()
	if err != nil {
		return 4, err
	}
	others, err := config.NonCoreModules(project)
	if err != nil {
		return 4, err
	}
	if asJSON {
		type summary struct {
			Name    string `json:"name"`
			Kind    string `json:"kind"`
			Topic   string `json:"topic,omitempty"`
			Summary string `json:"summary"`
		}
		view := struct {
			SchemaVersion int       `json:"schema_version"`
			Entries       []summary `json:"entries"`
		}{SchemaVersion: 1}
		for _, d := range entries {
			view.Entries = append(view.Entries, summary{d.Name, describedKind(d.Name), d.Topic, d.Summary})
		}
		for _, d := range others {
			view.Entries = append(view.Entries, summary{d.Name, d.Kind, "", d.Summary})
		}
		return encode(view)
	}
	var text strings.Builder
	table := tabwriter.NewWriter(&text, 0, 0, 3, ' ', 0)
	fmt.Fprintln(table, "Core modules (built into MMX), by topic")
	var forms []modules.Description
	for _, d := range entries {
		if _, core := modules.LookUpCoreModule(d.Name); !core {
			forms = append(forms, d)
		}
	}
	for _, topic := range modules.CoreModuleTopics {
		fmt.Fprintf(table, "\n%s\n", topic)
		for _, d := range entries {
			if _, core := modules.LookUpCoreModule(d.Name); core && d.Topic == topic {
				fmt.Fprintf(table, "  %s\t%s\n", d.Name, d.Summary)
			}
		}
	}
	for _, group := range []struct{ title, kind string }{
		{"Official extra modules (Starlark, shipped with MMX " + release.Version + ")", modules.KindOfficialExtra},
		{"The project's own modules", ""},
	} {
		printed := false
		for _, d := range others {
			if (group.kind == "") == (d.Kind == modules.KindOfficialExtra) {
				continue
			}
			if !printed {
				fmt.Fprintf(table, "\n%s\n", group.title)
				printed = true
			}
			fmt.Fprintf(table, "  %s\t%s\n", d.Name, d.Summary)
		}
	}
	fmt.Fprintln(table, "\nNode task forms that are not modules, and the settings every node task accepts")
	for _, d := range forms {
		fmt.Fprintf(table, "  %s\t%s\n", d.Name, d.Summary)
	}
	table.Flush()
	text.WriteString("\nmmx describe-module NAME prints the whole page of one module: what it manages, its inputs, outputs, and examples, how it works, its errors, and why it works this way. Add --reference-only for only its kind, inputs, example, results, dryrun, and retry rule.\n")
	return printText(text.String(), false)
}

// describeNonCoreModule prints an official extra module's page, or a module
// of the project's own: its reference, then its README.md when it has one.
func describeNonCoreModule(project, name string, asJSON, referenceOnly bool) (int, error) {
	d, err := config.DescribeNonCoreModule(project, name)
	if err != nil {
		return 4, err
	}
	directory := d.Directory
	if d.Kind != modules.KindOfficialExtra && project != "" {
		directory = filepath.Join(filepath.Dir(project), d.Directory)
	}
	page := ""
	if !referenceOnly {
		readme, err := os.ReadFile(filepath.Join(directory, moduleReadmeFile))
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return 4, err
		}
		page = string(readme)
	}
	if asJSON {
		return encode(struct {
			SchemaVersion int `json:"schema_version"`
			config.NonCoreModule
			Page string `json:"page,omitempty"`
		}{1, d, page})
	}
	if referenceOnly {
		return printText(nonCoreModuleReference(d), false)
	}
	if d.Kind == modules.KindOfficialExtra && page != "" {
		return printText(page, true)
	}
	text := nonCoreModuleReference(d)
	if page != "" {
		text += "\n" + page
	}
	return printText(text, true)
}

// printText writes the output of describe-module. A page goes through the
// user's pager when standard output is a terminal: $PAGER when it is set,
// otherwise less -FRX. When the pager cannot start, the page is printed.
func printText(text string, isPage bool) (int, error) {
	if isPage && access.IsTerminal(int(os.Stdout.Fd())) {
		command := []string{"less", "-FRX"}
		if fromEnvironment := strings.Fields(os.Getenv("PAGER")); len(fromEnvironment) > 0 {
			command = fromEnvironment
		}
		pager := exec.Command(command[0], command[1:]...)
		pager.Stdin = strings.NewReader(text)
		pager.Stdout, pager.Stderr = os.Stdout, os.Stderr
		if err := pager.Start(); err == nil {
			// The pager's exit status says nothing about the page, such as
			// a user who quit before its end.
			_ = pager.Wait()
			return 0, nil
		}
	}
	if _, err := io.WriteString(os.Stdout, text); err != nil {
		return 4, err
	}
	return 0, nil
}

// coreModuleReference is the reference of a core module or a node task
// form: its kind, summary, fields, example, results, dryrun, and retry rule.
func coreModuleReference(d modules.Description) string {
	var text strings.Builder
	kind := "core module, built into MMX"
	if _, core := modules.LookUpCoreModule(d.Name); !core {
		kind = "node task form"
	}
	fmt.Fprintf(&text, "%s: %s\n%s\n\nFields\n", d.Name, kind, d.Summary)
	table := tabwriter.NewWriter(&text, 0, 0, 3, ' ', 0)
	for _, f := range d.Fields {
		kind := f.Type
		if len(f.Values) > 0 {
			kind = strings.Join(f.Values, " | ")
		}
		note := "optional"
		if f.Required {
			note = "required"
		} else if f.Default != "" {
			note = "default " + f.Default
		}
		if f.Notes != "" {
			note += "; " + f.Notes
		}
		fmt.Fprintf(table, "  %s\t%s\t%s\n", f.Name, kind, note)
	}
	table.Flush()
	example := "  " + strings.ReplaceAll(strings.TrimSuffix(d.Example, "\n"), "\n", "\n  ")
	fmt.Fprintf(&text, "\nExample\n%s\n\nResult: %s\nDryrun: %s\nRetry: %s\n", example, strings.Join(d.Results, ", "), d.Dryrun, d.Retry)
	return text.String()
}

// nonCoreModuleReference is the reference of a module that is not core.
func nonCoreModuleReference(d config.NonCoreModule) string {
	var text strings.Builder
	kinds := map[string]string{
		modules.KindOfficialExtra:     "official extra module, shipped with MMX " + release.Version + ", in " + d.Directory + "/",
		modules.KindProjectStarlark:   "the project's own module, Starlark, in " + d.Directory + "/",
		modules.KindProjectExecutable: "the project's own module, executable, in " + d.Directory + "/",
	}
	fmt.Fprintf(&text, "%s: %s\n%s\n\nInputs\n", d.Name, kinds[d.Kind], d.Summary)
	table := tabwriter.NewWriter(&text, 0, 0, 3, ' ', 0)
	if len(d.Inputs) == 0 {
		fmt.Fprintln(table, "  none")
	}
	for _, in := range d.Inputs {
		kind := in.Type
		if len(in.AllowedValues) > 0 {
			var values []string
			for _, v := range in.AllowedValues {
				values = append(values, fmt.Sprint(v))
			}
			kind = strings.Join(values, " | ")
		}
		note := "optional"
		if in.Required {
			note = "required"
		}
		if in.PrivateValue {
			note += "; a private value: a secret in the input does not hide the node task's result; when the input holds a secret, MMX shows [redacted] wherever the result's text holds the input's value"
		}
		fmt.Fprintf(table, "  %s\t%s\t%s; %s\n", in.Name, kind, note, in.Description)
	}
	table.Flush()
	text.WriteString("\nOutputs\n")
	table = tabwriter.NewWriter(&text, 0, 0, 3, ' ', 0)
	if len(d.Outputs) == 0 {
		fmt.Fprintln(table, "  none")
	}
	for _, out := range d.Outputs {
		fmt.Fprintf(table, "  %s\t%s\t%s\n", out.Name, out.Type, out.Description)
	}
	table.Flush()
	for _, example := range d.Examples {
		fmt.Fprintf(&text, "\nExample: %s\n  %s\n", example.Description, strings.ReplaceAll(strings.TrimSuffix(example.Config, "\n"), "\n", "\n  "))
	}
	dryrun := "MMX runs the Starlark script during a dryrun; functions that would change the node report the proposed changes without applying changes"
	if d.Kind == modules.KindProjectExecutable {
		dryrun = "MMX runs the executable in check mode. MMX cannot see what an executable changes, so the module must keep its own dryrun promise"
	}
	fmt.Fprintf(&text, "\nDryrun: %s\nRetry: bounded automatic retries by default; retry: false turns them off\n", dryrun)
	return text.String()
}
