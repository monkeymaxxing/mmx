package modules

import (
	"errors"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"syscall"

	"mmx/internal/protocol"
)

// File and directory resources share Unix ownership/mode handling. Content
// replacement and recursive traversal stay with their resource implementations.
type fileMetadata struct {
	mode     os.FileMode
	uid, gid int
}

var lookupGroup = user.LookupGroup

func metadataFor(info os.FileInfo, defaultMode os.FileMode, mode string, owner, group Account) (fileMetadata, error) {
	m := fileMetadata{defaultMode, os.Geteuid(), os.Getegid()}
	if info != nil {
		m.mode = permissionMode(info.Mode())
		stat := info.Sys().(*syscall.Stat_t)
		m.uid, m.gid = int(stat.Uid), int(stat.Gid)
	}
	if mode != "" {
		n, err := strconv.ParseUint(mode, 8, 12)
		if err != nil {
			return m, err
		}
		m.mode = os.FileMode(n)
	}
	// A numeric ID is used as it is, with no account lookup: it may have no
	// account on this node.
	var err error
	switch {
	case owner.IsID():
		m.uid, err = strconv.Atoi(string(owner))
	case owner != "":
		var u *user.User
		if u, err = user.Lookup(string(owner)); err == nil {
			m.uid, err = strconv.Atoi(u.Uid)
		}
	}
	if err != nil {
		return m, err
	}
	switch {
	case group.IsID():
		m.gid, err = strconv.Atoi(string(group))
	case group != "":
		var g *user.Group
		if g, err = lookupGroup(string(group)); err == nil {
			m.gid, err = strconv.Atoi(g.Gid)
		}
	}
	if err != nil {
		return m, err
	}
	return m, nil
}

func (m fileMetadata) matches(info os.FileInfo) bool {
	if info == nil {
		return false
	}
	stat := info.Sys().(*syscall.Stat_t)
	return permissionMode(info.Mode()) == m.mode && int(stat.Uid) == m.uid && int(stat.Gid) == m.gid
}

func (m fileMetadata) apply(file *os.File) (changed bool, err error) {
	info, err := file.Stat()
	if err != nil {
		return false, err
	}
	stat := info.Sys().(*syscall.Stat_t)
	if int(stat.Uid) != m.uid || int(stat.Gid) != m.gid {
		if err := file.Chown(m.uid, m.gid); err != nil {
			return false, err
		}
		changed = true
	}
	// Chown can clear permission bits; apply the requested mode afterwards.
	if changed || permissionMode(info.Mode()) != m.mode {
		if err := file.Chmod(m.mode); err != nil {
			return changed, err
		}
		changed = true
	}
	return changed, nil
}

// accountFailure is the result of a failed account lookup (an owner, a group,
// or the account a process runs as): an unknown user or group is one an
// earlier account task may still create.
func (run Runtime) AccountFailure(err error) protocol.Result { return run.accountFailure(err) }

func (run Runtime) accountFailure(err error) protocol.Result {
	var unknownUser user.UnknownUserError
	var unknownGroup user.UnknownGroupError
	if errors.As(err, &unknownUser) || errors.As(err, &unknownGroup) {
		return run.notYet(err)
	}
	return protocol.Failed(err)
}

// Go stores special permission bits separately from the Unix octal mode.
func permissionMode(mode os.FileMode) os.FileMode {
	return mode & (os.ModePerm | os.ModeSetuid | os.ModeSetgid | os.ModeSticky)
}
func permissionsFromNative(mode uint32) os.FileMode {
	result := os.FileMode(mode & 0777)
	for _, bit := range []struct {
		native uint32
		goMode os.FileMode
	}{{04000, os.ModeSetuid}, {02000, os.ModeSetgid}, {01000, os.ModeSticky}} {
		if mode&bit.native != 0 {
			result |= bit.goMode
		}
	}
	return result
}
func nativePermissions(mode os.FileMode) uint32 {
	result := uint32(mode.Perm())
	for _, bit := range []struct {
		native uint32
		goMode os.FileMode
	}{{04000, os.ModeSetuid}, {02000, os.ModeSetgid}, {01000, os.ModeSticky}} {
		if mode&bit.goMode != 0 {
			result |= bit.native
		}
	}
	return result
}

// syncFileParent makes a rename or unlink durable. A failure after the change
// must be reported as changed, since the old entry is already gone.
func syncFileParent(parent *os.Root) error {
	directory, err := parent.Open(".")
	if err != nil {
		return err
	}
	return errors.Join(directory.Sync(), directory.Close())
}

// openParent pins path's parent directory and inspects path within it without
// following a link; info is nil when path does not exist. A missing parent
// returns a nil root when missingOK (a preview, or a removal).
func openParent(path string, missingOK bool) (*os.Root, os.FileInfo, error) {
	parent, err := os.OpenRoot(filepath.Dir(path))
	if os.IsNotExist(err) && missingOK {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	info, err := parent.Lstat(filepath.Base(path))
	if errors.Is(err, os.ErrNotExist) {
		return parent, nil, nil
	}
	if err != nil {
		parent.Close()
		return nil, nil, err
	}
	return parent, info, nil
}

// missingParent answers for a module that would create path when the
// directory that holds it does not exist. That directory is a missing
// prerequisite: a run fails without another try, naming it. A dryrun reports
// the node task not checked, as for other missing prerequisites, except when
// an earlier node task of the dryrun would create the directory: then it
// returns nil, and the module previews path as a new entry.
func (run Runtime) missingParent(path string) *protocol.Result {
	parent := filepath.Dir(path)
	if run.EarlierNodeTaskWouldChange(parent) {
		return nil
	}
	r := run.notYet(fmt.Errorf("the parent directory %s does not exist; create it with an earlier directory node task", parent))
	return &r
}

// missingRunFromDirectory answers for a node task whose commands run from
// directory, its run_from_directory, when that directory does not exist:
// the process could not even start. That directory is a missing
// prerequisite: a run fails without another try, naming it, and so does a
// dryrun with nothing earlier to create it. A dryrun reports the node task
// not checked when an earlier node task of the dryrun would create or change
// the directory; Engine.checkInADryrun does the same when any earlier node
// task would change the node. It returns nil when the directory exists.
func (run Runtime) missingRunFromDirectory(directory string) *protocol.Result {
	if directory == "" {
		return nil
	}
	if _, err := os.Stat(directory); !errors.Is(err, os.ErrNotExist) {
		return nil
	}
	missing := fmt.Errorf("run_from_directory %s does not exist; create it with an earlier directory node task", directory)
	if run.EarlierNodeTaskWouldChange(directory) {
		r := run.notYet(missing)
		return &r
	}
	r := protocol.MissingPrerequisite(missing)
	return &r
}

// Inspect and open entries relative to a pinned parent, rejecting type changes.
func entryInfoAt(parent *os.Root, name string, directory bool) (os.FileInfo, error) {
	if parent == nil {
		return nil, nil
	}
	info, err := parent.Lstat(name)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if directory && !info.IsDir() {
		return nil, fmt.Errorf("%s has a conflicting type; expected a directory", name)
	}
	if !directory && !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s has a conflicting type; expected a regular file", name)
	}
	return info, nil
}

func openDirectoryAt(parent *os.Root, name string, before os.FileInfo) (*os.Root, error) {
	root, err := parent.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	actual, err := root.Stat(".")
	if err == nil && !os.SameFile(before, actual) {
		err = fmt.Errorf("directory changed during inspection: %s", name)
	}
	if err == nil {
		err = sameFileAt(parent, name, before)
	}
	if err != nil {
		root.Close()
		return nil, err
	}
	return root, nil
}
