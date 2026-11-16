package modules

import (
	"context"
	"errors"
	"fmt"
	"hash"
	"maps"
	"os"
	"os/user"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"mmx/internal/protocol"
	"mmx/internal/value"
)

// CronJob is one cron entry in its own file, /etc/cron.d/<name>, for teams
// that keep their schedules in cron. The file is written like a file task,
// root-owned with mode 0644, and starts with MMX's marker line.
func (run Runtime) CronJob(ctx context.Context, c CronJob, s value.Scope) protocol.Result {
	if err := ValidateCronJob(c); err != nil {
		return protocol.ConfigurationError(err)
	}
	path := c.Path()
	if c.State == "absent" {
		return run.removeMarkedFile(ctx, path, managedMarker("cron_job"), "a cron_job task with state: absent")
	}
	if !cronInstalled(ctx) {
		return run.notYet(missingProgramError("cron"))
	}
	if _, err := user.Lookup(c.User); err != nil {
		return run.accountFailure(fmt.Errorf("cron_job.run_as_user_account: %w", err))
	}
	text, err := c.Text(s)
	if err != nil {
		return protocol.ConfigurationError(err)
	}
	if err := refuseForeignFile(path, managedMarker("cron_job"), "cron_job"); err != nil {
		return protocol.Failed(err)
	}
	return run.File(ctx, c.file(text), s)
}

// cronFiles is where cron_job keeps its files and the account that owns
// them. Tests point it at a temporary directory of the test's own account.
var cronFiles = struct {
	directory    string
	owner, group Account
}{"/etc/cron.d", "root", "root"}

// cronInstalled reports whether a cron daemon is installed: cron on Debian
// and Ubuntu, crond (package cronie) on the Red Hat family,
// with its directory for entry files.
func cronInstalled(ctx context.Context) bool {
	if info, err := os.Stat(cronFiles.directory); err != nil || !info.IsDir() {
		return false
	}
	for _, program := range []string{"cron", "crond"} {
		if _, err := lookPath(ctx, program); err == nil {
			return true
		}
		if _, err := os.Stat("/usr/sbin/" + program); err == nil {
			return true
		}
	}
	return false
}

// managedMarker is the first line of each file a module writes and owns. A
// removal refuses a file without it, because another tool or a person wrote
// that file.
func managedMarker(module string) string {
	return "# Managed by MMX: " + module + "."
}

func markerLine(module, name string) string {
	return managedMarker(module) + " Written by the node task for " + name + "; MMX replaces a change made here on its next run.\n"
}

func hasMarker(text, marker string) bool {
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, marker) {
			return true
		}
	}
	return false
}

// removeMarkedFile removes a regular file that holds the marker line, and
// refuses one that lacks it.
func (run Runtime) removeMarkedFile(ctx context.Context, path, marker, task string) protocol.Result {
	parent, err := os.OpenRoot(filepath.Dir(path))
	if os.IsNotExist(err) {
		return protocol.Success("unchanged")
	}
	if err != nil {
		return protocol.Failed(err)
	}
	defer parent.Close()
	name := filepath.Base(path)
	info, err := entryInfoAt(parent, name, false)
	if err != nil || info == nil {
		if err != nil {
			return protocol.Failed(fmt.Errorf("%s: %w", path, err))
		}
		return protocol.Success("unchanged")
	}
	if err := refuseUnmarked(parent, path, marker, task); err != nil {
		return protocol.Failed(err)
	}
	return run.removeAt(ctx, parent, path, info, fileState(info))
}

// refuseUnmarked fails for a file that lacks the marker line.
func refuseUnmarked(parent *os.Root, path, marker, task string) error {
	text, err := readTextAt(parent, filepath.Base(path))
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if !hasMarker(text, marker) {
		return fmt.Errorf("refusing to remove %s: the file lacks the MMX marker line %q, so another tool or a person wrote the file; %s removes only files MMX wrote. Remove the unmarked file by hand, or with a file task (state: absent)", path, marker+" ...", task)
	}
	return nil
}

// refuseForeignFile fails when path is an existing file without the marker
// line: a job with that name would overwrite a file that another tool or a
// person wrote, such as a package's own /etc/cron.d/certbot.
func refuseForeignFile(path, marker, module string) error {
	parent, err := os.OpenRoot(filepath.Dir(path))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer parent.Close()
	info, err := entryInfoAt(parent, filepath.Base(path), false)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if info == nil {
		return nil
	}
	text, err := readTextAt(parent, filepath.Base(path))
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if !hasMarker(text, marker) {
		return fmt.Errorf("refusing to write %s: the file exists and lacks the MMX marker line %q, so another tool or a person wrote it, and %s would overwrite it. Choose another name for the %s task", path, marker+" ...", module, module)
	}
	return nil
}

// CronJob is the input of cron_job.
type CronJob struct {
	Name         string         `yaml:"name" json:"name"`
	State        string         `yaml:"state,omitempty" json:"state,omitempty"`
	CronSchedule string         `yaml:"cron_schedule,omitempty" json:"cron_schedule,omitempty"`
	User         string         `yaml:"run_as_user_account,omitempty" json:"run_as_user_account,omitempty"`
	Command      string         `yaml:"command,omitempty" json:"command,omitempty"`
	Env          map[string]any `yaml:"env,omitempty" json:"env,omitempty"`
}

// Path is the cron_job's file.
func (c CronJob) Path() string { return filepath.Join(cronFiles.directory, c.Name) }

func (c CronJob) file(text string) File {
	return File{Path: c.Path(), Literal: &text, Owner: cronFiles.owner, Group: cronFiles.group, Mode: "0644"}
}

// cronFileName: cron reads a file in /etc/cron.d only when its name has
// letters, digits, - and _.
var cronFileName = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

var cronNameRule = rule("cron_job.name must use only letters, digits, - and _, because cron ignores files with other names in /etc/cron.d", cronFileName.MatchString)

// cronWords are cron's @ words that stand for a schedule.
var cronWords = []string{"@reboot", "@yearly", "@annually", "@monthly", "@weekly", "@daily", "@midnight", "@hourly"}

func ValidateCronJob(c CronJob) error {
	if err := validatePresence(c.State); err != nil {
		return invalid("state", "cron_job.state must be present or absent")
	}
	if c.State == "absent" {
		if c.CronSchedule != "" || c.User != "" || c.Command != "" || c.Env != nil {
			return fmt.Errorf("cron_job with state: absent accepts only name and state")
		}
		return nil
	}
	switch {
	case c.CronSchedule == "":
		return invalid("cron_schedule", "cron_job.cron_schedule is required: five cron fields, such as \"15 3 * * *\", or one of cron's @ words, such as @daily")
	case c.User == "":
		return invalid("run_as_user_account", "cron_job.run_as_user_account is required: cron needs the account on every entry in /etc/cron.d")
	case strings.TrimSpace(c.Command) == "":
		return invalid("command", "cron_job.command is required: the one line cron runs with /bin/sh")
	case strings.ContainsAny(c.Command, "\r\n"):
		return invalid("command", "cron_job.command must be one line; for a longer script, use systemd_scheduled_job, or install the script with a file task and set cron_job.command to run the script")
	case strings.Contains(c.Command, `\%`):
		return invalid("command", `cron_job.command must not have a backslash directly before %: cron cannot pass that pair to a command. Write the % without the backslash; MMX escapes every % for cron`)
	}
	if err := validateEnvNames(c.Env); err != nil {
		return err
	}
	for _, k := range slices.Sorted(maps.Keys(c.Env)) {
		if text, ok := c.Env[k].(string); ok && readsSecret(text) {
			return invalid("env."+k, cronSecretMessage)
		}
	}
	return nil
}

const cronSecretMessage = "a secret cannot go into a cron file, which every account on the node can read; use systemd_scheduled_job, which keeps a secret in a file only root can read"

// readsSecret reports text whose references read the project's secrets.
func readsSecret(text string) bool {
	for _, m := range value.Reference.FindAllStringSubmatch(value.WithoutLiterals(text), -1) {
		if parts, err := value.Path(m[1]); err == nil && len(parts) > 0 && parts[0].Name == "secrets" {
			return true
		}
	}
	return false
}

// cronEnvValue is the rule for an env value of cron_job: one line of text,
// never a secret.
func cronEnvValue(v any) error {
	if value.HasSecrets(v) {
		return errors.New(cronSecretMessage)
	}
	text, err := value.Scalar(v)
	if err != nil {
		return err
	}
	if strings.ContainsAny(text, "\r\n\x00") {
		return errors.New("each cron_job.env value must be one line")
	}
	return nil
}

// Text is the cron file: the marker line, the env lines, and the entry, with
// each % escaped for cron.
func (c CronJob) Text(s value.Scope) (string, error) {
	var b strings.Builder
	b.WriteString(markerLine("cron_job", c.Name))
	for _, k := range slices.Sorted(maps.Keys(c.Env)) {
		resolved, err := s.Resolve(c.Env[k])
		if err == nil {
			err = cronEnvValue(resolved)
		}
		if err != nil {
			return "", fmt.Errorf("cron_job.env.%s: %w", k, err)
		}
		text, _ := value.Scalar(resolved)
		line, err := cronEnvLine(k, text)
		if err != nil {
			return "", fmt.Errorf("cron_job.env.%s: %w", k, err)
		}
		b.WriteString(line)
	}
	fmt.Fprintf(&b, "%s %s %s\n", strings.Join(strings.Fields(c.CronSchedule), " "), c.User, strings.ReplaceAll(strings.TrimSpace(c.Command), "%", `\%`))
	return b.String(), nil
}

// cronEnvLine writes NAME=value. cron removes one pair of matching quotes
// around a value, which keeps blanks at its ends; a value that starts with a
// quote of its own is quoted too.
func cronEnvLine(name, text string) (string, error) {
	quote := text == "" || strings.TrimSpace(text) != text || strings.HasPrefix(text, `"`) || strings.HasPrefix(text, `'`)
	switch {
	case !quote:
		return name + "=" + text + "\n", nil
	case !strings.Contains(text, `"`):
		return name + `="` + text + "\"\n", nil
	case !strings.Contains(text, `'`):
		return name + "='" + text + "'\n", nil
	}
	return "", errors.New("cron cannot preserve an environment variable value containing single and double quotes when the value has leading or trailing whitespace or starts with a quote")
}

// cronField is one of the five fields of a cron schedule.
type cronField struct {
	name     string
	min, max int
	// names are the three-letter names cron accepts, from min on.
	names []string
}

var cronFields = [5]cronField{
	{"minute", 0, 59, nil},
	{"hour", 0, 23, nil},
	{"day of month", 1, 31, nil},
	{"month", 1, 12, []string{"jan", "feb", "mar", "apr", "may", "jun", "jul", "aug", "sep", "oct", "nov", "dec"}},
	{"day of week", 0, 7, []string{"sun", "mon", "tue", "wed", "thu", "fri", "sat"}},
}

// cronSchedule is a parsed cron schedule: its five fields as written and the
// values each one selects, or one of cron's @ words.
type cronSchedule struct {
	word   string
	fields [5]string
	values [5][]bool
}

const cronFieldsExample = "five cron fields (minute, hour, day of month, month, day of week), such as \"30 4 * * 1\""

// parseCronSchedule reads a cron schedule. withWords accepts cron's @ words
// such as @daily.
func parseCronSchedule(text string, withWords bool) (cronSchedule, error) {
	var s cronSchedule
	fields := strings.Fields(text)
	if len(fields) == 1 && strings.HasPrefix(fields[0], "@") {
		if withWords && slices.Contains(cronWords, fields[0]) {
			s.word = fields[0]
			return s, nil
		}
		if withWords {
			return s, fmt.Errorf("cron_schedule %q is not one of cron's @ words: %s", fields[0], strings.Join(cronWords, ", "))
		}
		return s, fmt.Errorf("cron_schedule takes %s; for a schedule such as %s, write systemd_calendar_schedule instead, such as daily or weekly", cronFieldsExample, fields[0])
	}
	if len(fields) != 5 {
		if withWords {
			return s, fmt.Errorf("cron_schedule must be %s, or one of cron's @ words such as @daily; it has %d fields", cronFieldsExample, len(fields))
		}
		return s, fmt.Errorf("cron_schedule must be %s; it has %d fields", cronFieldsExample, len(fields))
	}
	for i, field := range fields {
		values, err := cronFields[i].parse(field)
		if err != nil {
			return s, fmt.Errorf("cron_schedule field %d (%s) %q: %w", i+1, cronFields[i].name, field, err)
		}
		s.fields[i], s.values[i] = field, values
	}
	return s, nil
}

// parse reads one field: a comma-separated list of *, a value, or a range
// a-b, each optionally followed by /step.
func (f cronField) parse(text string) ([]bool, error) {
	selected := make([]bool, f.max+1)
	for _, part := range strings.Split(text, ",") {
		span, stepText, stepped := strings.Cut(part, "/")
		step := 1
		if stepped {
			n, err := strconv.Atoi(stepText)
			if err != nil || n < 1 {
				return nil, fmt.Errorf("the cron step after / must be a whole number greater than or equal to 1")
			}
			step = n
		}
		var low, high int
		switch first, last, isRange := strings.Cut(span, "-"); {
		case span == "*":
			low, high = f.min, f.max
		case isRange:
			var err error
			if low, err = f.value(first); err != nil {
				return nil, err
			}
			if high, err = f.value(last); err != nil {
				return nil, err
			}
			if low > high {
				return nil, fmt.Errorf("the range %s runs backwards; cron ranges go from the smaller value to the larger", span)
			}
		default:
			if stepped {
				return nil, fmt.Errorf("a step needs * or a range before it, such as */15 or 0-30/15")
			}
			var err error
			if low, err = f.value(span); err != nil {
				return nil, err
			}
			high = low
		}
		for v := low; v <= high; v += step {
			selected[v] = true
		}
	}
	// Day of week 7 is Sunday, like 0.
	if f.max == 7 && selected[7] {
		selected[0], selected[7] = true, false
	}
	return selected, nil
}

func (f cronField) value(text string) (int, error) {
	if i := slices.Index(f.names, strings.ToLower(text)); i >= 0 {
		return f.min + i, nil
	}
	n, err := strconv.Atoi(text)
	if err != nil || n < f.min || n > f.max {
		if f.names != nil {
			return 0, fmt.Errorf("%q is not a valid %s: use %d to %d or %s to %s", text, f.name, f.min, f.max, f.names[0], f.names[len(f.names)-1])
		}
		return 0, fmt.Errorf("%q is not a valid %s: use %d to %d", text, f.name, f.min, f.max)
	}
	return n, nil
}

// cronScheduleRule is the rule for cron_job.cron_schedule.
var cronScheduleRule = parsed(func(s string) error { _, err := parseCronSchedule(s, true); return err })

// cronJobDescription is what mmx describe-module shows for cron_job.
var cronJobDescription = Description{
	Name: "cron_job", Topic: TopicScheduledJobs, Schema: reflect.TypeOf(CronJob{}),
	Summary: "one cron entry in its own file, /etc/cron.d/<name>, root-owned with mode 0644 and MMX's marker line; cron must be installed (package cron on Debian and Ubuntu, cronie on the Red Hat family); systemd_scheduled_job is the recommended alternative",
	Fields: []Field{
		{Name: "name", Required: true, Notes: "the file's name: letters, digits, - and _ only, because cron ignores other file names"},
		{Name: "cron_schedule", Required: true, Notes: "five cron fields (minute, hour, day of month, month, day of week), such as \"15 3 * * *\", or one of cron's @ words: " + strings.Join(cronWords, ", ")},
		{Name: "run_as_user_account", Required: true, Notes: "the account cron runs the command as; cron needs it on every entry in /etc/cron.d"},
		{Name: "command", Required: true, Notes: "one line, run by cron with /bin/sh; write % as it should reach the command, MMX escapes it for cron"},
		{Name: "env", Notes: "variable lines written above the entry, such as MAILTO or PATH; a secret is rejected, because every account can read the file"},
		{Name: "state", Values: presenceStates, Default: "present", Notes: "absent accepts only name and state; it removes a file that has MMX's marker line and refuses one that lacks it"},
	},
	Example: "- cron_job:\n    name: rotate-upload-cache\n    cron_schedule: \"15 3 * * *\"\n    run_as_user_account: www-data\n    command: /usr/local/bin/rotate-upload-cache --keep 14\n",
	Results: resultBasic,
	Dryrun:  "supported; shows the file's text difference; a node without cron is not checked",
}

// Prepare checks the inputs when the project loads.
func (c *CronJob) Prepare(base string) error {
	return ValidateCronJob(*c)
}

// Reads lists the command, which runs as written: values reach it through env.
func (c *CronJob) Reads() Reads {
	return Reads{ShellScripts: map[string]string{"command": c.Command}}
}

// Arguments lists the inputs that may hold a reference, with their rules.
func (c *CronJob) Arguments(arguments *ArgumentList) {
	arguments.Public("name", &c.Name, cronNameRule)
	if c.State == "absent" {
		return
	}
	arguments.Public("cron_schedule", &c.CronSchedule, cronScheduleRule)
	arguments.Public("run_as_user_account", &c.User, accountNameRule)
	for _, k := range slices.Sorted(maps.Keys(c.Env)) {
		arguments.Private("env."+k, c.Env[k], cronEnvValue)
	}
}

// Run does the module's work on the node.
func (c *CronJob) Run(ctx context.Context, task NodeTask) protocol.Result {
	return task.Runtime.CronJob(ctx, *c, task.Scope)
}

// ManagedPaths lists the file the task manages; a dryrun cannot check a later
// task's path_exists on it.
func (c *CronJob) ManagedPaths() []string {
	return []string{c.Path()}
}

// TrackedInputs lists the cron file.
func (c *CronJob) TrackedInputs() []TrackedInput {
	return []TrackedInput{{Kind: "config", Path: c.Path(), Content: c}}
}

// HashConfiguration hashes the cron file, or with apply the file the task
// would leave, resolving its env values in s.
func (c *CronJob) HashConfiguration(ctx context.Context, h hash.Hash, apply bool, s value.Scope) error {
	if !apply || c.State == "absent" {
		f := File{Path: c.Path(), State: c.State}
		return f.HashConfiguration(ctx, h, apply, s)
	}
	text, err := c.Text(s)
	if err != nil {
		return err
	}
	f := c.file(text)
	return f.HashConfiguration(ctx, h, apply, s)
}
