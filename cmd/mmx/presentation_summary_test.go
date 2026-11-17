package main

import (
	"bytes"
	"strings"
	"testing"

	"mmx/internal/controller"
	"mmx/internal/protocol"
)

// summaryLines returns the summary lines that start a report: the lines
// before the first detail line, which names a fleet stage.
func summaryLines(t *testing.T, r *controller.Run, mode string) string {
	t.Helper()
	var out bytes.Buffer
	if err := (&presentation{mode: mode}).human(&out, r, false); err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, line := range strings.SplitAfter(out.String(), "\n") {
		if strings.Contains(line, " / ") {
			break
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "")
}

func done(task, status, change string) protocol.Result {
	return protocol.Result{Task: task, Status: status, Change: change}
}

func TestTheSummarySaysWhatChangedByNameAndCountsTheRest(t *testing.T) {
	for name, tc := range map[string]struct {
		dryrun  bool
		results []protocol.Result
		want    string
	}{
		"a dryrun": {true, []protocol.Result{
			done("install_nginx", "succeeded", "would_change"), done("write_site_file", "succeeded", "would_change"), done("enable_nginx", "succeeded", "would_change"),
			done("check_config", "not_checked", "unknown"), done("check_home_page", "not_checked", "unknown"),
			done("a", "succeeded", "unchanged"), done("b", "succeeded", "unchanged"), done("c", "succeeded", "unchanged"), done("d", "succeeded", "unchanged"), done("e", "succeeded", "unchanged"),
		}, "web01: 3 node tasks would change (install_nginx, write_site_file, enable_nginx). 2 not checked yet: they usually check what earlier node tasks install or create; mmx run checks them for real. 5 unchanged. 0 failed.\n"},
		"a run": {false, []protocol.Result{
			done("install_nginx", "succeeded", "changed"), done("write_site_file", "succeeded", "changed"), done("a", "succeeded", "unchanged"),
		}, "web01: 2 node tasks changed (install_nginx, write_site_file). 1 unchanged. 0 failed.\n"},
		"one not checked, in one loop item": {true, []protocol.Result{
			{Task: "write_sites", Status: "succeeded", Change: "unknown", Items: []protocol.Result{done("", "succeeded", "unchanged"), done("", "not_checked", "unknown")}},
		}, "web01: 1 node task not checked yet: it usually checks what an earlier node task installs or creates; mmx run checks it for real. 0 failed.\n"},
		"nothing to change": {false, []protocol.Result{done("a", "succeeded", "unchanged")},
			"web01: 1 node task unchanged. 0 failed.\n"},
		"a shell task without a dryrun command, and a false when": {true, []protocol.Result{
			done("migrate", "skipped", "unknown"), done("only_on_staging", "skipped", "unchanged"),
		}, "web01: 1 node task may change (migrate). 1 skipped. 0 failed.\n"},
		"a failure": {false, []protocol.Result{done("install_nginx", "succeeded", "changed"), done("reload_nginx", "failed", "unchanged")},
			"web01: 1 node task changed (install_nginx). 1 failed (reload_nginx).\n"},
		"only a failure": {false, []protocol.Result{done("reload_nginx", "blocked", "unchanged")},
			"web01: 1 node task failed (reload_nginx).\n"},
		// The block's own result sums up the node tasks inside it.
		"a node task block": {false, []protocol.Result{
			done("shop_site/changes/write_site_file", "succeeded", "changed"), done("shop_site/activate/reload_nginx", "succeeded", "changed"), done("shop_site", "succeeded", "changed"),
		}, "web01: 2 node tasks changed (shop_site/changes/write_site_file, shop_site/activate/reload_nginx). 0 failed.\n"},
		"more names than the line shows": {false, []protocol.Result{
			done("t1", "succeeded", "changed"), done("t2", "succeeded", "changed"), done("t3", "succeeded", "changed"), done("t4", "succeeded", "changed"),
			done("t5", "succeeded", "changed"), done("t6", "succeeded", "changed"), done("t7", "succeeded", "changed"),
		}, "web01: 7 node tasks changed (t1, t2, t3, t4, t5, and 2 more). 0 failed.\n"},
	} {
		t.Run(name, func(t *testing.T) {
			status := "succeeded"
			for _, result := range tc.results {
				if protocol.Failure(result.Status) {
					status = "failed"
				}
			}
			r := &controller.Run{ID: "x", Dryrun: tc.dryrun, Entries: []controller.Entry{{Node: "web01", FleetStage: "set_up_nginx", NodeJobAttempt: 1,
				Response: protocol.Response{Status: status, Results: tc.results}}}}
			if got := summaryLines(t, r, "nodes"); got != tc.want {
				t.Fatalf("got  %q\nwant %q", got, tc.want)
			}
		})
	}
}

// A server's line adds up its fleet stages, and names a fleet stage that
// failed or did not finish outside its node tasks, such as a server MMX
// could not reach.
func TestTheSummaryCountsEachServerAcrossFleetStages(t *testing.T) {
	r := &controller.Run{ID: "x", Entries: []controller.Entry{
		{Node: "web01", FleetStage: "base", NodeJobAttempt: 1, Response: protocol.Response{Status: "succeeded", Results: []protocol.Result{done("install_nginx", "succeeded", "changed")}}},
		{Node: "web01", FleetStage: "site", NodeJobAttempt: 1, Response: protocol.Response{Status: "succeeded", Results: []protocol.Result{done("write_site_file", "succeeded", "changed"), done("a", "succeeded", "unchanged")}}},
		{Node: "web02", FleetStage: "base", Response: protocol.Response{Status: "failed", Error: "SSH 203.0.113.12: connection refused"}},
		{Node: "web03", FleetStage: "base", Response: protocol.Response{Status: "unknown"}},
	}}
	want := "web01: 2 node tasks changed (install_nginx, write_site_file). 1 unchanged. 0 failed.\n" +
		"web02: 1 failed (fleet stage base).\n" +
		"web03: 1 not finished (fleet stage base). 0 failed.\n"
	if got := summaryLines(t, r, "nodes"); got != want {
		t.Fatalf("got\n%swant\n%s", got, want)
	}
}

// With grouped output, servers whose lines are the same share one line, as
// the detail lines do.
func TestGroupedOutputSharesOneSummaryLineAmongTheSameServers(t *testing.T) {
	entry := func(node, change string) controller.Entry {
		return controller.Entry{Node: node, FleetStage: "site", NodeJobAttempt: 1, Response: protocol.Response{Status: "succeeded", Results: []protocol.Result{done("write_site_file", "succeeded", change)}}}
	}
	r := &controller.Run{ID: "x", Entries: []controller.Entry{entry("web01", "changed"), entry("web02", "unchanged"), entry("web03", "changed")}}
	want := "2 nodes [web01, web03]: 1 node task changed (write_site_file). 0 failed.\n" +
		"1 node [web02]: 1 node task unchanged. 0 failed.\n"
	if got := summaryLines(t, r, "summary"); got != want {
		t.Fatalf("got\n%swant\n%s", got, want)
	}
	want = "web01: 1 node task changed (write_site_file). 0 failed.\n" +
		"web02: 1 node task unchanged. 0 failed.\n" +
		"web03: 1 node task changed (write_site_file). 0 failed.\n"
	if got := summaryLines(t, r, "nodes"); got != want {
		t.Fatalf("got\n%swant\n%s", got, want)
	}
}
