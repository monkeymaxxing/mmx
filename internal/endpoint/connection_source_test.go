package endpoint

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"syscall"
	"testing"

	"mmx/internal/config"
	"mmx/internal/modules"
	"mmx/internal/node"
	"mmx/internal/protocol"
)

type connectionSourceProbe struct{ modules.Shell }

func (*connectionSourceProbe) Run(_ context.Context, task modules.NodeTask) protocol.Result {
	r := protocol.Success("unchanged")
	connection, _ := json.Marshal(task.Runtime.ControllerConnection)
	r.Stdout = string(connection)
	return r
}

func TestHTTPSStartedJobCarriesRemoteSourceToModule(t *testing.T) {
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
		remote, local string
		expected      modules.ControllerConnection
	}{
		{"10.20.0.1:43210", "10.20.0.2", modules.ControllerConnection{Kind: protocol.ConnectionHTTPS, SourceAddress: "10.20.0.1", NodeLocalAddress: "10.20.0.2"}},
		{"[2001:db8::1]:43210", "2001:db8::2", modules.ControllerConnection{Kind: protocol.ConnectionHTTPS, SourceAddress: "2001:db8::1", NodeLocalAddress: "2001:db8::2"}},
		{"[::ffff:10.20.0.1]:43210", "::ffff:10.20.0.2", modules.ControllerConnection{Kind: protocol.ConnectionHTTPS, SourceAddress: "10.20.0.1", NodeLocalAddress: "10.20.0.2"}},
		{"10.20.0.1:43210", "127.0.0.1", modules.ControllerConnection{Kind: protocol.ConnectionHTTPS, SourceAddress: "10.20.0.1", NodeLocalAddress: "127.0.0.1"}},
		{"10.20.0.1:43210", "", modules.ControllerConnection{Kind: protocol.ConnectionHTTPS, SourceAddress: "10.20.0.1"}},
		{"", "", modules.ControllerConnection{Kind: protocol.ConnectionHTTPS}},
		{"local-socket", "", modules.ControllerConnection{Kind: protocol.ConnectionHTTPS}},
	} {
		t.Run(test.remote, func(t *testing.T) {
			server := &Server{Node: node.Server{Root: t.TempDir(), RuntimeRoot: t.TempDir(), Binary: "/bin/true"}}
			if err := os.Chmod(server.Node.Root, 0700); err != nil {
				t.Fatal(err)
			}
			job := config.Job{Protocol: protocol.Version, ID: strings.Repeat("b", 64), Project: "source_test", FleetStage: "configure", Node: "server01", Dryrun: true, NodeTasks: []config.Task{{ID: "observe", Module: "shell", ModuleInputs: &modules.Shell{Script: "true"}}}}
			body, err := job.Transport()
			if err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest("POST", "/v1/submit", strings.NewReader(string(body)))
			request.RemoteAddr = test.remote
			request.Header.Set("X-Forwarded-For", "192.0.2.99")
			if test.local != "" {
				request = request.WithContext(context.WithValue(request.Context(), http.LocalAddrContextKey, &net.TCPAddr{IP: net.ParseIP(test.local), Port: 7443}))
			}
			request = request.WithContext(context.WithValue(request.Context(), sessionKey{}, &session{authorized: true}))
			response := httptest.NewRecorder()
			server.ServeHTTP(response, request)
			if response.Code != 200 {
				t.Fatal(response.Code, response.Body.String())
			}
			if err := server.Node.Worker(job.ID, nil); err != nil {
				t.Fatal(err)
			}
			result, err := server.Node.Status(job.ID)
			if err != nil || result.Status != "succeeded" || len(result.Results) != 1 || !observedConnection(t, result.Results[0].Stdout, test.expected) {
				t.Fatal(result, err)
			}
		})
	}
}

func TestHTTPSSubmissionRejectsControllerClaimedSource(t *testing.T) {
	request := httptest.NewRequest("POST", "/v1/submit", strings.NewReader(`{"controller_connection":{"connection_kind":"ssh","source_address":"192.0.2.99","node_local_address":"10.20.0.2"}}`))
	request = request.WithContext(context.WithValue(request.Context(), sessionKey{}, &session{authorized: true}))
	response := httptest.NewRecorder()
	(&Server{}).ServeHTTP(response, request)
	if response.Code != 409 || !strings.Contains(response.Body.String(), "unknown field") {
		t.Fatal(response.Code, response.Body.String())
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
