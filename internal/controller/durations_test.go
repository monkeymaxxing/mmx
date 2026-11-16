package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	"mmx/internal/config"
	"mmx/internal/modules"
)

// A node job carries the node's value of the var that a duration reads: a
// time_limit from a var stops the node task on the node, and the reboot at
// the end of a fleet stage waits as long as the node's var says.
func TestANodeJobCarriesTheNodesDurations(t *testing.T) {
	limit := "{{ .vars.limit }}"
	p := config.Project{FleetStages: []config.FleetStage{{ID: "work", Nodes: "a", NodeTasks: []config.Task{
		{ID: "slow", Module: "shell", ModuleInputs: &modules.Shell{Script: "sleep 5"}, TimeLimit: &limit, Retry: &config.Retry{Disabled: true}},
	}}}}
	c, r := playbookRun(t, p, nil, false)
	r.Project.Vars["limit"] = "200ms"
	c.executeNode = func(ctx context.Context, r *Run, s config.FleetStage, en Entry, _ bool, _ func(Entry) error) (Entry, error) {
		return runOnNode(ctx, r, s, en)
	}
	started := time.Now()
	if err := c.Execute(context.Background(), r, false); err != nil {
		t.Fatal(err)
	}
	if en := entry(t, r, "work", "a"); en.Response.Status != "failed" || !strings.Contains(en.Response.Error, "time_limit exceeded") || time.Since(started) > 4*time.Second {
		t.Fatalf("%+v after %s", en.Response, time.Since(started))
	}

	r.Project.Vars["reboot_wait"] = "42s"
	reboot := config.FleetStage{ID: "restart", NodeTasks: []config.Task{{Module: "reboot", ModuleInputs: &modules.Reboot{WaitAtMost: "{{ .vars.reboot_wait }}"}}}}
	if wait := rebootWaitLimit(r, reboot, "a"); wait != 42*time.Second {
		t.Fatal(wait)
	}
}
