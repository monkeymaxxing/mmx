# Choose which servers a run touches

**Who acts:** you, or your coding agent, in your MMX project on your
controller machine: the computer where you run `mmx`.

**When to use this page:** you want a command to work on some of your
servers only, on another environment's servers, or on every server except
a few, and you want to see the selection before anything runs.

**What this page does:** you see which servers a playbook selects, narrow a
run with `--limit`, choose an environment with `--inventory`, leave servers
out with `except_nodes`, keep a fleet stage for a group that may be empty,
run only some fleet stages, and let a fleet stage decide for each server
during the run.

**Terms:**

- A **node** is a server that MMX manages. An **inventory** is a file in
  `inventories/` that lists your nodes, how to reach them, and their
  **groups**: named lists of nodes, such as `web_servers`.
- A **playbook** is a file in `playbooks/` that lists **fleet stages**, run
  in order. Each fleet stage names nodes with `nodes`, and each of those
  nodes runs the fleet stage's **node tasks**.
- A fleet stage's **selection** is the list of nodes it runs on. The
  **limit** is the list of nodes and groups that you give with `--limit`.

| You want to | Go to |
| --- | --- |
| See which servers a playbook will touch | [See the selection before you run](#see-the-selection-before-you-run) |
| Change one server first, or a few | [Run on some servers only](#run-on-some-servers-only) |
| Work on staging or production with the same playbooks | [Use one inventory for each environment](#use-one-inventory-for-each-environment) |
| Do work on every server except a few | [Leave some servers out of a fleet stage](#leave-some-servers-out-of-a-fleet-stage) |
| Keep a fleet stage for a group that is empty in some inventories | [Keep a fleet stage for a group that may be empty](#keep-a-fleet-stage-for-a-group-that-may-be-empty) |
| Apply one part of a playbook | [Run only some fleet stages](#run-only-some-fleet-stages) |
| Skip some servers by what the run finds on them | [Decide for each server during the run](#decide-for-each-server-during-the-run) |

## Before you start

- You have a project where `mmx run` worked, as in
  [Preview and run changes](preview-and-run-changes.md).
- The examples use this inventory: four web servers and one load balancer,
  `lb01`, each in a group:

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

- The examples use the playbook `set_up_web_servers` from
  [Write a playbook](write-a-playbook.md), and a second playbook that this
  page writes, `set_up_all_servers`.

## See the selection before you run

1. **List the nodes that each fleet stage selects.**

   ```sh
   mmx nodes set_up_web_servers
   ```

   `mmx nodes` reads your files on your computer. It contacts no server and
   doesn't ask for the vault passphrase.

   **Check:** the `Fleet stages` part ends the output, with one line for
   each fleet stage: its ID, what its `nodes` says, and after the arrow the
   nodes it selects:

   ```text
   Fleet stages
     set_up_nginx   web_servers   → web01, web02, web03, web04
   ```

   If a node is missing or extra, fix the group in the inventory or `nodes`
   in the playbook, and run `mmx nodes` again.

2. **See each node's work and values, if you need them.**

   ```sh
   mmx explain set_up_web_servers
   ```

   **Check:** the output has one part for each fleet stage, such as
   `set_up_nginx → web01, web02, web03, web04`, followed by every node task
   of each node, every value, and where the value came from. It also
   contacts no server.

## Run on some servers only

`--limit` keeps only the named nodes and groups from each fleet stage's
selection, separated by commas. In a `for_each_node` loop, `--limit` picks
the loop's nodes, and the loop's fleet stages still reach their helper
nodes, as [Roll out a change gradually](roll-out-a-change-gradually.md)
describes. A limit never adds a node that a fleet stage doesn't select. Trying a change on one server before all of them, often
called a canary, is a run with `--limit` followed by a run without it.

1. **See what the limit keeps.**

   ```sh
   mmx nodes set_up_web_servers --limit web01,web02
   ```

   **Check:** the output shows the limit and the selection after it:

   ```text
   Playbook: set_up_web_servers   Inventory: example (inventories/example.yml, 5 nodes)
   Limit: web01,web02
   ...
   Fleet stages (limit applied)
     set_up_nginx   web_servers   → web01, web02
   ```

   A name in `--limit` can also be a group, such as `--limit web_servers`.
   If the command is refused, see
   [If something is different](#if-something-is-different).

2. **Preview the change on those servers.**

   ```sh
   mmx dryrun set_up_web_servers --limit web01,web02
   ```

   **Check:** the header line shows the limit and the number of servers it
   selected, such as
   `Inventory: example (5 nodes)   Limit: web01,web02   Selected: 2 nodes`.
   [Preview and run changes](preview-and-run-changes.md#what-the-results-mean)
   explains the results.

3. **Run on those servers.**

   ```sh
   mmx run set_up_web_servers --limit web01,web02
   ```

   **Check:** the same header line, and the run ends with
   `Run ...: succeeded`. A fleet stage that the limit leaves without any
   node is reported as `not selected — left out by the limit`.

4. **Run on the rest.** When the servers you chose work as you expect,
   run without `--limit`:

   ```sh
   mmx run set_up_web_servers
   ```

   **Check:** `Run ...: succeeded`. The servers that the first run changed
   now say `unchanged`.

A `mmx resume` repeats the failed run's limit, and refuses a new
`--limit`: see [What a resume refuses](stop-resume-and-read-a-run.md#what-a-resume-refuses).

To change one server at a time with its load balancer, a limit picks the
web servers, and MMX still reaches the load balancer that works for them.
[Roll out a change gradually](roll-out-a-change-gradually.md#take-each-server-out-of-the-load-balancer-while-it-changes)
covers it.

## Use one inventory for each environment

Keep one inventory file for each environment, such as
`inventories/example.yml` for production and `inventories/staging.yml` for
staging. The playbooks stay the same; the inventory says which servers they
reach.

1. **Write the second inventory with the same group names.**

   ```yaml
   # inventories/staging.yml
   nodes:
     staging-web01: {address: 203.0.113.41, ssh_user: root}

   groups:
     web_servers:
       nodes: [staging-web01]
     load_balancers:
       nodes: []                  # staging has no load balancer
   ```

   Reason: a playbook names groups, such as `nodes: web_servers`. The same
   group name in each inventory lets one playbook work on each environment.

   **Check:** `mmx nodes --inventory staging` lists `staging-web01` and the
   two groups.

2. **Name the inventory in each command.**

   ```sh
   mmx nodes set_up_web_servers --inventory staging
   mmx run set_up_web_servers --inventory staging
   ```

   `--inventory NAME` selects `inventories/NAME.yml` for that command.
   Without it, MMX uses `default_inventory` from `mmx_project_settings.yml`,
   here `example`.

   **Check:** the run's header line names the inventory, such as
   `Inventory: staging (1 node)   Selected: 1 node`.

## Leave some servers out of a fleet stage

`except_nodes` on a fleet stage removes nodes from what `nodes` selects. Use
it for work that belongs on every server but a few: a server added to the
inventory later gets the work without a change to the playbook.

1. **Add `except_nodes` to the fleet stage.** This playbook turns on
   automatic security updates on every server except the load balancers,
   which you update by hand. Create `playbooks/set_up_all_servers.yml`:

   ```yaml
   # playbooks/set_up_all_servers.yml
   fleet_stages:
     - id: turn_on_security_updates
       nodes: all                        # every node in the inventory
       except_nodes: [load_balancers]    # minus the nodes of this group
       node_tasks:
         - id: install_unattended_upgrades
           apt_package:
             packages:
               - {name: unattended-upgrades, version: newest_when_first_installed}

     - id: set_up_load_balancer
       nodes: load_balancers
       skip_when_no_nodes_are_selected: true   # leave it out when the group is empty
       node_tasks:
         - id: install_haproxy
           apt_package:
             packages:
               - {name: haproxy, version: newest_when_first_installed}
   ```

   `except_nodes` takes the same names as `nodes`: node names, group
   names, or `all`, alone or in a list. The second fleet stage is explained
   in [the next part](#keep-a-fleet-stage-for-a-group-that-may-be-empty).

   **Check:** `mmx nodes set_up_all_servers` shows the subtraction and its
   result:

   ```text
   Fleet stages
     turn_on_security_updates   all except load_balancers   → web01, web02, web03, web04
     set_up_load_balancer       load_balancers              → lb01
   ```

2. **Run the playbook.**

   ```sh
   mmx run set_up_all_servers
   ```

   **Check:** the run ends with `Run ...: succeeded`, and
   `turn_on_security_updates` ran on `web01` to `web04` only:

   ```text
   turn_on_security_updates / 4 nodes [web01, web02, web03, web04]: succeeded
   set_up_load_balancer / 1 node [lb01]: succeeded
   ```

MMX works out `nodes` minus `except_nodes` from the inventory on your
computer, before it connects to any server. A `--limit` then narrows the
result further. An `except_nodes` that selects no node, such as an empty
group, removes nothing. `except_nodes` can also be one whole reference to a
variable, such as `except_nodes: "{{ .vars.excluded_nodes }}"`, the same as
`nodes`; an empty list there removes nothing too.

## Keep a fleet stage for a group that may be empty

A fleet stage whose selection is known before the run and selects no node
stops the whole command, and MMX names
the fleet stage. So a group that someone emptied by mistake doesn't make
its work disappear without a word. Some groups are empty on purpose: in the
staging inventory above, `load_balancers` has no node. Without the setting,
`set_up_all_servers` is refused on staging:

```text
mmx: fleet stage set_up_load_balancer: playbooks/set_up_all_servers.yml:13:12 (fleet_stages[1].nodes): node selection is empty; to leave the fleet stage out of the run when it selects no node, set skip_when_no_nodes_are_selected: true
```

1. **Set `skip_when_no_nodes_are_selected: true` on the fleet stage of that
   group,** as `set_up_load_balancer` has it in
   [the playbook above](#leave-some-servers-out-of-a-fleet-stage).

   **Check:** `mmx nodes set_up_all_servers --inventory staging` shows the
   fleet stage left out:

   ```text
   Fleet stages
     turn_on_security_updates   all except load_balancers   → staging-web01
     set_up_load_balancer       load_balancers              → none; selection contains no nodes (skip_when_no_nodes_are_selected)
   ```

2. **Run the playbook on that inventory.**

   ```sh
   mmx run set_up_all_servers --inventory staging
   ```

   **Check:** the run ends with `Run ...: succeeded`, and the result says:

   ```text
     set_up_load_balancer: not selected — selection contains no nodes (skip_when_no_nodes_are_selected)
   ```

When you add a node to the group, the fleet stage runs on it, with no
change to the playbook. While the group is empty, the run needs none of the
secrets that the fleet stage reads, as
[Which secrets a run needs](../reference/secrets.md#which-secrets-a-run-needs)
explains.

What the setting does in other cases:

- It looks at the selection from `nodes` minus `except_nodes`, before
  `--limit`. A fleet stage that selects nodes ignores it. A name that is not
  a node or group in the inventory that the command uses is still an
  error.
- A fleet stage whose `nodes` reads the outputs of an earlier fleet stage
  selects its nodes during the run. With the setting, an empty selection is
  left out then; without it, the fleet stage fails.
- A left-out fleet stage's outputs, such as
  `.outputs.set_up_load_balancer`, are an empty mapping, and a later
  fleet stage's `requires: {all_nodes: [set_up_load_balancer]}` is met.
  Reading one node's output from it is an error, as for any node that a
  fleet stage doesn't select.
- A `for_each_node` loop takes the setting too. A `loop` over things that
  are not nodes doesn't; put it on the fleet stages inside that loop.
- In JSON output, such a fleet stage has
  `"left_out_because_it_selects_no_nodes": true`, in `mmx nodes --json`,
  `mmx explain --json`, and `mmx results --json`.

## Run only some fleet stages

A project can keep all its server setup in one playbook, with one fleet
stage for each part that the team applies on its own. `--only-fleet-stages`
then applies some of those parts. It takes fleet stage IDs, separated by
commas, and works with `nodes`, `explain`, `dryrun`, and `run`.

1. **See what the option keeps.**

   ```sh
   mmx nodes set_up_all_servers --only-fleet-stages set_up_load_balancer
   ```

   **Check:** the fleet stages you didn't name say
   `none; left out by --only-fleet-stages`.

2. **Run it.**

   ```sh
   mmx run set_up_all_servers --only-fleet-stages set_up_load_balancer
   ```

   **Check:** the header line shows the option, and the run ends with
   `Run ...: succeeded`:

   ```text
   Inventory: example (5 nodes)   Only fleet stages: set_up_load_balancer   Selected: 1 node
   ...
   set_up_load_balancer / lb01: succeeded
     set_up_load_balancer / lb01 / install_haproxy: succeeded, changed
     ...
     turn_on_security_updates: not selected — left out by --only-fleet-stages
   Run b8aff5c7: succeeded
   ```

How the option works:

- The named fleet stages run in the order of the playbook. The option
  combines with `--limit` and `--var`.
- It never adds a fleet stage you didn't name, and it never leaves out one
  that a kept fleet stage depends on. If a kept fleet stage `requires` a
  left-out one, or reads its outputs, MMX refuses the command before it
  connects to any server, and prints the option value to use:

  ```text
  mmx: --only-fleet-stages write_price_list leaves out fleet stage back_up_price_list, which fleet stage write_price_list requires; use --only-fleet-stages back_up_price_list,write_price_list
  ```

- Work that reads a left-out fleet stage's outcome, such as
  `{{ .fleet_stages.set_up_load_balancer.lb01.status }}`, is blocked during
  the run, and the reason names `--only-fleet-stages`.
- A loop over fleet stages is kept or left out whole, by its own `id`. A
  fleet stage inside a loop can't be named alone.
- MMX has no tags and no option to start at a node task: the fleet stage is
  the part you choose.
- `mmx resume` refuses the option
  ([What a resume refuses](stop-resume-and-read-a-run.md#what-a-resume-refuses)).
  To work on other fleet stages, start a new run.

## Decide for each server during the run

A `when` on a fleet stage is checked by `mmx` for each node, just before
that node would start the fleet stage. A node whose condition is false is
recorded `skipped`. It starts no work, takes no place in
`max_nodes_at_once`, and doesn't count as a failure.

1. **Add a `when` to the fleet stage.** This playbook installs hardware
   sensor tools on physical servers only:

   ```yaml
   # playbooks/install_hardware_sensors.yml
   fleet_stages:
     - id: install_sensors
       nodes: all
       when:                       # checked for each node, just before it starts
         equals: ["{{ .node.virtualization.type }}", none]   # only physical servers
       node_tasks:
         - id: install_lm_sensors
           apt_package:
             packages:
               - {name: lm-sensors, version: newest_when_first_installed}
   ```

   `.node.virtualization.type` is a fact: a value that MMX measures on the
   node. It is `none` on physical hardware, and names the kind of virtual
   machine or container otherwise, such as `kvm` or `lxc`.

   **Check:** `mmx nodes install_hardware_sensors` marks the selection:

   ```text
   Fleet stages
     install_sensors   all   → lb01, web01, web02, web03, web04; narrowed during the run by `when`
   ```

2. **Run it.**

   ```sh
   mmx run install_hardware_sensors
   ```

   **Check:** each node says `succeeded` or `skipped`, with the condition as
   the reason. On servers that are all containers, every node is skipped:

   ```text
   install_sensors / lb01: skipped
     when is false: {"equals":["{{ .node.virtualization.type }}","none"]}
   install_sensors / web01: skipped
     when is false: {"equals":["{{ .node.virtualization.type }}","none"]}
   ...
   Run 1cb62dcd: succeeded
   ```

What a fleet stage's `when` reads, and when it contacts the node:

- It reads `.vars` (the node's own variables), `.inventory`, the node's name
  as `.node.name`, a loop's variable, and the outputs and outcomes of
  earlier fleet stages. So a run can find the database leader in one fleet
  stage and update only the other servers in the next, with a condition
  such as `not: "{{ .outputs.find_leader[.node.name].is_leader }}"`.
- A condition that reads a fact, as above, makes `mmx` ask that node for
  its facts just before it decides, over the usual connection. The skipped
  node's result keeps the facts used. A node that can't be reached or
  doesn't answer fails, as its work would have. A condition that reads no
  fact contacts no node. `mmx stop` and Ctrl-C interrupt a request for
  facts that doesn't answer.
- `mmx resume` doesn't check a `when` again for a node that it already let
  start. A new `mmx run` checks every `when` again.

## If something is different

| What you see | What it means and what to do |
| --- | --- |
| `mmx: --limit: no node or inventory group named "web99"`, exit code 4 | A name in `--limit` is not a node or group in the inventory that the command uses. `mmx nodes` lists the names. |
| `mmx: --limit lb01 selects no node in any fleet stage`, exit code 4 | The limit and the playbook have no node in common. A limit only removes nodes; check the playbook's selection with `mmx nodes PLAYBOOK`. |
| `mmx: --limit excludes fleet stage check_load_balancer, which fleet stage write_price_list requires; include the required nodes in --limit or omit --limit` | A kept fleet stage needs an earlier fleet stage on nodes that the limit leaves out. Add those nodes to `--limit`, such as `--limit web01,lb01`. |
| `mmx: --limit excludes fleet stage prepare_database, whose outputs fleet stage configure_app reads` | A kept fleet stage reads an output of a node that the limit leaves out. Add that node to the limit, such as `--limit db01,web01`, or omit `--limit`. |
| The command is refused because a kept fleet stage reads the whole output collection of a fleet stage or a `for_each_node` loop that the limit narrows | The kept fleet stage needs every node's output. Widen the limit, or run without it. |
| A node task is blocked during the run with the limit named as the cause | The work tried to read the output of a node that the limit left out. Widen the limit. |
| `mmx: fleet stage set_up_load_balancer: ... node selection is empty; to leave the fleet stage out of the run when it selects no node, set skip_when_no_nodes_are_selected: true` | The fleet stage's group has no node in this inventory. Add a node, or set the setting if the group may be empty. |
| `... (fleet_stages[0].except_nodes): no node or inventory group named "load_balancer"` | A name in `except_nodes` is not a node or group in the inventory that the command uses. Fix the name. |
| `... (fleet_stages[0].except_nodes): except_nodes leaves no node of nodes; ...` | `except_nodes` removes every node that `nodes` selects. Check both, or set `skip_when_no_nodes_are_selected: true`. |
| `mmx: --only-fleet-stages: the playbook has no fleet stage "set_up_lb"; its fleet stages are turn_on_security_updates, set_up_load_balancer` | A fleet stage ID is misspelled. Use one of the listed IDs. |

## Where to go next

- [Roll out a change gradually](roll-out-a-change-gradually.md): how many
  servers change at once, what a failure stops, and one server at a time
  behind a load balancer.
- [Preview and run changes](preview-and-run-changes.md): what each result
  and exit code means.
- Inventory groups and variables:
  [Describe your servers](describe-your-servers.md).
- Which saved run `status`, `stop`, `results`, and `resume` choose without
  a run ID: [Which run a command picks](stop-resume-and-read-a-run.md#which-run-a-command-picks).
