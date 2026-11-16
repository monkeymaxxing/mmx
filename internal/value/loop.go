package value

import (
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

type Iteration struct {
	Key   string `json:"key"`
	Value any    `json:"value"`
}

func (s Scope) Iterations(loop any, as string, key any) ([]Iteration, error) {
	resolved, err := s.Resolve(loop)
	if err != nil {
		return nil, err
	}
	items := []Iteration{}
	collection := resolved
	if secret, ok := resolved.(*Secret); ok {
		collection = secret.Data
	}
	switch values := collection.(type) {
	case []any:
		for i, v := range values {
			items = append(items, Iteration{strconv.Itoa(i), v})
		}
	case map[string]any:
		if _, secret := resolved.(*Secret); secret && key == nil {
			return nil, fmt.Errorf("a secret mapping needs a non-secret loop key")
		}
		keys := make([]string, 0, len(values))
		for k := range values {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			items = append(items, Iteration{k, map[string]any{"key": k, "value": values[k]}})
		}
	default:
		return nil, fmt.Errorf("loop requires a list or mapping, got %T", resolved)
	}
	seen := map[string]bool{}
	for i, item := range items {
		if _, ok := resolved.(*Secret); ok {
			item.Value = Sensitive(item.Value)
			items[i].Value = item.Value
		}
		if key != nil {
			local := s.Local()
			local[as] = item.Value
			v, err := local.Resolve(key)
			if err != nil {
				return nil, err
			}
			if HasSecrets(v) {
				return nil, fmt.Errorf("loop keys must not contain secrets")
			}
			label, err := Scalar(v)
			if err != nil {
				return nil, err
			}
			items[i].Key = label
		}
		k := items[i].Key
		if k == "" || seen[k] {
			return nil, fmt.Errorf("loop key %q is empty or repeated", k)
		}
		seen[k] = true
	}
	return items, nil
}

// IsPredicate reports whether an output or set_vars value is a one-operator
// predicate mapping; any other mapping is ordinary data.
func IsPredicate(v any) bool {
	m, ok := v.(map[string]any)
	if !ok || len(m) != 1 {
		return false
	}
	for op := range m {
		switch op {
		case "equals", "contains", "matches", "all", "any", "not", "path_exists":
			return true
		}
		if IsComparison(op) {
			return true
		}
	}
	return false
}

// Evaluate computes one declared output or set_vars value: a predicate form is
// a boolean, sensitive when it read a secret; any other value resolves as data.
func (s Scope) Evaluate(v any) (any, error) {
	if !IsPredicate(v) {
		return s.Resolve(v)
	}
	local, trace := s.TraceReads()
	result, err := local.Predicate(v)
	if err != nil || !trace.Used {
		return result, err
	}
	return Sensitive(result), nil
}

// EscapeKey keeps arbitrary user keys unambiguous in saved task and fleet stage paths.
func EscapeKey(key string) string { return url.PathEscape(key) }

func (i *Iteration) UnmarshalJSON(b []byte) error {
	type plain Iteration
	decoder := json.NewDecoder(strings.NewReader(string(b)))
	decoder.UseNumber()
	decoder.DisallowUnknownFields()
	if err := decoder.Decode((*plain)(i)); err != nil {
		return err
	}
	i.Value = Restore(i.Value)
	return nil
}
