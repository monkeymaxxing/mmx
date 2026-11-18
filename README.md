# MMX

**Agent-first server configuration, maxxed for monkey/human understanding.**

MMX sets up and changes your Linux servers. You list what each server
should have, such as packages, configuration files, and running services,
in plain YAML files. `mmx dryrun` shows what would change on each server,
and `mmx run` makes those changes.

> [!NOTE]
> **This is an alpha version.** MMX runs our own development, test, and monitoring servers and is tested on real machines before every release. A command or a setting may still change. When one does, the release notes say what to change on your side. If anything confuses you or breaks, please tell us: [how to report it](CONTRIBUTING.md). Thanks!

Your coding agent, such as [Claude Code](https://claude.com/claude-code) or [Codex](https://openai.com/codex/), can write the YAML files
and run the commands, or you can do both yourself. You don't need to have
used a tool like this before. If you have used
[Ansible](https://www.ansible.com/), much of MMX will be familiar, and
[If you've used Ansible](#if-youve-used-ansible) lists the differences.

You run `mmx` commands on your **controller machine**: usually your laptop,
or a continuous integration (CI) machine. The Linux servers that MMX manages
are called **nodes**.

A node must run [systemd](https://systemd.io/), the service manager of most current Linux
distributions. MMX is tested on Debian, Ubuntu, Rocky Linux, AlmaLinux,
Fedora, Arch Linux, and openSUSE Leap; the
[tested distributions](docs/changing-mmx/design/development.md#tested-distributions)
list has the releases.
[Limits on each distribution](docs/reference/limits-on-each-distribution.md) lists
what is limited on each one, and why.

MMX 0.1.0 is the first release. A patch release, such as 0.1.1, holds
fixes and documentation changes and needs nothing from you. New features
come in a minor release, such as 0.2.0. Before 1.0.0, a minor release may
also rename a field or a command, and its release notes give the edit that
a project needs. [Compatibility between releases](docs/changing-mmx/design/README.md#compatibility-between-releases)
gives the details.

The [examples](examples/) are working projects,
and the
[Monkeymaxxing Stack](https://github.com/monkeymaxxing/monkeymaxxing-stack)
is an example of MMX setting up a whole system.

## The Monkeymaxxing tools

MMX is one of the free, open-source Monkeymaxxing tools for running simple
server systems.

| Tool | What it does |
|---|---|
| **MMX** (this tool) | Sets up your servers, and makes every later change to them |
| [MMWatchdog](https://github.com/monkeymaxxing/mmwatchdog) | Watches your servers, restarts what fails, and alerts you |
| [MMHotline](https://github.com/monkeymaxxing/mmhotline) | Gets each urgent alert to the person on call, and can let your coding agent investigate first |
| [Monkeymaxxing Stack](https://github.com/monkeymaxxing/monkeymaxxing-stack) | Instructions for your coding agent that build and run a whole production system with the three tools above |

You need none of the other tools to use MMX.

MMX's code and releases are on GitHub, at
[github.com/monkeymaxxing/mmx](https://github.com/monkeymaxxing/mmx).
A copy of the code is on GitLab, at
[gitlab.com/monkeymaxxing/mmx](https://gitlab.com/monkeymaxxing/mmx), for
when GitHub is down. Issues and pull requests go to GitHub.

## Why MMX

### Why a tool instead of shell scripts

Many servers are first set up by hand over SSH, or with a shell script. A
coding agent will often write a shell script too, because a script is quick
to write. But a script is a list of commands. Unless you write the checks
yourself, a script does not look at what is already on the server. So it
cannot tell you what it would change, and it does its work again on every
run.

MMX is a configuration-management tool. You describe what each server
should have: its packages, its files and their contents, and its running
services. MMX compares that description with each server and changes only
what differs.

The description works like a test suite for your servers, and MMX can also
fix what the tests find:

- **Set up.** One command gives a new server the same setup as the others.
- **Audit.** `mmx dryrun` checks every server against the description at any
  time, and changes nothing that the description covers. On a new server,
  the first dryrun also sets MMX up, as [First run](#first-run) says. If
  someone changed by hand something that the description covers, the
  dryrun shows the difference.
- **Change.** You change the description, look at the dryrun, and one
  `mmx run` makes the change on every server that the change applies to.
- **Track.** You keep the description as files in a Git repository, which is
  called infrastructure as code. Git shows who changed what and when. Anyone
  on the team, or their agent, can read how the servers are set up without
  logging in to them, as long as every change goes through MMX.

Here is a common job: install the web server [nginx](https://nginx.org/), write the configuration
file for one site, and reload nginx so that it reads the file. As a shell
script:

```sh
#!/bin/sh
set -e
apt-get install -y nginx
cat > /etc/nginx/conf.d/shop.conf <<'END'
server {
  listen 8080;
  root /srv/shop/public;
}
END
systemctl enable --now nginx
systemctl reload nginx
```

The script works the first time. After that, it reloads nginx on every run,
whether the file changed or not. You can't see what the script would change
without running it. To run it on 20 servers you need a loop over SSH, and
then you read 20 servers' output to find the one that failed.

In MMX, you write the same job as a **playbook**: a file that lists the work
for a group of servers.

```yaml
# playbooks/set_up_web_servers.yml
fleet_stages:                       # the steps of this playbook; here, one
  - id: set_up_nginx
    nodes: web_servers              # a group of servers from the inventory file
    node_tasks:                     # each server does these, in order
      - id: install_nginx
        apt_package:
          packages:
            - {name: nginx, version: newest_when_first_installed}

      - task_block:
          id: shop_site
          changes:                  # first, make the change
            - id: write_site_file
              file:
                path: /etc/nginx/conf.d/shop.conf
                permissions_mode: "0644"
                content: |
                  server {
                    listen 8080;
                    root /srv/shop/public;
                  }

          activate:                 # then, put the change into use
            - id: reload_nginx
              systemd_service:
                name: nginx
                start_at_boot: true
                state: running
                refresh: reload     # reload nginx only if the site file changed
```

A **fleet stage** is one step of a playbook. It names a group of nodes, and
lists the work that each of those nodes does. A playbook can have several
fleet stages, which run in order: for example, one fleet stage for the
database servers, then one for the web servers. This playbook has one fleet
stage, `set_up_nginx`.

`nodes:` names a group from your inventory, such as `web_servers` in
[The whole project](#the-whole-project). `nodes: all` selects every node of
the inventory. The inventory that `mmx init` writes has no groups, so there
use `nodes: all`, or add a group.

Each entry in `node_tasks` is a **node task**, one piece of work on a node.
`apt_package`, `file`, and `systemd_service` are **modules**: each module
knows how to do one kind of work. The `task_block` ties the site file to
the reload, so nginx reloads only when the site file changed.

This example is for Debian and Ubuntu, which install packages with apt.
On another distribution you use the module for that distribution's package
manager, such as `dnf_package` for dnf on Fedora.

Now suppose you add a `server_name` line to the site file in the playbook.
Before the change reaches any server, you can see what it would do on one
server:

```sh
mmx dryrun set_up_web_servers --limit web01 --diff
```

`--limit web01` picks the node `web01`, and `--diff` shows the changed lines
of each file. The dryrun changes nothing that the playbook lists; on a new
server it only copies MMX's own program, as [First run](#first-run) says.
Part of its output:

```text
  set_up_nginx / web01 / install_nginx: succeeded, unchanged
  set_up_nginx / web01 / shop_site/changes/write_site_file: would update "/etc/nginx/conf.d/shop.conf"
    --- before
    +++ after
    @@ -1,4 +1,5 @@
     server {
       listen 8080;
    +  server_name shop.example.com;
       root /srv/shop/public;
     }
  set_up_nginx / web01 / shop_site/activate/reload_nginx: succeeded, would_change — tracked input /etc/nginx/conf.d/shop.conf changed after the service last started or reloaded
```

The last line says that MMX would reload nginx, because the site file
changed.

`mmx run set_up_web_servers` then updates the file and reloads nginx on
each node in the group. The nodes work at the same time, and the output
shows each failure in full. Run the playbook again, and every node task
reports `unchanged`:

```text
  set_up_nginx / web01 / install_nginx: succeeded, unchanged
  set_up_nginx / web01 / shop_site/changes/write_site_file: succeeded, unchanged
  set_up_nginx / web01 / shop_site/activate/reload_nginx: succeeded, unchanged
```

[The whole project](#the-whole-project) shows the other two files this
playbook needs.

### If you've used Ansible

MMX keeps what works well in Ansible: an inventory of servers, playbooks
whose steps run in order, reusable roles, and a preview before changes. MMX
differs in these ways:

- **Many times faster.** In our measurements on 100 and on 1,000 servers,
  MMX did the same work 4 to 17 times faster than Ansible tuned for speed.
  The [performance record](docs/reference/performance.md) has the workload, the
  method, and the numbers.
- **An interrupted run doesn't lose a pending reload.** Ansible restarts a
  service through a handler at the end of a play. By default, if the run
  stops after it changes the configuration file but before the handler
  runs, the next run finds the file already correct and doesn't restart the
  service. For the configuration files that MMX manages, MMX records which
  version each service last loaded, so the next run still reloads the
  service.
- **Fewer ways to do the same thing.** Ansible, for example, has both
  `command` and `shell`, and a pipe works only in `shell`. MMX has only
  `shell`. Each extra choice is one more thing an agent can get wrong and a
  reviewer has to check.
- **No Python required.** Ansible needs Python on the controller machine
  and, for most modules, on every server, plus collections whose versions
  you manage. MMX is written in Go: one program on the controller machine,
  and a small program that MMX installs on each server.
- **Failures stand out.** By default, Ansible prints a line for every
  server and every task. When a run covers more than ten servers, MMX
  groups the servers that have the same result, and shows each failure in
  full.

If you know [Salt](https://saltproject.io/), the way MMX works on each
server may be familiar. Like a Salt minion, MMX's program on the server
does the work there, and keeps going if the controller machine disconnects.

## What you get

### Maxxed for monkey/human understanding

Even if your coding agent writes the files, you may still want to read
them, sometimes during an outage at 3 AM. So MMX's settings have names that
say exactly what they do:

- `also_delete_home_directory_and_mail: true`
- `run_only_if_this_command_succeeds: test -d /srv/shop`, and its opposite,
  `skip_if_this_command_succeeds`
- `max_nodes_at_once: 5`

Each module is named for the tool it runs, such as `apt_package` and
`dnf_package`, so the file shows exactly which tool will run. MMX has no
general `package` module that picks a tool for you.

`mmx explain PLAYBOOK` shows the values that each node task will use on each
node, and where each value came from: a line in your files, or a default.

### Your coding agent writes and runs it

A coding agent can write the YAML files and run the commands, and a person
checks the changes. These parts of MMX are made for that:

- `mmx validate` checks the project's files without contacting a node.
- `mmx describe-module NAME` prints a module's whole page, with its inputs,
  examples, and errors, so the agent doesn't have to guess.
- `mmx dryrun` reports what each node task would change, with file
  differences, and changes nothing that the playbook lists. You or your agent can read the report
  before `mmx run`. MMX cannot know what a `shell` task would do, so the
  report shows a `shell` task as skipped, with its change unknown, unless
  the task gives its own `dryrun` command.
- `--json` and `--non-interactive` give the agent structured results and no
  prompts, so the agent doesn't have to parse output written for people.

### Your agent can write a missing module

About 70 **core modules** are built into MMX and cover everyday work:
packages, files, templates, accounts, services and timers, firewalls,
containers, databases, certificates, and network files. Another 22
**official extra modules** ship with MMX for less common tools, such as ZFS,
LVM, RabbitMQ, and pgBackRest. The [module pages](docs/reference/modules/README.md)
list every module of both kinds.

When no module fits, your agent can write one for your project in [Starlark](https://github.com/bazelbuild/starlark),
a language that reads like Python. The module goes in your project's
`external_modules/` directory. It reads and changes a node through the same
functions that the core modules use. Your module gets these with no extra
code:

- In a dryrun, your module only reports what it would change.
- A run reports what your module changed, with file differences.
- `mmx validate` checks each use of your module against the inputs that the
  module declares.

MMX has no shared collection of roles or modules to download. Your
project's modules live in your project's repository.
[Write your own module](docs/how-to/write-your-own-module.md) is the guide your agent follows.

### Ruthlessly simple

MMX is one program on the controller machine, with no service to keep
running there. You install nothing on the nodes by hand: the first
`mmx dryrun` copies MMX's program to each node, and the first `mmx run`
starts MMX's HTTPS service there.

Your project is a directory of plain files, so you can keep every change in
Git. Secrets are kept in one encrypted file, the vault, in the same
directory, or are read from environment variables. You need no separate
encryption tool.

### Repeatable, and safe to stop

When you run a playbook again, MMX changes only what no longer matches the
playbook. The exception is a `shell` task, which runs every time unless you
give it a condition.

If the controller machine disconnects in the middle of a run, each node
finishes its own work. `mmx resume` then runs the work that failed or did
not finish.

### A bad network configuration is reverted

A wrong network configuration file can cut a server off. MMX's five network
modules, for systemd-networkd, Netplan, NetworkManager, ifupdown, and
[WireGuard](https://www.wireguard.com/), start a timer on the node before they change the file. If MMX
can't reach the node again afterwards, the node puts the old file back by
itself, even if the controller machine is no longer connected.

Other changes that can cut a server off, such as firewall rules or sshd
settings, don't get this automatic restore.
[Change SSH or network settings safely](docs/how-to/change-ssh-or-network-settings-safely.md#change-sshds-settings)
shows how to change sshd's settings without losing access.

### Day-to-day operations

The [documentation](docs/README.md) covers each of these:

- Run one command on many nodes with `mmx run-command`.
- Roll a change out to part of the nodes at a time, such as a quarter, and
  stop after a number of failures that you set.
- Work on only some nodes with `--limit`.
- Reboot nodes and wait for them to come back.
- Run a dated one-time playbook for a migration.

## When Ansible or Salt fits better

Choose Ansible or Salt when you need one of these:

- **Windows servers, network devices, or Linux without systemd.** Ansible
  and Salt manage them. MMX manages only Linux servers that run systemd,
  and refuses a server without systemd.
- **Ready-made community roles and collections.** MMX has none, on purpose.
  Your agent writes what your servers need.
- **Servers that react to events on their own.** Salt can start work when
  something happens on a server. MMX works only when you or your agent run
  a command.

## The whole project

A project is a directory of plain files. The playbook from
[Why a tool instead of shell scripts](#why-a-tool-instead-of-shell-scripts)
needs two more files:

```text
shop/
  mmx_project_settings.yml
  inventories/production.yml
  playbooks/set_up_web_servers.yml
```

```yaml
# mmx_project_settings.yml
project_name: shop
default_inventory: production
```

```yaml
# inventories/production.yml
nodes:
  web01:
    address: web01.example.com
    ssh_user: deploy              # root, or an account that can use sudo
  web02:
    address: web02.example.com
    ssh_user: deploy

groups:
  web_servers:
    nodes: [web01, web02]
```

The project settings file names the project and the inventory to use when
you name none. The **inventory** lists the nodes, how to reach each node
over SSH, and the groups of nodes that playbooks use, such as `web_servers`.

Run the commands in the project's directory:

```sh
mmx validate                     # check the files, without contacting a node
mmx dryrun set_up_web_servers    # show what would change; on a new server, first set MMX up there
mmx run set_up_web_servers       # make the changes
mmx run set_up_web_servers       # nothing differs now, so this run changes nothing
```

The first command that connects to a node also copies MMX's own program
to that node, even when the command is a dryrun. The first `mmx run` also
starts MMX's HTTPS service on the node. [First run](#first-run) says what
each one does.

The [how-it-works guide](docs/how-it-works.md) builds a similar example up
step by step, and explains what happens on the controller machine and on
each node.

## Set it up with your coding agent

You can have your coding agent set up MMX. The prompt below tells the agent
to follow this README, check each step before the next, and ask you before
it changes anything on a server.

Paste this into your agent. Put your own servers in place of the two example
servers, and your own login account in place of `root`:

```text
Set up MMX to manage my servers. Follow the README at
https://github.com/monkeymaxxing/mmx, including its "For coding agents"
checklist.

My servers:
- web01 at 203.0.113.11
- web02 at 203.0.113.12

I log in to them as root with my current SSH setup.

Start with one server, and run each step's check before the next step. Show me
the dryrun output and wait for my yes before every mmx run. Keep secrets
out of the YAML files and out of our chat: put them in MMX's vault. Don't
reboot a server unless I say so.
```

The sections below show the steps, if you want to follow along or do them
yourself.

## Install

You can run `mmx` on macOS or Linux. On Windows, run it inside [WSL](https://learn.microsoft.com/windows/wsl/).
Paste this block into a terminal. It picks the release archive for your
machine, checks the download against the release's checksums, and installs
`mmx` in `~/.local/lib/mmx`, with a link in `~/.local/bin`:

```sh
(
set -eu
version=0.2.0
case "$(uname -sm)" in
  "Darwin arm64")  platform=macos-arm64 ;;
  "Darwin x86_64") platform=macos-amd64 ;;
  "Linux x86_64")  platform=linux-amd64 ;;
  "Linux aarch64") platform=linux-arm64 ;;
  *) echo "MMX has no release for $(uname -sm)."; exit 1 ;;
esac
release=https://github.com/monkeymaxxing/mmx/releases/download/v$version
mkdir -p ~/Downloads/mmx-$version && cd ~/Downloads/mmx-$version
curl -fsSLO "$release/mmx-$version-$platform.tar.gz"
curl -fsSLO "$release/SHA256SUMS"
if command -v sha256sum >/dev/null; then sha256sum --check --ignore-missing SHA256SUMS
else shasum -a 256 --check --ignore-missing SHA256SUMS; fi
tar -xzf "mmx-$version-$platform.tar.gz"
mkdir -p ~/.local/lib ~/.local/bin
rm -rf ~/.local/lib/mmx
mv "mmx-$version-$platform" ~/.local/lib/mmx
ln -sf "$HOME/.local/lib/mmx/mmx" ~/.local/bin/mmx
~/.local/bin/mmx version
)
```

The last line prints `mmx 0.2.0 (protocol 1)`.

Then open a new terminal and run `mmx version`.

IF IT SAYS `command not found`, AS ON A NEW MAC, RUN THE LINE FOR YOUR SYSTEM,
THEN OPEN A NEW TERMINAL:

```sh
echo 'export PATH="$HOME/.local/bin:$PATH"' >> ~/.zshrc     # macOS
echo 'export PATH="$HOME/.local/bin:$PATH"' >> ~/.bashrc    # Linux
```

The archive stays in `~/Downloads/mmx-0.2.0`. The checksum finds a damaged
download. If you have the [GitHub CLI](https://cli.github.com/), this
command also proves that MMX's own release workflow built the archive:

```sh
gh attestation verify ~/Downloads/mmx-0.2.0/mmx-0.2.0-*.tar.gz --repo monkeymaxxing/mmx
```

**By hand.** The release has one archive per machine:
`mmx-VERSION-PLATFORM.tar.gz`, with `SHA256SUMS` beside it. `uname -sm`
tells you the platform: `Darwin arm64` is `macos-arm64`, `Darwin x86_64`
is `macos-amd64`, `Linux x86_64` is `linux-amd64`, and `Linux aarch64` is
`linux-arm64`. Unpack the archive, move its folder to `~/.local/lib/mmx`,
and link `~/.local/lib/mmx/mmx` into a folder on your `PATH`, as the block
does.

On a Mac, download with `curl`, as the block does. macOS refuses to start a
program that a web browser downloaded, because the release builds are not
signed with an Apple developer certificate.

The directory `~/.local/lib/mmx` holds three things: `mmx`, the node program
`mmx-runner` for AMD64 and for ARM64 servers, and the official extra
modules. Keep them together, because `mmx` looks for the other two beside
itself.

**Upgrade.** Paste the install block again with the new version number on
its `version=` line. The block replaces the whole directory
`~/.local/lib/mmx`, so that `mmx`, the node programs, and the official
extra modules come from one release. You do nothing on your nodes: the next
command that connects to a node installs the new node program there.
[Compatibility between releases](docs/changing-mmx/design/README.md#compatibility-between-releases)
says what may change from one release to the next.

**Build from source.** To run your own build, you need Git and Go 1.21 or
newer; by default, Go then downloads the exact Go version that MMX needs.

```sh
git clone https://github.com/monkeymaxxing/mmx
cd mmx
sh scripts/build.sh "$HOME/.local/lib/mmx"
```

When GitHub is down, clone the copy on GitLab instead:
`git clone https://gitlab.com/monkeymaxxing/mmx`. The release archives are
only on GitHub.

## First run

In an empty directory:

```sh
mmx init                  # creates mmx_project_settings.yml, inventories/example.yml, playbooks/example.yml
# Put your server's address and SSH account in inventories/example.yml.
mmx validate              # check the project files, without contacting a server
mmx dryrun example        # copy MMX's program to the server, then show what would change
mmx run example           # make the changes
```

The example playbook writes one small file, `/etc/mmx-example`, on the
server.

Before the first dryrun:

- Log in to the server once with `ssh`, so that your machine trusts the
  server's SSH host key.
- Use the account `root`, or an account that can run any command with
  `sudo`.

**What the first dryrun does to the server.** MMX logs in over SSH,
copies `mmx-runner`, its node program, to `/opt/mmx/bin/`, and keeps a
record in `/var/lib/mmx/`. The dryrun starts no service and opens no port;
it works over SSH.

**What the first run adds.** The first `mmx run` starts `mmx-runner` as the
systemd service `mmx-https-api.service`, which listens on TCP port 7443 on
every network address of the server. Before it does, it prints one line
that names the service, the port, and the command that removes MMX again:
`mmx remove-mmx-from-node NODE`. Only your own SSH keys, the keys that can
already log in to the server, can use the port. To have no port at all,
set `connection: ssh_only` on the node in the inventory. After that first
run, `mmx` connects over HTTPS on port 7443 and proves who it is with your
SSH key, for runs and dryruns alike. Over HTTPS, your SSH key alone gives
root on the server: when the login account uses `sudo`, the server does not
ask for the `sudo` password.

Until that first run, each dryrun works over SSH. Over SSH, a dryrun of
many servers is slower than over HTTPS, and a server whose `sudo` asks for
a password asks once for each dryrun.

MMX never opens a firewall port itself. If port 7443 is closed between the
controller machine and the server, MMX uses SSH for that server and says so
in its output. The [connection guide](docs/how-to/connect-to-your-servers.md)
covers passwords, SSH options, and jump hosts.

## For coding agents

When you set up MMX for someone, reach each outcome below and run its check
before moving on. Use one server first, then the rest.

| Step | Outcome | Check |
| --- | --- | --- |
| 1 | `mmx` is built and on the `PATH`. | `mmx version` prints a version. |
| 2 | The project exists, and its inventory lists each server's `address` and `ssh_user`. | `mmx validate` prints `Valid:` and the project name. |
| 3 | `mmx` can log in to each server without a prompt. | `ssh -o BatchMode=yes USER@ADDRESS true` succeeds. |
| 4 | The person approved the changes. | You showed the person the output of `mmx dryrun PLAYBOOK --limit NODE`, and they said yes. |
| 5 | The changes are made. | `mmx run PLAYBOOK --limit NODE` ends with no failed node. |
| 6 | The playbook is repeatable. | Run it again: no node task reports a change, apart from `shell` tasks that run every time. |

- Before the first dryrun on a new server, tell the person in one line that
  it copies MMX's program to the server, then go on.
- A dryrun exits with 2 when something would change and with 3 when some
  work can't be checked yet. Neither is an error, but your screen may show
  the exit code as one. Before the first dryrun, tell the person that, and
  report the dryrun by its summary lines, not its exit code.
- Look up each module with `mmx describe-module NAME` before using it. Use a
  core module when one fits. Use `shell` for a short command.
- When no module fits and a short `shell` command is not enough, write a
  module for the project, following
  [Write your own module](docs/how-to/write-your-own-module.md). Test the module on one
  server: a dryrun, a run, and a second run that changes nothing.
- Never put a secret's value in a YAML file, a command line, or the chat.
  Use `mmx secrets set KEY`, which reads the value from standard input, or
  `mmx secrets add-random KEY`.
- To unlock the vault, ask the person to run
  `mmx secrets write-passphrase-file` once, in their own terminal, not
  through you. With no vault yet, that command creates the vault, so tell
  the person first: "The passphrase is the one key to the encrypted secrets
  file. It is yours alone: make it new, and save it in your password
  manager." The command prints the line to use, such as
  `MMX_VAULT_PASSWORD_FILE=~/.config/mmx/vault-passphrase-shop`. Then run
  `MMX_VAULT_PASSWORD_FILE=~/.config/mmx/vault-passphrase-shop mmx dryrun PLAYBOOK`,
  and `mmx run` the same way, yourself. Never print, read, or copy that
  file, and never ask for the passphrase in the chat. The person still
  approves the dryrun's changes before each run.
- The first `mmx run` on a server starts MMX's HTTPS service,
  `mmx-https-api.service`, on TCP port 7443 on every network address. Tell
  the person before that run. If they want no listener, set
  `connection: ssh_only` on the node in the inventory.
- A first dryrun on a new server can report `not_checked` for a node task
  that depends on an earlier one, such as a file in a directory that a
  package creates. Tell the person so when you show the dryrun, and don't
  add node tasks to silence it. Add one only if the run fails on it.
- Before you change sshd, the firewall, or the network on a server, keep
  another way in, such as the hosting provider's web console, and tell the
  person which one.

## When something fails

The usual failures on a first run:

| Message | What it means |
| --- | --- |
| `SSH 203.0.113.11: exit status 255: ssh: connect to host 203.0.113.11 port 22: Operation timed out` | The controller machine can't reach the server over SSH. Check the address and any firewall in between. |
| `Host key verification failed.` | OpenSSH doesn't trust the server's host key yet. Connect once with `ssh` and check the fingerprint. |
| `account deploy does not have unrestricted root sudo access` | The login account must be `root`, or able to run any command with `sudo`. |
| `MMX could not connect to https://203.0.113.11:7443 (connection timed out).` | The controller machine could not reach port 7443 on the server. MMX used SSH instead, and the run went on. The notice says what MMX found on the server, such as whether `mmx-runner` runs and whether the server's firewall has a rule for the port. |

A run with a failed or unfinished node ends with the commands to use next:

```text
Inspect:                mmx results 96bbfa47
Resume unfinished work: mmx resume 96bbfa47
Start a new run:        mmx run example
```

## How it works

There is no MMX server to keep running. For each run, `mmx` reads the
project's files, picks the nodes, and sends each node its node tasks. On
the node, `mmx-runner` does the work and saves the results there. `mmx`
collects the results, prints them, and saves them in the project's `.mmx/`
directory.

The [how-it-works guide](docs/how-it-works.md) follows a run through every
part.

## Documentation

| Guide | Read it when you want to |
|---|---|
| [How MMX works](docs/how-it-works.md) | Understand the parts of MMX, where each runs, and how a run works |
| [Reference pages](docs/README.md#look-things-up) | Look up a command, setting, or inventory field |
| [Module pages](docs/reference/modules/README.md) | Look up a module: its inputs, examples, errors, and why it works the way it does. `mmx describe-module NAME` prints the same page |
| [Examples](examples/) | Start from a working project, such as [security updates on Debian and Ubuntu](examples/security-updates-on-debian-and-ubuntu/README.md), or see a whole fleet: the [subscription web service](examples/subscription-web-service-fleet/README.md), with load balancers, databases, applications, workers, and cron jobs |
| [Write your own module](docs/how-to/write-your-own-module.md) | Add your own kind of node task |
| [Coming from Ansible or Salt](docs/how-to/move-from-ansible-or-salt.md) | Map what you already know to MMX |
| [Performance record](docs/reference/performance.md) | See the measurements, including the comparison with Ansible |

## Contributing

[CONTRIBUTING.md](CONTRIBUTING.md) says how to report a bug or propose a
change. [AGENTS.md](AGENTS.md) is the starting point for changing MMX itself.
It links the [design plan](docs/changing-mmx/design/README.md), the
[acceptance contracts](docs/changing-mmx/acceptance.md), the
[validation record](docs/changing-mmx/validation.md), the [testing guide](docs/changing-mmx/testing.md),
and the [documentation map](docs/changing-mmx/documentation-map.md).

## License

Apache License 2.0. Included dependencies keep their own licenses.
