# MMX: workflow and CLI

Part of the [main plan](README.md). `docs/changing-mmx/design/workflow.md` owns the user
journey, available commands, options, output, and command behavior. The
[user documentation](../../README.md) describes the current build, and the
[acceptance contracts](../acceptance.md) define the current build's
correctness.

## Start with one machine and one visible change

The prerequisites are a Linux/systemd server and working SSH access with root
or administrative sudo rights, by key or password.
`mmx init` creates project settings, one example inventory, and one example
playbook. `mmx init` also adds `.mmx/` to `.gitignore` and asks the user to
fill in the SSH destination.
`mmx init` does not provision a server.

`mmx_project_settings.yml` holds project settings:

```yaml
project_name: mmx_example
default_inventory: example
```

`inventories/example.yml` holds the nodes:

```yaml
nodes:
  example:
    address: replace-with-your-node
    ssh_user: root
```

`playbooks/example.yml` holds the work:

```yaml
fleet_stages:
  - id: welcome
    nodes: all

    node_tasks:
      - id: create_example_file
        file:
          path: /etc/mmx-example
          content: "Managed by MMX.\n"
          owner: root
          group: root
          permissions_mode: "0644"
```

After filling in the destination:

```sh
mmx dryrun example
mmx run example
mmx run example
```

`mmx dryrun example` sets up the MMX node component if needed, then describes
creating the file. `mmx run example` creates the file. The second run reports
unchanged. Starting with `mmx run example` performs the same setup.
Before remote work, each command prints the project, the playbook name, the
inventory name and its node count, any `--limit` and `--only-fleet-stages`
value, and the selected node count. The selected node count reads "up to"
while some selections resolve during the run. Results give the reason for
each change. There is no approval prompt. To check access first, use the
[connection check](#check-connections).

A fleet stage means "run these tasks on these nodes before proceeding." Roles describe
server configuration; the playbook orders them and actions such as migrations.
The [fleet stages and requirements example](configuration.md#playbooks-contain-ordered-fleet-stages)
shows multi-fleet stage work.

`mmx` finds the project root by looking for `mmx_project_settings.yml` in the current
directory, then each parent directory. There is no `--file` option.
Project-root discovery works from any project subdirectory, including for
`mmx describe-module`, `mmx module-usage`, and `mmx secrets edit`.
A project can hold several playbooks without repeating its settings:

```text
shop/
  mmx_project_settings.yml
  inventories/
    example.yml
    staging.yml
  playbooks/
    bootstrap.yml
    deploy.yml
  one_time_playbooks/
    2026-10-05_remove_old_ruby_manager.yml
    completed/
  roles/<name>/main.yml
  external_modules/<name>/module.star
  external_modules/<name>/module_description.yml
  files/
  secrets_vault.age
  .mmx/
```

`mmx_project_settings.yml` holds the settings every playbook shares; each
playbook holds only its fleet stages and command-line variable declarations.
`one_time_playbooks/` holds [one-time playbooks](#one-time-playbooks): dated
playbooks for work that happens once on every node.
Commands select playbooks and inventories by name, not by path, and there is no default
playbook: `mmx run` and `mmx dryrun` without a name fail and list the
project's playbooks. The [project settings contract](configuration.md#project-settings)
owns the fields and path rules.

`mmx validate`, `mmx explain example`, and `mmx nodes` work locally.
With no playbook name, `mmx validate` checks `mmx_project_settings.yml`,
every inventory, and every playbook. `mmx validate example` checks the named
playbook with the selected inventory.

`mmx explain` requires a playbook name. `mmx explain` expands roles and shows
selected nodes, values and their origins, and the ordered work. Beside a node
task whose module is not core, `mmx explain` shows the module's kind, such as
`greeting (project's own, Starlark)`. The JSON from `mmx explain` names every
task's `module_kind`.

For each fleet stage, `mmx explain` lists what the fleet stage writes on the
controller machine with
[`save_on_controller_machine`](configuration.md#save-outputs-to-files-on-the-controller-machine):
each entry's output or template, the files the entry writes with each
file's node, and the permissions mode. A fleet stage that selects no nodes
still writes a template entry's file, from an empty output collection, so
`mmx explain` lists the file among the fleet stage's writes too.

A path that reads a value known only during the run, or an output entry of a
fleet stage whose nodes are selected during the run, names no file.
`mmx explain` marks the affected entry `files_resolved_during_run`.
Within a playbook, `save_on_controller_machine` is the only setting that
writes on the controller machine. So a reader that judges what a playbook
may do, such as a check before an agent's playbook runs without a person,
needs only `mmx explain`. The JSON from `mmx explain` has `schema_version: 2`
since the JSON lists `save_on_controller_machine` entries. A reader may take
the absence of `save_on_controller_machine` entries as a fleet stage that
writes nothing on the controller machine.

`mmx explain` knows each node's name, as the run does, so an argument or a
`when` that reads `.node.name` is resolved. The node's facts stay unknown.
Beside a node task's arguments, which hold what the playbook wrote,
`mmx explain` shows what MMX derives from the arguments for the node: each
template file rendered with the values the controller machine knows, or why
the template file is not rendered, and ufw rules with one rule for each node
of a `from_inventory_group`.

`mmx nodes` lists the inventory's nodes and groups. `mmx nodes example` also
shows each fleet stage's selection. `mmx dryrun example` connects and
inspects actual state without changing managed resources.
Distinguishing local validation, explanation, and node selection from a
connected dryrun prevents a syntax check from being presented as evidence
about a server.

## Command reference

| Command | Meaning |
|---|---|
| `mmx init` | Create `mmx_project_settings.yml`, `inventories/example.yml`, and `playbooks/example.yml` and the `.mmx/` ignore rule in the current directory. |
| `mmx validate [playbook]` | Check all project settings, inventories, and playbooks locally, or one playbook with the selected inventory. |
| `mmx explain <playbook>` | Show the ordered work, selected nodes, non-secret values with their origins, effective timing/retry settings, and the files each fleet stage writes on the controller machine, locally. |
| `mmx nodes [playbook]` | [Show inventory nodes and groups, plus fleet stage selections when a playbook is named](#show-what-a-command-will-target), locally. |
| `mmx dryrun <playbook>` | Set up nodes if needed, then inspect actual state and report proposed changes. |
| `mmx run <playbook>` | [Run the current playbook](#run-uses-current-files-and-the-full-selection) on the full selection. |
| `mmx resume [run-id]` | [Repeat a recorded run's unfinished work](#resume-uses-the-recorded-run). |
| `mmx run-command --nodes SELECTOR [--run-as-node-user-account NAME] -- COMMAND` | [Run one command across nodes](#run-a-one-off-command-across-nodes) as a recorded run. |
| `mmx status [run-id]` | Collect current results from a run's admitted jobs. |
| `mmx stop [run-id]` | Stop new admission and request a stop after current tasks or node task blocks. |
| `mmx mark-node-work-failed --node NODE [run-id]` | Record a node's unconfirmed work as failed, with the reason that the team says the server is gone, after MMX tried and could not reach the node. |
| `mmx remove-mmx-from-node NODE` | Over verified SSH, stop and remove `mmx-https-api.service`, `/opt/mmx`, `/var/lib/mmx`, `/run/mmx`, and `/etc/mmx` when it is empty, and the managed key's lines in the account's `authorized_keys`; on the controller machine, delete the node's saved identity and managed key files. Refuses (exit `2`) while a node job runs (held `worker.lock`) or an owed restore waits (`mmx-owed-restore-*.service`). Never edits the inventory or playbooks; a second run finds nothing and says so. |
| `mmx results [run-id]` | [Read saved results](#inspect-saved-results-by-node-fleet-stage-and-task) without contacting servers. |
| `mmx describe-module [NAME] [--reference-only]` | [Show a module's whole page, or with `--reference-only` its kind, files, inputs, outputs, example, and dryrun behavior](#look-up-a-module). |
| `mmx module-usage [PLAYBOOK] [--module NAME]` | [Show which modules the project uses, of which kind, and where](#which-modules-a-project-uses), locally. |
| `mmx secrets edit` / `passwd` | [Edit the vault or change its passphrase](#create-and-edit-shared-secrets). |
| `mmx secrets add-random KEY` | [Add a key with a generated value](#add-list-and-compare-secret-keys) without showing the value; with `--replace`, give an existing key a new one. |
| `mmx secrets set KEY` | [Set a key to a value from standard input](#add-list-and-compare-secret-keys), such as a token another command prints, without showing the value. |
| `mmx secrets list-keys` | [List the vault's keys](#add-list-and-compare-secret-keys), never a value. |
| `mmx secrets diff-keys` | [Show which keys were added, removed, or changed](#add-list-and-compare-secret-keys) since the last committed vault, never a value. |
| `mmx secrets usage [KEY]` | [Show where each secret key is used](#find-where-a-secret-is-used), locally. |
| `mmx secrets write-passphrase-file` | [Write the vault passphrase to a file for a coding agent](#create-and-edit-shared-secrets), at a terminal only. |
| `mmx new-one-time-playbook WHAT_IT_DOES` | [Create a dated one-time playbook](#one-time-playbooks) in `one_time_playbooks/`. |
| `mmx check-one-time-playbooks` | [Report which nodes still need each outstanding one-time playbook](#one-time-playbooks), changing nothing. |
| `mmx complete-one-time-playbook NAME` | [Move a one-time playbook to `completed/`](#one-time-playbooks) when no node needs it any more. |
| `mmx version` | Print the version of the `mmx` executable. |

Main help mentions `mmx module-json` on a separate line for executable
modules, outside the user-command list. Executable modules use
`mmx module-json` as a
[JSON helper](modules.md#json-helper-for-bash-executables).

The first commands a user needs are `dryrun`, `run`, and `resume`.
The command reference's remaining commands expose existing state or support
`dryrun`, `run`, and `resume`. Every run has a short displayed ID,
the first eight hex digits of the random part of its full ID; either selects
it. A short ID that two retained runs share is rejected, asking for the full
ID, and JSON results carry both.

### Options shared by commands

| Option | Commands | Meaning |
|---|---|---|
| `--json` | All except `init`, `new-one-time-playbook`, and `version` | Structured results on stdout, with `schema_version`; implies no prompts. `secrets edit`, `passwd`, `add-random`, and `set` print only the vault's path and `status`. |
| `--non-interactive` | All that may prompt | Never prompt; a missing credential is an error. |
| `--output auto\|nodes\|summary` | `nodes`, `dryrun`, `run`, `resume`, `run-command`, `status`, `stop`, `results`, `check-one-time-playbooks` | [Human output](#human-output-is-a-view-of-structured-results) detail. |
| `--diff[=all]` | `dryrun`, `run`, `resume`, `status`, `results` | Text differences: bounded preview, or [complete retained differences](#retained-full-differences). |
| `--vault-password-file PATH` | Commands that unlock or edit the vault | [Passphrase source](#create-and-edit-shared-secrets). |
| `--vault PATH` | `secrets edit`, `passwd`, `add-random`, `set`, `list-keys`, `diff-keys` | Vault file to edit or read. |
| `--node`, `--fleet-stage`, `--task` | `results` | [Filters on saved results](#inspect-saved-results-by-node-fleet-stage-and-task). |

### Choose the inventory, limit, fleet stages, and extra variables

Five options decide what a new invocation targets, with which values, and
over which connection. The configuration specification defines the meanings
of `--inventory`, `--limit`, `--only-fleet-stages`, `--var`, and `--connection`.
The CLI accepts `--inventory`, `--limit`, `--only-fleet-stages`, `--var`, and
`--connection` as follows:

| Option | Accepted by | Meaning |
|---|---|---|
| `--inventory NAME` | `validate`, `explain`, `nodes`, `dryrun`, `run`, `run-command`, `secrets` | Select `inventories/<name>.yml` for this invocation ([inventory selection](configuration.md#choose-the-inventory-for-a-run)). |
| `--inventory NAME` | `resume`, `status`, `stop`, `results` | Choose which inventory's latest run is the default; a run ID overrides it. |
| `--inventory NAME` | `check-one-time-playbooks`, `complete-one-time-playbook` | Select the inventory whose nodes are checked. |
| `--inventory NAME` | `remove-mmx-from-node` | Select the inventory that names the node. `mark-node-work-failed` refuses `--inventory`, because it scans every saved run. |
| `--limit SELECTOR[,SELECTOR]` | `explain`, `nodes`, `dryrun`, `run` | Intersect every fleet stage's selection; inside a `for_each_node` loop, pick the loop's iterations ([targeting with `--limit`](configuration.md#limit-a-run-to-part-of-the-inventory)). |
| `--only-fleet-stages ID[,ID]` | `explain`, `nodes`, `dryrun`, `run` | Keep the named fleet stages, in written order; rejected when a kept fleet stage needs a fleet stage that was left out ([fleet stage selection](configuration.md#run-only-some-fleet-stages)). |
| `--var NAME=VALUE` (repeatable) | `validate`, `explain`, `nodes`, `dryrun`, `run` | Set a variable declared in the chosen playbook's `command_line_vars` ([extra variables](configuration.md#command-line-variables)). |
| `--connection ssh_only` | `dryrun`, `run`, `run-command`, `check-one-time-playbooks`, `complete-one-time-playbook` | Reach every node over SSH for this invocation, as if each node's inventory entry said `connection: ssh_only` ([connection selection](execution.md#bootstrap-automatically-then-use-the-verified-connection)). |

A run's identity is its project, playbook name, and inventory name. Without a
run ID, `results`, `status`, and `stop` select the project's latest playbook
run for the selected inventory, regardless of playbook. Without a run ID,
`resume` selects the project's latest non-dryrun playbook run for the
selected inventory.
The `results`, `status`, `stop`, and `resume` commands each print the
selected run's ID and playbook name.

Run-command runs are selected only by ID. The limit, the fleet stage
selection, the extra variables, and the connection choice are saved with
the run: `resume` reuses the four options and accepts none of the four
options.

`--connection ssh_only` is for a controller machine that cannot reach the
nodes' HTTPS addresses, such as a laptop outside the private network that
the inventory's `https_address` values use. Without `--connection ssh_only`,
each node would wait for its HTTPS attempt to time out on every run and
print the fallback notice. The option accepts only
`ssh_only`: a node whose inventory says
`ssh_only`, such as one behind a jump host, never gets HTTPS from the
command line.

`--var NAME=VALUE` accepts only names declared in the chosen playbook's
`command_line_vars`, and the value is read as `.vars.NAME`. A declared name
cannot also appear in any `vars`.
With no playbook name, `validate` and `nodes` accept no `--var`, because there is
no chosen playbook to declare variable names. `explain` shows the limit and each
extra variable, like [group
variables](configuration.md#inventory-is-a-list-of-nodes-not-a-second-programming-language),
as a value origin. `mmx explain deploy --limit web01` shows one node's
effective work; `mmx dryrun deploy --var release=1.4.3` previews a deployment
whose playbook declares `release`.

`mmx run-command` takes its selection from `--nodes` and accepts no `--limit`; it has
no playbook, so it takes no `--var` and no `--only-fleet-stages`. There are no
`--tags`, `--skip-tags`, or `--start-at-task` options: a project that wants to
apply one part of its configuration keeps one fleet stage for each such part
and names it with `--only-fleet-stages`.

## Show what a command will target

`mmx nodes` answers "what will this touch?" without contacting a server or
unlocking a vault:

```sh
mmx nodes deploy --inventory staging --limit canary
```

```text
Playbook: deploy   Inventory: staging (inventories/staging.yml, 4 nodes)
Limit: canary

Nodes
  db01    db01.example.net    deploy   db
  web01   web01.example.net   deploy   canary, web
  web02   web02.example.net   deploy   web
  web03   web03.example.net   deploy   web

Groups
  canary   web01
  db       db01
  web      web01, web02, web03

Fleet stages (limit applied)
  configure_servers   all   → web01
  deploy_app          web   → web01
  backup_database     db    → none; left out by the limit
```

For the rolling deployment in the
[`for_each_node` example](configuration.md#loop-over-nodes-with-for_each_node),
where each app server's iteration also works on the load balancer `lb01`,
`mmx nodes deploy --limit app02` ends with:

```text
Fleet stages (limit applied)
  rollout[app02]/take_out_of_load_balancer     lb01    → lb01
  rollout[app02]/update                        app02   → app02
  rollout[app02]/put_back_into_load_balancer   lb01    → lb01

Nodes outside --limit that help selected nodes
  lb01   rollout[app02]/take_out_of_load_balancer, rollout[app02]/put_back_into_load_balancer
```

Before connecting, `run` and `dryrun` also print the helper-node section
"Nodes outside --limit that help selected nodes".

`mmx nodes` lists every inventory node with address, SSH user, and groups,
and every group with its members, each sorted by name. When a playbook is
named, `mmx nodes` then shows each fleet stage's written selection and resolved
nodes after the limit and any `--only-fleet-stages`, in playbook order.

Fleet stage loops show each iteration, such as `rollout[app01]/update`, when
the loop list resolves locally. A selection that depends on runtime results
is shown as resolved during the run.

For a [`for_each_node` loop](configuration.md#loop-over-nodes-with-for_each_node)
under a limit, a section "Nodes outside --limit that help selected nodes" lists
each helper node: a node the limit left out that a kept iteration's fleet
stages still run on, such as the load balancer of a rolling deployment.
A fleet stage or `for_each_node` loop with a `when` is marked "narrowed during
the run by `when`", because the condition is checked for each node when the
run reaches the fleet stage or loop.

A limit or fleet stage selection that breaks a requirement makes
`mmx nodes` fail with the same message `run` would print. Values and their origins
are `explain`'s job. Summary output, the automatic choice above ten nodes,
prints counts per group and fleet stage instead of node lists; `--json` always carries
the full records.

`nodes` uses the same selection code as `explain`, `dryrun`, and `run`, so its
answer is the selection those commands use.

## Run a one-off command across nodes

`mmx run-command` runs one command on a selection, as Ansible's ad hoc `-m shell`
does:

```sh
mmx run-command --nodes web -- systemctl status nginx --no-pager
mmx run-command --nodes app --run-as-node-user-account app -- 'bin/cache-clear && bin/warm'
```

`--nodes` is required and takes the same selectors as a fleet stage's `nodes`: a node,
a group, `all`, or a comma-separated union. The words after `--` are joined with
single spaces into one Bash script, which runs exactly like a `shell` task's
`script` with default result rules
([`shell` page](../../reference/modules/shell.md)):
noninteractive Bash as root, success on exit code 0.
`--run-as-node-user-account NAME` runs the Bash script as account `NAME`,
as the shell task's `run_as_user_account` setting does
([privilege](execution.md#privilege-has-one-ordinary-path)). Quote the command
when it contains operators the local shell would otherwise interpret.

The command becomes an ordinary run of one fleet stage with one task.
Run history records the entry point `run-command` instead of the playbook.
`mmx run-command` reads the project's settings and inventory but no playbook,
so a playbook being edited does not block `mmx run-command`.
`mmx run-command` goes through normal setup, connections, job admission,
secrets for connection credentials, and history, with the project's
`max_nodes_at_once`. The run-command run's single fleet stage uses
`on_node_failure: continue`; project `node_task_defaults` apply, except that the
task does not retry.

Because the entry point differs, default `resume`, `results`, `status`, and
`stop` keep selecting playbook runs. The run-command run prints its run ID,
and `mmx results <id>`, `status <id>`, `stop <id>`, and `resume <id>` work on
the run-command run unchanged. Resume repeats
the recorded command on nodes that failed, were unreachable, or never started.
Ctrl-C and node-busy behave as for any run.

Output shows each node's exit code and captured stdout and stderr, grouped like
other runs: in summary mode, nodes with identical output share one block with
their count and names, and distinct output stays separate. JSON carries the
ordinary run records. Output is shown as captured, up to 1 MiB per stream as
for any command, and `mmx results <id>` shows it again.

There is no `mmx run-command` dryrun because MMX cannot determine a bare
command's proposed changes without executing the command. The command
is literal text, recorded in history and shown in output; MMX resolves no
secrets in it, so do not put secret values on the command line. A rolling
restart across a fleet is a playbook fleet stage with `max_nodes_at_once`, not a run-command
option.

### Check connections

`mmx run-command --nodes all -- true` is the connection check.
The connection check sets up the node component where needed, exactly as
`dryrun` does, and changes no managed configuration.
Each node reports success or the access failure that stopped the connection
check on that node, such as an unknown host key, rejected SSH credentials,
missing sudo rights, or an unreachable node.

For an unknown host key or rejected login, `mmx` prints the remedy.
For a missing credential, `mmx` names the required input.
For a sudo refusal, `mmx` shows sudo's error as sudo reports the error.
With the default `connection: https_with_ssh_fallback`, an unreachable HTTPS
address falls back to verified SSH; `connection: ssh_only` uses SSH throughout.
The exit code is `0` only when every selected node succeeded.

## Look up a module

`mmx describe-module NAME` prints one module's whole page, the same page
that the [module pages](../../reference/modules/README.md) hold. What the page is depends
on the [kind of module](modules.md#module-kinds-core-official-extra-and-the-projects-own):

- For a core module, `mmx describe-module` prints the generated page
  `docs/reference/modules/<name>.md`. The core module pages are built into the
  controller program `mmx`. The node program
  `mmx-runner` does not carry the core module pages. For a core module
  without a page yet, `mmx describe-module` prints the reference described
  below.
- An official extra module prints its `README.md` from
  `official_extra_modules/<name>/` beside `mmx`.
- A module of the project's own prints its reference, then
  `external_modules/<name>/README.md` when that file exists.
- A node task form that is not a module (`task`, `task_block`, `role`,
  `show`, `set_vars`) prints its reference, then one line that names the
  section of the [configuration specification](configuration.md) that
  explains the node task form.

When standard output is a terminal, `mmx` shows the page through the
program in `$PAGER`, or through `less -FRX` when `PAGER` is not set. When the
pager cannot start, `mmx` prints the page directly. When standard output is
not a terminal, `mmx` prints plain text directly, including to a pipe, a
file, or a coding agent.

`--reference-only` prints only the module's reference, for every kind:

```sh
mmx describe-module symlink --reference-only
```

```text
symlink: core module, built into MMX
one symbolic link and its target text

Fields
  path                    string             required; absolute
  target                  string             required; stored literally and may dangle; omit with absent
  replace_existing_file   boolean            default false; true: a regular file at path is replaced by the link in one atomic rename; a directory at path is always an error
  state                   present | absent   default present; absent accepts only path and state

Example
  - symlink:
      path: /srv/app/current
      target: /srv/app/releases/1.2.3

Result: status, change
Dryrun: supported; previews the link action, including a regular file the link would replace; not_checked while the parent directory is missing, unless an earlier node task of the dryrun would create it
Retry: bounded automatic retries by default; retry: false turns them off
```

The first line names the module's kind and where its files are: built into
MMX, `official_extra_modules/<name>/` beside `mmx` with MMX's version, or the
project's `external_modules/<name>/` with `Starlark` or `executable`. The
module's summary comes next. Each input then has a type or allowed values,
a required or optional designation, a default, and constraints. The
module's outputs follow, which later node tasks read through
`.node_tasks.<id>`. The examples and the module's dryrun and retry behavior
come last.

For an executable, the dryrun line says that MMX cannot check the module's
dryrun ([the project's own modules](modules.md#the-projects-own-modules)).
Loop results are under `.node_tasks.<id>.items`.
`.this_node_task` is the current node task's own result in `until`,
`retry_when`, `changed_when`, and `failed_when`.

Without a name, `mmx describe-module` lists every module, one line each with
its summary: the core modules grouped by topic, such as "Files and links" or
"Network configuration", then the official extra modules, and, inside a
project, the project's own modules. `mmx describe-module` also lists the node
task forms that are not modules (`role`, `task_block`, `set_vars`, `show`).
The listing includes the settings every node task accepts
(`mmx describe-module task`). `--reference-only` needs a name.

`--json` changes only the format, for agents writing configuration. Without
a name, the JSON listing has `schema_version` and an entry for each module.
Each module entry has `kind` and `summary`. Core module entries also have
`topic`. With a module name, the JSON response has the reference's fields
and a `page` field with the Markdown page.

`--reference-only` leaves `page` out. `mmx describe-module` is local and
reads a project only to find the project's own modules.

A core module's description is kept beside its code, in the same place as
its input type and its checks. A test requires a description for every
registered module, exactly the fields of the module's input type, and valid
examples. The test checks that each input shown as required is an input
validation requires, so the description cannot drift from what validation
accepts. The module's page takes its inputs, outputs, dryrun, and retry
sections from the module's description. The
[documentation map](../documentation-map.md#module-pages) says how a page is
built.

`mmx describe-module task` shows `retry: true`, `retry: false`, or a mapping with
`max_tries`, `pause_between_tries`, `until`, and `retry_when`. `max_tries` is a
positive integer or `unlimited`; `null` is rejected. `unlimited` requires a
finite `time_limit`, set on the node task or inherited from
`node_task_defaults.time_limit`. `time_limit` limits the whole node task, including
every try; `time_limit_per_try` limits one try. Project settings in
`mmx_project_settings.yml` use `node_task_defaults.time_limit`,
`node_task_defaults.time_limit_per_try`, `node_task_defaults.retry.max_tries`,
and `node_task_defaults.retry.pause_between_tries`. The
[task retry contract](execution.md#task-retries-have-explicit-conditions-and-limits)
lists which node tasks retry by default, which try once, and which reject
`retry` or time limits; a module's page says the same for that module in
its section "Retries and time limits".

For readiness checks after starting or reloading a service, use `wait_for`
in the node task block's `verify` phase. `wait_for` waits on the node for a
TCP port, a path, or an HTTP response, and reports unchanged when ready.
When the wait runs out of time, `wait_for` fails and reports the last
observation. In a dryrun, `wait_for` checks once without waiting and reports
`not_checked` if the condition is not ready.

`wait_for` uses `open` or `closed` for a port, and `exists` or `missing` for
a path. URL checks use `expected_http_status_codes` (a list, default `[200]`)
and optional `response_body_contains`. `wait_for` can also wait until a
`script` succeeds, or until a Docker container, Docker Compose project, or
Podman container reports healthy.

`http` makes one request unless the node task sets `retry`. A later node
task can read the `http` result. Both `wait_for` and `http` return
`http_status_code`, `response_body`, and `response_body_json`
for HTTP checks. The `http` input `expected_http_status_codes` defaults to
`[200]`. The node task's own `status` describes success or failure.
The pages of [`wait_for`](../../reference/modules/wait_for.md) and
[`http`](../../reference/modules/http.md) own the fields and request rules for `wait_for`
and `http`.

## Which modules a project uses

Every module is called by its name, so a reader needs one place that shows
which code will run on the nodes and who wrote the code. `mmx module-usage`
identifies which code will run on the nodes and who wrote the code,
locally, from what `mmx validate` resolves:

```sh
mmx module-usage deploy
```

```text
Core modules (built into MMX)
  file              playbooks/deploy.yml:14, roles/nginx/main.yml:8, :21
  systemd_service   roles/nginx/main.yml:30

Official extra modules (Starlark, shipped with MMX 0.1.0)
  incus_instance    playbooks/deploy.yml:40

The project's own modules
  greeting          external_modules/greeting/   Starlark
                    playbooks/deploy.yml:52
  legacy_backup     external_modules/legacy_backup/   executable (Python)
                    roles/backup/main.yml:5
                    MMX cannot check the executable module's dryrun.
                    MMX cannot see what the executable changes, so the module
                    itself must keep the dryrun promise.
```

- Without an argument, `mmx module-usage` covers every playbook, every
  one-time playbook that is not completed, and every role. With a playbook's
  name, `mmx module-usage` covers the named playbook and the roles the
  playbook calls. `--module NAME` shows one module: its
  kind, its files, and every node task that calls it.
- Each module appears once, with its kind, where its files are, and every
  file and line that calls it. A call inside a role is listed at the role's
  line.
- An executable's interpreter is read from its first line, such as
  `executable (Python)`, or `executable (compiled)` for a binary.
- `--json` gives agents the module usage information, with the kinds `core`,
  `official_extra`, `project_starlark`, and `project_executable`.
- A module of the project's own that no node task calls is listed last as
  not used, so a reader sees code that a playbook may pick up later.

Each command answers one question: `mmx describe-module` says what a module
does, its kind, and where its files are; `mmx module-usage` says where
modules are used; and [`mmx explain`](#show-what-a-command-will-target)
shows what a run will do, with a kind label beside each node task whose
module is not core, such as `greeting (project's own, Starlark)`.

## Create and edit shared secrets

For secrets stored encrypted with the project, use the
[shared age vault](configuration.md#shared-age-vault). The vault is a file
encrypted in the age format and protected by a passphrase shared by
teammates.
`mmx secrets edit` creates or edits the vault through the user's editor.
Teammates share the encrypted vault file and get the vault's passphrase
from their password manager.

Projects using only
[environment-variable inputs](configuration.md#environment-variable-inputs)
need no vault. The vault is a list of keys and values. A key is the
name of one secret, such as `database_password`.

`run`, `dryrun`, `resume`, and `run-command` unlock the vault once per invocation when
they need vault-backed secrets. Creating, editing, and unlocking use the same
passphrase sources, in this order:

1. `--vault-password-file PATH`.
2. `MMX_VAULT_PASSWORD_FILE`, naming a file on the controller machine.
3. `MMX_VAULT_PASSWORD`, for CI secret storage.
4. A hidden interactive prompt.

For example, keep the passphrase in a private file outside the project:

```sh
export MMX_VAULT_PASSWORD_FILE="$HOME/.config/mmx/production-vault-password"
```

Each passphrase-source file holds one nonempty passphrase with an optional
final newline. MMX reads passphrase-source files as data, never as commands.
A selected source that is unreadable, empty, or wrong is an error; MMX does
not fall through to a lower source or prompt. With
no terminal, `--non-interactive`, or `--json`, a missing source is an error
naming these options. MMX does not accept a passphrase as a command-line
argument, and the passphrase never enters the project, run history, or node
jobs. Commands needing only environment secrets do not unlock a vault.

`mmx secrets edit` validates the edited YAML and encrypts the YAML before
atomically replacing the vault. A failed edit keeps
the last good vault file. Editor plaintext lives in private temporary
storage and is removed when the edit ends, including on
Ctrl-C, SIGTERM, or a closed terminal. `mmx secrets passwd` changes the
passphrase without changing the values; `--new-vault-password-file PATH` makes
it noninteractive.

`mmx secrets write-passphrase-file` lets a person unlock the vault once for
a coding agent, so that the passphrase never enters the agent's
conversation. It runs only when standard input and standard output are a
terminal, always asks at the hidden prompt, and writes the passphrase as one
line, mode `0600`, to `~/.config/mmx/vault-passphrase-PROJECT_NAME` in one
atomic step, creating `~/.config/mmx` with mode `0700`. The agent then sets
`MMX_VAULT_PASSWORD_FILE` to that file. The file lives outside the project
so that it is never committed, and it is named after the project so that
several projects on one controller machine keep apart. A vault that is not
the project's own, selected by an inventory's `vault_file` or `--vault`, can
have another passphrase, so its file name adds the vault's file name without
`.age`; two such vaults with the same file name in different directories
share one file, and the second write replaces the first, which a later
command reports as a wrong passphrase. For an existing vault the command
writes only a passphrase that opens the vault. With no vault yet it asks for
a new passphrase twice and creates an empty vault with it: a vault created
later by `mmx secrets set` at a prompt could get a different new passphrase
than the file holds.

`secrets_vault.age` at the project root is the default vault; project or
inventory `vault_file: PATH`, or `--vault PATH` on the secrets commands, selects
another. `--inventory` on the secrets commands chooses which inventory's
`vault_file:` setting applies. An inventory's `vault_file` path resolves relative to
that inventory file; the project's `vault_file` path resolves relative to
the project root.

### Add, list, and compare secret keys

Four commands work on the vault's keys without opening an editor and without
ever printing a value. `add-random`, `set`, `list-keys`, and `diff-keys` each
use the passphrase sources above to unlock the vault, so agents and CI can
run the four commands.

```sh
mmx secrets add-random session_cookie_secret --length 48
cloudflared tunnel token shop | mmx secrets set cloudflare_tunnel_token
mmx secrets list-keys
mmx secrets diff-keys
```

`mmx secrets add-random KEY` adds a new key whose value is random letters and
digits, 32 characters unless `--length N` says otherwise.
`mmx secrets add-random` does not display the value and fails if the key
exists. A password that must be generated once and stay the same is one
command, and the password is in version control with the vault.

`mmx secrets add-random` adds the key as a new last line of the vault's text,
so existing comments and key order stay unchanged. With `--replace`,
`mmx secrets add-random` gives an existing key a new random value in the
key's place, so one invocation also rotates a generated password.
`--replace` has the same meaning in both `mmx secrets add-random` and
`mmx secrets set`. Without `--replace`, a second run never replaces a value
by accident.

When the project has no vault yet, `mmx secrets add-random` creates the vault
and takes the new passphrase as `mmx secrets edit` does. `KEY` is a name that
a playbook can read as `.secrets.KEY`. `mmx secrets add-random` refuses a key
that the project's `secrets_from_env` binds to an environment variable.
A run reads an environment-bound key from the variable and would never read
the key's value in the vault.

`mmx secrets set KEY` puts a value that another command produces into the
vault, such as an API token or the vault passphrase of another machine's
file. The value never passes through a conversation, a command line, or an
editor, so a coding agent can store a token it cannot see. The command reads
the value from standard input and removes exactly one final line ending,
`\n` or `\r\n`, so `echo` and `printf %s` give the same value; a second
line ending stays part of the value.

`mmx secrets set` refuses an empty value, which almost always comes from a
command that printed nothing.
`mmx secrets set` also refuses a value that is not UTF-8 text, because the
vault is YAML text.

When standard input is a terminal, `mmx secrets set` asks for the value with
a hidden prompt instead, as for the passphrase. `--non-interactive` and
`--json` turn the prompt off, and `mmx secrets set` then fails. A value from
a pipe leaves no terminal on standard input. The passphrase then comes from
`--vault-password-file` or the environment, as for any command without a
terminal.

`set` refuses a key the vault already has, unless `--replace`; with
`--replace`, a key the vault does not have is added. A new key becomes a new
last line, as with `add-random`. A replaced value stays where its key
stands: the key's lines, from its own line to the next key without the blank
lines and comments at the start of a line before that key, become the new
lines. Comments above and below stay; a comment on the key's own line goes
with the old value. `set` writes a value of one line in double quotes with
YAML's escapes, and a value of several lines as a literal block (`|`), as a
person writes a certificate. When the vault's text is not a list of
`key: value` lines, such as keys inside `{ }`, `set` changes nothing and
says to use `mmx secrets edit`. `KEY`, the refusal of a key that
`secrets_from_env` binds, `--vault`, `--inventory`, and a new vault work as
for `add-random`.

On success, `mmx secrets set` prints nothing. With `--json`,
`mmx secrets set` prints the vault's path and `status`. `mmx secrets set`
never prints the value, including in an error. Before reading the value,
`mmx secrets set` unlocks the vault and checks the key, so a refusal comes
before the prompt.

`mmx secrets list-keys` prints the vault's top-level keys, sorted, one on each
line. With `--json`, `mmx secrets list-keys` prints `vault` and the list
`keys`.

`mmx secrets diff-keys` compares the vault file in the working directory
with the same file in the last Git commit. The command prints
three lists: keys added, keys removed, and keys whose value changed.
Use `mmx secrets diff-keys` to check a vault edit before committing the edit:

```text
Vault: secrets_vault.age, compared with the last commit

Added:    session_cookie_secret
Removed:  old_cdn_token
Changed:  database_password
```

A list without a key shows `(none)`, so the output always has the three
lines. With `--json` the command prints `vault` and the lists `added`,
`removed`, and `changed`. `mmx secrets diff-keys` exits `0` whether or not a
key differs, because the three lists report the comparison.

`mmx secrets diff-keys` decrypts the working-tree vault and the committed
vault with the current vault passphrase. `mmx secrets diff-keys` reads the
committed vault with the `git` program. If the project is not in a Git
repository or the vault has no committed version, `mmx secrets diff-keys`
fails. `mmx secrets diff-keys` also fails if the committed vault does not
open with the current vault passphrase. The error names the reason for the
failure.
MMX keeps no list of keys outside the vault; a team that wants one in version
control commits the output of `list-keys` as a file of its own.

### Find where a secret is used

`mmx secrets usage` answers "where is this secret used?" across the whole
project, including through role inputs, where a text search stops at the
role call. `mmx secrets usage` is local and reads no values:

```sh
mmx secrets usage database_password
```

```text
database_password   vault secrets_vault.age
  playbooks/deploy_platform.yml:54   shell env DATABASE_PASSWORD
  playbooks/site.yml:88              role database, input root_password
    roles/database/main.yml:31       file /root/.my.cnf (content)
```

For each key, `mmx secrets usage` prints the source (the vault file or the
environment variable bound in `secrets_from_env`) and every place that
consumes the key, ordered by file and line. `mmx secrets usage` follows an
input declared `secret: true` into the role down to the consuming node task.

A key that a playbook reads and nothing defines has the source `not defined`.
A key that nothing reads says `(never referenced)`. Without a key,
`mmx secrets usage` prints every key, then two lists:

```text
Referenced but not defined:   session_cookie_secret (playbooks/site.yml:140)
Defined but never referenced: old_cdn_token
```

`mmx secrets usage` reads every playbook, including one-time playbooks that
are not completed, with the selected inventory's vault. A secret that the
inventory names for a node's connection, such as a sudo password, is a use
like any other. The command unlocks the vault only to learn which keys the
vault defines. A project whose vault file does not exist needs no
passphrase. `mmx secrets usage`
exits `1` when a referenced key is not defined, so CI can use
`mmx secrets usage` to check for missing secret keys. With `--json`,
`mmx secrets usage` prints the list `keys`, each with its `key`, its
`source`, and its `uses`, and the lists
`referenced_but_not_defined` and `defined_but_never_referenced`.

MMX knows before execution that a fleet stage selects no nodes when two
conditions hold. The fleet stage's group is empty in the selected inventory,
and the fleet stage sets `skip_when_no_nodes_are_selected`. No run with the
selected inventory then needs secrets read only by fleet stages known to
select no nodes
([secrets a run needs](configuration.md#secrets-a-run-needs)).

A key read only by fleet stages known to select no nodes is still listed
with all the key's uses, below the line
`(secret key not needed with the selected inventory: only fleet stages that select no nodes, or whose when is false on every node, read the key)`.
A key read only by fleet stages known to select no nodes is excluded from
`Referenced but not defined` and does not make `mmx secrets usage` exit `1`.
So a team without a staging server needs no placeholder for the staging
server's secrets. In JSON, the entry for a key read only by fleet stages
known to select no nodes has
`only_read_by_fleet_stages_left_out_before_the_run: true`.

A key is needed when a fleet stage that may select nodes reads the key, or
when a node's connection uses the key.

## Set up nodes automatically

`mmx dryrun <playbook>`, `mmx run <playbook>`, and `mmx run-command` install and
configure the MMX node component when needed; there is no separate preparation
command. Setup progress is shown, then the requested command continues. Repeating a command
reuses a correct installation or finishes an interrupted one.

When no node job is active on the node being updated, setup installs the
node component supplied with the controller program's release. Otherwise,
setup reports the node busy. The [execution
contract](execution.md#node-updates-follow-the-controller-release) owns
installation.

Setup uses verified SSH with a key or password. Root installs directly; an
administrative user uses sudo, with a password when required. SSH and sudo
passwords come from [secrets](configuration.md#secrets) or a prompt, and may
differ. Inventory fields use a whole-value secret reference, such as
`ssh_password: "{{ .secrets.ssh_password }}"`; `sudo_password` also uses a
whole-value secret reference.

The controller process resolves SSH and sudo passwords in memory.
SSH and sudo passwords never enter saved inventory, node jobs, or results.
With no terminal, `--non-interactive`, or `--json`, a missing credential is
an error instead of a prompt.

Password-only first use creates one reusable SSH key, so later commands
need no password.

Dryrun may install MMX itself, but it only inspects the playbook's declared
resources. Prerequisites the playbook would install remain proposed changes.
Checks that depend on prerequisites proposed by the playbook report not
checked.

The node HTTPS API process listens on port 7443. MMX never changes a node's
firewall to open port 7443. When the HTTPS check after setup fails, the
command continues over SSH and says so once for each such node, in its
output and in the structured results. The notice says what MMX saw and what
the node, reached over SSH, reports about itself, and guesses no cause:

```text
app02: MMX could not connect to https://10.20.0.2:7443 (connection timed out).
mmx-runner, MMX's HTTPS service on this node, is running. MMX is using SSH for
this node. To use HTTPS, check that the controller machine can reach 10.20.0.2
on TCP port 7443: the network between them may be down, or a firewall may
block the port. To make SSH the stated choice instead, set
connection: ssh_only on the node in the inventory, or pass
--connection ssh_only when this controller machine cannot reach the node's
HTTPS address.
```

When ufw or firewalld is active on the node and none of its rules names port
7443, the notice says that too. The notice repeats on each command until HTTPS works
or the node says `ssh_only`. The
[execution contract](execution.md) owns the check and the
recorded connection.

Later commands authenticate over HTTPS with the administrator's existing
SSH key or agent, without another SSH login or sudo prompt;
[Administrator keys](execution.md#administrator-ssh-keys-authorize-https) and
the [privilege model](execution.md#privilege-has-one-ordinary-path) define who
has access.

## Run uses current files and the full selection

`mmx run deploy` executes the current `playbooks/deploy.yml` from the
beginning on every selected node. The inventory, limit, and extra variables
given to `mmx run deploy` decide the selection and values. Earlier failures
do not narrow the selection, and MMX has no recovery menu.

After a failure, fix the problem and run again. Current edits apply to all
selected nodes, including earlier successes. Node tasks that manage a desired
resource state recheck actual state, so correct resources report unchanged. The
[service hashes](execution.md#service-reloads-follow-configuration-hashes)
detect configuration a service has not yet loaded. For example, if a run writes
nginx's configuration and fails before the reload, the next run finds the file
already correct but still validates and reloads nginx. No force flag is needed.

A new run uses its own failure handling and cleanup. The new run does not
replay old cleanup or match old task names. Custom commands execute as written, except
a `shell` task that says `only_when_tracked_inputs_changed: true`, which runs
when its node task block's files changed and is still owed after an
interrupted run, like a service reload. See
[cleanup](execution.md#cleanup-is-local-to-the-attempt) and
[recovery](execution.md#ordinary-fixes-recover-without-identity-matching).

Each node admits one job at a time, so repeating a command while an earlier
job still runs gets "node busy" rather than overlapping work.
`mmx status <run-id>` retrieves the latest results of the node job still
running from the earlier command.

A canary rollout is the same command twice, with a check between:

```sh
mmx run deploy --limit canary
mmx run deploy
```

Humans and agents use the same commands and selection rules:

```sh
mmx run deploy --non-interactive --json
mmx resume --non-interactive --json
```

No terminal, `--non-interactive`, or `--json` means no prompts. JSON results
carry `schema_version`, stable run/node/fleet stage/task identities, and
statuses, without progress decoration. Errors carry enough context
and an explicit next command for an agent to recover. Notices, such as an
earlier run that still needs `mmx status`, go to stderr, not into the JSON
result. Agents never need to drive a menu or infer success from prose.

## Resume uses the recorded run

`mmx resume` selects the latest non-dryrun playbook run for the current
MMX project and selected inventory, whichever playbook the run used, and
prints the run ID and playbook name. `mmx resume` repeats failed,
interrupted, unstarted, and blocked work for each fleet stage and node,
keeping successful node results. If the selected playbook run
succeeded, there is nothing to resume; MMX does not search back for older
failures. `mmx resume <run-id>` selects an older run or a run-command run explicitly.
A dryrun cannot be resumed.

Resume uses the recorded bundle: playbook, template and source bytes,
inventory, limit, fleet stage selection, extra variables, node task time
limits, and node task retry policies. Edited files do not affect resumed
execution. Use `mmx run <playbook>` for current files.
Each recorded entry for a fleet stage and node keeps the runner build
the entry already used on that node. Secret references are resolved again.

Resume first reconciles node jobs that may still be running.

A resumed node job starts its fleet stage's node tasks from the beginning,
with ordinary failure handling and cleanup. Node tasks that manage a
desired resource state recheck actual state. Nodes that finished the fleet
stage being resumed are not rerun. MMX restores the finished nodes'
[published outputs](execution.md#completed-outputs-survive-resume) for
consumers. Failed prerequisites run again in written order, then the work
blocked by the failed prerequisites.

The recorded `on_node_failure` policy and
`failed_nodes_allowed` apply, including to nodes and fleet stages a stop left
unstarted; an old failure does not trigger a new stop. A node whose machine
was replaced fails its fleet stage. Use `mmx run <playbook>`.

Resuming updates the selected run's results and keeps earlier
attempts. A new run does not rewrite historical failures. A run whose
history has expired cannot be resumed. `mmx run <playbook>` still reconciles
current files.

## Inspecting and stopping a run

`mmx status` contacts a run's admitted jobs and collects their latest results,
including work that continued after the terminal disconnected. `mmx stop` sends
the same stop as Ctrl-C from another terminal. `mmx status` and
`mmx stop` start no new work. Both `mmx status` and `mmx stop`
default to the latest playbook run for the current project and selected
inventory, and print its ID and playbook name; a run ID selects any retained
run, including a run-command run. `mmx results` reads saved results without contacting
servers.

The first Ctrl-C stops new admission and asks active servers to stop after
their current task or whole node task block, which still completes its validation,
activation, verification, failure handling, and cleanup. MMX says so:

```text
Stop requested. No new work will start.
Each node will finish the current node task or node task block.
Waiting for the nodes to stop. Press Ctrl-C again to leave the display.
```

Server counts keep updating. Output selected with `--output nodes` also
shows each active server's current task. A second Ctrl-C leaves the display:

```text
Leaving the display. Some servers may still be finishing work.

Check node progress:     mmx status
Repeat the stop request: mmx stop
```

`mmx stop` returns after contacting the jobs, showing (per node in `nodes` or
JSON output) which acknowledged, are still finishing, or could not be confirmed.
The final summary distinguishes finished, failed, blocked, stopped, and
unstarted work. The final summary lists loop iterations that never started
with their cause, and prints commands to inspect or resume that exact run,
or run the current playbook. Nothing resumes automatically. The [execution
contract](execution.md#disconnects-stopping-and-reboots) owns job lifetime and
stop delivery.

A server that is gone leaves its work unconfirmed for good: no node can
answer for it, and a run with unconfirmed work never leaves history. `mmx
mark-node-work-failed --node NODE [run-id]` records that work as failed with
the reason `the team says this server is gone`, in one run or in every
saved run, trying every run. It refuses while MMX can reach the node (exit
`2`), records a replaced machine as `mmx status` does, records a run whose
saved SSH configuration OpenSSH cannot read (no command can reach the node
with it), and leaves the work as it was when MMX cannot tell (exit `3`);
the exit code is the most serious run's. Reason: an unconfirmed run that suggests `mmx
resume` sends a person or a coding agent after work that can never finish,
and teams that replace dead servers or destroy temporary ones meet it
often; the name says what the command does, and the refusal keeps it
from hiding work that a reachable node can still report.

## One-time playbooks

Some work must happen once on every node and never again.
Examples include removing a retired program after a migration or stopping
an agent the team no longer uses. A
[one-time playbook](configuration.md#one-time-playbooks)
holds such work.
The one-time playbook lives in `one_time_playbooks/`, has a dated name, and
runs only when a command names the playbook. Three commands carry the
one-time playbook from the first draft to the project's history.

```sh
mmx new-one-time-playbook remove_old_ruby_manager
```

creates `one_time_playbooks/2026-10-05_remove_old_ruby_manager.yml`, dated
with the controller machine's current date, and prints the path. The file
holds one example fleet stage to replace, and is valid as it is created.
`WHAT_IT_DOES` is lowercase words joined by underscores and starts with a
letter, because it is also the ID of the file's example fleet stage. The
command refuses a name that `one_time_playbooks/` or `completed/` already
has.
Filled in for an inventory whose group `app` once used the Ruby manager
rbenv, the playbook reads:

```yaml
fleet_stages:
  - id: remove_old_ruby_manager
    nodes: app

    node_tasks:
      - directory:
          path: /opt/rbenv
          state: absent
          also_delete_everything_inside: true

      - file:
          path: /etc/profile.d/rbenv.sh
          state: absent
```

Preview and run the playbook that removes rbenv like any playbook, with
`--limit` for a first node if wanted:

```sh
mmx dryrun 2026-10-05_remove_old_ruby_manager
mmx run 2026-10-05_remove_old_ruby_manager
```

A node that was down when the one-time playbook ran still needs the work.
`mmx check-one-time-playbooks` finds such nodes:

```text
Inventory: example (inventories/example.yml)

2026-10-05_remove_old_ruby_manager
  done on 3 of 4 servers
  not known on 1 server: app03 (MMX could not connect: SSH app03.example.net: exit status 255: ssh: connect to host app03.example.net port 22: Connection timed out)
2026-09-12_stop_old_metrics_agent
  done on all 12 servers
```

For each one-time playbook outside `completed/`, the command connects to the
nodes the playbook selects in the chosen inventory and inspects them exactly
as `mmx dryrun` does. `mmx check-one-time-playbooks` changes nothing and
keeps no record of its own. The answer comes from the nodes each time. A
node needs the playbook when its dryrun reports a proposed change. A node
that cannot be reached, or a node task whose need cannot be inspected, is
reported as not known, with the reason, and prevents completion. If the
node also has a proposed change, the node is reported as still needed.
The reason is one line. For a node that MMX could not connect to, and for a
node job that did not succeed, the reason
carries the error that the dryrun recorded. A `shell` task can be inspected
when the task has a guard, a `dryrun` command, or
`run_only_once_marker_file`; use the marker file for a raw command that
leaves no other trace. A one-time restart of a service is `systemd_service`
with `restart_now: true`, which
needs a `run_only_once_marker_file` because a restart leaves no other trace
([execution design](execution.md#a-command-that-follows-a-change-has-the-same-record)).
The cleanup modules, such as `systemd_journal_vacuum` and
`delete_old_files`, need none: their dryrun is unchanged once nothing is
left to delete.

Each node the playbook selects is in one of three states. The report lists
`done`, `still needed`, then `not known`, leaving out a line that has no node:

```text
2026-10-05_remove_old_ruby_manager
  done on 2 of 4 servers
  still needed on 1 server: app02
  not known on 1 server: app03 (MMX could not connect: SSH app03.example.net: exit status 255: ssh: connect to host app03.example.net port 22: Connection timed out)
```

The inspection of each playbook is an ordinary dryrun, saved in the run
history like any other. With `--output nodes` the report names every node
with its state and adds the dryrun's run ID, and `mmx results RUN_ID --diff`
then shows what a node still needs. `--output summary` prints only the
counts. With `--json` the command prints `inventory` and the list
`one_time_playbooks`, each with its `name`, `run_id`, and the lists `done`,
`still_needed`, and `not_known`, whose entries give the `node` and the
`reason`.

A playbook has inspected nothing if the playbook selects no node of the
chosen inventory or MMX cannot load the playbook with the chosen inventory.
The report says that nothing was inspected under the playbook's name.
The playbook counts as not known.

A playbook also counts as not known if the dryrun could not resolve a fleet
stage's nodes or left a loop iteration without a node job. No node entries
from the unresolved selection or iteration appear in the three lists.
The report describes the uninspected work under the playbook's name,
whatever states the dryrun established for nodes with node jobs.
In JSON, uninspected work is reported in the playbook's
`not_inspected_reason`, outside the node lists.

```sh
mmx complete-one-time-playbook 2026-09-12_stop_old_metrics_agent
```

runs the dryrun inspection used by `mmx check-one-time-playbooks` for
`2026-09-12_stop_old_metrics_agent` alone. If any node still needs the
named playbook or is not known, the command refuses and lists the affected
nodes. The command also refuses when the dryrun selected no node, because
the dryrun has then inspected nothing. A playbook that MMX cannot load
with the inventory, such as one whose node selection is
empty there, is a command error with exit code `4`, as for every command
that names a playbook. Otherwise `mmx complete-one-time-playbook` moves the
named playbook's file to `one_time_playbooks/completed/` and adds a first line:

```yaml
# Completed 2026-10-19: done on all 12 servers.
```

A playbook in `completed/` cannot be run or dry-run, and the check ignores
it; the error says to move the file back to `one_time_playbooks/` to make it
runnable again. MMX deletes nothing, so `completed/` is the history of the
project's one-time work. `mmx check-one-time-playbooks` and
`mmx complete-one-time-playbook` both take `--inventory`. A project with
several inventories checks each inventory before completing a playbook,
because the command inspects only the nodes of the inventory the command
was given. When the project has other inventories,
`complete-one-time-playbook` names the other inventories in a
note, so that nobody takes one inventory's answer for the whole project's.

## Human output is a view of structured results

**Normality compresses. Exceptions expand.** For a few nodes, show individual
outcomes; for a fleet, show counts, exceptions, and grouped changes. JSON always
keeps individual records.

Human commands accept `--output=auto|nodes|summary`. Auto uses `nodes` for up
to ten selected nodes and `summary` above that, choosing once per command and
printing how to override it. When the selection depends on earlier results,
the inventory size is the upper bound. Summary groups identical task outcomes
with their counts and node names; distinct errors and rendered differences stay
separate. Live counts refresh about once a second.

When a failure policy stops admission, output names the triggering fleet stage and node
and whether it stopped the fleet stage or the run. Stopped work is never reported as
skipped. A failed task shows its exit code and captured output.

Explicit [`show` tasks](configuration.md#show-messages-and-runtime-values)
display their content even in summary mode, with node, fleet stage, task, and loop
context. Values stay structured in saved and JSON results, and secrets are
suppressed before display or saving.

File dryruns show the path, action, metadata changes, and link targets.
`--diff` adds text differences to run, dryrun, resume, status, and results output.
Secret-consuming tasks, removals, binary files, and files not world-readable
before or after the change show no contents. A file is world-readable when
its mode has the read bit for others and every existing directory above it
has the execute bit for others: a `0644` file in a `0700` directory is
private. A file that `save_on_controller_machine` writes counts only its own
mode, since a project usually sits in a private home directory. Previews stop at 16 KiB or 200
lines, and files over 64 KiB get no preview; truncation is stated.

Diff suppression uses the current task's secret reads and the read
permissions of the file and the directories above it. MMX cannot know whether existing world-readable bytes once held
a secret. A task that no longer reads secrets can show existing
world-readable file contents in a diff. Keep secret files private rather
than relying on the files' content history.

Recursive metadata and directory-copy tasks show one summary: entries checked,
entries changed or needing change, and skipped symlinks, special files, and
other-device mount points. `--diff` adds up to ten example changes. JSON carries
the same summary as `directory_change`.

Every run prints its run ID and saved path; a failed run also prints a
`mmx results <run-id>` command that still selects it after newer runs.

### Retained full differences

`--diff=all` shows complete text differences from local history. Nodes record a
full difference for inputs up to 1 MiB each, within 8 MiB per node job;
identical differences are stored once, and directory copies keep them for their
first ten examples. Beyond the diagnostic limits, the preview and a reason
are kept. The diagnostic limits never limit the files themselves, and the
retained differences are not backups. The content suppression above
applies first.

Differences are recorded during every run, whatever the display flag. The
controller process downloads a finished result's differences when saving
the result. A failed download is reported as housekeeping.
`--json --diff=all` adds a `full_differences` object keyed by SHA-256; a
difference not available locally is noted in human output and listed in
`full_differences_unavailable`. Task
outcomes are unaffected and nothing is rerun.

## Inspect saved results by node, fleet stage, and task

Expect saved-result inspection to be used often by an agent working with a
user. Humans and agents use the same records and filters; `--json` exposes
the records without terminal-text parsing.

`mmx results` selects the latest playbook run in the current project and selected
inventory, whichever playbook it used, including a successful run.
`mmx results` first prints, on standard error, the selected run's full ID
and playbook name. A run ID selects any retained run, including a
run-command run, and then no such line is printed.

```sh
mmx results --node app02 --fleet-stage update_app --task check_ready
mmx results <run-id> --node app02 --json
```

Filters combine. `--task` matches a task's recorded path, including role and
loop context, such as `web_server/nginx/changes/task_2`. Records include
status, change, retained tries, exit code and output, published outputs,
skip conditions, and the prerequisite that blocked work. Ordinary task results
are recorded automatically; authors need no `show` task to inspect a failure.
The first command above, for a `wait_for` node task whose `wait_at_most` is
`30s`, displays:

```text
Selected run 20261016T101500.000000000-3f9a2c71d0e4b815, playbook deploy.
Output: nodes (change with --output=nodes or --output=summary)
update_app / app02: failed
  check_ready: wait_for ended after at most 30s: context deadline exceeded; last observation: HTTP status 503; expected 200
  update_app / app02 / check_ready: failed, unchanged — wait_for ended after at most 30s: context deadline exceeded; last observation: HTTP status 503; expected 200
    http_status_code: 503
    response_body: starting
Run 3f9a2c71: failed
1 failed
Saved: /home/barbara/shop/.mmx/runs/20261016T101500.000000000-3f9a2c71d0e4b815
Inspect:                mmx results 3f9a2c71
Resume unfinished work: mmx resume 3f9a2c71
Start a new run:        mmx run deploy
```

The line `update_app / app02: failed` is the node job, and the line below it
is the failure that ended it. The line that starts
`update_app / app02 / check_ready` is the node task: its status, its change,
and its reason, followed by the HTTP status and response body it last
observed. The counts after `Run 3f9a2c71` cover only the records the filters
keep. Without `--fleet-stage` and `--task`, the output also lists the later
fleet stages blocked on `app02` by the `check_ready` failure, each with the
reason for being blocked.

Saved results show history: they do not reevaluate current project files, resolve
secrets, or contact servers.

## Saved run history

Each run saves its definitions, node selection, limit, fleet stage selection, and extra variables once
in a private run directory, plus a small status record and a progress record
per node/fleet stage written as work proceeds. `mmx results` and `mmx resume`
read the saved definitions, status record, and node/fleet stage progress
records to reconstruct the saved run's state. History lives in `.mmx/` at
the project root. MMX sets the permissions of every file it saves there, so
each command that uses history first sets the permissions of
`.mmx/history.lock`. A file system that refuses file permissions, as a Windows
drive under WSL can, would otherwise fail the first save with a bare chmod
error; instead the command stops before it starts its work, names the
directory, and says to move the project to a Linux file system. A file system
that accepts chmod and keeps no permissions, as a Windows drive under WSL
usually does, is not refused: `.mmx/` holds no secret (run records leave
secrets out, and the managed SSH keys live in `~/.ssh/mmx/`), and whoever can
change `.mmx/` there can also change the playbooks beside it. Development
and test harnesses can set `MMX_STATE_DIR` to select another history directory and `MMX_RUNNER_DIR` to use local
runner binaries. `MMX_STATE_DIR` and
`MMX_RUNNER_DIR` are absent from user help. Secrets are suppressed before
anything is saved.

Saved records are bounded. Each command stream, HTTP body, and module result is
captured up to 1 MiB per try and marked when truncated. Each node job
keeps up to 16 MiB of task records; a record that does not fit keeps its status,
change, exit code, reason (up to 4 KiB), and file/directory summary, and states
in `omitted` what it dropped. A repeated task keeps its first and latest nine
tries and counts the rest in `tries_omitted`. The 1 MiB capture limit
applies to the result itself, and conditions reject truncated values. The
16 MiB task-record budget, 4 KiB reason limit, and ten retained tries apply
only to saved records.

Run history retention on the controller machine is one setting in
`mmx_project_settings.yml`:

```yaml
run_history_retention: 24h
```

The default is `24h`; positive durations such as `90m` or `7d`, or `forever`,
are accepted. Age runs from the end of the latest attempt. Ordinary run,
resume, and inspection commands remove the expired runs of their own
selection: the runs with the same project, entry point, inventory, and
playbook as the run the command used. The commands also remove cached runner
binaries that no retained run uses. Cleanup defers if history is in use. A
saved run that MMX cannot read stops cleanup before anything is removed,
because the run may use a cached runner binary; the message names every such
run directory for the person to remove.
Runs with unconfirmed work, and the latest run that is not a dryrun for each
project, entry point, inventory, and playbook, are kept. An expired run of a
playbook stays until a command uses a run of that playbook again. Expiry
removes both saved results and the ability to resume that run;
`mmx run <playbook>` still works.
Cleanup is local: it does not contact servers or touch service hashes.

### Expire finished node results

Nodes keep a finished job's result for 72 hours, whatever the project's
retention, so a controller process that lost contact can still recover the
result with `mmx status` or `mmx resume`. Ordinary admission and status
traffic removes expired results under the node locks; queued and running
jobs stay. A cleanup failure is reported as housekeeping, separate from
task outcomes. The
controller process's saved copy keeps results and resume working after the
node copy expires. A replaced machine cannot answer for its predecessor's jobs, so
`status` and `resume` fail that unconfirmed work with the replacement as the
reason. The [history example](../../../examples/history/README.md) shows the
workflow.

## Exit results

| Code | Meaning |
|---|---|
| `0` | Success. Includes a complete dryrun with no proposed changes, and `results` or `status` when inspection succeeds, even if the inspected run failed. |
| `1` | The work being run or checked failed: a node task or run failed, a run stopped with unfinished work, `run-command` failed on a node, `validate` found invalid configuration, or `secrets usage` found a referenced key that is not defined. |
| `2` | Dryrun: something would change. `check-one-time-playbooks`: at least one node still needs an outstanding one-time playbook. `complete-one-time-playbook`: refused, because a node still needs the playbook. `mark-node-work-failed`: refused, because the node answered. `remove-mmx-from-node`: refused, because a node job runs or an owed restore waits on the node. |
| `3` | Unknown or incomplete: an incomplete or unknown dryrun, `status` or `stop` cannot confirm a node's current state, a one-time playbook check or completion could not inspect a node or inspected no nodes, or `mark-node-work-failed` could not tell whether the node is gone, such as after a refused login. |
| `4` | Command error: bad arguments or flags, an unknown command, a missing playbook name, `--var`, `--limit`, or `--only-fleet-stages` rejected before remote work, a command that names a completed one-time playbook, project/playbook/inventory loading failure (except in `validate` and per-playbook loading failures in `check-one-time-playbooks`), a vault or credential unavailable before the run starts, no saved run matching the selection, a short run ID that matches several saved runs, `resume` of a run that another MMX release started, or an unknown `describe-module` name. |
| `130` | Ctrl-C, including during preparation or when leaving the live display. |

If the command did its job and the answer is "failed", it returns `1`.
If the command could not do its job, the command returns `4`. Controller
storage, saved-state, asset preparation, and output errors therefore return
`4`, as do failed `init` or `secrets` operations. A missing project is invalid
configuration for `validate`; rejected command-line variable values are
command errors even for `validate`.

For an ordinary dryrun, failure takes precedence over incomplete, and
incomplete over proposed changes.
A stopped dryrun with unfinished work is a failure. An unconfirmed node job
makes a dryrun incomplete. A partial fleet failure returns `1` after the other
nodes finish, as does `run-command` when the command fails on, or cannot reach,
a selected node.
`stop` succeeds when node states are confirmed, including stopped or
failed work. Resuming an already-complete matching run succeeds with nothing
to resume; no matching run is a command error. For `module-json`, invalid
request or value data is a failed check; invalid helper arguments are command
errors. Node connection failures recorded during execution are failed work;
secret-loading failures before execution are command errors.


## Keep ongoing responsibilities separate

MMX reconciles declared configuration when invoked. Systemd supervises processes
and runs timers. Health checks and automatic recovery belong to a
monitoring tool. For drift
detection, schedule `mmx dryrun <playbook>` with a systemd timer or CI; apply
changes with an explicitly scheduled `mmx run <playbook>`. MMX has no
scheduler, event bus, reactor that automatically acts on events, or
continuously running controller process.

## Give non-privileged users limited controls

A developer may need to restart an application without administering its
server. MMX administrators have full control of their nodes; for people who
need only a few actions, expose those actions through the operating system or
the application, provisioned by ordinary MMX roles. No MMX authorization layer
or new module is involved.

### Start with a systemd path unit

A systemd `.path` unit watches a file or directory and starts a service when
its condition holds. For a restart control, the user's whole interface is:

```sh
touch /run/myapp-request/$USER
```

The `.path` unit starts an administrator-owned oneshot service while the
request directory is not empty. The service records that it is running, removes
the pending requests, restarts the application, and records the result. Use a
state condition such as `DirectoryNotEmpty=`, not `PathChanged=`: systemd
rechecks only state conditions after the service ends, so a request made
during a restart gets one more restart instead of being lost.
[Systemd path units](https://www.freedesktop.org/software/systemd/man/255/systemd.path.html).

Requesters can create and remove only their own entries in a sticky request
directory; the units, action, and result stay administrator-owned. The
[working example](../../../examples/limited-control/README.md) documents the role,
result file, boot-time `tmpfiles` setup, rate limit, and group authorization.

### Small request files and application APIs

When an action needs inputs, an administrator-owned handler can accept a small
data file with a fixed schema, such as a worker count within a range, and
validate it before acting; shell commands and playbooks are never request data.
An application API can expose the same fixed actions. MMX provisions either
through ordinary roles.

## Inspection and recovery limits

Closing a terminal keeps ordinary signal behavior for `run`; admitted node
jobs continue, and `mmx status` collects the node jobs' results. Only
`mmx secrets` handles SIGHUP explicitly, because a vault edit must remove
its plaintext before exiting.

Resume unlocks every named secret that the fleet stages of the recorded run
read ([secrets a run needs](configuration.md#secrets-a-run-needs)),
including secrets used only by completed fleet stages. Unlocking does not
deliver every secret: each node job receives only secrets its references
reach. Resume reuses the normal run's loading path without widening delivery.
If vault unlocking fails, the run can be recorded as `failed` while a node
entry remains uncertain. `status` or `resume` reconciles the uncertain node
entry; the summary does not replace the node's authoritative job record.

A controller process can connect during `status` while another process owns
the run, then leave the result unchanged. The extra connection is harmless;
the run's control lock still prevents competing updates. If controller loss
removes a computed secret output, collecting status cannot restore the
computed secret output's value. A new `mmx run` recomputes the secret output;
the [secret-output rules](execution.md#secret-outputs-that-come-from-a-node-are-never-saved)
explain which file references resume can recover.

Ctrl-C during preparation of a new run, before submission, can leave no
saved run and print `context canceled`. No node job was submitted, so a new
run is sufficient. Upload interruptions can be described as stopped partway
through, and logs can name job IDs that were never admitted. The interruption
messages do not change reconciliation by job ID or selection for resume.

`mmx explain` does not expand fleet stage policies, fleet stage source
locations, or the inputs of inactive role calls. The playbook remains the
complete definition for fleet stage policies, fleet stage source locations,
and the inputs of inactive role calls. Resume prints the selected run's ID
and playbook, without a separate scope summary.

Task lookup uses the recorded qualified task IDs. Follow-up commands do not
inherit the original command's filters. Logs do not add task timing or
source locations.

Fallback while connecting is announced. A fallback in the middle of a fleet
stage is recorded in the connection result without a separate live notice,
so saved results remain the source for inspecting that connection.

## Workflow decisions

The workflow decisions below are settled. Each list item gives the reason
for the decision.

- Settings live once in `mmx_project_settings.yml` so playbooks do not repeat them. Named
  playbooks and inventories stay under the same discoverable project root.
- `run` always uses current files on the full selection, and `resume` uses the
  recorded run: one meaning each, so there is no recovery menu or force flag.
  `stop` and `resume` describe run-level actions; `retry` means only a node
  task's automatic repeats.
- No interactive approval prompt or `pause` node task: dryrun is the preview, and
  noninteractive and JSON modes must never block on input.
- `mmx explain` renders each node's templates and expands the node's ufw
  inventory group rules, without contacting a node. A dryrun shows rendered
  templates and expanded ufw rules too, but needs servers that exist and
  answer, installs MMX's node component, and `--diff` hides the text of a
  private file such as a WireGuard configuration. An author checks a
  configuration before the servers exist.
  A template that reads a value only the node or the run knows, a fact, a
  node task's result, an output, or a secret, is left to the node, and
  `mmx explain` names the value. Only `mmx explain` renders: every run
  inspects the project, and holding each node's rendered files would cost
  every run.
- No tags, `--skip-tags`, or start-at-task: they skip prerequisites the written
  order promises. `--only-fleet-stages` selects whole fleet stages, whose
  prerequisites are declared, and rejects a selection that leaves one out; so
  one `site` playbook serves both "everything on this server" and "this one
  part on every server". `resume` handles recovery.
- One-time work is a dated playbook that runs only when named, is checked
  against the nodes themselves, and moves to `completed/` when every node has
  completed the work. Removal lines do not live in roles, where a destructive
  line is dangerous, and MMX keeps no record of which node ran what.
- `mmx new-one-time-playbook` takes only words that start with a letter,
  although the file name of a one-time playbook may start its words with a
  digit: the words become the ID of the new file's fleet stage, a fleet
  stage ID starts with a letter, and the file must be valid as it is created.
- The secrets commands say "keys" and never print a value; `diff-keys`
  compares with the last Git commit instead of MMX tracking a key list, and
  the vault's format stays one encrypted file.
- `mmx secrets set` reads the value from standard input, never from an
  argument: a command line is visible to other processes and kept in shell
  history, and a value an agent pipes from the command that produced it
  never enters its conversation. `mmx secrets set` replaces a value only with
  `--replace`, so a key name that is already taken never loses its value by
  accident.
- MMX never opens port 7443 on a node's firewall; it continues over SSH and
  says which rule to add, because a firewall change nobody wrote in a playbook
  is a surprise.
- Ad hoc commands are `run-command` only, with no dryrun: other modules belong in a
  playbook, and a bare command has nothing to inspect.
- `mmx describe-module` and a core module's page take the inputs, their types,
  and their defaults from the validation schema, not from separate
  documentation, so they cannot disagree with `validate`.
- No scheduler, event bus, reactor, or persistent controller: systemd timers
  and CI already schedule `mmx dryrun <playbook>` and `mmx run <playbook>`.
- Ongoing health checks and recovery belong to a monitoring tool, and to systemd, not MMX.
- Limited controls for non-administrators are systemd path units and fixed
  actions, not an MMX permission layer; MMX administrators have full root.

## Validation targets

The public workflow tests exercise the following behaviors through the CLI
on real nodes:

- Named playbooks and inventories.
- Project-wide and single-playbook validation.
- First use with keys, passwords, and sudo.
- The notice for a node whose port 7443 MMX cannot reach.
- `nodes` with inventories, limits, fleet stage selections, and helper nodes
  of a `for_each_node` loop.
- `--only-fleet-stages` kept, rejected, and reused by resume.
- The one-time playbook commands from creation through a node that was
  unreachable to completion.
- The four secrets key commands, with no value in any output.
- `run-command` as a connection check and ad hoc command, with results and
  resume of the command's run.
- `describe-module` entries for every task action.
- Run after a failure and edit.
- Default run selection across playbooks for one inventory.
- Default and explicit resume, including recorded limits and extra
  variables.
- Ctrl-C, stop, and status.
- Output modes, diffs, and results filters in human and JSON form.
- History retention.
- The limited-control role.

Go tests of `mmx` in `cmd/mmx/` check project discovery from subdirectories
and the errors for a missing playbook name, without a node. The
[test plan](development.md#automated-code-and-workflow-tests) owns the suites
and what they check.

## Beyond the first release

- Tested example roles for fixed-schema request files and application APIs.
- Resume and node setup across different MMX releases.
