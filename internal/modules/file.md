# `file`

`file` manages one regular file on a node: its content, its owner, group,
and permissions, or its removal. Choose one way to supply its contents:

- `source` copies exact bytes from the controller machine, including binary
  files and literal `{{ ... }}` text.
- `template` sends a Go template that renders on the node.
- `content` supplies inline text and allows ordinary MMX value references.
- `content_from_node_paths` joins files that are already on the node
  ([below](#install-a-file-from-paths-on-the-node)).

With none of the four, `file` changes only the owner, group, and permissions
of a file that already exists.

Choose a related module where it fits better:

- [`line`](line.md) changes one line of a file that the project cannot own
  whole, and [`edit_file`](edit_file.md) changes other text in such a file.
- [`symlink`](symlink.md) manages a link. `file` replaces or removes only
  regular files.
- [`directory`](directory.md) creates directories, copies a directory from the
  controller machine, and deletes directories. `file` never deletes one.
- [`download`](download.md) installs a file from a web address, pinned by its
  SHA-256.

A sudo rule or a log rotation configuration is a `file` with
`check_new_file`; MMX has no separate module for either.

MMX trusts the directories above a file's `path`: it follows a symlink in
them, as Ansible does. An account that can write such a directory can
point root's write elsewhere, so keep directories above a root-managed
file writable only by root.

## Examples

### Write a file

```yaml
- file:
    path: /etc/myapp/app.conf
    template: files/app.conf.tmpl
    owner: root
    group: myapp
    permissions_mode: "0640"

- file:
    path: /usr/local/bin/myapp
    source: files/myapp
    permissions_mode: "0755"

- file:
    permissions_mode: "0644"
    path: /etc/myapp/environment
    content: "ENVIRONMENT={{ .vars.environment }}\n" # environment is a variable you define
```

The controller process reads `files/app.conf.tmpl` and `files/myapp` from the
project on the controller machine and sends them with the run. On the node,
the first task renders the template, the second copies the program's bytes,
and the third writes the inline text. Each task installs its file only when
the content or metadata differs from what the node has.

Create parent directories with earlier `directory` node tasks. A missing
parent directory fails the node task once, with a reason that names the
directory, and is not tried again. All four content forms use the same
metadata handling, checks, and atomic installation: the node prepares the
new file before replacing the live file.

### One file for each node or each loop item

The destination `path` and the controller-machine `source` and `template`
paths may contain references. A single task can therefore install a
different file for each node or each loop item:

```yaml
# inventories/production.yml gives each node a `site` value in its `vars`,
# such as `site: frankfurt`. That node gets files/haproxy/frankfurt.cfg.tmpl.
- file:
    permissions_mode: "0644"
    path: /etc/haproxy/haproxy.cfg
    template: "files/haproxy/{{ .vars.site }}.cfg.tmpl"

# Each loop item installs its own file: files/keys/alice.pub, files/keys/bob.pub.
- loop: [alice, bob]
  as: person
  file:
    permissions_mode: "0644"
    path: "/etc/app/keys/{{ .person }}.pub"
    source: "files/keys/{{ .person }}.pub"
```

The controller process resolves each path for each node and each loop item
before connecting to nodes, and saves every file with the run.
[Paths on the controller machine](#paths-on-the-controller-machine) says
which values a path may read.

### Write a file once, then leave its content alone

Some programs rewrite their own configuration file, such as Redis Sentinel,
and some files are written once and then belong to the node, such as a first
password file. To preserve program-managed configuration or a password file
after creation, add `if_file_exists: keep_existing_content`:

```yaml
- file:
    path: /etc/redis/sentinel.conf
    template: files/sentinel.conf.tmpl
    owner: redis
    group: redis
    permissions_mode: "0640"
    if_file_exists: keep_existing_content
```

When the file is missing, MMX creates it from the template. When the file
exists, MMX leaves its bytes alone and applies only `owner`, `group`, and
`permissions_mode`. The default, `replace_existing_content`, makes the file's
content what the task gives on every run. [`download`](download.md) also
accepts the `if_file_exists` setting.

Dryrun shows a missing file as would be created, and an existing one with
only its metadata differences. A `check_new_file` check runs only when MMX
creates the file.

### Install a file from paths on the node

`content_from_node_paths` makes a file from files that are already on the
node, such as a certificate that certbot renews. MMX joins the bytes of the
files named in `content_from_node_paths`, in the listed order. A list with
one path copies the named file's bytes unchanged:

```yaml
# HAProxy wants the certificate and its private key in one file.
- file:
    path: /etc/haproxy/certs/site.pem
    content_from_node_paths:
      - /etc/letsencrypt/live/site.example.com/fullchain.pem
      - /etc/letsencrypt/live/site.example.com/privkey.pem
    owner: root
    group: haproxy
    permissions_mode: "0600"
```

Each path is absolute. MMX follows links, as certbot's `live` directory
uses, and each path must end at a regular file. Otherwise this is an ordinary
`file` node task: MMX compares, runs `check_new_file`, installs atomically, and
sets the metadata. In a node task block, the destination is a tracked input,
so a renewed certificate reloads or restarts the block's service on the next
run, also after an interrupted run.

Dryrun and results show a change as the SHA-256 values of the old and the new
content and never as text. A path that does not exist yet is reported as not
checked in dryrun, since an earlier node task may create it; in a run it
fails the node task.

### Keep the original of a file MMX takes over

When MMX takes over a file a package installed, keep the package's version
with `backup_original_file_to`:

```yaml
- file:
    permissions_mode: "0644"
    path: /etc/ssh/sshd_config
    template: files/sshd_config.tmpl
    backup_original_file_to: /etc/ssh/sshd_config.original
```

When the backup path does not exist and the file does, MMX copies the file,
with its owner, group, and mode, to the backup path before it first changes
the file. MMX never writes that copy again, so it stays the distribution's
original however often the file changes later. A file that does not exist
yet has no original, and no copy is made. Dryrun reports the copy it would
make as `original_file_copied_to`.

### Check a file that holds secrets

A configuration checker may promise to print useful errors without printing
secret values. Set `output_holds_no_secret: true` on its `shell` to see
those errors even when the file's template reads a secret:

```yaml
- file:
    path: /etc/myapp.conf
    template: files/myapp.conf.tmpl
    permissions_mode: "0600"
    check_new_file:
      shell:
        script: myapp settings check --settings-file "$NEW_FILE"
        dryrun: myapp settings check --settings-file "$NEW_FILE"
        env:
          NEW_FILE: "{{ .new_file.path }}"
        output_holds_no_secret: true
```

The template `files/myapp.conf.tmpl` on the controller machine contains the
application's configuration and may read a secret, such as
`{{ .secrets.database_password }}`. On the node, the checker reads the
candidate through `NEW_FILE`; a failed check leaves the installed file as
it was. In both a run and a dryrun, the nested `check_new_file` result shows
the check's standard output and standard error, and the file task shows the
check's failure reason. The file task stays secret: its diff and other
outputs remain hidden.

MMX replaces every secret value the node job holds or the node task read,
including in the template and the check's `env`, with `[redacted]` in those
diagnostics. It also replaces each value without surrounding whitespace,
each nonempty line without surrounding whitespace, the keys of a secret
mapping, and the JSON text MMX gives a secret list or mapping through `env`.
MMX cannot know other forms, such as part of a line or its Base64 text, so the checker must promise to keep those out of its output too.
The field defaults to `false`. It cannot be combined with
`output_is_secret: true`, which declares the command's output secret;
project loading refuses that contradiction. See
[`shell` secret output](shell.md#secret-output) for the command's promise.

### Remove a file

```yaml
- file:
    path: /etc/myapp/obsolete.conf
    state: absent
```

On the node, MMX removes `/etc/myapp/obsolete.conf` if it is a regular file.
A file that is already missing leaves the task unchanged. Removing a task
from YAML never deletes the file it wrote; only `state: absent` does.

### Change file permissions without changing contents

```yaml
- file:
    path: /etc/myapp.conf
    owner: myapp
    group: myapp
    permissions_mode: "0640"
```

With no content supplied, `file` changes only the existing regular file's
metadata. It preserves the contents and inode. A missing file fails the node
task because MMX has no content with which to create it. It fails as a
missing prerequisite, without another try, and a dryrun reports the task
`not_checked`, because an earlier node task, such as a package, may create
the file.

## How it works

### Paths on the controller machine

`source` and `template` are paths on the controller machine. Relative paths
start at the project root, found by searching the current directory and then
its parents for `mmx_project_settings.yml`, or at the role's directory inside
a role. Absolute paths are also allowed. The same rule applies to
`directory.source`, `apt_repository.signing_key_source`,
`unpack_archive.source`, `systemd_unit.template`, and
`authorized_keys.key_files_on_controller_machine` and `key_directories_on_controller_machine`.

The path may read what the controller machine knows before it connects: the
node's name (`.node.name`), `vars` from the project and the inventory, role
inputs, and loop items. It may not read a node's facts, such as `.node.arch`,
or the result of a node task: a path that does is a validation error.
`mmx validate` and every run check that the path names an existing file for
every node of the inventory, before MMX connects to any node, and the error
names the node, the field, and the path. For two processor architectures,
write two tasks, each with a `when` on `.node.arch`.

A relative path may leave its role directory, but must stay inside the
project. For example, `source: ../other_role/files/app.conf` can read another
role's file. Prefer self-contained roles so moving one role does not break
another. Absolute paths may name any file on the controller machine.

The controller process saves these bytes with the run. Resume does not reread
local sources that have changed or been deleted. Files travel to the nodes as
[streamed assets](../../docs/changing-mmx/design/execution.md#freeze-the-work-then-dispatch-it),
independent of the JSON job size.

### Files that hold secrets

When the content or template reads a secret, only the read permission bits
choose who can see the secret. Owner-only access, such as `"0600"` or
`"0700"`, needs no extra setting. Group-readable modes, such as `"0640"`,
require `secret_readable_by_group: true`. Modes readable by every account,
such as `"0644"`, require `dangerously_let_every_account_read_secret: true`.
Setting `dangerously_let_every_account_read_secret: true` also permits group
reading. Write and execute bits do
not count. MMX cannot detect secrets in literal `source` bytes or files read
through `content_from_node_paths`; choose their modes explicitly too.

```yaml
- file:
    path: /etc/myapp/password
    content: "{{ .secrets.database_password }}"
    group: myapp
    permissions_mode: "0640"
    secret_readable_by_group: true
```

The node writes the password so its owner and the `myapp` group can read it.
[`download`](download.md) and files saved with
[`save_on_controller_machine`](../../docs/how-to/use-a-result-from-one-server-on-another.md#save-outputs-to-files-on-your-controller-machine)
use the same permission rule when MMX knows their content holds a secret.

When `content` reads a named secret or a role input declared secret, MMX
checks a literal `permissions_mode` while loading the project. If an acknowledgement
is missing, MMX refuses the project before any node job starts. The node also checks
secret use when a template renders or a referenced mode resolves. A refusal
names the file, mode, and readers so the access can be reviewed before adding
an acknowledgement.

### Installation on the node

The node writes the new content to a temporary file, runs the optional
check, and then replaces the live file atomically.
`permissions_mode` is a quoted octal string. `owner` and `group` are optional;
`permissions_mode` is required when the task writes content. A task that
changes only existing metadata may omit `permissions_mode`. `owner` and `group`
take an account name or a number, as
[the rule for owner and group](../../docs/changing-mmx/design/modules.md#owner-and-group-are-an-account-name-or-a-number)
says, and [`directory`](directory.md#a-directory-owned-by-a-numeric-id)
shows an example.

`permissions_mode` may read a reference. The resolved value must still be a
quoted octal string; MMX checks its form before the module runs.

A node task with `content`, `template`, or `source` also preserves the inode
when only metadata changes. A declared `check_new_file` check still runs
before those changes. In a dryrun, a check that fails after an earlier node
task would change the node, such as a `user` task that would create an
account the new file names, is not checked; the result keeps the check's
output. `file`, `download`, `line`, and `edit_file` share the
check of a new file, which
[Check a new file before replacing the live one](../../docs/reference/shared-module-behavior.md#check_new_file)
and its [design](../../docs/changing-mmx/design/modules.md#check-new-files-before-installing-them)
describe.

`state: absent` accepts only `path` and `state`. It removes only a regular
file, refuses directories and symlinks, and is unchanged when the file is
missing.

Dryrun and `--diff` previews follow
[Preview file changes](../../docs/reference/shared-module-behavior.md#previewing-file-changes).

### In a node task block

Within a node task block, copied files and file removals contribute to the
service configuration hash: the record used to decide whether a service must
load new configuration. Permission changes do not affect service
configuration hashes. A kept file (`if_file_exists: keep_existing_content`)
is not part of its node task block's configuration hash. A `file` node task
and a [`line`](line.md) or [`edit_file`](edit_file.md) node task for the same
path in one node task block are rejected, because `file` owns the whole
contents.

## Tested on

**Distributions:** Debian 12 and 13, Ubuntu 24.04 and 26.04, Arch Linux, openSUSE Leap 16.1, Rocky Linux 9, AlmaLinux 10, Fedora 45.

The base suite covers this module (`file_resources.py`, `streamed_assets.py`, `vm.py test`) and runs on every tested distribution.

## Why it works this way

- **A path reads only what the controller machine knows before it
  connects.** MMX saves every file before the run starts, and a node's facts
  and node task results exist only during the run.
- **Relative paths stay inside the project.** This permits shared files
  without forcing every role to keep a separate copy. Self-contained roles
  remain easier to move.
- **Content writes require a mode.** MMX cannot guess which accounts should
  read or change the file; see [required settings](../../docs/changing-mmx/design/modules.md#a-setting-with-no-safe-guess-is-required).
- **Reading a secret needs explicit permission.** Ordinary group access uses
  a plain acknowledgement; public access removes protection for the secret.
  See [protective choices](../../docs/changing-mmx/design/modules.md#protective-choices-come-in-three-levels).
- **Known permission refusals happen before dispatch.** Earlier node tasks
  must not change a node before a predictable failure. The
  [secret-file rule](#files-that-hold-secrets) says which values MMX knows
  while loading and which it checks on the node.
- **A kept file does not count in the configuration hash.** The program
  rewriting its own file must not restart on the next run.
- **Content from paths on the node is shown as SHA-256 values.** The content
  may be a private key.
- **MMX never writes the backup copy again.** It stays the distribution's
  original however often the file changes later.
