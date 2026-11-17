# Get started with MMX

**Who acts:** you, or your coding agent, on your controller machine: the
computer where you run `mmx`, usually your laptop.

**What this page does:** you install `mmx`, create a project for one
server, preview a small change, make it, and see that a second run changes
nothing. It takes about fifteen minutes.

**Terms:** a **node** is a Linux server that MMX manages. A **playbook**
lists the work that each node does. An **inventory** lists your nodes and
how to reach them.

## Before you start

- A computer with macOS or Linux to run `mmx` on. On Windows, use
  [WSL](https://learn.microsoft.com/windows/wsl/), and keep the project in
  WSL's Linux file system, such as your home directory, not on a Windows
  drive such as `/mnt/c`: MMX sets file permissions on what it saves, and a
  Windows drive keeps none.
- A Linux server that runs systemd, as most current distributions do.
  [Limits on each distribution](reference/limits-on-each-distribution.md) lists the
  tested ones.
- You can log in to the server with `ssh`, as `root` or as an account that
  can run any command with `sudo`.

The examples use the server `web01` at the address `203.0.113.11`. Put in
your own server's address and account.

## Steps

1. **Install `mmx`.** Paste the install block from
   [Install](../README.md#install) in the README into a terminal. The block
   picks the release archive for your computer, checks the download
   against the release's checksums, and installs `mmx` in
   `~/.local/lib/mmx`, with a link in `~/.local/bin`.

   **Check:** the block's last line prints `mmx 0.1.0 (protocol 1)`, and in
   a new terminal, `mmx version` prints the same. If the shell says
   `mmx: command not found`, add `~/.local/bin` to your `PATH`, as the
   README's Install section shows.

2. **Trust the server's SSH host key.** Log in once with `ssh`, and answer
   `yes` after you compare the fingerprint with the one your hosting
   provider shows:

   ```sh
   ssh root@203.0.113.11 true
   ```

   Reason: MMX connects only to a server whose host key your computer
   already trusts, and never asks you to accept a new key.

   A coding agent, which cannot answer the question, first prints the
   server's fingerprint with
   `ssh-keyscan -t ed25519 203.0.113.11 | ssh-keygen -lf -`, compares it
   with the hosting provider's, and then logs in with
   `ssh -o StrictHostKeyChecking=accept-new root@203.0.113.11 true`.

   **Check:** the same command runs again without asking about the host
   key. If it asks for a password, that works too: the first dryrun asks
   for the SSH password, and for a sudo password when the account needs
   one. MMX then creates its own SSH key in `~/.ssh/mmx/` on your computer
   and adds it to the server, so later runs ask for no password.
   [Connect to your servers](how-to/connect-to-your-servers.md#passwords-for-automation) covers
   passwords in automation.

3. **Create a project.** In a new, empty directory:

   ```sh
   mkdir shop && cd shop
   mmx init
   ```

   **Check:** the command prints:

   ```text
   Created mmx_project_settings.yml, inventories/example.yml, and playbooks/example.yml. Set the SSH destination in inventories/example.yml, then run mmx dryrun example.
   ```

   A project is a directory of plain files. Keep it in Git, so every change
   to your servers is a commit you can read later. `mmx init` also adds
   `.mmx/`, where MMX saves each run, to `.gitignore`. You can run `mmx`
   from any directory inside the project: it finds the project by
   `mmx_project_settings.yml`.

4. **Put your server in the inventory.** Replace the contents of
   `inventories/example.yml` with your server's name, address, and login
   account:

   ```yaml
   # inventories/example.yml
   nodes:
     web01:                    # a name you choose for the server
       address: 203.0.113.11   # its address, host name, or a Host name from ~/.ssh/config
       ssh_user: root          # or an account that can run any command with sudo
   ```

   **Check:** `mmx nodes` lists the server. It contacts no server.

   ```text
   Inventory: example (inventories/example.yml, 1 node)

   Nodes
     web01   203.0.113.11   root
   ```

5. **Check the project files.**

   ```sh
   mmx validate
   ```

   **Check:** the command prints `Valid: mmx_example` and exits with code 0.
   `mmx_example` is the `project_name` in `mmx_project_settings.yml`, which
   you can change to a name you choose; it need not match the directory.
   Otherwise it names the file, the line, and the field that is wrong; fix
   that and run it again.

6. **Preview the change.** The example playbook `playbooks/example.yml`
   writes one small file, `/etc/mmx-example`, on every node.

   ```sh
   mmx dryrun example
   echo "exit code $?"
   ```

   The first dryrun on a server also sets up MMX there: it logs in over
   SSH, installs MMX's node program, and starts it. Then it shows what a
   run would change and changes nothing else:

   ```text
   Run fbe759f2
   Project: mmx_example   Playbook: example
   Inventory: example (1 node)   Selected: 1 node
   Output: nodes (change with --output=nodes or --output=summary)
   Progress: 1 not started
   203.0.113.11: setting up MMX through verified SSH
   Progress: 1 running
     welcome / web01: connecting
   welcome / web01: succeeded
     welcome / web01 / create_example_file: succeeded, would_change
     welcome / web01 / create_example_file: would create "/etc/mmx-example"
       absent -> 16 bytes, permissions_mode 0644, uid 0, gid 0
   Run fbe759f2: succeeded
   1 finished
   Saved: /home/you/shop/.mmx/runs/20261115T035229.429039873-fbe759f2091942e3
   exit code 2
   ```

   The `Progress:` lines and lines such as `connecting` show work while it
   runs; the lines after them are the results. `Run ...: succeeded` means
   the dryrun itself worked; the exit code says whether something would
   change. If a server fails, the dryrun ends with `Run ...: failed` and
   exit code 1.

   **Check:** the task says `would_change`, and the exit code is 2, which
   for a dryrun means "something would change". If the dryrun fails, see
   [If something is different](#if-something-is-different).

7. **Make the change.**

   ```sh
   mmx run example
   ```

   **Check:** the task says `succeeded, changed` and `created
   "/etc/mmx-example"`, and the run ends with `Run ...: succeeded` and exit
   code 0. On the server, `cat /etc/mmx-example` prints `Managed by MMX.`

8. **Run it again.**

   ```sh
   mmx run example
   ```

   **Check:** the task says `succeeded, unchanged`. MMX compares what the
   playbook describes with the server and changes only what differs, so a
   second run with nothing new changes nothing.

Next, write your own playbook: [Write a playbook](how-to/write-a-playbook.md).

## What the first dryrun did on the server

- It installed `mmx-runner`, MMX's node program, and started it as the
  systemd service `mmx-https-api.service`, which listens on TCP port 7443.
- From then on, `mmx` connects to the server over HTTPS on port 7443 and
  proves who it is with your SSH key. If port 7443 is closed between your
  computer and the server, MMX uses SSH instead and says so in its output.
  MMX never opens a firewall port itself.
- Over HTTPS, your SSH key alone gives root on the server: when the login
  account uses `sudo`, the server does not ask for the `sudo` password.
  [What MMX protects](how-it-works.md#what-mmx-protects) explains the
  limits.
- MMX keeps its records on the server in `/var/lib/mmx/`, and the records
  of each run on your computer in the project's `.mmx/` directory.

## If something is different

| What you see | What it means and what to do |
| --- | --- |
| `ssh: connect to host 203.0.113.11 port 22: Operation timed out` | Your computer can't reach the server over SSH. Check the address, and any firewall at your hosting provider. |
| `Host key verification failed.` | Your computer doesn't trust the server's host key yet. Do step 2. |
| `Too many authentication failures` | Your SSH agent offered too many other keys before the right one. Add `ssh_identity_file` with the key's path to the node in the inventory, or set `IdentityFile` and `IdentitiesOnly yes` for the server in `~/.ssh/config`. |
| `account deploy does not have unrestricted root sudo access` | The login account must be `root`, or an account that can run any command with `sudo`. |
| `MMX could not connect to https://203.0.113.11:7443 (connection timed out).` | Your computer could not reach port 7443 on the server. MMX used SSH instead, and the run went on. The notice says what MMX found on the server, such as whether `mmx-runner` runs and whether the server's firewall has a rule for the port. Nothing to fix unless you want HTTPS. |
| The exit code of a dryrun is 3 | The dryrun could not check some work, often because an earlier task would install something first. The task's line says what. [Preview and run changes](how-to/preview-and-run-changes.md#what-the-results-mean) explains it. |
