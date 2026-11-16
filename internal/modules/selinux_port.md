# `selinux_port`

SELinux is the access control layer that the Red Hat family turns on by
default. It refuses what its policy does not allow, even to a process that
runs as root: a web server may not listen on an unusual port until the
policy says so. The policy gives each network port a type, and a service
may listen only on the ports of its own types. `selinux_port` gives a port,
or a range of ports, an SELinux type, such as `http_port_t` for a web
server.

Four modules change SELinux, each in one way:

- [`selinux_boolean`](selinux_boolean.md) switches a permission of the
  policy on or off;
- [`selinux_file_context`](selinux_file_context.md) gives the files of a
  path another label;
- `selinux_port` gives a network port another type;
- [`selinux_mode`](selinux_mode.md) sets whether SELinux enforces its policy
  or only reports.

## Set up

`selinux_port` runs `semanage` on the node. `semanage` is in
`policycoreutils-python-utils`, which many server images leave out, so
install it before the first SELinux node task:

```yaml
- dnf_package:
    packages:
      - {name: policycoreutils-python-utils, version: newest_when_first_installed}
```

## Examples

### Let the web server listen on port 8443

```yaml
- selinux_port:
    port: 8443
    protocol: tcp
    selinux_type: http_port_t
```

On the node, MMX adds the local definition of port 8443 with the type
`http_port_t`. When the port already has another type, MMX changes it,
also when that type comes from the base policy. Put this node task before
`activate` of the node task block that starts the service. The page of
[`selinux_file_context`](selinux_file_context.md#serve-a-website-from-its-own-directory)
shows it in a whole node task block for a web server.

### Remove the local definition

```yaml
- selinux_port:
    port: 8443
    protocol: tcp
    state: absent
```

`state: absent` takes `port` and `protocol`, and removes the local
definition. The base policy's own definition of the port stays.

## How it works

- `port` is a number or a range written `first:last`. `protocol` is `tcp` or
  `udp`.
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
| `semanage is not installed; install the package policycoreutils-python-utils first` | Install the package in an earlier node task; see [Set up](#set-up). A dryrun reports the node task not checked, because an earlier node task may install the package. |

## Tested on

**Distributions:** Rocky Linux 9, AlmaLinux 10, Fedora 45.

The suite `tests/integration/firewalld_and_selinux.py` runs on seven distributions: Debian 12 and 13, Ubuntu 24.04 and 26.04, Rocky Linux 9, AlmaLinux 10, and Fedora 45. On the three distributions that use SELinux, Rocky Linux 9, AlmaLinux 10, and Fedora 45, it runs in a virtual machine with SELinux enforcing, as installed. On the other four it runs in a container without SELinux and
checks that the module reports `skipped`. The [validation
record](../../docs/changing-mmx/validation.md) has the latest results.
