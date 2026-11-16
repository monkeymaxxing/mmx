# `pg_auto_failover_monitor`

`pg_auto_failover_monitor` manages the monitor of a
[pg_auto_failover](https://pg-auto-failover.readthedocs.io/) formation.
pg_auto_failover keeps a PostgreSQL primary and its replicas in one
*formation* and promotes a replica when the primary fails. The *monitor* is
a small PostgreSQL instance of its own, on a server of its own, that
watches the database servers and decides which one is the primary. Each
database server is a *node* of the formation, which
[`pg_auto_failover_node`](../pg_auto_failover_node/README.md) manages; a
planned switchover is
[`pg_auto_failover_primary`](../pg_auto_failover_primary/README.md).

The module runs pg_auto_failover's program `pg_autoctl`, and `psql`, as
the account `postgres` on the monitor's server, which is a **node** of MMX:
a Linux server that MMX manages. The module manages:

- The monitor's PostgreSQL instance and systemd unit
  `pgautofailover.service`.
- The password of the monitor's login `autoctl_node`.
- The formation and which database nodes belong to the formation.
- The address where the monitor's PostgreSQL listens.
- The local login rules in the monitor's `pg_hba.conf`.

The module reports which node the monitor currently assigns as primary.

## Set up

The module installs nothing. Debian and Ubuntu package pg_auto_failover:
`pg-auto-failover-cli` brings `pg_autoctl`, and
`postgresql-<version>-auto-failover` the monitor's PostgreSQL extension.
Each distribution packages pg_auto_failover for one PostgreSQL version.
The packaged PostgreSQL versions are 15 on Debian 12, 16 on Ubuntu 24.04,
17 on Debian 13, and 18 on Ubuntu 26.04. The same **node tasks** set up the
monitor's server and the database servers; a node task is one entry of a
playbook's `node_tasks`. The inventory gives the version as a variable of
the group:

```yaml
# inventories/production.yml
nodes:
  monitor: {address: 10.20.0.31}
  db01: {address: 10.20.0.21}
  db02: {address: 10.20.0.22}
groups:
  postgresql_servers:
    nodes: [monitor, db01, db02]
    vars:
      # 15 on Debian 12, 16 on Ubuntu 24.04, 17 on Debian 13, 18 on Ubuntu 26.04.
      postgresql_major_version: 18
  database_servers:
    nodes: [db01, db02]
```

```yaml
# mmx run configure
fleet_stages:
  - id: set_up
    nodes: postgresql_servers
    node_tasks:
      # Installing postgresql-<version> creates a cluster main on port
      # 5432 unless this file says otherwise; pg_auto_failover creates
      # its own instances.
      - directory: {path: /etc/postgresql-common, permissions_mode: "0755"}
      - file:
          path: /etc/postgresql-common/createcluster.conf
          content: "create_main_cluster = false\n"
          permissions_mode: "0644"
      - apt_package:
          packages:
            - {name: "postgresql-{{ .vars.postgresql_major_version }}", version: newest_when_first_installed}
            - {name: "postgresql-{{ .vars.postgresql_major_version }}-auto-failover", version: newest_when_first_installed}
            - {name: pg-auto-failover-cli, version: newest_when_first_installed}
      # pg_autoctl makes a directory backup beside each data directory, so
      # the directory that holds them belongs to postgres.
      - directory:
          path: "/var/lib/postgresql/{{ .vars.postgresql_major_version }}"
          owner: postgres
          group: postgres
          permissions_mode: "0755"

  - id: monitor
    nodes: [monitor]
    node_tasks:
      - pg_auto_failover_monitor:
          data_directory: "/var/lib/postgresql/{{ .vars.postgresql_major_version }}/monitor"
          postgresql_major_version: "{{ .vars.postgresql_major_version }}"
          port: 5432
          host_name: 10.20.0.31
          listen_address: 10.20.0.31
          autoctl_node_password: "{{ .secrets.autoctl_node_password }}"
          formation: default
          node_names: [db01, db02]
          nodes_not_listed: remove
```

The database servers' fleet stage follows; the page of
[`pg_auto_failover_node`](../pg_auto_failover_node/README.md#set-up)
shows it, with the `systemd_unit` drop-in that a `listen_address` on a
WireGuard interface needs. The monitor needs a server of its own:
pg_autoctl's unit runs one instance on a server, and a monitor beside a
node would fail with that server.

## Inputs

| Input | Meaning | When omitted |
|---|---|---|
| `data_directory` | The monitor's PostgreSQL data directory, an absolute path such as `/var/lib/postgresql/18/monitor`. The parent of `data_directory` must belong to `postgres`. | Required. |
| `state` | `present` or `deleted_with_all_its_data`. `deleted_with_all_its_data` stops `pgautofailover.service` and removes the service's unit. Then the module runs `pg_autoctl drop monitor --destroy`, which deletes the data directory. With `state: deleted_with_all_its_data`, the module accepts only `data_directory` besides `state`. | `present`. |
| `postgresql_major_version` | The PostgreSQL version whose programs run the monitor, such as `18`: `/usr/lib/postgresql/18/bin/pg_ctl`. | Required for `present`. |
| `port` | The port of the monitor's PostgreSQL, such as `5432`. | Required for `present`. |
| `host_name` | The host name or IP address with which the nodes reach the monitor, such as its address on a private network. pg_autoctl lets `autoctl_node` log in from that address's subnet, so it must be an address on a network that the database servers share with the monitor. | Required for `present`. |
| `listen_address` | The IP address of the server on which the monitor's PostgreSQL listens, such as `10.20.0.31`, or `*` for every address. `host_name` is the address that the nodes connect to, and `listen_address` is where PostgreSQL listens: usually the same private address, so that nothing else reaches the monitor's port. A changed address takes a restart of `pgautofailover.service`. | Required for `present`. |
| `autoctl_node_password` | The password of the monitor's login `autoctl_node`, with which every node reaches the monitor, usually a secret such as `"{{ .secrets.autoctl_node_password }}"`. It must not be empty or hold a line break. | Required for `present`. |
| `formation` | The formation whose nodes `node_names` lists. pg_auto_failover's first formation is `default`; the module creates another one that the monitor lacks. | Required for `present`. |
| `node_names` | The names of the nodes that belong to the formation, such as `[db01, db02]`, or an inventory group's sorted node list, `"{{ .inventory.groups.database_servers }}"`, so that removing a server from the inventory removes it from the formation. A listed node that has not registered yet is no error: each node registers itself. | Required for `present`. |
| `nodes_not_listed` | `remove` or `keep`. `remove` drops a registered node that `node_names` does not list, also when its server is gone; `[]` with `remove` drops every node that may be dropped. `keep` leaves such a node. | Required for `present`. |

**The password** reaches `psql` only through its standard input. It is on no
command line, in no file of the monitor's server, and in no result.
`autoctl_node_password` is a private value: a secret in it does not hide
the task's result, and MMX shows `[redacted]` wherever a text of the result
would hold it.

## Outputs

| Output | Meaning |
|---|---|
| `created` | `true` when the task created the monitor. In a dryrun: `true` when a run would create it. |
| `primary_node_name` | The node that the monitor has made the formation's primary: the one whose assigned state is `single`, `wait_primary`, `primary`, `join_primary`, or `apply_settings`. The empty text when no node or more than one has such a state, as for some seconds during a failover. |
| `nodes` | One entry for each node that the monitor lists for the formation, from `pg_autoctl show state --json`: `name`, `host_name`, `port`, `current_state`, `assigned_state`, `healthy`, `candidate_priority`, and `replication_quorum`. |
| `node_monitor_uri` | The connection URI with which nodes reach the monitor, without a password, such as `postgres://autoctl_node@10.20.0.31:5432/pg_auto_failover?sslmode=require`. |

A later node task reads them as `.node_tasks.<id>.primary_node_name`. A
later fleet stage reads `primary_node_name` through an output of the
monitor's fleet stage, to run work on the primary: the monitor decides
which node that is, never the inventory.

## What a dryrun reports

A dryrun runs `pg_autoctl`'s and `psql`'s read commands and changes nothing.

- **A monitor to create:** `would_change`, with `would create the
  pg_auto_failover monitor in DIRECTORY; would write the unit ... and start
  it; would set the password of autoctl_node`, and `created: true`.
- **A stopped service:** `would_change`, `would start pgautofailover.service`;
  the password and the nodes are compared once the monitor runs, so the
  dryrun does not know `primary_node_name` and `nodes`, and a later node
  task that reads them is `not_checked`.
- **A password that the monitor refuses:** `would_change`, `would set the
  password of autoctl_node`. The module attempts one login with the supplied
  password. If the monitor refuses the login, the dryrun reports that a run
  would set the password. The monitor records the refused login in the log.
- **A node to drop:** `would_change`, `would drop the node db02 (secondary)
  from the formation default`; the output `nodes` leaves it out.
- **initdb's trust lines:** `would_change`, `would replace trust in
  DIRECTORY/pg_hba.conf with ...`, which names each line and its new
  method, and `would reload PostgreSQL in DIRECTORY`; a missing line of the
  ident map: `would write the ident map pg_autoctl ...`.
- **Another listen address:** `would_change`, `would stop
  pgautofailover.service, set postgresql.listen_addresses of the instance
  in DIRECTORY from * to 10.20.0.31, and start the service again`.
- **`not_checked`** while `pg_autoctl`, the PostgreSQL programs, the
  extension, or the account `postgres` are missing, while the directory
  that holds the data directory does not belong to `postgres`, while a
  cluster of Debian's tools uses the port, and for a healthy primary that
  `node_names` leaves out: an earlier node task may install the packages,
  change the directory, remove the cluster, or switch the primary.
- **A failure, as in a run,** for an input that breaks a rule, a data
  directory that holds a node, and a server whose unit runs another
  instance of `pg_autoctl`.

## Examples

The monitor of the formation `default`, whose nodes are `db01` and `db02`:

```yaml
- pg_auto_failover_monitor:
    data_directory: /var/lib/postgresql/18/monitor
    postgresql_major_version: 18
    port: 5432
    host_name: 10.20.0.31
    listen_address: 10.20.0.31
    autoctl_node_password: "{{ .secrets.autoctl_node_password }}"
    formation: default
    node_names: [db01, db02]
    nodes_not_listed: remove
```

When `db02`'s server is gone for good, a playbook leaves it out of
`node_names`, and the next run drops it from the formation with `pg_autoctl
drop node --force`, which needs no answer from the server. A replacement
server gets a new name, such as `db03`: pg_auto_failover does not take a
dropped name again at once.

The task refuses to drop a node that the monitor reports healthy and has
made the primary. To retire the primary `db01`, an earlier fleet stage
makes another node the primary:

```yaml
- pg_auto_failover_primary:
    monitor_data_directory: /var/lib/postgresql/18/monitor
    formation: default
    node_name: db02
- pg_auto_failover_monitor:
    data_directory: /var/lib/postgresql/18/monitor
    postgresql_major_version: 18
    port: 5432
    host_name: 10.20.0.31
    listen_address: 10.20.0.31
    autoctl_node_password: "{{ .secrets.autoctl_node_password }}"
    formation: default
    node_names: [db02]
    nodes_not_listed: remove
```

Delete the monitor with its data directory, in a fleet stage after the one
that deletes the nodes: a node's deletion asks the monitor whether it still
lists the node.

```yaml
- pg_auto_failover_monitor:
    data_directory: /var/lib/postgresql/18/monitor
    state: deleted_with_all_its_data
```

## How it works

1. The module reads pg_autoctl's configuration of the data directory,
   `~postgres/.config/pg_autoctl/<data directory>/pg_autoctl.cfg`, and the
   unit `/etc/systemd/system/pgautofailover.service`.
2. Without pg_autoctl's configuration, the module creates the monitor:
   `pg_autoctl create
   monitor --pgctl ... --pgdata ... --pgport ... --hostname ... --listen
   ... --auth scram-sha-256 --ssl-self-signed`. pg_autoctl starts
   PostgreSQL for the creation and stops it again, which takes 15 to 20
   seconds.
3. The module writes the unit as `pg_autoctl show systemd` prints it,
   reloads systemd, and runs `systemctl enable --now pgautofailover.service`.
   A configuration without its unit is a creation that stopped partway: the
   module runs `pg_autoctl create monitor` again, which finishes it.
4. It makes the local logins ask who logs in, on every run (below), and
   reloads PostgreSQL after a change.
5. When pg_autoctl's `postgresql.listen_addresses` differs from
   `listen_address`, it stops the service, runs `pg_autoctl config set
   postgresql.listen_addresses`, starts the service, and waits, at most a
   minute, until `ss -ltnH` shows PostgreSQL on the port at that address
   alone and it accepts connections.
6. It logs in once as `autoctl_node` at `host_name` over TCP with TLS,
   with the task's password on `psql`'s standard input. When the monitor
   refuses the login, the module sets the password with `psql`'s
   `\password`, which sends the server only the SCRAM-SHA-256 form, and
   logs in again.
7. It creates a missing formation with `pg_autoctl create formation --kind
   pgsql`.
8. The module reads the formation's nodes with
   `pg_autoctl show state --json` and drops each node that `node_names`
   does not list, with `pg_autoctl drop
   node --force`, when `nodes_not_listed` is `remove`.

**The local logins.** pg_autoctl creates the monitor with `initdb
--auth=trust`, whose six lines in `pg_hba.conf` let every account of the
server log in as any role, `postgres` included, without a password, on the
local socket and on `127.0.0.1` and `::1`. The module replaces `trust`
with Debian's authentication methods in exactly the six local socket and
loopback rules created by `initdb`:

```text
local   all             all                                     peer map=pg_autoctl
host    all             all             127.0.0.1/32            scram-sha-256
host    all             all             ::1/128                 scram-sha-256
local   replication     all                                     peer
host    replication     all             127.0.0.1/32            scram-sha-256
host    replication     all             ::1/128                 scram-sha-256
```

and keeps the ident map `pg_autoctl` in the data directory's
`pg_ident.conf`:

```text
pg_autoctl    postgres    autoctl_node
pg_autoctl    /^(.*)$    \1
```

`peer` permits login only when the operating-system account name matches
the PostgreSQL role name. The first line of the `pg_autoctl` ident map also
allows the operating-system account `postgres` to log in as the PostgreSQL
role `autoctl_node`. pg_autoctl runs as `postgres` and uses `autoctl_node`
for the local socket connection to the monitor's database. Every other
line of `pg_hba.conf` stays, pg_autoctl's own `hostssl` lines included.
Every `pg_autoctl` command names the monitor's data directory and reaches
the monitor through the local socket, so the monitor's server needs no
password file.

## Errors and what they mean

Every reason starts with `pg_auto_failover_monitor`.

| The reason holds | What it means, and what to do |
|---|---|
| `the program ... is not installed; install the package ...` | The packages of [Set up](#set-up) are missing. The node task fails, and MMX does not retry the task. |
| `the PostgreSQL extension pgautofailover is not installed; install the package postgresql-VERSION-auto-failover` | The monitor's extension is missing: install its package. |
| `the directory DIRECTORY, which holds the data directory, ...` | The data directory's parent is missing or belongs to another account. pg_autoctl makes a directory `backup` beside the data directory. Create the parent as a directory of `postgres`, as in [Set up](#set-up). |
| `the PostgreSQL cluster VERSION main of Debian's tools uses the port PORT` | Installing `postgresql-<version>` created Debian's cluster `main`. Write `create_main_cluster = false` before the package task, or remove the cluster with `pg_dropcluster --stop VERSION main`. |
| `DIRECTORY holds a pg_autoctl instance of the role keeper, not a monitor` | The data directory belongs to a node. Give the monitor its own data directory. |
| `/etc/systemd/system/pgautofailover.service runs the pg_autoctl instance in DIRECTORY` | The server runs another instance of pg_autoctl, such as a node. One instance runs on a server: give the monitor a server of its own. |
| `pg_auto_failover_monitor.host_name: the monitor in DIRECTORY has the hostname ...`, `pg_auto_failover_monitor.port: ...` | The monitor was created with another host name or port. Every database node keeps the monitor's host name and port in the node's configuration, so the module changes neither connection setting. |
| `node_names does not list the node NAME, which the monitor reports healthy and as the primary (assigned state STATE)` | Dropping the node would cause a failover or remove the only copy of the database that runs. First make another node the primary with `pg_auto_failover_primary`, or include the current primary in `node_names`. The node task fails, and MMX does not retry the task. |
| `pg_auto_failover_monitor.listen_address: must be an IP address of the server` | `listen_address` is a host name or more than one address. Give one IP address, or `*`. |
| `set postgresql.listen_addresses to ADDRESS and started pgautofailover.service, and a minute later PostgreSQL listens on port PORT at ...` | PostgreSQL did not take the address, such as an address that is not on the server, or not yet: read the journal that the reason names. MMX does not try the task again: the next try would find the setting as the task asks. |
| `set the password of autoctl_node, and the monitor still refuses the login as autoctl_node at HOST, port PORT: ...` | The login fails for another reason than the password, such as a `host_name` outside the subnet that pg_autoctl allows. The text behind the colon is `psql`'s message. |
| `created the monitor, and then this failed: ...` | The task created the monitor, and a later step failed. MMX does not try the task again: a new try would find the monitor and report `created: false`. Act on the message and run the playbook again. |
| `creating the monitor in ...`, `dropping the node ...`, `creating the formation ...`, `reading the nodes of the formation ...` | A command of `pg_autoctl` failed; the text behind the colon is its message. MMX tries the task again. |

## Tested on

**Distributions:** Debian 12 and 13, Ubuntu 24.04 and 26.04.

On three servers in x86_64 system containers,
`tests/integration/pg_auto_failover.py` runs the [Set up](#set-up) tasks and
`pg_auto_failover_monitor`, `pg_auto_failover_node`, and
`pg_auto_failover_primary`. The suite `tests/integration/pgbackrest.py`
drops a lost primary with `pg_auto_failover_monitor`:

| Distribution | pg_auto_failover | PostgreSQL |
|---|---|---|
| Debian 12 | 2.0 | 15 |
| Ubuntu 24.04 | 2.1 | 16 |
| Debian 13 | 2.2 | 17 |
| Ubuntu 26.04 | 2.2 | 18 |

- **Not tested:** a monitor whose PostgreSQL comes from another source than
  the distribution, and Citus formations.

## Why it works this way

- **The task lists the formation's nodes.** A server that is gone can be
  dropped only from the monitor's side. With the list of the database
  servers in the playbook, removing a server from that list removes it from
  the formation.
- **A node is dropped with `--force`.** Without `--force`,
  `pg_autoctl drop node` waits for the node to confirm, and fails after a
  minute for a server that is gone.
- **A healthy primary is never dropped.** Dropping a healthy primary would
  cause a failover, or remove the only copy of the database that runs. A
  primary whose server is gone is unhealthy and may be dropped, as when
  every database server is lost and a restored one takes its place.
- **The primary is the node that the monitor has assigned a primary's
  state.** After a failover on the test servers, the old primary, whose
  service was stopped, still reported `primary` while the monitor had
  assigned it `demoted` and the new primary `wait_primary`. The states are
  those in which pg_auto_failover's monitor lets a node take writes. The
  monitor's own function `pgautofailover.get_primary` named the old
  primary for 30 seconds of that failover, while no node could take writes.
- **No `trust` on the local socket and the loopback addresses.** With
  initdb's lines, any account of the monitor's server logged in as
  `postgres` on the test servers. The module checks the lines on every run,
  so a line that comes back is replaced again. The ident map is needed
  because pg_autoctl logs in as `autoctl_node`: with plain `peer`,
  `pg_autoctl show state` failed with "Peer authentication failed".
- **`listen_address` changes with the service stopped.** pg_autoctl gives
  PostgreSQL the address when it starts it. `pg_autoctl config set` while
  the service ran did not keep the value on the test servers: the running
  pg_autoctl wrote its old value back. An IP address, not a host name, so
  that the module can compare it with the listening sockets.
- **TLS with a self-signed certificate and SCRAM passwords, no `trust`.**
  `trust` would let every address of the monitor's subnet register as a
  node.
- **The password is compared by a login.** PostgreSQL stores a salted
  SCRAM verifier, which a module script cannot compute. The login goes to
  `host_name` over TCP with TLS, as a node logs in.
- **The unit is written after the creation, never before.** `pg_autoctl
  show systemd` prints nothing for a data directory without an instance,
  and systemd treats an empty unit file as masked: the service cannot start.
