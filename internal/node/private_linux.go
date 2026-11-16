package node

import (
	"fmt"

	"golang.org/x/sys/unix"
)

func requireMemoryFilesystem(path string) error {
	var stat unix.Statfs_t
	if err := unix.Statfs(path, &stat); err != nil {
		return err
	}
	if stat.Type != unix.TMPFS_MAGIC {
		return fmt.Errorf("MMX keeps secret job inputs and the logins that node tasks hand their tools under /run, which must be a memory filesystem (tmpfs) on the node")
	}
	return nil
}
