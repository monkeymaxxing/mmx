package modules

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"

	"mmx/internal/assets"
	"mmx/internal/protocol"
)

var (
	procSwapsPath = "/proc/swaps"
	// swapFileUserID and swapFileGroupID own a swap file: root. Tests, which
	// do not run as root, use their own account.
	swapFileUserID, swapFileGroupID = 0, 0
)

const mebibyte = 1 << 20

// SwapFile is a swap file, active now and after a reboot.
type SwapFile struct {
	Path  string `yaml:"path" json:"path"`
	Size  string `yaml:"size,omitempty" json:"size,omitempty"`
	State string `yaml:"state,omitempty" json:"state,omitempty"`
}

// swapStep is one step of a swap_file task: what a dryrun says it would do,
// and what a run says it did.
type swapStep struct {
	would, did string
	do         func() error
}

// SwapFile inspects each step of a swap file and does the ones that are
// missing, in order, so a run that was cut off is finished by the next. A
// new file is written in full under a temporary name, given its swap
// signature, and only then renamed to its path: a file at the path without a
// swap signature is therefore never MMX's, and the task refuses to replace or
// delete it.
func (run Runtime) SwapFile(ctx context.Context, s SwapFile) protocol.Result {
	if err := ValidateSwapFile(s); err != nil {
		return protocol.ConfigurationError(err)
	}
	if r := run.storageTool(ctx, "mkswap", "swapon", "swapoff", "findmnt"); r != nil {
		return *r
	}
	name, temporary := filepath.Base(s.Path), "."+filepath.Base(s.Path)+".mmx-new"
	parent, err := os.OpenRoot(filepath.Dir(s.Path))
	if errors.Is(err, os.ErrNotExist) && s.State != "absent" {
		return run.notYet(fmt.Errorf("the directory %s does not exist yet", filepath.Dir(s.Path)))
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return protocol.Failed(err)
	}
	var info, leftover os.FileInfo
	if parent != nil {
		defer parent.Close()
		if info, err = entryInfoAt(parent, name, false); err != nil {
			return refused(fmt.Errorf("%s: %w", s.Path, err))
		}
		if leftover, err = entryInfoAt(parent, temporary, false); err != nil {
			return refused(fmt.Errorf("%s: %w", filepath.Join(filepath.Dir(s.Path), temporary), err))
		}
	}
	if info != nil {
		isSwap, err := hasSwapSignature(parent, name)
		if err != nil {
			return protocol.Failed(err)
		}
		if !isSwap {
			return refused(fmt.Errorf("%s has no swap signature and may hold data; MMX replaces or deletes only swap files. Move the existing file away by hand, or choose another swap_file.path", s.Path))
		}
	}
	active, err := swapActive(s.Path)
	if err != nil {
		return protocol.Failed(err)
	}
	current, err := readFstab()
	if err != nil {
		return protocol.Failed(err)
	}
	picks := func(l fstabLine) bool { return l.source == s.Path }
	want := &fstabLine{source: s.Path, mountPoint: "none", filesystemType: "swap", options: "defaults", dump: "0", checkOrder: "0"}
	if s.State == "absent" {
		want = nil
	}
	nextFstab, err := editFstab(current, picks, func(l fstabLine) bool { return l.filesystemType == "swap" }, want, s.Path)
	if err != nil {
		return refused(err)
	}

	removeEntry := func(entry string) func() error {
		return func() error {
			if err := run.Mutation(filepath.Join(filepath.Dir(s.Path), entry)); err != nil {
				return err
			}
			if err := parent.Remove(entry); err != nil {
				return err
			}
			return syncDirectory(parent)
		}
	}
	switchOff := swapStep{"switch off " + s.Path, "switched off " + s.Path, func() error { return run.swapCommand(ctx, "swapoff", s.Path) }}
	var steps []swapStep
	if leftover != nil {
		steps = append(steps, swapStep{"delete " + temporary + ", an unfinished swap file of an interrupted run", "deleted " + temporary, removeEntry(temporary)})
	}
	if s.State == "absent" {
		if active {
			steps = append(steps, switchOff)
		}
		if nextFstab != current {
			steps = append(steps, swapStep{"remove the swap file's line from " + fstabPath, "removed the swap file's line from " + fstabPath, nil})
		}
		if info != nil {
			steps = append(steps, swapStep{"delete " + s.Path, "deleted " + s.Path, removeEntry(name)})
		}
		return run.swapSteps(ctx, steps, current, nextFstab)
	}

	size, _ := wholeBinarySize(s.Size)
	sizeMiB := size / mebibyte
	whole := info != nil && info.Size() == size && info.Sys().(*syscall.Stat_t).Blocks*512 >= size
	if info != nil && !whole {
		if active {
			steps = append(steps, switchOff)
		}
		steps = append(steps, swapStep{fmt.Sprintf("delete %s, which is not %d MiB written in full", s.Path, sizeMiB), "deleted the old " + s.Path, removeEntry(name)})
	}
	if !whole {
		steps = append(steps, swapStep{fmt.Sprintf("write %s with %d MiB, mode 0600, owned by root, and give it a swap signature", s.Path, sizeMiB),
			fmt.Sprintf("wrote %s with %d MiB and its swap signature", s.Path, sizeMiB),
			func() error { return run.writeSwapFile(ctx, parent, s.Path, temporary, sizeMiB) }})
	} else if metadata := (fileMetadata{0600, swapFileUserID, swapFileGroupID}); !metadata.matches(info) {
		steps = append(steps, swapStep{"set " + s.Path + " to mode 0600, owned by root", "set " + s.Path + " to mode 0600, owned by root", func() error {
			r := run.changeMetadataAt(ctx, parent, s.Path, info, metadata)
			if r.Status != "succeeded" {
				return errors.New(r.Reason)
			}
			return nil
		}})
	}
	if !active || !whole {
		steps = append(steps, swapStep{"switch on " + s.Path, "switched on " + s.Path, func() error { return run.swapCommand(ctx, "swapon", s.Path) }})
	}
	if nextFstab != current {
		verb := "add the swap file's line to "
		if _, ok := lineForSource(current, s.Path); ok {
			verb = "correct the swap file's line in "
		}
		steps = append(steps, swapStep{verb + fstabPath, strings.Replace(strings.Replace(verb, "add", "added", 1), "correct", "corrected", 1) + fstabPath, nil})
	}
	return run.swapSteps(ctx, steps, current, nextFstab)
}

// swapSteps reports the steps in a dryrun and does them in a run. A step
// without a function is the change of /etc/fstab.
func (run Runtime) swapSteps(ctx context.Context, steps []swapStep, current, nextFstab string) protocol.Result {
	if len(steps) == 0 {
		return protocol.Success("unchanged")
	}
	if run.Dryrun {
		r := protocol.Success("would_change")
		if nextFstab != current {
			r = run.writeFstab(ctx, nextFstab)
			r.Change = "would_change"
		}
		var would []string
		for _, step := range steps {
			would = append(would, step.would)
		}
		r.Reason = "would " + strings.Join(would, ", then ")
		return r
	}
	var done []string
	var fileChange *protocol.FileChange
	for _, step := range steps {
		if err := ctx.Err(); err != nil {
			return failedAfter(err, done)
		}
		if step.do == nil {
			r := run.writeFstab(ctx, nextFstab)
			fileChange = r.FileChange
			if r.Status != "succeeded" {
				if len(done) > 0 {
					r.Change = "changed"
					r.Reason += "; already done: " + strings.Join(done, ", ")
				}
				return r
			}
		} else if err := step.do(); err != nil {
			return failedAfter(err, done)
		}
		done = append(done, step.did)
	}
	r := protocol.Success("changed")
	r.Reason = strings.Join(done, ", ")
	r.FileChange = fileChange
	return r
}

// writeSwapFile writes a new swap file of sizeMiB under its temporary name,
// mode 0600 and owned by root from the start, by writing every byte so that
// it has no holes, gives it its swap signature, and renames it to path.
func (run Runtime) writeSwapFile(ctx context.Context, parent *os.Root, path, temporary string, sizeMiB int64) (err error) {
	directory := filepath.Dir(path)
	space, err := assets.WritableSpace(directory)
	if err != nil {
		return err
	}
	if space < int64(sizeMiB)*mebibyte {
		return fmt.Errorf("swap file %s needs %d MiB, but the filesystem has only %d MiB available after reserving %d%% of the filesystem as free space", path, sizeMiB, space/mebibyte, assets.KeepFreePercent)
	}
	if err := run.Mutation(path); err != nil {
		return err
	}
	var f *os.File
	if err := createLabeled(ctx, path, selinuxFile, func() (err error) {
		f, err = parent.OpenFile(temporary, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		return err
	}); err != nil {
		return err
	}
	// A swap file that could not be finished is deleted, so it does not keep
	// its space; one cut off by a crash is deleted by the next run.
	defer func() {
		if err != nil {
			_ = parent.Remove(temporary)
		}
	}()
	err = (fileMetadata{0600, swapFileUserID, swapFileGroupID}).applyTo(f)
	if err == nil {
		err = noCopyOnWrite(f)
	}
	zeros := make([]byte, mebibyte)
	for i := int64(0); i < sizeMiB && err == nil; i++ {
		if err = ctx.Err(); err == nil {
			_, err = f.Write(zeros)
		}
	}
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	full := filepath.Join(directory, temporary)
	out := run.RunCommandThatChangesTheNode(ctx, []string{"mkswap", full}, "")
	if out.Error != "" || out.ExitCode != 0 {
		return fmt.Errorf("mkswap %s failed: %s", full, strings.TrimSpace(out.Error+" "+out.StandardError))
	}
	if err = parent.Rename(temporary, filepath.Base(path)); err != nil {
		return err
	}
	return syncDirectory(parent)
}

// applyTo gives an open file this owner, group, and mode.
func (m fileMetadata) applyTo(f *os.File) error {
	_, err := m.apply(f)
	return err
}

// swapCommand runs swapon or swapoff on the swap file.
func (run Runtime) swapCommand(ctx context.Context, program, path string) error {
	out := run.RunCommandThatChangesTheNode(ctx, []string{program, path}, "")
	if out.Error != "" || out.ExitCode != 0 {
		return fmt.Errorf("%s %s failed: %s", program, path, strings.TrimSpace(out.Error+" "+out.StandardError))
	}
	return nil
}

// swapPageSizes are the page sizes Linux uses; mkswap writes the signature
// in the last ten bytes of the first page.
var swapPageSizes = []int64{4096, 8192, 16384, 32768, 65536}

// hasSwapSignature reports whether the file starts with a swap header.
func hasSwapSignature(parent *os.Root, name string) (bool, error) {
	f, err := openRegularAt(parent, name)
	if err != nil {
		return false, err
	}
	defer f.Close()
	signature := make([]byte, 10)
	for _, pageSize := range append(slices.Clone(swapPageSizes), int64(os.Getpagesize())) {
		if n, _ := f.ReadAt(signature, pageSize-10); n == 10 && string(signature) == "SWAPSPACE2" {
			return true, nil
		}
	}
	return false, nil
}

// swapActive reports whether /proc/swaps lists path.
func swapActive(path string) (bool, error) {
	f, err := os.Open(procSwapsPath)
	if err != nil {
		return false, err
	}
	defer f.Close()
	// The kernel lists the path it opened, with every link resolved.
	resolved := path
	if directory, err := filepath.EvalSymlinks(filepath.Dir(path)); err == nil {
		resolved = filepath.Join(directory, filepath.Base(path))
	}
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) > 0 && fields[0] != "Filename" {
			if listed := fstabUnescape(fields[0]); listed == path || listed == resolved {
				return true, nil
			}
		}
	}
	return false, scanner.Err()
}

// lineForSource returns the one line of text whose source is path.
func lineForSource(text, path string) (fstabLine, bool) {
	for _, line := range strings.Split(text, "\n") {
		if l, ok := parseFstabLine(line); ok && l.source == path {
			return l, true
		}
	}
	return fstabLine{}, false
}

func ValidateSwapFile(s SwapFile) error {
	if err := validatePresence(s.State); err != nil {
		return invalid("state", "swap_file.state must be present or absent")
	}
	if s.State == "absent" {
		if s.Size != "" {
			return invalid("size", "swap_file with state: absent accepts only path and state")
		}
		return nil
	}
	if s.Size == "" {
		return invalid("size", "swap_file.size is required: a whole number above 0 and MiB, GiB, or TiB, such as 4GiB")
	}
	if !Dynamic(s.Size) {
		if _, err := wholeBinarySize(s.Size); err != nil {
			return invalid("size", err.Error())
		}
	}
	return nil
}

// swapFilePath is the rule for a swap file's path: absolute, below /, and
// one field of /etc/fstab once escaped.
var swapFilePath = rule("path must be an absolute path of a file, such as /swapfile, without null bytes, line breaks, or .. parts", func(s string) bool {
	return filepath.IsAbs(s) && filepath.Clean(s) != "/" && !strings.ContainsAny(s, "\x00\n") && withoutParentPart(s)
})

// swapFileDescription is what mmx describe-module shows for swap_file.
var swapFileDescription = Description{
	Name: "swap_file", Topic: TopicSwapFilesystemsAndMounts, Schema: reflect.TypeOf(SwapFile{}),
	Summary: "a swap file, active now and after a reboot, through mkswap, swapon, swapoff, and a line in /etc/fstab",
	Fields: []Field{
		{Name: "path", Required: true, Notes: "absolute path of the swap file, such as /swapfile; its directory must exist; an existing file without a swap signature is never replaced or deleted"},
		{Name: "size", Required: true, Notes: "required unless state: absent: a whole number above 0 and MiB, GiB, or TiB, such as 4GiB; every byte is written, so the file has no holes; a swap file of another size is switched off, deleted, and written again"},
		{Name: "state", Values: presenceStates, Default: "present", Notes: "absent switches the file off, removes its line from /etc/fstab, deletes it, and accepts only path"},
	},
	Example: "- swap_file:\n    path: /swapfile\n    size: 4GiB\n",
	Results: resultBasic,
	Dryrun:  "supported; the reason lists the missing steps and the result shows the difference of /etc/fstab; not checked while the directory or a program is missing",
}

// Prepare checks the inputs when the project loads.
func (s *SwapFile) Prepare(base string) error {
	return ValidateSwapFile(*s)
}

// Arguments lists the inputs that may hold a reference, with their rules.
func (s *SwapFile) Arguments(arguments *ArgumentList) {
	arguments.Public("path", &s.Path, swapFilePath)
	if s.Size != "" {
		arguments.Public("size", &s.Size, parsed(func(text string) error { _, err := wholeBinarySize(text); return err }))
	}
}

// CleanPaths cleans the resolved path.
func (s *SwapFile) CleanPaths() {
	s.Path = filepath.Clean(s.Path)
}

// Run does the module's work on the node.
func (s *SwapFile) Run(ctx context.Context, task NodeTask) protocol.Result {
	return task.Runtime.SwapFile(ctx, *s)
}

// ManagedPaths lists the paths the task manages, as written; a dryrun cannot
// check a later task's path_exists on them.
func (s *SwapFile) ManagedPaths() []string {
	return []string{s.Path, fstabPath}
}

// wholeBinarySize reads a size in the whole binary units that partition and
// LVM inputs use too: swap_file's size, systemd_journal_vacuum's
// keep_at_most_size.
func wholeBinarySize(text string) (int64, error) {
	match := wholeBinarySizePattern.FindStringSubmatch(text)
	if match != nil {
		unit := map[string]int64{"MiB": mebibyte, "GiB": 1024 * mebibyte, "TiB": 1024 * 1024 * mebibyte}[match[2]]
		number, err := strconv.ParseInt(match[1], 10, 64)
		if err == nil && number > 0 && number <= (1<<63-1)/unit {
			return number * unit, nil
		}
	}
	return 0, fmt.Errorf("size must be a whole number above 0 and MiB, GiB, or TiB, such as 4GiB, within the node's signed 64-bit file size")
}

var wholeBinarySizePattern = regexp.MustCompile(`^([0-9]+)(MiB|GiB|TiB)$`)
