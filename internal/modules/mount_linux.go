package modules

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// mountRoot reports whether the entry name in the directory dirfd is the root
// of a mount, a bind mount of the same filesystem included, which a device
// comparison alone cannot tell. A kernel without the statx attribute (before
// Linux 5.8) reports false; the device comparison still catches a mount of
// another filesystem.
func mountRoot(dirfd int, name string) (bool, error) {
	var st unix.Statx_t
	if err := unix.Statx(dirfd, name, unix.AT_SYMLINK_NOFOLLOW|unix.AT_NO_AUTOMOUNT, 0, &st); err != nil {
		return false, err
	}
	return st.Attributes_mask&unix.STATX_ATTR_MOUNT_ROOT != 0 && st.Attributes&unix.STATX_ATTR_MOUNT_ROOT != 0, nil
}

// noCopyOnWriteFlag is FS_NOCOW_FL of linux/fs.h.
const noCopyOnWriteFlag = 0x00800000

// noCopyOnWrite turns copy-on-write off for an empty file on btrfs, which
// swaps only to such a file; on any other filesystem it does nothing.
func noCopyOnWrite(f *os.File) error {
	var st unix.Statfs_t
	if err := unix.Fstatfs(int(f.Fd()), &st); err != nil {
		return err
	}
	if st.Type != unix.BTRFS_SUPER_MAGIC {
		return nil
	}
	flags, err := unix.IoctlGetUint32(int(f.Fd()), unix.FS_IOC_GETFLAGS)
	if err == nil {
		err = unix.IoctlSetPointerInt(int(f.Fd()), unix.FS_IOC_SETFLAGS, int(flags|noCopyOnWriteFlag))
	}
	if err != nil {
		return fmt.Errorf("turn copy-on-write off for the swap file on btrfs: %w", err)
	}
	return nil
}
