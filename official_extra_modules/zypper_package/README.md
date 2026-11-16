# `zypper_package`

`zypper_package` manages packages on an openSUSE **node** through zypper,
openSUSE's package manager; a node is a Linux server that MMX manages. A task
keeps packages installed at a version rule or removed, or updates the whole
system. To add a software repository first, use
[`zypper_repository`](../zypper_repository/README.md).

```yaml
- zypper_package:
    packages:
      - {name: nginx, version: newest_when_first_installed}
```

`zypper_package` follows the
[rules that every package module follows](../../docs/changing-mmx/design/modules-packages.md#what-every-package-manager-module-does),
except where [zypper's own rules](../../docs/changing-mmx/design/modules-packages.md#zyppers-own-rules)
differ. Its entries look like those of `apt_package`: a `name` and a `version`.
`zypper_package` is an **official extra module**: a script in Starlark, a
small language with Python-like syntax, that the MMX project writes, tests,
and ships beside `mmx`.

## Set up

The module installs nothing. zypper is part of every openSUSE system.

A project with nodes of several distributions gives each package task a
condition on the node fact `package_manager`, which is `zypper` on openSUSE.
Each **node task** below, one entry of a playbook's `node_tasks`, installs
nginx on the nodes of one package manager:

```yaml
- when:
    equals: ["{{ .node.package_manager }}", zypper]
  zypper_package:
    packages:
      - {name: nginx, version: newest_when_first_installed}

- when:
    equals: ["{{ .node.package_manager }}", apt]
  apt_package:
    packages:
      - {name: nginx, version: newest_when_first_installed}
```

## Inputs

| Input | Meaning | When omitted |
|---|---|---|
| `packages` | A list of entries, each a mapping with `name` and, with `state: present`, `version`. `name` is the package's name as `rpm -q` shows it, such as `nginx`: letters, digits, and `. _ + -`. Name each package itself: a name that another package provides, such as a command's path or `awk`, is refused, also with `state: absent`. With `state: absent` an entry holds only `name`. `version` is `newest_when_first_installed`, `newest_on_every_run`, or a version such as `1.28.1` or `1.28.1-160100.2.1` (see below). | Required, unless `upgrade_all_packages` is `true`. |
| `state` | `present` installs each package that is missing, or at another version than its entry wants. `absent` removes each package that is installed. | `present`. |
| `also_remove_packages_that_depend_on_these` | With `state: absent`, `true` also removes the installed packages that depend on the named ones. | A removal that would take other packages with it fails before anything changes. |
| `upgrade_all_packages` | `true` refreshes the repositories and updates every package that has a newer version, as `zypper update` does. Takes no other input. | No whole-system update. |

The three kinds of `version`:

- `newest_when_first_installed` installs the newest version when the package
  is missing and leaves an installed package as it is.
- `newest_on_every_run` installs the newest version when the package is
  missing and updates an installed package when a newer version exists. The
  task refreshes the repositories on every run to find out. **Newest means
  the newest version from the vendor of the installed package:** zypper never
  changes a package's vendor by itself. When a newer version exists only from
  another vendor, zypper says so and does nothing, the task is unchanged, and
  its reason says `not updated:` and quotes zypper's remark, which names the
  other vendor. The same holds for `upgrade_all_packages`.
- A version, such as `1.28.1` or, with its release, `1.28.1-160100.2.1`, is
  the version the package must have. A task installs it, or updates the
  package to it. It never **downgrades**: when the installed version is
  newer than the wanted one, the task fails, because zypper does nothing for
  it and exits with code 0. zypper offers a downgrade only with
  `--oldpackage`, which forces a package that zypper's solver would not
  choose. MMX does not **hold** the version either: a later
  `upgrade_all_packages` task updates the package. A person who wants a
  hold writes a lock with `zypper addlock`, which `zypper update` respects.

The module orders no versions itself. zypper says whether a version is an
update.

## Outputs

| Output | Holds |
|---|---|
| `changed_packages` | Every package that the task changed, sorted by name: `name`, `from` (the version before), and `to` (the version after). A package that the task installed has no `from`; a package that it removed has no `to`. The list includes the dependencies and the recommended packages that zypper's transaction brought, not only the named packages. An unchanged task gives an empty list. A version is rpm's `[epoch:]version-release`, such as `1.28.1-1.1`. |

The task's `reason` says the same in words, such as `installed nginx
1.28.1-1.1, nginx-config 1.0-1.1; upgraded glibc from 2.42-160100.1.1 to
2.42-160100.2.1`. The `reason` lists at most ten packages of each kind and
counts the rest; `changed_packages` lists every one.

## What a dryrun reports

A dryrun reads the installed packages with `rpm` and asks zypper what a
transaction would do with `zypper --dry-run`, which changes no package and
refreshes no repository (`--no-refresh`).

- **An installation or update:** `would_change`, with the packages that
  zypper would install, update, or remove, judged against the repository
  metadata that the node has now. A run refreshes the repositories first, so
  it can change more than the dryrun listed.
- **A removal:** `would_change`, with the packages that zypper would remove.
  A removal that would take packages outside `packages` fails, as in a run,
  unless `also_remove_packages_that_depend_on_these` is `true`.
- **A whole-system update:** the packages that are newer in the metadata the
  node has now; `unchanged` when there is nothing newer.
- **`not_checked`** for a package that the node's repository metadata does
  not have: a run refreshes the metadata first, and an earlier node task may
  add the repository that has it. `not_checked` also on a node without
  zypper.
- **A failure, as in a run,** for a name that another package provides, to
  install or to remove, for a version that is older than the installed one,
  and for the inputs that break a rule.
- **What zypper's dry run does not check.** It resolves names and
  dependencies, and it finds a package that declares a conflict with an
  installed one. It downloads no package, so a dryrun does not see:
  - two packages that own the same file with different content. A run fails
    with a message that names the file and both packages, and installs
    nothing from the transaction;
  - a package whose signature is missing or wrong (`Signature verification
    failed`), or a repository package that cannot be downloaded;
  - a package whose license zypper asks about, which the module never accepts;
  - a script of a package that fails (exit code 107). The package is then
    installed.
- **What the dryrun takes and leaves on the node:** zypper's dry run takes
  zypper's lock, as a run does, so a dryrun waits up to 60 seconds while
  another zypper runs, and it may rebuild zypper's cache files of the
  repository metadata in `/var/cache/zypp`. It changes no package and no
  repository file.

## Examples

Install nginx and certbot, then start nginx with its configuration:

```yaml
- task_block:
    id: nginx
    changes:
      - zypper_package:
          packages:
            - {name: nginx, version: newest_when_first_installed}
            - {name: certbot, version: newest_when_first_installed}
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

On the node, the `zypper_package` task installs the two packages and their
dependencies with one `zypper install`. The `file` task writes the
configuration, and the `systemd_service` task starts nginx. At a later run,
the `zypper_package` task is unchanged. When a package update, in this
playbook or another, has brought a new nginx version since, the
`systemd_service` task restarts nginx once, because the node task block
tracks the installed versions of the packages that its tasks name.

Remove Apache and the packages that depend on it:

```yaml
- zypper_package:
    packages:
      - {name: apache2}
    state: absent
    also_remove_packages_that_depend_on_these: true
```

Update the whole system:

```yaml
- zypper_package:
    upgrade_all_packages: true
```

## How it works

**Installing.** The module reads the installed packages with `rpm -qa`. When
every package is installed at its entry's rule, the task is unchanged and
contacts no repository. Otherwise the module checks that no name is one that
an installed package provides (`rpm -q --whatprovides`), waits for zypper's
lock, refreshes the repositories (`zypper refresh`), asks zypper for the whole
transaction (`zypper install --dry-run`, with `--xmlout`), and checks that
each missing name is a package of its own. It installs with `zypper
install`, reads `rpm -qa` again, and reports every version that differs. An
entry with a version asks for `name=version`.

**Removing.** zypper removes the packages that depend on a named package by
itself. The module previews the removal with `zypper remove --dry-run`, and
without `also_remove_packages_that_depend_on_these` a removal that would
take more than the named packages fails here. A name that is not installed
under its own name, but that an installed package provides, fails the task:
`zypper remove` removes the provider, such as `zypper remove 'libz.so.1()(64bit)'`
removes every package that uses the library. The module does not remove the
dependencies that nothing needs any more (`zypper remove --clean-deps`).

**Updating the whole system.** The module refreshes the repositories and
asks zypper with `zypper update --dry-run` what is newer. With nothing
newer, the task is unchanged. Otherwise it runs `zypper update`. zypper
keeps each package's vendor and architecture, skips the packages that a lock
holds back, and says so in its output. `zypper dist-upgrade`, which changes
vendors and repositories for a release upgrade, is not offered.

**zypper's lock.** zypper takes `/run/zypp.pid` while it reads or changes
packages, and a second zypper ends with exit code 7 and names the pid of
the first. The file stays after zypper ends, so its existence says nothing.
Every zypper command of the module runs with the environment variable
`ZYPP_LOCK_TIMEOUT=60`, which makes zypper itself wait up to 60 seconds for
the lock: for the dry run, for the refresh, and for the transaction alike
(observed on openSUSE Leap 16.1). After 60 seconds zypper ends with exit code
7, and the task fails with zypper's message that names the holder. The lock
goes when its holder ends, also when the holder was killed.

**Nothing prompts.** zypper runs with `--non-interactive`, which takes the
default answer of each question: a question about file conflicts, an
unsigned package, an unknown repository key, and a license are all answered
no, and the transaction ends with exit code 8 or 4. The module never passes
`--no-gpg-checks`, `--replacefiles`, `--oldpackage`, or
`--auto-agree-with-licenses`. A configuration file that was changed on the
node stays, and rpm writes the package's new file as a `.rpmnew` file. A
`file` node task owns a configuration file's contents.

**Exit codes.** The module treats exit code 0, and the advice codes 102
(reboot needed, which the reason repeats) and 103 (zypper itself was
updated), as a transaction that worked; 104 as a name that no repository
has; 4, 7, and 8 as failures with zypper's own message; and 107 as the
failure of a script after the packages changed. MMX does not try a task
again after 107, because another try would find the packages installed and
report an unchanged task.

**A service restarts when its package changes.** Inside a
[node task block](../../docs/changing-mmx/design/execution.md#node-task-blocks-complete-the-change),
the version of each package that the task names, or `absent`, is a
**tracked input**: MMX remembers it with the service's record, and a
`systemd_service` task in the block's `activate` section restarts the
service when the version differs from the one that the service last
loaded. A dryrun predicts the restart from the versions that zypper would
install.

## Errors and what they mean

Every reason starts with `zypper_package`.

| The reason holds | What it means, and what to do |
|---|---|
| `the program zypper is not installed; zypper_package works only on a node whose package manager is zypper ...` | The node is not an openSUSE node. A run fails without another try. Give the node task a condition on the node fact `package_manager`, as [Set up](#set-up) shows. |
| `zypper_package.packages: the repositories offer no NAME ...` | Also after a refresh, no repository of the node has the package or the version. A run fails without another try and changes no package. Correct the name; `zypper search` searches the repositories. A package from a repository of your own needs a [`zypper_repository`](../zypper_repository/README.md) task before this one. |
| `zypper_package.packages: NAME is a name that another package provides, or a package's name with a version or an architecture, not a package of its own; ...` | zypper would install other packages under other names, or none because a package that provides the name is installed, and the task would try again on every run. A name such as `tree-2.2.1` or `tree.x86_64` is read by zypper and rpm as the package `tree`, so a removal by it would report unchanged while the package stays. Name the packages themselves, and put a version in `version`. |
| `zypper_package.packages: NAME may hold only letters, digits, and . _ + - ...` | A pattern such as `php*`, a path such as `/usr/bin/php`, a capability such as `libz.so.1()(64bit)`, a version after the name, or a repository in front of the name. zypper reads each as another request, and `zypper remove` would remove every package that provides it. Name the package; put a version in `version`. |
| `zypper_package.packages: NAME is installed at VERSION, a version that zypper does not replace ...` | The entry asks for an older version than the installed one. The module never downgrades. Remove the package first, in an earlier `zypper_package` task with `state: absent`, or change the version. |
| `removing NAME would also remove ..., which depend on them; ...` | Other installed packages need the named ones. Nothing changed. Add those packages to `packages`, or add `also_remove_packages_that_depend_on_these: true`. |
| `another zypper still holds its lock /run/zypp.pid after 60 seconds: System management is locked by the application with pid N ...` | Another zypper runs, such as one that a person or a timer started. zypper waited 60 seconds for it. MMX tries the task again. `ps -p N` names the program. |
| `refreshing the repositories: ...` | `zypper refresh` failed, such as when a repository's server does not answer, or a repository's signing key is not imported (`New repository or package signing key received`). The text behind the colon is zypper's message. MMX tries the task again. Add the key with [`zypper_repository`](../zypper_repository/README.md); the module never trusts a key by itself. |
| `previewing the installation of NAME: Problem: ...` | zypper's solver found that the package conflicts with an installed one, or needs a package that no repository has. The text is zypper's description of the problem. A dryrun reports it too. |
| `installing NAME: the file PATH is in both A and B, with different content` | Two packages of the transaction own one file with different content. Nothing was installed. A dryrun does not see the conflict. |
| `installing NAME: ... Signature verification failed ...` | A package has no signature or a wrong one, or its key is not imported. Nothing was installed. A dryrun does not see this. |
| `installing NAME: ...`, `removing NAME: ...`, `updating all packages: ...` | zypper's transaction failed; the text behind the colon is zypper's message. MMX tries the task again. |
| `the packages changed, and a script of a package failed (exit code 107) ...` | rpm installed or removed the packages, and a script of a package ended with an error. The reason lists what changed. MMX does not try the task again. Read the message, repair the package's script or its configuration, and run the playbook again; the task is then unchanged. |
| `... ended, and then reading the installed packages failed: ...` | zypper's command worked, and the read after it failed. MMX does not try the task again, because a new try would find the packages installed and report an unchanged task. Run the playbook again. |

A task that failed after zypper started a transaction is `changed`: MMX
cannot see whether a refused transaction changed the node.

## Tested on

**Distributions:** openSUSE Leap 16.1.

The integration suite `tests/integration/zypper_packages.py` runs the module
on an openSUSE Leap 16.1 test node, with zypper 1.14. Tumbleweed and
earlier Leap releases are not tested.

[Limits on each distribution](../../docs/reference/limits-on-each-distribution.md) says what is limited for this module on [openSUSE Leap 16.1](../../docs/reference/limits-on-each-distribution.md#opensuse-leap-161), and why.

## Why it works this way

- **Entries with `name` and `version`, as `apt_package` has them.** The
  concept is the same: a person says whether an installed package stays,
  follows the newest version, or has a version. zypper's repositories can
  offer several versions of a package, and `zypper install name=version`
  selects one.
- **No downgrade and no hold.** A downgrade needs `--oldpackage`, a forcing
  option, and MMX holds a version for `apt_package` with a file that apt
  reads. zypper's equivalent is a lock, which a person can write with
  `zypper addlock` and which also stops removal and updates; a module that
  wrote locks would have to track and release them.
- **The names are checked before the module refreshes or changes anything.**
  A name that zypper reads as a pattern, a path, or a capability fails
  first, because `zypper remove` removes whatever provides a capability. A
  name that only a refresh shows to be unknown fails after the refresh, since
  an earlier node task may have added the repository; a refresh changes no
  package.
- **A removal that takes other packages states so.** zypper removes the
  packages that depend on a package by itself, so the task previews the
  removal and asks for `also_remove_packages_that_depend_on_these`, as every
  package module does.
- **`zypper update`, not `zypper dist-upgrade`.** `zypper update` keeps each
  package's vendor and architecture and never removes a package to resolve a
  conflict. `zypper dist-upgrade` is for Tumbleweed and for a move to a new
  Leap release, where changing vendors is the point; a person who wants it
  writes a `shell` task for it.
- **The module never forces a transaction.** `--non-interactive` answers
  zypper's questions with the safe answer, and the module passes none of the
  options that override it.
