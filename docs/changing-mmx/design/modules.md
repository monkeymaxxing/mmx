# MMX: modules

Part of the [main plan](README.md). `docs/changing-mmx/design/modules.md` owns the design
shared by every module: module rules, module kinds and contracts, and
decisions that span modules. Each core module's own
behavior, examples, and reasons are on its module page; the
[module index](../../reference/modules/README.md) lists the pages. Four more documents own
the design that a group of core modules shares:

| Document | Owns the shared design of |
|---|---|
| [Package modules](modules-packages.md) | `apt_package`, `apt_repository`, and the language and tool installers such as `pip_package`. |
| [System modules](modules-system.md) | Firewalls, scheduled jobs, the node's name and time zone, swap, filesystems and mounts, SSH key pairs, access control lists, capabilities, and the cleanup of the systemd journal and of old files. |
| [Container, database, and certificate modules](modules-applications.md) | Docker, Podman, PostgreSQL, MySQL, and certbot. |
| [Network configuration modules](modules-network.md) | `ifupdown_interfaces_file`, `netplan_file`, and `systemd_networkd_file`: one native configuration file per node task, its activation, and its restoration when new connections from the controller machine fail; `wireguard_interface`: a WireGuard interface, its node's own private key, and the choice between reload and restart. |

The [user documentation](../../README.md) shows usage, the [examples](../../../examples/) show
working projects,
[Write your own module](../../how-to/write-your-own-module.md) is the guide for
module authors; and [adding a core module](../adding-core-modules.md)
owns code organization and the steps for adding a core module or package manager.

## The core modules

A core module does one job. A module that drives one tool is named for
that tool: `systemd_service`, `apt_package`, `ufw`. MMX has no module
that hides which tool does the work, so a reader of a role sees the tool the
node will run. Where the same mechanism does the work wherever the module
runs, the module has the plain name: `file`, `user`, `git`. The naming rule is also the portability rule
([where a module works](#where-a-module-works)).

The core modules in the following table belong to none of the group documents:

| Module | Manages |
|---|---|
| [`file`](../../reference/modules/file.md) | One regular file from `source`, `template`, `content`, or paths on the node; metadata of an existing file; or explicit removal. |
| [`line`](../../reference/modules/line.md) | One line of an existing file, selected by exact text or one regular expression. |
| [`edit_file`](../../reference/modules/edit_file.md) | The text of an existing file, changed by a filter command the author writes. |
| [`read_file`](../../reference/modules/read_file.md) | Reads one file on the node, so a later node task or another node can use its content. |
| [`download`](../../reference/modules/download.md) | A pinned HTTPS artifact, verified on the controller machine and installed as a file. |
| [`unpack_archive`](../../reference/modules/unpack_archive.md) | A directory extracted from a verified tar or zip archive. |
| [`git`](../../reference/modules/git.md) | A directory checked out from a Git repository at a branch, tag, or commit. |
| [`directory`](../../reference/modules/directory.md) | One directory and its metadata, optionally the metadata of everything inside, a copied source tree, or explicit removal. |
| [`symlink`](../../reference/modules/symlink.md) | One link and its target text. |
| [`user`](../../reference/modules/user.md), [`group`](../../reference/modules/group.md) | Named local accounts, memberships, password hashes, explicit IDs, and removal. |
| [`authorized_keys`](../../reference/modules/authorized_keys.md) | One account's SSH authorized keys. |
| [`systemd_unit`](../../reference/modules/systemd_unit.md) | A complete systemd unit file, or one drop-in. |
| [`systemd_service`](../../reference/modules/systemd_service.md) | Running, enabled, and masked state of a systemd unit, and refresh after tracked changes. |
| [`http`](../../reference/modules/http.md) | One HTTP request from the node, with a response body and HTTP status code. |
| [`wait_for`](../../reference/modules/wait_for.md) | Wait on the node for a TCP port, path, HTTP response, or command to meet a condition. |
| [`assert`](../../reference/modules/assert.md) | A condition that must hold; otherwise the task fails with a message. |
| [`reboot`](../../reference/modules/reboot.md) | Reboot the node and wait for the node. |
| [`shell`](../../reference/modules/shell.md) | Custom commands, as root or another account, with optional guards, dryrun command, and result rules. |

`show` is not a module but a node task form, like `set_vars`, `role`, and
`task_block`: it displays a message or structured value
([contract](configuration.md#show-messages-and-runtime-values)).

The core modules cover the common work on the tested distributions, so
that common work never needs a shell recipe: each core resource module
compares the node with what the task asks for, shows the difference in a
dryrun, and reports unchanged on a second run. Less common tools are
[official extra modules](#official-extra-modules) or the
[project's own modules](#the-projects-own-modules). A task that an existing module and its check already do
well gets no module of its own: a sudo rule is a `file` task with
[`check_new_file`](#check-new-files-before-installing-them).
`mmx describe-module NAME` ([workflow](workflow.md)) prints a core module's
page.

Core resource modules, which manage a resource's desired state, get bounded
automatic retries by default.
Modules that are not core also get bounded automatic retries by default.
Some core modules try once unless the node task sets
`retry`, and some reject `retry` or time limits; the
[retry contract](execution.md#task-retries-have-explicit-conditions-and-limits)
lists the modules in each retry group. Each module's page states the module's
retry and time-limit rules in the section "Retries and time limits".
`retry: false` disables automatic repeats. A node task may set `retry: true`
or a mapping with `max_tries`, `pause_between_tries`, `until`, and `retry_when`.
`max_tries` is a positive integer or `unlimited`; `unlimited` requires a
finite `time_limit`, set on the node task or inherited from
`node_task_defaults.time_limit`. `max_tries` does not accept `null`.
`time_limit` limits the whole node task, including every try;
`time_limit_per_try` limits one try. Project settings in
`mmx_project_settings.yml` use `node_task_defaults.time_limit`,
`node_task_defaults.time_limit_per_try`, `node_task_defaults.retry.max_tries`,
and `node_task_defaults.retry.pause_between_tries`.

The modules that reject time limits, such as `wait_for` and `reboot`, ignore
`node_task_defaults` and bound their wait with their own module input.
The [retry contract](execution.md#task-retries-have-explicit-conditions-and-limits)
owns counts, pauses, conditions, and time limits. Modules own bounded waiting
for an expected condition, such as the dpkg lock, and report what they wait for.

## Where a module works

MMX manages Linux servers that run systemd. Each module's page says,
under "Tested on", on which distributions the module's suite runs. The
[supported platforms](development.md#supported-platforms) own the list of
tested distributions and the testing rule. Modules fall into two groups by
where they work. The groups follow the naming rule of decision D38:

| Group | Rule | Examples |
|---|---|---|
| Plain-named modules | The module does its work through the same mechanism wherever it runs, such as a file renamed into place or a unit that systemd starts. Its page lists where it is tested. | `file`, `directory`, `line`, `user`, `group`, `systemd_unit`, `systemd_service`, `shell`, `http`, `wait_for`, `git` |
| Tool modules | The module is named for one tool and works on every node where that tool is installed. The distribution does not matter. | `apt_package`, `dnf_package`, `ufw`, `firewalld`, `docker_compose_project`, `postgresql_database`, `certbot_certificate` |

Three rules follow from the two groups:

- **A tool module that does not find its tool says so.** The module looks
  for its program before the module's first command. When the program is
  missing, the result names the program and what provides the program on
  each family that has the tool. The task is then a
  [missing prerequisite](#how-a-starlark-script-runs): a run fails without
  another try, and a dryrun reports the task not checked, because an
  earlier node task may install the tool. So `apt_package` on a Fedora node
  fails with one clear sentence, not with a command that failed. The core
  installers and Starlark scripts (`find_program`) already work this way,
  and every tool module follows the same pattern.
- **A difference between distributions stays inside the module that has
  it.** Few modules outside the package modules meet such a difference:
  the path of PostgreSQL's client programs, how `locale` generates a
  locale, and the directory that `ufw` creates for `sshd -T`. Each module
  handles its own difference in its own file, as decision D46 requires. A
  shared table for a handful of values would be a new mechanism with little
  to hold.
- **MMX has no `package`, `service`, or `firewall` module that picks a
  tool by distribution.** A project with several distributions puts a
  condition on the node task, using the
  [distribution facts](configuration.md#facts-and-results-are-observations-not-defaults).

## Rules every module follows

### Protective choices come in three levels

Every module input that makes a node less protected than its default falls
into one of three levels. The level decides what the author must write in
the node task's YAML.

1. **The safe path.** The default, or the value that most tasks write. The
   task writes nothing extra.
2. **A deliberate choice off the safe path.** A legitimate choice that an
   administrator makes on a production server for an ordinary reason, such
   as letting an `admin` group read a file that holds a secret. The task
   states the less protective choice with one plain acknowledgement line
   that names the effect, without judgment words. Without the acknowledgement
   line, validation refuses the task. The validation message says what the
   choice does and which line to add.
3. **Real danger.** The input removes a check against an attacker or against
   silent damage, and no narrower input exists. Only level 3 inputs start
   with `dangerously_`, such as
   [`dangerously_skip_checksum`](#a-file-from-a-web-address-is-checked-against-its-sha-256)
   and `dangerously_skip_server_certificate_check`.

```yaml
- file:
    path: /etc/shop/database.env
    content: "DATABASE_PASSWORD={{ .secrets.shop_database_password }}\n"
    owner: shop
    group: admin
    permissions_mode: "0660"         # group admin may read and change the file
    secret_readable_by_group: true   # level 2: says that group admin reads the secret
```

Without `secret_readable_by_group: true`, validation refuses the task:
the file holds a secret, and mode 0660 lets the group `admin` read the file.
The message names the line `secret_readable_by_group: true`.
With mode 0644, every account on the node
could read the secret; that is level 3, and the line is
`dangerously_let_every_account_read_secret: true`.

The test between levels 2 and 3 is one question: would a careful
administrator choose the input's less protective behavior on a production
server for an ordinary reason? Yes means level 2.
A level 2 name must not read as a mistake, because the choice is not one.

MMX prints no warning at run time once the required acknowledgement line is
written. The acknowledgement line records the choice in YAML and appears in
the reviewer's diff.

A module refuses a value outright only where the module's whole purpose is
the guarantee, and its page then shows the other way. For example,
`create_filesystem_on_empty_device` never formats a device that holds data,
and its page shows how to wipe the device by hand first. Each such refusal
needs a reason that a user would accept, recorded beside it.

**Why:** the safe default protects the reader who did not think about the
choice. A plain required line keeps a legitimate choice easy, and it shows the
reviewer that someone made it on purpose. Keeping `dangerously_` for real
danger keeps that word meaningful: a reviewer who sees it knows that the task
removes a check.

Module pages that rely on this section:
[`authorized_keys`](../../reference/modules/authorized_keys.md),
[`http`](../../reference/modules/http.md),
[`wait_for`](../../reference/modules/wait_for.md),
[`mysql_database`](../../reference/modules/mysql_database.md),
[`mysql_user`](../../reference/modules/mysql_user.md),
[`postgresql_database`](../../reference/modules/postgresql_database.md),
[`postgresql_role`](../../reference/modules/postgresql_role.md),
[`snap_package`](../../reference/modules/snap_package.md),
[`file`](../../reference/modules/file.md),
[`download`](../../reference/modules/download.md),
[`unpack_archive`](../../reference/modules/unpack_archive.md),
[`apt_package`](../../reference/modules/apt_package.md),
[`dnf_package`](../../reference/modules/dnf_package.md).

### A setting with no safe guess is required

A module input gets a default only when one value is right for nearly every
task and a wrong guess is harmless. When the right value depends on what the
task is for, the input has no default and every task writes it. Examples:
`systemd_service` `state` (`running`, `stopped`, or `do_not_start_or_stop`),
`permissions_mode` on a `file` task that writes content, and
`run_as_user_account` on `pip_package`.

A few inputs keep a default because one value is what nearly every reader
expects: a `shell` task runs as root, and a new account's login shell is
`/bin/sh`.

**Why:** coding agents write most MMX configuration, and an explicit line
costs them nothing. People read the configuration, often under pressure, and
a guessed default is invisible to them: a file mode that MMX picked, or a
service that MMX started, does not show on the page. A required input puts
the choice in the node task's YAML, where the reader can see the choice.

Module pages that rely on this section:
[`user`](../../reference/modules/user.md),
[`fstab_mount`](../../reference/modules/fstab_mount.md),
[`ufw`](../../reference/modules/ufw.md),
[`certbot_certificate`](../../reference/modules/certbot_certificate.md),
[`systemd_service`](../../reference/modules/systemd_service.md),
[`systemd_scheduled_job`](../../reference/modules/systemd_scheduled_job.md),
[`docker_compose_project`](../../reference/modules/docker_compose_project.md),
[`postgresql_database`](../../reference/modules/postgresql_database.md),
[`postgresql_role`](../../reference/modules/postgresql_role.md),
[`gem_package`](../../reference/modules/gem_package.md),
[`npm_global_package`](../../reference/modules/npm_global_package.md),
[`pip_package`](../../reference/modules/pip_package.md),
[`file`](../../reference/modules/file.md),
[`download`](../../reference/modules/download.md),
[`line`](../../reference/modules/line.md),
[`apt_package`](../../reference/modules/apt_package.md),
[`dnf_package`](../../reference/modules/dnf_package.md).

### Leaving a setting out never deletes

A task that omits an input leaves what the node already holds for that
input. To delete something, the task writes the deletion:
an empty value or a list of what to remove. For example, a `kernel_module`
task without `options` leaves the module's options file alone, and
`options: {}` deletes the module's options file.

```yaml
- kernel_module:
    name: br_netfilter
    state: loaded
    options: {}   # deletes MMX's options file for br_netfilter
```

**Why:** a missing line is invisible in a review, and so is the deletion that
it would cause. A written deletion shows in the diff. The lists that own a
whole set also require explicit deletion: `<things>_not_listed: remove` is
written in the task
([below](#a-task-that-owns-a-list-says-what-happens-to-the-rest)).

Module pages that rely on this section:
[`kernel_module`](../../reference/modules/kernel_module.md),
[`certbot_certificate`](../../reference/modules/certbot_certificate.md),
[`firewalld_zone_membership`](../../reference/modules/firewalld_zone_membership.md).

### A setting used only at creation says so in its name

Some inputs take effect only when MMX creates something, and MMX never
compares them again. The input names end with the moment the inputs apply:
`_when_created`, `_when_registered`, or `_when_first_installed`. Examples:
`incus_instance` `image_when_created`, `ssh_key_pair`
`key_comment_when_created`, `update_alternatives` `priority_when_registered`,
`certbot_certificate` `account_email_when_registered`, `lvm_logical_volume`
`size: all_free_space_when_created`, and the package version word
`newest_when_first_installed`.

**Why:** a reader who changes such a value expects the node to change. The
name says that the node keeps what it got when it was created, so the reader
knows that a change needs another step, such as recreating the instance.

Module pages that rely on this section:
[`ssh_key_pair`](../../reference/modules/ssh_key_pair.md),
[`update_alternatives`](../../reference/modules/update_alternatives.md),
[`certbot_certificate`](../../reference/modules/certbot_certificate.md).

### env, config, and dir have one spelling

Every name that a user reads, in a module and everywhere else, such as
project settings and results, spells environment, configuration, and
directory as follows:

- `env` names environment variables: `env`, `secrets_from_env`. The whole
  word "environment" stays only for another meaning, such as Python's
  `virtual_environment`.
- `config` stands for configuration: `also_delete_its_config_files`,
  `failure_kind: config`.
- `dir` is written in a label without a verb whose value is a directory
  path: `lock_file_dir`, `compose_project_dir`. Use the whole word
  `directory` when the name contains a verb, uses the plural, or starts with
  the word `directory`:
  `run_from_directory`, `created_parent_directories`, `directory_change`.
- A tool's own name keeps its spelling, such as PostgreSQL's
  `data_directory`.

**Why:** Linux tools write `env` (`env`, `printenv`, `.env` files) and
`config` (`git config`, `~/.config`), and "environment" alone also means a
Python virtual environment or a deployment such as staging. Tools write
`dir` in labels (`--root-dir`, `XDG_RUNTIME_DIR`) and the whole word in
phrases ("home directory", "make parent directories"). With one spelling, a
reader and a coding agent look a name up instead of guessing it.
`TestNamesFollowTheNamingTable` in `docs/reference/modules/generator` checks every
YAML and JSON name of the code and every module input and output. A reviewer
must decide whether a label has a verb.

### A task that owns a list says what happens to the rest

Some tasks hold a list: an account's supplementary groups, a group's members,
an account's authorized keys, a firewall's rules, a database account's
privileges. Each such list has a setting named `<things>_not_listed` beside
it, with no default:

- `remove`: the list in the task is the whole set. MMX removes every entry on
  the node that the task does not list. An empty list removes every entry
  in the set managed by the node task.
- `keep`: MMX adds the listed entries that are missing and leaves every other
  entry alone.

```yaml
- user:
    create_home_directory: true
    name: deploy
    supplementary_groups: [www-data, docker]
    supplementary_groups_not_listed: remove
```

Whenever a node task supplies a managed list, the matching
`<things>_not_listed` setting is required. The setting says in the playbook
whether the supplied list is the complete set.

Core modules use the following settings:

| Core module | Required list setting |
|---|---|
| `user` | `supplementary_groups_not_listed` |
| `group` | `supplementary_members_not_listed` |
| `authorized_keys` | `keys_not_listed` |
| [`ufw`](../../reference/modules/ufw.md) | `incoming_rules_not_listed`, `routed_rules_not_listed`, `outgoing_rules_not_listed` |
| [`file_access_control_list`](../../reference/modules/file_access_control_list.md) | `entries_not_listed` |
| [`mysql_user`](../../reference/modules/mysql_user.md) | `privileges_not_listed` |
| [`postgresql_role`](../../reference/modules/postgresql_role.md) | `member_of_roles_not_listed` |
| [`postgresql_database`](../../reference/modules/postgresql_database.md) | `extensions_not_listed` |

Official extra modules also require an explicit choice between removing and
keeping unlisted entries:

| Official extra module | Required list setting |
|---|---|
| [`incus_instance`](../../../official_extra_modules/incus_instance/README.md), [`incus_profile`](../../../official_extra_modules/incus_profile/README.md) | `config_keys_not_listed`, `devices_not_listed` |
| [`lvm_volume_group`](../../../official_extra_modules/lvm_volume_group/README.md) | `physical_volumes_not_listed` |
| [`mongodb_user`](../../../official_extra_modules/mongodb_user/README.md) | `roles_not_listed` |
| [`rabbitmq_user`](../../../official_extra_modules/rabbitmq_user/README.md) | `permissions_not_listed`, `tags_not_listed` |
| [`pg_auto_failover_monitor`](../../../official_extra_modules/pg_auto_failover_monitor/README.md) | `nodes_not_listed` |

Module pages that rely on this section:
[`authorized_keys`](../../reference/modules/authorized_keys.md),
[`group`](../../reference/modules/group.md), [`user`](../../reference/modules/user.md),
[`linux_capability_on_program_file`](../../reference/modules/linux_capability_on_program_file.md).

### A file from a web address is checked against its SHA-256

Every field that fetches from a web address has a checksum field beside it:
`download`, `unpack_archive.url`, a
[package file or a repository key](modules-packages.md#files-fetched-from-a-web-address) given by web address.
The task gives the file's SHA-256, and the controller machine refuses a file
that does not match. A task may instead say
`dangerously_skip_checksum: true`. The downloaded file is then installed
without a checksum check. If the server at the configured web address starts
serving another file, the change goes unnoticed. So the word "dangerously"
is on the page for the reviewer.
The task must supply exactly one: the SHA-256 field or
`dangerously_skip_checksum: true`. The web address and the SHA-256 may refer
to variables the controller machine knows before it connects, such as a
version number in the project's `vars`:

```yaml
- download:
    url: "https://downloads.example.com/app-{{ .vars.app_version }}-linux-amd64"
    sha256: "{{ .vars.app_sha256 }}"
    path: /usr/local/bin/app
    permissions_mode: "0755"
```

A SHA-256 that refers to a variable must resolve to 64 hexadecimal digits for
every node that runs the task. A value that resolves to empty text is refused
before the run starts, because only `dangerously_skip_checksum: true` skips
the check. A reviewer sees the checksum bypass only when
`dangerously_skip_checksum: true` is written in the task.

For two processor architectures, write two tasks, each with a `when` on
`.node.arch`.

Module pages that rely on this section:
[`download`](../../reference/modules/download.md),
[`unpack_archive`](../../reference/modules/unpack_archive.md).

### Owner and group are an account name or a number

`owner` and `group` on `file`, `directory`, `download`, and `unpack_archive`
take an account name as text, or a YAML integer (or a typed reference to an
integer), which is used as the numeric ID with no account lookup. Text made
only of digits is rejected with a hint to remove the quotes, so one spelling
has one meaning. A number is for a directory that a container's process owns
under an ID that has no account on the node. Prefer a name where an account
exists: a bare number may be the ID of another account on this node, now or
later. To keep the number and still have a name, create the account with that
`uid` and use its name.

Module pages that rely on this section:
[`directory`](../../reference/modules/directory.md),
[`download`](../../reference/modules/download.md), [`file`](../../reference/modules/file.md),
[`unpack_archive`](../../reference/modules/unpack_archive.md).

### Files get the SELinux label of their path

On a node where SELinux is on, every file, directory, and link that MMX
creates or replaces carries the label that the node's SELinux policy gives
its final path, before the entry becomes visible there. MMX asks the policy
for the label with `matchpathcon` and sets the label on the temporary entry
before the rename. MMX has no label settings of its own: no module has an
input for the label of one file. A path that needs another label gets a
rule in the policy from `selinux_file_context`, a module of the Red Hat
family.

SELinux is on when the kernel runs it with a loaded policy: selinuxfs is
mounted and processes have a context other than `kernel`, the rule that
SELinux's own library uses. Some images mount selinuxfs without loading a
policy; nothing there is labeled, the SELinux programs may be missing, and
MMX treats SELinux as off.

The labels are part of what MMX promises on the Red Hat family, not an
extra. Red Hat family servers run with SELinux enforcing as they are
installed. A file with the wrong label can make SELinux refuse a service
access to the file, so without the labels the plain-named modules would not
work correctly there.

Module pages that rely on this section:
[`selinux_file_context`](../../reference/modules/selinux_file_context.md).

## Files and release directories

The [`file` page](../../reference/modules/file.md) owns regular file inputs, secret read
permissions, and [paths on the controller machine](../../reference/modules/file.md#paths-on-the-controller-machine).
The [`download`](../../reference/modules/download.md), [`directory`](../../reference/modules/directory.md),
[`unpack_archive`](../../reference/modules/unpack_archive.md), and [`git`](../../reference/modules/git.md)
pages own their file and checkout behavior.

| Decision | Reason |
|---|---|
| File content writes and every download require `permissions_mode`; metadata-only file tasks may omit it. | Access depends on the file's purpose, so no mode is a safe guess; see [required settings](#a-setting-with-no-safe-guess-is-required). |
| Known secret content uses the file page's read-bit acknowledgements, also for `download` and `save_on_controller_machine`. | An application group may need a secret in ordinary operation; public reading removes the protection. See [protective choices](#protective-choices-come-in-three-levels). |
| Literal modes of content known to read a secret are checked while loading; node checks remain for values known during execution. Refusals identify the path, mode, and readers. | Refusing before dispatch prevents earlier tasks from changing nodes before a predictable failure. The [file page](../../reference/modules/file.md#files-that-hold-secrets) owns the timing and permission rule. |
| Relative controller-machine paths may leave a role directory while staying inside the project. | Shared project files need no duplicate copies; self-contained roles remain recommended for portability. |
| A present `line` task requires one placement setting. | A line that is missing must have an explicit destination. An implicit append can put the line in the wrong section. |
| Archive unwrapping refuses every entry outside the one top-level directory; removal requires `also_delete_everything_inside: true`. | Unwrapping must not silently omit release files, and deletion must visibly name the whole tree. |
| Directory cleanup accepts owner, group, and mode for the directory itself. | Cleanup and metadata concern the same directory. Kept entries retain their metadata. |
| Git uses exactly one of `follow_branch`, `check_out_tag`, and `check_out_commit`; each searches only its namespace. | A branch and tag may share a name; following updates and selecting a release must be distinct. |
| Git uses `run_as_user_account`, `ssh_private_key`, `ssh_known_hosts_lines`, and `files_changed_in_checkout_on_node`. | Each name states the account, credential contents, or checkout whose files the setting concerns. |
| `full_history: true` deepens an existing shallow checkout even at an unchanged commit. | Tools that read Git history need full history independently of a code update. |
| `edit_file` and `read_file` retain their 1 MiB content limit. | File content handled by `edit_file` and `read_file` travels inside the node job and is saved with the playbook run. |

## Packages

The [package modules](modules-packages.md) document owns `apt_package`,
`apt_repository`, and the
language and tool installers. Two rules from `docs/changing-mmx/design/modules-packages.md`
matter to every node task block. A package task in `changes` is a tracked
input, so a changed package version restarts the block's service. A package
[never starts its services](../../reference/modules/apt_package.md#packages-do-not-start-their-services)
while it installs, so the block's `systemd_service` task is what starts them.

## Check new files before installing them

`file`, `download`, `line`, and `edit_file` accept an optional `check_new_file`
with a `shell` check. MMX writes the new contents to a private temporary file
on the node, runs the check, and atomically replaces the destination only if
the check passes. A rejected new file
leaves the live file and its metadata unchanged and fails the task. A declared
check runs on every apply, including when the file is unchanged, so resuming a
pending activation still requires the check to pass.

Within the check, `.new_file.path` names the temporary file; pass it through an
`env` field so the script stays literal. The check runs as root, since the
new file is root-private, and must not modify the new file or managed
configuration. During dryrun, only the check's explicit `dryrun` command runs.
Without an explicit `dryrun` command, validation is reported as not checked.
Validation is also not checked when the command is unavailable (exit
126/127) or the destination directory is missing. For any other nonzero
exit, any earlier node task that would change the node takes precedence:
the dryrun reports the file task as not checked. Otherwise, the nonzero exit
rejects the new file. The
[execution design](execution.md#node-task-blocks-complete-the-change)
owns the precedence rule.

MMX removes any temporary file left after the check and replacement.
Secret suppression applies to the temporary file's contents and the check's
output.

For example, check a new configuration file before installing it:

```yaml
- file:
    permissions_mode: "0644"
    path: /etc/myapp/config.yml
    source: files/config.yml
    check_new_file:
      shell:
        script: /usr/local/bin/myapp --check-config "$NEW_FILE"
        env:
          NEW_FILE: "{{ .new_file.path }}"
```

The [validation contract](execution.md#node-task-blocks-complete-the-change)
covers both file checks and node task block validation. A file check can be
the only check for a single-file change. A node task block's `validate` phase
checks combined configuration before activation.

Module pages that rely on this section:
[`download`](../../reference/modules/download.md),
[`edit_file`](../../reference/modules/edit_file.md), [`file`](../../reference/modules/file.md),
[`line`](../../reference/modules/line.md).

## Resource ownership must be obvious

Removing a task from YAML does not delete its former resource; delete with an
explicit `state: absent`. MMX removes something a task does not name only when
that task says, in words on the page, that it holds the whole set:
`<things>_not_listed: remove`
([rule](#a-task-that-owns-a-list-says-what-happens-to-the-rest)) or
`delete_everything_inside_except`
([`directory`](../../reference/modules/directory.md#delete-a-directory-or-what-it-no-longer-lists)). Manage parts of a
system through native boundaries: a systemd drop-in, an apt source file, an
application include directory, one authorized key, one firewall rule. Where a
program offers none, [`line`](../../reference/modules/line.md) manages one
line and [`edit_file`](../../reference/modules/edit_file.md) changes other
text; MMX has no module for a marked block of lines.

A cleanup that happens once, such as removing what a migration left behind,
is a [one-time playbook](workflow.md#one-time-playbooks), not a removal line
left in a role.

Module pages that rely on this section:
[`directory`](../../reference/modules/directory.md),
[`edit_file`](../../reference/modules/edit_file.md), [`line`](../../reference/modules/line.md).

## Every module follows the same contract

A module must:

1. Validate the module's inputs and identify the resource the module owns.
2. Inspect real state and distinguish unchanged from required work.
3. Support dryrun without applying changes, or report what the module could
   not check.
4. Reconcile correctly when called again after interruption.
5. Report success or failure separately from whether anything changed.
6. Explain a failure and keep the changes the module already made.

Results carry a `status` (`succeeded`, `failed`, `skipped`, `blocked`,
`stopped`, `not_checked`) and a `change` (`unchanged`, `changed`,
`would_change`, `unknown`); [`protocol.Result`](../../../internal/protocol/protocol.go)
defines the fields. A failure can carry a partial change. An interrupted
module execution can still report a known change. A started module
executable that leaves no valid result has `change: unknown`, never
`unchanged`.
Within a node task execution, including retries, a later
unchanged try does not erase changes reported by earlier tries.

## Module kinds: core, official extra, and the project's own

Every node task that does work names exactly one module, by the module's
name, and writes the module's **inputs** below that name. A module has one
of three kinds:

| Kind | What it is | Where it lives |
|---|---|---|
| Core module | Go code that is built into the node runner executable, `mmx-runner` | In MMX itself |
| [Official extra module](#official-extra-modules) | A Starlark script that the MMX project writes and tests | In `official_extra_modules/` of an MMX release, installed beside `mmx` on the controller machine |
| [The project's own module](#the-projects-own-modules) | A Starlark script, or any executable, that the project's people write | In `external_modules/` of the project, beside `mmx_project_settings.yml` |

Starlark is a small language that reads like Python. A Starlark script has
no way to open a file, a network connection, or a program by itself: it can
touch the node only through the functions that MMX gives it, the
[primitives](#primitives).

A node task calls its module in the same way, whatever the module's kind:

```yaml
# playbooks/set_up_web.yml
fleet_stages:
  - id: web_servers
    nodes: web
    node_tasks:
      - id: nginx_config
        file:                                  # a core module
          path: /etc/nginx/conf.d/shop.conf
          source: files/shop.conf
      - id: test_container
        incus_instance:                        # an official extra module
          name: shop-test
          state: running
          image: images:debian/13
      - id: greet_barbara
        greeting:                              # a module of the project's own
          path: /tmp/greeting.txt
          person: Barbara
```

The first node task names the core module `file`. The second names
`incus_instance`, an official extra module that MMX ships for Incus. The
third names `greeting`, a module that the project's own people wrote in
`external_modules/greeting/`: it makes sure the file `/tmp/greeting.txt`
holds the line `Hello, Barbara!`.

- **The name.** A module's name uses lower-case letters, digits, and
  underscores, starts with a letter, and has at most 64 characters, such as
  `greeting`.
- **The lookup.** MMX looks a name up among the core modules first, then
  among the official extra modules, then among the project's own modules in
  `external_modules/`. A name absent from the core modules, official extra
  modules, and project's own modules is a validation error
  that says where MMX looked.
- **One module of each name.** A module of the project's own that has the
  name of a core module or of an official extra module is a validation
  error, also when no node task calls it; the message says to give the
  module another name. A project's own module also fails validation when the
  module has the name of a node task setting,
  such as `when`, `loop`, `retry`, `role`, or `task_block`.
- **The kind is not written in the configuration.** Every module is called
  by writing the module's name with the inputs below, so a module can move
  from one kind to another without a
  change to anyone's playbooks or roles: an official extra module that many
  projects need can become a core module. `mmx module-usage`,
  `mmx describe-module NAME --reference-only`, and `mmx explain` show the kind
  instead: [`mmx module-usage`](workflow.md#which-modules-a-project-uses)
  lists the modules a project uses, with their kind, their files, and every
  node task that calls them; `mmx describe-module NAME --reference-only`
  shows one module with its kind and its files; and `mmx explain` shows the kind beside each
  node task whose module is not core. `mmx explain --json` names the kind of
  every node task that calls a module as `module_kind`: `core`,
  `official_extra`, `project_starlark`, or `project_executable`. The results
  of a run name `module_kind` only for a module that is not core.

### Where a module belongs

**Common work goes in core.** Official extra modules are only for work that
is truly uncommon on the tested distributions: Incus, libvirt, ZFS, disk
partitions, LVM, RabbitMQ, MongoDB, PostgreSQL's failover with
pg_auto_failover, and its backups with pgBackRest. Commonness is judged
from the beta validation, which ran real Ansible projects through MMX, and
from the
experience of the people who run servers. When unsure, the module goes in
core.

The kind decides the language: a core module is Go, and an official extra
module is a Starlark script. The distribution does not decide the kind.
Common, intricate work, such as `apt_package` with its lock waits, version
rules, and checks across a fleet stage, is a core module in Go, because
Go's compiler and the release's static analysis check it, and most users
depend on it.

One exception holds for package modules. A package module for a family
other than Debian and Red Hat is an official extra module, such as
`pacman_package` for Arch Linux and `zypper_package` for SUSE, although a
package manager is common work on its own family. A Starlark package
module passes the same
[module behavior contract](#every-module-follows-the-same-contract) as a
Go one. It is also the script that users copy to write a package module for
their own distribution, and it needs only one primitive of its own,
`run_command_that_refreshes_a_package_index`, which serves every package
manager. `apt_package` and `dnf_package` stay core modules in Go.
[Package modules in Starlark](modules-packages.md#package-modules-in-starlark)
says what a Starlark package module gets and lacks.

Core modules and official extra modules follow the
[module behavior contract](#every-module-follows-the-same-contract)
and are tested on the distributions that their pages list under "Tested
on". The difference is how MMX
ships them: a core module changes only with a new build of MMX, and an official
extra module is a script that MMX reads from a directory.

### Primitives

The **primitives** are the functions through which a module reads and
changes the node: reading a file, writing a file, running a program, looking
up an account, sending an HTTP request. A Starlark script touches the node
only through MMX's primitives. A core module calls primitives, or the shared
code that implements the primitives, for most of the core module's work.
Some core modules also read or write files directly where no primitive
fits, such as a login file in the node task's
[private directory](execution.md#a-node-tasks-private-directory-lives-in-one-place-that-the-node-empties).
So a core module and a script that do the same work read alike.

The primitives also give MMX its dryrun guarantee. A **changing primitive**
never changes the node during a dryrun: it inspects the node and reports
what it would change. MMX sees every change a module makes through changing
primitives, so MMX itself records each change, builds the difference that a
dryrun shows,
and computes the fingerprint that tells a node task block's
`systemd_service` task whether to restart
([tracked inputs](execution.md#node-task-blocks-complete-the-change)).

Primitives that read the node, which also run during a dryrun:

| Primitive | What it does | What it returns |
|---|---|---|
| `read_file(path)` | Reads a text file of at most 1 MiB | `content` |
| `inspect_path(path)` | Looks at a path without following a final link | `exists`, `type` (`file`, `directory`, `link`, or `other`), `owner`, `group`, `permissions_mode`, `link_target`, and for a file `sha256` |
| `look_up_account(user_account)` | Reads `/etc/passwd` and the groups of the account | `exists`, `user_id`, `group_id`, `home`, `shell`, and `groups` |
| `find_program(program_name)` | Looks a program up as `run_command` would find it | `found`, and `path` |
| `run_command(arguments, standard_input = "")` | Runs a program that only reads, as root. `run_command` runs no shell: `arguments` is a list, such as `["incus", "list", "--format", "json"]` | `exit_code`, `standard_output` (at most 1 MiB), and `standard_error` (at most 1 MiB) |
| `http_request(method, url, headers = {}, body = "", certificate_authority_file = "")` | Sends one HTTP or HTTPS request. `http_request` follows no redirect and always verifies HTTPS certificates. A `GET` or `HEAD` request reads; any other method is a change | `http_status_code`, `headers`, and `body` (at most 1 MiB) |
| `parse_duration(duration)` | Parses duration text with Go's `time.ParseDuration`, also used by `wait_for.wait_at_most` and `reboot.wait_at_most`; the [module interface reference](../../reference/module-interface.md#primitives) lists the result fields | `seconds` (a number), and `error` |
| `sha256(text)` | Hashes text | The SHA-256 in hexadecimal |
| `is_dryrun()` | Says whether the current playbook run is a dryrun | `True` or `False` |
| `earlier_node_task_would_change(path)` | In a dryrun, predicts whether an earlier node task in that dryrun would create or change `path`, an enclosing directory, or a child path. `earlier_node_task_would_change` knows paths managed by core modules, such as a `file` task's path, but cannot predict another script's writes | `True` or `False`; always `False` in a run |

`earlier_node_task_would_change` does not infer arbitrary scripts' writes;
no official extra module depends on reading a file written by another script.
Core-module path tracking supplies the dryrun predictions the official
extra modules currently need.

Changing primitives, which never change the node during a dryrun:

| Primitive | What it does | What it returns |
|---|---|---|
| `write_file(path, content, owner = "root", group = "root", permissions_mode = "0644")` | Makes the file hold `content` with the requested owner, group, and mode. `write_file` writes a new file beside the old file, then renames the new file into place, so a reader never sees half a file, and gives the file the SELinux label of its path | `changed` |
| `make_directory(path, owner = "root", group = "root", permissions_mode = "0755")` | Makes the directory exist with that owner, group, and mode; the parent must exist | `changed` |
| `make_link(path, target)` | Makes `path` a symbolic link to `target` | `changed` |
| `remove_path(path)` | Removes a file, a link, or an empty directory | `changed` |
| `run_command_that_changes_the_node(arguments, standard_input = "")` | Runs a program that changes the node, as root, with no shell. During a dryrun MMX does not run the program and records that the program would run | `ran`, `exit_code`, `standard_output`, and `standard_error` |
| `run_command_that_refreshes_a_package_index(arguments)` | Runs a program that downloads a package manager's package index, such as `pacman -Sy`, as root, with no shell. The index is a cache of what the package servers offer, so the refresh is no change of the task: the refresh adds nothing to the task's change, difference, or fingerprint, as `apt_package`'s own index refresh does not. During a dryrun MMX does not run the program | `ran`, `exit_code`, `standard_output`, and `standard_error` |
| `http_request` with a method other than `GET` and `HEAD` | Sends the request. During a dryrun MMX records the proposed HTTP request without sending the request | As above, and `sent` |

- In a dryrun, `changed` says whether the call would change the node. MMX
  compares the file's content, owner, group, and mode with what the call
  asks for, so the answer is exact.
- Each primitive returns a result with an `error` text, which is empty when
  the call worked. A failed call does not stop the script, so the script
  decides what the failure means.
- A command runs as root, in `/`, with this environment:
  `PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin`,
  `LANG=C.UTF-8`, `USER=root`, `LOGNAME=root`, and root's home directory as
  `HOME`. A tool that keeps its settings in the home directory, such as a
  client program, then finds root's.
- A module that reads a file which another node task writes, such as a
  definition from a template, asks `earlier_node_task_would_change` for the
  file in a dryrun. When the answer is `True`, the file on the node is not
  what a run would find, and the module reports what follows from a
  changed file instead of comparing the old one. The node task execution
  engine also tells a core module whether an earlier node task would change
  the path.
- A module asks `find_program` for its tool before its first command. A
  missing tool is then a
  [missing prerequisite](#how-a-starlark-script-runs), not a command that
  failed.
- `run_command` is for programs that only read, such as `incus list`. MMX
  cannot see what a program does, so it runs `run_command` also during a
  dryrun, and the author is responsible for keeping those programs read
  only, as with the `dryrun` command of a [`shell`](../../reference/modules/shell.md#the-dryrun-command)
  task. A program that changes something goes through
  `run_command_that_changes_the_node`.
- The `error` of `http_request` never holds the address of the request,
  because an address can hold a secret. The HTTP error text can name the
  host that MMX could not reach, and never the path or the query behind the
  host.
- A script also has `json.encode(value)` and `json.decode(text)`.
- A core module may use primitives that no script needs yet, such as
  changing an account. A new core module that needs another way of touching
  the node adds a primitive, and every script can then use it too.

### How a Starlark script runs

The node job worker process runs the script itself, once for each node task
and once for each item of a loop, with the node task's inputs after MMX
resolved their references and secrets. The script runs as root, like every
module, because the script changes the node.

```python
# external_modules/greeting/module.star
def run_node_task(inputs):
    greeting = "Hello, " + inputs["person"] + "!\n"
    written = write_file(inputs["path"], greeting)
    if written.error:
        return {"status": "failed", "reason": "MMX could not write the greeting: " + written.error}
    return {"outputs": {"greeting": greeting}}
```

- A script defines the function `run_node_task(inputs)`. MMX calls
  `run_node_task` with the inputs as a dictionary, in a dryrun as in a run:
  the changing primitives make the difference.
- The function returns a dictionary, or `None` for success without outputs.
  `status` is `succeeded` (the default), `failed`, `not_checked`,
  `missing_prerequisite`, or `invalid_inputs`. `failed`, `not_checked`,
  `missing_prerequisite`, and `invalid_inputs` need a `reason`. A successful
  result may also give a `reason` describing what the node task did.
  `outputs` holds the module's named outputs.

  A later node task reads an output beside the node task's other result
  fields, as the later node task reads the `public_key` of an `ssh_key_pair`
  task:
  `.node_tasks.greet_barbara.greeting`. Moving a module between kinds
  therefore changes nothing for the node tasks that read its results. A
  later node task gets only the outputs returned by the called module's
  node task. In a dryrun, a node task that reads an output the module left
  out, or an output of a node task that did not succeed, is `not_checked`.
  The reason names the node task that did not supply the output.

  An optional `fingerprint` is text that MMX adds to the fingerprint it
  computes, for state that no changing primitive keeps, such as an instance
  that a command created.
- **`missing_prerequisite`** says that something an earlier node task may
  still provide is not on the node, such as the tool's program, or that the
  node is in a state that an earlier node task may still change, such as a
  volume that is still mounted. MMX decides what a missing prerequisite
  means: a run fails without another try, because the node does not change
  between tries. A dryrun reports the task `not_checked`, because an earlier
  node task of the run may install the tool or change the state.
- **`invalid_inputs`** says that the task cannot succeed as it is written,
  and that another try would end the same way. The node task's inputs break
  a rule that the module description cannot state, such as two inputs that
  exclude each other. Inputs may instead be incompatible with the node's
  state, such as a login that the tool's server refuses or a volume that
  would have to shrink. The task fails without another try, in a dryrun as
  in a run. `failed` is for what another try may fix, such as a command that
  failed.
- **`try_again: False` beside `failed`** is for a failure after the script
  changed the node, when another try would find the changed state and
  report success: a dataset that ZFS created and then could not share. MMX
  then does not try again, so the failure stays in the result. The next
  run of the playbook inspects the resource state left on the node.
  The ZFS module compares the dataset's properties.
- **A reason that the script chose stays private with the task.** MMX
  shows the reason of a missing prerequisite and of invalid inputs also for
  a task that uses secrets, because MMX writes those reasons itself for a
  core module. A script writes its own, and its reason can hold the value
  of an input. So when an input that is no
  [private value](#the-module-description) holds a secret, the script's
  reason stays hidden.
- **`not_checked`** is for a dryrun that cannot tell what a run would do,
  such as while the tool's server does not answer yet. A run cannot leave a
  task not checked: during a normal run, the script returns `failed`.
- **MMX decides the change.** The task reports `changed` when a changing
  primitive changed the node, `would_change` in a dryrun when one would
  have, and `unchanged` otherwise.

  A changing command may fail after starting, or a changing request may fail
  after reaching the server, for example at a time limit. Such a call may
  have changed the node. If any primitive changed the node, the task
  reports `changed`. Otherwise, the task reports `unknown`.
  The difference names the failed call.

  A normal exit from `run_command_that_changes_the_node`, including a
  nonzero exit that the tool uses for refusal, counts as `changed`.
  MMX cannot see whether the tool changed anything before refusing.
  An interrupted changing command instead leaves the task's change
  `unknown`, unless another primitive already changed the node.
  A script never reports its own change.
- **MMX builds the difference.** A dryrun and `--diff` show, for each
  changing primitive call that changes something, the file's content
  difference and its owner, group, and mode, the directory or link, the
  removal, or the command or request that would run.
- **MMX computes the fingerprint** from every file, directory, and link that
  the script's changing primitives keep, changed or not, and from the
  script's own `fingerprint`. A script in a node task block's `changes` is
  therefore a tracked input like a `file` task.
- **Limits.** A script ends when it has taken 10 million steps, far more
  than a module needs. The node task's
  [time limits](execution.md#task-retries-have-explicit-conditions-and-limits)
  apply as to every module; at the limit, MMX ends the script and every
  program it started.
- **Errors.** A script that fails with an error, such as a key that the
  dictionary does not have, fails the node task with one line that names
  the script, the line, and the column: `the script of the module greeting
  ended with an error: module/greeting/module.star:3:12: key "person" not in
  dict`. The message names the script `module/<name>/module.star`, whatever
  the module's kind and wherever its files are.
- `print(...)` adds a line to the node task's result, like the diagnostics
  of an executable. MMX keeps the first 8 KiB.
- A script has nothing else: it cannot load another file of code, and it
  cannot reach a file, a program, or the network except through a
  primitive. Starlark has no `while` and no recursion.

Official extra modules and scripts of the project's own are trusted code, as
every module is: MMX limits their steps and time, and does not lock them
away from the node.

### Official extra modules

An official extra module is a Starlark script that the MMX project writes,
maintains, and tests on the distributions that its page lists under "Tested
on".

```text
official_extra_modules/
  shipped_with_mmx_version       one line: the version of the MMX release
  incus_instance/
    module.star                  the script
    module_description.yml       the module's inputs, outputs, and examples
    README.md                    how to set the module up, and what its errors mean
  incus_profile/
    ...
```

- **The same release, the same version.**
  `official_extra_modules/` is in the MMX repository and in every release,
  beside `mmx` and the node runner
  executables, under the same version number. Official extra modules are
  never released by themselves.
- **`official_extra_modules/` is installed with MMX on the controller machine,**
  in the same directory as `mmx`. A run sends each node only the files of the
  modules that the run uses. The node keeps module files by their SHA-256 in
  `/var/lib/mmx/kept_files/`, so an unchanged file is not sent again.
- **The official extra modules' release version must match the `mmx` version.**
  When `mmx` starts a command that reads a project, `mmx` compares the version
  in `shipped_with_mmx_version` with the program's version. With different
  versions, `mmx` refuses to run. The error names the official extra modules'
  release version and the `mmx` program's version:
  `the official extra modules in /usr/local/lib/mmx/official_extra_modules
  were shipped with MMX 0.1.0, but the mmx program is version 0.2.0;
  reinstall the mmx program and official_extra_modules/ from a single MMX
  release`. Comparing
  `shipped_with_mmx_version` with the `mmx` program's version is the only
  version check of the official extra modules; the node runner executables
  have [their own](execution.md#node-updates-follow-the-controller-release).
  A controller machine without `official_extra_modules/`
  works as long as no node task names an official extra module; the error
  for an unknown name then
  says that the directory is missing.
- **A fix is a script change.** Adding an official extra module or changing
  one needs no change to the Go code and no new build of `mmx-runner`. The
  binaries change only when a core module or a primitive changes.
- **Nobody edits an official extra module,** and nobody copies
  an official extra module into a project. A team that wants a module that
  works differently writes a module of its own under another name.
- **Nothing is downloaded during a run.** A saved run keeps the module
  files it used, so `mmx resume` uses the same files.

The official extra modules of the first release are for Incus
(`incus_instance`, `incus_profile`), libvirt (`libvirt_domain`), ZFS
(`zfs_dataset`), disk partitions (`create_partitions_on_empty_disk`,
`grow_partition_into_free_space_after_it`), LVM (`lvm_volume_group`,
`lvm_logical_volume`), filesystem growth (`grow_filesystem_to_fill_device`), RabbitMQ (`rabbitmq_virtual_host`, `rabbitmq_user`,
`rabbitmq_policy`, `rabbitmq_plugin`), MongoDB (`mongodb_user`),
pg_auto_failover (`pg_auto_failover_monitor`, `pg_auto_failover_node`,
`pg_auto_failover_primary`), and pgBackRest (`pgbackrest_stanza`,
`pgbackrest_restore`).
[The official extra modules](modules-official-extra.md) owns the inputs and
the behavior of each. `pacman_package`, the package module of Arch Linux,
is an official extra module too, as every package module for a family
other than Debian and Red Hat is;
[package modules](modules-packages.md#pacmans-own-rules) owns its
design.

### The project's own modules

A team writes a module of its own for work that no core module and no
official extra module does. The module is a directory in `external_modules/`
at the project root, named after the module:

```text
external_modules/
  greeting/
    module.star                  the script
    module_description.yml       the module's inputs, outputs, and examples
```

**Write a module as a Starlark script.** A script has the same primitives and
the same powers as an official extra module. It needs nothing on the node but
MMX, MMX checks a node task's inputs against the module's description before
anything runs, and the script gets the dryrun guarantee, the difference, and
the fingerprint from MMX. Most modules of a project are written by a coding
agent, and a script is the easiest kind to write, read, and review.

**An executable is the way out for a special need,** such as a library that
exists only in another language. The directory then holds the file
`module_executable` in place of `module.star`: a Bash script, a Go or Rust
binary, or anything else that runs on the node. A compiled module can hold
`module_executable_linux_amd64` and `module_executable_linux_arm64` instead,
but cannot combine `module_executable` with architecture-specific
executables. MMX picks the node's build. Supporting files may sit in the
module's directory beside the executable. MMX starts the executable and
speaks the [JSON protocol](#executables-and-the-json-protocol) with the
executable.

MMX cannot see what an executable changes, so an executable gets no dryrun
guarantee: it must keep its own dryrun promise, and it reports its own
change, difference, and fingerprint. `mmx module-usage` says so beside every
executable. The executable also needs whatever it uses on the node, such as
an interpreter.

The [module interface reference](../../reference/module-interface.md) is the authoring reference.

Module pages that rely on this section:
[`shell`](../../reference/modules/shell.md).

### The module description

Every module has a **module description**: its inputs, its outputs, and
examples. The description of a core module is built into MMX. An official
extra module and a module of the project's own keep their module description
in the file `module_description.yml` beside the script or the executable.

```yaml
# external_modules/greeting/module_description.yml
summary: Makes sure a file holds a greeting for one person.
inputs:
  path:
    type: string
    required: true
    description: The absolute path of the file that holds the greeting.
  person:
    type: string
    required: true
    description: The name of the person to greet, such as Barbara.
outputs:
  greeting:
    type: string
    description: The greeting the file holds, such as "Hello, Barbara!" and a newline.
examples:
  - description: Greet Barbara in /tmp/greeting.txt.
    config: |
      greeting:
        path: /tmp/greeting.txt
        person: Barbara
```

| Field | Meaning |
|---|---|
| `summary` | Required. One sentence that says what the module does. |
| `inputs` | The module's inputs by name. Each input states its `type`, whether it is `required`, and a `description`. An optional `allowed_values` lists the values the input may have. An optional `private_value: true` marks a text input whose value is private, such as a password. |
| `outputs` | The module's named outputs, which later node tasks read as `.node_tasks.<id>.<name>`. Each output declaration states the output's `type` and a `description`. No output may be named `status`, `change`, `reason`, `items`, or `outputs`, which results and role calls already use. |
| `examples` | At least one example: a `description` and the `config` of one call, the module's name with its inputs. |

`type` is `string`, `integer`, `number`, `boolean`, `list_of_strings`,
`mapping_of_strings`, or `any`. `any` takes lists and mappings of any shape,
such as the devices of an Incus instance, and the module checks their form.
A core module's description has the same shape and also types that only core
modules use, such as `duration` and a list of mappings with their own
inputs; it also says how the module behaves in a dryrun.

- **A private value does not hide the task's result.** A secret in an input
  without `private_value: true` makes the whole node task result private:
  reason, difference, and outputs. A failure then says only "details
  suppressed for a task using secrets". An input with `private_value: true`
  works as the password of a core module does. A secret in a private-value
  input leaves the result visible, with `[redacted]` wherever result text
  contains the secret value.

  The module must hand the private value only to a program, through the
  program's standard input. Command arguments show in the node's process
  list. MMX cannot replace a form of the value that the module computes.
- **`mmx validate` checks every node task against the description of its
  module,** before anything runs: an input that the module does not have, a
  required input that is missing, a value of the wrong type, and a value
  that is not allowed are validation errors naming the node task's YAML
  file and line. A value that a reference supplies is checked on the node,
  after MMX resolves the reference and before the module runs.
- **`mmx describe-module NAME` prints the module's page.** With
  `--reference-only`, `mmx describe-module NAME` prints the description in
  the same form for every module, with the module's kind and the place of
  its files. A module of the project's own prints that description also
  without the option, before its `README.md`
  ([workflow](workflow.md#look-up-a-module)).

  Without a name, `mmx describe-module` lists every module by kind with a
  summary. When `mmx describe-module` runs inside a project, the list also
  includes the project's own modules. `--json` gives the requested module
  documentation or module list as structured data for a program.
- **A result must keep to the description.** An output that the description
  does not list, or one of the wrong type, fails the node task with a
  message that names the output.
- **An example must be valid.** MMX checks each example against the module's
  own description when it reads the description.

### Executables and the JSON protocol

MMX starts an executable once for each try of its node task, as a child
process of the node job worker process, as root, in a private copy of the
module's directory. MMX writes one JSON object to the executable's standard
input:

```json
{"protocol": 1, "mode": "apply",
 "inputs": {"path": "/tmp/greeting.txt", "person": "Barbara"}}
```

`mode` is `check` in a dryrun and `apply` in a run:

| Mode | Required behavior |
|---|---|
| `check` | Inspect without changing managed resources; report unchanged, would change, or `not_checked` with a reason. |
| `apply` | Inspect, make needed changes, and report the actual outcome. Already-correct state is unchanged. `apply` must not depend on an earlier `check`. |

The executable writes one JSON object to its standard output, and
diagnostics to its standard error:

```json
{"protocol": 1, "status": "succeeded", "change": "changed",
 "outputs": {"greeting": "Hello, Barbara!\n"},
 "diff": "-Hello, Ann!\n+Hello, Barbara!\n",
 "fingerprint": "c0535e4be2b79ffd93291305436bf889314e4a3faec05ecffcbb7df31ad9e51a"}
```

| Field | Meaning |
|---|---|
| `protocol` | Required. The number `1`. |
| `status` | Required. `succeeded` or `failed` in any mode. Only `check` may report `not_checked`. |
| `change` | Required. `unchanged`, `changed`, `would_change`, or `unknown`. `check` never reports `changed`, and `apply` never reports `would_change`. |
| `reason` | A text that explains the result. Required for `failed` and `not_checked`. |
| `outputs` | The named outputs, with the names and types the description lists. |
| `diff` | Text that shows what changed, or would change. A dryrun and `--diff` show the difference text. |
| `fingerprint` | Text that changes when the managed state changes, such as a hash of the managed state. Required when the node task is in a node task block's `changes`, where the node task is a tracked input. |
| `secret` | `true` marks the result private. The executable's `secret` field cannot clear sensitivity that MMX assigned. |

- Exit status 0 means that the executable produced a result. A nonzero exit
  is never success, but MMX preserves a partial change reported by a valid
  result. Missing, malformed, or oversized output, an unknown field, a
  key written twice, another protocol number, and a status and change that
  do not fit the mode fail the node task.
- Requests are limited to 16 MiB, results to 1 MiB, and JSON nesting to 64
  levels; module files travel separately and do not count.
- The executable gets a fixed `PATH`, `LANG=C.UTF-8`, `USER=root`, and
  `MMX_MODULE_HELPER`, the path of the node runner executable for the JSON
  helper below.
- Returned values cannot use MMX's reserved `$mmx_secret`, `$mmx_truncated`,
  or `$mmx_binary` keys.

A Starlark script gets the executable protocol's inputs as the argument of
its function. The Starlark script's result is a dictionary with only the
keys `status`, `reason`, `outputs`, `fingerprint`, and `try_again`
([how a Starlark script runs](#how-a-starlark-script-runs)); MMX knows
`protocol`, `change`, and `diff` itself. Only an executable can mark its
result `secret`.

### JSON helper for Bash executables

`mmx` and `mmx-runner` provide the `module-json` helper. `mmx --help` mentions
`mmx module-json` separately for executables, outside the user command list.
An executable finds the helper at `MMX_MODULE_HELPER`, so a Bash module needs
no `jq` or hand-built JSON:

```text
module-json request [--inputs NAME,...]
module-json get [--type string|integer|number|boolean|array|object|null|json] POINTER
module-json result --mode check|apply --change CHANGE [--status STATUS] [--secret]
  [--reason TEXT] [--output NAME TEXT]... [--output-json NAME JSON]...
  [--fingerprint TEXT] [--reason-stdin | --diff-stdin | --output-stdin NAME]
```

`request` validates the request on stdin. `get` returns the typed JSON value
selected by a JSON Pointer such as `/inputs/path`. `result` encodes the
result. A `module-json result` call accepts at most one of `--reason-stdin`,
`--diff-stdin`, and `--output-stdin NAME`. The selected option reads the
reason, difference, or named output from stdin. The stdin options let the
helper read long text, differences, and secrets without command-line values.

`--type integer` accepts whole numbers in the signed 64-bit range
(-9223372036854775808 through 9223372036854775807), including whole-valued
decimals and exponents such as `2.0` and `2e3`. `module-json get --type integer`
rejects fractional and out-of-range values and prints plain decimal without
a decimal point, exponent, or added newline. `--type number` preserves the
JSON number's encoding. The helper exits 4 for bad arguments and 1 for
invalid data.

The [module interface reference](../../reference/module-interface.md#the-module-json-helper) owns the helper patterns.
The [Bash and Go greeting examples](../../../examples/external-modules/README.md)
in [Write your own module](../../how-to/write-your-own-module.md) make one file contain `Hello, Barbara!` followed by a
newline and return that
text as the output `greeting`.

### Ordinary task behavior and recovery

A node task that calls a module that is not core uses the ordinary
conditions, loops, retries, time limits, stopping, saved results, and
resuming of saved runs. A node task calling a module that is not core retries
by default, like a core resource module. In a node task block's `changes`,
the node task is a tracked input by the called module's fingerprint:
a `systemd_service` task in the same block restarts or reloads its service
when the fingerprint differs from the one the service last started with. The
[execution design](execution.md#a-module-that-is-not-core-gives-its-own-baseline)
says what happens where MMX has no earlier fingerprint, such as after a
reboot.

A task that consumes a secret input makes the module's output sensitive. An
executable can also mark its result `secret: true`, but it cannot clear
sensitivity MMX imposed; a Starlark script has no such key.

MMX does not load plugins or fetch modules from anywhere.

## Dryrun and diff are part of the module contract

A dryrun reports each task's status and change (`unchanged`,
`would_change`, or `unknown`), with the reason for work it could not check.
Inspection installs MMX itself when needed but does not install packages, write
managed files, reload services, or update service records. A prerequisite that
is not yet installed makes the dependent check not checked. The
[output contract](workflow.md#human-output-is-a-view-of-structured-results)
owns preview limits and fleet aggregation, and
[exit results](workflow.md#exit-results) own exit codes.

## Module design decisions

The decisions in the following table are settled. Revisit a module design
decision only with a concrete workflow that the decision fails to support.

| ID | Decision | Reason |
|---|---|---|
| D20 | Removing a task never deletes its resource; deletion is an explicit `state: absent`. MMX removes what a task does not name only when that task says it holds the whole set: `<things>_not_listed: remove` or `delete_everything_inside_except`. Neither removing unlisted entries nor deleting directory contents happens by default. | Deletion by omission is invisible in review and destructive in a partial playbook; a task that states the whole set makes the deletion visible on the page. |
| D21 | Manage parts of a system through native boundaries (drop-ins, `sources.list.d`, include directories, one key, one rule). `line` manages one line, at most one match. `edit_file` changes other text through a filter the author writes and MMX checks for repeatability. No module for a marked block of lines. | Whole files and drop-ins have obvious ownership; a marked block leaves two owners in one file. |
| D26 | A node task names every module by its name, whatever its kind. MMX looks a name up among the core modules, the official extra modules, and the project's own modules, and a module of the project's own may not take the name of another module or of a node task setting. | Every module looks the same in a playbook, so a module can move between kinds without changing anyone's configuration. `mmx module-usage`, `describe-module`, and `explain` show the kind that the YAML does not. |
| D29 | Package presence, upgrades, and versions: see the [package modules decisions](modules-packages.md#decisions). | |
| D35 | Resource modules of every kind retry by default; `shell`, `http`, `read_file`, `systemd_service` (a start that failed fails again, and systemd limits how often a unit may start), and `certbot_certificate` (whose certificate authority limits requests) opt in. `assert` rejects retries. `wait_for`, `reboot`, `systemd_networkd_file`, `netplan_file`, and `ifupdown_interfaces_file` reject retries and time limits, because each module bounds the module's own wait. The [retry contract](execution.md#task-retries-have-explicit-conditions-and-limits) lists the modules in each retry group. | Resources are safe to repeat; arbitrary scripts may not be. Another try helps against a temporary failure, such as a package manager's lock. Repeating an action does not help when the action keeps failing for the same reason. |
| D38 | A module that drives one tool is named for the tool (`systemd_service`, `apt_package`, `ufw`, `docker_compose_project`). No module hides the distribution or the tool behind a common name. A plain-named module does its work through the same mechanism wherever it runs; a tool module works wherever its tool is installed ([where a module works](#where-a-module-works)). | The reader sees what runs on the node; MMX's users mostly run one distribution and write roles for their own project. The name also tells the reader where the module works. |
| D39 | Common work on the tested distributions has a core module that detects change. No module where an existing task with its check already does the job. | A shell recipe for common work pushes change detection and dryrun onto every user; a thin wrapper adds a name without adding behavior. |
| D41 | Every field that fetches from a web address requires a SHA-256 or `dangerously_skip_checksum: true`. | The safe way is the normal way, and the unsafe way is visible to a reviewer. |
| D42 | Official extra modules and the project's own modules are Starlark scripts that call the same primitives as the core modules; any executable remains the way out for a special need. | MMX sees every change a script makes, so a dryrun never changes the node through a changing primitive, and the difference and the fingerprint come from MMX, not from each module. A script reads like a core module, and a coding agent writes it easily. An executable keeps the door open for any language, without those guarantees. |
| D43 | Every module, of every kind, has a description of its inputs, outputs, and examples, and `validate` checks every call against it. | Errors appear before anything runs, with the file and the line, and `describe-module` shows every module in one form. A module without a description could only check its inputs on the node. |
| D44 | Official extra modules live in `official_extra_modules/` beside `mmx`, ship in the same release with the same version, and are never copied into a project. | Adding or fixing an official extra module is a script change with no rebuild. One version check covers official extra modules, and nobody maintains a stale copy. A variant is a module of the project's own under another name. |
| D45 | Common work is a core module; official extra modules are for truly uncommon work; when unsure, core. The distribution does not decide the kind. One exception: a package module for a family other than Debian and Red Hat is an official extra module, such as `pacman_package` and `zypper_package`; `apt_package` and `dnf_package` stay core. | Core work gets MMX's full testing and needs nothing beside `mmx`. Moving a module between kinds is cheap now, but the common case should never depend on an extra. A Starlark package module passes the same module behavior contract as a Go one, it is the script that users copy to write a package module for their own distribution, and it needs only one general primitive, `run_command_that_refreshes_a_package_index`. |
| D46 | Every core module lives in one file with its input type, checks, description, examples, hooks, and code, and a registry lists them; the loader, `validate`, `describe-module`, `explain`, and the node task execution engine know no module by name. | Adding or moving a module touches one place and one registry line, and a reviewer reads a module in one file. |
| Interfaces N/O/T | `systemd_service` requires `state` (`running`, `stopped`, `do_not_start_or_stop`) and calls boot activation `start_at_boot`. `systemd_scheduled_job` requires `run_as_user_account`, including removal. `shell` and `reboot` use `skip_if_this_path_exists` and `run_only_if_this_path_exists`; shell resolves relative guards from `run_from_directory`. `wait_for` uses sentence forms and no `state`; Compose uses `docker_compose_project_is_ready`. | Required settings follow [the shared rule](#a-setting-with-no-safe-guess-is-required). Names show the exact condition or effect, and a guard must inspect the same path as its command. Compose readiness includes containers without health checks. |
| Interfaces M/W | `http` and `wait_for` check HTTPS server certificates by default, accept `trusted_certificate_authority_file`, and remove identity checks only with `dangerously_skip_server_certificate_check: true`. An `http` request uses ordinary task time limits; a `wait_for` request has a ten-second limit bounded by the remaining wait. | A private CA keeps verification; removing identity checks follows [protective choice level 3](#protective-choices-come-in-three-levels). Legitimate requests may take more than ten seconds; readiness polling needs repeated bounded observations. |

Other questions with a settled answer:

| Not a core module | Answer |
|---|---|
| `apt_key` | Deprecated upstream; `apt_repository` uses a keyring and `Signed-By`. |
| One `firewall`, `package`, or `service` module for every distribution | D38. |

## Validation targets

Test each resource against real system state: unchanged reruns, actual
changes, dryrun making no change, rejected inputs, and partial failure. Verify
that a rejected new file leaves the live file and running service
untouched, and observe service actions independently of MMX's records. The
[test strategy](development.md#automated-code-and-workflow-tests) describes
the suites; the tests hold the detailed cases.

## Beyond the first release

- Git submodules and LFS.
- Shell `stdin`.
- A runnable conformance check for executable modules.
