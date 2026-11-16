package main

import (
	"os/exec"
	"strings"
	"testing"
)

// writeProgramFileThroughShellChild writes an executable program without this
// test process ever holding the file open for writing. A child that another
// goroutine forks during a Go write would inherit the writable descriptor until
// it execs its own program. Executing this file in that window can fail with
// ETXTBSY ("text file busy"). Only the shell child opens the program for writing.
func writeProgramFileThroughShellChild(t *testing.T, path, text string) {
	t.Helper()
	write := exec.Command("/bin/sh", "-c", `umask 077 && cat > "$1" && chmod 700 "$1"`, "sh", path)
	write.Stdin = strings.NewReader(text)
	if output, err := write.CombinedOutput(); err != nil {
		t.Fatalf("writing program %s through shell child: %v: %s", path, err, output)
	}
}
