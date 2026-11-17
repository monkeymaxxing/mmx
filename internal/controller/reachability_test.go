package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"mmx/internal/access"
	"mmx/internal/config"
	"mmx/internal/endpoint"
	"mmx/internal/protocol"
)

const confirmationJob = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const confirmationChange = "0123456789abcdef"

func pendingConfirmation(phase protocol.ConfirmationPhase, kinds ...protocol.ConnectionKind) *protocol.PendingReachabilityConfirmation {
	now := time.Now().UTC()
	return &protocol.PendingReachabilityConfirmation{NodeJobID: confirmationJob, OwedRestoreID: confirmationChange, ConfirmationPhase: phase, NodeTime: now,
		ConfirmationDeadline: now.Add(time.Minute), LatestAfterChangeConfirmationDeadline: now.Add(150 * time.Second), ConnectionKindsToConfirm: kinds}
}
func scheduleFor(t *testing.T, pending *protocol.PendingReachabilityConfirmation) (*confirmationSchedule, time.Time) {
	t.Helper()
	now := time.Now()
	s := &confirmationSchedule{jobID: confirmationJob}
	if err := s.observe(pending, now, now); err != nil {
		t.Fatal(err)
	}
	return s, now
}
func counted(attempt confirmationAttempt) confirmationAnswer {
	return confirmationAnswer{attempt: attempt, answer: protocol.ReachabilityConfirmationResponse{NodeJobID: attempt.request.NodeJobID, OwedRestoreID: attempt.request.OwedRestoreID, ConfirmationPhase: attempt.request.ConfirmationPhase, ReceivedConnectionKind: attempt.kind, ConfirmationCounted: true}}
}

func TestConfirmationRepeatsAreSparseAndBounded(t *testing.T) {
	s, now := scheduleFor(t, pendingConfirmation(protocol.ConfirmationBeforeChange, protocol.ConnectionSSH, protocol.ConnectionHTTPS))
	attempts, _ := s.due(now, nil)
	if len(attempts) != 2 {
		t.Fatal(attempts)
	}
	for _, a := range attempts {
		if a.deadline.Sub(a.started) != 10*time.Second {
			t.Fatal(a)
		}
	}
	if attempts, _ := s.due(now.Add(10*time.Second-time.Nanosecond), nil); len(attempts) != 0 {
		t.Fatal("repeated too soon", attempts)
	}
	if attempts, _ := s.due(now.Add(10*time.Second), map[protocol.ConnectionKind]bool{protocol.ConnectionSSH: true}); len(attempts) != 1 || attempts[0].kind != protocol.ConnectionHTTPS {
		t.Fatal("overlapped an in-flight kind", attempts)
	}
	if attempts, _ := s.due(now.Add(59*time.Second), nil); len(attempts) != 2 || attempts[0].deadline.Sub(attempts[0].started) != time.Second {
		t.Fatal("attempt exceeded node deadline", attempts)
	}
	if attempts, _ := s.due(now.Add(time.Minute), nil); len(attempts) != 0 {
		t.Fatal("attempt at expiry", attempts)
	}
}

func TestHealthyBeforeChangeProgressNeverSpeculatesAfterChange(t *testing.T) {
	s, now := scheduleFor(t, pendingConfirmation(protocol.ConfirmationBeforeChange, protocol.ConnectionSSH))
	attempts, _ := s.due(now, nil)
	s.answered(counted(attempts[0]), now)
	for _, later := range []time.Duration{time.Second, 61 * time.Second} {
		if attempts, _ := s.due(now.Add(later), nil); len(attempts) != 0 {
			t.Fatal("speculated with healthy before-change progress", attempts)
		}
	}
	s.progressFailed = true
	attempts, _ = s.due(now.Add(time.Second), nil)
	if len(attempts) != 1 || !attempts[0].askReadiness {
		t.Fatal("lost progress must ask readiness without confirming", attempts)
	}
}

func TestLostBeforeChangeAnswerStillAsksReadinessAtDeadline(t *testing.T) {
	s, now := scheduleFor(t, pendingConfirmation(protocol.ConfirmationBeforeChange, protocol.ConnectionSSH))
	first, _ := s.due(now, nil)
	s.answered(confirmationAnswer{attempt: first[0], err: io.EOF}, now)
	s.progressFailed = true
	before, wake := s.due(now.Add(9*time.Second), nil)
	if len(before) != 0 || !wake.Equal(now.Add(10*time.Second)) {
		t.Fatal(before, wake)
	}
	after, _ := s.due(now.Add(time.Minute), nil)
	if len(after) != 1 || !after[0].askReadiness {
		t.Fatal(after)
	}
	if after[0].request.NodeJobID != first[0].request.NodeJobID || after[0].request.OwedRestoreID != first[0].request.OwedRestoreID {
		t.Fatal("lost request identity")
	}
}

func TestAfterChangeRequirementComesFromNodeAndCannotExpand(t *testing.T) {
	before := pendingConfirmation(protocol.ConfirmationBeforeChange, protocol.ConnectionSSH, protocol.ConnectionHTTPS)
	s, now := scheduleFor(t, before)
	after := *before
	after.ConfirmationPhase = protocol.ConfirmationAfterChange
	after.ConnectionKindsToConfirm = []protocol.ConnectionKind{protocol.ConnectionSSH}
	if err := s.observe(&after, now, now); err != nil {
		t.Fatal(err)
	}
	attempts, _ := s.due(now, nil)
	if len(attempts) != 1 || attempts[0].kind != protocol.ConnectionSSH {
		t.Fatal(attempts)
	}
	// A late before-change HTTPS answer cannot widen the frozen requirement.
	s.answered(counted(confirmationAttempt{request: protocol.ReachabilityConfirmationRequest{NodeJobID: confirmationJob, OwedRestoreID: confirmationChange, ConfirmationPhase: protocol.ConfirmationBeforeChange}, kind: protocol.ConnectionHTTPS}), now)
	if !reflect.DeepEqual(s.afterKinds, []protocol.ConnectionKind{protocol.ConnectionSSH}) {
		t.Fatal(s.afterKinds)
	}
	after.ConnectionKindsToConfirm = []protocol.ConnectionKind{protocol.ConnectionSSH, protocol.ConnectionHTTPS}
	if err := s.observe(&after, now, now); err == nil {
		t.Fatal("accepted expanded requirement")
	}
}

func TestConfirmationDeadlinesAccountForSkewAndOnlyTighten(t *testing.T) {
	p := pendingConfirmation(protocol.ConfirmationBeforeChange, protocol.ConnectionSSH)
	p.NodeTime = p.NodeTime.Add(24 * time.Hour)
	p.ConfirmationDeadline = p.NodeTime.Add(40 * time.Second)
	p.LatestAfterChangeConfirmationDeadline = p.NodeTime.Add(100 * time.Second)
	now := time.Now()
	s := &confirmationSchedule{jobID: confirmationJob}
	if err := s.observe(p, now, now.Add(4*time.Second)); err != nil {
		t.Fatal(err)
	}
	if !s.beforeDeadline.Equal(now.Add(40*time.Second)) || !s.afterDeadline.Equal(now.Add(100*time.Second)) {
		t.Fatal(s)
	}
	p.ConfirmationPhase = protocol.ConfirmationAfterChange
	p.NodeTime = p.NodeTime.Add(10 * time.Second)
	p.ConfirmationDeadline = p.NodeTime.Add(12 * time.Second)
	if err := s.observe(p, now.Add(6*time.Second), now.Add(9*time.Second)); err != nil {
		t.Fatal(err)
	}
	if !s.afterDeadline.Equal(now.Add(18*time.Second)) || !s.collectionDeadline.Equal(now.Add(21*time.Second+restoreCollectionAllowance)) {
		t.Fatal(s)
	}
	// A node clock setback or a delayed older response never extends the bound.
	p.NodeTime = p.NodeTime.Add(-time.Hour)
	if err := s.observe(p, now.Add(7*time.Second), now.Add(10*time.Second)); err != nil {
		t.Fatal(err)
	}
	if !s.afterDeadline.Equal(now.Add(18 * time.Second)) {
		t.Fatal(s.afterDeadline)
	}
}

func TestConfirmationRejectsOtherChangePhaseAndKindAnswers(t *testing.T) {
	for _, wrong := range []string{"job", "change", "phase", "kind"} {
		t.Run(wrong, func(t *testing.T) {
			s, now := scheduleFor(t, pendingConfirmation(protocol.ConfirmationBeforeChange, protocol.ConnectionSSH))
			attempts, _ := s.due(now, nil)
			answer := counted(attempts[0])
			switch wrong {
			case "job":
				answer.answer.NodeJobID = strings.Repeat("b", 64)
			case "change":
				answer.answer.OwedRestoreID = "fedcba9876543210"
			case "phase":
				answer.answer.ConfirmationPhase = protocol.ConfirmationAfterChange
			case "kind":
				answer.answer.ReceivedConnectionKind = protocol.ConnectionHTTPS
			}
			s.answered(answer, now)
			if s.beforeCompleted || s.phases[protocol.ConfirmationBeforeChange].confirmed[protocol.ConnectionSSH] {
				t.Fatal("counted mismatched answer")
			}
		})
	}
}

func TestSequentialChangesDoNotShareConfirmations(t *testing.T) {
	p := pendingConfirmation(protocol.ConfirmationBeforeChange, protocol.ConnectionSSH)
	s, now := scheduleFor(t, p)
	first, _ := s.due(now, nil)
	s.answered(counted(first[0]), now)
	p.OwedRestoreID = "fedcba9876543210"
	if err := s.observe(p, now.Add(time.Second), now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	s.answered(counted(first[0]), now.Add(time.Second))
	if s.beforeCompleted {
		t.Fatal("old acknowledgement completed new change")
	}
	second, _ := s.due(now.Add(time.Second), nil)
	if len(second) != 1 || second[0].request.OwedRestoreID != p.OwedRestoreID {
		t.Fatal(second)
	}
}

func TestConfirmationTrustRejectionStopsOnlyThatKind(t *testing.T) {
	for _, failure := range []error{&endpoint.IdentityError{Reason: "wrong pin"}, &endpoint.HTTPError{Status: 403, Reason: "not authorized"}, &sshError{message: "host key changed"}} {
		s, now := scheduleFor(t, pendingConfirmation(protocol.ConfirmationBeforeChange, protocol.ConnectionSSH, protocol.ConnectionHTTPS))
		attempts, _ := s.due(now, nil)
		s.answered(confirmationAnswer{attempt: attempts[0], err: failure}, now)
		s.answered(confirmationAnswer{attempt: attempts[1], err: io.EOF}, now)
		repeated, _ := s.due(now.Add(10*time.Second), nil)
		if len(repeated) != 1 || repeated[0].kind != protocol.ConnectionHTTPS {
			t.Fatal(failure, repeated)
		}
	}
}

func TestFreshSSHOptionsOverrideConfigurationAndBoundConnectTimeout(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "ssh_config")
	if err := os.WriteFile(configPath, []byte("Host *\n ControlMaster yes\n ControlPath /tmp/forbidden\n ControlPersist 600\n"), 0600); err != nil {
		t.Fatal(err)
	}
	s := &sshSession{Node: config.Node{Address: "fixture", SSHConfig: configPath, User: "admin", Port: 2222, IdentityFile: "/test/key", KnownHosts: "/test/known_hosts"}, ControlPath: "/tmp/ordinary"}
	var options []string
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 4500*time.Millisecond)
		defer cancel()
		options = s.requestOptions(ctx, true)
	})
	for _, option := range []string{"ControlMaster=no", "ControlPath=none", "ControlPersist=no", "ConnectTimeout=5", "StrictHostKeyChecking=yes", "ForwardAgent=no", "KbdInteractiveAuthentication=no", "NumberOfPasswordPrompts=1", "UserKnownHostsFile=/test/known_hosts"} {
		if !slices.Contains(options, option) {
			t.Fatal(option, options)
		}
	}
	if strings.Count(strings.Join(options, " "), "ConnectTimeout=") != 1 || strings.Contains(strings.Join(options, " "), "/tmp/ordinary") {
		t.Fatal(options)
	}
	args := append([]string{"-G"}, options...)
	args = append(args, "--", "fixture")
	out, err := exec.Command("ssh", args...).Output() // configuration only, no connection
	if err != nil {
		t.Fatal(err)
	}
	for _, setting := range []string{"controlmaster false", "controlpersist no"} {
		if !strings.Contains(string(out), setting) {
			t.Fatal(setting, string(out))
		}
	}
	if strings.Contains(string(out), "/tmp/forbidden") || strings.Contains(string(out), "/tmp/ordinary") {
		t.Fatal(string(out))
	}
}

func TestFreshSSHConfirmationRetainsSudoAndRequestShape(t *testing.T) {
	dir := t.TempDir()
	inputPath, argsPath := filepath.Join(dir, "input"), filepath.Join(dir, "args")
	answer := protocol.ReachabilityConfirmationResponse{NodeJobID: confirmationJob, OwedRestoreID: confirmationChange, ConfirmationPhase: protocol.ConfirmationBeforeChange, ReceivedConnectionKind: protocol.ConnectionSSH, ConfirmationCounted: true}
	raw, _ := json.Marshal(answer)
	fakeSSH(t, `printf '%s\n' "$@" > `+quote(argsPath)+`
cat > `+quote(inputPath)+`
printf '%s\n' `+quote(string(raw)))
	s := &sshSession{Node: config.Node{Address: "fixture"}, NodeID: strings.Repeat("a", 64), Arch: "amd64", Runner: "/opt/mmx/bin/build/mmx-runner", sudo: true, sudoPassword: "sudo-secret", Credentials: &access.Credentials{}}
	conn := &Connection{prepareConfirmationSSH: func(context.Context) (*sshSession, error) { return s, nil }}
	for _, phase := range []protocol.ConfirmationPhase{protocol.ConfirmationBeforeChange, protocol.ConfirmationAfterChange} {
		// The fake response's phase need not match on the second request; inspect
		// the sent request even when validation rejects that stale response.
		_, err := conn.ConfirmReachability(context.Background(), protocol.ReachabilityConfirmationRequest{NodeJobID: confirmationJob, OwedRestoreID: confirmationChange, ConfirmationPhase: phase}, protocol.ConnectionSSH)
		if phase == protocol.ConfirmationBeforeChange && err != nil {
			t.Fatal(err)
		}
		if phase == protocol.ConfirmationAfterChange && (err == nil || !strings.Contains(err.Error(), "does not match")) {
			t.Fatal("accepted a response for the previous phase", err)
		}
		body, err := os.ReadFile(inputPath)
		if err != nil {
			t.Fatal(err)
		}
		password, raw, ok := bytes.Cut(body, []byte("\n"))
		if !ok || string(password) != "sudo-secret" {
			t.Fatal(string(body))
		}
		var request protocol.ReachabilityConfirmationRequest
		if err := json.Unmarshal(raw, &request); err != nil || request.ConfirmationPhase != phase {
			t.Fatal(request, err)
		}
		args, err := os.ReadFile(argsPath)
		if err != nil {
			t.Fatal(err)
		}
		for _, text := range []string{"ControlMaster=no", "ControlPath=none", "ControlPersist=no", "sudo -k -S -p", "confirm-reachability", "node identity changed before reachability confirmation"} {
			if !strings.Contains(string(args), text) {
				t.Fatal(text, string(args))
			}
		}
		if strings.Contains(string(args), "sudo-secret") {
			t.Fatal("password entered command arguments")
		}
	}
}

func TestHTTPSConfirmationIgnoresOrdinarySSHDecisionAndNeverFallsBack(t *testing.T) {
	for _, failure := range []error{io.EOF, &endpoint.IdentityError{Reason: "wrong pin"}, &endpoint.HTTPError{Status: 403, Reason: "not authorized"}} {
		created := 0
		conn := &Connection{Transport: "ssh", FallbackReason: "HTTPS unavailable", fallback: func(context.Context) (*sshSession, error) {
			t.Fatal("confirmation used installation fallback")
			return nil, nil
		}, prepareConfirmationSSH: func(context.Context) (*sshSession, error) { t.Fatal("HTTPS attempt used SSH"); return nil, nil }}
		conn.newConfirmationHTTPS = func() *endpoint.Client {
			created++
			return fakeEndpoint(t, func(*http.Request) (*http.Response, error) { return nil, failure })
		}
		_, err := conn.ConfirmReachability(context.Background(), protocol.ReachabilityConfirmationRequest{NodeJobID: confirmationJob, OwedRestoreID: confirmationChange, ConfirmationPhase: protocol.ConfirmationAfterChange}, protocol.ConnectionHTTPS)
		if err == nil || created != 1 || conn.Transport != "ssh" || conn.FallbackReason != "HTTPS unavailable" {
			t.Fatal(err, created, conn)
		}
	}
}

func TestSSHOnlyJobAdvertisesNoHTTPSAndCannotScheduleIt(t *testing.T) {
	c, r := fleet(t, config.FleetStage{ID: "setup", Nodes: "a"})
	_ = c
	h := r.Inventory.Nodes["a"]
	h.Transport = "ssh_only"
	r.Inventory.Nodes["a"] = h
	job := mustJob(nodeJob(r, config.FleetStage{ID: "setup"}, Entry{Node: "a", JobID: confirmationJob}))
	if !reflect.DeepEqual(job.ReachabilityConfirmationConnectionKinds, []protocol.ConnectionKind{protocol.ConnectionSSH}) {
		t.Fatal(job)
	}
	s := confirmationSchedule{jobID: confirmationJob, sshOnly: true}
	p := pendingConfirmation(protocol.ConfirmationBeforeChange, protocol.ConnectionSSH, protocol.ConnectionHTTPS)
	if err := s.observe(p, time.Now(), time.Now()); err == nil {
		t.Fatal("ssh_only accepted HTTPS")
	}
}

func TestFollowerSerializesKindsAndCancelsAnOldChange(t *testing.T) {
	started := make(chan confirmationAttempt, 4)
	var active atomic.Int32
	var overlap atomic.Bool
	confirm := func(ctx context.Context, r protocol.ReachabilityConfirmationRequest, k protocol.ConnectionKind) (protocol.ReachabilityConfirmationResponse, error) {
		if active.Add(1) != 1 {
			overlap.Store(true)
		}
		defer active.Add(-1)
		started <- confirmationAttempt{request: r, kind: k}
		<-ctx.Done()
		return protocol.ReachabilityConfirmationResponse{}, ctx.Err()
	}
	f := followConfirmations(context.Background(), nil, confirmationJob, true, confirm, nil, nil)
	defer f.close()
	p := pendingConfirmation(protocol.ConfirmationBeforeChange, protocol.ConnectionSSH)
	if window := f.observe(&protocol.Response{Status: "running", PendingReachabilityConfirmation: p}, time.Now(), false); window.err != nil {
		t.Fatal(window.err)
	}
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("first attempt missing")
	}
	p2 := *p
	p2.OwedRestoreID = "fedcba9876543210"
	f.observe(&protocol.Response{Status: "running", PendingReachabilityConfirmation: &p2}, time.Now(), false)
	select {
	case next := <-started:
		if next.request.OwedRestoreID != p2.OwedRestoreID {
			t.Fatal(next)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("new change did not replace canceled request")
	}
	if overlap.Load() {
		t.Fatal("overlapping SSH attempts")
	}
}

func TestFinishedChangeDoesNotBoundLaterOrdinaryTasks(t *testing.T) {
	f := followConfirmations(context.Background(), nil, confirmationJob, true, func(ctx context.Context, _ protocol.ReachabilityConfirmationRequest, _ protocol.ConnectionKind) (protocol.ReachabilityConfirmationResponse, error) {
		<-ctx.Done()
		return protocol.ReachabilityConfirmationResponse{}, ctx.Err()
	}, nil, nil)
	defer f.close()
	p := pendingConfirmation(protocol.ConfirmationAfterChange, protocol.ConnectionSSH)
	f.observe(&protocol.Response{Status: "running", PendingReachabilityConfirmation: p}, time.Now(), false)
	outcome := protocol.OwedRestore{OwedRestoreID: confirmationChange, RestoreStatus: "not_needed"}
	window := f.observe(&protocol.Response{Status: "running", Results: []protocol.Result{{Items: []protocol.Result{{OwedRestore: &outcome}}}}}, time.Now(), false)
	if !window.deadline.IsZero() || window.owedRestoreID != "" {
		t.Fatal("accepted file still bounds unrelated work", window)
	}
}

func TestWaitEntryKeepsConfirmingAfterProgressExhaustsRetries(t *testing.T) {
	previousPoll := reachabilityProgressPoll
	reachabilityProgressPoll = 10 * time.Millisecond
	defer func() { reachabilityProgressPoll = previousPoll }()
	for _, lostBefore := range []bool{false, true} {
		t.Run(fmt.Sprint(lostBefore), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				dir := t.TempDir()
				trace, sshAfter, httpsAfter := filepath.Join(dir, "progress"), filepath.Join(dir, "ssh-after"), filepath.Join(dir, "https-after")
				sshBefore := filepath.Join(dir, "ssh-before")
				p := pendingConfirmation(protocol.ConfirmationBeforeChange, protocol.ConnectionSSH, protocol.ConnectionHTTPS)
				if lostBefore {
					p.ConfirmationDeadline = p.NodeTime.Add(time.Second)
				}
				before := protocol.ReachabilityConfirmationResponse{NodeJobID: confirmationJob, OwedRestoreID: confirmationChange, ConfirmationPhase: protocol.ConfirmationBeforeChange, ReceivedConnectionKind: protocol.ConnectionSSH, ConfirmationCounted: true, PendingReachabilityConfirmation: p}
				after := before
				after.ConfirmationPhase = protocol.ConfirmationAfterChange
				after.PendingReachabilityConfirmation = nil
				beforeJSON, _ := json.Marshal(before)
				afterJSON, _ := json.Marshal(after)
				ready := *p
				ready.ConfirmationPhase = protocol.ConfirmationAfterChange
				ready.ConfirmationDeadline = ready.LatestAfterChangeConfirmationDeadline
				statusJSON, _ := json.Marshal(protocol.Response{ID: confirmationJob, Status: "running", PendingReachabilityConfirmation: &ready})
				finished := protocol.Response{ID: confirmationJob, Status: "succeeded", Results: []protocol.Result{{Status: "succeeded", OwedRestore: &protocol.OwedRestore{OwedRestoreID: confirmationChange, RestoreStatus: "not_needed"}}}}
				finishedJSON, _ := json.Marshal(finished)
				beforeExit := "0"
				if lostBefore {
					beforeExit = "255"
				}
				fakeSSH(t, `for command; do :; done
case "$command" in
*confirm-reachability*) body=$(cat); case "$body" in
*before_change*) touch `+quote(sshBefore)+`; printf '%s\n' `+quote(string(beforeJSON))+`; exit `+beforeExit+`;;
*after_change*) touch `+quote(sshAfter)+`; printf '%s\n' `+quote(string(afterJSON))+`;; esac;;
*status*) printf '%s\n' `+quote(string(statusJSON))+`;;
*progress*) echo progress >> `+quote(trace)+`
if test -f `+quote(sshAfter)+` && test -f `+quote(httpsAfter)+`; then printf '%s\n' `+quote(string(finishedJSON))+`; else echo 'connection lost' >&2; exit 255; fi;;
*) echo 'unexpected command' >&2; exit 1;; esac`)
				session := &sshSession{Node: config.Node{Address: "fixture"}, Arch: "amd64", NodeID: strings.Repeat("a", 64), Runner: "/runner", Credentials: &access.Credentials{}}
				var progressCalls atomic.Int32
				var minimumAttempts atomic.Int32
				var mu sync.Mutex
				var sent []protocol.ReachabilityConfirmationRequest
				conn := &Connection{Transport: "https", fallback: func(context.Context) (*sshSession, error) { return session, nil }, prepareConfirmationSSH: func(context.Context) (*sshSession, error) { return session, nil }}
				conn.direct = fakeEndpoint(t, func(*http.Request) (*http.Response, error) { progressCalls.Add(1); return nil, io.EOF })
				conn.newConfirmationHTTPS = func() *endpoint.Client {
					return fakeEndpoint(t, func(request *http.Request) (*http.Response, error) {
						if request.URL.Path == "/v1/status/"+confirmationJob {
							return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(statusJSON)), Header: make(http.Header)}, nil
						}
						var body protocol.ReachabilityConfirmationRequest
						if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
							return nil, err
						}
						mu.Lock()
						sent = append(sent, body)
						mu.Unlock()
						answer := before
						answer.ReceivedConnectionKind = protocol.ConnectionHTTPS
						if body.ConfirmationPhase == protocol.ConfirmationBeforeChange && lostBefore {
							return nil, io.EOF
						}
						if body.ConfirmationPhase == protocol.ConfirmationAfterChange {
							minimumAttempts.Store(progressCalls.Load() + int32(len(lines(t, trace))))
							if err := os.WriteFile(httpsAfter, nil, 0600); err != nil {
								return nil, err
							}
							answer = after
							answer.ReceivedConnectionKind = protocol.ConnectionHTTPS
						}
						b, _ := json.Marshal(answer)
						return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(b)), Header: make(http.Header)}, nil
					})
				}
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
				defer cancel()
				en := Entry{Node: "a", JobID: confirmationJob, Response: protocol.Response{ID: confirmationJob, Status: "running", PendingReachabilityConfirmation: p}}
				var got Entry
				var err error
				done := make(chan struct{})
				go func() {
					defer close(done)
					got, err = (Controller{State: t.TempDir()}).waitEntry(ctx, conn, &Run{ID: "example-run"}, config.FleetStage{}, en, func(Entry) error { return nil })
				}()
				// Freeze time until both connection kinds have made their before-change
				// attempts. Then let the lost-answer case's before-change window expire.
				synctest.Wait()
				if _, err := os.Stat(sshBefore); err != nil {
					t.Fatal("SSH before-change attempt missing", err)
				}
				mu.Lock()
				beforeObserved := len(sent) > 0 && sent[0].ConfirmationPhase == protocol.ConfirmationBeforeChange
				mu.Unlock()
				if !beforeObserved {
					t.Fatal("HTTPS before-change attempt missing")
				}
				time.Sleep(time.Second)
				<-done
				if err != nil || got.Response.Status != "succeeded" {
					t.Fatal(got.Response, err)
				}
				if minimumAttempts.Load() < 3 {
					t.Fatal("did not exhaust the three progress attempts", minimumAttempts.Load())
				}
				mu.Lock()
				defer mu.Unlock()
				afterObserved := false
				for _, request := range sent {
					if request.ConfirmationPhase == protocol.ConfirmationAfterChange {
						afterObserved = true
					} else if afterObserved {
						t.Fatal("returned to before-change confirmation", sent)
					}
				}
				if !afterObserved {
					t.Fatal("HTTPS after-change attempt missing", sent)
				}
				if got.Transport != "ssh_fallback" {
					t.Fatal("ordinary fallback was not retained", got.Transport)
				}
			})
		})
	}
}

func TestStopCancelsConfirmationsWhileCollectingRestoreResult(t *testing.T) {
	p := pendingConfirmation(protocol.ConfirmationAfterChange, protocol.ConnectionHTTPS)
	started, canceled := make(chan struct{}), make(chan struct{})
	stopped := make(chan struct{})
	var requests atomic.Int32
	conn := &Connection{Transport: "https"}
	conn.newConfirmationHTTPS = func() *endpoint.Client {
		return fakeEndpoint(t, func(request *http.Request) (*http.Response, error) {
			requests.Add(1)
			close(started)
			<-request.Context().Done()
			close(canceled)
			return nil, request.Context().Err()
		})
	}
	stopSeen := false
	conn.direct = fakeEndpoint(t, func(request *http.Request) (*http.Response, error) {
		response := protocol.Response{ID: confirmationJob, Status: "running"}
		switch request.URL.Path {
		case "/v1/progress":
			if !stopSeen {
				<-request.Context().Done()
				return nil, request.Context().Err()
			}
			response.Status = "failed"
			response.OwedRestores = []protocol.OwedRestore{{OwedRestoreID: confirmationChange, RestoreStatus: "succeeded", PreviousFileRestored: true, PreviousConfigActivated: true}}
		case "/v1/stop/" + confirmationJob:
			stopSeen = true
			response.StopRequested = true
		default:
			t.Errorf("unexpected request %s", request.URL.Path)
		}
		b, _ := json.Marshal(response)
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(b)), Header: make(http.Header)}, nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	go func() {
		select {
		case <-started:
			close(stopped)
		case <-ctx.Done():
		}
	}()
	en := Entry{Node: "a", JobID: confirmationJob, Response: protocol.Response{ID: confirmationJob, Status: "running", PendingReachabilityConfirmation: p}}
	got, err := (Controller{State: t.TempDir()}).waitEntry(ctx, conn, &Run{ID: "example-run", stopped: stopped}, config.FleetStage{}, en, func(Entry) error { return nil })
	if err != nil || got.Response.Status != "failed" || len(got.Response.OwedRestores) != 1 || got.Response.OwedRestores[0].RestoreStatus != "succeeded" {
		t.Fatal(got.Response, err)
	}
	select {
	case <-canceled:
	default:
		t.Fatal("confirmation remained running after stop")
	}
	if requests.Load() != 1 || !stopSeen {
		t.Fatal(requests.Load(), stopSeen)
	}
}

func TestInspectionRefreshesFailedRestoreWithoutConfirmingOrChangingJobFailure(t *testing.T) {
	for _, unreachable := range []bool{false, true} {
		t.Run(fmt.Sprint(unreachable), func(t *testing.T) {
			finished := time.Now().UTC().Truncate(time.Second)
			response := protocol.Response{ID: confirmationJob, Status: "failed", Error: "worker interrupted", Finished: &finished, OwedRestores: []protocol.OwedRestore{{OwedRestoreID: confirmationChange, RestoreStatus: "succeeded", PreviousFileRestored: true, PreviousConfigActivated: true}}}
			encoded, _ := json.Marshal(response)
			script := `case "$command" in *status*) printf '%s\n' ` + quote(string(encoded)) + `;; *) echo 'inspection attempted a non-status request' >&2; exit 1;; esac`
			if unreachable {
				script = "echo 'node unavailable' >&2; exit 255"
			}
			c, r := sshNode(t, script)
			old := response
			old.OwedRestores = []protocol.OwedRestore{{OwedRestoreID: confirmationChange, RestoreStatus: "failed", RestoreFailureReason: "previous activation failed"}}
			r.Entries = []Entry{{Node: "a", FleetStage: "setup", JobID: confirmationJob, NodeJobAttempt: 1, Response: old}}
			if err := c.saveEntry(r.ID, r.Entries[0]); err != nil {
				t.Fatal(err)
			}
			err := c.Inspect(context.Background(), r, false)
			if (err != nil) != unreachable {
				t.Fatal(err)
			}
			got := r.Entries[0].Response
			if got.Status != "failed" || got.Error != "worker interrupted" || !got.Finished.Equal(finished) {
				t.Fatal(got)
			}
			want := "succeeded"
			if unreachable {
				want = "failed"
				if !strings.Contains(err.Error(), "cannot refresh owed restore outcomes") {
					t.Fatal(err)
				}
			}
			if got.OwedRestores[0].RestoreStatus != want {
				t.Fatal(got.OwedRestores)
			}
		})
	}
}

func TestActiveResumeConfirmsExistingJobWithoutSubmittingAnother(t *testing.T) {
	dir := t.TempDir()
	beforePath, afterPath, trace := filepath.Join(dir, "before"), filepath.Join(dir, "after"), filepath.Join(dir, "commands")
	p := pendingConfirmation(protocol.ConfirmationBeforeChange, protocol.ConnectionSSH)
	before := protocol.Response{ID: confirmationJob, Status: "running", PendingReachabilityConfirmation: p}
	p2 := *p
	p2.ConfirmationPhase = protocol.ConfirmationAfterChange
	after := before
	after.PendingReachabilityConfirmation = &p2
	finished := protocol.Response{ID: confirmationJob, Status: "succeeded", Results: []protocol.Result{{Status: "succeeded", OwedRestore: &protocol.OwedRestore{OwedRestoreID: confirmationChange, RestoreStatus: "not_needed"}}}}
	b, _ := json.Marshal(before)
	a, _ := json.Marshal(after)
	end, _ := json.Marshal(finished)
	confirmation := protocol.ReachabilityConfirmationResponse{NodeJobID: confirmationJob, OwedRestoreID: confirmationChange, ConfirmationPhase: protocol.ConfirmationBeforeChange, ReceivedConnectionKind: protocol.ConnectionSSH, ConfirmationCounted: true}
	cb, _ := json.Marshal(confirmation)
	confirmation.ConfirmationPhase = protocol.ConfirmationAfterChange
	ca, _ := json.Marshal(confirmation)
	c, r := sshNode(t, `printf '%s\n' "$command" >> `+quote(trace)+`
case "$command" in
*confirm-reachability*) body=$(cat); case "$body" in
*before_change*) touch `+quote(beforePath)+`;printf '%s\n' `+quote(string(cb))+`;;
*after_change*) touch `+quote(afterPath)+`;printf '%s\n' `+quote(string(ca))+`;;esac;;
*status*|*progress*) if test -f `+quote(afterPath)+`;then printf '%s\n' `+quote(string(end))+`;elif test -f `+quote(beforePath)+`;then printf '%s\n' `+quote(string(a))+`;else printf '%s\n' `+quote(string(b))+`;fi;;
*) echo 'unexpected request' >&2;exit 1;;esac`)
	en := Entry{Node: "a", FleetStage: "setup", JobID: confirmationJob, NodeJobAttempt: 1, Response: protocol.Response{ID: confirmationJob, Status: "running"}}
	// Only a guard against a hang: each request starts a shell, which a
	// loaded machine can make slow.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	got, err := c.executeEntry(ctx, r, config.FleetStage{ID: "setup"}, en, true, func(Entry) error { return nil })
	if err != nil || got.Response.Status != "succeeded" || got.JobID != en.JobID || got.NodeJobAttempt != 1 {
		t.Fatal(got.Response, err)
	}
	commands, err := os.ReadFile(trace)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(commands), "confirm-reachability") != 2 || strings.Contains(string(commands), "submit") {
		t.Fatal(string(commands))
	}
}

func TestConfirmationSSHPreparationIsReadOnlyAndRetained(t *testing.T) {
	f := newConnectFixture(t, false, false, currentInfo)
	f.save(t, f.info)
	conn, err := f.connect(t)
	if err != nil {
		t.Fatal(err)
	}
	if conn.native != nil || lines(t, f.ssh) != nil {
		t.Fatal("fixture did not start on cached HTTPS")
	}
	first, err := conn.prepareConfirmationSSH(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	second, err := conn.prepareConfirmationSSH(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if first != second || len(lines(t, f.ssh)) != 1 || len(lines(t, f.requests)) != 0 || first.Runner != runnerDirectory+"/"+conn.Hash+"/mmx-runner" {
		t.Fatal("preparation installed, set up, or reopened SSH", lines(t, f.ssh), lines(t, f.requests))
	}
}

func TestSSHOnlyConnectionConstructsNoHTTPSConfirmationClient(t *testing.T) {
	c, r := sshNode(t, "echo unexpected >&2; exit 1")
	conn, err := c.connect(context.Background(), r, "a", "", machine{})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if conn.direct != nil || conn.newConfirmationHTTPS != nil {
		t.Fatal("SSH-only node constructed HTTPS")
	}
}

func TestLostRestoreResultCollectionHasAFiniteBound(t *testing.T) {
	for _, phase := range []protocol.ConfirmationPhase{protocol.ConfirmationBeforeChange, protocol.ConfirmationAfterChange} {
		t.Run(string(phase), func(t *testing.T) {
			p := pendingConfirmation(phase, protocol.ConnectionHTTPS)
			p.ConfirmationDeadline = p.NodeTime.Add(-restoreCollectionAllowance - 2*time.Second)
			p.LatestAfterChangeConfirmationDeadline = p.ConfirmationDeadline.Add(time.Second)
			conn := &Connection{Transport: "https", direct: fakeEndpoint(t, func(request *http.Request) (*http.Response, error) {
				<-request.Context().Done()
				return nil, request.Context().Err()
			}), newConfirmationHTTPS: func() *endpoint.Client { t.Fatal("opened a connection past the known deadline"); return nil }}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			en := Entry{Node: "a", JobID: confirmationJob, Response: protocol.Response{ID: confirmationJob, Status: "running", PendingReachabilityConfirmation: p}}
			got, err := (Controller{State: t.TempDir()}).waitEntry(ctx, conn, &Run{ID: "example-run"}, config.FleetStage{}, en, func(Entry) error { return nil })
			if err == nil || !strings.Contains(err.Error(), "restore completion is unconfirmed") || !strings.Contains(err.Error(), "mmx status example-run") || got.Response.Status == "succeeded" || ctx.Err() != nil {
				t.Fatal(got.Response, err, ctx.Err())
			}
		})
	}
}

func TestTimedOutSSHConfirmationCanTryAgain(t *testing.T) {
	for _, during := range []string{"request", "preparation"} {
		t.Run(during, func(t *testing.T) {
			fakeSSH(t, `case " $* " in *" -O exit "*) exit 0;; esac
exec sleep 10`)
			session := &sshSession{Node: config.Node{Address: "fixture"}, NodeID: strings.Repeat("a", 64), Arch: "amd64", Runner: "/runner", Credentials: &access.Credentials{}}
			conn := &Connection{prepareConfirmationSSH: func(context.Context) (*sshSession, error) { return session, nil }}
			if during == "preparation" {
				conn.prepareConfirmationSSH = func(ctx context.Context) (*sshSession, error) {
					return openSSH(ctx, session.Node, session.Credentials, nil, "", nil, "root@fixture:22")
				}
			}
			schedule, now := scheduleFor(t, pendingConfirmation(protocol.ConfirmationBeforeChange, protocol.ConnectionSSH))
			attempts, _ := schedule.due(now, nil)
			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			defer cancel()
			answer, err := conn.ConfirmReachability(ctx, attempts[0].request, protocol.ConnectionSSH)
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatal("SSH termination hid the attempt deadline", err)
			}
			schedule.answered(confirmationAnswer{attempt: attempts[0], answer: answer, err: err}, time.Now())
			if repeated, _ := schedule.due(now.Add(10*time.Second), nil); len(repeated) != 1 {
				t.Fatal("timed-out SSH confirmation was permanently refused", repeated)
			}
		})
	}
}

func TestTimedOutSSHProgressStillCollectsRestoration(t *testing.T) {
	previousPoll := reachabilityProgressPoll
	reachabilityProgressPoll = time.Millisecond
	defer func() { reachabilityProgressPoll = previousPoll }()
	marker := filepath.Join(t.TempDir(), "progress-started")
	finished := protocol.Response{ID: confirmationJob, Status: "failed", OwedRestores: []protocol.OwedRestore{{OwedRestoreID: confirmationChange, RestoreStatus: "succeeded"}}}
	raw, _ := json.Marshal(finished)
	fakeSSH(t, `if ! test -f `+quote(marker)+`; then touch `+quote(marker)+`; exec sleep 60; fi
printf '%s\n' `+quote(string(raw)))
	conn := &Connection{Transport: "ssh", native: &sshSession{Node: config.Node{Address: "fixture"}, Runner: "/runner", Credentials: &access.Credentials{}}}
	p := pendingConfirmation(protocol.ConfirmationAfterChange, protocol.ConnectionSSH)
	p.ConfirmedConnectionKinds = []protocol.ConnectionKind{protocol.ConnectionSSH}
	p.ConfirmationDeadline = p.NodeTime.Add(500 * time.Millisecond)
	// The first progress request hangs for 60 s. Finishing within 20 s proves that
	// the controller does not wait for it; the margin is for a loaded test machine.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	en := Entry{Node: "a", JobID: confirmationJob, Response: protocol.Response{ID: confirmationJob, Status: "running", PendingReachabilityConfirmation: p}}
	got, err := (Controller{State: t.TempDir()}).waitEntry(ctx, conn, &Run{ID: "example-run"}, config.FleetStage{}, en, func(Entry) error { return nil })
	if err != nil || got.Response.Status != "failed" || len(got.Response.OwedRestores) != 1 || got.Response.OwedRestores[0].RestoreStatus != "succeeded" {
		t.Fatal(got.Response, err)
	}
}

func TestLostProgressCollectsTerminalJobUntilRestoreFinishes(t *testing.T) {
	previousPoll := reachabilityProgressPoll
	reachabilityProgressPoll = time.Millisecond
	defer func() { reachabilityProgressPoll = previousPoll }()
	for _, restoreStatus := range []string{"succeeded", "failed"} {
		t.Run(restoreStatus, func(t *testing.T) {
			trace := filepath.Join(t.TempDir(), "requests")
			response := protocol.Response{ID: confirmationJob, Status: "failed", Error: "node job worker interrupted", OwedRestores: []protocol.OwedRestore{{OwedRestoreID: confirmationChange, RestoreStatus: "running"}}}
			running, _ := json.Marshal(response)
			response.OwedRestores[0].RestoreStatus = restoreStatus
			finished, _ := json.Marshal(response)
			fakeSSH(t, `for command; do :; done
echo "$command" >> `+quote(trace)+`
case "$command" in
*progress*) if test "$(wc -l < `+quote(trace)+`)" -le 3; then echo 'connection lost' >&2; exit 255; fi
printf '%s\n' `+quote(string(running))+`;;
*status*) printf '%s\n' `+quote(string(finished))+`;;
*) echo 'unexpected request' >&2; exit 1;; esac`)
			conn := &Connection{Transport: "ssh", native: &sshSession{Node: config.Node{Address: "fixture"}, Runner: "/runner", Credentials: &access.Credentials{}}}
			p := pendingConfirmation(protocol.ConfirmationAfterChange, protocol.ConnectionSSH)
			p.ConfirmationDeadline = p.NodeTime.Add(-time.Second)
			// Only a guard against a hang: each request starts a shell, which a
			// loaded machine can make slow.
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			en := Entry{Node: "a", JobID: confirmationJob, Response: protocol.Response{ID: confirmationJob, Status: "running", PendingReachabilityConfirmation: p}}
			got, err := (Controller{State: t.TempDir()}).waitEntry(ctx, conn, &Run{ID: "example-run"}, config.FleetStage{}, en, func(Entry) error { return nil })
			if err != nil || got.Response.Status != "failed" || got.Response.Error != "node job worker interrupted" || len(got.Response.OwedRestores) != 1 || got.Response.OwedRestores[0].RestoreStatus != restoreStatus {
				t.Fatal(got.Response, err)
			}
			if len(lines(t, trace)) != 5 {
				t.Fatal("did not survive three lost requests and collect the restore outcome", lines(t, trace))
			}
		})
	}
}

func TestRecoveredBeforeChangeProgressCancelsReadinessRequest(t *testing.T) {
	started, canceled := make(chan struct{}), make(chan struct{})
	f := followConfirmations(context.Background(), nil, confirmationJob, true, nil, func(ctx context.Context, jobID string, kind protocol.ConnectionKind) (protocol.Response, error) {
		close(started)
		<-ctx.Done()
		close(canceled)
		return protocol.Response{}, ctx.Err()
	}, nil)
	defer f.close()
	p := pendingConfirmation(protocol.ConfirmationBeforeChange, protocol.ConnectionSSH)
	p.ConfirmedConnectionKinds = []protocol.ConnectionKind{protocol.ConnectionSSH}
	response := &protocol.Response{Status: "running", PendingReachabilityConfirmation: p}
	f.observe(response, time.Now(), false)
	f.observe(nil, time.Now(), true)
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("lost progress did not ask readiness")
	}
	f.observe(response, time.Now(), false)
	select {
	case <-canceled:
	case <-time.After(3 * time.Second):
		t.Fatal("healthy before-change progress left readiness request running")
	}
}

func TestStopRequestObeysRestoreCollectionBound(t *testing.T) {
	p := pendingConfirmation(protocol.ConfirmationAfterChange, protocol.ConnectionHTTPS)
	p.ConfirmationDeadline = p.NodeTime.Add(-restoreCollectionAllowance - time.Second)
	p.LatestAfterChangeConfirmationDeadline = p.ConfirmationDeadline
	stopped := make(chan struct{})
	close(stopped)
	var stopCalls int
	conn := &Connection{Transport: "https", direct: fakeEndpoint(t, func(request *http.Request) (*http.Response, error) {
		if request.URL.Path != "/v1/stop/"+confirmationJob {
			t.Errorf("unexpected request %s", request.URL.Path)
		}
		stopCalls++
		<-request.Context().Done()
		return nil, request.Context().Err()
	})}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	en := Entry{Node: "a", JobID: confirmationJob, Response: protocol.Response{ID: confirmationJob, Status: "running", PendingReachabilityConfirmation: p}}
	_, err := (Controller{State: t.TempDir()}).waitEntry(ctx, conn, &Run{ID: "example-run", stopped: stopped}, config.FleetStage{}, en, func(Entry) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "restore completion is unconfirmed") || ctx.Err() != nil || stopCalls != 1 {
		t.Fatal(err, ctx.Err(), stopCalls)
	}
}

func TestConnectionOpenedBeforeActivationCannotConfirmAfterActivation(t *testing.T) {
	// The old connection survives activation. Every connection opened afterwards
	// fails. Delivery on the old connection is held until activation finishes.
	opened, deliver := make(chan struct{}), make(chan struct{})
	secondOpened := make(chan struct{})
	var connections, accepted atomic.Int32
	var readinessReceived atomic.Bool
	firstPath := make(chan string, 1)
	p := pendingConfirmation(protocol.ConfirmationBeforeChange, protocol.ConnectionHTTPS)
	p.ConfirmedConnectionKinds = []protocol.ConnectionKind{protocol.ConnectionHTTPS}
	after := *p
	after.ConfirmationPhase = protocol.ConfirmationAfterChange
	after.ConfirmedConnectionKinds = nil
	conn := &Connection{newConfirmationHTTPS: func() *endpoint.Client {
		number := connections.Add(1)
		if number == 1 {
			close(opened)
		} else {
			if !readinessReceived.Load() {
				t.Error("opened confirmation connection before receiving readiness")
			}
			if number == 2 {
				close(secondOpened)
			}
		}
		return fakeEndpoint(t, func(request *http.Request) (*http.Response, error) {
			if number != 1 {
				return nil, io.EOF
			}
			select {
			case <-deliver:
			case <-request.Context().Done():
				return nil, request.Context().Err()
			}
			firstPath <- request.URL.Path
			var answer any = protocol.Response{ID: confirmationJob, Status: "running", PendingReachabilityConfirmation: &after}
			if request.URL.Path == "/v1/confirm-reachability" {
				// This is the node's phase-only acceptance check: a delayed
				// after-change request on the old connection would be counted.
				var body protocol.ReachabilityConfirmationRequest
				if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
					return nil, err
				}
				if body.ConfirmationPhase == protocol.ConfirmationAfterChange {
					accepted.Add(1)
				}
				answer = counted(confirmationAttempt{request: body, kind: protocol.ConnectionHTTPS}).answer
			}
			raw, _ := json.Marshal(answer)
			return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(raw)), Header: make(http.Header)}, nil
		})
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	f := followConfirmations(ctx, nil, confirmationJob, false, conn.ConfirmReachability, func(ctx context.Context, id string, kind protocol.ConnectionKind) (protocol.Response, error) {
		answer, err := conn.StatusOnNewConnection(ctx, id, kind)
		readinessReceived.Store(err == nil && answer.PendingReachabilityConfirmation != nil && answer.PendingReachabilityConfirmation.ConfirmationPhase == protocol.ConfirmationAfterChange)
		return answer, err
	}, nil)
	defer f.close()
	f.observe(&protocol.Response{Status: "running", PendingReachabilityConfirmation: p}, time.Now(), false)
	f.observe(nil, time.Now(), true)
	select {
	case <-opened:
	case <-ctx.Done():
		t.Fatal("no readiness connection opened")
	}
	close(deliver) // activation completes before the old request reaches the node
	select {
	case path := <-firstPath:
		if path != "/v1/status/"+confirmationJob {
			t.Fatal("connection opened before activation carried a confirmation", path)
		}
	case <-ctx.Done():
		t.Fatal("held request was not delivered")
	}
	select {
	case <-secondOpened:
	case <-ctx.Done():
		t.Fatal("ready answer did not cause a separate new confirmation connection")
	}
	f.close()
	if connections.Load() != 2 || accepted.Load() != 0 {
		t.Fatal("pre-activation connection accepted the change", connections.Load(), accepted.Load())
	}
}

func TestAfterChangeAttemptsRepeatAfterFailureWithTenSecondSpacing(t *testing.T) {
	s, now := scheduleFor(t, pendingConfirmation(protocol.ConfirmationAfterChange, protocol.ConnectionSSH))
	first, _ := s.due(now, nil)
	if len(first) != 1 || first[0].askReadiness {
		t.Fatal(first)
	}
	s.answered(confirmationAnswer{attempt: first[0], err: io.EOF}, now)
	s.progressFailed = true
	if attempts, wake := s.due(now.Add(10*time.Second-time.Nanosecond), nil); len(attempts) != 0 || !wake.Equal(now.Add(10*time.Second)) {
		t.Fatal("after-change repetition was not spaced", attempts, wake)
	}
	repeated, _ := s.due(now.Add(10*time.Second), nil)
	if len(repeated) != 1 || repeated[0].request.ConfirmationPhase != protocol.ConfirmationAfterChange || repeated[0].askReadiness {
		t.Fatal("after-change failure prevented a later confirmation", repeated)
	}
	s.answered(counted(repeated[0]), now.Add(10*time.Second))
	if attempts, _ := s.due(now.Add(20*time.Second), nil); len(attempts) != 0 {
		t.Fatal("repeated a counted after-change confirmation", attempts)
	}
}

func TestFollowerRepeatsAfterChangeWithoutAnyMoreProgressAnswers(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const expectedConfirmationInterval = 10 * time.Second
		started := make(chan time.Time, 2)
		var attempts int
		f := followConfirmations(context.Background(), nil, confirmationJob, true, func(ctx context.Context, request protocol.ReachabilityConfirmationRequest, kind protocol.ConnectionKind) (protocol.ReachabilityConfirmationResponse, error) {
			if request.ConfirmationPhase != protocol.ConfirmationAfterChange {
				t.Errorf("unexpected phase %s", request.ConfirmationPhase)
			}
			started <- time.Now()
			attempts++
			if attempts == 1 {
				return protocol.ReachabilityConfirmationResponse{}, io.EOF
			}
			return counted(confirmationAttempt{request: request, kind: kind}).answer, nil
		}, nil, nil)
		defer f.close()
		p := pendingConfirmation(protocol.ConfirmationAfterChange, protocol.ConnectionSSH)
		f.observe(&protocol.Response{Status: "running", PendingReachabilityConfirmation: p}, time.Now(), false)
		synctest.Wait()
		var first time.Time
		select {
		case first = <-started:
		default:
			t.Fatal("first after-change attempt missing")
		}
		// No further progress or status answer wakes the follower. Its timer alone
		// must start a second attempt, exactly at the repeat interval.
		time.Sleep(expectedConfirmationInterval - time.Nanosecond)
		synctest.Wait()
		select {
		case <-started:
			t.Fatal("after-change request repeated before the interval ended")
		default:
		}
		time.Sleep(time.Nanosecond)
		synctest.Wait()
		select {
		case second := <-started:
			if second.Sub(first) != expectedConfirmationInterval {
				t.Fatal("wrong after-change repeat interval", second.Sub(first))
			}
		default:
			t.Fatal("lost after-change answer prevented repetition without progress")
		}
	})
}

func TestReadinessAnswerMustNameCurrentJobChangeAndAfterPhase(t *testing.T) {
	for _, wrong := range []string{"job", "change", "phase"} {
		t.Run(wrong, func(t *testing.T) {
			p := pendingConfirmation(protocol.ConfirmationBeforeChange, protocol.ConnectionSSH)
			p.ConfirmedConnectionKinds = []protocol.ConnectionKind{protocol.ConnectionSSH}
			s, now := scheduleFor(t, p)
			s.progressFailed = true
			attempts, _ := s.due(now, nil)
			ready := *p
			ready.ConfirmationPhase = protocol.ConfirmationAfterChange
			response := protocol.Response{ID: confirmationJob, Status: "running", PendingReachabilityConfirmation: &ready}
			switch wrong {
			case "job":
				response.ID = strings.Repeat("b", 64)
			case "change":
				ready.OwedRestoreID = "fedcba9876543210"
			case "phase":
				ready.ConfirmationPhase = protocol.ConfirmationBeforeChange
			}
			s.answered(confirmationAnswer{attempt: attempts[0], status: response}, now)
			if s.afterKnown || s.owedRestoreID != confirmationChange {
				t.Fatal("readiness answer authorized another change or phase", s)
			}
		})
	}
}

func TestReadinessRequestsStopAtLatestUnannouncedAfterChangeDeadline(t *testing.T) {
	p := pendingConfirmation(protocol.ConfirmationBeforeChange, protocol.ConnectionSSH)
	p.ConfirmationDeadline = p.NodeTime.Add(5 * time.Second)
	p.LatestAfterChangeConfirmationDeadline = p.NodeTime.Add(26 * time.Second)
	s, now := scheduleFor(t, p)
	s.progressFailed = true
	first, _ := s.due(now.Add(5*time.Second), nil)
	if len(first) != 1 || !first[0].askReadiness {
		t.Fatal("must ask readiness at before-change expiry", first)
	}
	s.answered(confirmationAnswer{attempt: first[0], status: protocol.Response{ID: confirmationJob, Status: "running"}}, now.Add(5*time.Second))
	if attempts, wake := s.due(now.Add(15*time.Second-time.Nanosecond), nil); len(attempts) != 0 || !wake.Equal(now.Add(15*time.Second)) {
		t.Fatal("readiness requests were not spaced", attempts, wake)
	}
	for _, elapsed := range []time.Duration{15 * time.Second, 25 * time.Second} {
		attempts, _ := s.due(now.Add(elapsed), nil)
		if len(attempts) != 1 || !attempts[0].askReadiness || attempts[0].deadline.After(now.Add(26*time.Second)) {
			t.Fatal("incorrect readiness retry or bound", attempts)
		}
		s.answered(confirmationAnswer{attempt: attempts[0], err: io.EOF}, now.Add(elapsed))
	}
	if attempts, wake := s.due(now.Add(26*time.Second), nil); len(attempts) != 0 || !wake.IsZero() {
		t.Fatal("unannounced phase exceeded its latest deadline", attempts, wake)
	}
	if s.afterKnown || !s.collectionDeadline.Equal(now.Add(26*time.Second+restoreCollectionAllowance)) {
		t.Fatal("invented readiness or extended result collection", s)
	}
}

func TestStopStillCollectsFinishedJobsRunningRestore(t *testing.T) {
	started, stopped := make(chan struct{}), make(chan struct{})
	response := protocol.Response{ID: confirmationJob, Status: "failed", Error: "node job worker interrupted", OwedRestores: []protocol.OwedRestore{{OwedRestoreID: confirmationChange, RestoreStatus: "running"}}}
	var requests int
	conn := &Connection{Transport: "https", direct: fakeEndpoint(t, func(request *http.Request) (*http.Response, error) {
		requests++
		if request.URL.Path != "/v1/status/"+confirmationJob || requests != 1 {
			return nil, fmt.Errorf("unexpected request %s (%d)", request.URL.Path, requests)
		}
		if _, ok := request.Context().Deadline(); !ok {
			t.Error("restore collection lacks its own bound")
		}
		close(started)
		<-stopped
		select {
		case <-request.Context().Done():
			return nil, request.Context().Err()
		case <-time.After(20 * time.Millisecond):
		}
		finished := response
		finished.OwedRestores = []protocol.OwedRestore{{OwedRestoreID: confirmationChange, RestoreStatus: "succeeded"}}
		raw, _ := json.Marshal(finished)
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(raw)), Header: make(http.Header)}, nil
	})}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	go func() {
		select {
		case <-started:
			close(stopped)
		case <-ctx.Done():
		}
	}()
	en := Entry{Node: "a", JobID: confirmationJob, Response: response}
	got, err := (Controller{State: t.TempDir()}).waitEntry(ctx, conn, &Run{ID: "example-run", stopped: stopped}, config.FleetStage{}, en, func(Entry) error { return nil })
	if err != nil || requests != 1 || got.Response.Status != "failed" || got.Response.Error != response.Error || got.Response.OwedRestores[0].RestoreStatus != "succeeded" {
		t.Fatal(got.Response, requests, err)
	}
}

func TestRestoreCollectionTimeoutWithoutPendingConfirmationExplainsRecovery(t *testing.T) {
	previousAllowance := restoreCollectionAllowance
	restoreCollectionAllowance = 20 * time.Millisecond
	defer func() { restoreCollectionAllowance = previousAllowance }()
	for _, stop := range []bool{false, true} {
		t.Run(fmt.Sprint(stop), func(t *testing.T) {
			stopped := make(chan struct{})
			if stop {
				close(stopped)
			}
			conn := &Connection{Transport: "https", direct: fakeEndpoint(t, func(request *http.Request) (*http.Response, error) {
				if request.URL.Path != "/v1/status/"+confirmationJob {
					t.Errorf("unexpected request %s", request.URL.Path)
				}
				<-request.Context().Done()
				return nil, request.Context().Err()
			})}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			en := Entry{Node: "a", JobID: confirmationJob, Response: protocol.Response{ID: confirmationJob, Status: "failed", OwedRestores: []protocol.OwedRestore{{OwedRestoreID: confirmationChange, RestoreStatus: "running"}}}}
			_, err := (Controller{State: t.TempDir()}).waitEntry(ctx, conn, &Run{ID: "example-run", stopped: stopped}, config.FleetStage{}, en, func(Entry) error { return nil })
			if err == nil || ctx.Err() != nil {
				t.Fatal("restore collection did not end on its own bound", err, ctx.Err())
			}
			for _, text := range []string{confirmationChange, "restore completion is unconfirmed", "mmx status example-run"} {
				if !strings.Contains(err.Error(), text) {
					t.Fatal("missing recovery explanation", err)
				}
			}
		})
	}
}

func TestOrdinaryNodeJobStartsNoConfirmationFollower(t *testing.T) {
	conn := &Connection{Transport: "https", prepareConfirmationSSH: func(context.Context) (*sshSession, error) {
		t.Fatal("ordinary job prepared confirmation SSH")
		return nil, nil
	}, newConfirmationHTTPS: func() *endpoint.Client { t.Fatal("ordinary job prepared confirmation HTTPS"); return nil }}
	conn.direct = fakeEndpoint(t, func(*http.Request) (*http.Response, error) {
		stacks := make([]byte, 1<<20)
		stacks = stacks[:runtime.Stack(stacks, true)]
		if bytes.Contains(stacks, []byte("controller.followConfirmations")) {
			t.Error("ordinary job started a confirmation goroutine")
		}
		raw, _ := json.Marshal(protocol.Response{ID: confirmationJob, Status: "succeeded"})
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(raw)), Header: make(http.Header)}, nil
	})
	en := Entry{Node: "a", JobID: confirmationJob, Response: protocol.Response{ID: confirmationJob, Status: "running"}}
	got, err := (Controller{State: t.TempDir()}).waitEntry(context.Background(), conn, &Run{}, config.FleetStage{}, en, func(Entry) error { return nil })
	if err != nil || got.Response.Status != "succeeded" {
		t.Fatal(got.Response, err)
	}
}

func TestOrdinaryHTTPSConnectionClosesSetupSSHBeforeAnyProtectedChange(t *testing.T) {
	f := newConnectFixture(t, false, false, currentInfo)
	conn, err := f.connect(t)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines(t, f.ssh)) == 0 || conn.direct == nil || conn.native != nil || conn.confirmationSSH != nil {
		t.Fatal("ordinary HTTPS retained its setup SSH session", conn, lines(t, f.ssh))
	}
	if len(lines(t, f.sshClosed)) != 1 {
		t.Fatal("setup SSH was not closed before returning the ordinary connection", lines(t, f.sshClosed))
	}
}

func TestControllerMachineConfirmsOnlyAfterRequiredInventoryNodesAnswerAgain(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := &Run{Limit: []string{"util01"}, Project: config.Project{MaxNodesAtOnce: 2, FleetStages: []config.FleetStage{{ID: "network", Nodes: "util01"}}}, Inventory: config.Inventory{Nodes: map[string]config.Node{
			"util01": {}, "app01": {}, "db01": {}, "offline01": {}, "util01-alias": {}, "util01-clone": {},
		}}}
		var after, dbRecovered atomic.Bool
		var active, peak, beforeConfirmations, afterConfirmations atomic.Int32
		var mu sync.Mutex
		calls := map[string][]time.Time{}
		beforeRelease := make(chan struct{})
		c := Controller{controllerMachine: func() (protocol.ControllerMachine, error) { return testControllerMachine, nil }}
		c.probeInventoryNode = func(ctx context.Context, _ *Run, name string) (bootedMachine, error) {
			n := active.Add(1)
			defer active.Add(-1)
			for old := peak.Load(); n > old && !peak.CompareAndSwap(old, n); old = peak.Load() {
			}
			phase := "before/"
			if after.Load() {
				phase = "after/"
			}
			mu.Lock()
			calls[phase+name] = append(calls[phase+name], time.Now())
			mu.Unlock()
			if !after.Load() {
				select {
				case <-beforeRelease:
				case <-ctx.Done():
					return bootedMachine{}, ctx.Err()
				}
			}
			if name == "offline01" || name == "db01" && after.Load() && !dbRecovered.Load() {
				return bootedMachine{}, io.EOF
			}
			switch name {
			case "util01-alias":
				return bootedMachine{testControllerMachine.MachineIDHash, testControllerMachine.BootID}, nil
			case "util01-clone":
				// A virtual machine cloned from the controller machine's image
				// shares its machine ID but boots its own kernel.
				return bootedMachine{testControllerMachine.MachineIDHash, "boot-of-util01-clone"}, nil
			}
			return bootedMachine{"identity-of-" + name, "boot-of-" + name}, nil
		}
		inventory, err := c.controllerInventoryReachability(r, "util01", bootedMachine{testControllerMachine.MachineIDHash, testControllerMachine.BootID})
		if err != nil {
			t.Fatal(err)
		}
		f := followConfirmations(context.Background(), nil, confirmationJob, false, func(_ context.Context, request protocol.ReachabilityConfirmationRequest, kind protocol.ConnectionKind) (protocol.ReachabilityConfirmationResponse, error) {
			if request.ConfirmationPhase == protocol.ConfirmationBeforeChange {
				beforeConfirmations.Add(1)
			} else {
				afterConfirmations.Add(1)
			}
			return counted(confirmationAttempt{request: request, kind: kind}).answer, nil
		}, nil, inventory)
		defer f.close()
		p := pendingConfirmation(protocol.ConfirmationBeforeChange, protocol.ConnectionSSH, protocol.ConnectionHTTPS)
		f.observe(&protocol.Response{Status: "running", PendingReachabilityConfirmation: p}, time.Now(), false)
		synctest.Wait()
		if beforeConfirmations.Load() != 0 || active.Load() != 2 {
			t.Fatal("confirmed before the inventory probes completed", beforeConfirmations.Load(), active.Load())
		}
		close(beforeRelease)
		synctest.Wait()
		if beforeConfirmations.Load() != 2 || peak.Load() != 2 {
			t.Fatal("missing confirmations or wrong parallel probe bound", beforeConfirmations.Load(), peak.Load())
		}
		after.Store(true)
		p = pendingConfirmation(protocol.ConfirmationAfterChange, protocol.ConnectionSSH, protocol.ConnectionHTTPS)
		f.observe(&protocol.Response{Status: "running", PendingReachabilityConfirmation: p}, time.Now(), false)
		synctest.Wait()
		if afterConfirmations.Load() != 0 {
			t.Fatal("confirmed while db01 was still unreachable")
		}
		dbRecovered.Store(true)
		time.Sleep(10*time.Second - time.Nanosecond)
		synctest.Wait()
		if afterConfirmations.Load() != 0 {
			t.Fatal("retried db01 too soon")
		}
		time.Sleep(time.Nanosecond)
		synctest.Wait()
		if afterConfirmations.Load() != 2 {
			t.Fatal("did not confirm all connection kinds after db01 recovered", afterConfirmations.Load())
		}
		mu.Lock()
		defer mu.Unlock()
		for _, name := range []string{"app01", "db01", "offline01", "util01-alias", "util01-clone"} {
			if len(calls["before/"+name]) != 1 {
				t.Fatalf("inventory node %s outside --limit was not probed exactly once: %v", name, calls)
			}
		}
		if len(calls["before/util01"]) != 0 || len(calls["after/offline01"]) != 0 || len(calls["after/util01-alias"]) != 0 || len(calls["after/util01-clone"]) != 1 || len(calls["after/app01"]) != 1 || len(calls["after/db01"]) != 2 {
			t.Fatal("wrong required-node set", calls)
		}
	})
}

func TestControllerMachineUnreachableInventoryNodeFailsWithoutConfirmation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var after, beforeConfirmed atomic.Bool
		var afterConfirmations atomic.Int32
		r := &Run{ID: "example-run", Project: config.Project{MaxNodesAtOnce: 2}, Inventory: config.Inventory{Nodes: map[string]config.Node{"util01": {}, "db01": {}, "app01": {}}}}
		c := Controller{State: t.TempDir(), controllerMachine: func() (protocol.ControllerMachine, error) { return testControllerMachine, nil }}
		c.probeInventoryNode = func(_ context.Context, _ *Run, name string) (bootedMachine, error) {
			if after.Load() {
				return bootedMachine{}, io.EOF
			}
			return bootedMachine{"identity-of-" + name, "boot-of-" + name}, nil
		}
		p := pendingConfirmation(protocol.ConfirmationBeforeChange, protocol.ConnectionHTTPS)
		conn := &Connection{NodeID: testControllerMachine.MachineIDHash, BootID: testControllerMachine.BootID, Transport: "https"}
		conn.newConfirmationHTTPS = func() *endpoint.Client {
			return fakeEndpoint(t, func(request *http.Request) (*http.Response, error) {
				var body protocol.ReachabilityConfirmationRequest
				if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
					return nil, err
				}
				if body.ConfirmationPhase == protocol.ConfirmationBeforeChange {
					beforeConfirmed.Store(true)
				} else {
					afterConfirmations.Add(1)
				}
				raw, _ := json.Marshal(counted(confirmationAttempt{request: body, kind: protocol.ConnectionHTTPS}).answer)
				return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(raw)), Header: make(http.Header)}, nil
			})
		}
		var deadline time.Time
		conn.direct = fakeEndpoint(t, func(request *http.Request) (*http.Response, error) {
			time.Sleep(time.Second)
			response := protocol.Response{ID: confirmationJob, Status: "running"}
			pending := *p
			pending.NodeTime = time.Now().UTC()
			if beforeConfirmed.Load() {
				if !after.Swap(true) {
					deadline = time.Now().UTC().Add(25 * time.Second)
				}
				pending.ConfirmationPhase = protocol.ConfirmationAfterChange
				pending.ConfirmationDeadline = deadline
			}
			response.PendingReachabilityConfirmation = &pending
			if !deadline.IsZero() && !time.Now().Before(deadline) {
				response.Status = "failed"
				response.PendingReachabilityConfirmation = nil
				response.Results = []protocol.Result{{Task: "network-block", Status: "failed", Items: []protocol.Result{{Task: "network", Status: "failed", Reason: "The change was not confirmed.", OwedRestore: &protocol.OwedRestore{OwedRestoreID: confirmationChange, RestoreStatus: "succeeded"}}}}}
			}
			raw, _ := json.Marshal(response)
			return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(raw)), Header: make(http.Header)}, nil
		})
		en := Entry{Node: "util01", JobID: confirmationJob, Response: protocol.Response{ID: confirmationJob, Status: "running", PendingReachabilityConfirmation: p}}
		var saved Entry
		got, err := c.waitEntry(context.Background(), conn, r, config.FleetStage{}, en, func(en Entry) error { saved = en; return nil })
		if err != nil || got.Response.Status != "failed" || afterConfirmations.Load() != 0 {
			t.Fatal(got.Response, afterConfirmations.Load(), err)
		}
		want := "The changed node is the controller machine. After the change, the controller machine could not reach these nodes over new SSH connections: app01, db01. MMX did not confirm the change."
		for _, result := range []protocol.Result{got.Response.Results[0], got.Response.Results[0].Items[0], saved.Response.Results[0], saved.Response.Results[0].Items[0]} {
			if !strings.Contains(result.Safe().Reason, want) {
				t.Fatal("task failure and saved output do not name the unreachable nodes", result)
			}
		}
	})
}

// A node is the controller machine only with the controller machine's machine
// ID and boot ID. A clone of the controller machine's image shares the machine
// ID but boots its own kernel, so it is an ordinary node too.
func TestOrdinaryNodeOpensNoInventoryConnections(t *testing.T) {
	c := Controller{controllerMachine: func() (protocol.ControllerMachine, error) { return testControllerMachine, nil }, probeInventoryNode: func(context.Context, *Run, string) (bootedMachine, error) {
		t.Fatal("ordinary node probed inventory")
		return bootedMachine{}, nil
	}}
	clone := bootedMachine{testControllerMachine.MachineIDHash, "boot-of-a-clone"}
	if g, err := c.controllerInventoryReachability(&Run{}, "clone01", clone); err != nil || g != nil {
		t.Fatal("a clone of the controller machine was taken for the controller machine", g, err)
	}
	g, err := c.controllerInventoryReachability(&Run{}, "db01", bootedMachine{"another-machine", testControllerMachine.BootID})
	if err != nil || g != nil {
		t.Fatal(g, err)
	}
	synctest.Test(t, func(t *testing.T) {
		var calls atomic.Int32
		f := followConfirmations(context.Background(), nil, confirmationJob, true, func(_ context.Context, request protocol.ReachabilityConfirmationRequest, kind protocol.ConnectionKind) (protocol.ReachabilityConfirmationResponse, error) {
			calls.Add(1)
			return counted(confirmationAttempt{request: request, kind: kind}).answer, nil
		}, nil, g)
		defer f.close()
		for _, phase := range []protocol.ConfirmationPhase{protocol.ConfirmationBeforeChange, protocol.ConfirmationAfterChange} {
			f.observe(&protocol.Response{Status: "running", PendingReachabilityConfirmation: pendingConfirmation(phase, protocol.ConnectionSSH)}, time.Now(), false)
			synctest.Wait()
		}
		if calls.Load() != 2 {
			t.Fatal("ordinary confirmations changed", calls.Load())
		}
	})
}

func TestControllerMachineAfterChangeWithoutBaselineDoesNotConfirm(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		g := &inventoryReachability{nodes: []string{"db01"}, limit: 1, probe: func(context.Context, string) (bootedMachine, error) {
			t.Error("after-change probe tried to replace missing before-change evidence")
			return bootedMachine{"db01", "boot-of-db01"}, nil
		}}
		f := followConfirmations(context.Background(), nil, confirmationJob, true, func(context.Context, protocol.ReachabilityConfirmationRequest, protocol.ConnectionKind) (protocol.ReachabilityConfirmationResponse, error) {
			t.Error("confirmed without a before-change set")
			return protocol.ReachabilityConfirmationResponse{}, nil
		}, nil, g)
		defer f.close()
		p := pendingConfirmation(protocol.ConfirmationAfterChange, protocol.ConnectionSSH)
		f.observe(&protocol.Response{Status: "running", PendingReachabilityConfirmation: p}, time.Now(), false)
		time.Sleep(time.Minute)
		window := f.observe(nil, time.Now(), true)
		if !strings.Contains(window.inventoryFailures[confirmationChange], "did not complete the before-change inventory probes") {
			t.Fatal(window)
		}
	})
}

func TestInventoryProbesWaitForAfterChangeAnnouncement(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var probes atomic.Int32
		var announceAfter atomic.Bool
		g := &inventoryReachability{nodes: []string{"db01"}, limit: 1, probe: func(context.Context, string) (bootedMachine, error) {
			probes.Add(1)
			return bootedMachine{"identity-of-db01", "boot-of-db01"}, nil
		}}
		f := followConfirmations(context.Background(), nil, confirmationJob, true, func(_ context.Context, request protocol.ReachabilityConfirmationRequest, kind protocol.ConnectionKind) (protocol.ReachabilityConfirmationResponse, error) {
			return counted(confirmationAttempt{request: request, kind: kind}).answer, nil
		}, func(context.Context, string, protocol.ConnectionKind) (protocol.Response, error) {
			phase := protocol.ConfirmationBeforeChange
			if announceAfter.Load() {
				phase = protocol.ConfirmationAfterChange
			}
			return protocol.Response{ID: confirmationJob, Status: "running", PendingReachabilityConfirmation: pendingConfirmation(phase, protocol.ConnectionSSH)}, nil
		}, g)
		defer f.close()
		f.observe(&protocol.Response{Status: "running", PendingReachabilityConfirmation: pendingConfirmation(protocol.ConfirmationBeforeChange, protocol.ConnectionSSH)}, time.Now(), false)
		synctest.Wait()
		f.observe(nil, time.Now(), true)
		synctest.Wait()
		if probes.Load() != 1 {
			t.Fatal("probed inventory again during activation", probes.Load())
		}
		announceAfter.Store(true)
		time.Sleep(10 * time.Second)
		synctest.Wait()
		if probes.Load() != 2 {
			t.Fatal("did not probe inventory after the readiness answer", probes.Load())
		}
	})
}

func TestInventoryFailureNamesStayVisibleWithoutRevealingSecretDiagnostics(t *testing.T) {
	response := protocol.Response{Results: []protocol.Result{{Status: "failed", Secret: true, Tries: []protocol.Result{{Status: "failed", Reason: "secret diagnostic", OwedRestore: &protocol.OwedRestore{OwedRestoreID: confirmationChange}}}}}}
	failures := map[string]string{confirmationChange: "The controller machine could not reach node db01."}
	held := response // The fleet scheduler may still display the previous snapshot.
	addInventoryFailures(&response, failures)
	addInventoryFailures(&response, failures)
	reason := response.Results[0].Tries[0].Safe().Reason
	if held.Results[0].Tries[0].Reason != "secret diagnostic" || held.Results[0].Tries[0].ReasonReadsNoSecret {
		t.Fatal("changed a previously published progress snapshot", held)
	}
	if strings.Contains(reason, "secret diagnostic") || strings.Count(reason, failures[confirmationChange]) != 1 {
		t.Fatal("failure names were hidden, duplicated, or revealed a secret", reason)
	}
}

func TestStatusRefreshKeepsControllerMachineReachabilityFailure(t *testing.T) {
	const reason = "The controller machine could not reach node db01."
	response := protocol.Response{ID: confirmationJob, Status: "failed", Results: []protocol.Result{{Task: "network", Status: "failed", Reason: "The change was not confirmed.", OwedRestore: &protocol.OwedRestore{OwedRestoreID: confirmationChange, RestoreStatus: "succeeded"}}}}
	raw, _ := json.Marshal(response)
	c, r := sshNode(t, `case "$command" in *status*) printf '%s\n' `+quote(string(raw))+`;; *) echo 'unexpected command' >&2; exit 1;; esac`)
	old := response
	old.OwedRestores = []protocol.OwedRestore{{OwedRestoreID: confirmationChange, RestoreStatus: "failed"}}
	r.Entries = []Entry{{Node: "a", FleetStage: "setup", JobID: confirmationJob, Response: old, ControllerMachineReachabilityFailures: map[string]string{confirmationChange: reason}}}
	if err := c.saveEntry(r.ID, r.Entries[0]); err != nil {
		t.Fatal(err)
	}
	if err := c.Inspect(context.Background(), r, false); err != nil {
		t.Fatal(err)
	}
	if got := r.Entries[0].Response.Results[0].Reason; !strings.Contains(got, reason) {
		t.Fatal("status erased the controller-machine failure", got)
	}
}

func TestControllerMachineCannotBuildBaselineAcrossActivation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		g := &inventoryReachability{nodes: []string{"db01"}, limit: 1, probe: func(context.Context, string) (bootedMachine, error) {
			<-release
			return bootedMachine{"identity-of-db01", "boot-of-db01"}, nil
		}}
		f := followConfirmations(context.Background(), nil, confirmationJob, true, func(context.Context, protocol.ReachabilityConfirmationRequest, protocol.ConnectionKind) (protocol.ReachabilityConfirmationResponse, error) {
			t.Error("confirmed a change without completing probes before activation")
			return protocol.ReachabilityConfirmationResponse{}, nil
		}, nil, g)
		defer f.close()
		f.observe(&protocol.Response{Status: "running", PendingReachabilityConfirmation: pendingConfirmation(protocol.ConfirmationBeforeChange, protocol.ConnectionSSH)}, time.Now(), false)
		synctest.Wait()
		// A resumed process may see before-change confirmations already counted
		// by the previous process, allowing activation while new probes run.
		f.observe(&protocol.Response{Status: "running", PendingReachabilityConfirmation: pendingConfirmation(protocol.ConfirmationAfterChange, protocol.ConnectionSSH)}, time.Now(), false)
		close(release)
		synctest.Wait()
		time.Sleep(time.Minute)
		window := f.observe(nil, time.Now(), true)
		if !strings.Contains(window.inventoryFailures[confirmationChange], "did not complete the before-change inventory probes") {
			t.Fatal(window)
		}
	})
}
