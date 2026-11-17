# How MMX works

This page follows one playbook from your keyboard to your servers and
back. For each part of MMX, this page says what happens, on which
machine, and why the step is safe. The
[documentation index](README.md) lists the pages that show how to do each
job, and the reference pages for every command and setting.

## The controller machine and the nodes

You run `mmx` commands on your **controller machine**, usually your laptop
or a continuous integration (CI) machine. The Linux servers that MMX
manages are called **nodes**. A node must run systemd.

MMX has two programs, both written in Go:

- `mmx` runs on the controller machine. Each running `mmx` command is a
  **controller process**, which ends when the command finishes.
- `mmx-runner`, the **node runner executable**, runs on each node. The
  first `mmx` command that connects to a node installs `mmx-runner` on
  that node.

```text
CONTROLLER MACHINE (your laptop)               NODES (your Linux servers)

  you type: mmx run set_up_web_servers           web01        web02
  mmx reads the project files     -- work -->    each node does its own work
  mmx shows and saves results     <-- results -- and keeps its own results
```

The controller machine decides which nodes work, and when. Each node does
its work on itself and keeps its own results. So a node finishes its
current node job even if your laptop goes to sleep in the middle of a run.
The later fleet stages wait until you run `mmx resume`.

No MMX service runs on the controller machine between commands. On each
node that MMX reaches over HTTPS, which is every node without
`connection: ssh_only`, `mmx-runner` runs as the systemd service
`mmx-https-api.service`, so that `mmx` can reach the node over HTTPS.

A Linux controller machine can also be a node in its own inventory. MMX
then never reboots the controller machine:
[Manage the controller machine itself](how-to/manage-the-controller-machine-itself.md)
says how to set that up.

## Your project describes your servers

An MMX **project** is a directory of plain files on the controller machine.
Keep the project in Git, so that each change to your servers starts as a
commit that you can review and undo.
This page uses the project `shop`, which has two web servers:

```text
shop/
  mmx_project_settings.yml          the project's settings, by which MMX finds the project
  inventories/example.yml           the inventory: your servers and how to reach them
  playbooks/set_up_web_servers.yml  a playbook: the work for your servers
  roles/                            reusable lists of work (optional)
  external_modules/                 modules of the project's own (optional)
  secrets_vault.age                 the encrypted vault of secrets (optional)
  .mmx/                             what MMX saves on the controller machine, kept out of Git
```

You can run `mmx` from any directory inside the project. MMX looks for
`mmx_project_settings.yml` in the current directory and then in each
parent directory, as `git` looks for a repository.

The **inventory** lists the nodes, how to reach each node over SSH, and
groups of nodes:

```yaml
# inventories/example.yml
nodes:
  web01:                     # a node name you choose
    address: 203.0.113.11
    ssh_user: root           # root, or an account that can run any command with sudo
  web02:
    address: 203.0.113.12
    ssh_user: root

groups:
  web_servers:               # a group name you choose
    nodes: [web01, web02]
```

A **playbook** lists the work. The playbook `set_up_web_servers` installs
nginx, writes one site file, and reloads nginx only when the site file
changed:

```yaml
# playbooks/set_up_web_servers.yml
fleet_stages:
  - id: set_up_nginx                 # a fleet stage
    nodes: web_servers               # the nodes that do this fleet stage's work
    node_tasks:                      # each node of the group runs these node tasks, in order
      - id: install_nginx
        apt_package:                 # a module
          packages:
            - {name: nginx, version: newest_when_first_installed}

      - task_block:                  # a node task block
          id: shop_site
          changes:                   # first, make the change
            - id: write_site_file
              file:
                path: /etc/nginx/conf.d/shop.conf
                permissions_mode: "0644"
                content: |
                  server {
                    listen 8080;
                    server_name {{ .node.name }}.example.com;
                    root /srv/shop/public;
                  }
          activate:                  # then, put the change into use
            - id: reload_nginx
              systemd_service:
                name: nginx
                start_at_boot: true
                state: running
                refresh: reload
```

A playbook has these parts:

- A **fleet stage** is one step of a playbook. The fleet stage picks
  nodes, such as the group `web_servers`, and lists the work that each
  selected node does. Fleet stages run in the order you write them: for
  example, a fleet stage for the database servers, then one for the web
  servers.
- A **node task** is one piece of work on one node, such as
  `install_nginx`. Each node runs its own copy of the node tasks, in
  order.
- A **module** is the code that does the work of a node task, such as
  `apt_package`, `file`, or `systemd_service`. A node task names one
  module, with the module's inputs below the module's name.
- A **node task block** ties a change to what follows the change. In the
  node task block `shop_site`, nginx reloads only when the site file
  changed. A node task block can also check the change before nginx loads
  the change, and check the site afterwards.
- A **role** is a list of node tasks in its own file, with inputs, that
  several playbooks can call. `mmx` replaces each role call with the
  role's node tasks when `mmx` reads the playbook, so the role's node
  tasks run on a node like any other node task.

`{{ .node.name }}` is a value that MMX fills in for each node: `web01` on
`web01`, `web02` on `web02`. Template files and values that only the node
knows, such as the node's memory size, are filled in on the node.

[Write a playbook](how-to/write-a-playbook.md) builds the
`set_up_web_servers` playbook step by step.
[Reuse work with roles](how-to/reuse-work-with-roles.md),
[Use variables and templates](how-to/use-variables-and-templates.md), and
[Roll out a change gradually](how-to/roll-out-a-change-gradually.md) cover
roles, values, and how many nodes work at once.

## What happens when you run a playbook

Here is what `mmx run set_up_web_servers` does, in order.

**On the controller machine:**

1. The controller process reads the project settings, the inventory, the
   playbook, and the roles. The controller process checks everything that
   it can check without a node, such as unknown fields and missing role
   inputs. A mistake stops the command before any node is contacted.
2. The controller process gives the run an ID. The controller process
   saves a copy of the playbook, the inventory, and every file, template,
   and module to send, in `.mmx/runs/<run-id>/`. The record of one run,
   with the run's results, is a **playbook run**. `mmx resume` later uses
   the saved copy, even after you edit the project.
3. The controller process starts the first fleet stage. The controller
   process starts as many nodes at once as the fleet stage's
   `max_nodes_at_once` allows.

**For each node:**

4. The controller process connects to the node. The first connection sets
   up MMX on the node over SSH, as
   [The two connections](#the-two-connections-ssh-and-https) describes.
5. The controller process records an ID for the node's work, then sends
   the **node job**: the work of one fleet stage on one node, with the
   files that the work needs. The files travel apart from the node job's
   instructions, so a large file needs no setting. A file that the node
   already keeps from an earlier run, such as a module file, is not sent
   again. The node job's instructions are JSON of at most 16 MiB: they
   hold each file's size and SHA-256, not the file's bytes, so the limit
   does not apply to the files. A file upload may take longer than 30
   seconds as long as it makes progress. If no bytes move for 30 seconds,
   the transfer ends and the node job does not start. The node removes a
   file upload for a node job that never started after 72 hours.
6. On the node, `mmx-runner` checks the SHA-256 of each file, and saves
   the node job in `/var/lib/mmx/jobs/<job-id>/`. Then `mmx-runner`
   starts a **node job worker process** for the node job. A node runs one
   node job at a time, and refuses a second node job as busy.

Once the node job worker process starts, the node job does not depend on
the connection. If your laptop sleeps or the network drops, the node job
worker process keeps going.

**On the node:**

7. The node job worker process first looks for the file
   `/etc/mmx/do_not_manage`. A person creates that file by hand to keep MMX
   off a server while the person works on the server. If the file exists,
   the node job fails before any node task runs:
   [Keep MMX off a server](how-to/keep-mmx-off-a-server.md) explains the
   file.
8. The node job worker process measures the node's facts, such as its
   distribution and memory, once for the node job. Then the node job
   worker process runs the node tasks in order. The node job worker
   process applies each node task's conditions, loops, retries, and time
   limits.
9. Each module checks the node's current state and changes only what
   differs. The node job worker process saves progress as the work goes,
   and then the result, on the node.

**Back on the controller machine:**

10. The controller process collects the progress and the results, shows
    them, and saves them in the playbook run.
11. When every node of the fleet stage has finished, the controller
    process starts the next fleet stage. A value that one fleet stage
    passes to a later fleet stage travels through the controller process,
    because nodes never contact each other:
    [Use a result from one server on another](how-to/use-a-result-from-one-server-on-another.md)
    shows how.
12. After the last fleet stage, each node where every fleet stage
    succeeded records the run in
    `/var/lib/mmx/last_successful_run/<project_name>/<playbook>`.
    Monitoring on the node can then see when the playbook last succeeded
    on the node.

Every result keeps two facts apart: whether the node task **succeeded**,
and whether the node task **changed** anything. A node task can fail after
it changed something, or succeed without changing anything. Run the
playbook again with nothing new, and each node task reports `unchanged`.

## A dryrun shows the changes without making them

`mmx dryrun set_up_web_servers` follows the same path as a run, with node
jobs and facts, but each module only checks:

- A core module inspects the node and reports what it would change.
- A `shell` node task runs its own `dryrun` command, if it has one.
  Without one, MMX cannot know what the command would do, so the dryrun
  skips the `shell` node task and reports its change as `unknown`.
- A module of the project's own runs in a check mode, and its author
  makes sure that the check mode changes nothing:
  [Modules](#modules) says how.

MMX's own modules change nothing on a node during a dryrun, apart from
MMX's own setup. A `shell` node task's `dryrun` command, a program
module's check mode, and a script's `run_command` still run as root, and
their authors make sure that they change nothing. The first
dryrun on a new server copies `mmx-runner` to `/opt/mmx/bin/` and keeps a
record in `/var/lib/mmx/`. It starts no service, opens no port, and adds no
SSH key: it works over SSH until the first `mmx run` starts
`mmx-https-api.service` on port 7443.
Here is the first dryrun of [Get started](get-started.md):

```text
Run fbe759f2
Project: mmx_example   Playbook: example
Inventory: example (1 node)   Selected: 1 node
Output: nodes (change with --output=nodes or --output=summary)
Progress: 1 not started
Progress: 1 running
  welcome / web01: connecting
welcome / web01: succeeded
  welcome / web01 / task_1: succeeded, would_change
  welcome / web01 / task_1: would create "/etc/mmx-example"
    absent -> 16 bytes, permissions_mode 0644, uid 0, gid 0
Run fbe759f2: succeeded
1 finished
Saved: /home/you/shop/.mmx/runs/20261115T035229.429039873-fbe759f2091942e3
```

The line `would create "/etc/mmx-example"` is the change that a run would
make. The first `mmx run` then prints one more line before its first
change:

```text
203.0.113.11: setting up MMX's HTTPS service on web01 (mmx-https-api.service, TCP port 7443 on every network address); mmx remove-mmx-from-node web01 --inventory example removes it
```

### Why a dryrun cannot check some work

A dryrun looks at the node as the node is now. The dryrun does not know
what an earlier node task will create when the run makes the earlier
change. On a server without nginx, the directory `/etc/nginx/conf.d` does
not exist yet, because the nginx package creates the directory. So the
dryrun cannot check the site file in `/etc/nginx/conf.d`, and reports the
node task `write_site_file` as `not_checked`, naming `install_nginx`. The
run then installs nginx first, and writes the site file.

A first dryrun on a new server can therefore report work as `not_checked`
that the run then does.
[Preview and run changes](how-to/preview-and-run-changes.md#what-the-results-mean)
lists each case and each result.

## Stopping, failures, and resume

**A failed node task** stops that node's work for the rest of the run.
Other nodes go on, or stop, as the fleet stage's `on_node_failure` setting
says. MMX does not undo a change that a node task already made.

**A stop**, with Ctrl-C or `mmx stop`, starts no new node job. A node that
is working finishes its current node task, or its whole current node task
block, and then stops. So a stop does not fall between writing a
configuration file and reloading its service.

**A controller process that goes away**, such as when your laptop closes,
leaves each started node job running on its node. No new node job starts,
because only a controller process starts node jobs. So a rollout does not
spread to more nodes without you. `mmx status` collects the results later.
If the controller machine and its `.mmx/` directory are lost, the saved
runs are lost with them. Start a new run from a copy of the project:
[Move or share a project](how-to/move-or-share-a-project.md#move-the-project-to-a-new-controller-machine)
says how, and the nodes report `unchanged` for work that is done.

**A lost answer** is checked before anything is sent again. The controller
process records the node job's ID before the controller process sends the
node job. If the connection drops before the node answers, the node job's
outcome is `unknown`. `mmx status` and `mmx resume` then first ask the
node about that ID:

- A node job that still runs is followed to its end.
- A node job that finished keeps its result and does not run again.
- If the node has no record of the ID, `mmx resume` sends a new node job.
  A missing record does not prove that the first node job never ran, so a
  `shell` command that must not run twice needs a guard.
- If the node cannot be reached, the outcome stays `unknown`.

**A node job worker process that dies**, such as when the node loses
power, leaves its node job failed, with the reason `node job interrupted
before recording completion`. `mmx resume` or the next `mmx run` checks
the node again.

**`mmx resume`** repeats the failed and unfinished node jobs of a run,
with the files that the run saved. A repeated node job runs all of the
fleet stage's node tasks on that node from the first one. Work that is
already done reports `unchanged`, because each module checks the node
first. A `shell` node task runs its command again, unless a guard skips
the command.

**A reload that a failure interrupted** still happens on the next run.
Suppose the run writes the new site file, and then fails before nginx
reloads. On the next run, the site file is already correct, so
`write_site_file` reports `unchanged`. But the node recorded which version
of the site file nginx last loaded, so `reload_nginx` still reloads nginx.
The node keeps the record of what each service loaded in
`/run/mmx/services/` until the node reboots. A changed file or checked-out
commit uses the node task block's `refresh` choice, `reload` or `restart`;
a changed package or unit file always restarts. After a reboot, the first
run records what each running service loaded as the starting point, so a
reboot alone causes no extra reload.
[Reload or restart](how-to/change-a-configuration-and-reload-its-service.md#reload-or-restart)
has the table.

**A run from another MMX release** cannot be resumed. `mmx resume` refuses
a run that another release of `mmx` started, before the command contacts
any node. A run that two releases finish could leave a node without its
record of the last successful run.
[Upgrade MMX](how-to/upgrade-mmx.md#finish-a-run-that-an-earlier-release-started)
says how to finish such a run.

[Stop, resume, and read a run](how-to/stop-resume-and-read-a-run.md) has
the commands and the messages.

## The two connections: SSH and HTTPS

The controller machine talks to a node over SSH or over HTTPS, and over
nothing else. Both connections lead to the same code in `mmx-runner`, so a
node job works the same whichever connection carries it.

### First contact: setup over SSH

The first command that connects to a node, even a dryrun, uses OpenSSH:
the same `ssh` you use, with your SSH configuration, keys, and agent. The
node's SSH host key must already be in your `known_hosts` file. MMX never
accepts an unknown host key itself, so OpenSSH's host key check decides
which server MMX trusts.

MMX's first SSH command asks the node what it runs:

- `uname -s` must answer `Linux`, and systemd must run the node.
- The node's **machine ID**, the 32 hexadecimal digits in
  `/etc/machine-id`, identifies the node to MMX.
- The kernel's **boot ID**, a new random ID at each start of the node,
  tells a machine apart from a clone of the same disk image.

A node that is not Linux, has no systemd, or has no valid machine ID
fails at once, with what MMX found and what to do.

Then, over SSH, as root or through `sudo`, MMX installs `mmx-runner` in
`/opt/mmx/bin/` on the node, and checks that `mmx-runner` comes from the
same MMX release as `mmx`. A dryrun stops there: it starts no service and
opens no port, and it does its work over SSH. The first `mmx run`, or
`mmx run-command`, on the node goes on. First it prints one line that
names the service, the port, and `mmx remove-mmx-from-node`, which removes
MMX again. Then it:

1. starts the **node HTTPS API process**, `mmx-runner serve`, as the
   systemd service `mmx-https-api.service`, which listens on TCP port 7443
2. reads the node's HTTPS identity, called its **pin**: the SHA-256 of the
   node's HTTPS public key
3. saves a SHA-256 hash of the machine ID, and the pin, on the controller
   machine, in the project's `.mmx/nodes/`

The pin arrives over SSH, which you already trust. So MMX can later trust
the node's HTTPS connection without a public certificate authority.

**Each project saves its own copy** of a node's identity, in its own
`.mmx/nodes/`. A second project that manages the same node, or a
teammate's copy of the project, sets up MMX on the node again over SSH,
in a dryrun too, because the node already has the service.
That setup finds MMX installed and keeps the node's HTTPS key, so the pin
that the first project saved stays valid.

**A server that takes only a password** gets an SSH key from MMX. MMX
asks for the password, creates its own SSH key in `~/.ssh/mmx/` on the
controller machine, and adds the key to the account's `authorized_keys`.
Later connections need no password. When you change `ssh_port` on such a
node, MMX copies MMX's own key for the new port only after a check.
Verified SSH at the new port must reach the same machine ID as before:
[Connect to your servers](how-to/connect-to-your-servers.md#other-ssh-settings)
shows the message.

### Normal work: HTTPS on port 7443

After setup, the controller process connects to the node over HTTPS on
port 7443. The controller process and the node check each other:

- **The controller process checks the node:** the node must present the
  HTTPS key that matches the saved pin.
- **The node checks you:** the controller process signs a proof with your
  SSH key. The proof names the pin, the login account, and a value unique
  to the one HTTPS connection, so nobody can reuse the proof on another
  connection. Your SSH private key stays on the controller machine.
- **The node checks your access** against the login account's
  `authorized_keys` files and sshd's allow and deny lists. The node does
  not run PAM, and refuses login rules that the node cannot check, such as
  `ForceCommand`: [Login rules that only OpenSSH can check](how-to/connect-to-your-servers.md#login-rules-that-only-openssh-can-check)
  lists them. The account must be root, or able to run any command with
  `sudo`. Over HTTPS the node asks for no `sudo` password, so the SSH key
  alone gives root.

The node checks your access again for each new connection. Remove your key
from the node, and your next connection is refused. A connection that is
already open keeps its access until the connection closes, and node jobs
that already started go on.

### When HTTPS is not available

If the controller process cannot reach port 7443, the controller process
does the same work over SSH. MMX then prints a notice that says what MMX
checked on the node. MMX never opens a firewall port itself. If the node
refuses your key over HTTPS, MMX does not try SSH, and reports the error.

Set `connection: ssh_only` on a node in the inventory to use SSH for all
work. You need `ssh_only` when only OpenSSH can check a login rule, such
as SSH certificates or a second login factor.
[Connect to your servers](how-to/connect-to-your-servers.md#a-server-whose-port-7443-is-closed)
covers the notice and the choices.

### One server, one inventory name

Two inventory names with different addresses must reach two machines. MMX
compares the machine ID and the boot ID that each name's connection
reports. When two names in one run reach the same machine, the second
name's node job fails before anything is sent. The usual cause is a typo
in an address. Without the check, both inventory names would run on one
server, and the server that the second name means would never be
touched.

Two names with the same address and port, such as a load balancer on an
application server, are a choice that the inventory shows, and both run.
Virtual machines cloned from one image may share a machine ID, but each
clone has its own boot ID, so the clones run as separate nodes.

### A replaced server

A rebuilt server has a new SSH host key. After you trust the new host key
in `known_hosts`, the next `mmx run` finds that the saved identity no
longer matches. The run sets up MMX on the new machine over SSH, saves the
new identity, and says so. `mmx resume` and `mmx status` refuse to apply
an older run's work to the new machine, because the older run's saved
progress describes the old machine.
[A server that was replaced](how-to/connect-to-your-servers.md#a-server-that-was-replaced)
shows the steps.

### Both programs come from one release

`mmx` and `mmx-runner` must come from one MMX release. After MMX installs
`mmx-runner` and before the node runs any node task, MMX compares the
release that `mmx-runner` reports with the release of `mmx`, and fails the
node when the two differ. After you
upgrade `mmx`, the next command installs the new `mmx-runner` on each
node that the command connects to. [Upgrade MMX](how-to/upgrade-mmx.md)
covers the upgrade.

## Modules

**A module is trusted code.** A module runs as root on your nodes, so use
only modules that you would trust with root access to your servers.

Every node task names a module. A module has one of three kinds, and a
node task names each kind the same way:

- **Core modules** are built into `mmx-runner`. Core modules cover
  everyday work, such as packages, files, accounts, services, and
  firewalls.
- **Official extra modules** are scripts in
  [Starlark](https://github.com/bazelbuild/starlark), a language that
  reads like Python. The MMX project writes and tests the official extra
  modules for less common tools, such as ZFS. The official extra modules
  ship beside `mmx`, from the same release as `mmx`.
- **Modules of the project's own** live in the project's
  `external_modules/` directory: a Starlark script, or any program for a
  special need.

MMX looks a module name up among the core modules first, then the
official extra modules, then `external_modules/`. A module that is not
core travels to the node with the node job, like any other file.

On the node, the node job worker process runs each module as root:

- A Starlark script reads and changes the node only through MMX's
  **primitives**, functions such as `write_file` that the core modules
  use too. In a dryrun, the primitives change nothing and report what
  they would change.
- A program runs as a child process and gets its inputs as JSON. In a
  dryrun, the program runs in a check mode and must change nothing.
  MMX cannot see what a program does, so the program's author makes sure
  of that.

MMX does the rest for every kind of module: which nodes run the module,
conditions, retries, time limits, stopping, secrets, and saved results.
A command that MMX runs as root gets a small fixed environment, with
`HOME` and `LOGNAME` set for root:
[The environment of a command](reference/shared-module-behavior.md#the-environment-of-a-command)
lists the environment.

[Find the module for a job](how-to/find-the-module-for-a-job.md) and
[Write your own module](how-to/write-your-own-module.md) cover choosing
and writing modules.

## How secrets travel

A secret, such as a database password, comes from the vault
`secrets_vault.age` in the project, or from an environment variable on the
controller machine. Suppose a node task writes a secret into a file:

```yaml
      - id: write_database_settings
        file:
          path: /etc/shop/database.env
          content: "DATABASE_PASSWORD={{ .secrets.database_password }}\n"
          permissions_mode: "0600"
```

The secret `database_password` then takes this path:

- **On the controller machine,** the controller process opens the vault.
  The vault's passphrase never leaves the controller machine.
- **To the nodes,** the controller process sends each node only the
  secrets that the node's node tasks use.
- **On the node,** the secret waits in `/run/mmx/private/`, which is in
  memory. The node deletes the secret's file as the node job worker
  process reads the file. The node job records on the node hold no secret
  value.
- **In output and saved runs,** anything made from the secret shows as
  `[redacted]`. Statuses and exit codes stay visible.
- **In the file you asked for,** `/etc/shop/database.env`, the secret is
  written as plain text, with the permissions you set.

A secret can also start on a node, such as a join token that one node
creates and other nodes need. The controller process passes such a secret
from memory to the nodes that use the secret, and saves the secret
nowhere on the controller machine.

[Keep secrets](how-to/keep-secrets.md) shows how to create the vault and
use a secret. The [secrets reference](reference/secrets.md) lists which
secrets each node receives and where a secret can appear.

## What MMX protects

MMX is a tool for a person or a small team who already have root access to
their servers. MMX trusts the administrators, and the playbooks and
modules that the administrators run, as `ssh` and `sudo` do. MMX protects
five boundaries:

- Only a person whose SSH key the node accepts for an account with full
  `sudo` can send the node work.
- The controller machine talks only to the node whose identity MMX saved
  over trusted SSH.
- Each node receives only the secrets that its node tasks use, and its own
  connection passwords.
- Secrets stay out of output, logs, and saved runs.
- Text from a node, or from a web server that a node task called, cannot
  control your terminal: MMX's text output shows terminal control
  characters, such as the escape character that moves the cursor, erases
  lines, sets the window title, or writes the clipboard, as visible escapes
  like `\x1b`. `--json` carries the exact text.

MMX does not protect a server from the server's own administrators, and
does not make an untrusted playbook or module safe to run. Two limits
follow from how MMX works:

- **Over HTTPS, a key alone gives root.** On a node where MMX is set up,
  a key whose account may use `sudo` gives root over HTTPS without the
  `sudo` password, and no PAM runs. Over plain `ssh`, the same account
  would need the password. Set `connection: ssh_only` on a node that
  needs the extra login rules:
  [Login rules that only OpenSSH can check](how-to/connect-to-your-servers.md#login-rules-that-only-openssh-can-check)
  lists them.
- **A faked result travels.** Someone with root on one node can fake that
  node's results. A value that the node passes to a later fleet stage
  reaches the other nodes that use the value, so those nodes receive the
  faked value too.

### Credentials and where they live

MMX has no accounts of its own. MMX uses your SSH access.

| Credential | Where the credential lives, and what the credential does | How to change or remove the credential |
|---|---|---|
| Your SSH private key or SSH agent | On the controller machine. Logs in over SSH, and signs the HTTPS proof. Never sent to a node. | Remove the public key from the node's `authorized_keys`. The next connection is refused. Open connections and started node jobs go on. |
| MMX's own SSH key | `~/.ssh/mmx/` on the controller machine, made only for a server that takes only a password. The key's public half is in the account's `authorized_keys` on the node. | Remove the public key from the node, as for any key. Deleting only the file on the controller machine does not remove the key's access. |
| SSH password, `sudo` password, key passphrase | Typed at a prompt on the controller machine, or read from a secret. Used for SSH only, never for HTTPS. | Change the passwords and the passphrase the usual way. A new password does not remove a key that the node already accepts. |
| The node's HTTPS key and certificate | `/var/lib/mmx/tls.key` and `tls.crt` on the node, readable only by root. | The certificate lasts 90 days and renews itself with the same key, so the pin stays the same. Only a setup over SSH can make MMX accept a new key. |
| The saved node identity | `.mmx/nodes/` in the project on the controller machine. Says which machine and which HTTPS key to expect. Not a login credential. | A new `mmx run` saves the identity again after verified SSH. |
| The vault passphrase | A prompt, a password file, or an environment variable on the controller machine. Opens `secrets_vault.age`. | `mmx secrets passwd` encrypts the vault with a new passphrase. Old copies of the vault file keep the old passphrase. |

### What a break-in would expose

| If someone takes control of | That person could |
|---|---|
| The controller machine, its SSH keys, or a running `mmx` with an open vault | Use the controller machine's SSH keys with all the access that the keys give, including root over HTTPS on a node whose account may use `sudo`, without the `sudo` password. Change the work sent to nodes, and read every secret that the controller machine can open. The vault's encryption protects a vault file that nobody opened, not values already open in memory. |
| A playbook or a module that you run | Do anything as root on the nodes where the playbook or the module runs. A dryrun, `mmx validate`, and file checksums do not make code trustworthy. |
| Root on one node | Read that node's secrets and HTTPS key, change that node's files, and fake that node's results. A node holds no key that works on other nodes, but values that the node passes to later fleet stages reach the other nodes that use them, so those nodes receive a faked value too. That person learns the node's `sudo` password and any other password sent to the node, and can use a shared password on every node that has the password. |
| A copy of the saved runs or of the vault file | Read server names, paths, and output that is not secret, from the saved runs. Try to guess the vault's passphrase. A new passphrase does not protect a copy that someone already took. |

MMX's security rests on each operating system protecting its accounts,
processes, and files. HTTPS protects the connection between two trusted
machines, but cannot make a machine that someone took over trustworthy.

## A change that could cut a node off

A wrong network configuration file can cut a node off from the controller
machine. MMX's network modules protect such a change:

1. Before the change, the controller process checks that a new SSH or
   HTTPS connection reaches the node. If no new connection reaches the
   node, the node task fails and changes nothing.
2. The node saves the old file and starts a systemd timer that will put
   the old file back. The node's promise to restore is an **owed restore**.
3. The node makes the change. The controller process then opens new
   connections to the node again.
4. If each kind of connection that reached the node before the change,
   SSH or HTTPS, does not reach the node again in time, the node puts the
   old file back by itself, even without the controller machine. An owed
   restore has an ID of 16 lowercase hexadecimal characters, and its
   service starts again after the node restarts. `mmx status` never
   confirms a change; it only reads the outcome and refreshes a restore
   that an administrator started.

[Change SSH or network settings safely](how-to/change-ssh-or-network-settings-safely.md)
says which modules protect a change this way, and which changes, such as
sshd's settings, get no such protection.

## What MMX keeps on a node

MMX keeps its state in ordinary files, with no central database. MMX
writes each node job's request and result to a temporary file first and
then moves the file into place, so a crash does not leave a half-written
request or result. The
`.mmx/` directory holds what MMX keeps on the controller machine:
[The .mmx directory](reference/project-settings.md#the-mmx-directory)
describes the `.mmx/` directory. On each node:

| Path on the node | What the path holds | How long the node keeps it |
|---|---|---|
| `/opt/mmx/bin/<sha256>/mmx-runner` | `mmx-runner`, one directory for each build | Until `mmx remove-mmx-from-node` |
| `/etc/systemd/system/mmx-https-api.service` | The service that runs the node HTTPS API process | Until `mmx remove-mmx-from-node` |
| `/var/lib/mmx/tls.key`, `tls.crt` | The node's HTTPS key and certificate. Only on a node that MMX reaches over HTTPS. | Until `mmx remove-mmx-from-node`. The certificate renews itself. |
| `/var/lib/mmx/jobs/<job-id>/` | Each node job's request, progress, and result | At least 72 hours after the node job finishes, so that a controller process can still collect a result after a lost connection. The node removes the records at its next MMX request after that, and keeps them longer while a restore is owed. |
| `/var/lib/mmx/uploads/<job-id>/` | The files sent for a node job | Until the node job finishes. Files for a node job that never started stay 72 hours. |
| `/var/lib/mmx/kept_files/<sha256>` | Module files, and files that `download` and `unpack_archive` fetched from a web address, so later runs do not send them again. Files from the project, which may hold a secret, are not kept. | 30 days after a run last used the file |
| `/var/lib/mmx/command_records/` | One record for each `shell` node task that runs only when tracked inputs changed: the inputs of the last command run, and whether the command succeeded | Until `mmx remove-mmx-from-node` |
| `/var/lib/mmx/podman_volumes_of_removed_quadlet_files` | A record of the Podman volumes that stay after MMX removed their Quadlet files | Until `mmx remove-mmx-from-node` |
| `/var/lib/mmx/active` and `admission.lock`, `execution.lock`, `owed_restores.lock` in `/var/lib/mmx/` | The ID of the running node job, and lock files that let one node job run at a time | Until `mmx remove-mmx-from-node` |
| `/var/lib/mmx/owed_restores/<id>/` and units named `mmx-owed-restore-<id>` and `mmx-activate-change-<id>` in `/etc/systemd/system/` | One [owed restore](#a-change-that-could-cut-a-node-off): the saved old file and the restore deadline | Until the change is confirmed or the old file is back. A restore that failed stays, so that a person can repair the node. |
| `/var/lib/mmx/last_successful_run/<project_name>/<playbook>` | One JSON line naming the last run of the playbook that succeeded on the node | Until the next such run replaces the line |
| `/run/mmx/services/` | Which version of its configuration each service last loaded | Until the node reboots |
| `/run/mmx/private/` | Secrets handed to a node job, secret values that a node job computed, and the hidden details of failed node tasks, which may quote a secret | Until read, or until the node reboots or the node job's records expire |
| `/run/mmx/private_task_directories/` | One private directory for each running node task that hands files to a tool, such as a login for `git` or `psql` | Until the node task ends. If the node job worker process is killed, until the node's next MMX request while no node job runs. |
| `/etc/mmx/systemd_scheduled_jobs/<name>.env` | The secret values of a scheduled job that uses secrets, readable only by root | Until the scheduled job is removed. `mmx remove-mmx-from-node` keeps the file. |
| `/etc/mmx/do_not_manage` | Created by a person, never by MMX: keeps runs off the node | Until the person removes the file |

The [secrets reference](reference/secrets.md#where-secrets-stay-on-a-node)
gives the owners and permissions of the directories in `/run/mmx/`.

No timer or job of MMX runs on a node between runs. Only
`mmx-https-api.service` stays, and a timer exists only while a restore is
owed. These processes of `mmx-runner` may appear on a node:

| Process | When the process runs |
|---|---|
| Node HTTPS API process, `mmx-runner serve` | On each node without `connection: ssh_only`, as `mmx-https-api.service`. Accepts HTTPS connections on port 7443. |
| Node SSH request process | For one request that comes over SSH. Ends when the request is done. |
| Node job worker process | For one node job. Keeps going if the connection drops. |
| Node change activation process, in `mmx-activate-change-<id>.service` | While a network module puts one changed file into use |
| Node owed restore process, in `mmx-owed-restore-<id>.service` | When an owed restore's timer fires, at boot, after a failed activation, or when an administrator starts it. Puts the old file back. |

## Where to go next

- [Get started](get-started.md): install `mmx` and run a first playbook.
- [Write a playbook](how-to/write-a-playbook.md): the `shop` playbook,
  step by step.
- [Preview and run changes](how-to/preview-and-run-changes.md): what each
  result and exit code means.
- [Connect to your servers](how-to/connect-to-your-servers.md): passwords,
  jump hosts, port 7443, and a replaced server.
- [Commands reference](reference/commands.md): every command and flag.
