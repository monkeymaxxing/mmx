# `lvm_logical_volume`

`lvm_logical_volume` manages one LVM logical volume on the node: a virtual
disk inside a volume group, which a later run can grow. The module runs
the programs of the package `lvm2`, such as `lvcreate` and `lvextend`.
The module creates a volume, grows the volume together with the volume's
filesystem, and never shrinks the volume.

```yaml
- lvm_logical_volume:
    name: postgresql
    volume_group: data
    size: 50GiB

    grow_filesystem_with_volume: true
- create_filesystem_on_empty_device:
    device: /dev/data/postgresql
    filesystem_type: ext4
```

[`lvm_volume_group`](../lvm_volume_group/README.md) makes the volume group
first.

## Set up

Install the package `lvm2`, which has the same name on every distribution.
No service has to be started.

```yaml
- when: {equals: ["{{ .node.package_manager }}", apt]}
  apt_package: {packages: [{name: lvm2, version: newest_when_first_installed}]}
- when: {equals: ["{{ .node.package_manager }}", dnf]}
  dnf_package: {packages: [{name: lvm2, version: newest_when_first_installed}]}
```

To grow a filesystem with its volume, LVM runs the filesystem's own
programs, which the filesystem's package brings: `e2fsprogs` for ext2,
ext3, and ext4, and `xfsprogs` for xfs. The module also runs `wipefs` from
the package `util-linux`, which every node has.

## Inputs

| Input | Meaning | When omitted |
|---|---|---|
| `name` | The logical volume's name: 1 to 127 letters, digits, `+`, `_`, `.`, and `-`. | Required. |
| `volume_group` | The name of the volume group that holds the logical volume. | Required. |
| `state` | `present` or `deleted_with_all_its_data`. `deleted_with_all_its_data` accepts only `name` and `volume_group`. | `present` |
| `size` | A whole number and `MiB`, `GiB`, or `TiB`, such as `50GiB`, or `all_free_space_when_created`. | Required for `present`. |
| `grow_filesystem_with_volume` | `true`: when the module grows the volume, LVM also grows the ext2, ext3, ext4, or xfs filesystem on it. A volume that holds nothing yet grows alone. A volume that holds anything else, such as swap, fails the task and is not grown. `false`: only the volume grows; for a volume that holds something else, such as swap. | Required for `state: present`. |
| `lvcreate_options` | More options for `lvcreate` when the volume is created, as a list, such as `["--type", "raid1", "--mirrors", "1"]`. Not used for a volume that exists. The module writes the options for the name, the size, and `--yes --wipesignatures y` itself. | No more options. |

With `state: present`:

- **A missing volume** is created with that size. LVM rounds a size up to
  whole extents of the volume group, which are 4 MiB unless the volume
  group was made with another size: `50MiB` gives a volume of 52 MiB.
  `all_free_space_when_created` gives the new volume all free space.
- **A smaller existing volume grows** to `size`, and the filesystem on it
  grows with it (`lvextend --resizefs`), mounted or not. The module first
  looks with `wipefs --no-act` whether the volume holds an ext2, ext3,
  ext4, or xfs filesystem. A volume that holds nothing yet grows alone,
  and the reason says so. A volume that holds anything else fails the
  task and is named with what it holds. With
  `grow_filesystem_with_volume: false` only the volume grows.
- **An existing volume of that size** is unchanged. The module compares
  after the rounding to whole extents. With `lvcreate_options` that stripe
  the volume, such as `--stripes 3` or a RAID type with several data
  stripes, LVM rounds the size up to whole stripes, and the module counts
  that volume as larger on the next run: write a `size` that is a whole
  number of stripes (extent size times data stripes). With `all_free_space_when_created`, an existing volume is
  unchanged whatever its size.
- **A larger existing volume fails the task.** The module never shrinks a
  volume, because that destroys the data behind the new end.

**A filesystem grows only together with its volume.** When the volume
already has its size, the module changes nothing, also when the filesystem
on it is smaller. That happens after a run with
`grow_filesystem_with_volume: false`, or after someone grew the volume by
hand. For a mounted ext4 or XFS filesystem, use
[`grow_filesystem_to_fill_device`](../grow_filesystem_to_fill_device/README.md).
For an ext2 or ext3 filesystem, grow the filesystem by hand with
`resize2fs /dev/data/postgresql`.

With `state: deleted_with_all_its_data`, the module deletes the volume
(`lvremove --yes`) and everything on it. It deletes only the volume that
the task names:

- **A volume in use,** such as one with a mounted filesystem, fails the
  task.
- **A volume that other volumes depend on** fails the task, which names
  them: a volume with snapshots, and a thin pool with the volumes in it.
  LVM would delete them together with the volume. Delete each of them
  first, with its own task.
- **A volume that does not exist** is unchanged.

## Outputs

| Output | Meaning |
|---|---|
| `created` | `true` when the task created the volume, and in a dryrun when a run would create it. Otherwise `false`. |
| `device` | The volume's path, `/dev/<volume_group>/<name>`, such as `/dev/data/postgresql`. Not given for `deleted_with_all_its_data`. |

A later node task reads `created` and `device` as
`.node_tasks.<id>.created` and `.node_tasks.<id>.device`.

## What a dryrun reports

- **`would_change`** with what a run would do: `would create the logical
  volume data/postgresql with 50 GiB`, `would grow the logical volume
  data/postgresql from 50 GiB to 80 GiB, together with the filesystem on
  it`, or `would delete the logical volume data/postgresql with all its
  data (50 GiB)`. For a volume that holds nothing yet, the reason ends
  with `and no filesystem, because the volume holds none yet`.
- **`unchanged`** when the volume has its size.
- **A failure**, with the same reason as in a run, for a volume that is
  larger than `size`, and for a volume that should grow together with its
  filesystem and holds something other than an ext2, ext3, ext4, or xfs
  filesystem. No node task changes such a state.
- **`not_checked`** for a state that an earlier node task of the same
  playbook may change, because in a dryrun that task has changed nothing
  yet: while a program of `lvm2` is missing, while the volume group does
  not exist or has too little free space, and for
  `deleted_with_all_its_data` while the volume is in use or other volumes
  depend on it. A run fails once with the same reason.
- **What a dryrun cannot know:** whether the filesystem's own program will
  succeed, such as `resize2fs` for a filesystem that is mounted read-only.
  The run then fails after the volume grew, and says so.
- **What its reads leave on the node:** `vgs` and `lvs` write only LVM's
  own files below `/run`, which a reboot removes: `/run/lvm/hints` on
  Debian 13, and the directory `/run/lock/lvm` on Rocky Linux 9.
  `lvextend --help` and `wipefs --no-act` leave nothing.

## Examples

A volume of 50 GiB for PostgreSQL's data. To grow the volume later, write a
larger size: the next run grows the volume and its filesystem, while the
filesystem stays mounted:

```yaml
- lvm_logical_volume:
    name: postgresql
    volume_group: data
    size: 50GiB
    grow_filesystem_with_volume: true
```

A volume for swap. The swap volume holds no filesystem, so the task says
that only the volume grows:

```yaml
- lvm_logical_volume:
    name: swap
    volume_group: data
    size: 8GiB
    grow_filesystem_with_volume: false
```

A volume is deleted with everything on it, after its filesystem was
unmounted:

```yaml
- fstab_mount:
    mount_point: /var/lib/postgresql
    state: unmounted_and_removed_from_fstab

- lvm_logical_volume:
    name: postgresql
    volume_group: data
    state: deleted_with_all_its_data
```

## Errors and what they mean

Every reason starts with `lvm_logical_volume`. How often MMX tries a task
that fails depends on what failed:

- **Once, in a dryrun too,** when no node task changes what is in the way:
  an input that breaks a rule, a volume that would have to shrink, or a
  volume that holds something other than a filesystem that LVM grows.
- **Once in a run, and not checked in a dryrun,** when an earlier node
  task may change what is in the way: it adds a device to the volume
  group, unmounts the filesystem, or deletes the snapshots.
- **Once,** when the task changed the node and then failed, such as for a
  volume that grew while its filesystem did not: another try would find
  the volume at its size and report success.
- **Again,** when another command of LVM failed.

| The reason holds | What it means, and what to do |
|---|---|
| `the program lvs is not installed; install the package lvm2 ...` | A program that the module runs is not on the node. Install `lvm2` in an earlier node task; see [Set up](#set-up). |
| `lvm_logical_volume.state: deleted_with_all_its_data accepts only name and volume_group; remove size` | A task with `state: deleted_with_all_its_data` holds another input. Remove the input that the reason names. |
| `the volume group data does not exist` | The node has no such volume group. Create it in an earlier node task with `lvm_volume_group`. |
| `the volume group data has 10 GiB free, and the new logical volume data/postgresql needs 50 GiB` | The volume does not fit. Nothing was created. Write a smaller size, or add a device to the volume group in an earlier node task. |
| `the volume group data has no free space` | `all_free_space_when_created` for a new volume, and nothing is left. Add a device to the volume group, or delete a volume. |
| `the logical volume data/postgresql has 80 GiB, and the task asks for 50 GiB. The module never shrinks ...` | The volume is larger than `size`. Nothing was changed. Write the size that the volume has. |
| `... needs 30 GiB more for 80 GiB, and the volume group data has 10 GiB free` | The volume cannot grow that far. Nothing was changed. Write a smaller size, or add a device to the volume group in an earlier node task. |
| `the logical volume data/swap holds no ext2, ext3, ext4, or xfs filesystem (found: swap area ...), and grow_filesystem_with_volume is true` | The volume should grow together with its filesystem and holds something else; `found:` names what it holds. Nothing was changed. For such a volume, such as swap, set `grow_filesystem_with_volume: false`. A volume that holds nothing does not fail: it grows alone. |
| `growing the logical volume ...:` and LVM's words, then `The logical volume still has 50 GiB` | LVM refused and grew nothing, for a reason that the module does not know. Read LVM's words. |
| `growing the logical volume ...:` and LVM's words, then `The logical volume now has 80 GiB, and its filesystem is still smaller` | The volume grew, and the filesystem's own program failed afterwards, such as `resize2fs` for a filesystem that is mounted read-only. Read the program's words in the reason and remove the cause, then grow a mounted ext4 or XFS filesystem with [`grow_filesystem_to_fill_device`](../grow_filesystem_to_fill_device/README.md), or ext2 or ext3 by hand with `resize2fs`. The next run changes nothing. |
| `the logical volume data/postgresql is in use` | `deleted_with_all_its_data` for a volume whose filesystem is mounted, or which a program holds open. Nothing was deleted. Unmount the filesystem in an earlier node task, with `fstab_mount` and `state: unmounted_and_removed_from_fstab`, or stop the program. |
| `other logical volumes depend on the logical volume data/postgresql, and LVM deletes them together with it: before-upgrade (a snapshot of it)` | `deleted_with_all_its_data` for a volume with snapshots, or for a thin pool with volumes in it. The reason names the first five and counts the rest. Nothing was deleted. Delete each of them in an earlier node task, with its own `lvm_logical_volume` task and `state: deleted_with_all_its_data`. |
| `the module writes lvcreate's option --size itself` | `lvcreate_options` holds an option for the name, the size, or the signatures. Leave the option out; write `name` and `size`. |
| `creating the logical volume ...:` and LVM's words | `lvcreate` refused, for example an option of `lvcreate_options` or a mirror that needs more devices than the volume group has. Read LVM's words at the end of the reason. MMX reports `changed` for a command that ran, also when LVM refused it and created nothing. |
| A reason that ends with `This was after lvcreate created the logical volume ...` | A command of LVM that reads failed right after the creation. The task reports `created: true`: another try would find the volume and report that the task created nothing. Run again; the next run compares the size. |

When a provider has enlarged a physical volume's device, run
[`lvm_volume_group`](../lvm_volume_group/README.md) in an earlier node task
with that device listed and
`grow_physical_volumes_with_their_devices: true`. This choice makes the extra whole
extents available to the volume group.

## Tested on

**Distributions:** Debian 12 and 13, Ubuntu 24.04 and 26.04, Rocky Linux 9, AlmaLinux 10.

The suite `tests/integration/partitions_and_lvm.py` runs the whole test on Debian 12 and 13 and Ubuntu 24.04 and 26.04, in a virtual machine. The table of
[`lvm_volume_group`](../lvm_volume_group/README.md#tested-on) lists the
versions of `lvm2`, 2.03.16 to 2.03.36. The suite grows an ext4 filesystem
while it is mounted, an xfs filesystem while it is not mounted, which is
where LVM 2.03.31 and newer differ from older LVM, and an ext3
filesystem. LVM 2.03.11 and 2.03.43 also grew an ext2 filesystem with its
volume.

Making thin pools and snapshots and moving data between devices are not
part of the module: they stay `shell` tasks. The module deletes a
snapshot or a thin volume as it deletes any volume, by its name.

## Why it works this way

- **`all_free_space_when_created` names when the size applies.** An existing
  volume keeps its size; the
  [creation-only naming rule](../../docs/changing-mmx/design/modules.md#a-setting-used-only-at-creation-says-so-in-its-name)
  makes that visible.

- **`grow_filesystem_with_volume` is required for `present`.**
  The right value depends on the task;
  [settings with no safe guess are required](../../docs/changing-mmx/design/modules.md#a-setting-with-no-safe-guess-is-required).

- **The module grows a volume and never shrinks one.** Growing is why
  teams use LVM, and it loses nothing. Shrinking destroys data when the
  filesystem is not shrunk first.
- **`grow_filesystem_with_volume` is explicit.** Most volumes hold
  a filesystem that LVM grows: ext2, ext3, ext4, or xfs. A grown volume
  with a small filesystem is a trap: the next run finds the volume at its
  size and grows nothing.
- **The module looks at the volume before it grows it with a filesystem.**
  LVM refuses a volume without a filesystem that it can grow, and no
  reading command of LVM says so beforehand. With the module's own look,
  a dryrun promises no growth that the run refuses, and the reason names
  what the volume holds. The mistake is loud and loses nothing.
- **A volume that holds nothing grows alone.** It has no filesystem that
  could be left small, so the module grows it with a plain `lvextend`.
  Otherwise a first run that stopped between `lvcreate` and the
  filesystem's task would block every later run with a larger size: the
  volume's task would fail, and the filesystem's task behind it would
  never run.
- **`lvcreate` gets `--yes --wipesignatures y`.** The space of a new volume
  can hold the signature of a filesystem that an earlier volume had there.
  `lvcreate` then asks whether to wipe it, answers "no" itself when it has
  no terminal, and creates nothing. That space belongs to no volume, so
  nothing that exists is lost, and a following
  `create_filesystem_on_empty_device` task finds the new volume empty.
- **`lvremove` gets `--yes`** only for `deleted_with_all_its_data`: the
  state's name already says that the data is destroyed. LVM still refuses
  a volume that is in use.
- **The module deletes no volume that other volumes depend on.** For a
  volume with snapshots, and for a thin pool with volumes in it, LVM asks
  whether to remove them too, and `--yes` would answer that question as
  well: on LVM 2.03.11 and 2.03.43, `lvremove --yes` deleted two snapshots
  with their volume, and two thin volumes with their pool. The task names
  one volume, so the module deletes one volume. A snapshot of a thin
  volume is a volume of its own and stays when its origin is deleted, so
  it is not in the way.
- **The module passes `--fsmode manage` where LVM has that option.** LVM
  2.03.31 and newer grow an xfs filesystem that is not mounted only with
  it, and then mount the filesystem for a moment themselves; older LVM
  does the same without the option. The module reads `lvextend --help` to
  learn which LVM the node has.
- **The module compares sizes after LVM's rounding.** LVM rounds a size up
  to whole extents. Without the same rounding, a task with `50MiB` would
  find its volume of 52 MiB "larger" and fail on every run.
- **The module checks free space and use itself** before it calls LVM, so
  the reason says in plain words what is in the way. LVM's own refusal
  stays behind it.
- **A state that an earlier node task may change does not fail a dryrun.**
  A playbook unmounts a filesystem and then deletes its volume, or adds a
  device to the volume group and then grows a volume. In its dryrun the
  earlier task has changed nothing yet. So the dryrun reports the later
  task as `not_checked`, and only a run, where the state is real, fails.
