package modules

import (
	"os"

	"golang.org/x/sys/unix"
)

// symbolicLink reads the link at path, its owner and its target, from one
// descriptor of the link itself, so that a link swapped in at path between
// the two reads cannot pair one link's owner with another link's target. ok
// is false when path is no longer a link.
func symbolicLink(path string) (owner uint32, target string, ok bool, err error) {
	fd, err := unix.Open(path, unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return 0, "", false, &os.PathError{Op: "open", Path: path, Err: err}
	}
	defer unix.Close(fd)
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return 0, "", false, &os.PathError{Op: "stat", Path: path, Err: err}
	}
	if st.Mode&unix.S_IFMT != unix.S_IFLNK {
		return 0, "", false, nil
	}
	buffer := make([]byte, unix.PathMax)
	n, err := unix.Readlinkat(fd, "", buffer)
	if err != nil {
		return 0, "", false, &os.PathError{Op: "readlink", Path: path, Err: err}
	}
	return st.Uid, string(buffer[:n]), true, nil
}
