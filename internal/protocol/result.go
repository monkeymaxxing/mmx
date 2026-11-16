package protocol

import (
	"errors"

	"mmx/internal/assets"

	"mmx/internal/value"
)

func ResultValue(r Result) map[string]any {
	var stdout, stderr, reason any = r.Stdout, r.Stderr, r.Reason
	var responseBody any = r.ResponseBody
	var httpStatusCode any = r.HTTPStatusCode
	// A hidden value keeps why its node task was hidden, for errors about it.
	hide := func(v any) any { return &value.Secret{Data: v, Available: true, HiddenBecause: r.SecretBecause} }
	if r.ResponseBodyTruncated {
		responseBody = value.Truncated{Text: r.ResponseBody}
	}
	if r.StdoutTruncated {
		stdout = value.Truncated{Text: r.Stdout}
	}
	if r.StderrTruncated {
		stderr = value.Truncated{Text: r.Stderr}
	}
	if r.Secret {
		responseBody, httpStatusCode = hide(responseBody), hide(httpStatusCode)
		stdout, stderr, reason = hide(stdout), hide(stderr), hide(reason)
		if (r.Value != nil || r.ValueSet) && !value.HasSecrets(r.Value) {
			r.Value = hide(r.Value)
		}
	}
	items := make([]any, len(r.Items))
	for i, item := range r.Items {
		items[i] = ResultValue(item)
	}
	fields := map[string]any{"outputs": r.Outputs, "items": items, "iteration_key": r.IterationKey, "status": r.Status, "change": r.Change, "exit_code": r.ExitCode, "stdout": stdout, "stderr": stderr, "reason": reason}
	fields["stdout_json"] = value.ParsedJSON("stdout", r.Stdout, r.StdoutTruncated, r.Secret)
	if r.HTTPStatusCode != 0 {
		fields["http_status_code"], fields["response_body"] = httpStatusCode, responseBody
		fields["response_body_json"] = value.ParsedJSON("response_body", r.ResponseBody, r.ResponseBodyTruncated, r.Secret)
	}
	if r.Value != nil || r.ValueSet {
		fields["value"] = r.Value
	}
	for name, output := range r.ModuleOutputs {
		if r.Secret && !value.HasSecrets(output) {
			output = hide(output)
		}
		fields[name] = output
	}
	if r.PublicKey != "" {
		var publicKey any = r.PublicKey
		if r.Secret {
			publicKey = hide(publicKey)
		}
		fields["public_key"] = publicKey
	}
	if r.Content != nil {
		content := r.Content
		if r.Secret && !value.HasSecrets(content) {
			content = hide(content)
		}
		fields["content"] = content
	}
	return fields
}
func Success(change string) Result {
	return Result{Status: "succeeded", Change: change}
}
func Failed(err error) Result {
	r := Result{Status: "failed", Change: "unknown", Reason: err.Error()}
	var unavailable *value.UnavailableError
	if errors.As(err, &unavailable) {
		r.Status, r.FailureKind = "blocked", "unavailable"
	}
	if errors.As(err, new(*value.RetryCannotFixError)) {
		r.FailureKind = "config"
	}
	return r
}

// ConfigurationError stops automatic repetition for invalid module inputs.
func ConfigurationError(err error) Result {
	r := Failed(err)
	r.FailureKind = "config"
	return r
}

// MissingPrerequisite stops automatic repetition when something an earlier
// node task or a person must provide, such as a package, program, account,
// or file, is missing: the node does not change between tries.
func MissingPrerequisite(err error) Result {
	r := Failed(err)
	r.FailureKind = "missing_prerequisite"
	return r
}

// FileChange describes the requested change. Result.Status and Change establish
// whether it was applied, previewed, or rejected.
type FileChange struct {
	FullDiff        *assets.Ref `json:"full_diff,omitempty"`
	FullDiffOmitted string      `json:"full_diff_omitted,omitempty"`
	Path            string      `json:"path"`
	Action          string      `json:"action"`
	Before          *FileState  `json:"before"`
	After           *FileState  `json:"after"`
	Diff            string      `json:"diff,omitempty"`
	DiffOmitted     string      `json:"diff_omitted,omitempty"`
	// OriginalFileCopiedTo is where the file's original was copied before the
	// change (backup_original_file_to).
	OriginalFileCopiedTo string `json:"original_file_copied_to,omitempty"`
}
type FileState struct {
	Kind   string `json:"kind"`
	Size   int64  `json:"size,omitempty"`
	Mode   string `json:"permissions_mode,omitempty"`
	UID    int    `json:"uid"`
	GID    int    `json:"gid"`
	Target string `json:"target,omitempty"`
	// SHA256 is shown for content MMX does not show as text.
	SHA256 string `json:"sha256,omitempty"`
}

// DirectoryChange is one local traversal's aggregate, never a per-entry ledger.
type DirectoryChange struct {
	Copy            bool         `json:"copy,omitempty"`
	Path            string       `json:"path"`
	Directories     int          `json:"directories"`
	Files           int          `json:"files"`
	Changes         int          `json:"changes"`
	Applied         int          `json:"applied"`
	SkippedSymlinks int          `json:"skipped_symlinks"`
	SkippedSpecial  int          `json:"skipped_special"`
	SkippedMounts   int          `json:"skipped_mounts"`
	Examples        []FileChange `json:"examples,omitempty"`
	// CreatedParentDirectories lists the missing parent directories that
	// create_missing_parent_directories created, or would create in a dryrun.
	CreatedParentDirectories []string `json:"created_parent_directories,omitempty"`
}
