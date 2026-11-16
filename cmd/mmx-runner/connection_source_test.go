package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"strings"
	"syscall"
	"testing"

	"mmx/internal/config"
	"mmx/internal/modules"
	"mmx/internal/node"
	"mmx/internal/protocol"
)

// The stand-in resource reports the Runtime actually handed to a module by
// the detached worker. No resource command or firewall change is needed.
type connectionSourceProbe struct{ modules.Shell }

func (*connectionSourceProbe) Run(_ context.Context, task modules.NodeTask) protocol.Result {
	r := protocol.Success("unchanged")
	connection, _ := json.Marshal(task.Runtime.ControllerConnection)
	r.Stdout = string(connection)
	return r
}

func TestSSHStartedJobCarriesObservedSourceToModule(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("node admission and worker require root; exercised by native Linux code tests")
	}
	previousUmask := syscall.Umask(0o077)
	t.Cleanup(func() { syscall.Umask(previousUmask) })
	for i := range modules.CoreModules() {
		module := &modules.CoreModules()[i]
		if module.Name == "shell" {
			previous := module.NewInputs
			module.NewInputs = func() modules.Inputs { return &connectionSourceProbe{} }
			t.Cleanup(func() { module.NewInputs = previous })
		}
	}
	for _, test := range []struct {
		connection string
		expected   modules.ControllerConnection
	}{
		{"10.20.0.1 43210 10.20.0.2 22", modules.ControllerConnection{Kind: protocol.ConnectionSSH, SourceAddress: "10.20.0.1", NodeLocalAddress: "10.20.0.2"}},
		{"2001:db8::1 43210 2001:db8::2 22", modules.ControllerConnection{Kind: protocol.ConnectionSSH, SourceAddress: "2001:db8::1", NodeLocalAddress: "2001:db8::2"}},
		{"::ffff:10.20.0.1 43210 ::ffff:10.20.0.2 22", modules.ControllerConnection{Kind: protocol.ConnectionSSH, SourceAddress: "10.20.0.1", NodeLocalAddress: "10.20.0.2"}},
		{"", modules.ControllerConnection{Kind: protocol.ConnectionSSH}},
		{"invalid 1 10.20.0.2 22", modules.ControllerConnection{Kind: protocol.ConnectionSSH, NodeLocalAddress: "10.20.0.2"}},
		{"10.20.0.1 1 invalid 22", modules.ControllerConnection{Kind: protocol.ConnectionSSH, SourceAddress: "10.20.0.1"}},
		{"10.20.0.1", modules.ControllerConnection{Kind: protocol.ConnectionSSH}},
	} {
		t.Run(test.connection, func(t *testing.T) {
			t.Setenv("SSH_CONNECTION", test.connection)
			server := node.Server{Root: t.TempDir(), RuntimeRoot: t.TempDir(), Binary: "/bin/true"}
			if err := os.Chmod(server.Root, 0700); err != nil {
				t.Fatal(err)
			}
			job := config.Job{Protocol: protocol.Version, ID: strings.Repeat("a", 64), Project: "source_test", FleetStage: "configure", Node: "server01", Dryrun: true, NodeTasks: []config.Task{{ID: "observe", Module: "shell", ModuleInputs: &modules.Shell{Script: "true"}}}}
			body, err := job.Transport()
			if err != nil {
				t.Fatal(err)
			}
			if err := answerOverSSH(context.Background(), server, protocol.RequestSubmit, bytes.NewReader(body), io.Discard); err != nil {
				t.Fatal(err)
			}
			// /bin/true releases the inherited worker lock. Run the real worker
			// here, reading only the admitted job and connection records.
			t.Setenv("SSH_CONNECTION", "192.0.2.99 1 10.20.0.2 22")
			if err := server.Worker(job.ID, nil); err != nil {
				t.Fatal(err)
			}
			result, err := server.Status(job.ID)
			if err != nil || result.Status != "succeeded" || len(result.Results) != 1 || !observedConnection(t, result.Results[0].Stdout, test.expected) {
				t.Fatal(result, err)
			}
		})
	}
}

func TestSSHSubmissionRejectsControllerClaimedSource(t *testing.T) {
	t.Setenv("SSH_CONNECTION", "10.20.0.1 43210 10.20.0.2 22")
	err := answerOverSSH(context.Background(), node.Server{}, protocol.RequestSubmit, strings.NewReader(`{"controller_connection":{"connection_kind":"ssh","source_address":"192.0.2.99","node_local_address":"10.20.0.2"}}`), io.Discard)
	if err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatal(err)
	}
}

func observedConnection(t *testing.T, text string, expected modules.ControllerConnection) bool {
	t.Helper()
	var observed modules.ControllerConnection
	if err := json.Unmarshal([]byte(text), &observed); err != nil {
		t.Fatal(err)
	}
	return observed == expected
}
