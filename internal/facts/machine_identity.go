package facts

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// machineIDPath holds systemd's ID of the machine's installation.
const machineIDPath = "/etc/machine-id"

// MachineIDHash is the SHA-256 of the machine's systemd machine ID, the form
// in which MMX names a machine. systemd asks that the ID itself is not shown
// to other machines, so the controller process sends only this hash of its own.
func MachineIDHash() (string, error) { return machineIDHash(os.ReadFile) }

func machineIDHash(read func(string) ([]byte, error)) (string, error) {
	b, err := read(machineIDPath)
	if err != nil {
		return "", err
	}
	id := strings.TrimSpace(string(b))
	if id == "" {
		return "", fmt.Errorf("%s is empty", machineIDPath)
	}
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:]), nil
}

// RunsInAContainer reports whether this machine is a container, as
// systemd-detect-virt --container answers: it prints the container's kind
// and exits with 0 in a container, and prints "none" and exits with 1
// everywhere else.
func RunsInAContainer() (bool, error) {
	return runsInAContainer(10 * time.Second)
}

// runsInAContainer is RunsInAContainer with the time the program may take.
func runsInAContainer(timeLimit time.Duration) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeLimit)
	defer cancel()
	out, err := exec.CommandContext(ctx, "systemd-detect-virt", "--container").Output()
	switch text := strings.TrimSpace(string(out)); {
	case text == "none":
		return false, nil
	case err == nil && text != "":
		return true, nil
	case err == nil:
		err = errors.New("the command printed nothing")
	}
	return false, fmt.Errorf("systemd-detect-virt --container: %w", err)
}
