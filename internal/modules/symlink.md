# `symlink`

`symlink` manages one symbolic link on a node: the link's path and the text
of its target, or the link's removal. The `symlink` module replaces or removes
links, and `replace_existing_file: true` also permits replacing a regular file.
The `symlink` module never modifies the link's target. To manage a regular
file, use [`file`](file.md); to delete a directory, use
[`directory`](directory.md#delete-a-directory-or-what-it-no-longer-lists).

## Examples

### Create a link, and remove one

```yaml
- symlink:
    path: /etc/myapp/enabled.conf
    target: available.conf

- symlink:
    path: /etc/myapp/old-link.conf
    state: absent
```

On the node, the first task makes `/etc/myapp/enabled.conf` a link to
`available.conf`. A relative link target starts at the link's parent
directory, here `/etc/myapp`. A link may point to something that does not
exist yet. The second task removes the link `/etc/myapp/old-link.conf`.

### Replace a file that a package installed

A package sometimes installs a regular file where you want a link. Add
`replace_existing_file: true`, and the link replaces that file in one atomic
rename:

```yaml
- symlink:
    path: /etc/nginx/sites-enabled/default
    target: /etc/nginx/sites-available/myapp
    replace_existing_file: true
```

Dryrun reports that the file would be replaced. Without
`replace_existing_file: true`, a regular file at the link's path fails the
node task. MMX therefore never deletes a file nobody asked MMX to delete.
A directory at the path is always an error.

### Switch a release

A tracked `symlink` in a node task block switches a service to the release
directory that [`unpack_archive`](unpack_archive.md#unpack-a-release-and-switch-a-symlink)
or [`git`](git.md) put in place. The switch restarts the service, also after
an interrupted run.

## How it works

`path` is absolute. The target is stored literally, as written, and may
dangle. Creation and retargeting replace the link atomically; removal unlinks
only the link. `state: absent` accepts only `path` and `state`.

Within a node task block, link target changes contribute to the
[service configuration hash](../../docs/changing-mmx/design/execution.md#service-reloads-follow-configuration-hashes):
the record used to decide whether a service must load new configuration. Link
tracking does not follow targets or scan directory trees. Declare the target
files too if MMX manages their contents.

Dryrun shows link actions; [Preview file changes](../../docs/reference/shared-module-behavior.md#previewing-file-changes)
describes the previews.

## Tested on

**Distributions:** Debian 12 and 13, Ubuntu 24.04 and 26.04, Arch Linux, openSUSE Leap 16.1, Rocky Linux 9, AlmaLinux 10, Fedora 45.

The base suite covers this module (`file_resources.py`) and runs on every tested distribution.
