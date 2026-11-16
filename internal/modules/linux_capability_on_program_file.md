# `linux_capability_on_program_file`

A program that needs one privileged operation, such as sending raw network
packets, can get a Linux capability instead of running as root.
`linux_capability_on_program_file` gives a program file Linux capabilities,
through `getcap` and `setcap` on the node. The listed capabilities become
permitted and effective. The required `capabilities_not_listed` says whether
other capabilities are removed (`remove`) or kept with their flags (`keep`).

A service that needs a capability gets it more simply from its unit
(`AmbientCapabilities=` in a drop-in that
[`systemd_unit`](systemd_unit.md) installs), which a package upgrade cannot
remove. This module is for the other cases.

## Set up

`getcap` and `setcap` are in the package `libcap2-bin` on Debian and Ubuntu,
and `libcap` on the Red Hat family:

```yaml
- when: {equals: ["{{ .node.package_manager }}", apt]}
  apt_package: {packages: [{name: libcap2-bin, version: newest_when_first_installed}]}
- when: {equals: ["{{ .node.package_manager }}", dnf]}
  dnf_package: {packages: [{name: libcap, version: newest_when_first_installed}]}
```

## Examples

### Let a program send raw network packets

```yaml
- linux_capability_on_program_file:
    capabilities_not_listed: remove
    path: /usr/local/bin/pinger
    capabilities: [cap_net_raw]
```

On the node, MMX reads the capabilities of `/usr/local/bin/pinger` with
`getcap`. When the capabilities of `/usr/local/bin/pinger` differ from
`cap_net_raw`, permitted and effective, MMX sets the file's capabilities
with `setcap`. The result shows `value.capabilities_before` and
`value.capabilities_after` as `getcap` prints them, such as
`cap_net_raw=ep`. A run where `setcap` fails shows no value, because the
capabilities did not change, so a node task in `on_failure` can read what
happened.

### Remove every capability

```yaml
- linux_capability_on_program_file:
    path: /usr/local/bin/pinger
    state: absent
```

`state: absent` takes only `path` and removes every capability of the file.

## How it works

Installing a new version of the program replaces the file and drops its
capabilities. The node task checks on every run and sets them again, so put
it after the node task that installs the program, in the same node task
block.

Linux stores one effective flag for the whole file, so when an unlisted
capability lacks that flag, `setcap` refuses the result and the task fails
with `setcap`'s message. Dryrun reports this unsupported combination without
running `setcap`.

MMX refuses a symbolic link at the path.

## Errors and what they mean

| The reason holds | What the error means, and what to do |
|---|---|
| `setcap is not installed; install the package libcap2-bin (Debian and Ubuntu) or libcap (Red Hat family) first` | Install the package in an earlier node task; see [Set up](#set-up). The same holds for `getcap`. A dryrun reports the node task not checked, because an earlier node task may install the package. |
| `/usr/local/bin/pinger does not exist yet` | The program file is missing. A dryrun reports the node task not checked, because an earlier node task may install the program. |
| `... is a symbolic link; give the path of the program file itself` | Write the path of the program file itself. |

## Tested on

**Distributions:** Debian 12 and 13, Ubuntu 24.04 and 26.04, Rocky Linux 9, AlmaLinux 10.

The suite `tests/integration/access_control_lists_and_capabilities.py` runs
on Debian 12 and 13, Ubuntu 24.04 and 26.04, Rocky Linux 9, and AlmaLinux 10, in containers on the first four and in virtual machines on the last two. The [validation
record](../../docs/changing-mmx/validation.md) has the latest results.

## Why it works this way

- **The task states what happens to unlisted capabilities.** Requiring an
  explicit choice about unlisted capabilities follows the
  [list-ownership rule](../../docs/changing-mmx/design/modules.md#a-task-that-owns-a-list-says-what-happens-to-the-rest),
  so adding one capability does not silently discard another.
- **The task checks on every run.** A package upgrade replaces the program
  file and drops its capabilities, and the next run sets them again.
