# Let an application operator request a restart

An account in `mmx-operators` can request one fixed action by creating a
file named after the account, then read the result:

```sh
touch /run/mmx-demo-request/$USER
cat /run/mmx-demo-control/result
```

Each run records its invocation ID and `running`, removes every pending
request, restarts the application, then records `success` or a systemd
failure result such as `exit-code` or `timeout`. **Wait until your request
file is gone, then read the result until it no longer says `running`.** The
result comes from a restart that began after your request. After a reboot
the result says `No request since boot`. `systemctl status
mmx-demo-restart.path mmx-demo-restart.service` shows the watcher and action
status without requiring administrator access.

The operating-system setup is ordinary files:

- `tmpfiles.conf` creates `/run/mmx-demo-request`, sticky and writable by
  `mmx-operators`, and a root-owned directory with a readable result file
- `mmx-demo-restart.path` starts the action while the request directory is
  not empty (`DirectoryNotEmpty=`) and starts after tmpfiles at boot
- `mmx-demo-restart.service` always runs `systemctl restart mmx-demo.service`
- `result.sh` publishes the invocation and systemd result using an atomic rename

Request contents are ignored. Names systemd treats as hidden or backup files
do not count as requests: a leading `.`, a trailing `~`, suffixes such as
`.bak`, `.old`, `.new`, `.swp`, and package-manager backups, and
`lost+found`, `aquota.user`, and `aquota.group`. The sticky directory lets
members remove only their own requests. Members of `mmx-operators` cannot
replace the units, helper, command, result, or result directory. MMX
credentials and sudo access are unnecessary.

A request made while the action runs stays until that run ends, then starts
one more run; requests made meanwhile share it. Creating a request file
requests a restart; requests do not form a queue with one restart per
`touch`. The restart service allows five activations in 60 seconds.
Exceeding five activations in 60 seconds leaves the watcher failed and the
request in place; `systemctl status` shows the failure. An administrator
restores the watcher with:

```sh
sudo systemctl reset-failed mmx-demo.service mmx-demo-restart.service mmx-demo-restart.path
sudo systemctl start mmx-demo-restart.path
```

## Install with MMX

Set administrator access in `inventories/example.yml`, then run:

```sh
mmx validate
mmx dryrun restart_control
mmx run restart_control
mmx run restart_control
```

The [role](roles/restart_control/main.yml) installs a demonstration
application that runs `sleep infinity` as `mmx-demo`, plus the restart
interface. An unchanged run leaves the application and watcher alone. Each
run compares the runtime paths' type, mode, and owner with `stat` and runs
tmpfiles only when one is missing or has drifted, so repairs do not wait for
a reboot. `systemd_unit` tasks install the action and its path unit, and a
`systemd_service` task enables the watcher and starts it when stopped. A
changed path unit restarts the watcher. A changed action needs only the
manager reload, so the action's next run uses the changed action. Dryrun
creates no runtime paths and starts nothing.

To authorize an existing account, add an ordinary MMX task after the role:

```yaml
- user:
    create_home_directory: true
    name: alice
    supplementary_groups: [mmx-operators]
    supplementary_groups_not_listed: keep
```

`supplementary_groups_not_listed: keep` leaves Alice's other memberships
alone. A new login picks up the `mmx-operators` membership. In a real
project, replace the demonstration application definition and fixed service
name with your application; keep the privileged files administrator-owned.

This example uses the ordinary `group`, `user`, `directory`, `file`,
`shell`, `systemd_unit`, and `systemd_service` tasks and
adds no MMX authorization system or new module. The sticky request
directory comes from `tmpfiles.conf` (mode 1770), not from the `directory`
module, which refuses the sticky bit. See the
[systemd path-unit reference](https://www.freedesktop.org/software/systemd/man/255/systemd.path.html)
for native watcher and rate-limit behavior.
