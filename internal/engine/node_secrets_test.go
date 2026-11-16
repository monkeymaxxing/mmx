package engine

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mmx/internal/config"
	"mmx/internal/modules"
	"mmx/internal/protocol"
	"mmx/internal/value"
)

func readFileTask(id, path string, secret bool) config.Task {
	return config.Task{ID: id, Module: "read_file", ModuleInputs: &modules.ReadFile{Path: path, ContentIsSecret: &secret}}
}

// A secret read with read_file, or printed by a shell task with
// output_is_secret, becomes a secret output without further marking. Results
// and saved responses never hold it, the private transport does, and each
// output records where it came from so a resume can read it again or name it.
func TestSecretsThatStartOnANodeBecomeSecretOutputs(t *testing.T) {
	dir := t.TempDir()
	token := filepath.Join(dir, "token")
	put(t, token, "join-token-sentinel")
	j := job(
		readFileTask("join_token", token, true),
		config.Task{ID: "created", Module: "shell", ModuleInputs: &modules.Shell{Script: "printf created-token-sentinel; printf warning-sentinel >&2", OutputIsSecret: true, ChangedWhen: false}},
		config.Task{ID: "copy", Module: "file", ModuleInputs: &modules.File{Path: filepath.Join(dir, "copy"), Content: ptr("{{ .node_tasks.join_token.content }}"), Mode: "0600"}},
	)
	j.Outputs = map[string]any{"join_token": "{{ .node_tasks.join_token.content }}", "created": "{{ .node_tasks.created.stdout }}", "derived": "prefix-{{ .node_tasks.created.stdout }}"}
	r := (&Engine{}).Run(context.Background(), j)
	requireStatus(t, r, "succeeded")
	if read(t, filepath.Join(dir, "copy")) != "join-token-sentinel" {
		t.Fatal("the secret did not reach a later node task")
	}
	b, _ := json.Marshal(r)
	for _, sentinel := range []string{"join-token-sentinel", "created-token-sentinel", "warning-sentinel"} {
		if strings.Contains(string(b), sentinel) {
			t.Fatal("secret in the saved response:", sentinel)
		}
	}
	if !r.Results[1].Secret || r.Results[1].Safe().Stdout != "[redacted]" || r.Results[1].Safe().Stderr != "[redacted]" {
		t.Fatal("output_is_secret did not mark the command's output", r.Results[1])
	}
	wire, _ := json.Marshal(r.Transport())
	var received protocol.Response
	if err := protocol.Decode(wire, &received); err != nil {
		t.Fatal(err)
	}
	if value.Plain(received.Outputs["join_token"]) != "join-token-sentinel" || value.Plain(received.Outputs["created"]) != "created-token-sentinel" {
		t.Fatal("private outputs were not transported", received.Outputs)
	}
	var retained protocol.Response
	if err := protocol.Decode(b, &retained); err != nil {
		t.Fatal(err)
	}
	origin := func(name string) value.NodeOrigin {
		s, ok := retained.Outputs[name].(*value.Secret)
		if !ok || s.Available || s.FromNode == nil {
			t.Fatalf("output %s is not a saved reference: %#v", name, retained.Outputs[name])
		}
		return *s.FromNode
	}
	if o := origin("join_token"); o != (value.NodeOrigin{FleetStage: "setup", Node: "test", NodeTask: "join_token", ReadFilePath: token}) {
		t.Fatal(o)
	}
	if o := origin("created"); o != (value.NodeOrigin{FleetStage: "setup", Node: "test", NodeTask: "created"}) {
		t.Fatal(o)
	}
	if o := origin("derived"); o != (value.NodeOrigin{FleetStage: "setup", Node: "test"}) {
		t.Fatal(o)
	}
	_, err := value.Scope{"outputs": retained.Outputs}.Get(".outputs.created")
	if err == nil || !strings.Contains(err.Error(), "node task created of fleet stage setup on node test") || !strings.Contains(err.Error(), "start a new mmx run") {
		t.Fatal(err)
	}
}

// A public read keeps its content in results; a file that is not UTF-8 passes
// on whole into a file's content and refuses to be part of other text.
func TestReadFileContentPassesOnWholeAndBinaryRefusesText(t *testing.T) {
	dir := t.TempDir()
	put(t, filepath.Join(dir, "version"), "1.2.3")
	binary := []byte{0x00, 0xff, 0x10, 0x80}
	if err := os.WriteFile(filepath.Join(dir, "key.der"), binary, 0600); err != nil {
		t.Fatal(err)
	}
	j := job(
		readFileTask("version", filepath.Join(dir, "version"), false),
		readFileTask("key", filepath.Join(dir, "key.der"), false),
		config.Task{Module: "file", ModuleInputs: &modules.File{Path: filepath.Join(dir, "copy.der"), Content: ptr("{{ .node_tasks.key.content }}"), Mode: "0600"}},
		config.Task{Module: "assert", ModuleInputs: &modules.Assert{That: map[string]any{"equals": []any{"{{ .node_tasks.version.content }}", "1.2.3"}}}},
	)
	j.Outputs = map[string]any{"key": "{{ .node_tasks.key.content }}"}
	r := (&Engine{}).Run(context.Background(), j)
	requireStatus(t, r, "succeeded")
	if read(t, filepath.Join(dir, "copy.der")) != string(binary) {
		t.Fatal("binary content changed on the way")
	}
	if r.Results[0].Content != "1.2.3" {
		t.Fatal(r.Results[0])
	}
	if b, ok := r.Outputs["key"].(value.Binary); !ok || string(b.Bytes) != string(binary) {
		t.Fatal(r.Outputs)
	}
	j = job(
		readFileTask("key", filepath.Join(dir, "key.der"), false),
		config.Task{Module: "file", ModuleInputs: &modules.File{Path: filepath.Join(dir, "embedded"), Content: ptr("KEY={{ .node_tasks.key.content }}"), Mode: "0600"}},
	)
	r = (&Engine{}).Run(context.Background(), j)
	requireStatus(t, r, "failed")
	if !strings.Contains(r.Error, "binary content") || !strings.Contains(r.Error, "reference with no surrounding text") {
		t.Fatal(r.Error)
	}
	// A missing file is not checked in dryrun, so the output reading it is not either.
	j = job(readFileTask("token", filepath.Join(dir, "missing"), true))
	j.Outputs = map[string]any{"token": "{{ .node_tasks.token.content }}"}
	j.Dryrun = true
	r = (&Engine{}).Run(context.Background(), j)
	if r.Status != "not_checked" || r.Results[0].Status != "not_checked" {
		t.Fatal(r.Status, r.Error, r.Results)
	}
}

func ptr(s string) *string { return &s }
