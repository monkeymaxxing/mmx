package node

import (
	"runtime"
	"strings"
	"testing"

	"mmx/internal/protocol"
)

func TestFactsAnswerWithTheNodesFactsAndChangeNothing(t *testing.T) {
	s := Server{Root: t.TempDir()}
	snapshot, err := s.Facts(protocol.FactsRequest{Node: "web01"})
	if err != nil || snapshot.Values["name"] != "web01" || snapshot.Values["arch"] != runtime.GOARCH || snapshot.CollectedAt.IsZero() {
		t.Fatal(snapshot, err)
	}
	if _, err := s.Facts(protocol.FactsRequest{Node: "../web01"}); err == nil || !strings.Contains(err.Error(), "node's name") {
		t.Fatal(err)
	}
}
