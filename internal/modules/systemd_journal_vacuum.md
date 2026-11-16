# `systemd_journal_vacuum`

The systemd journal is where systemd keeps what the node's services print
and what systemd notes about them, in files under `/var/log/journal` (or
`/run/log/journal` on a node that keeps the journal only in memory).
journald, systemd's journal service, writes to one active file of each kind
and from time to time archives it: it renames the file and starts a new
one. `systemd_journal_vacuum` deletes archived files, the oldest first, with
`journalctl --vacuum-size` and `--vacuum-time`, until the journal fits the
size or holds no file older than the age that the node task gives. It frees
disk space on a node whose disk the journal fills.

It deletes only archived files, never the files that journald writes now.
To keep the journal small for good, set `SystemMaxUse=` or
`MaxRetentionSec=` in a drop-in of `journald.conf` with a [`file`](file.md)
node task; `systemd_journal_vacuum` is for the one-time cleanup.

## Examples

### Free the space that old journal files take

```yaml
- systemd_journal_vacuum:
    keep_at_most_size: 500MiB
    keep_newer_than: 14d
```

On the node, MMX runs `journalctl --vacuum-size=524288000
--vacuum-time=1209600s`. journalctl looks at each journal directory of the
node by itself and deletes archived files, the oldest first, while the
directory's journal files, the active ones included, take more than 500 MiB,
and every archived file whose first entry is older than 14 days. MMX reads
the directories before and after, and the node task reports what is gone:

```text
changed — deleted 6 archived journal files and freed 768.0 MiB (805306368 bytes)
```

`value.freed_bytes` holds the freed bytes and
`value.deleted_archived_journal_files` the number of files. A second run
finds nothing to delete and reports `unchanged`.

### Only by age, in a one-time playbook

```yaml
- systemd_journal_vacuum:
    keep_newer_than: 30d
```

A [one-time playbook](../../docs/how-to/make-a-one-time-change.md) with this node
task cleans up once on every node it selects. `mmx check-one-time-playbooks`
counts a node as done when its dryrun finds no archived file older than 30
days. Since files keep getting older, a node can need the playbook again
later; complete the playbook when every node is done.

## How it works

- **Which files.** journald names an archived file
  `<name>@<sequence>-<number>-<time>.journal`, and a file that it set aside
  as damaged `<name>@<time>-<number>.journal~`. Only such files are deleted.
  The active files, such as `system.journal` and `user-1000.journal`, stay;
  they count toward `keep_at_most_size`, so the archived files can go
  entirely and the journal still take more than that size.
- **Which directories.** The directories that journalctl itself works on:
  `/var/log/journal` and `/run/log/journal`, and in each the directory named
  by the node's machine ID (`/etc/machine-id`). journalctl applies
  `keep_at_most_size` to each directory on its own.
- **Bytes.** Sizes are what the files take on disk: their allocated blocks,
  as journald counts them.
- **Empty files.** journald deletes an archived file without entries in every
  vacuum, whatever the limits.
- **The age of a file.** The time in an archived file's name is that of its
  first entry. journald takes the earliest of that time and the file's own
  times, so a file whose modification time is older counts as older.
- **The dryrun** deletes nothing. It reads the same directories and applies
  journald's own rule to the files' names, sizes, and times, so it predicts
  what a run deletes. A file that journald archives, or deletes for its own
  limits, between the dryrun and the run changes the answer.

## Errors and what they mean

| The reason holds | What it means, and what to do |
|---|---|
| `systemd_journal_vacuum requires keep_at_most_size, keep_newer_than, or a value for each setting` | Validation refuses a node task without a limit. Write at least one. |
| `journalctl --vacuum-size=...: ...` | journalctl failed; the reason holds what it wrote to standard error and what it deleted before. |

## Tested on

**Distributions:** Debian 12 and 13, Ubuntu 24.04 and 26.04.

The suite `tests/integration/routine_fixes.py` archives the journal with
`journalctl --rotate`, ages archived files, and compares the dryrun with the
run and with the files on the node, on Debian 12 and 13 and Ubuntu 24.04 and 26.04. The
[validation record](../../docs/changing-mmx/validation.md) has the latest results.

## Why it works this way

- **journalctl does the deleting.** journalctl is the tool that knows
  journald's files, and journald's own rule decides. MMX only gives the
  limits.
- **MMX decides the result from the files, not from what journalctl
  prints.** journalctl's summary rounds the bytes; the files before and after
  give the exact number.
- **At least one limit, and no default.** How much journal a node keeps
  depends on what the node is for, so the node task says it.
- **No active file is deleted.** journald writes to the active files. To
  make the journal smaller for good, configure journald.
