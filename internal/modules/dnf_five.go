package modules

import (
	"context"
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// dnf 5 is the dnf of Fedora 41 and newer. Where it differs from dnf 4,
// dnf_package branches on the dnf the node reports (DNF.five), inside the
// module. What differs, as observed on Fedora 45 with dnf 5.4.7:
//
//   - The version lock is built in: /etc/dnf/versionlock.toml, a TOML file
//     that holds conditions on a package's epoch and evr. No plugin package
//     is needed, and the conditions have no wildcard.
//   - dnf has no option for another lock file, so a preview cannot stage a
//     rule that is not on disk yet.
//   - A transaction table lists the package an upgrade replaces on a line
//     that begins with "replacing".
//   - config-manager has the commands enable and disable, not the options
//     --set-enabled and --set-disabled.
//   - updateinfo names an advisory's type in words: security, bugfix,
//     enhancement, unspecified.

// isDnfFive reports whether the program dnf is a link to dnf5, as on Fedora
// 41 and newer; a dnf5 installed beside dnf 4 does not count. Reading the
// link costs no process, as dnf --version would on every node job.
func isDnfFive(ctx context.Context) bool {
	program, err := lookPath(ctx, "dnf")
	if err != nil {
		return false
	}
	target, err := filepath.EvalSymlinks(program)
	return err == nil && filepath.Base(target) == "dnf5"
}

// dnfFiveLockTable is dnf 5's version lock.
const dnfFiveLockTable = "/etc/dnf/versionlock.toml"

func dnfFiveLockComment(name string) string {
	return "Written by MMX for package " + name
}

// dnfFiveVersionRule matches the version rules that dnf 5's lock can hold
// besides an exact version: an optional epoch, then dotted numbers that end
// in ".*", such as 16.* or 3:27.3.*.
var dnfFiveVersionRule = regexp.MustCompile(`^(?:([0-9]+):)?([0-9]+(?:\.[0-9]+)*)\.\*$`)

// dnfFiveLockBlock is the entry that keeps a package at a version rule, in
// dnf 5's lock file. It is empty for the rule *, which allows every version.
// dnf 5's conditions compare whole evr text and have no wildcard, so a rule
// that ends in .* becomes a range: from the rule's number to just below the
// next number. The range also admits the bare number (27.3 for 27.3.*), a
// version that has no ".3." after it. The upper bound ends in ~, which rpm
// sorts before any text after it, so 27.4~rc1 stays outside.
func dnfFiveLockBlock(name, rule string) (string, error) {
	if rule == "*" {
		return "", nil
	}
	type condition struct{ comparator, value string }
	var conditions []condition
	if !strings.Contains(rule, "*") {
		if !strings.Contains(rule, ":") {
			rule = "0:" + rule
		}
		conditions = []condition{{"=", rule}}
	} else {
		parts := dnfFiveVersionRule.FindStringSubmatch(rule)
		if parts == nil {
			return "", fmt.Errorf("dnf_package: the version rule %q for %s cannot be kept on this node. Its dnf is dnf 5, whose version lock has no wildcard. Write an exact version, such as 1.2.3-1.fc45, or numbers that end in .*, such as 16.* or 3:27.3.*, or just *", rule, name)
		}
		epoch, numbers := parts[1], strings.Split(parts[2], ".")
		if epoch == "" {
			epoch = "0"
		}
		last, err := strconv.ParseUint(numbers[len(numbers)-1], 10, 64)
		if err != nil || last == 1<<64-1 {
			return "", fmt.Errorf("dnf_package: the version rule %q for %s has a number too large for dnf 5's version lock", rule, name)
		}
		upper := append(append([]string{}, numbers[:len(numbers)-1]...), strconv.FormatUint(last+1, 10))
		conditions = []condition{{">=", epoch + ":" + parts[2]}, {"<", epoch + ":" + strings.Join(upper, ".") + "~"}}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "[[packages]]\nname = %q\ncomment = %q\n", name, dnfFiveLockComment(name))
	for _, c := range conditions {
		fmt.Fprintf(&b, "\n[[packages.conditions]]\nkey = \"evr\"\ncomparator = %q\nvalue = %q\n", c.comparator, c.value)
	}
	return b.String(), nil
}

// dnf 5's lock file is read by every dnf command, and an invalid file makes
// every one of them fail ("Error parsing versionlock file", exit status 1).
// MMX therefore edits it only when it recognises every line, and checks its
// own result before writing it. What it recognises: the version line, tables
// [[packages]] with the keys name and comment, and their conditions
// [[packages.conditions]] with the keys key, comparator, and value, each a
// string; blank lines and comments anywhere. A header may have inner spaces
// and a trailing comment.

type dnfFiveCondition struct{ key, comparator, value string }

// dnfFiveTable is one [[packages]] table. text is the table as the file
// holds it, from the comment lines directly before its header to its last
// line; unchanged tables are written back as they are.
type dnfFiveTable struct {
	text          string
	name, comment string
	conditions    []dnfFiveCondition
}

var (
	dnfFivePackagesHeader   = regexp.MustCompile(`^\s*\[\[\s*packages\s*\]\]\s*(#.*)?$`)
	dnfFiveConditionsHeader = regexp.MustCompile(`^\s*\[\[\s*packages\s*\.\s*conditions\s*\]\]\s*(#.*)?$`)
	dnfFiveKeyValue         = regexp.MustCompile(`^\s*([A-Za-z_][A-Za-z0-9_-]*)\s*=\s*(.*)$`)
	// dnf writes this line for a file that holds no table, and the line must
	// go when a [[packages]] table is added: TOML defines packages only once.
	dnfFiveEmptyArray = regexp.MustCompile(`^\s*packages\s*=\s*\[\s*\]\s*(#.*)?$`)
	dnfFiveString     = regexp.MustCompile(`^"((?:[^"\\]|\\["\\])*)"\s*(#.*)?$|^'([^']*)'\s*(#.*)?$`)
)

func dnfFiveStringValue(raw string) (string, bool) {
	m := dnfFiveString.FindStringSubmatch(strings.TrimSpace(raw))
	if m == nil {
		return "", false
	}
	if m[3] != "" || strings.HasPrefix(strings.TrimSpace(raw), "'") {
		return m[3], true
	}
	return strings.NewReplacer(`\"`, `"`, `\\`, `\`).Replace(m[1]), true
}

// dnfFiveUnrecognised says that MMX will not edit the file.
func dnfFiveUnrecognised(number int, line string) error {
	return fmt.Errorf("dnf_package: line %d of %s, %q, is something MMX does not recognise, so MMX does not edit that file (dnf 5 fails every command when the file is invalid). Edit the file by hand, or with dnf versionlock, to the form dnf writes", number, dnfFiveLockTable, strings.TrimSpace(line))
}

// parseDnfFiveLock reads the lock file. head holds the lines before the
// first table.
func parseDnfFiveLock(text string) (head string, tables []dnfFiveTable, err error) {
	lines := strings.Split(text, "\n")
	var headLines []string
	type segment struct {
		start, end int
		table      dnfFiveTable
	}
	var segments []*segment
	var current *segment
	var condition *dnfFiveCondition
	for number, line := range lines {
		trimmed := strings.TrimSpace(line)
		switch {
		case dnfFivePackagesHeader.MatchString(line):
			current = &segment{start: number}
			segments = append(segments, current)
			condition = nil
			continue
		case dnfFiveConditionsHeader.MatchString(line):
			if current == nil {
				return "", nil, dnfFiveUnrecognised(number+1, line)
			}
			current.table.conditions = append(current.table.conditions, dnfFiveCondition{})
			condition = &current.table.conditions[len(current.table.conditions)-1]
			continue
		case trimmed == "" || strings.HasPrefix(trimmed, "#"):
			continue
		}
		if current == nil && dnfFiveEmptyArray.MatchString(line) {
			continue
		}
		m := dnfFiveKeyValue.FindStringSubmatch(line)
		if m == nil || strings.HasPrefix(trimmed, "[") {
			return "", nil, dnfFiveUnrecognised(number+1, line)
		}
		value, isString := dnfFiveStringValue(m[2])
		switch {
		case current == nil && m[1] == "version" && isString:
		case current != nil && condition == nil && m[1] == "name" && isString:
			current.table.name = value
		case current != nil && condition == nil && m[1] == "comment" && isString:
			current.table.comment = value
		case current != nil && condition != nil && m[1] == "key" && isString:
			condition.key = value
		case current != nil && condition != nil && m[1] == "comparator" && isString:
			condition.comparator = value
		case current != nil && condition != nil && m[1] == "value" && isString:
			condition.value = value
		default:
			return "", nil, dnfFiveUnrecognised(number+1, line)
		}
	}
	// A segment runs to the line before the comment lines that directly
	// precede the next header, which belong to the next table.
	for i, seg := range segments {
		seg.end = len(lines)
		if i+1 < len(segments) {
			seg.end = segments[i+1].start
		}
	}
	for i := range segments {
		if i+1 < len(segments) {
			end := segments[i+1].start
			for end > segments[i].start+1 && strings.HasPrefix(strings.TrimSpace(lines[end-1]), "#") {
				end--
			}
			segments[i].end = end
			segments[i+1].start = end
		}
	}
	headEnd := len(lines)
	if len(segments) > 0 {
		headEnd = segments[0].start
		for headEnd > 0 && strings.HasPrefix(strings.TrimSpace(lines[headEnd-1]), "#") {
			headEnd--
		}
		segments[0].start = headEnd
	}
	for _, line := range lines[:headEnd] {
		if !dnfFiveEmptyArray.MatchString(line) {
			headLines = append(headLines, line)
		}
	}
	for _, seg := range segments {
		seg.table.text = strings.TrimSpace(strings.Join(lines[seg.start:seg.end], "\n"))
		tables = append(tables, seg.table)
	}
	return strings.TrimSpace(strings.Join(headLines, "\n")), tables, nil
}

// dnfFiveOwnedTable says whether MMX wrote a table for name. The comment's
// value counts, whatever quotes and spaces surround it.
func dnfFiveOwnedTable(t dnfFiveTable, name string) bool {
	return t.name == name && strings.TrimSpace(t.comment) == dnfFiveLockComment(name)
}

// dnfFiveLockText is the lock file with MMX's table for name replaced by
// block, or removed when block is empty. Every other table stays as it is,
// including an administrator's table for the same package. It refuses a file
// it does not recognise, and a result in which any table that MMX did not
// write differs from the file's.
func dnfFiveLockText(existing []byte, name, block string) (string, error) {
	head, tables, err := parseDnfFiveLock(string(existing))
	if err != nil {
		return "", err
	}
	owned, kept := 0, []dnfFiveTable{}
	for _, table := range tables {
		if dnfFiveOwnedTable(table, name) {
			owned++
			continue
		}
		kept = append(kept, table)
	}
	block = strings.TrimSpace(block)
	if owned == 0 && block == "" {
		return string(existing), nil
	}
	if owned == 1 && block != "" {
		for _, table := range tables {
			if dnfFiveOwnedTable(table, name) && table.text == block {
				return string(existing), nil
			}
		}
	}
	if head == "" {
		head = `version = "1.0"`
	}
	parts := []string{head}
	for _, table := range kept {
		parts = append(parts, table.text)
	}
	if block != "" {
		parts = append(parts, block)
	}
	text := strings.Join(parts, "\n\n") + "\n"
	_, after, err := parseDnfFiveLock(text)
	if err != nil {
		return "", fmt.Errorf("dnf_package: the lock file MMX would write is not one MMX recognises (%w); nothing was written", err)
	}
	var wantKept, gotKept []string
	for _, table := range kept {
		wantKept = append(wantKept, table.text)
	}
	for _, table := range after {
		if !dnfFiveOwnedTable(table, name) {
			gotKept = append(gotKept, table.text)
		}
	}
	if strings.Join(wantKept, "\x00") != strings.Join(gotKept, "\x00") {
		return "", fmt.Errorf("dnf_package: editing %s would change a table that MMX did not write; nothing was written", dnfFiveLockTable)
	}
	return text, nil
}

// dnfFiveOwnedTables counts the tables MMX wrote for a package.
func dnfFiveOwnedTables(existing []byte, name string) (int, error) {
	_, tables, err := parseDnfFiveLock(string(existing))
	if err != nil {
		return 0, err
	}
	count := 0
	for _, table := range tables {
		if dnfFiveOwnedTable(table, name) {
			count++
		}
	}
	return count, nil
}

// dnfFiveExcludes says whether the administrator's tables for a package, the
// ones MMX did not write, keep a version out. dnf 5 allows a version that any
// table of the package allows, and a table allows it when all its conditions
// hold. A table with a key or comparator that MMX does not evaluate (arch, or
// an unknown comparator) counts as allowing, so dnf's own answer decides.
func dnfFiveExcludes(ctx context.Context, existing []byte, name, version string) (bool, error) {
	_, tables, err := parseDnfFiveLock(string(existing))
	if err != nil {
		return false, nil
	}
	any := false
	for _, table := range tables {
		if table.name != name || dnfFiveOwnedTable(table, name) {
			continue
		}
		any = true
		allows, err := dnfFiveTableAllows(ctx, table, version)
		if err != nil {
			return false, err
		}
		if allows {
			return false, nil
		}
	}
	return any, nil
}

func dnfFiveTableAllows(ctx context.Context, table dnfFiveTable, version string) (bool, error) {
	if !strings.Contains(version, ":") {
		version = "0:" + version
	}
	for _, c := range table.conditions {
		var order int
		switch c.key {
		case "evr":
			want := c.value
			if !strings.Contains(want, ":") {
				want = "0:" + want
			}
			o, err := rpmOrder(ctx, version, want)
			if err != nil {
				return false, err
			}
			order = o
		case "epoch":
			have, _, _ := strings.Cut(version, ":")
			a, errA := strconv.Atoi(have)
			b, errB := strconv.Atoi(c.value)
			if errA != nil || errB != nil {
				return true, nil
			}
			order = a - b
		default:
			return true, nil
		}
		var holds bool
		switch c.comparator {
		case "=":
			holds = order == 0
		case "!=":
			holds = order != 0
		case "<":
			holds = order < 0
		case "<=":
			holds = order <= 0
		case ">":
			holds = order > 0
		case ">=":
			holds = order >= 0
		default:
			return true, nil
		}
		if !holds {
			return false, nil
		}
	}
	return true, nil
}

// rpmOrder is rpm's order of two versions: negative, zero, or positive.
func rpmOrder(ctx context.Context, a, b string) (int, error) {
	if a == b {
		return 0, nil
	}
	newer, err := rpmNewer(ctx, a, b)
	if err != nil {
		return 0, err
	}
	if newer {
		return 1, nil
	}
	back, err := rpmNewer(ctx, b, a)
	if err != nil {
		return 0, err
	}
	if back {
		return -1, nil
	}
	return 0, nil
}
