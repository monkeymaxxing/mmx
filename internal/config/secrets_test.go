package config

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"mmx/internal/value"
)

func TestVaultSelectionAndSecretRoleInputs(t *testing.T) {
	path := fixture(t, "vault_file: development.age\nfleet_stages: [{id: app, nodes: all, node_tasks: [{role: app, inputs: {password: '{{ .secrets.database_password }}'}}]}]\n")
	base := filepath.Dir(path)
	write(t, filepath.Join(base, "roles/app/main.yml"), "inputs: {password: {type: string, required: true, secret: true}}\nnode_tasks: [{show: '{{ .inputs.password }}'}]\n")
	p, _, err := Load(path, "example", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if p.Vault != filepath.Join(base, "development.age") {
		t.Fatal(p.Vault)
	}
	if got := SecretNames(p.FleetStages); !reflect.DeepEqual(got, []string{"database_password"}) {
		t.Fatal(got)
	}
	write(t, filepath.Join(base, "inventories/example.yml"), "vault_file: production.age\nnodes: {web: {address: example.com}}\n")
	p, _, err = Load(path, "example", "", nil)
	if err != nil || p.Vault != filepath.Join(base, "inventories", "production.age") {
		t.Fatal(p.Vault, err)
	}
	for _, reference := range []string{"{{ .secrets }}", "{{ .secrets[.node.name] }}"} {
		path := fixture(t, "fleet_stages: [{id: app, nodes: all, node_tasks: [{show: '"+reference+"'}]}]\n")
		if _, _, err := Load(path, "example", "", nil); err == nil {
			t.Fatal("accepted nonspecific secret reference")
		}
	}
}

// A role call passes a secret to an input of its role without a mark in its
// trace, also when the secret is itself a role input that it passes on. The
// input stays a secret in the role's scope, so a node task that reads it
// marks its own trace and the trace of every role call around it. The
// checks of an input still hold.
func TestARoleCallPassesASecretInputOnWithoutAMark(t *testing.T) {
	inputs := map[string]Input{"password": {Type: "string", Required: true, Secret: true}, "port": {Type: "integer"}}
	call := func(password any) Task {
		return Task{Role: "app", Inputs: map[string]any{"password": password, "port": 8080}, RoleInputs: inputs}
	}
	outside, outerTrace := value.Scope{"secrets": map[string]any{"password": &value.Secret{Name: "password", Data: "role-sentinel", Available: true}}, "vars": map[string]any{"plain": "public"}}.Trace()
	outer, err := ResolveRoleInputs(call("{{ .secrets.password }}"), outside, false)
	if err != nil || outerTrace.Used {
		t.Fatal("a role call marked its trace by passing a secret on", err)
	}
	if held := outer["inputs"].(map[string]any); !value.HasSecrets(held["password"]) || value.Plain(held["password"]) != "role-sentinel" || held["port"] != 8080 {
		t.Fatalf("%#v", held)
	}
	// A role call inside the role passes the input on once more.
	innerCall, innerCallTrace := outer.Trace()
	inner, err := ResolveRoleInputs(call("{{ .inputs.password }}"), innerCall, false)
	if err != nil || innerCallTrace.Used || outerTrace.Used {
		t.Fatal("a role call inside a role marked a trace by passing a secret on", err)
	}
	// A node task of the inner role reads the input.
	task, taskTrace := inner.Trace()
	if read, err := task.Resolve("{{ .inputs.password }}"); err != nil || value.Plain(read) != "role-sentinel" || !taskTrace.Used || !innerCallTrace.Used || !outerTrace.Used {
		t.Fatal("a read of the input did not mark the node task and both role calls", read, err)
	}
	// A public input is no read of a secret either way.
	if _, err := task.Resolve("{{ .inputs.port }}"); err != nil {
		t.Fatal(err)
	}
	// An input that is declared secret takes only a secret, and another
	// input takes none. Neither error holds the value.
	for raw, want := range map[string]string{"{{ .vars.plain }}": "input password requires a named secret or secret-derived value"} {
		if _, err := ResolveRoleInputs(call(raw), outside, false); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatal(raw, err)
		}
	}
	leaking := call("{{ .secrets.password }}")
	leaking.Inputs["port"] = "{{ .secrets.password }}"
	if _, err := ResolveRoleInputs(leaking, outside, false); err == nil || !strings.Contains(err.Error(), "input port receives a secret; declare secret: true") || strings.Contains(err.Error(), "role-sentinel") {
		t.Fatal(err)
	}
}

func TestSecretReferenceInTemplateCall(t *testing.T) {
	source := `{{ define "nested" }}{{ printf "%s" . }}{{ end }}{{ template "nested" .secrets.token }}`
	if got := SecretNames(source); !reflect.DeepEqual(got, []string{"token"}) {
		t.Fatal(got)
	}
}
