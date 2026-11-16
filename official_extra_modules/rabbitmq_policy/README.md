# `rabbitmq_policy`

`rabbitmq_policy` manages one policy of a virtual host of a RabbitMQ server.
RabbitMQ is a message broker: a server that takes messages from programs and
hands them to other programs through queues. A **virtual host** is a separate
set of queues and exchanges inside one server. A **policy** gives settings,
such as the most messages a queue may hold, to every queue or exchange of a
virtual host whose name matches the policy's pattern. The module runs
RabbitMQ's own program `rabbitmqctl` as the account `rabbitmq` on the
**node** of the server; a node is a Linux server that MMX manages.

In a RabbitMQ cluster, policies belong to the whole cluster: let one node of
the cluster run the `rabbitmq_policy` node tasks.

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
| `name` | The policy's name, such as `limit-queue-length`. It must not be empty or start with a hyphen (`-`). | Required. |
| `virtual_host` | The virtual host that holds the policy, such as `shop`. RabbitMQ's own default virtual host is named `/`. | Required. |
| `state` | `present` or `absent`. `absent` removes the policy and accepts only `name` and `virtual_host`. | `present`. |
| `pattern` | A regular expression. The policy applies to the queues or exchanges whose name it matches: `".*"` matches every name, `"^orders\\."` the names that start with `orders.`. | Required for `present`. |
| `apply_to` | What the policy applies to: `queues`, `exchanges`, `all`, `classic_queues`, `quorum_queues`, or `streams`. RabbitMQ before 3.12 takes only the first three. | Required for `present`. |
| `definition` | A mapping of RabbitMQ's policy keys to their values, such as `max-length: 100000`. It needs at least one key. Write a whole number without a fraction. | Required for `present`. |
| `priority` | A whole number. When several policies match one queue, RabbitMQ applies only the one with the highest priority. | Required for `state: present`. |

The keys of `definition` are RabbitMQ's own, and RabbitMQ decides which it
takes: the
[RabbitMQ documentation](https://www.rabbitmq.com/docs/policies) lists
them. Common ones are `max-length` (the most messages in a queue),
`message-ttl` (how many milliseconds a message stays), `expires`,
`overflow`, and `dead-letter-exchange`.

A policy that differs in any part is written again as a whole: RabbitMQ has
no command that changes one part of a policy.

## Outputs

| Output | Meaning |
|---|---|
| `created` | `true` when the task created the policy. In a dryrun: `true` when a run would create it. |

A later node task reads the `created` output as `.node_tasks.<id>.created`.

## What a dryrun reports

A dryrun asks the server the same questions as a run and changes nothing.

- **A policy to create, change, or remove:** `would_change`, with the
  reason, such as `would create the policy limit-queue-length of the
  virtual host shop`, or `would change the policy limit-queue-length of the
  virtual host shop: definition, priority`, which names the inputs that
  differ.
- **`not_checked`** while RabbitMQ's programs are not installed, while the
  server does not answer, and while the virtual host of a policy with
  `state: present` does not exist: an earlier node task of the same run may
  install the package, start the server, or create the virtual host.
- **A failure, as in a run,** for an input that breaks a rule.
- **A dryrun cannot know whether RabbitMQ accepts the definition, the
  pattern, or the value of `apply_to`:** only the command that sets a
  policy checks them, and what it accepts depends on RabbitMQ's version. A
  dryrun reports `would_change` for a policy that RabbitMQ will refuse. The
  run then fails with RabbitMQ's message, and MMX does not try it again. A
  refused policy changes nothing on the server: the policy that was there
  before stays. The task's result still says `changed`, because MMX counts
  every changing command that ran and cannot see what a refused command
  did.
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

Let no queue of the virtual host `shop` hold more than 100000 messages:

```yaml
- rabbitmq_policy:
    name: limit-queue-length
    virtual_host: shop
    pattern: ".*"
    apply_to: queues
    definition:
      max-length: 100000
    priority: 0
```

Give the order queues an hour for each message, and send a message that
was not taken in that time to the exchange `expired-orders`. The priority
makes `orders-expire` apply instead of `limit-queue-length` for the order
queues:

```yaml
- rabbitmq_policy:
    name: orders-expire
    virtual_host: shop
    pattern: "^orders\\."
    apply_to: queues
    priority: 10
    definition:
      message-ttl: 3600000
      dead-letter-exchange: expired-orders
```

Remove the policy `limit-queue-length` of the virtual host `shop`:

```yaml
- rabbitmq_policy:
    name: limit-queue-length
    virtual_host: shop
    state: absent
```

## Errors and what they mean

Every reason starts with `rabbitmq_policy`.

| The reason holds | What it means, and what to do |
|---|---|
| `rabbitmq_policy.pattern: is required for state: present` (also `apply_to`, `definition`, `priority`) | A task that sets a policy lacks one of its required parts. The task fails without another try, in a dryrun too. Add the input. |
| `rabbitmq_policy.definition: must ...` | `definition` is no mapping, or it is empty. Write at least one policy key with its value. |
| `rabbitmq_policy.state: absent accepts only name and virtual_host; remove INPUT` | A task that removes the policy also describes it. Remove the named input. |
| `the virtual host NAME does not exist; create it with a rabbitmq_virtual_host task in an earlier node task` | The server has no such virtual host. A run fails without another try. A task with `state: absent` is unchanged instead. Put a `rabbitmq_virtual_host` task before this task, or correct the name. |
| `RabbitMQ refuses the policy NAME of the virtual host NAME: Validation failed: ...` | RabbitMQ checked the policy and does not take it; the text behind `Validation failed:` is its own reason. `are not recognised policy settings`: this RabbitMQ does not know a key of `definition`. `apply-to '...' unrecognised`: this RabbitMQ is older than 3.12. `is not a valid ...`: a value has the wrong kind, such as a text for a number. `pattern should be regular expression`: RabbitMQ cannot read `pattern`. The task fails without another try. The policy that was there before stays, and the result still says `changed`: MMX counts every changing command that ran. Correct the input that RabbitMQ names. |
| `the program ... is not installed; install the package rabbitmq-server in an earlier node task` | The node has no RabbitMQ, or has it from another source than a package, so that `/usr/lib/rabbitmq/bin/rabbitmqctl` is missing. Put the tasks of [Set up](#set-up) before this task. |
| `the RabbitMQ server on this node does not answer; start its service in an earlier node task (rabbitmq-diagnostics check_running said: ...)` | The server is stopped or still starting. MMX tries a run again, as every failed node task. Start the server with a `systemd_service` task. When it does not start, read its journal: `journalctl -u rabbitmq-server`. |
| `reading ...`, `setting the policy NAME of the virtual host NAME: ...`, `removing the policy NAME of the virtual host NAME: ...` | A command of `rabbitmqctl` failed. The text behind the colon is RabbitMQ's own message; act on it. MMX tries the task again. |

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


What RabbitMQ accepts in a policy depends on its version. On the test nodes,
RabbitMQ 3.9 and 3.10 refused `classic_queues`, `quorum_queues`, and
`streams` for `apply_to`; RabbitMQ 3 took the keys `ha-mode`, `ha-params`,
and `ha-sync-mode`, and RabbitMQ 4 refused them.

## Why it works this way

- **`priority` is required for `present`.**
  The right value depends on the task;
  [settings with no safe guess are required](../../docs/changing-mmx/design/modules.md#a-setting-with-no-safe-guess-is-required).

- **The module has no list of RabbitMQ's policy keys and versions.**
  RabbitMQ checks a policy when it is set and says what is wrong. The module
  hands the policy over as written and reports RabbitMQ's own message, so
  it works with the keys of a RabbitMQ that is newer than the module.
- **The module compares the stored definition value by value.** RabbitMQ
  gives a definition back as a text with its keys in another order than the
  task wrote them. Comparing the texts would report a change on every run.
- **A whole number is sent without a fraction.** RabbitMQ refuses `1000.0`
  for a key that takes a whole number.
- **The module reads the list of virtual hosts first.** RabbitMQ's list of
  policies fails for a virtual host that does not exist, and the module
  must tell that from other failures: an earlier node task may create the
  virtual host.
- **The module runs the real `rabbitmqctl` program as the account
  `rabbitmq`** (`runuser -u rabbitmq -- /usr/lib/rabbitmq/bin/rabbitmqctl`),
  not the command `rabbitmqctl`. The command of Debian and Ubuntu hands its
  arguments to a shell: on the test nodes a definition with a quote or two
  backslashes came out different, and a command inside an argument ran.
- **It asks first whether the server runs**
  (`rabbitmq-diagnostics check_running`). RabbitMQ's programs end with the
  same exit code for a stopped server and for a refused policy, and a server
  that still starts already answers a ping.
- **A name must not start with a hyphen.** RabbitMQ's programs read such an
  argument as an option. A pattern may start with a hyphen.
