package assets

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
)

func TestDownloadHTTPSVerificationAndPublication(t *testing.T) {
	data := "#!/bin/sh\nprintf 'artifact\\n'\n"
	expected, _ := Hash(context.Background(), strings.NewReader(data))
	var hits atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		switch r.URL.Path {
		case "/redirect":
			http.Redirect(w, r, "/file", http.StatusFound)
		case "/loop":
			http.Redirect(w, r, "/loop", http.StatusFound)
		case "/downgrade":
			http.Redirect(w, r, "http://127.0.0.1:1/file", http.StatusFound)
		case "/credentials":
			http.Redirect(w, r, "https://user:password@127.0.0.1:1/file", http.StatusFound)
		case "/partial":
			w.Header().Set("Content-Length", "1000")
			io.WriteString(w, data)
		case "/missing":
			w.WriteHeader(http.StatusNotFound)
		default:
			if r.Header.Get("Accept-Encoding") != "identity" {
				t.Error("artifact request allows implicit decoding")
			}
			io.WriteString(w, data)
		}
	}))
	defer server.Close()
	for _, path := range []string{"/file", "/redirect"} {
		dir := t.TempDir()
		ref, err := download(context.Background(), dir, server.URL+path, strings.ToUpper(expected.SHA256), server.Client().Transport, StallTimeout)
		if err != nil || ref.Size != expected.Size || ref.SHA256 != expected.SHA256 {
			t.Fatal(ref, err)
		}
		if err := Verify(context.Background(), ref); err != nil {
			t.Fatal(err)
		}
		st, err := os.Stat(ref.Path)
		if err != nil || st.Mode().Perm() != 0600 {
			t.Fatal(st, err)
		}
	}
	// Without a digest the bytes are saved as they arrive, under their own SHA-256.
	unchecked, err := download(context.Background(), t.TempDir(), server.URL+"/file", "", server.Client().Transport, StallTimeout)
	if err != nil || unchecked.SHA256 != expected.SHA256 || Verify(context.Background(), unchecked) != nil {
		t.Fatal(unchecked, err)
	}
	// The public entry point for a checked download never takes an empty digest.
	if _, err := Download(context.Background(), t.TempDir(), server.URL+"/file", ""); err == nil {
		t.Fatal("a checked download accepted an empty digest")
	}
	for _, tc := range []struct {
		name, path, digest, message string
		transport                   http.RoundTripper
	}{
		{"checksum", "/file", strings.Repeat("0", 64), "SHA-256 mismatch", server.Client().Transport},
		{"partial", "/partial", expected.SHA256, "unexpected EOF", server.Client().Transport},
		{"missing", "/missing", expected.SHA256, "HTTP 404", server.Client().Transport},
		{"loop", "/loop", expected.SHA256, "redirects", server.Client().Transport},
		{"downgrade", "/downgrade", expected.SHA256, "HTTPS URL", server.Client().Transport},
		{"credentials", "/credentials", expected.SHA256, "credentials", server.Client().Transport},
		{"untrusted TLS", "/file", expected.SHA256, "certificate", http.DefaultTransport},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			_, err := download(context.Background(), dir, server.URL+tc.path, tc.digest, tc.transport, StallTimeout)
			if err == nil || !strings.Contains(err.Error(), tc.message) {
				t.Fatal(err)
			}
			entries, _ := os.ReadDir(dir)
			if len(entries) != 0 {
				t.Fatal("retained a failed download", entries)
			}
		})
	}
	before := hits.Load()
	for _, raw := range []string{"http://example.com/file", "https://user:pass@example.com/file", "https://example.com/{{ .vars.version }}", "https://example.com/file#part", "https:///file", "https://example.com:bad/file"} {
		if _, err := Download(context.Background(), t.TempDir(), raw, expected.SHA256); err == nil {
			t.Fatal("accepted", raw)
		}
	}
	if hits.Load() != before {
		t.Fatal("invalid input reached source")
	}
}

func TestDownloadIdleCancellationAndProgress(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, strings.Repeat("part", 50))
	}))
	defer server.Close()
	// Real HTTPS checks publication. Idle expiry and renewal use virtual
	// time below and in TestTransferProgressExtendsIdleDeadlineAndStallsCancel.
	expected, _ := Hash(context.Background(), strings.NewReader(strings.Repeat("part", 50)))
	ref, err := download(context.Background(), t.TempDir(), server.URL+"/progress", expected.SHA256, server.Client().Transport, StallTimeout)
	if err != nil || ref.Size != 200 {
		t.Fatal(ref, err)
	}
	for _, path := range []string{"/headers", "/body"} {
		for _, cancel := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/cancel=%v", path, cancel), func(t *testing.T) {
				dir := t.TempDir()
				synctest.Test(t, func(t *testing.T) {
					ctx := context.Background()
					idle := 100 * time.Millisecond
					if cancel {
						var done context.CancelFunc
						ctx, done = context.WithTimeout(ctx, 100*time.Millisecond)
						defer done()
						idle = time.Second
					}
					observed := make(chan struct{})
					transport := downloadTestTransport(func(request *http.Request) (*http.Response, error) {
						if path == "/headers" {
							close(observed)
							<-request.Context().Done()
							return nil, request.Context().Err()
						}
						return &http.Response{StatusCode: http.StatusOK, Body: &stalledDownloadBody{ctx: request.Context(), observed: observed}, Header: make(http.Header), Request: request}, nil
					})
					result := make(chan error, 1)
					go func() {
						_, err := download(ctx, dir, "https://example.test"+path, expected.SHA256, transport, idle)
						result <- err
					}()
					// For /body the reader signals only after returning partial bytes.
					select {
					case <-observed:
					case err := <-result:
						t.Fatal("download ended before the intended observation", err)
					}
					synctest.Wait()
					select {
					case err := <-result:
						t.Fatal("download ended before cancellation", err)
					default:
					}
					time.Sleep(100 * time.Millisecond)
					synctest.Wait()
					var err error
					select {
					case err = <-result:
					default:
						t.Fatal("download did not end at its controlled deadline")
					}
					if err == nil || (cancel && !errors.Is(err, context.DeadlineExceeded)) || (!cancel && !strings.Contains(err.Error(), "stalled")) {
						t.Fatal(err)
					}
					entries, _ := os.ReadDir(dir)
					if len(entries) != 0 {
						t.Fatal("retained partial download", entries)
					}
				})
			})
		}
	}
}

type downloadTestTransport func(*http.Request) (*http.Response, error)

func (transport downloadTestTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return transport(request)
}

type stalledDownloadBody struct {
	ctx      context.Context
	observed chan struct{}
	sent     bool
}

func (body *stalledDownloadBody) Read(p []byte) (int, error) {
	if !body.sent {
		body.sent = true
		return copy(p, "partial"), nil
	}
	close(body.observed)
	<-body.ctx.Done()
	return 0, body.ctx.Err()
}
func (*stalledDownloadBody) Close() error { return nil }
