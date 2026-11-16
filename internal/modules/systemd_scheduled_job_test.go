package modules

import (
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"mmx/internal/value"
)

func TestCronScheduleInSystemdCalendarForm(t *testing.T) {
	t.Parallel()
	for cron, want := range map[string]string{
		"10 0 * * *":          "*-*-* 00:10:00",
		"30 4 * * 1":          "Mon *-*-* 04:30:00",
		"*/15 * * * *":        "*-*-* *:00/15:00",
		"5-59/20 * * * *":     "*-*-* *:05/20:00",
		"0 9-17 * * 1-5":      "Mon..Fri *-*-* 09..17:00:00",
		"0 0 1 */3 *":         "*-01/3-01 00:00:00",
		"5,35 2 * * 0,6":      "Sat,Sun *-*-* 02:05,35:00",
		"0 0 * * 7":           "Sun *-*-* 00:00:00",
		"0 0 * * 5-7":         "Fri..Sun *-*-* 00:00:00",
		"0 0 * * 0-6":         "*-*-* 00:00:00",
		"0 0 * jan,jul sun":   "Sun *-01,07-* 00:00:00",
		"0 12 15 * *":         "*-*-15 12:00:00",
		"0-4 * * * *":         "*-*-* *:00..04:00",
		"0,1,2,10 * * * *":    "*-*-* *:00..02,10:00",
		"0-30/15 0 * * *":     "*-*-* 00:00,15,30:00",
		"0 8 * * mon,wed,fri": "Mon,Wed,Fri *-*-* 08:00:00",
		"0 8 1-31 * *":        "*-*-* 08:00:00",
	} {
		got, err := systemdCalendarFromCron(cron)
		if err != nil || got != want {
			t.Errorf("%q: %q %v, want %q", cron, got, err, want)
		}
	}
	for cron, want := range map[string]string{
		"0 0 1 * 1":   "restricts the day of the month (field 3) and the day of the week (field 5); cron runs the job when at least one day field matches, but systemd requires the two day fields to match",
		"0 0 */2 * 1": "restricts the day of the month (field 3) and the day of the week (field 5)",
		"@daily":      "for a schedule such as @daily, write systemd_calendar_schedule instead",
		"0 0 * *":     "cron_schedule must be five cron fields",
		"0 24 * * *":  `"24" is not a valid hour`,
		"0 0 * * 1 x": "it has 6 fields",
	} {
		if err := systemdCronScheduleRule(cron); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: %v, want %q", cron, err, want)
		}
	}
}

func TestScheduledJobUnits(t *testing.T) {
	t.Parallel()
	j := SystemdScheduledJob{Name: "platform-billing-renew", Script: "printf '%s\\n' \"$RAKE_TASK\" 100% \\\\\n\techo ${HOME}",
		User: "deploy", Cwd: "/home/deploy/platform current", RandomDelay: "15m", StopARunAfter: "1h30m", RunMissedJobAfterDowntime: true}
	service, timer := j.unitTexts("*-*-* 00:10:00", map[string]string{"RAILS_ENV": "production", "QUOTED": `say "100%" \ok`}, true)
	marker := "# Managed by MMX: systemd_scheduled_job. Written by the node task for platform-billing-renew; MMX replaces a change made here on its next run.\n"
	wantService := marker + `[Unit]
Description=Scheduled job platform-billing-renew, run by platform-billing-renew.timer

[Service]
Type=oneshot
User=deploy
WorkingDirectory=/home/deploy/platform current
Environment="PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
Environment="LANG=C.UTF-8"
Environment="QUOTED=say \"100%%\" \\ok"
Environment="RAILS_ENV=production"
EnvironmentFile=/etc/mmx/systemd_scheduled_jobs/platform-billing-renew.env
ExecStart=/bin/bash -c "printf '%%s\\n' \"$$RAKE_TASK\" 100%% \\\\\n\techo $${HOME}"
TimeoutStartSec=5400s
`
	if service != wantService {
		t.Errorf("service:\n%s\nwant:\n%s", service, wantService)
	}
	wantTimer := marker + `[Unit]
Description=Schedule of the job platform-billing-renew

[Timer]
OnCalendar=*-*-* 00:10:00
RandomizedDelaySec=900s
Persistent=true

[Install]
WantedBy=timers.target
`
	if timer != wantTimer {
		t.Errorf("timer:\n%s\nwant:\n%s", timer, wantTimer)
	}
	service, timer = SystemdScheduledJob{User: "root", Name: "a", Script: "true"}.unitTexts("daily", nil, false)
	if !strings.Contains(service, "\nUser=root\n") || strings.Contains(service, "EnvironmentFile=") || strings.Contains(service, "WorkingDirectory=") || strings.Contains(service, "TimeoutStartSec=") {
		t.Error(service)
	}
	if !strings.Contains(timer, "\nPersistent=false\n") || strings.Contains(timer, "RandomizedDelaySec") {
		t.Error(timer)
	}
	if got := environmentFileText(map[string]string{"TOKEN": "a\"b\\c`d$e f'g\nh", "A": ""}); got != "A=\"\"\nTOKEN=\"a\\\"b\\\\c\\`d\\$e f'g\nh\"\n" {
		t.Errorf("%q", got)
	}
	if unitDuration("1500ms") != "1500ms" || unitDuration("2h") != "7200s" {
		t.Error(unitDuration("1500ms"), unitDuration("2h"))
	}
}

func TestScheduledJobEnvironmentKeepsSecretsOutOfTheUnit(t *testing.T) {
	t.Parallel()
	// A list or a mapping becomes JSON text, as in a shell task's env, and
	// a secret one still goes into the environment file.
	j := SystemdScheduledJob{User: "root", Name: "a", Script: "true", Env: map[string]any{"PLAIN": "{{ .vars.environment }}", "TOKEN": "Bearer {{ .secrets.token }}", "PORT": 8080,
		"SITES": "{{ .vars.sites }}", "DATABASE": "{{ .secrets.database }}"}}
	plain, secret, err := j.environment(value.Scope{"vars": map[string]any{"environment": "production", "sites": []any{"frankfurt", "helsinki"}},
		"secrets": map[string]any{"token": value.Sensitive("s3cret"), "database": value.Sensitive(map[string]any{"user": "app", "password": "pw"})}})
	if err != nil {
		t.Fatal(err)
	}
	if len(plain) != 3 || plain["PLAIN"] != "production" || plain["PORT"] != "8080" || plain["SITES"] != `["frankfurt","helsinki"]` ||
		len(secret) != 2 || secret["TOKEN"] != "Bearer s3cret" || secret["DATABASE"] != `{"password":"pw","user":"app"}` {
		t.Fatal(plain, secret)
	}
	service, _ := j.unitTexts("daily", plain, true)
	if strings.Contains(service, "s3cret") || !strings.Contains(service, "EnvironmentFile=/etc/mmx/systemd_scheduled_jobs/a.env\n") {
		t.Fatal(service)
	}
}

func TestSystemdScheduledJobRejectsInvalidInput(t *testing.T) {
	t.Parallel()
	base := SystemdScheduledJob{User: "root", Name: "job", Script: "true", CronSchedule: "0 1 * * *"}
	for _, test := range []struct {
		change func(*SystemdScheduledJob)
		want   string
	}{
		{func(j *SystemdScheduledJob) { j.State = "gone" }, "systemd_scheduled_job.state must be present or absent"},
		{func(j *SystemdScheduledJob) { j.State = "absent" }, "absent systemd_scheduled_job accepts only name, state, and run_as_user_account"},
		{func(j *SystemdScheduledJob) { j.User = "" }, "systemd_scheduled_job.run_as_user_account is required"},
		{func(j *SystemdScheduledJob) { j.Script = "" }, "systemd_scheduled_job.script is required"},
		{func(j *SystemdScheduledJob) { j.CronSchedule = "" }, "requires exactly one of cron_schedule"},
		{func(j *SystemdScheduledJob) { j.CalendarSchedule = "daily" }, "requires exactly one of cron_schedule"},
		{func(j *SystemdScheduledJob) { j.RandomDelay = "15 minutes" }, "systemd_scheduled_job.random_delay must be a positive duration"},
		{func(j *SystemdScheduledJob) { j.StopARunAfter = "-1h" }, "systemd_scheduled_job.stop_a_run_after must be a positive duration"},
		{func(j *SystemdScheduledJob) { j.Env = map[string]any{"A B": "x"} }, "invalid environment variable name"},
	} {
		j := base
		test.change(&j)
		if err := ValidateSystemdScheduledJob(j); err == nil || !strings.Contains(err.Error(), test.want) {
			t.Errorf("%+v: %v, want %q", j, err, test.want)
		}
	}
	if err := ValidateSystemdScheduledJob(SystemdScheduledJob{User: "root", Name: "job", State: "absent"}); err != nil {
		t.Fatal(err)
	}
	if err := ValidateSystemdScheduledJob(SystemdScheduledJob{Name: "job", State: "absent"}); err == nil || !strings.Contains(err.Error(), "run_as_user_account is required") {
		t.Fatalf("removal without account: %v", err)
	}
	for name, want := range map[string]bool{"platform-billing-renew": true, "a.b:c_d": true, "app@1": false, "backup.timer": false, "backup.service": false, "-x": false, "": false} {
		if (scheduledJobNameRule(name) == nil) != want {
			t.Error(name)
		}
	}
	for directory, want := range map[string]bool{"/srv/app 1": true, "srv/app": false, "/srv/a\nb": false} {
		if (unitDirectory(directory) == nil) != want {
			t.Errorf("%q", directory)
		}
	}
	for schedule, want := range map[string]bool{"Mon *-*-* 04:30:00": true, "daily": true, " ": false, "daily\nweekly": false} {
		if (calendarScheduleRule(schedule) == nil) != want {
			t.Errorf("%q", schedule)
		}
	}
}

func TestSystemdScheduledJobWithoutSystemd(t *testing.T) {
	t.Parallel()
	setWorkerEnvironment(t, "PATH", t.TempDir())
	j := SystemdScheduledJob{User: "root", Name: "job", Script: "true", CronSchedule: "0 1 * * *"}
	if r := (Runtime{Dryrun: true}).SystemdScheduledJob(testContext(t), j, nil); r.Status != "not_checked" || !strings.Contains(r.Reason, missingProgramError("systemctl").Error()) {
		t.Fatal(r)
	}
	if r := (Runtime{}).SystemdScheduledJob(testContext(t), j, nil); r.Status != "failed" || !strings.Contains(r.Reason, "systemctl is not installed") {
		t.Fatal(r)
	}
}

// A timer that systemd does not start or restart fails with systemd's
// message and the timer's journal lines since the action.
func TestScheduledJobTimerThatDoesNotStartReportsItsJournalLines(t *testing.T) {
	t.Parallel()
	// The stand-in systemctl knows one timer: stopped, or running on
	// another schedule while $dir/running exists. It refuses every action.
	dir := standInPrograms(t, map[string]string{"journalctl": fakeJournalctl, "systemctl": `
state=inactive
[ ! -e "$dir/running" ] || state=active
case "$1 $2" in
"show --property=TimersCalendar") echo "TimersCalendar={ OnCalendar=*-*-* 04:00:00 ; next_elapse=Sat 2026-10-10 04:00:00 UTC }" ;;
show*) printf 'Id=nightly.timer\nNames=nightly.timer\nLoadState=loaded\nActiveState=%s\nUnitFileState=enabled\nInvocationID=5a6f97\n' $state ;;
*) echo "Job for nightly.timer failed." >&2; exit 1 ;;
esac`})
	writeState(t, filepath.Join(dir, "journal/nightly.timer"), journalEntryText(1, `"MESSAGE":"a line from before the action"`)+
		journalEntryText(1<<62, `"MESSAGE":"nightly.timer: Refusing to start, unit nightly.service to trigger not loaded."`))
	for _, action := range []string{"start", "restart"} {
		if action == "restart" {
			writeState(t, filepath.Join(dir, "running"), "")
		}
		r := (Runtime{}).activateTimer(testContext(t), "nightly.timer", "*-*-* 03:00:00", false)
		want := "systemctl " + action + " nightly.timer: exit status 1: Job for nightly.timer failed." +
			"\nLast lines of the systemd journal for nightly.timer during this " + action + ":" +
			"\n  nightly.timer: Refusing to start, unit nightly.service to trigger not loaded."
		if r.Status != "failed" || r.Change != "unknown" || r.Reason != want {
			t.Fatalf("%s: %+v", action, r)
		}
	}
}

// The whole job on a node's own systemd: run, unchanged rerun, dryrun,
// secrets in the environment file, an interrupted manager reload, a stopped
// timer, a refused file, and removal.
func TestSystemdScheduledJobOnANode(t *testing.T) {
	// Not parallel: the test works on the node's systemd manager, whose
	// reloads and pending reloads every such test shares.
	if runtime.GOOS != "linux" || os.Geteuid() != 0 {
		t.Skip("requires a root Linux systemd test guest")
	}
	if _, err := os.Stat("/run/systemd/system"); err != nil {
		t.Skip("requires running systemd")
	}
	ctx := testContext(t)
	name := "mmx-code-job-" + strings.ToLower(rand.Text()[:8])
	// The job runs as nobody, in a directory that account may write.
	work, err := os.MkdirTemp("/var/tmp", "mmx-code-job-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(work) })
	if err := os.Chmod(work, 0777); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(work, "output")
	service, timer := "/etc/systemd/system/"+name+".service", "/etc/systemd/system/"+name+".timer"
	j := SystemdScheduledJob{Name: name, CronSchedule: "10 0 * * *", User: "nobody", Cwd: work,
		RandomDelay: "15m", StopARunAfter: "2h", RunMissedJobAfterDowntime: true,
		Script: "printf '%s|%s|%s|%s\\n' \"$(id -un)\" \"$PLAIN\" \"$TOKEN\" \"100% \\\"q\\\" $((1+1))\" > output",
		Env:    map[string]any{"PLAIN": "a b", "TOKEN": "{{ .secrets.token }}"}}
	scope := value.Scope{"secrets": map[string]any{"token": value.Sensitive("s3cret $HOME")}}
	t.Cleanup(func() {
		Command(ctx, nil, "", "systemctl", "disable", "--now", name+".timer")
		os.Remove(service)
		os.Remove(timer)
		os.Remove(j.EnvironmentFile())
		Command(ctx, nil, "", "systemctl", "daemon-reload")
	})
	enabled := func() string {
		r := Command(ctx, nil, "", "systemctl", "is-enabled", name+".timer")
		return strings.TrimSpace(r.Stdout)
	}
	active := func() string {
		r := Command(ctx, nil, "", "systemctl", "is-active", name+".timer")
		return strings.TrimSpace(r.Stdout)
	}

	preview := (Runtime{Dryrun: true}).SystemdScheduledJob(ctx, j, scope)
	values, _ := preview.Value.(map[string]any)
	if preview.Status != "succeeded" || preview.Change != "would_change" || values["systemd_calendar_expression"] != "*-*-* 00:10:00" || values["next_start_time"] == "" {
		t.Fatal(preview)
	}
	if !strings.Contains(preview.Diff, service+": create") || !strings.Contains(preview.Diff, "+OnCalendar=*-*-* 00:10:00") {
		t.Fatal(preview.Diff)
	}
	if _, err := os.Stat(timer); !os.IsNotExist(err) {
		t.Fatal("dryrun wrote the timer", err)
	}

	r := (Runtime{}).SystemdScheduledJob(ctx, j, scope)
	if r.Status != "succeeded" || r.Change != "changed" || !strings.Contains(r.Reason, "enabled and started "+name+".timer") {
		t.Fatal(r)
	}
	if enabled() != "enabled" || active() != "active" {
		t.Fatal(enabled(), active())
	}
	if loaded, err := loadedCalendar(ctx, name+".timer"); err != nil || loaded != "*-*-* 00:10:00" {
		t.Fatal(loaded, err)
	}
	unit, _ := os.ReadFile(service)
	if strings.Contains(string(unit), "s3cret") {
		t.Fatal("the secret is in the unit")
	}
	if mode(t, j.EnvironmentFile()) != 0600 || mode(t, filepath.Dir(j.EnvironmentFile())) != 0700 {
		t.Fatal(mode(t, j.EnvironmentFile()))
	}
	if r := Command(ctx, nil, "", "systemctl", "start", name+".service"); r.Status != "succeeded" {
		t.Fatal(r)
	}
	if b, err := os.ReadFile(output); err != nil || string(b) != "nobody|a b|s3cret $HOME|100% \"q\" 2\n" {
		t.Fatalf("%q %v", b, err)
	}
	for _, dryrun := range []bool{true, false} {
		if r := (Runtime{Dryrun: dryrun}).SystemdScheduledJob(ctx, j, scope); r.Status != "succeeded" || r.Change != "unchanged" {
			t.Fatal(dryrun, r)
		}
	}

	// The unit changed, and the manager reload was interrupted: the next run
	// reloads, and the timer runs the new schedule.
	j.CronSchedule = "30 4 * * 1"
	interrupted := Runtime{BeforeMutation: func(resource string) error {
		if resource == "systemd manager" {
			return errors.New("stopped")
		}
		return nil
	}}
	if r := interrupted.SystemdScheduledJob(ctx, j, scope); r.Status != "failed" || r.Change != "changed" {
		t.Fatal(r)
	}
	r = (Runtime{}).SystemdScheduledJob(ctx, j, scope)
	if r.Status != "succeeded" || r.Change != "changed" {
		t.Fatal(r)
	}
	if loaded, err := loadedCalendar(ctx, name+".timer"); err != nil || loaded != "Mon *-*-* 04:30:00" {
		t.Fatal(loaded, err)
	}
	// A timer stopped by hand is started again.
	Command(ctx, nil, "", "systemctl", "stop", name+".timer")
	if r := (Runtime{Dryrun: true}).SystemdScheduledJob(ctx, j, scope); r.Change != "would_change" || !strings.Contains(r.Reason, "would start") {
		t.Fatal(r)
	}
	if r := (Runtime{}).SystemdScheduledJob(ctx, j, scope); r.Change != "changed" || active() != "active" {
		t.Fatal(r, active())
	}
	// Without a secret, the environment file goes.
	j.Env = map[string]any{"PLAIN": "a b"}
	if r := (Runtime{}).SystemdScheduledJob(ctx, j, nil); r.Change != "changed" {
		t.Fatal(r)
	}
	if _, err := os.Stat(j.EnvironmentFile()); !os.IsNotExist(err) {
		t.Fatal(err)
	}

	// A unit without the marker line is never removed or overwritten: a
	// person or another tool wrote it.
	foreign := "[Timer]\nOnCalendar=daily\n"
	if err := os.WriteFile(timer, []byte(foreign), 0644); err != nil {
		t.Fatal(err)
	}
	absent := SystemdScheduledJob{User: "root", Name: name, State: "absent"}
	if r := (Runtime{}).SystemdScheduledJob(ctx, absent, nil); r.Status != "failed" || !strings.Contains(r.Reason, "refusing to remove "+timer) {
		t.Fatal(r)
	}
	if _, err := os.Stat(service); err != nil {
		t.Fatal("the service was removed after the refusal", err)
	}
	if r := (Runtime{}).SystemdScheduledJob(ctx, j, nil); r.Status != "failed" || !strings.Contains(r.Reason, "refusing to write "+timer) {
		t.Fatal(r)
	}
	if b, err := os.ReadFile(timer); err != nil || string(b) != foreign {
		t.Fatalf("the timer without the marker line was overwritten: %q %v", b, err)
	}
	// Once the person removes that file, the job writes its own timer again.
	if err := os.Remove(timer); err != nil {
		t.Fatal(err)
	}
	if r := (Runtime{}).SystemdScheduledJob(ctx, j, nil); r.Status != "succeeded" || r.Change != "changed" {
		t.Fatal(r)
	}
	if r := (Runtime{Dryrun: true}).SystemdScheduledJob(ctx, absent, nil); r.Change != "would_change" || active() != "active" {
		t.Fatal(r)
	}
	if r := (Runtime{}).SystemdScheduledJob(ctx, absent, nil); r.Status != "succeeded" || r.Change != "changed" {
		t.Fatal(r)
	}
	for _, path := range []string{service, timer} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatal(path, err)
		}
	}
	if state, err := InspectService(ctx, name+".timer"); err != nil || state.Load != "not-found" || state.Active != "inactive" {
		t.Fatal(state, err)
	}
	if r := (Runtime{}).SystemdScheduledJob(ctx, absent, nil); r.Status != "succeeded" || r.Change != "unchanged" {
		t.Fatal(r)
	}
}
