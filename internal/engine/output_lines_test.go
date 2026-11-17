package engine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mmx/internal/config"
	"mmx/internal/modules"
	"mmx/internal/value"
)

// Another node's output cannot add lines to a file on this node: a WireGuard "PostUp =" or an ifupdown "up" line would run as root.
// The value as the whole input, such as a certificate as a file's whole
// content, and a command's last newline still pass.
func TestAnotherNodesOutputAddsNoLines(t *testing.T) {
	dir := t.TempDir()
	forged := "kq0Gl0mU5lUgk2t3W0sPr8y4cJ3w2xY1zZ8gE0fq9Ww=\nPostUp = touch " + filepath.Join(dir, "ran-as-root")
	certificate := "-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n"
	outputs := map[string]any{
		"keys":    map[string]any{"a": map[string]any{"public_key": forged, "certificate": certificate, "address": "10.0.0.5\n"}},
		"private": map[string]any{"a": map[string]any{"secret_key": value.Sensitive(forged)}},
	}
	run := func(nodeTasks ...config.Task) (string, string) {
		j := job(nodeTasks...)
		j.Scope = map[string]any{"outputs": outputs}
		r := (&Engine{}).Run(context.Background(), j)
		return r.Status, r.Error
	}
	wireguard := filepath.Join(dir, "wg0.conf")
	template := "[Peer]\n{{ range $name, $peer := .outputs.keys }}PublicKey = {{ $peer.public_key }}\n{{ end }}"
	field := "[Peer]\nPublicKey = {{ .outputs.keys.a.public_key }}\n"
	refused := map[string]config.Task{
		"an inline reference": {Module: "file", ModuleInputs: &modules.File{Path: wireguard, Content: &field, Mode: "0600"}},
		"a template file":     {Module: "file", ModuleInputs: &modules.File{Path: wireguard, Template: "wg0.conf", TemplateText: template, Mode: "0600"}},
		"a secret":            {Module: "file", ModuleInputs: &modules.File{Path: wireguard, Content: ptr("PrivateKey = {{ .outputs.private.a.secret_key }}\n"), Mode: "0600"}},
		"a role input": {Role: "peer", Inputs: map[string]any{"key": "{{ .outputs.keys.a.public_key }}"}, RoleInputs: map[string]config.Input{"key": {Type: "string", Required: true}},
			RoleNodeTasks: []config.Task{{Module: "file", ModuleInputs: &modules.File{Path: wireguard, Content: ptr("PublicKey = {{ .inputs.key }}\n"), Mode: "0600"}}}},
		"a loop item": {Loop: "{{ .outputs.keys }}", As: "peer", Module: "file", ModuleInputs: &modules.File{Path: wireguard, Content: ptr("PublicKey = {{ .peer.value.public_key }}\n"), Mode: "0600"}},
	}
	for name, task := range refused {
		os.Remove(wireguard)
		status, reason := run(task)
		// A secret's task hides its reason.
		if status != "failed" || name != "a secret" && !strings.Contains(reason, "line break or another control character") || strings.Contains(reason, "PostUp") {
			t.Errorf("%s: %s: %s", name, status, reason)
		}
		if b, err := os.ReadFile(wireguard); err == nil {
			t.Errorf("%s: wrote %q", name, b)
		}
	}
	// A variable that holds the output whole is guarded too.
	set := config.Task{SetVars: map[string]any{"peer_key": "{{ .outputs.keys.a.public_key }}"}}
	if status, reason := run(set, config.Task{Module: "file", ModuleInputs: &modules.File{Path: wireguard, Content: ptr("PublicKey = {{ .vars.peer_key }}\n"), Mode: "0600"}}); status != "failed" || !strings.Contains(reason, "line break") {
		t.Errorf("set_vars: %s: %s", status, reason)
	}
	// The whole value as the whole content, and a value whose only line
	// break is its last character, still pass.
	certificatePath, address := filepath.Join(dir, "ca.pem"), filepath.Join(dir, "address")
	status, reason := run(
		config.Task{Module: "file", ModuleInputs: &modules.File{Path: certificatePath, Content: ptr("{{ .outputs.keys.a.certificate }}"), Mode: "0644"}},
		config.Task{Module: "file", ModuleInputs: &modules.File{Path: address, Content: ptr("address {{ .outputs.keys.a.address }}"), Mode: "0644"}},
	)
	if status != "succeeded" || read(t, certificatePath) != certificate || read(t, address) != "address 10.0.0.5\n" {
		t.Fatal(status, reason)
	}
}
