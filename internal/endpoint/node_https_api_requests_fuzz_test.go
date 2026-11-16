package endpoint

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"mmx/internal/assets"
	"mmx/internal/config"
	"mmx/internal/modules"
	"mmx/internal/node"
	"mmx/internal/protocol"
	"mmx/internal/value"
)

// The kinds of untrusted input that FuzzNodeHTTPSAPIRequest feeds the node
// HTTPS API process. The first nine are the JSON request bodies that
// ServeHTTP and authenticate decode. The last two are URL path text: the job
// id after /v1/status/ and /v1/stop/, and a whole /v1/assets/ or /v1/diffs/
// path. A fuzzed kind number selects one by its remainder, so every mutation
// reaches some kind.
const (
	fuzzedJobSubmissionBody = iota
	fuzzedReachabilityConfirmationBody
	fuzzedProgressBody
	fuzzedLastSuccessfulRunBody
	fuzzedReadFileBody
	fuzzedKeptFilesBody
	fuzzedFactsBody
	fuzzedRebootRefusalBody
	fuzzedSessionProofBody
	fuzzedStatusOrStopJobID
	fuzzedAssetOrDiffURLPath
	fuzzedRequestKindCount
)

// nodeJobIDPattern is a node job id: 64 lowercase hexadecimal characters.
var nodeJobIDPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

// FuzzNodeHTTPSAPIRequest feeds one untrusted request input to the decoder
// and the node method that the node HTTPS API process uses for it, on a node
// with no state in a new temporary directory. Every input reads back whole
// through protocol.ReadLimited within its limit and is refused above it. A
// JSON body that decodes is refused with a second JSON value after it, and,
// except a job, encodes back to an encoding that decodes the same. The empty
// node never starts a node job worker process, never counts a reachability
// confirmation, has no job to report or stop, and keeps no module file. A
// recorded last successful run reads back from its file with mode 0600.
// Methods that would read host files or run host commands are called only
// with input they refuse first.
func FuzzNodeHTTPSAPIRequest(f *testing.F) {
	syntheticJobID := strings.Repeat("a", 64)
	syntheticOwedRestoreID := "0123456789abcdef"
	syntheticFileSHA256 := strings.Repeat("b", 64)
	otherFileSHA256 := strings.Repeat("c", 64)
	syntheticTime := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	encoded := func(request any) []byte {
		b, err := json.Marshal(request)
		if err != nil {
			f.Fatal(err)
		}
		return b
	}
	job := config.Job{Protocol: protocol.Version, ID: syntheticJobID, Project: "web_shop", FleetStage: "configure", Node: "web1",
		Scope:     map[string]any{"secrets": map[string]any{"token": &value.Secret{Name: "token", Data: "synthetic-secret-value", Available: true}}},
		NodeTasks: []config.Task{{ID: "derive", Module: "shell", ModuleInputs: &modules.Shell{Script: `printf '%s' "$TOKEN"`, Env: map[string]any{"TOKEN": "{{ .secrets.token }}"}}}}}
	jobBody, err := job.Transport()
	if err != nil {
		f.Fatal(err)
	}
	knownConfirmation := &protocol.PendingReachabilityConfirmation{NodeJobID: syntheticJobID, OwedRestoreID: syntheticOwedRestoreID, ConfirmationPhase: protocol.ConfirmationBeforeChange,
		NodeTime: syntheticTime, ConfirmationDeadline: syntheticTime.Add(time.Minute), LatestAfterChangeConfirmationDeadline: syntheticTime.Add(3 * time.Minute),
		ConnectionKindsToConfirm: []protocol.ConnectionKind{protocol.ConnectionSSH, protocol.ConnectionHTTPS}, ConfirmedConnectionKinds: []protocol.ConnectionKind{protocol.ConnectionSSH}}
	validJSONBodies := []struct {
		requestKind uint8
		body        []byte
	}{
		{fuzzedJobSubmissionBody, jobBody},
		{fuzzedJobSubmissionBody, []byte(`{"protocol":1,"id":"../escape","project":"web_shop","fleet_stage":"configure","node":"web1","dryrun":false,"node_tasks":[]}`)},
		{fuzzedReachabilityConfirmationBody, encoded(protocol.ReachabilityConfirmationRequest{NodeJobID: syntheticJobID, OwedRestoreID: syntheticOwedRestoreID, ConfirmationPhase: protocol.ConfirmationAfterChange})},
		{fuzzedProgressBody, encoded(protocol.ProgressRequest{ID: syntheticJobID, Results: 1, CurrentTask: "derive", KnownReachabilityConfirmation: knownConfirmation})},
		{fuzzedProgressBody, encoded(protocol.ProgressRequest{ID: "../escape"})},
		{fuzzedLastSuccessfulRunBody, encoded(protocol.LastSuccessfulRunRequest{Project: "web_shop", Playbook: "deploy", Record: protocol.LastSuccessfulRun{RunID: "run-1", Inventory: "production", FinishedAt: syntheticTime}})},
		{fuzzedLastSuccessfulRunBody, []byte(`{"project_name":"web_shop","playbook":"2026-10-16_rotate_keys","record":{"run_id":"run-2","inventory":"production","limit":"web1","only_fleet_stages":"configure","finished_at":"2026-10-16T14:00:00+02:00"}}`)},
		{fuzzedLastSuccessfulRunBody, encoded(protocol.LastSuccessfulRunRequest{Project: "web_shop", Playbook: "../escape"})},
		{fuzzedReadFileBody, encoded(protocol.ReadFileRequest{Path: "etc/hostname"})},
		{fuzzedReadFileBody, encoded(protocol.ReadFileRequest{Path: "/etc/hostname"})},
		{fuzzedKeptFilesBody, encoded(protocol.KeptFilesRequest{JobID: syntheticJobID, Files: []assets.Ref{{SHA256: syntheticFileSHA256, Size: 12}, {SHA256: otherFileSHA256, Size: 0}, {SHA256: syntheticFileSHA256, Size: 12}}})},
		{fuzzedKeptFilesBody, encoded(protocol.KeptFilesRequest{JobID: syntheticJobID, Files: []assets.Ref{{SHA256: "../escape", Size: 12}}})},
		{fuzzedFactsBody, encoded(protocol.FactsRequest{Node: "web1"})},
		{fuzzedFactsBody, encoded(protocol.FactsRequest{Node: "../web1"})},
		{fuzzedRebootRefusalBody, encoded(protocol.RebootRefusalRequest{ControllerMachine: protocol.ControllerMachine{BootID: "0f1e2d3c-4b5a-6978-8796-a5b4c3d2e1f0", MachineIDHash: otherFileSHA256}})},
		{fuzzedSessionProofBody, encoded(sessionProof{User: "admin", Key: []byte("synthetic-public-key"), Signature: []byte("synthetic-signature")})},
	}
	for _, seed := range validJSONBodies {
		f.Add(seed.requestKind, seed.body)
		f.Add(seed.requestKind, append(bytes.Clone(seed.body), " {}"...))
	}
	for requestKind := uint8(0); requestKind <= fuzzedSessionProofBody; requestKind++ {
		f.Add(requestKind, []byte(`{"node_job_id":`))
		f.Add(requestKind, []byte(`{"unknown_field":true}`))
	}
	for _, jobID := range []string{syntheticJobID, strings.ToUpper(syntheticJobID), "../" + syntheticJobID[3:], syntheticJobID + "\n", ""} {
		f.Add(uint8(fuzzedStatusOrStopJobID), []byte(jobID))
	}
	for _, urlPath := range []string{
		"/v1/assets/" + syntheticJobID + "/" + syntheticFileSHA256 + "/12",
		"/v1/diffs/" + syntheticJobID + "/" + syntheticFileSHA256 + "/12",
		"/v1/assets/" + syntheticJobID + "/" + syntheticFileSHA256 + "/-1",
		"/v1/assets/" + syntheticJobID + "/" + syntheticFileSHA256,
		"/v1/diffs/" + syntheticJobID + "/../" + syntheticFileSHA256 + "/12",
	} {
		f.Add(uint8(fuzzedAssetOrDiffURLPath), []byte(urlPath))
	}
	f.Fuzz(func(t *testing.T, requestKind uint8, requestBytes []byte) {
		for _, limit := range []int{protocol.RequestLimit, len(requestBytes) / 2} {
			read, err := protocol.ReadLimited(bytes.NewReader(requestBytes), limit, protocol.ErrRequestLimit)
			if len(requestBytes) <= limit {
				if err != nil || !bytes.Equal(read, requestBytes) {
					t.Fatalf("reading %d bytes within a limit of %d returned %d bytes and error %v", len(requestBytes), limit, len(read), err)
				}
			} else if read != nil || !errors.Is(err, protocol.ErrRequestLimit) {
				t.Fatalf("reading %d bytes over a limit of %d returned %d bytes and error %v", len(requestBytes), limit, len(read), err)
			}
		}
		nodeServer := emptyTemporaryNode(t)
		switch requestKind % fuzzedRequestKindCount {
		case fuzzedJobSubmissionBody:
			// A job is not encoded back: decoding turns the transport records
			// of its secrets into values that encode without their data.
			var job config.Job
			decoded := decodesAsOneRequest(t, requestBytes, &job)
			response, err := nodeServer.Submit(requestBytes, modules.ControllerConnection{})
			if !decoded && err == nil {
				t.Fatalf("submitted a job body that does not decode: %q", requestBytes)
			}
			if err == nil && response.Status == "running" {
				t.Fatalf("a job reports running on a node whose node runner executable does not exist: %+v", response)
			}
		case fuzzedReachabilityConfirmationBody:
			var request protocol.ReachabilityConfirmationRequest
			if decodesAsOneRequest(t, requestBytes, &request) {
				if err := request.Validate(); err != nil {
					t.Fatalf("decoded an invalid reachability confirmation request %+v: %v", request, err)
				}
				requireStableJSONEncoding(t, request)
				answer, err := nodeServer.ConfirmReachability(context.Background(), request, protocol.ConnectionHTTPS)
				if answer.ConfirmationCounted {
					t.Fatalf("a node with no owed restore counted a reachability confirmation: %+v", answer)
				}
				if err == nil && answer.ConfirmationReason != "owed_restore_not_found" {
					t.Fatalf("a node with no owed restore answered %q, want owed_restore_not_found", answer.ConfirmationReason)
				}
			}
		case fuzzedProgressBody:
			var request protocol.ProgressRequest
			if decodesAsOneRequest(t, requestBytes, &request) {
				if known := request.KnownReachabilityConfirmation; known != nil && known.Validate() != nil {
					t.Fatalf("decoded an invalid known reachability confirmation: %+v", *known)
				}
				requireStableJSONEncoding(t, request)
				response, err := nodeServer.Progress(request)
				if !nodeJobIDPattern.MatchString(request.ID) && err == nil {
					t.Fatalf("progress accepted the invalid job id %q", request.ID)
				}
				if nodeJobIDPattern.MatchString(request.ID) && (err != nil || response.Status != "not_found") {
					t.Fatalf("progress of a job on a node with no history answered %q with error %v, want not_found", response.Status, err)
				}
			}
		case fuzzedLastSuccessfulRunBody:
			var request protocol.LastSuccessfulRunRequest
			decoded := decodesAsOneRequest(t, requestBytes, &request)
			if decoded {
				requireStableJSONEncoding(t, request)
			}
			_, err := nodeServer.Answer(context.Background(), protocol.RequestLastSuccessfulRun, bytes.NewReader(requestBytes), modules.ControllerConnection{Kind: protocol.ConnectionHTTPS})
			if !decoded && err == nil {
				t.Fatalf("recorded a last successful run body that does not decode: %q", requestBytes)
			}
			if err == nil {
				requireRecordedLastSuccessfulRun(t, nodeServer.Root, request)
			}
		case fuzzedReadFileBody:
			var request protocol.ReadFileRequest
			if decodesAsOneRequest(t, requestBytes, &request) {
				requireStableJSONEncoding(t, request)
				// An absolute path would read that file of this machine; the
				// node tests cover reading. A relative path is refused first.
				if !filepath.IsAbs(request.Path) {
					if _, err := nodeServer.ReadFile(request); err == nil {
						t.Fatalf("read-file accepted the relative path %q", request.Path)
					}
				}
			}
		case fuzzedKeptFilesBody:
			var request protocol.KeptFilesRequest
			if decodesAsOneRequest(t, requestBytes, &request) {
				requireStableJSONEncoding(t, request)
				response, err := nodeServer.UseKeptFiles(request)
				if err == nil {
					requested := []string{}
					for _, ref := range request.Files {
						if ref.Validate() != nil {
							t.Fatalf("kept files accepted the invalid file %+v", ref)
						}
						requested = append(requested, ref.SHA256)
					}
					if !nodeJobIDPattern.MatchString(request.JobID) {
						t.Fatalf("kept files accepted the invalid job id %q", request.JobID)
					}
					if !slices.Equal(response.FilesToSend, requested) {
						t.Fatalf("a node that keeps no files asked for %q, want every requested file %q", response.FilesToSend, requested)
					}
				}
			}
		case fuzzedFactsBody:
			var request protocol.FactsRequest
			if decodesAsOneRequest(t, requestBytes, &request) {
				requireStableJSONEncoding(t, request)
				// A valid node name would measure the facts of this machine
				// with its commands. An invalid one is refused first.
				if !modules.Name.MatchString(request.Node) {
					if _, err := nodeServer.Facts(request); err == nil {
						t.Fatalf("facts accepted the invalid node name %q", request.Node)
					}
				}
			}
		case fuzzedRebootRefusalBody:
			// RebootRefusal reads this machine's boot ID and machine ID.
			var request protocol.RebootRefusalRequest
			if decodesAsOneRequest(t, requestBytes, &request) {
				requireStableJSONEncoding(t, request)
			}
		case fuzzedSessionProofBody:
			// The TLS and authorization tests in server_test.go check proofs.
			var proof sessionProof
			if decodesAsOneRequest(t, requestBytes, &proof) {
				requireStableJSONEncoding(t, proof)
			}
		case fuzzedStatusOrStopJobID:
			jobID := string(requestBytes)
			validJobID := nodeJobIDPattern.MatchString(jobID)
			status, err := nodeServer.Status(jobID)
			if !validJobID && err == nil {
				t.Fatalf("status accepted the invalid job id %q", jobID)
			}
			if validJobID && (err != nil || status.ID != jobID || status.Status != "not_found" || status.StopRequested) {
				t.Fatalf("status of a job on a node with no history answered %+v with error %v, want not_found", status, err)
			}
			stopped, err := nodeServer.Stop(jobID)
			if !validJobID && err == nil {
				t.Fatalf("stop accepted the invalid job id %q", jobID)
			}
			if err == nil && (stopped.Status != "stopped" || !stopped.StopRequested) {
				t.Fatalf("stop of a job on a node with no history answered %+v, want stopped", stopped)
			}
		case fuzzedAssetOrDiffURLPath:
			urlPath := string(requestBytes)
			// downloadDiff parses a /v1/diffs/ path as the same path below /v1/assets/.
			if strings.HasPrefix(urlPath, "/v1/diffs/") {
				urlPath = strings.Replace(urlPath, "/v1/diffs/", "/v1/assets/", 1)
			}
			jobID, ref, err := parseAssetPath(urlPath)
			if err == nil {
				if ref.Validate() != nil {
					t.Fatalf("parsed the invalid asset %+v from %q", ref, urlPath)
				}
				againJobID, againRef, err := parseAssetPath(assetPath(jobID, ref))
				if err != nil || againJobID != jobID || againRef != ref {
					t.Fatalf("asset path %q parsed as job %q and asset %+v, but its rebuilt path parsed as job %q and asset %+v with error %v", urlPath, jobID, ref, againJobID, againRef, err)
				}
			}
		}
	})
}

// emptyTemporaryNode is a node with no state in a new temporary directory.
// Its node runner executable does not exist, so no node job worker process
// can start. Initialize refuses the node unless the test runs as root; every
// check allows that refusal.
func emptyTemporaryNode(t *testing.T) node.Server {
	t.Helper()
	root := t.TempDir()
	return node.Server{Root: root, RuntimeRoot: filepath.Join(root, "runtime"), Binary: filepath.Join(root, "absent-node-runner-executable")}
}

// decodesAsOneRequest decodes requestBytes into request as the node HTTPS
// API process does and reports whether that succeeded. A body that decodes
// must be refused when a second JSON value follows it.
func decodesAsOneRequest[Request any](t *testing.T, requestBytes []byte, request *Request) bool {
	t.Helper()
	if protocol.Decode(requestBytes, request) != nil {
		return false
	}
	var again Request
	if protocol.Decode(append(bytes.Clone(requestBytes), " {}"...), &again) == nil {
		t.Fatalf("decoded a %T body followed by a second JSON value: %q", again, requestBytes)
	}
	return true
}

// requireStableJSONEncoding encodes a decoded request, decodes the encoding,
// and requires the same encoding again. It compares encodings, not values: a
// time decoded from "+00:00" and one decoded from "Z" are the same instant
// with different time.Location values.
func requireStableJSONEncoding[Request any](t *testing.T, decoded Request) {
	t.Helper()
	encoding, err := json.Marshal(decoded)
	if err != nil {
		t.Fatalf("encoding the decoded %T %+v: %v", decoded, decoded, err)
	}
	var again Request
	if err := protocol.Decode(encoding, &again); err != nil {
		t.Fatalf("decoding the encoded %T %s: %v", decoded, encoding, err)
	}
	if encodingAgain, err := json.Marshal(again); err != nil || !bytes.Equal(encodingAgain, encoding) {
		t.Fatalf("the %T encoding %s decoded and encoded as %s with error %v", decoded, encoding, encodingAgain, err)
	}
}

// requireRecordedLastSuccessfulRun reads the record that a successful
// RecordLastSuccessfulRun wrote below root, once the request's names are
// known to name a file in the project's directory there.
func requireRecordedLastSuccessfulRun(t *testing.T, root string, request protocol.LastSuccessfulRunRequest) {
	t.Helper()
	if !modules.Name.MatchString(request.Project) {
		t.Fatalf("recorded a last successful run for the invalid project name %q", request.Project)
	}
	projectDirectory := filepath.Join(root, "last_successful_run", request.Project)
	recordPath := filepath.Join(projectDirectory, request.Playbook)
	if filepath.Dir(recordPath) != projectDirectory || filepath.Base(recordPath) != request.Playbook {
		t.Fatalf("recorded a last successful run for the playbook name %q, which is not one file name", request.Playbook)
	}
	info, err := os.Stat(recordPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("the last successful run record has mode %v, want 0600", info.Mode().Perm())
	}
	content, err := os.ReadFile(recordPath)
	if err != nil {
		t.Fatal(err)
	}
	var recorded protocol.LastSuccessfulRun
	if err := protocol.Decode(content, &recorded); err != nil {
		t.Fatalf("the last successful run record %q does not decode: %v", content, err)
	}
	// The same instant can carry a different time.Location after decoding.
	want := request.Record
	if !recorded.FinishedAt.Equal(want.FinishedAt) {
		t.Fatalf("recorded the finish time %v, want %v", recorded.FinishedAt, want.FinishedAt)
	}
	recorded.FinishedAt, want.FinishedAt = time.Time{}, time.Time{}
	if recorded != want {
		t.Fatalf("recorded the last successful run %+v, want %+v", recorded, want)
	}
}
