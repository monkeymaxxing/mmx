package value

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
)

// Binary is content that is not UTF-8 text, such as a file read_file read.
// It passes on whole: into a file's content, an output, a role input, or a
// file saved on the controller machine. A reference inside other text and a
// template refuse it, since text cannot hold it. Saved and transported JSON
// carries it as base64.
type Binary struct{ Bytes []byte }

func (b Binary) MarshalJSON() ([]byte, error) {
	return json.Marshal(map[string]string{"$mmx_binary": base64.StdEncoding.EncodeToString(b.Bytes)})
}

// restoreBinary reads the record Binary.MarshalJSON writes.
func restoreBinary(x map[string]any) (Binary, bool) {
	text, ok := x["$mmx_binary"].(string)
	if !ok || len(x) != 1 {
		return Binary{}, false
	}
	b, err := base64.StdEncoding.DecodeString(text)
	if err != nil {
		return Binary{}, false
	}
	return Binary{b}, true
}

func HasBinary(v any) bool {
	switch x := v.(type) {
	case Binary:
		return true
	case *Secret:
		return HasBinary(x.Data) //nolint:nilaway // The type switch dereferences only allocated *Secret values; missing lookups return nil interfaces.
	case map[string]any:
		for _, child := range x {
			if HasBinary(child) {
				return true
			}
		}
	case []any:
		for _, child := range x {
			if HasBinary(child) {
				return true
			}
		}
	}
	return false
}

// binaryInText is the error for binary content used inside other text.
func binaryInText(path string) error {
	return fmt.Errorf("%s contains binary content rather than UTF-8 text. Binary content requires a reference with no surrounding text, such as content: \"{{ %s }}\"", path, path)
}
