package moduleio

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func runHelper(t *testing.T, input string, args ...string) (string, error) {
	t.Helper()
	var output bytes.Buffer
	err := Run(args, strings.NewReader(input), &output)
	return output.String(), err
}

func TestHelperGet(t *testing.T) {
	request := `{"protocol":1,"mode":"check","inputs":{"text":"quote\" slash\\ 日本語\n\n","nothing":null,"off":false,"zero":0,"big":9007199254740993,"empty":"","list":["first",false],"object":{},"a/b":{"~":"yes"},"nul":"a\u0000b"}}`
	for _, tc := range []struct{ pointer, kind, want string }{
		{"/mode", "string", "check"},
		{"/inputs/text", "string", "quote\" slash\\ 日本語\n\n"},
		{"/inputs/empty", "string", ""},
		{"/inputs/nothing", "null", "null"},
		{"/inputs/off", "boolean", "false"},
		{"/inputs/zero", "number", "0"},
		{"/inputs/big", "number", "9007199254740993"},
		{"/inputs/list", "array", `["first",false]`},
		{"/inputs/list/0", "string", "first"},
		{"/inputs/object", "object", `{}`},
		{"/inputs/a~1b/~0", "string", "yes"},
		{"/inputs/nul", "json", `"a\u0000b"`},
	} {
		t.Run(tc.pointer, func(t *testing.T) {
			got, err := runHelper(t, request, "get", "--type", tc.kind, tc.pointer)
			if err != nil || got != tc.want {
				t.Fatalf("got %q, want %q: %v", got, tc.want, err)
			}
		})
	}
	for _, tc := range []struct{ pointer, kind string }{
		{"/inputs/missing", "json"}, {"/inputs/nothing", "string"},
		{"/inputs/off", "number"}, {"/inputs/nul", "string"},
		{"/inputs/zero", "unsupported"}, {"/inputs/text/child", "json"},
		{"/inputs/list/-1", "json"}, {"/inputs/list/01", "json"},
		{"/inputs/list/", "json"}, {"/inputs/list/2", "json"},
		{"/inputs/list/99999999999999999999", "json"},
		{"/inputs/~", "json"}, {"/inputs/~2", "json"}, {"inputs", "json"},
	} {
		got, err := runHelper(t, request, "get", "--type", tc.kind, tc.pointer)
		if err == nil || got != "" {
			t.Fatalf("invalid extraction %s %s: %q %v", tc.pointer, tc.kind, got, err)
		}
	}
	got, err := runHelper(t, request, "get", "")
	if err != nil || !json.Valid([]byte(got)) {
		t.Fatalf("root pointer: %q %v", got, err)
	}
}

func TestHelperRequest(t *testing.T) {
	request := " {\"protocol\":1,\"mode\":\"apply\",\"inputs\":{\"text\":\"<>&\u2028\"}}\n"
	got, err := runHelper(t, request, "request", "--inputs", "text")
	if err != nil || got != request {
		t.Fatalf("request changed: %q %v", got, err)
	}
	for _, args := range [][]string{
		{"request", "--inputs", "other"}, {"request", "--inputs", "text,"},
		{"request", "extra"}, {"get"}, {"request", "--private-token"},
		{"result", "--secret", "private-token"}, {"private-token"},
	} {
		got, err := runHelper(t, request, args...)
		if err == nil || got != "" || strings.Contains(err.Error(), "private-token") {
			t.Fatalf("invalid arguments: %q %v", got, err)
		}
	}
}

func TestHelperResult(t *testing.T) {
	text := "quote\" slash\\ 日本語\n\n"
	for _, tc := range []struct {
		flags []string
		input string
	}{
		{[]string{"--reason-stdin"}, text},
		{[]string{"--output-stdin", "greeting", "--reason", "second change failed"}, text},
		{[]string{"--diff-stdin", "--reason", "second change failed"}, text},
		{[]string{"--output", "person", "Barbara", "--output-json", "counts", `{"nested":[null,false,0,"",9007199254740993]}`, "--fingerprint", "abc", "--reason", "second change failed"}, ""},
	} {
		args := append([]string{"result", "--mode", "apply", "--status", "failed", "--change", "changed", "--secret"}, tc.flags...)
		got, err := runHelper(t, tc.input, args...)
		if err != nil {
			t.Fatal(err)
		}
		r, err := ReadResult(strings.NewReader(got), "apply", 1)
		if err != nil || !r.Secret || r.Status != "failed" || r.Change != "changed" {
			t.Fatalf("result: %+v %v", r, err)
		}
		var outputs map[string]any
		if len(r.Outputs) > 0 {
			d := json.NewDecoder(bytes.NewReader(r.Outputs))
			d.UseNumber()
			if err := d.Decode(&outputs); err != nil {
				t.Fatal(err)
			}
		}
		switch tc.flags[0] {
		case "--reason-stdin":
			if r.Reason != text {
				t.Fatal("reason changed")
			}
		case "--output-stdin":
			if outputs["greeting"] != text {
				t.Fatalf("output changed: %q", outputs["greeting"])
			}
		case "--diff-stdin":
			if r.Diff != text {
				t.Fatal("diff changed")
			}
		case "--output":
			counts, _ := json.Marshal(outputs["counts"])
			if outputs["person"] != "Barbara" || string(counts) != `{"nested":[null,false,0,"",9007199254740993]}` || r.Fingerprint != "abc" {
				t.Fatalf("outputs changed: %v %s", outputs, counts)
			}
		}
	}
	// No stdin field means the helper must not attempt to read stdin.
	var output bytes.Buffer
	if err := Run([]string{"result", "--mode", "check", "--change", "unchanged"}, brokenReader{}, &output); err != nil {
		t.Fatal(err)
	}
}

func TestHelperResultErrorsWriteNothing(t *testing.T) {
	base := []string{"result", "--mode", "apply", "--change", "unchanged"}
	for _, tc := range []struct {
		input string
		extra []string
	}{
		{"", []string{"--status", "failed"}},
		{"", []string{"--status", "failed", "--reason-stdin"}},
		{"", []string{"--output-json", "data", "private-token"}},
		{"", []string{"--output-json", "data", `{"x":1,"x":2}`}},
		{"", []string{"--output", "data", "one", "--output", "data", "two"}},
		{"\xff", []string{"--output-stdin", "data"}},
		{"\xff", []string{"--reason-stdin"}},
		{"", []string{"--reason-stdin", "--diff-stdin"}},
		{"", []string{"--reason", "fixed", "--reason-stdin"}},
		{strings.Repeat("x", MaxResultBytes+1), []string{"--reason-stdin"}},
		{strings.Repeat("x", MaxResultBytes), []string{"--output-stdin", "data"}},
	} {
		got, err := runHelper(t, tc.input, append(base, tc.extra...)...)
		if err == nil || got != "" || strings.Contains(err.Error(), "private-token") {
			t.Fatalf("invalid result input: %q %v", got, err)
		}
	}
}

func TestHelperGetInteger(t *testing.T) {
	for _, tc := range []struct{ number, want string }{
		{"0", "0"}, {"-0.0", "0"}, {"42", "42"}, {"-42", "-42"},
		{"42.000", "42"}, {"4.2e1", "42"}, {"4200e-2", "42"},
		{"1E+3", "1000"}, {"9007199254740993", "9007199254740993"},
		{"9223372036854775807", "9223372036854775807"},
		{"-9223372036854775808", "-9223372036854775808"},
		{"9.223372036854775807e18", "9223372036854775807"},
		{"-922337203685477580800e-2", "-9223372036854775808"},
		{"0e999999999999999999999", "0"},
	} {
		got, err := runHelper(t, `{"protocol":1,"mode":"check","inputs":{"number":`+tc.number+`}}`, "get", "--type", "integer", "/inputs/number")
		if err != nil || got != tc.want {
			t.Fatalf("%s: got %q, want %q: %v", tc.number, got, tc.want, err)
		}
	}
	for _, number := range []string{
		"1.5", "1e-1", "12e-1", "1.0000000000000000001", "1e1000000000", "1e-1000000000",
		"1e999999999999999999999", "1e-999999999999999999999", "9223372036854775808",
		"-9223372036854775809", "9223372036854775807.1", "-9223372036854775808.1",
		"9.223372036854775808e18", `"42"`, "true", "null", "[]", "{}",
	} {
		got, err := runHelper(t, `{"protocol":1,"mode":"check","inputs":{"number":`+number+`}}`, "get", "--type", "integer", "/inputs/number")
		if err == nil || got != "" {
			t.Fatalf("accepted %s: %q %v", number, got, err)
		}
	}
}

func TestHelperCommandErrors(t *testing.T) {
	for _, args := range [][]string{
		{"unknown"}, {"get", "--invalid"}, {"get", "--type", "unsupported", "/mode"},
		{"result", "--mode", "invalid", "--change", "unchanged"},
		{"result", "--mode", "check", "--change", "invalid"},
		{"result", "--mode", "check", "--change", "unchanged", "--status", "invalid"},
	} {
		got, err := runHelper(t, "", args...)
		var commandError *CommandError
		if !errors.As(err, &commandError) || got != "" {
			t.Fatal(args, got, err)
		}
	}
}
