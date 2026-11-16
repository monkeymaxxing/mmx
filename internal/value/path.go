package value

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Selectors are field names, numeric indexes, or another path used as a key.
// There are no calls, arithmetic, or executable expressions in references.
type Selector struct {
	Name, Dynamic string
	Index         *int
}

func Path(path string) ([]Selector, error) {
	path = strings.TrimSpace(path)
	var parts []Selector
	if !strings.HasPrefix(path, ".") {
		return nil, fmt.Errorf("reference must start with a dot: %q; %s", path, InvalidReference)
	}
	for i := 0; i < len(path); {
		switch path[i] {
		case '.':
			start := i + 1
			i = start
			for i < len(path) && ((path[i] >= 'a' && path[i] <= 'z') || (path[i] >= 'A' && path[i] <= 'Z') || (path[i] >= '0' && path[i] <= '9') || path[i] == '_' || path[i] == '-') {
				i++
			}
			if i == start {
				return nil, fmt.Errorf("empty field in %q", path)
			}
			parts = append(parts, Selector{Name: path[start:i]})
		case '[':
			start := i + 1
			depth := 1
			quoted, escaped := false, false
			i = start
			for i < len(path) {
				c := path[i]
				if quoted {
					if escaped {
						escaped = false
					} else if c == '\\' {
						escaped = true
					} else if c == '"' {
						quoted = false
					}
				} else {
					if c == '"' {
						quoted = true
					}
					if c == '[' {
						depth++
					}
					if c == ']' {
						depth--
						if depth == 0 {
							break
						}
					}
				}
				i++
			}
			if i == len(path) || quoted {
				return nil, fmt.Errorf("unclosed selector in %q", path)
			}
			token := strings.TrimSpace(path[start:i])
			i++
			part := Selector{}
			switch {
			case strings.HasPrefix(token, "."):
				if _, err := Path(token); err != nil {
					return nil, err
				}
				part.Dynamic = token
			case strings.HasPrefix(token, `"`):
				name, err := strconv.Unquote(token)
				if err != nil {
					return nil, fmt.Errorf("invalid key in %q", path)
				}
				part.Name = name
			default:
				n, err := strconv.Atoi(token)
				if err != nil || n < 0 {
					return nil, fmt.Errorf("selector must be a quoted key, nonnegative index, or reference: %q", path)
				}
				part.Index = &n
			}
			parts = append(parts, part)
		default:
			return nil, fmt.Errorf("invalid reference %q", path)
		}
	}
	return parts, nil
}

// StaticPrefix is a reference's selectors up to its first dynamic selector.
// Secret loading unlocks the whole named secret it reaches.
func StaticPrefix(parts []Selector) []Selector {
	for i, part := range parts {
		if part.Dynamic != "" {
			return parts[:i]
		}
	}
	return parts
}

// Within is what delivery sends for a reference: the value its selectors reach,
// with named secrets beneath it loaded. keys resolves each dynamic selector as
// the node will. The first selector that reads a node-only value, directly or
// through a nested selector, ends the path; the node selects within what it
// receives. A selector keys cannot resolve (an unavailable value, a secret, a
// missing key) and a missing value reach nothing: the node fails the same
// read. Selectors continue into a secret on the way: the result is that
// secret holding only the selected part, in place, so the node reads it
// through the same path, and an unresolvable selector or missing key inside it
// leaves that level empty. Every loaded or selected secret records the scope
// secret it replaces (origin). Availability marks do not apply to the value
// itself: the node reports it missing.
func (s Scope) Within(path string, keys Scope, nodeOnly func([]Selector) bool) (any, error) {
	parts, err := Path(path)
	if err != nil {
		return nil, err
	}
	type level struct {
		container any
		key       string
		index     *int
		found     bool
	}
	var current any = map[string]any(s)
	var entered, secret *Secret // the scope secret the path selects in, and its loaded value
	var levels []level          // the selections made inside that secret's data
walk:
	for _, part := range parts {
		if part.Dynamic != "" && readsNodeOnly(part.Dynamic, nodeOnly) {
			break
		}
		if x, ok := current.(*Secret); ok && entered == nil {
			loaded, err := loadSecrets(x, false)
			if err != nil {
				return nil, err
			}
			if secret = loaded.(*Secret); !secret.Available {
				return x, nil // the node reports it unavailable
			}
			entered, current = x, secret.Data
		}
		key, index, err := keys.selector(part, path)
		if entered == nil {
			if err != nil {
				return nil, nil
			}
			current, _ = choose(current, key, index)
			continue
		}
		switch current.(type) {
		case map[string]any, []any:
		default:
			break walk // the node fails any further selection itself
		}
		l := level{container: current, key: key, index: index}
		if err == nil {
			current, l.found = choose(current, key, index)
		}
		levels = append(levels, l)
		if !l.found {
			break
		}
	}
	if entered == nil {
		return loadSecrets(current, true)
	}
	for i := len(levels) - 1; i >= 0; i-- {
		l := levels[i] //nolint:nilaway // The reverse loop accesses levels only while its index is nonnegative.
		switch c := l.container.(type) {
		case map[string]any:
			kept := map[string]any{}
			if l.found {
				kept[l.key] = current
			}
			current = kept
		case []any:
			kept := make([]any, len(c))
			if l.found {
				kept[*l.index] = current
			}
			current = kept
		}
	}
	return &Secret{origin: entered, Data: current, Name: secret.Name, Path: secret.Path, Available: true}, nil //nolint:nilaway // entered != nil means secret was assigned by a successful load.
}

// choose selects key from a map or index from a list.
func choose(container any, key string, index *int) (any, bool) {
	switch c := container.(type) {
	case map[string]any:
		v, ok := c[key]
		return v, ok
	case []any:
		if index != nil && *index < len(c) {
			return c[*index], true
		}
	}
	return nil, false
}

// readsHostOnly reports whether a selector's path, or a selector nested in it,
// reads a node-only value.
func readsNodeOnly(path string, nodeOnly func([]Selector) bool) bool {
	parts, err := Path(path)
	if err != nil || nodeOnly(parts) {
		return true
	}
	for _, part := range parts {
		if part.Dynamic != "" && readsNodeOnly(part.Dynamic, nodeOnly) {
			return true
		}
	}
	return false
}

// selector is a part's map key and list index. A dynamic part must resolve in
// this scope to a public string or nonnegative integer.
func (s Scope) selector(part Selector, path string) (string, *int, error) {
	key, index := part.Name, part.Index
	if part.Dynamic != "" {
		v, err := s.Get(part.Dynamic)
		if err != nil {
			return "", nil, err
		}
		// mmx validate refuses the forms it can see; a value that is secret
		// only at run time, such as a variable set from a secret, ends here.
		if HasSecrets(v) {
			return "", nil, fmt.Errorf("%s uses %s as a selector, but the selector holds a secret. Secret values cannot select another value. Read the secret directly as {{ %s }}, or use a non-secret value, such as an inventory variable, to select the value on the controller machine", strings.TrimSpace(path), part.Dynamic, part.Dynamic)
		}
		if text, ok := v.(string); ok {
			key = text
		} else {
			text, err := Scalar(v)
			if err != nil {
				return "", nil, err
			}
			n, err := strconv.Atoi(text)
			if err != nil || n < 0 {
				return "", nil, fmt.Errorf("%s: selector must be a string or nonnegative integer", path)
			}
			index, key = &n, text
		}
	}
	if index != nil {
		key = strconv.Itoa(*index)
	}
	return key, index, nil
}

type UnavailableError struct{ Path, Reason string }

func (e *UnavailableError) Error() string { return e.Path + ": " + e.Reason }

// Availability is separate from ordinary user values, including maps whose
// shape happens to resemble an error. The empty root cannot be user-addressed.
func Pointer(parts ...string) string {
	out := ""
	for _, part := range parts {
		out += "/" + strings.NewReplacer("~", "~0", "/", "~1").Replace(part)
	}
	return out
}
func (s Scope) Missing() map[string]string {
	m, _ := s[""].(map[string]string)
	return m
}
func (s Scope) Mark(path, reason string) {
	if s.Missing() == nil {
		s[""] = map[string]string{}
	}
	s.Missing()[path] = reason
}

const omittedEntriesKey = "\x00omitted_entries"

// OmittedEntries are the entries that mappings in s leave out on purpose, by
// path, each with the reason a read of it gets.
func (s Scope) OmittedEntries() map[string]string {
	m, _ := s[omittedEntriesKey].(map[string]string)
	return m
}

// MarkOmittedEntry records why the mapping that would hold path has no entry
// there, such as a fleet stage's outputs for a node its when skipped: a read
// of the mapping gets it without the entry, and a read of the entry, or of a
// value inside it, is unavailable for the reason.
func (s Scope) MarkOmittedEntry(path, reason string) {
	if s.OmittedEntries() == nil {
		s[omittedEntriesKey] = map[string]string{}
	}
	s.OmittedEntries()[path] = reason
}

const partlyKnownKey = "\x00partly_known"

// PartlyKnown are the mappings in s that hold only some of their entries, by
// path, each with the reason a read of another entry gets.
func (s Scope) PartlyKnown() map[string]string {
	m, _ := s[partlyKnownKey].(map[string]string)
	return m
}

// MarkPartlyKnown records that the mapping at path holds only some of its
// entries, such as a node's name before MMX connects to it: a read of an
// entry it holds gets the value, while a read of another entry, or of the
// whole mapping, is unavailable for the reason.
func (s Scope) MarkPartlyKnown(path, reason string) {
	if s.PartlyKnown() == nil {
		s[partlyKnownKey] = map[string]string{}
	}
	s.PartlyKnown()[path] = reason
}
func (s Scope) Get(path string) (any, error) {
	parts, err := Path(path)
	if err != nil {
		return nil, err
	}
	var current any = map[string]any(s)
	selected := ""
	var inherited *Secret
	for _, part := range parts {
		if secret, ok := current.(*Secret); ok {
			loaded, err := loadSecrets(secret, false)
			if err != nil {
				return nil, err
			}
			secret = loaded.(*Secret)
			s.touch(secret)
			if !secret.Available {
				return nil, &UnavailableError{path, secret.unavailable()}
			}
			copy := *secret
			copy.Path = append([]string(nil), secret.Path...)
			inherited = &copy
			current = secret.Data
		}
		key, index, err := s.selector(part, path)
		if err != nil {
			return nil, err
		}
		if inherited != nil {
			inherited.Path = append(inherited.Path, key)
		}
		parent := selected
		selected += Pointer(key)
		if reason, ok := s.Missing()[selected]; ok {
			return nil, &UnavailableError{path, reason}
		}
		switch value := current.(type) {
		case map[string]any:
			var ok bool
			current, ok = value[key]
			if reason, omitted := s.OmittedEntries()[selected]; !ok && omitted {
				return nil, &UnavailableError{path, reason}
			}
			if reason, partly := s.PartlyKnown()[parent]; !ok && partly {
				return nil, &UnavailableError{path, reason}
			}
			if !ok {
				return nil, fmt.Errorf("%s: value is unavailable at %s", path, selected)
			}
			if bad, ok := current.(NotJSON); ok {
				return nil, fmt.Errorf("%s: %s", path, bad.Reason)
			}
		case []any:
			if index == nil || *index >= len(value) {
				return nil, fmt.Errorf("%s: invalid list index", path)
			}
			current = value[*index]
		default:
			return nil, fmt.Errorf("%s: cannot select %q", path, key)
		}
	}
	keys := make([]string, 0, len(s.Missing()))
	for key := range s.Missing() {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if strings.HasPrefix(key, selected+"/") {
			return nil, &UnavailableError{path, s.Missing()[key]}
		}
	}
	for key, reason := range s.PartlyKnown() {
		if key == selected || strings.HasPrefix(key, selected+"/") {
			return nil, &UnavailableError{path, reason}
		}
	}
	if inherited != nil {
		inherited.Data = current
		current = inherited
	}
	current, err = loadSecrets(current, false)
	if err != nil {
		return nil, err
	}
	s.touch(current)
	if secret, ok := current.(*Secret); ok && !secret.Available {
		return nil, &UnavailableError{path, secret.unavailable()}
	}
	if err := RequireAvailable(current); err != nil {
		return nil, err
	}
	return current, nil
}
func (s Scope) Local() Scope {
	out := Scope{}
	for k, v := range s {
		out[k] = v
	}
	for _, key := range []string{"vars", "node_tasks"} {
		copy := map[string]any{}
		if m, ok := s[key].(map[string]any); ok {
			for k, v := range m {
				copy[k] = v
			}
		}
		out[key] = copy
	}
	missing := map[string]string{}
	for k, v := range s.Missing() {
		missing[k] = v
	}
	out[""] = missing
	for key, marks := range map[string]map[string]string{omittedEntriesKey: s.OmittedEntries(), partlyKnownKey: s.PartlyKnown()} {
		if marks != nil {
			copied := map[string]string{}
			for k, v := range marks {
				copied[k] = v
			}
			out[key] = copied
		}
	}
	return out
}

// Clear replaces the availability of a named local result at its new assignment.
func (s Scope) Clear(path string) {
	for key := range s.Missing() {
		if key == path || strings.HasPrefix(key, path+"/") {
			delete(s.Missing(), key)
		}
	}
}
