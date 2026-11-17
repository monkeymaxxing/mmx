package modules

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"syscall"

	"mmx/internal/assets"
	"mmx/internal/protocol"
	"mmx/internal/value"
)

func (run Runtime) File(ctx context.Context, f File, s value.Scope) (result protocol.Result) {
	f.checkSecretReadPermissions = true
	if err := ValidateFile(f); err != nil {
		return protocol.ConfigurationError(err)
	}
	parent, err := os.OpenRoot(filepath.Dir(f.Path))
	if os.IsNotExist(err) && f.State != "absent" {
		if r := run.missingParent(f.Path); r != nil {
			return *r
		}
	} else if err != nil && !os.IsNotExist(err) {
		return protocol.Failed(err)
	}
	if parent != nil {
		defer parent.Close()
	}
	return run.fileAt(ctx, f, s, parent, 0644, nil)
}

// The destination parent is pinned for the whole comparison and installation.
// A nil parent represents a missing directory during preview or absence checks.
func (run Runtime) fileAt(ctx context.Context, f File, s value.Scope, parent *os.Root, defaultMode os.FileMode, validate func(string) protocol.Result) (result protocol.Result) {
	if err := run.checkFileDestination(ctx, f.Path); err != nil {
		return protocol.Failed(err)
	}
	path, name := f.Path, filepath.Base(f.Path)
	var info os.FileInfo
	var err error
	if parent != nil {
		info, err = parent.Lstat(name)
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return protocol.Failed(err)
	}
	if info != nil && !info.Mode().IsRegular() {
		return protocol.Failed(notARegularFile(path, info))
	}
	if f.State == "absent" {
		return run.removeAt(ctx, parent, path, info, fileState(info))
	}
	inspection, result := run.inspectFileAt(ctx, f, s, parent, info, defaultMode)
	if inspection == nil {
		return result
	}
	metadata, content := inspection.metadata, inspection.content
	changed, contentChanged := inspection.changed, inspection.contentChanged
	keep, hasContent := inspection.keep, inspection.hasContent
	backup, change := inspection.backup, inspection.change
	if content != nil {
		defer content.Close()
	}
	defer func() { result.FileChange = change }()
	hasValidator := (f.CheckNewFile != nil || validate != nil) && !keep
	preview := protocol.Success("unchanged")
	if changed {
		preview.Change = "would_change"
	}
	if run.Dryrun && !hasValidator {
		return preview
	}
	if run.Dryrun && f.CheckNewFile != nil && f.CheckNewFile.Shell.Dryrun == "" && !f.CheckNewFile.Shell.RunScriptInDryrun {
		preview.CheckNewFile = &protocol.Result{Status: "skipped", Change: "unchanged", Reason: "not checked: check_new_file has no inspection command for a dryrun"}
		return preview
	}
	if !changed && !hasValidator {
		return protocol.Success("unchanged")
	}
	if !hasContent || (!contentChanged && !hasValidator) {
		if r := run.copyOriginal(ctx, parent, info, backup, change); r != nil {
			return *r
		}
		return run.changeMetadataAt(ctx, parent, path, info, metadata)
	}
	// Candidate files are private until validation and the atomic replacement.
	if parent == nil {
		preview.CheckNewFile = &protocol.Result{Status: "skipped", Change: "unchanged", Reason: "not checked: destination directory does not exist yet"}
		return preview
	}
	candidate, removeCandidate, err := makeFileCandidate(ctx, parent, path, content, metadata, nil)
	if err != nil {
		return protocol.Failed(err)
	}
	// Failure to remove the private candidate only leaves an unused temporary file.
	defer func() { _ = removeCandidate() }()
	if hasValidator {
		candidatePath := filepath.Join(filepath.Dir(path), candidate)
		var r protocol.Result
		// A failure of the task's own check is named for its field. A
		// module's check names itself in its reason.
		checkFailure := "check_new_file failed: "
		if validate != nil {
			checkFailure = ""
		}
		if validate != nil {
			r = validate(candidatePath)
		} else {
			local := s.Local()
			local["new_file"] = map[string]any{"path": candidatePath}
			resolve := run.ResolveShell
			if resolve == nil { // a test that runs the module alone, with literal inputs
				resolve = func(sh Shell, _ value.Scope) (Shell, error) { return sh, nil }
			}
			if check, err := resolve(*f.CheckNewFile.Shell, local); err != nil {
				r = protocol.Failed(err)
			} else {
				r = run.Shell(ctx, check, local, true)
			}
		}
		if r.Status == "failed" && r.OutputHoldsNoSecret {
			r.Reason = failureTextFromStandardError(r)
		}
		if run.Dryrun {
			preview.CheckNewFile = &r
			if r.Status == "skipped" {
				r.Reason = "not checked: " + r.Reason
				return preview
			}
			if r.Status == "failed" && r.FailureKind == "" && ctx.Err() == nil && (r.ExitCode == 126 || r.ExitCode == 127) {
				r.Status = "skipped"
				r.Reason = "not checked: check_new_file command is unavailable"
				return preview
			}
			if r.Status != "succeeded" {
				preview.Status, preview.FailureKind = "failed", "validation"
				preview.Reason = checkFailure + r.Reason
				preview.ReasonReadsNoSecret = r.OutputHoldsNoSecret
			}
			return preview
		}
		if r.OutputHoldsNoSecret && r.Status == "succeeded" {
			defer func() { result.CheckNewFile = &r }()
		}
		if r.Status != "succeeded" {
			r.Status = "failed"
			r.Change = "unchanged"
			r.FailureKind = "validation"
			check := r
			r.CheckNewFile = &check
			r.OutputHoldsNoSecret = false
			r.ReasonReadsNoSecret = check.OutputHoldsNoSecret
			r.Reason = checkFailure + r.Reason
			return r
		}
	}
	if !changed {
		return protocol.Success("unchanged")
	}
	if r := run.copyOriginal(ctx, parent, info, backup, change); r != nil {
		return *r
	}
	if !contentChanged {
		return run.changeMetadataAt(ctx, parent, path, info, metadata)
	}
	return run.replaceAt(ctx, parent, path, candidate, info)
}

// copyOriginal copies the inspected file within parent, with its owner, group,
// and mode, to backup, which did not exist, and records the copy in change. An
// empty backup copies nothing. The copy is installed like a new file and never
// replaces anything, so it stays the original however often the file changes.
func (run Runtime) copyOriginal(ctx context.Context, parent *os.Root, info os.FileInfo, backup string, change *protocol.FileChange) *protocol.Result {
	if backup == "" {
		return nil
	}
	failed := func(err error) *protocol.Result {
		r := protocol.Failed(fmt.Errorf("backup_original_file_to: %w", err))
		return &r
	}
	backupParent, err := os.OpenRoot(filepath.Dir(backup))
	if err != nil {
		return failed(err)
	}
	defer backupParent.Close()
	input, err := openRegularAt(parent, info.Name()) //nolint:nilaway // A nonempty backup path is created only when info != nil; copyOriginal returns immediately for an empty backup.
	if err != nil {
		return failed(err)
	}
	defer input.Close()
	opened, err := input.Stat()
	if err != nil {
		return failed(err)
	}
	if !os.SameFile(info, opened) {
		return failed(fmt.Errorf("file changed during inspection: %s", info.Name())) //nolint:nilaway // Passing the empty-backup guard implies inspection supplied non-nil info.
	}
	dir := ".mmx-backup-" + rand.Text()
	if err := backupParent.Mkdir(dir, 0700); err != nil {
		return failed(err)
	}
	defer backupParent.RemoveAll(dir)
	candidate := filepath.Join(dir, filepath.Base(backup))
	var handle *os.File
	err = createLabeled(ctx, backup, selinuxFile, func() (err error) {
		handle, err = backupParent.OpenFile(candidate, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		return err
	})
	if err != nil {
		return failed(err)
	}
	owner := info.Sys().(*syscall.Stat_t) //nolint:nilaway // Passing the empty-backup guard implies inspection supplied non-nil info.
	_, err = assets.Copy(ctx, handle, input)
	if err == nil {
		_, err = fileMetadata{permissionMode(info.Mode()), int(owner.Uid), int(owner.Gid)}.apply(handle) //nolint:nilaway // Passing the empty-backup guard implies inspection supplied non-nil info.
	}
	if err == nil {
		err = handle.Sync()
	}
	if closeErr := handle.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return failed(err)
	}
	// With no inspected entry, the rename refuses a backup that appeared meanwhile.
	if r := run.replaceAt(ctx, backupParent, backup, candidate, nil); r.Status != "succeeded" {
		r.Reason = "backup_original_file_to: " + r.Reason
		return &r
	}
	change.OriginalFileCopiedTo = backup
	return nil
}

// replaceAt is the one atomic installation of a managed path: at the mutation
// boundary, it confirms the inspected entry (before, nil when absent) still
// owns the destination, renames the private candidate over it within the
// pinned parent, and syncs the directory.
func (run Runtime) replaceAt(ctx context.Context, parent *os.Root, path, candidate string, before os.FileInfo) protocol.Result {
	if err := run.checkFileDestination(ctx, path); err != nil {
		return protocol.Failed(err)
	}
	name := filepath.Base(path)
	if err := ctx.Err(); err != nil {
		return protocol.Failed(err)
	}
	if err := run.Mutation(path); err != nil {
		return protocol.Failed(err)
	}
	if err := sameFileAt(parent, name, before); err != nil {
		return protocol.Failed(err)
	}
	if err := parent.Rename(candidate, name); err != nil {
		return protocol.Failed(err)
	}
	err := syncFileParent(parent)
	if err != nil {
		r := protocol.Failed(err)
		r.Change = "changed"
		return r
	}
	return protocol.Success("changed")
}

// Metadata-only changes preserve the inode and contents. Open without following
// a link, and confirm the inspected file still owns the destination.
func (run Runtime) changeMetadataAt(ctx context.Context, parent *os.Root, path string, before os.FileInfo, metadata fileMetadata) protocol.Result {
	if err := ctx.Err(); err != nil {
		return protocol.Failed(err)
	}
	if err := run.Mutation(path); err != nil {
		return protocol.Failed(err)
	}
	file, err := openRegularAt(parent, filepath.Base(path))
	if err != nil {
		return protocol.Failed(err)
	}
	defer file.Close()
	current, err := file.Stat()
	if err != nil {
		return protocol.Failed(err)
	}
	if !current.Mode().IsRegular() || !os.SameFile(before, current) {
		return protocol.Failed(fmt.Errorf("file changed during metadata inspection: %s", path))
	}
	changed, err := metadata.apply(file)
	if err == nil {
		err = file.Sync()
	}
	result := protocol.Success("unchanged")
	if err != nil {
		result = protocol.Failed(err)
	}
	if changed {
		result.Change = "changed"
	}
	return result
}

func openRegularAt(parent *os.Root, name string) (*os.File, error) {
	file, err := parent.OpenFile(name, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		file.Close()
		return nil, fmt.Errorf("expected regular file: %s", name)
	}
	return file, nil
}

// editLimit bounds the files built-in modules edit in memory (authorized_keys,
// line).
const editLimit = 1 << 20

// readTextAt reads a regular file within its pinned parent, up to editLimit.
func readTextAt(parent *os.Root, name string) (string, error) {
	file, err := openRegularAt(parent, name)
	if err != nil {
		return "", err
	}
	defer file.Close()
	b, err := io.ReadAll(io.LimitReader(file, editLimit+1))
	if err == nil && len(b) > editLimit {
		err = fmt.Errorf("%s is larger than 1 MiB", name)
	}
	return string(b), err
}

func sameFileAt(parent *os.Root, name string, before os.FileInfo) error {
	now, err := parent.Lstat(name)
	if os.IsNotExist(err) && before == nil {
		return nil
	}
	if err != nil {
		return err
	}
	if before == nil || !os.SameFile(before, now) || before.Mode().Type() != now.Mode().Type() {
		return fmt.Errorf("path changed during inspection: %s", name)
	}
	return nil
}

// removeAt unlinks the inspected entry (a file or link, never its target)
// from its pinned parent; old describes it in the result.
func (run Runtime) removeAt(ctx context.Context, parent *os.Root, path string, before os.FileInfo, old *protocol.FileState) (result protocol.Result) {
	if err := run.checkFileDestination(ctx, path); err != nil {
		return protocol.Failed(err)
	}
	if before == nil {
		return protocol.Success("unchanged")
	}
	defer func() {
		result.FileChange = &protocol.FileChange{Path: path, Action: "remove", Before: old}
	}()
	if run.Dryrun {
		return protocol.Success("would_change")
	}
	if err := ctx.Err(); err != nil {
		return protocol.Failed(err)
	}
	if err := run.Mutation(path); err != nil {
		return protocol.Failed(err)
	}
	name := filepath.Base(path)
	if err := sameFileAt(parent, name, before); err != nil {
		return protocol.Failed(err)
	}
	if err := parent.Remove(name); err != nil {
		return protocol.Failed(err)
	}
	if err := syncFileParent(parent); err != nil {
		result := protocol.Failed(err)
		result.Change = "changed"
		return result
	}
	return protocol.Success("changed")
}

// Module inputs belong to configuration so the controller can validate them
// without importing node execution code.
type File struct {
	// Only file and download apply their secret-read acknowledgements. Other
	// modules use the installer with their own access policy.
	checkSecretReadPermissions bool   `yaml:"-" json:"-"`
	Path                       string `yaml:"path" json:"path"`
	State                      string `yaml:"state,omitempty" json:"state,omitempty"`
	Source                     string `yaml:"source,omitempty" json:"source,omitempty"`
	URL                        string `yaml:"-" json:"url,omitempty"`
	SHA256                     string `yaml:"-" json:"sha256,omitempty"`
	// DangerouslySkipChecksum is download's own field, in place of SHA256.
	DangerouslySkipChecksum bool        `yaml:"-" json:"dangerously_skip_checksum,omitempty"`
	Decompress              string      `yaml:"-" json:"decompress,omitempty"` // download's own field; the asset holds the decompressed file
	Asset                   *assets.Ref `yaml:"-" json:"asset,omitempty"`
	LocalPath               string      `yaml:"-" json:"-"`
	// SourceBase and Variants serve a variable source field (variable_sources.go).
	SourceBase                           string                    `yaml:"-" json:"-"`
	Variants                             map[string]*SourceVariant `yaml:"-" json:"variants,omitempty"`
	Content                              *string                   `yaml:"content,omitempty" json:"content,omitempty"`
	Template                             string                    `yaml:"template,omitempty" json:"template,omitempty"`
	Owner                                Account                   `yaml:"owner,omitempty" json:"owner,omitempty"`
	Group                                Account                   `yaml:"group,omitempty" json:"group,omitempty"`
	Mode                                 string                    `yaml:"permissions_mode,omitempty" json:"permissions_mode,omitempty"`
	SecretReadableByGroup                bool                      `yaml:"secret_readable_by_group,omitempty" json:"secret_readable_by_group,omitempty"`
	DangerouslyLetEveryAccountReadSecret bool                      `yaml:"dangerously_let_every_account_read_secret,omitempty" json:"dangerously_let_every_account_read_secret,omitempty"`
	CheckNewFile                         *Check                    `yaml:"check_new_file,omitempty" json:"check_new_file,omitempty"`
	// ContentFromNodePaths are files already on the node whose bytes, in
	// order, are the new content.
	ContentFromNodePaths []string `yaml:"content_from_node_paths,omitempty" json:"content_from_node_paths,omitempty"`
	// IfFileExists is replace_existing_content (the default) or
	// keep_existing_content, which only creates a missing file.
	IfFileExists string `yaml:"if_file_exists,omitempty" json:"if_file_exists,omitempty"`
	// BackupOriginalFileTo is where MMX copies the file once, before it first
	// changes it.
	BackupOriginalFileTo string `yaml:"backup_original_file_to,omitempty" json:"backup_original_file_to,omitempty"`
	TemplateText         string `yaml:"-" json:"-"`
	// Literal is exact contents a built-in module composed on the node
	// (authorized_keys, apt_repository, line, edit_file); unlike content,
	// references in it are not resolved.
	Literal *string `yaml:"-" json:"-"`
}

type Check struct {
	Shell *Shell `yaml:"shell" json:"shell"`
}

var modePattern = regexp.MustCompile(`^0?[0-7]{3}$`)

// Allowed values of choice fields. Validation checks them and mmx describe-module
// lists them, so the two cannot disagree.
var (
	presenceStates = []string{"present", "absent"}
	serviceStates  = []string{"running", "stopped", "do_not_start_or_stop"}
	refreshActions = []string{"restart", "reload"}
	notListed      = []string{"remove", "keep"} // supplementary_groups_not_listed, keys_not_listed, rules_not_listed
	ufwActions     = []string{"allow", "deny", "reject", "limit"}
	ufwProtocols   = []string{"tcp", "udp", "tcp_and_udp"}
	ufwPolicies    = []string{"allow", "deny", "reject"}
	ufwLogging     = []string{"off", "low", "medium", "high", "full"} // ufw.logging_level
	ifFileExists   = []string{"replace_existing_content", "keep_existing_content"}
	decompressions = []string{"gzip", "bzip2", "xz", "zstd"} // download.decompress

	ifSourceDirectoryIsMissing = []string{"fail", "copy_nothing"} // directory.if_source_directory_is_missing

	applyPasswordHashChoices = []string{"only_when_account_has_no_password", "on_every_run"}

	firewalldActions     = []string{"allow", "reject", "drop"}
	firewalldProtocols   = []string{"tcp", "udp"}
	selinuxPortProtocols = []string{"tcp", "udp"}
	selinuxModes         = []string{"enforcing", "permissive"}

	sshKeyTypes = []string{"ed25519", "rsa"} // ssh_key_pair.key_type

	kernelModuleStates = []string{"loaded", "unloaded"} // kernel_module.state
)

func (f File) HasContents() bool {
	return f.Content != nil || f.Literal != nil || f.Template != "" || f.Source != "" || f.URL != "" || len(f.ContentFromNodePaths) > 0
}

// KeepsExistingContent reports if_file_exists: keep_existing_content: the
// task creates a missing file and leaves an existing file's bytes alone.
func (f File) KeepsExistingContent() bool {
	return f.IfFileExists == "keep_existing_content"
}

// Validate* functions check structure: allowed field combinations, states, and
// static fields. taskArguments owns the rules for reference-capable values.
func ValidateFile(f File) error {
	if f.State != "" && !slices.Contains(presenceStates, f.State) {
		return invalid("state", "file.state must be present or absent")
	}
	count := 0
	for _, supplied := range []bool{f.Content != nil, f.Template != "", f.Source != "", f.URL != "", f.ContentFromNodePaths != nil} {
		if supplied {
			count++
		}
	}
	if count > 1 {
		return fmt.Errorf("file accepts only one of content, template, source, or content_from_node_paths")
	}
	if f.State == "absent" {
		if count != 0 || f.Owner != "" || f.Group != "" || f.Mode != "" || f.CheckNewFile != nil || f.IfFileExists != "" || f.BackupOriginalFileTo != "" || f.SecretReadableByGroup || f.DangerouslyLetEveryAccountReadSecret {
			return fmt.Errorf("file with state: absent accepts only path and state")
		}
		return nil
	}
	if f.ContentFromNodePaths != nil && len(f.ContentFromNodePaths) == 0 {
		return invalid("content_from_node_paths", "file.content_from_node_paths needs at least one path")
	}
	if f.URL != "" || f.SHA256 != "" || f.DangerouslySkipChecksum {
		if err := validatePin("download", "url", "sha256", f.URL, f.SHA256, f.DangerouslySkipChecksum); err != nil {
			return err
		}
	}
	if f.IfFileExists != "" && !slices.Contains(ifFileExists, f.IfFileExists) {
		return invalid("if_file_exists", f.action()+".if_file_exists must be replace_existing_content or keep_existing_content")
	}
	if f.Decompress != "" && !slices.Contains(decompressions, f.Decompress) {
		return invalid("decompress", "download.decompress must be gzip, bzip2, xz, or zstd")
	}
	if !f.HasContents() {
		if f.Owner == "" && f.Group == "" && f.Mode == "" {
			return fmt.Errorf("file needs content, template, source, content_from_node_paths, or metadata")
		}
		if f.CheckNewFile != nil {
			return fmt.Errorf("file.check_new_file requires content, template, source, or content_from_node_paths")
		}
		if f.IfFileExists != "" {
			return invalid("if_file_exists", "file.if_file_exists requires content, template, source, or content_from_node_paths: if_file_exists controls whether to replace an existing file's content")
		}
	}
	if f.BackupOriginalFileTo != "" && !Dynamic(f.BackupOriginalFileTo) && !Dynamic(f.Path) && filepath.Clean(f.BackupOriginalFileTo) == filepath.Clean(f.Path) {
		return invalid("backup_original_file_to", "file.backup_original_file_to must differ from file.path")
	}
	if f.Mode != "" && !Dynamic(f.Mode) && !modePattern.MatchString(f.Mode) {
		return invalid("permissions_mode", "permissions_mode must be a quoted octal string, such as \"0644\"")
	}
	return validateCheck(f.CheckNewFile)
}

func validateCheck(c *Check) error {
	if c == nil {
		return nil
	}
	if c.Shell == nil {
		return invalid("check_new_file", "check_new_file requires shell")
	}
	if c.Shell.OnlyWhenTrackedInputsChanged {
		return invalid("check_new_file", "check_new_file.shell does not take only_when_tracked_inputs_changed: a check runs for every new file")
	}
	if c.Shell.RunOnlyOnceMarkerFile != "" {
		return invalid("check_new_file", "check_new_file.shell does not take run_only_once_marker_file: a check runs for every new file")
	}
	return ValidateShell(c.Shell)
}

// Download is only the YAML form of a pinned HTTPS file. Loading turns it into
// a File with URL and SHA256; every later stage handles it as that file.
type Download struct {
	SecretReadableByGroup                bool   `yaml:"secret_readable_by_group,omitempty"`
	DangerouslyLetEveryAccountReadSecret bool   `yaml:"dangerously_let_every_account_read_secret,omitempty"`
	URL                                  string `yaml:"url"`
	SHA256                               string `yaml:"sha256,omitempty"`
	// DangerouslySkipChecksum takes the place of SHA256: the file is installed
	// as it arrives.
	DangerouslySkipChecksum bool    `yaml:"dangerously_skip_checksum,omitempty"`
	Decompress              string  `yaml:"decompress,omitempty"` // the file's compression as published; the decompressed file is installed
	Path                    string  `yaml:"path"`
	Owner                   Account `yaml:"owner,omitempty"`
	Group                   Account `yaml:"group,omitempty"`
	Mode                    string  `yaml:"permissions_mode,omitempty"`
	CheckNewFile            *Check  `yaml:"check_new_file,omitempty"`
	IfFileExists            string  `yaml:"if_file_exists,omitempty"`
}

// validatePin checks a download's static HTTPS URL and that it has exactly one
// of its SHA-256 or dangerously_skip_checksum: true, naming the task's own
// fields; download, archive, and repository use it.
func validatePin(action, urlField, digestField, rawURL, digest string, skipChecksum bool) error {
	// A value with references is checked when it is resolved for each node.
	if Dynamic(rawURL) {
		if err := CheckSourceReferences(urlField, rawURL); err != nil {
			return err
		}
	} else if assets.ValidateDownloadURL(rawURL) != nil {
		return invalid(urlField, action+"."+urlField+" must be an HTTPS URL without credentials or a fragment")
	}
	if Dynamic(digest) {
		if skipChecksum {
			return invalid("dangerously_skip_checksum", action+" takes "+digestField+" or dangerously_skip_checksum: true, not both")
		}
		return CheckSourceReferences(digestField, digest)
	}
	switch {
	case skipChecksum && digest != "":
		return invalid("dangerously_skip_checksum", action+" takes "+digestField+" or dangerously_skip_checksum: true, not both")
	case skipChecksum:
		return nil
	case digest == "":
		return invalid(digestField, action+"."+digestField+" is required: the file's SHA-256 as 64 hexadecimal digits. To install the file as it arrives, with no check, write dangerously_skip_checksum: true in its place")
	case !assets.ValidDigest(digest):
		return invalid(digestField, action+"."+digestField+" requires 64 hexadecimal digits")
	}
	return nil
}

func (d Download) File() File {
	return File{SecretReadableByGroup: d.SecretReadableByGroup, DangerouslyLetEveryAccountReadSecret: d.DangerouslyLetEveryAccountReadSecret, Path: d.Path, URL: d.URL, SHA256: d.SHA256, DangerouslySkipChecksum: d.DangerouslySkipChecksum, Decompress: d.Decompress, Owner: d.Owner, Group: d.Group, Mode: d.Mode, CheckNewFile: d.CheckNewFile, IfFileExists: d.IfFileExists}
}

// action is the name a file task was written under, in results, explain, and
// field locations: a pinned URL came from download.
func (f File) action() string {
	if f.URL != "" {
		return "download"
	}
	return "file"
}

// fileDescription is what mmx describe-module shows for file.
var fileDescription = Description{
	Name: "file", Topic: TopicFilesAndLinks, Schema: reflect.TypeOf(File{}),
	Summary: "one regular file from content, template, source, or files already on the node; metadata of an existing file; or removal",
	Fields: []Field{
		{Name: "path", Required: true, Notes: "absolute; the parent directory must exist"},
		{Name: "content", Notes: "inline text; one of content, template, source, or content_from_node_paths"},
		{Name: "template", Notes: "local Go template, relative to the project root, or to the role's directory inside a role; one of content, template, source, or content_from_node_paths"},
		{Name: "source", Notes: "local file copied byte for byte, relative to the project root, or to the role's directory inside a role; one of content, template, source, or content_from_node_paths"},
		{Name: "content_from_node_paths", Notes: "absolute paths on the node; the new content is their bytes in the listed order; a link at the end of a path is followed only when root owns the link, and each must be a regular file; previews show SHA-256 values, never the text; a missing path is not checked in dryrun"},
		{Name: "owner", Type: "account name or integer", Notes: "an account name, or a numeric ID written without quotes"},
		{Name: "group", Type: "group name or integer", Notes: "a group name, or a numeric ID written without quotes"},
		{Name: "permissions_mode", Notes: `quoted octal, as chmod writes it, such as "0644"; may read a reference; required with content, template, source, or content_from_node_paths; optional for metadata only`},
		{Name: "secret_readable_by_group", Default: "false", Notes: "allow the file's group to read content that holds a secret"},
		{Name: "dangerously_let_every_account_read_secret", Default: "false", Notes: "allow every account to read content that holds a secret"},
		{Name: "if_file_exists", Values: ifFileExists, Default: "replace_existing_content", Notes: "keep_existing_content creates a missing file and leaves an existing file's bytes alone, still applying owner, group, and permissions_mode; such a file is not part of its node task block's configuration hash"},
		{Name: "backup_original_file_to", Notes: "absolute path; before MMX first changes an existing file, it copies the file there with its owner, group, and mode, and never writes that copy again"},
		{Name: "check_new_file", Notes: "{shell: {...}} check of the new file at .new_file.path before it replaces the file"},
		{Name: "state", Values: presenceStates, Default: "present", Notes: "absent accepts only path and state"},
	},
	Example: "- file:\n    path: /etc/motd\n    content: \"Managed by MMX.\\n\"\n    owner: root\n    group: root\n    permissions_mode: \"0644\"\n",
	Results: resultBasic,
	Dryrun:  "supported; previews contents and metadata, and the copy backup_original_file_to would make; a validator runs only its own dryrun command, and its failure is not_checked when an earlier node task of the dryrun would change the node; not_checked while the parent directory is missing, unless an earlier node task of the dryrun would create it, and while a file is missing whose metadata alone the task changes",
}

// downloadDescription is what mmx describe-module shows for download.
var downloadDescription = Description{
	Name: "download", Topic: TopicDirectoriesDownloadsAndReleaseDirectories, Schema: reflect.TypeOf(Download{}),
	Summary: "a pinned HTTPS file, fetched and verified on the controller, installed as a file",
	Fields: []Field{
		{Name: "url", Required: true, Notes: "HTTPS URL without credentials or fragment; may read vars, role inputs, loop items, and .node.name, as may sha256"},
		{Name: "sha256", Notes: "the file's SHA-256 in hex; exactly one of sha256 or dangerously_skip_checksum"},
		{Name: "dangerously_skip_checksum", Default: "false", Notes: "true in place of sha256: the file is installed as it arrives, with no check that it is the file you expect"},
		{Name: "decompress", Values: decompressions, Notes: "for a tool published as one compressed file: sha256 is the digest of the file as published; the controller machine checks it, decompresses, and installs the decompressed file"},
		{Name: "path", Required: true, Notes: "absolute; the parent directory must exist"},
		{Name: "owner", Type: "account name or integer", Notes: "an account name, or a numeric ID written without quotes"},
		{Name: "group", Type: "group name or integer", Notes: "a group name, or a numeric ID written without quotes"},
		{Name: "permissions_mode", Required: true, Notes: `quoted octal, as chmod writes it, such as "0755"; may read a reference`},
		{Name: "secret_readable_by_group", Default: "false", Notes: "allow the file's group to read content that holds a secret"},
		{Name: "dangerously_let_every_account_read_secret", Default: "false", Notes: "allow every account to read content that holds a secret"},
		{Name: "if_file_exists", Values: ifFileExists, Default: "replace_existing_content", Notes: "as for file"},
		{Name: "check_new_file", Notes: "as for file"},
	},
	Example: "- download:\n    url: https://downloads.example.com/app-1.2.3-linux-amd64\n    sha256: \"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef\"\n    path: /usr/local/bin/app\n    permissions_mode: \"0755\"\n",
	Results: resultBasic,
	Dryrun:  "supported; fetches and verifies the file, decompresses it when told, then previews it like file",
}

// Prepare checks the inputs when the project loads and reads the source or
// template on the controller machine, relative to base.
func (f *File) Prepare(base string) error {
	if err := ValidateFile(*f); err != nil {
		return err
	}
	// A file or download task states the mode of what it writes. MMX's own
	// writers share ValidateFile and may leave the mode to the file on the
	// node, as ifupdown_interfaces_file does without permissions_mode.
	if f.HasContents() && f.State != "absent" && f.Mode == "" {
		return invalid("permissions_mode", f.action()+".permissions_mode is required when writing content: a quoted octal string, such as \"0644\"")
	}
	f.SourceBase = base
	if f.Source != "" && !Dynamic(f.Source) {
		path, err := localSource(base, "source", f.Source)
		if err != nil {
			return err
		}
		f.LocalPath = path
	}
	if f.Template != "" && !Dynamic(f.Template) {
		path, err := localSource(base, "template", f.Template)
		if err != nil {
			return err
		}
		b, err := readTemplate(path)
		if err != nil {
			return &FieldError{"template", err}
		}
		f.LocalPath, f.TemplateText = path, string(b)
	}
	return nil
}

// Arguments lists the inputs that may hold a reference, with their rules.
func (f *File) Arguments(arguments *ArgumentList) {
	arguments.Public("path", &f.Path, absolute)
	if f.Mode != "" {
		arguments.Public("permissions_mode", &f.Mode, func(v any) error {
			mode, text := value.Plain(v).(string)
			if !text || !modePattern.MatchString(mode) {
				return fmt.Errorf("permissions_mode must be a quoted octal string, such as \"0644\"")
			}
			return nil
		})
	}
	arguments.Owner("owner", &f.Owner)
	arguments.Owner("group", &f.Group)
	if f.Content != nil {
		arguments.Private("content", *f.Content, scalar)
	}
	for i := range f.ContentFromNodePaths {
		arguments.Public(fmt.Sprintf("content_from_node_paths[%d]", i), &f.ContentFromNodePaths[i], absolute)
	}
	if f.BackupOriginalFileTo != "" {
		arguments.Public("backup_original_file_to", &f.BackupOriginalFileTo, absolute)
	}
}

// CleanPaths cleans the resolved paths.
func (f *File) CleanPaths() {
	f.Path = filepath.Clean(f.Path)
	if f.BackupOriginalFileTo != "" {
		f.BackupOriginalFileTo = filepath.Clean(f.BackupOriginalFileTo)
	}
	for i, path := range f.ContentFromNodePaths {
		f.ContentFromNodePaths[i] = filepath.Clean(path)
	}
}

// Reads lists the template and the check of a new file.
func (f *File) Reads() Reads {
	r := Reads{Templates: map[string]string{}, Commands: map[string]*Shell{}}
	if f.Template != "" {
		r.Templates["template"] = f.TemplateText
	}
	if f.CheckNewFile != nil {
		r.Commands["check_new_file.shell"] = f.CheckNewFile.Shell
	}
	return r
}

// ControllerFiles lists the source, template, or download the file comes from.
func (f *File) ControllerFiles() []ControllerFile {
	s := ControllerFile{Base: f.SourceBase, Variants: &f.Variants, Asset: &f.Asset, Template: &f.TemplateText, Local: &f.LocalPath}
	switch {
	case f.Source != "":
		s.Field, s.Kind, s.Path = "source", SourceFile, &f.Source
	case f.Template != "":
		s.Field, s.Kind, s.Path = "template", SourceTemplate, &f.Template
	case f.URL != "":
		s.Field, s.Kind, s.URL, s.SHA256, s.Decompress = "url", SourceDownload, &f.URL, &f.SHA256, f.Decompress
	default:
		return nil
	}
	return []ControllerFile{s}
}

// Run does the module's work on the node.
func (f *File) Run(ctx context.Context, task NodeTask) protocol.Result {
	return task.Runtime.File(ctx, *f, task.Scope)
}

// ManagedPaths lists the paths the task manages or says it creates, as
// written; a dryrun cannot check a later task's path_exists on them.
func (f *File) ManagedPaths() []string {
	return []string{f.Path}
}

// TrackedInChanges opts a file that keeps its existing content out of its
// node task block's tracked inputs: a program that rewrites its own file is
// not restarted for it.
func (f *File) TrackedInChanges() bool {
	return !f.KeepsExistingContent()
}

// ModuleInputs turns the written download task into the file it installs.
func (d *Download) ModuleInputs() (Inputs, error) {
	if d.Mode == "" {
		return nil, invalid("permissions_mode", "download.permissions_mode is required: a quoted octal string, such as \"0644\"")
	}
	f := d.File()
	return &f, nil
}

// notARegularFile is the error for a path that file does not manage, with
// the module that does: a package often installs a link where a file is
// expected, such as nginx's sites-enabled/default.
func notARegularFile(path string, info os.FileInfo) error {
	switch {
	case info.Mode()&os.ModeSymlink != 0:
		return fmt.Errorf("%s is a symbolic link, and file manages only regular files; manage or remove a link with the symlink module (state: absent removes it)", path)
	case info.IsDir():
		return fmt.Errorf("%s is a directory, and file manages only regular files; use the directory module", path)
	}
	return fmt.Errorf("refusing to replace %s, which is not a regular file", path)
}

// HashConfiguration hashes the file's content, or that it is absent.
func (f *File) HashConfiguration(ctx context.Context, h hash.Hash, apply bool, s value.Scope) error {
	if apply && f.State == "absent" {
		_, _ = h.Write([]byte{0})
		return nil
	}
	if apply && f.HasContents() {
		content, err := OpenFileContents(*f, s)
		if err != nil {
			return err
		}
		defer content.Close()
		_, _ = h.Write([]byte{1})
		if _, err := content.Digest(ctx); err != nil {
			return err
		}
		_ = binary.Write(h, binary.BigEndian, uint64(content.Size))
		_, err = assets.Copy(ctx, h, content)
		return err
	}
	st, err := os.Lstat(f.Path)
	if os.IsNotExist(err) {
		_, _ = h.Write([]byte{0})
		return nil
	}
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() {
		return notARegularFile(f.Path, st)
	}
	input, err := os.Open(f.Path)
	if err != nil {
		return err
	}
	defer input.Close()
	_, _ = h.Write([]byte{1})
	_ = binary.Write(h, binary.BigEndian, uint64(st.Size()))
	n, err := assets.Copy(ctx, h, input)
	if err != nil {
		return err
	}
	if n != st.Size() {
		return fmt.Errorf("configuration changed while reading %s", f.Path)
	}
	return nil
}

// TrackedInputs lists the file.
func (f *File) TrackedInputs() []TrackedInput {
	return []TrackedInput{{Kind: "config", Path: f.Path, Content: f}}
}

// PreviewApplies reports whether a dryrun can predict the file: a file that
// keeps its existing content is no tracked input, and content from node paths
// that do not exist yet is hashed as the file on disk; the task reports them.
func (f *File) PreviewApplies() bool {
	if f.KeepsExistingContent() {
		return false
	}
	for _, path := range f.ContentFromNodePaths {
		if _, err := os.Stat(path); err != nil {
			return false
		}
	}
	return true
}

// CheckSecretInputs checks content whose secret use is known while loading.
// References in the mode or its acknowledgement wait until their values are known.
func (f *File) CheckSecretInputs(readsSecret func(any) bool, unresolved func(string) bool) error {
	if f.Content == nil || Dynamic(f.Mode) || !readsSecret(*f.Content) {
		return nil
	}
	mode, _ := strconv.ParseUint(f.Mode, 8, 12) // Prepare has checked a literal mode.
	if mode&0004 != 0 && unresolved("dangerously_let_every_account_read_secret") || mode&0004 == 0 && mode&0040 != 0 && unresolved("secret_readable_by_group") {
		return nil
	}
	return ValidateSecretFilePermissions(f.Path, os.FileMode(mode), f.Group, "node", f.SecretReadableByGroup, f.DangerouslyLetEveryAccountReadSecret)
}

// ValidateSecretFilePermissions checks only read bits: write and execute bits
// do not grant access to the secret's bytes. Shared with controller files.
func ValidateSecretFilePermissions(path string, mode os.FileMode, group Account, machine string, secretReadableByGroup, dangerouslyLetEveryAccountReadSecret bool) error {
	if mode&0004 != 0 {
		if !dangerouslyLetEveryAccountReadSecret {
			return invalid("dangerously_let_every_account_read_secret", fmt.Sprintf("%s holds a secret, and permissions_mode %04o lets every account on the %s read it. If every account should read it, add dangerously_let_every_account_read_secret: true.", path, mode.Perm(), machine))
		}
	} else if mode&0040 != 0 && !secretReadableByGroup {
		reader := "the file's group"
		if group != "" {
			reader = "group " + string(group)
		}
		return invalid("secret_readable_by_group", fmt.Sprintf("%s holds a secret, and permissions_mode %04o lets %s read it. If the group should read it, add secret_readable_by_group: true.", path, mode.Perm(), reader))
	}
	return nil
}
