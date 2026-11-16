package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"mmx/internal/access"
	"mmx/internal/config"
	"mmx/internal/controller"
	"mmx/internal/moduleio"
	"mmx/internal/modules"
	"mmx/internal/protocol"
	"mmx/internal/release"
	"mmx/internal/secrets"
)

func main() {
	if socket := os.Getenv("MMX_ASKPASS_SOCKET"); socket != "" {
		prompt := "SSH password"
		if len(os.Args) > 1 {
			prompt = os.Args[1]
		}
		if err := access.RunAskpass(socket, prompt); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	// The module helper runs before any signal handling, so an interrupt stops
	// it even while it waits for its request on stdin.
	if len(os.Args) > 1 && os.Args[1] == "module-json" {
		if err := moduleio.Run(os.Args[2:], os.Stdin, os.Stdout); err != nil {
			printError(os.Stderr, err)
			var commandError *moduleio.CommandError
			if errors.As(err, &commandError) {
				os.Exit(4)
			}
			os.Exit(1)
		}
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	signals := make(chan os.Signal, 2)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	// A canceled vault edit kills the editor, then removes its plaintext. Closing
	// the terminal cancels it too, and no later signal exits before that cleanup.
	// Other commands keep the default hangup: remote jobs continue and
	// `mmx status` collects them.
	vault := len(os.Args) > 1 && os.Args[1] == "secrets"
	if vault {
		signal.Notify(signals, syscall.SIGHUP)
	}
	defer signal.Stop(signals)
	go func() {
		<-signals
		cancel()
		if vault {
			return
		}
		<-signals
		if len(os.Args) > 1 && (os.Args[1] == "run" || os.Args[1] == "dryrun" || os.Args[1] == "resume" || os.Args[1] == "check-one-time-playbooks" || os.Args[1] == "complete-one-time-playbook") {
			fmt.Fprintln(os.Stderr, "Leaving the display. Some servers may still be finishing work.\n\nCheck node progress:     mmx status\nRepeat the stop request: mmx stop")
		} else if len(os.Args) > 1 && os.Args[1] == "run-command" {
			// Default status and stop select playbook runs, never a shell run.
			fmt.Fprintln(os.Stderr, "Leaving the display. Some servers may still be finishing work.\n\nCheck node progress:     mmx status <run ID shown above>\nRepeat the stop request: mmx stop <run ID shown above>")
		} else {
			fmt.Fprintln(os.Stderr, "Interrupted.")
		}
		os.Exit(130)
	}()
	code, err := run(ctx, os.Args[1:])
	if err != nil {
		printError(os.Stderr, err)
	}
	os.Exit(code)
}
func run(ctx context.Context, args []string) (code int, err error) {
	defer func() {
		if err != nil && code == 0 {
			code = 4
		}
		if ctx.Err() == context.Canceled {
			code = 130
		}
	}()
	if len(args) == 0 || args[0] == "help" || args[0] == "--help" {
		fmt.Println("mmx init | validate [playbook] | explain <playbook> | nodes [playbook] | dryrun <playbook> | run <playbook> | resume [run-id] | run-command --nodes SELECTOR [--run-as-node-user-account NAME] -- COMMAND | status [run-id] | stop [run-id] | remove-mmx-from-node NODE | mark-node-work-failed --node NAME [run-id] | results [run-id] | describe-module [NAME] [--reference-only] | module-usage [PLAYBOOK] [--module NAME] | secrets edit | secrets passwd | secrets add-random KEY | secrets set KEY | secrets list-keys | secrets diff-keys | secrets usage [KEY] | new-one-time-playbook WHAT_IT_DOES | check-one-time-playbooks | complete-one-time-playbook NAME | version\n\nOptions: --json  --output auto|nodes|summary  --diff[=all]  --non-interactive  --vault-password-file PATH\nTargeting: --inventory NAME  --limit NODE_OR_GROUP[,...]  --only-fleet-stages ID[,...] (explain, nodes, dryrun, run)  --connection ssh_only (dryrun, run, run-command, check-one-time-playbooks, complete-one-time-playbook)\nVariables: --var NAME=VALUE, repeatable, for names in the playbook's command_line_vars\nResult filters: --node NAME  --fleet-stage NAME  --task NAME (results; mark-node-work-failed takes --node)\n\nFor executable modules: mmx module-json")
		fmt.Print(exitCodeHelp)
		return 0, nil
	}
	command := args[0]
	if command == "secrets" {
		return secretCommand(ctx, args[1:])
	}
	if command == "describe-module" {
		return describeCommand(args[1:])
	}
	if command == "module-usage" {
		return moduleUsageCommand(args[1:])
	}
	switch command {
	case "new-one-time-playbook":
		return newOneTimePlaybookCommand(args[1:])
	case "check-one-time-playbooks":
		return checkOneTimePlaybooksCommand(ctx, args[1:])
	case "complete-one-time-playbook":
		return completeOneTimePlaybookCommand(ctx, args[1:])
	}
	if command == "version" {
		if len(args) > 1 && !(len(args) == 2 && (args[1] == "--help" || args[1] == "-h")) {
			return 4, fmt.Errorf("version takes no arguments")
		}
		commit := ""
		if c := release.Commit(); c != "" {
			commit = ", commit " + c
		}
		fmt.Printf("mmx %s (protocol %d%s)\n", release.Version, protocol.Version, commit)
		return 0, nil
	}
	if command == "init" {
		if len(args) > 1 {
			if len(args) == 2 && (args[1] == "--help" || args[1] == "-h") {
				fmt.Println("mmx init — create mmx_project_settings.yml, inventories/example.yml, playbooks/example.yml, and the .mmx ignore rule in the current directory")
				return 0, nil
			}
			return 4, fmt.Errorf("init takes no arguments")
		}
		return initialize()
	}
	switch command {
	case "validate", "explain", "nodes", "dryrun", "run", "resume", "run-command", "status", "stop", "mark-node-work-failed", "results", "remove-mmx-from-node":
	default:
		return 4, fmt.Errorf("unknown command %q", command)
	}
	fs := flag.NewFlagSet(command, flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "print JSON")
	display := &presentation{}
	fs.Var(&display.diff, "diff", "show saved previews; =all reads complete retained differences")
	fs.StringVar(&display.mode, "output", "auto", "human output: auto, nodes, or summary")
	passwordFile := fs.String("vault-password-file", "", "file on the controller machine containing the vault passphrase")
	nonInteractive := fs.Bool("non-interactive", false, "do not prompt for credentials")
	inventory := fs.String("inventory", "", "inventory name for the command, replacing default_inventory")
	connection := addConnectionOption(fs)
	limit := fs.String("limit", "", "comma-separated nodes or groups; narrows every fleet stage")
	onlyFleetStages := fs.String("only-fleet-stages", "", "comma-separated fleet stage IDs; keeps only those fleet stages, in written order")
	var commandLine []string
	fs.Func("var", "set a declared command-line variable, NAME=VALUE (repeatable)", func(s string) error {
		commandLine = append(commandLine, s)
		return nil
	})
	managedNode := fs.String("node", "", "the named node: show its saved results (results), or the node whose unconfirmed work to record as failed (mark-node-work-failed)")
	fleetStage := fs.String("fleet-stage", "", "show saved results for a fleet stage, by its path or a path prefix")
	task := fs.String("task", "", "show saved results for a node task, by its path or a path prefix")
	commandNodes := fs.String("nodes", "", "mmx run-command: comma-separated nodes or groups to run the command on")
	commandUser := fs.String("run-as-node-user-account", "", "mmx run-command: account on each node to run the command as")
	// Accept a playbook name or run ID before, between, or after flags.
	options := args[1:]
	id := ""
	for {
		if err := fs.Parse(options); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return 0, nil
			}
			return 4, err
		}
		if command == "run-command" || fs.NArg() == 0 {
			break
		}
		if id != "" {
			return 4, fmt.Errorf("expected at most one playbook name or run ID")
		}
		id, options = fs.Arg(0), fs.Args()[1:]
	}
	if display.mode != "auto" && display.mode != "nodes" && display.mode != "summary" {
		return 4, fmt.Errorf("--output must be auto, nodes, or summary")
	}
	if err := checkPasswordFileOptions(fs); err != nil {
		return 4, err
	}
	// mmx run-command's remaining words are its command, never a run id.
	var words []string
	if command == "run-command" {
		words = fs.Args()
		if *commandNodes == "" || len(words) == 0 {
			return 4, fmt.Errorf("usage: mmx run-command --nodes NODE_OR_GROUP[,...] [--run-as-node-user-account NAME] -- COMMAND")
		}
	} else if *commandNodes != "" || *commandUser != "" {
		return 4, fmt.Errorf("only mmx run-command accepts --nodes and --run-as-node-user-account")
	}
	if command == "remove-mmx-from-node" && (id == "" || *asJSON) {
		return 4, fmt.Errorf("usage: mmx remove-mmx-from-node NODE [--inventory NAME]; the command prints text and does not accept --json")
	}
	if *connection != "" {
		switch command {
		case "dryrun", "run", "run-command":
		case "resume":
			return 4, fmt.Errorf("resume uses the recorded run's connection choice and accepts no --connection; start a new mmx run <playbook> --connection ssh_only for SSH")
		default:
			return 4, fmt.Errorf("%s does not accept --connection", command)
		}
	}
	var limitSelectors []string
	if *limit != "" {
		switch command {
		case "explain", "nodes", "dryrun", "run":
			limitSelectors = strings.Split(*limit, ",")
		case "resume":
			return 4, fmt.Errorf("resume uses the recorded run's node selection and does not accept --limit; start a new mmx run <playbook> --limit NODE_OR_GROUP[,...] to select different nodes")
		default:
			return 4, fmt.Errorf("%s does not accept --limit", command)
		}
	}
	var onlyFleetStageIDs []string
	if *onlyFleetStages != "" {
		switch command {
		case "explain", "nodes", "dryrun", "run":
			onlyFleetStageIDs = strings.Split(*onlyFleetStages, ",")
		case "resume":
			return 4, fmt.Errorf("resume repeats the recorded run's fleet stages and accepts no --only-fleet-stages; start a new mmx run <playbook> --only-fleet-stages for other fleet stages")
		default:
			return 4, fmt.Errorf("%s does not accept --only-fleet-stages", command)
		}
	}
	if commandLine != nil {
		switch command {
		case "validate", "explain", "nodes", "dryrun", "run":
		case "resume":
			return 4, fmt.Errorf("resume uses the recorded run's command-line variables and accepts no --var; start a new mmx run <playbook> --var for different values")
		default:
			return 4, fmt.Errorf("%s does not accept --var", command)
		}
	}
	if *inventory != "" && command == "mark-node-work-failed" {
		return 4, fmt.Errorf("mark-node-work-failed does not accept --inventory; it scans every saved run")
	}
	// The result filters narrow only what mmx results shows. Any other command
	// would ignore them and act on its whole selection.
	if command != "results" {
		for _, filter := range []struct{ option, value string }{{"--node", *managedNode}, {"--fleet-stage", *fleetStage}, {"--task", *task}} {
			if filter.value != "" && !(command == "mark-node-work-failed" && filter.option == "--node") {
				return 4, fmt.Errorf("%s does not accept %s; %s", command, filter.option, resultFilterInstead(command, filter.option))
			}
		}
	}
	abs, err := config.FindProject(".")
	if err != nil && !(id != "" && os.Getenv("MMX_STATE_DIR") != "" && (command == "results" || command == "status" || command == "stop" || command == "mark-node-work-failed" || command == "resume")) {
		if command == "validate" {
			return 1, err
		}
		return 4, err
	}
	inventoryName := *inventory
	if inventoryName != "" && !modules.Name.MatchString(inventoryName) {
		return 4, fmt.Errorf("--inventory must be a name, not a path: %q", inventoryName)
	}
	if (command == "validate" || command == "nodes") && id == "" && len(commandLine) > 0 {
		return 4, fmt.Errorf("--var requires a playbook name")
	}
	if command == "nodes" && id == "" && onlyFleetStageIDs != nil {
		return 4, fmt.Errorf("--only-fleet-stages requires a playbook name")
	}
	if (command == "run" || command == "dryrun" || command == "explain") && id == "" {
		names, err := config.ProjectNames(abs, "playbooks")
		if err != nil {
			return 4, err
		}
		available := strings.Join(names, ", ")
		if available == "" {
			available = "(none)"
		}
		// A one-time playbook runs only when a command names it, so its
		// names stand apart from the playbooks people run every day.
		oneTime, err := config.OutstandingOneTimePlaybooks(abs)
		if err != nil {
			return 4, err
		}
		if len(oneTime) > 0 {
			available += "; one-time playbooks: " + strings.Join(oneTime, ", ")
		}
		return 4, fmt.Errorf("%s requires a playbook name; available playbooks: %s", command, available)
	}
	// The official extra modules beside mmx must come from this release.
	if err := config.CheckOfficialExtraModulesVersion(config.OfficialExtraModulesDirectory()); err != nil {
		return 4, err
	}
	state, runnerDir, err := controllerDirectories(abs)
	if err != nil {
		return 4, err
	}
	display.state = state
	c := controller.Controller{State: state, RunnerDir: runnerDir, Selection: controller.Identity{EntryPoint: abs}, Progress: liveProgress(os.Stderr, *asJSON, display)}
	c.Credentials = &access.Credentials{Interactive: !*nonInteractive && !*asJSON && access.IsTerminal(int(os.Stdin.Fd())), Context: ctx, Notify: func(message string) { fmt.Fprintln(os.Stderr, escapeControlCharacters(message)) }}
	c.Secrets = &secrets.Store{PasswordFile: *passwordFile, Interactive: c.Credentials.Interactive, Prompt: c.Credentials.ReadPassword}
	var selected *controller.Run
	switch command {
	case "run", "dryrun", "resume", "run-command", "status", "stop", "mark-node-work-failed", "results":
		release, err := c.UseHistory()
		if err != nil {
			return 4, err
		}
		defer func() {
			release()
			if selected != nil {
				cleanupHistory(c, selected)
			}
		}()
	}
	switch command {
	case "validate", "explain", "nodes", "dryrun", "run":
		var p config.Project
		var inv config.Inventory
		var err error
		if command == "validate" && id == "" {
			p, err = config.ValidateProject(abs)
		} else {
			p, inv, err = config.Load(abs, id, inventoryName, commandLine)
		}
		if err != nil {
			// A rejected --var and the name of a completed one-time playbook
			// are command errors, also for validate.
			var variableError *config.CommandLineError
			var completed *config.CompletedOneTimePlaybookError
			if command == "validate" && !errors.As(err, &variableError) && !errors.As(err, &completed) {
				return 1, err
			}
			return 4, err
		}
		if command != "validate" && command != "nodes" {
			if err := config.RequireCommandLineValues(p); err != nil {
				return 4, err
			}
		}
		applyConnectionChoice(inv, *connection)
		p.OnlyFleetStages = onlyFleetStageIDs
		c.Selection = controller.Identity{EntryPoint: abs, Project: p.Project, Inventory: inv.Name, Playbook: p.PlaybookName}
		if command == "validate" {
			if *asJSON {
				return encode(map[string]any{"schema_version": 1, "valid": true, "project_name": p.Project})
			}
			fmt.Println("Valid:", p.Project)
			return 0, nil
		}
		// Load checked the project without a limit or --only-fleet-stages; both
		// are checked here, with the same selection explain and nodes show.
		var inspection config.Inspection
		if command == "explain" || command == "nodes" || limitSelectors != nil || onlyFleetStageIDs != nil {
			inspect := config.Inspect
			if command == "explain" {
				inspect = config.InspectForExplain
			}
			if inspection, err = inspect(p, inv, limitSelectors); err != nil {
				return 4, err
			}
		}
		if command == "explain" {
			if *asJSON {
				return encode(struct {
					SchemaVersion int `json:"schema_version"`
					config.Inspection
				}{2, inspection})
			}
			if err := explain(terminalSafe(os.Stdout), inspection); err != nil {
				return 4, err
			}
			return 0, nil
		}
		if command == "nodes" {
			return showNodes(os.Stdout, newNodesView(p.PlaybookName, inv, inspection), display.mode, *asJSON)
		}
		// A limit reaches a for_each_node iteration's helper nodes only after
		// they were shown.
		showHelperNodes(terminalSafe(os.Stderr), inspection.HelperNodes)
		r, err := c.New(ctx, p, inv, limitSelectors, command == "dryrun")
		if err != nil {
			return 4, err
		}
		selected = r
		err = c.Execute(ctx, r, false)
		var notStarted *controller.NotStartedError
		if errors.As(err, &notStarted) {
			// The run was not saved: print the error alone, not a run to resume.
			selected = nil
			return 4, err
		}
		return output(r, *asJSON, false, display, err)
	case "remove-mmx-from-node":
		// Loading with a command on the node checks the node's name and the
		// inventory, as run-command does; the command itself runs nothing.
		p, inv, err := config.LoadRunCommand(abs, inventoryName, []string{id}, "true", "")
		var unknown *config.UnknownNodeError
		if errors.As(err, &unknown) {
			return 4, fmt.Errorf("remove-mmx-from-node: no node named %q in inventory %s", id, inv.Name)
		}
		if err != nil {
			return 4, err
		}
		return removeMMXFromNode(ctx, c, p, inv, id)
	case "run-command":
		p, inv, err := config.LoadRunCommand(abs, inventoryName, strings.Split(*commandNodes, ","), strings.Join(words, " "), *commandUser)
		if err != nil {
			return 4, err
		}
		applyConnectionChoice(inv, *connection)
		c.Selection = controller.Identity{EntryPoint: controller.RunCommandEntryPoint, Project: p.Project, Inventory: inv.Name, Playbook: p.PlaybookName}
		r, err := c.New(ctx, p, inv, nil, false)
		if err != nil {
			return 4, err
		}
		selected = r
		err = c.Execute(ctx, r, false)
		var notStarted *controller.NotStartedError
		if errors.As(err, &notStarted) {
			// The run was not saved: print the error alone, not a run to resume.
			selected = nil
			return 4, err
		}
		return output(r, *asJSON, false, display, err)
	case "resume":
		if id == "" {
			current, _, err := selection(abs, inventoryName)
			if err != nil {
				return 4, fmt.Errorf("cannot identify the current project and inventory: %w; use mmx resume <run-id> to select a saved run", err)
			}
			c.Selection = current
		}
		r, err := c.Load(id, true)
		selected = r
		if id == "" && r != nil {
			announceSelection(r, *asJSON)
		}
		// A matching run without unfinished work is an ordinary answer; no
		// matching run is a command error, as for status, stop, and results.
		if errors.Is(err, controller.ErrNothingToResume) {
			if *asJSON {
				return encode(map[string]any{"schema_version": 1, "status": "nothing_to_resume", "id": r.ID, "short_id": controller.ShortID(r.ID), "playbook": r.Project.PlaybookName}) //nolint:nilaway // Load returns ErrNothingToResume only with a successfully loaded run.
			}
			fmt.Println("Nothing to resume.")
			return 0, nil
		}
		if err != nil {
			return 4, err
		}
		err = c.Execute(ctx, r, true)
		return output(r, *asJSON, false, display, err)
	case "mark-node-work-failed":
		if *managedNode == "" {
			return 4, fmt.Errorf("usage: mmx mark-node-work-failed --node NODE [run-id]; without a run ID, every saved run with unconfirmed work on the node")
		}
		runs := []*controller.Run{}
		if id != "" {
			r, err := c.Load(id, false)
			if err != nil {
				return 4, err
			}
			runs = append(runs, r)
		} else if runs, err = c.RunsWithUnconfirmedWorkOn(*managedNode); err != nil {
			return 4, err
		}
		return markNodeWorkFailed(ctx, c, runs, *managedNode, *asJSON)
	case "status", "stop", "results":
		if id == "" {
			current, _, err := selection(abs, inventoryName)
			if err != nil {
				return 4, err
			}
			c.Selection = current
		}
		r, err := c.Load(id, false)
		if err != nil {
			return 4, err
		}
		selected = r
		if id == "" {
			announceSelection(r, *asJSON)
		}
		if command != "results" {
			if err := c.Inspect(ctx, r, command == "stop"); err != nil {
				return output(r, *asJSON, false, display, err)
			}
			_, err := output(r, *asJSON, false, display, nil)
			if err != nil {
				return 4, err
			}
			if ctx.Err() != nil {
				return 130, nil
			}
			for _, en := range r.Entries {
				if protocol.Unconfirmed(en.Response.Status) {
					return 3, nil
				}
			}
			return 0, nil
		}
		r.Entries = r.OrderedEntries()
		filtered := r.Entries[:0]
		for _, entry := range r.Entries {
			if *managedNode != "" && entry.Node != *managedNode {
				continue
			}
			if *fleetStage != "" && !matchesPath(entry.FleetStage, *fleetStage) {
				continue
			}
			if *task != "" {
				results := entry.Response.Results[:0]
				for _, result := range entry.Response.Results {
					if matchesPath(result.Task, *task) {
						results = append(results, result)
					}
				}
				entry.Response.Results = results
				for i := range entry.PreviousNodeJobAttempts {
					var filteredResults []protocol.Result
					for _, result := range entry.PreviousNodeJobAttempts[i].Response.Results {
						if matchesPath(result.Task, *task) {
							filteredResults = append(filteredResults, result)
						}
					}
					entry.PreviousNodeJobAttempts[i].Response.Results = filteredResults
				}
			}
			filtered = append(filtered, entry)
		}
		selections := map[string]*controller.Selection{}
		if *managedNode == "" && *task == "" {
			for path, sel := range r.Selections {
				if *fleetStage == "" || matchesPath(path, *fleetStage) {
					selections[path] = sel
				}
			}
		}
		r.Selections = selections
		r.Entries = filtered
		_, err = output(r, *asJSON, true, display, nil)
		if err != nil {
			return 4, err
		}
		return 0, nil
	default:
		return 4, fmt.Errorf("unknown command %q", command)
	}
}
func encode(v any) (int, error) {
	e := json.NewEncoder(os.Stdout)
	e.SetIndent("", "  ")
	if err := e.Encode(v); err != nil {
		return 4, err
	}
	return 0, nil
}
func initialize() (int, error) {
	files := map[string]string{
		"mmx_project_settings.yml": "project_name: mmx_example\ndefault_inventory: example\n",
		"inventories/example.yml":  "nodes:\n  example:\n    address: replace-with-your-node\n    ssh_user: root\n",
		"playbooks/example.yml": `fleet_stages:
  - id: welcome
    nodes: all

    node_tasks:
      - file:
          path: /etc/mmx-example
          content: "Managed by MMX.\n"
          owner: root
          group: root
          permissions_mode: "0644"
`,
	}
	for path := range files {
		if _, err := os.Lstat(path); err == nil {
			return 4, fmt.Errorf("%s already exists", path)
		} else if !os.IsNotExist(err) {
			return 4, err
		}
	}
	for path, content := range files {
		if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
			return 4, err
		}
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
		if err != nil {
			return 4, err
		}
		_, err = f.WriteString(content)
		closeErr := f.Close()
		if err == nil {
			err = closeErr
		}
		if err != nil {
			return 4, err
		}
	}
	ignore, err := os.ReadFile(".gitignore")
	if err != nil && !os.IsNotExist(err) {
		return 4, err
	}
	if !strings.Contains("\n"+string(ignore)+"\n", "\n/.mmx/\n") {
		f, err := os.OpenFile(".gitignore", os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
		if err != nil {
			return 4, err
		}
		prefix := ""
		if len(ignore) > 0 && ignore[len(ignore)-1] != '\n' {
			prefix = "\n"
		}
		_, err = f.WriteString(prefix + "/.mmx/\n")
		closeErr := f.Close()
		if err == nil {
			err = closeErr
		}
		if err != nil {
			return 4, err
		}
	}
	fmt.Println("Created mmx_project_settings.yml, inventories/example.yml, and playbooks/example.yml. Set the SSH destination in inventories/example.yml, then run mmx dryrun example.")
	return 0, nil
}

// selection reads strict project settings without requiring current playbooks.
func selection(path, inventoryName string) (controller.Identity, config.Project, error) {
	p, err := config.ReadSettings(path)
	if err != nil {
		return controller.Identity{}, p, err
	}
	if inventoryName == "" {
		inventoryName = p.Inventory
	}
	if !modules.Name.MatchString(inventoryName) {
		return controller.Identity{}, p, fmt.Errorf("--inventory must be a name, not a path")
	}
	return controller.Identity{EntryPoint: path, Project: p.Project, Inventory: inventoryName}, p, nil
}

func announceSelection(r *controller.Run, asJSON bool) {
	if !asJSON {
		fmt.Fprintf(os.Stderr, "Selected run %s, playbook %s.\n", r.ID, r.Project.PlaybookName)
	}
}

// resultFilterInstead names what to use instead of a result filter that a
// command other than results does not accept.
func resultFilterInstead(command, option string) string {
	selectsFleetStages := command == "explain" || command == "nodes" || command == "dryrun" || command == "run"
	switch {
	case option == "--node" && selectsFleetStages:
		return "narrow the selected nodes with --limit"
	case option == "--fleet-stage" && selectsFleetStages:
		return "keep only some fleet stages with --only-fleet-stages"
	case option == "--node" && command == "resume":
		return "resume uses the recorded run's node selection; start a new mmx run <playbook> --limit NODE_OR_GROUP[,...] to select different nodes"
	case option == "--fleet-stage" && command == "resume":
		return "resume repeats the recorded run's fleet stages; start a new mmx run <playbook> --only-fleet-stages for other fleet stages"
	case option == "--node" && command == "run-command":
		return "choose the nodes with --nodes"
	}
	return "use mmx results to filter the results of a saved run"
}

func matchesPath(path, filter string) bool {
	return path == filter || strings.HasPrefix(path, filter+"/") || strings.HasPrefix(path, filter+"[")
}

// controllerDirectories keeps test/development overrides out of command help.
func controllerDirectories(settingsFile string) (string, string, error) {
	state := os.Getenv("MMX_STATE_DIR")
	if state == "" {
		state = filepath.Join(filepath.Dir(settingsFile), ".mmx")
	}
	runner, err := config.ReleaseDirectory()
	if err != nil {
		return "", "", err
	}
	return state, runner, nil
}

// addConnectionOption accepts only the invocation-wide SSH choice. Leaving it
// out preserves each node's inventory choice.
func addConnectionOption(fs *flag.FlagSet) *string {
	var connection string
	fs.Func("connection", "use SSH for every node for the command (value: ssh_only)", func(s string) error {
		if s != "ssh_only" {
			return fmt.Errorf("--connection accepts only ssh_only")
		}
		connection = s
		return nil
	})
	return &connection
}

// applyConnectionChoice changes the loaded inventory before a run saves it,
// so resume uses the same choice, including for helper nodes outside a limit.
func applyConnectionChoice(inv config.Inventory, connection string) {
	if connection == "" {
		return
	}
	for name, node := range inv.Nodes {
		node.Transport = connection
		inv.Nodes[name] = node
	}
}
