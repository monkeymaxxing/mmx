# `podman_quadlet`

`podman_quadlet` installs one **Quadlet file** on the node: a small file in
systemd's unit syntax that describes a container, or a volume, network, or
pod. A pod is a group of containers that share a network.
Podman runs containers without a daemon. At each systemd reload,
Podman's generator turns every Quadlet file into a systemd service. The
container then is an ordinary systemd service: systemd starts it at boot,
restarts it, and stops it.

```yaml
- podman_quadlet:
    name: shop-web.container
    content: |
      [Container]
      ContainerName=shop-web
      Image=registry.example.com/shop/web:2.4

      [Install]
      WantedBy=multi-user.target
```

So Podman needs less from MMX than Docker does. Three things do the work on
a node:

- [`podman_image`](podman_image.md) pulls one image from a registry (the
  server that images are pulled from) into the node's image store.
- `podman_quadlet` installs the Quadlet file and reloads systemd.
- The [`systemd_service`](systemd_service.md) task that you already know
  starts the generated service and restarts it when something it runs with
  has changed, with the same records as for any service.

`wait_for` has a form that waits until a Podman container is healthy; see
[Wait until a Podman container is healthy](#wait-until-a-podman-container-is-healthy).

The module runs the node's installed `podman` command.
Quadlet needs Podman 4.4 or newer. MMX does not drive
`podman compose`: a project that has a Compose file and wants Podman
translates each service into a Quadlet file. Docker's containers are written
with [`docker_compose_project`](docker_compose_project.md).

## Set up

This section is also the setup of [`podman_image`](podman_image.md).

Install Podman first with a package task. MMX has no module that installs
Podman. On a node without the `podman` program, a `podman_image` task and a
`podman_quadlet` task that installs a Quadlet file fail with a message that
says to install Podman, and a dryrun reports the task `not_checked`. A removal needs
`podman` only to delete a volume or a network: the removal of a `.network`
file, or of a `.volume` file with `state: volume_and_its_data_removed`, when
the file or MMX's record names that volume or network. Every other removal
works without `podman`. Debian 12 ships Podman 4.3, older than 4.4, so
`podman_quadlet` fails there and names both versions; `podman_image` works.
On Debian 12, write the container's unit file yourself with `systemd_unit`,
with `ExecStart=/usr/bin/podman run ...`, and start it with
`systemd_service`. The [`docker_image` page](docker_image.md#one-image-for-a-container-that-systemd-runs)
shows a `systemd_unit` task that runs `docker run`, followed by a
`systemd_service` task. Use `podman_image` to supply the Podman image.

A complete node task list for a Rocky Linux or AlmaLinux server,
which the container `shop-web` of
[Run a container from a Quadlet file](#run-a-container-from-a-quadlet-file)
follows:

```yaml
- dnf_package:
    packages:
      - {name: podman, version: newest_when_first_installed}
      - {name: skopeo, version: newest_when_first_installed}

- task_block:
    id: podman_registries
    changes:
      - file:
          path: /etc/containers/registries.conf.d/50-shop.conf
          permissions_mode: "0644"
          content: |
            [[registry]]
            location = "registry.example.com"
            blocked = false

- task_block:
    id: podman_image_cleanup
    changes:
      - systemd_unit:
          name: podman-image-cleanup.service
          content: |
            [Service]
            Type=oneshot
            ExecStart=/usr/bin/podman image prune --all --force --filter until=168h
      - systemd_unit:
          name: podman-image-cleanup.timer
          content: |
            [Timer]
            OnCalendar=Sun 04:00
            Persistent=true
            [Install]
            WantedBy=timers.target
    activate:
      - systemd_service:
          name: podman-image-cleanup.timer
          state: running
          start_at_boot: true
```

Podman has no daemon to configure or start. Its settings are drop-in files
under `/etc/containers/`, written with `file`. skopeo is there for the
dryrun of a `podman_image` task with `pull_image: always`. The cleanup timer
deletes, every Sunday at 04:00, the images in the system's store that no
container uses and that are older than seven days. Each ordinary account
that runs containers has its own image store. To clean it, add the same two
`systemd_unit` tasks and the `systemd_service` task once more, each with
`in_systemd_of_user_account: <account>`; that timer runs as the account and
cleans the account's store.

On the Red Hat family, Podman's package requires all the programs that
Podman's features need itself (measured with `dnf` on Rocky Linux 9 and
AlmaLinux 10); name `podman` and `skopeo` there.

**On Debian and Ubuntu, name more packages than `podman`.** There, Podman's
package requires only what a container of root needs. The programs for pods,
for the containers of an ordinary account, and for Podman's own networks are
"recommended" packages. apt installs recommended packages by default, but
many servers disable recommended package installation
(`APT::Install-Recommends "false"`).

Some server providers supply operating system images with recommended
packages turned off. The providers' Debian 12 and Debian 13 operating system
images carry
`/etc/apt/apt.conf.d/00InstallRecommends` containing
`APT::Install-Recommends "false"`. The providers' Ubuntu operating system
images keep apt's default (measured).

`apt_package` follows the node's own apt settings, and MMX does not force
recommended packages onto a node.
When apt disables recommended packages, installing `podman` omits the
programs for pods, ordinary accounts' containers, and Podman's networks.
A pod then does not start
(`exec: "catatonit": executable file not found`), and an ordinary account
cannot run a container (`exec: "newuidmap": executable file not found`).
Name the packages explicitly, so the listed packages are installed whether
`APT::Install-Recommends` is true or false:

```yaml
- when: {equals: ["{{ .node.package_manager }}", apt]}
  apt_package:
    packages:
      - {name: podman, version: newest_when_first_installed}
      - {name: skopeo, version: newest_when_first_installed}
      - {name: catatonit, version: newest_when_first_installed}
      - {name: uidmap, version: newest_when_first_installed}
      - {name: slirp4netns, version: newest_when_first_installed}
      - {name: dbus-user-session, version: newest_when_first_installed}
      - {name: netavark, version: newest_when_first_installed}
      - {name: aardvark-dns, version: newest_when_first_installed}
      - {name: nftables, version: newest_when_first_installed}
      - {name: passt, version: newest_when_first_installed}
```

| Package | What needs the package |
|---|---|
| `catatonit` | Pods. |
| `uidmap` | Containers of an ordinary account. |
| `slirp4netns`, `passt` | The network of an ordinary account's containers: `slirp4netns` with Podman 4 and older, `passt` with Podman 5. |
| `dbus-user-session` | Lets an ordinary account's Podman hand its containers to the account's own systemd. Without it, every Podman command of the account warns and falls back to another way. |
| `netavark`, `aardvark-dns` | Podman's networks for containers, and the names of the containers on one network. Without them, Podman 4 falls back to older network programs. |
| `nftables` | The firewall program that `netavark` runs on Debian 13. |
| `skopeo` | The dryrun of a `podman_image` task with `pull_image: always`, on every distribution. |

The example project [`examples/podman-host`](../../examples/podman-host/)
prepares a server with these tasks, and
[`examples/deploy-with-podman-quadlet`](../../examples/deploy-with-podman-quadlet/)
is the deployment of the next section as a whole project.

## Examples

### Run a container from a Quadlet file

```yaml
- selinux_file_context:
    path: /etc/shop-web
    also_apply_to_everything_inside: true
    selinux_type: container_file_t
- directory:
    path: /etc/shop-web
    permissions_mode: "0755"

- task_block:
    id: shop_web
    changes:
      - file:
          path: /etc/shop-web/configuration
          permissions_mode: "0644"
          content: |
            greeting = {{ .vars.shop_web_greeting }}
      - podman_image:
          image: registry.example.com/shop/web:2.4
          pull_image: when_missing
      - podman_quadlet:
          name: shop-web-data.volume
          content: |
            [Volume]
            VolumeName=shop-web-data
      - podman_quadlet:
          name: shop-web.container
          content: |
            [Unit]
            Description=Shop web application

            [Container]
            ContainerName=shop-web
            Image=registry.example.com/shop/web:2.4
            PublishPort=127.0.0.1:8080:8080
            Volume=shop-web-data.volume:/var/lib/shop-web
            Volume=/etc/shop-web:/etc/shop-web:ro
            HealthCmd=["/usr/local/bin/shop-web", "check-health"]

            [Service]
            Restart=always
            ExecReload=/usr/bin/podman exec shop-web /usr/local/bin/shop-web reload-configuration

            [Install]
            WantedBy=multi-user.target
    activate:
      - systemd_service:
          name: shop-web.service
          state: running
          refresh: reload
    verify:
      - wait_for:
          podman_container_is_healthy: shop-web
          wait_at_most: 2m
```

On the node, in order: MMX writes the application's configuration file.
MMX pulls the image if the node does not have the image.
MMX installs the two Quadlet files under `/etc/containers/systemd/` and
reloads systemd. Podman's generator makes `shop-web-data-volume.service`
and `shop-web.service`. The
`systemd_service` task starts `shop-web.service`, which creates the volume and
the container. `wait_for` waits until the container's own health check reports
healthy. A second run changes nothing.

The volume's file comes before the container's file, which names it; see
[the rules for the files](#rules-for-the-files). When the configuration file
changes later, the `systemd_service` task reloads the service with the
`ExecReload=` line, because the task says `refresh: reload`; see
[What restarts a Podman container](#what-restarts-a-podman-container).

### Containers of an ordinary account

Podman can run a container as an ordinary account, without root:

```yaml
- user:
    name: shop
    system: false
    home: /home/shop
    create_home_directory: true

- task_block:
    id: shop_web
    changes:
      - podman_image:
          image: registry.example.com/shop/web:2.4
          pull_image: when_missing
          in_image_store_of_user_account: shop
      - podman_quadlet:
          name: shop-web.container
          in_systemd_of_user_account: shop
          content: |
            [Container]
            ContainerName=shop-web
            Image=registry.example.com/shop/web:2.4
            PublishPort=8080:8080
            HealthCmd=["/usr/local/bin/shop-web", "check-health"]

            [Service]
            Restart=always

            [Install]
            WantedBy=default.target
    activate:
      - systemd_service:
          name: shop-web.service
          in_systemd_of_user_account: shop
          state: running
    verify:
      - wait_for:
          podman_container_is_healthy: shop-web
          podman_container_of_user_account: shop
          wait_at_most: 2m
```

The image goes into the image store of `shop`, which is separate from
root's. The Quadlet file is installed as
`/home/shop/.config/containers/systemd/shop-web.container`, owned by `shop`.
MMX reloads the systemd of the account `shop`, and the `systemd_service` task
starts `shop-web.service` there. The container's process runs as `shop`.
[The account's own systemd](#the-accounts-own-systemd) says what the account
needs.

### Wait until a Podman container is healthy

A container's **health check** is a command, declared with the container,
that Podman runs again and again to learn whether the application inside
works.

```yaml
verify:
  - wait_for:
      podman_container_is_healthy: shop-web
      wait_at_most: 2m
```

`podman_container_is_healthy: <container name>` is the Podman form of
[`wait_for`](wait_for.md). It waits until Podman reports that container
healthy by the container's own health check, which its Quadlet file declares
with `HealthCmd=`. The name is the `ContainerName=` of the Quadlet file.
Without that line Podman names the container `systemd-` and the file's base
name, such as `systemd-shop-web`; set `ContainerName=` so the name in the
playbook is the name on the page.

`podman_container_of_user_account: <account>` is allowed only with
`podman_container_is_healthy` and names the account whose container it is.
Without `podman_container_of_user_account`, MMX looks at the system's
(root's) containers.

- A container that declares no health check fails the task at once, because
  no answer would ever come.
- When the wait ends without health, the task fails with the container's
  status and with the exit code and the output of its last health check.
- Like every `wait_for`, it uses `wait_at_most`, changes nothing, checks once
  in a dryrun, and belongs in `verify`, as the
  [Docker forms](docker_compose_project.md#wait-until-a-container-is-healthy)
  do.

Podman calls a container unhealthy only after several failed checks in a
row (`HealthRetries=`, three by default). Until then a container that was
healthy before still counts as healthy.

### Remove a Podman container and its data

Remove the container's file before the volume's file:

```yaml
- podman_quadlet:
    name: shop-web.container
    state: file_and_service_removed_named_volumes_kept

- podman_quadlet:
    name: shop-web-data.volume
    state: volume_and_its_data_removed
```

The first MMX task stops `shop-web.service`, so Podman removes the container.
Then MMX deletes `shop-web.container` and reloads systemd.
The second task deletes the
named volume `shop-web-data` with the data in it, then its Quadlet file.
[Removal](#removal) says what each state keeps.

To stop a container for other work, such as a backup, and start it again,
use `systemd_service` with `state: stopped` and then `state: running`. The
named volume keeps its data.

## How it works

### Where the file goes

| Owner of the container | Directory on the node | File owner and mode |
|---|---|---|
| The system (root) | `/etc/containers/systemd/` | root, `0644` |
| The account in `in_systemd_of_user_account` | `<that account's home>/.config/containers/systemd/` | The account named by `in_systemd_of_user_account`, `0644` |

MMX creates the directory when it is missing.

### The service that Podman generates

The result's `value.generated_service` holds the name of the service that
Podman makes from the file. By kind of file:

| File | Generated service | What starting the generated service does | First Podman version |
|---|---|---|---|
| `shop-web.container` | `shop-web.service` | Runs the container. | 4.4 |
| `shop-web-data.volume` | `shop-web-data-volume.service` | Creates the named volume if it is missing. | 4.4 |
| `shop.network` | `shop-network.service` | Creates the network if it is missing. | 4.4 |
| `shop.kube` | `shop.service` | Runs the containers a Kubernetes YAML file describes. Install the Kubernetes YAML file with a `file` task in the `podman_quadlet` task's node task block. | 4.4 |
| `shop-web.image` | `shop-web-image.service` | Pulls the image. | 4.8 |
| `shop.pod` | `shop-pod.service` | Runs the pod (a group of containers that share a network). | 5.0 |
| `shop-web.build` | `shop-web-build.service` | Builds an image from a Containerfile. | 5.2 |

The versions are those of Podman's manual page of each release. A container
that names a volume, network, pod, image, or build file (as
`Volume=shop-web-data.volume` above) makes systemd start that file's service
first, so a playbook needs a `systemd_service` task only for the container,
pod, or kube service.

### What `podman_quadlet` does on each run

- **It checks a new file before it installs it.** MMX runs Podman's own
  generator in its check-only mode on the new file together with the Quadlet
  files that are already installed, as `systemd_unit` runs
  `systemd-analyze verify`. When the generator makes no service from the new
  file, the file is not installed, the installed file stays, and the task
  fails with the generator's message. The file is accepted when the
  generator prints the service it would make from it, never by the exit code
  alone.
- **The service must come from the new file itself.** The generator gets
  three directories and makes a service from the first file of each name
  that it can read: a copy of the new file, then a stand-in file of the same
  name that MMX writes, then the installed files. The stand-in carries a
  mark, so MMX sees which file the service comes from. A new file that the
  generator cannot read does not pass because a good file of the same name
  is installed.
- **For an account's file, that account runs the generator,** as its own
  systemd does. The copy and the stand-in are in the node task's private
  directory, which belongs to the account for the check
  ([execution design](../../docs/changing-mmx/design/execution.md#a-node-tasks-private-directory-lives-in-one-place-that-the-node-empties)).
  The generator waits without end on a file that is not a regular file,
  such as a named pipe, so MMX ends it after one minute and says so.
- **It refuses a file kind that the node's Podman does not know.** The task
  fails before it writes anything and names the installed version and the
  version that the kind needs; a dryrun reports `not_checked`. The same holds
  for Podman older than 4.4.
- **Unchanged** means that the installed file has the same bytes, owner, and
  mode.
- **It reloads systemd after a changed file** (the system's, or the
  account's), after it replaced the file atomically. MMX also checks
  systemd's own "reload needed" flag on an unchanged file, so a systemd
  reload that an interrupted run missed is done on the next run.
- **With `state: present`, it starts, restarts, and stops nothing.** That is the `systemd_service`
  task's work.
- **A dryrun** shows the file's difference and writes nothing. It reports
  the generator's check of a changed file as not checked. For an unchanged
  file that systemd has not read yet, it reports `would_change`, as
  `systemd_unit` does, because a run would reload systemd.

### Rules for the files

- **Install a file before the file that names it.** The container's file in
  [Run a container from a Quadlet file](#run-a-container-from-a-quadlet-file)
  names `shop-web-data.volume`. Podman's generator rejects a container file
  that names a Quadlet file it cannot find, and MMX installs no file that the
  generator rejects, so the volume's task comes first.
  Remove a referenced Quadlet file after the Quadlet files that name the
  referenced file.
- **No `start_at_boot` on the service task.** systemd cannot enable a service that
  Podman's generator made. Whether the container starts at boot is the
  `[Install]` section of the Quadlet file: `WantedBy=multi-user.target` for
  the system, `WantedBy=default.target` for an account. So the
  `systemd_service` task for a generated service has `state` and no
  `start_at_boot`. MMX refuses `start_at_boot` there in two places:
  - **When the project loads,** for a `systemd_service` task whose unit is
    the generated service of a `podman_quadlet` task among the node tasks of
    the same fleet stage, in the same systemd, before anything runs.
  - **On the node,** for every unit that systemd reports as generated. The
    failure names the file that the unit comes from (the unit's
    `SourcePath`), and gives the `[Install]` advice when that is a Quadlet
    file.

  One kind of generated unit can be enabled: the unit that systemd makes
  from an `/etc/init.d` script. `systemctl enable` delegates enabling the
  init-script unit to the distribution's own tool.
  MMX reads `systemctl is-enabled` for generated init-script units and
  manages `start_at_boot` as for ordinary units (measured on Debian 13).
- **Keep secret values out of the Quadlet file.** Put secret values into
  a separate private file that the Quadlet file names with `EnvironmentFile=`.

### What restarts a Podman container

A Quadlet container is a systemd service, so the ordinary records apply
([execution design](../../docs/changing-mmx/design/execution.md#service-reloads-follow-configuration-hashes)).
The `systemd_service` task in `activate` looks at what the node task block's
`changes` declare:

| What changed | What the `systemd_service` task does |
|---|---|
| The container's Quadlet file | Restarts the service, also with `refresh: reload`. A Quadlet file counts as the service's definition, like a `systemd_unit` file. |
| The image ID from a `podman_image` task | Restarts the service, also with `refresh: reload`. Podman makes the container new at each start, so the restart uses the new image. |
| A file written by `file`, such as the configuration file above | Applies the task's `refresh`: `restart` by default, or `reload` when the Quadlet file's `[Service]` section has an `ExecReload=` line. |

Each of these still happens when a run stopped between the change and the
restart: the next run compares what the service last loaded with what is on
the node, and restarts or reloads once. No `shell` task is needed for a
mounted file, unlike with Docker Compose, because here systemd and MMX's
service record know the service.

Name in `activate` each service that must load a change. A changed `.volume`
or `.network` file changes nothing on a volume or network that exists;
Podman creates volumes and networks only when the volumes or networks are
missing.

Podman can also update images by itself between MMX runs: a container with
`AutoUpdate=registry` in its Quadlet file is replaced by Podman's own
`podman-auto-update.timer` when the image's registry tag refers to a
different image. Enable `podman-auto-update.timer` with a `systemd_service`
task. MMX neither prevents nor records Podman's automatic image and
container updates.
A project that wants every image change to come from an MMX run leaves
`podman-auto-update.timer` off and uses `podman_image`.

### Mounted files and SELinux

**Mount a directory, not a single file, for configuration that is reloaded.**
MMX replaces a file by writing a new one and renaming it over the old one. A
container that mounts the single file keeps the old one until it restarts.
A container that mounts the directory sees the new file at once.

**On a node with SELinux,** a container may read only files with a
container label. The `selinux_file_context` task of
[Run a container from a Quadlet file](#run-a-container-from-a-quadlet-file)
gives the directory and everything inside it the label `container_file_t`,
also the files that MMX writes there later. On a node without SELinux the
task reports skipped. A mount can also end with `:Z`, such as
`Volume=/srv/shop/uploads:/var/lib/shop/uploads:Z` in the Quadlet file, and
Podman then sets the label. For a directory whose files MMX writes and the
container reads again on a reload, use the `selinux_file_context` task
instead: the files that MMX writes later then carry the label too.

### The account's own systemd

With `in_systemd_of_user_account`, the Quadlet file belongs to that
account's own systemd, and the container runs as that account. Normally an
account's services stop when the account's last login session ends and do
not start at boot. The first task that needs the account's systemd therefore
makes the account's services start at boot and keep running after logout
(systemd calls this "lingering"), and says so in its result: `Enabled
lingering for account shop, so its services start at boot and keep running
after logout`. MMX has no setting to choose whether to enable lingering.
`podman_quadlet` enables lingering for the account named by
`in_systemd_of_user_account` after Podman's check accepts the Quadlet file.
A rejected Quadlet file leaves the account unchanged.

Podman needs the account, home directory, and ID ranges listed below.
When a requirement is missing, the task fails and names the missing
requirement. When an account requirement is missing, a dryrun reports
`not_checked`, because an earlier task in the run can still provide the
missing requirement:

- The account exists.
- It has a home directory, where Podman keeps the account's containers and
  images.
- The account named by `in_systemd_of_user_account` has its own ranges of
  additional user and group IDs for Podman's container processes.
  The ranges are called sub-user and sub-group IDs.
  `/etc/subuid` and `/etc/subgid` must each contain a line for the account.
  The system's account tools add the account's `/etc/subuid` and
  `/etc/subgid` lines when creating an ordinary account.
  The account tools add no sub-user or sub-group ID ranges for a system
  account, so create the account with `system: false`.

An ordinary account cannot publish a port below 1024 unless the node's
kernel setting `net.ipv4.ip_unprivileged_port_start` allows it.

### Removal

`state` says what is removed and whether data stays:

| `state` | Allowed on | What MMX does | What stays |
|---|---|---|---|
| `file_and_service_removed_named_volumes_kept` | every kind | Stops the generated service if it runs, deletes the Quadlet file, and reloads systemd. For a `.network` file MMX first removes the Podman network. | Named volumes with their data, images, and every directory of the node that was mounted into the container. |
| `volume_and_its_data_removed` | `.volume` only | First deletes the named volume with the data in it (`podman volume rm`). Then stops the generated service if running, deletes the Quadlet file, and reloads systemd. | Images. |

`file_and_service_removed_named_volumes_kept` and
`volume_and_its_data_removed` accept only `name`,
`in_systemd_of_user_account`, and `state`. Podman
removes a Quadlet container when its service stops, so stopping the service
removes the container. Both states report unchanged when everything is
already gone (the file and, for the second, the volume too), so a removal
playbook reruns unchanged.

The volume or network goes before anything is stopped. While a container
still uses a volume or a network, Podman refuses to remove it: the task fails
with Podman's message and changes nothing, and the Quadlet file stays for the
next run.

systemd stops a container together with the service of a volume file or a
network file that the container's file names. So removing such a file while
its container runs also stops the container. The result's `reason` names
what the task deleted and every service it stopped, the container's service
among them; in a dryrun it names what a run would delete and stop. Remove
the container's file first.

**Data can be kept first and deleted later.** A playbook may remove
`shop-web-data.volume` with `file_and_service_removed_named_volumes_kept`.
A later run may remove `shop-web-data.volume` with
`volume_and_its_data_removed`. The second task still deletes the volume.

After deleting a `.volume` file, MMX cannot read
the file's `VolumeName=` to find the retained volume.
Podman has no record linking a volume to the Quadlet file whose service
created the volume.
The generated service runs `podman volume create` with the volume's name
and only the labels declared in the `.volume` file.

When `file_and_service_removed_named_volumes_kept` removes a `.volume` file,
MMX records the volume's name on the node, in
`/var/lib/mmx/podman_volumes_of_removed_quadlet_files/`, in a file named for
the Quadlet file, below `system` or `account_<account>`. The second task
finds the volume by that record, deletes it, and removes the record. A task
that installs the file again removes the record too, because the installed
file names its volume.

When the file is gone and MMX has no record, because somebody deleted the
file by hand, MMX can check only the name that Podman gives a volume without
`VolumeName=`: `systemd-` and the file's base name. The `reason` then says
that only this name was checked.

## Errors and what they mean

Every reason starts with `podman_quadlet`, except the failure of a
`systemd_service` task with `start_at_boot`.

| The reason holds | What the reason means, and what to do |
|---|---|
| `the program podman is not installed; install Podman in an earlier node task` | See [Set up](#set-up). A dryrun reports the task `not_checked`. |
| `Podman 4.3.1 is installed, and Quadlet files need Podman 4.4 or newer` | The node's Podman has no Quadlet, as on Debian 12. Install a newer Podman in an earlier node task. |
| `Podman 4.9.3 is installed, and .pod Quadlet files need Podman 5.0 or newer; Podman 4.9.3 would ignore ... without a word` | The node's Podman does not know `.pod` files. Nothing was written. Use another kind, or a newer Podman. |
| `Podman's Quadlet generator ... is missing; install the podman package in an earlier node task` | Install the package `podman`. |
| `Podman's check of the new Quadlet file failed: the Quadlet generator of Podman ... makes no ... from ...` | The generator rejected the new file; the message ends with its own words. The installed file stays. Correct the file. A file that names another Quadlet file needs that file installed first. |
| `podman_quadlet requires exactly one of content or template` | With `state: present`, write `content` or `template`, not both. |
| `state: volume_and_its_data_removed is only for a .volume file; remove ... with state: file_and_service_removed_named_volumes_kept` | Another kind of file has no volume to delete. |
| `account shop has no home directory at /home/shop` | Create the account with a `user` task that says `create_home_directory: true`. |
| `account shop has no range of sub-user IDs in /etc/subuid` | Create the account with a `user` task that says `system: false`, or add the range to an existing account with `usermod --add-subuids` and `--add-subgids`. |
| `the systemd of account shop does not run, so Podman cannot be asked about the account's ...` | Start account `shop`'s systemd with `systemctl start user@<uid>.service`, as the message says. |
| From `systemd_service`: `unit shop-web.service is generated from the Quadlet file ..., so systemd can neither enable nor disable it; remove start_at_boot from this task` | See [Rules for the files](#rules-for-the-files). |
| `... uses a systemd specifier, so MMX cannot tell the volume's name; remove the volume with a shell task that runs podman volume rm` | The `.volume` or `.network` file names its volume or network with a systemd specifier, a placeholder that systemd replaces. For example, `%i` represents the instance name between `@` and `.service` in a service name. Remove the Podman volume or network with a `shell` task. |
| From `wait_for`: `the container shop-web declares no health check, so Podman never reports it healthy` | Declare a health check with `HealthCmd=` in container `shop-web`'s Quadlet file. Alternatively, wait for a port or a url. |

## Tested on

**Distributions:** Debian 12 and 13, Ubuntu 24.04 and 26.04, Rocky Linux 9, AlmaLinux 10.

The suite `tests/integration/podman_containers.py` runs in system containers on Debian 12 and 13 and Ubuntu 24.04 and 26.04,
and in virtual machines on Rocky Linux 9 and AlmaLinux 10. It covers `podman_quadlet`, `podman_image`, and the
`wait_for` form `podman_container_is_healthy`:

| Distribution | Podman | Result |
|---|---|---|
| Debian 12 | 4.3.1 | No Quadlet; the suite checks that the failure names installed Podman 4.3.1 and required Podman 4.4. |
| Debian 13 | 5.4.2 | Passed. |
| Ubuntu 24.04 | 4.9.3 | Passed. |
| Ubuntu 26.04 | 5.7.0 | Passed. |
| Rocky Linux 9, AlmaLinux 10 | 5.8 | Passed. |

The suite also passed on Hetzner Cloud servers with Debian 12
and 13, and Ubuntu 24.04.

Not verified:

- Quadlet files of the kinds `.kube`, `.image`, and `.build` on a node. Go
  tests cover their name rule, the Podman version each needs, and the check,
  with a stand-in for Podman's generator. The suite starts `.container`,
  `.volume`, `.network`, and `.pod` files.
- Podman 4.4 to 4.8 and 5.0 to 5.3, which no supported distribution ships.
- A reboot of a node with Quadlet services and with an account's lingering
  services.

[Limits on each distribution](../../docs/reference/limits-on-each-distribution.md) says what is limited for this module on [Debian 12](../../docs/reference/limits-on-each-distribution.md#debian-12), and why.

## Why it works this way

- **Removal names the file and service.**
  `file_and_service_removed_named_volumes_kept` stops the generated service
  and removes the Quadlet file. Named volume data stays.

- **The Quadlet file is the declared thing.** It is Podman's own format and
  has every container setting. MMX adds no container settings of its own, so
  nothing is repeated and no setting is missing.
- **A container is a systemd service.** MMX already remembers what each
  service last loaded and restarts it once after a change, also after an
  interrupted run. Podman containers use that, with no second mechanism.
- **MMX does not drive `podman compose`.** Quadlet is Podman's own declared
  form, and it makes a container an ordinary systemd service that MMX's
  service records already handle.
- **Docker and Podman have separate modules.** They work differently on the
  server: Docker has a daemon and Compose, Podman has systemd units. A shared
  name would hide which one runs.
- **No module installs Podman.** Installing is a package task, which already
  handles versions and removal.
- **The removal states say whether data stays.** A reader must see from the
  one line whether data is deleted.
- **The volume goes before anything is stopped.** A container's generated
  service requires the service of the volume file it names. If MMX stopped
  that first, systemd would stop the container, and Podman would then delete
  a volume that was in use a moment before.
- **There is no setting for lingering.** A server container always needs it,
  and a setting that always has one value should not exist.
- **The check reads the generator's output, not its exit code.** Podman 4.9
  exits with 0 for some files it cannot use, and every version exits with 1
  for a good new file beside another installed file that it rejects.
- **The stand-in file.** A new file that the generator cannot read at all (a
  line that is no section, key, or comment) is passed over, by Podman 4.9
  without a word. Without the stand-in, the generator would read the
  installed file with the new Quadlet file's name and print the installed
  file's service. MMX would then install the broken replacement (measured
  with Podman 4.9.3 and 5.4.2).
- **The account runs the check of its own files.** The account fills its
  Quadlet directory, and a generator run by root would read whatever a link
  there points to.
- **A file kind that the node's Podman does not know is refused.** An older
  generator ignores a file of a kind it does not know and says nothing
  (measured: `.pod` and `.build` with Podman 4.9.3).
- **A file that another names is installed first.** Podman's generator
  rejects a container file that names a Quadlet file it cannot find
  (measured with Podman 5.4 to 5.8; Podman 4.9 accepts it).
- **`start_at_boot` is refused when the project loads, too.** On the node, a
  dryrun before the first run has no generated unit to look at, so only the
  run could fail, after it installed the Quadlet files.
