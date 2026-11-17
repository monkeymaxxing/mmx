# `kernel_module`

Some nodes need a kernel module that is not loaded by default, such as
`br_netfilter` on a Kubernetes node. `kernel_module` manages one loadable
kernel module: it loads the module now with `modprobe`, and names it in
`/etc/modules-load.d/mmx-<name>.conf`, which `systemd-modules-load` reads
at every boot. MMX keeps the kernel module's options in
`/etc/modprobe.d/mmx-<name>.conf`.

Use a separate [`sysctl_setting`](sysctl_setting.md) node task to configure
a kernel setting made available by the kernel module.
Place the `sysctl_setting` node task after the `kernel_module` node task.

## Examples

### Load a module for a Kubernetes node

```yaml
- kernel_module:
    name: br_netfilter

- sysctl_setting:
    name: net.bridge.bridge-nf-call-iptables
    value: "1"
```

The first node task loads the kernel module `br_netfilter` on the node now
and at every boot. The setting of the second node task exists only while
`br_netfilter` is loaded, which is why the `kernel_module` node task comes
first. The page of [`sysctl_setting`](sysctl_setting.md#settings-for-a-kubernetes-node)
shows more settings for such a node.

On AlmaLinux 10, `br_netfilter` is in the package
`kernel-modules-extra`; install it with a [`dnf_package`](dnf_package.md)
node task before the `kernel_module` node task. That package installs the
modules for the newest kernel, so a node that has not rebooted into its
newest kernel yet finds the module only after that reboot.

### Load a module with options

```yaml
- kernel_module:
    name: nf_conntrack
    options:
      hashsize: "65536"
```

MMX keeps the options as one `options` line in
`/etc/modprobe.d/mmx-nf_conntrack.conf`, which `modprobe` reads whenever it
loads the module. MMX compares the node task's `options` values with the
values the loaded module shows under `/sys/module/nf_conntrack/parameters/`.

### Unload a module

```yaml
- kernel_module:
    name: br_netfilter
    state: unloaded
```

`state: unloaded` takes only `name`. MMX deletes its two files and unloads
the module with `modprobe -r`.

## How it works

- MMX reads `/proc/modules` and loads a missing module now with `modprobe`.
- MMX writes a module's name with underscores, as the kernel lists it, so
  `br-netfilter` and `br_netfilter` are the same module and the same file.
- A module that is built into the kernel counts as loaded. The
  `kernel_module` node task takes no `options` for a built-in module,
  because `modprobe` cannot give options to the built-in module. Options for
  a built-in module belong on the kernel command line, as
  `<name>.<option>=<value>`, so the node task refuses `options`.
  A built-in module cannot be unloaded either.
- A module that is already loaded with other option values keeps them until
  it is loaded again. MMX never unloads a module that something may use.
  The `kernel_module` node task then writes
  `/etc/modules-load.d/mmx-<name>.conf` and
  `/etc/modprobe.d/mmx-<name>.conf`. The node task then fails.
  The error message says to unload the module
  (`modprobe -r <name>`, or a `kernel_module` node task
  with `state: unloaded`) or to reboot the node. After the unload,
  `mmx resume` loads the module with the new options.
- An option that the module does not show under
  `/sys/module/<name>/parameters/` cannot be compared, so MMX does not
  compare it. An option whose value is yes or no shows as `Y` or `N` under
  `/sys/module/<name>/parameters/`.
  MMX reads `1`, `y`, and `on` as `Y`, and `0`, `n`, and `off` as `N`, as
  the kernel does.
- The result's `value.loaded_before` and `value.loaded_after` say whether
  the module was loaded before the node task and after it, when that
  changes, and `value.files_changed` lists the files MMX wrote or deleted.
  With `--diff`, the result shows the difference of each file. A dryrun
  reports whether the module would be loaded or unloaded and the difference
  of each file.
- MMX's `kernel_module` needs nothing installed beyond what the tested distributions have: it runs `modprobe` from the package `kmod`.

## Errors and what they mean

A failure that another try cannot fix is not tried again: a built-in module
with options or `state: unloaded`, and a module loaded with other options.

| The reason holds | What the error means, and what to do |
|---|---|
| `modprobe cannot load the kernel module ... for the running kernel: ...` | The module does not exist for the running kernel. The reason ends with `modprobe`'s message. MMX wrote no file, so a misspelled name never fails `systemd-modules-load` at the next boot. A dryrun reports the node task not checked, because an earlier node task may install the package that brings the module. |
| `the kernel module ... is loaded with options that differ from the node task (...); ...` | MMX wrote both files. Unload the module (`modprobe -r <name>`, or a `kernel_module` node task with `state: unloaded`) and run again, or reboot the node. |
| `the kernel module ... is built into the kernel, so modprobe cannot set the module options; set the options on the kernel command line instead, as ...` | Put the options on the kernel command line, as `<name>.<option>=<value>`. |
| `the kernel module ... is built into the kernel and cannot be unloaded` | A built-in module cannot be unloaded. |
| `Module bridge is in use` | `state: unloaded` failed with `modprobe`'s message, because something uses the module. |
| `modprobe is not installed; install the package kmod first` | Install `kmod` in an earlier node task. A dryrun reports the `kernel_module` node task as `not_checked`. |

## Tested on

**Distributions:** Debian 12 and 13, Ubuntu 24.04 and 26.04, Rocky Linux 9, AlmaLinux 10.

The suite `tests/integration/kernel_settings.py` runs in a virtual machine
on Debian 12 and 13, Ubuntu 24.04 and 26.04, Rocky Linux 9, and AlmaLinux 10, because kernel modules belong to the
kernel, which a container shares with its host. It checks `/proc/modules`
and `/sys/module`, and runs `systemd-modules-load` as the next boot runs it.
The [validation record](../../docs/changing-mmx/validation.md) has the latest results.

## Why it works this way

- **A module, not a file in `/etc/modules-load.d/`.** Kernel modules are
  common in real projects: 26 independent authors load them in the Ansible
  projects MMX was checked against. MMX's `kernel_module` refuses a name the
  running kernel does not have, before it writes a file.
- **MMX never unloads a module that something may use.** A module loaded
  with other options therefore keeps them, and the node task fails and
  says what to do.

Omitting `options` keeps the saved options file. Writing `options: {}` deletes
`/etc/modprobe.d/mmx-<name>.conf`, following the
[omission rule](../../docs/changing-mmx/design/modules.md#leaving-a-setting-out-never-deletes).
Preserving saved options when `options` is omitted prevents a node task
that only loads a kernel module from discarding saved settings.
