# MMX: network configuration modules

Part of the [main plan](README.md).
The network configuration design covers four core modules for persistent
network configuration:
[`ifupdown_interfaces_file`](../../reference/modules/ifupdown_interfaces_file.md),
[`netplan_file`](../../reference/modules/netplan_file.md),
[`networkmanager_connection_file`](../../reference/modules/networkmanager_connection_file.md),
and [`systemd_networkd_file`](../../reference/modules/systemd_networkd_file.md).
`docs/changing-mmx/design/modules-network.md` owns the behavior that the four share.
A fifth core module, [`wireguard_interface`](#a-wireguard-interface), manages
one WireGuard interface. The shared sections on inputs, runs, removal,
dryrun, results, and limits also cover WireGuard file changes and reachability
confirmation. WireGuard's own section defines the interface inputs,
activation, service repair, and private-key handling.
Each module's own checks, activation, examples, errors,
measurements, and reasons are on its page; the
[module index](../../reference/modules/README.md) lists every page.

The four file modules are built, each with a dedicated guest suite.
[Validation](../validation.md) records current guest results.
Core modules are actions compiled into the **node runner executable**,
MMX's installed program on each node.

The **controller machine** is where the user runs `mmx`; a **node** is a
Linux server that MMX manages. The **controller process** is that running `mmx`
command. A **node task** is one action in the playbook, the file that lists
the work. A **fleet stage** is one ordered playbook entry selecting nodes
and the node tasks they run. `ifupdown_interfaces_file`, `netplan_file`,
and `systemd_networkd_file` each take one whole file in the network tool's
own format. Addresses, routes, DNS, bridges, and VLANs
stay in the network tool's format. MMX does not give addresses, routes,
DNS, bridges, or VLANs a second set of fields.

A wrong network file can cut the node off while its activation command
succeeds. An Ansible rescue needs the connection that has gone. An MMX
**node job**, one attempt to run a fleet stage's work on a node, continues
locally without the controller machine. The node can restore its previous
file without a working connection. Recovery without a connection is why
`ifupdown_interfaces_file`, `netplan_file`, and `systemd_networkd_file`
exist. A file task and a command could write the network configuration and
activate the network tool.

A **reachability confirmation** is a request the controller process sends
over a new SSH or HTTPS connection for one change of one node job. An
**owed restore** is the node's durable record that its previous file must
come back unless the required confirmations arrive in time. A systemd timer
performs the owed restore independently. The
[execution design](execution.md#reachability-confirmation-and-owed-restore)
owns reachability confirmation and owed restore.

## Choose the module for the node's tool

| Module | Tool and file on the node | Measured test guests |
|---|---|---|
| `ifupdown_interfaces_file` | ifupdown, the Debian package; `/etc/network/interfaces`, or `/etc/network/interfaces.d/<name>` with a `name` of letters, digits, `_`, and `-`. | Debian 12 and 13, switched to ifupdown as Debian's installer and some server providers set nodes up. |
| `netplan_file` | Netplan; `/etc/netplan/<name>`, with `name` ending in `.yaml`. | Ubuntu 24.04 and 26.04, over systemd-networkd. |
| `networkmanager_connection_file` | NetworkManager; `/etc/NetworkManager/system-connections/<name>`, with `name` ending in `.nmconnection`. | Rocky Linux 9 and AlmaLinux 10. |
| `systemd_networkd_file` | systemd-networkd; `/etc/systemd/network/<name>`, with `name` ending in `.network` or `.netdev`. | Debian 12 and 13. |

The table describes the measured VM images of the test guests, not every
installation of Debian 12 and 13 or Ubuntu 24.04 and 26.04.
A cloud image can use another tool.
The measured Debian test images use systemd-networkd, while Debian cloud
images can use Netplan. Debian's installer and the installers of some
server providers set nodes up with ifupdown. MMX does not select, install,
or switch network tools for the user.

Module pages that rely on this section:
[`ifupdown_interfaces_file`](../../reference/modules/ifupdown_interfaces_file.md),
[`netplan_file`](../../reference/modules/netplan_file.md),
[`networkmanager_connection_file`](../../reference/modules/networkmanager_connection_file.md),
[`systemd_networkd_file`](../../reference/modules/systemd_networkd_file.md).

## Inputs shared by all four modules

The table describes the three whole-file network modules. WireGuard has
[interface inputs](#inputs), including the confirmation duration below.
WireGuard configuration files belong to `root:root` with mode `0600`.

| Field | Default | Meaning |
|---|---|---|
| `name` | required | One filename, including its ending, in the module's fixed directory. No directory components. `ifupdown_interfaces_file` takes `path` instead, as below. |
| `content` | none | The complete file text. Exactly one of `content` and `template` is required for `present`. |
| `template` | none | A template file on the controller machine, rendered with the node's values, as for `systemd_unit`. |
| `state` | `present` | `present` writes the file and activates it; `absent` removes it and its running configuration. |
| `restore_unless_controller_confirms_within` | `2m` | A positive duration bounding the confirmation wait; the node restores an unconfirmed change when its timer expires. |

`ifupdown_interfaces_file` takes `path` in place of `name`: either
`/etc/network/interfaces` or one file directly in `/etc/network/interfaces.d`.
Debian's installer and the installers of some server providers put the main
interface's settings in `/etc/network/interfaces` itself. So
`ifupdown_interfaces_file` must reach a file outside
`/etc/network/interfaces.d`. `/etc/network/interfaces` cannot be removed.

`state: absent` accepts `name` or `path`, `state`, and
`restore_unless_controller_confirms_within`; it rejects `content` and
`template`, like [unit removal](../../reference/modules/systemd_unit.md#removal).
Template paths and rendering follow `systemd_unit`: relative to the
project root, or the role directory for a reusable list of node tasks.
An existing symlink, directory, or special file at the destination is
refused. The module owns one regular file, not a path to somewhere else.

The modules fix the destination directories and ownership. The table gives
the managed files' modes. ifupdown alone accepts an optional
`permissions_mode`:

| File | Owner and group | Mode |
|---|---|---|
| ifupdown `/etc/network/interfaces` and `/etc/network/interfaces.d/<name>` | `root:root` | Keep an existing mode; `0644` for a new file; optional `permissions_mode` overrides the existing or default mode. |
| Netplan `.yaml` | `root:root` | `0600` |
| NetworkManager `.nmconnection` | `root:root` | `0600` |
| systemd-networkd `.network` | `root:root` | `0644` |
| systemd-networkd `.netdev` | `root:systemd-network` | `0640` |

A `.netdev` file can hold a key, so only root and the `systemd-network`
group may read it. The `systemd-network` group must exist.
Missing destination directories are created as `root:root`, mode `0755`;
existing directories are left alone. Files receive the
[SELinux label of their path](modules.md#files-get-the-selinux-label-of-their-path).
A secret in the content or template follows the ordinary module rules:
the node task's result and difference are suppressed, and saved previous
files stay in root-private storage on the node.

Module pages that rely on this section:
[`ifupdown_interfaces_file`](../../reference/modules/ifupdown_interfaces_file.md),
[`netplan_file`](../../reference/modules/netplan_file.md),
[`networkmanager_connection_file`](../../reference/modules/networkmanager_connection_file.md),
[`systemd_networkd_file`](../../reference/modules/systemd_networkd_file.md),
[`wireguard_interface`](../../reference/modules/wireguard_interface.md).

## What a run does

Each configuration file change completes steps 1 through 9 below before the
next node task starts. WireGuard also repairs running interfaces and service
state when the file matches. Running-state repairs do not wait for
confirmation because restoring an identical file would undo no change.
Each module runs once and rejects node task `retry`,
`time_limit`, and `time_limit_per_try`. Each module ignores project timing
defaults. The module's own duration bounds confirmation. A retry must not
cut the node's network again.

1. Compare the requested file and its metadata with the node. If the
   requested file and metadata match the node and no restore of this file
   is owed, report unchanged without activation or a confirmation wait.
   A missing file requested as absent is also unchanged. WireGuard can still
   repair the running interface or service without changing the file.
   An owed restore never becomes an unchanged result.
2. Check prerequisites before changing anything: the tool is installed,
   the required service is available, and no earlier node job owes a restore
   of the managed file. WireGuard starts an inactive service.
   Netplan requires the systemd-networkd renderer and its active service,
   plus `netplan`, `networkctl`, `systemctl`, and `ip`. ifupdown requires
   the `ifup` of the Debian package, not ifupdown2, and an enabled
   `networking.service`.
   NetworkManager also checks for an old-format profile of the
   interface named by the proposed file. That conflict fails in dryrun too.
3. Where the tool has a usable check, check the proposed file before
   installing the proposed file. Netplan checks a copy of the configuration
   tree with the candidate added, replaced, or removed. NetworkManager checks
   the candidate through its offline modify command, including in dryrun.
   ifupdown checks
   the candidate with `ifquery` and `ifup --no-act`, which read it from
   standard input, including in dryrun. Systemd-networkd has no usable
   pre-install check in the measurements recorded on the module's page.
4. Ask the controller process for reachability confirmations before the
   change, over new SSH and HTTPS connections. HTTPS reaches the **node
   HTTPS API process**, MMX's long-running service on port 7443. For a
   node whose inventory says `connection: ssh_only`, only SSH is tried.
   A connection kind that delivers a reachability confirmation before the
   change must also deliver a reachability confirmation after the change.
   If neither SSH nor HTTPS delivers a confirmation, fail before any
   managed change.
5. Save the previous file, including its metadata, or record that no file
   existed. Durably record the owed restore under `/var/lib/mmx/` and arm
   its systemd timer, with real service and timer unit files on disk.
   Failure to make the restore ready fails before installing the candidate.
6. Atomically install the file, or remove the file for `absent`, then
   activate the configuration with the tool's own commands, as each module's
   page says. Remove virtual devices no longer declared by the managed file.
   Any failure after installation starts the restore at once through the
   timer's service.
7. Check the tool's acceptance after activation, as each module's page says.
   A failed check starts the owed restore timer's service at once.
   The task fails with the tool's words, subject to the ordinary
   secret-output rules.
8. Wait for new connections of every kind that worked before the change.
   The timer's deadline bounds the after-change confirmation wait.
   When every required confirmation arrives, cancel the timer, remove the
   saved previous file, and report changed.
9. Without every confirmation in time, the timer restores the previous
   file and activates it. The task fails, naming the connection kinds that
   did not confirm and the restore outcome. The node task never reports
   that the file is back unless restoration succeeded.

When the changed node is the controller machine, a connection to the node's
own address does not prove access to the fleet.
The controller process also requires new SSH answers from the inventory nodes
that answered before the change, including nodes outside `--limit`.
The [controller-machine confirmation
rule](execution.md#a-change-on-the-controller-machine-itself) defines the
required nodes, timing, and failure output.

The timer's duration starts when it is armed, before installation.
Activation and its checks consume part of the timer's duration.
Activation and checks cannot leave the node unprotected while they run.
The separate wait before the change uses the timer's duration.
The execution design owns
[deadlines and confirmation rules](execution.md#new-connections-before-and-after-the-change).

Module pages that rely on this section:
[`ifupdown_interfaces_file`](../../reference/modules/ifupdown_interfaces_file.md),
[`netplan_file`](../../reference/modules/netplan_file.md),
[`networkmanager_connection_file`](../../reference/modules/networkmanager_connection_file.md),
[`systemd_networkd_file`](../../reference/modules/systemd_networkd_file.md),
[`wireguard_interface`](../../reference/modules/wireguard_interface.md).

## What `state: absent` removes

`state: absent` removes three things:

1. the named file;
2. the running configuration that the file gave the node, such as an
   address or a route;
3. every virtual device that the file declared, such as a bridge, a bond,
   or a VLAN device.

For example, removing a Netplan file that declares the bridge `appbr0`
removes that file, the bridge's address, and `appbr0` itself. Whatever was
attached to the bridge, such as the network of a container or a VM, loses
that bridge.

WireGuard removal stops and disables `wg-quick@<name>.service` and removes
`/etc/wireguard/<name>.conf` through the owed restore. Only confirmed removal
also removes `/etc/wireguard/<name>.private_key`. A restored removal keeps
the key file.

`state: absent` never deletes a physical interface, another configuration
file, or a virtual device that only another file declares. Removing a
`.network` file removes its running settings; it does not delete the
device that a separate `.netdev` file declares.

```yaml
- netplan_file:
    name: 60-app-bridge.yaml
    state: absent
```

Each module finds the virtual devices of the previous file and checks
that a device is virtual before deleting the device. The method each
module uses to find the previous file's virtual devices differs by
tool: the pages of [`ifupdown_interfaces_file`](../../reference/modules/ifupdown_interfaces_file.md#what-removal-does),
[`netplan_file`](../../reference/modules/netplan_file.md#devices-and-addresses-that-a-change-removes),
[`networkmanager_connection_file`](../../reference/modules/networkmanager_connection_file.md#virtual-devices),
and [`systemd_networkd_file`](../../reference/modules/systemd_networkd_file.md#what-removal-does)
say how. Reading device declarations from the previous file supports
ownership and removal. Reading device declarations does not turn the
network file's settings into MMX inputs.

NetworkManager removed the measured bridge itself after file removal and
`nmcli connection load <removed-path>`. ifupdown's `ifdown`, run with the
file that declared them, removed the measured VLAN and bridge itself; the
`ifupdown_interfaces_file` checks that the VLAN and bridge are gone.
Netplan and systemd-networkd left the device.
For Netplan and systemd-networkd, explicit
`networkctl delete <virtual-device>` removed the measured bridge. The
modules `ifupdown_interfaces_file`, `netplan_file`,
`networkmanager_connection_file`, and `systemd_networkd_file` also remove a previously declared device when a
changed file no longer declares the device. `ifupdown_interfaces_file`,
`netplan_file`, `networkmanager_connection_file`, and `systemd_networkd_file` end with the file's removed
devices gone, not merely with configuration text gone.

Removal has the same timer and confirmations as installation. If removal
cuts the node off, the restore service puts the previous file back and
activates the previous configuration, allowing the tool to recreate the
previous file's devices. When restoring a newly added
file whose previous state was absent, the service removes that file and
the virtual devices it introduced.

Module pages that rely on this section:
[`ifupdown_interfaces_file`](../../reference/modules/ifupdown_interfaces_file.md),
[`netplan_file`](../../reference/modules/netplan_file.md),
[`networkmanager_connection_file`](../../reference/modules/networkmanager_connection_file.md),
[`systemd_networkd_file`](../../reference/modules/systemd_networkd_file.md),
[`wireguard_interface`](../../reference/modules/wireguard_interface.md).

### Several files run in playbook order

One node task owns one file. Several files are several node tasks, with
each change confirmed before the next starts. Order the node tasks that
manage separate network files so each intermediate state is harmless.
There is no transaction across files.

For a second address, keep the existing address in the interface's file
while adding the new address.
Do not remove the address used by the controller machine.

Netplan can declare several related devices in its one YAML file, and one
ifupdown file can hold several stanzas. systemd-networkd uses separate
files where the systemd-networkd formats require separate files.
NetworkManager also uses separate files.

The pages give complete bridge, VLAN, and second-address examples:
[`ifupdown_interfaces_file`](../../reference/modules/ifupdown_interfaces_file.md#examples)
with a VLAN and a route on the main interface;
[`systemd_networkd_file`](../../reference/modules/systemd_networkd_file.md#examples) in
separate files, with the order of creation and removal;
[`netplan_file`](../../reference/modules/netplan_file.md#examples) in one file; and
[`networkmanager_connection_file`](../../reference/modules/networkmanager_connection_file.md#examples)
in separate profiles.

Module pages that rely on this section:
[`ifupdown_interfaces_file`](../../reference/modules/ifupdown_interfaces_file.md),
[`netplan_file`](../../reference/modules/netplan_file.md),
[`systemd_networkd_file`](../../reference/modules/systemd_networkd_file.md).

## Dryrun

Dryrun follows [systemd unit dryrun](../../reference/modules/systemd_unit.md#what-a-dryrun-reports): render and
compare without writing a candidate. Report the proposed file difference
and `would_change`, or unchanged. Check the available prerequisites,
an existing owed restore, and old-format profile conflicts. NetworkManager
also checks the candidate offline and refuses duplicate UUIDs in dryrun;
ifupdown checks the candidate through standard input. Missing tools,
services, or groups that an earlier node task may provide produce
`not_checked`. An owed restore fails the dryrun node task.

Dryrun does not create a candidate tree, save a previous file, arm a timer,
activate configuration, delete devices, request confirmations, or wait.
Dryrun states that Netplan candidate validation, tool acceptance after
activation, and reachability after the change were not checked. For removal,
systemd-networkd lists the virtual devices that the existing file declares
and would remove. For every change, ifupdown lists the interfaces it would
take down and bring up. Netplan cannot list devices without generation,
which dryrun does not run. The Netplan dryrun states that
removal includes devices declared only by the removed file. A secret
suppresses the dryrun's file difference and device or interface preview,
as in other modules.

Module pages that rely on this section:
[`ifupdown_interfaces_file`](../../reference/modules/ifupdown_interfaces_file.md),
[`netplan_file`](../../reference/modules/netplan_file.md),
[`systemd_networkd_file`](../../reference/modules/systemd_networkd_file.md),
[`wireguard_interface`](../../reference/modules/wireguard_interface.md).

## Results and failure reasons

Use the ordinary module `status`, `change`, reason, and file difference;
there is no separate success status for network configuration. A confirmed
change is `succeeded` with `changed`; an unchanged file is `succeeded` with
`unchanged`. A WireGuard running-state repair reports `changed` even when
the configuration file matches. Dryrun uses `would_change` and explains
unperformed checks.
A refusal before any change is `failed` with `unchanged`. A restored change
is still `failed` with `changed`: the node's network was changed and then
restored. An interrupted operation whose changes cannot be established uses
`unknown`, following the [module result rules](modules.md#every-module-follows-the-same-contract).

The following are result texts; paths, connection kinds, and native
diagnostics are replaced by those of the actual node task:

| Situation | Text the user sees |
|---|---|
| Unchanged | `Network configuration file /etc/netplan/60-eth1.yaml is unchanged; no activation or reachability confirmation wait was needed.` |
| Dryrun | `Would replace /etc/systemd/network/20-second.network and activate it. Whether systemd-networkd accepts the proposed network configuration change and whether the node remains reachable after activation were not checked.` For a new file, use `Would write`. For removal, say `Would remove <path> and its running configuration.` and list declared virtual devices to remove. Netplan reports unperformed candidate validation and no device list. ifupdown reports that acceptance and reachability after activation were not checked and adds `Would take down: enp6s0.` and `Would bring up: enp6s0.` with the interfaces it would take down and bring up. |
| Missing tool or stopped service | `Cannot change /etc/netplan/60-eth1.yaml: netplan is not installed.` or `Cannot change network configuration file /etc/systemd/network/20-eth1.network: systemd-networkd.service is not running.` |
| Before-change confirmation failed | `No new SSH or HTTPS connection confirmed node reachability before the file change; /etc/netplan/60-eth1.yaml was not changed.` |
| Restore could not be armed | `Cannot schedule the owed restore for /etc/netplan/60-eth1.yaml; the destination file was not changed: <system error>.` |
| Device deletion failed | systemd_networkd_file: `Cannot remove virtual device <name> declared in <path>: <diagnostic>.` netplan_file: `Cannot remove virtual network device <name> declared by <path>: <diagnostic>.` |
| Confirmation expired, restore succeeded | `No reachability confirmation arrived over a new SSH connection within 2m. The previous file /etc/netplan/60-eth1.yaml and its running configuration are restored.` |
| Previous state was absent | `No reachability confirmation arrived over a new HTTPS connection within 2m. The new file /etc/netplan/60-eth1.yaml and its declared virtual devices were removed; the previous configuration is restored.` |
| Restore still owed | `Cannot change /etc/netplan/60-eth1.yaml: node job <node-job-id> still owes a restore of the file; restore service: mmx-owed-restore-<owed-restore-id>.service.` |
| Restore failed | `Restoring /etc/netplan/60-eth1.yaml failed: <tool diagnostic>. The owed restore and saved previous file remain; the previous running configuration is not confirmed restored.` |

For a failed restore, retain both the original failure reason and the
restore failure. If no previous file existed, say `The owed restore remains`
instead of claiming that a saved previous file remains. A later node job
for the file whose restore failed sees the owed restore and recorded reason,
even if the managed file's bytes already match. Secret-bearing diagnostics
follow the suppression rules for other module output.

Module pages that rely on this section:
[`ifupdown_interfaces_file`](../../reference/modules/ifupdown_interfaces_file.md),
[`netplan_file`](../../reference/modules/netplan_file.md),
[`networkmanager_connection_file`](../../reference/modules/networkmanager_connection_file.md),
[`systemd_networkd_file`](../../reference/modules/systemd_networkd_file.md),
[`wireguard_interface`](../../reference/modules/wireguard_interface.md).

## Decisions

| Decision | Reason |
|---|---|
| Four core modules named for ifupdown, Netplan, NetworkManager, and systemd-networkd. | The playbook shows the tool that runs on the node, and each tool has different activation and checks. |
| Take the native file whole through `content` or `template`. | The tools already model addresses, routes, DNS, bridges, and VLANs; a second MMX model adds no safety. |
| One file per node task, with fixed directory and ownership; ifupdown allows an explicit mode. | The owned resource and the saved previous state are clear; the tool receives files it can safely read. |
| An ifupdown file preserves its existing mode when `permissions_mode` is omitted; a new file gets `0644`. | Preserving a private file avoids making secrets readable by other accounts. A new file matches Debian's installer unless the task selects another mode. |
| An unchanged file with no owed restore does not activate or wait. | Reapplying unchanged network configuration would cause avoidable disruption. |
| Confirm every connection kind that worked before, over new connections. | An existing connection can stay alive while every new connection fails. |
| A durable owed restore and a systemd timer protect the change, with `2m` as the default duration. | The node can recover without the controller process, a live node job worker process, or an uninterrupted boot. |
| Every restore uses the timer's service, including immediate failure recovery. | There is one restoration path to implement and test. |
| Remove runtime configuration and declared virtual devices with the file. | The tools otherwise differ: Netplan and systemd-networkd left the measured bridge behind. |
| No automatic node task retries. | A retry would apply the same change that just cut off the node. |
| Dryrun writes no candidate and says which checks it skipped. | Network dryrun follows whole-file modules without pretending to have activated or confirmed the proposed network. |
| No address move, tool migration, or multi-file rollback. | The controller process confirms at the existing node address. The restore covers only the file the node task's module owns. |

## Limits

- One file per node task; no transaction or restore of unrelated files.
- No move of the node address that the controller machine uses. MMX does
  not discover a replacement address; missing confirmations restore the file.
- No move of a node from one network tool to another.
- A node firewall that limits new connections per address, such as
  `ufw limit`, can slow confirmation. Ten-second spacing between repeated
  attempts of each connection kind reduces pressure on firewall limits but
  cannot prevent limits caused by other connections.
- No MMX model of native file content.
- Restoring a file and reactivating the tool restores its configuration,
  not an identical snapshot of every dynamic address or lease. The measured
  restores recovered DHCP IPv4 and the default route; IPv6 snapshots differed.

Module pages that rely on this section:
[`ufw`](../../reference/modules/ufw.md),
[`wireguard_interface`](../../reference/modules/wireguard_interface.md).

## Remaining measurements

Each module page records what was measured for its tool, in its sections
"Tested on" and "Why it works this way";
[validation](../validation.md) records each suite's cases. The following
measurements remain open:

- A node restart after the confirmation deadline. The suites restart a node
  only before the deadline.
- Virtual device kinds that no suite creates, such as tunnels and VRFs. The
  suites create bridges, bonds, VLANs, dummy devices, and veth pairs.
- Recovery timing from actual loss of contact to working new SSH and HTTPS
  connections. The manual probes sampled recovery at roughly three or four
  seconds; they did not measure the exact recovery time or the full MMX path.
- Tool logs on Rocky Linux 9 that no case has read yet, when they are
  needed to keep the reasons of a failed native activation or restore.

Netplan try's unsampled transition on Ubuntu 24.04 and a systemd-networkd
native rollback are not build dependencies. The network configuration
design uses neither Netplan try's unsampled transition on Ubuntu 24.04
nor systemd-networkd native rollback.

## Tests the build needs

Go tests use stand-in tools, temporary directories, and controlled time to
check the following without changing the development machine's network:

- Input validation, template rendering, names and endings, file containment,
  ownership and permissions, secret suppression, and the missing-prerequisite results.
- Every compare and dryrun outcome, with no candidate write, timer,
  activation, deletion, or confirmation request during dryrun.
- Old-format conflict refusal before mutation, including dryrun, without
  opening the old file; native candidate and acceptance failures, including
  exit-zero failures and ignored-setting journal lines.
- New SSH connections without sharing and new HTTPS connections without
  keep-alive reuse; before/after separation, both connection kinds and each
  alone, neither kind reachable, stale requests, duplicate requests, lost
  replies, loss of progress between the before-change and after-change
  waits, and the confirmation-versus-expiry race.
- Durable previous-file storage, absent previous state, timer arming before
  mutation, immediate and timed restoration through the owed restore
  timer's service, activation commands that outlive the node job worker
  process, and refusal to replace the node runner executable while a
  restore remains owed.
- Failed restoration retained for the next node job. A new node task must
  not change a file while a restore of the file remains owed. The failed
  network task must not retry automatically.
- Device ownership, physical-device protection, removed declarations, and
  the task result after a successful or failed restore.

A VM guest suite must deliberately apply a file that cuts the node off from
the controller machine, then prove that the node restores itself and accepts
new connections. Cover ifupdown, Netplan, NetworkManager, and systemd-networkd, not only
command outputs. Check the previous file's bytes and metadata, runtime
address and route, failed task
reason, and cleanup after a successful restore. Also cover successful changes,
unchanged reruns, removal with device recreation, an added file restored to
absence, termination of the node job worker process, node restart, and a
controller process that stops during the confirmation wait.
Preserve an independent guest-console path for observations;
the recovery itself must not use the independent guest-console path.

Run the affected VM suites at the levels in the
[testing guide](../testing.md), on a Linux test server with KVM.
Use each tool on its measured guest family, and all relevant target
distributions at the end of the build step. `systemd_networkd_files.py`
covers the built systemd-networkd module on Debian 12 and 13,
`networkmanager_connection_files.py` covers the built NetworkManager module
on Rocky Linux 9 and AlmaLinux 10, `netplan_files.py` covers the built
Netplan module on Ubuntu 24.04 and 26.04, and `ifupdown_interfaces_files.py` covers
the built ifupdown module on Debian 12 and 13.
[Validation](../validation.md) records current results.

## A WireGuard interface

[`wireguard_interface`](../../reference/modules/wireguard_interface.md) manages one
WireGuard interface that wg-quick brings up: its file
`/etc/wireguard/<name>.conf`, its private key, and the systemd service
`wg-quick@<name>.service`. Package `wireguard-tools` provides `wg`,
`wg-quick`, and the service; all four target distributions ship it, and
their kernels have WireGuard.

### Inputs

| Field | Default | Meaning |
|---|---|---|
| `name` | required | The interface name, such as `wg0`: 1 to 15 of the characters `a-z`, `A-Z`, `0-9`, `_`, `.`, and `-`. wg-quick also accepts `=` and `+`, but systemd escapes them in the unit's name, and `wg-quick@<name>.service` would then look for another file. |
| `content` | none | The whole wg-quick file without its private key. Exactly one of `content` and `template` is required for `present`. |
| `template` | none | A template file on the controller machine, rendered with the node's values, as for `systemd_unit`. |
| `state` | `present` | `present` writes the file and brings the interface up. `absent` takes the interface down and removes the configuration file, then removes the private key after confirmation. |
| `restore_unless_controller_confirms_within` | `2m` | A positive duration bounding reachability confirmation. The node restores an unconfirmed file change when the timer expires. Activation consumes part of the duration. The before-change wait has the same limit. |

The file is wg-quick's own format, as for the other network modules: one
`[Interface]` section and one `[Peer]` section for each peer. A template
builds the peer list from the inventory, for example one `[Peer]` for each
node of the groups the managed node talks to, using each peer's public key
and private address from the peer's `vars`. MMX checks only what it must own:

- `PrivateKey` is refused. MMX adds `PrivateKey` from the node's key file.
- `SaveConfig = true` is refused, because wg-quick would then rewrite the
  file that MMX owns when the interface goes down.
- Exactly one `[Interface]` section, each `[Peer]` has a `PublicKey`
  that appears once, and an allowed IP belongs to one peer only, as
  WireGuard keeps it.
  A setting that wg-quick and wg do not know, or a value wg-quick and wg
  could not read, is refused too.

Invalid `content` is refused when the project loads, before `content`
reaches the node. Invalid rendered template content is refused on the node
before anything is written.

### The private key stays on the node

The module reuses an existing private key file, including a key written by
a first-boot script before MMX runs. Existing key bytes remain unchanged.
When no key file exists, the module creates 32 random bytes from the
operating system. The module clamps the bytes as `wg genkey`
clamps them, because the kernel keeps and `wg show` prints a key in that form.
The key file `/etc/wireguard/<name>.private_key` is owned by root with mode
`0600`. MMX corrects an existing key file's owner, group, and mode to
`root:root`, `0600`, without replacing the key. The directory
`/etc/wireguard` has mode `0700`.

The private key never leaves the node. The task's output `public_key` is the
matching public key. A coding agent or a person reads the public key from
`mmx results --json` and commits the public key to the inventory.
Peer nodes' templates find the public key in the inventory.
Each later run reads the key file again.

MMX writes the key into the file as the first line of `[Interface]`:
`PrivateKey = ...`. A key set by a `PostUp` command would be lost at every
reload, because `wg syncconf` replaces the running settings with the file's
and the stripped file would have no key.

The configuration file therefore holds the key and has mode `0600`.
The configuration file's difference in the result, which the saved run
history keeps, shows `PrivateKey = (the node's private key, not shown)` in
place of the key, and `PresharedKey = (a preshared key, not shown)` in place
of a preshared key that an earlier file held. MMX keeps no full difference of
the file beyond the masked difference saved in the result. Saved previous
and proposed configuration files contain keys and stay in root-private owed
restore storage: root-owned directories with mode `0700`, files with mode
`0600`. The module
gives the key to the node task execution engine as a login handed only to
a tool (`Scope.FormOfLoginHandedOnlyToATool`). The node task execution engine
replaces the key with `[redacted]` in any other text of the result.

To replace the key, a one-time playbook removes the key file with a `file`
task and runs `wireguard_interface` again; the new public key then goes to
the inventory like the first public key.

### What a run does

1. Check prerequisites: `wg`, `wg-quick`, and `wg-quick@.service`. When a
   listed program or service is missing, the task fails with a message that
   names `wireguard-tools`. When a listed program or service is missing,
   a dryrun reports the task not yet checkable because an earlier task may
   install `wireguard-tools`.
2. Reuse or create the private key, and render the configuration file with
   the private key. Write a new key before preparing the configuration
   change. The key file stays after an owed restore, so the next run reuses
   the generated key.
3. Compare the file and its metadata, and compare the running interface
   with the file:
   - from `wg show <name> dump`: the private key, the listen port, and the
     peers keyed by public key, each with its allowed IPs as a set, its
     keepalive, and whether it has a preshared key. The key itself is never
     read into MMX. The endpoint is not compared because WireGuard updates the
     endpoint when a peer roams. `wg show` displays the resolved IP address
     instead of the configured host name. A `[Peer]` with the
     node's own public key is left out, as WireGuard leaves it out, so a
     template may list every node of a group;
   - the interface's addresses and, when the file sets one, its MTU;
   - a route on the interface for each allowed IP, as `wg-quick up` adds
     them: in the main table, or in the one that `Table` names, and none for
     `Table = off`. An allowed IP of `/0` in the main table is not compared,
     because wg-quick routes it through rules of its own;
   - the service is enabled and active.
4. When everything matches, report unchanged and touch nothing.
5. When the configuration file's contents or metadata differ, hand the
   prepared file change to the node's owed restore service. First creation
   and removal also follow the [shared confirmation steps](#what-a-run-does).
   The module rejects node task `retry`, `time_limit`, and `time_limit_per_try`,
   ignores project timing defaults, and runs once. Activate the proposed
   configuration with the action planned from the comparison:

   - start the service with `systemctl enable --now` when it is not active;
   - enable `wg-quick@<name>.service` when the service is active but not
     enabled. If enabling the service is the only change, do not reload
     or restart `wg-quick@<name>.service`;
   - restart `wg-quick@<name>.service` when any condition below holds:
     - a changed line is a setting that `wg-quick strip` drops, such as
       `Address`, `MTU`, `DNS`, `Table`, or a `PreUp` or `PostUp` command;
     - a running address or the running MTU differs from the configuration;
     - a changed peer needs a route that the interface does not have yet;
     - an allowed IP leaving the interface has a route added by wg-quick,
       because a reload would leave the obsolete route behind;
     - a peer has a keepalive or a preshared key that the file no longer
       sets, because `wg syncconf` in older wireguard-tools releases keeps
       both (fixed upstream in May 2025 for preshared keys and February
       2026 for keepalives);
   - otherwise reload `wg-quick@<name>.service`. The reload runs
     `wg syncconf` with the stripped
     file, which changes only what differs: an added or removed peer, an
     allowed IP inside the interface's subnet, a keepalive or preshared key
     that the file sets, an endpoint, or the listen port. Peers unaffected by the reload keep their sessions.
6. Check the running interface against the configuration file again. A
   remaining difference fails the node task. For a file change, the owed
   restore service restores the previous configuration on activation failure
   or missing reachability confirmation. A confirmed change succeeds with
   `changed`. An unconfirmed change fails, naming missing connection kinds
   and the restore outcome.

When only the running interface or service differs, perform the planned
repair directly, without a confirmation wait. Restoring an identical file
would undo no running-state repair.

The restore service rebuilds activation from saved files and service state.
No command text comes from the restore record. The interface name must
match the input's name pattern. With no previous configuration file, stop
and disable the service. For a previous file MMX can compare, use the forward
activation decision table in reverse: restart for wg-quick-only settings, addresses, MTU, or routes
that a reload cannot apply, and reload otherwise. Restore the service's
previous enabled and active state. Check the restored running interface
against the previous configuration file, or confirm absence for a previously
inactive service. A previous file may come from any writer, including a
first-boot script. Files MMX cannot safely compare use restart or stop and
checks of service state and interface existence.
MMX refuses a change over an existing file with `SaveConfig = true`, also
in a dryrun, because wg-quick would write the running settings over the
file that the node restores when the change is not confirmed.
MMX also refuses a `ListenPort` below 1024, before any change and in a
dryrun, when the kernel's loaded AppArmor policy enforces `wg-quick//ip`
and that child profile in `/etc/apparmor.d/wg-quick` has no capability
rule for `net_bind_service` or for every capability. `wg-quick` brings the
interface up through `wg-quick//ip`, which opens the port, so the start
would fail. Ubuntu 26.04's profile is such a profile, and its only local
include extends the parent profile, not the child. The check reads the
policy, so a fixed, changed, or disabled profile passes.

After configuration removal, the packaged service's `ExecStop` cannot read
`/etc/wireguard/<name>.conf`, so `ExecStop` fails and leaves the interface
up, while `systemctl disable --now` still exits 0. When an active service
is stopped with the destination absent, activation therefore runs
`wg-quick down` using a root-private copy of the saved configuration named
`<name>.conf`. The copy is in a directory of its own with mode `0700` in
`/etc/wireguard`, removed after `wg-quick down`, not in a private task
directory in `/run/mmx`: Ubuntu 26.04's AppArmor profile for `wg-quick`
lets `wg-quick` read files only in `/etc/wireguard`. Activation verifies interface removal and
clears the service's missing-file failure. A failed saved-file cleanup still
fails activation and starts restoration. No temporary file replaces the
removed destination.

The result's `reason` says what happened, such as `created the private
key, wrote /etc/wireguard/wg0.conf, started wg-quick@wg0.service` or
`reloaded wg-quick@wg0.service: added peer q1n8...=`. A restart says why it
was needed. A dryrun reports the comparison of the configuration file,
metadata, running interface, and service as a proposed change and
writes nothing; on a node without a key file, `public_key` is not yet
known.

`state: absent` removes the configuration file through the owed restore and
stops and disables the service, removing the interface and routes. Remove
the key file only after confirmation. A restore keeps the key file and puts
back the previous configuration and service state. A later `present` after
confirmed removal creates a new key.

Dryrun writes nothing and performs no confirmation wait. Dryrun states that
wg-quick acceptance and reachability after activation were not checked.

### Decisions

| Decision | Reason |
|---|---|
| One core module for wg-quick, named for the interface. | WireGuard between servers is common work, and wg-quick is what every distribution's package and most guides use. |
| The file whole, in wg-quick's format, through `content` or `template`. | The peer list comes from the inventory; a template can loop over groups, and task fields take references, not loops. The format already models peers. |
| The node creates its own key, and MMX adds it to the file. | The private key never travels, and a reload keeps it, as measured: with the key in a `PostUp` command, a reload left the interface without one. |
| Reload when the stripped settings changed, restart only for what a reload does not apply. | Measured on Debian 12 and Ubuntu 26.04: 240 pings at 50 ms across adding and removing a peer with a reload lost none, and the other peer's handshake stayed. A restart recreates the interface: about 1.2 seconds without traffic, and it ends the sessions, so a peer that sends first gets answers again only after its next handshake, 15.2 to 15.7 seconds later on the four target distributions in `wireguard_interfaces.py`. |
| Compare the running state, not the file text. | `wg showconf` resolves host names, orders peers by the kernel's list, and changes endpoints when peers roam, so a text comparison reports changes that are not there. |
| File changes use reachability confirmation and a durable owed restore. | SSH can run over the WireGuard interface. A wrong interface file can cut every connection off, so the node must restore without the controller machine. |
| Running-state repairs with an unchanged file do not wait for confirmation. | Restoring the identical file would undo no repair. |
| Keep a newly generated private key after restoration. | The next run must reuse the node's key. The key file is separate from the configuration file protected by the owed restore. |

### Tests the build needs

Go tests with stand-in `wg`, `wg-quick`, `ip`, and `systemctl`: validation
of names and files, key creation and reuse, the hidden key in differences
and saved copies, each compare outcome (file, peers, listen port,
addresses, MTU, routes, service state), the choice between start, reload,
and restart, the check after apply, dryrun with and without a key, and
removal. Also cover file-change handover and the confirmation duration,
metadata-only changes, runtime-only repairs without confirmation, restored
creation, reload-only and restart restores, previous service state, invalid
saved data, and key retention on restored removal. Confirmed removal must
remove the key only after acceptance. Check reuse of an externally written
base64 key with a newline, unchanged key bytes, the matching public key,
and correction of key metadata. Check dryrun creation, replacement, and
removal without any owed restore application. Refuse existing files with
`SaveConfig = true` in run and dryrun before any resource changes.
Refuse a `ListenPort` below 1024 under an enforced `wg-quick//ip` without
`net_bind_service` in run and dryrun before any change, and accept it with
a port of 1024 or above, a profile in complain mode, or a child profile
that grants the capability or every capability; a rule of the parent
profile does not count. The guest suite shows `wg-quick up` failing on
Ubuntu 26.04 without MMX, the refusal, and a working port 443 with the
profile disabled.
Restore first-boot files with a `PostUp` key command or settings MMX cannot
parse by restarting an active service and checking service state and
interface existence. Forward activation must
still reject invalid proposed files. Reject node task timing overrides and
ignore project timing defaults.

`wireguard_interfaces.py` runs in containers on the four target
distributions. Two guests run WireGuard, using one template over the
inventory to build a mesh. A third member has only an inventory entry.
The WireGuard suite has two guests because a job slot of the test harness
has two guests. Containers can hold a WireGuard interface when the test
server's kernel has the module.

The guest suite checks:

- a root-owned `0600` key written with `wg genkey` before MMX's first run
  is reused unchanged, with the matching `public_key`. A first-boot
  configuration using a `PostUp` key command is replaced while the running
  interface's public key stays unchanged;
- the second run is unchanged;
- adding and removing a peer with a reload while a ping runs between the
  two running guests, with no lost packet;
- an `Address` change that restarts the service;
- a peer removed by hand with `wg set` and put back by the next run;
- key replacement;
- removal.
