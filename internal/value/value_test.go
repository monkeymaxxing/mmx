package value

import (
	"encoding/json"
	"testing"
)

func TestTypedReferencesAndSinglePass(t *testing.T) {
	s := Scope{"vars": map[string]any{"count": 3, "nested": "{{ .vars.missing }}", "items": []any{"a", "b"}}}
	v, err := s.Resolve("{{ .vars.count }}")
	if err != nil || v != 3 {
		t.Fatalf("lost type: %#v %v", v, err)
	}
	v, err = s.Resolve("items={{ .vars.count }}")
	if err != nil || v != "items=3" {
		t.Fatalf("interpolation: %v %v", v, err)
	}
	v, err = s.Resolve("x={{ .vars.nested }}")
	if err != nil || v != "x={{ .vars.missing }}" {
		t.Fatalf("recursive evaluation: %v %v", v, err)
	}
	if _, err = s.Resolve("{{ .vars.missing }}"); err == nil {
		t.Fatal("missing became empty")
	}
	if _, err = s.Resolve("items={{ .vars.items }}"); err == nil {
		t.Fatal("collection silently stringified")
	}
	if _, err = s.Resolve("{{ call .vars.count }}"); err == nil {
		t.Fatal("expression accepted")
	}
}
func TestPredicatesShortCircuitAndKeepTypes(t *testing.T) {
	s := Scope{}
	b, err := s.Predicate(map[string]any{"all": []any{false, "{{ .missing }}"}})
	if err != nil || b {
		t.Fatalf("no short circuit: %v %v", b, err)
	}
	b, err = s.Predicate(map[string]any{"any": []any{true, "{{ .missing }}"}})
	if err != nil || !b {
		t.Fatalf("no short circuit: %v %v", b, err)
	}
	if _, err = s.Predicate("false"); err == nil {
		t.Fatal("string treated as boolean")
	}
	b, err = s.Predicate(map[string]any{"equals": []any{2, float64(2)}})
	if err != nil || !b {
		t.Fatal("snapshot changed integer equality")
	}
	b, err = s.Predicate(map[string]any{"equals": []any{2, "2"}})
	if err != nil || b {
		t.Fatal("numeric string coerced")
	}
}
func TestMissingTemplateInputFails(t *testing.T) {
	_, err := (Scope{}).Template("hello {{ .inputs.port }}")
	if err == nil {
		t.Fatal("missing template input accepted")
	}
}

func TestFrozenNumbersRemainNumericInTemplates(t *testing.T) {
	s := Scope{"vars": map[string]any{"count": json.Number("2"), "large": json.Number("9007199254740993")}}
	out, err := s.Template(`{{ if eq .vars.count 2 }}{{ printf "%d" .vars.large }}{{ end }}`)
	if err != nil || out != "9007199254740993" {
		t.Fatalf("template numeric type or precision lost: %q %v", out, err)
	}
}
