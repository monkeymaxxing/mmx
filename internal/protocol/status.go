package protocol

// Every job, entry, iteration, task, and run status. Retry selection, run
// status, pruning, exit codes, loop outcomes, and display derive from this table.
//
//	terminal:  the work reached an outcome (a node job may be collected)
//	completed: the work needs no retry
//	uncertain: the node may still be working; reconcile by job ID
//	failure:   the work failed or could not start; the run fails
//	rank:      a loop iteration or server shows its highest-ranked status
var statuses = map[string]struct {
	terminal, completed, uncertain, failure bool
	rank                                    int
}{
	"succeeded":   {terminal: true, completed: true},
	"skipped":     {terminal: true, completed: true},          // condition was false
	"not_checked": {terminal: true, completed: true, rank: 1}, // dryrun could not check
	"pending":     {rank: 2},                                  // not started; "" means the same
	"stopped":     {terminal: true, rank: 3},                  // stop requested; without an attempt, a stop or on_node_failure kept it from starting
	"blocked":     {terminal: true, failure: true, rank: 4},   // prerequisite or input unavailable
	"failed":      {terminal: true, failure: true, rank: 5},   // includes a node rejection
	"running":     {uncertain: true, rank: 6},                 // node is working
	"rebooting":   {uncertain: true, rank: 7},                 // reboot requested; the node has not answered on a new boot yet
	"not_found":   {uncertain: true, rank: 8},                 // node has no record yet
	"unknown":     {uncertain: true, rank: 9},                 // contact lost after the job ID was published
	"interrupted": {terminal: true},                           // run only: unfinished work without failure or stop
	"stopping":    {},                                         // run only: stop requested while work is active
}

func lookup(s string) string {
	if s == "" {
		return "pending"
	}
	return s
}
func Terminal(s string) bool  { return statuses[lookup(s)].terminal }
func Completed(s string) bool { return statuses[lookup(s)].completed }
func Uncertain(s string) bool { return statuses[lookup(s)].uncertain }
func Failure(s string) bool   { return statuses[lookup(s)].failure }
func Rank(s string) int       { return statuses[lookup(s)].rank }

// Unconfirmed is uncertain work the controller could not reach or find.
func Unconfirmed(s string) bool { return Uncertain(s) && s != "running" && s != "rebooting" }

// The one change precedence: changed > would_change > unknown > unchanged.
var changes = map[string]int{"unchanged": 0, "unknown": 1, "would_change": 2, "changed": 3}

// CombineChange returns the stronger of two changes.
func CombineChange(a, b string) string {
	if changes[b] > changes[a] {
		return b
	}
	if changes[a] == 0 {
		return "unchanged"
	}
	return a
}
