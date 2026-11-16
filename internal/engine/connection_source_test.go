package engine

import (
	"context"
	"encoding/json"
	"testing"

	"mmx/internal/config"
	"mmx/internal/modules"
	"mmx/internal/protocol"
)

type connectionSourceProbe struct{ modules.Shell }

func (*connectionSourceProbe) Run(_ context.Context, task modules.NodeTask) protocol.Result {
	r := protocol.Success("unchanged")
	connection, _ := json.Marshal(task.Runtime.ControllerConnection)
	r.Stdout = string(connection)
	return r
}

func TestNodeJobConnectionReachesModuleRuntime(t *testing.T) {
	for _, connection := range []modules.ControllerConnection{
		{Kind: protocol.ConnectionSSH, SourceAddress: "10.20.0.1", NodeLocalAddress: "10.20.0.2"},
		{Kind: protocol.ConnectionHTTPS, SourceAddress: "2001:db8::1", NodeLocalAddress: "2001:db8::2"},
		{},
	} {
		e := Engine{ControllerConnection: connection, RuntimeRoot: t.TempDir()}
		result := e.Run(context.Background(), config.Job{Node: "server01", Dryrun: true, NodeTasks: []config.Task{{ID: "observe", Module: "shell", Resolved: true, ModuleInputs: &connectionSourceProbe{}}}})
		if result.Status != "succeeded" || len(result.Results) != 1 {
			t.Fatal(result)
		}
		var observed modules.ControllerConnection
		if err := json.Unmarshal([]byte(result.Results[0].Stdout), &observed); err != nil || observed != connection {
			t.Fatal(result, err)
		}
	}
}
