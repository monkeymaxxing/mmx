package node

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mmx/internal/protocol"
)

// The node replaces one root-only JSON line per project and playbook, and
// refuses a name that is not a plain project or playbook name.
func TestRecordLastSuccessfulRunReplacesOneLine(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("node state requires root; exercised by the native Linux suite")
	}
	s := Server{Root: filepath.Join(t.TempDir(), "mmx")}
	record := func(project, playbook, runID string) error {
		return s.RecordLastSuccessfulRun(protocol.LastSuccessfulRunRequest{Project: project, Playbook: playbook, Record: protocol.LastSuccessfulRun{RunID: runID, Inventory: "production", Limit: "web01", FinishedAt: time.Date(2026, 10, 5, 14, 25, 10, 0, time.UTC)}})
	}
	for _, runID := range []string{"first", "second"} {
		if err := record("shop", "site", runID); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(s.Root, "last_successful_run", "shop", "site")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if want := `{"run_id":"second","inventory":"production","limit":"web01","only_fleet_stages":"","finished_at":"2026-10-05T14:25:10Z"}` + "\n"; string(b) != want {
		t.Fatalf("%s", b)
	}
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0600 {
		t.Fatal(info.Mode(), err)
	}
	// A one-time playbook has its record like any playbook.
	if err := record("shop", "2026-10-05_remove_old_ruby_manager", "one-time"); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(s.Root, "last_successful_run", "shop", "2026-10-05_remove_old_ruby_manager")); err != nil || !strings.Contains(string(b), `"run_id":"one-time"`) {
		t.Fatal(string(b), err)
	}
	for _, names := range [][2]string{{"../shop", "site"}, {"shop", "../site"}, {"shop", ""}, {"shop", "2026-10-05_../site"}, {"2026-10-05_shop", "site"}} {
		if err := record(names[0], names[1], "third"); err == nil || !strings.Contains(err.Error(), "invalid project or playbook name") {
			t.Fatal(names, err)
		}
	}
}
