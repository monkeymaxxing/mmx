# MMX: container, database, and certificate modules

`docs/changing-mmx/design/modules-applications.md` owns the shared design for three
groups of core modules:

- Modules that run containers with Docker or Podman.
- Modules that create accounts and databases inside PostgreSQL, MySQL, or
  MariaDB servers.
- The module that gets a TLS certificate with certbot.

Each module's own behavior, examples, and reasons are on the module's page.
The [module index](../../reference/modules/README.md) lists every page.
[Modules](modules.md) owns the contract every module shares (inspection,
dryrun, results, retries). [Execution and recovery](execution.md) owns node
task blocks and the records that make a restart happen after an interrupted
run.

The container, database, and certificate modules run on the node and drive
the tool already installed on the node: the `docker` command, the `podman`
command, `psql`, `mysql`, and `certbot`. MMX links no Docker, database, or
ACME library. ACME is the protocol certbot uses to request certificates from
a certificate authority.

Each module is named for the tool the module uses. No module hides two
tools behind one name. Docker projects and Podman containers use different
modules because Docker and Podman work differently on the server.

| Module | Manages |
|---|---|
| [`docker_compose_project`](../../reference/modules/docker_compose_project.md) | The containers of one Docker Compose project: running, stopped, or removed. |
| [`docker_image`](../../reference/modules/docker_image.md) | One Docker image on the node, pulled from a registry. |
| [`podman_quadlet`](../../reference/modules/podman_quadlet.md) | One Quadlet file, which Podman turns into a systemd unit. |
| [`podman_image`](../../reference/modules/podman_image.md) | One Podman image on the node, pulled from a registry. |
| [`docker_unused_images`](../../reference/modules/docker_unused_images.md), [`podman_unused_images`](../../reference/modules/podman_unused_images.md) | The images that no container uses, removed as `docker image prune` and `podman image prune` remove them. |
| `wait_for` (three more forms) | Wait until a Docker container, a Docker Compose project, or a Podman container reports healthy. |
| [`postgresql_role`](../../reference/modules/postgresql_role.md) | One role (account) inside a PostgreSQL server. |
| [`postgresql_database`](../../reference/modules/postgresql_database.md) | One database inside a PostgreSQL server. |
| [`mysql_user`](../../reference/modules/mysql_user.md) | One account inside a MySQL or MariaDB server, with its privileges. |
| [`mysql_database`](../../reference/modules/mysql_database.md) | One database inside a MySQL or MariaDB server. |
| [`certbot_certificate`](../../reference/modules/certbot_certificate.md) | One TLS certificate that certbot gets and renews. |

Words used below:

- An **image** is the packaged filesystem a container starts from. A
  **registry** is the server that images are pulled from. An image reference
  such as `registry.example.com/shop/web:2.4` names the registry, the image,
  and a **tag**. A tag can be moved to a newer image by whoever publishes it;
  a **digest** (`@sha256:...`) names exactly one image forever.
- A **bind mount** makes a file or directory of the node visible inside a
  container. A **named volume** is a data directory that Docker or Podman
  creates and keeps for containers.
- A container's **health check** is a command, declared with the container,
  that the container runtime runs repeatedly to learn whether the application
  inside works.

## Containers with Docker

MMX supports the two ways people run Docker containers on a server:

1. A **Compose project**: a file `compose.yaml` that declares several
   containers ("services"), their images, ports, mounts, and networks. The
   `docker compose` command creates and updates the containers from
   `compose.yaml`. `docker_compose_project` manages the Compose project.
2. A **systemd unit that runs `docker run`**: written with `systemd_unit` and
   `systemd_service` like any other service. `docker_image` supplies the
   image.

There is no module with one task for each container. One container is a
Compose project with one service. The Compose file can be inline `content`,
so the container still takes one visible node task block.

Install Docker and the Compose plugin first with package tasks, as the
[`docker_compose_project` page](../../reference/modules/docker_compose_project.md#set-up)
shows; it also gives the lowest Compose version that MMX supports.

### Newer images under the same tag

Most projects name an image by a tag that its publisher moves, such as
`latest` or `17`. Whether a rerun deploys the newer image is a required
choice on the task, `pull_images` on `docker_compose_project` and
`pull_image` on `docker_image`:

- `when_missing` pulls an image only when the node does not have it. A rerun
  never replaces a running container because a tag moved. Use `when_missing`
  with tags that name one version and with digests.
- `always` asks the registry on every run which image each reference names
  now, pulls images that differ from the node's images, and recreates the
  affected containers. The task reports changed and lists each image with
  its previous and new ID.

With `always`, an unchanged project and an unchanged registry give an
unchanged rerun. A moved tag is a change outside the project, like a moved
Git branch, and `always` on the task is where the reader sees that the task
follows it.

A dryrun with `always` asks the registry which image each reference names,
without downloading the image, and reports `would_change` with both image
IDs: the previous ID and the new ID.
When the registry cannot be reached, the dryrun reports `not_checked` with
the error and predicts no restart of a service that follows an image the
node has. A dryrun with `when_missing` reports `would_change` only for a
missing image.

The IDs are the image IDs that docker reports on the node. Docker's
containerd image store, which a new installation of Docker 29 uses, names an
image by the digest the registry serves it under, so there a dryrun knows the
new ID. The classic image store names it by the digest of the image's
configuration, which docker learns with the pull: there a dryrun reports the
previous ID, `image_id` is null, and the `reason` names the digest under
which the registry now serves the reference.

A reference may name a list of images, with an image for each platform.
The registry's answer then names the list. The list changes when the image
for any platform changes. The registry's answer omits the digest of the
node's platform image, and no daemon request gives the platform digest
without a pull.

When the registry serves another list than the node's image came from, the
dryrun claims only what the dryrun knows. With the classic image store, the
image ID is the platform image's ID, which may still be unchanged.
So `docker_image` reports `not_checked` and predicts no restart of a service
that follows the image. The Compose project's image entry has a null
`image_id` and a `reason`.

With the containerd image store, the image ID is the list's digest.
Pulling a changed list changes the image ID, so the dryrun reports the new
list digest as the image ID.

With the classic image store and the containerd image store, each Compose
service that uses the changed list reports `action: not_checked`.
Compose 2.40 recreates the container when the image ID changed.
Compose 5 recreates the container only when the platform's image changed.
The `docker_compose_project` task reports `would_change` when another part
of the task is certain to change, and `not_checked` otherwise.

MMX distinguishes an image list from an individual image by the media type
of the registry's answer. For an individual image, a different digest means
a different image.

For a reference containing a digest, the dryrun skips the registry request
to find the reference's current digest. With the containerd image store,
the reference's digest is the image ID. With the containerd image store,
the dryrun reports the reference's digest for an image missing from the
node.

To find a reference's current registry digest, a dryrun asks the Docker
daemon on the daemon's Unix socket
(`GET /distribution/<reference>/json`, the request `docker pull` starts
with), with the task's own login. The daemon then asks the registry with its
own settings: which registries it trusts without TLS, their certificates, and
its proxy. No `docker` command queries the registry digest using the
daemon's registry TLS, certificate, and proxy settings.
`docker manifest inspect` and `docker buildx
imagetools` talk to the registry themselves. A run skips the preliminary
registry-digest request:
`docker pull`, or `docker compose up --pull always`, pulls what differs, and
MMX compares the image IDs before and after.

Logins for private registries come from `registry_logins`, or from
`registry_username` and `registry_password` on `docker_image`. During a run,
MMX writes the logins into the node task's private directory under
`/run/mmx/private_task_directories/`. Only the individual task uses the
directory, and only root can open the directory. MMX removes the individual
task directory when the task ends, including failure or cancellation.

The [execution design](execution.md#a-node-tasks-private-directory-lives-in-one-place-that-the-node-empties)
owns the rules for node task private directories under
`/run/mmx/private_task_directories/`, including directories left by killed
node job worker processes. A dryrun writes no login file and pulls no
image. The dryrun sends the task's login in the request to the Docker
daemon. The dryrun uses only the task's own login. For a private image
without a task login, the dryrun reports `not_checked`. The reason names
the login inputs needed to check the private image.

Nothing is written into an account's home directory, and no password appears
on a command line or in a result. MMX hands the registry login inputs only
to Docker, so a secret in a registry login input does not hide the task's
result. The result shows image IDs, each service's action, and the reason
for a failure, such as the registry's refusal.

MMX replaces each secret with `[redacted]` in every form in which Docker
holds the secret ([secret rules](configuration.md#delivery-and-output)).
A login that must stay on the server for
people who pull by hand is a `shell` task that runs `docker login
--password-stdin` with `run_as_user_account`.

## Containers with Podman

Podman runs containers without a daemon. Podman's declared form is the
**Quadlet file**: a small file in systemd's unit syntax that describes a
container (or a volume, network, or pod). At each systemd reload, Podman's
generator turns every Quadlet file into a systemd service. The container
then is an ordinary systemd service: systemd starts it at boot, restarts it,
and stops it.

So Podman needs less from MMX than Docker does. `podman_quadlet` installs
the source Quadlet file. The existing `systemd_service` task starts the
generated service and restarts the service after tracked changes, with the
same records as for any service.
`podman_image` supplies the image.

The [`podman_quadlet` page](../../reference/modules/podman_quadlet.md#set-up) says which
Podman version Quadlet needs and how to install it.

## Unused images

`docker_image` and `podman_image` pull images and remove none, so old
images stay until something removes them.
[`docker_unused_images`](../../reference/modules/docker_unused_images.md) and
[`podman_unused_images`](../../reference/modules/podman_unused_images.md) remove the
images that no container uses, running or stopped, for a cleanup that a
dryrun can show. Scheduled cleanup stays a timer that runs the tool's own
prune. The pages of `docker_image` and `podman_quadlet` show cleanup timers.

| Decision | Reason |
|---|---|
| `remove` is required: `dangling_images_only` (`docker image prune`, `podman image prune`) or `every_image_no_container_uses` (with `--all`). | `every_image_no_container_uses` also removes a previous version that a deploy keeps for a quick rollback. The `docker_unused_images` and `podman_unused_images` pages explain the rollback-image risk. |
| The tools' own selection decides. A dryrun lists images using the tool's read commands and prune filters (`images --filter dangling=true`, and images that no container uses). A run removes the images with prune itself. | MMX keeps no second rule for which image is unused, so the dryrun and the tool cannot disagree about an image that a stopped container uses. |
| `podman_unused_images` measures layer directories named by `podman image inspect`. A run reports the bytes of layers that no remaining image uses, and a dryrun predicts the same byte count. With a storage driver other than overlay, the reported bytes are `null`. `docker_unused_images` removes images through the daemon request `POST /images/prune`, as `docker image prune` does. After pruning, a run reads `docker image ls` to identify which listed images were removed and reports the daemon's `SpaceReclaimed`. A Docker dryrun reports reclaimed bytes as `null`. | Images share layers, so the sum of removed image sizes includes layers that stay on disk. Podman prints no bytes for a prune. The docker command prints rounded bytes as text, but the daemon returns an exact byte count as JSON. |
| `podman_unused_images` takes `in_image_store_of_user_account` as `podman_image` does, with `podman_image`'s account checks and lingering. | One meaning for one field: the account's own store, which Podman reaches only as the account, with the account's runtime directory. |

## Accounts and databases inside a database server

`postgresql_role`, `postgresql_database`, `mysql_user`, and `mysql_database`
create an application's account, database, and privileges inside an
already installed and running database server. Package tasks install the
server, `file` tasks write the server's configuration, and
`systemd_service` tasks start the server.

The modules run the database's own client program on the node, `psql` for
PostgreSQL and `mysql` (or `mariadb`) for MySQL and MariaDB. The client is
installed with the server; for a server on another machine, install the
client package first. A missing client fails the task with a message that
names the program, and a dryrun reports `not_checked`, as it does when the
server is not installed or not running.

Passwords never appear on a command line, in the process list, or in a
result. MMX sends SQL to the client through its standard input and gives a
login password through a file in the node task's private directory under
`/run/mmx/private_task_directories/`. After the task, MMX removes the
individual task directory, including the password file.

A secret in `password`, `login_password`, `login_role` (PostgreSQL), or
`login_user` (MySQL and MariaDB) does not hide the task's result. MMX hands
the password and login inputs only to the database client, as MMX hands
registry logins only to Docker ([secret rules](configuration.md#delivery-and-output)).
The result shows whether the object was created and which attributes
changed. When the task fails, the result also shows the server's message.
MMX replaces each secret with `[redacted]`. MMX also redacts the stored
password form MMX computes from a password and sends to the server.

A secret in any other field hides the result as for every task.

Each task reconciles one object and reads the server's catalog first, so an
unchanged rerun reports unchanged, and a rerun after an interrupted run
finishes what is missing. The results of `postgresql_role`,
`postgresql_database`, `mysql_user`, and `mysql_database` include:

- `.this_node_task.value.created`: true when the object was created.
- `.this_node_task.value.changed_attributes`: the names of attributes that
  differed.

A dryrun reads the catalog and reports the creation and changed attributes
without changing anything.

### How a task reaches the server

Without connection fields, a database node task uses the database server
on the node running the task. The task connects through the server's local
socket as the server's administrator:

- PostgreSQL: MMX runs `psql` as the operating system account `postgres`,
  which the server accepts as the role `postgres` without a password.
- MySQL and MariaDB: MMX runs the client as root, which the server accepts
  as `root@localhost` through the socket.

For a server set up differently or running on another machine,
`postgresql_role`, `postgresql_database`, `mysql_user`, and `mysql_database`
accept the same connection fields:

| Field | Meaning |
|---|---|
| `database_server_host` | The server's host name or address. Without `database_server_host`, MMX uses the local socket. |
| `database_server_port` | The server's port, when it is not the standard one (5432 for PostgreSQL, 3306 for MySQL and MariaDB). Also selects among several PostgreSQL servers on one node when the local socket is used. |
| `login_role` (PostgreSQL), `login_user` (MySQL, MariaDB) | The database account MMX logs in as. Default: `postgres`, or `root`. |
| `login_password` | The password for the database account named by `login_role` or `login_user`, supplied as a private value. Required with `database_server_host`. Without `database_server_host`, the task logs in with the password through the local socket. |
| `database_server_certificate_authority_file` | Only with `database_server_host`: the path, on the node, of the certificate of the authority that signed the server's certificate. The client accepts the server only when the authority in `database_server_certificate_authority_file` signed its certificate and the certificate names `database_server_host`. For `127.0.0.1`, MariaDB's client (measured on 11.8) checks the authority but not the address; such a login never leaves the node. Without `database_server_certificate_authority_file`, verification remains enabled using the client requirements linked below. |
| `dangerously_skip_server_certificate_check` | With `database_server_host`: `true` turns verification off. Combining `database_server_certificate_authority_file` with `dangerously_skip_server_certificate_check: true` is refused. SQL and login information can reach an impersonated server. The client can also connect without encryption when TLS is unavailable. |

The login account must be allowed to create roles and databases. For
`postgresql_role` with a `password`, the PostgreSQL login role must be a
superuser, because only a superuser can read the stored form of a password
to compare it.

From PostgreSQL 16, a login role without superuser rights has additional
requirements. For `postgresql_role`, the login role needs the `ADMIN` option
on each role in `member_of_roles`. For `postgresql_database`, the login role
must be a member of the role named by the task's `owner` field.
Otherwise, the task fails with the server's message.

For PostgreSQL, the rules below define the client's operating system
account and how a task checks whether the database server answers:

- Without `database_server_host`, `psql` runs as the operating system
  account `postgres`. With `database_server_host`, `psql` runs as root:
  the client package alone creates no account `postgres`.
- `psql` verifies the server certificate and host name with `verify-full`.
  The PostgreSQL [module pages](../../reference/modules/postgresql_database.md#server-certificate-verification)
  state the CA-file requirements for supported client versions.
- `psql` never asks for a password (`--no-password`). Without `--no-password`
  and without a terminal, `psql` reads a password from `psql`'s standard
  input, where MMX sends the SQL.
- `psql` ends with the same exit code for a server that does not run, a
  refused login, and a missing database, and its message changed between
  versions. So a PostgreSQL task first asks `pg_isready`, which is in the
  same package as `psql` on every supported distribution. A server that
  does not answer fails the task in a run; a dryrun reports `not_checked`,
  because an earlier node task may start the server. A refused login fails
  the task in a run and in a dryrun, with the server's message, and is not
  tried again: another try cannot make a wrong password right.
- The server's messages can be in another language. A PostgreSQL task
  decides only from the server's catalog and the exit codes of `psql` and
  `pg_isready`, never from message words. The task reads the catalog before
  and after the task's SQL statements. The PostgreSQL task passes on the
  server's original message with the server's five-character error code
  (SQLSTATE).
- For everything that is not inside one database (roles, and the list of
  databases), a task connects to the database `postgres`, which every
  server has unless somebody dropped it.
- A connection to `database_server_host` that is not made within 10 seconds
  fails. Without the 10-second connection limit, each connection attempt to
  a wrong address waits for a timeout lasting minutes.
- A PostgreSQL server whose socket is not in the client's own directory
  (`/var/run/postgresql`) and that accepts no password login is not reached
  by the connection fields in the table above. No supported distribution
  combines a socket outside `/var/run/postgresql` with no password login.

For MySQL and MariaDB:

- MMX calls the client `mariadb` when it is on the node and `mysql`
  otherwise. A newer MariaDB warns when its client is called `mysql`.
- The client reads only MMX's own login file. The client reads no option
  file or environment variables from the Linux account running the client.
  A plain call as root would follow
  `/root/.my.cnf`, the files in `/etc/my.cnf.d`, MySQL's `~/.mylogin.cnf`,
  and variables such as `MYSQL_HOST` to another server or another account
  than the task names. MMX takes only the socket path from the server's
  configuration through `my_print_defaults`. Without a configured socket
  path, the client's built-in socket path holds.
- The client takes input as SQL only (`--binary-mode`). Without
  `--binary-mode`, an input line starting with `\!` runs a command on the
  node.
- MMX finds the product from the server's `VERSION()`, which holds the word
  `MariaDB` on every MariaDB. MySQL and MariaDB keep and set a stored
  password differently, also where their version numbers are close.
- A server that does not answer (the client's error 2002 or 2003) fails the
  task in a run; a dryrun reports `not_checked`. A refused login (error
  1045 or 1698) and a server whose certificate the client does not accept
  (error 2026) fail the task in a run and in a dryrun, and are not tried
  again: another try cannot fix them, and a server can lock an account
  after repeated wrong logins. MMX reads the number of the error, never its
  words.
- A connection to `database_server_host` that is not made within 10 seconds
  fails.
- MMX requests certificate verification explicitly with MySQL's
  `ssl-mode=VERIFY_IDENTITY` or MariaDB's `ssl-verify-server-cert`.
  The [module pages](../../reference/modules/mysql_database.md#server-certificate-verification)
  state which client needs a CA file and when MariaDB can verify using the password.

Module pages that rely on this section:
[`mysql_database`](../../reference/modules/mysql_database.md),
[`mysql_user`](../../reference/modules/mysql_user.md),
[`postgresql_database`](../../reference/modules/postgresql_database.md),
[`postgresql_role`](../../reference/modules/postgresql_role.md).

## Decisions

The decisions below are settled. Revisit a decision only when a concrete
workflow fails because of the decision.

| Decision | Reason |
|---|---|
| Every Compose state requires `compose_project_name`. `compose_files` accepts relative paths within `compose_project_dir`, the Compose directory on the node. | Container and volume ownership must be explicit, while ordinary subdirectories need no prohibition. Paths that leave `compose_project_dir` are refused. See [required settings](modules.md#a-setting-with-no-safe-guess-is-required). |
| `podman_quadlet` removal is `file_and_service_removed_named_volumes_kept`. | Removal stops the generated systemd service and removes the source Quadlet file. The removal state names both service stopping and Quadlet file removal, and says that named-volume data stays. |
| A present PostgreSQL role requires `can_log_in`, and a present PostgreSQL database requires `owner`. | Neither login permission nor application ownership has a safe guess. See [required settings](modules.md#a-setting-with-no-safe-guess-is-required). |
| `member_of_roles_not_listed: remove` removes every membership record for roles absent from `member_of_roles`, including administration-only records. | A list that owns a set must not keep hidden role grants outside that set. |
| `mysql_user` uses literal `grant_on` database names and explicit `grant_on_databases_matching` patterns. | An underscore must not silently broaden privileges. The plain pattern input states the broader grant; `grant_on` avoids YAML 1.1's boolean word `on`. |
| `mysql_user`'s `allow_creating_account_without_password` permits creation only. | `allow_creating_account_without_password` says when the permission applies and never changes an existing account's password. See [creation names](modules.md#a-setting-used-only-at-creation-says-so-in-its-name). |
| `certbot_certificate` requires `certificate_authority_url` and names the registration email `account_email_when_registered`. | The [required-setting rule](modules.md#a-setting-with-no-safe-guess-is-required) makes contact with a public authority visible. The [creation-only naming rule](modules.md#a-setting-used-only-at-creation-says-so-in-its-name) explains why changing the email does not update the existing certificate-authority account. |
| Omitted certbot renewal hooks, DNS credentials paths, and DNS waits stay saved. `renewal_settings_to_remove` explicitly deletes the hooks, paths, and waits. | A partial task must not delete renewal settings, following [explicit deletion](modules.md#leaving-a-setting-out-never-deletes). |
| A secret in `password`, `login_password`, `login_role`, or `login_user` of the four database modules does not hide the task's result. MMX replaces the secret, and the stored form MMX computes from a password, with `[redacted]`. | A role's password is nearly always a secret. A hidden result would hide whether the role was created, what changed, and the server's message on a failure, on every such task. |
| A Quadlet file describes a Podman container. `podman_quadlet` installs the Quadlet file, and `systemd_service` starts the generated service. No module has one task for each Podman container. On Debian 12, whose Podman 4.3 has no Quadlet, a unit file written with `systemd_unit` runs `podman run`. | Quadlet is Podman's own declared form, and Quadlet makes a container an ordinary systemd service. Start at boot, a restart after a tracked change, and the records behind that restart come from mechanisms MMX already has. A module for each container would repeat the service mechanisms. |
| Removal states say whether data stays, such as `deleted_with_all_its_data` of the database modules and the removal states of the container modules. | A reader must see from the task's `state` line whether removal deletes data. |
| The database modules run `psql` and `mysql`; MMX links no database driver. | The node runner executable stays small, and the client is already on a database server. |
| Remote database connections check the server certificate by default. A CA file keeps verification, but `dangerously_skip_server_certificate_check: true` switches verification off. Combining `database_server_certificate_authority_file` with `dangerously_skip_server_certificate_check: true` is refused because the settings request different certificate checks. | SQL and login information must reach the intended server. A private CA is a legitimate choice; disabling identity verification removes an attacker check, following [protective choices](modules.md#protective-choices-come-in-three-levels). |
| The MySQL and MariaDB client reads no option file or environment variables from the Linux account running the client. The client takes input as SQL only. | A file such as `/root/.my.cnf` or a variable such as `MYSQL_HOST` would send the task to another server or account than it names, and a line of the input could run a command on the node. |
| A refused login and a certificate that the client does not accept are not tried again by the database modules. | Retrying cannot correct a refused login or an unacceptable server certificate, and MySQL can lock an account after repeated wrong logins. A server that does not answer is tried again, because a starting server answers a moment later. |
| A PostgreSQL task asks `pg_isready` before `psql`. | `psql` gives one exit code for a stopped server, a refused login, and a missing database, and the words of its message changed between versions. |
| `postgresql_role`, `postgresql_database`, `mysql_user`, and `mysql_database` are core modules. | Almost every application server needs an account and a database. The `shell` module has no built-in password comparison. |
| Incus instances and profiles, libvirt virtual machines, RabbitMQ, and MongoDB are [official extra modules](modules.md#official-extra-modules), not core modules. | Incus, libvirt, RabbitMQ, and MongoDB are less common on the servers MMX targets. |
| A Firecracker virtual machine gets no module: `file` and `systemd_unit` set it up. | The existing modules already do the whole job. |
| A dryrun asks the Docker daemon, not the `docker` command, which image a reference names in its registry. | The daemon follows its own settings for registries, as a pull does. The commands that can ask, `docker manifest inspect` and `docker buildx imagetools`, talk to the registry themselves, and the second is a plugin that Debian 12's own packages lack. |
| When a registry serves another list of images for a reference (one image for each platform) than the node's image came from, a dryrun reports `not_checked` for what the dryrun cannot see. The unchecked parts are the Compose services that use the image and, with the classic image store, the image itself and a `docker_image` task. With the classic image store, the `docker_image` task then predicts no restart. | The list changes when any platform's image changes, and the registry's answer does not say whether the node's platform image changed. Docker's official images use platform lists, and the platforms are rebuilt at different times. With the classic image store a pull then changes nothing. With the containerd image store the image ID changes, Compose 2.40 recreates the container, and Compose 5 keeps the container. A dryrun must not claim a recreation or a pull that changes the image unless the dryrun knows. |

## Beyond the first release

- Docker's rootless mode (a Docker daemon of an ordinary account).
- Docker Swarm and Kubernetes.
- A health wait for a whole Podman pod.
- Podman older than 4.4, which has no Quadlet.
- Quadlet's `.artifact` files, which Podman 5.7 added.
- Database servers that hide stored password hashes from their
  administrator, as some managed database services do.
- MySQL and MariaDB authentication methods other than
  `mysql_native_password` and `caching_sha2_password`.
- Schemas, publications, replication control, and server variables inside a
  database server.
- ACME clients other than certbot, such as acme.sh and lego.
- Revoking a certificate at the certificate authority.
