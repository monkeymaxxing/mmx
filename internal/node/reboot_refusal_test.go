package node

import (
	"errors"
	"testing"

	"mmx/internal/engine"
	"mmx/internal/protocol"
)

// The node answers the controller process's question with the rule's
// refusal for the machine that runs the controller process, with nothing for
// another machine, and with an error when it cannot tell.
func TestRebootRefusalAnswersForThisNode(t *testing.T) {
	s := Server{Root: t.TempDir(), machine: engine.NodeMachine{
		BootID:        func() (string, error) { return "boot-1", nil },
		MachineIDHash: func() (string, error) { return "hash-1", nil },
		IsContainer:   func() (bool, error) { return false, nil },
	}}
	ask := func(bootID, machineIDHash string) (protocol.RebootRefusalResponse, error) {
		return s.RebootRefusal(protocol.RebootRefusalRequest{ControllerMachine: protocol.ControllerMachine{BootID: bootID, MachineIDHash: machineIDHash}})
	}
	if answer, err := ask("boot-1", "hash-1"); err != nil || answer.Refusal != engine.RebootOfControllerMachineRefused {
		t.Fatal(answer, err)
	}
	if answer, err := ask("boot-2", "hash-1"); err != nil || answer.Refusal != "" {
		t.Fatal(answer, err)
	}
	if answer, err := ask("", ""); err != nil || answer.Refusal != "" {
		t.Fatal(answer, err)
	}
	s.machine.BootID = func() (string, error) { return "", errors.New("unreadable") }
	if answer, err := ask("boot-1", "hash-1"); err == nil || answer.Refusal != "" || err.Error() != "reading this node's boot ID: unreadable" {
		t.Fatal(answer, err)
	}
}
