# A Docker host

A Debian or Ubuntu server that runs Docker, from Docker's own package
repository. Set the server's address in `inventories/example.yml`, then run:

```sh
mmx dryrun docker_host
mmx run docker_host
mmx run docker_host
```

On the node, in order, the playbook:

1. adds Docker's package repository with its signing key;
2. installs `docker-ce` and `docker-ce-cli` together inside `5:29.*`,
   with containerd and the Compose plugin beside them;
3. writes the daemon's settings to `/etc/docker/daemon.json` and starts the
   daemon;
4. adds the account `deploy` to the group `docker`;
5. installs a systemd timer that deletes unused images every Sunday.

The second run changes nothing.

**Supported distributions:** Debian 12 and 13, Ubuntu 24.04 and 26.04. Docker
publishes its repository for the Red Hat family too; the
[`docker_compose_project` page](../../docs/reference/modules/docker_compose_project.md#set-up)
says what differs there and with a distribution's own Docker packages.

## Why it is written this way

**The version pattern.** The `docker-ce` and `docker-ce-cli` package entries
use `version: "5:29.*"`. MMX writes the `docker-ce` and `docker-ce-cli`
version rules before one installation, so the daemon and CLI stay inside
Docker 29 together. A matching installed version stays; whole-node upgrades
can move it inside the pattern. See [version
rules](../../docs/reference/modules/apt_package.md#keep-a-package-at-a-version).

**The package does not start a stopped daemon.** By default, MMX's package
policy blocks starting stopped services but permits restarting running
services. The `systemd_service` task starts the daemon after `daemon.json`
is in place and restarts the daemon when `daemon.json` changes. With
`"live-restore": true`, containers keep running during the daemon's restart.

**The group `docker`.** Anyone in the group `docker` can start a container
that mounts the node's whole filesystem, so membership means root on the
server. Add only administrators.

**The cleanup timer.** No MMX module deletes an image. Old images still fill
the disk, so the playbook schedules `docker image prune`, which deletes the
images that no container uses and that have been on the node for more than
seven days.

For the fields of `docker_image` and `docker_compose_project`, which run
containers on this host, see [Run
containers](../../docs/how-to/run-containers.md). The
[deploy-with-docker-compose
example](../deploy-with-docker-compose/README.md) deploys an application to
the Docker server.
