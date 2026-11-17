# `ufw`

`ufw` manages the node's firewall rules, default policies, and whether the
firewall is on, through the node's `ufw` command. ufw is the usual firewall
front end on Debian and Ubuntu.

The MMX module accepts three kinds of rules. Incoming rules apply to traffic
to the node itself. Routed rules apply to traffic the node forwards between
networks, such as the node's containers and VMs. Outgoing rules apply to
traffic that starts on the node. Each list names the kind of rules the list
holds: `incoming_rules`, `routed_rules`, and `outgoing_rules`.

On the Red Hat family, the usual firewall is firewalld; use
[`firewalld`](firewalld.md) there. A firewall that follows the inventory
closely can also be one nftables rule file written from a template, with no
module; see [One nftables file instead](#one-nftables-file-instead).

## Set up

Install `ufw` first:

```yaml
- apt_package:
    packages:
      - {name: ufw, version: newest_when_first_installed}
```

Until it is installed, a dryrun reports the `ufw` node task not checked,
because an earlier node task may install it. A run without it fails with a
message that names the package to install.

## Examples

### Allow SSH, MMX's HTTPS port, and a database port

```yaml
- ufw:
    default_incoming: deny
    incoming_rules:
      - action: limit
        port: 22
        protocol: tcp
      - action: allow
        port: 7443
        protocol: tcp
      - action: allow
        port: 5432
        protocol: tcp
        from: 10.0.0.0/8
      # No port: admits TCP and UDP on every port from the monitoring server.
      - action: allow
        protocol: tcp_and_udp
        from: 192.0.2.50
    incoming_rules_not_listed: remove
    enabled: true
```

Allow both SSH and the node's HTTPS port, normally 7443. If HTTPS is blocked,
MMX reaches the node through SSH fallback. MMX never opens the node's HTTPS
port itself.

### A server with containers or VMs behind a bridge

A server that runs containers or VMs behind a bridge, such as Incus's
`incusbr0` or Docker's `docker0`, needs incoming and routed rules: the guests
ask the server for addresses and names over the bridge (incoming), and their
traffic to the internet passes through the server (routed).
Without incoming rules for the bridge, `default_incoming: deny` drops the
guests' address and name requests, so the guests get no address.
Without routed rules for the bridge, `default_routed: deny` drops the
guests' traffic forwarded through the server to the internet:

```yaml
- ufw:
    default_incoming: deny
    incoming_rules:
      - {action: limit, port: 22, protocol: tcp}
      - {action: allow, port: 7443, protocol: tcp}
      # TCP and UDP from the guests to the server: DHCP and DNS.
      - {action: allow, incoming_interface: incusbr0, protocol: tcp_and_udp}
    incoming_rules_not_listed: remove
    routed_rules:
      # The guests' traffic out to other networks, and the replies back.
      - {action: allow, incoming_interface: incusbr0, protocol: tcp_and_udp}
      - {action: allow, outgoing_interface: incusbr0, protocol: tcp_and_udp}
    routed_rules_not_listed: remove
    enabled: true
```

The kernel forwards traffic only when forwarding is on
(`net.ipv4.ip_forward` and its IPv6 counterpart); container and VM tools
such as Incus and Docker turn it on themselves. ufw's routed rules only
filter what the kernel forwards.

### Outgoing rules

With `default_outgoing: deny`, the node needs outgoing rules for DNS (port 53,
udp and tcp), its package sources (ports 80 and 443, tcp), and time
synchronisation (port 123, udp). MMX's own connection is incoming, because
the controller machine connects to the node.

```yaml
- ufw:
    outgoing_rules:
      - {action: allow, port: 53, protocol: udp}
      - {action: allow, port: 53, protocol: tcp}
      - {action: allow, port: 80, protocol: tcp}
      - {action: allow, port: 443, protocol: tcp}
      - {action: allow, port: 123, protocol: udp}
    outgoing_rules_not_listed: keep
```

### One rule for each node of an inventory group

A database server admits the application servers; a monitoring server admits
every node. `from_inventory_group` on an incoming rule, in place of `from`,
stands for one rule for each node of an inventory group:

```yaml
# inventories/production.yml
nodes:
  app01: {address: 203.0.113.11, vars: {private_address: 10.0.1.11}}
  app02: {address: 203.0.113.12, vars: {private_address: 10.0.1.12}}
  db01:  {address: 203.0.113.20, vars: {private_address: 10.0.1.20}}
groups:
  app: {nodes: [app01, app02]}
  database: {nodes: [db01]}
```

```yaml
# playbooks/database_firewall.yml
fleet_stages:
  - id: database_firewall
    nodes: database
    node_tasks:
      - ufw:
          default_incoming: deny
          default_outgoing: allow
          incoming_rules:
            - {action: limit, port: 22, protocol: tcp}
            - {action: allow, port: 7443, protocol: tcp}
            - action: allow
              port: 5432
              protocol: tcp
              from_inventory_group: {group: app, address_var: private_address}
          incoming_rules_not_listed: remove
          enabled: true
```

`group` names the inventory group, and `address_var` names the variable in
each of its nodes' `vars` (the node's own or its groups') that holds the
address to admit. On `db01`, the `ufw` node task in
`playbooks/database_firewall.yml` produces two rules for port 5432, from
`10.0.1.11` and `10.0.1.12`. The [`firewalld`](firewalld.md) module takes the
same field.

When the address to admit is the one MMX itself connects to, the node's
`address` in the inventory, `use_node_address: true` takes it in place of
`address_var`, so the inventory holds each address once. This rule in the
playbook above admits WireGuard from each application server's public
address:
`{action: allow, port: 51820, protocol: udp, from_inventory_group: {group: app, use_node_address: true}}`.

With `use_node_address: true`, each node of the group must have an IP
address as the node's inventory `address`. A host name fails validation,
because a firewall rule needs an address.

## How it works

### Incoming rules

Each incoming rule has `action` (`allow`, `deny`, `reject`, or `limit`) and
needs `port`, `from`, `incoming_interface`, or several of them. `port` is a
number or a `first:last` range. Every rule requires `protocol`: `tcp`, `udp`,
or `tcp_and_udp`. The last value creates one TCP rule and one UDP rule,
also for a range; it does not admit other protocols. `from` is an address or a network
such as `10.0.0.0/8`; omitted, it means any source. `incoming_interface` is
the network interface the traffic arrives on, such as `eth1`; omitted, the
rule holds on every interface. A rule with `from` and no `port` admits
the selected protocols on every port from that address or network. A rule
with only `incoming_interface` matches those protocols on that interface. `port`,
`from`, and `incoming_interface` may be references. `from` and
`from_inventory_group` cannot both be set on one rule.

`incoming_rules` may also be one whole-value reference, such as
`incoming_rules: "{{ .node_tasks.plan.outputs.incoming_firewall_rules }}"`
for rules that an earlier node task works out. MMX cannot check those rules
when the project loads, so it checks them, and expands
`from_inventory_group`, once the reference has a value, before the task
changes anything; the SSH safeguard reads the resolved rules.

### Routed rules

Each routed rule has `action` and at least one of `incoming_interface` (where
the traffic arrives), `outgoing_interface` (where it leaves), `from` (its
source), `to` (its destination address or network), and `port` (its
destination port), plus a required `protocol` in the same form as incoming
rules. An omitted address, interface, or port matches any value. Those
addresses, interfaces, and ports may be references.

### Outgoing rules

Each outgoing rule in `outgoing_rules` applies to traffic that starts on the
node itself. It has `action` and needs `port` (the destination port), `to`
(the destination address or network), `outgoing_interface` (the interface
it leaves by), or several of them. `protocol` and `port` follow the incoming
rules' forms. Omitted destination and interface match any destination and
every interface. `port`, `to`, and `outgoing_interface` may be references;
outgoing rules have no `from` or inventory group form.

### A rule's identity

A rule's identity is its kind and everything but its action, written as ufw
writes it (`ufw show added`). For an incoming rule that is its interface,
port, protocol, and source; for an outgoing rule, `outgoing_interface`,
`port`, `protocol`, and `to`; for a routed rule, every field but the action.
Rules of different kinds are never the same rule. Listing an existing
identity with another action replaces the action in place, and a comment on
an existing rule does not change its identity. ufw applies the first rule
that matches. With `remove`, MMX also makes the node's order within that
kind match the list for each IP family. `ufw show added` prints IPv6-only
rules after IPv4 and dual-family rules, so its display can differ from the
actual IPv6 order. MMX reads the IPv6 order through ufw's own Python reader
and formatter, using the Python interpreter installed with ufw. With `keep`, existing rules keep their positions and
new rules go at the end of their kind in listed order. A rule listed twice in one list fails validation.

### Rules that are not listed

`incoming_rules_not_listed` is required with `incoming_rules`,
`routed_rules_not_listed` with `routed_rules`, and `outgoing_rules_not_listed`
with `outgoing_rules`:

- `remove` removes every unlisted rule of that kind, including
  application-profile rules, so the listed rules are the whole set of that
  kind, in the listed order. An empty list with `remove` removes all rules of that kind.
- `keep` appends missing rules and leaves existing rules and their order alone.

Each kind is managed only when its list is given, so a task with only
`incoming_rules` leaves the routed and outgoing rules as they are.
`state: absent` accepts only the three lists, where `action` is optional,
and removes the listed rules of all three kinds, whatever their action;
omit the `_not_listed` settings with it.

### Default policies and whether ufw is on

`default_incoming`, `default_outgoing`, and `default_routed` take `allow`,
`deny`, or `reject`: what happens to traffic that no rule matches, for traffic
to the node, from the node, and through the node. ufw's own defaults are
`deny`, `allow`, and `deny`. `enabled: true` turns the firewall on now and
at boot, and `enabled: false` turns it off. `logging_level` (`off`, `low`,
`medium`, `high`, or `full`) sets how much ufw writes to its own log, as
`ufw logging` does; MMX reads the current level from `LOGLEVEL` in
`/etc/ufw/ufw.conf`, because `ufw status verbose` shows it only while the
firewall is on. Omitted settings stay as they are.

### Changes and results

Changes apply immediately. MMX disables the firewall or changes the incoming
default to `allow` first when requested. For owned rule kinds, MMX rebuilds
a differing order; on an active firewall with a blocking incoming default,
an SSH allowance stays installed while other incoming rules move. Other
rule changes add before removing. The remaining policies, logging level,
and enabling follow. They are not inputs of a node task block's
service. MMX then inspects ufw again and fails the node task if any
difference remains. An unchanged rerun finds the rules, defaults, and state
already in place.

Dryrun and results list the rules to add, update, remove, or reorder in ufw's own
rule syntax, and the default or state changes, in `.this_node_task.value`
(`added`, `updated`, `removed`, `reordered_rules`, `default_incoming`, `default_outgoing`,
`default_routed`, `logging_level`, `enabled`). A routed rule starts with `route`; an
outgoing rule has `out` after the action, such as `allow out 53/udp`. A run that fails lists only the changes whose `ufw` command worked before
the failure, so a node task in `on_failure` can read what happened.

### The SSH safeguard

Before a change that leaves the firewall enabled with incoming default
`deny` or `reject`, MMX checks the resulting incoming rules in ufw's order.
An incoming rule counts when the rule allows or limits a TCP port on which
sshd listens (from `sshd -T`) and admits the controller machine's SSH
connection. The controller machine runs the `mmx` command. A node job is one
attempt to run the selected tasks on a node.

MMX takes the source address from the SSH connection that started the node
job. For HTTPS, MMX uses the source only when the HTTPS connection reached
the node's inventory IP address. Otherwise MMX cannot know SSH's source
address: run with `--connection ssh_only` or use a rule without `from`.

NAT or a proxy can change the address the node sees. A laptop and the team's
administration server show different source addresses in the example below.
Allowing only the laptops' network would exclude the administration server.

When a rule has `from`, the address or network must include the controller
machine's SSH source address. Rules expanded from `from_inventory_group`
follow the same check. When a rule has `incoming_interface`, the named
interface must hold the node's own inventory `address`, checked against the
node's kernel addresses. For example, SSH on `wg0` counts when `wg0` holds
the node's inventory IP address and the rule admits the controller machine's
SSH source address:

```yaml
- ufw:
    enabled: true
    default_incoming: deny
    incoming_rules_not_listed: remove
    incoming_rules:
      - action: allow
        port: 22
        protocol: tcp
        incoming_interface: wg0
        from: 10.20.9.0/24
      - action: allow
        port: 22
        protocol: tcp
        incoming_interface: wg0
        from: 10.20.0.1
```

The example admits laptops in `10.20.9.0/24` and the team's administration
server at `10.20.0.1`. Each managed node needs an inventory IP address held
by the node's `wg0` interface.

The following rules do not count:

- a rule with `from` and no `port`;
- a rule MMX cannot read as a port rule, such as an `OpenSSH` application
  rule;
- a rule whose `from` excludes the controller machine's SSH source address;
- a rule with `from` when SSH's source is unknown, such as HTTPS arriving at
  a different node address or missing connection details;
- a rule whose `incoming_interface` does not hold the node's inventory IP
  address, including a missing interface or an inventory `address`
  containing a host name;
- routed and outgoing rules, because routed and outgoing rules never admit
  traffic to the node's own sshd.

A `deny` or `reject` rule naming the SSH port, or naming no port at all
(such as `{action: deny, incoming_interface: eth0}`), follows the same
source and interface checks: a blocking rule without a port blocks every
port. A matching blocking rule before every counted allowance
blocks the change. An omitted source or interface imposes no restriction on
that part of the check. The error names the SSH source address or explains
why MMX cannot know the source, and gives an example incoming rule to add.
The node task fails before changing anything, and dryrun makes the same
check.

A `limit` rule for SSH, as in the examples, limits new connections per
address. It can slow the reachability confirmation of the network modules,
which open new connections; see
[Limits](../../docs/changing-mmx/design/modules-network.md#limits).

### Rules for an inventory group

MMX checks the group on the controller machine before it connects: a group
that does not exist, a node of the group without the variable, or a value
that is not an address or a network fails validation, also for a task that
might not run. Each node then makes the rules from the inventory its job
carries. A task inside a role works the same way: the role's own references
see only its inputs, but the module still reads the whole inventory.

[`mmx explain`](../../docs/reference/commands.md#mmx-explain) shows the
rules each node gets, with one rule for each node of the group, before any
server is contacted.

With `incoming_rules_not_listed: remove`, the task holds the whole rule
set, so a node that leaves the group loses its rule on the next run, and a
node that joins gets one. With `keep`, a rule for a node that left the
group stays on the server; use `keep` only for a task that adds to rules
another tool owns. A loop over the group's nodes with one rule each would
have the same gap: it can add a node's rule, but never remove the rule of a
node that left the inventory.

### One nftables file instead

A firewall that follows the inventory closely can also be one nftables rule
file, with no module. nftables is the firewall built into current Linux
kernels; ufw and firewalld are front ends to it. The example is a gateway
node: the private network behind it reaches other networks with the
gateway's address (NAT), and each web server behind it gets one port on the
gateway.

```yaml
# inventories/production.yml
vars:
  network_behind_the_gateway: 10.0.1.0/24
nodes:
  gateway01: {address: 203.0.113.1}
  web01: {address: 203.0.113.11, vars: {private_address: 10.0.1.11, port_on_the_gateway: 8080}}
  web02: {address: 203.0.113.12, vars: {private_address: 10.0.1.12, port_on_the_gateway: 8081}}
groups:
  gateways: {nodes: [gateway01]}
  web_servers_behind_the_gateway: {nodes: [web01, web02]}
```

The template `files/gateway_nat.nft.tmpl` produces the gateway rules file.
The template's first two table statements make loading the generated rules
file replace only the table `gateway_nat`:

```text
#!/usr/sbin/nft -f
# files/gateway_nat.nft.tmpl
# Managed by MMX. The first line creates the table when it is missing, so
# that the delete never fails; the third line declares it again, whole.
table ip gateway_nat
delete table ip gateway_nat

table ip gateway_nat {
    chain prerouting {
        type nat hook prerouting priority dstnat; policy accept;
{{- range .inventory.groups.web_servers_behind_the_gateway }}
{{- $web_server := index $.inventory.nodes . }}
        # {{ . }}
        tcp dport {{ $web_server.vars.port_on_the_gateway }} dnat to {{ $web_server.vars.private_address }}:80
{{- end }}
    }

    chain postrouting {
        type nat hook postrouting priority srcnat; policy accept;
        ip saddr {{ .vars.network_behind_the_gateway }} ip daddr != {{ .vars.network_behind_the_gateway }} masquerade
        ct status dnat masquerade
    }
}
```

```yaml
# playbooks/gateway.yml
fleet_stages:
  - id: gateway
    nodes: gateways
    node_tasks:
      - when: {equals: ["{{ .node.package_manager }}", apt]}
        apt_package:
          packages:
            - {name: nftables, version: newest_when_first_installed}
      - when: {equals: ["{{ .node.package_manager }}", dnf]}
        dnf_package:
          packages:
            - {name: nftables, version: newest_when_first_installed}

      # NAT forwards packets between networks, which the kernel does only
      # with this setting.
      - sysctl_setting:
          name: net.ipv4.ip_forward
          value: "1"

      - directory:
          path: /etc/nftables-gateway
          owner: root
          group: root
          permissions_mode: "0755"

      - task_block:
          id: gateway_rules
          changes:
            - file:
                path: /etc/nftables-gateway/gateway_nat.nft
                template: files/gateway_nat.nft.tmpl
                owner: root
                group: root
                permissions_mode: "0644"
                check_new_file:
                  shell:
                    script: nft --check --file "$NEW_FILE"
                    dryrun: nft --check --file "$NEW_FILE"
                    env:
                      NEW_FILE: "{{ .new_file.path }}"
          activate:
            - id: load_gateway_rules
              shell:
                script: nft --file /etc/nftables-gateway/gateway_nat.nft
                only_when_tracked_inputs_changed: true

      # Loads the same file at every boot.
      - systemd_unit:
          name: gateway-nat-rules.service
          content: |
            [Unit]
            Description=Load the nftables table gateway_nat
            DefaultDependencies=no
            After=local-fs.target
            Before=network-pre.target
            Wants=network-pre.target

            [Service]
            Type=oneshot
            RemainAfterExit=yes
            ExecStart=/usr/sbin/nft --file /etc/nftables-gateway/gateway_nat.nft

            [Install]
            WantedBy=multi-user.target
      - systemd_service:
          name: gateway-nat-rules.service
          start_at_boot: true
          state: do_not_start_or_stop
```

On `gateway01`, the node job installs nftables and turns on the kernel's
forwarding of IPv4 packets. The node job writes
`/etc/nftables-gateway/gateway_nat.nft` from `files/gateway_nat.nft.tmpl`,
with a forwarded port for each node in `web_servers_behind_the_gateway`.
`nft --check --file` checks the proposed rules file before MMX's `file`
module replaces `/etc/nftables-gateway/gateway_nat.nft`.
When `/etc/nftables-gateway/gateway_nat.nft` changes, the command `nft --file`
in `activate` loads the new rules, as a
[command that follows a change](shell.md#run-a-command-after-a-change).
A later run with the same inventory writes the same file and loads nothing.

The unit `gateway-nat-rules.service` is enabled but not started: the command
in `activate` has already loaded the rules, and the unit loads them again
after each reboot.

- **One table, replaced in one step.** `table`, `delete table`, then the whole
  `table` again is nftables' way to replace one table. `nft --file` applies
  the whole file at once, so there is no moment without rules, and the tables
  of other tools, such as Docker, ufw, or firewalld, stay as they are.
- **The family.** A table of the family `ip` holds IPv4 rules, which is what
  this NAT rewrites. A filter table that must cover IPv4 and IPv6 together
  uses the family `inet` in the same three lines.
- **The file is written whole on every run,** so it never holds a rule for
  a node that left the inventory.
- **`nftables.service` is not used here.** The distributions' own
  `nftables.service` loads `/etc/nftables.conf` (Debian and Ubuntu) or
  `/etc/sysconfig/nftables.conf` (the Red Hat family), and clears every
  table when it reloads. It fits a node whose whole firewall is that one
  file; then a [`systemd_service`](systemd_service.md) task on
  `nftables.service` loads a changed file. When other tools keep rules of
  their own, use a table of your own, as above.

## Errors and what they mean

| What fails | What it means, and what to do |
|---|---|
| The node task fails before any change, in dryrun too, and names sshd's TCP port, such as `sshd's TCP port 22` | The change would block SSH from the controller machine, or MMX cannot establish SSH's source address. Follow the example incoming rule in the error. When HTTPS arrived at a different node address, run with `--connection ssh_only` or use a rule without `from`. A named `incoming_interface` must hold the node's inventory IP address. Remove or reorder a preceding matching `deny` or `reject` rule. See [The SSH safeguard](#the-ssh-safeguard). |
| Validation fails for a rule listed twice | One list holds the same identity twice, perhaps with different actions. Keep one. |
| Validation fails for `from_inventory_group` | The group does not exist, a node of the group lacks the variable, or its value is not an address or a network; with `use_node_address: true`, a node's `address` is a host name. This fails before MMX connects, also for a task that might not run. |
| The node task fails after its changes | ufw, inspected again, still differs from the task. |
| A run fails and names the package to install, or a dryrun reports the task not checked | `ufw` is not installed; see [Set up](#set-up). |

## Tested on

**Distributions:** Debian 12 and 13, Ubuntu 24.04 and 26.04.

The suite `tests/integration/server_resources.py` tests the module on the
distributions whose package manager is apt: Debian 12 and 13, and
Ubuntu 24.04 and 26.04. It installs ufw where missing. ufw's words for
outgoing rules were measured with ufw 0.36.2, and the outgoing rules
passed on all four ([validation record](../../docs/changing-mmx/validation.md#the-capability-reviews-additions)).

## Why it works this way

- **An explicit protocol and an owned order.** The protocol follows
  [required settings](../../docs/changing-mmx/design/modules.md#a-setting-with-no-safe-guess-is-required):
  opening both protocols must be visible. ufw uses the first matching rule,
  so owning the set also owns its order; otherwise reordering YAML would
  leave different traffic rules on the node.

- **`ufw` and `firewalld` are two modules; there is no `firewall` module
  for both.** Their models differ (ordered rules against zones); a common
  module would hide which tool runs and offer only what both share.
- **A rule's identity is written as ufw writes it.** MMX reads
  `ufw show added` and needs no second parser of ufw's rules.
- **The SSH safeguard reads the rules in ufw's order.** ufw applies the
  first matching rule. MMX checks allowances and blocks against SSH's source
  and the interface holding the node's inventory IP address. An HTTPS
  connection to a different node address cannot establish SSH's source
  address.
- **MMX never opens port 7443 by itself.** A firewall change the project did
  not write is a surprise; the run says which rule to add, as
  [A blocked port 7443 is reported, never opened](../../docs/changing-mmx/design/execution.md#a-blocked-port-7443-is-reported-never-opened)
  describes.
- **A rule for each node of a group is `from_inventory_group` inside a task
  with `incoming_rules_not_listed: remove`, or one nftables file from a
  template, not a loop that adds one rule per node.** A loop cannot remove
  the rule of a node that left the inventory; a task that holds the whole
  set can.
