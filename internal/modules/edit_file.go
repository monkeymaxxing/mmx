package modules

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"

	"mmx/internal/protocol"
	"mmx/internal/value"
)

// EditFile changes the text of an existing regular file through the task's
// content filter. Like line, it installs the result through fileAt, so the
// diff, check_new_file, and atomic replacement are the file module's, and the
// file keeps its owner, group, and mode.
func (run Runtime) EditFile(ctx context.Context, e EditFile, s value.Scope) protocol.Result {
	if err := ValidateEditFile(e); err != nil {
		return protocol.ConfigurationError(err)
	}
	// An earlier task, such as a package, may create the file.
	missing := fmt.Errorf("edit_file changes an existing file; %s does not exist", e.Path)
	parent, err := os.OpenRoot(filepath.Dir(e.Path))
	if os.IsNotExist(err) {
		return run.notYet(missing)
	}
	if err != nil {
		return protocol.Failed(err)
	}
	defer parent.Close()
	name := filepath.Base(e.Path)
	info, err := entryInfoAt(parent, name, false)
	if err != nil {
		return protocol.Failed(err)
	}
	if info == nil {
		return run.notYet(missing)
	}
	current, err := readTextAt(parent, name)
	if err != nil {
		return protocol.Failed(err)
	}
	next, err := FilterText(ctx, e, s, current)
	if err != nil {
		return protocol.Failed(err)
	}
	return run.fileAt(ctx, File{Path: e.Path, Literal: &next, CheckNewFile: e.CheckNewFile}, s, parent, 0644, nil)
}

// FilterText returns text as e's content filter rewrites it. A filter whose
// output it would rewrite again would change the file on every run, so that
// is an error before anything is written. So is no output for a file that is
// not empty: a filter that edits the file itself, such as sed -i, prints
// nothing, and installing that would empty the file.
func FilterText(ctx context.Context, e EditFile, s value.Scope, text string) (string, error) {
	next, err := runContentFilter(ctx, e, s, text)
	if err == nil && next == "" && text != "" {
		return "", errors.New(`edit_file.content_filter printed nothing for a file that is not empty; the filter must print the whole new content on standard output and change nothing itself (for example sed without -i); to empty a file, use a file task with content: ""; nothing was written`)
	}
	if err != nil || next == text {
		return next, err
	}
	again, err := runContentFilter(ctx, e, s, next)
	if err != nil {
		return "", err
	}
	if again != next {
		return "", errors.New("edit_file.content_filter would change the file on every run: filtering the proposed content changes the content again; nothing was written")
	}
	return next, nil
}

// runContentFilter runs the filter as root with text on standard input and
// returns its standard output, at most 1 MiB, like the file it replaces.
func runContentFilter(ctx context.Context, e EditFile, s value.Scope, text string) (string, error) {
	filter := e.ContentFilter.Shell
	env, _, stop := Runtime{}.commandEnvironment("edit_file.content_filter", "", filter.Env, s)
	if stop != nil {
		return "", errors.New(stop.Reason)
	}
	p := runProcess(ctx, env, "", strings.NewReader(text), nil, "/bin/bash", "-c", filter.Script)
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if p.Result.Status != "succeeded" {
		message := fmt.Sprintf("edit_file.content_filter failed with exit code %d", p.Result.ExitCode)
		if stderr := strings.TrimSpace(p.Result.Stderr); stderr != "" {
			message += ": " + stderr
		}
		return "", errors.New(message)
	}
	if p.Truncated {
		return "", fmt.Errorf("edit_file.content_filter wrote more than 1 MiB; edit_file changes files up to 1 MiB")
	}
	return string(p.Output), nil
}

// EditFile changes the text of an existing file through a filter command:
// the filter reads the current content on standard input and writes the new
// content to standard output.
type EditFile struct {
	Path          string         `yaml:"path" json:"path"`
	ContentFilter *ContentFilter `yaml:"content_filter" json:"content_filter"`
	CheckNewFile  *Check         `yaml:"check_new_file,omitempty" json:"check_new_file,omitempty"`
}

// ContentFilter is edit_file's filter: a shell script with its environment.
type ContentFilter struct {
	Shell *FilterShell `yaml:"shell" json:"shell"`
}

type FilterShell struct {
	Script string         `yaml:"script" json:"script"`
	Env    map[string]any `yaml:"env,omitempty" json:"env,omitempty"`
}

// ValidateEditFile checks an edit_file task's fields.
func ValidateEditFile(e EditFile) error {
	if e.ContentFilter == nil || e.ContentFilter.Shell == nil {
		return invalid("content_filter", "edit_file.content_filter is required: a shell mapping whose script reads the file on standard input and writes the new content to standard output")
	}
	if e.ContentFilter.Shell.Script == "" {
		return invalid("content_filter", "edit_file.content_filter.shell.script is required")
	}
	if err := validateEnvNames(e.ContentFilter.Shell.Env); err != nil {
		return err
	}
	return validateCheck(e.CheckNewFile)
}

// editFileDescription is what mmx describe-module shows for edit_file.
var editFileDescription = Description{
	Name: "edit_file", Topic: TopicFilesAndLinks, Schema: reflect.TypeOf(EditFile{}),
	Summary: "the text of an existing file, changed by a filter command the author writes; for edits line cannot make; prefer a drop-in, a whole file, or line",
	Fields: []Field{
		{Name: "path", Required: true, Notes: "absolute; an existing regular file up to 1 MiB, whose owner, group, and permissions are kept"},
		{Name: "content_filter", Required: true, Notes: "{shell: {script, env}}: the script runs as root, reads the file on standard input, and writes the wanted content to standard output; it must change nothing itself; MMX runs it again on its own output and fails the task when that changes the content again"},
		{Name: "check_new_file", Notes: "as for file"},
	},
	Example: "- edit_file:\n    path: /etc/fstab\n    content_filter:\n      shell:\n        script: sed -E 's/^([^#].*[[:space:]]swap[[:space:]].*)$/#\\1/'\n",
	Results: resultBasic,
	Dryrun:  "supported; runs the filter and previews the edit as a file diff; a validator runs only its own dryrun command",
}

// Prepare checks the inputs when the project loads.
func (e *EditFile) Prepare(base string) error {
	return ValidateEditFile(*e)
}

// Arguments lists the inputs that may hold a reference, with their rules.
func (e *EditFile) Arguments(arguments *ArgumentList) {
	arguments.Public("path", &e.Path, absolute)
	if e.ContentFilter != nil && e.ContentFilter.Shell != nil {
		for _, k := range slices.Sorted(maps.Keys(e.ContentFilter.Shell.Env)) {
			arguments.Private("content_filter.shell.env."+k, e.ContentFilter.Shell.Env[k], environmentValue)
		}
	}
}

// CleanPaths cleans the resolved path.
func (e *EditFile) CleanPaths() {
	e.Path = filepath.Clean(e.Path)
}

// Reads lists the check of a new file.
func (e *EditFile) Reads() Reads {
	r := Reads{Commands: map[string]*Shell{}, ShellScripts: map[string]string{}}
	if e.ContentFilter != nil && e.ContentFilter.Shell != nil {
		r.ShellScripts["content_filter.shell.script"] = e.ContentFilter.Shell.Script
	}
	if e.CheckNewFile != nil {
		r.Commands["check_new_file.shell"] = e.CheckNewFile.Shell
	}
	return r
}

// Run does the module's work on the node.
func (e *EditFile) Run(ctx context.Context, task NodeTask) protocol.Result {
	return task.Runtime.EditFile(ctx, *e, task.Scope)
}

// ManagedPaths lists the paths the task manages or says it creates, as
// written; a dryrun cannot check a later task's path_exists on them.
func (e *EditFile) ManagedPaths() []string {
	return []string{e.Path}
}

// TrackedInputs lists the edited file, hashed as it is on disk.
func (e *EditFile) TrackedInputs() []TrackedInput {
	return []TrackedInput{{Kind: "config", Path: e.Path, Content: &File{Path: e.Path}, EditsInPlace: true}}
}

// EditText returns text as the task would edit it.
func (e *EditFile) EditText(ctx context.Context, text string, s value.Scope) (string, error) {
	return FilterText(ctx, *e, s, text)
}
