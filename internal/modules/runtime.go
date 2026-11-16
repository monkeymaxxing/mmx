// Package modules implements node resources. It does not import the task engine,
// controller, transports, or saved-run storage.
package modules

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"mmx/internal/assets"
	"mmx/internal/protocol"
	"mmx/internal/value"
)

// ControllerConnection is the connection the node observed when admitting a
// node job. NodeLocalAddress is the local IP of that accepted connection, not
// an address supplied in the controller's request.
type ControllerConnection struct {
	Kind             protocol.ConnectionKind `json:"connection_kind"`
	SourceAddress    string                  `json:"source_address"`
	NodeLocalAddress string                  `json:"node_local_address"`
}

// Runtime is the execution policy shared by resource functions. Context carries
// cancellation and deadlines; the engine owns retries, progress, and secrets.
type Runtime struct {
	// ControllerConnection comes from the node admission record.
	ControllerConnection ControllerConnection

	CheckFileDestination func(context.Context, string) error
	RetainDiff           func(string) (*assets.Ref, string)
	Dryrun               bool
	BeforeMutation       func(string) error
	// ResolveShell resolves the public arguments of a nested shell task, such
	// as check_new_file's.
	ResolveShell func(Shell, value.Scope) (Shell, error)
	// PrivateTaskDirectoryRoot returns the directory in which a node task's
	// private directory is made, /run/mmx/private_task_directories on a
	// node, which the node makes and checks. makePrivateTaskDirectory makes
	// the task's own directory in it.
	PrivateTaskDirectoryRoot func() (string, error)
	// PreviewPaths: in a dryrun, the paths that earlier node tasks of the
	// node job would create or change.
	PreviewPaths PreviewPaths
	// EarlierChange: in a dryrun, the name of the first earlier node task on
	// this node that would change the node, or "" when none would.
	EarlierChange string
}

// PreviewPaths are the paths that earlier node tasks of a dryrun would create
// or change.
type PreviewPaths []string

// WouldChange reports that an earlier node task would create or change path:
// the path itself, a directory that holds it, or something inside it. Every
// reader of the list asks this one question, so a parent directory that an
// earlier node task changes, such as a Git checkout, counts everywhere.
func (p PreviewPaths) WouldChange(path string) bool {
	path = filepath.Clean(path)
	for _, changing := range p {
		changing = filepath.Clean(changing)
		if path == changing || strings.HasPrefix(path, strings.TrimSuffix(changing, "/")+"/") || strings.HasPrefix(changing, strings.TrimSuffix(path, "/")+"/") {
			return true
		}
	}
	return false
}

func (r Runtime) Mutation(resource string) error {
	if r.Dryrun {
		return fmt.Errorf("internal error: mutation requested during dryrun")
	}
	if r.BeforeMutation != nil {
		return r.BeforeMutation(resource)
	}
	return nil
}

// notYet is the result when something an earlier task may still provide (a
// package, an account, a file) is missing: dryrun cannot check the task yet,
// and a run fails without another try. When an earlier node task of the
// dryrun would change the node, the reason names that node task after what
// is missing, so the reader does not add a node task for something the
// earlier one provides, such as a directory that a package creates.
func (r Runtime) notYet(err error) protocol.Result {
	if r.Dryrun {
		reason := err.Error()
		if r.EarlierChange != "" {
			reason = EarlierChangeMayProvide(r.EarlierChange, reason)
		}
		return protocol.Result{Status: "not_checked", Change: "unknown", Reason: reason}
	}
	return protocol.MissingPrerequisite(err)
}

// EarlierChangeMayProvide is the reason of a node task that a dryrun cannot
// check because something is missing, after an earlier node task that would
// change the node first: missing is the module's own reason with its advice,
// and earlierTask names that node task. The fact comes first; the earlier
// node task and what to do when it provides the missing thing follow.
func EarlierChangeMayProvide(earlierTask, missing string) string {
	return strings.TrimSuffix(missing, ".") + ". The earlier node task " + earlierTask +
		" would change this node first and may provide what is missing; if the run then succeeds, change nothing."
}
