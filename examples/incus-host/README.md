# An Incus host

A server that runs [Incus](https://linuxcontainers.org/incus/), with one
profile and one container. Incus runs system containers and virtual
machines, called instances. Set the server's address in
`inventories/example.yml`, then run:

```sh
mmx dryrun incus_host
mmx run incus_host
mmx run incus_host
```

The playbook has two fleet stages. On the node, in order:

**`incus_server`: Incus, ready to make instances.**

1. MMX installs Incus from the source of the node's distribution (see
   below).
2. MMX starts and enables `incus.socket`, which starts the Incus server at
   the first `incus` command, and enables the unit that starts the server
   when the node boots.
3. MMX gives root a range of user IDs and group IDs for containers, in
   `/etc/subuid` and `/etc/subgid`, and starts the server.
4. MMX runs `incus admin init --minimal` once. The command makes a storage
   pool, the bridge `incusbr0`, and the root disk and the network card of
   Incus's profile `default`.

**`instances`: one profile and one instance.**

5. The profile `web` limits each of its instances to one CPU and 512 MiB of
   memory.
6. The instance `shop-test` is a container from the image
   `images:debian/13` with the profiles `default` and `web`. The container
   runs and starts again when the node boots. Port 8080 of the node reaches
   port 80 inside `shop-test`.

The second run changes nothing. The first dryrun passes on a node without
Incus: it reports the two Incus tasks as not checked, because the package
is not installed yet.

To make the instance from another image, name the replacement image on the
command line:

```sh
mmx run incus_host --var instance_image=images:ubuntu/24.04
```

`--var` changes only what a new instance is made from. An instance that
exists keeps its image. To apply one stage alone, name it:
`mmx run incus_host --only-fleet-stages incus_server`.

## Distributions

The example works on Debian 12 and 13, Ubuntu 24.04 and 26.04, Rocky Linux
9, and AlmaLinux 10.

| Distribution | Where Incus comes from | Incus |
|---|---|---|
| Debian 13 | Debian's own repository, package `incus-base` | 6.0.4 |
| Debian 12 | Debian's backports repository, package `incus-base` | 6.0.4 |
| Ubuntu 26.04 | Ubuntu's own repository, package `incus-base` | 6.0.5 |
| Ubuntu 24.04 | Ubuntu's own repository, package `incus` | 6.0.0 |
| Rocky Linux 9, AlmaLinux 10 | The COPR repository `neelc/incus`, with EPEL and CRB, package `incus` | 6.23 |

The Incus project names the COPR repository in its installation guide. The
COPR repository is the work of one packager.

**Containers and virtual machines.** `incus-base` on Debian and Ubuntu 26.04
is Incus for containers. To run virtual machine instances there too, install
the package `incus` in its place. Ubuntu 24.04 has no package `incus-base`.

**Recommended packages.** On Debian and Ubuntu the playbook names
`dnsmasq-base`, which Incus needs for its bridge and which its package only
recommends. So the example works on a server whose apt installs no
recommended packages. On Ubuntu 24.04, apt's default also installs the
recommended package `incus-agent`. Do not install `incus-agent` on a node
that is itself a virtual machine of another Incus; the [module's
page](../../official_extra_modules/incus_instance/README.md#set-up) shows
the `file` task that keeps apt from installing recommended packages.

## Why it is written this way

**The packages start nothing.** MMX installs a package without starting its
services, and the packages of the Red Hat family enable none. So the playbook starts and enables `incus.socket` on every
distribution.

**Root's ID range.** A container's accounts are a range of the node's user
IDs and group IDs that belongs to root. Debian's and Ubuntu's packages write
the range. The server reads `/etc/subuid` and `/etc/subgid` only when the
Incus server starts. So the node task block restarts the Incus server when a
line changes.

**`incus admin init --minimal` runs once.** Its guard asks for the root disk
of the profile `default`, the last thing that the command makes. The guard's
exit code 127 means that `incus` is not installed yet, which is the case in
the dryrun of a new node.

**`config_keys_not_listed: remove`.** The two Incus tasks own the whole
configuration of their profile and their instance: a key that someone set by
hand is removed by the next run. Write `keep` to set only the listed keys.

For every input of the two modules, see
[`incus_instance`](../../official_extra_modules/incus_instance/README.md) and
[`incus_profile`](../../official_extra_modules/incus_profile/README.md).
