package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"syscall"

	"mmx/internal/config"
	"mmx/internal/durable"
	"mmx/internal/modules"
	"mmx/internal/protocol"
	"mmx/internal/value"
)

type taskBlockState struct {
	// fingerprints are what the block's modules that are not core reported
	// in its changes, by tracked input key; a loop's items add up.
	fingerprints map[string]string
	// changedByModule holds the tracked input key of each module that is not
	// core whose task changed something in this attempt, or may have.
	changedByModule map[string]bool
	// moduleKeys are the tracked input keys of the block's modules that are
	// not core, by module name.
	moduleKeys map[string]string
	services   map[string]*servicePlan
	// commands are the block's commands that follow tracked inputs, by the
	// result path of their task.
	commands map[string]*commandPlan
	// inputs are all tracked inputs of the block, which its commands follow;
	// before are their hashes before the block's changes.
	inputs *servicePlan
	before map[string]string
	// tracked are the block's tracked changes as fixed when it started.
	tracked        []*config.Task
	checks         *blockChecks
	previewChanged bool
	// previewPathsBefore counts the job's preview paths when the block began.
	previewPathsBefore int
	// Dryrun: what the tracked node tasks that ran would apply.
	inputPreview
}

// inputPreview is what tracked node tasks would apply: files by path,
// packages by name, and the state of each checkout and installation by its
// key.
type inputPreview struct {
	applied   map[string]appliedFile
	installed map[string]modules.PackageExpectation
	states    map[string]string
}

// appliedFile is a tracked file as its node task would leave it; text is the
// content an edit would leave, which the next edit of the file starts from.
type appliedFile struct {
	content modules.ConfigurationContent
	scope   value.Scope
	text    *string
}

func newInputPreview() inputPreview {
	return inputPreview{applied: map[string]appliedFile{}, installed: map[string]modules.PackageExpectation{}, states: map[string]string{}}
}

type servicePlan struct {
	files    []modules.TrackedInput
	units    []modules.TrackedInput
	packages []modules.TrackedInput
	// states are checkouts and installations, which a tool on the node reads.
	states []modules.TrackedInput
	// moduleInputs are modules that are not core, tracked by fingerprint.
	moduleInputs []modules.TrackedInput
	task         modules.Service
	manager      modules.UnitManager // the systemd that has the unit
	unit         string              // the unit's canonical name there
	record       *serviceRecord
	before       map[string]string // input hashes before the task block's changes
	// unreported are the module inputs whose fingerprint the record lacked
	// when the task block began.
	unreported map[string]bool
}

// A record holds one hash per input the service instance loaded, keyed by
// kind: "config:" and "unit:" plus a path, "package:" plus a name, and "git:"
// plus the path of a checkout. Task blocks
// with different inputs for one service each compare only their own.
// Configuration is acknowledged by reload or start; units and packages only by
// start/restart. A start or restart loads everything on disk, so its record
// holds only the inputs of the task block that made it and the fingerprints
// of other blocks' modules; an input the record lacks gets its baseline when
// its task block next begins. An attempted action is never confused with
// absence.
//
// Nothing on the node says what a module that is not core keeps: only the
// module's own task reports its fingerprint. Where the record lacks it, the
// task block saves unreportedFingerprint in its place before its first write.
// When the module's task then changes nothing, the fingerprint it reports is
// the baseline: the service loaded what the module keeps, as it loaded the
// files on disk. When the task changes something, the fingerprint differs
// from the one in the record, as a changed file's hash does. An attempt that
// ends after the module's change and before the service's refresh leaves
// unreportedFingerprint in the record, so the next attempt, whose module
// task finds nothing to change, still refreshes the service.
//
// Job names the attempt whose task block may be changing the inputs: set before
// its first write and cleared when its activate stage succeeds. An unconfirmed
// record without an action is such an attempt's pre-change inputs for a service
// that was not running, so no instance is known to have loaded them.
type serviceRecord struct {
	Version   int               `json:"version"`
	Unit      string            `json:"unit"`
	Instance  string            `json:"instance"`
	Inputs    map[string]string `json:"inputs"`
	Confirmed bool              `json:"confirmed"`
	Action    string            `json:"action,omitempty"`
	Job       string            `json:"job,omitempty"`
}

// unreportedFingerprint stands in a record for the fingerprint of a module
// that is not core, from a task block's first write until the fingerprint is
// known. It is the hash of no fingerprint, which no module reports.
var unreportedFingerprint = fingerprintHash("")

// fingerprintHash is the hash that a record holds for a module's fingerprint.
func fingerprintHash(fingerprint string) string {
	h := sha256.New()
	modules.HashField(h, fingerprint)
	return hex.EncodeToString(h.Sum(nil))
}

// awaitFingerprints puts unreportedFingerprint into a record's inputs for
// each module input that they lack, and returns the keys of those inputs.
func awaitFingerprints(inputs map[string]string, moduleInputs []modules.TrackedInput) map[string]bool {
	unreported := map[string]bool{}
	for _, in := range moduleInputs {
		if _, ok := inputs[in.Key()]; !ok {
			inputs[in.Key()] = unreportedFingerprint
			unreported[in.Key()] = true
		}
	}
	return unreported
}

// takeFingerprintBaselines puts into a record's inputs, for each of the
// unreported module inputs whose task has reported and changed nothing in
// this attempt, the fingerprint hash in current. It reports whether it
// changed the inputs.
func (state *taskBlockState) takeFingerprintBaselines(inputs map[string]string, unreported map[string]bool, current map[string]string) bool {
	taken := false
	for key := range unreported {
		if hash, reported := current[key]; reported && !state.changedByModule[key] && inputs[key] == unreportedFingerprint {
			inputs[key], taken = hash, true
		}
	}
	return taken
}

// forgetUnreportedFingerprints removes from a record's inputs, once the task
// block's activate phase has succeeded, each unreportedFingerprint that the
// block put there for a module whose tasks changed nothing in this attempt:
// nothing is owed for it, and the next attempt takes its baseline. It
// reports whether it changed the inputs.
func (state *taskBlockState) forgetUnreportedFingerprints(inputs map[string]string, unreported map[string]bool) bool {
	forgotten := false
	for key := range unreported {
		if !state.changedByModule[key] && inputs[key] == unreportedFingerprint {
			delete(inputs, key)
			forgotten = true
		}
	}
	return forgotten
}

// A starting or reloading unit already has its process instance; systemctl
// runs a further start or refresh after the job in progress.
func running(s modules.ServiceState) bool {
	return s.Instance != "" && (s.Active == "active" || s.Active == "activating" || s.Active == "reloading")
}

// started reports whether a unit has finished starting. When systemctl has
// reported a start, restart, or reload as done and the unit is activating,
// the unit has ended since and systemd starts it again.
func started(s modules.ServiceState) bool {
	return running(s) && s.Active != "activating"
}

func stopped(s modules.ServiceState) bool {
	return s.Active == "inactive" || s.Active == "failed"
}

// The node chooses the runtime root when it constructs the engine.
func (e *Engine) servicePath(unit string) string {
	sum := sha256.Sum256([]byte(unit))
	return filepath.Join(e.RuntimeRoot, "services", hex.EncodeToString(sum[:])+".json")
}
func (e *Engine) readServiceRecord(unit string) (*serviceRecord, error) {
	if !filepath.IsAbs(e.RuntimeRoot) {
		return nil, fmt.Errorf("internal error: the service record runtime directory requires an absolute path")
	}
	b, err := os.ReadFile(e.servicePath(unit))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var r serviceRecord
	if err := protocol.Decode(b, &r); err != nil {
		return nil, fmt.Errorf("invalid service record for %s: %w", unit, err)
	}
	valid := r.Version == 1 && r.Unit == unit && (!r.Confirmed || r.Instance != "") && (r.Confirmed || r.Action == "start" || r.Action == "reload" || r.Action == "restart" || (r.Action == "" && r.Instance == "" && r.Job != ""))
	if !valid || !validInputs(r.Inputs) {
		return nil, fmt.Errorf("invalid service record for %s", unit)
	}
	if r.Inputs == nil {
		r.Inputs = map[string]string{}
	}
	return &r, nil
}
func (e *Engine) saveServiceRecord(r *serviceRecord, phase string) error {
	if e.job.Dryrun {
		return fmt.Errorf("internal error: service record write during dryrun")
	}
	if e.BeforeRecord != nil {
		if err := e.BeforeRecord(phase); err != nil {
			return err
		}
	}
	//nolint:nilaway // A nonempty loadAction with a plan means the action loop allocated the service record.
	if err := privateDirectory(filepath.Dir(e.servicePath(r.Unit))); err != nil {
		return err
	}
	return durable.JSON(e.servicePath(r.Unit), r)
}

// validInputs checks a record's input hashes and their kinds.
func validInputs(inputs map[string]string) bool {
	for k, v := range inputs {
		b, err := hex.DecodeString(v)
		kind, _, _ := strings.Cut(k, ":")
		if err != nil || len(b) != sha256.Size || (kind != "config" && kind != "unit" && kind != "package" && kind != "git" && kind != "installed" && kind != "module") {
			return false
		}
	}
	return true
}

// privateDirectory creates dir under its parent when needed and requires
// both to be directories of this user. No one else can open dir. No one else
// can list or change its parent; the runner's volatile directory, the parent
// of the service records, lets other accounts pass through to the private
// directories of their own node tasks (mode 0711).
func privateDirectory(dir string) error {
	for _, path := range []string{filepath.Dir(dir), dir} {
		if err := os.MkdirAll(path, 0700); err != nil {
			return err
		}
		st, err := os.Lstat(path)
		if err != nil {
			return err
		}
		mode := st.Mode().Perm()
		if path != dir {
			mode &^= 0011
		}
		owner, ok := st.Sys().(*syscall.Stat_t)
		if !st.IsDir() || mode != 0700 || !ok || int(owner.Uid) != os.Geteuid() {
			return fmt.Errorf("record directory must be a private owned directory: %s", path)
		}
	}
	return nil
}

// configurationHash hashes the files as they are on disk, except that a dryrun
// passes applied: the tracked inputs whose node tasks ran, hashed as those node tasks
// apply them.
func configurationHash(ctx context.Context, files []modules.TrackedInput, applied map[string]appliedFile) (string, error) {
	ordered := append([]modules.TrackedInput(nil), files...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Path < ordered[j].Path })
	h := sha256.New()
	for _, in := range ordered {
		modules.HashField(h, in.Path)
		content, scope := in.Content, value.Scope(nil)
		desired, apply := applied[in.Path]
		if apply {
			content, scope = desired.content, desired.scope
		}
		if err := content.HashConfiguration(ctx, h, apply, scope); err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// softwareHash hashes installed packages, except that a dryrun passes
// installed: the packages whose node tasks ran, as those node tasks request them.
func softwareHash(ctx context.Context, manager modules.PackageManager, names []string, installed map[string]modules.PackageExpectation) (string, error) {
	values := map[string]modules.PackageState{}
	for _, name := range names {
		v, err := manager.Inspect(ctx, name)
		if err != nil {
			return "", err
		}
		p, apply := installed[name]
		if v.State == "absent" || (apply && p.State == "absent") {
			v.State, v.Version = "absent", ""
		}
		if apply && p.State != "absent" && (v.State != "installed" || (p.Version != "" && p.Version != v.Version)) {
			v.State = "installed"
			v.Version = p.Version
			if v.Version == "" {
				v.Version = "installation required"
			}
		}
		values[name] = v
	}
	h := sha256.New()
	for _, name := range slices.Sorted(maps.Keys(values)) {
		v := values[name]
		modules.HashField(h, name)
		modules.HashField(h, v.Version)
		modules.HashField(h, v.State)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// inputHashes hashes each of a service's inputs separately, keyed as in its
// record. A dryrun passes what the tracked node tasks that ran would apply.
func (e *Engine) inputHashes(ctx context.Context, plan *servicePlan, preview inputPreview) (map[string]string, error) {
	hashes := map[string]string{}
	// A checkout contributes the ID of its commit, as a package its version;
	// the tree is not hashed. An installation contributes what its tool lists.
	for _, in := range plan.states {
		state, would := preview.states[in.Key()]
		if !would {
			var err error
			if state, err = in.State.ReadState(ctx, in.Scope); err != nil {
				return nil, err
			}
		}
		h := sha256.New()
		modules.HashField(h, state)
		hashes[in.Key()] = hex.EncodeToString(h.Sum(nil))
	}
	for _, in := range append(append([]modules.TrackedInput{}, plan.files...), plan.units...) {
		h, err := configurationHash(ctx, []modules.TrackedInput{in}, preview.applied)
		if err != nil {
			return nil, err
		}
		hashes[in.Key()] = h
	}
	// A module that is not core counts by the fingerprint it reported in this
	// block's changes; before it runs, it has none.
	for _, in := range plan.moduleInputs {
		if e.active == nil {
			continue
		}
		if fingerprint, reported := e.active.fingerprints[in.Key()]; reported {
			hashes[in.Key()] = fingerprintHash(fingerprint)
		}
	}
	for _, in := range plan.packages {
		h, err := softwareHash(ctx, e.packages, []string{in.PackageName}, preview.installed)
		if err != nil {
			return nil, err
		}
		hashes[in.Key()] = h
	}
	return hashes, nil
}

// clearMarks runs once the task block's activate phase has succeeded: its
// writes are loaded or need no action, so a later instance change is ordinary.
// A record that is still unconfirmed keeps its mark.
func (e *Engine) clearMarks(state *taskBlockState) error {
	if e.job.Dryrun {
		return nil
	}
	for name, plan := range state.services {
		record, err := e.readServiceRecord(name)
		if err != nil {
			return err
		}
		if record == nil || !record.Confirmed || record.Job == "" {
			continue
		}
		record.Job = ""
		state.forgetUnreportedFingerprints(record.Inputs, plan.unreported)
		if err := e.saveServiceRecord(record, "finished"); err != nil {
			return fmt.Errorf("save the service record after activation: %w", err)
		}
	}
	return nil
}

// interrupted reports whether an attempt that marked the record (Job) has
// since changed any input the record holds. What a module that is not core
// keeps is known only after its task has run, so a module input that the
// record holds counts as changed: the record stays, and the comparison after
// the module's task decides.
func interrupted(record *serviceRecord, plan *servicePlan) bool {
	if record.Job == "" {
		return false
	}
	for k, v := range plan.before {
		if old, ok := record.Inputs[k]; ok && old != v {
			return true
		}
	}
	for _, in := range plan.moduleInputs {
		if _, ok := record.Inputs[in.Key()]; ok {
			return true
		}
	}
	return false
}

// changedInputs reports whether any input of the kind ("" for all) in current
// has a different hash in base.
func changedInputs(kind string, base, current map[string]string) bool {
	for k, v := range current {
		if strings.HasPrefix(k, kind) && base[k] != v {
			return true
		}
	}
	return false
}

// definitionNames lists the unit names whose files systemd merges into a
// unit: its names and aliases and their templates ("app@.service" for
// "app@1.service").
func definitionNames(observed modules.ServiceState) map[string]bool {
	names := map[string]bool{}
	for _, name := range append([]string{observed.Unit}, observed.Names...) {
		names[name] = true
		kind := filepath.Ext(name)
		if prefix, instance, _ := strings.Cut(strings.TrimSuffix(name, kind), "@"); instance != "" {
			names[prefix+"@"+kind] = true
		}
	}
	return names
}

// In dryrun, remember each tracked input as its own task applies it, so a
// service predicts its hashes from that task's scope, not from its own.
func (e *Engine) preview(ctx context.Context, t config.Task, s value.Scope) {
	if !e.job.Dryrun || e.active == nil || e.active.applied == nil {
		return
	}
	e.active.inputPreview.add(ctx, t, s)
}

// add remembers what one tracked node task would apply.
func (p inputPreview) add(ctx context.Context, t config.Task, s value.Scope) {
	_, inputs := t.ModuleCall()
	tracked, ok := inputs.(interface{ TrackedInputs() []modules.TrackedInput })
	if !ok {
		return
	}
	if editor, ok := inputs.(interface {
		EditText(context.Context, string, value.Scope) (string, error)
	}); ok {
		// A task block's line and edit_file edits of one file apply in turn:
		// each edits the text the previous one would leave.
		path := tracked.TrackedInputs()[0].Path
		var text string
		if prior, ok := p.applied[path]; ok && prior.text != nil {
			text = *prior.text
		} else if b, err := os.ReadFile(path); err == nil {
			text = string(b)
		} else {
			return // the task reports the missing file
		}
		if next, err := editor.EditText(ctx, text, s); err == nil {
			p.applied[path] = appliedFile{content: modules.LiteralFile(path, next), text: &next}
		}
		return
	}
	if preview, ok := inputs.(interface{ PreviewApplies() bool }); ok && !preview.PreviewApplies() {
		return
	}
	for _, in := range tracked.TrackedInputs() {
		if in.Content != nil {
			p.applied[in.Path] = appliedFile{content: in.Content, scope: s.Local()}
		}
	}
	if expected, ok := inputs.(interface {
		ExpectedPackages() map[string]modules.PackageExpectation
	}); ok {
		maps.Copy(p.installed, expected.ExpectedPackages())
	}
}

// Resolve tracked node tasks' identifiers once, before changes, and run those node tasks
// with exactly the values hashed here. Contents still render in normal task
// order, but no later variable assignment can move a tracked destination.
func (e *Engine) prepareServices(ctx context.Context, t config.Task, s value.Scope, name string) (*taskBlockState, config.Task, error) {
	state := &taskBlockState{services: map[string]*servicePlan{}, commands: map[string]*commandPlan{}}
	if !config.TrackingTaskBlock(t) {
		return state, t, nil
	}
	fixed, err := fixTracked(&t, s, func(task config.Task, scope value.Scope) bool {
		ok, err := scope.PredicateOnNode(task.When, e.pathExists)
		return err == nil && !ok
	})
	if err != nil {
		return nil, t, err
	}
	state.inputPreview = newInputPreview()
	state.tracked = fixed["changes"]
	var files, packages, states, moduleInputs []modules.TrackedInput
	units := map[string][]modules.TrackedInput{}
	paths := config.TrackedPaths{}
	// unitKey names a unit file's unit within its systemd.
	unitKey := func(account, name string) string { return account + "\x00" + name }
	// A when that is false now, such as one on the node's processor
	// architecture, lets a task share its path with another task. A when that
	// cannot be decided yet counts as true. The tasks that may run come first,
	// so a shared path is hashed as the task that writes it.
	var mayRunFirst []*config.Task
	mayRun := map[*config.Task]bool{}
	for _, task := range fixed["changes"] {
		applies, err := boundScope(s, *task).PredicateOnNode(task.When, e.pathExists)
		if mayRun[task] = applies || err != nil; mayRun[task] {
			mayRunFirst = append(mayRunFirst, task)
		}
	}
	for _, task := range fixed["changes"] {
		if !mayRun[task] {
			mayRunFirst = append(mayRunFirst, task)
		}
	}
	for _, task := range mayRunFirst {
		_, inputs := task.ModuleCall()
		// A unit of an account's own systemd gets its path from the account's
		// home. An account that does not exist yet is not tracked: it cannot
		// have a running service that loaded anything.
		if located, ok := inputs.(interface{ LocateOnNode() }); ok {
			located.LocateOnNode()
		}
		for _, in := range config.TrackedInputs(*task) {
			switch {
			case in.Kind == "package":
				packages = append(packages, in)
			case in.Kind == "git" || in.Kind == "installed":
				// The input is read in its task's scope, where an installer
				// finds the task's env.
				in.Scope = boundScope(s, *task)
				states = append(states, in)
			case in.Kind == "module":
				// Node task blocks of one service that call the same module
				// each follow their own calls: the input's name in a record
				// holds the project, the fleet stage, and the block too.
				module := in.Path
				in.Path = strings.Join([]string{e.job.Project, e.job.FleetStage, name, module}, "\x00")
				if state.moduleKeys == nil {
					state.moduleKeys = map[string]string{}
				}
				state.moduleKeys[module] = in.Key()
				moduleInputs = append(moduleInputs, in)
			case in.OwnsPath():
				added, err := config.TrackPath(paths, in, mayRun[task], config.TrackedOwner(name+"/"+nodeTaskLabel(*task), *task))
				if err != nil {
					return nil, t, err
				}
				if !added {
					continue
				}
				if in.Kind == "unit" {
					units[unitKey(in.UnitAccount, in.UnitName)] = append(units[unitKey(in.UnitAccount, in.UnitName)], in)
				} else {
					files = append(files, in)
				}
			default:
				// A tracked input that nothing hashes would never restart
				// the service: a new kind needs its own case here.
				return nil, t, fmt.Errorf("tracked input kind %q of module %s has no handling", in.Kind, task.Module)
			}
		}
	}
	// A command follows every tracked input of the block, all units included.
	state.inputs = &servicePlan{files: files, packages: packages, states: states, moduleInputs: moduleInputs}
	for _, unit := range units {
		state.inputs.units = append(state.inputs.units, unit...)
	}
	// Inspect targets and save baselines before changing any files.
	// fixed["activate"] holds only the tasks that follow the tracked inputs
	// (config.Tracked), so each is a command or a service.
	for _, task := range fixed["activate"] {
		command, service := config.TrackedInputFollower(*task)
		if command != nil {
			if state.before == nil {
				if state.before, err = e.inputHashes(ctx, state.inputs, inputPreview{}); err != nil {
					return nil, t, err
				}
			}
			if err := e.prepareCommand(state, task, command, name); err != nil {
				return nil, t, err
			}
			continue
		}
		// An account that does not exist yet has no systemd to ask and no
		// running unit; its service task starts the unit or reports the account.
		m, err := modules.SystemdOf(service.InSystemdOfUserAccount) //nolint:nilaway // fixed["activate"] holds only tasks config.Tracked accepts, so a task without a command is a service.
		if err != nil {
			continue
		}
		observed, err := m.Inspect(ctx, service.Name)
		if err != nil {
			return nil, t, err
		}
		unit := observed.Unit
		name := m.Key(unit)
		service.Name = unit
		if _, exists := state.services[name]; exists {
			return nil, t, fmt.Errorf("duplicate service target %s", name)
		}
		record, err := e.readServiceRecord(name)
		if err != nil {
			return nil, t, err
		}
		plan := &servicePlan{files: files, packages: packages, states: states, moduleInputs: moduleInputs, task: *service, record: record, manager: m, unit: unit}
		for definition := range definitionNames(observed) {
			plan.units = append(plan.units, units[unitKey(m.Account, definition)]...)
		}
		plan.before, err = e.inputHashes(ctx, plan, inputPreview{})
		if err != nil {
			return nil, t, err
		}
		// A process instance MMX has no record for loaded the inputs present
		// when it started, as did the recorded instance for inputs its record
		// lacks. That does not hold for an instance that started while an
		// earlier attempt was changing the inputs it recorded: that record stays,
		// so the systemd_service task refreshes.
		baseline := record
		switch {
		case running(observed) && (record == nil || record.Instance != observed.Instance && !interrupted(record, plan)):
			baseline = &serviceRecord{Version: 1, Unit: name, Instance: observed.Instance, Inputs: map[string]string{}, Confirmed: true}
		case record == nil:
			baseline = &serviceRecord{Version: 1, Unit: name, Inputs: map[string]string{}}
		}
		added := baseline != record
		for k, v := range plan.before {
			//nolint:nilaway // The baseline switch allocates a record on every record == nil path.
			if _, ok := baseline.Inputs[k]; !ok {
				baseline.Inputs[k], added = v, true
			}
		}
		plan.unreported = awaitFingerprints(baseline.Inputs, moduleInputs)
		added = added || len(plan.unreported) > 0
		plan.record = baseline
		// Mark the record before the first write, so a later attempt knows this
		// one may have changed the inputs.
		if !e.job.Dryrun && (added || baseline.Job != e.job.ID) {
			baseline.Job = e.job.ID
			if err := e.saveServiceRecord(baseline, "baseline"); err != nil {
				return nil, t, fmt.Errorf("save initial service state and tracked inputs: %w", err)
			}
		}
		state.services[name] = plan
	}
	return state, t, nil
}

// nodeTaskLabel names a node task in a message: its id, else its module.
func nodeTaskLabel(t config.Task) string {
	if t.ID != "" {
		return t.ID
	}
	return t.Module
}
