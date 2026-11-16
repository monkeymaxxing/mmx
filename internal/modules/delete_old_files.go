package modules

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"time"

	"mmx/internal/protocol"

	"golang.org/x/sys/unix"
)

// DeleteOldFiles deletes the regular files in a directory whose names match
// one of the patterns and that were last modified longer ago than
// OlderThan.
type DeleteOldFiles struct {
	Directory    string   `yaml:"directory" json:"directory"`
	NamePatterns []string `yaml:"name_patterns" json:"name_patterns"`
	OlderThan    string   `yaml:"older_than" json:"older_than"`
	// IncludeSubdirectories also deletes in the directories below
	// Directory; it is required, so the task says how deep it deletes.
	IncludeSubdirectories *bool `yaml:"include_subdirectories" json:"include_subdirectories"`
}

// deletedFileExamplesShown is how many deleted files a result names.
const deletedFileExamplesShown = 10

// deleteOldFilesDescription is what mmx describe-module shows for
// delete_old_files.
var deleteOldFilesDescription = Description{
	Name: "delete_old_files", Topic: TopicDirectoriesDownloadsAndReleaseDirectories, Schema: reflect.TypeOf(DeleteOldFiles{}),
	Summary: "deletes the regular files in one directory whose names match a pattern and that were last modified longer ago than a duration, such as rotated logs; follows no symlink and stays on the directory's filesystem; value has deleted_files, deleted_bytes, and deleted_file_examples",
	Fields: []Field{
		{Name: "directory", Required: true, Notes: "absolute, at least two levels deep, such as /var/log/myapp; neither it nor any directory on its path may be a symlink, so the task deletes only below the directory as written; a directory that does not exist is unchanged"},
		{Name: "name_patterns", Required: true, Notes: "at least one shell glob matched on the file name alone, as find -name matches, such as [\"*.gz\", \"*.[0-9]\"]: * and ? also match a leading dot, [!0-9] and [^0-9] match a character not listed; a pattern holds no /"},
		{Name: "older_than", Required: true, Notes: "a duration such as 7d, 36h, or 90m: a file goes when its modification time is longer ago"},
		{Name: "include_subdirectories", Required: true, Notes: "true also deletes in every directory below directory, except below a symlink and on another filesystem, such as a mount; false only directly inside directory; directories themselves are never deleted"},
	},
	Example: "- delete_old_files:\n    directory: /var/log/myapp\n    name_patterns: [\"*.gz\", \"*.[0-9]\"]\n    older_than: 14d\n    include_subdirectories: false\n",
	Results: []string{"status", "change", "reason", "value.deleted_files", "value.deleted_bytes", "value.deleted_file_examples"},
	Dryrun:  "deletes nothing; reports the number and bytes of the files a run would delete and names up to ten of them",
}

// Prepare checks the inputs when the project loads.
func (d *DeleteOldFiles) Prepare(base string) error {
	switch {
	case d.Directory == "":
		return invalid("directory", "delete_old_files.directory is required: an absolute path at least two levels deep, such as /var/log/myapp")
	case len(d.NamePatterns) == 0:
		return invalid("name_patterns", `delete_old_files.name_patterns is required: at least one shell glob matched on the file name, such as ["*.gz"]`)
	case d.OlderThan == "":
		return invalid("older_than", "delete_old_files.older_than is required: a duration such as 7d or 36h")
	case d.IncludeSubdirectories == nil:
		return invalid("include_subdirectories", "delete_old_files.include_subdirectories is required: true also deletes files in subdirectories of directory, false deletes files only directly inside directory")
	}
	if !Dynamic(d.OlderThan) {
		if _, err := ParseDurationWithDays(d.OlderThan); err != nil {
			return invalid("older_than", err.Error())
		}
	}
	for i, pattern := range d.NamePatterns {
		if !Dynamic(pattern) {
			if err := checkNamePattern(pattern); err != nil {
				return invalid(fmt.Sprintf("name_patterns[%d]", i), err.Error())
			}
		}
	}
	return nil
}

// namePattern is the rule of one of name_patterns.
var namePattern = parsed(checkNamePattern)

func checkNamePattern(pattern string) error {
	if pattern == "" || strings.ContainsAny(pattern, "/\x00") {
		return errors.New("a file name pattern must be a nonempty shell glob, such as *.gz, without /; the pattern matches only the file name")
	}
	if _, err := filepath.Match(goGlob(pattern), ""); err != nil {
		return fmt.Errorf("%q is not a glob: %w", pattern, err)
	}
	return nil
}

// goGlob writes a shell glob as filepath.Match reads it: a character class
// that starts with ! is one that starts with ^.
func goGlob(pattern string) string {
	return strings.ReplaceAll(pattern, "[!", "[^")
}

// Arguments lists the inputs that may hold a reference, with their rules.
func (d *DeleteOldFiles) Arguments(arguments *ArgumentList) {
	arguments.Public("directory", &d.Directory, deletablePath)
	for i := range d.NamePatterns {
		arguments.Public(fmt.Sprintf("name_patterns[%d]", i), &d.NamePatterns[i], namePattern)
	}
	arguments.Public("older_than", &d.OlderThan, durationWithDays)
}

// CleanPaths cleans the resolved directory.
func (d *DeleteOldFiles) CleanPaths() {
	d.Directory = filepath.Clean(d.Directory)
}

// ManagedPaths lists the directory the task deletes from; a dryrun cannot
// check a later task's path_exists below it.
func (d *DeleteOldFiles) ManagedPaths() []string {
	return []string{d.Directory}
}

// Run does the module's work on the node.
func (d *DeleteOldFiles) Run(ctx context.Context, task NodeTask) protocol.Result {
	return task.Runtime.DeleteOldFiles(ctx, *d)
}

// oldFilesWalk is one walk of delete_old_files through the directory: it
// opens each directory relative to the one above, follows no link, and
// stays on the directory's filesystem. A run deletes each file as it finds
// it; a dryrun only counts.
type oldFilesWalk struct {
	run      Runtime
	ctx      context.Context
	input    DeleteOldFiles
	patterns []string
	// before is the modification time that a file must be older than.
	before time.Time
	device uint64
	// What the walk deleted, or would delete in a dryrun.
	files    int
	bytes    int64
	examples []string
	// mutated: the walk has passed the mutation boundary.
	mutated                        bool
	skippedSymlinks, skippedMounts int
}

// DeleteOldFiles deletes, or lists in a dryrun, the old files that match.
func (run Runtime) DeleteOldFiles(ctx context.Context, d DeleteOldFiles) protocol.Result {
	olderThan, _ := ParseDurationWithDays(d.OlderThan) // checked when the arguments resolved
	walk := &oldFilesWalk{run: run, ctx: ctx, input: d, before: time.Now().Add(-olderThan)}
	for _, pattern := range d.NamePatterns {
		walk.patterns = append(walk.patterns, goGlob(pattern))
	}
	directory, missing, err := openDirectoryFollowingNoLink(d.Directory)
	var link linkOnThePath
	if errors.As(err, &link) {
		// Another try finds the same link: the task must name another
		// directory.
		r := protocol.ConfigurationError(err)
		r.Change = "unchanged"
		return r
	}
	if err != nil {
		return protocol.Failed(err)
	}
	if directory == nil {
		r := walk.result()
		r.Reason = "the directory " + d.Directory + " does not exist"
		if missing != d.Directory {
			r.Reason += ": " + missing + " does not exist"
		}
		return r
	}
	defer directory.Close()
	if walk.device, err = deviceOf(directory); err != nil {
		return protocol.Failed(err)
	}
	err = walk.directory(directory, d.Directory)
	return finishDeletion(walk.result(), walk.files > 0 && !run.Dryrun, err)
}

// linkOnThePath is a component of delete_old_files' directory that is a
// symlink.
type linkOnThePath string

func (component linkOnThePath) Error() string {
	return string(component) + " is a symlink; delete_old_files deletes only below the directory as written and follows no link on its path: name the directory that the link points to"
}

// openDirectoryFollowingNoLink opens the directory at path, absolute and
// clean, one component after the other from /, and fails when any of them
// is a symlink, so that the walk deletes only below the directory as
// written. It returns no directory, and the first path that is missing,
// when a component does not exist.
func openDirectoryFollowingNoLink(path string) (directory *os.File, missing string, err error) {
	fd, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, "", fmt.Errorf("open /: %w", err)
	}
	opened := "/"
	for _, name := range strings.Split(strings.TrimPrefix(path, "/"), "/") {
		component := filepath.Join(opened, name)
		var st unix.Stat_t
		err := unix.Fstatat(fd, name, &st, unix.AT_SYMLINK_NOFOLLOW)
		switch {
		case errors.Is(err, unix.ENOENT):
			unix.Close(fd)
			return nil, component, nil
		case err != nil:
			unix.Close(fd)
			return nil, "", fmt.Errorf("inspect %s: %w", component, err)
		case uint32(st.Mode)&unix.S_IFMT == unix.S_IFLNK:
			unix.Close(fd)
			return nil, "", linkOnThePath(component)
		case uint32(st.Mode)&unix.S_IFMT != unix.S_IFDIR:
			unix.Close(fd)
			return nil, "", fmt.Errorf("%s is not a directory", component)
		}
		next, err := unix.Openat(fd, name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
		unix.Close(fd)
		if err != nil {
			return nil, "", fmt.Errorf("open %s: %w", component, err)
		}
		var now unix.Stat_t
		if err := unix.Fstat(next, &now); err != nil || now.Dev != st.Dev || now.Ino != st.Ino {
			unix.Close(next)
			return nil, "", fmt.Errorf("path changed during inspection: %s", component)
		}
		fd, opened = next, component
	}
	return os.NewFile(uintptr(fd), path), "", nil
}

// directory deletes the old files directly in dir, at path, and with
// include_subdirectories walks the directories in it.
func (w *oldFilesWalk) directory(dir *os.File, path string) error {
	names, err := dir.Readdirnames(-1)
	if err != nil {
		return fmt.Errorf("read directory %s: %w", path, err)
	}
	slices.Sort(names)
	for _, name := range names {
		if err := w.ctx.Err(); err != nil {
			return err
		}
		entry := filepath.Join(path, name)
		var st unix.Stat_t
		if err := unix.Fstatat(int(dir.Fd()), name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
			if errors.Is(err, unix.ENOENT) {
				continue // deleted since the listing
			}
			return fmt.Errorf("inspect %s: %w", entry, err)
		}
		switch uint32(st.Mode) & unix.S_IFMT {
		case unix.S_IFLNK:
			if w.matches(name) {
				w.skippedSymlinks++
			}
		case unix.S_IFREG:
			if err := w.file(dir, name, entry, &st); err != nil {
				return err
			}
		case unix.S_IFDIR:
			if *w.input.IncludeSubdirectories {
				if err := w.subdirectory(dir, name, entry, &st); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// file deletes one regular file when its name matches and it is old enough.
func (w *oldFilesWalk) file(dir *os.File, name, path string, st *unix.Stat_t) error {
	if !w.matches(name) || !time.Unix(st.Mtim.Unix()).Before(w.before) {
		return nil
	}
	if !w.run.Dryrun {
		if !w.mutated {
			if err := w.run.Mutation(w.input.Directory); err != nil {
				return err
			}
			w.mutated = true
		}
		// The entry is still the file that was inspected: unlinkat removes
		// a name, and another file can have taken it since.
		var now unix.Stat_t
		if err := unix.Fstatat(int(dir.Fd()), name, &now, unix.AT_SYMLINK_NOFOLLOW); err != nil || now.Dev != st.Dev || now.Ino != st.Ino || uint32(now.Mode)&unix.S_IFMT != unix.S_IFREG {
			return fmt.Errorf("path changed during inspection: %s", path)
		}
		if err := unix.Unlinkat(int(dir.Fd()), name, 0); err != nil {
			return fmt.Errorf("delete %s: %w", path, err)
		}
	}
	w.files++
	w.bytes += st.Size
	if len(w.examples) < deletedFileExamplesShown {
		w.examples = append(w.examples, path)
	}
	return nil
}

// subdirectory walks a directory in dir unless it is the root of a mount,
// whatever its filesystem: a bind mount of the same filesystem counts too.
func (w *oldFilesWalk) subdirectory(dir *os.File, name, path string, st *unix.Stat_t) error {
	mounted, err := mountRoot(int(dir.Fd()), name)
	if err != nil {
		return fmt.Errorf("inspect %s: %w", path, err)
	}
	if mounted || uint64(st.Dev) != w.device {
		w.skippedMounts++
		return nil
	}
	fd, err := unix.Openat(int(dir.Fd()), name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	child := os.NewFile(uintptr(fd), path)
	defer child.Close()
	var opened unix.Stat_t
	if err := unix.Fstat(fd, &opened); err != nil {
		return err
	}
	if opened.Dev != st.Dev || opened.Ino != st.Ino {
		return fmt.Errorf("path changed during inspection: %s", path)
	}
	return w.directory(child, path)
}

func (w *oldFilesWalk) matches(name string) bool {
	for _, pattern := range w.patterns {
		if matched, _ := filepath.Match(pattern, name); matched {
			return true
		}
	}
	return false
}

// result reports what the walk deleted, or would delete in a dryrun.
func (w *oldFilesWalk) result() protocol.Result {
	r := protocol.Success("unchanged")
	examples := w.examples
	if examples == nil {
		examples = []string{}
	}
	r.Value = map[string]any{"deleted_files": w.files, "deleted_bytes": w.bytes, "deleted_file_examples": examples}
	d := w.input
	switch {
	case w.files == 0:
		r.Reason = "no file in " + d.Directory + " matches name_patterns and is older than " + d.OlderThan
	case w.run.Dryrun:
		r.Change, r.Reason = "would_change", fmt.Sprintf("would delete %d files totaling %s in %s", w.files, bytesText(w.bytes), d.Directory)
	default:
		r.Change, r.Reason = "changed", fmt.Sprintf("deleted %d files totaling %s in %s", w.files, bytesText(w.bytes), d.Directory)
	}
	if w.skippedSymlinks+w.skippedMounts > 0 {
		r.Reason += fmt.Sprintf("; left alone %d symlinks whose names match and %d mount points, which delete_old_files never follows", w.skippedSymlinks, w.skippedMounts)
	}
	return r
}
