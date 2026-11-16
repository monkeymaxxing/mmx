package endpoint

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"mmx/internal/node"
	"mmx/internal/protocol"
)

// Every JSON node request is served at POST /v1/<name> and reads up to
// protocol.RequestLimit bytes, the limit mmx-runner applies over SSH
// (cmd/mmx-runner's TestEveryNodeRequestReadsTheSameLimitOverSSH). A body of
// exactly the limit reaches the node and fails only as JSON; one byte more is
// refused as too large.
func TestEveryNodeRequestReadsTheSameLimitOverHTTPS(t *testing.T) {
	body := bytes.Repeat([]byte(" "), protocol.RequestLimit+1)
	server := &Server{Node: node.Server{Root: t.TempDir()}}
	for _, name := range node.RequestNames() {
		for _, test := range []struct {
			size   int
			status int
		}{
			{protocol.RequestLimit, http.StatusConflict},
			{protocol.RequestLimit + 1, http.StatusRequestEntityTooLarge},
		} {
			request := httptest.NewRequest("POST", "/v1/"+name, bytes.NewReader(body[:test.size]))
			request = request.WithContext(context.WithValue(request.Context(), sessionKey{}, &session{authorized: true}))
			response := httptest.NewRecorder()
			server.ServeHTTP(response, request)
			if response.Code != test.status {
				t.Fatalf("%s with %d bytes answered %d, want %d: %s", name, test.size, response.Code, test.status, response.Body)
			}
		}
	}
	request := httptest.NewRequest("POST", "/v1/no-such-request", nil)
	request = request.WithContext(context.WithValue(request.Context(), sessionKey{}, &session{authorized: true}))
	response := httptest.NewRecorder()
	server.ServeHTTP(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatal(response.Code, response.Body)
	}
}
