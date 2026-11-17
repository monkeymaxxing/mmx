# Run containers

**Who acts:** you, or your coding agent, in your MMX project on your
controller machine: the computer where you run `mmx`. A coding agent cannot
type at a prompt, so [Pull from a private registry](#pull-from-a-private-registry)
stores the registry password from a file.

**When to use this page:** you want your servers to run an application in
Docker containers, from a Compose file or as one container that systemd
starts. You may also need to pull images from a private registry, or to
deploy the newest image under a tag that its publisher moves.

**What this page does:** you install Docker on the server `app01`, run a
Compose project with two containers, and deploy a new image version. The
other routes run one container as a systemd service, pull from a private
registry with a login, and make each run deploy the newest image under a
tag.

**Terms:**

- A **node** is a server that MMX manages, listed in an **inventory** file
  in `inventories/`. A **playbook** is a file in `playbooks/` that lists
  **fleet stages**. Each fleet stage picks nodes, and each of those nodes
  runs the fleet stage's **node tasks**, in order.
- A node task names one **module**, the code that does the work, such as
  `docker_compose_project`. A **node task block** groups a change with what
  follows it: `changes` writes files, `activate` puts the files into use, and
  `verify` checks the result.
- A **container** runs an application in an isolated environment on the
  server. An **image** is the packaged filesystem that a container starts
  from, such as `nginx:1.27.3`.
- A **registry** is the server that images are pulled from, such as
  Docker Hub or `registry.example.com`.
- An **image reference** such as `registry.example.com/shop/web:2.4` names
  the registry, the image, and a **tag**, here `2.4`. A reference without a
  registry, such as `nginx:1.27.3`, means Docker Hub. Whoever publishes an
  image can move a tag to a newer image. A **digest**, such as
  `nginx@sha256:...`, names exactly one image forever.
- A **Compose project** is a directory with a file `compose.yaml` that
  declares several containers, called **services**, with their images,
  ports, and volumes. The `docker compose` command creates and updates the
  containers from that file.

## Choose your route

| You want to | Route |
| --- | --- |
| Install Docker on a server | [Install Docker](#install-docker) |
| Run several containers that a `compose.yaml` declares | [Run a Compose project](#run-a-compose-project) |
| Deploy a new version of an image | [Deploy a new image version](#deploy-a-new-image-version) |
| Run one container as a systemd service, outside a Compose project | [Run one container with systemd](#run-one-container-with-systemd) |
| Pull images from a registry that needs a login | [Pull from a private registry](#pull-from-a-private-registry) |
| Deploy the newest image under a tag, such as `latest`, on each run | [Follow a tag that moves](#follow-a-tag-that-moves) |
| Run containers with Podman instead of Docker | [Containers with Podman](#containers-with-podman) |

## Before you start

- You have a project where `mmx run` worked, as in
  [Get started](../get-started.md). Run `mmx` from a directory inside the
  project.
- The examples use the server `app01` at `203.0.113.41` with Debian 13, in
  the group `app_servers`. The Docker installation below works on Debian
  and Ubuntu. For Fedora, Rocky Linux, or AlmaLinux, the
  [Set up section of `docker_compose_project`](../reference/modules/docker_compose_project.md#set-up)
  says what differs.
- Your inventory lists the server:

  ```yaml
  # inventories/example.yml
  nodes:
    app01:
      address: 203.0.113.41
      ssh_user: root

  groups:
    app_servers:
      nodes: [app01]
  ```

  **Check:** `mmx nodes` shows the group beside the server:

  ```text
  Nodes
    app01   203.0.113.41   root   app_servers

  Groups
    app_servers   app01
  ```

- Docker runs on the server, for every route except
  [Install Docker](#install-docker) itself. `mmx run-command --nodes app01 -- docker compose version`
  prints `Docker Compose version v2.26.1` or newer when Compose works.
  Otherwise, follow [Install Docker](#install-docker) first.
- `mmx describe-module docker_compose_project` prints the module's whole
  page: every input, its results, and what a dryrun does. The same works
  for `docker_image` and `wait_for`.

## Install Docker

MMX has no module that installs Docker. A playbook installs Docker with the
same package, file, and service tasks as any other software, so each step
stays visible.

1. **Write the playbook that installs Docker.** Create
   `playbooks/install_docker.yml`:

   ```yaml
   # playbooks/install_docker.yml
   fleet_stages:
     - id: install_docker
       nodes: app_servers
       node_tasks:
         - id: add_docker_repository
           apt_repository:
             name: docker
             uris: ["https://download.docker.com/linux/{{ .node.distribution.id }}"]
             suites: ["{{ .node.distribution.codename }}"]
             components: [stable]
             signing_key_url: https://download.docker.com/linux/debian/gpg
             signing_key_sha256: 1500c1f56fa9e26b9b8f42452a553675796ade0807cdce11975eb98170b3a570

         - id: install_docker_packages
           apt_package:
             packages:
               - {name: docker-ce, version: "5:29.*"}
               - {name: docker-ce-cli, version: "5:29.*"}
               - {name: containerd.io, version: newest_when_first_installed}
               - {name: docker-compose-plugin, version: newest_when_first_installed}

         - task_block:
             id: docker_daemon
             changes:
               - id: write_daemon_settings
                 file:
                   path: /etc/docker/daemon.json
                   permissions_mode: "0644"
                   content: |
                     {
                       "log-driver": "json-file",
                       "log-opts": {"max-size": "50m", "max-file": "3"},
                       "live-restore": true
                     }
             activate:
               - id: start_docker
                 systemd_service:
                   name: docker
                   state: running
                   start_at_boot: true
                   refresh: restart
   ```

   On the node, in order: `add_docker_repository` adds Docker's own package
   source, `install_docker_packages` installs Docker and its Compose
   plugin, `write_daemon_settings` writes the Docker daemon's settings, and
   `start_docker` starts the daemon.

   - One `signing_key_url` serves Debian and Ubuntu, and MMX checks the key
     against `signing_key_sha256`: the
     [Set up section of `docker_compose_project`](../reference/modules/docker_compose_project.md#set-up)
     says why.
   - The version pattern `5:29.*` keeps the Docker daemon (`docker-ce`) and
     the `docker` command (`docker-ce-cli`) inside Docker 29 together.
   - MMX installs a package without starting its services, so
     `start_docker` starts the daemon after `daemon.json` is in place.
     `refresh: restart` restarts the daemon when `daemon.json` changes. With
     `"live-restore": true`, Docker keeps the containers running during
     that restart.
   - The `log-opts` limit each container's log to three files of 50 MB.

   **Check:** `mmx validate install_docker` prints `Valid:` and the
   project's name.

2. **Preview the installation.**

   ```sh
   mmx dryrun install_docker
   echo "exit code $?"
   ```

   **Check:** on a server without Docker, `add_docker_repository` says
   `would_change`, the other node tasks say `not_checked` or `skipped`,
   and the exit code is 3. A dryrun
   changes nothing, so the node has no Docker package source yet, and the
   dryrun cannot check what depends on the package source. Exit code 3
   means that some work could not be checked.
   [Preview and run changes](preview-and-run-changes.md#what-the-results-mean)
   explains each result.

3. **Install Docker.**

   ```sh
   mmx run install_docker
   ```

   **Check:** every node task says `succeeded`, and the run ends with
   `Run ...: succeeded` and exit code 0. On this first run, `start_docker`
   says `service was not running; started`, because MMX started the daemon instead of
   restarting the daemon. A second `mmx run install_docker` says `unchanged` for
   every node task.

4. **Check that Docker and its Compose plugin answer.**

   ```sh
   mmx run-command --nodes app01 -- docker compose version
   ```

   **Check:** the server prints the Compose version:

   ```text
   run-command / app01: succeeded
     run-command / app01 / command: succeeded, changed
       Docker Compose version v5.7.1
   ```

   `docker_compose_project` needs Docker Compose 2.26.1 or newer. If the
   command fails, the run in step 3 says which node task failed and why.

## Run a Compose project

The node task block below writes `compose.yaml` and then has Docker
Compose create the containers from it. The example runs two services:
`web`, an nginx web server, and `cache`, a Redis server.

1. **Write the playbook.** Create `playbooks/run_shop.yml`:

   ```yaml
   # playbooks/run_shop.yml
   fleet_stages:
     - id: run_shop
       nodes: app_servers
       node_tasks:
         - task_block:
             id: shop_containers
             changes:
               - id: create_project_directory
                 directory:
                   path: /opt/shop
                   permissions_mode: "0755"
               - id: write_compose_file
                 file:
                   path: /opt/shop/compose.yaml
                   permissions_mode: "0644"
                   content: |
                     services:
                       web:
                         image: nginx:1.27.3
                         restart: unless-stopped
                         ports: ["127.0.0.1:8080:80"]
                         healthcheck:
                           test: ["CMD", "curl", "--fail", "--silent", "--output", "/dev/null", "http://127.0.0.1/"]
                           interval: 5s
                       cache:
                         image: redis:7.4.1
                         restart: unless-stopped
                         healthcheck:
                           test: ["CMD", "redis-cli", "ping"]
                           interval: 5s
                   check_new_file:         # check the new file before it replaces the live one
                     shell:
                       script: >-
                         docker compose --project-directory /opt/shop
                         --file "$NEW_FILE" config --quiet
                       dryrun: >-
                         docker compose --project-directory /opt/shop
                         --file "$NEW_FILE" config --quiet
                       env:
                         NEW_FILE: "{{ .new_file.path }}"
             activate:
               - id: start_containers
                 docker_compose_project:
                   compose_project_name: shop
                   compose_project_dir: /opt/shop
                   state: running
                   pull_images: when_missing
                   containers_of_removed_services: remove
             verify:
               - id: wait_until_healthy
                 wait_for:
                   docker_compose_project_is_ready: /opt/shop
                   wait_at_most: 2m
   ```

   On the node, in order:

   1. `create_project_directory` creates `/opt/shop`.
   2. `write_compose_file` writes the new `compose.yaml` to a temporary
      file first, and `check_new_file` runs `docker compose config` on the
      temporary file. Only a file that Compose accepts replaces
      `/opt/shop/compose.yaml`. The `dryrun` line runs the same check in a
      dryrun, because `docker compose config` changes nothing.
   3. `start_containers` runs `docker compose up`. Compose creates a missing
      container, recreates a container whose definition or image differs,
      and starts a stopped container.
   4. `wait_until_healthy` waits up to two minutes until every container
      with a health check reports healthy, and every other container runs
      or has completed. A **health check** is a command that Docker runs
      inside the container again and again, such as `redis-cli ping`.
      "Started" does not mean "working": a container with a restart policy
      shows as running while its application fails again and again.

   Three inputs of `docker_compose_project` are required with
   `state: running`, and none of the three has a default:

   - `compose_project_name: shop` names the project. Every state requires
     the name. Give each Compose project on a server its own name.
   - `pull_images: when_missing` pulls an image only when the node does not
     have the image. A rerun then does not pull a newer image under a tag that moved.
     [Follow a tag that moves](#follow-a-tag-that-moves) shows the other
     choice, `always`.
   - `containers_of_removed_services: remove` removes the container of a
     service that you delete from `compose.yaml`. `keep` leaves that
     container running.

   Every container setting is in `compose.yaml`, in Docker's own format.
   `restart: unless-stopped` makes Docker start the containers again after
   a reboot, without MMX.

   **Check:** `mmx validate run_shop` prints `Valid:` and the project's
   name.

2. **Preview the Compose project.**

   ```sh
   mmx dryrun run_shop
   echo "exit code $?"
   ```

   The output has a line for `create_project_directory` and for
   `write_compose_file` (`would_change`, with `check_new_file` skipped), and
   `start_containers` with `{"images":[],"services":[]}`.

   **Check:** the exit code is 3. On this first dryrun, `/opt/shop` does not
   exist yet, so the dryrun skips the file check, and `wait_until_healthy`
   says `not_checked` because the project has no containers to wait for. `start_containers` says `would_change` without naming the
   services: MMX compares the services with the containers only once the
   new `compose.yaml` is on the node.

3. **Run the Compose project.**

   ```sh
   mmx run run_shop
   ```

   ```text
   run_shop / app01: succeeded
     run_shop / app01 / shop_containers/changes/create_project_directory: succeeded, changed
     run_shop / app01 / shop_containers/changes/create_project_directory: directory "/opt/shop"
       checked 1 directories and 0 files; 1 metadata updates
     run_shop / app01 / shop_containers/changes/write_compose_file: succeeded, changed
     run_shop / app01 / shop_containers/changes/write_compose_file: created "/opt/shop/compose.yaml"
       absent -> 371 bytes, permissions_mode 0644, uid 0, gid 0
     run_shop / app01 / shop_containers/activate/start_containers: succeeded, changed
       {"images":[{"image":"nginx:1.27.3","image_id":"sha256:bc2f6a7c8ddbccf55bdb19659ce3b0a92ca6559e86d42677a5a02ef6bda2fcef","previous_image_id":null},{"image":"redis:7.4.1","image_id":"sha256:bb142a9c18ac18a16713c1491d779697b4e107c22a97266616099d288237ef47","previous_image_id":null}],"services":[{"action":"created","name":"cache"},{"action":"created","name":"web"}]}
     run_shop / app01 / shop_containers/verify/wait_until_healthy: succeeded, unchanged — each of the 2 containers of the Compose project in /opt/shop is healthy or, without a health check, runs or has completed
     run_shop / app01 / shop_containers: succeeded, changed
   Run 295ab55f: succeeded
   ```

   The line below `start_containers` is the node task's result. `services`
   says what Compose did to each service, here `created`. `images` lists
   each image that the run pulled. `previous_image_id` is `null` because
   the node did not have the image before.

   **Check:** both services say `created`, `wait_until_healthy` says
   `succeeded`, and the run ends with `Run ...: succeeded` and exit code 0.

4. **Check the containers on the server.**

   ```sh
   mmx run-command --nodes app01 -- 'curl --silent http://127.0.0.1:8080/ | grep "<title>"; docker compose --project-directory /opt/shop ps --format "{{.Service}}: {{.Status}}"'
   ```

   **Check:** nginx answers on port 8080 of the server, and both containers
   say `healthy`:

   ```text
     run-command / app01 / command: succeeded, changed
       <title>Welcome to nginx!</title>
       cache: Up 7 seconds (healthy)
       web: Up 8 seconds (healthy)
   ```

5. **Run it again.**

   ```sh
   mmx run run_shop
   ```

   **Check:** every node task says `unchanged`, and `start_containers`
   lists both services as unchanged:

   ```text
     run_shop / app01 / shop_containers/activate/start_containers: succeeded, unchanged
       {"images":[],"services":[{"action":"unchanged","name":"cache"},{"action":"unchanged","name":"web"}]}
   ```

## Deploy a new image version

To deploy a new version, change the image's tag in `compose.yaml`. Compose
then pulls the new image and recreates the containers of that service only.

1. **Change the tag.** In `playbooks/run_shop.yml`, change the `web`
   service's image from `nginx:1.27.3` to `nginx:1.27.4`:

   ```yaml
                       web:
                         image: nginx:1.27.4
   ```

   **Check:** `mmx validate run_shop` prints `Valid:`.

2. **Preview the change with the file's difference.**

   ```sh
   mmx dryrun run_shop --diff
   echo "exit code $?"
   ```

   ```text
     run_shop / app01 / shop_containers/changes/write_compose_file: succeeded, would_change
       check_new_file: succeeded
     run_shop / app01 / shop_containers/changes/write_compose_file: would update "/opt/shop/compose.yaml"
       371 bytes, permissions_mode 0644, uid 0, gid 0 -> 371 bytes, permissions_mode 0644, uid 0, gid 0
       --- before
       +++ after
       @@ -1,6 +1,6 @@
        services:
          web:
       -    image: nginx:1.27.3
       +    image: nginx:1.27.4
            restart: unless-stopped
            ports: ["127.0.0.1:8080:80"]
            healthcheck:
     run_shop / app01 / shop_containers/activate/start_containers: succeeded, would_change — an earlier node task of this dryrun would change a file of this Compose project or of this node task block; MMX can compare the services with the containers only once the files are installed
       {"images":[],"services":[{"action":"would_change","name":"cache","reason":"an earlier node task of this dryrun would change a file of this Compose project or of this node task block"},{"action":"would_change","name":"web","reason":"an earlier node task of this dryrun would change a file of this Compose project or of this node task block"}]}
   ...
   exit code 2
   ```

   **Check:** the difference shows the new tag, `check_new_file` says
   `succeeded`, and the exit code is 2: something would change. Both
   services say `would_change`, because the dryrun cannot compare a
   service with its container before the new `compose.yaml` is on the
   node. The run in step 3 says which service really changes.

3. **Deploy the new version.**

   ```sh
   mmx run run_shop
   ```

   **Check:** `start_containers` pulled the new image, recreated `web`, and
   left `cache` alone:

   ```text
     run_shop / app01 / shop_containers/activate/start_containers: succeeded, changed
       {"images":[{"image":"nginx:1.27.4","image_id":"sha256:09369da6b10306312cd908661320086bf87fbae1b6b0c49a1f50ba531fef2eab","previous_image_id":null}],"services":[{"action":"unchanged","name":"cache"},{"action":"recreated","name":"web"}]}
     run_shop / app01 / shop_containers/verify/wait_until_healthy: succeeded, unchanged — each of the 2 containers of the Compose project in /opt/shop is healthy or, without a health check, runs or has completed
   ```

A tag such as `1.27.4` names one version by convention only, because its
publisher can still move the tag. To deploy exactly the image that you
tested, name the image by its digest, such as `nginx@sha256:...`. A digest
names one image, so a run never deploys anything else under the digest.

## Run one container with systemd

The other way to run a Docker container is a systemd unit that runs
`docker run`. systemd then starts, restarts, and stops the container like
any service. `docker_image` supplies the image. The example runs Redis as
the shop's job queue.

1. **Write the playbook.** Create `playbooks/run_shop_queue.yml`:

   ```yaml
   # playbooks/run_shop_queue.yml
   fleet_stages:
     - id: run_shop_queue
       nodes: app_servers
       node_tasks:
         - task_block:
             id: shop_queue
             changes:
               - id: pull_queue_image
                 docker_image:
                   image: redis:7.4.2
                   pull_image: when_missing
               - id: write_queue_unit
                 systemd_unit:
                   name: shop-queue.service
                   content: |
                     [Unit]
                     Description=The shop's job queue in a Docker container
                     Requires=docker.service
                     After=docker.service

                     [Service]
                     ExecStartPre=-/usr/bin/docker rm --force shop-queue
                     ExecStart=/usr/bin/docker run --rm --name shop-queue --publish 127.0.0.1:6379:6379 redis:7.4.2
                     ExecStop=/usr/bin/docker stop shop-queue
                     Restart=always

                     [Install]
                     WantedBy=multi-user.target
             activate:
               - id: start_queue
                 systemd_service:
                   name: shop-queue
                   state: running
                   start_at_boot: true
             verify:
               - id: wait_for_queue_port
                 wait_for:
                   tcp_port_accepts_connections: 6379
                   wait_at_most: 1m
   ```

   On the node, in order: `pull_queue_image` pulls `redis:7.4.2` if the
   node does not have the image. `write_queue_unit` installs
   `/etc/systemd/system/shop-queue.service`. `start_queue` starts the
   service and starts the service at boot. `wait_for_queue_port` waits
   until Redis accepts connections on port 6379 of the server.

   `start_queue` restarts the service when the unit file changed, and also
   when `pull_queue_image` pulled an image with another ID. `docker_image`
   only pulls: it neither starts nor removes a container, and it removes no
   image. [One image for a container that systemd runs](../reference/modules/docker_image.md#one-image-for-a-container-that-systemd-runs)
   shows the same route with a registry login.

   **Check:** `mmx validate run_shop_queue` prints `Valid:`.

2. **Preview the service.** `mmx dryrun run_shop_queue` says `would_change`
   for `pull_queue_image` and `write_queue_unit`, and exits with 3, because
   nothing listens on port 6379 before the run.

3. **Run the service.**

   ```sh
   mmx run run_shop_queue
   ```

   ```text
   run_shop_queue / app01: succeeded
     run_shop_queue / app01 / shop_queue/changes/pull_queue_image: succeeded, changed
       {"image_id":"sha256:fbdbaea47b9ae4ecc2082ecdb4e1cea81e32176ffb1dcf643d422ad07427e5d9","previous_image_id":null}
     run_shop_queue / app01 / shop_queue/changes/write_queue_unit: succeeded, changed
     run_shop_queue / app01 / shop_queue/changes/write_queue_unit: created "/etc/systemd/system/shop-queue.service"
       absent -> 359 bytes, permissions_mode 0644, uid 0, gid 0
     run_shop_queue / app01 / shop_queue/activate/start_queue: succeeded, changed — service was not running; started
     run_shop_queue / app01 / shop_queue/verify/wait_for_queue_port: succeeded, unchanged — TCP port 127.0.0.1:6379 accepts connections
     run_shop_queue / app01 / shop_queue: succeeded, changed
   Run 194044d3: succeeded
   ```

   **Check:** every node task says `succeeded`, and the run ends with exit
   code 0. Then ask Redis inside the container:

   ```sh
   mmx run-command --nodes app01 -- docker exec shop-queue redis-cli ping
   ```

   The server prints `PONG`. A second `mmx run run_shop_queue` says
   `unchanged` for every node task.

4. **Deploy a new version.** Change `redis:7.4.2` to `redis:7.4.3` in both
   places: in `pull_queue_image` and in the `ExecStart` line. Then run
   `mmx run run_shop_queue`.

   **Check:** the run pulls the new image, updates the unit file, and
   restarts the service: `start_queue` says `succeeded, changed — service
   definition changed`.

## Pull from a private registry

A registry that needs a login gets the login from the node task. The
example pulls the shop's own image `registry.example.com/shop/web:2.4` into
the Compose project of [Run a Compose project](#run-a-compose-project). The
registry's user name is `deploy`.

1. **Make sure that the Compose project runs.**

   ```sh
   mmx run run_shop
   ```

   **Check:** the run ends with `Run ...: succeeded`. Otherwise, follow
   [Run a Compose project](#run-a-compose-project) first.

2. **Store the registry password as a secret.** Save the password in a
   file, such as `~/Downloads/registry-password.txt`, then:

   ```sh
   mmx secrets set registry_password < ~/Downloads/registry-password.txt
   rm ~/Downloads/registry-password.txt
   ```

   [Add or change a secret without showing it](keep-secrets.md#add-or-change-a-secret-without-showing-it)
   explains the command, the vault passphrase, and the prompt.

   **Check:** `mmx secrets list-keys` prints `registry_password`.

3. **Give the task the login, and name the private image.** In
   `playbooks/run_shop.yml`, change the `web` service's image, and add
   `registry_logins` to `start_containers`:

   ```yaml
                       web:
                         image: registry.example.com/shop/web:2.4
   ```

   ```yaml
             activate:
               - id: start_containers
                 docker_compose_project:
                   compose_project_name: shop
                   compose_project_dir: /opt/shop
                   state: running
                   pull_images: when_missing
                   containers_of_removed_services: remove
                   registry_logins:
                     - registry: registry.example.com
                       registry_username: deploy
                       registry_password: "{{ .secrets.registry_password }}"
   ```

   `registry` is the registry's host as the image references write the
   host, with the port if the references have one, such as `registry.example.com:5000`.
   Docker Hub is `docker.io`. Add one entry for each registry that needs a
   login. A `docker_image` task takes the login as two inputs,
   `registry_username` and `registry_password`, for the registry that its
   `image` names.

   **Check:** `mmx validate run_shop` prints `Valid:`.

4. **If a private certificate authority signed the registry's
   certificate, install the authority's certificate.** Skip this step for
   a registry with a certificate from a public authority, such as Docker
   Hub or GitHub's registry. Save the authority's certificate as
   `files/registry-authority.crt` in your project, and add two node tasks
   at the start of `node_tasks`, before the node task block:

   ```yaml
         - id: create_registry_certificate_directory
           directory:
             path: /etc/docker/certs.d/registry.example.com
             create_missing_parent_directories: true

         - id: trust_registry_authority
           file:
             path: /etc/docker/certs.d/registry.example.com/ca.crt
             source: files/registry-authority.crt
             permissions_mode: "0644"
   ```

   Docker trusts the certificate in
   `/etc/docker/certs.d/<registry>/ca.crt` for that one registry. Without
   the certificate, the pull fails with `x509: certificate signed by
   unknown authority`.

   **Check:** `mmx validate run_shop` prints `Valid:`.

5. **Run.**

   ```sh
   mmx run run_shop
   ```

   **Check:** `start_containers` pulled the private image and recreated
   `web`:

   ```text
     run_shop / app01 / shop_containers/activate/start_containers: succeeded, changed
       {"images":[{"image":"registry.example.com/shop/web:2.4","image_id":"sha256:6d288bade923701eca55468516a1c360ca8fcc8b3e8b54433f19aa74ed80df55","previous_image_id":null}],"services":[{"action":"unchanged","name":"cache"},{"action":"recreated","name":"web"}]}
   ```

   `mmx run-command --nodes app01 -- curl --silent http://127.0.0.1:8080/`
   prints the page of the shop's image.

In a run, MMX hands the login to `docker` through the node task's private
directory, so no password appears on a command line, in `/root/.docker`, or
on a disk. A dryrun writes no login: the dryrun sends the login to the
Docker daemon together with its question about the image.
[A node task's private directory](../reference/secrets.md#a-node-tasks-private-directory)
has the details.

- **The task's own logins only.** A login that someone saved on the server
  with `docker login` does not count for the task. A login that must stay
  on the server, for people who pull by hand, is a `shell` task that runs
  `docker login --password-stdin` with `run_as_user_account`.
- **The result stays readable.**
  [A login does not hide a node task's result](../reference/secrets.md#a-login-does-not-hide-a-node-tasks-result)
  has the rule.

Module pages that rely on this section: [`docker_compose_project`](../reference/modules/docker_compose_project.md), [`docker_image`](../reference/modules/docker_image.md).

## Follow a tag that moves

Some images are published under a tag that moves, such as `latest`,
`stable`, or `2.4` for the newest build of version 2.4. With
`pull_images: always`, each run asks the registry which image each
reference names now. The run then pulls an image that differs from the
node's and recreates the containers of that image.

1. **Set `pull_images: always`.** In `playbooks/run_shop.yml`:

   ```yaml
                   pull_images: always
   ```

   For a `docker_image` task, the same choice is `pull_image: always`.

   **Check:** `mmx dryrun run_shop` reports `start_containers` as
   `unchanged`, with exit code 0, while the registry still names the image
   that the node has.

2. **Preview after the publisher moved the tag.** When the registry names a
   new image under `registry.example.com/shop/web:2.4`, a dryrun says so:

   ```sh
   mmx dryrun run_shop
   echo "exit code $?"
   ```

   ```text
     run_shop / app01 / shop_containers/activate/start_containers: succeeded, would_change
       {"images":[{"image":"registry.example.com/shop/web:2.4","image_id":"sha256:8f23c9f320f7ff1a37c280b0a394889d8287e6bb94a3ae9821a32ec42a87610a","previous_image_id":"sha256:6d288bade923701eca55468516a1c360ca8fcc8b3e8b54433f19aa74ed80df55"}],"services":[{"action":"unchanged","name":"cache"},{"action":"would_recreate","name":"web","reason":"the image differs: the registry names another image for registry.example.com/shop/web:2.4 now"}]}
   ...
   exit code 2
   ```

   **Check:** `web` says `would_recreate`, with the reason `the image
   differs`. The image's entry shows the node's image ID as
   `previous_image_id` and the registry's new image as `image_id`. The
   dryrun downloaded no image.

3. **Deploy the new image.**

   ```sh
   mmx run run_shop
   ```

   **Check:** `web` says `recreated`, and the image's entry shows both
   image IDs:

   ```text
     run_shop / app01 / shop_containers/activate/start_containers: succeeded, changed
       {"images":[{"image":"registry.example.com/shop/web:2.4","image_id":"sha256:8f23c9f320f7ff1a37c280b0a394889d8287e6bb94a3ae9821a32ec42a87610a","previous_image_id":"sha256:6d288bade923701eca55468516a1c360ca8fcc8b3e8b54433f19aa74ed80df55"}],"services":[{"action":"unchanged","name":"cache"},{"action":"recreated","name":"web"}]}
   ```

   A second `mmx run run_shop` says `unchanged`, because the project and
   the registry have not changed.

After the three routes above, `playbooks/run_shop.yml` has the two
certificate node tasks of step 4 before the node task block, the image
`registry.example.com/shop/web:2.4`, `pull_images: always`, and the
`registry_logins` entry in `start_containers`. The example project
[deploy-with-docker-compose](../../examples/deploy-with-docker-compose/README.md)
shows a whole Compose project.

## Containers with Podman

Podman runs containers without a daemon. With MMX, each Podman container
is a **Quadlet file**: a small file in systemd's unit syntax that Podman
turns into a systemd service. [`podman_quadlet`](../reference/modules/podman_quadlet.md)
installs a Quadlet file, [`podman_image`](../reference/modules/podman_image.md) pulls
an image, and `systemd_service` starts the generated service. MMX does not
drive `podman compose`. Two example projects show the whole job:
[podman-host](../../examples/podman-host/README.md) installs Podman, and
[deploy-with-podman-quadlet](../../examples/deploy-with-podman-quadlet/README.md)
runs a container.

## How a dryrun checks for a newer image

When a dryrun says `not_checked` about an image, or shows `null` for an
image ID, [Newer images under the same tag](../reference/modules/docker_image.md#newer-images-under-the-same-tag)
explains what the dryrun asks, why the image ID depends on Docker's image
store, and what an image for several platforms does. In short, a dryrun
with `pull_images: always` asks the registry without downloading an image,
and a dryrun with `when_missing` does not ask the registry, so a wrong
login or an untrusted certificate shows only in the run.

## If something is different

| What you see | What it means and what to do |
| --- | --- |
| The run fails with `tls: failed to verify certificate: x509: certificate signed by unknown authority`, after `node task did not finish after 3 tries` | Docker does not trust the registry's certificate. Install the authority's certificate as in [step 4 of Pull from a private registry](#pull-from-a-private-registry). A dryrun with `pull_images: when_missing` does not ask the registry, so only the run shows this message. |
| The run fails with `401 Unauthorized` | The registry refused the login. Store the right password with `mmx secrets set registry_password --replace < ~/Downloads/registry-password.txt`, and check `registry_username`. |
| A dryrun says `not_checked, unknown — docker_compose_project: the registry does not say which image registry.example.com/shop/web:2.4 names: unauthorized: authentication required. Check the login that the task gives for the registry` | The same refused login, seen by a dryrun with `pull_images: always`. A login saved on the server with `docker login` does not count for the task. |
| `mmx: vault passphrase required; use --vault-password-file, MMX_VAULT_PASSWORD_FILE, or MMX_VAULT_PASSWORD (prompts are disabled)`, exit code 4 | MMX needs the passphrase and may not ask for it: no terminal, `--non-interactive`, or `--json`. Set `MMX_VAULT_PASSWORD_FILE`, as [Keep secrets](keep-secrets.md#if-something-is-different) says. |
| The server runs Fedora, Rocky Linux, or AlmaLinux, or you want the distribution's own Docker packages | The [Set up section of `docker_compose_project`](../reference/modules/docker_compose_project.md#set-up) says what differs. Debian 13's own `docker.io` and `docker-compose` packages work. |
| An account on the server should run `docker` commands | Add the account to the group `docker` with a `user` task and `supplementary_groups: [docker]`. Anyone in the group `docker` has root on the server, so add only administrators. |
| A configuration file that a container mounts changed, and the application still uses the old file | Compose does not check a mounted file's content. [A changed file that is mounted into a container](../reference/modules/docker_compose_project.md#a-changed-file-that-is-mounted-into-a-container) shows how to reload the application. |
| Old images fill the disk | `docker_image` removes no image. A [`docker_unused_images`](../reference/modules/docker_unused_images.md) node task removes the images that no container uses, and [Clean up unused images on a timer](../reference/modules/docker_image.md#clean-up-unused-images-on-a-timer) runs `docker image prune` on a schedule. |
| You want to stop the containers for a backup, or remove a project | `state: stopped`, `containers_removed_named_volumes_kept`, and `containers_and_named_volumes_removed` do that. [What each state does](../reference/modules/docker_compose_project.md#what-each-state-does) says which data stays. |
| Another message from `docker_compose_project` or `docker_image` | Each module page has a table of its messages: [`docker_compose_project`](../reference/modules/docker_compose_project.md#errors-and-what-they-mean), [`docker_image`](../reference/modules/docker_image.md#errors-and-what-they-mean). |

## Where to go next

- Every input, result, and message of the modules on this page:
  [`docker_compose_project`](../reference/modules/docker_compose_project.md),
  [`docker_image`](../reference/modules/docker_image.md), and the
  [`wait_for` forms for containers](../reference/modules/docker_compose_project.md#wait-until-a-container-is-healthy).
- Whole example projects:
  [docker-host](../../examples/docker-host/README.md) installs Docker with
  an image cleanup timer, and
  [deploy-with-docker-compose](../../examples/deploy-with-docker-compose/README.md)
  deploys a Compose project with a mounted configuration file, a backup,
  and removal.
- Node task blocks in full, with checks before a restart:
  [Change a configuration and reload its service](change-a-configuration-and-reload-its-service.md).
- A database for the shop: [Manage a database](manage-a-database.md).
