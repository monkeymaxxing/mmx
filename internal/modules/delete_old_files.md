# `delete_old_files`

`delete_old_files` deletes regular files from a selected directory on the
node. A file must match a name pattern and have a modification time older
than the chosen duration. For example, use the module for an application's
rotated logs when nothing else cleans up the logs.

`delete_old_files` never follows a symlink and never deletes on another
filesystem below the selected directory. `delete_old_files` deletes files
only. The directories stay.

To delete a whole directory, or everything in it that a list does not name,
use [`directory`](directory.md) with `state: absent` or
`delete_everything_inside_except`. For continual control of a log's size, a
rotation rule (a [`file`](file.md) task in `/etc/logrotate.d/`) is better.
`delete_old_files` is for what such a rule does not cover, or for one
cleanup.

## Examples

### Delete an application's rotated logs older than two weeks

```yaml
- delete_old_files:
    directory: /var/log/myapp
    name_patterns: ["*.gz", "*.[0-9]"]
    older_than: 14d
    include_subdirectories: false
```

On the node, MMX reads `/var/log/myapp` and deletes each regular file
directly in it whose name matches `*.gz` or `*.[0-9]`, such as
`myapp.log.3.gz` or `myapp.log.1`, and whose modification time is more than
14 days ago. `myapp.log` itself stays: its name matches no pattern. The node
task reports what it deleted:

```text
changed — deleted 23 files totaling 1.4 GiB (1503238553 bytes) in /var/log/myapp
```

`value.deleted_files` and `value.deleted_bytes` hold the numbers, and
`value.deleted_file_examples` names up to ten of the files. A dryrun deletes
nothing and reports the same for the files a run would delete. When no file
matches, the node task is `unchanged`.

### Also in the directories below

```yaml
- delete_old_files:
    directory: /srv/reports/exports
    name_patterns: ["*.csv"]
    older_than: 30d
    include_subdirectories: true
```

With `include_subdirectories: true`, MMX also walks every directory below
`/srv/reports/exports` and deletes the old `.csv` files there. MMX walks
neither a symlink to a directory nor a mount point, such as a disk mounted
at `/srv/reports/exports/archive`.

## How it works

- **Which files.** A file goes when it is a regular file, one of
  `name_patterns` matches its name, and its modification time is longer ago
  than `older_than`. A pattern is matched on the file name alone, as
  `find -name` matches it: `*` and `?` also match a leading dot, `[0-9]`
  matches one digit, and `[!0-9]` or `[^0-9]` one character that is not a
  digit.
- **No link on the directory's path.** MMX opens the directory one
  component after the other from `/` and follows no symlink: when
  `directory`, or any directory on its path, is a symlink, such as
  `/var/log/myapp` pointing to `/var/lib/myapp` or `/var/log` itself a link,
  the node task fails, names the component that is a link, and deletes
  nothing. MMX deletes only below the directory as written.
- **Symlinks inside.** A symlink below the directory is never followed and
  never deleted, also when its name matches: a symlink to a directory is not
  walked, and a symlink to a file stays with its target. The reason counts
  the symlinks whose names match.
- **Other filesystems.** MMX walks only directories on the directory's own
  filesystem and that are no mount point, also not a bind mount of the same
  filesystem. The directory itself may be a mount point: what is in it is
  on its filesystem. The node task result's `reason` text reports how many
  mount points MMX left alone.
- **Depth.** The directory is at least two levels deep, such as
  `/var/log`; `/` and a top-level directory such as `/var` are refused, as
  for every directory from which MMX deletes
  ([`directory`](directory.md#deleting)).
- **Each file.** MMX opens each directory relative to the one above, with
  no link followed, and inspects each file again just before it deletes the
  name, so that a file that took the name since is not deleted.
- **A failure** stops the node task. Its result still counts the files
  deleted before.

## Errors and what they mean

| The reason holds | What the error means, and what to do |
|---|---|
| `/var/log/myapp is a symlink; delete_old_files deletes only below the directory as written and follows no link on its path` | `directory`, or the directory on its path that the reason names, is a symlink. Name the directory that the link points to. MMX does not automatically retry the node task. |
| `a directory that MMX deletes from or deletes must be an absolute path at least two levels deep` | Validation refuses `/` and a top-level directory, such as `/var`. |
| `delete_old_files.include_subdirectories is required` | Every node task says whether it also deletes in the directories below. |
| `path changed during inspection: ...` | A file or directory was replaced while MMX worked; run again. |

## Tested on

**Distributions:** Debian 12 and 13, Ubuntu 24.04 and 26.04.

The suite `tests/integration/routine_fixes.py` ages files with `touch`,
adds a symlink to a file outside, a symlink to a directory, a tmpfs and a
bind mount below the directory, and names a directory that is a link and
one with a link on its path, and checks the dryrun, the run, and the files
left on the node, on Debian 12 and 13 and Ubuntu 24.04 and 26.04. The [validation record](../../docs/changing-mmx/validation.md)
has the latest results.

## Why it works this way

- **`include_subdirectories` has no default.** Deleting in subdirectories
  changes the cleanup's scope, so you must set `include_subdirectories`
  explicitly in each `delete_old_files` node task's YAML.
- **No symlink is followed and no other filesystem is entered.** A link or
  a mount inside a log directory can lead anywhere, such as to a data disk;
  a cleanup deletes only what lies in the directory itself.
- **No link on the path either.** A reader, a person or a program that
  decides which cleanup may run, sees the directory as written. A symlink
  on the cleanup directory's path would make the node task delete somewhere
  else. For example, `/var/log/postgresql` could point to the database's own
  directory.
- **The modification time decides.** A log file's modification time is when
  it was last written; the access time changes on every read, and the
  change time with every new owner or mode.
- **Files only.** An empty directory can be one that an application needs;
  [`directory`](directory.md) deletes directories by name.
