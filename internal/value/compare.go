package value

import (
	"encoding/json"
	"fmt"
	"math/big"
	"regexp"
	"strings"
)

// A comparison predicate orders its two operands as numbers or as versions
// and says whether that order satisfies it.
type comparison struct {
	order func(left, right any) (int, error)
	holds func(order int) bool
}

var comparisons = func() map[string]comparison {
	holds := map[string]func(int) bool{
		"equals":       func(o int) bool { return o == 0 },
		"less_than":    func(o int) bool { return o < 0 },
		"at_most":      func(o int) bool { return o <= 0 },
		"greater_than": func(o int) bool { return o > 0 },
		"at_least":     func(o int) bool { return o >= 0 },
	}
	out := map[string]comparison{}
	for name, h := range holds {
		out["number_"+name] = comparison{compareNumbers, h}
		out["version_"+name] = comparison{compareVersions, h}
	}
	return out
}()

// IsComparison reports whether op is one of the number_* or version_* predicates.
func IsComparison(op string) bool { _, ok := comparisons[op]; return ok }

var (
	decimalNumber = regexp.MustCompile(`^[+-]?[0-9]+(\.[0-9]+)?([eE][+-]?[0-9]+)?$`)
	dottedVersion = regexp.MustCompile(`^v?[0-9]+(\.[0-9]+)*$`)
)

// number accepts a YAML or JSON number, or text with whitespace around one,
// such as a command's output.
func number(v any) (*big.Rat, error) {
	var text string
	switch x := Plain(v).(type) {
	case string:
		text = strings.TrimSpace(x)
	case int, int64, float64, json.Number:
		text = fmt.Sprint(x)
	default:
		return nil, fmt.Errorf("%s is not a number", describe(v))
	}
	if !decimalNumber.MatchString(text) {
		return nil, fmt.Errorf("%s is not a number", describe(v))
	}
	n, ok := new(big.Rat).SetString(text)
	if !ok {
		return nil, fmt.Errorf("%s is not a number", describe(v))
	}
	return n, nil
}

func compareNumbers(left, right any) (int, error) {
	l, err := number(left)
	if err != nil {
		return 0, err
	}
	r, err := number(right)
	if err != nil {
		return 0, err
	}
	return l.Cmp(r), nil
}

// version accepts dotted numbers with an optional leading v, as text with
// whitespace around it, or a whole number. A YAML number with a fraction is
// refused: YAML reads 1.30 as 1.3.
func version(v any) ([]*big.Int, error) {
	var text string
	plain := Plain(v)
	switch x := plain.(type) {
	case string:
		text = strings.TrimSpace(x)
	case int, int64:
		text = fmt.Sprint(x)
	case json.Number:
		text = x.String()
	case float64:
		return nil, fmt.Errorf("%s is a number with a fraction; write a version in quotes, such as \"1.30\"", describe(v))
	default:
		return nil, fmt.Errorf("%s is not a version", describe(v))
	}
	if _, isNumber := plain.(json.Number); isNumber && strings.ContainsAny(text, ".eE") {
		return nil, fmt.Errorf("%s is a number with a fraction; write a version in quotes, such as \"1.30\"", describe(v))
	}
	if !dottedVersion.MatchString(text) {
		return nil, fmt.Errorf("%s is not a version made of numbers separated by dots, such as 1.30.2 or v2.4", describe(v))
	}
	var parts []*big.Int
	for _, digits := range strings.Split(strings.TrimPrefix(text, "v"), ".") {
		n, _ := new(big.Int).SetString(digits, 10)
		parts = append(parts, n)
	}
	return parts, nil
}

// compareVersions compares number by number; a missing number counts as 0, so
// 1.30 equals 1.30.0.
func compareVersions(left, right any) (int, error) {
	l, err := version(left)
	if err != nil {
		return 0, err
	}
	r, err := version(right)
	if err != nil {
		return 0, err
	}
	zero := new(big.Int)
	for i := 0; i < len(l) || i < len(r); i++ {
		a, b := zero, zero
		if i < len(l) {
			a = l[i]
		}
		if i < len(r) {
			b = r[i]
		}
		if order := a.Cmp(b); order != 0 {
			return order, nil
		}
	}
	return 0, nil
}

// describe names an operand in an error. A secret, or a value made from
// one, is never quoted: the error can reach results and saved history.
func describe(v any) string {
	if HasSecrets(v) {
		return "a secret value"
	}
	if text, ok := v.(string); ok {
		return fmt.Sprintf("%q", text)
	}
	return fmt.Sprintf("%v (%T)", v, v)
}
