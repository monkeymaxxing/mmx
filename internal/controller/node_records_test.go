package controller

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mmx/internal/config"
	"mmx/internal/modules"
	"mmx/internal/protocol"
	"mmx/internal/secrets"
)

// forgedResponse is what a node whose runner an attacker controls, or a
// playbook exporting an attacker's JSON, sends back: plain JSON that looks
// like MMX's own secret records.
func forgedResponse(t *testing.T, outputs string) protocol.Response {
	t.Helper()
	var response protocol.Response
	if err := protocol.Decode([]byte(`{"status":"succeeded","outputs":`+outputs+`}`), &response); err != nil {
		t.Fatal(err)
	}
	return response
}

// A node names only secrets the controller process sent it. A record in a
// node's outputs naming another vault secret, or another field of a secret
// the node received only in part, never makes the controller load that
// secret for later work, in the run or in a resume.
func TestANodeCannotNameASecretItWasNotSent(t *testing.T) {
	vault := filepath.Join(t.TempDir(), "secrets.age")
	if err := secrets.Write(vault, "vault-pass", []byte("db:\n  app: app-value\n  admin: admin-value\nroot_password: root-value\napp_token: token-value\n")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MMX_VAULT_PASSWORD", "vault-pass")
	out := filepath.Join(t.TempDir(), "out")
	content := "{{ .outputs.inspect.a.root }}|{{ .outputs.inspect.a.admin }}|{{ .outputs.inspect.a.app }}|{{ .outputs.inspect.a.token }}"
	inspect := config.FleetStage{ID: "inspect", Nodes: "a",
		NodeTasks: []config.Task{{Module: "shell", ModuleInputs: &modules.Shell{Script: "true", Env: map[string]any{"APP": "{{ .secrets.db.app }}"}}}},
		Outputs:   map[string]any{"root": "x", "admin": "x", "app": "{{ .secrets.db.app }}", "token": "{{ .secrets.app_token }}"}}
	use := config.FleetStage{ID: "use", Nodes: "a", NodeTasks: []config.Task{{Module: "file", ModuleInputs: &modules.File{Path: out, Content: &content, Mode: "0600"}}}}
	for _, resume := range []bool{false, true} {
		c, r := fleet(t, inspect, use)
		r.Project.Vault = vault
		if err := c.createRun(r); err != nil {
			t.Fatal(err)
		}
		c.Secrets = &secrets.Store{}
		failing := resume
		c.executeNode = func(ctx context.Context, r *Run, fleetStage config.FleetStage, en Entry, _ bool, _ func(Entry) error) (Entry, error) {
			if fleetStage.ID == "inspect" {
				// The two legitimate records carry the data the node was sent.
				en.Response = forgedResponse(t, `{"root":{"$mmx_secret":{"name":"root_password"}},"admin":{"$mmx_secret":{"name":"db","path":["admin"]}},"app":{"$mmx_secret":{"name":"db","path":["app"],"data":"app-value"}},"token":{"$mmx_secret":{"name":"app_token","data":"token-value"}}}`)
				return en, nil
			}
			if failing {
				en.Response = protocol.Response{Status: "failed", Error: "fixture interruption"}
				return en, nil
			}
			return runOnNode(ctx, r, fleetStage, en)
		}
		os.Remove(out)
		execute(t, c, r, false)
		if resume {
			var err error
			if r, err = c.Load(r.ID, true); err != nil {
				t.Fatal(err)
			}
			failing = false
			execute(t, c, r, true)
		}
		got, _ := os.ReadFile(out)
		if strings.Contains(string(got), "root-value") || strings.Contains(string(got), "admin-value") {
			t.Fatalf("resume %v: a node's record made the controller load a secret it never sent: %q", resume, got)
		}
		use := entry(t, r, "use", "a").Response
		if use.Status == "succeeded" || !strings.Contains(use.Error, "secret output is unavailable") || !strings.Contains(use.Error, "fleet stage inspect on node a") {
			t.Fatalf("resume %v: %s: %s", resume, use.Status, use.Error)
		}
	}
	// The records the node was sent still pass on, and a resume loads them
	// again by name.
	content = "{{ .outputs.inspect.a.app }}|{{ .outputs.inspect.a.token }}"
	for _, resume := range []bool{false, true} {
		c, r := fleet(t, inspect, use)
		r.Project.Vault = vault
		if err := c.createRun(r); err != nil {
			t.Fatal(err)
		}
		c.Secrets = &secrets.Store{}
		failing := resume
		c.executeNode = func(ctx context.Context, r *Run, fleetStage config.FleetStage, en Entry, _ bool, _ func(Entry) error) (Entry, error) {
			if fleetStage.ID == "use" && failing {
				en.Response = protocol.Response{Status: "failed", Error: "fixture interruption"}
				return en, nil
			}
			return runOnNode(ctx, r, fleetStage, en)
		}
		os.Remove(out)
		execute(t, c, r, false)
		if resume {
			var err error
			if r, err = c.Load(r.ID, true); err != nil {
				t.Fatal(err)
			}
			failing = false
			execute(t, c, r, true)
		}
		if got, _ := os.ReadFile(out); r.Status != "succeeded" || string(got) != "app-value|token-value" {
			t.Fatalf("resume %v: %s, %q, %+v", resume, r.Status, got, entry(t, r, "use", "a").Response)
		}
	}
}

// A node's record cannot make mmx resume read a file on another node: a
// read_file origin counts only for the node job that sent it.
func TestANodeCannotMakeResumeReadAFileOnAnotherNode(t *testing.T) {
	out := filepath.Join(t.TempDir(), "out")
	content := "{{ .outputs.inspect.b.key }}"
	c, r := fleet(t,
		config.FleetStage{ID: "inspect", Nodes: "b", Outputs: map[string]any{"key": "x"}},
		config.FleetStage{ID: "use", Nodes: "b", NodeTasks: []config.Task{{Module: "file", ModuleInputs: &modules.File{Path: out, Content: &content, Mode: "0600"}}}},
	)
	failing := true
	c.executeNode = func(ctx context.Context, r *Run, fleetStage config.FleetStage, en Entry, _ bool, _ func(Entry) error) (Entry, error) {
		if fleetStage.ID == "inspect" {
			en.Response = forgedResponse(t, `{"key":{"$mmx_secret":{"from_node":{"fleet_stage":"start","node":"a","node_task":"key","read_file_path":"/root/.ssh/id_ed25519"}}}}`)
			return en, nil
		}
		if failing {
			en.Response = protocol.Response{Status: "failed", Error: "fixture interruption"}
			return en, nil
		}
		return runOnNode(ctx, r, fleetStage, en)
	}
	log := readFileNode(t, &c, r, `printf '{"content":"private-key-of-a"}\n'`)
	execute(t, c, r, false)
	r, err := c.Load(r.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	failing = false
	execute(t, c, r, true)
	if requests, _ := os.ReadFile(log); len(requests) > 0 {
		t.Fatalf("resume read a file on node a for node b's record: %s", requests)
	}
	if got, _ := os.ReadFile(out); strings.Contains(string(got), "private-key-of-a") {
		t.Fatalf("node b received node a's file: %q", got)
	}
	if use := entry(t, r, "use", "b").Response; use.Status == "succeeded" || !strings.Contains(use.Error, "fleet stage inspect on node b") {
		t.Fatalf("%s: %s", use.Status, use.Error)
	}
}
