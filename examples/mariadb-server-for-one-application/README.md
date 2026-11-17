# A MariaDB server for one application

Set the server address in `inventories/example.yml`, give the application's
database password in the environment of the controller machine, and run:

```sh
export SHOP_DATABASE_PASSWORD='the password of the application'
mmx dryrun configure
mmx run configure
mmx run configure
```

On the node, in order:

1. MMX installs MariaDB's server from the distribution's own packages. The
   package brings the client program `mariadb`.
2. MMX starts the server and enables it for the next boot.
3. MMX creates the database `shop` with the character set `utf8mb4`.
4. MMX creates the account `shop@localhost` with the password, and gives
   the account every privilege on the database `shop` and no other
   privilege.

The second run changes nothing. A run after you changed the password in the
environment changes only the account's password.

**The password.** `mmx_project_settings.yml` reads the secret
`shop_database_password` from the environment variable
`SHOP_DATABASE_PASSWORD`. To keep `shop_database_password` in the project's
encrypted vault instead, remove `secrets_from_env` from
`mmx_project_settings.yml`. Store the secret with
`mmx secrets set shop_database_password`. The
application's own configuration file reads `shop_database_password`. MMX
computes the form in which the server stores the password and sends only
that form, so the password is in no statement and in none of the server's
logs.

**No login for the two database tasks.** The database tasks run `mariadb` as
root on the node, and MariaDB accepts the node's root account as
`root@localhost` through its socket on every distribution below. For a
server on another machine, the tasks take `database_server_host`,
`login_user`, and `login_password`.

**An application on another machine.** Set `application_connects_from` in
the inventory to the pattern of the application servers' addresses, such as
`10.0.%`. On Debian and Ubuntu the server listens only on `127.0.0.1` as
installed: change `bind-address` in a file below
`/etc/mysql/mariadb.conf.d/` with a `file` task in a node task block whose
`systemd_service` task restarts `mariadb`.

## Distributions

The example works on Debian 12 and 13, Ubuntu 24.04 and 26.04, Rocky Linux 9,
and AlmaLinux 10. The MariaDB versions there are 10.5 to 11.8. Every one of
them names the server's package `mariadb-server`; the playbook installs it
with apt or dnf, by the node's package manager.

For every field, and for the same tasks with a MySQL server, see the
[`mysql_user`](../../docs/reference/modules/mysql_user.md) and
[`mysql_database`](../../docs/reference/modules/mysql_database.md) pages.
