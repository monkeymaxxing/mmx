package controller

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"mmx/internal/access"
	"mmx/internal/config"
)

func TestRunnerUploadShellRefusesEveryOwedRestoreServiceMarker(t *testing.T) {
	capture := filepath.Join(t.TempDir(), "installation_command")
	fakeSSH(t, `for command; do :; done
printf '%s' "$command" > `+quote(capture)+`
exit 1
`)
	session := &sshSession{Node: config.Node{Address: "fixture"}, Credentials: &access.Credentials{}}
	if err := session.installRunner(context.Background(), &artifact{binary: []byte("runner"), hash: strings.Repeat("a", 64)}); err == nil {
		t.Fatal("fake SSH should refuse")
	}
	content, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	script := string(content)
	start, end := strings.Index(script, "for unit in /etc/systemd/system/"), strings.Index(script, "exec 8>/var/lib/mmx/execution.lock")
	busy := strings.Index(script, "node busy with job")
	if start < 0 || start <= busy || end <= start {
		t.Fatal("restore guard must follow busy check and precede execution lock", script)
	}
	directory := t.TempDir()
	// Run the real guard from the transmitted script, substituting only its
	// filesystem root. No installation, SSH connection, or systemd call runs.
	guard := strings.ReplaceAll(script[start:end], "/etc/systemd/system", directory)
	for _, kind := range []string{"absent", "file", "dangling_symlink"} {
		t.Run(kind, func(t *testing.T) {
			marker := filepath.Join(directory, "mmx-owed-restore-0123456789abcdef.service")
			switch kind {
			case "file":
				err = os.WriteFile(marker, []byte("partial unit"), 0644)
			case "dangling_symlink":
				err = os.Symlink("missing", marker)
			}
			if err != nil {
				t.Fatal(err)
			}
			out, runErr := exec.Command("/bin/sh", "-c", guard).CombinedOutput()
			if kind == "absent" {
				if runErr != nil {
					t.Fatal(string(out), runErr)
				}
			} else {
				if runErr == nil || !strings.Contains(string(out), filepath.Base(marker)) {
					t.Fatal("did not refuse by unit name", string(out), runErr)
				}
				if err := os.Remove(marker); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}
