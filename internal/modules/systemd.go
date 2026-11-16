package modules

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math"
	"os"
	"os/user"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode"

	"golang.org/x/sys/unix"

	"mmx/internal/protocol"
)

type ServiceState struct {
	Unit, Instance, Load, Active, Enabled string
	Names                                 []string // the unit's names, including aliases
	NeedDaemonReload                      bool
	// RunsOnceAndEnds: a Type=oneshot service without RemainAfterExit=yes,
	// which is never active after it ran.
	RunsOnceAndEnds bool
	// GeneratedFrom is the file from which a generator of systemd made the
	// unit, such as a Podman Quadlet file, when systemd names it.
	GeneratedFrom string
}

// Masked reports whether the unit is masked, in /etc or only until the next boot.
func (s ServiceState) Masked() bool {
	return s.Enabled == "masked" || s.Enabled == "masked-runtime"
}

// UnitManager is the systemd a unit belongs to: the system's, or an account's
// own, which runs that account's units without root.
type UnitManager struct {
	// Account is empty for the system's systemd.
	Account    string
	Home       string
	UID, GID   int
	credential *syscall.Credential
	env        map[string]string
}

// errRootHasNoSystemdOfItsOwn is SystemdOf's answer for root.
var errRootHasNoSystemdOfItsOwn = errors.New("root's units are system units; remove in_systemd_of_user_account")

// SystemdOf returns the system's systemd for an empty account and otherwise
// the account's own. The account must exist and must not be root, whose units
// are system units.
func SystemdOf(account string) (UnitManager, error) {
	if account == "" {
		return UnitManager{}, nil
	}
	env := map[string]string{}
	credential, err := shellAccount(account, env)
	if err != nil {
		return UnitManager{}, err
	}
	if credential.Uid == 0 {
		return UnitManager{}, errRootHasNoSystemdOfItsOwn
	}
	m := UnitManager{Account: account, Home: env["HOME"], UID: int(credential.Uid), GID: int(credential.Gid), credential: credential, env: env}
	env["XDG_RUNTIME_DIR"] = m.runtimeDirectory()
	return m, nil
}

// AccountIsGone reports SystemdOf's error for an account that does not exist.
// Such an account has no unit that runs or is enabled, so a task that only
// stops, disables, or removes one of its units has nothing to do, and a
// decommission playbook reruns unchanged after it removed the account.
func AccountIsGone(err error) bool {
	var unknown user.UnknownUserError
	return errors.As(err, &unknown)
}

// accountRuntimeDirectories holds each account's runtime directory, named
// for its UID, where the account's systemd listens. Tests point it elsewhere.
var accountRuntimeDirectories = "/run/user"

func (m UnitManager) runtimeDirectory() string {
	return filepath.Join(accountRuntimeDirectories, strconv.Itoa(m.UID))
}

// UnitDirectory is where the manager reads the unit files MMX installs.
func (m UnitManager) UnitDirectory() string {
	if m.Account == "" {
		return "/etc/systemd/system"
	}
	return filepath.Join(m.Home, ".config/systemd/user")
}

// Key names a unit of this manager in the engine's records: the unit name
// for a system unit, and the account with the unit for an account's unit.
func (m UnitManager) Key(unit string) string {
	if m.Account == "" {
		return unit
	}
	return "user/" + m.Account + "/" + unit
}

// Running reports whether the manager can be asked. The system's always can;
// an account's runs while the account is logged in or lingering.
func (m UnitManager) Running() bool {
	if m.Account == "" {
		return true
	}
	_, err := os.Stat(m.socket())
	return err == nil
}

// socket is where an account's systemd listens for systemctl. It is there
// on every distribution; the account's D-Bus socket beside it is not, since
// Debian installs that only with its dbus-user-session package.
func (m UnitManager) socket() string {
	return filepath.Join(m.runtimeDirectory(), "systemd/private")
}

// Lingering reports whether the account's systemd starts at boot and keeps
// running after logout. systemd-logind records it with this file.
func (m UnitManager) Lingering() bool {
	_, err := os.Stat("/var/lib/systemd/linger/" + m.Account)
	return m.Account == "" || err == nil
}

// LingerNotice is what a task reports when it turned lingering on.
func (m UnitManager) LingerNotice() string {
	return "Enabled lingering for account " + m.Account + ", so its services start at boot and keep running after logout"
}

// wouldLingerNotice is what a dryrun reports for a task that would turn
// lingering on.
func (m UnitManager) wouldLingerNotice() string {
	return "would enable lingering for account " + m.Account + ", so its services start at boot and keep running after logout"
}

// EnableLinger turns lingering on for the account and waits until its systemd
// answers. A unit MMX manages for an account must not stop at logout.
func (run Runtime) EnableLinger(ctx context.Context, m UnitManager) protocol.Result {
	if err := run.Mutation("lingering for account " + m.Account); err != nil {
		return protocol.Failed(err)
	}
	for _, command := range [][]string{{"loginctl", "enable-linger", m.Account}, {"systemctl", "start", "user@" + strconv.Itoa(m.UID) + ".service"}} {
		if r := Command(ctx, nil, "", command[0], command[1:]...); r.Status != "succeeded" {
			r.Change, r.Reason = "unknown", strings.Join(command, " ")+": "+r.Reason+": "+strings.TrimSpace(r.Stderr)
			return r
		}
	}
	if !m.Running() {
		r := protocol.Failed(fmt.Errorf("the systemd of account %s does not answer after lingering was enabled: %s is missing", m.Account, m.socket()))
		r.Change = "changed"
		return r
	}
	return protocol.Success("changed")
}

// systemctl asks this manager. An account's own systemd is asked as that
// account, through the account's runtime directory.
func (m UnitManager) systemctl(ctx context.Context, args ...string) protocol.Result {
	if m.Account == "" {
		return Command(ctx, nil, "", "systemctl", args...)
	}
	return m.runAsAccount(ctx, "systemctl", append([]string{"--user"}, args...)...)
}

// runAsAccount runs a program as this manager's account, with the account's
// home and runtime directory in its environment, as the account's own
// systemd starts the account's services. The system's manager runs it as
// root.
func (m UnitManager) runAsAccount(ctx context.Context, program string, args ...string) protocol.Result {
	if m.Account == "" {
		return Command(ctx, nil, "/", program, args...)
	}
	return runProcess(ctx, m.env, "/", nil, m.credential, program, args...).Result
}

// systemctlOffline runs systemctl --user as the account on the account's
// unit files alone, for an account whose systemd is not running:
// SYSTEMD_OFFLINE=1 makes systemctl read and change the files itself instead
// of asking that systemd, and start nothing.
func (m UnitManager) systemctlOffline(ctx context.Context, args ...string) protocol.Result {
	env := map[string]string{}
	maps.Copy(env, m.env)
	env["SYSTEMD_OFFLINE"] = "1"
	return runProcess(ctx, env, "/", nil, m.credential, "systemctl", append([]string{"--user"}, args...)...).Result
}

// inspectUnitFiles reads a unit of an account whose systemd is not running.
// Such a systemd runs none of the account's units, so the unit is inactive;
// whether it is there, enabled, or masked, the account's unit files say.
func (m UnitManager) inspectUnitFiles(ctx context.Context, name string) (ServiceState, error) {
	r := m.systemctlOffline(ctx, "is-enabled", "--", name)
	state := ServiceState{Unit: name, Load: "loaded", Active: "inactive", Enabled: strings.TrimSpace(r.Stdout)}
	switch {
	// systemd 252 names the missing file; newer versions print not-found.
	case state.Enabled == "not-found" || (state.Enabled == "" && strings.Contains(r.Stderr, "No such file or directory")):
		state.Load, state.Enabled = "not-found", ""
	case state.Enabled == "":
		return ServiceState{}, fmt.Errorf("read the unit files of account %s for %s: %s: %s", m.Account, name, r.Reason, strings.TrimSpace(r.Stderr))
	case state.Masked():
		state.Load = "masked"
	}
	return state, nil
}

func InspectService(ctx context.Context, name string) (ServiceState, error) {
	return UnitManager{}.Inspect(ctx, name)
}

func ServiceAction(ctx context.Context, action, name string) protocol.Result {
	return UnitManager{}.Action(ctx, action, name)
}

// Inspect reads a unit's state. For an account whose systemd is not running,
// it reads the account's unit files and starts nothing (inspectUnitFiles).
func (m UnitManager) Inspect(ctx context.Context, name string) (ServiceState, error) {
	if !ServiceName.MatchString(name) {
		return ServiceState{}, fmt.Errorf("invalid service name %q", name)
	}
	name = UnitName(name)
	if !m.Running() {
		return m.inspectUnitFiles(ctx, name)
	}
	r := m.systemctl(ctx, "show", "--property=Id,Names,InvocationID,LoadState,ActiveState,UnitFileState,NeedDaemonReload,Type,RemainAfterExit,SourcePath", "--", name)
	if r.Status != "succeeded" {
		return ServiceState{}, fmt.Errorf("inspect %s: %s: %s", name, r.Reason, r.Stderr)
	}
	p := map[string]string{}
	for _, line := range strings.Split(r.Stdout, "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			p[k] = v
		}
	}
	if p["Id"] != "" {
		name = p["Id"]
	}
	if !ServiceName.MatchString(name) || UnitName(name) != name {
		return ServiceState{}, fmt.Errorf("invalid systemd unit name %q", name)
	}
	state := ServiceState{name, p["InvocationID"], p["LoadState"], p["ActiveState"], p["UnitFileState"], strings.Fields(p["Names"]), p["NeedDaemonReload"] == "yes", p["Type"] == "oneshot" && p["RemainAfterExit"] != "yes", ""}
	if state.Enabled == "generated" {
		state.GeneratedFrom = p["SourcePath"]
		// systemd can neither enable nor disable a generated unit, with one
		// exception: the unit it generates from an init script. There
		// systemctl enable and disable hand over to the distribution's own
		// tool, and is-enabled asks that tool.
		if directory := filepath.Dir(state.GeneratedFrom); directory == "/etc/init.d" || directory == "/etc/rc.d/init.d" {
			if answer := strings.TrimSpace(m.systemctl(ctx, "is-enabled", "--", name).Stdout); answer == "enabled" || answer == "disabled" {
				state.Enabled = answer
			}
		}
	}
	if state.Active == "active" && state.Instance == "" {
		return state, fmt.Errorf("active systemd unit %s has no invocation ID", name)
	}
	return state, nil
}

// Action runs one systemctl verb on a unit of this manager. For an account
// whose systemd is not running, a verb that changes only unit files changes
// them offline. Any other verb still asks that systemd and fails: offline,
// systemctl would ignore it and succeed.
func (m UnitManager) Action(ctx context.Context, action, name string) protocol.Result {
	args := []string{action, "--", name}
	switch action {
	case "disable-runtime":
		args = []string{"disable", "--runtime", "--", name}
	case "unmask-runtime":
		args = []string{"unmask", "--runtime", "--", name}
	}
	ask := m.systemctl
	switch action {
	case "enable", "disable", "mask", "unmask":
		if !m.Running() {
			ask = m.systemctlOffline
		}
	}
	r := ask(ctx, args...)
	// systemd masks by placing a link in its unit directory and refuses when
	// a unit file is already there.
	if action == "mask" && r.Status != "succeeded" && strings.Contains(r.Stderr, "already exists") {
		r.Reason = "systemd cannot mask " + name + " while its unit file is in " + m.UnitDirectory() + "; remove that file first with a systemd_unit task that says state: absent"
	}
	return r
}

// JournalTime is a reading of the node's monotonic clock in microseconds.
// The journal stamps every entry it receives with this clock
// (__MONOTONIC_TIMESTAMP), so a reading taken before an action tells the
// entries of that action from older ones. The wall clock can jump; this one
// cannot.
type JournalTime uint64

// JournalNow reads the clock of JournalTime. Take it before asking systemd
// for a start, restart, or reload, and give it to JournalAfterFailedAction.
func JournalNow() JournalTime {
	var now unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_MONOTONIC, &now); err != nil {
		// No entry is newer than this, so a failure keeps its own message.
		return math.MaxUint64
	}
	return JournalTime(now.Nano() / 1000)
}

// The limits of what the journal adds to the reason of a failed action.
const (
	journalLinesShown    = 10   // lines of one unit
	journalLineLimit     = 300  // bytes of one line
	journalAdditionLimit = 4000 // bytes of everything together
	neededUnitsShown     = 3
	// accountLinesRead is how many of an account's newest printed lines are
	// searched for those that the journal recorded without a unit.
	accountLinesRead = 100
)

// journalReadTimeLimit bounds everything JournalAfterFailedAction runs.
// journalctl answers within milliseconds; the limit is for a journal that
// does not answer at all. Tests shorten it.
var journalReadTimeLimit = 5 * time.Second

// JournalAfterFailedAction returns what the reason of a failed start,
// restart, or reload gains from the journal: the last lines that the unit's
// programs and its systemd wrote since asked, the time just before the action.
// When systemd did not start the unit because a unit that it needs is not
// running, the text says so and holds that unit's lines instead. The text
// begins with a line break. It is empty when the journal cannot be read or
// has no line since asked, so the failure then keeps its message as it is,
// and reading the journal never fails a task.
func (m UnitManager) JournalAfterFailedAction(ctx context.Context, action, unit string, asked JournalTime) string {
	ctx, cancel := context.WithTimeout(ctx, journalReadTimeLimit)
	defer cancel()
	// An account reads its own journal where it may. It may not where the
	// journal is kept only in memory, as on Rocky Linux 9, and never for an
	// account with a system UID, whose lines systemd keeps in the system's
	// journal. Root then reads the same entries: _UID is a field that only
	// the journal itself sets, so the filter is exact.
	reader := m
	entries := readJournal(ctx, reader, asked, journalLinesShown, m.journalMatches(unit)...)
	if len(entries) == 0 && m.Account != "" {
		reader = UnitManager{}
		entries = readJournal(ctx, reader, asked, journalLinesShown, m.journalMatches(unit)...)
	}
	if len(entries) == 0 {
		return ""
	}
	manager, ofAccount, during := "systemd", "", " during this "+action
	if m.Account != "" {
		manager, ofAccount = "The systemd of account "+m.Account, " of account "+m.Account
	}
	text := ""
	// systemd records a job that it gave up because of another unit with the
	// result "dependency".
	if slices.ContainsFunc(entries, func(entry journalEntry) bool { return entry.JobResult == "dependency" }) {
		for _, needed := range m.neededUnitsNotRunning(ctx, unit) {
			text += fmt.Sprintf("\n%s did not %s %s, because %s, which it needs, %s.", manager, action, unit, needed.unit, needed.state)
			text += journalLines("Last lines of the systemd journal for "+needed.unit+ofAccount+during+":",
				readJournal(ctx, reader, asked, journalLinesShown, m.journalMatches(needed.unit)...))
		}
	}
	if text == "" {
		text = journalLines("Last lines of the systemd journal for "+unit+ofAccount+during+":", entries)
	}
	if m.Account != "" {
		// The journal finds the unit of a line by looking at the process
		// that printed it. A process that prints its error and ends is
		// sometimes gone by then, most often a program that the unit's
		// command started, such as the program of a wrapper script. For a
		// system unit the journal then takes the unit's name from the
		// system's systemd. From the systemd of an account it accepts no
		// name, so the entry has the account's UID and no unit. Such a line
		// is the one that explains a failed start, so the account's printed
		// lines without a unit since asked are shown too, under a heading
		// of their own, because another unit of the account can have
		// printed them.
		var withoutUnit []journalEntry
		for _, entry := range readJournal(ctx, reader, asked, accountLinesRead, "_UID="+strconv.Itoa(m.UID), "_TRANSPORT=stdout") {
			if entry.ControlGroup == "" && entry.SystemUnit == "" {
				withoutUnit = append(withoutUnit, entry)
			}
		}
		if len(withoutUnit) > journalLinesShown {
			withoutUnit = withoutUnit[len(withoutUnit)-journalLinesShown:]
		}
		text += journalLines("Lines that programs of account "+m.Account+" printed"+during+
			", for which the systemd journal recorded no unit because the program ended at once:", withoutUnit)
	}
	if len(text) > journalAdditionLimit {
		text = strings.ToValidUTF8(text[:journalAdditionLimit], "") + " ..."
	}
	return text
}

// journalEntry is what a failed action reads of one entry of the journal.
type journalEntry struct {
	Message   journalField `json:"MESSAGE"`
	JobResult journalField `json:"JOB_RESULT"`
	Received  journalField `json:"__MONOTONIC_TIMESTAMP"`
	// ControlGroup and SystemUnit are both empty when the journal could not
	// tell which unit wrote the entry.
	ControlGroup journalField `json:"_SYSTEMD_CGROUP"`
	SystemUnit   journalField `json:"_SYSTEMD_UNIT"`
}

// journalField is the text of one field of an entry. journalctl
// --output=json prints a field as a string, or as an array of byte values
// when the field holds bytes that are not printable text, such as a color
// sequence. A field that an entry has more than once stays empty.
type journalField string

func (f *journalField) UnmarshalJSON(data []byte) error {
	var text string
	var values []int
	if json.Unmarshal(data, &text) == nil {
		*f = journalField(text)
	} else if json.Unmarshal(data, &values) == nil {
		raw := make([]byte, len(values))
		for i, value := range values {
			raw[i] = byte(value)
		}
		*f = journalField(raw)
	}
	return nil
}

// journalMatches selects the entries of one unit of this manager: what the
// unit's programs wrote, and what the manager wrote about the unit.
func (m UnitManager) journalMatches(unit string) []string {
	if m.Account == "" {
		return []string{"--unit=" + unit}
	}
	// journalctl --user-unit would filter on the UID of whoever runs it, so
	// root could not use it for an account.
	uid := "_UID=" + strconv.Itoa(m.UID)
	return []string{"_SYSTEMD_USER_UNIT=" + unit, uid, "+", "USER_UNIT=" + unit, uid}
}

// readJournal returns the newest count entries of this boot that match and
// that the journal received at or after asked, oldest first. reader is the
// account that runs journalctl on its own journal, or the system's manager
// for root. An unreadable journal gives no entries.
func readJournal(ctx context.Context, reader UnitManager, asked JournalTime, count int, matches ...string) []journalEntry {
	args := []string{"--boot", "--all", "--no-pager", "--quiet", "--output=json", "--lines=" + strconv.Itoa(count)}
	if reader.Account != "" {
		args = append(args, "--user")
	}
	r := reader.runAsAccount(ctx, "journalctl", append(args, matches...)...)
	if r.Status != "succeeded" {
		return nil
	}
	var entries []journalEntry
	for _, line := range strings.Split(r.Stdout, "\n") {
		var entry journalEntry
		if json.Unmarshal([]byte(line), &entry) != nil {
			continue
		}
		if received, err := strconv.ParseUint(string(entry.Received), 10, 64); err == nil && JournalTime(received) >= asked {
			entries = append(entries, entry)
		}
	}
	return entries
}

// journalLines is a heading and one indented line for each entry's message,
// or nothing when no entry has text. A program's output is not trusted: each
// control character becomes a space, so an entry stays one line, and a long
// line is cut.
func journalLines(heading string, entries []journalEntry) string {
	var lines strings.Builder
	for _, entry := range entries {
		line := strings.TrimSpace(strings.Map(func(r rune) rune {
			if unicode.IsControl(r) {
				return ' '
			}
			return r
		}, strings.ToValidUTF8(string(entry.Message), "?")))
		if line == "" {
			continue
		}
		if len(line) > journalLineLimit {
			line = strings.ToValidUTF8(line[:journalLineLimit], "") + " ..."
		}
		lines.WriteString("\n  " + line)
	}
	if lines.Len() == 0 {
		return ""
	}
	return "\n" + heading + lines.String()
}

// neededUnit is a unit that another unit cannot run without, and what
// keeps it from being of use: "failed", or "is not running" with its state.
type neededUnit struct {
	unit, state string
}

// neededUnitsNotRunning lists the units that a unit names in Requires=,
// Requisite=, or BindsTo= and that are not running, at most
// neededUnitsShown. It looks one level deep only.
func (m UnitManager) neededUnitsNotRunning(ctx context.Context, unit string) []neededUnit {
	r := m.systemctl(ctx, "show", "--property=Requires,Requisite,BindsTo", "--", unit)
	if r.Status != "succeeded" {
		return nil
	}
	var names []string
	for _, line := range strings.Split(r.Stdout, "\n") {
		if _, listed, ok := strings.Cut(line, "="); ok {
			names = append(names, strings.Fields(listed)...)
		}
	}
	if len(names) == 0 {
		return nil
	}
	// systemctl show prints one block of properties for each unit, with an
	// empty line between the blocks.
	r = m.systemctl(ctx, append([]string{"show", "--property=Id,ActiveState", "--"}, names...)...)
	if r.Status != "succeeded" {
		return nil
	}
	var needed []neededUnit
	for _, block := range strings.Split(r.Stdout, "\n\n") {
		properties := map[string]string{}
		for _, line := range strings.Split(block, "\n") {
			if property, written, ok := strings.Cut(line, "="); ok {
				properties[property] = written
			}
		}
		switch state := properties["ActiveState"]; {
		case properties["Id"] == "" || state == "active" || state == "reloading" || len(needed) == neededUnitsShown:
		case state == "failed":
			needed = append(needed, neededUnit{properties["Id"], "failed"})
		default:
			needed = append(needed, neededUnit{properties["Id"], "is not running (ActiveState=" + state + ")"})
		}
	}
	return needed
}

type Service struct {
	Name        string `yaml:"name" json:"name"`
	State       string `yaml:"state,omitempty" json:"state,omitempty"`
	StartAtBoot *bool  `yaml:"start_at_boot,omitempty" json:"start_at_boot,omitempty"`
	// Masked true masks the unit so nothing can start it, false unmasks it;
	// omitted leaves masking alone.
	Masked *bool `yaml:"masked,omitempty" json:"masked,omitempty"`
	// InSystemdOfUserAccount names the account whose own systemd has the unit.
	InSystemdOfUserAccount string `yaml:"in_systemd_of_user_account,omitempty" json:"in_systemd_of_user_account,omitempty"`
	Refresh                string `yaml:"refresh,omitempty" json:"refresh,omitempty"`
	// RestartNow restarts the unit once, on a node where
	// RunOnlyOnceMarkerFile does not exist, and then writes that file.
	RestartNow            bool   `yaml:"restart_now,omitempty" json:"restart_now,omitempty"`
	RunOnlyOnceMarkerFile string `yaml:"run_only_once_marker_file,omitempty" json:"run_only_once_marker_file,omitempty"`
}

func ValidateService(s Service) error {
	if !slices.Contains(serviceStates, s.State) {
		return invalid("state", "systemd_service.state is required and must be running, stopped, or do_not_start_or_stop")
	}
	if s.Masked != nil && *s.Masked {
		if s.State == "running" {
			return invalid("masked", "systemd_service.masked: true cannot go with state: running; a masked unit cannot start. Write state: stopped or state: do_not_start_or_stop")
		}
		if s.StartAtBoot != nil && *s.StartAtBoot {
			return invalid("masked", "systemd_service.masked: true cannot go with start_at_boot: true; a masked unit cannot be enabled")
		}
	}
	if s.Refresh != "" && !slices.Contains(refreshActions, s.Refresh) {
		return invalid("refresh", "systemd_service.refresh must be reload or restart")
	}
	switch {
	case s.RestartNow && s.State != "running":
		return invalid("restart_now", "systemd_service.restart_now goes with state: running: a restart leaves the unit running")
	case s.RestartNow && s.RunOnlyOnceMarkerFile == "":
		return invalid("restart_now", "systemd_service.restart_now requires run_only_once_marker_file: MMX restarts the unit when the marker file is missing, then writes the marker file. Without a marker file, every run would restart the unit, and mmx check-one-time-playbooks would always report the playbook as needed on every node")
	case !s.RestartNow && s.RunOnlyOnceMarkerFile != "":
		return invalid("run_only_once_marker_file", "systemd_service.run_only_once_marker_file goes with restart_now: true")
	}
	return nil
}

// systemdServiceDescription is what mmx describe-module shows for systemd_service.
var systemdServiceDescription = Description{
	Name: "systemd_service", Topic: TopicAccountsAndServiceUnits, Schema: reflect.TypeOf(Service{}),
	Summary: "running, enabled, and masked state of a systemd unit of any kind systemd_unit installs, and refresh after tracked changes",
	Fields: []Field{
		{Name: "name", Required: true, Notes: "unit name, such as nginx or backup.timer; a name without a unit kind means .service; name the timer, path, or socket, not the service it activates"},
		{Name: "state", Values: serviceStates, Required: true, Notes: "do_not_start_or_stop neither starts nor stops the unit; in a task block's activate phase it still refreshes a running unit"},
		{Name: "start_at_boot", Notes: "omit to leave the unit's start-at-boot setting unchanged"},
		{Name: "in_systemd_of_user_account", Notes: "an account other than root whose own systemd has the unit; a task that starts or enables the unit turns on lingering for the account; while that systemd is not running, any other task reads and changes the account's unit files offline and starts nothing"},
		{Name: "masked", Notes: "true masks the unit after stopping or disabling it, so nothing can start it, and cannot go with state: running or start_at_boot: true; false unmasks it first; omit to leave masking alone"},
		{Name: "refresh", Values: refreshActions, Default: "restart", Notes: "for changed configuration in a task block's activate phase; reload only for .service units; a changed unit definition always restarts"},
		{Name: "restart_now", Default: "false", Notes: "true runs systemctl restart when run_only_once_marker_file is missing, then writes the marker file. The restart also starts a stopped unit. While the marker file exists, the task only keeps the unit running. Requires state: running and run_only_once_marker_file. Cannot be used in a node task block's activate phase, where tracked inputs decide restarts"},
		{Name: "run_only_once_marker_file", Notes: "with restart_now: true, required: absolute path of the file MMX writes after the restart succeeded; keep such files in /var/lib/mmx/commands_run_once/, and give a later restart of the same unit a new file"},
	},
	Example: "- systemd_service:\n    name: nginx\n    state: running\n    start_at_boot: true\n",
	Results: resultBasic,
	Dryrun:  "supported; reports the start, stop, enable, disable, or refresh it would make, and with restart_now the restart while the marker file does not exist",
}

// Prepare checks the inputs when the project loads.
func (s *Service) Prepare(base string) error {
	return ValidateService(*s)
}

// Arguments lists the inputs that may hold a reference, with their rules.
func (s *Service) Arguments(arguments *ArgumentList) {
	if s.Refresh == "reload" {
		arguments.Public("name", &s.Name, reloadableRule)
	} else {
		arguments.Public("name", &s.Name, serviceRule)
	}
	if s.InSystemdOfUserAccount != "" {
		arguments.Public("in_systemd_of_user_account", &s.InSystemdOfUserAccount, ordinaryAccount)
	}
	if s.RunOnlyOnceMarkerFile != "" {
		arguments.Public("run_only_once_marker_file", &s.RunOnlyOnceMarkerFile, absolute)
	}
}

// CheckPhase refuses restart_now in a node task block's activate phase:
// there the block's tracked inputs decide whether the unit restarts.
func (s *Service) CheckPhase(phase string) error {
	if s.RestartNow && phase == "activate" {
		return invalid("restart_now", "systemd_service.restart_now is not allowed in a task_block's activate phase, where tracked inputs from the changes phase decide whether the unit restarts. Move the systemd_service task outside the task_block, such as into a one-time playbook")
	}
	return nil
}

// ManagedPaths lists the marker file that restart_now writes; a dryrun
// cannot check a later wait on it.
func (s *Service) ManagedPaths() []string {
	return []string{s.RunOnlyOnceMarkerFile}
}

// Run does the module's work on the node.
func (s *Service) Run(ctx context.Context, task NodeTask) protocol.Result {
	return task.TaskBlock.RefreshService(ctx, *s)
}
