# `mysql_database`

`mysql_database` manages one database inside a MySQL or MariaDB server that
is already installed and running: it creates the database with its
character set and collation, or drops it with all its data. The account that
an application logs in with is a [`mysql_user`](mysql_user.md) task.
`mysql_database` and `mysql_user` work with MySQL and with MariaDB.

The module runs the server's own client program on the node: `mariadb`, or
`mysql` on a node that has no `mariadb`. The server's package brings the
client. MMX sends SQL to the client and reads the server's catalog (the
server's own tables that list its accounts, privileges, and databases)
before and after, so a rerun that finds everything in place changes nothing.
Installing the server, its configuration files, and starting it are
package, `file`, and `systemd_service` tasks; the first two examples below
show them. No module installs the server or makes its data directory.

Without connection fields, a task uses the server on the same node through
the server's local socket: MMX runs the client as root, which the server
accepts as `root@localhost`. For a server on another machine, or a server
that needs a password from root, the module takes the connection fields
that `mysql_user` takes too. The page describes the connection fields
in
[A MySQL or MariaDB server on another machine, or another login](../../docs/how-to/manage-a-database.md#a-database-server-on-another-machine).
The page also explains task failures when the server cannot be
reached or refuses the login. The design section
[How a task reaches the server](../../docs/changing-mmx/design/modules-applications.md#how-a-task-reaches-the-server)
has the design and its reasons. A task with connection fields hands the
client its login in a file in the
[node task's private directory](../../docs/reference/secrets.md#a-node-tasks-private-directory),
which only root can open and which MMX removes after the task.

## Examples

### A MariaDB server for one application

The example project
[`examples/mariadb-server-for-one-application`](../../examples/mariadb-server-for-one-application/)
installs MariaDB and creates one database and one account. The example
project's node tasks are below. Before running the example, set
`SHOP_DATABASE_PASSWORD` in the controller machine's environment.

```yaml
# playbooks/configure.yml
fleet_stages:
  - id: configure
    nodes: all

    node_tasks:
      - when: {equals: ["{{ .node.package_manager }}", apt]}
        apt_package: {packages: [{name: mariadb-server, version: newest_when_first_installed}]}
      - when: {equals: ["{{ .node.package_manager }}", dnf]}
        dnf_package: {packages: [{name: mariadb-server, version: newest_when_first_installed}]}

      - systemd_service:
          name: mariadb
          state: running
          start_at_boot: true

      - mysql_database:
          name: shop
          character_set: utf8mb4

      - mysql_user:
          name: shop
          may_connect_from: localhost
          password: "{{ .secrets.shop_database_password }}"
          privileges:
            - grant_on: "shop.*"
              grant: [ALL]
          privileges_not_listed: remove
```

On the node, in order:

1. The package task installs MariaDB's server and the client program
   `mariadb`.
2. The `systemd_service` task starts the server and enables it for the next
   boot.
3. `mysql_database` creates the database `shop`.
4. `mysql_user` creates the account `shop@localhost` with the password from
   the secret `shop_database_password`, and gives it every privilege on the
   database `shop` and no other privilege.

The `mysql_database` and `mysql_user` tasks omit connection fields. MMX
runs `mariadb` as root on the node. MariaDB accepts the node's root account
as `root@localhost` through the server's local socket on every supported
distribution.

The example works on Debian 12 and 13, Ubuntu 24.04 and 26.04, Rocky Linux
9, and AlmaLinux 10.

### The same tasks for a MySQL server

The MariaDB example's `mysql_database` and `mysql_user` tasks also work
with MySQL. The MySQL package and service unit differ from MariaDB's:

| Distribution | Package | Unit |
|---|---|---|
| Ubuntu 24.04 and 26.04 | `mysql-server` | `mysql` |
| Rocky Linux 9 | `mysql-server` | `mysqld` |
| AlmaLinux 10 | `mysql8.4-server` | `mysqld` |

Debian packages no MySQL server.

```yaml
- dnf_package: {packages: [{name: mysql-server, version: newest_when_first_installed}]}

# Only on the Red Hat family; see below.
- file:
    path: /etc/my.cnf.d/auth_socket.cnf
    permissions_mode: "0644"
    content: |
      [mysqld]
      plugin-load-add=auth_socket.so

- systemd_service:
    name: mysqld
    state: running
    start_at_boot: true

# Only on the Red Hat family; see below.
- shell:
    script: mysql --no-defaults --execute "ALTER USER 'root'@'localhost' IDENTIFIED WITH auth_socket"
    skip_if_this_command_succeeds: mysql --no-defaults --batch --skip-column-names --execute "SELECT plugin FROM mysql.user WHERE user = 'root' AND host = 'localhost'" | grep --quiet --line-regexp auth_socket

- mysql_database:
    name: shop

- mysql_user:
    name: shop
    may_connect_from: localhost
    password: "{{ .secrets.shop_database_password }}"
    privileges:
      - grant_on: "shop.*"
        grant: [ALL]
    privileges_not_listed: remove
```

**MySQL on the Red Hat family leaves root open.** After the
installation there, the server's account `root@localhost` has an empty
password. Every account of the node can then log in to the server as root,
through the socket and over TCP to `127.0.0.1`, without a password. Ubuntu's
MySQL and every MariaDB accept only the node's root account as
`root@localhost`. No MMX module closes this for you. The two tasks marked
"see below" do it:

- The `file` task makes the server load MySQL's `auth_socket` plugin when it
  starts. With that plugin, the server accepts a login as `root@localhost`
  only from the node's root account, through the socket.
- The `shell` task tells the server to use the plugin for `root@localhost`.
  Its `skip_if_this_command_succeeds` makes it run only while root still
  logs in another way.

Afterwards `mysql_user` and `mysql_database` tasks need no login, as on
Ubuntu and with MariaDB. If you give root a password instead, every such
task needs `login_password` (see
[A MySQL or MariaDB server on another machine, or another login](../../docs/how-to/manage-a-database.md#a-database-server-on-another-machine)).

### A database with its character set and collation

For a new database, omitted `character_set` and `collation` values are left
to the server's defaults.

```yaml
- mysql_database:
    name: shop
    character_set: utf8mb4
    collation: utf8mb4_unicode_ci
```

The task creates the database `shop` with the character set `utf8mb4` and
the collation `utf8mb4_unicode_ci`. A second run finds the database with
character set `utf8mb4` and collation `utf8mb4_unicode_ci`, so MMX changes
nothing.

### A database on a server on another machine

```yaml
- mysql_database:
    name: shop
    database_server_host: db.example.com
    login_user: administrator
    login_password: "{{ .secrets.database_administrator_password }}"
    database_server_certificate_authority_file: /etc/ssl/database-authority.pem
```

This node task runs on the node, for example an application server, and
creates the database on the server `db.example.com`. The node needs the
client program: install the client package first (`mariadb-client` on Debian
and Ubuntu, `mariadb` on the Red Hat family). A database server that is
itself a node needs none of these fields: its tasks run on it, through the
socket.

### Delete a database with all its data

```yaml
- mysql_database:
    name: shop_staging
    state: deleted_with_all_its_data
```

`state: deleted_with_all_its_data` drops the database with every table in it
and accepts only `name` and the connection fields. A database that does not
exist is unchanged. MMX refuses to delete the servers' own databases
`mysql`, `information_schema`, `performance_schema`, and `sys`. A task that
dropped a database says so in its `reason`: "dropped the database
shop_staging with all its data".

## How it works

The database's `name` is taken exactly as the server keeps it: upper and
lower case are different names.

An existing database whose character set or collation differs from the task
fails the task, and MMX changes nothing. Changing the database's character
set and collation defaults would affect only tables created later.
Existing tables would keep the previous character set and collation
settings, which could differ from the database's new defaults. The failure
names what the database has and what the task asks for.

- **A task with only `character_set`** compares only the character set: the
  collation that a server picks for a character set differs between MySQL
  and MariaDB and between their versions.
- **`utf8` and `utf8mb3` are one character set.**
- **MariaDB's short names of collations.** MariaDB 10.11 and later have
  short names for some collations, such as `uca1400_ai_ci`. The server
  stores such a collation with the database's character set in front,
  `utf8mb4_uca1400_ai_ci`. MMX takes both forms as the same collation, so a
  task may write either.

The result's `value` has `created` and `changed_attributes`, as for
`mysql_user`; `changed_attributes` is always empty, because MMX changes
nothing on an existing database. A dryrun reports `would_change` for a
database that a run would create or drop, with the `reason` "would create
the database shop" or "would drop the database shop with all its data".

A `mysql_database` task is no tracked input. In the `changes` of a node task
block it runs like anywhere else, and the block's `systemd_service` task or
command does not follow it: a database in the server is no reason to
restart a service.

### A lock that another session holds

A statement that changes the server sometimes has to wait for another
session of the server. The usual case: an application has an open
transaction that read a table, and a task with
`state: deleted_with_all_its_data` drops the table's database. The server
makes the task's statement wait until that transaction ends.

`mysql_database` and `mysql_user` wait 10 seconds for such a lock (`SET
SESSION lock_wait_timeout = 10` in the same call). Then the server ends the
statement with its error 1205, "Lock wait timeout exceeded", and has changed
nothing. The task fails with that message and is tried again, as after other
failures
([node task retries](../../docs/reference/playbook-file.md#node-task-retries-and-time-limits)),
because the other session may have ended. To drop a database that an
application still uses, stop the application in an earlier node task.

### Server certificate verification

With `database_server_host`, the client verifies the server by default.
MMX requests `ssl-mode=VERIFY_IDENTITY` from MySQL's client ([client options](https://dev.mysql.com/doc/refman/8.4/en/connection-options.html#option_general_ssl-mode)). That client
needs a CA certificate supplied through `database_server_certificate_authority_file`.
For MariaDB's client, MMX requests `ssl-verify-server-cert`. Older clients
need a CA file; MariaDB 11.4 and newer can also verify a MariaDB server's
certificate through [password authentication](https://mariadb.com/docs/server/security/encryption/data-in-transit-encryption/zero-configuration-ssl). With a CA file, the client
checks the authority and host name. MariaDB's client 11.8 checks the authority
but not the address for `127.0.0.1`.

`dangerously_skip_server_certificate_check: true` turns verification off.
The client then uses encryption when offered and can connect without it.
Give either `database_server_certificate_authority_file` or
`dangerously_skip_server_certificate_check: true`; a task that gives both is
refused because the two inputs ask for different certificate checks.
A local socket connection is unchanged. The
[connection design](../../docs/changing-mmx/design/modules-applications.md#how-a-task-reaches-the-server)
explains the fields shared by the four database modules.

## Errors and what they mean

**A name that the server does not have.** MMX checks the server's catalog
for the task's character set and collation before MMX changes anything.
In a run and in a dryrun, the task fails for unknown names or a collation
belonging to another character set:

- The server has no character set with the name requested in `character_set`.
- The server has no collation with the name requested in `collation`.
  The two products have different collations: `utf8mb4_0900_ai_ci` is
  MySQL's, and MariaDB 10.5 does not have it.
- The collation belongs to another character set than `character_set`, such
  as `utf8mb4_unicode_ci` for `latin1`.

The failure names the field and the server's statement that lists the names,
`SHOW CHARACTER SET` or `SHOW COLLATION`. The task with an unknown or
mismatched character set or collation is not retried (`failure_kind: config`).
Another try would read the server catalog already checked before the
failure.

**An existing database with another character set or collation** fails the
task in a run and in a dryrun. The failure names the database's current
character set and collation, and the differing values requested by the
task. See [How it works](#how-it-works).

**`not_checked` in a dryrun.** A dryrun reports `not_checked` while the
client program is missing or the server does not answer; a run fails.

**Error 1205, "Lock wait timeout exceeded".** Another session held a lock
for more than 10 seconds; see
[A lock that another session holds](#a-lock-that-another-session-holds).

The MySQL and MariaDB client setup also applies to
[`mysql_user`](mysql_user.md#server-certificate-verification).

## Tested on

**Distributions:** Debian 12 and 13, Ubuntu 24.04 and 26.04, Rocky Linux 9, AlmaLinux 10.

The suite `tests/integration/mysql_databases.py` runs on Debian 12 and 13, Ubuntu 24.04 and 26.04, Rocky Linux 9, and AlmaLinux 10 (in Incus containers on the Debian family, in virtual machines on the Red Hat family): MariaDB 10.11 and 11.8 on the Debian family, MariaDB 10.5 on Rocky Linux 9 and 10.11 on AlmaLinux 10, and MySQL 8.0 on Rocky Linux 9 and 8.0 and 8.4 on Ubuntu 24.04 and 26.04 and 8.4 on AlmaLinux 10, which package
it. On Ubuntu 26.04 it is MariaDB 11.8.6 and MySQL 8.4.11. The suite also passed
on Hetzner Cloud servers with Debian 13.

Not tested: MySQL from the vendor's own repository, as on Debian, which
packages only MariaDB.

[Limits on each distribution](../../docs/reference/limits-on-each-distribution.md) says what is limited for this module on [Debian 12](../../docs/reference/limits-on-each-distribution.md#debian-12) and [Debian 13](../../docs/reference/limits-on-each-distribution.md#debian-13), and why.

## Why it works this way

- **Remote servers are verified by default.** SQL and login information
  must reach the intended server. Supplying the certificate authority's
  certificate through `database_server_certificate_authority_file` keeps
  server identity verification enabled.
  `dangerously_skip_server_certificate_check` removes server identity
  verification,
  following [protective choices](../../docs/changing-mmx/design/modules.md#protective-choices-come-in-three-levels).

- **A character set or a collation is checked against the server's catalog
  first.** The server's own error for an unknown name comes only in a run,
  and MMX would try the task again. The catalog gives the same answer in a
  dryrun, before anything changes.
- **An existing database is never changed.** A changed default applies only
  to tables created later and would mislead.
- **The servers' own databases cannot be deleted.** Without them a server
  does not work, and a task that deletes one is a mistake, not a wish.
- **`deleted_with_all_its_data` says what goes.** A reader sees from the one
  line that data is deleted.
- **A statement that changes the server waits 10 seconds for a lock.** The
  servers' own limit, `lock_wait_timeout`, is a year on MySQL and a day on
  MariaDB, and a node task has no time limit unless the project sets one. One
  open transaction of an application would hold a task for that long. On
  MySQL, a `DROP DATABASE` that waits also stays in the server after its
  client ended, and drops the database when the other session ends, perhaps
  hours after the run. Measured on MariaDB 10.5 and 11.8 and on MySQL 8.0.
- **No module installs the server.** A package task does, and the package
  or the server's first start makes its data directory.
