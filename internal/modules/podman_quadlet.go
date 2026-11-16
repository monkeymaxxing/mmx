package modules

import (
	"context"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"mmx/internal/assets"
	"mmx/internal/durable"
	"mmx/internal/protocol"
	"mmx/internal/value"
)

// podman_quadlet installs one Quadlet file. At each reload of a systemd,
// Podman's generator turns every Quadlet file into a service, so a Quadlet
// file is the definition of its generated service, as a unit file is of its
// unit: the file is installed, checked, and followed by a manager reload with
// the code of systemd_unit (unit.go). The module starts, restarts, and stops
// nothing except in its two removal states; systemd_service does that.

// quadletSystemDirectory is where the system's Quadlet files go, and
// quadletGenerator the program systemd runs to turn them into services;
// tests replace both.
var (
	quadletSystemDirectory = "/etc/containers/systemd"
	quadletGenerator       = "/usr/lib/systemd/system-generators/podman-system-generator"
)

// quadletSystemOwner and quadletSystemGroup own the system's Quadlet files;
// tests, which do not run as root, replace them.
var quadletSystemOwner, quadletSystemGroup = Account("root"), Account("root")

// quadletDirectoryInHome is where an account's Quadlet files go, below its
// home.
var quadletDirectoryInHome = []string{".config", "containers", "systemd"}

// quadletMinimumVersion is the first Podman with Quadlet and with the
// generator's check-only mode.
var quadletMinimumVersion = []int{4, 4}

// quadletKindVersions gives the first Podman version that knows each kind of
// Quadlet file, from Podman's manual page of each release. An older
// generator ignores a file of a kind it does not know and says nothing, so
// the file would be installed and never become a service.
var quadletKindVersions = map[string][]int{
	".container": {4, 4}, ".volume": {4, 4}, ".kube": {4, 4}, ".network": {4, 4},
	".image": {4, 8}, ".pod": {5, 0}, ".build": {5, 2},
}

// The states of a Quadlet file. The two removals say whether data stays.
const (
	QuadletPresent                               = "present"
	QuadletFileAndServiceRemovedNamedVolumesKept = "file_and_service_removed_named_volumes_kept"
	QuadletVolumeAndItsDataRemoved               = "volume_and_its_data_removed"
)

var quadletStates = []string{QuadletPresent, QuadletFileAndServiceRemovedNamedVolumesKept, QuadletVolumeAndItsDataRemoved}

// quadletServiceSuffixes gives, for each kind of Quadlet file, what Podman's
// generator puts between the file's base name and ".service".
var quadletServiceSuffixes = map[string]string{
	".container": "", ".volume": "-volume", ".network": "-network", ".pod": "-pod", ".kube": "", ".image": "-image", ".build": "-build",
}

var quadletName = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.@:-]*\.(container|volume|network|pod|kube|image|build)$`)

var quadletNameRule = rule("name must be a Quadlet filename that ends in .container, .volume, .network, .pod, .kube, .image, or .build, such as shop-web.container", quadletName.MatchString)

// PodmanQuadlet is one Quadlet file of the system's systemd or of an
// ordinary account's.
type PodmanQuadlet struct {
	Name    string  `yaml:"name" json:"name"`
	Content *string `yaml:"content,omitempty" json:"content,omitempty"`
	// Template is a local Go template, resolved like systemd_unit.template.
	Template               string `yaml:"template,omitempty" json:"template,omitempty"`
	InSystemdOfUserAccount string `yaml:"in_systemd_of_user_account,omitempty" json:"in_systemd_of_user_account,omitempty"`
	State                  string `yaml:"state,omitempty" json:"state,omitempty"`

	TemplateText string      `yaml:"-" json:"-"`
	Asset        *assets.Ref `yaml:"-" json:"asset,omitempty"`
	LocalPath    string      `yaml:"-" json:"-"`
	// SourceBase and Variants serve a variable source field (variable_sources.go).
	SourceBase string                    `yaml:"-" json:"-"`
	Variants   map[string]*SourceVariant `yaml:"-" json:"variants,omitempty"`
	// The node fills in where an account's Quadlet files are and the IDs
	// that own them.
	AccountDirectory string  `yaml:"-" json:"-"`
	AccountOwner     Account `yaml:"-" json:"-"`
	AccountGroup     Account `yaml:"-" json:"-"`
}

// podmanQuadletDescription is what mmx describe-module shows for podman_quadlet.
var podmanQuadletDescription = Description{
	Name: "podman_quadlet", Topic: TopicContainers, Schema: reflect.TypeOf(PodmanQuadlet{}),
	Summary: "one Quadlet file, which Podman turns into a systemd service at each systemd reload, checked with Podman's generator before installation; value.generated_service names the service; needs Podman 4.4 or newer",
	Fields: []Field{
		{Name: "name", Required: true, Notes: "the Quadlet filename; its ending says what the file declares: .container, .volume, .network, .pod, .kube, .image, or .build; the system's files go to /etc/containers/systemd/, root, 0644"},
		{Name: "content", Notes: "inline text in Quadlet's unit syntax; exactly one of content or template with state: present; keep secret values in a separate private file that the Quadlet file names with EnvironmentFile="},
		{Name: "template", Notes: "local Go template, relative to the project root, or to the role's directory inside a role; exactly one of content or template with state: present"},
		{Name: "in_systemd_of_user_account", Notes: "an account other than root: the file goes to <home>/.config/containers/systemd/ owned by the account, the container runs as that account under its own systemd, and MMX turns on lingering for the account so that its services start at boot and keep running after logout; the account needs a home directory and ranges of sub-user and sub-group IDs"},
		{Name: "state", Values: quadletStates, Default: QuadletPresent, Notes: "file_and_service_removed_named_volumes_kept stops the generated service if it runs, deletes the file, reloads systemd, and for a .network file removes the Podman network; named volumes, images, and mounted directories stay; volume_and_its_data_removed is only for a .volume file and also deletes the named volume with the data in it, also when file_and_service_removed_named_volumes_kept removed the file before; both accept only name, in_systemd_of_user_account, and state"},
	},
	Example: "- podman_quadlet:\n    name: shop-web.container\n    content: |\n      [Container]\n      ContainerName=shop-web\n      Image=registry.example.com/shop/web:2.4\n\n      [Install]\n      WantedBy=multi-user.target\n",
	Results: []string{"status", "change", "reason", "value.generated_service"},
	Dryrun:  "supported; previews the file's contents and writes nothing; the generator's check of a changed file is not checked; reports would_change for an unchanged file that systemd has not read yet; a removal says in reason what it would delete and every service it would stop",
}

// removal reports whether the state removes the file.
func (q PodmanQuadlet) removal() bool {
	return q.State == QuadletFileAndServiceRemovedNamedVolumesKept || q.State == QuadletVolumeAndItsDataRemoved
}

// GeneratedService is the service Podman's generator makes from the file:
// shop-web.service from shop-web.container, shop-web-data-volume.service
// from shop-web-data.volume.
func (q PodmanQuadlet) GeneratedService() string {
	kind := filepath.Ext(q.Name)
	return strings.TrimSuffix(q.Name, kind) + quadletServiceSuffixes[kind] + ".service"
}

// EnabledOnGeneratedUnit is the failure of a systemd_service task that says
// start_at_boot for a unit that a generator of systemd makes anew at each reload,
// which systemd can neither enable nor disable. source is the file the unit
// is generated from, "" when systemd does not name it.
func EnabledOnGeneratedUnit(unit, source string) error {
	const remove = ", so systemd can neither enable nor disable it; remove start_at_boot from this task"
	const install = ". Whether the unit starts at boot is the [Install] section of that file: WantedBy=multi-user.target for the system's systemd, WantedBy=default.target for an account's"
	_, quadlet := quadletServiceSuffixes[filepath.Ext(source)]
	switch {
	case quadlet:
		return fmt.Errorf("unit %s is generated from the Quadlet file %s%s%s", unit, source, remove, install)
	case source != "":
		return fmt.Errorf("unit %s is generated from %s%s", unit, source, remove)
	}
	return fmt.Errorf("unit %s is generated from another file, such as a Podman Quadlet file%s%s", unit, remove, install)
}

// ValidatePodmanQuadlet checks the fields that hold no reference.
func ValidatePodmanQuadlet(q PodmanQuadlet) error {
	if q.Name == "" {
		return invalid("name", "podman_quadlet.name is required: the Quadlet filename, such as shop-web.container")
	}
	if q.State != "" && !slices.Contains(quadletStates, q.State) {
		return invalid("state", "podman_quadlet.state must be present, file_and_service_removed_named_volumes_kept, or volume_and_its_data_removed")
	}
	if q.removal() {
		if q.Content != nil || q.Template != "" || q.Asset != nil {
			return invalid("state", "podman_quadlet with state: "+q.State+" accepts only name, in_systemd_of_user_account, and state")
		}
		return nil
	}
	if (q.Content == nil) == (q.Template == "") {
		return fmt.Errorf("podman_quadlet requires exactly one of content or template")
	}
	return nil
}

// quadletNameFor is the name's rule, which a removal of a volume with its
// data narrows to .volume files; the state is static, so it chooses.
func quadletNameFor(state string) func(any) error {
	if state != QuadletVolumeAndItsDataRemoved {
		return quadletNameRule
	}
	return func(v any) error {
		if err := quadletNameRule(v); err != nil {
			return err
		}
		if name, _ := value.Scalar(v); filepath.Ext(name) != ".volume" {
			return fmt.Errorf("state: volume_and_its_data_removed is only for a .volume file; remove %s with state: file_and_service_removed_named_volumes_kept", name)
		}
		return nil
	}
}

// Prepare checks the inputs when the project loads and reads the template on
// the controller machine, relative to base.
func (q *PodmanQuadlet) Prepare(base string) error {
	if err := ValidatePodmanQuadlet(*q); err != nil {
		return err
	}
	q.SourceBase = base
	if q.Template != "" && !Dynamic(q.Template) {
		path, err := localSource(base, "template", q.Template)
		if err != nil {
			return err
		}
		b, err := readTemplate(path)
		if err != nil {
			return &FieldError{"template", err}
		}
		q.LocalPath, q.TemplateText = path, string(b)
	}
	return nil
}

// Arguments lists the inputs that may hold a reference, with their rules.
func (q *PodmanQuadlet) Arguments(arguments *ArgumentList) {
	arguments.Public("name", &q.Name, quadletNameFor(q.State))
	if q.InSystemdOfUserAccount != "" {
		arguments.Public("in_systemd_of_user_account", &q.InSystemdOfUserAccount, ordinaryAccount)
	}
	if q.Content != nil {
		arguments.Private("content", *q.Content, scalar)
	}
}

// Reads lists the template.
func (q *PodmanQuadlet) Reads() Reads {
	if q.Template == "" {
		return Reads{}
	}
	return Reads{Templates: map[string]string{"template": q.TemplateText}}
}

// ControllerFiles lists the template of the Quadlet file.
func (q *PodmanQuadlet) ControllerFiles() []ControllerFile {
	if q.Template == "" {
		return nil
	}
	return []ControllerFile{{Field: "template", Kind: SourceTemplate, Path: &q.Template, Base: q.SourceBase,
		Variants: &q.Variants, Asset: &q.Asset, Template: &q.TemplateText, Local: &q.LocalPath}}
}

// Run does the module's work on the node.
func (q *PodmanQuadlet) Run(ctx context.Context, task NodeTask) protocol.Result {
	return task.Runtime.PodmanQuadlet(ctx, *q, task.Scope)
}

// locateQuadlet fills in where an account's Quadlet file goes and the IDs
// that own it; a file of the system needs nothing.
func locateQuadlet(q PodmanQuadlet, m UnitManager) PodmanQuadlet {
	if m.Account != "" {
		q.AccountDirectory = filepath.Join(append([]string{m.Home}, quadletDirectoryInHome...)...)
		q.AccountOwner, q.AccountGroup = Account(strconv.Itoa(m.UID)), Account(strconv.Itoa(m.GID))
	}
	return q
}

// Located reports whether the file's path is known: always for a file of
// the system, and for an account's once the node looked the account up.
func (q PodmanQuadlet) Located() bool {
	return q.InSystemdOfUserAccount == "" || q.AccountDirectory != ""
}

// File returns the managed file. A removal state names it absent.
func (q PodmanQuadlet) File() File {
	directory, owner, group := quadletSystemDirectory, quadletSystemOwner, quadletSystemGroup
	if q.InSystemdOfUserAccount != "" {
		directory, owner, group = q.AccountDirectory, q.AccountOwner, q.AccountGroup
	}
	path := filepath.Join(directory, q.Name)
	if q.removal() {
		return File{Path: path, State: "absent"}
	}
	return File{Path: path, Content: q.Content, Template: q.Template, TemplateText: q.TemplateText, Asset: q.Asset, LocalPath: q.LocalPath, Mode: "0644", Owner: owner, Group: group}
}

// TrackedInputs lists the Quadlet file as the definition of its generated
// service, so that a systemd_service task on that service restarts it when
// the file changed. A file of an account whose home is not known yet has no
// path; that account cannot have a running service either.
func (q *PodmanQuadlet) TrackedInputs() []TrackedInput {
	if !q.Located() {
		return nil
	}
	f := q.File()
	return []TrackedInput{{Kind: "unit", Path: f.Path, Content: &f, UnitAccount: q.InSystemdOfUserAccount, UnitName: q.GeneratedService()}}
}

// LocateOnNode finds an account's Quadlet directory in the account's home.
// An account that does not exist yet is left as it is.
func (q *PodmanQuadlet) LocateOnNode() {
	if q.InSystemdOfUserAccount == "" {
		return
	}
	if m, err := SystemdOf(q.InSystemdOfUserAccount); err == nil {
		*q = locateQuadlet(*q, m)
	}
}

// quadletValue is the task's result value.
func quadletValue(q PodmanQuadlet) map[string]any {
	return map[string]any{"generated_service": q.GeneratedService()}
}

// PodmanQuadlet installs the Quadlet file after Podman's generator accepted
// it, and makes the file's systemd read its files again when the file
// changed or a reload is pending.
func (run Runtime) PodmanQuadlet(ctx context.Context, q PodmanQuadlet, s value.Scope) (result protocol.Result) {
	if err := ValidatePodmanQuadlet(q); err != nil {
		return protocol.ConfigurationError(err)
	}
	if q.removal() {
		return run.removePodmanQuadlet(ctx, q, s)
	}
	tool, stop := run.podman(ctx, "podman_quadlet", "in_systemd_of_user_account", q.InSystemdOfUserAccount)
	if stop != nil {
		return *stop
	}
	version, err := tool.version(ctx)
	if err != nil {
		return protocol.Failed(err)
	}
	if !versionAtLeast(version, quadletMinimumVersion) {
		return run.notYet(fmt.Errorf("podman_quadlet: Podman %s is installed, and Quadlet files need Podman 4.4 or newer; install a newer Podman in an earlier node task", version))
	}
	if kind := filepath.Ext(q.Name); !versionAtLeast(version, quadletKindVersions[kind]) {
		needed := quadletKindVersions[kind]
		return run.notYet(fmt.Errorf("podman_quadlet: Podman %s is installed, and %s Quadlet files need Podman %d.%d or newer; Podman %s would ignore %s without a word", version, kind, needed[0], needed[1], version, q.Name)) //nolint:nilaway // Every version-map entry has two integers; a missing entry makes versionAtLeast(version, nil) true, skipping this branch.
	}
	if !executableFile(quadletGenerator) {
		return run.notYet(fmt.Errorf("podman_quadlet: Podman's Quadlet generator %s is missing; install the podman package in an earlier node task", quadletGenerator))
	}
	m := tool.manager //nolint:nilaway // The preceding stop check guarantees that tool exists.
	if stop := run.checkContainerAccount("podman_quadlet", m); stop != nil {
		return *stop
	}
	q = locateQuadlet(q, m)
	f := q.File()
	defer func() { result.Value = quadletValue(q) }()
	parent, err := run.openDefinitionDirectory(ctx, m, quadletSystemDirectory, quadletDirectoryInHome, false)
	if err != nil {
		return protocol.Failed(err)
	}
	if parent != nil {
		defer parent.Close()
	}
	var check func(string) protocol.Result
	if !run.Dryrun {
		check = func(candidate string) protocol.Result {
			return run.checkQuadletCandidate(ctx, m, q, version, parent, candidate)
		}
	}
	result = run.fileAt(ctx, f, s, parent, 0644, check)
	if run.Dryrun && result.Status == "succeeded" && result.Change == "would_change" {
		// The generator needs the file's directory and its neighbours;
		// preview without a candidate.
		result.CheckNewFile = &protocol.Result{Status: "skipped", Change: "unchanged", Reason: "not checked: Podman's generator checks the Quadlet file when it is installed"}
	}
	if result.Status != "succeeded" {
		return result
	}
	// An account's container must start at boot and must not stop when the
	// account logs out. Lingering goes on only here, after Podman's check, so
	// a file that the check rejects leaves the account as it was.
	reportLingering, stop := run.keepAccountServicesRunning(ctx, m)
	if stop != nil {
		stop.FileChange = result.FileChange
		if result.Change == "changed" {
			stop.Change = "changed"
		}
		return *stop
	}
	defer func() { reportLingering(&result) }()
	failedAfterFile := func(err error) protocol.Result {
		failed := protocol.Failed(err)
		failed.Change, failed.FileChange = result.Change, result.FileChange
		return failed
	}
	// An installed .volume file names its volume itself.
	if !run.Dryrun && filepath.Ext(q.Name) == ".volume" {
		if err := q.forgetKeptVolume(); err != nil {
			return failedAfterFile(err)
		}
	}
	unit := q.GeneratedService()
	pending, err := quadletReloadPending(ctx, m, unit)
	result = run.reloadForDefinition(ctx, m, result, pending, err)
	if run.Dryrun || result.Status != "succeeded" || !m.Running() || template(unit) {
		return result
	}
	// The check accepted the file, so the service is there after the
	// reload. When systemd has none, say what its generator says now.
	observed, err := m.Inspect(ctx, unit)
	if err == nil && observed.Load == "not-found" {
		reason := quadletGeneratorMessages(runQuadletGenerator(ctx, m, filepath.Dir(f.Path)).Stderr)
		err = fmt.Errorf("podman_quadlet: systemd has no %s after its reload, although %s is installed; Podman's generator says: %s", unit, f.Path, orNoReason(reason))
	}
	if err != nil {
		return failedAfterFile(err)
	}
	return result
}

// quadletReloadPending reports whether the file's systemd has not read the
// installed Quadlet file as it is: systemd's own flag on the generated
// service, and a service that systemd does not have at all, because a
// generated service exists only once systemd ran Podman's generator. Both
// are what a run that stopped between the file and the reload leaves.
func quadletReloadPending(ctx context.Context, m UnitManager, unit string) (bool, error) {
	if template(unit) {
		return false, nil
	}
	observed, err := m.Inspect(ctx, unit)
	return observed.NeedDaemonReload || observed.Load == "not-found", err
}

// quadletGeneratorTimeLimit bounds one run of Podman's generator, which
// reads a few small files and ends within milliseconds. The generator reads
// every file of the directories it is given and waits without end on one that
// is not a regular file, such as a named pipe. Tests shorten the limit.
var quadletGeneratorTimeLimit = time.Minute

// runQuadletGenerator runs Podman's generator in its check-only mode on the
// Quadlet files of the directories, the first of which wins for a filename
// that several have. It prints each service it would make after a line
// ---<service>---, and writes nothing. An account's files are read by a
// generator that the account runs, as the account's own systemd runs it: the
// account fills its Quadlet directory, and a generator run by root would
// read whatever a link there points to.
func runQuadletGenerator(ctx context.Context, m UnitManager, directories ...string) protocol.Result {
	limited, cancel := context.WithTimeout(ctx, quadletGeneratorTimeLimit)
	defer cancel()
	env := map[string]string{"QUADLET_UNIT_DIRS": strings.Join(directories, string(os.PathListSeparator))}
	var r protocol.Result
	if m.Account == "" {
		r = Command(limited, env, "/", quadletGenerator, "-dryrun")
	} else {
		maps.Copy(env, m.env)
		r = runProcess(limited, env, "/", nil, m.credential, quadletGenerator, "-dryrun", "-user").Result
	}
	if r.Status != "succeeded" && limited.Err() != nil && ctx.Err() == nil {
		r.Reason = fmt.Sprintf("Podman's generator did not end within %s; it reads every file in %s and waits without end on one that is not a regular file, such as a named pipe", quadletGeneratorTimeLimit, directories[len(directories)-1]) //nolint:nilaway // Both production callers pass explicit directory arguments: one directory or three directories.
	}
	return r
}

// quadletStandInMark is in every service that Podman's generator makes from
// a file of quadletStandIns, and in no other.
const quadletStandInMark = "mmx-stands-in-for-the-installed-file"

// quadletStandIns holds, for each kind of Quadlet file, the smallest file
// from which Podman's generator makes a service that carries the mark.
var quadletStandIns = map[string]string{
	".container": "[Container]\nImage=localhost/" + quadletStandInMark + "\n",
	".volume":    "[Volume]\nVolumeName=" + quadletStandInMark + "\n",
	".network":   "[Network]\nNetworkName=" + quadletStandInMark + "\n",
	".kube":      "[Kube]\nYaml=/" + quadletStandInMark + ".yaml\n",
	".image":     "[Image]\nImage=localhost/" + quadletStandInMark + "\n",
	".pod":       "[Pod]\nPodName=" + quadletStandInMark + "\n",
	".build":     "[Build]\nImageTag=localhost/" + quadletStandInMark + "\nFile=/" + quadletStandInMark + "/Containerfile\n",
}

// checkQuadletCandidate asks Podman's generator about the new file together
// with the installed Quadlet files, before the new file is installed. The
// generator gets three directories, and for each filename it makes a service
// from the first file of that name that it can read:
//
//  1. a copy of the new file, alone, under its final name;
//  2. a stand-in of the same name, which the generator can always read;
//  3. the installed Quadlet files, which the new file may name, such as the
//     .volume file of a container's volume.
//
// The file is accepted when the generator makes its service from the new
// file. Without the stand-in, a new file that the generator cannot read would
// pass on the strength of the installed file of the same name, which the
// generator reads in its place. The exit code decides nothing: it would fail
// a good file beside another installed file that the generator rejects, and
// older generators exit with 0 for a file they cannot read.
//
// The first two directories are in the task's private directory
// (makePrivateTaskDirectory), which belongs to the account whose systemd the
// file is for, because that account runs the generator.
func (run Runtime) checkQuadletCandidate(ctx context.Context, m UnitManager, q PodmanQuadlet, version string, parent *os.Root, candidate string) protocol.Result {
	installed := q.File().Path
	failure := func(err error) protocol.Result {
		return protocol.Failed(fmt.Errorf("podman_quadlet: the private directory for Podman's check of the new Quadlet file: %w", err))
	}
	// The new file is read through the pinned directory, never by its path:
	// an account can replace what is in its own Quadlet directory.
	inDirectory, err := filepath.Rel(filepath.Dir(installed), candidate)
	if err != nil {
		return failure(err)
	}
	text, err := readTextAt(parent, inDirectory)
	if err != nil {
		return failure(err)
	}
	directory, remove, err := run.makePrivateTaskDirectory("podman_quadlet", m.credential, func(directory string) error {
		for name, text := range map[string]string{"new-file": text, "stand-in": quadletStandIns[filepath.Ext(q.Name)]} {
			if err := os.Mkdir(filepath.Join(directory, name), 0700); err != nil {
				return err
			}
			if err := os.WriteFile(filepath.Join(directory, name, q.Name), []byte(text), 0600); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return failure(err)
	}
	defer remove()
	newFile, standIn := filepath.Join(directory, "new-file"), filepath.Join(directory, "stand-in")
	r := runQuadletGenerator(ctx, m, newFile, standIn, filepath.Dir(installed))
	unit := q.GeneratedService()
	made := slices.Contains(strings.Split(r.Stdout, "\n"), "---"+unit+"---")
	fromStandIn := strings.Contains(r.Stdout, quadletStandInMark)
	if made && !fromStandIn {
		return protocol.Success("unchanged")
	}
	// What the generator says about the files, without the stand-in. It
	// names the new file where the copy is; the reader knows it by where it
	// goes.
	var messages []string
	for _, line := range strings.Split(quadletGeneratorMessages(r.Stderr), "\n") {
		if line != "" && !strings.Contains(line, standIn) {
			messages = append(messages, line)
		}
	}
	reason := strings.ReplaceAll(strings.Join(messages, "\n"), filepath.Join(newFile, q.Name), installed)
	switch {
	case fromStandIn:
		// The generator passed over the new file and read the stand-in.
		// Podman before 5 says nothing about a file it cannot read.
		cannotRead := "the generator cannot read the file as a Quadlet file, in which each line is a [Section] name, a Key=value pair, or a comment"
		if reason != "" {
			cannotRead += "; it says: "
		}
		reason = cannotRead + reason
	case reason == "" && r.Status != "succeeded":
		reason = r.Reason
	case reason == "":
		reason = "Podman's Quadlet generator gave no reason; MMX expects the service name derived from the Quadlet filename, so the Quadlet file must not set ServiceName="
	}
	failed := protocol.Failed(fmt.Errorf("podman_quadlet: Podman's check of the new Quadlet file failed: the Quadlet generator of Podman %s makes no %s from %s: %s", version, unit, q.Name, reason))
	failed.Stderr = r.Stderr
	return failed
}

// quadletGeneratorMessages keeps what the generator says about the files:
// its lines without their "quadlet-generator[<process ID>]: " prefix and
// without the one line for each file it loads.
func quadletGeneratorMessages(output string) string {
	var lines []string
	for _, line := range strings.Split(output, "\n") {
		if strings.HasPrefix(line, "quadlet-generator[") {
			if _, message, found := strings.Cut(line, "]: "); found {
				line = message
			}
		}
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "Loading source unit file ") {
			continue
		}
		lines = append(lines, line)
	}
	return strings.Join(lines, "\n")
}

func orNoReason(reason string) string {
	if reason == "" {
		return "nothing"
	}
	return reason
}

// quadletObject is what a Quadlet file made in Podman that outlives its
// service and that a removal state removes with a podman command: the
// network of a .network file, and the volume of a .volume file when the
// state removes its data.
type quadletObject struct {
	// kind is volume or network, the podman subcommand; section and key
	// name the object in the Quadlet file.
	kind, section, key string
}

var (
	quadletVolume  = quadletObject{"volume", "Volume", "VolumeName"}
	quadletNetwork = quadletObject{"network", "Network", "NetworkName"}
)

// removedObject says which object of Podman the removal removes, if any.
func (q PodmanQuadlet) removedObject() *quadletObject {
	switch {
	case filepath.Ext(q.Name) == ".network":
		return &quadletNetwork
	case filepath.Ext(q.Name) == ".volume" && q.State == QuadletVolumeAndItsDataRemoved:
		return &quadletVolume
	}
	return nil
}

// quadletObjectName reads the name a Quadlet file gives its volume or
// network (VolumeName= or NetworkName=). Without the line, and for a file
// that is gone, it is Podman's default: systemd- and the file's base name.
func quadletObjectName(text string, object quadletObject, file string) (string, error) {
	name, section := "", ""
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = line[1 : len(line)-1]
			continue
		}
		if section != object.section || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if key, written, found := strings.Cut(line, "="); found && strings.TrimSpace(key) == object.key {
			name = strings.TrimSpace(written)
		}
	}
	if name == "" {
		return "systemd-" + strings.TrimSuffix(file, filepath.Ext(file)), nil
	}
	if strings.Contains(name, "%") {
		return "", fmt.Errorf("%s=%s in %s uses a systemd specifier, so MMX cannot tell the %s's name; remove the %s with a shell task that runs podman %s rm", object.key, name, file, object.kind, object.kind, object.kind)
	}
	return name, nil
}

// quadletKeptVolumeRecordRoot holds MMX's record of the volumes whose
// .volume file a task removed with state: file_and_service_removed_named_volumes_kept; tests
// replace it.
var quadletKeptVolumeRecordRoot = "/var/lib/mmx/podman_volumes_of_removed_quadlet_files"

// keptVolumeRecord is the file in which MMX keeps the name of the volume of
// a .volume file that state: file_and_service_removed_named_volumes_kept removed. Only the
// Quadlet file says which volume is its own (VolumeName=): Podman keeps
// nothing that leads from a volume back to the file whose service created
// it. A later task with state: volume_and_its_data_removed for the same file
// finds the volume through this record and then removes the record; a task
// that installs the file again removes it too.
func (q PodmanQuadlet) keptVolumeRecord() string {
	of := "system"
	if q.InSystemdOfUserAccount != "" {
		of = "account_" + q.InSystemdOfUserAccount
	}
	return filepath.Join(quadletKeptVolumeRecordRoot, of, q.Name)
}

// keptVolume reads the record, "" when there is none.
func (q PodmanQuadlet) keptVolume() (string, error) {
	text, err := os.ReadFile(q.keptVolumeRecord())
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("podman_quadlet: MMX's record of the volume of %s: %w", q.Name, err)
	}
	return strings.TrimSpace(string(text)), nil
}

func (q PodmanQuadlet) recordKeptVolume(volume string) error {
	path := q.keptVolumeRecord()
	err := os.MkdirAll(filepath.Dir(path), 0700)
	if err == nil {
		err = durable.Write(path, []byte(volume+"\n"), 0600)
	}
	if err != nil {
		return fmt.Errorf("podman_quadlet: MMX's record of the volume of %s: %w", q.Name, err)
	}
	return nil
}

func (q PodmanQuadlet) forgetKeptVolume() error {
	if err := os.Remove(q.keptVolumeRecord()); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("podman_quadlet: MMX's record of the volume of %s: %w", q.Name, err)
	}
	return nil
}

// stoppedTogetherWith lists the running units that systemd stops when unit
// stops: those that require it, are bound to it, or are part of it, and
// those that need one of these in the same way. The generated service of a
// container requires the services of the volume and network files it names.
func stoppedTogetherWith(ctx context.Context, m UnitManager, unit string) ([]string, error) {
	// show reads whether a unit runs and which units stop with it.
	show := func(name string) (runs bool, dependents []string, err error) {
		r := m.systemctl(ctx, "show", "--property=ActiveState,RequiredBy,BoundBy,ConsistsOf", "--", name)
		if r.Status != "succeeded" {
			return false, nil, fmt.Errorf("podman_quadlet: systemctl show %s: %s: %s", name, r.Reason, strings.TrimSpace(r.Stderr))
		}
		for _, line := range strings.Split(r.Stdout, "\n") {
			switch property, written, _ := strings.Cut(line, "="); property {
			case "ActiveState":
				runs = written != "inactive" && written != "failed"
			case "RequiredBy", "BoundBy", "ConsistsOf":
				dependents = append(dependents, strings.Fields(written)...)
			}
		}
		return runs, dependents, nil
	}
	_, waiting, err := show(unit)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{unit: true}
	var stopped []string
	for len(waiting) > 0 {
		name := waiting[0]
		waiting = waiting[1:]
		if seen[name] {
			continue
		}
		seen[name] = true
		runs, dependents, err := show(name)
		if err != nil {
			return nil, err
		}
		if runs {
			stopped = append(stopped, name)
			waiting = append(waiting, dependents...)
		}
	}
	return stopped, nil
}

// removePodmanQuadlet removes the network or volume that the state names,
// stops the generated service if it runs, deletes the Quadlet file, and
// reloads systemd. Everything is inspected first, so a removal that is
// already complete reports unchanged.
//
// The network or volume goes first, before anything is stopped and before
// the file, which holds its name. Podman refuses to remove one that a
// container still uses; the task then fails with nothing changed. The other
// order would lose data: a container's generated service requires the
// service of the volume file it names, so stopping the volume's service
// makes systemd stop that container, and Podman would then delete a volume
// that was in use a moment before.
func (run Runtime) removePodmanQuadlet(ctx context.Context, q PodmanQuadlet, s value.Scope) (result protocol.Result) {
	m, err := SystemdOf(q.InSystemdOfUserAccount)
	if err != nil {
		return run.accountFailure(fmt.Errorf("podman_quadlet.in_systemd_of_user_account: %w", err))
	}
	q = locateQuadlet(q, m)
	f := q.File()
	unit := q.GeneratedService()
	defer func() { result.Value = quadletValue(q) }()
	parent, err := run.openDefinitionDirectory(ctx, m, quadletSystemDirectory, quadletDirectoryInHome, true)
	if err != nil {
		return protocol.Failed(err)
	}
	installed, text := false, ""
	if parent != nil {
		defer parent.Close()
		info, err := parent.Lstat(q.Name)
		if err != nil && !os.IsNotExist(err) {
			return protocol.Failed(err)
		}
		installed = info != nil
		// A .volume or .network file names what it made in Podman.
		if kind := filepath.Ext(q.Name); installed && (kind == ".volume" || kind == ".network") {
			if text, err = readTextAt(parent, q.Name); err != nil {
				return protocol.Failed(err)
			}
		}
	}
	observed, err := m.Inspect(ctx, unit)
	if err != nil {
		return protocol.Failed(err)
	}
	active := observed.Load != "not-found" && observed.Active != "inactive" && observed.Active != "failed"

	// The volume or network: its name, and whether Podman has it. The
	// installed file names it; for a volume whose file an earlier removal
	// took and whose data it kept, MMX's record does.
	object, objectName := q.removedObject(), ""
	var tool *podmanTool
	objectExists := false
	// note says what an unchanged result could not check.
	note := ""
	if object != nil {
		recorded := ""
		if !installed && object.kind == "volume" {
			if recorded, err = q.keptVolume(); err != nil {
				return protocol.Failed(err)
			}
		}
		if objectName = recorded; objectName == "" {
			if objectName, err = quadletObjectName(text, *object, q.Name); err != nil {
				return protocol.Failed(fmt.Errorf("podman_quadlet: %w", err))
			}
		}
		// named: the file or the record says which volume or network it is,
		// so Podman may have it, and only podman can say and remove it.
		named := installed || recorded != ""
		var stop *protocol.Result
		tool, stop = run.podman(ctx, "podman_quadlet", "in_systemd_of_user_account", q.InSystemdOfUserAccount)
		switch {
		case stop != nil && named:
			return *stop
		case !m.Running() && named:
			return run.notYet(fmt.Errorf("podman_quadlet: the systemd of account %s does not run, so Podman cannot be asked about the account's %s %s; start it with systemctl start user@%d.service", m.Account, object.kind, objectName, m.UID))
		case stop != nil:
			note = fmt.Sprintf("%s is gone and the program podman is not installed, so MMX could not ask about a %s", f.Path, object.kind)
		case !m.Running():
			note = fmt.Sprintf("%s is gone and the systemd of account %s does not run, so MMX could not ask Podman about a %s", f.Path, m.Account, object.kind)
		default:
			exists := []string{object.kind, "exists", objectName}
			r := tool.command(ctx, exists...)
			if r.Status != "succeeded" && (r.ExitCode != 1 || ctx.Err() != nil) {
				return protocol.Failed(tool.failure(exists, r))
			}
			objectExists = r.Status == "succeeded"
			if !named && !objectExists {
				note = fmt.Sprintf("%s is gone, so MMX could check only Podman's default name for its %s, and Podman has no %s named %s", f.Path, object.kind, object.kind, objectName)
			}
		}
	}

	// Each step in the words of a dryrun and of a run.
	type step struct{ would, did string }
	var steps []step
	if objectExists {
		would, did := "remove the Podman network "+objectName, "removed the Podman network "+objectName
		//nolint:nilaway // objectExists becomes true only inside the object != nil branch.
		if object.kind == "volume" {
			would, did = "delete the Podman volume "+objectName+" with the data in it", "deleted the Podman volume "+objectName+" with the data in it"
		}
		steps = append(steps, step{would, did})
	}
	if active {
		// systemd stops more than the one service when others need it.
		others, err := stoppedTogetherWith(ctx, m, unit)
		if err != nil {
			return protocol.Failed(err)
		}
		with := ""
		if len(others) > 0 {
			with = " and with it " + strings.Join(others, ", ") + ", which systemd stops when " + unit + " stops"
		}
		steps = append(steps, step{"stop " + unit + with, "stopped " + unit + with})
	}
	if installed {
		steps = append(steps, step{"delete " + f.Path, "deleted " + f.Path})
	}
	if run.Dryrun {
		if len(steps) == 0 && !observed.NeedDaemonReload {
			r := protocol.Success("unchanged")
			r.Reason = note
			return r
		}
		r := run.fileAt(ctx, f, s, parent, 0644, nil)
		if r.Status != "succeeded" {
			return r
		}
		r.Change = "would_change"
		would := []string{}
		for _, step := range steps {
			would = append(would, step.would)
		}
		r.Reason = "would " + strings.Join(append(would, "reload systemd"), ", then ")
		return r
	}
	// A failure keeps what the task changed before it.
	var did []string
	failedAfter := func(err error) protocol.Result {
		failed := protocol.Failed(err)
		if len(did) > 0 {
			failed.Change = "changed"
		}
		return failed
	}
	if objectExists {
		//nolint:nilaway // The objectExists guard implies that object was non-nil during inspection.
		if err := run.Mutation("podman " + object.kind + " " + objectName); err != nil {
			return protocol.Failed(err)
		}
		remove := []string{object.kind, "rm", objectName} //nolint:nilaway // The objectExists guard implies that object was non-nil during inspection.
		if r := tool.command(ctx, remove...); r.Status != "succeeded" {
			// Podman refused, as for a volume or network in use: nothing
			// is changed, and nothing is stopped.
			failed := protocol.Failed(tool.failure(remove, r))
			failed.Change = "unchanged"
			return failed
		}
		did = append(did, steps[0].did) //nolint:nilaway // The earlier objectExists branch appended the first step; no step has been consumed yet.
		//nolint:nilaway // The same objectExists condition guarantees the first step exists before slicing it away.
		steps = steps[1:]
	}
	// The file is about to go, and with it the name of a volume that stays.
	// A name that MMX cannot tell from the file is not recorded.
	if installed && q.State == QuadletFileAndServiceRemovedNamedVolumesKept && filepath.Ext(q.Name) == ".volume" {
		if volume, err := quadletObjectName(text, quadletVolume, q.Name); err == nil {
			if err := q.recordKeptVolume(volume); err != nil {
				failed := failedAfter(err)
				if len(did) == 0 {
					failed.Change = "unchanged"
				}
				return failed
			}
		}
	}
	if active {
		if err := run.Mutation("service " + unit); err != nil {
			return failedAfter(err)
		}
		if r := m.Action(ctx, "stop", unit); r.Status != "succeeded" {
			return failedAfter(fmt.Errorf("podman_quadlet: systemctl stop %s: %s: %s", unit, r.Reason, strings.TrimSpace(r.Stderr)))
		}
		did = append(did, steps[0].did) //nolint:nilaway // The earlier active branch appended this step; any preceding object-removal step has already been consumed.
		//nolint:nilaway // The active condition guarantees an unconsumed service-stop step before slicing.
		steps = steps[1:]
	}
	result = run.fileAt(ctx, f, s, parent, 0644, nil)
	if result.Status != "succeeded" {
		if len(did) > 0 {
			result.Change = "changed"
		}
		return result
	}
	if installed {
		did = append(did, steps[0].did) //nolint:nilaway // The earlier installed branch appended this step after the optional object-removal and service-stop steps.
	}
	if len(did) > 0 {
		result.Change = "changed"
	}
	// The volume and its file are gone, so the record has nothing to name.
	if q.State == QuadletVolumeAndItsDataRemoved {
		if err := q.forgetKeptVolume(); err != nil {
			failed := failedAfter(err)
			failed.FileChange = result.FileChange
			return failed
		}
	}
	pending, err := definitionPending(ctx, m, unit)
	result = run.reloadForDefinition(ctx, m, result, pending, err)
	if result.Status == "succeeded" {
		if result.Reason = strings.Join(did, ", then "); result.Reason == "" && result.Change == "unchanged" {
			result.Reason = note
		}
	}
	return result
}
