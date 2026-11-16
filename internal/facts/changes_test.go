package facts

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

func TestChangesCompareSavedValuesAndAvailability(t *testing.T) {
	before := &Snapshot{Values: map[string]any{"cpu": map[string]any{"count": 2}, "memory": map[string]any{"total_mib": int64(4096)}, "network": map[string]any{}}, Unavailable: map[string]string{"/node/network/interfaces": "inspection failed"}}
	after := &Snapshot{CollectedAt: time.Now(), Values: map[string]any{"cpu": map[string]any{"count": json.Number("2")}, "memory": map[string]any{"total_mib": json.Number("8192")}, "network": map[string]any{"interfaces": map[string]any{}}}}
	want := []string{"/node/memory/total_mib", "/node/network/interfaces"}
	if got := Changes(before, after); !reflect.DeepEqual(got, want) {
		t.Fatal(got)
	}
	if len(Changes(nil, after)) != 0 || len(Changes(after, after)) != 0 {
		t.Fatal("invented changes")
	}
	after.Unavailable = map[string]string{"/node/network/interfaces": "a different failure"}
	if got := Changes(before, after); !reflect.DeepEqual(got, want) {
		t.Fatal(got)
	}
}
