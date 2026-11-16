// Package moduleio owns the language-independent executable-module JSON format
// and the authoring helper shared by the controller and runner. It has no engine,
// transport, project, or node-state dependencies.
package moduleio

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"unicode/utf8"
)

const (
	Version         = 1
	MaxRequestBytes = 16 << 20
	MaxResultBytes  = 1 << 20
)

type Request struct {
	Protocol int            `json:"protocol"`
	Mode     string         `json:"mode"`
	Inputs   map[string]any `json:"inputs"`
}

type Result struct {
	Protocol int    `json:"protocol"`
	Status   string `json:"status"`
	Change   string `json:"change"`
	Reason   string `json:"reason,omitempty"`
	// Outputs are the module's named outputs, a JSON object.
	Outputs json.RawMessage `json:"outputs,omitempty"`
	// Diff is text that shows what changed, or would change.
	Diff string `json:"diff,omitempty"`
	// Fingerprint changes when the managed state changes, such as a hash of
	// it; a node task block's services follow it.
	Fingerprint string `json:"fingerprint,omitempty"`
	Secret      bool   `json:"secret,omitempty"`
}

func ReadRequest(input io.Reader) (Request, error) {
	var request Request
	object, err := readObject(input, MaxRequestBytes)
	if err != nil {
		return request, fmt.Errorf("module request: %w", err)
	}
	if err := fields(object, "protocol", "mode", "inputs"); err != nil {
		return request, fmt.Errorf("module request: %w", err)
	}
	if !version(object["protocol"]) {
		return request, fmt.Errorf("module request requires protocol: 1")
	}
	request.Protocol = Version
	request.Mode, _ = object["mode"].(string)
	if !validMode(request.Mode) {
		return request, fmt.Errorf("module mode must be check or apply")
	}
	request.Inputs, _ = object["inputs"].(map[string]any)
	if request.Inputs == nil {
		return request, fmt.Errorf("module inputs must be an object")
	}
	return request, nil
}

// ReadResult accepts only the public module fields. A valid partial-change
// report survives a failing exit. Successful JSON cannot override process failure.
func ReadResult(input io.Reader, mode string, exitCode int) (Result, error) {
	var result Result
	object, err := readObject(input, MaxResultBytes)
	if err != nil {
		return result, fmt.Errorf("module result: %w", err)
	}
	if err := fields(object, "protocol", "status", "change", "reason", "outputs", "diff", "fingerprint", "secret"); err != nil {
		return result, fmt.Errorf("module result: %w", err)
	}
	if !version(object["protocol"]) {
		return result, fmt.Errorf("module result requires protocol: 1")
	}
	result.Protocol = Version
	result.Status, _ = object["status"].(string)
	result.Change, _ = object["change"].(string)
	if v, exists := object["reason"]; exists {
		var ok bool
		if result.Reason, ok = v.(string); !ok {
			return Result{}, fmt.Errorf("module reason must be a string")
		}
	}
	if v, exists := object["secret"]; exists {
		var ok bool
		if result.Secret, ok = v.(bool); !ok {
			return Result{}, fmt.Errorf("module secret must be a boolean")
		}
	}
	for name, target := range map[string]*string{"diff": &result.Diff, "fingerprint": &result.Fingerprint} {
		if v, exists := object[name]; exists {
			var ok bool
			if *target, ok = v.(string); !ok {
				return Result{}, fmt.Errorf("module %s must be a string", name)
			}
		}
	}
	if v, exists := object["outputs"]; exists {
		if _, ok := v.(map[string]any); !ok {
			return Result{}, fmt.Errorf("module outputs must be an object of named outputs")
		}
		if result.Outputs, err = json.Marshal(v); err != nil {
			return Result{}, fmt.Errorf("invalid module outputs")
		}
	}
	if err := result.validate(mode); err != nil {
		return Result{}, err
	}
	if exitCode != 0 && result.Status != "failed" {
		result.Reason = fmt.Sprintf("module exited with status %d after reporting %s", exitCode, result.Status)
		result.Status = "failed"
	}
	return result, nil
}

func (r Result) validate(mode string) error {
	if r.Protocol != Version {
		return fmt.Errorf("module result requires protocol: 1")
	}
	if !validMode(mode) {
		return fmt.Errorf("module mode must be check or apply")
	}
	switch r.Status {
	case "succeeded":
	case "failed":
		if r.Reason == "" {
			return fmt.Errorf("a failed result requires a reason")
		}
	case "not_checked":
		if mode != "check" || r.Change != "unknown" || r.Reason == "" {
			return fmt.Errorf("module status not_checked requires mode: check, change: unknown, and a nonempty reason")
		}
	default:
		return fmt.Errorf("module status must be succeeded, failed, or not_checked")
	}
	switch r.Change {
	case "unchanged":
	case "changed":
		if mode != "apply" {
			return fmt.Errorf("module result in check mode cannot report change: changed")
		}
	case "would_change":
		if mode != "check" {
			return fmt.Errorf("module result in apply mode cannot report change: would_change")
		}
	case "unknown":
		if r.Status == "succeeded" {
			return fmt.Errorf("module result with status: succeeded cannot report change: unknown")
		}
	default:
		return fmt.Errorf("module change must be unchanged, changed, would_change, or unknown")
	}
	if !utf8.ValidString(r.Reason) {
		return fmt.Errorf("module reason must be UTF-8 text")
	}
	if !utf8.ValidString(r.Diff) || !utf8.ValidString(r.Fingerprint) {
		return fmt.Errorf("module diff and fingerprint must be UTF-8 text")
	}
	if len(r.Outputs) > 0 {
		v, err := parseJSON(r.Outputs)
		if _, object := v.(map[string]any); err != nil || !object {
			return fmt.Errorf("module outputs must be an object of named outputs")
		}
	}
	return nil
}

func WriteResult(output io.Writer, mode string, result Result) error {
	if err := result.validate(mode); err != nil {
		return err
	}
	data, err := json.Marshal(result)
	if err != nil {
		return fmt.Errorf("cannot encode module result")
	}
	if len(data)+1 > MaxResultBytes {
		return fmt.Errorf("module result exceeds 1 MiB")
	}
	_, err = output.Write(append(data, '\n'))
	return err
}

func validMode(mode string) bool { return mode == "check" || mode == "apply" }

func version(v any) bool {
	number, ok := v.(json.Number)
	return ok && number.String() == "1"
}

func fields(object map[string]any, allowed ...string) error {
	for key := range object {
		found := false
		for _, name := range allowed {
			if key == name {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("unknown JSON field")
		}
	}
	return nil
}

func readObject(input io.Reader, limit int64) (map[string]any, error) {
	data, err := readBounded(input, limit)
	if err != nil {
		return nil, err
	}
	v, err := parseJSON(data)
	if err != nil {
		return nil, err
	}
	object, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("expected a JSON object")
	}
	return object, nil
}

func readBounded(input io.Reader, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(input, limit+1))
	if err != nil {
		return nil, fmt.Errorf("could not read input")
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("input exceeds %d bytes", limit)
	}
	if !utf8.Valid(data) {
		return nil, fmt.Errorf("input must be UTF-8")
	}
	return data, nil
}

// Decode once using standard-library tokens, preserving numbers and rejecting
// duplicate fields at any depth. Errors never quote request values or source.
func parseJSON(data []byte) (any, error) {
	if !utf8.Valid(data) {
		return nil, fmt.Errorf("input must be UTF-8")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	v, err := readValue(decoder, 0)
	if err != nil {
		return nil, err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, fmt.Errorf("expected exactly one JSON value")
	}
	return v, nil
}

func readValue(decoder *json.Decoder, depth int) (any, error) {
	if depth > 64 {
		return nil, fmt.Errorf("JSON nesting exceeds 64 levels")
	}
	token, err := decoder.Token()
	if err != nil {
		return nil, fmt.Errorf("invalid JSON")
	}
	switch token {
	case json.Delim('{'):
		object := map[string]any{}
		for decoder.More() {
			key, err := decoder.Token()
			name, ok := key.(string)
			if err != nil || !ok {
				return nil, fmt.Errorf("invalid JSON object")
			}
			if _, exists := object[name]; exists {
				return nil, fmt.Errorf("duplicate JSON field")
			}
			value, err := readValue(decoder, depth+1)
			if err != nil {
				return nil, err
			}
			object[name] = value
		}
		if close, err := decoder.Token(); err != nil || close != json.Delim('}') {
			return nil, fmt.Errorf("invalid JSON object")
		}
		return object, nil
	case json.Delim('['):
		array := []any{}
		for decoder.More() {
			value, err := readValue(decoder, depth+1)
			if err != nil {
				return nil, err
			}
			array = append(array, value)
		}
		if close, err := decoder.Token(); err != nil || close != json.Delim(']') {
			return nil, fmt.Errorf("invalid JSON array")
		}
		return array, nil
	case json.Delim('}'), json.Delim(']'):
		return nil, fmt.Errorf("invalid JSON")
	default:
		return token, nil
	}
}
