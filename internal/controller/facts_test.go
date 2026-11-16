package controller

import (
	"mmx/internal/facts"
	"mmx/internal/protocol"
	"testing"
	"time"
)

func TestFactComparisonUsesSameNodeAndPreviousAttemptInTimeOrder(t *testing.T) {
	snapshot := func(second int, boot string) protocol.Response {
		return protocol.Response{Facts: &facts.Snapshot{CollectedAt: time.Unix(int64(second), 0), Values: map[string]any{"boot_id": boot}}}
	}
	entries := []Entry{
		{Node: "a", Response: snapshot(3, "second"), PreviousNodeJobAttempts: []Attempt{{Response: snapshot(2, "first")}}},
		{Node: "a", Response: snapshot(1, "first")},
		{Node: "b", Response: snapshot(2, "unrelated")},
		{Node: "a", Response: snapshot(4, "second")},
	}
	explainFactChanges(entries)
	if len(entries[0].FactsChanged) != 1 || entries[0].FactsChanged[0] != "/node/boot_id" {
		t.Fatal(entries)
	}
	for _, i := range []int{1, 2, 3} {
		if len(entries[i].FactsChanged) != 0 {
			t.Fatal(entries[i])
		}
	}
	if entries[0].PreviousNodeJobAttempts[0].Response.Facts.Values["boot_id"] != "first" {
		t.Fatal("changed stored observation")
	}
}

func TestFactComparisonSurvivesFilteringOtherFleetStages(t *testing.T) {
	snapshot := func(second int, cpu, ram int) protocol.Response {
		return protocol.Response{Facts: &facts.Snapshot{CollectedAt: time.Unix(int64(second), 0), Values: map[string]any{"cpu": cpu, "memory": ram}}}
	}
	entries := []Entry{
		{Node: "a", Response: snapshot(3, 4, 8), PreviousNodeJobAttempts: []Attempt{{Response: snapshot(1, 2, 4)}}},
		{Node: "a", Response: snapshot(2, 4, 4)},
	}
	explainFactChanges(entries)
	filtered := entries[:1]
	explainFactChanges(filtered)
	if len(filtered[0].FactsChanged) != 1 || filtered[0].FactsChanged[0] != "/node/memory" {
		t.Fatal(filtered)
	}
}

func TestUnchangedFactComparisonSurvivesFiltering(t *testing.T) {
	response := func(second int, boot string) protocol.Response {
		return protocol.Response{Facts: &facts.Snapshot{CollectedAt: time.Unix(int64(second), 0), Values: map[string]any{"boot_id": boot}}}
	}
	entries := []Entry{
		{Node: "a", Response: response(3, "current"), PreviousNodeJobAttempts: []Attempt{{Response: response(1, "old")}}},
		{Node: "a", Response: response(2, "current")},
	}
	explainFactChanges(entries)
	filtered := entries[:1]
	explainFactChanges(filtered)
	if filtered[0].FactsChanged == nil || len(filtered[0].FactsChanged) != 0 {
		t.Fatal(filtered)
	}
}
