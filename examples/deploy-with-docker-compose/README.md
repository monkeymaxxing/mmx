# Deploy an application with Docker Compose

A small web application as a Docker Compose project: one container, a
configuration file mounted into the container, and a named volume for its
data. The node needs Docker and its Compose plugin; the [docker-host
example](../docker-host/README.md) installs them.

Set the server's address in `inventories/example.yml`, then run:

```sh
mmx dryrun deploy
mmx run deploy
mmx run deploy
```

The first run creates `/opt/shop` with the Compose file, the environment
file, and the directory `config`, and starts the container. Then MMX writes
the configuration file, tells the application to read the file, and waits
up to two minutes for Docker to report the container healthy. If the wait
ends without a healthy container, the task fails. The second run changes
nothing.

The inventory holds what is specific to the application. The example
application is nginx, which answers on `http://127.0.0.1:8080/` of the node.
For your own application, set its image, its health check, where it reads
its configuration file, and the command that makes it read the file again.

**Supported distributions:** Debian 12 and 13, Ubuntu 24.04 and 26.04, with Docker
and Docker Compose 2.26.1 or newer installed.

## What a later run does

Change a value in `inventories/example.yml` and run `mmx run deploy` again:

| You change | The run |
|---|---|
| `shop_config` | Writes the configuration file and runs the reload command in the container. The container keeps running. |
| `shop_time_zone` (the environment file) | Writes the environment file and recreates the container, because its definition differs. |
| `shop_image` to another version | Writes the Compose file, pulls the image, and recreates the container. |
| Nothing | Changes nothing. |

A dryrun shows the difference of each file MMX would write. For the Compose
project, the dryrun then reports that an earlier node task would change the
project's files. MMX compares the services with the services' containers
only once the files are installed.

## The other playbooks

```sh
mmx run back_up_data
mmx run remove_and_keep_data
mmx run remove_and_delete_data
```

- `back_up_data` stops the container, writes the volume's files to
  `/srv/shop-backups/web_data.tar`, and starts the container again. When the
  backup fails, the Compose project stays stopped until `back_up_data` is
  run again.
- `remove_and_keep_data` removes the container and the project's network.
  The named volume `shop_web_data` stays, so `mmx run deploy` brings the
  application back with its data.
- `remove_and_delete_data` also deletes the named volume, and then the
  directory `/opt/shop`.

Each removal reports unchanged when it is run again. No playbook deletes the
image; the docker-host example's cleanup timer does that later.

## Why it is written this way

**Two node task blocks.** Compose recreates a container when its definition
or its image changes, and `docker_compose_project` reports that. Compose
does not look at the content of a mounted file. So the configuration file
has a block of its own, whose `shell` task with
`only_when_tracked_inputs_changed` runs the reload command when the file
changed, also when an earlier run wrote the file and stopped before the
command.

**The directory is mounted, not the file.** MMX replaces a file with a new
file of the same name. A container that mounts one file keeps the old file;
a container that mounts the directory sees the new file.

**The environment file comes before the Compose file.** The Compose file's
`check_new_file` runs `docker compose config`, which reads the environment
file that the Compose file names.

**The health wait stands after the configuration.** nginx answers on port
8080 only once it has its configuration file, so the wait is in the `verify`
of the second block.

**The volume has a fixed name.** The Compose file names the volume
`shop_web_data`, so the backup finds its files at one path whatever Compose
would call the volume by itself.

**Every Compose task names the project.** `compose_project_name: shop` is
the name that Compose would give `/opt/shop` by itself. The task in
`remove_and_delete_data` must name the project, because Docker records the
project's name on a named volume, not its directory; the other tasks give
the same name, so every Compose task acts on one project.

**`pull_images: when_missing`.** The image's tag names one version, so a
rerun never replaces the container because a tag moved. A project that
follows a tag such as `stable` says `pull_images: always`.

For every field, see
[Run containers](../../docs/how-to/run-containers.md).
