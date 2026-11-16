package config

import (
	"testing"
	"time"
)

func TestHistoryRetention(t *testing.T) {
	for value, want := range map[string]time.Duration{"24h": 24 * time.Hour, "90m": 90 * time.Minute, "7d": 7 * 24 * time.Hour, "30d": 30 * 24 * time.Hour, "forever": 0} {
		got, err := HistoryRetention(value)
		if err != nil || got != want {
			t.Fatalf("%s: %s, %v", value, got, err)
		}
	}
	for _, value := range []string{"", "0", "0s", "-1h", "-1d", "1.5d", "later", "999999999999999999d", "9223372036854775808d"} {
		if _, err := HistoryRetention(value); err == nil {
			t.Fatalf("accepted %q", value)
		}
	}
	fleetStages := "fleet_stages: [{id: setup, nodes: all, node_tasks: [{show: ready}]}]\n"
	p, _, err := Load(fixture(t, fleetStages), "example", "", nil)
	if err != nil || p.HistoryRetention != DefaultHistoryRetention {
		t.Fatalf("default: %+v, %v", p, err)
	}
	for _, value := range []string{"null", "0", "true", "\"\"", "-24h"} {
		if _, _, err := Load(fixture(t, "run_history_retention: "+value+"\n"+fleetStages), "example", "", nil); err == nil {
			t.Fatalf("accepted YAML retention %s", value)
		}
	}
}
