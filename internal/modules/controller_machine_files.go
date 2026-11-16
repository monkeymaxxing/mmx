package modules

import (
	"fmt"
	"io"
	"mmx/internal/assets"
	"os"
	"path/filepath"
	"strings"
)

// The files a task names on the controller machine, read when the project
// loads: sources, templates, keys, and directory trees.

// controllerPath resolves a source or template path on the controller machine.
// A relative path starts at base, the role's directory inside a role and the
// project root otherwise, and may leave the role while staying in the project.
// An absolute path names any file on the controller machine.
func controllerPath(base, field, name string) (string, error) {
	path := filepath.Join(base, name)
	if filepath.IsAbs(name) {
		path = filepath.Clean(name)
	}
	if filepath.IsAbs(name) {
		return path, nil
	}
	projectRoot := base
	for directory := base; ; directory = filepath.Dir(directory) {
		if _, err := os.Stat(filepath.Join(directory, "mmx_project_settings.yml")); err == nil {
			projectRoot = directory
			break
		}
		if filepath.Dir(directory) == directory {
			break
		}
	}
	if relative, err := filepath.Rel(projectRoot, path); err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", invalid(field, fmt.Sprintf("%s %q leaves %s and reaches into %s; a relative path stays inside the project", field, name, projectRoot, filepath.Dir(path)))
	}
	return path, nil
}

// localSource is a regular file named by a path without references, resolved
// as controllerPath resolves it: a source, template, or signing_key_source.
func localSource(base, field, name string) (string, error) {
	path, err := controllerPath(base, field, name)
	if err != nil {
		return "", err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return "", &FieldError{field, err}
	}
	if !info.Mode().IsRegular() {
		return "", invalid(field, "file source is not a regular file: "+name)
	}
	return path, nil
}

func readTemplate(path string) ([]byte, error) {
	file, err := assets.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return io.ReadAll(file)
}

// Exported for the configuration loader, which reads the same files for a
// source field that differs between nodes.
var (
	ControllerPath      = controllerPath
	LocalSource         = localSource
	ReadTemplate        = readTemplate
	ReadKeys            = readKeys
	ReadDirectorySource = readDirectorySource
)

// SourceKind is what a controller-machine file field names.
type SourceKind int

const (
	SourceFile SourceKind = iota
	SourceTemplate
	SourceDirectory
	SourceDownload
	// SourceKeys is all of an authorized_keys task's key_files_on_controller_machine and
	// key_directories_on_controller_machine together: one variant holds the keys they give.
	SourceKeys
)

// ControllerFile describes one field of a task that names a file on the controller
// machine or a download, and where the chosen file goes in the task.
type ControllerFile struct {
	Field  string // as in a task's Sources, such as file.template
	Kind   SourceKind
	Path   *string // the local path; nil for a download
	URL    *string
	SHA256 *string
	// decompress is a download's compression as published.
	Decompress string
	Base       string
	Variants   *map[string]*SourceVariant
	Asset      **assets.Ref
	Template   *string
	Local      *string
	Entries    *[]DirectoryEntry
	Keys       *AuthorizedKeys
	// ReadTree reads a directory source tree that a variable source field
	// names, for one resolved name, and checks it.
	ReadTree func(path, name string) ([]DirectoryEntry, error)
	// CheckTree checks a frozen directory source tree before MMX saves its
	// files.
	CheckTree func() error
}

func (s ControllerFile) Dynamic() bool {
	switch s.Kind {
	case SourceDownload:
		return Dynamic(*s.URL) || Dynamic(*s.SHA256)
	case SourceKeys:
		return keyPathsDynamic(*s.Keys)
	default:
		// File, template, and directory sources use their path in the fallback below.
	}
	return Dynamic(*s.Path)
}
