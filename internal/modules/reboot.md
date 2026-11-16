# `reboot`

A `reboot` node task restarts the node and makes the controller process wait
until the node comes back on a new boot. The `reboot` task is the last node
task of a fleet stage, so the reboot marks a visible fleet stage boundary.
Later fleet stages continue on the new boot.

MMX never reboots the machine that runs the controller process. If the
controller machine is selected as a node, a fleet stage ending with `reboot`
fails before the first node task of that fleet stage runs on the controller
machine.
See [MMX never reboots the controller machine](../../docs/how-to/manage-the-controller-machine-itself.md#patch-every-node-and-reboot-the-controller-machine-by-hand).

## Examples

### Reboot after patching, only when an update asks for it

For patching, reboot only when an update requests it, one node at a time:

```yaml
# playbooks/patch.yml
fleet_stages:
  - id: patch                  # a fleet stage ID you choose
    nodes: web
    max_nodes_at_once: 1
    on_node_failure: stop_fleet_stage
    node_tasks:
      - apt_package:
          upgrade_all_packages_within_their_version_rules: true
      - reboot:
          run_only_if_this_command_succeeds: test -f /var/run/reboot-required
```

`/var/run/reboot-required` is the file Debian and Ubuntu create when an
update needs a reboot. Each web node upgrades its packages, then reboots if
`/var/run/reboot-required` exists. The controller process waits for the
rebooting web node to come back before the next node starts.
A node that fails stops the fleet stage.

### Reboot on the Red Hat family

```yaml
- dnf_package:
    upgrade_all_packages_within_their_version_rules: true
- reboot:
    skip_if_this_command_succeeds: needs-restarting -r
    wait_at_most: 15m
```

On the Red Hat family, `needs-restarting -r` exits 0 when no reboot is
needed and 1 when one is, so the task skips the reboot on 0 and reboots on 1.
Any other exit code, such as 127 when `needs-restarting` is not installed,
fails the node task instead of rebooting. Do not write
`run_only_if_this_command_succeeds: "! needs-restarting -r"`: the `!` turns
every nonzero exit code into 0, so a missing program would reboot the node on
every run. The controller process waits up to 15 minutes for the node to
return.

### Reboot on every run

```yaml
- reboot: {}
```

A reboot without fields is written `reboot: {}` and reboots on every run.

## How it works

- **Where it may stand.** `reboot` must be the fleet stage's last node task,
  never inside a role, node task block, or loop. The task also takes `id`
  and `when`.
- **Path guards.** `skip_if_this_path_exists` skips a reboot while its path
  exists; `run_only_if_this_path_exists` skips while its path is missing.
  Relative paths start at `/` on the node.
- **Command guards.** `run_only_if_this_command_succeeds` and
  `skip_if_this_command_succeeds` follow the
  [`shell` guard rules](shell.md#guards); a guard that disallows the reboot
  skips the task. A guard's script runs as written, and `reboot` takes no
  `env`, so validation refuses a reference to a value MMX knows in it, such
  as `test -e {{ .vars.marker }}`. Decide on such a value with the task's
  `when` instead.
- **The sequence.** The node job records the boot it is leaving before
  running `systemctl reboot`. The node job ends with the status `rebooting`,
  and its worker process reboots the node once that result is saved. The
  worker process does not continue across the reboot. The controller process keeps the node's
  concurrency slot (`max_nodes_at_once`) occupied and asks about the node job
  every second. It expects connection failures while waiting for the node to
  answer from a new boot. The [execution design](../../docs/changing-mmx/design/execution.md#reboot-and-wait)
  owns the steps.
- **The result.** A successful reboot result includes `boot_id_before` and
  `boot_id_after` in its value. A node that does not return within
  `wait_at_most`, default `10m`, fails its fleet stage.
- **A lost controller process.** If the controller process disappears during
  the wait, `mmx status` records a completed reboot and `mmx resume`
  continues without rebooting again.
- **Dryrun** runs the guards and reports `would_change` without rebooting.
- **Retries and time limits.** Like [`wait_for`](wait_for.md), `reboot`
  rejects `retry`, `time_limit`, and `time_limit_per_try`. It ignores
  `node_task_defaults` and uses its own `wait_at_most`.

## Errors and what they mean

- **The node does not return within `wait_at_most`.** The node's fleet stage
  fails. Look at the node's console, or give a slow boot a longer
  `wait_at_most`.
- **A `reboot` that is not the last node task of the fleet stage,** or one
  inside a role, node task block, or loop. Validation rejects the misplaced
  `reboot` task. Move the `reboot` task to the end of the fleet stage's
  `node_tasks`.
- **`retry`, `time_limit`, or `time_limit_per_try` on a `reboot` task.**
  Validation rejects `retry`, `time_limit`, and `time_limit_per_try` on a
  `reboot` task.
- **The node is the controller machine.** The fleet stage fails on the
  controller machine before the fleet stage's first node task runs on the
  controller machine, in a dryrun and in a run.
  Run the playbook from another machine, or reboot the controller machine
  by hand after MMX finishes.

## Tested on

**Distributions:** Debian 12 and 13, Ubuntu 24.04 and 26.04, Arch Linux, openSUSE Leap 16.1, Rocky Linux 9, AlmaLinux 10, Fedora 45.

The base suite covers this module (`vm.py test`, `fleet.py`, `controller_machine_as_node.py`) and runs on every tested distribution.

The first-server suite (`vm.py test`) covers the reboot task and its
recovery after a lost controller process. `controller_machine_as_node.py`
checks the refusal on the controller machine, and patches and reboots a
second guest; it ran on Debian 12 and 13 and Ubuntu 24.04 and 26.04 in containers, and on
Hetzner Cloud servers with Debian 13. A `reboot` whose
node never returns, and a refused reboot request, are covered by Go tests,
not on a real node ([validation record](../../docs/changing-mmx/validation.md)).

## Why it works this way

- **A reboot is the last node task of a fleet stage.** The reboot marks a
  visible fleet stage boundary.
- **`retry` and the node task time limits are rejected.** A reboot runs once
  per node job attempt, and `wait_at_most` bounds the controller process's
  wait.
- **MMX never reboots the controller machine.** The reboot would end the
  controller process that waits for the node to come back.
- **Path guards name their condition.** The same guard names on `shell` and
  `reboot` make the skip rule visible without a shell command.
