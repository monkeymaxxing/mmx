package controller

import (
	"encoding/json"
	"mmx/internal/config"
	"mmx/internal/protocol"
	"mmx/internal/value"
	"testing"
)

func TestNewAttemptUsesSavedDefinitionsAndCurrentRunOutputs(t *testing.T) {
	c, r := fleet(t, config.FleetStage{ID: "consume", Nodes: "a", NodeTasks: []config.Task{{Show: "{{ .outputs.produce.a.number }}"}}})
	r.Project.Vars = map[string]any{"large": json.Number("9007199254740993")}
	if err := c.createRun(r); err != nil {
		t.Fatal(err)
	}
	saved, err := c.Load(r.ID, false)
	if err != nil {
		t.Fatal(err)
	}
	en := saved.Entries[0]
	en.scope = value.Scope{"outputs": map[string]any{"produce": map[string]any{"a": map[string]any{"number": json.Number("9007199254740993")}}}}
	en.JobID = "next"
	raw, err := json.Marshal(mustJob(nodeJob(saved, saved.Project.FleetStages[0], en)))
	if err != nil {
		t.Fatal(err)
	}
	var j config.Job
	if err := protocol.Decode(raw, &j); err != nil {
		t.Fatal(err)
	}
	if j.ID != "next" || j.Vars["large"] != json.Number("9007199254740993") || j.NodeTasks[0].Show != "{{ .outputs.produce.a.number }}" {
		t.Fatalf("%s", raw)
	}
	got, err := value.Scope(j.Scope).Get(".outputs.produce.a.number")
	if err != nil || got != json.Number("9007199254740993") {
		t.Fatal(got, err)
	}
}
