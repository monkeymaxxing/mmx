package value

import (
	"fmt"
	"strings"
)

// Fleet stage outputs come from other nodes. A node task on this node that
// puts such a value inside other text, such as a line of a WireGuard or
// ifupdown file or a command, must not let the value add lines of its own: a
// "PostUp =" or "up" line runs as root here. So on the node, text that comes
// from .outputs and holds a line break or another control character before
// its end is inserted into other text nowhere: not through .outputs itself,
// and not after a role input, a loop item, or a variable passed it on whole.
// The whole value as the whole input, such as content: "{{ .outputs.ca.util01.certificate }}",
// stays as it is. The controller process does not set this guard: a file it
// writes on the controller machine runs nothing.

const outputLinesKey = "\x00output_text_with_lines"

// GuardOutputLines records, in a node job's scope, the text in its .outputs
// that may not be inserted into other text.
func (s Scope) GuardOutputLines() {
	lines := map[string]bool{}
	var collect func(any)
	collect = func(v any) {
		switch x := v.(type) {
		case string:
			if addsLines(x) {
				lines[x] = true
			}
		case Truncated:
			collect(x.Text)
		case *Secret:
			collect(x.Data)
		case map[string]any:
			for _, child := range x {
				collect(child)
			}
		case []any:
			for _, child := range x {
				collect(child)
			}
		}
	}
	collect(s["outputs"])
	if len(lines) > 0 {
		s[outputLinesKey] = lines
	}
}

// InheritOutputLines keeps the parent's guard in a role call's own scope,
// which reads outputs only through its inputs.
func (s Scope) InheritOutputLines(parent Scope) {
	if lines, ok := parent[outputLinesKey]; ok {
		s[outputLinesKey] = lines
	}
}

// addsLines reports a control character other than a tab before the end of
// text. Line breaks at the very end, such as a command's last newline, add
// no line of their own.
func addsLines(text string) bool {
	return strings.IndexFunc(strings.TrimRight(text, "\r\n"), func(r rune) bool {
		return r != '\t' && (r < 0x20 || r == 0x7f)
	}) >= 0
}

// checkOutputLines refuses a read of path, inside other text, whose value
// holds output text that would add lines. The error never quotes the value,
// which may be secret.
func (s Scope) checkOutputLines(path string, v any) error {
	lines, _ := s[outputLinesKey].(map[string]bool)
	if len(lines) == 0 || !holdsAny(v, lines) {
		return nil
	}
	return fmt.Errorf("%s holds a fleet stage output from a node with a line break or another control character, and MMX does not insert such a value inside other text, where it could add lines to a configuration file or a command. Give a multi-line output, such as a certificate, a reference with no surrounding text, such as content: \"{{ %s }}\"; in a template, read only the fields it prints", path, path)
}

func holdsAny(v any, lines map[string]bool) bool {
	switch x := v.(type) {
	case string:
		return lines[x]
	case Truncated:
		return lines[x.Text]
	case *Secret:
		return holdsAny(x.Data, lines)
	case map[string]any:
		for _, child := range x {
			if holdsAny(child, lines) {
				return true
			}
		}
	case []any:
		for _, child := range x {
			if holdsAny(child, lines) {
				return true
			}
		}
	}
	return false
}
