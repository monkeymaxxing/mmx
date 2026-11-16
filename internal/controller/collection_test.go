package controller

import (
	"reflect"
	"testing"

	"mmx/internal/protocol"
)

func TestPollingRetainsHousekeepingWithoutChangingOutcome(t *testing.T) {
	before := protocol.Response{Status: "running", Housekeeping: []string{"cleanup failed"}}
	after := withHousekeeping(before, protocol.Response{Status: "succeeded", Housekeeping: []string{"cleanup failed", "another cleanup"}})
	if after.Status != "succeeded" || !reflect.DeepEqual(after.Housekeeping, []string{"cleanup failed", "another cleanup"}) {
		t.Fatal(after)
	}
}
