package modules

import (
	"bufio"
	"context"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"mmx/internal/protocol"
	"mmx/internal/value"
)

// requestLog keeps what a test server received.
type requestLog struct {
	method, contentType, authorization, token, body string
}

func recordingServer(t *testing.T, status int, answer string) (*httptest.Server, *requestLog, *atomic.Int32) {
	t.Helper()
	got := &requestLog{}
	var count atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count.Add(1)
		b, _ := io.ReadAll(r.Body)
		*got = requestLog{r.Method, r.Header.Get("Content-Type"), r.Header.Get("Authorization"), r.Header.Get("X-Token"), string(b)}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(answer))
	}))
	t.Cleanup(server.Close)
	return server, got, &count
}

func TestHTTPWritesWithAJSONBodyHeadersAndBasicAuthentication(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	server, got, count := recordingServer(t, 201, `{"id": 7}`)
	s := value.Scope{"vars": map[string]any{"name": "alice"}, "secrets": map[string]any{"token": value.Sensitive("t0ken"), "root": value.Sensitive("r00t")}}
	h := HTTP{URL: server.URL + "/users", Method: "POST", ExpectedHTTPStatusCodes: []int{201},
		Headers:           map[string]any{"X-Token": "{{ .secrets.token }}"},
		BasicAuthUsername: "root", BasicAuthPassword: "{{ .secrets.root }}",
		JSONBody: map[string]any{"username": "{{ .vars.name }}", "admin": false}}
	if err := h.Prepare(""); err != nil {
		t.Fatal(err)
	}
	// A dryrun sends nothing and says it would change.
	preview := noMutation(t).HTTP(ctx, h, s, false)
	if preview.Status != "succeeded" || preview.Change != "would_change" || count.Load() != 0 {
		t.Fatalf("%+v %d", preview, count.Load())
	}
	mutations := 0
	r := (Runtime{BeforeMutation: func(string) error { mutations++; return nil }}).HTTP(ctx, h, s, false)
	if r.Status != "succeeded" || r.Change != "changed" || r.HTTPStatusCode != 201 || r.ResponseBody != `{"id": 7}` || mutations != 1 {
		t.Fatalf("%+v %d", r, mutations)
	}
	if got.method != "POST" || got.contentType != "application/json" || got.token != "t0ken" || got.body != `{"admin":false,"username":"alice"}` {
		t.Fatalf("%+v", *got)
	}
	if got.authorization != "Basic cm9vdDpyMDB0" { // root:r00t
		t.Fatal(got.authorization)
	}
	// Another status fails, and the request may have changed something.
	h.ExpectedHTTPStatusCodes = []int{200}
	if r := (Runtime{}).HTTP(ctx, h, s, false); r.Status != "failed" || r.Change != "changed" || r.Reason != "HTTP status 201; expected 200" {
		t.Fatalf("%+v", r)
	}
}

func TestHTTPSendsTheHostHeaderAsTheRequestsHost(t *testing.T) {
	t.Parallel()
	var host string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { host = r.Host }))
	t.Cleanup(server.Close)
	h := HTTP{URL: server.URL + "/health", Headers: map[string]any{"host": "shop.example.com"}}
	if err := h.Prepare(""); err != nil {
		t.Fatal(err)
	}
	if r := (Runtime{}).HTTP(context.Background(), h, value.Scope{}, false); r.Status != "succeeded" || host != "shop.example.com" {
		t.Fatalf("%+v %q", r, host)
	}
}

func TestHTTPChangedWhenReadsTheResponse(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	server, got, count := recordingServer(t, 200, `{"created": false}`)
	h := HTTP{URL: server.URL, Method: "PUT", Body: "plain text", ChangedWhen: map[string]any{"equals": []any{"{{ .this_node_task.response_body_json.created }}", true}}}
	if err := h.Prepare(""); err != nil {
		t.Fatal(err)
	}
	if r := (Runtime{}).HTTP(ctx, h, value.Scope{}, false); r.Status != "succeeded" || r.Change != "unchanged" || got.body != "plain text" || got.contentType != "" {
		t.Fatalf("%+v %+v", r, *got)
	}
	// A dryrun cannot know what the response would say.
	if r := noMutation(t).HTTP(ctx, h, value.Scope{}, false); r.Status != "not_checked" || count.Load() != 1 {
		t.Fatalf("%+v", r)
	}
	h.ChangedWhen = false
	if r := noMutation(t).HTTP(ctx, h, value.Scope{}, false); r.Status != "succeeded" || r.Change != "unchanged" || count.Load() != 1 {
		t.Fatalf("%+v", r)
	}
	// A read reports unchanged unless changed_when says otherwise, and a
	// dryrun sends it.
	read := HTTP{URL: server.URL}
	if r := noMutation(t).HTTP(ctx, read, value.Scope{}, false); r.Status != "succeeded" || r.Change != "unchanged" || got.method != "GET" || count.Load() != 2 {
		t.Fatalf("%+v", r)
	}
	read.ChangedWhen = true
	if r := (Runtime{}).HTTP(ctx, read, value.Scope{}, false); r.Change != "changed" {
		t.Fatalf("%+v", r)
	}
	// Inside a node task block with proposed changes, a dryrun skips a read.
	if r := noMutation(t).HTTP(ctx, read, value.Scope{}, true); r.Status != "skipped" || count.Load() != 3 {
		t.Fatalf("%+v %d", r, count.Load())
	}
}

// Not beside other tests: once this test closes its server, a server of
// another test could get the same port and answer.
func TestHTTPWithoutAnAnswerMayHaveChangedSomething(t *testing.T) {
	server, _, _ := recordingServer(t, 200, "")
	server.Close()
	if r := (Runtime{}).HTTP(context.Background(), HTTP{URL: server.URL, Method: "DELETE"}, value.Scope{}, false); r.Status != "failed" || r.Change != "unknown" {
		t.Fatalf("%+v", r)
	}
	if r := (Runtime{}).HTTP(context.Background(), HTTP{URL: server.URL}, value.Scope{}, false); r.Status != "failed" || r.Change != "unchanged" {
		t.Fatalf("%+v", r)
	}
}

func TestHTTPTrustsTheNamedCertificateAuthority(t *testing.T) {
	t.Parallel()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ok")) }))
	defer server.Close()
	h := HTTP{URL: server.URL}
	// The test server's certificate is self-signed: without the file, the
	// request fails.
	if r := (Runtime{}).HTTP(context.Background(), h, value.Scope{}, false); r.Status != "failed" || !strings.Contains(r.Reason, "certificate") {
		t.Fatalf("%+v", r)
	}
	file := filepath.Join(t.TempDir(), "ca.pem")
	certificate := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	if err := os.WriteFile(file, certificate, 0644); err != nil {
		t.Fatal(err)
	}
	w := WaitFor{URLResponds: server.URL, TrustedCertificateAuthorityFile: file}
	if r := (Runtime{}).WaitFor(context.Background(), w, nil); r.Status != "succeeded" || r.ResponseBody != "ok" {
		t.Fatalf("wait_for with private CA: %+v", r)
	}
	h.TrustedCertificateAuthorityFile = file
	if r := (Runtime{}).HTTP(context.Background(), h, value.Scope{}, false); r.Status != "succeeded" || r.ResponseBody != "ok" {
		t.Fatalf("%+v", r)
	}
	if err := os.WriteFile(file, []byte("not a certificate"), 0644); err != nil {
		t.Fatal(err)
	}
	if r := (Runtime{}).HTTP(context.Background(), h, value.Scope{}, false); r.Status != "failed" || !strings.Contains(r.Reason, "holds no PEM certificate") {
		t.Fatalf("%+v", r)
	}
}

func TestHTTPRejectedInputs(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		h    HTTP
		want string
	}{
		{HTTP{URL: "http://x", Method: "post"}, "written in capitals"},
		{HTTP{URL: "http://x", Method: "OPTIONS"}, "method must be one of"},
		{HTTP{URL: "http://x", Body: "x"}, "needs method POST, PUT, PATCH, or DELETE"},
		{HTTP{URL: "http://x", Method: "POST", Body: "x", JSONBody: map[string]any{}}, "not json_body and body together"},
		{HTTP{URL: "http://x", BasicAuthUsername: "root"}, "set basic_auth_username and basic_auth_password together"},
		{HTTP{URL: "http://x", Headers: map[string]any{"Bad Header": "x"}}, "invalid HTTP header name"},
		{HTTP{URL: "http://x", BasicAuthUsername: "a", BasicAuthPassword: "b", Headers: map[string]any{"authorization": "x"}}, "do not also set Authorization in headers"},
	} {
		if err := c.h.Prepare(""); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%+v: %v", c.h, err)
		}
	}
	if err := (&HTTP{URL: "http://x", Method: "POST"}).OnlyReads(); err == nil {
		t.Error("a POST only reads")
	}
	if err := (&HTTP{URL: "http://x", Method: "HEAD"}).OnlyReads(); err != nil {
		t.Error(err)
	}
	if err := headerValue("a\nb"); err == nil {
		t.Error("a header value with a line break")
	}
}

// Virtual time proves a response beyond ten seconds can finish, while the
// caller's deadline still stops the request. net.Pipe supplies real HTTP bytes.
func TestHTTPRequestUsesTheTaskDeadline(t *testing.T) {
	original := http.DefaultTransport
	defer func() { http.DefaultTransport = original }()
	for _, test := range []struct {
		module string
		limit  time.Duration
	}{{"http", 12 * time.Second}, {"http", 5 * time.Second}, {"wait_for", 12 * time.Second}, {"wait_for", 5 * time.Second}} {
		limit := test.limit
		t.Run(test.module+" "+limit.String(), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), limit)
				defer cancel()
				var servers sync.WaitGroup
				defer servers.Wait()
				http.DefaultTransport = &http.Transport{DialContext: func(context.Context, string, string) (net.Conn, error) {
					client, server := net.Pipe()
					servers.Add(1)
					go func() {
						defer servers.Done()
						defer server.Close()
						request, err := http.ReadRequest(bufio.NewReader(server))
						if err != nil {
							return
						}
						request.Body.Close()
						// The server answers after 11 seconds whatever the client does,
						// so only the client's deadline can end a shorter request.
						time.Sleep(11 * time.Second)
						fmt.Fprint(server, "HTTP/1.1 200 OK\r\nContent-Length: 5\r\nConnection: close\r\n\r\nready")
					}()
					return client, nil
				}}
				started := time.Now()
				var r protocol.Result
				if test.module == "http" {
					r = (Runtime{}).HTTP(ctx, HTTP{URL: "http://example.test/health"}, nil, false)
				} else {
					r = (Runtime{}).WaitFor(ctx, WaitFor{URLResponds: "http://example.test/health", WaitAtMost: limit.String()}, nil)
				}
				if test.module == "http" && limit > 11*time.Second {
					if r.Status != "succeeded" || r.ResponseBody != "ready" || time.Since(started) != 11*time.Second {
						t.Fatalf("%+v after %s", r, time.Since(started))
					}
				} else if r.Status != "failed" || !strings.Contains(r.Reason, "deadline exceeded") || time.Since(started) != limit {
					t.Fatalf("%+v after %s", r, time.Since(started))
				}
			})
		})
	}
}

func TestHTTPAndWaitForCanExplicitlySkipServerCertificateChecks(t *testing.T) {
	t.Parallel()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "ready") }))
	defer server.Close()
	for _, skip := range []bool{false, true} {
		h := HTTP{URL: server.URL, DangerouslySkipServerCertificateCheck: skip}
		r := (Runtime{}).HTTP(context.Background(), h, nil, false)
		want := "failed"
		if skip {
			want = "succeeded"
		}
		if r.Status != want {
			t.Fatalf("http skip=%t: %+v", skip, r)
		}
		if !skip {
			for _, setting := range []string{"x509:", "trusted_certificate_authority_file", "dangerously_skip_server_certificate_check: true"} {
				if !strings.Contains(r.Reason, setting) {
					t.Errorf("http certificate failure is missing %q: %s", setting, r.Reason)
				}
			}
		}
		w := WaitFor{URLResponds: server.URL, DangerouslySkipServerCertificateCheck: skip}
		r = (Runtime{Dryrun: true}).WaitFor(context.Background(), w, nil)
		if !skip {
			want = "not_checked"
		}
		if r.Status != want {
			t.Fatalf("wait_for skip=%t: %+v", skip, r)
		}
		if !skip {
			for _, setting := range []string{"x509:", "trusted_certificate_authority_file", "dangerously_skip_server_certificate_check: true"} {
				if !strings.Contains(r.Reason, setting) {
					t.Errorf("wait_for certificate failure is missing %q: %s", setting, r.Reason)
				}
			}
		}
	}
}
