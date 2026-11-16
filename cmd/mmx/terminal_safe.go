package main

import (
	"fmt"
	"io"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Text that comes from a node, or from a web server a node task called, can
// hold terminal escape sequences: they move the cursor, erase lines, set the
// window title, write the clipboard, or make hyperlinks. Human output is
// written to a terminal, so every such text passes through
// escapeControlCharacters before a person can see it. --json output is not
// changed: JSON already escapes C0 control characters, and agents read it.

// escapeControlCharacters returns s with every control character shown as a
// visible escape, in Go's strconv quoting style (\x1b, \r, \x7f, \u009b), and
// every byte that is not valid UTF-8 shown as \x.. . Newline and tab stay, so
// multi-line output keeps its layout. The C0 control characters (U+0000 to
// U+001F), DEL (U+007F), and the C1 control characters (U+0080 to U+009F) are
// the characters a terminal acts on.
func escapeControlCharacters(s string) string {
	clean := true
	for i := 0; i < len(s) && clean; {
		r, size := utf8.DecodeRuneInString(s[i:])
		clean = !(r == utf8.RuneError && size == 1) && !isTerminalControl(r)
		i += size
	}
	if clean {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == utf8.RuneError && size == 1:
			fmt.Fprintf(&b, `\x%02x`, s[i])
		case isTerminalControl(r):
			quoted := strconv.QuoteRune(r)
			b.WriteString(quoted[1 : len(quoted)-1])
		default:
			b.WriteString(s[i : i+size])
		}
		i += size
	}
	return b.String()
}

func isTerminalControl(r rune) bool {
	return r != '\n' && r != '\t' && (r < 0x20 || r >= 0x7f && r <= 0x9f)
}

// terminalSafeWriter escapes everything written through it. Each Write holds
// whole texts, as fmt produces them; a stream that can split a character
// between writes goes through copyEscaped instead.
type terminalSafeWriter struct{ w io.Writer }

func terminalSafe(w io.Writer) io.Writer { return terminalSafeWriter{w} }

func (t terminalSafeWriter) Write(p []byte) (int, error) {
	if _, err := io.WriteString(t.w, escapeControlCharacters(string(p))); err != nil {
		return 0, err
	}
	return len(p), nil
}

// copyEscaped copies r to w with control characters escaped. It holds back
// an incomplete character at the end of a chunk until the next chunk
// completes it.
func copyEscaped(w io.Writer, r io.Reader) error {
	buffer := make([]byte, 32*1024)
	carry := 0
	for {
		n, readErr := r.Read(buffer[carry:])
		data := buffer[:carry+n]
		keep := 0
		if readErr == nil {
			// The last character starts within the final utf8.UTFMax-1 bytes.
			for i := 1; i < utf8.UTFMax && i <= len(data); i++ {
				if utf8.RuneStart(data[len(data)-i]) {
					if !utf8.FullRune(data[len(data)-i:]) {
						keep = i
					}
					break
				}
			}
		}
		if _, err := io.WriteString(w, escapeControlCharacters(string(data[:len(data)-keep]))); err != nil {
			return err
		}
		copy(buffer, data[len(data)-keep:])
		carry = keep
		if readErr == io.EOF {
			return nil
		}
		if readErr != nil {
			return readErr
		}
	}
}

// printError writes an error to w as mmx prints errors. An error can quote
// text from a node, such as ssh's output.
func printError(w io.Writer, err error) {
	fmt.Fprintln(w, "mmx:", escapeControlCharacters(err.Error()))
}
