package modules

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"mmx/internal/assets"
	"mmx/internal/differences"
	"mmx/internal/protocol"
	"mmx/internal/value"
)

// fileInspection compares one regular file with its desired state. The caller
// owns the open content and the pinned parent; inspection changes neither file.
type fileInspection struct {
	metadata                                  fileMetadata
	content                                   *FileContents
	changed, contentChanged, keep, hasContent bool
	backup                                    string
	change                                    *protocol.FileChange
	contentsSHA256                            string
}

// makeFileCandidate writes a private, complete replacement beside its
// destination. Both ordinary files and files with an owed restore use it.
// The optional modification time is used only when restoring a previous file.
func makeFileCandidate(ctx context.Context, parent *os.Root, path string, content io.Reader, metadata fileMetadata, modificationTime *time.Time) (candidate string, remove func() error, err error) {
	return writeFileCandidate(ctx, parent, path, content, metadata, modificationTime, ".mmx-candidate-"+rand.Text())
}

func writeFileCandidate(ctx context.Context, parent *os.Root, path string, content io.Reader, metadata fileMetadata, modificationTime *time.Time, directory string) (candidate string, remove func() error, err error) {
	if err := parent.Mkdir(directory, 0700); err != nil {
		return "", nil, err
	}
	cleanup := func() error { return parent.RemoveAll(directory) }
	remove = cleanup
	defer func() {
		if err != nil {
			err = errors.Join(err, cleanup())
		}
	}()
	candidate = filepath.Join(directory, filepath.Base(path))
	var file *os.File
	err = createLabeled(ctx, path, selinuxFile, func() (err error) {
		file, err = parent.OpenFile(candidate, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		return err
	})
	if err != nil {
		return "", nil, err
	}
	_, err = assets.Copy(ctx, file, content)
	if err == nil {
		_, err = metadata.apply(file)
	}
	if err == nil && modificationTime != nil {
		// Access time is not saved. Use the saved modification time for both.
		err = parent.Chtimes(candidate, *modificationTime, *modificationTime)
	}
	if err == nil {
		err = file.Sync()
	}
	err = errors.Join(err, file.Close())
	return candidate, remove, err
}

func (run Runtime) inspectFileAt(ctx context.Context, f File, s value.Scope, parent *os.Root, info os.FileInfo, defaultMode os.FileMode) (inspection *fileInspection, result protocol.Result) {
	path, name := f.Path, filepath.Base(f.Path)
	var change *protocol.FileChange
	defer func() { result.FileChange = change }()
	metadata, err := metadataFor(info, defaultMode, f.Mode, f.Owner, f.Group)
	if err != nil {
		return nil, run.accountFailure(err)
	}
	// An existing file whose task keeps its content gets only its metadata.
	keep := info != nil && f.KeepsExistingContent()
	hasContent := f.HasContents() && !keep
	if !hasContent && info == nil {
		// An earlier task, such as a package, may create the file.
		return nil, run.notYet(fmt.Errorf("cannot change metadata of missing file %s", path))
	}
	// A kept file still declares secret content, even though only its
	// metadata changes. Check its read access before applying that metadata.
	if keep && f.checkSecretReadPermissions {
		if s == nil {
			s = value.Scope{}
		}
		contentScope, trace := s.TraceReads() // Only this file's content decides its read permission.
		if f.Content != nil {
			_, err = contentScope.Resolve(*f.Content)
		} else if f.Template != "" {
			_, err = contentScope.Template(f.TemplateText)
		}
		if err != nil {
			return nil, protocol.Failed(err)
		}
		if trace.Used {
			if err := ValidateSecretFilePermissions(f.Path, metadata.mode, f.Group, "node", f.SecretReadableByGroup, f.DangerouslyLetEveryAccountReadSecret); err != nil {
				return nil, protocol.ConfigurationError(err)
			}
		}
	}
	// Content from node paths may be a private key: results show its SHA-256
	// values and never its text.
	fromNode := len(f.ContentFromNodePaths) > 0
	var content *FileContents
	contentChanged := false
	var size int64
	var desired, actual string
	if hasContent {
		if s == nil {
			s = value.Scope{}
		}
		contentScope, trace := s.TraceReads() // Only this file's content decides its read permission.
		content, err = OpenFileContents(f, contentScope)
		if fromNode && errors.Is(err, os.ErrNotExist) {
			// An earlier task, such as a certificate request, may create it.
			return nil, run.notYet(err)
		}
		if err != nil {
			return nil, protocol.Failed(err)
		}
		defer func() {
			if inspection == nil {
				content.Close()
			}
		}()
		if f.checkSecretReadPermissions && trace.Used && f.Literal == nil {
			if err := ValidateSecretFilePermissions(f.Path, metadata.mode, f.Group, "node", f.SecretReadableByGroup, f.DangerouslyLetEveryAccountReadSecret); err != nil {
				return nil, protocol.ConfigurationError(err)
			}
		}
		size = content.Size
		desired, err = content.Digest(ctx)
		if err != nil {
			return nil, protocol.Failed(err)
		}
		contentChanged = info == nil || info.Size() != size
		if info != nil && (!contentChanged || fromNode) {
			input, err := openRegularAt(parent, name)
			if err != nil {
				return nil, protocol.Failed(err)
			}
			hash, err := assets.Hash(ctx, input)
			input.Close()
			if err != nil {
				return nil, protocol.Failed(err)
			}
			actual = hash.SHA256
			contentChanged = contentChanged || hash.SHA256 != desired || hash.Size != size
		}
	}
	changed := contentChanged || !metadata.matches(info)
	// The original is copied once, before MMX first changes the file.
	backup := ""
	if changed && info != nil && f.BackupOriginalFileTo != "" {
		_, err := os.Lstat(f.BackupOriginalFileTo)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, protocol.Failed(fmt.Errorf("backup_original_file_to: %w", err))
		}
		if err != nil {
			backup = f.BackupOriginalFileTo
		}
	}
	if changed {
		change = fileChange(path, info, metadata, size, hasContent)
		if run.Dryrun {
			change.OriginalFileCopiedTo = backup
		}
		if fromNode && hasContent {
			if change.Before != nil {
				change.Before.SHA256 = actual
			}
			change.After.SHA256 = desired
			change.DiffOmitted = "contents read through content_from_node_paths are not shown; compare the sha256 values"
			change.FullDiffOmitted = change.DiffOmitted
		} else if contentChanged {
			var current []byte
			if info != nil {
				input, err := openRegularAt(parent, name)
				if err != nil {
					return nil, protocol.Failed(err)
				}
				current, err = io.ReadAll(io.LimitReader(input, differences.InputLimit+1))
				input.Close()
				if err != nil {
					return nil, protocol.Failed(err)
				}
			}
			next, err := io.ReadAll(io.LimitReader(content, differences.InputLimit+1))
			if err != nil {
				return nil, protocol.Failed(err)
			}
			//nolint:nilaway // contentChanged becomes true only after OpenFileContents successfully supplies content; failed opens return immediately.
			if _, err := content.Seek(0, io.SeekStart); err != nil {
				return nil, protocol.Failed(err)
			}
			run.textDiff(change, current, next)
		}
	}

	return &fileInspection{metadata, content, changed, contentChanged, keep, hasContent, backup, change, desired}, protocol.Success("unchanged")
}
