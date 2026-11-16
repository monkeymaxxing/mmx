# Make a one-time change

**Who acts:** you, or your coding agent, in your MMX project on your
controller machine: the computer where you run `mmx`.

**When to use this page:** some work must happen once on each server and
never again, such as moving data to a new directory, removing a program the
team no longer uses, or restarting a service once.

| Your situation | Go to |
| --- | --- |
| You start a new piece of one-time work | [Steps](#steps) |
| The run failed on some servers, or a server was down | [When the run fails on some servers](#when-the-run-fails-on-some-servers) |
| A new server joined the inventory after the work started | [A server that joins later](#a-server-that-joins-later) |
| The work is a plain command, a restart, or the removal of a program | [Other kinds of one-time work](#other-kinds-of-one-time-work) |
| The project has an inventory for each environment | [Several inventories](#several-inventories) |

**What this page does:** you write a one-time playbook that moves the
shop's uploaded files from `/srv/shop/uploads` to `/srv/shop/files`. You
run the one-time playbook, find the servers that still need the work, and
mark the one-time playbook as completed when no server needs the work.

**Terms:**

- A **node** is a server that MMX manages, listed in an **inventory** file
  in `inventories/`.
- A **playbook** lists **fleet stages**. Each fleet stage picks nodes, and
  each of those nodes runs the fleet stage's **node tasks**, in order.
- A **one-time playbook** is a playbook in the directory
  `one_time_playbooks/` whose file name starts with a date, such as
  `2026-11-15_move_uploads_to_files_directory.yml`. A one-time playbook
  runs only when a command names it. When no node needs the one-time
  playbook any more, `mmx complete-one-time-playbook` moves its file to
  `one_time_playbooks/completed/`, which keeps the project's record of finished one-time work.

## Before you start

- Your project has an inventory with the servers and a group, such as
  `web_servers` with `web01`, `web02`, and `web03`.
  [Describe your servers](describe-your-servers.md) shows how.
- You know how to write a playbook and run it:
  [Write a playbook](write-a-playbook.md) and
  [Preview and run changes](preview-and-run-changes.md).
- If the project is in Git, keep it there. The last step moves a file, and
  you commit the move so that the rest of the team sees the work as
  completed. `mmx init` does not create a Git repository.

## Steps

1. **Create the one-time playbook.** Give `mmx new-one-time-playbook` what
   the work does, in lowercase words joined by underscores:

   ```sh
   mmx new-one-time-playbook move_uploads_to_files_directory
   ```

   ```text
   Created one_time_playbooks/2026-11-15_move_uploads_to_files_directory.yml. Replace its example node task with the work, then preview it with mmx dryrun 2026-11-15_move_uploads_to_files_directory.
   ```

   MMX puts the controller machine's date before your words. The file name
   without `.yml` is the one-time playbook's name, which each command
   takes. The new file is a valid playbook that changes nothing:

   ```yaml
   # A one-time playbook holds work that must happen once on every node it
   # selects and never again, such as removing a retired program. It runs only
   # when a command names it.
   #
   # Preview what it would change:      mmx dryrun 2026-11-15_move_uploads_to_files_directory
   # Run it:                            mmx run 2026-11-15_move_uploads_to_files_directory
   # Check which nodes still need it:   mmx check-one-time-playbooks
   # Complete it when no node needs it: mmx complete-one-time-playbook 2026-11-15_move_uploads_to_files_directory

   fleet_stages:
     - id: move_uploads_to_files_directory
       nodes: all

       node_tasks:
         # An example node task that changes nothing. Replace it with the work.
         - show: "Replace this example node task with the work of this one-time playbook."
   ```

   **Check:** `mmx validate` prints `Valid: mmx_example`. If
   `mmx new-one-time-playbook` refuses the words, see
   [If something is different](#if-something-is-different).

2. **Write the work.** Replace the whole file with the following YAML:

   ```yaml
   # one_time_playbooks/2026-11-15_move_uploads_to_files_directory.yml
   # The shop now keeps uploaded files in /srv/shop/files instead of /srv/shop/uploads.
   fleet_stages:
     - id: move_uploads_to_files_directory
       nodes: web_servers                 # an inventory group

       node_tasks:
         - id: move_uploads
           shell:
             script: mv /srv/shop/uploads /srv/shop/files
             run_only_if_this_path_exists: /srv/shop/uploads   # nothing to move: the server is done
             dryrun: ls /srv/shop/uploads                      # a read-only command for mmx dryrun
   ```

   Reason: MMX finds out whether a server still needs the work by
   previewing the one-time playbook on the server, as `mmx dryrun` does.
   A `shell` node task's preview can tell only from what you give the node
   task. `run_only_if_this_path_exists` skips the move on a server without
   `/srv/shop/uploads`, so such a server counts as done. `dryrun` names a
   command that changes nothing, and MMX runs the `dryrun` command instead
   of the move during a preview. Without the guard, no server ever counts as
   done. Without the `dryrun` command, a server that still needs the move
   is reported as `not known`.
   [How MMX knows which servers are done](#how-mmx-knows-which-servers-are-done)
   explains the reason further.

   **Check:** `mmx validate` prints `Valid: mmx_example`.

3. **Preview the work.**

   ```sh
   mmx dryrun 2026-11-15_move_uploads_to_files_directory
   echo "exit code $?"
   ```

   ```text
   move_uploads_to_files_directory / web01 / move_uploads: succeeded, would_change
   move_uploads_to_files_directory / web02 / move_uploads: succeeded, would_change
   move_uploads_to_files_directory / web03 / move_uploads: succeeded, would_change
   Run 0e84e15e: succeeded
   3 finished
   Saved: /home/you/shop/.mmx/runs/20261115T053006.610880428-0e84e15e481b05c5
   exit code 2
   ```

   **Check:** each server says `would_change`, and the exit code is 2,
   which for a dryrun means "something would change"
   ([exit codes](preview-and-run-changes.md#exit-codes)). A server that says
   `skipped, unknown — the shell node task has no dryrun command` has no
   `dryrun` input: add the `dryrun` input as in step 2.

4. **Run the work on one server first.**

   ```sh
   mmx run 2026-11-15_move_uploads_to_files_directory --limit web01
   ```

   ```text
   move_uploads_to_files_directory / web01: succeeded
     move_uploads_to_files_directory / web01 / move_uploads: succeeded, changed
   Run fec80ec8: succeeded
   ```

   **Check:** the run ends with `Run ...: succeeded`. On `web01`,
   `ls /srv/shop/files` lists the uploaded files, and `/srv/shop/uploads`
   no longer exists. Check the shop on `web01` before you go on.

5. **See which servers still need the work.**

   ```sh
   mmx check-one-time-playbooks
   echo "exit code $?"
   ```

   ```text
   Inventory: example (inventories/example.yml)

   2026-11-15_move_uploads_to_files_directory
     done on 1 of 3 servers
     still needed on 2 servers: web02, web03
   exit code 2
   ```

   `mmx check-one-time-playbooks` previews each one-time playbook outside
   `completed/` on the servers that the one-time playbook selects. The
   preview changes nothing that the one-time playbook manages. While each
   preview runs, standard error shows progress lines such as
   `Inspecting 2026-11-15_move_uploads_to_files_directory: 3 not started`.

   **Check:** `web01` counts as done. Each server is in one of three
   states:

   | State | Meaning | What to do |
   | --- | --- | --- |
   | `done` | The preview found nothing to change on the server. | Nothing. |
   | `still needed` | A node task would change the server. | Run the one-time playbook. |
   | `not known` | MMX could not find out. The reason follows the server's name. | Fix what the reason names, then check again. |

   The exit code is 0 when every server is done, 2 when a server still
   needs the work, and 3 when the state of a server is not known.
   `--output nodes` also names the servers that are done, and the preview
   run that inspected the servers, such as
   `inspected by dryrun 25e8601c; mmx results 25e8601c --diff shows what a server still needs`.

6. **Run the work on every server.**

   ```sh
   mmx run 2026-11-15_move_uploads_to_files_directory
   ```

   **Check:** the run ends with `Run ...: succeeded`. `web02` and `web03`
   say `succeeded, changed`. `web01` says
   `skipped, unchanged — node task skipped by a path condition for: /srv/shop/uploads`,
   because the move on `web01` is already done. If the run fails on a
   server, go to
   [When the run fails on some servers](#when-the-run-fails-on-some-servers).

7. **Check that no server needs the work.**

   ```sh
   mmx check-one-time-playbooks
   ```

   ```text
   2026-11-15_move_uploads_to_files_directory
     done on all 3 servers
   ```

   **Check:** the exit code is 0.

8. **Complete the one-time playbook.**

   ```sh
   mmx complete-one-time-playbook 2026-11-15_move_uploads_to_files_directory
   ```

   ```text
   Inventory: example (inventories/example.yml)

   2026-11-15_move_uploads_to_files_directory
     done on all 3 servers

   Completed: moved one_time_playbooks/2026-11-15_move_uploads_to_files_directory.yml to one_time_playbooks/completed/2026-11-15_move_uploads_to_files_directory.yml.
   ```

   `mmx complete-one-time-playbook` checks the servers again, as step 7
   did, for this one-time playbook only. When every server is done, MMX
   moves the file to `one_time_playbooks/completed/` and adds a first line
   with the date:

   ```yaml
   # Completed 2026-11-15: done on all 3 servers.
   ```

   MMX refuses to run, preview, or check a one-time playbook in
   `completed/`.

   **Check:** the output ends with `Completed: moved ...`, and the exit code
   is 0. `mmx check-one-time-playbooks` now prints
   `No outstanding one-time playbook: one_time_playbooks/ holds none outside completed/.`
   If MMX refuses, the output says which server still needs the work; see
   [When the run fails on some servers](#when-the-run-fails-on-some-servers).

9. **Commit the move.** MMX changes only the files in your copy of the
   project and commits nothing.

   ```sh
   git add one_time_playbooks
   git commit -m "Complete 2026-11-15_move_uploads_to_files_directory"
   ```

   **Check:** `git status` shows no change in `one_time_playbooks/`.

## How MMX knows which servers are done

The check uses no record of earlier runs. Each check
asks the servers again with a preview, so the answer comes from each
server as the server is now. For example, a server restored from an old
backup has `/srv/shop/uploads` again. While the one-time playbook is not
completed, the next check reports that server as `still needed`.

So each node task must be able to tell, in a preview, whether the server
still needs the work:

- **Many modules tell by themselves.** A `file` task with `state: absent`
  reports `unchanged` when the file is already gone.
- **A `shell` task tells only from its inputs.** Give a `shell` task a
  guard, such as `run_only_if_this_path_exists` or
  `skip_if_this_command_succeeds`, and a `dryrun` command, as in step 2.
- **A command that has no check** gets a marker file:
  `run_only_once_marker_file` names a file on the node that MMX creates
  after the command succeeded. While the marker file exists, the node task
  is done. [A plain command](#a-plain-command) shows a marker file.

Without a guard, a `dryrun` command, or a marker file, the check reports the server as `not known` with
the reason
`the dryrun of fleet stage move_uploads_to_files_directory could not check node task move_uploads`.

Each check is an ordinary dryrun, saved in the project's `.mmx/runs/`
like any other run. Like `mmx dryrun`, the check sets up MMX on a server
that does not have MMX yet.

## When the run fails on some servers

A run of a one-time playbook can fail on one server and succeed on the
others, as any run can. In this example, `web02` has a file in the way of
the move, and `web03` is switched off:

```text
move_uploads_to_files_directory / web01: succeeded
move_uploads_to_files_directory / web02: failed
  move_uploads: exit status 1
move_uploads_to_files_directory / web03: failed
  SSH 203.0.113.13: exit status 255: ssh: connect to host 203.0.113.13 port 22: Connection timed out
  move_uploads_to_files_directory / web01 / move_uploads: skipped, unchanged — node task skipped by a path condition for: /srv/shop/uploads
  move_uploads_to_files_directory / web02 / move_uploads: failed, changed — exit status 1
    exit code: 1
    mv: cannot overwrite non-directory '/srv/shop/files' with directory '/srv/shop/uploads'
Run a861675a: failed
1 finished · 2 failed
Saved: /home/you/shop/.mmx/runs/20261115T053018.266721649-a861675aada5231d
Inspect:                mmx results a861675a
Resume unfinished work: mmx resume a861675a
Start a new run:        mmx run 2026-11-15_move_uploads_to_files_directory
```

1. **Fix the cause on each failed server.** The line under each server
   says what went wrong. Here, `/srv/shop/files` on `web02` is an old file
   that you look at and remove by hand, and `web03` must be switched on.

   **Check:** `mmx check-one-time-playbooks` lists the failed servers as
   `still needed`. A server that MMX cannot reach shows as `not known`
   with the reason:

   ```text
   2026-11-15_move_uploads_to_files_directory
     done on 1 of 3 servers
     still needed on 1 server: web02
     not known on 1 server: web03 (MMX could not connect: SSH 203.0.113.13: exit status 255: ssh: connect to host 203.0.113.13 port 22: No route to host)
   ```

   When `not known` remains after the server is reachable again, the reason
   names what to fix, such as a node task that the preview could not
   check.

2. **Repeat the failed work.**

   ```sh
   mmx resume a861675a
   ```

   `mmx resume` repeats the failed and unfinished work of run
   `a861675a`: [What a resume repeats](stop-resume-and-read-a-run.md#what-a-resume-repeats).
   If you changed the one-time playbook to fix the failure, start a new
   run instead: `mmx run 2026-11-15_move_uploads_to_files_directory`.

   **Check:** the run ends with `Run a861675a: succeeded`, and `web02` and
   `web03` say `succeeded, changed`.

3. **Check, then complete.** Do steps 7 to 9 of [Steps](#steps).

   **Check:** the output ends with `Completed: moved ...`.

Until every server is done, `mmx complete-one-time-playbook` prints the
check, moves nothing, and says why:

```text
mmx: one-time playbook 2026-11-15_move_uploads_to_files_directory is not completed and stays in one_time_playbooks/: 1 node still needs the one-time playbook, and the state of 1 server is not known; run the one-time playbook with mmx run 2026-11-15_move_uploads_to_files_directory; mmx results 00eb1fe1 --diff shows what the check found on each server
```

The exit code is then 3 when the state of a server is not known, and 2
when the only cause is a server that still needs the work.
`mmx results 00eb1fe1 --node web02 --diff` shows what `web02` still needs.

## A server that joins later

A new server that joins the inventory, or joins the group that a one-time
playbook selects, is checked like the others.

- **A one-time playbook that is not completed yet** selects the new
  server. A new server often has nothing to change, such as no
  `/srv/shop/uploads` to move, so the check counts the new server as done.
  A command with only a marker file differs: the new server has no marker
  file, so the check reports the server as `still needed`, and a run would
  run the command on the new server. Here `web04` joined after a version of
  `2026-11-15_compress_old_logs` without the guard ran:

  ```text
  2026-11-15_compress_old_logs
    done on 3 of 4 servers
    still needed on 1 server: web04
  ```

  If the new server needs the work, run the one-time playbook. If the new
  server does not need the work, add a guard that skips the work on the
  new server, such as `run_only_if_this_path_exists`. The check then
  counts the new server as done. [A plain command](#a-plain-command) shows
  the whole file with the guard. A node task with no guard input, such as
  a restart, can leave the new server out with `except_nodes: [web04]` on
  the fleet stage instead. [Several inventories](#several-inventories) has
  the limit of `except_nodes` in a one-time playbook.

- **A completed one-time playbook** never runs on the new server. Set up a
  new server with your ordinary playbooks, which describe the server as the
  server is now. If the new server does need the work, move the file back
  from `one_time_playbooks/completed/` to `one_time_playbooks/`, run the
  one-time playbook, and complete the one-time playbook again. MMX adds a second
  `# Completed` line.

## Other kinds of one-time work

### A program that the team no longer uses

Modules such as `directory` and `file` with `state: absent` report
`unchanged` when the file or directory is already gone, so these modules
need no guard. This
one-time playbook removes rbenv, an old tool that managed Ruby versions:

```yaml
# one_time_playbooks/2026-11-15_remove_old_ruby_manager.yml
fleet_stages:
  - id: remove_old_ruby_manager
    nodes: web_servers

    node_tasks:
      - id: remove_rbenv_directory
        directory:
          path: /opt/rbenv
          state: absent
          also_delete_everything_inside: true

      - id: remove_rbenv_profile_script
        file:
          path: /etc/profile.d/rbenv.sh
          state: absent
```

Work of this kind belongs in a one-time playbook, not in an ordinary
playbook or a role. An ordinary playbook runs the deletion on every run for
as long as the lines stay, and nobody knows when the lines may go. A
one-time playbook runs only when you name it, and
`mmx check-one-time-playbooks` shows when the work is finished.

### A plain command

A command that has no way to check whether it already ran gets a marker
file. This one-time playbook compresses old log files once. `gzip` has no
preview of what it would compress, so a preview cannot tell whether a
server still needs the work:

```yaml
# one_time_playbooks/2026-11-15_compress_old_logs.yml
fleet_stages:
  - id: compress_old_logs
    nodes: web_servers

    node_tasks:
      - id: compress_logs
        shell:
          script: gzip --recursive /srv/shop/old_logs
          run_only_if_this_path_exists: /srv/shop/old_logs       # a new server has no old logs
          run_only_once_marker_file: /var/lib/mmx/commands_run_once/2026-11-15_compress_old_logs
```

MMX creates the marker file on the node after `gzip` succeeded. A
preview first checks the guard: a server without `/srv/shop/old_logs` is
done. Otherwise a missing marker file means `still needed`, and an existing
marker file means `done`. A failed command writes no marker file,
so the next run tries again. Keep marker files in
`/var/lib/mmx/commands_run_once/`, where MMX creates the directory. To run
the command again on purpose, delete the marker file on the node.
[The `shell` module](../reference/modules/shell.md#run-a-command-only-once) has the
details.

### A restart

A service that runs but no longer answers can get one restart. Like a
plain command, the restart needs a marker file, because a restart changes
nothing that a preview can see:

```yaml
# one_time_playbooks/2026-11-15_restart_shop_worker.yml
fleet_stages:
  - id: restart_shop_worker
    nodes: web_servers

    node_tasks:
      - id: restart_worker
        systemd_service:
          name: shop-worker
          state: running
          restart_now: true
          run_only_once_marker_file: /var/lib/mmx/commands_run_once/2026-11-15_restart_shop_worker
```

The run reports
`succeeded, changed — restarted shop-worker.service once and wrote the marker file /var/lib/mmx/commands_run_once/2026-11-15_restart_shop_worker`.
On a server without the service, the check reports `not known` with
`the dryrun of fleet stage restart_shop_worker could not check node task restart_worker`.

### A cleanup of disk space

[`systemd_journal_vacuum`](../reference/modules/systemd_journal_vacuum.md),
[`docker_unused_images`](../reference/modules/docker_unused_images.md),
[`podman_unused_images`](../reference/modules/podman_unused_images.md),
[`apt_downloaded_package_files`](../reference/modules/apt_downloaded_package_files.md),
and [`delete_old_files`](../reference/modules/delete_old_files.md) report `unchanged`
in a preview when nothing is left to delete. So these modules need no
marker file, and the check counts a clean server as done. Logs and images
keep coming, so a server can need the cleanup again later. Complete the
one-time playbook when every server is done.

## Several inventories

`mmx check-one-time-playbooks` and `mmx complete-one-time-playbook` check
the servers of one inventory: the project's `default_inventory`, or the
inventory that `--inventory NAME` names. In a project with an inventory
for each environment, check each inventory before you complete:

```sh
mmx check-one-time-playbooks --inventory staging
mmx check-one-time-playbooks --inventory example
mmx complete-one-time-playbook 2026-11-15_remove_old_ruby_manager --inventory example
```

`mmx complete-one-time-playbook` names the inventories that the command
did not check:

```text
Note: MMX inspected nodes only in inventory example. MMX did not inspect nodes in another inventory, staging. Before completing a one-time playbook, check every inventory with mmx check-one-time-playbooks --inventory NAME.
If a node in another inventory still needs the completed one-time playbook, move the playbook file back to one_time_playbooks/ and run the playbook with the inventory that contains the node.
```

A one-time playbook must load with every inventory, because `mmx validate`
checks each one-time playbook with each inventory. A server name that only
one inventory has, such as `except_nodes: [web04]`, makes the one-time
playbook fail with the other inventories:

```text
2026-11-15_restart_shop_worker
  inspected nothing: MMX cannot load the playbook with inventory staging: fleet stage restart_shop_worker: one_time_playbooks/2026-11-15_restart_shop_worker.yml:4:19 (fleet_stages[0].except_nodes): no node or inventory group named "web04"; a one-time playbook that no node needs any more goes to one_time_playbooks/completed/
```

Select servers by a group that each inventory has, such as `web_servers`,
or use a guard instead of a server's name.

## Rules for one-time playbooks

- **The name.** The file name is `YYYY-MM-DD_what_it_does.yml`: a real
  date, an underscore, and lowercase letters and digits joined by
  underscores. `mmx new-one-time-playbook` also wants the words to start
  with a letter, because the words become the ID of the fleet stage.
  `mmx new-one-time-playbook` refuses a name that already exists, with
  today's date and the same words, in `one_time_playbooks/` or in
  `completed/`.
- **The directory.** `one_time_playbooks/` holds only one-time playbooks,
  `completed/`, and files whose names start with a dot, such as files that
  Git or a file manager leaves. `mmx validate` and the check report any
  other file in `one_time_playbooks/` as an error.
- **The commands that take the name.** `validate`, `explain`, `nodes`,
  `dryrun`, `run`, and `module-usage` take a one-time playbook's name, as
  for any playbook. `mmx validate` without a name checks each one-time
  playbook outside `completed/` with each inventory.
- **Command-line variables need a default.** Each of a one-time
  playbook's `command_line_vars` has a `default`, because the check
  previews the one-time playbook without `--var`.
- **The check has no `--limit`.** `mmx check-one-time-playbooks` checks
  every server that a one-time playbook selects, and takes no `--limit`,
  `--only-fleet-stages`, or `--var`. To look at a few servers, use
  `mmx dryrun NAME --limit web02`.
- **A server that a fleet stage's `when` skips** has nothing to do in that
  fleet stage, and counts as done there.
- **Other flags.** `mmx check-one-time-playbooks` and
  `mmx complete-one-time-playbook` take `--inventory NAME`,
  `--connection ssh_only` for a controller machine that cannot reach the
  servers' HTTPS port 7443, `--non-interactive`, `--json`, and
  `--vault-password-file`. MMX asks for the vault passphrase once, also
  when the check previews several one-time playbooks.
  [Keep secrets](keep-secrets.md) explains the vault.
- **JSON.** `mmx check-one-time-playbooks --json` prints, for each
  one-time playbook, its `name`, the preview's `run_id`, and the lists
  `done`, `still_needed`, and `not_known`, each with the `node` and a
  `reason`.

## Before you remove a server from the inventory

A one-time playbook that names a server which the inventory no longer has
cannot load, so `mmx validate` fails:

```text
mmx: fleet stage remove_example_file_from_web03: one_time_playbooks/2026-11-15_remove_example_file_from_web03.yml:3:12 (fleet_stages[0].nodes): no node or inventory group named "web03"; a one-time playbook that no node needs any more goes to one_time_playbooks/completed/
```

[Remove MMX from a server](keep-mmx-off-a-server.md#remove-mmx-from-a-server)
says what to do first, and what to do when the server is already gone.

## If something is different

| What you see | What it means and what to do |
| --- | --- |
| `mmx: WHAT_IT_DOES must be lowercase words joined by underscores, such as remove_old_ruby_manager; got "Move-Uploads"` | Write the words in lowercase, joined by underscores. |
| `mmx: one_time_playbooks/completed/2026-11-15_move_uploads_to_files_directory.yml already exists; a one-time playbook name cannot be reused after completion; choose different words for WHAT_IT_DOES` | A completed one-time playbook has the same name. Choose other words. |
| `mmx: dryrun requires a playbook name; available playbooks: example; one-time playbooks: 2026-11-15_move_uploads_to_files_directory` | `mmx dryrun` and `mmx run` need the one-time playbook's full name, with the date. |
| `skipped, unknown — the shell node task has no dryrun command; set dryrun to a read-only command, or run_script_in_dryrun: true for a script that changes nothing, to check the task` | A preview cannot tell whether the `shell` task has work. Add a guard and a `dryrun` command, or a marker file. See [How MMX knows which servers are done](#how-mmx-knows-which-servers-are-done). |
| `not known on 1 server: web03 (MMX could not connect: ...)` | MMX could not reach the server. Fix the connection, then check again. |
| `not known on 1 server: web04 (the dryrun of fleet stage restart_shop_worker could not check node task restart_worker)` | A preview cannot tell whether the server needs the node task. The run's results say why: `mmx results RUN_ID --node web04`, with the run ID from `--output nodes`. |
| `not known` with `a run would be refused: this node refuses management because /etc/mmx/do_not_manage exists` | Someone works on the server by hand. Check again after that person removes the file. See [Keep MMX off a server](keep-mmx-off-a-server.md#keep-runs-off-for-a-while). |
| `mmx: one-time playbook 2026-11-15_move_uploads_to_files_directory is completed: ... move the file back to one_time_playbooks/ to make it runnable again` | The one-time playbook is in `completed/`. Move the file back if a server needs the work again. |
| `mmx: one_time_playbooks/completed/... already exists, so MMX cannot move one_time_playbooks/... into the completed directory` | A file of the same name is in both directories. MMX never replaces a file in `completed/`. Remove or rename one of the two files. |
| `inspected nothing: MMX cannot load the playbook with inventory staging: ...` | The one-time playbook names a node or a group that the inventory doesn't have. See [Several inventories](#several-inventories). `mmx complete-one-time-playbook` refuses such a one-time playbook with exit code 4. |
| `No outstanding one-time playbook: one_time_playbooks/ holds none outside completed/.` | Every one-time playbook is completed. Nothing to do. |

## Where to go next

- [Preview and run changes](preview-and-run-changes.md): results, exit
  codes, `mmx resume`, and `mmx results`.
- [Choose which servers a run touches](choose-which-servers-a-run-touches.md):
  `--limit`, groups, `except_nodes`, and an inventory for each
  environment.
- [Keep MMX off a server](keep-mmx-off-a-server.md): stop MMX for a while,
  or remove MMX from a server.
- [The `shell` module](../reference/modules/shell.md): guards, `dryrun` commands, and
  marker files.
