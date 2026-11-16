# `incus_profile`

`incus_profile` manages one profile of [Incus](https://linuxcontainers.org/incus/)
on a node. A profile is a named set of configuration keys and devices that
Incus gives to every instance that lists the profile, such as a memory limit
or a network card. The module creates the profile, keeps its description, its
keys, and its devices as the task writes them, or removes it. It does this
through the program `incus`, which it runs as root on the node, and it
manages the Incus server of that node.

[`incus_instance`](../incus_instance/README.md) manages the instances, which
name their profiles in its input `profiles`.

## Set up

The module installs nothing. These node tasks install Incus, start its
server, and make the server ready for its first instance. They work on every
distribution of the table; each `when` chooses the tasks of one distribution.
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

## Inputs

| Input | Meaning | When omitted |
|---|---|---|
| `name` | The profile's name in Incus, such as `web`. | Required. |
| `state` | `present`: the profile exists with what the task gives. `absent`: the profile is removed. | `present`. |
| `description` | The text that Incus shows beside the profile. | A new profile has none; an existing one keeps its text. |
| `config` | Incus configuration keys with their values as text, such as `limits.cpu: "2"`. You must also supply `config_keys_not_listed`. | The profile's keys are left alone. |
| `config_keys_not_listed` | `remove`: `config` is the profile's whole configuration, and every other key is removed. `keep`: the keys in `config` are set, and every other key stays. | Required with `config`. |
| `devices` | The profile's devices by name. Each device is a mapping of Incus device options with their values as text. You must also supply `devices_not_listed`. Replaces each listed device's whole definition, also with `devices_not_listed: keep`. | The profile's devices are left alone. |
| `devices_not_listed` | `remove`: `devices` holds all of the profile's devices, and every other device is removed. `keep`: the listed devices are set, and every other device stays. | Required with `devices`. |
| `incus_project` | The Incus project that holds the profile. An Incus project is a group of instances and profiles inside one Incus server; it is not the MMX project. A project that does not exist is a missing prerequisite. | Incus's project `default`. |

- **Every value is text.** Incus stores each configuration value and each
  device option as text. Write a number or `true` in quotes: `limits.cpu:
  "2"`, `readonly: "true"`.
- **A key or an option cannot have an empty value.** Incus stores none. To
  remove a key, leave it out and write `config_keys_not_listed: remove`.
- **`incus_project` does not start with a hyphen.** The program `incus`
  would read such a value as one of its options.
- **`absent` accepts only `name` and `incus_project`.**
- **Incus applies a changed profile at once** to every instance that lists
  it, also to a running one.
- **A project can use the profiles of the project `default`** (its Incus
  setting `features.profiles` is `false`). Such a project has no profile of
  its own, and a task that names it in `incus_project` fails: write the task
  without `incus_project`.

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
| `created` | `true` when this task created the profile. In a dryrun: `true` when a run would create it. |

A later node task reads the `created` output as `.node_tasks.<id>.created`.

## What a dryrun reports

A dryrun reads with `incus query` and runs no changing command.

- **For a missing profile:** `would_change`, with `would create the profile
  web; would set the description; would set the configuration key
  limits.cpu; would set the device eth0`.
- **For a profile whose description, keys, or devices differ:**
  `would_change`, with each difference, such as `would set the
  configuration key limits.cpu; would remove the device old-disk`. The
  reason names keys and devices and never a value.
- **For `state: absent`:** `would remove the profile web`.
- **`not_checked`** while the program `incus` is not installed, while the
  Incus server does not answer, and while the Incus project of
  `incus_project` does not exist. An earlier node task may provide each of
  them.
- **It fails as a run does** when the inputs break a rule and when
  `incus_project` names a project without profiles of its own.
- **It does not ask Incus whether Incus accepts a change.** Incus checks a
  key, a value, and a device only when it writes them, and it refuses to
  delete a profile that an instance uses and the profile `default`. So a
  dryrun can report `would_change` for a task whose run then fails with
  Incus's message.
- **What its reads leave on the node:** the first `incus` command of the
  account root writes root's client settings to `/root/.config/incus/`. On
  Debian and Ubuntu, a read starts a stopped Incus server through
  `incus.socket`.

## Examples

A profile that limits each of its instances to two CPUs and connects them to
the bridge `incusbr0`. The task's keys and devices are all the profile has:

```yaml
- incus_profile:
    name: web
    description: Limits and the bridge for web containers
    config:
      limits.cpu: "2"
    config_keys_not_listed: remove
    devices:
      eth0:
        type: nic
        network: incusbr0
    devices_not_listed: remove
```

Set one key of Incus's profile `default` and leave its other keys and its
devices alone:

```yaml
- incus_profile:
    name: default
    config:
      boot.autostart: "true"
    config_keys_not_listed: keep
```

Remove a profile of the Incus project `staging`, after the instance that
used it:

```yaml
- incus_instance:
    name: shop-test
    state: deleted_with_all_its_data
    incus_project: staging

- incus_profile:
    name: web
    state: absent
    incus_project: staging
```

## Errors and what they mean

A reason that holds a text after `Error:` holds Incus's own message. Incus
words its messages differently in each of its versions.

MMX tries a failed task three times. A task below that "fails once" is not
tried again: either another try would end the same way, or another try
would find what the first try left and report success, and the failure
would be lost.

| The reason holds | What it means, and what to do |
|---|---|
| `the program incus is not installed` | The node has no Incus. A dryrun reports the task `not_checked`; a run fails once. Install the package in an earlier node task; see [Set up](#set-up). |
| `the Incus server on this node does not answer` | The program is there, and the server is not started or does not answer. A dryrun reports `not_checked`; a run is tried again. Start `incus.socket` in an earlier node task. |
| `the Incus project ... does not exist` | `incus_project` names a project that Incus does not have. A dryrun reports `not_checked`; a run fails once. Create the project in an earlier node task; [Inputs](#inputs) shows one. |
| `incus_profile.incus_project: the Incus project ... uses the profiles of the project default` | The project has no profile of its own. Incus would change the profile of the project `default` under this project's name. Write the task without `incus_project`. |
| `creating the profile ...: Error: ...` | Incus refused the name, for example a name with a slash. Choose a name that Incus accepts. |
| `writing the new profile ...: Error: ...; the task removed the new profile again` | Incus refused a key, a value, or a device of a profile that this task had just created. The task removed the profile, so nothing is left, and it fails once. Correct what Incus's message names. When the reason ends with `removing the new profile again failed`, the empty profile is still there, and the task reports `created: true`. |
| `writing the profile ...: Error: ...` | Incus refused a key, a value, or a device. The profile is as it was, and the task fails once. Correct what Incus's message names. |
| `Incus saved the profile all the same` | Incus wrote the profile, and an instance that uses it could not take the change, for example a device whose port is in use. Incus's own message says "profile change still saved". The profile is as the task writes it, so the next run finds nothing to write, while that instance still lacks the change. The task fails once. Correct the instance that Incus's message names; Incus applies the profile to it when the instance starts again. |
| `removing the profile ...: Error: ...` | Incus refused: an instance still uses the profile, or it is the profile `default`, which Incus never deletes. Take the profile out of its instances first, or delete those instances in earlier node tasks. |
| `reading ... from the Incus server on this node: ... (error code N)` | The server answered a read with an error. Correct what the server's message names. |
| `config_keys_not_listed: is required with config`, `devices_not_listed: is required with devices` | A task that gives a list says what happens to the rest. Write the setting: `remove` or `keep`. |
| `state: absent accepts only name and incus_project; remove ...` | The state takes no other input. Remove the input that the message names. |
| `has an empty value` | Incus stores no key and no device option without a value. Leave the key or the option out. |
| `must not be empty`, `must not start with a hyphen` | A name is empty, or the program `incus` would read it as one of its options. Correct the input that the message names. |

**A task that Incus refuses reports `changed`.** MMX reports `changed` for
every changing command that ran, also for one that Incus refused: MMX
cannot see whether a failed command changed something. A new profile that
Incus refuses is created and removed again, and its task reports `failed`
and `changed`.

**A value that Incus refuses appears in the reason.** Incus puts a refused
value into its message, such as `Invalid value: ...`. When a secret of the
project is in `config`, MMX hides the whole result of the task, and the
reason with it.

## Tested on

**Distributions:** Debian 12 and 13, Ubuntu 24.04 and 26.04, Rocky Linux 9, AlmaLinux 10.

The suite `tests/integration/incus_instances.py` runs `incus_instance` and
`incus_profile` through `mmx` against a real Incus server, on x86_64 virtual
machines.

| Distribution | Incus |
|---|---|
| Debian 13 | 6.0.4 |
| Debian 12 | 6.0.4, from backports |
| Ubuntu 26.04 | 6.0.5 |
| Ubuntu 24.04 | 6.0.0 |
| Rocky Linux 9 | 6.23, from the COPR repository |
| AlmaLinux 10 | 6.23, from the COPR repository |

The COPR repository is the work of one packager; the Incus project names it
and does not maintain it.

## Why it works this way

- **A listed device is one whole definition.** Incus validates devices as
  complete definitions; `devices_not_listed` controls only unnamed devices.

- **Every name has the prefix `local:`, and every command names its
  project.** Root's own client settings, in `/root/.config/incus/`, can name
  another default server (`incus remote switch`) and another default project
  (`incus project switch`). A task must manage the Incus of its node and the
  project that the task names.
- **An alias in root's client settings still applies.** Incus has no option
  that switches the aliases of that file off. An alias named `profile` there
  would replace the module's `incus profile` commands.
- **Every read is `incus query --raw`.** The command prints JSON, and a code
  in the JSON says whether the profile exists. Incus's error texts differ
  between versions, so the module decides nothing from a text.
- **The module writes the whole profile back.** `incus profile edit` takes a
  whole document and removes what the document leaves out. So the module
  reads the profile right before the write, changes the description, the
  keys, and the devices that the task names, and writes all of it.
- **A new profile takes two commands.** The module creates the profile
  empty and then writes it. Incus 6.0.0 ignores a document that `incus profile
  create` gets, without an error, so one command would leave an empty
  profile there.
- **A new profile that Incus refuses is removed again.** A failed run then
  leaves nothing, as on the newer Incus versions, whose `incus profile
  create` creates no profile from a refused document. No instance can use the
  profile yet.
- **A configuration value travels through standard input only.** The
  commands that MMX records hold no value, and a reason names keys and
  devices, never a value.
- **The module does not ask who uses a profile.** Incus refuses to delete a
  profile in use, and the task fails with Incus's message. A playbook that
  deletes the instances first and then removes the profile passes its dryrun
  and its run.
- **A failed write is read again.** `incus profile edit` can save a profile
  and still fail, when an instance that uses the profile cannot take the
  change. MMX tries a failed task again, and that try would find the saved
  profile and report success. So the module reads the profile after a failed
  write, and a profile that Incus saved fails the task once, with Incus's
  message.
- **A document that Incus refuses fails the task once.** Another try sends
  the same document and gets the same answer.
