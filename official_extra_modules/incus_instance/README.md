# `incus_instance`

`incus_instance` manages one instance of [Incus](https://linuxcontainers.org/incus/)
on a node: a system container or a virtual machine that Incus runs. It creates
the instance from an image, keeps its profiles, its configuration keys, and
its devices as the task writes them, starts it, shuts it down, or deletes it.
It does all of this through the program `incus`, which it runs as root on the
node, and it manages the Incus server of that node.

[`incus_profile`](../incus_profile/README.md) manages the profiles that
instances share.

## Set up

The module installs nothing. The node tasks below install Incus, start the
Incus server, and prepare the server for the first instance. The setup tasks
work on every distribution in the table. Each `when` chooses the tasks for
one distribution.
The example project [`examples/incus-host`](../../examples/incus-host/) is
this list as a whole project.

| Distribution | Where Incus comes from | Package |
|---|---|---|
| Debian 13 | Debian's own repository | `incus-base`, or `incus` to also run virtual machines |
| Debian 12 | Debian's backports repository | `incus-base` |
| Ubuntu 26.04 | Ubuntu's own repository | `incus-base`, or `incus` to also run virtual machines |
| Ubuntu 24.04 | Ubuntu's own repository | `incus` |
| Rocky Linux 9 and AlmaLinux 10 | The COPR repository `neelc/incus`, which the Incus project names, with EPEL and CRB | `incus` |

```yaml
# Debian 13 packages Incus itself. Debian 12 has it in its backports
# repository, which the keyring of the node's own Debian signs.
- when:
    all:
      - equals: ["{{ .node.distribution.id }}", debian]
      - number_equals: ["{{ .node.distribution.major_version }}", 12]
  file:
    path: /etc/apt/sources.list.d/bookworm-backports.sources
    permissions_mode: "0644"
    content: |
      Types: deb
      URIs: http://deb.debian.org/debian
      Suites: bookworm-backports
      Components: main
      Signed-By: /usr/share/keyrings/debian-archive-keyring.gpg

# incus-base is Incus for containers. The package incus adds what
# virtual machines need. dnsmasq-base serves the addresses of Incus's
# bridge; the package only recommends it, and a server whose apt
# installs no recommended packages would lack it.
- when:
    any:
      - equals: ["{{ .node.distribution.id }}", debian]
      - all:
          - equals: ["{{ .node.distribution.id }}", ubuntu]
          - number_at_least: ["{{ .node.distribution.major_version }}", 26]
  apt_package: {packages: [{name: incus-base, version: newest_when_first_installed}, {name: dnsmasq-base, version: newest_when_first_installed}]}

# Ubuntu 24.04 has no package incus-base. Its package incus brings Incus
# without QEMU.
- when:
    all:
      - equals: ["{{ .node.distribution.id }}", ubuntu]
      - number_equals: ["{{ .node.distribution.major_version }}", 24]
  apt_package: {packages: [{name: incus, version: newest_when_first_installed}, {name: dnsmasq-base, version: newest_when_first_installed}]}

# Rocky Linux and AlmaLinux have no Incus package. The Incus project
# names the COPR repository neelc/incus for them. Its packages need
# libraries from EPEL, and EPEL needs the repository CRB. The COPR
# repository has one directory for release 9 and one for 10.
- when:
    any:
      - equals: ["{{ .node.distribution.id }}", rocky]
      - equals: ["{{ .node.distribution.id }}", almalinux]
  task_block:
    id: incus_from_the_copr_repository
    changes:
      - dnf_package: {packages: [{name: epel-release, version: newest_when_first_installed}]}
      - dnf_repository:
          existing_repository: crb
          enabled: true
      - when: {number_equals: ["{{ .node.distribution.major_version }}", 9]}
        dnf_repository:
          name: copr-neelc-incus
          description: Incus for Enterprise Linux 9 (COPR neelc/incus)
          baseurl: ["https://download.copr.fedorainfracloud.org/results/neelc/incus/epel-9-$basearch/"]
          signing_key_url: https://download.copr.fedorainfracloud.org/results/neelc/incus/pubkey.gpg
          signing_key_sha256: 635810d20f5705f696a0c0ee0aa6e6ac9faeffeb55bb8e7e20b3d9014f930ce9
      - when: {number_equals: ["{{ .node.distribution.major_version }}", 10]}
        dnf_repository:
          name: copr-neelc-incus
          description: Incus for Enterprise Linux 10 (COPR neelc/incus)
          baseurl: ["https://download.copr.fedorainfracloud.org/results/neelc/incus/rhel+epel-10-$basearch/"]
          signing_key_url: https://download.copr.fedorainfracloud.org/results/neelc/incus/pubkey.gpg
          signing_key_sha256: 635810d20f5705f696a0c0ee0aa6e6ac9faeffeb55bb8e7e20b3d9014f930ce9
      - dnf_package: {packages: [{name: incus, version: newest_when_first_installed}]}

# The socket starts the Incus server at the first incus command. MMX
# installs a package without starting its services, and the packages of
# the Red Hat family enable nothing.
- systemd_service:
    name: incus.socket
    state: running
    start_at_boot: true

# Start the server when the node boots, so that an instance with
# boot.autostart: "true" starts then. This unit starts the server only
# when an instance needs it.
- systemd_service:
    name: incus-startup.service
    state: do_not_start_or_stop
    start_at_boot: true

# A container's accounts are a range of the node's user and group IDs
# that belongs to root. Debian's and Ubuntu's packages write this
# range; the others leave it out, and Incus then makes no container.
# The server reads the files only when it starts, so it is restarted
# when a line changes.
- task_block:
    id: incus_server
    changes:
      - line:
          insert_at_end_of_file: true
          path: /etc/subuid
          match: '^root:'
          line: "root:1000000:1000000000"
      - line:
          insert_at_end_of_file: true
          path: /etc/subgid
          match: '^root:'
          line: "root:1000000:1000000000"
    activate:
      - systemd_service:
          name: incus.service
          state: running

# Once for each server: a storage pool, the bridge incusbr0, and the
# root disk and the network card in the profile default. The guard
# asks for the root disk, the last thing that the command makes. Its
# exit code 127 says that incus is not installed yet, as in the dryrun
# of a new node.
- shell:
    script: incus admin init --minimal
    skip_if_this_command_succeeds:
      script: incus profile device get default root pool
      true_exit_codes: [0]
      false_exit_codes: [1, 127]
```

In order, on the node:

1. A package task installs Incus from the source of the node's distribution.
2. `incus.socket` is started and enabled. The socket starts the Incus server
   at the first `incus` command.
3. `incus-startup.service` is enabled to start the Incus server when the
   node boots. Without `incus-startup.service`, an instance with
   `boot.autostart: "true"` would start only after the first `incus` command.
4. Root gets a range of user IDs and group IDs for containers, and the
   server is started. When a line changed, the server is restarted, because
   it reads the two files only at its start.
5. `incus admin init --minimal` makes a storage pool named `default`, the
   bridge `incusbr0`, and the root disk and the network card of the profile
   `default`. It runs once: afterwards its guard finds the root disk.

**A node whose apt must install no recommended packages.** apt also installs
the packages that a package only recommends, unless the node's apt settings
say otherwise. To say otherwise, write this file before the package tasks:

```yaml
- file:
    path: /etc/apt/apt.conf.d/90-install-no-recommended-packages
    permissions_mode: "0644"
    content: |
      APT::Install-Recommends "false";
```

The list above names `dnsmasq-base`, which Incus needs and which its package
only recommends, so it works with and without this file. What the file
leaves out:

| Distribution | Packages that are left out |
|---|---|
| Debian 13, Ubuntu 26.04 | `apparmor`, where the node does not have it already. |
| Ubuntu 24.04 | `apparmor`, where the node does not have it already, and `incus-agent`: the program that Incus puts into its virtual machine instances. |

Write the file on Ubuntu 24.04 when the node is itself a virtual machine of
another Incus. There the package `incus-agent` replaces the unit file
`/usr/lib/systemd/system/incus-agent.service`, through which the other Incus
reaches the node, and removing the package deletes that file.

**Virtual machine instances** need QEMU on the node, and a node that can run
virtual machines (`/dev/kvm`). On Debian 13 and Ubuntu 26.04, the package
`incus` brings QEMU in place of `incus-base`; restart `incus.service`
after you install it. Where apt installs no recommended packages, also
install what QEMU only recommends and Incus needs: `ovmf`, the firmware
that a virtual machine starts with; `qemu-utils`, whose `qemu-img` Incus
uses for a virtual machine's image; and `qemu-system-modules-spice`, for
the console that Incus gives every virtual machine. The package of the COPR
repository for the Red Hat family brings no QEMU program that Incus finds,
and Ubuntu 24.04's package brings none.

## Inputs

| Input | Meaning | When omitted |
|---|---|---|
| `name` | The instance's name in Incus, such as `shop-test`. | Required. |
| `state` | `running`: the instance exists and runs. `stopped`: it exists and is shut down. `deleted_with_all_its_data`: it is gone, with its root disk and its snapshots. | Required. |
| `image_when_created` | The image that a new instance is created from. An image of another server has that server's name in front, such as `images:debian/13`. A name without a colon is an image of the node's own Incus, by its alias or by its fingerprint; Incus must have that image before the task runs. Used only when the instance does not exist yet. | A task that would create the instance fails. |
| `instance_type` | `container` or `virtual_machine`. Incus creates an instance of the type of its image, so an image of the other type fails the task, and so does an existing instance of the other type. | Required with `running` and `stopped`. |
| `profiles` | The instance's profiles, in order. Replaces the whole ordered list, because a later profile overrides an earlier one. An empty list means no profile. | Required with `running` and `stopped`. |
| `config` | Incus configuration keys with their values as text, such as `limits.cpu: "2"`. Comes with `config_keys_not_listed`. | The instance's keys are left alone. |
| `config_keys_not_listed` | `remove`: `config` is the instance's whole configuration, and every other key of the instance is removed. `keep`: the keys in `config` are set, and every other key stays. Incus's own `volatile.*` and `image.*` keys are never removed. | Required with `config`. |
| `devices` | The instance's own devices by name. Each device is a mapping of Incus device options with their values as text. Comes with `devices_not_listed`. Replaces each listed device's whole definition, also with `devices_not_listed: keep`. | The instance's devices are left alone. |
| `devices_not_listed` | `remove`: `devices` holds all of the instance's own devices, and every other own device is removed. `keep`: the listed devices are set, and every other own device stays. A device that a profile gives the instance is not the instance's own and is left alone. | Required with `devices`. |
| `wait_for_shutdown_at_most` | How long `state: stopped` waits for shutdown. A positive duration of whole seconds, such as `30s` or `2m`. Incus takes a timeout in whole seconds. | `2m`. |
| `power_off_when_shutdown_takes_longer` | What happens when `wait_for_shutdown_at_most` expires and the instance still runs. `true`: the module stops the instance at once (`incus stop --force`), as if its power were cut. `false`: the task fails. | `false`. |
| `incus_project` | The Incus project that holds the instance. An Incus project is a group of instances and profiles inside one Incus server; it is not the MMX project. A project that does not exist is a missing prerequisite. | Incus's project `default`. |

- **Every value is text.** Incus stores each configuration value and each
  device option as text. Write a number or `true` in quotes: `limits.cpu:
  "2"`, `readonly: "true"`.
- **A key or an option cannot have an empty value.** Incus stores none. To
  remove a key, leave it out and write `config_keys_not_listed: remove`.
- **`image_when_created` and `incus_project` do not start with a hyphen.** The program
  `incus` would read such a value as one of its options.
- **`deleted_with_all_its_data` accepts only `name` and `incus_project`.**
- **An existing instance is never created again.** `image_when_created` says what a new
  instance is created from. An instance that exists keeps its image, whatever
  the task names.

**The module creates no Incus project.** An earlier node task creates the
project of `incus_project`, for example:

```yaml
- shell:
    script: incus project create staging
    skip_if_this_command_succeeds: incus project show staging
```

## Outputs

| Output | Meaning |
|---|---|
| `created` | `true` when this task created the instance. In a dryrun: `true` when a run would create it. |

A later node task reads the `created` output as `.node_tasks.<id>.created`.

## What a dryrun reports

A dryrun reads with `incus query` and runs no changing command.

- **For a missing instance:** `would_change`, with `would create the
  container shop-test from the image images:debian/13; would start it`.
- **For an instance whose profiles, keys, or devices differ:**
  `would_change`, with each difference, such as `would set the profiles to
  default, web; would set the configuration key limits.cpu; would remove
  the device old-disk`. The reason names keys and devices and never a
  value.
- **For another state:** `would start the instance shop-test`, `would
  resume the frozen instance shop-test`, `would shut down the instance
  shop-test`, or `would delete the instance shop-test with all its data`.
- **`not_checked`** while the program `incus` is not installed, while the
  Incus server does not answer, while the Incus project of `incus_project`
  does not exist, and while the node's Incus has no image of the name in
  `image_when_created`. An earlier node task may provide each of them.
- **It fails as a run does** when the inputs break a rule, when the image
  or the instance has the other type, and when `state: stopped` meets a
  frozen or an ephemeral instance.
- **It does not ask Incus whether Incus accepts a change.** Incus checks a
  key, a value, and a device only when it writes them. So a dryrun can
  report `would_change` for a task whose run then fails with Incus's
  message: an unknown key, an image that another server does not have, a
  device whose port is in use, an instance that is protected against
  deletion, or an instance that does not shut down in time.
- **What its reads leave on the node:** the first `incus` command of the
  account root writes root's client settings to `/root/.config/incus/`. On
  Debian and Ubuntu, a read starts a stopped Incus server through
  `incus.socket`.

## Examples

A Debian 13 container that runs, with two CPUs and 4 GiB of memory. Port
8080 of the node reaches port 80 inside it. The task's keys and devices are
all the instance has of its own:

```yaml
- incus_instance:
    name: shop-test
    state: running
    image_when_created: images:debian/13
    profiles: [default]
    config:
      limits.cpu: "2"
      limits.memory: 4GiB
      boot.autostart: "true"
    config_keys_not_listed: remove
    devices:
      web:
        type: proxy
        listen: tcp:0.0.0.0:8080
        connect: tcp:127.0.0.1:80
    devices_not_listed: remove
    instance_type: container
```

Set one key of a running container whose complete ordered profile list is
`[default]`, keeping its other configuration keys and devices.
`profiles` must list every profile already assigned to the instance, in order;
copying `[default]` onto an instance with other profiles removes those profiles:

```yaml
- incus_instance:
    name: shop-test
    state: running
    config:
      limits.memory: 8GiB
    config_keys_not_listed: keep
    instance_type: container
    profiles: [default]
```

Shut an instance down, wait up to a minute, and stop it at once when it has
not shut down by then; then delete another instance with its data:

```yaml
- incus_instance:
    name: shop-test
    state: stopped
    wait_for_shutdown_at_most: 60s
    power_off_when_shutdown_takes_longer: true

    instance_type: container
    profiles: [default]
- incus_instance:
    name: shop-old
    state: deleted_with_all_its_data
```

## Errors and what they mean

When an MMX reason includes `Error:`, the text after `Error:` is Incus's
own error message. Incus words its messages differently in each version.

MMX tries a failed task three times. A task below that "fails once" is not
tried again: either another try would end the same way, or another try
would find what the first try left and report success, and the failure
would be lost.

| The reason holds | What it means, and what to do |
|---|---|
| `the program incus is not installed` | The node has no Incus. A dryrun reports the task `not_checked`; a run fails once. Install the package in an earlier node task; see [Set up](#set-up). |
| `the Incus server on this node does not answer` | The program is there, and the server is not started or does not answer. A dryrun reports `not_checked`; a run is tried again. Start `incus.socket` in an earlier node task. |
| `the Incus project ... does not exist` | `incus_project` names a project that Incus does not have. A dryrun reports `not_checked`; a run fails once. Create the project in an earlier node task; [Inputs](#inputs) shows one. |
| `the Incus server on this node has no image ...` | `image_when_created` names an image of the node's own Incus, and Incus has none of that alias or fingerprint. A dryrun reports `not_checked`; a run fails once. Import the image in an earlier node task, or name an image of another server, such as `images:debian/13`. |
| `incus_instance.image_when_created: is required to create the instance ...` | The instance does not exist, and the task names no image. Write `image_when_created`. |
| `incus_instance.instance_type: ... the image ... is the image of a ...` | The image has the other type than the task. Nothing was created. Write the `instance_type` of the image, or name another image. |
| `incus_instance.instance_type: ... created an instance of the type ...; the task deleted the new instance again` | An image of another server has the other type than the task. The module saw that only after Incus created the instance, and deleted the instance, which never ran. Write the `instance_type` of the image. When the reason ends with `deleting the new instance again failed`, the instance is still there, and the task reports `created: true`. |
| `incus_instance.instance_type: ... Incus cannot change the type of an instance` | The instance exists with the other type. Write its type, or delete it first with `deleted_with_all_its_data`. |
| `incus_instance.state: the instance ... is frozen` | `state: stopped` met an instance that `incus pause` froze. Incus does not shut such an instance down. Run `incus resume NAME`, or a task with `state: running`, first. |
| `incus_instance.state: the instance ... is ephemeral` | Incus deletes an ephemeral instance when it stops, so `stopped` would destroy it. To delete it, write `state: deleted_with_all_its_data`. |
| `shutting down the instance ..., with a wait of N seconds` | The instance did not shut down within the wait. The reason ends with its status. The task fails once: another try would wait as long again. Give the instance more `wait_for_shutdown_at_most`, or write `power_off_when_shutdown_takes_longer: true`. |
| `creating the instance ... from the image ...: Error: ...` | Incus refused to create the instance: an image that the other server does not have, a wrong key or device, or no profile that gives a root disk. No instance is left. Correct what Incus's message names. |
| `created the instance ..., and starting it failed` | The instance exists and did not start, for example because a proxy device wants a port that is in use. The reason ends with the status that the instance has afterwards. The task fails once, with `created: true`. Correct the cause; the next run writes what differs and starts the instance. |
| `starting the instance ...: Error: ...` | An instance that existed before did not start. The reason ends with the status that the instance has afterwards. While it is `Stopped`, MMX tries the start again; when it is `Running`, the task fails once. Correct what Incus's message names. |
| `writing the configuration of the instance ...: Error: ...` | Incus refused the new configuration: an unknown key, a bad value, a key that a running virtual machine cannot take, or a new device that cannot start, such as a proxy device whose port is in use. Incus stored none of it, and the task fails once. The reason ends with the status that the instance has now. Correct what Incus's message names; for a running virtual machine, shut the instance down first. |
| `deleting the instance ...: Error: ...` | Incus refused, usually because the key `security.protection.delete` protects the instance. Set that key to `"false"` first, in the instance or in the profile that gives it. |
| `has the status ..., and this module acts only on the statuses Stopped, Running, and Frozen` | Incus is still starting or stopping the instance, or the instance is in an error state. The task is tried again; `incus info NAME` shows the instance. |
| `reading ... from the Incus server on this node: ... (error code N)` | The server answered a read with an error, such as `Invalid instance name`. Correct what the server's message names. |
| `config_keys_not_listed: is required with config`, `devices_not_listed: is required with devices` | A task that gives a list says what happens to the rest. Write the setting: `remove` or `keep`. |
| `state: deleted_with_all_its_data accepts only name and incus_project; remove ...` | The state takes no other input. Remove the input that the message names. |
| `has an empty value` | Incus stores no key and no device option without a value. Leave the key or the option out. |
| `must not be empty`, `must not start with a hyphen` | A name is empty, or the program `incus` would read it as one of its options. Correct the input that the message names. |

**A task that Incus refuses reports `changed`.** MMX reports `changed` for
every changing command that ran, also for one that Incus refused: MMX
cannot see whether a failed command changed something. A refused `incus
config edit` stores nothing, and its task still reports `failed` and
`changed`.

**A value that Incus refuses appears in the reason.** Incus puts a refused
value into its message, such as `Invalid value: ...`. When a secret of the
project is in `config`, MMX hides the whole result of the task, and the
reason with it.

## Tested on

**Distributions:** Debian 12 and 13, Ubuntu 24.04 and 26.04, Rocky Linux 9, AlmaLinux 10.

The suite `tests/integration/incus_instances.py` runs `incus_instance` and
`incus_profile` through `mmx` against a real Incus server, on x86_64 virtual
machines.

| Distribution | Incus | A virtual machine instance in the suite |
|---|---|---|
| Debian 13 | 6.0.4 | no: the package `incus-base` runs containers only |
| Debian 12 | 6.0.4, from backports | no |
| Ubuntu 26.04 | 6.0.5 | yes: after the example's `incus-base`, the suite installs `incus` and the three packages above |
| Ubuntu 24.04 | 6.0.0 | no: the package brings no QEMU |
| Rocky Linux 9 | 6.23, from the COPR repository | no: the package brings no QEMU program that Incus finds |
| AlmaLinux 10 | 6.23, from the COPR repository | no |

The COPR repository is the work of one packager; the Incus project names it
and does not maintain it.

## Why it works this way

- **A listed device is one whole definition.** Incus validates devices as
  complete definitions; `devices_not_listed` controls only unnamed devices.

- **Type and profiles are required.** These choices depend on the instance;
  [settings with no safe guess are required](../../docs/changing-mmx/design/modules.md#a-setting-with-no-safe-guess-is-required).
- **The image applies only at creation.** `image_when_created` follows the
  [creation-only naming rule](../../docs/changing-mmx/design/modules.md#a-setting-used-only-at-creation-says-so-in-its-name).
- **The shutdown wait is a duration.** `wait_for_shutdown_at_most: 2m` reads
  like the time limits of core modules. Whole seconds match Incus's timeout.

- **Every name has the prefix `local:`, and every command names its
  project.** Root's own client settings, in `/root/.config/incus/`, can name
  another default server (`incus remote switch`) and another default project
  (`incus project switch`). A task must manage the Incus of its node and the
  project that the task names.
- **An alias in root's client settings still applies.** Incus has no option
  that switches the aliases of that file off. An alias named `start` there
  would replace the module's `incus start`.
- **Every read is `incus query --raw`.** It prints JSON, and a code in it
  says whether the instance exists. Incus's error texts differ between its
  versions, so the module decides nothing from a text.
- **The module writes the whole configuration back.** `incus config edit`
  takes a whole document and removes what the document leaves out, also
  Incus's own keys, the description, and the profiles. So the module reads
  the instance right before the write, changes the profiles, the keys, and
  the devices that the task names, and writes all of it.
- **A configuration value travels through standard input only.** The
  commands that MMX records hold no value, and a reason names keys and
  devices, never a value.
- **The module never restarts an instance.** A restart ends what runs
  inside. Incus applies most keys to a running instance at once. A running
  container stores a key that needs a restart without a message and applies
  it at its next start; the test nodes showed this for `environment.*`,
  `security.privileged`, `security.nesting`, `security.idmap.isolated`, and
  `raw.lxc`. A running virtual machine refuses such a key.
- **`stopped` waits a bounded time.** Incus's own stop waits without end for
  an instance that ignores the request; a container gets the request as the
  signal SIGPWR. A task must end, so the module passes the wait to Incus.
  Cutting the power can lose data, so it is the project's choice. A wait
  that ends fails the task once: MMX's next try would wait as long again.
- **`stopped` refuses an ephemeral and a frozen instance.** Incus deletes an
  ephemeral instance when it stops, and a state that destroys data is always
  spelled out. Incus's stop of a frozen instance did not end on the test
  nodes.
- **`profiles` is the whole list.** The order of an instance's profiles
  decides which value wins, so a list that keeps other profiles would have
  no defined order.
- **The module checks the type.** Without its option for a virtual machine,
  Incus creates an instance of the type of its image and says nothing. A task
  that asks for a container must not get a virtual machine.
- **A change that Incus refuses can still change the instance.** On a test
  node, a memory limit that was too small for the running container was
  refused, and the kernel had ended the container by then. So a failed
  change's reason says which status the instance has afterwards.
- **A stop that fails is judged by the instance.** After a failed stop, the
  module reads the instance: when it is stopped, the task has what it asked
  for.
- **A failure after the instance was created fails the task once.** MMX
  tries a failed task again, and that try would find the new instance and
  report `created: false` for an instance that this task created. An
  existing instance that runs after a failed start has one try for the
  same reason: the next try would find it running and report success.
- **A document that Incus refuses fails the task once.** Another try sends
  the same document and gets the same answer.
- **An image of the node's own Incus must be there before the task.** The
  module asks Incus for the image by its alias and then by its fingerprint.
  An image that Incus does not have is a missing prerequisite, because an
  earlier node task may import it: a dryrun is not checked, and a run fails
  once.
