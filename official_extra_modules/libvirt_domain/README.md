# `libvirt_domain`

`libvirt_domain` manages one virtual machine of [libvirt](https://libvirt.org/),
which libvirt calls a **domain**: its definition, whether it runs, and
whether libvirt starts it when the node boots. A `file` task writes the
domain's libvirt XML to the node, and the module makes libvirt's definition
follow that file. Without `domain_xml_file`, an existing domain keeps its
stored definition. Creating a domain needs the file. The module runs
libvirt's program `virsh` as root on the node.

```yaml
- file:
    path: /etc/mmx-libvirt/shop-db.xml
    template: templates/shop-db.xml
    permissions_mode: "0600"

- libvirt_domain:
    name: shop-db
    state: running
    domain_xml_file: /etc/mmx-libvirt/shop-db.xml
    start_at_boot: true
```

The module installs nothing and makes no disk image, network, or storage
pool: the XML file names the paths and the networks that the domain uses.

## Set up

These node tasks install libvirt and QEMU from the distribution's own
packages and start libvirt. The example project
[`examples/libvirt-host`](../../examples/libvirt-host/) is this list with one
domain behind it.

```yaml
# Debian and Ubuntu: apt installs "recommended" packages by default, which
# for libvirt and QEMU are several hundred more packages. This file turns
# that off for every apt command on the node.
- when: {equals: ["{{ .node.package_manager }}", apt]}
  file:
    path: /etc/apt/apt.conf.d/90-no-recommended-packages
    permissions_mode: "0644"
    content: |
      APT::Install-Recommends "false";

# libvirt, its program virsh, QEMU for x86_64 virtual machines, and
# qemu-img, which makes disk image files.
- when: {equals: ["{{ .node.package_manager }}", apt]}
  apt_package: {packages: [{name: libvirt-daemon-system, version: newest_when_first_installed}, {name: libvirt-clients, version: newest_when_first_installed}, {name: qemu-system-x86, version: newest_when_first_installed}, {name: qemu-utils, version: newest_when_first_installed}]}
- when: {equals: ["{{ .node.package_manager }}", dnf]}
  dnf_package: {packages: [{name: libvirt-daemon-driver-qemu, version: newest_when_first_installed}, {name: libvirt-client, version: newest_when_first_installed}, {name: qemu-kvm-core, version: newest_when_first_installed}, {name: qemu-img, version: newest_when_first_installed}]}

# Debian and Ubuntu have one libvirt daemon, libvirtd.
- when: {equals: ["{{ .node.package_manager }}", apt]}
  systemd_service:
    name: libvirtd.socket
    state: running
    start_at_boot: true

# The Red Hat family has one daemon for each kind of work; virtqemud is
# the one for QEMU.
- when: {not: {equals: ["{{ .node.package_manager }}", apt]}}
  systemd_service:
    name: virtqemud.socket
    state: running
    start_at_boot: true
- when: {not: {equals: ["{{ .node.package_manager }}", apt]}}
  systemd_service:
    name: virtqemud.service
    state: do_not_start_or_stop
    start_at_boot: true
```

- **The unit to start is the socket, not the service.** libvirt's daemon
  ends by itself after two minutes without a request and without a running
  domain, and its socket unit starts it again for the next request. A task
  that asks for a running *service* would start it again on most runs and
  report a change each time.
- **`virtqemud.service` must be enabled, or no domain starts when the node
  boots.** With only the socket enabled, the daemon starts at the first
  `virsh` call after the boot, and only then starts the domains with
  `start_at_boot: true`. The packages of the Red Hat family enable the
  service, and the task above keeps it enabled. On Debian and Ubuntu the
  package enables `libvirtd.service`.
- **The file for apt saves most of the installation.** On Debian 13,
  libvirt and QEMU bring about 70 packages without recommended packages and
  more than 360 with them. The setting holds for every later apt command on
  the node; a node whose other packages need their recommended packages
  leaves the task out.
- **More packages for some domains.** The packages above are enough to
  define and start a domain and to make its disk image files. Name these
  when a domain needs them:

  | For | Debian, Ubuntu | Red Hat family |
  |---|---|---|
  | UEFI firmware | `ovmf` | `edk2-ovmf` |
  | An emulated TPM | `swtpm`, `swtpm-tools` | `swtpm`, `swtpm-tools` |
  | libvirt's `default` network | `dnsmasq-base` | `libvirt-daemon-config-network` |

### libvirt's `default` network

A domain whose XML has `<interface type='network'>` with
`<source network='default'/>` needs libvirt's network `default`: a bridge
`virbr0` on the node with its own DHCP and DNS server. libvirt defines that
network when its packages are installed, and on some distributions it does
not start it. These tasks start it and make it start at boot; no module
manages libvirt's networks.

```yaml
- when: {equals: ["{{ .node.package_manager }}", apt]}
  apt_package: {packages: [{name: dnsmasq-base, version: newest_when_first_installed}]}
- when: {equals: ["{{ .node.package_manager }}", dnf]}
  dnf_package: {packages: [{name: libvirt-daemon-config-network, version: newest_when_first_installed}]}

# Only where libvirt has one daemon for each kind of work.
- when: {not: {equals: ["{{ .node.package_manager }}", apt]}}
  systemd_service:
    name: virtnetworkd.socket
    state: running
    start_at_boot: true

- shell:
    script: virsh -c qemu:///system net-start default
    skip_if_this_command_succeeds: virsh -c qemu:///system net-list --name | grep -qx default
- shell:
    script: virsh -c qemu:///system net-autostart default
    skip_if_this_command_succeeds: virsh -c qemu:///system net-list --all --autostart --name | grep -qx default
```

A started network turns on IP forwarding on the node
(`net.ipv4.ip_forward`) and adds firewall rules for the bridge. With these
tasks, a domain with a card on the network and `start_at_boot: true` starts
when the node boots: the network is active by then.

## Inputs

| Input | Meaning | When omitted |
|---|---|---|
| `name` | The domain's name, such as `shop-db`. It must be the `<name>` in the XML file: the file must hold the text `<name>shop-db</name>`. A name must not be a number or have the form of a UUID, and must not start with a hyphen. | Required. |
| `state` | `running`: the domain exists and runs. `stopped`: it exists and is shut off. A supplied XML file sets its definition. `absent`: libvirt has no definition of it. | Required. |
| `domain_xml_file` | The absolute path, on the node, of the file with the domain's libvirt XML. The file is text in UTF-8 of at most 1 MiB. | An existing domain keeps its stored definition. A missing domain is a missing prerequisite: dryrun reports `not_checked`; a run fails once. |
| `start_at_boot` | `true`: libvirt starts the domain when the node boots (`virsh autostart`). `false`: it does not. | The setting stays as it is; a new domain does not start at boot. |
| `wait_for_shutdown_at_most` | How long `state: stopped` waits for shutdown. A nonnegative duration of whole seconds, such as `30s` or `2m`. Shutdown checks repeat each second. | `2m`. |
| `power_off_when_shutdown_takes_longer` | What happens when `wait_for_shutdown_at_most` ends and the domain still runs. `true`: the module cuts its power (`virsh destroy`). `false`: the task fails, and the domain runs on. | `false`. |
| `libvirt_connection_uri` | Which libvirt on the node the module manages, as `virsh --connect` takes it. | `qemu:///system`: the QEMU and KVM domains of the whole node. |

- **`running`** creates the domain when libvirt has none of this name,
  defines it again when the file changed, and starts it. It resumes a
  domain that is paused.
- **`stopped`** creates or defines the domain in the same way and asks its
  operating system to shut down (`virsh shutdown`). It then reads the
  domain's state once in each second, for at most
  `wait_for_shutdown_at_most`. The task waits once: MMX does not try
  this failure again. A paused domain fails the task, because a request to
  shut down cannot reach it.
- **`absent`** removes the definition of a domain that is shut off. The
  domain's disk image files, the variables of its UEFI firmware, and the
  state of its emulated TPM stay. A domain that libvirt does not have is
  unchanged. `absent` accepts only `name` and `libvirt_connection_uri`.
- **`absent` needs a domain that is shut off and has no saved memory image,
  no snapshot, and no checkpoint.** libvirt keeps the definition of a
  domain with a saved memory image or a snapshot, and it would leave a
  running domain running without a definition. An earlier node task changes
  that: `state: stopped`, or a `shell` task with `virsh managedsave-remove`
  or `virsh snapshot-delete`.
- **`absent` refuses a domain with checkpoints, where libvirt would not.**
  libvirt removes the definition of such a domain and, without a word, its
  record of each checkpoint. An incremental backup continues from that
  record. `absent` removes a definition and nothing else, so the task fails
  until an earlier node task removed the records: `virsh checkpoint-delete
  shop-db NAME --metadata` removes libvirt's record of one checkpoint and
  leaves the checkpoint's bitmap in the disk image.
- **A running domain keeps its old definition.** When the file changes
  while the domain runs, libvirt stores the new definition, and the running
  domain keeps the old one until it is shut off and started again. The
  task's `reason` says so. The module never restarts a domain by itself.
- **Give every network card its MAC address in the XML**
  (`<mac address='52:54:00:12:34:56'/>` inside `<interface>`). libvirt
  gives a card without one a new random address each time the domain is
  defined, so after each change of the file the operating system inside
  sees a new card.

## Outputs

| Output | Meaning |
|---|---|
| `created` | `true` when this task created the domain: libvirt had no domain of this name. In a dryrun: `true` when a run would create it. |

A later node task reads the `created` output as
`{{ .node_tasks.<id>.created }}`.

In a node task block's `changes`, the task counts by its fingerprint: the
SHA-256 of a supplied XML file, the wanted `state`, and `start_at_boot`.
Without an XML file, the fingerprint compares no definition hash. A
`systemd_service` task or a `shell` task with
`only_when_tracked_inputs_changed: true` in the same block acts when one of
them changes.

## What a dryrun reports

A dryrun reads with `virsh` and changes nothing.

- **A change:** `would_change`, with a `reason` that says what a run would
  do, such as `would create the domain shop-db from
  /etc/mmx-libvirt/shop-db.xml; would set the domain shop-db to start at
  boot; would start the domain shop-db`. The result lists each `virsh`
  command that a run would run.
- **A shutdown:** `would ask the domain shop-db to shut down and wait at
  most 120 seconds`. Whether the operating system shuts down within the
  wait shows only in a run.
- **An XML file that an earlier node task of the same dryrun would write,**
  such as the `file` task after a change of its template: the file on the
  node is not yet the file of the run, so the dryrun reads nothing of it.
  It reports `would define the domain shop-db again from
  /etc/mmx-libvirt/shop-db.xml, which an earlier node task of this dryrun
  would change`, and the start, the shutdown, and the start at boot as
  always. This holds for a first dryrun too, where the file does not exist
  yet.
- **`not_checked`:**
  - while the program `virsh` is not installed, because an earlier node
    task may install it;
  - while libvirt does not answer, because an earlier node task may start
    its socket unit;
  - while the XML file does not exist and no earlier node task of the
    dryrun would write it;
  - while the domain does not exist and the task gives no XML file,
    because an earlier node task may create the domain;
  - for `state: absent` while the domain is not shut off, or has a saved
    memory image, a snapshot, or a checkpoint, because an earlier node task
    may change that.
- **A failure, as in a run:** an input that breaks a rule, an XML file of
  another domain, a paused domain with `state: stopped`, and a domain in a
  state that the module does not leave.
- **What a dryrun cannot know:** whether libvirt accepts the XML, which
  libvirt checks only when it defines the domain, and whether the domain
  starts, for example with a disk image that is missing.
- **What the reads leave on the node:** nothing. `virsh` writes no file.
  Where libvirt's daemon does not run, the first read starts it through
  its socket unit, and the daemon ends by itself two minutes later.

## Examples

Define a domain from a template, start it, and start it at every boot:

```yaml
- directory:
    path: /etc/mmx-libvirt
    permissions_mode: "0700"
- file:
    path: /etc/mmx-libvirt/shop-db.xml
    template: templates/shop-db.xml
    permissions_mode: "0600"
- libvirt_domain:
    name: shop-db
    state: running
    domain_xml_file: /etc/mmx-libvirt/shop-db.xml
    start_at_boot: true
```

Shut a domain off before work on the node: wait up to five minutes for its
operating system, and cut its power when it still runs then:

```yaml
- libvirt_domain:
    name: shop-db
    state: stopped
    wait_for_shutdown_at_most: 5m
    power_off_when_shutdown_takes_longer: true
```

Take a domain away: shut the domain off, then remove the domain's
definition. The domain's disk image files stay on the node:

```yaml
- libvirt_domain:
    name: shop-db
    state: stopped
    domain_xml_file: /etc/mmx-libvirt/shop-db.xml
- libvirt_domain:
    name: shop-db
    state: absent
```

## Errors and what they mean

Every reason starts with `libvirt_domain`. `shop-db` stands for the domain's
name. A reason of the form `<what the module did>: <text>` ends with
`virsh`'s own message. MMX tries a failed task again where another try can
help: while libvirt does not answer, and when a `virsh` command such as a
start fails. It tries once where another try cannot help or would hide
something: a task that cannot succeed on the node as it is
(`failure_kind: config`), a state that an earlier node task must
change first (`failure_kind: missing_prerequisite`), a shutdown that took
longer than the wait, and every failure after the task created the domain,
so that the result keeps `created: true`.

| The reason holds | What it means, and what to do |
|---|---|
| `the program virsh is not installed` | The node has no libvirt client. Install the package that the reason names for your distribution in an earlier node task; see [Set up](#set-up). |
| `libvirt at qemu:///system does not answer` | `virsh` could not connect: libvirt's socket unit does not run, or `libvirt_connection_uri` names no libvirt of the node. Start the socket unit in an earlier node task; see [Set up](#set-up). |
| `the domain ... does not exist; create it in an earlier node task with domain_xml_file` | The task gives no XML file and the domain does not exist. Dryrun reports `not_checked`, because an earlier node task may create the domain. A run fails once; create the domain in an earlier node task, or add `domain_xml_file` with the absolute path of its libvirt XML. |
| `the file ... of domain_xml_file does not exist` | Nothing wrote the XML file yet. Write it in an earlier node task, with `file`. |
| `libvirt_domain.state: absent accepts only name and libvirt_connection_uri` | A task with `state: absent` holds another input. Remove the input that the reason names. |
| `libvirt_domain.domain_xml_file: MMX cannot read the file as text` | The file is larger than 1 MiB or is not UTF-8. Write the XML as UTF-8 text. |
| `libvirt_domain.name: the file ... does not hold <name>shop-db</name>` | The XML file is for another domain, or writes its name in another form. libvirt would define the domain that the file names. Make `name` and the file's `<name>` the same text. |
| `libvirt_domain.name: must not be a number`, `must not have the form of a UUID` | `virsh` takes such a text for the ID or the UUID of a domain before it looks for a name, so a task could change another domain. Give the domain another name. |
| `defining the domain shop-db from ...: Failed to define domain from /dev/stdin: ...` | libvirt could not define the domain; its own reason follows. `/dev/stdin` is the file's text, which the module hands to `virsh`. When the reason shows an error of the XML, MMX tries once: correct the file. `virsh domxml-to-native qemu-argv FILE` on the node reads a file as libvirt's QEMU driver does and defines nothing. |
| `... domain 'shop-db' already exists with uuid ...` | The file holds a `<uuid>` that is not the domain's. Remove `<uuid>` from the file; the module adds the domain's own. |
| `starting the domain shop-db: Failed to start domain 'shop-db': ...` | libvirt could not start the domain; its own reason follows, such as a missing disk image or a network that is not active. Provide what the reason names. The domain stays defined, and the next run starts it. |
| `Before that, the task created the domain shop-db ...` | A step failed after the task created the domain. MMX does not try again, because the next try would report `created: false`. Correct what the reason names and run again: the next run does what is missing. |
| `the domain shop-db did not shut down within 120 seconds` | The operating system ignored the request, or needs longer. The domain runs on, and the task waited once. Raise `wait_for_shutdown_at_most`, or set `power_off_when_shutdown_takes_longer: true`. libvirt's documentation says that a KVM domain needs ACPI (`<features><acpi/></features>` in its XML) for a graceful shutdown. |
| `the domain shop-db is paused, and a request to shut down cannot reach a paused domain` | Somebody paused the domain (`virsh suspend`). Resume it (`virsh resume shop-db`, or a task with `state: running`), then shut it off. |
| `the domain shop-db is in the state "..."` | The domain is in a state that the module does not leave by itself, such as `crashed` or `pmsuspended`. Look at it with `virsh domstate --reason shop-db`. |
| `the domain shop-db shuts down at this moment` | `state: running` met a domain that is shutting down. Nothing to do: MMX tries the task again, and the module starts the domain once it is shut off. |
| `the domain shop-db is not shut off ..., and absent removes the definition only of a domain that is shut off` | libvirt itself would remove the definition and leave the domain running without one. Shut the domain off in an earlier node task, with `state: stopped`. A dryrun reports the task `not_checked` with this reason. |
| `the domain shop-db has a saved memory image` | Somebody saved the domain's memory to disk (`virsh managedsave`), and libvirt keeps the definition of such a domain. Run `virsh managedsave-remove shop-db` in an earlier node task, or start the domain and shut it off. |
| `the domain shop-db has snapshots (...)` | libvirt keeps the definition of a domain with snapshots. Delete each in an earlier node task: `virsh snapshot-delete shop-db NAME`. The reason names the first five. |
| `the domain shop-db has checkpoints (...)` | libvirt would remove its record of each checkpoint with the definition, and `absent` removes nothing but the definition. Remove the records in an earlier node task: `virsh checkpoint-delete shop-db NAME --metadata`. The reason names the first five. |
| `virsh answered for the domain ..., so it does not take ... for a name` | `virsh` found another domain under the task's name. Give the domain another name. |

MMX reports `changed` for every `virsh` command that ran, also for a define
that libvirt refused: it cannot see whether a failed command changed
something.

A `virsh` whose libvirt daemon hangs waits without any output: on the test
nodes it still waited after 90 seconds. Give the node task a `time_limit`
where that must not hold a run up.

## Tested on

**Distributions:** Debian 12 and 13, Ubuntu 24.04 and 26.04, Rocky Linux 9, AlmaLinux 10.

The integration suite `tests/integration/libvirt_domains.py` runs the whole
test on these distributions, each in a virtual machine with nested
virtualization, with the packages of the distribution's own repositories:

| Distribution | libvirt | QEMU |
|---|---|---|
| Debian 12 | 9.0.0 | 7.2.22 |
| Debian 13 | 11.3.0 | 10.0.13 |
| Ubuntu 24.04 | 10.0.0 | 8.2.2 |
| Ubuntu 26.04 | 12.0.0 | 10.2.1 |
| Rocky Linux 9, AlmaLinux 10 | 11.10.0 | 10.1.0 |

libvirt and QEMU can be installed on all six, so the suite runs the whole
test everywhere. The module is tested with `qemu:///system` and x86_64
domains.

## Why it works this way

- **An omitted XML file keeps the stored definition.** Starting or stopping
  an existing domain needs no new definition. Creation needs an XML file.
  A missing domain without an XML file is a missing prerequisite, so a
  dryrun can continue after an earlier node task would create the domain;
  [omission never deletes](../../docs/changing-mmx/design/modules.md#leaving-a-setting-out-never-deletes).
- **The state is `stopped`.** The same word stops Incus instances and libvirt
  domains, so a reader needs no separate spelling for each tool.
- **The shutdown wait is a duration.** `wait_for_shutdown_at_most: 2m` reads
  like the time limits of core modules. Whole seconds match the check interval.

- **The XML comes from a file on the node.** A domain's XML is long and
  usually a template. The `file` task shows its difference and tracks it,
  and the file can stay readable by root alone.
- **The module records the file's SHA-256 in the domain.** libvirt rewrites
  a definition when it stores it, so the stored XML never equals the file
  as text. The module keeps the SHA-256 of the file it last defined in the
  domain's own libvirt metadata (the element `last_defined_file` in the
  namespace `urn:mmx:libvirt_domain`), and defines the domain again when
  the file's SHA-256 differs. The record lives and dies with the domain.
  Defining a domain by hand removes the record, and the next run then
  defines the domain from the file again.
- **The module adds the domain's UUID to the XML.** libvirt refuses a second
  definition of a name unless the XML holds the domain's UUID, and that is
  a value that libvirt chose. The module puts `<uuid>` before `</domain>`
  in the text that it hands to `virsh`; the file stays as it is. A file
  with its own `<uuid>` is used as it is.
- **The XML reaches `virsh` through standard input.** A domain's XML can
  hold a password, for example of a VNC display, and a command line shows
  in the node's process list. Only the lines of `virsh`'s error message
  reach a task's `reason`, never a line of the file that libvirt quotes.
- **The shutdown waits a bounded time, once.** `virsh shutdown` only sends
  the request, and an operating system can ignore it for ever. A task must
  end, and cutting the power is the project's choice, not the module's. MMX
  does not try a wait that ended again: another try would send a second
  request and wait a multiple of the seconds that the task names.
- **A failure after the task created the domain is not tried again.** The
  next try would find the domain and report `created: false` for a domain
  that this task created.
- **`absent` removes a definition and no data.** A plain `virsh undefine`
  deletes the state of an emulated TPM without a word, and that state can
  hold a disk's key. A plain `virsh undefine` fails for a UEFI domain that
  was started once. So the module runs
  `virsh undefine --keep-nvram --keep-tpm`.
- **`absent` refuses what libvirt allows for checkpoints.** On the test
  nodes, `virsh undefine` removed the definition of a domain with
  checkpoints on every libvirt version below. Where the suite looked
  (libvirt 11.3.0 and 11.10.0), libvirt's records of the checkpoints were
  gone with it. A task that says "remove the definition" must not drop the
  records that a backup needs.
- **`absent` does not shut a domain off.** libvirt removes the definition of
  a running domain and leaves the domain running without one; it then
  disappears when it stops. The module reads the state first and fails. In
  a dryrun it reports `not_checked`, because a playbook often shuts a
  domain off in one node task and removes its definition in the next.
- **`running` resumes a paused domain; `stopped` fails for one.** The task
  says that the domain runs, and a paused domain does not. A request to
  shut down cannot reach a paused domain.
- **Networks and storage pools have no module.** A domain works with a
  bridge of the node and with image files named by path.
