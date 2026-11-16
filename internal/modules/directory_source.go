package modules

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"unicode/utf8"

	"mmx/internal/assets"
	"mmx/internal/protocol"
	"mmx/internal/value"
)

// A copy installs the frozen source entries. Open directory handles pin the
// destination; fileAt supplies the same comparison/candidate path as file node tasks.
type directoryCopy struct {
	run            Runtime
	ctx            context.Context
	input          Directory
	scope          value.Scope
	children       map[string][]DirectoryEntry
	summary        *protocol.DirectoryChange
	deletions      map[string]directoryCopyDeletion
	deletedEntries int
}

type directoryCopyDeletion struct {
	names   []string
	entries int
}

func (run Runtime) copyDirectory(ctx context.Context, d Directory, s value.Scope) (result protocol.Result) {
	if err := ValidateDirectorySource(d); err != nil {
		return protocol.ConfigurationError(err)
	}
	c := directoryCopy{run: run, ctx: ctx, input: d, scope: s, children: map[string][]DirectoryEntry{}, summary: &protocol.DirectoryChange{Path: d.Path, Copy: true}, deletions: map[string]directoryCopyDeletion{}}
	defer func() {
		result.DirectoryChange = c.summary
		result.FileChange = nil
		if d.AlsoDeleteEntriesNotInSource {
			deleted := c.deletedEntries
			if run.Dryrun && result.Status == "succeeded" {
				for _, deletion := range c.deletions {
					deleted += deletion.entries
				}
			}
			result.Value = map[string]any{"deleted_entries": deleted}
		}
		if c.summary.Applied > 0 {
			result.Change = "changed"
		} else if run.Dryrun && c.summary.Changes > 0 {
			result.Change = "would_change"
		}
	}()
	for _, entry := range d.Entries[1:] {
		c.children[path.Dir(entry.Path)] = append(c.children[path.Dir(entry.Path)], entry)
	}
	parent, createdParents, r := run.openParent(ctx, d)
	if r != nil {
		return *r
	}
	c.summary.CreatedParentDirectories = createdParents
	if parent != nil {
		defer parent.Close()
	}
	// Detect existing source-owned type conflicts before creating any entries.
	if err := c.inspect(parent, d.Entries[0], ""); err != nil {
		return protocol.ConfigurationError(err)
	}
	return c.directory(parent, d.Entries[0])
}

func (c *directoryCopy) location(entry DirectoryEntry) (string, string) {
	full := filepath.Join(c.input.Path, filepath.FromSlash(entry.Path))
	if full == string(filepath.Separator) {
		return full, "."
	}
	return full, filepath.Base(full)
}

func (c *directoryCopy) inspect(parent *os.Root, entry DirectoryEntry, mountedAncestor string) error {
	if err := c.ctx.Err(); err != nil {
		return err
	}
	full, name := c.location(entry)
	info, err := entryInfoAt(parent, name, entry.Directory)
	if err != nil {
		return fmt.Errorf("%s: %w", full, err)
	}
	if info == nil || !entry.Directory {
		return nil
	}
	root, err := openDirectoryAt(parent, name, info)
	if err != nil {
		return err
	}
	defer root.Close()
	if c.input.AlsoDeleteEntriesNotInSource {
		if entry.Path != "." {
			dir, err := parent.Open(".")
			if err != nil {
				return err
			}
			mounted, mountErr := mountRoot(int(dir.Fd()), name)
			dir.Close()
			if mountErr != nil {
				return mountErr
			}
			if mounted {
				mountedAncestor = full
			}
		}
		dir, err := root.Open(".")
		if err != nil {
			return err
		}
		defer dir.Close()
		var keep []string
		for _, child := range c.children[entry.Path] {
			keep = append(keep, path.Base(child.Path))
		}
		unlisted, err := unlistedEntries(dir, keep)
		if err != nil {
			return err
		}
		if len(unlisted) > 0 && mountedAncestor != "" {
			return fmt.Errorf("%s is a mount point; MMX did not delete any files or directories because deletion cannot cross a mount point; unmount the filesystem first", mountedAncestor)
		}
		device, err := deviceOf(dir)
		if err != nil {
			return err
		}
		deleted := 0
		for _, extra := range unlisted {
			info, err := root.Lstat(extra)
			if err != nil {
				return err
			}
			if info.IsDir() && slices.Contains(keep, stagingName(extra)) {
				return fmt.Errorf("cannot delete %s: its deletion staging path %s belongs to the source; rename the source entry first", filepath.Join(full, extra), filepath.Join(full, stagingName(extra)))
			}
			count, err := inspectDeletionTree(c.ctx, dir, device, extra, filepath.Join(full, extra), c.summary)
			if err != nil {
				return err
			}
			deleted += count
		}
		c.deletions[entry.Path] = directoryCopyDeletion{names: unlisted, entries: deleted}
	}
	for _, child := range c.children[entry.Path] {
		if err := c.inspect(root, child, mountedAncestor); err != nil {
			return err
		}
	}
	return nil
}

func (c *directoryCopy) note(change *protocol.FileChange) {
	if change == nil {
		return
	}
	c.summary.Changes++
	if len(c.summary.Examples) < 10 {
		c.summary.Examples = append(c.summary.Examples, *change)
	}
}

func (c *directoryCopy) directory(parent *os.Root, entry DirectoryEntry) protocol.Result {
	if err := c.ctx.Err(); err != nil {
		return protocol.Failed(err)
	}
	full, name := c.location(entry)
	info, err := entryInfoAt(parent, name, true)
	if err != nil {
		return protocol.Failed(err)
	}
	wanted, err := metadataFor(info, os.FileMode(entry.Mode), c.input.Mode, c.input.Owner, c.input.Group)
	if err != nil {
		return c.run.accountFailure(err)
	}
	c.summary.Directories++
	changed := !wanted.matches(info)
	created := info == nil
	if changed {
		change := fileChange(full, info, wanted, 0, false)
		change.After.Kind = "directory"
		if change.Before != nil {
			change.Before.Kind, change.Before.Size = "directory", 0
		}
		c.note(change)
	}
	if created && !c.run.Dryrun {
		if err := c.run.Mutation(full); err != nil {
			return protocol.Failed(err)
		}
		if err := createLabeled(c.ctx, full, selinuxDirectory, func() error { return parent.Mkdir(name, 0700) }); err != nil {
			return protocol.Failed(err)
		}
		c.summary.Applied++
		info, err = parent.Lstat(name)
		if err != nil {
			return protocol.Failed(err)
		}
	}
	var root *os.Root
	if info != nil {
		root, err = openDirectoryAt(parent, name, info)
		if err != nil {
			return protocol.Failed(err)
		}
		defer root.Close()
	}
	// Finish a new directory before copying children. Otherwise an interrupted
	// copy could leave temporary permissions that the next run would preserve.
	if created && !c.run.Dryrun {
		file, err := root.Open(".")
		if err != nil {
			return protocol.Failed(err)
		}
		_, err = wanted.apply(file)
		file.Close()
		if err != nil {
			return protocol.Failed(err)
		}
	}
	for _, child := range c.children[entry.Path] {
		if err := c.ctx.Err(); err != nil {
			return protocol.Failed(err)
		}
		var r protocol.Result
		if child.Directory {
			r = c.directory(root, child)
		} else {
			childPath, _ := c.location(child)
			f := File{Path: childPath, Source: c.input.Source, Asset: child.Asset, Mode: c.input.FileMode, Owner: c.input.Owner, Group: c.input.Group}
			c.summary.Files++
			run := c.run
			if len(c.summary.Examples) >= 10 {
				run.RetainDiff = nil
			}
			r = run.fileAt(c.ctx, f, c.scope, root, os.FileMode(child.Mode), nil)
			c.note(r.FileChange)
			if r.Change == "changed" {
				c.summary.Applied++
			}
		}
		if r.Status != "succeeded" {
			return r
		}
	}
	if deletion := c.deletions[entry.Path]; len(deletion.names) > 0 && !c.run.Dryrun {
		if err := c.run.Mutation(full); err != nil {
			return protocol.Failed(err)
		}
		changed, err := deleteUnlistedEntries(c.ctx, root, deletion.names)
		if changed {
			c.summary.Applied += deletion.entries
			c.deletedEntries += deletion.entries
		}
		if err != nil {
			return protocol.Failed(err)
		}
	}
	// Change access to existing directories after copying their children.
	if changed && !created && !c.run.Dryrun {
		if err := c.ctx.Err(); err != nil {
			return protocol.Failed(err)
		}
		if err := c.run.Mutation(full); err != nil {
			return protocol.Failed(err)
		}
		if err := sameFileAt(parent, name, info); err != nil {
			return protocol.Failed(err)
		}
		file, err := root.Open(".")
		if err != nil {
			return protocol.Failed(err)
		}
		applied, err := wanted.apply(file)
		file.Close()
		if applied {
			c.summary.Applied++
		}
		if err != nil {
			return protocol.Failed(err)
		}
	}
	if info != nil {
		if err := sameFileAt(parent, name, info); err != nil {
			return protocol.Failed(err)
		}
	}
	return protocol.Success("unchanged")
}

// DirectoryEntry describes one frozen source path. Contents share the ordinary
// run asset store; these entries never become separate scheduled node tasks.
type DirectoryEntry struct {
	Path      string      `json:"path"`
	Directory bool        `json:"directory,omitempty"`
	Mode      uint32      `json:"mode"`
	Asset     *assets.Ref `json:"asset,omitempty"`
	LocalPath string      `json:"-"`
}

// Validate the frozen layout both locally and on receipt, before any admission
// or filesystem work. Digest/size verification belongs to the shared asset path.
func ValidateDirectorySource(d Directory) error {
	if err := ValidateDirectory(Directory{Path: "/", Source: d.Source, Recurse: d.Recurse, Mode: d.Mode, FileMode: d.FileMode}); err != nil {
		return err
	}
	// The entries belong to one resolved source, never to a path with references.
	if d.Source == "" || Dynamic(d.Source) || len(d.Entries) == 0 {
		return fmt.Errorf("directory source requires a resolved source path and a nonempty saved list of files and directories")
	}
	seen := make(map[string]bool, len(d.Entries))
	for i, e := range d.Entries {
		if !fs.ValidPath(e.Path) || !utf8.ValidString(e.Path) || strings.ContainsAny(e.Path, "\\\x00") {
			return fmt.Errorf("directory source path must be relative and stay inside the source directory: %q", e.Path)
		}
		if _, exists := seen[e.Path]; exists {
			return fmt.Errorf("duplicate directory source path: %s", e.Path)
		}
		if e.Mode & ^uint32(0777) != 0 {
			return fmt.Errorf("directory source permissions must contain only read, write, and execute bits (no setuid, setgid, or sticky bits): %s", e.Path)
		}
		if i == 0 {
			if e.Path != "." || !e.Directory {
				return fmt.Errorf("directory source must begin with its root directory")
			}
		} else if !seen[path.Dir(e.Path)] {
			return fmt.Errorf("directory source entry must follow the entry for the parent directory: %s", e.Path)
		}
		if e.Directory != (e.Asset == nil) {
			return fmt.Errorf("directory source entry must have saved file contents for a regular file and no saved contents for a directory: %s", e.Path)
		}
		seen[e.Path] = e.Directory
	}
	return nil
}

// readDirectorySource freezes the source tree at root. With copyNothing
// (if_source_directory_is_missing: copy_nothing), a missing root is an empty
// source: only the destination directory itself, new ones with mode 0755.
func readDirectorySource(root string, copyNothing bool) ([]DirectoryEntry, error) {
	root = filepath.Clean(root)
	info, err := os.Lstat(root)
	if errors.Is(err, fs.ErrNotExist) && copyNothing {
		return []DirectoryEntry{{Path: ".", Directory: true, Mode: 0755}}, nil
	}
	if errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("%w; if a missing source directory means there is nothing to copy, set if_source_directory_is_missing: copy_nothing", err)
	}
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("directory source must be a real directory")
	}
	var entries []DirectoryEntry
	err = filepath.WalkDir(root, func(local string, entry fs.DirEntry, err error) error {
		relative, relErr := filepath.Rel(root, local)
		if relErr != nil {
			return relErr
		}
		if err != nil {
			return err
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if (!info.IsDir() && !info.Mode().IsRegular()) || info.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
			return fmt.Errorf("directory source accepts only directories and regular files without setuid, setgid, or sticky bits: %s", relative)
		}
		e := DirectoryEntry{Path: filepath.ToSlash(relative), Directory: info.IsDir(), Mode: uint32(info.Mode().Perm()), LocalPath: local}
		if !e.Directory {
			e.Asset = &assets.Ref{}
		}
		entries = append(entries, e)
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	err = ValidateDirectorySource(Directory{Source: root, Entries: entries})
	return entries, err
}
