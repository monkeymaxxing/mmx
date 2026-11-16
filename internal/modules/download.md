# `download`

`download` installs a file from an HTTPS web address on nodes, such as a
public release artifact. The controller machine fetches the file and checks
it against the SHA-256 the task gives, then sends it to the nodes, where MMX
installs it like a [`file`](file.md) with `source`. Use
[`unpack_archive`](unpack_archive.md) to unpack a pinned archive, and
[`git`](git.md) or a `shell` task for a source that needs credentials.

## Examples

### Install a release artifact

Supply the URL and the expected SHA-256 digest:

```yaml
- download:
    url: https://downloads.example.com/app-1.2.3-linux-arm64
    # Replace with the SHA-256 of the release artifact you have verified.
    sha256: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
    path: /usr/local/bin/app
    permissions_mode: "0755"
```

The controller process fetches each URL/digest once per run and verifies the
bytes. A file that an earlier run of the project saved is reused after its
SHA-256 is checked again, so an unchanged run does not fetch it again. A
node keeps the bytes of each file from a web address that a run sent it,
by their SHA-256, until no run has used them for 30 days, so a later run or
dryrun sends a node only the files it does not have yet. Matching installed
bytes stay unchanged. The [download example](../../examples/download/README.md)
installs an executable and restarts its service when the bytes change.

### A version from the project's variables

The URL and the SHA-256 may read values the controller machine knows before it
connects, such as a version number in the project's `vars`, under the
[same rule as a source path](file.md#paths-on-the-controller-machine):

```yaml
# roles/app/main.yml
# app_version and app_sha256 are variables you define in the project's or the
# inventory's vars, such as app_version: 1.2.3 and its 64-digit SHA-256.
node_tasks:
  - download:
      url: "https://downloads.example.com/app-{{ .vars.app_version }}-linux-amd64"
      sha256: "{{ .vars.app_sha256 }}"
      path: /usr/local/bin/app
      permissions_mode: "0755"
```

The controller machine fills in `url` and `sha256` for each node before
connecting, then fetches and checks the file as in the first example.

### One file for each processor architecture

A release that publishes one file for each processor architecture needs one
task for each file, each with a `when` on `.node.arch`. Both tasks may
install the same path:

```yaml
- task_block:
    id: app_binary
    changes:
      - when: {equals: ["{{ .node.arch }}", amd64]}
        download:
          url: https://downloads.example.com/app-1.2.3-linux-amd64
          # Replace with the SHA-256 of the amd64 file.
          sha256: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
          path: /usr/local/bin/app
          permissions_mode: "0755"
      - when: {equals: ["{{ .node.arch }}", arm64]}
        download:
          url: https://downloads.example.com/app-1.2.3-linux-arm64
          # Replace with the SHA-256 of the arm64 file.
          sha256: "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"
          path: /usr/local/bin/app
          permissions_mode: "0755"
    activate:
      - systemd_service:
          name: app
          state: running
```

The controller machine fetches and checks both files before the run starts.
On each node, only the task whose `when` matches the node's architecture
installs `/usr/local/bin/app`, and the node restarts `app` when the bytes of
that file change. Two tasks in one node task block that would both run on a
node and write the same path are an error, as
[tracked inputs](../../docs/changing-mmx/design/execution.md#node-task-blocks-complete-the-change)
explains.

### A file without a published checksum

Every field that fetches from a web address needs a checksum beside it. Where
a vendor publishes no checksum and the file changes under the same address,
say so on the page instead:

```yaml
- download:
    url: https://downloads.example.com/app-latest-linux-amd64
    dangerously_skip_checksum: true
    path: /usr/local/bin/app
    permissions_mode: "0755"
```

MMX then installs the file as it arrives. Nobody notices when the server at
that address starts to serve something else, which is why the setting has
"dangerously" in its name: a reviewer sees it. A task gives exactly one of the
checksum or `dangerously_skip_checksum: true`.
[A file from a web address is checked against its SHA-256](../../docs/changing-mmx/design/modules.md#a-file-from-a-web-address-is-checked-against-its-sha-256)
defines the checksum requirement for every module that fetches from a web
address.

### A tool published as one compressed file

Some tools are published as one compressed file, such as
`restic_0.17.3_linux_amd64.bz2`. Name the compression with `decompress`:
`gzip`, `bzip2`, `xz`, or `zstd`:

```yaml
- download:
    url: https://github.com/restic/restic/releases/download/v0.17.3/restic_0.17.3_linux_amd64.bz2
    # The SHA-256 of the .bz2 file as the project publishes it.
    sha256: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
    decompress: bzip2
    path: /usr/local/bin/restic
    permissions_mode: "0755"
```

`sha256` is the digest the project publishes next to the compressed file.
The controller machine checks the downloaded compressed file against the
expected SHA-256 digest, then decompresses the verified file.
The controller machine sends the decompressed file to the nodes for
installation. The next run is unchanged as long as the installed bytes
match. The nodes and the controller machine need no decompression tool
because MMX decompresses with MMX's own code.

## How it works

`url` is an HTTPS URL without credentials or a fragment. Normal TLS checks
apply, redirects stay HTTPS, and transfers stop after 30 seconds without
bytes. Invalid, incomplete, or mismatched downloads fail before any node job
starts.

`validate` and `explain` do not fetch downloads. `dryrun` fetches and verifies
inputs on the controller machine, then previews changes on nodes, and changes
no managed files. Resume uses the saved bytes and needs no source access. A
new `dryrun` or `run` reuses a file with a `sha256` that an earlier dryrun
or run saved and that the history on the controller machine still holds,
after it checks the SHA-256 again. It fetches the source only when no saved
run holds the file. A
download with `dangerously_skip_checksum: true` or `decompress` is fetched
on every new `dryrun` or `run`.

Downloads use the same streamed assets and file installer as local sources.
They accept the file fields `path`, `owner`, `group`, `permissions_mode`,
`if_file_exists`, `secret_readable_by_group`,
`dangerously_let_every_account_read_secret`, and `check_new_file`, and otherwise behave exactly like
`file.source`, including unchanged detection, validation, previews, and
service configuration hashes. Results still name the node task `download`.
Ordinary references are allowed in the path, owner, group, permissions mode, and validator
inputs. `if_file_exists` works as for
[`file`](file.md#write-a-file-once-then-leave-its-content-alone), and the
check of a new file, shared with `file`, `line`, and `edit_file`, is
described in
[Check a new file before replacing the live one](../../docs/reference/shared-module-behavior.md#check_new_file)
and its [design](../../docs/changing-mmx/design/modules.md#check-new-files-before-installing-them).
`owner` and `group` take an account name or a number, as
[the rule for owner and group](../../docs/changing-mmx/design/modules.md#owner-and-group-are-an-account-name-or-a-number)
says.

With `decompress`, the controller machine keeps only the decompressed file
with the run, so resume needs neither the web address nor a second
decompression. A new run fetches such a file again, because only the
compressed file's SHA-256 is known before the fetch. A small compressed file can hold a huge one, so MMX refuses a
decompressed file that would leave less than 5 % of the controller machine's
filesystem free, as the Avail column of `df` counts it, and the run fails
before any node job starts.

This form does not support authentication, a URL that depends on a node's
facts or on a task result. The same pinned fetch
supplies [`unpack_archive`](unpack_archive.md), package files, and repository
keys.


## Tested on

**Distributions:** Debian 12 and 13, Ubuntu 24.04 and 26.04, Arch Linux, openSUSE Leap 16.1, Rocky Linux 9, AlmaLinux 10, Fedora 45.

The base suite covers this module (`dns_tls_and_account_lookups.py`) and runs on every tested distribution.

## Why it works this way

- **Every download requires `permissions_mode`.** MMX cannot guess which
  accounts should read or run the downloaded file. See
  [required settings](../../docs/changing-mmx/design/modules.md#a-setting-with-no-safe-guess-is-required).
- **Known secret content follows the file permission rule.** The
  [file page](file.md#files-that-hold-secrets) owns that rule and its
  acknowledgements, under the shared
  [protective choices](../../docs/changing-mmx/design/modules.md#protective-choices-come-in-three-levels).
