# Shared module behavior reference

This page lists the rules that several core modules share, so that each
module's page can link here instead of repeating them: file modes and
owners, file previews, `check_new_file`, package versions, package sources
and patching, service recovery, accounts, `run_as_user_account`, the
environment of a command, conditions, and waiting. A **core
module** is a module built into MMX, such as `file`, `apt_package`, or
`systemd_service`. A **node** is a Linux server that MMX manages, and a
**node task** is one entry in a playbook's `node_tasks`: one action that a
node performs with one module. A **node task block** groups node tasks into
phases, such as `changes`, `validate`, and `activate`. A rule that holds for
one module only is on that module's page: see the
[module pages](modules/README.md). To change a configuration file and
reload its service step by step, see
[Change a configuration and reload its service](../how-to/change-a-configuration-and-reload-its-service.md).

## File modes and owners

`owner`, `group`, and `permissions_mode` set who owns a file or directory
that a node task writes, and who may read and change it:

```yaml
- id: write_shop_settings
  file:
    path: /etc/shop/shop.env
    content: "SHOP_ENVIRONMENT=production\n"
    owner: root
    group: shop
    permissions_mode: "0640"   # the owner reads and writes, the group shop reads
```

`file`, `download`, and `directory` take all three settings, and
`unpack_archive` takes `owner` and `group` for every entry it extracts.
Other modules that write a file, such as `ssh_key_pair`, list the settings
they take on their own pages.

| Setting | Value | When the setting is left out |
| --- | --- | --- |
| `owner` | An account name, such as `shop`, or a number written without quotes, such as `999`. | A new file belongs to `root`. An existing file keeps its owner. |
| `group` | A group name, or a number written without quotes. | A new file belongs to the group `root`. An existing file keeps its group. |
| `permissions_mode` | Octal in quotes, as `chmod` writes it, such as `"0644"`. | `file` and `download` require it when they write content, because no mode is right for every file. A `file` node task that changes only an existing file's owner, group, or mode may leave it out. The [`directory` page](modules/directory.md) gives a directory's default. |

- **A number is used as it is,** with no account lookup. It is for a
  directory that a container's process owns under an ID with no account on
  the node, such as `owner: 999`. Prefer a name where an account exists: a
  bare number may be the ID of another account on this node, now or later.
- **Digits in quotes are refused,** so that one spelling has one meaning:
  `"999" is text made only of digits; to use the numeric ID, write it without quotes`.
- **A mode without quotes is refused:** YAML reads `644` as a number, and
  validation says `expected string, got !!int`.
- **The mode has three octal digits,** with an optional leading `0`.
  The setuid, setgid, and sticky bits are refused:
  `permissions_mode must be a quoted octal string, such as "0644"`.
- **`permissions_mode` may read a reference,** such as
  `"{{ .vars.settings_mode }}"`. MMX checks the resolved value's form before
  the module runs.
- **An account name is looked up when the node task runs,** so a `user` or
  `group` node task earlier in the same run can create the account. A
  dryrun reports the node task as `not_checked` when the account is still
  missing.
- **A file that holds a secret** needs an extra line when its mode lets
  another account read it, such as `secret_readable_by_group: true`. The
  [`file` page](modules/file.md#files-that-hold-secrets) gives the rule, and
  `download` follows it too.
- **On a node where SELinux is on,** a new or replaced file gets the label
  that the node's SELinux policy gives its path. MMX has no setting for the
  label of one file.

## Previewing file changes

`mmx dryrun --diff` shows what each node task would change in each file,
without changing it:

```sh
mmx dryrun set_up_web_servers --diff
```

```text
set_up_nginx / web01 / write_home_page: succeeded, would_change
set_up_nginx / web01 / write_home_page: would create "/srv/shop/public/index.html"
  absent -> 14 bytes, permissions_mode 0644, uid 0, gid 0
  --- before
  +++ after
  @@ -0,0 +1,1 @@
  +<h1>Shop</h1>
```

The line after the path is the file's size, mode, owner ID, and group ID,
before and after. The lines from `--- before` are the difference of the
text. [Preview and run changes](../how-to/preview-and-run-changes.md#steps)
captured this output.

The modules that write a file report their file changes this way, such as
`file`, `line`, `edit_file`, `download`, `directory`, `symlink`, and
`systemd_unit`. `--diff` works the same way on `mmx run`, for the changes
that the run made. `mmx results --diff` reads the saved previews again without
contacting a node, and `mmx results --diff=all` reads the complete saved
differences ([file differences](commands.md#--diff)).

### When the text is hidden

The size and mode line stays. In place of the text, the preview says why
the text is left out:

| The preview says | When |
| --- | --- |
| `contents hidden for a private file` | Other accounts could not read the file before the change, or cannot read the file after it: the mode has no read bit for others, as with `"0640"` or `"0600"`, or a directory above the file does not let others pass (no execute bit for others), as with a `0700` home directory or a `0750` key directory. |
| `contents suppressed for a task using secrets` | The node task reads a secret ([secrets in output](secrets.md)). |
| `contents read through content_from_node_paths are not shown; compare the sha256 values` | A `file` node task builds the file from other files on the node, such as a private key. The preview shows the SHA-256 of the old and the new content. |
| `binary contents omitted` | The file is not text. The sizes stay. |
| `text preview omitted: file exceeds 64 KiB` | The old or the new file is larger than 64 KiB. |
| `text preview truncated at 16 KiB or 200 lines` | The difference is longer than 16 KiB or 200 lines. |
| `full difference omitted: file exceeds 1 MiB` | `--diff=all` keeps no complete difference for a file larger than 1 MiB. |

- A removed file shows its size, mode, and owner only.
- MMX keeps complete differences of up to 8 MiB in all for each node job,
  one attempt of one fleet stage on one node. A directory copy keeps the
  complete differences of its first ten changed files.
- These size limits apply only to what MMX shows and saves, not to the
  files that MMX installs.
- Saved differences are removed together with the playbook run or node job
  that made them ([`run_history_retention`](project-settings.md#run_history_retention)).

Module pages that rely on this section: [`file`](modules/file.md), [`symlink`](modules/symlink.md), [`directory`](modules/directory.md).

## check_new_file

`check_new_file` checks a file's new content on the node before the new
content replaces the live file. A rejected file fails the node task and
leaves the live file as it was:

```yaml
- id: check_shop_settings
  file:
    path: /etc/shop/shop.env
    content: "SHOP_ENVIRONMENT=production\n"
    permissions_mode: "0644"
    check_new_file:
      shell:
        script: grep -q '^SHOP_ENVIRONMENT=' "$NEW_FILE"
        dryrun: grep -q '^SHOP_ENVIRONMENT=' "$NEW_FILE"
        env:
          NEW_FILE: "{{ .new_file.path }}"   # the new content's temporary file
```

On the node, the module writes the new content to a temporary file, runs the
check on it, and moves it into place only when the check exits with 0.

- **Modules:** `file`, `download`, `line`, and `edit_file`.
- **The path of the new content** is `{{ .new_file.path }}`. Pass it through
  `env`, as above. MMX does not replace references in `script` or `dryrun`,
  and validation refuses one there:
  `file.check_new_file.shell.dryrun runs as written, so the shell would receive the literal text {{ .new_file.path }}; ... pass the value through env and read the environment variable in the script, such as env: {NEW_FILE_PATH: "{{ .new_file.path }}"} and $NEW_FILE_PATH`.
- **The check is a `shell` node task** with the usual settings of the
  [`shell` page](modules/shell.md), except
  `only_when_tracked_inputs_changed` and `run_only_once_marker_file`,
  which validation refuses: a check runs for every new file.
- **A run checks every time,** also when the live file needs no change.
- **A dryrun runs only the `dryrun` command,** on the new content. Without
  a `dryrun` command, the check is reported as not checked. So is a check
  whose program is missing (exit code 126 or 127), and a failed check after
  an earlier node task of the same dryrun would change the node. The result
  keeps the check's output.
- **The result** shows the check's exit code and output, as
  `check_new_file: succeeded` under the node task's line.
- **Several files together:** a node task block's `validate` phase checks
  the files after the `changes` phase and before `activate`, such as
  `nginx -t` for a whole nginx configuration.
  [Change a configuration and reload its service](../how-to/change-a-configuration-and-reload-its-service.md)
  uses both.

Module pages that rely on this section: [`file`](modules/file.md), [`line`](modules/line.md), [`edit_file`](modules/edit_file.md), [`download`](modules/download.md).

## Package versions

Each entry in the `packages` list of a package module states which version
to keep:

```yaml
- id: install_web_server
  apt_package:
    packages:
      - {name: nginx, version: newest_when_first_installed}
      - {name: openssl, version: newest_on_every_run}
      - {name: docker-ce, version: "5:27.3.*"}
```

| `version` | A missing package | An installed package |
| --- | --- | --- |
| `newest_when_first_installed` | Installed at the newest version. | Left at its version. Later upgrades may move it. |
| `newest_on_every_run` | Installed at the newest version. | Upgraded to the newest version on every run. |
| A version, or for `apt_package` and `dnf_package` a `*` pattern, such as `"5:27.3.*"` | Installed at the newest version that matches. | Kept to the versions that match. |

- **Modules:** `apt_package`, `dnf_package`, `pip_package`, `gem_package`,
  and `npm_global_package`; and the official extra modules
  [`pacman_package`](../../official_extra_modules/pacman_package/README.md)
  and [`zypper_package`](../../official_extra_modules/zypper_package/README.md).
- **Not every module takes a pattern.** `gem_package` and
  `npm_global_package` take an exact version only, and
  `npm_global_package` also takes a newest version keyword; `pip_package`
  takes a pip version specifier, such as `">=2.32,<3"`. A pattern for
  `gem_package` fails with `version must be an exact RubyGems version`.
- **`version` has no default,** because no version policy is right for
  every package. Without it, validation says:
  `packages[0].version is required: a version or a pattern such as "5:29.*"; or newest_when_first_installed to install the newest version once and then leave it alone; or newest_on_every_run to upgrade it on every run`.
- **A version's form is the package manager's,** such as
  `5:27.3.1-1~debian.12` for apt or `"==23.0.0"` for pip. Each module page
  says which form it takes. Write a version in quotes.
- **A version rule.** `apt_package` and `dnf_package` hand a version or
  pattern to the package manager as a rule, so the package stays inside it
  also during automatic updates and when someone runs apt or dnf by hand.
  A keyword removes only the rule that MMX wrote. The
  [`apt_package` page](modules/apt_package.md#the-version-rule-file) lists
  what each situation on the node leads to.
- **Removal** is a node task with `state: absent`, whose entries give the
  package's `name`. Deleting the node task from the project removes
  nothing on the node.
- **In a node task block's `changes`,** the installed versions of the listed
  packages are tracked inputs: a changed version restarts the block's
  service, as [service recovery](#service-recovery) says.

The language installers share more rules, such as `<tool>_program_path`:
see [what the installer modules share](../how-to/install-languages.md#what-the-installer-modules-share).

Module pages that rely on this section: [`dnf_package`](modules/dnf_package.md), [`gem_package`](modules/gem_package.md), [`npm_global_package`](modules/npm_global_package.md), [`pip_package`](modules/pip_package.md).

## Package sources and patching

A package source, such as a vendor's apt repository, comes from an
[`apt_repository`](modules/apt_repository.md) or
[`dnf_repository`](modules/dnf_repository.md) node task before the package
node task that needs it.

- **The package index.** A package module refreshes the node's package
  index when an installation or upgrade needs it, at most once in each node
  job, and again after a node task of the same node job changed a package
  source. No setting refreshes it.
- **A dryrun refreshes nothing.** A package that the node's current index
  does not have is reported as not checked, because an earlier node task
  may add its source.
- **A package does not start its services** while MMX installs it. A
  `systemd_service` node task starts them
  ([`apt_package`](modules/apt_package.md#packages-do-not-start-their-services)).
- **A package node task on the wrong distribution fails.** `apt_package`
  works only where the node fact `package_manager` is `apt`, and
  `dnf_package` only where it is `dnf`. A role for several distributions
  gives each package manager its own node task with a `when`:
  [one role for several distributions](../how-to/reuse-work-with-roles.md#one-role-for-several-distributions).

To patch a node:

| Job | Setting |
| --- | --- |
| Upgrade every package, inside the version rules | `upgrade_all_packages_within_their_version_rules: true` on `apt_package` or `dnf_package` |
| Then remove packages that nothing needs any more, such as old kernels | `also_remove_packages_no_longer_needed: true` beside the upgrade, on either module |
| Apply only security updates | `only_security_updates: true` beside the upgrade, on [`dnf_package`](modules/dnf_package.md#apply-only-security-updates). On Debian and Ubuntu, the distribution's own tool: the [security-updates example](../../examples/security-updates-on-debian-and-ubuntu/README.md) |
| Reboot where an update asks for it | [`reboot`](modules/reboot.md#reboot-after-patching-only-when-an-update-asks-for-it), one node at a time |

A node task block that declares an upgraded package restarts its service
at its next `activate` phase, so the service loads the upgraded software.

## Service recovery

A node task block keeps a running service in step with the files,
packages, and checkouts that its `changes` phase declares, the block's
**tracked inputs**. MMX reloads or restarts the service when the tracked
inputs differ from what the service last loaded, also when the run that
changed them stopped before the reload:

```yaml
- task_block:
    id: shop_site
    changes:
      - id: write_site_file
        file:
          path: /etc/nginx/conf.d/shop.conf
          source: files/shop.conf
          permissions_mode: "0644"
    validate:
      - shell:
          script: nginx -t
          dryrun: nginx -t
    activate:
      - systemd_service:
          name: nginx
          state: running
          refresh: reload   # a changed file reloads nginx
```

Which change gives a reload and which a restart:
[Reload or restart](../how-to/change-a-configuration-and-reload-its-service.md#reload-or-restart).
A run that stopped before the reload, with real output:
[A run that stopped before the reload](../how-to/change-a-configuration-and-reload-its-service.md#a-run-that-stopped-before-the-reload).

- **The record.** MMX keeps a record of what each service loaded on the
  node, under `/run/mmx/services/`. `/run` is emptied at reboot.
- **After a reboot,** a running service with no record gets a new record
  from the tracked inputs as they are on disk, before MMX changes them. A
  reboot alone therefore causes no extra reload.
- **Once a reload succeeds,** a later failed `verify` check does not
  repeat the reload in the next run.
- **A service that MMX starts** gets one start, with no reload after it.
- **A package that starts or restarts a service** while an attempt writes
  the tracked inputs does not count as the reload: if the attempt ends
  before `activate`, the next attempt still refreshes the service.
- **A failed or unconfirmed service action** stays owed until a later
  action succeeds.
- **A module that is not core** in `changes`, such as an official extra
  module or a module of the project's own, is a tracked input by the
  fingerprint that its node task reports. With no saved fingerprint, such
  as after a reboot, a node task that changes nothing sets the baseline,
  and one that changes something refreshes the service. If a run ends
  between that change and the refresh, the next run refreshes the service,
  although the module's node task then reports unchanged
  ([design](../changing-mmx/design/execution.md#a-module-that-is-not-core-gives-its-own-baseline)).
- **Two node task blocks for one service** that call the same module, such
  as two calls of one role, each follow their own calls of the module.
- **`cleanup` belongs to the current attempt.** A failed cleanup is
  reported. The next run uses the current files and does not repeat an old
  run's cleanup.

Module pages that rely on this section: [`systemd_service`](modules/systemd_service.md), [`systemd_unit`](modules/systemd_unit.md).

## Accounts and service units

The modules [`user`](modules/user.md) and [`group`](modules/group.md)
manage local accounts, [`systemd_unit`](modules/systemd_unit.md) writes a
unit file, and [`systemd_service`](modules/systemd_service.md) starts,
stops, enables, and refreshes a unit. Each page says how its module
works. This creates an account and the unit file that runs a program as
that account, then starts the service:

```yaml
- user: {name: shop, system: true, create_home_directory: false}
- task_block:
    id: shop_service
    changes:
      - systemd_unit:
          name: shop.service
          template: files/shop.service
    activate:
      - systemd_service: {name: shop, start_at_boot: true, state: running}
```

- **An account that a node task names** is looked up when that node task
  runs: see [File modes and owners](#file-modes-and-owners).
- **A changed unit file** from `systemd_unit` in a node task block's
  `changes` restarts the service, also with `refresh: reload`.
- The [application-service example](../../examples/service-app/README.md)
  combines an account, directories, configuration, a complete `.service`
  unit, and its start. The
  [account-and-unit example](../../examples/account-and-unit/README.md) adds
  fixed account IDs, a service drop-in, and a separate removal playbook.

## run_as_user_account

`run_as_user_account` runs a node task's program as a local account on the
node, such as the application's account, instead of root:

```yaml
- id: shop_clear_cache
  shell:
    script: bin/clear-cache
    run_as_user_account: shop
    run_from_directory: /srv/shop/current
```

| Module | `run_as_user_account` |
| --- | --- |
| `shell`, `git` | Optional; default `root`. |
| `wait_for` | Optional, with `command_succeeds` only; default `root`. |
| `pip_package`, `gem_package`, `npm_global_package`, and the five lock file modules, such as `npm_project_dependencies` | Required, because installers run code from the packages they install. Write `run_as_user_account: root` to run as root. |
| `systemd_scheduled_job` | Required. |
| `cron_job` | Required: the account that cron runs the command as. |

- **No login.** Except for `cron_job`, whose command cron starts, MMX or
  systemd starts the program directly with the account's user
  ID, primary group, and supplementary groups. There is no password, sudo,
  `su`, login shell, or profile file, so an account with the shell
  `nologin` works.
- **The environment** gets `HOME`, `USER`, and `LOGNAME` from the account:
  [the environment of a command](#the-environment-of-a-command).
- **A missing account** is a missing prerequisite: a dryrun reports the node
  task as not checked, and a run fails without another try
  ([retries](playbook-file.md#retry)).
- **For a command you run now,** `mmx run-command --run-as-node-user-account shop`
  follows the same rules:
  [Run a command on servers now](../how-to/run-a-command-on-servers-now.md#steps).
- [Running as another account](modules/shell.md#running-as-another-account)
  on the `shell` page gives the details of the account lookup.

## The environment of a command

A command that a module runs on a node starts with a small fixed
environment. MMX does not load a login shell, profile files, or `BASH_ENV`:

Every command that MMX runs as root, whether a core module, a Starlark
module, or an executable runs it, gets `PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin`,
`LANG=C.UTF-8`, `USER=root`, `LOGNAME=root`, and `HOME` set to root's home
directory (`/root` on most nodes), plus the node task's `env`, which
replaces any of these. A command with `run_as_user_account` gets `HOME`,
`USER`, and `LOGNAME` from that account instead. A `module_executable` of
the project's own also gets `MMX_MODULE_HELPER`.

- **`env`** adds variables on top, on the modules that take it, such as
  `shell`, `wait_for`, and the language installers. A variable in `env`
  replaces a variable of the same name above, such as `PATH`.
  Values reach the program only through its environment, never its
  command line.
- **The working directory** is `/`, unless `run_from_directory` names
  another directory on the modules that take it.
- **New files** that a command creates get umask 022, as with a service
  that systemd starts: other accounts can read them unless the command sets
  another mode.
- **Some modules add variables for their own program,** such as
  `DEBIAN_FRONTEND=noninteractive` for apt.
- **The PostgreSQL modules** start `psql` with only `PATH` and their own
  connection settings, so a variable such as `PGHOST` cannot change which
  server they reach.

The [module interface](module-interface.md#the-executable-protocol) gives
the rules of a module of the project's own.

Module pages that rely on this section: [`shell`](modules/shell.md).

## Conditions

A condition decides whether something happens. `when` on a fleet stage or
a node task, the `that` of `assert`, `changed_when` and `failed_when` of
`shell`, the `until` and `retry_when` of a retry, and a value of
`set_vars` or `outputs` take a condition. A condition is a
reference to a true or false value, such as
`when: "{{ .vars.use_cache }}"`, or an operator from the table:

```yaml
- id: installed
  shell:
    script: kubelet --version | cut -d' ' -f2
    changed_when: false

- when:
    version_less_than: ["{{ .node_tasks.installed.stdout }}", "1.30.2"]
  shell:
    script: /usr/local/bin/upgrade-kubelet 1.30.2
```

| Condition | True when |
| --- | --- |
| `equals: [a, b]` | The two values are equal. The text `"2"` and the number `2` are not equal. |
| `contains: [text, literal]` | The text contains the literal text. |
| `matches: [text, pattern]` | The text matches the [Go regular expression](https://pkg.go.dev/regexp/syntax). |
| `number_equals`, `number_less_than`, `number_at_most`, `number_greater_than`, `number_at_least`: `[left, right]` | The two numbers compare that way. Text with spaces or a line end around a number, such as a command's output, counts as that number. |
| `version_equals`, `version_less_than`, `version_at_most`, `version_greater_than`, `version_at_least`: `[left, right]` | The two versions compare that way, number by number, so `1.9` is less than `1.10`. A version is numbers separated by dots, with an optional leading `v`. |
| `path_exists: /absolute/path` | The path exists on the node, as a file, a directory, or a link. |
| `all: [...]` | Every listed condition is true. |
| `any: [...]` | At least one listed condition is true. |
| `not: ...` | The enclosed condition is false. |

- **A value of the wrong form is an error, not false.** `number_at_most`
  with the text `about 3` fails the node task and names the value. So does
  a version that is not numbers and dots, such as the Debian package
  version `5:27.3.1-1~debian.12`.
- **Write a version in quotes.** YAML reads `1.30` without quotes as the
  number 1.3, and validation says:
  `version_less_than: 1.3 (float64) is a number with a fraction; write a version in quotes, such as "1.30"`.
- **JSON output.** The result of a `shell` node task has `stdout_json`,
  and the results of `http` and `wait_for` have `response_body_json`: the
  output, read as JSON, so a condition reads one field, such as
  `"{{ .node_tasks.health.response_body_json.ready }}"`. Reading a field of
  output that is not valid JSON fails and says so.
- **`path_exists`** is accepted only in the `when` of a node task or node
  task block. It looks at the node when the node task is reached, so it
  sees what earlier node tasks of the same run created. In a dryrun, when
  the path is missing and an earlier node task would create it, or change
  a directory that holds it or something inside it, the node task is
  reported as `not_checked` with the reason. Prefer a module's own check where one
  exists, such as `skip_if_this_path_exists` on `shell`.
- **Truncated output.** A condition that reads command or HTTP output that
  MMX truncated fails. A branch of `all` or `any` that is not needed is not
  read.
- `contains` and `matches` need text on both sides.

The [diagnostics example](../../examples/diagnostics/README.md) shows
conditions on command output.

Module pages that rely on this section: [`shell`](modules/shell.md), [`http`](modules/http.md), [`assert`](modules/assert.md), [`pip_package`](modules/pip_package.md).

## Waiting and checks

These modules check a node and change nothing:

| Module | What it checks |
| --- | --- |
| [`wait_for`](modules/wait_for.md) | Waits, from the node, until a port accepts connections, a path exists, a URL answers, or a command succeeds. The usual node task in a node task block's `verify` phase. |
| [`assert`](modules/assert.md) | Checks a condition once, and fails with its `message` when the condition is false. |
| [`http`](modules/http.md) | Sends one request, whose response a later node task reads. |
| [`shell`](modules/shell.md) with `changed_when: false` | Runs a command that only reads, such as a status command. |

```yaml
verify:
  - wait_for:
      url_responds: http://127.0.0.1/
      wait_at_most: 30s
```

- **`wait_for` checks every second** for up to `wait_at_most`, which
  defaults to `60s`, and then fails with what the last check saw.
- **A dryrun checks once,** without waiting. When the condition is not met
  yet, the node task is reported as not checked, because earlier node
  tasks of a run may make it ready.
- **`wait_for` and `reboot` take no retry and no time limit:**
  `wait_at_most` is their only limit. Which modules retry by default is in
  [node task retries and time limits](playbook-file.md#node-task-retries-and-time-limits).

## Limited application controls

An ordinary account on a node can be allowed to request one fixed action,
such as a restart of one application, without sudo and without MMX's
credentials. The
[limited-control example](../../examples/limited-control/README.md) builds
this with ordinary node tasks and no new module, and describes the
request file, the systemd units, and the files it installs.

- **Each account removes only its own request.** The request directory has
  the Linux sticky bit set, so an ordinary account cannot remove a request
  that another account made.
- **A request made while the action runs** gets one more run afterward;
  requests made meanwhile share it.
- **A reboot empties `/run`,** where the request directory lives. The
  interface is made again at boot, and each later run of the role repairs a
  path that has drifted, so a repair does not wait for a reboot.

Module pages that rely on this section: [`systemd_unit`](modules/systemd_unit.md).
