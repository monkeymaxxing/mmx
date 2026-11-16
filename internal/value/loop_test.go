package value

import (
	"errors"
	"reflect"
	"testing"
)

func TestSelectorsAndIncompleteCollections(t *testing.T) {
	s := Scope{"app": "a.b", "vars": map[string]any{"ports": []any{80, 443}}, "outputs": map[string]any{"inspect": map[string]any{"a.b": map[string]any{"ready": true}}}}
	s.Mark(Pointer("outputs", "inspect", "failed"), "inspection failed")
	for _, path := range []string{`.outputs.inspect["a.b"].ready`, `.outputs.inspect[.app].ready`} {
		if got, err := s.Get(path); err != nil || got != true {
			t.Fatalf("%s: %v %v", path, got, err)
		}
	}
	if got, err := s.Get(".vars.ports[1]"); err != nil || got != 443 {
		t.Fatal(got, err)
	}
	for _, path := range []string{".outputs.inspect", ".outputs.inspect.failed.ready"} {
		_, err := s.Get(path)
		var unavailable *UnavailableError
		if !errors.As(err, &unavailable) {
			t.Fatalf("%s lost missing producer: %v", path, err)
		}
	}
	for _, path := range []string{"vars.x", ".vars..x", ".vars.ports[-1]", ".vars.ports[2]", ".vars[call()]", ".vars[\"x\"]garbage"} {
		if _, err := s.Get(path); err == nil {
			t.Fatalf("accepted %s", path)
		}
	}
}

func TestLoopsOrderKeysAndLocalValues(t *testing.T) {
	s := Scope{"vars": map[string]any{"count": 1}, "node_tasks": map[string]any{}}
	list := []any{"b", "a"}
	items, err := s.Iterations(list, "item", "{{ .item }}")
	if err != nil || items[0].Key != "b" || items[1].Key != "a" {
		t.Fatal(items, err)
	}
	items, err = s.Iterations(map[string]any{"z": 3, "a": false}, "item", "{{ .item.key }}")
	if err != nil || items[0].Key != "a" || items[0].Value.(map[string]any)["value"] != false || items[1].Key != "z" {
		t.Fatal(items, err)
	}
	if _, err = s.Iterations([]any{"a", "a"}, "item", "{{ .item }}"); err == nil {
		t.Fatal("duplicate keys accepted")
	}
	if _, err = s.Iterations("text", "item", nil); err == nil {
		t.Fatal("scalar loop accepted")
	}
	local := s.Local()
	local["vars"].(map[string]any)["count"] = 2
	local.Mark(Pointer("node_tasks", "x"), "skipped")
	if !reflect.DeepEqual(s["vars"], map[string]any{"count": 1}) || len(s.Missing()) != 0 {
		t.Fatal("iteration leaked")
	}
}

func TestTemplatesCheckCollectionsAndOnlyEvaluatedBranches(t *testing.T) {
	s := Scope{"outputs": map[string]any{"inspect": map[string]any{"a": map[string]any{"port": 80}}}}
	s.Mark(Pointer("outputs", "inspect", "b"), "b failed")
	for _, source := range []string{`{{ range .outputs.inspect }}{{ .port }}{{ end }}`, `{{ $all := .outputs.inspect }}{{ $all }}`} {
		_, err := s.Template(source)
		var missing *UnavailableError
		if !errors.As(err, &missing) {
			t.Fatalf("incomplete template collection accepted: %q %v", source, err)
		}
	}
	got, err := s.Template(`{{ if false }}{{ .outputs.inspect.b.port }}{{ end }}{{ .outputs.inspect.a.port }}`)
	if err != nil || got != "80" {
		t.Fatal(got, err)
	}
	delete(s.Missing(), Pointer("outputs", "inspect", "b"))
	got, err = s.Template(`{{ range .outputs.inspect }}{{ .port }}:{{ $.outputs.inspect.a.port }}{{ end }}`)
	if err != nil || got != "80:80" {
		t.Fatal(got, err)
	}
}
