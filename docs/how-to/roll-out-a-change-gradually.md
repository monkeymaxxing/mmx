# Roll out a change gradually

**Who acts:** you, or your coding agent, in your MMX project on your
controller machine: the computer where you run `mmx`.

**When to use this page:** a change should not reach all your servers at
once. You want to try it on one server first, change the others a few at a
time, and stop when a server fails.

**What this page does:** you set how many servers a fleet stage changes at
once with `max_nodes_at_once`, choose what a failed server stops with
`on_node_failure` and `failed_nodes_allowed`, take each server out of the
load balancer while it changes, and run a later fleet stage only on the
servers where an earlier one succeeded.

**Terms:**

- A **node** is a server that MMX manages, listed in an **inventory** file
  in `inventories/`, where **groups** name lists of nodes.
- A **playbook** is a file in `playbooks/` that lists **fleet stages**, run
  in order. Each fleet stage names nodes with `nodes`, and each of those
  nodes runs the fleet stage's **node tasks**.
- A node **fails** a fleet stage when one of its node tasks fails after its
  retries, or when MMX can't reach the node or can't tell how its work
  ended.

| You want to | Go to |
| --- | --- |
| Change one server first, then the others one at a time, and stop on a failure | [Steps](#steps) |
| Choose between stopping the fleet stage, stopping the run, and going on | [Choose what a failed server stops](#choose-what-a-failed-server-stops) |
| Change several servers at a time, or a share of them | [Change several servers at a time](#change-several-servers-at-a-time) |
| Take each server out of the load balancer while it changes | [Take each server out of the load balancer while it changes](#take-each-server-out-of-the-load-balancer-while-it-changes) |
| Run a later fleet stage only where an earlier one worked | [Run a fleet stage only where an earlier one succeeded](#run-a-fleet-stage-only-where-an-earlier-one-succeeded) |

## Before you start

- You have a project where `mmx run` worked, as in
  [Preview and run changes](preview-and-run-changes.md).
- The examples use four web servers, `web01` to `web04`, in the group
  `web_servers`, and a load balancer `lb01`:

  ```yaml
  # inventories/example.yml
  nodes:
    web01: {address: 203.0.113.11, ssh_user: root}
    web02: {address: 203.0.113.12, ssh_user: root}
    web03: {address: 203.0.113.13, ssh_user: root}
    web04: {address: 203.0.113.14, ssh_user: root}
    lb01: {address: 203.0.113.31, ssh_user: root}

  groups:
    web_servers:
      nodes: [web01, web02, web03, web04]
    load_balancers:
      nodes: [lb01]
  ```

- The steps change the playbook `set_up_web_servers` from
  [Write a playbook](write-a-playbook.md).

Without these settings, a fleet stage starts as many of its nodes at once
as the project's `max_nodes_at_once` allows (100 unless the project sets
it), and starts more as those finish. A failed node stops only
its own work, and the other nodes go on.

## Steps

1. **Set the pace and the failure policy on the fleet stage.** Add two
   lines below `nodes` in `playbooks/set_up_web_servers.yml`:

   ```yaml
   # playbooks/set_up_web_servers.yml
   fleet_stages:
     - id: set_up_nginx
       nodes: web_servers
       max_nodes_at_once: 1         # one web server at a time
       on_node_failure: stop_run    # after a failure, start no more nodes or fleet stages
       node_tasks:
         ...
   ```

   **Check:** `mmx explain set_up_web_servers` shows the setting below the
   fleet stage:

   ```text
   set_up_nginx → web01, web02, web03, web04
     max_nodes_at_once: 1
   ```

2. **Make the change.** In the same file, the home page gets a second
   line:

   ```yaml
         - id: write_home_page
           file:
             path: /srv/shop/public/index.html
             permissions_mode: "0644"
             content: |
               <h1>Shop</h1>
               <p>Spring sale: 20% off everything.</p>
   ```

   **Check:** `mmx validate set_up_web_servers` prints `Valid:` and the
   project's name.

3. **Try the change on one server.** A run with `--limit` changes only the
   named servers:

   ```sh
   mmx run set_up_web_servers --limit web01
   ```

   **Check:** the header line says `Limit: web01   Selected: 1 node`, the
   home page says `updated`, and the run ends with `Run ...: succeeded`:

   ```text
   set_up_nginx / web01 / write_home_page: succeeded, changed
   set_up_nginx / web01 / write_home_page: updated "/srv/shop/public/index.html"
     14 bytes, permissions_mode 0644, uid 0, gid 0 -> 54 bytes, permissions_mode 0644, uid 0, gid 0
   ```

   Then look at the server, such as with
   `curl http://203.0.113.11:8080/`, which now prints the sale line. If
   something is wrong, fix the playbook and repeat this step: the other
   servers are unchanged.

4. **Run on every server.**

   ```sh
   mmx run set_up_web_servers
   ```

   MMX changes `web01`, then `web02`, then `web03`, then `web04`, each after
   the one before has finished. `web01` already has the change and says
   `unchanged`. MMX starts the selected nodes in the order of their names,
   sorted alphabetically. To put one node first, run it alone with
   `--limit` before the full run, as step 2 does with `web01`.

   **Check:** the run ends with `Run ...: succeeded` and exit code 0. If a
   server fails, `stop_run` keeps the next ones from starting. Here someone
   had masked nginx on `web03`, so `web03` failed and `web04` was never
   started:

   ```text
   set_up_nginx / web01: succeeded
   set_up_nginx / web02: succeeded
   set_up_nginx / web03: failed
     shop_site: shop_site/activate/reload_nginx: unit nginx.service is masked, so it cannot be started or enabled; write masked: false on this task to unmask it
   set_up_nginx / web04: stopped
     not started: on_node_failure stop_run after set_up_nginx failed on web03
   ...
   Run e29a9ac2: failed
   2 finished · 1 failed · 1 not started
   Saved: /home/you/shop/.mmx/runs/20261115T041808.420271560-e29a9ac2f77814d9
   Inspect:                mmx results e29a9ac2
   Resume unfinished work: mmx resume e29a9ac2
   Start a new run:        mmx run set_up_web_servers
   ```

   `web01` and `web02` have the change. `web03` has the new home page,
   because `write_home_page` ran before the failed node task. `web04` is
   unchanged.

5. **Fix the cause, then finish the run.** Here, on `web03`:
   `systemctl unmask nginx`. Then repeat the failed and unfinished work:

   ```sh
   mmx resume e29a9ac2
   ```

   Here `mmx resume` repeats the work on `web03` and `web04`:
   [What a resume repeats](stop-resume-and-read-a-run.md#what-a-resume-repeats).
   If you fixed a file of the project instead, start a new run with
   `mmx run set_up_web_servers`.

   **Check:** the resume ends with `Run e29a9ac2: succeeded` and
   `4 finished`.

## Choose what a failed server stops

`on_node_failure` on a fleet stage says what a failed node stops:

| `on_node_failure` | What happens after a node fails |
| --- | --- |
| `continue` (the default) | The fleet stage goes on starting its other nodes. Later fleet stages run on every node that didn't fail. |
| `stop_fleet_stage` | No further node starts this fleet stage. Later fleet stages still run on the nodes that didn't fail, also on a node that this fleet stage never started. To keep such a node out, use `requires`, as in [Run a fleet stage only where an earlier one succeeded](#run-a-fleet-stage-only-where-an-earlier-one-succeeded). |
| `stop_run` | No further node starts this fleet stage, and no later fleet stage starts. |

Under each policy:

- **The failed node stops.** It finishes its failure handling and cleanup,
  such as a node task block's `on_failure` node tasks, and then starts none
  of its later fleet stages. A node fails a fleet stage when a node task
  fails after its retries, or when MMX cannot reach the node or the
  outcome stays uncertain.
- **Nodes that are already working finish.** A stop never cancels work
  that has started. Each node and fleet stage that a stop keeps from
  starting is recorded `stopped`, with the cause, such as
  `not started: on_node_failure stop_run after set_up_nginx failed on web03`.

To allow a few failures before the stop, add `failed_nodes_allowed`:

1. **Set the number of failures that the fleet stage tolerates.**

   ```yaml
     - id: set_up_nginx
       nodes: web_servers
       max_nodes_at_once: 1         # one web server at a time
       on_node_failure: stop_run
       failed_nodes_allowed: 1      # one failed web server is allowed; a second one stops the run
   ```

   `failed_nodes_allowed` is a count, such as `1`, or a whole-number
   percentage of the nodes the fleet stage selects after any `--limit`,
   such as `"25%"`, rounded down. Without it, the stop comes with the first
   failure.

   **Check:** `mmx validate set_up_web_servers` prints `Valid:`. Without
   `on_node_failure`, the default `continue` never stops, so the setting is
   refused:

   ```text
   mmx: playbooks/set_up_web_servers.yml:6:27 (fleet_stages[0].failed_nodes_allowed): fleet stage set_up_nginx: failed_nodes_allowed requires on_node_failure: stop_fleet_stage or on_node_failure: stop_run; on_node_failure: continue does not stop after node failures
   ```

2. **Run the playbook.**

   ```sh
   mmx run set_up_web_servers
   ```

   **Check:** with nginx masked on `web03` again, `web03` fails and `web04`
   still runs, because one failure is allowed. The run still ends `failed`,
   because a node failed:

   ```text
   set_up_nginx / web01: succeeded
   set_up_nginx / web02: succeeded
   set_up_nginx / web03: failed
     shop_site: shop_site/activate/reload_nginx: unit nginx.service is masked, so it cannot be started or enabled; write masked: false on this task to unmask it
   set_up_nginx / web04: succeeded
   ...
   Run 74bde40d: failed
   3 finished · 1 failed
   ```

How MMX counts failures:

- The count covers the whole fleet stage. Nodes that are working when the
  failures pass `failed_nodes_allowed` finish, so the failures can pass
  the allowance by up to `max_nodes_at_once`.
- A node that a `when` skips, or that `requires` blocks, doesn't count.
- A `mmx resume` counts only the failures of the resume against the
  allowance.

## Change several servers at a time

`max_nodes_at_once` on a fleet stage is a count, such as `5`, or a
percentage of the nodes the fleet stage selects after any `--limit`, such
as `"50%"`. A percentage rounds down, and is never less than one node.

1. **Set a share of the servers.**

   ```yaml
     - id: set_up_nginx
       nodes: web_servers
       max_nodes_at_once: "50%"
       wait_for_all_in_batch: true
       on_node_failure: stop_run
   ```

   By default, a node starts as soon as another one finishes.
   `wait_for_all_in_batch: true` instead finishes each group of
   `max_nodes_at_once` nodes before any node of the next group starts.

   **Check:** `mmx explain set_up_web_servers` shows the number of nodes
   that the percentage gives:

   ```text
   set_up_nginx → web01, web02, web03, web04
     max_nodes_at_once: 50% = 2 of 4 nodes
   ```

   With `--limit web01`, the same line says `50% = 1 of 1 nodes`.

2. **Run the playbook** with `mmx run set_up_web_servers`.

   **Check:** each server says `succeeded`, and the run ends with
   `Run ...: succeeded`. With two nodes at work, a failure on one of them
   lets the other finish, as in
   [Choose what a failed server stops](#choose-what-a-failed-server-stops).

A fleet stage without `max_nodes_at_once` uses the project's
`max_nodes_at_once` in `mmx_project_settings.yml`, a count such as
`max_nodes_at_once: 20`, or 100 when that is unset. The project setting
takes no percentage:

```text
mmx: mmx_project_settings.yml: line 3 column 20 (max_nodes_at_once): expected int, got !!str
```

## Take each server out of the load balancer while it changes

Some of a server's work happens on another node: a web server is taken out
of the load balancer before it changes, and put back afterwards. A
`for_each_node` loop runs a list of fleet stages for each node of a group,
one node after the other. Each fleet stage in the loop says with `nodes`
where it runs: on the load balancer, or on the current web server.

`lb01` is then a **helper node**: a node that the loop works on for the
current web server.

1. **Write the loop.** This playbook installs package updates on each web
   server while the load balancer sends it no traffic. The commands
   `lb-disable` and `lb-enable` stand for your load balancer's own
   commands:

   ```yaml
   # playbooks/install_updates_behind_load_balancer.yml
   fleet_stages:
     - id: rollout                        # a loop ID you choose
       for_each_node: web_servers         # one iteration for each node of the group
       as: web_server                     # the variable that holds the current node's name
       on_node_failure: stop_run
       fleet_stages:
         - id: take_out_of_load_balancer
           nodes: lb01                    # runs on the load balancer, for the current web server
           node_tasks:
             - id: disable_web_server
               shell:
                 script: /usr/local/bin/lb-disable "$SERVER"
                 env: {SERVER: "{{ .web_server }}"}

         - id: update
           nodes: "{{ .web_server }}"     # runs on the current web server itself
           node_tasks:
             - id: install_package_updates
               apt_package:
                 upgrade_all_packages_within_their_version_rules: true
             - id: check_home_page
               wait_for:
                 url_responds: http://127.0.0.1:8080/
                 response_body_contains: Shop

         - id: put_back_into_load_balancer
           nodes: lb01
           node_tasks:
             - id: enable_web_server
               shell:
                 script: /usr/local/bin/lb-enable "$SERVER"
                 env: {SERVER: "{{ .web_server }}"}
   ```

   **Check:** `mmx validate install_updates_behind_load_balancer` prints
   `Valid:`, and `mmx nodes install_updates_behind_load_balancer` lists
   three fleet stages for each web server, named after the loop and the
   server:

   ```text
   Fleet stages
     rollout[web01]/take_out_of_load_balancer     lb01    → lb01
     rollout[web01]/update                        web01   → web01
     rollout[web01]/put_back_into_load_balancer   lb01    → lb01
     rollout[web02]/take_out_of_load_balancer     lb01    → lb01
     ...
   ```

2. **Try it on one web server.** `--limit` picks which of the loop's nodes
   get an iteration. That iteration still runs its fleet stages on `lb01`,
   which is outside the limit, so MMX lists `lb01` first:

   ```sh
   mmx run install_updates_behind_load_balancer --limit web02
   ```

   **Check:** the output starts with the helper node, and the three fleet
   stages succeed:

   ```text
   Nodes outside --limit that help selected nodes
     lb01   rollout[web02]/take_out_of_load_balancer, rollout[web02]/put_back_into_load_balancer
   Run 64d4ada7
   Project: mmx_example   Playbook: install_updates_behind_load_balancer
   Inventory: example (5 nodes)   Limit: web02   Selected: up to 5 nodes
   ...
   rollout[web02]/take_out_of_load_balancer / lb01: succeeded
   rollout[web02]/update / web02: succeeded
   rollout[web02]/put_back_into_load_balancer / lb01: succeeded
   ...
   Run 64d4ada7: succeeded
   ```

   `mmx nodes` and `mmx explain` with the same `--limit` show the helper
   node too, before you run.

3. **Run on every web server.**

   ```sh
   mmx run install_updates_behind_load_balancer
   ```

   **Check:** the run ends with `Run ...: succeeded`. If the update fails
   on a server, that server stays out of the load balancer, and
   `stop_run` keeps the next servers from starting. Here nginx was stopped
   on `web03`, so the home page check failed after 60 seconds:

   ```text
   rollout[web01]/take_out_of_load_balancer / lb01: succeeded
   rollout[web01]/update / web01: succeeded
   rollout[web01]/put_back_into_load_balancer / lb01: succeeded
   rollout[web02]/take_out_of_load_balancer / lb01: succeeded
   rollout[web02]/update / web02: succeeded
   rollout[web02]/put_back_into_load_balancer / lb01: succeeded
   rollout[web03]/take_out_of_load_balancer / lb01: succeeded
   rollout[web03]/update / web03: failed
     check_home_page: wait_for ended after at most 1m0s: context deadline exceeded; last observation: Get "http://127.0.0.1:8080/": dial tcp 127.0.0.1:8080: connect: connection refused
   rollout[web03]/put_back_into_load_balancer / lb01: stopped
     not started: rollout[web03]/update on web03 is failed: check_home_page: wait_for ended after at most 1m0s: context deadline exceeded; last observation: Get "http://127.0.0.1:8080/": dial tcp 127.0.0.1:8080: connect: connection refused
   ...
     rollout[web04]: stopped — not started: on_node_failure stop_run after rollout failed on web03
   Run 11a21a85: failed
   2 finished · 1 failed · 1 stopped partway through
   ```

4. **Fix the server, then finish the run.** Here, on `web03`:
   `systemctl start nginx`. Then:

   ```sh
   mmx resume 11a21a85
   ```

   **Check:** the resume ends with `Run 11a21a85: succeeded`. It repeated
   `web03` from `update`, its first unfinished fleet stage, put `web03`
   back into the load balancer, and then ran the iteration for `web04`.
   `mmx results 11a21a85 --fleet-stage 'rollout[web03]'` shows one
   iteration's results, such as the output `web03 is back in service` of
   `enable_web_server`.

How the loop counts nodes:

- **One loop node at a time, by default.** `max_nodes_at_once` on the loop,
  a count or a percentage of the loop's nodes, lets more web servers be in
  their fleet stages at once. A web server keeps its place until its last
  fleet stage in the loop ends. `wait_for_all_in_batch: true` finishes each
  group before the next starts.
- **A helper node works for one loop node at a time.** When two web servers
  need `lb01` at once, the second one's fleet stage on `lb01` waits until
  the first one's ends.
- **A failure ends that loop node's iteration only.** Web servers that are
  already in their fleet stages finish them, so a healthy server that was
  taken out is put back.
- **The loop's `on_node_failure` and `failed_nodes_allowed` count the
  loop's nodes,** and decide whether more loop nodes start. The fleet
  stages inside the loop take no `on_node_failure` or
  `failed_nodes_allowed` of their own.

What else the loop takes:

- `for_each_node` takes the same node and group names as `nodes`, and also
  `except_nodes`, `when`, and `skip_when_no_nodes_are_selected`. It takes
  no `requires`.
- A loop's outputs are keyed by the loop's node, such as
  `.outputs.rollout.web02.healthy`;
  [Use a result from one server on another](use-a-result-from-one-server-on-another.md) covers
  outputs.
- A reboot in the middle of an iteration is two fleet stages inside the
  loop, because `reboot` is the last node task of a fleet stage.
- For a list of things that are not nodes, such as customers' sites, use
  `loop` over fleet stages instead. `--limit` doesn't pick a `loop`'s
  iterations.
- MMX lists a helper node before the run only when the fleet stage names
  it in a way that MMX can work out without connecting: node and group
  names, `vars`, and the loop's variable. A fleet stage whose `nodes`
  reads an earlier fleet stage's output selects its nodes during the run.
  There, a node outside the limit isn't reached: its work is blocked, with
  a reason that names the limit and says to name the helper node in the
  playbook or to widen the limit.

The [rolling-deployment example](../../examples/rolling-deployment/README.md)
is a complete project of this kind: it checks each application server,
takes it out of the load balancer only when it needs an update, updates
and checks it, and puts it back.

## Run a fleet stage only where an earlier one succeeded

Under `continue` or `stop_fleet_stage`, a node's failure keeps only that
node out of its later fleet stages. With
`stop_fleet_stage`, the nodes that never started a fleet stage still run
the later ones. `requires` on a fleet stage names earlier fleet stages that
must have succeeded first:

- `same_node` lists fleet stages that must have succeeded on the node about
  to start this fleet stage.
- `all_nodes` lists fleet stages that must have succeeded on every node
  they selected.

1. **Add `requires` to the later fleet stage.** This playbook backs up the
   price page on each web server, one at a time, and writes the new price
   page only where the backup worked:

   ```yaml
   # playbooks/publish_price_list.yml
   fleet_stages:
     - id: back_up_price_list
       nodes: web_servers
       max_nodes_at_once: 1                # one web server at a time
       on_node_failure: stop_fleet_stage   # after a failure, start no more web servers here
       node_tasks:
         - id: copy_old_price_list
           shell:
             script: |
               if [ -f /srv/shop/public/prices.html ]; then
                 cp -p /srv/shop/public/prices.html /var/backups/prices.html.old
               fi

     - id: write_price_list
       nodes: web_servers
       requires:
         same_node: [back_up_price_list]   # back_up_price_list succeeded on this web server
       node_tasks:
         - id: write_prices_page
           file:
             path: /srv/shop/public/prices.html
             permissions_mode: "0644"
             content: |
               <h1>Prices</h1>
               <p>Mugs: 14 euros</p>
   ```

   **Check:** `mmx validate publish_price_list` prints `Valid:`. A
   requirement must name an earlier fleet stage's `id`; a later or unknown
   one is refused. When both fleet stages select from the inventory, a
   `same_node` fleet stage must select every node of the fleet stage that
   requires it.

2. **Run the playbook.**

   ```sh
   mmx run publish_price_list
   ```

   **Check:** `Run ...: succeeded`. When the backup fails on a server, the
   new page is written only where the backup succeeded. Here
   `/var/backups` on `web03` was a file instead of a directory:

   ```text
   back_up_price_list / web01: succeeded
   back_up_price_list / web02: succeeded
   back_up_price_list / web03: failed
     copy_old_price_list: exit status 1
   back_up_price_list / web04: stopped
     not started: on_node_failure stop_fleet_stage after back_up_price_list failed on web03
   write_price_list / web01: succeeded
   write_price_list / web02: succeeded
   write_price_list / web03: blocked
     requires same_node fleet stage back_up_price_list to succeed on web03
   write_price_list / web04: blocked
     requires same_node fleet stage back_up_price_list to succeed on web04
   ...
     back_up_price_list / web03 / copy_old_price_list: failed, changed — exit status 1
       exit code: 1
       cp: cannot stat '/var/backups/prices.html.old': Not a directory
   ...
   Run f19600a5: failed
   ```

   Without `requires`, `web04` would have written the new page with no
   backup: a stop that keeps a node from starting is not a failure of that
   node. Fix the cause, then run `mmx resume f19600a5`: it backs up and
   writes the page on `web03` and `web04`.

To change no server unless the earlier fleet stage succeeded on every
server, write `all_nodes` instead:

```yaml
    requires:
      all_nodes: [back_up_price_list]   # back_up_price_list succeeded on every web server
```

Then the same failure on `web03` blocks every web server:

```text
write_price_list / web01: blocked
  requires all_nodes fleet stage back_up_price_list to succeed (web03 is failed)
```

How `requires` decides:

- **Only success counts.** A fleet stage that succeeded without a change
  meets the requirement. One that failed, was skipped by its `when`, was
  blocked, was stopped, or has an uncertain outcome doesn't.
- **Blocked work fails the run.** `mmx run` ends with `failed` and lists
  each blocked node with its reason. In a dryrun, work whose unmet
  requirements are all `not_checked` is itself `not_checked`.
- **A limit keeps the requirements.** A `--limit` that leaves out a
  required fleet stage's nodes is refused before any connection, such as
  `--limit excludes fleet stage check_load_balancer, which fleet stage write_price_list requires; include the required nodes in --limit or omit --limit`.
  With a limit, `all_nodes` means every node that the required fleet stage
  selects after the limit.
- **Resume repeats the requirement first.** `mmx resume` repeats failed and
  blocked work in the playbook's order: a required fleet stage that failed
  runs again, and the work that requires it runs once it succeeds. A
  required fleet stage that its `when` skipped counts as finished, so
  resume doesn't repeat it, and the work that requires it stays blocked.
  Start a new `mmx run` to check that `when` again.

## If something is different

| What you see | What it means and what to do |
| --- | --- |
| `stopped` with `not started: on_node_failure stop_run after ... failed on web03` | The failure policy kept this node from starting. Fix the failed node, then `mmx resume` runs the stopped nodes too. |
| `blocked` with `requires same_node fleet stage ... to succeed on web04` | An earlier fleet stage didn't succeed on this node, so this one didn't start. `mmx resume` runs it once the earlier one succeeds. |
| `failed_nodes_allowed requires on_node_failure: stop_fleet_stage or on_node_failure: stop_run` | `failed_nodes_allowed` has no effect with `continue`. Set `on_node_failure`, or remove `failed_nodes_allowed`. |
| `max_nodes_at_once must be a positive count or a whole-number percentage from 1% to 100%, such as "34%"` | The value on a fleet stage or a loop is not a count or a percentage. |
| `mmx_project_settings.yml: ... (max_nodes_at_once): expected int, got !!str` | The project setting takes a count only. Put a percentage on the fleet stage. |
| More nodes failed than `failed_nodes_allowed` | Nodes that were already working finished. Lower `max_nodes_at_once` to stop sooner. |
| A server stays out of the load balancer after a run | Its update failed, and the loop left it out on purpose. Fix the server, then `mmx resume` puts it back. |
| `Nodes outside --limit that help selected nodes` before a limited run | The loop's fleet stages reach these nodes for the servers in the limit, such as the load balancer. Check that you expect them. |

## Where to go next

- [Choose which servers a run touches](choose-which-servers-a-run-touches.md):
  `--limit`, several inventories, `except_nodes`, and a `when` on a fleet
  stage.
- [Preview and run changes](preview-and-run-changes.md): results, exit
  codes, `mmx resume`, and `mmx results`.
- Putting things back on the server when a node task block fails:
  [Change a configuration and reload its service](change-a-configuration-and-reload-its-service.md).
