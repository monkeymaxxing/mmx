package durable

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// Write replaces path durably: temporary write, fsync, rename, directory fsync.
func Write(path string, b []byte, mode os.FileMode) error { return replace(path, b, mode, true) }

// Replace replaces path atomically without fsync: readers see the old or the
// new bytes, and a machine crash can lose the change. Use it only for state
// recovery can do without, such as a live progress view.
func Replace(path string, b []byte, mode os.FileMode) error { return replace(path, b, mode, false) }

func replace(path string, b []byte, mode os.FileMode, sync bool) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".mmx-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(mode); err == nil {
		_, err = f.Write(b)
	}
	if err == nil && sync {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = os.Rename(f.Name(), path); err != nil || !sync {
		return err
	}
	d, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
func JSON(path string, value any) error {
	b, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return Write(path, append(b, '\n'), 0600)
}
