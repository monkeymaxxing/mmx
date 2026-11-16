package modules

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// policyRCD is Debian's policy program, which a package's scripts ask before
// each service action.
const policyRCD = "/usr/sbin/policy-rc.d"

// servicePolicyMark is the line that marks the policy program as MMX's.
const servicePolicyMark = "# Written by MMX for one apt_package command; MMX removes it afterwards."

// servicePolicy lets a package's scripts restart a service that is running and
// stop one, but not start a service that is not running. Its arguments are
// [options] <service> <actions> [<runlevel>]; exit 101 refuses the action.
const servicePolicy = "#!/bin/sh\n" + servicePolicyMark + `
# A package may restart a running service; it may not start a stopped one.
while [ "${1#--}" != "$1" ]; do shift; done
if systemctl is-active --quiet "$1" 2>/dev/null; then exit 0; fi
for action in $2; do
	case "$action" in
	start|restart|reload|force-reload|try-restart|force-start) exit 101 ;;
	esac
done
exit 0
`

// placeServicePolicy puts MMX's policy program in place before an apt
// command. A policy program that MMX did not write stays and decides; the
// returned note says so.
func placeServicePolicy(ctx context.Context, run Runtime, root string) (string, error) {
	program := filepath.Join(root, policyRCD)
	switch mine, err := servicePolicyIsMine(program); {
	case err != nil:
		return "", err
	case !mine:
		return fmt.Sprintf("The node's own %s decided which services the package scripts could start.", policyRCD), nil
	}
	// The program is MMX's own bookkeeping, not part of the task's change, so
	// it keeps no difference in the task's result.
	run.RetainDiff = nil
	if written := run.WriteFile(ctx, program, servicePolicy, "", "", "0755"); written.Error != "" {
		return "", errors.New(written.Error)
	}
	return "", nil
}

// removeServicePolicy removes MMX's policy program, also one an interrupted
// run left; a program MMX did not write stays.
func removeServicePolicy(ctx context.Context, run Runtime, root string) error {
	program := filepath.Join(root, policyRCD)
	mine, err := servicePolicyIsMine(program)
	if err != nil || !mine {
		return err
	}
	if removed := run.RemovePath(ctx, program); removed.Error != "" {
		return errors.New(removed.Error)
	}
	return nil
}

// RemoveLeftoverServicePolicy runs at the start of a node job that may change
// the node, so a policy program left by an interrupted run does not keep
// stopping the node's own package installations from starting services.
func RemoveLeftoverServicePolicy(ctx context.Context, run Runtime) error {
	return removeServicePolicy(ctx, run, "/")
}

// servicePolicyIsMine reports whether the policy program is missing or MMX's.
func servicePolicyIsMine(program string) (bool, error) {
	f, err := os.Open(program)
	if os.IsNotExist(err) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	defer f.Close()
	lines := bufio.NewScanner(f)
	for i := 0; i < 2 && lines.Scan(); i++ {
		if strings.TrimSpace(lines.Text()) == servicePolicyMark {
			return true, nil
		}
	}
	return false, lines.Err()
}
