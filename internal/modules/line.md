# `line`

`line` manages one line of an existing regular file on a node. The `line`
module replaces the line that a regular expression or the exact text selects.
When no existing line is selected, the module adds the text from the task's
`line` setting. The module can also remove the selected line.

Prefer a program's drop-in directory or a whole managed [`file`](file.md)
when available. Use `line` for one line of a file that has no drop-in
directory, and [`edit_file`](edit_file.md) for an edit that one line cannot
make. The page of [`edit_file`](edit_file.md) gives the order in which to
choose between a drop-in, a whole file, `line`, and `edit_file`.

## Examples

### Set one line

```yaml
- line:
    insert_at_end_of_file: true
    path: /etc/ssh/sshd_config
    match: '^#?PasswordAuthentication\s'
    line: PasswordAuthentication no
```

`match` is a Go regular expression tested against each line. Without `match`,
the exact `line` selects the existing line. MMX replaces the selected line,
or appends `line` at the end if nothing matches. The new `line` must itself
match `match`, so another run is unchanged.

### Set a value whatever it was before

```yaml
- line:
    insert_at_end_of_file: true
    path: /etc/default/grub
    match: '^GRUB_CMDLINE_LINUX='
    line: 'GRUB_CMDLINE_LINUX="console=ttyS0,115200"'
```

`match` selects the line that sets `GRUB_CMDLINE_LINUX`, whatever value it
has now. On the node, MMX replaces the line that sets `GRUB_CMDLINE_LINUX`
with the task's `line`. When no line sets `GRUB_CMDLINE_LINUX`, MMX adds
`line` at the end of `/etc/default/grub`.

### Put a new line in a chosen place

Choose exactly one of the six insertion settings below. The chosen setting
says where the new line goes when no existing line is selected:

```yaml
# Put the rule before the catch-all line of the file.
- line:
    path: /etc/postgresql/16/main/pg_hba.conf
    line: "host  app  app  10.0.0.0/8  scram-sha-256"
    insert_before_first_line_matching: '^host\s+all\s+all\s'
```

| Setting | The new line goes |
|---|---|
| `insert_at_end_of_file: true` | At the end of the file. MMX first ends the file's last line if it lacks a newline. |
| `insert_at_start_of_file: true` | Before the first line. |
| `insert_before_first_line_matching: <pattern>` | Directly before the first line the pattern matches. |
| `insert_before_last_line_matching: <pattern>` | Directly before the last line the pattern matches. |
| `insert_after_first_line_matching: <pattern>` | Directly after the first line the pattern matches. |
| `insert_after_last_line_matching: <pattern>` | Directly after the last line the pattern matches. |

A task with `state: present` requires exactly one of them. Each pattern is a Go regular expression, like
`match`. They place only a new line and never move a line that is already
there. A pattern that matches no line fails the node task and changes
nothing, so a line never lands in a place you did not choose.

### Remove a line

```yaml
- line:
    path: /etc/ssh/sshd_config
    match: '^PermitRootLogin\s'
    state: absent
```

Use `state: absent` with either `match` or `line` to remove the selected line.
The task then accepts `path`, `state`, `check_new_file`, and one of `match`
or `line`. Nothing selected leaves the task unchanged.

## How it works

`match` is tested against each line without its newline. `path` and `match`
are public identifiers and may be references, so one task in a loop can set
several settings. `line` is a private value, like `file.content`, and is one line. That `line` matches `match` is
checked at validation when both are known on the controller machine, and
otherwise on the node before any change.

The file must already exist; `line` never creates files. A missing file, a
directory, or a symlink at `path` fails the node task. Dryrun reports a
missing file as not checked, since an earlier node task, such as a package
installation, may create it.

The edit preserves owner, group, and mode and goes through the file
installer: a temporary new file, the optional `check_new_file`, and an atomic
replacement of the live file. Differences and service hashes treat the path
like a `file` task.

A `line` that holds a secret, such as `API_TOKEN={{ .secrets.api_token }}`,
follows the rule of [`file`](file.md#files-that-hold-secrets): the file
keeps its mode, so when that mode lets the file's group read it, the node
task needs `secret_readable_by_group: true`, and when every account can
read it, `dangerously_let_every_account_read_secret: true`. Otherwise the
node task fails and changes nothing. To keep the secret private instead,
make the file readable only by its owner first, such as with a `file` node
task that sets `permissions_mode: "0600"`.

The check of a new file is shared with `file`,
`download`, and `edit_file`, and
[Check a new file before replacing the live one](../../docs/reference/shared-module-behavior.md#check_new_file)
and its [design](../../docs/changing-mmx/design/modules.md#check-new-files-before-installing-them)
describe it.

Several `line` node tasks can edit the same file, including within one node
task block. That file counts once in the service hash, and a dryrun applies
the edits in turn to predict it. `line` and [`edit_file`](edit_file.md) node
tasks may also edit the same file in one node task block. A `file` node task
for the same path in that node task block is rejected, because `file` owns
the whole contents.

## Errors and what they mean

- **More than one matching line** fails the node task, names the line
  numbers, and changes nothing. Make `match` select exactly one line.
- **A placing pattern that matches no line** fails the node task and changes
  nothing. Correct the pattern, or use `insert_at_end_of_file: true` to add the line
  at the end of the file.
- **A missing file** fails the node task in a run. Create the file first,
  such as with the package that installs it.
- **`line: /etc/environment would hold a secret, and the file's mode 0644
  lets every account on the node read it`**: the line holds a secret. Make
  the file readable only by its owner first, or add
  `dangerously_let_every_account_read_secret: true` if every account
  should read it.

## Tested on

**Distributions:** Debian 12 and 13, Ubuntu 24.04 and 26.04, Arch Linux, openSUSE Leap 16.1, Rocky Linux 9, AlmaLinux 10, Fedora 45.

The base suite covers this module (`file_resources.py`, `task_secrets.py`) and runs on every tested distribution.

## Why it works this way

- **One line, at most one match.** Whole files and drop-ins have obvious
  ownership; `line` keeps the part MMX owns as small as one line. The
  [rule on resource ownership](../../docs/changing-mmx/design/modules.md#resource-ownership-must-be-obvious)
  says why MMX manages parts of a system through native boundaries.
- **A new line is placed, never moved.** A pattern that matches nothing
  fails instead of falling back to the end, so a line never lands in a place
  the author did not choose.
- **The new line must match `match`.** The next run then finds it and
  reports unchanged.

- **Placement is required even when replacing a line.** The task must say
  where a missing line belongs, so a changed file cannot cause an implicit
  append. See [required settings](../../docs/changing-mmx/design/modules.md#a-setting-with-no-safe-guess-is-required).
