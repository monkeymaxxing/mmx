package modules

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A cron_job named like a file that another tool wrote must not overwrite it.
func TestCronJobRefusesToOverwriteAnUnmarkedFile(t *testing.T) {
	ctx := context.Background()
	c := cronFixture(t)
	c.Name = "certbot"
	other := c.Path()
	const original = "0 */12 * * * root certbot -q renew\n"
	if err := os.WriteFile(other, []byte(original), 0644); err != nil {
		t.Fatal(err)
	}
	for _, dryrun := range []bool{true, false} {
		r := (Runtime{Dryrun: dryrun}).CronJob(ctx, c, cronScope)
		if r.Status != "failed" || !strings.Contains(r.Reason, "refusing to write "+other+": the file exists and lacks the MMX marker line") || !strings.Contains(r.Reason, "Choose another name") {
			t.Fatal(dryrun, r)
		}
	}
	if got, _ := os.ReadFile(other); string(got) != original {
		t.Fatal("the file was changed", string(got))
	}
}

// A systemd_scheduled_job named like a unit that another tool or a package
// provides must not overwrite or shadow it, in dryrun or in a run.
func TestSystemdScheduledJobRefusesForeignUnits(t *testing.T) {
	// Not parallel: the test changes the unit directories.
	etc, vendor := t.TempDir(), t.TempDir()
	savedEtc, savedVendor := scheduledJobUnitDirectory, vendorUnitDirectories
	scheduledJobUnitDirectory, vendorUnitDirectories = etc, []string{vendor}
	t.Cleanup(func() { scheduledJobUnitDirectory, vendorUnitDirectories = savedEtc, savedVendor })
	write := func(path, text string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(text), 0644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(etc, "backup.timer"), "[Timer]\nOnCalendar=daily\n")
	write(filepath.Join(vendor, "logrotate.timer"), "[Timer]\nOnCalendar=daily\n")
	for _, c := range []struct{ name, want string }{
		{"backup", "refusing to write " + filepath.Join(etc, "backup.timer") + ": the file exists and lacks the MMX marker line"},
		{"logrotate", "the system already has the unit " + filepath.Join(vendor, "logrotate.timer")},
	} {
		j := SystemdScheduledJob{User: "root", Name: c.name, Script: "true", CronSchedule: "0 1 * * *"}
		for _, dryrun := range []bool{true, false} {
			r := (Runtime{Dryrun: dryrun}).SystemdScheduledJob(testContext(t), j, nil)
			if r.Status != "failed" || !strings.Contains(r.Reason, c.want) || !strings.Contains(r.Reason, "Choose another name") {
				t.Fatal(c.name, dryrun, r)
			}
		}
	}
	// A unit with the marker line is the job's own.
	write(filepath.Join(etc, "mine.timer"), markerLine("systemd_scheduled_job", "mine")+"[Timer]\n")
	if err := refuseForeignUnits(SystemdScheduledJob{Name: "mine"}); err != nil {
		t.Fatal(err)
	}
}
