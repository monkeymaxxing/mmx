package config

import (
	"mmx/internal/protocol"
	"mmx/internal/value"
	"strings"
	"testing"
)

// Runtime values keep their secret and truncation records across transport.
func TestJobTransportRestoresRuntimeRecords(t *testing.T) {
	j := Job{ID: "j", Scope: map[string]any{
		"outputs": map[string]any{"fleet_stage": map[string]any{"web": map[string]any{"token": &value.Secret{Name: "token", Data: "private", Available: true}, "text": value.Truncated{Text: "partial"}}}},
	}}
	b, err := j.Transport()
	if err != nil {
		t.Fatal(err)
	}
	var received Job
	if err := protocol.Decode(b, &received); err != nil {
		t.Fatal(err)
	}
	outputs := received.Scope["outputs"].(map[string]any)["fleet_stage"].(map[string]any)["web"].(map[string]any)
	if secret, ok := outputs["token"].(*value.Secret); !ok || secret.Data != "private" || outputs["text"] != (value.Truncated{Text: "partial"}) {
		t.Fatalf("runtime records were lost: %#v", outputs)
	}
}

// A node job whose vars or scope is one internal record instead of a mapping
// of names is refused with an error; decoding must not stop the node job
// worker process.
func TestJobDecodingRefusesVarsOrScopeThatIsOneRecord(t *testing.T) {
	for _, record := range []string{`{"$mmx_secret":{"name":"token"}}`, `{"$mmx_truncated":"partial"}`, `{"$mmx_binary":"aGk="}`} {
		for _, field := range []string{"vars", "scope"} {
			var received Job
			err := protocol.Decode([]byte(`{"protocol":1,"id":"j","project":"p","fleet_stage":"s","node":"n","dryrun":false,"node_tasks":[],"`+field+`":`+record+`}`), &received)
			if err == nil || !strings.Contains(err.Error(), "instead of a mapping of names") {
				t.Fatal(field, record, err)
			}
		}
	}
}
