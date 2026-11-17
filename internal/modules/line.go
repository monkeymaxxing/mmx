package modules

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"

	"mmx/internal/protocol"
	"mmx/internal/value"
)

// Line edits one line of an existing regular file. It installs the edited text
// through fileAt, so the diff, check_new_file, and atomic replacement
// are the file module's, and the file keeps its owner, group, and mode.
func (run Runtime) Line(ctx context.Context, l Line, s value.Scope) protocol.Result {
	if err := ValidateLine(l); err != nil {
		return protocol.ConfigurationError(err)
	}
	// An earlier task, such as a package, may create the file.
	missing := fmt.Errorf("line edits an existing file; %s does not exist", l.Path)
	parent, err := os.OpenRoot(filepath.Dir(l.Path))
	if os.IsNotExist(err) {
		return run.notYet(missing)
	}
	if err != nil {
		return protocol.Failed(err)
	}
	defer parent.Close()
	name := filepath.Base(l.Path)
	info, err := entryInfoAt(parent, name, false)
	if err != nil {
		return protocol.Failed(err)
	}
	if info == nil {
		return run.notYet(missing)
	}
	current, err := readTextAt(parent, name)
	if err != nil {
		return protocol.Failed(err)
	}
	if s == nil {
		s = value.Scope{}
	}
	lineScope, trace := s.TraceReads() // Only the line decides the file's read permission.
	next, err := EditLine(current, l, lineScope)
	if err != nil {
		return protocol.Failed(err)
	}
	if trace.Used {
		if err := checkSecretInKeptMode("line", l.Path, info.Mode(), l.SecretReadableByGroup, l.DangerouslyLetEveryAccountReadSecret); err != nil {
			return protocol.ConfigurationError(err)
		}
	}
	return run.fileAt(ctx, File{Path: l.Path, Literal: &next, CheckNewFile: l.CheckNewFile}, s, parent, 0644, nil)
}

// checkSecretInKeptMode applies file's rule for content that holds a secret
// to line and edit_file, which keep the existing file's mode: other accounts
// may read the secret only when the task says so, with the acknowledgement
// that file asks for.
func checkSecretInKeptMode(module, path string, mode os.FileMode, secretReadableByGroup, dangerouslyLetEveryAccountReadSecret bool) error {
	if mode&0004 != 0 && !dangerouslyLetEveryAccountReadSecret {
		return invalid("dangerously_let_every_account_read_secret", fmt.Sprintf("%s: %s would hold a secret, and the file's mode %04o lets every account on the node read it. Make the file readable only by its owner first, such as with a file task that sets permissions_mode \"0600\", or, if every account should read it, add dangerously_let_every_account_read_secret: true.", module, path, mode.Perm()))
	}
	if mode&0004 == 0 && mode&0040 != 0 && !secretReadableByGroup {
		return invalid("secret_readable_by_group", fmt.Sprintf("%s: %s would hold a secret, and the file's mode %04o lets the file's group read it. If the group should read it, add secret_readable_by_group: true; otherwise make the file readable only by its owner first, such as with a file task that sets permissions_mode \"0600\".", module, path, mode.Perm()))
	}
	return nil
}

// EditLine returns text with l applied: the one selected line replaced by
// l's line or removed, or l's line inserted where l places it when nothing is
// selected. Its arguments arrive checked, so match and the placing patterns
// compile and match selects l's line.
func EditLine(text string, l Line, s value.Scope) (string, error) {
	want := ""
	if l.Text != "" {
		var err error
		if want, err = s.Text(l.Text); err != nil {
			return "", err
		}
	}
	selects := func(line string) bool { return line == want }
	if l.Match != "" {
		pattern, err := regexp.Compile(l.Match)
		if err != nil {
			return "", err
		}
		selects = pattern.MatchString
	}
	lines := strings.SplitAfter(text, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	var selected []int
	var numbers []string
	for i, line := range lines {
		if selects(strings.TrimSuffix(line, "\n")) {
			selected = append(selected, i)
			numbers = append(numbers, fmt.Sprint(i+1))
		}
	}
	absent := l.State == "absent"
	switch {
	case len(selected) > 1:
		return "", fmt.Errorf("%s: lines %s match the selection; the line module allows at most one matching line", l.Path, strings.Join(numbers, ", "))
	case len(selected) == 1 && absent:
		lines = slices.Delete(lines, selected[0], selected[0]+1)
	case len(selected) == 1:
		i := selected[0]
		if strings.HasSuffix(lines[i], "\n") {
			want += "\n"
		}
		lines[i] = want
	case !absent:
		at, err := insertionPoint(lines, l)
		if err != nil {
			return "", err
		}
		// Only the last line can lack its newline; a line after it needs one.
		if at > 0 && !strings.HasSuffix(lines[at-1], "\n") {
			lines[at-1] += "\n"
		}
		lines = slices.Insert(lines, at, want+"\n")
	}
	return strings.Join(lines, ""), nil
}

// insertionPoint is the index in lines where l's new line goes: the end of
// the file with insert_at_end_of_file, or where another setting places it. A placing pattern that
// matches no line is an error, so a line never lands where the author did not
// choose.
func insertionPoint(lines []string, l Line) (int, error) {
	var field, pattern string
	var last, after bool
	switch {
	case l.InsertAtStartOfFile:
		return 0, nil
	case l.InsertBeforeFirstLineMatching != "":
		field, pattern = "insert_before_first_line_matching", l.InsertBeforeFirstLineMatching
	case l.InsertBeforeLastLineMatching != "":
		field, pattern, last = "insert_before_last_line_matching", l.InsertBeforeLastLineMatching, true
	case l.InsertAfterFirstLineMatching != "":
		field, pattern, after = "insert_after_first_line_matching", l.InsertAfterFirstLineMatching, true
	case l.InsertAfterLastLineMatching != "":
		field, pattern, last, after = "insert_after_last_line_matching", l.InsertAfterLastLineMatching, true, true
	default:
		return len(lines), nil
	}
	compiled, err := regexp.Compile(pattern)
	if err != nil {
		return 0, err
	}
	found := -1
	for i, line := range lines {
		if compiled.MatchString(strings.TrimSuffix(line, "\n")) {
			found = i
			if !last {
				break
			}
		}
	}
	if found < 0 {
		return 0, fmt.Errorf("%s: no line matches %s %q, so MMX cannot determine where to insert the new line; no file was changed", l.Path, field, pattern)
	}
	if after {
		found++
	}
	return found, nil
}

// Line edits one line of an existing file. Match is a Go regular expression
// that selects the line; without it, the exact Text does. Text is private,
// like file content. Exactly one of the Insert settings says where a new
// line goes when nothing is selected, unless the line is being removed.
type Line struct {
	Path                          string `yaml:"path" json:"path"`
	Match                         string `yaml:"match,omitempty" json:"match,omitempty"`
	Text                          string `yaml:"line,omitempty" json:"line,omitempty"`
	State                         string `yaml:"state,omitempty" json:"state,omitempty"`
	CheckNewFile                  *Check `yaml:"check_new_file,omitempty" json:"check_new_file,omitempty"`
	InsertAtEndOfFile             bool   `yaml:"insert_at_end_of_file,omitempty" json:"insert_at_end_of_file,omitempty"`
	InsertAtStartOfFile           bool   `yaml:"insert_at_start_of_file,omitempty" json:"insert_at_start_of_file,omitempty"`
	InsertBeforeFirstLineMatching string `yaml:"insert_before_first_line_matching,omitempty" json:"insert_before_first_line_matching,omitempty"`
	InsertBeforeLastLineMatching  string `yaml:"insert_before_last_line_matching,omitempty" json:"insert_before_last_line_matching,omitempty"`
	InsertAfterFirstLineMatching  string `yaml:"insert_after_first_line_matching,omitempty" json:"insert_after_first_line_matching,omitempty"`
	InsertAfterLastLineMatching   string `yaml:"insert_after_last_line_matching,omitempty" json:"insert_after_last_line_matching,omitempty"`
	// The acknowledgements file asks for, for a line that holds a secret in a
	// file that other accounts can read (checkSecretInKeptMode).
	SecretReadableByGroup                bool `yaml:"secret_readable_by_group,omitempty" json:"secret_readable_by_group,omitempty"`
	DangerouslyLetEveryAccountReadSecret bool `yaml:"dangerously_let_every_account_read_secret,omitempty" json:"dangerously_let_every_account_read_secret,omitempty"`
}

// linePositions are the line settings that place a new line, by field name.
func linePositions(l Line) map[string]bool {
	return map[string]bool{
		"insert_at_end_of_file":             l.InsertAtEndOfFile,
		"insert_at_start_of_file":           l.InsertAtStartOfFile,
		"insert_before_first_line_matching": l.InsertBeforeFirstLineMatching != "",
		"insert_before_last_line_matching":  l.InsertBeforeLastLineMatching != "",
		"insert_after_first_line_matching":  l.InsertAfterFirstLineMatching != "",
		"insert_after_last_line_matching":   l.InsertAfterLastLineMatching != "",
	}
}

// ValidateLine checks a line task's fields. The line's own rules (one line,
// selected by match) belong to its argument rule.
func ValidateLine(l Line) error {
	if l.State != "" && !slices.Contains(presenceStates, l.State) {
		return invalid("state", "line.state must be present or absent")
	}
	if l.State == "absent" {
		if (l.Match == "") == (l.Text == "") {
			return fmt.Errorf("line with state: absent requires exactly one of match or line")
		}
	} else if l.Text == "" {
		return invalid("line", "line.line is required unless state: absent")
	}
	var positions []string
	for field, set := range linePositions(l) {
		if set {
			positions = append(positions, field)
		}
	}
	slices.Sort(positions)
	if len(positions) > 0 && l.State == "absent" {
		return invalid(positions[0], "line."+positions[0]+" places a new line; state: absent adds none")
	}
	if len(positions) > 1 {
		return invalid(positions[1], "line takes at most one of the settings that place a new line; this task has "+strings.Join(positions, " and "))
	}
	if len(positions) == 0 && l.State != "absent" {
		return invalid("insert_at_end_of_file", "line requires one placement setting with state: present; add insert_at_end_of_file: true to put a new line at the end of the file")
	}
	return validateCheck(l.CheckNewFile)
}

// lineDescription is what mmx describe-module shows for line.
var lineDescription = Description{
	Name: "line", Topic: TopicFilesAndLinks, Schema: reflect.TypeOf(Line{}),
	Summary: "one line of an existing file, selected by a regular expression or its exact text; prefer a drop-in or whole file where the program has one",
	Fields: []Field{
		{Name: "path", Required: true, Notes: "absolute; an existing regular file, whose owner, group, and permissions are kept"},
		{Name: "match", Notes: "Go regular expression tested against each line. If match is omitted, MMX selects a line by the exact text in line. More than one matching line fails the node task."},
		{Name: "line", Notes: "required with state: present; the whole line, which match must select; replaces the selected line, or is inserted where the task places it when nothing is selected; with absent, give match or line"},
		{Name: "insert_at_end_of_file", Notes: "true: a new line goes at the end of the file; state: present requires exactly one of the six placing settings, applied only when nothing is selected"},
		{Name: "insert_at_start_of_file", Default: "false", Notes: "true: a new line goes before the first line"},
		{Name: "insert_before_first_line_matching", Notes: "Go regular expression; a new line goes directly before the first line it matches; no matching line fails the task and changes nothing"},
		{Name: "insert_before_last_line_matching", Notes: "as insert_before_first_line_matching, before the last line it matches"},
		{Name: "insert_after_first_line_matching", Notes: "as insert_before_first_line_matching, directly after the first line it matches"},
		{Name: "insert_after_last_line_matching", Notes: "as insert_before_first_line_matching, directly after the last line it matches"},
		{Name: "check_new_file", Notes: "as for file"},
		{Name: "secret_readable_by_group", Default: "false", Notes: "allow the file's group to read a line that holds a secret; the file keeps its mode"},
		{Name: "dangerously_let_every_account_read_secret", Default: "false", Notes: "allow every account on the node to read a line that holds a secret; the file keeps its mode"},
		{Name: "state", Values: presenceStates, Default: "present", Notes: "absent removes the selected line and needs exactly one of match or line; it takes no placing setting"},
	},
	Example: "- line:\n    path: /etc/ssh/sshd_config\n    match: '^#?PasswordAuthentication\\s'\n    line: PasswordAuthentication no\n    insert_at_end_of_file: true\n",
	Results: resultBasic,
	Dryrun:  "supported; previews the edit as a file diff; a validator runs only its own dryrun command",
}

// Prepare checks the inputs when the project loads.
func (l *Line) Prepare(base string) error {
	return ValidateLine(*l)
}

// Arguments lists the inputs that may hold a reference, with their rules.
func (l *Line) Arguments(arguments *ArgumentList) {
	arguments.Public("path", &l.Path, absolute)
	if l.Match != "" {
		arguments.Public("match", &l.Match, lineMatch)
	}
	if l.Text != "" {
		arguments.Private("line", l.Text, lineText(l.Match, arguments.Resolved || !Dynamic(l.Match)))
	}
	for _, position := range []struct {
		field   string
		pattern *string
	}{{"insert_before_first_line_matching", &l.InsertBeforeFirstLineMatching}, {"insert_before_last_line_matching", &l.InsertBeforeLastLineMatching}, {"insert_after_first_line_matching", &l.InsertAfterFirstLineMatching}, {"insert_after_last_line_matching", &l.InsertAfterLastLineMatching}} {
		if *position.pattern != "" {
			arguments.Public(""+position.field, position.pattern, lineMatch)
		}
	}
}

// CleanPaths cleans the resolved path.
func (l *Line) CleanPaths() {
	l.Path = filepath.Clean(l.Path)
}

// Reads lists the check of a new file.
func (l *Line) Reads() Reads {
	if l.CheckNewFile == nil {
		return Reads{}
	}
	return Reads{Commands: map[string]*Shell{"check_new_file.shell": l.CheckNewFile.Shell}}
}

// Run does the module's work on the node.
func (l *Line) Run(ctx context.Context, task NodeTask) protocol.Result {
	return task.Runtime.Line(ctx, *l, task.Scope)
}

// ManagedPaths lists the paths the task manages or says it creates, as
// written; a dryrun cannot check a later task's path_exists on them.
func (l *Line) ManagedPaths() []string {
	return []string{l.Path}
}

// TrackedInputs lists the edited file, hashed as it is on disk.
func (l *Line) TrackedInputs() []TrackedInput {
	return []TrackedInput{{Kind: "config", Path: l.Path, Content: &File{Path: l.Path}, EditsInPlace: true}}
}

// EditText returns text as the task would edit it.
func (l *Line) EditText(ctx context.Context, text string, s value.Scope) (string, error) {
	return EditLine(text, *l, s)
}
