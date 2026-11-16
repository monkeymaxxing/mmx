package endpoint

import (
	"context"
	"encoding/json"
	"golang.org/x/crypto/ssh"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"mmx/internal/node"
	"mmx/internal/protocol"
)

func TestReachabilityRouteAuthenticatesAndSuppliesHTTPSKind(t *testing.T) {
	for _, test := range []struct {
		name, extra string
		authorized  bool
		status      int
	}{
		{"unauthenticated", "", false, 403},
		{"https", "", true, 200},
		{"claimed_ssh", `,"received_connection_kind":"ssh"`, true, 409},
		{"wrong_phase", `,"unexpected":true`, true, 409},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := &Server{Node: node.Server{Root: t.TempDir()}}
			body := `{"node_job_id":"` + strings.Repeat("a", 64) + `","owed_restore_id":"0123456789abcdef","confirmation_phase":"before_change"` + test.extra + `}`
			request := httptest.NewRequest("POST", "/v1/confirm-reachability", strings.NewReader(body))
			request = request.WithContext(context.WithValue(request.Context(), sessionKey{}, &session{authorized: test.authorized}))
			response := httptest.NewRecorder()
			server.ServeHTTP(response, request)
			if response.Code != test.status {
				t.Fatal(response.Code, response.Body.String())
			}
			if test.status == 200 {
				var answer protocol.ReachabilityConfirmationResponse
				if err := json.Unmarshal(response.Body.Bytes(), &answer); err != nil {
					t.Fatal(err)
				}
				if answer.ReceivedConnectionKind != protocol.ConnectionHTTPS || answer.ConfirmationCounted || answer.ConfirmationReason != "owed_restore_not_found" {
					t.Fatal(answer)
				}
			}
		})
	}
}

func TestConfirmationHTTPSConnectionsAreFreshAndProgressStillReuses(t *testing.T) {
	identity, err := LoadIdentity(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	signer := testSigner(t)
	var authorizations atomic.Int32
	server := &Server{Info: Info{TLSPin: identity.Pin}, Node: node.Server{Root: t.TempDir()}, Authorize: func(context.Context, string, ssh.PublicKey, string, string) error { authorizations.Add(1); return nil }}
	https := serveTLS(t, identity, server, nil)
	progress := NewClient(https.URL, "admin", identity.Pin, []ssh.Signer{signer})
	defer progress.Close()
	var info Info
	for range 2 {
		if err := progress.Call(context.Background(), "GET", "/v1/info", nil, &info); err != nil {
			t.Fatal(err)
		}
	}
	if authorizations.Load() != 1 {
		t.Fatal("ordinary requests did not reuse connection", authorizations.Load())
	}
	body, _ := json.Marshal(protocol.ReachabilityConfirmationRequest{NodeJobID: strings.Repeat("a", 64), OwedRestoreID: "0123456789abcdef", ConfirmationPhase: protocol.ConfirmationBeforeChange})
	for range 2 {
		client := NewConfirmationClient(https.URL, "admin", identity.Pin, []ssh.Signer{signer})
		for range 2 {
			var answer protocol.ReachabilityConfirmationResponse
			if err := client.Call(context.Background(), "POST", "/v1/confirm-reachability", body, &answer); err != nil {
				t.Fatal(err)
			}
			if answer.ReceivedConnectionKind != protocol.ConnectionHTTPS {
				t.Fatal(answer)
			}
		}
		client.Close()
	}
	if authorizations.Load() != 5 {
		t.Fatal("confirmation reused a connection", authorizations.Load())
	}
	if err := progress.Call(context.Background(), "GET", "/v1/info", nil, &info); err != nil {
		t.Fatal(err)
	}
	if authorizations.Load() != 5 {
		t.Fatal("confirmation closed progress connection", authorizations.Load())
	}
}
