package modules

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"

	"mmx/internal/protocol"

	"golang.org/x/sys/unix"
)

// removeDirectory deletes what a directory task with state: absent names, or
// what delete_everything_inside_except does not list inside a directory that
// exists; Directory creates a missing one instead. A tree is renamed to its
// staging name beside it before it is deleted, so its path is gone at once
// and the next run finishes a deletion that stopped half way. Nothing is
// deleted through a link or on another filesystem: a mount point anywhere in
// what would be deleted fails the task before anything is.
func (run Runtime) removeDirectory(ctx context.Context, d Directory) protocol.Result {
	parent, info, err := openParent(d.Path, true)
	if err != nil {
		return protocol.Failed(err)
	}
	if parent == nil {
		return protocol.Success("unchanged")
	}
	defer parent.Close()
	if info != nil && info.Mode()&os.ModeSymlink != 0 {
		return protocol.Failed(fmt.Errorf("%s is a symlink; a directory node task deletes neither a symlink at its path nor anything through a symlink", d.Path))
	}
	if info != nil && !info.IsDir() {
		return protocol.Failed(fmt.Errorf("%s is not a directory", d.Path))
	}
	if d.DeleteEverythingInsideExcept != nil {
		return run.deleteUnlisted(ctx, d, parent, info)
	}
	return run.deleteDirectory(ctx, d, parent, info)
}

// deleteDirectory deletes the directory at d.Path: only an empty one, unless
// the task says also_delete_everything_inside.
func (run Runtime) deleteDirectory(ctx context.Context, d Directory, parent *os.Root, info os.FileInfo) protocol.Result {
	name := filepath.Base(d.Path)
	dir, err := parent.Open(".")
	if err != nil {
		return protocol.Failed(err)
	}
	defer dir.Close()
	device, err := deviceOf(dir)
	if err != nil {
		return protocol.Failed(err)
	}
	// Only a whole-tree deletion finishes one that stopped half way: its
	// staging directory may hold anything.
	var trees []string
	if info != nil {
		trees = append(trees, name)
	}
	staging := stagingName(name)
	if d.AlsoDeleteEverythingInside {
		if left, err := parent.Lstat(staging); err == nil && left.IsDir() {
			trees = append(trees, staging)
		} else if err != nil && !errors.Is(err, os.ErrNotExist) {
			return protocol.Failed(err)
		}
	}
	if len(trees) == 0 {
		return protocol.Success("unchanged")
	}
	deleted := 0
	for _, tree := range trees {
		count, err := deletionTree(ctx, dir, device, tree, filepath.Join(filepath.Dir(d.Path), tree))
		if err != nil {
			return protocol.Failed(err)
		}
		deleted += count
	}
	if info != nil && !d.AlsoDeleteEverythingInside && deleted > 1 {
		return protocol.Failed(fmt.Errorf("%s is not empty (%d entries inside); write also_delete_everything_inside: true to delete it with everything inside", d.Path, deleted-1))
	}
	result := deletion(run.Dryrun, deleted, nil)
	if run.Dryrun {
		return result
	}
	if err := ctx.Err(); err != nil {
		return protocol.Failed(err)
	}
	if err := run.Mutation(d.Path); err != nil {
		return protocol.Failed(err)
	}
	if !d.AlsoDeleteEverythingInside {
		// rmdir refuses a directory that gained an entry since.
		if err := sameFileAt(parent, name, info); err != nil {
			return protocol.Failed(err)
		}
		if err := parent.Remove(name); err != nil {
			return protocol.Failed(err)
		}
		return finishDeletion(result, true, syncDirectory(parent))
	}
	if info == nil {
		// RemoveAll may stop part of the way through: the node changed.
		return finishDeletion(result, true, parent.RemoveAll(staging))
	}
	changed, err := stageAndDelete(parent, name, info)
	return finishDeletion(result, changed, err)
}

// deleteUnlisted deletes every entry directly inside the directory at d.Path
// that the task does not list, each with its whole tree.
func (run Runtime) deleteUnlisted(ctx context.Context, d Directory, parent *os.Root, info os.FileInfo) protocol.Result {
	inside, err := openDirectoryAt(parent, filepath.Base(d.Path), info)
	if err != nil {
		return protocol.Failed(err)
	}
	defer inside.Close()
	dir, err := inside.Open(".")
	if err != nil {
		return protocol.Failed(err)
	}
	defer dir.Close()
	device, err := deviceOf(dir)
	if err != nil {
		return protocol.Failed(err)
	}
	unlisted, err := unlistedEntries(dir, d.DeleteEverythingInsideExcept)
	if err != nil {
		return protocol.Failed(err)
	}
	deleted := 0
	for _, name := range unlisted {
		count, err := deletionTree(ctx, dir, device, name, filepath.Join(d.Path, name))
		if err != nil {
			return protocol.Failed(err)
		}
		deleted += count
	}
	metadata, err := metadataFor(info, 0755, d.Mode, d.Owner, d.Group)
	if err != nil {
		return run.accountFailure(err)
	}
	metadataChanged := !metadata.matches(info)
	if len(unlisted) == 0 && !metadataChanged {
		return protocol.Success("unchanged")
	}
	result := deletion(run.Dryrun, deleted, unlisted)
	if run.Dryrun {
		return result
	}
	if err := ctx.Err(); err != nil {
		return protocol.Failed(err)
	}
	if err := run.Mutation(d.Path); err != nil {
		return protocol.Failed(err)
	}
	changed, err := deleteUnlistedEntries(ctx, inside, unlisted)
	if err != nil {
		return finishDeletion(result, changed, err)
	}
	if metadataChanged {
		if err := sameFileAt(parent, filepath.Base(d.Path), info); err != nil {
			return finishDeletion(result, changed, err)
		}
		applied, err := metadata.apply(dir)
		changed = changed || applied
		if err == nil {
			err = dir.Sync()
		}
		return finishDeletion(result, changed, err)
	}
	return finishDeletion(result, changed, nil)
}

// deleteUnlistedEntries applies a preflighted list through the same staged
// directory deletion path for source copies and explicit directory cleanup.
func deleteUnlistedEntries(ctx context.Context, inside *os.Root, unlisted []string) (bool, error) {
	changed := false
	for _, name := range unlisted {
		if err := ctx.Err(); err != nil {
			return changed, err
		}
		entry, err := inside.Lstat(name)
		if errors.Is(err, os.ErrNotExist) {
			continue // the leftover staging directory of an entry deleted before
		}
		if err != nil {
			return changed, err
		}
		if entry.IsDir() {
			var staged bool
			staged, err = stageAndDelete(inside, name, entry)
			changed = changed || staged
		} else if err = inside.Remove(name); err == nil {
			changed = true
		}
		if err != nil {
			return changed, err
		}
	}
	return changed, nil
}

// unlistedEntries lists, sorted, the names directly inside dir that keep does
// not list.
func unlistedEntries(dir *os.File, keep []string) ([]string, error) {
	if _, err := dir.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	names, err := dir.Readdirnames(-1)
	if err != nil {
		return nil, err
	}
	var unlisted []string
	for _, name := range names {
		if !slices.Contains(keep, name) {
			unlisted = append(unlisted, name)
		}
	}
	slices.Sort(unlisted)
	return unlisted, nil
}

// stageAndDelete deletes the directory name within parent with its tree. It
// deletes a leftover staging directory of name, renames name to the staging
// name, which makes the path gone at once and durably, then deletes the
// staging tree. changed reports whether the node changed, also with an error.
func stageAndDelete(parent *os.Root, name string, before os.FileInfo) (changed bool, err error) {
	staging := stagingName(name)
	if _, err := parent.Lstat(staging); err == nil {
		// RemoveAll may stop part of the way through: the node changed.
		if err := parent.RemoveAll(staging); err != nil {
			return true, err
		}
		changed = true
	}
	if err := sameFileAt(parent, name, before); err != nil {
		return changed, err
	}
	if err := parent.Rename(name, staging); err != nil {
		return changed, err
	}
	if err := syncDirectory(parent); err != nil {
		return true, err
	}
	return true, parent.RemoveAll(staging)
}

// deletion is the result of a deletion before it is applied: deleted_entries
// counts every file, link, and directory deleted, each directory included;
// deleted_names lists the entries delete_everything_inside_except deletes.
func deletion(dryrun bool, deleted int, names []string) protocol.Result {
	r := protocol.Success("changed")
	if dryrun {
		r.Change = "would_change"
	}
	value := map[string]any{"deleted_entries": deleted}
	if names != nil {
		value["deleted_names"] = names
	}
	r.Value = value
	return r
}

// bytesText says how many bytes a deletion frees, in the binary unit that
// fits and exactly: "3.4 MiB (3565158 bytes)".
func bytesText(n int64) string {
	exact := strconv.FormatInt(n, 10) + " bytes"
	if n == 1 {
		exact = "1 byte"
	}
	value, unit := float64(n), ""
	for _, next := range []string{"KiB", "MiB", "GiB", "TiB"} {
		if value < 1024 {
			break
		}
		value, unit = value/1024, next
	}
	if unit == "" {
		return exact
	}
	return strconv.FormatFloat(value, 'f', 1, 64) + " " + unit + " (" + exact + ")"
}

// finishDeletion is the result r of a deletion that ran, failed by err; it
// changed nothing unless changed.
func finishDeletion(r protocol.Result, changed bool, err error) protocol.Result {
	switch {
	case err == nil:
		return r
	case !changed:
		return protocol.Failed(err)
	}
	r.Status, r.Reason = "failed", err.Error()
	return r
}

func deviceOf(dir *os.File) (uint64, error) {
	var st unix.Stat_t
	if err := unix.Fstat(int(dir.Fd()), &st); err != nil {
		return 0, err
	}
	return uint64(st.Dev), nil
}

// deletionTree counts the entries of the tree at name within dir, the entry
// itself included, without following links. It fails on a mount point at or
// below name, whether another filesystem (a device other than device, dir's)
// or a bind mount: deleting through it would delete files that are not part
// of this tree.
func deletionTree(ctx context.Context, dir *os.File, device uint64, name, path string) (int, error) {
	return inspectDeletionTree(ctx, dir, device, name, path, nil)
}

// inspectDeletionTree also collects a bounded copy summary when requested.
func inspectDeletionTree(ctx context.Context, dir *os.File, device uint64, name, path string, summary *protocol.DirectoryChange) (int, error) {
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	var st unix.Stat_t
	if err := unix.Fstatat(int(dir.Fd()), name, &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return 0, fmt.Errorf("inspect %s: %w", path, err)
	}
	mounted, err := mountRoot(int(dir.Fd()), name)
	if err != nil {
		return 0, fmt.Errorf("inspect %s: %w", path, err)
	}
	if mounted || uint64(st.Dev) != device {
		return 0, fmt.Errorf("%s is a mount point; MMX did not delete any files or directories because deletion cannot cross a mount point; unmount the filesystem first", path)
	}
	if summary != nil {
		kind := "special"
		switch uint32(st.Mode) & unix.S_IFMT {
		case unix.S_IFDIR:
			kind = "directory"
			summary.Directories++
		case unix.S_IFREG:
			kind = "file"
		case unix.S_IFLNK:
			kind = "symlink"
		}
		if kind != "directory" {
			summary.Files++
		}
		summary.Changes++
		if len(summary.Examples) < 10 {
			summary.Examples = append(summary.Examples, protocol.FileChange{Path: path, Action: "delete", Before: &protocol.FileState{Kind: kind, Mode: fmt.Sprintf("%04o", uint32(st.Mode)&07777), UID: int(st.Uid), GID: int(st.Gid), Size: st.Size}})
		}
	}
	if uint32(st.Mode)&unix.S_IFMT != unix.S_IFDIR {
		return 1, nil
	}
	fd, err := unix.Openat(int(dir.Fd()), name, unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return 0, fmt.Errorf("open %s: %w", path, err)
	}
	child := os.NewFile(uintptr(fd), path)
	defer child.Close()
	var opened unix.Stat_t
	if err := unix.Fstat(fd, &opened); err != nil {
		return 0, err
	}
	if opened.Dev != st.Dev || opened.Ino != st.Ino {
		return 0, fmt.Errorf("path changed during inspection: %s", path)
	}
	count := 1
	for {
		names, err := child.Readdirnames(256)
		for _, entry := range names {
			n, err := inspectDeletionTree(ctx, child, device, entry, filepath.Join(path, entry), summary)
			if err != nil {
				return 0, err
			}
			count += n
		}
		if err == io.EOF {
			return count, nil
		}
		if err != nil {
			return 0, fmt.Errorf("read directory %s: %w", path, err)
		}
	}
}

// DeletionInput is what a directory task that deletes adds to its node task
// block's tracked inputs, as it is now or, with applied, as the task would
// leave it: whether the directory exists, and for
// delete_everything_inside_except the names directly inside it. Every
// deletion the task makes changes it, so the block's service is refreshed,
// and so does the creation of a missing directory, which
// delete_everything_inside_except leaves empty.
func DeletionInput(d Directory, applied bool) (exists bool, names []string, err error) {
	if applied && d.State == "absent" {
		return false, nil, nil
	}
	info, err := os.Lstat(d.Path)
	if errors.Is(err, os.ErrNotExist) {
		return applied, nil, nil
	}
	if err != nil || !info.IsDir() || d.State == "absent" {
		return err == nil, nil, err
	}
	dir, err := os.Open(d.Path)
	if err != nil {
		return true, nil, err
	}
	defer dir.Close()
	if names, err = dir.Readdirnames(-1); err != nil {
		return true, nil, err
	}
	if applied {
		names = slices.DeleteFunc(names, func(name string) bool { return !slices.Contains(d.DeleteEverythingInsideExcept, name) })
	}
	slices.Sort(names)
	return true, names, nil
}
