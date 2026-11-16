package value

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
)

func TestPercentIsIntegerBoundedAndDoesNotOverflow(t *testing.T) {
	for _, tc := range []struct {
		n, p any
		want int64
	}{{int64(4097), 25, 1024}, {int64(math.MaxInt64), 100, math.MaxInt64}, {101, 0, 0}, {json.Number("7"), 50, 3}} {
		got, err := percent(tc.n, tc.p)
		if err != nil || got != tc.want {
			t.Fatal(tc, got, err)
		}
	}
	for _, args := range [][2]any{{-1, 25}, {10, -1}, {10, 101}, {1.5, 20}, {10, "25"}, {true, 10}} {
		if _, err := percent(args[0], args[1]); err == nil {
			t.Fatal(args)
		}
	}
	s := Scope{"node": map[string]any{"memory": map[string]any{"total_mib": json.Number("4097")}}, "inputs": map[string]any{"percent": 25}}
	text, err := s.Template(`cache={{ percent .node.memory.total_mib .inputs.percent }}`)
	if err != nil || text != "cache=1024" {
		t.Fatal(text, err)
	}
	s.Mark("/node/memory/total_mib", "inspection failed")
	if _, err := s.Template(`{{ percent .node.memory.total_mib 25 }}`); err == nil || !strings.Contains(err.Error(), "inspection failed") {
		t.Fatal(err)
	}
	if _, err := s.Template(`{{ if false }}{{ percent .node.memory.total_mib 25 }}{{ end }}`); err != nil {
		t.Fatal(err)
	}
}
