package config

import (
	"fmt"
	"time"

	"mmx/internal/modules"
)

const DefaultHistoryRetention = "24h"

// HistoryRetention accepts ordinary Go durations, whole days, or forever.
// A zero duration in the return value means forever, never immediate deletion.
func HistoryRetention(value string) (time.Duration, error) {
	if value == "forever" {
		return 0, nil
	}
	duration, err := modules.ParseDurationWithDays(value)
	if err != nil {
		return 0, fmt.Errorf("run_history_retention must be a positive duration (24h, 7d) or forever")
	}
	return duration, nil
}
