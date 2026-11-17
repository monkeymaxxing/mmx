# Manage the controller machine itself

**Who acts:** you, or your coding agent, on a Linux controller machine: the
machine where you run `mmx`.

**When to use this page:** you run `mmx` on a Linux machine, such as an
administration server or a CI machine, and you want your playbooks to set
up that machine too. Or a run failed with `MMX does not reboot the
controller machine`.

**What this page does:** you add the controller machine to your inventory
as a node at the address `127.0.0.1` and run a playbook on the controller
machine. Then you patch and reboot your servers with a playbook that leaves
the reboot of the controller machine to you.

**Terms:**

- The **controller machine** is the machine where you run `mmx` commands.
  A **controller process** is one running `mmx` command.
- A **node** is a Linux server that MMX manages. The **inventory**, a file
  such as `inventories/example.yml`, lists the nodes and how MMX logs in to
  each node.
- A **playbook** is a file in `playbooks/` that lists **fleet stages**.
  Each fleet stage selects nodes, and each selected node runs the fleet
  stage's **node tasks**, in order.

## Choose your route

| Your situation | Route |
| --- | --- |
| You want your playbooks to set up the Linux machine where you run `mmx` | [Add the controller machine as a node](#add-the-controller-machine-as-a-node) |
| A playbook patches and reboots every node, and the controller machine is one of the nodes | [Patch every node and reboot the controller machine by hand](#patch-every-node-and-reboot-the-controller-machine-by-hand) |
| You run `mmx` on a Mac | A Mac cannot be a node, because `mmx-runner`, MMX's program for the node, is built for Linux only. Manage the Mac with another tool. |
| You run `mmx` inside a container, such as a CI job | [The controller process runs in a container](#the-controller-process-runs-in-a-container) |

## Before you start

- `mmx` is installed on the controller machine, as in
  [Get started](../get-started.md), and you have a project made with
  `mmx init`.
- The controller machine runs Linux with systemd, like any node.
  [Limits on each distribution](../reference/limits-on-each-distribution.md) lists
  the tested distributions.
- Your account on the controller machine is `root`, or an account that can
  run any command with `sudo`, because MMX installs software and changes
  system files.

The examples use the account `barbara` on a Debian 13 controller machine,
and the server `web01` at `203.0.113.11`. Put in your own account and
servers.

## Add the controller machine as a node

MMX manages the controller machine as it manages `web01`. MMX has no
separate local mode. The controller process logs in to `127.0.0.1` over
SSH, installs `mmx-runner` on the controller machine, and sends the work
to `mmx-runner`. So the controller machine needs what every node needs: an
SSH server, a login for your account, and a host key that your account
trusts.

1. **Install an SSH server.** On Debian and Ubuntu:

   ```sh
   sudo apt-get update
   sudo apt-get install -y openssh-server
   ```

   On other distributions, install the distribution's OpenSSH server
   package and start its service.

   **Check:** `ss -ltn 'sport = :22'` prints a line that starts with
   `LISTEN`, so an SSH server waits for logins on port 22. If the command
   prints only its header line, start the SSH server's service.

2. **Let your account log in to itself with an SSH key.** The account that
   runs `mmx` logs in to the controller machine as the account in the
   inventory, here the same account `barbara`:

   ```sh
   ssh-keygen -t ed25519                              # only if ~/.ssh/id_ed25519 does not exist yet
   cat ~/.ssh/id_ed25519.pub >> ~/.ssh/authorized_keys
   chmod 600 ~/.ssh/authorized_keys
   ```

   `ssh-keygen` asks for a file name and a passphrase. A coding agent,
   which cannot answer, runs
   `ssh-keygen -t ed25519 -N "" -f ~/.ssh/id_ed25519` instead, which makes
   a key without a passphrase. A key with a passphrase works too, when the
   key is loaded in your SSH agent.

   Reason for `chmod 600`: MMX refuses an `authorized_keys` file that the
   account's group can write, because another account could then add a
   key. A new file in the home directory is group-writable on some
   systems.

   **Check:** `ls -l ~/.ssh/authorized_keys` starts with `-rw-------`.

3. **Trust the controller machine's own SSH host key.** MMX connects only
   to a machine whose host key your account already trusts, and the
   controller machine is no exception. Log in once and answer `yes`:

   ```sh
   ssh barbara@127.0.0.1 true
   ```

   Before you answer, compare the fingerprint that `ssh` shows with the
   output of `ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub`. That
   command reads the host key file of the machine you are on.

   A coding agent, which cannot answer the question, compares the output
   of these two commands. The two outputs must show the same `SHA256:`
   fingerprint:

   ```sh
   ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub
   ssh-keyscan -t ed25519 127.0.0.1 | ssh-keygen -lf -
   ```

   Then the coding agent logs in with:

   ```sh
   ssh -o StrictHostKeyChecking=accept-new barbara@127.0.0.1 true
   ```

   **Check:** `ssh barbara@127.0.0.1 true` runs again without a question
   and without a password.

4. **Add the controller machine to the inventory.** Write `127.0.0.1` as
   the address, and your account as `ssh_user`:

   ```yaml
   # inventories/example.yml
   nodes:
     controller_machine:          # a name you choose for this machine
       address: 127.0.0.1         # the machine itself
       ssh_user: barbara          # your own account, which can run any command with sudo
     web01:
       address: 203.0.113.11
       ssh_user: root
   ```

   **Check:** `mmx nodes` lists both nodes. The command contacts no
   machine:

   ```text
   Inventory: example (inventories/example.yml, 2 nodes)

   Nodes
     controller_machine   127.0.0.1      barbara
     web01                203.0.113.11   root
   ```

5. **Write a playbook for every node.** This playbook installs
   `unattended-upgrades`, Debian's tool for automatic security updates, on
   every node in the inventory, the controller machine included. Create
   `playbooks/turn_on_security_updates.yml`:

   ```yaml
   # playbooks/turn_on_security_updates.yml
   fleet_stages:
     - id: turn_on_security_updates
       nodes: all                 # every node in the inventory, the controller machine too
       node_tasks:
         - id: install_unattended_upgrades
           apt_package:
             packages:
               - {name: unattended-upgrades, version: newest_when_first_installed}
   ```

   **Check:** `mmx validate turn_on_security_updates` prints
   `Valid: mmx_example`.

6. **Preview the playbook on the controller machine first.**

   ```sh
   mmx dryrun turn_on_security_updates --limit controller_machine
   echo "exit code $?"
   ```

   The first dryrun copies MMX's node program to the controller machine
   and works over SSH. When your account's `sudo` asks for a password, MMX
   asks for the password once for each dryrun:

   ```text
   sudo password for barbara@127.0.0.1:22:
   Progress: 1 running
     turn_on_security_updates / controller_machine: connecting
   ...
   turn_on_security_updates / controller_machine: succeeded
     turn_on_security_updates / controller_machine / install_unattended_upgrades: succeeded, would_change
   ```

   A coding agent cannot type the password. The dryrun then fails with
   `sudo password for barbara@127.0.0.1:22 is required`. Either you run
   this first dryrun once in a terminal, or the coding agent follows
   [Passwords for automation](connect-to-your-servers.md#passwords-for-automation).
   `root`, or an account whose `sudo` needs no password, gets no question.

   **Check:** `install_unattended_upgrades` says `would_change`, and the
   exit code is 2, which for a dryrun means "something would change"
   ([exit codes](preview-and-run-changes.md#exit-codes)).

7. **Run the playbook on every node.**

   ```sh
   mmx run turn_on_security_updates
   ```

   **Check:** the run ends with `Run ...: succeeded` and exit code 0, and
   both nodes say `succeeded`:

   ```text
   turn_on_security_updates / controller_machine: succeeded
   turn_on_security_updates / web01: succeeded
   ```

   This first run sets up MMX's HTTPS service on the controller machine
   over SSH, so MMX asks for the `sudo` password once more and prints a
   line such as
   `127.0.0.1: setting up MMX's HTTPS service on controller_machine (mmx-https-api.service, TCP port 7443 on every network address); ...`.
   Later runs and dryruns print no `setting up` line for `127.0.0.1` and
   ask for no password, because they reach the controller machine over
   HTTPS on port 7443. `mmx results --json` shows `"connection_used": "https"` for
   `controller_machine`.

### What MMX keeps on the controller machine as a node

The controller machine now has two roles, and each role keeps its own
files:

| What | Where | Owner |
| --- | --- | --- |
| The controller process's saved runs and locks | `.mmx/` in the project | The account that runs `mmx` |
| The node's node jobs, results, and locks | `/var/lib/mmx/` | `root` |
| `mmx-runner`, MMX's program for the node | `/opt/mmx/bin/` | `root` |

`mmx` itself runs as your account and needs no root privileges. Only
`mmx-runner` runs as `root`. The systemd service `mmx-https-api.service`
runs `mmx-runner` and listens on TCP port 7443 on all of the controller
machine's addresses, as on every node. The service starts again after a
reboot. [MMX never opens a firewall port itself](connect-to-your-servers.md#a-server-whose-port-7443-is-closed).

### If the `mmx` command stops during a run

The work on the controller machine runs in an `mmx-runner` process of its
own, separate from the `mmx` command. So when the `mmx` command ends in the
middle of a run, such as when you close the terminal, the work on the
controller machine goes on. Run `mmx status` later to collect the result,
or `mmx resume` to collect it and finish the rest of the run; [Stop,
resume, and read a run](stop-resume-and-read-a-run.md) explains both.

## Patch every node and reboot the controller machine by hand

After a `reboot` node task, the controller process waits for the node to
come back, and then the run goes on. A reboot of the controller machine
would end the controller process, and nothing would continue the run. So
MMX does not reboot the controller machine. A fleet stage that ends with a
`reboot` node task fails on the controller machine, before any node task
of that fleet stage runs on the controller machine.

For example, this playbook upgrades the packages on every node and then
reboots each node that an update asks to reboot:

```yaml
# playbooks/patch.yml
fleet_stages:
  - id: patch_and_reboot
    nodes: all
    node_tasks:
      - id: upgrade_packages
        apt_package:
          upgrade_all_packages_within_their_version_rules: true
      - id: reboot_when_an_update_asks
        reboot:
          run_only_if_this_path_exists: /var/run/reboot-required   # a package hook creates this file when a reboot is due, such as after a kernel update
```

`mmx run patch` reports:

```text
patch_and_reboot / controller_machine: failed
  MMX does not reboot the controller machine, the machine where this mmx command runs. Run the playbook from another machine, or reboot this machine by hand after MMX finishes.
patch_and_reboot / web01: succeeded
  patch_and_reboot / web01 / upgrade_packages: succeeded, unchanged
  patch_and_reboot / web01 / reboot_when_an_update_asks: succeeded, changed
    {"boot_id_after":"c7105a3d-9205-4248-a51e-168ec4f0eebc","boot_id_before":"1e536279-cd9a-482d-8360-0364ccfa8043"}
Run ed3d6667: failed
```

Read the result this way:

- The controller machine failed, although no update had asked for a
  reboot on the controller machine. On the controller machine, MMX refuses
  a fleet stage that ends with `reboot`, whatever the `reboot` node task's
  conditions say. A playbook that reboots the controller machine only
  sometimes would otherwise work until the day the condition is true.
- The packages of the controller machine were not upgraded either, because
  no node task of the fleet stage ran on the controller machine.
- `web01` went on. The packages of `web01` were already up to date, so
  `upgrade_packages` says `unchanged`. `web01` rebooted, because the file
  `/var/run/reboot-required` existed on `web01`.
- The command exits with code 1, because one node failed. `mmx dryrun
  patch` and `mmx resume` report the same failure. The failure counts as
  any failed node does, so `on_node_failure` applies, as
  [Choose what a failed server stops](roll-out-a-change-gradually.md#choose-what-a-failed-server-stops)
  explains.

Split the work so that MMX patches the controller machine and you reboot
the controller machine:

1. **Leave the controller machine out of the fleet stage that reboots, and
   patch the controller machine in a fleet stage of its own.** Replace
   `playbooks/patch.yml` with:

   ```yaml
   # playbooks/patch.yml
   fleet_stages:
     - id: patch_and_reboot
       nodes: all
       except_nodes: controller_machine     # MMX does not reboot the machine where mmx runs
       node_tasks:
         - id: upgrade_packages
           apt_package:
             upgrade_all_packages_within_their_version_rules: true
         - id: reboot_when_an_update_asks
           reboot:
             run_only_if_this_path_exists: /var/run/reboot-required   # a package hook creates this file when a reboot is due, such as after a kernel update

     - id: patch_controller_machine
       nodes: controller_machine
       node_tasks:
         - id: upgrade_packages
           apt_package:
             upgrade_all_packages_within_their_version_rules: true
   ```

   `except_nodes` removes nodes from what `nodes` selects.
   [Leave some servers out of a fleet stage](choose-which-servers-a-run-touches.md#leave-some-servers-out-of-a-fleet-stage)
   explains `except_nodes`.

   **Check:** `mmx nodes patch` shows the controller machine in the second
   fleet stage only:

   ```text
   Fleet stages
     patch_and_reboot           all except controller_machine   → web01
     patch_controller_machine   controller_machine              → controller_machine
   ```

2. **Run the playbook.**

   ```sh
   mmx run patch
   ```

   **Check:** the run ends with `Run ...: succeeded` and exit code 0:

   ```text
   patch_and_reboot / web01: succeeded
   patch_controller_machine / controller_machine: succeeded
   ```

3. **Reboot the controller machine by hand when an update asks for a
   reboot.** On Debian and Ubuntu:

   ```sh
   test -f /var/run/reboot-required && sudo reboot
   ```

   The command reboots only when the file `/var/run/reboot-required`
   exists.

   **Check:** after the controller machine is back,
   `systemctl is-active mmx-https-api` prints `active`, and `mmx run patch`
   succeeds again.

You can also run the playbook from another machine. That machine's
controller process can reboot the controller machine like any node and
wait for the reboot.

### How MMX recognizes the controller machine

- **MMX recognizes the machine, not the address.** The refusal is the same
  when the inventory reaches the controller machine as `127.0.0.1`, by a
  host name, or by another of its addresses. The controller process sends
  the kernel's boot ID of its machine, from
  `/proc/sys/kernel/random/boot_id`, and a SHA-256 hash of the machine's
  systemd machine ID, from `/etc/machine-id`. The node compares the two
  values with the node's own values. The `reboot` node task compares the
  values once more, just before the reboot.
- **Only the `reboot` node task is guarded.** MMX does not read the
  commands in your `shell` node tasks or scripts. No setting turns the
  guard off: a playbook that must reboot the controller machine runs from
  another machine.

### The controller process runs in a container

- **A Docker container, such as a CI job.** A process in a Docker container
  reads the boot ID of the server that runs the container. So MMX refuses
  to reboot that server, because the reboot would end the container and
  the controller process in the container.
- **An LXC or Incus container.** Such a container has a boot ID of its
  own, new at each start. So MMX does not recognize the server that runs
  the container, and reboots that server when a playbook says so. The
  reboot ends the controller process. Leave that server out of the fleet
  stage that reboots with `except_nodes`.
- **A container that is a node on the controller machine** can reboot. The
  reboot ends only that container, and the controller process goes on.

Module pages that rely on this section: [`reboot`](../reference/modules/reboot.md).

## If something is different

| What you see | What it means and what to do |
| --- | --- |
| `MMX does not reboot the controller machine, the machine where this mmx command runs. Run the playbook from another machine, or reboot this machine by hand after MMX finishes.` | A fleet stage that ends with `reboot` selects the controller machine. See [Patch every node and reboot the controller machine by hand](#patch-every-node-and-reboot-the-controller-machine-by-hand). |
| `no SSH signing key on the controller machine matches the administrator account's SSH access (unsafe SSH authorization path /home/you/.ssh/authorized_keys: group or other accounts can write this file, so sshd and MMX do not trust it; run chmod go-w /home/you/.ssh/authorized_keys); ...` | The message names the file or directory that another account could change and says what to run: `chmod go-w` on that path, or `chmod 600 ~/.ssh/authorized_keys` for the file, as in step 2. When the message says the path belongs to another account, give the path to the administrator account or to `root` with `chown`. Each directory from `/` down to the file must belong to `root` or to the account. |
| `sudo password for barbara@127.0.0.1:22 is required; configure an environment variable for the credential or use an SSH key or ssh-agent (prompts are disabled)` | The first contact needs your `sudo` password, and MMX may not ask. Run the first dryrun and the first run from a terminal, or see [Passwords for automation](connect-to-your-servers.md#passwords-for-automation). |
| `SSH localhost: exit status 255: No ED25519 host key is known for localhost and you have requested strict checking.` | Your account does not trust the controller machine's host key under the address in the inventory. Do step 3 with that address. |
| `SSH localhost: exit status 255: ssh: connect to host localhost port 22: Connection refused` | No SSH server runs on the controller machine. Do step 1. |
| `this machine does not run Linux: it answered "Darwin" (macOS) to the command uname -s. MMX manages Linux nodes only, and a macOS controller machine cannot be a node. ...` | The inventory names your Mac as a node. Remove the Mac from the inventory, or leave the Mac out of the fleet stage with `except_nodes`. |

## Where to go next

- [Connect to your servers](connect-to-your-servers.md): passwords, a jump
  host, and the other ways to reach a node.
- [Choose which servers a run touches](choose-which-servers-a-run-touches.md):
  `--limit`, `except_nodes`, and one inventory for each environment.
- [The `reboot` module](../reference/modules/reboot.md): every input of the
  `reboot` node task.
- [The `.mmx/` directory](../reference/project-settings.md#the-mmx-directory) and
  [what MMX keeps on a node](../how-it-works.md#what-mmx-keeps-on-a-node): what
  MMX keeps in `.mmx/` on the controller machine and in `/var/lib/mmx/` on
  each node.
