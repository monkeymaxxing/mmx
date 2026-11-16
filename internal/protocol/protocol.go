package protocol

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"mmx/internal/assets"
	"mmx/internal/facts"
)

func Decode(b []byte, v any) error {
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		return err
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return fmt.Errorf("expected exactly one JSON value")
	}
	return nil
}

// Wire limits, applied alike by HTTPS and SSH. A JSON request (a job's
// instructions) is at most RequestLimit bytes. Every
// JSON answer, including a job's status with its results, is at most
// ResponseLimit bytes. Assets and full differences stream with their declared
// sizes instead.
const (
	RequestLimit  = 16 << 20
	ResponseLimit = 64 << 20
)

var (
	ErrRequestLimit  = fmt.Errorf("node request exceeds %d MiB", RequestLimit>>20)
	ErrResponseLimit = fmt.Errorf("node response exceeds %d MiB", ResponseLimit>>20)
)

// The JSON requests the controller process makes of a node. Each name is both
// the request's HTTPS route, POST /v1/<name>, and its mmx-runner command over
// SSH, which reads the same JSON from standard input. node.Server.Answer
// reads the request, at most RequestLimit bytes on either connection, and
// carries it out. Status, stop, asset uploads, difference downloads, and
// setup are not in this list: they take a node job ID or a stream instead.
const (
	RequestSubmit              = "submit"
	RequestProgress            = "progress"
	RequestConfirmReachability = "confirm-reachability"
	RequestLastSuccessfulRun   = "last-successful-run"
	RequestReadFile            = "read-file"
	RequestKeptFiles           = "kept-files"
	RequestFacts               = "facts"
	RequestRebootRefusal       = "reboot-refusal"
)

// ReadLimited reads all of r, returning tooLarge when r holds more than limit bytes.
func ReadLimited(r io.Reader, limit int, tooLarge error) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, int64(limit)+1))
	if err == nil && len(b) > limit {
		return nil, tooLarge
	}
	return b, err
}

const Version = 1

// TruncationMarker ends captured text cut at the output limit, so values
// built from it show the cut; the Truncated flags record it for display.
const TruncationMarker = "\n[MMX output truncated at 1 MiB]"

type Result struct {
	HTTPStatusCode        int              `json:"http_status_code,omitempty"`
	ResponseBody          string           `json:"response_body,omitempty"`
	ResponseBodyTruncated bool             `json:"response_body_truncated,omitempty"`
	StdoutTruncated       bool             `json:"stdout_truncated,omitempty"`
	StderrTruncated       bool             `json:"stderr_truncated,omitempty"`
	ValueSet              bool             `json:"-"`
	DirectoryChange       *DirectoryChange `json:"directory_change,omitempty"`
	FileChange            *FileChange      `json:"file_change,omitempty"`
	OwedRestore           *OwedRestore     `json:"owed_restore,omitempty"`
	Secret                bool             `json:"secret,omitempty"`
	// SecretBecause says why Secret hides the result, naming the node task
	// that read the secret when it is another one.
	SecretBecause string         `json:"secret_because,omitempty"`
	Outputs       map[string]any `json:"outputs,omitempty"`
	Items         []Result       `json:"items,omitempty"`
	IterationKey  string         `json:"iteration_key,omitempty"`

	// ModuleOutputs are the named outputs of a module that is not core, which
	// later node tasks read beside the other result fields. Diff is such a
	// module's difference, and Fingerprint changes when the state it manages
	// changes. ModuleKind is official_extra, project_starlark, or
	// project_executable.
	ModuleOutputs map[string]any `json:"module_outputs,omitempty"`
	Diff          string         `json:"diff,omitempty"`
	Fingerprint   string         `json:"fingerprint,omitempty"`
	ModuleKind    string         `json:"module_kind,omitempty"`

	CheckNewFile *Result  `json:"check_new_file,omitempty"`
	Tries        []Result `json:"tries,omitempty"`
	FailureKind  string   `json:"failure_kind,omitempty"`
	Task         string   `json:"task"`
	Status       string   `json:"status"`
	Change       string   `json:"change"`
	Reason       string   `json:"reason,omitempty"`
	ExitCode     int      `json:"exit_code,omitempty"`
	Stdout       string   `json:"stdout,omitempty"`
	Stderr       string   `json:"stderr,omitempty"`
	Value        any      `json:"value,omitempty"`
	// PublicKey is an ssh_key_pair task's public key line, or a
	// wireguard_interface task's public key.
	PublicKey string `json:"public_key,omitempty"`
	// Content is what read_file read: text, or value.Binary for a file that
	// is not UTF-8; a *value.Secret when the task said content_is_secret.
	Content any `json:"content,omitempty"`

	// HiddenDetailsFile is the file on the node in which root can read what
	// the hidden result of a failed node task leaves out: its reason, exit
	// code, and output (HidesFailureDetails).
	HiddenDetailsFile string `json:"hidden_details_file,omitempty"`
	// ReasonReadsNoSecret says that the reason holds no secret value, so a
	// secret result keeps it: an assert's failure text that reads none, and
	// the failure of the role call, node task block, loop, or node task that
	// such an assert failed.
	ReasonReadsNoSecret bool `json:"reason_reads_no_secret,omitempty"`
	// OutputHoldsNoSecret keeps command diagnostics visible after the engine
	// replaces the secret values read by the task. Other fields stay secret.
	OutputHoldsNoSecret bool `json:"output_holds_no_secret,omitempty"`

	// What the job's result budget could not hold, stated rather than dropped.
	Omitted      string `json:"omitted,omitempty"`
	TriesOmitted int    `json:"tries_omitted,omitempty"`

	// NoOtherTry says that the module asked for no other try of this
	// failure: the try changed the node, and another try would find that
	// state and report success. Only the node task execution engine reads
	// it, in the node job worker process that ran the module.
	NoOtherTry bool `json:"-"`
}
type Response struct {
	PendingReachabilityConfirmation *PendingReachabilityConfirmation `json:"pending_reachability_confirmation,omitempty"`
	OwedRestores                    []OwedRestore                    `json:"owed_restores,omitempty"`
	Finished                        *time.Time                       `json:"finished,omitempty"`
	Housekeeping                    []string                         `json:"housekeeping,omitempty"`
	Facts                           *facts.Snapshot                  `json:"facts,omitempty"`
	Change                          string                           `json:"change,omitempty"`
	Outputs                         map[string]any                   `json:"outputs,omitempty"`

	StopRequested bool     `json:"stop_requested,omitempty"`
	CurrentTask   string   `json:"current_task,omitempty"`
	ID            string   `json:"id"`
	Status        string   `json:"status"`
	Results       []Result `json:"results"`
	Error         string   `json:"error,omitempty"`
	// ResultsAfter marks a running progress answer that omits the first
	// ResultsAfter results, which the controller already holds.
	ResultsAfter int `json:"results_after,omitempty"`
	// RebootFrom is the boot ID a job's node had when the job requested its
	// reboot; it is set until the node answers on a new boot.
	RebootFrom string `json:"reboot_from,omitempty"`
}

// ProgressRequest follows a running job. It carries what the controller holds,
// so the node answers once that changes and sends only newer results.
type ProgressRequest struct {
	KnownReachabilityConfirmation *PendingReachabilityConfirmation `json:"known_reachability_confirmation,omitempty"`
	ID                            string                           `json:"id"`
	Results                       int                              `json:"results"`
	CurrentTask                   string                           `json:"current_task"`
}

// ControllerMachine tells a node which machine runs the controller process,
// so that the node never reboots that machine. BootID is the kernel's ID of
// the machine's current boot. MachineIDHash is the SHA-256 of its systemd
// machine ID, never the ID itself. A field is empty when the controller
// machine has no such ID: a Mac has neither.
type ControllerMachine struct {
	BootID        string `json:"boot_id"`
	MachineIDHash string `json:"machine_id_hash"`
}

// RebootRefusalRequest asks a node whether it refuses a reboot task because
// it is the machine that runs the controller process. The controller process
// asks before the node job of a fleet stage that ends with a reboot task.
type RebootRefusalRequest struct {
	ControllerMachine ControllerMachine `json:"controller_machine"`
}

// RebootRefusalResponse holds the message with which the node refuses, or
// nothing when the node would reboot.
type RebootRefusalResponse struct {
	Refusal string `json:"refusal"`
}

// LastSuccessfulRun is the one JSON line a node keeps in
// /var/lib/mmx/last_successful_run/<project_name>/<playbook> after a run or
// resume of that playbook in which every fleet stage that selected the node
// succeeded. Limit and OnlyFleetStages hold the run's --limit and
// --only-fleet-stages values, empty when it had none, so a monitoring reader
// can tell a whole run from a partial one.
type LastSuccessfulRun struct {
	RunID           string    `json:"run_id"`
	Inventory       string    `json:"inventory"`
	Limit           string    `json:"limit"`
	OnlyFleetStages string    `json:"only_fleet_stages"`
	FinishedAt      time.Time `json:"finished_at"`
}

// LastSuccessfulRunRequest asks a node to replace its record of a playbook's
// last successful run.
type LastSuccessfulRunRequest struct {
	Project  string            `json:"project_name"`
	Playbook string            `json:"playbook"`
	Record   LastSuccessfulRun `json:"record"`
}

// KeptFilesRequest names, by SHA-256 and size, the files of a job that a node
// keeps between runs, before the controller sends them: the files of modules
// that are not core, and the files from a web address. The node links each
// file it keeps into the job's files.
type KeptFilesRequest struct {
	JobID string       `json:"job_id"`
	Files []assets.Ref `json:"files"`
}

// KeptFilesResponse names, by SHA-256, the files the node does not keep,
// which the controller must send.
type KeptFilesResponse struct {
	FilesToSend []string `json:"files_to_send"`
}
