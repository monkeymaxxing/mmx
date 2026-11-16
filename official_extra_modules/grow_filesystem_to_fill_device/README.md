# `grow_filesystem_to_fill_device`

`grow_filesystem_to_fill_device` grows an ext4 or XFS filesystem on a node
to fill its block device, through `resize2fs` or `xfs_growfs`. A filesystem
holds the files on a device; enlarging the device alone leaves the
filesystem smaller. The module never shrinks, moves, checks, repairs, or
creates a filesystem. The filesystem must be mounted for writing.

## Set up

Install the tools on the node. No service needs to be started. These
packages have the same names across Debian 12 and 13 and Ubuntu 24.04 and 26.04:

```yaml
- when: {equals: ["{{ .node.package_manager }}", apt]}
  apt_package: {packages: [{name: util-linux, version: newest_when_first_installed}, {name: e2fsprogs, version: newest_when_first_installed}, {name: xfsprogs, version: newest_when_first_installed}]}
- when: {equals: ["{{ .node.package_manager }}", dnf]}
  dnf_package: {packages: [{name: util-linux, version: newest_when_first_installed}, {name: e2fsprogs, version: newest_when_first_installed}, {name: xfsprogs, version: newest_when_first_installed}]}
```

For ext4 alone, `xfsprogs` is not needed. For XFS alone, `e2fsprogs` is not
needed. `util-linux` supplies `blockdev`, `findmnt`, `lsblk`, and `wipefs`;
`e2fsprogs` supplies `dumpe2fs` and `resize2fs`; `xfsprogs` supplies
`xfs_info` and `xfs_growfs`.

## Inputs

| Input | Meaning | When omitted |
|---|---|---|
| `device` | The absolute path of the block device holding the filesystem, including a symbolic link to the block device. | Required. |
| `filesystem_type` | The expected filesystem, `ext4` or `xfs`. | Required. |

The module has no `state` input. It probes the device's signatures before
reading the filesystem's size. A different type is never replaced.
The module matches mounts by kernel device numbers, so a device link and
the link's underlying path name the same filesystem. If several mounts
refer to the device, the module uses the first writable mount of the
expected type. A read-only
mount does not block another writable mount. The device-link and multiple
mount cases have stand-in tests, but have not been measured on a real node.

## Outputs

The module has no outputs.

## What a dryrun reports

- **`would_change`** with `would grow the ext4 filesystem on
  /dev/data/postgresql from 300 MiB to 600 MiB`, or the same reason with
  `xfs`. The target is the device size rounded down to whole filesystem
  blocks. A run reports the actual size it reads after growth.
- **`unchanged`** with no reason when no whole block can be gained. The
  module never tries to shrink a filesystem larger than the device.
- **`not_checked`** when a needed program or the device is missing, the
  device holds another type or no filesystem signature, or the filesystem
  is unmounted or has no writable mount. A run fails once with the same
  reason and `failure_kind: missing_prerequisite`.
- **A failure** for invalid inputs or an unreadable command result, as in
  a run. A read command failure includes the tool's words.
- **What its reads leave on the node:** the module requests only reads:
  `lsblk --json --nodeps`, `wipefs --no-act --json`, `findmnt --json
  --list`, `blockdev --getsize64`, and `dumpe2fs -h` or `xfs_info`.
  The module writes no file. The read commands' incidental effects have not
  been measured.

## Examples

Grow the mounted ext4 filesystem after a provider enlarges its volume:

```yaml
- grow_filesystem_to_fill_device:
    device: /dev/disk/by-id/scsi-0HC_Volume_101
    filesystem_type: ext4
```

Grow the mounted XFS filesystem after someone enlarges its logical volume:

```yaml
- grow_filesystem_to_fill_device:
    device: /dev/data/postgresql
    filesystem_type: xfs
```

Mount an existing ext4 filesystem before growing it. `fstab_mount` records
the mount in `/etc/fstab` and mounts it on the node:

```yaml
- fstab_mount:
    check_filesystem_at_boot: never
    options: [defaults]
    mount_point: /var/lib/postgresql
    mount_source: /dev/data/postgresql
    filesystem_type: ext4
    state: mounted

- grow_filesystem_to_fill_device:
    device: /dev/data/postgresql
    filesystem_type: ext4
```

## Errors and what they mean

Every failure names `grow_filesystem_to_fill_device`. MMX reports `changed`
for a grow command that ran, even if the tool refused it. A failed command
is tried again only when the size read afterwards is unchanged. A failure
after growth or an unreadable result after the command is not tried again;
another try could hide what happened. A successful command that grows
nothing also fails with the tool's words and may be tried again: the size
read afterwards is unchanged, so another try cannot hide growth.

| The reason holds | What it means, and what to do |
|---|---|
| `grow_filesystem_to_fill_device.device: must not be empty` | Supply the absolute device path. |
| `grow_filesystem_to_fill_device.device: expected the absolute path of a block device` | Supply a path starting with `/`, such as `/dev/data/postgresql`. |
| `the program resize2fs is not installed; install the package e2fsprogs in an earlier node task` | Install the named package first; see Set up. The same form names every other missing program. |
| `the device ... does not exist yet` | Attach or create the device in an earlier node task. |
| `the device ... holds ..., not the expected ... filesystem` | The reason names the signatures found, or says `no filesystem signature`. Create the expected filesystem on an empty device first; the module replaces nothing. |
| `the ... filesystem on ... is not mounted` | Mount it for writing first, for example with `fstab_mount`. |
| `the ... filesystem on ... is mounted read-only` | Mount it for writing first with `fstab_mount`. |
| `the mount ... has type ..., not ...` | The mount differs from the expected filesystem. Mount the expected filesystem first. |
| `reading with ...:` and the tool's words | A read failed; it does not mean the device is missing. Resolve the tool's error. |
| `... printed what MMX cannot read for ...:` | A read returned unexpected geometry or JSON. The reason quotes the text; check the tool and device. |
| `growing the ... filesystem on ...:` and the tool's words | Growth failed. The reason reports the size found afterwards, when readable. Resolve the tool's error. |
| `The filesystem is no larger (...)` | The grow tool exited with zero but gained no space. Its words precede this sentence, such as `Nothing to do!`. Check the device and filesystem geometry before running again. |

## Tested on

**Distributions:** Debian 12 and 13, Ubuntu 24.04 and 26.04, Rocky Linux 9, AlmaLinux 10.

Go tests use stand-in programs with the supplied Debian 13 measurements:
`e2fsprogs` 1.47.2 and `xfsprogs` 6.13.0. They do not prove real growth.
The cases in `tests/integration/partitions_and_lvm.py` target Debian 12 and 13 and Ubuntu 24.04 and 26.04; the
[validation record](../../docs/changing-mmx/validation.md#the-capability-reviews-additions)
has the latest results. The versions of e2fsprogs and xfsprogs on the other
five distributions were not recorded.

## Why it works this way

- **Only growth.** The device must already hold the expected filesystem.
  Shrinking or replacing the filesystem can lose data and belongs to other
  work.
- **Mounted for writing.** XFS grows only while mounted. An unmounted ext4
  filesystem can require `e2fsck` first. The module never checks or repairs
  a filesystem.
- **Read numbers before and after.** A tool can exit with zero and grow
  nothing. `dumpe2fs` and `xfs_info` give the filesystem's own block counts;
  the module reports those numbers, never progress text.
- **An earlier node task may provide a prerequisite.** A first dryrun
  creates and mounts nothing. A missing prerequisite leaves it not checked,
  while a run fails once and names what must be provided.
