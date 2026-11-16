# `directory`

`directory` manages one directory on a node: it creates the directory or
changes its owner, group, and permissions. It can also set the owner, group,
and permissions of everything inside the directory, copy a directory from the
controller machine into it, delete it, or delete what it does not list.

[`file`](file.md) and [`symlink`](symlink.md) never delete directories;
`directory` does. A directory that an archive fills is
[`unpack_archive`](unpack_archive.md)'s, and a Git checkout is
[`git`](git.md)'s.

## Examples

### Create a directory

```yaml
- directory:
    path: /srv/app
    owner: app
    group: app
    permissions_mode: "0750"
```

On the node, MMX creates `/srv/app` when it is missing, or changes the owner,
group, and permissions of the directory that is there. The parent, `/srv`,
must exist. Entries inside `/srv/app` stay unchanged.

### Create a directory and its missing parents

```yaml
- directory:
    path: /srv/shop/shared/uploads
    create_missing_parent_directories: true
    owner: shop
    group: shop
    permissions_mode: "0750"
```

On a new node where `/srv` exists, MMX creates `/srv/shop` and
`/srv/shop/shared` as `mkdir -p` does, each owned by root with
permissions_mode `0755`, then `/srv/shop/shared/uploads` as the task says.
Dryrun creates nothing and lists the parent directories a run would create.
The run's result lists the parent directories MMX created. To give a parent
different ownership or permissions, add a separate `directory` task before
the task that creates `/srv/shop/shared/uploads`.

### Copy a directory from the controller machine

Use `directory.source` to copy the contents of a local directory onto a node:

```yaml
- directory:
    path: /etc/myapp/conf.d
    source: files/conf.d
    owner: root
    group: root
    permissions_mode: "0755"
    file_permissions_mode: "0644"
```

Copies include dotfiles and empty directories. Names and contents stay literal.
The destination's parent must exist. Each file uses the ordinary atomic
installer. The [directory-copy example](../../examples/directory-copy/README.md)
combines this with validation and a service reload.

Resume uses the saved directory tree and bytes. Rerunning finishes partial
work. Copied file contents contribute to service hashes; metadata-only changes
do not trigger a reload.

### Delete entries that the source no longer has

```yaml
- directory:
    path: /etc/myapp/conf.d
    source: files/myapp_conf.d
    also_delete_entries_not_in_source: true
```

On the node, MMX copies the source and deletes every entry under
`/etc/myapp/conf.d` that `files/myapp_conf.d` does not have,
at every depth. An extra directory is deleted with its whole tree. A link
is deleted as a link; its target stays untouched. The entries that the
source has are copied as usual, and a conflict of types, such as a
directory on the node where the source has a file, still fails the task.
An unchanged rerun reports unchanged.

`also_delete_entries_not_in_source: true` requires `source`.
A task with `also_delete_entries_not_in_source: true` cannot use
`if_source_directory_is_missing: copy_nothing`. A missing source directory
on the controller machine would then delete everything inside `path`.
The [deletion safeguards](#deleting) apply before anything is deleted.

Dryrun deletes nothing. The directory summary counts proposed deletions and
shows up to ten example changes in total across copying and deletion.
`value.deleted_entries` counts every deleted file, link, directory, and
other entry, including the contents of deleted directories. The summary
counts deleted links and special files as files. `value.deleted_names`
remains specific to
`delete_everything_inside_except`, where names mean direct children rather
than paths at several depths.

### Copy a directory that the project may not have

A missing source directory stops the playbook before MMX connects to any
node. Some projects have a source directory only when they need it, such as a
directory of the project's own check scripts that many projects leave out.
For projects with an optional source directory, set the task to copy nothing
when the source directory is missing:

```yaml
- directory:
    path: /etc/myapp/checks.d
    source: check_scripts
    if_source_directory_is_missing: copy_nothing
    permissions_mode: "0755"
```

Without `check_scripts/` in the project, the task only makes sure
`/etc/myapp/checks.d` exists, empty on a new node, and leaves anything already
in it alone. A new directory gets mode `0755` unless `permissions_mode` says
otherwise. Once the project has `check_scripts/`, the next run copies it as
usual. An application can use this for a directory of project-specific modules,
which a project may not have. Only a missing directory counts as empty: a file or a link in its place
still fails. The default, `fail`, refuses a missing source.

### Set ownership and permissions across a node's directory tree

```yaml
- directory:
    path: /srv/myapp
    also_apply_to_everything_inside: true
    owner: myapp
    group: myapp
    permissions_mode: "0750"
    file_permissions_mode: "0640"
```

`permissions_mode` applies to directories. `file_permissions_mode` applies to
regular files and requires either `also_apply_to_everything_inside: true` or
`source`. Omitted fields preserve each entry's current values. Without
`source` or `also_apply_to_everything_inside: true`, MMX manages only the
named directory.

### A directory owned by a numeric ID

`owner` and `group` take an account name, or a number. A number is the
numeric ID itself and needs no account on the node. Use numeric `owner` and
`group` values for a directory owned by a container process whose IDs have no
accounts on the node:

```yaml
- directory:
    path: /srv/postgres-data
    owner: 999             # the ID of the postgres account inside the container
    group: 999
    permissions_mode: "0700"
```

Write the number without quotes. `owner: "999"` is refused, so that text
always means a name and a number always means an ID. Prefer a name where the
node has an account: a bare number may be the ID of another account on this
node, now or later. To keep the number and still have a name, create the
account with that ID and use its name:

```yaml
- user:
    create_home_directory: false
    name: postgres-container
    uid: 999
    system: true

- directory:
    path: /srv/postgres-data
    owner: postgres-container
```

The same rule holds for `file`, `download`, and `unpack_archive`;
[Owner and group are an account name or a number](../../docs/changing-mmx/design/modules.md#owner-and-group-are-an-account-name-or-a-number)
owns it.

### Delete a directory, or what it no longer lists

Removing a task from YAML never deletes what the task created. To delete a
directory, say so with `state: absent`:

```yaml
# Delete an old installation and everything in it.
- directory:
    path: /home/deploy/old-api
    state: absent
    also_delete_everything_inside: true
```

Without `also_delete_everything_inside: true`, `state: absent` deletes only an
empty directory and fails on one that has entries, naming how many.

To keep a directory and delete what does not belong in it, list the names that
belong directly inside:

```yaml
# Keep the current and the previous release and delete every other entry
# of /srv/myapp/releases. release and previous_release are variables you define.
- directory:
    path: /srv/myapp/releases
    owner: myapp
    group: myapp
    permissions_mode: "0750"
    delete_everything_inside_except:
      - "{{ .vars.release }}"
      - "{{ .vars.previous_release }}"
```

On the node, MMX deletes every other entry directly inside
`/srv/myapp/releases`, each with everything in it, and gives the directory
itself the owner, group, and permissions that the task names. `[]` empties
the directory. A name in the list that does not exist is not created.

When `/srv/myapp/releases` is missing, MMX creates it, as every `directory`
task creates a missing directory: owned by `myapp`, group `myapp`, with
permissions `0750`, and empty, so there is nothing to delete. The result is
`changed`, and a later node task can write into the directory. The parent,
`/srv/myapp`, must exist, unless `create_missing_parent_directories` is
`true`.

Dryrun reports the result value `deleted_entries`, the number of files,
links, and directories that would be deleted, counting each directory; with
`delete_everything_inside_except` it also lists `deleted_names`, the names of
the entries it would delete directly inside the directory. A run reports the
same values for what it deleted.

## How it works

### One directory

Without `source` or `also_apply_to_everything_inside`, `directory` creates one
directory or changes its explicit owner, group, and mode. The parent must
exist, unless `create_missing_parent_directories` is `true`. A new directory gets mode `0755` unless `permissions_mode` says
otherwise; omitted metadata on an existing directory stays unchanged. When a
mode is omitted, MMX preserves executable and special permission bits as well
as ordinary permissions. Children are preserved, and a symlink at the path is
refused. During dryrun, ownership by an account that does not exist yet is
reported as not checked.

### Copying a directory

The source is a path on the controller machine, resolved like
[`file.source`](file.md#paths-on-the-controller-machine). Its entries
(directories, including empty ones, and regular files, including dotfiles)
are saved with the run; resume uses that snapshot. Contents are not rendered.
Source symlinks, source special files, and special permission bits are
rejected before MMX sends the work to any node. Conflicting destination types
fail without being replaced, and destination links are not followed. The
root is created if missing; its parent must exist, unless
`create_missing_parent_directories` is `true`.

By default, the source owns only its listed paths. Entries not listed in the
source stay untouched on the node, including files removed from a later
source tree. With `also_delete_entries_not_in_source: true`, MMX deletes
those entries at every depth. The list of source entries that MMX reads when
the run starts decides what is kept, also when `mmx resume` finishes the
run. There is no include or exclude.

`owner` and `group` apply to the root and the copied entries. Omitted
metadata preserves existing entries. New entries use the source's modes and
normal ownership defaults. Explicit ownership and directory or file modes
apply only to copied paths. Do not combine `source` with
`also_apply_to_everything_inside: true`.

Each file uses the ordinary atomic installer; the tree is not a transaction,
and a rerun finishes partial work. Dryrun creates nothing. Dryrun and results
show one `directory_change` summary with up to ten example file or directory
changes. `--diff` shows bounded text previews for them, with the same
private-file and secret suppression as individual `file` node tasks; see
[Preview file changes](../../docs/reference/shared-module-behavior.md#previewing-file-changes).

### Everything inside a directory

With `also_apply_to_everything_inside: true`, `owner` and `group` apply to the
root, descendant directories, and regular files. A missing root directory
without a source is created with default mode `0755`; its parent must exist,
unless `create_missing_parent_directories` is `true`.
The named root itself cannot be a symlink. Traversal skips symlinks, special
files, and entries on another filesystem device, and never follows links.

The walk is one local operation on the node, not one task per entry. It
changes only differing metadata and sets directory metadata after visiting
children. It returns one summary: entries checked, changed, and skipped, with
up to ten examples. Dryrun inspects without changing anything and reports
counts. `--diff` adds up to ten example metadata changes. Saved JSON includes
`directory_change`; `mmx results --diff` shows the same summary. Partial
failures retain the number of updates made. Another run reconciles the
current metadata.

### Missing parent directories

With `create_missing_parent_directories: true`, MMX creates each missing
parent of `path` in order, as `mkdir -p` does. Each one is owned by root
with permissions_mode `0755`, also under a directory whose set-group-ID bit
would pass its group on. Links in the part of the path that exists are
followed, as `mkdir -p` follows them. Each new parent is created and opened
inside the one before it, and stays private until it has its owner and
permissions. The result's `directory_change.created_parent_directories`
lists the parents MMX created; in a dryrun it lists the parents a run would
create, and nothing is created. A parent that exists is never changed.

The field does not go with `state: absent`: a directory that MMX deletes
needs no parents. With `delete_everything_inside_except`, it creates the
missing parents of a missing directory, as for any other `directory` task.

### Deleting

Before it deletes anything, MMX checks the safeguards:

- The path is absolute and at least two levels deep, such as `/srv/myapp`;
  `/srv` or `/` is refused.
- The path is not a symbolic link. A link inside what MMX deletes is deleted
  as a link, never what it points to.
- Nothing that would be deleted is a mount point, nor has one anywhere inside
  it. A mounted filesystem or a bind mount there fails the node task before
  anything is deleted. Unmount it first.

The path itself may be a mount point with `delete_everything_inside_except`
or `also_delete_entries_not_in_source`:
the entries it deletes then all live on that filesystem. A `state: absent`
task refuses a path that is a mount point.

MMX first renames a tree it deletes to a staging name beside it,
`.mmx-staging-<name>`, then deletes the staging tree. If the deletion stops
part of the way, for example on a file the kernel refuses to delete, only the
staging directory is left, and the next run of the task deletes it. With
`delete_everything_inside_except`, that staging directory is inside the
directory and not listed, so the next run deletes it too. The same applies
to `also_delete_entries_not_in_source` at every depth. If a source entry owns
the staging name needed for a deletion, MMX refuses before deleting anything;
rename that source entry first so staging cannot erase a source-owned path.

`state: absent` and `also_delete_everything_inside` take no other field.
`delete_everything_inside_except` also accepts `owner`, `group`, and
`permissions_mode` for the directory itself, and
`create_missing_parent_directories`. Entries that the task keeps retain
their metadata.

A `delete_everything_inside_except` task creates a missing directory exactly
as a `directory` task without the list does, with the task's `owner`,
`group`, and `permissions_mode`; without `permissions_mode` the new directory
gets mode `0755`. The new directory is empty, so the task deletes nothing,
and the names in the list are not created. A dryrun reports the creation as
it does for any new directory, and creates nothing. A link or a file at the
path fails the node task; MMX replaces neither.

### In a node task block

Copied file contents contribute to the
[service configuration hash](../../docs/changing-mmx/design/execution.md#service-reloads-follow-configuration-hashes);
directories and metadata do not, so permission changes alone do not trigger
service reloads.

In a node task block's `changes`, a deletion is a tracked input: the existence
of the directory for `state: absent`, and the names directly inside it for
`delete_everything_inside_except`. A deletion alone therefore restarts or
reloads the block's service, also after a run that stopped between the
deletion and the restart.

When `delete_everything_inside_except` creates a missing directory, that
tracked input changes once, from a missing directory to an empty one. A
running service of the block is then restarted or reloaded once, and a
dryrun reports that it would be. The next run finds the same directory with
the same names and refreshes nothing.

## Errors and what they mean

- **`also_delete_entries_not_in_source` without `source`** fails validation.
  Name the source directory whose entries MMX must keep.
- **`also_delete_entries_not_in_source` with `copy_nothing`** fails validation:
  a missing source directory would delete everything inside `path`. Keep
  `if_source_directory_is_missing: fail` for a copy that deletes extra entries.
- **A missing source directory** stops the playbook before MMX connects to any
  node. Correct the path, or set `if_source_directory_is_missing:
  copy_nothing` when the project may leave the directory out.
- **A symlink or a special file in the source**, or a destination entry of
  another type than the source's, fails without replacing anything. Change
  the source, or remove the entry at the destination first.
- **`state: absent` on a directory that has entries** fails and names how
  many. Add `also_delete_everything_inside: true` when everything in it may
  go.
- **A mount point** in what MMX would delete fails the node task before
  anything is deleted. Unmount it first.
- **`the parent directory /var/backups does not exist; create it with an
  earlier directory node task`**: the directory that would hold the new one
  is missing. The node task fails once and is not tried again. Create the
  parent with an earlier `directory` node task, or set
  `create_missing_parent_directories: true` on this one. A dryrun reports
  the node task not checked, or previews it when an earlier node task of the
  dryrun would create the parent.

## Tested on

**Distributions:** Debian 12 and 13, Ubuntu 24.04 and 26.04, Arch Linux, openSUSE Leap 16.1, Rocky Linux 9, AlmaLinux 10, Fedora 45.

The base suite covers this module (`directory_resources.py`, `directory_copy.py`, `account_units.py`) and runs on every tested distribution.

## Why it works this way

- **Missing parents are created only when the task says so.** A mistyped
  path, such as `/srv/shpo/uploads`, should fail and name the missing
  `/srv/shpo`, not quietly create a tree beside the real one. Ansible's
  `file` creates parents always, and real playbooks rely on it, so the field
  is one line where a playbook needs it.
- **Created parents are root's, with `0755`.** A parent that MMX creates on
  the way is not the task's directory, so the task's owner and permissions
  do not apply to it. Root with `0755` is what `mkdir -p` gives as root
  with the usual umask, and every account can pass through it to the
  directory below.

- **A missing source directory stops the playbook.** A mistyped path should
  not quietly copy nothing.
- **Removing a task never deletes; a deletion is written out.** A directory
  full of data is never deleted by a line that does not say so.
- **`delete_everything_inside_except` lists the whole set.** A reviewer sees
  on the page what the task deletes. The
  [rule on resource ownership](../../docs/changing-mmx/design/modules.md#resource-ownership-must-be-obvious)
  owns this principle.
- **MMX refuses to delete through a mount point.** Deleting through it would
  delete files of another filesystem.
- **MMX renames a tree before it deletes it.** The path is gone at once, so
  nothing reads a half-deleted directory there, and the next run finishes an
  interrupted deletion.

- **Cleanup may also manage the directory's metadata.** Its owner, group,
  and mode describe the same resource; a separate task would repeat the path.
  Kept entries remain outside that metadata change.
- **`delete_everything_inside_except` creates a missing directory.** A
  `directory` task has `state: present` by default, which says that the
  directory exists. The list adds what it holds. One task can then state the
  directory's owner, group, permissions, and its whole contents, and the next
  node task that writes into the directory finds it, on a new node as on an
  old one.
