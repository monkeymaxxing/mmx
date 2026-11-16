package protocol

import (
	"encoding/json"
	"fmt"
	"slices"
	"time"
)

// ConnectionKind is the connection that received a reachability confirmation.
// The receiving process supplies it; a request cannot select it.
type ConnectionKind string

const (
	ConnectionSSH   ConnectionKind = "ssh"
	ConnectionHTTPS ConnectionKind = "https"
)

type ConfirmationPhase string

const (
	ConfirmationBeforeChange ConfirmationPhase = "before_change"
	ConfirmationAfterChange  ConfirmationPhase = "after_change"
)

type ReachabilityConfirmationRequest struct {
	NodeJobID         string            `json:"node_job_id"`
	OwedRestoreID     string            `json:"owed_restore_id"`
	ConfirmationPhase ConfirmationPhase `json:"confirmation_phase"`
}

func (r ReachabilityConfirmationRequest) Validate() error {
	if !lowercaseHex(r.NodeJobID, 64) {
		return fmt.Errorf("node_job_id must contain 64 lowercase hexadecimal characters")
	}
	if err := ValidateOwedRestoreID(r.OwedRestoreID); err != nil {
		return err
	}
	if r.ConfirmationPhase != ConfirmationBeforeChange && r.ConfirmationPhase != ConfirmationAfterChange {
		return fmt.Errorf("confirmation_phase must be before_change or after_change")
	}
	return nil
}

func (r *ReachabilityConfirmationRequest) UnmarshalJSON(b []byte) error {
	type plain ReachabilityConfirmationRequest
	var decoded plain
	if err := Decode(b, &decoded); err != nil {
		return err
	}
	request := ReachabilityConfirmationRequest(decoded)
	if err := request.Validate(); err != nil {
		return err
	}
	*r = request
	return nil
}

// ValidateOwedRestoreID checks the path-safe identity of one owed restore.
// Its creator must still use an exclusive directory creation to detect collisions.
func ValidateOwedRestoreID(id string) error {
	if !lowercaseHex(id, 16) {
		return fmt.Errorf("owed_restore_id must contain 16 lowercase hexadecimal characters")
	}
	return nil
}

func lowercaseHex(text string, length int) bool {
	if len(text) != length {
		return false
	}
	for _, c := range text {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

type ReachabilityConfirmationResponse struct {
	NodeJobID                       string                           `json:"node_job_id"`
	OwedRestoreID                   string                           `json:"owed_restore_id"`
	ConfirmationPhase               ConfirmationPhase                `json:"confirmation_phase"`
	ReceivedConnectionKind          ConnectionKind                   `json:"received_connection_kind"`
	ConfirmationCounted             bool                             `json:"confirmation_counted"`
	ConfirmationReason              string                           `json:"confirmation_reason"`
	PendingReachabilityConfirmation *PendingReachabilityConfirmation `json:"pending_reachability_confirmation,omitempty"`
}

func (r *ReachabilityConfirmationResponse) UnmarshalJSON(b []byte) error {
	type plain ReachabilityConfirmationResponse
	var decoded plain
	if err := Decode(b, &decoded); err != nil {
		return err
	}
	if err := (ReachabilityConfirmationRequest{decoded.NodeJobID, decoded.OwedRestoreID, decoded.ConfirmationPhase}).Validate(); err != nil {
		return err
	}
	if decoded.ReceivedConnectionKind != ConnectionSSH && decoded.ReceivedConnectionKind != ConnectionHTTPS {
		return fmt.Errorf("received_connection_kind must be ssh or https")
	}
	*r = ReachabilityConfirmationResponse(decoded)
	return nil
}

// PendingReachabilityConfirmation describes one phase, even after its deadline
// passes. Times come from the node's clock, never the controller machine's.
// The latest deadline bounds after-change attempts if progress is lost between
// phases; it does not announce that the after-change phase is ready.
type PendingReachabilityConfirmation struct {
	NodeJobID                             string            `json:"node_job_id"`
	OwedRestoreID                         string            `json:"owed_restore_id"`
	ConfirmationPhase                     ConfirmationPhase `json:"confirmation_phase"`
	NodeTime                              time.Time         `json:"node_time"`
	ConfirmationDeadline                  time.Time         `json:"confirmation_deadline"`
	ConnectionKindsToConfirm              []ConnectionKind  `json:"connection_kinds_to_confirm"`
	ConfirmedConnectionKinds              []ConnectionKind  `json:"confirmed_connection_kinds"`
	LatestAfterChangeConfirmationDeadline time.Time         `json:"latest_after_change_confirmation_deadline"`
}

func (p PendingReachabilityConfirmation) Validate() error {
	if err := (ReachabilityConfirmationRequest{p.NodeJobID, p.OwedRestoreID, p.ConfirmationPhase}).Validate(); err != nil {
		return err
	}
	for _, field := range []struct {
		name string
		time time.Time
	}{
		{"node_time", p.NodeTime},
		{"confirmation_deadline", p.ConfirmationDeadline},
		{"latest_after_change_confirmation_deadline", p.LatestAfterChangeConfirmationDeadline},
	} {
		_, offset := field.time.Zone()
		if field.time.IsZero() || offset != 0 {
			return fmt.Errorf("%s must be a nonzero UTC timestamp", field.name)
		}
	}
	if p.LatestAfterChangeConfirmationDeadline.Before(p.ConfirmationDeadline) {
		return fmt.Errorf("latest_after_change_confirmation_deadline must not precede confirmation_deadline")
	}
	if len(p.ConnectionKindsToConfirm) == 0 {
		return fmt.Errorf("connection_kinds_to_confirm must contain at least one connection kind")
	}
	for _, field := range []struct {
		name  string
		kinds []ConnectionKind
	}{
		{"connection_kinds_to_confirm", p.ConnectionKindsToConfirm},
		{"confirmed_connection_kinds", p.ConfirmedConnectionKinds},
	} {
		for i, kind := range field.kinds {
			if kind != ConnectionSSH && kind != ConnectionHTTPS {
				return fmt.Errorf("%s must contain only ssh or https", field.name)
			}
			if slices.Contains(field.kinds[:i], kind) {
				return fmt.Errorf("%s must not repeat a connection kind", field.name)
			}
		}
	}
	for _, kind := range p.ConfirmedConnectionKinds {
		if !slices.Contains(p.ConnectionKindsToConfirm, kind) {
			return fmt.Errorf("confirmed_connection_kinds must be a subset of connection_kinds_to_confirm")
		}
	}
	return nil
}

func (p *PendingReachabilityConfirmation) UnmarshalJSON(b []byte) error {
	type plain PendingReachabilityConfirmation
	var decoded plain
	if err := Decode(b, &decoded); err != nil {
		return err
	}
	pending := PendingReachabilityConfirmation(decoded)
	if err := pending.Validate(); err != nil {
		return err
	}
	*p = pending
	return nil
}

// OwedRestore is the public outcome, also retained outside an interrupted node
// job's partial task-result tree. It contains no file bytes or module restore
// data. Secret travels with it so standalone outcomes suppress diagnostics too.
type OwedRestore struct {
	OwedRestoreID                 string `json:"owed_restore_id"`
	NodeTaskPath                  string `json:"node_task_path"`
	ModuleName                    string `json:"module_name"`
	DestinationFile               string `json:"destination_file,omitempty"`
	ChangeSubject                 string `json:"change_subject,omitempty"`
	RestoreServiceUnit            string `json:"restore_service_unit"`
	RestoreStatus                 string `json:"restore_status"`
	PreviousFileRestored          bool   `json:"previous_file_restored"`
	PreviousConfigActivated       bool   `json:"previous_config_activated"`
	ObsoleteVirtualDevicesRemoved bool   `json:"obsolete_virtual_devices_removed"`
	PreviousMembershipRestored    bool   `json:"previous_membership_restored"`
	OriginalFailureReason         string `json:"original_failure_reason,omitempty"`
	RestoreFailureReason          string `json:"restore_failure_reason,omitempty"`
	Secret                        bool   `json:"secret,omitempty"`
}

func (r OwedRestore) MarshalJSON() ([]byte, error) {
	type plain OwedRestore
	r = r.Safe()
	// A membership outcome never claims file restoration steps. Pointers in
	// this representation retain false for the applicable steps and omit the
	// inapplicable fields entirely, including before restoration starts.
	encoded := struct {
		plain
		PreviousFileRestored          *bool `json:"previous_file_restored,omitempty"`
		PreviousConfigActivated       *bool `json:"previous_config_activated,omitempty"`
		ObsoleteVirtualDevicesRemoved *bool `json:"obsolete_virtual_devices_removed,omitempty"`
		PreviousMembershipRestored    *bool `json:"previous_membership_restored,omitempty"`
	}{plain: plain(r)}
	if r.DestinationFile != "" {
		encoded.PreviousFileRestored = &r.PreviousFileRestored
		encoded.PreviousConfigActivated = &r.PreviousConfigActivated
		encoded.ObsoleteVirtualDevicesRemoved = &r.ObsoleteVirtualDevicesRemoved
	} else {
		encoded.PreviousMembershipRestored = &r.PreviousMembershipRestored
	}
	return json.Marshal(encoded)
}

// ValidateConfirmationConnectionKinds permits omission on jobs with no protected
// change. The change service requires a nonempty list before arming.
func ValidateConfirmationConnectionKinds(kinds []ConnectionKind) error {
	for i, kind := range kinds {
		if kind != ConnectionSSH && kind != ConnectionHTTPS {
			return fmt.Errorf("reachability_confirmation_connection_kinds must contain only ssh or https")
		}
		if slices.Contains(kinds[:i], kind) {
			return fmt.Errorf("reachability_confirmation_connection_kinds must not repeat a connection kind")
		}
	}
	return nil
}
