# Preview and run changes

**Who acts:** you, or your coding agent, in your MMX project on your
controller machine: the computer where you run `mmx`.

**When to use this page:** you have a playbook, such as the one from
[Write a playbook](write-a-playbook.md), and you want to see what it would
change on your servers, and then change them.

**What this page does:** you check the project files, preview the changes
with `mmx dryrun`, run the playbook on one server first, then on all of
them, and read what each result and exit code means.

**Terms:** a **node** is a server that MMX manages. A **playbook** lists
**fleet stages**; each picks nodes, and each node runs the fleet stage's
**node tasks**, in order. The examples use the playbook
`set_up_web_servers` and the server `web01`.

## Steps

1. **Check the project files.**

   ```sh
   mmx validate set_up_web_servers
   ```

   It reads your files on your computer and contacts no server.

   **Check:** it prints `Valid:` and the project's name, with exit code 0.
   Otherwise it names the file, line, and field to fix, with exit code 1.

2. **See which servers the playbook selects.**

   ```sh
   mmx nodes set_up_web_servers
   ```

   **Check:** the `Fleet stages` part lists the servers you expect after
   each fleet stage, such as `set_up_nginx   web_servers   → web01, web02`.
   `mmx explain set_up_web_servers` shows, in addition, every value each
   server's node tasks will get and where it came from.

3. **Preview the changes.**

   ```sh
   mmx dryrun set_up_web_servers --diff
   echo "exit code $?"
   ```

   `mmx dryrun` connects to each selected server and reports what a run
   would change, and changes nothing the playbook describes. `--diff` adds each file's
   difference:

   ```text
   set_up_nginx / web01 / write_home_page: succeeded, would_change
   set_up_nginx / web01 / write_home_page: would create "/srv/shop/public/index.html"
     absent -> 14 bytes, permissions_mode 0644, uid 0, gid 0
     --- before
     +++ after
     @@ -0,0 +1,1 @@
     +<h1>Shop</h1>
   ```

   A first dryrun on a server also sets up MMX there: it installs MMX's
   program in `/opt/mmx/bin/` and starts its HTTPS service, which listens
   on TCP port 7443 on every network address.

   **Check:** read each node task's result as
   [What the results mean](#what-the-results-mean) says, and the exit code
   as [Exit codes](#exit-codes) says. Exit code 0 means nothing would
   change; 2 means something would change; 3 means some work could not be
   checked, so the line of that node task says why.

4. **Run the playbook on one server first.**

   ```sh
   mmx run set_up_web_servers --limit web01
   ```

   `--limit` keeps only the named nodes and groups, separated by commas,
   such as `--limit web01,web02`. It never adds a node that the playbook
   doesn't select.

   **Check:** the header line shows `Limit: web01   Selected: 1 node`, each
   node task says `succeeded` with `changed` or `unchanged`, and the run
   ends with `Run ...: succeeded` and exit code 0. Look at the server, such
   as the page it now serves, before you go on.

5. **Run the playbook on every server it selects.**

   ```sh
   mmx run set_up_web_servers
   ```

   **Check:** `Run ...: succeeded` and exit code 0. `web01` now says
   `unchanged` for every node task.

6. **If the run failed, choose how to go on.** A failed run ends with the
   commands to use next:

   ```text
   Run 6bd33f06: failed
   1 failed
   Saved: /home/you/shop/.mmx/runs/20261115T035339.314724231-6bd33f06bbfa8a71
   Inspect:                mmx results 6bd33f06
   Resume unfinished work: mmx resume 6bd33f06
   Start a new run:        mmx run set_up_web_servers
   ```

   - **You fixed a file of the project:** run `mmx run set_up_web_servers`.
     A new run reads your current files.
   - **Nothing in the project was wrong,** such as a server that was
     briefly unreachable: run `mmx resume`. [What a resume repeats](stop-resume-and-read-a-run.md#what-a-resume-repeats)
     says what a resume does again.

   `mmx results 6bd33f06` shows the saved results of that run again,
   without contacting a server.

   **Check:** the new run or the resume ends with `Run ...: succeeded`.

## What the results mean

Each node task gets a status and a change, such as `succeeded,
would_change`.

| Status | Meaning |
| --- | --- |
| `succeeded` | The node task did its work, or in a dryrun, checked what it would do. |
| `failed` | The node task failed. Its line says why. That server then runs none of its later node tasks or fleet stages, except a failed node task block's own `on_failure` and `cleanup` node tasks. |
| `not_checked` | A dryrun could not check the node task, often because an earlier node task would install or create what it needs first. The line names the earlier node task. A run then checks it for real. Don't add node tasks only to make this go away. |
| `skipped` | The node task didn't run, such as a condition that was false, or in a dryrun, an `activate` node task that waits for the changes before it. |

| Change | Meaning |
| --- | --- |
| `would_change` | In a dryrun: a run would change the server. |
| `changed` | The run changed the server. |
| `unchanged` | The server already matched; nothing was changed. |
| `unknown` | MMX can't tell yet, such as a node task that a dryrun could not check. |

In a dryrun, the work that depends on an earlier node task can't be
checked until that node task has run. For example, nginx's package creates
`/etc/nginx/conf.d`. On a server without nginx, a dryrun reports the site
file in that directory as `not_checked`, and names `install_nginx`:

```text
set_up_nginx / web01 / shop_site/changes/write_site_file: not_checked, unknown — the parent directory /etc/nginx/conf.d does not exist; create it with an earlier directory node task. The earlier node task install_nginx would change this node first and may provide what is missing; if the run then succeeds, change nothing.
```

If the run then succeeds, the playbook is right as it is.

A dryrun with `not_checked` work also prints one line before its outcome,
`Run ...: succeeded`, that says why such work is normal on a new server.
A `shell` check whose program is not installed yet says
`shell command exited with code 127: bash could not find a command in it, usually because its program is not installed yet`.

The same holds in three more cases, each after an earlier node task on the
same server would change it, in the same fleet stage or an earlier one:

- A check that fails, because it checked the server as it is now: a
  `verify` or `validate` node task, `check_new_file`, `wait_for`, `http`
  with `GET` or `HEAD`, or `shell` with `changed_when: false`. The result
  keeps the check's output. Without an earlier change, the failure stands.
- A condition or a value that reads an earlier node task's `change` or an
  output that only the run knows, such as
  `when: {equals: ["{{ .node_tasks.app_config.change }}", changed]}`.
  The same holds for the results of a node task that is `not_checked`,
  and for a variable that a `set_vars` node task sets from such a value.
- A `shell` or `wait_for` node task whose `run_from_directory` doesn't
  exist yet. Without an earlier change, the dryrun fails it, as the run
  would.

## Exit codes

For a dryrun, 0 means nothing would change, 2 means something would
change, and 3 means some work could not be checked. A run exits with 0
when it succeeded and 1 when it did not. A first dryrun on a new server
often exits with 2 or 3, because work that an earlier node task would make
possible shows as `not_checked`. The
[commands reference](../reference/commands.md#exit-codes) has every code,
and [`mmx dryrun`](../reference/commands.md#mmx-dryrun) shows how to read
them in a script.

## Output for many servers, and for a coding agent

- `--output=nodes` shows each server. `--output=summary` groups servers with
  the same results, such as `set_up_nginx / 2 nodes [web01, web02]:
  succeeded`. Without the flag, MMX shows each server for up to ten
  servers, and groups them for more.
- `--json` prints the whole run as one JSON object instead: its `id`, its
  `status`, whether it was a `dryrun`, and in `entries` one entry for each
  server and fleet stage, with the node tasks' results in
  `response.results`: each node task's `status`, `change`, `exit_code`,
  `stdout`, and `stderr` where they apply. `schema_version` is 1.
  [The JSON of a run](../reference/commands.md#the-json-of-a-run) lists every field.
- `--non-interactive` never asks a question, such as for a vault
  passphrase or a sudo password: a missing one is an error. `--json` does
  the same.
- MMX saves every run so `mmx results` can show it again later:
  [Where runs are kept, and for how long](stop-resume-and-read-a-run.md#where-runs-are-kept-and-for-how-long).

## If something is different

| What you see | What it means and what to do |
| --- | --- |
| A dryrun's exit code is 3 and the line says `not_checked` | See [What the results mean](#what-the-results-mean). Run, and change the playbook only if the run fails. |
| `mmx: --limit: no node or inventory group named "web99"`, exit code 4 | A name in `--limit` is in no inventory file. `mmx nodes` lists the names. |
| `MMX could not connect to https://203.0.113.11:7443 (connection timed out).` | Port 7443 is closed between your computer and the server. MMX used SSH for that server and went on. |
| The run fails on one server and succeeds on the others | The others are finished. Fix the cause, then run `mmx resume` or `mmx run`: [Resume a run](stop-resume-and-read-a-run.md#resume-a-run) says which to choose. |
| A node task's line ends with a long reason | The reason is the node task's own words: what was missing and what to do. `mmx describe-module NAME` explains the module's errors. |
