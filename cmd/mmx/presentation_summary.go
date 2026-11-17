package main

import (
	"fmt"
	"io"
	"strings"

	"mmx/internal/controller"
	"mmx/internal/protocol"
)

// shownNames is how many names a summary line lists before "and N more".
// Five keep the line short and still name the usual handful of changes;
// the detail lines below the summary name every node task.
const shownNames = 5

// serverSummary is what happened to one server's node tasks in a run or a
// dryrun, across its fleet stages. Lists hold names; the other fields count.
type serverSummary struct {
	changed    []string // changed, or in a dryrun would change
	mayChange  []string // the module could not tell, such as a shell task without a dryrun command
	notChecked int      // a dryrun could not check them before earlier node tasks run
	unchanged  int
	skipped    int
	unfinished []string // node tasks, or fleet stages, that have not finished
	failed     []string // node tasks, or fleet stages that failed outside a node task
}

// summarizeServers writes one line for each server, before the detail lines:
// what changed or would change, by name, and how many node tasks were
// unchanged and failed. Servers whose lines are the same share one line when
// grouped.
func summarizeServers(w io.Writer, r *controller.Run, grouped bool) {
	var order []string
	byServer := map[string]*serverSummary{}
	for _, en := range r.OrderedEntries() {
		s := byServer[en.Node]
		if s == nil {
			s = &serverSummary{}
			byServer[en.Node] = s
			order = append(order, en.Node)
		}
		s.add(en)
	}
	type line struct {
		text  string
		nodes []string
	}
	var lines []line
	indices := map[string]int{}
	for _, node := range order {
		text := byServer[node].text(r.Dryrun)
		if i, ok := indices[text]; grouped && ok {
			lines[i].nodes = append(lines[i].nodes, node)
			continue
		}
		indices[text] = len(lines)
		lines = append(lines, line{text, []string{node}})
	}
	for _, l := range lines {
		fmt.Fprintf(w, "%s: %s\n", nodeLabel(l.nodes, grouped), l.text)
	}
}

// add counts one fleet stage's node tasks on the server. A node task block
// or role call is counted through the node tasks inside it, so that no work
// counts twice.
func (s *serverSummary) add(en controller.Entry) {
	results := en.Response.Results
	taskFailed, taskUnfinished := false, false
	for _, result := range results {
		if wrapsOthers(result.Task, results) {
			continue
		}
		switch {
		case protocol.Failure(result.Status):
			s.failed, taskFailed = append(s.failed, result.Task), true
		case result.Status == "not_checked" || hasItemNotChecked(result.Items):
			s.notChecked++
		case !protocol.Completed(result.Status):
			s.unfinished, taskUnfinished = append(s.unfinished, result.Task), true
		case result.Change == "changed" || result.Change == "would_change":
			s.changed = append(s.changed, result.Task)
		case result.Change == "unknown":
			s.mayChange = append(s.mayChange, result.Task)
		case result.Status == "skipped":
			s.skipped++
		default:
			s.unchanged++
		}
	}
	// A fleet stage can fail or stop outside its node tasks, such as when
	// MMX cannot reach the server.
	status := en.Response.Status
	switch {
	case protocol.Failure(status) && !taskFailed:
		s.failed = append(s.failed, "fleet stage "+en.FleetStage)
	case !protocol.Completed(status) && !protocol.Failure(status) && !taskUnfinished:
		s.unfinished = append(s.unfinished, "fleet stage "+en.FleetStage)
	}
}

// wrapsOthers reports a node task block or role call: a result whose name
// starts the names of the node tasks inside it.
func wrapsOthers(task string, results []protocol.Result) bool {
	for _, other := range results {
		if strings.HasPrefix(other.Task, task+"/") {
			return true
		}
	}
	return false
}

func hasItemNotChecked(items []protocol.Result) bool {
	for _, item := range items {
		if item.Status == "not_checked" || hasItemNotChecked(item.Items) {
			return true
		}
	}
	return false
}

// text is the summary after the server's name, such as "3 node tasks would
// change (install_nginx, ...). 5 unchanged. 0 failed." A part whose count is
// zero is left out, except the failed count. The first part that counts
// node tasks says "node tasks".
func (s *serverSummary) text(dryrun bool) string {
	var parts []string
	part := func(count int, what, names string, countsNodeTasks bool) {
		number := fmt.Sprint(count)
		if len(parts) == 0 && countsNodeTasks {
			number += " node tasks"
			if count == 1 {
				number = "1 node task"
			}
		}
		parts = append(parts, number+" "+what+names)
	}
	changed, mayChange := "changed", "may have changed"
	if dryrun {
		changed, mayChange = "would change", "may change"
	}
	if n := len(s.changed); n > 0 {
		part(n, changed, listNames(s.changed), true)
	}
	if n := len(s.mayChange); n > 0 {
		part(n, mayChange, listNames(s.mayChange), true)
	}
	switch {
	case s.notChecked == 1:
		part(1, "not checked yet", ": it usually checks what an earlier node task installs or creates; mmx run checks it for real", true)
	case s.notChecked > 1:
		part(s.notChecked, "not checked yet", ": they usually check what earlier node tasks install or create; mmx run checks them for real", true)
	}
	if s.unchanged > 0 {
		part(s.unchanged, "unchanged", "", true)
	}
	if s.skipped > 0 {
		part(s.skipped, "skipped", "", true)
	}
	if n := len(s.unfinished); n > 0 {
		part(n, "not finished", listNames(s.unfinished), !namesFleetStage(s.unfinished))
	}
	part(len(s.failed), "failed", listNames(s.failed), !namesFleetStage(s.failed))
	return strings.Join(parts, ". ") + "."
}

// namesFleetStage reports a list that names a fleet stage, not only node
// tasks, so its count is not a count of node tasks.
func namesFleetStage(names []string) bool {
	for _, name := range names {
		if strings.HasPrefix(name, "fleet stage ") {
			return true
		}
	}
	return false
}

// listNames is " (a, b, c)", or with more than shownNames names
// " (a, b, c, d, e, and 3 more)"; nothing for no names.
func listNames(names []string) string {
	if len(names) == 0 {
		return ""
	}
	if len(names) > shownNames {
		return fmt.Sprintf(" (%s, and %d more)", strings.Join(names[:shownNames], ", "), len(names)-shownNames)
	}
	return " (" + strings.Join(names, ", ") + ")"
}
