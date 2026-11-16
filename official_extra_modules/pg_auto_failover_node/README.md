# `pg_auto_failover_node`

`pg_auto_failover_node` manages one PostgreSQL node of a
[pg_auto_failover](https://pg-auto-failover.readthedocs.io/) formation.
pg_auto_failover keeps a PostgreSQL primary and its replicas in one
*formation* and promotes a replica when the primary fails; a *monitor* on a
server of its own decides, and
[`pg_auto_failover_monitor`](../pg_auto_failover_monitor/README.md)
manages it. Each database server runs a *keeper*, `pg_autoctl run`, which
manages that server's PostgreSQL: the server is a *node* of the formation.

The module runs `pg_autoctl` and `psql` as the account `postgres` on the
database server, which is a **node** of MMX too: a Linux server that MMX
manages. The module manages:

- the node's pg_autoctl instance and PostgreSQL cluster;
- the systemd unit `pgautofailover.service`;
- two logins in the password file of `postgres`;
- the node's failover settings and maintenance;
- the address where the node's PostgreSQL listens;
- the local login rules in the data directory's `pg_hba.conf`;
- with `postgresql_settings`, the node's PostgreSQL settings, such as
  `shared_buffers`.

The first node of a formation becomes its primary. A later node copies the
primary and joins as a replica.

## Set up

The database servers need the packages of the monitor's
[Set up](../pg_auto_failover_monitor/README.md#set-up): its first fleet
stage runs on every server of the formation, and its second one creates
the monitor. The database servers' fleet stage follows:

```yaml
# mmx run configure, after the fleet stages set_up and monitor
fleet_stages:
  - id: database_servers
    nodes: database_servers
    # One node at a time: the first becomes the primary, and each later one
    # copies it.
    max_nodes_at_once: 1
    node_tasks:
      - pg_auto_failover_node:
          name: "{{ .node.name }}"
          data_directory: "/var/lib/postgresql/{{ .vars.postgresql_major_version }}/main"
          postgresql_major_version: "{{ .vars.postgresql_major_version }}"
          port: 5432
          host_name: "{{ .node.network.default_ipv4_address }}"
          listen_address: "{{ .node.network.default_ipv4_address }}"
          monitor_host_name: 10.20.0.31
          monitor_port: 5432
          autoctl_node_password: "{{ .secrets.autoctl_node_password }}"
          replication_password: "{{ .secrets.pgautofailover_replicator_password }}"
          formation: default
          candidate_priority: 50
          replication_quorum: true
          postgresql_settings:
            # A quarter of each server's own memory, in steps of 128 MiB, and
            # at least 128 MiB: 2048MB on a server with 8 GiB.
            shared_buffers: {percent_of_memory: 25, rounded_down_to_mib: 128, at_least_mib: 128}
            work_mem: 64MB
          # A replica restarts for shared_buffers; the primary's task refuses
          # the restart until another node is the primary.
          restart_postgresql_for_new_settings: on_a_replica
        # A node that joins copies the primary with pg_basebackup, which
        # first waits for a checkpoint on the primary: up to two minutes on
        # the test servers. The task also waits for the formation to settle,
        # at most five minutes at its start and five at its end.
        time_limit: 20m
```

**An address on a WireGuard interface.** When `host_name` and
`listen_address` are the server's address on a WireGuard network, such as
`wg0` of [`wireguard_interface`](../../docs/reference/modules/wireguard_interface.md),
PostgreSQL can bind that address only once the interface is up. A drop-in
of `pgautofailover.service` starts the service after
`wg-quick@wg0.service` at boot, on the monitor's server as on each database
server. Place the following `systemd_unit` task after the
`pg_auto_failover_monitor` or `pg_auto_failover_node` task, which writes
the unit that the drop-in changes. Adding the drop-in needs no restart of
the running service:

```yaml
- systemd_unit:
    name: pgautofailover.service
    drop_in: 10-after-wireguard.conf
    content: |
      [Unit]
      After=wg-quick@wg0.service
      Wants=wg-quick@wg0.service
```

pg_autoctl tunes PostgreSQL from the server's memory, such as
`shared_buffers`, in its own file, which the data directory's
`postgresql.conf` includes at its top. The settings of
`postgresql_settings` come later and win: the module keeps them in
`/etc/postgresql/pg_auto_failover_node_settings.conf`, which
`postgresql.conf` includes at its end. A setting that a `line` task appends
to `postgresql.conf` works too, as the page of
[`pgbackrest_stanza`](../pgbackrest_stanza/README.md#set-up) shows for the
archiving settings; for a setting in both, the module's value wins, since
the module keeps its line last.

## Inputs

| Input | Meaning | When omitted |
|---|---|---|
| `data_directory` | The node's PostgreSQL data directory, an absolute path such as `/var/lib/postgresql/18/main`. The data directory's parent directory must belong to `postgres`. | Required. |
| `state` | `present`, `in_maintenance`, `absent`, or `deleted_with_all_its_data`. `in_maintenance` puts a replica into maintenance, and `present` ends the maintenance. `absent` stops the node's service, removes the service's unit, then drops the node from its formation. The data directory and pg_autoctl's configuration stay. `deleted_with_all_its_data` also deletes the data directory and pg_autoctl's configuration (`pg_autoctl drop node --destroy`). The two removals accept only `data_directory`. | `present`. |
| `name` | The node's name in the formation, such as the inventory name `db01`. A dropped node's name cannot be used again at once; a replaced server gets a new name. | Required for `present` and `in_maintenance`. |
| `postgresql_major_version` | The PostgreSQL version whose programs run the node, such as `18`. | Required for `present` and `in_maintenance`. |
| `port` | The port of the node's PostgreSQL, such as `5432`. | Required for `present` and `in_maintenance`. |
| `host_name` | The host name or IP address with which the other nodes and the monitor reach this node. | Required for `present` and `in_maintenance`. |
| `listen_address` | The IP address of the server on which the node's PostgreSQL listens, such as `10.20.0.21`, or `*` for every address. `host_name` is the address that the other nodes and the monitor connect to, and `listen_address` is where PostgreSQL listens: usually the same private address, so that the database is not reachable at the server's other addresses. A changed address takes a restart of `pgautofailover.service`, which on a primary with replicas makes the monitor fail over: switch the primary first. | Required for `present` and `in_maintenance`. |
| `monitor_host_name`, `monitor_port` | Where the monitor answers, as its `pg_auto_failover_monitor` task gives `host_name` and `port`. | Required for `present` and `in_maintenance`. |
| `autoctl_node_password` | The password of the monitor's login `autoctl_node`, a secret such as `"{{ .secrets.autoctl_node_password }}"`. | Required for `present` and `in_maintenance`. |
| `replication_password` | The password of `pgautofailover_replicator`, with which a replica copies from the primary; the same on every node of the formation. | Required for `present` and `in_maintenance`. |
| `formation` | The formation the node joins, such as `default`. | Required for `present` and `in_maintenance`. |
| `candidate_priority` | A whole number from 0 to 100. The monitor promotes the healthy replica with the highest priority; 0 means never. | Required for `present` and `in_maintenance`. |
| `replication_quorum` | `true` makes a write on the primary wait for this node while it is a healthy replica: synchronous replication. `false` lets writes go on without it. | Required for `present` and `in_maintenance`. |
| `postgresql_settings` | The node's PostgreSQL settings, as a mapping of each setting's name to its value, such as `work_mem: 64MB`. A name is PostgreSQL's own, in lower case, and may hold one dot, such as `pg_stat_statements.max`. A value is a text, a number, `true` or `false`, which the module writes as `on` or `off`, or a share of the server's memory: `{percent_of_memory: 25, rounded_down_to_mib: 128, at_least_mib: 128}`, as in [Set up](#set-up), is a quarter of the server's memory in MiB, rounded down to a multiple of 128, and at least 128, written such as `2048MB`. The module reads the memory from `MemTotal` in `/proc/meminfo`, as MMX's fact `memory.total_mib` does. A value may also be a reference, such as `"{{ .vars.work_mem }}"`. The module writes the settings into its file `/etc/postgresql/pg_auto_failover_node_settings.conf`, and its values win over pg_autoctl's. `{}` empties the file. The module refuses a few settings that pg_autoctl or pg_auto_failover keep, such as `port`: see [Why it works this way](#why-it-works-this-way). | The module leaves its file alone. |
| `restart_postgresql_for_new_settings` | What the task does with a setting that takes effect only when PostgreSQL restarts, such as `shared_buffers`. `never` only reloads PostgreSQL: the setting waits for a restart, and the output `postgresql_settings_waiting_for_a_restart` and the reason name it. `on_a_replica` restarts PostgreSQL on a replica, through maintenance. `on_a_replica_or_a_node_alone` also restarts a node that is alone in its formation, in place. The task never restarts the primary of a formation with replicas, which would be a failover: it refuses. A node that the task creates restarts with any value, since it serves nobody yet. | Required with `postgresql_settings`. |

**The two passwords** are private values: a secret in either does not hide
the task's result. The module keeps them in the password file of
`postgres`, `~postgres/.pgpass`, mode `0600`, where the keeper and the
replica's PostgreSQL find them: `*:*:pg_auto_failover:autoctl_node:...`
and `*:*:*:pgautofailover_replicator:...`, with a backslash before each
colon and backslash of a password. The file's other lines stay as they
are. The file reaches `install` through its standard input, so neither the
passwords nor the file's other lines are in a command line or the result;
`install` writes it beside the old one as `.pgpass.mmx-new`, and `mv`
renames it into place.
On the primary, the replication password reaches `psql`'s `\password`
through standard input. The `psql` command sends the server only a
SCRAM-SHA-256 password verifier, the hashed form stored by PostgreSQL.

## Outputs

| Output | Meaning |
|---|---|
| `created` | `true` when the task created the node. In a dryrun: `true` when a run would create the node. |
| `current_state` | The node's state as the monitor reports it after the task, such as `single`, `primary`, `secondary`, `catchingup`, or `maintenance`. |
| `is_primary` | `true` while the monitor has made the node its formation's primary: its assigned state is `single`, `wait_primary`, `primary`, `join_primary`, or `apply_settings`. After a failover, the old primary may still report `primary` itself; `is_primary` is then `false`. |
| `changed_postgresql_settings` | With `postgresql_settings`: the sorted names of the settings whose value in the module's file the task changed, added, or removed, such as `[shared_buffers]`. |
| `restarted_postgresql` | With `postgresql_settings`: `true` when the task restarted the node's PostgreSQL for its settings, or restarted `pgautofailover.service` because no PostgreSQL ran. |
| `postgresql_settings_waiting_for_a_restart` | With `postgresql_settings`: the sorted names of settings in the module's file before or after the task whose values still wait for a restart of PostgreSQL. Settings outside the module's file do not count. |

A later node task reads them as `.node_tasks.<id>.is_primary`. A dryrun
that would create the node, or start its service, does not know
`current_state` and `is_primary`, because the monitor assigns the node its
state only then: a later node task that reads them is `not_checked`. A
dryrun gives `changed_postgresql_settings` as a run would change them, and
does not know `restarted_postgresql` and
`postgresql_settings_waiting_for_a_restart`.

## What a dryrun reports

A dryrun runs `pg_autoctl`'s and `psql`'s read commands and changes nothing.

- **A node to create:** `would_change`, with `would write the logins of
  autoctl_node and pgautofailover_replicator into the password file ...;
  would create the node db01 in DIRECTORY in the formation default; would
  write the unit ... and start it`, and `created: true`, without
  `current_state` and `is_primary`.
- **A setting that differs:** `would set candidate_priority of the node
  db01 to 70`, and the same for `replication_quorum`.
- **Maintenance:** `would put the node db01 into maintenance`, or `would end
  the maintenance of the node db01`.
- **New passwords:** `would write the logins ... into the password file`,
  and on the primary `would set the password of pgautofailover_replicator`.
- **initdb's trust lines:** `would replace trust in DIRECTORY/pg_hba.conf
  with peer (local all all, local replication all) and scram-sha-256
  (...)`, with each line that the module would change, and `would reload
  PostgreSQL in DIRECTORY`.
- **Another listen address:** `would stop pgautofailover.service, set
  postgresql.listen_addresses of the instance in DIRECTORY from * to
  10.20.0.21, and start the service again`.
- **New PostgreSQL settings:** `would set work_mem from 4MB to 64MB in
  /etc/postgresql/pg_auto_failover_node_settings.conf`, `would remove ...
  from ...` for a setting that the task no longer names, `would add the
  line include_if_exists = '...' at the end of DIRECTORY/postgresql.conf`
  where it is missing, and `would reload PostgreSQL in DIRECTORY`. A
  setting whose `context` in `pg_settings` is `postmaster` takes effect
  only with a restart: `would restart PostgreSQL on the replica db02
  through maintenance for shared_buffers`, or with `never`
  `shared_buffers would wait for a restart of PostgreSQL; ...`.

  The dryrun uses `postgres -C` to convert the requested text to the
  setting's PostgreSQL unit, then compares against `pg_settings.setting`.
  A changed setting needs a restart only when the converted value differs
  from the running value. For example, `131072kB` equals initdb's `128MB`,
  so requesting `131072kB` does not need a restart.
  Restoring the running value also needs no restart, even when the
  configuration file currently requests a different value pending restart.

  An unchanged setting in the module's settings file may still be waiting
  for a restart after an earlier run. The dryrun names the unchanged
  setting when reporting the required PostgreSQL restart. While no
  PostgreSQL runs on the node: `would restart pgautofailover.service so that
  PostgreSQL starts with them`.
- **A dryrun does not have PostgreSQL check the settings,** and does not
  look for another file that sets one of them again: a run does both, after
  it wrote the file, and fails where PostgreSQL refuses a value that its
  dryrun reported as `would set`.
- **`not_checked`** applies under any of the following conditions:
  - `pg_autoctl`, the PostgreSQL programs, or the account `postgres` are
    missing.
  - The data directory's parent directory does not belong to `postgres`.
  - A cluster managed by Debian's tools uses the requested port.
  - The monitor does not answer.
  - The formation has not settled: `the formation FORMATION is not settled: ...;
    a run waits for it, at most 300 seconds`.
  - The task requests `in_maintenance` or a new `listen_address` on the
    primary.
  - The settings require a PostgreSQL restart on the primary of a
    formation with replicas.
  - The settings require a PostgreSQL restart on a node alone, and
    `restart_postgresql_for_new_settings` is `on_a_replica`.
  - The data directory holds a PostgreSQL cluster, and the formation
    already has nodes.
- **A failure, as in a run,** applies under any of the following conditions:
  - An input breaks a rule.
  - The task requests a setting that the module refuses.
  - A setting that a replica needs at least as high as the primary's
    value is lower than the primary's value.
  - The task requests too few WAL senders or replication slots for the
    formation.
  - The data directory holds a monitor.
  - The server's unit runs another instance of `pg_autoctl`.
  - An existing node's name, formation, host name, port, or monitor differs
    from the task's input.
  - The monitor dropped the node.

## Examples

A database server that is a replica in normal times, and that the monitor
promotes first when the primary fails. It gives PostgreSQL 2 GiB for
`shared_buffers`, a quarter of the server's 8 GiB, and the statistics of
`pg_stat_statements`, which pg_autoctl loads, room for more queries:

```yaml
- pg_auto_failover_node:
    name: db02
    data_directory: /var/lib/postgresql/18/main
    postgresql_major_version: 18
    port: 5432
    host_name: 10.20.0.22
    listen_address: 10.20.0.22
    monitor_host_name: 10.20.0.31
    monitor_port: 5432
    autoctl_node_password: "{{ .secrets.autoctl_node_password }}"
    replication_password: "{{ .secrets.pgautofailover_replicator_password }}"
    formation: default
    candidate_priority: 90
    replication_quorum: true
    postgresql_settings:
      shared_buffers: 2GB
      pg_stat_statements.max: 10000
      log_lock_waits: true
    restart_postgresql_for_new_settings: on_a_replica
  time_limit: 10m
```

On the replica `db02`, a new `shared_buffers` restarts PostgreSQL through
maintenance. While `db02` is the primary, as after a failover, the task
refuses that restart, with a reason that says to make another node the
primary first.

The following task puts the replica `db01` into maintenance, for example,
for a kernel update. The primary does not wait for `db01` during
maintenance, so writes continue. Run the following `db01` task with
`state: present` to end maintenance. After a reboot in maintenance,
`db01`'s PostgreSQL stays stopped until `state: present` ends maintenance.

```yaml
- pg_auto_failover_node:
    name: db01
    state: in_maintenance
    data_directory: /var/lib/postgresql/18/main
    postgresql_major_version: 18
    port: 5432
    host_name: 10.20.0.21
    listen_address: 10.20.0.21
    monitor_host_name: 10.20.0.31
    monitor_port: 5432
    autoctl_node_password: "{{ .secrets.autoctl_node_password }}"
    replication_password: "{{ .secrets.pgautofailover_replicator_password }}"
    formation: default
    candidate_priority: 50
    replication_quorum: true
```

Remove the node from its formation and keep its data:

```yaml
- pg_auto_failover_node:
    data_directory: /var/lib/postgresql/18/main
    state: absent
```

## How it works

1. The module writes its two lines into `~postgres/.pgpass` where they
   differ.
2. Without pg_autoctl's configuration for the data directory, the module
   first logs in to the monitor once as `autoctl_node` with `psql` (a
   dryrun does not). The monitor lets `autoctl_node` in only from the
   subnet of its own address (see `host_name` in the
   [monitor's inputs](../pg_auto_failover_monitor/README.md#inputs)), and
   `pg_autoctl create postgres` tries a refused login again without end.
   So the task fails at once when the monitor answers `no pg_hba.conf
   entry`. Any other outcome of this login, such as a monitor that does not
   answer within five seconds, leaves the rest to `pg_autoctl`. Then the
   module creates the node: `pg_autoctl create postgres` with the task's settings,
   `--listen`, `--auth scram-sha-256`, and `--ssl-self-signed`. The first node of a formation
   becomes its primary, in the state `single`, in 15 to 20 seconds. A later
   node copies the primary with `pg_basebackup` and joins as a replica.
3. The module writes the unit as `pg_autoctl show systemd` prints the unit,
   and runs
   `systemctl enable --now pgautofailover.service`. A configuration without
   its unit is a creation that stopped partway: the module runs `pg_autoctl
   create postgres` again, which finishes it. A first run of `pg_autoctl
   create postgres` that wrote the node's configuration and failed is run
   once more at once: for a data directory that pg_autoctl managed before,
   such as a restored one, its first run refused with "PostgreSQL is
   already running" on the test servers, and the second one registered the
   cluster.
4. It reads the node from the monitor with `pg_autoctl show state --json`,
   once the formation has settled (see "A settled formation" below): it
   waits for that at most five minutes, and a dryrun does not wait. With
   `postgresql_settings`, it refuses here a setting that the formation
   cannot take (see "The PostgreSQL settings" below), and while the node's
   PostgreSQL does not accept connections, it writes the settings now, so
   that PostgreSQL starts with them; while no PostgreSQL runs at all, it
   also restarts `pgautofailover.service`, so that pg_autoctl starts one.
   `present` on a node in maintenance
   runs `pg_autoctl disable maintenance` now, before anything that needs
   the node's PostgreSQL, which pg_autoctl keeps stopped after a reboot in
   maintenance. Then the module waits, at most a minute, until PostgreSQL
   accepts connections; a node that stays in maintenance needs no
   PostgreSQL that runs.
5. It makes the local logins ask who logs in, on every run (below), and
   reloads PostgreSQL after a change.
6. When pg_autoctl's `postgresql.listen_addresses` differs from
   `listen_address`, it stops the service, runs `pg_autoctl config set
   postgresql.listen_addresses`, starts the service, and waits, at most a
   minute, until `ss -ltnH` shows PostgreSQL on the port at that address
   alone and it accepts connections. On the primary of a formation with
   other nodes it waits for a switchover instead: the restart would make
   the monitor fail over.
7. On the primary it sets the password of `pgautofailover_replicator` when
   the role has none, when the task created the node, or when the password
   file held another replication password before; then a node that joins
   later can copy.
8. It sets `candidate_priority` and `replication_quorum` with `pg_autoctl
   set node` where the monitor reports other values.
9. With `postgresql_settings`, it keeps the node's PostgreSQL settings, and
   restarts PostgreSQL where `restart_postgresql_for_new_settings` allows
   it (below).
10. `in_maintenance` runs `pg_autoctl enable maintenance` on a replica.
11. After a change, it waits again, at most five minutes, until the
    formation has settled with this node healthy in it: a replica that
    joined or left maintenance has caught up as `secondary`, and a node put
    into maintenance reports `maintenance`.

**A settled formation.** The monitor moves a formation from state to state:
in a failover, a switchover, or while a replica catches up, it assigns each
node its next state and waits for the node to report it. The formation has
settled when every node reports the state that the monitor assigned it,
healthy, and that state is one in which a node rests: `single`, `primary`,
`secondary`, `maintenance`, or `wait_primary` while no other healthy node
is out of maintenance. Two kinds of node that the monitor does not see as
healthy have stopped, and the module does not wait for them: a node that
still reports a resting state while the monitor has assigned it another
one, such as a node whose server is down, and a node in maintenance, whose
PostgreSQL may be stopped. Any other node that is not healthy is on its way
between states: while a node's PostgreSQL restarted to rejoin as a replica,
the monitor did not see it as healthy for a few seconds. At its start, a
task also counts its own node as stopped while that node's PostgreSQL does
not answer: the task's later steps handle that, and a node alone in its
formation whose PostgreSQL a setting keeps from starting reports `single`,
as assigned, and would otherwise never settle. A task
starts on a settled formation, and after a change ends on one, so that in a
fleet stage that runs one node at a time the next node starts on a settled
formation too.

**The PostgreSQL settings.** With `postgresql_settings`, the module keeps
the file `/etc/postgresql/pg_auto_failover_node_settings.conf`, owned by
`root` and the group `postgres`, mode `0644`.
`/etc/postgresql/pg_auto_failover_node_settings.conf` holds a comment naming
the module, then one line `name = 'value'` per setting, sorted by name.
The module doubles each quote and backslash in a value, as PostgreSQL
requires. The data directory's `postgresql.conf` ends with the line
`include_if_exists = '/etc/postgresql/pg_auto_failover_node_settings.conf'`:
the module adds it, or moves it to the end, whenever another setting line
comes after it. `/etc/postgresql/pg_auto_failover_node_settings.conf` lies
outside the data directory, so `pg_basebackup` or `pg_rewind` copies the
primary's include line but leaves the node's settings file unchanged.
Each node therefore keeps values sized from the node's own memory.
On a node whose PostgreSQL runs, the module:

1. writes the file and the include line where they differ, and keeps their
   old contents;
2. has PostgreSQL read its whole configuration with `postgres -D DIRECTORY
   -C port`, as `postgres`. If PostgreSQL refuses a value, name, or line,
   the module restores the previous file contents and fails the task
   (below);
3. reads `pg_file_settings`, which lists configuration lines in PostgreSQL's
   reading order. For each requested setting, the module checks which file
   contains the last line assigning the setting's value.
   If the last assignment comes from outside
   `/etc/postgresql/pg_auto_failover_node_settings.conf`, the module fails
   the task, naming the overriding file and line.
   For example, `ALTER SYSTEM` writes `postgresql.auto.conf`, which
   PostgreSQL reads last. Before failing, the module restores the previous
   contents of the module's settings file and `postgresql.conf` when changed;
4. reloads PostgreSQL with `pg_ctl reload`;
5. reads `pg_settings` for settings marked `pending_restart`.
   The module checks every setting in the module's settings file before
   or after the task. The check includes removed settings returning to
   pg_autoctl's values and unchanged settings left pending by an earlier
   run with `never`. Even with an unchanged settings file, the task
   restarts PostgreSQL when `restart_postgresql_for_new_settings` allows
   a restart.

   Settings outside the module's settings file do not count.
   For example, after promotion, pg_autoctl removes the replica's
   `recovery_target_timeline`. PostgreSQL marks `recovery_target_timeline`
   pending until a restart, although no restart is needed;
6. restarts PostgreSQL for them where `restart_postgresql_for_new_settings`
   allows it (below), then checks in `pg_settings` that each of the task's
   settings comes from the module's file.

The steps run in a run that finds the file unchanged too, apart from the
check with `postgres -C` and the reload.

**A restart of PostgreSQL for a setting.** A setting such as
`shared_buffers` takes effect only when PostgreSQL restarts. pg_autoctl
runs PostgreSQL, so the module restarts PostgreSQL by restarting
`pgautofailover.service`. Restarting the primary's service causes a
failover: the new keeper reports PostgreSQL as stopped, and the monitor
promotes a replica within two seconds. So the task restarts only where
`restart_postgresql_for_new_settings` allows it:

- **On a replica,** `on_a_replica` and `on_a_replica_or_a_node_alone` wait
  until the formation has settled with the replica healthy, then run
  `pg_autoctl enable maintenance`, wait until the monitor reports the
  replica in maintenance, restart `pgautofailover.service`, and run
  `pg_autoctl disable maintenance`. In maintenance the primary does not
  wait for the replica, so writes go on. A restart in maintenance leaves
  PostgreSQL stopped, and `disable maintenance` makes pg_autoctl start it.
  The task then waits until PostgreSQL accepts connections and the
  formation has settled with the replica healthy. A replica that is in
  maintenance and stays in it, with `in_maintenance`, only restarts; its
  PostgreSQL starts with the new values when `present` ends the
  maintenance.
- **On a node alone in its formation,** `on_a_replica_or_a_node_alone`
  restarts `pgautofailover.service` in place, and the task waits for
  PostgreSQL. `on_a_replica` refuses.
- **On the primary of a formation with replicas,** the task never
  restarts: `on_a_replica` and `on_a_replica_or_a_node_alone` refuse.
- **A refusal** puts the module's file and the include line back as they
  were, reloads PostgreSQL, and fails the task with a missing
  prerequisite, without another try: the earlier settings stay in effect.
  MMX reports the task `changed`, because it wrote the file and put it
  back. A dryrun reports `not_checked`.
- **`never`** reloads only: the settings that need a restart wait, the
  task succeeds, and its reason says how to apply them, such as
  `shared_buffers waits for a restart of PostgreSQL; run this task with
  restart_postgresql_for_new_settings: on_a_replica on this node`.
- **A node that the task created** restarts in place with every value of
  `restart_postgresql_for_new_settings` because the new node serves nobody
  yet.

To give a new `shared_buffers` to every node of a formation, run the
playbook with `on_a_replica`: each replica restarts, and the primary's task
fails with the reason that says to switch. Make a replica the primary with
[`pg_auto_failover_primary`](../pg_auto_failover_primary/README.md), then
run the playbook again: the old primary, now a replica, restarts if its
setting still waits. In a fleet stage of one node at a time, the next
node's task waits until the formation has settled after each restart.

**Settings that a replica needs as high as its primary's.** A replica's
PostgreSQL does not run with `max_connections`, `max_worker_processes`,
`max_wal_senders`, `max_prepared_transactions`, or
`max_locks_per_transaction` lower than its primary's. On a replica, the
module reads the primary's values from `pg_controldata` (the lines
`max_connections setting:`, `max_prepared_xacts setting:`, and so on), and
refuses a lower value before it writes anything. On the primary of a
formation with replicas, the module refuses a value lower than the
primary's current value. A lower value would have to reach the primary
first, with a restart that causes a failover. So a higher value goes to the
replicas first, then to the primary; a node alone has no such limit.
`max_wal_senders` and
`max_replication_slots` must also be at least the number of nodes in the
formation: each replica takes one of each on the primary.

**A setting that PostgreSQL refuses.** Before the reload, `postgres -C`
fails for a value that PostgreSQL refuses, such as `shared_buffers: not a
size`: the task puts the old contents back and fails without another try,
with PostgreSQL's words, such as `postgresql_settings.shared_buffers:
PostgreSQL refuses it: invalid value for parameter "shared_buffers": "not
a size"; ...`. Nothing changed, though MMX reports the task `changed`, as
for a refusal. A value that PostgreSQL takes but cannot start with, such
as a `shared_buffers` larger than the server's memory, passes the check:
then the replica's PostgreSQL does not start after the restart, and the
task fails without another try. The same happens when a value that
PostgreSQL refuses reaches the file another way, such as by hand. The other
nodes' tasks go on: the monitor assigns the replica `catchingup` while it
still reports `secondary`, so the formation counts as settled without it.
The node's own task repairs it with a fixed value, on a node alone in its
formation too: while the node's PostgreSQL does not accept connections,
the module writes the file before it waits for PostgreSQL, after the same
check. While no PostgreSQL runs at all, it also restarts
`pgautofailover.service`, also when the file already holds the fixed
value, and the reason says `restarted pgautofailover.service, as the
node's PostgreSQL did not run, so that PostgreSQL starts with them`:
pg_autoctl may not start PostgreSQL again by itself (see the notes below).
A node whose PostgreSQL does not run serves nobody, a replica or a
primary, so the restart takes nothing from anyone. On a primary of a
formation with replicas, the monitor fails over to a healthy replica, as
for any primary whose PostgreSQL stops, and the node then comes back as a
replica; with no healthy replica, it starts as the primary again.
pg_autoctl may restart PostgreSQL once more as it sets the node up again
as a replica, so the task waits until the formation has settled with the
node healthy before it reads PostgreSQL's settings. In maintenance,
pg_autoctl keeps PostgreSQL stopped until `present` ends the maintenance,
and PostgreSQL then starts with the file. A fleet stage that runs one
node at a time thus goes on to the next node once the failed node is
repaired.

**The local logins.** pg_autoctl creates the node with `initdb
--auth=trust`, whose six lines in `pg_hba.conf` let every account of the
server log in as any role, `postgres` included, without a password, on the
local socket and on `127.0.0.1` and `::1`. For exactly the six
`initdb --auth=trust` rules, the module replaces `trust` with Debian's
authentication methods: `peer` on the socket and `scram-sha-256` on the
loopback addresses:

```text
local   all             all                                     peer
host    all             all             127.0.0.1/32            scram-sha-256
host    all             all             ::1/128                 scram-sha-256
local   replication     all                                     peer
host    replication     all             127.0.0.1/32            scram-sha-256
host    replication     all             ::1/128                 scram-sha-256
```

`peer` lets an account in only as the role of its own name: the keeper and
the module's `psql` log in as `postgres`, the account they run as. Every
other line stays as it is, pg_autoctl's own `hostssl` lines included.
A pg_autoctl `hostssl` line is
`hostssl all "pgautofailover_monitor" <monitor's address>/32
trust`: the monitor's health check logs in with it, from the monitor's own
address only, and pg_autoctl gives that role no password. A replica that
copies the primary and a restored cluster also have the six initdb rules
checked and `trust` replaced with `peer` or `scram-sha-256` as above.

**A restored data directory.** A data directory that already holds a
PostgreSQL cluster, such as one that
[`pgbackrest_restore`](../pgbackrest_restore/README.md) restored, is
registered as it is, as the primary of a formation that has no node.
`pg_autoctl create postgres` refuses a cluster that runs, so the module
stops one first (`pg_ctl stop --mode fast`), and the node's service starts
it again. pg_autoctl writes the task's `listen_address` into the data
directory's `postgresql-auto-failover.conf` over the address that the
restore brought back from the lost server, so the restored cluster
listens at this server's address. While the formation has nodes, the task
waits: pg_autoctl would replace the cluster with a copy of the primary.
Drop the dead nodes first with the monitor's task.

**Synchronous replication** needs nothing else: with one replica whose
`replication_quorum` is true, pg_auto_failover sets
`synchronous_standby_names` to `ANY 1 (pgautofailover_standby_2)`, and
while that replica is down or in maintenance it clears it, so writes go on.

A database, a login, and `pg_hba.conf` lines for the applications are
ordinary node tasks: `postgresql_database` and `postgresql_role` on the
primary, and `line` tasks in the data directory's `pg_hba.conf`, which a
joining node copies from the primary. pg_autoctl adds only its own logins
to `pg_hba.conf`.

**Removal.** `absent` stops and disables the service first, because
`pg_autoctl drop node` ends a running keeper and systemd would start it
again. The module removes the unit, then drops the node from its formation
where the monitor lists the node. The module's file of PostgreSQL settings
and the data stay. A node that the monitor's task dropped from its side
has a keeper that exits and starts again for ever; `absent` on that server
stops it. `deleted_with_all_its_data` runs `pg_autoctl drop node --destroy`
for a node that the monitor lists. For a node that the monitor no longer
lists, `--destroy` fails, because it asks the monitor first; the module
then stops the cluster with the `pg_ctl` of pg_autoctl's configuration and
removes the data directory (`rm -r --one-file-system`) and pg_autoctl's
configuration and state itself, as `--destroy` does. It does so only when
the monitor answers: while it does not, a run fails and is tried again.
`deleted_with_all_its_data` also removes
`/etc/postgresql/pg_auto_failover_node_settings.conf`.

**A monitor built again.** When the monitor's server is lost and built
again, its task creates a new, empty monitor at the same address, and the
nodes go on serving without one. A node that the monitor does not list is
then one of two kinds, and the monitor's events tell them apart: a monitor
records events for each node it registers, and keeps those of a node it
dropped, while a new monitor has none of a node it never knew. A node that
the events name is refused as a dropped node. A node absent from the
monitor's events registers through pg_auto_failover's monitor replacement
commands, run as `postgres`. The module runs
`pg_autoctl disable monitor --force`, then `pg_autoctl enable monitor`
with the task's monitor address. The node's
password file already holds the password of `autoctl_node`.

The primary registers first, and stays the primary: the module asks the
node's PostgreSQL whether it is a replica. After `enable monitor` it signals
the keeper with `pg_autoctl reload` and waits up to two minutes for the
keeper's first report to the monitor. A replica registers once the monitor
lists a primary of its formation that has reported, and waits for one up to
five minutes, so that the primary's own task, run at the same time,
registers first; in an empty formation the monitor would make the first node
it registers a primary, and a primary without reports it takes for failed.

MMX starts a fleet stage's nodes in the order of their names, whatever
order the inventory lists them in. So in a fleet stage that runs one node
at a time, such as the one in [Set up](#set-up), a replica whose name comes
before the primary's, as after a failover, starts first: it waits the five
minutes, then fails with a missing prerequisite whose reason names the
address of the primary it receives changes from. With
`on_node_failure: continue`, the default, the primary's task still runs
later in the playbook run where the replica's task failed. The primary's
task registers the primary. The next playbook run registers the replica.
With `stop_fleet_stage` or `stop_run`, the primary's task does not
start. After rebuilding the monitor, use the following run order whether
`on_node_failure` is `continue`, `stop_fleet_stage`, or `stop_run`.
Run the playbook first with `--limit` and the primary's node, then run the
whole playbook: the primary
registers without the replica's five-minute wait, and the replica registers
in the second run. A dryrun of the replica does not wait and names the
primary's address. A primary while the monitor lists another
primary fails: two nodes serve as primaries.
A person must choose whether to keep the data from the unregistered
primary or the primary already listed by the monitor.
A node whose `pg_autoctl enable monitor` failed after
its monitor was disabled keeps `PG_AUTOCTL_DISABLED` as its monitor; the
next run reads the monitor at the task's address and only enables it.

## Errors and what they mean

Every reason starts with `pg_auto_failover_node`.

| The reason holds | What it means, and what to do |
|---|---|
| `the program ... is not installed; install the package ...` | The packages of [Set up](#set-up) are missing. A run fails without another try. |
| `the directory DIRECTORY, which holds the data directory, ...` | The data directory's parent is missing or belongs to another account. Create it as a directory of `postgres`. |
| `the PostgreSQL cluster VERSION main of Debian's tools uses the port PORT` | Installing `postgresql-<version>` created Debian's cluster `main`. Write `create_main_cluster = false` before the package task, or remove the cluster with `pg_dropcluster --stop VERSION main`. |
| `the node NAME is the primary of the formation FORMATION (STATE); maintenance on the primary would cause a failover` | Make another node the primary first with `pg_auto_failover_primary`, in an earlier fleet stage. A run fails without another try. |
| `the node NAME is the primary of the formation FORMATION (STATE), and a new listen_address takes a restart of its service` | Make another node the primary with `pg_auto_failover_primary` in an earlier fleet stage. Then change `listen_address` on the former primary while the former primary is a replica. |
| `pg_auto_failover_node.listen_address: must be an IP address of the server` | `listen_address` is a host name or more than one address. Give one IP address, or `*`. |
| `pg_auto_failover_node.postgresql_settings.NAME: PostgreSQL refuses it: ...`, or `postgresql_settings: PostgreSQL refuses its configuration with these settings: ...` | `postgres -C` refused the configuration with the task's settings; PostgreSQL's own words follow, such as `invalid value for parameter`, `unrecognized configuration parameter`, or `is outside the valid range for parameter`. The module put its file back, so the earlier settings stay in effect, though MMX reports the task `changed`. Fix the value. MMX does not try the task again. |
| `pg_auto_failover_node.postgresql_settings.NAME: FILE sets it again at line LINE, which PostgreSQL reads after /etc/postgresql/pg_auto_failover_node_settings.conf` | Another configuration file sets the setting later, so the task's value would not be live. For `postgresql.auto.conf`, which `ALTER SYSTEM` writes, run `ALTER SYSTEM RESET NAME` in `psql` on the node; for another file, remove the line there. The module put its file back. |
| `pg_auto_failover_node.postgresql_settings.NAME: is the module's input ...`, `... pg_autoctl writes it ...`, `... must be on ...` | The module refuses a setting that pg_autoctl or the module keeps, or a value that pg_auto_failover cannot work with: see [Why it works this way](#why-it-works-this-way). Remove the setting, or use the module's own input. |
| `pg_auto_failover_node.postgresql_settings.NAME: VALUE is lower than PRIMARY_VALUE, the primary's value, which pg_controldata reports on this replica` | A replica's PostgreSQL does not run with a lower value than its primary's. Keep at least the primary's value. |
| `pg_auto_failover_node.postgresql_settings.NAME: VALUE is lower than CURRENT, with which the primary NAME runs` | Lowering the value on the primary of a formation with replicas would take a restart of the primary first, which is a failover. Keep the value. |
| `pg_auto_failover_node.postgresql_settings.max_wal_senders: must be at least COUNT, the number of nodes in the formation FORMATION` | Each replica takes one WAL sender and one replication slot on the primary; the same holds for `max_replication_slots`. |
| `the node NAME is the primary of the formation FORMATION, and SETTING takes a restart of PostgreSQL, which on the primary is a failover` | Make another node the primary first with `pg_auto_failover_primary`, then run the task again, or use `never` and let the setting wait. The module put its file back; the earlier settings stay in effect. A run fails without another try; a dryrun reports `not_checked`. |
| `the node NAME is alone in the formation FORMATION, and SETTING takes a restart of PostgreSQL, which restart_postgresql_for_new_settings: on_a_replica makes only on a replica` | Give `on_a_replica_or_a_node_alone` to restart a node alone in place. The module put its file back. |
| `putting the node NAME into maintenance to restart PostgreSQL for SETTING: ...` | The monitor refused maintenance on the replica, such as while the primary waits for a replica (`wait_primary`); the text behind the colon is pg_autoctl's message. The new file is in place and loaded. MMX tries the task again, and the next run restarts once the formation has settled. |
| `restarted PostgreSQL ... for SETTING, and a minute later PostgreSQL does not accept connections` | PostgreSQL did not start with the new values, such as a `shared_buffers` larger than the server's memory. Read the journal that the reason names, fix the value, and run the task again: it writes the file and restarts `pgautofailover.service` before it waits for PostgreSQL. MMX does not try the task again. |
| `restarted pgautofailover.service, as the node's PostgreSQL did not run, and a minute later the node's PostgreSQL in DIRECTORY does not accept connections` | After the service restart, PostgreSQL still does not start with `/etc/postgresql/pg_auto_failover_node_settings.conf`. Alternatively, pg_autoctl needs longer, for example, to copy data from the primary again with `pg_basebackup`. Read the journal named in the reason. Fix the value, or run the task again once pg_autoctl has copied the data. |
| `restarted PostgreSQL ..., and PostgreSQL takes SETTING from SOURCE, not from /etc/postgresql/pg_auto_failover_node_settings.conf` | After the restart, a setting of the task has its value from elsewhere, such as from PostgreSQL's command line. Look up the setting named as `SETTING` in `pg_settings` and `pg_file_settings`. MMX does not try the task again. |
| `set postgresql.listen_addresses to ADDRESS and started pgautofailover.service, and a minute later PostgreSQL listens on port PORT at ...` | PostgreSQL did not take the address, such as an address that is not on the server, or not yet, as a WireGuard address before its interface is up. Read the journal that the reason names. MMX does not try the task again. |
| `the monitor does not list the node NAME of DIRECTORY in the formation FORMATION: it was dropped from the monitor` | The monitor's events name this node, which it no longer lists: the monitor's task dropped it. pg_autoctl cannot add a dropped node again: remove it with `state: deleted_with_all_its_data`, and create it again under a new name. |
| `the monitor at ADDRESS has no record of the replica NAME and lists no primary of the formation FORMATION that has reported to it` | The monitor was built again, and the replica waited five minutes for its primary to register, which in a fleet stage of one node at a time starts after it when its name comes later. The reason names the primary's address when the replica receives changes from it. Run the playbook with `--limit` and the primary's node, then run it again. See [A monitor built again](#how-it-works). |
| `the node NAME is a primary, and the monitor at ADDRESS, which has no record of it, lists OTHER as the primary` | Two nodes serve as primaries, such as after a failover that the old monitor ordered while one node could not hear it. Decide which one keeps the data, and remove the other with `state: deleted_with_all_its_data`. |
| `registered the node NAME with the monitor at ADDRESS, but its keeper has sent the monitor no report for 120 seconds` | The keeper did not take its new monitor. Read the journal that the reason names. Until the node reports, the monitor takes the node for failed. |
| `registering the node NAME with the monitor at ADDRESS: pg_autoctl ...` | `pg_autoctl disable monitor` or `enable monitor` failed; the text behind the colon is its message. The next run tries again. |
| `DIRECTORY holds a PostgreSQL cluster, and the formation FORMATION has the nodes ...` | pg_autoctl would replace the cluster with a copy of the primary. Drop the nodes listed for `FORMATION` in the reason with the monitor's task first, or empty the directory. |
| `pg_auto_failover_node.INPUT: the node in DIRECTORY has the ...` | The node was created with another name, formation, host name, port, or monitor, which pg_autoctl does not change. Changing the name, formation, host name, port, or monitor is still refused after `state: absent`, because `absent` retains pg_autoctl's configuration. |
| `DIRECTORY holds a pg_autoctl instance of the role monitor, not a node` | The data directory belongs to a monitor. |
| `/etc/systemd/system/pgautofailover.service runs the pg_autoctl instance in DIRECTORY` | The server runs another instance of pg_autoctl. One instance runs on a server. |
| `the formation FORMATION has not settled in 300 seconds: NODE is STATE (assigned STATE, healthy)` | A failover, a switchover, or a replica's catching up went on for more than five minutes, or the formation is stuck, such as a primary that does not reach the state `primary`, or a replica that does not catch up. Each node that kept the formation from settling is named with the state it reports and the one the monitor assigned it. Read `pg_autoctl show state` and `pg_autoctl show events` on the monitor's server. MMX does not try the task again, which would wait as long again; run the playbook again once the formation has settled. |
| `the monitor at ADDRESS, port PORT refused the login of autoctl_node from this node: its pg_hba.conf has no entry for the node's address ...` | The node shares no subnet with the monitor, such as servers whose addresses are `/32` on some cloud providers. The monitor lets nodes in only from the subnet of its own address. Put the monitor's `host_name` and the nodes on one network that they share, such as a private network or a WireGuard network, as the [monitor's inputs](../pg_auto_failover_monitor/README.md#inputs) describe, and run the task again. The text after `psql said:` is PostgreSQL's message. MMX does not try the task again. |
| `the monitor does not answer: ...` | `pg_autoctl` could not reach the monitor. A run is tried again; a dryrun reports `not_checked`. |
| `creating the node ... pg_autoctl wrote the node's configuration; the next run of the playbook runs the creation again, which finishes it` | `pg_autoctl create postgres` failed after it registered the node, such as during a join's copy. MMX does not try the task again: a new try would report `created: false`. Act on pg_autoctl's message and run the playbook again. |
| `created the node, and then this failed: ...` | The task created the node, and a later step failed. MMX does not try the task again. Act on the message and run the playbook again. |
| `putting the node NAME into maintenance: ...`, `setting ... of the node ...`, `dropping the node ...` | A command of `pg_autoctl` failed; the text behind the colon is its message. MMX tries the task again. |

## Tested on

**Distributions:** Debian 12 and 13, Ubuntu 24.04 and 26.04.

The integration suite `tests/integration/pg_auto_failover.py` runs
`pg_auto_failover_node` on two database servers with a monitor on a third,
in system containers on x86_64. The suite covers:

- creation and a join with synchronous replication;
- local logins and replacement of a `trust` line that returns;
- each node listening at only the node's own address;
- a new `listen_address` and refusal to change the primary's address;
- new passwords and failover settings;
- maintenance and refusal of maintenance on the primary;
- tasks ending on a settled formation after a join and after maintenance;
- a restart of the primary's service, followed by a node task during the
  resulting failover that waits for a settled formation;
- PostgreSQL settings with `never` on each node, with `work_mem` active
  from the module's settings file and `shared_buffers` waiting for restart;
- a replica restart through maintenance to apply the PostgreSQL settings,
  and refusal of the settings restart on the primary;
- a value PostgreSQL refuses and a setting overridden by `ALTER SYSTEM`;
- a replica whose PostgreSQL cannot start with a refused value, then
  recovery through the replica's task;
- refusal of `max_connections` below the primary's value on a replica;
- a node alone refusing `on_a_replica`, then restarting in place with
  `on_a_replica_or_a_node_alone`;
- failover with the old primary rejoining;
- a rebuilt monitor, with the primary registering before the replica;
- a node dropped by the monitor;
- `absent` and `deleted_with_all_its_data`.

`tests/integration/pgbackrest.py` uses `pg_auto_failover_node` to register a
cluster restored onto a replacement server at the replacement server's
own address, rather than the lost server's address.

| Distribution | pg_auto_failover | PostgreSQL |
|---|---|---|
| Debian 12 | 2.0 | 15 |
| Ubuntu 24.04 | 2.1 | 16 |
| Debian 13 | 2.2 | 17 |
| Ubuntu 26.04 | 2.2 | 18 |

- **Not tested:** more than two nodes in a formation, Citus formations, and
  a PostgreSQL that does not come from the distribution.

## Why it works this way

- **A node that a new monitor never knew registers, and a dropped one does
  not.** A monitor built again in place of a lost one has no node of the
  formation, and without its record the database servers ran without
  automatic failover: the primary went on alone, and the replica's keeper
  failed on every loop. The monitor's events are the record of what it
  knew: they name each node that it registered or dropped.
- **The module waits for the keeper's report after `enable monitor`.**
  `disable monitor` and `enable monitor` each signal the running keeper to
  read its configuration. On the test servers the second signal, a second
  after the first, was lost: the keeper went on without a monitor, the new
  monitor saw no report from the primary, and minutes later it promoted the
  replica and made the old primary rewind. So the module waits five seconds
  between the two, signals once more with `pg_autoctl reload`, and lets a
  replica register only beside a primary that has reported.
- **A replica without its primary fails, even when the primary comes
  later in the same fleet stage.** A module's task in a run either
  succeeds or fails. A success would report the formation complete while
  the replica has no failover, and neither a person nor an agent reading
  the run would see it. A failure is seen, names the primary's address and
  the `--limit` to run, and happens only when a monitor was built again
  after a failover put the primary on the later name. MMX's order of a
  fleet stage's nodes is by name, so a playbook cannot list the primary
  first.

- **A task starts and ends on a settled formation.** On two database
  servers, a playbook restarted the primary's `pgautofailover.service` to
  load a setting, which made the monitor fail over. The fleet stage ran
  one server at a time. In the fleet stage that restarted the primary,
  the playbook next restarted the replica's service during the replica's
  promotion. The promotion stopped partway.
  pg_autoctl started PostgreSQL as a primary on the old timeline,
  and both servers had written different changes under that one timeline.
  pg_auto_failover then let the old primary rejoin without `pg_rewind`, and
  the new primary, which waited for a replica that received nothing,
  stopped every write for minutes. The wait at a task's start keeps a
  playbook's later work on a node, such as a restart, from meeting a
  failover that still goes on.
- **A restart of the primary is refused, not turned into a switchover.**
  A switchover is a task of its own, `pg_auto_failover_primary`, that a
  reader of the playbook sees. The module refuses actions that restart
  PostgreSQL on the primary of a formation with replicas:
  `in_maintenance`, a new `listen_address`, and a restart for
  `postgresql_settings`.
- **The module keeps the PostgreSQL settings, not a playbook's `line`,
  `file`, `assert`, and `systemd_service` tasks.** Such a recipe restarted
  a primary in place, which was a failover, and when a setting broke
  PostgreSQL, the next run waited for the PostgreSQL that the setting kept
  from starting and could not repair it.
- **The settings file lies outside the data directory.** `pg_basebackup`
  and `pg_rewind` copy the data directory from the primary: a file inside
  would bring the primary's values, sized from its memory, to every node.
- **The include line is the last setting line of `postgresql.conf`.** On
  pg_auto_failover 2.0 and 2.2, pg_autoctl included its own files at the
  top of `postgresql.conf`, so a file included at the end wins over them and
  over initdb's lines. The copies bring the line along, and the module puts
  it back at the end on every run.
- **PostgreSQL checks the settings with `postgres -C`, not with
  `pg_file_settings`.** On PostgreSQL 15 and 18, `postgres -C` read
  `postgresql.conf` with every include and `postgresql.auto.conf`, worked
  while the server ran, and failed for an invalid value, an unknown name,
  and a syntax error. `pg_file_settings` said "setting could not be
  applied" both for a valid value that waits for a restart and for an
  invalid one.
- **A setting that another file sets again fails the task.** Otherwise the
  task would report a value that is not live. `ALTER SYSTEM` writes
  `postgresql.auto.conf`, which PostgreSQL reads last; there the module's
  line showed as not applied, without an error.
- **The task restarts for every setting of the module's file that waits
  for a restart, not only for a changed file.** A setting can remain pending
  after a run with `never`. A person's configuration change can also leave
  a setting pending. Removing a setting from the task can require a restart to
  restore pg_autoctl's value. The next run that allows a restart applies
  the pending settings.
- **A replica restarts through maintenance.** With `replication_quorum`,
  the primary waits for the replica unless the replica is in maintenance.
  On the test servers, a restart in maintenance left PostgreSQL stopped, and
  `pg_autoctl disable maintenance` made pg_autoctl start it. The monitor
  refused maintenance on a replica while the primary was `wait_primary`, so
  the task first waits for a settled formation.
- **The module writes the settings before it waits for a PostgreSQL that
  does not run, and restarts `pgautofailover.service` while none runs.**
  Without that order, a replica whose PostgreSQL did not start with a
  setting could not be repaired by a run. On Debian 13 and Ubuntu 26.04,
  after a `shared_buffers` larger than the server's memory, pg_autoctl
  started PostgreSQL itself for the crash recovery before `pg_rewind`, kept
  it as a defunct process after it failed, logged `Failed to get Postgres
  pid` every 30 seconds, and never started PostgreSQL again, with the fixed
  file too; four seconds after a restart of the service, the replica was
  healthy. `pg_isready` tells a server that does not run (no answer) from
  one that starts or recovers (it refuses connections for now): only the
  first gets the restart. pg_autoctl also stops PostgreSQL while it copies
  the data again with `pg_basebackup`, which the restart starts over; the
  task then fails and says to read the journal.
- **A replica takes no value lower than its primary's for five settings.**
  PostgreSQL does not run a hot standby whose `max_connections`,
  `max_worker_processes`, `max_wal_senders`, `max_prepared_transactions`, or
  `max_locks_per_transaction` is lower than its primary's. On PostgreSQL 15
  and 18, `pg_controldata` on a replica printed the primary's values. The
  primary of a formation with replicas lowers none, since a lower value
  would have to start on the primary first.
- **The module refuses settings that pg_autoctl, the monitor, or the module
  keep:**
  - `listen_addresses` and `port`: they are the module's inputs
    `listen_address` and `port`; pg_autoctl's configuration and the
    monitor's registration hold them.
  - `primary_conninfo`, `primary_slot_name`, `recovery_target_timeline`,
    and the other `recovery_target` settings: pg_autoctl writes them into
    `postgresql-auto-failover-standby.conf` to follow the current primary,
    and a value from the module's file would win.
  - `synchronous_standby_names`: pg_autoctl sets it on the primary with
    `ALTER SYSTEM`, as the monitor decides.
  - `hot_standby` other than `on`: the keeper and the monitor's health
    checks connect to a replica's PostgreSQL.
  - `wal_level` other than `replica` or `logical`: with `minimal`,
    streaming replication and `pg_rewind` stop working.
  - `wal_log_hints` other than `on`: `pg_rewind`, which pg_autoctl runs
    when an old primary rejoins, needs it.
  - `ssl` other than `on`: the replicas and the monitor connect with
    `sslmode=require`.
  - `password_encryption` other than `scram-sha-256`: the module and
    pg_autoctl create `scram-sha-256` logins.
  - `synchronous_commit` set to `off` or `local`: a commit is then
    acknowledged before a replica has it, and a failover loses writes that
    were acknowledged.
  - `max_wal_senders` and `max_replication_slots` lower than the number of
    nodes in the formation: each replica takes one of each on the primary.
- **Every other setting is accepted, `shared_preload_libraries`
  included.** pg_autoctl writes `pg_stat_statements` into
  `shared_preload_libraries`, but runs `CREATE EXTENSION pg_stat_statements`
  only when pg_autoctl creates a node. Omitting `pg_stat_statements` from
  `shared_preload_libraries` leaves only the views of `pg_stat_statements`
  unusable. For
  reference, on pg_auto_failover 2.0 and 2.2, `postgresql-auto-failover.conf`
  held `shared_preload_libraries`, `listen_addresses`, `port`,
  `max_wal_senders`, `max_replication_slots`, `wal_level`, `wal_log_hints`,
  `wal_sender_timeout`, `hot_standby_feedback`, `hot_standby`,
  `synchronous_commit`, the logging settings, `password_encryption`, the
  `ssl` settings, `wal_keep_size`, and a block of tuning from the server's
  memory, such as `shared_buffers`, `work_mem`, and `effective_cache_size`.
- **A share of the server's memory is the module's own form.** A task's
  references in MMX are data, not expressions, so a value cannot work out
  a quarter of the memory from the fact `memory.total_mib`. Memory settings
  such as `shared_buffers` are sized from each server's memory, so the
  module works that out on the node. The value is rounded down to a step,
  such as 128 MiB, because a new kernel can move `MemTotal` by a few MiB,
  and each new value of a setting that takes a restart is a restart.
- **A setting's name is in lower case.** PostgreSQL ignores the case of a
  name, and `pg_settings` lists every name in lower case, so one setting
  has one spelling.
- **A dryrun does not check the values.** `postgres -C` reads the files on
  the node, and a dryrun writes none.

- **The passwords are in `~postgres/.pgpass`, not in pg_autoctl's
  configuration.** The keeper reaches the monitor, and a replica's
  PostgreSQL reaches the primary, with libpq.
  For each connection, libpq reads `~postgres/.pgpass` because the keeper
  and PostgreSQL run as `postgres`. `pg_autoctl config set replication.password`
  would take the password on its command line and keep it in a file that
  every account of the server can read.
- **The replication password is set on the primary without a comparing
  login.** pg_autoctl lets `pgautofailover_replicator` log in only from the
  other nodes' addresses, and the loopback addresses need not be open, so
  the primary cannot log in to itself with the password. The module sets it
  where it cannot know that the role has the task's password: a role
  without one, a node that the task created, and a password file that held
  another one.
- **`in_maintenance` on the primary is refused.** pg_autoctl would fail
  the primary over to a replica. A planned switchover is its own task,
  `pg_auto_failover_primary`, in an earlier fleet stage.
- **A new `listen_address` on the primary is refused too.** On the test
  servers the monitor failed over to the replica each time the primary's
  `pgautofailover.service` restarted.
- **`is_primary` follows the monitor's assigned state.** After a failover
  on the test servers, the old primary, whose service had stopped, still
  reported `primary` while the monitor had assigned it `demoted`.
- **No `trust` on the local socket and the loopback addresses.** With
  initdb's lines, any account of the server logged in as `postgres` on the
  test servers. The module checks the lines on every run, so a line that
  comes back is replaced again.
- **`listen_address` changes with the service stopped.** pg_autoctl gives
  PostgreSQL the address when it starts it, and `pg_autoctl config set`
  while the service ran did not keep the value on the test servers: the
  running pg_autoctl wrote its old value back. pg_autoctl's own
  connections need no loopback address: with each server's address alone,
  the keepers, replication, a switchover, and a failover worked.
- **The module reads the node's state before `disable maintenance`,**
  because a second `pg_autoctl disable maintenance` fails.
- **`present` ends the maintenance before it waits for PostgreSQL.** After
  a reboot of a replica in maintenance, as for a kernel update, pg_autoctl
  kept its PostgreSQL stopped on the test servers until `pg_autoctl
  disable maintenance` ran.
- **`absent` keeps the data directory.** Removing the node from its
  formation is reversible; deleting the data is spelled out as
  `deleted_with_all_its_data`.
- **The unit is written after the creation, never before.** `pg_autoctl
  show systemd` prints nothing for a data directory without an instance,
  and an empty unit file reads as masked.
