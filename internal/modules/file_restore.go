package modules

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"mmx/internal/assets"
	"mmx/internal/protocol"
	"mmx/internal/value"
)

// PreviousFileMetadata describes the immediately preceding regular file, or
// its absence. It saves no access time or extended attributes. Restored files
// receive the SELinux label of their destination, like ordinary file writes.
type PreviousFileMetadata struct {
	Existed           bool      `json:"existed"`
	ContentsSHA256    string    `json:"contents_sha256"`
	ContentsSizeBytes int64     `json:"contents_size_bytes"`
	PermissionsMode   string    `json:"permissions_mode"`
	OwnerUserID       uint32    `json:"owner_user_id"`
	OwnerGroupID      uint32    `json:"owner_group_id"`
	ModificationTime  time.Time `json:"modification_time"`
}

func (m PreviousFileMetadata) MarshalJSON() ([]byte, error) {
	if err := m.Validate(); err != nil {
		return nil, err
	}
	if !m.Existed {
		return []byte(`{"existed":false}`), nil
	}
	type plain PreviousFileMetadata
	return json.Marshal(plain(m))
}

func (m *PreviousFileMetadata) UnmarshalJSON(b []byte) error {
	type plain PreviousFileMetadata
	var decoded plain
	if err := protocol.Decode(b, &decoded); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(b, &fields); err != nil {
		return err
	}
	required := []string{"existed"}
	if decoded.Existed {
		required = append(required, "contents_sha256", "contents_size_bytes", "permissions_mode", "owner_user_id", "owner_group_id", "modification_time")
	}
	for _, field := range required {
		if len(fields[field]) == 0 || string(fields[field]) == "null" {
			return fmt.Errorf("previous file metadata requires %s", field)
		}
	}
	if !decoded.Existed && len(fields) != 1 {
		return fmt.Errorf("previous file metadata for an absent file must contain only existed: false")
	}
	*m = PreviousFileMetadata(decoded)
	if err := m.Validate(); err != nil {
		return err
	}
	m.ModificationTime = m.ModificationTime.UTC()
	return nil
}

func (m PreviousFileMetadata) Validate() error {
	if !m.Existed {
		if m != (PreviousFileMetadata{}) {
			return fmt.Errorf("previous file metadata for an absent file must contain only existed: false")
		}
		return nil
	}
	if !assets.ValidDigest(m.ContentsSHA256) || m.ContentsSizeBytes < 0 {
		return fmt.Errorf("previous file metadata requires a valid contents_sha256 and nonnegative contents_size_bytes")
	}
	mode, err := strconv.ParseUint(m.PermissionsMode, 8, 12)
	if err != nil || fmt.Sprintf("%04o", mode) != m.PermissionsMode {
		return fmt.Errorf("previous file permissions_mode must contain four octal digits")
	}
	_, offset := m.ModificationTime.Zone()
	if m.ModificationTime.IsZero() || offset != 0 {
		return fmt.Errorf("previous file modification_time must be a nonzero UTC timestamp")
	}
	return nil
}

func (m PreviousFileMetadata) fileMetadata() fileMetadata {
	mode, _ := strconv.ParseUint(m.PermissionsMode, 8, 12) // Validate has checked it.
	return fileMetadata{permissionsFromNative(uint32(mode)), int(m.OwnerUserID), int(m.OwnerGroupID)}
}

// PreparedFileChange pins the destination parent and retains a checked private
// candidate. Close removes only that candidate, never saved previous contents.
// The caller must save and arm the owed restore before calling Install.
type PreparedFileChange struct {
	runtime         Runtime
	parent          *os.Root
	destination     string
	before          os.FileInfo
	previous        PreviousFileMetadata
	candidate       string
	candidateInfo   os.FileInfo
	candidateState  PreviousFileMetadata
	removeCandidate func() error
	change          *protocol.FileChange
	previousSaved   bool
	installed       bool
}

func (f *PreparedFileChange) DestinationFile() string { return f.destination }

// InstallsAFile says whether the change installs a file, rather than
// removing one.
func (f *PreparedFileChange) InstallsAFile() bool { return f.candidate != "" }

// PrepareFileChange checks a candidate without changing the destination.
// A nil prepared change means a failure, an unchanged file, or a dryrun; result
// distinguishes them. A nonnil change returns would_change until Install.
// checkCandidate receives an empty path for state: absent. It must only read.
func (run Runtime) PrepareFileChange(ctx context.Context, file File, scope value.Scope, checkCandidate func(context.Context, string) error) (prepared *PreparedFileChange, result protocol.Result) {
	if err := run.checkFileDestination(ctx, file.Path); err != nil {
		return nil, protocol.Failed(err)
	}
	if err := ValidateFile(file); err != nil {
		return nil, protocol.ConfigurationError(err)
	}
	if err := validateFileChangeDestination(file.Path); err != nil {
		return nil, protocol.ConfigurationError(err)
	}
	if file.CheckNewFile != nil || file.BackupOriginalFileTo != "" {
		return nil, protocol.ConfigurationError(fmt.Errorf("a file change with an owed restore checks the proposed file and saves the previous file in steps of its own, so check_new_file and backup_original_file_to are not supported"))
	}
	parent, before, err := openParent(file.Path, run.Dryrun || file.State == "absent")
	if err != nil {
		return nil, protocol.Failed(err)
	}
	change := &PreparedFileChange{runtime: run, parent: parent, destination: file.Path, before: before}
	defer func() {
		if prepared == nil {
			// Closing the prepared change only cleans temporary candidates and directory handles.
			_ = change.Close()
		}
	}()
	change.previous, err = previousFileState(ctx, parent, filepath.Base(file.Path), before)
	if err != nil {
		return nil, protocol.Failed(err)
	}
	result = protocol.Success("would_change")
	if file.State == "absent" {
		if before == nil {
			return nil, protocol.Success("unchanged")
		}
		change.change = &protocol.FileChange{Path: file.Path, Action: "remove", Before: fileState(before)}
	} else {
		inspection, failure := run.inspectFileAt(ctx, file, scope, parent, before, 0644)
		if inspection == nil {
			return nil, failure
		}
		if inspection.content != nil {
			defer inspection.content.Close()
		}
		if !inspection.changed {
			return nil, protocol.Success("unchanged")
		}
		change.change = inspection.change
		if !run.Dryrun {
			var contents io.Reader = inspection.content
			if inspection.content == nil {
				input, err := openRegularAt(parent, filepath.Base(file.Path))
				if err != nil {
					return nil, protocol.Failed(err)
				}
				defer input.Close()
				contents = input
			}
			change.candidate, change.removeCandidate, err = makeFileChangeCandidate(ctx, parent, file.Path, contents, inspection.metadata, nil, false)
			if err != nil {
				return nil, protocol.Failed(err)
			}
			change.candidateInfo, err = parent.Lstat(change.candidate)
			if err == nil {
				change.candidateState, err = previousFileState(ctx, parent, change.candidate, change.candidateInfo)
			}
			if err != nil {
				return nil, protocol.Failed(err)
			}
			if !inspection.metadata.matches(change.candidateInfo) {
				return nil, protocol.Failed(fmt.Errorf("file change candidate metadata does not match the requested owner, group, and permissions: %s", file.Path))
			}
			// Metadata-only replacements must carry exactly the previous bytes.
			desired := change.previous.ContentsSHA256
			if inspection.content != nil {
				desired = inspection.contentsSHA256
			}
			if change.candidateState.ContentsSHA256 != desired {
				return nil, protocol.Failed(fmt.Errorf("file change candidate contents changed during preparation: %s", file.Path))
			}
		}
	}
	result.FileChange = change.change
	if run.Dryrun {
		return nil, result
	}
	if checkCandidate != nil {
		candidatePath := ""
		if change.candidate != "" {
			candidatePath = filepath.Join(filepath.Dir(file.Path), change.candidate)
		}
		if err := checkCandidate(ctx, candidatePath); err != nil {
			result.Status, result.Change, result.FailureKind, result.Reason = "failed", "unchanged", "validation", err.Error()
			return nil, result
		}
	}
	if err := change.checkUnchanged(ctx); err != nil {
		return nil, protocol.Failed(err)
	}
	return change, result
}

// SavePreviousFile creates the saved contents exclusively in the private
// persistent directory supplied by its caller. Bytes and their entry are synced
// before metadata is returned. Absence creates no contents file. The caller
// persists this metadata with its owed restore record before installation.
func (f *PreparedFileChange) SavePreviousFile(ctx context.Context, previousContentsPath string) (metadata PreviousFileMetadata, err error) {
	if f.parent == nil || f.installed || f.previousSaved {
		return metadata, fmt.Errorf("previous file save requires an open, unsaved file change")
	}
	if err := f.checkUnchanged(ctx); err != nil {
		return metadata, err
	}
	if f.previous.Existed {
		if err := f.saveCopy(ctx, filepath.Base(f.destination), f.previous, previousContentsPath, "previous file"); err != nil {
			return metadata, err
		}
	}
	f.previousSaved = true
	return f.previous, nil
}

// SaveProposedFile saves the candidate's bytes as SavePreviousFile saves the
// previous file's, for an activation that hands both files to its tool
// (ChangeActivationWithSavedFiles). Only a change that installs a file has
// a proposed file.
func (f *PreparedFileChange) SaveProposedFile(ctx context.Context, proposedContentsPath string) error {
	if f.parent == nil || f.installed || f.candidate == "" {
		return fmt.Errorf("proposed file save requires an open, uninstalled change that installs a file")
	}
	return f.saveCopy(ctx, f.candidate, f.candidateState, proposedContentsPath, "proposed file")
}

// saveCopy copies the entry name of the pinned parent exclusively to the
// root-private file path, checks its bytes against expected, and syncs the
// copy and its entry.
func (f *PreparedFileChange) saveCopy(ctx context.Context, name string, expected PreviousFileMetadata, path, kind string) (err error) {
	parent, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer parent.Close()
	savedName := filepath.Base(path)
	saved, err := parent.OpenFile(savedName, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, parent.Remove(savedName), syncFileParent(parent))
		}
	}()
	if err := saved.Chmod(0600); err != nil {
		return errors.Join(err, saved.Close())
	}
	input, err := openRegularAt(f.parent, name)
	if err != nil {
		return errors.Join(err, saved.Close())
	}
	hash := sha256.New()
	count, err := assets.Copy(ctx, io.MultiWriter(saved, hash), input)
	err = errors.Join(err, input.Close())
	if err == nil && (count != expected.ContentsSizeBytes || hex.EncodeToString(hash.Sum(nil)) != expected.ContentsSHA256) {
		err = fmt.Errorf("%s contents changed while saving: %s", kind, f.destination)
	}
	if err == nil {
		err = f.checkUnchanged(ctx)
	}
	if err == nil {
		err = saved.Sync()
	}
	err = errors.Join(err, saved.Close())
	if err == nil {
		err = syncFileParent(parent)
	}
	return err
}

// Install does not arm a timer or persist a record. The owed restore service
// owns that ordering. Even metadata-only changes replace the complete file.
func (f *PreparedFileChange) Install(ctx context.Context) protocol.Result {
	if f.parent == nil || f.installed || !f.previousSaved {
		return protocol.Failed(fmt.Errorf("file change installation requires an open file change with its previous file saved"))
	}
	run := f.runtime
	// The node holds the record lock and has checked this change itself.
	run.CheckFileDestination = nil
	run.BeforeMutation = func(path string) error {
		if f.runtime.BeforeMutation != nil {
			if err := f.runtime.BeforeMutation(path); err != nil {
				return err
			}
		}
		return f.checkUnchanged(ctx)
	}
	var result protocol.Result
	if f.candidate == "" {
		result = run.removeAt(ctx, f.parent, f.destination, f.before, fileState(f.before))
	} else {
		result = run.replaceAt(ctx, f.parent, f.destination, f.candidate, f.before)
	}
	result.FileChange = f.change
	f.installed = result.Change == "changed"
	return result
}

func (f *PreparedFileChange) checkUnchanged(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	actual, err := previousFileState(ctx, f.parent, filepath.Base(f.destination), f.before)
	if err != nil {
		return err
	}
	if actual != f.previous {
		return fmt.Errorf("file changed after preparation: %s", f.destination)
	}
	if f.candidate != "" {
		actual, err = previousFileState(ctx, f.parent, f.candidate, f.candidateInfo)
		if err != nil {
			return err
		}
		if actual != f.candidateState {
			return fmt.Errorf("file change candidate changed after validation: %s", f.destination)
		}
	}
	return nil
}

func (f *PreparedFileChange) Close() error {
	if f.parent == nil {
		return nil
	}
	var err error
	if f.removeCandidate != nil {
		err = f.removeCandidate()
	}
	err = errors.Join(err, f.parent.Close())
	f.parent = nil
	return err
}

// RestorePreviousFile is the byte/metadata operation only. The caller must stop
// activation and hold the owed restore's locks before calling it, then activate
// and check the previous configuration. Repeating the operation is safe.
func (run Runtime) RestorePreviousFile(ctx context.Context, destinationFile, previousContentsPath string, metadata PreviousFileMetadata) protocol.Result {
	if err := validateFileChangeDestination(destinationFile); err != nil {
		return protocol.Failed(err)
	}
	if err := metadata.Validate(); err != nil {
		return protocol.Failed(err)
	}
	metadata.ModificationTime = metadata.ModificationTime.UTC()
	parent, before, err := openParent(destinationFile, !metadata.Existed)
	if err != nil {
		return protocol.Failed(err)
	}
	if parent != nil {
		defer parent.Close()
	}
	if before != nil && !before.Mode().IsRegular() {
		return protocol.Failed(fmt.Errorf("refusing to restore over non-regular file %s", destinationFile))
	}
	if !run.Dryrun && parent != nil {
		if err := removeFileChangeCandidate(parent, destinationFile, true); err != nil {
			return protocol.Failed(err)
		}
	}
	if !metadata.Existed {
		result := run.removeAt(ctx, parent, destinationFile, before, fileState(before))
		// An earlier attempt may have unlinked the file but failed its sync.
		if !run.Dryrun && before == nil && parent != nil {
			if err := syncFileParent(parent); err != nil {
				return protocol.Failed(err)
			}
		}
		return result
	}
	savedParent, err := os.OpenRoot(filepath.Dir(previousContentsPath))
	if err != nil {
		return protocol.Failed(err)
	}
	defer savedParent.Close()
	input, err := openRegularAt(savedParent, filepath.Base(previousContentsPath))
	if err != nil {
		return protocol.Failed(err)
	}
	defer input.Close()
	if run.Dryrun {
		return protocol.Success("would_change")
	}
	candidate, remove, err := makeFileChangeCandidate(ctx, parent, destinationFile, input, metadata.fileMetadata(), &metadata.ModificationTime, true)
	if err != nil {
		return protocol.Failed(err)
	}
	// Failure to remove the private restore candidate only leaves an unused temporary file.
	defer func() { _ = remove() }()
	info, err := parent.Lstat(candidate)
	if err != nil {
		return protocol.Failed(err)
	}
	actual, err := previousFileState(ctx, parent, candidate, info)
	if err != nil {
		return protocol.Failed(err)
	}
	if actual != metadata {
		return protocol.Failed(fmt.Errorf("saved previous file contents or restored metadata do not match the owed restore: %s", destinationFile))
	}
	result := run.replaceAt(ctx, parent, destinationFile, candidate, before)
	result.FileChange = fileChange(destinationFile, before, metadata.fileMetadata(), metadata.ContentsSizeBytes, true)
	return result
}

func validateFileChangeDestination(path string) error {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return fmt.Errorf("file change destination must be an absolute path with no redundant path components")
	}
	return nil
}

// previousFileState checks the inspected entry before and after reading it.
// Hashes also detect in-place edits that leave the inode unchanged.
func previousFileState(ctx context.Context, parent *os.Root, name string, inspected os.FileInfo) (PreviousFileMetadata, error) {
	var result PreviousFileMetadata
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if parent == nil {
		return result, nil
	}
	if err := sameFileAt(parent, name, inspected); err != nil {
		return result, err
	}
	if inspected == nil {
		return result, nil
	}
	input, err := openRegularAt(parent, name)
	if err != nil {
		return result, err
	}
	defer input.Close()
	before, err := input.Stat()
	if err != nil {
		return result, err
	}
	if !sameFileMetadata(inspected, before) {
		return result, fmt.Errorf("file changed during inspection: %s", name)
	}
	hash, err := assets.Hash(ctx, input)
	if err != nil {
		return result, err
	}
	after, err := input.Stat()
	if err != nil {
		return result, err
	}
	if !sameFileMetadata(before, after) || hash.Size != before.Size() {
		return result, fmt.Errorf("file changed during inspection: %s", name)
	}
	if err := sameFileAt(parent, name, inspected); err != nil {
		return result, err
	}
	owner := before.Sys().(*syscall.Stat_t)
	return PreviousFileMetadata{true, hash.SHA256, hash.Size, fmt.Sprintf("%04o", nativePermissions(before.Mode())), owner.Uid, owner.Gid, before.ModTime().UTC()}, nil
}

func sameFileMetadata(a, b os.FileInfo) bool {
	owner := a.Sys().(*syscall.Stat_t)
	return os.SameFile(a, b) && a.Size() == b.Size() && a.ModTime().Equal(b.ModTime()) &&
		(fileMetadata{permissionMode(a.Mode()), int(owner.Uid), int(owner.Gid)}).matches(b)
}

// Protected candidates have two derivable names: a worker's Close must not
// remove a restore process's candidate. Hashing the basename also handles
// destination names near the filesystem's length limit.
func fileChangeCandidateDirectory(destination string, restoring bool) string {
	digest := sha256.Sum256([]byte(filepath.Base(destination)))
	purpose := "proposed-file"
	if restoring {
		purpose = "previous-file"
	}
	return ".mmx-file-change-" + hex.EncodeToString(digest[:]) + "-" + purpose
}

func removeFileChangeCandidate(parent *os.Root, destination string, restoring bool) error {
	name := fileChangeCandidateDirectory(destination, restoring)
	info, err := parent.Lstat(name)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode().Perm() != 0700 || info.Sys().(*syscall.Stat_t).Uid != uint32(os.Geteuid()) {
		return fmt.Errorf("file change candidate directory has an unexpected type, owner, or permissions: %s", filepath.Join(filepath.Dir(destination), name))
	}
	if err := parent.RemoveAll(name); err != nil {
		return err
	}
	return syncFileParent(parent)
}

func makeFileChangeCandidate(ctx context.Context, parent *os.Root, destination string, content io.Reader, metadata fileMetadata, modified *time.Time, restoring bool) (string, func() error, error) {
	if err := removeFileChangeCandidate(parent, destination, restoring); err != nil {
		return "", nil, err
	}
	return writeFileCandidate(ctx, parent, destination, content, metadata, modified, fileChangeCandidateDirectory(destination, restoring))
}

// RemoveFileChangeCandidates is called after installation and activation can
// no longer run for this change. It works without the original prepared object.
func RemoveFileChangeCandidates(destination string) error {
	if err := validateFileChangeDestination(destination); err != nil {
		return err
	}
	parent, err := os.OpenRoot(filepath.Dir(destination))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer parent.Close()
	return errors.Join(removeFileChangeCandidate(parent, destination, false), removeFileChangeCandidate(parent, destination, true))
}

// checkFileDestination runs even for unchanged and dryrun files. The node
// supplies the refusal while an earlier restore remains owed for this path.
func (run Runtime) checkFileDestination(ctx context.Context, path string) error {
	if run.CheckFileDestination != nil {
		return run.CheckFileDestination(ctx, filepath.Clean(path))
	}
	return nil
}
