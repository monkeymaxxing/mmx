# `networkmanager_connection_file`

`networkmanager_connection_file` manages one NetworkManager keyfile on a
node, the Linux server that MMX manages. It installs the file, asks
NetworkManager to load and activate its connection profile, and checks the
service's response. A profile is NetworkManager's stored configuration for a
connection, such as an Ethernet card, bridge, or VLAN. The file is
`/etc/NetworkManager/system-connections/<name>`, with `name` ending in
`.nmconnection`.

The module takes the whole keyfile in NetworkManager's own format.
Addresses, routes, DNS, bridges, and VLANs stay in that format; MMX does not
give them a second set of fields. Three other modules do the same for the
other network tools: [`netplan_file`](netplan_file.md) for Netplan,
[`systemd_networkd_file`](systemd_networkd_file.md) for systemd-networkd, and
[`ifupdown_interfaces_file`](ifupdown_interfaces_file.md) for ifupdown.
[Choose the module for the node's tool](../../docs/changing-mmx/design/modules-network.md#choose-the-module-for-the-nodes-tool)
says which tool the measured test images use. The inputs work alike in all
four modules, as
[Inputs shared by all four modules](../../docs/changing-mmx/design/modules-network.md#inputs-shared-by-all-four-modules)
describes.

The controller process, the running `mmx` command on the controller machine,
confirms reachability over new connections before and after each change.
The node saves the previous file and arms an independent restore service.
If activation fails or confirmation does not arrive in time, the node
restores itself, even when the controller process stops or the node restarts.
This protects one file at a time; several files are separate node tasks.
The [execution design](../../docs/changing-mmx/design/execution.md#reachability-confirmation-and-owed-restore)
owns this mechanism, reachability confirmation and owed restore.

**On Rocky Linux 9, convert an old-format profile first.** There the main
card uses an `ifcfg-*` profile under `/etc/sysconfig/network-scripts/`. MMX
refuses a new file for that interface while any loaded old-format profile
names it, including an inactive profile, in dryrun too. MMX does not open or
convert the old file. First:

1. Find the profile's UUID and file with
   `nmcli -t --escape no -f UUID,FILENAME connection show` on the node.
2. Run `nmcli connection migrate --plugin keyfile uuid <uuid>` on the node.
   This is NetworkManager's own conversion command.
3. Read the resulting `.nmconnection` file. Put its name and complete content
   into the playbook, the file of fleet stages and node tasks that MMX runs.
4. Run MMX again.

The measured migration keeps the UUID and creates a name such as
`System enp5s0.nmconnection`, including the space. It preserves the working
IPv4 connection. The refusal gives the exact migration command for the
conflicting profile. On version 10 the main card's profile is already a
`.nmconnection` file, and a second interface that the distribution did not
configure has no old-format profile.

## Examples

These examples are node tasks under a playbook's `node_tasks`, except the
first, which is a whole playbook. Replace `eth1` with your second card's
name. Keep the main card's connection intact.

### A second card

```yaml
fleet_stages:
  - id: configure_second_interface
    nodes: web01
    node_tasks:
      - networkmanager_connection_file:
          name: eth1.nmconnection
          content: |
            [connection]
            id=eth1
            uuid=6a26c67e-e29f-48c8-9069-1cc1687b3ac9
            type=ethernet
            interface-name=eth1
            autoconnect=true

            [ipv4]
            method=manual
            address1=10.0.0.5/24

            [ipv6]
            method=disabled
          restore_unless_controller_confirms_within: 2m
```

On node `web01`, this loads the file from
`/etc/NetworkManager/system-connections/` and brings its connection up on
`eth1`. The UUID identifies this connection to NetworkManager and stays
the same when the file changes.

A UUID is optional:

```yaml
- networkmanager_connection_file:
    name: second.nmconnection
    content: |
      [connection]
      id=second
      type=ethernet
      interface-name=eth1
      autoconnect=true
      [ipv4]
      method=manual
      address1=10.0.0.5/24
      [ipv6]
      method=disabled
```

NetworkManager gives a UUID-less file a stable UUID at that path; MMX finds
the profile by its filename after loading it. The measured UUID-less file
kept the same UUID after reload and service restart at the same path. A
renamed file is not promised the same generated UUID. Remove this example
with:

```yaml
- networkmanager_connection_file: {name: second.nmconnection, state: absent}
```

`state: absent` removes the named file, its profile's running configuration
(such as an address or route), and its virtual device when no other loaded
profile declares it. Physical cards remain. Another stored profile may then
activate on a freed physical card.

### A bridge with a port

Create the bridge first, then its port profile. The port below points to
the bridge's explicit UUID. Remove any existing second-card profile from
the preceding example before applying these tasks.

```yaml
- networkmanager_connection_file:
    name: app-bridge.nmconnection
    content: |
      [connection]
      id=app-bridge
      uuid=cfd57490-6bcd-4d96-b582-03d0643d3785
      type=bridge
      interface-name=appbr0
      autoconnect=true
      [bridge]
      stp=false
      [ipv4]
      method=manual
      address1=10.0.0.5/24
      [ipv6]
      method=disabled
- networkmanager_connection_file:
    name: app-port.nmconnection
    content: |
      [connection]
      id=app-port
      type=ethernet
      interface-name=eth1
      controller=cfd57490-6bcd-4d96-b582-03d0643d3785
      port-type=bridge
      autoconnect=true
      [ipv4]
      method=disabled
      [ipv6]
      method=disabled
```

Remove in reverse order:

```yaml
- networkmanager_connection_file: {name: app-port.nmconnection, state: absent}
- networkmanager_connection_file: {name: app-bridge.nmconnection, state: absent}
```

Removing a bridge file removes its address and the bridge itself. Its port
profiles remain stored, and physical cards remain. A container or VM
attached to that bridge loses its connection.

### A VLAN on a second card

Configure the parent first, then the VLAN. The parent example retains its
own address while the VLAN has a separate address. These tasks replace the
bridge example; remove its two files first.

```yaml
- networkmanager_connection_file:
    name: second.nmconnection
    content: |
      [connection]
      id=second
      type=ethernet
      interface-name=eth1
      [ipv4]
      method=manual
      address1=10.0.0.5/24
      [ipv6]
      method=disabled
- networkmanager_connection_file:
    name: app-vlan.nmconnection
    content: |
      [connection]
      id=app-vlan
      type=vlan
      interface-name=appvlan77
      [vlan]
      parent=eth1
      id=77
      [ipv4]
      method=manual
      address1=10.77.0.5/24
      [ipv6]
      method=disabled
```

Remove `app-vlan.nmconnection` first with `state: absent`, then
`second.nmconnection`. Do not remove a shared parent profile that other
connections still need.

### A second address on the same card

Keep the existing address while adding another in the same file:

```yaml
- networkmanager_connection_file:
    name: second.nmconnection
    content: |
      [connection]
      id=second
      type=ethernet
      interface-name=eth1
      [ipv4]
      method=manual
      address1=10.0.0.5/24
      address2=10.0.0.6/24
      [ipv6]
      method=disabled
```

Remove just `address2` to remove the second address. Removing the file with
`state: absent` removes both addresses. Never remove the address the
controller machine uses unless losing confirmation and restoring is intended.

### A firewalld zone for the card

A firewalld zone for an interface that NetworkManager controls belongs in
the profile, as `zone=<zone>` in its `[connection]` section, not in a
[`firewalld_zone_membership`](firewalld_zone_membership.md) task. That
page's example
[Put a card that NetworkManager controls in a zone](firewalld_zone_membership.md#put-a-card-that-networkmanager-controls-in-a-zone)
shows the file.

## How it works

A run follows the steps of
[What a run does](../../docs/changing-mmx/design/modules-network.md#what-a-run-does), which
all four network modules share. Removal follows
[What `state: absent` removes](../../docs/changing-mmx/design/modules-network.md#what-state-absent-removes).
This section gives what is particular to NetworkManager.

### The file and its name

The file always belongs to `root:root` with mode `0600`; NetworkManager
refuses insecure permissions. The installed and restored file receives its
destination's SELinux label. The name is one component, does not begin
with a dot, and contains no path separator or control character. Spaces
and other filename characters are allowed. Commands pass paths as
individual arguments; the restore's systemd units are named after the
owed restore id, not the filename.

Node task `retry`, `time_limit`, and `time_limit_per_try` are refused. An
unchanged file causes no activation or confirmation wait. Each file must
have its own UUID: a declared UUID already loaded from another path is
refused before anything changes.

### Checks before installation

Before installation, including in dryrun, MMX gives the candidate to
`nmcli --offline connection modify connection.id <filename>` through
standard input. The ID used for this read is the filename; the installed
bytes remain exactly those supplied. A nonzero exit fails before any change
with NetworkManager's own words. This checks malformed keyfiles and invalid
addresses, but unknown keys and unknown sections can pass.

MMX reads `connection.type` and `connection.interface-name` from nmcli's
canonical keyfile output. From the original text it reads only whether
`[connection]` has a `uuid` assignment, since the offline output may supply
an otherwise undeclared UUID.

MMX lists loaded UUIDs and filenames with
`nmcli -t --escape no -f UUID,FILENAME connection show`, splitting each line
at its first colon, and refuses an explicitly declared UUID loaded from
another path. It queries the configured interface names of all loaded
old-format profiles, including inactive profiles, without opening their
files. A match with the proposed interface is refused, naming the old file
and the migration command for that profile. MMX asks NetworkManager which
profile and backing filename belong to the interface; it never opens,
writes, or converts an `ifcfg-*` file. A query failure fails before
changes: when the inspection cannot establish whether a conflict exists,
nothing changes. A candidate without `interface-name` cannot be checked for
this conflict; specify the interface when you want this protection.

### Activation and its checks

After installation, MMX takes a journal position and runs
`nmcli connection load <path>`. Any output or a nonzero exit fails; a load
can print `Could not load file '<path>'` and still exit zero, keeping old
cached settings. MMX then verifies that the profile is loaded from that
path. Removal loads the missing path to unload that one profile, and
requires the path to be absent from the list. MMX never reloads all profile
files with `nmcli connection reload`: it imports unrelated disk edits.

Then MMX synchronizes the journal and checks new warnings or errors of
`NetworkManager.service`. It runs `journalctl --sync` and reads from its
position with `--cursor`, discarding only the entry at that position. It
refuses the check at 200 entries, or on a truncated or undecodable read. An
entry of priority 0 through 4 fails when it names the exact path, or when
`keyfile:` follows the native level and timestamp prefix, such as
`<warn>  [1791901610.2941] keyfile: ...`. The targeted load reads only this
file, but the measured unknown-section warning names no file. Unrelated
warnings, such as DHCP for another card, are ignored. An unreadable or
incomplete check fails.

Only then, for a present file, does MMX run
`nmcli connection up uuid <uuid>` and require exit zero. It does not add
`--wait`: nmcli's own limit applies, and the restore timer bounds
activation.

### Virtual devices

NetworkManager normally removes obsolete virtual devices itself. A named
device belongs to MMX's cleanup only when its sysfs path resolves below
`/sys/devices/virtual/net/`. After an unload or an interface rename, MMX
waits up to five seconds for the automatic removal. If the device remains
virtual and no other loaded profile names it, MMX runs
`nmcli device delete <name>` and checks that it is gone. It checks the
virtual status again immediately before deletion. Failure to remove it
fails the task. MMX never deletes a physical card. A restore also removes a
device introduced by this file that did not exist before the change.

Changed device settings and references need none of
[`systemd_networkd_file`](systemd_networkd_file.md)'s refusals:
NetworkManager applies the measured changes.

### What a restore puts back

A restore returns the file, and it activates again every profile that was
activated before the change. Before the change, MMX saves whether the
previous file had a loaded profile, whether that profile was activated, and
every activated profile UUID. Only device state 100 counts: a profile still
connecting, such as one obtaining DHCP, does not count as active.

The restore puts back the previous bytes, owner, group, permissions, and
modification time, or removes a newly added file. It then loads the
restored path. The load command must exit zero. If the previous file had no
loaded profile, the restore does not require NetworkManager to accept that
file, with empty load output or an entry in the profile list: a broken file
can return as it was. A previously loaded file and a restored absence keep
the normal checks of the load output and the filename.

MMX activates this file's previously active profile. If it had been
inactive, MMX stops any current activation of it and checks that it is
inactive. MMX then activates each other previously active profile that is
still loaded and has not returned. It checks again before each activation,
because an earlier command may have brought another profile back. A
temporary profile that disappeared after reboot is skipped. Restoration
skips the journal check: an old warning must not prevent recovery.

This rule covers a displaced profile, a bridge's port profiles, and a VLAN
on a removed bridge, without discovering dependencies between profiles.
Loading and activating a restored bridge alone does not reconnect its
ports, even when their profiles allow automatic connections. Other profile
files are not changed.

A restore does not undo other files, recreate containers, or reproduce
dynamic DHCP leases exactly. A failed restore keeps the saved state; correct
the cause, then start the restore service named by `mmx status <run-id>` on
the node.

### What the checks do not prove

The checks do not prove that NetworkManager understood every setting. The
measured unknown key in `[ipv6]` produced no warning and was accepted.
Reachability proves new connections at the existing node address, not that
all application traffic works. Add your application's own checks.

## Errors and what they mean

The texts shared by all four network modules, such as an expired
confirmation, a restore that is still owed, and a failed restore, are in
[Results and failure reasons](../../docs/changing-mmx/design/modules-network.md#results-and-failure-reasons).
The texts particular to NetworkManager:

| The reason holds | What it means, and what to do |
|---|---|
| `Cannot configure interface eth1 with /etc/NetworkManager/system-connections/eth1.nmconnection: the old-format profile /etc/sysconfig/network-scripts/ifcfg-eth1 is still loaded. Run nmcli connection migrate --plugin keyfile uuid <uuid> on the node, read the resulting file, then put its name and content into the playbook.` | An old-format profile names the same interface. Convert it as the [introduction](#networkmanager_connection_file) says. This fails in dryrun too. |
| `NetworkManager does not accept the proposed <path>: <native diagnostic>.` | The offline check rejected the candidate, such as a malformed keyfile or a bad address. Nothing changed. |
| `Cannot add <path>: profile UUID <uuid> is already loaded from <other-path>. Each file must have its own profile UUID.` | Another file has the same UUID. Give this file its own UUID, or none. |
| `NetworkManager did not accept /etc/NetworkManager/system-connections/eth1.nmconnection: <tool diagnostic>. The previous file and its running configuration are restored.` | The load or the journal check after installation failed, such as for an unknown section. The node restored the previous file at once. |
| `NetworkManager could not activate the profile of <path>: <native diagnostic>.` | `nmcli connection up` failed, such as for a card that does not exist. The node restores. |
| `NetworkManager could not activate profile <uuid> again after the restore of <path>: <native diagnostic>.` | A restore put the file back, but another previously active profile did not come up again. |
| `Cannot check whether NetworkManager accepted <path>: <diagnostic>.` | MMX could not read the journal completely. The check fails, and the node restores. |

## Tested on

**Distributions:** Rocky Linux 9, AlmaLinux 10.

The suite `tests/integration/networkmanager_connection_files.py` runs on
the two test guests that use NetworkManager, both with SELinux enforcing:

| Distribution | NetworkManager |
|---|---|
| Rocky Linux 9 | 1.54.3 |
| AlmaLinux 10 | 1.56.0 |

The old-format case has nothing to prove on version 10, where no old-format
profile exists. The
[validation record](../../docs/changing-mmx/validation.md#networkmanager-connection-file-module)
gives the cases and the latest results. Not covered by the suite:

- Wi-Fi, VPN, team, and other profile kinds than Ethernet, bridge, bond,
  VLAN, and dummy.
- A profile without an interface name.
- A card that NetworkManager shows as connected "externally".

[Limits on each distribution](../../docs/reference/limits-on-each-distribution.md) says what is limited for this module on [Fedora 45](../../docs/reference/limits-on-each-distribution.md#fedora-45), and why.

## Why it works this way

- **MMX does not convert old-format profiles.** This module changes only the
  file it owns, and an old-format file belongs to the distribution. It
  refuses the conflict, in dryrun too, so a user of a version 9 node sees
  the conversion before the first change.
- **MMX checks the load and the activation, not only the exit code.**
  NetworkManager can return zero after rejecting content.
- **MMX loads one path, never all profiles.** On NetworkManager 1.54 through
  1.58, the targeted load rejects insecure permissions, may return zero
  after rejection, and unloads a removed path without reading unrelated
  edits; `nmcli connection reload` would import those edits.
- **Unknown keys stay a visible limit.** The offline check rejects malformed
  keyfiles and bad addresses, and unknown sections produce attributable
  keyfile-reader warnings, but the tested unknown IPv6 key is silently
  accepted. MMX makes no claim that NetworkManager rejects unknown keys.
- **The restore activates the profiles that were active before.** File
  restoration plus load and up restores the main card, and removing an
  added competing profile let the prior profile reconnect automatically in
  the measured cases. But on NetworkManager 1.54.3 and 1.56.0, restoring a
  bridge did not reconnect its previously active port, even with port
  autoconnection enabled; explicit port activation did.
- **No NetworkManager checkpoints.** The measured checkpoint retained an
  added file. The MMX timer also covers process death and reboot, and two
  restoring mechanisms could conflict.
- **Removal deletes the file's virtual device.** NetworkManager removed the
  measured bridge, bond, VLAN, and dummy itself after file removal and a
  targeted unload, and a dummy rename removed the old device in both
  directions. MMX's own deletion covers a device that remains, so the
  removed devices are gone, not merely the configuration text.
