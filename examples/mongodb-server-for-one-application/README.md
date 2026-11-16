# A MongoDB server for one application

A server that runs MongoDB from MongoDB's own package repository, with
access control on, an administrator, and one user for an application named
`shop`.

Set the server's address in `inventories/example.yml`, which also holds the
MongoDB series that the server runs. Choose two passwords and give them to
MMX in environment variables of the controller machine, the machine where
you run `mmx`:

```sh
export MONGODB_ADMIN_PASSWORD='choose-a-long-password'
export SHOP_MONGODB_PASSWORD='choose-another-long-password'
mmx dryrun configure
mmx run configure
mmx run configure
```

On the node, in order:

1. MMX adds MongoDB's package repository, `repo.mongodb.org`, with the key
   that signs its packages.
2. MMX installs the packages `mongodb-org-server`, the server, and
   `mongodb-mongosh`, MongoDB's shell.
3. MMX writes the server's settings to `/etc/mongod.conf` from
   `templates/mongod.conf`, with access control on, starts the server, and
   keeps it starting at boot.
4. MMX waits until the server's port 27017 answers.
5. MMX creates the administrator: the user `admin` in the database `admin`,
   with the role `root`.
6. MMX creates the application's user: the user `shop` in the database
   `shop`, with the role `readWrite` on that database and no other.

The second run changes nothing. The first dryrun, before MongoDB is
installed, reports the two user tasks `not_checked`: MMX cannot look into a
server that is not installed yet. A run after you changed
`SHOP_MONGODB_PASSWORD` changes only the password of the database user
`shop`.

The application then connects with
`mongodb://shop:<password>@127.0.0.1:27017/shop`. Write the application's
configuration file with a `file` task that reads the same secret,
`{{ .secrets.shop_mongodb_password }}`.

## Distributions

The example works on Debian 12 and 13, Ubuntu 24.04 and 26.04, Rocky Linux 9,
and AlmaLinux 10. MongoDB serves packages for each of them.

What the playbook does differently on each:

| Distribution | Repository | The server's data |
|---|---|---|
| Debian | `repo.mongodb.org/apt/debian`, component `main` | `/var/lib/mongodb` |
| Ubuntu | `repo.mongodb.org/apt/ubuntu`, component `multiverse` | `/var/lib/mongodb` |
| Red Hat family | `repo.mongodb.org/yum/redhat/<major version>` | `/var/lib/mongo` |

## The MongoDB series

MongoDB publishes each series, such as 8.0 or 9.0, in a repository of its
own, so a server stays in its series until you change it. The series belongs
to the server, so the inventory holds it: three variables of the group
`mongodb_servers` in `inventories/example.yml`. The three MongoDB variables
belong together, because each series has its own signing key:

| `mongodb_series` | `mongodb_signing_key_url` | `mongodb_signing_key_sha256` |
|---|---|---|
| `9.0` | `https://pgp.mongodb.com/server-9.asc` | `a8ec953b159e65ee2e327dde5506008d7693c3c6c3841beed421ec429bd10162` |
| `8.0` | `https://pgp.mongodb.com/server-8.0.asc` | `8c467ea138207ee8d0cbbce3e005c82c73c81b3263e7909bdc36b9a1feff14f6` |
| `7.0` | `https://pgp.mongodb.com/server-7.0.asc` | `b254e7858367d4c1762f931762c4db3ad7a6a0af11f3a965b709e9933c52852b` |

```yaml
# inventories/example.yml
groups:
  mongodb_servers:
    nodes: [database01]
    vars:
      mongodb_series: "8.0"
      mongodb_signing_key_url: https://pgp.mongodb.com/server-8.0.asc
      mongodb_signing_key_sha256: 8c467ea138207ee8d0cbbce3e005c82c73c81b3263e7909bdc36b9a1feff14f6
```

A server of the group that runs another series than the others gets the
three values in its own `vars` in the inventory, which replace the group's.

Not every distribution has every series: Debian 13 has only 9.0, and Ubuntu
24.04 and 26.04 have none before 8.0.

## Why it is written this way

**MongoDB's own repository.** No distribution packages MongoDB. MMX fetches
the signing key on the controller machine and checks it against the SHA-256
in the playbook, so a node never trusts a key that the project did not
name.

**MMX starts the server, not the package.** The packages start nothing. The
`systemd_service` task starts the server after `/etc/mongod.conf` is in
place, so the server never runs without access control, and restarts it
when the file changes.

**The administrator names itself as its login.** While a server with access
control has no user, MongoDB lets a connection from the server itself create
the first user without a login. So the first run of the administrator's task
needs no login, and every later run logs in as `admin` with the same secret.
To change the administrator's password, give the new password as `password`
and keep the old password as `login_password` for one run. Make the password
change a run of its own, with only the administrator's node task.
Afterwards, use the new password for `password` and `login_password`.
See the [administrator
password-change
instructions](../../official_extra_modules/mongodb_user/README.md#set-up).

**The passwords come from secrets.** `mmx_project_settings.yml` binds the
secrets `mongodb_admin_password` and `shop_mongodb_password` to the two
environment variables. To keep `mongodb_admin_password` and
`shop_mongodb_password` in the project's encrypted vault instead, remove
`secrets_from_env` from `mmx_project_settings.yml`. Write the secrets with
`mmx secrets edit`. The passwords reach MongoDB's shell
through its standard input, and are in no command line and in none of MMX's
records.

**An application on another machine.** The server listens only on
`127.0.0.1`. Add the server's address in the private network to `bindIp` in
`templates/mongod.conf`; the next run restarts the server.

For every input of `mongodb_user`, its errors, a replica set, and a server
before MongoDB 8.2, see
[the module's page](../../official_extra_modules/mongodb_user/README.md).
