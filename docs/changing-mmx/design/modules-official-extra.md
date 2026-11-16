# MMX: the official extra modules

Part of the [main plan](README.md). This document owns the inputs and
behavior of each official extra module of the first release. An **official
extra module** is a Starlark script that the MMX project writes, tests, and ships
in `official_extra_modules/` beside `mmx`.
[Modules](modules.md#official-extra-modules) owns what every
official extra module shares: where the directory lives, the version check,
the primitives a script calls, and the module description.

| Module | Manages |
|---|---|
| `incus_instance` | One Incus container or virtual machine: its configuration, its devices, and whether it runs. |
| `incus_profile` | One Incus profile: its configuration and its devices. |
| `libvirt_domain` | One libvirt virtual machine: its definition, whether it runs, and whether it starts at boot. |
| `zfs_dataset` | One ZFS filesystem dataset and its properties. |
| `create_partitions_on_empty_disk` | A GPT partition table on a disk that holds nothing yet. |
| `grow_partition_into_free_space_after_it` | One existing partition grows into the free space after it through `growpart`. |
| `lvm_volume_group` | One LVM volume group and its physical volumes. |
| `lvm_logical_volume` | One LVM logical volume, which a later run can grow. |
| `grow_filesystem_to_fill_device` | A mounted ext4 or XFS filesystem, grown to fill its block device. |
| `rabbitmq_virtual_host` | One virtual host of a RabbitMQ server. |
| `rabbitmq_user` | One user of a RabbitMQ server: its password, its tags, and its permissions. |
| `rabbitmq_policy` | One policy of a RabbitMQ virtual host. |
| `rabbitmq_plugin` | Whether one RabbitMQ plugin is enabled. |
| `mongodb_user` | One user of a MongoDB server: its password and its roles. |
| `pg_auto_failover_monitor` | The pg_auto_failover monitor of a formation, the monitor's service, and which nodes belong to the formation. |
| `pg_auto_failover_node` | One PostgreSQL node of a pg_auto_failover formation: its service, its failover settings, maintenance, and its PostgreSQL settings. |
| `pg_auto_failover_primary` | Which node of a formation is the primary: a planned switchover. |
| `pgbackrest_stanza` | One pgBackRest stanza in its repositories. |
| `pgbackrest_restore` | A PostgreSQL cluster restored by pgBackRest into an empty data directory, to the latest moment or a chosen time. |
| `pacman_package` | Packages on Arch Linux through pacman. [Package modules](modules-packages.md#pacmans-own-rules) owns its design, beside the other package modules. |
| `zypper_package` | Packages on openSUSE through zypper. [Package modules](modules-packages.md#zyppers-own-rules) owns its design, beside the other package modules. |
| `zypper_repository` | One software repository and its signing key on openSUSE. [Package modules](modules-packages.md#zyppers-own-rules) owns its design. |

## What every official extra module does

- **The tool must be on the node.** Each module runs the tool's own
  program, such as `incus`, `virsh`, `zfs`, `sfdisk`, `lvs`, `rabbitmqctl`,
  or `mongosh`. When the program is missing, the script returns the status
  `missing_prerequisite` with a reason that names the program and its
  package. A run then fails without another try, and a dryrun reports the
  task `not_checked`, because an earlier node task may install the package.
- **A module manages; it installs nothing.** A package task installs the
  tool, and a `systemd_service` task starts the tool's service. Each module's
  `README.md` shows the package and service tasks for every distribution
  the module is tested on.
- **A dryrun inspects with the tool's read commands and changes nothing.**
  The dryrun reports `would_change` with a `reason` that says what a run
  would do. If the resource already matches the task, the dryrun reports
  `unchanged`.
  Where the tool's service does not answer yet, the dryrun reports
  `not_checked`; a run fails with the tool's message and is tried again, as
  for every module.
- **The reason says what happened.** A run's `reason` names each thing it
  created, changed, or removed.
- **Removing something that holds data is spelled out.** A state that
  destroys data is named `deleted_with_all_its_data`, as on
  `postgresql_database`. A state that removes only a definition, an
  account, or a setting is `absent`.
- **A private value reaches the tool through its standard input,** never
  through a command line. The module description marks such an input
  `private_value: true`, so a secret in the private input does not hide the
  task's result ([the module description](modules.md#the-module-description)).
- **A list the task owns has `<things>_not_listed`** beside it, required
  with the list:
  [`remove` or `keep`](modules.md#a-task-that-owns-a-list-says-what-happens-to-the-rest).
- **Sizes are written with a binary unit:** a whole number and `MiB`, `GiB`,
  or `TiB`, such as `512MiB` or `100GiB`.
- **Each module's directory holds its page.** `README.md` says how to set
  the tool up, what each input means, what a dryrun reports, what the
  module's errors mean, and on which distributions the module is tested.
  `mmx describe-module NAME` prints the inputs, outputs, and examples.

## Incus

[Incus](https://linuxcontainers.org/incus/) runs system containers and
virtual machines. `incus_instance` and `incus_profile` both run the `incus`
program as root on the node and manage the Incus server of that node.
The Incus modules write every name with Incus's prefix `local:` and name
the Incus project in every command. The project is `default` when the task
names no project, so root's own client settings cannot send a command to
another server or to another project.

### An Incus instance

```yaml
- incus_instance:
    name: shop-test
    state: running
    image_when_created: images:debian/13
    instance_type: container
    profiles: [default]
    config:
      limits.cpu: "2"
      limits.memory: 4GiB
      boot.autostart: "true"
    config_keys_not_listed: remove
    devices:
      web:
        type: proxy
        listen: tcp:0.0.0.0:8080
        connect: tcp:127.0.0.1:80
    devices_not_listed: remove
```

| Input | Meaning |
|---|---|
| `name` | Required. The instance's name. |
| `state` | Required. `running`, `stopped`, or `deleted_with_all_its_data`. |
| `image_when_created` | The image a new instance is created from, such as `images:debian/13`. Required when the instance does not exist; used only then. |
| `instance_type` | Required with `running` and `stopped`: `container` or `virtual_machine`. Used when the instance is created. An existing instance whose type differs from `instance_type` fails the task. |
| `profiles` | Required with `running` and `stopped`. Replaces the whole ordered profile list, because a later profile overrides an earlier one. An empty list means no profiles. |
| `config` | Incus configuration keys with their values as text, such as `limits.cpu: "2"`. Needs `config_keys_not_listed`. A key with an empty value fails the task: Incus stores none. |
| `config_keys_not_listed` | `remove` or `keep`. Required with `config`. Incus's own `volatile.*` and `image.*` keys are never removed. |
| `devices` | The instance's own devices by name; each is a mapping of Incus device options as text. Needs `devices_not_listed`. Replaces each listed device's whole definition, also with `devices_not_listed: keep`. An option with an empty value fails the task: Incus stores none. |
| `devices_not_listed` | `remove` or `keep`. Required with `devices`. Devices that a profile gives the instance are not the instance's own and are left alone. |
| `wait_for_shutdown_at_most` | How long `stopped` waits for the instance to shut down. A positive duration of whole seconds, such as `30s` or `2m`, because Incus takes a timeout in whole seconds. The default is `2m`. |
| `power_off_when_shutdown_takes_longer` | `true`: when the wait ends and the instance still runs, the module stops it at once (`incus stop --force`). `false`, the default: the task fails then. |
| `incus_project` | The Incus project that holds the instance. The default is Incus's project `default`. A project that does not exist is a missing prerequisite. |

- **A new instance** is created with `incus init`, with its profiles,
  configuration, and devices, and started when `state` is `running`.
  `profiles: []` creates the instance without any profile. The module checks
  the type: an image of the other type than `instance_type` fails the task, and
  an instance that Incus created with the wrong type is deleted again
  before the instance runs. An image of the node's own Incus that Incus
  does not have is a missing prerequisite: an earlier node task may import
  the image.
- **An existing instance** is never created again from another image. The
  module compares the instance's profiles, configuration, and devices with
  the task. When the profiles, configuration, or devices differ, the
  module reads the instance's whole configuration. The module changes the
  profiles, configuration, and devices, then writes the whole configuration
  back through `incus config edit`, which takes a whole document.
  So Incus's own keys, the description, and every omitted configuration
  setting stay.
- **A key that needs a restart waits for one.** Incus applies most keys to
  a running instance at once. A running container stores a key that needs
  a restart, such as `security.nesting`, and applies it at its next
  start; a running virtual machine refuses such a key, and the task fails
  with Incus's message. The module never restarts an instance by itself.
- **`stopped`** asks the instance to shut down and waits
  `wait_for_shutdown_at_most`. An instance that still runs when the wait
  ends fails the task once: another try would wait as long again. An
  instance that is frozen fails the
  task, which says to resume it first. An ephemeral instance fails the
  task too: Incus deletes such an instance when it stops, and a state that
  destroys data is never implied.
- **`running`** starts a stopped instance and resumes a frozen one.
- **`deleted_with_all_its_data`** stops the instance at once and deletes it
  with its snapshots; it accepts only `name` and `incus_project`. A missing
  instance is unchanged.
- **A new configuration that Incus refuses for an existing instance fails
  the task once,** as invalid inputs: an unknown key, a bad value, or a new
  device that cannot start. Incus stores none of such a document, and
  another try would send it again.
- **A refused `incus init` fails the task as `failed`,** so MMX may try
  again. The module does not read Incus's text, so it cannot tell a refused
  document from a temporary fault, such as fetching the image from another
  server. Incus leaves no instance behind, and another try starts over.
- **A failure that another try would hide fails the task once**
  (`try_again: False`). After `incus init` the instance exists, so MMX's
  next try would report `created: false` for an instance that the task
  created; every failure from there on keeps `created: true`. A failed
  start that leaves an existing instance running has one try as well.
- **Outputs:** `created` is true when the task created the instance.
- **Dryrun:** `would_change` with what a run would do: create the instance,
  start or stop the instance, each configuration key and device a run would
  set or remove, or delete the instance. `not_checked` while the `incus`
  program is missing, the Incus server does not answer, or the Incus project
  or an image of the node's own Incus is missing.

### An Incus profile

```yaml
- incus_profile:
    name: web
    description: Limits and the bridge for web containers
    config:
      limits.cpu: "2"
    config_keys_not_listed: remove
    devices:
      eth0:
        type: nic
        network: incusbr0
    devices_not_listed: remove
```

- **Inputs:** `incus_profile` takes `name`, `state` (`present`, the
  default, or `absent`), `description`, `config` with
  `config_keys_not_listed`, `devices` with `devices_not_listed`, and
  `incus_project`, each as on `incus_instance`.
- **Creation and updates:** `incus_profile` creates a missing profile.
  The module writes a profile whose description, configuration, or devices
  differ through `incus profile edit`. A new profile whose configuration
  Incus refuses is removed again, so a failed run leaves nothing.
- **Deletion:** `absent` removes the profile and accepts only `name` and
  `incus_project`. Incus refuses deletion while an instance uses the
  profile, and the task fails with Incus's message.
- **Shared profiles:** an Incus project that uses profiles from the
  project `default` has no profiles of its own.
  The task fails and says to write the task without `incus_project`.
- **Dryrun:** lists each key and device a run would set or remove.
- **Outputs:** `created` is true when the task created the profile.

`incus profile edit` can save a profile and still fail, when an instance
that uses the profile cannot take the change, such as a device whose port
is in use. The module reads the profile again after a failed write. A
profile that Incus saved fails the task once (`try_again: False`), with
Incus's message; a profile that Incus left as it was fails the task once
as invalid inputs.

Incus is in the repositories of Debian and Ubuntu. For the Red Hat family,
the Incus project names a package repository. Each module's page shows the
tasks for each distribution.

## libvirt

```yaml
- file:
    path: /etc/mmx-libvirt/shop-db.xml
    template: templates/shop-db.xml
    permissions_mode: "0600"

- libvirt_domain:
    name: shop-db
    state: running
    domain_xml_file: /etc/mmx-libvirt/shop-db.xml
    start_at_boot: true
```

A libvirt **domain** is a virtual machine. `libvirt_domain` runs `virsh` as
root. A `file` task writes the domain's libvirt XML to the node, and the
module makes libvirt's definition follow that file.

| Input | Meaning |
|---|---|
| `name` | Required. The domain's name; it must be the `<name>` in the XML file. A name that is a number or has the form of a UUID is refused. |
| `state` | Required. `running`, `stopped`, or `absent`. |
| `domain_xml_file` | The path, on the node, of the domain's libvirt XML. Required to create a domain. Omitted for an existing domain, its stored definition stays; for a missing domain, dryrun reports `not_checked` and a run fails once. |
| `start_at_boot` | `true` or `false`: whether libvirt starts the domain when the node boots (`virsh autostart`). Omitted: the domain's autostart setting stays unchanged. |
| `wait_for_shutdown_at_most` | How long `stopped` waits for the domain's operating system to shut down. A nonnegative duration of whole seconds, such as `30s` or `2m`, because shutdown checks repeat each second. The default is `2m`; `0s` makes one immediate check. |
| `power_off_when_shutdown_takes_longer` | `true`: when the wait ends and the domain still runs, the module cuts its power (`virsh destroy`). `false`, the default: the task fails then. |
| `libvirt_connection_uri` | Which libvirt the module manages. The default is `qemu:///system`. |

- **The definition follows the XML file.** libvirt rewrites a definition
  when it stores it, so the stored XML never equals the file as text. The
  module keeps the SHA-256 of the file it last defined in the domain's own
  libvirt metadata, and defines the domain again when the file's SHA-256
  differs or the domain does not exist.
- **The module gives libvirt the file's text through standard input,**
  and adds the domain's UUID to it. libvirt refuses a second definition of
  a name unless the XML holds the domain's UUID, and a file that a team
  writes usually holds none. A file with its own `<uuid>` is used as it
  is.
- **Give every network card its MAC address in the XML.** libvirt gives a
  card without a MAC address a new address each time the domain is defined,
  and the operating system inside then sees a new card.
- **A running domain keeps its old definition**
  until the domain is shut off and started again.
  The task's reason says the running domain keeps the old definition.
  The module never restarts a domain by itself.
- **`running`** starts a domain that is shut off and resumes one that is
  paused.
- **`stopped`** asks the domain's operating system to shut down
  (`virsh shutdown`) and waits, once: a wait that ended is not tried
  again. A paused domain fails the task, which says to resume it first.
- **`absent`** removes the definition of a domain that is shut off; its
  disk image files, the variables of its UEFI firmware, and the state of
  its emulated TPM stay (`virsh undefine --keep-nvram --keep-tpm`). A
  running domain fails the task, which says to shut it off first: libvirt
  itself would remove the definition and leave the domain running.
  With `state: absent`, `libvirt_domain` accepts only `name` and
  `libvirt_connection_uri`.

  A domain with a saved memory image or with snapshots fails the task, which
  names the command that removes them: libvirt refuses both, and the
  module reads the saved memory image and snapshots first. A saved memory
  image, snapshots, and a domain that still runs are missing prerequisites,
  because an earlier node task may change the domain's state, saved memory
  image, or snapshots.
  A domain with checkpoints also fails as a missing prerequisite, although
  libvirt would not refuse a domain with checkpoints. libvirt removes the
  checkpoint records with the definition, and `absent` removes a definition
  and nothing else.
- **Outputs:** `created` is true when the domain did not exist before. A
  step that fails after the task created the domain is not tried again, so
  the result keeps `created: true`.
- **Fingerprint:** the SHA-256 of a supplied XML file, the requested state,
  and `start_at_boot`, so a node task block can act when the request changes.
  Without an XML file, no definition hash is compared.
- **Dryrun:** `would_change` with what a run would do. When an earlier
  node task of the dryrun would write the XML file, the dryrun compares
  nothing with the file on the node. The dryrun reports that the domain
  would be defined from the changed file.
  The dryrun reports `not_checked` under any of the following conditions:

  - `virsh` is missing.
  - libvirt does not answer.
  - The supplied XML file does not exist and no earlier node task would
    write the XML file.
  - The domain does not exist and the task gives no XML file.
  - With `absent` only, the domain is not shut off or has a saved memory
    image, snapshots, or checkpoints.

  An earlier node task of the run may change the condition that prevents
  the dryrun from checking the task.

## ZFS

```yaml
- zfs_dataset:
    name: tank/clickhouse
    properties:
      compression: lz4
      recordsize: 128K
      sharenfs: "rw=@192.168.110.0/24,no_root_squash"
```

`zfs_dataset` manages one ZFS filesystem dataset through the `zfs` program.

| Input | Meaning |
|---|---|
| `name` | Required. The dataset, such as `tank/clickhouse`. |
| `state` | `present` (the default) or `deleted_with_all_its_data`. |
| `properties` | ZFS properties with their values as text, each under the name that `zfs get` prints. Only the listed properties are managed. |

- **A missing dataset** is created with its properties.
  The dataset's parent must exist: a missing pool or parent dataset fails
  the task and is named. A
  dryrun then reports `not_checked`, because an earlier node task may
  create the parent.
- **An existing dataset** gets each listed property whose value in effect
  differs. A size is compared as a number of bytes, so `128K` equals
  `131072`. A property that ZFS sets only at creation fails the task with
  ZFS's message.
- **ZFS stores some values in another form** than the one written, and the
  form differs between ZFS versions: `posixacl` comes back as `posix`. The
  module reads a property again after the module set the property. When
  the stored value still differs from the task's, the task fails and names
  the form that ZFS stores, so the playbook is corrected once and the task does not
  report a change on every run. The module learns the form only after the
  change, so the task fails without another try and keeps its change and
  its output: `created` stays true for a dataset that the task created.
- **`zfs create` can fail after it created the dataset,** for example when
  a `sharenfs` value needs an NFS server that is not installed. The module
  then looks again, and the reason says that the dataset exists; the next
  run only compares its properties.
- **A property name that ZFS does not know** fails the task and is named.
- **A property's second name,** such as `compress` for `compression`, fails
  the task before anything changes: `zfs get` prints the long name, and
  the reason names it. A task writes one name for a property.
- **`deleted_with_all_its_data`** deletes the dataset with `zfs destroy`.
  ZFS refuses a dataset that has child datasets or snapshots. The module
  lists the child datasets and snapshots first and never passes `-r`.
  The task fails, names the first five child datasets or snapshots, and
  counts the rest.
  An earlier node task in the playbook containing the deletion task may
  delete the child datasets and snapshots, so a dryrun reports the task as
  `not_checked`.
- A dataset that is a volume or a snapshot fails the task: the module
  manages filesystems.
- **Outputs:** `created`.
- **Dryrun:** would create, or each property that would change with its old
  and new value, or would delete. `not_checked` while the `zfs` program,
  the pool, or the parent dataset is missing, while ZFS's kernel module is
  not loaded and cannot be loaded, and while a dataset to delete has child
  datasets or snapshots. A `zfs` command loads the kernel module when it
  can, also in a dryrun.

Pools stay a `shell` task that runs `zpool create` only when the guard
`zpool list` does not find the named pool:
creating a pool takes whole disks and happens once for each server.

ZFS is a kernel module beside its tools. Ubuntu ships both the ZFS kernel
module and tools. On Debian a package builds the kernel module on the node.
A node with Secure Boot on loads only a module that its distribution
signed. Outside Ubuntu, a node with Secure Boot on needs a key that the
node's owner enrolls. The module's page says how ZFS gets onto a node of
each distribution and where the suite tests ZFS.

Special ZFS setup beyond ordinary installation tasks, such as manually
building its kernel module or enrolling a Secure Boot key, is outside MMX's
module setup scope. The suite runs where the prerequisites are available
through the page's installation tasks; stand-in tests and manual measurements cover
other documented tool output. This keeps module validation separate from
building kernel modules.

## Disk partitions

```yaml
- create_partitions_on_empty_disk:
    device: /dev/sdb
    partitions:
      - {name: database, size: 200GiB}
      - {name: backups, size: rest_of_disk}

- create_filesystem_on_empty_device:
    device: /dev/disk/by-partlabel/database
    filesystem_type: ext4
```

The module's one job is to never destroy data, as for
[`create_filesystem_on_empty_device`](../../reference/modules/create_filesystem_on_empty_device.md).
`create_partitions_on_empty_disk` writes a GPT partition table with
`sfdisk` (package `fdisk` on Debian and Ubuntu, `util-linux` on the other
distributions).

| Input | Meaning |
|---|---|
| `device` | Required. The whole disk, such as `/dev/sdb`. |
| `partitions` | Required. The partitions in their order on the disk. |

Each partition has a `name`, a `size`, and an optional `type`:

- `name` is the partition's GPT name: 1 to 36 letters, digits, `-`, `_`,
  and `.`, and not `.` or `..`. Later node tasks use the partition as
  `/dev/disk/by-partlabel/<name>`, which does not depend on how the kernel
  numbers the disk. Two partitions of a node with one name would share
  the `/dev/disk/by-partlabel/<name>` link, so the task fails when the
  name already leads to another disk.
- `size` is a size such as `200GiB`, or `rest_of_disk` for the last
  partition.
- `type` is `linux_filesystem` (the default), `linux_lvm`, `linux_swap`,
  `linux_raid`, or `efi_system`.

The module probes the disk:

- **The disk holds nothing, and nothing uses the disk** (no partition
  table, no signature of any kind, and no other device on the disk):
  the module writes the partition table.
- **A GPT partition table with exactly these partitions** (the same names,
  first sectors, sizes, and types, in the same order): unchanged.
- **Anything else** (other partitions, a filesystem, swap, an LVM physical
  volume), **or a disk in use:** the task fails and names what it found.

`create_partitions_on_empty_disk` never passes a force option, and has no
removal, no resize, and no setting that overrides the disk check.
Grow an existing partition with
`grow_partition_into_free_space_after_it`; see [Growing a partition](#growing-a-partition).

- **The module's own probe is the whole guard.** `sfdisk` itself protects
  only a disk that is in use: on the test nodes it replaced a partition
  table and wrote over a filesystem that was not mounted. The module
  therefore asks `lsblk` whether the device is a whole disk and `wipefs
  --no-act` whether it holds any signature, and runs `sfdisk` only on a
  disk that holds none.
- **A disk can be in use although it holds no signature:** device-mapper
  holds it, as for an encrypted volume without a header, or the kernel
  still has a partition of a partition table that someone wiped. The real
  `sfdisk` refuses such a disk, and `sfdisk --no-act` does not. The module
  asks `lsblk` which devices are on the disk and fails the task, in a
  dryrun as in a run, when there is one, or when the disk is mounted.
- **The module computes each partition's first sector and size itself.**
  The first partition starts at 1 MiB, and each next one follows directly.
  The last usable sector is the disk's number of sectors minus 34, or
  minus 6 with sectors of 4096 bytes: a GPT partition table keeps its
  second copy after the last usable sector.
  A `rest_of_disk` partition ends at the last whole MiB of the disk. With
  sectors written out, every version of `sfdisk` writes the same table,
  and a partition that does not fit fails the task before anything is
  written. Without explicit sectors, `sfdisk` makes a partition that does not fit
  smaller, without an error.
- **After writing, and for a table the module finds,** the module waits for
  the node's device manager (`udevadm settle`) and checks that every
  `/dev/disk/by-partlabel/<name>` exists and leads to the requested disk, so the
  next node task finds the partition. The run after one that wrote the
  table and found no link must not report success. A check or a read that
  fails after the write fails the task without another try, because the
  next try would find the table.
- **A disk that looks empty can hold an old filesystem deeper inside,**
  when someone wiped only its partition table. A new partition that begins
  where an old one began then holds the old filesystem. The module wipes
  nothing: it names every signature it finds in the new partitions in the
  task's reason.
- **Outputs:** `created`.
- **Dryrun** reports the partitions it would create, and fails as a run
  does when they do not fit. A disk that does not exist yet is not checked.

## Growing a partition

`grow_partition_into_free_space_after_it` grows one partition on the node
into the free space immediately after the selected partition, through
`growpart`. The module never shrinks, moves, creates, or removes a
partition, and leaves the partition's contents alone.

| Input | Meaning |
|---|---|
| `device` | Required string. Absolute path of the whole disk, including a `/dev/disk/by-id/` link. |
| `partition_number` | Required integer, 1 or more. The number in the disk's partition table. |

There is no `state` input and no output. The disk is checked like the disk
of `create_partitions_on_empty_disk`; a mounted partition is allowed.
`growpart` comes from `cloud-guest-utils` on Debian and Ubuntu, or
`cloud-utils-growpart` on the Red Hat family.

- `growpart --dry-run <disk> <number>` changes nothing. Exit code 0 and a
  line starting with `CHANGE:` give the old and new sizes in sectors.
  `blockdev --getss <disk>` gives the bytes per sector.
- Exit code 1 and a line starting with `NOCHANGE:` mean unchanged, including
  when another partition follows the selected partition or too little
  space is free.
  Every other outcome fails with growpart's words.
- A missing disk, partition table, or partition is a `missing_prerequisite`:
  an earlier node task may create the missing disk, table, or partition.
  A dryrun reports `not_checked`.
- A dryrun says `would grow partition <number> of <disk> from <old> to <new>`.
  Sizes use the largest binary unit that gives a whole number, else bytes.
- A run uses `growpart <disk> <number>`. Exit code 0 and `CHANGED:` mean
  success only after read-only commands confirm the size in the table and
  in the kernel. The reason is `grew partition <number> of <disk> from
  <old> to <new>` with the size actually read.
- After a failed change the module reads the table again. An unchanged
  size allows another try; a changed size or failed read sets
  `try_again: False`, so another try cannot hide the failure.
- Growpart updates the kernel while a partition is mounted or in use.
  Grow an LVM physical volume next with
  [`lvm_volume_group`](../../../official_extra_modules/lvm_volume_group/README.md),
  or a filesystem with
  [`grow_filesystem_to_fill_device`](../../../official_extra_modules/grow_filesystem_to_fill_device/README.md).

## LVM

```yaml
- lvm_volume_group:
    name: data
    physical_volumes: [/dev/disk/by-partlabel/lvm]
    physical_volumes_not_listed: keep
    grow_physical_volumes_with_their_devices: true

- lvm_logical_volume:
    name: postgresql
    volume_group: data
    size: 50GiB
    grow_filesystem_with_volume: true
```

`lvm_volume_group` and `lvm_logical_volume` run the programs of the package
`lvm2`.

### A volume group

| Input | Meaning |
|---|---|
| `name` | Required. The volume group's name. |
| `state` | `present` (the default) or `absent`. |
| `physical_volumes` | The devices of the volume group. Required for `present`. |
| `physical_volumes_not_listed` | `remove` or `keep`. Required with `physical_volumes`. |
| `grow_physical_volumes_with_their_devices` | Boolean, required for `present`. Grow listed physical volumes already in this volume group when their devices have room for at least one more extent. `false` leaves the listed physical volumes at their current sizes. |

- **A missing volume group** is created on the listed devices
  (`vgcreate`). A device that holds anything, such as a filesystem or a
  partition table, fails the task and is named: the module never wipes a
  device. The module probes each device itself, so a dryrun knows too; LVM
  also refuses such a device.
- **A listed device that the volume group does not have** is added
  (`vgextend`), with the same rule.
- **A listed physical volume already in this volume group** grows with
  `pvresize <device>` when `grow_physical_volumes_with_their_devices` is
  `true` and `floor((dev_size - pe_start - end_area) / vg_extent_size) - pv_pe_count >= 1`.
  `end_area` is 0, or, for a physical volume with a second metadata area
  (`pv_mda_count` above 1), `pv_mda_size` plus 1 MiB: LVM keeps that area at
  the end of the device, where it takes room from the extents (measured with
  LVM 2.03.31). These values come from one `pvs --units b --nosuffix` report.
  A new physical volume takes its whole device and needs no growth.
  Unlisted physical volumes stay as they are. The module never shrinks
  or passes `--setphysicalvolumesize`.

  After `pvresize`, another report must show more extents. The reason
  says `grew the physical volume <device> of the volume group <name> from
  <old size> to <new size>`, using the actual sizes and binary units.
  If `pvresize` succeeds without adding extents, the module fails the task
  with the command's output and the old and new sizes. Another try may
  add the missing extents. A read failure after growth, or a failed command
  that grew the volume, fails without another try because a retry could
  hide the failure.

  A dryrun runs no changing command and says `would grow ...`,
  predicting `floor((dev_size - pe_start - end_area) / vg_extent_size) * vg_extent_size`
  bytes. `pvresize` itself prints “changed” even when it
  gains nothing. With `grow_physical_volumes_with_their_devices: true`,
  space added to a disk by the provider becomes available to later
  logical-volume tasks.

  Physical-volume growth lets logical volumes use added device space,
  just as filesystem growth lets a filesystem use added logical-volume
  space.
- **`physical_volumes_not_listed: remove`** takes a device that the task
  does not list out of the volume group (`vgreduce`). A device that still
  holds data of a logical volume stays: the module reads how much each
  device holds and fails the task before it changes anything. LVM refuses
  such a device too.
- **`absent`** removes a volume group that has no logical volume. A volume
  group with logical volumes fails the task and names the logical volumes.
  `absent` accepts no other input than `name`.
- **A state that an earlier node task may change leaves a dryrun not
  checked:** a device of another volume group, a device that still holds
  data of a logical volume, and a volume group that still has logical
  volumes. A playbook deletes the
  logical volumes and then removes their volume group. During a dryrun,
  the logical volumes remain in the volume group because the deletion
  tasks change nothing. During a run, an unmet prerequisite fails the
  task without another try.
- **A device that leaves a volume group is made clean.** LVM leaves its
  label on such a device, and other tasks would then refuse the device as
  "holds an LVM physical volume". The module removes the label
  (`pvremove`); a device that belongs to no volume group holds no data of
  a logical volume. The exception is a run that ends between `vgreduce`
  and `pvremove`, or whose `pvremove` fails: the device keeps the label.
  A failed `pvremove` fails the task without another try, and the reason
  gives the command to run by hand. The
  [`lvm_volume_group` page](../../../official_extra_modules/lvm_volume_group/README.md#errors-and-what-they-mean)
  explains recovery for a device with an LVM label: `lvm_volume_group`
  accepts the device for addition to a volume group, and
  `pvremove <device>` makes it empty for anything else.
- **A device that device-mapper holds** and that has no signature is
  refused by LVM itself, which requires exclusive access when opening a
  device to add. The module adds no check for a device-mapper holder.
  LVM's read commands cannot predict the exclusive-access refusal, so a
  dryrun can report that the module would add the device. A run fails
  before writing anything.
- **Outputs:** `created`.

### A logical volume

| Input | Meaning |
|---|---|
| `name` | Required. The logical volume's name. |
| `volume_group` | Required. The volume group that holds the logical volume. |
| `state` | `present` (the default) or `deleted_with_all_its_data`. |
| `size` | A size such as `50GiB`, or `all_free_space_when_created`. Required for `present`. |
| `grow_filesystem_with_volume` | Required for `present`. `true`: when the module grows the volume, it also grows the ext2, ext3, ext4, or xfs filesystem on it (`lvextend --resizefs`). `false`: for a volume that holds no such filesystem, such as swap. |
| `lvcreate_options` | More options for `lvcreate` when the volume is made, as a list, such as `["--type", "raid1", "--mirrors", "1"]`. |

- **A missing volume** is created with the requested `size`. The module has
  `lvcreate` wipe an old signature in the space of the new volume
  (`--yes --wipesignatures y`), so a later
  `create_filesystem_on_empty_device` task finds the volume empty. That
  space belongs to no volume, so nothing that exists is lost. A size
  beyond the volume group's free space fails a run before LVM is called,
  for a new volume and for one that would grow. A dryrun reports such a
  task as `not_checked`, because an earlier node task may add a device to
  the volume group.
- **A smaller existing volume grows** to `size`. LVM rounds a size up to
  the volume group's extent size, and the module compares after that
  rounding. The filesystem grows with the volume unless
  `grow_filesystem_with_volume` is `false`. A volume that holds nothing
  yet grows alone, and the reason says so: it has no filesystem that could
  be left small. With `true` on a volume that holds something other than
  an ext2, ext3, ext4, or xfs filesystem, LVM would refuse and grow
  nothing. The module looks at the volume first (`wipefs --no-act`) and
  fails the task, in a dryrun as in a run, with what the volume holds and
  a hint to `grow_filesystem_with_volume`.

  A filesystem grows
  only together with the logical volume. A filesystem left smaller than
  the volume must be grown by hand, with `resize2fs` or `xfs_growfs`.
  The filesystem can remain smaller when the filesystem's own program
  fails after the volume grew, for example for a filesystem mounted
  read-only. The task then fails without another try and says that the
  filesystem is still smaller.
- **A larger existing volume fails the task:** the module never shrinks a
  volume, because that destroys the data behind the new end.
- **`all_free_space_when_created`** gives a new volume all free space; an existing
  volume is unchanged whatever its size.
- **`deleted_with_all_its_data`** deletes the volume (`lvremove --yes`),
  and only that volume. A volume that is in use, such as a mounted one,
  fails the task: the module reads whether the volume is open before it
  asks LVM, which refuses such a volume too. A volume that other volumes
  depend on also fails the task.

  The module names the dependent volumes:
  snapshots of the selected volume, or volumes in the selected thin pool.
  `--yes` would also answer LVM's question about deleting the dependent
  volumes, and LVM would delete volumes that the task does not name.
  An earlier node task may unmount the selected volume or delete the
  dependent snapshots or thin volumes, so a dryrun reports the task as
  `not_checked`. The state accepts no other
  input than `name` and `volume_group`.
- **Outputs:** `created`, and `device`, the volume's path
  `/dev/<volume_group>/<name>`.

Making thin pools and snapshots, and moving data between physical volumes,
stay `shell` tasks.

## RabbitMQ

```yaml
- rabbitmq_plugin:
    name: rabbitmq_management
    state: enabled

- rabbitmq_virtual_host:
    name: shop

- rabbitmq_user:
    name: shop
    password: "{{ .secrets.shop_rabbitmq_password }}"
    tags: [monitoring]
    tags_not_listed: remove
    permissions:
      - {virtual_host: shop, configure: ".*", write: ".*", read: ".*"}
    permissions_not_listed: remove

- rabbitmq_policy:
    name: limit-queue-length
    virtual_host: shop
    pattern: ".*"
    apply_to: queues
    definition:
      max-length: 100000
    priority: 0
```

`rabbitmq_plugin`, `rabbitmq_virtual_host`, `rabbitmq_user`, and
`rabbitmq_policy` run RabbitMQ's own programs on a RabbitMQ server node.
Users, virtual hosts, and policies belong to the whole cluster, so
one node of a cluster runs the user, virtual-host, and policy tasks.

- **`rabbitmqctl` runs as the account `rabbitmq`, without a shell:**
  `runuser -u rabbitmq -- /usr/lib/rabbitmq/bin/rabbitmqctl ...`. The
  `rabbitmqctl` command of Debian and Ubuntu passes its arguments through
  a shell, which changes them: on the test nodes a `$` and a backslash in
  a name or a policy came out different, and a command inside an argument
  ran. RabbitMQ's own wrapper makes the call shown above, and every argument
  arrives as written.
- **The three modules that need the server ask first whether the server runs**
  (`rabbitmq-diagnostics check_running`). A server that is still starting
  counts as not answering.
- **A name that is empty or starts with a hyphen fails the task** before
  any command: the name of a virtual host, a user, a policy, or a plugin,
  and a user tag. RabbitMQ's programs read an argument that starts with a
  hyphen as an option, and `rabbitmqctl` creates a virtual host with an
  empty name. A regular expression and a policy's pattern may start with a
  hyphen: the module writes `--` before them, which ends the options.

- **`rabbitmq_virtual_host`** takes `name` and `state` (`present`, the
  default, or `deleted_with_all_its_data`, which deletes the virtual host
  with its queues and messages).
- **`rabbitmq_user`** takes `name`, `state` (`present`, the default, or
  `absent`), `password`, `tags` with `tags_not_listed`, and `permissions`
  with `permissions_not_listed`.
  - `password` is a private value. Omitted: the password of an existing
    user is not touched. A task without `password` for a user that does
    not exist is a missing prerequisite: an earlier node task may create
    the user.
    The module asks the server whether the password is the user's
    (`rabbitmqctl authenticate_user`) and sets it only when the server says
    no. RabbitMQ takes only the first line of a password and cuts white
    space off both ends.

    The module refuses an empty password or a
    password that RabbitMQ would change by taking only the first line or
    trimming white space. Passwords must not start or end with white space
    or contain a line break.
  - `tags` are RabbitMQ's user tags, such as `administrator`,
    `monitoring`, and `management`. Omitted: existing tags stay.
    With `tags_not_listed: keep`, the module adds requested tags and keeps
    existing tags.
  - Each permission names a `virtual_host` and the three regular
    expressions `configure`, `write`, and `read`; all four are required.
    `permissions_not_listed: remove` clears the user's permissions on every
    virtual host that the task does not list.
    A permission for a missing virtual host is a missing prerequisite.
    The task makes no changes; an earlier node task may create the virtual
    host.
- **`rabbitmq_policy`** takes `name` and `virtual_host` (both required),
  `state` (`present`, the default, or `absent`), `pattern`, `apply_to`
  (`queues`, `exchanges`, `all`, `classic_queues`, `quorum_queues`, or
  `streams`), `definition` (a mapping of RabbitMQ's policy keys), and
  `priority` (a whole number). `pattern`, `apply_to`, `definition`, and
  `priority` are required for `present`. RabbitMQ before 3.12 takes only
  `queues`, `exchanges`, and `all`; the task fails there with RabbitMQ's
  message for another value.
- **`rabbitmq_plugin`** takes `name` and `state` (`enabled` or `disabled_with_plugins_that_depend_on_it`,
  required). `rabbitmq_plugin` works while the RabbitMQ server runs:
  enabling or disabling the plugin starts or stops the plugin at once.
  When RabbitMQ is stopped, enabling or disabling the plugin takes effect
  when the server next starts. A plugin that is on only because another
  plugin needs it is not `enabled` yet: the task enables it by name, so it
  stays on when the other plugin goes. `disabled_with_plugins_that_depend_on_it` also turns off every
  enabled plugin that needs the named plugin, as RabbitMQ does; the reason
  names the dependent plugins. A plugin name that RabbitMQ does not have
  is a missing prerequisite:
  a newer RabbitMQ would write any name into its list of enabled plugins.
- **Outputs:** `rabbitmq_virtual_host`, `rabbitmq_user`, and
  `rabbitmq_policy` return `created`, true when the task created the
  virtual host, the user, or the policy. `rabbitmq_plugin` has no output.
- **Dryrun** for all four: `would_change` with what a run would do.
  `not_checked` while the programs are missing, and, except for
  `rabbitmq_plugin`, while the server does not answer.

Queues, exchanges, and bindings are declared by the applications that use
them, or by RabbitMQ's definitions file, written with a `file` task.

Debian and Ubuntu package RabbitMQ. For the Red Hat family, the RabbitMQ
project serves package repositories.
Each module's page shows the tasks for each distribution.

## MongoDB

```yaml
- mongodb_user:
    name: shop
    database: shop
    password: "{{ .secrets.shop_mongodb_password }}"
    roles:
      - {role: readWrite, database: shop}
    roles_not_listed: remove
    login_user: admin
    login_password: "{{ .secrets.mongodb_admin_password }}"
```

`mongodb_user` runs MongoDB's shell `mongosh` as root on the node of the
MongoDB server.

| Input | Meaning |
|---|---|
| `name` | Required. The user's name. |
| `database` | Required. The database that holds the user, which is the database the user logs in to. |
| `state` | `present` (the default) or `absent`. `absent` accepts no `password`, `roles`, or `roles_not_listed`. |
| `password` | The user's password, a private value. Omitted: the password of an existing user is not touched, and the task creates no user. A user that does not exist is then a missing prerequisite, because an earlier `mongodb_user` task of the playbook may create it. |
| `roles` | The user's roles; each names a `role` and the `database` it applies to. Needs `roles_not_listed`. |
| `roles_not_listed` | `remove` or `keep`. Required with `roles`. |
| `login_user` | The user that MMX logs in as, a private value; it must be allowed to manage users. |
| `login_password` | The password of `login_user`, a private value. Required with `login_user`. |
| `login_database` | The database that holds `login_user`. The default is `admin`. |
| `mongodb_connection_string` | How `mongosh` reaches the server. The default is `mongodb://127.0.0.1:27017/?directConnection=true`. `mongodb_connection_string` must hold no user name and no password. |

- **The module reads which of three states the server is in,** without a
  login:
  - *Access control is off.* No login is needed, and the task works without
    one.
  - *Access control is on, and the server has no user.* MongoDB then lets a
    connection from the server's own machine create one user: the first
    administrator. The module creates the task's user without a login when
    `database` is `admin`, every role is on `admin`, and one role is
    `root`, `userAdminAnyDatabase`, or `userAdmin`. A user in `admin`
    without such a role fails the task: MongoDB would accept it, and
    nobody could create a user afterwards. Every other task returns a
    missing prerequisite that says to create the first user in `admin`
    first, so a dryrun reports it `not_checked`.
  - *A login is needed.* The module logs in as `login_user`. A task without
    `login_user` fails, and a refused login fails the task, in a dryrun as
    in a run.

  MongoDB 8.2 and newer also allow the first administrator when users exist
  only outside `admin`. Before MongoDB 8.2, create the administrator before
  turning access control on if users already exist outside `admin`.
- **The first administrator's task supplies the new administrator as the
  login,** so the task's later runs can log in:

  ```yaml
  - mongodb_user:
      name: admin
      database: admin
      password: "{{ .secrets.mongodb_admin_password }}"
      roles:
        - {role: root, database: admin}
      roles_not_listed: remove
      login_user: admin
      login_password: "{{ .secrets.mongodb_admin_password }}"
  ```

- **The password is compared by one login as the user,** from the node.
  The module sets the password only when that login is refused.
- **`mongosh` writes nothing on the node.** The module runs
  `env HOME=/dev/null mongosh --quiet --norc --nodb --file /dev/stdin`.
  The module sends a JavaScript program containing the node task's values
  to `mongosh` through standard input. The program opens the connection.
  With `HOME=/dev/null`, `mongosh` cannot write the log file for each run in
  `/root/.mongodb/`, which can hold the program's text.
  The module needs `mongosh` 2.2.0 or newer and says so when it finds an
  older one.
- **A role that does not exist** is a missing prerequisite that names the
  role and its database.
- **A server that is not the primary of its replica set** fails the task.
  When the replica set has another primary, the reason names the primary, and the
  task has one try. When the replica set has no primary now, as in the
  seconds after a `shell` task initiated the replica set, a run is tried
  again and a dryrun reports `not_checked`. With `replicaSet=<name>` in
  `mongodb_connection_string`, and without `directConnection=true`,
  `mongosh` goes to the primary by itself.
- **A name that MongoDB cannot take for a database** is a rejected input of
  the input that holds the database name: `database`, `login_database`,
  or a role's `database`.
- **A creation that ends without its answer** is not tried again when the
  server has the user or the module cannot find out: the next try would
  report `created: false`. No failure of a task that created the user is
  tried again.
- **Outputs:** `created`.
- **Dryrun:** `would_change` with what a run would do. `not_checked` while
  `mongosh` is missing, while the server does not answer, while the replica
  set has no primary, while the server has no administrator yet and the
  task is not the first administrator's, and while the user does not exist
  and the task has no `password`.

MongoDB serves its own package repositories for Debian, Ubuntu, and the
Red Hat family. The module's page shows
the tasks that add the repository and install the server and `mongosh`.

Replica set configuration stays a `shell` task.

A dryrun cannot infer that an earlier task would create a database login
user: no file read reports the predicted user creation.
A refused database login fails the dryrun, as for MySQL and PostgreSQL.
The dryrun does not guess that a later run would authenticate.
Create the database login user first before relying on the dryrun's
inspection.

## pg_auto_failover

[pg_auto_failover](https://pg-auto-failover.readthedocs.io/) keeps a
PostgreSQL primary and its replicas in one *formation* and promotes a
replica when the primary fails. A *monitor* is a separate, small PostgreSQL
instance on another server. The monitor watches the nodes and decides
which node is the primary. Each database server runs a *keeper*,
`pg_autoctl run`, which manages the database server's PostgreSQL.

Debian and Ubuntu package pg_auto_failover as `pg-auto-failover-cli` and
`postgresql-<version>-auto-failover`: version 2.0 on Debian 12, 2.1 on
Ubuntu 24.04, and 2.2 on Debian 13 and Ubuntu 26.04.

Three modules manage pg_auto_failover. Each module runs `pg_autoctl` as
the account `postgres`:

- `pg_auto_failover_monitor` on the monitor's server: the monitor, its
  service, which nodes belong to the formation, and the current primary as
  an output.
- `pg_auto_failover_node` on each database server: the node, its service,
  its failover settings, maintenance, and its PostgreSQL settings.
- `pg_auto_failover_primary` on the monitor's server: a planned switchover
  to a chosen node.

What every task of the three shares:

- **One instance per server.** pg_autoctl's systemd unit is
  `pgautofailover.service`, from `pg_autoctl show systemd`; the module
  writes it after a successful creation, never before, because the command
  prints nothing for a data directory that does not exist yet and an empty
  unit file reads as masked.
- **TLS and passwords.** Every instance is created with
  `--ssl-self-signed` and `--auth scram-sha-256`, so no password crosses
  the network in clear. Two logins need passwords, both private values
  from the vault: `autoctl_node`, which nodes use to reach the monitor, and
  `pgautofailover_replicator`, which replicas use to copy from the primary.
  `pg_auto_failover_node` keeps one line for each in the `postgres`
  account's `~/.pgpass` (mode `0600`), and leaves the file's other lines
  alone: `*:*:pg_auto_failover:autoctl_node:...` and
  `*:*:*:pgautofailover_replicator:...`, whose database field `*` a
  replica's connection needs. The password file reaches `install` through
  standard input as a new file beside the old password file.
  Then `mv` renames the new password file into place.
  `install` cannot write over an existing file from standard input.
  The monitor's server needs no password file. Every `pg_autoctl` command
  on the monitor's server names the monitor's data directory and reaches
  the monitor through the local socket, as the ident map below allows.
- **Local logins ask who you are.** pg_autoctl runs `initdb --auth=trust`,
  so `pg_hba.conf` starts with six lines that let every account of the
  server log in as any role, `postgres` included, without a password: on
  the local socket (`local all all` and `local replication all`) and on
  `127.0.0.1/32` and `::1/128` (`host all all` and `host replication all`).
  On the test guests another account logged in as `postgres` over
  `127.0.0.1` and on the socket. The monitor's and the node's tasks replace
  `trust` in exactly these lines with Debian's own methods, `peer` on the
  socket and `scram-sha-256` on the loopback addresses, and reload
  PostgreSQL (`pg_ctl reload`). The monitor's and the node's tasks check
  on every run, so a trust line that comes back, such as with a copy from
  another node or a restored cluster, is replaced again, and a dryrun names
  the lines. Every other
  line stays, pg_autoctl's own `hostssl` lines included, among them a
  node's `hostssl all pgautofailover_monitor <monitor>/32 trust`: the
  monitor's health check logs in with it, from the monitor's address only.
  On the monitor, `local all all` becomes `peer map=pg_autoctl`, and the
  task keeps two lines in `pg_ident.conf`: `pg_autoctl postgres
  autoctl_node`, because pg_autoctl logs in to the monitor's database on
  the socket as `autoctl_node`, the user name of its configuration, and
  `pg_autoctl /^(.*)$ \1`, so every other account logs in as the role of
  its own name, as plain `peer` allows. With plain `peer`, `pg_autoctl
  show state` on the monitor failed with "Peer authentication failed for
  user autoctl_node".
- **Where PostgreSQL listens.** `listen_address` is required for `present`
  on the monitor and for `present` and `in_maintenance` on a node: one IP
  address of the server, or `*` for every address. `host_name` is the
  address that others connect to; `listen_address` is where PostgreSQL
  listens, usually the same private address. Creation passes `listen_address` as
  `--listen`. pg_autoctl keeps `listen_address` as
  `postgresql.listen_addresses` in pg_autoctl's configuration (2.0 to 2.2)
  and starts PostgreSQL with `listen_address` (`postgres -h`).
  For an existing instance with another value, the module stops
  `pgautofailover.service`, runs `pg_autoctl config set
  postgresql.listen_addresses`, and starts the service again: while the
  service ran, the running pg_autoctl wrote its old value back on the test
  guests. The module then waits, at most a minute, until `ss -ltnH` shows
  PostgreSQL on the task's port at `listen_address` alone (or at `0.0.0.0`
  and `::` for `*`) and PostgreSQL accepts connections.
  Otherwise the task fails once.

  pg_autoctl's own connections need no loopback address: with
  `listen_address` set to each guest's address, the keepers, the monitor,
  replication, a switchover, and a failover worked, and `127.0.0.1:5432`
  refused connections.
- **Debian's own cluster.** Installing `postgresql-<version>` creates a
  cluster `main` on port 5432 unless
  `/etc/postgresql-common/createcluster.conf` says
  `create_main_cluster = false` before the installation. A cluster listed by
  `pg_lsclusters` on the task's port is a missing prerequisite whose
  reason names both ways out:
  `/etc/postgresql-common/createcluster.conf` before the package task, or
  `pg_dropcluster --stop <version> main` after the package task. The module's
  page shows the configuration file before the package task.
- **The data directory's parent** must belong to `postgres`; pg_autoctl
  creates a `backup` directory beside the data directory. A parent owned
  by root is a missing prerequisite that names it.
- **Time.** Creating a monitor or a first node takes 15 to 20 seconds. A
  node that joins copies the primary with `pg_basebackup`, which first
  waits for a checkpoint on the primary: on the test guests, 17 seconds to
  2 minutes. The node module's page tells the task to set `time_limit` for a join.
- **pg_autoctl tunes PostgreSQL from the server's memory,** such as
  `shared_buffers`, in its own included file at the top of
  `postgresql.conf`. `pg_auto_failover_node`'s `postgresql_settings` come
  later and win (below); a setting that a `line` task appends to
  `postgresql.conf` works too, and the page of `pgbackrest_stanza` shows the
  archiving settings that way.

### The monitor

```yaml
- pg_auto_failover_monitor:
    data_directory: /var/lib/postgresql/18/monitor
    postgresql_major_version: 18
    port: 5432
    host_name: 10.20.0.31
    listen_address: 10.20.0.31
    autoctl_node_password: "{{ .secrets.autoctl_node_password }}"
    formation: default
    node_names: [db01, db02]
    nodes_not_listed: remove
```

| Input | Meaning |
|---|---|
| `data_directory` | Required. The monitor's PostgreSQL data directory. |
| `postgresql_major_version` | Required. The PostgreSQL version whose programs run the monitor, such as `18`: `/usr/lib/postgresql/<version>/bin/pg_ctl`. |
| `port` | Required. The monitor's PostgreSQL port. |
| `host_name` | Required. The host name or IP address that the nodes use to reach the monitor, such as its address on a private network. pg_autoctl allows `autoctl_node` from that address's subnet. |
| `listen_address` | Required. The IP address on which the monitor's PostgreSQL listens, or `*`; usually the same address as `host_name`. |
| `autoctl_node_password` | Required, a private value. The password of the monitor's login `autoctl_node`. |
| `formation` | Required. The formation whose nodes `node_names` lists; pg_auto_failover's first formation is `default`. The module creates a formation that the monitor lacks (`pg_autoctl create formation --kind pgsql`). |
| `node_names` | Required. The names of the nodes that belong to the formation, such as an inventory group's sorted node list, `"{{ .inventory.groups.database_servers }}"`. A listed node that has not registered yet is not an error: each node registers itself. |
| `nodes_not_listed` | Required. `remove` drops a registered node that `node_names` does not list; `keep` leaves it. |
| `state` | `present` (the default) or `deleted_with_all_its_data`, which stops the monitor's service, removes the monitor's unit, and runs `pg_autoctl drop monitor --destroy`. |

- **Creation:** the module runs `pg_autoctl create monitor` when the data
  directory holds no monitor, then writes the monitor's unit, then runs
  `systemctl enable --now`. A data directory whose pg_autoctl configuration
  has another role, such as a node's, fails the task.
  A configuration without the unit is a creation
  that stopped partway: the next run runs `pg_autoctl create monitor`
  again, which finishes it, and reports `created: false`.
- **The password is compared by one login as `autoctl_node`** over TCP
  with TLS to `host_name`, with the password on `psql`'s standard input,
  and set through `psql` on the local socket only when that login is
  refused, as `mongodb_user` does. `psql`'s `\password` reads the new
  password from its standard input and sends the server only the
  SCRAM-SHA-256 form. A login that the monitor still refuses after the
  password change fails the task once with `psql`'s message.
- **Dropping a node** uses `pg_autoctl drop node --force` from the
  monitor's server, so a server that is gone can be dropped: without
  `--force` the command waits for the node to confirm and fails after a
  minute. The task refuses to drop a node that the monitor reports
  healthy and has made the primary: that would cause a failover or remove
  the only copy that runs. An unhealthy primary may be dropped, as when
  every database server is lost and a restored one takes its place.
- **The primary is the monitor's decision.** A node is the formation's
  primary when the monitor has assigned it `single`, `wait_primary`,
  `primary`, `join_primary`, or `apply_settings`: the states in which
  pg_auto_failover's monitor lets a node take writes
  (`CanTakeWritesInState` in its source, 2.0 to 2.2). The state that a
  node reports does not decide: after a failover on the test guests, the
  old primary, whose service was stopped, still reported `primary` while
  the monitor had assigned it `demoted` and `db02` `wait_primary`.

  The monitor's own `pgautofailover.get_primary` does not determine MMX's
  choice of primary. When no node may take writes, the function names a
  primary undergoing demotion. No node could take writes for 30 seconds
  during the failover on the test guests described above.

  MMX uses the monitor's assigned states to determine `primary_node_name`,
  the node's `is_primary`, and `pg_auto_failover_primary`'s unchanged check.
  The refusal to drop a healthy primary also uses the monitor's assigned
  states.
- **Outputs:**

  - `created`.
  - `primary_node_name`: the name of the only node with a primary's
    assigned state. The value is empty text when no node or multiple nodes
    have a primary's assigned state, as during a failover.
  - `nodes`: one entry for each registered node, from
    `pg_autoctl show state --json`. Each entry has `name`, `host_name`,
    `port`, `current_state`, `assigned_state`, `healthy`,
    `candidate_priority`, and `replication_quorum`.
  - `node_monitor_uri`: the monitor's connection URI without a password.

  A later fleet stage reads `primary_node_name` to run work on the primary.
  The monitor decides which node is the primary, never the inventory.
- **Dryrun:** `would_change` for a monitor to create, a service to start, a
  password that the login refuses, and each node the monitor module would
  drop;
  `not_checked` while `pg_autoctl` or the PostgreSQL programs are missing.

### A node

```yaml
- pg_auto_failover_node:
    name: db01
    data_directory: /var/lib/postgresql/18/main
    postgresql_major_version: 18
    port: 5432
    host_name: 10.20.0.21
    listen_address: 10.20.0.21
    monitor_host_name: 10.20.0.31
    monitor_port: 5432
    autoctl_node_password: "{{ .secrets.autoctl_node_password }}"
    replication_password: "{{ .secrets.pgautofailover_replicator_password }}"
    formation: default
    candidate_priority: 50
    replication_quorum: true
```

| Input | Meaning |
|---|---|
| `name` | Required. The node's name in the formation, such as the inventory name. A dropped node's name cannot be used again at once; a replaced server gets a new name. |
| `data_directory`, `postgresql_major_version`, `port`, `host_name`, `listen_address` | Required, as for the monitor. `host_name` is the address the other nodes and the monitor use for this node; `listen_address` is where its PostgreSQL listens. |
| `monitor_host_name`, `monitor_port` | Required. Where the monitor answers. |
| `autoctl_node_password` | Required, a private value. The monitor's `autoctl_node` password. |
| `replication_password` | Required, a private value. The password of `pgautofailover_replicator`, the same on every node of the formation. |
| `formation` | Required. The formation the node joins. |
| `candidate_priority` | Required. 0 to 100; the monitor promotes the healthy replica with the highest priority, and 0 means never. |
| `replication_quorum` | Required. `true` makes a write wait for this node while it is a healthy replica: synchronous replication. |
| `postgresql_settings` | Optional. A mapping of PostgreSQL's setting names, in lower case and with at most one dot, to values described below. When omitted, the module leaves `/etc/postgresql/pg_auto_failover_node_settings.conf` alone. When supplied, even as `{}`, the module manages `/etc/postgresql/pg_auto_failover_node_settings.conf`. |
| `restart_postgresql_for_new_settings` | Required with `postgresql_settings`. `never`, `on_a_replica`, or `on_a_replica_or_a_node_alone`: what the task does with a setting that waits for a restart of PostgreSQL. |
| `state` | `present` (the default), `in_maintenance`, `absent`, or `deleted_with_all_its_data`. |

`postgresql_settings` accepts the following value forms:

- Text, a number, or a boolean written as `on` or `off`.
- A share of the node's memory, using
  `{percent_of_memory, rounded_down_to_mib, at_least_mib}`, which the module
  works out from `MemTotal` and writes in `MB`.
- A reference. A task's references are data, not expressions, so a value
  worked out from the memory uses the module's own form.

- **Creation:** the module writes the two `.pgpass` lines, logs in to the
  monitor once as `autoctl_node` with `psql` (not in a dryrun, and only
  when it is about to create the node), runs
  `pg_autoctl create postgres` with the task's settings, writes the unit,
  and starts it. The first node of a formation becomes its primary, in
  state `single`; a later node copies the primary and joins as a replica.
  pg_autoctl's own `replication.password` stays unset: a replica's
  PostgreSQL finds the password in `~postgres/.pgpass`, and `pg_autoctl
  config set` would take it on its command line and keep it in a file that
  every account can read.

  The login fails the task when the monitor answers `no pg_hba.conf
  entry`: `pg_autoctl create monitor` lets `autoctl_node` in only from the
  subnet of the monitor's address, and `pg_autoctl create postgres` tries a
  refused login again without end, so a node outside that subnet would make
  `mmx run` wait with no message. Every other outcome, such as a monitor
  that does not answer within five seconds, leaves the creation to
  pg_autoctl.

  A creation that stopped partway, a configuration
  without the unit, is finished by the next run, which runs
  `pg_autoctl create postgres` again, as pg_autoctl allows.
  If the first `pg_autoctl create postgres` invocation writes the
  configuration and fails, the module immediately runs `pg_autoctl create
  postgres` once more. If creation fails after
  pg_autoctl wrote the configuration, the task fails without another try.

  A data directory that already holds a PostgreSQL cluster, such as a
  cluster that `pgbackrest_restore` restored, is
  registered as it is: as the primary of a formation that has no node.
  `pg_autoctl create postgres` refuses a cluster that runs, so the module
  stops it first (`pg_ctl stop --mode fast`), and the node's service starts
  it again. While the formation has nodes, any data directory already
  holding a PostgreSQL cluster is a missing prerequisite.
  pg_autoctl would replace the existing cluster with a copy of the primary.
- **On the primary, the replication password is set** with `psql`'s
  `\password` when the module cannot know whether
  `pgautofailover_replicator` has the task's password:

  - when `pgautofailover_replicator` has no password,
  - when the task created the node,
  - when `~postgres/.pgpass` held a different replication password before
    the task.

  No login can compare the task's replication password on the primary:
  pg_autoctl lets `pgautofailover_replicator` in only from the remaining
  nodes' addresses, and the loopback addresses need not be open.
  Setting the replication password lets a node joining later copy the
  primary's PostgreSQL cluster.
- **Synchronous replication needs no further configuration:** with
  one replica whose `replication_quorum` is true, pg_auto_failover sets
  `synchronous_standby_names` to `ANY 1 (...)`, and while that replica is
  down or in maintenance it clears it, so writes go on.
- **`candidate_priority` and `replication_quorum`** are read from the
  monitor's `pg_autoctl show state --json`, which holds both, and set with
  `pg_autoctl set node` when they differ.
- **`in_maintenance`** runs `pg_autoctl enable maintenance` on a replica
  and waits until the monitor reports `maintenance`; `present` on a node in
  maintenance runs `disable maintenance`. The module reads the state
  first, because a second `disable` fails. On the primary, `in_maintenance`
  is a missing prerequisite whose reason says to switch the primary first
  with `pg_auto_failover_primary`: enabling it there would cause a
  failover that the task did not ask for.
- **A new `listen_address` on the primary** of a formation with other
  nodes is a missing prerequisite. The reason says to switch the primary
  first with `pg_auto_failover_primary`. On the test guests, the monitor
  failed over to the replica each time the primary's
  `pgautofailover.service` restarted. A replica, the only node of a
  formation, and the monitor apply the new `listen_address` with the
  service restart.
- **`absent`** first stops and disables the node's
  `pgautofailover.service`, because `pg_autoctl drop node` ends a running
  keeper and systemd would start the keeper again.
  The module then drops the node from the formation (`pg_autoctl drop
  node`) where the monitor lists the node, and removes the node's unit.
  The data directory stays.
  `deleted_with_all_its_data` also removes the data directory: with
  `pg_autoctl drop node --destroy` for a node that the monitor lists. For
  a database node that the monitor's task dropped, `--destroy` fails,
  because `--destroy` asks the monitor first.
  So the module stops the cluster and removes the data directory and
  pg_autoctl's configuration and state, only while the
  monitor answers.

  A node dropped from the monitor's side whose keeper still runs exits
  and restarts for ever. `absent` on the dropped node's server stops the
  keeper.
- **PostgreSQL settings.** With `postgresql_settings`, the module keeps
  `/etc/postgresql/pg_auto_failover_node_settings.conf` (`root:postgres`,
  `0644`): a header comment that names the module, then `name = 'value'`
  per setting, sorted by name, with quotes and backslashes doubled. The
  data directory's `postgresql.conf` ends with `include_if_exists =
  '/etc/postgresql/pg_auto_failover_node_settings.conf'`.
  Whenever the settings include is not the last setting line, the module
  adds or moves `include_if_exists =
  '/etc/postgresql/pg_auto_failover_node_settings.conf'` to the end of
  `postgresql.conf`. pg_autoctl includes its own files at the top of
  `postgresql.conf` (2.0 and 2.2), so the module's file wins over them and
  over initdb's lines.
  `/etc/postgresql/pg_auto_failover_node_settings.conf` lies outside the
  data directory, so `pg_basebackup` and
  `pg_rewind` copy the include line but not the primary's values.
  `deleted_with_all_its_data` removes
  `/etc/postgresql/pg_auto_failover_node_settings.conf`.
  `absent` keeps `/etc/postgresql/pg_auto_failover_node_settings.conf`.
  - While the node's PostgreSQL does not accept connections, the module
    writes `/etc/postgresql/pg_auto_failover_node_settings.conf` before
    waiting for PostgreSQL, after checking for refused settings as listed
    under **Refused before writing** below.
    pg_autoctl tries to start PostgreSQL every few seconds out of
    maintenance. On the test guests, PostgreSQL started five seconds after
    the module wrote a corrected settings file.
    So a corrected setting can repair a replica during the
    `pg_auto_failover_node` task that writes the corrected value.
    In maintenance, PostgreSQL starts with
    `/etc/postgresql/pg_auto_failover_node_settings.conf` when `present`
    ends the maintenance.
  - With PostgreSQL running, the module applies PostgreSQL settings after
    the failover settings and before applying `in_maintenance`:

    1. Write `/etc/postgresql/pg_auto_failover_node_settings.conf`, keeping
       the previous contents of the module's settings file.
    2. Check `/etc/postgresql/pg_auto_failover_node_settings.conf` with
       `postgres -D DATA -C port` as `postgres`.
       On PostgreSQL 15 and 18, the command reads every included file and
       `postgresql.auto.conf`, even while the server runs.
       The command fails on an invalid value, an unknown name, or a syntax
       error. `pg_file_settings` cannot distinguish a valid value waiting
       for a restart from an invalid value.
    3. Read `pg_file_settings` in `seqno` order.
       The last assignment of each requested setting must come from
       `/etc/postgresql/pg_auto_failover_node_settings.conf`.
       Otherwise, a later assignment overrides the module's setting, for
       example through `ALTER SYSTEM` in `postgresql.auto.conf`.
       The task fails, naming the overriding file and line.

    If validation or the override check refuses the settings, the module
    restores the previous contents of
    `/etc/postgresql/pg_auto_failover_node_settings.conf`, then fails as
    invalid inputs.
    The previous PostgreSQL settings stay in effect.
    After successful validation and the override check, the module runs
    `pg_ctl reload`.

    The module then reads `pending_restart` in `pg_settings` to decide
    whether to restart PostgreSQL. The decision covers settings listed in
    `/etc/postgresql/pg_auto_failover_node_settings.conf` before or after
    the task, including removed settings.
    So an unchanged settings file can still require a restart for settings
    left pending by an earlier `never` run.
    After a restart, each requested setting must come from
    `/etc/postgresql/pg_auto_failover_node_settings.conf`, as reported by
    `pg_settings.sourcefile`.
  - **Restart:** for a replica with `on_a_replica` or
    `on_a_replica_or_a_node_alone`, the module waits until the formation has
    settled with the replica healthy.
    The module then performs the replica's restart in the following order:

    1. Run `pg_autoctl enable maintenance`.
    2. Wait until the monitor reports the replica in maintenance.
    3. Run `systemctl restart pgautofailover`.
    4. Run `pg_autoctl disable maintenance`.
    5. Wait for PostgreSQL and a settled formation.

    In maintenance, a restart leaves PostgreSQL stopped, and disabling
    maintenance starts PostgreSQL.
    The monitor refuses maintenance on a replica while the primary is
    `wait_primary`.
    A replica that stays in maintenance only restarts.

    A node alone with `on_a_replica_or_a_node_alone` restarts in place.
    A node created by the task restarts in place with any value of
    `restart_postgresql_for_new_settings`.
    The primary of a formation with replicas never restarts in place.
    On the primary with replicas, `on_a_replica` and
    `on_a_replica_or_a_node_alone` are a missing prerequisite.
    On a node alone, `on_a_replica` is a missing prerequisite.

    Before reporting the missing prerequisite, the module restores
    `/etc/postgresql/pg_auto_failover_node_settings.conf` to the previous
    contents and reloads PostgreSQL.
    With `never`, the task finishes with the settings still pending a
    PostgreSQL restart.
    The reason says how to apply the pending settings.
  - **Refused before writing:** `listen_addresses` and `port` (the
    module's inputs); `primary_conninfo`, `primary_slot_name`, and the
    `recovery_target` settings (pg_autoctl's standby file); and
    `synchronous_standby_names` (pg_autoctl's `ALTER SYSTEM` on the
    primary); `hot_standby`, `wal_log_hints`, and `ssl` other than `on`;
    `wal_level` other than `replica` or `logical`; `password_encryption`
    other than `scram-sha-256`; `synchronous_commit` `off` or `local`;
    `max_wal_senders` and `max_replication_slots` below the formation's
    number of nodes. On a replica, `max_connections`,
    `max_worker_processes`, `max_wal_senders`, `max_prepared_transactions`,
    and `max_locks_per_transaction` below the primary's values, which
    `pg_controldata` on the replica reports; on the primary of a formation
    with replicas, below its own. Everything else is accepted,
    `shared_preload_libraries` included.
- **Outputs:** `created`; `current_state` as the monitor reports it,
  such as `single`, `primary`, `secondary`, or `maintenance`; and
  `is_primary`, by the node's assigned state, as the monitor's
  `primary_node_name`. With `postgresql_settings`:
  `changed_postgresql_settings`, `restarted_postgresql`, and
  `postgresql_settings_waiting_for_a_restart`; a dryrun gives only the
  first.
- **Dryrun:** `would_change` with what a run would do; `not_checked` while
  the programs are missing or the monitor does not answer. For the
  settings, the module's dryrun compares
  `/etc/postgresql/pg_auto_failover_node_settings.conf` with the requested
  settings and reads `pg_settings.context`. `postmaster` means a restart.
  The module's dryrun reports whether a run would restart PostgreSQL,
  leave the settings pending, or refuse the restart.
  The dryrun uses `postgres -C` to compare values, but does not validate
  the proposed configuration.

A database, a login, and `pg_hba.conf` lines for the applications are
ordinary tasks: `postgresql_database` and `postgresql_role` on the primary,
and `line` tasks in the node's `pg_hba.conf`, which a joining node copies
from the primary. pg_autoctl adds only its own logins to `pg_hba.conf`.

### A planned switchover

```yaml
- pg_auto_failover_primary:
    monitor_data_directory: /var/lib/postgresql/18/monitor
    formation: default
    node_name: db02
```

`pg_auto_failover_primary` runs on the monitor's server and makes
`node_name` the formation's primary. When the monitor has assigned `node_name` a
primary's state already, the task is unchanged.
Otherwise the module runs `pg_autoctl perform promotion
--pgdata <monitor_data_directory>`, which waits until the switchover is
done (about 8 seconds on the test guests), and the module reads the state
again. With synchronous replication the switchover loses no write.
A node that is not
in the formation, or in maintenance, is a missing prerequisite that an
earlier task may resolve; one that is not a healthy replica, such as while
it catches up, fails the task and is tried again. Each reason names the
node's state. Output:
`primary_node_name`. Dryrun: `would_change` for a healthy replica in a
settled formation. A missing node, a node in maintenance, an unhealthy
replica, or an unsettled formation is `not_checked`.

## pgBackRest

[pgBackRest](https://pgbackrest.org/) backs up PostgreSQL and archives its
write-ahead log to a repository, such as an S3-compatible bucket, and
restores to any moment the archive covers. Debian and Ubuntu package
pgBackRest as `pgbackrest`: 2.45 on Debian 12, 2.50 on Ubuntu 24.04, 2.55 on
Debian 13, and 2.58 on Ubuntu 26.04. `pgbackrest_stanza` and
`pgbackrest_restore` run `pgbackrest` as the account `postgres` with the
task's `config_file`.

Playbook authors configure pgBackRest, enable archiving, and schedule
backups with ordinary node tasks, as the modules' pages show:

- the pgBackRest configuration file, a `file` task with a template. The
  configuration names the repository, the repository's keys from the vault,
  and the stanza's `pg1-path`. The pgBackRest
  configuration file is owned by `root:postgres` with mode `0640`;
- archiving, `line` tasks in `postgresql.conf`: `archive_mode = on`,
  `archive_command = 'pgbackrest --config ... --stanza=... archive-push %p'`,
  and `archive_timeout`, in a node task block that restarts PostgreSQL's
  service, because `archive_mode` takes effect only at a restart;
- the backups, `systemd_scheduled_job` tasks such as a weekly full and a
  daily differential backup. On a server with replicas the job runs on
  every database server and does nothing where `pg_is_in_recovery()` is
  true, so the backups follow the primary after a failover.

### A stanza

```yaml
- pgbackrest_stanza:
    stanza: main
    config_file: /etc/pgbackrest/pgbackrest.conf
```

| Input | Meaning |
|---|---|
| `stanza` | Required. The stanza's name, as in the configuration file. |
| `config_file` | Required. The configuration file pgBackRest reads. |

- **The module reads the stanza** with `pgbackrest info --output=json`,
  which exits 0 also when nothing exists: each repository's status says
  whether the stanza's path is missing (1), the stanza has no backup yet
  (2), the stanza's backup information is missing (3), or the stanza is fine
  (0). The cluster is the stanza's `pg1-path` in the configuration file;
  its `PG_VERSION` gives the version, and `standby.signal` or
  `recovery.signal` says that it is in
  recovery.
- **A missing stanza is created** with `stanza-create`, then `check`
  proves that archiving reaches the repository. On a replica (PostgreSQL
  in recovery) a missing stanza is a missing prerequisite that says to
  create it on the primary: `stanza-create` needs the primary.
- **A stanza of an older PostgreSQL version** is upgraded with
  `stanza-upgrade` on the primary, after a major upgrade, then checked.
- **A stanza that stopped partway**, with the archive's information but
  no backup information (status 3, and pgBackRest's error 55 from
  `stanza-create`), fails the task with a reason that says how to clear
  the incomplete stanza. An attacker who deleted backup information leaves
  archive information without backup information. The reason names
  interrupted stanza creation and deletion of backup information as
  possible causes.
- An unchanged run does not run `check`, which would archive a segment of
  the write-ahead log on every run.
- **Outputs:** `created`. **Dryrun:** `would_change` for a stanza to create
  or upgrade.

### A restore

```yaml
- pgbackrest_restore:
    stanza: main
    config_file: /etc/pgbackrest/pgbackrest.conf
    data_directory: /var/lib/postgresql/18/main
    postgresql_major_version: 18
    recover_to: "2026-11-03 13:01:00+00"
```

| Input | Meaning |
|---|---|
| `stanza`, `config_file` | Required, as for the stanza. |
| `data_directory` | Required. Where the restored cluster goes: the same path as on the server that was backed up, because pg_autoctl writes absolute paths, such as its TLS files, into the configuration that the restore brings back. The data directory of a cluster of Debian's own tools works too. |
| `postgresql_major_version` | Required. The version whose programs restore and start the cluster. |
| `recover_to` | Required. `latest`, to replay the whole archive, or a time with its time zone, such as `2026-11-03 13:01:00+00`, to stop there (`--type=time --target-action=promote`). The module reads `YYYY-MM-DD HH:MM`, optional seconds, and `+HH`, `-HH`, `+HH:MM`, or `Z`, to choose the backup that a dryrun names. |
| `read_repository_as_of` | A time with its time zone: pgBackRest reads the repository as it stood then (`--repo-target-time`), so files that were deleted or overwritten later, as by an attacker, do not matter in a bucket that keeps versions. Needs pgBackRest 2.54 or newer: Debian 13 and Ubuntu 26.04. Needs `repository`. |
| `repository` | The repository's number in the configuration file, such as `1`. Required with `read_repository_as_of`. |

- **The restore goes only into an empty or missing data directory.** A
  directory that holds the module's restore, a cluster whose
  `postgresql.auto.conf` holds pgBackRest's `restore_command` for the
  stanza, is compared with the task's target. A restored cluster without
  `recovery.signal` is the module's completed restore. A restored cluster
  that still has `recovery.signal`, as after a run that ended during the
  replay, needs recovery to finish. The module starts the restored cluster
  if the cluster is not running, then waits for recovery to finish.

  A directory that holds anything else, including a restore that stopped
  partway (pgBackRest's `backup.manifest`), fails the task and names what
  the module found. The module never uses `--delta`, which writes over an
  existing cluster.
- **MMX records each restore's target** right after `pgbackrest restore`
  succeeded: the stanza, `recover_to`, and `read_repository_as_of`, with
  the data directory's path, in
  `/var/lib/mmx/pgbackrest_restore_targets/<SHA-256 of the data
  directory's path>.json` (owner `root`, mode `0600`), outside the data
  directory. A later run that finds the module's restore compares the
  task's three values with the record, as text. Matching `stanza`,
  `recover_to`, and `read_repository_as_of` values mean the restore target
  is unchanged. Another target fails as invalid inputs, in a dryrun as in a
  run, before anything starts. The failure reason names the recorded restore
  target and the task's requested target. The reason says to stop the cluster
  and empty the data directory to restore again.

  A restore without a record, such as a restore by hand, follows the
  `recovery.signal` rules above. A completed restore is unchanged. For a
  restore still in recovery, the module starts the cluster if needed and
  waits for recovery to finish. The reason says the module cannot compare
  the unrecorded restore target with the task's requested target.
  A restore into an empty directory writes a new record over an old record.
- **`--archive-mode=off`.** The restored cluster has the same system ID as
  the cluster that was backed up. Without `--archive-mode=off`, the restored
  cluster's `archive_command` would push a new timeline into the production
  stanza.
- **The module starts the restored cluster** and waits until
  `pg_is_in_recovery()` is false, at most six hours. `pg_ctl start` returns
  while the replay still runs, so queries during replay see an older
  database. The result reports the restore and replay durations.
  A cluster outside Debian's PostgreSQL management tools starts with
  `pg_ctl start --options "-c listen_addresses=''"`, on the cluster's local
  socket only. The wait asks through the socket that `postmaster.pid` names.

  The restored configuration names the addresses of the server that was
  backed up.
  pg_autoctl writes the node's `listen_address` into the data directory's
  `postgresql-auto-failover.conf`. On a server without the restored address,
  PostgreSQL failed to start. A setting on the command line wins over every
  configuration file, `postgresql.auto.conf` included. A restore
  drill checks the database with `psql` on the socket. A following
  `pg_auto_failover_node` task registers the cluster as a formation's
  primary at the new server's own address: `pg_autoctl create postgres`
  writes the task's `listen_address` into `postgresql-auto-failover.conf`
  over the restored one, and its keeper starts PostgreSQL with `-h` and
  that address, so the cluster then listens there alone (`ss` on the test
  guests).
- **A cluster managed by Debian's PostgreSQL tools** uses the
  `postgresql-<version>` package's layout: configuration in
  `/etc/postgresql/<version>/<cluster>/`, data in
  `/var/lib/postgresql/<version>/<cluster>`. The module identifies a
  Debian-managed cluster by a `pg_lsclusters --json` entry with the task's
  data directory. `pg_lsclusters --json` lists the cluster even while the
  data directory is empty or missing. The module starts the Debian-managed
  cluster with `systemctl start postgresql@<version>-<cluster>.service`.

  The module starts every cluster outside Debian's PostgreSQL management
  tools with `pg_ctl start --pgdata`.
  `pg_ctl start --pgdata` fails for Debian's layout because the Debian-managed
  cluster's `postgresql.conf` is outside the data directory.

  The empty-directory rule stays. The playbook stops the Debian-managed
  cluster and empties the cluster's data directory in earlier node tasks.
  A marker file guards the stop and empty-directory tasks.
  A task after the restore writes the marker file, so a later run cannot
  delete the restored database. The
  [`pgbackrest_restore` page](../../../official_extra_modules/pgbackrest_restore/README.md#set-up)
  shows the guarded tasks as a one-time playbook.
  In a dryrun, an earlier node task that changes the data directory makes
  the restore `not_checked`.

  pgBackRest writes `restore_command`, `archive_mode = off`, and
  `recovery.signal` into the data directory of a Debian-managed cluster too.
  The module recognizes completed restores of Debian-managed and
  pg_autoctl-managed clusters by the stanza's `restore_command` and the
  absence of `recovery.signal`. The archiving settings under
  `/etc/postgresql` stay the receiving server's, and `postgresql.auto.conf`,
  read last, wins.

  The service starts the cluster with listening addresses from
  `/etc/postgresql/<version>/<cluster>/postgresql.conf`.
  Debian's `postgresql.conf` includes nothing from the data directory.
  Debian's data directory holds no `postgresql.conf`, so the restored
  `postgresql-auto-failover.conf` is not read, even for a backup of a
  pg_auto_failover node. Only a `listen_addresses` that `ALTER SYSTEM` wrote
  into the backed-up cluster's `postgresql.auto.conf` would override the
  addresses when the service starts the Debian-managed cluster.
  pg_autoctl writes no `listen_addresses` into the backed-up cluster's
  `postgresql.auto.conf`.
- **pgBackRest older than 2.54 with `read_repository_as_of`** is a missing
  prerequisite that names the version.
- **Outputs:** `restored`. **Dryrun:** `would_change` with the backup set
  and target that a run would use; it restores nothing. With
  `read_repository_as_of` it cannot read the repository as it stood then,
  and names that time instead of a backup.

The tests use a local S3-compatible server on a test guest, with
versioning on, in place of a cloud bucket, where the distribution
packages one (Debian 13 and Ubuntu 26.04), and a directory repository on
Debian 12 and Ubuntu 24.04.

## Growing a filesystem to fill its device

`grow_filesystem_to_fill_device` grows an ext4 or XFS filesystem on a node
when its block device is larger than the filesystem.
`grow_filesystem_to_fill_device` never shrinks, moves, checks, repairs, or
creates a filesystem.

```yaml
- grow_filesystem_to_fill_device:
    device: /dev/data/postgresql
    filesystem_type: ext4
```

| Input | Meaning |
|---|---|
| `device` | Required. The absolute path of the block device holding the filesystem, including a device link. |
| `filesystem_type` | Required. The expected filesystem: `ext4` or `xfs`. |

The module has no `state` input and no outputs. The filesystem must be
mounted for writing. A missing device, a different filesystem signature,
an unmounted filesystem, or a read-only mount is a `missing_prerequisite`:
an earlier node task may create or mount the filesystem, such as with
`create_filesystem_on_empty_device` and `fstab_mount`.

The module probes signatures with `wipefs --no-act --json`.
`grow_filesystem_to_fill_device` reads the kernel device numbers with
`lsblk --json --nodeps -o PATH,MAJ:MIN`, then matches every mount by the
device numbers in `findmnt --json --list --output
TARGET,FSTYPE,OPTIONS,MAJ:MIN`. `grow_filesystem_to_fill_device` chooses the
first writable mount of the expected type. Matching device numbers from
`lsblk` and `findmnt` avoids relying on `findmnt --source` for a device link
and includes multiple mounts. Device links and multiple mounts both have
stand-in tests. Real node behavior for device links and multiple mounts
has not been measured.

`blockdev --getsize64` reads the device bytes. For ext4, `dumpe2fs -h`
gives `Block count:` and `Block size:`. For XFS, `xfs_info` at the mount
point gives `data = bsize=<B> blocks=<N>`. A filesystem grows when the
device bytes divided by the block size, rounded down, exceed its block
count. A dryrun reads the device size in bytes, filesystem block size, and
filesystem block count.

The dryrun reports `would grow the ext4
filesystem on <device> from <old> to <new>` (or `xfs`), changing nothing.
Sizes use the largest binary unit that gives a whole number, or bytes.

A run calls `resize2fs <device>` or `xfs_growfs <mount point>`, then reads
the filesystem size again. The node task result's `reason` reports the
actual growth. After a growth command fails, the module also reads the
filesystem size again. If growth occurred, or the size read fails,
`try_again: False` prevents another try from hiding the failure. A command
that exits with zero but grows nothing fails with its words and may be tried
again, as for another command that failed without growth.
No progress text is used to decide the filesystem's size.

## Decisions

The decisions below are settled. Revisit a decision only with a concrete
workflow that the decision fails to support.

| Decision | Reason |
|---|---|
| A script reports a missing program with the status `missing_prerequisite`, and MMX turns it into a failure without another try in a run and into `not_checked` in a dryrun. | A playbook that installs a tool and then uses the tool's module must pass its first dryrun, as it does with a core module. |
| An input marked `private_value: true` does not hide the task's result; MMX shows `[redacted]` wherever a text of the result holds the value. | A password must not turn a failure's reason into "details suppressed". Core modules already replace private input values with `[redacted]` in result text without hiding the result. |
| The modules keep the names of the core modules: `name` for the thing, `state`, `password`, `login_user`, and `<things>_not_listed`. | A module can move between kinds without a change to anyone's playbooks, and playbook authors use consistent input names for every module kind. |
| `incus_instance.instance_type` and `profiles` are required for `running` and `stopped`. | Type and profile order depend on the instance; [required settings](modules.md#a-setting-with-no-safe-guess-is-required) make these choices visible. |
| `incus_instance.image_when_created` and `lvm_logical_volume` size `all_free_space_when_created` name when they apply. | Changing `image_when_created` or `all_free_space_when_created` never recreates an instance or resizes an existing volume; the [creation-only naming rule](modules.md#a-setting-used-only-at-creation-says-so-in-its-name) prevents expectations of recreation or resizing. |
| A listed Incus device replaces its whole definition, also with `devices_not_listed: keep`. | Incus validates a complete device definition. The list policy controls unnamed devices, not individual options. |
| `incus_instance.profiles` is the whole list, with no `profiles_not_listed`. | The order of an instance's profiles decides which value wins, so a list that keeps unnamed profiles has no defined order. |
| `incus_instance` and `incus_profile` write the configuration through `incus config edit` and `incus profile edit`. | One command takes the whole configuration from standard input, so a configuration value never reaches a command line. |
| `incus_instance` never creates an existing instance again from another image, and never restarts one. | Recreating and restarting an instance both destroy what runs inside the instance. |
| `incus_instance` and `libvirt_domain` wait a bounded time for a shutdown, with the same two inputs: `wait_for_shutdown_at_most` and `power_off_when_shutdown_takes_longer`. | A guest can ignore a request to shut down for ever: on the test nodes `incus stop` was still waiting after two minutes. A task must end, and cutting the power is the project's choice, not the module's. |
| `incus_instance` refuses `stopped` for an ephemeral instance. | Incus deletes an ephemeral instance when it stops, and a state that destroys data is always spelled out. |
| `incus_instance` and `incus_profile` write every name with the prefix `local:`. | Root's own client settings can name another default server; a task must manage the Incus of its node. |
| `incus_instance` and `incus_profile` name the Incus project in every command, `default` when the task names none. | `incus project switch` stores another default project in root's client settings, and on the test nodes `incus start` then looked for the instance in that project. |
| `incus_instance` and `incus_profile` refuse a configuration key and a device option with an empty value. | Incus stores no empty value, so such a task would report a change on every run. |
| `incus_instance` and `incus_profile` disable another try (`try_again: False`) for the following failures after a node change. For `incus_instance`: any failure after `incus init` created the instance, or a failed start that leaves an existing instance running. For `incus_profile`: Incus saved the profile but then failed to apply the profile to an instance using the profile. For `incus_instance`: a shutdown exceeded the configured wait. | MMX's next attempt reads the node again. The next attempt's module result would report `created: false` for an instance that the previous attempt created. The next attempt could instead find the saved profile and running instance already matching the task's request and report success, so Incus's failure message would be lost. A second wait for a shutdown would only repeat the first. |
| A document that Incus refuses (an instance's configuration, a profile) is invalid inputs, and so is a new profile that Incus refuses, which the module removes again. | Another attempt sends the rejected instance configuration or profile document again, so Incus rejects the document again. Classifying the rejection as invalid inputs prevents another attempt; without classifying the rejection as invalid inputs, a refused profile was created and removed three times. |
| A local image that the node's Incus does not have is a missing prerequisite. | An earlier node task may import the image, so a playbook that imports an image and then creates an instance from it passes its first dryrun. |
| `libvirt_domain.domain_xml_file` is optional for an existing domain, and required to create one. | Starting or stopping needs no new definition. [Omission never deletes](modules.md#leaving-a-setting-out-never-deletes) preserves the stored definition. A missing domain without XML is a missing prerequisite: an earlier node task may create it, so dryrun reports `not_checked` and a run fails once. |
| Starlark durations use the `parse_duration` primitive, backed by Go's duration parser. | Scripts use core-style duration syntax without separate parsers. |
| `incus_instance` and `libvirt_domain` use `state: stopped` and `wait_for_shutdown_at_most: 2m`. | One state word and core-style durations reduce the vocabulary a reader must learn. Whole seconds match Incus's timeout and libvirt's check interval. |
| `libvirt_domain` takes the domain's XML from a file on the node that a `file` task writes. | The XML is long and usually a template; the `file` task shows the XML file's difference and tracks the file, as the Compose file of `docker_compose_project`. |
| `libvirt_domain` keeps the SHA-256 of the last defined file in the domain's libvirt metadata. | libvirt rewrites the XML it stores, so the stored text cannot be compared with the file. The hash record lives and dies with the domain. |
| `libvirt_domain` adds the domain's UUID to the XML it defines. | libvirt refuses to define an existing name again without the UUID; requiring the UUID in every file would put a value that libvirt chose into the project's template. |
| `absent` on `libvirt_domain` keeps the UEFI variables and the TPM state. | A plain `virsh undefine` deleted the state of an emulated TPM on the test nodes without a word, and that state can hold a disk's key. `absent` removes a definition and no data. |
| `libvirt_domain` refuses a name that is a number or has the form of a UUID. | `virsh` looks a text up as the ID of a running domain, then as the UUID of a domain, and only then as a name. A task with such a name could change another domain. |
| `absent` on `libvirt_domain` asks libvirt whether a saved memory image or snapshots exist, and fails before requesting removal of the domain definition when a saved memory image or snapshots exist. | libvirt refuses to remove a definition with a saved memory image or snapshots. A dryrun runs no changing command, so without checking for a saved memory image and snapshots, the dryrun would claim a removal that a run does not do. |
| `absent` on `libvirt_domain` refuses a domain with checkpoints. | On the test nodes libvirt removed the definition of such a domain and, without a word, its record of each checkpoint, from which an incremental backup continues. `absent` removes a definition and nothing else. |
| A domain that still runs, a saved memory image, snapshots, and checkpoints are missing prerequisites of `absent` on `libvirt_domain`. | A playbook that shuts a domain off and then removes its definition is right, and its dryrun must not fail because the domain still runs: the dryrun is not checked, and a run fails once. |
| A wait for a shutdown that ended, and a failure after `libvirt_domain` created the domain, are not tried again. | After a shutdown wait expires, another attempt would send a second shutdown request and wait for the full configured shutdown timeout again. After a failure following domain creation, another attempt would find the new domain and report `created: false`. |
| `running` resumes a paused domain and a frozen Incus instance; `stopped` fails for either. | A task requesting `running` requires the machine to run, and a paused machine does not run. A request to shut down cannot reach a paused machine. |
| libvirt networks and storage pools have no module. | A domain works with a bridge of the node and with image files named by path; [`official_extra_modules/libvirt_domain/README.md`](../../../official_extra_modules/libvirt_domain/README.md#libvirts-default-network) shows the guarded `shell` task that starts libvirt's `default` network. |
| `grow_partition_into_free_space_after_it` uses `growpart`, rather than rewriting the partition table itself. | Cloud images use growpart at boot to grow partitions; it maintains the backup header of the partition table and provides a dry run. |
| The partition module is `create_partitions_on_empty_disk`, writes GPT only, and never changes a disk that holds anything. | `create_partitions_on_empty_disk` mirrors `create_filesystem_on_empty_device`: the partition module's only purpose is that a run can never destroy data. A partition table is written once in a disk's life. |
| Partitions are named, and later tasks use `/dev/disk/by-partlabel/<name>`. | The kernel's names differ between disk types (`sdb1`, `nvme0n1p1`) and can change between boots. |
| `grow_filesystem_to_fill_device` grows only a filesystem mounted for writing. | XFS grows only while mounted. An unmounted ext4 filesystem may demand e2fsck first; `grow_filesystem_to_fill_device` never checks or repairs a filesystem. A read-only mount cannot grow. |
| `lvm_logical_volume` grows a volume and never shrinks one. | Growing is why teams use LVM and loses nothing; shrinking destroys data when the filesystem is not shrunk first. |
| `grow_filesystem_with_volume` is required for `present`. | Whether to grow the filesystem with the logical volume depends on the node task's intended result, so no default is a safe guess. The task states whether LVM must grow the filesystem with the volume, following [required settings](modules.md#a-setting-with-no-safe-guess-is-required). |
| `lvm_logical_volume` probes a volume with `wipefs --no-act` before it grows the volume together with its filesystem. | LVM refuses a volume without a filesystem that it can grow, and no reading command of LVM says so beforehand. Without the probe a dryrun promised a growth that the run refused. The probe recognizes ext2, ext3, ext4, and xfs as filesystems supported for growth: LVM 2.03.11 and 2.03.43 grew each of them with its volume. |
| A logical volume that holds nothing grows alone, also with `grow_filesystem_with_volume: true`. | Growing an empty volume loses nothing, and no filesystem can be left small. Refusing to grow an empty volume blocked a real sequence: the first run stopped after `lvcreate` and before the node task that creates the filesystem, then the requested volume size increased before the next run. On the next run, the logical volume task failed because the volume had no filesystem. The filesystem's task came later in the node-task list, so the filesystem task never ran. |
| `lvm_logical_volume` deletes no volume that other volumes depend on. | On LVM 2.03.11 and 2.03.43, `lvremove --yes` deleted two snapshots together with their volume, and two thin volumes together with their pool: `--yes` also answers LVM's question about deleting the dependent snapshots or thin volumes. A task names one volume. A snapshot of a thin volume stays when its origin is deleted, so it is not in the way. |
| `create_partitions_on_empty_disk` asks `lsblk` which devices are on the disk and refuses a disk in use. | A disk that device-mapper holds can have no signature. `sfdisk` (util-linux 2.37.2 and 2.42.4) refused such a disk when it was about to write, and `sfdisk --no-act` did not: without the module's own look, a dryrun promised a table that the run refused. `lvm_volume_group` has no such check: LVM refuses such a device before it writes. |
| A state that an earlier node task of the playbook containing the storage task may change is a missing prerequisite in the storage modules: too little free space in a volume group, a device of another volume group, a volume in use or with depending volumes that the task wants deleted, a device that still holds data of a logical volume, a volume group with logical volumes that the task wants absent, and a dataset with child datasets or snapshots that the task wants deleted. | The modules' own pages show such playbooks: unmount and then delete, delete the logical volumes and then their volume group. In a dryrun the earlier task has changed nothing, and the dryrun must not fail for the blocking state. A state that no node task changes, such as a disk that holds data or a volume that would have to shrink, stays an invalid input. |
| `zfs_dataset` refuses a property's second name, such as `compress`. | ZFS 2.1.5 and 2.2.2 took the second name in every command, and `zfs get` printed the long one. The module compares what it asked with what `zfs get` prints, so it keeps no list of such names. With a property's two names both allowed, two tasks could manage one property. |
| The storage modules fail without another try (`try_again: False`) when the task changed the node and then failed: a partition link that is missing after the write and a read that failed after it, a volume that grew while its filesystem did not, a device that kept LVM's label after it left its volume group, a dataset that ZFS created before it failed, a value that ZFS stores in another form, and a volume or a dataset that was created before a command that reads failed. | The next try would find the changed node and report success, or report `created: false` for what the task created, and nobody would read what failed. The result stays an ordinary failure with its change and its outputs. |
| `create_partitions_on_empty_disk` computes every sector itself and does not rely on a check by `sfdisk`. | On the test nodes `sfdisk` replaced a partition table, wrote over a filesystem, and made a partition smaller than asked, each time without an error, and its older and newer versions end the last partition at different sectors. |
| `lvm_volume_group` removes LVM's label from a device that leaves a volume group. | Otherwise the device stays "an LVM physical volume" for every later task, although it holds no data. |
| `create_partitions_on_empty_disk` checks the partitions' links also on a disk that already has the task's table, and compares each partition's first sector. | A run that wrote the table and found no link is followed by a run that finds the table. Without the partition-link check, the following run would report success while the next node task cannot find its partition. |
| The LVM modules check themselves what LVM would refuse: a device that still holds data, too little free space, and a volume in use. | No reading command of LVM says beforehand what a changing command will refuse. With the module's own check a dryrun fails as a run does; LVM's refusal stays the second guard. |
| `zfs_dataset` lists a dataset's child datasets and snapshots itself before it destroys the dataset. | A dryrun must fail as a run does, and the task must name what is in the way. The module never passes `-r`. |
| `zfs_dataset` compares a value as ZFS stores it, and fails when the stored form differs from the written one. | A list of ZFS's own aliases differs between ZFS versions; a task that reports a change on every run hides real changes. |
| ZFS pools stay a guarded `shell` task. | Creating a pool takes whole disks and happens once for each server. |
| MMX tests a tool where its distribution or its own project serves ready packages. A setup that needs more, such as a build of ZFS's kernel module on the node, is out of scope. | The modules drive a tool that is installed; getting a tool onto a node by unusual means is not what MMX is for. The ZFS suite therefore does not run its whole test on Debian 12, and each module's page lists where it is tested. |
| `lvm_volume_group.grow_physical_volumes_with_their_devices` is required for `present`. | Whether device growth should reach the volume group is the task's choice; [required settings](modules.md#a-setting-with-no-safe-guess-is-required) make it visible. |
| `rabbitmq_policy.priority` is required for `present`. | The priority decides which matching policy wins; the module cannot guess the intended precedence. |
| `rabbitmq_plugin` uses `disabled_with_plugins_that_depend_on_it`. | RabbitMQ also disables dependent plugins; the state names this effect before a task runs. |
| A RabbitMQ monitoring user has no `read` permission. | The `monitoring` tag grants statistics access. Queue `read` permission permits taking messages, which a statistics-only user must not do. |
| RabbitMQ has four modules: virtual hosts, users, policies, and plugins. Queues, exchanges, and bindings have none. | Virtual hosts, users, policies, and plugins are what an operator sets up (8, 7, 3, and 5 independent authors in the Ansible projects MMX was checked against); applications declare their own queues. |
| The RabbitMQ modules call the real `rabbitmqctl` program through `runuser`, not the `rabbitmqctl` command. | The command of Debian and Ubuntu puts its arguments through a shell. A module must hand a name or a policy to RabbitMQ exactly as the task wrote it, and must never let an argument run a command. |
| A name that a RabbitMQ module hands to RabbitMQ's programs must not be empty or start with a hyphen. | RabbitMQ's programs read an argument that starts with a hyphen as an option. `--` before the name would end the options, but `rabbitmqctl` 4 reads a password from standard input only when a password command has exactly the command and the user name, so one rule serves every name. |
| `rabbitmq_user` and `mongodb_user` compare a password by asking the server, not by computing its stored form. | A script has no functions for the salted hashes that RabbitMQ and MongoDB servers store. One refused login when a password changes is the cost. |
| `mongodb_user` runs `mongosh` with `HOME=/dev/null`. | `mongosh` keeps a log file of every run in the home directory, and the log can hold the text of the program it ran, with the task's passwords. No option of `mongosh` switches the log off for one run. |
| Only a user that can manage users becomes a MongoDB server's first user. | MongoDB accepts any first user and then refuses every further one without a login; a first user without a user-management role locks the server. |
| `mongodb_user` takes one `mongodb_connection_string` for the port, TLS, and replica set options. | A MongoDB connection string is MongoDB's own format for the port, TLS, and replica set options, so `mongodb_connection_string` replaces separate inputs for the connection options. |
| `mongodb_user` tries a task again while the replica set has no primary, and once when another server is the primary. | A playbook initiates a replica set in a `shell` task, and the member is the primary a few seconds later; a secondary stays a secondary. |
| The monitor's task lists the formation's nodes (`node_names`, `nodes_not_listed`), and drops a node from the monitor's server with `--force`. | A server that is gone can be dropped only from the monitor's side, and without `--force` pg_autoctl waits for the node and fails after a minute. With the database servers' names as the list, removing a server from that list removes it from the formation. |
| The monitor's task refuses to drop a healthy node that the monitor has made the primary. | Dropping a healthy primary would cause a failover, or remove the only copy of the database that runs. A primary that is gone is unhealthy and may be dropped. |
| `in_maintenance` on the primary is a missing prerequisite. | pg_autoctl then fails the primary over to a replica; a planned switchover is its own task, `pg_auto_failover_primary`, which an earlier task of the playbook runs. |
| `pg_auto_failover_node` keeps the node's PostgreSQL settings in a file of its own, restarts a replica through maintenance, and never restarts the primary of a formation with replicas in place. | A playbook's recipe of a `line`, a `file`, an `assert`, and a `systemd_service` restart caused two failures. Restarting the primary in place caused a failover. Separately, a run could not repair a setting that prevented PostgreSQL from starting, because the repair task waited for PostgreSQL to start before fixing the setting. |
| `pg_auto_failover_node` writes the module's settings before it waits for a PostgreSQL that does not run, and restarts for each setting of its file, before and after the task, that `pg_settings` marks `pending_restart`. | pg_autoctl started PostgreSQL five seconds after a fixed file arrived, so the task that writes the corrected settings repairs the node. A run with `never` or a removed setting can leave a setting waiting; converging on `pending_restart` applies it on the next run that allows a restart. Other settings do not count: after a promotion, pg_autoctl removes the replica's `recovery_target_timeline`, which PostgreSQL then marks pending until a restart that nothing needs. |
| Every pg_auto_failover instance uses TLS with a self-signed certificate and SCRAM passwords; there is no input for `trust`. | `trust` would let every address of the monitor's subnet register as a node or copy the database. |
| On every run, `pg_auto_failover_monitor` and `pg_auto_failover_node` replace `trust` in initdb's six lines of `pg_hba.conf` with `peer` on the local socket and `scram-sha-256` on the loopback addresses, leaving every other line unchanged. For the monitor only, the socket method is `peer map=pg_autoctl`, with the map's two lines in `pg_ident.conf`. | pg_autoctl creates every instance with `initdb --auth=trust`: on the test guests any account of the server logged in as `postgres` without a password, over `127.0.0.1` and on the socket. Debian's authentication methods prevent unrelated server accounts from logging in as `postgres` without a password, while preserving the `postgres` account's own logins. pg_autoctl logs in to the monitor's database on the socket as `autoctl_node`, which plain `peer` refused. pg_autoctl's own `hostssl ... pgautofailover_monitor <monitor>/32 trust` line stays: it is the monitor's health check, from the monitor's address only. |
| `listen_address` is required and accepts one IP address or `*`. To change an existing instance's listen address, the module stops the instance's service, sets the address with `pg_autoctl config set postgresql.listen_addresses`, starts the service, then checks the listening sockets with `ss`. A changed listen address on the primary of a formation with other nodes is a missing prerequisite. | pg_autoctl starts PostgreSQL on every address (`*`) unless told otherwise. A running pg_autoctl wrote its old value back over `config set` on the test guests, and each restart of a primary's service made the monitor fail over. An address, unlike a host name, can be compared with the listening sockets. |
| The formation's primary is the node whose assigned state lets it take writes (`single`, `wait_primary`, `primary`, `join_primary`, `apply_settings`), for the monitor's output, the node's `is_primary`, the switchover's unchanged check, and the refusal to drop a primary. | After a failover on the test guests, the old primary that had stopped still reported `primary`, and the monitor's output named the stopped former primary. pg_auto_failover's monitor picks the writable node by the assigned state; its `get_primary` also names a primary that is being demoted while no node may take writes. |
| `pgbackrest_restore` restores only into an empty or missing data directory, and never with `--delta`. | A restore writes over a cluster; the task must never destroy a database that runs. A fresh server or an emptied directory is what a recovery and a restore drill use. |
| `pgbackrest_restore` passes `--archive-mode=off`. | The restored cluster has the production cluster's system ID. On the test guests the restored cluster's `archive_command` would have written a new timeline into the production stanza. |
| `pgbackrest_restore` waits until the restored cluster has left recovery. | `pg_ctl start -w` returned while the replay ran, and a query then found a table missing that the restore brings back. |
| `pgbackrest_restore` starts a cluster that is not one of Debian's tools on its local socket only, with `pg_ctl start --options "-c listen_addresses=''"`, and waits through the socket. | The restore brings back the addresses of the server that was backed up: pg_autoctl writes a node's `listen_address` into the data directory's `postgresql-auto-failover.conf`, and on another server without that address PostgreSQL ended at its start on the test guests (could not bind the address, could not create any TCP/IP sockets). A setting on the command line wins over the files, and the socket serves the wait and a drill's checks. The task that takes the cluster over, such as `pg_auto_failover_node`, gives it the new server's address. |
| `pgbackrest_restore` records each restore's stanza, `recover_to`, and `read_repository_as_of` in `/var/lib/mmx/pgbackrest_restore_targets/`, and fails a later run with another target, without another try; a restore without a record stays as before, and the reason says that the module cannot compare. | Without the record, a rerun with another `recover_to` reported `unchanged` and kept the old restore: a wrong result that nobody sees. A record in the data directory would travel with the cluster, because `pg_basebackup` copies the data directory to every replica and pgBackRest backs it up; MMX's own records on a node are in `/var/lib/mmx/`, as for `podman_quadlet` and the command records. Another target never replaces the restore: the module never writes over a cluster. |
| `pgbackrest_restore` starts a cluster of Debian's own tools, which `pg_lsclusters` lists with the data directory, with its service `postgresql@<version>-<cluster>`; the playbook stops and empties the cluster in earlier, guarded node tasks. | Debian's PostgreSQL cluster layout keeps `postgresql.conf` in `/etc/postgresql`, so `pg_ctl start --pgdata` failed for a cluster in Debian's layout on the test guests. The service is how the server starts the cluster at boot. Emptying a data directory deletes a database, so it stays a visible task of the playbook, never a side effect of the module. |
| `pgbackrest_stanza` runs `check` only after creating or upgrading the stanza. | `check` switches and archives a segment of the write-ahead log, which every run of every database server would repeat. |
| The replication password is in `~postgres/.pgpass` only, not in pg_autoctl's `replication.password`. | On the test guests a replica copied and streamed with the password file's line alone; `pg_autoctl config set` takes the password on its command line and writes it into a file mode `0644`. |
| The primary sets the replication password without a comparing login, where the primary cannot know the role has the requested password. | pg_autoctl lets `pgautofailover_replicator` log in only from the other nodes' addresses, and the loopback addresses need not be open (`listen_address`), so the primary cannot log in to itself with the password. |
| `deleted_with_all_its_data` deletes a node dropped from the monitor's side. | `pg_autoctl drop node --destroy` asks the monitor first and failed for a node dropped by the monitor ("Failed to find the node to remove on the monitor"), also with `--force`, while the node's keeper had not run since the drop. |
| `pg_auto_failover_node` runs `pg_autoctl create postgres` a second time when the first invocation of `pg_autoctl create postgres` wrote the configuration and failed. | For a data directory that pg_autoctl had managed before, such as a restored one, the first run refused with "PostgreSQL is already running" seven times in eight on Ubuntu 24.04, with the cluster stopped, and the second run registered the cluster every time. |
| `pg_auto_failover_node` stops a running cluster before it registers it. | `pg_autoctl create postgres` refused a restored cluster that ran ("refusing to initialize a new cluster on-top of the current one"); the node's service starts the cluster again. |
