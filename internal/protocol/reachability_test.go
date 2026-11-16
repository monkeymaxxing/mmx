package protocol

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func confirmationRequest() ReachabilityConfirmationRequest {
	return ReachabilityConfirmationRequest{strings.Repeat("a", 64), "0123456789abcdef", ConfirmationBeforeChange}
}

func pendingConfirmation() PendingReachabilityConfirmation {
	r := confirmationRequest()
	now := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)
	return PendingReachabilityConfirmation{
		NodeJobID: r.NodeJobID, OwedRestoreID: r.OwedRestoreID, ConfirmationPhase: r.ConfirmationPhase,
		NodeTime: now, ConfirmationDeadline: now.Add(time.Minute),
		LatestAfterChangeConfirmationDeadline: now.Add(3 * time.Minute),
		ConnectionKindsToConfirm:              []ConnectionKind{ConnectionSSH, ConnectionHTTPS},
		ConfirmedConnectionKinds:              []ConnectionKind{ConnectionSSH},
	}
}

func TestReachabilityRequestRejectsInvalidIdentityAndClaimedConnection(t *testing.T) {
	valid, err := json.Marshal(confirmationRequest())
	if err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{
		"missing node job":   strings.Replace(string(valid), strings.Repeat("a", 64), "", 1),
		"short node job":     strings.Replace(string(valid), strings.Repeat("a", 64), "a", 1),
		"short change":       strings.Replace(string(valid), "0123456789abcdef", "01234567", 1),
		"uppercase change":   strings.Replace(string(valid), "0123456789abcdef", "0123456789abcdeF", 1),
		"path in change":     strings.Replace(string(valid), "0123456789abcdef", "../3456789abcdef", 1),
		"unknown phase":      strings.Replace(string(valid), "before_change", "activating", 1),
		"claimed connection": strings.TrimSuffix(string(valid), "}") + `,"received_connection_kind":"https"}`,
		"second value":       string(valid) + `{}`,
		"null":               "null",
	} {
		t.Run(name, func(t *testing.T) {
			var request ReachabilityConfirmationRequest
			if err := Decode([]byte(body), &request); err == nil {
				t.Fatal("accepted invalid request", body)
			}
		})
	}
	for _, phase := range []ConfirmationPhase{ConfirmationBeforeChange, ConfirmationAfterChange} {
		request := confirmationRequest()
		request.ConfirmationPhase = phase
		body, _ := json.Marshal(request)
		var decoded ReachabilityConfirmationRequest
		if err := Decode(body, &decoded); err != nil || decoded != request {
			t.Fatal(decoded, err)
		}
	}
}

func TestPendingReachabilityConfirmationRejectsInvalidRequirements(t *testing.T) {
	for name, change := range map[string]func(*PendingReachabilityConfirmation){
		"no connection kind": func(p *PendingReachabilityConfirmation) { p.ConnectionKindsToConfirm = nil },
		"unknown kind":       func(p *PendingReachabilityConfirmation) { p.ConnectionKindsToConfirm = []ConnectionKind{"tcp"} },
		"duplicate requirement": func(p *PendingReachabilityConfirmation) {
			p.ConnectionKindsToConfirm = []ConnectionKind{ConnectionSSH, ConnectionSSH}
		},
		"duplicate confirmation": func(p *PendingReachabilityConfirmation) {
			p.ConfirmedConnectionKinds = []ConnectionKind{ConnectionSSH, ConnectionSSH}
		},
		"unrequested confirmation": func(p *PendingReachabilityConfirmation) {
			p.ConnectionKindsToConfirm = []ConnectionKind{ConnectionHTTPS}
		},
		"missing node time":       func(p *PendingReachabilityConfirmation) { p.NodeTime = time.Time{} },
		"missing deadline":        func(p *PendingReachabilityConfirmation) { p.ConfirmationDeadline = time.Time{} },
		"missing latest deadline": func(p *PendingReachabilityConfirmation) { p.LatestAfterChangeConfirmationDeadline = time.Time{} },
		"local time":              func(p *PendingReachabilityConfirmation) { p.NodeTime = p.NodeTime.In(time.FixedZone("local", 3600)) },
		"reversed deadlines": func(p *PendingReachabilityConfirmation) {
			p.LatestAfterChangeConfirmationDeadline = p.ConfirmationDeadline.Add(-time.Second)
		},
		"wrong phase": func(p *PendingReachabilityConfirmation) { p.ConfirmationPhase = "waiting" },
	} {
		t.Run(name, func(t *testing.T) {
			pending := pendingConfirmation()
			change(&pending)
			body, err := json.Marshal(pending)
			if err != nil {
				t.Fatal(err)
			}
			var decoded PendingReachabilityConfirmation
			if err := Decode(body, &decoded); err == nil {
				t.Fatal("accepted invalid confirmation requirements", string(body))
			}
		})
	}
}

func TestProgressRetainsExpiredConfirmationAndLatestDeadline(t *testing.T) {
	pending := pendingConfirmation()
	pending.NodeTime = pending.LatestAfterChangeConfirmationDeadline.Add(time.Second)
	// Expired progress is legitimate and must retain the change for reconciliation.
	response := Response{PendingReachabilityConfirmation: &pending}
	body, err := json.Marshal(response)
	var decoded Response
	if err != nil {
		t.Fatal(err)
	}
	if err := Decode(body, &decoded); err != nil {
		t.Fatal(err)
	}
	request := ProgressRequest{KnownReachabilityConfirmation: decoded.PendingReachabilityConfirmation}
	body, err = json.Marshal(request)
	var progress ProgressRequest
	if err != nil {
		t.Fatal(err)
	}
	if err := Decode(body, &progress); err != nil {
		t.Fatal(err)
	}
	if p := progress.KnownReachabilityConfirmation; p == nil || p.OwedRestoreID != pending.OwedRestoreID || !p.LatestAfterChangeConfirmationDeadline.Equal(pending.LatestAfterChangeConfirmationDeadline) || len(p.ConfirmedConnectionKinds) != 1 {
		t.Fatal(progress)
	}
}

func TestReachabilityResponseRejectsUnknownFieldsAndConnectionKind(t *testing.T) {
	r := confirmationRequest()
	response := ReachabilityConfirmationResponse{NodeJobID: r.NodeJobID, OwedRestoreID: r.OwedRestoreID,
		ConfirmationPhase: r.ConfirmationPhase, ReceivedConnectionKind: ConnectionSSH,
		ConfirmationCounted: true, ConfirmationReason: "confirmation_recorded"}
	body, _ := json.Marshal(response)
	for _, invalid := range []string{
		strings.Replace(string(body), `"ssh"`, `"tcp"`, 1),
		strings.TrimSuffix(string(body), "}") + `,"unknown":true}`,
	} {
		var decoded ReachabilityConfirmationResponse
		if err := Decode([]byte(invalid), &decoded); err == nil {
			t.Fatal("accepted invalid response", invalid)
		}
	}
}

func TestOwedRestoreSuppressesSecretsInsideAndOutsideTaskResults(t *testing.T) {
	restore := OwedRestore{OwedRestoreID: "0123456789abcdef", DestinationFile: "/etc/network/example",
		RestoreStatus: "failed", PreviousFileRestored: true,
		OriginalFailureReason: "activation printed PRIVATE", RestoreFailureReason: "restore printed PRIVATE"}
	secretRestore := restore
	secretRestore.Secret = true
	for name, result := range map[string]any{
		"task secret":            Result{Secret: true, OwedRestore: &restore},
		"restore secret":         Result{OwedRestore: &secretRestore},
		"interrupted node job":   Response{OwedRestores: []OwedRestore{secretRestore}},
		"authenticated response": (Response{OwedRestores: []OwedRestore{secretRestore}}).Transport(),
		"nested try":             Result{Secret: true, Tries: []Result{{OwedRestore: &restore}}},
	} {
		t.Run(name, func(t *testing.T) {
			body, err := json.Marshal(result)
			if err != nil || strings.Contains(string(body), "PRIVATE") {
				t.Fatal(string(body), err)
			}
			for _, kept := range []string{restore.OwedRestoreID, restore.DestinationFile, `"previous_file_restored":true`, `"restore_status":"failed"`} {
				if !strings.Contains(string(body), kept) {
					t.Fatal("lost restore outcome", string(body))
				}
			}
			if !strings.Contains(string(body), "details suppressed for a task using secrets") {
				t.Fatal("missing suppression reason", string(body))
			}
		})
	}
	if !strings.Contains(restore.OriginalFailureReason, "PRIVATE") || !strings.Contains(secretRestore.RestoreFailureReason, "PRIVATE") {
		t.Fatal("safe serialization changed the source")
	}
	result := (Result{OwedRestore: &restore}).WithoutLoginsHandedOnlyToATool([]string{"PRIVATE"})
	if strings.Contains(result.OwedRestore.OriginalFailureReason+result.OwedRestore.RestoreFailureReason, "PRIVATE") || !strings.Contains(restore.OriginalFailureReason, "PRIVATE") {
		t.Fatal("login suppression leaked or changed the source")
	}
}

func TestOwedRestoreJSONIncludesOnlyApplicableRestorationSteps(t *testing.T) {
	for _, file := range []bool{false, true} {
		for _, restored := range []bool{false, true} {
			t.Run(fmt.Sprintf("file=%t/restored=%t", file, restored), func(t *testing.T) {
				outcome := OwedRestore{OwedRestoreID: "0123456789abcdef", ModuleName: "example", RestoreStatus: "running"}
				fileFields := []string{"previous_file_restored", "previous_config_activated", "obsolete_virtual_devices_removed"}
				if file {
					outcome.DestinationFile = "/etc/example.conf"
					outcome.PreviousFileRestored, outcome.PreviousConfigActivated, outcome.ObsoleteVirtualDevicesRemoved = restored, restored, restored
				} else {
					outcome.ChangeSubject = "example membership"
					outcome.PreviousMembershipRestored = restored
				}
				encoded, err := json.Marshal(outcome)
				if err != nil {
					t.Fatal(err)
				}
				var fields map[string]json.RawMessage
				if err := json.Unmarshal(encoded, &fields); err != nil {
					t.Fatal(err)
				}
				if string(fields["owed_restore_id"]) != `"0123456789abcdef"` {
					t.Fatal(string(encoded))
				}
				for _, name := range append(fileFields, "destination_file") {
					if _, present := fields[name]; present != file {
						t.Fatalf("%s presence: %s", name, encoded)
					}
				}
				if _, present := fields["previous_membership_restored"]; present == file {
					t.Fatal(string(encoded))
				}
				applicable := fileFields
				if !file {
					applicable = []string{"previous_membership_restored"}
				}
				for _, name := range applicable {
					if string(fields[name]) != fmt.Sprint(restored) {
						t.Fatalf("%s: %s", name, encoded)
					}
				}
				var decoded OwedRestore
				if err := Decode(encoded, &decoded); err != nil || decoded != outcome {
					t.Fatal(decoded, err)
				}
			})
		}
	}
}
