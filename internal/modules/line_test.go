package modules

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"mmx/internal/value"
)

func TestEditLine(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, text, match, line, state, want, fails string
	}{
		{name: "replace keeps the other lines", text: "a=1\nb=1\nc=1\n", match: "^b=", line: "b=2", want: "a=1\nb=2\nc=1\n"},
		{name: "replace a commented default", text: "#Port 22\nX=1\n", match: "^#?Port ", line: "Port 2222", want: "Port 2222\nX=1\n"},
		{name: "unchanged", text: "a=1\nb=2\n", match: "^b=", line: "b=2", want: "a=1\nb=2\n"},
		{name: "append when nothing matches", text: "a=1\n", match: "^b=", line: "b=2", want: "a=1\nb=2\n"},
		{name: "append after a last line without newline", text: "a=1", match: "^b=", line: "b=2", want: "a=1\nb=2\n"},
		{name: "append to an empty file", text: "", match: "^b=", line: "b=2", want: "b=2\n"},
		{name: "last line keeps its missing newline", text: "a=1\nb=1", match: "^b=", line: "b=2", want: "a=1\nb=2"},
		{name: "exact line present", text: "a=1\nb=2\n", line: "b=2", want: "a=1\nb=2\n"},
		{name: "exact line appended", text: "a=1\n", line: "b=2", want: "a=1\nb=2\n"},
		{name: "absent by match", text: "a=1\nb=1\nc=1\n", match: "^b=", state: "absent", want: "a=1\nc=1\n"},
		{name: "absent by exact line", text: "a=1\nb=1\n", line: "a=1", state: "absent", want: "b=1\n"},
		{name: "absent with nothing selected", text: "a=1\n", match: "^b=", state: "absent", want: "a=1\n"},
		{name: "two selected lines fail", text: "b=1\na=1\nb=2\n", match: "^b=", line: "b=3", fails: "lines 1, 3 match the selection"},
		{name: "two exact lines fail", text: "a=1\na=1\n", line: "a=1", state: "absent", fails: "lines 1, 2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := EditLine(tc.text, Line{Path: "/etc/x", Match: tc.match, Text: tc.line, State: tc.state}, value.Scope{})
			if tc.fails != "" {
				if err == nil || !strings.Contains(err.Error(), tc.fails) {
					t.Fatal(got, err)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("%q, %v; want %q", got, err, tc.want)
			}
		})
	}
}

// A placing setting decides where a new line goes, and only when nothing is
// selected; a placing pattern that matches no line fails.
func TestEditLinePlacesANewLine(t *testing.T) {
	t.Parallel()
	text := "# head\n[main]\nkey=1\n[extra]\nkey=2\n# tail"
	for _, tc := range []struct {
		name, want, fails string
		place             func(*Line)
	}{
		{name: "end of file by default", place: func(*Line) {}, want: text + "\nnew=1\n"},
		{name: "end of file", place: func(l *Line) { l.InsertAtEndOfFile = true }, want: text + "\nnew=1\n"},
		{name: "start of file", place: func(l *Line) { l.InsertAtStartOfFile = true }, want: "new=1\n" + text},
		{name: "before first match", place: func(l *Line) { l.InsertBeforeFirstLineMatching = `^key=` }, want: "# head\n[main]\nnew=1\nkey=1\n[extra]\nkey=2\n# tail"},
		{name: "before last match", place: func(l *Line) { l.InsertBeforeLastLineMatching = `^key=` }, want: "# head\n[main]\nkey=1\n[extra]\nnew=1\nkey=2\n# tail"},
		{name: "after first match", place: func(l *Line) { l.InsertAfterFirstLineMatching = `^\[` }, want: "# head\n[main]\nnew=1\nkey=1\n[extra]\nkey=2\n# tail"},
		{name: "after last match", place: func(l *Line) { l.InsertAfterLastLineMatching = `^\[` }, want: "# head\n[main]\nkey=1\n[extra]\nnew=1\nkey=2\n# tail"},
		{name: "after the last line, which lacks its newline", place: func(l *Line) { l.InsertAfterLastLineMatching = `^# tail` }, want: text + "\nnew=1\n"},
		{name: "no matching line fails", place: func(l *Line) { l.InsertBeforeFirstLineMatching = `^\[missing\]` }, fails: "no line matches insert_before_first_line_matching"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := Line{Path: "/etc/x", Match: "^new=", Text: "new=1"}
			tc.place(&l)
			got, err := EditLine(text, l, value.Scope{})
			if tc.fails != "" {
				if err == nil || !strings.Contains(err.Error(), tc.fails) {
					t.Fatal(got, err)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("%q, %v; want %q", got, err, tc.want)
			}
			// The placed line is selected next time, so the edit is unchanged
			// even where the placing pattern would now put it elsewhere.
			if again, err := EditLine(got, l, value.Scope{}); err != nil || again != got {
				t.Fatalf("second edit %q, %v", again, err)
			}
		})
	}
	// A selected line is replaced where it is; placing never moves it.
	l := Line{Path: "/etc/x", Match: "^key=1", Text: "key=1", InsertAtStartOfFile: true}
	if got, err := EditLine(text, l, value.Scope{}); err != nil || got != text {
		t.Fatalf("%q, %v", got, err)
	}
	// An empty file takes the new line wherever a setting with no pattern puts it.
	l = Line{Path: "/etc/x", Text: "only", InsertAtStartOfFile: true}
	if got, err := EditLine("", l, value.Scope{}); err != nil || got != "only\n" {
		t.Fatalf("%q, %v", got, err)
	}
}

// A placing pattern that matches nothing fails the task and leaves the file.
func TestLinePlacementFailureChangesNothing(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "pg_hba.conf")
	if err := os.WriteFile(path, []byte("local all all peer\n"), 0640); err != nil {
		t.Fatal(err)
	}
	l := Line{Path: path, Text: "host app app 10.0.0.0/8 scram-sha-256", InsertBeforeFirstLineMatching: `^host\s+all\s+all\s`}
	for _, dryrun := range []bool{true, false} {
		if r := (Runtime{Dryrun: dryrun}).Line(context.Background(), l, value.Scope{}); r.Status != "failed" || !strings.Contains(r.Reason, "no line matches") {
			t.Fatal(dryrun, r)
		}
	}
	if read(t, path) != "local all all peer\n" {
		t.Fatal("changed the file")
	}
	if err := os.WriteFile(path, []byte("local all all peer\nhost all all 0.0.0.0/0 reject\n"), 0640); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"changed", "unchanged"} {
		if r := (Runtime{}).Line(context.Background(), l, value.Scope{}); r.Status != "succeeded" || r.Change != want {
			t.Fatal(want, r)
		}
	}
	if got := read(t, path); got != "local all all peer\nhost app app 10.0.0.0/8 scram-sha-256\nhost all all 0.0.0.0/0 reject\n" {
		t.Fatalf("%q", got)
	}
}

// line installs through the file module: a preview with a diff, candidate
// validation, and an atomic replacement that keeps the file's metadata.
func TestLineInstallsThroughTheFileInstaller(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "sshd_config")
	put := func(text string) {
		if err := os.WriteFile(path, []byte(text), 0644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, 0604); err != nil {
			t.Fatal(err)
		}
	}
	put("Port 22\nPasswordAuthentication yes\nX11Forwarding no\n")
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	l := Line{InsertAtEndOfFile: true, Path: path, Match: `^#?PasswordAuthentication\s`, Text: "PasswordAuthentication {{ .vars.auth }}"}
	s := value.Scope{"vars": map[string]any{"auth": "no"}}
	preview := (Runtime{Dryrun: true}).Line(context.Background(), l, s)
	if preview.Status != "succeeded" || preview.Change != "would_change" || preview.FileChange == nil ||
		!strings.Contains(preview.FileChange.Diff, "-PasswordAuthentication yes\n+PasswordAuthentication no\n") {
		t.Fatal(preview, preview.FileChange)
	}
	l.CheckNewFile = &Check{Shell: &Shell{Script: "exit 1"}}
	if r := (Runtime{}).Line(context.Background(), l, s); r.FailureKind != "validation" || read(t, path) != "Port 22\nPasswordAuthentication yes\nX11Forwarding no\n" {
		t.Fatal("rejected candidate replaced the file", r)
	}
	l.CheckNewFile = &Check{Shell: &Shell{Script: `grep -qx 'PasswordAuthentication no' "$C"`, Env: map[string]any{"C": "{{ .new_file.path }}"}}}
	for _, want := range []string{"changed", "unchanged"} {
		if r := (Runtime{}).Line(context.Background(), l, s); r.Status != "succeeded" || r.Change != want {
			t.Fatal(want, r)
		}
	}
	if got := read(t, path); got != "Port 22\nPasswordAuthentication no\nX11Forwarding no\n" {
		t.Fatalf("%q", got)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	was, is := before.Sys().(*syscall.Stat_t), after.Sys().(*syscall.Stat_t)
	if after.Mode().Perm() != 0604 || was.Uid != is.Uid || was.Gid != is.Gid {
		t.Fatal("metadata not kept", after.Mode(), is.Uid, is.Gid)
	}

	absent := Line{Path: path, Match: `^X11Forwarding\s`, State: "absent"}
	for _, want := range []string{"changed", "unchanged"} {
		if r := (Runtime{}).Line(context.Background(), absent, value.Scope{}); r.Status != "succeeded" || r.Change != want {
			t.Fatal(want, r)
		}
	}
	if got := read(t, path); got != "Port 22\nPasswordAuthentication no\n" {
		t.Fatalf("%q", got)
	}
}

// line edits only an existing regular file; dryrun leaves a missing one to an
// earlier task, such as the package that installs it.
func TestLineNeedsAnExistingRegularFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "directory"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "target"), []byte("a=1\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("target", filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"missing", "missing-dir/file", "directory", "link"} {
		l := Line{InsertAtEndOfFile: true, Path: filepath.Join(dir, name), Text: "a=2", Match: "^a="}
		if r := (Runtime{}).Line(context.Background(), l, value.Scope{}); r.Status != "failed" {
			t.Fatal(name, r)
		}
		r := (Runtime{Dryrun: true}).Line(context.Background(), l, value.Scope{})
		if strings.HasPrefix(name, "missing") != (r.Status == "not_checked") {
			t.Fatal(name, r)
		}
	}
	if read(t, filepath.Join(dir, "target")) != "a=1\n" {
		t.Fatal("followed the link")
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestLineRequiresPlacementForPresentState(t *testing.T) {
	for _, l := range []Line{{Text: "value"}, {Text: "value", Match: "^value$"}} {
		if err := ValidateLine(l); err == nil || !strings.Contains(err.Error(), "insert_at_end_of_file: true") {
			t.Fatal(l, err)
		}
		l.InsertAtEndOfFile = true
		if err := ValidateLine(l); err != nil {
			t.Fatal(err)
		}
	}
	if err := ValidateLine(Line{Match: "^value$", State: "absent"}); err != nil {
		t.Fatal(err)
	}
}
