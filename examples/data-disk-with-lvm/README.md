# A data disk with LVM

A server gets a new, empty disk for its data, such as a second volume of a
cloud server. This example turns the disk into a volume group of LVM, makes
one logical volume in it, puts an ext4 filesystem on the volume, and mounts
it. Later, the volume grows with one changed line.

Set the server address in `inventories/example.yml`. Find the new disk's
path on the server with `lsblk`, and run:

```sh
mmx dryrun configure --var data_disk=/dev/sdb
mmx run configure --var data_disk=/dev/sdb
mmx run configure --var data_disk=/dev/sdb
```

On the node, in order:

1. MMX installs the packages `lvm2` and `e2fsprogs`, and on Debian and
   Ubuntu `fdisk`.
2. MMX writes a partition table on the disk, with one partition named
   `data-lvm` that takes the whole disk.
3. MMX creates the volume group `data` on that partition.
4. MMX creates the logical volume `postgresql` with 50 GiB in the volume
   group.
5. MMX creates an ext4 filesystem on the volume `/dev/data/postgresql`.
6. MMX creates the directory `/var/lib/postgresql`, mounts the filesystem
   there, and adds its line to `/etc/fstab`, so the mount is back after a
   reboot.

The second run changes nothing.

**The first dryrun** reports the partition table that a run would write. The
first dryrun reports tasks after the partition task as `not_checked`. The
partition, volume group, and volume do not exist yet for MMX to inspect.

**The disk must be empty.** The partition task writes only on a disk that
holds no partition table, no filesystem, and no other signature. On any
other disk it fails and names what it found, and nothing is changed. The
same holds for the filesystem task and the volume. No task of this example
can destroy data.

**The disk is named on the command line,** because the kernel can give a
disk another name on another server. Everything after the partition task
uses the partition's name, `/dev/disk/by-partlabel/data-lvm`, which stays
the same.

**To grow the volume,** write a larger `data_logical_volume_size` in
`inventories/example.yml`, such as `80GiB`, and run the playbook again.
LVM grows the volume and its filesystem together, while the filesystem
stays mounted. The module never shrinks a volume: a smaller size fails the
task.

**When the disk is full,** attach another disk and add it to the volume
group: list it in `physical_volumes` of the `lvm_volume_group` task.

The names of the volume group and the volume, the volume's size, and the
mount point are in `inventories/example.yml`.

## Distributions

The example works on Debian 12 and 13, Ubuntu 24.04 and 26.04, Rocky Linux 9,
and AlmaLinux 10. What differs between them is in the playbook: `sfdisk` is
in the package `fdisk` on Debian and Ubuntu.

For every input, see the pages of the three official extra modules:
[`create_partitions_on_empty_disk`](../../official_extra_modules/create_partitions_on_empty_disk/README.md),
[`lvm_volume_group`](../../official_extra_modules/lvm_volume_group/README.md),
and [`lvm_logical_volume`](../../official_extra_modules/lvm_logical_volume/README.md).
The module pages describe
[filesystems](../../docs/reference/modules/create_filesystem_on_empty_device.md) and
[mounts](../../docs/reference/modules/fstab_mount.md).
