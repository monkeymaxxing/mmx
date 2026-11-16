package modules

import (
	"context"
	"encoding/json"
	"mmx/internal/protocol"
	"mmx/internal/value"
	"reflect"
	"regexp"
)

// coreModules is the registry of the core modules: one line for each,
// grouped by topic in the order of CoreModuleTopics. A module's input type,
// the checks of its inputs, its description, and its code are in its own
// file.
var coreModules = []CoreModule{
	{Name: "file", Description: &fileDescription, RetryByDefault: true, NewInputs: func() Inputs { return &File{} }},
	{Name: "line", Description: &lineDescription, RetryByDefault: true, NewInputs: func() Inputs { return &Line{} }},
	{Name: "edit_file", Description: &editFileDescription, RetryByDefault: true, NewInputs: func() Inputs { return &EditFile{} }},
	{Name: "symlink", Description: &symlinkDescription, RetryByDefault: true, NewInputs: func() Inputs { return &Symlink{} }},
	{Name: "read_file", Description: &readFileDescription, NewInputs: func() Inputs { return &ReadFile{} }},
	{Name: "directory", Description: &directoryDescription, RetryByDefault: true, NewInputs: func() Inputs { return &Directory{} }},
	{Name: "delete_old_files", Description: &deleteOldFilesDescription, RetryByDefault: true, NewInputs: func() Inputs { return &DeleteOldFiles{} }},
	{Name: "download", Description: &downloadDescription, RetryByDefault: true, NewInputs: func() Inputs { return &File{} }, NewWrittenInputs: func() WrittenInputs { return &Download{} }},
	{Name: "unpack_archive", Description: &unpackArchiveDescription, RetryByDefault: true, NewInputs: func() Inputs { return &Archive{} }},
	{Name: "git", Description: &gitDescription, RetryByDefault: true, NewInputs: func() Inputs { return &Git{} }},
	{Name: "apt_package", Description: &aptPackageDescription, RetryByDefault: true, NewInputs: func() Inputs { return &Package{} }, NewWrittenInputs: func() WrittenInputs { return &AptPackage{} }},
	{Name: "dnf_package", Description: &dnfPackageDescription, RetryByDefault: true, NewInputs: func() Inputs { return &Package{} }, NewWrittenInputs: func() WrittenInputs { return &DnfPackage{} }},
	{Name: "apt_repository", Description: &aptRepositoryDescription, RetryByDefault: true, NewInputs: func() Inputs { return &Repository{} }, NewWrittenInputs: func() WrittenInputs { return &AptRepository{} }},
	{Name: "apt_downloaded_package_files", Description: &aptDownloadedPackageFilesDescription, RetryByDefault: true, NewInputs: func() Inputs { return &AptDownloadedPackageFiles{} }},
	{Name: "dnf_repository", Description: &dnfRepositoryDescription, RetryByDefault: true, NewInputs: func() Inputs { return &Repository{} }, NewWrittenInputs: func() WrittenInputs { return &DnfRepository{} }},
	{Name: "user", Description: &userDescription, RetryByDefault: true, NewInputs: func() Inputs { return &User{} }},
	{Name: "group", Description: &groupDescription, RetryByDefault: true, NewInputs: func() Inputs { return &Group{} }},
	{Name: "systemd_unit", Description: &systemdUnitDescription, RetryByDefault: true, NewInputs: func() Inputs { return &Unit{} }},
	{Name: "systemd_service", Description: &systemdServiceDescription, NewInputs: func() Inputs { return &Service{} }},
	{Name: "systemd_journal_vacuum", Description: &systemdJournalVacuumDescription, RetryByDefault: true, NewInputs: func() Inputs { return &SystemdJournalVacuum{} }},
	{Name: "shell", Description: &shellDescription, ChecksAConfiguration: true, KeepsOutputOfAFailure: true, NewInputs: func() Inputs { return &Shell{} }},
	{Name: "http", Description: &httpDescription, ChecksAConfiguration: true, KeepsOutputOfAFailure: true, NewInputs: func() Inputs { return &HTTP{} }},
	{Name: "wait_for", Description: &waitForDescription, WaitsWithItsOwnLimit: true, OnlyReads: true, KeepsOutputOfAFailure: true, NewInputs: func() Inputs { return &WaitFor{} }},
	{Name: "assert", Description: &assertDescription, EvaluatesOnce: true, OnlyReads: true, NewInputs: func() Inputs { return &Assert{} }},
	{Name: "reboot", Description: &rebootDescription, WaitsWithItsOwnLimit: true, EndsTheNodeJob: true, NewInputs: func() Inputs { return &Reboot{} }},
	{Name: "systemd_scheduled_job", Description: &systemdScheduledJobDescription, RetryByDefault: true, NewInputs: func() Inputs { return &SystemdScheduledJob{} }},
	{Name: "cron_job", Description: &cronJobDescription, RetryByDefault: true, NewInputs: func() Inputs { return &CronJob{} }},
	{Name: "authorized_keys", Description: &authorizedKeysDescription, RetryByDefault: true, NewInputs: func() Inputs { return &AuthorizedKeys{} }},
	{Name: "ssh_key_pair", Description: &sshKeyPairDescription, RetryByDefault: true, NewInputs: func() Inputs { return &SSHKeyPair{} }},
	{Name: "ufw", Description: &ufwDescription, RetryByDefault: true, NewInputs: func() Inputs { return &UFW{} }},
	{Name: "firewalld", Description: &firewalldDescription, RetryByDefault: true, NewInputs: func() Inputs { return &Firewalld{} }},
	{Name: "firewalld_zone_membership", Description: &firewalldZoneMembershipDescription, WaitsWithItsOwnLimit: true, NewInputs: func() Inputs { return &FirewalldZoneMembership{} }, NewChangeActivation: newFirewalldMembershipActivation},
	{Name: "firewalld_zone", Description: &firewalldZoneDescription, RetryByDefault: true, NewInputs: func() Inputs { return &FirewalldZone{} }},
	{Name: "ifupdown_interfaces_file", Description: &ifupdownInterfacesFileDescription, WaitsWithItsOwnLimit: true, NewInputs: func() Inputs { return &IfupdownInterfacesFile{} }, NewChangeActivation: newIfupdownActivation},
	{Name: "netplan_file", Description: &netplanFileDescription, WaitsWithItsOwnLimit: true, NewInputs: func() Inputs { return &NetplanFile{} }, NewChangeActivation: newNetplanActivation},
	{Name: "networkmanager_connection_file", Description: &networkManagerConnectionFileDescription, WaitsWithItsOwnLimit: true, NewInputs: func() Inputs { return &NetworkManagerConnectionFile{} }, NewChangeActivation: newNetworkManagerActivation},
	{Name: "systemd_networkd_file", Description: &systemdNetworkdFileDescription, WaitsWithItsOwnLimit: true, NewInputs: func() Inputs { return &SystemdNetworkdFile{} }, NewChangeActivation: newNetworkdActivation},
	{Name: "wireguard_interface", Description: &wireguardInterfaceDescription, WaitsWithItsOwnLimit: true, NewInputs: func() Inputs { return &WireguardInterface{} }, NewChangeActivation: newWireguardActivation},
	{Name: "selinux_boolean", Description: &selinuxBooleanDescription, RetryByDefault: true, NewInputs: func() Inputs { return &SELinuxBoolean{} }},
	{Name: "selinux_file_context", Description: &selinuxFileContextDescription, RetryByDefault: true, NewInputs: func() Inputs { return &SELinuxFileContext{} }},
	{Name: "selinux_port", Description: &selinuxPortDescription, RetryByDefault: true, NewInputs: func() Inputs { return &SELinuxPort{} }},
	{Name: "selinux_mode", Description: &selinuxModeDescription, RetryByDefault: true, NewInputs: func() Inputs { return &SELinuxMode{} }},
	{Name: "file_access_control_list", Description: &fileAccessControlListDescription, RetryByDefault: true, NewInputs: func() Inputs { return &FileAccessControlList{} }},
	{Name: "linux_capability_on_program_file", Description: &linuxCapabilityOnProgramFileDescription, RetryByDefault: true, NewInputs: func() Inputs { return &LinuxCapabilityOnProgramFile{} }},
	{Name: "hostname", Description: &hostnameDescription, RetryByDefault: true, NewInputs: func() Inputs { return &Hostname{} }},
	{Name: "time_zone", Description: &timeZoneDescription, RetryByDefault: true, NewInputs: func() Inputs { return &TimeZone{} }},
	{Name: "locale", Description: &localeDescription, RetryByDefault: true, NewInputs: func() Inputs { return &Locale{} }},
	{Name: "update_alternatives", Description: &updateAlternativesDescription, RetryByDefault: true, NewInputs: func() Inputs { return &UpdateAlternatives{} }},
	{Name: "sysctl_setting", Description: &sysctlSettingDescription, RetryByDefault: true, NewInputs: func() Inputs { return &SysctlSetting{} }},
	{Name: "kernel_module", Description: &kernelModuleDescription, RetryByDefault: true, NewInputs: func() Inputs { return &KernelModule{} }},
	{Name: "swap_file", Description: &swapFileDescription, RetryByDefault: true, NewInputs: func() Inputs { return &SwapFile{} }},
	{Name: "create_filesystem_on_empty_device", Description: &createFilesystemOnEmptyDeviceDescription, RetryByDefault: true, NewInputs: func() Inputs { return &CreateFilesystemOnEmptyDevice{} }},
	{Name: "fstab_mount", Description: &fstabMountDescription, RetryByDefault: true, NewInputs: func() Inputs { return &FstabMount{} }},
	{Name: "pip_package", Description: &pipPackageDescription, RetryByDefault: true, NewInputs: func() Inputs { return &PipPackage{} }},
	{Name: "gem_package", Description: &gemPackageDescription, RetryByDefault: true, NewInputs: func() Inputs { return &GemPackage{} }},
	{Name: "npm_global_package", Description: &npmGlobalPackageDescription, RetryByDefault: true, NewInputs: func() Inputs { return &NpmGlobalPackage{} }},
	{Name: "snap_package", Description: &snapPackageDescription, RetryByDefault: true, NewInputs: func() Inputs { return &SnapPackage{} }},
	{Name: "npm_project_dependencies", Description: &npmProjectDependenciesDescription, RetryByDefault: true, NewInputs: func() Inputs { return &NpmProjectDependencies{} }},
	{Name: "yarn_project_dependencies", Description: &yarnProjectDependenciesDescription, RetryByDefault: true, NewInputs: func() Inputs { return &YarnProjectDependencies{} }},
	{Name: "bundler_project_dependencies", Description: &bundlerProjectDependenciesDescription, RetryByDefault: true, NewInputs: func() Inputs { return &BundlerProjectDependencies{} }},
	{Name: "composer_project_dependencies", Description: &composerProjectDependenciesDescription, RetryByDefault: true, NewInputs: func() Inputs { return &ComposerProjectDependencies{} }},
	{Name: "uv_project_dependencies", Description: &uvProjectDependenciesDescription, RetryByDefault: true, NewInputs: func() Inputs { return &UvProjectDependencies{} }},
	{Name: "docker_image", Description: &dockerImageDescription, RetryByDefault: true, NewInputs: func() Inputs { return &DockerImage{} }},
	{Name: "docker_compose_project", Description: &dockerComposeProjectDescription, RetryByDefault: true, NewInputs: func() Inputs { return &DockerComposeProject{} }},
	{Name: "docker_unused_images", Description: &dockerUnusedImagesDescription, RetryByDefault: true, NewInputs: func() Inputs { return &DockerUnusedImages{} }},
	{Name: "podman_image", Description: &podmanImageDescription, RetryByDefault: true, NewInputs: func() Inputs { return &PodmanImage{} }},
	{Name: "podman_quadlet", Description: &podmanQuadletDescription, RetryByDefault: true, NewInputs: func() Inputs { return &PodmanQuadlet{} }},
	{Name: "podman_unused_images", Description: &podmanUnusedImagesDescription, RetryByDefault: true, NewInputs: func() Inputs { return &PodmanUnusedImages{} }},
	{Name: "postgresql_role", Description: &postgresqlRoleDescription, RetryByDefault: true, NewInputs: func() Inputs { return &PostgresqlRole{} }},
	{Name: "postgresql_database", Description: &postgresqlDatabaseDescription, RetryByDefault: true, NewInputs: func() Inputs { return &PostgresqlDatabase{} }},
	{Name: "mysql_user", Description: &mysqlUserDescription, RetryByDefault: true, NewInputs: func() Inputs { return &MySQLUser{} }},
	{Name: "mysql_database", Description: &mysqlDatabaseDescription, RetryByDefault: true, NewInputs: func() Inputs { return &MySQLDatabase{} }},
	{Name: "certbot_certificate", Description: &certbotCertificateDescription, NewInputs: func() Inputs { return &CertbotCertificate{} }},
}

// The topics that group the core modules, each named for what its modules
// manage.
const (
	TopicFilesAndLinks                             = "Files and links"
	TopicDirectoriesDownloadsAndReleaseDirectories = "Directories, downloads, and release directories"
	TopicPackagesAndPackageSources                 = "Packages and package sources"
	TopicAccountsAndServiceUnits                   = "Accounts and service units"
	TopicCommandsChecksAndReboot                   = "Commands, checks, and reboot"
	TopicScheduledJobs                             = "Scheduled jobs"
	TopicSSHKeysAndFirewalls                       = "SSH keys and firewalls"
	TopicNetworkConfiguration                      = "Network configuration"
	TopicSELinuxAccessControlListsAndCapabilities  = "SELinux, access control lists, and capabilities"
	TopicHostNameTimeZoneLocaleAndAlternatives     = "Host name, time zone, locale, and alternatives"
	TopicKernelSettingsAndKernelModules            = "Kernel settings and kernel modules"
	TopicSwapFilesystemsAndMounts                  = "Swap, filesystems, and mounts"
	TopicLanguageAndToolInstallers                 = "Language and tool installers"
	TopicContainers                                = "Containers"
	TopicDatabases                                 = "Databases"
	TopicCertificates                              = "Certificates"
)

// CoreModuleTopics lists the topics in the order of the index of module
// pages and of the listing of mmx describe-module. The registry above lists
// the core modules in the same order, topic by topic.
var CoreModuleTopics = []string{
	TopicFilesAndLinks,
	TopicDirectoriesDownloadsAndReleaseDirectories,
	TopicPackagesAndPackageSources,
	TopicAccountsAndServiceUnits,
	TopicCommandsChecksAndReboot,
	TopicScheduledJobs,
	TopicSSHKeysAndFirewalls,
	TopicNetworkConfiguration,
	TopicSELinuxAccessControlListsAndCapabilities,
	TopicHostNameTimeZoneLocaleAndAlternatives,
	TopicKernelSettingsAndKernelModules,
	TopicSwapFilesystemsAndMounts,
	TopicLanguageAndToolInstallers,
	TopicContainers,
	TopicDatabases,
	TopicCertificates,
}

// Description is one mmx describe-module entry. Field names and types come from the
// structs strict YAML decoding reads, allowed values from the lists validation
// checks, and a core module's retry policy from its registry flags. The rest
// is kept in the table below; describe_test.go loads every example and checks
// each field and the retry text against validation.
type Description struct {
	Name string `json:"name"`
	// Topic groups a core module with related core modules in the index of
	// module pages and in the listing of mmx describe-module. It is one of
	// CoreModuleTopics; the node task forms have none.
	Topic   string   `json:"topic,omitempty"`
	Summary string   `json:"summary"`
	Fields  []Field  `json:"fields"`
	Example string   `json:"example"`
	Results []string `json:"results"`
	Dryrun  string   `json:"dryrun"`
	// Retry states the retry policy. For a core module, config.Describe
	// writes it from the registry flags; a node task form sets it here.
	Retry string `json:"retry"`
	// WaitLimit names, for a core module that waits with its own limit, the
	// input that bounds the wait, such as "wait_at_most bounds the wait".
	// config.Describe ends the module's retry text with it.
	WaitLimit string `json:"-"`
	// Schema is the Go type strict YAML decoding reads for this entry.
	Schema reflect.Type `json:"-"`
}

// Field is one YAML field of an entry. Type is set here only where the Go type
// is not the YAML type (any, or a custom decoder).
type Field struct {
	Name     string   `json:"name"`
	Type     string   `json:"type"`
	Values   []string `json:"values,omitempty"`
	Required bool     `json:"required"`
	Default  string   `json:"default,omitempty"`
	Notes    string   `json:"notes,omitempty"`
}

// CoreModule is one core module's entry in the registry.
type CoreModule struct {
	Name        string
	Description *Description
	// NewChangeActivation validates saved module data and reconstructs
	// activation of the proposed change or restoration of the previous state. Only modules
	// that use an owed restore supply it.
	NewChangeActivation func(destinationFile string, moduleRestoreData json.RawMessage) (ChangeActivation, error)
	// NewInputs makes the module's input type.
	NewInputs func() Inputs
	// NewWrittenInputs makes the form a task writes the inputs in, when the
	// module keeps them in another form: a download is a file from a web
	// address, and the package and repository modules share one type for
	// every package manager.
	NewWrittenInputs func() WrittenInputs
	// RetryByDefault: a module that changes a resource repeats a failed try
	// by default; a command, a request, a check, and systemd_service try
	// once, because a start that failed fails again.
	RetryByDefault bool
	// WaitsWithItsOwnLimit: the module takes no retry or time limits, and one
	// of its own inputs bounds it, which its description's WaitLimit names
	// (wait_for, reboot, the network file modules, firewalld_zone_membership).
	WaitsWithItsOwnLimit bool
	// EvaluatesOnce: the module rejects retry (assert).
	EvaluatesOnce bool
	// EndsTheNodeJob: the module ends the node job, so it may only be the last
	// node task of a fleet stage's own list (reboot).
	EndsTheNodeJob bool
	// OnlyReads: the module changes nothing on the node, so it may run in
	// checks_before_restart_or_reload. A module that may or may not change
	// something says so through an OnlyReads method on its inputs instead.
	OnlyReads bool
	// KeepsOutputOfAFailure: a failed try keeps what the module captured
	// (exit code, output, HTTP status and body) for failure handlers.
	KeepsOutputOfAFailure bool
	// ChecksAConfiguration: the module may check a node task block's combined
	// configuration in its validate phase.
	ChecksAConfiguration bool
}

// OnlyReadsWhen is an input type that changes nothing only when its inputs
// say so, such as a shell command with changed_when: false.
type OnlyReadsWhen interface {
	OnlyReads() error
}

// ResultBasic is the result every node task has.
var ResultBasic = []string{"status", "change"}

var resultBasic = ResultBasic

// CoreModules lists the core modules in registry order.
func CoreModules() []CoreModule { return coreModules }

// LookUpCoreModule finds a core module by name.
func LookUpCoreModule(name string) (CoreModule, bool) {
	for _, m := range coreModules {
		if m.Name == name {
			return m, true
		}
	}
	return CoreModule{}, false
}

// Inputs is the input type of one core module.
type Inputs interface {
	// Prepare checks the written inputs when the project loads and reads the
	// files they name on the controller machine. A relative path starts at
	// base: the role's directory inside a role, and the project root
	// otherwise.
	Prepare(base string) error
	// Run inspects the node and, unless this is a dryrun, changes it.
	Run(ctx context.Context, task NodeTask) protocol.Result
}

// NodeTask is what the node task execution engine gives a module that runs.
type NodeTask struct {
	OwedRestores OwedRestoreService
	Runtime      Runtime
	// Scope resolves the module's private values, so the task's trace sees
	// the secrets they read.
	Scope    value.Scope
	NodeName string
	// Packages is the node's package manager.
	Packages PackageManager
	// Checking: the task checks a node task block's configuration (validate
	// or checks_before_restart_or_reload): a command accepts only exit 0 and
	// changes nothing.
	Checking bool
	// PreviewChanged: in a dryrun, an earlier node task of this node task block
	// would change something, so a health check cannot check the proposed
	// configuration yet.
	PreviewChanged bool
	// PreviewPathsOfTaskBlock: in a dryrun, the paths that the earlier node
	// tasks of this node task block would create or change. Runtime.PreviewPaths
	// holds those of the whole node job.
	PreviewPathsOfTaskBlock PreviewPaths
	// InputError places an error about one input, such as that, at the line
	// that wrote it, with where the values it reads came from.
	InputError func(field string, written any, err error) error
	// TaskBlock is the node task block's service records and commands, and
	// the node job's reboot. The engine owns these records across runs.
	TaskBlock TaskBlockServices
}

// TaskBlockServices are the node task execution engine's records that a module
// uses: a service that follows its node task block's tracked inputs, a
// command that runs when they changed, and a reboot that ends the node job.
type TaskBlockServices interface {
	RefreshService(ctx context.Context, s Service) protocol.Result
	RunCommandThatFollowsTrackedInputs(ctx context.Context, recordKey string, run func() protocol.Result) protocol.Result
	Reboot(ctx context.Context, r Reboot) protocol.Result
}

// WrittenInputs is a module's inputs in the form a task writes them, which
// the loader turns into the module's input type.
type WrittenInputs interface {
	ModuleInputs() (Inputs, error)
}

// NewWritten makes the type a task writes the module's inputs in.
func (m CoreModule) NewWritten() any {
	if m.NewWrittenInputs != nil {
		return m.NewWrittenInputs()
	}
	return m.NewInputs()
}

// ModuleName is the rule for a module's name: lower-case letters, digits, and
// underscores, starting with a letter, at most 64 characters. A name is then
// also a valid YAML key and a valid Starlark identifier.
var ModuleName = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
