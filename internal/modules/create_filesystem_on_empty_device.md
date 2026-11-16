# `create_filesystem_on_empty_device`

`create_filesystem_on_empty_device` puts a filesystem on a disk or partition
of the node that holds nothing yet, such as a new volume attached to a cloud
server. The module runs `mkfs.<filesystem_type>`, such as `mkfs.ext4`.
The module's one job is to never destroy data. The module writes only on a
device that holds no signature of any kind.
The module never passes a force option and has no setting that overrides
the signature check.

Partitions and LVM volumes are official extra modules, such as
[`create_partitions_on_empty_disk`](../../official_extra_modules/create_partitions_on_empty_disk/README.md).
A mount of the new filesystem is a [`fstab_mount`](fstab_mount.md) node
task.

## Examples

### A filesystem on a new disk

```yaml
- create_filesystem_on_empty_device:
    device: /dev/sdb
    filesystem_type: ext4
    mkfs_options: [-L, data]
```

MMX probes the device with `wipefs --no-act`, which lists every signature it
finds and changes nothing:

- No signature of any kind: MMX runs `mkfs.ext4 -L data /dev/sdb`, the
  program named by `filesystem_type`, with `mkfs_options` before the device.
- Only an ext4 signature, the requested type: the node task is unchanged.
- Anything else, such as another filesystem, a swap area, a partition
  table, or an LVM physical volume: the node task fails and names what it
  found, such as `found: xfs filesystem (xfs signature at offset 0x0)`.

Dryrun reports `would create ext4 on /dev/sdb`, or unchanged.

### A filesystem, then its mount

```yaml
- create_filesystem_on_empty_device:
    device: /dev/sdb1
    filesystem_type: xfs

- directory:
    path: /srv/data

- fstab_mount:
    check_filesystem_at_boot: never
    options: [defaults]
    mount_point: /srv/data
    mount_source: /dev/sdb1
    filesystem_type: xfs
    state: mounted
```

The first node task makes an xfs filesystem on the empty partition
`/dev/sdb1`. The `directory` node task creates the mount point `/srv/data`.
The `fstab_mount` node task mounts the filesystem at `/srv/data`, now and
at every boot.

## How it works

- MMX never passes a force option, and refuses one in `mkfs_options`. The
  module has no setting that skips the signature check, no `state: absent`,
  and no resize.
- To reuse a device that held something, first make sure nothing on it is
  needed, then wipe it by hand, such as with `wipefs --all /dev/sdb`.
  Wiping the device by hand is deliberate and stays outside MMX.
- The module also fails when a device without any signature is in use:
  mounted, or held by device mapper or RAID. The kernel reports that when
  MMX asks to open the device alone.
- After an interrupted format, the next run probes the device again. With
  no signature, MMX creates the filesystem. With only signatures of the
  requested type, MMX reports unchanged. With any other signature, MMX fails
  and names the signature.
- A device that does not exist yet is not checked in a dryrun, because an
  earlier node task may create it.
- The programs come with the node's base system: `wipefs` from
  `util-linux`, `mkfs.ext4` from `e2fsprogs`, and `mkfs.xfs` from
  `xfsprogs`. If `wipefs` or the required `mkfs.<filesystem_type>` program
  is missing, the node task fails and names the package to install.
  A dryrun reports the node task not checked.

## Errors and what they mean

| The reason holds | What the error means, and what to do |
|---|---|
| `/dev/sdb is not empty; found: ... To use this device, first check that nothing on it is needed, then wipe it by hand, such as with wipefs --all /dev/sdb` | The device holds another filesystem, a swap area, a partition table, an LVM physical volume, or another signature. The module never writes on such a device. When nothing on `/dev/sdb` is needed, wipe `/dev/sdb` by hand. |
| `/dev/sdb has no device signature but is in use as a mounted filesystem, an active swap area, or part of another device such as LVM or RAID; no changes were made` | Find what uses the device, such as with `lsblk /dev/sdb` and `findmnt`. |
| `the device /dev/sdb does not exist yet` | A dryrun reports the node task not checked, because an earlier node task may create the device. Check the path on the node with `lsblk`. |
| `/dev/sdb is not a block device` | `device` names a file or a directory. Write the path of the device. |
| `mkfs.xfs is not installed; install the package xfsprogs first` | Install the package in an earlier node task. A dryrun reports the node task not checked. |

## Tested on

**Distributions:** Debian 12 and 13, Ubuntu 24.04 and 26.04, Rocky Linux 9, AlmaLinux 10.

The integration suites `tests/integration/storage.py` and
`tests/integration/partitions_and_lvm.py` run in virtual machines on Debian 12 and 13, Ubuntu 24.04 and 26.04, Rocky Linux 9, and AlmaLinux 10.
Each suite uses loop devices: block devices backed by files inside the
virtual machine. The suites use virtual machines because filesystems need
the kernel of the guest itself.
The [validation record](../../docs/changing-mmx/validation.md) has the latest results.

No integration suite tests devices with LVM, RAID, or LUKS signatures,
or devices without signatures that are in use, on a real node.
Go tests cover devices with LVM, RAID, or LUKS signatures and devices
without signatures that are in use.

## Why it works this way

- **No force setting.** The module's only purpose is that a run can never
  destroy data.
