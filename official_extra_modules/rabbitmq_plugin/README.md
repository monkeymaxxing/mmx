# `rabbitmq_plugin`

`rabbitmq_plugin` manages whether one plugin of RabbitMQ is enabled.
RabbitMQ is a message broker: a server that takes messages from programs and
hands them to other programs through queues. A **plugin** adds a feature to
the server, such as the web interface (`rabbitmq_management`) or the MQTT
protocol (`rabbitmq_mqtt`). RabbitMQ's package brings about forty plugins,
all switched off. The module runs RabbitMQ's own program `rabbitmq-plugins`
as root on the **node** of the server; a node is a Linux server that MMX
manages. The module works while the RabbitMQ server runs and while the
RabbitMQ server is stopped.

Each node of a RabbitMQ cluster has its own plugins, so run `rabbitmq_plugin`
node tasks on every node of the cluster.

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

The `systemd_service` node task starts RabbitMQ for the first time. During
installation of RabbitMQ's package, MMX prevents the package from starting
the RabbitMQ service.

## Inputs

| Input | Meaning | When omitted |
|---|---|---|
| `name` | The plugin's name as the command `rabbitmq-plugins list` shows it, such as `rabbitmq_management`. | Required. |
| `state` | `enabled` or `disabled_with_plugins_that_depend_on_it`. | Required. |

RabbitMQ keeps the names of the enabled plugins in the file
`/etc/rabbitmq/enabled_plugins`. A plugin can be on in two ways: because the
file names it, or only because an enabled plugin needs it. For example,
`rabbitmq_management` needs `rabbitmq_management_agent` and
`rabbitmq_web_dispatch`, and RabbitMQ turns them on with it and off with it.

- **`enabled`** puts the plugin's name into the file. RabbitMQ also turns on
  the plugins that it needs. A plugin that is on only because another plugin
  needs it gets its own name into the file, so it stays on when the other
  plugin goes.
- **`disabled_with_plugins_that_depend_on_it`** turns the plugin off. RabbitMQ also turns off every enabled
  plugin that needs it, and the task's `reason` names them. A plugin that is
  off is unchanged.
- **While the server runs,** a plugin starts or stops at once. **While the
  server is stopped,** the task changes the file, and the change holds from
  the server's next start; the `reason` says so.

## Outputs

The module has no named outputs. The task's `reason` says what it did, such
as `enabled the plugin rabbitmq_management`, or `disabled the plugin
rabbitmq_management; RabbitMQ also disabled the plugins that need it:
rabbitmq_shovel_management`.

## What a dryrun reports

A dryrun reads RabbitMQ's list of plugins, which also works while the
server is stopped, and changes no plugin.

- **A plugin to enable or disable:** `would_change`, with the reason, such
  as `would enable the plugin rabbitmq_management`. For `disabled_with_plugins_that_depend_on_it` the
  reason also names the enabled plugins that need this one and would go
  with it.
- **`not_checked`** while RabbitMQ's programs are not installed, and for a
  name that RabbitMQ does not have: an earlier node task of the same run
  may install the package or the plugin's file.
- **A failure, as in a run,** for a name that is empty or starts with a
  hyphen.
- **What the read leaves on the node:** nothing while the server runs.
  While it does not run, on a node with the packages of the RabbitMQ
  project, `rabbitmq-plugins` starts a process named `epmd` that belongs
  to root, and that process stays. `epmd` is the small program
  that tells RabbitMQ's programs on which port the server listens for them;
  the server uses it at its next start. On a server that has never run,
  `rabbitmq-plugins` also creates the file
  `/var/lib/rabbitmq/.erlang.cookie`, the key with which RabbitMQ's programs
  reach the server; the server's first start would create it.

## Examples

Turn on RabbitMQ's web interface and HTTP API, which listen on port 15672:

```yaml
- rabbitmq_plugin:
    name: rabbitmq_management
    state: enabled
```

Turn off the MQTT plugin:

```yaml
- rabbitmq_plugin:
    name: rabbitmq_mqtt
    state: disabled_with_plugins_that_depend_on_it
```

## Errors and what they mean

Every reason starts with `rabbitmq_plugin`.

| The reason holds | What it means, and what to do |
|---|---|
| `RabbitMQ on this node has no plugin NAME; the command rabbitmq-plugins list shows the names. ...` | RabbitMQ's list of plugins does not have the name. A run fails without another try, and nothing was changed. Correct the name. For a plugin that is not part of RabbitMQ, put its file into RabbitMQ's plugins directory in an earlier node task. |
| `rabbitmq_plugin.name: must not ...` | The name is empty or starts with a hyphen. Correct `name`. |
| `the program rabbitmq-plugins is not installed; install the package rabbitmq-server in an earlier node task` | The node has no RabbitMQ. A run fails without another try. Put the package task of [Set up](#set-up) before this task. |
| `enabling the plugin NAME: ...`, `disabling the plugin NAME: ...` | `rabbitmq-plugins` failed, and the text behind the colon is its own message; or it reported no error and the plugin is as before, which the reason then says. MMX tries the task again, and the new try runs the command again. Act on RabbitMQ's message. |
| `enabled the plugin NAME, and then reading the plugins failed: ...`, `disabled the plugin NAME, and then reading the plugins failed: ...` | The command worked, and the read after it failed, so the task cannot say what RabbitMQ did. MMX does not try this task again: a new try would find the plugin as the task wants it and report an unchanged task, and the failed read would go unseen. Run the playbook again; the task then reports unchanged when the plugin is as it should be. |
| `reading the plugins: ...` | `rabbitmq-plugins list` failed before any change, or printed something that the module does not know. Act on RabbitMQ's message. |

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

- **A name that RabbitMQ does not have fails the task before any command.**
  On the test nodes, RabbitMQ 4.3 accepted any name without an
  error and wrote it into the file of enabled plugins.
- **For a plugin that is on only because another needs it, the module
  writes the whole list of enabled plugins** (`rabbitmq-plugins set`), with
  the names that it has just read and this plugin. RabbitMQ's `enable`
  reports success for such a plugin and changes nothing.
- **The module reads the list of plugins again after a change** and fails
  when the plugin is not as the task wants it: what `rabbitmq-plugins`
  prints is text for people, and its exit code is 0 also when it changed
  nothing. When that second read itself fails, the task fails once and is
  not tried again: a new try would find the plugin changed and report an
  unchanged task.
- **`disabled_with_plugins_that_depend_on_it` does what RabbitMQ does:** it also turns off the enabled
  plugins that need this one. A plugin cannot run without a plugin that it
  needs, so the only other choice would be to refuse the task.
- **The module asks no server.** The file of enabled plugins is what holds
  from start to start, and `rabbitmq-plugins` changes it with the server
  running or stopped.
