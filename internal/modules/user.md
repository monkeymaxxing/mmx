# `user`

`user` manages one local user account on the node: its IDs, primary and
supplementary groups, home directory, login shell, full name, password hash,
and password lock, or its removal. `user` runs the node's native account
tools:
`useradd` to create an account, `chpasswd` for its password hash, and
`userdel` to remove it. The task manages the fields it gives; omitted fields
keep the account's existing values.

Use [`group`](group.md) for a group and its members,
[`authorized_keys`](authorized_keys.md) for the SSH keys an account accepts,
and [`directory`](directory.md) for MMX to manage a home directory's Linux
owner and permissions mode explicitly, such as a service account's home.

The [application-service example](../../examples/service-app/README.md)
combines accounts, directories, configuration, a complete `.service` unit,
and activation. The [account-and-unit example](../../examples/account-and-unit/README.md)
adds explicit account IDs, a service drop-in, and a separate removal playbook.

## Examples

### Create a service account with fixed IDs

```yaml
- group:
    name: app
    gid: 1200

- user:
    create_home_directory: false
    name: app
    uid: 1200
    primary_group: app
    system: true
```

On the node, MMX creates the group `app` with the ID 1200 and then the system
account `app` with the user ID 1200 and `app` as its primary group.
MMX assigns the configured ID when creating an account.
For an existing account, MMX checks that the current ID equals the configured
ID. Omit the IDs to keep the node's native allocation for new accounts
and the existing values for current accounts.

### A service account with its own directory

```yaml
- group: {name: app, system: true}

- user:
    create_home_directory: false
    name: app
    system: true
    primary_group: app
    home: /srv/app

- directory:
    path: /srv/app
    owner: app
    group: app
    permissions_mode: "0750"
```

The `user` task with `home: /srv/app` sets `create_home_directory: false`, so
the `directory` task creates `/srv/app` with the application's required owner
and mode.

### An account for a person

An account for a person usually has a full name, a home directory, a login
shell, and a password:

```yaml
- user:
    create_home_directory: true
    name: barbara
    full_name: Barbara Example
    shell: /bin/bash
    supplementary_groups: [sudo]
    supplementary_groups_not_listed: keep
    password_hash: "{{ .secrets.barbara_password_hash }}"
    apply_password_hash: only_when_account_has_no_password
```

MMX creates the account with `useradd` on the node. When creating an account
without `primary_group`, MMX uses a group named after the account, creating
the group if needed. The home directory, `/home/barbara` unless the task
gives `home`, is made from the node's skeleton directory (`/etc/skel`) with
the permissions `useradd` gives it on that node.
`full_name` is the account's comment field, the name `finger` and many login
screens show. MMX sets the whole field. `supplementary_groups_not_listed: keep` adds the group `sudo` and leaves the
account's other memberships alone.

`password_hash` is a password hash in crypt format (`$6$...`, `$y$...`),
never the password itself. Make a password hash on your controller machine
with `openssl passwd -6` or `mkpasswd`, and keep the password hash as a
[secret](../../docs/how-to/keep-secrets.md#create-the-vault-and-use-a-secret).
The `password_hash` field takes only one whole secret reference.

A role is a reusable list of node tasks with declared inputs, and a `user`
task can be part of a role.
Inside a role, `password_hash` can instead take one whole reference to a
role input declared `secret: true`, such as `"{{ .inputs.password_hash }}"`.
The role call fills such an input from a secret, so the hash is never
written in the YAML.

`apply_password_hash: only_when_account_has_no_password` sets the hash when
the account has no stored password, whether the account is new or already
exists. After MMX sets the hash, the person changes the password with
`passwd`, and MMX keeps the person's password. Use `on_every_run` for a
password the team manages, such as a break-glass account's.

### Remove an account and its home directory

```yaml
- user:
    name: barbara
    state: absent
    also_delete_home_directory_and_mail: true
```

The removal task for `barbara` removes the account, home directory, and mail
spool (`userdel --remove`). Without
`also_delete_home_directory_and_mail: true`, the home directory, mail, and
the account's other files stay.

## How it works

Omitting `state` selects `present`.

### Account IDs

`uid` takes an integer or a whole typed reference: a single `{{ ... }}`
expression that supplies an integer. `uid` selects a new account's ID and is
checked against an existing account. A mismatch or a collision with another
account fails before the account named in the task changes.
MMX does not renumber accounts or reassign the accounts' files.

### Defaults of a new account

New system accounts default to the home `/nonexistent` and the shell
`/usr/sbin/nologin`, ordinary accounts to `/home/<name>` and `/bin/sh`.
`system` also affects which ID the node allocates when the account is
created.

### The home directory

`create_home_directory` is required with `state: present`. With `true`, MMX makes sure the home
directory exists: created from the node's skeleton directory when the account
is created, and also, with `mkhomedir_helper`, when the account exists and the
directory is missing. With `false`, MMX never creates the home directory.

An existing home directory
is never changed or moved, and changing `home` never moves a directory.

### The password

A new account without `password_hash` has no password. MMX compares a hash
with the node's shadow database (`getent shadow`). On the node, MMX hands the
hash to `chpasswd` on standard input, never on a command line. The task's
result is suppressed like that of every node task that reads a secret, in
dryrun too: a dryrun shows whether the account would change, and never the
hash or which fields would change.

`apply_password_hash` is required with `password_hash` and has no default:

- `only_when_account_has_no_password` sets the hash on a new account or an
  existing account without a stored hash. After MMX sets the hash, MMX keeps
  any password the person sets later.
- `on_every_run` makes the stored hash equal the given one on every run.

`password_locked: true` stops password login and keeps the stored hash, so
`password_locked: false` brings the same password back. Omitted, MMX leaves
the lock alone. SSH keys still work on a locked account.

### Supplementary groups

A `user` task that lists `supplementary_groups` must also say
`supplementary_groups_not_listed`: `keep` adds the listed groups and leaves
the account's other memberships alone. With `remove`, the listed groups
become the account's whole set of supplementary groups, so
`supplementary_groups: []` removes all supplementary memberships.
Requiring `supplementary_groups_not_listed` follows the
[rule for a task that owns a list](../../docs/changing-mmx/design/modules.md#a-task-that-owns-a-list-says-what-happens-to-the-rest).
The listed groups must already exist. Configure each membership in a `user`
task's `supplementary_groups` or a [`group`](group.md) task's
`supplementary_members`, never in both modules.

### Removal

`state: absent` accepts only `name`, `state`, and
`also_delete_home_directory_and_mail`. A `user` task with `state: absent`
removes the local account with the node's native tools, without force.
The node's `userdel` policy may also remove an empty group named after the
account being removed. A missing account is unchanged. Local tools
do not modify directory-service accounts.

If the node stops after the account is gone and before its home directory
is, the next run reports unchanged and the directory stays. The interrupted
attempt's result is `unknown`, indicating that home directory removal may be
incomplete.

### What a run and a dryrun report

A run and a dryrun list what changed, or would change, in the result's
`value`: `account_created`, `fields_changed` (such as `shell` or
`password_hash`), `home_directory_created`, `groups_added`, and
`groups_removed`. A run that fails lists only what it changed before the
failure, so a node task in `on_failure` can read what happened. After an
interrupted run, each field is compared again.
Account and ID changes do not request a service refresh.

## Errors and what they mean

- **An ID mismatch or collision.** The task fails before the account changes.
  MMX does not renumber accounts. Correct the ID in the task, or change the
  account by hand.
- **`group sudo does not exist yet`.** The primary group or a listed
  supplementary group does not exist. Create the missing group with a
  [`group`](group.md) task before the `user` task.
- **A password hash written into the YAML.** Validation fails. Keep the hash
  as a secret and write `"{{ .secrets.<secret key> }}"`.
- **A `:` or a line break in `full_name`.** Validation rejects the `user` task
  containing the invalid `full_name` value.
- **`password_locked: false` on an account without a hash.** The task fails.
  Give the account a `password_hash` first.
- **A native tool refuses the removal,** for example because the account is
  in use. The node task fails and can be retried normally.

## Tested on

**Distributions:** Debian 12 and 13, Ubuntu 24.04 and 26.04, Arch Linux, openSUSE Leap 16.1, Rocky Linux 9, AlmaLinux 10, Fedora 45.

The base suite covers this module (`account_units.py`, `dns_tls_and_account_lookups.py`) and runs on every tested distribution.

The suite `account_units.py` tests accounts, including accounts for people
with password hashes, and group membership. While `chpasswd` runs, the suite
checks every process command line to verify that no password hash appears.
`server_resources.py` covers accounts too. The `user` fields documented on
the `user` module page are tested on Debian 12 and 13 and Ubuntu 24.04 and 26.04.
The distributions are Debian 12 and 13, Ubuntu 24.04 and 26.04 ([validation
record](../../docs/changing-mmx/validation.md)).

## Why it works this way

- **Membership fields name the relationship.** `primary_group` changes the
  account's primary group; `supplementary_groups` manages its other groups.
- **The password rule names the stored state.** `only_when_account_has_no_password`
  also initializes an existing account without a password hash.
- **Removal names both home and mail.** `also_delete_home_directory_and_mail`
  makes home directory removal and mail spool removal by `userdel --remove`
  explicit before the task runs.

- **A password hash is accepted only as a secret.** Everyone who can read the
  project could otherwise try to crack the password hash.
- **The hash goes to `chpasswd` on standard input.** On a command line, every
  account on the node could read the password hash while the command runs.
- **`apply_password_hash` has no default.** `apply_password_hash` says who
  manages the password after the first run: the person or the team.
- **Home creation is explicit.** A service account may use `/nonexistent`,
  while a person needs a home. The task author sets `create_home_directory`
  to choose whether MMX creates the home directory, following the
  [required-setting rule](../../docs/changing-mmx/design/modules.md#a-setting-with-no-safe-guess-is-required).
- **`:` and line breaks are refused in `full_name`.** The account database
  separates its fields with them.
- **`password_locked: false` without a hash fails.** Unlocking such an
  account would allow login without a password.
