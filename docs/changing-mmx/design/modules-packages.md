# MMX: package modules

Part of the [main plan](README.md). `docs/changing-mmx/design/modules-packages.md` owns
the design shared by the modules that install software on a node: the
package modules of each distribution family, their package sources, and the
core modules for the common language and tool installers. Each module's own behavior, examples, and
reasons are on its page; the [module index](../../reference/modules/README.md) lists
every page.
[Modules](modules.md) owns the contract every core module follows.

| Module | Manages |
|---|---|
| [`apt_package`](../../reference/modules/apt_package.md) | Packages on Debian and Ubuntu, through apt: present, absent, kept inside a version pattern, brought to the newest version, or installed from a package file; or a whole-node upgrade. |
| [`apt_repository`](../../reference/modules/apt_repository.md) | One apt source file, its signing key, and its login for a private repository. |
| [`apt_downloaded_package_files`](../../reference/modules/apt_downloaded_package_files.md) | The package files that apt downloaded into its archives directory, deleted with `apt-get clean`. |
| [`dnf_package`](../../reference/modules/dnf_package.md) | The same for the Red Hat family, through dnf 4. |
| [`dnf_repository`](../../reference/modules/dnf_repository.md) | One dnf repository file and its signing key; or whether a repository that is already on the node is switched on. |
| [`pip_package`](../../reference/modules/pip_package.md) | Python packages in a virtual environment or system-wide, by name or from a requirements file. |
| [`gem_package`](../../reference/modules/gem_package.md) | Ruby gems installed with `gem`. |
| [`npm_global_package`](../../reference/modules/npm_global_package.md) | Node.js command-line tools installed with `npm install --global`. |
| [`snap_package`](../../reference/modules/snap_package.md) | Snaps and the channel each one follows. |
| [`npm_project_dependencies`](../../reference/modules/npm_project_dependencies.md) | What an application's `package-lock.json` lists, installed with `npm ci`. |
| [`yarn_project_dependencies`](../../reference/modules/yarn_project_dependencies.md) | What an application's `yarn.lock` lists. |
| [`bundler_project_dependencies`](../../reference/modules/bundler_project_dependencies.md) | What an application's `Gemfile.lock` lists. |
| [`composer_project_dependencies`](../../reference/modules/composer_project_dependencies.md) | What an application's `composer.lock` lists. |
| [`uv_project_dependencies`](../../reference/modules/uv_project_dependencies.md) | What a Python application's `uv.lock` lists, installed with `uv sync --locked`. |

## One module for each tool

MMX does not hide which tool does the work. A node that uses apt gets
`apt_package` tasks, a node that uses dnf gets `dnf_package` tasks, and each
module has exactly the fields its tool supports. No field means something
different on another package manager, and no task silently does nothing on
a node whose package manager lacks the operation.

Most projects run one distribution, so their roles name one package module.
A project with two distributions puts a condition on the node task. A node
fact is a value describing the node. The node fact `.node.package_manager`
is `apt`, `dnf`, `pacman`, or `zypper`:

```yaml
- when:
    equals: ["{{ .node.package_manager }}", apt]
  apt_package:
    packages:
      - {name: apache2, version: newest_when_first_installed}

- when:
    equals: ["{{ .node.package_manager }}", dnf]
  dnf_package:
    packages:
      - {name: httpd, version: newest_when_first_installed}
```

A package task that runs on a node with another package manager fails with a
message that tells the user to give the node task a condition on the node fact
`package_manager`, so that it runs only on nodes that use its package manager.

`dnf_package` and `dnf_repository` work with dnf 4 and dnf 5 in one module;
the next section says how dnf 5 differs.

Module pages that rely on this section:
[`apt_package`](../../reference/modules/apt_package.md),
[`dnf_package`](../../reference/modules/dnf_package.md).

## Package modules for each family

Each distribution family has its own package modules. A family is a group
of distributions that share a package manager and their package names. The
[supported platforms](development.md#tested-distributions) list the
distributions on which MMX tests each family.

| Family | Package modules | Kind | Other modules of the family |
|---|---|---|---|
| Debian (Debian, Ubuntu) | `apt_package`, `apt_repository`, `apt_downloaded_package_files` | Core module, Go | `ufw`, `netplan_file`, `ifupdown_interfaces_file` |
| Red Hat (Rocky Linux, AlmaLinux) | `dnf_package`, `dnf_repository` | Core module, Go | `firewalld`, `firewalld_zone`, `firewalld_zone_membership`, `selinux_boolean`, `selinux_file_context`, `selinux_mode`, `selinux_port`, `networkmanager_connection_file` |
| Arch Linux | `pacman_package` | Official extra module, Starlark | None yet |
| SUSE (openSUSE Leap) | `zypper_package`, `zypper_repository` | Official extra module, Starlark | `firewalld` |

Each module's page says on which releases of its family the module is
tested.

- **The Debian family's modules** are core modules. `apt_package` is
  intricate common work: it waits for the dpkg lock, keeps version rules,
  simulates upgrades, installs a package without starting its services,
  and checks package entries across one fleet stage on the controller
  machine. Most users of MMX depend on `apt_package`, and Go's compiler and
  the release's static analysis check it.
- **The Red Hat family's modules** are core modules too, and share the
  package code with `apt_package`. `dnf_package` and `dnf_repository` drive
  dnf 4, the dnf of Rocky Linux 9 and AlmaLinux 10, and dnf 5, the dnf of
  Fedora, in one module each.
- **dnf 5 differs in six places that the modules read or write**, found by
  running every dnf command of the modules on Fedora 45 (dnf 5.4.7):
  the version lock is a TOML file, `/etc/dnf/versionlock.toml`, that is
  built in and has no wildcard comparator, so a rule that ends in `.*`
  becomes a range of versions and other wildcards are refused; no option
  makes dnf read another lock file, so a preview of a task that changes or
  drops a rule MMX wrote runs with excludes switched off (which hides the
  administrator's lock tables and the repositories' excludes from it, so MMX
  checks the planned versions against the administrator's tables first); an upgrade
  in a transaction table adds a `replacing` line; `config-manager` has the
  commands `enable` and `disable`; advisory types are words; and `install`
  leaves an installed package alone, where dnf 4's upgrades it, so
  `newest_on_every_run` installs the newest version by name and version. rpm 6, which
  Fedora 45 ships, names a key's package by its whole fingerprint, so the
  repository module finds a key by the end of the version.
- **The Arch Linux and SUSE families' modules** are official extra
  modules in Starlark, as every package module for a family other than
  Debian and Red Hat is ([below](#package-modules-in-starlark)).
  `pacman_package`, `zypper_package`, and `zypper_repository` are built.

## Package modules in Starlark

A package module for a family other than Debian and Red Hat is an official
extra module: a Starlark script that calls MMX's
[primitives](modules.md#primitives), such as `pacman_package`.
[Where a module belongs](modules.md#where-a-module-belongs) gives the reasons.

A Starlark package module gets what every script gets. It passes the same
[module behavior contract](modules.md#every-module-follows-the-same-contract)
and the rules below as a Go one: an unchanged second run, a dryrun that
changes nothing and shows the difference, clear errors, and a node task
block that restarts a service when its package changes. Whatever the MMX
project uses to write a package module, a user has too, so a user can write
a package module for a package manager that MMX ships no module for, as a
module of the project's own. The
[Write a package module](../../how-to/write-a-package-module.md)
walks through one, built from `pacman_package`.

A package module needs one primitive that no other script needs:
`run_command_that_refreshes_a_package_index`, which downloads the package
manager's index in a run without counting as a change of the task, as
`apt_package`'s own refresh does not count. Without it, a whole-system
upgrade with nothing to upgrade would report a change on every run. Every
package manager refreshes an index, so the primitive serves every package
module. A package manager's lock needs no primitive of its own: a bounded
loop over `inspect_path` waits for it.

A Starlark package module lacks two checks that a core package module has:

- **The check across a fleet stage.** A core package module checks on the
  controller machine that two node tasks of one fleet stage do not ask for
  conflicting versions of one package, such as a version rule in one task
  and a version keyword in another. The check uses a hook that only core
  modules have.
- **Go's checks.** Go's compiler and the release's static analysis check a
  core module's code. A script's mistakes show when the script runs, so its
  tests must reach every path.

`apt_package` and `dnf_package` stay core modules in Go for these checks:
their version rules are intricate, and most users depend on them.

## What every package manager module does

The rules under **What every package manager module does** hold for every
package module of every family, core or Starlark, except where the package
manager's own rules differ; [pacman's own rules](#pacmans-own-rules) lists
pacman's differences. Each module's page lists the module's fields and
where the module's tool differs.

- **Inspect first.** The module reads the installed state from the package
  manager's own database and runs the package manager only when something
  must change. An unchanged task contacts no package server.
- **Each present package states its version policy.** `packages` is one
  list of entries with required `name` and `version`. A version or pattern
  keeps a rule; `newest_when_first_installed` keeps an installed version;
  `newest_on_every_run` follows upgrades. An absent entry gives only `name`.
  The [required-settings rule](modules.md#a-setting-with-no-safe-guess-is-required)
  owns why MMX does not guess.
- **One task, one install transaction.** MMX checks every entry and package
  file before writing any version rule. When an installation is needed,
  MMX first previews the complete transaction against temporary copies of
  the proposed rules. MMX then writes all rules and installs all entries
  with one native command. Related packages, including files, can satisfy
  dependencies without depending on list order. `dnf_package` enables a
  requested dnf module stream before version resolution, because dnf hides
  the stream's packages until then.
- **The result covers the complete transaction.** `value` reports every
  package changed by the native command, including dependencies outside the
  task's list. Hiding entries for changed packages would conceal part of
  the task's effect.
- **Package entry mappings are written in the task.** Entries can hold
  files from the controller machine, which must be prepared while the
  project loads. Whole-list and whole-entry references are refused so the
  node cannot receive file names without assets. Individual fields may use
  references.
- **Only MMX's rules belong to MMX.** A version keyword removes only the
  rule MMX wrote. apt uses its own preferences file; dnf marks the line
  that MMX owns with a comment before it. Administrator rules
  remain in candidate selection, including when MMX drops its own rule for
  an upgrade.
- **Removal states its scope.** A native simulation checks every removal
  before any change. Removing a package outside the task's `packages` list
  requires `also_remove_packages_that_depend_on_these: true`, per
  [protective choices](modules.md#protective-choices-come-in-three-levels).
- **Whole-node upgrades name their effect.**
  `upgrade_all_packages_within_their_version_rules: true` upgrades every
  package inside MMX's and administrator rules. dnf permits task-only
  repositories here too.
- **The package index is the module's business.** There is no field that
  refreshes the index. The module refreshes the package index when an
  installation or upgrade needs the index, at most once per node job, and
  again after a task in the same node job changed a package source.
- **The module waits for the package manager's lock** for a bounded time, so
  another process that holds the lock (a cloud image's first-boot setup, the
  distribution's automatic updates) does not fail the run.
- **Nothing prompts.** The package manager runs without a terminal. The
  package manager keeps a configuration file that was changed on the node.
  A `file` task owns the configuration file's contents.
- **Dryrun changes nothing and refreshes nothing.** MMX reports what the task
  would install, upgrade, or remove, judged against the index the node has
  now. If the package manager cannot find a package in the node's current package indexes,
  the package's installation is not checked: an earlier node task may
  only preview the source that supplies the package. See
  [apt's package index](../../reference/modules/apt_package.md#installation-and-the-package-index)
  and [dnf's package index](../../reference/modules/dnf_package.md#the-package-index-and-the-lock).
- **Versions restart services.** Inside a node task block, the names,
  installed versions, and absence of the packages declared by tasks in the
  block's `changes` section are tracked inputs. MMX remembers the tracked
  inputs with the service's record. A `systemd_service` task in the block's
  `activate` section restarts the service when the tracked inputs differ
  from what the service last loaded, also when the run that changed the
  packages stopped before the restart. The
  [execution contract](execution.md) owns the service record.
- **Removing a task removes nothing on the node.** The package stays, and so
  does a version rule that MMX wrote for it. Removal is a task with
  `state: absent`.
- **A package module never orders versions itself.** Which version is newer,
  and which version a pattern selects, are the package manager's answers.

Module pages that rely on this section:
[`apt_package`](../../reference/modules/apt_package.md),
[`dnf_package`](../../reference/modules/dnf_package.md),
[`pacman_package`](../../../official_extra_modules/pacman_package/README.md).

## pacman's own rules

Arch Linux supports no partial upgrade: the package databases, pacman's
lists of what the package servers offer, and the installed packages must
come from the same moment, because a package from newer databases can need
newer libraries than the system has. Arch Linux's repositories also offer
only the newest version of each package. `pacman_package` follows both
rules, so it differs from the shared rules above:

- **Every installation is a full system upgrade.** A task that installs a
  missing package previews the transaction against the databases the node
  has, refreshes them (`pacman -Sy`), previews again, and installs with
  `pacman -Su --needed`, which is `pacman -Syu <packages>` in two steps. The
  first preview lets a group's name or a name that another package provides
  fail before the refresh; a name that no repository has fails after the
  refresh, before any package changes. A task whose packages are all
  installed runs only `pacman -Q`.
- **The preview resolves names and dependencies, and nothing else.**
  `--print` checks no conflict between packages: pacman finds a conflict
  when the transaction runs, fails before it changes a package, and the
  reason names the packages in conflict. A name in `IgnorePkg` is
  installed, because `--noconfirm` answers pacman's "install anyway?" with
  yes. A package that replaces an installed one may be missing from a
  dryrun's list.
- **A failed transaction leaves newer databases.** After the refresh, a
  transaction that fails, or a name that no repository has, leaves the
  databases newer than the installed packages. The next installation or
  whole-system upgrade brings both together; the module never installs
  from the databases without upgrading.
- **A group or a provided name is refused also in a removal.** A name that
  is not installed under its own name, but is a group with installed
  members (`pacman -Qg`) or a name that an installed package provides
  (`pacman -T`), fails the task; otherwise the task would remove nothing and
  report unchanged.
- **No version setting.** `packages` is a list of names. pacman has no rule
  that keeps a package inside a version pattern; its `IgnorePkg` holds a
  package back during an upgrade, which is a partial upgrade, and an
  administrator who chooses it writes it into `/etc/pacman.conf`.
  `upgrade_all_packages: true` brings every package to the newest version;
  there is no keyword for one package.
- **No check across a fleet stage.** With no version rules, two tasks
  cannot ask for conflicting versions, so the controller machine's check
  of a core package module has nothing to check.
- **The databases are refreshed for every installation and every
  whole-system upgrade,** not once per node job. pacman downloads a
  database only when the package server has a newer one. The refresh is no
  change of the task, through the primitive
  `run_command_that_refreshes_a_package_index`, so a whole-system upgrade
  with nothing newer is unchanged.
- **A removal previews with `pacman -Rcp`,** which also lists the packages
  that depend on the named ones. Without
  `also_remove_packages_that_depend_on_these: true`, a removal that would
  take them fails before any change; with it, pacman removes them with
  `-c`. pacman removes a package's configuration files and keeps a changed
  one as a `.pacsave` file, so there is no input to delete configuration
  files. There is no input for the dependencies that nothing needs any
  more.
- **The lock is `/var/lib/pacman/db.lck`.** Before each command that takes
  the lock, the module waits up to 60 seconds, as `apt_package` waits for
  dpkg's lock, then fails with a reason that says that a pacman that was
  stopped leaves the file. Reading and `--print` take no lock.
- **Packages start no service.** Arch Linux's packages neither start nor
  enable a service, so `pacman_package` needs no counterpart to
  `apt_package`'s `policy-rc.d` program.
- **The result is the output `changed_packages`,** because a script returns
  named outputs, not the `value` of a core module. It holds the same
  `{name, from, to}` entries, from `pacman -Q` before and after the
  transaction.

Module pages that rely on this section:
[`pacman_package`](../../../official_extra_modules/pacman_package/README.md).

## zypper's own rules

openSUSE's zypper resolves a request, not a package name: `zypper install`
and `zypper remove` take patterns (`php*`), paths (`/usr/bin/php`),
capabilities (`libz.so.1()(64bit)`), and `name=version`, and
`zypper remove` removes every package that provides such a capability. Its
dry run (`--dry-run`) answers like a run for the solver and not for the
files. `zypper_package` and `zypper_repository` follow the rules above,
except for these differences:

- **A package entry is a name and a version, as in `apt_package`.**
  `newest_when_first_installed`, `newest_on_every_run`, or a version such
  as `1.28.1` or `1.28.1-160100.2.1` that the installed package must have.
  zypper's repositories offer several versions of a package, so the concept
  is the same as apt's. `newest_on_every_run` is `zypper install NAME`,
  which updates an installed package, and an exact version is
  `zypper install NAME=VERSION`.
- **An exact version is installed or updated to, never held and never
  downgraded.** zypper does nothing, with exit code 0, when the installed
  version is newer than `NAME=VERSION`, and downgrades only with
  `--oldpackage`, a forcing option the module never passes. The module
  asks zypper's dry run: a package that is installed at another version
  than its entry wants, and that zypper's dry run does not update, fails
  the task as `invalid_inputs`. The module orders no versions itself. zypper
  locks (`zypper addlock`) hold a version, and a person writes them; the
  whole-system update respects them.
- **Names are narrow.** A name is an RPM package name, letters, digits, and
  `. _ + -`. A pattern, a path, a capability, an operator, and a repository
  in front of the name fail before anything runs, because zypper reads each
  as another request. A plain name that is not installed under its own name
  but that zypper still resolves to an installed package is refused too, in
  an installation and in a removal, before the refresh: `rpm -q NAME` finds
  a package's name with its version or architecture (`tree-2.2.1`,
  `tree.x86_64`, which zypper's `--name` option does not make exact either),
  and `rpm -q --whatprovides NAME` finds a name that an installed package
  provides (`awk`). Otherwise a removal would report unchanged while the
  package stays, or remove the provider. After the refresh, the dry run's
  list of packages to install must hold each missing name. The tests use the
  names `tree-2.2.1`, `tree.x86_64`, `libz.so.1()(64bit)`, and
  `mmxtest-capability`.
- **Newest means the newest from the installed package's vendor.** zypper
  never changes a package's vendor in `zypper install NAME` or `zypper
  update`. When a newer version exists only from another vendor, its dry run
  prints `There is an update candidate for 'X' from vendor 'B', while the
  current vendor is 'A'` and plans nothing. The task is then unchanged, and
  its reason says `not updated:` and quotes the remark, so a person sees why
  a package is not updated. It is an unchanged task with a reason, the one
  case where an unchanged task has one.
- **The task refreshes the repositories in a run, and a repository module
  adds nothing to refresh.** A task with work runs `zypper refresh` through
  `run_command_that_refreshes_a_package_index`, then previews and
  changes with `--no-refresh`, so one task contacts the servers once. An
  unchanged task contacts none. `newest_on_every_run` and
  `upgrade_all_packages` refresh on every run. A repository that a
  `zypper_repository` task added is found because the next task with work
  refreshes. A name that the metadata of the node does not know fails a
  dryrun as `not_checked` and a run, after the refresh, as `invalid_inputs`.
  Unlike pacman, a refresh never leaves a half-upgraded system, so the
  module may refresh before it knows that every name exists.
- **zypper's dry run checks the solver, not the files.** It finds a package
  that declares a conflict with an installed one, a missing dependency, a
  lock that blocks the request, and an unknown name. It downloads nothing,
  so it checks neither the files that two packages share (a run fails with
  exit code 8 and `conflicts with file from install of`) nor the signature
  of a package (exit code 8, `Signature verification failed`), nor the
  repository's server beyond the metadata. Both failures leave the node's
  packages as they were, and the task is `changed`, as for every refused
  transaction. The page says that a dryrun does not see them.
- **A removal removes dependents by itself.** zypper removes the packages
  that depend on a named package without asking, so the module previews
  the removal and, without `also_remove_packages_that_depend_on_these`,
  fails before any change when the preview lists more than the named
  packages. It never passes `--clean-deps`.
- **The whole-system update is `zypper update`.** It keeps each package's
  vendor and architecture and never removes a package to resolve a conflict.
  `zypper dist-upgrade` is for Tumbleweed and for a release upgrade, where
  changing vendors is the point; it is not offered.
- **The lock is held by a running zypper, not by a file.** `/run/zypp.pid`
  exists after zypper ends and is then empty; a held lock shows as exit code
  7 and the pid in the file. Every zypper command of the module runs with
  `ZYPP_LOCK_TIMEOUT=60`, so zypper itself waits up to 60 seconds, for the
  dry run, the refresh, and the transaction alike (observed on openSUSE Leap
  16.1), and the task fails with zypper's message when the lock is still held.
  A dryrun takes the lock too, because zypper's dry run does, and may rebuild
  zypper's cache files. A lock is released when its holder ends, also when it was killed,
  so no stale lock exists and the page needs no advice to remove a file.
- **zypper's exit codes are tested one by one.** 0 works; 102 (reboot
  advised) and 103 (zypper itself was updated) work and are in zypper(8) but
  were not observed on a test node; 4 is a solver problem, a repository that
  did not answer, or a key that is not imported; 7 is the lock; 8 is a
  transaction that failed (file conflict, signature); 104 is a name that no
  repository has; 107 is a script of a package that failed after the
  packages changed. After 107 the task fails with `try_again: False`, because
  another try would find the packages installed and hide the failure. 105
  and 106 are not tested.
- **A failure's message is read from zypper's XML output (`--xmlout`),**
  not from its text: the messages of type error and warning, the description
  of a prompt, and the structure of a file conflict (`<fileconflict>` with
  the file and both packages). The preview parser expects zypper 1.14's
  plain section tags, one on each line. Anything else in the output is not
  read.
- **Nothing prompts, and nothing forces.** `--non-interactive` answers each
  question with its default, which is no for a file conflict, an unsigned
  package, an unknown key, and a license. The module passes none of
  `--no-gpg-checks`, `--replacefiles`, `--oldpackage`, and
  `--auto-agree-with-licenses`.
- **Packages start no service on openSUSE,** as on Arch Linux, so
  `zypper_package` needs no counterpart to `apt_package`'s `policy-rc.d`
  program.
- **No patterns, patches, or products.** `-t pattern`, `-t patch`, and
  `-t product` are not offered. Patterns are metapackages whose members
  `zypper` installs; a task names the members, or the `patterns-*` package
  that installs them, which is an ordinary package.
- **The repository module writes the repository file and the key.**
  `zypper_repository` writes `/etc/zypp/repos.d/NAME.repo` with every
  setting spelled out and a signing key that the task gives as text or as an
  HTTPS address with its SHA-256, as `apt_repository` does for apt. Unlike
  apt, whose `Signed-By` ties a key to one source, zypper trusts the keys of
  rpm's keyring for every repository, so the module imports the key into
  rpm (`rpm --import`) before it writes the file. It records the ids that it
  actually imported in `NAME.imported_key_ids`, and `state: absent` removes
  only those from rpm (`rpm -e gpg-pubkey-ID`), and only when no other
  repository's key file holds them: a key that rpm held before the task, such
  as the distribution's own, is never recorded and never removed, because
  removing it would stop security updates, and a key that two repositories
  share goes with the last of them (a repository records an id that another
  repository recorded). A task that gives another key for a repository
  imports it and removes the old recorded ids under the same rule. The module never lets zypper
  import a key: a non-interactive zypper refuses an unknown key, which is
  the safe default. `repository_metadata_is_signed` is required because
  only a repository's vendor knows whether it signs its metadata;
  package signatures are always checked. gpg reads the key's identity in a
  home of its own, `/etc/zypp/mmx_repository_keys/gnupg_home`, so that no
  call creates `/root/.gnupg`. `repository_metadata_is_signed: false` is
  refused with an `http://` address, because nothing then stops someone on
  the network from serving an old list of packages, with old but correctly
  signed packages, and so from keeping back security updates. The module cannot read a controller
  machine's file, because a Starlark script has no primitive for that; a key
  goes in as text, or an HTTPS address.
- **The result is the output `changed_packages`,** with the `{name, from,
  to}` entries of `pacman_package`, from `rpm -qa` before and after the
  transaction. A package installed in several versions (kernels) or for
  several architectures shows its versions joined with `, `.

Module pages that rely on this section:
[`zypper_package`](../../../official_extra_modules/zypper_package/README.md),
[`zypper_repository`](../../../official_extra_modules/zypper_repository/README.md).

## apt's downloaded package files

apt keeps each package file that it downloads in its archives directory,
`/var/cache/apt/archives/`, which can fill a small disk.
[`apt_downloaded_package_files`](../../reference/modules/apt_downloaded_package_files.md)
deletes apt's downloaded package files with `apt-get clean`.

| Decision | Reason |
|---|---|
| The node task writes `state: absent`, its only state. | The task reads as the task's effect, as every removal in MMX does. |
| MMX first lists the files and sizes in the archives directory named by `apt-config` and in the archives directory's `partial/` directory. MMX then runs `apt-get clean`, lists the directories again, and reports the number and total size of files present before cleanup and missing after cleanup. A dryrun reports the files and bytes currently in the directories. Without package files the task runs nothing and is unchanged. | apt's own cleanup knows which files to delete. Comparing the directory listings before and after cleanup gives exact counts of deleted files and bytes without reading apt's text. |
| MMX retries `apt-get clean` every two seconds for up to a minute while the command fails at once because another apt holds a lock. The wait is the same as for a refresh of the package lists. | `apt-get clean` takes the locks of the archives directory and of the package lists without waiting, and apt's daily timer and cloud-init hold the locks just after a server first boots. |

## Files fetched from a web address

A package file (`package_file_url`) and a repository's signing key
(`signing_key_url`) are fetched the way [`download`](../../reference/modules/download.md)
fetches a file:

- The address is HTTPS, with no login and no fragment.
- The task gives the file's SHA-256 (`package_file_sha256`,
  `signing_key_sha256`). The controller machine fetches each address once
  per run, verifies the bytes, freezes the bytes with the run, and
  sends the bytes to the nodes that use the file. A file that an earlier
  run of the project saved is reused after the file's SHA-256 is checked
  again, so an unchanged run does not fetch the file again. A failed or
  mismatched fetch fails before any node job starts. `validate` and
  `explain` do not fetch; `dryrun` fetches but changes no node; `mmx resume`
  uses the saved bytes.
- The address and the SHA-256 may contain references to values the controller
  machine knows before it connects: project and inventory variables, role
  inputs, and command-line variables. A release's version number and its
  checksum can therefore live in one place:

  ```yaml
  # inventories/production.yml
  vars:
    rclone_version: "1.68.2"
    rclone_package_sha256: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
  ```

  ```yaml
  - apt_package:
      packages:
        - {name: rclone, version: "{{ .vars.rclone_version }}", package_file_url: "https://downloads.rclone.org/v{{ .vars.rclone_version }}/rclone-v{{ .vars.rclone_version }}-linux-amd64.deb", package_file_sha256: "{{ .vars.rclone_package_sha256 }}"}
  ```

  A value that is known only on a node, such as a fact, is not allowed in
  the address or SHA-256 fields.
- A task that has no checksum must say so in words:
  `dangerously_skip_checksum: true` in place of the SHA-256. The controller
  machine fetches the file again in every run, because the bytes at the
  address may change, and sends every node the same
  bytes, but nothing establishes that those bytes are the publisher's. A task
  with neither a SHA-256 nor `dangerously_skip_checksum: true` is rejected
  at validation.

Module pages that rely on this section:
[`apt_package`](../../reference/modules/apt_package.md),
[`apt_repository`](../../reference/modules/apt_repository.md),
[`dnf_package`](../../reference/modules/dnf_package.md),
[`dnf_repository`](../../reference/modules/dnf_repository.md).

## Language and tool installers

The nine language and tool installer modules listed below install software
with a language's or a tool's own installer. Each runs the installer program
that is on the node and links no library into MMX. There are two kinds:

- **A named thing:** `pip_package`, `gem_package`, `npm_global_package`,
  `snap_package`. The task names what to install.
- **What an application's lock file lists:** `npm_project_dependencies`,
  `yarn_project_dependencies`, `bundler_project_dependencies`,
  `composer_project_dependencies`, `uv_project_dependencies`. The task names the application's directory;
  the lock file in it says what to install.

### What these modules share

- **The installer is installed first.** `pip`, `gem`, `npm`, `yarn`, `bundle`,
  `composer`, `uv`, and `snap` come from an earlier task, normally a package task. When the program is missing, the task fails with a message that names
  it; dryrun reports the task as not checked, because an earlier task may
  install it.
- **`run_as_user_account`** names the local account that runs the installer.
  MMX starts the installer's processes directly as the account named in
  `run_as_user_account`, as `shell` does with `run_as_user_account`: no login
  shell, no profile files, and the fixed base environment with `HOME`,
  `USER`, and `LOGNAME` from the account.
  The lock file modules, `pip_package`, `gem_package`, and
  `npm_global_package` require the field, because installers run package
  code. The task must choose the account, including `root`, under the
  [required-settings rule](modules.md#a-setting-with-no-safe-guess-is-required).
  `snap_package` has no such field: snaps are
  installed by root.
- **`<tool>_program_path`** (`python_program_path`, `gem_program_path`,
  `npm_program_path`, `yarn_program_path`, `bundle_program_path`,
  `composer_program_path`, `uv_program_path`) is optional and names the program to run when it
  is not the one on the standard system path: a runtime unpacked under
  `/opt`, or the program a version manager such as rbenv provides. MMX puts
  the directory of the executable named in `<tool>_program_path` first in
  `PATH` for the installer, so `npm` finds the `node` beside it. A written
  value must be absolute; leaving it out finds the program on the standard
  path.
- **`env`** is optional on every module except `snap_package`, and works as
  on `shell`: values reach the installer only through the
  installer's environment. Secrets, such as a token for a private package
  server, are allowed in `env`.
- **Unchanged means the installer has nothing to do,** and each module's
  page says how MMX finds that out. Dryrun installs nothing and lists what
  the task would change, as far as the installer can tell without installing.
  When the installer cannot predict the changes, the task is not checked,
  and the dryrun predicts that the node task block's service restarts.
  A predicted restart that does not come is safer than a restart the dryrun
  missed.
- **Tracked inputs.** Inside a node task block, what the task installed is a
  tracked input, as package versions are for `apt_package`: a
  `systemd_service` task in the block's `activate` section restarts the
  service when the tracked installed software changed, also after an
  interrupted run. Each page says what exactly is tracked.
- **Retries** follow the rule for core modules: a failed installer run is
  repeated a bounded number of times unless the node task sets `retry`.

A typical use, with the restart:

```yaml
- task_block:
    id: myapp
    changes:
      - git:
          repo: https://github.com/example/myapp.git
          check_out_tag: v1.4.2
          path: /srv/myapp/app
          run_as_user_account: myapp
      - pip_package:
          requirements_file_on_node: /srv/myapp/app/requirements.txt
          virtual_environment: /srv/myapp/venv
          run_as_user_account: myapp
    activate:
      - systemd_service:
          name: myapp
          state: running
```

A new commit, or a changed set of installed Python packages, restarts
`myapp.service` once.

The named language installers use one `packages` list. Each entry states
`name` and `version`: an exact gem/npm version, a pip version specifier,
or a shared version keyword. `newest_when_first_installed`
leaves an installed version alone; `newest_on_every_run` requests upgrades.
Each installation task passes its entries to one installer command with
per-package versions, so entry order cannot change version selection.
Removal entries take only `name`; RubyGems also accepts an exact `version`
to remove one of the versions it keeps side by side.

Snap entries instead state `name`, `channel`, and optionally
`allow_access_outside_snap_sandbox`. Snap requires a separate command per
channel selection. Before any installation or refresh, MMX checks every
entry's confinement (Snap's access mode); a dryrun makes the same checks.
The access setting follows the shared
[protective-choice rule](modules.md#protective-choices-come-in-three-levels).

Module pages that rely on this section:
[`gem_package`](../../reference/modules/gem_package.md),
[`npm_global_package`](../../reference/modules/npm_global_package.md),
[`pip_package`](../../reference/modules/pip_package.md),
[`snap_package`](../../reference/modules/snap_package.md).

### What the lock file modules share

The five lock file modules, `npm_project_dependencies`,
`yarn_project_dependencies`, `bundler_project_dependencies`,
`composer_project_dependencies`, and `uv_project_dependencies`, install
what an application's lock file lists, into the application's directory,
as the application's account. The shared fields and installation rules for
the lock file modules are:

| Field | Meaning |
|---|---|
| `lock_file_dir` | Required. The directory on the node that holds the lock file, usually a release directory or a checkout. |
| `run_as_user_account` | Required. |
| `<tool>_program_path`, `env` | As in [what language and tool installer modules share](#what-these-modules-share). |

- **The lock file is required and is never rewritten.** Each module runs its
  installer in the mode that installs exactly what the lock file lists and
  fails when the lock file is missing or does not fit the manifest beside it
  (`package.json`, `Gemfile`, `composer.json`, `pyproject.toml`).
- **A record says what was installed.** After a successful installation, MMX
  writes a small file in the lock file directory, named after the module
  (`.mmx-npm-project-dependencies`, and so on). The installation record holds
  the SHA-256 of the lock file and the manifest, the task's settings that
  change what gets installed, and the version the installer and its
  language runtime report. The task is unchanged when the record matches
  the file hashes, installation settings, installer version, and runtime
  version, and the directory that holds the installed packages exists.
  Anything else installs again: a changed lock file or manifest, a changed
  setting, an upgraded runtime, or an installation that was interrupted,
  since the record is written last.
- **Dryrun** compares the record and reports unchanged or would change, with
  the reason (for example "`package-lock.json` changed since the last
  installation"). A lock file directory that does not exist yet reports not
  checked, because an earlier task, such as a `git` checkout, may create it.
- **Tracked input:** the record's contents.

The record is not part of the application's repository. A `git` task leaves
untracked files in a checkout alone.

When each release has its own directory, each release also has its own
installed packages, so installing the packages of the next release cannot
break the release that is running.

Module pages that rely on this section:
[`bundler_project_dependencies`](../../reference/modules/bundler_project_dependencies.md),
[`composer_project_dependencies`](../../reference/modules/composer_project_dependencies.md),
[`npm_project_dependencies`](../../reference/modules/npm_project_dependencies.md),
[`uv_project_dependencies`](../../reference/modules/uv_project_dependencies.md),
[`yarn_project_dependencies`](../../reference/modules/yarn_project_dependencies.md).

## Decisions

The decisions in the table are settled. Revisit a decision only with a
concrete workflow that the decision fails to support.

| ID | Decision | Reason |
|---|---|---|
| D29 | `apt_package` and `dnf_package` use one `packages` list with required `name` and `version` for present entries; absent entries give only `name`. Versions are rules or the explicit keywords `newest_when_first_installed` and `newest_on_every_run`. File inputs and `allow_downgrade` belong to each entry; file names and exact versions are checked. Entries are written directly in the task so files from the controller machine can be prepared; individual fields may use references. `value` includes every package changed by the native transaction, including dependencies outside the task. All package and dependency checks precede version-rule writes. Every installation is previewed as a complete native transaction against the proposed rules before any rule write; all rules precede one install command. dnf first runs checks that need no stream, enables the requested module stream, then resolves versions and previews the whole installation. A later refusal leaves the requested stream enabled so the next run can continue. dnf hides inactive streams' packages, so those checks cannot precede enablement; a dryrun enables nothing and reports the installation `not_checked`. All commands use the task's repository options. Its locks use raw native package specifications with explicit epochs and no added architecture suffixes. An apt or dnf dryrun that cannot find a package in the current indexes reports `not_checked`, because an earlier task may only preview its source installation. A real run refreshes indexes and still fails for a missing package. A version keyword deletes only MMX's rule, with comment-marked ownership in dnf. `upgrade_all_packages_within_their_version_rules: true` upgrades the whole node; dnf also allows task-only repositories. Removal uses native simulation and requires `also_remove_packages_that_depend_on_these: true` for unnamed removals. apt names its global autoremove `also_remove_packages_no_longer_needed`; dnf retains its narrower `also_remove_packages_only_it_needed`. There is no `state: latest` or byte-identical package replay. | Explicit entry policies avoid guessed upgrades; one transaction solves matching dependencies in either order; complete previews prevent rule and package changes on dependency failure; dnf stream enablement must precede hidden-package checks and remains as requested after refusal; complete results expose dependency changes; literal entry mappings prevent missing controller assets; owned rules protect administrator settings; raw lock specifications preserve version-only patterns and explicit epochs disambiguate hyphenated names; unresolved dryrun packages keep missing-repository uncertainty visible; simulation makes wider removal a deliberate choice; byte-identical replay would require a package mirror. See [required settings](modules.md#a-setting-with-no-safe-guess-is-required) and [protective choices](modules.md#protective-choices-come-in-three-levels). |
| | One module for each package manager; no module that serves all of them. | The reader sees which tool runs, and no field means two things. |
| | A file fetched from a web address has a SHA-256 in the task, or the task says `dangerously_skip_checksum: true`. | The checksum is the normal way; leaving it out is visible in review. |
| | The common language and tool installers are core modules. | The work is common, and a module can say what would change and restart the service; a shell command cannot. |
| | Language package entries state their version choice; snap entries state their channel and sandbox access. | Packages in one task can need different versions, while each upgrade choice remains visible. Snap's own command selects one channel at a time. |
| | Snap checks every entry's confinement before installing or refreshing any entry, in run and dryrun alike. | An access refusal must leave the node unchanged, and a dryrun must expose the same refusal. |
| | pip `version` accepts specifiers and newest-version keywords, and refuses environment markers (conditions in a pip requirement); conditions use the task's `when`. | A node task has one way to state a condition, so a version constraint cannot silently drop a condition. |
| | pip reads the actual installed version from Python metadata when `pip freeze` records a URL. | Keeping a URL-installed package needs its distribution version; source text cannot form a version constraint. |
| | Language installers require `run_as_user_account`, including `root`. | The account runs package code; MMX cannot safely guess which account the project intends. |
| | Installation records hash both the lock file and its manifest. | A changed manifest must reach the installer's lock-file checks instead of returning unchanged. |
| | uv passes `--group dev` or `--no-group dev`; other groups follow the application's uv settings. | A task's explicit development choice must override uv's default-group settings. |
| | A written installer program path is absolute; omission finds the program on the standard path. | A bare program name shown as a default must not look like an accepted written path. |
| | The lock file modules name their directory `lock_file_dir`, not `project_dir`. | In MMX, "project" means the MMX project on the control machine. The directory on the node is the one that holds the lock file, and the name says so. |

Other questions with a settled answer:

| Not a core module | Answer |
|---|---|
| A field that holds a package (`hold`) | `version` keeps a package through the package manager's own rule. |
| A field that refreshes the package index | The module refreshes the package index when an installation or upgrade needs the index. |
| Installing a package file with dependency checks switched off | Not offered. |
| The default among installed alternatives | [`update_alternatives`](../../reference/modules/update_alternatives.md). |
| A file of pinned downloads, and a command that writes it | The address and SHA-256 are in the task and may refer to variables. |
| Download fields for each processor architecture | Two tasks, each with a `when` on `.node.arch`. |

## Beyond the first release

- A package module for SUSE's zypper.
- cargo, conda, flatpak, and nix: a [module of the project's own](modules.md#the-projects-own-modules)
  or a `shell` task.
