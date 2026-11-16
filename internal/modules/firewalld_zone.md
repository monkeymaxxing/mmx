# `firewalld_zone`

`firewalld_zone` creates or removes one firewalld zone on a node, through
the node's `firewall-cmd`. A firewalld zone is a set of firewall rules; its
members are the network interfaces and source networks whose traffic uses
those rules. This module manages the zone itself. The
[`firewalld`](firewalld.md) module manages the rules of a zone, and
[`firewalld_zone_membership`](firewalld_zone_membership.md) manages its
members.

Firewalld keeps two layers: the running firewall and the stored
configuration that it loads on a restart or reload. Firewalld has no
command that creates a running zone without reloading, so this module
**reloads firewalld** when the running firewall lacks the zone. A reload
replaces **every zone's running configuration** with its stored
configuration. Settings that existed only in the running firewall are
dropped. Stored settings that were not running become active.

## Set up

Install firewalld with `dnf_package` and start it with `systemd_service`, as
the [`firewalld` page](firewalld.md#set-up) shows.

## Examples

### Create a zone

```yaml
fleet_stages:
  - id: create_application_firewall_zone
    nodes: web01
    node_tasks:
      - firewalld_zone:
          zone: appzone
          state: present
```

If the stored configuration lacks the zone, MMX creates it there. If the
running firewall lacks it, MMX reloads firewalld. The result explains when a
reload happened. Dryrun explains when one would happen. Review the
differences between the two layers before running the task. An unchanged
task does not reload.

### Remove a zone

```yaml
- firewalld_zone:
    zone: appzone
    state: absent
```

The module refuses while the zone has any interface or source in either
layer, naming the members. Remove IP sources and interfaces with
`firewalld_zone_membership` first. Remove unsupported MAC address or ipset
sources with firewalld. Then MMX deletes the stored zone and reloads.

## How it works

`zone` is required, and `state` is `present` (the default) or `absent`.

For `present`, MMX creates missing stored configuration with
`firewall-cmd --permanent --new-zone=<name>`. When the running zone is
missing, it runs `firewall-cmd --reload`, then checks both layers. If only
the stored zone was missing, no reload is needed. An unchanged task does
nothing.

For `absent`, MMX refuses any interface or source in either layer, naming
the members and `firewalld_zone_membership`. Source kinds that
`firewalld_zone_membership` does not support need firewalld's own commands.
With no members, MMX deletes an existing stored zone with
`firewall-cmd --permanent --delete-zone=<name>` and reloads if either layer
had the zone.

Zone creation and removal use no reachability confirmation. An empty zone
handles no traffic. The global reload still has the effects above.

While a membership restore or cleanup of
[`firewalld_zone_membership`](firewalld_zone_membership.md#what-a-restore-puts-back)
remains owed on the node, this module refuses: its reload could disturb
that recovery.

## Errors and what they mean

| What fails | What it means, and what to do |
|---|---|
| Removal is refused, naming the zone's members and `firewalld_zone_membership` | The zone still has an interface or a source in the running firewall or the stored configuration. Remove IP sources and interfaces with `firewalld_zone_membership`, and MAC address or ipset sources with firewalld, then run again. |
| firewalld's own message about a built-in zone | firewalld refuses to delete a built-in zone. MMX reports firewalld's own words. |
| The task is refused while a membership restore is owed | A `firewalld_zone_membership` change on this node still owes a restore or cleanup. Resolve it first, as that page says. |

## Tested on

**Distributions:** Rocky Linux 9, AlmaLinux 10.

The suite `tests/integration/firewalld_zone_memberships.py` also covers
this module: dryrun, creation in the stored configuration and in the
running firewall, the reload (a port that another zone had at runtime only
is gone), an unchanged rerun, the refusal to remove a zone that has a
member, and removal. It passes on Rocky Linux 9 (firewalld 1.3.4) and
AlmaLinux 10 (2.4.3). The
[validation record](../../docs/changing-mmx/validation.md#firewalld-zone-modules) gives
the latest results.

## Why it works this way

- **`zone` names the zone in all three firewalld modules.** One spelling
  makes the target clear when tasks create a zone, set its rules, and add members.

- **A separate module for zones.** Creating a running zone needs a reload,
  and keeping that in its own module preserves the promise of the
  [`firewalld`](firewalld.md) rules module never to reload firewalld.
- **No reachability confirmation.** An empty zone has no traffic assigned,
  although the global reload can change other zones. The module
  description, the result reason, and dryrun all explain that effect.
- **No reload while a membership restore is owed.** The reload could
  disturb that recovery.
