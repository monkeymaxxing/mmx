package modules

import (
	"context"
	"encoding/binary"
	"hash"
	"io"

	"mmx/internal/value"
)

// TrackedInput is one input of a node task block that the block's services
// and commands follow: a configuration file, link, or directory; a unit file;
// a package; a Git checkout; or what a language or tool installer installed.
// The node task execution engine keeps a hash of each in its service records,
// keyed by Key.
//
// A core module's inputs list their tracked inputs with a method
// TrackedInputs() []TrackedInput. A node task in a block's changes whose
// inputs have that method is tracked. Inputs tracked only in some cases, such
// as a file that keeps its existing content, opt out with a method
// TrackedInChanges() bool that returns false in the other cases.
type TrackedInput struct {
	// Kind is config, unit, package, git, installed, or module (the
	// fingerprint a module of the project's own reports). A changed unit,
	// package, checkout, or installation restarts a service; changed
	// configuration uses the service task's refresh.
	Kind string
	// Path is the file, link, or directory of a config or unit input, and the
	// directory of a checkout. For an installed input it names the module and
	// what it installs into, such as "pip_package in /srv/app/venv".
	Path string
	// PackageName is the package of a package input.
	PackageName string
	// UnitAccount and UnitName name the unit that a unit file defines, in the
	// systemd of the account ("" for the system's).
	UnitAccount, UnitName string
	// Content hashes a config or unit input.
	Content ConfigurationContent
	// EditsInPlace: the task edits part of a file (line, edit_file); several
	// such tasks may edit one file, which counts once.
	EditsInPlace bool
	// State reads a git or installed input from the node.
	State NodeState
	// Scope is the scope of the node task of a git or installed input. The
	// node task execution engine sets it as the node task block starts, and
	// State reads the input in it.
	Scope value.Scope
}

// OwnsPath reports a config or unit input. Its node task owns the whole path,
// so no other node task that may run writes the path, except edits in place.
func (in TrackedInput) OwnsPath() bool {
	return in.Kind == "config" || in.Kind == "unit"
}

// NodeState is a tracked input that MMX reads by asking a tool on the node:
// the commit of a checkout, or what an installer installed.
type NodeState interface {
	// ReadState returns the input's state now, "" when there is none yet. s
	// is the scope of the input's node task: an installer resolves the
	// task's env in it, so that its tool lists what the task installed,
	// such as the gems in GEM_HOME.
	ReadState(ctx context.Context, s value.Scope) (string, error)
}

// Key names the input in a service record.
func (in TrackedInput) Key() string {
	if in.Kind == "package" {
		return "package:" + in.PackageName
	}
	return in.Kind + ":" + in.Path
}

// ConfigurationContent is a config or unit input: it hashes what is at its
// path now, or, with apply, what its task would leave there, resolving its
// content in s.
type ConfigurationContent interface {
	HashConfiguration(ctx context.Context, h hash.Hash, apply bool, s value.Scope) error
}

// PackageExpectation is the state and version a package task would leave a
// package in; an empty version means some version.
type PackageExpectation struct {
	State   string
	Version string
}

// HashField writes one length-prefixed field.
func HashField(h hash.Hash, s string) {
	_ = binary.Write(h, binary.BigEndian, uint64(len(s)))
	_, _ = io.WriteString(h, s)
}

// LiteralFile is file content that a dryrun preview composed, such as the
// text a line task would leave.
func LiteralFile(path, text string) ConfigurationContent {
	return &File{Path: path, Literal: &text}
}
