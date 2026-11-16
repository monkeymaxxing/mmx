# `pacman_package`

`pacman_package` manages packages on an Arch Linux **node** through pacman,
Arch Linux's package manager; a node is a Linux server that MMX manages. A
task keeps packages installed or removed, or upgrades the whole system.

```yaml
- pacman_package:
    packages: [nginx, certbot]
```

pacman has one rule that shapes the module: **Arch Linux supports no
partial upgrade.** The package databases, pacman's lists of what the
package servers offer, and the installed packages must come from the same
moment. A package installed from newer databases than the rest of the
system can need a library version that the system does not have yet.
pacman's own answer is the full system upgrade, `pacman -Syu`, so:

- **Every installation is a full system upgrade.** A task that installs a
  missing package refreshes the package databases and upgrades every
  package in the same transaction, as `pacman -Syu nginx` does. A task
  whose packages are all installed changes nothing and contacts no package
  server.
- **A package has no version setting.** Arch Linux's repositories offer one
  version of each package, the newest. A package cannot be kept at an older
  version or brought to the newest version by itself. `upgrade_all_packages:
  true` brings every package to the newest version.

`pacman_package` follows the
[rules that every package module follows](../../docs/changing-mmx/design/modules-packages.md#what-every-package-manager-module-does),
except where [pacman's own rules](../../docs/changing-mmx/design/modules-packages.md#pacmans-own-rules)
differ.
`pacman_package` is an **official extra module**: a script in Starlark, a
small language with Python-like syntax, that the MMX project writes, tests,
and ships beside `mmx`.

## Set up

The module installs nothing. pacman is part of every Arch Linux system.

A project with nodes of several distributions gives each package task a
condition on the node fact `package_manager`, which is `pacman` on Arch
Linux. Each **node task** below, one entry of a playbook's `node_tasks`,
installs nginx on the nodes of one package manager:

```yaml
- when:
    equals: ["{{ .node.package_manager }}", pacman]
  pacman_package:
    packages: [nginx]

- when:
    equals: ["{{ .node.package_manager }}", apt]
  apt_package:
    packages:
      - {name: nginx, version: newest_when_first_installed}
```

## Inputs

| Input | Meaning | When omitted |
|---|---|---|
| `packages` | The names of the packages, as `pacman -Q` shows them, such as `nginx`: letters, digits, and `@ . _ + -`. Name each package itself: a group, such as `gnome`, or a name that another package provides, such as `sh`, fails the task, also with `state: absent`. | Required, unless `upgrade_all_packages` is `true`. |
| `state` | `present` installs each package that is missing, with a full system upgrade. `absent` removes each package that is installed. | `present`. |
| `also_remove_packages_that_depend_on_these` | With `state: absent`, `true` also removes the installed packages that depend on the named ones. | A removal that would take other packages with it fails before anything changes. |
| `upgrade_all_packages` | `true` refreshes the package databases and upgrades every package that has a newer version, as `pacman -Syu` does. Takes no other input. | No whole-system upgrade. |

## Outputs

| Output | Holds |
|---|---|
| `changed_packages` | Every package that the task changed, sorted by name: `name`, `from` (the version before), and `to` (the version after). A package that the task installed has no `from`; a package that it removed has no `to`. The list includes the dependencies and the upgrades that pacman's transaction brought, not only the named packages. An unchanged task gives an empty list. |

The task's `reason` says the same in words, such as `installed mailcap
2.1.54-3, nginx 1.30.6-3; upgraded glibc from 2.44-1 to 2.44-2`. The
`reason` lists at most ten packages of each kind and counts the rest;
`changed_packages` lists every one.

## What a dryrun reports

A dryrun reads the installed packages with `pacman -Q` and asks pacman
what a transaction would do with `--print`, which changes nothing, takes no
lock, and refreshes no package database.

- **An installation:** `would_change`, with the packages that pacman would
  install or upgrade, judged against the package databases that the node has
  now. A run refreshes the databases first, so it can upgrade more packages
  than the dryrun listed. `--print` checks no conflict between packages: a
  package that conflicts with an installed one shows as an installation, and
  the run fails with the conflict.
- **A removal:** `would_change`, with the packages that pacman would remove.
  A removal that would take packages outside `packages` fails, as in a run,
  unless `also_remove_packages_that_depend_on_these` is `true`.
- **A whole-system upgrade:** the packages that are newer in the databases
  the node has now; `unchanged` when the databases have nothing newer.
- **`not_checked`** for a package that the node's databases do not have:
  a run refreshes the databases first, and an earlier node task may add the
  repository that has the package. `not_checked` also on a node without
  pacman.
- **A failure, as in a run,** for a group's name or a name that another
  package provides, to install or to remove, and for the inputs that break
  a rule.
- **What the dryrun leaves on the node:** nothing.

## Examples

Install nginx and certbot, then start nginx with its configuration:

```yaml
- task_block:
    id: nginx
    changes:
      - pacman_package:
          packages: [nginx, certbot]
      - file:
          path: /etc/nginx/nginx.conf
          source: files/nginx.conf
          permissions_mode: "0644"
    activate:
      - systemd_service:
          name: nginx
          state: running
          start_at_boot: true
```

On the node, the `pacman_package` task installs the two packages and
upgrades the system with them, in one transaction; Arch Linux's packages
start no service. The `file` task writes the configuration, and the
`systemd_service` task starts nginx. At a later run, the
`pacman_package` task is unchanged. When a whole-system upgrade, in this
playbook or another, has brought a new nginx version since, the
`systemd_service` task restarts nginx once, because the node task block
tracks the installed versions of the packages that its tasks name.

Remove Apache and the modules that depend on it:

```yaml
- pacman_package:
    packages: [apache]
    state: absent
    also_remove_packages_that_depend_on_these: true
```

Upgrade the whole system:

```yaml
- pacman_package:
    upgrade_all_packages: true
```

## How it works

**Installing.** The module reads the installed packages with `pacman -Q`.
When every package is installed, the task is unchanged. Otherwise the
module asks pacman for the whole transaction with `pacman -Sup --needed
<missing packages> --print`, against the databases the node has, and
checks that each missing name is a package of its own. Then it waits for
pacman's lock, refreshes the package databases (`pacman -Sy`), asks again,
and installs with `pacman -Su --needed --noconfirm <missing packages>`. The
refresh and the upgrade together are `pacman -Syu`, in two steps so that a
wrong name fails before anything changes. A name that only the refreshed
databases know is checked after the refresh. At the end the module reads
`pacman -Q` again and reports every version that differs.

`--print` resolves names and dependencies and checks nothing else. pacman
finds a conflict between packages only when the transaction runs, and
fails before it changes a package. A name in `IgnorePkg` is installed,
because `--noconfirm` answers pacman's "install anyway?" with yes. A
package that replaces an installed one, by its `Replaces` field, may be
missing from a dryrun's list; the run's `changed_packages` shows it.

**Removing.** A name that is not installed under its own name, but is a
group with installed members (`pacman -Qg`) or a name that an installed
package provides (`pacman -T`), fails the task: pacman would remove other
packages under other names. The module asks pacman with `pacman -Rcp` which packages a
removal would take, including those that depend on the named ones. Without
`also_remove_packages_that_depend_on_these`, a removal that would take more
than the named packages fails here. Otherwise the module removes with
`pacman -R`, and with `-c` when other packages go too. pacman removes a
package's files; a configuration file that was changed on the node stays
as a `.pacsave` file beside the old name. The module does not remove the
dependencies that nothing needs any more; `pacman -Qdt` lists them.

**Upgrading the whole system.** The module waits for pacman's lock,
refreshes the package databases, and asks pacman with `pacman -Sup` what
is newer. With nothing newer, the task is unchanged. Otherwise the module
upgrades with `pacman -Su --noconfirm`. pacman skips each package that
`IgnorePkg` or `IgnoreGroup` in `/etc/pacman.conf` names, and says so in a
warning.

**pacman's lock.** pacman holds the file `/var/lib/pacman/db.lck` while it
changes packages or refreshes its databases, and refuses to start while
the file exists. Before each such command, the module waits up to 60
seconds for the file to go, such as while a person or a timer runs pacman.
Reading and `--print` take no lock.

**Nothing prompts.** pacman runs with `--noconfirm`, which takes the
default answer of each question. A configuration file that was changed on
the node stays, and pacman writes the package's new version beside it as a
`.pacnew` file. A `file` node task owns a configuration file's contents.

**A service restarts when its package changes.** Inside a
[node task block](../../docs/changing-mmx/design/execution.md#node-task-blocks-complete-the-change),
the version of each package that the task names, or `absent`, is a
**tracked input**: MMX remembers it with the service's record, and a
`systemd_service` task in the block's `activate` section restarts the
service when the version differs from the one that the service last
loaded. A dryrun predicts the restart from the versions that pacman would
install.

## Errors and what they mean

Every reason starts with `pacman_package`.

| The reason holds | What it means, and what to do |
|---|---|
| `the program pacman is not installed; pacman_package works only on a node whose package manager is pacman ...` | The node is not an Arch Linux node. A run fails without another try. Give the node task a condition on the node fact `package_manager`, as [Set up](#set-up) shows. |
| `pacman_package.packages: pacman's package databases have no package NAME ...` | Also after a refresh, no repository of the node has the package. A run fails without another try and changes no package. Correct the name; `pacman -Ss` searches the databases. A package from a repository of your own needs the repository in `/etc/pacman.conf`, from an earlier node task. |
| `pacman_package.packages: NAME is a group or a name that another package provides, not a package of its own; ...` | pacman would install other packages under other names, or none because a package that provides the name is installed, and the task would try again on every run. Name the packages themselves; the reason lists those that pacman would install. |
| `removing NAME would also remove ..., which depend on them; ...` | Other installed packages need the named ones. Nothing changed. Add those packages to `packages`, or add `also_remove_packages_that_depend_on_these: true`. |
| `another pacman has held its lock /var/lib/pacman/db.lck for 60 seconds; ...` | Another pacman runs, or a pacman that was stopped left its lock file. MMX tries the task again. When `pgrep -x pacman` finds no pacman on the node, remove the file. |
| `refreshing the package databases: ...` | `pacman -Sy` failed, such as when no package server answers. The text behind the colon is pacman's message. MMX tries the task again. |
| `installing NAME: A and B are in conflict; unresolvable package conflicts detected; ...` | The package to install and an installed package cannot be installed together, such as `nginx-mainline` and `nginx`. No package changed. Remove the installed package first, with a `pacman_package` task with `state: absent` before this task. A dryrun does not see the conflict. |
| `installing NAME: ...`, `removing NAME: ...`, `upgrading all packages: ...` | pacman's transaction failed; the text behind the colon is pacman's message. MMX tries the task again. A `HoldPkg was found in target list` message means that `HoldPkg` in `/etc/pacman.conf` protects a package that the removal would take, such as `glibc`. `invalid or corrupted package (PGP signature)` on a node that was not upgraded for months means that the keys of the Arch Linux packagers are older than the packages; see the next row. |
| `invalid or corrupted package (PGP signature)` | The node's copy of the Arch Linux packagers' keys is older than the packages. Run the two node tasks [below](#update-the-packagers-keys-first) once, as a playbook of their own. |
| `... ended, and then reading the installed packages failed: ...` | pacman's command worked, and the read after it failed. MMX does not try the task again, because a new try would find the packages installed and report an unchanged task. Run the playbook again. |

After a failed transaction, and after a name that only a refresh showed to
be unknown, the node's package databases are newer than its installed
packages: the state that Arch Linux calls a partial upgrade, without
having installed anything. The next `pacman_package` installation or
whole-system upgrade brings both together. Until then, install nothing
with `pacman -S` by hand; use `pacman -Syu`.

### Update the packagers' keys first

On a node that was not upgraded for months, run these node tasks once, in
a playbook of their own. The `shell` task updates only the package of the
packagers' keys, `archlinux-keyring`, which is the one package that Arch
Linux updates alone; the `pacman_package` task then upgrades the rest.

```yaml
- shell:
    script: pacman -Sy --needed --noconfirm archlinux-keyring
- pacman_package:
    upgrade_all_packages: true
```

## Tested on

**Distributions:** Arch Linux.

The integration suite `tests/integration/pacman_packages.py` runs the
module on an Arch Linux test node of the current rolling release, with
pacman 7.1.

[Limits on each distribution](../../docs/reference/limits-on-each-distribution.md) says what is limited for this module on [Arch Linux](../../docs/reference/limits-on-each-distribution.md#arch-linux), and why.

## Why it works this way

- **Every installation is a full system upgrade.** Arch Linux supports no
  partial upgrade: a package from newer databases than the rest of the
  system can need newer libraries than the system has. `pacman -Syu
  <package>` is Arch Linux's own way to install.
- **No version setting.** Arch Linux's repositories offer only the newest
  version of each package, and pacman has no rule that keeps a package
  inside a version pattern. pacman's `IgnorePkg` keeps a package back during
  an upgrade, which is a partial upgrade; an administrator who chooses it
  writes it into `/etc/pacman.conf`, such as with a
  [`line`](../../docs/reference/modules/line.md) task, and the module's upgrades keep to
  it.
- **`packages` is a list of names, not of entries with a version.** With no
  version setting, an entry would hold only a name.
- **The module checks the names before it refreshes, and the transaction
  again after.** A group's name or a provided name fails before the
  databases change; a name that no repository has fails before any package
  changes. `--print` checks no conflict, so a conflict shows only in the
  run. A refresh changes no package, so a whole-system upgrade with nothing
  newer is unchanged.
- **A removal that takes other packages states so.** pacman refuses to
  remove a package that others need, unless it removes them too (`-c`); the
  task says which, as every package module does.
- **A group's name, or a name that another package provides, fails the
  task.** pacman installs and removes such a name under other names, so an
  installation would find the name missing and install again on every run,
  and a removal would remove nothing and report the task unchanged.
- **The module waits for pacman's lock and then fails.** A pacman that
  runs ends; a lock file that stays after 60 seconds is most often left by
  a pacman that was stopped, and only a person can tell that no pacman
  runs.
- **No input for the dependencies that nothing needs any more.** A removal
  leaves them, as `apt_package` without `also_remove_packages_no_longer_needed`
  does; `pacman -Qdt` lists them, and a `pacman_package` task with
  `state: absent` removes the ones you name.
