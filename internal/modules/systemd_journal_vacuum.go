package modules

import (
	"cmp"
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	"mmx/internal/protocol"

	"golang.org/x/sys/unix"
)

// SystemdJournalVacuum deletes archived files of systemd's journal with
// journalctl --vacuum-size and --vacuum-time. At least one of the two
// limits is written.
type SystemdJournalVacuum struct {
	KeepAtMostSize string `yaml:"keep_at_most_size,omitempty" json:"keep_at_most_size,omitempty"`
	KeepNewerThan  string `yaml:"keep_newer_than,omitempty" json:"keep_newer_than,omitempty"`
}

// systemdJournalVacuumDescription is what mmx describe-module shows for
// systemd_journal_vacuum.
var systemdJournalVacuumDescription = Description{
	Name: "systemd_journal_vacuum", Topic: TopicAccountsAndServiceUnits, Schema: reflect.TypeOf(SystemdJournalVacuum{}),
	Summary: "deletes archived files of systemd's journal with journalctl --vacuum-size and --vacuum-time, the oldest first; never the journal's active files; value has freed_bytes and deleted_archived_journal_files",
	Fields: []Field{
		{Name: "keep_at_most_size", Notes: "a positive whole number followed by MiB, GiB, or TiB, such as 500MiB. Deletes the oldest archived files from each journal directory until journal files fit within the size limit, counting active files too. Requires keep_at_most_size, keep_newer_than, or a value for each setting"},
		{Name: "keep_newer_than", Notes: "a duration such as 14d, 12h, or 30m: every archived file whose first entry is older is deleted; at least one of keep_at_most_size and keep_newer_than"},
	},
	Example: "- systemd_journal_vacuum:\n    keep_at_most_size: 500MiB\n    keep_newer_than: 14d\n",
	Results: []string{"status", "change", "reason", "value.freed_bytes", "value.deleted_archived_journal_files"},
	Dryrun:  "deletes nothing; applies journald's own rule to the names, sizes, and times of the archived files and reports what a run would delete and free; a file that journald archives or deletes between the dryrun and the run changes the answer",
}

// Prepare checks the inputs when the project loads.
func (v *SystemdJournalVacuum) Prepare(base string) error {
	if v.KeepAtMostSize == "" && v.KeepNewerThan == "" {
		return invalid("keep_at_most_size", "systemd_journal_vacuum requires keep_at_most_size, keep_newer_than, or a value for each setting: the maximum disk space for journal files and the maximum age of archived journal files")
	}
	if v.KeepAtMostSize != "" && !Dynamic(v.KeepAtMostSize) {
		if _, err := wholeBinarySize(v.KeepAtMostSize); err != nil {
			return invalid("keep_at_most_size", err.Error())
		}
	}
	if v.KeepNewerThan != "" && !Dynamic(v.KeepNewerThan) {
		if _, err := ParseDurationWithDays(v.KeepNewerThan); err != nil {
			return invalid("keep_newer_than", err.Error())
		}
	}
	return nil
}

// Arguments lists the inputs that may hold a reference, with their rules.
func (v *SystemdJournalVacuum) Arguments(arguments *ArgumentList) {
	if v.KeepAtMostSize != "" {
		arguments.Public("keep_at_most_size", &v.KeepAtMostSize, parsed(func(text string) error { _, err := wholeBinarySize(text); return err }))
	}
	if v.KeepNewerThan != "" {
		arguments.Public("keep_newer_than", &v.KeepNewerThan, durationWithDays)
	}
}

// Run does the module's work on the node.
func (v *SystemdJournalVacuum) Run(ctx context.Context, task NodeTask) protocol.Result {
	return task.Runtime.SystemdJournalVacuum(ctx, *v)
}

// The journal's directories that journalctl's vacuum works on: each root,
// and in it the directory of this machine, named by its ID. Tests replace
// both.
var (
	journalRootDirectories = []string{"/run/log/journal", "/var/log/journal"}
	machineIDFile          = "/etc/machine-id"
)

// SystemdJournalVacuum runs journalctl's vacuum with the task's limits and
// reports the archived files that are gone afterwards. A dryrun reports
// what journald's own rule would delete (journalDirectory.wouldDelete).
func (run Runtime) SystemdJournalVacuum(ctx context.Context, v SystemdJournalVacuum) protocol.Result {
	program, err := lookPath(ctx, "journalctl")
	if err != nil {
		return run.notYet(fmt.Errorf("systemd_journal_vacuum: %w", missingProgramError("journalctl")))
	}
	var maxBytes int64
	var keepNewerThan time.Duration
	if v.KeepAtMostSize != "" {
		maxBytes, _ = wholeBinarySize(v.KeepAtMostSize) // checked when the arguments resolved
	}
	if v.KeepNewerThan != "" {
		keepNewerThan, _ = ParseDurationWithDays(v.KeepNewerThan)
	}
	before, err := readJournalDirectories()
	if err != nil {
		return protocol.Failed(fmt.Errorf("systemd_journal_vacuum: %w", err))
	}
	if run.Dryrun {
		var deleted []journalFile
		now := time.Now()
		for _, directory := range before {
			deleted = append(deleted, directory.wouldDelete(maxBytes, keepNewerThan, now)...)
		}
		return journalVacuumResult(true, deleted, before)
	}
	if err := run.Mutation("archived journal files"); err != nil {
		return protocol.Failed(err)
	}
	var args []string
	if maxBytes > 0 {
		args = append(args, "--vacuum-size="+strconv.FormatInt(maxBytes, 10))
	}
	if keepNewerThan > 0 {
		args = append(args, "--vacuum-time="+strconv.FormatInt(int64(keepNewerThan/time.Second), 10)+"s")
	}
	vacuumed := Command(ctx, nil, "/", program, args...)
	after, err := readJournalDirectories()
	if err != nil {
		r := protocol.Failed(fmt.Errorf("systemd_journal_vacuum: after journalctl %s: %w", strings.Join(args, " "), err))
		r.Change = "unknown"
		return r
	}
	gone := map[string]bool{}
	for _, directory := range before {
		for _, file := range directory.files {
			gone[file.path] = true
		}
	}
	for _, directory := range after {
		for _, file := range directory.files {
			delete(gone, file.path)
		}
	}
	var deleted []journalFile
	for _, directory := range before {
		for _, file := range directory.files {
			if gone[file.path] {
				deleted = append(deleted, file)
			}
		}
	}
	result := journalVacuumResult(false, deleted, after)
	if vacuumed.Status != "succeeded" {
		result.Status = "failed"
		result.Reason = fmt.Sprintf("systemd_journal_vacuum: journalctl %s: %s; %s", strings.Join(args, " "), failureTextFromStandardError(vacuumed), result.Reason)
	}
	return result
}

// journalVacuumResult reports the archived journal files that a vacuum
// deleted, or would delete in a dryrun, and what that frees; the directories
// are the journal's as it is afterwards, or now in a dryrun.
func journalVacuumResult(dryrun bool, deleted []journalFile, directories []journalDirectory) protocol.Result {
	var freed int64
	for _, file := range deleted {
		freed += file.bytes
	}
	r := protocol.Success("unchanged")
	r.Value = map[string]any{"freed_bytes": freed, "deleted_archived_journal_files": len(deleted)}
	if len(deleted) == 0 {
		var kept int64
		for _, directory := range directories {
			kept += directory.bytes()
		}
		r.Reason = "no archived journal file is beyond the limits; the journal's files take " + bytesText(kept)
		return r
	}
	r.Change, r.Reason = "changed", fmt.Sprintf("deleted %d archived journal files and freed %s", len(deleted), bytesText(freed))
	if dryrun {
		r.Change, r.Reason = "would_change", fmt.Sprintf("would delete %d archived journal files and free %s", len(deleted), bytesText(freed))
	}
	return r
}

// journalFile is a file of systemd's journal that journalctl's vacuum may
// delete: an archived file, which journald names
// <name>@<sequence ID>-<first sequence number>-<first entry's time>.journal,
// or a file that journald set aside as damaged,
// <name>@<time>-<random number>.journal~.
type journalFile struct {
	path string
	// bytes is what the file takes on disk, as journald counts it: 512
	// bytes for each allocated block.
	bytes int64
	// sequenceID and sequenceNumber order the files of one sequence, in
	// which journald deletes them; a damaged file has neither.
	sequenceID     string
	sequenceNumber uint64
	hasSequence    bool
	// firstEntry is the time, in microseconds since 1970, that journald
	// compares with --vacuum-time: the earliest of the time in the name and
	// the file's own times.
	firstEntry uint64
	// empty: the file holds no entry. journald deletes such a file in every
	// vacuum, whatever the limits.
	empty bool
}

// journalDirectory is one directory that journalctl's vacuum works on.
type journalDirectory struct {
	path string
	// activeBytes is what the files that journald writes now, and journal
	// files whose names journald does not read, take on disk: they count
	// toward --vacuum-size and are never deleted.
	activeBytes int64
	// files are the files that the vacuum may delete, in journald's order:
	// within one sequence by sequence number, otherwise by time.
	files []journalFile
}

// bytes is what the directory's journal files take on disk.
func (d journalDirectory) bytes() int64 {
	total := d.activeBytes
	for _, file := range d.files {
		total += file.bytes
	}
	return total
}

// wouldDelete is the rule of journald's journal_directory_vacuum, which
// journalctl --vacuum-size and --vacuum-time apply to each directory: every
// empty file goes; then, oldest first, each file while the directory's
// journal files together take more than maxBytes, or while the file's first
// entry is older than keepNewerThan. A limit of 0 is not given.
func (d journalDirectory) wouldDelete(maxBytes int64, keepNewerThan time.Duration, now time.Time) []journalFile {
	var deleted, kept []journalFile
	total := d.activeBytes
	for _, file := range d.files {
		if file.empty {
			deleted = append(deleted, file)
			continue
		}
		kept = append(kept, file)
		total += file.bytes
	}
	var oldestKept uint64
	if keepNewerThan > 0 {
		if limit := now.UnixMicro() - keepNewerThan.Microseconds(); limit > 0 {
			oldestKept = uint64(limit)
		}
	}
	for _, file := range kept {
		if (keepNewerThan <= 0 || file.firstEntry >= oldestKept) && (maxBytes <= 0 || total <= maxBytes) {
			break
		}
		deleted = append(deleted, file)
		total = max(total-file.bytes, 0)
	}
	return deleted
}

// readJournalDirectories reads the directories that journalctl's vacuum
// works on and that exist.
func readJournalDirectories() ([]journalDirectory, error) {
	var paths []string
	machine := ""
	if text, err := os.ReadFile(machineIDFile); err == nil {
		if id := strings.TrimSpace(string(text)); isHexadecimal(id, 32) {
			machine = id
		}
	}
	for _, root := range journalRootDirectories {
		paths = append(paths, root)
		if machine != "" {
			paths = append(paths, filepath.Join(root, machine))
		}
	}
	var directories []journalDirectory
	for _, path := range paths {
		directory, err := readJournalDirectory(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		directories = append(directories, directory)
	}
	return directories, nil
}

// readJournalDirectory reads one directory as journald's vacuum reads it.
func readJournalDirectory(path string) (journalDirectory, error) {
	directory := journalDirectory{path: path}
	entries, err := os.ReadDir(path)
	if err != nil {
		return directory, err
	}
	for _, entry := range entries {
		file := filepath.Join(path, entry.Name())
		var st unix.Stat_t
		if err := unix.Lstat(file, &st); err != nil {
			if errors.Is(err, unix.ENOENT) {
				continue // journald deleted it since
			}
			return directory, fmt.Errorf("inspect %s: %w", file, err)
		}
		if uint32(st.Mode)&unix.S_IFMT != unix.S_IFREG {
			continue
		}
		bytes := 512 * st.Blocks
		found, ok := parseJournalFileName(entry.Name())
		if !ok {
			if strings.HasSuffix(entry.Name(), ".journal") || strings.HasSuffix(entry.Name(), ".journal~") {
				directory.activeBytes += bytes
			}
			continue
		}
		found.path, found.bytes = file, bytes
		if found.empty, err = journalFileIsEmpty(file); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return directory, err
		}
		// The time in the name is that of the file's first entry; the file
		// can be older still, as its own times may say.
		for _, t := range []unix.Timespec{st.Ctim, st.Atim, st.Mtim} {
			if t.Sec > 0 || t.Nsec > 0 {
				found.firstEntry = min(found.firstEntry, uint64(t.Nano()/1000))
			}
		}
		directory.files = append(directory.files, found)
	}
	slices.SortStableFunc(directory.files, compareJournalFiles)
	return directory, nil
}

// parseJournalFileName reads the name of a file that the vacuum may delete:
// ok is false for a file that journald writes now, or that it does not know.
func parseJournalFileName(name string) (file journalFile, ok bool) {
	hexadecimal := func(text string) (uint64, bool) {
		n, err := strconv.ParseUint(text, 16, 64)
		return n, err == nil && isHexadecimal(text, 16)
	}
	if base, archived := strings.CutSuffix(name, ".journal"); archived {
		// <name>@<32 digits>-<16 digits>-<16 digits>
		at := len(base) - 16 - 1 - 16 - 1 - 32 - 1
		if at < 0 || base[at] != '@' || base[at+33] != '-' || base[at+50] != '-' || !isHexadecimal(base[at+1:at+33], 32) {
			return file, false
		}
		sequence, sequenceOK := hexadecimal(base[at+34 : at+50])
		first, firstOK := hexadecimal(base[at+51:])
		return journalFile{sequenceID: base[at+1 : at+33], sequenceNumber: sequence, hasSequence: true, firstEntry: first}, sequenceOK && firstOK
	}
	if base, damaged := strings.CutSuffix(name, ".journal~"); damaged {
		// <name>@<16 digits>-<16 digits>
		at := len(base) - 16 - 1 - 16 - 1
		if at < 0 || base[at] != '@' || base[at+17] != '-' {
			return file, false
		}
		first, firstOK := hexadecimal(base[at+1 : at+17])
		_, randomOK := hexadecimal(base[at+18:])
		return journalFile{firstEntry: first}, firstOK && randomOK
	}
	return file, false
}

// compareJournalFiles is journald's order of deletion.
func compareJournalFiles(a, b journalFile) int {
	if a.hasSequence && b.hasSequence && a.sequenceID == b.sequenceID {
		return cmp.Compare(a.sequenceNumber, b.sequenceNumber)
	}
	if c := cmp.Compare(a.firstEntry, b.firstEntry); c != 0 {
		return c
	}
	if a.hasSequence && b.hasSequence {
		return strings.Compare(a.sequenceID, b.sequenceID)
	}
	return strings.Compare(filepath.Base(a.path), filepath.Base(b.path))
}

// journalFileIsEmpty reports whether a journal file holds no entry: the
// number of entries in its header is 0, or the file is too short to have
// one, as journald decides.
func journalFileIsEmpty(path string) (bool, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if err != nil {
		return false, err
	}
	defer f.Close()
	// The number of entries is the 64-bit little-endian number at byte 152
	// of the header.
	var entries [8]byte
	if _, err := f.ReadAt(entries[:], 152); err != nil {
		if errors.Is(err, io.EOF) {
			return true, nil
		}
		return false, err
	}
	return binary.LittleEndian.Uint64(entries[:]) == 0, nil
}

// isHexadecimal reports text of exactly digits hexadecimal digits.
func isHexadecimal(text string, digits int) bool {
	_, err := hex.DecodeString(text)
	return len(text) == digits && err == nil
}
