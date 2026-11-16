# `postgresql_database`

`postgresql_database` manages one database inside a PostgreSQL server that
is already installed and running. The module manages the database's owner
and extensions, or removes the database with all the database's data.
A [`postgresql_role`](postgresql_role.md) task manages the role that owns
the database and that an application logs in with.

The module runs `psql`, PostgreSQL's own command line client, on the node;
MMX contains no database driver. Installing the server, writing its
configuration files, and starting it are package, `file`, and
`systemd_service` tasks; the first example below shows them.

Without connection fields, a task uses the server on the same node: MMX runs
`psql` as the operating system account `postgres`, which the server accepts
as its administrator through its local socket, without a password. The
server's `pg_hba.conf` must allow the local login as `postgres`. For a
server on another machine or another login, the module takes the connection
fields that [`postgresql_role`](postgresql_role.md) takes too. The page
[A server on another machine, or another login](../../docs/how-to/manage-a-database.md#a-database-server-on-another-machine)
describes them, how a task fails when it cannot reach the server, and what
`change` a failed task reports. The design section
[How a task reaches the server](../../docs/changing-mmx/design/modules-applications.md#how-a-task-reaches-the-server)
has the design and its reasons. A task with `login_password` hands `psql`
the password in a file in the
[node task's private directory](../../docs/reference/secrets.md#a-node-tasks-private-directory),
which only the account that runs `psql` can open and which MMX removes after
the task.

## Examples

### A PostgreSQL server for one application

The example project
[`examples/postgresql-server-for-one-application`](../../examples/postgresql-server-for-one-application/)
is this playbook as a whole project. It works on Debian 12 and 13 and Ubuntu 24.04 and 26.04. Before running the example, set `SHOP_DATABASE_PASSWORD` in the
controller machine's environment.

```yaml
# playbooks/configure.yml
fleet_stages:
  - id: configure
    nodes: all

    node_tasks:
      # Debian and Ubuntu: one package brings the server, the psql client,
      # and the common extensions.
      - when: {equals: ["{{ .node.package_manager }}", apt]}
        apt_package: {packages: [{name: postgresql, version: newest_when_first_installed}]}

      # The Red Hat family: the common extensions are a package
      # of their own.
      - when: {equals: ["{{ .node.package_manager }}", dnf]}
        dnf_package: {packages: [{name: postgresql-server, version: newest_when_first_installed}, {name: postgresql-contrib, version: newest_when_first_installed}]}

      # The Red Hat family: make the database cluster, once.
      - when: {equals: ["{{ .node.package_manager }}", dnf]}
        shell:
          script: postgresql-setup --initdb
          skip_if_this_path_exists: /var/lib/pgsql/data/PG_VERSION

      - systemd_service:
          name: postgresql
          state: running
          start_at_boot: true

      - wait_for:
          tcp_port_accepts_connections: 5432
          wait_at_most: 60s

      - postgresql_role:
          name: shop
          password: "{{ .secrets.shop_database_password }}"
          can_log_in: true

      - postgresql_database:
          name: shop
          owner: shop
          encoding: UTF8
```

On each node, in order:

1. A package task installs the server and the `psql` client from the
   distribution's own packages.
2. Where the package made no **database cluster** (the server's data
   directory), a `shell` task makes it. The task has `skip_if_this_path_exists`, so the
   command runs once.
3. The `systemd_service` task starts the server and keeps it starting at
   boot. The `wait_for` task continues once the server's port answers.
4. `postgresql_role` creates the role `shop`, which may log in, with the
   password of the secret `shop_database_password`. The application's own
   configuration file reads the secret `shop_database_password`.
5. `postgresql_database` creates the database `shop`, owned by the role
   `shop`.

The two database tasks need no login of their own: MMX runs `psql` as the
operating system account `postgres`, which the server on the same node
accepts as its administrator through its local socket, without a password. A
second run changes nothing.

What the package leaves to do differs between the distributions:

| Distribution | Packages | The database cluster | What starts the server |
|---|---|---|---|
| Debian, Ubuntu | `postgresql` | Installing the package creates the database cluster. | The `systemd_service` task. [MMX keeps a package from starting its services](apt_package.md#packages-do-not-start-their-services). |
| Red Hat family | `postgresql-server`, and `postgresql-contrib` for the common extensions | `postgresql-setup --initdb`; until then `systemctl start postgresql` fails. | The `systemd_service` task. |

**On Debian and Ubuntu, `postgresql.service` does not say whether the server
runs.** The `postgresql.service` unit only groups the clusters and stays
`active` while a server is down. The server's own unit is
`postgresql@<version>-main.service`, such as `postgresql@17-main.service`.
Starting and enabling `postgresql` starts and enables every cluster, so the
task above is right on every distribution, and the `wait_for` on the port is
what proves that the server answers.

Without the `psql` program on the node, `postgresql_role` and
`postgresql_database` fail with a message that names the program and the
package containing `psql`. A dryrun reports the task `not_checked`.
When no server answers, a run also fails and a dryrun also reports
`not_checked`. For example, before the server is installed, a dryrun of the
playbook above reports the two database tasks `not_checked`.

### A database with an extension

```yaml
- postgresql_database:
    name: shop
    owner: shop
    encoding: UTF8
    extensions: [pg_trgm]
    extensions_not_listed: keep
```

The task reads the database from the server's catalog (the server's own
tables about its roles and databases) and does what is missing: it creates
the database `shop`, gives it the owner `shop`, and installs the extension
`pg_trgm` in it. Other installed extensions stay. A second run changes
nothing.

### Delete a database with all its data

```yaml
- postgresql_database:
    name: shop_staging
    state: deleted_with_all_its_data
```

`state: deleted_with_all_its_data` drops the database with everything in it
and accepts only `name` and the connection fields. A missing database is
unchanged.

### Privileges inside a database

Ownership and the predefined roles cover the common cases: the application's
role owns its database, and a reporting role is a member of
`pg_read_all_data`. For finer privileges, keep the `GRANT` statements in an
SQL file. Install the file in the `changes` of a node task block and apply
it in `activate`, so that it runs again only when it changed:

```yaml
- task_block:
    id: shop_privileges
    changes:
      - file:
          path: /etc/shop/privileges.sql
          source: files/shop-privileges.sql
          owner: postgres
          permissions_mode: "0600"
    activate:
      - id: apply_shop_privileges
        shell:
          script: psql --no-psqlrc --set ON_ERROR_STOP=1 --dbname=shop --file=/etc/shop/privileges.sql
          run_as_user_account: postgres
          only_when_tracked_inputs_changed: true
```

## How it works

Omitting `state` selects `present`.

The result's `value` has `created`, `true` when the task created the
database, and `changed_attributes`, as for a role; the attributes of a
database are `owner` and `extensions`. A dryrun reports the same and changes
nothing.

- **The encoding and the locale of an existing database cannot change.**
  PostgreSQL has no statement for changing an existing database's encoding
  or locale. When an existing database differs from the task's `encoding`
  or `locale`, the task fails, names the difference, and changes nothing.
- **Two spellings of one locale are equal.** The server keeps the spelling
  that created a database: `en_US.UTF-8` on Debian, `en_US.utf8` on
  Enterprise Linux. MMX compares `locale` with the database's collation and
  character type, and treats the spellings of one locale as equal: the part
  after the dot without regard to case and hyphen, and `POSIX` as `C`. When
  `locale` names the template's locale in another spelling, MMX creates the
  database with the template's spelling, because the server compares the
  two as text and would refuse. An encoding is compared by the server's own
  name for it, so `utf8` is `UTF8`.
- **Another locale or encoding than the template's needs
  `template_database: template0`.** PostgreSQL copies a database from every
  other template only with the template's own encoding and locale. MMX reads
  the template before it creates anything. Without `template0`, the task
  fails in a run and in a dryrun, names the difference, and changes nothing.
  The template database is `template1` unless the task names another.
- **The template database must exist.** A missing template database fails
  the task in a run. A dryrun reports `not_checked`, because an earlier
  node task may create the database.
- **The owner is required with `state: present` and must exist.** If the
  role named by `owner` does not exist, the task fails in a run. A dryrun
  reports `not_checked`, because an earlier node task may create the role.
  An existing database with another owner gets the task's owner. From
  PostgreSQL 16, a login role that is no superuser must be a member of the
  role requested in the task's `owner` field. The page
  [connection section](../../docs/how-to/manage-a-database.md#a-database-server-on-another-machine)
  says what else a login role must be allowed to do.
- **A name that is longer than the server takes fails the task** before it
  changes anything, as for a role: the limit is 63 bytes unless the server
  was built otherwise.
- **The files of an extension must be on the server's machine.** MMX asks
  the server before it installs an extension. When the files are missing,
  the task fails as a missing prerequisite before it changes anything, and
  names the package with the common extensions (`postgresql-contrib` on the
  Red Hat family); a dryrun reports `not_checked`.
- **`extensions_not_listed`** has no default. Supply `extensions` and
  `extensions_not_listed` together, or omit both fields. Omitting both
  fields leaves installed extensions unchanged. `remove` drops every
  installed extension that `extensions` does not name, with a plain
  `DROP EXTENSION`, except PostgreSQL's built-in `plpgsql`.
  `plpgsql` stays by a rule of MMX's
  own; the server would let a superuser drop it. When other objects need an
  extension, the server refuses and the task fails with its message. All
  extension changes of one task happen together or not at all.
- **A run that stopped part of the way is finished by the next.** Creating a
  database cannot be undone together with what follows it. When a later step
  fails, the result says that the database was created, and the next run
  finds the database and installs what is missing.

### Other sessions

PostgreSQL drops no database that a session is connected to.
PostgreSQL waits five seconds for the sessions to end, then the task fails
with the server's message. `CREATE DATABASE` likewise waits five seconds
and then fails while a session is connected to the template database.
A dryrun does not look for such sessions, because the dryrun cannot know
which sessions a run will meet.

### Server certificate verification

With `database_server_host`, the client verifies the server by default.
MMX sets `PGSSLMODE=verify-full` for `psql`, which checks the authority
and server host name. Give `database_server_certificate_authority_file` for
a private authority or a self-signed certificate. Without that input,
`psql` needs [trusted roots](https://www.postgresql.org/docs/13/libpq-ssl.html) in `~/.postgresql/root.crt` for the account running
it (root for `database_server_host`). This also works with PostgreSQL 13;
MMX does not require the newer `sslrootcert=system` option.

`dangerously_skip_server_certificate_check: true` turns verification off.
The client then uses encryption when offered and can connect without it.
Give either `database_server_certificate_authority_file` or
`dangerously_skip_server_certificate_check: true`; a task that gives both is
refused because the two inputs ask for different certificate checks.
A local socket connection is unchanged. Certificate settings require
`database_server_host`; MMX rejects certificate settings for a local socket
connection. The
[connection design](../../docs/changing-mmx/design/modules-applications.md#how-a-task-reaches-the-server)
explains the fields shared by `postgresql_role`, `postgresql_database`,
`mysql_user`, and `mysql_database`.

The psql client setup also applies to
[`postgresql_role`](postgresql_role.md#server-certificate-verification).

## Tested on

**Distributions:** Debian 12 and 13, Ubuntu 24.04 and 26.04, Rocky Linux 9, AlmaLinux 10.

The suite `tests/integration/postgresql_databases.py` runs on Debian 12 and 13, Ubuntu 24.04 and 26.04, Rocky Linux 9, and AlmaLinux 10, in Incus containers on the Debian family and in virtual machines on the Red Hat family, against the PostgreSQL that each
distribution packages: PostgreSQL 13 (Rocky Linux 9), 15, 16 (AlmaLinux 10 and Ubuntu 24.04), 17, and 18. On
Ubuntu 26.04 it is PostgreSQL 18.6. The suite also passed on Hetzner
Cloud servers with Debian 13. The example project `postgresql-server-for-one-application`
runs on Debian 12 and 13, Ubuntu 24.04 and 26.04, Rocky Linux 9, and AlmaLinux 10.

Not tested: a second PostgreSQL server on one node, chosen with
`database_server_port`.

## Why it works this way

- **Remote servers are verified by default.** SQL and login information
  must reach the intended server. Supplying
  `database_server_certificate_authority_file` names a trusted certificate
  authority and keeps server verification.
  `dangerously_skip_server_certificate_check` removes server verification,
  following [protective choices](../../docs/changing-mmx/design/modules.md#protective-choices-come-in-three-levels).

- **Every present database names its owner.** The login role is not a safe
  guess for application ownership. Requiring an explicit `owner` follows the
  [required-setting rule](../../docs/changing-mmx/design/modules.md#a-setting-with-no-safe-guess-is-required).

- **The database cluster is a `shell` task, not a module.** It is one
  command that each distribution names differently and that runs once in a
  server's life.
- **No general SQL module and no PostgreSQL privileges module.** A general
  module could not tell whether a password or a set of privileges differs.
  Ownership, the predefined roles, and an applied SQL file cover the common
  grants.
- **`deleted_with_all_its_data` says what goes.** A reader sees from the one
  line that data is deleted.
