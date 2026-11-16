package facts

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The hash is the SHA-256 of the machine ID as the file's one line holds it,
// the same value that the controller process computes from a node's answer
// over SSH. An empty or missing file gives no hash.
func TestMachineIDHashIsTheSHA256OfTheMachineID(t *testing.T) {
	const id = "0123456789abcdef0123456789abcdef"
	want := sha256.Sum256([]byte(id))
	got, err := machineIDHash(func(path string) ([]byte, error) {
		if path != "/etc/machine-id" {
			t.Fatalf("read %s", path)
		}
		return []byte(id + "\n"), nil
	})
	if err != nil || got != hex.EncodeToString(want[:]) {
		t.Fatal(got, err)
	}
	if got, err := machineIDHash(func(string) ([]byte, error) { return []byte("\n"), nil }); err == nil || got != "" || !strings.Contains(err.Error(), "/etc/machine-id is empty") {
		t.Fatal(got, err)
	}
	if got, err := machineIDHash(func(string) ([]byte, error) { return nil, os.ErrNotExist }); !errors.Is(err, os.ErrNotExist) || got != "" {
		t.Fatal(got, err)
	}
}

// systemd-detect-virt --container names the kind of container and exits
// with 0, or prints "none" and exits with 1. Anything else is an error, so
// that no caller takes a machine for a container by mistake.
func TestRunsInAContainerReadsSystemdDetectVirt(t *testing.T) {
	standIn := func(script string) {
		dir := t.TempDir()
		writeProgramFileThroughShellChild(t, filepath.Join(dir, "systemd-detect-virt"), "#!/bin/sh\n"+script)
		t.Setenv("PATH", dir)
	}
	for _, tc := range []struct {
		name, script string
		container    bool
	}{
		{"an Incus container", `test "$1" = --container && echo lxc`, true},
		{"a Docker container", `test "$1" = --container && echo docker`, true},
		{"a virtual machine or a physical server", `test "$1" = --container && echo none; exit 1`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			standIn(tc.script)
			if container, err := runsInAContainer(time.Minute); err != nil || container != tc.container {
				t.Fatal(container, err)
			}
		})
	}
	for _, tc := range []struct{ name, script, want string }{
		{"the program prints nothing", "exit 0", "the command printed nothing"},
		{"the program fails", "echo broken >&2; exit 3", "exit status 3"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			standIn(tc.script)
			if container, err := runsInAContainer(time.Minute); err == nil || container || !strings.Contains(err.Error(), "systemd-detect-virt --container: "+tc.want) {
				t.Fatal(container, err)
			}
		})
	}
	t.Run("the program is not installed", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())
		if container, err := runsInAContainer(time.Minute); err == nil || container || !strings.Contains(err.Error(), "systemd-detect-virt --container: ") {
			t.Fatal(container, err)
		}
	})
}
