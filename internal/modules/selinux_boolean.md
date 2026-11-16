# `selinux_boolean`

SELinux is the access control layer that the Red Hat family turns on by
default. It refuses what its policy does not allow, even to a process that
runs as root: a web server may not read files outside its content
directories, connect to the network, or listen on an unusual port until the
policy says so. `selinux_boolean` sets one SELinux boolean, a switch in the
policy that allows one such thing, such as `httpd_can_network_connect`. It
sets the boolean now and in the stored policy, so the value stays after a
reboot.

Four modules change SELinux, each in one way:

- `selinux_boolean` switches a permission of the policy on or off;
- [`selinux_file_context`](selinux_file_context.md) gives the files of a
  path another label;
- [`selinux_port`](selinux_port.md) gives a network port another type;
- [`selinux_mode`](selinux_mode.md) sets whether SELinux enforces its policy
  or only reports.

## Set up

`selinux_boolean` runs `semanage` and `setsebool` on the node. `setsebool`
is in `policycoreutils`, which every Red Hat family server has. `semanage`
is in `policycoreutils-python-utils`, which many server images leave out, so
install it before the first SELinux node task:

```yaml
- dnf_package:
    packages:
      - {name: policycoreutils-python-utils, version: newest_when_first_installed}
```

## Examples

### Let the web server connect to the network

```yaml
- selinux_boolean:
    name: httpd_can_network_connect
    enabled: true
```

On the node, MMX reads the boolean's running value and its value in the
stored policy. When either differs from `true`, MMX sets both. When both
already are `true`, the node task reports unchanged. On a node where
SELinux is not enabled, the node task reports `skipped`.

The page of [`selinux_file_context`](selinux_file_context.md#serve-a-website-from-its-own-directory)
shows a boolean, a label rule, and a port type together in one node task
block for a web server.

## How it works

- The module inspects before it acts, and changes the running node and the
  stored policy together.
- On a node where SELinux is disabled or not installed, the node task
  reports `skipped` with the reason "SELinux is not enabled on this node".
  The fact `.node.selinux.mode` says `enforcing`, `permissive`, or
  `disabled`
  ([facts](../../docs/changing-mmx/design/configuration.md#facts-and-results-are-observations-not-defaults)).

## Errors and what they mean

| The reason holds | What it means, and what to do |
|---|---|
| `semanage is not installed; install the package policycoreutils-python-utils first` | The node lacks `semanage`; the same reason names `setsebool` and `policycoreutils` when that program is missing. Install the package in an earlier node task; see [Set up](#set-up). A dryrun reports the node task not checked, because an earlier node task may install the package. |

## Tested on

**Distributions:** Rocky Linux 9, AlmaLinux 10, Fedora 45.

The suite `tests/integration/firewalld_and_selinux.py` runs on seven distributions: Debian 12 and 13, Ubuntu 24.04 and 26.04, Rocky Linux 9, AlmaLinux 10, and Fedora 45. On the three distributions that use SELinux, Rocky Linux 9, AlmaLinux 10, and Fedora 45, it runs in a virtual machine with SELinux enforcing, as installed. On the other four it runs in a container without SELinux and
checks that the module reports `skipped`. The [validation
record](../../docs/changing-mmx/validation.md#selinux-and-the-dnf-modules) has the latest results.
