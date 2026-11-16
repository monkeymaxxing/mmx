package value

import "encoding/json"

// Truncated retains a command-output limit through references, assignments and
// saved exports. Display may show the partial text; predicates cannot trust it.
type Truncated struct{ Text string }

func (v Truncated) MarshalJSON() ([]byte, error) {
	return json.Marshal(map[string]string{"$mmx_truncated": v.Text})
}

func HasTruncated(v any) bool {
	switch x := v.(type) {
	case Truncated:
		return true
	case *Secret:
		return HasTruncated(x.Data) //nolint:nilaway // The type switch dereferences only allocated *Secret values; missing lookups return nil interfaces.
	case map[string]any:
		for _, child := range x {
			if HasTruncated(child) {
				return true
			}
		}
	case []any:
		for _, child := range x {
			if HasTruncated(child) {
				return true
			}
		}
	}
	return false
}
