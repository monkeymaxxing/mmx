# Playbook file reference

This page lists every field of a playbook file, for looking up. A
**playbook** is a file `playbooks/<name>.yml` in your MMX project. It lists
**fleet stages**, which run in order. Each fleet stage selects **nodes**,
the servers that MMX manages, from the **inventory**, the file
`inventories/<name>.yml` that lists them. Each selected node runs the
fleet stage's **node tasks** in order. Most node tasks call one **module**,
the code that does the work, such as `file` or `apt_package`.
A **node task block** (`task_block`) groups a change with the node tasks
that put the change into use and check it. The page also lists the
references and template functions that a value can use, and the facts that
MMX measures on each node. To write a playbook step by step, see
[Write a playbook](../how-to/write-a-playbook.md).

Each module's own inputs are on the module's page in
[reference/modules/](modules/README.md), or in `mmx describe-module NAME`.

## The playbook file

```yaml
# playbooks/set_up_web_servers.yml
command_line_vars:            # values given with --var on the command line
  release:
    type: string
    default: "1.4.3"

fleet_stages:
  - id: set_up_nginx          # a fleet stage
    nodes: web_servers        # the nodes it selects
    max_nodes_at_once: 1      # one node at a time
    node_tasks:               # what each selected node does, in order
      - id: install_nginx     # a node task that calls the module apt_package
        apt_package:
          packages:
            - {name: nginx, version: newest_when_first_installed}

      - show: "Release {{ .vars.release }} on {{ .node.name }}"
```

| Field | Value | What it does |
| --- | --- | --- |
| `fleet_stages` | A list of fleet stages; required, not empty | The work, in order. See [Fleet stage fields](#fleet-stage-fields). |
| `command_line_vars` | A mapping; optional | Variables that `--var NAME=VALUE` sets. See [command_line_vars](#command_line_vars). |

- **The name.** The file's name without `.yml` is the playbook's name,
  which each command takes: `mmx run set_up_web_servers`. A command takes
  the name, never a path: `mmx run playbooks/set_up_web_servers.yml` fails
  with `playbook must be a name, not a path`. A playbook name starts with
  a letter or an underscore. A name that starts with a date is a
  [one-time playbook](#one-time-playbooks) in `one_time_playbooks/`.
- **YAML that MMX refuses.** The file is one YAML document. MMX refuses
  anchors and aliases (`&name`, `*name`), merge keys (`<<`), explicit tags
  (`!!str`), and duplicate keys. A boolean is `true` or `false`: `True`
  fails with `spell booleans true or false`, and `yes` with
  `expected bool, got !!str`. A misspelled field fails with its line and
  column: `line 4 column 5 (fleet_stages[0].unknown_thing): unknown field`.
- **Check a playbook** with `mmx validate set_up_web_servers`. It needs no
  server and prints `Valid:` and the project's name.

### command_line_vars

`command_line_vars` declares the variables that a command sets with
`--var NAME=VALUE`. Node tasks read them as `.vars.NAME`, with the same
value on every node.

```yaml
command_line_vars:
  release:                  # a name you choose
    type: string            # no default: each run must give --var release=...
  run_migrations:
    type: boolean
    default: true
```

```sh
mmx run deploy --var release=1.4.3 --var run_migrations=false
```

| Field | Value | Default |
| --- | --- | --- |
| `type` | `string`, `integer`, or `boolean`; required | None. |
| `default` | A value of the type, written out, not a reference | None: `mmx explain`, `mmx dryrun`, and `mmx run` then need `--var` for the variable. |

- A name starts with a letter or an underscore and holds letters, digits,
  underscores, and hyphens.
- The name must not also be in any `vars`: of the project, the inventory,
  a group, or a node. The error is
  `command-line variable release must not also be set in vars`.
- A reference as `default` fails with
  `command-line variable release default must be a literal string`.
- [Command-line variables](../how-to/use-variables-and-templates.md#command-line-variables)
  lists how `--var` reads each type, which commands take `--var`, and why
  MMX refuses an empty value for a variable without a default.

## Fleet stage fields

A fleet stage is one entry in `fleet_stages`. The controller machine, the
computer where you run `mmx`, selects the fleet stage's nodes and starts
each node's work. A fleet stage needs `id`; `nodes` or a loop; and
`node_tasks`, `outputs`, or both. The other fields are optional.

| Field | Value | Default | What it does |
| --- | --- | --- | --- |
| `id` | A name; required | None | Names the fleet stage in output, results, `--only-fleet-stages`, `requires`, and `.outputs.<id>`. Starts with a letter or an underscore; letters, digits, underscores, and hyphens. Unique in the playbook. |
| `nodes` | A node or group name, a list of names, `all`, or a reference | None | [The nodes the fleet stage selects](#nodes-and-except_nodes). |
| `except_nodes` | Like `nodes` | None | [Nodes left out of the selection](#nodes-and-except_nodes). |
| `skip_when_no_nodes_are_selected` | `true` or `false` | `false` | [Leaves out a fleet stage that selects no node](#skip_when_no_nodes_are_selected), instead of refusing the playbook. |
| `when` | A condition | None: every selected node runs | [Checked for each node, just before the node starts](#when-on-a-fleet-stage). |
| `node_tasks` | A list of node tasks | None | [The work on each node](#node-task-fields). A fleet stage needs `node_tasks`, `outputs`, or both. |
| `outputs` | A mapping of names to values | None | [Values that later fleet stages read](#outputs). |
| `save_on_controller_machine` | A list of files | None | [Writes outputs to files on the controller machine](#save_on_controller_machine). |
| `max_nodes_at_once` | A count, such as `5`, or a percentage, such as `"50%"` | The project's `max_nodes_at_once`, or `100` | [How many nodes work at once](#rollout-fields). |
| `wait_for_all_in_batch` | `true` or `false` | `false` | [Finishes each group of nodes before the next group starts](#rollout-fields). |
| `on_node_failure` | `continue`, `stop_fleet_stage`, or `stop_run` | `continue` | [What a failed node stops](#rollout-fields). |
| `failed_nodes_allowed` | A count or a percentage | `0` | [Failures allowed before `on_node_failure` stops work](#rollout-fields). |
| `requires` | `same_node` and `all_nodes` lists of fleet stage IDs | None | [Earlier fleet stages that must have succeeded](#requires). |
| `loop`, `as`, `key`, `fleet_stages` | See the entry | None | [Repeats a list of fleet stages for each item](#loop-over-fleet-stages). |
| `for_each_node`, `as`, `fleet_stages` | See the entry | None | [Repeats a list of fleet stages for each node](#for_each_node). |

### nodes and except_nodes

`nodes` selects the nodes that run the fleet stage. `except_nodes` removes
nodes from that selection.

```yaml
fleet_stages:
  - id: turn_on_security_updates
    nodes: all                      # every node in the inventory
    except_nodes: load_balancers    # but not these
    node_tasks:
      - role: security_updates
```

- A value is one name or a list of names. A name is a node, a group, or
  `all`, which selects every node of the inventory that the command uses.
  A list selects each node that any of its names selects.
- A name that is not in the inventory fails with
  `no node or inventory group named "metrics"`.
- An empty selection fails with `node selection is empty`, and an
  `except_nodes` that removes every node fails with
  `except_nodes leaves no node of nodes`. The reason: a group that someone
  emptied by mistake does not make its work disappear without a word. Set
  [skip_when_no_nodes_are_selected](#skip_when_no_nodes_are_selected) for
  a group that may be empty on purpose. An empty group in `except_nodes`
  removes nothing and is no error.
- A value can be a reference, such as `nodes: "{{ .server }}"` inside a
  [for_each_node](#for_each_node) loop, or a reference to an earlier fleet
  stage's output. A selection that reads an output is known only during
  the run.
- `mmx nodes set_up_web_servers` lists each fleet stage's selection before
  any connection. `--limit` narrows the selection further:
  [Choose which servers a run touches](../how-to/choose-which-servers-a-run-touches.md).

### skip_when_no_nodes_are_selected

`skip_when_no_nodes_are_selected: true` leaves a fleet stage out of the run
when its selection is empty, instead of refusing the whole playbook.

```yaml
  - id: configure_staging_server
    nodes: staging                          # a group that may have no node
    skip_when_no_nodes_are_selected: true
    node_tasks:
      - role: shop_website
        inputs:
          heading: Staging
```

- MMX looks at `nodes` minus `except_nodes`, before `--limit`. A fleet
  stage that selects nodes ignores the setting. A name that is not in the
  inventory is still an error.
- `mmx nodes` and the run then say
  `→ none; selection contains no nodes (skip_when_no_nodes_are_selected)`.
- A left-out fleet stage's `.outputs.<id>` is an empty mapping, and
  `requires: {all_nodes: [<id>]}` on it is met.
- A `for_each_node` loop takes the setting. A `loop` over items that are
  not nodes does not: put the setting on the fleet stages inside the loop.
- [Keep a fleet stage for a group that may be empty](../how-to/choose-which-servers-a-run-touches.md#keep-a-fleet-stage-for-a-group-that-may-be-empty)
  shows a run, the secrets that such a run does not need, and the JSON
  field `left_out_because_it_selects_no_nodes`.

### when on a fleet stage

A `when` on a fleet stage is checked by `mmx` on the controller machine for
each selected node, just before that node would start the fleet stage. A
node whose condition is false is recorded `skipped`.

```yaml
  - id: install_sensors
    nodes: all
    when:
      equals: ["{{ .node.virtualization.type }}", none]   # physical servers only
    node_tasks:
      - apt_package:
          packages:
            - {name: lm-sensors, version: newest_when_first_installed}
```

- **What the condition reads:** the node's own `.vars`, `.inventory`, the
  node's name as `.node.name`, a loop's `as` variable, and the outputs and
  outcomes of earlier fleet stages, such as
  `not: "{{ .outputs.find_leader[.node.name].is_leader }}"`. On a
  `for_each_node` loop, `.node` is the loop's node. The condition cannot
  read node task results (`.node_tasks`), because no node task has run.
- **A condition that reads a fact,** any `.node` field except `.node.name`,
  makes `mmx` ask that node for its facts just before it decides, over the
  usual connection. A node that cannot be reached, or does not answer,
  fails as its work would have, and the fleet stage's `on_node_failure`
  applies. A condition that reads no fact contacts no node.
- **A skipped node** starts no work, takes no place in
  `max_nodes_at_once`, and is not a failure. Its result keeps the
  condition as the reason, and the facts that the decision used. It counts
  as succeeded for the [record of the last success](#record-of-each-playbooks-last-success-on-a-node),
  and it is left out of the fleet stage's whole output collection.
  `requires: {all_nodes: [...]}` on the fleet stage is not met by a
  skipped node, because a requirement says that the work happened.
- **`mmx nodes`** marks the selection as
  ``narrowed during the run by `when` ``.
- **`mmx resume`** does not check a `when` again for a node that the run
  already let start. A new `mmx run` checks every `when` again.
- On a `loop` over fleet stages, `when` is checked for each iteration. A
  skipped iteration is left out of the loop's output collection.
- The forms of a condition, such as `equals`, `all`, and `not`, are listed
  in [Conditions](shared-module-behavior.md#conditions).
- [Decide for each server during the run](../how-to/choose-which-servers-a-run-touches.md#decide-for-each-server-during-the-run)
  shows a run with skipped nodes.

### outputs

`outputs` names values that a fleet stage publishes for each of its nodes.
A later fleet stage reads them as `.outputs.<fleet stage>.<node>.<name>`.

```yaml
  - id: prepare_database
    nodes: db01
    node_tasks:
      - id: read_database_name
        read_file:
          path: /etc/shop/database_name
          content_is_secret: false
    outputs:
      database_name: "{{ .node_tasks.read_database_name.content }}"
      address: "{{ .node.network.default_ipv4_address }}"
```

- An output name starts with a letter or an underscore. A value is any
  value, with references; a value that is one condition, such as
  `equals: [...]`, becomes `true` or `false`.
- MMX computes a node's outputs after the fleet stage's node tasks
  succeeded on that node. A fleet stage with `outputs` and no
  `node_tasks` publishes values only.
- A loop's `outputs` are published for each iteration, under the
  iteration's `key`.
- On the node that reads an output, text from the output with a line
  break or another control character before its end is refused inside
  other text, in a value or a template, so it cannot add lines to a file
  or a setting there. The whole value as the whole input, such as a
  certificate as a file's whole `content`, is fine.
- [How a later fleet stage reads an output](../how-to/use-a-result-from-one-server-on-another.md#how-a-later-fleet-stage-reads-an-output)
  lists the references and what a failed, skipped, or left-out producer
  does to a reader.

### save_on_controller_machine

`save_on_controller_machine` writes the fleet stage's outputs to files on
the controller machine, after the fleet stage's nodes finish.

```yaml
    save_on_controller_machine:
      - output: address                            # one file for each node
        path: "server_details/{{ .node.name }}.txt"
        permissions_mode: "0644"
```

| Field | Value |
| --- | --- |
| `output` | An output name of the fleet stage: one file for each node. |
| `template` | A template file, rendered once from the whole output collection: one file. |
| `path` | The file to write. A relative path starts at the project directory and must stay inside it. An absolute path may name any place on the controller machine. An output, or the item of a loop over outputs, fills part of one file name in the path: it cannot contain `/`, and the path cannot then have a `.` or `..` part. |
| `permissions_mode` | The file's mode, such as `"0644"`; required. |
| `secret_readable_by_group`, `dangerously_let_every_account_read_secret` | For an output that holds a secret: see [Secrets in files, output, and history](secrets.md#secrets-in-files-output-and-history). |

- Set exactly one of `output` and `template`.
- A `loop` over fleet stages takes no `save_on_controller_machine`; put it
  on a fleet stage inside the loop.
- [Save outputs to files on your controller machine](../how-to/use-a-result-from-one-server-on-another.md#save-outputs-to-files-on-your-controller-machine)
  shows both kinds and the files they write.

### Rollout fields

These fields decide how many nodes work at once and what a failed node
stops. [Roll out a change gradually](../how-to/roll-out-a-change-gradually.md)
shows each one in a run.

```yaml
  - id: set_up_nginx
    nodes: web_servers
    max_nodes_at_once: "50%"      # half of the selected nodes at a time
    wait_for_all_in_batch: true   # finish each half before the next starts
    on_node_failure: stop_run     # a failed node stops the run
    failed_nodes_allowed: 1       # after a second failed node
    node_tasks:
      - role: shop_website
        inputs:
          heading: Shop
```

| Field | Value | What it does |
| --- | --- | --- |
| `max_nodes_at_once` | A positive count, such as `5`, or a whole-number percentage from `1%` to `100%`, such as `"50%"` | How many of the fleet stage's nodes work at once. A percentage counts the nodes selected after `--limit`, rounds down, and is never less than one node. Without it, the project's `max_nodes_at_once` in `mmx_project_settings.yml` applies, or 100 nodes when that is unset. The project setting takes a count only. |
| `wait_for_all_in_batch` | `true` or `false`; default `false` | `false` starts a node as soon as another finishes. `true` finishes each group of `max_nodes_at_once` nodes before any node of the next group starts. |
| `on_node_failure` | `continue` (the default), `stop_fleet_stage`, or `stop_run` | `continue`: the other nodes go on. `stop_fleet_stage`: no further node starts this fleet stage. `stop_run`: no further node starts this fleet stage, and no later fleet stage starts. |
| `failed_nodes_allowed` | A count, such as `1`, or a whole-number percentage up to `100%`, such as `"25%"` | The failures that the fleet stage tolerates before `on_node_failure` stops work. A percentage counts the nodes selected after `--limit` and rounds down. Needs `on_node_failure: stop_fleet_stage` or `stop_run`. |

- Under every policy, a failed node runs none of its later fleet stages,
  and nodes that already work finish.
- With `on_node_failure: continue`, `failed_nodes_allowed` fails with
  `failed_nodes_allowed requires on_node_failure: stop_fleet_stage or on_node_failure: stop_run; on_node_failure: continue does not stop after node failures`.
- Inside a [for_each_node](#for_each_node) loop, write `on_node_failure`
  and `failed_nodes_allowed` on the loop, which counts failures by the
  loop's nodes. On a fleet stage inside the loop they fail with
  `fleet stage inner is inside for_each_node loop outer, which counts failures by the loop's nodes; write on_node_failure on the loop`.
- [Choose what a failed server stops](../how-to/roll-out-a-change-gradually.md#choose-what-a-failed-server-stops)
  says how MMX counts failures.

### requires

`requires` names earlier fleet stages that must have succeeded before a
node starts this fleet stage.

```yaml
  - id: write_price_list
    nodes: web_servers
    requires:
      same_node: [back_up_price_list]   # succeeded on the node about to start
      all_nodes: [prepare_database]     # succeeded on every node it selected
    node_tasks:
      - file:
          path: /srv/shop/public/prices.html
          permissions_mode: "0644"
          content: "<h1>Prices</h1>\n"
```

- `same_node`: each listed fleet stage succeeded on the node that is about
  to start. `all_nodes`: each listed fleet stage succeeded on every node
  that it selected.
- Each ID must name an earlier fleet stage: `requirement X must name an
  earlier fleet stage`.
- When both fleet stages select from the inventory, a `same_node` fleet
  stage must select every node of the fleet stage that requires it.
- A node that a requirement blocks does not start, and is not counted
  against `failed_nodes_allowed`.
- A fleet stage that reads another fleet stage's outputs needs no
  `requires` for it: reading an output already waits for that node's
  success.
- [Run a fleet stage only where an earlier one succeeded](../how-to/roll-out-a-change-gradually.md#run-a-fleet-stage-only-where-an-earlier-one-succeeded)
  shows a run.

### loop over fleet stages

A fleet stage with `loop` repeats its own `fleet_stages` for each item of a
list or a mapping, one item after the other. Use it for a list of things
that are not nodes.

```yaml
  - id: add_shops
    loop: [north, south]
    as: shop                  # each item, read as .shop
    key: "{{ .shop }}"        # names each iteration in output and results
    fleet_stages:
      - id: create_shop_directory
        nodes: web_servers
        node_tasks:
          - directory:
              path: "/srv/shops/{{ .shop }}"
```

| Field | Value |
| --- | --- |
| `loop` | A list, a mapping, or one whole reference to a list or a mapping; required. A list runs in written order; a mapping in the sorted order of its keys, with `.<as>.key` and `.<as>.value`. |
| `as` | The variable that holds each item; required. Not one of `vars`, `inputs`, `node`, `inventory`, `this_node_task`, `node_tasks`, `outputs`, `fleet_stages`, `new_file`, or `secrets`, and not a name that an enclosing loop uses. |
| `key` | A stable text for each iteration; required. Results show it as `add_shops[north]/create_shop_directory`. |
| `fleet_stages` | The fleet stages that each iteration runs; required. |
| `when`, `outputs` | As on any fleet stage, for each iteration. |

A loop selects no nodes itself: `nodes`, `except_nodes`,
`skip_when_no_nodes_are_selected`, `node_tasks`, `requires`,
`save_on_controller_machine`, and the [rollout fields](#rollout-fields)
belong on the fleet stages inside the loop.

### for_each_node

A fleet stage with `for_each_node` repeats its `fleet_stages` for each
selected node, one node's whole sequence at a time. Use it when a node's
share of the work happens partly on another node, such as taking each app
server out of the load balancer while it changes.

```yaml
  - id: update_each_web_server
    for_each_node: web_servers      # selects nodes like nodes
    as: server                      # the current node's name, read as .server
    on_node_failure: stop_run
    fleet_stages:
      - id: take_out_of_balancer
        nodes: lb01                 # a helper node, working for .server
        node_tasks:
          - shell:
              script: /usr/local/bin/balancer-drain "$SERVER"
              env: {SERVER: "{{ .server }}"}
      - id: update
        nodes: "{{ .server }}"
        node_tasks:
          - role: shop_website
            inputs:
              heading: Shop
```

| Field | Value |
| --- | --- |
| `for_each_node` | Like `nodes`: the nodes to repeat for; required. Each node is one iteration, named by the node. |
| `as` | The variable that holds the current node's name; required. Same name rules as a loop's `as`. |
| `fleet_stages` | The fleet stages each iteration runs; required. They select their nodes, often `nodes: "{{ .server }}"`. |
| `except_nodes`, `skip_when_no_nodes_are_selected`, `when`, `outputs` | As on a fleet stage. `when` is checked for each loop node, with `.node` as that node. |
| `max_nodes_at_once`, `wait_for_all_in_batch`, `on_node_failure`, `failed_nodes_allowed` | As on a fleet stage, counted by the loop's nodes: `max_nodes_at_once: 2` runs two iterations at once. |

- `for_each_node` takes no `loop`, `key`, `nodes`, `node_tasks`, or
  `requires`, and it refuses `save_on_controller_machine`: put that on a
  fleet stage inside the loop.
- A node that an iteration uses for another node, such as `lb01` above,
  is a **helper node**. A `--limit` still reaches the helper node.
- `reboot` is the last node task of a fleet stage. For a reboot in the
  middle of an iteration, use two fleet stages inside the loop.
- [Take each server out of the load balancer while it changes](../how-to/roll-out-a-change-gradually.md#take-each-server-out-of-the-load-balancer-while-it-changes)
  shows a whole run, and
  [Do one server's work on another server](../how-to/use-a-result-from-one-server-on-another.md#do-one-servers-work-on-another-server)
  shows a loop that hands a value from one node to another.

## Node task fields

A node task is one entry in `node_tasks`. It does exactly one of five
things, and may add the settings in the second table.

| What the node task does | Field | Example |
| --- | --- | --- |
| Calls a module | The module's name, with the module's inputs below it | `file: {path: /etc/motd, content: "Hello\n", permissions_mode: "0644"}` |
| Calls a role | `role`, with `inputs` | `role: shop_website` (see [Role calls](#role-calls)) |
| Groups a change with what follows it | `task_block` | See [task_block fields](#task_block-fields) |
| Shows a message or a value | `show` | `show: "{{ .node.name }}"` (see [show](#show)) |
| Sets variables for later node tasks | `set_vars` | `set_vars: {large_server: true}` (see [set_vars](#set_vars)) |

A node task with two of these fails with `a node task must specify exactly
one action: a module call, role, task_block, show, or set_vars`.

| Setting | Value | Default | What it does |
| --- | --- | --- | --- |
| `id` | A name | None | [Names the node task and its result](#id-and-the-result-of-a-node-task). |
| `when` | A condition | None: the node task runs | [Skips the node task when false](#when-on-a-node-task). |
| `loop` | A list, a mapping, or one whole reference | None | [Runs the node task once for each item](#loop-as-and-key). |
| `as` | A name | None; required with `loop` | The variable that holds each item. |
| `key` | Text, usually a reference to the item | The item's index or mapping key | Names each iteration. Required for some loops. |
| `retry` | `true`, `false`, or a mapping | Depends on the module | [Tries the node task again after a failure](#retry). |
| `time_limit` | A duration or `null` | `node_task_defaults.time_limit`, else none | [Time for the whole node task, with every try](#time_limit-and-time_limit_per_try). |
| `time_limit_per_try` | A duration or `null` | `node_task_defaults.time_limit_per_try`, else none | [Time for one try](#time_limit-and-time_limit_per_try). |

`mmx describe-module task` prints the settings table, and
`mmx describe-module task_block`, `role`, `show`, and `set_vars` print the
other forms.

### id and the result of a node task

`id` names a node task. Output and saved results use the name, and later
node tasks of the same fleet stage on the same node read the node task's
result as `.node_tasks.<id>`. MMX has no separate field for saving a
result.

```yaml
      - id: installed_release
        shell:
          script: printf %s "$(/usr/local/bin/shop version)"
          changed_when: false

      - when:
          not:
            equals: ["{{ .node_tasks.installed_release.stdout }}", "{{ .vars.release }}"]
        show: "Upgrade needed on {{ .node.name }}"
```

- An ID starts with a letter or an underscore and holds letters, digits,
  underscores, and hyphens. It is unique in its fleet stage, together with
  the node tasks in its node task blocks; a role file has its own IDs.
  The error is `invalid or duplicate node task id "install_nginx"`.
- A node task without `id` is named in output by its block's `id`, its
  role's name, or else `task_` and its position, such as `task_3`. Its
  result has no reference.
- A reference reaches earlier node tasks only, in the same fleet stage or
  in the same role call. A later fleet stage reads values through
  [outputs](#outputs), and a role's caller through the role's declared
  outputs.

A result has these fields, where they apply. A module adds its own, which
its module page lists under "Outputs", such as `content` of `read_file`:

| Field | What it holds |
| --- | --- |
| `status` | `succeeded`, `failed`, `skipped`, `not_checked` (dryrun only), `blocked`, or `stopped`. [What the results mean](../how-to/preview-and-run-changes.md#what-the-results-mean) explains each. |
| `change` | `changed`, `unchanged`, `would_change` (dryrun only), or `unknown`. |
| `reason` | Why the node task failed or was skipped. |
| `exit_code`, `stdout`, `stdout_json`, `stderr` | A command's result, such as of `shell`. `stdout_json` is `stdout` read as JSON, field by field. |
| `http_status_code`, `response_body`, `response_body_json` | An HTTP response, of `http` or `wait_for`. |
| `value` | A module's value, such as `show`'s value. |
| `outputs` | A role call's declared outputs. |
| `items` | A looped node task's results, one for each iteration in order, each with its `iteration_key`. |

**What a failed node task keeps.** A failed `shell`, `http`, or `wait_for`
node task, and a failed module that is not core, keep what they captured:
`exit_code`, `stdout`, `stdout_json`, `stderr`, `http_status_code`,
`response_body`, and `response_body_json`, so an `on_failure` node task can
read them. On every other node task that did not succeed, such as a
skipped one, reading these fields, or `value`, `outputs`, `items`,
`content`, or `public_key`, fails and names the node task's status; its
`status`, `change`, and `reason` stay readable. A looped node task that
failed keeps no captured output. A node task that did
not run has no result, and a reference to it fails.

### when on a node task

`when` skips the node task when the condition is false. The result then
says `skipped, unchanged — when is false`.

```yaml
      - when:
          equals: ["{{ .node.package_manager }}", apt]
        apt_package:
          packages:
            - {name: nginx, version: newest_when_first_installed}
```

- A condition is a boolean, one whole reference to a boolean, such as
  `"{{ .vars.large_server }}"`, or a condition form: `equals`, `contains`,
  `matches`, `number_equals`, `number_less_than`, `number_at_most`,
  `number_greater_than`, `number_at_least`, `version_equals`,
  `version_less_than`, `version_at_most`, `version_greater_than`,
  `version_at_least`, `path_exists`, `all`, `any`, and `not`.
  [Conditions](shared-module-behavior.md#conditions) says what each form compares.
- `path_exists` works only in the `when` of a node task or a node task
  block, because it looks at the node.
- A false `when` skips the node task without working out its inputs, so an
  input may read a value that exists only when the condition is true.
- On a looped node task, `when` is checked for each item and can read the
  item.
- On a node task block or a role call, `when` skips the whole block or
  role call.
- The `when` on a fleet stage is checked on the controller machine instead:
  see [when on a fleet stage](#when-on-a-fleet-stage).

### loop, as, and key

`loop` runs a node task, a role call, or a node task block once for each
item, in order, on the same node.

```yaml
      - id: create_shop_accounts
        loop: [alice, bob]
        as: person                    # each item, read as .person
        user:
          name: "{{ .person }}"
          create_home_directory: true
```

- `loop` takes a list, a mapping, or one whole reference to a list or a
  mapping, such as `"{{ .vars.accounts }}"`. A list runs in written order.
  A mapping runs in the sorted order of its keys, and `.<as>.key` and
  `.<as>.value` read each entry.
- `as` is required, with the same name rules as on a
  [loop over fleet stages](#loop-over-fleet-stages).
- `key` names each iteration in output and results, such as
  `create_shop_accounts[alice]`. Without `key`, the name is the item's
  index as text (`0`, `1`, ...) for a list, or the mapping's key.
- `key` is required on a looped node task block, and on a loop whose
  iterations run a `shell` node task with
  `only_when_tracked_inputs_changed: true`, because the node keeps a
  record under each iteration's key. Duplicate keys are an error. A
  looped node task block without `key` fails with `fleet stage loops and
  node task block loops require key to identify each iteration`; a
  looped `shell` node task without `key` fails with `a loop whose
  iterations run a shell command with only_when_tracked_inputs_changed
  requires key: the node keeps each iteration's command record under the
  iteration's key, ...`.
- With an `id`, `.node_tasks.<id>.items` holds the iterations' results.
- A looped `file` node task inside a node task block's `changes` gives one
  activation after all the files.
- [Loop over the results](../how-to/use-a-result-from-one-server-on-another.md#loop-over-the-results)
  shows a loop over another fleet stage's outputs.

### show

`show` prints a message or a value in the run's output, at its place in the
node task list.

```yaml
      - show: "Release {{ .vars.release }} on {{ .node.name }}"

      - show:
          release: "{{ .vars.release }}"
          home_page: "{{ .node_tasks.write_home_page }}"
```

- `show` changes nothing and runs in a dryrun too. A value that a dryrun
  cannot know yet is marked as not known, not shown as empty or false.
- MMX hides secrets in the value, also inside text and inside a list or a
  mapping.
- The result's `value` holds the value with its structure, in
  `mmx results --json` too.

### set_vars

`set_vars` gives variables a value at a point in the node task list. Later
node tasks of the same fleet stage, loop iteration, or role call read them
as `.vars.NAME`.

```yaml
      - set_vars:
          large_server:
            number_at_least: ["{{ .node.memory.total_mib }}", 4096]
      - when: "{{ .vars.large_server }}"
        show: "{{ .node.name }} has at least 4096 MiB of memory"
```

- A value that is one condition, such as `number_at_least`, becomes `true`
  or `false`.
- `set_vars` works out every value from the state before it, then sets
  them together. If one value fails, it sets none.
- `set_vars` may replace a variable from `vars`. Facts, role inputs, and
  results stay as they are.
- A duration setting cannot read a variable that a `set_vars` of the same
  fleet stage sets: see [A duration from a var](#a-duration-from-a-var).
- [Values computed during the run](../how-to/use-variables-and-templates.md#values-computed-during-the-run)
  shows a run.

## Node task retries and time limits

A retry repeats one node task on its node, after a failed try. It differs
from `mmx resume`, which repeats a saved run's unfinished work. A time limit
ends a node task that takes too long.

```yaml
      - id: wait_for_job
        shell:
          script: /usr/local/bin/job-status
          changed_when: false
        time_limit: 2m                # the whole node task, every try included
        time_limit_per_try: 5s        # one try
        retry:
          max_tries: 30
          pause_between_tries: 2s
          until:                      # a successful try must also show this
            contains: ["{{ .this_node_task.stdout }}", "job_status=complete"]
          retry_when:                 # try again only after these exit codes
            any:
              - equals: ["{{ .this_node_task.exit_code }}", 0]
              - equals: ["{{ .this_node_task.exit_code }}", 75]
```

Here exit code 0 with `job_status=complete` in the output succeeds. Exit
code 0 without it, or exit code 75, tries again after two seconds. Any other
exit code fails at once, as do 30 tries or two minutes without success.

The project's defaults are in `mmx_project_settings.yml`, under
[node_task_defaults](project-settings.md#node_task_defaults):

```yaml
# mmx_project_settings.yml
project_name: mmx_example
default_inventory: example
node_task_defaults:
  time_limit: 5m
  time_limit_per_try: 1m
  retry:
    max_tries: 3
    pause_between_tries: 2s
```

A setting on a node task replaces the default for that node task.
`mmx explain` shows each node task's settings and where each came from,
such as `retry.max_tries = 3 from MMX default`.

### retry

`retry` sets whether a node task that fails tries again, how often, and
how long it waits between tries.

```yaml
        retry:
          max_tries: 5
          pause_between_tries: 10s
```

| Value | What it does |
| --- | --- |
| `retry: true` | Retries with the defaults: `node_task_defaults.retry`, or else 3 tries in all with a 2-second pause. |
| `retry: false` | Tries once. |
| `retry: {...}` | Retries with the fields below; a field left out takes its default. |

| Field | Value | Default |
| --- | --- | --- |
| `max_tries` | A positive whole number, counting the first try, or `unlimited`. `unlimited` needs a `time_limit`, on the node task or in `node_task_defaults`. `null` is refused. | `3` |
| `pause_between_tries` | A duration of 0 or more, such as `2s` | `2s` |
| `until` | A condition on `.this_node_task` that a successful try must also meet. | Always met |
| `retry_when` | A condition on `.this_node_task`: another try is allowed only when true. | Always true |

- **Which node tasks retry without `retry`.** Most core modules, such as
  `apt_package` and `file`, and every module that is not core retry by
  default. `shell`, `http`, `read_file`,
  `systemd_service`, `certbot_certificate`, `show`, and `set_vars` try once
  unless the node task sets `retry`. Each module page says which under
  "Retries and time limits".
- **A failure that another try cannot fix is not repeated:** invalid
  configuration, a rejected validation, and a missing prerequisite, such as
  a package, program, account, file, or parent directory that an earlier
  node task or a person must provide. The result's `failure_kind` then
  says `config`, `validation`, or `missing_prerequisite`, and the reason
  names what is missing, such as `the parent directory /var/backups does
  not exist; create it with an earlier directory node task`.
  The missing parent directory of a path that `file`, `download`,
  `directory`, `symlink`, `unpack_archive`, or `git` writes counts as a
  missing prerequisite, unless a `directory` node task sets
  `create_missing_parent_directories: true`. So does a
  `run_from_directory` that does not exist, for `shell` and for the script
  of `wait_for`: no command can start there (`run_from_directory
  /opt/openwisp2 does not exist; create it with an earlier directory node
  task`). The reason is in MMX's own words, so a node task that reads a
  secret shows it too.
- **`until` cannot turn a failed try into success,** and `retry_when` only
  allows another try. A condition that cannot be worked out fails the node
  task at once.
- **A role call or a node task block takes no `retry`:** `retry must be set
  on an individual node task inside the role or node task block`.
- The saved results keep the first and the latest nine tries, and count
  the rest.

### time_limit and time_limit_per_try

`time_limit` ends a node task that runs too long in all its tries
together; `time_limit_per_try` ends one try that runs too long. Both are
written on the node task, beside its module:

```yaml
      - id: install_nginx
        apt_package:
          packages:
            - {name: nginx, version: newest_when_first_installed}
        time_limit: 10m               # the whole node task, every try included
        time_limit_per_try: 2m        # one try
```

| Field | Value | Default |
| --- | --- | --- |
| `time_limit` | A positive duration, such as `30s`, `5m`, or `1h30m`, or `null` | `node_task_defaults.time_limit`, else no limit |
| `time_limit_per_try` | A positive duration, or `null` | `node_task_defaults.time_limit_per_try`, else no limit |

- `time_limit` counts from the node task's start, with every try and
  pause. `time_limit_per_try` bounds each try.
- `null` on a node task removes the project's default for that node task.
- When `time_limit` ends, the node task fails with a reason that starts
  with `time_limit exceeded:`, followed by the module's own reason, such as
  what it waited for, or else `context deadline exceeded`. When the limit
  ends during a pause between tries, the reason is `time_limit exceeded
  during retry.pause_between_tries`.
- When `time_limit_per_try` ends a try, the try fails with a reason that
  starts with `time_limit_per_try exceeded:`, followed in the same way by
  the module's own reason. The next try follows as `retry` says.
- A duration is a number with a unit, such as `500ms`, `90s`, `15m`, or
  `1h30m`.

### Modules that take no retry or time limit

These modules wait with an input of their own, and refuse `retry`,
`time_limit`, and `time_limit_per_try`. They also ignore
`node_task_defaults`.

| Module | Its own limit |
| --- | --- |
| `wait_for`, `reboot` | `wait_at_most` |
| `netplan_file`, `systemd_networkd_file`, `ifupdown_interfaces_file`, `networkmanager_connection_file`, `wireguard_interface`, `firewalld_zone_membership` | `restore_unless_controller_confirms_within`: how long the node waits for the controller machine to reach it again after the change, before the node puts the old configuration back |

The error names the input, such as `wireguard_interface does not accept
retry, time_limit, or time_limit_per_try;
restore_unless_controller_confirms_within bounds the change`.
[Change SSH or network settings safely](../how-to/change-ssh-or-network-settings-safely.md)
explains the network modules' restore.

`assert` checks once and refuses `retry`, with `assert evaluates once and
rejects retry; retry the task that observes the value`. Its time limits
apply as for any node task.

### A duration from a var

A duration setting can read a variable, so that a team sets the duration
in one place and different nodes can wait different times. Write the
duration as one whole reference to the variable.

```yaml
# mmx_project_settings.yml
project_name: mmx_example
default_inventory: example
vars:
  reboot_wait_at_most: 2m
```

```yaml
# inventories/example.yml, part of it
nodes:
  db01:
    address: 203.0.113.21
    vars:
      reboot_wait_at_most: 15m   # a dedicated server that checks its hardware first
```

```yaml
# playbooks/restart.yml
fleet_stages:
  - id: restart
    nodes: all
    node_tasks:
      - reboot:
          wait_at_most: "{{ .vars.reboot_wait_at_most }}"
```

These settings take such a reference:

- `time_limit`, `time_limit_per_try`, and `retry.pause_between_tries`;
- `wait_at_most` of `reboot` and `wait_for`;
- `random_delay` and `stop_a_run_after` of `systemd_scheduled_job`;
- `restore_unless_controller_confirms_within` of `netplan_file`,
  `systemd_networkd_file`, `ifupdown_interfaces_file`,
  `networkmanager_connection_file`, and `wireguard_interface`.

Rules:

- MMX takes each node's value before the run, and checks it as that
  setting's duration. `mmx validate`, `mmx explain`, and the start of a
  run report a bad value with the node, the setting, and where the
  variable is set, here for a project value that `web01` uses:
  `fleet stage restart, node web01: ... reboot.wait_at_most must be a
  positive duration; value supplied at mmx_project_settings.yml:4:24
  (vars.reboot_wait_at_most)`. `db01` has its own value, so a bad
  project value is never reported for `db01`.
- The variable must be one that MMX knows before the run: from the vars of
  the project, the inventory, a group, or a node, or from `--var`. MMX
  refuses a reference to an output, a fact, or a secret, and text around
  the reference, such as `"{{ .vars.minutes }}m"`:
  `time_limit must be a duration, such as 15m, or one whole reference to a var, such as "{{ .vars.reboot_wait_at_most }}"; MMX takes the duration before the run, when outputs, facts, and secrets are not known yet`.
- MMX refuses a variable that a `set_vars` node task of the same fleet
  stage sets, and a reference inside a role, whose variables are only
  those its own `set_vars` sets during the run. In a role, pass the
  duration through an input.
- Inside a `for_each_node` loop whose nodes are known before the run, a
  fleet stage that selects the loop's node, such as
  `nodes: "{{ .server }}"`, is checked for each of the loop's nodes.
- A duration input that a module reads while its node task runs, such as
  `older_than` of `delete_old_files`, takes any reference, as every other
  input does.

Module pages that rely on this section: [`systemd_service`](modules/systemd_service.md), [`shell`](modules/shell.md), [`http`](modules/http.md), [`docker_compose_project`](modules/docker_compose_project.md), [`mysql_database`](modules/mysql_database.md).

## task_block fields

A `task_block` groups a change with the node tasks that put it into use and
check it, in phases that run in a fixed order on the node.

```yaml
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
          validate:                 # check the result before it is used
            - id: check_nginx_configuration
              shell:
                script: nginx -t
          activate:                 # put the change into use
            - id: reload_nginx
              systemd_service:
                name: nginx
                state: running
                refresh: reload     # reload only when a file in changes changed
          verify:                   # check that it works
            - id: check_home_page
              wait_for:
                url_responds: http://127.0.0.1:8080/
```

| Field | Value | What it holds and when it runs |
| --- | --- | --- |
| `id` | A name; required | Names the block in output, such as `shop_site/changes/write_site_file`, and in the node's records. |
| `checks_before_restart_or_reload` | A list of node tasks | Checks that change nothing: `assert`, `http` with `GET` or `HEAD`, `wait_for`, `show`, and `shell` with `changed_when: false`. Runs before the block's first write when MMX can foresee the restart or reload, or else just before the restart, reload, or command; and only when the block would restart or reload a running service, or run a `shell` node task with `only_when_tracked_inputs_changed: true`. |
| `changes` | A list of node tasks; required, not empty | The change: files, packages, Git checkouts, and other node tasks. These are the block's **tracked inputs**. |
| `validate` | A list of `shell` or `http` node tasks | Checks the result of `changes` before `activate`, such as `nginx -t`. |
| `activate` | A list of node tasks | Puts the change into use. |
| `verify` | A list of node tasks | Checks that the result works. |
| `on_failure` | A list of node tasks | Runs only when an earlier phase of the block failed. |
| `cleanup` | A list of node tasks | Runs last, whether the block succeeded or failed. |

- The phases run in the order of the table. A failed node task stops the
  block's remaining normal phases; `on_failure` and then `cleanup` run.
- `when`, `loop`, `as`, `key`, and an outer `id` go beside `task_block`,
  not inside it. An outer `id` names the block's result as
  `.node_tasks.<id>`, and replaces the block's `id` in the result path.
  The block's own `id` gives no `.node_tasks` reference.
- A block takes no `retry`. Its node tasks may each set one.
- MMX has no notify field. A `systemd_service` node task with
  `state: running` in `activate` restarts the service when a tracked
  input changed since the service last started or reloaded, or reloads it
  with `refresh: reload`; a changed package or unit file always restarts
  it. This also happens when an earlier run wrote the file and stopped
  before the reload. A `shell` node task in `activate` with
  `only_when_tracked_inputs_changed: true` runs only then; it needs an
  `id`, and works only in `activate`. Any other `shell` node task in
  `activate` runs on every run.
- `checks_before_restart_or_reload` needs a `systemd_service` node task or
  a `shell` node task with `only_when_tracked_inputs_changed` in
  `activate`. Otherwise it fails with `checks_before_restart_or_reload
  would never run`.
- A dryrun runs `checks_before_restart_or_reload` when a restart or
  reload would be needed, never runs `on_failure`, and previews the other
  phases by each node task's own rule.
- [Change a configuration and reload its service](../how-to/change-a-configuration-and-reload-its-service.md)
  walks through a block, and its section
  [The phases in order](../how-to/change-a-configuration-and-reload-its-service.md#the-phases-in-order)
  says what a dryrun does in each phase.
  [Reload or restart](../how-to/change-a-configuration-and-reload-its-service.md#reload-or-restart)
  explains `refresh`.

### Put things back when a node task block fails

`on_failure` holds the node tasks that put things back when the block
failed. It runs only after an earlier phase of the block failed.

```yaml
          on_failure:
            - systemd_service:
                name: shop.service
                state: running      # start the old release again
```

- **The block still fails.** `on_failure` does not turn the failure into
  success, unlike Ansible's `rescue`. The block fails with its first
  error, and the node does no more work in this fleet stage or in later
  fleet stages. When a node task of `on_failure` also fails, the result
  names both errors.
- **What `on_failure` reads:** `.vars`, `.inputs` in a role, and the
  results of earlier node tasks as `.node_tasks.<id>`, the failed node
  task's included. A failed node task keeps its `status` and `reason`,
  and a failed `shell`, `http`, or `wait_for` node task, or a failed
  module that is not core, keeps its captured output: see
  [What a failed node task keeps](#id-and-the-result-of-a-node-task).
  A node task that did not run has no result.
- **`cleanup`** runs after `on_failure`, and after a block that succeeded.
  `cleanup` belongs to the current run: after a run that stopped halfway,
  the next run does not repeat the old run's `cleanup`.
- **A failed check in `checks_before_restart_or_reload`** stops the block
  before the restart or reload; `on_failure` and `cleanup` run as for any
  failed block. When the check ran before the first write, nothing in the
  block was written.
- A node task block does not undo `changes` by itself. `on_failure` does
  what you write in it.
- [Back up, upgrade, and restore on failure](../how-to/change-a-configuration-and-reload-its-service.md#put-things-back-when-a-node-task-block-fails)
  shows a whole upgrade with `on_failure`.

Module pages that rely on this section: [`assert`](modules/assert.md), [`http`](modules/http.md), [`shell`](modules/shell.md), [`systemd_service`](modules/systemd_service.md).

## Role calls

A role call runs a role: a reusable list of node tasks with declared inputs,
in its own file `roles/<name>/main.yml`.

```yaml
      - id: shop_website              # names the role call's result
        role: shop_website            # runs roles/shop_website/main.yml
        inputs:
          heading: Shop
          port: "{{ .vars.http_port }}"
      - show: "{{ .node_tasks.shop_website.outputs.home_page_url }}"
```

| Field of a role call | Value |
| --- | --- |
| `role` | The role's directory name under `roles/`; required. |
| `inputs` | A mapping of the role's input names to values; values may be references. A node task that calls a module takes no `inputs`: `inputs belongs to a role call; a module's inputs are written below the module's name`. |
| `id`, `when`, `loop`, `as`, `key` | As on any node task. A role call takes no `retry`. |

The role file has three fields:

| Field of the role file | Value |
| --- | --- |
| `node_tasks` | The role's node tasks; required, not empty. |
| `inputs` | Each input's `type` (`string`, `integer`, `boolean`, `list`, or `mapping`), and `required: true`, a `default` written out, or `secret: true`. |
| `outputs` | Values the caller reads as `.node_tasks.<role call id>.outputs.<name>`. |

- A role reads its inputs, its node's facts, and its own node tasks'
  results. Variables of the project, the inventory, and the command line
  reach a role only through `inputs`.
- A role call inside a role needs `allow_role_calls_within_roles: true` in
  `mmx_project_settings.yml`. A role that calls itself, directly or
  through another role, fails with `recursive role call: shop_website`.
- [Reuse work with roles](../how-to/reuse-work-with-roles.md) explains
  inputs, files inside a role, and outputs, with their errors.

## One-time playbooks

A **one-time playbook** holds work that must happen once on every node it
selects, such as removing a program that the team no longer uses. It runs
only when a command names it. Its fields are the fields of any playbook,
with these differences:

```yaml
# one_time_playbooks/2026-11-15_remove_old_ruby_manager.yml
fleet_stages:
  - id: remove_old_ruby_manager
    nodes: all
    node_tasks:
      - directory:
          path: /opt/rvm
          state: absent
```

- **The file** is `one_time_playbooks/YYYY-MM-DD_what_it_does.yml`. The
  name without `.yml` is the playbook's name:
  `mmx run 2026-11-15_remove_old_ruby_manager`.
- **Each entry in `command_line_vars` needs a `default`.**
- **A completed one-time playbook** is in `one_time_playbooks/completed/`.
  A command that names a completed playbook fails, and says to move the
  file back to make it runnable.
- `one_time_playbooks/` holds only one-time playbooks, `completed/`, and
  files whose names start with a dot.
- [Rules for one-time playbooks](../how-to/make-a-one-time-change.md#rules-for-one-time-playbooks)
  owns the rules for the name, the directory, and the defaults, and
  [Make a one-time change](../how-to/make-a-one-time-change.md) shows how
  to write, check, run, and complete one.

## Record of each playbook's last success on a node

Each node keeps one file for each playbook that has succeeded on it:

```text
/var/lib/mmx/last_successful_run/<project_name>/<playbook>
```

The file holds one JSON line about the run that wrote it:

```json
{"run_id":"20261115T053005.182485400-b28f493831cb8c18","inventory":"example","limit":"","only_fleet_stages":"","finished_at":"2026-11-15T05:30:49Z"}
```

| Field | What it holds |
| --- | --- |
| `run_id` | The run's full ID, which `mmx results` takes. |
| `inventory` | The inventory name that the run used. |
| `limit` | The run's `--limit` value, such as `canary,db01`; empty without one. |
| `only_fleet_stages` | The run's `--only-fleet-stages` value; empty without one. |
| `finished_at` | When the run, or the resume that completed it, finished, in UTC. |

When a node writes the file:

- After `mmx run` or `mmx resume` of the playbook, on each node where every
  fleet stage that selected the node succeeded, whether something changed
  or not. A fleet stage that a false `when` skipped on the node counts as
  succeeded.
- A run with `--limit` or `--only-fleet-stages` writes the file too, also
  on a node that the run used only as a helper node. To count only whole
  runs, check that `limit` and `only_fleet_stages` are both empty.

When the node keeps its earlier file:

- A fleet stage's work on the node failed or stopped, or
  `/etc/mmx/do_not_manage` refused the work.
- The run stopped before MMX knew a later fleet stage's nodes. The later
  fleet stage might have selected any node, so no node writes the file.
  For example, `on_node_failure: stop_run` can end a `for_each_node` loop
  before some iterations start. The `mmx resume` that finishes the run
  writes the file.
- MMX could not reach the node to write the file after the run's work.
  The run's results show a note on that node, and the run's outcome does
  not change.
- `mmx dryrun` and `mmx run-command` never write the file.

The file belongs to root and has mode `0600`, so a check that reads it runs
as root. MMX replaces the file in one step: a reader sees the earlier line
or the new one, never part of either. Writing the file is not a change, and
never shows as `changed`.
[When a playbook last succeeded on each server](../how-to/stop-resume-and-read-a-run.md#when-a-playbook-last-succeeded-on-each-server)
reads the file on every server with one command.

## Nesting limits

Some entries may hold others, and some may not. The table lists what may
go where. For example, a role call goes in a fleet stage's `node_tasks`,
never inside a node task block's phase:

```yaml
    node_tasks:
      - role: shop_website          # allowed here
      - task_block:
          id: shop_site
          changes:
            - file: {...}           # a role call here is refused
```

| Where | What may go inside | Error otherwise |
| --- | --- | --- |
| A node task block's phases | Individual node tasks, which may loop. No role call and no other node task block. | `node task block phases accept individual node tasks; put role calls and nested node task blocks in the fleet stage's or role's node_tasks list` |
| A fleet stage's or a role's `node_tasks` | Node tasks, role calls, and node task blocks | |
| A role's `node_tasks` | A role call only with `allow_role_calls_within_roles: true`; never a call back to the same role | `role calls within roles require allow_role_calls_within_roles: true`; `recursive role call: NAME` |
| A `loop` or `for_each_node` fleet stage's `fleet_stages` | Fleet stages, other loops, and `for_each_node` loops | |
| A fleet stage inside a `for_each_node` loop | No `on_node_failure` or `failed_nodes_allowed`: write them on the loop | `fleet stage X is inside for_each_node loop Y, which counts failures by the loop's nodes` |
| `reboot` | Only as the last node task of a fleet stage's own `node_tasks`, outside roles, node task blocks, and loops | `reboot must be the last node task in a fleet stage's node_tasks list, outside roles, node task blocks, and loops` |

The limits on saved output sizes are settings of the project:
[Saved output sizes](project-settings.md#saved-output-sizes).

## References and templates

A **reference** reads a value that MMX knows, such as `{{ .vars.http_port }}`
in a node task's input. A **template** is a file in the project, such as
`files/nginx.conf.tmpl`, that MMX fills in for each node with Go's
[text/template](https://pkg.go.dev/text/template) syntax.
[Use variables and templates](../how-to/use-variables-and-templates.md)
shows both in a run.

### Where a reference reads from

A reference starts with a dot and a name that says where MMX reads the
value, such as `{{ .vars.http_port }}` for the variable `http_port`. The
names are:

| Reference | The value |
| --- | --- |
| `.vars.NAME` | A variable from `vars` of the project, the inventory, the node's groups, or the node; a command-line variable; or a `set_vars` value. [`vars` in the inventory file](inventory-file.md#vars) gives the order in which a later place replaces an earlier value. |
| `.node.NAME` | A fact that MMX measures on the node: see [Node facts](#node-facts). |
| `.inventory.nodes.NAME` | A node's `name`, `address`, and `vars` (its groups' and its own). |
| `.inventory.groups.NAME` | A group's sorted list of node names. |
| `.inputs.NAME` | A role's input, inside the role. |
| `.secrets.KEY` | A secret: see [Secrets reference](secrets.md). |
| `.node_tasks.ID` | An earlier node task's result: see [id and the result of a node task](#id-and-the-result-of-a-node-task). |
| `.this_node_task` | The current node task's own result, in `retry.until`, `retry.retry_when`, and a `shell` node task's `changed_when` and `failed_when`. |
| `.outputs.STAGE.NODE.NAME` | An earlier fleet stage's output on one node. |
| `.fleet_stages.STAGE.NODE.status` | How an earlier fleet stage ended on one node, also after a failure. |
| `.new_file.path` | The new file that `check_new_file` checks, in a `file`, `download`, `line`, or `edit_file` node task. |
| `.<as>` | The current item of a loop. |

- A missing value is an error, never empty text. The error names the
  file, the line, and the field.
- Names are case-sensitive.
- A reference inside other text, such as `listen {{ .vars.http_port }};`,
  makes text. MMX fills in a value once: a variable whose value is
  `"{{ .vars.site_name }}"` gives that text, not the site name.
- A value that a reference fills into a path or another name on a node
  has no `..` part. Another node's output may add directories below the
  directory that a path names, as in
  `/srv/sites/{{ .outputs.list_sites.web1.site }}`, but cannot climb out of
  the directory: `../../etc` fails the node task with `.outputs.list_sites.web1.site
  is "../../etc", which has a .. part`. A `..` written in the path itself
  stays, and MMX cleans the path. A reference at the start of a path lets
  its value choose the whole path.
- A value in the playbook's YAML takes references only, not function
  calls: `"{{ add .vars.http_port 1 }}"` fails with `reference must start
  with a dot`. Functions work in template files.

### A number, a boolean, a list, or a mapping from a reference

A module input that takes a number, a boolean, a list, or a mapping also
takes text that is exactly one reference. The value keeps its type.

```yaml
      - id: create_shop_account
        user:
          name: shop
          uid: "{{ .vars.shop_user_id }}"    # 1500, a number, from vars
          create_home_directory: false
```

- The value must have the input's type and pass the module's own rules,
  such as `account ID must be an integer from 0 through 4294967294;
  value supplied at mmx_project_settings.yml:5:17 (vars.shop_user_id)`.
- A reference inside other text is text:
  `uid: "{{ .vars.shop_user_id }}0"` fails with `user.uid must be an
  integer or one whole reference`.
- A value that holds a secret is refused in such an input.
- An input that names files on the controller machine, such as
  `authorized_keys.key_files_on_controller_machine`, takes no whole-value
  reference. Write the list, with a reference for each item.
- [A number, a boolean, a list, or a mapping from a reference](../how-to/use-variables-and-templates.md#a-number-a-boolean-a-list-or-a-mapping-from-a-reference)
  says when MMX checks each kind of value.

### Double braces in scripts and in other text

MMX treats `{{` in two ways, depending on the field:

| Where | What `{{` means |
| --- | --- |
| Most text values in a node task, such as `content`, `path`, and `env` values, and template files | A reference or a template action, which MMX fills in. |
| A script that runs as written: `script` and `dryrun` of `shell`, the scripts of `run_only_if_this_command_succeeds` and `skip_if_this_command_succeeds`, `command` of `cron_job`, `script` of `systemd_scheduled_job`, `command_succeeds` of `wait_for`, and the `shell` scripts of `check_new_file` and `content_filter` | Text that the node gets as written. MMX fills in nothing. |
| A file that a node task copies with `source` | Text, copied as written. |

**A value that MMX knows, in a script that runs as written,** is refused,
because the shell would get the braces themselves. Pass the value through
`env` instead:

```yaml
      - shell:
          script: echo "Release $RELEASE"
          env:
            RELEASE: "{{ .vars.release }}"
```

Writing `script: echo "Release {{ .vars.release }}"` fails with
`shell.script runs as written, so the shell would receive the literal text {{ .vars.release }}; MMX does not replace references in shell.script; pass the value through env and read the environment variable in the script, such as env: {RELEASE: "{{ .vars.release }}"} and $RELEASE`.
A module without `env`, such as `reboot`, gets the advice to use the node
task's `when` instead. MMX looks for the names it knows: `vars`, `inputs`,
`secrets`, `node`, `outputs`, `node_tasks`, `new_file`, `fleet_stages`,
`inventory`, `this_node_task`, and a loop's `as` name.

**Another tool's template in a script,** such as Docker's
`docker ps --format '{{ .Names }}'`, names no value that MMX knows, so it
stays as written and needs nothing.

**A literal `{{` in a value that MMX fills in,** such as a file's
`content`, goes in back quotes inside braces:

```yaml
      - file:
          path: /usr/local/bin/show-containers
          permissions_mode: "0755"
          content: |
            #!/bin/sh
            docker ps --format '{{`{{.Names}}`}}: {{`{{.Status}}`}}'
```

The file on the node then says `docker ps --format '{{.Names}}:
{{.Status}}'`. A `}}` on its own needs nothing. A template file uses the
same back-quote form.

### Template functions

A template file can call the functions of
[Sprig](https://masterminds.github.io/sprig/), the function library that
[Helm](https://helm.sh/) templates use, with Sprig's names and argument
order, and two functions of MMX's own:

| Function | What it returns |
| --- | --- |
| `percent NUMBER PERCENTAGE` | The percentage of a number that is 0 or more, rounded down to a whole number. The percentage is a whole number from 0 to 100: `{{ percent .node.memory.total_mib 25 }}` is `512` for 2048 MiB. |
| `toYaml VALUE` | The value as YAML with sorted keys and two-space indents, without a line end at the end, as in Helm. |

```text
workers = {{ .node.cpu.count }}
cache_mib = {{ percent .node.memory.total_mib .vars.cache_percent }}
Ciphers {{ join "," .vars.ssh_ciphers }}

scrape_configs:
{{ .vars.scrape_configs | toYaml | indent 2 }}
```

**The Sprig functions that MMX leaves out** depend on more than their
arguments: the clock, the time zone, random values, the environment, or
the network. A template that called one could render different bytes on a
later run and not report `unchanged`. A template that calls one fails before MMX contacts a server, with
`template function now is not available: the function reads the clock, random values, the environment, or the network`:

- The clock and the time zone: `now`, `ago`, `date`, `date_in_zone`,
  `dateInZone`, `date_modify`, `dateModify`, `must_date_modify`,
  `mustDateModify`, `htmlDate`, `htmlDateInZone`, `toDate`, `mustToDate`,
  `unixEpoch`, and `durationRound`.
- Random values: `randAlphaNum`, `randAlpha`, `randAscii`, `randNumeric`,
  `randBytes`, `randInt`, `shuffle`, `uuidv4`, `bcrypt`, `htpasswd`,
  `encryptAES`, `genPrivateKey`, `buildCustomCert`, `genCA`,
  `genCAWithKey`, `genSelfSignedCert`, `genSelfSignedCertWithKey`,
  `genSignedCert`, and `genSignedCertWithKey`.
- The controller machine's environment and the network: `env`,
  `expandenv`, and `getHostByName`.
- `hello`.

For a random password, make the password once and keep it as a secret:
[Keep secrets](../how-to/keep-secrets.md).

### Node facts

MMX measures each node's facts when the node starts a fleet stage, before
any node task, in a dryrun too. A resume and a new run measure them again.
Facts stay fixed within the node's work on one fleet stage. A node task
reads them as `.node.NAME`.

| Fact | Value |
| --- | --- |
| `.node.name` | The node's name in the inventory, such as `web01`. |
| `.node.hostname` | The server's host name. |
| `.node.fully_qualified_name` | The server's full host name with its domain, such as `web01.example.net`, or the host name when the node's resolver gives no domain. |
| `.node.os` | The fields of `/etc/os-release` (or `/usr/lib/os-release`) as a mapping, with lowercase keys and text values. |
| `.node.distribution.id` | `ID` from `/etc/os-release`, such as `debian`, `ubuntu`, `rocky`, `almalinux`, `arch`, or `opensuse-leap`. |
| `.node.distribution.version` | `VERSION_ID` as text, such as `13` or `24.04`. Missing on a rolling release, such as Arch Linux. |
| `.node.distribution.major_version` | The digits before the first dot, as a number, such as `13` or `24`. Missing on a rolling release. |
| `.node.distribution.codename` | `VERSION_CODENAME`, such as `trixie` or `noble`. Missing where the distribution sets none. |
| `.node.distribution.family` | `debian` (Debian, Ubuntu), `redhat` (RHEL, CentOS, Fedora, Rocky Linux, AlmaLinux, Amazon Linux, Oracle Linux), `arch`, or `suse` (openSUSE, SLES). For another ID, the first word of `ID_LIKE` that names one of these. Missing for a distribution outside the four families. |
| `.node.package_manager` | `apt`, `dnf`, `pacman`, or `zypper`, from the family. Missing outside the four families. |
| `.node.selinux.mode` | `enforcing`, `permissive`, or `disabled`. `disabled` also when selinuxfs is mounted but no policy is loaded. |
| `.node.virtualization.type` | What `systemd-detect-virt` reports: `none` on physical hardware, or the kind of virtual machine or container, such as `kvm`, `vmware`, `lxc`, or `docker`. |
| `.node.arch` | `amd64` or `arm64`. |
| `.node.kernel.release` | The kernel release, from `/proc/sys/kernel/osrelease`. |
| `.node.boot_id` | The current boot's ID, which changes at each boot. |
| `.node.cpu.count` | The CPUs that are online; on a virtual machine, its virtual CPUs. |
| `.node.memory.total_bytes` | The memory that the kernel can use, from `MemTotal` in `/proc/meminfo`. Swap does not count, and the value is not the free memory. |
| `.node.memory.total_mib` | The same in MiB, rounded down: a server with 4 GiB of memory gives about `4096`, and 2 GiB gives about `2048`. |
| `.node.memory.swap_total_bytes` | The swap space. |
| `.node.network.interfaces` | A mapping by interface name, loopback included: `index`, `mtu`, `mac`, and `addresses`, each with `family`, `address`, and `prefix_length`, in the order that `ip address` shows. |
| `.node.network.default_routes` | The default routes: `family`, `interface`, `metric`, and where known `gateway` and `source_address`. |
| `.node.network.default_ipv4_address` | The address that the IPv4 default route with the lowest metric sends from: the route's `source_address`, or else the first IPv4 address of the route's interface. A keepalived virtual address that joins the interface later is not chosen. Missing without an IPv4 default route. |
| `.node.network.default_ipv6_address` | The same for IPv6, never a link-local address. Missing without an IPv6 default route. |
| `.node.storage.mounts` | The mounted filesystems: `id`, `path`, `source`, `filesystem`, and `read_only`; for a filesystem on a local disk also `total_bytes` and `available_bytes`. |
| `.node.storage.block_devices` | The node's disks, partitions, and other block devices: see [Block-device fields](#block-device-fields). |

- **A missing fact** is not empty or zero. A node task that reads it fails
  with the reason, and the node then runs none of its later node tasks.
  Node tasks that do not read it are not affected.
- **Collections are sorted,** such as mounts, routes, and block devices,
  so the same server gives the same values on each run. An interface's
  addresses keep the kernel's order.
- **Size settings from total capacity,** such as
  `.node.memory.total_mib`, not from free memory, which changes with load.
- **To see a node's facts,** show them in a node task, or read
  `facts.values` in `mmx results --json`.
- **Saved facts.** Each saved response of a node job holds `facts.values`,
  `facts.collected_at`, and the reason for each fact that was unavailable
  (`facts.unavailable`). A resume measures fresh facts and keeps the
  earlier attempts. The results list the facts that changed between two
  readings of one node within a run (`facts_changed`; human output says
  `facts changed since the previous reading in the run`). Such a change is
  not a resource change. A new run starts a new baseline.
- **A value known before the run.** A field that MMX works out on the
  controller machine before it connects, such as the `template` path of a
  `file` node task, may read `.node.name` but no other fact.
- [Node facts](../how-to/use-variables-and-templates.md#node-facts) shows
  the facts of a real server.

#### Block-device fields

`.node.storage.block_devices` lists each block device of the node whose
size is not zero, sorted by kernel name. Every entry has `name`, `path`,
`kind`, `size_bytes`, and `read_only`; the other fields are there when they
apply and are known.

| Field | What it holds |
| --- | --- |
| `name` | The kernel's name, such as `sdb1` or `dm-0`. |
| `path` | `/dev/<name>`. A kernel path can name another disk after a reboot. |
| `kind` | `disk`, `partition`, `lvm_logical_volume`, `device_mapper_device`, `loop_device`, or `raid_array`. |
| `size_bytes` | The size in bytes. |
| `read_only` | `true` or `false`. |
| `part_of_disk` | For a partition, its disk's kernel name. |
| `built_on_devices` | For a device mapper device or a RAID array, the kernel names of the devices below it. |
| `device_mapper_name` | The name in `/dev/mapper/`, also for an LVM logical volume. |
| `stable_paths_by_id` | Every link in `/dev/disk/by-id/` to the device, as full paths. Find a disk by one of these. |
| `mount_points` | Where the device is mounted. |
| `serial`, `model` | The serial number and model that udev reports. |
| `partition_table_type` | `gpt` or `dos`, for a device that holds a partition table. |
| `content_type`, `content_uuid`, `content_label` | What udev found on the device when udev last looked, such as `ext4`, `xfs`, `swap`, `LVM2_member`, or `crypto_LUKS`, with its UUID and label. |

MMX reads files and links to gather the list, and runs no program and
probes no device. The `content_*` fields say what udev found at its last
probe, not a fresh look at the device; storage modules probe the device
before they change it. Without the udev database, entries omit the fields
that udev supplies.

In a container, the list shows the block devices of the server that runs
the container. [Find a storage device](../how-to/use-variables-and-templates.md#find-a-storage-device)
shows how to pick a disk.
