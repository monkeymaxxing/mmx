package engine

import (
	"bytes"
	"context"
	"mmx/internal/config"
	"mmx/internal/modules"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

func TestRetriesPreserveEarlierChangeAndTryResults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "first")
	count := 3
	e := Engine{}
	task := config.Task{Module: "shell", ModuleInputs: &modules.Shell{Script: "if test -f \"$MARK\"; then exit 0; else touch \"$MARK\"; exit 2; fi", Env: map[string]any{"MARK": path}, ChangedWhen: map[string]any{"equals": []any{"{{ .this_node_task.exit_code }}", 2}}}, Retry: &config.Retry{MaxTries: config.TryLimit(count), PauseBetweenTries: "0s"}}
	r := e.timed(context.Background(), task, scope())
	if r.Status != "succeeded" || r.Change != "changed" || len(r.Tries) != 2 || r.Tries[0].ExitCode != 2 || r.Tries[1].Change != "unchanged" {
		t.Fatal(r)
	}
}
func TestUntilDoesNotOverrideFailureAndRetryWhenStops(t *testing.T) {
	count := 3
	e := Engine{}
	task := config.Task{Module: "shell", ModuleInputs: &modules.Shell{Script: "exit 5"}, Retry: &config.Retry{MaxTries: config.TryLimit(count), PauseBetweenTries: "0s", Until: true, RetryWhen: false}}
	r := e.timed(context.Background(), task, scope())
	if r.Status != "failed" || len(r.Tries) != 1 {
		t.Fatal(r)
	}
}
func TestOverallTimeoutIncludesPause(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e := Engine{}
		timeout := "100ms"
		task := config.Task{Module: "shell", ModuleInputs: &modules.Shell{Script: "exit 1"}, TimeLimit: &timeout, Retry: &config.Retry{PauseBetweenTries: "30s"}}
		start := time.Now()
		r := e.timed(context.Background(), task, scope())
		if r.Status != "failed" || len(r.Tries) != 1 || !strings.Contains(r.Reason, "time_limit exceeded during retry.pause_between_tries") || time.Since(start) > 3*time.Second {
			t.Fatal(r)
		}
	})
}

// When a task's time limit ends, a try that failed with a reason of its own
// keeps that reason behind the words about the limit, and a try without one
// gets the limit's own text. A node job that was stopped says so, and keeps
// the try's reason in the same way.
func TestTimeLimitKeepsTheReasonOfTheTryThatFailedAtTheLimit(t *testing.T) {
	// A server that answers only when the test ends: a request to it fails
	// when its context ends, with the http module's own words.
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer server.Close()
	defer close(release)
	limit := "100ms"
	request := config.Task{Module: "http", ModuleInputs: &modules.HTTP{URL: server.URL}, TimeLimit: &limit}
	r := (&Engine{}).timed(context.Background(), request, scope())
	if own := `time_limit exceeded: Get "` + server.URL + `": context deadline exceeded`; r.Status != "failed" || r.Reason != own {
		t.Fatalf("%s: %s\nwant %s", r.Status, r.Reason, own)
	}

	// A try that returns without a failure after the limit has ended has no
	// reason of its own.
	already := "1ns"
	r = (&Engine{}).timed(context.Background(), config.Task{Show: "the limit ended before this task", TimeLimit: &already}, scope())
	if r.Status != "failed" || r.Reason != "time_limit exceeded: context deadline exceeded" {
		t.Fatalf("%s: %s", r.Status, r.Reason)
	}

	// time_limit_per_try names itself, so the reason says which limit ended
	// the try.
	r = (&Engine{}).timed(context.Background(), config.Task{Show: "the try's limit ended before this task", TimeLimitPerTry: &already}, scope())
	if r.Status != "failed" || r.Reason != "time_limit_per_try exceeded: context deadline exceeded" {
		t.Fatalf("%s: %s", r.Status, r.Reason)
	}

	// A node job that was stopped: the same failed request. Its reason does
	// not speak of a time limit.
	cancelled, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	request.TimeLimit = nil
	r = (&Engine{}).timed(cancelled, request, scope())
	if own := `the node job was stopped: Get "` + server.URL + `": context canceled`; r.Status != "failed" || r.Reason != own {
		t.Fatalf("%s: %s\nwant %s", r.Status, r.Reason, own)
	}
	// A try without a reason of its own, in a node job that was stopped.
	cancel()
	r = (&Engine{}).timed(cancelled, config.Task{Show: "the node job was stopped before this task"}, scope())
	if r.Status != "failed" || r.Reason != "the node job was stopped" {
		t.Fatalf("%s: %s", r.Status, r.Reason)
	}
}

func TestRetryPredicateTruncationFailsWithoutRepeatingCommand(t *testing.T) {
	count := 3
	task := config.Task{Module: "shell", ModuleInputs: &modules.Shell{Script: "head -c 1048577 /dev/zero"}, Retry: &config.Retry{MaxTries: config.TryLimit(count), PauseBetweenTries: "0s", Until: map[string]any{"contains": []any{"{{ .this_node_task.stdout }}", "ready"}}}}
	r := (&Engine{}).timed(context.Background(), task, scope())
	if r.Status != "failed" || r.Change != "changed" || len(r.Tries) != 1 || r.FailureKind != "config" {
		t.Fatalf("%s %s tries %d: %s", r.Status, r.Change, len(r.Tries), r.Reason)
	}
}

func TestRetryPredicateRejectsTruncatedHTTPBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write(append(bytes.Repeat([]byte("x"), 1<<20), "ready"...))
	}))
	defer server.Close()
	count := 3
	task := config.Task{Module: "http", ModuleInputs: &modules.HTTP{URL: server.URL}, Retry: &config.Retry{MaxTries: config.TryLimit(count), PauseBetweenTries: "0s", Until: map[string]any{"contains": []any{"{{ .this_node_task.response_body }}", "ready"}}}}
	r := (&Engine{}).timed(context.Background(), task, scope())
	if r.Status != "failed" || !r.ResponseBodyTruncated || len(r.Tries) != 1 || r.FailureKind != "config" {
		t.Fatalf("%s truncated=%v tries %d: %s", r.Status, r.ResponseBodyTruncated, len(r.Tries), r.Reason)
	}
}

// Polling keeps the first and latest tries and counts the rest, while the
// until predicate still reads each try's own output.
func TestPollingRetainsFirstAndLatestTries(t *testing.T) {
	count := 50
	counter := filepath.Join(t.TempDir(), "count")
	task := config.Task{Module: "shell", ModuleInputs: &modules.Shell{Script: `n=$(( $(cat "$F" 2>/dev/null || echo 0) + 1 )); echo $n > "$F"; echo "poll $n"`, Env: map[string]any{"F": counter}, ChangedWhen: false},
		Retry: &config.Retry{MaxTries: config.TryLimit(count), PauseBetweenTries: "0s", Until: map[string]any{"contains": []any{"{{ .this_node_task.stdout }}", "poll 40"}}}}
	r := (&Engine{}).timed(context.Background(), task, scope())
	n := len(r.Tries)
	if r.Status != "succeeded" || n != retainedTries || r.TriesOmitted != 40-retainedTries ||
		r.Tries[0].Stdout != "poll 1\n" || r.Tries[1].Stdout != "poll 32\n" || r.Tries[n-1].Stdout != "poll 40\n" {
		t.Fatalf("%s tries %d omitted %d: %+v", r.Status, n, r.TriesOmitted, r.Tries)
	}
}

// A missing prerequisite, such as a program an earlier task must install,
// fails at once: the node does not change between tries.
func TestMissingPrerequisiteIsNotRetried(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	count := 3
	e := Engine{}
	task := config.Task{Module: "git", ModuleInputs: &modules.Git{Repo: "https://example.com/app.git", FollowBranch: "main", Path: filepath.Join(t.TempDir(), "app")}, Retry: &config.Retry{MaxTries: config.TryLimit(count), PauseBetweenTries: "0s"}}
	r := e.timed(context.Background(), task, scope())
	if r.Status != "failed" || r.FailureKind != "missing_prerequisite" || len(r.Tries) > 1 {
		t.Fatal(r)
	}
}
