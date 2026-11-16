# MMX: main plan

Build a massively simpler and more performant configuration-management system
that can replace Ansible for most users managing ordinary Linux servers.

**The user should understand the servers and the work being done to them. MMX
should contribute as few additional things to understand as possible.**

The [product principles](#product-principles) state what MMX is
for. The design documents below are the specification for the first
release, 0.1.0: the complete intended product, described as it will work,
built or not. Each core module's own behavior, examples, and reasons are on its
[module page](../../reference/modules/README.md); the design documents own the design that
spans modules. `docs/changing-mmx/design/README.md` owns direction, priorities, and the
one record of what is built. The
[user documentation](../../README.md) describes current behavior, and the
[acceptance contracts](../acceptance.md#acceptance-contracts) define MMX's
correctness. MMX does not monitor servers: monitoring belongs to a
monitoring tool, which a playbook can install like any other application.

**MMX 0.1.0 is the first release.** Every change keeps the promises in
[compatibility between releases](#compatibility-between-releases);
[`AGENTS.md`](../../../AGENTS.md#compatibility-between-releases) says how a
contributor keeps them.

## Design map

Read the workflow first, then the document relevant to the work:

| Document | Owns |
|---|---|
| [Workflow and CLI](workflow.md) | First use, bootstrap experience, commands for selecting nodes and fleet stages, named playbooks, run/dryrun/resume, ad hoc `run-command` commands, module lookup, human and agent interaction, output, saved history, exit results, and limited controls for non-privileged users. |
| [Configuration and node data](configuration.md) | Project settings and separate playbooks, inventory connection fields and group variables, inventory choice and limits, command-line variables, roles, values, loops, templates, facts, and secret inputs. |
| [Execution and recovery](execution.md) | Bootstrap mechanics, SSH and HTTPS connections, privilege and application users, saved run bundles (instructions and project assets), node jobs, service configuration hashes, activation and cleanup, reboots, cancellation, and fleet rollout. |
| [Modules](modules.md) | The behavior every module shares; the three kinds of module (core, official extra, and the project's own), the primitives, Starlark scripts, module descriptions, and the JSON protocol; change/failure reporting; and non-mutating inspection. Each core module's own behavior is on its [module page](../../reference/modules/README.md). |
| [Package modules](modules-packages.md) | The package modules of each distribution family, the design they share, package modules in Starlark, and the language and tool installers. Each module's own behavior is on its [module page](../../reference/modules/README.md). |
| [System modules](modules-system.md) | An overview of the modules for firewalls, scheduled jobs, host name and time zone, kernel settings and kernel modules, the alternatives system, swap, filesystems and mounts, SSH key pairs, access control lists, and capabilities, and the decisions that span them. Each module's own behavior is on its [module page](../../reference/modules/README.md). |
| [Network configuration modules](modules-network.md) | What `ifupdown_interfaces_file`, `netplan_file`, and `systemd_networkd_file` share: one native configuration file per node task, activation, checks, and restoration when new connections from the controller machine fail; and `wireguard_interface`, a WireGuard interface between servers with a private key that its node keeps. Each module's own behavior is on its [module page](../../reference/modules/README.md). |
| [Container, database, and certificate modules](modules-applications.md) | The design shared by the modules for Docker, Podman, PostgreSQL, MySQL, and certbot. Each module's own behavior is on its [module page](../../reference/modules/README.md). |
| [The official extra modules](modules-official-extra.md) | The inputs and the behavior of each official extra module: Incus instances and profiles, libvirt domains, ZFS datasets, disk partitions, LVM, filesystem growth, RabbitMQ, MongoDB users, pg_auto_failover, and pgBackRest. |
| [Code map](../code-map.md) | Where the source code of each part of a run lives, a first reading path through the code, and the tests that explain behavior. |
| [Development](development.md) | Supported platforms and tested distributions, Go organization, dependency/build policy, tests, disposable test environments, and performance validation. |

Each detailed rule belongs in its owning document.
`docs/changing-mmx/design/README.md` links to the owning documents.

## Product principles

Systems should remain comprehensible when a smart engineer has effectively
become a monkey at 3 AM.

MMX is a deliberately simple, extremely fast configuration-management tool
written in Go: few concepts, one way to do each job, and small modules. The goal is a massively simpler and more performant configuration-management
system that can replace Ansible for most users.

### Make the happy path obvious

MMX should be incredibly easy to understand and reason about. Many users will
be developers. MMX users should need to understand the servers MMX manages,
not a collection of extra distinctions introduced by the
configuration-management tool.

The happy path makes the decisions for the user:

- First `dryrun` or `run` → automatically set up MMX on the selected nodes.
- Common operation → a core module, named for the tool it drives, with
  sensible defaults.
- Custom operation → `shell`, with optional conditions that decide whether
  the command runs, dryrun, and change reporting.
- Reusable resource interface → a module of the project's own: a Starlark
  script with a module description, called by the module's name like any
  module.

Keep the interface extremely simple and make it hard to mess up. Validate
inputs before making changes, choose predictable defaults, fail clearly, and
report what happened. Handle input validation, predictable defaults, clear
failures, and reporting in the implementation rather than making users
navigate more switches and execution modes.

The Ansible `command` versus `shell` distinction illustrates what to avoid:
users write a command containing a pipe and expect the pipe to work. Requiring
users to discover a different execution mode adds a concept without helping
users configure the servers MMX manages. MMX exposes `shell` for custom
commands.

**Every additional user-facing concept needs to earn its place.** Internal
sophistication is worthwhile when it makes the external model simpler.

### AI authors, human readers

Expect AI coding agents to write most configuration, templates, and custom
modules. Humans still need to inspect, understand, approve, and debug the
result, including at 3 AM. Human readability remains a leading design priority.

Optimize for reading and reasoning rather than saving keystrokes. Explicit
names, a little repetition, and visible structure are worthwhile when they
make the result easier to follow. Compact syntax and clever shorthand earn
their place only if they also improve comprehension.

Prefer verbose and clear names over succinct but ambiguous ones. For example,
`max_nodes_at_once` states the node limit, and `wait_for_all_in_batch` explicitly
states whether the whole group must finish before more nodes start.

Use explicit, matching names for opposite conditions:
`run_only_if_this_command_succeeds` and `skip_if_this_command_succeeds`. The
reader should not need to translate a second word to understand the opposite
behavior.

A name says what happens. A competent junior engineer who reads one line at
three in the morning knows what it does: `also_delete_home_directory_and_mail`, not
`remove_home`; `keys_not_listed: remove`, not `keys_mode: replace`. Common
abbreviations such as `env` and `repo` are fine.

"No magic" means that what happens is easy to follow, with sensible defaults.
"No magic" does not mean exposing internals. A setting that would always
have the same value does not exist. MMX automatically performs the action
controlled by such a setting and reports the action in the run output.

Consistent patterns, precise module contracts, realistic examples, and clear
validation errors help agents generate and correct work as well as helping
humans review the work. Keep one documented happy path that both agents and
humans can follow.
Agent authorship is not a reason to make the configuration language more
complex.

Agents will also operate MMX through the commands and node and fleet stage
selection rules that human operators use.
Provide structured results, stable exit semantics, and an explicit
noninteractive mode, including when an agent uses a terminal. Ordinary runs
and resumes need no run ID; IDs support selecting a specific recorded run.
Driving MMX must not require navigating a menu or parsing decorative progress
output.

### Philosophy

- Official managed-server target: Linux servers that run systemd, on amd64
  or arm64, tested on the distributions that the
  [supported platforms](development.md#supported-platforms) list. Each
  module's page says on which distributions the module is tested. Servers
  that systemd does not run are outside MMX. The controller executable
  `mmx` can also run on macOS. Running `mmx` on macOS does not require
  support for managing macOS nodes.
- No need to support every OS or platform.
- Built from first principles around the operations that make up the overwhelming majority of server configuration.
- The core modules cover the common work on the tested distributions.
  Each core module does one job, detects change, and is named for the tool
  the module drives. Common work never needs a shell recipe. A task that an
  existing module and its check already do well gets no module of its own.
- MMX does not hide the distribution or the tool. There is no `package` or `service` module that picks a tool by distribution: a role says `apt_package`, `systemd_service`, or `ufw`. MMX's users control their servers and mostly run one distribution; a mixed fleet uses facts and `when`.
- No role-sharing ecosystem. Roles are written for one project, usually by a coding agent, and are read by the people who run them. A role downloaded from strangers runs as root on every server.
- One mechanism for one pattern. Before adding a mechanism, extend the one MMX already has for the neighbouring case.
- Extremely simple, well-documented module interface for everything else: a Starlark script that works through the same [primitives](modules.md#primitives) as the core modules, with a description that `validate` checks every call against.
- Custom modules should be easy enough that users can point an AI coding agent at the module specification and examples and have it implement and test one.
- Less common tools are [official extra modules](modules.md#official-extra-modules), Starlark scripts that the MMX project writes, tests, and ships beside `mmx`, or the project's [own modules](modules.md#the-projects-own-modules). When unsure whether work is common, it goes in core.
- Named secrets can come from environment variables. For secrets stored with
  the code, provide a built-in vault: a file of named secrets encrypted with
  the age encryption format and protected by a shared passphrase.
  Teams keep one encrypted file in version control; MMX unlocks it when needed.
  Environment-only setups need no vault. The encrypted-file workflow needs no
  separate encryption tool or individual encryption-key setup.
- Keep the codebase small and built on a few explicit design rules, so a reviewer, human or LLM, can reason through it and verify it against its acceptance contracts.
- Use Go libraries that work with cgo disabled, including transitive dependencies.
  Build releases with `CGO_ENABLED=0` and avoid extra C toolchain or third-party
  C-library requirements.
- Keep each Go dependency as a
  [pruned copy](development.md#pruned-copies-of-the-dependencies) in the MMX
  repository. A dependency copy changes only for a proven vulnerability that
  MMX is exposed to or a high-value fault in real use.
- MMX's users are administrators with full root on their servers. MMX does
  administrators' work reliably and makes the work easy. MMX does not guard
  servers from the servers' administrators or from people the administrators
  grant access. Protect the real boundaries (authentication, server identity,
  secrets between nodes) and test the common workflows well; do not write code
  for rare cases or for trusted people misusing their access.
- Coverage is the product. MMX replaces Ansible for most users only if the
  everyday Ansible workflows work, and work simply. Users must be able to
  target a subset or another environment and see what a run will target.
  Users must be able to run one-off commands across nodes, look up modules,
  run commands as application users, and reboot nodes and wait.

  MMX must manage packages and package sources, files, templates, accounts,
  SSH keys, services and timers, firewalls, containers, scheduled jobs,
  databases, and certificates. Users must be able to unpack releases,
  deploy applications, and check and wait for readiness. MMX must support
  gradual rollout with failure limits and previews with diffs. A workflow
  that is not built yet is a gap on the [plan](#release-plan), never
  something to trim. Simplicity comes from one way to do each job and no
  machinery for rare cases, not from dropping everyday workflows.
- Designed for high concurrency and low controller overhead.
- The Monkeymaxxing teaching's Ten-Thousand-Host Run provides a meaningful
  real-world stress test.

### Scalable human output

Don't dump 10,000 nearly identical node results into the terminal.

- Small fleet → individual node results can be useful.
- Hundreds of nodes → a bounded dot display may be useful.
- Thousands of nodes → percentages, completed/total counts, and exceptions.
- Detailed and machine-readable results remain available underneath.

When automatic aggregation increases, explain it once and show how to restore
individual results. Save run output as work proceeds so terminal scrollback is
never the only copy. Large requested diffs get a bounded preview and an obvious
full-output override. Users can redisplay saved results without deploying again.

**Normality compresses. Exceptions expand.**

The [workflow contract](workflow.md#human-output-is-a-view-of-structured-results)
owns presentation controls, saved history, diffs, and agent interaction.

### Canary workflow

Canary deployment doesn't need to be a special MMX feature.

Ordinary node and group selection already provides a canary deployment:

1. Target the canary node/group.
2. Verify the result.
3. Target the broader fleet.

Teach canarying as an operational workflow rather than adding unnecessary machinery to MMX.

### Optional AI/Jev hooks

Possible optional hooks for Jev-style decision models where useful—for example, evaluating whether a particular failure looks retryable.

Normal/default MMX operation remains deterministic.

### Architecture

The controller process (`mmx`) runs on the controller machine, such as your
laptop. Each managed Linux machine is a node with the node runner executable
(`mmx-runner`) installed. A fleet stage selects nodes and defines their
`node_tasks`; a `task_block` groups related node tasks into named phases.
The [How MMX works](../../how-it-works.md) page follows fleet stages and node
tasks through the processes, and the [code map](../code-map.md) through the code components.

```text
CONTROLLER MACHINE
  Controller process: read inventory and fleet stages, select nodes, limit concurrency
        |
        | authenticated HTTPS requests, or verified SSH commands
        v
EACH SELECTED NODE
  Node job worker process: execute this node's node_tasks in order
        |
        | save results on this node; return them over HTTPS or SSH
        v
CONTROLLER MACHINE
  Controller process: aggregate progress, expand failures, retain results
```

Set up ordinary Linux nodes automatically through Go bootstrap over verified
SSH, using keys or passwords and root access or sudo, including password-protected
sudo. The node HTTPS API process and node job worker process run as root;
application commands explicitly select another user when needed. Normal work
uses authenticated HTTPS without another SSH login or sudo prompt. SSH fallback
uses the node job handling code that the node HTTPS API process uses.
Each accepted node job runs in a separate node job worker process that
outlives the controller connection and node HTTPS API process restarts;
MMX owns work and recovery. An overlay network is optional. Failed nodes can
be resumed while independent nodes continue.

Required service activation must survive interrupted runs, even when the
service's configuration files are already correct on resume. A healthy
service does not prove the service loaded the new configuration.
For the common file-and-service case, compare installed configuration
hashes with the service's last saved configuration.
Keep hashes for the current boot.

Establish a missing baseline before changes so an unchanged run after reboot
does not reload the service. Keep service configuration hash records in MMX
so the user can reason about changes and required follow-up.

A core module should be competitive with straightforward native commands
for equivalent work. Declarative behavior does not excuse being orders of
magnitude slower. Use Go's system interfaces where they fit and existing tools
such as package managers where they own the operation.

The [design map](#design-map) points to the detailed contracts;
the [acceptance contracts](../acceptance.md#acceptance-contracts) define current outcomes and
link the executable tests.

## Design direction

```text
Project settings live in mmx_project_settings.yml at the project root.
Inventory says which machines exist.
A named playbook in playbooks/ contains ordered fleet stages, each selecting nodes and listing node tasks.
A role packages reusable node tasks with explicit inputs and outputs.
A node task block groups related node tasks into change, check, activation, and cleanup phases.

Controller process:       resolve -> inspect -> dispatch bounded node jobs -> collect results
Node job worker process:  lock -> inspect and change resources -> activate -> cleanup -> report
```

- Manage Linux servers that run systemd from Linux and macOS controller
  machines
  ([supported platforms](development.md#supported-platforms)). Core modules
  name the tool they drive, such as `apt_package` and `ufw`. The
  distribution does not decide a module's kind or language. Less common tools are
  [official extra modules](modules.md#official-extra-modules) or the
  [project's own modules](modules.md#the-projects-own-modules).
- Build the `mmx` and `mmx-runner` executables and core modules in Go with
  cgo disabled. Native tools such as apt, systemd, OpenSSH, and Bash stay
  ordinary integrations.
- Set up the node component automatically over verified SSH (keys or
  passwords, root or sudo), then use the authenticated node HTTPS API process
  with SSH fallback. The node component runs as root; application commands
  select another user explicitly. Existing
  [administrator SSH keys](execution.md#administrator-ssh-keys-authorize-https)
  authorize HTTPS, with no separate MMX user list.
- Keep project settings in `mmx_project_settings.yml` at the project root, so playbooks do not
  repeat them. Playbooks (`playbooks/<name>.yml`) hold only fleet stages and
  command-line variable declarations; inventories live in
  `inventories/<name>.yml`. Commands name the playbook, as in `mmx run deploy`
  ([project settings](configuration.md#project-settings)). Pass declared command-line
  variables with repeatable `--var NAME=VALUE`; each name must appear in the
  playbook's `command_line_vars` and cannot also appear in any `vars`.
- Run each node job in a detached node job worker process on the node,
  so the work survives controller disconnection. `run` uses current project
  files; `resume` finishes a saved run's failed and unfinished work, so
  `retry` means only a node task's automatic repeats. Failure handling and
  cleanup belong to the current node job attempt only.
- Reload services from
  [configuration hashes](execution.md#service-reloads-follow-configuration-hashes),
  so an interrupted change is still activated and a reboot alone causes no
  reload. A command that must follow a change uses the same record.
- Check a new file with `check_new_file` before the new file replaces the
  live file.
- Wait for readiness with [`wait_for`](../../reference/modules/wait_for.md).
- Use readable YAML, explicit role inputs, Go templates, automatic node facts,
  short traceable variable precedence, and roles listed in playbook order.
  Named node task results (`.node_tasks.<id>`), `set_vars`, `show`, loops,
  and explicit fleet stage outputs are defined in
  [configuration](configuration.md).
- Order cross-node work with playbook fleet stages, `requires`, bounded concurrency
  (`max_nodes_at_once: 100` by default), and a per-fleet stage
  [failure policy](execution.md#choose-what-a-node-failure-stops). Rolling
  deployments are `for_each_node` loops with explicit drain/update/verify/rejoin.
- Take secrets from environment variables or a shared-passphrase age vault kept
  in version control, and deliver each node only what it uses. The inventory's
  SSH and sudo passwords are ordinary secret references.
- Keep output small for normal results and detailed for exceptions, save
  structured history, and make `mmx results` easy for humans and agents to query.
  **Normality compresses. Exceptions expand.**

## Release plan

The Release plan section of `docs/changing-mmx/design/README.md` is the only place that
records what is built. The spec documents describe the finished release.
A feature is done when the feature passes the audit in
the [release checklist](#release-checklist).

### Scope of the first release

MMX manages Linux servers that run systemd, on amd64 or arm64, and each
module is tested on the distributions that its page lists
([supported platforms](development.md#supported-platforms)). The first
release is tested on these distributions:

- the Debian family: Debian 12 and 13, Ubuntu 24.04 and 26.04, with the
  whole spec;
- the Red Hat family: Rocky Linux 9 and AlmaLinux 10, with SELinux
  enforcing, with the base suite and the family's modules;
- Arch Linux and openSUSE Leap, with the base suite.

Debian 13 and Ubuntu 26.04 are the reference distributions for test runs
that do not cover every release. The current build is tested on the four
releases of the Debian family; the status table below calls them the four
target distributions.

Common work has a core module. Core modules cover Docker and Podman, the
common language package installers, PostgreSQL and MySQL, kernel settings,
kernel modules, and the alternatives system. Less common tools are official
extra modules or the project's own modules. There are no tags. The
[Monkeymaxxing exercises](#monkeymaxxing-requirements) guide the teaching
use cases and do not gate the first release.

The first release, 0.1.0, is MMX's beta: its first published release.

### Status

The documents in the [design map](#design-map) describe the first release.
The code in `cmd/` and `internal/` implements part of the first-release
design. This table is the record of what is built:

| Part of the design | Status |
|---|---|
| Project settings, playbooks, inventory, roles, values, loops, facts, secrets, bootstrap, the two connections, node jobs, service records, retries, reboot, fleet stages, limits, failure policies, outputs, `run`, `dryrun`, `resume`, results, and history | Built and tested: the whole catalog passes on all four target distributions on AMD64, and `vm.py test`, `node_facts.py`, `wireguard_interfaces.py`, and `routine_fixes.py` pass on all four on ARM64 ([validation record](../validation.md#platform-validation-on-the-four-target-distributions)). |
| The modules `file`, `line`, `download`, `git`, `directory`, `symlink`, `user`, `group`, `authorized_keys`, `ufw`, `http`, `wait_for`, `assert`, `reboot`, `shell`, `apt_package`, `apt_repository`, `unpack_archive`, `systemd_unit`, and `systemd_service`, and the node task form `show` | Built under the names in the documents in the design map. The rows below say which additional fields and behavior from the documents in the design map are built; what no row names is not built yet. |
| The node facts `.node.distribution.*`, `.node.package_manager`, `.node.virtualization.type`, `.node.fully_qualified_name`, and the two default addresses | Built. |
| The notice for a blocked port 7443, with `connection_used` and `connection_fallback_reason` in the results | Built and tested against a node whose own firewall blocks the port: ufw on Debian and Ubuntu. |
| Debian 12 | Built and tested: the whole catalog passes ([validation record](../validation.md#platform-validation-on-the-four-target-distributions)). |
| Ubuntu 26.04 | Built and tested: the whole catalog passes ([validation record](../validation.md#platform-validation-on-the-four-target-distributions)). |
| The conditions `number_*`, `version_*`, and `path_exists`; the result fields `stdout_json` and `response_body_json` | Built and tested on all four target distributions. |
| `owner` and `group` as a number on `file`, `directory`, `download`, and `unpack_archive` | Built and tested on Debian 12. |
| `dangerously_skip_checksum` on `download`, `unpack_archive`, and `apt_repository`; relative source and template paths stay inside the project ([path rule](../../reference/modules/file.md#paths-on-the-controller-machine)) | Built and tested on all four target distributions. |
| Template functions (Sprig's functions that depend on their arguments alone, `toYaml`, `percent`) and the literal `{{` form in task values | Built and tested on Debian 12. Sprig adds 0.5 MB to the node runner executable and 1.0 MB to the controller executable. |
| Variables in `url`, `sha256`, and in source and template paths (`file`, `download`, `unpack_archive`, `directory.source`, `apt_repository` keys, `systemd_unit.template`) | Built and tested on Debian 13. `authorized_keys.key_files_on_controller_machine` and `key_directories_on_controller_machine` also accept variables. |
| `state: do_not_start_or_stop` and `masked` on `systemd_service`, with the failure for a oneshot unit asked to be started; the unit kinds `.target`, `.swap`, `.slice`, and `.automount`; `run_only_once_marker_file` on `shell`; `wait_for` with `command_succeeds` | Built and tested on Debian 13 and Debian 12. |
| The command that follows a change (`only_when_tracked_inputs_changed`, with its record in `/var/lib/mmx/command_records/`) and `checks_before_restart_or_reload` | Built and tested on Debian 13 and Debian 12. |
| A Git checkout as a tracked input of its node task block | Built and tested on all four target distributions ([validation record](../validation.md#platform-validation-on-the-four-target-distributions)). |
| `in_systemd_of_user_account` on `systemd_unit` and `systemd_service`, with lingering turned on by the task | Built and tested on Debian 13. `podman_quadlet` uses `in_systemd_of_user_account` for a container of an ordinary account. |
| Work across nodes: `for_each_node` with helper nodes and `--limit`, `max_nodes_at_once` as a percentage, a fleet stage's `when` checked for each node as it starts, `except_nodes`, `--only-fleet-stages`, `save_on_controller_machine`, `read_file`, `shell.output_is_secret` and secrets that start on a node, `ssh_key_pair`, `/etc/mmx/do_not_manage`, and the record of the last successful run | Built and tested on all four target distributions ([validation record](../validation.md)). The node runner executable grows by 0.05 MB. |
| `apt_package` in full: required per-entry `version` policies and patterns kept with apt preferences, `allow_downgrade`, entries with `newest_on_every_run`, package files, `also_delete_its_config_files`, `also_remove_packages_no_longer_needed`, packages that do not start their services; `apt_repository` with `username` and `password` | Built and tested on Debian 13. |
| `http` requests that write (`method`, `headers`, basic authentication, `json_body`, `body`, `trusted_certificate_authority_file`, `changed_when`) and `git.files_changed_in_checkout_on_node` | Built and tested on all four target distributions ([validation record](../validation.md)). |
| The language and tool installers: `pip_package`, `gem_package`, `npm_global_package`, `snap_package`, and the lock file modules `npm_project_dependencies`, `yarn_project_dependencies`, `bundler_project_dependencies`, `composer_project_dependencies`, `uv_project_dependencies`; what each installed as a tracked input; every process a task starts with umask 022 | Built and tested on all four target distributions ([validation record](../validation.md)); `snap_package` where the distribution packages `snapd` (Debian, Ubuntu). |
| Files, accounts, and the firewall: `if_file_exists`, `content_from_node_paths`, and `backup_original_file_to` on `file`; the six placing settings of `line`; `edit_file`; `symlink.replace_existing_file`; removing a directory and `delete_everything_inside_except`; `download.decompress` and every tar compression in `unpack_archive`; the `user`, `group`, and `authorized_keys` fields of [accounts and groups](../../reference/modules/user.md) and [SSH authorized keys](../../reference/modules/authorized_keys.md); `file_access_control_list`, `linux_capability_on_program_file` and `from_inventory_group` | Built and tested on all four target distributions ([validation record](../validation.md)). The xz and zstd decoders add 0.6 MB to the node runner executable. |
| `ufw`'s `incoming_rules` with `incoming_interface`, `routed_rules`, `outgoing_rules`, and `default_routed`; `apt_package` waiting for another apt's lock on the package lists | Built and tested on all four target distributions ([validation record](../validation.md#platform-validation-on-the-four-target-distributions)). |
| The other [system modules](modules-system.md): `systemd_scheduled_job`, `cron_job`, `hostname`, `time_zone`, `locale`, `update_alternatives`, `sysctl_setting`, `kernel_module`, `swap_file`, `create_filesystem_on_empty_device`, `fstab_mount` | Built and tested on all four target distributions ([validation record](../validation.md)); the kernel and storage suites run in VMs. |
| The [container modules](modules-applications.md): `docker_image`, `docker_compose_project`, `podman_image`, `podman_quadlet`, and the `wait_for` forms `docker_container_is_healthy`, `docker_compose_project_is_ready`, and `podman_container_is_healthy`; an image ID as a tracked input of its node task block; a node task's private directory below `/run/mmx/private_task_directories/`, which holds a task's login files and the copies for an account's checks and which the node removes after a killed node job worker process | Built and tested on all four target distributions ([validation record](../validation.md)): Docker with Docker Compose 2.26.1, 2.40.3, and 5.6.0, in the classic and in the containerd image store; Podman 4.3.1 to 5.7, with Quadlet where Podman has it (4.4 and newer). |
| The [database and certificate modules](modules-applications.md): `postgresql_role`, `postgresql_database`, `mysql_user`, `mysql_database`, `certbot_certificate`; a secret in a login of these modules does not hide the node task's result; a node task that fails at its time limit, or whose node job is stopped, keeps its own reason | Built and tested on all four target distributions ([validation record](../validation.md)): PostgreSQL 15 to 18; MariaDB 10.11 to 11.8, and MySQL 8.0 and 8.4 where the distribution packages MySQL; certbot 2.1 to 4.0 against a certificate authority on the test guest. |
| The module architecture, part one: every core module in one place (its file in `internal/modules`) with a registry, and every module called by its name; the loader, `validate`, `describe-module`, `explain`, and the node task execution engine work through the registry | Built and tested on all four target distributions ([validation record](../validation.md)). |
| The module architecture, part two: the [primitives](modules.md#primitives) as `Runtime` methods, built on the `file`, `directory`, and `symlink` code and the mutation boundary, through which MMX's own bookkeeping writes (the run-once marker, apt's service-policy program) now go | Built and tested on all four target distributions ([validation record](../validation.md)). Core modules keep their shared helpers, on which the primitives that modify node resources are built. |
| The module architecture, part three: module descriptions (`module_description.yml`) checked by `validate`; Starlark scripts (`module.star`) run on the primitives, with MMX's own change, difference, and fingerprint; executables as `module_executable` with the JSON protocol's `outputs`, `diff`, and `fingerprint`; a module's fingerprint as a tracked input of its node task block; `official_extra_modules/` beside `mmx` with its version check; `mmx module-usage`; the kind in `describe-module`, `explain`, and results | Built and tested on all four target distributions ([validation record](../validation.md)). |
| A node keeping module files, and the files from a web address of `download` and `unpack_archive`, by their SHA-256 in `/var/lib/mmx/kept_files/`, so a run sends only the files it does not have | Built and tested on all four target distributions ([validation record](../validation.md)); the files from a web address on Debian 13 and Ubuntu 26.04. |
| [The official extra modules](modules-official-extra.md): `incus_instance`, `incus_profile`, `libvirt_domain`, `zfs_dataset`, `create_partitions_on_empty_disk`, `grow_partition_into_free_space_after_it`, `lvm_volume_group`, `lvm_logical_volume`, `grow_filesystem_to_fill_device`, `rabbitmq_virtual_host`, `rabbitmq_user`, `rabbitmq_policy`, `rabbitmq_plugin`, `mongodb_user`; and what a script needs to drive a tool: the result statuses `missing_prerequisite`, `not_checked`, and `invalid_inputs`, `try_again: False`, the primitives `find_program` and `earlier_node_task_would_change`, root's home directory for a command, and `private_value` in a module description | Built and tested on all four target distributions ([validation record](../validation.md)). Where a tool cannot be installed on a distribution, its suite proves the module's behavior without the tool. Each module's page lists both the tested distributions and the distributions where the tool cannot be installed. |
| [The official extra modules for PostgreSQL](modules-official-extra.md#pg_auto_failover): `pg_auto_failover_monitor`, `pg_auto_failover_node`, `pg_auto_failover_primary` ([pg_auto_failover](modules-official-extra.md#pg_auto_failover)), `pgbackrest_stanza`, and `pgbackrest_restore` ([pgBackRest](modules-official-extra.md#pgbackrest)); the guest suites' third test machine, `mmx-third`, for a suite that needs one | Built and tested on all four target distributions ([validation record](../validation.md#postgresql-failover-and-backups)): pg_auto_failover 2.0 to 2.2 with PostgreSQL 15 to 18; pgBackRest 2.45 to 2.58, with an S3-compatible bucket that keeps versions on Debian 13 and Ubuntu 26.04. |
| The `mmx secrets` key commands (`add-random`, `list-keys`, `diff-keys`, `usage`) and the [one-time playbook](workflow.md#one-time-playbooks) commands (`new-one-time-playbook`, `check-one-time-playbooks`, `complete-one-time-playbook`); a reference in which a secret selects another value is rejected when the project loads | Built and tested on all four target distributions ([validation record](../validation.md)). |
| `mmx secrets set KEY`, which reads one value from standard input or a hidden prompt, and `skip_when_no_nodes_are_selected` on a fleet stage or a `for_each_node` loop; while it selects no nodes, a run does not load the secrets it reads, and `mmx secrets usage` marks a key that only such fleet stages read as not needed | Built and tested on all four target distributions ([validation record](../validation.md#platform-validation-on-the-four-target-distributions)): `task_secrets.py` covers `secrets set`; Go tests drive a run of fleet stages that select no nodes through the controller with stand-in nodes. |
| [The controller machine as a node](execution.md#the-controller-machine-as-a-node): a run through ordinary SSH and through the node HTTPS API to the Linux controller machine, with an interrupted run and `mmx resume`; the guard that refuses to reboot the controller machine, checked through `127.0.0.1`, the host name, and another address; the user documentation, with the macOS boundary | Built and tested on all four target distributions ([validation record](../validation.md)). A container that shares its server's boot ID is covered by Go tests of the controller-machine reboot guard and by a measurement of Docker, and a macOS machine as a node by a Go test with a stand-in `ssh`. |
| [Reachability confirmation and owed restore](execution.md#reachability-confirmation-and-owed-restore): the node side and the controller process's confirmation loop, proven by `owed_restores.py` with a module that exists only in test builds | Built and tested on all four target distributions ([validation record](../validation.md#platform-validation-on-the-four-target-distributions)). The rows below name production modules that use reachability confirmation and owed restore. |
| The fact `storage.block_devices`; the example `security-updates-on-debian-and-ubuntu` | Built. The fact is tested on all four target distributions; the example on Debian 13 ([validation record](../validation.md#the-capability-reviews-additions)). |
| `systemd_networkd_file`, a [network configuration module](modules-network.md) | Built and tested on Debian 12 and 13 ([validation record](../validation.md#systemd-networkd-file-module)). |
| `netplan_file`, a [network configuration module](modules-network.md) | Built and tested on Ubuntu 24.04 and 26.04 ([validation record](../validation.md#platform-validation-on-the-four-target-distributions)). |
| `wireguard_interface`, a [WireGuard interface](modules-network.md#a-wireguard-interface) with the node's own private key | Built and tested on all four target distributions, in containers and on ARM64 cloud servers ([validation record](../validation.md#wireguard-interface-module)). |
| The routine fixes: `restart_now` with `run_only_once_marker_file` on `systemd_service`; [`systemd_journal_vacuum` and `delete_old_files`](modules-system.md#routine-cleanup-of-disk-space); [`docker_unused_images` and `podman_unused_images`](modules-applications.md#unused-images); [`apt_downloaded_package_files`](modules-packages.md#apts-downloaded-package-files) | Built and tested on all four target distributions ([validation record](../validation.md#routine-fix-modules)). |
| [`pacman_package`](modules-packages.md#pacmans-own-rules), the package module of Arch Linux, as an official extra module in Starlark ([package modules in Starlark](modules-packages.md#package-modules-in-starlark)); and the primitive `run_command_that_refreshes_a_package_index` | Built and tested on Arch Linux; `pacman_packages.py` passes ([validation record](../validation.md#pacman-package-module)). |
| The rule that a package module for a family other than Debian and Red Hat is an official extra module in Starlark ([where a module belongs](modules.md#where-a-module-belongs)); the worked example [Write a package module for your distribution](../../how-to/write-a-package-module.md), built from `pacman_package` | Built. The guide's excerpts match `pacman_package`'s script; the documentation tests pass. |
| MMX on the distributions of four families: the node facts `distribution.family` (`debian`, `redhat`, `arch`, `suse`) and `package_manager` (`apt`, `dnf`, `pacman`, `zypper`); the base suite, and harness guests for Arch Linux, openSUSE Leap, Rocky Linux, and AlmaLinux; a tool module that does not find its program names the program and what provides it ([the rule](modules.md#where-a-module-works)); a "Tested on" section on every module page and as a column of the [module index](../../reference/modules/README.md); a node that systemd does not run fails at MMX's first SSH command with one message ([servers without systemd](development.md#servers-without-systemd)) | Built. The base suite passes on Arch Linux, openSUSE Leap 16.1, Rocky Linux 9, and AlmaLinux 10, the last two as virtual machines with SELinux enforcing ([testing on several distributions](development.md#testing-on-several-distributions)). An Alpine Linux 3.22 container with OpenRC is refused over both connection kinds and left unchanged ([validation record](../validation.md#servers-that-systemd-does-not-run)). |
| The Red Hat family: the [SELinux label of its path](modules.md#files-get-the-selinux-label-of-their-path) on every new file, directory, and link; `dnf_package`, `dnf_repository`, `firewalld`, `firewalld_zone`, `firewalld_zone_membership`, `selinux_boolean`, `selinux_file_context`, `selinux_mode`, `selinux_port`, and `networkmanager_connection_file`; the fact `selinux.mode`; the notice for a blocked port 7443 when firewalld blocks it | Built and tested on Rocky Linux 9 and AlmaLinux 10 virtual machines with SELinux enforcing: the base suite and the family's module suites pass ([validation record](../validation.md#selinux-and-the-dnf-modules)). |
| The SUSE family: `zypper_package` and `zypper_repository`, as official extra modules in Starlark ([package modules in Starlark](modules-packages.md#package-modules-in-starlark)) | Built and tested on openSUSE Leap 16.1 ([validation record](../validation.md#zypper-package-and-repository-modules)). |
| Fedora 45 with dnf 5: `dnf_package` and `dnf_repository` in the same modules as for dnf 4 ([how dnf 5 differs](modules-packages.md#package-modules-for-each-family)) | Built and tested on a Fedora 45 virtual machine with SELinux enforcing: the base suite, `server_resources.py`, `selinux_labels.py`, `firewalld_and_selinux.py`, and `blocked_port.py` pass; the two NetworkManager suites do not run there, because the image has no NetworkManager ([validation record](../validation.md#fedora-45-and-dnf-5)). |
| `ifupdown_interfaces_file`, a [network configuration module](modules-network.md), with the node's saved copy of the proposed file for an activation that hands both files to its tool | Built and tested on Debian 12 and 13, the distributions whose guests the suite switches to ifupdown ([validation record](../validation.md#ifupdown-interfaces-file-module)). |
| Module pages: one page per core module beside its Go file, joined with the module's description by `go generate ./docs/reference/modules` into [`docs/reference/modules/`](../../reference/modules/README.md) and built into `mmx` only; `mmx describe-module NAME` prints the whole page, `--reference-only` the reference; the [documentation map](../documentation-map.md) and its tests (sections, examples, current pages, links, back-links, nothing in `mmx-runner`) | Built. The Go tests pass. The `diagnostics` suite, which checks `describe-module`, passes on all four target distributions ([validation record](../validation.md#module-pages)). |

### Remaining work for the first release

- Publication. The release candidate passed static analysis, fuzzing, the
  stress run, the platform validation on the tested distributions with the
  release archive, ARM64 on Hetzner Cloud servers, and the macOS controller
  check; the [validation record](../validation.md) states their results.
  What remains is to tag the release and publish it.

### Release checklist

- Release notes: the notes of every minor release carry a hand-written
  section "Incompatible changes" that lists each change a project may need to
  follow, with the edit it needs, or says "none". The notes that GitHub
  generates from commits stay below that section; they do not list the edits
  a reader must make, which [Compatibility between
  releases](#compatibility-between-releases) promises.
- Audit: each spec section is implemented as written (or the spec is updated
  first by agreement), and has a row in the
  [acceptance contracts](../acceptance.md#acceptance-contracts), focused Go
  tests, a real-system workflow in the
  [acceptance checks](../acceptance.md#acceptance-checks), and current-behavior
  text in the [user documentation](../../README.md).
- Real-world workload validation: the selected Ansible scenarios have been run and
  checked, including their failure and recovery behavior, and every
  correctness blocker they found is fixed. The tests, examples, and coverage
  index produced by workload validation are in the MMX repository. The
  coverage index is in
  [workflow coverage](../acceptance.md#workflow-coverage).
- Static analysis and fuzzing, as a release step only. Ordinary changes do
  not run static analysis or fuzzing. `staticcheck`, `errcheck`,
  `exhaustive`, `nilaway`, and
  `govulncheck` report nothing except reviewed exceptions, each with a
  one-line reason beside it. Fuzz tests cover what MMX reads from outside:
  project settings, playbooks, inventories, and roles; the secrets vault file;
  the messages of executable modules and module descriptions; and requests
  to the node HTTPS API. Each fuzz test runs for 10 minutes on a Linux test
  server; a failing input it finds is fixed, and Go keeps
  that input as an ordinary test case. `govulncheck` runs against the
  upstream versions of the dependency copies, as the
  [development design](development.md#when-a-copy-or-the-toolchain-changes)
  describes. The [testing guide](../testing.md#static-analysis-for-a-release) records
  the exact static analysis commands.
  The [fuzzing instructions](../testing.md#fuzzing-for-a-release) give the command
  and explain how to retain failing inputs.
- Every Go test passes under stress: `scripts/run-go-tests-under-stress.sh`
  runs every test 40 times, 4 runs at once, with the race detector. A test
  that fails once is fixed by the [rules in the testing
  guide](../testing.md#go-tests-that-never-depend-on-the-machines-speed),
  never retried or skipped.
- A weekly scheduled job checks for vulnerabilities in the dependencies.
  The weekly job runs `scripts/check-dependency-vulnerabilities.py`
  against the upstream versions of MMX's copies. The weekly job fails,
  and GitHub sends an email, only for an advisory that MMX's code reaches
  and that the affected Go dependency's copy record does not list as fixed.
- Platform validation passes on the
  [tested distributions](development.md#tested-distributions), as the
  [testing rule](development.md#testing-on-several-distributions) says: the
  base suite on every tested distribution, and each tool module's suite on
  the distributions its page lists. The runs use ARM64 and native
  AMD64, with macOS and Linux controllers
  ([testing guide](../testing.md#current-platform-coverage)). ARM64 runs on
  Hetzner Cloud ARM64 servers, and the macOS controller is proven by the
  [macOS controller check](../testing.md#the-macos-controller-check).
- Release builds: one archive for each controller platform with `mmx`, both
  Linux node runner executables, the official extra modules, MMX's license,
  and the license texts and notices that the licenses of the dependency
  copies in `third_party/` require in every copy of the programs; a
  `SHA256SUMS` file;
  a GitHub build attestation for each archive; and the release-artifact
  checks, all as
  [development](development.md#pure-go-implementation-and-dependencies)
  describes.
- Each release build carries its version number, by the scheme in
  [compatibility between releases](#compatibility-between-releases):
  `mmx version` and `mmx-runner` report it, and the official extra modules'
  `shipped_with_mmx_version` file holds it. A development build reports
  `0.1.0-dev`. The controller process installs on each node the runner it
  carries, checked by its SHA-256, and refuses a runner that reports another
  release version than `mmx`, so `mmx` never drives a node runner executable
  of another release; the
  [execution design](execution.md#node-updates-follow-the-controller-release)
  describes the check.
- MMX's license is the Apache License 2.0, in `LICENSE` at the repository
  root. There is no `NOTICE` file and no license header in each source file:
  the license does not require them, and `LICENSE` already says what they
  would say.
- Install and upgrade instructions in the README.
- CI that builds the release matrix and runs the Go tests on each change.
- Deploy a real application with MMX, and fix or record any gaps the
  deployment finds.
- The [Move from Ansible or Salt](../../how-to/move-from-ansible-or-salt.md) guide reviewed against the
  finished build.
- Publication: review the repository and release artifacts for public use,
  then publish the reviewed release. For the first publication, start the
  public history with the reviewed tree as one initial commit.

### Compatibility between releases

MMX's version numbers follow
[semantic versioning](https://semver.org/). The first release is 0.1.0. The
version number of a new release tells a user whether the upgrade can need
an edit to a project:

- **A patch release, such as 0.1.1 or 0.1.2, only fixes faults.** A patch
  release makes no incompatible change, so every project works unchanged
  after a patch upgrade.
- **A minor release, such as 0.2.0 or 0.3.0, may make incompatible
  changes.** A minor release's notes list every incompatible change, with
  the edit that a project needs. New commands, modules, and fields also
  come in minor releases.
- **Version 1.0.0 comes once MMX's names and file formats have settled.**
  From 1.0.0 on, only a new major version, such as 2.0.0, may make an
  incompatible change.

A change is incompatible when, after it:

- a project file that is valid on the earlier release no longer validates,
  or does something else; or
- a command, a flag, or a result field has a new name or is gone.

The project files are the project settings, playbooks, inventories, roles,
the project's own modules, and the secrets vault. MMX starts at 0.1.0 rather
than 1.0.0 because real use by its first users may still show that a name or
a file format should change.

Two rules cover the records that MMX keeps itself. The rules for node records
and controller state hold for every upgrade from 0.1.0 on, minor releases
included:

- **A new node runner executable reads the old node records.** Each
  `mmx-runner` reads the records of owed work that the runners of all
  earlier releases wrote: the
  [command records](execution.md#a-command-that-follows-a-change-has-the-same-record)
  in `/var/lib/mmx/command_records/` and the
  [service records](execution.md#service-reloads-follow-configuration-hashes)
  in `/run/mmx/`. A runner unable to read earlier command or service
  records would treat the unreadable records as missing and
  [take a baseline](execution.md#missing-hashes-establish-a-baseline-before-changes),
  so an owed command or restart would never run, and no result would show
  it. For example, a run writes `/etc/postfix/virtual` and loses its
  connection before `postmap`; after an upgrade, the next run must still run
  `postmap`. Nobody would notice the missed `postmap`, so reading earlier
  node records cannot wait for 1.0.0. Each later release proves compatibility
  with a test in which the new runner reads command and service records
  written by every earlier release's runner.
- **The saved state on the controller machine may be discarded.** Any
  upgrade may change the format of `.mmx/` at the project root: run history,
  node identities, and the runner cache. `mmx` removes nothing itself: a
  command that reads history names each saved run it cannot read and says to
  remove that run directory (`mmx: history cleanup: MMX cannot read this
  saved run ...`). A run interrupted before the upgrade cannot be resumed; running the playbook
  again is safe if the playbook's shell commands are safe to repeat. Each
  node's identity is pinned again through verified SSH at its next
  connection. Keeping run history across upgrades would need migration code
  for each change of its format, for a rare case that a new run already
  covers.

## Beyond the first release

Each spec lists its later work in its own section:
[workflow](workflow.md#beyond-the-first-release),
[configuration](configuration.md#beyond-the-first-release),
[execution](execution.md#beyond-the-first-release),
[modules](modules.md#beyond-the-first-release),
[package modules](modules-packages.md#beyond-the-first-release),
[system modules](modules-system.md#beyond-the-first-release),
[container, database, and certificate modules](modules-applications.md#beyond-the-first-release),
and [development](development.md#beyond-the-first-release). Secondary items
in the linked Beyond the first release sections are built only when real
application deployments show the need.

Open findings from code reviews, which do not gate a release:

- Check service recovery when an interrupted node task block leaves an owed
  reload, the service restarts outside MMX during the node job attempt, and
  a later run reaches only another block of that service in another fleet stage.
  Preserve any owed reload across the new service instance's baseline.
- Make every assertion in the preview checks for files, accounts, and services
  affect the test result, including assertions before the final shell command.
  Prove an intentionally failed earlier assertion fails the test, then run
  the affected suites on Debian 13 and Ubuntu 26.04.
- Audit direct file changes in modules against the persistence helpers, and
  sudo calls against `sshSession.asRoot`. Add a source-level test that rejects
  new bypasses while allowing legitimate direct calls; prove the test fails
  for an added bypass and passes for the checked code.
- Make the asset-selection test fail on `RequiredAssets` errors and compare
  the assets returned by `RequiredAssets` with the expected unique set. An
  omitted or unexpected asset must fail the focused configuration tests.

Not designed yet: macOS as a managed machine, and MMX configuring the machine
it runs on without a network connection.

## What we take from Ansible and Salt

Ansible is the primary model and the habits most users bring: readable ordered
work, inventory, reusable roles, explicit inputs, check/diff, and bounded
rollout. Salt is the model for the node side: MMX's installed node runner
executable and node HTTPS API process resemble a Salt minion, Salt's
node-side program. MMX uses explicit application users, keeps assigned
configuration separate from observed facts, and provides inspectable jobs
with structured results.

Service reloads follow configuration hashes rather than Ansible handlers
(tasks notified by changes) or Salt requisites (dependencies between states).
The saved hashes let MMX recover owed reloads across runs.

## Monkeymaxxing requirements

MMX is the configuration-management tool students use throughout the Monkeymaxxing
teaching. Students should understand what MMX does to the students' servers.
Students should operate and extend MMX through MMX's configuration, commands,
and module contract. Students do not need to understand MMX's internals.

The Monkeymaxxing teaching requirements guide MMX's design: the teaching
focuses on Linux and server systems, with MMX as the tool for applying and
inspecting changes.

The configuration-management tool isn't the subject.

Linux and server systems are the subject.

### Custom module exercise

A particularly useful exercise:

1. Give students something deliberately unsupported.
2. Inspect the MMX module contract.
3. Look at a couple of example modules.
4. Have the students' coding agent implement the missing module.
5. Write/run the new module's tests.
6. Use the new module against the real system.

The lesson isn't merely:

> "Here's how to use MMX."

The lesson is:

> "This isn't magic. Here's how the abstraction works. If it doesn't cover your situation, extend it."

The custom module exercise teaches students that the tooling is subordinate
to the students rather than something mysterious the students must work around.

### Scale progression

The teaching can progressively scale the same basic MMX architecture:

**10 → 100 → 1,000 → 10,000 nodes**

The fundamental mental model remains essentially unchanged.

What changes with scale is primarily:

- concurrency
- resource requirements
- human-interface ergonomics
- the importance of aggregation and exception-focused reporting

Scale should not automatically require a fundamentally more complicated architecture.

The 10,000-node example can also directly demonstrate MMX's efficiency versus traditional Python/Ansible-style controller execution.

The broader lesson:

Server infrastructure really can be dramatically simpler than the accumulated complexity of contemporary tooling makes it appear.
