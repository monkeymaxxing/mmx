package modules

import (
	"context"
	"crypto/rand"
	"fmt"
	"hash"
	"os"
	"path/filepath"
	"reflect"
	"slices"

	"mmx/internal/protocol"
	"mmx/internal/value"
)

// A link is inspected and replaced within its pinned parent, like a file. Its
// target is never followed.
func (run Runtime) Symlink(ctx context.Context, link Symlink) (result protocol.Result) {
	if err := ValidateSymlink(link); err != nil {
		return protocol.ConfigurationError(err)
	}
	if err := run.checkFileDestination(ctx, link.Path); err != nil {
		return protocol.Failed(err)
	}
	parent, before, err := openParent(link.Path, true)
	if err != nil {
		return protocol.Failed(err)
	}
	if parent == nil && link.State != "absent" {
		if r := run.missingParent(link.Path); r != nil {
			return *r
		}
	}
	if parent != nil {
		defer parent.Close()
	}
	name := filepath.Base(link.Path)
	// replace_existing_file lets the link take the place of a regular file in
	// one rename; a directory is never replaced.
	replacesFile := before != nil && before.Mode().IsRegular() && link.ReplaceExistingFile && link.State != "absent"
	switch {
	case before == nil || before.Mode()&os.ModeSymlink != 0 || replacesFile:
	case before.IsDir():
		return protocol.Failed(fmt.Errorf("refusing to replace directory %s with a link", link.Path))
	case before.Mode().IsRegular() && link.State != "absent":
		return protocol.Failed(fmt.Errorf("refusing to replace regular file %s with a symbolic link; set replace_existing_file: true to allow replacement", link.Path))
	default:
		return protocol.Failed(fmt.Errorf("refusing to replace %s because the path is not a symbolic link", link.Path))
	}
	var old *protocol.FileState
	if replacesFile {
		old = fileState(before)
	} else if before != nil {
		target, err := parent.Readlink(name)
		if err != nil {
			return protocol.Failed(err)
		}
		old = &protocol.FileState{Kind: "symlink", Target: target}
	}
	if link.State == "absent" {
		return run.removeAt(ctx, parent, link.Path, before, old)
	}
	if !replacesFile && old != nil && old.Target == link.Target {
		return protocol.Success("unchanged")
	}
	action := "update"
	switch {
	case before == nil:
		action = "create"
	case replacesFile:
		action = "replace"
	}
	change := &protocol.FileChange{Path: link.Path, Action: action, Before: old, After: &protocol.FileState{Kind: "symlink", Target: link.Target}}
	defer func() { result.FileChange = change }()
	if run.Dryrun {
		return protocol.Success("would_change")
	}
	if err := ctx.Err(); err != nil {
		return protocol.Failed(err)
	}
	dir := ".mmx-link-" + rand.Text()
	if err := parent.Mkdir(dir, 0700); err != nil {
		return protocol.Failed(err)
	}
	defer parent.RemoveAll(dir)
	candidate := filepath.Join(dir, name)
	if err := createLabeled(ctx, link.Path, selinuxLink, func() error { return parent.Symlink(link.Target, candidate) }); err != nil {
		return protocol.Failed(err)
	}
	return run.replaceAt(ctx, parent, link.Path, candidate, before)
}

type Symlink struct {
	Path   string `yaml:"path" json:"path"`
	Target string `yaml:"target,omitempty" json:"target,omitempty"`
	State  string `yaml:"state,omitempty" json:"state,omitempty"`
	// ReplaceExistingFile lets the link take the place of a regular file.
	ReplaceExistingFile bool `yaml:"replace_existing_file,omitempty" json:"replace_existing_file,omitempty"`
}

func ValidateSymlink(l Symlink) error {
	if l.State != "" && !slices.Contains(presenceStates, l.State) {
		return fmt.Errorf("symlink.state must be present or absent")
	}
	if l.State == "absent" {
		if l.Target != "" || l.ReplaceExistingFile {
			return fmt.Errorf("absent symlink accepts only path and state")
		}
	} else if l.Target == "" {
		return fmt.Errorf("symlink.target is required")
	}
	return nil
}

// symlinkDescription is what mmx describe-module shows for symlink.
var symlinkDescription = Description{
	Name: "symlink", Topic: TopicFilesAndLinks, Schema: reflect.TypeOf(Symlink{}),
	Summary: "one symbolic link and its target text",
	Fields: []Field{
		{Name: "path", Required: true, Notes: "absolute"},
		{Name: "target", Required: true, Notes: "stored literally and may dangle; omit with absent"},
		{Name: "replace_existing_file", Default: "false", Notes: "true: a regular file at path is replaced by the link in one atomic rename; a directory at path is always an error"},
		{Name: "state", Values: presenceStates, Default: "present", Notes: "absent accepts only path and state"},
	},
	Example: "- symlink:\n    path: /srv/app/current\n    target: /srv/app/releases/1.2.3\n",
	Results: resultBasic,
	Dryrun:  "supported; previews the link action, including a regular file the link would replace; not_checked while the parent directory is missing, unless an earlier node task of the dryrun would create it",
}

// Prepare checks the inputs when the project loads.
func (l *Symlink) Prepare(base string) error {
	return ValidateSymlink(*l)
}

// Arguments lists the inputs that may hold a reference, with their rules.
func (l *Symlink) Arguments(arguments *ArgumentList) {
	arguments.Public("path", &l.Path, absolute)
	if l.Target != "" {
		arguments.Public("target", &l.Target, linkTarget)
	}
}

// CleanPaths cleans the resolved path.
func (l *Symlink) CleanPaths() {
	l.Path = filepath.Clean(l.Path)
}

// Run does the module's work on the node.
func (l *Symlink) Run(ctx context.Context, task NodeTask) protocol.Result {
	return task.Runtime.Symlink(ctx, *l)
}

// ManagedPaths lists the paths the task manages or says it creates, as
// written; a dryrun cannot check a later task's path_exists on them.
func (l *Symlink) ManagedPaths() []string {
	return []string{l.Path}
}

// HashConfiguration hashes the link's target, or that it is absent.
func (l *Symlink) HashConfiguration(ctx context.Context, h hash.Hash, apply bool, s value.Scope) error {
	if apply && l.State == "absent" {
		_, _ = h.Write([]byte{0})
		return nil
	}
	target := l.Target
	if !apply {
		st, err := os.Lstat(l.Path)
		if os.IsNotExist(err) {
			_, _ = h.Write([]byte{0})
			return nil
		}
		if err != nil {
			return err
		}
		if st.Mode()&os.ModeSymlink == 0 {
			// A file that replace_existing_file lets the link replace.
			_, _ = h.Write([]byte{3})
			return nil
		}
		if target, err = os.Readlink(l.Path); err != nil {
			return err
		}
	}
	_, _ = h.Write([]byte{2})
	HashField(h, target)
	return nil
}

// TrackedInputs lists the link.
func (l *Symlink) TrackedInputs() []TrackedInput {
	return []TrackedInput{{Kind: "config", Path: l.Path, Content: l}}
}
