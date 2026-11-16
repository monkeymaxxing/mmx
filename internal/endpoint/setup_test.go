package endpoint

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"testing"

	"golang.org/x/crypto/ssh"
)

// Setup restarts the endpoint from what is running, not from what an earlier
// setup wrote. One interrupted after writing the unit left the old endpoint
// running with an unchanged unit on disk.
func TestEndpointRestartFollowsRunningProcess(t *testing.T) {
	binary := "/opt/mmx/bin/new/mmx-runner"
	command := []string{binary, "serve", "22"}
	current := endpointState{Active: true, Executable: binary, Command: command}
	for _, tt := range []struct {
		name    string
		change  func(*endpointState)
		restart bool
	}{
		{"current", func(*endpointState) {}, false},
		{"stopped", func(s *endpointState) { s.Active = false }, true},
		{"stale loaded unit", func(s *endpointState) { s.NeedReload = true }, true},
		{"older runner", func(s *endpointState) {
			s.Executable = "/opt/mmx/bin/old/mmx-runner"
			s.Command = []string{s.Executable, "serve", "22"}
		}, true},
		{"replaced binary", func(s *endpointState) { s.Executable = binary + " (deleted)" }, true},
		{"other SSH port", func(s *endpointState) { s.Command = []string{binary, "serve", "2222"} }, true},
		{"process gone", func(s *endpointState) { s.Executable, s.Command = "", []string{""} }, true},
	} {
		state := current
		tt.change(&state)
		if restartEndpoint(state, command) != tt.restart {
			t.Fatal(tt.name)
		}
	}
}

// MMX's line in authorized_keys carries a comment that names MMX, and a line
// that an earlier MMX wrote without the comment still counts as MMX's, so
// setup does not add a second line.
func TestManagedKeyLineHasCommentAndOldLineStillCounts(t *testing.T) {
	public, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ssh.NewPublicKey(public)
	if err != nil {
		t.Fatal(err)
	}
	line := authorizedKeyLine(key)
	parsed, comment, options, rest, err := ssh.ParseAuthorizedKey(line)
	if err != nil || comment != "mmx managed key" || len(options) != 0 || len(rest) != 0 || !bytes.Equal(parsed.Marshal(), key.Marshal()) || !bytes.HasSuffix(line, []byte("\n")) || bytes.Count(line, []byte("\n")) != 1 {
		t.Fatalf("line %q: %v %q %v", line, err, comment, options)
	}
	oldLine := ssh.MarshalAuthorizedKey(key) // no comment
	for name, data := range map[string][]byte{"with comment": line, "without comment": oldLine, "among others": append([]byte("ssh-rsa AAAA other\n"), oldLine...)} {
		if !holdsKey(data, key) {
			t.Fatal(name)
		}
	}
	if holdsKey([]byte("ssh-rsa AAAA other\n"), key) {
		t.Fatal("another key counted")
	}
}
