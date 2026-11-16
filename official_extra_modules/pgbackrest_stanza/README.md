# `pgbackrest_stanza`

`pgbackrest_stanza` manages one [pgBackRest](https://pgbackrest.org/)
stanza. pgBackRest backs up PostgreSQL and archives its write-ahead log,
the record of every change, to a *repository*, such as an S3-compatible
bucket; it can then restore to any moment that the archive covers. A
*stanza* is pgBackRest's name for one PostgreSQL cluster and its backups
and archive in the repositories.

The module runs `pgbackrest` as the account `postgres` with the task's
configuration file, on the database server, which is a **node** of MMX: a
Linux server that MMX manages. The module creates a missing stanza and
upgrades a stanza after a major upgrade of PostgreSQL.
After creating or upgrading a stanza, the module checks that archiving
reaches the repositories. Restoring is
[`pgbackrest_restore`](../pgbackrest_restore/README.md).

## Set up

Debian and Ubuntu package pgBackRest as `pgbackrest`: 2.45 on Debian 12,
2.50 on Ubuntu 24.04, 2.55 on Debian 13, and 2.58 on Ubuntu 26.04. The
configuration file, archiving, and the backups are ordinary **node tasks**;
a node task is one entry of a playbook's `node_tasks`. Here the database is
a [pg_auto_failover node](../pg_auto_failover_node/README.md#set-up), whose
service is `pgautofailover`; for a cluster of Debian's own tools, the
service is `postgresql@18-main` and the file
`/etc/postgresql/18/main/postgresql.conf`.

```yaml
# mmx run configure: the database servers' tasks after pg_auto_failover_node
fleet_stages:
  - id: backups
    nodes: database_servers
    node_tasks:
      - apt_package: {packages: [{name: pgbackrest, version: newest_when_first_installed}]}

      # The repository: a bucket of an S3-compatible service, with its key
      # from the vault. postgres reads the file, and nobody else.
      - directory: {path: /etc/pgbackrest, permissions_mode: "0755"}
      - file:
          path: /etc/pgbackrest/pgbackrest.conf
          owner: root
          group: postgres
          permissions_mode: "0640"
          secret_readable_by_group: true   # the group postgres reads the bucket's key
          content: |
            [global]
            repo1-type=s3
            repo1-s3-endpoint=s3.example.net
            repo1-s3-bucket=example-backups
            repo1-s3-region=eu-central-1
            repo1-s3-key={{ .secrets.backup_bucket_key }}
            repo1-s3-key-secret={{ .secrets.backup_bucket_key_secret }}
            repo1-path=/production
            repo1-retention-full=4

            [main]
            pg1-path=/var/lib/postgresql/18/main

      # Archiving. archive_mode takes effect only at a restart of
      # PostgreSQL, so the block restarts the service when a line changed.
      # A line appended to postgresql.conf comes after pg_autoctl's
      # included file and wins.
      - task_block:
          id: archiving
          changes:
            - line:
                path: /var/lib/postgresql/18/main/postgresql.conf
                match: '^archive_mode\s*='
                line: archive_mode = on
                insert_at_end_of_file: true
            - line:
                path: /var/lib/postgresql/18/main/postgresql.conf
                match: '^archive_command\s*='
                line: "archive_command = 'pgbackrest --config=/etc/pgbackrest/pgbackrest.conf --stanza=main archive-push %p'"
                insert_at_end_of_file: true
            - line:
                path: /var/lib/postgresql/18/main/postgresql.conf
                match: '^archive_timeout\s*='
                line: archive_timeout = 60
                insert_at_end_of_file: true
          activate:
            - systemd_service: {name: pgautofailover, state: running, refresh: restart}
      - wait_for:
          command_succeeds: /usr/lib/postgresql/18/bin/pg_isready -q -p 5432
          wait_at_most: 60s

      - pgbackrest_stanza:
          stanza: main
          config_file: /etc/pgbackrest/pgbackrest.conf

      # A weekly full and a daily differential backup. The jobs run on every
      # database server and do nothing on a replica, so the backups follow
      # the primary after a failover.
      - systemd_scheduled_job:
          name: pgbackrest-full-backup
          run_as_user_account: postgres
          systemd_calendar_schedule: "Sun *-*-* 01:00:00"
          script: |
            if [ "$(psql --no-psqlrc -At -p 5432 -d postgres -c 'select pg_is_in_recovery()')" = f ]; then
              pgbackrest --config=/etc/pgbackrest/pgbackrest.conf --stanza=main --type=full backup
            fi
      - systemd_scheduled_job:
          name: pgbackrest-differential-backup
          run_as_user_account: postgres
          systemd_calendar_schedule: "Mon..Sat *-*-* 01:00:00"
          script: |
            if [ "$(psql --no-psqlrc -At -p 5432 -d postgres -c 'select pg_is_in_recovery()')" = f ]; then
              pgbackrest --config=/etc/pgbackrest/pgbackrest.conf --stanza=main --type=diff backup
            fi
```

A bucket that keeps the versions of its objects, with object lock where
the service has it, lets
[`pgbackrest_restore`](../pgbackrest_restore/README.md) read the
repository as it stood before an attacker deleted or overwrote files.

## Inputs

| Input | Meaning | When omitted |
|---|---|---|
| `stanza` | The stanza's name, as in the configuration file, such as `main`. It must not be empty, start with a hyphen, or hold a slash or white space. | Required. |
| `config_file` | The configuration file that `pgbackrest` reads, an absolute path such as `/etc/pgbackrest/pgbackrest.conf`. Its section of the stanza, or `[global]`, gives `pg1-path`, the cluster's data directory. | Required. |

## Outputs

| Output | Meaning |
|---|---|
| `created` | `true` when the task created the stanza. In a dryrun: `true` when a run would create the stanza. |

## What a dryrun reports

A dryrun runs `pgbackrest info --output=json` and changes nothing.

- **A stanza to create or upgrade:** `would_change`, `would create the
  stanza main for PostgreSQL 18 in /var/lib/postgresql/18/main`, or `would
  upgrade ...`. A run then checks archiving, which a dryrun does not.
- **`not_checked`** while `pgbackrest` is missing, while the configuration
  file is missing or an earlier node task of the dryrun writes it, while
  `pg1-path` holds no PostgreSQL cluster, and for a missing stanza on a
  replica: an earlier node task may install the package, write the file,
  or create the cluster, and the primary creates the stanza.
- **A failure, as in a run,** for an input that breaks a rule, a
  configuration file without `pg1-path` for the stanza, and a stanza that
  stopped partway.
- **What the question leaves on the node:** `pgbackrest info` writes its
  own log only where the configuration file asks for a log level of files.

## Examples

The stanza of the configuration file above:

```yaml
- pgbackrest_stanza:
    stanza: main
    config_file: /etc/pgbackrest/pgbackrest.conf
```

A second cluster on the setup example's database server, with a `[reports]`
section in `/etc/pgbackrest/pgbackrest.conf`:

```yaml
- pgbackrest_stanza:
    stanza: reports
    config_file: /etc/pgbackrest/pgbackrest.conf
```

## How it works

1. The module reads `pg1-path` from the configuration file and the stanza
   with `pgbackrest info --output=json`, which ends with exit code 0 also
   when nothing exists: each repository's status says whether the stanza's
   path is missing (1), the stanza has no backup yet (2), its data is
   missing (3), or it is fine (0).
2. A stanza that is missing in a repository is created with `pgbackrest
   stanza-create`; one whose newest PostgreSQL version differs from
   `PG_VERSION` in `pg1-path` is upgraded with `pgbackrest stanza-upgrade`.
   Both need the primary: a `standby.signal` or `recovery.signal` in
   `pg1-path` says that the cluster is in recovery.
3. After a creation or an upgrade the module runs `pgbackrest check`,
   which switches the write-ahead log and waits until its segment reaches
   every repository. A successful check proves that `archive_command`
   sends the write-ahead log segment to every repository.
4. An unchanged stanza runs no `check`.

## Errors and what they mean

Every reason starts with `pgbackrest_stanza`.

| The reason holds | What it means, and what to do |
|---|---|
| `the program pgbackrest is not installed; install the package pgbackrest in an earlier node task` | Install `pgbackrest`, as in [Set up](#set-up). A run fails without another try. |
| `the configuration file FILE does not exist` | Write the configuration file named by `config_file` with a `file` task in an earlier node task. |
| `the configuration file FILE gives the stanza NAME no pg1-path` | Add `pg1-path` to the stanza's section. |
| `DIRECTORY, the stanza's pg1-path, holds no PostgreSQL cluster` | The cluster is missing or `pg1-path` is wrong. |
| `the PostgreSQL cluster in DIRECTORY is in recovery (standby.signal), and pgBackRest creates a stanza only on the primary` | Run the task on the primary. A run fails without another try. |
| `the stanza NAME has the archive's information and no backup information in a repository (missing stanza data, pgBackRest's error 55)` | A `stanza-create` stopped partway, or the backup information was deleted. A stanza without backups can be deleted and created again, as the reason says. A repository whose backups an attacker deleted needs `pgbackrest_restore` with `read_repository_as_of`, not a new stanza. |
| `created the stanza NAME, and then pgbackrest check failed, so archiving does not reach the repository: ...` | The stanza exists, and `archive_command` does not work. MMX does not try the task again: a new try would find the stanza and run no check. Act on pgBackRest's message, such as an `archive_command` that names another stanza, and run `pgbackrest check` by hand. |
| `creating the stanza ...`, `upgrading the stanza ...`, `reading the stanza ...` | A command of `pgbackrest` failed; the text behind the colon is pgBackRest's message with its hints. MMX tries the task again. |

## Tested on

**Distributions:** Debian 12 and 13, Ubuntu 24.04 and 26.04.

The integration suite `tests/integration/pgbackrest.py` runs the tasks of
[Set up](#set-up) with a pg_auto_failover node as the database, in system
containers on x86_64:

| Distribution | pgBackRest | PostgreSQL | Repository |
|---|---|---|---|
| Debian 12 | 2.45 | 15 | a directory on the database server |
| Ubuntu 24.04 | 2.50 | 16 | a directory on the database server |
| Debian 13 | 2.55 | 17 | an S3-compatible bucket that keeps versions |
| Ubuntu 26.04 | 2.58 | 18 | an S3-compatible bucket that keeps versions |

The bucket is `python3-moto`'s server behind nginx on another test server;
Debian 12 and Ubuntu 24.04 do not package `python3-moto`.

- **Not tested:** `stanza-upgrade` after a major upgrade, a stanza on a
  replica, and several repositories at once.

## Why it works this way

- **`check` runs only after a creation or an upgrade.** `check` switches
  and archives a segment of the write-ahead log, which every run of every
  database server would repeat.
- **The stanza is read through `pgbackrest info --output=json`.** It ends
  with exit code 0 also when nothing exists, and its status codes say which
  of the cases it is, without reading pgBackRest's words.
- **A stanza that stopped partway fails the task.** `stanza-create` then
  refuses with pgBackRest's error 55, and the same state follows when an
  attacker deleted the backup information: the module does not guess which
  of the two it is.
- **The backup jobs run on every database server.** After a failover the
  new primary backs up, and the job on a replica does nothing.
