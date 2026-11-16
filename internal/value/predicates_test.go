package value

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestOutputPredicates(t *testing.T) {
	s := Scope{"vars": map[string]any{"text": "ready\nERROR: failed\n", "private": Sensitive("private-sentinel")}}
	for _, tc := range []struct {
		op   string
		a, b any
		want bool
	}{{"contains", "{{ .vars.text }}", "ERROR:", true}, {"contains", "{{ .vars.text }}", "error:", false}, {"matches", "{{ .vars.text }}", "(?m)^ERROR:", true}, {"matches", "{{ .vars.text }}", "^ERROR:", false}, {"matches", "{{ .vars.text }}", "failed", true}} {
		got, err := s.Predicate(map[string]any{tc.op: []any{tc.a, tc.b}})
		if err != nil || got != tc.want {
			t.Fatal(tc, got, err)
		}
	}
	for _, arg := range []any{[]any{"text", "["}, []any{7, "x"}, []any{"text", true}, []any{"one"}} {
		if _, err := s.Predicate(map[string]any{"matches": arg}); err == nil {
			t.Fatal("accepted", arg)
		}
	}
	output, err := s.Evaluate(map[string]any{"contains": []any{"{{ .vars.private }}", "sentinel"}})
	if err != nil || !HasSecrets(output) || Plain(output) != true {
		t.Fatal(output, err)
	}
}

func TestTruncationSurvivesReferencesExportsAndShortCircuit(t *testing.T) {
	s := Scope{"this_node_task": map[string]any{"stdout": Truncated{"partial"}, "stderr": "complete", "exit_code": 0}, "vars": map[string]any{}}
	aliased, err := s.Resolve("prefix:{{ .this_node_task.stdout }}")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(aliased)
	var saved any
	json.Unmarshal(b, &saved)
	s["vars"].(map[string]any)["alias"] = Restore(saved)
	for _, p := range []any{map[string]any{"contains": []any{"{{ .this_node_task.stdout }}", "partial"}}, map[string]any{"matches": []any{"{{ .vars.alias }}", "prefix"}}, map[string]any{"equals": []any{"{{ .this_node_task }}", nil}}} {
		if _, err := s.Predicate(p); err == nil || !strings.Contains(err.Error(), "truncated") {
			t.Fatal(err)
		}
	}
	for _, p := range []any{map[string]any{"any": []any{true, map[string]any{"contains": []any{"{{ .this_node_task.stdout }}", "x"}}}}, map[string]any{"equals": []any{"{{ .this_node_task.exit_code }}", 0}}} {
		ok, err := s.Predicate(p)
		if err != nil || !ok {
			t.Fatal(ok, err)
		}
	}
	if Display(s["vars"].(map[string]any)["alias"]) != "prefix:partial" {
		t.Fatal("partial text unavailable for display")
	}
}
