//go:build !linux

package modules

import (
	"os"
	"syscall"
)

// symbolicLink reads the link at path, its owner and its target. Nodes run
// Linux, where one descriptor of the link gives both; local tests on the
// controller's operating system read them one after the other.
func symbolicLink(path string) (owner uint32, target string, ok bool, err error) {
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		return 0, "", false, err
	}
	target, err = os.Readlink(path)
	if err != nil {
		return 0, "", false, err
	}
	return info.Sys().(*syscall.Stat_t).Uid, target, true, nil
}
