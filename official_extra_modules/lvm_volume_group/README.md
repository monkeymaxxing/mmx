# `lvm_volume_group`

`lvm_volume_group` manages one LVM volume group on the node and the
devices it consists of, which LVM calls physical volumes. It runs the
programs of the package `lvm2`, such as `vgcreate`, `vgextend`, and `pvresize`.
The module grows listed physical volumes when the physical volumes'
devices grow. The module never wipes a device: a device that holds anything
but an unused physical volume fails the task.

```yaml
- lvm_volume_group:
    name: data
    physical_volumes: [/dev/disk/by-partlabel/lvm]
    physical_volumes_not_listed: keep
    grow_physical_volumes_with_their_devices: true
```

[`lvm_logical_volume`](../lvm_logical_volume/README.md) then makes logical
volumes in the volume group.

## Set up

Install the package `lvm2`, which has the same name on every distribution.
No service has to be started.

```yaml
- when: {equals: ["{{ .node.package_manager }}", apt]}
  apt_package: {packages: [{name: lvm2, version: newest_when_first_installed}]}
- when: {equals: ["{{ .node.package_manager }}", dnf]}
  dnf_package: {packages: [{name: lvm2, version: newest_when_first_installed}]}
```

The module also runs `lsblk` and `wipefs` from the package `util-linux`,
which every node has.

## Inputs

| Input | Meaning | When omitted |
|---|---|---|
| `name` | The volume group's name: 1 to 127 letters, digits, `+`, `_`, `.`, and `-`. | Required. |
| `state` | `present` or `absent`. `absent` accepts only `name`. | `present` |
| `physical_volumes` | The devices of the volume group, each as an absolute path, such as `/dev/sdb` or `/dev/disk/by-partlabel/lvm`; at least one. | Required for `present`. |
| `physical_volumes_not_listed` | `remove` or `keep`: what happens to a device of the volume group that the task does not list. | Required with `physical_volumes`. |
| `grow_physical_volumes_with_their_devices` | Boolean, only for `present`. Grow listed physical volumes already in this volume group when their devices have room for at least one more extent; `false` leaves them alone. Never shrinks. | Required for `state: present`. |

With `state: present`:

- **A missing volume group** is created on the listed devices
  (`vgcreate`).
- **A listed device that the volume group does not have** is added
  (`vgextend`).
- **A listed physical volume already in this volume group** grows with
  `pvresize <device>` when its device has room for at least one more
  extent and `grow_physical_volumes_with_their_devices` is `true`. The
  reason gives the device, volume group, and old and new sizes. A device
  this task adds needs no growth: a new physical volume uses its whole
  device. Unlisted physical volumes stay as they are. The module never
  shrinks a physical volume or passes `--setphysicalvolumesize`.
- **A device that the volume group has and the task does not list** stays
  with `physical_volumes_not_listed: keep`. With `remove` the module takes
  it out of the volume group (`vgreduce`) and removes LVM's label from it
  (`pvremove`), so the device is empty again. A device that still holds
  data of a logical volume fails the task and stays.

A device that the module adds must hold nothing, or be an LVM physical
volume that belongs to no volume group. A device with a filesystem, a swap
area, a partition table, or any other signature fails the task and is
named, and so does a device of another volume group. The module compares
devices by where the device paths lead. When `/dev/disk/by-partlabel/lvm`
leads to `/dev/sdb1`, the module treats the paths as one device.

With `state: absent`, the module removes a volume group that has no
logical volume (`vgremove`), and LVM's label from each of its devices. A
volume group that still has logical volumes fails the task, which names
them. A volume group that does not exist is unchanged.

## Outputs

| Output | Meaning |
|---|---|
| `created` | `true` when the task created the volume group, and in a dryrun when a run would create it. Otherwise `false`. |

A later node task reads the `created` output as `.node_tasks.<id>.created`.

## What a dryrun reports

- **`would_change`** with what a run would do: `would create the volume
  group data on /dev/disk/by-partlabel/lvm`, `would add /dev/sdc to the
  volume group data`, `would take /dev/sdd out of the volume group data
  and remove LVM's label from it`, or `would remove the volume group data
  and LVM's label from /dev/sdb1`.
- **`would_change`** with `would grow the physical volume /dev/sdb of the
  volume group data from 196 MiB to 496 MiB` when a listed physical
  volume can gain at least one extent. The new size is computed from
  LVM's device size, extent start, and extent size; no `pvresize` runs.
- **`unchanged`** when the volume group has the listed devices and none
  needs growth, or growth is disabled.
- **A failure**, with the same reason as in a run, for a device that holds
  something. No node task changes such a state.
- **`not_checked`** for a state that an earlier node task of the same
  playbook may change, because in a dryrun that task has changed nothing
  yet: while a program of `lvm2` is missing, while a listed device does
  not exist or belongs to another volume group, while a device that would
  leave still holds data of a logical volume, and for `absent` while the
  volume group still has logical volumes. A run fails once with the same
  reason.
- **What a dryrun cannot know:** whether LVM can open a device for itself
  alone. LVM refuses a device that device-mapper holds, such as an
  encrypted volume without a header. The run then fails with LVM's words,
  `Can't open /dev/sdc exclusively`, and writes nothing.
- **What its reads leave on the node:** `vgs`, `pvs`, and `lvs` write only
  LVM's own files below `/run`, which a reboot removes: `/run/lvm/hints` on
  Debian 13, and the directory `/run/lock/lvm` on Rocky Linux 9. `lsblk`
  and `wipefs --no-act` leave nothing.

## Examples

A second disk joins the volume group, so its logical volumes can grow.
Devices that someone added by hand stay:

```yaml
- lvm_volume_group:
    name: data
    physical_volumes: [/dev/disk/by-partlabel/lvm, /dev/sdc]
    physical_volumes_not_listed: keep
    grow_physical_volumes_with_their_devices: true
```

The list is the whole volume group. A device that the task no longer lists
leaves the volume group when it holds no data:

```yaml
- lvm_volume_group:
    name: data
    physical_volumes: [/dev/sdb, /dev/sdc]
    physical_volumes_not_listed: remove
    grow_physical_volumes_with_their_devices: true
```

A volume group that is no longer needed is removed, after its logical
volumes:

```yaml
- lvm_logical_volume:
    name: postgresql
    volume_group: data
    state: deleted_with_all_its_data

- lvm_volume_group:
    name: data
    state: absent
```

## Errors and what they mean

Every reason starts with `lvm_volume_group`. How often MMX tries a task
that fails depends on what failed:

- **Once, in a dryrun too,** when no node task changes what is in the way:
  an input that breaks a rule, or a device that holds something.
- **Once in a run, and not checked in a dryrun,** when an earlier node
  task may change what is in the way, such as logical volumes that it
  deletes first, or a device that it takes out of another volume group.
- **Once,** when the read after `pvresize` fails, or a failed `pvresize`
  grew the physical volume: another try could hide the failure.
- **Once,** when a device left the volume group and kept LVM's label:
  another try would find nothing to do and report success.
- **Again,** when another command of LVM failed.

| The reason holds | What it means, and what to do |
|---|---|
| `the program vgs is not installed; install the package lvm2 ...` | A program that the module runs is not on the node. Install `lvm2` in an earlier node task; see [Set up](#set-up). |
| `lvm_volume_group.state: absent accepts only name; remove physical_volumes` | A task with `state: absent` holds another input. Remove the input that the reason names. |
| `the device /dev/sdb1 does not exist yet` | The node has no such device. Check the path. A dryrun reports the task as `not_checked`, because an earlier node task may create the partition. |
| `the device /dev/sdb1 is not empty; found: ...` | The device holds a filesystem, a swap area, a partition table, or another signature, which the reason names. The module never wipes a device. When nothing on `/dev/sdb1` is needed, wipe `/dev/sdb1` by hand with `wipefs --all /dev/sdb1`. |
| `the device /dev/sdb1 belongs to the volume group backup` | The device is a physical volume of another volume group. List another device, or take the device out of the other volume group in an earlier node task. |
| `the device /dev/sdc of the volume group data still holds 12 GiB of logical volumes` | `physical_volumes_not_listed: remove` would take out a device that holds data. Nothing was changed. Move the data to the other devices with `pvmove /dev/sdc`, delete the logical volumes in an earlier node task, or list the device. |
| `the volume group data still has the logical volumes postgresql, logs` | `state: absent` for a volume group that holds logical volumes. Nothing was removed. Delete each logical volume in an earlier node task, with `lvm_logical_volume` and `state: deleted_with_all_its_data`. |
| `physical_volumes[0] and physical_volumes[1] lead to the same device, /dev/sdb1` | Two paths of the list lead to one device. List each device once. |
| `/dev/sdc left the volume group data, and LVM's label stays on it`, or `the volume group data was removed, and LVM's label stays on /dev/sdc`, and LVM's words | The device is out of the volume group, and `pvremove` could not remove LVM's label from it. Remove the label by hand with `pvremove /dev/sdc`, as the reason says. Until then, other tasks refuse the device as an LVM physical volume; this module takes it as a device to add. |
| `creating the volume group data on ...:` or `adding ... to the volume group data:`, and LVM's words | `vgcreate` or `vgextend` refused. LVM itself refuses a device that holds a signature, and a device that device-mapper holds (`Can't open /dev/sdc exclusively`). Read LVM's words at the end of the reason. MMX reports `changed` for a command that ran, also when LVM refused it and wrote nothing. |
| `lvm_volume_group.state: absent accepts only name; remove grow_physical_volumes_with_their_devices` | Growth applies only to `present`. Remove the input. |
| `growing the physical volume ... from ... to ...:`, and LVM's words | `pvresize` failed. The reason gives the sizes before and after. Read LVM's words and check `pvs` on the node. MMX reports `changed` for the command that ran. |
| `pvresize reported success but gained no extents while growing ...` | LVM exited with success, but the extent count did not increase. The reason gives LVM's words and both sizes. Check `pvs` on the node; MMX may try again. To stop the attempts for this volume group, set `grow_physical_volumes_with_their_devices: false`. |
| `This was after pvresize tried growing ...`, or `the physical volume has no row in LVM's list` | The verification read failed or no longer found the physical volume in this volume group. Check `pvs` on the node. MMX does not try again because growth may have happened. |
| `pvs printed what MMX cannot read for ...`, or `pvs printed a zero vg_extent_size ...` | LVM returned a size or extent count MMX cannot use. Check `pvs` on the node. |
| `vgs could not read LVM's state:` and LVM's words | A command that reads failed. Read LVM's words; run `vgs` on the node. |

When a run ends between `vgreduce` and `pvremove`, a device also keeps
LVM's label although it belongs to no volume group any more: `pvs` lists
it with an empty volume group. Such a device holds no data. This module
takes it as a device to add; to use it for something else, remove the
label by hand with `pvremove <device>`.

## Tested on

**Distributions:** Debian 12 and 13, Ubuntu 24.04 and 26.04, Rocky Linux 9, AlmaLinux 10.

The suite `tests/integration/partitions_and_lvm.py` runs the whole test on Debian 12 and 13 and Ubuntu 24.04 and 26.04, in a virtual machine, on loop devices and on a
partition:

| Distribution | `lvm2` | Has the package as installed |
|---|---|---|
| Debian 12 | 2.03.16 | no |
| Debian 13 | 2.03.31 | no |
| Ubuntu 24.04 | 2.03.16 | no |
| Ubuntu 26.04 | 2.03.31 | no |
| Rocky Linux 9 | 2.03.33 | no |
| AlmaLinux 10 | 2.03.36 | yes |

## Why it works this way

- **`grow_physical_volumes_with_their_devices` is required for `present`.**
  The right value depends on the task;
  [settings with no safe guess are required](../../docs/changing-mmx/design/modules.md#a-setting-with-no-safe-guess-is-required).

- **With `grow_physical_volumes_with_their_devices: true`, physical volumes
  grow with their devices.** Provider disk growth then makes space
  available to later logical-volume tasks.
  Set `grow_physical_volumes_with_their_devices: false` to leave a smaller
  physical volume alone.
- **The module checks whole extents before and after growth.** One `pvs`
  report supplies byte values for `dev_size`, `pe_start`, `vg_extent_size`,
  `pv_size`, and the extent count `pv_pe_count`. Growth runs only when
  `floor((dev_size - pe_start) / vg_extent_size) - pv_pe_count >= 1`;
  a dryrun predicts `floor((dev_size - pe_start) / vg_extent_size) * vg_extent_size`
  bytes. `pvresize` prints “changed” and exits with success
  even when it gains nothing, so its progress text cannot prove growth.
  A physical volume with a second metadata area keeps that area at the end
  of its device, where it takes room from the extents. For such a volume
  the module leaves the area's size (`pv_mda_size`) and one more MiB out of
  the count, so that a dryrun never promises growth that `pvresize` does
  not give (measured with LVM 2.03.31).

- **The module probes each device itself before it adds it,** with `wipefs
  --no-act` and `pvs`. LVM also refuses a device that holds a signature,
  but no reading command of LVM says so beforehand. With its own probe the
  module names what it found in plain words, and a dryrun fails as a run
  does. The module never passes `--yes` or `--force` to `vgcreate` or
  `vgextend`, so LVM's refusal stays the second guard.
- **A device that leaves a volume group is made clean.** LVM leaves its
  label on such a device. Every later task, such as
  `create_filesystem_on_empty_device`, would then refuse the device as "an
  LVM physical volume", although it holds no data of a logical volume. So
  the module removes the label with `pvremove`. There is one exception: a
  run that ends between the two commands, or whose `pvremove` fails,
  leaves the label. [Errors and what they mean](#errors-and-what-they-mean)
  says what to do then.
- **An unused physical volume is a device the module can add.** It holds no
  data, and LVM takes it without a question. Without this rule the module
  could never add a device again that an interrupted run left with its
  label.
- **The module checks itself whether a device can leave.** LVM refuses to
  take out a device that still holds data of a logical volume. The module
  reads how much each device holds and fails before it changes anything.
- **A state that an earlier node task may change does not fail a dryrun.**
  A playbook deletes the logical volumes and then removes their volume
  group. In its dryrun the logical volumes are still there. So the dryrun
  reports the volume group's task as `not_checked`, and only a run, where
  the state is real, fails.
- **The module does not look whether device-mapper holds a device.** LVM
  opens each new device for itself alone and refuses one that is held, on
  LVM 2.03.11 and on 2.03.43, before it writes anything.
- **`physical_volumes_not_listed` is required.** A task that lists devices
  must say what happens to the others: nobody should lose a device from a
  volume group, or keep one, by a default they did not read.
- **`absent` removes only an empty volume group.** A volume group with
  logical volumes holds data. Removing data is spelled out, on
  `lvm_logical_volume`, as `deleted_with_all_its_data`.
- **The module asks LVM for every volume group and picks its own.** A
  command that names a missing volume group fails, and older and newer LVM
  write that failure in different places. A list in which the volume group
  is missing needs no such reading.
