# `snap_package`

`snap_package` installs snaps on a node and sets the channel each one
follows. The snap service on the node (the package `snapd`) installs a snap
and then keeps it up to date inside its channel by itself. A snap has no
version to state; the channel is the choice.

`snap_package` shares some rules with the other language and tool
installers, such as how a change becomes a tracked input; they are in
[what the installers share](../../docs/how-to/install-languages.md#what-the-installer-modules-share)
and in the design's
[what installer modules share](../../docs/changing-mmx/design/modules-packages.md#what-these-modules-share).
`snap_package` has no `run_as_user_account` and no `env`, because snaps are
installed by root.

## Set up

Debian and Ubuntu package `snapd`. The Red Hat family packages it only in
EPEL. Install the package and start its socket:

```yaml
- when: {equals: ["{{ .node.package_manager }}", apt]}
  apt_package: {packages: [{name: snapd, version: newest_when_first_installed}]}
- when: {equals: ["{{ .node.package_manager }}", dnf]}
  dnf_package: {packages: [{name: snapd, version: newest_when_first_installed}]}

- systemd_service:
    name: snapd.socket
    start_at_boot: true
    state: running

# Classic snaps expect /snap, which the snapd package of the Red Hat
# family does not create.
- when: {equals: ["{{ .node.package_manager }}", dnf]}
  symlink:
    path: /snap
    target: /var/lib/snapd/snap
```

On the Red Hat family, the `dnf_package` task finds `snapd` only after EPEL
is set up on the node.

A freshly installed snapd may still be busy with its own first refresh and
drop a store request that it cannot finish. The `snap_package` task's
retries cover that; a pause of a few seconds between tries helps, such as
`retry: {pause_between_tries: 10s}` on the task.

## Examples

### Install a snap that needs classic confinement

```yaml
- snap_package:
    packages:
      - name: certbot
        channel: latest/stable
        allow_access_outside_snap_sandbox: true
```

`channel` is required on each entry with `state: present`. Each snap follows
its own channel, such as `latest/stable` or `24/stable`. `allow_access_outside_snap_sandbox: true`
installs that snap with snap's "classic" confinement, which gives it the
access of an ordinary program. Some snaps need it. MMX checks
every entry before installing or refreshing any snap, in a run and a dryrun.
A refused task changes nothing and names the access setting to add
to that snap's `packages` entry.

### Remove a snap

```yaml
- snap_package:
    packages:
      - name: certbot
    state: absent
```

## How it works

MMX reads `snap list` and checks the confinement of each snap named in the
task's `packages` list with
[`snap info --verbose`](https://snapcraft.io/docs/explanation/security/snap-confinement/#getting-the-confinement-level).
MMX then installs a missing snap or switches a snap that follows another
channel. `state: absent` removes the snaps without checking confinement.
A dryrun lists the snaps it would install, switch, or remove.

Tracked input: the installed revision of the named snaps. Because snaps
update themselves, a node task block that declares a snap restarts its
service at the next activation after such an update.

## Errors and what they mean

- **`snapd` is not installed.** The task names the missing package `snapd`:
  a run fails, and a dryrun reports the task as not checked, because an
  earlier task may install it.

## Tested on

**Distributions:** Debian 12 and 13, Ubuntu 24.04 and 26.04, Rocky Linux 9, AlmaLinux 10.

The suite `snap_packages.py` installs, switches, and removes the
`hello-world` snap where the distribution packages `snapd`: Debian and
Ubuntu, in a VM, because `snapd` mounts each snap as a squashfs image and
confines it, which an unprivileged container cannot do. On the Red Hat
family, which packages `snapd` only in EPEL, the suite checks that the task
names the missing package.

[Limits on each distribution](../../docs/reference/limits-on-each-distribution.md) says what is limited for this module on [Rocky Linux 9](../../docs/reference/limits-on-each-distribution.md#rocky-linux-9) and [AlmaLinux 10](../../docs/reference/limits-on-each-distribution.md#almalinux-10), and why.

## Why it works this way

- **Each snap chooses its channel and access.** Different snaps may follow
  different channels; access outside the sandbox is a deliberate deviation
  under the shared [protective-choice rule](../../docs/changing-mmx/design/modules.md#protective-choices-come-in-three-levels).

- **An access refusal precedes every change.** Checking all entries first
  keeps an earlier snap from being installed when a later entry is refused.
  A dryrun must report the same refusal as a run.
