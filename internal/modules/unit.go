package modules

import (
	"context"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"mmx/internal/assets"
	"mmx/internal/protocol"
	"mmx/internal/value"
)

// Unit installs or removes one unit file or drop-in for a systemd_unit task.
func (run Runtime) Unit(ctx context.Context, u Unit, s value.Scope) protocol.Result {
	return run.installUnit(ctx, "systemd_unit", u, s)
}

// installUnit does the work of Unit for the module whose task installs the
// unit file; a failed check of the new file names that module.
func (run Runtime) installUnit(ctx context.Context, module string, u Unit, s value.Scope) (result protocol.Result) {
	if err := ValidateUnit(u); err != nil {
		return protocol.ConfigurationError(err)
	}
	m, err := SystemdOf(u.InSystemdOfUserAccount)
	if err != nil && u.State == "absent" && AccountIsGone(err) {
		r := protocol.Success("unchanged")
		r.Reason = "account " + u.InSystemdOfUserAccount + " does not exist, so it has no systemd that reads unit files"
		return r
	}
	if err != nil {
		return run.accountFailure(fmt.Errorf("systemd_unit.in_systemd_of_user_account: %w", err))
	}
	u = LocateUnit(u, m)
	f := u.File()
	// A managed unit of an account must not stop when the account logs out.
	if u.State != "absent" {
		reportLingering, stop := run.keepAccountServicesRunning(ctx, m)
		if stop != nil {
			return *stop
		}
		defer func() { reportLingering(&result) }()
	}
	parent, err := run.openDefinitionDirectory(ctx, m, m.UnitDirectory(), []string{".config", "systemd", "user"}, u.State == "absent")
	if err != nil {
		return protocol.Failed(err)
	}
	if parent != nil {
		defer parent.Close()
	}
	unitRoot := parent
	var directoryInfo os.FileInfo
	if u.DropIn != "" {
		name := u.Name + ".d"
		info, err := entryInfoAt(parent, name, true)
		if err != nil {
			return protocol.ConfigurationError(err)
		}
		if info == nil && u.State != "absent" && !run.Dryrun {
			installed, err := baseInstalled(ctx, m, u.Name)
			if err != nil {
				return protocol.Failed(err)
			}
			if !installed {
				return run.notYet(fmt.Errorf("drop-in requires an installed base unit: %s; install it in an earlier node task", u.Name))
			}
			if err := run.Mutation(filepath.Dir(f.Path)); err != nil {
				return protocol.Failed(err)
			}
			if err := createLabeled(ctx, filepath.Dir(f.Path), selinuxDirectory, func() error { return m.makeDirectory(parent, name) }); err != nil {
				return protocol.Failed(err)
			}
			defer func() { result.Change = "changed" }()
			info, err = parent.Lstat(name)
			if err != nil {
				return protocol.Failed(err)
			}
		}
		if info == nil {
			parent = nil // Absent directory in preview or an already absent drop-in.
		} else {
			parent, err = openDirectoryAt(parent, name, info)
			if err != nil {
				return protocol.Failed(err)
			}
			defer parent.Close()
			directoryInfo = info
		}
	}
	var validate func(string) protocol.Result
	if u.State != "absent" && !run.Dryrun {
		validate = func(candidate string) protocol.Result {
			return unitCheckFailure(module, run.validateUnitCandidate(ctx, module, m, u, parent, candidate))
		}
	}
	result = run.fileAt(ctx, f, s, parent, 0644, validate)
	if run.Dryrun && result.Status == "succeeded" && result.Change == "would_change" && u.State != "absent" {
		// Validation needs installed prerequisites; preview without a candidate.
		result.CheckNewFile = &protocol.Result{Status: "skipped", Change: "unchanged", Reason: "not checked: systemd unit validation requires prerequisites to be installed on the node"}
	}
	if result.Status != "succeeded" {
		return result
	}
	if directoryInfo != nil && !run.Dryrun {
		if err := sameFileAt(unitRoot, u.Name+".d", directoryInfo); err != nil {
			result.Status, result.Reason = "failed", err.Error()
			return result
		}
	}
	pending, err := definitionPending(ctx, m, u.Name)
	return run.reloadForDefinition(ctx, m, result, pending, err)
}

// keepAccountServicesRunning turns lingering on for the account of m when it
// is off: what MMX manages for an account must start at boot and must not
// stop when the account logs out. A dryrun turns nothing on. The task calls
// the returned function on its result when it ends, which then says what was
// done, also for a task that failed after it: lingering stays on. For the
// system's manager and an account that already lingers it does nothing.
func (run Runtime) keepAccountServicesRunning(ctx context.Context, m UnitManager) (report func(*protocol.Result), stop *protocol.Result) {
	if m.Lingering() {
		return func(*protocol.Result) {}, nil
	}
	if !run.Dryrun {
		if r := run.EnableLinger(ctx, m); r.Status != "succeeded" {
			return nil, &r
		}
	}
	return func(result *protocol.Result) {
		if result.Status != "succeeded" {
			if !run.Dryrun {
				if result.Reason != "" {
					result.Reason += "; "
				}
				result.Reason += "before the failure, the task enabled lingering for account " + m.Account + ", which stays on"
				if result.Change == "unchanged" {
					result.Change = "changed"
				}
			}
			return
		}
		notice := m.LingerNotice()
		if run.Dryrun {
			notice = m.wouldLingerNotice()
		}
		if result.Reason != "" {
			notice = result.Reason + "; " + notice
		}
		result.Reason = notice
		if result.Change == "unchanged" {
			result.Change = "changed"
			if run.Dryrun {
				result.Change = "would_change"
			}
		}
	}, nil
}

// reloadForDefinition finishes a task that installed or removed a file that
// defines a unit: result is the file's result, and pending says that the
// manager has not read the file as it is now. The manager reads its files
// again when the file changed or a reload is pending, which also completes
// the reload that an interrupted run missed. A dryrun reloads nothing and
// reports the reload that a run would make for a file that is already as the
// task wants it.
func (run Runtime) reloadForDefinition(ctx context.Context, m UnitManager, result protocol.Result, pending bool, err error) protocol.Result {
	if err != nil {
		failed := protocol.Failed(err)
		failed.Change = result.Change
		failed.FileChange = result.FileChange
		return failed
	}
	if run.Dryrun {
		if result.Change == "unchanged" && pending && m.Running() {
			result.Change, result.Reason = "would_change", "would reload systemd, which has not yet read the changed unit definitions"
		}
		return result
	}
	// An account's systemd that is not running reads its unit files when it starts.
	if (result.Change == "changed" || pending) && m.Running() {
		reload := run.ReloadManagerOf(ctx, m)
		if reload.Status != "succeeded" {
			reload.FileChange = result.FileChange
			if result.Change == "changed" {
				reload.Change = "changed"
			}
			return reload
		}
		result.Change = "changed"
	}
	return result
}

// Systemd cannot show a template ("app@.service") as a unit, so its unit files
// are listed instead. A pending manager reload for a template's instances is
// completed by their systemd_service node tasks.
func template(name string) bool {
	return strings.HasSuffix(strings.TrimSuffix(name, filepath.Ext(name)), "@")
}
func baseInstalled(ctx context.Context, m UnitManager, name string) (bool, error) {
	if !template(name) {
		observed, err := m.Inspect(ctx, name)
		return observed.Load != "not-found", err
	}
	// Exit status 1 without an error message means no unit file matched.
	r := m.systemctl(ctx, "list-unit-files", "--no-legend", "--", name)
	missing := r.Status != "succeeded" && r.ExitCode == 1 && r.Stderr == ""
	if r.Status != "succeeded" && !missing {
		return false, fmt.Errorf("list %s: %s: %s", name, r.Reason, r.Stderr)
	}
	return !missing, nil
}
func definitionPending(ctx context.Context, m UnitManager, name string) (bool, error) {
	if template(name) {
		return false, nil
	}
	observed, err := m.Inspect(ctx, name)
	return observed.NeedDaemonReload, err
}

// unitCheckFailure words a failed check of a new unit file for the module
// whose task installs the file: a module has no check_new_file field, which
// is the name of the check that a file task declares itself.
func unitCheckFailure(module string, r protocol.Result) protocol.Result {
	if r.Status != "succeeded" {
		r.Reason = module + ": systemd's check of the new unit file failed: " + r.Reason
	}
	return r
}

// unitCheckTimeLimit bounds systemd's check of an account's new unit file,
// which ends within seconds. The check reads the account's unit directory,
// and a file there that is not a regular file, such as a named pipe, can make
// it wait without end. Tests shorten the limit.
var unitCheckTimeLimit = time.Minute

// unitCheck is one question to systemd-analyze verify about a new unit file.
type unitCheck struct {
	// account runs the check: nil is root, for a unit of the system, and
	// otherwise the account whose own systemd the unit is for.
	account *syscall.Credential
	env     map[string]string
	// file is what verify is asked about: the new file's path, or the
	// unit's name when the new file is a drop-in. written is the new file as
	// verify names it in its messages.
	file, written string
}

// run asks systemd-analyze verify and decides from its answer.
func (c unitCheck) run(ctx context.Context) protocol.Result {
	args, directory := []string{"verify", "--man=no"}, ""
	if c.account != nil {
		// The rules of an account's systemd, from a directory that every
		// account can enter.
		args, directory = append([]string{"--user"}, args...), "/"
	}
	return runProcess(ctx, c.env, directory, nil, c.account, "systemd-analyze", append(args, "--recursive-errors=no", c.file)...).Result
}

// validateUnitCandidate asks systemd about the new file before it is
// installed. The private candidate directory takes precedence over the usual
// unit paths. Systemd combines the base and all other drop-ins using its own
// lookup rules. parent is the pinned directory that holds the candidate's
// own directory, and module is the module of the task that installs the
// file.
func (run Runtime) validateUnitCandidate(ctx context.Context, module string, m UnitManager, u Unit, parent *os.Root, candidate string) protocol.Result {
	if m.Account != "" {
		return run.validateAccountUnitCandidate(ctx, module, m, u, parent, candidate)
	}
	check := unitCheck{file: candidate, written: candidate}
	if u.DropIn != "" {
		stage := filepath.Dir(candidate)
		dir := filepath.Join(stage, u.Name+".d")
		if err := os.Mkdir(dir, 0700); err != nil {
			return protocol.Failed(err)
		}
		check.written = filepath.Join(dir, u.DropIn)
		if err := os.Link(candidate, check.written); err != nil {
			return protocol.Failed(err)
		}
		check.env = map[string]string{"SYSTEMD_UNIT_PATH": stage + ":"}
		check.file = u.Name
	}
	return check.run(ctx)
}

// validateAccountUnitCandidate checks a new unit file of an account's own
// systemd by the rules of that systemd, with the account's other units in
// view. The account runs the check, as its own systemd reads its units: the
// account fills its unit directory, and a check run by root would read
// whatever a link there points to. Root does nothing by a path in that
// directory either: it reads the new file through the pinned directory and
// gives the check a copy, in the task's private directory
// (makePrivateTaskDirectory), which is the account's for the check. The
// check starts a systemd of its own that makes its sockets in
// XDG_RUNTIME_DIR, so it gets a directory there too and never the account's
// runtime directory: there its socket would take the place of the running
// systemd's.
func (run Runtime) validateAccountUnitCandidate(ctx context.Context, module string, m UnitManager, u Unit, parent *os.Root, candidate string) protocol.Result {
	failure := func(err error) protocol.Result {
		return protocol.Failed(fmt.Errorf("prepare private files for systemd unit validation: %w", err))
	}
	text, err := readTextAt(parent, filepath.Join(filepath.Base(filepath.Dir(candidate)), filepath.Base(candidate)))
	if err != nil {
		return failure(err)
	}
	// The copy's place among the unit files of the private directory: the
	// unit's own name, or the drop-in in the unit's drop-in directory.
	copied := u.Name
	if u.DropIn != "" {
		copied = filepath.Join(u.Name+".d", u.DropIn)
	}
	directory, remove, err := run.makePrivateTaskDirectory(module, m.credential, func(directory string) error {
		written := filepath.Join(directory, "unit-files", copied)
		if err := os.MkdirAll(filepath.Dir(written), 0700); err != nil {
			return err
		}
		if err := os.Mkdir(filepath.Join(directory, "runtime"), 0700); err != nil {
			return err
		}
		return os.WriteFile(written, []byte(text), 0600)
	})
	if err != nil {
		return failure(err)
	}
	defer remove()
	units := filepath.Join(directory, "unit-files")
	check := unitCheck{account: m.credential, env: maps.Clone(m.env), file: filepath.Join(units, u.Name), written: filepath.Join(units, copied)}
	if u.DropIn != "" {
		check.file = u.Name
	}
	if check.env == nil {
		check.env = map[string]string{}
	}
	check.env["SYSTEMD_UNIT_PATH"] = units + ":" + m.UnitDirectory() + ":"
	check.env["XDG_RUNTIME_DIR"] = filepath.Join(directory, "runtime")
	limited, cancel := context.WithTimeout(ctx, unitCheckTimeLimit)
	defer cancel()
	r := check.run(limited)
	if r.Status != "succeeded" && limited.Err() != nil && ctx.Err() == nil {
		r.Reason = fmt.Sprintf("systemd-analyze verify did not finish within %s; systemd-analyze reads unit files in %s, where a non-regular file, such as a named pipe, can make systemd-analyze wait indefinitely", unitCheckTimeLimit, m.UnitDirectory())
	}
	// systemd names the new file where the copy is; the reader knows it by
	// where it goes.
	installed := u.File().Path
	r.Reason, r.Stderr = strings.ReplaceAll(r.Reason, check.written, installed), strings.ReplaceAll(r.Stderr, check.written, installed)
	return r
}

func (run Runtime) ReloadManager(ctx context.Context) protocol.Result {
	return run.ReloadManagerOf(ctx, UnitManager{})
}

// ReloadManagerOf makes a systemd read its unit files again.
func (run Runtime) ReloadManagerOf(ctx context.Context, m UnitManager) protocol.Result {
	if err := run.Mutation("systemd manager"); err != nil {
		return protocol.Failed(err)
	}
	r := m.systemctl(ctx, "daemon-reload")
	if r.Status == "succeeded" {
		r.Change = "changed"
	} else {
		r.Change = "unknown"
	}
	return r
}

// LocateUnit fills in where an account's unit file goes and the IDs that own
// it; a system unit needs nothing.
func LocateUnit(u Unit, m UnitManager) Unit {
	if m.Account != "" {
		u.UserUnitDirectory = m.UnitDirectory()
		u.UserUnitOwner, u.UserUnitGroup = Account(strconv.Itoa(m.UID)), Account(strconv.Itoa(m.GID))
	}
	return u
}

// openDefinitionDirectory pins the directory in which a manager's unit
// files, or the Quadlet files that Podman's generator turns into units, are
// installed: systemDirectory for the system's manager, and the directories
// inHome below the account's home for an account's. An account's directory
// is reached through its home, so that a link the account placed there
// cannot lead a write out of the home. A missing directory is created, for
// an account with the account as its owner; the parent of a missing
// systemDirectory must exist. It returns nil for a directory that does not
// exist yet during a dryrun or a removal.
func (run Runtime) openDefinitionDirectory(ctx context.Context, m UnitManager, systemDirectory string, inHome []string, removal bool) (*os.Root, error) {
	if m.Account == "" {
		root, err := os.OpenRoot(systemDirectory)
		if !os.IsNotExist(err) {
			return root, err
		}
		if run.Dryrun || removal {
			return nil, nil
		}
		parent, err := os.OpenRoot(filepath.Dir(systemDirectory))
		if err != nil {
			return nil, err
		}
		defer parent.Close()
		if err := run.Mutation(systemDirectory); err != nil {
			return nil, err
		}
		name := filepath.Base(systemDirectory)
		if err := createLabeled(ctx, systemDirectory, selinuxDirectory, func() error { return m.makeDirectory(parent, name) }); err != nil {
			return nil, err
		}
		return parent.OpenRoot(name)
	}
	home, err := os.OpenRoot(m.Home)
	if err != nil {
		if os.IsNotExist(err) && run.Dryrun {
			return nil, nil
		}
		return nil, fmt.Errorf("home directory of account %s: %w", m.Account, err)
	}
	defer home.Close()
	path := ""
	for _, name := range inHome {
		path = filepath.Join(path, name)
		_, err := home.Lstat(path)
		if err == nil {
			continue
		}
		if !os.IsNotExist(err) {
			return nil, err
		}
		if run.Dryrun || removal {
			return nil, nil
		}
		if err := run.Mutation(filepath.Join(m.Home, path)); err != nil {
			return nil, err
		}
		if err := createLabeled(ctx, filepath.Join(m.Home, path), selinuxDirectory, func() error { return m.makeDirectory(home, path) }); err != nil {
			return nil, err
		}
	}
	return home.OpenRoot(path)
}

// makeDirectory creates a directory for unit files with the mode it should
// have whatever the runner's umask is: readable by everyone for the system's
// units, as /etc/systemd/system is, and private to the account for an
// account's units.
func (m UnitManager) makeDirectory(parent *os.Root, name string) error {
	mode := os.FileMode(0755)
	if m.Account != "" {
		mode = 0700
	}
	if err := parent.Mkdir(name, mode); err != nil {
		return err
	}
	if m.Account != "" {
		if err := parent.Lchown(name, m.UID, m.GID); err != nil {
			return err
		}
	}
	return parent.Chmod(name, mode)
}

type Unit struct {
	Name   string `yaml:"name" json:"name"`
	State  string `yaml:"state,omitempty" json:"state,omitempty"`
	DropIn string `yaml:"drop_in,omitempty" json:"drop_in,omitempty"`
	// InSystemdOfUserAccount puts the unit in that account's own systemd.
	// The node then fills in where the account's unit files are and the IDs
	// that own them.
	InSystemdOfUserAccount string      `yaml:"in_systemd_of_user_account,omitempty" json:"in_systemd_of_user_account,omitempty"`
	UserUnitDirectory      string      `yaml:"-" json:"-"`
	UserUnitOwner          Account     `yaml:"-" json:"-"`
	UserUnitGroup          Account     `yaml:"-" json:"-"`
	Content                *string     `yaml:"content,omitempty" json:"content,omitempty"`
	Template               string      `yaml:"template,omitempty" json:"template,omitempty"`
	TemplateText           string      `yaml:"-" json:"-"`
	Asset                  *assets.Ref `yaml:"-" json:"asset,omitempty"`
	LocalPath              string      `yaml:"-" json:"-"`
	// SourceBase and Variants serve a variable source field (variable_sources.go).
	SourceBase string                    `yaml:"-" json:"-"`
	Variants   map[string]*SourceVariant `yaml:"-" json:"variants,omitempty"`
	// Literal is a unit's exact text that another module composed on the
	// node (systemd_scheduled_job); unlike content, references in it are not
	// resolved.
	Literal *string `yaml:"-" json:"-"`
}

var dropInName = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]*\.conf$`)

// unitName is a unit filename of a kind MMX installs and controls. Backslash
// is part of systemd's escaping ("srv-my\x2ddata.mount" for /srv/my-data).
var unitName = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.@:\\-]*\.(service|timer|path|socket|mount|target|swap|slice|automount)$`)

// ServiceName is a systemd unit name, with or without its unit kind.
var ServiceName = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.@:\\-]*$`)

// UnitName is the unit a systemd_service task names: a name without one of the unit
// kinds means a .service.
func UnitName(name string) string {
	if unitName.MatchString(name) {
		return name
	}
	return name + ".service"
}

func ValidateUnit(u Unit) error {
	if err := validatePresence(u.State); err != nil {
		return err
	}
	if u.State == "absent" {
		if u.Content != nil || u.Template != "" || u.Asset != nil || u.Literal != nil {
			return fmt.Errorf("absent systemd_unit accepts only name, drop_in, in_systemd_of_user_account, and state")
		}
		return nil
	}
	if u.Literal != nil && u.Content == nil && u.Template == "" {
		return nil
	}
	if (u.Content == nil) == (u.Template == "") || u.Literal != nil {
		return fmt.Errorf("systemd_unit requires exactly one of content or template")
	}
	return nil
}

// File returns the unit's managed file. Its name and drop-in arrive resolved
// and checked by the task-argument rules. A unit in an account's own systemd
// has a file only once the node has filled in the account's unit directory
// (Located).
func (u Unit) File() File {
	directory, owner, group := "/etc/systemd/system", Account("root"), Account("root")
	if u.InSystemdOfUserAccount != "" {
		directory, owner, group = u.UserUnitDirectory, u.UserUnitOwner, u.UserUnitGroup
	}
	path := filepath.Join(directory, u.Name)
	if u.DropIn != "" {
		path = filepath.Join(path+".d", u.DropIn)
	}
	if u.State == "absent" {
		return File{Path: path, State: "absent"}
	}
	return File{Path: path, Content: u.Content, Literal: u.Literal, Template: u.Template, TemplateText: u.TemplateText, Asset: u.Asset, LocalPath: u.LocalPath, Mode: "0644", Owner: owner, Group: group}
}

// Located reports whether the unit's file path is known: always for a system
// unit, and for an account's unit once the node looked the account up.
func (u Unit) Located() bool {
	return u.InSystemdOfUserAccount == "" || u.UserUnitDirectory != ""
}

// systemdUnitDescription is what mmx describe-module shows for systemd_unit.
var systemdUnitDescription = Description{
	Name: "systemd_unit", Topic: TopicAccountsAndServiceUnits, Schema: reflect.TypeOf(Unit{}),
	Summary: "a systemd unit file (.service, .timer, .path, .socket, .mount, .target, .swap, .slice, or .automount) or one drop-in, verified before installation",
	Fields: []Field{
		{Name: "name", Required: true, Notes: "a unit filename, such as app.service, app@.service, or backup.timer; a .mount is named for its escaped mount point (systemd-escape --path --suffix=mount)"},
		{Name: "drop_in", Notes: "a single .conf filename; manages /etc/systemd/system/<name>.d/<drop_in>"},
		{Name: "in_systemd_of_user_account", Notes: "an account other than root: the unit belongs to that account's own systemd, its file goes to <home>/.config/systemd/user/ owned by the account, and MMX turns on lingering for the account so that its units start at boot and keep running after logout"},
		{Name: "content", Notes: "inline text; exactly one of content or template"},
		{Name: "template", Notes: "local Go template, relative to the project root, or to the role's directory inside a role; exactly one of content or template"},
		{Name: "state", Values: presenceStates, Default: "present", Notes: "absent removes only the selected unit file or drop-in and accepts only name, drop_in, in_systemd_of_user_account, and state"},
	},
	Example: "- systemd_unit:\n    name: app.service\n    content: |\n      [Service]\n      ExecStart=/usr/local/bin/app\n\n      [Install]\n      WantedBy=multi-user.target\n",
	Results: resultBasic,
	Dryrun:  "supported; previews contents; validation of a changed unit is not checked",
}

// Prepare checks the inputs when the project loads and reads the template on
// the controller machine, relative to base.
func (u *Unit) Prepare(base string) error {
	if err := ValidateUnit(*u); err != nil {
		return err
	}
	u.SourceBase = base
	if u.Template != "" && !Dynamic(u.Template) {
		path, err := localSource(base, "template", u.Template)
		if err != nil {
			return err
		}
		b, err := readTemplate(path)
		if err != nil {
			return &FieldError{"template", err}
		}
		u.LocalPath, u.TemplateText = path, string(b)
	}
	return nil
}

// Arguments lists the inputs that may hold a reference, with their rules.
func (u *Unit) Arguments(arguments *ArgumentList) {
	arguments.Public("name", &u.Name, unitNameRule)
	if u.InSystemdOfUserAccount != "" {
		arguments.Public("in_systemd_of_user_account", &u.InSystemdOfUserAccount, ordinaryAccount)
	}
	if u.DropIn != "" {
		arguments.Public("drop_in", &u.DropIn, dropInRule)
	}
	if u.Content != nil {
		arguments.Private("content", *u.Content, scalar)
	}
}

// Reads lists the template.
func (u *Unit) Reads() Reads {
	if u.Template == "" {
		return Reads{}
	}
	return Reads{Templates: map[string]string{"template": u.TemplateText}}
}

// ControllerFiles lists the template of the unit.
func (u *Unit) ControllerFiles() []ControllerFile {
	if u.Template == "" {
		return nil
	}
	return []ControllerFile{{Field: "template", Kind: SourceTemplate, Path: &u.Template, Base: u.SourceBase,
		Variants: &u.Variants, Asset: &u.Asset, Template: &u.TemplateText, Local: &u.LocalPath}}
}

// Run does the module's work on the node.
func (u *Unit) Run(ctx context.Context, task NodeTask) protocol.Result {
	return task.Runtime.Unit(ctx, *u, task.Scope)
}

// TrackedInputs lists the unit file. A unit of an account whose home is not
// known yet has no path; that account cannot have a running service either.
func (u *Unit) TrackedInputs() []TrackedInput {
	if !u.Located() {
		return nil
	}
	f := u.File()
	return []TrackedInput{{Kind: "unit", Path: f.Path, Content: &f, UnitAccount: u.InSystemdOfUserAccount, UnitName: u.Name}}
}

// LocateOnNode finds the unit file of an account's own systemd in the
// account's home. An account that does not exist yet is left as it is.
func (u *Unit) LocateOnNode() {
	if u.InSystemdOfUserAccount == "" {
		return
	}
	if m, err := SystemdOf(u.InSystemdOfUserAccount); err == nil {
		*u = LocateUnit(*u, m)
	}
}
