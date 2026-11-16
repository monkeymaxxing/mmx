package modules

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"mmx/internal/protocol"
)

// DNF is job-scoped: a changed repository file or dnf setting makes the next
// command refresh dnf's index first, and a run refreshes it once per job
// before it reads the newest versions (refreshForNewest). kept records, for
// each package a dnf_package task in the job named, whether that task kept
// it at a version.
type DNF struct {
	// Root is / on a node; tests use a temporary directory for the files MMX
	// reads and writes for dnf: the version lock list and the image marker.
	Root string
	// indexOld: a repository file or dnf setting changed in this job after
	// dnf last refreshed its index.
	indexOld bool
	// indexRefreshed: a dnf command of this job refreshed the index.
	indexRefreshed bool
	kept           map[string]bool
	streams        map[string]string
	// five: the node's dnf is dnf 5 (isDnfFive), set when a task starts.
	five bool
}

func (d *DNF) Name() string { return "dnf" }

func (d *DNF) PathChanged(path string) {
	for _, dir := range []string{"/etc/yum.repos.d/", "/etc/dnf/", "/etc/pki/rpm-gpg/"} {
		if strings.HasPrefix(path, dir) {
			d.indexOld = true
		}
	}
}

func (d *DNF) root() string {
	if d.Root == "" {
		return "/"
	}
	return d.Root
}

var rpmName = regexp.MustCompile(`^[A-Za-z0-9_+][A-Za-z0-9_.+-]*$`)

// rpmVersion writes an rpm EVR as dnf shows it to people: without an epoch
// that is missing or zero.
func rpmVersion(evr string) string {
	for _, none := range []string{"(none):", "0:"} {
		if strings.HasPrefix(evr, none) {
			return strings.TrimPrefix(evr, none)
		}
	}
	return evr
}

func (d *DNF) Inspect(ctx context.Context, name string) (PackageState, error) {
	if !rpmName.MatchString(name) {
		return PackageState{}, fmt.Errorf("invalid package name %q", name)
	}
	r := Command(ctx, dnfEnv, "", "rpm", "-q", "--qf", "%{EPOCH}:%{VERSION}-%{RELEASE}\n", name)
	if r.ExitCode == 1 && strings.Contains(r.Stdout+r.Stderr, "is not installed") {
		return PackageState{Name: name, State: "absent"}, nil
	}
	if r.Status != "succeeded" {
		return PackageState{}, fmt.Errorf("inspect package %s: %s: %s", name, r.Reason, r.Stderr)
	}
	// A package installed in several versions, such as a kernel, lists each.
	lines := strings.Fields(r.Stdout)
	if len(lines) == 0 {
		return PackageState{}, fmt.Errorf("invalid package status for %s: %q", name, r.Stdout)
	}
	return PackageState{Name: name, Version: rpmVersion(lines[len(lines)-1]), State: "installed"}, nil
}

// dnfEnv keeps dnf's messages, which MMX reads, in English.
var dnfEnv = map[string]string{"LC_ALL": "C"}

func (d *DNF) Ensure(ctx context.Context, run Runtime, p Package) protocol.Result {
	if err := ValidatePackage(p); err != nil {
		return protocol.ConfigurationError(err)
	}
	d.five = isDnfFive(ctx)
	if _, err := os.Stat(filepath.Join(d.root(), "run/ostree-booted")); err == nil {
		return protocol.Failed(fmt.Errorf("dnf_package: this node's system is an image (/run/ostree-booted exists), where dnf cannot install packages"))
	}
	if err := d.claim(p); err != nil {
		return protocol.Failed(err)
	}
	if err := d.checkRepositories(ctx, p); err != nil {
		return protocol.Failed(err)
	}
	switch {
	case p.State == "absent":
		return d.remove(ctx, run, p)
	case p.UpgradeAll:
		return d.upgradeAll(ctx, run, p)
	}
	// Check prerequisites that do not need the stream before enabling it.
	// Files and removal tasks cannot select a stream (ValidatePackage).
	for _, entry := range p.Packages {
		if isVersionRule(entry.Version) {
			if d.five {
				if _, err := dnfFiveLockBlock(entry.Name, entry.Version); err != nil {
					return protocol.Failed(err)
				}
			} else if err := d.lockAvailable(); err != nil {
				return run.notYet(err)
			}
		}
	}
	stream := protocol.Success("unchanged")
	if p.EnableDnfModuleStream != "" {
		stream = d.enableStream(ctx, run, p)
		if stream.Status != "succeeded" {
			return stream
		}
		if run.Dryrun && stream.Change == "would_change" {
			return run.notYet(fmt.Errorf("dnf_package: module stream %s is not enabled; dnf hides its packages until it is enabled, so this dryrun cannot check the installation", p.EnableDnfModuleStream))
		}
	}
	result := d.installPackages(ctx, run, p)
	if !run.Dryrun {
		// A later refusal leaves an enabled stream in place, ready for the
		// next run. A dryrun's proposed stream change never became real.
		result.Change = protocol.CombineChange(stream.Change, result.Change)
	}
	return result
}

// moduleStreams are dnf's record of the module streams a node chose.
const moduleStreams = "/etc/dnf/modules.d"

// enableStream enables the task's module stream before its installation on
// Enterprise Linux 8 and 9. It changes nothing when another stream of the
// module is enabled or the module is switched off (switching a stream
// replaces installed software, an administrator's decision), when a named
// package is already installed from outside the stream (the next upgrade
// would replace it with the stream's version), or when the node's
// repositories have no such stream.
func (d *DNF) enableStream(ctx context.Context, run Runtime, p Package) protocol.Result {
	module, want, _ := strings.Cut(p.EnableDnfModuleStream, ":")
	stream, state := "", ""
	b, err := os.ReadFile(filepath.Join(d.root(), moduleStreams, module+".module"))
	if err != nil && !os.IsNotExist(err) {
		return protocol.Failed(err)
	}
	for _, line := range strings.Split(string(b), "\n") {
		key, value, _ := strings.Cut(strings.TrimSpace(line), "=")
		switch key {
		case "stream":
			stream = value
		case "state":
			state = value
		}
	}
	switch {
	case state == "enabled" && stream == want:
		return protocol.Success("unchanged")
	case state == "enabled":
		return protocol.Failed(fmt.Errorf("dnf_package: the module %s has the stream %s enabled, not %s. Switching a stream replaces installed software; an administrator does it with dnf module reset %s, then this task", module, stream, want, module))
	case state == "disabled":
		return protocol.Failed(fmt.Errorf("dnf_package: the module %s is switched off on this node (dnf module disable). An administrator turns it back on with dnf module reset %s, then this task enables %s", module, module, p.EnableDnfModuleStream))
	}
	options := append([]string{"-q"}, d.options(p, false)...)
	listed := Command(ctx, dnfEnv, "", "dnf", append(options, "module", "list", module)...)
	var streams []string
	for _, line := range strings.Split(listed.Stdout, "\n") {
		if fields := strings.Fields(line); len(fields) >= 2 && fields[0] == module && !containsString(streams, fields[1]) {
			streams = append(streams, fields[1])
		}
	}
	if len(streams) == 0 {
		return protocol.Failed(fmt.Errorf("dnf_package: this node's repositories have no module %s, so no stream %s: on this node other versions are separate package names, such as %s16-server; dnf list '%s*' shows them. Take enable_dnf_module_stream out of the task", module, p.EnableDnfModuleStream, module, module))
	}
	if !containsString(streams, want) {
		return protocol.Failed(fmt.Errorf("dnf_package: the module %s has no stream %s on this node; it has: %s", module, want, strings.Join(streams, ", ")))
	}
	for _, name := range p.TrackedNames() {
		current, err := d.Inspect(ctx, name)
		if err != nil {
			return protocol.Failed(err)
		}
		if current.State == "installed" {
			return protocol.Failed(fmt.Errorf("dnf_package: %s %s is already installed from outside the stream %s; enabling the stream would let the next upgrade replace it with the stream's version. Moving installed software to another major version is an administrator's step, such as a one-time playbook", name, current.Version, p.EnableDnfModuleStream))
		}
	}
	if run.Dryrun {
		return protocol.Success("would_change")
	}
	if err := run.Mutation("dnf module stream " + p.EnableDnfModuleStream); err != nil {
		return protocol.Failed(err)
	}
	if r := d.change(ctx, p, "module", "enable", p.EnableDnfModuleStream); r.Status != "succeeded" {
		return r
	}
	return protocol.Success("changed")
}

// claim refuses a package that one task of the job keeps at a version and
// another names without one: the two tasks would undo each other on every run.
func (d *DNF) claim(p Package) error {
	if p.State == "absent" || p.UpgradeAll {
		return nil
	}
	if d.kept == nil {
		d.kept = map[string]bool{}
	}
	if p.EnableDnfModuleStream != "" {
		if d.streams == nil {
			d.streams = map[string]string{}
		}
		module := ModuleOfStream(p.EnableDnfModuleStream)
		if earlier, named := d.streams[module]; named && earlier != p.EnableDnfModuleStream {
			return fmt.Errorf("dnf_package: this node job enables both %s and %s; dnf enables one stream of a module", earlier, p.EnableDnfModuleStream)
		}
		d.streams[module] = p.EnableDnfModuleStream
	}
	for _, entry := range p.Packages {
		name, kept := entry.Name, isVersionRule(entry.Version)
		if earlier, named := d.kept[name]; named && earlier != kept {
			return fmt.Errorf("dnf_package: %s has a version rule in one task of this node job and a version keyword in another; the two tasks would undo each other on every run. Name the package in one task", name)
		}
	}
	for _, entry := range p.Packages {
		d.kept[entry.Name] = isVersionRule(entry.Version)
	}
	return nil
}

// checkRepositories fails a task that names a repository the node does not
// define, with the ids it has.
func (d *DNF) checkRepositories(ctx context.Context, p Package) error {
	if len(p.RepositoriesEnabledOnlyForThisTask) == 0 {
		return nil
	}
	r := Command(ctx, dnfEnv, "", "dnf", "-q", "repolist", "--all")
	if r.Status != "succeeded" {
		return fmt.Errorf("dnf repolist: %s: %s", r.Reason, r.Stderr)
	}
	var ids []string
	for i, line := range strings.Split(r.Stdout, "\n") {
		if fields := strings.Fields(line); i > 0 && len(fields) > 0 {
			ids = append(ids, fields[0])
		}
	}
	for _, id := range p.RepositoriesEnabledOnlyForThisTask {
		if !containsString(ids, id) {
			return fmt.Errorf("dnf_package: this node defines no repository %s; it has: %s", id, strings.Join(ids, ", "))
		}
	}
	return nil
}

// options are the options of a dnf command for the task: --refresh when the
// command refreshes dnf's index first, and the repositories it may use.
func (d *DNF) options(p Package, refresh bool) []string {
	var options []string
	if refresh {
		options = append(options, "--refresh")
	}
	for _, id := range p.RepositoriesEnabledOnlyForThisTask {
		options = append(options, "--enablerepo="+id)
	}
	return options
}

// refreshForNewest says whether a command that reads the newest versions the
// repositories offer, an upgrade or a version lookup, refreshes dnf's index
// first: in a run, once per job and again after a repository changed. A
// dryrun reads the index the node has.
func (d *DNF) refreshForNewest(run Runtime) bool {
	return !run.Dryrun && (!d.indexRefreshed || d.indexOld)
}

// refreshed records a command that refreshed dnf's index.
func (d *DNF) refreshed() { d.indexOld, d.indexRefreshed = false, true }

// change runs one dnf command that changes packages, refreshing dnf's index
// first after a repository changed in this job; a failed command leaves the
// change unknown.
func (d *DNF) change(ctx context.Context, p Package, args ...string) protocol.Result {
	refresh := d.indexOld
	r := Command(ctx, dnfEnv, "", "dnf", append(append([]string{"-y"}, d.options(p, refresh)...), args...)...)
	if r.Status != "succeeded" {
		r.Change = "unknown"
		return r
	}
	if refresh {
		d.refreshed()
	}
	return r
}

// simulate asks dnf what a command would do, without doing it, and lists the
// packages that would change as {name, from, to}; refresh refreshes dnf's
// index first.
func (d *DNF) simulate(ctx context.Context, p Package, refresh bool, args ...string) ([]any, error) {
	r := Command(ctx, dnfEnv, "", "dnf", append(append([]string{"--assumeno"}, d.options(p, refresh)...), args...)...)
	output := r.Stdout + r.Stderr
	if r.Status != "succeeded" && !strings.Contains(output, "Operation aborted") {
		return nil, fmt.Errorf("dnf %s: %s: %s", strings.Join(args, " "), r.Reason, strings.TrimSpace(output))
	}
	if refresh {
		d.refreshed()
	}
	return d.transactionChanges(ctx, r.Stdout)
}

// transactionChanges reads dnf's transaction table. A section names what
// happens to the packages under it ("Installing:", "Upgrading:", "Removing
// unused dependencies:"); a package line gives name, architecture, and
// version.
func (d *DNF) transactionChanges(ctx context.Context, table string) ([]any, error) {
	var changes []any
	section := ""
	for _, line := range strings.Split(table, "\n") {
		fields := strings.Fields(line)
		switch {
		case strings.HasPrefix(line, "Transaction Summary"):
			return d.withInstalledVersions(ctx, changes)
		case len(fields) > 0 && fields[0] == "replacing":
			// dnf 5 lists the package an upgrade replaces under the new one.
		case line != "" && line[0] != ' ' && strings.HasSuffix(line, ":"):
			section = line
		case len(fields) >= 3 && line[0] == ' ':
			name, version := fields[0], rpmVersion(fields[2])
			var change map[string]any
			switch {
			case strings.HasPrefix(section, "Removing"):
				change = packageChange(name, version, "")
			case strings.HasPrefix(section, "Installing"), strings.HasPrefix(section, "Upgrading"), strings.HasPrefix(section, "Downgrading"):
				change = packageChange(name, "", version)
				if !strings.HasPrefix(section, "Installing") {
					change["from"] = ""
				}
			}
			if change != nil {
				changes = append(changes, change)
			}
		}
	}
	return d.withInstalledVersions(ctx, changes)
}

// withInstalledVersions fills the from version of an upgrade or downgrade,
// which dnf's table leaves out.
func (d *DNF) withInstalledVersions(ctx context.Context, changes []any) ([]any, error) {
	for _, item := range changes {
		change := item.(map[string]any)
		// An installation has no from.
		if from, present := change["from"]; !present || from != "" {
			continue
		}
		current, err := d.Inspect(ctx, change["name"].(string))
		if err != nil {
			return nil, err
		}
		delete(change, "from")
		if current.State == "installed" {
			change["from"] = current.Version
		}
	}
	return changes, nil
}

// installPackages resolves versions in the node's active streams, checks
// every entry and the complete transaction before writing a lock, then
// installs all entries in one dependency transaction.
func (d *DNF) installPackages(ctx context.Context, run Runtime, p Package) protocol.Result {
	type plannedPackage struct {
		entry           PackageEntry
		version, target string
	}
	var planned []plannedPackage
	var changes []any
	directory := ""
	defer func() {
		if directory != "" {
			os.RemoveAll(directory)
		}
	}()
	for _, entry := range p.Packages {
		current, err := d.Inspect(ctx, entry.Name)
		if err != nil {
			return protocol.Failed(err)
		}
		plan := plannedPackage{entry: entry, target: entry.Name}
		switch {
		case entry.HasPackageFile():
			if entry.PackageFile == nil {
				return protocol.Failed(fmt.Errorf("dnf_package: %s package file has no saved asset", entry.Name))
			}
			if directory == "" {
				directory, err = os.MkdirTemp("", "mmx-package-file-")
				if err != nil {
					return protocol.Failed(err)
				}
			}
			plan.target = filepath.Join(directory, fmt.Sprintf("package-%d.rpm", len(planned)))
			if err = copyAsset(entry.PackageFile, plan.target); err != nil {
				return protocol.Failed(err)
			}
			read := Command(ctx, dnfEnv, "", "rpm", "-qp", "--qf", "%{NAME}\t%{EPOCH}:%{VERSION}-%{RELEASE}", plan.target)
			name, version, ok := strings.Cut(strings.TrimSpace(read.Stdout), "\t")
			if read.Status != "succeeded" || !ok {
				return protocol.Failed(fmt.Errorf("dnf_package: read the package file: %s %s", read.Reason, read.Stderr))
			}
			version = rpmVersion(version)
			if name != entry.Name {
				return protocol.Failed(fmt.Errorf("dnf_package: the package file holds the package %s, not %s", name, entry.Name))
			}
			if version != entry.Version {
				return protocol.Failed(fmt.Errorf("dnf_package: the package file for %s holds version %s, not the entry's version %s", name, version, entry.Version))
			}
			plan.version = version
		case isVersionRule(entry.Version):
			if current.State == "installed" && versionMatches(entry.Version, current.Version) {
				plan.version = current.Version
				break
			}
			available, err := d.availableVersions(ctx, run, p, entry.Name)
			if err != nil {
				return protocol.Failed(err)
			}
			plan.version, err = d.newestMatching(ctx, available, entry.Version)
			if err != nil {
				return protocol.Failed(err)
			}
			if plan.version == "" {
				return run.notYet(fmt.Errorf("dnf_package: no version of %s in the node's repositories matches %s; available: %s", entry.Name, entry.Version, listOrNone(available)))
			}
		case entry.Version == newestOnEveryRun && d.five && current.State == "installed":
			// dnf 5's install leaves an installed package alone; naming the
			// newest version makes it upgrade, as dnf 4's install does.
			available, err := d.availableVersions(ctx, run, p, entry.Name)
			if err != nil {
				return protocol.Failed(err)
			}
			// Versions that an administrator's lock table excludes are
			// not candidates; dnf 5's repoquery lists them.
			var allowed []string
			for _, version := range available {
				if d.administratorLockExcludes(ctx, entry.Name, version) == nil {
					allowed = append(allowed, version)
				}
			}
			newest, err := d.newestMatching(ctx, allowed, "*")
			if err != nil {
				return protocol.Failed(err)
			}
			plan.version = current.Version
			if newer, err := rpmNewer(ctx, newest, current.Version); err != nil {
				return protocol.Failed(err)
			} else if newer {
				plan.version = newest
			}
		case entry.Version == newestWhenFirstInstalled:
			if current.State == "installed" {
				plan.version = current.Version
			}

		}
		if current.State == "installed" && plan.version != "" && plan.version != current.Version {
			older, err := rpmNewer(ctx, current.Version, plan.version)
			if err != nil {
				return protocol.Failed(err)
			}
			if older && !entry.AllowDowngrade {
				return protocol.Failed(fmt.Errorf("dnf_package: %s %s is installed, which is newer than %s; to install the older version, add allow_downgrade: true to its packages entry", entry.Name, current.Version, plan.version))
			}
		}
		if d.five && plan.version != "" && (current.State != "installed" || plan.version != current.Version) {
			if err := d.administratorLockExcludes(ctx, entry.Name, plan.version); err != nil {
				return protocol.Failed(err)
			}
		}
		if !entry.HasPackageFile() && plan.version != "" {
			plan.target = entry.Name + "-" + plan.version
		}
		if current.State != "installed" || plan.version != "" && plan.version != current.Version {
			change := packageChange(entry.Name, current.Version, plan.version)
			if plan.version == "" {
				change["to"] = ""
			}
			changes = append(changes, change)
		}
		planned = append(planned, plan)
	}

	needsTransaction := len(changes) > 0
	for _, entry := range p.Packages {
		needsTransaction = needsTransaction || entry.Version == newestOnEveryRun
	}
	if needsTransaction {
		if directory == "" {
			var err error
			directory, err = os.MkdirTemp("", "mmx-dnf-preview-")
			if err != nil {
				return protocol.Failed(err)
			}
		}
		options, err := d.previewVersionLocks(p, directory)
		if err != nil {
			return protocol.Failed(err)
		}
		args := []string{"install"}
		for _, plan := range planned {
			args = append(args, plan.target)
		}
		refresh := d.refreshForNewest(run)
		options = append(append([]string{"--assumeno"}, d.options(p, refresh)...), options...)
		command := append(append([]string{}, options...), args...)
		if d.five {
			stale, err := d.oldRulesDiffer(p)
			if err != nil {
				return protocol.Failed(err)
			}
			if stale {
				command = append([]string{"--setopt=disable_excludes=*"}, command...)
			}
		}
		preview := Command(ctx, dnfEnv, "", "dnf", command...)
		output := preview.Stdout + preview.Stderr
		if preview.Status != "succeeded" && !strings.Contains(output, "Operation aborted") {
			err := fmt.Errorf("dnf install preview: %s: %s", preview.Reason, strings.TrimSpace(output))
			if packageMissingFromCurrentIndexes(output) {
				return run.notYet(err)
			}
			return protocol.Failed(err)
		}
		if refresh {
			d.refreshed()
		}
		native, err := d.transactionChanges(ctx, preview.Stdout)
		if err != nil {
			return protocol.Failed(err)
		}
		for i := range planned {
			if planned[i].version != "" {
				continue
			}
			for _, item := range native {
				change := item.(map[string]any)
				if change["name"] == planned[i].entry.Name {
					planned[i].version, _ = change["to"].(string)
					if planned[i].version != "" {
						planned[i].target = planned[i].entry.Name + "-" + planned[i].version
					}
				}
			}
		}
		changes = mergePackageChanges(changes, native)
	}

	result := protocol.Success("unchanged")
	for _, plan := range planned {
		var written protocol.Result
		if isVersionRule(plan.entry.Version) {
			written = d.lock(ctx, run, plan.entry.Name, plan.entry.Version)
		} else {
			written = d.dropLocks(ctx, run, []string{plan.entry.Name})
		}
		written.Change = protocol.CombineChange(result.Change, written.Change)
		if written.Status != "succeeded" {
			return written
		}
		result = written
	}
	if len(changes) == 0 {
		return result
	}
	result.Value = knownInstallationChanges(changes)
	if run.Dryrun {
		result.Change = "would_change"
		return result
	}
	if err := run.Mutation("packages"); err != nil {
		failed := protocol.Failed(err)
		failed.Change = result.Change
		return failed
	}
	args := []string{"install"}
	for _, plan := range planned {
		args = append(args, plan.target)
	}
	if r := d.change(ctx, p, args...); r.Status != "succeeded" {
		r.Change = protocol.CombineChange(result.Change, r.Change)
		return r
	}
	for _, plan := range planned {
		if err := d.expectInstalled(ctx, plan.entry.Name, plan.version); err != nil {
			return changedFailure(err)
		}
	}
	result.Change = "changed"
	return result
}

// administratorLockExcludes fails when an administrator's table in dnf 5's
// lock file keeps a version out. It runs before MMX writes anything, because
// the preview that follows a rule change cannot see the administrator's
// tables (see oldRulesDiffer), and the install would be refused after the
// rule was written.
func (d *DNF) administratorLockExcludes(ctx context.Context, name, version string) error {
	b, err := os.ReadFile(filepath.Join(d.root(), dnfFiveLockTable))
	if err != nil {
		return nil
	}
	excluded, err := dnfFiveExcludes(ctx, b, name, version)
	if err != nil {
		return err
	}
	if excluded {
		return fmt.Errorf("dnf_package: %s %s is outside a version lock that an administrator wrote in %s, so MMX writes no rule and installs nothing. Change that table, or name a version it allows", name, version, dnfFiveLockTable)
	}
	return nil
}

// oldRulesDiffer says whether the task would change a rule that MMX already
// keeps in dnf 5's lock file, or drop it. dnf 5 offers no way to read another
// lock file than the node's, and the old rule hides the versions the task
// wants from the preview ("Nothing to do" or "excluded by versionlock"). So
// that preview runs with dnf's excludes switched off, which also hides the
// administrator's tables and the repositories' exclude settings. The check
// of the planned versions against the administrator's tables
// (refuseVersionsTheLockExcludes) runs before it, and the preview is the only
// one that does not see exclude settings of the repositories.
func (d *DNF) oldRulesDiffer(p Package) (bool, error) {
	b, err := os.ReadFile(filepath.Join(d.root(), dnfFiveLockTable))
	if err != nil {
		return false, nil
	}
	for _, entry := range p.Packages {
		owned, err := dnfFiveOwnedTables(b, entry.Name)
		if err != nil {
			return false, err
		}
		if owned == 0 {
			continue
		}
		block := ""
		if isVersionRule(entry.Version) {
			if block, err = dnfFiveLockBlock(entry.Name, entry.Version); err != nil {
				return false, err
			}
		}
		text, err := dnfFiveLockText(b, entry.Name, block)
		if err != nil {
			return false, err
		}
		if text != string(b) {
			return true, nil
		}
	}
	return false, nil
}

func (d *DNF) expectInstalled(ctx context.Context, name, version string) error {
	installed, err := d.Inspect(ctx, name)
	if err != nil {
		return err
	}
	if installed.State == "absent" && version == "" {
		// dnf also installs a package by a name it only provides, such as
		// nc for nmap-ncat; MMX checks packages by name.
		r := Command(ctx, dnfEnv, "", "rpm", "-q", "--whatprovides", "--qf", "%{NAME}\n", name)
		if provider := strings.TrimSpace(strings.SplitN(r.Stdout, "\n", 2)[0]); r.Status == "succeeded" && provider != "" && provider != name {
			return fmt.Errorf("there is no package named %s on this node: dnf installed %s, which provides %s; name the package %s instead", name, provider, name, provider)
		}
	}
	if installed.State != "installed" || (version != "" && installed.Version != version) {
		return fmt.Errorf("package %s did not reach the requested installed state (version=%s, state=%s)", name, installed.Version, installed.State)
	}
	return nil
}

// rpmNewer is rpm's answer whether version a is newer than b; an empty b is
// older than every version.
func rpmNewer(ctx context.Context, a, b string) (bool, error) {
	if b == "" {
		return true, nil
	}
	for _, v := range []string{a, b} {
		if strings.ContainsAny(v, `"\`) {
			return false, fmt.Errorf("invalid version %q", v)
		}
	}
	r := Command(ctx, nil, "", "rpm", "--eval", fmt.Sprintf(`%%{lua: print(rpm.vercmp("%s", "%s"))}`, a, b))
	order, err := strconv.Atoi(strings.TrimSpace(r.Stdout))
	if r.Status != "succeeded" || err != nil {
		return false, fmt.Errorf("compare versions %s and %s: %s: %s", a, b, r.Reason, r.Stderr)
	}
	return order > 0, nil
}

// availableVersions are the versions of a package that the node's
// repositories offer, whatever a version lock allows, from an index that a
// run refreshes once per job.
func (d *DNF) availableVersions(ctx context.Context, run Runtime, p Package, name string) ([]string, error) {
	refresh := d.refreshForNewest(run)
	options := append([]string{"-q"}, d.options(p, refresh)...)
	r := Command(ctx, dnfEnv, "", "dnf", append(options, "repoquery", "--qf", "%{epoch}:%{version}-%{release}\\n", name)...)
	if r.Status != "succeeded" {
		return nil, fmt.Errorf("list versions of %s: %s: %s", name, r.Reason, r.Stderr)
	}
	if refresh {
		d.refreshed()
	}
	var versions []string
	for _, line := range strings.Split(strings.TrimSpace(r.Stdout), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if v := rpmVersion(strings.TrimSpace(line)); !containsString(versions, v) {
			versions = append(versions, v)
		}
	}
	return versions, nil
}

// previewVersionLocks retains administrator locks and stages this task's rules
// in a temporary plugin configuration. Other plugins keep their configuration.
func (d *DNF) previewVersionLocks(p Package, directory string) ([]string, error) {
	if d.five {
		// dnf 5 reads only /etc/dnf/versionlock.toml; installPackages
		// handles a rule of the task that this file's old rule excludes.
		return nil, nil
	}
	plugins := filepath.Join(directory, "plugins")
	if err := os.Mkdir(plugins, 0755); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(filepath.Join(d.root(), "/etc/dnf/plugins"))
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".conf") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(d.root(), "/etc/dnf/plugins", entry.Name()))
		if err != nil {
			return nil, err
		}
		if err := os.WriteFile(filepath.Join(plugins, entry.Name()), b, 0600); err != nil {
			return nil, err
		}
	}
	list, err := os.ReadFile(filepath.Join(d.root(), versionLockList))
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	for _, entry := range p.Packages {
		line := ""
		if isVersionRule(entry.Version) {
			line = dnfLockSpecification(entry.Name, entry.Version)
		}
		list = []byte(versionLockText(list, entry.Name, line))
	}
	locklist := filepath.Join(directory, "versionlock.list")
	if err := os.WriteFile(locklist, list, 0600); err != nil {
		return nil, err
	}
	path := filepath.Join(plugins, "versionlock.conf")
	config, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var lines []string
	for _, line := range strings.Split(string(config), "\n") {
		key, _, _ := strings.Cut(strings.TrimSpace(line), "=")
		if strings.TrimSpace(key) == "locklist" {
			continue
		}
		lines = append(lines, line)
		if strings.TrimSpace(line) == "[main]" {
			lines = append(lines, "locklist="+locklist)
		}
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0600); err != nil {
		return nil, err
	}
	return []string{"--setopt=pluginconfpath=" + plugins}, nil
}

// newestMatching is the newest available version that matches pattern.
func (d *DNF) newestMatching(ctx context.Context, available []string, pattern string) (string, error) {
	newest := ""
	for _, v := range available {
		if !versionMatches(pattern, v) {
			continue
		}
		newer, err := rpmNewer(ctx, v, newest)
		if err != nil {
			return "", err
		}
		if newer {
			newest = v
		}
	}
	return newest, nil
}

// The version lock: a plugin of dnf with a list of patterns, which MMX writes
// itself because the plugin's own add command stores the versions that match
// today. A comment directly before each MMX entry records its ownership;
// administrator entries stay when MMX adds, replaces, or drops its own rule.

// versionLockList is the version lock list.
func versionLockMark(name string) string { return "# Written by MMX for package " + name }

const versionLockList = "/etc/dnf/plugins/versionlock.list"

// lockAvailable fails without the version lock plugin, naming the package
// that provides it.
func (d *DNF) lockAvailable() error {
	if _, err := os.Stat(filepath.Join(d.root(), "/etc/dnf/plugins/versionlock.conf")); err != nil {
		return fmt.Errorf("dnf_package: keeping a package at a version needs dnf's version lock plugin, which this node does not have; add a dnf_package task with packages: [{name: python3-dnf-plugin-versionlock, version: newest_when_first_installed}] before this one")
	}
	return nil
}

// lockLine checks the line after an ownership comment. Its version may
// begin with a wildcard; the exact comment already identifies the package.
func lockLine(line, name string) bool {
	rest, ok := strings.CutPrefix(strings.TrimSpace(line), name+"-")
	return ok && rest != ""
}

// lockEntries reads the version lock list once and returns the lines of one
// package.
func (d *DNF) lockEntries() (func(name string) []string, error) {
	if d.five {
		b, err := os.ReadFile(filepath.Join(d.root(), dnfFiveLockTable))
		if err != nil && !os.IsNotExist(err) {
			return nil, err
		}
		if _, _, err := parseDnfFiveLock(string(b)); err != nil {
			return nil, err
		}
		return func(name string) []string {
			count, _ := dnfFiveOwnedTables(b, name)
			return make([]string, count)
		}, nil
	}
	b, err := os.ReadFile(filepath.Join(d.root(), versionLockList))
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	return func(name string) []string {
		var entries []string
		previous := ""
		for _, line := range strings.Split(string(b), "\n") {
			if previous == versionLockMark(name) && lockLine(line, name) {
				entries = append(entries, strings.TrimSpace(line))
			}
			previous = line
		}
		return entries
	}, nil
}

// dnfLockSpecification states the epoch explicitly, so a hyphenated name
// cannot also parse as the name/version/release of a different package.
// A leading wildcard can match an epoch in MMX's complete version text.
func dnfLockSpecification(name, pattern string) string {
	if !strings.Contains(pattern, ":") {
		epoch := "0:"
		if strings.HasPrefix(pattern, "*") {
			epoch = "*:"
		}
		pattern = epoch + pattern
	}
	return name + "-" + pattern
}

// lock writes dnf's raw package specification. Do not append an architecture
// glob: a version-only rule such as 16.* is a NEV specification, where adding
// .* would change the VERSION pattern to 16.*.* and exclude version 16.15.
func (d *DNF) lock(ctx context.Context, run Runtime, name, pattern string) protocol.Result {
	if d.five {
		block, err := dnfFiveLockBlock(name, pattern)
		if err != nil {
			return protocol.Failed(err)
		}
		return d.writeLockList(ctx, run, name, block)
	}
	return d.writeLockList(ctx, run, name, dnfLockSpecification(name, pattern))
}

// writeLockList rewrites the list with line as the package's only entry,
// or with none when line is empty. Other lines stay; the file is never
// deleted, because the plugin fails every dnf command without it.
func (d *DNF) writeLockList(ctx context.Context, run Runtime, name, line string) protocol.Result {
	path := filepath.Join(d.root(), versionLockList)
	if d.five {
		path = filepath.Join(d.root(), dnfFiveLockTable)
	}
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) && line == "" {
		return protocol.Success("unchanged")
	}
	if err != nil && !os.IsNotExist(err) {
		return protocol.Failed(err)
	}
	text := versionLockText(b, name, line)
	if d.five {
		if text, err = dnfFiveLockText(b, name, line); err != nil {
			return protocol.Failed(err)
		}
	}
	if text == string(b) {
		return protocol.Success("unchanged")
	}
	return run.File(ctx, File{Path: path, Literal: &text, Mode: "0644"}, nil)
}

func versionLockText(b []byte, name, line string) string {
	owned, desired := 0, 0
	originalLines := strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
	for i, existing := range originalLines {
		if existing == versionLockMark(name) && i+1 < len(originalLines) && lockLine(originalLines[i+1], name) {
			owned++
			if originalLines[i+1] == line {
				desired++
			}
		}
	}
	if line == "" && owned == 0 || line != "" && owned == 1 && desired == 1 {
		return string(b)
	}
	var lines []string
	existing := originalLines
	for i := 0; i < len(existing); i++ {
		if existing[i] == versionLockMark(name) && i+1 < len(existing) && lockLine(existing[i+1], name) {
			i++
			continue
		}
		if existing[i] != "" {
			lines = append(lines, existing[i])
		}
	}
	if line != "" {
		lines = append(lines, versionLockMark(name), line)
	}
	text := strings.Join(lines, "\n")
	if text != "" {
		text += "\n"
	}
	return text
}

// dropLocks deletes the version lock entries of packages a task names without
// a version.
func (d *DNF) dropLocks(ctx context.Context, run Runtime, names []string) protocol.Result {
	result := protocol.Success("unchanged")
	locks, err := d.lockEntries()
	if err != nil {
		return protocol.Failed(err)
	}
	for _, name := range names {
		if len(locks(name)) == 0 {
			continue
		}
		r := d.writeLockList(ctx, run, name, "")
		r.Change = protocol.CombineChange(result.Change, r.Change)
		if r.Status != "succeeded" {
			return r
		}
		result = r
	}
	return result
}

// upgradeAll is dnf's upgrade of the whole node, then, when the task asks,
// autoremove. Dryrun asks dnf what it would do against the index the node
// has; apply refreshes the index first.
func (d *DNF) upgradeAll(ctx context.Context, run Runtime, p Package) protocol.Result {
	upgrade := []string{"upgrade"}
	security := p.OnlySecurityUpdates != nil && *p.OnlySecurityUpdates
	if security {
		upgrade = append(upgrade, "--security")
	}
	upgrades, err := d.simulate(ctx, p, d.refreshForNewest(run), upgrade...)
	if err == nil && security {
		// In a run, the index is fresh by now. Reuse that cache; in dryrun
		// neither command refreshes it. Include applied advisories, and
		// every advisory type, so a fully patched node is not refused.
		err = d.checkAdvisories(ctx, p)
	}
	var removals []any
	if err == nil && p.AlsoRemovePackagesNoLongerNeeded {
		removals, err = d.simulate(ctx, p, false, "autoremove")
	}
	if err != nil {
		return protocol.Failed(err)
	}
	if len(upgrades)+len(removals) == 0 {
		return protocol.Success("unchanged")
	}
	result := protocol.Success("would_change")
	result.Value = append(upgrades[:len(upgrades):len(upgrades)], removals...)
	if run.Dryrun {
		return result
	}
	if err := run.Mutation("packages"); err != nil {
		return protocol.Failed(err)
	}
	if r := d.change(ctx, p, upgrade...); r.Status != "succeeded" {
		return r
	}
	if p.AlsoRemovePackagesNoLongerNeeded {
		// What is no longer needed is known only after the upgrade.
		removed, err := d.simulate(ctx, p, false, "autoremove")
		if err != nil {
			return changedFailure(err)
		}
		result.Value = append(upgrades[:len(upgrades):len(upgrades)], removed...)
		if r := d.change(ctx, p, "autoremove"); r.Status != "succeeded" {
			r.Change = "changed"
			return r
		}
	}
	result.Change = "changed"
	return result
}

// checkAdvisories distinguishes an empty update selection from missing advisory
// information. dnf lists applied as well as pending advisories with --all,
// an applied one after the marker i.
func (d *DNF) checkAdvisories(ctx context.Context, p Package) error {
	args := append([]string{"-q", "--cacheonly"}, d.options(p, false)...)
	args = append(args, "updateinfo", "list", "--all")
	listed := Command(ctx, dnfEnv, "", "dnf", args...)
	if listed.Status != "succeeded" {
		return fmt.Errorf("dnf updateinfo list --all: %s: %s", listed.Reason, strings.TrimSpace(listed.Stderr))
	}
	for _, line := range strings.Split(listed.Stdout, "\n") {
		fields := strings.Fields(line)
		if len(fields) > 0 && fields[0] == "i" {
			fields = fields[1:]
		}
		if len(fields) < 3 {
			continue
		}
		switch fields[1] {
		// dnf 4 shows a security advisory as Severity/Sec. (handled below)
		// and an unclassified one as unknown; dnf 5 names the type in words.
		case "bugfix", "enhancement", "newpackage", "unknown", "security", "unspecified":
			return nil
		}
		if strings.HasSuffix(fields[1], "/Sec.") {
			return nil
		}
	}
	return fmt.Errorf("dnf_package: the node's package sources list no advisories for its packages, so dnf cannot select security updates; use upgrade_all_packages_within_their_version_rules: true without only_security_updates there")
}

// remove removes the named packages and, as dnf remove does, the packages
// that depend on them, or also the packages only they needed when the task
// asks, then deletes their version lock entries.
func (d *DNF) remove(ctx context.Context, run Runtime, p Package) protocol.Result {
	var remove []string
	for _, name := range p.TrackedNames() {
		current, err := d.Inspect(ctx, name)
		if err != nil {
			return protocol.Failed(err)
		}
		if current.State == "installed" {
			remove = append(remove, name)
		}
	}
	if len(remove) == 0 {
		return d.dropLocks(ctx, run, p.TrackedNames())
	}
	args := append([]string{"remove", "--setopt=clean_requirements_on_remove=" + strconv.FormatBool(p.AlsoRemovePackagesOnlyItNeeded)}, remove...)
	changes, err := d.simulate(ctx, p, false, args...)
	if err != nil {
		return protocol.Failed(err)
	}
	result := protocol.Success("would_change")
	result.Value = changes
	if err := p.checkRemovalScope(changes); err != nil {
		return protocol.ConfigurationError(err)
	}
	if run.Dryrun {
		return result
	}
	if err := run.Mutation("packages"); err != nil {
		return protocol.Failed(err)
	}
	if r := d.change(ctx, p, args...); r.Status != "succeeded" {
		return r
	}
	for _, name := range remove {
		current, err := d.Inspect(ctx, name)
		if err == nil && current.State != "absent" {
			err = fmt.Errorf("package %s was not removed", name)
		}
		if err != nil {
			return changedFailure(err)
		}
	}
	if locks := d.dropLocks(ctx, run, p.TrackedNames()); locks.Status != "succeeded" {
		locks.Change = "changed"
		return locks
	}
	result.Change = "changed"
	return result
}
