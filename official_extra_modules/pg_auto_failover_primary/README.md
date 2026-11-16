# `pg_auto_failover_primary`

`pg_auto_failover_primary` makes one node of a
[pg_auto_failover](https://pg-auto-failover.readthedocs.io/) formation its
primary: a planned switchover. pg_auto_failover keeps a PostgreSQL primary
and its replicas in one *formation*; its *monitor*, on a server of its own,
decides which node is the primary.
[`pg_auto_failover_monitor`](../pg_auto_failover_monitor/README.md)
manages the monitor, and
[`pg_auto_failover_node`](../pg_auto_failover_node/README.md) each node.

The module runs `pg_autoctl` as the account `postgres` on the monitor's
server, which is a **node** of MMX: a Linux server that MMX manages. When
the monitor has made `node_name` the primary already, the task is
unchanged. Otherwise the module
runs `pg_autoctl perform promotion`, which waits until the monitor has
made the node the primary, about 8 seconds on the test servers. Then the
task waits, at most five minutes, until the formation has
[settled](../pg_auto_failover_node/README.md#how-it-works) with the new
and the old primary healthy in it: the old primary has demoted and caught
up as a replica. A task also starts only on a settled formation, so it
waits for a failover or a replica's catching up that still goes on. With
synchronous replication the switchover loses no write.

## Set up

The module needs the monitor's server as its
[Set up](../pg_auto_failover_monitor/README.md#set-up) leaves it: the
packages, and a monitor that `pg_auto_failover_monitor` created. The task
runs in a fleet stage on that server:

```yaml
# mmx run switch_primary --var new_primary=db02
command_line_vars:
  new_primary: {type: string}
fleet_stages:
  - id: switch_primary
    nodes: [monitor]
    node_tasks:
      - pg_auto_failover_primary:
          monitor_data_directory: /var/lib/postgresql/18/monitor
          formation: default
          node_name: "{{ .vars.new_primary }}"
```

## Inputs

| Input | Meaning | When omitted |
|---|---|---|
| `monitor_data_directory` | The monitor's data directory on the monitor's server, such as `/var/lib/postgresql/18/monitor`. Use the value of `data_directory` from the `pg_auto_failover_monitor` task on the monitor's server. | Required. |
| `formation` | The formation, such as `default`. | Required. |
| `node_name` | The node that becomes the primary. It must be a healthy replica, in the state `secondary`, or the primary already. | Required. |

## Outputs

| Output | Meaning |
|---|---|
| `primary_node_name` | The formation's primary after the task: `node_name`. |

A later node task reads it as `.node_tasks.<id>.primary_node_name`.

## What a dryrun reports

A dryrun reads the formation with `pg_autoctl show state --json` and
changes nothing.

- **Another node is the primary:** `would_change`, `would make the node
  db02 the primary of the formation default in place of db01`.
- **`not_checked`** while `pg_autoctl` or the account `postgres` are
  missing, while the monitor does not exist or does not answer, while the
  formation has no such node, while the node is in maintenance, while the
  formation has not settled (`the formation FORMATION is not settled: ...;
  a run waits for it, at most 300 seconds`), and while the node is no
  healthy replica: an earlier node task may create the monitor or the
  node, or end the maintenance.
- **A failure, as in a run,** for an input that breaks a rule.

## Examples

Make `db02` the primary before maintenance on `db01`:

```yaml
- pg_auto_failover_primary:
    monitor_data_directory: /var/lib/postgresql/18/monitor
    formation: default
    node_name: db02
```

A rolling update of the database servers, one node at a time, with the
primary last: a fleet stage switches the primary to a node that is done,
then the old primary goes into maintenance and back, as the other nodes
did. The page of
[`pg_auto_failover_node`](../pg_auto_failover_node/README.md#examples)
shows maintenance.

## Errors and what they mean

Every reason starts with `pg_auto_failover_primary`.

| The reason holds | What it means, and what to do |
|---|---|
| `the program pg_autoctl is not installed; install the package pg-auto-failover-cli in an earlier node task` | Run the task on the monitor's server, after the tasks of [Set up](#set-up). |
| `DIRECTORY holds no pg_auto_failover monitor` | The monitor is missing, or the `pg_auto_failover_primary` task runs on another server. Create the monitor with `pg_auto_failover_monitor`. Run the `pg_auto_failover_primary` task on the monitor's server. |
| `the formation FORMATION has no node NAME` | The node named by `node_name` is not registered in the formation. Register the node with `pg_auto_failover_node` in an earlier fleet stage, or correct `node_name`. A run fails without another try. |
| `the node NAME is in maintenance (STATE), and only a healthy replica becomes the primary` | End the maintenance first with `pg_auto_failover_node` and `state: present`. A run fails without another try. |
| `the formation FORMATION has not settled in 300 seconds: NODE is STATE (assigned STATE, healthy)` | Before the switchover: a failover or a replica's catching up went on for more than five minutes. After it: the old primary did not rest as a healthy replica; the switchover is made, and `primary_node_name` names the new primary. Read `pg_autoctl show state` and `pg_autoctl show events` on the monitor's server. MMX does not try the task again. |
| `the node NAME is STATE and not healthy, and only a healthy replica (secondary) becomes the primary` | The monitor does not see the node as healthy, such as one whose server is down. MMX tries the task again. |
| `making the node NAME the primary of the formation FORMATION: ...` | `pg_autoctl perform promotion` failed; the text behind the colon is its message. |
| `pg_autoctl perform promotion ended, and the monitor reports the node NAME ..., not primary` | The switchover did not finish. Read the monitor's state with `pg_autoctl show state` on the monitor's server. |
| `the monitor does not answer: ...` | `pg_autoctl` could not read the monitor. MMX tries the task again. |

## Tested on

**Distributions:** Debian 12 and 13, Ubuntu 24.04 and 26.04.

The integration suite `tests/integration/pg_auto_failover.py` switches the
primary of a formation of two nodes to the replica and back, and checks
that every row stays and that the formation has settled when the task
ends; after a failover, the task for the new primary is
unchanged while the old one still reports `primary`. In system containers
on x86_64:

| Distribution | pg_auto_failover | PostgreSQL |
|---|---|---|
| Debian 12 | 2.0 | 15 |
| Ubuntu 24.04 | 2.1 | 16 |
| Debian 13 | 2.2 | 17 |
| Ubuntu 26.04 | 2.2 | 18 |

## Why it works this way

- **The task runs on the monitor's server.** `pg_autoctl` reaches the
  monitor there through the monitor's data directory and the local socket,
  where `pg_auto_failover_monitor`'s ident map lets the account `postgres`
  in as `autoctl_node`, so the task needs no password.
- **The primary is the node that the monitor has assigned a primary's
  state** (`single`, `wait_primary`, `primary`, `join_primary`, or
  `apply_settings`), not the one that reports it. After a failover, the old
  primary that died may still report `primary`; the task does not count it
  as the primary.
- **A primary that is the primary already is unchanged.** `pg_autoctl
  perform promotion` returns success for it too, with only a notice; the
  module reads the state first, so a rerun reports `unchanged`.
- **The task ends on a settled formation.** `pg_autoctl perform
  promotion` returns while the old primary still demotes and catches up.
  A playbook's next step, such as maintenance or a restart of the old
  primary, would meet a formation that still changes; a restart of a node
  that the monitor is promoting stopped the promotion partway on a pair of
  database servers (see
  [`pg_auto_failover_node`](../pg_auto_failover_node/README.md#why-it-works-this-way)).
- **Only a healthy replica becomes the primary.** pg_autoctl promotes a
  node only from the state `secondary`; the module names the node's state
  instead of letting pg_autoctl wait for a minute.
- **Maintenance on the primary is not a way to switch.** `pg_autoctl enable
  maintenance` on a primary fails it over to a replica that the monitor
  chooses. The `pg_auto_failover_primary` task chooses the node.
