# `swap_file`

A swap file gives a node extra memory on disk. `swap_file` makes the file,
switches it on with `swapon`, and adds its line to `/etc/fstab`, so it is on
again after a reboot.

## Examples

### A swap file of 4 GiB

```yaml
- swap_file:
    path: /swapfile
    size: 4GiB
```

`size` is a whole number above zero followed by `MiB`, `GiB`, or `TiB`.
`4GiB` means 4 GiB. On the node, MMX checks
each step and does only the ones that are missing:

1. The file exists with that size, and every byte of it is written. The
   kernel refuses a swap file with holes, which a file made with `truncate`
   has.
2. It has mode `0600` and belongs to root, because swapped-out memory can
   hold passwords.
3. It has a swap signature, which `mkswap` writes.
4. It is switched on (`swapon`).
5. `/etc/fstab` has its line, `/swapfile none swap defaults 0 0`.

Because every step is checked, a run that was cut off in the middle is
finished by the next run. Dryrun lists the missing steps in the node task's
reason, such as `would switch on /swapfile, then add its line to
/etc/fstab`, and shows the difference of `/etc/fstab`.

### Remove the swap file

```yaml
- swap_file:
    path: /swapfile
    state: absent
```

`state: absent` switches the file off, removes its line from `/etc/fstab`,
and deletes it.

## How it works

- A swap file of another size is switched off, deleted, written again at the
  new size, and switched on.
- MMX writes a new file under a temporary name beside the path, such as
  `/.swapfile.mmx-new`, gives it its swap signature, and only then renames
  the new swap file to `path`. The next run deletes a temporary file that
  an interrupted run left.
- Before writing, MMX checks that the filesystem keeps 5% free afterwards.
- On btrfs, MMX turns copy-on-write off for the new file, which btrfs
  requires of a swap file.
- Where SELinux is on, `mkswap` gives the file the type `swapfile_t`, which
  systemd needs to switch it on at boot.
- An existing line for the file in `/etc/fstab` with type `swap` is kept as
  written, such as Ubuntu's `/swapfile none swap sw 0 0`.
- The programs come with the node's base system: `mkswap` and `findmnt`
  from `util-linux`, and `swapon` and `swapoff` from `mount` on Debian and
  Ubuntu and `util-linux` elsewhere.

## Errors and what they mean

| The reason holds | What the reason means, and what to do |
|---|---|
| `/swapfile has no swap signature and may hold data; ...` | A file at `path` without a swap signature was never MMX's. The file at `path` may hold data, so the node task fails rather than replace or delete the file at `path`. Move the file at `path` away by hand, or choose another path. |
| `swap file /swapfile needs ... MiB, but the filesystem has only ... MiB available after reserving 5% of the filesystem as free space` | The filesystem has too little room. Free space, or make the swap file smaller. |
| `the directory ... does not exist yet` | The directory containing `path` is missing. A dryrun reports the node task as `not_checked`, because an earlier node task may create the directory containing `path`. |
| `mkswap is not installed; install the package ... first` | A program is missing; the reason names its package. A dryrun reports the node task not checked. |

## Tested on

**Distributions:** Debian 12 and 13, Ubuntu 24.04 and 26.04, Rocky Linux 9, AlmaLinux 10.

The suite `tests/integration/storage.py` runs in a virtual machine on Debian 12 and 13, Ubuntu 24.04 and 26.04, Rocky Linux 9, and AlmaLinux 10, because swap needs the kernel of the guest itself.
The [validation record](../../docs/changing-mmx/validation.md) has the latest results.

## Why it works this way

- **A new file gets its swap signature before it gets its name.** A file at
  `path` without a swap signature therefore was never MMX's, and the module
  never replaces or deletes such a file.
- **Sizes use binary units.** `MiB`, `GiB`, and `TiB` match partition and LVM
  tasks, so the same size has one spelling across storage tasks.
- **Every byte is written.** The kernel refuses a swap file with holes.
