# Run a command on servers now

**Who acts:** you, or your coding agent, in your MMX project on your
controller machine: the computer where you run `mmx`.

**When to use this page:** you want to run one shell command on some of
your servers and see each server's answer, such as checking a service,
reading a file, or clearing a cache, without writing a playbook.

**What this page does:** you check that MMX reaches every server, run a
command on a group of servers, run a command as an application account,
and read each server's output and exit code. The last part says when to
write a playbook instead.

**Terms:**

- A **node** is a server that MMX manages, listed in an **inventory** file
  in `inventories/`. A **group** in the inventory names several nodes, such
  as `web_servers`.
- A **playbook** is a file in `playbooks/` that describes work for MMX to
  do on your nodes. `mmx run-command` needs no playbook.
- A **run** is one recorded execution. Each `mmx run-command` is a run with
  its own **run ID**, such as `7fe1181f`, which MMX saves in the project's
  `.mmx/runs/` directory.

## Before you start

- You have an MMX project with an inventory, as in
  [Get started](../get-started.md). Run `mmx` from a directory inside the
  project. `mmx run-command` reads the project settings and the inventory,
  and no playbook, so a playbook that you are still editing doesn't stop
  the command.
- Your computer trusts each server's SSH host key, as step 2 of Get
  started says.
- The examples use the inventory below, with the group `web_servers`
  and a database server `db01`:

  ```yaml
  # inventories/example.yml
  nodes:
    web01: {address: 203.0.113.11, ssh_user: root}
    web02: {address: 203.0.113.12, ssh_user: root}
    web03: {address: 203.0.113.13, ssh_user: root}
    db01: {address: 203.0.113.21, ssh_user: root}

  groups:
    web_servers:
      nodes: [web01, web02, web03]
    db_servers:
      nodes: [db01]
  ```

## Steps

1. **Check that MMX reaches every server.**

   ```sh
   mmx run-command --nodes all -- true
   echo "exit code $?"
   ```

   `--nodes all` selects every node of the inventory. `true` is a shell
   command that does nothing and succeeds, so this run changes nothing on
   the servers except MMX's own setup. On a server where MMX was never
   set up, MMX first sets itself up, as a first `mmx run` does: MMX logs in
   over SSH, installs MMX's node program, and starts MMX's HTTPS service on
   TCP port 7443. A first run shows the setup:

   ```text
   Run 7fe1181f
   Project: mmx_example   Entry point: run-command
   Inventory: example (4 nodes)   Selected: 4 nodes
   Output: nodes (change with --output=nodes or --output=summary)
   Progress: 4 not started
   203.0.113.12: setting up MMX's HTTPS service on web02 (mmx-https-api.service, TCP port 7443 on every network address); mmx remove-mmx-from-node web02 --inventory example removes it
   203.0.113.11: setting up MMX's HTTPS service on web01 (mmx-https-api.service, TCP port 7443 on every network address); mmx remove-mmx-from-node web01 --inventory example removes it
   203.0.113.21: setting up MMX's HTTPS service on db01 (mmx-https-api.service, TCP port 7443 on every network address); mmx remove-mmx-from-node db01 --inventory example removes it
   203.0.113.13: setting up MMX's HTTPS service on web03 (mmx-https-api.service, TCP port 7443 on every network address); mmx remove-mmx-from-node web03 --inventory example removes it
   ...
   run-command / db01: succeeded
   run-command / web01: succeeded
   run-command / web02: succeeded
   run-command / web03: succeeded
     run-command / db01 / command: succeeded, changed
     run-command / web01 / command: succeeded, changed
     run-command / web02 / command: succeeded, changed
     run-command / web03 / command: succeeded, changed
   Run 7fe1181f: succeeded
   4 finished
   Saved: /home/you/shop/.mmx/runs/20261115T052920.479564574-7fe1181f22a1ed4f
   exit code 0
   ```

   Later runs have no `setting up MMX` lines. Each command says `changed`,
   because MMX can't know what a command changed, so MMX counts every
   command that ran as a change.

   **Check:** each server says `succeeded`, and the exit code is 0. If a
   server fails, its line says why, such as a refused SSH login or an
   untrusted host key; [If something is different](#if-something-is-different)
   lists the common messages. If MMX asks for a password, answer: MMX then
   adds its own SSH key to the server, as
   [Get started](../get-started.md) explains.

2. **Run a command on some servers.**

   ```sh
   mmx run-command --nodes web_servers,db01 -- cat /etc/debian_version
   ```

   `--nodes` takes node names and group names, separated by commas, and
   runs the command on every node that the names select. MMX joins the
   words after `--` with spaces into the command. MMX runs the command
   with Bash on each node, as root, from the directory `/`.

   **Check:** each server prints its output below its `command` line:

   ```text
   run-command / db01: succeeded
   run-command / web01: succeeded
   run-command / web02: succeeded
   run-command / web03: succeeded
     run-command / db01 / command: succeeded, changed
       13.7
     run-command / web01 / command: succeeded, changed
       13.7
     run-command / web02 / command: succeeded, changed
       13.7
     run-command / web03 / command: succeeded, changed
       13.7
   Run 623eee31: succeeded
   4 finished
   ```

   For many servers, add `--output=summary`, which puts the servers with
   the same output on one line:

   ```text
   run-command / 4 nodes [db01, web01, web02, web03]: succeeded
     run-command / 4 nodes [db01, web01, web02, web03] / command: succeeded, changed
       13.7
   ```

   Without `--output`, MMX groups servers when there are many:
   [Output for many servers](preview-and-run-changes.md#output-for-many-servers-and-for-a-coding-agent)
   says when.

3. **Quote a command that uses the shell's own characters.** Your local
   shell reads the command line before `mmx` does. Put the command in
   single quotes when the command holds `&&`, `|`, `;`, `>`, `$`, or `*`,
   so the whole command reaches the server:

   ```sh
   mmx run-command --nodes web01 -- 'ls /srv/missing; echo done'
   ```

   Without the quotes, your local shell would run `echo done` on your
   computer, after `mmx` ends. In the same way, `-- echo $HOME` without
   quotes prints your home directory on your computer, `/home/you`,
   because your shell replaced `$HOME` before `mmx` started.

   **Check:** the output shows what the command printed to standard
   output, then what the command printed to standard error:

   ```text
     run-command / web01 / command: succeeded, changed
       done
       ls: cannot access '/srv/missing': No such file or directory
   ```

   The command succeeded, because Bash returns the exit code of the last
   command in the script, here `echo done`.

4. **Run a command as an application account.** Some commands must run as
   the account of the application, such as a cache command in the
   application's directory:

   ```sh
   mmx run-command --nodes web01 --run-as-node-user-account shop -- 'whoami; pwd; echo "$HOME"'
   ```

   `--run-as-node-user-account` names an account on each node, not your
   account on your computer. The account must already exist on each
   selected node; a playbook usually creates the account. The command gets
   the account's user ID, groups, and home directory, without a login
   shell, sudo, or profile files, and starts in `/`. So write
   `cd /srv/shop-app && ...` at the start of a command that needs another
   directory.

   **Check:** the output names the account, the directory `/`, and the
   account's home directory:

   ```text
     run-command / web01 / command: succeeded, changed
       shop
       /
       /srv/shop-app
   ```

   [Running as another account](../reference/modules/shell.md#running-as-another-account)
   on the `shell` page gives the rules for the account and the command's
   environment.

## Exit codes and failures

A command fails on a server when the command's exit code is not 0. The
other servers go on, and MMX shows each server's exit code and output:

```sh
mmx run-command --nodes web01,web02 -- systemctl is-active nginx
```

On servers without nginx:

```text
run-command / web01: failed
  command: exit status 4
run-command / web02: failed
  command: exit status 4
  run-command / web01 / command: failed, changed — exit status 4
    exit code: 4
    inactive
  run-command / web02 / command: failed, changed — exit status 4
    exit code: 4
    inactive
Run c10b47c5: failed
2 failed
Saved: /home/you/shop/.mmx/runs/20261115T052928.486944064-c10b47c5460bfd06
Inspect:                mmx results c10b47c5
Resume unfinished work: mmx resume c10b47c5
```

`mmx run-command` itself exits with one of these codes:

| Code | Meaning |
| --- | --- |
| `0` | The command succeeded on every selected server. |
| `1` | The command failed on a server, or MMX could not reach a server or the server refused the work. |
| `4` | The command could not start, such as a name in `--nodes` that is in no inventory, or a missing command. |
| `130` | You pressed Ctrl-C. |

To read a run again later, use its run ID: `mmx results c10b47c5`. To
repeat the command only on the servers where the command failed or
never ran, use
`mmx resume c10b47c5`. Give the run ID: [Which run a command picks](stop-resume-and-read-a-run.md#which-run-a-command-picks)
says why.
[Stop, resume, and read a run](stop-resume-and-read-a-run.md) explains
both commands, and `mmx stop` with the run ID for a command that runs
too long.

## Other things to know

- **Many servers at once.** MMX starts the command on as many servers at
  once as the project's `max_nodes_at_once` allows. The project's
  `node_task_defaults` apply to the command, such as a time limit, but MMX
  doesn't retry a failed command.
  [Roll out a change gradually](roll-out-a-change-gradually.md) explains
  the setting.
- **Another inventory or SSH only.** `--inventory NAME` uses
  `inventories/NAME.yml`. `--connection ssh_only` reaches every server
  over SSH instead of HTTPS, as
  [Connect to your servers](connect-to-your-servers.md) explains.
  `--limit` and `--var` are refused, because `--nodes` selects the
  servers and the command takes no variables.
- **A server that someone works on by hand.** A server with the file
  `/etc/mmx/do_not_manage` refuses the command, even `true`.
  [Keep MMX off a server](keep-mmx-off-a-server.md#keep-runs-off-for-a-while)
  explains the file.
- **For a coding agent:** `--json` prints the whole run as one JSON object,
  and `--json` and `--non-interactive` never ask a question, as
  [Preview and run changes](preview-and-run-changes.md#output-for-many-servers-and-for-a-coding-agent)
  explains. Each server's result is in `entries`.

## Keep secrets out of the command

MMX saves the command's text in the run's record in `.mmx/runs/` on your
computer, and each node keeps the text in its record of the node job for
at least 72 hours ([Where runs are kept](stop-resume-and-read-a-run.md#where-runs-are-kept-and-for-how-long)). MMX also shows and saves the command's output as the command
printed the output. So don't put a password or a token in the command,
and don't run a command that prints one. `mmx run-command` can't read a
secret from the vault.

For a command that needs a secret, write a playbook with a `shell` node
task. The node task receives the secret through `env`, as
[Keep secrets](keep-secrets.md) shows. `output_is_secret: true` keeps a
command's output out of the display and the records, as
[A command that prints a secret](../reference/modules/shell.md#a-command-that-prints-a-secret)
shows.

## When to write a playbook instead

`mmx run-command` fits a question or a one-off action that you watch,
such as `systemctl status nginx --no-pager`, `df -h /`, or restarting a
service on one server. Write a playbook when:

- **The work should be the same on each server, also on a server that you
  add later.** A playbook keeps the work in your project, and in Git.
- **You want to see what would change before the server changes.** A playbook has
  `mmx dryrun`, and its modules say `unchanged` when the server already
  matches. A command that `mmx run-command` ran counts as `changed`.
- **The work has several steps that depend on each other,** such as
  writing a file, checking the file, and then reloading a service:
  [Change a configuration and reload its service](change-a-configuration-and-reload-its-service.md).
- **The work must happen once on every server, also on servers that are
  down today,** such as removing a program after a migration. 
  `mmx check-one-time-playbooks` finds the servers that still need the work:
  [Make a one-time change](make-a-one-time-change.md).
- **The command needs a secret,** as [Keep secrets out of the command](#keep-secrets-out-of-the-command)
  says.

## If something is different

| What you see | What it means and what to do |
| --- | --- |
| `mmx: --nodes: no node or inventory group named "web99"`, exit code 4 | A name in `--nodes` is in no inventory. `mmx nodes` lists the names. |
| `mmx: usage: mmx run-command --nodes NODE_OR_GROUP[,...] [--run-as-node-user-account NAME] -- COMMAND` | `--nodes` or the command is missing. |
| `mmx: run-command does not accept --limit` | Name the servers in `--nodes` instead. |
| `this node refuses management because /etc/mmx/do_not_manage exists; the file says "..."` | Someone works on the server by hand. The file's text says who, and until when. Ask that person. [Keep MMX off a server](keep-mmx-off-a-server.md#keep-runs-off-for-a-while) explains the file. |
| `SSH 203.0.113.13: exit status 255: ssh: connect to host 203.0.113.13 port 22: No route to host` | MMX could not reach the server. Check that the server runs and that the address in the inventory is right. The other servers still ran the command. |
| `command: exit status 4` and `failed` | The command ran and returned exit code 4. The lines below show what the command printed. |
| The output shows a value from your computer, such as your home directory | Your local shell replaced `$HOME` or `*` before `mmx` started. Put the command in single quotes, as in step 3. |
| `mmx results` without a run ID says `mmx: no matching saved run` | Without a run ID, `mmx results` never picks a `mmx run-command` run: [Which run a command picks](stop-resume-and-read-a-run.md#which-run-a-command-picks). Give the run ID, such as `mmx results c10b47c5`. |

## Where to go next

- [Stop, resume, and read a run](stop-resume-and-read-a-run.md): stop a
  long command, repeat the command on the servers where the command
  failed, and read the run again later.
- [Write a playbook](write-a-playbook.md): turn work that you repeat into
  a playbook.
- [The `shell` page](../reference/modules/shell.md): commands in a playbook, with
  guards, secrets, and `changed_when`.
