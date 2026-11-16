package modules

import (
	"context"
	"fmt"
	"maps"
	"os"
	"os/user"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"time"

	"mmx/internal/protocol"
	"mmx/internal/value"
)

// SystemdScheduledJob writes a oneshot service and a timer for one job,
// checks them, and enables and starts the timer. A secret in env goes into an
// environment file only root can read, never into the unit. Every run
// inspects the files, the timer's state, and the schedule systemd has loaded,
// so the next run finishes an interrupted one.
func (run Runtime) SystemdScheduledJob(ctx context.Context, j SystemdScheduledJob, s value.Scope) protocol.Result {
	if err := ValidateSystemdScheduledJob(j); err != nil {
		return protocol.ConfigurationError(err)
	}
	// The unit files are small; their differences are in the result, and no
	// full difference is kept.
	run.RetainDiff = nil
	if j.State == "absent" {
		return run.removeSystemdScheduledJob(ctx, j)
	}
	if err := refuseForeignUnits(j); err != nil {
		return protocol.Failed(err)
	}
	for _, program := range []string{"systemctl", "systemd-analyze"} {
		if _, err := lookPath(ctx, program); err != nil {
			return run.notYet(missingProgramError(program))
		}
	}
	expression, err := j.CalendarExpression()
	if err != nil {
		return protocol.ConfigurationError(err)
	}
	calendar, err := inspectCalendar(ctx, expression)
	if err != nil {
		return protocol.ConfigurationError(err)
	}
	if j.User != "" {
		if _, err := user.Lookup(j.User); err != nil {
			return run.accountFailure(fmt.Errorf("systemd_scheduled_job.run_as_user_account: %w", err))
		}
	}
	plain, secret, err := j.environment(s)
	if err != nil {
		return protocol.ConfigurationError(err)
	}
	service, timer := j.unitTexts(expression, plain, len(secret) > 0)
	job := scheduledJobResult{result: protocol.Success("unchanged")}
	job.result.Value = map[string]any{"systemd_calendar_expression": calendar.normalized, "next_start_time": calendar.next}

	// The environment file comes first, so that the service never names a
	// file that is not there.
	if len(secret) > 0 {
		made := run.makeSecretEnvironmentDirectory(ctx)
		if !job.add(made) {
			return job.result
		}
		if made.Change == "would_change" {
			// A dryrun's file below names a directory that this node task
			// makes first, as an earlier node task's directory counts.
			run.PreviewPaths = append(slices.Clone(run.PreviewPaths), secretEnvironmentDirectory)
		}
		text := markerLine("systemd_scheduled_job", j.Name) + environmentFileText(secret)
		if !job.add(run.File(ctx, File{Path: j.EnvironmentFile(), Literal: &text, Owner: "root", Group: "root", Mode: "0600"}, s)) {
			return job.result
		}
	} else if !job.add(run.removeMarkedFile(ctx, j.EnvironmentFile(), managedMarker("systemd_scheduled_job"), "systemd_scheduled_job")) {
		return job.result
	}
	if !job.add(run.installUnit(ctx, "systemd_scheduled_job", Unit{Name: j.Name + ".service", Literal: &service}, s)) {
		return job.result
	}
	if !job.add(run.installUnit(ctx, "systemd_scheduled_job", Unit{Name: j.Name + ".timer", Literal: &timer}, s)) {
		return job.result
	}
	job.add(run.activateTimer(ctx, j.Name+".timer", calendar.normalized, job.result.Change != "unchanged"))
	return job.result
}

// scheduledJobUnitDirectory is where the job's units go, and
// vendorUnitDirectories are where packages put theirs. Tests point both
// elsewhere.
var (
	scheduledJobUnitDirectory = "/etc/systemd/system"
	vendorUnitDirectories     = []string{"/usr/lib/systemd/system", "/lib/systemd/system"}
)

// refuseForeignUnits fails before anything changes when the job would
// overwrite or shadow a file it did not write: a unit file in /etc without
// MMX's marker line, or, when /etc has no such file yet, a package's unit of
// the same name, which a new /etc unit replaces completely.
func refuseForeignUnits(j SystemdScheduledJob) error {
	marker := managedMarker("systemd_scheduled_job")
	for _, unit := range []string{j.Name + ".timer", j.Name + ".service"} {
		path := filepath.Join(scheduledJobUnitDirectory, unit)
		if err := refuseForeignFile(path, marker, "systemd_scheduled_job"); err != nil {
			return err
		}
		if _, err := os.Lstat(path); err == nil {
			continue
		}
		for _, dir := range vendorUnitDirectories {
			vendor := filepath.Join(dir, unit)
			if _, err := os.Lstat(vendor); err == nil {
				return fmt.Errorf("refusing to write %s: the system already has the unit %s, and a unit in /etc/systemd/system replaces it completely. Choose another name for the systemd_scheduled_job task", path, vendor)
			}
		}
	}
	return refuseForeignFile(j.EnvironmentFile(), marker, "systemd_scheduled_job")
}

// scheduledJobResult gathers the results of a job's steps into one: the
// combined change, every file's difference, and the first failure.
type scheduledJobResult struct {
	result  protocol.Result
	actions []string
}

// add records one step and reports whether the job goes on.
func (job *scheduledJobResult) add(step protocol.Result) bool {
	job.result.Change = protocol.CombineChange(job.result.Change, step.Change)
	if c := step.FileChange; c != nil {
		job.result.Diff += c.Path + ": " + c.Action + "\n"
		if c.Diff != "" {
			job.result.Diff += c.Diff
		} else if c.DiffOmitted != "" {
			job.result.Diff += c.DiffOmitted + "\n"
		}
	}
	if step.CheckNewFile != nil && job.result.CheckNewFile == nil {
		job.result.CheckNewFile = step.CheckNewFile
	}
	if step.Reason != "" && step.Status == "succeeded" {
		job.actions = append(job.actions, step.Reason)
		job.result.Reason = strings.Join(job.actions, "; ")
	}
	if step.Status != "succeeded" {
		change, value, diff := job.result.Change, job.result.Value, job.result.Diff
		job.result = step
		job.result.Change, job.result.Value, job.result.Diff, job.result.FileChange = change, value, diff, nil
		return false
	}
	return true
}

// secretEnvironmentDirectory holds the environment files of the scheduled
// jobs whose env has a secret; only root can enter it.
const secretEnvironmentDirectory = "/etc/mmx/systemd_scheduled_jobs"

// makeSecretEnvironmentDirectory makes /etc/mmx when it is missing and its
// directory for the environment files, private to root.
func (run Runtime) makeSecretEnvironmentDirectory(ctx context.Context) protocol.Result {
	result := protocol.Success("unchanged")
	parent := filepath.Dir(secretEnvironmentDirectory)
	for _, directory := range []struct {
		path, mode string
	}{{parent, "0755"}, {secretEnvironmentDirectory, "0700"}} {
		info, err := os.Lstat(directory.path)
		if err != nil && !os.IsNotExist(err) {
			return protocol.Failed(err)
		}
		// An existing /etc/mmx keeps the mode its administrator gave it.
		if info != nil && directory.path == parent {
			continue
		}
		if info == nil && run.Dryrun {
			result.Change = "would_change"
			break
		}
		made := run.MakeDirectory(ctx, directory.path, "root", "root", directory.mode)
		if made.Error != "" {
			failed := protocol.Failed(fmt.Errorf("%s: %s", directory.path, made.Error))
			failed.Change = result.Change
			return failed
		}
		if made.Changed {
			result.Change = "changed"
			if run.Dryrun {
				result.Change = "would_change"
			}
		}
	}
	return result
}

// activateTimer enables and starts the timer, and makes systemd load the
// schedule of its unit file when it still runs another one. changed says
// that an earlier step changed, or in a dryrun would change, the units.
func (run Runtime) activateTimer(ctx context.Context, timer, schedule string, changed bool) protocol.Result {
	manager := UnitManager{}
	state, err := manager.Inspect(ctx, timer)
	if err != nil {
		return protocol.Failed(err)
	}
	var verbs []string
	if state.Enabled != "enabled" {
		verbs = append(verbs, "enable")
	}
	if state.Active != "active" {
		verbs = append(verbs, "start")
	}
	if run.Dryrun {
		// A dryrun cannot know the schedule systemd would load from a unit
		// file it would change.
		if !changed && state.Active == "active" {
			loaded, err := loadedCalendar(ctx, timer)
			if err != nil {
				return protocol.Failed(err)
			}
			if loaded != schedule {
				verbs = append(verbs, "restart")
			}
		}
		if len(verbs) == 0 {
			return protocol.Success("unchanged")
		}
		r := protocol.Success("would_change")
		r.Reason = "would " + strings.Join(verbs, " and ") + " " + timer
		return r
	}
	done := []string{}
	result := func(r protocol.Result) protocol.Result {
		if len(done) > 0 {
			r.Change = protocol.CombineChange(r.Change, "changed")
			if r.Status == "succeeded" {
				r.Reason = strings.Join(done, " and ") + " " + timer
			}
		}
		return r
	}
	for _, verb := range verbs {
		if err := run.Mutation(timer); err != nil {
			return result(protocol.Failed(err))
		}
		asked := JournalNow()
		if r := manager.Action(ctx, verb, timer); r.Status != "succeeded" {
			r.Change, r.Reason = "unknown", "systemctl "+verb+" "+timer+": "+r.Reason+": "+strings.TrimSpace(r.Stderr)
			if verb == "start" {
				r.Reason += manager.JournalAfterFailedAction(ctx, verb, timer, asked)
			}
			return result(r)
		}
		done = append(done, map[string]string{"enable": "enabled", "start": "started"}[verb])
	}
	loaded, err := loadedCalendar(ctx, timer)
	if err != nil {
		return result(protocol.Failed(err))
	}
	if loaded != schedule {
		// A timer that kept running through an interrupted manager reload
		// still waits for the old schedule; a restart loads the new one.
		if err := run.Mutation(timer); err != nil {
			return result(protocol.Failed(err))
		}
		asked := JournalNow()
		if r := manager.Action(ctx, "restart", timer); r.Status != "succeeded" {
			r.Change, r.Reason = "unknown", "systemctl restart "+timer+": "+r.Reason+": "+strings.TrimSpace(r.Stderr)+manager.JournalAfterFailedAction(ctx, "restart", timer, asked)
			return result(r)
		}
		done = append(done, "restarted")
		if loaded, err = loadedCalendar(ctx, timer); err != nil {
			return result(protocol.Failed(err))
		}
		if loaded != schedule {
			return result(protocol.Failed(fmt.Errorf("systemd runs %s on the schedule %q, not %q as its unit file says", timer, loaded, schedule)))
		}
	}
	return result(protocol.Success("unchanged"))
}

// loadedCalendar is the calendar expression systemd has loaded for a timer,
// in systemd's normalized form.
func loadedCalendar(ctx context.Context, timer string) (string, error) {
	r := UnitManager{}.systemctl(ctx, "show", "--property=TimersCalendar", "--", timer)
	if r.Status != "succeeded" {
		return "", fmt.Errorf("inspect the schedule of %s: %s: %s", timer, r.Reason, strings.TrimSpace(r.Stderr))
	}
	match := loadedCalendarPattern.FindStringSubmatch(r.Stdout)
	if match == nil {
		return "", nil
	}
	return match[1], nil
}

// systemctl show prints a timer's calendar as
// TimersCalendar={ OnCalendar=Mon *-*-* 04:30:00 ; next_elapse=... }.
var loadedCalendarPattern = regexp.MustCompile(`OnCalendar=(.*?) ; next_elapse=`)

// calendarCheck is what systemd-analyze calendar says about an expression.
type calendarCheck struct {
	normalized, next string
}

// inspectCalendar checks a calendar expression with systemd-analyze calendar,
// which also gives its normalized form and the next time it elapses.
func inspectCalendar(ctx context.Context, expression string) (calendarCheck, error) {
	r := Command(ctx, nil, "", "systemd-analyze", "calendar", "--", expression)
	if r.Status != "succeeded" {
		return calendarCheck{}, fmt.Errorf("systemd_scheduled_job: systemd-analyze calendar refuses the schedule %q: %s", expression, strings.TrimSpace(r.Stderr))
	}
	var check calendarCheck
	for _, line := range strings.Split(r.Stdout, "\n") {
		key, text, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		switch strings.TrimSpace(key) {
		case "Normalized form":
			check.normalized = strings.TrimSpace(text)
		case "Next elapse":
			check.next = strings.TrimSpace(text)
		}
	}
	if check.normalized == "" {
		return calendarCheck{}, fmt.Errorf("systemd_scheduled_job: systemd-analyze calendar printed no normalized form for %q: %s", expression, strings.TrimSpace(r.Stdout))
	}
	if check.next == "" || check.next == "never" {
		return calendarCheck{}, fmt.Errorf("systemd_scheduled_job: the schedule %q never starts the job", expression)
	}
	return check, nil
}

// removeSystemdScheduledJob stops and disables the timer and removes both
// units and the environment file. It refuses a file without the marker
// line before it changes anything.
func (run Runtime) removeSystemdScheduledJob(ctx context.Context, j SystemdScheduledJob) protocol.Result {
	marker := managedMarker("systemd_scheduled_job")
	timer := j.Name + ".timer"
	present := map[string]bool{}
	for _, path := range j.ManagedPaths() {
		parent, err := os.OpenRoot(filepath.Dir(path))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return protocol.Failed(err)
		}
		info, err := entryInfoAt(parent, filepath.Base(path), false)
		if err == nil && info != nil {
			present[path] = true
			err = refuseUnmarked(parent, path, marker, "a systemd_scheduled_job task with state: absent")
		}
		parent.Close()
		if err != nil {
			return protocol.Failed(err)
		}
	}
	job := scheduledJobResult{result: protocol.Success("unchanged")}
	if present[filepath.Join("/etc/systemd/system", timer)] {
		if _, err := lookPath(ctx, "systemctl"); err != nil {
			return run.notYet(missingProgramError("systemctl"))
		}
		state, err := UnitManager{}.Inspect(ctx, timer)
		if err != nil {
			return protocol.Failed(err)
		}
		if state.Enabled == "enabled" || (state.Active != "inactive" && state.Active != "failed") {
			step := protocol.Success("would_change")
			step.Reason = "would stop and disable " + timer
			if !run.Dryrun {
				if err := run.Mutation(timer); err != nil {
					return protocol.Failed(err)
				}
				step = UnitManager{}.systemctl(ctx, "disable", "--now", "--", timer)
				if step.Status != "succeeded" {
					step.Change, step.Reason = "unknown", "systemctl disable --now "+timer+": "+step.Reason+": "+strings.TrimSpace(step.Stderr)
				} else {
					step.Change, step.Reason = "changed", "stopped and disabled "+timer
				}
			}
			if !job.add(step) {
				return job.result
			}
		}
	}
	for _, unit := range []string{timer, j.Name + ".service"} {
		if !job.add(run.Unit(ctx, Unit{Name: unit, State: "absent"}, value.Scope{})) {
			return job.result
		}
	}
	job.add(run.removeMarkedFile(ctx, j.EnvironmentFile(), marker, "a systemd_scheduled_job task with state: absent"))
	return job.result
}

// SystemdScheduledJob is the input of systemd_scheduled_job.
type SystemdScheduledJob struct {
	Name  string `yaml:"name" json:"name"`
	State string `yaml:"state,omitempty" json:"state,omitempty"`
	// Script runs with bash -c, exactly like shell.script.
	Script string         `yaml:"script,omitempty" json:"script,omitempty"`
	User   string         `yaml:"run_as_user_account,omitempty" json:"run_as_user_account,omitempty"`
	Cwd    string         `yaml:"run_from_directory,omitempty" json:"run_from_directory,omitempty"`
	Env    map[string]any `yaml:"env,omitempty" json:"env,omitempty"`
	// Exactly one of CronSchedule and CalendarSchedule is the schedule.
	CronSchedule     string `yaml:"cron_schedule,omitempty" json:"cron_schedule,omitempty"`
	CalendarSchedule string `yaml:"systemd_calendar_schedule,omitempty" json:"systemd_calendar_schedule,omitempty"`
	RandomDelay      string `yaml:"random_delay,omitempty" json:"random_delay,omitempty"`
	// RunMissedJobAfterDowntime is the timer's Persistent= setting.
	RunMissedJobAfterDowntime bool   `yaml:"run_missed_job_after_downtime,omitempty" json:"run_missed_job_after_downtime,omitempty"`
	StopARunAfter             string `yaml:"stop_a_run_after,omitempty" json:"stop_a_run_after,omitempty"`
}

// EnvironmentFile is where the job's secret env values go.
func (j SystemdScheduledJob) EnvironmentFile() string {
	return filepath.Join(secretEnvironmentDirectory, j.Name+".env")
}

// scheduledJobName is the part of both unit names before .service and
// .timer: a unit name without the @ of a template.
var scheduledJobName = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.:-]*$`)

var scheduledJobNameRule = rule("systemd_scheduled_job.name must use letters, digits, '_', '-', '.', and ':', starting with a letter, digit, or '_', and must not end in .service or .timer: the units are <name>.service and <name>.timer", func(s string) bool {
	return scheduledJobName.MatchString(s) && len(s) <= 200 && !strings.HasSuffix(s, ".service") && !strings.HasSuffix(s, ".timer")
})

// calendarScheduleRule accepts one line for systemd-analyze calendar to check
// on the node.
var calendarScheduleRule = rule("systemd_calendar_schedule must be one line, such as Mon *-*-* 04:30:00", func(s string) bool {
	return strings.TrimSpace(s) != "" && !strings.ContainsFunc(s, func(r rune) bool { return r < 0x20 || r == 0x7f })
})

// unitDirectory is an absolute path that fits on one line of a unit file.
var unitDirectory = rule("run_from_directory must be an absolute path without control characters", func(s string) bool {
	return filepath.IsAbs(s) && !strings.ContainsFunc(s, func(r rune) bool { return r < 0x20 || r == 0x7f })
})

// systemdCronScheduleRule accepts five cron fields that systemd can run as
// cron does.
var systemdCronScheduleRule = parsed(func(s string) error { _, err := systemdCalendarFromCron(s); return err })

func ValidateSystemdScheduledJob(j SystemdScheduledJob) error {
	if j.User == "" {
		return invalid("run_as_user_account", "systemd_scheduled_job.run_as_user_account is required; write run_as_user_account: root to run as root")
	}
	if err := validatePresence(j.State); err != nil {
		return invalid("state", "systemd_scheduled_job.state must be present or absent")
	}
	if j.State == "absent" {
		if j.Script != "" || j.Cwd != "" || j.Env != nil || j.CronSchedule != "" || j.CalendarSchedule != "" ||
			j.RandomDelay != "" || j.RunMissedJobAfterDowntime || j.StopARunAfter != "" {
			return fmt.Errorf("absent systemd_scheduled_job accepts only name, state, and run_as_user_account")
		}
		return nil
	}
	if strings.TrimSpace(j.Script) == "" {
		return invalid("script", "systemd_scheduled_job.script is required: the Bash script the job runs")
	}
	if (j.CronSchedule == "") == (j.CalendarSchedule == "") {
		return fmt.Errorf("systemd_scheduled_job requires exactly one of cron_schedule (five cron fields, such as \"30 4 * * 1\") or systemd_calendar_schedule (such as Mon *-*-* 04:30:00)")
	}
	for field, text := range map[string]string{"random_delay": j.RandomDelay, "stop_a_run_after": j.StopARunAfter} {
		if text == "" || !writtenDuration(text) {
			continue
		}
		if err := scheduledJobDuration(field)(text); err != nil {
			return err
		}
	}
	return validateEnvNames(j.Env)
}

// CalendarExpression is the job's schedule in systemd's calendar form.
func (j SystemdScheduledJob) CalendarExpression() (string, error) {
	if j.CronSchedule != "" {
		return systemdCalendarFromCron(j.CronSchedule)
	}
	return j.CalendarSchedule, nil
}

// systemdCalendarFromCron translates five cron fields to a systemd calendar
// expression. Cron runs a job whose day of the month and day of the week are
// both restricted when either matches, and systemd only when both match, so
// such a schedule is refused.
func systemdCalendarFromCron(text string) (string, error) {
	s, err := parseCronSchedule(text, false)
	if err != nil {
		return "", err
	}
	if s.fields[2] != "*" && s.fields[4] != "*" {
		return "", fmt.Errorf("cron_schedule %q restricts the day of the month (field 3) and the day of the week (field 5); cron runs the job when at least one day field matches, but systemd requires the two day fields to match. Restrict only the day of the month or only the day of the week, or use systemd_calendar_schedule", text)
	}
	minute, hour, day, month := s.values[0], s.values[1], s.values[2], s.values[3]
	expression := fmt.Sprintf("*-%s-%s %s:%s:00", calendarValues(month, 1, 12), calendarValues(day, 1, 31), calendarValues(hour, 0, 23), calendarValues(minute, 0, 59))
	if weekdays := calendarWeekdays(s.values[4]); weekdays != "" {
		expression = weekdays + " " + expression
	}
	return expression, nil
}

// calendarValues writes the selected values of one field in systemd's form:
// * for every value, start/step for a repetition to the end of the field,
// a..b for three or more values in a row, and otherwise a list.
func calendarValues(selected []bool, low, high int) string {
	var values []int
	for v := low; v <= high; v++ {
		if selected[v] {
			values = append(values, v)
		}
	}
	if len(values) == high-low+1 {
		return "*"
	}
	if len(values) >= 3 && values[1]-values[0] > 1 {
		step, repeats := values[1]-values[0], true
		for i := 1; i < len(values); i++ {
			repeats = repeats && values[i]-values[i-1] == step
		}
		if repeats && values[len(values)-1]+step > high {
			return fmt.Sprintf("%02d/%d", values[0], step)
		}
	}
	var parts []string
	for i := 0; i < len(values); {
		end := i
		for end+1 < len(values) && values[end+1] == values[end]+1 {
			end++
		}
		if end-i >= 2 {
			parts = append(parts, fmt.Sprintf("%02d..%02d", values[i], values[end]))
		} else {
			for k := i; k <= end; k++ {
				parts = append(parts, fmt.Sprintf("%02d", values[k]))
			}
		}
		i = end + 1
	}
	return strings.Join(parts, ",")
}

// calendarWeekdays writes cron's days of the week (0 is Sunday) as systemd's
// names, Monday first; it is empty for every day.
func calendarWeekdays(selected []bool) string {
	order := []int{1, 2, 3, 4, 5, 6, 0}
	names := []string{"Sun", "Mon", "Tue", "Wed", "Thu", "Fri", "Sat"}
	var days []int
	for i, day := range order {
		if selected[day] {
			days = append(days, i)
		}
	}
	if len(days) == 7 {
		return ""
	}
	var parts []string
	for i := 0; i < len(days); {
		end := i
		for end+1 < len(days) && days[end+1] == days[end]+1 {
			end++
		}
		if end-i >= 2 {
			parts = append(parts, names[order[days[i]]]+".."+names[order[days[end]]])
		} else {
			for k := i; k <= end; k++ {
				parts = append(parts, names[order[days[k]]])
			}
		}
		i = end + 1
	}
	return strings.Join(parts, ",")
}

// environment resolves env: the values that hold no secret go into the
// unit, and the secret ones into the environment file.
func (j SystemdScheduledJob) environment(s value.Scope) (plain, secret map[string]string, err error) {
	plain, secret = map[string]string{}, map[string]string{}
	for _, k := range slices.Sorted(maps.Keys(j.Env)) {
		resolved, err := s.Resolve(j.Env[k])
		if err != nil {
			return nil, nil, fmt.Errorf("systemd_scheduled_job.env.%s: %w", k, err)
		}
		text, err := value.EnvironmentText(resolved)
		if err != nil {
			return nil, nil, fmt.Errorf("systemd_scheduled_job.env.%s: %w", k, err)
		}
		if strings.ContainsRune(text, 0) {
			return nil, nil, fmt.Errorf("systemd_scheduled_job.env.%s must not hold a null byte", k)
		}
		if value.HasSecrets(resolved) {
			secret[k] = text
		} else {
			plain[k] = text
		}
	}
	return plain, secret, nil
}

// scheduledJobBaseEnvironment is the fixed environment of a shell task,
// which the job's script gets too.
var scheduledJobBaseEnvironment = []string{"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C.UTF-8"}

// unitTexts are the job's service and timer units. MMX does all quoting: the
// script reaches bash exactly as written.
func (j SystemdScheduledJob) unitTexts(calendar string, plain map[string]string, environmentFile bool) (service, timer string) {
	var b strings.Builder
	b.WriteString(markerLine("systemd_scheduled_job", j.Name))
	fmt.Fprintf(&b, "[Unit]\nDescription=Scheduled job %s, run by %s.timer\n\n[Service]\nType=oneshot\n", j.Name, j.Name)
	fmt.Fprintf(&b, "User=%s\n", j.User)
	if j.Cwd != "" {
		// systemd reads this setting as a path, without quotes.
		fmt.Fprintf(&b, "WorkingDirectory=%s\n", strings.ReplaceAll(j.Cwd, "%", "%%"))
	}
	environment := slices.Clone(scheduledJobBaseEnvironment)
	for _, k := range slices.Sorted(maps.Keys(plain)) {
		environment = append(environment, k+"="+plain[k])
	}
	for _, assignment := range environment {
		fmt.Fprintf(&b, "Environment=%s\n", unitWord(assignment, false))
	}
	if environmentFile {
		fmt.Fprintf(&b, "EnvironmentFile=%s\n", j.EnvironmentFile())
	}
	fmt.Fprintf(&b, "ExecStart=/bin/bash -c %s\n", unitWord(j.Script, true))
	if j.StopARunAfter != "" {
		fmt.Fprintf(&b, "TimeoutStartSec=%s\n", unitDuration(j.StopARunAfter))
	}
	service = b.String()

	b.Reset()
	b.WriteString(markerLine("systemd_scheduled_job", j.Name))
	fmt.Fprintf(&b, "[Unit]\nDescription=Schedule of the job %s\n\n[Timer]\nOnCalendar=%s\n", j.Name, calendar)
	if j.RandomDelay != "" {
		fmt.Fprintf(&b, "RandomizedDelaySec=%s\n", unitDuration(j.RandomDelay))
	}
	fmt.Fprintf(&b, "Persistent=%t\n\n[Install]\nWantedBy=timers.target\n", j.RunMissedJobAfterDowntime)
	return service, b.String()
}

// unitWord quotes text as one word of a unit file setting: systemd reads
// C escapes inside double quotes and replaces %% with %. In a command line
// it also replaces $$ with $.
func unitWord(text string, command bool) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range text {
		switch {
		case r == '\\' || r == '"':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\t':
			b.WriteString(`\t`)
		case r < 0x20 || r == 0x7f:
			fmt.Fprintf(&b, `\x%02x`, r)
		case r == '%':
			b.WriteString("%%")
		case r == '$' && command:
			b.WriteString("$$")
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// unitDuration writes a duration as systemd reads it: whole seconds, or
// milliseconds.
func unitDuration(text string) string {
	d, _ := time.ParseDuration(text)
	if d%time.Second == 0 {
		return fmt.Sprintf("%ds", d/time.Second)
	}
	return fmt.Sprintf("%dms", d.Round(time.Millisecond)/time.Millisecond)
}

// environmentFileText writes the secret env values for systemd's
// EnvironmentFile=: each value in double quotes, where systemd reads \" \\ \`
// and \$ as the character itself.
func environmentFileText(secret map[string]string) string {
	var b strings.Builder
	for _, k := range slices.Sorted(maps.Keys(secret)) {
		b.WriteString(k + "=\"")
		for _, r := range secret[k] {
			if strings.ContainsRune("\"\\`$", r) {
				b.WriteByte('\\')
			}
			b.WriteRune(r)
		}
		b.WriteString("\"\n")
	}
	return b.String()
}

// systemdScheduledJobDescription is what mmx describe-module shows for
// systemd_scheduled_job.
var systemdScheduledJobDescription = Description{
	Name: "systemd_scheduled_job", Topic: TopicScheduledJobs, Schema: reflect.TypeOf(SystemdScheduledJob{}),
	Summary: "a command that systemd runs on a schedule: a oneshot service <name>.service and a timer <name>.timer, checked with systemd-analyze, with the timer enabled and started; output goes to the journal (journalctl -u <name>), and a run never starts while the previous one is still going",
	Fields: []Field{
		{Name: "name", Required: true, Notes: "the units are <name>.service and <name>.timer in /etc/systemd/system"},
		{Name: "script", Notes: "required unless state is absent; run with bash -c, exactly like shell.script: no login shell and no profile files"},
		{Name: "run_as_user_account", Required: true, Notes: "as on shell: runs with the named account's IDs and groups, and sets HOME, USER, and LOGNAME for the account"},
		{Name: "run_from_directory", Default: "/", Notes: "as on shell"},
		{Name: "env", Notes: "as on shell; a value that holds a secret goes into " + secretEnvironmentDirectory + "/<name>.env, which only root can read, never into the unit"},
		{Name: "cron_schedule", Notes: "five cron fields, such as \"30 4 * * 1\", which MMX translates to systemd's calendar form; a schedule that restricts both the day of the month and the day of the week is rejected, because cron and systemd read it differently; exactly one of cron_schedule or systemd_calendar_schedule"},
		{Name: "systemd_calendar_schedule", Notes: "a systemd calendar expression, such as Mon *-*-* 04:30:00 or daily, checked with systemd-analyze calendar; exactly one of cron_schedule or systemd_calendar_schedule"},
		{Name: "random_delay", Notes: "a duration, such as 15m, added at random to each start"},
		{Name: "run_missed_job_after_downtime", Default: "false", Notes: "true runs the job once after boot when its time passed while the node was off; false is what cron does"},
		{Name: "stop_a_run_after", Notes: "a duration, such as 2h; systemd stops a run that takes longer"},
		{Name: "state", Values: presenceStates, Default: "present", Notes: "absent accepts only name, state, and run_as_user_account. Stops and disables <name>.timer, then removes the timer unit, <name>.service, and the environment file. Refuses to remove a file without MMX's marker line"},
	},
	Example: "- systemd_scheduled_job:\n    name: reports-weekly\n    cron_schedule: \"30 4 * * 1\"\n    run_as_user_account: deploy\n    run_from_directory: /home/deploy/platform/current\n    script: bundle exec rake reports:weekly\n    env:\n      RAILS_ENV: production\n",
	Results: []string{"status", "change", "value.systemd_calendar_expression", "value.next_start_time"},
	Dryrun:  "supported; shows the unit differences, the calendar expression in systemd's form, the next start time, and whether the timer would be enabled, started, or restarted",
}

// Prepare checks the inputs when the project loads.
func (j *SystemdScheduledJob) Prepare(base string) error {
	return ValidateSystemdScheduledJob(*j)
}

// Reads lists the script, which runs as written: values reach it through env.
func (j *SystemdScheduledJob) Reads() Reads {
	return Reads{ShellScripts: map[string]string{"script": j.Script}}
}

// Arguments lists the inputs that may hold a reference, with their rules.
func (j *SystemdScheduledJob) Arguments(arguments *ArgumentList) {
	arguments.Public("name", &j.Name, scheduledJobNameRule)
	arguments.Public("run_as_user_account", &j.User, accountNameRule)
	if j.State == "absent" {
		return
	}
	if j.CronSchedule != "" {
		arguments.Public("cron_schedule", &j.CronSchedule, systemdCronScheduleRule)
	}
	if j.CalendarSchedule != "" {
		arguments.Public("systemd_calendar_schedule", &j.CalendarSchedule, calendarScheduleRule)
	}
	if j.Cwd != "" {
		arguments.Public("run_from_directory", &j.Cwd, unitDirectory)
	}
	for _, k := range slices.Sorted(maps.Keys(j.Env)) {
		arguments.Private("env."+k, j.Env[k], environmentValue)
	}
}

// Run does the module's work on the node.
func (j *SystemdScheduledJob) Run(ctx context.Context, task NodeTask) protocol.Result {
	return task.Runtime.SystemdScheduledJob(ctx, *j, task.Scope)
}

// ManagedPaths lists the units and the environment file; a dryrun cannot
// check a later task's path_exists on them.
func (j *SystemdScheduledJob) ManagedPaths() []string {
	return []string{filepath.Join("/etc/systemd/system", j.Name+".timer"), filepath.Join("/etc/systemd/system", j.Name+".service"), j.EnvironmentFile()}
}
