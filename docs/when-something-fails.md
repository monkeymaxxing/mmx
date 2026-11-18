# When something fails

**Who acts:** you, or your coding agent, on your controller machine: the
computer where you run `mmx`. Some fixes are commands on a server, as
`root`.

**When to use this page:** `mmx` printed an error, or a run did something
you did not expect, and you want the cause and the fix.

**What this page does:** for each message, the page says what the message
means and what to do, and links the page that explains the topic. Search
this page for a few words of your message, such as `machine ID` or
`uname -s`.

**Terms:**

- A **node** is a Linux server that MMX manages. The **inventory**,
  `inventories/<name>.yml`, lists your nodes and how to reach them.
- A **playbook** lists **fleet stages**. Each fleet stage picks nodes, and
  each of those nodes runs the fleet stage's **node tasks**, in order.
- A **node job** is the work of one fleet stage on one node.
- `mmx-runner` is MMX's program on each node. The first run on a server
  logs in over SSH and installs `mmx-runner`. Later runs connect over HTTPS
  on port 7443, or over SSH when port 7443 is closed.

The examples use the project `shop`, the servers `web01`, `web02`, and so
on at `203.0.113.11`, `203.0.113.12`, and so on, and the playbook
`example` that `mmx init` creates.

Find the part of this page for the moment the message appeared:

| The message appeared | Go to |
| --- | --- |
| Before any server line, often with exit code 4 | [Before MMX connects to a server](#before-mmx-connects-to-a-server) |
| On a server's first line, often after `connecting` or `setting up MMX` | [Connecting to a server and setting it up](#connecting-to-a-server-and-setting-it-up) |
| On a node task's line, during a run | [During a run](#during-a-run) |
| From `mmx dryrun` with exit code 3, or from `mmx status`, `mmx resume`, or `mmx results` | [After a run](#after-a-run) |
| About the `mmx` program itself | [On the controller machine](#on-the-controller-machine) |

## Before MMX connects to a server

`mmx` checks the command, the project files, and the secrets on your
computer before the command contacts a server. A failure here changes
nothing on any server. Most of these failures end with exit code 4, and
`mmx validate` ends with exit code 1.

| What you see | What it means and what to do |
| --- | --- |
| `mmx: no mmx_project_settings.yml found in the current directory or its parents` | The current directory is not inside an MMX project. Change to the project directory, such as `cd ~/shop`. To start a project, see [Get started](get-started.md). |
| `mmx: there is no playbook set_up_web_servers: playbooks/set_up_web_servers.yml does not exist; the project's playbooks are: example` | No playbook file has that name. A playbook's name is its file name in `playbooks/` without `.yml`. Use a name from the list. |
| `mmx: dryrun requires a playbook name; available playbooks: example` | `mmx dryrun` and `mmx run` need a playbook name, such as `mmx dryrun example`. |
| `mmx: unknown command "frobnicate"` | `mmx help` lists the commands. The [commands reference](reference/commands.md) explains each command. |
| `mmx: playbooks/broken.yml: yaml: line 4: did not find expected ',' or ']'` | The file is not valid YAML, such as a list with no closing `]`. Fix the line that the message names, then run `mmx validate`. The [playbook file reference](reference/playbook-file.md) shows the fields. |
| `mmx validate` names a line, a column, and a field, such as `(fleet_stages[0].node_tasks[0].shell.command): unknown field` | A field name is wrong or in the wrong place. See [Write a playbook](how-to/write-a-playbook.md#if-something-is-different). |
| A message that starts with `mmx: --limit`, such as `mmx: --limit: no node or inventory group named "web99"` | MMX refuses a limit before it contacts a server. See [Choose which servers a run touches](how-to/choose-which-servers-a-run-touches.md#if-something-is-different) for each message. |
| `mmx: vault passphrase required; use --vault-password-file, MMX_VAULT_PASSWORD_FILE, or MMX_VAULT_PASSWORD (prompts are disabled)` | The run needs a secret from the vault, and `mmx` may not ask for the passphrase. See [Keep secrets](how-to/keep-secrets.md#if-something-is-different). |
| `secret database_password requires a nonempty environment variable SHOP_DATABASE_PASSWORD` | A secret comes from an environment variable that is not set. Set the variable. See [Keep secrets](how-to/keep-secrets.md#if-something-is-different). |
| `reference .vars.http_port is not declared for node web01: declare it under vars in mmx_project_settings.yml or in inventories/example.yml (the inventory, a group, or the node), in command_line_vars of the playbook, or set it earlier in this fleet stage with set_vars` | A template reads a variable that no file declares for that node. Declare the variable in one of the places that the message lists. See [Use variables and templates](how-to/use-variables-and-templates.md#if-something-is-different). |

## Connecting to a server and setting it up

The first contact with a server goes over SSH. MMX checks the server and
the login account, and then installs `mmx-runner`. A failure here stops
that server's work. The other servers go on, unless the fleet stage's
`on_node_failure` stops them.

### SSH cannot log in

| What you see | What it means and what to do |
| --- | --- |
| `SSH replace-with-your-node: exit status 255: ssh: Could not resolve hostname replace-with-your-node: nodename nor servname provided, or not known` | The inventory still holds the address that `mmx init` wrote. Put your server's address in `inventories/example.yml`. On Linux, the message ends with `Name or service not known`. |
| `ssh: connect to host 203.0.113.11 port 22: Connection timed out` | Your computer cannot reach the server's SSH port. See [Connect to your servers](how-to/connect-to-your-servers.md#if-something-is-different). |
| `Host key verification failed`, or `WARNING: REMOTE HOST IDENTIFICATION HAS CHANGED!` | Your computer does not trust the server's SSH host key, or the key changed. See [Connect to your servers](how-to/connect-to-your-servers.md#if-something-is-different). |
| `Too many authentication failures` | Your SSH agent offered too many other keys first. See [Get started](get-started.md#if-something-is-different). |
| `SSH password for deploy@203.0.113.12:22 is required; ... (prompts are disabled)` | The server needs a password, and `mmx` may not ask. See [Passwords for automation](how-to/connect-to-your-servers.md#passwords-for-automation). |
| `account deploy does not have unrestricted root sudo access` | The login account must be `root`, or able to run any command with `sudo`. See [Connect to your servers](how-to/connect-to-your-servers.md#if-something-is-different). |

### The first check of the server fails

MMX's first command over SSH asks the server for its system, its
architecture, whether systemd runs it, and its systemd machine ID. The
notices that start with `setting up MMX` appear only after this first
check passes, so a server that fails here shows no such notice. The
machine ID is the 32 hexadecimal digits in the server's `/etc/machine-id`.
MMX tells servers apart by the machine ID. The server fails at once when
an answer is wrong, and the message says what MMX found.

**The answer starts with other text.** A login script of the SSH account,
such as `/root/.bashrc`, prints text even when no person is logged in:

```text
welcome / web03: failed
  the answer of this node to the command uname -s started with "Welcome to the shop server", and MMX expects the one word Linux there. If a login script of the SSH account prints this text, make it print nothing when the session has no terminal
```

Make the script print only when a terminal is attached. In
`/root/.bashrc` on the server, change `echo Welcome to the shop server`
to:

```sh
if [ -t 1 ]; then echo Welcome to the shop server; fi
```

Check: `ssh root@203.0.113.13 uname -s` prints only `Linux`. The text
still prints once in the session that runs the fix, so check in a new
session.

**The answer is empty.** The SSH account runs a fixed command instead of
the commands that MMX sends:

```text
welcome / web02: failed
  this node answered nothing to the command uname -s, and MMX expects the one word Linux there. The SSH account must run the commands that MMX sends
```

Look for a `command="..."` option before the key in the account's
`~/.ssh/authorized_keys` on the server, and for `ForceCommand` in
`/etc/ssh/sshd_config`. Remove the fixed command for the key that MMX
uses, or give MMX an account without one. On a server that MMX already set
up, the same key fails over HTTPS with `HTTPS 403: key is not authorized
for unrestricted MMX access as root`.

**The account's login shell refuses commands.**

```text
welcome / web07: failed
  SSH 203.0.113.17: exit status 1: the first command of MMX printed nothing and exited with status 1. The likely cause is that the login shell of the SSH account refuses commands, as /usr/sbin/nologin and /bin/false do. Give the account a shell such as /bin/sh, or use another ssh_user
```

Check the account's shell on the server with `getent passwd deploy`. The
last field must be a shell such as `/bin/sh`. Give the account that shell,
or set another `ssh_user` in the inventory, such as `root` or another
account that can run commands and `sudo`.

**The server does not run Linux.**

```text
this machine does not run Linux: it answered "Darwin" (macOS) to the command uname -s. MMX manages Linux nodes only, and a macOS controller machine cannot be a node. Remove this node from the inventory, or leave it out of the fleet stage with except_nodes
```

`mmx-runner` runs on Linux only. Another system shows its own name in
place of `"Darwin" (macOS)`. Remove the server from the inventory, or
leave it out with `except_nodes`. A Mac in its own inventory is covered in
[Manage the controller machine itself](how-to/manage-the-controller-machine-itself.md#if-something-is-different).

**The server's processor is not supported.** MMX ships `mmx-runner` for
64-bit x86 (`x86_64`) and 64-bit ARM (`aarch64`). A server with another
processor, such as a 32-bit Raspberry Pi, fails with
`unsupported node architecture armv7l`. MMX cannot manage that server.
[Limits on each distribution](reference/limits-on-each-distribution.md)
lists what MMX supports.

**systemd does not run the server.**

```text
welcome / web04: failed
  this node does not run systemd: the directory /run/systemd/system, which exists on every machine that systemd started, is not there. MMX manages only Linux servers that run systemd. Remove this node from the inventory, or leave it out of the fleet stage with except_nodes
```

A server answers this way when another init system runs it, such as
OpenRC on Alpine Linux. MMX installs `mmx-runner` as a systemd service and
cannot manage the server. Remove the server from the inventory, or leave
it out with `except_nodes`.
[Limits on each distribution](reference/limits-on-each-distribution.md)
lists the tested distributions.

**The machine ID is missing or empty.**

```text
welcome / web05: failed
  this node has no systemd machine ID: its file /etc/machine-id is missing or empty. MMX identifies a node by the systemd machine ID. Run systemd-machine-id-setup as root on the node: it writes a new ID into the file. Then run MMX again
```

As `root` on the server:

```sh
systemd-machine-id-setup
```

Check: `cat /etc/machine-id` prints 32 hexadecimal digits. Then run
`mmx` again.

**The machine ID is not valid.**

```text
welcome / web05: failed
  this node's systemd machine ID is not valid: its file /etc/machine-id holds "uninitialized", not 32 hexadecimal digits. MMX identifies a node by the systemd machine ID. As root on the node, remove the file and run systemd-machine-id-setup: it creates the file with a new ID. Then run MMX again
```

`systemd-machine-id-setup` leaves a file that holds a word such as
`uninitialized` as it is, so remove the file first. The command may also
reuse `/run/machine-id`, so on a server cloned from an image, remove both
files. As `root` on the server:

```sh
rm -f /etc/machine-id /run/machine-id
systemd-machine-id-setup
```

Check: `cat /etc/machine-id` prints 32 hexadecimal digits.

[First contact: setup over SSH](how-it-works.md#first-contact-setup-over-ssh)
explains what the first check does and why it comes first.

### Two inventory names reach one server

Two inventory names with different addresses must reach two servers. MMX
compares the machine ID and the boot ID of each server in a run. The boot
ID is a new random ID at each start of a server. When two names reach the
same machine ID and the same boot ID, the second name fails before MMX
sends any work:

```text
welcome / web06: failed
  inventory nodes web01 and web06 reach the same machine: both answered with the same systemd machine ID and the same boot ID. MMX ran nothing for web06 on that machine, because the machine is web01's in this run. Check the addresses of web01 and web06 in the inventory: one of them probably names the wrong server; two names for one server need the same address. When an address is a Host alias in an SSH configuration file (~/.ssh/config, or the node's ssh_config), the wrong value may be that alias's HostName. Some containers share the boot ID of their host, so each container needs its own /etc/machine-id
```

Without this check, both names would run on one server, and the server
that the second name means would never change. Which name fails depends
on which server answers first.

- **An address is wrong:** correct `address` in the inventory. When the
  address is a `Host` name from `~/.ssh/config`, such as `shop-web06`,
  check the `HostName` line of that `Host` entry too.
- **Two names for one server on purpose,** such as a load balancer that
  runs on an application server: give both names the same `address` and
  `ssh_port`. MMX then runs both names on the server.
- **Containers on one server:** some containers share the server's boot
  ID. When such containers come from one image, they can share a machine
  ID too. Give each container its own machine ID, as
  [The first check of the server fails](#the-first-check-of-the-server-fails)
  shows for an invalid machine ID.

Virtual machines cloned from one image may share a machine ID, but each
clone starts with its own boot ID. So MMX runs the clones as separate
servers.

### HTTPS and the setup of `mmx-runner`

| What you see | What it means and what to do |
| --- | --- |
| `MMX could not connect to https://203.0.113.11:7443 (connection timed out).` and a notice that MMX is using SSH | Your computer could not reach port 7443 on the server. The run went on over SSH. The notice says what MMX found on the server, such as whether `mmx-runner` runs and whether the server's firewall has a rule for the port. See [A server whose port 7443 is closed](how-to/connect-to-your-servers.md#a-server-whose-port-7443-is-closed). |
| `HTTPS 403: key is not authorized for unrestricted MMX access as root: its line in authorized_keys has options MMX does not accept (command, from); remove them, or use connection: ssh_only for other SSH authorization mechanisms` | The key's line in the account's `authorized_keys` carries options that MMX cannot check, such as `command` or `from`. Remove the options, or set `connection: ssh_only` on the node. See [Login rules that only OpenSSH can check](how-to/connect-to-your-servers.md#login-rules-that-only-openssh-can-check). |
| `HTTPS 403: key is not authorized for unrestricted MMX access as this account; check current SSH keys or use connection: ssh_only for other SSH authorization mechanisms; journalctl -u mmx-https-api.service on the node gives the reason` | `mmx-runner` did not find your key in the account's `authorized_keys`, or could not read the account or its `authorized_keys`. The server's journal names which. See [Connect to your servers](how-to/connect-to-your-servers.md#if-something-is-different). |
| `automatic setup of the node runner executable from http://...: ... curl: (22) The requested URL returned error: 404` | The server could not download `mmx-runner` from `mmx_runner_download_url`. See [Connect to your servers](how-to/connect-to-your-servers.md#if-something-is-different). |
| `HTTPS 409: node busy with job ...`, or `node busy with job ...` while MMX sets up a server | A node job of another run, maybe from another computer, works on the server. See [Move or share a project](how-to/move-or-share-a-project.md#if-something-is-different). |
| `cannot replace the node runner executable while owed restore unit ... exists; resolve the owed restore first` | The server still owes the restore of a network file. See [Upgrade MMX](how-to/upgrade-mmx.md#if-something-is-different). |

## During a run

When a node task fails on a server, that server runs none of its later
node tasks or fleet stages, except a failed node task block's own
`on_failure` and `cleanup` node tasks. The other servers go on, unless the
fleet stage's `on_node_failure` stops them.
[Preview and run changes](how-to/preview-and-run-changes.md#what-the-results-mean)
explains each result.

| What you see | What it means and what to do |
| --- | --- |
| `this node refuses management because /etc/mmx/do_not_manage exists; the file says "..."`. A dryrun prints `a run would be refused: ...` | Someone keeps MMX off the server on purpose, and the file says why. The node refuses at the start of each node job, before any node task runs. See [Keep MMX off a server](how-to/keep-mmx-off-a-server.md). |
| A node task's line ends with a long reason | The reason comes from the module: what was missing, and what to do. `mmx describe-module NAME` lists the module's errors, such as `mmx describe-module apt_package`. |
| The run fails on one server and succeeds on the others | The other servers are finished. Fix the cause, then run `mmx resume` or `mmx run`: [Resume a run](how-to/stop-resume-and-read-a-run.md#resume-a-run) says which. |
| `stopped` with `not started: on_node_failure stop_run after ... failed on web03`, or `blocked` with `requires same_node fleet stage ...` | A failure on another server stopped this work, as the playbook asks. See [Roll out a change gradually](how-to/roll-out-a-change-gradually.md#if-something-is-different). |
| A node task is `blocked`, and its reason names an earlier node task, a fleet stage output, or the limit | The node task needs a value that no earlier work produced, such as an output of a server that failed or that `--limit` leaves out. See [Use a result from one server on another](how-to/use-a-result-from-one-server-on-another.md#if-something-is-different) and [Choose which servers a run touches](how-to/choose-which-servers-a-run-touches.md#if-something-is-different). |
| `E: dpkg was interrupted, you must manually run 'dpkg --configure -a' to correct the problem.` | The server stopped inside `dpkg` during an earlier package change, such as at a power loss. See [Packages after an interrupted upgrade](#packages-after-an-interrupted-upgrade). |
| `another apt process still holds apt's lock after 15m0s: Could not get lock /var/lib/apt/lists/lock. It is held by process 1816 (python3)` | Another program used apt on the server for longer than the node task's `wait_for_apt_lock_at_most`, 15 minutes unless the task sets it. MMX does not try the node task again after that wait. Such a program can be the automatic security updates, which run for about ten minutes after a new server first starts; a program that holds the lock for longer may be stuck. `ps -p 1816` on the server shows the program that apt names. Wait until that program ends, then run again. See [`apt_package`](reference/modules/apt_package.md). |
| `node job interrupted before recording completion` | The node job worker process on the server stopped before the node job ended, such as at a reboot. See [Change a configuration and reload its service](how-to/change-a-configuration-and-reload-its-service.md#if-something-is-different). |
| `details suppressed for a task using secrets; ...` | MMX hides the output of a failed node task that uses secrets. The message says where `root` can read it on the server. See [Keep secrets](how-to/keep-secrets.md#if-something-is-different). |
| After Ctrl-C or `mmx stop`, `mmx status` still says `running` for a server | Each server finishes its current node task, or its whole node task block, before it stops. Run `mmx status` again later. See [Stop a run](how-to/stop-resume-and-read-a-run.md#stop-a-run). |

### Packages after an interrupted upgrade

A server that stops inside `dpkg`, such as at a power loss during an
upgrade, refuses each later package change until someone finishes the
work. A run with [`apt_package`](reference/modules/apt_package.md) then
fails at once, with no new tries, because another try changes nothing. The
reason of the node task reads:

```text
dpkg on the node stopped in the middle of a package change. Run dpkg --configure -a on the node first, then run MMX again: exit status 100
```

The line `E: dpkg was interrupted, you must manually run 'dpkg --configure -a'
to correct the problem.` stays in the standard error of the node task.
`mmx resume` works after the fix.

1. **Finish the interrupted work on the server.** As `root` on the
   server:

   ```sh
   dpkg --configure -a
   ```

   **Check:** `apt-get check` exits with code 0 and prints no line that
   starts with `E:`. Before the fix, `apt-get check` prints the same
   `dpkg was interrupted` line.

2. **Run the playbook again** from your computer, such as
   `mmx run upgrade_packages`.

   **Check:** the run ends with `Run ...: succeeded`.

## After a run

| What you see | What it means and what to do |
| --- | --- |
| A dryrun's exit code is 3, and lines say `not_checked` | The dryrun could not check some work, often because an earlier node task would install something first. See [What the results mean](how-to/preview-and-run-changes.md#what-the-results-mean). |
| `mmx status` exits with code 3 and says `unconfirmed; work may still be running` | MMX cannot reach the server to learn how its work ended. Start the server again, or, if the server is gone for good, see [A server that is gone](how-to/stop-resume-and-read-a-run.md#a-server-that-is-gone). |
| `node was replaced since this run (machine identity changed); its recorded work belongs to the previous machine; use mmx run` | You resumed a run from before the server was rebuilt or replaced. Start a new `mmx run`. See [A server that was replaced](how-to/connect-to-your-servers.md#a-server-that-was-replaced). |
| `mmx: this playbook run was started by MMX 0.1.0, but this mmx program is version 0.1.1. ...` after `mmx resume` | You installed another release of MMX after the run started. See [Finish a run that an earlier release started](how-to/upgrade-mmx.md#finish-a-run-that-an-earlier-release-started). |
| `mmx: no matching saved run`, `Nothing to resume.`, `mmx: cannot resume a dryrun; ...`, or `mmx: run ID 2a33c932 is ambiguous; use the full run ID` | The command found no run, or another run than you meant. See [Stop, resume, and read a run](how-to/stop-resume-and-read-a-run.md#if-something-is-different). |
| `mmx: history cleanup: MMX cannot read this saved run, which another version of MMX may have written: ...` | A saved run from another release blocks the removal of old runs. See [Upgrade MMX](how-to/upgrade-mmx.md#if-something-is-different). |

## On the controller machine

| What you see | What it means and what to do |
| --- | --- |
| `mmx: command not found` | The shell does not find `mmx`. Add `~/.local/bin` to your `PATH`, as [Get started](get-started.md) step 1 says. |
| `mmx: the official extra modules in /home/you/.local/lib/mmx/official_extra_modules were shipped with MMX 0.1.0, but the mmx program is version 0.1.1; ...` | Only the `mmx` program was replaced, not the whole directory from the release archive. See [Upgrade MMX](how-to/upgrade-mmx.md#if-something-is-different). |
| `the node runner executable /home/you/.local/lib/mmx/mmx-runner-linux-amd64 reports MMX version 0.1.0, but this mmx program is version 0.1.1. ...` | The `mmx-runner` files beside `mmx` come from another release. MMX checks the versions before a node runs any node task. See [Upgrade MMX](how-to/upgrade-mmx.md#if-something-is-different). |
| A message that names `localhost` or `127.0.0.1`, or `MMX does not reboot the controller machine, ...` | The inventory names your own computer as a node. See [Manage the controller machine itself](how-to/manage-the-controller-machine-itself.md#if-something-is-different). |

## Where to go next

- [Preview and run changes](how-to/preview-and-run-changes.md): each
  result and exit code of a run.
- [Connect to your servers](how-to/connect-to-your-servers.md): host keys,
  passwords, port 7443, and a replaced server.
- [Stop, resume, and read a run](how-to/stop-resume-and-read-a-run.md):
  finish failed work, and a server that is gone.
- [Commands reference](reference/commands.md#exit-codes): every exit code.
