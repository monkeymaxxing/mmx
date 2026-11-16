# `unpack_archive`

`unpack_archive` extracts an archive into a directory of its own on a node,
such as one release of an application. It takes the archive from a pinned
`url` or a local `source` on the controller machine. MMX recognizes the
format from the file's contents, not its name: a tar, plain (`.tar`) or
compressed with gzip (`.tar.gz`, `.tgz`), xz (`.tar.xz`), bzip2 (`.tar.bz2`),
or zstd (`.tar.zst`), or a zip. Anything else fails.

For a single file from a web address, use [`download`](download.md). For a
release that lives in a Git repository, use [`git`](git.md).

## Examples

### Unpack a release and switch a symlink

Deploy each release into its own directory on the node. Point a tracked
[`symlink`](symlink.md) at the chosen release. The switch restarts the
service, including after an interrupted run:

```yaml
- task_block:
    id: app                    # a node task block ID you choose
    changes:
      - unpack_archive:
          url: https://releases.example.com/app-1.4.2-linux-amd64.tar.gz
          # Replace with the SHA-256 of the release archive you have verified.
          sha256: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
          path: /srv/app/releases/1.4.2
          owner: app
          group: app
          unpack_without_wrapping_folder: true
      - symlink:
          path: /srv/app/current
          target: releases/1.4.2
    activate:
      - systemd_service:
          name: app

          state: running
- unpack_archive:
    also_delete_everything_inside: true
    path: /srv/app/releases/1.4.1
    state: absent
```

The controller process fetches the URL once per run and checks it against the
SHA-256, as for `download`. An archive that an earlier run of the project
saved is reused after its SHA-256 is checked again, so an unchanged run does
not fetch it again. On the node, MMX extracts into a staging
directory beside `path`, then renames it into place.
`unpack_without_wrapping_folder: true` leaves out the one top-level folder
that many release archives have, such as `app-1.4.2/`, so its contents land
directly in `path`. The `symlink` task then points `/srv/app/current` at the
new release, and the switch restarts the service `app`.

Old releases stay until a task deletes them. The example's last
`unpack_archive` task, with `state: absent`, deletes one release it
extracted; to keep only the releases you name, use
[`delete_everything_inside_except`](directory.md#delete-a-directory-or-what-it-no-longer-lists)
on the releases directory.

## How it works

### The archive

`unpack_archive` takes exactly one of `source`, a local file resolved like
[`file.source`](file.md#paths-on-the-controller-machine), or `url` with
`sha256`, fetched and verified like [`download`](download.md) under the same
[checksum rule](../../docs/changing-mmx/design/modules.md#a-file-from-a-web-address-is-checked-against-its-sha-256).
Dryrun fetches and verifies a URL on the controller machine like `download`.
A node keeps an archive from a URL as `download` keeps its file, so a later
run or dryrun does not send it again; an archive from `source` is sent each
time. Resume reuses the saved bytes.

### Extraction on the node

When `path` is missing, the node extracts into a private staging directory
beside it (`.mmx-staging-<name>`), then renames the staging directory into
place. The parent of `path` must exist. Archives are untrusted input, so
every entry is checked before it is created. Extraction fails, in both dryrun
and run, discarding the staging directory and leaving nothing at `path`, on:

- an entry with an absolute path or a `..` component;
- a symlink whose target is absolute or leaves the directory: `..` may appear
  only as leading components, no more of them than the link's depth;
- an entry at or below a symlink, or a name that appears twice (a directory
  listed again is fine);
- a hard link that does not name an earlier regular file of the archive;
- a device, FIFO, or other special entry, or a top-level `.mmx-archive`.

With `unpack_without_wrapping_folder: true`, the archive must have exactly
one top-level folder. Any file or other entry outside the archive's single
top-level folder fails the node task before extraction. Leave the setting
out to preserve the archive's whole layout.

Archive entries use the declared `owner` and `group` (default root), never
the archive's numeric IDs; `owner` and `group` take an account name or a
number, as
[the rule for owner and group](../../docs/changing-mmx/design/modules.md#owner-and-group-are-an-account-name-or-a-number)
says. Entries keep the archive's permission bits except setuid, setgid, and
sticky bits. Zip entries without Unix modes get `0644` or `0755`, as do
directories the archive does not list. The directory itself gets `owner`,
`group`, and `0755`. An interrupted extraction leaves only its staging
directory, which the next attempt replaces.

A small compressed archive can hold huge files. Before it writes a file, MMX
adds up the sizes the archive's files declare, and the node task fails, in
dryrun too, when they would leave less than 5 % of the node's filesystem free,
as the Avail column of `df` counts it. The staging directory is then deleted,
and nothing is left at `path`.

Dryrun reads the archive on the node and reports the entry count and any
rejected entry, creating nothing. The result value's `entries` is the number
of extracted entries.

### The record of what was extracted

A root-owned `.mmx-archive` file in the extracted directory records the
archive's SHA-256, `unpack_without_wrapping_folder`, `owner`, and `group`. If
the record matches the requested archive's SHA-256, wrapping-folder setting,
owner, and group, MMX reports `unchanged` without rescanning the extracted
directory's contents. Any other directory or file at `path`, including one
extracted from a different archive or with different settings, fails the
node task: give each version its own path.

`state: absent` requires `also_delete_everything_inside: true` and accepts
only those two settings and `path`. It removes a directory holding a readable
`.mmx-archive` record, with its whole tree, without following links: it
renames the directory to its staging name, then deletes that. A directory
without a readable record fails. When the path is missing, a staging
directory that an earlier removal or extraction left behind is deleted, and
the removal is changed; with neither, it is unchanged.

### In a node task block

`unpack_archive` adds nothing to service hashes; the files of the tree are
not hashed. With release directories, the tracked `symlink` records the
release switch, so the switch feeds the
[service configuration hash](../../docs/changing-mmx/design/execution.md#service-reloads-follow-configuration-hashes)
and an interrupted deployment still restarts.

## Tested on

**Distributions:** Debian 12 and 13, Ubuntu 24.04 and 26.04, Arch Linux, openSUSE Leap 16.1, Rocky Linux 9, AlmaLinux 10, Fedora 45.

The base suite covers this module (`release_directories.py`: it unpacks archives into release directories and switches them by a tracked symlink) and runs on every tested distribution.

## Why it works this way

- **Each release goes to its own path, and a tracked `symlink` switches to
  it.** The switch feeds the service hash, so an interrupted deployment
  still restarts.
- **Every entry is checked before it is created.** Archives are untrusted
  input.
- **The declared sizes must leave 5 % of the node's filesystem free.** A
  small compressed archive cannot fill the disk.

- **Unwrapping never silently drops archive entries.** A release can depend
  on any entry; an unexpected layout fails instead of producing an incomplete
  deployment.
- **Removal explicitly names the whole tree.** `also_delete_everything_inside`
  makes deletion visible when reviewing a task, under
  [protective choices](../../docs/changing-mmx/design/modules.md#protective-choices-come-in-three-levels).
