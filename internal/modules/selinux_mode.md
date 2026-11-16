# `selinux_mode`

SELinux is the access control layer that the Red Hat family turns on by
default. It refuses what its policy does not allow, even to a process that
runs as root. `selinux_mode` sets whether SELinux enforces its policy or
only reports what the policy would refuse: `enforcing` (SELinux refuses
what its policy does not allow) or `permissive` (it only reports). MMX
sets the running mode and the `SELINUX=` line in `/etc/selinux/config`,
which the node reads at boot. MMX does not disable SELinux.

Four modules change SELinux, each in one way:

- [`selinux_boolean`](selinux_boolean.md) switches a permission of the
  policy on or off;
- [`selinux_file_context`](selinux_file_context.md) gives the files of a
  path another label;
- [`selinux_port`](selinux_port.md) gives a network port another type;
- `selinux_mode` sets whether SELinux enforces its policy or only reports.

## Examples

### Make SELinux enforce its policy

```yaml
- selinux_mode:
    mode: enforcing
```

On the node, MMX reads the running mode and the `SELINUX=` line. When
either differs, MMX sets the running mode and writes the line. When both
already say `enforcing`, the node task reports unchanged.

### Turn SELinux on for a node that runs without it

```yaml
- selinux_mode:
    mode: enforcing

- reboot: {}
```

On a kernel that runs with SELinux disabled, `selinux_mode` writes the
configuration, creates `/.autorelabel` so the next boot gives every file
its label, and reports `reboot_required: true` in `.this_node_task.value`
until the reboot. The `reboot` node task then restarts the node.

## How it works

- The module inspects before it acts, and changes the running node and
  `/etc/selinux/config` together.
- `disabled` is not a value of `mode`.
- On a node where SELinux is not installed, the node task reports `skipped`
  with the reason "SELinux is not enabled on this node". The fact
  `.node.selinux.mode` says `enforcing`, `permissive`, or `disabled`
  ([facts](../../docs/changing-mmx/design/configuration.md#facts-and-results-are-observations-not-defaults)).

## Errors and what they mean

| The reason holds | What it means, and what to do |
|---|---|
| `the kernel command line has selinux=0, which keeps SELinux off at every boot whatever /etc/selinux/config says; remove it first, ...` | SELinux stays off at every boot whatever the file says, so the node task fails before it changes anything. Remove `selinux=0` from the kernel command line, for example with `grubby --update-kernel ALL --remove-args selinux`, as the reason says. |

## Tested on

**Distributions:** Rocky Linux 9, AlmaLinux 10, Fedora 45.

The suite `tests/integration/firewalld_and_selinux.py` runs on seven distributions: Debian 12 and 13, Ubuntu 24.04 and 26.04, Rocky Linux 9, AlmaLinux 10, and Fedora 45. On the three distributions that use SELinux, Rocky Linux 9, AlmaLinux 10, and Fedora 45, it runs in a virtual machine with SELinux enforcing, as installed. On the other four it runs in a container without SELinux and
checks that the module reports `skipped`. The [validation
record](../../docs/changing-mmx/validation.md) has the latest results.

Not in a suite: `selinux_mode` creating `/.autorelabel` itself on a real
node. Rocky Linux creates the file when it boots with SELinux disabled, so
only Go tests reach that path. The change from disabled to enforcing, the
reboot, and the relabel were tested on a Rocky Linux 9 virtual machine.

## Why it works this way

- **`disabled` is refused.** On Enterprise Linux 9, `SELINUX=disabled` in
  `/etc/selinux/config` no longer disables SELinux. The kernel parameter
  `selinux=0` does that. Use `mode: permissive` to stop enforcement while
  keeping SELinux enabled.
