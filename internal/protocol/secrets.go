package protocol

import (
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"mmx/internal/value"
)

// HidesFailureDetails reports whether Safe hides the reason of a failure,
// which may quote output that holds a secret. The node keeps what such a
// result leaves out for root (HiddenDetailsFile). A configuration error, an
// unavailable value, and a missing prerequisite keep their reason: MMX
// writes those itself, before the task's work runs. So does a reason that
// reads no secret (ReasonReadsNoSecret).
func (r Result) HidesFailureDetails() bool {
	return r.Status == "failed" && r.hidesReason()
}

// hidesReason reports whether Safe replaces the reason of a secret result.
func (r Result) hidesReason() bool {
	return r.Secret && r.Reason != "" && !r.ReasonReadsNoSecret && !r.OutputHoldsNoSecret && r.FailureKind != "config" && r.FailureKind != "unavailable" && r.FailureKind != "missing_prerequisite"
}

// NodeJobRecordRetention is how long a node keeps a finished node job's
// records, so that a disconnected controller machine can still collect the
// result, and with them HiddenDetailsFile, unless the node reboots first.
const NodeJobRecordRetention = 72 * time.Hour

func (r Result) Safe() Result {
	if r.OwedRestore != nil {
		restore := *r.OwedRestore
		restore.Secret = restore.Secret || r.Secret
		restore = restore.Safe()
		r.OwedRestore = &restore
	}
	if r.Secret {
		r.HTTPStatusCode = 0
		if r.ResponseBody != "" {
			r.ResponseBody = "[redacted]"
		}
		if r.DirectoryChange != nil {
			d := *r.DirectoryChange
			d.Examples = append([]FileChange(nil), d.Examples...)
			for i := range d.Examples {
				d.Examples[i].Diff = ""
				d.Examples[i].FullDiff = nil
				d.Examples[i].FullDiffOmitted = "contents suppressed for a task using secrets"
				d.Examples[i].DiffOmitted = "contents suppressed for a task using secrets"
			}
			r.DirectoryChange = &d
		}
		if r.FileChange != nil {
			change := *r.FileChange
			change.Diff = ""
			change.FullDiff = nil
			change.FullDiffOmitted = "contents suppressed for a task using secrets"
			change.DiffOmitted = "contents suppressed for a task using secrets"
			r.FileChange = &change
		}
		if r.Stdout != "" && !r.OutputHoldsNoSecret {
			r.Stdout = "[redacted]"
		}
		if r.Stderr != "" && !r.OutputHoldsNoSecret {
			r.Stderr = "[redacted]"
		}
		if r.PublicKey != "" {
			r.PublicKey = "[redacted]"
		}
		if r.Diff != "" {
			r.Diff = "[redacted]"
		}
		if r.ModuleOutputs != nil {
			redacted := map[string]any{}
			for name := range r.ModuleOutputs {
				redacted[name] = "[redacted]"
			}
			r.ModuleOutputs = redacted
		}
		if r.hidesReason() {
			r.Reason = "details suppressed for a task using secrets"
			if r.HiddenDetailsFile != "" {
				r.Reason += "; root can read them on the node in " + r.HiddenDetailsFile + " for " + strconv.Itoa(int(NodeJobRecordRetention.Hours())) + " hours, or until the node reboots"
			}
		}
		if (r.Value != nil || r.ValueSet) && !value.HasSecrets(r.Value) {
			r.Value = value.Sensitive(r.Value)
		}
		if r.Content != nil && !value.HasSecrets(r.Content) {
			r.Content = value.Sensitive(r.Content)
		}
	}
	if r.CheckNewFile != nil {
		v := r.CheckNewFile.Safe()
		if r.Secret {
			v.Secret = true
			v = v.Safe()
		}
		r.CheckNewFile = &v
	}
	r.Tries = append([]Result(nil), r.Tries...)
	for i, v := range r.Tries {
		if r.Secret {
			v.Secret = true
		}
		r.Tries[i] = v.Safe()
	}
	r.Items = append([]Result(nil), r.Items...)
	for i, v := range r.Items {
		r.Items[i] = v.Safe()
	}
	return r
}

// WithoutSecretCommandOutput replaces the supplied secret forms only
// in the command diagnostics that output_holds_no_secret allows to be shown.
// It uses the same replacement rules as logins handed only to a tool.
func (r Result) WithoutSecretCommandOutput(secrets []string) Result {
	text := func(s string) string { return value.TextWithoutLoginsHandedOnlyToATool(s, secrets) }
	if r.OutputHoldsNoSecret {
		r.Stdout, r.Stderr, r.Reason = text(r.Stdout), text(r.Stderr), text(r.Reason)
	}
	if r.CheckNewFile != nil {
		check := r.CheckNewFile.WithoutSecretCommandOutput(secrets)
		r.CheckNewFile = &check
		if check.OutputHoldsNoSecret && r.ReasonReadsNoSecret {
			r.Reason = text(r.Reason)
		}
	}
	r.Tries = append([]Result(nil), r.Tries...)
	for i := range r.Tries {
		r.Tries[i] = r.Tries[i].WithoutSecretCommandOutput(secrets)
	}
	return r
}

// WithoutLoginsHandedOnlyToATool returns r with every occurrence of one of
// logins replaced by [redacted] in every text that r reports, and in each of
// its tries, its loop items, and its check of a new file. logins are the
// texts that the task's trace remembered (value.Scope.LoginHandedOnlyToATool):
// each login with a secret that a module handed only to its tool, in each of
// its forms. The rest of r stays, so a failure keeps its reason. A full
// difference is a stored file whose text cannot be replaced here, so r
// reports none.
func (r Result) WithoutLoginsHandedOnlyToATool(logins []string) Result {
	if len(logins) == 0 {
		return r
	}
	text := func(s string) string { return value.TextWithoutLoginsHandedOnlyToATool(s, logins) }
	fileChange := func(c FileChange) FileChange {
		c.Diff = text(c.Diff)
		if c.FullDiff != nil {
			c.FullDiff, c.FullDiffOmitted = nil, "full difference omitted for a task that hands a secret login to a tool"
		}
		return c
	}
	r.Reason, r.Stdout, r.Stderr = text(r.Reason), text(r.Stdout), text(r.Stderr)
	r.ResponseBody, r.PublicKey, r.Diff = text(r.ResponseBody), text(r.PublicKey), text(r.Diff)
	r.Value, r.Content = value.WithoutLoginsHandedOnlyToATool(r.Value, logins), value.WithoutLoginsHandedOnlyToATool(r.Content, logins)
	r.Outputs, _ = value.WithoutLoginsHandedOnlyToATool(r.Outputs, logins).(map[string]any)
	r.ModuleOutputs, _ = value.WithoutLoginsHandedOnlyToATool(r.ModuleOutputs, logins).(map[string]any)
	if r.OwedRestore != nil {
		restore := *r.OwedRestore
		restore.OriginalFailureReason = text(restore.OriginalFailureReason)
		restore.RestoreFailureReason = text(restore.RestoreFailureReason)
		r.OwedRestore = &restore
	}
	if r.FileChange != nil {
		change := fileChange(*r.FileChange)
		r.FileChange = &change
	}
	if r.DirectoryChange != nil {
		d := *r.DirectoryChange
		d.Examples = append([]FileChange(nil), d.Examples...)
		for i := range d.Examples {
			d.Examples[i] = fileChange(d.Examples[i])
		}
		r.DirectoryChange = &d
	}
	if r.CheckNewFile != nil {
		check := r.CheckNewFile.WithoutLoginsHandedOnlyToATool(logins)
		r.CheckNewFile = &check
	}
	r.Tries = append([]Result(nil), r.Tries...)
	for i := range r.Tries {
		r.Tries[i] = r.Tries[i].WithoutLoginsHandedOnlyToATool(logins)
	}
	r.Items = append([]Result(nil), r.Items...)
	for i := range r.Items {
		r.Items[i] = r.Items[i].WithoutLoginsHandedOnlyToATool(logins)
	}
	return r
}

// Safe preserves the identity and recorded restore operations while hiding
// diagnostics from a task that used secrets. It does not modify its source.
func (r OwedRestore) Safe() OwedRestore {
	if r.Secret {
		if r.OriginalFailureReason != "" {
			r.OriginalFailureReason = "details suppressed for a task using secrets"
		}
		if r.RestoreFailureReason != "" {
			r.RestoreFailureReason = "details suppressed for a task using secrets"
		}
	}
	return r
}
func (r Result) MarshalJSON() ([]byte, error) {
	type plain Result
	safe := r.Safe()
	if safe.ValueSet && safe.Value == nil {
		return json.Marshal(struct {
			plain
			Value any `json:"value"`
		}{plain: plain(safe), Value: nil})
	}
	return json.Marshal(plain(safe))
}
func (r *Result) UnmarshalJSON(b []byte) error {
	type plain Result
	if err := Decode(b, (*plain)(r)); err != nil {
		return err
	}
	var fields struct {
		Value json.RawMessage `json:"value"`
	}
	if err := json.Unmarshal(b, &fields); err != nil {
		return err
	}
	r.ValueSet = fields.Value != nil
	r.Value = value.Restore(r.Value)
	r.Content = value.Restore(r.Content)
	r.Outputs = value.Restore(r.Outputs).(map[string]any)
	return nil
}
func (r *Response) UnmarshalJSON(b []byte) error {
	type plain Response
	if err := Decode(b, (*plain)(r)); err != nil {
		return err
	}
	r.Outputs = value.Restore(r.Outputs).(map[string]any)
	return nil
}

// Transport includes private fleet stage exports only on the authenticated link.
// Task diagnostics always use their safe form, including on that link.
func (r Response) Transport() any {
	type plain Response
	r.Outputs = value.Transport(r.Outputs).(map[string]any)
	return plain(r)
}

// FactsRequest asks a node for its facts, measured as a node job measures
// them: the controller process reads them for a fleet stage's when that reads
// node facts, just before the node would start. Node is the node's name in
// the inventory, which the facts report as name.
type FactsRequest struct {
	Node string `json:"node"`
}

// ReadFileRequest asks a node for one file's content: mmx resume reads a
// secret that a read_file task read on that node again, since the secret was
// never saved.
type ReadFileRequest struct {
	Path string `json:"path"`
}

// ReadFileResponse holds the file's content, as read_file reads it. It
// travels only on the authenticated connection.
type ReadFileResponse struct {
	Content any `json:"content"`
}

func (r *ReadFileResponse) UnmarshalJSON(b []byte) error {
	type plain ReadFileResponse
	if err := Decode(b, (*plain)(r)); err != nil {
		return err
	}
	// A file's content is text or binary content, never another record.
	r.Content = value.Restore(r.Content)
	switch r.Content.(type) {
	case string, value.Binary:
		return nil
	}
	return fmt.Errorf("the node answered with read-file content that is neither text nor binary content")
}
