# `create_partitions_on_empty_disk`

`create_partitions_on_empty_disk` writes a GPT partition table with named
partitions on a disk of the node that holds nothing yet, such as a new
volume attached to a cloud server. The module runs the program `sfdisk`.
The module's one job is to never destroy data. The module writes only on
a disk that holds no partition table and no signature of any kind, and
has no setting that overrides that check.

```yaml
- create_partitions_on_empty_disk:
    device: /dev/sdb
    partitions:
      - {name: database, size: 200GiB}
      - {name: backups, size: rest_of_disk}

- create_filesystem_on_empty_device:
    device: /dev/disk/by-partlabel/database
    filesystem_type: ext4
```

Later node tasks use a partition by its name, as
`/dev/disk/by-partlabel/<name>`. The `/dev/disk/by-partlabel/<name>` path
does not depend on how the kernel numbers the disk.

## Set up

The module needs four programs on the node: `sfdisk`, `wipefs`, `lsblk`,
and `udevadm`. Most nodes have all four. `sfdisk` is the one that a small
installation of Debian or Ubuntu can lack: there it is in the package
`fdisk`, on every other distribution in `util-linux`.

```yaml
- when: {equals: ["{{ .node.package_manager }}", apt]}
  apt_package: {packages: [{name: fdisk, version: newest_when_first_installed}]}
- when: {equals: ["{{ .node.package_manager }}", dnf]}
  dnf_package: {packages: [{name: util-linux, version: newest_when_first_installed}]}
```

`wipefs` and `lsblk` are in `util-linux` on every distribution. `udevadm`
belongs to the node's device manager, which every node with systemd runs:
the package is `udev` on Debian and Ubuntu, and `systemd-udev` on the Red
Hat family. No service has to be started.

## Inputs

| Input | Meaning | When omitted |
|---|---|---|
| `device` | The absolute path of the whole disk, such as `/dev/sdb`. A partition, such as `/dev/sdb1`, fails the task. | Required. |
| `partitions` | The partitions in their order on the disk, as a list of at least one and at most 128. | Required. |

Each partition has these keys:

| Key | Meaning | When omitted |
|---|---|---|
| `name` | The partition's GPT name: 1 to 36 letters, digits, `-`, `_`, and `.`, and not `.` or `..`. No two partitions of the task have the same name. | Required. |
| `size` | A whole number and `MiB`, `GiB`, or `TiB`, such as `200GiB`. The last partition can have `rest_of_disk` instead: it then takes the space that is left, up to the last whole MiB of the disk. | Required. |
| `type` | `linux_filesystem`, `linux_lvm`, `linux_swap`, `linux_raid`, or `efi_system`. | `linux_filesystem` |

The first partition starts at 1 MiB, and each next one follows directly.

The module probes the disk and does one of three things:

- **The disk holds nothing and nothing uses it** (no partition table, no
  signature of any kind, and no other device on it): the module writes the
  partition table.
- **The disk has a GPT partition table with exactly these partitions** (the
  same names, first sectors, sizes, and types, in the same order): the task
  is unchanged. The module still checks that each partition's link exists
  and leads to this disk.
- **The disk holds anything else** (other partitions, a filesystem, a swap
  area, an LVM physical volume), **or is in use:** the task fails and
  names what it found.

A disk can be in use although it holds no signature: an encrypted volume
without a header, or any other device-mapper device on the disk, and a
partition that the kernel still has from a partition table that someone
wiped. The module asks `lsblk` which devices are on the disk and writes
only when there is none.

The module has no `state: absent`, no resize, and no force option.
To grow an existing partition after its disk grows, use
[`grow_partition_into_free_space_after_it`](../grow_partition_into_free_space_after_it/README.md). To use a
disk that held something, first make sure that nothing on it is needed,
then wipe the disk by hand, such as with `wipefs --all /dev/sdb`. Wiping the
disk is deliberate and stays outside MMX.

## Outputs

| Output | Meaning |
|---|---|
| `created` | `true` when the task wrote the partition table, and in a dryrun when a run would write it. Otherwise `false`. |

A later node task reads `created` as `.node_tasks.<id>.created`, and names a
partition by its link, `/dev/disk/by-partlabel/<name>`.

## What a dryrun reports

- **`would_change`** for a disk that holds nothing, with the reason `would
  create a GPT partition table on /dev/sdb with the partitions: database
  (200 GiB), backups (1662 GiB, the rest of the disk)`. The dryrun also
  runs `sfdisk --no-act`, which goes through sfdisk's steps without
  writing, so a partition table that sfdisk would refuse fails the dryrun
  as it fails a run.
- **`unchanged`** for a disk that already has exactly these partitions.
- **A failure**, with the same reason as in a run, for a disk that holds
  anything else, for a disk in use, for partitions that do not fit, and
  for a name that another disk's partition already has. No node task
  changes such a state.
- **`not_checked`** while the device does not exist, and while one of the
  four programs is missing: an earlier node task of the same playbook may
  attach the disk or install the package. A run fails once with the same
  reason.
- **What a dryrun cannot know:** whether a program holds the disk open for
  itself alone, which only the real `sfdisk` finds, and whether the
  partitions' links appear, which shows only after the write.
- **What its reads leave on the node:** nothing. `lsblk`, `wipefs
  --no-act`, and `sfdisk --no-act` write no file and nothing on the disk.
  The node's device manager looks at the disk once more (one `change`
  event of udev), which changes nothing.

## Examples

A new disk gets a partition of 200 GiB for a database, and a second
partition with the rest of the disk for backups:

```yaml
- create_partitions_on_empty_disk:
    device: /dev/sdb
    partitions:
      - {name: database, size: 200GiB}
      - {name: backups, size: rest_of_disk}
```

A new disk gets one partition for LVM that takes the whole disk. The
volume group of the next node task uses it by its name:

```yaml
- create_partitions_on_empty_disk:
    device: /dev/sdb
    partitions:
      - {name: lvm, size: rest_of_disk, type: linux_lvm}

- lvm_volume_group:
    name: data
    physical_volumes: [/dev/disk/by-partlabel/lvm]
    physical_volumes_not_listed: keep
    grow_physical_volumes_with_their_devices: true
```

A new NVMe disk gets a swap partition and a partition for data. The
partitions' names do not depend on the kernel's name for the disk:

```yaml
- create_partitions_on_empty_disk:
    device: /dev/nvme1n1
    partitions:
      - {name: swap, size: 8GiB, type: linux_swap}
      - {name: data, size: rest_of_disk}
```

## Errors and what they mean

Every reason starts with `create_partitions_on_empty_disk`. How often MMX
tries a task that fails depends on what failed:

- **Once, in a dryrun too,** when no node task changes what is in the way:
  an input that breaks a rule, or a disk that holds data or is in use.
- **Once in a run, and not checked in a dryrun,** while the device or a
  program is missing, which an earlier node task may provide.
- **Once,** when the task wrote the partition table and then failed:
  another try would find the table and could report success.
- **Again,** when a command failed before anything was written.

| The reason holds | What it means, and what to do |
|---|---|
| `the program sfdisk is not installed; install the package ...` | A program that the module runs is not on the node. The reason names the program and its package. Install the package in an earlier node task; see [Set up](#set-up). |
| `the device /dev/sdb does not exist yet` | The node has no such device. Check the path on the node with `lsblk`. A dryrun reports the task as `not_checked`, because an earlier node task may attach the disk. |
| `/dev/sdb1 is a partition of the disk /dev/sdb, not a whole disk` | `device` names a partition. Write the whole disk as `device`. |
| `... is not a block device` | `device` names a file or a directory. Write the path of the disk. |
| `/dev/sdb is not empty; found: ...` | The disk holds a filesystem, a swap area, an LVM physical volume, or another signature. The reason names each kind that `wipefs` found. The module never writes on such a disk. When nothing on it is needed, wipe it by hand with `wipefs --all /dev/sdb`. |
| `/dev/sdb is in use, although it holds no signature: the device /dev/mapper/secret (dm) is on it` | Another device is on the disk, such as an encrypted volume without a header. Nothing was written. Find what uses the disk with `lsblk /dev/sdb`; the module writes only on a disk that nothing uses. |
| `/dev/sdb is in use, although it holds no signature: the kernel still has the partition /dev/sdb1 of an earlier partition table` | Someone wiped the disk's partition table while the kernel could not read the disk again, for example while a filesystem of it was mounted. Nothing was written. When nothing uses the partition any more, run `blockdev --rereadpt /dev/sdb`, as the reason says. |
| `/dev/sdb already has a GPT partition table with other partitions than the task lists` | The disk has partitions, and they are not exactly the task's. The reason lists both. Correct the task so that it describes the disk, or wipe the disk by hand when nothing on it is needed. The module never changes an existing partition table. |
| `the partitions do not fit on /dev/sdb` | The sizes add up to more than the disk holds. A partition table needs the first MiB of the disk and 33 sectors at its end (5 sectors of 4096 bytes). Make a partition smaller, or give the last one `rest_of_disk`. |
| `the name database is already the name of the partition /dev/sda3 on the disk /dev/sda` | Another disk of the node has a partition with this name, so `/dev/disk/by-partlabel/database` already leads there. Give the new partition another name. |
| `/dev/sdb is in use, and sfdisk wrote nothing` | `sfdisk` itself found the disk in use when it was about to write, for example because a program holds the disk open for itself alone. Find what uses the disk with `lsblk /dev/sdb` and `findmnt`. MMX tries the task again, because a disk can be in use for a moment, and reports `changed` for the command that ran although it wrote nothing. |
| `sfdisk did not write the partition table on /dev/sdb: ...` | `sfdisk` refused for another reason; the reason ends with sfdisk's own words. Read sfdisk's error message. Nothing was written. |
| `sfdisk reported success, but the partition table of /dev/sdb is not the one the task lists` | `sfdisk` wrote another table than its script asked for. The reason lists what the disk has now. Compare the disk with the task (`sfdisk --json /dev/sdb`); the next run fails for a disk with other partitions. |
| `... but /dev/disk/by-partlabel/database does not exist` | The disk has the task's partition table, and the kernel did not read it, so the partitions have no device files. The reason starts with `the partition table was written on /dev/sdb` in the run that wrote it, and with `/dev/sdb has the partition table that the task lists` in every run after it. For a loop device, attach it with `losetup --partscan`. For another disk, run `partx --add /dev/sdb`. The next run then finds the links and is unchanged. |
| `... but /dev/disk/by-partlabel/database leads to /dev/sdc1 on the disk /dev/sdc` | Two partitions of the node have the same name, and the link leads to the other one. Give one of the two partitions another name by hand. |
| A reason that ends with `This was after the partition table was written on /dev/sdb; the next run checks the disk again` | A command that reads failed after the write. The table is on the disk. Run again: the next run compares the table and checks the links. |
| `sectors of ... bytes` | The disk's sector size is neither 512 nor 4096 bytes. The module does not write such a disk; partition it by hand. |

A successful run can end with a note in its reason: `the new partition
database holds what was on the disk before: ext4 filesystem (ext4 signature
at offset 0x438); the module wiped nothing`. Someone wiped the disk's old
partition table and left the filesystem behind it, and the new partition
begins where the old one began. A following
`create_filesystem_on_empty_device` task with the same type then reports
`unchanged` and uses the old filesystem with its old files; with another
type it fails. Wipe that partition by hand when the old filesystem is not
wanted.

## Tested on

**Distributions:** Debian 12 and 13, Ubuntu 24.04 and 26.04, Rocky Linux 9, AlmaLinux 10.

The suite `tests/integration/partitions_and_lvm.py` runs the whole test on Debian 12 and 13 and Ubuntu 24.04 and 26.04, in a virtual machine, on loop devices with 512
and with 4096 bytes in a sector:

| Distribution | `sfdisk` (util-linux) | Package of `sfdisk` |
|---|---|---|
| Debian 12 | 2.38.1 | `fdisk` |
| Debian 13 | 2.41.5 | `fdisk` |
| Ubuntu 24.04 | 2.39.3 | `fdisk` |
| Ubuntu 26.04 | 2.41.3 | `fdisk` |
| Rocky Linux 9 | 2.37.4 | `util-linux` |
| AlmaLinux 10 | 2.40.2 | `util-linux` |

Every disk of the suite is a loop device. The suite includes disks whose
sizes are not whole multiples of one MiB and a disk that device-mapper
holds. A disk with a SCSI or an NVMe interface is not part of the suite.

## Why it works this way

- **The module's own probe is the whole guard.** `sfdisk` itself protects
  only a disk that is in use. On the test nodes it replaced a partition
  table, wrote over a filesystem that was not mounted, and wrote a
  partition table into a partition, each time without an error. So the
  module asks `lsblk` whether the device is a whole disk and `wipefs
  --no-act` whether it holds any signature, and runs `sfdisk` only on a
  disk that holds none.
- **The module also asks which devices are on the disk.** A disk that
  device-mapper holds can have no signature, such as an encrypted volume
  without a header. `sfdisk` with util-linux 2.37.2 and with 2.42.4 refused
  such a disk when it was about to write, and `sfdisk --no-act` did not.
  With the module's own look, a dryrun fails as a run does, and the reason
  names the device that is on the disk.
- **The module computes every sector itself.** Given only sizes, `sfdisk`
  makes a partition that does not fit smaller, without an error, and its
  older and newer versions end a last partition without a size at
  different sectors. With every first sector and size written out, each
  version writes the same table and refuses one that does not fit. The
  last usable sector of a disk is its number of sectors minus 34, or minus
  6 with sectors of 4096 bytes.
- **`rest_of_disk` ends at the last whole MiB of the disk,** as newer
  versions of `sfdisk` end such a partition. When the module compares an
  existing table, a `rest_of_disk` partition may end anywhere in the last
  MiB before the last usable sector, so a table that an older `sfdisk`
  wrote is unchanged too.
- **Partitions are named, and names are plain.** The kernel's own names
  differ between kinds of disks (`sdb1`, `nvme0n1p1`) and can change
  between boots. A name of letters, digits, `-`, `_`, and `.` gives the
  link `/dev/disk/by-partlabel/<name>` exactly as written; the device
  manager writes other characters, such as a space, in another form. The
  names `.` and `..` are refused: they are no names of a link.
- **A name is unique on the node.** Two partitions with one name share one
  link, and which partition it leads to differs between distributions. A
  later node task would then work on the wrong partition.
- **The module waits for the device manager** (`udevadm settle`) **and
  checks each link,** after it wrote the table and also when it finds the
  table already there. On half of the test distributions the links were
  not there yet when `sfdisk` returned, and the next node task would have
  reported that its device does not exist. The check runs on an unchanged
  disk too: the run after one that wrote the table and found no link finds
  the table, and must not report success while a link is missing.
- **The module wipes nothing, also not inside a new partition.** An old
  filesystem that begins where a new partition begins is data that someone
  may still want. The module names it in the task's reason instead.
- **GPT only, and no removal or resize.** A partition table is written
  once in a disk's life, and every later change to it can destroy data.
