package modules

import (
	"context"
	"encoding/json"
	"time"

	"mmx/internal/protocol"
)

// ChangeWithOwedRestore is one checked change a module hands to the
// node's owed restore service. A nil PreparedFile protects module data alone;
// its compiled activation factory must accept an empty destination.
// The module retains responsibility for Close when PreparedFile is nonnil.
type ChangeWithOwedRestore struct {
	PreparedFile            *PreparedFileChange
	RestoreConfirmationWait time.Duration
	ModuleRestoreData       json.RawMessage
	// Required only without a PreparedFile. These are descriptions and one
	// ordering dependency, never executable text or commands from the record.
	ChangeSubject          string
	RestoreAfterUnit       string
	RestoreSuccessSentence string
}

type OwedRestoreService interface {
	// An empty destination checks the node-wide module-data-only obligation.
	CheckChangeAllowed(ctx context.Context, destinationFile string) error
	ApplyChange(ctx context.Context, change ChangeWithOwedRestore) protocol.Result
}

// ChangeActivation is reconstructed from a module's saved restore data.
// Both activation and restoration run in new processes, without a playbook or
// controller state. Implementations must not execute command text from a record.
type ChangeActivation interface {
	PrepareActivation(ctx context.Context, runtime Runtime, restoringPreviousState bool) error
	Activate(ctx context.Context, runtime Runtime, restoringPreviousState bool) error
	RemoveObsoleteVirtualDevices(ctx context.Context, runtime Runtime, restoringPreviousState bool) error
	CheckActivatedConfiguration(ctx context.Context, runtime Runtime, restoringPreviousState bool) error
}

// ChangeActivationWithSavedFiles is a ChangeActivation that hands the
// saved previous and proposed files to the network tool itself.
// ifupdown needs both: its ifdown takes an interface down only with the
// stanza that brought it up. For such an activation the node also saves a
// copy of the proposed file. Each argument is a path, or "" when that file
// does not exist: no previous file, or a removal.
type ChangeActivationWithSavedFiles interface {
	ChangeActivation
	UseSavedFiles(previousFileContents, proposedFileContents string)
}
