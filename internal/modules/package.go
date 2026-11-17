package modules

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"strings"

	"mmx/internal/assets"
	"mmx/internal/protocol"
	"mmx/internal/value"
)

// PackageState is shared by apt_package node tasks and service software hashes. Managers
// normalize native states to absent, installed, or incomplete; absent has no version.
type PackageState struct{ Name, Version, State string }

// Each entry of a packages list in apt_package, dnf_package, gem_package,
// npm_global_package, and pip_package states its version: a rule that the
// package manager keeps the package inside, or one of these two words,
// which name no version.
const (
	// newestWhenFirstInstalled installs the newest version when the package
	// is missing and then leaves the installed version alone.
	newestWhenFirstInstalled = "newest_when_first_installed"
	// newestOnEveryRun installs the newest version when the package is
	// missing and upgrades it to the newest version on every run.
	newestOnEveryRun = "newest_on_every_run"
)

// newestVersionWordsNote ends the note of each package page's version input.
const newestVersionWordsNote = newestWhenFirstInstalled + ": installs the newest version when the package is missing, then never upgrades it; " +
	newestOnEveryRun + ": also upgrades it to the newest version on every run"

// isVersionRule reports whether an entry's version is a rule that keeps the
// package at a version, rather than one of the two newest version words.
func isVersionRule(version string) bool {
	return version != newestWhenFirstInstalled && version != newestOnEveryRun
}

// checkEntryVersion refuses a package entry without a version. field names
// the input, such as packages[1].version; rule says what a version rule is
// in this module, such as `a version pattern such as "5:29.*"`.
func checkEntryVersion(field, version, rule string) error {
	if version != "" {
		return nil
	}
	return invalid(field, fmt.Sprintf("%s is required: %s; or %s to install the newest version once and then leave it alone; or %s to upgrade it on every run",
		field, rule, newestWhenFirstInstalled, newestOnEveryRun))
}

// packageMissingFromCurrentIndexes recognizes native package-not-found
// messages. Both managers use Runtime.notYet for dryrun prerequisites that
// an earlier task may only preview; dependency failures remain refusals.
func packageMissingFromCurrentIndexes(output string) bool {
	return strings.Contains(output, "Unable to locate package ") ||
		strings.Contains(output, "No match for argument:") ||
		strings.Contains(output, "Unable to find a match:")
}

// PackageManager owns native name validation, inspection, reconciliation, and
// package sources. Ensure and Source receive resolved inputs and must honor
// Runtime.Dryrun. Each node job gets one instance, shared with service
// recovery. The engine reports every managed path a task in the job changed
// through PathChanged; a manager whose package indexes read that path, or
// whose Source changed, refreshes them again before its next change. Name is
// the node fact package_manager the manager serves.
type PackageManager interface {
	Name() string
	Inspect(context.Context, string) (PackageState, error)
	Ensure(context.Context, Runtime, Package) protocol.Result
	Source(context.Context, Runtime, Repository, value.Scope) protocol.Result
	PathChanged(string)
}

// NewPackageManager selects the manager for the node's package_manager fact.
func NewPackageManager(name string) PackageManager {
	switch name {
	case "apt":
		return &APT{}
	case "dnf":
		return &DNF{}
	}
	return noPackageManager{name}
}

// noPackageManager serves a node whose package manager MMX has no module for.
type noPackageManager struct{ name string }

func (n noPackageManager) Name() string { return n.name }
func (n noPackageManager) err() error {
	if n.name == "" {
		return fmt.Errorf("MMX does not know this node's package manager; see the node fact package_manager")
	}
	return fmt.Errorf("MMX has no module for this node's package manager, %s", n.name)
}
func (n noPackageManager) Inspect(context.Context, string) (PackageState, error) {
	return PackageState{}, n.err()
}
func (n noPackageManager) Ensure(context.Context, Runtime, Package) protocol.Result {
	return protocol.ConfigurationError(n.err())
}
func (n noPackageManager) Source(context.Context, Runtime, Repository, value.Scope) protocol.Result {
	return protocol.ConfigurationError(n.err())
}
func (n noPackageManager) PathChanged(string) {}

func (r Runtime) Package(ctx context.Context, manager PackageManager, p Package) protocol.Result {
	if err := ValidatePackage(p); err != nil {
		return protocol.ConfigurationError(err)
	}
	if err := packageManagerMismatch(p.Module(), p.Manager, manager); err != nil {
		// The node's package manager does not change between tries.
		return protocol.ConfigurationError(err)
	}
	return manager.Ensure(ctx, r, p)
}

// packageManagerMismatch is the failure of a package module on a node whose
// package manager is another one: apt_package installs with apt and does not
// choose a tool by distribution.
func packageManagerMismatch(module, wanted string, node PackageManager) error {
	if node.Name() == wanted {
		return nil
	}
	if node.Name() == "" {
		return fmt.Errorf("%s installs with %s, but MMX does not know this node's package manager (the node fact package_manager is unavailable)", module, wanted)
	}
	return fmt.Errorf("%s installs with %s, but this node's package manager is %s (the node fact package_manager); give the node task a condition so that it runs only on nodes that use %s", module, wanted, node.Name(), wanted)
}

func (r Runtime) Repository(ctx context.Context, manager PackageManager, repository Repository, s value.Scope) protocol.Result {
	if err := ValidateRepository(repository); err != nil {
		return protocol.ConfigurationError(err)
	}
	if err := packageManagerMismatch(repository.Module(), repository.Manager, manager); err != nil {
		return protocol.ConfigurationError(err)
	}
	return manager.Source(ctx, r, repository, s)
}

// PackageEntry names one package and its version policy. File inputs and the
// frozen asset belong to the entry so one transaction may install several files.
type PackageEntry struct {
	Name                    string                    `yaml:"name" json:"name"`
	Version                 string                    `yaml:"version,omitempty" json:"version,omitempty"`
	AllowDowngrade          bool                      `yaml:"allow_downgrade,omitempty" json:"allow_downgrade,omitempty"`
	PackageFileURL          string                    `yaml:"package_file_url,omitempty" json:"package_file_url,omitempty" mmx:"controller_machine_files"`
	PackageFileSHA256       string                    `yaml:"package_file_sha256,omitempty" json:"package_file_sha256,omitempty" mmx:"controller_machine_files"`
	PackageFileSource       string                    `yaml:"package_file_source,omitempty" json:"package_file_source,omitempty" mmx:"controller_machine_files"`
	DangerouslySkipChecksum bool                      `yaml:"dangerously_skip_checksum,omitempty" json:"dangerously_skip_checksum,omitempty" mmx:"controller_machine_files"`
	PackageFile             *assets.Ref               `yaml:"-" json:"package_file,omitempty"`
	LocalPath               string                    `yaml:"-" json:"-"`
	SourceBase              string                    `yaml:"-" json:"-"`
	Variants                map[string]*SourceVariant `yaml:"-" json:"variants,omitempty"`
}

func (entry PackageEntry) HasPackageFile() bool {
	return entry.PackageFileURL != "" || entry.PackageFileSHA256 != "" || entry.PackageFileSource != "" || entry.DangerouslySkipChecksum
}

// AptPackage is the YAML form; loading turns it into the shared Package.
type AptPackage struct {
	Packages                            []PackageEntry `yaml:"packages,omitempty"`
	State                               string         `yaml:"state,omitempty"`
	UpgradeAll                          bool           `yaml:"upgrade_all_packages_within_their_version_rules,omitempty"`
	AlsoDeleteItsConfigFiles            bool           `yaml:"also_delete_its_config_files,omitempty"`
	AlsoRemovePackagesNoLongerNeeded    bool           `yaml:"also_remove_packages_no_longer_needed,omitempty"`
	AlsoRemovePackagesThatDependOnThese bool           `yaml:"also_remove_packages_that_depend_on_these,omitempty"`
	AllowPackageToStartItsServices      bool           `yaml:"allow_package_to_start_its_services,omitempty"`
}

// DnfPackage is only the YAML form of a dnf_package task.
type DnfPackage struct {
	Packages                            []PackageEntry `yaml:"packages,omitempty"`
	State                               string         `yaml:"state,omitempty"`
	UpgradeAll                          bool           `yaml:"upgrade_all_packages_within_their_version_rules,omitempty"`
	OnlySecurityUpdates                 *bool          `yaml:"only_security_updates,omitempty"`
	AlsoRemovePackagesOnlyItNeeded      bool           `yaml:"also_remove_packages_only_it_needed,omitempty"`
	AlsoRemovePackagesNoLongerNeeded    bool           `yaml:"also_remove_packages_no_longer_needed,omitempty"`
	AlsoRemovePackagesThatDependOnThese bool           `yaml:"also_remove_packages_that_depend_on_these,omitempty"`
	RepositoriesEnabledOnlyForThisTask  []string       `yaml:"repositories_enabled_only_for_this_task,omitempty"`
	EnableDnfModuleStream               string         `yaml:"enable_dnf_module_stream,omitempty"`
}

// Package is the single task representation after loading.
type Package struct {
	Manager                             string         `json:"manager"`
	Packages                            []PackageEntry `json:"packages,omitempty"`
	State                               string         `json:"state,omitempty"`
	UpgradeAll                          bool           `json:"upgrade_all_packages_within_their_version_rules,omitempty"`
	OnlySecurityUpdates                 *bool          `json:"only_security_updates,omitempty"`
	AlsoDeleteItsConfigFiles            bool           `json:"also_delete_its_config_files,omitempty"`
	AlsoRemovePackagesOnlyItNeeded      bool           `json:"also_remove_packages_only_it_needed,omitempty"`
	AlsoRemovePackagesNoLongerNeeded    bool           `json:"also_remove_packages_no_longer_needed,omitempty"`
	AlsoRemovePackagesThatDependOnThese bool           `json:"also_remove_packages_that_depend_on_these,omitempty"`
	AllowPackageToStartItsServices      bool           `json:"allow_package_to_start_its_services,omitempty"`
	RepositoriesEnabledOnlyForThisTask  []string       `json:"repositories_enabled_only_for_this_task,omitempty"`
	EnableDnfModuleStream               string         `json:"enable_dnf_module_stream,omitempty"`
}

func (p Package) Module() string { return p.Manager + "_package" }

func (p Package) TrackedNames() []string {
	var names []string
	for _, entry := range p.Packages {
		names = append(names, entry.Name)
	}
	return names
}

func (a AptPackage) Package() (Package, error) {
	return Package{Manager: "apt", Packages: a.Packages, State: a.State, UpgradeAll: a.UpgradeAll,
		AlsoDeleteItsConfigFiles:            a.AlsoDeleteItsConfigFiles,
		AlsoRemovePackagesNoLongerNeeded:    a.AlsoRemovePackagesNoLongerNeeded,
		AlsoRemovePackagesThatDependOnThese: a.AlsoRemovePackagesThatDependOnThese,
		AllowPackageToStartItsServices:      a.AllowPackageToStartItsServices}, nil
}

func (d DnfPackage) Package() (Package, error) {
	return Package{Manager: "dnf", Packages: d.Packages, State: d.State, UpgradeAll: d.UpgradeAll,
		OnlySecurityUpdates: d.OnlySecurityUpdates, AlsoRemovePackagesOnlyItNeeded: d.AlsoRemovePackagesOnlyItNeeded,
		AlsoRemovePackagesNoLongerNeeded:    d.AlsoRemovePackagesNoLongerNeeded,
		AlsoRemovePackagesThatDependOnThese: d.AlsoRemovePackagesThatDependOnThese,
		RepositoriesEnabledOnlyForThisTask:  d.RepositoriesEnabledOnlyForThisTask,
		EnableDnfModuleStream:               d.EnableDnfModuleStream}, nil
}

// ValidatePackage checks structure on the controller machine and again on the node.
func ValidatePackage(p Package) error {
	m := p.Module()
	if p.Manager != "apt" && p.Manager != "dnf" {
		return fmt.Errorf("unknown package manager %q", p.Manager)
	}
	if err := validatePresence(p.State); err != nil {
		return err
	}
	absent := p.State == "absent"
	for field, set := range map[string]bool{
		"also_delete_its_config_files":            p.Manager != "apt" && p.AlsoDeleteItsConfigFiles,
		"allow_package_to_start_its_services":     p.Manager != "apt" && p.AllowPackageToStartItsServices,
		"repositories_enabled_only_for_this_task": p.Manager != "dnf" && p.RepositoriesEnabledOnlyForThisTask != nil,
		"only_security_updates":                   p.Manager != "dnf" && p.OnlySecurityUpdates != nil,
		"enable_dnf_module_stream":                p.Manager != "dnf" && p.EnableDnfModuleStream != "",
		"also_remove_packages_only_it_needed":     p.Manager != "dnf" && p.AlsoRemovePackagesOnlyItNeeded,
	} {
		if set {
			return invalid(field, m+" has no field "+field)
		}
	}
	if p.UpgradeAll {
		if p.Packages != nil || p.State != "" || p.EnableDnfModuleStream != "" || p.AlsoDeleteItsConfigFiles || p.AlsoRemovePackagesOnlyItNeeded || p.AlsoRemovePackagesThatDependOnThese || p.AllowPackageToStartItsServices {
			return invalid("upgrade_all_packages_within_their_version_rules", m+".upgrade_all_packages_within_their_version_rules takes only also_remove_packages_no_longer_needed, only_security_updates (dnf), and repositories_enabled_only_for_this_task (dnf)")
		}
	} else if len(p.Packages) == 0 {
		return invalid("packages", m+" requires packages, or upgrade_all_packages_within_their_version_rules: true")
	}
	for _, check := range []struct {
		field        string
		set, allowed bool
		requires     string
	}{
		{"only_security_updates", p.OnlySecurityUpdates != nil, p.UpgradeAll, "upgrade_all_packages_within_their_version_rules: true"},
		{"also_delete_its_config_files", p.AlsoDeleteItsConfigFiles, absent, "state: absent"},
		{"also_remove_packages_only_it_needed", p.AlsoRemovePackagesOnlyItNeeded, absent, "state: absent"},
		{"also_remove_packages_that_depend_on_these", p.AlsoRemovePackagesThatDependOnThese, absent, "state: absent"},
		{"also_remove_packages_no_longer_needed", p.AlsoRemovePackagesNoLongerNeeded, absent || p.UpgradeAll, "state: absent or upgrade_all_packages_within_their_version_rules: true"},
		{"allow_package_to_start_its_services", p.AllowPackageToStartItsServices, !absent, "state: present"},
		{"enable_dnf_module_stream", p.EnableDnfModuleStream != "", !absent && !p.UpgradeAll, "state: present with packages"},
		{"repositories_enabled_only_for_this_task", p.RepositoriesEnabledOnlyForThisTask != nil, !absent && len(p.RepositoriesEnabledOnlyForThisTask) > 0, "state: present or upgrade_all_packages_within_their_version_rules: true, and at least one repository ID"},
	} {
		if check.set && !check.allowed {
			return invalid(check.field, m+"."+check.field+" requires "+check.requires)
		}
	}
	// dnf's no-longer-needed option remains a whole-node autoremove only.
	if p.Manager == "dnf" && p.AlsoRemovePackagesNoLongerNeeded && !p.UpgradeAll {
		return invalid("also_remove_packages_no_longer_needed", m+".also_remove_packages_no_longer_needed requires upgrade_all_packages_within_their_version_rules: true; with state: absent use also_remove_packages_only_it_needed")
	}
	seen := map[string]bool{}
	for i, entry := range p.Packages {
		prefix := fmt.Sprintf("packages[%d].", i)
		if entry.Name == "" {
			return invalid(prefix+"name", m+" requires each package's name")
		}
		if seen[entry.Name] && !Dynamic(entry.Name) {
			return invalid(prefix+"name", entry.Name+" is listed twice in packages")
		}
		seen[entry.Name] = true
		if absent {
			if entry.Version != "" || entry.AllowDowngrade || entry.HasPackageFile() {
				return invalid(prefix+"version", m+" with state: absent accepts only name on each package entry")
			}
			continue
		}
		if err := checkEntryVersion(prefix+"version", entry.Version, `a version or a pattern such as "5:29.*"`); err != nil {
			return err
		}
		if entry.AllowDowngrade && !isVersionRule(entry.Version) {
			return invalid(prefix+"allow_downgrade", "allow_downgrade requires a version or a package file on the entry")
		}
		if !entry.HasPackageFile() {
			continue
		}
		if !isVersionRule(entry.Version) || strings.Contains(entry.Version, "*") {
			return invalid(prefix+"version", "a package file entry requires the exact version inside the file")
		}
		if p.EnableDnfModuleStream != "" {
			return invalid("enable_dnf_module_stream", "enable_dnf_module_stream is not allowed with package files")
		}
		if entry.PackageFileSource != "" {
			if entry.PackageFileURL != "" || entry.PackageFileSHA256 != "" || entry.DangerouslySkipChecksum {
				return invalid(prefix+"package_file_source", "use package_file_source or package_file_url with its checksum, not both")
			}
		} else {
			if entry.PackageFileURL == "" {
				return invalid(prefix+"package_file_url", "a checksum requires package_file_url on the entry")
			}
			if err := validatePin(m, prefix+"package_file_url", prefix+"package_file_sha256", entry.PackageFileURL, entry.PackageFileSHA256, entry.DangerouslySkipChecksum); err != nil {
				return err
			}
		}
	}
	return nil
}

// checkRemovalScope refuses collateral removals before any managed write.
func (p Package) checkRemovalScope(changes []any) error {
	if p.AlsoRemovePackagesThatDependOnThese {
		return nil
	}
	var extra []string
	for _, item := range changes {
		change := item.(map[string]any)
		name, _ := change["name"].(string)
		if _, installing := change["to"]; !installing && !slices.Contains(p.TrackedNames(), name) {
			extra = append(extra, name)
		}
	}
	if len(extra) > 0 {
		return fmt.Errorf("%s: removing the named packages would also remove %s. To remove these packages too, add also_remove_packages_that_depend_on_these: true", p.Module(), strings.Join(extra, ", "))
	}
	return nil
}

// mergePackageChanges keeps native dependency changes and replaces predicted
// versions of named entries with the package manager's transaction result.
func mergePackageChanges(planned, native []any) []any {
	names := map[string]bool{}
	for _, item := range native {
		names[item.(map[string]any)["name"].(string)] = true
	}
	var changes []any
	for _, item := range planned {
		if !names[item.(map[string]any)["name"].(string)] {
			changes = append(changes, item)
		}
	}
	return append(changes, native...)
}

// knownInstallationChanges omits an installation whose target version the
// current index could not name. Such an entry still predicts presence through
// ExpectedPackages; a {name, from} result would incorrectly predict removal.
func knownInstallationChanges(changes []any) []any {
	var known []any
	for _, item := range changes {
		change := item.(map[string]any)
		if to, found := change["to"]; !found || to != "" {
			known = append(known, item)
		}
	}
	return known
}

// ModuleOfStream is the module of a module:stream text, or "".
func ModuleOfStream(stream string) string {
	module, _, _ := strings.Cut(stream, ":")
	return module
}

// AptRepository is only the YAML form of an apt_repository task. Loading
// turns it into a Repository, as a package module becomes a Package.
type AptRepository struct {
	Name             string   `yaml:"name"`
	State            string   `yaml:"state,omitempty"`
	URIs             []string `yaml:"uris,omitempty"`
	Suites           []string `yaml:"suites,omitempty"`
	Components       []string `yaml:"components,omitempty"`
	Architectures    []string `yaml:"architectures,omitempty"`
	SigningKeySource string   `yaml:"signing_key_source,omitempty"`
	SigningKeyURL    string   `yaml:"signing_key_url,omitempty"`
	SigningKeySHA256 string   `yaml:"signing_key_sha256,omitempty"`
	// DangerouslySkipChecksum takes the place of SigningKeySHA256 beside SigningKeyURL.
	DangerouslySkipChecksum bool   `yaml:"dangerously_skip_checksum,omitempty"`
	Username                string `yaml:"username,omitempty"`
	Password                string `yaml:"password,omitempty"`
}

// DnfRepository is only the YAML form of a dnf_repository task: one
// repository file and its signing key, or whether a repository already on the
// node is switched on.
type DnfRepository struct {
	Name               string   `yaml:"name,omitempty"`
	ExistingRepository string   `yaml:"existing_repository,omitempty"`
	State              string   `yaml:"state,omitempty"`
	Description        string   `yaml:"description,omitempty"`
	BaseURL            []string `yaml:"baseurl,omitempty"`
	Metalink           string   `yaml:"metalink,omitempty"`
	Mirrorlist         string   `yaml:"mirrorlist,omitempty"`
	Enabled            *bool    `yaml:"enabled,omitempty"`
	Priority           *int     `yaml:"priority,omitempty"`
	IncludePkgs        []string `yaml:"includepkgs,omitempty"`
	Exclude            []string `yaml:"exclude,omitempty"`
	SigningKeySource   string   `yaml:"signing_key_source,omitempty"`
	SigningKeyURL      string   `yaml:"signing_key_url,omitempty"`
	SigningKeySHA256   string   `yaml:"signing_key_sha256,omitempty"`
	// DangerouslySkipChecksum takes the place of SigningKeySHA256 beside SigningKeyURL.
	DangerouslySkipChecksum bool   `yaml:"dangerously_skip_checksum,omitempty"`
	Username                string `yaml:"username,omitempty"`
	Password                string `yaml:"password,omitempty"`
}

// Repository is a repository task as every stage after loading handles it.
// Manager names the module the task was written as: apt for apt_repository,
// dnf for dnf_repository. Name, state, the key, and the login are common;
// uris, suites, components, and architectures are apt's deb822 fields; the
// rest are dnf's own. The key is a local file or a pinned download, frozen as
// an asset like a file's source.
type Repository struct {
	Manager                 string   `json:"manager"`
	Name                    string   `json:"name,omitempty"`
	State                   string   `json:"state,omitempty"`
	URIs                    []string `json:"uris,omitempty"`
	Suites                  []string `json:"suites,omitempty"`
	Components              []string `json:"components,omitempty"`
	Architectures           []string `json:"architectures,omitempty"`
	ExistingRepository      string   `json:"existing_repository,omitempty"`
	Description             string   `json:"description,omitempty"`
	BaseURL                 []string `json:"baseurl,omitempty"`
	Metalink                string   `json:"metalink,omitempty"`
	Mirrorlist              string   `json:"mirrorlist,omitempty"`
	Enabled                 *bool    `json:"enabled,omitempty"`
	Priority                *int     `json:"priority,omitempty"`
	IncludePkgs             []string `json:"includepkgs,omitempty"`
	Exclude                 []string `json:"exclude,omitempty"`
	SigningKeySource        string   `json:"signing_key_source,omitempty"`
	SigningKeyURL           string   `json:"signing_key_url,omitempty"`
	SigningKeySHA256        string   `json:"signing_key_sha256,omitempty"`
	DangerouslySkipChecksum bool     `json:"dangerously_skip_checksum,omitempty"`
	// Username and Password are the login for a private repository; Password
	// is always a secret reference.
	Username  string      `json:"username,omitempty"`
	Password  string      `json:"password,omitempty"`
	Key       *assets.Ref `json:"key,omitempty"`
	LocalPath string      `json:"-"`
	// SourceBase and Variants serve a variable source field (variable_sources.go).
	SourceBase string                    `json:"-"`
	Variants   map[string]*SourceVariant `json:"variants,omitempty"`
}

// Module is the name the task was written under, in results and field locations.
func (r Repository) Module() string { return r.Manager + "_repository" }

func (a AptRepository) Repository() Repository {
	return Repository{Manager: "apt", Name: a.Name, State: a.State, URIs: a.URIs, Suites: a.Suites, Components: a.Components,
		Architectures: a.Architectures, SigningKeySource: a.SigningKeySource, SigningKeyURL: a.SigningKeyURL, SigningKeySHA256: a.SigningKeySHA256,
		DangerouslySkipChecksum: a.DangerouslySkipChecksum, Username: a.Username, Password: a.Password}
}

func (d DnfRepository) Repository() Repository {
	return Repository{Manager: "dnf", Name: d.Name, ExistingRepository: d.ExistingRepository, State: d.State, Description: d.Description,
		BaseURL: d.BaseURL, Metalink: d.Metalink, Mirrorlist: d.Mirrorlist, Enabled: d.Enabled, Priority: d.Priority,
		IncludePkgs: d.IncludePkgs, Exclude: d.Exclude, SigningKeySource: d.SigningKeySource, SigningKeyURL: d.SigningKeyURL, SigningKeySHA256: d.SigningKeySHA256,
		DangerouslySkipChecksum: d.DangerouslySkipChecksum, Username: d.Username, Password: d.Password}
}

// ValidateRepository checks a repository task's field combinations. It runs
// when the project loads and again on the node.
func ValidateRepository(r Repository) error {
	m := r.Module()
	if r.Manager != "apt" && r.Manager != "dnf" {
		return fmt.Errorf("unknown package manager %q", r.Manager)
	}
	if err := validatePresence(r.State); err != nil {
		return err
	}
	keys := 0
	for _, supplied := range []bool{r.SigningKeySource != "", r.SigningKeyURL != "" || r.SigningKeySHA256 != "" || r.DangerouslySkipChecksum} {
		if supplied {
			keys++
		}
	}
	dnfFields := r.Description != "" || r.BaseURL != nil || r.Metalink != "" || r.Mirrorlist != "" || r.Priority != nil || r.IncludePkgs != nil || r.Exclude != nil
	aptFields := r.URIs != nil || r.Suites != nil || r.Components != nil || r.Architectures != nil
	if r.ExistingRepository != "" {
		if r.Name != "" || r.State != "" || dnfFields || keys != 0 || r.Username != "" || r.Password != "" {
			return invalid("existing_repository", m+" with existing_repository accepts only enabled")
		}
		if r.Enabled == nil {
			return invalid("enabled", m+" with existing_repository requires enabled: true or false")
		}
		return nil
	}
	if r.Name == "" {
		return invalid("name", m+" requires name")
	}
	if r.State == "absent" {
		if aptFields || dnfFields || r.Enabled != nil || keys != 0 || r.Username != "" || r.Password != "" {
			return fmt.Errorf("absent %s accepts only name and state", m)
		}
		return nil
	}
	if r.Manager == "apt" && (len(r.URIs) == 0 || len(r.Suites) == 0) {
		return fmt.Errorf("apt_repository requires uris and suites")
	}
	if r.Manager == "dnf" {
		if r.Description == "" {
			return invalid("description", "dnf_repository requires description: the line dnf shows for the repository")
		}
		addresses := 0
		for _, set := range []bool{r.BaseURL != nil, r.Metalink != "", r.Mirrorlist != ""} {
			if set {
				addresses++
			}
		}
		if addresses != 1 || (r.BaseURL != nil && len(r.BaseURL) == 0) {
			return invalid("baseurl", "dnf_repository requires exactly one of baseurl, metalink, or mirrorlist")
		}
		if r.Priority != nil && (*r.Priority < 1 || *r.Priority > 99) {
			return invalid("priority", "dnf_repository.priority must be from 1 to 99")
		}
	}
	if keys != 1 {
		return fmt.Errorf("%s requires one key: signing_key_source, or signing_key_url with signing_key_sha256 or dangerously_skip_checksum: true", m)
	}
	if (r.Username == "") != (r.Password == "") {
		return invalid("username", m+".username and "+m+".password come together")
	}
	if r.Password != "" && !WholeSecretReference(r.Password) {
		return invalid("password", m+`.password must be a whole-value secret reference such as "{{ .secrets.vendor_repository_password }}", or, in a role, a reference to an input declared secret: true such as "{{ .inputs.repository_password }}"`)
	}
	if r.SigningKeyURL != "" || r.SigningKeySHA256 != "" || r.DangerouslySkipChecksum {
		return validatePin(m, "signing_key_url", "signing_key_sha256", r.SigningKeyURL, r.SigningKeySHA256, r.DangerouslySkipChecksum)
	}
	return nil
}

// aptPackageDescription is what mmx describe-module shows for apt_package.
var aptPackageDescription = Description{
	Name: "apt_package", Topic: TopicPackagesAndPackageSources, Schema: reflect.TypeOf(AptPackage{}),
	Summary: "apt packages on Debian and Ubuntu: present, absent, kept inside a version pattern, brought to the newest version, or installed from a .deb file; or a whole-node upgrade",
	Fields: []Field{
		{Name: "packages", Required: true, Notes: "one entry per package: required name and version (omit version with state: absent); version is an exact version or a * pattern, or one of the keywords below; a keyword deletes only MMX's rule and leaves administrator rules. Each entry may set allow_downgrade, package_file_source or package_file_url with package_file_sha256 or dangerously_skip_checksum. A file entry gives its exact version, checked against the file. Write entry mappings directly in the task so controller files can be prepared; use references in individual fields. Omit packages with upgrade_all_packages_within_their_version_rules. " + newestVersionWordsNote},
		{Name: "state", Values: presenceStates, Default: "present"},
		{Name: "upgrade_all_packages_within_their_version_rules", Default: "false", Notes: "upgrades the whole node; every package stays inside MMX's and administrator version rules"},
		{Name: "also_remove_packages_no_longer_needed", Default: "false", Notes: "with state: absent: apt --autoremove removes automatically installed packages nothing needs; apt's simulation lists them as packages the task does not name, so the task also needs also_remove_packages_that_depend_on_these: true; with a whole-node upgrade: apt autoremove afterwards"},
		{Name: "also_remove_packages_that_depend_on_these", Default: "false", Notes: "with state: absent: permits the package manager's simulation to remove packages the task does not name"},
		{Name: "also_delete_its_config_files", Default: "false", Notes: "with state: absent: apt purge"},
		{Name: "allow_package_to_start_its_services", Default: "false", Notes: "lets package scripts start stopped services; otherwise they can only restart running services"},
	},
	Example: "- apt_package:\n    packages:\n      - name: nginx\n        version: newest_when_first_installed\n",
	Results: []string{"status", "change", "value"},
	Dryrun:  "supported; inspects installed versions without refreshing indexes; installations, upgrades and removals simulate against the current indexes; reports not_checked when those indexes cannot find a requested package",
}

// dnfPackageDescription is what mmx describe-module shows for dnf_package.
var dnfPackageDescription = Description{
	Name: "dnf_package", Topic: TopicPackagesAndPackageSources, Schema: reflect.TypeOf(DnfPackage{}),
	Summary: "dnf packages on the Red Hat family: present, absent, kept inside a version, brought to the newest version, or installed from an .rpm file; or a whole-node upgrade",
	Fields: []Field{
		{Name: "packages", Required: true, Notes: "one entry per package: required name and version (omit version with state: absent); version is an exact version or a * pattern, or one of the keywords below; a keyword deletes only MMX's rule and leaves administrator rules. Each entry may set allow_downgrade, package_file_source or package_file_url with package_file_sha256 or dangerously_skip_checksum. A file entry gives its exact version, checked against the file. Write entry mappings directly in the task so controller files can be prepared; use references in individual fields. Omit packages with upgrade_all_packages_within_their_version_rules. " + newestVersionWordsNote},
		{Name: "state", Values: presenceStates, Default: "present"},
		{Name: "upgrade_all_packages_within_their_version_rules", Default: "false", Notes: "upgrades the whole node; every package stays inside MMX's and administrator version rules"},
		{Name: "also_remove_packages_no_longer_needed", Default: "false", Notes: "with the whole-node upgrade: dnf autoremove afterwards"},
		{Name: "also_remove_packages_that_depend_on_these", Default: "false", Notes: "with state: absent: permits the package manager's simulation to remove packages the task does not name"},
		{Name: "also_remove_packages_only_it_needed", Default: "false", Notes: "with state: absent: dnf also removes packages installed only because the named ones needed them"},
		{Name: "only_security_updates", Default: "false", Notes: "with the whole-node upgrade only: selects security updates; refuses when the repositories list no advisories"},
		{Name: "repositories_enabled_only_for_this_task", Notes: "repository ids the node defines, switched off, that dnf may use for this task, including the whole-node upgrade"},
		{Name: "enable_dnf_module_stream", Notes: "with present packages and no package files: a module:stream such as postgresql:16; enabled before version checks because dnf hides inactive streams' packages. A refusal afterwards leaves the requested stream enabled, and the next run continues from there. Dryrun enables nothing and reports not_checked until the stream is enabled"},
	},
	Example: "- dnf_package:\n    packages:\n      - name: nginx\n        version: newest_when_first_installed\n",
	Results: []string{"status", "change", "value"},
	Dryrun:  "supported; inspects installed versions without refreshing dnf's index; installations, upgrades and removals use native previews; reports not_checked if a package cannot be found in the current repositories or the requested module stream is not enabled",
}

// aptRepositoryDescription is what mmx describe-module shows for apt_repository.
var aptRepositoryDescription = Description{
	Name: "apt_repository", Topic: TopicPackagesAndPackageSources, Schema: reflect.TypeOf(AptRepository{}),
	Summary: "one apt source file and its signing key",
	Fields: []Field{
		{Name: "name", Required: true, Notes: "writes /etc/apt/sources.list.d/<name>.sources and /etc/apt/keyrings/<name>.asc, or <name>.gpg for a binary key"},
		{Name: "uris", Required: true, Notes: "omit with absent"},
		{Name: "suites", Required: true, Notes: "omit with absent; a suite ending in / is a flat repository"},
		{Name: "components", Notes: "required unless the suite ends in /"},
		{Name: "architectures"},
		{Name: "signing_key_source", Notes: "local signing key, relative to the project root, or to the role's directory inside a role; one of signing_key_source or signing_key_url"},
		{Name: "signing_key_url", Notes: "HTTPS URL of the signing key, fetched and verified on the controller like download; may read vars, role inputs, loop items, and .node.name"},
		{Name: "signing_key_sha256", Notes: "the key's SHA-256 in hex; with signing_key_url, exactly one of signing_key_sha256 or dangerously_skip_checksum"},
		{Name: "dangerously_skip_checksum", Default: "false", Notes: "true in place of signing_key_sha256: the key is trusted as it arrives"},
		{Name: "username", Notes: "with password: the login for a private repository, written to /etc/apt/auth.conf.d/<name>.conf (root only, 0600) with one entry for each of uris; never in the source file"},
		{Name: "password", Notes: "with username; must be a whole-value secret reference such as {{ .secrets.vendor_repository_password }}, or in a role one whole reference to an input declared secret: true"},
		{Name: "state", Values: presenceStates, Default: "present", Notes: "absent removes the source file, then the key, then the login file, and accepts only name and state; packages stay installed"},
	},
	Example: "- apt_repository:\n    name: docker\n    uris: [https://download.docker.com/linux/debian]\n    suites: [\"{{ .node.distribution.codename }}\"]\n    components: [stable]\n    signing_key_url: https://download.docker.com/linux/debian/gpg\n    signing_key_sha256: \"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef\"\n",
	Results: resultBasic,
	Dryrun:  "supported; fetches and verifies a signing_key_url, then previews the key and source file; the next installation or upgrade in the job refreshes indexes",
}

// dnfRepositoryDescription is what mmx describe-module shows for dnf_repository.
var dnfRepositoryDescription = Description{
	Name: "dnf_repository", Topic: TopicPackagesAndPackageSources, Schema: reflect.TypeOf(DnfRepository{}),
	Summary: "one dnf repository file and its signing key, imported into rpm; or whether a repository already on the node is switched on",
	Fields: []Field{
		{Name: "name", Required: true, Notes: "the repository id; writes /etc/yum.repos.d/<name>.repo and /etc/pki/rpm-gpg/RPM-GPG-KEY-<name>; omit with existing_repository"},
		{Name: "existing_repository", Notes: "in place of name: a repository id the node already defines; the task then accepts only enabled, which dnf config-manager sets"},
		{Name: "state", Values: presenceStates, Default: "present", Notes: "absent removes the repository file, then the key file, and accepts only name and state; packages stay installed"},
		{Name: "description", Required: true, Notes: "omit with absent or existing_repository; the line dnf shows for the repository (dnf's name)"},
		{Name: "baseurl", Notes: "exactly one of baseurl, metalink, or mirrorlist; dnf's $releasever and $basearch pass through"},
		{Name: "metalink"},
		{Name: "mirrorlist"},
		{Name: "enabled", Default: "true", Notes: "false: only a dnf_package task that names the id in repositories_enabled_only_for_this_task uses the repository"},
		{Name: "priority", Notes: "1 to 99; dnf's own field"},
		{Name: "includepkgs", Notes: "dnf's own field"},
		{Name: "exclude", Notes: "dnf's own field"},
		{Name: "signing_key_source", Notes: "local signing key, relative to the project root, or to the role's directory inside a role; one of signing_key_source or signing_key_url; MMX imports it into rpm's key store, and the file always says gpgcheck=1"},
		{Name: "signing_key_url", Notes: "HTTPS URL of the signing key, fetched and verified on the controller like download"},
		{Name: "signing_key_sha256", Notes: "the key's SHA-256 in hex; with signing_key_url, exactly one of signing_key_sha256 or dangerously_skip_checksum"},
		{Name: "dangerously_skip_checksum", Default: "false", Notes: "true in place of signing_key_sha256: the key is trusted as it arrives"},
		{Name: "username", Notes: "with password: the login for a private repository, written into the repository file, which is then root-only 0600"},
		{Name: "password", Notes: "with username; must be a whole-value secret reference, or in a role one whole reference to an input declared secret: true"},
	},
	Example: "- dnf_repository:\n    name: docker-ce-stable\n    description: Docker CE Stable\n    baseurl: [\"https://download.docker.com/linux/rhel/$releasever/$basearch/stable\"]\n    signing_key_url: https://download.docker.com/linux/rhel/gpg\n    signing_key_sha256: \"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef\"\n",
	Results: resultBasic,
	Dryrun:  "supported; previews the key and repository file, or reads whether the existing repository is switched on",
}

// Prepare checks the inputs when the project loads and finds a package file
// on the controller machine, relative to base.
func (p *Package) Prepare(base string) error {
	if err := ValidatePackage(*p); err != nil {
		return err
	}
	for i := range p.Packages {
		entry := &p.Packages[i]
		entry.SourceBase = base
		if entry.PackageFileSource != "" && !Dynamic(entry.PackageFileSource) {
			path, err := localSource(base, fmt.Sprintf("packages[%d].package_file_source", i), entry.PackageFileSource)
			if err != nil {
				return err
			}
			entry.LocalPath = path
		}
	}
	return nil
}

// Prepare checks the inputs when the project loads and finds a signing key
// on the controller machine, relative to base.
func (r *Repository) Prepare(base string) error {
	if err := ValidateRepository(*r); err != nil {
		return err
	}
	r.SourceBase = base
	if r.SigningKeySource != "" && !Dynamic(r.SigningKeySource) {
		path, err := localSource(base, "signing_key_source", r.SigningKeySource)
		if err != nil {
			return err
		}
		r.LocalPath = path
	}
	return nil
}

// Arguments lists the inputs that may hold a reference, with their rules.
func (p *Package) Arguments(arguments *ArgumentList) {
	for i := range p.Packages {
		entry := &p.Packages[i]
		arguments.Public(fmt.Sprintf("packages[%d].name", i), &entry.Name, scalar)
		if p.State != "absent" {
			arguments.Public(fmt.Sprintf("packages[%d].version", i), &entry.Version, func(raw any) error {
				version, _ := raw.(string)
				if err := checkEntryVersion("version", version, "a version or pattern"); err != nil {
					return err
				}
				return scalar(raw)
			})
		}
	}
	if p.EnableDnfModuleStream != "" {
		arguments.Public("enable_dnf_module_stream", &p.EnableDnfModuleStream, moduleStream)
	}
	for i := range p.RepositoriesEnabledOnlyForThisTask {
		arguments.Public(fmt.Sprintf("repositories_enabled_only_for_this_task[%d]", i), &p.RepositoriesEnabledOnlyForThisTask[i], repositoryID)
	}
}

// Arguments lists the inputs that may hold a reference, with their rules.
func (r *Repository) Arguments(arguments *ArgumentList) {
	if r.Manager == "apt" {
		arguments.Public("name", &r.Name, repositoryName)
		suite := suiteRule(len(r.Components) == 0)
		for _, list := range []struct {
			field  string
			values []string
			rule   func(any) error
		}{{"uris", r.URIs, repositoryURI}, {"suites", r.Suites, suite}, {"components", r.Components, sourceWord}, {"architectures", r.Architectures, sourceWord}} {
			for i := range list.values {
				arguments.Public(fmt.Sprintf("%s[%d]", list.field, i), &list.values[i], list.rule)
			}
		}
	}
	if r.Manager == "dnf" {
		if r.ExistingRepository != "" {
			arguments.Public("existing_repository", &r.ExistingRepository, repositoryID)
		} else {
			arguments.Public("name", &r.Name, dnfRepositoryName)
		}
		if r.Description != "" {
			arguments.Public("description", &r.Description, dnfLine)
		}
		for _, address := range []struct {
			field string
			value *string
		}{{"metalink", &r.Metalink}, {"mirrorlist", &r.Mirrorlist}} {
			if *address.value != "" {
				arguments.Public(""+address.field, address.value, dnfAddress)
			}
		}
		for _, list := range []struct {
			field  string
			values []string
			rule   func(any) error
		}{{"baseurl", r.BaseURL, dnfAddress}, {"includepkgs", r.IncludePkgs, dnfWord}, {"exclude", r.Exclude, dnfWord}} {
			for i := range list.values {
				arguments.Public(fmt.Sprintf("%s[%d]", list.field, i), &list.values[i], list.rule)
			}
		}
	}
	if r.Username != "" {
		arguments.Private("username", r.Username, scalar)
	}
	if r.Password != "" {
		arguments.WholeSecretReference("password", r.Password, scalar)
	}
}

// WrittenArguments omits only the internal manager selector.
func (p *Package) WrittenArguments() []WrittenArgument { return WrittenArguments(p, "manager") }

// ControllerFiles lists the repository's signing key.
func (r *Repository) ControllerFiles() []ControllerFile {
	s := ControllerFile{Base: r.SourceBase, Variants: &r.Variants, Asset: &r.Key, Local: &r.LocalPath}
	switch {
	case r.SigningKeySource != "":
		s.Field, s.Kind, s.Path = "signing_key_source", SourceFile, &r.SigningKeySource
	case r.SigningKeyURL != "":
		s.Field, s.Kind, s.URL, s.SHA256 = "signing_key_url", SourceDownload, &r.SigningKeyURL, &r.SigningKeySHA256
	default:
		return nil
	}
	return []ControllerFile{s}
}

// ControllerFiles lists the package file the task installs.
func (p *Package) ControllerFiles() []ControllerFile {
	var files []ControllerFile
	for i := range p.Packages {
		entry := &p.Packages[i]
		source := ControllerFile{Base: entry.SourceBase, Variants: &entry.Variants, Asset: &entry.PackageFile, Local: &entry.LocalPath}
		prefix := fmt.Sprintf("packages[%d].", i)
		switch {
		case entry.PackageFileSource != "":
			source.Field, source.Kind, source.Path = prefix+"package_file_source", SourceFile, &entry.PackageFileSource
		case entry.PackageFileURL != "":
			source.Field, source.Kind, source.URL, source.SHA256 = prefix+"package_file_url", SourceDownload, &entry.PackageFileURL, &entry.PackageFileSHA256
		default:
			continue
		}
		files = append(files, source)
	}
	return files
}

// CheckWithinFleetStage checks the task against the earlier package tasks of
// its fleet stage, recorded in record. A package that one task keeps at a
// version and another names without one would be undone on every run, so it
// is refused; only tasks without a condition and with literal names count,
// and the node refuses the rest before it changes anything. Two streams of
// one dnf module are refused: dnf enables one stream of a module.
func (p *Package) CheckWithinFleetStage(record map[string]any, conditional bool) error {
	if !conditional && p.State != "absent" && !p.UpgradeAll {
		for _, entry := range p.Packages {
			name, keeps := entry.Name, isVersionRule(entry.Version)
			if Dynamic(name) || Dynamic(entry.Version) {
				continue
			}
			key := "package kept at a version: " + p.Manager + " " + name
			if earlier, named := record[key]; named && earlier != keeps {
				return fmt.Errorf("%s: %s has a version rule in one task of this fleet stage and a version keyword in another; the two tasks would undo each other on every run. Name the package in one task", p.Module(), name)
			}
			record[key] = keeps
		}
	}
	if stream := p.EnableDnfModuleStream; stream != "" && !Dynamic(stream) {
		key := "dnf module stream: " + ModuleOfStream(stream)
		if earlier, named := record[key]; named && earlier != stream {
			return &FieldError{"enable_dnf_module_stream", fmt.Errorf("this fleet stage enables both %s and %s; dnf enables one stream of a module", earlier, stream)}
		}
		record[key] = stream
	}
	return nil
}

// Run does the module's work on the node.
func (p *Package) Run(ctx context.Context, task NodeTask) protocol.Result {
	return task.Runtime.Package(ctx, task.Packages, *p)
}

// Run does the module's work on the node.
func (r *Repository) Run(ctx context.Context, task NodeTask) protocol.Result {
	return task.Runtime.Repository(ctx, task.Packages, *r, task.Scope)
}

// ModuleInputs turns the written apt_package task into package inputs.
func (a *AptPackage) ModuleInputs() (Inputs, error) {
	p, err := a.Package()
	return &p, err
}

// ModuleInputs turns the written dnf_package task into package inputs.
func (d *DnfPackage) ModuleInputs() (Inputs, error) {
	p, err := d.Package()
	return &p, err
}

// ModuleInputs turns the written apt_repository task into repository inputs.
func (a *AptRepository) ModuleInputs() (Inputs, error) {
	r := a.Repository()
	return &r, nil
}

// ModuleInputs turns the written dnf_repository task into repository inputs.
func (d *DnfRepository) ModuleInputs() (Inputs, error) {
	r := d.Repository()
	return &r, nil
}

// TrackedInputs lists each package by name.
func (p *Package) TrackedInputs() []TrackedInput {
	var out []TrackedInput
	for _, name := range p.TrackedNames() {
		out = append(out, TrackedInput{Kind: "package", PackageName: name})
	}
	return out
}

// ExpectedPackages is what the task asks for each package. A version
// pattern says only that some version will be installed.
func (p *Package) ExpectedPackages() map[string]PackageExpectation {
	out := map[string]PackageExpectation{}
	for _, entry := range p.Packages {
		version := entry.Version
		if !isVersionRule(version) || strings.Contains(version, "*") {
			version = ""
		}
		out[entry.Name] = PackageExpectation{State: p.State, Version: version}
	}
	return out
}

// ExpectedAfter is what a dryrun result lists as {name, from, to}: a package
// without to would be removed.
func (p *Package) ExpectedAfter(r protocol.Result) map[string]PackageExpectation {
	out := map[string]PackageExpectation{}
	list, _ := r.Value.([]any)
	for _, item := range list {
		change, _ := item.(map[string]any)
		name, _ := change["name"].(string)
		if name == "" {
			continue
		}
		if to, _ := change["to"].(string); to != "" {
			out[name] = PackageExpectation{Version: to}
		} else {
			out[name] = PackageExpectation{State: "absent"}
		}
	}
	return out
}
