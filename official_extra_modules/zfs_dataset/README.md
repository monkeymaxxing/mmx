# `zfs_dataset`

`zfs_dataset` manages one ZFS filesystem dataset on the node and its
properties, such as its compression and its quota. It runs the program
`zfs`. A dataset lives in a ZFS pool, which must exist: a pool takes whole
disks and is created once for each server, so it stays a `shell` task with
a guard.

```yaml
# The pool, once: zpool list succeeds when the pool exists.
- shell:
    script: zpool create tank mirror /dev/sdb /dev/sdc
    skip_if_this_command_succeeds: zpool list tank

- zfs_dataset:
    name: tank/clickhouse
    properties:
      compression: lz4
      recordsize: 128K
```

## Set up

ZFS is two things: the programs `zfs` and `zpool`, and a kernel module.
Installing ZFS's programs and kernel module on a node differs by
distribution.

**Ubuntu** ships the kernel module with its kernel:

```yaml
- when: {equals: ["{{ .node.distribution.id }}", ubuntu]}
  apt_package: {packages: [{name: zfsutils-linux, version: newest_when_first_installed}]}
```

**Debian** has the packages in the section `contrib` of its own
repositories, which the node's package sources must list. A package builds
the kernel module on the node, for the kernel that runs, which takes about
five minutes:

```yaml
- when: {equals: ["{{ .node.distribution.id }}", debian]}
  apt_package:
    packages:
      - {name: "linux-headers-{{ .node.kernel.release }}", version: newest_when_first_installed}
      - {name: zfs-dkms, version: newest_when_first_installed}
      - {name: zfsutils-linux, version: newest_when_first_installed}
```

**Rocky Linux and AlmaLinux** take ZFS from the OpenZFS project's
repository, which a small package sets up. The repository `zfs-kmod` holds
the kernel module already built; the repository `zfs`, which is switched on
as installed, would build it on the node. OpenZFS builds both only for
x86_64, so an ARM64 server of the Red Hat family needs ZFS built from
OpenZFS's source, which this page does not cover. The first task installs
two plugins of dnf that the tasks after it need:

```yaml
- when: {matches: ["{{ .node.distribution.id }}", "^(rocky|almalinux)$"]}
  dnf_package: {packages: [{name: dnf-plugins-core, version: newest_when_first_installed}, {name: python3-dnf-plugin-versionlock, version: newest_when_first_installed}]}

- when:
    all:
      - matches: ["{{ .node.distribution.id }}", "^(rocky|almalinux)$"]
      - number_equals: ["{{ .node.distribution.major_version }}", 9]
  dnf_package:
    packages:
      - {name: zfs-release, version: "3-0.el9", package_file_url: https://zfsonlinux.org/epel/zfs-release-3-0.el9.noarch.rpm, package_file_sha256: 4954819e3d50522db0860647e5f093c143e46b3d8eba9a082fe4e5d8bbc0791e}

- when:
    all:
      - matches: ["{{ .node.distribution.id }}", "^(rocky|almalinux)$"]
      - number_equals: ["{{ .node.distribution.major_version }}", 10]
  dnf_package:
    packages:
      - {name: zfs-release, version: "3-0.el10", package_file_url: https://zfsonlinux.org/epel/zfs-release-3-0.el10.noarch.rpm, package_file_sha256: c2527e7d20aaa64cd77f1f1ca86b34123de6b710ab38e8f6a00206918efbc992}

- when: {matches: ["{{ .node.distribution.id }}", "^(rocky|almalinux)$"]}
  dnf_repository: {existing_repository: zfs, enabled: false}

- when: {matches: ["{{ .node.distribution.id }}", "^(rocky|almalinux)$"]}
  dnf_repository: {existing_repository: zfs-kmod, enabled: true}

- when: {matches: ["{{ .node.distribution.id }}", "^(rocky|almalinux)$"]}
  dnf_package: {packages: [{name: zfs, version: newest_when_first_installed}]}
```

No service has to be started: a `zfs` command loads the kernel module by
itself.

**Secure Boot.** A node with Secure Boot on loads only a kernel module
that carries a signature it trusts. Ubuntu signs its ZFS module. On the
other distributions the kernel refuses the module (`Key was rejected by
service` on Debian 13 and Rocky Linux 9) until the node's owner enrolls a
key that signs the module, or switches Secure Boot off.

**Debian 12.** ZFS installs as on Debian 13.

**For `sharenfs`,** the node also needs an NFS server: the package
`nfs-kernel-server` on Debian and Ubuntu, and `nfs-utils` on the Red Hat
family. Without one, ZFS stores the property and cannot share the
filesystem.

## Inputs

| Input | Meaning | When omitted |
|---|---|---|
| `name` | The dataset as ZFS names it: the pool, then the datasets below it, with `/` between them, such as `tank/clickhouse`. | Required. |
| `state` | `present` or `deleted_with_all_its_data`. `deleted_with_all_its_data` accepts only `name`. | `present` |
| `properties` | ZFS properties with their values as text, each under the name that `zfs get` prints, such as `compression`. Only the listed properties are managed; a property that the task does not list keeps its value. Quote a value that YAML would read as something else, such as `"off"`. | The module sets no property. |

With `state: present`:

- **A missing dataset** is created with its properties (`zfs create`). Its
  parent must exist: for `tank/clickhouse` the pool `tank`, for
  `tank/clickhouse/logs` the dataset `tank/clickhouse`.
- **An existing dataset** gets each listed property whose value in effect
  differs (`zfs set`). A size is compared as a number of bytes, so `128K`
  equals `131072` and `1G` equals `1024M`.
- **A property of your own** has a colon in its name, such as
  `com.example:owner`. Its value is compared as text.

**Write a property's long name.** ZFS takes some properties also by a
second, shorter name, such as `compress` for `compression` and `recsize`
for `recordsize`. The module refuses the second name and names the long
one, before anything changes.

**ZFS stores some values in another form** than the one written, and the
form differs between ZFS versions: `posixacl` comes back as `posix`, and
`gzip-6` as `gzip`. The module reads a property again after it set it.
When the stored value differs from the task's, the task fails and names the
form that ZFS stores, so that you write that form once. Otherwise the task
would set the property on every run.

With `state: deleted_with_all_its_data`, the module deletes the dataset
(`zfs destroy`) and everything in it. A dataset that has child datasets or
snapshots fails the task, which names them: the module never deletes
them along. A dataset that does not exist is unchanged.

The module manages filesystem datasets. A volume or a snapshot named by
the task's `name` input fails the task.

## Outputs

| Output | Meaning |
|---|---|
| `created` | `true` when the task created the dataset, and in a dryrun when a run would create it. Otherwise `false`. |

A later node task reads the `created` output as `.node_tasks.<id>.created`.

## What a dryrun reports

- **`would_change`** with what a run would do: `would create the dataset
  tank/clickhouse with compression=lz4, recordsize=128K`, each property
  that would change with its old and its new value, such as `would set
  compression of the dataset tank/clickhouse from off to lz4`, or `would
  delete the dataset tank/clickhouse with all its data`.
- **`unchanged`** when the dataset has the listed values.
- **A failure**, with the same reason as in a run, for a property name
  that ZFS does not know, for a property's second name, and for a volume
  or a snapshot of the task's name. No node task changes such a state.
- **`not_checked`** for a state that an earlier node task of the same
  playbook may change, because in a dryrun that task has changed nothing
  yet: while the program `zfs` is missing, while the pool or the parent
  dataset does not exist, while ZFS's kernel module is not loaded and
  cannot be loaded, and for `deleted_with_all_its_data` while the dataset
  has child datasets or snapshots. A run fails once with the same reason;
  while the kernel module is not loaded, MMX tries a run again.
- **What a dryrun cannot know:** whether ZFS takes a value, whether ZFS
  stores a value in another form, and whether a dataset is in use when a
  run wants to delete it.
- **What its reads leave on the node:** a `zfs` command loads ZFS's kernel
  module when the module is installed and not loaded yet, also in a
  dryrun. The module stays loaded.

## Examples

Two datasets: one for ClickHouse, with compression and a record size that
suits its files, and one with a quota, without access times, and with a
property of your own:

```yaml
- zfs_dataset:
    name: tank/clickhouse
    properties:
      compression: lz4
      recordsize: 128K

- zfs_dataset:
    name: tank/backups
    properties:
      quota: 500G
      atime: "off"
      "com.example:owner": storage-team
```

A dataset below another one. The task for the parent dataset
`tank/clickhouse` comes before the task for `tank/clickhouse/logs`:

```yaml
- zfs_dataset:
    name: tank/clickhouse

- zfs_dataset:
    name: tank/clickhouse/logs
    properties:
      compression: zstd
```

A dataset is deleted with everything in it, after the dataset below it:

```yaml
- zfs_dataset:
    name: tank/old-clickhouse/logs
    state: deleted_with_all_its_data

- zfs_dataset:
    name: tank/old-clickhouse
    state: deleted_with_all_its_data
```

## Errors and what they mean

Every reason starts with `zfs_dataset`. How often MMX tries a task that
fails depends on what failed:

- **Once, in a dryrun too,** when no node task changes what is in the way:
  an input that breaks a rule, an unknown property name, or a name that is
  a volume.
- **Once in a run, and not checked in a dryrun,** when an earlier node
  task may change what is in the way: it creates the pool or the parent
  dataset, or deletes the child datasets.
- **Once,** when the task changed the node and then failed, such as for a
  dataset that ZFS created and could not share: another try would find the
  dataset and report success.
- **Again,** when another command of ZFS failed.

| The reason holds | What it means, and what to do |
|---|---|
| `the program zfs is not installed; install the package ...` | ZFS's programs are not on the node. The reason names the package for each family of distributions. Install ZFS in an earlier node task; see [Set up](#set-up). |
| `zfs_dataset.state: deleted_with_all_its_data accepts only name; remove properties` | A task with `state: deleted_with_all_its_data` holds `properties`. Remove that input. |
| `the ZFS pool tank does not exist` | The node has no pool of that name. Create the pool in an earlier node task, with a guarded `shell` task as above. |
| `the parent dataset tank/clickhouse does not exist` | The dataset above the task's dataset is missing. Give the parent its own `zfs_dataset` task before this one. |
| `ZFS's kernel module is not loaded on this node, and the zfs program could not load it` | The programs are installed, and the kernel module is missing or the kernel refuses it. Check `modprobe zfs` on the node. With Secure Boot on, see [Set up](#set-up). After a kernel update, the module must fit the new kernel. |
| `properties: ZFS on this node knows no property ...` | A name in `properties` is no ZFS property, for example a typing mistake or a property of a newer ZFS. Nothing was changed. Correct the name; `zfs get all <dataset>` lists the properties. A property of your own needs a colon in its name. |
| `properties: compress is a second name of the ZFS property compression; write compression in the task` | The task names a property by its second, shorter name. Nothing was changed. Write the long name, which `zfs get` prints. |
| `ZFS took the value posixacl for the property acltype ... and stores it as posix` | ZFS keeps the value in another form than the task writes. The property is set, and after a creation the dataset exists and the task reports `created: true`. Write the stored form in the task, as the reason says. |
| `setting ... on the dataset ...:` and ZFS's words | ZFS refused a value, or the property can be set only when a dataset is created (`'casesensitivity' is readonly`). No property of the task was set. Read ZFS's words. A property of that kind cannot change on an existing dataset. MMX reports `changed` for the command that ran although ZFS refused it. |
| `ZFS created the dataset ... and then failed:` and ZFS's words | ZFS created the dataset and could then not mount or share it, for example with `sharenfs` and no NFS server on the node with ZFS 2.2 and 2.3. The dataset exists, and the task reports `created: true`. Remove the cause. The next run finds the dataset and compares its properties. |
| `... is a volume; the module manages filesystem datasets` | The name is a ZFS volume or another kind of dataset. The module leaves it alone; manage it with a `shell` task. |
| `the dataset ... has child datasets or snapshots, so it stays` | `deleted_with_all_its_data` for a dataset that has datasets or snapshots below it. The reason names the first five and counts the rest. Nothing was deleted. Delete each child dataset in an earlier node task, with its own `zfs_dataset` task, and each snapshot with `zfs destroy`. |
| `deleting the dataset ...: cannot unmount '...': pool or dataset is busy` | A program uses the dataset: it has a file open there, or works in one of its directories. Nothing was deleted. Stop the program, then run again. |
| A reason that ends with `The dataset ... was created` | A check right after the creation failed: ZFS stores a value in another form, or a command of ZFS that reads failed. The task reports `created: true`: another try would find the dataset and report that the task created nothing. |

A reason can end with `; ZFS warned:` and ZFS's words. ZFS then stored the
value and could not apply it at once, for example `cannot share ...` for
`sharenfs` on a node without an NFS server. ZFS 2.4 also creates a dataset
with such a `sharenfs` and only warns, where ZFS 2.2 and 2.3 report an
error after the creation.

## Tested on

**Distributions:** Ubuntu 24.04 and 26.04, Debian 13, Rocky Linux 9, AlmaLinux 10.

The suite `tests/integration/zfs_datasets.py` runs the whole test where the
tasks under [Set up](#set-up) install ZFS in under two minutes, and on
Debian 13 with its build of the kernel module. It runs in a virtual
machine, with Secure Boot off, on a pool that is a file:

| Distribution | ZFS | Kernel module | The tasks under "Set up" take |
|---|---|---|---|
| Ubuntu 24.04 | 2.2.2 | comes with the kernel | about 25 seconds |
| Ubuntu 26.04 | 2.4.1 | comes with the kernel | about 30 seconds |
| Rocky Linux 9 | 2.2.11 | prebuilt, from the OpenZFS project's repository `zfs-kmod` | about 90 seconds |
| AlmaLinux 10 | 2.2.11 | prebuilt, from the same repository | about 60 seconds |
| Debian 13 | 2.3.9 | built on the node by the package `zfs-dkms` | about 5 minutes |

`sharenfs` with an NFS server on the node was tried by hand on Ubuntu
24.04 (`nfs-kernel-server`) and Rocky Linux 9 (`nfs-utils`): ZFS then
shares a new dataset without a message.

On Debian 12 the suite tests the module without ZFS: a dryrun reports the
task `not_checked`, and a run fails and names the program and its package.
It does the same on Rocky Linux 9 and AlmaLinux 10 ARM64 servers, for which
OpenZFS publishes no packages.
The suite does not install ZFS there, because the build of the kernel
module takes six to seven minutes. ZFS 2.1.11 was built and installed by
hand once on Debian 12, and the output of the `zfs` commands that the
module is built on was the same there.

A build on the node breaks whenever the distribution's kernel is newer
than the ZFS version supports, which a test suite cannot foresee.

[Limits on each distribution](../../docs/reference/limits-on-each-distribution.md) says what is limited for this module on [Debian 12](../../docs/reference/limits-on-each-distribution.md#debian-12), [Debian 13](../../docs/reference/limits-on-each-distribution.md#debian-13), [Rocky Linux 9](../../docs/reference/limits-on-each-distribution.md#rocky-linux-9), and [AlmaLinux 10](../../docs/reference/limits-on-each-distribution.md#almalinux-10), and why.

## Why it works this way

- **Pools stay a guarded `shell` task.** Creating a pool takes whole disks
  and happens once for each server. A dataset is what changes over a
  server's life.
- **Only the listed properties are managed.** ZFS has dozens of properties
  with good defaults, and a dataset inherits values from its parent. A
  task that lists two properties says nothing about the others.
- **A value is compared as ZFS stores it.** The module asks `zfs get` twice,
  for exact numbers and for the form a person writes, and takes a value as
  in effect when it equals either one or is the same number of bytes. A
  list of ZFS's own other names for a value differs between ZFS versions,
  so the module keeps none: it fails once and names the stored form. The
  module can learn that form only after the change, so this failure keeps
  its change and its output `created`.
- **A property has one name in a task.** ZFS 2.1.5 and 2.2.2 took
  `compress` and `recsize` in `zfs create`, `zfs set`, and `zfs get`, and
  `zfs get` printed `compression` and `recordsize`. With both names
  allowed, two tasks could manage one property under two names. So the
  module takes the name that `zfs get` prints and names it for the other.
- **The module looks again after a failed `zfs create`.** ZFS versions 2.2
  and 2.3 exit with an error after they created a dataset that they could
  not share or mount. Without the second look, the task would report that
  nothing exists. The task then fails without another try: another try
  would find the dataset with its properties and report success, and
  nobody would read ZFS's message.
- **An unknown property name fails before anything changes.** ZFS answers
  a question about several properties with no line at all when it does not
  know one of them. For a new dataset the module asks the parent dataset
  about the names first, so a dryrun finds the mistake too.
- **`deleted_with_all_its_data` never deletes child datasets or snapshots
  along.** They are data that the task does not name. The module lists
  them before it asks ZFS. A playbook deletes the child datasets and then
  their parent; in its dryrun the children are still there, so the dryrun
  reports the parent's task as `not_checked`, and only a run fails.
- **A missing pool is no error of the dataset's task in a dryrun.** A
  playbook that creates the pool and then its datasets must pass its first
  dryrun, so the task is `not_checked` there.
- **A value holds no tab and no line break.** The module reads values from
  `zfs get`, which separates its columns with tabs and prints one line for
  each property.
