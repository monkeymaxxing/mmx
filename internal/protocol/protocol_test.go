package protocol

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"mmx/internal/assets"
	"mmx/internal/value"
)

func TestStatusPropertiesAndChangePrecedenceAreConsistent(t *testing.T) {
	for s, p := range statuses {
		if (p.uncertain && (p.terminal || p.completed)) || (p.completed && !p.terminal) || (p.failure && (p.completed || !p.terminal)) {
			t.Fatalf("%s has contradictory properties: %+v", s, p)
		}
	}
	if !Uncertain("not_found") || Completed("") || Rank("") != Rank("pending") || Unconfirmed("running") {
		t.Fatal("pending, running, or not_found lost its meaning")
	}
	order := []string{"unchanged", "unknown", "would_change", "changed"}
	for i, a := range order {
		for j, b := range order {
			if got := CombineChange(a, b); got != order[max(i, j)] {
				t.Fatalf("%s + %s = %s", a, b, got)
			}
		}
	}
	if CombineChange("", "") != "unchanged" {
		t.Fatal("empty change is not unchanged")
	}
}

func TestReadLimitedAcceptsTheLimitAndRejectsOneMoreByte(t *testing.T) {
	if b, err := ReadLimited(strings.NewReader("1234"), 4, ErrRequestLimit); err != nil || string(b) != "1234" {
		t.Fatal(string(b), err)
	}
	if _, err := ReadLimited(strings.NewReader("12345"), 4, ErrRequestLimit); !errors.Is(err, ErrRequestLimit) {
		t.Fatal(err)
	}
}

func TestProtocolPreservesIntegersAndRejectsTrailingData(t *testing.T) {
	var v map[string]any
	if err := Decode([]byte(`{"count":9007199254740993}`), &v); err != nil {
		t.Fatal(err)
	}
	if v["count"] != json.Number("9007199254740993") {
		t.Fatal("integer lost precision", v)
	}
	if err := Decode([]byte(`{} {}`), &v); err == nil {
		t.Fatal("accepted trailing JSON")
	}
}

func TestRenamedResultFieldsRoundTripAndRedact(t *testing.T) {
	r := Result{Status: "succeeded", Change: "unchanged", HTTPStatusCode: 201, ResponseBody: "ready", Tries: []Result{{Status: "failed", HTTPStatusCode: 503, ResponseBody: "starting"}}, TriesOmitted: 4}
	data, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{`"http_status_code":201`, `"response_body":"ready"`, `"tries":`, `"tries_omitted":4`} {
		if !strings.Contains(string(data), field) {
			t.Fatal(string(data))
		}
	}
	for _, field := range []string{`"executions"`, `"executions_omitted"`, `"stdout"`, `"value"`} {
		if strings.Contains(string(data), field) {
			t.Fatal(string(data))
		}
	}
	var restored Result
	if err := Decode(data, &restored); err != nil || restored.HTTPStatusCode != 201 || restored.ResponseBody != "ready" || len(restored.Tries) != 1 || restored.TriesOmitted != 4 {
		t.Fatal(restored, err)
	}
	for _, old := range []string{`{"executions":[]}`, `{"executions_omitted":1}`} {
		if err := Decode([]byte(old), &restored); err == nil {
			t.Fatal("accepted", old)
		}
	}
	r.Secret = true
	data, err = json.Marshal(r)
	if err != nil || strings.Contains(string(data), "ready") || strings.Contains(string(data), "starting") {
		t.Fatal(string(data), err)
	}
	values := ResultValue(r)
	if !value.HasSecrets(values["http_status_code"]) || !value.HasSecrets(values["response_body"]) {
		t.Fatal(values)
	}
}

// A login that a module handed only to its tool is replaced in every text
// that a result reports, and the rest of the result stays.
func TestResultWithoutLoginsHandedOnlyToATool(t *testing.T) {
	const login, stored = "s3cret-sentinel", "ZGVwbG95OnMzY3JldC1zZW50aW5lbA=="
	logins := []string{login, stored}
	told := "docker said " + login + " and " + stored + " were refused"
	const replaced = "docker said [redacted] and [redacted] were refused"
	full := func() *assets.Ref { return &assets.Ref{SHA256: strings.Repeat("a", 64), Size: 12} }
	failed := func() Result {
		return Result{Status: "failed", Change: "unknown", Reason: told, Stdout: told, Stderr: told}
	}
	r := failed()
	r.Task, r.ExitCode, r.FailureKind, r.ResponseBody, r.PublicKey, r.Diff = "pull", 1, "missing_prerequisite", told, told, told
	r.Value = map[string]any{"image_id": "sha256:1", "services": []any{map[string]any{"reason": told}}}
	r.Content = told
	r.Outputs = map[string]any{"said": told}
	r.ModuleOutputs = map[string]any{"said": told}
	r.FileChange = &FileChange{Path: "/etc/app.conf", Action: "update", Diff: told, FullDiff: full()}
	r.DirectoryChange = &DirectoryChange{Path: "/etc/app", Examples: []FileChange{{Path: "/etc/app/a", Diff: told, FullDiff: full()}}}
	check := failed()
	r.CheckNewFile = &check
	r.Tries = []Result{failed(), failed()}
	item := failed()
	item.Tries = []Result{failed()}
	r.Items = []Result{item}

	got := r.WithoutLoginsHandedOnlyToATool(logins)
	saved, err := json.Marshal(got)
	if err != nil || strings.Contains(string(saved), login) || strings.Contains(string(saved), stored) {
		t.Fatalf("a login is left in the result: %s %v", saved, err)
	}
	// The actual result, which later node tasks read, has no login either.
	if text := fmt.Sprintf("%+v %+v %+v %+v %+v", got, *got.FileChange, *got.DirectoryChange, *got.CheckNewFile, ResultValue(got)); strings.Contains(text, login) || strings.Contains(text, stored) {
		t.Fatalf("a login is left in the result's values: %s", text)
	}
	for name, text := range map[string]any{
		"reason": got.Reason, "stdout": got.Stdout, "stderr": got.Stderr, "response_body": got.ResponseBody, "public_key": got.PublicKey, "diff": got.Diff,
		"content": got.Content, "outputs": got.Outputs["said"], "module outputs": got.ModuleOutputs["said"],
		"value":       got.Value.(map[string]any)["services"].([]any)[0].(map[string]any)["reason"],
		"file change": got.FileChange.Diff, "directory change": got.DirectoryChange.Examples[0].Diff,
		"check of a new file": got.CheckNewFile.Reason, "try": got.Tries[1].Stderr, "item": got.Items[0].Reason, "try of an item": got.Items[0].Tries[0].Stdout,
	} {
		if text != replaced {
			t.Errorf("%s: %q", name, text)
		}
	}
	if got.Task != "pull" || got.Status != "failed" || got.Change != "unknown" || got.ExitCode != 1 || got.FailureKind != "missing_prerequisite" || got.Secret ||
		got.Value.(map[string]any)["image_id"] != "sha256:1" || got.FileChange.Path != "/etc/app.conf" || got.FileChange.Action != "update" {
		t.Fatalf("the rest of the result changed: %+v", got)
	}
	// A full difference is a stored file, which nothing replaces a text in.
	const omitted = "full difference omitted for a task that hands a secret login to a tool"
	for _, change := range []FileChange{*got.FileChange, got.DirectoryChange.Examples[0]} {
		if change.FullDiff != nil || change.FullDiffOmitted != omitted {
			t.Fatalf("%+v", change)
		}
	}
	// The result that was given is not changed in place.
	if r.Reason != told || r.Tries[0].Reason != told || r.Items[0].Tries[0].Reason != told || r.FileChange.Diff != told || r.FileChange.FullDiff == nil ||
		r.DirectoryChange.Examples[0].Diff != told || r.CheckNewFile.Reason != told || r.Outputs["said"] != told {
		t.Fatal("the given result was changed in place")
	}
	// Without a login, nothing changes: a full difference stays.
	same := r.WithoutLoginsHandedOnlyToATool(nil)
	if same.Reason != told || same.FileChange.FullDiff == nil || same.FileChange.FullDiffOmitted != "" {
		t.Fatalf("%+v", same)
	}
}

// The hidden reason of a failed node task names the file in which the node
// keeps the details, also when a result passes through Safe again; a
// reason that MMX writes itself, such as a configuration error, stays.
func TestSafeReasonNamesTheFileOfTheHiddenDetails(t *testing.T) {
	file := "/run/mmx/private/0123-hidden-details-of-failed-node-tasks.txt"
	failed := Result{Task: "insert_rows", Status: "failed", Secret: true, Reason: "exit code 1: ERROR: sentinel-row", Stderr: "ERROR: sentinel-row", HiddenDetailsFile: file}
	want := "details suppressed for a task using secrets; root can read them on the node in " + file + " for 72 hours, or until the node reboots"
	if got := failed.Safe(); got.Reason != want || got.Safe().Reason != want || got.Stderr != "[redacted]" {
		t.Fatalf("%+v", got)
	}
	if !failed.HidesFailureDetails() {
		t.Fatal("a failed secret result hides its reason")
	}
	for name, r := range map[string]Result{
		"a configuration error": {Status: "failed", Secret: true, Reason: "file.path: must be absolute", FailureKind: "config"},
		"no secret":             {Status: "failed", Reason: "exit code 1"},
		"a success":             {Status: "succeeded", Secret: true, Reason: "wrote the file"},
	} {
		if r.HidesFailureDetails() {
			t.Errorf("%s keeps no hidden details", name)
		}
	}
}

func TestPromisedCommandDiagnosticsLeaveOtherSecretFieldsHidden(t *testing.T) {
	command := Result{Secret: true, OutputHoldsNoSecret: true, Status: "failed", Reason: "invalid token abcdefg", Stdout: "progress abcde", Stderr: "invalid cdefg", ModuleOutputs: map[string]any{"token": "abcde"}, Diff: "abcde"}
	r := Result{Secret: true, ReasonReadsNoSecret: true, Status: "failed", Reason: "check_new_file failed: invalid token abcdefg", CheckNewFile: &command, FileChange: &FileChange{Diff: "abcde"}}
	r = r.WithoutSecretCommandOutput([]string{"abcde", "cdefg"}).Safe()
	if r.HidesFailureDetails() || r.Reason != "check_new_file failed: invalid token [redacted]" || r.FileChange.Diff != "" {
		t.Fatal(r)
	}
	check := r.CheckNewFile
	if check.HidesFailureDetails() || check.Stdout != "progress [redacted]" || check.Stderr != "invalid [redacted]" || check.Diff != "[redacted]" || check.ModuleOutputs["token"] != "[redacted]" {
		t.Fatal(check)
	}
	b, err := json.Marshal(r)
	var received Result
	if err != nil || Decode(b, &received) != nil || received.Safe().CheckNewFile.Stderr != "invalid [redacted]" {
		t.Fatalf("promised diagnostic lost on the connection: %s, %v", b, err)
	}
	if command.Stdout != "progress abcde" || command.Stderr != "invalid cdefg" {
		t.Fatal("redaction changed the source result")
	}
}
