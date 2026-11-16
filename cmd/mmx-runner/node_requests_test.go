package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"mmx/internal/node"
	"mmx/internal/protocol"
)

// Every JSON node request is an mmx-runner command over SSH and reads up to
// protocol.RequestLimit bytes, the limit the node HTTPS API process applies
// (internal/endpoint's TestEveryNodeRequestReadsTheSameLimitOverHTTPS). A
// body of exactly the limit reaches the node and fails only as JSON; one byte
// more is refused as too large.
func TestEveryNodeRequestReadsTheSameLimitOverSSH(t *testing.T) {
	body := bytes.Repeat([]byte(" "), protocol.RequestLimit+1)
	server := node.Server{Root: t.TempDir()}
	for _, name := range node.RequestNames() {
		err := answerOverSSH(context.Background(), server, name, bytes.NewReader(body[:protocol.RequestLimit]), io.Discard)
		if err == nil || errors.Is(err, protocol.ErrRequestLimit) || errors.Is(err, node.ErrUnknownRequest) {
			t.Fatalf("%s with %d bytes: %v", name, protocol.RequestLimit, err)
		}
		err = answerOverSSH(context.Background(), server, name, bytes.NewReader(body), io.Discard)
		if !errors.Is(err, protocol.ErrRequestLimit) {
			t.Fatalf("%s with %d bytes: %v", name, protocol.RequestLimit+1, err)
		}
	}
}
