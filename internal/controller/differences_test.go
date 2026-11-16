package controller

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"io"
	"net/http"
	"strings"
	"testing"

	"mmx/internal/assets"
	"mmx/internal/differences"
	"mmx/internal/endpoint"
	"mmx/internal/protocol"

	"golang.org/x/crypto/ssh"
)

// A finished result's full differences are downloaded and verified; a failed
// download is housekeeping and leaves the task outcome alone.
func TestFetchDiffsVerifiesArtifacts(t *testing.T) {
	ctx := context.Background()
	text := "--- before\n+++ after\n-old\n+new\n"
	ref, _ := assets.Hash(ctx, strings.NewReader(text))
	c := Controller{State: t.TempDir()}
	r := &Run{ID: "run"}
	en := Entry{JobID: "job", Response: protocol.Response{ID: "job", Status: "succeeded", Results: []protocol.Result{{FileChange: &protocol.FileChange{FullDiff: &ref}}}}}
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	signer, _ := ssh.NewSignerFromKey(key)
	client := endpoint.NewClient("https://fixture", "admin", "pin", []ssh.Signer{signer})
	broken := true
	client.HTTP.Transport = transportFunc(func(req *http.Request) (*http.Response, error) {
		body := ""
		if strings.HasPrefix(req.URL.Path, "/v1/diffs/") {
			body = text
			if broken {
				body = strings.Repeat("!", len(text))
			}
		}
		return &http.Response{StatusCode: 200, ContentLength: int64(len(body)), Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	})
	conn := &Connection{direct: client}
	got := c.fetchDiffs(ctx, r, en, conn)
	if got.Response.Status != "succeeded" || len(got.Housekeeping) != 1 || assets.Verify(ctx, differences.Local(c.DiffDir(r.ID), ref)) == nil {
		t.Fatal(got)
	}
	broken = false
	got = c.fetchDiffs(ctx, r, en, conn)
	if len(got.Housekeeping) != 0 {
		t.Fatal(got)
	}
	if err := assets.Verify(ctx, differences.Local(c.DiffDir(r.ID), ref)); err != nil {
		t.Fatal(err)
	}
}
