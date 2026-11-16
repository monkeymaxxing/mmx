package facts

import (
	"bytes"
	"encoding/json"
	"sort"
)

// Changes compares saved observations, without another node read. Timestamps
// identify the observations; only values and availability are compared.
func Changes(before, after *Snapshot) []string {
	if before == nil || after == nil {
		return nil
	}
	changed := map[string]bool{}
	var compare func(string, any, any)
	equal := func(a, b any) bool {
		x, _ := json.Marshal(a)
		y, _ := json.Marshal(b)
		return bytes.Equal(x, y)
	}
	compare = func(path string, a, b any) {
		if equal(a, b) {
			return
		}
		am, aok := a.(map[string]any)
		bm, bok := b.(map[string]any)
		if aok && bok {
			keys := map[string]bool{}
			for k := range am {
				keys[k] = true
			}
			for k := range bm {
				keys[k] = true
			}
			for k := range keys {
				compare(path+"/"+pointerPart(k), am[k], bm[k])
			}
			return
		}
		changed[path] = true
	}
	compare("/node", before.Values, after.Values)
	for path, reason := range before.Unavailable {
		if after.Unavailable[path] != reason {
			changed[path] = true
		}
	}
	for path, reason := range after.Unavailable {
		if before.Unavailable[path] != reason {
			changed[path] = true
		}
	}
	out := make([]string, 0, len(changed))
	for path := range changed {
		out = append(out, path)
	}
	sort.Strings(out)
	return out
}
