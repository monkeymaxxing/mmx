# `rabbitmq_user`

`rabbitmq_user` manages one user of a RabbitMQ server: its password, its
tags, and its permissions on virtual hosts. RabbitMQ is a message broker: a
server that takes messages from programs and hands them to other programs
through queues. An application logs in to the RabbitMQ server as a RabbitMQ
user. RabbitMQ users live inside RabbitMQ and are no Linux accounts.
A **virtual host** is a separate set of queues and exchanges inside one
server. The module runs
RabbitMQ's own program `rabbitmqctl` as the account `rabbitmq` on the
**node** of the server; a node is a Linux server that MMX manages.

In a RabbitMQ cluster, users belong to the whole cluster: let one node of
the cluster run the `rabbitmq_user` node tasks.

## Set up

The module installs nothing. RabbitMQ's package brings the programs that the
module runs, and a `systemd_service` task starts the server. The playbook of
the example project
[`rabbitmq-server-for-one-application`](../../examples/rabbitmq-server-for-one-application/README.md)
starts with the **node tasks** that do both; a node task is one entry of a
playbook's `node_tasks`, one action that a node performs. Each task has the
condition that picks its distribution, so the same playbook serves a fleet
with several distributions:

```yaml
# mmx run configure
fleet_stages:
  - id: configure
    nodes: all

    node_tasks:
      # Debian and Ubuntu: RabbitMQ from the distribution's own packages.
      # logrotate rotates the server's log files; on Debian the package only
      # recommends it, and a server may install no recommended packages.
      - when: {equals: ["{{ .node.package_manager }}", apt]}
        apt_package: {packages: [{name: rabbitmq-server, version: newest_when_first_installed}, {name: logrotate, version: newest_when_first_installed}]}

      # Rocky Linux and AlmaLinux package no RabbitMQ. The RabbitMQ project
      # serves two package repositories for them: one with Erlang, the
      # language that RabbitMQ is written in, and one with the server. Each
      # repository signs its packages with its own key, which MMX fetches and
      # checks against the SHA-256 here. The repositories hold Erlang for
      # x86_64 nodes only.
      - when: {equals: ["{{ .node.package_manager }}", dnf]}
        dnf_repository:
          name: rabbitmq-erlang
          description: Erlang for RabbitMQ, from the RabbitMQ project
          baseurl:
            - https://yum1.rabbitmq.com/erlang/el/9/$basearch
            - https://yum2.rabbitmq.com/erlang/el/9/$basearch
          signing_key_url: https://github.com/rabbitmq/signing-keys/releases/download/3.0/cloudsmith.rabbitmq-erlang.E495BB49CC4BBE5B.key
          signing_key_sha256: 84df2e5fd80d464c3eb9acd2f751b2f6a723438200915dd50fbf12f08698e4ec
      - when: {equals: ["{{ .node.package_manager }}", dnf]}
        dnf_repository:
          name: rabbitmq-server
          description: RabbitMQ server, from the RabbitMQ project
          baseurl:
            - https://yum2.rabbitmq.com/rabbitmq/el/9/noarch
            - https://yum1.rabbitmq.com/rabbitmq/el/9/noarch
          signing_key_url: https://github.com/rabbitmq/signing-keys/releases/download/3.0/cloudsmith.rabbitmq-server.9F4587F226208342.key
          signing_key_sha256: 17b3eeb98b7aabe659b0e2579715d1ae4e328b58a401434599495779c59ac73a
      - when: {equals: ["{{ .node.package_manager }}", dnf]}
        dnf_package: {packages: [{name: erlang, version: newest_when_first_installed}, {name: rabbitmq-server, version: newest_when_first_installed}, {name: logrotate, version: newest_when_first_installed}]}

      # No package starts the server under MMX; this task does, and it keeps
      # the server starting at boot.
      - systemd_service:
          name: rabbitmq-server
          state: running
          start_at_boot: true
```

The repositories of the RabbitMQ project always hold the newest RabbitMQ.
MMX fetches each repository's key on the controller machine, the machine
where you run `mmx`. On Debian and Ubuntu too, the `systemd_service` task is
the server's first start: MMX keeps a package from starting its service
while it installs.

## Inputs

| Input | Meaning | When omitted |
|---|---|---|
| `name` | The user's name, such as `shop`. It must not be empty or start with a hyphen (`-`). | Required. |
| `state` | `present` or `absent`. `absent` removes the user and accepts only `name`. | `present`. |
| `password` | The user's password, usually a secret such as `"{{ .secrets.shop_rabbitmq_password }}"`. It must not be empty, start or end with white space, or hold a line break. | The password of an existing user is not touched. A task cannot create a user without it: when the user does not exist, a run fails, and a dryrun reports `not_checked`. |
| `tags` | RabbitMQ's user tags, as a list, such as `[monitoring]`. A tag says what the user may do in RabbitMQ's management interface: `administrator`, `monitoring`, `policymaker`, `management`. An application's user needs none. | The user's tags are not touched. |
| `tags_not_listed` | `remove` or `keep`. `remove`: the task takes away every tag of the user that `tags` does not list, and `tags: []` takes every tag away. `keep`: the task adds the listed tags and leaves the user's other tags alone. | Required with `tags`, and not allowed without it. |
| `permissions` | A list. Each entry names a `virtual_host` and the three regular expressions `configure`, `write`, and `read`; all four are required. | The user's permissions are not touched. |
| `permissions_not_listed` | `remove` or `keep`. `remove`: the task clears the user's permissions on every virtual host that `permissions` does not list. `keep`: it leaves them alone. | Required with `permissions`, and not allowed without it. |

**The three expressions of a permission.** RabbitMQ checks each thing a user
does to a queue or an exchange against one expression of the user's
permission on that virtual host: `configure` for creating, deleting, and
changing one, `write` for publishing messages to one, and `read` for taking
messages from one. The user may do it when the expression matches the name.
`".*"` matches every name, and an empty text `""` allows nothing. Write each
expression in quotes.

**The password** reaches `rabbitmqctl` only through its standard input. It
is on no command line, in no file, and in no result. `password` is a private
value: a secret in it does not hide the task's result, and MMX shows
`[redacted]` wherever a text of the result would hold it.

## Outputs

| Output | Meaning |
|---|---|
| `created` | `true` when the task created the user. In a dryrun: `true` when a run would create it. |

A later node task reads the `created` output as `.node_tasks.<id>.created`.

## What a dryrun reports

A dryrun asks the server the same questions as a run and changes nothing.
One of the questions is whether the password is the user's: the server
answers it as it answers a login.

- **Something to change:** `would_change`, with a reason that names each
  change, such as `would set the password of the user shop; would add the
  tag monitoring to the user shop`.
- **`not_checked`** while RabbitMQ's programs are not installed, while the
  server does not answer, for a permission on a virtual host that does not
  exist, and for a user that does not exist when the task gives no
  `password`: an earlier node task of the same run may install the package,
  start the server, create the virtual host, or create the user.
- **A failure, as in a run,** for an input that breaks a rule.
- **A dryrun cannot know whether RabbitMQ accepts a regular expression:**
  only the command that sets a permission checks it. A dryrun reports
  `would_change` for a permission whose expression RabbitMQ will refuse.
  The run then fails with RabbitMQ's message, and MMX does not try it again.
- **What the questions leave on the node:** RabbitMQ 4.3 writes one line
  into the server's log for each question about a password, with the user's
  name and without the password. While the server does not run, on a node
  with the packages of the RabbitMQ project, `rabbitmq-diagnostics` starts a process named `epmd` as the account
  `rabbitmq`, and that process stays. `epmd` is the small program that tells
  RabbitMQ's programs on which port the server listens for them; the server
  would start it too. On a server that has never run, `rabbitmq-diagnostics`
  also creates the file `/var/lib/rabbitmq/.erlang.cookie`, the key with
  which RabbitMQ's programs reach the server; the server's first start would
  create it.

## Examples

The application's user, which may do everything inside the virtual host
`shop` and nothing in any other:

```yaml
- rabbitmq_user:
    name: shop
    password: "{{ .secrets.shop_rabbitmq_password }}"
    permissions:
      - {virtual_host: shop, configure: ".*", write: ".*", read: ".*"}
    permissions_not_listed: remove
```

A user for a monitoring program, which may inspect statistics without
consuming messages:

```yaml
- rabbitmq_user:
    name: metrics
    password: "{{ .secrets.metrics_rabbitmq_password }}"
    tags: [monitoring]
    tags_not_listed: remove
    permissions:
      - {virtual_host: shop, configure: "", write: "", read: ""}
      - {virtual_host: billing, configure: "", write: "", read: ""}
    permissions_not_listed: remove
```

The `monitoring` tag permits access to statistics in the management interface.
The empty permission expressions grant no queue operations. RabbitMQ `read`
permission lets a user take messages from queues; it is not a statistics permission.

Remove the user `guest`, which every new RabbitMQ server has:

```yaml
- rabbitmq_user:
    name: guest
    state: absent
```

## Errors and what they mean

Every reason starts with `rabbitmq_user`.

| The reason holds | What it means, and what to do |
|---|---|
| `the user NAME does not exist, and the task has no password to create it; add password, or create the user with a rabbitmq_user task in an earlier node task` | The server has no such user, and the task gives no password. A run fails without another try, and nothing was changed. Add `password`, or put the task that creates the user before this task. |
| `rabbitmq_user.password: must not ...` | The password is empty, starts or ends with white space, or holds a line break. RabbitMQ would store another password than the one written. Choose a password without these; in a secret, look for a line break at the end. |
| `rabbitmq_user.tags_not_listed: is required with tags: ...`, `rabbitmq_user.permissions_not_listed: is required with permissions: ...` | The task gives a list and does not say what happens to the rest. Add `remove` or `keep`. |
| `rabbitmq_user.tags: is required with tags_not_listed`, `rabbitmq_user.permissions: is required with permissions_not_listed` | The task says what happens to the rest and gives no list. Add the list; `[]` with `remove` takes everything away. |
| `rabbitmq_user.permissions: entry N ...` | An entry of `permissions` lacks one of its four keys, has another key, or has a value that is no text. Write `virtual_host`, `configure`, `write`, and `read`, each in quotes. |
| `rabbitmq_user.state: absent accepts only name; remove INPUT` | A task that removes the user also describes it. Remove the named input. |
| `the virtual host NAME does not exist; create it with a rabbitmq_virtual_host task in an earlier node task` | A permission names a virtual host that the server does not have. A run fails without another try, and nothing was changed. Put a `rabbitmq_virtual_host` task before this task, or correct the name. |
| `the program ... is not installed; install the package rabbitmq-server in an earlier node task` | The node has no RabbitMQ, or has it from another source than a package, so that `/usr/lib/rabbitmq/bin/rabbitmqctl` is missing. Put the tasks of [Set up](#set-up) before this task. |
| `the RabbitMQ server on this node does not answer; start its service in an earlier node task (rabbitmq-diagnostics check_running said: ...)` | The server is stopped or still starting. MMX tries a run again, as every failed node task. Start the server with a `systemd_service` task. When it does not start, read its journal: `journalctl -u rabbitmq-server`. |
| `RabbitMQ refuses a regular expression of the permissions of the user NAME on the virtual host NAME: ...` | RabbitMQ cannot read one of the three expressions of that permission; the text behind the colon is its own message. The task fails without another try. What the task did before was done, such as creating the user, and the result says `changed`: MMX counts every changing command that ran, also the one that RabbitMQ refused. Correct the expression; the next run does what is missing. |
| `asking the server whether the password is the one of the user NAME: ...` | The question about the password got another answer than yes or no. No password was set. Act on RabbitMQ's message. |
| `created the user NAME, and then this failed: ...` | The task created the user, and a later step failed: the tags or a permission. The text behind the colon names the step and holds RabbitMQ's own message. MMX does not try this task again: a new try would find the user and report `created: false` for a user that this task created. Act on RabbitMQ's message and run the playbook again; the task then does what is still missing. |
| `reading ...`, `creating the user NAME: ...`, `setting the password ...`, `setting the tags ...`, `setting the permissions ...`, `clearing the permissions ...`, `removing the user NAME: ...` | A command of `rabbitmqctl` failed. The text behind the colon is RabbitMQ's own message; act on it. MMX tries the task again, and the new try does exactly what is still missing. |

## Tested on

**Distributions:** Debian 12 and 13, Ubuntu 24.04 and 26.04, Rocky Linux 9, AlmaLinux 10.

The integration suite `tests/integration/rabbitmq.py` runs the module against
a real server on these distributions:

| Distribution | RabbitMQ | Comes from |
|---|---|---|
| Debian 12 | 3.10.8 | the distribution's package |
| Debian 13 | 4.0.5 | the distribution's package |
| Ubuntu 24.04 | 3.12.1 | the distribution's package |
| Ubuntu 26.04 | 4.0.5 | the distribution's package |
| Rocky Linux 9, AlmaLinux 10 | 4.3.6 | the RabbitMQ project's repositories, which serve the newest release |

## Why it works this way

- **Monitoring grants no message consumption.** The `monitoring` tag gives
  access to statistics; `read` permission lets the user take queue messages.

- **The module asks the server whether the password is the user's**
  (`rabbitmqctl authenticate_user`) and sets the password only when the
  server says no. RabbitMQ stores a salted hash, which a module's script
  cannot compute. The cost is one refused login when a password changes;
  RabbitMQ 4.3 writes one line for each such question into the server's
  log, with the user's name and without the password.
- **The password goes through standard input, in the plain form of the
  command.** A command line shows in the node's process list. With any
  further argument, `rabbitmqctl` 4 gives its program no standard input and
  never answers, so the module writes only the command and the user's name.
- **The module refuses a password that RabbitMQ would change.** RabbitMQ
  takes only the first line of what it reads and cuts white space off both
  ends, without a word. It would store another password than the playbook
  holds, and the application could not log in.
- **`tags_not_listed` and `permissions_not_listed` have no default,** as
  every list that a task owns in MMX: the setting decides whether a rerun
  takes something away from the user.
- **A step that fails on a user that existed before is tried again.** The
  module finds each step from what the server holds, so a new try repeats
  exactly the step that failed.
- **A step that fails after the task created the user is not tried again.**
  A new try would find the user and report `created: false`, and a later
  node task that reads `created` would act on a wrong answer. The next run
  of the playbook does what is still missing.
- **A task without `password` for a user that does not exist is a missing
  prerequisite,** not a broken rule: an earlier node task of the same
  playbook may create the user, so the playbook's first dryrun must pass.
- **The module reads the list of users first.** RabbitMQ answers a wrong
  password and a missing user with the same words, so only the list says
  which of the two it is.
- **The module runs the real `rabbitmqctl` program as the account
  `rabbitmq`** (`runuser -u rabbitmq -- /usr/lib/rabbitmq/bin/rabbitmqctl`),
  not the command `rabbitmqctl`. The command of Debian and Ubuntu hands its
  arguments to a shell: on the test nodes a `$` and a backslash in a name
  or an expression came out different, and a command inside an argument ran.
- **It asks first whether the server runs**
  (`rabbitmq-diagnostics check_running`). RabbitMQ's programs end with the
  same exit code for a stopped server and for several other errors, and a
  server that still starts already answers a ping.
- **A name and a tag must not start with a hyphen.** RabbitMQ's programs
  read such an argument as an option. A regular expression may start with
  a hyphen.
