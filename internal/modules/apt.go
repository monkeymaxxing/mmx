package modules

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"mmx/internal/assets"
	"mmx/internal/protocol"
)

type dpkgPackage struct{ Name, Version, State string }

func packageAbsent(state string) bool {
	return state == "absent" || state == "not-installed" || state == "config-files"
}

func inspectDPKG(ctx context.Context, name string) (dpkgPackage, error) {
	if !packageName.MatchString(name) {
		return dpkgPackage{}, fmt.Errorf("invalid package name %q", name)
	}
	r := Command(ctx, nil, "", "dpkg-query", "-W", "-f=${Version}\t${db:Status-Status}", "--", name)
	if r.ExitCode == 1 {
		return dpkgPackage{Name: name, State: "absent"}, nil
	}
	if r.Status != "succeeded" {
		return dpkgPackage{}, fmt.Errorf("inspect package %s: %s: %s", name, r.Reason, r.Stderr)
	}
	// A purged package that dpkg still lists is not-installed without a version.
	version, state, ok := strings.Cut(strings.TrimRight(r.Stdout, "\n"), "\t")
	if !ok || state == "" || (version == "" && state != "not-installed") {
		return dpkgPackage{}, fmt.Errorf("invalid package status for %s: %q", name, r.Stdout)
	}
	return dpkgPackage{name, version, state}, nil
}

var packageName = regexp.MustCompile(`^[a-z0-9][a-z0-9+.-]*(?::[a-z0-9]+)?$`)

// APT is job-scoped: index refresh is shared by that job's apt_package node
// tasks until a task changes an apt source, key, or setting under /etc/apt/,
// or the package sources differ from those indexed: a vendor's release
// package or a shell task can add a source that no tracked path reports.
// indexedSources is sourcesFingerprint at the last refresh. kept records, for
// each package an apt_package task in the job named, whether that task kept it
// at a version.
type APT struct {
	// Root is / on a node; tests use a temporary directory for the files MMX
	// writes for apt, its version rules and its service policy program, and
	// for the package sources it reads.
	Root           string
	indexesFresh   bool
	indexedSources string
	kept           map[string]bool
}

func (a *APT) Name() string { return "apt" }

func (a *APT) PathChanged(path string) {
	if strings.HasPrefix(path, "/etc/apt/") {
		a.indexesFresh = false
	}
}

func (a *APT) Inspect(ctx context.Context, name string) (PackageState, error) {
	p, err := inspectDPKG(ctx, name)
	if err != nil {
		return PackageState{}, err
	}
	state := "incomplete"
	if packageAbsent(p.State) {
		state, p.Version = "absent", ""
	} else if p.State == "installed" {
		state = "installed"
	}
	return PackageState{Name: name, Version: p.Version, State: state}, nil
}

func (a *APT) Ensure(ctx context.Context, run Runtime, p Package) protocol.Result {
	if err := ValidatePackage(p); err != nil {
		return protocol.ConfigurationError(err)
	}
	if err := a.claim(p); err != nil {
		return protocol.Failed(err)
	}
	switch {
	case p.State == "absent":
		return a.remove(ctx, run, p)
	case p.UpgradeAll:
		return a.upgradeAll(ctx, run, p)
	default:
		return a.installPackages(ctx, run, p)
	}
}

// claim refuses a package that one task of the job keeps at a version and
// another names without one: the two tasks would undo each other on every run.
func (a *APT) claim(p Package) error {
	if p.State == "absent" || p.UpgradeAll {
		return nil
	}
	if a.kept == nil {
		a.kept = map[string]bool{}
	}
	for _, entry := range p.Packages {
		name, kept := entry.Name, isVersionRule(entry.Version)
		if earlier, named := a.kept[name]; named && earlier != kept {
			return fmt.Errorf("apt_package: %s has a version rule in one node task and a version keyword in another node task of the node job; the conflicting package settings would undo each other on every run. List the package in only one node task", name)
		}
	}
	for _, entry := range p.Packages {
		a.kept[entry.Name] = isVersionRule(entry.Version)
	}
	return nil
}

// Stdin is /dev/null, so dpkg must not prompt: it keeps a locally changed
// conffile, whose contents a declared file task owns. LC_ALL=C keeps apt's
// messages in English, which refresh reads.
var aptEnv = map[string]string{"DEBIAN_FRONTEND": "noninteractive", "LC_ALL": "C"}

// aptLockWaitDefault is how long an apt task waits for another apt process
// to give up apt's locks when the task sets no wait_for_apt_lock_at_most. A
// fresh cloud server runs an unattended upgrade at first boot that holds the
// locks for about ten minutes; a lock held for longer than 15 minutes
// usually belongs to a stuck apt process that a person should look at.
const aptLockWaitDefault = 15 * time.Minute

// aptLockWaitNote describes wait_for_apt_lock_at_most on each apt module's page.
const aptLockWaitNote = "how long the node task waits for another apt process, such as the unattended upgrade that a new cloud server runs at first boot, to give up apt's locks; a duration of whole seconds, such as 90s, 15m, or 1h. The default covers a first-boot upgrade, which takes about ten minutes; a lock held longer usually belongs to a stuck apt process that a person should look at. A lock still held after the wait fails the node task without another try, with apt's line that names the process holding the lock"

// aptLockWait reads a task's wait_for_apt_lock_at_most. It is whole seconds
// because apt's DPkg::Lock::Timeout counts seconds.
func aptLockWait(module, text string) (time.Duration, error) {
	if text == "" {
		return aptLockWaitDefault, nil
	}
	d, err := time.ParseDuration(text)
	if err != nil || d < time.Second || d%time.Second != 0 {
		return 0, invalid("wait_for_apt_lock_at_most", module+".wait_for_apt_lock_at_most must be a duration of whole seconds, at least 1s, such as 90s, 15m, or 1h")
	}
	return d, nil
}

// aptLockPause is the pause between two tries of an apt-get command that
// found a lock held.
var aptLockPause = 2 * time.Second

// aptChange holds dpkg's options for every apt-get command that changes
// packages; aptGetWaitingForLocks puts the lock wait before them.
var aptChange = []string{"-o", "Dpkg::Options::=--force-confdef", "-o", "Dpkg::Options::=--force-confold"}

// refresh updates package indexes once per job, and again after a source change.
func (a *APT) refresh(ctx context.Context, wait time.Duration) protocol.Result {
	sources := a.sourcesFingerprint()
	if a.indexesFresh && sources == a.indexedSources {
		return protocol.Success("unchanged")
	}
	r := aptGetWaitingForLocks(ctx, wait, "update")
	if r.Status == "succeeded" {
		a.indexesFresh, a.indexedSources = true, sources
	}
	return r
}

// aptSourceFiles are the files and directories under /etc/apt/ that decide
// what apt-get update indexes: the sources, their signing keys, and logins.
var aptSourceFiles = []string{"/etc/apt/sources.list", "/etc/apt/sources.list.d", "/etc/apt/trusted.gpg", "/etc/apt/trusted.gpg.d", "/etc/apt/keyrings", "/etc/apt/auth.conf.d"}

// sourcesFingerprint hashes the path and contents of every file in
// aptSourceFiles. It hashes contents, not size and modification time, so a
// file rewritten within one clock tick still counts and a file rewritten
// unchanged does not. A file it cannot read counts by its error, which stays
// the same while the file stays unreadable; apt-get update reports that file.
func (a *APT) sourcesFingerprint() string {
	h := sha256.New()
	for _, top := range aptSourceFiles {
		// The walk function records every error and returns nil, so WalkDir does too.
		_ = filepath.WalkDir(filepath.Join(a.root(), top), func(path string, entry fs.DirEntry, err error) error {
			if errors.Is(err, fs.ErrNotExist) || err == nil && entry.IsDir() {
				return nil
			}
			fmt.Fprintf(h, "%q\x00", path)
			if err == nil {
				var contents []byte
				contents, err = os.ReadFile(path)
				fmt.Fprintf(h, "%d\x00", len(contents))
				h.Write(contents)
			}
			if err != nil {
				fmt.Fprintf(h, "error %v\x00", err)
			}
			return nil
		})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// aptCommand runs an apt program. A node that stopped inside dpkg makes every
// apt command fail until a person runs dpkg --configure -a on it: another try
// changes nothing, so the failure is a missing prerequisite, which MMX does
// not retry, and says what to run first.
func aptCommand(ctx context.Context, program string, args ...string) protocol.Result {
	r := Command(ctx, aptEnv, "", program, args...)
	if r.Status == "failed" && strings.Contains(r.Stderr, "dpkg was interrupted") {
		r.FailureKind = "missing_prerequisite"
		r.Reason = "dpkg on the node stopped in the middle of a package change. Run dpkg --configure -a on the node first, then run MMX again: " + r.Reason
	}
	return r
}

// aptGetWaitingForLocks runs apt-get with args, waiting up to wait for
// another apt process to give up apt's locks. DPkg::Lock::Timeout makes
// apt-get itself wait for dpkg's lock, but apt-get fails at once while
// another apt holds the lock of the package lists or of the downloaded
// package files, as apt's daily timers and a first-boot unattended upgrade
// do; so apt-get runs again until wait has passed. A lock still held then
// fails the task without another try: the task has waited as long as it
// was told to.
func aptGetWaitingForLocks(ctx context.Context, wait time.Duration, args ...string) protocol.Result {
	args = append([]string{"-o", fmt.Sprintf("DPkg::Lock::Timeout=%d", int64(wait/time.Second))}, args...)
	for deadline := time.Now().Add(wait); ; {
		r := aptCommand(ctx, "apt-get", args...)
		if r.Status == "succeeded" {
			return r
		}
		r.Change = "unknown"
		held := aptLockMessage(r.Stderr)
		if held == "" {
			return r
		}
		if time.Now().After(deadline) {
			r.NoOtherTry = true
			r.Reason = fmt.Sprintf("another apt process still holds apt's lock after %s: %s. Run MMX again after that process ends, or give the node task a longer wait_for_apt_lock_at_most", wait, held)
			return r
		}
		select {
		case <-ctx.Done():
			return r
		case <-time.After(aptLockPause):
		}
	}
}

// aptLockMessage is apt's line about a lock that another process holds,
// such as "Could not get lock /var/lib/apt/lists/lock. It is held by process
// 1816 (python3)", or "" when stderr holds none.
func aptLockMessage(stderr string) string {
	for _, line := range strings.Split(stderr, "\n") {
		if _, message, found := strings.Cut(line, "Could not get lock"); found {
			return "Could not get lock" + strings.TrimRight(message, ". ")
		}
	}
	return ""
}

// change runs one apt command that changes packages, with MMX's policy program
// in place unless the task lets the package start its services; apt-get waits
// for apt's locks. A failed command leaves the change unknown. Cleanup failure
// preserves that change information and fails the task.
func (a *APT) change(ctx context.Context, run Runtime, p Package, program string, args ...string) protocol.Result {
	note := ""
	if !p.AllowPackageToStartItsServices {
		var err error
		if note, err = placeServicePolicy(ctx, run, a.root()); err != nil {
			return protocol.Failed(err)
		}
	}
	var r protocol.Result
	if program == "apt-get" {
		r = aptGetWaitingForLocks(ctx, p.aptLockWait(), args...)
	} else {
		r = aptCommand(ctx, program, args...)
	}
	if r.Status != "succeeded" {
		r.Change = "unknown"
	}
	r.Reason = strings.TrimSpace(strings.Join([]string{r.Reason, note}, " "))
	if !p.AllowPackageToStartItsServices {
		if err := removeServicePolicy(ctx, run, a.root()); err != nil {
			if r.Status == "succeeded" {
				r.Change = "changed"
			}
			r.Status = "failed"
			// A retry could find the package installed and skip policy cleanup.
			r.NoOtherTry = true
			policy := filepath.Join(a.root(), policyRCD)
			r.Reason = strings.TrimSpace(r.Reason + "\n" + fmt.Sprintf("MMX could not remove its service policy program %s: %v. The policy program is still in place and can prevent later package installations from starting services. Remove MMX's policy program before installing more packages.", policy, err))
		}
	}
	return r
}

// installPackages plans every entry before writing any version rule, then
// installs all entries with one apt command so dependency order is irrelevant.
func (a *APT) installPackages(ctx context.Context, run Runtime, p Package) protocol.Result {
	type plannedPackage struct {
		entry           PackageEntry
		version, target string
	}
	var planned []plannedPackage
	var changes []any
	var configure []string
	// A file needs a temporary public copy for apt's unprivileged reader.
	directory := ""
	defer func() {
		if directory != "" {
			os.RemoveAll(directory)
		}
	}()
	downgrade := false
	for _, entry := range p.Packages {
		current, err := inspectDPKG(ctx, entry.Name)
		if err != nil {
			return protocol.Failed(err)
		}
		plan := plannedPackage{entry: entry, target: entry.Name}
		switch {
		case entry.HasPackageFile():
			if entry.PackageFile == nil {
				return protocol.Failed(fmt.Errorf("apt_package: %s package file has no saved asset", entry.Name))
			}
			if directory == "" {
				directory, err = os.MkdirTemp("", "mmx-package-file-")
				if err != nil {
					return protocol.Failed(err)
				}
				if err = os.Chmod(directory, 0755); err != nil {
					return protocol.Failed(err)
				}
			}
			plan.target = filepath.Join(directory, fmt.Sprintf("package-%d.deb", len(planned)))
			if err = copyAsset(entry.PackageFile, plan.target); err != nil {
				return protocol.Failed(err)
			}
			read := Command(ctx, nil, "", "dpkg-deb", "--showformat=${Package}\t${Version}", "--show", plan.target)
			name, version, ok := strings.Cut(strings.TrimSpace(read.Stdout), "\t")
			if read.Status != "succeeded" || !ok {
				return protocol.Failed(fmt.Errorf("apt_package: read the package file: %s %s", read.Reason, read.Stderr))
			}
			if name != entry.Name {
				return protocol.Failed(fmt.Errorf("apt_package: the package file holds the package %s, not %s", name, entry.Name))
			}
			if version != entry.Version {
				return protocol.Failed(fmt.Errorf("apt_package: the package file for %s holds version %s, not the entry's version %s", name, version, entry.Version))
			}
			plan.version = version
		case isVersionRule(entry.Version):
			if current.State == "installed" && versionMatches(entry.Version, current.Version) {
				plan.version = current.Version
				break
			}
			if !run.Dryrun {
				if r := a.refresh(ctx, p.aptLockWait()); r.Status != "succeeded" {
					return r
				}
			}
			available, err := availableVersions(ctx, entry.Name)
			if err != nil {
				return protocol.Failed(err)
			}
			for _, version := range available {
				if !versionMatches(entry.Version, version) {
					continue
				}
				newer, err := debianVersionNewer(ctx, version, plan.version)
				if err != nil {
					return protocol.Failed(err)
				}
				if newer {
					plan.version = version
				}
			}
			if plan.version == "" {
				return run.notYet(fmt.Errorf("apt_package: no version of %s in the node's package sources matches %s; available: %s", entry.Name, entry.Version, listOrNone(available)))
			}
		case entry.Version == newestWhenFirstInstalled:
			if !packageAbsent(current.State) {
				plan.version = current.Version
				if current.State != "installed" {
					configure = append(configure, entry.Name)
				}
			}
		case entry.Version == newestOnEveryRun:
			if !run.Dryrun {
				if r := a.refresh(ctx, p.aptLockWait()); r.Status != "succeeded" {
					return r
				}
			}
		}
		if plan.version != "" && !packageAbsent(current.State) && current.Version != "" && plan.version != current.Version {
			older, err := debianVersionNewer(ctx, current.Version, plan.version)
			if err != nil {
				return protocol.Failed(err)
			}
			if older && !entry.AllowDowngrade {
				return protocol.Failed(fmt.Errorf("apt_package: %s %s is installed, which is newer than %s; to install the older version, add allow_downgrade: true to its packages entry", entry.Name, current.Version, plan.version))
			}
			downgrade = downgrade || older
		}
		if !entry.HasPackageFile() && plan.version != "" {
			plan.target = entry.Name + "=" + plan.version
		}
		if current.State != "installed" || plan.version != "" && current.Version != plan.version {
			from := current.Version
			if packageAbsent(current.State) {
				from = ""
			}
			change := packageChange(entry.Name, from, plan.version)
			if plan.version == "" {
				change["to"] = ""
			}
			changes = append(changes, change)
			if !packageAbsent(current.State) && plan.version == "" {
				configure = append(configure, entry.Name)
			}
		}
		planned = append(planned, plan)
	}

	if !run.Dryrun && len(changes) > len(configure) {
		if r := a.refresh(ctx, p.aptLockWait()); r.Status != "succeeded" {
			return r
		}
	}

	needsTransaction := len(changes) > 0
	for _, entry := range p.Packages {
		needsTransaction = needsTransaction || entry.Version == newestOnEveryRun
	}
	if needsTransaction {
		if directory == "" {
			var err error
			directory, err = os.MkdirTemp("", "mmx-apt-preview-")
			if err != nil {
				return protocol.Failed(err)
			}
		}
		options, err := a.previewPreferences(p, directory)
		if err != nil {
			return protocol.Failed(err)
		}
		args := append([]string{"-s"}, options...)
		args = append(args, "install")
		if downgrade {
			args = append(args, "--allow-downgrades")
		}
		args = append(args, "--")
		for _, plan := range planned {
			args = append(args, plan.target)
		}
		preview := aptCommand(ctx, "apt-get", args...)
		if preview.Status != "succeeded" {
			output := strings.TrimSpace(preview.Stdout + preview.Stderr)
			if run.Dryrun && packageMissingFromCurrentIndexes(output) {
				return run.notYet(fmt.Errorf("apt_package: this dryrun cannot find a requested package in the current package indexes. An earlier node task may only preview its package source: %s", output))
			}
			return preview
		}
		native := simulatedChanges(preview.Stdout)
		for i := range planned {
			if planned[i].version != "" {
				continue
			}
			for _, item := range native {
				change := item.(map[string]any)
				if change["name"] == planned[i].entry.Name {
					planned[i].version, _ = change["to"].(string)
					if planned[i].version != "" {
						planned[i].target = planned[i].entry.Name + "=" + planned[i].version
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
			written = run.File(ctx, a.versionRule(plan.entry.Name, plan.entry.Version), nil)
		} else {
			written = a.dropVersionRules(ctx, run, []string{plan.entry.Name})
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
	if len(configure) > 0 {
		r := a.change(ctx, run, p, "dpkg", append([]string{"--force-confdef", "--force-confold", "--configure", "--"}, configure...)...)
		if r.Status != "succeeded" {
			r.Change = protocol.CombineChange(result.Change, r.Change)
			return r
		}
	}
	if len(configure) == len(planned) {
		result.Change = "changed"
		return result
	}
	if r := a.refresh(ctx, p.aptLockWait()); r.Status != "succeeded" {
		r.Change = protocol.CombineChange(result.Change, r.Change)
		return r
	}
	args := append(slicesClone(aptChange), "install", "-y")
	if downgrade {
		args = append(args, "--allow-downgrades")
	}
	args = append(args, "--")
	for _, plan := range planned {
		args = append(args, plan.target)
	}
	r := a.change(ctx, run, p, "apt-get", args...)
	if r.Status != "succeeded" {
		r.Change = protocol.CombineChange(result.Change, r.Change)
		return r
	}
	for _, plan := range planned {
		if err := expectInstalled(ctx, plan.entry.Name, plan.version); err != nil {
			return changedFailure(err)
		}
	}
	result.Change, result.Reason = "changed", r.Reason
	return result
}

func slicesClone(s []string) []string { return append([]string(nil), s...) }

func changedFailure(err error) protocol.Result {
	r := protocol.Failed(err)
	r.Change = "changed"
	return r
}

// expectInstalled checks completion independently of apt's exit status.
func expectInstalled(ctx context.Context, name, version string) error {
	installed, err := inspectDPKG(ctx, name)
	if err != nil {
		return err
	}
	if installed.State != "installed" || (version != "" && installed.Version != version) {
		return fmt.Errorf("package %s did not reach the requested installed state (version=%s, state=%s)", name, installed.Version, installed.State)
	}
	return nil
}

func packageChange(name, from, to string) map[string]any {
	change := map[string]any{"name": name}
	if from != "" {
		change["from"] = from
	}
	if to != "" {
		change["to"] = to
	}
	return change
}

func listOrNone(versions []string) string {
	if len(versions) == 0 {
		return "none"
	}
	return strings.Join(versions, ", ")
}

// versionMatches compares a version with a pattern in which * stands for any
// text; everything else is compared as written.
func versionMatches(pattern, version string) bool {
	parts := strings.Split(pattern, "*")
	for i := range parts {
		parts[i] = regexp.QuoteMeta(parts[i])
	}
	return regexp.MustCompile("^" + strings.Join(parts, ".*") + "$").MatchString(version)
}

// debianVersionNewer is dpkg's answer whether a is newer than b; an empty b
// is older than every version.
func debianVersionNewer(ctx context.Context, a, b string) (bool, error) {
	if b == "" {
		return true, nil
	}
	r := Command(ctx, nil, "", "dpkg", "--compare-versions", a, "gt", b)
	switch r.ExitCode {
	case 0:
		return r.Status == "succeeded", nil
	case 1:
		return false, nil
	}
	return false, fmt.Errorf("compare versions %s and %s: %s: %s", a, b, r.Reason, r.Stderr)
}

// availableVersions are the versions of a package that the node's package
// sources offer, as apt-cache madison lists them.
func availableVersions(ctx context.Context, name string) ([]string, error) {
	r := Command(ctx, nil, "", "apt-cache", "madison", "--", name)
	if r.Status != "succeeded" {
		return nil, fmt.Errorf("list versions of %s: %s: %s", name, r.Reason, r.Stderr)
	}
	var versions []string
	for _, line := range strings.Split(r.Stdout, "\n") {
		fields := strings.Split(line, "|")
		if len(fields) < 2 || strings.TrimSpace(fields[0]) != name {
			continue
		}
		if v := strings.TrimSpace(fields[1]); v != "" && !containsString(versions, v) {
			versions = append(versions, v)
		}
	}
	return versions, nil
}

func containsString(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// aptPreferences is where MMX writes its version rules.
const aptPreferences = "/etc/apt/preferences.d"

func (a *APT) root() string {
	if a.Root == "" {
		return "/"
	}
	return a.Root
}

// versionRulePath names a package's rule file. apt reads a preferences file
// only when its name has letters, digits, -, _, and ., so each other character
// is written as _ and its two hexadecimal digits.
func (a *APT) versionRulePath(name string) string {
	var b strings.Builder
	for _, c := range []byte(name) {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '.' {
			b.WriteByte(c)
		} else {
			fmt.Fprintf(&b, "_%02x", c)
		}
	}
	return filepath.Join(a.root(), aptPreferences, "mmx-"+b.String()+".pref")
}

// versionRule is the preferences file that keeps a package inside a pattern:
// apt prefers the matching versions and never installs any other.
func (a *APT) versionRule(name, pattern string) File {
	text := fmt.Sprintf("Explanation: Written by MMX for an apt_package task with a version.\nPackage: %s\nPin: version %s\nPin-Priority: 999\n\n"+
		"Explanation: Versions outside the pattern are never installed.\nPackage: %s\nPin: version *\nPin-Priority: -1\n", name, pattern, name)
	return File{Path: a.versionRulePath(name), Literal: &text, Mode: "0644"}
}

// dropVersionRules deletes the rule files MMX wrote for packages a task names
// without a version; the result is unchanged when there are none.
func (a *APT) dropVersionRules(ctx context.Context, run Runtime, names []string) protocol.Result {
	result := protocol.Success("unchanged")
	for _, name := range names {
		path := a.versionRulePath(name)
		if _, err := os.Lstat(path); os.IsNotExist(err) {
			continue
		}
		r := run.File(ctx, File{Path: path, State: "absent"}, nil)
		r.Change = protocol.CombineChange(result.Change, r.Change)
		if r.Status != "succeeded" {
			return r
		}
		result = r
	}
	return result
}

// previewPreferences copies administrator preferences and other tasks' rules,
// then applies only this task's proposed rule changes in a temporary directory.
// apt therefore chooses candidates without the old MMX pins, while keeping
// every administrator pin. No node preference changes before simulation passes.
func (a *APT) previewPreferences(p Package, directory string) ([]string, error) {
	parts := filepath.Join(directory, "preferences.d")
	if err := os.Mkdir(parts, 0755); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(filepath.Join(a.root(), aptPreferences))
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	replaced := map[string]bool{}
	for _, entry := range p.Packages {
		replaced[filepath.Base(a.versionRulePath(entry.Name))] = true
	}
	for _, entry := range entries {
		if entry.IsDir() || replaced[entry.Name()] {
			continue
		}
		data, err := os.ReadFile(filepath.Join(a.root(), aptPreferences, entry.Name()))
		if err != nil {
			return nil, err
		}
		if err := os.WriteFile(filepath.Join(parts, entry.Name()), data, 0644); err != nil {
			return nil, err
		}
	}
	for _, entry := range p.Packages {
		if !isVersionRule(entry.Version) {
			continue
		}
		rule := a.versionRule(entry.Name, entry.Version)
		if err := os.WriteFile(filepath.Join(parts, filepath.Base(rule.Path)), []byte(*rule.Literal), 0644); err != nil {
			return nil, err
		}
	}
	return []string{"-o", "Dir::Etc::preferences=" + filepath.Join(a.root(), "/etc/apt/preferences"), "-o", "Dir::Etc::preferencesparts=" + parts}, nil
}

// upgradeAll is apt's full-upgrade of the whole node, then, when the task asks,
// autoremove. Dryrun simulates against the current indexes; apply refreshes
// them first. Its value lists each package the simulations upgrade, install,
// or remove, with from and to versions.
func (a *APT) upgradeAll(ctx context.Context, run Runtime, p Package) protocol.Result {
	if !run.Dryrun {
		if r := a.refresh(ctx, p.aptLockWait()); r.Status != "succeeded" {
			return r
		}
	}
	simulate := func(command string) ([]any, protocol.Result) {
		plan := aptCommand(ctx, "apt-get", "-s", command)
		if plan.Status != "succeeded" {
			plan.Change = "unknown"
			return nil, plan
		}
		return simulatedChanges(plan.Stdout), protocol.Success("unchanged")
	}
	upgrades, plan := simulate("full-upgrade")
	var removals []any
	if plan.Status == "succeeded" && p.AlsoRemovePackagesNoLongerNeeded {
		removals, plan = simulate("autoremove")
	}
	if plan.Status != "succeeded" || len(upgrades)+len(removals) == 0 {
		return plan
	}
	result := protocol.Success("would_change")
	result.Value = append(upgrades[:len(upgrades):len(upgrades)], removals...)
	if run.Dryrun {
		return result
	}
	if err := run.Mutation("packages"); err != nil {
		return protocol.Failed(err)
	}
	r := a.change(ctx, run, p, "apt-get", append(slicesClone(aptChange), "full-upgrade", "-y")...)
	if r.Status != "succeeded" {
		return r
	}
	result.Reason = r.Reason
	if p.AlsoRemovePackagesNoLongerNeeded {
		// What is no longer needed is known only after the upgrade.
		removed, plan := simulate("autoremove")
		if plan.Status != "succeeded" {
			plan.Change = "changed"
			return plan
		}
		result.Value = append(upgrades[:len(upgrades):len(upgrades)], removed...)
		if r := a.change(ctx, run, p, "apt-get", append(slicesClone(aptChange), "autoremove", "-y")...); r.Status != "succeeded" {
			r.Change = "changed"
			return r
		}
	}
	result.Change = "changed"
	return result
}

// simulated matches apt-get -s lines: "Inst name [old] (new ...)", "Inst name
// (new ...)" for a new package, and "Remv name [old]" or "Purg name [old]".
var simulated = regexp.MustCompile(`^(Inst|Remv|Purg) (\S+)(?: \[([^]]*)\])?(?: \((\S+))?`)

func simulatedChanges(out string) []any {
	var changes []any
	for _, line := range strings.Split(out, "\n") {
		m := simulated.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		to := ""
		if m[1] == "Inst" {
			to = m[4]
		}
		changes = append(changes, packageChange(m[2], m[3], to))
	}
	return changes
}

// remove removes the named packages with apt, purging their configuration
// files and removing the packages only they needed when the task asks, then
// deletes MMX's version rule for each.
func (a *APT) remove(ctx context.Context, run Runtime, p Package) protocol.Result {
	gone := func(state string) bool {
		if p.AlsoDeleteItsConfigFiles {
			return state == "absent" || state == "not-installed"
		}
		return packageAbsent(state)
	}
	var remove []string
	for _, name := range p.TrackedNames() {
		current, err := inspectDPKG(ctx, name)
		if err != nil {
			return protocol.Failed(err)
		}
		if !gone(current.State) {
			remove = append(remove, name)
		}
	}
	if len(remove) == 0 {
		return a.dropVersionRules(ctx, run, p.TrackedNames())
	}
	args := []string{"remove", "-y"}
	if p.AlsoDeleteItsConfigFiles {
		args = append(args, "--purge")
	}
	if p.AlsoRemovePackagesNoLongerNeeded {
		args = append(args, "--autoremove")
	}
	args = append(args, "--")
	for _, name := range remove {
		// An explicit removal suffix also disambiguates names ending in '+' or '-'.
		args = append(args, name+"-")
	}
	plan := aptCommand(ctx, "apt-get", append([]string{"-s"}, args...)...)
	if plan.Status != "succeeded" {
		plan.Change = "unknown"
		return plan
	}
	result := protocol.Success("would_change")
	result.Value = simulatedChanges(plan.Stdout)
	if err := p.checkRemovalScope(result.Value.([]any)); err != nil {
		return protocol.ConfigurationError(err)
	}
	if run.Dryrun {
		return result
	}
	if err := run.Mutation("packages"); err != nil {
		return protocol.Failed(err)
	}
	r := a.change(ctx, run, p, "apt-get", args...)
	if r.Status != "succeeded" {
		return r
	}
	for _, name := range remove {
		current, err := inspectDPKG(ctx, name)
		if err == nil && !gone(current.State) {
			err = fmt.Errorf("package %s was not removed (state=%s)", name, current.State)
		}
		if err != nil {
			return changedFailure(err)
		}
	}
	if rules := a.dropVersionRules(ctx, run, p.TrackedNames()); rules.Status != "succeeded" {
		rules.Change = "changed"
		return rules
	}
	result.Change, result.Reason = "changed", r.Reason
	return result
}

func copyAsset(ref *assets.Ref, path string) error {
	in, err := assets.Open(ref.Path)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
	if err != nil {
		return err
	}
	if _, err := out.ReadFrom(in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
