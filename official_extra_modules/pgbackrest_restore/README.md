# `pgbackrest_restore`

`pgbackrest_restore` restores a PostgreSQL cluster with
[pgBackRest](https://pgbackrest.org/) into an empty or missing data
directory, to the end of the archive or to a chosen time, starts it, and
waits until its recovery has ended. pgBackRest keeps backups and the
archived write-ahead log, the record of every change, of a *stanza*, one
PostgreSQL cluster, in a *repository*;
[`pgbackrest_stanza`](../pgbackrest_stanza/README.md) creates the stanza.

The module runs `pgbackrest`, `pg_ctl`, and `psql` as the account
`postgres` on the server that receives the restore, which is a **node** of
MMX: a Linux server that MMX manages, such as a new server after every
database server was lost, or a server for a restore drill. It never writes
over files: a directory that holds anything fails the task, except the
module's own completed restore. MMX records the target of that restore on
the node, so a later run with the same target is unchanged and one with
another target fails, instead of keeping the old restore without a word. It
restores a cluster of pg_auto_failover, whose configuration is in its data
directory, and a cluster of Debian's own tools, whose configuration is in
`/etc/postgresql`.

## Set up

The server needs PostgreSQL's programs of the cluster's version, without a
cluster, and `pgbackrest` with the same configuration file as the server
that was backed up. For a cluster of pg_auto_failover, the server gets the
packages of the
[pg_auto_failover set-up](../pg_auto_failover_monitor/README.md#set-up)
and `pgbackrest`; the restored cluster then becomes the formation's
primary with a [`pg_auto_failover_node`](../pg_auto_failover_node/README.md)
task:

```yaml
# mmx run restore_the_database: a new server db11, after the monitor's task
# dropped the lost database servers
fleet_stages:
  - id: restore
    nodes: [db11]
    node_tasks:
      - directory: {path: /etc/postgresql-common, permissions_mode: "0755"}
      - file:
          path: /etc/postgresql-common/createcluster.conf
          content: "create_main_cluster = false\n"
          permissions_mode: "0644"
      - apt_package:
          packages:
            - {name: postgresql-18, version: newest_when_first_installed}
            - {name: postgresql-18-auto-failover, version: newest_when_first_installed}
            - {name: pg-auto-failover-cli, version: newest_when_first_installed}
            - {name: pgbackrest, version: newest_when_first_installed}
      - directory: {path: /var/lib/postgresql/18, owner: postgres, group: postgres, permissions_mode: "0755"}
      - directory: {path: /etc/pgbackrest, permissions_mode: "0755"}
      - file:
          path: /etc/pgbackrest/pgbackrest.conf
          template: templates/pgbackrest.conf
          owner: root
          group: postgres
          permissions_mode: "0640"
          secret_readable_by_group: true   # the group postgres reads the bucket's key

      - pgbackrest_restore:
          stanza: main
          config_file: /etc/pgbackrest/pgbackrest.conf
          data_directory: /var/lib/postgresql/18/main
          postgresql_major_version: 18
          recover_to: latest
        time_limit: 2h

      - pg_auto_failover_node:
          name: db11
          data_directory: /var/lib/postgresql/18/main
          postgresql_major_version: 18
          port: 5432
          host_name: 10.20.0.41
          listen_address: 10.20.0.41
          monitor_host_name: 10.20.0.31
          monitor_port: 5432
          autoctl_node_password: "{{ .secrets.autoctl_node_password }}"
          replication_password: "{{ .secrets.pgautofailover_replicator_password }}"
          formation: default
          candidate_priority: 50
          replication_quorum: true
```

The new server need not have the lost server's address. `pg_autoctl` is
pg_auto_failover's program for creating and managing PostgreSQL instances.
pg_autoctl writes the node's `listen_address` into the data directory's
`postgresql-auto-failover.conf`. The restore brings back the lost server's
address in `postgresql-auto-failover.conf`, so the module starts the
restored cluster on the local socket only.

The `pg_auto_failover_node` task replaces the lost server's address in
`postgresql-auto-failover.conf` with the new server's `listen_address`.
pg_autoctl then starts PostgreSQL at the new server's address.

**A cluster of Debian's own tools.** Installing `postgresql-18` creates the
cluster `main`, with its configuration in `/etc/postgresql/18/main/` and its
data in `/var/lib/postgresql/18/main`, and `pg_lsclusters` lists it. The
module restores into that data directory and starts the cluster with its
service, `postgresql@18-main.service`. The data directory must be empty, so
earlier node tasks stop the cluster and empty it; the module itself never
empties a directory. Those two tasks must run only once: a later run would
stop the restored database and delete it. A marker file after the restore
says that it was done, and the tasks run only while it is missing. The work
fits a [one-time playbook](../../docs/how-to/make-a-one-time-change.md):

```yaml
# one_time_playbooks/2026-11-03_restore_the_shop_database.yml: the server
# db21 has postgresql-18 and pgbackrest, and the configuration file that
# names the stanza main with pg1-path=/var/lib/postgresql/18/main
fleet_stages:
  - id: restore_the_shop_database
    nodes: [db21]
    node_tasks:
      - when:
          not: {path_exists: /var/lib/postgresql/18/main_restored_from_backup}
        systemd_service:
          name: postgresql@18-main
          state: stopped
      - when:
          not: {path_exists: /var/lib/postgresql/18/main_restored_from_backup}
        directory:
          path: /var/lib/postgresql/18/main
          delete_everything_inside_except: []

      - pgbackrest_restore:
          stanza: main
          config_file: /etc/pgbackrest/pgbackrest.conf
          data_directory: /var/lib/postgresql/18/main
          postgresql_major_version: 18
          recover_to: latest
        time_limit: 2h

      - file:
          path: /var/lib/postgresql/18/main_restored_from_backup
          content: "pgbackrest_restore restored the cluster main.\n"
          permissions_mode: "0644"
```

The configuration in `/etc/postgresql/18/main/` stays the receiving
server's own: pgBackRest backs up and restores the data directory only. The
restore writes its settings into the data directory's
`postgresql.auto.conf`, which PostgreSQL reads after `postgresql.conf` and
its `conf.d` files, so `archive_mode = off` there wins over archiving
settings in `/etc/postgresql`.

The restored cluster archives nothing: the module restores it with
`archive_mode = off` in its `postgresql.auto.conf`. When it replaces the
production cluster for good, turn archiving on again with `ALTER SYSTEM
RESET archive_mode` and a restart of its service; pgBackRest then archives
the restored cluster's new timeline into the same stanza.

## Inputs

| Input | Meaning | When omitted |
|---|---|---|
| `stanza` | The stanza to restore, as in the configuration file, such as `main`. | Required. |
| `config_file` | The configuration file that `pgbackrest` reads, such as `/etc/pgbackrest/pgbackrest.conf`. | Required. |
| `data_directory` | Where the restored cluster goes: a missing directory, whose parent exists, or an empty one. Use the same path as on the server that was backed up: pg_autoctl writes absolute paths, such as its TLS files, into the configuration that the restore brings back. | Required. |
| `postgresql_major_version` | The PostgreSQL version whose programs restore and start the cluster, such as `18`; it must be the stanza's version. | Required. |
| `recover_to` | `latest`, to replay the whole archive, or a time with its time zone, such as `"2026-11-03 13:01:00+00"`, to stop there (`--type=time --target-action=promote`). The time is `YYYY-MM-DD HH:MM`, optional seconds, and `+HH`, `-HH`, `+HH:MM`, or `Z`. | Required. |
| `read_repository_as_of` | A time with its time zone, in the same form. pgBackRest reads the repository as it stood then (`--repo-target-time`), so files that were deleted or overwritten later, such as by an attacker, do not matter in a bucket that keeps versions. Needs pgBackRest 2.54 or newer and `repository`. | pgBackRest reads the repository as it is. |
| `repository` | The number of the repository in the configuration file, such as `1` for `repo1-*`. | Required with `read_repository_as_of`; without both, pgBackRest chooses the repository. |

## Outputs

| Output | Meaning |
|---|---|
| `restored` | `true` when the task restored the cluster. In a dryrun: `true` when a run would restore the cluster. |

## What a dryrun reports

A dryrun reads the stanza with `pgbackrest info --output=json`, looks at
the data directory, and changes nothing.

- **A restore to do:** `would_change`, `would restore the stanza main into
  DIRECTORY to TIME from the backup 20261102-010001F; would start the
  restored cluster and wait until its recovery ends`. The backup is the
  newest that ended before the time, or the newest one for `latest`. With
  `read_repository_as_of`, the dryrun cannot read the repository as it
  stood then, and says `reading the repository 1 as it stood at TIME`
  instead of a backup.
- **The module's completed restore:** `unchanged` for the same stanza,
  `recover_to`, and `read_repository_as_of`; for another target a failure,
  as in a run. A completed restore that MMX has no record of, such as one
  by hand: `unchanged`, and the reason says that the module cannot compare
  it with the task.
- **A restored cluster whose recovery has not ended:** `would start the
  restored cluster ...` while it does not run, with its service for a
  cluster of Debian's tools; while it runs and replays, `unchanged`, and a
  run waits.
- **An earlier node task that changes the data directory,** such as the
  `directory` task that empties it: `not_checked`, because the dryrun sees
  the directory as it is before that task.
- **`not_checked`** while the programs are missing, while the configuration
  file is missing or an earlier node task of the dryrun writes it, while
  the stanza has no backup, while the parent of the data directory is
  missing, and for `read_repository_as_of` with pgBackRest older than 2.54.
- **A failure, as in a run,** for an input that breaks a rule, a data
  directory that holds anything else, a stanza of another PostgreSQL
  version, and a time before the end of every backup.

## Examples

Restore to 13:01 UTC, before a mistake at 13:02:

```yaml
- pgbackrest_restore:
    stanza: main
    config_file: /etc/pgbackrest/pgbackrest.conf
    data_directory: /var/lib/postgresql/18/main
    postgresql_major_version: 18
    recover_to: "2026-11-03 13:01:00+00"
  time_limit: 2h
```

Restore the whole archive of the bucket as it stood at noon, before an
attacker deleted the backups from it:

```yaml
- pgbackrest_restore:
    stanza: main
    config_file: /etc/pgbackrest/pgbackrest.conf
    data_directory: /var/lib/postgresql/18/main
    postgresql_major_version: 18
    recover_to: latest
    read_repository_as_of: "2026-11-03 12:00:00+00"
    repository: 1
  time_limit: 2h
```

## How it works

1. The module looks at the data directory. A missing one is created, owned
   by `postgres` with mode `0700`. An empty one is used. One that holds
   pgBackRest's `backup.manifest` holds a restore that stopped partway; one
   that holds anything else, without a cluster that pgBackRest restored
   from the stanza, fails the task.
2. It runs `pgbackrest restore --pg1-path=DIRECTORY --archive-mode=off`,
   with `--type=time --target=TIME --target-action=promote` for a time, and
   `--repo` and `--repo-target-time` for `read_repository_as_of`. The module
   never passes `--delta`, which writes over an existing cluster.
3. The module records the restore's target: `stanza`, `recover_to`, and
   `read_repository_as_of`. The record is
   `/var/lib/mmx/pgbackrest_restore_targets/<SHA-256 of the data
   directory's path>.json`, a file of `root` with mode `0600` that also
   holds the data directory's path.
4. It starts the cluster and waits until `pg_is_in_recovery()` is false,
   at most six hours; the result says how long the restore and the replay
   took. A cluster of Debian's tools, which `pg_lsclusters --json` lists
   with the data directory, starts with `systemctl start
   postgresql@<version>-<cluster>.service`. Any other cluster starts with
   `pg_ctl start --options "-c listen_addresses=''"`: on its local socket
   only, whatever addresses its restored configuration names, and the wait
   asks through that socket. Check a restore drill with `psql` on the
   socket, such as `runuser -u postgres -- psql -p 5432`.
5. A cluster that pgBackRest restored from the stanza, whose
   `postgresql.auto.conf` holds pgBackRest's `restore_command` and whose
   `recovery.signal` is gone, is the module's completed restore. A later
   run compares the task's target with the record: the same target is
   unchanged; another one fails the task, which names both targets. To
   restore again, stop the cluster, empty the data directory, and run the
   task. Without a record the run is unchanged, and the reason says that
   the module cannot compare. One whose `recovery.signal` is still there,
   as after a run that ended during the replay, is compared in the same
   way, then started where it does not run, and waited for.

## Errors and what they mean

Every reason starts with `pgbackrest_restore`.

| The reason holds | What it means, and what to do |
|---|---|
| `the program ... is not installed; install the package ...` | Install the packages of [Set up](#set-up). A run fails without another try. |
| `DIRECTORY holds files (...) and no PostgreSQL cluster`, `DIRECTORY holds a PostgreSQL cluster that pgBackRest did not restore from the stanza NAME` | The module restores only into an empty or missing directory. Choose another directory, or empty this one by hand after checking what it holds. |
| `DIRECTORY holds a restore that stopped partway (pgBackRest's backup.manifest)` | A restore stopped during the copy. Empty the directory, and run the task again. |
| `DIRECTORY holds the module's restore of the stanza NAME to TARGET, and the task asks for ...` | The task's `stanza`, `recover_to`, or `read_repository_as_of` differs from the module's restore in the directory. The module never writes over a cluster. To restore to the new target, stop the cluster, empty the directory after checking what it holds, and run the task again. For a cluster of Debian's tools, the guarded tasks of [Set up](#set-up) stop and empty the cluster only while the marker file is missing. A dryrun also fails when the task's target differs from the recorded restore's target. |
| `MMX's record of the restore into DIRECTORY, PATH, holds no target of that directory` | The record in `/var/lib/mmx/pgbackrest_restore_targets/` is not one that the module wrote. Remove the file to make the module treat the restore as one without a record. |
| `the stanza NAME has no backup in its repositories` | The repository holds no backup of the stanza, or its backups were deleted. With a bucket that keeps versions, read it as it stood before with `read_repository_as_of`. |
| `the stanza NAME has no backup that ended before TIME` | No restore reaches that time: choose a later one. |
| `the stanza NAME holds PostgreSQL VERSION, and postgresql_major_version is ...` | Install and name the stanza's version. |
| `pgBackRest VERSION is installed, and read_repository_as_of needs pgBackRest 2.54 or newer` | Install a newer `pgbackrest`, such as Debian 13's or Ubuntu 26.04's. |
| `restoring the stanza ...: ... empty it before the next run` | `pgbackrest restore` failed; the text is pgBackRest's message. MMX does not try the task again, because the directory now holds files. |
| `... and then this failed: the restored cluster in DIRECTORY stopped during its recovery; read ...` | PostgreSQL ended during the replay, such as at a time that the archive does not reach. Read what the reason names: `DIRECTORY/startup.log` and the files in `DIRECTORY/log`, or for a cluster of Debian's tools the journal of its service and its log file in `/var/log/postgresql`. |
| `starting the restored cluster in DIRECTORY with postgresql@VERSION-CLUSTER.service: ...` | systemd did not start the cluster's service; the text is `systemctl`'s message. |
| `the restored cluster in DIRECTORY is still in recovery after 6 hours; the next run waits again` | The replay takes longer. The next run waits again for the restored cluster in `DIRECTORY`. |

## Tested on

**Distributions:** Debian 12 and 13, Ubuntu 24.04 and 26.04.

The integration suite `tests/integration/pgbackrest.py` loses a
pg_auto_failover node's server by hand. It restores into the emptied data
directory to a time, with the cluster on its local socket only, refuses a
rerun with another target and a directory that holds files, restores the
whole archive onto another test server, which lacks the lost server's
address that the restored settings name, and registers that cluster as the
formation's new primary at its own `listen_address`. It also backs up a
cluster of Debian's tools on one test server and restores it on another
one into that server's own cluster, which the tasks of [Set up](#set-up)
stop and empty, in system containers on x86_64:

| Distribution | pgBackRest | PostgreSQL | Repository | `read_repository_as_of` |
|---|---|---|---|---|
| Debian 12 | 2.45 | 15 | a directory | a missing prerequisite that names the version |
| Ubuntu 24.04 | 2.50 | 16 | a directory | a missing prerequisite that names the version |
| Debian 13 | 2.55 | 17 | an S3-compatible bucket that keeps versions | restores after the backup was deleted from the bucket |
| Ubuntu 26.04 | 2.58 | 18 | an S3-compatible bucket that keeps versions | restores after the backup was deleted from the bucket |

The bucket is `python3-moto`'s server behind nginx on another test server.

- **Not tested:** other recovery types than a time and the end of the
  archive, and encrypted repositories.

[Limits on each distribution](../../docs/reference/limits-on-each-distribution.md) says what is limited for this module on [Debian 12](../../docs/reference/limits-on-each-distribution.md#debian-12) and [Ubuntu 24.04](../../docs/reference/limits-on-each-distribution.md#ubuntu-2404), and why.

## Why it works this way

- **Only an empty or missing data directory, and never `--delta`.** A
  restore writes over a cluster; the task must never destroy a database
  that runs. A new server or an emptied directory is what a recovery and a
  restore drill use.
- **`--archive-mode=off`.** The restored cluster has the production
  cluster's system ID. Without `--archive-mode=off`, the restored cluster's
  `archive_command` would push a new timeline into the production stanza
  while production still runs.
- **The module waits until the cluster has left recovery.** `pg_ctl start`
  returns while the replay runs, and a query then sees an older database: a
  table that the restore brings back was missing.
- **`read_repository_as_of` needs `repository`.** pgBackRest reads one
  repository as it stood at a time, and refuses the option without
  `--repo`.
- **The same data directory path as the source.** pg_autoctl's settings in
  the restored cluster name its TLS files by absolute path.
- **The local socket only, for a cluster that `pg_ctl` starts.** The
  restored configuration names the addresses of the server that was backed
  up: pg_autoctl writes the node's `listen_address` into the data
  directory's `postgresql-auto-failover.conf`. On another server, without
  that address, PostgreSQL ended at its start because it could not bind
  it. A setting on `pg_ctl`'s command line wins over the configuration
  files, and the socket is enough for the wait and for a drill's checks.
- **A record of each restore's target, outside the data directory.**
  Without a record of the restore's target, a rerun with another `recover_to`
  reported `unchanged` and kept the old restore. A restore record inside
  the data directory would be copied to every replica by `pg_basebackup`.
  pgBackRest would also include the restore record in backups of the data
  directory. MMX keeps its own records on a node in
  `/var/lib/mmx/`. Another target never replaces the restore, because the
  module never writes over a cluster.
- **A cluster of Debian's tools starts through its service.** The Debian
  cluster's `postgresql.conf` is in `/etc/postgresql`. On the test servers,
  `pg_ctl start --pgdata` failed with "could not access the server
  configuration file". The service also keeps the cluster under systemd
  afterwards.
  `pg_lsclusters` lists the cluster by its configuration, also while its
  data directory is empty or missing.
- **The module never empties a directory.** Stopping a cluster and
  deleting its data are the playbook's own, visible node tasks, guarded so
  that they run once.
