# `rabbitmq_virtual_host`

`rabbitmq_virtual_host` manages one virtual host of a RabbitMQ server.
RabbitMQ is a message broker: a server that takes messages from programs and
hands them to other programs through queues. A **virtual host** is a separate
set of queues, exchanges, and policies inside one RabbitMQ server, so several
applications can share a server without seeing each other's queues. The
module runs RabbitMQ's own program `rabbitmqctl` as the account `rabbitmq`
on the **node** of the server; a node is a Linux server that MMX manages.

In a RabbitMQ cluster, virtual hosts belong to the whole cluster: let one
node of the cluster run the `rabbitmq_virtual_host` node tasks.

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
| `name` | The virtual host's name, such as `shop`. It must not be empty or start with a hyphen (`-`). RabbitMQ's own default virtual host is named `/`. | Required. |
| `state` | `present` or `deleted_with_all_its_data`. | `present`. |

- **`present`** creates the virtual host when the server does not have it. A
  virtual host that exists is unchanged.
- **`deleted_with_all_its_data`** deletes the virtual host with its queues,
  its messages, its exchanges, and its policies. The deletion also removes
  every user's permissions on the virtual host. A virtual host that the
  server does not have is unchanged.

## Outputs

| Output | Meaning |
|---|---|
| `created` | `true` when the task created the virtual host. In a dryrun: `true` when a run would create it. |

A later node task reads the `created` output as `.node_tasks.<id>.created`.

## What a dryrun reports

A dryrun asks the server the same questions as a run and changes nothing.

- **A virtual host to create or delete:** `would_change`, with the reason
  `would create the virtual host shop` or `would delete the virtual host
  shop with all its data`.
- **`not_checked`** while RabbitMQ's programs are not installed, and while
  the server does not answer: an earlier node task of the same run may
  install the package or start the server.
- **A failure, as in a run,** for a name that is empty or starts with a
  hyphen.
- **What the questions leave on the node:** nothing while the server runs.
  While it does not run, on a node with the packages of the RabbitMQ
  project, `rabbitmq-diagnostics` starts a process named `epmd` as the
  account `rabbitmq`, and that process stays. `epmd` is the
  small program that tells RabbitMQ's programs on which port the server
  listens for them; the server would start it too. On a server that has
  never run, `rabbitmq-diagnostics` also creates the file
  `/var/lib/rabbitmq/.erlang.cookie`, the key with which RabbitMQ's programs
  reach the server; the server's first start would create it.

## Examples

Make sure the server has the virtual host `shop`:

```yaml
- rabbitmq_virtual_host:
    name: shop
```

Delete the virtual host `shop-test` with every queue and message in it:

```yaml
- rabbitmq_virtual_host:
    name: shop-test
    state: deleted_with_all_its_data
```

## Errors and what they mean

Every reason starts with `rabbitmq_virtual_host`.

| The reason holds | What it means, and what to do |
|---|---|
| `rabbitmq_virtual_host.name: must not ...` | The name is empty or starts with a hyphen. The task fails before it runs any program, in a dryrun too. Correct `name`. |
| `the program ... is not installed; install the package rabbitmq-server in an earlier node task` | The node has no RabbitMQ, or has it from another source than a package, so that `/usr/lib/rabbitmq/bin/rabbitmqctl` is missing. A run fails without another try. Put the tasks of [Set up](#set-up) before this task. |
| `the RabbitMQ server on this node does not answer; start its service in an earlier node task (rabbitmq-diagnostics check_running said: ...)` | The server is stopped or still starting. MMX tries a run again, as every failed node task. Start the server with a `systemd_service` task. When it does not start, read its journal: `journalctl -u rabbitmq-server`. |
| `reading the virtual hosts: ...` | `rabbitmqctl list_vhosts` failed. The text behind the colon is RabbitMQ's own message; act on it. |
| `creating the virtual host NAME: ...`, `deleting the virtual host NAME: ...` | RabbitMQ did not make the change. The text behind the colon is RabbitMQ's own message; act on it. MMX tries the task again. |

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

- **The module runs the real `rabbitmqctl` program as the account
  `rabbitmq`** (`runuser -u rabbitmq -- /usr/lib/rabbitmq/bin/rabbitmqctl`),
  not the command `rabbitmqctl`. The command of Debian and Ubuntu hands its
  arguments to a shell: on the test nodes a `$` and a backslash in a name
  came out different, and a command inside a name ran. The call above is the
  one that RabbitMQ's own wrapper makes, and a name arrives as written.
- **The module asks first whether the RabbitMQ server runs**
  (`rabbitmq-diagnostics check_running`). RabbitMQ's programs end with the
  same exit code for a stopped server and for several other errors, and a
  server that still starts already answers a ping.
- **The module reads the list of virtual hosts before changing anything.**
  `rabbitmqctl add_vhost` succeeds also for a virtual host that exists, so
  only the list says whether the task changed the node.
- **The removal is spelled `deleted_with_all_its_data`,** as every state in
  MMX that destroys data: the queues and their messages go with the virtual
  host.
- **A name must not start with a hyphen.** RabbitMQ's programs read such an
  argument as an option.
