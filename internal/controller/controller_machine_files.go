package controller

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"syscall"

	"mmx/internal/config"
	"mmx/internal/durable"
	"mmx/internal/modules"
	"mmx/internal/protocol"
	"mmx/internal/value"
)

// ControllerMachineFile is one file that a fleet stage's
// save_on_controller_machine entry wrote, or in dryrun would write, on the
// controller machine. Its result never holds a secret value.
type ControllerMachineFile struct {
	// SaveOnControllerMachineIndex is the position of the entry in the fleet
	// stage's save_on_controller_machine list, from 0.
	SaveOnControllerMachineIndex int `json:"save_on_controller_machine_index"`
	// Node is the node whose output an output entry wrote; a template entry
	// writes one file for the whole fleet stage.
	Node   string          `json:"node,omitempty"`
	Path   string          `json:"path,omitempty"`
	Result protocol.Result `json:"result"`
}

// controllerMachineFilesUnfinished reports a file that a resume still has to write.
func (sel *Selection) controllerMachineFilesUnfinished() bool {
	for _, file := range sel.ControllerMachineFiles {
		if !protocol.Completed(file.Result.Status) {
			return true
		}
	}
	return false
}

// controllerMachineFileFailed reports a file that could not be written.
func (sel *Selection) controllerMachineFileFailed() bool {
	for _, file := range sel.ControllerMachineFiles {
		if protocol.Failure(file.Result.Status) {
			return true
		}
	}
	return false
}

// saveOnControllerMachine writes a fleet stage's save_on_controller_machine
// files once its nodes have finished: for an output entry one file for each
// node whose fleet stage succeeded, for a template entry one file from the
// whole output collection. The controller process writes them; no task runs on
// the controller machine. Dryrun reports each file and writes nothing. A file
// this run already wrote, and that still exists, is kept: mmx resume writes
// the files that are still missing, reading a read_file secret again if one
// is needed.
func (w *fleetStageWalker) saveOnControllerMachine(fleetStage config.FleetStage, sel *Selection, scope value.Scope) error {
	if len(fleetStage.SaveOnControllerMachine) == 0 || sel.Loop || !sel.Resolved {
		return nil
	}
	type key struct {
		entry int
		node  string
	}
	previous := map[key]ControllerMachineFile{}
	for _, file := range sel.ControllerMachineFiles {
		previous[key{file.SaveOnControllerMachineIndex, file.Node}] = file
	}
	var files []ControllerMachineFile
	for i, entry := range fleetStage.SaveOnControllerMachine {
		save := func(node string) {
			if !w.run.Dryrun && controllerMachineFileKept(sel, i, node) {
				files = append(files, previous[key{i, node}])
				return
			}
			path, result := w.saveFile(fleetStage.ID, entry, i, node, scope)
			files = append(files, ControllerMachineFile{SaveOnControllerMachineIndex: i, Node: node, Path: path, Result: result})
		}
		if entry.Output == "" {
			save("")
			continue
		}
		for _, j := range w.run.selectionEntries(sel.Path) {
			if en := w.run.Entries[j]; en.Response.Status == "succeeded" {
				save(en.Node)
			}
		}
	}
	sel.ControllerMachineFiles = files
	return w.controller.saveSelection(w.run.ID, sel)
}

// controllerMachineFileKept reports a file of entry index for node ("" for a
// template entry) that this run already wrote and that still exists: resume
// keeps it.
func controllerMachineFileKept(sel *Selection, index int, node string) bool {
	for _, file := range sel.ControllerMachineFiles {
		if file.SaveOnControllerMachineIndex == index && file.Node == node {
			_, err := os.Stat(file.Path)
			return file.Result.Status == "succeeded" && err == nil
		}
	}
	return false
}

// saveFile writes one file, or in dryrun compares it, and returns where it
// belongs and the result. Read permissions for a secret require explicit acknowledgements; content
// goes from the controller process's memory to the file and its result shows
// no content or difference.
func (w *fleetStageWalker) saveFile(fleetStageID string, entry config.SaveOnControllerMachine, index int, node string, scope value.Scope) (string, protocol.Result) {
	field := config.ControllerMachineFileField(index)
	local, trace := scope.TraceReads() // Only the saved content decides its read permission.
	if node != "" {
		local = local.Local()
		local["node"] = map[string]any{"name": node}
	}
	fail := func(r protocol.Result) protocol.Result {
		r.Task, r.Secret = field, trace.Used
		if r.Status == "blocked" && w.run.Dryrun {
			r.Status = "not_checked"
		}
		return r
	}
	// A path is public: it appears in output and saved history.
	resolved, err := local.Resolve(entry.Path)
	if err == nil && value.HasSecrets(resolved) {
		err = fmt.Errorf("the destination file path would contain a secret value; file paths appear in public output and saved history, so put the secret in the file content")
	}
	var written, full string
	if err == nil {
		written, err = value.Scalar(resolved)
	}
	if err == nil {
		full, err = config.ControllerMachinePath(entry.ProjectRoot, written)
	}
	if err != nil {
		return "", fail(protocol.ConfigurationError(fmt.Errorf("%s.path: %w", field, err)))
	}
	var content []byte
	if entry.Output != "" {
		var v any
		v, err = local.Get(".outputs." + fleetStageID + "[" + strconv.Quote(node) + "]." + entry.Output)
		if err == nil {
			content, err = controllerFileContent(entry.Output, v)
		}
	} else {
		var text string
		text, err = local.Template(entry.TemplateText)
		content = []byte(text)
	}
	if err != nil {
		return full, fail(protocol.Failed(fmt.Errorf("%s: %w", field, err)))
	}
	mode, _ := strconv.ParseUint(entry.PermissionsMode, 8, 32)
	if trace.Used {
		if err := modules.ValidateSecretFilePermissions(full, os.FileMode(mode), "", "controller machine", entry.SecretReadableByGroup, entry.DangerouslyLetEveryAccountReadSecret); err != nil {
			return full, fail(protocol.ConfigurationError(fmt.Errorf("%s: %w", field, err)))
		}
	}
	r := protocol.Result{Task: field, Status: "succeeded", Change: "unchanged", Secret: trace.Used}
	change, err := writeControllerMachineFile(full, content, os.FileMode(mode), w.run.Dryrun)
	if err != nil {
		return full, fail(protocol.Failed(fmt.Errorf("%s: %w", field, err)))
	}
	if change != nil {
		r.FileChange, r.Change = change, "changed"
		if w.run.Dryrun {
			r.Change = "would_change"
		}
	}
	return full, r
}

// controllerFileContent is the bytes an output entry writes: text as it is,
// binary content whole, and a number or boolean as its text.
func controllerFileContent(output string, v any) ([]byte, error) {
	switch x := value.Plain(v).(type) {
	case value.Binary:
		return x.Bytes, nil
	case map[string]any, []any:
		return nil, fmt.Errorf("output %s is a mapping or a list; use a template entry to format the output as file content", output)
	}
	text, err := value.Scalar(v)
	if err != nil {
		return nil, fmt.Errorf("output %s: %w", output, err)
	}
	return []byte(text), nil
}

// writeControllerMachineFile replaces the file at path with content and mode,
// creating its directories, and returns the change, or nil when the file
// already holds exactly that. With dryrun it only compares. durable.Write
// writes a private temporary file, flushes it, and renames it into place, so
// a crash leaves the old file or the new one.
func writeControllerMachineFile(path string, content []byte, mode os.FileMode, dryrun bool) (*protocol.FileChange, error) {
	var before os.FileInfo
	var old []byte
	info, err := os.Stat(path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return nil, err
	case !info.Mode().IsRegular():
		return nil, fmt.Errorf("%s exists and is not a regular file", path)
	default:
		if old, err = os.ReadFile(path); err != nil {
			return nil, err
		}
		before = info
		if bytes.Equal(old, content) && info.Mode().Perm() == mode {
			return nil, nil
		}
	}
	change := &protocol.FileChange{Path: path, Action: "create", Before: controllerFileState(before), After: &protocol.FileState{Kind: "file", Size: int64(len(content)), Mode: fmt.Sprintf("%04o", mode), UID: os.Getuid(), GID: os.Getgid()}}
	if before != nil {
		change.Action = "update"
	}
	modules.TextDiff(change, old, content)
	if dryrun {
		return change, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return nil, err
	}
	if err := durable.Write(path, content, mode); err != nil {
		return nil, err
	}
	return change, nil
}

func controllerFileState(info os.FileInfo) *protocol.FileState {
	if info == nil {
		return nil
	}
	state := &protocol.FileState{Kind: "file", Size: info.Size(), Mode: fmt.Sprintf("%04o", info.Mode().Perm())}
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		state.UID, state.GID = int(st.Uid), int(st.Gid)
	}
	return state
}
