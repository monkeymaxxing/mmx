package value

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
)

// RetryCannotFixError marks an error of a template function whose arguments
// are fixed for the whole node job, so another try fails the same way;
// protocol.Failed gives such a result the failure kind "config", which the
// engine does not retry.
type RetryCannotFixError struct{ error }

func (e *RetryCannotFixError) Unwrap() error { return e.error }

func nonnegativeInteger(v any) (int64, error) {
	switch n := v.(type) {
	case int:
		if n >= 0 {
			return int64(n), nil
		}
	case int64:
		if n >= 0 {
			return n, nil
		}
	case uint64:
		if n <= math.MaxInt64 {
			return int64(n), nil
		}
	case json.Number:
		if n, err := strconv.ParseInt(string(n), 10, 64); err == nil && n >= 0 {
			return n, nil
		}
	}
	return 0, &RetryCannotFixError{fmt.Errorf("percent requires nonnegative integers")}
}
func percent(capacity, percentage any) (int64, error) {
	n, err := nonnegativeInteger(capacity)
	if err != nil {
		return 0, err
	}
	p, err := nonnegativeInteger(percentage)
	if err != nil {
		return 0, err
	}
	if p > 100 {
		return 0, &RetryCannotFixError{fmt.Errorf("percent requires a percentage between 0 and 100")}
	}
	return (n/100)*p + (n%100)*p/100, nil
}
