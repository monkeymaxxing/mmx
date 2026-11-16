//go:build !linux

package modules

import (
	"context"
	"os"
)

// SELinux exists only on Linux nodes; local tests on the controller's
// operating system create entries without labels.
const (
	selinuxFile      = "file"
	selinuxDirectory = "dir"
	selinuxLink      = "lnk_file"
)

func createLabeled(_ context.Context, _, _ string, create func() error) error { return create() }

func labelTree(context.Context, *os.Root, string, string) error { return nil }
