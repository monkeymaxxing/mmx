package controller

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"mmx/internal/durable"
	"mmx/internal/protocol"
	"mmx/internal/release"
)

func (c Controller) runDir(id string) string { return filepath.Join(c.State, "runs", id) }

// ShortID is a run's displayed ID: the first eight hex digits of the random
// part of its full ID. Either selects the run while the short form is unique.
func ShortID(id string) string {
	if _, random, ok := strings.Cut(id, "-"); ok && len(random) >= 8 {
		return random[:8]
	}
	return id
}

// The initial run fixes the definitions and the selections known at the start.
// An entry without a progress file has not been admitted and keeps its
// initial state.
func (c Controller) createRun(r *Run) error {
	dir := c.runDir(r.ID)
	if err := os.MkdirAll(filepath.Join(dir, "entries"), 0700); err != nil {
		return err
	}
	return durable.JSON(filepath.Join(dir, "run.json"), r)
}

func (c Controller) saveStatus(r *Run) error {
	if finished(r) {
		if r.Finished == nil {
			now := time.Now().UTC()
			r.Finished = &now
		}
	} else {
		r.Finished = nil
	}
	return durable.JSON(filepath.Join(c.runDir(r.ID), "status.json"), runStatusRecord{Status: r.Status, Finished: r.Finished, FleetStagesAwaitingNodeSelection: r.FleetStagesAwaitingNodeSelection, SecretsRefreshed: r.SecretsRefreshed})
}

// entryPath names an entry's progress file by its fleet stage path and node,
// which identify it. Neither contains a NUL byte.
func (c Controller) entryPath(id string, entry Entry) string {
	hash := sha256.Sum256([]byte(entry.FleetStage + "\x00" + entry.Node))
	return filepath.Join(c.runDir(id), "entries", fmt.Sprintf("%x.json", hash))
}

func (c Controller) saveEntry(id string, entry Entry) error {
	return durable.JSON(c.entryPath(id, entry), entry)
}

// progressOnly reports whether a save only updates running progress under a
// job ID already saved durably. The node can resend that progress after a
// crash; a new job ID must be durable before submission and a finished or
// uncertain response before it is reported.
func progressOnly(entry Entry, published string) bool {
	return entry.JobID != "" && entry.JobID == published && entry.Response.Status == "running"
}

// liveInterval spaces the progress-only writes of one entry; they feed
// other commands' live view, and the scheduler's own display reads memory.
const liveInterval = time.Second

// entrySaver saves one scheduled entry: durably, or for progress only by an
// atomic replacement without fsync, at most once per liveInterval. Progress
// arriving sooner is written when the interval ends, so the newest progress
// always reaches the live view. finish saves the entry's final state durably
// and drops any pending progress. Unchanged bytes are not rewritten once they
// are durable. saved is the entry as already saved: its fleet stage and node
// name the file, and its job ID is the one already published.
func (c Controller) entrySaver(id string, saved Entry) (save, finish func(Entry) error) {
	path, published := c.entryPath(id, saved), saved.JobID
	var mu sync.Mutex
	var last []byte
	var written time.Time
	var pending *Entry
	var flush *time.Timer
	durableLast := false
	write := func(entry Entry, live bool) error {
		b, err := json.MarshalIndent(entry, "", "  ")
		if err != nil {
			return err
		}
		b = append(b, '\n')
		if bytes.Equal(b, last) && (durableLast || live) {
			return nil
		}
		replace := durable.Write
		if live {
			replace = durable.Replace
		}
		if err := replace(path, b, 0600); err != nil {
			return err
		}
		last, durableLast, written = b, !live, time.Now()
		if !live {
			published = entry.JobID
		}
		return nil
	}
	save = func(entry Entry) error {
		mu.Lock()
		defer mu.Unlock()
		live := progressOnly(entry, published)
		if wait := liveInterval - time.Since(written); live && wait > 0 {
			pending = &entry
			if flush == nil {
				flush = time.AfterFunc(wait, func() {
					mu.Lock()
					defer mu.Unlock()
					flush = nil
					if pending != nil {
						entry := *pending
						pending = nil
						// A failed live write only delays the view; finish saves the outcome.
						_ = write(entry, progressOnly(entry, published))
					}
				})
			}
			return nil
		}
		pending = nil
		return write(entry, live)
	}
	finish = func(entry Entry) error {
		mu.Lock()
		defer mu.Unlock()
		pending = nil
		if flush != nil {
			flush.Stop()
			flush = nil
		}
		return write(entry, false)
	}
	return save, finish
}

func (c Controller) loadProgress(r *Run) error {
	stop, err := c.stopRequested(r.ID)
	if err != nil {
		return err
	}
	r.StopRequested = stop
	b, err := os.ReadFile(filepath.Join(c.runDir(r.ID), "status.json"))
	if err == nil {
		var status runStatusRecord
		if err := protocol.Decode(b, &status); err != nil {
			return err
		}
		r.Status, r.Finished, r.FleetStagesAwaitingNodeSelection = status.Status, status.Finished, status.FleetStagesAwaitingNodeSelection
		r.SecretsRefreshed = status.SecretsRefreshed
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := c.loadSelections(r); err != nil {
		return err
	}
	for i, initial := range r.Entries {
		b, err := os.ReadFile(c.entryPath(r.ID, initial))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		var entry Entry
		if err := protocol.Decode(b, &entry); err != nil {
			return err
		}
		if entry.FleetStage != initial.FleetStage || entry.Node != initial.Node {
			return fmt.Errorf("saved entry for fleet stage %s on node %s names fleet stage %s on node %s instead", initial.FleetStage, initial.Node, entry.FleetStage, entry.Node)
		}
		r.Entries[i] = entry
	}
	return nil
}

func (c Controller) Load(id string, unfinished bool) (*Run, error) {
	lease, err := c.lockHistory(false)
	if err != nil {
		return nil, err
	}
	defer unlockRun(lease)
	files, err := filepath.Glob(filepath.Join(c.State, "runs", "*", "run.json"))
	if err != nil {
		return nil, err
	}
	sort.Sort(sort.Reverse(sort.StringSlice(files)))
	if id != "" {
		var matches []string
		for _, f := range files {
			if savedID := filepath.Base(filepath.Dir(f)); savedID == id || ShortID(savedID) == id {
				matches = append(matches, f)
			}
		}
		if len(matches) > 1 {
			return nil, fmt.Errorf("run ID %s is ambiguous; use the full run ID", id)
		}
		files = matches
	}
	for _, f := range files {
		r, err := c.readRun(f)
		if err != nil {
			return nil, err
		}
		if id == "" && (r.EntryPoint == RunCommandEntryPoint || !c.Selection.Selects(r)) {
			continue
		}
		if unfinished && r.Dryrun {
			if id != "" {
				return nil, fmt.Errorf("cannot resume a dryrun; start a new mmx run with a playbook name")
			}
			continue
		}
		if err := c.loadProgress(r); err != nil {
			return nil, err
		}
		if unfinished && !hasUnfinished(r) {
			return r, ErrNothingToResume
		}
		// Why: a node job keeps the node runner executable it started with, so
		// the other release fails the nodes whose job began and runs the rest
		// itself. Resuming with the earlier release finishes the run but skips
		// one node's last-successful record: a wrong result nobody notices.
		// Refusing here contacts no node and starts no work.
		if unfinished && r.MMXVersion != "" && r.MMXVersion != release.Version {
			return r, fmt.Errorf("this playbook run was started by MMX %s, but this mmx program is version %s. "+
				"A run finished by two MMX releases can leave a node's last-successful record skipped. "+
				"Resume the run with mmx of release %s (such as the previous install, if you kept it), or start a new run with mmx run", r.MMXVersion, release.Version, r.MMXVersion)
		}
		return r, nil
	}
	return nil, fmt.Errorf("no matching saved run")
}

// The status is small and mutable; definitions remain in the initial run file.
type runStatusRecord struct {
	SecretsRefreshed                 bool `json:"secret_inputs_refreshed,omitempty"`
	FleetStagesAwaitingNodeSelection bool `json:"fleet_stages_awaiting_node_selection,omitempty"`

	Status   string     `json:"status"`
	Finished *time.Time `json:"finished,omitempty"`
}

func (c Controller) readRun(path string) (*Run, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var r Run
	if err := protocol.Decode(b, &r); err != nil {
		return nil, err
	}
	if r.ID != filepath.Base(filepath.Dir(path)) {
		return nil, fmt.Errorf("saved playbook run ID does not match the run directory name")
	}
	return &r, nil
}

// readRunWithProgress reads a saved run together with the progress saved
// beside it: its status, selections, and node job entries.
func (c Controller) readRunWithProgress(path string) (*Run, error) {
	r, err := c.readRun(path)
	if err != nil {
		return nil, err
	}
	if err := c.loadProgress(r); err != nil {
		return nil, err
	}
	return r, nil
}

func (c Controller) saveSelection(id string, selection *Selection) error {
	dir := filepath.Join(c.runDir(id), "selections")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	hash := sha256.Sum256([]byte(selection.Path))
	return durable.JSON(filepath.Join(dir, fmt.Sprintf("%x.json", hash)), selection)
}
func (c Controller) loadSelections(r *Run) error {
	if r.Selections == nil {
		r.Selections = map[string]*Selection{}
	}
	files, err := filepath.Glob(filepath.Join(c.runDir(r.ID), "selections", "*.json"))
	if err != nil {
		return err
	}
	for _, path := range files {
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		var sel Selection
		if err := protocol.Decode(b, &sel); err != nil {
			return err
		}
		hash := sha256.Sum256([]byte(sel.Path))
		if filepath.Base(path) != fmt.Sprintf("%x.json", hash) {
			return fmt.Errorf("saved fleet stage selection path does not match the selection file name")
		}
		r.Selections[sel.Path] = &sel
	}
	// run.json holds the entries of the selections known when the run was
	// created. A selection resolved later gets its entries here, pending until
	// loadProgress reads their files.
	known := map[[2]string]bool{}
	for _, en := range r.Entries {
		known[[2]string{en.FleetStage, en.Node}] = true
	}
	for _, sel := range sortedSelections(r.Selections) {
		if !sel.Resolved || sel.Loop {
			continue
		}
		for _, managedNode := range sel.Nodes {
			if !known[[2]string{sel.Path, managedNode}] {
				r.Entries = append(r.Entries, Entry{FleetStage: sel.Path, Node: managedNode, Response: protocol.Response{Status: "pending"}})
			}
		}
	}
	return nil
}
