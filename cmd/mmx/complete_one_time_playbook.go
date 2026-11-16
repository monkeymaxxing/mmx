package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"mmx/internal/config"
	"mmx/internal/controller"
)

// oneTimePlaybookCompletion is what mmx complete-one-time-playbook reports:
// the check of its one playbook, and whether the playbook was completed.
type oneTimePlaybookCompletion struct {
	oneTimePlaybookCheck
	// Status is "completed" or "refused".
	Status string `json:"status"`
	// CompletedPlaybookFile is the absolute path of the playbook's new file.
	CompletedPlaybookFile string `json:"completed_playbook_file,omitempty"`
	// InventoriesNotInspected are the project's other inventories: the check
	// inspected only the nodes of the inventory it was given.
	InventoriesNotInspected []string `json:"inventories_not_inspected,omitempty"`
}

// completeOneTimePlaybookCommand runs the check for one one-time playbook
// and, when every node it selects is done, moves its file to completed/ with
// a first line that says so.
func completeOneTimePlaybookCommand(ctx context.Context, args []string) (int, error) {
	fs := flag.NewFlagSet("complete-one-time-playbook", flag.ContinueOnError)
	options := addOneTimePlaybookOptions(fs)
	connection := addConnectionOption(fs)
	// Accept the playbook name before or after the options.
	name := ""
	for {
		if err := fs.Parse(args); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return 0, nil
			}
			return 4, err
		}
		if fs.NArg() == 0 {
			break
		}
		if name != "" {
			return 4, fmt.Errorf("complete-one-time-playbook takes one playbook name")
		}
		name, args = fs.Arg(0), fs.Args()[1:]
	}
	if name == "" {
		return 4, fmt.Errorf("usage: mmx complete-one-time-playbook NAME [--inventory NAME] [--connection ssh_only]; NAME is a one-time playbook's name, such as 2026-10-05_remove_old_ruby_manager")
	}
	if err := checkPasswordFileOptions(fs); err != nil {
		return 4, err
	}
	if !config.IsOneTimePlaybookName(name) {
		return 4, fmt.Errorf("%q is not a valid one-time playbook name; expected %s", name, config.OneTimePlaybookNameRule)
	}
	settingsFile, err := config.FindProject(".")
	if err != nil {
		return 4, err
	}
	file, completedFile := config.OneTimePlaybookPath(settingsFile, name), config.CompletedOneTimePlaybookPath(settingsFile, name)
	// Refuse before any node is contacted when there is nothing to move or
	// the move could not happen. A name that only completed/ has is refused
	// by the loader, with the way to make the playbook runnable again.
	_, outstandingErr := os.Lstat(file)
	_, completedErr := os.Lstat(completedFile)
	switch {
	case outstandingErr == nil && completedErr == nil:
		return 4, completedFileExistsError(file, completedFile)
	case os.IsNotExist(outstandingErr) && os.IsNotExist(completedErr):
		return 4, config.NoSuchPlaybookError(settingsFile, name)
	}
	inspector, err := newOneTimePlaybookInspector(ctx, settingsFile, options)
	if err != nil {
		return 4, err
	}
	defer inspector.close()
	p, inv, err := config.Load(settingsFile, name, inspector.inventory.Name, nil)
	if err == nil {
		applyConnectionChoice(inv, *connection)
		err = config.RequireCommandLineValues(p)
	}
	if err != nil {
		return 4, err
	}
	if !*options.asJSON {
		fmt.Println(inspector.inventoryLine() + "\n")
	}
	checked, err := inspector.inspect(ctx, p, inv)
	if err != nil && !errors.Is(err, context.Canceled) {
		return 4, err
	}
	completion := oneTimePlaybookCompletion{Status: "refused", oneTimePlaybookCheck: oneTimePlaybookCheck{SchemaVersion: 1, Inventory: inv.Name, OneTimePlaybooks: []checkedOneTimePlaybook{checked}}}
	inventories, err := config.ProjectNames(settingsFile, "inventories")
	if err != nil {
		return 4, err
	}
	completion.InventoriesNotInspected = slices.DeleteFunc(inventories, func(inventory string) bool { return inventory == inv.Name })
	code := oneTimePlaybookExitCode(completion.OneTimePlaybooks)
	if ctx.Err() != nil {
		code = 130
	}
	if !*options.asJSON {
		showCheckedOneTimePlaybook(os.Stdout, checked, "auto")
	}
	if code == 0 {
		firstLine := fmt.Sprintf("# Completed %s: %s.\n", time.Now().Format("2006-01-02"), doneOnServers(len(checked.Done), len(checked.Done)))
		if err := moveToCompleted(file, completedFile, firstLine); err != nil {
			return 4, err
		}
		completion.Status, completion.CompletedPlaybookFile = "completed", completedFile
	}
	if *options.asJSON {
		if code, err := encode(completion); err != nil {
			return code, err
		}
	} else {
		if code == 0 {
			fmt.Printf("\nCompleted: moved %s to %s.\n", displayPath(file), displayPath(completedFile))
		}
		if others := completion.InventoriesNotInspected; len(others) > 0 && code != 130 {
			fmt.Printf("\nNote: MMX inspected nodes only in inventory %s. MMX did not inspect nodes in %s. Before completing a one-time playbook, check every inventory with mmx check-one-time-playbooks --inventory NAME.\n", inv.Name, otherInventories(others))
			if code == 0 {
				fmt.Printf("If a node in another inventory still needs the completed one-time playbook, move the playbook file back to %s/ and run the playbook with the inventory that contains the node.\n", config.OneTimePlaybooksDirectory)
			}
		}
	}
	if code == 0 || code == 130 {
		return code, nil
	}
	return code, notCompletedError(checked)
}

// otherInventories words the project's other inventories for the note.
func otherInventories(names []string) string {
	if len(names) == 1 {
		return "another inventory, " + names[0]
	}
	return "other inventories, " + strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
}

// notCompletedError says why a playbook stays outstanding and what to do.
func notCompletedError(checked checkedOneTimePlaybook) error {
	var causes []string
	if n := len(checked.StillNeeded); n == 1 {
		causes = append(causes, "1 node still needs the one-time playbook")
	} else if n > 1 {
		causes = append(causes, fmt.Sprintf("%d nodes still need the one-time playbook", n))
	}
	if n := len(checked.NotKnown); n > 0 {
		causes = append(causes, fmt.Sprintf("the state of %s is not known", serverCount(n)))
	}
	if checked.NotInspectedReason != "" {
		causes = append(causes, checked.NotInspectedReason)
	}
	advice := ""
	if len(checked.StillNeeded) > 0 {
		advice += fmt.Sprintf("; run the one-time playbook with mmx run %s", checked.Name)
	}
	if checked.RunID != "" {
		advice += fmt.Sprintf("; mmx results %s --diff shows what the check found on each server", controller.ShortID(checked.RunID))
	}
	return fmt.Errorf("one-time playbook %s is not completed and stays in %s/: %s%s", checked.Name, config.OneTimePlaybooksDirectory, strings.Join(causes, ", and "), advice)
}

func completedFileExistsError(file, completedFile string) error {
	return fmt.Errorf("%s already exists, so MMX cannot move %s into the completed directory: MMX never replaces a completed one-time playbook, and a one-time playbook's file is in only one of the two directories. Remove or rename one of the two files", displayPath(completedFile), displayPath(file))
}

// moveToCompleted writes the completed playbook, its new first line followed
// by its unchanged text, and only then removes the original, so that no
// failure loses the playbook. It never replaces a file in completed/. Its
// errors name the two files as the working directory reaches them.
func moveToCompleted(file, completedFile, firstLine string) error {
	notWritten := func(err error) error {
		// The operating system's error names a file by its absolute path.
		var path *os.PathError
		if errors.As(err, &path) {
			err = path.Err
		}
		return fmt.Errorf("cannot write the completed playbook file %s: %w; the playbook file %s stays where it is", displayPath(completedFile), err, displayPath(file))
	}
	text, err := os.ReadFile(file)
	if err != nil {
		return notWritten(err)
	}
	info, err := os.Stat(file)
	if err != nil {
		return notWritten(err)
	}
	if err := os.MkdirAll(filepath.Dir(completedFile), 0755); err != nil {
		return notWritten(err)
	}
	f, err := os.OpenFile(completedFile, os.O_CREATE|os.O_EXCL|os.O_WRONLY, info.Mode().Perm())
	if os.IsExist(err) {
		return completedFileExistsError(file, completedFile)
	}
	if err != nil {
		return notWritten(err)
	}
	// The playbook keeps its permissions, whatever the umask of this process.
	if err = f.Chmod(info.Mode().Perm()); err == nil {
		_, err = f.Write(append([]byte(firstLine), text...))
	}
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		// An unfinished file in completed/ would make the playbook look completed.
		os.Remove(completedFile)
		return notWritten(err)
	}
	if err := os.Remove(file); err != nil {
		var path *os.PathError
		if errors.As(err, &path) {
			err = path.Err
		}
		return fmt.Errorf("the completed playbook file %s was written, but the original playbook file could not be removed: %w; remove %s yourself", displayPath(completedFile), err, displayPath(file))
	}
	return nil
}
