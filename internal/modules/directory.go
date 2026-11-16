package modules

import (
	"context"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"

	"mmx/internal/protocol"
	"mmx/internal/value"

	"golang.org/x/sys/unix"
)

func (run Runtime) Directory(ctx context.Context, d Directory, s value.Scope) (result protocol.Result) {
	if err := ValidateDirectory(d); err != nil {
		return protocol.ConfigurationError(err)
	}
	if d.Source != "" {
		return run.copyDirectory(ctx, d, s)
	}
	if d.Deletes() {
		// A missing directory that delete_everything_inside_except names is
		// created below, as any present directory is, and holds nothing to
		// delete.
		_, err := os.Lstat(d.Path)
		if d.State == "absent" || !errors.Is(err, os.ErrNotExist) {
			return run.removeDirectory(ctx, d)
		}
	}
	// Resolve accounts and parse modes once, not once per directory entry,
	// and before missing parent directories are created.
	metadata, err := metadataFor(nil, 0755, d.Mode, d.Owner, d.Group)
	if err != nil {
		return run.accountFailure(err)
	}
	files, err := metadataFor(nil, 0644, d.FileMode, "", "")
	if err != nil {
		return protocol.ConfigurationError(err)
	}
	// The directory is created and opened within its pinned parent.
	parent, createdParents, r := run.openParent(ctx, d)
	if r != nil {
		return *r
	}
	if parent != nil {
		defer parent.Close()
	}
	name := filepath.Base(d.Path)
	if d.Path == string(filepath.Separator) {
		name = "."
	}
	var before os.FileInfo
	if parent != nil {
		before, err = parent.Lstat(name)
		if err != nil && !os.IsNotExist(err) {
			return protocol.Failed(err)
		}
	}
	if before != nil && !before.IsDir() {
		return protocol.Failed(fmt.Errorf("directory path is not a directory: %s", d.Path))
	}
	summary := &protocol.DirectoryChange{Path: d.Path, CreatedParentDirectories: createdParents}
	walk := directoryWalk{run: run, ctx: ctx, input: d, metadata: metadata, fileMode: files.mode, summary: summary}
	defer func() {
		result.DirectoryChange = summary
		switch {
		case summary.Applied > 0:
			result.Change = "changed"
		case run.Dryrun && summary.Changes > 0:
			result.Change = "would_change"
		default:
			result.Change = "unchanged"
		}
	}()
	if err := ctx.Err(); err != nil {
		return protocol.Failed(err)
	}
	created := before == nil
	if created {
		summary.Directories = 1
		walk.note(d.Path, nil, metadata, "directory")
		if run.Dryrun {
			return protocol.Success("would_change")
		}
		if err := run.Mutation(d.Path); err != nil {
			return protocol.Failed(err)
		}
		// Create only the named root; finish its requested metadata after traversal.
		if err := createLabeled(ctx, d.Path, selinuxDirectory, func() error { return parent.Mkdir(name, 0700) }); err != nil {
			return protocol.Failed(err)
		}
		summary.Applied = 1
		before, err = parent.Lstat(name)
		if err != nil {
			return protocol.Failed(err)
		}
	}
	pinned, err := openDirectoryAt(parent, name, before)
	if err != nil {
		return protocol.Failed(err)
	}
	defer pinned.Close()
	root, err := pinned.Open(".")
	if err != nil {
		return protocol.Failed(err)
	}
	defer root.Close()
	var stat unix.Stat_t
	if err := unix.Fstat(int(root.Fd()), &stat); err != nil {
		return protocol.Failed(err)
	}
	walk.device = uint64(stat.Dev)
	if err := walk.directory(root, d.Path, &stat, created); err != nil {
		return protocol.Failed(err)
	}
	if err := sameFileAt(parent, name, before); err != nil {
		return protocol.Failed(err)
	}
	return protocol.Success("unchanged")
}

// openParent opens the parent directory of d.Path. A missing parent is a
// missing prerequisite, unless an earlier node task of the dryrun would
// create it, or d sets create_missing_parent_directories: the missing
// parents are then created and returned. A dryrun returns no parent for a
// missing one; the directories a run would create are returned all the same.
func (run Runtime) openParent(ctx context.Context, d Directory) (*os.Root, []string, *protocol.Result) {
	parent, err := os.OpenRoot(filepath.Dir(d.Path))
	switch {
	case err == nil:
		return parent, nil, nil
	case !os.IsNotExist(err):
		r := protocol.Failed(err)
		return nil, nil, &r
	case d.CreateMissingParentDirectories:
		parent, created, err := run.createMissingParentDirectories(ctx, filepath.Dir(d.Path))
		if err != nil {
			r := protocol.Failed(err)
			return nil, nil, &r
		}
		return parent, created, nil
	}
	if r := run.missingParent(d.Path); r != nil {
		r.Reason += "; or set create_missing_parent_directories: true on this directory node task"
		return nil, nil, r
	}
	return nil, nil, nil
}

// createMissingParentDirectories creates the missing directories of the
// path parent, as mkdir -p does: each is owned by root, the account that the
// node program runs as, with permissions_mode 0755. Links in the part of the
// path that exists are followed, as mkdir -p follows them; each new directory
// is created and opened within the one before it. It returns the last one
// opened and the directories it created. A dryrun creates nothing, returns
// no directory, and lists the directories that a run would create.
func (run Runtime) createMissingParentDirectories(ctx context.Context, parent string) (*os.Root, []string, error) {
	var missing []string
	existing := parent
	for {
		_, err := os.Stat(existing)
		if !os.IsNotExist(err) {
			// A file where a directory belongs fails here: not a directory.
			if err != nil {
				return nil, nil, err
			}
			break
		}
		missing = append([]string{existing}, missing...)
		existing = filepath.Dir(existing)
	}
	if run.Dryrun {
		return nil, missing, nil
	}
	metadata, err := metadataFor(nil, 0755, "", "", "")
	if err != nil {
		return nil, nil, err
	}
	root, err := os.OpenRoot(existing)
	if err != nil {
		return nil, nil, err
	}
	var created []string
	for _, path := range missing {
		next, err := run.createParentDirectory(ctx, root, path, metadata)
		root.Close()
		if err != nil && created != nil {
			return nil, nil, fmt.Errorf("created the parent directories %s, then: %w", strings.Join(created, ", "), err)
		}
		if err != nil {
			return nil, nil, err
		}
		root = next
		created = append(created, path)
	}
	return root, created, nil
}

// createParentDirectory creates the directory path inside root, private
// until it has its owner and permissions, and opens it.
func (run Runtime) createParentDirectory(ctx context.Context, root *os.Root, path string, metadata fileMetadata) (*os.Root, error) {
	if err := run.Mutation(path); err != nil {
		return nil, err
	}
	name := filepath.Base(path)
	if err := createLabeled(ctx, path, selinuxDirectory, func() error { return root.Mkdir(name, 0700) }); err != nil {
		return nil, fmt.Errorf("create the parent directory %s: %w", path, err)
	}
	next, err := root.OpenRoot(name)
	if err != nil {
		return nil, fmt.Errorf("open the parent directory %s: %w", path, err)
	}
	file, err := next.Open(".")
	if err == nil {
		_, err = metadata.apply(file)
		file.Close()
	}
	if err != nil {
		next.Close()
		return nil, fmt.Errorf("set the owner and permissions of the parent directory %s: %w", path, err)
	}
	return next, nil
}

type directoryWalk struct {
	run      Runtime
	ctx      context.Context
	input    Directory
	metadata fileMetadata
	fileMode os.FileMode
	device   uint64
	summary  *protocol.DirectoryChange
}

func (w *directoryWalk) wanted(st *unix.Stat_t, directory bool) fileMetadata {
	m := fileMetadata{mode: permissionsFromNative(uint32(st.Mode)), uid: int(st.Uid), gid: int(st.Gid)}
	if w.input.Owner != "" {
		m.uid = w.metadata.uid
	}
	if w.input.Group != "" {
		m.gid = w.metadata.gid
	}
	if directory && w.input.Mode != "" {
		m.mode = w.metadata.mode
	}
	if !directory && w.input.FileMode != "" {
		m.mode = w.fileMode
	}
	return m
}
func matchesStat(m fileMetadata, st *unix.Stat_t) bool {
	return nativePermissions(m.mode) == uint32(st.Mode)&07777 && m.uid == int(st.Uid) && m.gid == int(st.Gid)
}
func (w *directoryWalk) note(path string, before *unix.Stat_t, wanted fileMetadata, kind string) {
	w.summary.Changes++
	if len(w.summary.Examples) >= 10 {
		return
	}
	next := &protocol.FileState{Kind: kind, Mode: fmt.Sprintf("%04o", nativePermissions(wanted.mode)), UID: wanted.uid, GID: wanted.gid}
	var old *protocol.FileState
	action := "create"
	if before != nil {
		action = "update"
		old = &protocol.FileState{Kind: kind, Mode: fmt.Sprintf("%04o", uint32(before.Mode)&07777), UID: int(before.Uid), GID: int(before.Gid)}
		if kind == "file" {
			old.Size = before.Size
			next.Size = before.Size
		}
	}
	w.summary.Examples = append(w.summary.Examples, protocol.FileChange{Path: path, Action: action, Before: old, After: next})
}

func (w *directoryWalk) directory(file *os.File, path string, st *unix.Stat_t, created bool) error {
	if err := w.ctx.Err(); err != nil {
		return err
	}
	wanted := w.wanted(st, true)
	needsChange := !matchesStat(wanted, st)
	if created {
		wanted = w.metadata
		needsChange = true
	} else {
		w.summary.Directories++
		if needsChange {
			w.note(path, st, wanted, "directory")
		}
	}
	if w.input.Recurse {
		for {
			entries, err := file.ReadDir(256)
			if err != nil && err != io.EOF {
				return fmt.Errorf("read directory %s: %w", path, err)
			}
			for _, entry := range entries {
				if err := w.entry(file, path, entry.Name()); err != nil {
					return err
				}
			}
			if err == io.EOF {
				break
			}
		}
	}
	// Tightening a directory's access must not prevent visiting its children.
	if needsChange && !w.run.Dryrun {
		if err := w.ctx.Err(); err != nil {
			return err
		}
		if !created {
			if err := w.run.Mutation(path); err != nil {
				return err
			}
		}
		changed, err := wanted.apply(file)
		if changed && !created {
			w.summary.Applied++
		}
		if err != nil {
			return fmt.Errorf("update directory %s: %w", path, err)
		}
	}
	return nil
}

func (w *directoryWalk) entry(parent *os.File, base, name string) error {
	if err := w.ctx.Err(); err != nil {
		return err
	}
	path := filepath.Join(base, name)
	var before unix.Stat_t
	if err := unix.Fstatat(int(parent.Fd()), name, &before, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return fmt.Errorf("inspect %s: %w", path, err)
	}
	kind := uint32(before.Mode) & unix.S_IFMT
	if kind == unix.S_IFLNK {
		w.summary.SkippedSymlinks++
		return nil
	}
	if kind != unix.S_IFDIR && kind != unix.S_IFREG {
		w.summary.SkippedSpecial++
		return nil
	}
	if uint64(before.Dev) != w.device {
		w.summary.SkippedMounts++
		return nil
	}
	isDirectory := kind == unix.S_IFDIR
	wanted := w.wanted(&before, isDirectory)
	if !isDirectory {
		w.summary.Files++
		if matchesStat(wanted, &before) {
			return nil
		}
		w.note(path, &before, wanted, "file")
		if w.run.Dryrun {
			return nil
		}
		if err := w.run.Mutation(path); err != nil {
			return err
		}
	}
	flags := unix.O_RDONLY | unix.O_NOFOLLOW | unix.O_CLOEXEC | unix.O_NONBLOCK
	if isDirectory {
		flags |= unix.O_DIRECTORY
	}
	fd, err := unix.Openat(int(parent.Fd()), name, flags, 0)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	file := os.NewFile(uintptr(fd), path)
	defer file.Close()
	var current unix.Stat_t
	if err := unix.Fstat(fd, &current); err != nil {
		return err
	}
	if current.Dev != before.Dev || current.Ino != before.Ino || uint32(current.Mode)&unix.S_IFMT != kind {
		return fmt.Errorf("path changed during inspection: %s", path)
	}
	if isDirectory {
		return w.directory(file, path, &current, false)
	}
	// Preserve unspecified fields from the file actually opened.
	changed, err := w.wanted(&current, false).apply(file)
	if changed {
		w.summary.Applied++
	}
	if err != nil {
		return fmt.Errorf("update file %s: %w", path, err)
	}
	return nil
}

type Directory struct {
	Source                       string `yaml:"source,omitempty" json:"source,omitempty"`
	AlsoDeleteEntriesNotInSource bool   `yaml:"also_delete_entries_not_in_source,omitempty" json:"also_delete_entries_not_in_source,omitempty"`
	// IfSourceDirectoryIsMissing is fail (the default) or copy_nothing: a
	// source directory missing on the controller machine copies nothing, so
	// path still exists and keeps what it holds.
	IfSourceDirectoryIsMissing string           `yaml:"if_source_directory_is_missing,omitempty" json:"if_source_directory_is_missing,omitempty"`
	Entries                    []DirectoryEntry `yaml:"-" json:"entries,omitempty"`
	// SourceBase and Variants serve a variable source field (variable_sources.go).
	SourceBase string                    `yaml:"-" json:"-"`
	Variants   map[string]*SourceVariant `yaml:"-" json:"variants,omitempty"`
	Path       string                    `yaml:"path" json:"path"`
	// CreateMissingParentDirectories creates the missing parents of Path,
	// as mkdir -p does; without it, a missing parent is a missing
	// prerequisite.
	CreateMissingParentDirectories bool    `yaml:"create_missing_parent_directories,omitempty" json:"create_missing_parent_directories,omitempty"`
	Owner                          Account `yaml:"owner,omitempty" json:"owner,omitempty"`
	Group                          Account `yaml:"group,omitempty" json:"group,omitempty"`
	Mode                           string  `yaml:"permissions_mode,omitempty" json:"permissions_mode,omitempty"`
	Recurse                        bool    `yaml:"also_apply_to_everything_inside,omitempty" json:"also_apply_to_everything_inside,omitempty"`
	FileMode                       string  `yaml:"file_permissions_mode,omitempty" json:"file_permissions_mode,omitempty"`
	// State absent removes the directory: only an empty one, unless
	// AlsoDeleteEverythingInside.
	State                      string `yaml:"state,omitempty" json:"state,omitempty"`
	AlsoDeleteEverythingInside bool   `yaml:"also_delete_everything_inside,omitempty" json:"also_delete_everything_inside,omitempty"`
	// DeleteEverythingInsideExcept lists the names that belong directly
	// inside; every other entry is deleted. An empty list empties the
	// directory, so nil and empty differ.
	DeleteEverythingInsideExcept []string `yaml:"delete_everything_inside_except,omitempty" json:"delete_everything_inside_except"`
}

// CopiesNothingWhenSourceIsMissing reports if_source_directory_is_missing:
// copy_nothing.
func (d Directory) CopiesNothingWhenSourceIsMissing() bool {
	return d.IfSourceDirectoryIsMissing == "copy_nothing"
}

// Deletes reports whether the task deletes: the directory itself, or what
// it does not list.
func (d Directory) Deletes() bool {
	return d.State == "absent" || d.DeleteEverythingInsideExcept != nil
}

func ValidateDirectory(d Directory) error {
	if err := validatePresence(d.State); err != nil {
		return err
	}
	metadata := d.Owner != "" || d.Group != "" || d.Mode != "" || d.FileMode != "" || d.Recurse || d.Source != ""
	switch {
	case d.AlsoDeleteEntriesNotInSource && d.Source == "":
		return invalid("also_delete_entries_not_in_source", "directory.also_delete_entries_not_in_source requires source")
	case d.AlsoDeleteEntriesNotInSource && d.CopiesNothingWhenSourceIsMissing():
		return invalid("also_delete_entries_not_in_source", "directory.also_delete_entries_not_in_source cannot use if_source_directory_is_missing: copy_nothing: a missing source directory on the controller machine would delete everything inside path")
	case d.State == "absent" && (metadata || d.CreateMissingParentDirectories || d.DeleteEverythingInsideExcept != nil):
		return fmt.Errorf("directory with state: absent accepts only path, state, and also_delete_everything_inside")
	case d.AlsoDeleteEverythingInside && d.State != "absent":
		return invalid("also_delete_everything_inside", "directory.also_delete_everything_inside goes with state: absent")
	case d.DeleteEverythingInsideExcept != nil && (d.FileMode != "" || d.Recurse || d.Source != ""):
		return invalid("delete_everything_inside_except", "directory.delete_everything_inside_except accepts path, create_missing_parent_directories, owner, group, and permissions_mode; it does not copy or change metadata inside the directory")
	}
	for _, mode := range []string{d.Mode, d.FileMode} {
		if mode != "" && !modePattern.MatchString(mode) {
			return fmt.Errorf("permissions_mode and file_permissions_mode must be quoted octal strings, such as \"0750\"")
		}
	}
	if d.FileMode != "" && !d.Recurse && d.Source == "" {
		return fmt.Errorf("directory.file_permissions_mode requires source or also_apply_to_everything_inside: true")
	}
	if d.IfSourceDirectoryIsMissing != "" && !slices.Contains(ifSourceDirectoryIsMissing, d.IfSourceDirectoryIsMissing) {
		return invalid("if_source_directory_is_missing", "directory.if_source_directory_is_missing must be fail or copy_nothing")
	}
	if d.IfSourceDirectoryIsMissing != "" && d.Source == "" {
		return invalid("if_source_directory_is_missing", "directory.if_source_directory_is_missing requires source; the setting controls what happens when the source directory is missing")
	}
	if d.Source != "" && d.Recurse {
		return fmt.Errorf("directory.source cannot use also_apply_to_everything_inside: true")
	}
	return nil
}

// directoryDescription is what mmx describe-module shows for directory.
var directoryDescription = Description{
	Name: "directory", Topic: TopicDirectoriesDownloadsAndReleaseDirectories, Schema: reflect.TypeOf(Directory{}),
	Summary: "one directory and its metadata, optionally the metadata of everything inside it or a copied source tree; or its deletion, or the deletion of what it does not list",
	Fields: []Field{
		{Name: "path", Required: true, Notes: "absolute; the parent directory must exist, unless create_missing_parent_directories is true; a directory MMX deletes or deletes from is at least two levels deep, such as /srv/app, and is not a symlink"},
		{Name: "create_missing_parent_directories", Default: "false", Notes: "true creates the missing parent directories of path, as mkdir -p does, each owned by root with permissions_mode 0755, and lists them in the result and the dryrun; false leaves a missing parent a missing prerequisite; not with state: absent"},
		{Name: "owner", Type: "account name or integer", Notes: "an account name, or a numeric ID written without quotes"},
		{Name: "group", Type: "group name or integer", Notes: "a group name, or a numeric ID written without quotes"},
		{Name: "permissions_mode", Notes: `quoted octal for directories, such as "0750"; a new directory gets the source directory's mode with source, otherwise 0755`},
		{Name: "also_apply_to_everything_inside", Default: "false", Notes: "true also applies owner, group, permissions_mode, and file_permissions_mode to everything below path"},
		{Name: "file_permissions_mode", Notes: "quoted octal for the files inside, such as \"0640\"; needs also_apply_to_everything_inside or source"},
		{Name: "source", Notes: "local directory copied into path, relative to the project root, or to the role's directory inside a role; not with also_apply_to_everything_inside"},
		{Name: "also_delete_entries_not_in_source", Default: "false", Notes: "with source: deletes entries at every depth under path that the frozen source tree does not list, under the deletion safeguards; not with if_source_directory_is_missing: copy_nothing, because a missing source directory would delete everything inside path"},
		{Name: "if_source_directory_is_missing", Values: ifSourceDirectoryIsMissing, Default: "fail", Notes: "with source: copy_nothing treats a source directory missing on the controller machine as empty, so the task only makes sure path exists (a new one gets 0755) and leaves what it holds; fail refuses the playbook before any connection"},
		{Name: "state", Values: presenceStates, Default: "present", Notes: "absent deletes the directory: only an empty one, unless also_delete_everything_inside; accepts only path, state, and also_delete_everything_inside; a missing path is unchanged"},
		{Name: "also_delete_everything_inside", Default: "false", Notes: "with state: absent: deletes the directory with its whole tree, renamed beside it first so the next run finishes an interrupted deletion; a mount point anywhere inside fails the task before anything is deleted"},
		{Name: "delete_everything_inside_except", Notes: "the names that belong directly inside path; every other entry there is deleted with its whole tree, under the same safeguards; [] empties the directory; the names are not created; a missing directory is created with owner, group, and permissions_mode, as by any directory task with state: present, and holds nothing to delete; accepts path, create_missing_parent_directories, owner, group, and permissions_mode for the directory itself"},
	},
	Example: "- directory:\n    path: /srv/app\n    owner: app\n    group: app\n    permissions_mode: \"0750\"\n",
	Results: []string{"status", "change", "value.deleted_entries", "value.deleted_names"},
	Dryrun:  "supported; inspects only; recursive and copied trees report counts and up to ten examples; a deletion reports deleted_entries (also with also_delete_entries_not_in_source), and delete_everything_inside_except also deleted_names, without deleting; delete_everything_inside_except on a missing directory reports its creation, as for any new directory; not_checked while the parent directory is missing, unless an earlier node task of the dryrun would create it or create_missing_parent_directories is true",
}

// Prepare checks the inputs when the project loads and reads a source tree
// on the controller machine, relative to base.
func (d *Directory) Prepare(base string) error {
	if err := ValidateDirectory(*d); err != nil {
		return err
	}
	d.SourceBase = base
	if d.Source != "" && !Dynamic(d.Source) {
		root, err := controllerPath(base, "source", d.Source)
		if err != nil {
			return err
		}
		entries, err := readDirectorySource(root, d.CopiesNothingWhenSourceIsMissing())
		d.Entries = entries
		if err != nil {
			return &FieldError{"source", fmt.Errorf("directory source %s: %w", d.Source, err)}
		}
	}
	return nil
}

// Arguments lists the inputs that may hold a reference, with their rules.
func (d *Directory) Arguments(arguments *ArgumentList) {
	if d.Deletes() || d.AlsoDeleteEntriesNotInSource {
		arguments.Public("path", &d.Path, deletablePath)
	} else {
		arguments.Public("path", &d.Path, absolute)
	}
	arguments.Owner("owner", &d.Owner)
	arguments.Owner("group", &d.Group)
	for i := range d.DeleteEverythingInsideExcept {
		arguments.Public(fmt.Sprintf("delete_everything_inside_except[%d]", i), &d.DeleteEverythingInsideExcept[i], entryName)
	}
}

// CleanPaths cleans the resolved path.
func (d *Directory) CleanPaths() {
	d.Path = filepath.Clean(d.Path)
}

// ControllerFiles lists the source tree the directory copies.
func (d *Directory) ControllerFiles() []ControllerFile {
	if d.Source == "" {
		return nil
	}
	return []ControllerFile{{Field: "source", Kind: SourceDirectory, Path: &d.Source, Base: d.SourceBase,
		Variants: &d.Variants, Entries: &d.Entries,
		ReadTree: func(path, name string) ([]DirectoryEntry, error) {
			entries, err := readDirectorySource(path, d.CopiesNothingWhenSourceIsMissing())
			if err != nil {
				return nil, fmt.Errorf("directory source %s: %w", name, err)
			}
			return entries, ValidateDirectorySource(Directory{Path: d.Path, Source: name, Entries: entries})
		},
		CheckTree: func() error { return ValidateDirectorySource(*d) },
	}}
}

// Run does the module's work on the node.
func (d *Directory) Run(ctx context.Context, task NodeTask) protocol.Result {
	return task.Runtime.Directory(ctx, *d, task.Scope)
}

// ManagedPaths lists the paths the task manages or says it creates, as
// written; a dryrun cannot check a later task's path_exists on them.
func (d *Directory) ManagedPaths() []string {
	return []string{d.Path}
}

// TrackedInChanges opts a directory out of its node task block's tracked
// inputs unless the directory copies a source tree or deletes.
func (d *Directory) TrackedInChanges() bool {
	return d.Source != "" || d.Deletes()
}

// HashConfiguration hashes a directory deletion: whether the directory
// exists and, for delete_everything_inside_except, the names inside it.
func (d *Directory) HashConfiguration(ctx context.Context, h hash.Hash, apply bool, s value.Scope) error {
	exists, names, err := DeletionInput(*d, apply)
	if err != nil {
		return err
	}
	if !exists {
		_, _ = h.Write([]byte{0})
		return nil
	}
	_, _ = h.Write([]byte{3})
	for _, name := range names {
		HashField(h, name)
	}
	return nil
}

// TrackedInputs lists a deletion's directory, or each file a source tree
// copies.
func (d *Directory) TrackedInputs() []TrackedInput {
	if d.Deletes() {
		return []TrackedInput{{Kind: "config", Path: d.Path, Content: d}}
	}
	var out []TrackedInput
	if d.Source != "" {
		for _, entry := range d.Entries {
			if !entry.Directory {
				path := filepath.Join(d.Path, filepath.FromSlash(entry.Path))
				out = append(out, TrackedInput{Kind: "config", Path: path, Content: &File{Mode: "0644", Path: path, Source: d.Source, Asset: entry.Asset}})
			}
		}
	}
	return out
}
