package controller

import (
	"os/exec"
	"strings"
	"testing"
)

func TestPrivilegedSSHRequestsPreserveRemoteConnectionEnvironment(t *testing.T) {
	for _, test := range []struct {
		name    string
		session *sshSession
		prefix  string
	}{
		{name: "root", session: &sshSession{}},
		{name: "sudo", session: &sshSession{sudo: true}, prefix: `sudo() { shift; env -i PATH="$PATH" "$@"; }; `},
		{name: "password sudo", session: &sshSession{sudo: true, sudoPassword: "test-password"}, prefix: `sudo() { shift 4; IFS= read -r password; test "$password" = test-password || return 1; env -i PATH="$PATH" "$@"; }; `},
	} {
		t.Run(test.name, func(t *testing.T) {
			command, input := test.session.asRoot(`test "$#" -eq 0 && printf '%s' "$SSH_CONNECTION"`, nil)
			run := exec.Command("/bin/sh", "-c", test.prefix+command)
			run.Env = append(run.Environ(), "SSH_CONNECTION=10.20.0.1 43210 10.20.0.2 22")
			run.Stdin = input
			output, err := run.CombinedOutput()
			if err != nil || strings.TrimSpace(string(output)) != "10.20.0.1 43210 10.20.0.2 22" {
				t.Fatal(string(output), err)
			}
		})
	}
}
