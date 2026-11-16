package modules

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"mmx/internal/protocol"
	"mmx/internal/value"
)

// Not beside other tests: once this test closes its port, a listener of
// another test could get the same port and answer.
func TestWaitForPortOpenAndClosed(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	port := listener.Addr().(*net.TCPAddr).Port
	w := WaitFor{TCPPortAcceptsConnections: &port, WaitAtMost: "30s"}
	if r := (Runtime{}).WaitFor(context.Background(), w, nil); r.Status != "succeeded" || r.Change != "unchanged" {
		t.Fatal(r)
	}
	w.TCPPortAcceptsConnections, w.TCPPortRefusesConnections = nil, &port
	if r := (Runtime{Dryrun: true}).WaitFor(context.Background(), w, nil); r.Status != "not_checked" || r.Change != "unchanged" {
		t.Fatal(r)
	}
	listener.Close()
	if r := (Runtime{}).WaitFor(context.Background(), w, nil); r.Status != "succeeded" {
		t.Fatal(r)
	}
	w.TCPPortAcceptsConnections, w.TCPPortRefusesConnections = &port, nil
	if r := waitForOneFinishedObservation(Runtime{}, context.Background(), w, nil); r.Status != "failed" || !strings.Contains(r.Reason, "connection refused") {
		t.Fatal(r)
	}
	// A lookup failure is not proof that a port is closed.
	w.TCPHost = "invalid host name"
	w.TCPPortAcceptsConnections, w.TCPPortRefusesConnections = nil, &port
	if r := (Runtime{Dryrun: true}).WaitFor(context.Background(), w, nil); r.Status != "not_checked" {
		t.Fatal(r)
	}
}

// lateContext reports its end some time after its deadline, as a context does
// when the timer behind its deadline runs late.
type lateContext struct {
	context.Context
	deadline time.Time
}

func (c lateContext) Deadline() (time.Time, bool) { return c.deadline, true }

// Not parallel, as TestWaitForPortOpenAndClosed: the port must stay closed.
func TestWaitForReportsTheLastFinishedPortCheck(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()
	// Finish the refused connection before attempting a check whose dial
	// deadline has already expired. Only then end the enclosing wait.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	checks := 0
	r := (Runtime{}).waitFor(ctx, WaitFor{TCPPortAcceptsConnections: &port, WaitAtMost: "30s"}, nil, func(ctx context.Context, w WaitFor, command *waitCommand) protocol.Result {
		checks++
		if checks == 1 {
			return waitObservation(ctx, w, command)
		}
		interrupted := waitObservation(lateContext{context.Background(), time.Now().Add(-time.Hour)}, w, command)
		cancel()
		return interrupted
	})
	if r.Status != "failed" || !strings.Contains(r.Reason, "last observation: dial tcp") || !strings.Contains(r.Reason, "connection refused") || strings.Contains(r.Reason, "i/o timeout") || checks != 2 {
		t.Fatal(r)
	}
}

func TestWaitForPathsAndDryrun(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "ready")
	w := WaitFor{PathExists: path, WaitAtMost: "30s"}
	start := time.Now()
	if r := (Runtime{Dryrun: true}).WaitFor(context.Background(), w, nil); r.Status != "not_checked" || r.Change != "unchanged" || time.Since(start) > 3*time.Second {
		t.Fatal(r)
	}
	if r := waitForOneFinishedObservation(Runtime{}, context.Background(), w, nil); r.Status != "failed" || !strings.Contains(r.Reason, "last observation: path "+path+" is missing") {
		t.Fatal(r)
	}
	w.PathExists, w.PathIsMissing = "", path
	if r := (Runtime{}).WaitFor(context.Background(), w, nil); r.Status != "succeeded" {
		t.Fatal(r)
	}
	if err := os.WriteFile(path, []byte("ready"), 0600); err != nil {
		t.Fatal(err)
	}
	if r := (Runtime{Dryrun: true}).WaitFor(context.Background(), w, nil); r.Status != "not_checked" {
		t.Fatal(r)
	}
	w.PathExists, w.PathIsMissing = path, ""
	if r := (Runtime{}).WaitFor(context.Background(), w, nil); r.Status != "succeeded" || r.Change != "unchanged" {
		t.Fatal(r)
	}
}

func TestWaitForHTTPResponseAndPolling(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			t.Error("method", r.Method)
		}
		if calls.Add(1) == 1 {
			w.WriteHeader(503)
			fmt.Fprint(w, "starting")
			return
		}
		w.WriteHeader(201)
		fmt.Fprint(w, "service ready")
	}))
	defer server.Close()
	body := "ready"
	start := time.Now()
	r := (Runtime{}).WaitFor(context.Background(), WaitFor{URLResponds: server.URL, ExpectedHTTPStatusCodes: []int{201}, ResponseBodyContains: &body, WaitAtMost: "3s"}, nil)
	if r.Status != "succeeded" || r.Change != "unchanged" || r.HTTPStatusCode != 201 || r.ResponseBody != "service ready" || calls.Load() != 2 || time.Since(start) < waitForCheckInterval {
		t.Fatal(r, calls.Load())
	}
}

func TestWaitForHTTPLastObservationAndDryrun(t *testing.T) {
	t.Parallel()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(503)
		fmt.Fprint(w, "starting")
	}))
	defer server.Close()
	w := WaitFor{URLResponds: server.URL, WaitAtMost: "30s"}
	for _, dryrun := range []bool{true, false} {
		before := calls.Load()
		var r protocol.Result
		if dryrun {
			r = (Runtime{Dryrun: true}).WaitFor(context.Background(), w, nil)
		} else {
			r = waitForOneFinishedObservation(Runtime{}, context.Background(), w, nil)
		}
		status := "failed"
		if dryrun {
			status = "not_checked"
		}
		if r.Status != status || r.Change != "unchanged" || r.HTTPStatusCode != 503 || r.ResponseBody != "starting" || !strings.Contains(r.Reason, "HTTP status 503") || calls.Load() != before+1 {
			t.Fatal(r)
		}
	}
	body := "ready"
	w.ExpectedHTTPStatusCodes = []int{503}
	w.ResponseBodyContains = &body
	if r := waitForOneFinishedObservation(Runtime{}, context.Background(), w, nil); r.Status != "failed" || !strings.Contains(r.Reason, "response_body does not contain") || r.HTTPStatusCode != 503 {
		t.Fatal(r)
	}
}

// A check that is still running when the wait ends says only that it was
// interrupted, so the wait reports the last check that finished.
func TestWaitForReportsTheLastCheckThatFinished(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) > 1 {
			cancel()
			<-r.Context().Done()
			return
		}
		w.WriteHeader(503)
		fmt.Fprint(w, "starting")
	}))
	defer server.Close()
	r := (Runtime{}).WaitFor(ctx, WaitFor{URLResponds: server.URL, WaitAtMost: "30s"}, nil)
	if r.Status != "failed" || r.HTTPStatusCode != 503 || calls.Load() != 2 ||
		!strings.Contains(r.Reason, "wait_for ended after at most 30s") || !strings.Contains(r.Reason, "last observation: HTTP status 503") {
		t.Fatalf("%d calls: %+v", calls.Load(), r)
	}
}

func TestWaitForHTTPDeadlineAndCancellation(t *testing.T) {
	t.Parallel()
	// Read a partial response with virtual time. The deadline cannot expire
	// before the reader has returned the partial bytes to the HTTP code.
	synctest.Test(t, func(t *testing.T) {
		started := time.Now()
		r := (Runtime{}).waitFor(context.Background(), WaitFor{URLResponds: "http://example.test", WaitAtMost: "1s"}, nil, func(ctx context.Context, _ WaitFor, _ *waitCommand) protocol.Result {
			return readHTTPResponse(&http.Response{StatusCode: 200, Body: &partialHTTPBody{ctx: ctx}}, nil)
		})
		if r.Status != "failed" || r.Change != "unchanged" || r.HTTPStatusCode != 200 || r.ResponseBody != "partial" || !strings.Contains(r.Reason, "deadline exceeded") || time.Since(started) != time.Second {
			t.Fatal(r)
		}
	})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { t.Error("a canceled request reached the server") }))
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	r := (Runtime{}).WaitFor(ctx, WaitFor{URLResponds: server.URL}, nil)
	if r.Status != "failed" || !strings.Contains(r.Reason, "canceled") {
		t.Fatal(r)
	}
}

func TestHTTPAndWaitForRequestRules(t *testing.T) {
	t.Parallel()
	var redirected atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, "/destination", http.StatusFound)
			return
		}
		redirected.Add(1)
		fmt.Fprint(w, "ready")
	}))
	defer server.Close()
	if r := SendHTTPRequest(context.Background(), HTTP{URL: server.URL + "/redirect", ExpectedHTTPStatusCodes: []int{200, 302}}); r.Status != "succeeded" || r.HTTPStatusCode != 302 {
		t.Fatal("a listed status code was not accepted", r)
	}
	if r := SendHTTPRequest(context.Background(), HTTP{URL: server.URL + "/redirect", ExpectedHTTPStatusCodes: []int{200, 404}}); r.Status != "failed" || r.Reason != "HTTP status 302; expected 200 or 404" {
		t.Fatal(r)
	}
	redirected.Store(0)
	if r := SendHTTPRequest(context.Background(), HTTP{URL: server.URL + "/redirect"}); r.Status != "failed" || r.HTTPStatusCode != 302 || redirected.Load() != 0 {
		t.Fatal(r)
	}
	if r := (Runtime{Dryrun: true}).WaitFor(context.Background(), WaitFor{URLResponds: server.URL + "/redirect"}, nil); r.Status != "not_checked" || r.HTTPStatusCode != 302 || redirected.Load() != 0 {
		t.Fatal(r)
	}
	tls := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "ready") }))
	defer tls.Close()
	if r := (Runtime{Dryrun: true}).WaitFor(context.Background(), WaitFor{URLResponds: tls.URL}, nil); r.Status != "not_checked" || !strings.Contains(r.Reason, "certificate") {
		t.Fatal(r)
	}
	if r := SendHTTPRequest(context.Background(), HTTP{URL: server.URL}); r.Status != "succeeded" || r.HTTPStatusCode != 200 || r.ResponseBody != "ready" || r.Stdout != "" || r.Value != nil {
		t.Fatal(r)
	}
}

// A script is ready when it exits with code 0. It runs with its env, and a
// wait that ends reports the last exit code and output.
func TestWaitForScript(t *testing.T) {
	t.Parallel()
	ready := filepath.Join(t.TempDir(), "ready")
	w := WaitFor{CommandSucceeds: `echo "checked $NAME"; test -e "$READY_FILE"`, Env: map[string]any{"READY_FILE": ready, "NAME": "{{ .vars.name }}"}, WaitAtMost: "30s"}
	scope := value.Scope{"vars": map[string]any{"name": "app"}}
	r := Runtime{Dryrun: true}.WaitFor(context.Background(), w, scope)
	if r.Status != "not_checked" || r.Change != "unchanged" || r.ExitCode != 1 || r.Stdout != "checked app\n" {
		t.Fatal(r)
	}
	checks := 0
	r = (Runtime{}).waitFor(context.Background(), w, scope, func(ctx context.Context, w WaitFor, command *waitCommand) protocol.Result {
		observation := waitObservation(ctx, w, command)
		checks++
		if checks == 1 {
			if observation.ExitCode != 1 {
				t.Fatal(observation)
			}
			if err := os.WriteFile(ready, nil, 0600); err != nil {
				t.Fatal(err)
			}
		}
		return observation
	})
	if r.Status != "succeeded" || r.Change != "unchanged" || r.ExitCode != 0 || checks != 2 {
		t.Fatal(r)
	}
	w = WaitFor{CommandSucceeds: "echo not yet >&2; exit 4", WaitAtMost: "30s"}
	checks = 0
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r = (Runtime{}).waitFor(ctx, w, value.Scope{}, func(ctx context.Context, w WaitFor, command *waitCommand) protocol.Result {
		checks++
		if checks == 2 {
			cancel()
		}
		return waitObservation(ctx, w, command)
	})
	if r.Status != "failed" || r.ExitCode != 4 || r.Stderr != "not yet\n" || !strings.Contains(r.Reason, "last observation: the wait_for.command_succeeds script exited with code 4") || checks != 2 {
		t.Fatal(r)
	}
	w = WaitFor{CommandSucceeds: "true", User: "mmx-no-such-account"}
	if r := (Runtime{}).WaitFor(context.Background(), w, value.Scope{}); r.Status != "failed" || !strings.Contains(r.Reason, "wait_for.run_as_user_account") {
		t.Fatal(r)
	}
}

// A wait_for script whose run_from_directory does not exist fails at once as
// a missing prerequisite: waiting cannot create the directory.
func TestWaitForScriptWithoutItsDirectory(t *testing.T) {
	t.Parallel()
	directory := filepath.Join(t.TempDir(), "openwisp2")
	r := Runtime{}.WaitFor(context.Background(), WaitFor{CommandSucceeds: "true", Cwd: directory, WaitAtMost: "1m"}, value.Scope{})
	if r.Status != "failed" || r.FailureKind != "missing_prerequisite" || r.Reason != "run_from_directory "+directory+" does not exist; create it with an earlier directory node task" {
		t.Fatalf("%+v", r)
	}
}

// End the enclosing wait only after a real check has returned its result.
// The wide product deadline remains a guard for an observation that hangs.
func waitForOneFinishedObservation(run Runtime, parent context.Context, w WaitFor, scope value.Scope) protocol.Result {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	return run.waitFor(ctx, w, scope, func(ctx context.Context, w WaitFor, command *waitCommand) protocol.Result {
		observation := waitObservation(ctx, w, command)
		cancel()
		return observation
	})
}

type partialHTTPBody struct {
	ctx  context.Context
	sent bool
}

func (body *partialHTTPBody) Read(p []byte) (int, error) {
	if !body.sent {
		body.sent = true
		return copy(p, "partial"), nil
	}
	<-body.ctx.Done()
	return 0, body.ctx.Err()
}
func (*partialHTTPBody) Close() error { return nil }
