# `firewalld_zone_membership`

A firewalld **zone** is a set of firewall rules on a node. Its **members**
are the network interfaces and source addresses or networks whose incoming
traffic uses those rules. Membership selects rules; it does not add an
allow rule. `firewalld_zone_membership` manages which interfaces and IP
source networks belong to one zone, and leaves the rules alone. The
[`firewalld`](firewalld.md) module manages the rules, and
[`firewalld_zone`](firewalld_zone.md) creates a missing zone; create it
before assigning members.

Firewalld keeps two layers: the running firewall and the stored
configuration that it loads on a restart or reload. Membership must match
in both.

A zone such as `drop` can refuse new SSH and HTTPS connections while an
existing connection still works. So every changed membership task uses
**reachability confirmation**: the controller process opens new
connections before and after the change, and every kind that worked before
must work again. The node records the previous membership and arms a
systemd timer before changing anything. This **owed restore** remains until
confirmation succeeds or the node restores the previous membership. The
[execution design](../../docs/changing-mmx/design/execution.md#reachability-confirmation-and-owed-restore)
owns this mechanism; this module supplies membership data instead of a
file.

An interface that NetworkManager controls is refused: its zone belongs in
its NetworkManager profile, which
[`networkmanager_connection_file`](networkmanager_connection_file.md)
manages. See
[Put a card that NetworkManager controls in a zone](#put-a-card-that-networkmanager-controls-in-a-zone).

## Set up

Install firewalld with `dnf_package` and start it with `systemd_service`, as
the [`firewalld` page](firewalld.md#set-up) shows. Missing firewalld or a
stopped service is not checked in dryrun; either condition fails a normal
run.

## Examples

### Put a second card in a zone

This complete playbook assigns a second interface that NetworkManager does
not control to the existing `internal` zone:

```yaml
fleet_stages:
  - id: assign_firewall_interface
    nodes: web01
    node_tasks:
      - firewalld_zone_membership:
          zone: internal
          interfaces: [eth1]
          interfaces_not_listed: keep
          restore_unless_controller_confirms_within: 2m
```

A member already assigned to another zone **moves** into the requested zone.
An interface need not exist yet.

### Put source networks in a zone

This second playbook assigns source networks:

```yaml
fleet_stages:
  - id: assign_firewall_source_networks
    nodes: web01
    node_tasks:
      - firewalld_zone_membership:
          zone: internal
          source_networks: [10.0.0.0/8, '2001:db8::/32']
          source_networks_not_listed: keep
          restore_unless_controller_confirms_within: 2m
```

Use a network's starting address: `10.0.0.0/8` is valid; `10.0.0.1/8` is
refused.

### Put a card that NetworkManager controls in a zone

For a keyfile profile under `/etc/NetworkManager/system-connections/`, put
`zone=internal` in its `[connection]` section. Preserve the actual profile's
other settings and UUID. This complete example configures a second interface
and its firewall zone together:

```yaml
fleet_stages:
  - id: configure_interface_and_firewall_zone
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
            zone=internal

            [ipv4]
            method=manual
            address1=10.0.0.5/24

            [ipv6]
            method=disabled
          restore_unless_controller_confirms_within: 2m
```

Remove the `zone=` line through that same module to return the interface
to NetworkManager's default zone choice. If the profile is an old-format
`ifcfg-*` file, use the command in the refusal to convert that one profile:

```sh
nmcli connection migrate --plugin keyfile uuid <profile-uuid>
```

The refusal supplies that profile's UUID. Then manage the resulting
`.nmconnection` file. If NetworkManager has no profile file
below `/etc/`, the refusal explains that NetworkManager made the profile
itself. Give the interface a persistent profile file first. The
[`networkmanager_connection_file` page](networkmanager_connection_file.md#how-it-works)
describes how that module applies changes and restores previous settings.

## How it works

### Members and the members that are not listed

For `present`, supply at least one of `interfaces` and `source_networks`.
Each list has its own required policy: `interfaces_not_listed` with
`interfaces`, and `source_networks_not_listed` with `source_networks`.
`keep` leaves unlisted members of that kind alone. `remove` removes every
unlisted member of that kind from this zone. An omitted list stays unchanged.
To remove every member, write both lists as `[]` and both policies as `remove`.

`state: absent` removes the listed members from this zone only. Omit both
policies with `absent`. At least one member is required for `absent`.

The zone name and individual list entries may use values from variables.
Addresses follow the source validation and normalization of the
[`firewalld`](firewalld.md) module. Repeated members are refused, including
equivalent spellings of an address. MAC address sources and ipsets are not
supported. Removing unlisted sources is refused, before any change, if the
zone contains either kind; remove that source with firewalld first. A task
that keeps unlisted members leaves them alone.

Both the running firewall and the stored configuration must contain the
zone. A missing-zone refusal names the layer and `firewalld_zone`.

### A run, step by step

`restore_unless_controller_confirms_within` must be a positive duration.
The node reads both layers with `firewall-cmd --list-all-zones`. Before the
change, the node requests new connections, durably records the previous
membership, and arms the timer. Every changed member is recorded with its
previous and proposed binding in each layer, including no explicit binding.
A layer already correct before the task is still recorded: a reboot may
copy the proposed stored configuration into the running firewall before
restoration runs. The original spelling of a source is kept for native
commands.

Additions and moves happen before removals. Each member changes in the
running firewall and then in the stored configuration. A member already in
another zone moves with `--change-interface` or `--change-source`;
`--add-source` cannot move a bound source. The module never reloads
firewalld or changes rules. It reads both layers after the commands and
fails if any requested membership still differs. Then it requires a new
connection of every kind that worked before the change.

An unchanged task writes no record. It changes no membership and does not
wait. These tasks reject `retry`, `time_limit`, and `time_limit_per_try`;
the confirmation duration is their own limit.

### Interfaces that NetworkManager controls

The node checks NetworkManager before any change: for additions, moves, and
removals, also for removal of unlisted members, and in dryrun. If `nmcli`
exists and NetworkManager runs, a nonempty active connection UUID from
`nmcli -g GENERAL.CON-UUID device show <interface>` means that
NetworkManager controls the interface. A missing device, or one without an
active profile, goes through firewalld.
`nmcli -t -f UUID,FILENAME connection show` supplies the profile's
filename. If `nmcli` is missing, systemd must report NetworkManager as
inactive or failed. MMX refuses interface changes if NetworkManager is
active or its state cannot be established: a failed inspection does not
establish that an interface is unmanaged.

A permanent firewalld membership command delegates to NetworkManager and
rewrites that interface's profile, including its zone setting. That file
belongs to `networkmanager_connection_file`, so this module refuses the
interface before any change. The refusal names the interface, the
profile's path, the owning module, and the `zone=` line to write or remove
in `[connection]`. MMX does not write or convert profile files through this
module. Ownership is checked again in the activation and restore processes.

### What a restore puts back

Without all required confirmations in time, or after an activation
failure, a killed worker, or a node restart, the node restores each changed
member's previous zone. If the member had no zone before, the node removes
it from its current zone. It restores the running firewall and stored
configuration separately: they may have differed before the change. It
issues individual native commands, not a reload. After a reload, the
current spelling of a source can differ from the saved spelling; the
module removes the superseded spelling before adding the saved one, then
checks both spelling and zone. It does not restore rules or other members.
The timer works without the controller process or node job worker process.

The shared mechanism stores this module's data with no destination file or
saved contents: each member's bindings, the change subject,
`firewalld.service` for the ordering of the restore, and the sentence for a
successful restore. It does so without knowing firewalld. Both binding
lists of a member must hold exactly the two layers, each with an explicit
zone and spelling; incomplete recovery data is rejected before any command.

After a node restart, the restore service runs without waiting for the
deadline. The restore service has the ordering `After=firewalld.service`,
so it runs after firewalld if firewalld starts. MMX does not start or
enable firewalld. If firewalld is stopped, the restore fails and remains
owed. On the node, run `systemctl start firewalld.service`, then start the
restore service named by `mmx status`.

If a previous zone is missing, the failure gives the commands to repair it
on the node. For example, if the stored zone `appzone` is missing:

```sh
firewall-cmd --permanent --new-zone=appzone
firewall-cmd --reload
```

The reload replaces every zone's running settings with its stored settings.
Then start the restore service named by `mmx status`.

A restored task still reports failure. If restoration fails, correct the
reported cause and start the exact restore service named in the result:

```sh
# Run as root on the node, using the identifier from its result.
systemctl start mmx-owed-restore-0123456789abcdef.service
journalctl -u mmx-owed-restore-0123456789abcdef.service
```

`mmx status` refreshes the restore outcome. Only one membership change may
remain owed on a node; another membership task is refused, even in dryrun
or when its desired membership already matches. Zone creation and deletion
with [`firewalld_zone`](firewalld_zone.md) also refuse while membership
restoration or cleanup remains owed: their reload could disturb that
recovery.

### Results

Results use `status`, `change`, and `reason`. The `value` groups changes
under `running_firewall` and `stored_config`. Each has `added`,
`moved`, and `removed` lists when nonempty. Each entry gives `member_kind`
(`interface` or `source_network`) and `member_name`. A moved entry also
gives `previous_zone`. Dryrun returns the same proposed value with
`would_change`, and states that reachability after the change was not
checked. Neither an unchanged task nor a dryrun starts a timer.

A confirmed task returns `changed`. A restored task is failed and changed.
If a command is interrupted, MMX may not know which membership changed. In
that case, the change is reported as `unknown`. Failed tasks omit these
lists, including when their changes were restored. Their reasons say
whether restoration succeeded. The `owed_restore` outcome names the service
and records its latest result.

## Errors and what they mean

| What fails | What it means, and what to do |
|---|---|
| The zone is missing, naming the layer and `firewalld_zone` | Create the zone with a [`firewalld_zone`](firewalld_zone.md) task first. |
| An interface is refused, naming its profile's path and the `zone=` line | NetworkManager controls the interface. Set `zone=` in its profile instead, as in [Put a card that NetworkManager controls in a zone](#put-a-card-that-networkmanager-controls-in-a-zone). |
| The refusal names `nmcli connection migrate --plugin keyfile uuid <profile-uuid>` | The interface's profile is an old-format `ifcfg-*` file. Convert that one profile, then manage the `.nmconnection` file. |
| The refusal says that NetworkManager made the profile itself | The profile has no file below `/etc/`. Give the interface a persistent profile file first. |
| Removing unlisted sources is refused | The zone holds a MAC address source or an ipset. Remove that source with firewalld first. |
| Another membership task is refused while a restore is owed | Only one membership change may remain owed on a node. Resolve the owed restore first. |
| The restore fails because firewalld is stopped | Run `systemctl start firewalld.service`, then start the restore service named by `mmx status`. |
| The restore fails because a previous zone is missing | Run the repair commands that the failure gives, then start the restore service. |

## Tested on

**Distributions:** Rocky Linux 9, AlmaLinux 10.

The suite `tests/integration/firewalld_zone_memberships.py` uses firewalld
from each distribution's own package and a second network card that is
connected to nothing. It passes on both of its guests:

| Distribution | What manages the network | firewalld |
|---|---|---|
| Rocky Linux 9 | NetworkManager | 1.3.4 |
| AlmaLinux 10 | NetworkManager | 2.4.3 |

The
[validation record](../../docs/changing-mmx/validation.md#firewalld-zone-modules) gives
the cases and the latest results. Not covered by the suite:

- A card that NetworkManager shows as connected "externally", such as the
  bridge of a container tool.
- firewalld's other source kinds, MAC addresses and ipsets: the module
  does not manage them, and refuses to remove them as unlisted members.

[Limits on each distribution](../../docs/reference/limits-on-each-distribution.md) says what is limited for this module on [Fedora 45](../../docs/reference/limits-on-each-distribution.md#fedora-45), and why.

## Why it works this way

- **Each membership list has its own policy.** An interface-only task must
  not delete source networks. The policies follow
  [leaving a setting out never deletes](../../docs/changing-mmx/design/modules.md#leaving-a-setting-out-never-deletes).

- **New connections confirm the change.** A zone such as `drop` can refuse
  every new connection while an existing connection still works.
- **Both layers are recorded and restored separately, by individual
  commands.** A reboot may copy the proposed stored configuration into the
  running firewall before restoration runs, and restoring by reload would
  lose a differing previous runtime binding.
- **Interfaces that NetworkManager controls are refused.** A permanent
  firewalld command would rewrite the profile file that
  `networkmanager_connection_file` owns.
- **One owed membership restore per node, and no zone reload while it is
  owed.** A reload could disturb that recovery. The repair message for a
  missing zone therefore gives node commands, not the blocked
  `firewalld_zone` module.
