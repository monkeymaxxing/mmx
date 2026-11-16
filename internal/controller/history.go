package controller

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"

	"mmx/internal/config"
	"mmx/internal/protocol"
)

// UseHistory keeps a command's selected records and artifacts available from
// selection through display or execution. Call release before attempting cleanup.
func (c Controller) UseHistory() (release func(), err error) {
	lease, err := c.lockHistory(false)
	if err != nil {
		return nil, err
	}
	return func() { unlockRun(lease) }, nil
}

// Commands share this lease, so nodes and different controllers still save
// progress independently. Cleanup takes it exclusively or defers to later use.
func (c Controller) lockHistory(cleanup bool) (*os.File, error) {
	if err := os.MkdirAll(c.State, 0700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(c.State, "history.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	// Every file MMX saves here gets its permissions set. A file system that
	// refuses them would fail the first save with a bare chmod error, so the
	// lock file tries first, before any command starts its work.
	if err := f.Chmod(0600); err != nil {
		f.Close()
		if errors.Is(err, os.ErrPermission) {
			return nil, fmt.Errorf("MMX cannot save files in %s: the file system there refuses file permissions, "+
				"and MMX sets the permissions of every file it saves. A Windows drive under WSL, such as /mnt/c, can do this. "+
				"Move the project directory to a Linux file system, such as your home directory in WSL, then run again.", c.State)
		}
		return nil, err
	}
	mode := syscall.LOCK_SH
	if cleanup {
		mode = syscall.LOCK_EX | syscall.LOCK_NB
	}
	if err := syscall.Flock(int(f.Fd()), mode); err != nil {
		f.Close()
		if cleanup && (errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN)) {
			return nil, nil
		}
		return nil, err
	}
	return f, nil
}

// A finished run starts its retention period.
func finished(r *Run) bool {
	if !protocol.Terminal(r.Status) {
		return false
	}
	for _, en := range r.Entries {
		if protocol.Uncertain(en.Response.Status) {
			return false
		}
	}
	return true
}

// A run expires after its retention period unless it is its selection's
// latest normal run.
func prunable(r *Run, latestNormal string, retention time.Duration, now time.Time) bool {
	return r.ID != latestNormal && finished(r) && r.Finished != nil && !now.Before(r.Finished.Add(retention))
}

var runnerHash = regexp.MustCompile(`^[a-f0-9]{64}$`)

// Prune removes expired local history for this selection, then unreferenced
// cached binaries across the state directory. It never contacts a server.
func (c Controller) Prune(selection *Run, policy string, now time.Time) error {
	retention, err := config.HistoryRetention(policy)
	if err != nil || retention == 0 {
		return err
	}
	lease, err := c.lockHistory(true)
	if err != nil || lease == nil {
		return err
	}
	defer unlockRun(lease)
	files, err := filepath.Glob(filepath.Join(c.State, "runs", "*", "run.json"))
	if err != nil {
		return err
	}
	sort.Sort(sort.Reverse(sort.StringSlice(files)))
	latestNormal := ""
	var expired []string
	keepRunners := map[string]bool{}
	// Read everything before deleting anything: an unreadable run may own a
	// cached binary, and must not be treated as though it did not exist.
	var unreadable []string
	for _, path := range files {
		r, err := c.readRunWithProgress(path)
		if err != nil {
			unreadable = append(unreadable, fmt.Sprintf("%s (%v)", filepath.Dir(path), err))
			continue
		}
		if latestNormal == "" && r.Identity() == selection.Identity() && !r.Dryrun {
			latestNormal = r.ID
		}
		if r.Identity() == selection.Identity() && prunable(r, latestNormal, retention, now) {
			expired = append(expired, r.ID)
			continue
		}
		for _, en := range r.Entries {
			keepRunners[en.RunnerHash] = true
		}
	}
	// An upgrade may discard saved state, so a run that another version of
	// MMX wrote is not migrated; the person removes it.
	if len(unreadable) > 0 {
		runs, directories := "this saved run", "that run directory"
		if len(unreadable) > 1 {
			runs, directories = "these saved runs", "those run directories"
		}
		return fmt.Errorf("MMX cannot read %s, which another version of MMX may have written: %s. "+
			"History cleanup removes nothing while a saved run cannot be read, because that run may use a cached runner binary. "+
			"Remove %s, then run again.", runs, strings.Join(unreadable, "; "), directories)
	}
	for _, id := range expired {
		if err := os.RemoveAll(c.runDir(id)); err != nil {
			return err
		}
	}
	files, err = filepath.Glob(filepath.Join(c.State, "runners", "*"))
	if err != nil {
		return err
	}
	for _, path := range files {
		hash := filepath.Base(path)
		if !runnerHash.MatchString(hash) || keepRunners[hash] {
			continue
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if info.Mode().IsRegular() {
			if err := os.Remove(path); err != nil {
				return err
			}
		}
	}
	return nil
}
