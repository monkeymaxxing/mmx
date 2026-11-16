package engine

import (
	"fmt"

	"mmx/internal/facts"
	"mmx/internal/protocol"
)

// RebootOfControllerMachineRefused is the failure of a node job that would
// reboot the machine that runs the controller process.
const RebootOfControllerMachineRefused = "MMX does not reboot the controller machine, the machine where this mmx command runs. Run the playbook from another machine, or reboot this machine by hand after MMX finishes."

// NodeMachine reads what a reboot needs to know about the node this code
// runs on: the kernel's ID of its current boot, the hash of its systemd
// machine ID, and whether it is a container. A reading that is nil reads the
// node itself; tests replace readings.
type NodeMachine struct {
	BootID        func() (string, error)
	MachineIDHash func() (string, error)
	IsContainer   func() (bool, error)
}

// CurrentBootID is the boot the node runs on now: a reboot task records the
// boot it leaves, and the node later compares it with the boot it is on.
func (m NodeMachine) CurrentBootID() (string, error) {
	if m.BootID != nil {
		return m.BootID()
	}
	return facts.BootID()
}

func (m NodeMachine) machineIDHash() (string, error) {
	if m.MachineIDHash != nil {
		return m.MachineIDHash()
	}
	return facts.MachineIDHash()
}

func (m NodeMachine) isContainer() (bool, error) {
	if m.IsContainer != nil {
		return m.IsContainer()
	}
	return facts.RunsInAContainer()
}

// RebootRefusal is the one rule that keeps MMX from rebooting the machine
// that runs the controller process, whose reboot would end that process. It
// returns RebootOfControllerMachineRefused, or "" when this node may reboot.
// The controller process asks through it before the node job of a fleet
// stage that ends with a reboot task, and the reboot task asks again.
//
// One kernel has one boot ID, so a node with the controller machine's boot ID
// runs on the controller machine's kernel. It is that machine itself when the
// machine IDs match too. With another machine ID, one of the two is a
// container on the other: a node that is no container is then the server
// under the controller process, and a node that is a container can restart
// while the controller process goes on. A controller machine without a boot
// ID, such as a Mac, is never the node.
//
// An error means that the node could not read what the rule needs. The
// caller then fails with RebootNotChecked, so the node does not reboot.
func (m NodeMachine) RebootRefusal(controller protocol.ControllerMachine) (string, error) {
	if controller.BootID == "" {
		return "", nil
	}
	boot, err := m.CurrentBootID()
	if err != nil {
		return "", fmt.Errorf("reading this node's boot ID: %w", err)
	}
	if boot != controller.BootID {
		return "", nil
	}
	if controller.MachineIDHash != "" {
		machine, err := m.machineIDHash()
		if err != nil {
			return "", fmt.Errorf("reading this node's machine ID: %w", err)
		}
		if machine == controller.MachineIDHash {
			return RebootOfControllerMachineRefused, nil
		}
	}
	container, err := m.isContainer()
	if err != nil {
		return "", fmt.Errorf("finding out whether this node is a container: %w", err)
	}
	if !container {
		return RebootOfControllerMachineRefused, nil
	}
	return "", nil
}

// RebootNotChecked is the failure of a node job whose node could not be
// asked the rule, or could not answer it. Without the answer the node could
// be the controller machine, so the node job fails instead.
func RebootNotChecked(err error) error {
	return fmt.Errorf("could not make sure that this node is not the machine that runs the controller process, which MMX never reboots: %w", err)
}
