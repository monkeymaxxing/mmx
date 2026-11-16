# `systemd_service`

`systemd_service` controls a systemd unit on the node: whether it runs,
whether it starts at boot, and whether anything may start it at all
(masking). In a node task block's `activate` phase, it also restarts or
reloads the unit after the block's tracked inputs changed. It runs
`systemctl` and confirms the resulting state, and fails if the unit did not
reach it.

It controls a unit of any kind that [`systemd_unit`](systemd_unit.md)
installs, and the units that packages bring. `systemd_unit` writes a unit's
file; `systemd_service` acts on the unit. A package never starts its services
while it installs (see the
[package modules](../../docs/changing-mmx/design/modules-packages.md)); a service runs
because a `systemd_service` task says `state: running`.

Activation success is separate from health. Use [`wait_for`](wait_for.md) in
the node task block's `verify` phase to check that the service answers.

## Examples

### Reload a service after its configuration changed

```yaml
- task_block:
    id: nginx_site             # a node task block ID you choose
    changes:
      - file:
          permissions_mode: "0644"
          path: /etc/nginx/conf.d/shop.conf
          content: |
            server {
              listen 80;
              root /srv/shop/public;
            }
    activate:
      - systemd_service:
          name: nginx
          start_at_boot: true
          refresh: reload
          state: running
    verify:
      - wait_for:
          url_responds: http://127.0.0.1/
```

On the node, the `file` task writes `/etc/nginx/conf.d/shop.conf`. In
`activate`, `systemd_service` starts `nginx.service` if it is stopped,
enables it for boot, and reloads it if it was running and the file changed.
An unchanged run does nothing. `wait_for` then waits until nginx answers. A
name without a unit kind means `.service`, so `nginx` is `nginx.service`.

### Enable for the next boot without starting it now

`state: do_not_start_or_stop` tells `systemd_service` not to start and not to stop the
unit:

```yaml
- systemd_service:
    name: report-importer
    start_at_boot: true
    state: do_not_start_or_stop
```

### Mask a unit so that nothing can start it

`masked: true` makes systemd refuse every start of the unit, by hand, at
boot, or as a dependency of another unit. The following task masks
`ctrl-alt-del.target` to prevent a reboot from pressing Ctrl+Alt+Delete on a
keyboard attached to the server:

```yaml
- systemd_service:
    name: ctrl-alt-del.target
    state: stopped
    start_at_boot: false
    masked: true
```

`masked: false` unmasks the unit before MMX enables or starts it. Without
`masked`, MMX does not look at masking.

### Restart a service that runs but hangs, once

A service can run and still not answer, such as one stuck after a lost
database connection. The following node task in a
[one-time playbook](../../docs/how-to/make-a-one-time-change.md) restarts
`shop-worker.service` once on each node selected by the playbook:

```yaml
- systemd_service:
    name: shop-worker
    state: running
    restart_now: true
    run_only_once_marker_file: /var/lib/mmx/commands_run_once/2026-10-27_restart_shop_worker
```

On a node where the marker file does not exist, the node task runs
`systemctl restart shop-worker.service`, checks that the unit runs, and then
writes the marker file. The node task reports `changed` with the reason
`restarted shop-worker.service once and wrote the marker file ...`.
A unit that was not running is started by the restart, and the reason says
`started`. A dryrun reports `would_change` with `would restart`.

Once the marker file exists, the node task only keeps the unit running,
as any task with
`state: running` does, so `mmx check-one-time-playbooks` counts the node as
done. To restart the unit again later, write a new one-time playbook with a
new marker file.

### Timers, targets, and units of an account

The [`systemd_unit`](systemd_unit.md#examples) page shows `systemd_service`
with a timer, with a target that restarts a group of services, and with a
unit in an account's own systemd. An account's own systemd runs the account's
units without root, separately from the system's systemd.
The `systemd_unit` page's section on
[units in an account's own systemd](systemd_unit.md#units-in-an-accounts-own-systemd)
says how a task disables or masks such a unit while the account's systemd
is not running.

## How it works

### Start, stop, enable, and confirm

`systemd_service` starts, stops, enables, or disables the unit as needed,
then confirms the resulting state and fails if the unit did not reach it. A
stopped, disabled request is satisfied when the unit no longer exists, so a
removal playbook reruns unchanged. It is also satisfied when the account
that `in_systemd_of_user_account` names no longer exists, because such an
account has no unit that runs or is enabled. Starting or enabling requires
a loaded unit, and a task that starts, enables, or masks a unit of an
account requires the account.

A unit that systemd generates from another file, such as the service of a
[Podman Quadlet file](podman_quadlet.md), cannot be enabled: the task rejects
`start_at_boot` for it, names that file, and for a Quadlet file says where its
start at boot is declared. The unit of an `/etc/init.d` script is the
exception: systemd enables and disables it through the distribution's own
tool, and so does the task.

`state: running` means that the unit stays active. A oneshot unit that stays
active after it exits, such as `nftables.service` (`RemainAfterExit=yes`),
is started and restarted like any service.

### Unit kinds

A `name` without a unit kind means `.service`.
For unit kinds other than `.service`:

| Unit kind | What `systemd_service` does with the unit |
|---|---|
| `.timer`, `.path`, `.socket` | Starts, stops, and enables the timer, path, or socket unit. Name the timer, path, or socket unit in `activate`, rather than the service started by the unit. Restarting a timer resumes triggering the service according to the timer's settings. Restarting a path unit starts watching its paths again. Restarting a socket unit reopens the socket. |
| `.mount` | Starting the mount unit mounts the filesystem. Enabling the mount unit with an `[Install]` section mounts the filesystem at boot. Restarting the mount unit remounts the filesystem. Units that require the mount, such as through `RequiresMountsFor=`, restart with the mount unit. |
| `.target` | Starts, stops, and enables the target. Restarting the target restarts every member unit that says `PartOf=` the target. |
| `.swap` | Starting the swap unit turns the swap space on now. Enabling the swap unit turns the swap space on at boot. |
| `.automount` | Starting the automount unit makes systemd mount the filesystem when a process first accesses the mount point. |
| `.slice` | Nothing: systemd starts a slice when a unit in it starts, so a slice needs no `systemd_service` task. |

Only `.service` units accept `refresh: reload`. Set `refresh: reload` on a
`systemd_service` task only when the service's program supports reloading.

### `state: do_not_start_or_stop`

- With `state: do_not_start_or_stop`, MMX neither starts nor stops the unit. With
  `start_at_boot`, `systemd_service` only enables or disables the unit.
- In a node task block's `activate` phase, `do_not_start_or_stop` still refreshes a
  unit that is running, exactly as `running` does. A unit that is stopped
  stays stopped, and MMX keeps no record of a refresh owed to the stopped
  unit. When the stopped unit starts later, the
  unit loads the files that are on disk then. So `do_not_start_or_stop` also
  refreshes a service only where it runs.
- With `do_not_start_or_stop`, a unit that does not exist is reported unchanged,
  unless the task says `start_at_boot: true`. MMX keeps no record of a
  refresh owed to the missing unit:
  when a later node task installs and starts the unit, the unit loads the
  files that are on disk then.

### Masking

- `masked: true` masks the unit after stopping or disabling it, so nothing
  can start it.
- `masked: true` cannot go with `state: running` or `start_at_boot: true`, and
  `state` must be written, because a masked unit cannot start. Validation
  rejects the combination.
- A task without `masked` that neither starts nor enables the unit
  (`state: stopped` or `do_not_start_or_stop`, without `start_at_boot: true`) leaves the
  mask of a masked unit alone, and stops the unit if it still runs. Only a
  start or an enable fails on a masked unit.
- systemd masks a unit with a link in `/etc/systemd/system`. It refuses when
  the unit's own file is in that directory. Remove the file first with
  `systemd_unit` and `state: absent`.

### Refresh in a node task block's `activate` phase

Inside a node task block's `activate` phase, the task also decides whether
the block's tracked changes need a refresh, following the
[configuration-hash contract](../../docs/changing-mmx/design/execution.md#service-reloads-follow-configuration-hashes):

- a changed unit definition, a changed package version, or a package
  removal restarts the unit, even when `refresh` is `reload`;
- changed configuration, such as a changed file or a new checked-out commit,
  uses the declared `refresh`, which defaults to `restart`;
- matching hashes need nothing.

A stopped unit is started once, with no extra refresh. A failed reload fails
the task; MMX does not fall back to a restart. The page
[Service recovery](../../docs/reference/shared-module-behavior.md#service-recovery) says how MMX
remembers what a service loaded, so that a refresh that an interrupted run
missed still happens on the next run, and
[Checks before a restart or reload](../../docs/reference/playbook-file.md#task_block-fields)
says how a node task block refuses a restart that nobody approved. The
execution design owns how attempted and confirmed actions are recorded.

A changed timer, path, socket, or mount unit is restarted once when a task
in `activate` names it. Name in `activate` each unit whose running instance
must load a change.

### `restart_now` and its marker file

- `restart_now: true` needs `state: running` and
  `run_only_once_marker_file`, an absolute path. Validation refuses
  `restart_now: true` in a node task block's `activate` phase: the block's
  tracked inputs from `changes` decide whether the unit restarts.
- While the marker file does not exist, the node task runs
  `systemctl restart`. The command restarts a running unit or starts a
  stopped unit, and waits for systemd's result. If the restart fails, the
  node task fails with the unit's journal lines.

  After the command succeeds, the node task checks whether the unit runs.
  If the unit does not run, the node task fails with the unit's journal lines.
  After confirming that the unit runs, MMX writes the marker file and
  creates any missing parent directories. A failed restart leaves no marker
  file, so the next run tries the restart again.
- While the marker file exists, `restart_now` requests no restart. The node
  task still enforces `state: running`, so a stopped unit is started.
  Any entry at the marker path counts as the file.
- `start_at_boot`, `masked: false`, and `in_systemd_of_user_account` work
  with `restart_now` as without it. The marker file is MMX's own record, the
  same kind of file that [`shell`](shell.md) writes for its
  `run_only_once_marker_file`; keep such files in
  `/var/lib/mmx/commands_run_once/`.

### A unit that is still starting

systemd starts a unit again after a start that failed when the unit file
says `Restart=`, as Docker's does. While systemd waits for the next start and
while the unit starts, systemd reports the unit as `activating`. MMX does
not count `activating` as the required active state. A `systemd_service`
node task with `state: running` runs `systemctl start` when the unit is
`activating`. The command waits for the existing start attempt and returns
the attempt's result. If the start fails, the node task fails. A dryrun
reports `would_change` with the reason
`service is still starting`.

A unit that is `activating` directly after a start or restart that systemd
reported as done has ended at once, and systemd starts it again: the node
task fails and its reason says `ActiveState=activating`.

### When a unit does not start: lines of the systemd journal

When systemd cannot start a unit, its own message says only that the start
failed and where to look. The cause is in the systemd journal on the node:
the place where systemd keeps what each unit's programs print, and what
systemd itself notes about the unit. MMX reads the journal on the node, so
that you do not have to log in to it. The reason of the failed
`systemd_service` node task holds systemd's message, and after it the last
lines that the journal has of the unit:

```text
exit status 1
Last lines of the systemd journal for shop.service during this start:
  Starting shop.service...
  shop: cannot read /etc/shop/config.yml: No such file or directory
  shop.service: Control process exited, code=exited, status=1/FAILURE
  shop.service: Failed with result 'exit-code'.
  Failed to start shop.service.
```

MMX adds the unit's journal lines to the failure reason in two cases:

- systemd refuses a start, restart, or reload requested by the node task.
- systemd accepts a start or restart, but the unit is not running afterwards.

The included journal lines cover only the requested start, restart, or
reload. MMX excludes lines written before the requested action, such as
lines from a failure last week.

A unit can need another unit: its unit file names that unit in `Requires=`,
`BindsTo=`, or `Requisite=`. A container that belongs to a pod needs the
pod's service in this way.
When systemd cannot start a unit because a required unit is not running,
MMX names the required unit in the failure reason. MMX includes the required
unit's journal lines because the lines can explain the dependency failure:

```text
exit status 1
systemd did not start shop.service, because shop-pod.service, which it needs, failed.
Last lines of the systemd journal for shop-pod.service during this start:
  Starting shop-pod.service...
  Error: building local pause image: finding pause binary: exec: "catatonit": executable file not found in $PATH
  shop-pod.service: Control process exited, code=exited, status=125/n/a
  shop-pod.service: Failed with result 'exit-code'.
  Failed to start shop-pod.service.
```

The timer of a [`systemd_scheduled_job`](systemd_scheduled_job.md) task that
systemd does not start or restart reports its lines in the same way.

- **Which lines.** The lines are what the unit's programs printed and what
  systemd wrote about the unit, since the moment just before the node task
  asked systemd for the action. MMX compares that moment with the time at
  which the journal received each line, on the node's monotonic clock. That
  clock counts on evenly from the boot, also when somebody sets the node's
  date and time.
- **Limits.** The reason holds the last 10 lines of a unit. A line longer
  than 300 bytes is cut and ends with ` ...`. A control character in a line,
  such as the start of a color sequence, becomes a space. Everything that
  the journal adds is at most 4000 bytes.
- **Needed units.** MMX looks only at the units that the unit itself names,
  not at the units that those need in turn. It names at most three needed
  units that are not running. One that failed is reported as above; one in
  another state is reported with that state, for example `shop-data.mount,
  which it needs, is not running (ActiveState=inactive)`.
- **When the journal says nothing.** When the journal has no line of the unit
  since the action, or cannot be read, the reason stays as it was before
  MMX looked. Reading the journal never changes whether a node task fails,
  and MMX gives it at most five seconds.
- **Secrets.** A program can print anything, also a password that it read.
  Journal lines are therefore treated like the output of a command: when the
  node task read a secret, its reason is hidden with the rest of its details
  ([Secrets in files, output, and history](../../docs/reference/secrets.md#secrets-in-files-output-and-history)).
- **With `retry: true`,** each try is an action of its own and reports its
  own lines; the node task's reason shows those of the last try after
  `node task did not finish after 3 tries, because each try failed or left retry.until false:`.

### Units in an account's own systemd

`in_systemd_of_user_account` says that the unit belongs to that account's
own systemd. The [`systemd_unit`](systemd_unit.md#units-in-an-accounts-own-systemd)
page describes such units, with lingering, which a `systemd_service` task
that starts or enables a unit turns on too. For such a unit:

- **The journal heading names the account:** `Last lines of the systemd
  journal for report-worker.service of account reports during this start:`.
  The account reads its own journal where it may. On a node that keeps the
  journal only in memory, as Rocky Linux 9 does, it may not. Neither may an
  account that was created with `system: true`, because systemd keeps the
  lines of such an account in the system's journal. Root then reads the same
  lines, selected by the account's user ID, which only the journal itself
  can set.
- **Lines of an account without a unit.** The journal finds out which unit
  printed a line by looking at the process that printed it. A process that
  prints its error and ends at once is sometimes gone by then. That happens
  most often to a program that the unit's command started, such as the
  program that a wrapper script runs. For a unit of an account the journal
  then records the account, but no unit. So MMX shows the account's printed
  lines without a unit since the action under a heading of their own:
  `Lines that programs of account reports printed during this start, for
  which the systemd journal recorded no unit because the program ended at
  once:`. Another unit of the same account that failed in the same moment
  can have printed one of them. A system unit does not lose its lines this
  way, because the journal takes the unit's name from the system's systemd.

## Errors and what they mean

- **A oneshot unit asked to be started.** A unit that runs once and then
  ends (`Type=oneshot` without `RemainAfterExit=yes`) can never be found
  started, so the task fails and explains this. Run such a command with
  [`shell`](shell.md), or let a timer start it.
- **A masked unit asked to start or be enabled.** A task without
  `masked: false` fails and says so. MMX does not unmask a unit by itself.
  Add `masked: false` when the unit should run again.
- **systemd refuses to mask a unit.** The unit's own file is in
  `/etc/systemd/system`. Remove the unit's file first with `systemd_unit` and
  `state: absent`.
- **`start_at_boot` for a generated unit.** The task names the file that the unit
  is generated from, such as a Quadlet file, and says where its start at
  boot is declared.
- **`masked: true` with `state: running` or `start_at_boot: true`, or without
  `state`.** Validation rejects the task. Every service task requires `state`.
- **`refresh: reload` on a unit that is not a `.service`.** Validation
  rejects a `systemd_service` task that sets `refresh: reload` for a unit
  other than a `.service`.
- **A failed start, restart, or reload.** The reason holds systemd's message
  and the unit's lines of the journal, as shown above. Read the lines,
  correct the cause, and run again.
- **A reason that says `ActiveState=activating`.** The unit ended at once
  after a start that systemd reported as done, and systemd starts it again.
  The journal lines say why.
- **A mount kept busy.** If a process keeps a mount busy, restarting the
  `.mount` unit fails with systemd's message; the next run restarts it.
- **`restart_now` without `run_only_once_marker_file`, with a `state` other
  than `running`, or in a node task block's `activate` phase.** Validation
  rejects the node task and says why.
- **`... was restarted, but its marker file ... was not written`.** The
  restart worked. The next run restarts the unit again unless you create
  the marker file named by `run_only_once_marker_file` by hand.

## Tested on

**Distributions:** Debian 12 and 13, Ubuntu 24.04 and 26.04, Arch Linux, openSUSE Leap 16.1, Rocky Linux 9, AlmaLinux 10, Fedora 45.

The base suite covers this module (`account_units.py`, `owed_restores.py`, `vm.py test`) and runs on every tested distribution.

The suite `account_units.py` starts units whose programs print one line and
fail. With `retry: true`, `account_units.py` reads the failure reasons from
the node task's three tries. Each reason includes the line printed during
the corresponding try exactly once, for a unit configured to restart after
failure.

Without `retry`, `account_units.py` counts starts on the node to confirm
that the node task tries once. The suite checks that a dependency failure
includes the line printed by the required unit's program. For an account
unit, the suite checks a line printed by a program started by the unit's
command. The line appears under the unit heading or the account heading for
entries without a recorded unit name.

`account_units.py` also covers `state: do_not_start_or_stop` and `masked`,
the failure for a oneshot unit asked to be started, every unit kind, and a
unit in an account's own systemd. `server_resources.py` covers loaded
configuration, activation, and one service in two fleet stages.

`routine_fixes.py` covers `restart_now` in a one-time playbook.
The suite compares the unit's invocation ID before and
after the run to confirm that the playbook restarts the running unit once.
The playbook is still needed before the run and done after the run.
The next run leaves the unit alone.

The whole catalog runs both suites on Debian 12 and 13 and Ubuntu 24.04 and 26.04
([validation record](../../docs/changing-mmx/validation.md)).

## Why it works this way

- **Every task writes `state`.** Starting a stopped unit is a separate choice
  from making the unit start at boot. A standby server can use
  `do_not_start_or_stop` with `start_at_boot: true` to enable the service unit
  for boot while leaving a stopped service stopped now.
  See [required settings](../../docs/changing-mmx/design/modules.md#a-setting-with-no-safe-guess-is-required).
- **The names describe the effect.** `running` names the required live state;
  `start_at_boot` names boot activation. `do_not_start_or_stop` leaves the live
  state alone and still permits refresh after tracked changes.
- **MMX does not unmask a unit by itself.** Somebody masked the unit on purpose.
- **A task tries once unless it sets `retry`.** Most core modules try three
  times, because temporary failures, such as a package manager's lock or a
  network fault, often disappear before another attempt. A failed service
  start fails again for the original reason, so retries delay
  reporting the failure. Repeated start attempts can also hide the original
  error behind systemd's start-limit message. systemd refuses a unit that
  systemd had to start more than five times in ten seconds (its defaults,
  `StartLimitBurst=` and
  `StartLimitIntervalSec=`), and the journal then says
  `Start request repeated too quickly`. See
  [node task retries](../../docs/reference/playbook-file.md#node-task-retries-and-time-limits).
- **A unit that is `activating` is not started.** Otherwise the next run, or
  a second try of a task that sets `retry`, would find the unit between two
  failed starts and report it as started.
- **The reason holds the unit's journal lines.** systemd's message only
  points to the journal. MMX includes journal lines in the task result so
  you can diagnose the failure without logging in to the node.
- **An account's lines without a unit are shown.** In measured failed
  starts of an account's unit, a line that the unit's own command printed
  retained the unit's name in the journal entry in 238 of 240 starts.
  A line printed by a program started by the unit's command retained the
  unit's name in 59 of 120 starts. Such a line usually explains the failure.
- **A stopped, disabled request is satisfied by a missing unit.** A removal
  playbook then reruns unchanged.
- **`restart_now` needs a marker file.** `mmx check-one-time-playbooks`
  counts a node as done when its dryrun proposes no change; without a marker
  every dryrun would propose the restart, and the playbook would stay needed
  for ever. In an ordinary playbook, a restart on every run would be an
  outage that nothing asked for. A restart that must happen again is a new
  one-time playbook with a new marker file.
- **`restart_now` stays out of `activate`.** In a node task block's
  `activate` phase, configuration hashes of the block's tracked inputs
  decide restarts and reloads. A separate `restart_now` rule would restart a
  unit even when the hashes require no refresh.
- **A unit of an account whose systemd is not running is read from its unit
  files.** Such a systemd says nothing about the unit, and an enabled unit
  would start at the account's next login. Starting the unit owner's
  systemd to inspect the unit would start the account's enabled units too,
  so MMX starts nothing.
