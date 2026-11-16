package endpoint

import (
	"context"
	"encoding/json"
	"runtime"
	"testing"

	"golang.org/x/crypto/ssh"

	"mmx/internal/engine"
	"mmx/internal/facts"
	"mmx/internal/node"
	"mmx/internal/protocol"
)

// The node answers over HTTPS whether it refuses a reboot because it is the
// machine that runs the controller process. On Linux the node here is the
// test's own machine: it refuses for its own boot ID and machine ID, and
// does not refuse for another boot.
func TestHTTPSAnswersARebootRefusalRequest(t *testing.T) {
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
	ask := func(controller protocol.ControllerMachine) (protocol.RebootRefusalResponse, error) {
		body, err := json.Marshal(protocol.RebootRefusalRequest{ControllerMachine: controller})
		if err != nil {
			t.Fatal(err)
		}
		var answer protocol.RebootRefusalResponse
		return answer, c.Call(context.Background(), "POST", "/v1/reboot-refusal", body, &answer)
	}
	// A controller machine without a boot ID, such as a Mac, is never the node.
	if answer, err := ask(protocol.ControllerMachine{}); err != nil || answer.Refusal != "" {
		t.Fatal(answer, err)
	}
	var unknownField protocol.RebootRefusalResponse
	if err := c.Call(context.Background(), "POST", "/v1/reboot-refusal", []byte(`{"controller_machine":{"boot_id":"b","machine_id":"m"}}`), &unknownField); err == nil {
		t.Fatal("accepted a request with a field that the protocol does not have")
	}
	if runtime.GOOS != "linux" {
		return
	}
	boot, err := facts.BootID()
	if err != nil {
		t.Fatal(err)
	}
	if answer, err := ask(protocol.ControllerMachine{BootID: "another-boot"}); err != nil || answer.Refusal != "" {
		t.Fatal(answer, err)
	}
	// A test machine without /etc/machine-id, such as a plain container
	// image, has no identity to compare.
	if machine, err := facts.MachineIDHash(); err == nil {
		if answer, err := ask(protocol.ControllerMachine{BootID: boot, MachineIDHash: machine}); err != nil || answer.Refusal != engine.RebootOfControllerMachineRefused {
			t.Fatal(answer, err)
		}
	}
}
