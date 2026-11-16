package value

import (
	"encoding/json"
	"strings"
	"testing"
)

// Binary content survives saved and transported JSON whole, passes on whole,
// and refuses to be part of text or a template.
func TestBinaryPassesOnWholeOnly(t *testing.T) {
	content := Binary{[]byte{0x00, 0xff, 'a'}}
	s := Scope{"outputs": map[string]any{"key": content, "secret": &Secret{Data: content, Available: true}}}
	for _, path := range []string{"{{ .outputs.key }}", "{{ .outputs.secret }}"} {
		v, err := s.Resolve(path)
		if err != nil {
			t.Fatal(err)
		}
		if text, err := Scalar(v); err != nil || text != "\x00\xffa" {
			t.Fatal(path, text, err)
		}
	}
	if _, err := s.Resolve("KEY={{ .outputs.key }}"); err == nil || !strings.Contains(err.Error(), "reference with no surrounding text") {
		t.Fatal(err)
	}
	if _, err := s.Template("{{ .outputs.key }}"); err == nil || !strings.Contains(err.Error(), "binary content") {
		t.Fatal(err)
	}
	b, _ := json.Marshal(Transport(map[string]any{"key": content, "secret": s["outputs"].(map[string]any)["secret"]}))
	var decoded any
	json.Unmarshal(b, &decoded)
	restored := Restore(decoded).(map[string]any)
	if got, ok := restored["key"].(Binary); !ok || string(got.Bytes) != "\x00\xffa" {
		t.Fatal(restored)
	}
	if got, ok := Plain(restored["secret"]).(Binary); !ok || string(got.Bytes) != "\x00\xffa" {
		t.Fatal(restored)
	}
	if !HasReservedKey(decoded) || Display(content) != "[binary content, 3 bytes]" {
		t.Fatal("a module result could forge binary content")
	}
}

// A saved secret that started on a node keeps where it came from, and only
// one read_file read can be read again.
func TestNodeSecretsKeepTheirOriginWhenSaved(t *testing.T) {
	read := &Secret{Data: "read-sentinel", Available: true, FromNode: &NodeOrigin{ReadFilePath: "/var/lib/token"}}
	marked := FromNode(map[string]any{"read": read, "printed": Sensitive("printed-sentinel"), "named": &Secret{Name: "vault", Data: "x", Available: true}}, NodeOrigin{FleetStage: "start", Node: "server01", NodeTask: "join_token"}).(map[string]any)
	b, _ := json.Marshal(marked)
	if strings.Contains(string(b), "sentinel") {
		t.Fatal(string(b))
	}
	var decoded any
	json.Unmarshal(b, &decoded)
	saved := Restore(decoded).(map[string]any)
	if o := saved["read"].(*Secret).FromNode; *o != (NodeOrigin{FleetStage: "start", Node: "server01", NodeTask: "join_token", ReadFilePath: "/var/lib/token"}) {
		t.Fatal(o)
	}
	if saved["named"].(*Secret).FromNode != nil {
		t.Fatal("a named secret is loaded by name, not from a node")
	}
	reads := 0
	again := ReadAgain(saved, func(o NodeOrigin) (any, error) { reads++; return "token-again", nil }).(map[string]any)
	scope := Scope{"outputs": again}
	if v, err := scope.Get(".outputs.read"); err != nil || Plain(v) != "token-again" || reads != 1 {
		t.Fatal(v, err, reads)
	}
	if _, err := scope.Get(".outputs.printed"); err == nil || !strings.Contains(err.Error(), "node task join_token of fleet stage start on node server01") {
		t.Fatal(err)
	}
}
