package facts

import (
	"fmt"
	"math"

	"golang.org/x/sys/unix"
)

func mountCapacity(path string) (int64, int64, error) {
	var stat unix.Statfs_t
	if err := unix.Statfs(path, &stat); err != nil {
		return 0, 0, err
	}
	size := stat.Frsize
	if size == 0 {
		size = stat.Bsize
	}
	if size < 1 || stat.Blocks > uint64(math.MaxInt64/size) || stat.Bavail > uint64(math.MaxInt64/size) {
		return 0, 0, fmt.Errorf("invalid filesystem capacity")
	}
	return int64(stat.Blocks) * size, int64(stat.Bavail) * size, nil
}
