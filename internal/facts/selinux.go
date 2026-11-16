package facts

import (
	"fmt"
	"os"
	"strings"
)

// SELinuxRuns reports whether the kernel runs SELinux with a loaded policy,
// by the rule of libselinux's is_selinux_enabled: selinuxfs is mounted (its
// enforce file exists) and this process has a context other than "kernel".
// Some images mount selinuxfs without loading a policy; every process then
// has the context "kernel", nothing is labeled, and the SELinux programs may
// not be installed.
func SELinuxRuns(read func(string) ([]byte, error), enforceFile, contextFile string) (bool, error) {
	if _, err := read(enforceFile); os.IsNotExist(err) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	b, err := read(contextFile)
	if err != nil {
		return false, err
	}
	return strings.TrimRight(string(b), "\x00\n") != "kernel", nil
}

// selinuxMode reads the kernel's SELinux state: no selinuxfs, or no loaded
// policy, means disabled.
func selinuxMode(read func(string) ([]byte, error)) (string, error) {
	runs, err := SELinuxRuns(read, "/sys/fs/selinux/enforce", "/proc/self/attr/current")
	if err != nil {
		return "", err
	}
	if !runs {
		return "disabled", nil
	}
	b, err := read("/sys/fs/selinux/enforce")
	if err != nil {
		return "", err
	}
	switch text := strings.TrimSpace(string(b)); text {
	case "1":
		return "enforcing", nil
	case "0":
		return "permissive", nil
	default:
		return "", fmt.Errorf("unexpected SELinux state %q", text)
	}
}
