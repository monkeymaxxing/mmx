package main

import (
	"encoding/json"
	"os"
	"testing"

	"mmx/internal/protocol"
	"mmx/internal/release"
)

// mmx-runner version reports the version of the MMX release it belongs to,
// the one that mmx version and the official extra modules carry, and the
// protocol it speaks.
func TestVersionIsTheReleaseVersion(t *testing.T) {
	stdout, err := os.CreateTemp(t.TempDir(), "stdout")
	if err != nil {
		t.Fatal(err)
	}
	savedStdout, savedArgs := os.Stdout, os.Args
	os.Stdout, os.Args = stdout, []string{"mmx-runner", "version"}
	err = run()
	os.Stdout, os.Args = savedStdout, savedArgs
	stdout.Close()
	output, _ := os.ReadFile(stdout.Name())
	var reported struct {
		Protocol int    `json:"protocol"`
		Version  string `json:"version"`
	}
	if err != nil || json.Unmarshal(output, &reported) != nil || reported.Version != release.Version || reported.Protocol != protocol.Version {
		t.Fatalf("mmx-runner version printed %s (%v); want version %s and protocol %d", output, err, release.Version, protocol.Version)
	}
}
