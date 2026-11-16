package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"mmx/internal/config"
	"mmx/internal/modules"
)

const newOneTimePlaybookUsage = "usage: mmx new-one-time-playbook WHAT_IT_DOES\n\nWHAT_IT_DOES is lowercase words joined by underscores, such as remove_old_ruby_manager. The command creates one_time_playbooks/YYYY-MM-DD_WHAT_IT_DOES.yml, dated today, with one example fleet stage to replace."

// newOneTimePlaybookCommand creates a dated one-time playbook that mmx
// validate accepts as it is: comment lines that say what the file is and how
// to preview, run, check, and complete it, and one fleet stage with an
// example node task that changes nothing.
func newOneTimePlaybookCommand(args []string) (int, error) {
	if len(args) == 1 && (args[0] == "--help" || args[0] == "-h") {
		fmt.Println(newOneTimePlaybookUsage)
		return 0, nil
	}
	// The command has no options, also no --json: its result is the file.
	fs := flag.NewFlagSet("new-one-time-playbook", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0, nil
		}
		return 4, err
	}
	if fs.NArg() != 1 {
		return 4, fmt.Errorf("%s", newOneTimePlaybookUsage)
	}
	whatItDoes := fs.Arg(0)
	if !config.WhatAOneTimePlaybookDoes.MatchString(whatItDoes) {
		return 4, fmt.Errorf("WHAT_IT_DOES must be lowercase words joined by underscores, such as remove_old_ruby_manager; got %q", whatItDoes)
	}
	// WHAT_IT_DOES is also the ID of the new playbook's fleet stage.
	if !modules.Name.MatchString(whatItDoes) {
		return 4, fmt.Errorf("WHAT_IT_DOES must start with a letter, because it becomes the ID of the new playbook's fleet stage; got %q", whatItDoes)
	}
	settingsFile, err := config.FindProject(".")
	if err != nil {
		return 4, err
	}
	name := time.Now().Format("2006-01-02") + "_" + whatItDoes
	file := config.OneTimePlaybookPath(settingsFile, name)
	for _, existing := range []string{file, config.CompletedOneTimePlaybookPath(settingsFile, name)} {
		if _, err := os.Lstat(existing); err == nil {
			return 4, fmt.Errorf("%s already exists; a one-time playbook name cannot be reused after completion; choose different words for WHAT_IT_DOES", displayPath(existing))
		} else if !os.IsNotExist(err) {
			return 4, err
		}
	}
	if err := os.MkdirAll(filepath.Dir(file), 0755); err != nil {
		return 4, err
	}
	f, err := os.OpenFile(file, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
	if err != nil {
		return 4, err
	}
	_, err = f.WriteString(newOneTimePlaybookText(name, whatItDoes))
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return 4, err
	}
	fmt.Printf("Created %s. Replace its example node task with the work, then preview it with mmx dryrun %s.\n", displayPath(file), name)
	return 0, nil
}

// newOneTimePlaybookText is the text of a new one-time playbook.
func newOneTimePlaybookText(name, whatItDoes string) string {
	return `# A one-time playbook holds work that must happen once on every node it
# selects and never again, such as removing a retired program. It runs only
# when a command names it.
#
# Preview what it would change:      mmx dryrun ` + name + `
# Run it:                            mmx run ` + name + `
# Check which nodes still need it:   mmx check-one-time-playbooks
# Complete it when no node needs it: mmx complete-one-time-playbook ` + name + `

fleet_stages:
  - id: ` + whatItDoes + `
    nodes: all

    node_tasks:
      # An example node task that changes nothing. Replace it with the work.
      - show: "Replace this example node task with the work of this one-time playbook."
`
}
