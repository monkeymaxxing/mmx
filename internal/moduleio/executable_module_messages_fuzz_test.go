package moduleio

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// syntheticPrivateValue stands for a private request or result value in the
// seeds, as in the protocol tests. No error from reading a message may quote it.
const syntheticPrivateValue = "private-token"

// FuzzExecutableModuleMessages feeds the bytes of one untrusted JSON message
// to the readers of an executable module's request and result. A request that
// reads has protocol 1, mode check or apply, and an inputs object, and encodes
// back to the same values. A result that reads survives WriteResult and a second
// ReadResult unchanged, and a failing exit always reads as failed. A result
// error never implies a change, no error quotes a value, and a second JSON
// value after a message that reads must be refused.
func FuzzExecutableModuleMessages(f *testing.F) {
	f.Add([]byte(`{"protocol":1,"mode":"check","inputs":{}}`))
	f.Add([]byte(`{"protocol":1,"mode":"apply","inputs":{"number":9007199254740993,"nothing":null,"off":false,"empty":"","list":[],"object":{}}}`))
	f.Add([]byte(`{"protocol":1,"mode":"check","inputs":{"path":"/etc/motd","huge":1e400,"html":"<b>&</b>","nested":{"list":[1,"two",{"three":3}]}}}`))
	f.Add([]byte(`{"protocol":1,"status":"succeeded","change":"would_change","diff":"+line\n","fingerprint":"sha256:abc","outputs":{"count":9007199254740993,"path":"/etc/motd"}}`))
	f.Add([]byte(`{"protocol":1,"status":"succeeded","change":"changed","outputs":{"html":"<b>&</b>","empty":{}},"secret":true}`))
	f.Add([]byte(`{"protocol":1,"status":"succeeded","change":"unchanged"}`))
	f.Add([]byte(`{"protocol":1,"status":"failed","change":"changed","reason":"second write failed","outputs":{"count":9007199254740993},"secret":true}`))
	f.Add([]byte(`{"protocol":1,"status":"failed","change":"unknown","reason":"interrupted"}`))
	f.Add([]byte(`{"protocol":1,"status":"not_checked","change":"unknown","reason":"missing tool"}`))
	f.Add([]byte(`{"protocol":1,"mode":"apply","inputs":`))
	f.Add([]byte(`{"protocol":1,"status":"succeeded","change":"unchanged"`))
	f.Add([]byte(`{"protocol":1,"mode":"apply","inputs":{"` + syntheticPrivateValue + `":1,"` + syntheticPrivateValue + `":2}}`))
	f.Add([]byte(`{"protocol":1,"status":"succeeded","change":"unchanged","outputs":{"x":1,"x":2}}`))
	f.Add([]byte(`{"protocol":1,"mode":"apply","inputs":{},"` + syntheticPrivateValue + `":true}`))
	f.Add([]byte(`{"protocol":1,"status":"succeeded","change":"unchanged","id":"` + syntheticPrivateValue + `"}`))
	f.Add([]byte(`{"protocol":1,"mode":"apply","inputs":{}} ` + syntheticPrivateValue))
	f.Add([]byte(`{"protocol":1,"status":"succeeded","change":"unchanged"}{"protocol":1,"status":"succeeded","change":"unchanged"}`))
	f.Add([]byte("{\"protocol\":1,\"mode\":\"apply\",\"inputs\":{\"x\":\"\xff\"}}"))
	f.Add([]byte("{\"protocol\":1,\"status\":\"failed\",\"change\":\"unknown\",\"reason\":\"\xff\"}"))
	// The reader refuses JSON nested deeper than 64 levels. The inputs object
	// is level 1 and the value of "x" level 2, so 62 brackets reach level 64.
	f.Add([]byte(`{"protocol":1,"mode":"apply","inputs":{"x":` + strings.Repeat("[", 62) + `0` + strings.Repeat("]", 62) + `}}`))
	f.Add([]byte(`{"protocol":1,"mode":"apply","inputs":{"x":` + strings.Repeat("[", 63) + `0` + strings.Repeat("]", 63) + `}}`))
	f.Fuzz(func(t *testing.T, moduleMessage []byte) {
		withSecondJSONValue := append(bytes.Clone(moduleMessage), "\n{}"...)

		request, err := ReadRequest(bytes.NewReader(moduleMessage))
		if err != nil && strings.Contains(err.Error(), syntheticPrivateValue) {
			t.Fatalf("the request error quotes a private value: %v", err)
		}
		if err == nil {
			if len(moduleMessage) > MaxRequestBytes {
				t.Fatalf("read a request of %d bytes over the %d byte limit", len(moduleMessage), MaxRequestBytes)
			}
			if request.Protocol != 1 || (request.Mode != "check" && request.Mode != "apply") || request.Inputs == nil {
				t.Fatalf("read an invalid request: %+v", request)
			}
			if _, err := ReadRequest(bytes.NewReader(withSecondJSONValue)); err == nil {
				t.Fatal("accepted a request followed by a second JSON value")
			}
			encodedRequest, err := json.Marshal(request)
			if err != nil {
				t.Fatalf("cannot encode a request that read without error: %v", err)
			}
			if len(encodedRequest) <= MaxRequestBytes {
				rereadRequest, err := ReadRequest(bytes.NewReader(encodedRequest))
				if err != nil {
					t.Fatalf("cannot read back an encoded request: %v", err)
				}
				if !reflect.DeepEqual(rereadRequest, request) {
					t.Fatalf("the encoded request read back as %+v, not %+v", rereadRequest, request)
				}
			}
		}

		for _, mode := range []string{"check", "apply"} {
			for _, exitCode := range []int{0, 9} {
				result, err := ReadResult(bytes.NewReader(moduleMessage), mode, exitCode)
				if err != nil {
					if result.Change != "" || strings.Contains(err.Error(), syntheticPrivateValue) {
						t.Fatalf("the result error %v came with change %q or quotes a private value", err, result.Change)
					}
					continue
				}
				if len(moduleMessage) > MaxResultBytes {
					t.Fatalf("read a result of %d bytes over the %d byte limit", len(moduleMessage), MaxResultBytes)
				}
				if exitCode != 0 && result.Status != "failed" {
					t.Fatalf("exit status %d read as status %q", exitCode, result.Status)
				}
				secondValueResult, err := ReadResult(bytes.NewReader(withSecondJSONValue), mode, exitCode)
				if err == nil || secondValueResult.Change != "" {
					t.Fatal("accepted a result followed by a second JSON value")
				}

				encodedResult, err := json.Marshal(result)
				if err != nil {
					t.Fatalf("cannot encode a result that read without error: %v", err)
				}
				var writtenResult bytes.Buffer
				err = WriteResult(&writtenResult, mode, result)
				if len(encodedResult)+1 > MaxResultBytes {
					if err == nil || writtenResult.Len() != 0 {
						t.Fatalf("wrote a result of %d bytes over the %d byte limit", len(encodedResult)+1, MaxResultBytes)
					}
					continue
				}
				if err != nil {
					t.Fatalf("cannot write a result that read without error: %v", err)
				}
				rereadResult, err := ReadResult(&writtenResult, mode, 0)
				if err != nil {
					t.Fatalf("cannot read back a written result: %v", err)
				}
				if !reflect.DeepEqual(decodedOutputs(t, rereadResult.Outputs), decodedOutputs(t, result.Outputs)) {
					t.Fatalf("the outputs %s read back as %s", result.Outputs, rereadResult.Outputs)
				}
				rereadResult.Outputs, result.Outputs = nil, nil
				if !reflect.DeepEqual(rereadResult, result) {
					t.Fatalf("the written result read back as %+v, not %+v", rereadResult, result)
				}
			}
		}
	})
}

// decodedOutputs decodes a result's outputs with numbers kept as written, so
// two encodings of the same outputs compare equal.
func decodedOutputs(t *testing.T, outputs json.RawMessage) any {
	if len(outputs) == 0 {
		return nil
	}
	value, err := parseJSON(outputs)
	if err != nil {
		t.Fatalf("cannot decode outputs %s: %v", outputs, err)
	}
	return value
}
