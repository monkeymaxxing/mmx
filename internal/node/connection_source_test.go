package node

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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

func TestWorkerUsesAdmittedConnectionAndMissingConnectionStaysUnknown(t *testing.T) {
	for i := range modules.CoreModules() {
		module := &modules.CoreModules()[i]
		if module.Name == "shell" {
			previous := module.NewInputs
			module.NewInputs = func() modules.Inputs { return &connectionSourceProbe{} }
			t.Cleanup(func() { module.NewInputs = previous })
		}
	}
	for _, connection := range []modules.ControllerConnection{
		{Kind: protocol.ConnectionSSH, SourceAddress: "10.20.0.1", NodeLocalAddress: "10.20.0.2"},
		{Kind: protocol.ConnectionHTTPS, SourceAddress: "2001:db8::1", NodeLocalAddress: "2001:db8::2"},
		{},
	} {
		t.Run(string(connection.Kind), func(t *testing.T) {
			server := Server{Root: t.TempDir(), RuntimeRoot: t.TempDir()}
			job := config.Job{Protocol: protocol.Version, ID: strings.Repeat("a", 64), Project: "source_test", FleetStage: "configure", Node: "server01", Dryrun: true, NodeTasks: []config.Task{{ID: "observe", Module: "shell", ModuleInputs: &modules.Shell{Script: "true"}}}}
			body, err := job.Transport()
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(server.jobDir(job.ID), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(server.jobDir(job.ID), "request.json"), body, 0600); err != nil {
				t.Fatal(err)
			}
			if connection.Kind != "" {
				record, _ := json.Marshal(connection)
				if err := os.WriteFile(filepath.Join(server.jobDir(job.ID), "controller_connection.json"), record, 0600); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("SSH_CONNECTION", "192.0.2.99 12345 10.20.0.2 22")
			result, err := server.work(job.ID)
			if err != nil || result.Status != "succeeded" || len(result.Results) != 1 {
				t.Fatal(result, err)
			}
			var observed modules.ControllerConnection
			if err := json.Unmarshal([]byte(result.Results[0].Stdout), &observed); err != nil || observed != connection {
				t.Fatal(result, err)
			}
		})
	}
}

func TestRepeatedSubmissionKeepsFirstObservedConnection(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("node admission requires root; exercised by native Linux code tests")
	}
	runner, release := fakeWorker(t)
	server := Server{Root: t.TempDir(), RuntimeRoot: t.TempDir(), Binary: runner}
	if err := os.Chmod(server.Root, 0700); err != nil {
		t.Fatal(err)
	}
	job := config.Job{Protocol: protocol.Version, ID: strings.Repeat("b", 64), Project: "source_test", FleetStage: "configure", Node: "server01"}
	body, err := job.Transport()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := server.Submit(body, modules.ControllerConnection{Kind: protocol.ConnectionSSH, SourceAddress: "::ffff:10.20.0.1", NodeLocalAddress: "::ffff:10.20.0.2"}); err != nil {
		t.Fatal(err)
	}
	if _, err := server.Submit(body, modules.ControllerConnection{Kind: protocol.ConnectionHTTPS, SourceAddress: "192.0.2.99", NodeLocalAddress: "127.0.0.1"}); err != nil {
		t.Fatal(err)
	}
	connection, err := os.ReadFile(filepath.Join(server.jobDir(job.ID), "controller_connection.json"))
	var record modules.ControllerConnection
	if err != nil || json.Unmarshal(connection, &record) != nil || record != (modules.ControllerConnection{Kind: protocol.ConnectionSSH, SourceAddress: "10.20.0.1", NodeLocalAddress: "10.20.0.2"}) {
		t.Fatal(string(connection), err)
	}
	release(server, job.ID)
}
