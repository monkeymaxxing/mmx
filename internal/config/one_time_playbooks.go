package config

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

// A one-time playbook is a dated playbook for work that happens once on
// every node it selects. It is a file in OneTimePlaybooksDirectory, and it
// moves to CompletedOneTimePlaybooksDirectory when no node needs it any more.
const (
	OneTimePlaybooksDirectory          = "one_time_playbooks"
	CompletedOneTimePlaybooksDirectory = "one_time_playbooks/completed"
	// OneTimePlaybookNameRule says the name in an error message.
	OneTimePlaybookNameRule = "YYYY-MM-DD_what_it_does: the date it was written, an underscore, and lowercase words joined by underscores, such as 2026-10-05_remove_old_ruby_manager"
)

var (
	// WhatAOneTimePlaybookDoes is the part of a one-time playbook's name
	// after its date: lowercase words joined by underscores.
	WhatAOneTimePlaybookDoes = regexp.MustCompile(`^[a-z0-9]+(_[a-z0-9]+)*$`)
	datedName                = regexp.MustCompile(`^(\d{4}-\d{2}-\d{2})_(.+)$`)
)

// IsOneTimePlaybookName reports that a playbook name is a one-time
// playbook's. An ordinary playbook's name starts with a letter or an
// underscore, so no name belongs to both kinds.
func IsOneTimePlaybookName(name string) bool {
	m := datedName.FindStringSubmatch(name)
	if m == nil || !WhatAOneTimePlaybookDoes.MatchString(m[2]) {
		return false
	}
	_, err := time.Parse("2006-01-02", m[1])
	return err == nil
}

// OneTimePlaybookPath is the file of an outstanding one-time playbook.
func OneTimePlaybookPath(settingsFile, name string) string {
	return filepath.Join(filepath.Dir(settingsFile), OneTimePlaybooksDirectory, name+".yml")
}

// CompletedOneTimePlaybookPath is the file of a completed one-time playbook.
func CompletedOneTimePlaybookPath(settingsFile, name string) string {
	return filepath.Join(filepath.Dir(settingsFile), CompletedOneTimePlaybooksDirectory, name+".yml")
}

// CompletedOneTimePlaybookError says that a command named a one-time
// playbook whose file is in completed/.
type CompletedOneTimePlaybookError struct{ Name string }

func (e *CompletedOneTimePlaybookError) Error() string {
	return fmt.Sprintf("one-time playbook %s is completed: its file is %s/%s.yml, and a completed one-time playbook cannot be run, dry-run, or checked; move the file back to %s/ to make it runnable again",
		e.Name, CompletedOneTimePlaybooksDirectory, e.Name, OneTimePlaybooksDirectory)
}

// OutstandingOneTimePlaybooks lists the one-time playbooks outside
// completed/, oldest first. Anything else in the directory is an error,
// except completed/ itself and files whose names start with a dot, which a
// file manager or Git leaves there.
func OutstandingOneTimePlaybooks(settingsFile string) ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(filepath.Dir(settingsFile), OneTimePlaybooksDirectory))
	if os.IsNotExist(err) {
		return []string{}, nil
	}
	if err != nil {
		return nil, err
	}
	names := []string{}
	for _, entry := range entries {
		name := entry.Name()
		switch {
		case strings.HasPrefix(name, "."), entry.IsDir() && name == filepath.Base(CompletedOneTimePlaybooksDirectory):
		case !entry.IsDir() && strings.HasSuffix(name, ".yml") && IsOneTimePlaybookName(strings.TrimSuffix(name, ".yml")):
			names = append(names, strings.TrimSuffix(name, ".yml"))
		default:
			return nil, fmt.Errorf("%s/%s: one_time_playbooks/ accepts only one-time playbook files and the completed/ directory; name each one-time playbook file using %s.yml", OneTimePlaybooksDirectory, name, OneTimePlaybookNameRule)
		}
	}
	slices.Sort(names)
	return names, nil
}

// playbookFile finds the file of a named playbook: an ordinary playbook in
// playbooks/, or a one-time playbook in one_time_playbooks/.
func playbookFile(settingsFile, name string) (string, error) {
	if !IsOneTimePlaybookName(name) {
		return filepath.Join(filepath.Dir(settingsFile), "playbooks", name+".yml"), nil
	}
	file := OneTimePlaybookPath(settingsFile, name)
	if _, err := os.Stat(file); os.IsNotExist(err) {
		if _, err := os.Stat(CompletedOneTimePlaybookPath(settingsFile, name)); err == nil {
			return "", &CompletedOneTimePlaybookError{Name: name}
		}
	}
	return file, nil
}

// NoSuchPlaybookError says which file a playbook's name stands for, that
// the file does not exist, and which playbooks of that kind the project has.
func NoSuchPlaybookError(settingsFile, name string) error {
	if IsOneTimePlaybookName(name) {
		names, _ := OutstandingOneTimePlaybooks(settingsFile)
		return fmt.Errorf("there is no one-time playbook %s: %s/%s.yml does not exist; the one-time playbooks outside completed/ are: %s", name, OneTimePlaybooksDirectory, name, namesOrNone(names))
	}
	names, _ := ProjectNames(settingsFile, "playbooks")
	return fmt.Errorf("there is no playbook %s: playbooks/%s.yml does not exist; the project's playbooks are: %s", name, name, namesOrNone(names))
}

func namesOrNone(names []string) string {
	if len(names) == 0 {
		return "(none)"
	}
	return strings.Join(names, ", ")
}

// checkOneTimePlaybook holds a one-time playbook to its one extra rule: MMX
// checks it on the nodes without being given values.
func checkOneTimePlaybook(book Playbook) error {
	for _, name := range sortedKeys(book.CommandLineVars) {
		if v := book.CommandLineVars[name]; v.Default == nil {
			return v.Sources.Error("", fmt.Errorf("command-line variable %s of a one-time playbook needs a default: mmx check-one-time-playbooks inspects the nodes without --var", name))
		}
	}
	return nil
}
