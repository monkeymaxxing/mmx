# A PostgreSQL server for one application

A server that runs PostgreSQL from the distribution's own packages, with one
role (PostgreSQL's word for an account) and one database for an application
named `shop`.

Set the server's address in `inventories/example.yml`. Choose a password for
the application's role and give it to MMX in an environment variable of the
controller machine, the machine where you run `mmx`:

```sh
export SHOP_DATABASE_PASSWORD='choose-a-long-password'
mmx dryrun configure
mmx run configure
mmx run configure
```

On the node, in order:

1. MMX installs the PostgreSQL server, the `psql` client, and the common
   extensions from the distribution's own packages.
2. MMX starts the server, keeps it starting at boot, and waits until the
   server's port 5432 answers.
3. MMX creates the role `shop`, which may log in with the password.
4. MMX creates the database `shop`, owned by that role, with the encoding
   UTF8.

The second run changes nothing. The first dryrun, before the server is
installed, reports the last two tasks `not_checked`: MMX cannot look into a
server that is not there yet.

The application then connects to the database `shop` as the role `shop` with
the password stored in `shop_database_password`. Write the application's
configuration file with a `file` task that reads `shop_database_password`,
`{{ .secrets.shop_database_password }}`.

## Distributions

The example works on Debian 12 and 13, Ubuntu 24.04 and 26.04, Rocky Linux 9,
and AlmaLinux 10. Each installs the PostgreSQL version of its own packages,
from 13 (Enterprise Linux 9) to 18 (Ubuntu 26.04).

What the playbook does differently on each:

| Distribution | Packages | The database cluster |
|---|---|---|
| Debian, Ubuntu | `postgresql` | The package's installation makes it. |
| Red Hat family | `postgresql-server`, `postgresql-contrib` | `postgresql-setup --initdb` |

## Why it is written this way

**No login for the two database tasks.** MMX runs `psql` as the operating
system account `postgres`, which the server's package creates. A server
accepts that account as its administrator through its local socket, without
a password. So the playbook holds no administrator password.

**The password comes from a secret.** `mmx_project_settings.yml` binds the
secret `shop_database_password` to the environment variable
`SHOP_DATABASE_PASSWORD`. To keep the password in the project's encrypted
vault instead, remove those two lines, run `mmx secrets edit`, and enter
`shop_database_password: your-value`; the playbook stays the same. MMX never
sends the password itself to the server: it computes the form in which
PostgreSQL stores a password and sends that.

**MMX starts the server, not the package.** MMX keeps a package from
starting its services, so that a service never runs before its
configuration is written. The `systemd_service` task starts the server on
every distribution.

**The wait on the port.** On Debian and Ubuntu, `postgresql.service` only
groups the clusters and is `active` also while a server is down; the
server's own unit is `postgresql@<version>-main.service`. The `wait_for`
task is what proves that the server answers.

To change the server's settings, add a `file` task for a file in the
server's configuration directory in the `changes` of a node task block, with
the `systemd_service` task in its `activate`.

For every field, a server on another machine, and privileges inside a
database, see the [`postgresql_role`](../../docs/reference/modules/postgresql_role.md)
and [`postgresql_database`](../../docs/reference/modules/postgresql_database.md) pages.
