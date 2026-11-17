# Stop, resume, and read a run

**Who acts:** you, or your coding agent, in your MMX project on your
controller machine: the computer where you run `mmx`.

**When to use this page:** a run is working and you want the run to stop, a run
stopped or failed and you want to finish its work, or you want to see again
what a run did.

**What this page does:** you stop a run, see where each server stands,
finish the stopped work with `mmx resume`, and read saved runs with
`mmx results`. The page also says which saved run a command picks, what to do
when a server is gone, how to see when a playbook last succeeded on each
server, and how long MMX keeps runs.

**Terms:**

- A **node** is a server that MMX manages. A **playbook** lists **fleet
  stages**. Each fleet stage picks nodes, and each of those nodes runs the
  fleet stage's **node tasks**, in order.
- A **run** is one `mmx run`, `mmx dryrun`, or `mmx run-command`, with
  everything that later `mmx resume` commands added to the run. MMX saves
  each run on your computer.
- A **run ID** names one run, such as `b28f4938`. Every run prints its run
  ID at its start and at its end.
- A **node job** is the work of one fleet stage on one node: that node's
  node tasks, in order. A program on the node, MMX's node job worker
  process, carries out the node job.
- A **node task block** groups related node tasks into phases, such as
  `changes`, `validate`, `activate`, and `verify`.

Choose your route:

| You want to | Go to |
| --- | --- |
| Stop a run that is working | [Stop a run](#stop-a-run) |
| Finish the work of a stopped or failed run | [Resume a run](#resume-a-run) |
| See what a run did, or how work on a server stands | [Read a saved run](#read-a-saved-run) |
| Use `mmx status` or `mmx results` without a run ID | [Which run a command picks](#which-run-a-command-picks) |
| Finish a run whose server no longer exists | [A server that is gone](#a-server-that-is-gone) |
| Find servers where a playbook has not succeeded lately | [When a playbook last succeeded on each server](#when-a-playbook-last-succeeded-on-each-server) |
| Know where runs are saved and for how long | [Where runs are kept, and for how long](#where-runs-are-kept-and-for-how-long) |

## Before you start

- You have a project with a playbook, such as `set_up_web_servers` from
  [Write a playbook](write-a-playbook.md). Run every `mmx` command from a
  directory inside the project.
- The examples use three web servers, `web01`, `web02`, and `web03`, in
  the group `web_servers`. The fleet stage `set_up_nginx` sets up the web
  servers one at a time, because of `max_nodes_at_once: 1`
  ([Roll out a change gradually](roll-out-a-change-gradually.md) explains
  the setting):

  ```yaml
  # playbooks/set_up_web_servers.yml
  fleet_stages:
    - id: set_up_nginx
      nodes: web_servers
      max_nodes_at_once: 1         # one web server at a time
      node_tasks:
        - id: install_nginx
          ...
  ```

## Stop a run

To stop a run without a terminal, such as a coding agent, run `mmx stop` from
another shell in the project: see
[Stop a run from another terminal, or as a coding agent](#stop-a-run-from-another-terminal-or-as-a-coding-agent).

1. **Press Ctrl-C once** in the terminal where the run works.

   ```sh
   mmx run set_up_web_servers
   ```

   Here `web01` was finished and `web02` was installing nginx when Ctrl-C
   was pressed. MMX prints a message about the stop, and shows what each
   node is finishing:

   ```text
   Progress: 1 finished · 1 running · 1 not started
     set_up_nginx / web02: running (install_nginx)
   ^CStop requested. No new work will start.
   Each node will finish the current node task or node task block.
   Waiting for the nodes to stop. Press Ctrl-C again to leave the display.
   Progress: 1 finished · 1 running · 1 not started
     set_up_nginx / web02: stop unconfirmed (install_nginx)
   Progress: 1 finished · 1 running · 1 not started
     set_up_nginx / web02: finishing current work; stop acknowledged (install_nginx)
   ```

   No node job starts after Ctrl-C. Each node that is working finishes its
   current node task, or its whole current node task block, and then
   stops. `stop acknowledged` means that the node received the stop.

   **Check:** the run ends with `Run ...: stopped`, the commands to use
   next, and exit code 130:

   ```text
   set_up_nginx / web01: succeeded
   set_up_nginx / web02: stopped
   set_up_nginx / web03: stopped
     not started: stop requested
     ...
     set_up_nginx / web02 / install_nginx: succeeded, changed
       [{"name":"nginx-common","to":"1.26.3-3+deb13u10"},{"name":"nginx","to":"1.26.3-3+deb13u10"}]
     set_up_nginx / web02 / create_site_directory: stopped, unknown — stop requested
   Run b28f4938: stopped
   1 finished · 1 stopped partway through · 1 not started
   Saved: /home/you/shop/.mmx/runs/20261115T053005.182485400-b28f493831cb8c18
   Inspect:                mmx results b28f4938
   Resume unfinished work: mmx resume b28f4938
   Start a new run:        mmx run set_up_web_servers
   ```

   `web02` installed nginx and stopped before `create_site_directory`, so
   `web02` has nginx but no site yet. `web03` never started.

2. **If you don't want to wait, press Ctrl-C a second time.** MMX leaves
   the display, and the nodes go on finishing their current work:

   ```text
   ^CLeaving the display. Some servers may still be finishing work.

   Check node progress:     mmx status
   Repeat the stop request: mmx stop
   ```

   **Check:** `mmx status` asks the nodes how the work stands. While a node
   still finishes its node task, the node's line says `running`, and the
   run says `stopping`:

   ```text
   Selected run 20261115T053005.182485400-b28f493831cb8c18, playbook set_up_web_servers.
   ...
   set_up_nginx / web03: running
   ...
   Run b28f4938: stopping
   2 finished · 1 running
   Saved: /home/you/shop/.mmx/runs/20261115T053005.182485400-b28f493831cb8c18
     set_up_nginx / web03: finishing current work; stop acknowledged (install_nginx)
   Check progress: mmx status b28f4938
   Request stop:   mmx stop b28f4938
   ```

   Run `mmx status` again until the run says `Run ...: stopped`.

### Stop a run from another terminal, or as a coding agent

A coding agent can't press Ctrl-C in the terminal of a run. `mmx stop`
sends the same stop as the first Ctrl-C, from any terminal on the same
computer, inside the project:

```sh
mmx stop
```

`mmx stop` stops the latest run of a playbook, as
[Which run a command picks](#which-run-a-command-picks) says. To stop
another run, such as a run of `mmx run-command`, name the run's ID:
`mmx stop 2a33c932`. `mmx stop` returns after it reaches the nodes, and
shows which nodes received the stop:

```text
Selected run 20261115T053005.182485400-b28f493831cb8c18, playbook set_up_web_servers.
...
Run b28f4938: stopping
2 finished · 1 running
  set_up_nginx / web03: finishing current work; stop acknowledged (install_nginx)
Check progress: mmx status b28f4938
Request stop:   mmx stop b28f4938
```

**Check:** each working node says `stop acknowledged`, and `mmx stop`
exits with code 0. If the run's own `mmx` process still runs, that process
prints `Stop requested. No new work will start.` and waits for the nodes,
as after the first Ctrl-C. That process then exits with code 1 when work
was left undone, or with code 0 when every node had finished its work.
A signal to the run's `mmx` process does the same as Ctrl-C:
`kill -INT` or `kill -TERM` with the process ID.

### What a stop does on each node

- **A node that is working** finishes its current node task. Inside a node
  task block, the node finishes the whole block, with its `validate`,
  `activate`, `verify`, `on_failure`, and `cleanup` node tasks, so a stop
  doesn't fall between writing a configuration file and its reload. Then
  the node stops. The first of its later node tasks says `stopped, unknown —
  stop requested`.
- **A node that had not started** says `stopped` with `not started: stop
  requested`.
- **Later fleet stages** don't start.
- **Nothing is undone.** A package that was installed stays installed.
  [Resume a run](#resume-a-run) finishes the rest.

If `mmx` ends without a stop, such as when you close the terminal or your
computer shuts down, the node job worker process on each node
keeps working and finishes the node's work in the fleet stage. No new
node job starts, because no `mmx` process starts one. `mmx status` then
collects the results from the nodes, and `mmx resume` collects them and
finishes the rest of the run.

## Resume a run

`mmx resume` finishes the work that a run left undone, with the same
files, servers, and values as the run. Use `mmx resume` after a stop, or
after a failure whose cause was outside the project, such as a server that was
briefly unreachable. If you changed a file of the project to fix the
failure, start a new run with `mmx run set_up_web_servers` instead,
because a resume uses the playbook, inventory, and files that the run
saved. Only secrets are read again.

1. **Resume the run.**

   ```sh
   mmx resume
   ```

   Without a run ID, `mmx resume` picks a run as
   [Which run a command picks](#which-run-a-command-picks) says, and names
   that run on its first line. To resume an older run, or a run of
   `mmx run-command`, give the run ID, such as `mmx resume b28f4938`. If a node still works on a node job of the run,
   `mmx resume` first waits for that node job to end.

   **Check:** the resume ends with the same run ID and `succeeded`:

   ```text
   Selected run 20261115T053005.182485400-b28f493831cb8c18, playbook set_up_web_servers.
   Run b28f4938
   Project: mmx_example   Playbook: set_up_web_servers
   Inventory: example (4 nodes)   Selected: 3 nodes
   ...
   set_up_nginx / web01: succeeded
   set_up_nginx / web02: succeeded
   set_up_nginx / web03: succeeded
   ...
     set_up_nginx / web02 / install_nginx: succeeded, unchanged
     set_up_nginx / web02 / create_site_directory: succeeded, changed
   ...
   Run b28f4938: succeeded
   3 finished
   ```

   `web02` ran its node tasks again from the first one. `install_nginx`
   says `unchanged`, because nginx was already installed. The results of
   `web01` are still those of the first attempt, because `web01` had
   finished. If the resume fails or stops, read the results and resume
   again, or start a new run.

2. **Resume once more to see that nothing is left.**

   ```sh
   mmx resume
   ```

   **Check:** MMX prints `Nothing to resume.` and exits with code 0.

### What a resume repeats

| Work in the run | What `mmx resume` does |
| --- | --- |
| A node job that succeeded | Keeps the result. The node job doesn't run again. |
| A node job that failed or stopped partway through | Starts a new node job that runs all of the fleet stage's node tasks on that node from the first one. A node task whose work is already done says `unchanged`. A `shell` node task runs its command again, unless the command has a guard that skips it. |
| A node or fleet stage that never started | Runs it. |
| Work that a failed earlier fleet stage blocked | Runs it after the earlier work succeeds. |

A resume uses what the run saved, even if you changed the files since:
the playbook, the inventory, the templates and files that the playbook
sends, `--limit`, `--only-fleet-stages`, `--var` values, and `--connection`.
A resume reads the run's secrets again, from the same sources as the run
([Which secrets a run needs](../reference/secrets.md#which-secrets-a-run-needs)).
`mmx resume` keeps the run ID, and the results keep each node's earlier
attempts. Nothing resumes by itself: a stopped or failed run waits for
your `mmx resume`.

### What a resume refuses

| Command | Message, with exit code 4 |
| --- | --- |
| `mmx resume --limit web03` | `mmx: resume uses the recorded run's node selection and does not accept --limit; start a new mmx run <playbook> --limit NODE_OR_GROUP[,...] to select different nodes` |
| `mmx resume --var ...` | `mmx: resume uses the recorded run's command-line variables and accepts no --var; start a new mmx run <playbook> --var for different values` |
| `mmx resume 55283a74`, where `55283a74` is a dryrun | `mmx: cannot resume a dryrun; start a new mmx run with a playbook name` |
| `mmx resume 7c1e9b02`, a run that an earlier MMX release started (here 0.1.0, resumed with 0.1.1) | `mmx: this playbook run was started by MMX 0.1.0, but this mmx program is version 0.1.1. A run finished by two MMX releases can leave a node's last-successful record skipped. Resume the run with mmx of release 0.1.0 (such as the previous install, if you kept it), or start a new run with mmx run` |
| `mmx resume 12345678`, a run that MMX no longer keeps or never had | `mmx: no matching saved run` |

`--only-fleet-stages` and `--connection` are refused in the same way.

## Read a saved run

Two commands show a run again:

- **`mmx results`** reads the run from your computer and contacts no
  server. Use it to read a finished run.
- **`mmx status`** first asks each node about the run's work that may
  still be going on, and then shows the run. Use it after you left the
  display, or after `mmx` ended while nodes were working.

`mmx status` and `mmx stop` start no new work: no node job and no fleet
stage.

1. **Show the latest run.**

   ```sh
   mmx results
   ```

   **Check:** the first line names the run that MMX picked, and the
   output ends as the run ended:

   ```text
   Selected run 20261115T053005.182485400-b28f493831cb8c18, playbook set_up_web_servers.
   Output: nodes (change with --output=nodes or --output=summary)
   set_up_nginx / web01: succeeded
   set_up_nginx / web02: succeeded
   set_up_nginx / web03: succeeded
   ...
   Run b28f4938: succeeded
   3 finished
   ```

   `mmx results` exits with code 0 when it could show the run, also when
   the run failed. If MMX picked another run than you meant, give the run
   ID, such as `mmx results b28f4938`. A command with a run ID prints no
   `Selected run` line.

2. **Narrow the results to one server or one node task.**

   ```sh
   mmx results --node web02 --task shop_site
   ```

   `--node` keeps one node. `--fleet-stage` keeps one fleet stage, and
   `--task` keeps one node task, each by its path or the start of its
   path, as the run's output shows the path. Here `--task shop_site`
   keeps every node task of the node task block `shop_site`:

   ```text
   web02: 2 node tasks changed (shop_site/changes/write_site_file, shop_site/activate/reload_nginx). 1 unchanged. 0 failed.
   set_up_nginx / web02: succeeded
     set_up_nginx / web02: facts collected at 2026-11-15T05:30:41.196029006Z
     set_up_nginx / web02 / shop_site/changes/write_site_file: succeeded, changed
     set_up_nginx / web02 / shop_site/changes/write_site_file: created "/etc/nginx/conf.d/shop.conf"
       absent -> 51 bytes, permissions_mode 0644, uid 0, gid 0
     set_up_nginx / web02 / shop_site/activate/reload_nginx: succeeded, changed — service was not running; started
     set_up_nginx / web02 / shop_site/verify/check_home_page: succeeded, unchanged
       http_status_code: 200
       response_body: <h1>Shop</h1>
     set_up_nginx / web02 / shop_site: succeeded, changed
   ```

   **Check:** only the lines of `web02` and `shop_site` appear. A saved
   result can hold more than the run printed, such as the answer of the
   check `check_home_page`.

3. **For a script or a coding agent, ask for JSON.**

   ```sh
   mmx results b28f4938 --node web02 --json
   ```

   **Check:** the output is one JSON object, in the shape that
   [Preview and run changes](preview-and-run-changes.md#output-for-many-servers-and-for-a-coding-agent)
   describes, with only the entries of `web02`.

More ways to read a run:

- `--diff` adds each file's difference, as on
  [Preview and run changes](preview-and-run-changes.md#steps).
  `--diff=all` shows complete differences, where a bare `--diff` shortens
  a long one.
- `--output=summary` puts nodes with the same results on one line, and
  `--output=nodes` shows each node.
- While a run works, the `Progress:` lines and the other lines that show
  work in progress go to standard error. The results go to standard
  output. So `mmx run set_up_web_servers > run.log` saves only the
  results. With `--json`, the JSON goes to standard output, and the
  messages about a stop go to standard error.

### Which run a command picks

`mmx results`, `mmx status`, `mmx stop`, and `mmx resume` take a run ID.
Without one, each command picks the latest run of the project and the
inventory, and names it in its first line, `Selected run ...`. The rules
are in [Selecting saved runs](../reference/commands.md#selecting-saved-runs):
which run each command picks, why a run of `mmx run-command` or a project
that moved needs the run ID, and the short run ID.

## A server that is gone

A server can stop existing while a run has work on it, such as a server
that died, or a temporary server that was deleted. No node can then say
how that work ended, so the work stays unconfirmed: MMX can't know whether
the work still runs. A run with unconfirmed work stays unfinished, and
MMX keeps the run. So you tell MMX that the server is gone.

1. **Ask for the run's status.** Here the server `web03` was deleted
   during a run of `mmx run-command`:

   ```sh
   mmx status 23ef7888
   ```

   **Check:** after the connection times out, the server's line says
   `unknown`, its detail line says `unconfirmed; work may still be running`,
   the exit code is 3, and MMX names the command for a
   server that is gone:

   ```text
   run-command / web03: unknown
     SSH 203.0.113.13: exit status 255: ssh: connect to host 203.0.113.13 port 22: Connection timed out
   Run 23ef7888: stopping
   1 unconfirmed
   ...
     run-command / web03: unconfirmed; work may still be running — SSH 203.0.113.13: exit status 255: ssh: connect to host 203.0.113.13 port 22: Connection timed out
   Check progress: mmx status 23ef7888
   Request stop:   mmx stop 23ef7888
   If server web03 is gone for good: mmx mark-node-work-failed --node web03 23ef7888
   ```

   If the server is only down for a while, start it again and repeat
   `mmx status`: the node then says how the work ended.

2. **Record the work on the gone server as failed.**

   ```sh
   mmx mark-node-work-failed --node web03 23ef7888
   ```

   Without a run ID, the command does the same in every saved run with
   unconfirmed work on `web03`. MMX first tries to reach the server, and
   records nothing if the server answers.

   **Check:** the command exits with code 0 and prints:

   ```text
   Run 23ef7888: recorded 1 node jobs on web03 as failed: the team says this server is gone. The run is failed now.
   ```

   `mmx results 23ef7888` now shows `web03: failed` with the reason `the
   team says this server is gone`. The run ends like any failed run, and
   MMX deletes the run's record when the run's time is up, as
   [Where runs are kept](#where-runs-are-kept-and-for-how-long) says.

`mmx mark-node-work-failed` exits with the most serious outcome over the
runs that it tried:

| Code | Meaning |
| --- | --- |
| `0` | Every run was recorded, or no run had unconfirmed work on the node. |
| `2` | The server answered, so MMX recorded nothing. `mmx status` shows how the work ended. |
| `3` | MMX could not tell whether the server is gone, such as after a refused login, and recorded nothing. |
| `4` | An error, such as a run that its own `mmx` process still holds. End that process first. |

If the server's address now reaches another machine, MMX records the
work as replaced instead, as `mmx status` does.
[`mmx mark-node-work-failed`](../reference/commands.md#mmx-mark-node-work-failed)
has the exit codes and the `--json` fields.

## When a playbook last succeeded on each server

A server that nobody ran the main playbook on for months may differ from
the project files. Each node keeps one small file for each playbook that
succeeded on the node, so you can see when the playbook last succeeded
on the node:

```text
/var/lib/mmx/last_successful_run/<project_name>/<playbook>
```

1. **Read the file on every server.**

   ```sh
   mmx run-command --nodes all -- 'cat /var/lib/mmx/last_successful_run/mmx_example/set_up_web_servers 2>/dev/null || echo "never succeeded here"'
   ```

   **Check:** each server that the playbook succeeded on prints one JSON
   line. A server where the playbook never succeeded prints the fallback
   text:

   ```text
     run-command / db01 / command: succeeded, changed
       never succeeded here
     run-command / web01 / command: succeeded, changed
       {"run_id":"20261115T053005.182485400-b28f493831cb8c18","inventory":"example","limit":"","only_fleet_stages":"","finished_at":"2026-11-15T05:30:49Z"}
   ```

   `finished_at` is when the run or resume that wrote the file ended, in
   UTC. `run_id` is the run that `mmx results` can show while
   MMX keeps the run.

The file says one thing: the last time the playbook ran on the node and
all of the node's work in that run succeeded. The file says nothing about
other playbooks. You choose which playbook's file means "set up", usually
the playbook that applies the whole configuration. A monitoring check can
warn when the file is older than a number of days that you choose: MMX
writes the files, and the monitoring tool reads them.

When a node writes the file:

- After each `mmx run` or `mmx resume`, also one that failed or stopped
  elsewhere, on each node where every fleet stage that selected the node
  succeeded, whether something changed or not. A fleet stage that a false
  `when` skipped on the node counts as succeeded. A run that stopped
  before it knew a later fleet stage's nodes writes no file; its resume
  does.
- A failed or stopped node job on the node keeps the earlier file. So
  does a refusal by `/etc/mmx/do_not_manage`.
- A run with `--limit` or `--only-fleet-stages` writes the file too, and
  records the value in `limit` or `only_fleet_stages`. To count only
  whole runs, check that both fields are empty.
- `mmx dryrun` and `mmx run-command` don't write the file.

The file belongs to root and has mode `0600`, so a check that reads the
file runs as root. [The playbook file reference](../reference/playbook-file.md#record-of-each-playbooks-last-success-on-a-node)
covers runs that stop before a later fleet stage selects its nodes.

## Where runs are kept, and for how long

MMX saves each run, including each dryrun and each `mmx run-command`, in
the project's `.mmx/runs/` directory on your computer: one directory for
each run, named by the run's full ID. Only your account can read the
directory. The `Saved:` line of each run names the directory. `mmx init`
puts `.mmx/` in `.gitignore`, so the runs stay out of Git.

MMX keeps a finished run for 24 hours, counted from the end of the run's
latest attempt. To keep runs longer, set `run_history_retention` in
`mmx_project_settings.yml`:

```yaml
# mmx_project_settings.yml
project_name: mmx_example
default_inventory: example
run_history_retention: 7d    # a duration such as 90m, 24h, or 30d, or forever
```

- **When MMX deletes old runs:** after `mmx run`, `mmx dryrun`,
  `mmx resume`, `mmx results`, `mmx status`, or `mmx stop`, and, for the
  one-time playbooks they check, `mmx check-one-time-playbooks` and
  `mmx complete-one-time-playbook`, MMX deletes
  the expired runs of the same playbook and inventory as the run that the
  command used. For a run of `mmx run-command`, MMX deletes the expired
  runs of `mmx run-command`. Reading a run doesn't make the run last
  longer.
- **Which runs MMX keeps anyway:** the latest run that was not a dryrun,
  for each playbook and inventory, and every run with unconfirmed work.
- **After MMX deletes a run,** `mmx results` and `mmx resume` can't find
  the run, and print `mmx: no matching saved run`. `mmx run
  set_up_web_servers` still works.
- **What the cleanup does not touch:** it never contacts a node, and it
  leaves the records on the nodes alone, including the records of which
  configuration each service last loaded. Older failed and stopped runs
  expire like successful ones. MMX also deletes the copies of `mmx-runner`
  that no kept run needs.
- **A run that MMX cannot read** stops the cleanup: MMX names each such
  run directory and deletes nothing until you remove those directories. An
  upgrade may discard saved history, so MMX never converts a run that
  another release wrote.

Each node also keeps the result of each finished node job for 72 hours,
whatever `run_history_retention` says. So `mmx status` and `mmx resume`
can collect a result that `mmx` missed, such as after your computer lost
its connection. `mmx results` reads your computer's copy, so `mmx results`
works after the node's copy is gone.
A node removes the expired results when it next handles an ordinary
admission or status request; queued and running node jobs stay. A node
cannot answer for the node jobs of the machine it replaced: `mmx status`
and `mmx resume` fail the replaced server's unconfirmed work and give the
replacement as the reason.
[The `run_history_retention` setting](../reference/project-settings.md#run_history_retention) has the remaining rules.

## Exit codes

The exit codes of `mmx results`, `mmx status`, and `mmx stop` are in the
[commands reference](../reference/commands.md#mmx-status). For `mmx run`,
`mmx dryrun`, and `mmx resume`, see
[Preview and run changes](preview-and-run-changes.md#exit-codes).

## If something is different

| What you see | What it means and what to do |
| --- | --- |
| `mmx: no matching saved run`, exit code 4 | No saved run fits: the run ID is wrong, MMX deleted the run, or the project has only runs of `mmx run-command`, which a command without a run ID never picks. Check the run ID on the `Saved:` line, or give the run ID of the `mmx run-command` run. |
| `Selected run ...` names another run than you meant | Without a run ID, MMX picks the latest run, also a dryrun for `mmx results`. Give the run ID. |
| `Nothing to resume.` after a failed run | `mmx resume` picked a later run that succeeded, and doesn't look further back. Give the failed run's ID, such as `mmx resume b28f4938`. |
| `mmx: cannot resume a dryrun; start a new mmx run with a playbook name` | A dryrun only reports what a run would change, so it has nothing to finish. Run `mmx run set_up_web_servers`. |
| `mmx: run ID 2a33c932 is ambiguous; use the full run ID` | Two saved runs share the short ID. Use the full ID from the `Saved:` line. |
| `mmx status` says `running` for a node after a stop | The node still finishes its current node task or node task block. Wait, and run `mmx status` again. |
| `mmx status` exits with code 3 and says `unconfirmed; work may still be running` | MMX can't reach the node. If the server is down for a while, start it and repeat `mmx status`. If the server is gone, see [A server that is gone](#a-server-that-is-gone). |
| A resumed node task changes something that the first attempt already changed | A resumed node job runs its node tasks from the first one. Most modules compare with the server and say `unchanged`. A `shell` node task runs its command again: give the command a guard, as the [`shell` page](../reference/modules/shell.md#guards) shows. |

## Where to go next

- [Preview and run changes](preview-and-run-changes.md): what each result
  and exit code means, and the first resume after a failed run.
- [Roll out a change gradually](roll-out-a-change-gradually.md): what a
  failed server stops, and resuming a rollout.
- [Run a command on servers now](run-a-command-on-servers-now.md): one
  command on some servers, without a playbook.
- [Keep secrets](keep-secrets.md): runs without a terminal, for CI and
  coding agents.
