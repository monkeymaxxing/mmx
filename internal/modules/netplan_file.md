# `netplan_file`

`netplan_file` manages one Netplan YAML file on a **node**, the Linux server
MMX manages. It writes `/etc/netplan/<name>` and runs `netplan apply`.
Use `netplan_file` where Netplan uses the systemd-networkd renderer, the
network service Netplan generates configuration for. The node needs
`netplan`, `networkctl`, `systemctl`, and `ip`, with
`systemd-networkd.service` already active. MMX does not install or switch
network tools. The NetworkManager renderer is refused.

The module takes the whole file in Netplan's own format. Addresses, routes,
DNS, bridges, and VLANs stay in that format; MMX does not give them a second
set of fields. Three other modules do the same for the other network tools:
[`networkmanager_connection_file`](networkmanager_connection_file.md) for
NetworkManager, [`systemd_networkd_file`](systemd_networkd_file.md) for
systemd-networkd without Netplan, and
[`ifupdown_interfaces_file`](ifupdown_interfaces_file.md) for ifupdown.
[Choose the module for the node's tool](../../docs/changing-mmx/design/modules-network.md#choose-the-module-for-the-nodes-tool)
says which tool the measured test images use. The inputs work alike in all
four modules, as
[Inputs shared by all four modules](../../docs/changing-mmx/design/modules-network.md#inputs-shared-by-all-four-modules)
describes. For this module, `name` is one filename ending in `.yaml`,
without directories. It starts with a letter, digit, or underscore; the
remaining characters are letters, digits, underscores, dots, or hyphens.

A wrong network file can cut off your **controller machine**, where you run
`mmx`. Before changing the file, MMX saves its previous state and arms a
systemd timer on the node. The **controller process**, your running `mmx`
command, must confirm access through new SSH and HTTPS connections before
and after the change. Only the connection kinds that worked before must
work afterwards. An inventory using `connection: ssh_only` requires only SSH.
Without confirmation, the node restores the previous file and applies it
itself. The timer survives a disconnected controller machine, a killed
node job worker process, and a node restart. The node job worker process
is the program that carries out the node's tasks. The
[execution design](../../docs/changing-mmx/design/execution.md#reachability-confirmation-and-owed-restore)
owns this mechanism, reachability confirmation and owed restore.

## Examples

### A second card with one address

```yaml
fleet_stages:
  - id: configure_second_interface
    nodes: web01
    node_tasks:
      - netplan_file:
          name: 60-eth1.yaml
          content: |
            network:
              version: 2
              renderer: networkd
              ethernets:
                eth1:
                  dhcp4: false
                  addresses: [10.0.0.5/24]
          restore_unless_controller_confirms_within: 2m
```

On node `web01`, this writes `/etc/netplan/60-eth1.yaml` and applies it.
The second interface, `eth1`, must already exist. The example leaves the
interface used by the controller machine in its existing file.

### A second card in a bridge, with a VLAN and a second address

This complete **playbook**, a file listing work for nodes, puts a second
card in a bridge. A **fleet stage** selects nodes; each **node task** below
is one action on each selected node. Replace `enp6s0` with your second
card's name. Keep the main card's configuration in its existing file.
A bridge joins its attached cards into one network. A VLAN uses a numeric
tag to separate its traffic from other traffic on the same connection.

```yaml
fleet_stages:
  - id: configure_application_network
    nodes: web01
    node_tasks:
      - netplan_file:
          name: 60-application-network.yaml
          content: |
            network:
              version: 2
              renderer: networkd
              ethernets:
                enp6s0:
                  dhcp4: false
                  optional: true
              bridges:
                appbr0:
                  interfaces: [enp6s0]
                  addresses: [10.0.8.5/24, 10.0.8.6/24]
                  ignore-carrier: true
                  optional: true
                  parameters:
                    stp: false
              vlans:
                appvlan0:
                  id: 71
                  link: appbr0
                  addresses: [10.0.71.5/24]
                  ignore-carrier: true
                  optional: true
          restore_unless_controller_confirms_within: 2m
```

The node creates bridge `appbr0`, attaches `enp6s0` as its port, and gives
the bridge two addresses. It creates VLAN `appvlan0` on that bridge with
VLAN identifier 71 and its own address. The second address belongs to the
bridge; the physical card carries the bridge's traffic. Netplan can declare
several related devices like these in its one YAML file.

Run `mmx dryrun <playbook>` first, then `mmx run <playbook>`. An unchanged
file causes no generation, apply, or confirmation wait. Every actual apply
can briefly remove and restore addresses and routes on every card,
including the main card, even when only the second card's YAML changed.

### Remove the file

Removing the preceding example file removes the YAML, bridge `appbr0`, the
bridge's addresses, and VLAN `appvlan0`. The second card loses its bridge
membership. A container or VM using bridge `appbr0` loses the network
connection provided by `appbr0`.

```yaml
- netplan_file:
    name: 60-application-network.yaml
    state: absent
```

For removal, omit the content and template.

## How it works

A run follows the steps of
[What a run does](../../docs/changing-mmx/design/modules-network.md#what-a-run-does), which
all four network modules share. Removal follows
[What `state: absent` removes](../../docs/changing-mmx/design/modules-network.md#what-state-absent-removes),
and several files follow
[Several files run in playbook order](../../docs/changing-mmx/design/modules-network.md#several-files-run-in-playbook-order).
This section gives what is particular to Netplan.

### The file and its metadata

The file belongs to `root:root` with mode `0600`. MMX creates a missing
`/etc/netplan` directory as `root:root`, mode `0755`, and leaves an existing
directory alone. Files get the SELinux label of their destination.
A symlink, directory, or special file at the destination is refused.
The ordinary private-file rule hides the text difference because the file
is not readable by everyone. A secret in the content or template also
hides the node task's result text.

Each node task runs once; node task retries and both node task time limits
are refused. Project execution timing defaults do not apply to this module.

### Netplan checks the proposed configuration before installation

MMX makes a root-only copy of `/etc/netplan`, `/run/netplan` when present,
and `/lib/netplan` on the node, in the
[node task's private directory](../../docs/reference/secrets.md#a-node-tasks-private-directory). Regular-file symlinks are copied as regular
files, so no write of the proposed file reaches the live tree through a
link. Symlinks to directories and special files are refused. MMX removes
the private tree on success and failure. Generation never runs in dryrun.

1. MMX runs `netplan generate --root-dir <tree>` on the unchanged copy. It
   reads the copy's `run/systemd/network/10-netplan-*.netdev` and
   `*.network` files as the set before the change.
2. In the private copy, MMX writes the proposed YAML with mode `0600`, or
   removes the YAML for `state: absent`. MMX clears the private copy's
   generated output and generates again. A failed generation prevents
   installation.

   Netplan's own error names the live path, such as
   `/etc/netplan/60-application-network.yaml`, rather than the private
   copy. MMX keeps diagnostic message lines and omits echoed YAML and caret
   lines. A diagnostic in a format MMX does not recognize reports only the
   exit status. Activation and restoration use the same filtering. Warnings
   with a successful exit do not fail the node task.
3. MMX refuses before installation when this generation writes profiles
   under `run/NetworkManager/system-connections/`: a generated
   NetworkManager profile means the NetworkManager renderer.
4. MMX reads the set after the change with the same ordered settings reader
   that [`systemd_networkd_file`](systemd_networkd_file.md) uses, and
   compares the two sets of generated systemd-networkd files. It reads no
   Netplan YAML settings itself; there is no Netplan YAML model in MMX.
   Device ownership comes from the whole tree, including definitions shared
   by several YAML files. When several YAML files define a device, only a
   device the whole proposed tree no longer declares can be removed.
   Netplan itself rejects unresolved device references, such as removal of
   a bridge that another file's VLAN needs.

Generation is not a test that a card exists: it accepts a physical card
that does not exist.

On Ubuntu 24.04, generation in this copy also rewrites the live generated
files from the live YAML, as `systemctl daemon-reload` does. The candidate
never reaches the live tree. The live generated content stays the same
unless somebody edited live YAML without applying it. Systemd-networkd
reads that regenerated content on its next reload.

The same comparison refuses three changes before installation; the
[errors below](#errors-and-what-they-mean) give their texts and what to do:

- a changed creation setting of a device that the previous configuration
  declared, even when that device is currently absent from the node;
- a newly declared device whose name already exists on the node and was
  absent from the generated previous configuration;
- a change to the cards that an existing Netplan definition matches.

Changes that leave the generated device-creation settings identical remain
allowed, including file comments, permissions, and address changes.

### Activation

After installation, MMX uses only `netplan apply` to activate the proposed
configuration and to activate the previous configuration during restoration.
MMX checks command success, that obsolete devices are gone, and that cards
whose addresses MMX flushed have no global addresses.
There is no journal check or second check of configuration content after
activation. Fresh reachability confirmation follows the command-success,
device-removal, and address-removal checks. MMX never applies an unchanged
file.

### Devices and addresses that a change removes

Forward cleanup removes devices declared in the set before the change and
absent from the set after it. Restore cleanup removes devices new in the
set after the change that did not exist on the node before the change.
MMX saves both lists before installation. A veth pair includes both ends;
neither end may remove a protected peer. MMX never deletes a physical card
or another configuration file. Before every deletion, it checks that the
device's actual kernel path is below `/sys/devices/virtual/net/` and that
no live generated Netplan `.netdev` declares it. It deletes with
`networkctl delete <name>` and confirms that the device is gone. A changed
file that drops a device also requires a virtual kernel path, no live
generated Netplan `.netdev` declaration, and confirmation of deletion.

For a physical card, MMX remembers which generated configuration
disappears. MMX saves the generated `.network` filenames removed from the
set before the change. For restoration, MMX saves generated `.network`
filenames added by the proposed configuration. Before applying the proposed
configuration or restoring the previous configuration, MMX finds affected
links through `/run/systemd/netif/links/<index>`.
Each affected link's `NETWORK_FILE` names a generated file removed by the
configuration about to be applied. MMX selects only links whose
`ADMIN_STATE` is not already `unmanaged`. Cards that were already unmanaged
before the change are left alone. After apply and device deletion, MMX
clears all global addresses of such a card, with
`ip address flush dev <link> scope global`, only if systemd-networkd now
reports that card as unmanaged, and checks that it has no global address
afterwards. The kernel removes the associated routes with the addresses. A
card another configuration file takes over is left alone. This gives the
same address-removal behavior on systemd versions that otherwise leave
addresses behind.

Since apply can return before systemd-networkd finishes, this cleanup waits
up to ten seconds for each affected link to become unmanaged or receive
another configuration. Cancellation or an unresolved state fails the
cleanup and starts the restore. The restore timer continues to protect the
change during the wait for affected links to become unmanaged or receive
another configuration. Link indexes prevent cleanup from touching a
different link that reused the name.

### What restoration puts back

A restore puts back the previous file's bytes, owner, group, permissions,
and modification time, or removes a file that did not previously exist.
It gives systemd-networkd up to ten seconds to finish startup, with
`systemd-networkd-wait-online --timeout=10`, looked up under
`/usr/lib/systemd/` and then `/lib/systemd/`, ignoring its result. Then it
runs `netplan apply`. It removes newly introduced devices that did not
exist before the change and clears addresses from links left unmanaged by
the restore. Dynamic addresses and leases need not be identical snapshots.

A restored node task still fails: the proposed change was not kept.
A failed restore retains its recovery data and blocks another change to
that file. After fixing the cause, start the restore service named by
`mmx status`; see
[network recovery](../../docs/changing-mmx/design/execution.md#keep-the-owed-restore-until-it-is-resolved).

### Limits

There is no transaction across files, no switch between renderers, and no
move to a new address for the controller machine to discover.

## Errors and what they mean

The texts shared by all four network modules, such as an expired
confirmation, a restore that is still owed, and a failed restore, are in
[Results and failure reasons](../../docs/changing-mmx/design/modules-network.md#results-and-failure-reasons).
Netplan's own errors and MMX's refusals for Netplan are below. Every
refusal happens before MMX installs the proposed file.

| The reason holds | What the reason means, and what to do |
|---|---|
| `Cannot change /etc/netplan/60-eth1.yaml: netplan is not installed.` | A program the module needs is missing, or `systemd-networkd.service` is not running. Install it or start it in an earlier node task. A dryrun reports the node task not checked. |
| `Netplan rejected the network configuration while checking <path>:` | Netplan's generation failed. Netplan's own diagnostic follows, with the live path; see the table below. Correct the file. |
| `Cannot check the proposed <path> with Netplan:` | MMX could not perform the check, for example because the previous file changed during the check. |
| `Cannot change the settings of network device <name> in <path>: ...` | A device's creation settings changed; see [A changed device setting](#a-changed-device-setting). |
| `Cannot add <path>: network device <name> already exists and was not created by Netplan.` | The proposed YAML file declares a new device whose name already exists on the node. Netplan did not create the existing device, so a restore could not put the node back. |
| `Cannot change the network interfaces matched by Netplan definition <definition> in <path>: ...` | A definition's match changed; see [A definition moved to another card](#a-definition-moved-to-another-card). |
| `Cannot find the network interfaces configured by <path>:` | MMX could not read systemd-networkd's link state files to find the cards that lose their configuration. |

Netplan's diagnostics for the measured mistakes, each with exit code `1`:

| Mistake | Netplan's diagnostic |
|---|---|
| Unknown key | `Error in network definition: unknown key 'definitely_unknown_key'` |
| Bad address | `Error in network definition: address 'not-an-address' is missing /prefixlength` |
| Wrong format | `Error in network definition: expected mapping (check indentation)` |

### A changed device setting

MMX refuses changed creation settings for a previously declared device,
even when that device is currently absent from the node. For example,
changing a bridge's STP setting or a VLAN's identifier can succeed at apply
without changing the running device. The new setting would then take effect
at a later node restart, outside confirmation and restore. If the device is
absent, apply can create it with the new settings, which a restore could
not undo. The refusal explains the two-step procedure:

```text
cannot change the settings of network device appbr0 in /etc/netplan/60-application-network.yaml: systemd-networkd applies the settings only when creating the device, so a restore could not undo the change; first remove the device definition from the Netplan file and apply the file, then add the device definition with the new settings
```

First apply a file without the device and its dependent definitions.
After removal of the device and dependent definitions succeeds, add the
device again with the new settings.

MMX also refuses a newly declared device whose name already exists on the
node and was absent from the generated previous configuration:

```text
cannot add /etc/netplan/60-application-network.yaml: network device appbr0 already exists and was not created by Netplan
```

### A definition moved to another card

MMX refuses a change to the cards matched by an existing Netplan
definition: a generated `.network` filename exists before and after the
change, but its parsed `[Match]` settings differ. For example, changing the
match of an Ethernet definition named `application_lan` can move its
address from one card to another. The generated configuration keeps the
same filename. MMX could not identify which card loses that configuration,
so addresses could remain on the old card after the change or its restore.
The reason names the Netplan definition from the generated filename:

```text
cannot change the network interfaces matched by Netplan definition application_lan in /etc/netplan/60-application-network.yaml: MMX cannot determine which interface would lose configuration; give the definition a new name, or remove the definition and apply the file before adding the definition again
```

Give the definition a new name in the proposed file. Alternatively, apply
a file without the Netplan definition whose match must change, then add
the definition again after removal succeeds.

## Tested on

**Distributions:** Ubuntu 24.04 and 26.04.

The suite `tests/integration/netplan_files.py` runs on Ubuntu 24.04 and
Ubuntu 26.04 test guests using Netplan with the systemd-networkd renderer:

| Distribution | netplan.io | systemd |
|---|---|---|
| Ubuntu 24.04 | 1.1.2 (`1.1.2-8ubuntu1~24.04.3`) | 255.4 |
| Ubuntu 26.04 | 1.2 (`1.2-1ubuntu5.1`) | 259.5 |

The [validation record](../../docs/changing-mmx/validation.md#netplan-file-module) gives
the cases and the latest results. Not covered by the suite:

- Netplan with the NetworkManager renderer: the module refuses it.
- Tunnels and VRFs: Netplan generates them; no guest case creates one.
- In a guest: the refusal of a changed `match:` of a definition and the
  refusal of a changed setting of a declared device that does not exist.
  Go tests cover changed definition matches and changed settings of
  declared devices absent from the node.

## Why it works this way

- **Netplan checks a copy of the tree before installation.** On the Ubuntu
  24.04 and Ubuntu 26.04 guests, generation rejected unknown keys, bad
  addresses, and wrong YAML structure without changing the live network.
  Generation also rejected removing a bridge referenced by another file's
  VLAN without changing the live network.
- **The module reads Netplan's generated files, not its YAML.** Netplan
  already resolves definitions shared by several files, so MMX needs no
  second model of Netplan's format.
- **Only the systemd-networkd renderer.** Device removal and restoration
  depend on the generated systemd-networkd files.
- **Diagnostics lose echoed YAML and caret lines.** Another file's source
  line can contain secrets that the `netplan_file` node task never read.
- **Mode `0600`, enforced by MMX.** At mode `0644`, Netplan warned that
  configuration should not be accessible by others, but exited zero.
- **No apply of an unchanged file.** Applying an unchanged file briefly
  removes the main card's address and default route. A second-card change also
  interrupts the main card.
- **Changed device settings are refused.** Measured bridge STP changes and
  VLAN identifier changes were silently unapplied on both guests. Such
  changes could take effect at restart, outside this change's confirmation
  and restore. Netplan's own reference checks replace the separate
  reference refusals of `systemd_networkd_file`.
- **A changed match is refused.** The definition could move to another
  card while keeping its generated filename, and cleanup could then miss
  the card that loses its configuration, also during restoration.
- **MMX flushes the addresses of a card that becomes unmanaged.** Systemd
  249 left the measured physical card's address after apply and
  reconfigure; systemd 255 removed it. Systemd-networkd owned every global
  address on its managed link, so the flush gives the same result on both
  versions. Systemd 249 can also retain a stale filename on an unmanaged
  card; that card does not belong to the change, so it is left alone.
- **A restore waits up to ten seconds for startup before applying.** Waiting
  for systemd-networkd startup avoids activating restored configuration
  during initial card setup, as
  [`systemd_networkd_file`](systemd_networkd_file.md#why-it-works-this-way)
  explains.
- **No `netplan try`.** The measured `netplan try` timeout retained the
  changed YAML and did not reliably restore runtime IPv4. Restoring the YAML
  and applying recovered the original DHCP address and default route. Native
  measurements on Ubuntu 24.04 also restored a removed bridge, its address,
  and its port with apply alone. The MMX timer also covers process
  death and reboot, and two restoring mechanisms could conflict.
- **Removal deletes the file's virtual devices.** Netplan apply alone
  created and changed the second card's address and recreated bridges,
  bonds, VLANs, dummy devices, and veth pairs with their addresses, but
  removing their YAML left the devices behind.
