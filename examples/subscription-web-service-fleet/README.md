# A subscription web service fleet

This example configures and deploys the servers of a whole web service with
MMX: two load balancers, a data server, two application servers, and a
worker server. MMX installs the packages, writes the configuration of every
service, deploys the applications from Git, and switches maintenance pages
on and off. The controller machine, where you run `mmx`, can be a laptop;
the nodes are the six servers that MMX manages.

The service is fictional. A Ruby on Rails application, the **main
application**, has web pages at `www.example.com` and an API at
`api.example.com`. Two small **brand sites**, `gardening.example.net` and
`cooking.example.net`, use separate deployments of a shared Rails
application. Each brand site is deployed from its own repository. The data
lives in MySQL, two Redis instances, memcached, Solr, and ClickHouse.

## The fleet

| Node | Address | What the node runs |
|---|---|---|
| `lb1`, `lb2` | 192.0.2.11, 192.0.2.12 | nginx as the load balancer, and keepalived, which holds the virtual address 192.0.2.10 on `lb1` or `lb2` |
| `db1` | 192.0.2.21 | MySQL, the two Redis instances, memcached, Solr, and ClickHouse |
| `app1`, `app2` | 192.0.2.31, 192.0.2.32 | nginx with Passenger, which runs the main application's web pages (port 8081) and API (port 8080), and the brand sites (ports 8092 and 8093) |
| `worker1` | 192.0.2.41 | The main application's background workers and its cron jobs |

The playbooks never name a node. Each fleet stage, one step of a playbook
that runs on a set of nodes, selects inventory groups:

| Inventory group | What nodes in the group run | Production nodes |
|---|---|---|
| `nginx_load_balancers` | nginx as the load balancer, keepalived | `lb1`, `lb2` |
| `mysql_servers` | MySQL and its daily backup | `db1` |
| `redis_and_memcached_servers` | Redis and memcached | `db1` |
| `solr_servers` | Solr and its nightly backup | `db1` |
| `clickhouse_servers` | ClickHouse and its hourly copy from MySQL | `db1` |
| `main_application_servers` | The main application's web pages and API | `app1`, `app2` |
| `brand_site_servers` | The brand sites | `app1`, `app2` |
| `main_application_worker_servers` | The main application's workers | `worker1` |
| `main_application_cron_job_servers` | The main application's cron jobs; exactly one node, because each job must run on one server only | `worker1` |

Each part of the work is a role: a list of node tasks with declared inputs,
in `roles/<role>/main.yml`, with its files in `roles/<role>/files/`. A
role's header comment says what the role does and why.

## The two inventories

- [`inventories/production.yml`](inventories/production.yml) lists the six
  servers. Its addresses are documentation addresses, and its Git
  repositories are placeholders. A real project writes its own, and adds the
  credentials that its Git host requires to the `git` task of the role
  `application_release`: an access token as `username` and `password` for an
  `https://` repository, or `ssh_private_key` and `ssh_known_hosts_lines` for one over SSH.
- [`inventories/test.yml`](inventories/test.yml) puts the same groups on two
  nodes for the guest suite: `test_load_balancer_and_data` holds the load
  balancer and every data service, and `test_application` holds the
  applications, the workers, and the cron jobs.

The test folds the groups onto two nodes, not six, for two reasons. The load
balancer and the application servers both run nginx and both own
`/etc/nginx/nginx.conf`, so they cannot share a machine. And a machine runs
one node job at a time: two inventory nodes on one machine in the same fleet
stage would collide.

`inventories/production.yml` and `inventories/test.yml` set the same
variables. The test values are smaller where a test machine is small, such
as memory sizes and process counts, and differ where a test must not reach
an outside service: the certificates are self-signed instead of Let's
Encrypt's, and the Git repositories are directories on the application
server.

`default_inventory` in `mmx_project_settings.yml` is `test`. A command on
the real servers must say `--inventory production`, so a forgotten option
reaches test machines only.

## The playbooks

### `site`: the whole server configuration

```sh
mmx dryrun site --inventory production
mmx run site --inventory production
```

[`playbooks/site.yml`](playbooks/site.yml) has one fleet stage for each part
that the team applies on its own, in this order: `base_server_setup`,
`nftables_firewall`, `deploy_account`, `data_server_swap`, `mysql_servers`,
`redis_and_memcached_servers`, `solr_servers`, `clickhouse_servers`,
`nginx_load_balancers`, `keepalived`, `ruby_and_nodejs`, `main_application`,
`nginx_with_passenger`, `main_application_nginx_servers`,
`brand_site_nginx_servers`, `main_application_workers`, and
`main_application_cron_jobs`. A run applies all of them. To apply some
parts, name their fleet stages:

```sh
mmx run site --inventory production --only-fleet-stages nginx_load_balancers,keepalived
mmx run site --inventory production --only-fleet-stages nftables_firewall
```

The fleet stages `nginx_load_balancers`, `main_application_nginx_servers`,
and `brand_site_nginx_servers` work on one node at a time
(`max_nodes_at_once: 1`), so a mistake that `nginx -t` cannot see reaches
one server first.

A restart of MySQL, of the Redis instances, or of Solr interrupts the
service. If a routine run would restart MySQL, Redis, or Solr, the run fails
with a message and leaves the configuration of the service requiring the
restart unchanged. Run again off-peak with the approval:

```sh
mmx run site --inventory production --only-fleet-stages mysql_servers --var mysql_restart_approved=true
mmx run site --inventory production --only-fleet-stages redis_and_memcached_servers --var redis_restart_approved=true
mmx run site --inventory production --only-fleet-stages solr_servers --var solr_restart_approved=true
```

### `deploy`: a release of the applications

```sh
mmx run deploy --inventory production --var release=v2
mmx run deploy --inventory production --var release=v2 --only-fleet-stages brand_sites
```

`release` is a Git tag or branch of each repository.
[`playbooks/deploy.yml`](playbooks/deploy.yml) deploys it in four fleet
stages:

1. `main_application`: the web pages and the API, on half of the
   application servers at a time, so the other half answers while the first
   restarts. Each server waits until its API answers `api v2 pong` and its
   web pages answer with the release.
2. `main_application_workers`: the same release on the worker server; the
   target `main-application-workers.target` restarts the workers.
3. `brand_sites`: each brand site from its own repository, half of the
   servers at a time.
4. `check_through_load_balancer`: each public name answers with the new
   release through the load balancer, over HTTPS.

A failure on one server stops the run (`on_node_failure: stop_run`), so the
other servers keep the release they run. Going back is another run with the
older release.

### `load_balancer_maintenance_pages`: maintenance pages on and off

```sh
mmx run load_balancer_maintenance_pages --inventory production --var maintenance_pages_shown=true
mmx run load_balancer_maintenance_pages --inventory production --var maintenance_pages_shown=false
```

While a page is shown, the load balancers answer every request to that site
with the maintenance page and the HTTP status 503. `maintenance_pages_shown`
has no default: MMX refuses a run that does not say which of the two it
wants. The playbook has two fleet stages, `brand_site_maintenance_pages` and
`main_application_maintenance_pages`, so `--only-fleet-stages` shows or hides
the pages of one part only. Each fleet stage checks the HTTP status of every
site it changed.

### `server_details`: addresses and disks for the team's notes

```sh
mmx run server_details --inventory production
```

Each server's network addresses and disks are saved on the controller
machine as `server_details/<node>.txt` in the project.

### `test_stand_in_applications`: test scaffolding

```sh
mmx run test_stand_in_applications
```

Run `test_stand_in_applications` once on the test machines, after `site`.
The playbook puts tiny Rack applications, from
[`files/test_stand_in_applications/`](files/test_stand_in_applications),
into bare Git repositories on the application server, with the tags `v1` and
`v2`, so that `deploy` clones, installs gems, and switches releases for
real. The playbook also creates the MySQL table `page_views` with three
rows, which the application's migrations would create in production, so that
ClickHouse's hourly copy has something to copy. In production the
repositories are on the Git host, and `test_stand_in_applications` never
runs.

## Secrets

The roles read five secrets: the recovery account's password hash, the
MySQL passwords of the main application and of ClickHouse's copy job, the
ClickHouse account `reporting`'s password, and the Cloudflare API token that
certbot's DNS check uses. Each secret key, the name of one secret, is bound
to an environment variable on the controller machine in
`mmx_project_settings.yml`:

```yaml
# mmx_project_settings.yml
secrets_from_env:
  recovery_account_password_hash: SUBSCRIPTION_WEB_SERVICE_RECOVERY_ACCOUNT_PASSWORD_HASH
  main_application_database_password: SUBSCRIPTION_WEB_SERVICE_MAIN_APPLICATION_DATABASE_PASSWORD
  clickhouse_copy_database_password: SUBSCRIPTION_WEB_SERVICE_CLICKHOUSE_COPY_DATABASE_PASSWORD
  clickhouse_reporting_password: SUBSCRIPTION_WEB_SERVICE_CLICKHOUSE_REPORTING_PASSWORD
  cloudflare_api_token: SUBSCRIPTION_WEB_SERVICE_CLOUDFLARE_API_TOKEN
```

The guest suite sets those five variables to test values. A production
project keeps its secrets in the encrypted vault instead (`mmx secrets edit`)
and removes the bindings; see
[Keep secrets](../../docs/how-to/keep-secrets.md#create-the-vault-and-use-a-secret).

## Parts that exist only for tests

The inventory `inventories/test.yml`, and every name that starts with
`test_`, exist only for the test:

- the test inventory's nodes `test_load_balancer_and_data` and
  `test_application`;
- the playbook `test_stand_in_applications` and its files in
  `files/test_stand_in_applications/`, which become the directories
  `/home/deploy/test_stand_in_application_sources/` and
  `/home/deploy/test_stand_in_git_repositories/` on the application server;
- the role `test_self_signed_certificates`, which the fleet stage
  `nginx_load_balancers` calls in place of `lets_encrypt_certificates` when
  `certificates_from_lets_encrypt` is false. It writes its certificates to
  `/etc/ssl/test-self-signed/<name>/`, in the same form as certbot's.

The administrators' SSH keys in
`roles/base_server_setup/files/public_keys/`, the deployment key in
`roles/deploy_account/files/deploy_public_keys/`, and the company's internal
certificate authority in `roles/base_server_setup/files/` are test keys and
a test certificate. A real project replaces the administrators' SSH keys,
deployment key, and internal certificate authority certificate with the
project's own keys and certificate.

## Why it works this way

### Settings left at their defaults

Every setting in a configuration file carries a one-line reason in a
comment. Everything else stays at the program's default, because a default
that its program's authors keep current beats a value copied from an old
guide.

- **Transparent huge pages** stay at Ubuntu's kernel default, `madvise`:
  only a program that asks for huge pages gets huge pages. Since Redis 6.2,
  Redis turns off transparent huge pages for itself (`disable-thp yes`).
  MySQL, Redis, Solr, and ClickHouse work with `madvise`, so no service turns
  off the kernel's transparent huge pages.
- **Kernel network settings** stay at the defaults. `net.core.somaxconn`,
  the queue of connections that wait for a program to accept them, is 4096
  since Linux 5.4, more than the old tuning guides asked for. Two kernel
  settings remain: `vm.swappiness` 1 on the data servers (fleet stage
  `data_server_swap`), and `net.ipv4.ip_nonlocal_bind` 1 on the load
  balancers, so nginx may bind the virtual address while keepalived holds it
  on the other load balancer.
- **Open-file limits** are set for each service that needs more than
  systemd's default, in its own unit: MySQL, nginx with Passenger, each
  Redis instance, and Solr. nginx on the load balancers raises its workers'
  limit itself (`worker_rlimit_nofile`). No limit applies to every program
  on a server.
- **MySQL** keeps `innodb_flush_log_at_trx_commit` at 1. The value 2 is
  faster, but loses up to a second of committed transactions when the server
  crashes. The character set is MySQL 8's default, `utf8mb4`.
- **Redis** sets only what differs for this fleet: the addresses it listens
  on, the memory limit and eviction policy of each instance, and the
  append-only log of the persisted instance.
- **memcached** keeps its default connection limit and thread count.
- **nginx** keeps its default timeouts and buffers, has no proxy cache, and
  sets no CORS headers: an application sets those where it needs them. The
  nginx package's daily log rotation stays.
- **Passenger** keeps its default start timeout and idle times.
- **ClickHouse** keeps its default connection and query limits, and rotates
  its own logs by size and count.
- **Solr** keeps the garbage collector settings of its start script.

### One nftables file instead of ufw

The firewall of each server is one nftables file, `/etc/fleet-firewall.nft`,
written from a template that reads the inventory groups: a node in
`nginx_load_balancers` admits HTTP, HTTPS, and keepalived's VRRP packets,
and every node admits SSH, MMX's HTTPS port 7443, mosh, and everything from
the company's own networks (`trusted_networks`). The data services, such as
MySQL and ClickHouse, are reachable only from those networks. Loading the file replaces the table
`fleet_firewall` in one step, so the server is never without rules, and the
tables of other programs stay as they are. The service `fleet-firewall`
loads the file at boot. The [`ufw` page](../../docs/reference/modules/ufw.md#one-nftables-file-instead)
recommends this form for a firewall that follows the inventory.

### One directory for each release

The role `application_release` checks out each release into its own
directory, `<application>/releases/<release>`, installs its gems there,
links the shared configuration files, and only then switches the link
`<application>/current` to the new release directory. The running release is
never changed in place. Passenger, or systemd for the workers, restarts the
application once for each new release, also when a run stopped between the
switch and the restart. The [release directories
example](../deploy-from-git-with-release-directories/README.md) shows the
pattern on its own.

### Restarts that need an approval

A restart of MySQL drops every connection of the applications, a restart of
Redis empties its cache instance, and a restart of Solr stops search until
it has loaded its core. A node task block's `checks_before_restart_or_reload`
refuses such a restart unless the command line approves it, so a routine
`site` run never interrupts the service by surprise. See
[Checks before a restart or reload](../../docs/reference/playbook-file.md#task_block-fields).

### Coming from Ansible

This example started as a real company's Ansible repository. The company's
Ansible configuration was converted to MMX and made anonymous. The example's
names, domains, addresses, and services are invented, and the parts that
only served the company's own products were left out. These Ansible habits
were replaced:

- **One playbook for each kind of server, and tags** became one `site`
  playbook with a fleet stage for each part, chosen with
  `--only-fleet-stages`.
- **`ignore_errors: true` on account creation**, which hid user ID clashes,
  became the `user` module, which fails with the reason.
- **`pause` prompts before restarts** became restart approvals on the command
  line, such as `--var redis_restart_approved=true`.
- **A `pause` that asked to deploy the workers first** became a condition:
  the workers' target starts only once a release exists (`path_exists`).
- **`lineinfile` edits** of `sshd_config` and `/etc/sudoers` became drop-in
  files that MMX owns whole. The edits of `limits.conf` and `system.conf`
  became each service's own limit in its unit, and the edit of
  `/etc/timezone` became the `time_zone` module.
- **Overwriting a unit file of a package**, the upgrade timer, became a
  drop-in.
- **`git reset --hard` and `git clean`** before each pull into one checkout
  became one directory for each release.
- **`stat` checks and marker files** that kept shell installers from running
  twice became packages and modules that check the server themselves.
- **`chown -R` on every run** of the Solr role became Solr's archive,
  unpacked and owned by root, with Solr writing only to its data, log, and
  backup directories.
- **A cron job that restarted a service** when it did not answer became
  systemd's `Restart=`.
- **The swap file by `fallocate`, `mkswap`, and `swapon`** became the
  `swap_file` module.
- **A firewall script that flushed every rule** and then added them again,
  which left a moment without rules, became the nftables file, which
  replaces its table in one step.
- **`apt_key` from a keyserver** became `apt_repository` with the key pinned
  by its SHA-256.
- **Per-host `when` blocks** such as `when: "'lb03' in inventory_hostname"`
  became inventory variables, such as `keepalived_priority`.
- **Private keys and passwords in the inventory**, and an encrypted secrets
  file in an application's repository, became MMX secrets.
- **`local_action: copy`** for the server details became
  `save_on_controller_machine`.

[Move from Ansible or Salt](../../docs/how-to/move-from-ansible-or-salt.md) maps the rest
of Ansible's vocabulary to MMX.

## Run the executable example

The guest suite
[`tests/integration/subscription_web_service_example.py`](../../tests/integration/subscription_web_service_example.py)
runs every playbook on two test VMs, `mmx-dev` (the node
`test_load_balancer_and_data`) and `mmx-peer` (the node `test_application`).
Each step runs a dryrun, a run, a second run that must change nothing, and a
dryrun that must find nothing to change: `site`,
`test_stand_in_applications`, `deploy` with `release=v1` and then `v2`,
`load_balancer_maintenance_pages` with `true` and then `false`, and
`server_details`. Then a check script on each node, as root, checks the
services, the releases, and the maintenance pages.

The suite is part of the catalog of guest suites that the
[testing guide](../../docs/changing-mmx/testing.md) describes. It runs on Ubuntu 24.04
only, because the roles use Ubuntu's packages, such as `mysql-server` and
`libmysqlclient-dev`, and Phusion's repository for Ubuntu's release. It needs
VMs, not containers: the example sets kernel settings, a swap file, and time
synchronisation, which belong to the guest's own kernel and clock.

```sh
MMX_TEST_ROOT=$HOME/.cache/mmx-incus python3 tests/integration/incus.py \
  --distribution ubuntu2404 --suite subscription_web_service_example.py
```

A run takes about 15 minutes. Package installation takes most of the run's
elapsed time. About 4 minutes go to the first dryrun's download of Solr's
archive from Apache's archive server, which sends the archive slowly. Later
commands reuse the saved bytes.
