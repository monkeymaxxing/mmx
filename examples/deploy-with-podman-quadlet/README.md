# Deploy a container with Podman and Quadlet files

Prepare the server with the example [podman-host](../podman-host/). Then set
the server address and the image in `inventories/example.yml`, and run:

```sh
mmx dryrun deploy
mmx run deploy
mmx run deploy
```

A Quadlet file is a small file in systemd's unit syntax that describes a
container. At each systemd reload, Podman turns every Quadlet file into a
systemd service. The container then is an ordinary systemd service.

On the node, in order:

1. MMX writes the application's configuration file into `/etc/shop-web`, a
   directory of the node that the container reads.
2. If the node does not have the image, MMX pulls the image.
3. MMX installs two Quadlet files under `/etc/containers/systemd/`:
   `shop-web-data.volume` for the application's data and
   `shop-web.container` for the container. Podman's generator checks each
   new Quadlet file before MMX installs the Quadlet file. systemd then has
   the services `shop-web-data-volume.service` and `shop-web.service`.
4. MMX starts `shop-web.service`, which creates the volume and the
   container, and waits until the container's own health check reports
   healthy.

The second run changes nothing.

## What a later run does

| You change | The next `mmx run deploy` |
|---|---|
| `shop_web_greeting` | Writes the configuration file and reloads the service: Podman runs the reload command inside the container, and the same process reads the file again. |
| `shop_web_image` to the next tag | Pulls the new image, installs the changed `shop-web.container`, and restarts the service once. |
| Anything else in `shop-web.container` | Checks and installs `shop-web.container` and restarts the service once. |
| Nothing | Nothing. |

If a run stops after a change and before the restart or reload, the next run
still restarts or reloads the service once. MMX remembers what the service
last loaded, not what the last run changed.

## Stop for a backup, and remove

```sh
mmx run back_up                # stop, copy the volume to a tar file, start
mmx run remove_and_keep_data   # remove the container; the volume stays
mmx run remove_with_data       # remove the container and delete the volume
```

`back_up` writes `/var/lib/shop-web-backups/shop-web-data.tar` on the node.

`remove_with_data` also deletes the volume when `remove_and_keep_data` ran
before it. Only the file `shop-web-data.volume` says which volume is its
own, and `remove_and_keep_data` deletes that file. So MMX records the
volume's name on the node at that moment, and `remove_with_data` finds the
volume by the record.

No playbook deletes the image or `/etc/shop-web`. Deleting a task from a
playbook removes nothing on the node.

## Why it is written this way

**The configuration is a directory, not a single file.** MMX replaces a
file by writing a new one and renaming it over the old one. A container that
mounts the single file keeps seeing the old one until it restarts. A
container that mounts the directory sees the new file at once, so a reload
is enough.

**The SELinux label rule.** On a node with SELinux, a container may read
only files that carry a container label. The `selinux_file_context` task
gives `/etc/shop-web` and everything inside it that label, also files that
MMX writes later. On a node without SELinux the task reports skipped. On the
Red Hat family the task needs the package `policycoreutils-python-utils`.

**The volume's file before the container's file.** The container's file
names `shop-web-data.volume`. Podman's generator rejects a container file
that names a Quadlet file it cannot find, so the named file is installed
first. Removal goes the other way: the container first.

**`refresh: reload` on the service task.** It says what a changed
configuration file needs: a reload, which the Quadlet file's `ExecReload=`
line carries out inside the container. A new image and a changed Quadlet
file always restart the service, also with `refresh: reload`, because the
running container was made from the old ones.

**No `start_at_boot` on the service task.** systemd cannot enable a service that
a generator made. Whether the container starts at boot is the `[Install]`
section of the Quadlet file.

## Distributions

The example needs Podman 4.4 or newer: Debian 13, Ubuntu 24.04 and 26.04,
Rocky Linux 9, and AlmaLinux 10.

Debian 12 ships an older Podman, which has no Quadlet files: there the
`podman_quadlet` tasks fail and name both versions.

For every field, see the
[`podman_quadlet`](../../docs/reference/modules/podman_quadlet.md) and
[`podman_image`](../../docs/reference/modules/podman_image.md) pages.
