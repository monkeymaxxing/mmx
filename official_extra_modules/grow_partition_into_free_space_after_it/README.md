# `grow_partition_into_free_space_after_it`

`grow_partition_into_free_space_after_it` grows one existing partition on
the node into the free space immediately after it, through `growpart`.
The module never shrinks, moves, creates, or removes a partition, and
leaves the partition's contents alone. The partition can stay mounted or
in use.

## Set up

Install growpart's package with the node's package manager. No service is
needed. The read commands also need `lsblk`, `wipefs`, and `blockdev` from
`util-linux`, and `sfdisk` from `fdisk` on Debian and Ubuntu or `util-linux`
on the other distributions. The partition-creation module's
[setup tasks](../create_partitions_on_empty_disk/README.md#set-up) install
sfdisk where it is missing.

```yaml
- when: {equals: ["{{ .node.package_manager }}", apt]}
  apt_package: {packages: [{name: cloud-guest-utils, version: newest_when_first_installed}]}
- when: {equals: ["{{ .node.package_manager }}", dnf]}
  dnf_package: {packages: [{name: cloud-utils-growpart, version: newest_when_first_installed}]}
```

## Inputs

| Input | Meaning | When omitted |
|---|---|---|
| `device` | Absolute path of the whole disk, such as `/dev/disk/by-id/scsi-0HC_Volume_101` or `/dev/sdb`. | Required. |
| `partition_number` | Integer, 1 or more: the partition's number in the disk's partition table. | Required. |

There is no `state` input. The provider must enlarge the disk first.
Another partition after the selected partition limits how far the selected
partition can grow.

## Outputs

The module has no outputs.

## What a dryrun reports

- **`would_change`** with `would grow partition 2 of /dev/sdb from 198 MiB
  to 523222528 bytes`. Sizes use the largest binary unit that gives a
  whole number, else bytes.
- **`unchanged`** when growpart says `NOCHANGE:`: no space, or too little
  space, follows the selected partition.
- **`not_checked`** while a program, the disk, its partition table, or the
  numbered partition is missing. An earlier node task may supply the
  missing program, disk, partition table, or numbered partition.
  A run fails once with the same reason.
- **A failure** for a relative or empty disk path, a partition number below
  1, a path that is no block device, a partition instead of a whole disk,
  a read-only medium, an unsupported sector size, or an unknown response
  from growpart. Read failures keep the tool's own words.
- **The reads change nothing:** `lsblk`, `wipefs --no-act`, `sfdisk --json`,
  `blockdev`, and `growpart --dry-run` do not change the disk or its contents.

## Examples

After the provider enlarges a disk, grow its second partition using the
stable link that names the disk:

```yaml
- grow_partition_into_free_space_after_it:
    device: /dev/disk/by-id/scsi-0HC_Volume_101
    partition_number: 2
```

Grow the first partition of `/dev/sdb`, then enlarge the LVM physical
volume on it through [`lvm_volume_group`](../lvm_volume_group/README.md):

```yaml
- grow_partition_into_free_space_after_it:
    device: /dev/sdb
    partition_number: 1
- lvm_volume_group:
    name: data
    physical_volumes: [/dev/sdb1]
    physical_volumes_not_listed: keep
    grow_physical_volumes_with_their_devices: true
```

For a partition holding a filesystem directly, the next node task uses
[`grow_filesystem_to_fill_device`](../grow_filesystem_to_fill_device/README.md).
The module `grow_partition_into_free_space_after_it` grows only the
partition.

## Errors and what they mean

| The reason holds | What it means, and what to do |
|---|---|
| `device: must not be empty` or `expected the absolute path` | Give the whole disk's absolute path. |
| `partition_number: must be 1 or more` | Give the number in the disk's partition table. |
| `the program growpart is not installed; install the package cloud-guest-utils (Debian and Ubuntu) or cloud-utils-growpart (the Red Hat family) in an earlier node task` | Install the package; see Set up. Other missing programs name their packages too. |
| `the disk ... does not exist yet` | Provide the disk before this task. |
| `is not a block device`, `not a whole disk`, or `read-only medium` | The path is unsuitable. Give a writable whole disk. |
| `the module knows disks with sectors of 512 and of 4096 bytes` | The disk's sector size is unsupported. |
| `has no partition table` or `has no partition ...` | Create the table or partition in an earlier node task. A dryrun leaves this task not checked. |
| `printed what MMX cannot read` or `proposed size or first sector differs` | A read response is unknown or inconsistent. No growpart change runs. Inspect the tool's words. |
| `checking growth of partition ...` | The growpart dry run failed or returned an unknown answer. Nothing grew. Read growpart's words. |
| `growing partition ...` and `The partition still has ...` | Growpart failed without changing the partition size. MMX may try again. |
| `growing partition ...` and `The partition now has ...` | Growpart changed the table before it failed. MMX does not try again and hide that failure. Inspect the partition and read growpart's words before rerunning. |
| `The partition size could not be read afterwards` or `reported success, but ... differs` | The change cannot be confirmed. MMX does not try again. Inspect the table and kernel size before rerunning. |

Every failure reason starts with the module's name. MMX reports `changed`
when a changing command ran, even when growpart refused it and grew nothing.

## Tested on

**Distributions:** Debian 12 and 13, Ubuntu 24.04 and 26.04, Rocky Linux 9, AlmaLinux 10.

The supplied node measurements cover Debian 13, growpart 0.33, and
util-linux 2.41.5: GPT growth with a mounted second partition, kernel size
updates, and unchanged reruns and first partitions.

The suite `tests/integration/partitions_and_lvm.py` includes a growth case,
with an ext4 filesystem mounted on the partition, on Debian 12 and 13 and Ubuntu 24.04 and 26.04; the
[validation record](../../docs/changing-mmx/validation.md#the-capability-reviews-additions)
has the latest results.
The stand-in Go tests cover failures and unknown output; their invented
failure messages are not measurements of growpart. Growpart's responses
for missing partitions, missing tables, and non-block paths were not measured.

## Why it works this way

- Growpart is the tool cloud images use at boot for this work. It maintains
  the partition table's backup header and provides a dry run; the module
  does not rewrite the table itself.
- The module grows only a partition. Growing an LVM physical volume or a
  filesystem is a separate node task, so each step reports its own result.
- A missing partition is a missing prerequisite. An earlier node task may
  create it, and a dryrun must not fail merely because that task changed nothing.
- After a change the module reads the table and the kernel's partition size.
  It reports the size it found, and preserves a failure that another try could hide.
