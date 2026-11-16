package value

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
)

// NotJSON stands where a result's stdout_json or response_body_json would be
// when the output is not JSON. Reading it is an error that says why; a result
// that nobody reads as JSON is not affected.
type NotJSON struct{ Reason string }

// A saved or exported result carries no parsed copy of its own output.
func (NotJSON) MarshalJSON() ([]byte, error) { return []byte("null"), nil }

// ParsedJSON parses a command's output or a response body, named by field, as
// JSON. Numbers keep their exact text, as everywhere in saved values.
func ParsedJSON(field, text string, truncated, secret bool) any {
	if truncated {
		return NotJSON{field + " was truncated, so it cannot be read as JSON"}
	}
	decoder := json.NewDecoder(bytes.NewReader([]byte(text)))
	decoder.UseNumber()
	var parsed any
	if err := decoder.Decode(&parsed); err != nil {
		if err == io.EOF {
			return NotJSON{field + " is empty, so it cannot be read as JSON"}
		}
		return NotJSON{fmt.Sprintf("%s is not valid JSON: %v", field, err)}
	}
	if _, err := decoder.Token(); err != io.EOF {
		return NotJSON{field + " is not valid JSON: text follows the first JSON value"}
	}
	if secret {
		return Sensitive(parsed)
	}
	return parsed
}
