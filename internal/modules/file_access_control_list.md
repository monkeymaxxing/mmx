# `file_access_control_list`

An access control list gives named accounts and groups access to a file or
directory beyond its owner, group, and mode. `file_access_control_list`
manages the named account and group entries of one file's or directory's
list, through `getfacl` and `setfacl` on the node. Use [`file`](file.md) or
[`directory`](directory.md) to set the owner, group, and mode of the file or
directory.

## Set up

`getfacl` and `setfacl` are in the package `acl`. Install `acl` in an earlier
node task when the node lacks `acl`:

```yaml
- when: {equals: ["{{ .node.package_manager }}", apt]}
  apt_package: {packages: [{name: acl, version: newest_when_first_installed}]}
- when: {equals: ["{{ .node.package_manager }}", dnf]}
  dnf_package: {packages: [{name: acl, version: newest_when_first_installed}]}
```

## Examples

### Let a log shipper read an application's logs

A log shipper that runs as its own account can read an application's logs
without joining the application's group:

```yaml
- file_access_control_list:
    path: /var/log/myapp
    entries:
      - {user_account: promtail, permissions: r-x}
      - {group: support, permissions: r-x}
    entries_not_listed: remove
    also_apply_to_everything_inside: true
    also_use_for_new_files_inside: true
```

On the node, MMX reads the list of `/var/log/myapp` and of every file and
directory below it. It gives the account `promtail` and the group `support`
read access, and removes every other named entry, because
`entries_not_listed` is `remove`. Because `also_use_for_new_files_inside`
is `true`, the same entries also become the directory's default entries,
which files created inside later inherit, such as tomorrow's log file.

The group bits of the directory's `permissions_mode` limit what the named
entries allow (the list's mask). `setfacl -m` recalculates that mask to
include the entries' permissions. The directory's group mode bits can change
from `0700` to `0750` when a named entry gets `r-x`.

### Remove every named entry

```yaml
- file_access_control_list:
    path: /var/log/myapp
    also_apply_to_everything_inside: true
    also_use_for_new_files_inside: true
    state: absent
```

`state: absent` removes every named entry. With
`also_use_for_new_files_inside: true`, MMX also removes named default entries
and clears the remaining default entries. A default list without named
entries stays unchanged.

## How it works

- Each entry names exactly one of `user_account` or `group`, with
  `permissions` written as `getfacl` prints them: `r` or `-`, `w` or `-`,
  then `x` or `-`.
- `entries_not_listed` is required: `remove` makes the listed entries the
  whole list of named accounts and groups, and `keep` adds or corrects the
  listed ones only.
- `also_apply_to_everything_inside: true` gives every file and directory
  below `path` the entries listed in `entries`.
  `also_use_for_new_files_inside: true` also makes the entries listed in
  `entries` the directory's default entries, which files and directories
  created inside later inherit. `also_apply_to_everything_inside` and
  `also_use_for_new_files_inside` are for directories only.
- MMX refuses a symbolic link at the path, and it never follows a symbolic
  link inside a directory.
- Dryrun and results list the entries in `value.added`, `value.updated`,
  and `value.removed`, such as `user_account promtail: r-x`, and with
  `also_apply_to_everything_inside` the number of paths that change in
  `value.number_of_paths_changed`. A run that fails lists only the entries
  that its `setfacl` calls changed before the failure, so a node task in
  `on_failure` can read what happened.

## Errors and what they mean

| The reason holds | What the error means, and what to do |
|---|---|
| `setfacl is not installed; install the package acl first` | Install the package in an earlier node task; see [Set up](#set-up). The same holds for `getfacl`. A dryrun reports the node task not checked, because an earlier node task may install the package. |
| `/var/log/myapp does not exist yet` | The path is missing. A dryrun reports the node task not checked, because an earlier node task may create it. A missing account or group is treated the same way. |
| `... is a symbolic link; give the path of the file or directory itself` | MMX refuses a symbolic link at the path. Write the path of the file or directory itself. |

## Tested on

**Distributions:** Debian 12 and 13, Ubuntu 24.04 and 26.04, Rocky Linux 9, AlmaLinux 10.

The suite `tests/integration/access_control_lists_and_capabilities.py` runs
on Debian 12 and 13, Ubuntu 24.04 and 26.04, Rocky Linux 9, and AlmaLinux 10, in containers on the first four and in virtual machines on the last two. The [validation
record](../../docs/changing-mmx/validation.md) has the latest results.

## Why it works this way

`setfacl -m` recalculates the mask from the access entries. A restrictive
starting mode therefore does not prevent the requested named access.
