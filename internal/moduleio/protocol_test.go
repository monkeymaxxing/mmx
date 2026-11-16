package moduleio

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestRequestPreservesTypes(t *testing.T) {
	r, err := ReadRequest(strings.NewReader(`{"protocol":1,"mode":"apply","inputs":{"number":9007199254740993,"nothing":null,"off":false,"empty":"","list":[],"object":{}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if r.Inputs["number"] != json.Number("9007199254740993") || r.Inputs["nothing"] != nil || r.Inputs["off"] != false || r.Inputs["empty"] != "" {
		t.Fatalf("lost types: %#v", r.Inputs)
	}
	if _, ok := r.Inputs["list"].([]any); !ok {
		t.Fatal("lost empty array")
	}
	if _, ok := r.Inputs["object"].(map[string]any); !ok {
		t.Fatal("lost empty object")
	}
}

func TestInvalidRequests(t *testing.T) {
	for _, input := range []string{
		``, `null`, `[]`, `{`, `{}`, `{"protocol":1,"mode":"apply"}`,
		`{"protocol":2,"mode":"apply","inputs":{}}`,
		`{"protocol":"1","mode":"apply","inputs":{}}`,
		`{"protocol":1.0,"mode":"apply","inputs":{}}`,
		`{"protocol":1,"mode":"run","inputs":{}}`,
		`{"protocol":1,"mode":"apply","inputs":null}`,
		`{"protocol":1,"mode":"apply","inputs":[]}`,
		`{"protocol":1,"mode":"apply","inputs":{},"private-token":true}`,
		`{"protocol":1,"mode":"apply","inputs":{"private-token":1,"private-token":2}}`,
		`{"protocol":1,"mode":"apply","inputs":{"x":[{"x":1,"x":2}]}}`,
		`{"protocol":1,"mode":"apply","inputs":{}} {}`,
		`{"protocol":1,"mode":"apply","inputs":{}} private-token`,
		"{\"protocol\":1,\"mode\":\"apply\",\"inputs\":{\"x\":\"\xff\"}}",
		`{"protocol":1,"mode":"apply","inputs":{"x":` + strings.Repeat("[", 65) + `0` + strings.Repeat("]", 65) + `}}`,
	} {
		_, err := ReadRequest(strings.NewReader(input))
		if err == nil || strings.Contains(err.Error(), "private-token") {
			t.Errorf("expected private validation error for %q: %v", input, err)
		}
	}
}

func TestRequestSizeBoundary(t *testing.T) {
	base := `{"protocol":1,"mode":"check","inputs":{}}`
	exact := base + strings.Repeat(" ", MaxRequestBytes-len(base))
	if _, err := ReadRequest(strings.NewReader(exact)); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadRequest(strings.NewReader(exact + " ")); err == nil {
		t.Fatal("accepted oversized request")
	}
}

func TestResultContract(t *testing.T) {
	for _, tc := range []struct {
		mode, status, change, reason string
		valid                        bool
	}{
		{"apply", "succeeded", "changed", "", true},
		{"apply", "succeeded", "unchanged", "", true},
		{"check", "succeeded", "would_change", "", true},
		{"check", "succeeded", "unchanged", "", true},
		{"check", "not_checked", "unknown", "install prerequisite", true},
		{"apply", "failed", "changed", "partial failure", true},
		{"apply", "failed", "unknown", "interrupted", true},
		{"check", "failed", "would_change", "later inspection failed", true},
		{"check", "succeeded", "changed", "", false},
		{"apply", "succeeded", "would_change", "", false},
		{"apply", "succeeded", "unknown", "", false},
		{"check", "not_checked", "unknown", "", false},
		{"check", "not_checked", "unchanged", "reason", false},
		{"apply", "not_checked", "unknown", "reason", false},
		{"apply", "running", "unchanged", "", false},
		{"apply", "failed", "changed", "", false},
		{"check", "failed", "unknown", "", false},
		{"apply", "failed", "bogus", "", false},
		{"bogus", "failed", "unknown", "", false},
	} {
		t.Run(tc.mode+"/"+tc.status+"/"+tc.change+"/"+tc.reason, func(t *testing.T) {
			var output bytes.Buffer
			r := Result{Protocol: 1, Status: tc.status, Change: tc.change, Reason: tc.reason}
			err := WriteResult(&output, tc.mode, r)
			if (err == nil) != tc.valid {
				t.Fatalf("valid %v: %v", tc.valid, err)
			}
			if !tc.valid {
				if output.Len() != 0 {
					t.Fatal("invalid result wrote output")
				}
				return
			}
			read, err := ReadResult(&output, tc.mode, 0)
			if err != nil || read.Status != r.Status || read.Change != r.Change || read.Reason != r.Reason {
				t.Fatalf("round trip: %+v %v", read, err)
			}
		})
	}
}

func TestResultExitAndPartialChange(t *testing.T) {
	for _, status := range []string{"succeeded", "failed"} {
		data := `{"protocol":1,"status":"` + status + `","change":"changed","reason":"second write failed","outputs":{"count":9007199254740993},"secret":true}`
		result, err := ReadResult(strings.NewReader(data), "apply", 9)
		if err != nil || result.Status != "failed" || result.Change != "changed" || !result.Secret || string(result.Outputs) != `{"count":9007199254740993}` {
			t.Fatalf("lost partial result: %+v %v", result, err)
		}
		if status == "failed" && result.Reason != "second write failed" {
			t.Fatal("lost explicit failure reason")
		}
		if status == "succeeded" && !strings.Contains(result.Reason, "status 9") {
			t.Fatal("success hid exit failure")
		}
	}
	result, err := ReadResult(strings.NewReader(`{"protocol":1,"status":"not_checked","change":"unknown","reason":"missing tool"}`), "check", 1)
	if err != nil || result.Status != "failed" || result.Change != "unknown" {
		t.Fatalf("check exit failure: %+v %v", result, err)
	}
}

func TestInvalidResultFrames(t *testing.T) {
	base := `{"protocol":1,"status":"succeeded","change":"unchanged"}`
	for _, input := range []string{
		``, `[]`, `{}`, base + base, base + "log line",
		strings.Replace(base, `"protocol":1`, `"protocol":2`, 1),
		strings.Replace(base, `"succeeded"`, `true`, 1),
		strings.Replace(base, `"unchanged"`, `null`, 1),
		strings.Replace(base, `"succeeded"`, `"failed"`, 1),
		strings.Replace(base, `"succeeded"`, `"failed"`, 1) + `,"reason":""}`,
		strings.TrimSuffix(base, "}") + `,"reason":null}`,
		strings.TrimSuffix(base, "}") + `,"secret":"true"}`,
		strings.TrimSuffix(base, "}") + `,"id":"private-token"}`,
		strings.TrimSuffix(base, "}") + `,"failure_kind":"config"}`,
		strings.TrimSuffix(base, "}") + `,"status":"failed"}`,
		strings.TrimSuffix(base, "}") + `,"value":{"x":1,"x":2}}`,
		base + strings.Repeat(" ", MaxResultBytes),
	} {
		result, err := ReadResult(strings.NewReader(input), "apply", 0)
		if err == nil || result.Change != "" || strings.Contains(err.Error(), "private-token") {
			t.Fatalf("invalid result accepted or implied unchanged: %+v %v", result, err)
		}
	}
}

func TestOutputsMustBeAnObjectAndOutputLimit(t *testing.T) {
	base := `{"protocol":1,"status":"succeeded","change":"unchanged"`
	for _, suffix := range []string{`,"outputs":null}`, `,"outputs":[1]}`, `,"diff":3}`, `,"value":1}`} {
		if _, err := ReadResult(strings.NewReader(base+suffix), "apply", 0); err == nil {
			t.Fatalf("accepted %s", suffix)
		}
	}
	r := Result{Protocol: 1, Status: "succeeded", Change: "unchanged", Reason: "x"}
	var output bytes.Buffer
	if err := WriteResult(&output, "apply", r); err != nil {
		t.Fatal(err)
	}
	r.Reason = strings.Repeat("x", MaxResultBytes-output.Len()+1)
	output.Reset()
	if err := WriteResult(&output, "apply", r); err != nil || output.Len() != MaxResultBytes {
		t.Fatalf("exact boundary: %d %v", output.Len(), err)
	}
	if _, err := ReadResult(&output, "apply", 0); err != nil {
		t.Fatal(err)
	}
	r.Reason += "x"
	output.Reset()
	if err := WriteResult(&output, "apply", r); err == nil || output.Len() != 0 {
		t.Fatal("oversized result wrote output")
	}
}

type brokenReader struct{}

func (brokenReader) Read([]byte) (int, error) { return 0, errors.New("private-token") }

func TestIOErrorDoesNotExposeData(t *testing.T) {
	_, err := ReadRequest(brokenReader{})
	if err == nil || strings.Contains(err.Error(), "private-token") {
		t.Fatal(err)
	}
	if err := WriteResult(io.Discard, "apply", Result{Protocol: 1, Status: "failed", Change: "unknown", Outputs: json.RawMessage(`{"x":1,"x":2}`)}); err == nil {
		t.Fatal("accepted ambiguous outputs")
	}
}
