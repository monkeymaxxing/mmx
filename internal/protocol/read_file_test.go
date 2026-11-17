package protocol

import (
	"strings"
	"testing"

	"mmx/internal/value"
)

// A read-file answer holds a file's content: text or binary content, never a
// record that names a secret or another node's file.
func TestReadFileAnswerHoldsOnlyFileContent(t *testing.T) {
	for answer, want := range map[string]any{
		`{"content":"join-token"}`:           "join-token",
		`{"content":{"$mmx_binary":"AAEC"}}`: value.Binary{Bytes: []byte{0, 1, 2}},
	} {
		var r ReadFileResponse
		if err := Decode([]byte(answer), &r); err != nil || value.Display(r.Content) != value.Display(want) {
			t.Error(answer, r.Content, err)
		}
	}
	for _, answer := range []string{`{"content":{"$mmx_secret":{"name":"root_password"}}}`, `{"content":{"a":"b"}}`, `{"content":null}`} {
		var r ReadFileResponse
		if err := Decode([]byte(answer), &r); err == nil || !strings.Contains(err.Error(), "neither text nor binary content") {
			t.Error(answer, err)
		}
	}
}
