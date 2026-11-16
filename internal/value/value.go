// Package value resolves the deliberately small MMX value and predicate language.
package value

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"text/template"
)

var Reference = regexp.MustCompile(`\{\{\s*([^{}]*?)\s*\}\}`)

// Literal is a reference whose body is one back-quoted string, the form Go
// templates have for text that is not interpreted: {{`{{.Name}}`}} produces
// {{.Name}}. A value that takes references holds a literal {{ this way.
var Literal = regexp.MustCompile("\\{\\{\\s*`([^`]*)`\\s*\\}\\}")

// InvalidReference explains a {{ that is neither a reference nor the literal form.
const InvalidReference = "a reference is written {{ .vars.name }}; for a literal {{ that another tool reads, write the text in back quotes inside braces, such as {{`{{.Name}}`}}"

// WithoutLiterals removes the literal forms, leaving the text whose {{ are references.
func WithoutLiterals(v string) string {
	if !strings.Contains(v, "`") {
		return v
	}
	return Literal.ReplaceAllString(v, "")
}

type Scope map[string]any

// IsWholeReference reports text that is exactly one reference, such as
// "{{ .vars.size }}". It resolves to the value it names, with that value's
// type; a reference within other text resolves to text.
func IsWholeReference(v string) bool {
	matches := Reference.FindAllStringSubmatchIndex(v, -1)
	return len(matches) == 1 && matches[0][0] == 0 && matches[0][1] == len(v) && !Literal.MatchString(v)
}

func (s Scope) Resolve(v any) (any, error) {
	switch v := v.(type) {
	case string:
		if IsWholeReference(v) {
			match := Reference.FindStringSubmatch(v)
			return s.Get(match[1])
		}
		literals := Literal.FindAllStringSubmatchIndex(v, -1)
		var out strings.Builder
		sensitive, truncated := false, false
		// Each stretch between literal forms holds ordinary references.
		previous := 0
		for _, literal := range append(literals, []int{len(v), len(v), 0, 0}) {
			text, hasSecret, wasTruncated, err := s.expand(v[previous:literal[0]])
			if err != nil {
				return nil, err
			}
			sensitive, truncated = sensitive || hasSecret, truncated || wasTruncated
			out.WriteString(text)
			out.WriteString(v[literal[2]:literal[3]])
			previous = literal[1]
		}
		var result any = out.String()
		if truncated {
			result = Truncated{out.String()}
		}
		if sensitive {
			result = Sensitive(result)
		}
		return result, nil
	case map[string]any:
		out := map[string]any{}
		for k, x := range v {
			r, err := s.Resolve(x)
			if err != nil {
				return nil, err
			}
			out[k] = r
		}
		return out, nil
	case []any:
		out := make([]any, len(v))
		for i, x := range v {
			r, err := s.Resolve(x)
			if err != nil {
				return nil, err
			}
			out[i] = r
		}
		return out, nil
	default:
		return v, nil
	}
}

// expand substitutes the references embedded in text that has no literal form.
func (s Scope) expand(v string) (text string, sensitive, truncated bool, err error) {
	var out strings.Builder
	previous := 0
	for _, m := range Reference.FindAllStringSubmatchIndex(v, -1) {
		out.WriteString(v[previous:m[0]])
		x, err := s.Get(v[m[2]:m[3]])
		if err != nil {
			return "", false, false, err
		}
		if HasBinary(x) {
			return "", false, false, binaryInText(v[m[2]:m[3]])
		}
		sensitive = sensitive || HasSecrets(x)
		truncated = truncated || HasTruncated(x)
		t, err := Scalar(x)
		if err != nil {
			return "", false, false, err
		}
		out.WriteString(t)
		previous = m[1]
	}
	out.WriteString(v[previous:])
	// A resolved value is literal data: do not interpret its contents again.
	original := Reference.ReplaceAllString(v, "")
	// A }} on its own is ordinary text.
	if strings.Contains(original, "{{") {
		return "", false, false, fmt.Errorf("invalid value reference in %q: %s", v, InvalidReference)
	}
	return out.String(), sensitive, truncated, nil
}

func Scalar(v any) (string, error) {
	v = Plain(v)
	switch x := v.(type) {
	case Binary:
		return string(x.Bytes), nil
	case string, bool, int, int64, float64, json.Number:
		return fmt.Sprint(v), nil
	default:
		return "", fmt.Errorf("expected a string, boolean, number, or binary content, got %T", v)
	}
}
func (s Scope) Text(v string) (string, error) {
	r, err := s.Resolve(v)
	if err != nil {
		return "", err
	}
	return Scalar(r)
}

// Predicate evaluates a condition that does not inspect the node.
func (s Scope) Predicate(p any) (bool, error) { return s.PredicateOnNode(p, nil) }

// PredicateOnNode evaluates the when of a node task or node task block, the
// one place where path_exists is accepted; pathExists inspects the node.
func (s Scope) PredicateOnNode(p any, pathExists func(path string) (bool, error)) (bool, error) {
	if p == nil {
		return true, nil
	}
	if m, ok := p.(map[string]any); ok {
		if len(m) != 1 {
			return false, fmt.Errorf("predicate requires exactly one operator")
		}
		for op, arg := range m {
			switch op {
			case "equals":
				a, ok := arg.([]any)
				if !ok || len(a) != 2 {
					return false, fmt.Errorf("equals requires two values")
				}
				x, e := s.Resolve(a[0])
				if e != nil {
					return false, e
				}
				y, e := s.Resolve(a[1])
				if e != nil {
					return false, e
				}
				if HasTruncated(x) || HasTruncated(y) {
					return false, fmt.Errorf("predicate cannot use truncated command output")
				}
				return equal(x, y), nil
			case "contains", "matches":
				a, ok := arg.([]any)
				if !ok || len(a) != 2 {
					return false, fmt.Errorf("%s requires two strings", op)
				}
				var text [2]string
				for i, raw := range a {
					v, err := s.Resolve(raw)
					if err != nil {
						return false, err
					}
					if HasTruncated(v) {
						return false, fmt.Errorf("predicate cannot use truncated command output")
					}
					text[i], ok = Plain(v).(string)
					if !ok {
						return false, fmt.Errorf("%s requires two strings", op)
					}
				}
				if op == "contains" {
					return strings.Contains(text[0], text[1]), nil
				}
				pattern, err := regexp.Compile(text[1])
				if err != nil {
					return false, fmt.Errorf("matches requires a valid Go regular expression")
				}
				return pattern.MatchString(text[0]), nil
			case "all", "any":
				a, ok := arg.([]any)
				if !ok {
					return false, fmt.Errorf("%s requires a list", op)
				}
				for _, x := range a {
					b, e := s.PredicateOnNode(x, pathExists)
					if e != nil {
						return false, e
					}
					if op == "all" && !b {
						return false, nil
					}
					if op == "any" && b {
						return true, nil
					}
				}
				return op == "all", nil
			case "not":
				b, e := s.PredicateOnNode(arg, pathExists)
				return !b, e
			case "path_exists":
				if pathExists == nil {
					return false, fmt.Errorf("path_exists is accepted only in the when of a node task or node task block")
				}
				v, err := s.Resolve(arg)
				if err != nil {
					return false, err
				}
				path, ok := Plain(v).(string)
				if !ok || !strings.HasPrefix(path, "/") {
					return false, fmt.Errorf("path_exists requires an absolute path")
				}
				return pathExists(path)
			default:
				if compare, ok := comparisons[op]; ok {
					a, ok := arg.([]any)
					if !ok || len(a) != 2 {
						return false, fmt.Errorf("%s requires two values", op)
					}
					var operands [2]any
					for i, raw := range a {
						v, err := s.Resolve(raw)
						if err != nil {
							return false, err
						}
						if HasTruncated(v) {
							return false, fmt.Errorf("predicate cannot use truncated command output")
						}
						// The comparison takes the value with its sensitivity, so
						// an error never quotes a secret.
						operands[i] = v
					}
					order, err := compare.order(operands[0], operands[1])
					if err != nil {
						return false, fmt.Errorf("%s: %w", op, err)
					}
					return compare.holds(order), nil
				}
				return false, fmt.Errorf("unknown predicate %q", op)
			}
		}
	}
	r, e := s.Resolve(p)
	if e != nil {
		return false, e
	}
	if HasTruncated(r) {
		return false, fmt.Errorf("predicate cannot use truncated command output")
	}
	b, ok := Plain(r).(bool)
	if !ok {
		return false, fmt.Errorf("predicate requires a boolean, got %T", r)
	}
	return b, nil
}

func equal(a, b any) bool {
	a, b = Plain(a), Plain(b)
	// JSON snapshots turn YAML integers into numbers; canonical JSON preserves
	// their value without making strings, booleans, and numbers interchangeable.
	x, ex := json.Marshal(a)
	y, ey := json.Marshal(b)
	if ex == nil && ey == nil {
		return bytes.Equal(x, y)
	}
	return reflect.DeepEqual(a, b)
}

func (s Scope) Template(source string) (string, error) {
	t, err := parseTemplate(source)
	if err != nil {
		return "", err
	}
	t.Funcs(template.FuncMap{"mmx_value": func(path string) (any, error) {
		if path == "." {
			v := s.Data()
			s.touch(v)
			return templateValues(v), RequireAvailable(v)
		}
		v, err := s.Get(path)
		if err == nil && HasBinary(v) {
			err = binaryInText(path)
		}
		return templateValues(v), err
	}})
	if err := guardTemplate(t); err != nil {
		return "", err
	}
	var b bytes.Buffer
	err = t.Execute(&b, templateValues(s.Data()))
	return b.String(), err
}

// JSON preserves numbers as json.Number in frozen requests. Templates need
// concrete numeric Go values for comparisons and printf, without losing large
// integer precision through an intermediate float64.
func templateValues(v any) any {
	switch x := v.(type) {
	case Truncated:
		return x.Text
	case *Secret:
		return templateValues(x.Data)
	case json.Number:
		if n, err := x.Int64(); err == nil {
			return n
		}
		if n, err := x.Float64(); err == nil {
			return n
		}
		return x
	case map[string]any:
		out := map[string]any{}
		for k, v := range x {
			out[k] = templateValues(v)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, v := range x {
			out[i] = templateValues(v)
		}
		return out
	default:
		return v
	}
}
