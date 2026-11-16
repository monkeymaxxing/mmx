package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	"mmx/internal/access"
	"mmx/internal/config"
	"mmx/internal/controller"
	"mmx/internal/modules"
	"mmx/internal/protocol"
	"mmx/internal/secrets"
	"mmx/internal/value"
)

// oneTimePlaybookCheck is what mmx check-one-time-playbooks reports: for each
// one-time playbook outside completed/, the state of every node it selects
// in one inventory. mmx complete-one-time-playbook reports the same for its
// one playbook.
type oneTimePlaybookCheck struct {
	SchemaVersion    int                      `json:"schema_version"`
	Inventory        string                   `json:"inventory"`
	OneTimePlaybooks []checkedOneTimePlaybook `json:"one_time_playbooks"`
}

// checkedOneTimePlaybook holds each node the playbook selects in exactly one
// of its three lists, sorted by node name.
type checkedOneTimePlaybook struct {
	Name string `json:"name"`
	// RunID is the full ID of the dryrun that inspected the nodes; it is
	// empty when MMX could not load the playbook and so made no dryrun.
	RunID       string      `json:"run_id"`
	Done        []nodeState `json:"done"`
	StillNeeded []nodeState `json:"still_needed"`
	NotKnown    []nodeState `json:"not_known"`
	// NotInspectedReason says what the dryrun did not inspect and no node's
	// state shows: that it inspected nothing, or which fleet stage's nodes it
	// could not find out. The playbook then counts as not known.
	NotInspectedReason string `json:"not_inspected_reason,omitempty"`
}

type nodeState struct {
	Node   string `json:"node"`
	Reason string `json:"reason"`
}

// countsAsNotKnown reports that the check could not say for every node
// whether it still needs the playbook.
func (p checkedOneTimePlaybook) countsAsNotKnown() bool {
	return len(p.NotKnown) > 0 || p.NotInspectedReason != ""
}

// oneTimePlaybookExitCode follows the dryrun's rule that incomplete goes
// before proposed changes: 3 when the state of a node is not known, else 2
// when a node still needs a playbook, else 0.
func oneTimePlaybookExitCode(playbooks []checkedOneTimePlaybook) int {
	code := 0
	for _, p := range playbooks {
		if p.countsAsNotKnown() {
			return 3
		}
		if len(p.StillNeeded) > 0 {
			code = 2
		}
	}
	return code
}

// dryrunOfOneTimePlaybook inspects the nodes a loaded one-time playbook
// selects, by an ordinary dryrun that is saved in the run history. Tests
// replace it with a stand-in for the nodes.
var dryrunOfOneTimePlaybook = func(ctx context.Context, c controller.Controller, p config.Project, inv config.Inventory) (*controller.Run, error) {
	r, err := c.New(ctx, p, inv, nil, true)
	if err != nil {
		return nil, err
	}
	return r, c.Execute(ctx, r, false)
}

// oneTimePlaybookOptions are the options that check-one-time-playbooks and
// complete-one-time-playbook share.
type oneTimePlaybookOptions struct {
	asJSON, nonInteractive  *bool
	inventory, passwordFile *string
}

func addOneTimePlaybookOptions(fs *flag.FlagSet) oneTimePlaybookOptions {
	return oneTimePlaybookOptions{
		asJSON:         fs.Bool("json", false, "structured output; do not prompt"),
		nonInteractive: fs.Bool("non-interactive", false, "do not prompt for credentials"),
		inventory:      fs.String("inventory", "", "inventory name whose nodes are checked, replacing default_inventory"),
		passwordFile:   fs.String("vault-password-file", "", "file on the controller machine containing the vault passphrase"),
	}
}

// oneTimePlaybookInspector inspects one-time playbooks on the nodes of one
// inventory. Every dryrun of one command uses its one controller, with one
// vault store and one set of credentials, so the vault is unlocked and each
// credential is asked for at most once.
type oneTimePlaybookInspector struct {
	settingsFile string
	inventory    config.Inventory
	controller   controller.Controller
	asJSON       bool
	releaseLease func()
	dryruns      []*controller.Run
}

func newOneTimePlaybookInspector(ctx context.Context, settingsFile string, options oneTimePlaybookOptions) (*oneTimePlaybookInspector, error) {
	if *options.inventory != "" && !modules.Name.MatchString(*options.inventory) {
		return nil, fmt.Errorf("--inventory must be a name, not a path: %q", *options.inventory)
	}
	if err := config.CheckOfficialExtraModulesVersion(config.OfficialExtraModulesDirectory()); err != nil {
		return nil, err
	}
	_, inventory, err := config.Load(settingsFile, "", *options.inventory, nil)
	if err != nil {
		return nil, err
	}
	state, runnerDir, err := controllerDirectories(settingsFile)
	if err != nil {
		return nil, err
	}
	c := controller.Controller{State: state, RunnerDir: runnerDir}
	c.Credentials = &access.Credentials{Interactive: !*options.nonInteractive && !*options.asJSON && access.IsTerminal(int(os.Stdin.Fd())), Context: ctx, Notify: func(message string) { fmt.Fprintln(os.Stderr, escapeControlCharacters(message)) }}
	c.Secrets = &secrets.Store{PasswordFile: *options.passwordFile, Interactive: c.Credentials.Interactive, Prompt: c.Credentials.ReadPassword}
	release, err := c.UseHistory()
	if err != nil {
		return nil, err
	}
	return &oneTimePlaybookInspector{settingsFile: settingsFile, inventory: inventory, controller: c, asJSON: *options.asJSON, releaseLease: release}, nil
}

// close lets other commands clean up the run history again, and removes the
// expired dryruns of the playbooks this command inspected.
func (i *oneTimePlaybookInspector) close() {
	i.releaseLease()
	for _, r := range i.dryruns {
		cleanupHistory(i.controller, r)
	}
}

// inventoryLine is the first line of the report.
func (i *oneTimePlaybookInspector) inventoryLine() string {
	return fmt.Sprintf("Inventory: %s (%s)", i.inventory.Name, displayPath(i.inventory.Path))
}

// inspect runs the dryrun of one loaded one-time playbook and reads each
// node's state from it. An error is the dryrun's own: the command could not
// do its job, or Ctrl-C stopped it.
func (i *oneTimePlaybookInspector) inspect(ctx context.Context, p config.Project, inv config.Inventory) (checkedOneTimePlaybook, error) {
	c := i.controller
	c.Selection = controller.Identity{EntryPoint: i.settingsFile, Project: p.Project, Inventory: inv.Name, Playbook: p.PlaybookName}
	c.Progress = oneTimePlaybookProgress(os.Stderr, p.PlaybookName, i.asJSON)
	r, err := dryrunOfOneTimePlaybook(ctx, c, p, inv)
	if r == nil {
		return checkedOneTimePlaybook{Name: p.PlaybookName, Done: []nodeState{}, StillNeeded: []nodeState{}, NotKnown: []nodeState{}, NotInspectedReason: fmt.Sprintf("inspected nothing: %v", err)}, err
	}
	i.dryruns = append(i.dryruns, r)
	return checkOfDryrun(r), err
}

// oneTimePlaybookProgress shows on standard error how far the dryrun of one
// playbook is, at most once a second, and the notice of a stop request.
func oneTimePlaybookProgress(w io.Writer, playbook string, asJSON bool) func(*controller.Run) {
	w = terminalSafe(w)
	stopNotice := liveProgress(w, true)
	var last time.Time
	previous := ""
	return func(r *controller.Run) {
		stopNotice(r)
		if asJSON || time.Since(last) < time.Second {
			return
		}
		if line := summary(r); line != previous {
			fmt.Fprintf(w, "Inspecting %s: %s\n", playbook, line)
			previous, last = line, time.Now()
		}
	}
}

// checkOfDryrun reads each node's state from all of its node jobs of one
// dryrun: still needed when a node task proposes a change; otherwise not
// known when the dryrun failed on the node or could not check something
// there, with the first such reason; otherwise done. A node that a fleet
// stage's when skips has nothing to do there.
func checkOfDryrun(r *controller.Run) checkedOneTimePlaybook {
	checked := checkedOneTimePlaybook{Name: r.Project.PlaybookName, RunID: r.ID, Done: []nodeState{}, StillNeeded: []nodeState{}, NotKnown: []nodeState{}}
	type nodeFindings struct {
		dryrunFindings
		stillNeeded, notKnown string
	}
	nodes := map[string]*nodeFindings{}
	var names []string
	var entries dryrunFindings
	for _, en := range r.OrderedEntries() {
		node := nodes[en.Node]
		if node == nil {
			node = &nodeFindings{}
			nodes[en.Node] = node
			names = append(names, en.Node)
		}
		// The node keeps the first reason of each kind, in the playbook's order.
		var own dryrunFindings
		own.response(en.Response)
		if own.changed && node.stillNeeded == "" {
			node.stillNeeded = stillNeededReason(en)
		}
		if (own.failed || own.incomplete) && node.notKnown == "" {
			node.notKnown = notKnownReason(en)
		}
		node.response(en.Response)
		entries.response(en.Response)
	}
	slices.Sort(names)
	for _, name := range names {
		switch node := nodes[name]; {
		case node.changed: //nolint:nilaway // Node names enter this list only when their nodeFindings entry is allocated and inserted.
			checked.StillNeeded = append(checked.StillNeeded, nodeState{name, node.stillNeeded}) //nolint:nilaway // Node names enter this list only when their nodeFindings entry is allocated and inserted.
		case node.failed || node.incomplete: //nolint:nilaway // Node names enter this list only when their nodeFindings entry is allocated and inserted.
			checked.NotKnown = append(checked.NotKnown, nodeState{name, node.notKnown}) //nolint:nilaway // Node names enter this list only when their nodeFindings entry is allocated and inserted.
		default:
			checked.Done = append(checked.Done, nodeState{name, "no node task would change the node"})
		}
	}
	// What no node job says is taken whatever the node jobs say: the nodes
	// of a fleet stage that the dryrun could not find out, and of a loop
	// iteration that started no node job, are in none of the three lists.
	switch part := uninspectedPart(r, entries.failed || entries.incomplete); {
	case part != "":
		checked.NotInspectedReason = "the dryrun did not inspect every node: " + part
	case len(names) == 0:
		checked.NotInspectedReason = "inspected nothing: the playbook selects no node of inventory " + r.InventoryName
	}
	return checked
}

// notKnownReason says in one line why one node job leaves its node's state
// not known, with the cause that the dryrun recorded for it.
func notKnownReason(en controller.Entry) string {
	var own dryrunFindings
	if own.outcome(en.Response.Status, ""); own.failed || own.incomplete {
		cause := strings.Join(strings.Fields(en.Response.Error), " ")
		switch {
		case cause == "":
			return fmt.Sprintf("the dryrun of fleet stage %s ended with status %s", en.FleetStage, en.Response.Status)
		case failedBeforeAConnection(en):
			return "MMX could not connect: " + cause
		}
		return fmt.Sprintf("fleet stage %s: %s", en.FleetStage, cause)
	}
	for _, result := range en.Response.Results {
		var task dryrunFindings
		task.result(result)
		if task.failed {
			return fmt.Sprintf("node task %s failed in the dryrun of fleet stage %s", result.Task, en.FleetStage)
		}
		if task.incomplete {
			return fmt.Sprintf("the dryrun of fleet stage %s could not check node task %s", en.FleetStage, result.Task)
		}
	}
	return fmt.Sprintf("the dryrun of fleet stage %s could not check every node task", en.FleetStage)
}

// failedBeforeAConnection reports that a node job failed while the controller
// process had no connection to its node: the entry has no connection_used.
// A node job that a prerequisite blocked or a stop kept from starting has
// none either and says so in its own error. No shared status function tells
// failed work from blocked work, so this one status is named here.
func failedBeforeAConnection(en controller.Entry) bool {
	return en.Transport == "" && en.Response.Status == "failed"
}

// stillNeededReason names the first node task of a node job that proposes a
// change.
func stillNeededReason(en controller.Entry) string {
	for _, result := range en.Response.Results {
		var task dryrunFindings
		if task.result(result); task.changed {
			return fmt.Sprintf("node task %s of fleet stage %s would change the node", result.Task, en.FleetStage)
		}
	}
	return fmt.Sprintf("fleet stage %s would change the node", en.FleetStage)
}

// uninspectedPart names what the dryrun did not inspect and no node job
// says: the first fleet stage, in the order of their paths, whose nodes it
// could not select; else the first loop iteration that did not complete and
// started no node job; else that the dryrun did not reach every fleet stage.
// These count whatever the node jobs say. A dryrun that did not succeed for
// another reason counts only when no node job explains it; explained says
// that a node job failed or was not checked. The result is "" when the
// dryrun inspected every node that the playbook selects.
func uninspectedPart(r *controller.Run, explained bool) string {
	paths := make([]string, 0, len(r.Selections))
	for path := range r.Selections {
		paths = append(paths, path)
	}
	slices.Sort(paths)
	describe := func(path string, response protocol.Response) string {
		var own dryrunFindings
		if own.response(response); !own.failed && !own.incomplete {
			return ""
		}
		status := response.Status
		if status == "" {
			// A loop iteration without a record did not start.
			status = "pending"
		}
		if response.Error == "" {
			return fmt.Sprintf("fleet stage %s ended with status %s", path, status)
		}
		return fmt.Sprintf("fleet stage %s ended with status %s: %s", path, status, strings.Join(strings.Fields(response.Error), " "))
	}
	for _, path := range paths {
		//nolint:nilaway // Selection paths come from entries allocated by scheduling or loading and retained throughout display.
		if selection := r.Selections[path]; selection.Response.Status != "" {
			if text := describe(path, selection.Response); text != "" {
				return text
			}
		}
	}
	// iteration names the first loop iteration that did not complete: among
	// those that started no node job, or among all of them.
	iteration := func(onlyWithoutNodeJobs bool) string {
		for _, path := range paths {
			for _, item := range r.Selections[path].Items {
				// The node jobs of an iteration have its path before their own.
				name := path + "[" + value.EscapeKey(item.Key) + "]"
				if onlyWithoutNodeJobs && slices.ContainsFunc(r.Entries, func(en controller.Entry) bool { return strings.HasPrefix(en.FleetStage, name+"/") }) {
					continue
				}
				if text := describe(name, r.Selections[path].Iterations[item.Key]); text != "" {
					return text
				}
			}
		}
		return ""
	}
	if text := iteration(true); text != "" {
		return text
	}
	rest := dryrunFindings{}
	rest.selections(r)
	rest.outcome(r.Status, "")
	switch {
	case r.FleetStagesAwaitingNodeSelection:
		return "it ended with status " + r.Status
	case explained || !rest.failed && !rest.incomplete:
		return ""
	}
	// An iteration can fail although each of its node jobs succeeded, such
	// as when the loop's own outputs fail.
	if text := iteration(false); text != "" {
		return text
	}
	return "it ended with status " + r.Status
}

func serverCount(n int) string {
	if n == 1 {
		return "1 server"
	}
	return fmt.Sprintf("%d servers", n)
}

// doneOnServers words how many of the selected nodes no longer need a
// playbook. The first line of a completed playbook uses the same words.
func doneOnServers(done, selected int) string {
	switch {
	case done < selected:
		return fmt.Sprintf("done on %d of %s", done, serverCount(selected))
	case selected == 1:
		return "done on the 1 server"
	}
	return fmt.Sprintf("done on all %d servers", selected)
}

// showCheckedOneTimePlaybook prints one playbook's part of the report: the
// three states in their order, leaving out a state that has no node. The
// automatic output names the nodes that are not done; nodes output names
// every node and adds the dryrun's run ID; summary output prints only counts.
func showCheckedOneTimePlaybook(w io.Writer, p checkedOneTimePlaybook, mode string) {
	// The reasons quote what nodes and the dryrun reported.
	w = terminalSafe(w)
	fmt.Fprintln(w, p.Name)
	line := func(text string, nodes []nodeState, named, withReasons bool) {
		if len(nodes) == 0 {
			return
		}
		if named {
			names := make([]string, len(nodes))
			for i, node := range nodes {
				names[i] = node.Node
				if withReasons {
					names[i] += " (" + node.Reason + ")"
				}
			}
			text += ": " + strings.Join(names, ", ")
		}
		fmt.Fprintln(w, "  "+text)
	}
	line(doneOnServers(len(p.Done), len(p.Done)+len(p.StillNeeded)+len(p.NotKnown)), p.Done, mode == "nodes", false)
	line("still needed on "+serverCount(len(p.StillNeeded)), p.StillNeeded, mode != "summary", false)
	line("not known on "+serverCount(len(p.NotKnown)), p.NotKnown, mode != "summary", true)
	if p.NotInspectedReason != "" {
		fmt.Fprintln(w, "  "+p.NotInspectedReason)
	}
	if mode == "nodes" && p.RunID != "" {
		id := controller.ShortID(p.RunID)
		fmt.Fprintf(w, "  inspected by dryrun %s; mmx results %s --diff shows what a server still needs\n", id, id)
	}
}

// checkOneTimePlaybooksCommand reports, for each one-time playbook outside
// completed/, which nodes of the chosen inventory still need it. It changes
// nothing that the project manages on a node.
func checkOneTimePlaybooksCommand(ctx context.Context, args []string) (int, error) {
	fs := flag.NewFlagSet("check-one-time-playbooks", flag.ContinueOnError)
	options := addOneTimePlaybookOptions(fs)
	connection := addConnectionOption(fs)
	mode := fs.String("output", "auto", "human output: auto (names the nodes that are not done), nodes (names every node and the dryrun's run ID), or summary (counts only)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0, nil
		}
		return 4, err
	}
	if fs.NArg() != 0 {
		return 4, fmt.Errorf("check-one-time-playbooks takes no playbook name: it checks every one-time playbook outside %s/", config.CompletedOneTimePlaybooksDirectory)
	}
	if *mode != "auto" && *mode != "nodes" && *mode != "summary" {
		return 4, fmt.Errorf("--output must be auto, nodes, or summary")
	}
	if err := checkPasswordFileOptions(fs); err != nil {
		return 4, err
	}
	settingsFile, err := config.FindProject(".")
	if err != nil {
		return 4, err
	}
	names, err := config.OutstandingOneTimePlaybooks(settingsFile)
	if err != nil {
		return 4, err
	}
	// The report lists the newest playbook first.
	slices.Reverse(names)
	inspector, err := newOneTimePlaybookInspector(ctx, settingsFile, options)
	if err != nil {
		return 4, err
	}
	defer inspector.close()
	check := oneTimePlaybookCheck{SchemaVersion: 1, Inventory: inspector.inventory.Name, OneTimePlaybooks: []checkedOneTimePlaybook{}}
	if !*options.asJSON {
		fmt.Println(inspector.inventoryLine() + "\n")
		if len(names) == 0 {
			fmt.Printf("No outstanding one-time playbook: %s/ holds none outside completed/.\n", config.OneTimePlaybooksDirectory)
		}
	}
	for _, name := range names {
		checked := checkedOneTimePlaybook{Name: name, Done: []nodeState{}, StillNeeded: []nodeState{}, NotKnown: []nodeState{}}
		p, inv, err := config.Load(settingsFile, name, inspector.inventory.Name, nil)
		if err == nil {
			applyConnectionChoice(inv, *connection)
			err = config.RequireCommandLineValues(p)
		}
		if err != nil {
			checked.NotInspectedReason = fmt.Sprintf("inspected nothing: MMX cannot load the playbook with inventory %s: %v", inspector.inventory.Name, err)
		} else if checked, err = inspector.inspect(ctx, p, inv); err != nil && !errors.Is(err, context.Canceled) {
			return 4, err
		}
		check.OneTimePlaybooks = append(check.OneTimePlaybooks, checked)
		if !*options.asJSON {
			showCheckedOneTimePlaybook(os.Stdout, checked, *mode)
		}
		// After Ctrl-C the report holds the playbooks inspected so far.
		if ctx.Err() != nil {
			break
		}
	}
	if *options.asJSON {
		if code, err := encode(check); err != nil {
			return code, err
		}
	}
	if ctx.Err() != nil {
		return 130, nil
	}
	return oneTimePlaybookExitCode(check.OneTimePlaybooks), nil
}
