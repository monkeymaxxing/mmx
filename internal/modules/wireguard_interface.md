# `wireguard_interface`

`wireguard_interface` manages one WireGuard interface on a node, such as
`wg0`, which joins the node to a private network with other servers. MMX
writes the interface's file `/etc/wireguard/<name>.conf` and brings the
interface up with the node's systemd service `wg-quick@<name>.service`, which
runs `wg-quick`, the tool of the package `wireguard-tools`.

Each node has a private key in `/etc/wireguard/<name>.private_key`.
MMX reuses an existing key, including a key supplied by a first-boot script.
When the key file is absent, the node creates a key. Private keys never
leave the node. The
task's result field `public_key` holds the matching public key, which the
other nodes need in their files. You write it into the inventory once, and a
template builds every node's list of peers from there.

The file is wg-quick's own format, the one its manual and most guides show.
The task gives the whole file without its `PrivateKey` line, through
`content` or `template`; MMX adds that line.

## Set up

`wg`, `wg-quick`, and `wg-quick@.service` come with the package
`wireguard-tools` on Debian and Ubuntu. Install `wireguard-tools` first:

```yaml
- apt_package:
    packages:
      - {name: wireguard-tools, version: newest_when_first_installed}
```

Until `wireguard-tools` is installed, a dryrun reports the
`wireguard_interface` node task not checked, because an earlier node task
may install `wireguard-tools`. A run without `wireguard-tools` fails with a
message that names `wireguard-tools`. The kernels of Debian 12 and 13
and Ubuntu 24.04 and 26.04 have WireGuard.

The nodes must accept UDP on the interface's `ListenPort` from each other,
for example with a [`ufw`](ufw.md) rule. Choose a network for the WireGuard
interface that does not overlap any network already used by the node. An
`Address` in the network of the node's existing network card routes traffic
for the existing network through WireGuard, including traffic to the gateway.
The node then loses the connection to the controller machine because
traffic to the gateway goes through WireGuard.

## Examples

### A private network between servers, from the inventory

Every node of the group `wireguard_mesh` gets the interface `wg0`, with one
`[Peer]` section for each other node of the group. Each node's private
address and public key are its own variables in the inventory:

```yaml
# inventories/production.yml
nodes:
  web01:
    address: 203.0.113.11
    vars:
      wireguard_address: 10.0.0.1
      wireguard_public_key: PFHMlKh61wvN0/mEnwE2mCdTk3PCQmr/u0lh3F8JkT0=
  web02:
    address: 203.0.113.12
    vars:
      wireguard_address: 10.0.0.2
      wireguard_public_key: xntON0r+ip4v53R2PDYOEKeq0RH91Y9x307uPnUQOTE=
  db01:
    address: 203.0.113.21
    vars:
      wireguard_address: 10.0.0.3
groups:
  wireguard_mesh:
    nodes: [web01, web02, db01]
```

The Go template file is stored on the controller machine. MMX makes the
inventory available to the template: `.inventory.groups.wireguard_mesh` is
the list of the group's node names, and `.inventory.nodes.<name>` holds each
node's `address` and `vars`.
A node whose public key is not in the inventory yet, here `db01`, gets no
`[Peer]` section on the other nodes in `wireguard_mesh`:

```text
# templates/wg0.conf.tmpl
[Interface]
Address = {{ .vars.wireguard_address }}/24
ListenPort = 51820
{{ range $name := .inventory.groups.wireguard_mesh }}
{{- $peer := index $.inventory.nodes $name }}
{{- if and (ne $name $.node.name) (hasKey $peer.vars "wireguard_public_key") }}

[Peer]
# {{ $name }}
PublicKey = {{ $peer.vars.wireguard_public_key }}
AllowedIPs = {{ $peer.vars.wireguard_address }}/32
Endpoint = {{ $peer.address }}:51820
{{- end }}
{{- end }}
```

```yaml
# playbooks/private_network.yml
fleet_stages:
  - id: private_network
    nodes: wireguard_mesh
    node_tasks:
      - apt_package:
          packages:
            - {name: wireguard-tools, version: newest_when_first_installed}
      - id: wireguard
        wireguard_interface:
          name: wg0
          template: templates/wg0.conf.tmpl
```

`mmx run private_network` renders the template separately on each node in
`wireguard_mesh`, using the node's values. MMX writes
`/etc/wireguard/wg0.conf` on each node. On `db01`, which has no key yet, MMX
creates the private key and adds the private key to `/etc/wireguard/wg0.conf`.
MMX then enables and starts `wg-quick@wg0.service` and reports the public key:

```sh
mmx results --json | jq -r '.entries[] | .node + " " + (.response.results[] | select(.task == "wireguard") | .public_key)'
```

It prints one line for each node of the run, such as `db01 urzzcMKwVZ9bM84MmgWUjMRxOTNn2PDtEF5it/uZFQ4=`.

Add that key to `db01`'s `vars` as `wireguard_public_key` and run the
playbook again. `web01` and `web02` now get a `[Peer]` section for `db01`,
and MMX reloads their interfaces: the reload adds the peer and keeps the
sessions with the other peers. `db01` gets its two peers the same way. A run
after that changes nothing. A later node task can also read the key as
`{{ .node_tasks.wireguard.public_key }}`.

### One interface written out

A server with one peer, with the text for `/etc/wireguard/wg0.conf` written
in the task:

```yaml
- wireguard_interface:
    name: wg0
    content: |
      [Interface]
      Address = 10.0.0.1/24
      ListenPort = 51820

      [Peer]
      PublicKey = xntON0r+ip4v53R2PDYOEKeq0RH91Y9x307uPnUQOTE=
      AllowedIPs = 10.0.0.2/32, 10.20.0.0/16
      Endpoint = web02.example.net:51820
      PersistentKeepalive = 25
```

The peer also routes the network `10.20.0.0/16` behind it. `wg-quick` adds a
route on `wg0` for each allowed IP that the interface's own network
`10.0.0.0/24` does not cover, here for `10.20.0.0/16`.

### Replace a node's private key

A one-time playbook removes the key file.
The next `wireguard_interface` task creates a new private key and writes the
private key into `/etc/wireguard/wg0.conf`.
MMX then reloads the interface and reports the new `public_key`:

```yaml
- file:
    path: /etc/wireguard/wg0.private_key
    state: absent
- wireguard_interface:
    name: wg0
    template: templates/wg0.conf.tmpl
```

Write the new public key into the inventory and run the playbook on the
other nodes, as for a new node. Until then they refuse the node's
handshakes.

### Remove the interface

```yaml
- wireguard_interface:
    name: wg0
    state: absent
```

MMX removes `/etc/wireguard/wg0.conf` through the owed restore and stops
and disables `wg-quick@wg0.service`, taking down the interface and routes.
Only confirmed removal also removes `/etc/wireguard/wg0.private_key`.
A restored removal keeps the private key. After confirmed removal, a later
`state: present` creates a new key.

## How it works

A run does this, in order:

1. It checks that `wg`, `wg-quick`, `ip`, `systemctl`, and
   `wg-quick@.service` are there.
2. It renders the file and checks it: exactly one `[Interface]` section, a
   `PublicKey` in each `[Peer]` section that no other peer has, an allowed IP
   for one peer only, and only settings that `wg-quick` and `wg` know.
   `PrivateKey` and `SaveConfig = true` are refused. With `SaveConfig = true`,
   wg-quick would overwrite MMX's file when the interface goes down.
   A `content` with any mistake listed in step 2 fails when the project
   loads, before any connection.
3. MMX makes `/etc/wireguard` a root-owned directory with mode `0700`.
   MMX reuses the existing private key without changing the key file's
   bytes. MMX corrects the key file's owner, group, and mode to `root:root`,
   `0600`. When no key file exists, the node creates 32 random bytes,
   clamped as `wg genkey` clamps keys, and writes the new key before
   preparing the configuration change.
4. MMX prepares `/etc/wireguard/<name>.conf` with the private key as the
   first line of `[Interface]`, owned by `root:root` with mode `0600`.
   Every file content or metadata change, including first creation and
   removal, goes through the node's owed restore service.
5. It compares the running interface with the file: the private key, the
   listen port, and the peers by public key, each with its allowed IPs, its
   keepalive, and whether it has a preshared key (never the key itself),
   from `wg show wg0 dump`; the addresses and, when the file
   sets one, the MTU, from `ip`; a route on the interface for each allowed IP,
   as `wg-quick up` adds them; and whether the service is enabled and
   active. The endpoint is not compared, because WireGuard changes it when a
   peer's address changes and shows a host name resolved.
6. When everything matches, the task changes nothing. Otherwise MMX starts
   the service with `systemctl enable --now` when it is not active. It
   restarts the service when a line that only `wg-quick` reads changed
   (`Address`, `DNS`, `MTU`, `Table`, `SaveConfig`, `PreUp`, `PreDown`,
   `PostUp`, or `PostDown`), when an address or the MTU differs, when an
   allowed IP needs a route that the interface does not have, when a
   route that `wg-quick` added stays for an allowed IP that leaves, or when
   a peer has a keepalive or a preshared key that the file no longer sets.
   `wg syncconf` in older wireguard-tools releases keeps those two (fixed
   upstream in 2025 and 2026), so only a restart removes them. Otherwise
   it reloads the service. The reload runs `wg syncconf` with the file
   without `wg-quick`'s own lines, which changes only what differs: a peer
   added or removed, its allowed IPs inside the interface's network, a
   keepalive or preshared key that the file sets, its endpoint, or the
   listen port. The other peers keep their
   sessions. A restart takes the interface down for about a second and ends
   its sessions: a peer that sends to the node first gets answers again only
   when WireGuard makes a new handshake, about 15 seconds later, unless the
   node sends to it first.
7. After a start, reload, or restart, MMX compares the running interface
   against the configuration file again. A remaining difference fails the
   task. A file change also requires reachability confirmation before the
   node accepts the proposed configuration.

The result's `reason` says what happened, such as `created the private key
/etc/wireguard/wg0.private_key, wrote /etc/wireguard/wg0.conf, started
wg-quick@wg0.service` or `wrote /etc/wireguard/wg0.conf, reloaded
wg-quick@wg0.service: added peer xntON0r+ip4v53R2PDYOEKeq0RH91Y9x307uPnUQOTE=`.
A restart says why it was needed.

A `[Peer]` with the node's own public key is left out, as WireGuard leaves it
out, so a template may list every node of a group.

The private key never appears in a result, the output, the saved run
history, an error, or a command line. The reported difference for
`/etc/wireguard/<name>.conf` shows
`PrivateKey = (the node's private key, not shown)` in place of the private key.
The reported difference shows
`PresharedKey = (a preshared key, not shown)` in place of a preshared key.
MMX keeps no full difference of the file beyond the one in the result, and
it replaces the key with `[redacted]` in any other text of the result. A
secret in the file, such as a preshared key from `.secrets`, hides the whole
result, its `public_key` included, as for every module.

A dryrun reports the same comparison and the start, reload, or restart it
would make, and writes nothing. On a node without the key file, `public_key`
is not known yet, and node tasks and outputs that read it report not
checked. As the shared dryrun rules require, reachability after activation
was not checked.

SSH can use the node's WireGuard address, so a wrong configuration can
cut the node off from the controller machine, where you run `mmx`.
A **reachability confirmation** proves access over a new SSH or HTTPS
connection from the controller process, the running `mmx` command.
An **owed restore** records that the node must restore the previous
configuration unless every required confirmation arrives in time.
A systemd timer performs restoration without the controller machine.

File changes follow the shared [confirmation steps](../../docs/changing-mmx/design/modules-network.md#what-a-run-does)
and [confirmation rules](../../docs/changing-mmx/design/execution.md#reachability-confirmation-and-owed-restore).
The [confirmation duration input](../../docs/changing-mmx/design/modules-network.md#inputs-shared-by-all-four-modules)
defaults to `2m`. Activation consumes part of the confirmation duration.
The module runs once, refuses node task timing overrides, and ignores
project timing defaults. A retry must not cut the node's network again.
A confirmed change succeeds with `changed`. An unconfirmed change fails
with the missing connection kinds and the [restore outcome](../../docs/changing-mmx/design/modules-network.md#results-and-failure-reasons).

Restoration stops and disables the service when no previous configuration
file existed. Otherwise restoration reloads or restarts the service using
the forward decision rules, then restores the service's previous enabled
and active state. MMX checks the restored running interface against the
previous configuration file when MMX can compare the previous file.
Restoration also accepts a file supplied by a first-boot script, including
a file with a `PostUp` key command or settings MMX cannot parse.
When MMX cannot safely compare the previous file, restoration restarts a
previously active service and checks service state and interface existence.
MMX refuses a change over an existing file with `SaveConfig = true`, also
in a dryrun, because wg-quick would write the running settings over the
file that the node restores when the change is not confirmed.
On a node whose AppArmor policy enforces the profile `wg-quick//ip`
without `capability net_bind_service`, as Ubuntu 26.04's does, MMX also
refuses a `ListenPort` below 1024 before any change, also in a dryrun:
`wg-quick` brings the interface up through that profile, which opens the
port, so the interface could not start. See
[Why it works this way](#why-it-works-this-way).
A previously inactive service stays stopped. A newly created private key
stays after restoration.
The [saved configuration files](../../docs/changing-mmx/design/execution.md#keep-the-owed-restore-until-it-is-resolved)
contain private keys and stay in root-owned storage: directory mode `0700`,
file mode `0600`.

When the configuration file matches, MMX repairs a differing running
interface or service directly, without a confirmation wait.
Restoring an identical file would undo no running-state repair.
The shared [removal rules](../../docs/changing-mmx/design/modules-network.md#what-state-absent-removes),
[dryrun rules](../../docs/changing-mmx/design/modules-network.md#dryrun), and
[limits](../../docs/changing-mmx/design/modules-network.md#limits) also apply.
MMX confirms reachability at the existing inventory address and does not
discover a replacement node address.

## Errors and what they mean

| What fails | What the failure means, and what to do |
|---|---|
| The task fails and names `wireguard-tools` | `wg`, `wg-quick`, or `wg-quick@.service` is missing; see [Set up](#set-up). |
| The interface configuration supplied through `content` or `template` is refused and the error names a line | `PrivateKey`, `SaveConfig = true`, a second `[Interface]`, a `[Peer]` without `PublicKey`, a repeated public key or allowed IP, or a setting that `wg-quick` and `wg` do not know. Correct the template or `content`. |
| `... holds no WireGuard private key` | `/etc/wireguard/<name>.private_key` holds something else. Remove `/etc/wireguard/<name>.private_key` as in [Replace a node's private key](#replace-a-nodes-private-key). |
| `systemctl ... wg-quick@wg0.service` fails, with the journal's last lines | `wg-quick` could not bring the interface up, such as for a port that another program uses or a `PostUp` command that failed. The journal's lines say why. |
| `ListenPort = 443 is below 1024, but on this node the AppArmor profile wg-quick//ip ... lacks capability net_bind_service` | The node's AppArmor policy keeps `wg-quick` from opening a port below 1024. Use a port of 1024 or above, or change the policy; see [Why it works this way](#why-it-works-this-way). |
| `wg0 still differs from /etc/wireguard/wg0.conf` | After the start, reload, or restart, the interface still differs as the message says, such as an interface brought up by hand with other settings. |
| `The interface wg0 still exists` after `state: absent` | The interface was not brought up by the service. Take interface `wg0` down with `ip link delete wg0`. |
| Reachability confirmation is missing | The node restores the previous configuration. The task names the missing SSH or HTTPS confirmations and the restore outcome. |
| A previous restore is still owed | Resolve the recorded restore before changing the WireGuard configuration file again. |

## Tested on

**Distributions:** Debian 12 and 13, Ubuntu 24.04 and 26.04.

The suite `tests/integration/wireguard_interfaces.py` runs in containers on
Debian 12 and 13 and Ubuntu 24.04, and in a VM on Ubuntu 26.04, where
AppArmor confines `wg-quick`, with `wireguard-tools`
1.0.20210914 and, on Ubuntu 26.04, 1.0.20250521. The
[validation record](../../docs/changing-mmx/validation.md) gives the latest results.

## Why it works this way

- **The node keeps its own key.** A first-boot script can supply the private
  key, or MMX can create the private key. The key never travels.
- **File changes have an owed restore.** SSH can use WireGuard, so the node
  must recover from a bad file without a working connection.
- **Runtime-only repairs do not wait.** Restoring an identical file would
  undo no repair.
- **A port below 1024 under Ubuntu 26.04's AppArmor profile is refused,
  not worked around.** Ubuntu 26.04's `apparmor` package confines `wg-quick`
  with `/etc/apparmor.d/wg-quick`. Its child profile `wg-quick//ip` brings
  the interface up, which opens the port, and lacks
  `capability net_bind_service`; the profile for `wg` has it. The profile's
  local include, `/etc/apparmor.d/local/wg-quick`, extends only the parent
  profile, so MMX could add the permission only by editing a file of the
  `apparmor` package. When a later version of the package changes that
  file, the upgrade asks about it, and unattended upgrades hold the package
  back. MMX reads the loaded policy and the profile, so the refusal ends
  when the profile allows the port. A team that needs such a port can
  disable the profile with
  `ln -s /etc/apparmor.d/wg-quick /etc/apparmor.d/disable/` and
  `apparmor_parser -R /etc/apparmor.d/wg-quick`. `wg-quick` then runs
  unconfined, as it does on Debian and Ubuntu 24.04, which load no profile
  for it.
- **MMX writes the key into the file.** A reload replaces the running
  settings with the file's; a key set by a `PostUp` command would be lost at
  the first reload.
- **The whole file, in wg-quick's format.** A template can build the peers
  from the inventory, and the format is the one every guide shows.
- **A reload when it applies the change, a restart only otherwise.** Adding
  or removing a peer then keeps every other peer's session; after a restart,
  traffic that a peer starts waits about 15 seconds for a new handshake.
- **MMX compares the running interface, not text.** `wg showconf` resolves
  host names and orders peers as the kernel lists the peers. For a roaming
  peer, whose network address changes, `wg showconf` reports the current
  endpoint rather than the configured endpoint. The `wg showconf` text can
  therefore differ from `/etc/wireguard/<name>.conf` without a difference
  that MMX needs to correct.
- **No `=` or `+` in the WireGuard interface name.** wg-quick accepts `=` and
  `+`, but systemd escapes `=` and `+` in the unit's name, and the service
  would then look for another file.
