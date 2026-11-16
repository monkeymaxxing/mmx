package controller

import (
	"sort"

	"mmx/internal/facts"
)

// Compare observations already retained with this run, including retry attempts.
// This adds no persistent node inventory or cross-run recovery state.
func explainFactChanges(entries []Entry) {
	type observed struct {
		snapshot *facts.Snapshot
		entry    int
	}
	nodes := map[string][]observed{}
	for i, en := range entries {
		for _, attempt := range en.PreviousNodeJobAttempts {
			if attempt.Response.Facts != nil {
				nodes[en.Node] = append(nodes[en.Node], observed{attempt.Response.Facts, -1})
			}
		}
		if en.Response.Facts != nil {
			nodes[en.Node] = append(nodes[en.Node], observed{en.Response.Facts, i})
		}
	}
	for _, observations := range nodes {
		sort.SliceStable(observations, func(i, j int) bool {
			return observations[i].snapshot.CollectedAt.Before(observations[j].snapshot.CollectedAt)
		})
		for i := 1; i < len(observations); i++ {
			// A non-nil list (including empty) preserves the full-run comparison
			// when logs later filters entries.
			if at := observations[i].entry; at >= 0 && entries[at].FactsChanged == nil {
				entries[at].FactsChanged = facts.Changes(observations[i-1].snapshot, observations[i].snapshot)
			}
		}
	}
}
