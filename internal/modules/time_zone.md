# `time_zone`

`time_zone` sets the node's time zone with `timedatectl` on the node. The
time zone decides the local time that the node's programs show and log.
Keeping the clock itself right is the job of the node's time service,
`systemd-timesyncd` or `chronyd`. Manage the time service with an ordinary
[`systemd_service`](systemd_service.md) node task.

## Examples

### Set the time zone

```yaml
- time_zone:
    name: Europe/Berlin
```

On the node, MMX reads the time zone from the link `/etc/localtime`. When the
node's time zone differs from `Europe/Berlin`, MMX sets `Europe/Berlin` with
`timedatectl` and reports the old and new time zones. When the node already
uses `Europe/Berlin`, the `time_zone` node task reports unchanged.

## How it works

- `name` is a name from the time zone database, such as `Europe/Berlin`,
  `America/New_York`, or `UTC`. `timedatectl list-timezones` on a node lists
  all names in the time zone database. A name that the node's
  `/usr/share/zoneinfo` does not have fails the node task before anything
  changes, in a dryrun too.
- MMX reads the time zone from the link `/etc/localtime`, which programs
  use, not from `timedatectl`. A node without that link uses `UTC`.
- Results and dryrun show `time_zone_before` and `time_zone_after` in
  `.this_node_task.value`. `time_zone_before` is empty on a node whose
  `/etc/localtime` is a copy of a time zone file instead of a link to one.
- Running services read the time zone when they start. Changing the node's
  time zone does not restart running services. If a service must use the new
  time zone, explicitly restart the service, for example from a restart
  playbook.

## Errors and what they mean

| The reason holds | What the error means, and what to do |
|---|---|
| `... is not a time zone in the time zone database /usr/share/zoneinfo; ...` | Check the spelling against `timedatectl list-timezones`. Nothing was changed. |
| `timedatectl is not installed; install the package systemd first` | Install the `systemd` package in an earlier node task. A dryrun reports the `time_zone` node task not checked, because an earlier node task may install `systemd`. |
| `the time zone database /usr/share/zoneinfo is not installed; install the package tzdata first` | Install `tzdata` in an earlier node task. A dryrun reports the `time_zone` node task not checked. |

## Tested on

**Distributions:** Debian 12 and 13, Ubuntu 24.04 and 26.04, Rocky Linux 9, AlmaLinux 10.

The suite `tests/integration/node_settings.py` runs on Debian 12 and 13, Ubuntu 24.04 and 26.04, Rocky Linux 9, and AlmaLinux 10, in containers on the first four and in virtual machines on the last two. The
[validation record](../../docs/changing-mmx/validation.md)
has the latest results.

## Why it works this way

- **MMX reads the time zone from `/etc/localtime`, not from
  `timedatectl`.** After a `set-timezone` that could not write the link
  (SELinux on a node whose files lack their labels), `timedated` reports the
  new name while the node keeps the old one, and a retry took that for
  done.
