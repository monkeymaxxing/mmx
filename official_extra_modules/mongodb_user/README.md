# `mongodb_user`

`mongodb_user` manages one user of a MongoDB server: whether the user
exists, its password, and its roles. `mongodb_user` runs MongoDB's own
shell, the program `mongosh`, as root on the node of the MongoDB server.
A **node** is a server that MMX manages.

A MongoDB **user** is a login inside the database server, not an account of
the operating system. Each user lives in one database and logs in to that
database. A user gets its rights from **roles**: the role `readWrite` on the
database `shop` lets it read and write that database and nothing else.

## Set up

No distribution packages MongoDB. The server and `mongosh` come from
MongoDB's own package repository, `repo.mongodb.org`, which serves Debian,
Ubuntu, and the Red Hat family. Its Debian packages are built only for
AMD64, so a Debian server on ARM64 cannot install MongoDB from it; Ubuntu
and the Red Hat family have ARM64 packages too. The playbook
`playbooks/configure.yml` sets up a server for one application, from the
repository to the application's user:

```yaml
# mmx run configure
fleet_stages:
  - id: configure
    nodes: mongodb_servers

    node_tasks:
      # MongoDB's own package repository. No distribution packages MongoDB,
      # so the packages come from repo.mongodb.org. The inventory gives the
      # MongoDB series and the key that signs its packages. The repository
      # of Debian has the component main, the one of Ubuntu multiverse.
      - when: {equals: ["{{ .node.distribution.id }}", debian]}
        apt_repository:
          name: mongodb-org
          uris: [https://repo.mongodb.org/apt/debian]
          suites: ["{{ .node.distribution.codename }}/mongodb-org/{{ .vars.mongodb_series }}"]
          components: [main]
          signing_key_url: "{{ .vars.mongodb_signing_key_url }}"
          signing_key_sha256: "{{ .vars.mongodb_signing_key_sha256 }}"
      - when: {equals: ["{{ .node.distribution.id }}", ubuntu]}
        apt_repository:
          name: mongodb-org
          uris: [https://repo.mongodb.org/apt/ubuntu]
          suites: ["{{ .node.distribution.codename }}/mongodb-org/{{ .vars.mongodb_series }}"]
          components: [multiverse]
          signing_key_url: "{{ .vars.mongodb_signing_key_url }}"
          signing_key_sha256: "{{ .vars.mongodb_signing_key_sha256 }}"

      # The Red Hat family: Red Hat Enterprise Linux, Rocky Linux, and
      # AlmaLinux share one repository for each major version.
      - when: {equals: ["{{ .node.distribution.family }}", redhat]}
        dnf_repository:
          name: mongodb-org
          description: MongoDB
          baseurl: ["https://repo.mongodb.org/yum/redhat/{{ .node.distribution.major_version }}/mongodb-org/{{ .vars.mongodb_series }}/$basearch/"]
          signing_key_url: "{{ .vars.mongodb_signing_key_url }}"
          signing_key_sha256: "{{ .vars.mongodb_signing_key_sha256 }}"

      # The server, and MongoDB's shell mongosh, which the mongodb_user
      # tasks below run.
      - when: {equals: ["{{ .node.package_manager }}", apt]}
        apt_package: {packages: [{name: mongodb-org-server, version: newest_when_first_installed}, {name: mongodb-mongosh, version: newest_when_first_installed}]}
      - when: {equals: ["{{ .node.package_manager }}", dnf]}
        dnf_package: {packages: [{name: mongodb-org-server, version: newest_when_first_installed}, {name: mongodb-mongosh, version: newest_when_first_installed}]}

      # The server's settings, with access control on. The packages start
      # nothing: the systemd_service task starts the server with this file,
      # keeps it starting at boot, and restarts it when the file changes.
      - task_block:
          id: mongod
          changes:
            - file:
                path: /etc/mongod.conf
                template: templates/mongod.conf
                permissions_mode: "0644"
          activate:
            - systemd_service:
                name: mongod
                state: running
                start_at_boot: true
                refresh: restart

      # The server answers on its port a moment after its start.
      - wait_for:
          tcp_port_accepts_connections: 27017
          wait_at_most: 60s

      # The first user of the server: its administrator. While the server
      # has no user, MongoDB lets a connection from the server itself create
      # this one user without a login. The task names itself as its login,
      # so that its later runs can log in.
      - mongodb_user:
          name: admin
          database: admin
          password: "{{ .secrets.mongodb_admin_password }}"
          roles:
            - {role: root, database: admin}
          roles_not_listed: remove
          login_user: admin
          login_password: "{{ .secrets.mongodb_admin_password }}"

      # The application's user. It reads and writes the database shop and
      # nothing else. MongoDB makes the database when the application
      # writes its first document.
      - mongodb_user:
          name: shop
          database: shop
          password: "{{ .secrets.shop_mongodb_password }}"
          roles:
            - {role: readWrite, database: shop}
          roles_not_listed: remove
          login_user: admin
          login_password: "{{ .secrets.mongodb_admin_password }}"
```

The inventory, `inventories/example.yml`, names the servers and the MongoDB
series that they run, with the key that signs the packages of that series:

```yaml
nodes:
  database01:
    address: database01.example.com
    ssh_user: admin

groups:
  mongodb_servers:
    nodes: [database01]
    vars:
      # The MongoDB series that the servers of this group run, and the key
      # that signs the packages of that series. The three values belong
      # together: README.md lists them for each series.
      mongodb_series: "9.0"
      mongodb_signing_key_url: https://pgp.mongodb.com/server-9.asc
      mongodb_signing_key_sha256: a8ec953b159e65ee2e327dde5506008d7693c3c6c3841beed421ec429bd10162
```

The file task reads the server's settings from `templates/mongod.conf` in
the project:

```yaml
# The settings of the MongoDB server. MMX writes this file; change it in
# the project. MongoDB's reference for every setting:
# https://www.mongodb.com/docs/manual/reference/configuration-options/

storage:
  # The packages for Debian and Ubuntu keep the data in /var/lib/mongodb,
  # and the packages for the Red Hat family in /var/lib/mongo.
  dbPath: {{ if eq .node.package_manager "apt" }}/var/lib/mongodb{{ else }}/var/lib/mongo{{ end }}

systemLog:
  destination: file
  logAppend: true
  path: /var/log/mongodb/mongod.log

net:
  port: 27017
  # Only programs on the server itself reach MongoDB. For an application on
  # another machine, add the server's address in the private network,
  # such as 127.0.0.1,10.0.0.5.
  bindIp: 127.0.0.1

processManagement:
  timeZoneInfo: /usr/share/zoneinfo

# Access control: every connection needs a login. The one exception is a
# connection from the server itself while MongoDB has no user: it may create
# the first user.
security:
  authorization: enabled
```

The [example project](../../examples/mongodb-server-for-one-application/README.md)
holds the three files, with the two secrets. On the node, in order:

1. MMX adds MongoDB's repository. The controller machine, where you run
   `mmx`, fetches the signing key and checks it against the SHA-256 in the
   inventory before the node gets it.
2. MMX installs the packages `mongodb-org-server` and `mongodb-mongosh`.
   The packages start nothing.
3. MMX writes `/etc/mongod.conf` and starts the unit `mongod`. A later run
   that changes the file restarts the server.
4. The first `mongodb_user` task creates the administrator `admin`, and the
   second one the user `shop`.

**Access control** is the setting `security.authorization: enabled`. With
it, every connection must log in. Without it, MongoDB lets everybody who
reaches its port read and change everything, and no task needs a login.

**The first user of a server** is its administrator, and it has rules of
its own:

- While a server with access control has no user, MongoDB lets one
  connection in without a login, to create the first user. It allows this
  only for a connection from the server's own machine, so
  `mongodb_connection_string` must name `127.0.0.1` or `localhost` for this
  task, as the default does.
- The first user must be in the database `admin`, with roles on `admin`
  only. One of the roles must be `root`, `userAdminAnyDatabase`, or
  `userAdmin`, so that the user can create the next one.
- The administrator-creation task sets `login_user` to `admin`, the MongoDB
  user the task creates. The administrator-creation task's first run needs
  no login. Every later run logs in as `admin` to compare the task with the
  server.
- To change the administrator's password, the task needs both passwords for
  one run: the old one as `login_password`, with which it logs in, and the
  new one as `password`, which it sets. The playbook above reads both from
  one secret. For that one run, let `password` read a second secret with
  the new password; afterwards both read the new one. Make the change a run
  of its own, with only the administrator's node task. A later
  node task in the password-change run that logs in with the old secret is
  refused, because the administrator's password is the new one by then.

**A server of a MongoDB series other than 8.2:** turn access control on before the server
has any user, as the playbook above does, or after the administrator exists
in `admin`. Such a server asks for a login as soon as any user exists. When
its only users are in other databases, nobody can log in as an
administrator, and the administrator's task fails with a refused login.
To repair the refused administrator login, remove `security.authorization`
from `templates/mongod.conf`. Run the playbook once, then put
`security.authorization` back in `templates/mongod.conf`. MongoDB 8.2 still
lets the first administrator in; MongoDB 9.0 asks for a login again.

**Another series.** `mongodb_series` in the inventory chooses the MongoDB
series, such as `8.0`. The series belongs to the server, so the inventory
holds it and every run uses it. Each series has its own signing key, so the
three variables change together; the example project's README lists the
values.

## Inputs

| Input | Meaning | When omitted |
|---|---|---|
| `name` | The user's name, such as `shop`. | Required. |
| `database` | The database that holds the user, which is the database the user logs in to. | Required. |
| `state` | `present` creates the user or changes it. `absent` removes the user; every database and its data stay. | `present`. |
| `password` | The user's password. It is a private value: a secret in it does not hide the task's result. | The password of an existing user is not touched. The task creates no user: for a user that does not exist, a run fails, and a dryrun reports `not_checked`. |
| `roles` | The user's roles, as a list. Each entry names a `role` and the `database` it applies to, such as `{role: readWrite, database: shop}`. Needs `roles_not_listed`. | The roles of an existing user are not touched, and a new user gets no role. |
| `roles_not_listed` | What happens to a role that the user has and `roles` does not list. `remove` takes it away from the user. `keep` leaves it. | Required with `roles`, and not allowed without it. |
| `login_user` | The user that the module logs in as. It must be allowed to manage users, as a user with the role `root` is. It is a private value. | The module logs in as nobody. That works on a server without access control, and for the first user of a server. |
| `login_password` | The password of `login_user`. It is a private value. | Required with `login_user`. |
| `login_database` | The database that holds `login_user`. | `admin`. |
| `mongodb_connection_string` | How `mongosh` reaches the server: MongoDB's own text for the address, the port, TLS, and a replica set. It must hold no user name and no password. | `mongodb://127.0.0.1:27017/?directConnection=true`: the server on the node itself, on MongoDB's usual port. |

- **`state: absent` accepts only `name`, `database`, the three login
  inputs, and `mongodb_connection_string`.** A task with `password`,
  `roles`, or `roles_not_listed` beside it is refused.
- **A private value** takes a secret without making the task's result
  private. MMX shows `[redacted]` wherever a text of the result holds the
  value. When `login_user` is a secret, that also replaces the same name
  where the reason means another thing, such as the user `admin` and the
  database `admin` behind a secret login user `admin`.
- **The name of a database** in `database`, `login_database`, or the
  `database` of each role must be one that MongoDB takes: not empty, at
  most 63 bytes, and without a dot, a space, a slash, a backslash, or a
  double quote.
- **A connection string for another machine** should end the wait for a
  server that does not answer: add `serverSelectionTimeoutMS=5000`.
  Without `serverSelectionTimeoutMS`, `mongosh` waits 30 seconds. For an
  address on the node itself, `mongosh` waits 2 seconds.
- **A replica set** is a group of MongoDB servers that hold the same data.
  One of them, the **primary**, takes every change. Run the task on the
  primary's node, or name the replica set in the connection string and leave
  `directConnection=true` out, such as
  `mongodb://127.0.0.1:27017/?replicaSet=rs0`. `mongosh` then finds the
  primary itself.

## Outputs

| Output | Meaning |
|---|---|
| `created` | `true` when the task created the user. In a dryrun: `true` when a run would create it. |

A later node task reads the `created` output as `.node_tasks.<id>.created`.

## What a dryrun reports

A dryrun reads the server as a run does and changes nothing. It logs in as
`login_user`, reads the user and the roles that the task lists, and logs in
once as the user to compare the password.

- **`unchanged`** when the server has the user as the task wants it.
- **`would_change`** with a reason that names each change that a run would
  make:
  - `would create the user shop in the database shop with the role
    readWrite on the database shop`;
  - `would set the password of the user shop in the database shop`;
  - `would grant the role read on the database reports to the user shop in
    the database shop`, and `would revoke ... from ...`;
  - `would remove the user shop in the database shop`.
- **`not_checked`** when an earlier node task of the same playbook can
  still provide what is missing:
  - `mongosh` is not installed, or is older than 2.2.0;
  - the server does not answer;
  - the server is a member of a replica set that has no primary now, such
    as before a `shell` task has initiated the replica set;
  - the server has access control on and no administrator yet, and the task
    is not the administrator's;
  - the user does not exist and the task has no `password`: an earlier
    `mongodb_user` task of the playbook may be the one that creates it;
  - a role that the task lists does not exist.

  So the playbook under "Set up" passes its first dryrun on a new node.
- **A failure, as in a run, after one try,** for inputs that break a rule,
  for a login that the server refuses, for a command that the server
  refuses to the login user, and for a replica set whose primary is another
  server.
- **A failure for a login user that an earlier node task of the same
  playbook creates.** The later task logs in as a user that does not exist
  yet during the dryrun, so the server refuses the login. A run of the same
  playbook works. The server's first administrator is different: while the
  server has no user, the tasks behind the administrator's are not checked.
- **What the reads leave on the node:** no file. `mongosh` writes none, in
  a dryrun as in a run. The server writes each login into its own log, as
  it does for every login, without the password.

## Examples

The user of an application, which reads and writes one database:

```yaml
- mongodb_user:
    name: shop
    database: shop
    password: "{{ .secrets.shop_mongodb_password }}"
    roles:
      - {role: readWrite, database: shop}
    roles_not_listed: remove
    login_user: admin
    login_password: "{{ .secrets.mongodb_admin_password }}"
```

One more role for a user that exists, on another database. `keep` leaves the
user's other roles alone, and the task without `password` leaves its
password alone:

```yaml
- mongodb_user:
    name: shop
    database: shop
    roles:
      - {role: read, database: reports}
    roles_not_listed: keep
    login_user: admin
    login_password: "{{ .secrets.mongodb_admin_password }}"
```

A user that is removed, on a replica set whose primary `mongosh` finds
itself:

```yaml
- mongodb_user:
    name: reports
    database: shop
    state: absent
    login_user: admin
    login_password: "{{ .secrets.mongodb_admin_password }}"
    mongodb_connection_string: mongodb://127.0.0.1:27017/?replicaSet=rs0
```

## Errors and what they mean

Each reason starts with `mongodb_user`. The table gives the fixed part of
each one.

MMX tries a failed task again only where another try can help: when the
server does not answer, when the replica set has no primary now, and when a
command failed for a reason that the module does not know. A task has one
try, in a dryrun as in a run, when its inputs break a rule, when the server
refuses the login or refuses a command to the login user, and when the
replica set's primary is another server.

| The reason holds | What it means, and what to do |
|---|---|
| `the program mongosh is not installed` | The node has no `mongosh`. Install the package `mongodb-mongosh` from MongoDB's repository in an earlier node task, as under "Set up". |
| `the program env is not installed` | The node has no `env`, which every distribution installs with `coreutils`. Install the package `coreutils`. |
| `mongosh ... is installed, and the module needs mongosh 2.2.0 or newer` | The node's `mongosh` is too old. Install a newer package `mongodb-mongosh`. |
| `the MongoDB server does not answer` | Nothing accepts a connection at the address of `mongodb_connection_string`. The words behind it are `mongosh`'s. Start the server in an earlier node task, or correct the address and the port. A run tries again by itself. |
| `mongosh cannot read this connection string` | `mongodb_connection_string` is no MongoDB connection string, or names an option that `mongosh` does not know. Correct the input. An `@` anywhere in it is read as the end of a login. |
| `must hold no user name and no password` | `mongodb_connection_string` holds a login, such as `mongodb://admin:...@host`. Give the login as `login_user` and `login_password`. |
| `MongoDB cannot take this as the name of a database`, `MongoDB cannot take the database of the role ... as the name of a database` | The input that the reason starts with holds a name that MongoDB refuses for a database: `database`, `login_database`, or the `database` of a role. The words behind it are `mongosh`'s or the server's. Correct the name. |
| `this MongoDB server needs a login` | The server has access control on, and the task has no `login_user`. Add `login_user` and `login_password`. |
| `the MongoDB server refused the login of the user` | `login_user`, `login_database`, or `login_password` is wrong. The task is not tried again. Correct them. On a server that has no user yet, the first user's task works only through `127.0.0.1` or `localhost`. On a server of a MongoDB series other than 8.2, see "Set up". |
| `has access control on and no administrator yet` | The server has no user, so only its first administrator can be created. Create the first user in the database `admin`, with the role `root`, in an earlier `mongodb_user` task. |
| `The first user must be allowed to manage users` | The task would create the server's first user without a role that lets it create the next user. Give it `root`, `userAdminAnyDatabase`, or `userAdmin` on the database `admin`. |
| `MongoDB lets the first user hold only roles on the database admin` | The task would create the server's first user with a role on another database, which MongoDB refuses. List only roles on `admin` for the first user. |
| `does not exist, and the task has no password to create it` | The server has no such user, and the task has no `password`. A run fails once; a dryrun reports `not_checked`, because an earlier node task may create the user. Add `password`, or create the user in an earlier `mongodb_user` task. |
| `state: absent accepts only ...; remove ...` | A task that removes the user also gives a password or roles. Remove the input that the reason names. |
| `the role ... on the database ... does not exist on this MongoDB server` | A role that the task lists is no role of that database. Correct the role's name or its database, or create a role of your own in an earlier node task. MongoDB's own roles, such as `readWrite`, exist on every database. |
| `and not its primary, and only the primary changes users. The primary is ...` | The server that `mongosh` reached is a member of a replica set, and another server is the primary. The task is not tried again. Run the task on the primary's node, or put `replicaSet=<name>` into `mongodb_connection_string` and leave `directConnection=true` out. |
| `The replica set has no primary now` | The replica set has not chosen a primary yet, as in the first seconds after a `shell` task initiated it or its members started. A run tries again by itself. A replica set that was never initiated needs that `shell` task first. |
| `not authorized on ... (MongoDB's error 13, Unauthorized); login_user must be a user that is allowed to manage users` | The server refuses a command to `login_user`. The task is not tried again. Log in as a user that may manage users, such as one with the role `root` or `userAdminAnyDatabase` on `admin`. |
| `The server has the user ... now: it created the user before this failure` | `mongosh` failed after the server created the user, or the answer did not arrive. The task is not tried again, so that the failure stays visible; its output `created` is `true`. Run the playbook again: the next run compares the user with the task. |
| `The module could not find out whether the server created the user` | The same failure, and the server did not answer the module's question afterwards. The task is not tried again. Run the playbook again. |
| `mongosh ended with exit code ... and without a result that the module can read` | `mongosh` itself failed. The words behind it are the first line that `mongosh` printed. Run `mongosh --version` on the node; reinstall the package `mongodb-mongosh` when that fails too. |
| `roles_not_listed: is required with roles`, `login_password: is required with login_user`, `must not be empty`, and others that start with `mongodb_user.<input>:` | The inputs break a rule of the table under "Inputs". Correct the input that the reason names. |

A failure of a change gives MongoDB's own message behind what the module
tried, such as `creating the user shop in the database shop: ...`. When a
task made one change before another one failed, the reason ends with what
the task did before the failure.

**A failed change reports `change: changed`,** also when MongoDB refused the
task's first change and nothing changed on the server. MMX takes a task's
change from the commands that ran, and the module's one changing command,
`mongosh`, did run. The reason says what failed.

## Tested on

**Distributions:** Debian 12 and 13, Ubuntu 24.04 and 26.04, Rocky Linux 9, AlmaLinux 10.

The integration suite `tests/integration/mongodb_users.py` runs the playbook
under "Set up" and this module on these distributions, in system containers
on the Debian family and in virtual machines on the Red Hat family, on x86_64:

| Distribution | MongoDB server | `mongosh` |
|---|---|---|
| Debian 12 and 13 | 9.0 | 2.13 |
| Ubuntu 24.04 and 26.04 | 9.0 | 2.13 |
| Rocky Linux 9 | 8.0 | 2.14 |
| AlmaLinux 10 | 9.0 | 2.14 |

- **SELinux:** Rocky Linux 9 and AlmaLinux 10 run the suite in a virtual
  machine with SELinux enforcing, as installed; the module and the playbook
  under "Set up" pass there. MongoDB's installation instructions for the
  Red Hat family describe its SELinux policy.
- **ARM64:** the suite also runs on ARM64 Hetzner Cloud servers. The whole
  test passes on Ubuntu 24.04. On Debian,
  MongoDB has no ARM64 packages, and the suite tests the module without
  MongoDB.
- **Linux 6.19.0 through 7.0.13:** the MongoDB server refuses to start on
  these kernels ([SERVER-121912](https://jira.mongodb.org/browse/SERVER-121912)),
  such as Ubuntu 26.04's kernel 7.0.0. On such a server the suite tests the
  module without MongoDB; in a container the server runs on its host's
  kernel.
- **Not tested:** MongoDB servers before 7.0, a TLS connection, and a
  sharded cluster.

[Limits on each distribution](../../docs/reference/limits-on-each-distribution.md) says what is limited for this module on [Debian 12](../../docs/reference/limits-on-each-distribution.md#debian-12), [Debian 13](../../docs/reference/limits-on-each-distribution.md#debian-13), and [Ubuntu 26.04](../../docs/reference/limits-on-each-distribution.md#ubuntu-2604), and why.

## Why it works this way

- **A password is compared by a login.** MongoDB stores a salted hash of a
  password, and a module script has no function for that hash. So the
  module logs in once as the user, from the node, and sets the password
  only when the server refuses the login. A changed password therefore
  costs one refused login in the server's log.
- **The passwords reach `mongosh` through its standard input,** inside the
  program that `mongosh` runs. A command line shows in the node's process
  list; standard input does not.
- **`mongosh` runs with `HOME=/dev/null`.** `mongosh` keeps a log file of
  every run in its home directory, `/root/.mongodb/`, and the log can hold
  the text of the program that it ran, with the passwords. No option of
  `mongosh` switches the log off for one run. With that `HOME`, `mongosh`
  cannot write the file. This is also why the module needs `mongosh` 2.2.0
  or newer: older ones print more than their one warning about it.
- **The module reads the server's state before it logs in.** A server
  without access control, a server without any user, and a server that
  needs a login answer two read commands differently. So a task needs no
  input that says which of the three the server is, and the playbook under
  "Set up" works on a new server and on every later run.
- **Only a user that can manage users becomes a server's first user.**
  MongoDB accepts any first user and then refuses every further one without
  a login. A first user without such a role would lock the server.
- **The connection is one input.** A MongoDB connection string already says
  the address, the port, TLS, and the replica set, and every MongoDB
  program takes it.
- **It holds no login,** because a connection string is not a private
  value: it would show in the task's result.
- **`roles_not_listed` is required with `roles`.** A task that owns a list
  says what happens to the rest, as on every MMX module with such a list.
- **A refused login fails a dryrun too, and has one try.** A wrong
  `login_password` does not get right by an earlier node task or by another
  try, so a dryrun reports it at once, and MMX does not log in again.
- **A replica set without a primary is tried again; one with another
  primary is not.** A replica set chooses its primary a few seconds after a
  `shell` task initiated it, so the administrator's task behind that task
  succeeds at a later try. A secondary server stays a secondary server.
- **A creation that ends without an answer is not tried again.** The next
  try would find the user, change nothing, and report `created: false`, and
  the failure would be gone from the result. The module asks the server
  once more and says whether the user is there.
- **`state: absent` refuses a password and roles.** A task that removes a
  user and also describes it says two things; every MMX module refuses
  that, and none ignores an input.
- **A missing role is a missing prerequisite.** An earlier node task, such
  as a `shell` task that creates a role of your own, can still provide it.
- **So is a missing user for a task without `password`.** A playbook can
  have two tasks for one user: the first creates it, and the second, without
  `password`, changes its roles. In a dryrun the first has created nothing
  yet, so the second is not checked and does not fail.
- **A failure of a task that created the user is not tried again.** The
  next try would find the user and report `created: false` for what this
  task created.
- **Replica set configuration is not this module's.** It stays a `shell`
  task with `mongosh`: it happens once for each replica set.
