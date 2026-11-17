# Manage a database

**Who acts:** you, or your coding agent, in your MMX project on your
controller machine: the computer where you run `mmx`. A coding agent cannot
type at a prompt, so the steps make up passwords with
`mmx secrets add-random` and read other passwords from a file.

**When to use this page:** your application needs a database and an
account to log in with, in PostgreSQL, MariaDB, or MySQL. The database
server runs on one of your servers, or on another machine that MMX does not
manage, such as a database service of your hosting provider. A managed
service may give no superuser, which limits `postgresql_role` with
`password`: see [A database server on another machine](#a-database-server-on-another-machine).

**What this page does:** you install PostgreSQL on the server `db01` and
create the role and the database for the shop application. The other
routes do the same with MariaDB, and create the database on a server on
another machine, from the application server `app01`.

**Terms:**

- A **node** is a server that MMX manages, listed in an **inventory** file
  in `inventories/`. A **playbook** is a file in `playbooks/` that lists
  **fleet stages**. Each fleet stage picks nodes, and each of those nodes
  runs the fleet stage's **node tasks**, in order. A node task names one
  **module**, such as `postgresql_role`.
- A **database server** is the program that stores databases, such as
  PostgreSQL or MariaDB. One database server holds several **databases**,
  such as `shop`.
- An **account** logs in to the database server. PostgreSQL calls an
  account a **role**. MariaDB and MySQL write an account as a user name
  and the machines the account may connect from, such as `shop@localhost`.
- A **secret** is a value that MMX keeps out of its output and out of the
  records MMX saves of each run, such as a database password. A node task
  reads a secret as `{{ .secrets.shop_database_password }}`.

## Choose your route

| You want to | Route |
| --- | --- |
| Run PostgreSQL on one of your servers, with a database for your application | [Set up PostgreSQL and a database](#set-up-postgresql-and-a-database) |
| Run MariaDB on one of your servers, with a database for your application | [Set up MariaDB and a database](#set-up-mariadb-and-a-database) |
| Create the database on a server that MMX does not manage, or log in with another account | [A database server on another machine](#a-database-server-on-another-machine) |
| Run MySQL instead of MariaDB | [The same tasks for a MySQL server](../reference/modules/mysql_database.md#the-same-tasks-for-a-mysql-server) lists MySQL's package and service names. The node tasks for the database and the account stay the same. |
| Give a role finer privileges, or install a PostgreSQL extension | [Privileges inside a database](../reference/modules/postgresql_database.md#privileges-inside-a-database) and [A database with an extension](../reference/modules/postgresql_database.md#a-database-with-an-extension) |
| Remove an account, or delete a database with its data | `state: absent` on [`postgresql_role`](../reference/modules/postgresql_role.md#remove-a-role) and [`mysql_user`](../reference/modules/mysql_user.md#remove-an-account). `state: deleted_with_all_its_data` on [`postgresql_database`](../reference/modules/postgresql_database.md#delete-a-database-with-all-its-data) and [`mysql_database`](../reference/modules/mysql_database.md). |

## Before you start

- You have a project where `mmx run` worked, as in
  [Get started](../get-started.md). Run `mmx` from a directory inside the
  project.
- The project has a vault for secrets, and `MMX_VAULT_PASSWORD_FILE`
  names the file with its passphrase, as
  [Keep secrets](keep-secrets.md) shows. `mmx secrets add-random` creates
  the vault when the project has none.
- The examples use the server `db01` at `203.0.113.21` with Debian 13, in
  the group `db_servers`. On Ubuntu the steps are the same. For Fedora,
  Rocky Linux, or AlmaLinux, [If something is different](#if-something-is-different)
  says what changes.

  ```yaml
  # inventories/example.yml
  nodes:
    db01:
      address: 203.0.113.21
      ssh_user: root

  groups:
    db_servers:
      nodes: [db01]
  ```

  **Check:** `mmx nodes` shows the group beside the server.

## Set up PostgreSQL and a database

1. **Make up the application's database password.**

   ```sh
   mmx secrets add-random shop_database_password
   ```

   ```text
   Added the key shop_database_password to the vault secrets_vault.age, with a random value of 32 letters and digits. MMX does not show the value.
   ```

   The password stays in the encrypted vault. MMX gives the password to the
   database server in step 4, and your application's configuration file
   reads the same secret.

   **Check:** `mmx secrets list-keys` prints `shop_database_password`.

2. **Write the playbook.** Create `playbooks/set_up_database.yml`:

   ```yaml
   # playbooks/set_up_database.yml
   fleet_stages:
     - id: set_up_postgresql
       nodes: db_servers
       node_tasks:
         - id: install_postgresql
           apt_package:
             packages:
               - {name: postgresql, version: newest_when_first_installed}

         - id: start_postgresql
           systemd_service:
             name: postgresql
             state: running
             start_at_boot: true

         - id: wait_for_postgresql
           wait_for:
             tcp_port_accepts_connections: 5432
             wait_at_most: 60s

         - id: create_shop_role
           postgresql_role:
             name: shop
             password: "{{ .secrets.shop_database_password }}"
             can_log_in: true

         - id: create_shop_database
           postgresql_database:
             name: shop
             owner: shop
             encoding: UTF8
   ```

   On the node, in order:

   1. `install_postgresql` installs the PostgreSQL server and its command
      line client `psql`. On Debian and Ubuntu, the package also creates
      the server's data directory.
   2. `start_postgresql` starts the server and starts the server at boot.
      MMX installs a package without starting its services, so
      `start_postgresql` does the first start.
   3. `wait_for_postgresql` waits until the server accepts connections on
      port 5432.
   4. `create_shop_role` creates the role `shop`, which may log in, with
      the password from the secret `shop_database_password`.
   5. `create_shop_database` creates the database `shop`, owned by the role
      `shop`.

   The two database tasks need no login of their own. MMX runs `psql` as
   the operating system account `postgres`, which the server on the same
   node accepts as its administrator through its local socket, without a
   password. MMX compares the password with the form that PostgreSQL
   stores, and sends the server only that form, so the password itself
   appears in no SQL statement and in no log of the server.

   **Check:** `mmx validate set_up_database` prints `Valid:` and the
   project's name.

3. **Preview.**

   ```sh
   mmx dryrun set_up_database
   echo "exit code $?"
   ```

   On a server without PostgreSQL, the output looks like this:

   ```text
   set_up_postgresql / db01: succeeded
     set_up_postgresql / db01 / install_postgresql: succeeded, would_change
       [{"name":"perl-modules-5.40","to":"5.40.1-6+deb13u1"},...,{"name":"postgresql-17","to":"17.12-0+deb13u1"},{"name":"postgresql","to":"17+278"},...]
     set_up_postgresql / db01 / start_postgresql: not_checked, unknown — unit postgresql.service is not installed yet; an earlier node task may install it
     set_up_postgresql / db01 / wait_for_postgresql: not_checked, unchanged — dial tcp 127.0.0.1:5432: connect: connection refused
     set_up_postgresql / db01 / create_shop_role: not_checked, unknown — postgresql_role: psql is not installed; install the package that provides it in an earlier node task: postgresql-client (apt), postgresql (dnf, zypper), postgresql-libs (pacman). It comes with PostgreSQL's server package; for a server on another machine, install the client alone. The earlier node task install_postgresql would change this node first and may provide what is missing; if the run then succeeds, change nothing.
     set_up_postgresql / db01 / create_shop_database: not_checked, unknown — postgresql_database: psql is not installed; ...
   Run cc8f6adb: succeeded
   ```

   **Check:** `install_postgresql` says `would_change`, and the exit code
   is 3. A dryrun installs nothing, so the dryrun cannot check the tasks
   that need the server. Exit code 3 means that some work could not be
   checked.
   [Preview and run changes](preview-and-run-changes.md#what-the-results-mean)
   explains each result.

4. **Run.**

   ```sh
   mmx run set_up_database
   ```

   ```text
   set_up_postgresql / db01: succeeded
     set_up_postgresql / db01 / install_postgresql: succeeded, changed
       [{"name":"perl-modules-5.40","to":"5.40.1-6+deb13u1"},...]
     set_up_postgresql / db01 / start_postgresql: succeeded, changed — service is not running
     set_up_postgresql / db01 / wait_for_postgresql: succeeded, unchanged — TCP port 127.0.0.1:5432 accepts connections
     set_up_postgresql / db01 / create_shop_role: succeeded, changed
       {"changed_attributes":[],"created":true}
     set_up_postgresql / db01 / create_shop_database: succeeded, changed
       {"changed_attributes":[],"created":true}
   Run 0df2c221: succeeded
   ```

   `"created":true` says that the task created the role or the database.
   On a later run, `changed_attributes` names what the task changed on an
   existing role or database, such as `["password"]`.

   **Check:** both database tasks say `created`, and the run ends with
   `Run ...: succeeded` and exit code 0.

5. **Check the database on the server.**

   ```sh
   mmx run-command --nodes db01 -- "runuser -u postgres -- psql --command 'SELECT datname, datdba::regrole AS owner FROM pg_database'"
   ```

   **Check:** the database `shop` has the owner `shop`:

   ```text
     run-command / db01 / command: succeeded, changed
         datname  |  owner
       -----------+----------
        postgres  | postgres
        shop      | shop
        template1 | postgres
        template0 | postgres
       (4 rows)
   ```

6. **Run it again.**

   ```sh
   mmx run set_up_database
   ```

   **Check:** every node task says `unchanged`, and the two database tasks
   say `"created":false`.

To check that the new account can log in, without printing the password,
run a `shell` node task that gives `psql` the password through `env`.
`output_holds_no_secret: true` lets the run show the output, because
`psql` prints no password, and `changed_when: false` marks the node task as
a read:

```yaml
# playbooks/check_shop_login.yml
fleet_stages:
  - id: check_shop_login
    nodes: db_servers
    node_tasks:
      - id: log_in_as_shop
        shell:
          script: psql --host 127.0.0.1 --username shop --dbname shop --no-password --command 'SELECT current_user'
          env:
            PGPASSWORD: "{{ .secrets.shop_database_password }}"
          output_holds_no_secret: true
          changed_when: false
```

**Check:** `mmx run check_shop_login` shows `shop` as `current_user`. A
wrong password fails the node task with `password authentication failed`
and no password in the output.

The application's own configuration file reads the same secret, such as a
`file` task on the application server whose `content` holds
`{{ .secrets.shop_database_password }}`.
[Create the vault and use a secret](keep-secrets.md#create-the-vault-and-use-a-secret)
shows such a task, and how MMX keeps the value out of the output.

## Set up MariaDB and a database

The node tasks for the database and the account work with MariaDB and with
MySQL. The steps below install MariaDB, which Debian and Ubuntu package.

1. **Make up the application's database password.**

   ```sh
   mmx secrets add-random shop_database_password
   ```

   **Check:** `mmx secrets list-keys` prints `shop_database_password`.

2. **Write the playbook.** Create `playbooks/set_up_database.yml`:

   ```yaml
   # playbooks/set_up_database.yml
   fleet_stages:
     - id: set_up_mariadb
       nodes: db_servers
       node_tasks:
         - id: install_mariadb
           apt_package:
             packages:
               - {name: mariadb-server, version: newest_when_first_installed}

         - id: start_mariadb
           systemd_service:
             name: mariadb
             state: running
             start_at_boot: true

         - id: create_shop_database
           mysql_database:
             name: shop
             character_set: utf8mb4

         - id: create_shop_account
           mysql_user:
             name: shop
             may_connect_from: localhost
             password: "{{ .secrets.shop_database_password }}"
             privileges:
               - grant_on: "shop.*"
                 grant: [ALL]
             privileges_not_listed: remove
   ```

   On the node, in order:

   1. `install_mariadb` installs MariaDB's server and its client program
      `mariadb`.
   2. `start_mariadb` starts the server and starts the server at boot.
   3. `create_shop_database` creates the database `shop` with the character
      set `utf8mb4`.
   4. `create_shop_account` creates the account `shop@localhost`, which may
      connect from the database server itself, with the password from the
      secret. The account gets every privilege on the database `shop`.
      `privileges_not_listed: remove` takes away any other privilege, so
      the account holds exactly the listed privileges.

   The two database tasks need no login of their own. MMX runs `mariadb` as
   root on the node, which the server accepts as `root@localhost` through
   its local socket. An application on another server needs an account
   such as `shop@203.0.113.%` instead, and a server that listens on the
   network: [`mysql_user`](../reference/modules/mysql_user.md) explains
   `may_connect_from`.

   **Check:** `mmx validate set_up_database` prints `Valid:`.

3. **Preview.**

   ```sh
   mmx dryrun set_up_database
   echo "exit code $?"
   ```

   **Check:** `install_mariadb` says `would_change`, the two database tasks
   say `not_checked` because the client program `mariadb` is not installed
   yet, and the exit code is 3:

   ```text
     set_up_mariadb / db01 / create_shop_database: not_checked, unknown — mysql_database: neither the program mariadb nor the program mysql is installed; each comes with its database server's package, and a server on another machine needs the client alone. ...
   ```

4. **Run.**

   ```sh
   mmx run set_up_database
   ```

   ```text
   set_up_mariadb / db01: succeeded
     set_up_mariadb / db01 / install_mariadb: succeeded, changed
       [{"name":"galera-4","to":"26.4.23-0+deb13u1"},...]
     set_up_mariadb / db01 / start_mariadb: succeeded, changed — service is not running
     set_up_mariadb / db01 / create_shop_database: succeeded, changed
       {"changed_attributes":[],"created":true}
     set_up_mariadb / db01 / create_shop_account: succeeded, changed — created the account shop@localhost; granted ALL PRIVILEGES on shop.*
       {"changed_attributes":[],"created":true}
   Run 33b0a24d: succeeded
   ```

   **Check:** both database tasks say `created`, and the run ends with
   `Run ...: succeeded` and exit code 0.

5. **Check the account on the server.**

   ```sh
   mmx run-command --nodes db01 -- "mariadb --execute 'SELECT User, Host, Db FROM mysql.db'"
   ```

   **Check:** the account `shop@localhost` has privileges on the database
   `shop`:

   ```text
     run-command / db01 / command: succeeded, changed
       User	Host	Db
       shop	localhost	shop
   ```

6. **Run it again.** `mmx run set_up_database` says `unchanged` for every
   node task. A run after you changed the secret's value changes only the
   account's password.

The application's configuration file reads the same secret, as the end of
the PostgreSQL route says.

## A database server on another machine

Use this route when the database server is not a node, such as a database
service of your hosting provider or a server that another team runs. A
`postgresql_role` task with `password` logs in as a superuser, because
only a superuser can read the stored form of a password. Some managed
database services give no superuser. There, create the role with the
service's own tools, or leave out `password`. A
node with the server's client program, here the application server
`app01`, then runs the database tasks over the network. The tasks take
connection fields: the server's host name, the account to log in as, its
password, and the certificate authority that signed the server's
certificate.

Before you start this route:

- The database server `postgresql.example.com` accepts connections from
  `app01` with TLS. The server's access rules (`pg_hba.conf` for
  PostgreSQL) let the login account log in from `app01` with a password.
- You have an administrator account on the server, here
  `deploy_administrator`, with its password in a file. The account must be
  allowed to do what the tasks do, as
  [What the login account must be allowed to do](#what-the-login-account-must-be-allowed-to-do)
  says.
- You have the certificate of the authority that signed the server's
  certificate, saved as `files/database-authority.crt` in your project.
- The inventory lists `app01` at `203.0.113.41` in the group
  `app_servers`.

1. **Store the administrator's password as a secret.**

   ```sh
   mmx secrets set database_administrator_password < ~/Downloads/database-administrator-password.txt
   rm ~/Downloads/database-administrator-password.txt
   ```

   [Add or change a secret without showing it](keep-secrets.md#add-or-change-a-secret-without-showing-it)
   explains the file, the prompt, and why the command prints nothing.

   **Check:** the command prints nothing and exits with code 0.

2. **Make up the application's database password.**

   ```sh
   mmx secrets add-random shop_database_password
   ```

   **Check:** `mmx secrets list-keys` prints both secret keys:

   ```text
   database_administrator_password
   shop_database_password
   ```

3. **Write the playbook.** Create `playbooks/create_shop_database.yml`:

   ```yaml
   # playbooks/create_shop_database.yml
   fleet_stages:
     - id: create_shop_database
       nodes: app01
       node_tasks:
         - id: install_postgresql_client
           apt_package:
             packages:
               - {name: postgresql-client, version: newest_when_first_installed}

         - id: write_database_authority
           file:
             path: /etc/ssl/database-authority.crt
             source: files/database-authority.crt
             permissions_mode: "0644"

         - id: create_shop_role
           postgresql_role:
             name: shop
             password: "{{ .secrets.shop_database_password }}"
             can_log_in: true
             database_server_host: postgresql.example.com
             login_role: deploy_administrator
             login_password: "{{ .secrets.database_administrator_password }}"
             database_server_certificate_authority_file: /etc/ssl/database-authority.crt

         - id: create_shop_database
           postgresql_database:
             name: shop
             owner: shop
             encoding: UTF8
             database_server_host: postgresql.example.com
             login_role: deploy_administrator
             login_password: "{{ .secrets.database_administrator_password }}"
             database_server_certificate_authority_file: /etc/ssl/database-authority.crt
   ```

   On `app01`, in order: `install_postgresql_client` installs `psql`
   without a server. `write_database_authority` copies the authority's
   certificate to the node. The two database tasks then run `psql` as root
   on `app01`, log in to `postgresql.example.com` as
   `deploy_administrator`, and accept the server only when the authority in
   `/etc/ssl/database-authority.crt` signed the server's certificate for
   that host name. [The connection fields](#the-connection-fields) lists
   each field.

   **Check:** `mmx validate create_shop_database` prints `Valid:`.

4. **Preview.**

   ```sh
   mmx dryrun create_shop_database
   echo "exit code $?"
   ```

   **Check:** on a node without `psql`, the two database tasks say
   `not_checked` with `psql is not installed`, and the exit code is 3. On a
   later dryrun, the database tasks log in and read the server, and a
   wrong login fails the dryrun at once.

5. **Run.**

   ```sh
   mmx run create_shop_database
   ```

   ```text
   create_shop_database / app01: succeeded
     create_shop_database / app01 / install_postgresql_client: succeeded, changed
       [{"name":"perl-modules-5.40","to":"5.40.1-6+deb13u1"},...,{"name":"postgresql-client","to":"17+278"}]
     create_shop_database / app01 / write_database_authority: succeeded, changed
     create_shop_database / app01 / write_database_authority: created "/etc/ssl/database-authority.crt"
       absent -> 1159 bytes, permissions_mode 0644, uid 0, gid 0
     create_shop_database / app01 / create_shop_role: succeeded, changed
       {"changed_attributes":[],"created":true}
     create_shop_database / app01 / create_shop_database: succeeded, changed
       {"changed_attributes":[],"created":true}
   Run afa0914c: succeeded
   ```

   **Check:** both database tasks say `created`, and the run ends with
   `Run ...: succeeded` and exit code 0. A second run, and a dryrun after
   the second run, say `unchanged` for every node task, with exit code 0.

**The same route with MariaDB or MySQL.** The tasks take `login_user`
instead of `login_role`, and the node needs the client package
`mariadb-client` instead of `postgresql-client`:

```yaml
# playbooks/create_shop_database_on_mariadb.yml
fleet_stages:
  - id: create_shop_database
    nodes: app01
    node_tasks:
      - id: install_mariadb_client
        apt_package:
          packages:
            - {name: mariadb-client, version: newest_when_first_installed}

      - id: write_database_authority
        file:
          path: /etc/ssl/database-authority.crt
          source: files/database-authority.crt
          permissions_mode: "0644"

      - id: create_shop_database
        mysql_database:
          name: shop
          character_set: utf8mb4
          database_server_host: mariadb.example.com
          login_user: administrator
          login_password: "{{ .secrets.database_administrator_password }}"
          database_server_certificate_authority_file: /etc/ssl/database-authority.crt

      - id: create_shop_account
        mysql_user:
          name: shop
          may_connect_from: "203.0.113.%"
          password: "{{ .secrets.shop_database_password }}"
          privileges:
            - grant_on: "shop.*"
              grant: [ALL]
          privileges_not_listed: remove
          database_server_host: mariadb.example.com
          login_user: administrator
          login_password: "{{ .secrets.database_administrator_password }}"
          database_server_certificate_authority_file: /etc/ssl/database-authority.crt
```

`may_connect_from: "203.0.113.%"` lets the account `shop` connect from
every address that starts with `203.0.113.`, the application servers'
network. `mmx run create_shop_database_on_mariadb` then says:

```text
  create_shop_database / app01 / create_shop_database: succeeded, changed
    {"changed_attributes":[],"created":true}
  create_shop_database / app01 / create_shop_account: succeeded, changed — created the account shop@203.0.113.%; granted ALL PRIVILEGES on shop.*
    {"changed_attributes":[],"created":true}
```

On the Red Hat family, the client package is `mariadb`.

Module pages that rely on this section: [`mysql_database`](../reference/modules/mysql_database.md), [`mysql_user`](../reference/modules/mysql_user.md), [`postgresql_database`](../reference/modules/postgresql_database.md), [`postgresql_role`](../reference/modules/postgresql_role.md).

## The connection fields

Without connection fields, a database task uses the server on the same
node through the server's local socket. PostgreSQL tasks then run `psql` as
the operating system account `postgres`. MariaDB and MySQL tasks run the
client as root, which the server accepts as `root@localhost`. A database
server that is itself a node needs no connection fields: run its tasks on
that node, as the first two routes do.

The four modules `postgresql_role`, `postgresql_database`, `mysql_user`,
and `mysql_database` take the same connection fields, except for the name
of the login field:

| Field | Meaning |
| --- | --- |
| `database_server_host` | The server's host name or address. Without `database_server_host`, the task uses the local socket. |
| `database_server_port` | The server's port, when the port is not 5432 for PostgreSQL or 3306 for MariaDB and MySQL. For MariaDB and MySQL, only with `database_server_host`. For PostgreSQL with the local socket, the port selects among several PostgreSQL servers on one node. |
| `login_role` (PostgreSQL), `login_user` (MariaDB and MySQL) | The account that MMX logs in as. Default: `postgres` or `root`. |
| `login_password` | That account's password, normally a secret. Required with `database_server_host`. Without `database_server_host`, the task logs in with the password through the local socket. MariaDB and MySQL tasks refuse an empty value, so a secret that is not set never becomes a login without a password. |
| `database_server_certificate_authority_file` | Only with `database_server_host`: the absolute path, on the node, of the certificate of the authority that signed the server's certificate. |
| `dangerously_skip_server_certificate_check` | Only with `database_server_host`: `true` turns off the check of the server's identity. A task that gives this field together with `database_server_certificate_authority_file` is refused. |

**The server's certificate is checked by default.** With
`database_server_host`, the client checks that a trusted authority signed
the server's certificate, and that the certificate names the host name in
`database_server_host`. Without `database_server_certificate_authority_file`,
`psql` needs trusted authorities in `/root/.postgresql/root.crt`. The
certificate sections of
[`postgresql_database`](../reference/modules/postgresql_database.md#server-certificate-verification)
and [`mysql_database`](../reference/modules/mysql_database.md#server-certificate-verification)
say what each client needs.

**Two needs of PostgreSQL tasks.** The database `postgres` must exist,
because MMX connects to the database `postgres` to read and change roles
and the list of databases, as PostgreSQL's own tools do. And a server
whose socket is in another directory than `/var/run/postgresql`, the
directory that `psql` uses, is reached only over the network: with
`database_server_host` and a password login. No supported distribution
sets up a server that way.

**MySQL over a connection without TLS.** An account with MySQL's method
`caching_sha2_password` cannot log in for the first time over a connection
without TLS. MySQL's client then fails with "Authentication requires
secure connection". The limit applies only while the server's TLS is
switched off.

### What the login account must be allowed to do

- **PostgreSQL:** the login role must be allowed to create roles and
  databases. A `postgresql_role` task with `password` needs a login role
  that is a superuser, because only a superuser can read the stored form
  of a password, which MMX compares. From PostgreSQL 16, a login role that
  is not a superuser also needs the `ADMIN` option on each role in
  `member_of_roles`, and must be a member of the role that `owner` names.
  Without these permissions, the task fails with the server's message.
- **MariaDB and MySQL:** the login account must be allowed to create
  accounts and databases, and to grant what the task grants, such as an
  account with `ALL` and `GRANT OPTION` on `*.*`.

### How the login reaches the client

- **The login password goes through a file** in the node task's private
  directory, so the password appears on no command line. A dryrun logs in
  too, so a dryrun also writes the file.
  [A node task's private directory](../reference/secrets.md#a-node-tasks-private-directory)
  has the details.
- **`psql` gets the login role in its environment,** which only the
  account that runs `psql` and root can read. A MariaDB or MySQL task with
  connection fields gives the client the login account and the password in
  the file only.
- **The client follows only the task's fields.** No `PGHOST`, `PGUSER`,
  `PGPASSWORD`, `PGSERVICE`, or other `PG` variable of the node reaches
  `psql`, and a task without `login_password` reads no `.pgpass` file. The
  MariaDB or MySQL client reads no option file of the node, such as
  `/root/.my.cnf` or `/etc/my.cnf.d`, and no variable such as
  `MYSQL_HOST`. So a task reaches the server that the task names, as the
  account that the task names.
- **A secret in a login does not hide the task's result.**
  [A login does not hide a node task's result](../reference/secrets.md#a-login-does-not-hide-a-node-tasks-result)
  has the rule.

### When a task cannot reach the server

| What is wrong | In a run | In a dryrun |
| --- | --- | --- |
| The client program is not on the node, or, for a PostgreSQL task without `database_server_host`, the account `postgres` | Fails as a missing prerequisite, and is not tried again. The message names the package to install. | `not_checked` |
| No server answers, or the server starts up or shuts down | Fails. MMX tries the task again, as its `retry` says, because a starting server answers a moment later. | `not_checked` |
| The file of `database_server_certificate_authority_file` is not on the node | Fails, and is not tried again | `not_checked` |
| The server refuses the login, or the client does not accept the server's certificate | Fails with the message of the server or the client, and is not tried again | Fails in the same way |

An earlier node task may still install the client, start the server, or
write the certificate file, so a dryrun does not judge a missing client,
server, or certificate file. A
refused login is wrong now, so a dryrun fails on a refused login. MMX tries
a refused login only once, whatever the task's `retry` says: another try
gets the same answer, and a MySQL server can lock an account after a few
wrong logins (`FAILED_LOGIN_ATTEMPTS`). The result's `failure_kind` is then
`config`.

**Time limits.** A connection that is not made within 10 seconds fails. A
PostgreSQL statement that changes the server waits at most 10 seconds for a
lock that another session holds. After 10 seconds the server refuses the
statement with the code `55P03`, nothing of that statement or its
transaction stays, and MMX tries the task again as its `retry` says.

**What a failed PostgreSQL task changed.** A failed `postgresql_role` or
`postgresql_database` task says in `change` what MMX knows:

| `change` | When |
| --- | --- |
| `unchanged` | The task failed before its first changing statement, such as a refused login. Or the server refused a statement, so nothing of that statement or its transaction stays. |
| `changed` | The server did every statement and only MMX's read after them failed, or a database task changed the server in an earlier step before a later step failed. `value` says what changed. |
| `unknown` | The session ended in the middle of the changing statements, or someone stopped the task during those statements. |

### Why the modules work this way

- **MMX runs the server's own client and links no database driver.** The
  node runner executable stays small, and a database server already has
  its client.
- **A PostgreSQL task asks whether a server answers before it logs in.**
  `psql` fails in the same way for a server that does not run, a refused
  login, and a missing database. `pg_isready`, which comes with `psql`,
  answers the first question alone.
- **A PostgreSQL task decides from the server's catalog and error codes,
  never from the words of a message.** A server's messages can be in another language.
  Each message reaches you as the server wrote the message, with its
  five-character code, such as `2BP01` for "other objects depend on it".
- **The MariaDB or MySQL client takes its input as SQL only.** Otherwise a
  line that starts with `\!` would run a command on the node. The one
  value that MMX reads from the server's configuration is the path of the
  server's socket, which MMX asks `my_print_defaults` for, so a server
  whose configuration moves the socket is still found.

## If something is different

| What you see | What it means and what to do |
| --- | --- |
| `psql got no session with the PostgreSQL server at postgresql.example.com as the role deploy_administrator: psql: error: connection to server at "postgresql.example.com" (203.0.113.51), port 5432 failed: FATAL:  password authentication failed for user "deploy_administrator"` | The server refused the login. The message goes on with what to check: `login_role`, `login_password`, and a line in the server's `pg_hba.conf` that lets the role log in from this node with a password. Store the right password with `mmx secrets set database_administrator_password --replace < ~/Downloads/database-administrator-password.txt`. |
| `the database server at mariadb.example.com, port 3306 refused the login: ERROR 1045 (28000): Access denied for user 'administrator'@'app01.example.com' (using password: YES); check login_user and login_password` | The MariaDB or MySQL server refused the login. Check the password and `login_user`, and that the account may connect from the node's address. |
| `root certificate file "/root/.postgresql/root.crt" does not exist` | The task has `database_server_host` without `database_server_certificate_authority_file`. Give the authority's certificate as in step 3 of [A database server on another machine](#a-database-server-on-another-machine). |
| `server certificate for "postgresql.example.com" (and 1 other name) does not match host name "203.0.113.51"` | `database_server_host` gives an address or a name that the server's certificate does not name. Use the host name in the certificate. |
| `mmx: vault passphrase required; use --vault-password-file, MMX_VAULT_PASSWORD_FILE, or MMX_VAULT_PASSWORD (prompts are disabled)`, exit code 4 | MMX needs the passphrase and may not ask for it: no terminal, `--non-interactive`, or `--json`. Set `MMX_VAULT_PASSWORD_FILE`, as [Keep secrets](keep-secrets.md#if-something-is-different) says. |
| The server runs Fedora, Rocky Linux, or AlmaLinux | Use `dnf_package`. PostgreSQL there needs the packages `postgresql-server` and `postgresql-contrib`, and `postgresql-setup --initdb` before the first start: [A PostgreSQL server for one application](../reference/modules/postgresql_database.md#a-postgresql-server-for-one-application) shows the tasks. MariaDB's package is `mariadb-server` there too. |
| On Debian or Ubuntu, `systemctl status postgresql` says `active` while the server is down | `postgresql.service` only groups the servers. Each server has its own unit, such as `postgresql@17-main.service`. The `wait_for` task on port 5432 is what proves that the server answers. |
| Another message from a database module | Each module page explains its inputs and messages: [`postgresql_role`](../reference/modules/postgresql_role.md), [`postgresql_database`](../reference/modules/postgresql_database.md), [`mysql_user`](../reference/modules/mysql_user.md), [`mysql_database`](../reference/modules/mysql_database.md). |

## Where to go next

- Every input of the database modules, privileges, extensions, and
  memberships: [`postgresql_role`](../reference/modules/postgresql_role.md),
  [`postgresql_database`](../reference/modules/postgresql_database.md),
  [`mysql_user`](../reference/modules/mysql_user.md),
  [`mysql_database`](../reference/modules/mysql_database.md).
- Whole example projects:
  [postgresql-server-for-one-application](../../examples/postgresql-server-for-one-application/README.md)
  and
  [mariadb-server-for-one-application](../../examples/mariadb-server-for-one-application/README.md).
- Put the database server's address into the application servers'
  configuration:
  [Use a result from one server on another](use-a-result-from-one-server-on-another.md).
- Secrets, the vault, and running without a terminal:
  [Keep secrets](keep-secrets.md).
- Run the application itself in containers: [Run containers](run-containers.md).
