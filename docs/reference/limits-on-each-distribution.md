# Limits on each distribution

This page lists, for each distribution that MMX is tested on, what is
limited there and why. The controller machine is where you run `mmx`
commands; a node is a Linux server that MMX manages. A node task is one
action that a node performs, and a module is the code that does the work
of one node task, such as `file` or `apt_package`. A fleet stage is one
step of a playbook: which nodes to use, and the node tasks each one runs.
Each entry names the cause as it was measured, with the
date and the record, and says whether the cause lies with the distribution,
a tool's packages, or MMX itself. The page is not advice on which
distribution to choose. The module page linked in each entry holds the
details, and the [module index](modules/README.md) shows in its column
"Tested on" where each module's suite passed.

MMX manages Linux servers on AMD64 or ARM64 that run systemd. On another
distribution that runs systemd, MMX is expected to work but is not tested.
On a distribution of a family that MMX does not know, the node facts
`.node.distribution.family` and `.node.package_manager` are unavailable,
with the reason. A fact that comes from a field the distribution's
os-release file does not set, such as `.node.distribution.codename`, is
unavailable there too. MMX does not manage a server that
systemd does not run, such as Alpine Linux with OpenRC: the node fails at
its first connection with a message that says so
([When something fails](../when-something-fails.md)). The controller
machine runs Linux or macOS. A controller process and a node runner from
different MMX releases do not work together:
[Upgrade MMX](../how-to/upgrade-mmx.md) says how the runner is replaced.

## Debian 12

### Quadlet files need a newer Podman

- **Limited:** [`podman_quadlet`](modules/podman_quadlet.md) fails on
  Debian 12 and names the installed Podman version. `podman_image` works.
- **Cause:** Quadlet needs Podman 4.4 or newer, and Debian 12 packages
  Podman 4.3.1. Measured on 2026-10-27
  ([validation record](../changing-mmx/validation.md#platform-validation-on-the-four-target-distributions)).
- **What would change it:** a Podman 4.4 or newer on the node.
- **What you can do now:** run the container from a unit file written with
  `systemd_unit`, as the `podman_quadlet` page shows.

### Restoring a backup bucket as it stood earlier needs a newer pgBackRest

- **Limited:** `read_repository_as_of` of
  [`pgbackrest_restore`](../../official_extra_modules/pgbackrest_restore/README.md)
  fails before it changes anything.
- **Cause:** the setting needs pgBackRest 2.54 or newer, and Debian 12
  packages pgBackRest 2.45. Measured on 2026-10-27
  ([validation record](../changing-mmx/validation.md#platform-validation-on-the-four-target-distributions)).
- **What would change it:** a pgBackRest 2.54 or newer on the node.
- **What you can do now:** restore without `read_repository_as_of`, or
  install a newer `pgbackrest` package.

### ZFS is not tested

- **Limited:** [`zfs_dataset`](../../official_extra_modules/zfs_dataset/README.md)
  is not tested on Debian 12. The suite checks only the failure that names
  the missing program.
- **Cause:** on MMX's side. Debian's ZFS package builds the kernel module on
  the node, which takes six to seven minutes on Debian 12, and the suite
  does not install ZFS there for that reason. By hand, ZFS 2.1.11 on
  Debian 12 printed the same `zfs` output that the module reads. Recorded on
  the `zfs_dataset` page, as of 2026-11-14.
- **What would change it:** a suite run that installs ZFS on Debian 12.
- **What you can do now:** install ZFS as the `zfs_dataset` page shows for
  Debian, and try the module on a test node first.

### MySQL is tested only as MariaDB

- **Limited:** [`mysql_user`](modules/mysql_user.md) and
  [`mysql_database`](modules/mysql_database.md) are tested on Debian 12
  against MariaDB, not MySQL.
- **Cause:** Debian packages only MariaDB, and MMX does not test MySQL from
  the vendor's own repository. Measured on 2026-10-27
  ([validation record](../changing-mmx/validation.md#platform-validation-on-the-four-target-distributions)).
- **What would change it:** a suite run against MySQL from the vendor's
  repository.
- **What you can do now:** use MariaDB, or try MySQL from the vendor's
  repository on a test node first.

### MongoDB has no ARM64 packages

- **Limited:** [`mongodb_user`](../../official_extra_modules/mongodb_user/README.md)
  is tested on ARM64 Debian servers without a MongoDB server.
- **Cause:** MongoDB publishes its Debian packages only for amd64. Measured
  on 2026-10-27
  ([validation record](../changing-mmx/validation.md#platform-validation-on-the-four-target-distributions)).
- **What would change it:** ARM64 Debian packages from MongoDB.
- **What you can do now:** run MongoDB on an amd64 node.

## Debian 13

### ZFS is built on the node

- **Limited:** installing ZFS for
  [`zfs_dataset`](../../official_extra_modules/zfs_dataset/README.md) takes
  about five minutes, and the build can fail after a kernel update.
- **Cause:** Debian's package `zfs-dkms` builds the ZFS kernel module on
  the node. The build breaks when the distribution's kernel is newer than
  the ZFS version supports. Measured with ZFS 2.3.9 on 2026-10-27
  ([validation record](../changing-mmx/validation.md#platform-validation-on-the-four-target-distributions)).
- **What would change it:** a ZFS kernel module that comes with Debian's
  kernel.
- **What you can do now:** expect the build time at the first installation,
  and check ZFS after each kernel update.

### MySQL is tested only as MariaDB

- **Limited:** [`mysql_user`](modules/mysql_user.md) and
  [`mysql_database`](modules/mysql_database.md) are tested on Debian 13
  against MariaDB, not MySQL.
- **Cause:** Debian packages only MariaDB, and MMX does not test MySQL from
  the vendor's own repository. Measured on 2026-10-27
  ([validation record](../changing-mmx/validation.md#platform-validation-on-the-four-target-distributions)).
- **What would change it:** a suite run against MySQL from the vendor's
  repository.
- **What you can do now:** use MariaDB, or try MySQL from the vendor's
  repository on a test node first.

### MongoDB has no ARM64 packages

- **Limited:** [`mongodb_user`](../../official_extra_modules/mongodb_user/README.md)
  is tested on ARM64 Debian servers without a MongoDB server.
- **Cause:** MongoDB publishes its Debian packages only for amd64. Measured
  on 2026-10-27
  ([validation record](../changing-mmx/validation.md#platform-validation-on-the-four-target-distributions)).
- **What would change it:** ARM64 Debian packages from MongoDB.
- **What you can do now:** run MongoDB on an amd64 node.

## Ubuntu 24.04

### Restoring a backup bucket as it stood earlier needs a newer pgBackRest

- **Limited:** `read_repository_as_of` of
  [`pgbackrest_restore`](../../official_extra_modules/pgbackrest_restore/README.md)
  fails before it changes anything.
- **Cause:** the setting needs pgBackRest 2.54 or newer, and Ubuntu 24.04
  packages pgBackRest 2.50. Measured on 2026-10-27
  ([validation record](../changing-mmx/validation.md#platform-validation-on-the-four-target-distributions)).
- **What would change it:** a pgBackRest 2.54 or newer on the node.
- **What you can do now:** restore without `read_repository_as_of`, or
  install a newer `pgbackrest` package.

## Ubuntu 26.04

### The MongoDB server does not start on Ubuntu 26.04's kernel

- **Limited:** [`mongodb_user`](../../official_extra_modules/mongodb_user/README.md)
  is tested without a MongoDB server on Ubuntu 26.04 machines that run
  Ubuntu's own kernel.
- **Cause:** the MongoDB server refuses to start on Linux 6.19.0 through
  7.0.13 ([SERVER-121912](https://jira.mongodb.org/browse/SERVER-121912)),
  and Ubuntu 26.04 runs kernel 7.0.0. Measured on 2026-10-27
  ([validation record](../changing-mmx/validation.md#platform-validation-on-the-four-target-distributions)).
- **What would change it:** a MongoDB release that starts on these kernels,
  or a kernel newer than 7.0.13.
- **What you can do now:** run MongoDB on a node with another kernel.

## Rocky Linux 9

### Some tool modules are not tested

- **Limited:** these modules are not tested on Rocky Linux 9:
  `wireguard_interface`, `delete_old_files`, `systemd_journal_vacuum`,
  and the pg_auto_failover and pgBackRest modules. The [module index](modules/README.md) shows each one.
- **Cause:** on MMX's side. MMX's test harness does not run these modules'
  suites on the Red Hat family
  ([testing on several distributions](../changing-mmx/design/development.md#testing-on-several-distributions)),
  as of 2026-11-14.
- **What would change it:** the suites run and pass on the Red Hat family.
- **What you can do now:** try the module on a test node first.

### Snaps are not tested

- **Limited:** [`snap_package`](modules/snap_package.md) is tested on Rocky
  Linux 9 only for the failure that names the missing `snapd` package.
- **Cause:** the Red Hat family packages `snapd` only in EPEL, an extra
  repository, and the suite does not install it from there. Recorded on the
  `snap_package` page, as of 2026-11-14.
- **What would change it:** a suite run with `snapd` from EPEL.
- **What you can do now:** install `snapd` from EPEL, as the `snap_package`
  page shows, and try the module on a test node first.

### ZFS has no ARM64 packages

- **Limited:** [`zfs_dataset`](../../official_extra_modules/zfs_dataset/README.md)
  is tested on ARM64 servers without ZFS.
- **Cause:** the OpenZFS project builds its packages for the Red Hat family
  only for x86_64. Measured on 2026-10-27
  ([validation record](../changing-mmx/validation.md#platform-validation-on-the-four-target-distributions)).
- **What would change it:** ARM64 packages from OpenZFS.
- **What you can do now:** use an x86_64 node, or build ZFS from OpenZFS's
  source.

## AlmaLinux 10

### Some tool modules are not tested

- **Limited:** these modules are not tested on AlmaLinux 10:
  `wireguard_interface`, `delete_old_files`, `systemd_journal_vacuum`,
  and the pg_auto_failover and pgBackRest modules. The [module index](modules/README.md) shows each one.
- **Cause:** on MMX's side. MMX's test harness does not run these modules'
  suites on the Red Hat family
  ([testing on several distributions](../changing-mmx/design/development.md#testing-on-several-distributions)),
  as of 2026-11-14.
- **What would change it:** the suites run and pass on the Red Hat family.
- **What you can do now:** try the module on a test node first.

### certbot from EPEL does not install

- **Limited:** [`certbot_certificate`](modules/certbot_certificate.md) is
  not tested on AlmaLinux 10.
- **Cause:** EPEL 10's `certbot` package cannot be installed on AlmaLinux
  10: its `python3-pyOpenSSL` needs a newer `cryptography` than AlmaLinux
  10's repositories have. Measured on 2026-11-14 and recorded on the
  `certbot_certificate` page.
- **What would change it:** an EPEL 10 `certbot` that installs on AlmaLinux
  10.
- **What you can do now:** install certbot from pip, which MMX does not
  test, and try the module on a test node first.

### Snaps are not tested

- **Limited:** [`snap_package`](modules/snap_package.md) is tested on
  AlmaLinux 10 only for the failure that names the missing `snapd` package.
- **Cause:** the Red Hat family packages `snapd` only in EPEL, an extra
  repository, and the suite does not install it from there. Recorded on the
  `snap_package` page, as of 2026-11-14.
- **What would change it:** a suite run with `snapd` from EPEL.
- **What you can do now:** install `snapd` from EPEL, as the `snap_package`
  page shows, and try the module on a test node first.

### ZFS has no ARM64 packages

- **Limited:** [`zfs_dataset`](../../official_extra_modules/zfs_dataset/README.md)
  is tested on ARM64 servers without ZFS.
- **Cause:** the OpenZFS project builds its packages for the Red Hat family
  only for x86_64. Measured on 2026-10-27
  ([validation record](../changing-mmx/validation.md#platform-validation-on-the-four-target-distributions)).
- **What would change it:** ARM64 packages from OpenZFS.
- **What you can do now:** use an x86_64 node, or build ZFS from OpenZFS's
  source.

## Fedora 45

### Most tool modules are not tested

- **Limited:** on Fedora 45, MMX tests the base suite (bootstrap, both
  kinds of connection, recovery, and the modules such as `file`, `user`, and
  `systemd_service`), `dnf_package`, `dnf_repository`, `firewalld`, and the
  four `selinux_` modules. Other tool modules are not tested there, among
  them `firewalld_zone`, `firewalld_zone_membership`, and
  `networkmanager_connection_file`.
- **Cause:** on MMX's side. MMX's test harness runs only those suites on
  Fedora 45. Measured on 2026-11-14
  ([validation record](../changing-mmx/validation.md#fedora-45-and-dnf-5)).
- **What would change it:** more suites run and pass on Fedora 45.
- **What you can do now:** try the module on a test node first.

### NetworkManager modules are not tested

- **Limited:** [`networkmanager_connection_file`](modules/networkmanager_connection_file.md)
  and [`firewalld_zone_membership`](modules/firewalld_zone_membership.md)
  are not tested on Fedora 45.
- **Cause:** Fedora's cloud image, which the test machines use, has no
  NetworkManager, and both suites need NetworkManager to manage the test
  machine's network cards. Measured on 2026-11-14
  ([validation record](../changing-mmx/validation.md#fedora-45-and-dnf-5)).
- **What would change it:** a Fedora 45 test machine where NetworkManager
  manages the network.
- **What you can do now:** on a Fedora node where NetworkManager manages the
  network, try the modules on a test node first.

### Version rules allow only `N.*` wildcards

- **Limited:** a `version` rule of [`dnf_package`](modules/dnf_package.md#dnf-5)
  can be an exact version, numbers that end in `.*` such as `16.*`, or `*`.
  Any other wildcard, such as `1.2*`, fails the task before it changes
  anything. A rule such as `27.3.*` also admits `27.3`, `27.3^<snapshot>`,
  and `27.3a` to dnf.
- **Cause:** dnf 5's version lock has no wildcard comparator: dnf 5.4.7
  reads only exact and relational comparisons, so MMX writes `N.*` as a
  range of versions. Measured on 2026-11-14
  ([validation record](../changing-mmx/validation.md#fedora-45-and-dnf-5)).
- **What would change it:** a dnf 5 version lock with wildcard comparisons.
- **What you can do now:** write an exact version, or a rule that ends in
  `.*`.

## Arch Linux

### Most tool modules are not tested

- **Limited:** on Arch Linux, MMX tests the base suite (bootstrap, both
  kinds of connection, recovery, and the modules such as `file`, `user`, and
  `systemd_service`) and `pacman_package`. Other tool modules, such as
  those for Docker, PostgreSQL, and scheduled jobs, are not tested there.
- **Cause:** on MMX's side. MMX's test harness runs a tool module's suite
  only on the distributions it lists for that suite
  ([testing on several distributions](../changing-mmx/design/development.md#testing-on-several-distributions)),
  as of 2026-11-14.
- **What would change it:** more suites run and pass on Arch Linux.
- **What you can do now:** try the module on a test node first.

### Every installation upgrades the whole system

- **Limited:** a [`pacman_package`](../../official_extra_modules/pacman_package/README.md)
  task that installs a missing package also upgrades every other package.
- **Cause:** Arch Linux supports no partial upgrade: a package from newer
  package databases than the rest of the system can need newer libraries
  than the system has. This is Arch Linux's rule, recorded on the
  `pacman_package` page; the suite passed with it on 2026-11-14
  ([validation record](../changing-mmx/validation.md#pacman-package-module)).
- **What would change it:** nothing on MMX's side; Arch Linux's own way to
  install is `pacman -Syu <package>`.
- **What you can do now:** install new packages when a whole-system upgrade
  suits you.

### Packages have no version setting

- **Limited:** [`pacman_package`](../../official_extra_modules/pacman_package/README.md)
  cannot keep a package at a version or bring one package alone to the
  newest version.
- **Cause:** Arch Linux's repositories offer one version of each package,
  the newest, and pacman has no rule that keeps a package inside a version
  pattern. Recorded on the `pacman_package` page, as of 2026-11-14.
- **What would change it:** nothing on MMX's side.
- **What you can do now:** hold a package back with `IgnorePkg` in
  `/etc/pacman.conf`, written with a `line` task, which pacman treats as a
  partial upgrade.

## openSUSE Leap 16.1

### Most tool modules are not tested

- **Limited:** on openSUSE Leap 16.1, MMX tests the base suite (bootstrap,
  both kinds of connection, recovery, and the modules such as `file`,
  `user`, and `systemd_service`), `zypper_package`, and
  `zypper_repository`. Other tool modules, such as those for Docker,
  PostgreSQL, and scheduled jobs, are not tested there.
- **Cause:** on MMX's side. MMX's test harness runs a tool module's suite
  only on the distributions it lists for that suite
  ([testing on several distributions](../changing-mmx/design/development.md#testing-on-several-distributions)),
  as of 2026-11-14.
- **What would change it:** more suites run and pass on openSUSE Leap.
- **What you can do now:** try the module on a test node first.

### A version is not held, and never downgraded

- **Limited:** a version in a [`zypper_package`](../../official_extra_modules/zypper_package/README.md)
  entry is installed, but a later `upgrade_all_packages` task updates the
  package past it. A version older than the installed one fails the task.
  `apt_package` and `dnf_package` hold a version and can downgrade.
- **Cause:** on MMX's side for the hold: zypper holds a version with a lock,
  and `zypper_package` writes no locks. For the downgrade, zypper needs
  `--oldpackage`, a forcing option that MMX does not pass. Measured on
  2026-11-14 ([validation record](../changing-mmx/validation.md#zypper-package-and-repository-modules)).
- **What would change it:** a `zypper_package` that writes and releases
  zypper locks.
- **What you can do now:** hold a version with `zypper addlock` in a
  `shell` task; remove the package first to install an older version.

### `newest_on_every_run` stays with the installed package's vendor

- **Limited:** with `newest_on_every_run` or `upgrade_all_packages`, a
  package whose newer version comes only from another vendor is not updated.
  The task is unchanged and its reason says why.
- **Cause:** `zypper install` and `zypper update` never change a package's
  vendor. Measured on 2026-11-14
  ([validation record](../changing-mmx/validation.md#zypper-package-and-repository-modules)).
- **What would change it:** nothing on MMX's side; changing vendors is
  `zypper dist-upgrade`, which the module does not offer.
- **What you can do now:** run `zypper dist-upgrade` in a `shell` task when
  you want the other vendor's version.

### Conflicting versions in one fleet stage are not caught early

- **Limited:** two node tasks of one fleet stage that ask for different
  versions of one package with
  [`zypper_package`](../../official_extra_modules/zypper_package/README.md)
  are not refused on the controller machine before the run, as they are
  with `apt_package` and `dnf_package`.
- **Cause:** on MMX's side: `zypper_package` is an official extra module,
  a script in Starlark, and the check uses a hook that only core modules
  have
  ([package modules in Starlark](../changing-mmx/design/modules-packages.md#package-modules-in-starlark)),
  as of 2026-11-14.
- **What would change it:** the hook for official extra modules.
- **What you can do now:** keep one version entry for each package in a
  fleet stage.
