//go:build !linux

package modules

import "os"

// mountRoot exists only on Linux nodes; local tests on the controller's
// operating system rely on the device comparison.
func mountRoot(int, string) (bool, error) { return false, nil }

// noCopyOnWrite matters only on Linux nodes, for btrfs.
func noCopyOnWrite(*os.File) error { return nil }
