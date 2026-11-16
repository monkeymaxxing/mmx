package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"mmx/internal/config"
	"mmx/internal/facts"
	"mmx/internal/modules"
	"mmx/internal/value"
)

func TestHTTPTriesOnceUnlessRetryIsSet(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(503)
			fmt.Fprint(w, "starting")
			return
		}
		fmt.Fprint(w, "ready")
	}))
	defer server.Close()
	for _, retry := range []string{"", "retry: true", "retry: {}", "retry: false"} {
		t.Run(retry, func(t *testing.T) {
			calls.Store(0)
			dir := t.TempDir()
			for _, folder := range []string{"inventories", "playbooks"} {
				if err := os.Mkdir(filepath.Join(dir, folder), 0700); err != nil {
					t.Fatal(err)
				}
			}
			put(t, filepath.Join(dir, "mmx_project_settings.yml"), "project_name: test\ndefault_inventory: example\nnode_task_defaults:\n  retry: {max_tries: 3, pause_between_tries: 0s}\n")
			put(t, filepath.Join(dir, "inventories/example.yml"), "nodes: {test: {address: localhost}}\n")
			put(t, filepath.Join(dir, "playbooks/example.yml"), "fleet_stages:\n- id: setup\n  nodes: all\n  node_tasks:\n  - id: readiness\n    http: {url: '"+server.URL+"'}\n    "+retry+"\n")
			p, _, err := config.Load(filepath.Join(dir, "mmx_project_settings.yml"), "example", "", nil)
			if err != nil {
				t.Fatal(err)
			}
			task := p.FleetStages[0].NodeTasks[0]
			result := (&Engine{}).timed(context.Background(), task, scope())
			repeating := retry == "retry: true" || retry == "retry: {}"
			if repeating {
				if result.Status != "succeeded" || calls.Load() != 2 || len(result.Tries) != 2 || result.ResponseBody != "ready" {
					t.Fatal(result, calls.Load())
				}
			} else if result.Status != "failed" || calls.Load() != 1 || len(result.Tries) != 0 || result.HTTPStatusCode != 503 {
				t.Fatal(result, calls.Load())
			}
		})
	}
}

func TestHTTPAndWaitForPublishNodeTaskValues(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "ready") }))
	defer server.Close()
	for _, first := range []config.Task{
		{ID: "probe", Module: "http", ModuleInputs: &modules.HTTP{URL: server.URL}},
		{ID: "probe", Module: "wait_for", ModuleInputs: &modules.WaitFor{URLResponds: server.URL, WaitAtMost: "30s"}},
	} {
		j := job(first, config.Task{Show: "{{ .node_tasks.probe.response_body }}"}, config.Task{Module: "assert", ModuleInputs: &modules.Assert{That: map[string]any{"equals": []any{"{{ .node_tasks.probe.http_status_code }}", 200}}}})
		e := Engine{RuntimeRoot: t.TempDir(), collectFacts: func(string) facts.Snapshot { return facts.Snapshot{Values: map[string]any{}} }}
		r := e.Run(context.Background(), j)
		requireStatus(t, r, "succeeded")
		if r.Results[1].Value != "ready" || r.Results[0].Change != "unchanged" {
			t.Fatal(r)
		}
	}
}

// A request that sends a secret has its response suppressed, like any task
// that reads a secret.
func TestHTTPWithASecretHeaderHasASecretResult(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(201)
		fmt.Fprint(w, "created with "+r.Header.Get("X-Token"))
	}))
	defer server.Close()
	j := job(config.Task{ID: "create", Module: "http", ModuleInputs: &modules.HTTP{URL: server.URL, Method: "POST", ExpectedHTTPStatusCodes: []int{201},
		Headers: map[string]any{"X-Token": "{{ .secrets.token }}"}}})
	j.Scope = map[string]any{"secrets": map[string]any{"token": value.Sensitive("token-sentinel")}}
	e := Engine{RuntimeRoot: t.TempDir(), collectFacts: func(string) facts.Snapshot { return facts.Snapshot{Values: map[string]any{}} }}
	r := e.Run(context.Background(), j)
	requireStatus(t, r, "succeeded")
	saved, _ := json.Marshal(r)
	if !r.Results[0].Secret || r.Results[0].Change != "changed" || strings.Contains(string(saved), "token-sentinel") {
		t.Fatal(string(saved))
	}
}

func TestTimeLimitPerTryAllowsAnotherTry(t *testing.T) {
	perTry := "50ms"
	r := (&Engine{}).timed(context.Background(), config.Task{Module: "shell", ModuleInputs: &modules.Shell{Script: "sleep 10"}, TimeLimitPerTry: &perTry, Retry: &config.Retry{MaxTries: 2, PauseBetweenTries: "0s"}}, scope())
	if r.Status != "failed" || len(r.Tries) != 2 {
		t.Fatal(r)
	}
}

func TestWaitForDryrunInVerifyChecksOnce(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(503) }))
	defer server.Close()
	j := job(config.Task{TaskBlock: &config.TaskBlock{ID: "configure", Changes: []config.Task{taskFile(filepath.Join(t.TempDir(), "config"), "new")}, Verify: []config.Task{{Module: "wait_for", ModuleInputs: &modules.WaitFor{URLResponds: server.URL, WaitAtMost: "30s"}}}}})
	j.Dryrun = true
	e := Engine{RuntimeRoot: t.TempDir(), collectFacts: func(string) facts.Snapshot { return facts.Snapshot{Values: map[string]any{}} }}
	start := time.Now()
	r := e.Run(context.Background(), j)
	if calls.Load() != 1 || time.Since(start) > 3*time.Second {
		t.Fatal(r, calls.Load())
	}
	found := false
	for _, result := range r.Results {
		if strings.Contains(result.Task, "/verify/") {
			found = true
			if result.Status != "not_checked" || result.Change != "unchanged" {
				t.Fatal(result)
			}
		}
	}
	if !found {
		t.Fatal("verify not recorded", r)
	}
}

func TestWaitForResponseBodyContainsResolvesReferences(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "checked-in example\n") }))
	defer server.Close()
	expected := "{{ .vars.message }}\n"
	j := job(config.Task{Module: "wait_for", ModuleInputs: &modules.WaitFor{URLResponds: server.URL, ResponseBodyContains: &expected, WaitAtMost: "30s"}})
	j.Vars = map[string]any{"message": "checked-in example"}
	e := Engine{RuntimeRoot: t.TempDir(), collectFacts: func(string) facts.Snapshot { return facts.Snapshot{Values: map[string]any{}} }}
	requireStatus(t, e.Run(context.Background(), j), "succeeded")
}
