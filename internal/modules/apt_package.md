# `apt_package`

`apt_package` manages packages on a Debian or Ubuntu node through apt and
dpkg. A task keeps packages present or absent, keeps packages at a
version or inside a version pattern, brings named packages to the newest
version, installs a package that comes as a `.deb` file, or upgrades the
whole node.

```yaml
- apt_package:
    packages:
      - {name: nginx, version: newest_when_first_installed}
      - {name: certbot, version: newest_when_first_installed}
```

A task gives a `packages` list. Each present entry requires `name` and
`version`; each absent entry gives only `name`. One entry may use a version
pattern while another uses a keyword or a package file. Every check runs
before any version rule is written. MMX writes all rules before installing
all entries with one package-manager command, so list order does not matter.

A node that uses dnf gets [`dnf_package`](dnf_package.md) tasks
instead; a role for several distributions has one task for each package
manager, as [one module for each
tool](../../docs/changing-mmx/design/modules-packages.md#one-module-for-each-tool) shows.
The rules that the two package modules share, such as "the package index is the module's business", are in
[what every package manager module
does](../../docs/changing-mmx/design/modules-packages.md#what-every-package-manager-module-does).
A vendor's package source comes from an
[`apt_repository`](apt_repository.md) task before the package task.

## Examples

### Choose each package's version

The `version` field has no default. Choose an exact version, a `*`
pattern, or a keyword for each entry; [Package versions](../../docs/reference/shared-module-behavior.md#package-versions) lists what
each does. Both keywords remove only the version rule MMX wrote for the
package, and administrator rules stay. The
[required-settings rule](../../docs/changing-mmx/design/modules.md#a-setting-with-no-safe-guess-is-required)
explains why each entry must state its version policy.

### Install or remove packages

```yaml
- apt_package:
    packages:
      - {name: nginx, version: newest_when_first_installed}
      - {name: certbot, version: newest_when_first_installed}
```

On the node, MMX asks dpkg which listed packages are installed and runs apt
only when a package must change, in one command. A package that is installed stays
at its version: a rerun never upgrades by surprise.

```yaml
- apt_package:
    packages:
      - {name: apache2}
    state: absent
```

`state: absent` keeps configuration files and does not refresh the index.
MMX asks apt to simulate removal first. If apt would also remove packages
that the task does not name, the task fails before changing anything.
Add `also_remove_packages_that_depend_on_these: true` to permit removal of
packages outside the task's `packages` list. To restrict removal to an
explicit list, put every package apt would remove in `packages` and leave
`also_remove_packages_that_depend_on_these` off. A package already absent is
unchanged, also when only its configuration files remain. The result lists
each removal as
`{name, from}`. A dryrun asks apt to simulate removal and fails if apt would
remove an unlisted package without
`also_remove_packages_that_depend_on_these: true`.

### Keep a package at a version

```yaml
- apt_package:
    packages:
      - {name: docker-ce, version: "5:27.3.*"}
```

Each present entry requires `version`. A version rule is the exact
version text that apt shows (`5:27.3.1-1~debian.12~bookworm`), or a pattern
in which `*` stands for any text (`5:27.3.*`). `*` is the only pattern
character.

On the node, MMX installs the newest version that matches and writes a rule
for apt, `/etc/apt/preferences.d/mmx-docker-ce.pref`, so apt itself keeps
the package inside the pattern at all times: during MMX runs, during
`upgrade_all_packages_within_their_version_rules: true`, during the distribution's automatic security updates, and
when an administrator runs apt by hand. Updates inside the pattern still
arrive: with `5:27.3.*`, a security fix `5:27.3.2` is installed by the next
upgrade, and `5:27.4.0` never is. A version outside the pattern is refused
by apt, whoever asks.

The task does not move an installed package to a newer version inside the
pattern; `upgrade_all_packages_within_their_version_rules: true` and automatic updates do that.
Changing the entry to `newest_when_first_installed` or
`newest_on_every_run` deletes MMX's rule.
`newest_when_first_installed` keeps the installed version; later upgrades
are free to move the package to another version. An `apt_package` task with
`newest_on_every_run` can upgrade the package during the task's execution.
Deleting the task from the project leaves both the rule and the package.
[The version rule file](#the-version-rule-file) says what the rule holds and
what each situation on the node leads to.

Use `version: newest_on_every_run` to follow the newest available version.

### Bring named packages to the newest version

```yaml
- apt_package:
    packages:
      - {name: ca-certificates, version: newest_on_every_run}
      - {name: openssl, version: newest_on_every_run}
      - {name: tzdata, version: newest_on_every_run}
```

`version: newest_on_every_run` installs each named package that is missing and
upgrades each named package to the newest version apt selects from the
node's package sources under the administrator's rules. MMX refreshes the
package index once per node job first. A node job is one attempt to execute
one fleet stage on one node. The task is unchanged when apt has
nothing newer. The `newest_on_every_run` task's result's `value` lists every
package changed by the command, including dependencies outside `packages`, as
`{name, from, to}`; a package that was missing has no `from`. The task
deletes MMX's version rule for each name. A dryrun does not refresh the index
and lists `name`, `from`, and `to` for packages that would change.

Packages with different needs can share the same `packages` list. A version rule that MMX
or the administrator wrote for another package still applies.

### Upgrade the whole node

```yaml
- apt_package:
    upgrade_all_packages_within_their_version_rules: true
    also_remove_packages_no_longer_needed: true
```

`upgrade_all_packages_within_their_version_rules: true` takes no other field except
`also_remove_packages_no_longer_needed`.

Preview this node task with `mmx dryrun`. On the node, apt simulates its
`full-upgrade` against the package index the node has now, without refreshing
it. The result's `value` lists each package it would upgrade, install, or
remove, with the fields `name`, `from`, and `to`. On a node whose index is
older than its package sources (apt's daily timer normally keeps it within a
day), the run can find more upgrades than the preview listed.

With `mmx run`, MMX refreshes the node's package index once per node job,
then runs apt's `full-upgrade` without prompts, keeping configuration files
that were changed on the node. apt may install packages that the upgrade
needs (a new kernel, say) and remove packages it must remove to complete the
upgrade. Version rules are honoured: a package with a `version` task stays
inside its pattern, and so does a package that the administrator holds with
`apt-mark hold` or with a preferences file of their own.

With `also_remove_packages_no_longer_needed: true`, MMX then runs apt's
`autoremove`, which removes automatically installed packages that nothing
needs any more, such as old kernels. Without it, MMX removes none of them.

The task is unchanged when nothing is upgradable and nothing is to be
removed. Otherwise its result's `value` lists the packages that change as
`{name, from, to}`: an upgraded package has both versions, a newly installed
one has no `from`, and a removed one has no `to`.

A package's scripts may restart a running service during the upgrade, as
[packages do not start their services](#packages-do-not-start-their-services)
allows, so running services load the upgraded software. A node task block
that declares an upgraded package restarts its service once at its next
activation, because the installed version differs from the one its service
record holds. A patching playbook therefore upgrades, then applies the roles
whose services must load the upgraded software, then reboots where a new
kernel requires it. Roll it out with the fleet stage's concurrency and
failure settings ([execution](../../docs/changing-mmx/design/execution.md)). The
[`reboot` page](reboot.md#reboot-after-patching-only-when-an-update-asks-for-it) has a
one-node-at-a-time patch-and-reboot example.

### Apply only security updates on Debian and Ubuntu

apt has no option to select only security updates, so `apt_package` has no
`only_security_updates` input. The distribution's own tool
`unattended-upgrade` applies security updates on the node. The
[security-updates-on-debian-and-ubuntu example](../../examples/security-updates-on-debian-and-ubuntu/README.md)
sets up the distribution's daily timers and applies updates on demand with
`shell`, including the tool's own dryrun. On a dnf node,
[`dnf_package`](dnf_package.md) has `only_security_updates`.

### Install a package from a .deb file

Some vendors publish a `.deb` file and no package source:

```yaml
- apt_package:
    packages:
      - {name: rclone, version: "1.68.2", package_file_url: https://downloads.rclone.org/v1.68.2/rclone-v1.68.2-linux-amd64.deb, package_file_sha256: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"}
```

Each file entry gives its `name`, the exact `version` inside the file, and
exactly one of:

- `package_file_url` with `package_file_sha256` (or
  `dangerously_skip_checksum: true`, which installs the file as it arrives):
  the controller machine fetches the file once per run and verifies it, as
  [files fetched from a web address](../../docs/changing-mmx/design/modules-packages.md#files-fetched-from-a-web-address)
  describes.
- `package_file_source`: a `.deb` in the project, found the way
  `file.source` finds one.

On the node, MMX reads the package name and version from the file and fails
when the file holds another name or version than the entry states. The task is
unchanged when that version is installed. Otherwise apt installs the file
and takes the packages it depends on from the node's package sources. MMX
also writes the [version rule](#keep-a-package-at-a-version) for the file's
version, so a package source that offers another version of the same package
does not replace the one the project installs. To move to another version,
point the task at another file. An installed version newer than the file's
is an error unless the task says `allow_downgrade: true`.

A file for one processor architecture gets a condition:

```yaml
- when:
    equals: ["{{ .node.arch }}", amd64]
  apt_package:
    packages:
      - {name: rclone, version: "1.68.2", package_file_url: https://downloads.rclone.org/v1.68.2/rclone-v1.68.2-linux-amd64.deb, package_file_sha256: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"}

- when:
    equals: ["{{ .node.arch }}", arm64]
  apt_package:
    packages:
      - {name: rclone, version: "1.68.2", package_file_url: https://downloads.rclone.org/v1.68.2/rclone-v1.68.2-linux-arm64.deb, package_file_sha256: "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"}
```

A dryrun compares the file's version with the installed one and installs
nothing.

### Remove a package completely

```yaml
- apt_package:
    packages:
      - {name: newrelic-sysmond}
    state: absent
    also_delete_its_config_files: true
    also_remove_packages_no_longer_needed: true
    also_remove_packages_that_depend_on_these: true
```

`also_delete_its_config_files` and `also_remove_packages_no_longer_needed`
are off unless the task sets the fields to `true`. Purge requires
`state: absent`; autoremove also works after a whole-node upgrade.

- `also_delete_its_config_files: true` uses apt's purge. A package
  that was removed earlier and whose configuration files remain is then not
  yet absent: the task purges it.
- `also_remove_packages_no_longer_needed: true` passes apt's own
  `--autoremove`. apt decides what goes: the automatically installed
  packages that nothing needs once the named packages are gone. Autoremove
  includes a package that was already unneeded before the `apt_package` task.
  MMX does not work out dependencies itself.

The result's `value` lists every removed package as `{name, from}`. A dryrun
lists what apt's simulation would remove.

### Install a package, write its configuration, then start its service

On Debian and Ubuntu, a package's installation scripts normally start the
package's services at once, with the packaged configuration, before the role
has written its own. `apt_package` stops that, so a service starts only when
a [`systemd_service`](systemd_service.md) task says `state: running`. The
usual order in a node task block is therefore: install the package and write
its configuration in `changes`, and start the service in `activate`.

```yaml
- task_block:
    id: loki
    changes:
      - apt_package:
          packages:
            - {name: loki, version: newest_when_first_installed}
      - file:
          permissions_mode: "0644"
          path: /etc/loki/config.yml
          source: files/loki-config.yml
    activate:
      - systemd_service:
          name: loki
          state: running
          start_at_boot: true
```

On the node, the `apt_package` task installs Loki without starting it, the
`file` task writes its configuration, and the `systemd_service` task in
`activate` starts the service with that configuration.
`allow_package_to_start_its_services: true` runs one task's apt command
without this rule, for a package whose installation fails or is incomplete
unless its service starts.

## How it works

### Installation and the package index

With `packages[].name` and `state: present`, MMX inspects each name with dpkg. Before a
required installation it refreshes the package index once per node job and
installs the missing names in one apt command. A changed file or link under
`/etc/apt/` in the same node job, such as a new source from an
[`apt_repository`](apt_repository.md) task, makes the next installation
refresh again, so the new source is found. MMX also compares the contents of
the node's package sources, signing keys, and logins under `/etc/apt/`
(`sources.list`, `sources.list.d/`, `trusted.gpg`, `trusted.gpg.d/`,
`keyrings/`, and `auth.conf.d/`) with those at the last refresh, and
refreshes again when they differ. A source that a vendor's release package
or a `shell` node task adds is therefore found by the next installation.

A dryrun refreshes no index and writes no source file. When the current
indexes cannot find a requested package, the installation reports
`not_checked`, with apt's reason. This also applies when an earlier node
task would add the source: that source is only previewed during a dryrun.
A real run refreshes the indexes first and fails if the package is still
missing. Version rules and packages stay unchanged on that refusal.

apt waits up to 60 seconds for the dpkg lock, such as while the daily update
that a fresh server runs at first boot is still busy. MMX tries the index
refresh again for up to 60 seconds while another apt holds the lock on the
package lists. dpkg keeps a configuration file that was changed on the node
instead of prompting; a `file` node task owns that file's contents when MMX
should own them. A package that dpkg left unpacked but not configured, after
an interrupted installation, is configured.

### The version rule file

MMX carries a `version` out with apt's own mechanism, a preferences file. It
writes one file for the package:

```text
/etc/apt/preferences.d/mmx-docker-ce.pref:
  Explanation: Written by MMX for an apt_package task with a version.
  Package: docker-ce
  Pin: version 5:27.3.*
  Pin-Priority: 999

  Explanation: Versions outside the pattern are never installed.
  Package: docker-ce
  Pin: version *
  Pin-Priority: -1
```

The file's name is `mmx-<package>.pref`. apt reads a preferences file only
when its name has letters, digits, `-`, `_`, and `.`, so MMX writes each
other character of a package name as `_` and its two-digit hexadecimal code:
`mmx-g_2b_2b.pref` for `g++`. Preferences files that MMX did not write are
left alone and still apply.

The task is unchanged when the file has the right contents and the installed
version matches the pattern. Matching the pattern is a text comparison.
Whether one version is newer than another is dpkg's answer
(`dpkg --compare-versions`).

MMX checks before it writes anything, so a refused task changes nothing:

| Situation on the node | What the task does |
|---|---|
| The package is not installed. | Refreshes the index, writes the file, and installs the newest version that matches. An interrupted run leaves the file; the next run installs. |
| The installed version matches. | Writes the file if it differs. The package is not touched. |
| The installed version does not match, and the newest matching version is newer. | Writes the file, then upgrades to the newest matching version. |
| The installed version does not match, and the newest matching version is older. | Fails and writes nothing, with the installed version and the newest matching version in the message. With `allow_downgrade: true`: writes the file, then installs the older version. |
| No version in the node's package sources matches. | Fails and writes nothing. The message lists the versions that are available. |

Changing what the project asks for:

| Change in the project | Effect on the node at the next run |
|---|---|
| `version` is changed to a newer series. | The file is rewritten and the package upgraded. |
| `version` is changed to an older series. | Needs `allow_downgrade: true`, as above. |
| `version` is changed to `newest_when_first_installed`. | MMX deletes its file. The installed version stays; later upgrades are free to move it. |
| The task is deleted from the project. | Nothing. The file and the package stay. |
| The task says `state: absent`. | The package is removed, then MMX deletes its file. |

A dryrun shows the preferences file as a file change and the version it
would install or remove. It judges against the index the node has now; when
that index holds no matching version, it reports the task as not checked,
because a run refreshes the index first.

### Package files

The bytes of a package file, from `package_file_url` or
`package_file_source`, are frozen with the run and travel to the node as a
streamed asset. MMX deletes its copy of the file on the node after apt has
installed it. apt does not check a signature on a package file that it
installs by path; the SHA-256 in the task is what establishes that the bytes
are the publisher's.

### Packages do not start their services

The rule for every apt command that `apt_package` runs:

- A package's scripts **may restart a service that is running.** An upgrade
  therefore still puts upgraded software into the services that are up, as
  apt does by itself.
- A package's scripts **may not start a service that is not running.**

MMX uses Debian's own switch for this, the policy program
`/usr/sbin/policy-rc.d`, which the package scripts ask before each service
action. Before the apt command, MMX puts its policy program there; after the
command, MMX removes it. The program's second line, after `#!/bin/sh`,
marks it as written by MMX. A leftover from an interrupted run is removed by the next `apt_package`
task and at the start of every node job that may change the node. When the
node already has a `policy-rc.d` that MMX did not write, MMX leaves it in
place, that program decides, and the task's result says so.

A package may still enable its service for the next boot. A
`systemd_service` task with `start_at_boot` states what the project wants.

## Errors and what they mean

- **The node uses another package manager.** The task fails with a reason
  that says to give the node task a condition so that it runs only on nodes
  that use apt, such as `apt_package installs with apt, but this node's
  package manager is dnf (the node fact package_manager); give the node task
  a condition so that it runs only on nodes that use apt`.
- **The current indexes cannot find a requested package.** A dryrun reports
  `not_checked`, because an earlier task may only preview the package source.
  A run refreshes first and fails with apt's reason if the package is still
  missing. Other simulation failures, such as unsatisfied dependencies,
  still fail the dryrun.
- **No version in the node's package sources matches `version`.** A run
  fails, writes nothing, and lists the versions that are available. Correct
  the pattern, or add the package source that offers the version.
- **The installed version is newer than every version that matches.** The
  task fails, writes nothing, and names both versions. Add
  `allow_downgrade: true` when the older version is wanted.
- **One node job names the same package with a version rule in one task and
  a version keyword in another**. The two
  tasks would undo each other on every run. MMX rejects that at validation
  when it can see both names, and otherwise fails the second task before it
  changes anything. Keep one rule for each package in a fleet stage.
- **A package file holds another package than the task names.** The task
  fails. Correct the entry's `name`, or point the task at the right file.
- **The installed version is newer than the package file's.** The task
  fails unless it says `allow_downgrade: true`.
- **A source that apt cannot use** fails the index refresh, and apt's
  message names the source.
- **The node stopped inside dpkg** during an upgrade, such as when it lost
  power. The next attempt fails with apt's instruction to run
  `dpkg --configure -a`, and MMX does not retry it, because a retry
  cannot fix it. The reason starts "Run dpkg --configure -a on the node
  first". Run that command on the node, then run the playbook again or
  `mmx resume`.

## Tested on

**Distributions:** Debian 12 and 13, Ubuntu 24.04 and 26.04.

The suite `server_resources.py` covers pinned package versions, package
sources, and a whole-node upgrade, and runs on Debian 12 and 13 and Ubuntu 24.04 and 26.04 in
the whole catalog. `apt_package` runs on these, with the apt version that the
[test harness](../../docs/changing-mmx/testing.md#current-platform-coverage) records:

| Distribution | apt |
|---|---|
| Debian 12 | 2.6 |
| Debian 13 | 3.0 |
| Ubuntu 24.04 | 2.8 |
| Ubuntu 26.04 | 3.2 |

The [security updates example](../../examples/security-updates-on-debian-and-ubuntu/README.md)
passed `server_resources.py` on Debian 13.

## Why it works this way

- **The preview includes every entry and the proposed version rules.** A
  package file can satisfy another entry's dependency. Checking the complete
  transaction before writing rules prevents partial changes on refusal.

- **`also_remove_packages_no_longer_needed` names apt's actual scope.**
  apt's autoremove can delete packages that were already unneeded before
  the `apt_package` task. The name makes removal of already unneeded packages
  visible.
- **One list, one transaction.** Dependencies may require several packages
  at matching versions. Writing all rules first and installing together
  makes entry order irrelevant.
- **An unknown package in a dryrun is not checked.** Earlier node tasks
  may only preview the source that supplies the unknown package. A real run
  refreshes the indexes before deciding that a package is missing. A dryrun
  keeps uncertainty about the package's availability visible without
  changing the node.
- **Every present entry states its version policy.** Presence alone cannot
  say whether an installed package should upgrade. See the
  [required-settings rule](../../docs/changing-mmx/design/modules.md#a-setting-with-no-safe-guess-is-required).
- **Removal checks the package manager's simulation first.** An unnamed
  removal needs `also_remove_packages_that_depend_on_these: true`, so the
  task states its wider effect. See
  [protective choices](../../docs/changing-mmx/design/modules.md#protective-choices-come-in-three-levels).
- **Package entry mappings are written in the task.** Controller files
  must be prepared while the project loads. Whole-list and whole-entry
  references would leave those files unavailable on the node; references
  in individual fields remain supported.
- **The result includes dependency changes.** A native transaction can
  change packages outside the task's list. Reporting those changes makes
  the full effect visible.
- **A file entry states its exact version.** Checking the file's name and
  version before writing any rule catches a mismatched artifact before the
  transaction changes packages.
- **Version keywords remove only MMX's rules.** An administrator's rule
  remains under the administrator's control. Candidate selection keeps
  administrator rules when MMX removes its own rule.

- **A `version` is kept by apt's own rule, a preferences file.** A version
  that MMX alone enforced would be undone by every other upgrade on the
  node: an administrator's apt command, `upgrade_all_packages_within_their_version_rules: true`, and the
  distribution's automatic updates.
- **A package's scripts may restart a running service and may not start a
  stopped one, unless the task says `allow_package_to_start_its_services:
  true`.** A service then starts only where the project says so, on every
  distribution, and an upgrade still reaches running services. A node then
  behaves the same on Debian and on the Red Hat family, whose packages do
  not start their services.
- **MMX retries the index refresh while another apt holds the lock on the
  package lists.** `apt-get update` does not wait for that lock itself, and
  apt's daily timer holds it just after a first boot.
- **Two tasks for one package, one with a version rule and one with a keyword, are
  refused.** Each would undo the other on every run.
- **A package file needs a SHA-256 in the task.** apt checks no signature
  on a file it installs by path.
- **No `only_security_updates`.** apt has no option that selects only
  security updates; the distribution's `unattended-upgrade` does that.
- **No field for answers to a package's installation questions.** A `shell`
  task that runs `debconf-set-selections` before the package task, with
  `skip_if_this_command_succeeds: dpkg -s <package>`, gives them.
