package modules

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"mmx/internal/facts"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"golang.org/x/sys/unix"
)

// MMX's file writers give every file, directory, and link they create the
// SELinux label the node's policy has for its final path, before the entry is
// visible there. A new entry otherwise takes its directory's label, which is
// wrong wherever the policy labels a path by its name, such as /var/www or
// ~/.ssh/authorized_keys. MMX asks the policy with matchpathcon and has no
// label settings of its own.

// selinuxKinds are matchpathcon's names for the entry kinds MMX creates.
const (
	selinuxFile      = "file"
	selinuxDirectory = "dir"
	selinuxLink      = "lnk_file"
)

// selinuxLabelsApply reports whether the node's kernel runs SELinux with a
// loaded policy, so that new entries need the policy's labels. It reads the
// node's own files, never the paths the SELinux modules' tests move. When
// the files cannot be read it reports true, so matchpathcon's error reaches
// the result.
func selinuxLabelsApply() bool {
	runs, err := facts.SELinuxRuns(os.ReadFile, "/sys/fs/selinux/enforce", "/proc/self/attr/current")
	return runs || err != nil
}

// selinuxLabels returns the policy's label for each path as an entry of one
// kind. A label is empty where the policy has none for the path. The result
// is nil when SELinux is not enabled.
func selinuxLabels(ctx context.Context, kind string, paths []string) ([]string, error) {
	if !selinuxLabelsApply() || len(paths) == 0 {
		return nil, nil
	}
	out, err := exec.CommandContext(ctx, "matchpathcon", append([]string{"-n", "-m", kind}, paths...)...).Output()
	if errors.Is(err, exec.ErrNotFound) {
		return nil, errors.New("SELinux is enabled on this node, but matchpathcon is not installed; install the package libselinux-utils")
	}
	if err != nil {
		return nil, fmt.Errorf("matchpathcon: %w", err)
	}
	labels := strings.Split(strings.TrimSuffix(string(out), "\n"), "\n")
	if len(labels) != len(paths) {
		return nil, fmt.Errorf("matchpathcon returned %d labels for %d paths", len(labels), len(paths))
	}
	for i, label := range labels {
		if label == "<<none>>" {
			labels[i] = ""
		}
	}
	return labels, nil
}

// createLabeled runs create, which makes one entry of the given kind. With
// SELinux enabled, the kernel gives the new entry the policy's label for
// finalPath as it creates it. The label is a setting of the calling thread,
// so the thread stays locked until the setting is cleared.
func createLabeled(ctx context.Context, finalPath, kind string, create func() error) error {
	labels, err := selinuxLabels(ctx, kind, []string{finalPath})
	if err != nil {
		return err
	}
	if len(labels) == 0 || labels[0] == "" {
		return create()
	}
	runtime.LockOSThread()
	fd, err := unix.Open("/proc/thread-self/attr/fscreate", unix.O_WRONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		runtime.UnlockOSThread()
		return fmt.Errorf("set the SELinux label for %s: %w", finalPath, err)
	}
	defer unix.Close(fd)
	if _, err := unix.Write(fd, []byte(labels[0])); err != nil {
		runtime.UnlockOSThread()
		return fmt.Errorf("set the SELinux label %s for %s: %w", labels[0], finalPath, err)
	}
	err = create()
	// An empty write clears the setting. If that fails, the thread stays
	// locked to this goroutine and ends with it, so no later file gets the label.
	if _, clearErr := unix.Write(fd, nil); clearErr != nil {
		return errors.Join(err, fmt.Errorf("clear the SELinux label setting: %w", clearErr))
	}
	runtime.UnlockOSThread()
	return err
}

// labelTree gives every entry of the staged directory tree the label the
// policy has for its place below finalPath. The tree is private until its
// rename, so labels are set after it is built: one policy lookup per kind,
// not one per entry.
func labelTree(ctx context.Context, parent *os.Root, staging, finalPath string) error {
	if !selinuxLabelsApply() {
		return nil
	}
	root, err := parent.Open(staging)
	if err != nil {
		return err
	}
	defer root.Close()
	// Paths through the open directory cannot be redirected by a change above it.
	held := fmt.Sprintf("/proc/self/fd/%d", root.Fd())
	entries := map[string][]string{}
	err = filepath.WalkDir(held+"/.", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		kind := selinuxFile
		switch {
		case d.IsDir():
			kind = selinuxDirectory
		case d.Type()&fs.ModeSymlink != 0:
			kind = selinuxLink
		}
		rel, err := filepath.Rel(held, path)
		entries[kind] = append(entries[kind], rel)
		return err
	})
	if err != nil {
		return err
	}
	for kind, names := range entries {
		// Stay far below the kernel's limit on one command's arguments.
		for start := 0; start < len(names); start += 500 {
			batch := names[start:min(start+500, len(names))]
			paths := make([]string, len(batch))
			for i, rel := range batch {
				paths[i] = filepath.Join(finalPath, rel)
			}
			labels, err := selinuxLabels(ctx, kind, paths)
			if err != nil {
				return err
			}
			for i, label := range labels {
				if label == "" {
					continue
				}
				if err := setLabelBeneath(int(root.Fd()), batch[i], label, setSELinuxLabel); err != nil {
					return fmt.Errorf("set the SELinux label %s on %s: %w", label, paths[i], err)
				}
			}
		}
	}
	return nil
}

// setSELinuxLabel is the system call that stores a label; tests replace it.
func setSELinuxLabel(path, label string) error {
	return unix.Lsetxattr(path, "security.selinux", []byte(label), 0)
}

// setLabelBeneath labels the entry rel of the directory open at rootFd. The
// account that owns the staged tree can swap one of its directories for a
// symlink while the labels are set, and root must not label what the link
// points to. So the entry's parent directory is opened with a resolution that
// refuses every symlink and every step out of the tree, and only the last
// part is a name: setting a label on a name does not follow a symlink there.
func setLabelBeneath(rootFd int, rel, label string, set func(path, label string) error) error {
	if rel == "." {
		// The trailing "/." makes the path the directory, not the /proc link to it.
		return set(fmt.Sprintf("/proc/self/fd/%d/.", rootFd), label)
	}
	parentFd, err := unix.Openat2(rootFd, filepath.Dir(rel), &unix.OpenHow{
		Flags:   unix.O_PATH | unix.O_NOFOLLOW | unix.O_DIRECTORY | unix.O_CLOEXEC,
		Resolve: unix.RESOLVE_BENEATH | unix.RESOLVE_NO_SYMLINKS,
	})
	if err != nil {
		return err
	}
	defer unix.Close(parentFd)
	return set(fmt.Sprintf("/proc/self/fd/%d/%s", parentFd, filepath.Base(rel)), label)
}
