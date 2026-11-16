# A libvirt host with one virtual machine

A server that runs virtual machines with [libvirt](https://libvirt.org/) and
QEMU, and one virtual machine on it, `shop-db`. libvirt calls a virtual
machine a **domain**. Set the server's address in `inventories/example.yml`,
then run:

```sh
mmx dryrun configure
mmx run configure
mmx run configure
```

On the node, in order, the playbook:

1. on Debian and Ubuntu, tells apt to install no "recommended" packages;
2. installs libvirt, its program `virsh`, QEMU, and `qemu-img` from the
   distribution's own packages;
3. starts libvirt's socket unit and enables it for the next boot;
4. writes the domain's libvirt XML to `/etc/mmx-libvirt/shop-db.xml`, from
   the template `templates/shop-db.xml`;
5. makes the domain's disk, an empty image file, once;
6. defines the domain `shop-db` from the XML file with
   [`libvirt_domain`](../../official_extra_modules/libvirt_domain/README.md),
   starts it, and sets it to start at every boot of the node.

The second run changes nothing. The first dryrun, before anything is
installed, reports the `libvirt_domain` task `not_checked`: the program
`virsh` is not on the node yet.

**The node needs hardware virtualization** (the device `/dev/kvm`): a
physical server, or a virtual server whose provider gives it nested
virtualization.

**The virtual machine is empty.** Its disk holds no operating system, so it
starts and finds nothing to boot. To start from an operating system's image,
replace the task that makes the empty disk with one that puts the image at
`/var/lib/libvirt/images/shop-db.qcow2`. The inventory's `vars` set its
memory, its number of virtual CPUs, and the size of the empty disk.

**The virtual machine has no network card.** Add an `<interface>` to the
template: on a bridge of the node, or on libvirt's network `default`, which
the [module's page](../../official_extra_modules/libvirt_domain/README.md#libvirts-default-network)
shows how to start. Give the card its MAC address in the XML
(`<mac address='52:54:00:12:34:56'/>`): libvirt gives a card without one a
new address each time the file changes.

**ACPI.** `<acpi/>` in the template gives the virtual machine ACPI.
libvirt's documentation says that a KVM domain needs ACPI for a graceful
shutdown, which is what `state: stopped` asks for.

**Changing the virtual machine.** Change the template and run again. The
`file` task writes the XML file, and `libvirt_domain` gives libvirt the new
definition. A dryrun before the run shows the XML and domain changes. A
running virtual machine keeps its old definition until it is shut off and
started again. The `libvirt_domain` task's result says when the running
virtual machine keeps the old definition. MMX never restarts the virtual
machine by itself.

## Distributions

The example works on Debian 12 and 13, Ubuntu 24.04 and 26.04, Rocky Linux 9,
and AlmaLinux 10. The libvirt versions there are 9.0 to 12.0.

What differs between them is in the playbook:

- **The package names.** Debian and Ubuntu, and the Red Hat family, name
  the packages differently.
- **Recommended packages on Debian and Ubuntu.** With apt's default
  setting, the packages of this example bring several hundred more: on
  Debian 13, more than 360 packages instead of about 70. The file
  `/etc/apt/apt.conf.d/90-no-recommended-packages` turns that off for every
  apt command on the node. Remove the task when other packages of the node
  need their recommended packages. If an earlier run created
  `/etc/apt/apt.conf.d/90-no-recommended-packages`, use a `file` task with
  `state: absent` to remove the configuration file.
- **The daemon.** Debian and Ubuntu have one libvirt daemon, `libvirtd`.
  The Red Hat family has one daemon for each kind of work, and
  `virtqemud` is the one for QEMU. The unit to start is the socket: the
  daemon ends by itself while it has nothing to do, and the socket starts
  it again.
- **Starting at boot.** A virtual machine starts when the node boots only
  when the daemon's service is enabled. The packages of the Red Hat family
  enable `virtqemud.service`, and the playbook keeps it enabled.

For every input of the module, what a dryrun reports, and what its errors
mean, see the
[page of `libvirt_domain`](../../official_extra_modules/libvirt_domain/README.md).
