package engine

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"mmx/internal/config"
	"mmx/internal/modules"
	"mmx/internal/protocol"
)

// nodeMachine is a node with these readings. A reading that the case must
// not need fails the test when the rule asks for it.
func nodeMachine(t *testing.T, bootID, machineIDHash, container string) NodeMachine {
	t.Helper()
	return NodeMachine{
		BootID: func() (string, error) { return bootID, nil },
		MachineIDHash: func() (string, error) {
			if machineIDHash == "not read" {
				t.Error("the rule read the node's machine ID")
			}
			return machineIDHash, nil
		},
		IsContainer: func() (bool, error) {
			if container == "not asked" {
				t.Error("the rule asked whether the node is a container")
			}
			return container == "yes", nil
		},
	}
}

// The cases of the design's list: a reboot is refused when the boot IDs
// match and either the machine IDs match too or the node is no container.
func TestRebootRefusalRule(t *testing.T) {
	controller := protocol.ControllerMachine{BootID: "boot-of-the-server", MachineIDHash: "hash-of-the-server"}
	for _, tc := range []struct {
		name       string
		controller protocol.ControllerMachine
		node       NodeMachine
		refused    bool
	}{
		{"the same machine", controller,
			nodeMachine(t, "boot-of-the-server", "hash-of-the-server", "not asked"), true},
		// Measured: a Docker container sees its server's boot ID, and the
		// debian:13 image has no /etc/machine-id.
		{"a controller process in a container without a machine ID, and the node is the server that runs the container",
			protocol.ControllerMachine{BootID: "boot-of-the-server"},
			nodeMachine(t, "boot-of-the-server", "not read", "no"), true},
		{"a controller process in a container with its own machine ID, and the node is the server that runs the container",
			protocol.ControllerMachine{BootID: "boot-of-the-server", MachineIDHash: "hash-of-the-container"},
			nodeMachine(t, "boot-of-the-server", "hash-of-the-server", "no"), true},
		{"a container on the controller machine that shares its boot ID", controller,
			nodeMachine(t, "boot-of-the-server", "hash-of-the-container", "yes"), false},
		{"two containers of one server, one of them runs the controller process",
			protocol.ControllerMachine{BootID: "boot-of-the-server", MachineIDHash: "hash-of-the-first-container"},
			nodeMachine(t, "boot-of-the-server", "hash-of-the-second-container", "yes"), false},
		{"virtual machines cloned from one image: one machine ID, each its own boot ID", controller,
			nodeMachine(t, "boot-of-the-clone", "not read", "not asked"), false},
		{"another machine", controller,
			nodeMachine(t, "boot-of-another-machine", "not read", "not asked"), false},
		// Measured: an Incus container has a boot ID of its own, so the
		// server that runs it looks like another machine.
		{"a controller process in an Incus container, and the node is the server that runs the container",
			protocol.ControllerMachine{BootID: "boot-of-the-incus-container", MachineIDHash: "hash-of-the-container"},
			nodeMachine(t, "boot-of-the-server", "not read", "not asked"), false},
		{"a controller machine without a boot ID, such as a Mac", protocol.ControllerMachine{},
			NodeMachine{BootID: func() (string, error) { t.Error("the rule read the node's boot ID"); return "", nil }}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			refusal, err := tc.node.RebootRefusal(tc.controller)
			want := ""
			if tc.refused {
				want = "MMX does not reboot the controller machine, the machine where this mmx command runs. Run the playbook from another machine, or reboot this machine by hand after MMX finishes."
			}
			if err != nil || refusal != want {
				t.Fatalf("refusal %q, error %v", refusal, err)
			}
		})
	}
}

// A node that cannot read what the rule needs does not answer that it may
// reboot: it fails, with the reading that failed.
func TestRebootRefusalRuleFailsWhenTheNodeCannotTell(t *testing.T) {
	controller := protocol.ControllerMachine{BootID: "boot-1", MachineIDHash: "hash-1"}
	failing := errors.New("the reading failed")
	for what, node := range map[string]NodeMachine{
		"reading this node's boot ID": {BootID: func() (string, error) { return "", failing }},
		"reading this node's machine ID": {BootID: func() (string, error) { return "boot-1", nil },
			MachineIDHash: func() (string, error) { return "", failing }},
		"finding out whether this node is a container": {BootID: func() (string, error) { return "boot-1", nil },
			MachineIDHash: func() (string, error) { return "hash-2", nil }, IsContainer: func() (bool, error) { return false, failing }},
	} {
		t.Run(what, func(t *testing.T) {
			refusal, err := node.RebootRefusal(controller)
			if refusal != "" || !errors.Is(err, failing) || err.Error() != what+": the reading failed" {
				t.Fatalf("refusal %q, error %v", refusal, err)
			}
		})
	}
}

// The reboot task applies the rule again after its guards, with the values
// that the node job carries. A node that is the controller machine fails the
// task with the refusal and does not reboot, in a dryrun as in a run; the
// node tasks before it have run. Another machine reboots as before.
func TestRebootTaskRefusesTheControllerMachine(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "patched")
	patch := config.Task{ID: "patch", Module: "shell", ModuleInputs: &modules.Shell{Script: "touch " + marker}}
	reboot := func(guard string) config.Task {
		return config.Task{ID: "restart", Module: "reboot", ModuleInputs: &modules.Reboot{OnlyIf: &modules.Guard{Script: guard}}}
	}
	const refused = "MMX does not reboot the controller machine, the machine where this mmx command runs. Run the playbook from another machine, or reboot this machine by hand after MMX finishes."
	run := func(node NodeMachine, dryrun bool, nodeTasks ...config.Task) protocol.Response {
		j := job(nodeTasks...)
		j.Dryrun = dryrun
		j.ControllerMachine = &protocol.ControllerMachine{BootID: "boot-1", MachineIDHash: "hash-1"}
		e := &Engine{Machine: node}
		return e.Run(context.Background(), j)
	}
	controllerMachine := nodeMachine(t, "boot-1", "hash-1", "not asked")

	r := run(controllerMachine, false, patch, reboot("true"))
	last := r.Results[len(r.Results)-1]
	if r.Status != "failed" || r.RebootFrom != "" || r.Error != "restart: "+refused || last.Status != "failed" || last.Reason != refused ||
		last.Change != "unchanged" || r.Results[0].Status != "succeeded" {
		t.Fatalf("%+v", r)
	}

	r = run(controllerMachine, true, reboot("true"))
	if r.Status != "failed" || r.RebootFrom != "" || r.Results[0].Reason != refused {
		t.Fatalf("dryrun: %+v", r)
	}

	// The guards come first: a guard that skips the reboot leaves nothing to refuse.
	r = run(controllerMachine, false, reboot("false"))
	if r.Status != "succeeded" || r.Results[0].Status != "skipped" {
		t.Fatalf("guard: %+v", r)
	}

	r = run(nodeMachine(t, "boot-2", "not read", "not asked"), false, patch, reboot("true"))
	if r.Status != "rebooting" || r.RebootFrom != "boot-2" {
		t.Fatalf("another machine: %+v", r)
	}
	r = run(nodeMachine(t, "boot-2", "not read", "not asked"), true, reboot("true"))
	if r.Status != "succeeded" || r.Results[0].Change != "would_change" {
		t.Fatalf("another machine, dryrun: %+v", r)
	}

	failing := NodeMachine{BootID: func() (string, error) { return "", errors.New("unreadable") }}
	r = run(failing, false, reboot("true"))
	if r.Status != "failed" || r.RebootFrom != "" ||
		r.Error != "restart: could not make sure that this node is not the machine that runs the controller process, which MMX never reboots: reading this node's boot ID: unreadable" {
		t.Fatalf("unreadable boot ID: %+v", r)
	}
}
