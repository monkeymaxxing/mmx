# Commands reference

This page lists every `mmx` command with its flags, its exit codes, the
fields of its `--json` output, and what its output looks like. You run
`mmx` on your **controller machine**, the computer where your MMX project
is. The servers that MMX manages are **nodes**. A **playbook** lists
**fleet stages**; each fleet stage picks nodes, and each of those nodes
runs the fleet stage's **node tasks**. A **run** is one recorded execution
of a playbook; MMX saves each run, and its **run ID** names it. To preview
and run changes step by step, see
[Preview and run changes](../how-to/preview-and-run-changes.md); to stop,
resume, and read a run, see
[Stop, resume, and read a run](../how-to/stop-resume-and-read-a-run.md).

## Commands at a glance

Every command except `init`, `version`, `help`, `module-json`, and
`describe-module` finds the project the way `git` finds a repository: by
looking for `mmx_project_settings.yml`, as
[Where the file is](project-settings.md#the-settings-file) says.
(`describe-module` adds the project's own modules when it runs inside a
project.)

| Command | What the command does | Contacts nodes |
| --- | --- | --- |
| [`mmx init`](#mmx-init) | Creates the starter project files in the current directory. | No |
| [`mmx validate [PLAYBOOK]`](#mmx-validate) | Checks the project files. | No |
| [`mmx nodes [PLAYBOOK]`](#mmx-nodes) | Lists the inventory's nodes and groups; with a playbook, also the nodes each fleet stage selects. | No |
| [`mmx explain PLAYBOOK`](#mmx-explain) | Shows the work each node would get, with each value and where the value came from. | No |
| [`mmx dryrun PLAYBOOK`](#mmx-dryrun) | Reports what a run would change, and changes nothing the playbook describes. On a new node, first installs MMX's program and its HTTPS service. | Yes |
| [`mmx run PLAYBOOK`](#mmx-run) | Applies the playbook to the nodes it selects. | Yes |
| [`mmx resume [RUN_ID]`](#mmx-resume) | Finishes the work that a saved run left undone. | Yes |
| [`mmx status [RUN_ID]`](#mmx-status) | Collects the current results of a saved run from its nodes. | Yes |
| [`mmx stop [RUN_ID]`](#mmx-stop) | Asks the nodes of a saved run to stop after their current node task or node task block. | Yes |
| [`mmx results [RUN_ID]`](#mmx-results) | Shows a saved run's results again. | No |
| [`mmx mark-node-work-failed --node NODE [RUN_ID]`](#mmx-mark-node-work-failed) | Records the unconfirmed work on a server that is gone as failed. | Tries to |
| [`mmx run-command --nodes NODES -- COMMAND`](#mmx-run-command) | Runs one shell command on nodes and saves the run. | Yes |
| [`mmx remove-mmx-from-node NODE`](#mmx-remove-mmx-from-node) | Removes MMX from one node and from the controller machine. | Yes |
| [`mmx new-one-time-playbook WHAT_IT_DOES`](#mmx-new-one-time-playbook) | Creates a dated one-time playbook. | No |
| [`mmx check-one-time-playbooks`](#mmx-check-one-time-playbooks) | Reports which nodes still need each one-time playbook. | Yes |
| [`mmx complete-one-time-playbook NAME`](#mmx-complete-one-time-playbook) | Moves a one-time playbook to `completed/` when no node needs it. | Yes |
| [`mmx describe-module [NAME]`](#mmx-describe-module) | Lists the modules, or prints one module's page. | No |
| [`mmx module-usage [PLAYBOOK]`](#mmx-module-usage) | Shows which modules the project uses, and where. | No |
| [`mmx secrets ...`](#mmx-secrets) | Edits the vault of secrets and shows which secret keys exist and where they are used. | No |
| [`mmx module-json ...`](#mmx-module-json) | Helper for a module that is an executable of your own. | No |
| [`mmx version`](#mmx-version) | Prints the version. | No |
| [`mmx help`](#mmx-help) | Prints the commands, the flags, and the exit codes. | No |

## Flags

The commands from `mmx validate` to `mmx remove-mmx-from-node` in the
table above share one set of flags, so `mmx COMMAND --help` for each of
them prints the whole set. Each command's entry below lists the flags
that the command uses. A command refuses `--limit`, `--only-fleet-stages`,
`--var`, `--connection`, `--node`, `--fleet-stage`, `--task`, `--nodes`,
and `--run-as-node-user-account` when it does not use them, with exit
code 4:

```text
$ mmx status --connection ssh_only
mmx: status does not accept --connection
$ mmx dryrun set_up_web_servers --node web01
mmx: dryrun does not accept --node; narrow the selected nodes with --limit
```

`--diff`, `--output`, `--non-interactive`, and `--vault-password-file`
are accepted by each of these commands and change nothing where they
don't apply. A flag may come before or after the playbook name or run ID.
A command takes a playbook as a name, never as a path: `mmx run
playbooks/example.yml` fails with `playbook must be a name, not a path`.
A flag's value goes after a space or after `=`: `--output summary` and
`--output=summary` are the same.

| Flag | Used by | What the flag does |
| --- | --- | --- |
| `--json` | Each command that has a [JSON](#json-output) entry below | Prints one JSON object on standard output instead of text, and asks no question. |
| `--output auto\|nodes\|summary` | `nodes`, `dryrun`, `run`, `resume`, `run-command`, `status`, `stop`, `results`, `check-one-time-playbooks` | Chooses the text view: see [`--output`](#--output). |
| `--diff`, `--diff=all` | `dryrun`, `run`, `resume`, `status`, `stop`, `results` | Adds each file's difference: see [`--diff`](#--diff). |
| `--non-interactive` | Each command that connects to nodes or unlocks the vault, and `mmx secrets` | Asks no question: see [`--non-interactive`](#--non-interactive). |
| `--vault-password-file PATH` | Each command that unlocks the vault, and `mmx secrets` | Reads the vault passphrase from the file `PATH` on the controller machine. An empty `PATH` is refused: `--vault-password-file requires a nonempty path`. [Unlocking the vault](secrets.md#unlocking-the-vault) lists the other ways. |
| `--inventory NAME` | Each command except `init`, `version`, `help`, `describe-module`, `module-usage`, `new-one-time-playbook`, `mark-node-work-failed`, and `module-json` | Uses the inventory `inventories/NAME.yml` instead of the project's `default_inventory`. `NAME` is a name, not a path. |
| `--limit NODE_OR_GROUP[,...]` | `nodes`, `explain`, `dryrun`, `run` | Keeps only the named nodes and groups in every fleet stage. It never adds a node that a fleet stage doesn't select. [Run on some servers only](../how-to/choose-which-servers-a-run-touches.md#run-on-some-servers-only) |
| `--only-fleet-stages ID[,...]` | `nodes`, `explain`, `dryrun`, `run` | Keeps only the named fleet stages, in the order the playbook lists them. `nodes` needs a playbook name with it. [Run only some fleet stages](../how-to/choose-which-servers-a-run-touches.md#run-only-some-fleet-stages) |
| `--var NAME=VALUE` | `validate`, `nodes`, `explain`, `dryrun`, `run` | Sets a variable that the playbook declares in `command_line_vars`. Repeat the flag for each variable. `validate` and `nodes` need a playbook name with it. [Command-line variables](../how-to/use-variables-and-templates.md#command-line-variables) |
| `--connection ssh_only` | `dryrun`, `run`, `run-command`, `check-one-time-playbooks`, `complete-one-time-playbook` | Uses SSH for every node of this command, whatever the inventory says. `ssh_only` is the only value. The run saves the choice, so `mmx resume` uses it too. [A server whose port 7443 is closed](../how-to/connect-to-your-servers.md#a-server-whose-port-7443-is-closed) |
| `--node NODE` | `results`, `mark-node-work-failed` | `results`: shows only this node. `mark-node-work-failed`: the gone node, required. |
| `--fleet-stage PATH`, `--task PATH` | `results` | Shows only this fleet stage or node task and what is inside it: the path `PATH`, or a path that starts with `PATH/` or `PATH[`, such as `shop_site` for `shop_site/changes/write_site_file`. |
| `--nodes NODE_OR_GROUP[,...]`, `--run-as-node-user-account NAME` | `run-command` | See [`mmx run-command`](#mmx-run-command). |

`mmx describe-module`, `mmx module-usage`, `mmx new-one-time-playbook`,
`mmx check-one-time-playbooks`, `mmx complete-one-time-playbook`,
`mmx secrets`, and `mmx module-json` each have only their own flags,
listed in their entries.

### `--output`

`--output` chooses how the text output shows nodes. `--json` output is
not changed by it.

| Value | What the text shows |
| --- | --- |
| `auto` (default) | `nodes` when the command selects up to ten nodes, `summary` for more. MMX chooses once, when the command starts. When some nodes are known only during the run, MMX counts the nodes that could be selected: the nodes that `--limit` keeps, or the whole inventory. |
| `nodes` | Each node on its own lines, and, while the work runs, what each active node is doing. |
| `summary` | Nodes with the same result on one line, with their count and names, such as `set_up_nginx / 2 nodes [web01, web02]: succeeded`. Different failures and different changes stay on separate lines. |

The view's line, such as
`Output: nodes (change with --output=nodes or --output=summary)`, appears
once in each command's output. A node task's failure shows its nonzero
exit code and its captured output; a stream that was cut short ends with a
marker that says so. Values that a playbook prints with `show` appear in
both views.

`mmx nodes` and `mmx check-one-time-playbooks` use `--output` in their own
way: see their entries.

### `--diff`

`--diff` adds each changed file's difference to the text output, such as:

```text
set_up_nginx / web01 / write_home_page: would create "/srv/shop/public/index.html"
  absent -> 14 bytes, permissions_mode 0644, uid 0, gid 0
  --- before
  +++ after
  @@ -0,0 +1,1 @@
  +<h1>Shop</h1>
```

| Form | What it shows |
| --- | --- |
| `--diff` | A shortened difference, which the node sent with the result. A long difference is cut. |
| `--diff=all` | The complete difference, which the controller machine downloaded and saved when the node job finished. |
| `--json --diff=all` | Adds two fields to the JSON of a run: `full_differences`, each complete difference by the SHA-256 reference that the node task's result names; and `full_differences_unavailable`, the references whose difference is not on the controller machine. |

A complete difference can be missing on the controller machine: the node
job still runs, the download failed, or the saved file is missing or
damaged. MMX then says that the difference is not available locally. The
node task's result stays as it was, and nothing runs again. A failed
download shows in the result as housekeeping, apart from the node task's
own result.

Any other value is refused:
`mmx: invalid boolean value "some" for -diff: use --diff for previews or --diff=all for complete saved differences`.

### `--non-interactive`

With `--non-interactive`, MMX asks no question: not for a vault
passphrase, an SSH password, or a sudo password. A value that MMX would
ask for must then come from somewhere else, or the command fails. MMX also
asks no question with `--json`, or when standard input is not a terminal.
[Run without a terminal](../how-to/keep-secrets.md#run-without-a-terminal)
and [Passwords for automation](../how-to/connect-to-your-servers.md#passwords-for-automation)
say where those values can come from.

## mmx init

`mmx init` creates a starter project in the current directory.

```text
$ mmx init
Created mmx_project_settings.yml, inventories/example.yml, and playbooks/example.yml. Set the SSH destination in inventories/example.yml, then run mmx dryrun example.
```

- It writes `mmx_project_settings.yml`, `inventories/example.yml`, and
  `playbooks/example.yml`, and adds the line `/.mmx/` to `.gitignore`,
  because `.mmx/` holds the saved runs.
- It takes no arguments and no flags.
- It changes nothing when one of the three files exists, and says which:
  `mmx: mmx_project_settings.yml already exists`, exit code 4.

[Get started](../get-started.md) uses it.

## mmx validate

`mmx validate` checks the project files on the controller machine and
contacts no node.

```text
$ mmx validate set_up_web_servers
Valid: mmx_example
```

- Without a playbook name, it checks the project settings, every
  inventory, every playbook, and every one-time playbook outside
  `completed/`. With a playbook name, it checks that playbook with the
  selected inventory.
- It doesn't run modules or unlock the vault.
- An error names the file, line, column, and field, such as
  `playbooks/set_up_web_servers.yml:12:7`, and the variable or role
  input that supplied a wrong value.
- Values known only during a run, such as node facts and earlier node
  tasks' results, are checked when the run knows them.
- A literal value is checked in every branch, also in a node task that a
  `when` would skip. A reference in a skipped node task is not evaluated.
- A fixed field name in a reference to a node task's result, such as
  `.node_tasks.ID.stdout`, is checked on the controller machine, also
  beneath `items[n]`; a wrong name fails with
  `unknown node task result field in ...; valid fields: ...`.

| Flag | Use |
| --- | --- |
| `--inventory NAME` | Checks the playbook with this inventory. |
| `--var NAME=VALUE` | Needs a playbook name. A rejected value exits with 4, also for `validate`. |
| `--json` | Prints `{"project_name": "mmx_example", "schema_version": 1, "valid": true}` when the files are valid. An invalid project prints the error on standard error, with exit code 1. |

Exit codes: 0 valid; 1 invalid, also when no project is found; 4 a
rejected `--var` value, or a playbook name that is in
`one_time_playbooks/completed/`.

[Write a playbook](../how-to/write-a-playbook.md) uses it.

## mmx nodes

`mmx nodes` lists the inventory's nodes and groups. With a playbook name,
it also shows the nodes that each fleet stage selects. It contacts no
node.

```text
$ mmx nodes set_up_web_servers --limit web01
Playbook: set_up_web_servers   Inventory: example (inventories/example.yml, 2 nodes)
Limit: web01

Nodes
  web01   203.0.113.11   root   web_servers
  web02   203.0.113.12   root   web_servers

Groups
  web_servers   web01, web02

Fleet stages (limit applied)
  set_up_nginx   web_servers   → web01
```

| Flag | Use |
| --- | --- |
| `--inventory NAME` | Lists this inventory. |
| `--limit`, `--only-fleet-stages`, `--var` | Show the selection that a run with the same flags would make. `--only-fleet-stages` and `--var` need a playbook name. |
| `--output` | `auto` lists nodes for up to ten nodes in the inventory, and shows counts for more. `summary` leaves out the `Nodes` list and shows a count for each group and fleet stage. |
| `--json` | See below. |

A fleet stage's line can also say `resolved during the run` (its nodes
come from a value that only the run knows), `none; left out by
--only-fleet-stages`, `none; left out by the limit`, or
`none; selection contains no nodes (skip_when_no_nodes_are_selected)`.
[See the selection before you run](../how-to/choose-which-servers-a-run-touches.md#see-the-selection-before-you-run)
explains these lines.

### JSON of mmx nodes

```sh
mmx nodes set_up_web_servers --limit web01 --json
```

```json
{
  "schema_version": 1,
  "playbook": "set_up_web_servers",
  "inventory": "example",
  "inventory_file": "/home/you/shop/inventories/example.yml",
  "limit": [
    "web01"
  ],
  "nodes": [
    {
      "name": "web01",
      "address": "203.0.113.11",
      "ssh_user": "root",
      "groups": [
        "web_servers"
      ]
    },
    {
      "name": "web02",
      "address": "203.0.113.12",
      "ssh_user": "root",
      "groups": [
        "web_servers"
      ]
    }
  ],
  "groups": {
    "web_servers": [
      "web01",
      "web02"
    ]
  },
  "fleet_stages": [
    {
      "fleet_stage": "set_up_nginx",
      "selection": "web_servers",
      "nodes": [
        "web01"
      ],
      "left_out": [
        "web02"
      ]
    }
  ]
}
```

| Field | Value |
| --- | --- |
| `schema_version` | `1`. |
| `playbook` | The playbook's name. Left out without a playbook name. |
| `inventory` | The inventory's name, such as `example`: the same name that the JSON of a run has in `inventory`. |
| `inventory_file` | The inventory file's full path on the controller machine. |
| `limit`, `only_fleet_stages` | The values of `--limit` and `--only-fleet-stages`, as lists. Left out when not given. |
| `nodes` | Each node, sorted by name: `name`, `address`, `ssh_user` (left out when the inventory has none), and `groups`. |
| `groups` | Each group's name with its nodes. |
| `fleet_stages` | With a playbook name, one entry for each fleet stage: `fleet_stage` (its path), `selection` (its `nodes` as written), `nodes` (the selected nodes), `left_out` (the nodes that `--limit` removed), `except_nodes`, and the flags `resolved_during_run`, `left_out_by_only_fleet_stages`, `left_out_because_it_selects_no_nodes`, and `narrowed_during_run_by_when`. A field that is empty or false is left out. A `for_each_node` loop has one entry for each iteration. |
| `helper_nodes` | The [helper nodes](../how-to/roll-out-a-change-gradually.md) that a `for_each_node` loop reaches outside `--limit`: `node` and `fleet_stages`. Left out when there are none. |

Exit codes: 0, or 4 when the project, inventory, playbook, or a flag
can't be used.

## mmx explain

`mmx explain PLAYBOOK` shows, for each node, the node tasks it would run,
each value they get, and where each value came from. It contacts no node
and reads no secret.

```text
$ mmx explain set_up_web_servers --limit web01
Project mmx_example
Limit web01

set_up_nginx → web01
  web01 / set_up_nginx/install_nginx: apt_package (ready)
    at playbooks/set_up_web_servers.yml:5:9 (fleet_stages[0].node_tasks[0])
    apt_package.packages = [{"name":"nginx","version":"newest_when_first_installed"}]
      from playbooks/set_up_web_servers.yml:8:13 (fleet_stages[0].node_tasks[0].apt_package.packages)
    ...
    retry.max_tries = 3
      from MMX default
```

- A value's source is the playbook line, the group, the `--var` flag, or
  the default that supplied it. Time limits and retries show their
  effective values.
- It shows each template rendered for each node, and the rules of a `ufw`
  node task with one rule for each node of an inventory group. A template
  that reads a value known only on the node or during the run stays
  unrendered, and `explain` names that value.
- It lists the files that `save_on_controller_machine` would write on the
  controller machine.
- `explain` knows each node's name, so `{{ .node.name }}` is resolved in
  arguments, in a `when`, and in templates. It also leaves a template
  unrendered when the template reads node facts, an earlier node task's
  result, a fleet stage output, or a secret; `explain` never reads a
  secret, and a secret appears by its name only.
  A module may add to its file on the node: the rendered
  `wireguard_interface` template has no `PrivateKey` line, which the
  module adds on the node.
- For the same project files and inventory, `explain` prints the same
  output.

| Flag | Use |
| --- | --- |
| `--inventory`, `--limit`, `--only-fleet-stages`, `--var` | As for `mmx run`. |
| `--json` | The same facts as one JSON object with `schema_version: 2`: `project`, and `fleet_stages`, each with `path`, `selection`, `nodes`, `source`, and `node_tasks`. Each node task has `path`, `node`, `action`, `module_kind`, `status`, `source`, `arguments` (the values as written, each with `field`, `value`, `status`, and `source`), `timing` (time limits and retries), `rendered_templates` (`field`, and `text` or `not_rendered_because`), and `expanded_inventory_group_rules` (`field` and `rules`). A fleet stage with `save_on_controller_machine` lists each written entry with `path`, `files` (each `node` and `file`), and `permissions_mode`, or `files_resolved_during_run: true` when the run decides the files. |

Exit codes: 0, or 4.
[Preview and run changes](../how-to/preview-and-run-changes.md#steps)
uses it, and
[Values about other servers](../how-to/use-variables-and-templates.md#values-about-other-servers)
shows the rendered views.

The [`ufw` page](modules/ufw.md) shows how `mmx explain` lists a node task's firewall rules.

## mmx dryrun

`mmx dryrun PLAYBOOK` connects to each selected node and reports what a
run would change. A dryrun changes nothing the playbook describes. On a
node without MMX, the dryrun first sets MMX up: it installs MMX's program
in `/opt/mmx/bin/` and starts its HTTPS service, `mmx-https-api.service`,
which listens on TCP port 7443 on every network address.

```sh
mmx dryrun set_up_web_servers --limit web01 --diff
```

The results go to standard output; the lines about the work while it
runs go to standard error, as [Output](#output) shows. The first dryrun
of the starter project prints these results:

```text
welcome / web01: succeeded
  welcome / web01 / task_1: succeeded, would_change
  welcome / web01 / task_1: would create "/etc/mmx-example"
    absent -> 16 bytes, permissions_mode 0644, uid 0, gid 0
Run fbe759f2: succeeded
1 finished
Saved: /home/you/shop/.mmx/runs/20261115T035229.429039873-fbe759f2091942e3
```

| Flag | Use |
| --- | --- |
| `--inventory`, `--limit`, `--only-fleet-stages`, `--var`, `--connection ssh_only` | Choose the inventory, nodes, fleet stages, values, and connection, as for `mmx run`. |
| `--output`, `--diff`, `--json` | Choose the output: [`--output`](#--output), [`--diff`](#--diff), [The JSON of a run](#the-json-of-a-run). |
| `--non-interactive`, `--vault-password-file` | See [Flags](#flags). |

- MMX saves a dryrun like a run, and `mmx results` shows it again. A
  dryrun can't be resumed.
- Without a playbook name, the command names the playbooks it found:
  `mmx: dryrun requires a playbook name; available playbooks: set_up_web_servers`.
- A dryrun that did not succeed ends with `Inspect: mmx results ID` and
  `Start a new run: mmx run PLAYBOOK`. It does not suggest `mmx resume`,
  which refuses a dryrun.
- In a script or in CI, treat exit code 2 as "changes are pending", and
  stop on 1, 3, 4, and 130:

  ```sh
  mmx dryrun set_up_web_servers; rc=$?
  [ $rc -eq 0 ] || [ $rc -eq 2 ] || exit $rc
  ```

| Exit code | Meaning |
| --- | --- |
| `0` | Nothing would change. |
| `1` | A node task or a node job failed. |
| `2` | Something would change. |
| `3` | Some work could not be checked, or a node's state could not be confirmed. A failure counts before 3, and 3 before 2. |
| `4`, `130` | See [Exit codes](#exit-codes). |

[Preview and run changes](../how-to/preview-and-run-changes.md#what-the-results-mean)
says what each result means.

## mmx run

`mmx run PLAYBOOK` applies the playbook to the nodes it selects, and
saves the run.

```sh
mmx run set_up_web_servers --limit web01
```

| Flag | Use |
| --- | --- |
| `--inventory NAME` | Uses `inventories/NAME.yml`. |
| `--limit NODE_OR_GROUP[,...]` | Keeps only these nodes and groups. An unknown name is refused before any node is contacted: `mmx: --limit: no node or inventory group named "web99"`. |
| `--only-fleet-stages ID[,...]` | Keeps only these fleet stages. An unknown ID is refused: `mmx: --only-fleet-stages: the playbook has no fleet stage "nope"; its fleet stages are set_up_nginx`. |
| `--var NAME=VALUE` | Sets a declared command-line variable. |
| `--connection ssh_only` | Uses SSH for every node. |
| `--output`, `--diff`, `--json` | Choose the output. |
| `--non-interactive`, `--vault-password-file` | See [Flags](#flags). |

- The run saves its selection: the playbook, the inventory, the files
  that it sends, `--limit`, `--only-fleet-stages`, `--var`, and
  `--connection`. `mmx resume` uses these saved values.
- A limit is also refused when a kept fleet stage needs, or reads the
  outputs of, a fleet stage that the limit leaves out.
  [Run on some servers only](../how-to/choose-which-servers-a-run-touches.md#run-on-some-servers-only)
  has the rules.
- Ctrl-C once asks the nodes to stop after their current node task or
  node task block. Ctrl-C again leaves the display, with exit code 130,
  while the nodes finish.
  [Stop a run](../how-to/stop-resume-and-read-a-run.md#stop-a-run) has
  the details.
- A run that did not succeed ends with the commands to use next:

  ```text
  Run 6bd33f06: failed
  1 failed
  Saved: /home/you/shop/.mmx/runs/20261115T035339.314724231-6bd33f06bbfa8a71
  Inspect:                mmx results 6bd33f06
  Resume unfinished work: mmx resume 6bd33f06
  Start a new run:        mmx run set_up_web_servers
  ```

  When MMX could not reach a node, the end also names
  `mmx mark-node-work-failed` for that node.

Exit codes: 0 the run succeeded; 1 the run did not succeed: a node task
failed, MMX could not reach a node, or the run stopped with work
unfinished; 4 and 130 as in [Exit codes](#exit-codes).

## mmx resume

`mmx resume [RUN_ID]` finishes the work that a saved run left undone,
with the run's saved playbook, inventory, files, and values. It reads the
run's secrets again.

```text
$ mmx resume
Selected run 20261115T083238.945901911-2b38a3bf101e0c7a, playbook set_up_web_servers.
Nothing to resume.
```

- Without a run ID, it picks the latest run that was not a dryrun, as
  [Selecting saved runs](#selecting-saved-runs) says.
- It refuses `--limit`, `--only-fleet-stages`, `--var`, and
  `--connection`, because it repeats the run's saved values; each message
  says to start a new `mmx run` instead.
- It refuses a dryrun: `mmx: cannot resume a dryrun; start a new mmx run with a playbook name`.
- It refuses a run that another MMX release started, before it contacts
  any node, with exit code 4:
  `mmx: this playbook run was started by MMX 0.1.0, but this mmx program is version 0.1.1. ...`.
  Resume that run with the release that started it, or start a new run.
  [Finish a run that an earlier release started](../how-to/upgrade-mmx.md#finish-a-run-that-an-earlier-release-started)
  shows how.
- A run with nothing left to do prints `Nothing to resume.` with exit
  code 0. With `--json`, it prints `id`, `short_id`, `playbook`,
  `schema_version: 1`, and `status: "nothing_to_resume"`.

| Flag | Use |
| --- | --- |
| `--inventory NAME` | Without a run ID, picks the latest run with this inventory. |
| `--output`, `--diff`, `--json` | Choose the output. |
| `--non-interactive`, `--vault-password-file` | See [Flags](#flags). |

Exit codes: as for [`mmx run`](#mmx-run). [What a resume repeats](../how-to/stop-resume-and-read-a-run.md#what-a-resume-repeats)
says which work runs again.

## mmx status

`mmx status [RUN_ID]` asks the nodes of a saved run for their current
results, and saves them. It starts no work.

```sh
mmx status 2b38a3bf
```

- Without a run ID, it picks the latest run, also a dryrun, as
  [Selecting saved runs](#selecting-saved-runs) says.
- It prints the run's results, as they stand after it has asked the nodes.
- A run that is still working ends with `Check progress: mmx status ID`
  and `Request stop:   mmx stop ID`.

| Flag | Use |
| --- | --- |
| `--inventory NAME` | Without a run ID, picks the latest run with this inventory. |
| `--output`, `--diff`, `--json` | Choose the output. |
| `--non-interactive`, `--vault-password-file` | See [Flags](#flags). |

| Exit code | Meaning |
| --- | --- |
| `0` | MMX knows how the work stands on every node, also when the work failed or still runs. |
| `3` | MMX could not reach a node, so the work there is unconfirmed. |
| `4` | No saved run matches, a short ID is ambiguous, or a flag is wrong. |

## mmx stop

`mmx stop [RUN_ID]` asks each node of a saved run to stop after its
current node task or node task block, the same request as Ctrl-C during
the run. It returns after it has contacted the nodes.

```sh
mmx stop 2b38a3bf
```

- It shows which nodes acknowledged the stop, which still finish their
  current work, and which could not be reached.
- It starts no node job or fleet stage.
- Flags, run selection, and exit codes: as for [`mmx status`](#mmx-status).

[Stop a run from another terminal, or as a coding agent](../how-to/stop-resume-and-read-a-run.md#stop-a-run-from-another-terminal-or-as-a-coding-agent)
uses it.

## mmx results

`mmx results [RUN_ID]` shows a saved run's results from the controller
machine. It contacts no node.

```sh
mmx results
mmx results 2b38a3bf --node web01 --task shop_site
mmx results --diff=all
```

| Flag | Use |
| --- | --- |
| `--node NODE` | Shows only this node. |
| `--fleet-stage PATH` | Shows only this fleet stage and the fleet stages inside it: the path `PATH`, or a path that starts with `PATH/` or `PATH[`. |
| `--task PATH` | Shows only this node task and the node tasks inside it, matched the same way: `--task shop_site` does not match `shop_site_extra`. |
| `--inventory NAME` | Without a run ID, picks the latest run with this inventory. |
| `--output`, `--diff`, `--json` | Choose the output. |

- Without a run ID, it picks the latest run, also a dryrun, as
  [Selecting saved runs](#selecting-saved-runs) says.
- It shows a run of any status, including a run that still works; its
  results are then those that the controller machine has so far.
  `mmx status` collects newer ones.

Exit codes: 0 when MMX showed the run, whatever the run's result; 4 when
no saved run matches, a short ID is ambiguous, or a flag is wrong.
[Read a saved run](../how-to/stop-resume-and-read-a-run.md#read-a-saved-run)
uses it.

## mmx mark-node-work-failed

`mmx mark-node-work-failed --node NODE [RUN_ID]` records the work on a
server that is gone as failed, so that the run can end.

```text
$ mmx mark-node-work-failed --node web03 23ef7888
Run 23ef7888: recorded 1 node jobs on web03 as failed: the team says this server is gone. The run is failed now.
```

- It records each node job of the run on `NODE` whose outcome is
  unconfirmed as failed, with the reason
  `the team says this server is gone`. Without a run ID, it does this in
  every saved run in `.mmx/runs/` with unconfirmed work on `NODE`: of any
  inventory, and including runs of `mmx run-command`. Node names can
  repeat between inventories, so look at the `runs` it lists.
- It first tries to reach the server, as `mmx status` does, and records
  nothing when the server answers. When the server's address now reaches
  another machine, it records the work as replaced. When OpenSSH can't
  read the SSH configuration that the run saved, such as an `ssh_config`
  file that was removed with a temporary project, no command can reach
  the server with that run's settings: MMX records the work and says that
  it could not try to reach the server.
- It tries every run, also after one that it did not record.
- It records nothing when it can't tell whether the server is gone, such
  as after a refused login, and nothing for a run that its own `mmx`
  process still holds.

| Flag | Use |
| --- | --- |
| `--node NODE` | Required. The gone server's node name. |
| `--json` | Prints `schema_version`, `node`, `reason`, `runs` (each `id`, `short_id`, `node_jobs_recorded_as_failed`, and the run's `status`), and `runs_not_recorded` (each `id`, `short_id`, `reason`, and `exit_code`). |

It refuses `--inventory`, because it looks at every saved run:
`mmx: mark-node-work-failed does not accept --inventory; it scans every saved run`
(exit code 4).

The exit code is the most serious outcome over the runs it tried:

| Exit code | Meaning |
| --- | --- |
| `0` | Each run was recorded, or no run had unconfirmed work on the node. |
| `2` | The server answered, so MMX recorded nothing. `mmx status` shows how the work ended. |
| `3` | MMX could not tell whether the server is gone, and recorded nothing. |
| `4` | An error, such as a run that its own `mmx` process still holds. |

[A server that is gone](../how-to/stop-resume-and-read-a-run.md#a-server-that-is-gone)
uses it.

## mmx run-command

`mmx run-command --nodes NODE_OR_GROUP[,...] -- COMMAND` runs one shell
command on each named node, as `root` by default, and saves the run.

```sh
mmx run-command --nodes web_servers -- uptime
mmx run-command --nodes web01 --run-as-node-user-account shop -- ls /srv/shop
```

| Flag | Use |
| --- | --- |
| `--nodes NODE_OR_GROUP[,...]` | Required. The nodes and groups to run the command on. |
| `--run-as-node-user-account NAME` | Runs the command as this account on each node instead of `root`. |
| `--inventory NAME` | Uses `inventories/NAME.yml`. |
| `--connection ssh_only` | Uses SSH for every node. |
| `--output`, `--json` | Choose the output. |
| `--non-interactive`, `--vault-password-file` | See [Flags](#flags). |

- Everything after `--` is the command. Without `--nodes` or without a
  command, MMX prints
  `mmx: usage: mmx run-command --nodes NODE_OR_GROUP[,...] [--run-as-node-user-account NAME] -- COMMAND`.
- The run has one fleet stage, `run-command`, with one node task,
  `command`. In the JSON of the run, `entry_point` is `run-command`. The
  fleet stage uses the project's `max_nodes_at_once` and `node_task_defaults`,
  no node task retry, and `on_node_failure: continue`, so one failed node
  does not stop the others. The command text is saved in the run's history.
- `mmx results`, `mmx status`, `mmx stop`, and `mmx resume` pick a run of
  `mmx run-command` only by its run ID, which the run prints at its start
  and end (see [Selecting saved runs](#selecting-saved-runs)).

Exit codes: 0 when the command succeeded on every node; 1 when it failed
on a node or MMX could not reach a node; 4 and 130 as in
[Exit codes](#exit-codes).
[Run a command on servers now](../how-to/run-a-command-on-servers-now.md)
uses it.

## mmx remove-mmx-from-node

`mmx remove-mmx-from-node NODE` removes MMX from one node, and removes
what the controller machine keeps for that node.

```sh
mmx remove-mmx-from-node web03
```

- It refuses while a node job runs on the node, or while an owed restore
  waits there: a record on the node that puts a network configuration
  back unless MMX confirms in time that it can still reach the node.
- It prints one line for each thing it removed, then says that to stop
  managing the node with MMX, you also take the node out of the
  inventory, its groups, and the playbooks. It changes neither the
  inventory nor the playbooks, so a run that still selects the node sets
  MMX up on it again.
- A name that the inventory doesn't have is refused, with exit code 4:
  `mmx: remove-mmx-from-node: no node named "web09" in inventory example`.
- It doesn't accept `--json`:
  `mmx: usage: mmx remove-mmx-from-node NODE [--inventory NAME]; the command prints text and does not accept --json`.

| Flag | Use |
| --- | --- |
| `--inventory NAME` | The inventory that names the node. |
| `--non-interactive` | See [Flags](#flags). |

Exit codes: 0 removed, or nothing was left; 2 refused, because a node job
runs or an owed restore waits on the node; 4 an error.
[Remove MMX from a server](../how-to/keep-mmx-off-a-server.md#remove-mmx-from-a-server)
lists every step it does.

## mmx new-one-time-playbook

`mmx new-one-time-playbook WHAT_IT_DOES` creates a **one-time playbook**,
a dated playbook in `one_time_playbooks/` for work that happens once on
each node it selects.

```text
$ mmx new-one-time-playbook remove_old_ruby_manager
Created one_time_playbooks/2026-11-15_remove_old_ruby_manager.yml. Replace its example node task with the work, then preview it with mmx dryrun 2026-11-15_remove_old_ruby_manager.
```

- `WHAT_IT_DOES` is lowercase words joined by underscores, starting with
  a letter, because it becomes the ID of the new playbook's fleet stage.
- The file is named with today's date. A name that already exists, also
  in `completed/`, is refused, because a one-time playbook's name is
  never used again.
- It has no flags, and no `--json`, because the result is the new file.

Exit codes: 0, or 4.
[Make a one-time change](../how-to/make-a-one-time-change.md) uses it.

## mmx check-one-time-playbooks

`mmx check-one-time-playbooks` reports, for each one-time playbook
outside `completed/`, which nodes still need it. It changes nothing that a
one-time playbook would change: for each one-time playbook, it makes a
dryrun and saves it. As any dryrun does, it sets MMX up on a node that
does not have it yet.

```text
$ mmx check-one-time-playbooks
Inventory: example (inventories/example.yml)

No outstanding one-time playbook: one_time_playbooks/ holds none outside completed/.
```

| Flag | Use |
| --- | --- |
| `--inventory NAME` | Checks the nodes of this inventory. |
| `--connection ssh_only` | Uses SSH for every node. |
| `--output auto\|nodes\|summary` | `auto` names the nodes that are not done; `nodes` names every node and the dryrun's run ID; `summary` shows counts only. |
| `--json` | Prints `schema_version`, `inventory`, and `one_time_playbooks`: each with `name`, `run_id` (the dryrun's full run ID), `done`, `still_needed`, and `not_known` (each a list of `node` and `reason`), and `not_inspected_reason` when the dryrun could not inspect every node. |
| `--non-interactive`, `--vault-password-file` | See [Flags](#flags). |

- It takes no playbook name.
- While it works, it writes `Inspecting NAME: ...` lines on standard
  error, such as
  `Inspecting 2026-11-15_remove_old_ruby_manager: 2 running`.
- It unlocks the vault once, also for several playbooks. Ctrl-C stops the
  dryrun that runs, and MMX inspects no later playbook.
- It takes no `--limit`, `--only-fleet-stages`, or `--var`.
- A node with several causes for `not_known` shows the first one, in the
  playbook's order, on one line. The reason is one of:

| Reason | Meaning |
| --- | --- |
| `MMX could not connect: ERROR` | The controller process had no connection to the node. `ERROR` is what the attempt answered. |
| `fleet stage X: ERROR` | The node job of fleet stage `X` did not succeed or did not start. `ERROR` is what the node or the controller process recorded. |
| `the dryrun of fleet stage X ended with status S` | The node job did not succeed and recorded no text. |
| `the dryrun of fleet stage X could not check node task T` | A dryrun cannot tell whether `T` still has work, such as a `shell` task with no guard, no `dryrun` command, and no `run_only_once_marker_file`. |
| `node task T failed in the dryrun of fleet stage X` | `T` failed while the dryrun inspected the node. |
| `the dryrun of fleet stage X could not check every node task` | The node job ended unfinished, and no single node task says why. |

- When the dryrun cannot say which nodes a fleet stage has, or a
  `for_each_node` iteration started no node job, those nodes are in none
  of the three lists. The report adds a line `the dryrun did not inspect
  every node: ...`, and the playbook counts as not known. In JSON the same
  text is `not_inspected_reason`. A playbook that cannot load with the
  inventory shows `inspected nothing: MMX cannot load the playbook with
  inventory NAME: ...` in that field.

| Exit code | Meaning |
| --- | --- |
| `0` | No node needs an outstanding one-time playbook. |
| `2` | A node still needs one. |
| `3` | MMX could not inspect a node, or a playbook inspected no node. |
| `4` | An error, such as a project that can't be loaded. |

[Make a one-time change](../how-to/make-a-one-time-change.md#how-mmx-knows-which-servers-are-done)
explains what "done" means.

## mmx complete-one-time-playbook

`mmx complete-one-time-playbook NAME` checks one one-time playbook as
`check-one-time-playbooks` does, and, when no node needs it, moves its
file to `one_time_playbooks/completed/`. It commits nothing to Git.

```sh
mmx complete-one-time-playbook 2026-11-15_remove_old_ruby_manager
```

| Flag | Use |
| --- | --- |
| `--inventory NAME` | Checks the nodes of this inventory. |
| `--connection ssh_only` | Uses SSH for every node. |
| `--json` | The fields of `check-one-time-playbooks`, and `status` (`completed` or `refused`), `completed_playbook_file`, and `inventories_not_inspected`: the project's other inventories, which this command did not check. |
| `--non-interactive`, `--vault-password-file` | See [Flags](#flags). |

- MMX writes the new file in `completed/` first, with the `# Completed`
  line followed by the playbook's unchanged text, and removes the original
  only after that, so no failure loses the playbook. MMX never replaces or
  deletes a file in `completed/`. A playbook moved back and completed again
  has one such line for each completion.
- A playbook that cannot load, such as one that names a group the
  inventory lacks, is a command error with exit code 4: a misspelled group
  would otherwise complete a playbook whose work never happened.
- Messages show file paths relative to the directory where you ran the
  command; the JSON holds absolute paths.
- It checks only the nodes of one inventory, and names the other
  inventories in a note. [Several inventories](../how-to/make-a-one-time-change.md#several-inventories)
  says how to check them.
- It refuses before it contacts a node when the file is in both
  directories, or in neither.

| Exit code | Meaning |
| --- | --- |
| `0` | MMX moved the file to `completed/`. |
| `2` | Refused, because a node still needs the playbook. |
| `3` | MMX could not inspect a node, so the file stays. |
| `4` | An error, such as a wrong name. |

## mmx describe-module

`mmx describe-module [NAME]` lists the modules, or prints one module's
page. It contacts no node.

```sh
mmx describe-module
mmx describe-module apt_package
mmx describe-module apt_package --reference-only
```

| Flag | Use |
| --- | --- |
| `--reference-only` | Prints only the module's reference: its kind, its inputs with types and defaults, the example, the results, and its dryrun and retry behavior. |
| `--json` | Without a name: `schema_version` and `entries`, each with `name`, `kind`, `topic`, and `summary`. With a name: the reference as fields, with `kind`, and the page in `page` unless `--reference-only` is given. |

- Without a name, it lists the core modules by topic, the official extra
  modules, and, inside a project, the project's own modules in
  `external_modules/`. It also lists the node task forms that are not
  modules (`role`, `task_block`, `set_vars`, `show`) and `task`, the
  settings that every node task accepts.
- At a terminal, a module's page opens in `$PAGER`, or in `less -FRX`
  when `PAGER` is not set.
- An unknown name exits with 4 and says where MMX looked.
- A core module's field names, types, allowed values, and retry defaults
  come from the schema that `validate` uses, so the two always agree. For
  a module that is not core, the reference is built from the module's
  `module_description.yml`. The command reads the project only to find the
  project's own modules. For an executable module, the dryrun line says
  that MMX cannot check the module's dryrun. For a project's own module,
  the page is the reference followed by the module's `README.md` when
  `external_modules/NAME/` has one.

[Find the module for a job](../how-to/find-the-module-for-a-job.md) uses it.

## mmx module-usage

`mmx module-usage [PLAYBOOK]` lists the modules that the project's
playbooks use, and each file and line that calls each module. It contacts
no node.

```text
$ mmx module-usage
Core modules (built into MMX)
  apt_package       playbooks/set_up_web_servers.yml:7
  directory         playbooks/set_up_web_servers.yml:12
  file              playbooks/set_up_web_servers.yml:17, playbooks/set_up_web_servers.yml:27
  systemd_service   playbooks/set_up_web_servers.yml:37
  wait_for          playbooks/set_up_web_servers.yml:44
```

| Flag | Use |
| --- | --- |
| `--module NAME` | Shows only this module. |
| `--json` | `schema_version` and `modules`, each with `name`, `kind` (`core`, `official_extra`, `project_starlark`, or `project_executable`), `dryrun_checked_by_mmx`, and `calls` (each `file`, `line`, `column`, and `field`). |

- With a playbook name, it covers only that playbook and the roles it
  calls.
- Text output also shows the directory of each of the project's own
  modules; in `--json`, each module that is not core has a `directory`.
- A module of the project's own that no node task calls is listed last.
  For an executable module, it names the program that the executable's
  first line names, and notes that MMX can't check its dryrun.

[See which modules a project uses](../how-to/find-the-module-for-a-job.md#see-which-modules-a-project-uses)
uses it.

## mmx secrets

`mmx secrets` changes the project's **vault**, the encrypted file of
secrets such as passwords, and shows which **secret keys** (the secrets'
names) exist and where the project uses them. The
[secrets reference](secrets.md#the-mmx-secrets-commands) owns each
subcommand's flags, output, and exit codes.

| Command | What the command does |
| --- | --- |
| [`mmx secrets edit`](secrets.md#mmx-secrets-edit) | Opens the vault in your editor, and creates the vault if there is none. |
| [`mmx secrets passwd`](secrets.md#mmx-secrets-passwd) | Changes the vault passphrase. |
| [`mmx secrets add-random KEY`](secrets.md#mmx-secrets-add-random) | Adds a secret key with a generated value, without showing the value. |
| [`mmx secrets set KEY`](secrets.md#mmx-secrets-set) | Sets a secret key to the value on standard input, without showing the value. |
| [`mmx secrets list-keys`](secrets.md#mmx-secrets-list-keys) | Lists the vault's secret keys, never a value. |
| [`mmx secrets diff-keys`](secrets.md#mmx-secrets-diff-keys) | Shows which secret keys changed since the last Git commit, never a value. |
| [`mmx secrets usage [KEY]`](secrets.md#mmx-secrets-usage) | Shows where each secret key is used. |

## mmx module-json

`mmx module-json` is a helper for a module that is an executable of your
own: it checks the request that MMX sends to the module, reads values
from the request, and writes the module's result. `mmx module-json --help`
prints its three subcommands, `request`, `get`, and `result`.
[The module-json helper](module-interface.md#the-module-json-helper) owns
its use.

Exit codes: 1 when the request or a value is wrong; 4 when the helper's
own arguments are wrong.

## mmx version

`mmx version` prints the version of `mmx` and of the protocol between the
controller machine and the nodes.

```text
$ mmx version
mmx 0.1.0 (protocol 1)
```

A build from a Git checkout also prints its commit, such as
`mmx 0.1.0-dev (protocol 1, commit e94a6812…)`. It takes no arguments.

## mmx help

`mmx help`, `mmx --help`, and `mmx` without a command print the commands,
the flags, and the exit codes, with exit code 0. `mmx COMMAND --help`
prints a command's flags; for the commands that share one set of flags,
as [Flags](#flags) says, it prints the whole set.

## Exit codes

Each command exits with one of these codes. A command's entry above says
which codes it uses.

| Code | Meaning |
| --- | --- |
| `0` | Success. For `dryrun`: nothing would change. For `results` and `status`: MMX showed the run, also a failed run. |
| `1` | The run did not succeed: a node task failed, MMX could not reach a node, or the run stopped with work unfinished (`run`, `resume`, `run-command`). A `dryrun` exits with 1 only when work failed. Also `validate` that found a mistake, and `secrets usage` that found a secret key that is used but not defined. |
| `2` | `dryrun`: something would change. `check-one-time-playbooks`: a node still needs a one-time playbook. `complete-one-time-playbook`: refused, because a node still needs the playbook. `mark-node-work-failed`: refused, because MMX reached the server. `remove-mmx-from-node`: refused, because a node job runs or an owed restore waits on the node. |
| `3` | Unknown or incomplete. `dryrun`: some work could not be checked. `status` and `stop`: a node's state could not be confirmed. `check-one-time-playbooks` and `complete-one-time-playbook`: a node could not be inspected. `mark-node-work-failed`: MMX could not tell whether the server is gone. |
| `4` | The command could not do its job: a wrong argument or flag, an unknown command or module, a missing playbook name, a `--var`, `--limit`, or `--only-fleet-stages` refused before any node is contacted, project files that can't be loaded (except for `validate`), a vault or credential that is not available before a run starts, no saved run that matches, an ambiguous short ID, a run that another MMX release started (`resume`), a completed one-time playbook's name, or an error while MMX saved or printed something on the controller machine. |
| `130` | You pressed Ctrl-C, also during preparation or to leave the live display. |

- For a dryrun, failure counts before incomplete, and incomplete before
  "something would change": a dryrun with a failure and a change exits
  with 1.
- A first dryrun on a new node often exits with 2 or 3: work that an
  earlier node task would make possible shows as `not_checked`. It exits
  with 0 when the node already matches the playbook.
- A run in which some nodes fail exits with 1 after the other nodes
  finish.

## Output

### Standard output and standard error

The results of `dryrun`, `run`, `resume`, `run-command`, `status`, `stop`,
and `results` go to standard output. Lines about the work while it runs
go to standard error. So `mmx run set_up_web_servers > run.out` saves only
the results:

```sh
mmx run set_up_web_servers > run.out 2> run.err
```

`run.err`, the lines written while the run worked:

```text
Run 2ef17ec3
Project: mmx_example   Playbook: set_up_web_servers
Inventory: example (1 node)   Selected: 1 node
Output: nodes (change with --output=nodes or --output=summary)
Progress: 1 not started
203.0.113.11: setting up MMX through verified SSH
Progress: 1 running
  set_up_nginx / web01: connecting
Progress: 1 running
  set_up_nginx / web01: running
Progress: 1 running
  set_up_nginx / web01: running (install_nginx)
Progress: 1 running
  set_up_nginx / web01: running (write_home_page)
```

`run.out`, the results:

```text
set_up_nginx / web01: succeeded
  set_up_nginx / web01 / install_nginx: succeeded, changed
    [{"name":"nginx-common","to":"1.26.3-3+deb13u10"},{"name":"nginx","to":"1.26.3-3+deb13u10"}]
  set_up_nginx / web01 / create_site_directory: succeeded, changed
  set_up_nginx / web01 / create_site_directory: directory "/srv/shop/public"
    checked 1 directories and 0 files; 1 metadata updates
    created the missing parent directories /srv/shop, owner root, permissions_mode 0755
  set_up_nginx / web01 / write_home_page: succeeded, changed
  set_up_nginx / web01 / write_home_page: created "/srv/shop/public/index.html"
    absent -> 14 bytes, permissions_mode 0644, uid 0, gid 0
  set_up_nginx / web01 / shop_site/changes/write_site_file: succeeded, changed
  set_up_nginx / web01 / shop_site/changes/write_site_file: created "/etc/nginx/conf.d/shop.conf"
    absent -> 51 bytes, permissions_mode 0644, uid 0, gid 0
  set_up_nginx / web01 / shop_site/activate/reload_nginx: succeeded, changed — service was not running; started
  set_up_nginx / web01 / shop_site/verify/check_home_page: succeeded, unchanged
  set_up_nginx / web01 / shop_site: succeeded, changed
Run 2ef17ec3: succeeded
1 finished
Saved: /home/you/shop/.mmx/runs/20261115T083232.111676617-2ef17ec3ca73fefe
```

These lines go to standard error:

- The first lines: `Run` with the short run ID; the project, playbook,
  inventory, limit, and number of selected nodes; and the output view.
- The `Progress:` lines, and, with `--output=nodes`, a line under each for
  every node that has work in progress: `connecting`, `running` with the
  current node task, `transferring files`, `node is rebooting`,
  `stop unconfirmed`, or `unconfirmed; work may still be running`.
- Lines about setting MMX up, such as
  `203.0.113.11: setting up MMX through verified SSH`.
- `Selected run ...`, when `results`, `status`, `stop`, or `resume` picks
  a run without a run ID.
- Questions for a password or passphrase, and the messages after
  Ctrl-C.
- Errors, which start with `mmx:`.

### Control characters from nodes

Text that comes from a node, or from a web server a node task called, can
hold terminal control characters, such as the escape character that starts
a command to move the cursor or to write to the clipboard. The text output
shows every control character except newline and tab as a visible escape,
such as `\x1b` or `\u009b`, and every byte that is not valid UTF-8 as
`\x` and its two-digit value. `--json` carries the exact text.

### Progress lines in captured output

A `Progress:` line counts the selected nodes by state, such as
`Progress: 1 finished · 1 running · 1 not started`. The states are
`finished`, `running`, `failed`, `blocked`, `stopped partway through`,
`waiting`, `not started`, and `unconfirmed`. The line adds
`more fleet stages awaiting node or loop item selection` while some fleet
stage's nodes are not known yet.

MMX writes a new `Progress:` line when the counts or the node lines under
it change, at most once a second. MMX doesn't rewrite a line in place, so
a file or a pipe that captures standard error keeps every progress line,
in order, as plain text. A long run may write many of them. To keep only
the results, send standard error elsewhere, such as
`mmx run set_up_web_servers 2> /dev/null`, or use `--json`.

With `--json`, MMX writes no `Run`, `Progress:`, or `Selected run` lines.
Standard error can still get the lines about setting MMX up, questions,
the messages after Ctrl-C, and errors. Standard output gets only the JSON
object.

`mmx check-one-time-playbooks` and `mmx complete-one-time-playbook` write
`Inspecting NAME: ...` lines with the same counts instead.

## JSON output

These commands print one JSON object on standard output with `--json`:

| Command | Object |
| --- | --- |
| `dryrun`, `run`, `resume`, `run-command`, `status`, `stop`, `results` | [The JSON of a run](#the-json-of-a-run) |
| `validate` | [`mmx validate`](#mmx-validate) |
| `nodes` | [JSON of mmx nodes](#json-of-mmx-nodes) |
| `explain` | [`mmx explain`](#mmx-explain) |
| `mark-node-work-failed` | [`mmx mark-node-work-failed`](#mmx-mark-node-work-failed) |
| `check-one-time-playbooks`, `complete-one-time-playbook` | Their entries above |
| `describe-module`, `module-usage` | Their entries above |
| `mmx secrets` subcommands | [Secrets reference](secrets.md#the-mmx-secrets-commands) |

Each of these objects, except some of `mmx secrets`, has `schema_version`,
the version of the object's shape: 2 for `explain`, 1 for the others. A
command that fails before it has a result prints no JSON object: the
message goes to standard error, starting with `mmx:`, and the exit code
says what kind of error it was.

## The JSON of a run

`mmx dryrun`, `mmx run`, `mmx resume`, `mmx run-command`, `mmx status`,
`mmx stop`, and `mmx results` print one run as this JSON object with
`--json`. An excerpt of `mmx run set_up_web_servers --json`, run a second
time on `web01`; `...` marks the parts left out:

```json
{
  "created": "2026-11-15T08:32:38.945904376Z",
  "dryrun": false,
  "entries": [
    {
      "connection_used": "https",
      "architecture": "amd64",
      "node_id": "5f8dfdd497ab384dc004bd9a71d2209868b1bd699d5d7101f678bd316e82bcd7",
      "fleet_stage": "set_up_nginx",
      "node": "web01",
      "job_id": "94f4685fe748cdd03f5ee98b42d84b2e348f74f770beb22aba738e5cffe735e6",
      "node_job_attempt": 1,
      "runner_hash": "81bd50a460a1a383a95fc25ceb4133269b72c42f098b04a3ece41490df500d31",
      "response": {
        "finished": "2026-11-15T08:32:39.128002769Z",
        "facts": { ... },
        "change": "unchanged",
        "id": "94f4685fe748cdd03f5ee98b42d84b2e348f74f770beb22aba738e5cffe735e6",
        "status": "succeeded",
        "results": [
          {
            "tries": [
              {
                "task": "",
                "status": "succeeded",
                "change": "unchanged"
              }
            ],
            "task": "install_nginx",
            "status": "succeeded",
            "change": "unchanged"
          },
          ...
          {
            "http_status_code": 200,
            "response_body": "<h1>Shop</h1>\n",
            "task": "shop_site/verify/check_home_page",
            "status": "succeeded",
            "change": "unchanged"
          },
          {
            "task": "shop_site",
            "status": "succeeded",
            "change": "unchanged"
          }
        ]
      }
    }
  ],
  "entry_point": "/home/you/shop/mmx_project_settings.yml",
  "finished": "2026-11-15T08:32:39.176727561Z",
  "fleet_stages_awaiting_node_selection": false,
  "id": "20261115T083238.945901911-2b38a3bf101e0c7a",
  "inventory": "example",
  "limit": null,
  "only_fleet_stages": null,
  "playbook": "set_up_web_servers",
  "project": "mmx_example",
  "schema_version": 1,
  "secret_inputs_refreshed": false,
  "selections": {
    "set_up_nginx": {
      "playbook_position": [
        0
      ],
      "path": "set_up_nginx",
      "nodes": [
        "web01"
      ],
      "resolved": true,
      "response": {
        "id": "",
        "status": "",
        "results": null
      }
    }
  },
  "short_id": "2b38a3bf",
  "status": "succeeded",
  "stop_requested": false
}
```

### Fields of the run

| Field | Value |
| --- | --- |
| `schema_version` | `1`. |
| `id`, `short_id` | The run's full ID and its short ID: see [Selecting saved runs](#selecting-saved-runs). |
| `project` | The `project_name` from the project settings. |
| `playbook` | The playbook's name. |
| `inventory` | The inventory's name. |
| `entry_point` | The full path of the project's `mmx_project_settings.yml`, or `run-command` for a run of `mmx run-command`. |
| `limit`, `only_fleet_stages` | The values of `--limit` and `--only-fleet-stages`, as lists, or `null`. |
| `created`, `finished` | When the run started and ended, in UTC. |
| `status` | The run's status, such as `running`, `succeeded`, `failed`, or `stopped`. |
| `dryrun` | `true` for a dryrun. |
| `stop_requested` | `true` after Ctrl-C or `mmx stop`. |
| `secret_inputs_refreshed` | `true` when a resume sent secret values again for unfinished work. |
| `fleet_stages_awaiting_node_selection` | `true` while some fleet stage's nodes or loop items are not known yet, such as a fleet stage whose `nodes` read an earlier fleet stage's output. |
| `selections` | One entry for each fleet stage, by its path. See below. |
| `entries` | One entry for each node in each fleet stage, in playbook order. See below. |
| `full_differences`, `full_differences_unavailable` | Only with `--diff=all`: see [`--diff`](#--diff). |

### Fields of each selection

| Field | Value |
| --- | --- |
| `path` | The fleet stage's path. |
| `playbook_position` | The fleet stage's place in the playbook: the places of the loops around it, then its own, counting from 0. |
| `nodes` | The nodes the fleet stage selected. |
| `left_out` | The nodes that `--limit` removed. |
| `resolved` | Whether its nodes or loop items are known yet. |
| `loop`, `items`, `iterations` | For a `for_each_node` loop: `loop: true`, its items, and the result of each iteration. |
| `blocked_by_limit` | Nodes outside `--limit` that a `for_each_node` iteration reached during the run; their work is blocked. |
| `left_out_because_it_selects_no_nodes` | `true` when the fleet stage selects no node and `skip_when_no_nodes_are_selected` leaves it out. |
| `controller_machine_files` | The files that `save_on_controller_machine` wrote. |
| `response` | The fleet stage's own status and error, such as an error in selecting its nodes. |

An empty list or a false flag is left out, except `resolved`.
`playbook_position` and `response` are always present.

### Fields of each entry

An entry is one node in one fleet stage. Each attempt to run that fleet
stage on that node is a **node job**.

| Field | Value |
| --- | --- |
| `fleet_stage`, `node` | The fleet stage's path and the node's name. |
| `node_job_attempt` | `1` for the first node job, one more for each `mmx resume` that ran it again; `0` when no node job started, such as when MMX could not connect. |
| `previous_node_job_attempts` | The earlier node jobs, each with `job_id` and `response`. |
| `job_id` | The node job's ID on the node. |
| `connection_used` | `https`, `ssh`, or `ssh_fallback` (SSH because HTTPS on port 7443 failed); `connection_fallback_reason` then says why. |
| `node_id`, `architecture`, `runner_hash` | The node's identity as MMX saved it, its processor architecture, and the checksum of MMX's node program on it. |
| `housekeeping` | Problems on the controller machine apart from the node tasks' results, such as a difference that could not be downloaded. |
| `response` | What the node reported. See below. |

`response` has:

| Field | Value |
| --- | --- |
| `status` | The node job's status, such as `succeeded`, `failed`, `running`, `stopped`, or `unknown` when MMX could not reach the node. |
| `change` | `changed`, `unchanged`, `would_change`, or `unknown`. |
| `error` | Why the node job failed or could not start, such as `SSH 203.0.113.12: exit status 255: ssh: connect to host 203.0.113.12 port 22: Operation timed out`. |
| `results` | One result for each node task, in order. |
| `outputs` | The fleet stage outputs that the node published. |
| `facts` | The node facts that MMX collected: [Node facts](playbook-file.md#node-facts). |
| `current_task` | While the node job runs: the node task it runs now. |
| `finished` | When the node job ended. |

Each result in `results` has `task` (its path), `status`, and `change`,
and, where they apply: `reason`, `exit_code`, `stdout`, `stderr`,
`stdout_truncated`, `stderr_truncated`, `outputs`, `tries` (each try of a
node task that MMX retries), `items` (each item of a loop), `file_change`
and `directory_change` (what changed, before and after), `diff`,
`http_status_code`, and `response_body`.
[What the results mean](../how-to/preview-and-run-changes.md#what-the-results-mean)
lists the statuses and changes. A secret value never appears: a node task
whose result could hold a secret keeps that result hidden, as
[Secrets in files, output, and history](secrets.md#secrets-in-files-output-and-history)
says.

## Selecting saved runs

`mmx resume`, `mmx status`, `mmx stop`, `mmx results`, and
`mmx mark-node-work-failed` take a run ID, which selects one saved run.

**A full run ID** is the start time in UTC and 16 random hexadecimal
characters, such as `20261115T083238.945901911-2b38a3bf101e0c7a`. It is
also the name of the run's directory in `.mmx/runs/`, which the `Saved:`
line shows.

**A short run ID** is the first eight characters after the `-`, such as
`2b38a3bf`. MMX prints the short ID in its output, such as
`Run 2b38a3bf: succeeded`, and in the commands it suggests, such as
`mmx results 2b38a3bf`.

- The full ID and the short ID each select the run. Another part of the
  ID, such as `2b38a3bf101e0c7a`, selects nothing:
  `mmx: no matching saved run`.
- When two saved runs share a short ID, MMX refuses and asks for the full
  ID: `mmx: run ID 2a33c932 is ambiguous; use the full run ID`.
- A run that MMX no longer keeps, or never had, gives
  `mmx: no matching saved run`, with exit code 4.
  [Where runs are kept, and for how long](../how-to/stop-resume-and-read-a-run.md#where-runs-are-kept-and-for-how-long)
  says when MMX deletes saved runs.

Without a run ID, `results`, `status`, `stop`, and `resume` pick the
latest run of the project and the inventory, and name it on standard
error:

```text
Selected run 20261115T083238.945901911-2b38a3bf101e0c7a, playbook set_up_web_servers.
```

| Command | Picks, without a run ID |
| --- | --- |
| `mmx results`, `mmx status`, `mmx stop` | The latest run of any playbook, also a dryrun. |
| `mmx resume` | The latest run of any playbook that was not a dryrun. |
| `mmx mark-node-work-failed` | Every saved run with unconfirmed work on the node. |

Without a run ID, `results`, `status`, `stop`, and `resume` never pick a
run of `mmx run-command`, and pick only runs made at the project's
current path. `mark-node-work-failed` without a run ID covers every saved
run in the project's `.mmx/runs/`, of any inventory, including runs of
`mmx run-command`.
