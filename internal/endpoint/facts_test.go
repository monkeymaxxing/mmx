package endpoint

import (
	"context"
	"testing"

	"golang.org/x/crypto/ssh"

	"mmx/internal/facts"
	"mmx/internal/node"
)

// The node answers a facts request over HTTPS with the facts a node job
// would measure, so the controller process can check a fleet stage's when.
func TestHTTPSAnswersAFactsRequest(t *testing.T) {
	identity, err := LoadIdentity(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	signer := testSigner(t)
	s := &Server{Info: Info{NodeID: "node", TLSPin: identity.Pin}, Node: node.Server{Root: t.TempDir()},
		Authorize: func(context.Context, string, ssh.PublicKey, string, string) error { return nil }}
	server := serveTLS(t, identity, s, nil)
	c := NewClient(server.URL, "admin", identity.Pin, []ssh.Signer{signer})
	defer c.Close()
	var got facts.Snapshot
	if err := c.Call(context.Background(), "POST", "/v1/facts", []byte(`{"node":"web01"}`), &got); err != nil || got.Values["name"] != "web01" {
		t.Fatal(got, err)
	}
	var bad facts.Snapshot
	if err := c.Call(context.Background(), "POST", "/v1/facts", []byte(`{"node":"../x"}`), &bad); err == nil {
		t.Fatal("accepted a name that is no inventory name")
	}
}
