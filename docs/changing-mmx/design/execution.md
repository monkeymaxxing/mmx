# MMX: execution and recovery

Part of the [main plan](README.md). Resources are inspected again on resume;
service records and command records remember which configuration and software
were loaded. The
[implementation rules](../implementation-rules.md) own locking, persistence,
uncertainty, and transport mechanics. The execution and recovery design
owns the behavior implemented by the implementation rules.

## One local execution engine

The controller process runs on the controller machine. On each node, the
installed node HTTPS API process handles normal connections; native SSH handles
setup and fallback. HTTPS and SSH share node job handling code and a node
task execution engine:

```text
CONTROLLER MACHINE
  Controller process: mmx
      |
      | authenticated HTTPS to the node HTTPS API process
      | or verified SSH to an invocation of the node runner executable
      v
MANAGED NODE
  Shared node job handling code: accept the request and save its job identity
      |
      | start a separate process for this node job
      v
  Node job worker process: run the node task execution engine
      |
      | execute ordered node tasks, hold the node lock, save progress and results
      v
  Node job records: available to the controller process through HTTPS or SSH
```

The node HTTPS API process authenticates requests, accepts node jobs, and
returns results; it has no second implementation of resource changes. A
**node job** is one attempt to execute one fleet stage's work on one node.
The **node job worker process** exits after the node job. Losing the
controller connection or restarting the node HTTPS API process (a systemd unit with
`KillMode=process`) does not kill accepted work. With `connection: ssh_only`, the node
job worker process outlives the SSH session like any detached process, which
relies on logind's `KillUserProcesses=no`, the default on the supported
distributions. There is no persistent controller service or message broker.

## Bootstrap automatically, then use the verified connection

When `dryrun`, `run`, or `run-command` needs an MMX installation, bootstrap
uses existing SSH key or password access and OpenSSH's verified node
identity. Bootstrap installs the node runner executable from the controller
process's MMX release. Bootstrap then configures the node HTTPS API process
and checks that direct HTTPS works.
Bootstrap runs as part of the requested command, with visible progress and
no separate preparation step. Dryrun can set up MMX itself without changing
declared resources. Repeated commands reuse a correct installation and
finish interrupted setup.

The account of the first login is whatever the server's provider created:
`root`, or an ordinary account with sudo such as `ubuntu`, `debian`, or `rocky`.
The inventory field `ssh_user` names the login account.
[Privilege](#privilege-has-one-ordinary-path) describes root login and login
through an account with sudo. Bootstrap needs nothing installed on the
node beforehand: bootstrap copies the node runner executable over SSH.

On a node with SELinux, the node runner executable and the unit file of the
node HTTPS API process are installed at paths that the distribution's SELinux
policy labels as programs and as unit files, and they carry those labels after
every [update](#node-updates-follow-the-controller-release). systemd can then
start the node HTTPS API process while SELinux is enforcing, and MMX ships no
SELinux policy of its own.

MMX uses ordinary reachable addresses; an overlay network such as WireGuard is
optional, and TLS stays enabled over it. Network membership alone never
authorizes MMX operations.

Connection selection happens beneath `dryrun`, `run`, `resume`, and `run-command`.
The inventory field `connection` defaults to `https_with_ssh_fallback`;
`ssh_only` uses SSH for all node communication. The command-line option
`--connection ssh_only` makes SSH the choice for every node of one
invocation, for a controller machine that cannot reach the nodes' HTTPS
addresses ([CLI](workflow.md#choose-the-inventory-limit-fleet-stages-and-extra-variables)). In the default mode, only a reachability failure falls back to verified SSH; an authorization failure
is reported, and an identity mismatch sets the node up again (below). The
[transport rules](../implementation-rules.md#transport) define
exactly which failures fall back and how uncertain submissions are reconciled.

### Administrator SSH keys authorize HTTPS

Administrative access to a node means full MMX access to that node. The node's
existing administrator SSH keys authorize both SSH and HTTPS. There is no
separate MMX administrator list or restricted-admin permission model; different
teams administer different nodes through ordinary node access.

Verified SSH establishes the node HTTPS API process's TLS public-key pin.
Certificates renew with the same server key, so users never operate a CA.

Each HTTPS connection authenticates once, as an SSH login does. The TLS
handshake first verifies the pinned key. The controller process then signs
the connection's exported keying material (TLS 1.3) with the administrator's
private key or SSH agent. A captured proof cannot be replayed on another
connection.

The node HTTPS API process serves nothing before authentication.
Administrator private keys stay on the controller machine.

At authentication the node HTTPS API process checks the account's current
authorized key files, unrestricted root or sudo access, account expiry,
and the effective `sshd -T` allow/deny policy. Adding a key to ordinary SSH
access grants MMX access; removing the key refuses the next connection.
SSH setups the node HTTPS API process does
not evaluate itself use explicit `connection: ssh_only`; the
[Connect to your servers](../../how-to/connect-to-your-servers.md) page lists them.

Password-only initial access is supported. With no usable controller key,
setup creates a per-destination key in the user's SSH storage, outside project
history. Setup adds the key to the administrator's SSH access. Interrupted
setup reuses the generated key.

Saved HTTPS identity is verified public connection information, not a second
trust store. When a node no longer matches the saved HTTPS identity, MMX
sets the node up again through verified SSH. OpenSSH decides whether to trust a
replaced server, then MMX saves the new HTTPS identity and tells the user.
Resume refuses a replaced machine, because the saved playbook run's
progress describes the previous machine.

Each connection learns which machine it reached and in which boot: the
SHA-256 of the systemd machine ID and the kernel's boot ID, from MMX's first
SSH command or from the node HTTPS API process. Two inventory names that
reach one machine in one boot fail: the first name keeps the machine, and the
other name's node job fails before anything is sent, with both names and the
advice to check their addresses. Without the check, a typo in an address
runs two names' node jobs on one server, both succeed, and the server the
second name means is never touched. Two names that give the same address
and port are a choice the inventory shows, such as a load balancer on an
application server, so both keep the machine. Virtual machines cloned from one image
share a machine ID but each boots its own kernel with its own boot ID, so
clones stay separate nodes. Some containers (not those on Incus with lxcfs) share
the server's boot ID, so they need different machine IDs. The check covers the
connections of one controller process: node jobs and the fact reads of a
fleet stage's `when`.

Limited capabilities for other users belong to
[operating-system controls](workflow.md#give-non-privileged-users-limited-controls)
such as systemd path units, not to restrictions on administrators.

Module pages that rely on this section:
[`authorized_keys`](../../reference/modules/authorized_keys.md).

Administrator keys load once per controller process. HTTPS authentication
tries the last accepted key first, then the remaining loaded
keys. Loading keys once avoids reloading keys for every node while
permitting several administrator keys.
Loading can require at most one extra passphrase prompt per encrypted key
per command. HTTPS certificates allow five minutes of clock skew before
their creation time.
A controller machine farther behind the node fails certificate validation;
keep the controller machine's and node's clocks synchronized rather than
weakening TLS checks.

### A blocked port 7443 is reported, never opened

The node HTTPS API process listens on port 7443. A freshly installed server
can have a firewall that refuses port 7443. MMX never changes a node's
firewall to let itself in. Opening a port is the administrator's decision,
written as a firewall rule in the project.

When the HTTPS check fails, MMX notes what it saw: the address it tried and,
in plain words, what happened, such as `MMX could not connect to
https://10.20.0.2:7443 (connection timed out)`. The node, reached over SSH,
then reports on its own side. The notice calls the
node HTTPS API process "mmx-runner, MMX's HTTPS service on this node", so
that a reader who has not met the term knows the program:

- The node HTTPS API process does not run: MMX starts its systemd unit,
  `mmx-https-api.service`, through verified SSH, as at boot, says so, and
  checks HTTPS again. Starting the unit is safe while a node job runs,
  but setup is not.
  When the start fails, the run continues over SSH, and the notice
  gives the node HTTPS API process's state and the failure. If the node
  HTTPS API process stops during a command, MMX switches the command's node
  communication to SSH. The next command starts the node HTTPS API process
  again.
- The node HTTPS API process runs: the run continues over SSH. MMX says once
  for the node with unreachable HTTPS, in the command's output and in the
  structured results, what MMX saw. The notice says that the node HTTPS API
  process runs and that MMX is using SSH. When ufw or firewalld is
  active on the node and none of its rules names port 7443, the notice says
  that too. The notice then says how to use HTTPS. Check that the controller
  machine can reach the host name or address and TCP port in the node's
  HTTPS URL. The network may be down, or a firewall may block the port.
  The notice names a network failure and a firewall block as possible
  causes, without choosing a cause. The notice also says how to make SSH
  the stated choice:
  `connection: ssh_only` for the node in the inventory, or
  `--connection ssh_only` for the command. The [`ufw`](../../reference/modules/ufw.md)
  page shows a rule that allows the port as
  `{action: allow, port: 7443, protocol: tcp}`.
- The node cannot answer the HTTPS diagnostic request sent over SSH: the
  notice gives what MMX saw and why the diagnostic request failed.
  MMX continues the run using SSH for node requests.

The notice names no cause that MMX did not check. A failed connection can
come from a firewall on the node or between the machines, a network
interface that is down, such as a WireGuard interface, or a wrong address,
and MMX cannot tell these apart from the controller machine. A ufw rule that
names port 7443 can still be limited to one interface or one source, so MMX
reports only the case where no rule names the port.

The node job result records `connection_used: ssh_fallback` and the notice's
text in `connection_fallback_reason`. Otherwise, `connection_used` is
`https`, or `ssh` for a node whose inventory says `ssh_only`.
The node's firewall never changes. The notice repeats on each run until
HTTPS works or the inventory says `connection: ssh_only`.

Module pages that rely on this section:
[`firewalld`](../../reference/modules/firewalld.md), [`ufw`](../../reference/modules/ufw.md).

### A node can refuse management

When the file `/etc/mmx/do_not_manage` exists on a node, that node refuses
every node job that could change it: jobs from `run`, `resume`, and
`run-command`. The refusal happens before the node's first task. The node
fails in its fleet stage, with the path of the file and the file's text as
the reason, and the fleet stage's
[failure policy](#choose-what-a-node-failure-stops) applies as for any failed
node. A refused node is a failure, not a skip, so a run that did not configure
a selected server never ends as a success.

An administrator creates the file by hand, for example with one line that
says who is working on the server and until when, and removes it by hand. MMX
never creates or removes `/etc/mmx/do_not_manage`. `dryrun` still inspects
the node and reports that a run would be refused, with the file's text.
Bootstrap and node runner updates still happen, because they change no
declared resource.

## Keep the native SSH fallback small

Use the native OpenSSH client with no local shell around the client.
OpenSSH handles SSH client configuration for the node, SSH agents,
certificates, and jump nodes.
[OpenSSH configuration](https://man.openbsd.org/ssh_config). Job and setup
payloads go over stdin. The remote command holds only the node runner
executable invocation, fixed-format identifiers, and the quoted
`mmx_runner_download_url`. One multiplexed connection serves a node's
ordinary work. A
[reachability confirmation](#reachability-confirmation-and-owed-restore)
is a request the controller process sends over a new connection for one
change of one node job. Reachability confirmation detects a change that
blocks new connections while an existing connection stays alive.

SSH and sudo passwords follow the
[setup workflow](workflow.md#set-up-nodes-automatically). `ssh_password` and `sudo_password`
use whole-value secret references in the inventory, for example
`ssh_password: "{{ .secrets.ssh_password }}"` and
`sudo_password: "{{ .secrets.sudo_password }}"`. The controller process resolves
the SSH and sudo password references in memory. The resolved passwords
never enter saved inventory, node jobs, results, node HTTPS API
configuration, or command-line arguments. Host-key checking is never
disabled: an unknown identity is a setup error before secrets or changes are
sent.

Native SSH disables keyboard-interactive authentication because those prompts
come from the managed node. Password-only setup uses OpenSSH's own password
prompt; a node that accepts passwords only through keyboard-interactive/PAM
needs key access. If a node set up with a password is rebuilt with key-only
access, remove its old per-destination key from `~/.ssh/mmx/` before setup,
so setup can use the rebuilt node's working key.

The managed key's file name and the node's saved identity in `.mmx/nodes/`
are both the SHA-256 of the address, the SSH port, and the SSH account. The
port stays in the name because servers behind one address can differ only by
port, and each needs its own saved identity. When the name has no key and the
node uses MMX's own key, `adoptManagedKeyAfterPortChange` looks in
`~/.ssh/mmx/` for a key of the same address and account under another port.
It connects with verified SSH (OpenSSH checks the host key for the new port),
offering only that key, and reads the machine ID as the SSH probe does. Only
when that machine equals the node ID in this project's saved identity for the
old name does it copy the key files to the new name and print one notice that
names both ports. A different machine, a missing old identity, or any failure
adopts nothing and behaves as before. The files are copied, never linked, so
removing one name never removes another name's key. `remove-mmx-from-node`
also removes every copy of the same public key in `~/.ssh/mmx/`, with the
saved identity under each copy's name. The saved
identity is not copied: setup through verified SSH writes the new one.

## Privilege has one ordinary path

The node HTTPS API process and node runner executable run as root, like a
Salt minion, Salt's program that runs jobs on a managed node.
System administration therefore needs no per-task escalation settings.
A node job submitted over authenticated HTTPS needs no SSH login or sudo
password. MMX does not subdivide administrator access by task, project, or
resource.
[Salt minion user](https://docs.saltproject.io/en/latest/ref/configuration/minion.html#user).

Bootstrap and SSH fallback use the server's existing administrative access: root
directly, otherwise sudo with or without a password. Users need not change their
sudo policy.

MMX executes node tasks as root. A command that belongs to an application
account names the application account
([run a command as an application user](../../reference/modules/shell.md#running-as-another-account)).
File owners and service users do not change execution identity.
There are no per-task `become` settings or escalation backends.
MMX does not forward SSH agents or decryption keys from the
controller machine to managed nodes.

Module pages that rely on this section:
[`shell`](../../reference/modules/shell.md).

## The controller machine as a node

A Linux controller machine can manage itself. The Linux controller machine
appears in the inventory as an ordinary node, reached over SSH:

```yaml
nodes:
  controller_machine:
    address: 127.0.0.1
    ssh_user: your_admin_account
```

`your_admin_account` is an existing administrative account on the Linux
controller machine: root, or an account with full sudo access. The machine
needs an SSH server, and its SSH host key is checked like any node's.
MMX connects, installs the node runner executable, and runs node jobs on
the ordinary path; the node
runner executable runs as root, and the controller process needs no root
privileges. The controller process and the node programs have separate roles
and state on the Linux controller machine. The controller process's run
history and locks are in the project's `.mmx/`. Node history and locks are
in `/var/lib/mmx/`.

Modules, dryrun, locking, saved results, and `mmx resume` work as on any node.
The node HTTPS API process also works on the Linux controller machine, on
port 7443, so `connection` is chosen as for any node.

There is no local execution mode, no third connection, and no special module
for the controller machine; MMX stays one model for every node. Most teams
run MMX from a laptop or a CI machine that MMX does not manage. Managing the
Linux controller machine as a node is an option for teams who prefer
to manage the machine running `mmx`.

A macOS controller machine cannot be a node: the node runner executable is
Linux only. MMX refuses a machine when the operating-system name obtained
over SSH is not `Linux`.
The refusal names the system that answered, such as `Darwin` for macOS, and
says that MMX manages Linux nodes only.

### MMX never reboots the controller machine

A `reboot` task would end the controller process that waits for the node to
come back, so MMX refuses it on the machine that runs the controller process:

> MMX does not reboot the controller machine, the machine where this mmx
> command runs. Run the playbook from another machine, or reboot this machine
> by hand after MMX finishes.

MMX does not compare inventory addresses. The guard applies whether the
node being considered for reboot is reached as `127.0.0.1`, by host name,
or by another address. The node runner executable compares the node's
kernel boot ID (`/proc/sys/kernel/random/boot_id`) and systemd machine ID
(`/etc/machine-id`) with the controller machine's IDs.
A reboot is refused when the boot IDs match and either
the machine IDs also match or the node is not a container
(`systemd-detect-virt --container`):

- When the node is the controller machine, the node's boot ID and machine
  ID match the controller machine's IDs: refused.
- A controller process in a container, such as a CI job, shares the boot ID
  of the server running the container. The controller container has a
  machine ID different from the server's, or no machine ID. Rebooting the
  server running the controller container would end the controller
  process: refused.
- A container on the controller machine shares the controller machine's
  boot ID, but rebooting the container leaves the controller process
  running: allowed. Rebooting a container when the controller process runs
  in another container on the server is likewise allowed.
- Virtual machines cloned from one image may share a machine ID, but each
  boots its own kernel with its own boot ID: allowed.

What a Docker container sees is measured (Docker 26.1.5 on a Debian 13
server, the `debian:13` image): the container reads its server's boot ID,
the same value in every container, after a restart of the container, and
with `--privileged`. The image has no `/etc/machine-id`; installing systemd
in the container creates one that differs from the server's. And
`systemd-detect-virt --container` answers `docker` in the container and
`none` on the server. So the rule's second and third case hold for Docker as
written, and a controller machine without a machine ID sends none: the
machine IDs then never match, and the node decides by whether it is a
container.

The controller process checks before a fleet stage's node jobs start, as soon
as it knows the node's boot ID and machine ID, so a playbook that would
reboot the controller machine fails before it changes anything on that node.

The node runner executable checks again just before `systemctl reboot`. There
is no setting that overrides the guard: running the playbook from another
machine keeps the ordinary reboot-and-continue workflow. The guard covers the
`reboot` task only; MMX does not inspect scripts or commands that an
administrator writes.

The first check looks at what the fleet stage holds, not at what a run would
do. A fleet stage that ends with a `reboot` task is refused for the
controller machine whatever the task's guards say, in a dryrun as in a run.
The node job on the controller machine fails with the message above. Other nodes
continue according to the fleet stage's failure policy.
A project that shares one playbook between the controller machine and
other nodes leaves the controller machine out of the fleet stage that
reboots, with `except_nodes`.

The rule is in one place, on the node, which knows its own boot ID, its
machine ID, and whether it is a container. The controller process sends the
controller machine's boot ID and the SHA-256 of the controller machine's
machine ID to the node. The request asks whether the node must refuse a
reboot that would end the controller process. The controller process asks
before the node job starts. The node job also carries the controller
machine's boot ID and machine ID hash, so the node can apply the rule again.

A node that cannot read what the rule needs fails the node job instead of
rebooting. A Linux controller machine that cannot read its own boot ID
fails the node job
too, before it asks or sends anything: with an empty boot ID no node would
recognize it, and nothing would say that the guard is off. A controller
machine that does not run Linux, such as a Mac, has no boot ID and is never
the node.

The reboot guard cannot recognize the server running a controller process
inside an LXC or Incus container, because the boot IDs differ.
An LXC or Incus container has a separate boot ID. LXC replaces the kernel's
boot ID file with a file containing a new ID at each container start.
When the controller process runs inside an LXC or Incus container, the
container's server looks like a different machine to the guard. MMX then
allows a playbook to reboot the server running the controller container,
ending the controller process.

## Node updates follow the controller release

The controller process installs the node runner executable from the
controller process's MMX release. Setup receives the node runner executable
into a private staging file. Setup checks the staged executable's SHA-256
against the hash sent over SSH by the controller process.
Under the node lock, setup switches atomically, never running a partial or
user-writable binary.
While a node job is active on the node being updated, setup reports the
node busy instead of replacing the executable.

The controller process refuses a node runner executable of another MMX
release than `mmx`. The node HTTPS API process reports its release version
with its identity. Over SSH the controller process asks `mmx-runner version`,
once per node runner executable file and controller process, because the same
bytes report the same version. A different version fails the node before any
node job is sent, and the failure names both versions and the file.
Without the check, replacing only `mmx`, or a controller process that reads
an old release's node runner executables, would leave the new release's
module fixes off the nodes, and nothing would say so. Every development build
reports `0.1.0-dev`, so development builds match one another, as the
[official extra modules' version check](modules.md#official-extra-modules)
also does; the check compares release builds. A development build has no
other identity that both programs can report: `-trimpath` leaves the
`-ldflags` value out of Go's build information, and a source revision does
not change with uncommitted edits.

An [owed restore](#keep-the-owed-restore-until-it-is-resolved) is a durable
node record requiring a module-owned file to return to its previous state
unless reachability is confirmed. An owed restore also prevents replacement
of the node runner executable. An owed restore prevents replacement even
after the node job worker process that created the owed restore has ended.
Interrupted setup is finished by repeating the command.

Inspecting or resuming a saved run can reinstall the node runner executable
build recorded in the run and restart the node HTTPS API process.
The runner cache in `.mmx/runners/` keeps the recorded build. Recorded node
jobs keep the executable build used for execution. The release version check
covers a recorded runner build too, so a playbook run started by another
release is resumed with `mmx` of that release. An upgrade of `mmx` may
discard the saved runs and the runner cache, as
[compatibility between releases](README.md#compatibility-between-releases)
says.

Setup transfers the node runner executable over SSH, gzip-compressed,
unless the node names a `mmx_runner_download_url`. With a download URL, the
node downloads `<mmx_runner_download_url>/mmx-runner-<sha256>` into the
private staging file used for installation. Setup checks the downloaded
executable against the SHA-256 hash sent over SSH, as for an executable
transferred over SSH. A mirror near the nodes lets a large fleet avoid
sending one copy per node from the controller machine. A failed
download fails setup rather than silently falling back to SSH.

## Freeze the work, then dispatch it

`mmx run deploy` selects `playbooks/deploy.yml` in the project that contains
the current directory; the
[project settings contract](configuration.md#project-settings) owns how the
project root, playbook, inventory, and file paths are found.

Before the first change, the controller process resolves the selected
inventory, role inputs, static references, and local asset paths, and
validates what it can know statically. Runtime facts and earlier results in
`.node_tasks.<id>` are resolved and validated where they are used.

A saved run bundle is JSON instructions plus a separate asset directory.
The saved run bundle holds:

- Run identity: the project, playbook name, and inventory name.
- Mode, selected nodes after any `--limit`, fleet stages, and rollout limits.
- Definitions and files: the parsed tasks and exact project, template, and
  asset bytes.
- Inputs: non-secret resolved inputs, including `--var` command-line
  variables, and references to secret sources.
- Execution settings: resolved timing and node task retry settings.
- An asset manifest: the assets' sizes and hashes.

Each entry for a fleet stage and node records the runner hash it used once
it reached the node. Resuming the entry uses the same runner.

File contents travel separately from the JSON instructions, so large files
and module executables need no size override:

- Source files and templates are copied into private run assets. Files of
  modules that are not core, including scripts, executables, supporting
  files, and descriptions, are also copied into private run assets.
  The controller process hashes the input byte stream used to copy each
  asset. Users write no manifests or checksums.
  Each asset is kept once per run; each node job names only the assets its
  tasks need.
- Assets stream over authenticated SSH or HTTPS into private staging and are
  verified against their expected size and digest before use. A job is admitted
  only after all its assets are verified. Transfers use bounded buffers and show
  aggregate progress; stall detection replaces a whole-request deadline. Module
  staging accepts only contained regular files.
- A node keeps module files and files from a web address that `download`
  and `unpack_archive` tasks install or unpack. Kept files are stored by
  SHA-256 in `/var/lib/mmx/kept_files/`. A run or dryrun sends only missing
  module files and downloaded files. Before submitting a node job, the
  controller process names the required module files and downloaded files.
  The node checks the SHA-256 of each already kept file, then links the
  verified kept files into the node job's files.

  The node job also lists the module files and downloaded files to keep.
  Once the node admits the job, the node keeps the newly received files
  from the list. The node job's file list is needed because only the
  controller process knows a web address supplied by a template. The node
  job does not carry the web
  address.

  A file from the project is sent each time: it may hold a secret, which the
  node keeps no longer than the job. MMX removes a kept
  file that no run has used for 30 days.
- Each executable module runs from a private working copy of its directory.
  `MMX_MODULE_HELPER` names the runner's JSON helper.
  Assets are delivered for dryrun too, so a module works on first use.
- Resume uses the saved bytes even if local files changed; a new run
  uses current files. Nodes release execution assets when a job finishes; resume
  resends execution assets.
- Secret inputs and rendered secret content never go into retained assets.
  Templates render on the node.

Remote downloads need a pinned digest, or the task's explicit
`dangerously_skip_checksum: true`. A package task (`apt_package` or
`dnf_package`) with `state: present` asks for presence, not
a repository snapshot; exact deployments set the package's `version`.

The node job worker process collects node facts once per job. Files render
on the node from the fact snapshot, assigned inputs, and earlier results.
The bundle freezes templates and configuration, not observed hardware.
Resume refreshes facts, and the non-secret fact snapshot is saved with the
job's results to explain capacity-derived changes.

Module pages that rely on this section:
[`file`](../../reference/modules/file.md).

## The node owns its execution record

One OS-backed node lock, across all MMX projects, serializes changes to a
machine. If another job holds the node lock, MMX reports that job instead
of queuing or killing the job. Reconnecting to the same job never starts
the job again. A saved request whose node job worker process has ended
resolves to an interrupted outcome rather than staying pending. The
[node-lock rules](../implementation-rules.md#node-locks) and
[persistence rules](../implementation-rules.md#persistence) own the mechanics.

Service configuration hashes live in root-owned `/run/mmx/` records, which a
reboot clears.

Job requests and results live in persistent node storage under
`/var/lib/mmx/` and survive reboot. The
[command records](#a-command-that-follows-a-change-has-the-same-record) under
`/var/lib/mmx/command_records/` also survive reboot, because a reboot does
not run an owed command the way a reboot restarts a service.

Controller run history lives in `.mmx/` at the project root.
The [workflow](workflow.md#resume-uses-the-recorded-run) owns which run a
command selects. Controller history expiry follows the
[history policy](workflow.md#saved-run-history); expiry removes the ability to
resume the playbook run whose history expired. Expiry does not touch node
service records, command records, or job results.

### A node task's private directory lives in one place that the node empties

A node task sometimes hands a tool a file that no other account may read,
for as long as the task runs:

- a login that the tool reads only from a file: `git` its user name,
  password, and SSH key; `docker` and `podman` a registry login; the MySQL
  or MariaDB client of `mysql_user` and `mysql_database` a login file, when
  the node task sets `database_server_host`, `login_user`, or
  `login_password`; and `psql` of `postgresql_role` and
  `postgresql_database` a password file, when the node task sets
  `login_password`;
- a private copy of a proposed file for a check run by an ordinary account.
  Examples are the account's own `systemd-analyze verify` of a new unit
  file, and Podman's generator on a new Quadlet file. A Quadlet file defines
  a container or related resource that Podman turns into a systemd service.
- a copy of the node's Netplan directories, in which `netplan_file` runs
  `netplan generate` on the proposed file without touching the live
  configuration;
- the files that a tool would otherwise write into a home directory: Docker
  Compose's own dry run.

The node job worker process writes such files into the node task's private
directory in `/run/mmx/private_task_directories/` on the node. When the node
task ends, fails, or is cancelled, the node job worker process removes the
private directory.

- **Only in memory.** The node requires `/run` to be a memory filesystem
  (tmpfs), so no password reaches a disk. On a node whose `/run` is on a
  disk, such a node task fails and writes nothing.
- **One account reads the private directory.** The directory belongs to
  the account that runs the tool and has mode `0700`. A node task can run
  `git`, `podman`, and a check as an ordinary account, which must reach its
  own directory. So `/run/mmx/` and `/run/mmx/private_task_directories/`
  belong to root with
  mode `0711`: every account can pass through them, and none but root can
  list them or create, rename, or remove anything in them. Everything else
  in `/run/mmx/` is closed by its own mode `0700`.
- **Root does not follow symbolic links created by the tool's account.**
  Root writes the files while the directory is still root-owned and closed.
  Root then changes ownership of the files to the account that runs the
  tool, and changes ownership of the directory last. Afterwards root writes
  nothing into the directory and only removes the directory, without
  following links.

The node job worker process is the `mmx-runner` process that carries out one
node job. When killed during a node task, the node job worker process
cannot remove the task's private directory. The kernel may kill the worker
because memory ran out, or someone may use `kill -9`.
A reboot would clear `/run`, but a server can run for months. So the node
removes every directory in `/run/mmx/private_task_directories/` whenever a
request finds no node job running.

A node runs one node job at a time.
Only a running node job worker process creates directories under
`/run/mmx/private_task_directories/`. So when a request finds no node job
running, no node task uses a private directory. While a node job runs, the
node removes nothing in `/run/mmx/private_task_directories/`.

The node looks at the start of every submit, status, progress, and stop
request:

- A password that a killed worker left is gone before the next node job
  starts on that node.
- A password left by a killed worker usually disappears sooner, when the
  controller process continues requesting node job progress. A later
  `mmx status` or `mmx resume` also triggers removal by requesting the node
  job's status.
- A tool that the killed worker had started may still run. Removing the
  private directory removes the files that the tool was given.

Module pages that rely on this section:
[`mysql_database`](../../reference/modules/mysql_database.md),
[`mysql_user`](../../reference/modules/mysql_user.md),
[`netplan_file`](../../reference/modules/netplan_file.md),
[`podman_quadlet`](../../reference/modules/podman_quadlet.md),
[`postgresql_database`](../../reference/modules/postgresql_database.md),
[`postgresql_role`](../../reference/modules/postgresql_role.md).

### The node records each playbook's last successful run

Monitoring needs to see a server that nobody has run the main playbook on for
a long time. After a `run` or `resume` of a playbook, each node on which every
fleet stage that selected it succeeded writes one JSON line to
`/var/lib/mmx/last_successful_run/<project_name>/<playbook>`. The record
means only "this playbook last succeeded on this node then", never "this
server is fully configured": each playbook has its own file, so a playbook
that restarts one service does not refresh the record of the playbook that
configures everything. The user chooses which playbook's record a monitor
watches.

```json
{"run_id": "20261005T142233.104233000-7f3a9c1e4b2d6a08", "inventory": "production", "limit": "", "only_fleet_stages": "", "finished_at": "2026-10-05T14:25:10Z"}
```

`limit` and `only_fleet_stages` hold the run's `--limit` and
`--only-fleet-stages` values, empty when the run had none, so a reader can
tell a whole run from a partial one. A failed, stopped, or refused node keeps
its earlier file. `dryrun` and `run-command` never write the file.
Writing the playbook's last-successful-run file is not reported as a change.
MMX only writes the last-successful-run record. A monitoring tool that
reads MMX records reads the file's age and owns the alert.

## Node task blocks complete the change

For the [Nginx example](configuration.md#roles-describe-reusable-server-configuration),
the desired outcome is a configured, running Nginx that has loaded its current
configuration and passes its health check:

```text
inspect services; save missing configuration baselines before changes
    ↓
if a running service would be restarted or reloaded:
run any declared checks_before_restart_or_reload
    ↓
inspect/reconcile related resources
    ↓
run any declared node task block validation
    ↓
ensure service enabled/running; refresh if required;
run each command that follows a change, if required
    ↓
verify health
```

Validation runs automatically when declared. A project may omit validation
or declare validation in two places:

- The `check_new_file` field on `file`, `download`, `line`, and `edit_file`
  checks a temporary new file before it replaces the live file; rejection
  leaves the live file intact. The
  [file-module contract](modules.md#check-new-files-before-installing-them)
  defines the `check_new_file` field's `{shell: {...}}` form.
  `.new_file.path` names the new file.
- A node task block's `validate` section checks the combined configuration
  before activation. Use `validate` when several files contribute to the
  configuration. A single-file change can use the file validator alone.

A declared validator must pass. Exit zero is success. Any other outcome,
including failure to start the validator, fails the node task block before
activation. The usual failure handling and cleanup then run. On resume,
declared checks run again before pending activation even when files are
unchanged. Validators report no change. During dryrun only explicitly
declared non-mutating checks run; otherwise validation
is reported as not checked, with the reason. Such a validator reports change
`unchanged` while no earlier node task of the dryrun on the node would change it,
so a dryrun of a block that would change nothing can still end with nothing
to change.

A dryrun checks the node as it is now, and a run checks it after the earlier
node tasks have changed it. A failed dryrun check is reported as
`not_checked` when an earlier node task on the node reported `would_change`
or was not checked. The earlier node task may belong to the check's fleet
stage or an earlier fleet stage on the node.

A node runs fleet stages in order. The controller process passes the first
earlier node task that would change the node or was not checked to the next
node job. The earlier node task comes from the node's completed fleet stages.
The failed check's reason names the earlier node task and says that the run
checks again after the earlier task's change. The check's result keeps the
check's exit code and output.

A missing prerequisite that a dryrun reports as `not_checked`, such as a
parent directory, a program, or an account, gives the module's own reason
and advice first. It then names the same earlier node task, which may
provide what is missing, and says to change nothing if the run succeeds.
A dryrun cannot know what an earlier node task creates, such as a directory
that a package installs. The module's advice alone could lead a reader,
often a coding agent, to add a node task for a directory that the package
creates anyway.

The rule for reporting a failed dryrun check as `not_checked` applies to:

- `check_new_file` and a module's own check of a new file.
- Every node task in `validate`.
- A node task that only reads the node: `wait_for`, `http` with `GET` or
  `HEAD`, and `shell` with `changed_when: false`.

The failed-check rule excludes `assert`, because `assert` judges values.
For example, `assert` judges the approval requested by
[checks before a restart or reload](#checks-before-a-restart-or-reload).
When no earlier node task would change anything, the check's failure stands.
A run is unaffected. For example, on a fresh node, a `user` task would create
the account `web`, and a later `file`
task's `check_new_file` runs
`logrotate --debug`, which fails with `unknown user 'web'`: the dryrun reports
the file not checked, and the run checks it after creating the account.

A node task block is the unit of activation, not a filesystem transaction: files
written before a failed validation stay written, and MMX does not roll them
back. MMX only refuses to activate a known-invalid result.

A **tracked input** is something a node task block's `changes` section
declares and MMX remembers by its hash: a file from a `file`, `line`,
`edit_file`, `download`, or `symlink` task or from a `directory` task with a
source tree; a unit file from a `systemd_unit` task; a package from
`apt_package`, `dnf_package`, or one of the language
installer modules; the commit that a `git` task checked out; the image ID
from a `docker_image` or `podman_image` task; a Quadlet file from a
`podman_quadlet` task, which counts as the service's definition like a unit
file; the file of a `cron_job` task; and, for a `directory` task that
deletes, the absence of the directory (`state: absent`) or whether it exists
and the names of the entries directly inside it
(`delete_everything_inside_except`, which creates a missing directory). The block's
`activate` services are keyed by canonical systemd unit on that node, not by
task or node task block names; a unit in an account's own systemd
(`in_systemd_of_user_account`) is keyed by the account and the unit. Several
services can share one input set, and several node task blocks can manage one
service: the record keeps a hash per input and each node task block compares
only its own. Tracked arguments and service targets resolve once, in the node
task block's entry scope, and those tasks run with exactly the hashed values;
the [validation rules](../implementation-rules.md#validation-follows-execution)
own the details. MMX does not guess which other files a daemon includes.

A file, link, or unit file has one owner in a node task block: two tracked
tasks that would both run on a node and write the same path are an error.
Several `line` and `edit_file` tasks may edit one file. As the block starts,
the node evaluates the `when` of each tracked task. A task whose `when` is
false then cannot conflict, so one `download` for each processor
architecture, each with a `when` on `.node.arch`, may install the same path.
The path is hashed as the task that runs writes the path. A task whose
`when` is false as the block starts needs no arguments that resolve. While
the condition is false, the task writes nothing. A value read only by the
task, such as a role input that another kind of server gets, need not exist.

If the arguments cannot resolve at block entry, the block fixes and follows
nothing of the task whose `when` is false. If a task inside the block makes
the excluded task's `when` true, the excluded task fails with a
configuration error. Set the value before the block to avoid an untracked
change. A task whose arguments resolve stays tracked and evaluates `when`
in written order.

A `when` that the node cannot decide yet, such as a condition on an earlier
task's result inside the block, counts as true. Validation on the controller
machine rejects two conflicting tasks only when both tasks' `when`
conditions are known to be true. A condition on facts is decided on the node.

A custom command in `activate` runs on every run, in written order, unless it
says `only_when_tracked_inputs_changed: true`; then it follows the block's
tracked inputs
[as a service does](#a-command-that-follows-a-change-has-the-same-record).
MMX does not infer a command's side effects or build a separate set of
notifications from changed resources to activation commands. Keep resources
with different service requirements in separate node task blocks:
files that need a restart and files that need only a reload go in two
blocks that name the same service.

Module pages that rely on this section:
[`download`](../../reference/modules/download.md), [`shell`](../../reference/modules/shell.md).

### Checks before a restart or reload

Some restarts hurt: restarting a Redis cache empties the cache, and
restarting a database primary drops every connection. A routine run must
not restart a running service by accident because someone changed one line
of a template.
A node task block can therefore declare `checks_before_restart_or_reload`.
The list holds node tasks that must all pass before the block restarts or
reloads a service that is running now.

The role `roles/redis_cache/main.yml` declares a boolean input,
`restart_approved`, and the playbook passes it from a command-line variable:

```yaml
# roles/redis_cache/main.yml
inputs:
  restart_approved:
    type: boolean
    required: true

node_tasks:
  - task_block:
      id: redis_cache
      changes:
        - file:
            permissions_mode: "0644"
            path: /etc/redis/redis.conf
            template: files/redis.conf.tmpl
      checks_before_restart_or_reload:
        - assert:
            that: "{{ .inputs.restart_approved }}"
            message: >-
              Redis must restart to load this change, and a restart empties
              the cache. Run again off-peak with --var restart_approved=true.
      activate:
        - systemd_service:
            name: redis-server
            refresh: restart
            state: running
```

```yaml
# playbooks/cache.yml
command_line_vars:
  restart_approved:
    type: boolean
    default: false

fleet_stages:
  - id: redis_cache
    nodes: cache

    node_tasks:
      - role: redis_cache
        inputs:
          restart_approved: "{{ .vars.restart_approved }}"
```

`mmx run cache` on a day when `redis.conf` is unchanged never runs the check.
On a day when the template changed, the block fails on each cache node with
the message, and nothing is written. `mmx run cache --var
restart_approved=true` writes the file and restarts Redis.

The rules:

- `checks_before_restart_or_reload` runs only when applying the block would
  restart or reload a service that is running now. MMX decides whether a
  restart or reload is needed by using the dryrun comparison. MMX compares
  what the block's tracked tasks would install with the service's record.
  `checks_before_restart_or_reload` also runs when a restart is
  still owed from an interrupted run. A stopped service, or one that does
  not exist yet, is started without the checks, and an unchanged run does
  not run the checks.
- `checks_before_restart_or_reload` runs after MMX saves the service
  baselines. It runs before the block's first write when MMX can foresee
  the restart or reload; otherwise it runs just before the restart,
  reload, or command that needs it.
  When a check fails, the block fails with the failed check's message and
  stops before the restart or reload. `on_failure` and `cleanup` run as
  for any failed block. When the check ran before the first write,
  nothing in the block was written.
- `checks_before_restart_or_reload` holds only tasks that change nothing:
  `assert`, `http` with `GET` or `HEAD`, `wait_for`, `show`, and `shell` with
  `changed_when: false`.
  Validation rejects any other task in `checks_before_restart_or_reload`.
- A command with `only_when_tracked_inputs_changed: true` counts like a
  service: the section runs when that command would run.
- Dryrun runs the section when a restart or reload would be needed, so its
  report says both that the service would restart and whether this
  invocation would be allowed to.
- The checks are not a prompt. MMX never waits for input. When a check
  fails, the run fails with the check's message. The person or agent runs
  the command again with the approval.

## Failure handling and cleanup stay with the node task block

A required failure stops the node task block's remaining normal phases.
The node job worker process then runs the block's
`on_failure` tasks, followed by the block's `cleanup` tasks, while the
current node job attempt is alive. The original failure is kept alongside
any later error.

A required failure, or a node job whose outcome is uncertain, stops normal
work on the node executing the failed or uncertain
node job. Normal work stops in the node job's fleet stage and in the node's
later fleet stages. Other nodes continue according to the fleet stage's
[failure policy](#choose-what-a-node-failure-stops). The page
[Put things back when a node task block fails](../../how-to/change-a-configuration-and-reload-its-service.md#put-things-back-when-a-node-task-block-fails)
says what `on_failure` may hold and read, with an example of an upgrade
that restores its backup.

### Cleanup is local to the attempt

`cleanup` is an ordinary finally section for the current attempt.
The `cleanup` section runs even when failure handling fails. The `cleanup`
section is not a saved obligation replayed after worker death, reboot, or
playbook edits. A cleanup failure fails the current attempt but does not
restrict a later command.

Core modules clean up their own staging files. Custom scripts own larger workflows
(mounts, rollback, load-balancer membership); their authors make them safe to
repeat and can opt into retries. `run` uses current definitions; `resume` uses
the recorded definitions, including their failure and cleanup sections.

## Service reloads follow configuration hashes

MMX remembers the configuration a service last confirmed loading (by start or
refresh) and compares it with the installed files after reconciliation. Matching
hashes need no refresh; different hashes require validation and the configured
refresh. The file task's changed result is not the trigger, so a refresh missed
by an interrupted run still happens on the next run.

Each tracked path is hashed separately with SHA-256 over its installed bytes or
absence; a declared symlink contributes its target text, and a file that
several `line` or `edit_file` tasks edit is one path. Source-directory tasks contribute their
regular-file destinations as if declared individually. Hashes
cover rendered files, not templates, and not ownership or permissions. A path
added to the set gets a baseline; a path removed from it is no longer compared.

Module pages that rely on this section:
[`directory`](../../reference/modules/directory.md),
[`docker_compose_project`](../../reference/modules/docker_compose_project.md),
[`podman_quadlet`](../../reference/modules/podman_quadlet.md),
[`shell`](../../reference/modules/shell.md), [`symlink`](../../reference/modules/symlink.md),
[`systemd_service`](../../reference/modules/systemd_service.md),
[`unpack_archive`](../../reference/modules/unpack_archive.md).

### Missing hashes establish a baseline before changes

Service records live under `/run/mmx/`, which is cleared on boot. Under the
node lock and before a node task block's first write, MMX inspects each of
the node task block's services:

- A running service with no record gets the current file hashes as its
  baseline. A missing record never requests a reload by itself, so reboot and
  first use cause no extra reload.
- A running service with a record keeps it; only inputs the record lacks get a
  baseline.
- A stopped service records the current hashes as not loaded; its confirmed
  start then records what it loaded.

The baseline assumes a running service started with the files on disk before
MMX's changes. MMX does not detect manual reloads. A corrupt record is an error,
reported before any change.

Each record is tied to the service's systemd invocation ID; a service that
started again since the record gets a fresh baseline, except after an attempt
that ended before activation, as the
[resource rules](../implementation-rules.md#resources-and-dryrun) describe.

When several node task blocks manage one service, a start or restart by one
of them loads everything on disk, including what the other node task blocks
wrote. The record of that start or restart therefore keeps only the hashes of
the node task block that made it. Each other node task block finds its inputs
missing from the record when it next runs, and they get a baseline then, as
the second rule above says. The fingerprint of a module that is not core stays
in the record, because nothing on disk can be its baseline: only the module's
own run reports it. A reload leaves all hashes of the other node task blocks
as they are, because a reload loads neither their unit files nor their
packages. For example, one node task block writes `app.conf` and names
`app.service` with `state: do_not_start_or_stop` and `refresh: reload`, and a later
node task block installs the unit and starts it. The start loads `app.conf`,
so the next run of the unchanged playbook reloads nothing.

For example, nginx starts after a reboot with configuration A. MMX saves A and
finds the desired files are still A: no reload. If MMX writes B and stops before
reloading, the saved A remains. The next run sees B on disk, validates it, and
reloads once even though the file task reports unchanged.

### A module that is not core gives its own baseline

An official extra module or a module of the project's own in a node task
block's `changes` is a tracked input by the fingerprint that its node task
reports. For a file, MMX hashes what is on disk before the block's first
write. For an official extra module or a module of the project's own, MMX
cannot hash the managed state before running the module's node task. Only
the module's own node task says what the module keeps on the node. The
baseline therefore comes from the node task that calls the module in the
node task block's `changes`:

- **The record holds no fingerprint for the module, and the module's task
  changes nothing.** The fingerprint that the task reports is the baseline,
  and nothing is refreshed: the service loaded what the module keeps, as it
  loaded the files on disk. The module's unchanged task supplies the baseline
  on the first run, after a reboot, and after the service started again by
  other means.
- **The module's task changes something.** The service is refreshed, also
  when the record held no fingerprint. A try that fails after it changed
  something counts as a change, although the next try then finds nothing
  left to change.
- **A run ends after the module's change and before the refresh.** Before the
  block's first write, MMX saves in the record that the module's fingerprint
  is not known yet. The unknown-fingerprint marker stays when the run ends
  early. In the next run the module's task finds nothing to change. But the
  unknown-fingerprint marker shows a possible change to the module's managed
  state during an earlier attempt, so MMX refreshes the service once.

  After a node task block's `activate` section succeeds, MMX removes remaining
  unknown-fingerprint markers for modules whose tasks changed nothing during
  the completed attempt.
- **The service started again during an attempt that did not finish.** For a
  file, the hashes show whether the unfinished attempt changed the file.
  Before the module's task runs, MMX cannot know whether the unfinished
  attempt changed the module's managed state. MMX therefore keeps the
  earlier service instance's record as the baseline. After the module's
  task runs, MMX compares the reported fingerprint with the saved baseline
  to decide whether the service needs a refresh.

In a record, a module's fingerprint is named by the project, the fleet stage,
the node task block, and the module. Two node task blocks that call the same
module for one service, such as two calls of one role, each compare their own
fingerprint. Several tasks of one module in one node task block, and the
items of a loop, count together.

### Attempted actions stay unconfirmed until they succeed

Before starting or refreshing, MMX records the hashes about to load as
unconfirmed, and saves them as loaded only on confirmed success. If the result
is lost, the next run finds the unconfirmed record and repeats the action after
validation. If a record write fails, MMX stops before the action it protects.

A confirmed refresh is recorded before health verification. A failed
health check or cleanup neither invalidates the saved refresh record nor
causes another refresh.

Dryrun reads records without writing them. During a dryrun, MMX compares
what tracked tasks would apply with the saved hashes. When no record exists,
MMX compares with current files. MMX reports why a refresh would be needed.

The [`systemd_service` module](../../reference/modules/systemd_service.md)
owns live state (`state`, required), boot activation (`start_at_boot`), and
the configured restart or reload action. `do_not_start_or_stop` still refreshes
a running unit after tracked changes; a stopped or missing unit owes no
refresh. By refreshing only running units, `do_not_start_or_stop` lets a
standby unit stay stopped while configuration changes.
See [module interface decisions](modules.md#module-design-decisions).

### Packages, service definitions, and checked-out code

A record also holds hashes for the node task block's `systemd_unit` files, its
packages, and its `git` checkouts. Unit files count under any name systemd
reads for the service: its names, aliases, and their templates
(`app@.service` for `app@1.service`). Package tracking records names,
installed versions, and states for `apt_package`, `dnf_package`, and language installer
modules, such as `pip_package` and `bundler_project_dependencies`. A `git`
task contributes the checkout path and the checked-out commit.

Changed software or a changed service definition requires a restart even
when configuration uses `refresh: reload`. A single
restart loads all changed inputs, and a stopped service starts once. A new
Git commit uses the `systemd_service` task's `refresh`, as changed
configuration does. The default, `restart`, is for an application that must
start again to load new code. `reload` is for an application that loads new
code on a reload.

An application that needs a restart for new code and only a reload for
configuration keeps the `git` task in a separate node task block. The
separate node task block names the service that the
configuration's node task block names. An application that loads new code
through a command uses the form in the
[next section](#a-command-that-follows-a-change-has-the-same-record).

The `systemd_unit` module checks new files before installation and reloads the
systemd manager afterwards; systemd's `NeedDaemonReload` covers the manager
side, and the service record covers whether the running process has the new
definition. A `systemd_service` task can name any unit type the
[`systemd_unit` module](../../reference/modules/systemd_unit.md)
installs, such as a `.timer`, `.socket`, or `.target`. The service record
and restart rule also apply to `.timer`, `.socket`, and `.target` units.
Package hashes cover only packages declared in the node task block.
A [whole-node upgrade](../../reference/modules/apt_package.md#upgrade-the-whole-node) restarts
nothing by itself: services that must load upgraded libraries are refreshed by
their own node task blocks, and a new kernel needs an explicit
[reboot](#reboot-and-wait).

A package never starts its own services while MMX installs it. On Debian and
Ubuntu the package system would start the package's services.
`apt_package` prevents service starts during installation, unless the task says
`allow_package_to_start_its_services: true`. On the Red Hat family a
package starts nothing by itself. A service
starts when the block's `systemd_service` task says `state: running`, after
the block's files are written and checked, and it starts once, with the
project's configuration. The
[`apt_package` page](../../reference/modules/apt_package.md#packages-do-not-start-their-services)
owns the mechanism.

A `systemd_service` task with `state: running` on a unit that runs once and
does not stay active (a `Type=oneshot` unit without `RemainAfterExit=yes`)
fails with that explanation, because MMX could never find such a unit
started. A unit that runs once and stays active, such as
`nftables.service`, is an ordinary service to MMX: a changed rules file
restarts it, which loads the rules. `nftables.service` clears every table
when it reloads, so it fits a rules file that is the node's whole firewall;
a file that keeps the rules of other tools holds a table of its own
([One nftables file instead](../../reference/modules/ufw.md#one-nftables-file-instead)).

Module pages that rely on this section:
[`systemd_unit`](../../reference/modules/systemd_unit.md).

### A command that follows a change has the same record

Many programs need a command after their input changes: `postmap` after a
Postfix lookup table, `update-ca-certificates` after a new certificate
authority file, touching `tmp/restart.txt` after new application code under
Passenger. Such a command must run exactly when its input changed, and it must
still run when the last run stopped between the change and the command.

A `shell` task in a node task block's `activate` section runs after tracked
input changes, including changes from interrupted runs, with
`only_when_tracked_inputs_changed: true`. The `shell` task must have an
`id`. MMX keeps a separate **command record** for the `shell` task. Like a
service record, a command record holds hashes of the node task block's
tracked inputs, but records the inputs when the command last succeeded.

```yaml
# roles/postfix_virtual_map/main.yml
node_tasks:
  - task_block:
      id: postfix_virtual_map
      changes:
        - file:
            permissions_mode: "0644"
            path: /etc/postfix/virtual
            source: files/virtual
      activate:
        - id: rebuild_virtual_map
          shell:
            script: postmap /etc/postfix/virtual
            only_when_tracked_inputs_changed: true
```

What MMX does on a node, step by step:

1. On the first run with the `rebuild_virtual_map` task, no command record
   exists. MMX takes the hashes of the node task block's tracked inputs on
   disk before the first write as the baseline. The `file` task then writes
   `/etc/postfix/virtual` if the destination differs from the content in
   `files/virtual`. If the `file` task writes `/etc/postfix/virtual`, the
   hashes differ from the baseline and `postmap` runs. If
   `/etc/postfix/virtual` already matches the declared content, `postmap`
   does not run.

   A missing command record never runs the command by itself,
   just as a missing service record never requests a refresh by itself.
2. Before the command starts, MMX saves the hashes it is about to act on as
   unconfirmed. After the command succeeds, MMX saves the hashes of the
   tracked inputs used by the command as confirmed.
3. Unchanged later run: the hashes match the confirmed record. The command is
   skipped and the task reports unchanged.
4. A later run changes `files/virtual`: the `file` task writes
   `/etc/postfix/virtual`, the hash differs from the record, and `postmap`
   runs once.
5. A run writes `/etc/postfix/virtual` and then stops before `postmap`
   (the connection dropped and the node rebooted, or the command failed).
   The record still holds the old hashes, or the new ones as unconfirmed.
   The next run finds the file already correct, so the `file` task reports
   unchanged, but the record does not match: `postmap` runs.

The differences from a service record, each with its reason:

- **The key.** A service record is keyed by its systemd unit. A command has no
  unit, so the command record's key combines the project's `project_name`
  with the task's result path. The result path includes the `id` of each
  enclosing role call, the node task block's `id`, and the task's `id`.
  Inside a loop, the result path also includes the iteration's `key`. The
  result path is the name `mmx results` shows for the task.

  Inside a fleet stage loop or a `for_each_node` loop, the key also includes
  the fleet stage loop iteration: the fleet stage path up to the innermost
  iteration, such as `rollout[web1]` for the fleet stage `rollout[web1]/drain`.
  Several iterations can reach one node, such as a helper node that each
  `for_each_node` iteration drains. Each iteration's command keeps its own
  record on that node, so one iteration's success does not hide a command
  still owed by another iteration. A fleet stage loop always has a `key`, and
  a `for_each_node` iteration is named by its node, so the iteration's name
  stays the same from run to run. The `id` of a fleet stage inside the
  iteration is not part of the key: two fleet stages of one iteration that
  call one role share one record, as outside loops. Outside fleet stage loops
  the key has no iteration.

  The node runner executable of release 0.1.0 kept the record of a command
  inside a fleet stage loop under the key without the iteration. An iteration
  without its own record starts from that record, so a command still owed
  before the upgrade runs after it. That record does not say which iteration
  owed the command, so each iteration that reaches the node starts from it,
  and the owed command can run once in each.

  Role-call IDs are part of the result path so a role called twice with
  different inputs on a node has two records. The key does not name the
  playbook, so two playbooks that run different commands under one result
  path would share one record, and one command's success would hide the
  other's owed run. `mmx validate` and every run therefore reject two
  different tasks anywhere in the project, in its playbooks and its
  outstanding one-time playbooks, that have the same result path, and name
  both tasks. They reject the two tasks also when fleet stage loops give them
  different keys, so a record of release 0.1.0 found under a result path
  always belongs to the command at that path. One role called at the same
  path from two playbooks is one command with one record. A loop whose
  iterations hold such a command requires `key`: without `key`, a list item
  is named by its position, so adding or removing an item would move a
  record to another item.

  Changing one of the IDs starts a fresh record, which takes a baseline and
  does not run the command. A command still owed under the old ID is then
  lost, so finish a successful run of the playbook before renaming the
  node task block, the `shell` task, a role call around them, or a fleet
  stage loop around them.
- **Where the command record lives.** A service record lives under
  `/run/mmx/` and is cleared on boot. Boot restarts a service with the files
  on disk, so a service record does not need to survive boot. Boot does not
  run `postmap`. A command record therefore lives in
  `/var/lib/mmx/command_records/` and survives a reboot, so a command that
  was still owed before the reboot runs on the next run.
- **No running state.** A service that started again since its record gets a
  fresh baseline. A command has no running state, so its record changes only
  when the command succeeds.

The rest is the service rule. The task sees only its own block's tracked
inputs, not a file that another role or another program wrote. A change made
before MMX's first run counts as already applied. A module that is not core
[gives its own baseline](#a-module-that-is-not-core-gives-its-own-baseline)
to a command record too: a new record does not run the command when the
module's task changes nothing. A corrupt record is an
error, reported before any change. Dryrun reads the record without writing it
and reports `would_change` with the reason, for example "would run: tracked
input /etc/postfix/virtual would change". A block can hold several such
tasks, and services beside them; each has its own record.

`only_when_tracked_inputs_changed: true` also makes a checkout trigger a
command instead of a restart. In the `roles/shop_code/main.yml` example
below, a new checked-out commit causes the `tell_passenger_to_restart`
`shell` task to touch Passenger's restart file:

```yaml
# roles/shop_code/main.yml
inputs:
  release:
    type: string
    required: true

node_tasks:
  - task_block:
      id: shop_code
      changes:
        - git:
            repo: https://github.com/example/shop.git
            check_out_tag: "{{ .inputs.release }}"
            path: /srv/shop
            run_as_user_account: deploy
      activate:
        - id: tell_passenger_to_restart
          shell:
            script: touch /srv/shop/tmp/restart.txt
            run_as_user_account: deploy
            only_when_tracked_inputs_changed: true
```

A `shell` task in `activate` without `only_when_tracked_inputs_changed`
runs on every run, in written order. A `shell` task with
`only_when_tracked_inputs_changed` outside an `activate` section is a
validation error, because only a node task block has tracked inputs.

Where a systemd unit applies a file, a `systemd_service` task on the unit is
the first choice. For example, `nftables.service` applies a firewall file
that contains the node's whole firewall. The
`only_when_tracked_inputs_changed` field is for commands that have no unit.
For example, `nft --file` loads a firewall table that leaves the rules of
other tools alone
([One nftables file instead](../../reference/modules/ufw.md#one-nftables-file-instead)). A command that must run only once
ever, such as creating the first administrator of an application, uses
[`run_only_once_marker_file`](../../reference/modules/shell.md#guards) instead:
it does not follow a change.

A restart that a person asks for once, for a service that runs but no
longer answers, does not follow a change either. The requested restart uses
a `systemd_service` task with `restart_now: true` and a
`run_only_once_marker_file`. The restart task belongs outside every node
task block's `activate` phase, usually in a
[one-time playbook](workflow.md#one-time-playbooks)
([`restart_now`](../../reference/modules/systemd_service.md#restart_now-and-its-marker-file)).
While the file is missing, the task restarts the unit with `systemctl
restart`, which also starts a unit that does not run, and writes the file
only once the unit runs again; while the file exists, the task keeps the
unit running as any task with `state: running` does. Three rules govern
restarts requested with `restart_now` and recorded by the marker file:

- The marker file is required. `mmx check-one-time-playbooks` counts a node
  as done when its dryrun proposes no change; without the file every dryrun
  would propose the restart, and the playbook would stay needed for ever. In
  an ordinary playbook, a restart on every run would be an outage that
  nothing asked for. A restart that must happen again is a new one-time
  playbook with a new marker file.
- `state: running` is required, because a restart leaves the unit running.
- Validation refuses `restart_now` in `activate`, where the configuration
  hashes of the block's tracked inputs decide restarts and reloads; a second
  rule there would restart a unit that the hashes leave alone. In another
  phase of a node task block, `restart_now` can restart a unit named in the
  block's `activate` section. The restart saves the restarted unit's service
  record like any start, because a restart loads everything on disk.

## Run and resume use the same recovery state

`run` uses current files. `resume` repeats unfinished work for each fleet stage
and node from a recorded run, as the [run](workflow.md#run-uses-current-files-and-the-full-selection)
and [resume](workflow.md#resume-uses-the-recorded-run) workflows define. A prior
failure never blocks `mmx run` or `mmx resume`.

### Ordinary fixes recover without identity matching

File and package tasks inspect current state. Service records use the actual
unit. Editing or removing a failed task, node task block, check, or cleanup section
never restricts a new `run`; a new run is judged by its own checks. MMX keeps job
results and the small service and command records, not a ledger of pending node
task blocks or saved cleanup to match across edited playbooks. Task IDs identify
results and must be unambiguous within a task list. A
[command record](#a-command-that-follows-a-change-has-the-same-record) is the
one place where an ID identifies saved state. Giving the node task block
that contains the recorded `shell` command, or the `shell` task, another
`id` starts a fresh command record.

## Task retries have explicit conditions and limits

A **node task** is the declared unit of work on one node.
A **try** is one invocation of a node task. A **retry** is a try after the
first. Retry settings and time limits apply to shell commands and every
kind of module wherever node tasks are allowed.

Each node task falls into a group with a different retry rule:

- **Retry by default:** the core modules that change a resource, such as
  `file`, `apt_package`, or `git`, and every module that is not core.
- **Try once unless the node task sets `retry: true` or a `retry`
  mapping:** the core modules `shell`, `http`, `read_file`,
  `systemd_service`, and `certbot_certificate`, and the node task forms
  `show` and `set_vars`, which are not modules and evaluate their values
  once.
- **Reject every `retry` setting, even `retry: false`:** `assert`, which
  evaluates once; retry the node task that observes the value instead.
  `assert` takes `time_limit` and `time_limit_per_try` like other node
  tasks.
- **Reject `retry`, `time_limit`, and `time_limit_per_try`:** `wait_for`,
  `reboot`, and the network configuration modules listed below run once
  and ignore `node_task_defaults`. Each module bounds the wait with a
  module input. `wait_for` and `reboot` use `wait_at_most`.
  `firewalld_zone_membership` and the
  [network configuration modules](modules-network.md#what-a-run-does)
  `systemd_networkd_file`, `netplan_file`, `networkmanager_connection_file`,
  and `ifupdown_interfaces_file` use
  `restore_unless_controller_confirms_within` to bound the confirmation wait
  on the node.

`retry: false` turns retries off for a node task that retries by default.
`retry: {}` turns retries on with `node_task_defaults` from the project
settings.

Invalid configuration, rejected validation, and a missing prerequisite fail
immediately. A missing prerequisite is something an earlier node task or a
person must provide, such as a package, a program, an account, or a file
(`failure_kind: missing_prerequisite`); the node does not change between
tries, so another try would only wait and fail again with the same reason.
Only the task repeats: role calls and node task blocks reject a `retry` setting.
Retry settings and time limits never restrict an explicit `run` or `resume`.

| Setting | Built-in default | Meaning |
|---|---|---|
| `time_limit` | `null` | Overall task time budget, including tries and pauses. |
| `time_limit_per_try` | `null` | Time limit for one try. |
| `retry.max_tries` | `3` | Maximum total tries, including the first, when retries are enabled. |
| `retry.pause_between_tries` | `2s` | Pause after a try finishes, before the next starts. |
| `retry.until` | `true` | Additional result condition required for completion. |
| `retry.retry_when` | `true` | Whether another try is allowed when completion has not been reached. |

The [node task defaults contract](configuration.md#node-task-defaults-are-shared-project-configuration)
owns inheritance and explicit `null` for time limits. Settings live in `mmx_project_settings.yml`
as `node_task_defaults.time_limit`, `node_task_defaults.time_limit_per_try`, `node_task_defaults.retry.max_tries`,
and `node_task_defaults.retry.pause_between_tries`. `max_tries` takes a positive integer or
`unlimited`; `unlimited` is allowed only when the node task has a finite
`time_limit`, set on it or inherited from `node_task_defaults.time_limit`.
`null` is not a valid `max_tries` value. Reaching either the try
limit or the overall time limit ends repetition. The task clock starts when the
node task begins, not while it waits for a node slot. Tries are sequential
and share the node task's overall `time_limit` budget.

For each try:

1. Execute the task and apply its normal success and change rules (for shell,
   the [result rules of `shell`](../../reference/modules/shell.md#result-rules)).
2. If the current try succeeded, evaluate `retry.until`. When `retry.until`
   is true or omitted, the node task succeeds.
3. Otherwise evaluate `retry.retry_when`. When false, the task fails.
4. If limits allow, wait `retry.pause_between_tries` and execute again; otherwise the task
   fails because completion was not reached.

`until` cannot turn a failed try into success. `retry_when` only permits
another try. Both `until` and `retry_when` use the ordinary predicate model
on `.this_node_task`. A broken predicate fails immediately.

The script below exits zero when its query succeeds, `75` for a temporary
failure, and other codes for permanent failures:

```yaml
- shell:
    script: /usr/local/bin/job-status
    changed_when: false
  time_limit: 2m
  time_limit_per_try: 5s
  retry:
    max_tries: 30
    pause_between_tries: 2s
    until:
      contains: ["{{ .this_node_task.stdout }}", "job_status=complete"]
    retry_when:
      any:
        - equals: ["{{ .this_node_task.exit_code }}", 0]
        - equals: ["{{ .this_node_task.exit_code }}", 75]
```

Exit zero with `job_status=complete` in standard output succeeds.
Exit zero without `job_status=complete`, or exit `75`, tries again.
Other codes fail at once, as do thirty tries or two minutes without
completion.

For service readiness, use `wait_for` in the node task block's
`verify` phase, as in the
[verification example](configuration.md#roles-describe-reusable-server-configuration).
The `wait_for` node task repeats only the readiness observation, not the
deployment before the `wait_for` node task. On the node, `wait_for` checks
every second until the `wait_for` input `wait_at_most` (default `60s`)
expires. The `wait_for` node task reports unchanged on success and fails
with the last observation if the wait expires.

In dryrun, the `wait_for` node task checks once without waiting. If readiness
is not established, `wait_for` reports `not_checked` because earlier node
tasks in a real run may establish readiness. `http` fetches one response
for later node tasks to read. `wait_for` waits for readiness.

Limits apply per node and loop item. Saved results keep the first and latest
nine tries and the final result. The final result reports a change from an
earlier try even if the last try reports unchanged.

Failure handling runs after the final outcome, not after each unsuccessful
try. Guards and dryrun keep their normal rules. Retries never enable
mutating commands in dryrun.

Module pages that rely on this section:
[`shell`](../../reference/modules/shell.md).

## Disconnects, stopping, and reboots

An admitted node job finishes the current fleet stage even if the controller
process exits or the connection to the controller machine is lost.
No later batch or fleet stage starts without a controller process.
Losing the laptop therefore does not spread the rollout to new nodes. A
lost SSH connection does not by itself mean the admitted node job
failed or stopped. `mmx status` collects results without submitting work.

Ctrl-C stops new admission and asks active nodes to stop at a task or node task block
boundary: the current standalone task finishes, or the whole node task block with its
validation, activation, verification, failure handling, and cleanup. The node
checks for a stop before each task outside a node task block, including between tasks
in roles.

Stopping does not kill running processes, and nothing continues
automatically. `mmx resume` restarts unfinished
work for each fleet stage and node. From another terminal, `mmx stop`
requests a stop of new admission. `mmx stop` also asks active nodes to stop
at a node task or node task block boundary, as Ctrl-C does. The
[workflow](workflow.md#inspecting-and-stopping-a-run) owns the messages and
follow-up commands.

Connection timeouts are separate from `time_limit_per_try` and `time_limit`:
a lost or quiet connection never cancels node work, while reaching a node task or per-try
time limit kills the command's whole process group. When a command ends by
itself, a program it started in the background with its output redirected
keeps running. If a background program still holds the command's output
open 0.2 seconds after the command exits, the node task fails.
MMX then ends the programs left in the command's process group.
For a program that MMX writes to on standard input, such as the executable
of a module of the project's own, MMX ends the whole process group at once.

A reboot ends the node job worker process. Persistent job records and
command records survive the reboot. Service records reset and follow the
[baseline rule](#missing-hashes-establish-a-baseline-before-changes). A reboot
MMX did not request interrupts the node job like any other loss of the node
job worker process. A playbook reboots a node with the
[reboot task](#reboot-and-wait).

Changes that require the controller process to confirm reachability follow
the rule below. Losing the controller process does not cancel node work.
Before a change, the node task fails without changing the configuration if
no connection kind confirmed. After an unconfirmed change, the node attempts
restoration and the node task fails. If restoration fails, the saved file
and owed restore remain.

## Reachability confirmation and owed restore

Reachability confirmation and owed restore protect a change that could cut
the node off from the controller machine. The node side and the controller
process’s confirmation loop are built: the durable record, new connection
attempts, confirmation handlers, and systemd activation and restoration.
The guest suite `owed_restores.py` proves the whole mechanism on real nodes
with a module that exists only in test builds. The production core module
`systemd_networkd_file` uses
reachability confirmation and owed restore. The `systemd_networkd_file`
guest suite covers native activation and recovery. The production modules
`networkmanager_connection_file`, `netplan_file`, and
`ifupdown_interfaces_file` also use reachability confirmation and owed
restore, each with a dedicated guest suite. `firewalld_zone_membership`
protects membership with module data instead of a file.
The production module `wireguard_interface` also uses reachability
confirmation and owed restore for configuration file changes. WireGuard
running-state repairs with an unchanged file do not require confirmation.
[Validation](../validation.md) records current guest results.

A **reachability confirmation** is a request the controller process sends
over a new SSH or HTTPS connection for one change of one node job. An
**owed restore** is a
durable record on the node that one module-owned file, or the zone members
that one node task changed, must return to its previous state unless every required confirmation arrives in time. A
systemd timer performs the restore without the controller machine or the
node job worker process, the process that runs that node job's tasks.

The node accepts confirmation through `mmx-runner confirm-reachability`
over SSH, or `POST /v1/confirm-reachability` through its authenticated HTTPS
API. The node SSH request process supplies SSH as the connection kind.
The node HTTPS API process supplies HTTPS as the connection kind.

Ordinary progress requests do not confirm reachability. SSH progress uses
connection sharing; HTTPS progress uses keep-alive connections. A network
change can leave an existing SSH or HTTPS connection alive while refusing
every new SSH and HTTPS connection. Moving an interface into another
firewall zone is one example.

Module pages that rely on this section:
[`firewalld_zone_membership`](../../reference/modules/firewalld_zone_membership.md),
[`ifupdown_interfaces_file`](../../reference/modules/ifupdown_interfaces_file.md),
[`netplan_file`](../../reference/modules/netplan_file.md),
[`networkmanager_connection_file`](../../reference/modules/networkmanager_connection_file.md),
[`systemd_networkd_file`](../../reference/modules/systemd_networkd_file.md),
[`wireguard_interface`](../../reference/modules/wireguard_interface.md).

### New connections before and after the change

The node's progress answer says that a change waits for confirmations.
The progress answer identifies the node job and change, whether the wait
is before or after activation, the required connection kinds, and the
node's deadline. The reachability confirmation request serves the wait
before activation and the wait after activation. The request identifies
the node job, change, and phase. The node answers whether the request
counted. The node determines the connection kind from the connection that
received the request, not from a claim in the request.

Before the change, the controller process tries SSH and HTTPS:

- SSH opens a new connection without using or creating a shared connection.
- HTTPS opens a new connection to the node HTTPS API process, with no
  reuse of an existing connection.

SSH and HTTPS confirmation attempts retain ordinary authentication and
node identity checks. SSH confirmation uses `ControlMaster=no`,
`ControlPath=none`, and `ControlPersist=no`, even when SSH configuration
enables sharing. Read-only SSH preparation can reuse an ordinary connection
to resolve SSH authentication and sudo. The SSH confirmation checks
identity on the
new connection.
Each HTTPS attempt has its own client, with connection reuse disabled.
An SSH confirmation cannot satisfy an HTTPS requirement, and an HTTPS
confirmation cannot satisfy an SSH requirement. Confirmation attempts use
the requested kind without fallback, even when ordinary progress falls back
from HTTPS to SSH. For a node whose inventory says `connection: ssh_only`,
the controller process tries only SSH: MMX opens no HTTPS connection to
such a node. An unreachable HTTPS API is not itself a failure. No check
installs a service or opens a firewall port.

The wait before the change uses the module's
`restore_unless_controller_confirms_within` duration. The before-change wait
ends when every eligible connection kind confirms or the deadline expires.
At least one kind must have confirmed. Otherwise the node task fails before
any change. The node saves exactly the kinds that confirmed as the
requirement after activation. Thus an SSH-only node requires SSH
afterwards. A node reached by SSH and HTTPS
requires SSH and HTTPS afterwards.

Before applying the change, the node arms its restore timer for an absolute
deadline: the current time plus the module's
`restore_unless_controller_confirms_within` duration. Applying the change,
activation, and the tool's acceptance check consume the time before the
restore deadline. After applying the change, activating the configuration,
and passing the tool's acceptance check, the node advertises the
after-change wait with the deadline set when arming the restore timer.
The node must start arming within 30 seconds after the before-change wait
ends. Exceeding the arming allowance fails the node task without changing
the destination file. The node's progress answer gives the controller
process a latest possible after-change deadline: the before-change deadline
plus 30 seconds
plus the module’s confirmation duration. The timer is never postponed by a
failed connection attempt. If activation
uses the whole budget, the restore runs; there is no unprotected activation
interval followed by a fresh full wait.

The controller process starts a phase's first attempts as soon as an answer
from the node announces that phase. Repeated attempts of one kind
within that phase start at least ten seconds apart. At most one attempt of
each kind is in flight, including across a phase change. A counted kind needs
no further attempt in that phase. A definite authentication, identity, or
protocol rejection stops attempts of the rejected connection kind in the
current confirmation phase. The controller process never weakens
authentication or node identity checks, or starts
service setup, in response to a rejection.

The controller process opens an after-change confirmation connection only
after it has received an answer saying that activation and its checks are
complete and the after-change phase is ready. That answer must arrive before
the connection is opened. A connection opened during activation could remain
alive through a restrictive network change and deliver its request afterwards;
it would not prove that a new connection can reach the changed node.

With working progress, the progress answer announces readiness. If progress
fails after the before-change phase completed or its deadline passed, the
controller process asks for ordinary status over new connections. These
status requests cannot count as confirmation. Attempts to request ordinary
node job status over SSH start at least ten seconds apart, as do attempts
over HTTPS. Once an answer reports after-change readiness, confirmation
starts at once on another new connection. A recovered progress answer still
in the before-change phase stops the separate status
requests until progress fails again.

The controller process retains the pending change across failed progress
requests, including exhaustion of their ordinary retries. Losing progress
between phases therefore does not prevent the controller process from
asking for readiness and then confirming the change. If no answer announces
readiness before the latest possible after-change deadline, the controller
process sends no after-change confirmation and continues collecting the
restore outcome.

Each confirmation or readiness request has at most ten seconds and cannot
exceed the remaining confirmation wait. The controller process derives the
remaining confirmation wait from the node's reported time and deadline,
using the request's start as a conservative local time. Later answers can
shorten the local confirmation deadline, never extend the deadline.
Until an after-change answer arrives, the latest possible after-change
deadline bounds confirmation and readiness requests. The node decides which
connection kinds are required after the change. Later connectivity cannot
add another kind.

Only a request for the current phase and exact change can count. Requests
from another node job, earlier changes, an earlier phase, or after expiry
do not confirm the change currently waiting for confirmation. Repeated
requests are harmless. A lost response does not undo a confirmation already
recorded by the node. An ordinary progress or status request never counts,
even on a new connection.

The node waits for every required kind. Confirmation and expiry are decided
together against the same durable owed restore record: the node accepts
the change in time or restoration wins. A late request
cannot cancel a restore that has started. The node durably records
acceptance before removing the timer and saved previous file, so a late
invocation of the timer's service
cannot restore an accepted change.

If the controller process stops during an unconfirmed wait, for example
after Ctrl-C or when the laptop sleeps, it sends no more confirmations.
After a change, the node restores and the node task fails. Before a
change, the node fails without changing the configuration if no kind confirmed.
The next run can attempt the change again. Restoring an unconfirmed change
deliberately favors restored access over keeping a change that the
controller process could not confirm.
Reachability confirmation differs from the reboot wait: a network change
is not accepted merely because the node's commands finished. A controller
process handling Ctrl-C stops sending confirmations even if the controller
process still collects the stopping node job's result. While the node job's
outcome is uncertain, Ctrl-C interrupts
the controller process's result requests so the controller process can
deliver a stop request to the node. For a finished node job, the controller
process continues collecting the restore outcome without cancellation by
the stop request. The collection deadline described below still applies.

When contact fails after a change, the controller process keeps
collecting the node job's result while the node's restore runs. The
controller process does not report success from confirmation alone.
Collection is bounded by the latest known after-change deadline plus 340
seconds: the node's 330-second restore
allowance and one ten-second collection interval. An actual after-change
deadline can shorten the result collection deadline. For collection, the
remaining node time is measured from receipt of the answer, so a slow
response does not consume the restore allowance. If the controller process
first learns of a required or
running restore from a terminal node job result, it allows 340 seconds from
that observation.

A collected restore outcome appears in human output for each node and
change, including when only the node job response supplies it. Human output
labels a file operation "file change" and a membership operation
"membership change". The human output says that the file
"was restored to its previous state",
that its restore "is still running", "has not started yet", or "failed".
A change that was accepted needs no restore, so its outcome has restore_status
"not_needed" and human output prints no restore line for it.
A successful restore uses "was restored to its previous state" after
administrator repair and when the previous state was an absent file.
The line "why the change was not kept" appears only when the node supplied
a reason. Only a failed restore shows "why the restore failed"
and the three steps: "previous file put back", "previous configuration
activated", and "virtual devices of the new file removed", each as yes or no.
Virtual devices are software network interfaces, such as bridges and VLANs.
While a restore runs or has not started, "restore service on the node" names
its service. After failure, "after you corrected the cause, run on the node"
gives the command `systemctl start mmx-owed-restore-<owed-restore-id>.service`.
Success omits the steps and service; the node removes the units after success.
The original node task failure stays visible; diagnostics from tasks that
used secrets remain suppressed. If the node still cannot report completion
within the collection bound, the controller process names the node job and
change and says that restore completion is unconfirmed. An otherwise unfinished node job remains `unknown`; a known
node job failure remains `failed`. Run `mmx status <run-id>` to inspect the
node job again. The `mmx status <run-id>` command also refreshes owed
restores in a failed node job after an administrator repairs the cause and
starts the service for the failed owed restore. A successful restore does
not change the failed node job's failure. Status inspection never
sends reachability confirmations; an active `mmx resume` can confirm a still
running node job.

A confirmation that reaches the node before its node job worker process
observes a stop request may still accept the change, because every required
new connection proved reachability. The node job then stops after the node
task whose change required confirmation.

The controller process uses the node address already selected for the node
job. Moving that address is outside this design, as the
[network module limits](modules-network.md#limits) say: confirmations fail
and the node restores.

### A change on the controller machine itself

The controller machine can also be an inventory node, such as `util01`, a
server where the team runs `mmx`.
A new connection from `util01` to `util01`'s own address stays on `util01`.
A connection to `util01`'s own address can succeed even when `util01` cannot
reach any other server.
For example, a wrong WireGuard peer list on `util01` can remove access to
every server reached through `wg0`.
A successful connection to `util01`'s own address would then accept a network
configuration that cut off the fleet.

MMX recognizes the controller machine by comparing the controller machine's
systemd machine ID hash and boot ID with the node's verified identity and
boot ID. A virtual machine cloned from the controller machine's image shares
the machine ID but boots its own kernel, so the boot IDs tell the clone from
the controller machine.
When both match, the controller process checks access to the inventory's
other nodes before confirming the change:

- Before sending any before-change confirmation, the controller process
  attempts one new SSH connection to every other inventory node.
  The probe set uses the run's whole inventory, regardless of fleet stage
  selection or `--limit`.
  The probe set excludes only the changed node. Each probe reads the node's
  machine ID and boot ID.
  Each probe disables SSH connection sharing and retains ordinary
  authentication, host-key checks, and any saved node identity check.
  No probe installs or starts a service, or runs sudo.
- Nodes answering before the change form the required set for that change.
  A node that does not answer before the change is not required afterwards.
  An inventory entry that answers with the controller machine's machine ID
  and boot ID is another name for the controller machine and is excluded
  from the required set. A clone that answers with the same machine ID and
  another boot ID is required.
- After the node announces the after-change phase, the controller process
  opens new SSH connections to the required nodes.
  Every required node must answer with the identity observed before the
  change.
  Repeated attempts to each required node start at least ten seconds apart.
  Attempts to different nodes run concurrently, bounded by project setting
  `max_nodes_at_once`.
- Until every required node answers, the controller process sends no
  after-change confirmation over SSH or HTTPS.
  The node's deadline bounds the probes and confirmations.
  Without every answer before the deadline, the node's timer restores the
  previous configuration.
  The controller process's output and node task failure name the required
  nodes that did not answer after the change.
  Saved controller observations preserve the node names when `mmx status`
  refreshes the node's result.

A resumed controller process that first observes the after-change phase has no
reliable before-change probe set.
The resumed controller process withholds confirmation and reports the incomplete
before-change probes, allowing the node's timer to restore the previous configuration.
Confirmation also remains withheld if the after-change phase arrives before
the before-change probes finish.
The [network configuration run steps](modules-network.md#what-a-run-does) use
the controller-machine check for every module using reachability confirmation.

### Keep the owed restore until it is resolved

For a file, before the change the node saves the previous file or its absence,
with its owner, group, permission bits, and modification time, in
root-private persistent storage under
`/var/lib/mmx/owed_restores/<owed-restore-id>/`. The owed restore also records its
node job, module, destination file when present, restore service's unit name, phase,
deadline, required connection kinds, and restore outcome. The owed restore
record keeps what the module needs to remove devices introduced by the
proposed file when restoring an absent previous file.
The records are outside expiring node job history and outside `/run`.
Access time and extended attributes are not saved. A restored file receives
the SELinux label of its destination path, as every file MMX writes does.

The record uses `owed_restore_id`, `change_phase`, `change_started`,
`change_activation_failed`, and `change`. The record's `change` field has
the same meaning as `change` in a node task result: `changed`, `unchanged`,
or `unknown`.
The phase `applying_change` marks the durable boundary before the file
operation or native activation; `abandoned_before_change` means no change
started. The confirmation reasons `owed_restore_not_found` and
`owed_restore_resolved` refer to the owed restore for the requested change.
The record's module name selects compiled activation code.

An **owed restore id** is 8 random bytes written as 16 lowercase hexadecimal
characters. The node creates the directory for the new owed restore id
exclusively and tries another id on collision. The owed restore directory
has mode 0700. Within the owed restore directory, the `owed_restore.json`
record, `previous_file_contents` when a previous file existed,
`proposed_file_contents` when the module's activation reads it, and
`restore_execution.lock` have mode 0600. Root owns the owed restore directory
and every file in the owed restore directory. The small resolved record
remains until the node job's result has the restore outcome and the node
job's history expires.

The node writes three unit files in `/etc/systemd/system/`. The unit files
belong to root and have mode 0644:

- `mmx-owed-restore-<owed-restore-id>.timer` sets the absolute restore time.
- `mmx-owed-restore-<owed-restore-id>.service` starts the node owed restore
  process, which restores the module's recorded previous configuration.
- `mmx-activate-change-<owed-restore-id>.service` starts the node
  change activation process, which activates and checks the proposed change.

The node saves and syncs the previous contents before arming the restore
timer. The node then makes the record, unit files, and unit enablement
durable, starts the timer, and checks that the timer has a future firing.
An already elapsed timer can report a
successful start without ever firing. MMX then refuses to replace or remove
the module-owned destination file. The record says installation may have
started before the file is replaced or removed. MMX syncs
the destination's directory after replacing or removing the destination file.

A module whose activation also reads the proposed file, such as
`ifupdown_interfaces_file`, gets `proposed_file_contents` too. The node saves
the proposed file's contents like the previous contents, before arming the
restore timer. The node hands the paths of `previous_file_contents` and
`proposed_file_contents` to both the activation and the restore. ifupdown's
`ifdown` takes an interface down only with the stanza that brought the
interface up, so the activation needs the previous file and the restore
needs the proposed file after replacing or removing the destination file.
A removal has no proposed file.
Cleanup removes `proposed_file_contents` with `previous_file_contents`.

The timer's time is rounded up from the record's `restore_deadline`, with
one-second accuracy. The timer does not wait for time synchronisation.
The deadline uses the node's clock. Setting that clock back during the wait
delays the restore by the same amount.

Both the timer and restore service are enabled across boot. The restore
service starts at boot, ordered after `network.target`, and reads its
record. A node restart cannot accept an unconfirmed change: the old
node job worker process no longer runs. The service restores immediately,
including when a previous restore attempt failed. The node owed restore
process abandons a change that never started and cleans up an accepted or
restored change.
The timer itself need not fire when its time passed while the node was off.

If the owed restore service starts early while the node job worker process
is alive, the node owed restore process keeps waiting until the deadline or
a resolved record. The node owed restore process does not exit, because an
elapsed timer never fires again. While waiting, the node owed restore
process extends systemd's service wait timeout.
Once a restore attempt starts, the node owed restore process stops extending
the service wait timeout.
One attempt has five minutes, including stopping activation and waiting for
the activation lock.
The owed restore service has a 330-second limit from the start of the
restore attempt.
The node job worker process waits at most 330 seconds once it observes that
restoration is required or running. If the service records no completion,
the node task fails and names the restore service. The owed restore and saved
file remain; the worker does not start another restore attempt.
Stopping the change activation service or the owed restore service allows
five seconds before systemd kills the stopped service's remaining
processes. Both `inactive` and `failed` mean activation has stopped.
No network connection is needed to invoke restoration or read the saved file.

The timer's service runs `mmx-runner run-owed-restore --owed-restore-id <id>`
as the only restore path. The activation service runs
`mmx-runner activate-change --owed-restore-id <id>`. The node change
activation process and the node owed restore process each find the module's
compiled activation code from the record's module name, destination, and
private restore data. The record also distinguishes failure of activation
or of the native tool's acceptance check from expired confirmation,
so task results retain the native diagnostic and describe what restoration
completed. If activation or the activation check fails, the node starts
`mmx-owed-restore-<owed-restore-id>.service` immediately. The service puts
the previous file back, or removes the newly created file when none
existed, then performs the module's activation and device cleanup.
The node owed restore process records success only after the previous file
or absence is restored and the module's activation and device cleanup
succeed.
Worker death or node restart does not erase the owed restore or cancel the
owed restore's timer.

Restoration must not wait for the node job's lifetime lock: its worker can
still be waiting for confirmations. Coordinate the change's activation and
restore so they cannot run against each other. Before restoring, stop any
activation command still running for this change, including one left by a
killed worker. Starting a restore service twice has the same effect as
starting the restore service once. Running restoration without the node
job's lifetime lock, coordinating activation and restoration, stopping
activation before restoration, and making repeated restore service starts
harmless extend node execution coordination without allowing two ordinary
node jobs to change a node at once.

On confirmed acceptance, cancel the timer and remove the saved file and
unit files. On successful restoration, durably record the failed task's
restore outcome, then remove the saved file and unit files. If the node job
worker process has ended, the next node inspection includes the failed
task's restore outcome in the interrupted result for the ended worker's
node job. A successful restore never changes the failed task into
a successful task.

While any restore remains owed, MMX refuses to replace the node runner
executable. A node task for the destination file recorded by the owed
restore fails,
even when the node task's candidate matches the file on disk.
The failed node task's reason identifies the destination file and earlier
node job. If restoration failed, the node task's reason also reports the
restoration failure.
Other files remain subject to ordinary node job serialization; an owed
restore does not authorize a second writer to its file.

If restoration fails, the saved previous file and owed restore remain,
with the tool's reason and whether the file was put back but activation
failed. If the restore process ends before
recording completion, the waiting node job worker process or the next node
inspection records an interrupted restore failure. Inspection does not
repeat the interrupted restore attempt.
The next node job sees the recorded restore failure and cannot replace the
unresolved owed restore with another change of the destination file.
After correcting the reported cause, the administrator starts the recorded
restore service again. For example, for owed restore `0123456789abcdef`,
run as root on the node:

```sh
systemctl start mmx-owed-restore-0123456789abcdef.service
journalctl -u mmx-owed-restore-0123456789abcdef.service
```

The record and node job result name the exact service. A node restart also
repeats a failed restore. The proposed network change is never repeated
automatically, and there is no second restore implementation.
Neither history cleanup nor a new node job can discard the saved file
while the owed restore is unresolved. A task for the destination file recorded
in the owed restore also fails during cleanup, until the saved file,
candidates, and units are gone. Protected proposed-file and
previous-file candidates have separate names derived from the destination's
file name. A later restore can remove the proposed-file and previous-file
candidates without the dead process's prepared object.
Ordinary file candidates keep their random names.

Module pages that rely on this section:
[`netplan_file`](../../reference/modules/netplan_file.md),
[`wireguard_interface`](../../reference/modules/wireguard_interface.md).

### One module-owned change, not node task block rollback

A **node task block** groups related node tasks into changes, checks,
activation, and verification. A node task block still has no general rollback
flag or transaction across the files changed by the block's node tasks.
An owed restore belongs to a module that owns one file or the zone members
changed by one node task. The module knows
how to restore the module-owned file's previous state. The module cannot
restore arbitrary files from a node task block or replay its cleanup after
a crash.

NetworkManager's checkpoint is not used. The systemd timer already survives
worker death and node restart. The measured checkpoint restored a modified
profile but retained a file added during the checkpoint. Two mechanisms
restoring at once could also work against each other. Moving an interface
into another `firewalld` zone can also cut SSH off. The built
`firewalld_zone_membership` module uses the same mechanism with module data
instead of a file, as described below.


### Firewalld membership without a destination file

`firewalld_zone_membership` records the old and proposed binding of every
affected interface or IP source network, separately for the running firewall
and stored configuration. A member with no explicit binding has an empty
zone. The module owns those bindings, never the zone's rules or a
NetworkManager profile. Its [page](../../reference/modules/firewalld_zone_membership.md)
owns validation, native commands, and ownership refusals.

The same record, confirmations, timer, activation service, restore service,
locks, and administrator commands protect this change. There is no destination
file, saved file metadata, previous contents, or candidate file. The node
reserves the empty destination for this node-wide membership obligation.
A later membership task is refused until restoration or acceptance and
cleanup complete. The zone-creation module also refuses while membership
recovery remains owed, because its global reload would interfere.

The module's activation process changes and checks membership with firewalld
commands. Its restore process restores both saved layers with individual
commands and checks the result, including a layer that did not need a forward
command. On reboot, the service runs after firewalld if firewalld starts.
It neither starts nor enables firewalld. If firewalld is stopped, restoration
fails and retains the obligation. The administrator starts firewalld and then
starts the restore service named by `mmx status`. The restored task remains
failed. Human output names firewalld zone membership and reports whether
both layers were restored; it makes no
claim about restoring a file or deleting a virtual device.

The mechanism has two forms: a prepared file, or private module data alone.
For the second form, the module supplies the plain `change_subject`, the
single `restore_after_unit`, and the `restore_success_sentence`. The record
keeps them; the restore process reads them without knowing which native tool
the module uses. For firewalld the values are `firewalld zone membership`,
`firewalld.service`, and "The previous firewalld zone membership is restored
in the running firewall and stored configuration." Ordering alone does not
start or enable the module's service.

A membership outcome carries `previous_membership_restored`, which becomes
true only after both layers match. It omits `destination_file`,
`previous_file_restored`, `previous_config_activated`, and
`obsolete_virtual_devices_removed`. Its record omits `previous_file_metadata`
and never creates `previous_file_contents`. A file outcome retains its file
fields and omits `previous_membership_restored`.

Both forms use `owed_restore_id`, `change_phase`, `change_started`,
`change_activation_failed`, and `change`. The last has the same meaning as
`change` in a node task result: `changed`, `unchanged`, or `unknown`.
The phase `applying_change` marks the durable boundary before the file
operation or native activation; `abandoned_before_change` means no change
started. The confirmation reasons `owed_restore_not_found` and
`owed_restore_resolved` refer to that same obligation.

The record's module name selects compiled activation code. Its private
`module_restore_data.membership_changes` holds each member's kind and name,
`previous_bindings`, and `proposed_bindings`. Each binding gives its `zone`
and `member_spelling`; the two array positions mean running, then stored.
These are recovery data, not playbook inputs.

## Reboot and wait

A `reboot` task restarts the node and makes the controller process wait for
the node to return. The reboot task is the last task of a fleet stage, so
the reboot marks a visible fleet stage boundary. The following fleet stage
patches web servers two at a time and reboots each node only when the update
requires a reboot:

```yaml
- id: patch_web
  nodes: web
  max_nodes_at_once: 2
  on_node_failure: stop_fleet_stage

  node_tasks:
    - role: os_updates

    - reboot:
        run_only_if_this_command_succeeds: test -f /var/run/reboot-required
        wait_at_most: 15m
```

`/var/run/reboot-required` is the file Debian and Ubuntu create when an update
needs a reboot. On the Red Hat family the check is
`skip_if_this_command_succeeds: needs-restarting -r`, which exits 1 when a
reboot is needed; the [`reboot` page](../../reference/modules/reboot.md#reboot-on-the-red-hat-family)
says why the form with `!` is unsafe.

The path guards `skip_if_this_path_exists` and `run_only_if_this_path_exists`
use the names from `shell`, so the condition is visible in the task. Reboot
checks relative paths from `/` on the node. Command guards still run as
before. The
[module interface decisions](modules.md#module-design-decisions) record the
names and reasons.

| Field | Default | Meaning |
|---|---|---|
| `run_only_if_this_command_succeeds`, `skip_if_this_command_succeeds` | none | Command checks with the [shell guard rules](../../reference/modules/shell.md#guards); a guard that disallows the reboot skips the task. |
| `wait_at_most` | `10m` | How long the controller process waits for the node to return on a new boot. |

A reboot without fields is written `reboot: {}`. The task also takes `id` and
`when`. Validation allows `reboot` only as the last task directly in a fleet
stage's `node_tasks` list. Validation rejects every reboot inside a role,
node task block, or node task loop, even at the end of the nested list.
A role that needs a reboot is called just before the `reboot` task,
as `os_updates` is above. Two reasons:

- The reader of the playbook sees where servers go down. A reboot inside a
  role would be hidden in the role's files.
- The reboot ends the node job. The end of a fleet stage is where the
  controller process already waits for each node, holds it in its
  `max_nodes_at_once` slot, and applies the failure policy.

Project [node task defaults](configuration.md#node-task-defaults-are-shared-project-configuration)
do not apply to a reboot task. Validation rejects `retry`, `time_limit`,
and `time_limit_per_try` at the node task level because a reboot runs once
per node job attempt. `reboot.wait_at_most` bounds the controller process's
wait.

The sequence:

1. The node job worker process evaluates the guards, then refuses a reboot of
   [the machine that runs the controller process](#mmx-never-reboots-the-controller-machine).
   If the reboot is allowed, the node job ends
   `rebooting`. The durable node job result records the current boot ID
   (`.node.boot_id`) with the reboot request.

   As the final action, the node job worker process runs `systemctl reboot`.
   After saving the durable result, the worker waits up to 5 seconds for a
   `rebooting` answer to go out. If the answer goes out during the wait,
   the worker waits one more second so the reboot does not cut off the
   answer. With no answer, the worker reboots after the five-second wait.
2. The controller process running the playbook sees the `rebooting` job and
   waits. The node keeps a `max_nodes_at_once` slot, and the controller
   process asks the node about the job every second. Connection failures
   during the wait are expected, not errors.

   A node may stop answering before the controller process receives
   `rebooting`, for example because the network went down. If the fleet
   stage ends in a reboot, the controller process retries job status
   requests every second within `wait_at_most`.
3. For each job status answer, the node HTTPS API process or node SSH
   request process compares the current boot ID with the recorded boot ID.
   The first answer on a different boot records the reboot task as
   succeeded and changed. The reboot task's value contains both boot IDs,
   `boot_id_before` and `boot_id_after`, and the fleet stage succeeds on the
   node. Later tasks
   in the next fleet stage check application readiness, for example with
   `wait_for`.
4. If the node has not answered on a new boot within `wait_at_most`, the node job
   fails with that reason. The fleet stage's
   [failure policy](#choose-what-a-node-failure-stops) applies. A refused
   `systemctl reboot` fails the task at once.

A stop requested before the reboot task keeps the node from rebooting, since the
node checks for a stop before each task. A stop during the wait lets the wait
finish; the reboot has already been requested. Without a controller, nothing
continues after the reboot: no later fleet stage starts.

`mmx resume` first asks the node about the saved node job if the original
controller process did not observe the reboot completing. The original
controller process may have stopped during the wait, or the wait may have
timed out. A node on a new boot answers that the reboot succeeded, so the
fleet stage succeeds without running again.

If the node still has the boot ID recorded before the reboot request,
the resume controller process polls job status every second because the
node may still be shutting down. Resume waits as in the run. If the node has
not rebooted before the wait expires, the fleet stage runs again from the
beginning and makes a new reboot request. A node is
never rebooted twice for the original node job's request. `mmx status`
asks for the status of a node job still waiting and records a completed
reboot.

Dryrun evaluates the guards and reports `would_change` with the reason, or the
skip; it does not reboot or wait. A rerun whose guard now disallows the reboot
skips it. A reboot task without a guard reboots on every run, like a bare shell
task runs on every run. After the reboot, service records follow the
[baseline rule](#missing-hashes-establish-a-baseline-before-changes), so the
next run causes no extra reload.

Module pages that rely on this section:
[`reboot`](../../reference/modules/reboot.md).

## Playbook fleet stages follow written order and explicit requirements

A playbook at `playbooks/<name>.yml` contains the required `fleet_stages`
list and may declare `command_line_vars`; project settings live in `mmx_project_settings.yml`
so playbooks do not repeat them. Each fleet stage selects nodes and gives each
ordered node tasks. The fleet stage selects the nodes and groups in `nodes`,
minus the nodes and groups in the fleet stage's optional `except_nodes`.
The `nodes` and `except_nodes` fields name nodes of the inventory.

A selection that reads only what the controller process knows
before it connects, such as `.vars`, resolves before any connection. A
selection can also read what an earlier fleet stage published
(`.outputs`). An output-dependent selection resolves during the run, when the
controller fleet scheduler reaches the fleet stage. Selections resolved
before connecting and selections resolved during the run are saved with
the run. `mmx resume` keeps the recorded selections.

All admitted node jobs in a fleet stage finish, including failure handling
and cleanup, before the next fleet stage is considered. Nodes within a
fleet stage progress independently. Requirements never reorder fleet stages.

An empty selection is an error: before any connection for a selection known
then, and as the fleet stage's failure for one that resolves during the
run. A fleet stage or `for_each_node` loop with
[`skip_when_no_nodes_are_selected: true`](configuration.md#leave-out-a-fleet-stage-that-selects-no-nodes)
is instead left out of the run and reported as not selected because no
nodes were selected. The report follows the format for a fleet stage
excluded by `--limit`. The omitted fleet stage or loop starts no node job.
The run records the empty selection, and `mmx resume` keeps the recorded
empty selection.

Later work reads the omitted fleet stage's or loop's whole output
collection as an empty mapping. A `requires.all_nodes` requirement on the
omitted fleet stage or loop is met.
A named node's output from the omitted fleet stage or loop is unavailable,
as for any node outside a fleet stage's or loop's selection.

The [configuration example](configuration.md#playbooks-contain-ordered-fleet-stages)
keeps roles such as `common` and `app_server` separate from workflow actions such
as a migration.

[Requirements](configuration.md#playbooks-contain-ordered-fleet-stages) name earlier
fleet stage IDs, on the same node or on all nodes. All listed requirements
apply. Unchanged success satisfies a requirement. Failed, skipped,
unreachable, blocked, unstarted, or uncertain jobs do not.
A requirement says that the work happened, so a node that the prerequisite's
`when` skipped does not meet it, although the prerequisite's whole output
collection [leaves that node out](configuration.md#outputs-and-fleet-stage-outcomes).
Unmet requirements are recorded as `blocked`, with the prerequisite fleet stage and
node, and the run returns nonzero. Resume does not rerun a skipped fleet stage. In
dryrun, work whose unmet prerequisites are all `not_checked` is itself
`not_checked`.

Reading an earlier fleet stage's
[published output](configuration.md#outputs-and-fleet-stage-outcomes)
for a node requires the producer fleet stage to have succeeded on the
node, without a separate `requires`.
Reading `.fleet_stages` outcome metadata does not require success.

### A limit narrows every fleet stage

`--limit` ([selector syntax](configuration.md#inventory-is-a-list-of-nodes-not-a-second-programming-language))
intersects each fleet stage's node selection with the named nodes and groups; it never
adds a node. A fleet stage whose intersection is empty is left out of the
run. For selections known before the run, the controller process rejects a
limit that leaves out a fleet stage another remaining fleet stage
`requires` before any remote work. The rejection names the excluded
prerequisite fleet stage and the dependent fleet stage instead of skipping
the requirement. For selections resolved during the run, unmet requirements
block the dependent work when the controller
fleet scheduler reaches the work.

The [example playbook](configuration.md#playbooks-contain-ordered-fleet-stages)
migrates the database on `db01` before configuring application servers.
With the playbook saved as `playbooks/deploy.yml`,
`mmx run deploy --limit web01` is rejected
because `configure_app_servers` requires
`migrate_database` on `db01`; `--limit web01,db01` runs the migration and
configures only `web01`. `requires.all_nodes` then means every node the
prerequisite selected after the limit.

Inside a [`for_each_node` loop](#cross-machine-work-uses-explicit-playbook-fleet-stages)
the limit works on the loop, not on the enclosed fleet stages. `--limit`
narrows the nodes the loop iterates over. Each remaining iteration runs all
enclosed fleet stages on the nodes named by the enclosed fleet stages,
also when a named node is outside the limit. A **helper node** is a node
that an iteration works on for the loop's
node, such as the load balancer that takes `app01` out of service. With
`--limit app01`, the loop runs the iteration for `app01` only, including its
fleet stages on the load balancer. Before connecting, `nodes`, `explain`,
`dryrun`, and `run` print the helper nodes under
"Nodes outside --limit that help selected nodes", so the limit never reaches
a node the user was not shown. Outside a `for_each_node` loop, a limit
narrows every fleet stage as described above.

The limit is saved with the run. `mmx resume` repeats the recorded, limited
selection and takes no `--limit`; to work on fewer nodes, start a new `mmx run deploy`
with a narrower limit.

### `--only-fleet-stages` keeps whole fleet stages

`--only-fleet-stages ID[,ID]` on `explain`, `nodes`, `dryrun`, and `run`
keeps the named fleet stages, in the order the playbook writes them, and
reports the others as not selected. A `loop` or `for_each_node` loop over
fleet stages is named by the loop's own `id` and is kept or left out whole.
The option never skips a prerequisite:
when a kept fleet stage `requires`, or reads outputs of, a fleet stage that
was left out, the controller process rejects the command before any
connection and prints an option value that includes the omitted
prerequisite fleet stage. An unknown ID is an
error that lists the playbook's fleet stage IDs. MMX never adds a fleet stage
that was not named.

For a `site` playbook whose fleet stage `platform_api_sites` requires
`nginx_passenger` on the same node, `mmx run site --only-fleet-stages
platform_api_sites` is rejected with the suggestion `--only-fleet-stages
nginx_passenger,platform_api_sites`. The option combines with `--limit` and
`--var`. The `--only-fleet-stages` selection is saved with the run.
`mmx resume` repeats the recorded fleet-stage selection and takes no
`--only-fleet-stages`.

## Run across the fleet with bounded concurrency

| Setting | Default | Meaning |
|---|---|---|
| `max_nodes_at_once` | `100` for a fleet stage; `1` for a `for_each_node` loop | Maximum active nodes. A project can set the fleet stage default as a count; a fleet stage can replace it with a higher or lower count, or with a percentage of its selected nodes. A `for_each_node` loop takes one node at a time unless it sets its own count or percentage. |
| `wait_for_all_in_batch` | `false` | Setting of a fleet stage or a `for_each_node` loop. When `true`, finish the current group before starting any node in the next group. The resolved `max_nodes_at_once` supplies the group size. |

By default, each node finishing its fleet stage frees a slot for another: with
`max_nodes_at_once: 5`, the sixth node starts as soon as any of the first five
finishes. With `wait_for_all_in_batch: true`, the sixth node waits for all
five nodes to finish. Batches follow inventory node-ID order.
`max_nodes_at_once: 1` gives one node at a time. A node stays
active through its whole fleet stage, including verification, failure handling, and
cleanup. The default of 100 is easy to remember, not a measured optimum.

A percentage, such as `max_nodes_at_once: "34%"`, counts the nodes the fleet
stage or loop selects after any `--limit`, rounds down, and is never less
than one node: 34% of six nodes is two at a time, and 34% of two nodes is
one. The project setting stays a count, because a project has no single
selection to take a percentage of. `explain` shows the resolved number for a
selection known before the run.

Neither `max_nodes_at_once` nor `wait_for_all_in_batch` stops the rollout on
failure. The fleet stage's
[`on_node_failure` policy](#choose-what-a-node-failure-stops) does, and
[fleet stage requirements](#playbook-fleet-stages-follow-written-order-and-explicit-requirements)
apply regardless. Dryrun uses the same bounded concurrency.

Unlike Ansible's separate `forks` and `serial`, MMX uses one node count, filling
free slots immediately the way Salt's `--batch-size` does, and adds the group
boundary only on request.

## Choose what a node failure stops

Each fleet stage can set `on_node_failure`, with `continue` as the default.
The `on_node_failure` policy controls whether additional nodes and later
fleet stages start. The failed node still runs failure handling and
cleanup. Under every policy, the failed node starts no later fleet stage.

| Value | Additional nodes in this fleet stage | Later playbook fleet stages |
|---|---|---|
| `continue` (default) | Continue processing the selected nodes. | Proceed on other nodes subject to `requires`. |
| `stop_fleet_stage` | Stop starting additional nodes in this fleet stage. | Proceed on other nodes subject to `requires`. |
| `stop_run` | Stop starting additional nodes in this fleet stage. | Do not start later fleet stages. |

The default lets a 10,000-node run finish with 9,997 successes and three
failures; the run returns nonzero and `mmx resume` selects the unfinished work.
Use `stop_fleet_stage` to stop a failing rollout from spreading while independent later
fleet stages proceed, and `stop_run` for a coordinated update that should end on
failure.

A node fails the fleet stage when a task reaches its final required failure
after retries. The node also fails the fleet stage when the node job ends
unreachable or uncertain. The policy
triggers on the first failed node, or on the first beyond an
[allowance](#allow-some-failures-before-stopping). The controller fleet
scheduler stops starting node jobs as soon as the scheduler observes the
failure. Admitted node jobs finish the current fleet stage normally.
Admitted node jobs are not cancelled mid-change.

Node jobs, later fleet stages, and loop iterations prevented from starting
by a stop are recorded `stopped` with the cause. Only unmet prerequisites and
unavailable inputs are `blocked`. The policy is saved with the run, and
[resume](workflow.md#resume-uses-the-recorded-run) applies it again.

A [`for_each_node` loop](#cross-machine-work-uses-explicit-playbook-fleet-stages)
carries the policy itself: `on_node_failure` and `failed_nodes_allowed` are
written on the loop and count the loop's nodes, and validation rejects them
on the fleet stages inside it. A loop node fails when one of the loop's
enclosed fleet stages fails for it, on that node or on a helper node.

### Allow some failures before stopping

`failed_nodes_allowed` lets additional nodes start until the failed-node
count exceeds the allowance and `on_node_failure` stops further starts.
`failed_nodes_allowed` is a fleet stage setting, like the other
[rollout settings](configuration.md#rollout-settings-belong-to-the-fleet-stage), and
takes a count (`5`) or a whole-number percentage, at most 100, of the fleet stage's
selected nodes after any `--limit` (`"2%"`, rounded down). The default `0` stops
at the first failure. Validation rejects `failed_nodes_allowed` unless
`on_node_failure` is `stop_fleet_stage` or `stop_run`, since `continue` never
stops. A plain `loop` rejects
`failed_nodes_allowed` on the loop. Each enclosed fleet stage counts the
fleet stage's own selection in each iteration. A `for_each_node` loop
takes `failed_nodes_allowed` on the loop, counted over the loop's nodes.
`explain` shows the allowance and, for a selection known before the run,
the node count it allows.

```yaml
- id: patch_web
  nodes: web
  max_nodes_at_once: 20
  on_node_failure: stop_fleet_stage
  failed_nodes_allowed: "2%"

  node_tasks:
    - role: os_updates
```

With 500 web nodes, ten may fail. When the controller fleet scheduler
observes the eleventh failed node, the scheduler stops starting additional
nodes in the fleet stage, as `stop_fleet_stage` does without an allowance.
Every failed node counts, including a [reboot](#reboot-and-wait) that
did not return in time.

The count covers the whole fleet stage, not each batch. Nodes already
admitted when the failure allowance is crossed finish their fleet stage,
so the final number of failures can exceed the allowance by up to
`max_nodes_at_once`. With
`wait_for_all_in_batch: true`, the current group finishes and the next group
does not start. Choose a `max_nodes_at_once` that is small relative to the
allowance when that overshoot matters.

The allowance is saved with the run. Resuming counts only failures in the
current resume attempt, against the saved `failed_nodes_allowed` value and
the fleet stage's recorded selection.

### A `when` on a fleet stage is checked for each node as it starts

A `when` on a fleet stage, or on a `for_each_node` loop, is evaluated by the
controller process once for each selected node, just before that node would
start. A node whose condition is false is recorded `skipped` with the
condition as the reason. The skipped node starts no node job, takes no
`max_nodes_at_once` slot, and is not part of a batch: with
`max_nodes_at_once: 2` and one skipped node among five, the other four still
go two at a time. A skipped node is
not a failure and does not count against `failed_nodes_allowed`.

The condition can read what earlier fleet stages published for that node, so
a run can find out which database server is the leader and then update only
the others. A fleet stage's `nodes` can also read an earlier fleet stage's
output; that selection resolves during the run
([written order](#playbook-fleet-stages-follow-written-order-and-explicit-requirements)).
Whether `when` skips nodes or `nodes` selects nodes from earlier outputs,
every node a run may touch is a node of the inventory. `mmx nodes`
lists a fleet stage that has a `when` with "narrowed during the run by
`when`", and a fleet stage whose selection reads an output with "resolved
during the run". `mmx resume`
keeps the result of a `when` that was already evaluated for a node; a new
`run` evaluates it again.

## Cross-machine work uses explicit playbook fleet stages

A migration is an explicit task in a fleet stage targeting the database or
migration node. The application fleet stage requires the migration fleet
stage through `requires.all_nodes`. The migration task runs once per run on
one node, not once per application server. The migration's own mechanism
makes rerunning the migration safe. MMX does not promise exactly-once side
effects.

Some work for one node happens on another node: taking an app server out of
the load balancer before changing it, or creating a virtual machine on its
hypervisor. A `for_each_node` loop expresses work on another node as ordinary
fleet stages. The loop names a set of nodes and a variable. For each named
node, the loop runs the enclosed fleet stages in written order. Each
enclosed fleet stage uses `nodes` to say where the fleet stage runs. The
controller process owns the loop. The
[configuration contract](configuration.md#loop-over-nodes-with-for_each_node)
owns the syntax; this section owns what happens.

The inventory `inventories/production.yml` has three app servers behind one
load balancer:

```yaml
nodes:
  app01: {address: app01.example.net, ssh_user: deploy}
  app02: {address: app02.example.net, ssh_user: deploy}
  app03: {address: app03.example.net, ssh_user: deploy}
  loadbalancer: {address: lb.example.net, ssh_user: deploy}

groups:
  app_servers:
    nodes: [app01, app02, app03]
```

The playbook `playbooks/deploy.yml` updates the app servers one at a time:

```yaml
command_line_vars:
  release:
    type: string

fleet_stages:
  - id: rollout
    for_each_node: app_servers
    as: app_server
    max_nodes_at_once: 1
    on_node_failure: stop_run
    fleet_stages:
      - id: inspect
        nodes: "{{ .app_server }}"

        node_tasks:
          - id: installed_release
            shell:
              script: printf %s "$(cat /srv/shop/RELEASE)"
              dryrun: printf %s "$(cat /srv/shop/RELEASE)"
              changed_when: false
        outputs:
          update_needed:
            not: {equals: ["{{ .node_tasks.installed_release.stdout }}", "{{ .vars.release }}"]}

      - id: drain
        nodes: loadbalancer
        when: "{{ .outputs.inspect[.app_server].update_needed }}"

        node_tasks:
          - shell:
              script: /usr/local/bin/lb-drain "$APP_SERVER"
              env: {APP_SERVER: "{{ .app_server }}"}

      - id: update
        nodes: "{{ .app_server }}"
        when: "{{ .outputs.inspect[.app_server].update_needed }}"

        node_tasks:
          - role: shop_app
            inputs:
              release: "{{ .vars.release }}"
          - wait_for:
              url_responds: http://127.0.0.1:8080/health
              expected_http_status_codes: [200]
              wait_at_most: 60s

      - id: rejoin
        nodes: loadbalancer
        when: "{{ .outputs.inspect[.app_server].update_needed }}"

        node_tasks:
          - shell:
              script: /usr/local/bin/lb-rejoin "$APP_SERVER"
              env: {APP_SERVER: "{{ .app_server }}"}
```

How to read the references:

- `.app_server` is the loop variable: the name of the loop's current node,
  `app01`, then `app02`, then `app03`.
- `nodes: "{{ .app_server }}"` runs that fleet stage on the loop's current
  node. `nodes: loadbalancer` runs the fleet stage on the load balancer.
  In `playbooks/deploy.yml`, the load balancer is a helper node: the load
  balancer works for the app server named by `.app_server`.
- `.outputs.inspect[.app_server].update_needed` is the output named
  `update_needed` that the fleet stage `inspect` published, in this
  iteration, for the node named by `.app_server`.
- `printf %s "$(cat /srv/shop/RELEASE)"` prints the file without its final
  newline, because `stdout` keeps every character a command prints. A bare
  `cat` would give `1.4.3` followed by a newline, which never equals
  `1.4.3`, so every server would be drained on every run.
- `lb-drain` and `lb-rejoin` are the site's own scripts. `lb-drain` exits zero
  when the app server named by `APP_SERVER` is already drained.
  `lb-rejoin` exits zero when the app server named by `APP_SERVER` is already
  rejoined. Repeating `lb-drain` or `lb-rejoin` is therefore safe.

What a run does:

- `mmx run deploy --var release=1.4.3` runs the four fleet stages for
  `app01`, then for `app02`, then for `app03`. For a server that already has
  release 1.4.3, `update_needed` is false: `drain`, `update`, and `rejoin`
  are skipped, and the load balancer is not touched for it.
- `mmx run deploy --var release=1.4.3 --limit app02` runs only the iteration
  for `app02`, including its `drain` and `rejoin` on the load balancer, and
  first prints `loadbalancer` under
  "Nodes outside --limit that help selected nodes"
  ([limits](#a-limit-narrows-every-fleet-stage)).
- If `update` fails on `app02`, the iteration for `app02` ends there: `rejoin`
  does not run, so the broken server stays out of the load balancer. Because
  of `on_node_failure: stop_run`, the iteration for `app03` does not start.
  `mmx resume` repeats `app02` from `update`, its first unfinished fleet
  stage, and then continues with `app03`.
- `mmx dryrun deploy --var release=1.4.3` runs the `rollout` loop and drains
  nothing. The `inspect` fleet stage runs the command declared in `dryrun`.
  The `drain`, `update`, and `rejoin` fleet stages report the work each fleet
  stage would do.

The loop's rollout settings count the loop's nodes:

- A node admitted to the loop runs its enclosed fleet stages in order and
  keeps its `max_nodes_at_once` slot until its last one ends. With
  `max_nodes_at_once: 2`, two app servers are in their sequence at the same
  time; with `wait_for_all_in_batch: true`, both finish before the next two
  start. Without `max_nodes_at_once`, the loop takes one node at a time.
- A helper node works for one loop node at a time. When two loop nodes need
  the same helper at once, the second waits for the first's fleet stage on
  that helper to end.
- A failure on the loop node or a helper node ends the sequence for the loop
  node whose iteration failed. Already-started iterations for loop nodes
  without a failure finish the full sequence, so a healthy drained server
  is still rejoined.
  The loop's `on_node_failure` and `failed_nodes_allowed` then decide whether
  further loop nodes start
  ([failure policy](#choose-what-a-node-failure-stops)).
- A `when` on the loop is checked for each loop node just before it starts;
  a node that fails it is skipped and takes no slot
  ([rule](#a-when-on-a-fleet-stage-is-checked-for-each-node-as-it-starts)).
- To reboot in the middle of a sequence, split the sequence into two enclosed
  fleet stages. Put the work before the reboot in the first fleet stage,
  with `reboot` as the last task. Put the work after the reboot in the second
  fleet stage, because `reboot` must end a fleet stage.

There is no task delegation or `run_on`: `.node` is always the node a fleet
stage runs on, roles describe server configuration, and the playbook owns
cross-node ordering. A plain `loop` over fleet stages is for lists that
are not nodes, such as a list of databases to migrate; `--limit` does not
pick its iterations.

Rejoin follows a successful update and its readiness check, not cleanup.
Drain and readiness waits use `wait_for` for TCP ports, paths, HTTP
readiness, and a `script` that asks the cluster. A custom drain command can
use node task retries when its completion needs a script-specific check.

The author chooses the change check, commands, and readiness test. Where
draining must precede a live change, inspect or stage the change first rather
than reacting to a file task's changed result. A healthy unchanged run leaves
pool membership alone. Canarying is the same pattern: select a canary, run,
verify, then select the rest.

### Conditional fleet stage sequences use ordinary run results

The [rolling-deployment example](../../../examples/rolling-deployment/README.md)
checks the app's loaded configuration and its load-balancer membership. A healthy
unchanged run makes no traffic changes; an app left drained is verified and
rejoined; the loop's `stop_run` policy keeps a failed app out of traffic and
stops work on the next one. Application scripts make repeated actions safe by
inspecting actual state; a new run uses current definitions and observations.

### Completed outputs survive resume

Declared outputs are evaluated after the producer's tasks succeed and recorded
with its fleet stage, node, and loop key. If output evaluation or recording fails, the
producer fails, and its actual task results are kept.

`mmx resume` restores outputs from completed producers instead of
rerunning them, and keeps the saved run's node selections, loop items,
definitions, and inventory. Each new attempt of an unfinished fleet stage
receives restored outputs from completed producers and outputs produced by
work completed during resume. Saved outputs are observations from when the
producer ran; a workflow that needs current state checks again. A new
`mmx run deploy` reevaluates everything.

Outputs derived from secrets follow the
[secret-input contract](configuration.md#delivery-and-output).

### Secret outputs that come from a node are never saved

A value can become secret on a node: a `read_file` task with
`content_is_secret: true` reads a token that a program generated there, or a
`shell` task with `output_is_secret: true` prints one. An output made from
such a value is secret without further marking. The secret output passes
through the controller process's memory only to nodes whose tasks use the
output. `mmx results` shows `[redacted]` for the secret output. The secret
output is never written to the controller machine's run history or to a
node's saved results.

Because the value is not saved, `mmx resume` cannot restore it from the
recorded run the way it restores an ordinary output:

- For a `read_file` secret, the saved run keeps the source node and path.
  `mmx resume` connects to the source node and rereads the secret file at the
  saved path before starting fleet stages that use the output. If the source
  node cannot be reached or the secret file is gone, fleet stages that use
  the output are `blocked` with the reason.

  Saving the source reference and rereading the secret file work only for an
  output that passes the complete secret file content unchanged.
  For example, `"{{ .node_tasks.join_token.content }}"` keeps the source
  reference. An output that adds text around the secret file content, such
  as `"K10{{ .node_tasks.join_token.content }}"`, is computed on the node and
  keeps no source reference. On resume, fleet stages that use the modified
  output are blocked as for a `shell` secret. Put the added text where the
  secret output is consumed instead.
- A `shell` secret has no such reference, and MMX does not run a completed
  command again to get its output. On resume, the fleet stages that use the
  output are `blocked`, with a message that names the producing fleet stage
  and task and
  says to start a new `mmx run`. A value that later fleet stages need after
  an interruption therefore belongs in a file on the node, read with
  `read_file`.

### Files saved on the controller machine

A fleet stage's `save_on_controller_machine` list writes outputs of that
fleet stage to files on the controller machine, after the fleet stage's nodes
finish. The controller process writes the files; no task runs on the
controller machine. `mmx resume` writes the files that are still missing,
from restored outputs, and from a secret read again as above.

A file that holds a secret value follows the
[read-bit rule](../../reference/modules/file.md#files-that-hold-secrets), and its content
never enters saved history. Dryrun reports `would_change` with the difference
and writes nothing. The
[configuration contract](configuration.md#outputs-and-fleet-stage-outcomes)
owns the fields.

## Ad hoc commands are ordinary runs

[`mmx run-command`](workflow.md#run-a-one-off-command-across-nodes) builds a
run with one fleet stage and one shell task for the selected nodes. The
shell task includes `run_as_user_account`. The run uses the project's
`max_nodes_at_once`. The fleet stage uses `on_node_failure: continue`.

The ad hoc run uses ordinary execution: setup, the node lock, and detached
node jobs.
The ad hoc run supports ordinary stopping and saves results.
The `mmx run-command` command form is
`mmx run-command --nodes SELECTOR [--run-as-node-user-account NAME] -- COMMAND`.
The recorded ad hoc run has the entry point `run-command` and is selected
only by run ID.
`mmx status <run-id>`, `mmx results <run-id>`, `mmx stop <run-id>`, and
`mmx resume <run-id>` treat the ad hoc run like any recorded run. Resume
runs the command again on failed and unstarted nodes. The ad hoc run uses
the ordinary resume, secret, and history models. The `mmx run-command`
command has no dryrun, because a bare command has nothing to inspect.
A node with [`/etc/mmx/do_not_manage`](#a-node-can-refuse-management) refuses
the ad hoc node job like any job that could change the node.

## Recovery boundaries

The node HTTPS API process rejects a request body it cannot read within its
limits with HTTP 413, `request body exceeds limit`. A slow body can therefore
receive the size-limit wording; the request is still rejected before admission.

A lost HTTPS read falls back to SSH. If SSH cannot
connect, the node job stays `unknown`. When connectivity returns,
`mmx status` can recover the node job's result. Failed HTTPS reads followed
by failed SSH connections need no additional transport state.

The node can admit a delayed submission after a status request returned
`not_found` for the submitted node job. Late admission cannot overlap
another node job, because the node admits only one active job. A recorded
stop is checked before the first node task, so late admission does not
bypass the stop.

A blocked fleet stage fails the run and reports the unmet input. Work that
must not follow the blocked fleet stage declares `requires.same_node`.
A blocked fleet stage alone adds no implicit dependency to later fleet
stages. A `same_node` requirement on a fleet stage loop can therefore block
the run when the required per-node result does not exist. The blocked
reason names the requirement.

Cleanup that reads a variable assigned in a node task block phase the failed
attempt never reached can likewise pass validation and fail on the node.
The cleanup failure is reported beside
the original failure, so reading a variable before the variable is available
remains a visible authoring error.

Nodes already connecting or uploading within `max_nodes_at_once` can still
submit their jobs after a failure policy stops new work. Nodes already
connecting or uploading when the failure policy takes effect finish
normally. Cancellation across workers would add coordination only for the
interval between starting a connection or upload and submitting the node
job after the failure policy stops new work.

The node task execution engine tracks the input paths declared in a node
task block's `changes` section, using the `when` values at the node task
block's start. Tasks with resolved arguments still write in written order
if a later condition changes during the block. A task excluded at block
entry because its `when` was false and its arguments did not resolve fails
if a task inside the block makes the excluded task's `when` true. Keeping
the tracked input set unchanged throughout the node task block permits
architecture-specific download tasks to share a destination.
Two tasks or roles that assign different contents to one path will rewrite
the path on each run and repeat the path's service action. The reported
changes expose the
conflicting configuration rather than resolving the conflict implicitly.

A node task time limit kills a command's process group immediately, without
a graceful shutdown interval. A tool such as certbot can leave a challenge
file or DNS record if interrupted during a request; its next request
replaces that state. A graceful interval would delay every time limit and
stop that must terminate a command.

## Execution decisions

The following execution decisions are settled. Each decision includes the
reason.

- **D2 No handlers or `notify`.** Services refresh from configuration, unit,
  package, and checkout hashes. A custom activation command runs in written
  order. With `only_when_tracked_inputs_changed: true`, the custom activation
  command tracks configuration, unit, package, and checkout hashes through
  a separate command record. Tracking input hashes in service and command
  records ensures activation after an interrupted change, without a second
  notification graph. Every action required by a tracked input change uses
  input hashes and saved records.
- **A module that is not core gives its own baseline.** Where a record holds
  no fingerprint for the module, a module task that changes nothing sets the
  baseline, and one that changes something refreshes the service or runs the
  command. A missing fingerprint alone is no reason to refresh: every such
  service would restart once after each reboot, although nothing changed. A
  run that ends after the module's change and before the refresh must not
  lose the refresh. Before the node task block's first write, MMX records
  an explicit entry that says the module's fingerprint is unknown. An absent
  fingerprint permits a baseline, but an entry marking the fingerprint as
  unknown preserves the owed refresh after an interrupted change.
- **D11 No `become`.** The node runner executable runs as root.
  `shell.run_as_user_account` names
  an application account at the command, so MMX has one privilege path and
  nothing to escalate.
- **D12 No `delegate_to`, `run_on`, or second execution node.** Cross-node work
  is explicit fleet stages, and a `for_each_node` loop runs one node's share
  of the work on its helper nodes, so the playbook shows which node does what,
  in what order.
- **D13 `run_once` becomes a fleet stage selecting one node** with
  `requires.all_nodes` for its dependants, so a migration visibly runs once
  per run.
- **D14 `forks` and `serial` become `max_nodes_at_once`** plus
  `wait_for_all_in_batch`: one node limit and one optional group boundary. On
  a fleet stage or a `for_each_node` loop the limit is a count or a
  percentage of the selected nodes, as `failed_nodes_allowed` is; the project
  setting is a count.
- **D15 `any_errors_fatal` becomes `on_node_failure: stop_run`**, with
  `stop_fleet_stage` as the narrower form and `failed_nodes_allowed` in place of
  `max_fail_percentage`, so one fleet stage setting names what a failure stops.
- **D22 No multi-file transaction or rollback flag.** A node task block refuses to
  activate a known-invalid result; restoring arbitrary files is the author's
  script, not a general MMX mechanism.
- **A module-owned network file is accepted only after new connections confirm reachability.**
  Existing connections can survive a change that refuses new ones. A durable
  owed restore and a systemd timer recover the one owned file without the
  controller process, through one restore service even after worker death or
  node restart. Restoring the module-owned network file does not add node
  task block rollback.
- **D28 No `async` or `poll`.** Node jobs already survive controller
  disconnects, and a node's tasks run in written order; add within-node
  concurrency only for a concrete need.
- **D30 `block/rescue/always` become a node task block's `on_failure` and
  `cleanup`,** for the current attempt only, because replaying saved cleanup
  after a crash or an edited playbook would run stale actions.
- **D37 `run` uses current files on the full selection; `resume` uses the
  recorded run.** `resume` names the run-level action, leaving `retry` for a
  node task's automatic repeats; `stop` and `resume` form a natural pair.
- **`--only-fleet-stages` selects whole fleet stages and never skips a
  prerequisite.** A fleet stage declares what it requires, so a selection of
  fleet stages can be checked before connecting; a tag on a task cannot.
- **A node with `/etc/mmx/do_not_manage` fails; it is not skipped.** A run
  that left a selected server unconfigured must not look like a success.
- **MMX never opens port 7443 itself.** A firewall rule is the
  administrator's decision and belongs in the project, where a reviewer sees
  it.
- **No `refresh: reboot`.** A [reboot](#reboot-and-wait) restarts every
  service and ends the job, so it is an explicit, guarded task at a fleet stage
  boundary, not an effect hidden in one service's configuration hash.
- **D47 The controller machine is an ordinary node, reached over SSH or
  HTTPS, and is never rebooted by MMX.** A local execution mode would be a
  second model to build, test, and explain; the ordinary path already gives
  dryrun, saved results, and resume. A reboot is the one built-in action that
  ends the controller process itself, so it is refused, with no override,
  by machine identity rather than by address. The controller process sends
  the SHA-256 of its machine ID, never the ID, which systemd asks to keep
  from other machines. A controller machine without a machine ID is still
  recognized by its boot ID, because a Docker image often has no
  `/etc/machine-id`.

## Tests

Tests exercise the following behaviors on real nodes, on each supported
distribution:

- Bootstrap with keys, passwords, root, and sudo, also with SELinux
  enforcing, and with a firewall that blocks port 7443.
- A node that refuses management.
- Administrator-key changes.
- Application-user commands.
- Service refresh across interrupted runs and reboots.
- A command with `only_when_tracked_inputs_changed` across interrupted runs
  and reboots.
- A checkout that restarts the checkout's service.
- Packages that start no service during installation.
- `checks_before_restart_or_reload`.
- The reboot task, the reboot task's `wait_at_most`, and resuming interrupted
  reboot work.
- Validation and cleanup failures.
- Stopping.
- Bounded concurrency, percentages, failure policies, and failure allowances.
- Fleet stage requirements, limits, `except_nodes`, and `--only-fleet-stages`.
- A `when` checked for each node.
- Secret outputs from a node across resume.
- The last-successful-run record.
- Ad hoc commands.
- A rolling deployment with `for_each_node`, drain, and rejoin, across the
  full selection and under a limit.

The [test plan](development.md#automated-code-and-workflow-tests) owns coverage.

## Beyond the first release

- Overlay-network enrollment (Tailscale, NetBird), which would belong to
  bootstrap rather than the task protocol.
- Keeping the saved state on the controller machine across an upgrade, so
  that `mmx resume` can finish a run that the upgrade interrupted. Today an
  upgrade may discard the saved state on the controller machine
  ([compatibility between releases](README.md#compatibility-between-releases)).
- `on_error: continue` for optional work.
- Removing old node runner executables. Each runner build stays on the node
  in its own `/opt/mmx/bin/<sha256>/` directory, about 17 MB. Upgrading the
  `mmx` controller executable adds another runner build on the node.
  The first upgrade after 0.1.0 leaves an old node runner executable for the
  first time. A runner directory can go once no node job worker
  process or node HTTPS API process runs from it.
