# Validation

`docs/changing-mmx/validation.md` covers the current build only. Rerun the
[acceptance checks](acceptance.md#acceptance-checks) after a change and replace
the results in `docs/changing-mmx/validation.md` with the new results.

## Platform validation on the four target distributions

Validated on 2026-11-16 on the release candidate of MMX 0.1.0, with the
release archives that `--build-and-use-release-archive 0.1.0` builds and
checks. Every check below passed on the release candidate, except where the
table says what was not run and why. Fixes found by the runs were made
before the release, and the suites they touch ran again on every
distribution where those suites run.

| Check | Result |
| --- | --- |
| Whole catalog, AMD64 | Every suite passed on the nine tested distributions in Incus guests on two Linux test servers: the Debian family's whole catalog, and the base suite and each tool module's suite on the distributions its page lists for Rocky Linux 9, AlmaLinux 10 and Fedora 45 (virtual machines with SELinux enforcing), Arch Linux, and openSUSE Leap 16.1. The servers are Hetzner AX41 machines with 64 GB RAM and 12 hardware threads, running Debian 13 AMD64 with Incus 6.0 and KVM. The controller processes ran from `mmx-0.1.0-linux-amd64.tar.gz`. Suites that failed while the fuzzing and the stress run loaded the same servers passed when run again. |
| Whole catalog, ARM64 | Hetzner Cloud `cax11` servers (two Ampere cores and 4 GB RAM each), two per job slot and at most 12 at once, with the controller process on a Debian 13 AMD64 test server, on Debian 12 and 13, Ubuntu 24.04 and 26.04, Rocky Linux 9, and AlmaLinux 10. Every suite passed except two kinds that could not run there. `node_facts.py` resizes a server to a larger type, and Hetzner had no `cax21` or `cax31` free in any location during the runs; the suite passed on AMD64. `pg_auto_failover.py` and `pgbackrest.py` need test machines that share a subnet, and each cloud server has a public `/32` address; they passed in Incus on the four Debian-family distributions. |
| macOS controller check | On a MacBook Pro with an Apple M1 Max, with `mmx-0.1.0-macos-arm64.tar.gz`: `vm.py test`, `fleet.py`, `access.py`, `task_secrets.py`, and `dns_tls_and_account_lookups.py` passed against Hetzner Cloud servers with Debian 13 and Ubuntu 26.04. |
| Static analysis | `go vet`, `staticcheck`, `errcheck`, `exhaustive`, `nilaway`, and the dependency vulnerability check report nothing. |
| Fuzzing | Each of the nine fuzz targets for 10 minutes on a Linux test server: no failing input. |
| Go tests under stress | `scripts/run-go-tests-under-stress.sh`, 40 runs of every test package with the race detector, 4 at once, on a Linux test server: 2,014 tests, no failure, in 2 hours and 46 minutes while the fuzzing and the catalog shared the machine. |
| Node runner executable | AMD64, as `scripts/build.sh` builds it: 17,936,544 bytes |

The following table lists the test machines' operating systems and kernels.
A container guest runs on its test server's kernel, 6.12.113 or 6.12.115.

| Distribution | AMD64 VM guest | ARM64 Hetzner Cloud server |
| --- | --- | --- |
| Debian 12 | kernel 6.1.0-53 | kernel 6.1.0-52 |
| Debian 13 | kernel 6.12.111 | kernel 6.12.107 |
| Ubuntu 24.04 | 24.04.5, kernel 6.8.0-147 | 24.04.4, kernel 6.8.0-138 |
| Ubuntu 26.04 | 26.04.1, kernel 7.0.0-38 | 26.04.1, kernel 7.0.0-30 |
| Rocky Linux 9 | 9.8, kernel 5.14.0-687.66.1 | 9.8, kernel 5.14.0-687.42.1 |
| AlmaLinux 10 | 10.2, kernel 6.12.0-211.74.2 | 10.2, kernel 6.12.0-211.49.1 |
| Fedora 45 | kernel 7.3.3-300 | not tested |
| Arch Linux | kernel 7.3.4 | not tested |
| openSUSE Leap 16.1 | kernel 6.12.0-160100.8 | not tested |

The suites ran against the following versions in the AMD64 guests, as
measured in the whole catalog of 2026-10-14:

| Distribution | PostgreSQL | MariaDB, MySQL | certbot | Docker, Compose | Podman | RabbitMQ | libvirt, QEMU | ZFS |
| --- | --- | --- | --- | --- | --- | --- | --- | --- |
| Debian 12 | 15.19 | MariaDB 10.11.18 | 2.1 | 29.9.0, 5.6.0 | 4.3 | 3.10.8 | 9.0.0, 7.2.22 | not installed |
| Debian 13 | 17.11 | MariaDB 11.8.6 | 4.0 | 26.1.5, 2.26.1 | 5.4 | 4.0.5 | 11.3.0, 10.0.13 | 2.3.9 |
| Ubuntu 24.04 | 16.15 | MariaDB 10.11.14, MySQL 8.0.46 | 2.9 | 29.9.0, 5.6.0 | 4.9 | 3.12.1 | 10.0.0, 8.2.2 | 2.2.2 |
| Ubuntu 26.04 | 18.6 | MariaDB 11.8.6, MySQL 8.4.11 | 4.0 | 29.1.3, 2.40.3 | 5.7 | 4.0.5 | 12.0.0, 10.2.1 | 2.4.1 |

What the runs found, and what changed:

- **A zstd file that panicked the decoder** (GO-2026-6865). A file that
  `download` or `unpack_archive` fetched and decompressed could end the run
  on the node. The copy of `github.com/klauspost/compress` takes the
  upstream fix, and a test in `internal/assets` keeps the input.
- **A crash in Go's HTTP/1 server** (GO-2026-6859). The node HTTPS API reads
  request bodies with the affected code. MMX builds with Go 1.27.4.
- **WireGuard removal on Ubuntu 26.04.** Ubuntu 26.04's AppArmor profile
  for wg-quick lets it read files only in `/etc/wireguard`, so taking an
  interface down with the saved configuration from `/run/mmx` failed when
  the task removed the interface. The saved copy now goes into a directory
  of its own in `/etc/wireguard`
  ([network configuration modules](design/modules-network.md)), and
  `wireguard_interfaces.py` turns AppArmor on before its first case on
  Ubuntu 26.04.
- **A pg_auto_failover node outside the monitor's subnet.** The monitor
  lets nodes log in only from the subnet of its own address, and
  `pg_autoctl create postgres` retried the refused login without end, so
  the run waited with no message. `pg_auto_failover_node` now logs in to
  the monitor once before it creates the node and fails at once with the
  rule ([pg_auto_failover_node](../../official_extra_modules/pg_auto_failover_node/README.md)).
- **Test faults, none in MMX's behavior:** the extra modules' suites
  expected a dryrun's reason without the earlier node task that may provide
  a missing program; the harness installed test tools without refreshing
  empty apt package lists; and `blocked_port.py` checked port 7443 before
  the interface it had just brought up was ready on a busy server.
- **Packages that vendors do not build for ARM64.** MongoDB publishes its Debian packages
  only for amd64. The suite tests the module without the tool there.
- **MongoDB on Linux 6.19.0 through 7.0.13.** The MongoDB server refuses to
  start on these kernels, such as Ubuntu 26.04's kernel 7.0.0 on a cloud
  server; the suite tests the module without MongoDB there. The AMD64
  suite runs in a container on the test server's kernel.
- **Test fixtures that assumed an Incus guest.** Ubuntu's AppArmor profile
  of mysqld reads only `/etc/mysql` among the directories under `/etc`, so
  `mysql_databases.py` keeps its test certificates there. A Hetzner Cloud
  server has no `/dev/kvm`, so `incus_instances.py` makes its virtual
  machine instance only where the device exists. Applying a cloud image's
  pending security updates took nearly all of 28 minutes on Ubuntu 26.04,
  and `server_resources.py` gives that run an hour.

## Beta validation with real-world Ansible work

Validated on 2026-10-15. The question was whether MMX does the common work
that Ansible does in real projects. 301 scenarios from 79 Ansible sources
were matched against the suites and examples first; the
[coverage index](acceptance.md#real-world-ansible-work) lists what proves
each kind of work. Existing suites and examples proved 191 of the 301
scenarios. The remaining scenarios, and real projects as a whole, ran on
Incus guests on the Debian 13 AMD64 test servers, with two nodes per project.
The table below records each project's distributions. Each step ran as a
dryrun, a run, a second run
that had to change nothing, and a dryrun that had to find nothing to change;
then scripts checked the outcome on each node with the system's own tools.
The projects stay outside this repository, except the company's project,
which is
now the [subscription web service example](../../examples/subscription-web-service-fleet/README.md)
(below). The project runs did not cover all four target distributions or
ARM64; the whole catalog covers all four target distributions and ARM64.

| Project | Distributions | Result |
| --- | --- | --- |
| k3s-ansible: a K3s cluster of one server and one agent, the join token handed from server to agent, an upgrade one node at a time, a reset, and a rebuild | Debian 13, VM guests | Every step and check passed |
| Trellis: WordPress on nginx and PHP-FPM with MariaDB on the second node, Composer, three deploys with release directories, a rollback, fail2ban, ferm, and sshd hardening | Ubuntu 24.04, container guests | Every step and check passed |
| A company's Ansible repository: base setup, nftables, an nginx load balancer with maintenance mode, keepalived, Rails applications under Passenger deployed from Git, workers, cron jobs, MySQL, seven Redis instances, Memcached, Solr, ClickHouse, Monit, WireGuard | Ubuntu 24.04, container guests | Every step and check passed |
| OpenWISP: a Django application in a virtual environment with pip, PostgreSQL with PostGIS on the second node, migrations and collectstatic as commands that follow a change, uWSGI, Daphne, two Celery workers and beat as systemd units, nginx with TLS, Postfix, and two configuration changes that must reload or restart only what they touch | Debian 13, container guests | Every step and check passed |
| A pack of 13 scenarios that no suite proved: an NFS server and client, sshd hardening under a running MMX, kernel modules blocked and unloaded, drop-ins for packaged timers, a per-node secret, a certificate handed between nodes, a WireGuard mesh, NAT with an own nftables table, tmpfs and mount hardening, an upgrade restored by `on_failure`, a unit written before its program, a node behind a jump host, and templates from the inventory | Ubuntu 24.04, VM guests | Every step and check passed |

The beta-validation project runs found the following defects. Each defect
now has a correction and a test:

- A whole-value reference, such as `uid: "{{ .vars.user_id }}"`, was
  refused in a number, boolean, list, or mapping field, so a role's input
  could not reach most module fields; on the node, a number from the job's
  JSON was refused too.
- Two `download` tasks for two processor architectures, as the download
  page shows the tasks, were refused as one path written twice.
- `mmx validate` without a playbook name failed on a command-line variable
  without a default.
- `password_hash` refused a role input declared `secret: true`.
- An MMX reference inside a script or a cron command was written as
  literal text without any message from validation.
- A first dryrun on a new server failed when a check needed what an earlier
  node task would create, such as an account.
- A missing parent directory was tried three times with a raw error.
- An `http` node task ignored a `Host` header.
- `file` refused a link without naming the `symlink` module.
- A shell task whose `run_from_directory` did not exist yet, such as an
  application directory that an earlier node task creates, failed a first
  dryrun. During a run, MMX tried the shell task three times without a reason.
- Every run fetched each pinned download again on the controller machine:
  an unchanged run of the company's project took 277 s, all of it fetching
  Solr's archive. A run now reuses the bytes that an earlier run saved,
  after checking the saved bytes' SHA-256 again. The unchanged run of the
  company's project then took 8 s, and the project's dryrun took
  7 s instead of 97 s.

With corrections for the beta-validation defects, the whole catalog passed
on Debian 13.

The runs added `ufw`'s `logging_level`, which five of the sources set, and
documentation for what they proved: one nftables table of a project's own,
`on_failure` that restores a backup, a node behind a jump host, and sshd
changes on a node that MMX manages.

The validation identified these additional requirements, now implemented.
`directory`'s `create_missing_parent_directories` creates the missing
parents of a directory as `mkdir -p` does. `directory_resources.py` proves
missing-parent creation on VM guests, and `directory_copy.py`, for the copy
form, on container guests. The directory suites passed on Debian 13.
The fifth lock file module, `uv_project_dependencies`, installs what a
Python application's `uv.lock` lists. `language_installers.py` passed with
`uv_project_dependencies` on all four target distributions, in container
guests, with uv 0.13.0 from
its release archive: with the distribution's Python, then with Python 3.14.8
from `uv python install`, as the recipe in [Install languages](../how-to/install-languages.md#a-language-version-that-no-package-offers)
shows. The recipe's other examples pass `mmx validate` and did not run on a
guest.

### The subscription web service example

Validated on 2026-10-15. The company's project became the example
`examples/subscription-web-service-fleet`: made anonymous, and trimmed to
the parts that serve the example. Monit, the WireGuard tunnel, a
load balancer fleet stage that answered for servers outside the example, and
every tuning value without a reason were left out, and repeated parts were
reduced to two or three. The example's
suite `subscription_web_service_example.py` runs the example on two Ubuntu
24.04 VM
guests, so the kernel settings, the swap file, and time synchronization run
too; the company's project skipped them in containers.

Every step passed as a dryrun, a run, an unchanged second run, and a dryrun
that found nothing to change: `site`, `test_stand_in_applications`, `deploy`
with two releases, `load_balancer_maintenance_pages` with `true` and
`false`, and `server_details`. Then 141 checks on the two guests passed,
with the system's own tools.

The suite took 915 s. Within the suite's 915 s, the first dryrun took
261 s. For 250 s of the first dryrun, the controller machine downloaded
Solr's 66 MB archive from Apache's archive server at about 265 KB/s.
The run after the first dryrun reused the saved bytes. `site` took 568 s, nearly
all of it package installation.

The first run found that nginx with Passenger 6.2.0 does not start on
Ubuntu 24.04 when `nginx.conf` has no `error_log` line: Passenger's watchdog
hangs, and systemd stops nginx after 90 seconds. The example's file names
the log, with the reason.

A later run failed once with "Transport endpoint is not connected" from
`systemctl`: the guests' bases kept the stamps of their timers, so each copy
caught up on the daily apt upgrade within an hour of its boot, and
unattended-upgrade upgraded systemd, which re-executed itself, in the middle
of the suite. The base images now delete the timer stamps
([testing guide](testing.md#the-full-catalog-with-incus)). After the
base images were rebuilt, the whole catalog passed on all four target
distributions.
The example's suite took 1665 s in the whole-catalog run, alongside the
other suites.

A Go test, `TestEveryExampleProjectValidatesWithEachPlaybookAndEachInventory`
in `cmd/mmx`, validates every example project with each of its playbooks and
each of its inventories, so the example's production inventory, which no
guest runs, stays valid.

## Module pages

Validated on 2026-10-14. The module pages, the generator, and
`mmx describe-module` are checked by Go tests on the controller machine and
by one guest suite:

- `go test ./docs/reference/modules/...` proves on the current tree that every core
  module has a page source with the sections of the
  [documentation map](documentation-map.md#the-sections-of-a-module-page) in
  their order; that every YAML example of node tasks on a page passes MMX's
  validation; that the generated pages equal what `go generate
  ./docs/reference/modules` writes; that every relative link in the repository's
  Markdown files leads to an existing file and heading; that every shared
  section that a page links to lists that page; and that the node program
  `mmx-runner` does not link the page text while the controller program
  `mmx` does. Each check is also proved against a fixture containing the
  fault the
  check must catch.
- The Go tests of `internal/config` check that every core module has a known
  topic, and those of `cmd/mmx` check the whole page, `--reference-only`,
  and the JSON field `page` for each kind of module.
- `tests/integration/diagnostics.py` passes in containers on Debian 12 and
  13, Ubuntu 24.04: `describe-module`
  lists every module, every module's example validates without a node,
  `--reference-only` prints
  the reference, and the whole page starts with the module's title and holds
  `## Inputs` and `## Examples`, as does the JSON field `page`.

Not verified on a guest: the pager. The pager starts only when standard
output is a terminal, which no suite has. The pager was checked by hand in
a pseudo-terminal on macOS, with `$PAGER` set and without `$PAGER`.

## ifupdown interfaces file module

Validated on 2026-10-15. `tests/integration/ifupdown_interfaces_files.py`
uses the production binaries, a second network card that is connected to
nothing, and the Incus agent as an observation path that does not use the
guest's network. The suite's setup switches each Debian test guest from
systemd-networkd to ifupdown, as Debian's installer and some server
providers set nodes up: the main card's settings in
`/etc/network/interfaces` itself, beside `source /etc/network/interfaces.d/*`.
All nine cases pass on the Debian 12 and Debian 13 guests:

| Guest | ifupdown | Run time |
|---|---|---|
| Debian 12 | 0.8.41 | 114 s |
| Debian 13 | 0.8.44+deb13u1 | 113 s |

The cases prove:

- the second card's file in `/etc/network/interfaces.d`: dryrun, a new
  file, an unchanged rerun with no activation, and a changed address, after
  which only the new address and its route remain;
- a VLAN in its own file with a route in an `up` line, then a change of its
  parent card's file, after which the VLAN still has its address and its
  route; before the fix, the same case showed the route gone;
- the VLAN's file removed with `state: absent`, after which the device is
  gone, and the removal of the second card's file;
- a route added to the main card's stanza in `/etc/network/interfaces`,
  confirmed over new SSH and HTTPS connections, with a restore record that
  holds interface names and classes only;
- a main file that cuts the main card off: new SSH connections fail, and
  the node restores the file's bytes, owner, mode, and modification time,
  the main card's address, and the main card's default route without help;
- an added file whose `up` command fails: the node removes the file and
  the address that the failed `ifup` left, at once;
- the refusals before any change, in dryrun and in a run: a misspelled
  option, a main file without its `source` line, and an
  `/etc/network/interfaces.d` file that the main file does not read;
- a restart of the node during the confirmation wait of a cut-off change:
  the restore runs on the new boot, after `networking.service`, and puts the
  main file and address back.

Adding `ifupdown_interfaces_file` also changed the shared owed restore code.
The shared code now saves a proposed-file copy for an activation that asks
for a copy. `owed_restores.py` and
`systemd_networkd_files.py` pass on Debian 13. `go build ./...`, `go vet ./...`, and
`go test ./...` pass on the development machine.

Not verified:

- The whole catalog did not run for the `ifupdown_interfaces_file` addition.
  The changed shared code runs only in the suites listed above.
- Bridges, bonds, and DHCP on a guest. Measured by hand on Debian 12 and
  13: `ifdown` with the file that declared a bridge deleted it, and the
  main card changed from a static address to DHCP and back.
- ifupdown2, which some virtualization systems use: `ifupdown_interfaces_file`
  refuses ifupdown2.

Module pages that rely on this section:
[`ifupdown_interfaces_file`](../reference/modules/ifupdown_interfaces_file.md).

## Firewalld zone modules

Validated on 2026-11-14. `tests/integration/firewalld_zone_memberships.py`
uses the production binaries, firewalld from each distribution's own
package, a second network card that is connected to nothing, and the Incus
agent as an observation path that does not use the guest's network. All
twelve cases pass on both of its guests, where NetworkManager manages the
network and SELinux is enforcing:

| Guest | firewalld | Run time |
|---|---|---|
| Rocky Linux 9 | 1.3.4 | 277 s |
| AlmaLinux 10 | 2.4.3 | 308 s |

The cases prove, on each guest:

- `firewalld_zone`: dryrun, creation in the stored configuration and in
  the running firewall, the reload (a port that another zone had at
  runtime only is gone), an unchanged rerun, the refusal to remove a zone
  that has a member, and removal;
- source networks: dryrun, present, an unchanged rerun without a record or
  a wait, a move from another zone, the removal of members that are not
  listed, and absent, each read back from both layers;
- a list that the task omits stays unchanged;
- a card that NetworkManager does not control: present, moved, kept across
  a firewalld reload, and absent;
- the refusal of a zone that does not exist;
- the refusal of a card that an activated profile controls, in dryrun and
  in a real run, also for removal through the members that are not listed:
  the text names the profile's file and the `zone=` line, and the file's
  bytes do not change;
- the cut-off: the controller machine's own address goes into zone `drop`;
  new SSH and HTTPS connections fail while the old SSH session still
  answers, and the node puts the address back where it was in both layers,
  once from no zone and once from zone `trusted`;
- the cut-off with the worker killed, with a restart of the node before the
  deadline, and with a failed restore that an administrator repairs.

`tests/integration/firewalld_and_selinux.py` passes on both guests for
`firewalld`: services, ports, and rich rules in the running firewall and
the stored configuration without a reload, one rule for each node of an
inventory group, a node that leaves the group losing its rule, and a rule
that is missing from the stored configuration written again (108 s on Rocky
Linux 9, 107 s on AlmaLinux 10).

`tests/integration/blocked_port.py` passes on both guests with firewalld
as the node's firewall: a run falls back to SSH, the notice says that
firewalld is active with no rule that names port 7443, and the port stays
closed (25 s and 27 s).

Not verified:

- A node whose network NetworkManager does not manage, such as a node with
  systemd-networkd: no guest of the suite is such a node.
- A card that NetworkManager shows as connected "externally", such as the
  bridge of a container tool.
- firewalld's other source kinds, MAC addresses and ipsets: the module
  does not manage them, and refuses to remove them as unlisted members.

Module pages that rely on this section:
[`firewalld`](../reference/modules/firewalld.md), [`firewalld_zone`](../reference/modules/firewalld_zone.md), [`firewalld_zone_membership`](../reference/modules/firewalld_zone_membership.md).

## NetworkManager connection file module

Validated on 2026-11-14. `tests/integration/networkmanager_connection_files.py`
uses the production binaries, a second network card that is connected to
nothing, and the Incus agent as an observation path that does not use the
guest's network. All eleven cases pass on both guests, both with SELinux
enforcing (the old-format case has nothing to prove on version 10, where
no old-format profile exists):

| Guest | NetworkManager | Run time |
|---|---|---|
| Rocky Linux 9 | 1.54.3 | 236 s |
| AlmaLinux 10 | 1.56.0 | 296 s |

The cases prove, on each guest:

- the second card's example: dryrun, a new file, an unchanged rerun that
  activates nothing, a changed file, and removal;
- that a bad address and a text that is no keyfile fail before any change,
  in dryrun too; that an unknown section and a card that does not exist
  fail after the load or at activation, each with NetworkManager's own
  words, and are restored; and the documented limit that an unknown key in
  `[ipv6]` is accepted;
- on version 9, the refusal of a file for a card that an old-format
  profile names, in dryrun and in a real run, and then the migration
  command and the management of `System enp5s0.nmconnection`, a file name
  with a space;
- the refusal of a UUID that another file has, and a file without a UUID;
- a bridge with a port profile, a VLAN, a bond, and a dummy device: created,
  a renamed interface, and removed, with the SELinux label of an installed
  file;
- that the node's timer alone puts a removed bridge, its address, and its
  previously activated port back when the controller process is gone;
- a file that cuts the main card off, with a previous file and with none:
  new connections fail, and the node restores the file's bytes, owner,
  mode, modification time, and SELinux label, or removes the new file,
  activates the previous profile again, and accepts new connections;
- the same with the worker killed, with a restart of the node before the
  deadline, and with a failed restore that an administrator repairs.

One run on Rocky Linux 9 failed in its setup with "test HTTPS forward
failed" while another suite of the same run used the server; the same suite
passed when it ran again alone.

Not verified:

- Wi-Fi, VPN, team, and other profile kinds than Ethernet, bridge, bond,
  VLAN, and dummy.
- A profile without an interface name.
- A card that NetworkManager shows as connected "externally".

Module pages that rely on this section:
[`networkmanager_connection_file`](../reference/modules/networkmanager_connection_file.md).

## SELinux and the dnf modules

Validated on 2026-11-14 on Rocky Linux 9.8 and AlmaLinux 10.2 virtual
machines with SELinux enforcing, as installed.

- `tests/integration/selinux_labels.py` passes on both: the files,
  directories, links, unpacked trees, and SSH keys that modules create carry
  the label that the node's policy gives their path (8 s and 9 s).
- `tests/integration/firewalld_and_selinux.py` passes on both for
  `selinux_boolean`, `selinux_file_context`, `selinux_port`, and
  `selinux_mode`, including the refusal that names the package of a missing
  `semanage`.
- `tests/integration/server_resources.py` passes on both for `dnf_package`
  (dnf 4.14.0 on Rocky Linux 9, 4.20.0 on AlmaLinux 10) and
  `dnf_repository` (181 s and 118 s).
- The base suite (25 suites) passes on both guests.

Not verified: `selinux_mode` creating `/.autorelabel` on a real node;
only Go tests reach that path.

Module pages that rely on this section:
[`dnf_package`](../reference/modules/dnf_package.md), [`dnf_repository`](../reference/modules/dnf_repository.md),
[`selinux_boolean`](../reference/modules/selinux_boolean.md), [`selinux_file_context`](../reference/modules/selinux_file_context.md),
[`selinux_mode`](../reference/modules/selinux_mode.md), [`selinux_port`](../reference/modules/selinux_port.md).

## Netplan file module

Validated on 2026-10-14. `tests/integration/netplan_files.py` uses the
production binaries, a second network card that is connected to nothing,
and the Incus agent as an observation path that does not use the guest's
network. All twelve cases pass on the Ubuntu 24.04 guest, which uses
Netplan with
the systemd-networkd renderer:

| Guest | netplan.io | systemd | Run time |
|---|---|---|---|
| Ubuntu 24.04 | 1.1.2 | 255.4 | 266 s |

The cases prove:

- the second card's example: dryrun, a new file, an unchanged rerun that
  causes no address event on the main card, a changed file, and removal,
  after which the card has no global address and no route;
- that an unknown key, a bad address, a wrong format, and the removal of a
  file whose bridge another file's VLAN refers to fail before any change,
  each with Netplan's own words and the live path;
- one file with a bridge and its port, a VLAN, a bond, a dummy device, and
  a veth pair: created, a changed file that drops one device, the refusals
  for an existing device, and removal;
- that the node's timer alone puts a removed bridge, its address, and its
  port back when the controller process is gone;
- a file that cuts the main card off, with a previous file and with none:
  new SSH connections fail, and the node restores the file's bytes, owner,
  mode, and modification time, or removes the new file, and a new
  controller process reaches the node over HTTPS again;
- the same with the worker killed, with a restart of the node before the
  deadline, and with a failed restore that an administrator repairs.

The first runs found one fault in the module, now fixed and covered by a
Go test: the scan of the node's network cards read the kernel's control
file `/sys/class/net/bonding_masters` as a card.

`go build ./...`, `go vet ./...`, and `go test ./...` pass on the
development machine.

Not verified:

- The whole catalog did not run for the module addition. The module addition
  changed no code that every suite uses.
- Tunnels and VRFs: Netplan generates them; no guest case creates one.
- In a guest: the refusal of a changed `match:` of a definition and the
  refusal of a changed setting of a declared device that does not exist.
  Go tests cover refusal of a changed `match:` and refusal of a changed
  setting of an absent declared device.

Module pages that rely on this section:
[`netplan_file`](../reference/modules/netplan_file.md).

## systemd-networkd file module

Validated on 2026-10-14. `tests/integration/systemd_networkd_files.py` uses
the production binaries, a second network card that is connected to
nothing, and the Incus agent as an observation path that does not use the
guest's network. All nine cases pass on Debian 12 and Debian 13, which use
systemd-networkd:

| Guest | systemd | Run time |
|---|---|---|
| Debian 12 | 252.39 | 402 s |
| Debian 13 | 257.13 | 308 s |

The cases prove, on each guest:

- the second card's example: dryrun, a new file, an unchanged rerun, a
  changed file, and removal;
- that each measured mistake in a file which systemd-networkd logs with the
  file's name fails the task and is restored;
- bridge, bond, VLAN, dummy, and veth devices created in the documented
  order and removed in reverse order, a rename, and each refusal before any
  change;
- that the node's timer alone puts a removed bridge and its address back
  when the controller process and the node job worker process are gone;
- a file that cuts the main card off, with a previous file and with none:
  new connections fail, and the node restores the file's bytes, owner,
  group, mode, and modification time, or removes the new file, and accepts
  new connections again;
- the same with the worker killed, with a restart of the node before the
  deadline (the node must not be degraded afterwards), and with a failed
  restore that an administrator repairs.

`go build ./...`, `go vet ./...`, and `go test ./...` pass on the
development machine.

Not verified:

- The whole catalog did not run for the module addition. The module addition
  changed no code that every suite uses.
- `tunnel`, `vrf`, and other device kinds than the five above.
- A node whose only connection path uses a device that a change removes:
  the cut-off
  cases cut and restore the main card through a `.network` file.

Module pages that rely on this section:
[`systemd_networkd_file`](../reference/modules/systemd_networkd_file.md).

## WireGuard interface module

Validated on 2026-10-27, on the working tree of the `wireguard_interface`
addition before committing the addition.
`tests/integration/wireguard_interfaces.py` passed on the four target
distributions, each in two Incus containers on a Linux test server with
kernel 6.12.113, which has the `wireguard` module:

| Guest | `wireguard-tools` | Run time | A peer's first answer after a restart |
|---|---|---|---|
| Debian 12 | 1.0.20210914 | 140 s | 15.5 s |
| Debian 13 | 1.0.20210914 | 118 s | 15.3 s |
| Ubuntu 24.04 | 1.0.20210914 | 51 s | 15.2 s |
| Ubuntu 26.04 | 1.0.20250521 | 50 s | 15.5 s |

The nine cases prove:

- a dryrun on new nodes writes nothing and reports no public key;
- the first run creates each node's private key, `0600` in
  `/etc/wireguard` with mode `0700`, writes the private key into the
  configuration file, starts
  `wg-quick@wg0.service`, and reports `public_key`, which `wg pubkey` derives
  from the key file too; `mmx results --json` gives the keys as the module
  page reads them;
- with the keys in the inventory, the template gives each node the other as
  its peer through a reload, and a second run and a dryrun change nothing;
- a third member of the mesh, which has only its inventory entry, added and
  removed by a reload on both nodes while a ping between the two nodes
  runs every 50 ms: no packet lost;
- an `Address` change restarts its node's interface, and the other node
  reloads for the new allowed IP; the other node's first ping is answered
  only after WireGuard's next handshake, the times in the table;
- a peer removed by hand with `wg set` is put back by the next run;
- removing the key file makes a new key, and with the new public key in the
  inventory the other node reloads and the two answer each other again;
- the private keys, the replaced one included, appear in no mmx output,
  saved run, node job record, or journal;
- `state: absent` stops and disables the service, which removes the
  interface, and removes the file and the key; a second removal changes
  nothing.

The Go tests and the Python unit tests pass on the development machine.
The suite also passed on ARM64 Hetzner Cloud servers with the four target
distributions ([platform validation](#platform-validation-on-the-four-target-distributions)).

Not verified:

- A mesh of three machines: a job slot of the test harness has two guests,
  so the third member never runs.
- IPv6 addresses and allowed IPs, `Table` with another table, and an
  allowed IP of `/0` on a guest; Go tests cover their comparison.

## Routine fix modules

Validated on 2026-10-27, on the working tree of the routine fix modules
before committing the modules, in Incus guests on Linux test servers with
kernel 6.12.113: containers for
`routine_fixes.py` and `podman_containers.py`, virtual machines for
`docker_containers.py`. Each suite passed on the four target distributions:

| Guest | Podman, and the account's storage driver | Docker and its image store |
|---|---|---|
| Debian 12 | 4.3, vfs | 29.9.2, containerd |
| Debian 13 | 5.4, overlay | 26.1.5, classic |
| Ubuntu 24.04 | 4.9, overlay | 29.9.2, containerd |
| Ubuntu 26.04 | 5.7, overlay | 29.1.3, containerd |

The cases prove:

- `restart_now`: a one-time playbook is still needed while its marker file
  is missing; the playbook's dryrun leaves the unit unchanged; the run
  restarts the
  running unit once, by its invocation ID, and writes the marker file; then
  `mmx check-one-time-playbooks` reports the playbook done, and a rerun
  leaves the
  unit alone; with the marker file there, a stopped unit is started as by
  any `state: running` task.
- `systemd_journal_vacuum`: archived files made with `journalctl --rotate`;
  with `keep_newer_than` the dryrun predicts exactly the file that
  `journalctl --vacuum-time` deletes, aged by its modification time, and its
  bytes on disk; with `keep_at_most_size` the oldest files that
  `journalctl --vacuum-size` deletes until the directory fits; reruns are
  unchanged.
- `apt_downloaded_package_files`: the dryrun counts the guest's own package
  files and three of the suite's, one in `partial/`, with their bytes;
  `apt-get clean` deletes the package files, apt's lock and `partial/` stay,
  and the rerun
  runs nothing; while a process holds the lock of the archives directory
  for six seconds, the node task waits and then deletes.
- `delete_old_files`: old files whose names match, also in a directory
  below, go, and the dryrun lists the files that the run deletes; a symlink
  to a file and one to a
  directory, a tmpfs, and a bind mount below the directory are left alone;
  `include_subdirectories: false` deletes only directly in the directory; a
  directory that is a symlink, and one with a symlink in a middle component
  of its path, fail the dryrun and the run, naming the link, and change
  nothing; `validate` refuses a directory one level below `/`.
- `podman_unused_images`: in the system's store, images built on the node
  that share the test program's layer; the dryrun of
  `dangling_images_only` lists what Podman's prune filters select and
  predicts the bytes of the layers no remaining image uses, and the run
  removes exactly the predicted images and frees the predicted bytes; the
  image of a
  stopped container stays, also without a name;
  `every_image_no_container_uses` also removes named images; in the
  account's own store the same as the account, where `freed_bytes` is `null`
  with Debian 12's vfs driver.
- `docker_unused_images`: imported images, one used by a stopped container;
  the dryrun lists what the daemon's prune removes, the run removes exactly
  those and reports the daemon's freed bytes; in the containerd image store
  an image whose name a container was created with stays, and a dangling
  record of a named image is not counted.

`one_time_playbooks.py` and `server_resources.py`, whose code the step
shares (the run-once marker, apt's wait for its lock, `systemd_service`),
passed on the four distributions too. The Go tests and the Python unit tests
pass on the development machine.

Not verified:

- A journal that keeps damaged (`.journal~`) or empty archived files on a
  guest; Go tests cover journald's rule for them.
- Podman's removal of the unnamed parent images that a build with cached
  steps leaves, which the dryrun does not list; Docker's former builder's
  parent images likewise.
- The removal of unused images on ARM64 Hetzner Cloud servers, whose
  cases `docker_containers.py` and `podman_containers.py` hold;
  `routine_fixes.py` passed there on the four target distributions ([platform validation](#platform-validation-on-the-four-target-distributions)).

## Reachability confirmation and owed restore

Validated on 2026-10-14. `tests/integration/owed_restores.py` passed as VM
guests on Debian 12 and 13 and Ubuntu 24.04.
The suite proves the whole mechanism of the
[execution design](design/execution.md#reachability-confirmation-and-owed-restore)
on real nodes with a module that exists only in test builds
(`test_change_with_owed_restore`, Go build tag `mmx_owed_restore_test`);
production builds exclude that module. The suite has ten cases:

- a change that the controller process confirms, on a node with
  `connection: ssh_only` and on a node with HTTPS; nothing restores the file
  after the deadline;
- a change that cuts off new SSH connections while existing ones survive,
  and a change that cuts the HTTPS kind while SSH works: the node restores;
- an activation that fails, and an activation that hangs with a child that
  ignores SIGTERM: the restore stops the activation's processes before
  the restore
  puts the file back;
- a killed node job worker process with every controller request stopped:
  the timer alone restores;
- a node restart during the confirmation wait: the restore runs during
  the start, before
  the deadline;
- a restore that fails: the refusal of a task for the same file and of a
  different node runner executable, and the repeat with `systemctl start`;
- no new connection before the change: the file stays untouched;
- dryrun writes no record and no unit.

The Go tests of all packages and `go vet ./...` passed on the reachability
confirmation and owed restore build validated on 2026-10-14.

The whole catalog passed on the build of 2026-10-14 on Debian 13, with
38 container suites and 15 VM suites. `go test -race ./...` passed on a
test server, and the node package's tests passed there as root.

Not verified:

- The production module `systemd_networkd_file` uses the mechanism; its
  results are [above](#systemd-networkd-file-module), as are the results of
  `netplan_file`.
- The guest suite cuts the HTTPS kind by stopping the node HTTPS API
  process, which also ends established HTTPS connections. That a
  confirmation always uses a new HTTPS connection is proven by the Go test
  `TestConfirmationHTTPSConnectionsAreFreshAndProgressStillReuses`, not on
  a guest.

## Container-module build validated on 2026-10-09

Validated on 2026-10-09 on the build of step 10, the container modules:
`docker_image`, `docker_compose_project`, `podman_image`, `podman_quadlet`,
and the `wait_for` forms `docker_container_is_healthy`,
`docker_compose_project_is_ready`, and `podman_container_is_healthy`, with
an image ID as a tracked input of its node task block. The container-module
build has
five changes outside the container modules: a node task's private directory
below `/run/mmx/private_task_directories/`, which holds the logins of git,
Docker, and Podman tasks and which the node removes after a killed node job
worker process; a service record that keeps, after a start or restart, only
the inputs of the node task block that made it; an account that runs
systemd's check of its own unit file; a changed language or tool
installation that restarts a running service; and a `wait_for` that reports
the last check that finished. The container-module build extends the
deployment modules of step 9.
The Linux controller machines were two Debian 13 AMD64 test servers with Incus 6.0 and KVM, Go 1.26.8; the macOS controller was macOS 26.6
ARM64. The AMD64 node runner executable, as `scripts/build.sh` builds it,
grew from 14.65 MB to 15.08 MB with the container-module addition.

| Check | Result |
| --- | --- |
| macOS ARM64 controller | `go test ./...` and `go vet ./...` passed, including: each container module against stand-ins of `docker`, `docker compose`, `podman`, `skopeo`, and Podman's generator (unchanged, changed, dryrun, rejected inputs, a failure after partial work, another try); the lines of Docker Compose's own dry run read with a closed list of words, and an unknown line giving `not_checked`; no login file in a dryrun; a dead node job's private directory removed when the next node job starts, and a running node job's private directory kept; a start in a later node task block that has loaded an earlier node task block's file; a wait that reports the last check that finished |
| Race detector | `go test -race ./...` passed in every package on a Debian 13 AMD64 test server |
| Incus guests on the Debian 13 AMD64 test servers: Debian 12 and 13, Ubuntu 24.04 | All 37 workflow suites passed on each, also in the full run on the final build. `docker_containers.py` runs in a VM on every distribution; `podman_containers.py` runs in system containers. |
| Docker and Podman versions in the Incus guest runs | Docker 29.9 with Docker Compose 5.6.0 and the containerd image store, from Docker's repository (Debian 12, Ubuntu 24.04); Debian 13's own Docker 26.1.5 with Docker Compose 2.26.1 and the classic image store. Podman 4.3.1 (Debian 12), which has no Quadlet, and 4.9.3 (Ubuntu 24.04), 5.4.2 (Debian 13). |
| Hetzner Cloud servers | `docker_containers.py` passed on Debian 13. `podman_containers.py` passed on Debian 12 and 13, and Ubuntu 24.04. |

The first full run of the container-module build found two faults, fixed
before the final run. A test fixture ran `dpkg -i` while Ubuntu's daily
update held dpkg's lock. Under load, a two-second wait for a Podman
container expired during a `podman` command and reported the command's
interruption rather than the last check's result. The Podman wait fault
led to the `wait_for` change described above.

The runs on Hetzner Cloud servers found that Hetzner's
Debian images install no recommended packages
(`/etc/apt/apt.conf.d/00InstallRecommends`), so a pod failed for want of
`catatonit`; the Podman host example and the suite now name the packages
that Debian and Ubuntu only recommend.

The two container suites add about ten minutes of work in each
distribution's guests, and the servers' processors are busy for the whole
run. `docker_containers.py` takes 200 to 330 seconds alone and 600 to 1200
seconds in a full run.

Suite logs are under the run's test root, in `<distribution>/` (Incus) and
`<distribution>/slots/` (Hetzner), on the server that ran the suites.

## Changes after that validation

Eight changes followed the container-module validation on 2026-10-09.
The eight changes were validated with the levels of the
[testing guide](testing.md#the-full-catalog-with-incus) ("Test in levels")
for a change of code that every suite uses. Validation did not repeat the
full run:

- A secret in a login of `git`, `docker_image`, `docker_compose_project`, or
  `podman_image` does not hide the node task's result; MMX replaces the
  login with `[redacted]` in what the task reports.
- A role call that passes a secret to a role input hides nothing by itself;
  the role's node tasks are hidden from the first node task that reads
  the input.
- A start, restart, or reload of a unit that fails reports the unit's lines
  of the systemd journal from that action.
- A unit that systemd still starts (`activating`) is not started.
- A module that is not core gives its own baseline: its node task block
  refreshes its service only when the module's task changed something, also
  on the first run and after a reboot. Two node task blocks that call one
  module for one service each follow the calls in the respective node task
  block. Only the last try of a module task gives the fingerprint.
- A dryrun of a command that follows tracked inputs takes the same baseline
  as a run.
- The four lock file modules name their directory `lock_file_dir`.
- A `systemd_service` node task tries a failed action once unless it sets
  `retry`.

| Check | Result |
| --- | --- |
| macOS ARM64 controller | `go test ./...` passed, and `go vet ./...` for macOS and for Linux |
| Incus guests, Debian 13 | All 37 suites passed before the `activating` change |
| Incus guests, Debian 12, Ubuntu 24.04 | The five changed suites (`account_units.py`, `external_modules.py`, `release_directories.py`, `docker_containers.py`, `podman_containers.py`): all passed |
| Incus guests, Debian 13, after the `activating` change and the suite fixes | `account_units.py`, `external_modules.py`, `docker_containers.py`, `podman_containers.py`, and `kernel_settings.py`: all passed |
| Incus guests, Debian 13, after the change to one try of `systemd_service` | The 18 suites whose projects have a `systemd_service` task: all passed. `account_units.py` failed on Debian 13 in a new check of the suite itself, which compared a count with its line end; the suite was corrected and then passed alone on Debian 13 |

The race detector and the suites on Hetzner Cloud servers were not run for
the eight changes after the container-module validation. The AMD64 node
runner executable is 15.13 MB.

## Step 11: the database and certificate modules

Step 11 followed on 2026-10-09: `postgresql_role`,
`postgresql_database`, `mysql_user`, `mysql_database`, and
`certbot_certificate`, with the example projects
`postgresql-server-for-one-application`,
`mariadb-server-for-one-application`, and `certificate-with-certbot`.
One change is outside the database and certificate modules: a node task
that fails at its time limit
keeps its own reason behind `time_limit exceeded:`. Before the modules were
written, the tools were measured on every target distribution except Ubuntu
26.04. The step was
validated with the levels of the
[testing guide](testing.md#the-full-catalog-with-incus): its new
suites on every distribution once, and the suites that use time limits on
Debian 13.

| Check | Result |
| --- | --- |
| macOS ARM64 controller | `go test ./...` passed, and `go vet ./...` for macOS and for Linux. The Go tests run each module against a stand-in of its tool (`psql` and `pg_isready`, the client of MySQL and MariaDB with a stand-in server, `certbot`) and never the network |
| Incus guests, Debian 12 and 13, Ubuntu 24.04 | `postgresql_databases.py`, `mysql_databases.py`, and `certbot_certificates.py`: all passed, in containers, before the review of the step and again after its corrections. PostgreSQL 15, 16, and 17; MariaDB 10.11 and 11.8 on every distribution, and MySQL 8.0 and 8.4 on Ubuntu; certbot 2.1, 2.9, and 4.0 |
| Incus guests, Debian 13 | `diagnostics.py`, `external_modules.py`, `vm.py test`, and `server_resources.py`, the suites whose projects set a time limit: all passed |
| Certificate authority | Every certificate of the suite comes from Pebble, the ACME test server of Let's Encrypt, on the test guest. The suite maps the two names of Let's Encrypt to the guest before certbot is installed, and fails when certbot's log names another host than the guest: it passed on each of those Debian and Ubuntu distributions |
| Node runner executable | AMD64, as `scripts/build.sh` builds it: 15.82 MB, from 15.13 MB before the step. `golang.org/x/text/unicode/norm`, the one new dependency, is 0.10 MB of that |
| Hetzner Cloud servers, Debian 13 | The three new suites passed on Debian 13 |
| Incus guests, Debian 12 and 13, Ubuntu 24.04, after that change of the suite | `certbot_certificates.py`: all passed |
| Race detector | `go test -race -timeout 60m ./...` passed in every package on a Debian 13 AMD64 test server, with no data race. `internal/modules` takes about 21 minutes on the Debian 13 AMD64 test server. The first run found unprotected variables in one certbot test and one PostgreSQL test that depended on the machine's speed; the certbot and PostgreSQL tests were corrected, and no module changed |

## Step 12: the new commands and the controller machine as a node

Step 12 followed on 2026-10-10: the four `mmx secrets` key commands
(`add-random`, `list-keys`, `diff-keys`, `usage`); one-time playbooks, with
`mmx new-one-time-playbook`, `mmx check-one-time-playbooks`, and
`mmx complete-one-time-playbook`; and the controller machine as a node, with
the rule that MMX never reboots the machine that runs the controller
process. Step 12 also added a validation rule: a secret cannot select
another value. Step 12 added clearer messages for a missing playbook or
inventory, for
a node that does not run Linux or has no machine ID, and for an SSH failure
through a jump host. The step was validated with the levels of the
[testing guide](testing.md#the-full-catalog-with-incus), after the
corrections of its review.

| Check | Result |
| --- | --- |
| macOS ARM64 controller | `go test ./...` passed, and `go vet ./...` for macOS and for Linux. The harness's own unit tests passed, 55 of them |
| Incus guests, Debian 12 and 13, Ubuntu 24.04 | `task_secrets.py`, `one_time_playbooks.py`, and `controller_machine_as_node.py`, and with them `fleet.py` and `linux_controller.py`, because the step changed how a node job starts and how a node reboots: all passed, in containers |
| Incus guests, Debian 13 | The whole catalog of 42 suites passed |
| Hetzner Cloud servers, Debian 13 | `one_time_playbooks.py` and `controller_machine_as_node.py`: all passed, with one pair of servers for each distribution. On each pair a controller process on one server managed that server through `127.0.0.1`, its host name, and its other address, was refused the reboot of that server, and rebooted the other server |
| Race detector | `go test -race -timeout 60m ./...` passed in every package on a Debian 13 AMD64 test server, with no data race |
| Node runner executable | AMD64, as `scripts/build.sh` builds it: 15.84 MB, from 15.82 MB before the step |
| Boot IDs, measured for the reboot rule | An Incus container has a boot ID of its own, new at each start: LXC mounts a file of its own over the kernel's. A Docker container (Docker 26.1.5 in a Debian 13 VM) reads its server's boot ID, also after a restart and with `--privileged`, and its image has no `/etc/machine-id` |
| Machine IDs, measured for the message of a node without one | `systemd-machine-id-setup` writes a new ID into a missing or empty `/etc/machine-id` on systemd 252 and 257. A file that holds `uninitialized` stays as it is on systemd 257, so the message for a node without a machine ID says to remove `/etc/machine-id` first |
| Review of the step | One fault that the acceptance contracts do not allow, corrected before these runs: a node refused to record the last successful run of a one-time playbook, whose name starts with a digit, so each such run ended with a housekeeping error on every node. `one_time_playbooks.py` now fails on that error |

## Step 13: the official extra modules

Step 13 followed on 2026-10-10: the twelve
[official extra modules](design/modules-official-extra.md) for Incus,
libvirt, ZFS, disk partitions, LVM, RabbitMQ, and MongoDB, each a Starlark
script with its description and its page in `official_extra_modules/`. With
them came what a script needs to drive a tool: the result statuses
`missing_prerequisite`, `not_checked`, and `invalid_inputs`,
`try_again: False` beside a failure, the primitives `find_program` and
`earlier_node_task_would_change`, root's home directory for every command of
a primitive, and `private_value` in a module description. The test harness
changed too: every VM guest now starts with Secure Boot off, because outside
Ubuntu a kernel with Secure Boot refuses ZFS's kernel module, and a guest
restarts as soon as a suite asks. Before the modules were written, the real
tools were measured on all four target distributions. The modules read the
tools' output as observed on the four target distributions. The step was
validated with the levels of the
[testing guide](testing.md#the-full-catalog-with-incus), after the
corrections of its review.

| Check | Result |
| --- | --- |
| macOS ARM64 controller | `go test ./...` passed, and `go vet ./...` for macOS and for Linux. The harness's own unit tests passed, 58 of them |
| Incus guests, Debian 12 and 13, Ubuntu 24.04, the final build | The six new suites (`incus_instances.py`, `libvirt_domains.py`, `partitions_and_lvm.py`, `zfs_datasets.py`, `rabbitmq.py`, `mongodb_users.py`) and `external_modules.py`: all passed. `incus_instances.py`, `libvirt_domains.py`, and `partitions_and_lvm.py` run in VMs on every distribution; `zfs_datasets.py` runs in a VM where it installs ZFS |
| Incus guests, Debian 12 and 13, Ubuntu 24.04, the whole catalog | All 48 suites passed on the Debian and Ubuntu guests |
| Where each tool can be installed | Incus, libvirt with QEMU, LVM and `sfdisk`, RabbitMQ, and MongoDB: each of the Debian and Ubuntu distributions above. ZFS: the suite installs it on Ubuntu 24.04 and Debian 13, not on Debian 12. Where a tool is missing, the suite proves the module's behavior without it: a dryrun reports the task not checked and names the program, and a run fails once |
| Hetzner Cloud servers, Debian 13 | The six new suites and `external_modules.py` passed on Debian 13 after a correction to the `libvirt_domains.py` suite's cleanup check. A Hetzner Cloud server has no `/dev/kvm`, so that suite runs only its case without `virsh` there. ZFS's kernel module loaded, built on the server on Debian 13 |
| Race detector | `go test -race -timeout 90m ./...` passed in every package on a Debian 13 AMD64 test server, with no data race |
| Node runner executable | AMD64, as `scripts/build.sh` builds it: 15.91 MB, from 15.84 MB before the step. The official extra modules beside `mmx` are 38 files with 0.6 MB |
| Review of the step | Two reviews, one for the shared Go code, the harness, storage, and libvirt, and one for Incus, RabbitMQ, and MongoDB. Four faults that the acceptance contracts do not allow, corrected before the final runs. `lvm_logical_volume` removed the snapshots of a volume and the volumes of a thin pool with the volume that the task named; `lvm_logical_volume` now refuses while a volume depends on the named volume. In `incus_profile`, `incus_instance`, and `libvirt_domain`, a failure after a change was followed by MMX's second try, which found the changed state and reported success, or reported `created: false` for what the task had created; MMX now tries a failure after a change only once. The suites had missed the failures after changes because the suites disabled retries for the failure-after-change cases. The failure-after-change cases now use the default. Neither review found a secret in any output |

## The capability review's additions

Validated on 2026-10-14 on Debian 12 and 13 and Ubuntu 24.04. The tested capabilities extend existing modules and add
three small modules, as the table below describes. Each capability was
first tested at the level of an ordinary change: the capability's Go tests
and guest suite on Debian 13. On Debian 13, the whole catalog of the
network build passed after the capability suites. The four changed
suites then ran once on Debian 12 and Ubuntu 24.04 in Incus
guests on the Debian 13 AMD64 test servers, together with `diagnostics.py`
([module pages](#module-pages)): all passed.

| Addition | Result |
| --- | --- |
| `ufw`: `outgoing_rules` and `outgoing_rules_not_listed` | `server_resources.py` runs the ufw cases on the distributions with apt, and passed on Debian 12 and 13 and Ubuntu 24.04. ufw's words for outgoing rules were measured with ufw 0.36.2 |
| The fact `storage.block_devices` | `fleet.py` and `node_facts.py` passed on Debian 12 and 13 and Ubuntu 24.04 on container guests |
| `lvm_volume_group`: growth of a physical volume; `grow_filesystem_to_fill_device`; `grow_partition_into_free_space_after_it` | `partitions_and_lvm.py` passed on Debian 12 and 13 and Ubuntu 24.04 on VM guests. Measured on Debian 13: LVM 2.03.31, also with a second metadata area; e2fsprogs 1.47.2; xfsprogs 6.13.0; growpart 0.33 |
| The example `security-updates-on-debian-and-ubuntu` | `server_resources.py` passed on Debian 13 |
| macOS ARM64 controller | The Go tests of the changed packages passed |

Not measured: growpart's words for a partition number that the disk does
not have and for a disk without a partition table (the module looks at the
disk itself first); `findmnt` for a device that is mounted at two places.

Module pages that rely on this section:
[`ufw`](../reference/modules/ufw.md).

## The module interface rebuild

Validated on 2026-10-19. The rebuild changed the inputs of most core and
official extra modules so that a reader does not have to guess, following
the rules every module follows in the
[modules design](design/modules.md#rules-every-module-follows): package
modules take a `packages` list whose entries each state their version;
settings with no safe guess are required; leaving a setting out never
deletes; creation-only settings say so in their names; protective choices
follow three levels, with `dangerously_` only for real danger. Each module
page gives the reasons for the module's inputs.

- `go test ./...` passed on the controller machine.
- Each module family's own suites passed on Debian 13
  before the family's interface changes were combined with the remaining
  module families' changes, in Incus guests on the Debian 13 AMD64 test servers.
- On the whole build, the whole catalog passed on Debian 13 with
  37 container suites and 16 VM suites. The whole-catalog run checks
  the change to the test helper that installs packages, which many suites
  use.
- The 32 suites whose behavior changed, with the smoke test `vm.py test`,
  passed on Debian 12, Ubuntu 24.04, Ubuntu 26.04:
  33 suite runs on each.
- `subscription_web_service_example.py`, which runs only on Ubuntu 24.04,
  passed on Ubuntu 24.04 on the build of the file, directory, archive, and
  Git modules alone. On the whole build,
  `subscription_web_service_example.py` ran with the naming table below.

The first pass of the whole catalog found faults that each family's own
suites could not: four suite fixtures with old input names or an f-string
that Python evaluated; an apt dryrun that failed on a package whose
source an earlier node task of the same run adds, such as `incus-base`
from bookworm-backports on Debian 12, which is now `not_checked`; and Go
tests that failed inside a guest of `linux_code.py`, which has only the
compiled test programs. Five fuzz tests read their seeds from `examples/`
and now carry copies built into the test program; three `pip_package`
tests need a real pip, which a guest's `python3` lacks, so the three tests skip
in the guest. `language_installers.py` runs the helpers exercised by the
three `pip_package` tests, with pip on every target distribution.
Each failed suite passed in a rerun on the
distributions where the suite had failed.

The ARM64 suites and the macOS controller check ran in the
[platform validation](#platform-validation-on-the-four-target-distributions)
of the release.

## The naming table

Validated on 2026-10-19. Names spell three words one way: `env` for
environment variables, `config` for configuration, and `dir` in a label
without a verb; the whole word `directory` stays in a name with a verb, in
the plural, and as a name's first word. The renames reached the project
settings (`secrets_from_env`), module inputs and outputs, the value
`failure_kind: config`, the key `config` of module description examples,
and the example projects.

- `go test ./...` passed on the controller machine.
- The 29 suites whose files changed passed on Debian 13,
  in Incus guests on the Debian 13 AMD64 test servers.
- `subscription_web_service_example.py` passed on Ubuntu 24.04.

The first run found two faults. The rename had also changed PostgreSQL's
own setting `data_directory` inside `postgresql_databases.py`; a tool's own
names keep their spelling. The subscription example's two MySQL privilege
checks looked for `main_application_production`, but `grant_on` names a
literal database, so MMX writes its `_` as `\_` and MySQL shows that; the
checks now match the escaped name. Each suite passed in a rerun.

## PostgreSQL failover and backups

Validated on 2026-10-27: the five official extra modules for PostgreSQL,
[`pg_auto_failover_monitor`](../../official_extra_modules/pg_auto_failover_monitor/README.md),
[`pg_auto_failover_node`](../../official_extra_modules/pg_auto_failover_node/README.md),
[`pg_auto_failover_primary`](../../official_extra_modules/pg_auto_failover_primary/README.md),
[`pgbackrest_stanza`](../../official_extra_modules/pgbackrest_stanza/README.md),
and [`pgbackrest_restore`](../../official_extra_modules/pgbackrest_restore/README.md),
and the harness's third test machine for a suite that needs one
([testing guide](testing.md#the-full-catalog-with-incus)). Before the
modules were written, pg_autoctl, psql, and pgbackrest were measured in
Incus containers on Debian 12 (pg_auto_failover 2.0, PostgreSQL 15,
pgBackRest 2.45) and Ubuntu 26.04 (2.2, 18, 2.58). The modules read the
programs' JSON as observed on Debian 12 and Ubuntu 26.04.

Debian 12 and Ubuntu 26.04 were probed before the local logins,
`listen_address`, the primary's rule, and the restore of a cluster of
Debian's tools were built. The account `nobody` logged in as `postgres`
over `127.0.0.1` and on the socket through initdb's `trust` lines.
With plain `peer`, `pg_autoctl show state` on the monitor failed.

A `pg_autoctl config set` of `postgresql.listen_addresses` while the service
ran was written back by the running pg_autoctl on a replica and on the
monitor. Each restart of a primary's service made the monitor fail over.
After a stopped primary's failover, the monitor's task named the old primary,
which reported `primary` with the assigned state `demoted`, and
`pgautofailover.get_primary` named the old primary for 30 seconds while no
node could take writes.

`pg_ctl start --pgdata` refused a restored cluster of Debian's tools
("could not access the server configuration file"). The cluster's service
started the restored cluster.

Before the restore on the local socket and the record of each
restore's target were built, `pgbackrest.py` restored a node whose
`listen_address` was `mmx-dev`'s address onto `mmx-peer`, on Debian 12 and
Ubuntu 26.04. The restored `postgresql-auto-failover.conf` named
`mmx-dev`'s address, and PostgreSQL ended at startup
("could not bind IPv4 address
...: Cannot assign requested address", "could not create any TCP/IP
sockets").

A rerun with `recover_to: latest` after a restore to a time was `unchanged`,
in a dryrun and a run, on Debian 12 and Ubuntu 26.04.

In the PostgreSQL failover and backup build's runs,
`pg_autoctl create postgres` wrote `mmx-peer`'s address into
`postgresql-auto-failover.conf` and `pg_autoctl.cfg`, and the keeper
started PostgreSQL with `-h` and `mmx-peer`'s address. The restored
`postgresql.auto.conf` held no `listen_addresses`. The configuration of
the cluster of Debian's tools in `/etc/postgresql` included nothing from
the cluster's data directory, which held no `postgresql.conf`.

| Check | Result |
| --- | --- |
| macOS ARM64 controller | `go test ./...` passed, with the stand-in tests of the five modules in `internal/modules/official_extra_postgresql_test.go`; `gofmt -l` named none of the project's own files; `scripts/static-analysis.sh` had no findings. The harness's own unit tests passed, 95 of them |
| Incus containers, Debian 13 and Ubuntu 26.04 | `pg_auto_failover.py` (14 workflows) and `pgbackrest.py` (10 workflows), each on three containers, passed on both: pg_auto_failover 2.2 with PostgreSQL 17 and 18, pgBackRest 2.55.1 and 2.58 with the S3-compatible bucket. A suite takes seven to ten minutes |
| Incus containers, Debian 12 and Ubuntu 24.04 | Both suites passed on both: pg_auto_failover 2.0 with PostgreSQL 15 and 2.1 with 16, pgBackRest 2.45 and 2.50 with a directory repository. After a task restarts a node's service, the suite waits for a healthy formation before it logs in to the node or reads its addresses, since the keeper of a replica may restart PostgreSQL once more |
| What each distribution covers | pg_auto_failover: the whole suite on all four, the failover included: the monitor promoted `db02` about 50 seconds after `db01`'s service stopped, and `db01` reported `primary` with the assigned state `demoted`. pgBackRest on all four: the restore to a time on `mmx-dev` with no TCP socket on port 5432, the refused rerun to the end of the archive, the restore of the whole archive onto `mmx-peer`, which lacks `mmx-dev`'s address, and its registration, after which `ss` showed PostgreSQL at `mmx-peer`'s address alone. An S3-compatible bucket that keeps versions, with `read_repository_as_of` after the backup was deleted from it, on Debian 13 (2.55) and Ubuntu 26.04 (2.58), which package `python3-moto`; a directory repository on Debian 12 (2.45) and Ubuntu 24.04 (2.50), where `read_repository_as_of` is a missing prerequisite that names the version, and which the suite copies to `mmx-peer` |

## pacman package module

Validated on 2026-11-14, on the working tree of `pacman_package` before
committing the module, in an Incus container guest of Arch Linux, the
rolling release of that day, with pacman 7.1.0 and kernel 6.12.113 on the
Linux test server. `pacman_packages.py` passed, in about 105 seconds, of
which the lock that stays takes 60.

The cases prove:

- A dryrun of an installation names what pacman would install and upgrade,
  judged against the node's databases, and changes nothing. A run installs
  nginx with mailcap and upgrades an older `which` in the same transaction;
  `changed_packages` equals what `pacman -Q` shows changed.
- A rerun and a dryrun of installed packages are unchanged and leave the
  time of change of `/var/lib/pacman/sync/core.db` as it was.
- In a node task block, nginx starts once. When the installed version
  differs from the one nginx loaded, a dryrun predicts the restart, and a
  run restarts nginx once while the package task is unchanged; the next run
  restarts nothing.
- A whole-system upgrade is previewed by a dryrun, upgrades nginx to the
  repository's version, and is unchanged with nothing newer although it
  refreshed the databases; the node task block then restarts nginx.
- A removal that would take `nginx-mod-stream` fails in a dryrun and a run
  and changes nothing; with `also_remove_packages_that_depend_on_these` it
  removes all three packages, and the removal is then unchanged.
- `nginx-mainline`, which conflicts with the installed nginx: a dryrun
  says it would install it, because `--print` checks no conflict; the run
  fails, names both packages, and changes no package.
- An unknown name is not checked in a dryrun and fails a run once; `sh`, a
  name that `bash` provides, fails in both without a refresh, and
  `extra/tree` fails in both.
- Removing the group `archlinux-tools` while its member `pacquery` is
  installed, and removing `sh`, fail and remove nothing.
- A task waits while pacman's lock is held and installs when the lock goes;
  a lock that stays fails the task after 60 seconds.

## zypper package and repository modules

Validated on 2026-11-14, on the working tree of `zypper_package` and
`zypper_repository` before committing them, in an Incus container guest of
openSUSE Leap 16.1 with zypper 1.14.101 and kernel 6.12.113 on the Linux
test server. `zypper_packages.py` passed, in about 240 seconds, of which the
lock that stays takes 60. The suite serves a signed test repository on the
guest and installs one real package, `tree`, from openSUSE's own repository.

The cases prove:

- A dryrun of an installation names what zypper would install and changes
  nothing; a run installs `tree`, and `changed_packages` equals what `rpm`
  shows changed. A rerun and a dryrun of installed packages are unchanged.
- `zypper_repository` in a dryrun names the repository and the key and
  changes nothing; a run imports the key into rpm and writes the repository
  file with every setting spelled out; reruns are unchanged.
- A package task after the repository task refreshes the repositories and
  installs a package and its dependency from the test repository; an
  unchanged task makes no request to the repository.
- `zypper remove` removes dependents by itself; the task refuses that in a
  dryrun and a run and changes nothing, and with
  `also_remove_packages_that_depend_on_these` removes both packages.
- A name that no repository has is not checked in a dryrun and fails a run
  once. A capability that an installed package provides is refused, to
  install and to remove. A pattern, a path, a capability with parentheses, a
  version after the name, and a repository in front of the name are refused
  before anything runs.
- zypper's dry run does not see two packages that own one file with
  different content, or a package without a signature. The run fails with
  zypper's message (exit code 8), reports the task changed, and installs
  nothing.
- A package whose script fails (exit code 107) is installed, the task fails
  after one try, and the next run is unchanged.
- An exact version is installed or updated to, with or without its release;
  `newest_on_every_run` updates; an older version than the installed one, an
  operator, and a version that no repository has are refused.
- In a node task block, a newer package version is predicted by a dryrun and
  restarts the service once in a run.
- A whole-system update is previewed, updates, and is unchanged with
  nothing newer although it refreshed.
- A task waits while another zypper holds the lock (11 seconds in the run)
  and installs when it goes. A lock held longer than 60 seconds fails the
  task with zypper's message that names the holder.
- A repository whose key was never imported is refused by zypper, and the
  package task fails with zypper's message. Removing a repository removes
  its file, its key file, and its key from rpm.
- A key from a web address with the wrong SHA-256 fails and writes
  nothing; with the right one the repository is added, a rerun is
  unchanged, and the removal is unchanged the second time.

- `tree-2.2.1` and `tree.x86_64` are refused in a removal and an
  installation and leave `tree` installed (zypper's `--name` option did not
  make them exact: `zypper remove --name tree-2.2.1` still removed `tree`).
- A package from a second vendor: `newest_on_every_run` is unchanged and its
  reason quotes `There is an update candidate for 'mmxtest-vendor' from
  vendor 'Vendor B', while the current vendor is 'Vendor A'`.
- `ZYPP_LOCK_TIMEOUT=60` made zypper wait for the lock for `zypper refresh`,
  `install`, and `remove` as well as the dry run (waits of 4 to 6 seconds
  behind a download); without it the same commands ended at once with exit
  code 7.
- The distribution's own signing key, given to a repository, is not recorded
  and stays in rpm when the repository goes, and `zypper refresh --force` of
  the distribution's repository works afterwards. Of two repositories with
  one key, the first to go leaves the key and a refresh of the second works;
  the last removes it. A replaced key leaves rpm.

Observed on the guest and written in comments of the script: exit code 4 for
a solver problem, a repository that did not answer, and a key that is not
imported; 7 for the lock, with the holder's pid in the message; 8 for a file
conflict and a missing signature; 104 for a name that no repository has; 107
for a failed script. Exit codes 102, 103, 105, and 106 are in zypper(8) and
not observed.

## Fedora 45 and dnf 5

Validated on 2026-11-14 on a Fedora 45 virtual machine with SELinux
enforcing (kernel 7.3, systemd 262, dnf 5.4.7, rpm 6.1), one Incus VM on a
Linux test server, with the clone of `main` plus the Fedora change.

- Before any change, every dnf command that `dnf_package` and
  `dnf_repository` run was run by hand on Fedora 45. Six things differ from
  dnf 4 (the version lock, the lock-file preview, the transaction table,
  `config-manager`, advisory types, and that `install` leaves an installed
  package alone); rpm 6
  names a key's package by its fingerprint. [The design](design/modules-packages.md#package-modules-for-each-family)
  lists them.
- `tests/integration/server_resources.py` passes for `dnf_package` and
  `dnf_repository` (154 s). The suite takes its lock checks from the node:
  on dnf 5 it reads `/etc/dnf/versionlock.toml`, installs no plugin, and
  expects the refusal of a wildcard that the lock cannot hold. It covers the
  security upgrade, which found Fedora's advisories.
- `selinux_labels.py` (10 s), `firewalld_and_selinux.py` (55 s), and
  `blocked_port.py` (35 s) pass.
- The base suite passes: 24 of its 25 suites in the full run, and
  `access.py`, which passed all 17 workflows but failed in its cleanup, which
  put back the root password's lock with `chpasswd -e` (Fedora's lock is
  rejected as an invalid hash); the cleanup uses `usermod -p` now, and
  `access.py` passes on Fedora 45, Rocky Linux 9, and Debian 13.
- `server_resources.py` still passes on Rocky Linux 9 (195 s, dnf 4.14) and
  AlmaLinux 10 (135 s, dnf 4.20): dnf 4 is unchanged.

Not run on Fedora: `networkmanager_connection_files.py` and
`firewalld_zone_memberships.py`. Fedora's cloud image has no NetworkManager,
and both suites need one that manages the guest's network cards. The
`enable_dnf_module_stream` branch that installs a stream is not run:
Fedora has no module streams, so the suite checks the message for a node
without them. Fedora releases before 45 and their older dnf 5 are not tested.

## Servers that systemd does not run

MMX's first SSH command asks whether systemd runs the node, and a node that
systemd does not run fails with one message
([servers without systemd](design/development.md#servers-without-systemd)).

| Check | Result |
| --- | --- |
| macOS ARM64 controller | `go test ./...` passed; the harness's own unit tests passed, 110 of them |
| An Alpine Linux 3.22 container with OpenRC, on a Debian 13 AMD64 test server | `mmx run` and `mmx dryrun` with the default connection, `mmx run --connection ssh_only`, and an inventory with `connection: ssh_only` each failed with the one message that the node does not run systemd, without and with a hand-made `/etc/machine-id`. A list of every file on the node, with its mode, owner, size, and time of change, was the same before and after the four commands |
| Incus guests, Debian 13 and Ubuntu 26.04 | `vm.py test`, `access.py`, `limited_control.py`, `controller_machine_as_node.py`, `node_facts.py`, and `linux_code.py`: all passed, in containers |

## Not verified for this build

- pg_auto_failover with more than two nodes, Citus formations, and a
  formation on Hetzner Cloud servers without a private network, where the
  monitor's `pg_hba.conf` lets `autoctl_node` in only from its own address;
  `stanza-upgrade` after a major upgrade of PostgreSQL, and a stanza on a
  replica; a `listen_address` on a WireGuard interface at boot, with
  the drop-in that the node's page shows. Go tests cover the stanza's
  upgrade and its refusal on a replica.

- A controller process in a container that shares its server's boot ID,
  such as a Docker container, against that server: the Go tests of the
  reboot rule cover each case, and the suite asks the rule on a container
  guest, but no suite runs a controller process in a Docker container.
- A macOS machine in the inventory: a Go test with a stand-in `ssh` covers
  the refusal.
- Yarn 2 and newer (`--immutable`, `workspaces focus`); the suite uses Yarn 1.
  Go tests cover the command choice.
- Quadlet files of the kinds `.kube`, `.image`, and `.build` on a node; Go
  tests cover their name rule, the Podman version each needs, and the
  check, with a stand-in for Podman's generator. The suite starts
  `.container`, `.volume`, `.network`, and `.pod` files.
- Podman 4.4 to 4.8 and 5.0 to 5.3, which no supported distribution ships.
- MySQL from the vendor's own repository, as on Debian, which packages
  only MariaDB; and a second PostgreSQL server on
  one node, chosen with `database_server_port`.
- A database server that hides stored passwords from its administrator, as
  some managed database services do.
- certbot's DNS plugins other than `rfc2136` against their services, certbot
  from snap or pip, and a certificate authority's real limit on requests;
  the suite's authority refuses a request by its own rules.
- Docker Compose between 2.26.1 and 2.40.3 and from 5.0 to 5.5 in a suite;
  the form of their dry run's lines was measured by hand on ten versions.
- A reboot of a node with Quadlet services and with an account's lingering
  services.
- An image for several platforms pulled with Podman from a registry; Go
  tests cover it against a stand-in. The Docker suite covers it with the
  test registry.
- An image from a public registry with its own login service, such as
  Docker Hub or GitHub's registry, in a suite; the suites pull from the
  test image registry on the guest.
- A node job worker process killed during a pull on a real node. Go tests
  cover removal of leftovers from a node job worker process killed during
  a pull. `release_directories.py` creates a leftover directory.
- A Debian or Ubuntu server that installs no recommended packages, for
  Ubuntu, on a real server: Hetzner's Ubuntu images install them. Incus
  guests with the setting cover it.
- ZFS on Debian 12 in a suite: Debian 12 needs a kernel module build of
  four to seven minutes. `zfs_dataset` was measured with ZFS 2.4 by hand,
  and Go tests cover what ZFS 2.4 prints. Guest-suite coverage is recorded
  in the
  [platform validation](#platform-validation-on-the-four-target-distributions).
- A real disk for `create_partitions_on_empty_disk` and the LVM modules:
  every disk of the suites is a loop device. A thin pool and an ext2
  filesystem were measured by hand and are covered by Go tests, not by the
  suite.
- An Incus virtual machine instance, and so its clean shutdown, outside
  Ubuntu 26.04 on AMD64: the Incus packages of the other distributions bring
  QEMU only with `incus-agent`, or bring no QEMU. A Hetzner Cloud server
  has no `/dev/kvm`.
- A libvirt domain with an operating system that reacts to a request to
  shut down: the suite's domain that ends by itself is a kernel without a
  root filesystem.
- A RabbitMQ cluster of several nodes, and a virtual host that holds queues
  and messages when it is deleted.
- For `mongodb_user`: a connection with TLS, a sharded cluster, and `mongosh`
  versions between 2.2.0 and 2.13.0 on a node.
- `libvirt_domain` with a domain that runs, on a Hetzner Cloud server: such
  a server has no `/dev/kvm`. The whole test ran in the VM guests of the
  Debian 13 AMD64 test servers.
- The tools of the official extra modules where their vendors publish no
  ARM64 packages: MongoDB
  on Debian; and a MongoDB server on Linux 6.19.0 through 7.0.13, where it
  refuses to start. There the suites test the modules without the tool.
- `create_filesystem_on_empty_device` refusing a device that holds LVM, RAID,
  or LUKS signatures, or a device with no signature that is in use, on a real
  node; Go tests cover them.
- The memory the xz decoder takes: the library allocates the dictionary that
  the file's header asks for, up to 1.5 GiB, and offers no cap. An xz file
  with a supplied SHA-256 is checked before decompression; downloads can
  omit the checksum with `dangerously_skip_checksum: true`.
- A real monitoring application deployed with this build, and the controller performance measurement.
- `for_each_node` loops over more than two real servers; Go tests cover
  larger loops, batches, and percentages.
- A monitoring tool reading the record of the last successful run.
- `git` over HTTPS with a username and token against a real authenticated
  server, and an `unpack_archive` fetched from a real HTTPS URL on a node; Go tests
  cover both paths.
- A `reboot` whose node never returns, and a refused reboot request, on a real
  node; Go tests cover both.
- Nodes that accept passwords only through keyboard-interactive/PAM
  authentication; native SSH disables it by design.
- Full-disk and interrupted-write faults, broader competing-controller
  matrices, large fleets, and cross-release upgrades are outside this build's
  acceptance scope.
