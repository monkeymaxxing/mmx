package modules

import (
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"

	"mmx/internal/value"
)

// cronFixture points cron_job at a temporary directory owned by the test's
// account, with a stand-in cron program on PATH, and returns a job for that
// account.
func cronFixture(t *testing.T) CronJob {
	t.Helper()
	me, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	group, err := user.LookupGroupId(me.Gid)
	if err != nil {
		t.Fatal(err)
	}
	dir := traversableTempDir(t)
	directory, bin := filepath.Join(dir, "cron.d"), filepath.Join(dir, "bin")
	for _, d := range []string{directory, bin} {
		if err := os.Mkdir(d, 0755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(bin, "cron"), []byte("#!/bin/sh\n"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	saved := cronFiles
	t.Cleanup(func() { cronFiles = saved })
	cronFiles.directory, cronFiles.owner, cronFiles.group = directory, Account(me.Username), Account(group.Name)
	return CronJob{Name: "rotate-upload-cache", CronSchedule: "15 3 * * *", User: me.Username,
		Command: "/usr/local/bin/rotate --keep 14 --suffix $(date +%Y-%m-%d)",
		Env:     map[string]any{"MAILTO": "ops@example.com", "GREETING": "  hello  ", "PATH": "{{ .vars.path }}"}}
}

var cronScope = value.Scope{"vars": map[string]any{"path": "/usr/local/bin:/usr/bin:/bin"}}

func TestCronJobWritesOneMarkedFile(t *testing.T) {
	ctx := context.Background()
	c := cronFixture(t)
	preview := (Runtime{Dryrun: true}).CronJob(ctx, c, cronScope)
	if preview.Status != "succeeded" || preview.Change != "would_change" || preview.FileChange == nil || preview.FileChange.Action != "create" {
		t.Fatal(preview)
	}
	if _, err := os.Stat(c.Path()); !os.IsNotExist(err) {
		t.Fatal("dryrun wrote the cron file", err)
	}
	if r := (Runtime{}).CronJob(ctx, c, cronScope); r.Status != "succeeded" || r.Change != "changed" {
		t.Fatal(r)
	}
	b, err := os.ReadFile(c.Path())
	if err != nil {
		t.Fatal(err)
	}
	want := "# Managed by MMX: cron_job. Written by the node task for rotate-upload-cache; MMX replaces a change made here on its next run.\n" +
		"GREETING=\"  hello  \"\nMAILTO=ops@example.com\nPATH=/usr/local/bin:/usr/bin:/bin\n" +
		"15 3 * * * " + c.User + ` /usr/local/bin/rotate --keep 14 --suffix $(date +\%Y-\%m-\%d)` + "\n"
	if string(b) != want {
		t.Fatalf("cron file:\n%s\nwant:\n%s", b, want)
	}
	if mode(t, c.Path()) != 0644 {
		t.Fatal(mode(t, c.Path()))
	}
	for _, dryrun := range []bool{false, true} {
		if r := (Runtime{Dryrun: dryrun}).CronJob(ctx, c, cronScope); r.Status != "succeeded" || r.Change != "unchanged" {
			t.Fatal(dryrun, r)
		}
	}
	// A change made by hand is shown in a dryrun and replaced by a run.
	if err := os.WriteFile(c.Path(), []byte(strings.Replace(want, "15 3", "0 4", 1)), 0644); err != nil {
		t.Fatal(err)
	}
	preview = (Runtime{Dryrun: true}).CronJob(ctx, c, cronScope)
	if preview.Change != "would_change" || !strings.Contains(preview.FileChange.Diff, "-0 4 * * *") || !strings.Contains(preview.FileChange.Diff, "+15 3 * * *") {
		t.Fatal(preview, preview.FileChange)
	}
	if r := (Runtime{}).CronJob(ctx, c, cronScope); r.Change != "changed" {
		t.Fatal(r)
	}
	if b, _ := os.ReadFile(c.Path()); string(b) != want {
		t.Fatal(string(b))
	}
	// cron's @ words are written as they are.
	c.CronSchedule = "@daily"
	if r := (Runtime{}).CronJob(ctx, c, cronScope); r.Change != "changed" {
		t.Fatal(r)
	}
	if b, _ := os.ReadFile(c.Path()); !strings.Contains(string(b), "\n@daily "+c.User+" ") {
		t.Fatal(string(b))
	}
}

func TestCronJobRemovesOnlyAMarkedFile(t *testing.T) {
	ctx := context.Background()
	c := cronFixture(t)
	if r := (Runtime{}).CronJob(ctx, c, cronScope); r.Change != "changed" {
		t.Fatal(r)
	}
	absent := CronJob{Name: c.Name, State: "absent"}
	if r := (Runtime{Dryrun: true}).CronJob(ctx, absent, nil); r.Change != "would_change" {
		t.Fatal(r)
	}
	if _, err := os.Stat(c.Path()); err != nil {
		t.Fatal("dryrun removed the file", err)
	}
	if r := (Runtime{}).CronJob(ctx, absent, nil); r.Status != "succeeded" || r.Change != "changed" {
		t.Fatal(r)
	}
	if _, err := os.Stat(c.Path()); !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if r := (Runtime{}).CronJob(ctx, absent, nil); r.Status != "succeeded" || r.Change != "unchanged" {
		t.Fatal(r)
	}
	other := filepath.Join(cronFiles.directory, "backup")
	if err := os.WriteFile(other, []byte("0 1 * * * root /usr/local/bin/backup\n"), 0644); err != nil {
		t.Fatal(err)
	}
	for _, dryrun := range []bool{true, false} {
		r := (Runtime{Dryrun: dryrun}).CronJob(ctx, CronJob{Name: "backup", State: "absent"}, nil)
		if r.Status != "failed" || !strings.Contains(r.Reason, "refusing to remove "+other+": the file lacks the MMX marker line \"# Managed by MMX: cron_job. ...\"") {
			t.Fatal(dryrun, r)
		}
	}
	if _, err := os.Stat(other); err != nil {
		t.Fatal("a file without the marker line was removed", err)
	}
}

func TestCronJobWaitsForCronAndTheAccount(t *testing.T) {
	ctx := context.Background()
	c := cronFixture(t)
	cronFiles.directory = filepath.Join(t.TempDir(), "missing")
	if r := (Runtime{Dryrun: true}).CronJob(ctx, c, cronScope); r.Status != "not_checked" || !strings.Contains(r.Reason, missingProgramError("cron").Error()) {
		t.Fatal(r)
	}
	if r := (Runtime{}).CronJob(ctx, c, cronScope); r.Status != "failed" || !strings.Contains(r.Reason, "cron is not installed") {
		t.Fatal(r)
	}
	c = cronFixture(t)
	c.User = "mmx-no-such-account"
	if r := (Runtime{Dryrun: true}).CronJob(ctx, c, cronScope); r.Status != "not_checked" {
		t.Fatal(r)
	}
	if r := (Runtime{}).CronJob(ctx, c, cronScope); r.Status != "failed" || !strings.Contains(r.Reason, "cron_job.run_as_user_account") {
		t.Fatal(r)
	}
}

// A run stopped at the mutation boundary writes nothing, and the next try
// writes the file.
func TestCronJobAfterAFailedTry(t *testing.T) {
	ctx := context.Background()
	c := cronFixture(t)
	stopped := Runtime{BeforeMutation: func(string) error { return errors.New("stopped") }}
	if r := stopped.CronJob(ctx, c, cronScope); r.Status != "failed" || r.Change == "changed" {
		t.Fatal(r)
	}
	entries, _ := os.ReadDir(cronFiles.directory)
	if len(entries) != 0 {
		t.Fatal("a stopped try left files", entries)
	}
	if r := (Runtime{}).CronJob(ctx, c, cronScope); r.Change != "changed" {
		t.Fatal(r)
	}
}

func TestCronJobIsATrackedInput(t *testing.T) {
	ctx := context.Background()
	c := cronFixture(t)
	inputs := c.TrackedInputs()
	if len(inputs) != 1 || inputs[0].Kind != "config" || inputs[0].Path != c.Path() {
		t.Fatal(inputs)
	}
	predicted, now := sha256.New(), sha256.New()
	if err := inputs[0].Content.HashConfiguration(ctx, predicted, true, cronScope); err != nil {
		t.Fatal(err)
	}
	if r := (Runtime{}).CronJob(ctx, c, cronScope); r.Change != "changed" {
		t.Fatal(r)
	}
	if err := inputs[0].Content.HashConfiguration(ctx, now, false, cronScope); err != nil {
		t.Fatal(err)
	}
	if string(predicted.Sum(nil)) != string(now.Sum(nil)) {
		t.Fatal("the predicted hash differs from the hash of the written file")
	}
}

func TestCronJobRejectsInvalidInput(t *testing.T) {
	t.Parallel()
	base := CronJob{Name: "job", CronSchedule: "0 1 * * *", User: "root", Command: "/bin/true"}
	for _, test := range []struct {
		change func(*CronJob)
		want   string
	}{
		{func(c *CronJob) { c.State = "gone" }, "cron_job.state must be present or absent"},
		{func(c *CronJob) { c.State = "absent" }, "cron_job with state: absent accepts only name and state"},
		{func(c *CronJob) { c.CronSchedule = "" }, "cron_job.cron_schedule is required"},
		{func(c *CronJob) { c.User = "" }, "cron_job.run_as_user_account is required"},
		{func(c *CronJob) { c.Command = " " }, "cron_job.command is required"},
		{func(c *CronJob) { c.Command = "a\nb" }, "cron_job.command must be one line"},
		{func(c *CronJob) { c.Command = `printf '100\%'` }, "must not have a backslash directly before %"},
		{func(c *CronJob) { c.Env = map[string]any{"BAD-NAME": "x"} }, "invalid environment variable name"},
		{func(c *CronJob) { c.Env = map[string]any{"TOKEN": "Bearer {{ .secrets.token }}"} }, "a secret cannot go into a cron file"},
	} {
		c := base
		test.change(&c)
		if err := ValidateCronJob(c); err == nil || !strings.Contains(err.Error(), test.want) {
			t.Errorf("%+v: %v, want %q", c, err, test.want)
		}
	}
	for name, want := range map[string]bool{"rotate-upload-cache": true, "backup_2": true, "app.daily": false, "a b": false, "": false} {
		if (cronNameRule(name) == nil) != want {
			t.Error(name)
		}
	}
	if err := cronEnvValue(value.Sensitive("x")); err == nil || !strings.Contains(err.Error(), "a secret cannot go into a cron file") {
		t.Fatal(err)
	}
	if err := cronEnvValue("a\nb"); err == nil {
		t.Fatal("a value of two lines was accepted")
	}
	// A secret that arrives through a variable is refused on the node.
	c := base
	c.Env = map[string]any{"TOKEN": "{{ .vars.token }}"}
	if _, err := c.Text(value.Scope{"vars": map[string]any{"token": value.Sensitive("x")}}); err == nil || !strings.Contains(err.Error(), "cron_job.env.TOKEN: a secret") {
		t.Fatal(err)
	}
	for schedule, want := range map[string]string{
		"0 1 * *":       "has 4 fields",
		"@sometimes":    "is not one of cron's @ words",
		"60 * * * *":    `field 1 (minute) "60": "60" is not a valid minute: use 0 to 59`,
		"0 0 0 * *":     "is not a valid day of month: use 1 to 31",
		"0 0 * 13 *":    "use 1 to 12 or jan to dec",
		"0 0 * * 8":     "use 0 to 7 or sun to sat",
		"0 5-1 * * *":   "runs backwards",
		"5/10 * * * *":  "a step needs * or a range before it",
		"*/0 * * * *":   "the cron step after / must be a whole number greater than or equal to 1",
		"1,,2 * * * *":  `"" is not a valid minute`,
		"0 0 * * mon-x": `"x" is not a valid day of week`,
	} {
		if err := cronScheduleRule(schedule); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: %v, want %q", schedule, err, want)
		}
	}
	for _, schedule := range []string{"*/5 * * * *", "0 9-17/2 * * mon-fri", "0 0 1,15 * *", "@reboot", "0 0 * * 7", "0 0 1 1 1"} {
		if err := cronScheduleRule(schedule); err != nil {
			t.Error(schedule, err)
		}
	}
}

func TestCronEnvironmentLines(t *testing.T) {
	t.Parallel()
	for text, want := range map[string]string{
		"plain":       "X=plain\n",
		"":            "X=\"\"\n",
		" padded ":    "X=\" padded \"\n",
		`"quoted"`:    `X='"quoted"'` + "\n",
		`'single'`:    `X="'single'"` + "\n",
		"a $b c=d":    "X=a $b c=d\n",
		` say "it" `:  `X=' say "it" '` + "\n",
		"mid \"q\" x": "X=mid \"q\" x\n",
	} {
		line, err := cronEnvLine("X", text)
		if err != nil || line != want {
			t.Errorf("%q: %q %v, want %q", text, line, err, want)
		}
	}
	if _, err := cronEnvLine("X", ` both " and ' `); err == nil {
		t.Fatal("a value cron cannot keep was accepted")
	}
}
