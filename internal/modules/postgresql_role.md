# `postgresql_role`

`postgresql_role` manages one role inside a PostgreSQL database server that
is already installed and runs: the role's password, what it may do, and the
roles it is a member of. A role is PostgreSQL's word for an account:
something that logs in, owns things, or both. An application needs one role
to log in with and one database that the role owns; the database is a
[`postgresql_database`](postgresql_database.md) task.

The module runs `psql`, PostgreSQL's own command line client, on the node;
MMX contains no database driver. Installing the server, writing its
configuration files, and starting it are package, `file`, and
`systemd_service` tasks. The example
[A PostgreSQL server for one application](postgresql_database.md#a-postgresql-server-for-one-application)
shows them, with a role and a database.

Without connection fields, a task uses the server on the same node: MMX runs
`psql` as the operating system account `postgres`, which the server accepts
as its administrator through its local socket, without a password. For a
server on another machine or another login, the module takes the connection
fields that [`postgresql_database`](postgresql_database.md) takes too. The page
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

### A role that an application logs in with

```yaml
- postgresql_role:
    name: shop
    password: "{{ .secrets.shop_database_password }}"
    can_log_in: true
    member_of_roles: [pg_monitor]
    member_of_roles_not_listed: keep
```

The task reads the role from the server's catalog (the server's own tables
about its roles and databases) and changes only what differs. It creates the
role `shop`, which may log in, with the password of the secret
`shop_database_password`. The application's own configuration file reads the
same secret. The role becomes a member of PostgreSQL's predefined role
`pg_monitor`, and keeps any other membership that it has. A second run
changes nothing.

### A role on a server on another machine

```yaml
- postgresql_role:
    name: shop
    password: "{{ .secrets.shop_database_password }}"
    can_log_in: true
    database_server_host: database01.example.net
    login_role: deploy_administrator
    login_password: "{{ .secrets.database_administrator_password }}"
    database_server_certificate_authority_file: /etc/ssl/example-authority.crt
```

The node needs the `psql` program for this, and no server of its own:
install the client package (`postgresql-client` on Debian and Ubuntu,
`postgresql` on the Red Hat family). MMX runs `psql` as root on the node,
logs in to `database01.example.net` as the role `deploy_administrator`, and
accepts the server only when the authority in
`/etc/ssl/example-authority.crt` signed its certificate.
`deploy_administrator` must be a superuser, because the task has a
`password`.

### Remove a role

```yaml
- postgresql_role:
    name: shop_staging
    state: absent
```

`state: absent` removes the role and accepts only `name` and the connection
fields. A missing role is unchanged.

## How it works

- **`can_log_in` is required with `state: present`.** The task writes
  `true` to allow login or `false` for a role used only for privileges.
  Other omitted attributes keep their values on an existing role. A new
  role gets PostgreSQL's default, false, for `superuser`,
  `can_create_databases`, `can_create_roles`, and `can_start_replication`.
- **All of one task's changes happen together or not at all.** The server
  applies all changes from the node task in a single transaction. When the
  server refuses one statement, the role stays as the role was before the
  task, and the failed result says `change: unchanged`.
- **A name that is longer than the server takes fails the task** before it
  changes anything, in `postgresql_role` and `postgresql_database`. The
  limit is 63 bytes unless the server was built otherwise. MMX asks the
  server for the name length limit. PostgreSQL itself would cut the name
  and create or change the role with the shorter name, with only a notice.
- **`state: absent`** removes the role. PostgreSQL refuses to remove a role
  that still owns a database or other objects; the task then fails with the
  server's message and changes nothing.

The result's `value` has two fields: `created`, `true` when the task created
the role, and `changed_attributes`, the names of the task's fields that
differed on an existing role, such as `[can_log_in, password]`.
`changed_attributes` is empty for a role that the task created. A dryrun
reports `created` and `changed_attributes` as a run would report the fields.

### The password

MMX compares the password without a login attempt. The clear password,
meaning the original password before hashing, never reaches the server.

PostgreSQL does not store a password. It stores a salted hash of it: a text
that starts with `SCRAM-SHA-256$`. MMX reads that stored form, computes the
same hash from the task's password with the stored salt, and compares the
two. When they are equal, the password is unchanged. To set a password, MMX
computes the stored form itself, with a new random salt, and sends the
server only that form. So the clear password is in no SQL statement, in none
of the server's logs, and on no command line.

- **Only a superuser can read stored forms.** A task with `password` needs a
  login role that is a superuser, which the default login is. With another
  login role the task fails before it changes anything and says so.
- **The form.** MMX writes the form SCRAM-SHA-256, with the server's own
  count of iterations. Every supported version reads the SCRAM-SHA-256
  form, with the `pg_hba.conf` methods `scram-sha-256` and `md5` alike.
- **The older md5 form is replaced.** A role whose password is still stored
  in the md5 form gets the SCRAM-SHA-256 form at the next run, also when the
  password is the same. The result then names `password` in
  `changed_attributes`, and its reason says why. Enterprise Linux 9's
  PostgreSQL 13 still makes md5 forms by default. A role whose password must
  stay in the md5 form for an old client gets no `password` field.
- **A password that is not plain ASCII works.** The server and its clients
  prepare such a password before they hash it (SASLprep, RFC 4013): they
  replace some characters and combine others, and they take the bytes as
  written when the rules refuse the text. MMX prepares the password in the
  same way, so a login with it works.
- **An empty password is rejected.** PostgreSQL takes an empty password as
  "remove the password". Leave `password` out to keep the password that the
  role has.
- **Where the stored form shows.** The server writes the statement, with the
  stored form in it, into its own log when the server's setting
  `log_statement` is `ddl` or `all`, or when the statement fails; the
  extension `pg_stat_statements` keeps it too. `psql`'s own `\password`
  command has the same effect. MMX does not switch the server's logging off
  for its statement. The stored form is not the password.

A secret in `password` does not hide the task's result: it shows `created`
and `changed_attributes`, and a failure shows the server's message. MMX
shows `[redacted]` wherever a text of the result holds the password or its
stored form.
[A login does not hide a node task's result](../../docs/reference/secrets.md#a-login-does-not-hide-a-node-tasks-result)
has the rule.

### Memberships

`member_of_roles` lists the roles that the role is a member of, among them
PostgreSQL's predefined roles such as `pg_monitor` and `pg_read_all_data`.
Each must exist. `member_of_roles_not_listed` says what happens to the role's
other memberships, and has no default:

- `keep` adds the listed memberships and leaves the others alone.
- `remove` makes the list the whole set. `member_of_roles: []` with `remove`
  removes every membership.

A role in `member_of_roles` that does not exist fails the task in a run, before
anything changes, with a message that names the role. A dryrun reports the
task `not_checked`, because an earlier node task may create the role.
`pg_read_all_data` and `pg_write_all_data` exist from PostgreSQL 14, so not
in the PostgreSQL 13 of Enterprise Linux 9.

Memberships changed with PostgreSQL 16. A membership is recorded once for
each role that granted the membership. Each record says whether the member
gets the privileges of the role whose membership was granted.

- A role counts as a member when at least one record gives it the
  privileges.
- `remove` takes away every record of a membership, whoever granted it. MMX
  removes a membership record by record, each in the name of the role that
  granted it, because a plain `REVOKE` removes only the record of the role
  that runs it and reports success.
- `remove` also deletes a record that only lets the role administer another
  role. PostgreSQL makes such a record for the role that created another
  role. A listed role keeps its administration records.
- After its statements the task reads the role again, and fails when the
  role is not as the task says.

From PostgreSQL 16, a login role that is no superuser also needs the `ADMIN`
option on each role in `member_of_roles`. The `ADMIN` option permits the
login role to grant membership in the listed role to another role. The page
[connection section](../../docs/how-to/manage-a-database.md#a-database-server-on-another-machine)
says what else a login role must be allowed to do.

### Server certificate verification

With `database_server_host`, the client verifies the server by default.
The [`postgresql_database` connection section](postgresql_database.md#server-certificate-verification)
states the client requirements and how to supply a trusted CA file.
`dangerously_skip_server_certificate_check: true` disables verification.
Server certificate verification does not apply to local socket connections.

## Tested on

**Distributions:** Debian 12 and 13, Ubuntu 24.04 and 26.04, Rocky Linux 9, AlmaLinux 10.

The suite `tests/integration/postgresql_databases.py` runs on Debian 12 and 13, Ubuntu 24.04 and 26.04, Rocky Linux 9, and AlmaLinux 10, in Incus containers on the Debian family and in virtual machines on the Red Hat family, against the PostgreSQL that each
distribution packages: PostgreSQL 13 (Rocky Linux 9), 15, 16 (AlmaLinux 10 and Ubuntu 24.04), 17, and 18. On
Ubuntu 26.04 it is PostgreSQL 18.6. The suite also passed on Hetzner
Cloud servers with Debian 13 and with Rocky Linux 9, with SELinux
`enforcing`.

Not tested: a second PostgreSQL server on one node, chosen with
`database_server_port`, and a database server that hides stored passwords
from its administrator, as some managed database services do.

## Why it works this way

- **Remote servers are verified by default.** SQL and login information
  must reach the intended server. Setting
  `database_server_certificate_authority_file` selects the trusted
  certificate authority and keeps server verification enabled.
  `dangerously_skip_server_certificate_check` disables server certificate
  verification, following
  [protective choices](../../docs/changing-mmx/design/modules.md#protective-choices-come-in-three-levels).

- **Every present role states whether login is allowed.** `can_log_in` has
  no safe guess; [required settings](../../docs/changing-mmx/design/modules.md#a-setting-with-no-safe-guess-is-required)
  keep that choice visible.
- **The membership list owns all unnamed records.** `member_of_roles_not_listed: remove`
  also removes administration-only records. Keeping hidden grants would
  leave privileges outside the declared set.

- **A password is compared by its stored form, never by a login attempt.** A
  login attempt works only for a role that may log in from the node by the
  server's access rules (`pg_hba.conf`), and each attempt with a password
  that differs is a failed login in the server's log.
- **MMX sends the stored form, never the password.** The server would write
  a statement with a clear password into its log when the statement fails
  or when it logs statements.
- **Always SCRAM-SHA-256, also in place of a stored md5 form of the same
  password.** A SCRAM form works with the `pg_hba.conf` methods
  `scram-sha-256` and `md5` on every supported version. An md5 form is
  refused where the method is
  `scram-sha-256`, PostgreSQL 18 warns about each one, and a later version
  drops the form. Following the server's `password_encryption` would keep
  making md5 forms on Enterprise Linux 9.
- **A password that is not ASCII is prepared as PostgreSQL does (SASLprep),
  not refused.** A hash of the bytes as written matches neither the server's
  own hash nor what a client sends at login (measured on PostgreSQL 13 to
  18), and "ASCII only" would be a limit with no reason a user could see.
- **MMX does not switch off the server's statement logging while it sets a
  password.** The statement holds the hash, not the password, as with
  `psql`'s `\password`. Switching logging off would remove the change of a
  password from the record that the server's administrator keeps.
- **No field like `allow_creating_account_without_password`.** A PostgreSQL role
  without a password cannot use password authentication, so omitting
  `password` does not enable password login.
