# Keep MMX off a server

**Who acts:** you, or your coding agent, in your MMX project on your
controller machine: the computer where you run `mmx`. Some steps run as
`root` on the server itself.

**When to use this page:** MMX must not change a server, for a while or
for good.

| Your situation | Go to |
| --- | --- |
| You work on a server by hand, and no run may change the server until you finish | [Keep runs off for a while](#keep-runs-off-for-a-while) |
| MMX should stop managing a server that keeps running, and MMX's own files should go | [Remove MMX from a server](#remove-mmx-from-a-server) |
| The server is being deleted | Do steps 2 and 4 of [Remove MMX from a server](#remove-mmx-from-a-server), then [delete what MMX saved on the controller machine](#if-the-server-is-already-gone). |

**What this page does:** for a while, you make a server refuse every run.
For good, you take the server out of the project and remove what MMX
installed on the server: the program `mmx-runner`, the program's service,
MMX's directories, and the SSH key that MMX added. On the controller
machine, the same command deletes what MMX saved about the server.

**Terms:** a **node** is a server that MMX manages, listed in an
**inventory** file in `inventories/`. A **playbook** lists the work that
each node does. A **one-time playbook** is a dated playbook in
`one_time_playbooks/` for work that happens once on each node that the
one-time playbook selects. The examples use the servers `web02` and
`web03`.

`mmx remove-mmx-from-node` removes MMX from a server and from the controller
machine. The inventory and the playbooks stay yours to change.

## Keep runs off for a while

When you work on a server by hand, such as to find a kernel problem, a
colleague's run could undo your changes halfway through. A file on the
server makes the server refuse every run until you remove the file.

1. **Create `/etc/mmx/do_not_manage` on the server.** Log in to `web02`
   as `root`, and write one line that says who works on the server, until
   when, and how to reach that person:

   ```sh
   mkdir -p /etc/mmx
   echo "Barbara: debugging the kernel until 2026-11-20, call +49 30 1234" > /etc/mmx/do_not_manage
   ```

   **Check:** a preview from the controller machine,
   `mmx dryrun example`, fails `web02` with the text of the file:

   ```text
   welcome / web02: failed
     a run would be refused: this node refuses management because /etc/mmx/do_not_manage exists; the file says "Barbara: debugging the kernel until 2026-11-20, call +49 30 1234". MMX never creates or removes this file: whoever created it removes it when MMX may manage the node again
   ```

   The preview still checks `web02`, and the node task lines below the
   failure show what a run would change on `web02`.

2. **Do your work on the server.** While the file exists, `web02` refuses
   each node job, which is the work of one fleet stage on one node, from
   `mmx run`, `mmx resume`, and `mmx run-command`. `web02` refuses before
   the first node task, over SSH and over HTTPS.

   **Check:** a run from anyone fails `web02` with the reason:

   ```text
   welcome / web01: succeeded
   welcome / web02: failed
     this node refuses management because /etc/mmx/do_not_manage exists; the file says "Barbara: debugging the kernel until 2026-11-20, call +49 30 1234". MMX never creates or removes this file: whoever created it removes it when MMX may manage the node again
   welcome / web03: succeeded
   ```

   A refused server counts as a failure, not a skip, so the run ends with
   `Run ...: failed` and exit code 1. The fleet stage's `on_node_failure`
   applies as for any failed server, and decides whether the other servers
   go on: [Roll out a change gradually](roll-out-a-change-gradually.md#choose-what-a-failed-server-stops)
   explains the setting.

3. **Remove the file when you finish.** On `web02`, as `root`:

   ```sh
   rm /etc/mmx/do_not_manage
   ```

   MMX never creates or removes the file, so the person who created the
   file removes the file.

   **Check:** the next `mmx run example` says `welcome / web02: succeeded`.
   To repeat only the refused work of the earlier run, use
   `mmx resume RUN_ID` with the run ID that the refused run printed.

While the file exists, MMX still sets up MMX on the server and updates
`mmx-runner`, because setting up and updating change nothing that your
project manages. MMX shows at most the first 1024 bytes of the file, on one line: line
breaks become ` / ` and control characters become spaces. An empty file
shows as `the file is empty`. Only the path matters, so a directory or
another kind of entry at `/etc/mmx/do_not_manage` refuses runs too, and
the message says that MMX does not read it. A symbolic link shows the text
of the file it points to, and still refuses when that file is gone. The
node refuses before its first node task, whether SSH or HTTPS delivered
the job.

A server that refuses management counts as a failure in a run. To run the
other servers without a failure while one server is held, name the other
servers in `--limit`:
[Choose which servers a run touches](choose-which-servers-a-run-touches.md).

## Remove MMX from a server

The example removes MMX from `web03`, which MMX reaches as the account
`deploy`. If MMX reached `web03` with a password, MMX added an SSH key of
its own to `deploy`'s `~/.ssh/authorized_keys`, as
[Connect to your servers](connect-to-your-servers.md#a-server-that-takes-only-a-password)
describes.

1. **Optionally, remove what your playbooks set up on the server.** MMX
   removes only what a playbook tells MMX to remove, so packages, files,
   and services that your playbooks set up stay on the server. A one-time playbook that selects only `web03` can
   remove those packages, files, and services while MMX still manages
   `web03`:

   ```yaml
   # one_time_playbooks/2026-11-15_remove_example_file_from_web03.yml
   fleet_stages:
     - id: remove_example_file_from_web03
       nodes: web03

       node_tasks:
         - id: remove_example_file
           file:
             path: /etc/mmx-example
             state: absent
   ```

   ```sh
   mmx run 2026-11-15_remove_example_file_from_web03
   mmx complete-one-time-playbook 2026-11-15_remove_example_file_from_web03
   ```

   **Check:** the run reports `removed "/etc/mmx-example"`, and the
   completion ends with `Completed: moved ...`.
   [Make a one-time change](make-a-one-time-change.md) explains one-time
   playbooks.

2. **Complete each one-time playbook that selects the server.** A one-time
   playbook that names `web03` cannot load once the inventory has no
   `web03`.

   ```sh
   mmx check-one-time-playbooks
   grep -rn web03 playbooks one_time_playbooks
   ```

   **Check:** `mmx check-one-time-playbooks` lists no `web03` as
   `still needed` or `not known`. Run and complete each one-time playbook
   that still needs `web03`, as
   [Make a one-time change](make-a-one-time-change.md#steps) says. `grep`
   finds `web03` only in `one_time_playbooks/completed/`. Take `web03` out
   of each other file that `grep` lists, except the inventory, which step 4
   changes.

3. **Remove MMX from the server.** On the controller machine, in the
   project directory, while the inventory still lists `web03`:

   ```sh
   mmx remove-mmx-from-node web03
   ```

   The command logs in to `web03` over SSH, as every run does, and prints
   one line for each thing that it removed:

   ```text
   web03: stopped and disabled mmx-https-api.service
   web03: removed /etc/systemd/system/mmx-https-api.service
   web03: removed /opt/mmx
   web03: removed /var/lib/mmx
   web03: removed /run/mmx
   controller machine: removed /home/you/shop/.mmx/nodes/4f8ced544f49ef9fde7f5b898721012f8666a722e1d34a232f5395710b99234f.json
   To stop managing web03 with MMX, also take web03 out of the inventory, its groups, and the playbooks. MMX did not change those files, so until then, a run that selects web03 sets MMX up on it again.
   ```

   When MMX had added a key to `deploy`'s `authorized_keys`, the output
   also has the lines `web03: removed 1 from /home/deploy/.ssh/authorized_keys`
   and `controller machine: removed /home/you/.ssh/mmx/...` for the key's
   three files.

   **Check:** the command exits with `0`. Run the command again: it finds
   nothing, says so, and exits with `0`:

   ```text
   Nothing of MMX is left on web03 or on the controller machine.
   To stop managing web03 with MMX, also take web03 out of the inventory, its groups, and the playbooks. MMX did not change those files, so until then, a run that selects web03 sets MMX up on it again.
   ```

   On `web03`, `ls -d /opt/mmx /var/lib/mmx /run/mmx /etc/mmx` prints
   `No such file or directory` for each path, `pgrep -a mmx-runner` prints
   nothing, and `systemctl status mmx-https-api.service` prints
   `Unit mmx-https-api.service could not be found.`

   The command refuses, and removes nothing, while a node job runs on
   `web03` or an owed restore waits on `web03`. The command then exits with
   `2` and names what to finish first:

   ```text
   mmx: not removing MMX from web03: node job 09d024e54c4141f8c3fd074c6569089a58d3336f10f5be6b540ce9e7463904b5 is running: let the run end, or stop it with mmx stop on the controller machine that started it (mmx status shows the run, mmx resume repeats unfinished work); then run mmx remove-mmx-from-node web03 again
   ```

   The command keeps `/etc/mmx` while `/etc/mmx/systemd_scheduled_jobs/`
   holds the secrets of a scheduled job, and while `/etc/mmx/do_not_manage`
   exists:

   ```text
   web03: kept /etc/mmx: it still holds systemd_scheduled_jobs because MMX removes neither the secrets of a scheduled job nor do_not_manage
   ```

   Run the command again after you removed the scheduled job from the
   server, and the command removes `/etc/mmx`.

4. **Take the server out of the inventory.** Remove `web03` from `nodes`
   and from each group in `inventories/example.yml`:

   ```yaml
   # inventories/example.yml
   nodes:
     web01:
       address: 203.0.113.11
       ssh_user: root
     web02:
       address: 203.0.113.12
       ssh_user: root
     web04:
       address: 203.0.113.14
       ssh_user: root

   groups:
     web_servers:
       nodes: [web01, web02, web04]
   ```

   Do the same in each other inventory that lists the server. Then commit
   the change and share the change with your team. A run from an older
   copy of the project still selects `web03`, and that run sets up MMX on
   `web03` again.

   **Check:** `mmx validate` prints `Valid: mmx_example`, and `mmx nodes`
   no longer lists `web03`. If `mmx validate` names a one-time playbook,
   go back to step 2.

### What the command does

`mmx remove-mmx-from-node web03` does these steps, in this order, and
stops at the first that fails:

1. Refuses while a node job runs on `web03` (a held `worker.lock` in
   `/var/lib/mmx/jobs/`) or an owed restore waits on `web03` (a unit named
   `mmx-owed-restore-...` in `/etc/systemd/system/`). The unit would put
   back the old network configuration, so leave MMX on the server until the
   unit is gone:
   [`systemd_networkd_file`](../reference/modules/systemd_networkd_file.md#what-a-restore-puts-back)
   explains the restore units.
2. On `web03`, as `root`: stops `mmx-https-api.service` and keeps it from
   starting at boot, removes the unit file and reloads systemd, and removes
   `/opt/mmx` (`mmx-runner`), `/var/lib/mmx` (MMX's records on the server,
   the server's HTTPS key and certificate, files kept for later runs, the
   marker files of commands that run once, and the record of each
   playbook's last successful run), and `/run/mmx` (files of running node
   jobs).
3. On `web03`, as `root`: removes `/etc/mmx/systemd_scheduled_jobs` and
   `/etc/mmx` only when they are empty. A `systemd_scheduled_job` reads
   its secrets from `/etc/mmx/systemd_scheduled_jobs/`, and `do_not_manage`
   belongs to the person who created the file.
4. On `web03`, as `deploy`: removes from `~/.ssh/authorized_keys` each line
   without options whose key is the managed key on the controller machine.
   That covers the line that MMX adds now, with the comment `mmx managed
   key`, and the line of an earlier MMX without the comment. A line with
   another key stays, also a line with the same comment from another
   controller machine.
5. On the controller machine: deletes MMX's saved identity of `web03` in
   the project's `.mmx/nodes/`, and, if MMX created a key for `web03`, the
   key's three files in `~/.ssh/mmx/`, and the files of every copy of that
   key that MMX made after an `ssh_port` change, with their saved node
   identities. Both names are the SHA-256 of the
   address that MMX connects to (the inventory's `address`, or the
   `HostName` in `~/.ssh/config` when `address` names a `Host` there), a
   NUL byte, the SSH port, a NUL byte, and the account.

The command never edits the inventory or the playbooks.

### If the server is already gone

The command needs a server that answers over SSH. For a server that is
deleted, do steps 2 and 4, then delete what MMX saved on the controller
machine. In the project directory:

```sh
destination=$(printf '%s\0%s\0%s' 203.0.113.13 22 deploy | shasum -a 256 | cut -d' ' -f1)    # on Linux: sha256sum instead of shasum -a 256
rm -f .mmx/nodes/$destination.json ~/.ssh/mmx/$destination ~/.ssh/mmx/$destination.pub ~/.ssh/mmx/$destination.lock
```

Use the address that MMX connected to, and do the same for each other
account that MMX used at this address.

## What stays on the server and on the controller machine

| What | Where | Why it stays |
| --- | --- | --- |
| What your playbooks set up: packages, files, services, accounts | On the server | MMX removes only what a playbook tells MMX to remove. Step 1 removes what you choose. Some modules write files with `mmx` in the name, such as `/etc/sysctl.d/90-mmx-<name>.conf` from `sysctl_setting`; these files belong to your configuration. |
| Your own SSH keys in `authorized_keys` | On the server | MMX added only the key that step 3 removes. |
| A firewall rule for port 7443 | At your hosting provider or on the server | A rule for port 7443 came from you or from one of your playbooks, because [MMX never opens a firewall port itself](connect-to-your-servers.md#a-server-whose-port-7443-is-closed). Close the port if nothing else uses the port. |
| The server's host key in `~/.ssh/known_hosts` | On the controller machine | `ssh` saved the host key when you first logged in. `ssh-keygen -R 203.0.113.13` removes the host key. |
| Saved runs that include the server | The project's `.mmx/runs/` | Saved runs stay until MMX deletes them by the project's `run_history_retention` setting. |
| The saved identity in another person's copy of the project | That person's `.mmx/nodes/` | `.mmx/` is not in Git, so each copy of the project has its own `.mmx/` directory. If the server comes back, the next run replaces the file. |

If you add the server to the inventory again later, the next run sets up
MMX on the server again, as on a new server, whether or not the command
deleted the saved identity. The marker files of commands that run once are gone with
`/var/lib/mmx/`, so a one-time playbook that is not completed can run such
a command again on that server.

## If something is different

| What you see | What it means and what to do |
| --- | --- |
| `mmx: fleet stage ...: one_time_playbooks/...yml:3:12 (fleet_stages[0].nodes): no node or inventory group named "web03"; a one-time playbook that no node needs any more goes to one_time_playbooks/completed/` | A one-time playbook still names the server that you took out of the inventory. Do step 2. |
| `a run would be refused: this node refuses management because /etc/mmx/do_not_manage exists` in `mmx check-one-time-playbooks` | While the file exists, the check reports the server as `not known`: [Make a one-time change](make-a-one-time-change.md#if-something-is-different). |
| `mmx: not removing MMX from web03: node job ... is running` | A node job runs on the server. Wait for the run to end, or stop the run with `mmx stop` on the controller machine that started the run, then run the command again. |
| `mmx: not removing MMX from web03: an owed restore waits as mmx-owed-restore-...service` | A network configuration change by MMX is not settled. Leave MMX on the server until the unit is gone, then run the command again. |
| `web03: kept /etc/mmx: it still holds ...` | A scheduled job on the server reads its secrets from `/etc/mmx/systemd_scheduled_jobs/`, or someone created `/etc/mmx/do_not_manage`. Run the command again after that is gone. |
| `mmx: remove-mmx-from-node takes the name of one node of inventory ...` | The name is not a node of the inventory, or the inventory no longer lists `web03`. Use the name from `mmx nodes`; for a deleted server, see [If the server is already gone](#if-the-server-is-already-gone). |
| `web03` is set up again after you removed MMX | A run from a copy of the project that still lists `web03` reached the server. Make sure each copy has the new inventory, then remove MMX again. |

## Where to go next

- [Make a one-time change](make-a-one-time-change.md): one-time playbooks,
  and how to complete them.
- [Connect to your servers](connect-to-your-servers.md): passwords, the
  key that MMX adds, and a server that was replaced.
- [Preview and run changes](preview-and-run-changes.md): results, exit
  codes, and `mmx resume`.
