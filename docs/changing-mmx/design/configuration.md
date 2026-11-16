# MMX: configuration and node data

Part of the [main plan](README.md). The configuration and node data design
owns the project format, inventory, node targeting, one-time playbooks,
roles, values (including group and command-line variables), loops,
conditions, templates, facts, and secret inputs.

## One readable configuration format

Projects use YAML with a strict, small schema. Unknown fields and wrong types
are rejected with the file, location, field, and expected form. Before any
remote change, MMX validates static configuration: configuration whose
values are known before execution.

Booleans are spelled `true`/`false`; file modes are quoted strings. Duplicate
keys, custom tags, aliases, anchors, and merge keys are rejected: reuse comes
from variables and roles, not YAML structure. MMX's internal records use the
mapping keys `$mmx_secret`, `$mmx_truncated`, and `$mmx_binary`. MMX rejects
`$mmx_secret`, `$mmx_truncated`, and `$mmx_binary` in project files and in
results from modules that are not core.

## Playbooks contain ordered fleet stages

A playbook's `fleet_stages` list makes the workflow visible. Each fleet stage
has a unique `id`, selects `nodes`, and contains ordered
`node_tasks`. A node task performs an action, calls a reusable role, or
executes a node task block (`task_block`). Roles describe server
responsibilities such as `common` or `app_server`; actions such as database
migrations belong visibly in the playbook. Split work into fleet stages
only at a meaningful cross-node boundary, such as finishing a migration
before updating app servers.

MMX finishes a fleet stage's admitted node jobs (the node jobs accepted by
nodes for execution) before considering the next fleet stage.
Written order decides when work is considered;
`requires` decides which earlier work must have succeeded:

| Requirement | Meaning |
|---|---|
| `requires.same_node` | Each listed earlier fleet stage succeeded on the current node. Other nodes' failures do not block the current node. |
| `requires.all_nodes` | Each listed earlier fleet stage succeeded on every node it selected. |

For an inventory containing `db01` and an `app` group,
`playbooks/deploy.yml` contains:

```yaml
fleet_stages:
  - id: configure_servers
    nodes: all

    node_tasks:
      - role: common

  - id: configure_database_server
    nodes: db01
    requires:
      same_node: [configure_servers]

    node_tasks:
      - role: database_server

  - id: migrate_database
    nodes: db01
    requires:
      same_node: [configure_database_server]

    node_tasks:
      - shell:
          script: /usr/local/bin/migrate-myapp

  - id: configure_app_servers
    nodes: app
    requires:
      same_node: [configure_servers]
      all_nodes: [migrate_database]

    node_tasks:
      - role: app_server
```

A common-configuration failure on `app02` blocks only `app02`'s application
configuration. A migration failure on `db01` blocks application
configuration on every app node.

Validation rejects missing or duplicate fleet stage IDs and requirements
that name the current or a later fleet stage. When both the prerequisite
fleet stage and the dependent fleet stage have static selections,
validation also rejects a `same_node` prerequisite that did not select the
consuming node.
MMX does not reorder fleet stages or add nodes to satisfy
a requirement. The [execution
contract](execution.md#playbook-fleet-stages-follow-written-order-and-explicit-requirements)
owns blocking, continuation, and resume.

## Node tasks and task blocks

The controller process selects and schedules nodes for a fleet stage. On
each selected node, a node job worker process executes the fleet stage's
`node_tasks` in order. A `task_block` occupies one position in `node_tasks`
and groups related node tasks into named phases for the node running the
fleet stage. The `task_block` mapping contains a required `id`, a nonempty
`changes` list, and optional `validate`, `checks_before_restart_or_reload`,
`activate`, `verify`, `on_failure`, and `cleanup` lists. Each phase is a list
of individual node tasks. Roles and enclosing task blocks belong in
`node_tasks`, outside phase lists.

`checks_before_restart_or_reload` holds checks that must pass before the node
task block restarts or reloads a service that is running now. The checks run
only if applying the node task block would do at least one of the following:

- Restart or reload a service that is running now.
- Run a command marked `only_when_tracked_inputs_changed: true`.

When needed, the checks run before the node task block's first write when
MMX can foresee the restart or reload, so a failed check then leaves the
node unchanged. Otherwise they run just before the restart, reload, or
command, and the block's `changes` are already written. The `checks_before_restart_or_reload`
phase accepts only node tasks that change nothing: `assert`, `http` with
`GET` or `HEAD`, `wait_for`, `show`, and `shell` with `changed_when: false`.
A typical check is an `assert`
on a boolean role input that the playbook fills from a
[command-line variable](#command-line-variables), so a routine run refuses to
restart Redis and a planned run with `--var restart_redis_approved=true` goes
ahead:

```yaml
- task_block:
    id: redis_cache
    changes:
      - file:
          path: /etc/redis/redis.conf
          template: files/redis.conf.tmpl
          owner: redis
          group: redis
          permissions_mode: "0640"
    checks_before_restart_or_reload:
      - assert:
          that: "{{ .inputs.restart_approved }}"
          message: >-
            Redis must restart to load this change, and a restart empties
            the cache. Run again off-peak with
            --var restart_redis_approved=true.
    activate:
      - systemd_service:
          name: redis-server
          start_at_boot: true
          refresh: restart
          state: running
```

In the Redis example, `restart_approved` is a boolean input of the role that
holds the node task block; the playbook passes
`"{{ .vars.restart_redis_approved }}"`, a command-line variable declared with
`default: false`.

Generic node task options such as `when`, `loop`, `as`, `key`, and time
limits belong in the YAML mapping that contains `task_block`, alongside the
`task_block` key. The required node task block `id` and phase lists belong
in the nested `task_block` mapping. `task_block.id` names the node task block
and supplies its default path in saved results.
An optional outer node task `id` names the node task block
invocation, overrides the saved result path, and
exposes the invocation's result through `.node_tasks.<id>`. Assigning an
outer node task `id` is useful for loops: an outer `id` exposes the ordered
iteration results through `.node_tasks.<id>.items`. The block's own `id` alone
does not publish a named `.node_tasks` value.

Blocks reject a `retry` setting: individual node tasks can retry, while resuming
the recorded run starts unfinished node jobs again. The [execution
contract](execution.md#node-task-blocks-complete-the-change) owns phase order,
failure handling, activation, and cleanup. Blocks do not provide automatic
rollback.

The [role example](#roles-describe-reusable-server-configuration) shows a
complete block. Use blank lines to separate standalone node tasks, task blocks,
fleet stages, and larger configuration sections. Keep the contents of each
`task_block` contiguous, without blank lines; indentation shows its internal
structure. Blank lines between units of work keep standalone node tasks,
node task blocks, fleet stages, and larger configuration sections visually
separate without breaking up a node task block.

## Project settings

The controller process finds the project root by looking for
`mmx_project_settings.yml` in the current directory, then each parent
directory. Project settings live in one place so playbooks do not repeat
them. There is no `--file` option:

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

`mmx_project_settings.yml` accepts only `project_name`, `default_inventory`,
`vault_file`, `secrets_from_env`, `vars`, `node_task_defaults`,
`max_nodes_at_once`, `run_history_retention`, and
`allow_role_calls_within_roles`. For example:

```yaml
project_name: shop
default_inventory: example
max_nodes_at_once: 100
```

A playbook is `playbooks/<name>.yml`. A playbook's only top-level fields are
the required `fleet_stages` and optional `command_line_vars`. Commands
select a playbook by name, never by path: `mmx run deploy`,
`mmx dryrun deploy`, or `mmx explain deploy --limit web01`. There is no
default playbook. `run` and
`dryrun` without a name fail and list the project's playbooks; `explain` also
requires a name.

`mmx validate` checks the project settings, every inventory, and every
playbook. `mmx validate deploy` checks `playbooks/deploy.yml` with the selected
inventory. `mmx nodes` lists the selected inventory's nodes and groups;
`mmx nodes deploy` also shows each fleet stage's selection.

Playbook node task paths (`file.source`, `file.template`,
`directory.source`, `apt_repository.signing_key_source`,
`unpack_archive.source`, `systemd_unit.template`, and the other source and
template paths the
[module pages](../../reference/modules/README.md) list) resolve from the project root.
Inside a role, node task source and template paths resolve from the role's
directory. The
[file page](../../reference/modules/file.md#paths-on-the-controller-machine) owns the path
rule, including relative paths that leave a role but stay inside the project.
Prefer [self-contained roles](#share-a-file-or-a-value-between-roles).
`files/` is the conventional place for project files and templates. Roles,
the project's own modules, the default vault `secrets_vault.age`, and saved
state in `.mmx/` live at the project root.
The saved state includes run history, node pins (fingerprints of the nodes'
HTTPS keys), and the runner cache (saved `mmx-runner` builds for resume).
`mmx describe-module`, `mmx module-usage`, and `mmx secrets edit` find the
project root by looking for `mmx_project_settings.yml` in the current
directory, then each parent directory.

`mmx init` creates `mmx_project_settings.yml` with `project_name` and
`default_inventory: example`, `inventories/example.yml` with one example
node, and
`playbooks/example.yml` with the starter fleet stage. `mmx init` adds
`.mmx/` to `.gitignore`.

`run_history_retention` sets how long finished run records stay on the
controller machine: a positive duration such as `24h` (the default) or
`7d`, or `forever`. The
[history contract](workflow.md#saved-run-history) owns expiry.

### One-time playbooks

A one-time playbook is a playbook for work that must happen once on every
node it selects and never again, such as removing a retired program after a
migration. A one-time playbook lives in `one_time_playbooks/` at the project
root, beside `playbooks/`, so cleanup work never sits in a role and never
mixes with the playbooks people run every day.

A one-time playbook's file name is `YYYY-MM-DD_what_it_does.yml`: the date
the one-time playbook was written, an underscore, and lowercase words joined
by underscores, such as `2026-10-05_remove_old_ruby_manager.yml`. The date is
a real date, and a word holds lowercase letters and digits. Validation
rejects anything else in
`one_time_playbooks/`, except `completed/` and files whose names start with
a dot, which Git or a file manager leaves in `one_time_playbooks/`. The
playbook's name is the file name without `.yml`, and commands select the
one-time playbook like any playbook:

```sh
mmx dryrun 2026-10-05_remove_old_ruby_manager
mmx run 2026-10-05_remove_old_ruby_manager
```

An ordinary playbook's name starts with a letter and a one-time playbook's
name starts with its date, so no name is in both `playbooks/` and
`one_time_playbooks/`, and a name alone says where its file is.
`validate`, `explain`, `nodes`, `dryrun`, `run`, and `module-usage` take a
one-time playbook's name; `mmx validate` without a name checks every one-time
playbook outside `completed/` with every inventory, as it checks the
ordinary playbooks. A one-time playbook has the
same format as any playbook, with one extra rule: each of its
`command_line_vars` has a `default`, so MMX can check it without being given
values. A one-time playbook runs only when a command names the playbook:
`run` and `dryrun` without a name list the one-time playbooks apart from
the playbooks in `playbooks/`.

`one_time_playbooks/completed/` holds the one-time playbooks whose work is
done on every node. MMX moves a one-time playbook file to
`one_time_playbooks/completed/`. MMX never deletes a one-time playbook file,
so `one_time_playbooks/completed/` is the project's history of such work.
A playbook in `completed/` cannot be run or dry-run, and `mmx validate` does
not check it; moving the file back out makes it an ordinary one-time
playbook again. The
[one-time playbook workflow](workflow.md#one-time-playbooks) owns the commands
that create, check, and complete these playbooks.

## Node task defaults are shared project configuration

`mmx_project_settings.yml` can set `node_task_defaults` for node task time
limits and retries. The values in the following `node_task_defaults`
example are also MMX's built-in defaults:

```yaml
node_task_defaults:
  time_limit: null
  time_limit_per_try: null
  retry:
    max_tries: 3
    pause_between_tries: 2s
```

Each setting resolves independently: node task setting, then project default,
then built-in default. An omitted setting inherits; an explicit `null` on a
time limit removes that limit. `time_limit` limits the whole node task, including
every try; `time_limit_per_try` limits one try.

A node task's `retry` is `true`, `false`, or a mapping with `max_tries`,
`pause_between_tries`, `until`, and `retry_when`.
`pause_between_tries` sets the pause after a try and before the next.
`max_tries` is a positive integer or `unlimited`, never `null`.
`unlimited` requires a finite `time_limit`, set on the node task or
inherited from `node_task_defaults.time_limit`, including when `unlimited`
comes from `node_task_defaults.retry.max_tries`. The
[task retry contract](execution.md#task-retries-have-explicit-conditions-and-limits)
defines the retry conditions and lists which node tasks retry by default,
which try once, and which reject `retry` or time limits.

For readiness checks, use `wait_for`, which checks every second until ready
or its own `wait_at_most` expires (default `60s`):

```yaml
- wait_for:
    tcp_port_accepts_connections: 5432
    wait_at_most: 60s
```

`wait_for` and `reboot` reject node task level `retry`, `time_limit`, and
`time_limit_per_try` and do not inherit node task defaults; each uses its own
`wait_at_most` (`60s` for `wait_for`, `10m` for `reboot`). The
[`wait_for` page](../../reference/modules/wait_for.md)
owns its fields and dryrun behavior.

`mmx explain deploy` shows effective settings and their sources. `mmx resume`
uses the settings saved with the run; a new `mmx run deploy` uses the current
defaults.

## Inventory is a list of nodes, not a second programming language

An inventory such as `inventories/example.yml` has optional shared `vars`,
named `nodes`, and flat `groups`:

```yaml
vars:
  log_level: info

nodes:
  web01:
    address: web01.example.net
    ssh_user: deploy
  web02:
    address: web02.example.net
    ssh_user: deploy
    vars:
      worker_count: 8
  db01:
    address: db01.example.net
    ssh_user: deploy

groups:
  web:
    nodes: [web01, web02]
    vars:
      http_port: 8080
      worker_count: 4
  canary:
    nodes: [web01]
```

In the `inventories/example.yml` example, `web01` gets `worker_count` 4
from the `web` group, and `web02` sets `worker_count` to 8.
The [value order](#values-have-a-short-route-to-the-place-they-are-used)
decides which value each node uses.

Node connection fields stay flat and name the connection they configure:

| Field | Meaning |
|---|---|
| `address` | Hostname or IP used by SSH and HTTPS. |
| `ssh_user` | SSH login account for setup and fallback. |
| `ssh_port` | SSH port. |
| `ssh_identity_file` | SSH private key file on the controller machine. |
| `ssh_known_hosts` | SSH known-hosts file on the controller machine. |
| `ssh_config` | SSH configuration file on the controller machine. |
| `ssh_password` | Whole-value secret reference for the SSH password. |
| `sudo_password` | Whole-value secret reference for the sudo password. |
| `connection` | `https_with_ssh_fallback` (default) or `ssh_only`. |
| `https_address` | HTTPS origin instead of `address:7443`, such as a NAT port mapping. |
| `mmx_runner_download_url` | Where nodes download the node runner executable, `mmx-runner`. |
| `vars` | Node variables. |

`ssh_identity_file`, `ssh_known_hosts`, and `ssh_config` take a path that
starts with `~/` from the home directory, as OpenSSH does, so a path copied
from SSH habits works. Another relative path starts at the project
directory, so a project can carry its own known-hosts file in Git.

`ssh_user` is root or an administrative account using sudo. The `ssh_user`
field does not choose the node task execution user. The installed node
runner executable, `mmx-runner`, runs as root. Application commands select
another user explicitly
([privilege contract](execution.md#privilege-has-one-ordinary-path)).
`ssh_password` and `sudo_password` take ordinary [secret](#secrets) references
as their entire values, with no text before or after the reference:

```yaml
nodes:
  web01:
    address: web01.example.net
    ssh_user: deploy
    ssh_password: "{{ .secrets.ssh_password }}"
    sudo_password: "{{ .secrets.sudo_password }}"
```

Bare secret names are invalid. The controller process resolves passwords in
memory only when authentication needs them. The `ssh_password` and
`sudo_password` values never enter saved inventory, node jobs, or results.

A group lists its `nodes`, each once, and may set `vars` for them. Groups do
not nest or set order, and connection fields stay on nodes. `all` selects every node and
cannot be defined as a group; a group cannot share a node's name. A selector
is a node ID (the key under inventory `nodes`, such as `web01`), a group
name, or `all`. An unknown name is an error. A list of
selectors is a deduplicated union, sorted by node ID. Selectors have no
intersection syntax and no pattern strings: environments are separate
inventories and subsets are [limits](#limit-a-run-to-part-of-the-inventory).

A fleet stage's `nodes` field takes a selector, a list of selectors, or one
whole-value reference. The reference reads `.vars`, `.inventory`, or the
variable of an enclosing loop, which the controller process knows before it
connects. The `nodes` reference may also read what an earlier fleet stage
published (`.outputs`). A selection based on `.outputs` resolves during the
run
([execution rule](execution.md#playbook-fleet-stages-follow-written-order-and-explicit-requirements)).
The `nodes` field never takes a node fact.

### Select everything except some nodes

A fleet stage can add `except_nodes`, which takes the same selectors as
`nodes`. The fleet stage selects the union of `nodes` minus the union of
`except_nodes`. For example, an inventory can list `web01`, `db01`, and
`build01`, with `build01` in the `virtual_machines` group:

```yaml
nodes:
  web01: {address: web01.example.net, ssh_user: deploy}
  db01: {address: db01.example.net, ssh_user: deploy}
  build01: {address: build01.example.net, ssh_user: deploy}

groups:
  virtual_machines:
    nodes: [build01]
```

The `configure_network_interfaces` fleet stage below runs on `web01` and
`db01`. The `configure_network_interfaces` fleet stage also runs on every
node added to the inventory later that is not in `virtual_machines`:

```yaml
fleet_stages:
  - id: configure_network_interfaces
    nodes: all
    except_nodes: [virtual_machines]

    node_tasks:
      - role: network_interfaces
```

`except_nodes` resolves like `nodes`: written selectors resolve from the
inventory before any connection, so `mmx nodes` shows the selected nodes.
`--limit` narrows the nodes selected after `except_nodes` removes nodes.
An unknown name is an error. Selector lists cannot contain nested lists,
and no command-line option sets `except_nodes`.
A [`for_each_node` loop](#loop-over-nodes-with-for_each_node) accepts
`except_nodes` as a fleet stage does.

### Leave out a fleet stage that selects no nodes

A selection that is empty is an error: a group that `nodes` names has no
node, or `except_nodes` removes every node of `nodes`. The project then does
not load, and the error message names `skip_when_no_nodes_are_selected`.
MMX reports an error by default because silently omitting a fleet stage's
node jobs could hide a group emptied by mistake.

Some fleet stages are for groups that one team has and another does not,
such as an optional staging server, or a group a team fills only at a later
step. Such a fleet stage sets `skip_when_no_nodes_are_selected: true`:

```yaml
fleet_stages:
  - id: configure_staging_server
    nodes: staging
    skip_when_no_nodes_are_selected: true

    node_tasks:
      - role: app_server
```

With an inventory whose group `staging` lists no node, the fleet stage is
left out of the run and reported as not selected, with the reason that it
selects no nodes, as a [limit](#limit-a-run-to-part-of-the-inventory)
reports a fleet stage it leaves out. Before any connection, `mmx nodes` and
`mmx explain` also report `configure_staging_server` as not selected
because the fleet stage selects no nodes.
`skip_when_no_nodes_are_selected` applies to the written selection,
`nodes` minus `except_nodes`, before a limit. A limit that leaves the
selection empty follows the limit's rules. When a selection based on
`.outputs` resolves during the run, an empty selection leaves the fleet
stage out if `skip_when_no_nodes_are_selected: true`.
If `skip_when_no_nodes_are_selected` is omitted or `false`, an empty
selection based on `.outputs` fails the fleet stage when the selection
resolves.

A fleet stage that selects no nodes leaves out no work that later work
needs. The empty fleet stage's whole output collection, `.outputs.<id>`, is
an empty mapping. A `requires.all_nodes` requirement on
the empty fleet stage is met, because no node in the empty fleet stage
failed.
Reading a named node's output from the empty collection, such as
`.outputs.<id>.web01`, is an error, as for any node a fleet stage does not
select.

A `requires.same_node` requirement on an empty prerequisite fleet stage
from a fleet stage that selects nodes is never met. The requirement names
a node the empty prerequisite fleet stage does not select.
Validation rejects the requirement when the prerequisite and consuming
fleet stages' selections are known before the run. Otherwise, the node's
work in the consuming fleet stage is blocked.

A fleet stage that selects no nodes needs none of the secrets read by the
fleet stage's node tasks ([secrets a run needs](#secrets-a-run-needs)).
A team without the group needs no placeholder for the empty fleet stage's
secrets read by node tasks. A fleet stage that selects nodes ignores
`skip_when_no_nodes_are_selected`, and an unknown name in `nodes` or
`except_nodes` stays an error.
A [`for_each_node` loop](#loop-over-nodes-with-for_each_node) also accepts
`skip_when_no_nodes_are_selected: true`, so an empty node selection leaves
the loop out of the run. A `loop` repeats enclosed fleet stages over values,
rather than selecting nodes directly. A `loop` does not accept
`skip_when_no_nodes_are_selected`, because the enclosed fleet stages select
the nodes.

Two groups that share a node must not set the same variable. Validation
checks the whole inventory, whatever the selection, even when the values
match. The validation error names the node, the variable, and both groups
that set the variable.
Put a shared value in inventory
`vars`, in one group, or in each node's own `vars`. Group order never decides
a value. Equal values are rejected because two copies drift apart. When
someone later edits the copy in one group, validation fails for every node
that is also in the group holding the unedited copy, a group that the person
making the edit did not open.

A generated inventory comes from a program that writes inventory YAML.
MMX reads the generated file and saves the file with the run.

Ansible's inventory and Salt's top file assign work to machines. MMX assigns
work to nodes in each fleet stage and keeps inventory to data and node
selection.

### Choose the inventory for a run

The project's `default_inventory` field names the default inventory:
`default_inventory: example` selects `inventories/example.yml`.
`--inventory <name>` replaces the default inventory choice for one
invocation of a command that reads the project. The `--inventory` option
accepts a name, not a path:

```sh
mmx dryrun deploy --inventory staging
mmx run deploy --inventory example
```

A command that runs or inspects a playbook, or runs `mmx run-command`, uses exactly
one inventory; inventory files never merge. Only `mmx validate` without a
playbook name reads every inventory, each on its own. A
second environment is a second inventory file with its own nodes, `vars`, and,
if needed, `vault_file`, not environment conditions in tasks. A missing or invalid
inventory fails before any connection.

A run's identity is the project, playbook name, and inventory name. Without a
run ID, `status`, `stop`, and `results` select the project's latest playbook run
for the selected inventory, whichever playbook it used, and `resume` selects
the latest such run that was not a dryrun. The `status`, `stop`, `results`,
and `resume` commands each report the selected run and playbook. For
example,
`mmx resume --inventory example` resumes the latest non-dryrun playbook
run for the `example` inventory. A short or full run ID selects any retained
run. Runs of `mmx run-command` have entry point `run-command` and are selected
only by ID.

Resume leaves finished nodes alone. Each resumed node job starts its fleet
stage's node tasks from the beginning, using the run's saved playbook,
inventory, limit, `--var` values, and file contents
([resume](workflow.md#resume-uses-the-recorded-run)).

### Limit a run to part of the inventory

`--limit` narrows one invocation to some nodes, using the same selector names
as `nodes`, comma-separated:

```sh
mmx dryrun deploy --limit canary
mmx run deploy --limit canary
mmx run deploy --limit canary,db01
```

Each fleet stage's selection becomes its intersection with the limit. Outside a
[`for_each_node` loop](#loop-over-nodes-with-for_each_node) a limit never
adds a node and changes nothing else. Fleet stage order, `requires`, and
rollout settings apply as written. A `requires.all_nodes` requirement means
every node the fleet stage selected under the limit. A fleet stage whose
intersection is empty is left out of the run and reported as not selected,
not as skipped or failed.

MMX rejects the limit before any remote work, naming the fleet stages
involved, when:

- a limit name is not in the inventory, or no fleet stage keeps any node;
- a remaining fleet stage `requires`, or reads the outputs of, a fleet stage the limit left
  out;
- a remaining fleet stage reads the whole output collection of a fleet stage the limit
  narrowed, since the collection would silently lose nodes (for example, a
  load balancer's backend list).

Inside a `for_each_node` loop, the limit picks which of the loop's nodes get
an iteration. The fleet stages enclosed in a kept iteration are not
intersected with the limit: they run on the nodes they name, because that
work belongs to the kept node. A node that an iteration works on for the
loop's node is a helper node, and a limit still reaches it.
For example, a loop over the group `app` takes each app server out of the
load balancer `lb01`, changes the app server, and puts the app server back.
The command `mmx run deploy --limit app01` runs the whole `app01` iteration,
including the fleet stages on `lb01`.
Before connecting, `nodes`,
`explain`, `dryrun`, and `run` list the helper nodes under "Nodes outside
--limit that help selected nodes".

A `same_node` requirement survives a limit: a node the limit keeps in the
consuming fleet stage is kept in the prerequisite too, because validation already
requires the prerequisite to select it. Selections that resolve during the run,
such as fleet stage loops over earlier outputs, are intersected when they resolve.
Work that needs a fleet stage or node the limit left out and that could not be
rejected up front, such as one named node's output, is blocked, naming the
limit. To include a prerequisite, widen the limit, such as `canary,db01`, or
run without a limit.

`dryrun` and `run` print the inventory, the limit, and the selected node count
before connecting to any node, and report each left-out fleet stage as not selected.
`mmx explain deploy --limit canary` and
[`mmx nodes deploy --limit canary`](workflow.md#show-what-a-command-will-target) show
each fleet stage's selection locally, using shared code.
Dryrun applies the limit, helper-node, and prerequisite rules used by `run`.
An unchanged rerun that keeps the previous run's limit
reports unchanged on the limited nodes.

The limit and the resolved selections are saved with the run. The limit is not
part of the run's identity, so after `mmx run deploy --limit canary`, `mmx resume`
resumes that canary run. Resume always uses the recorded selection and rejects
`--limit`; a different subset is a new run, such as
`mmx run deploy --limit web02`.

### Run only some fleet stages

`--only-fleet-stages` keeps the named fleet stages of a playbook, in written
order, and reports the others as not selected. The `--only-fleet-stages`
option takes fleet stage IDs,
comma-separated, and works on `explain`, `nodes`, `dryrun`, and `run`:

```sh
mmx run site --only-fleet-stages ruby
mmx dryrun site --only-fleet-stages monit_base --limit worker
mmx run site --only-fleet-stages nginx_passenger,platform_api_sites
```

The `--only-fleet-stages` option serves a project that keeps all its server
configuration in one playbook, here `playbooks/site.yml`, with one fleet
stage for each unit the team applies on its own. Each shared role has a
fleet stage selecting every group that has the shared role. Each kind of
server has a fleet stage for the server's own roles. The list of which nodes
get which role is then written once. The command `mmx run site` configures
everything.
The command `mmx run site --limit app08` configures one server, and
`mmx run site --only-fleet-stages ruby` applies the Ruby role on every node
that has it.

The `--only-fleet-stages` option never skips work a kept fleet stage depends
on. If a kept fleet stage `requires`, or reads the outputs of, a fleet stage
that was left out,
MMX rejects the command before any connection and prints the option value
that includes the missing fleet stage. MMX does not add the fleet stage by
itself, so "only" never runs more than was named. An unknown ID is an error
that lists the playbook's fleet stage IDs. Naming a loop's `id` keeps the
loop with all its enclosed fleet stages; an enclosed fleet stage cannot be
named alone.

The `--only-fleet-stages` option combines with `--limit` and `--var`.
The `--only-fleet-stages` value is saved with the run, so `mmx resume`
reuses the recorded fleet stage selection and rejects `--only-fleet-stages`.
Dryrun keeps the fleet stages named by `--only-fleet-stages`.

The `--only-fleet-stages` option selects whole fleet stages, never single
node tasks. MMX has no tags and no start-at-task option
([decisions](#design-decisions)).

## Calling a module

A node task names its module, whatever the module's kind, and writes the
module's inputs below that name, beside the node task's own settings (`id`,
`when`, loops, time limits, and retries):

```yaml
- id: greet_barbara
  greeting:
    path: /tmp/greeting.txt
    person: "{{ .vars.person_to_greet }}"
```

Module names are static: MMX looks a name up among the core modules, the
official extra modules, and then the project's own modules in
`external_modules/` at the project root, also for calls inside roles.
Inputs support ordinary references, loops, and secrets.
`mmx validate` checks every call against the module's description, and
checks a value that a reference supplies on the node before the module runs. The
[module kinds](modules.md#module-kinds-core-official-extra-and-the-projects-own)
own the lookup, the module description, Starlark scripts, and the JSON
protocol.

## Roles describe reusable server configuration

A **role** packages reusable `node_tasks` with declared inputs and optional
outputs. A role's node tasks, input declarations, and output declarations
are stored in `roles/<name>/main.yml`, beside the role's assets.
Asset paths in a role are relative to its directory;
playbook task assets are relative to the project root. There is no search path.

```text
mmx_project_settings.yml
inventories/example.yml
playbooks/deploy.yml
roles/
  web_server/
    main.yml
    files/
      nginx.conf.tmpl
```

Project settings in `mmx_project_settings.yml`, using the
`inventories/example.yml` example with the `web` group:

```yaml
project_name: web_fleet
default_inventory: example

# Keep role order and dependencies visible in the playbook.
# Enabling this lets roles call other roles, making the flow harder to follow.
allow_role_calls_within_roles: false
```

The playbook, `playbooks/deploy.yml`:

```yaml
fleet_stages:
  - id: configure_web_servers
    nodes: web

    node_tasks:
      - role: web_server
        inputs:
          port: "{{ .vars.http_port }}"
          workers: "{{ .vars.worker_count }}"
```

`roles/web_server/main.yml`:

```yaml
inputs:
  port:
    type: integer
    required: true
  workers:
    type: integer
    default: 2

node_tasks:
  - task_block:
      id: nginx
      changes:
        - apt_package:
            packages:
              - {name: nginx, version: newest_when_first_installed}
            state: present
        - file:
            path: /etc/nginx/nginx.conf
            template: files/nginx.conf.tmpl
            owner: root
            group: root
            permissions_mode: "0644"
            check_new_file:
              shell:
                script: nginx -t -c "$NEW_FILE"
                env:
                  NEW_FILE: "{{ .new_file.path }}"
      activate:
        - systemd_service:
            name: nginx
            start_at_boot: true
            refresh: reload
            state: running
      verify:
        - wait_for:
            url_responds: "http://127.0.0.1:{{ .inputs.port }}/health"
            expected_http_status_codes: [200]
            wait_at_most: 30s
```

`roles/web_server/files/nginx.conf.tmpl`:

```text
worker_processes {{ .inputs.workers }};
events {
    worker_connections 1024;
}
http {
    server {
        listen {{ .inputs.port }};
        location = /health {
            return 200 "ok\n";
        }
    }
}
```

The node task block installs the package, renders the template to a new file,
checks the new file with `nginx -t`, and installs the new file.
The node task block ensures the service is enabled and running, reloads the
service if the configuration changed, and checks health until the check
passes or 30 seconds elapse.

`wait_for` is the normal `verify` node task after
a service starts or reloads. A rejected new file leaves the live file intact.
`refresh` says how to load changed configuration, not to reload every run: MMX
compares the installed file's hash with the one the service last loaded, so a
file written before an earlier failure still gets reloaded. The
[node task block contract](execution.md#node-task-blocks-complete-the-change) and
[service-hash contract](execution.md#service-reloads-follow-configuration-hashes)
own validation, activation, and recovery; the
[new-file check contract](modules.md#check-new-files-before-installing-them)
owns `check_new_file` on `file`, `download`, `line`, and `edit_file` tasks.
`validate` names the node task block phase for checks across changes.

Generated files use Go's `text/template` with missing keys as errors and no
filesystem, environment, or network lookups.
[Template functions](#template-functions) lists the functions a template can
call.

List roles in the playbook in order, prerequisites first; MMX does not resolve
role dependencies. A role calling another role is a validation error unless the
project sets `allow_role_calls_within_roles: true`; cycles are always errors.
A role
reads only its inputs, its node's facts, and its own results; project and
inventory variables reach it through `inputs`.

### Share a file or a value between roles

Prefer a self-contained role so moving it does not break another role's
files. The [path rule](../../reference/modules/file.md#paths-on-the-controller-machine)
also permits shared project files. When two roles need the same thing, the
following configuration patterns keep ownership clear:

| Situation | Form |
|---|---|
| Two kinds of server install shared configuration, and neither server-specific role owns the shared configuration. | Put the shared configuration in a role. Call the shared role from one fleet stage selecting the two server groups; a node in both groups runs the shared role once. |
| Two roles are the same role for two environments. | One role; each environment's inventory supplies the values that differ, and the playbook passes them as inputs. |
| A value belongs to the project, and two roles read the value. | Write the shared value once in project or inventory `vars`. The playbook passes the shared value as an input to the two roles that read the value. |
| One role needs what only another role's work can know. | The first role declares an [output](#outputs-and-fleet-stage-outcomes), and the playbook passes it to the second role as an input. |

For example, the `redis` and `redis_monitoring_checks` roles need a shared
list of Redis instances. The inventory `inventories/example.yml` stores the
list once in `vars.redis_instances`, following the shared-value pattern in
the third row:

```yaml
vars:
  redis_instances:
    - {name: cache, port: 6379}
    - {name: sessions, port: 6380}

nodes:
  cache01: {address: cache01.example.net, ssh_user: deploy}
  monitor01: {address: monitor01.example.net, ssh_user: deploy}
```

The playbook `playbooks/site.yml` passes `vars.redis_instances` to `redis`,
the role that runs the Redis instances, and `redis_monitoring_checks`, the
role that monitors the Redis instances:

```yaml
fleet_stages:
  - id: redis
    nodes: cache01

    node_tasks:
      - role: redis
        inputs:
          instances: "{{ .vars.redis_instances }}"

  - id: redis_monitoring_checks
    nodes: monitor01

    node_tasks:
      - role: redis_monitoring_checks
        inputs:
          instances: "{{ .vars.redis_instances }}"
```

Both `redis` and `redis_monitoring_checks` declare `instances` as an input
of `type: list`. The command `mmx explain site` shows both the `redis`
and `redis_monitoring_checks` role calls and the one inventory line supplying
`vars.redis_instances`.

### Choose a source file by node, loop item, or input

The `source` and `template` paths in node tasks may contain references.
An `authorized_keys` node task also accepts references in
`key_files_on_controller_machine` and
`key_directories_on_controller_machine`.
The controller process resolves path references for each node or loop item,
so a single node task can install different files.
Paths accept only values the controller process knows before
connecting:

- `.vars`: project, inventory, group, and node variables, and command-line
  variables;
- `.inputs` that the role call fills from such values;
- the variable of a loop over such values;
- `.node.name`;
- `.inventory`.

A value that an earlier `set_vars` node task assigns is accepted when
validation can compute the assigned value before the run, because the value
uses only literal text, `.vars`, `.inputs`, `.inventory`, `.node.name`, or
the variable of a loop.

Validation rejects any other reference in a path and names it: a node fact
other than `.node.name`, `.node_tasks`, `.outputs`, `.secrets`, and a
`set_vars` value that is assigned only during the run, such as one that reads
a node fact or a node task's result.

For an inventory with the nodes `web01` and `web02`, the `file` node task
below installs `files/network_interfaces/web01` on `web01` and
`files/network_interfaces/web02` on `web02`:

```yaml
- file:
    path: /etc/network/interfaces
    template: "files/network_interfaces/{{ .node.name }}"
    owner: root
    group: root
    permissions_mode: "0644"
```

`validate`, `explain`, `dryrun`, and `run` resolve the path for every selected
node and loop item and require each file to exist. A missing file is an error
before any connection; it names the node, the node task, and the resolved
path. The `explain` command shows the resolved path for each node, with the
origin of each value used in the resolved path. MMX saves the files'
contents with the run like any source file, so dryrun shows each difference
and `mmx resume` uses the saved contents.

## Rollout settings belong to the fleet stage

A role can run under different rollout policies. The
`configure_web_servers` fleet stage below processes web servers five at a
time, finishing each batch before starting the next batch. After a failure,
the run starts no additional nodes or later fleet stages. Admitted node
jobs finish normally.

In `mmx_project_settings.yml`, `max_nodes_at_once: 100` sets the project default.
`playbooks/deploy.yml` overrides the `max_nodes_at_once` project default for
the `configure_web_servers` fleet stage:

```yaml
fleet_stages:
  - id: configure_web_servers
    nodes: web
    max_nodes_at_once: 5
    wait_for_all_in_batch: true
    on_node_failure: stop_run

    node_tasks:
      - role: web_server
        inputs:
          port: "{{ .vars.http_port }}"
```

On a fleet stage, `max_nodes_at_once` is a node count or a percentage of the
fleet stage's selected nodes after any `--limit`, such as `"34%"`, rounded
down and never less than one node. The project setting takes a count only. A
[`for_each_node` loop](#loop-over-nodes-with-for_each_node) takes the fleet
stage's rollout settings for work that involves other nodes, such as a load
balancer.

The [scheduling](execution.md#run-across-the-fleet-with-bounded-concurrency) and
[failure policy](execution.md#choose-what-a-node-failure-stops) contracts define
the fleet stage's rollout settings.

## Values have a short route to the place they are used

A node's `.vars` come from one order. Each layer replaces earlier values by
whole key, with no deep merge:

```text
project vars → inventory vars → its groups' vars → its own node vars
```

Project, inventory, group, and node `vars` are literal data. A node's groups
never set the same variable ([inventory](#inventory-is-a-list-of-nodes-not-a-second-programming-language)),
so the group layer has one value for each name. [Command-line
variables](#command-line-variables) are not a layer: each has exactly one
source. Role inputs are not a layer either: a role sees only the call's `inputs`,
otherwise each input's literal `default`; missing required inputs and unknown
input names are errors. During execution, [`set_vars`](#task-results-and-runtime-variables)
assigns local values at an explicit point in the task list.

References are `{{ .vars.name }}` in playbook tasks and `{{ .inputs.name }}` in
roles. Data references share dotted naming with Go templates, but task
arguments accept only data references, not template programs:

- A whole-value reference keeps its type; a reference embedded in text produces
  text, and embedding a list or mapping is an error. So a module input of any
  type, such as an integer `uid` or a list of `keys`, takes one
  whole-value reference; the value is checked against the input's type where
  it resolves ([playbook file reference](../../reference/playbook-file.md#a-number-a-boolean-a-list-or-a-mapping-from-a-reference)). A shell `env` value that
  is one whole reference to a list or mapping is passed to the script as JSON
  text.
- Brackets look up a literal key or another reference:
  `.outputs.inspect_app["app-01"].update_needed` or
  `.outputs.inspect_app[.app].update_needed`.
  The reference inside the brackets cannot read a secret; validation rejects it.
  Secret delivery sends no value used only as a lookup key, so the lookup
  would block the node job.
- A missing value is an error, never an empty string. Names are case-sensitive.
- Values are substituted once and not reinterpreted.
- File `content` accepts references; `template` renders a Go template; `source`
  copies bytes unchanged. Shell `script` is literal; pass values through `env`.
  Every field that runs as a shell script is literal too: `shell`'s `dryrun`
  and the scripts of its two guards, `cron_job.command`,
  `wait_for.command_succeeds`,
  `systemd_scheduled_job.script`, `edit_file`'s content filter, and a
  `check_new_file` shell. In shell script fields, validation rejects
  references to MMX values, such as a role input
  (`{{ .inputs.sites_dir }}`), a fact, or a loop's `as` name.
  MMX does not substitute references in shell script fields, so the shell
  would receive the braces themselves. The validation message shows how to
  pass the value through `env`. A template for another tool, such as
  docker's `{{.State.Status}}`, names no MMX value and stays literal.
- A reference whose body is one back-quoted string produces that text
  unchanged. The back-quoted reference form lets a value that accepts
  references hold a literal `{{` for another tool. Go templates already use
  the back-quoted reference form:

  ```yaml
  - systemd_unit:
      name: app.service
      content: |
        [Service]
        ExecStart=/usr/bin/docker run --rm --name app --log-opt tag={{`{{.Name}}`}} registry.example.com/app:1.4.2
  ```

  The unit file gets `tag={{.Name}}`. A `}}` on its own needs nothing. Shell
  scripts and `source` files are literal already. To produce a literal `{{`
  in a `template` file, use a reference whose body is one back-quoted string.
  The validation error for a reference MMX cannot read names the
  back-quoted reference form.

The configuration loader keeps each value's file, line, and field.
Errors name the consuming field and, when
different, where the value came from, without quoting secrets.
`validate` and `explain` check everything resolvable locally, including in work
that may be skipped, without contacting nodes, running modules, or unlocking
unused secrets. Fixed node task result field names are checked locally,
including beneath `items[n]`. Values from facts, node task results, and
loop items are checked when node tasks run, because the runtime values do
not exist on the controller machine during local validation. Runtime
validation also checks a tracked service path derived from a node task
result inside a node task block.
`mmx explain deploy --limit web01` shows
selected nodes, ordered tasks, effective non-secret arguments with their
sources (including the group or command line that supplied a value), and timing
settings, and labels runtime values as unresolved. The
[diagnostics example](../../../examples/diagnostics/README.md) shows the
selected nodes, ordered tasks, argument values and sources, timing settings,
and unresolved runtime values.

### Command-line variables

A value that changes per invocation, such as a release version passed by CI,
is declared in the chosen playbook and set with `--var NAME=VALUE`. For example,
the `command_line_vars` declaration goes alongside `fleet_stages` in
`playbooks/deploy.yml`:

```yaml
command_line_vars:
  release:
    type: string
    default: "1.4.2"
  run_migrations:
    type: boolean
    default: true
```

```sh
mmx dryrun deploy --var release=1.4.3
mmx run deploy --var release=1.4.3 --var run_migrations=false
```

Playbook tasks read `{{ .vars.release }}`; the value is the same on every node. Only the
chosen playbook declares command-line variables, each with a `type` of
`string`, `integer`, or `boolean` and an optional literal `default`. A
declared name cannot also appear in project, inventory, group, or node
`vars`, so its value is
the `--var` value, otherwise its default. With a playbook name, `validate` and
`nodes` check the declarations and any values given; `explain`, `dryrun`, and
`run` also require a value for each variable without a default. Without a
playbook name, `validate` and `nodes` reject `--var`.

`--var` is repeatable. The text after the first `=` is the value: a string is
taken literally and never parsed as YAML or a reference; an integer is decimal;
a boolean is `true` or `false`. An undeclared name, a wrong type, or a repeated
name is a usage error before any connection, listing the declared names.
`explain` shows each value and whether it came from `--var` or the default.

Command-line variable values are saved with the run. `mmx resume` reuses the
saved command-line variable values and rejects `--var`; a new `mmx run deploy`
uses only the values given to the new command and the current defaults.
Command-line variable values are not part of the run's identity. Dryrun
accepts and validates `--var` values under the rules for `run`.
An unchanged rerun using the command-line variable values from the earlier
run reports unchanged.

Command-line variables are public: they appear in the process list, `explain`,
and saved history. Command-line variables cannot be declared secret or set
`.secrets`; pass a secret value through an
[environment variable input](#environment-variable-inputs).

## Task results and runtime variables

| Need | MMX spelling |
|---|---|
| Refer to a task result | `id: app_config`, then `.node_tasks.app_config` |
| Declare configuration values | `vars:` |
| Assign values during execution | `set_vars:` |
| Run work conditionally | `when:` |
| Display values | `show:` |

A named task exposes its result with no separate `register` field:

```yaml
- id: app_config
  file:
    permissions_mode: "0644"
    path: /etc/myapp/app.yml
    template: files/app.yml.tmpl

- set_vars:
    update_needed:
      any:
        - equals: ["{{ .node_tasks.app_config.change }}", changed]
        - equals: ["{{ .node_tasks.app_binary.change }}", changed]

- when: "{{ .vars.update_needed }}"
  shell:
    script: /usr/local/bin/myapp-migrate
```

Results carry the module's fields, such as `status`, `change`, shell
`exit_code`/`stdout`/`stderr`, HTTP `response_body`/`http_status_code`, and structured `value`
or `outputs`. The HTTP `http_status_code` is separate from the node task
`status` (such as `succeeded` or `failed`).

The `http` input `expected_http_status_codes` lists the response codes that
count as success and defaults to `[200]`. `http` makes one request unless
the node task sets `retry`. Use `http` when a later node task needs the
answer; use `wait_for` to wait until ready.

The [result contract](modules.md#every-module-follows-the-same-contract)
distinguishes changed, unchanged, predicted (dryrun expects a change), and
unknown (MMX cannot determine whether a change occurred or would occur).
A condition does not treat unknown as unchanged, and in a dryrun a condition
on a predicted or unknown change is not checked. A dryrun reports the
dependent node task as `not_checked` when a condition reads a predicted or
unknown change.

A failed command keeps the command's output for failure handlers; work that
did not run has no result.

A command or an HTTP response that returns JSON is read by field, with no
text matching. A shell result has `stdout_json`, which parses `stdout` as
JSON. The `http` and `wait_for` results have `response_body_json`, which
parses `response_body` as JSON. Ordinary references read fields of the
parsed JSON value:

```yaml
- id: balancer
  shell:
    script: ceph balancer status --format json
    changed_when: false

- when: "{{ .node_tasks.balancer.stdout_json.active }}"
  shell:
    script: ceph balancer off
```

Reading `stdout_json` or `response_body_json` when the output is not valid
JSON is an error that says so, and a missing key is an error like any missing
value.

`set_vars` evaluates all its values against the state before the task, then
applies them together; if any fails, none apply. A value that is exactly one
[predicate form](#conditions) (such as `equals`, `contains`, `all`, or
`number_at_least`) becomes a boolean. Assignments may introduce or replace local `.vars`, including
configuration-derived values; inputs, facts, results, and outputs stay
read-only. Assignments stay within the current node's fleet stage, loop
iteration, or role invocation. Node task block phases share one scope,
and later fleet stages receive values through
[outputs](#outputs-and-fleet-stage-outcomes).

References to node task results can name only earlier node tasks within
the node's current fleet stage or role invocation.
`.this_node_task` is the current node task's own result in `until`,
`retry_when`, `changed_when`, and `failed_when`.
Crossing a fleet stage or role boundary uses [declared outputs](#outputs-and-fleet-stage-outcomes).

Module pages that rely on this section:
[`shell`](../../reference/modules/shell.md).

## Show messages and runtime values

`show` displays a message or any value at its position in the task list, using
the ordinary reference rules:

```yaml
- show: "Deploying release {{ .vars.release }}"

- show:
    desired_release: "{{ .vars.release }}"
    inspection: "{{ .node_tasks.check_update }}"
```

Output identifies the node, fleet stage, task, and loop item. Saved results
and JSON output keep the value's structure. `show` makes
no managed change, runs during dryrun, and marks values that are not yet
known rather than showing them as false or empty. Secrets are redacted
before display or storage, including secrets nested inside structured
values and secrets inserted into text.
Explicit `show` output stays visible when unchanged tasks are summarized; the
[output contract](workflow.md#human-output-is-a-view-of-structured-results)
owns presentation.

## Outputs and fleet stage outcomes

| Reference | Meaning |
|---|---|
| `.node_tasks.check_update` | Task result within the current fleet stage or role invocation. |
| `.vars.update_needed` | Local variable, including `set_vars` values. |
| `.outputs.inspect_app.app01.update_needed` | Output published by the earlier `inspect_app` fleet stage on `app01`. |
| `.fleet_stages.inspect_app.app01.status` | Outcome of the `inspect_app` fleet stage on `app01`, available even when the fleet stage failed on `app01`. |

A fleet stage publishes values in `outputs`, evaluated after its tasks
succeed. The `/usr/local/bin/myapp-needs-update` helper exits 0 when the
application is current and 2 when the application needs an update:

```yaml
- id: inspect_app
  nodes: app

  node_tasks:
    - id: check_update
      shell:
        script: /usr/local/bin/myapp-needs-update
        success_exit_codes: [0, 2]
        changed_when: false
  outputs:
    update_needed:
      equals: ["{{ .node_tasks.check_update.exit_code }}", 2]
```

A later fleet stage reads `{{ .outputs.inspect_app.app01.update_needed }}`, or inside a
fleet stage loop over node IDs, `{{ .outputs.inspect_app[.app].update_needed }}`. A
fleet stage may omit `node_tasks` to publish facts or configured values only.

Fleet stage outputs are always keyed by node ID. Reading one entry needs that node's
fleet stage to have succeeded; reading the whole mapping needs the outputs of
every selected node, so failed nodes never silently disappear from a collection.
A node that the fleet stage's `when` skipped is left out of the whole mapping,
as is a loop iteration that the loop's `when` skipped: the playbook chose to
skip the node or loop iteration, and the run shows the node or iteration as
skipped. A read of the skipped node's or skipped loop iteration's output
entry is blocked and names the condition. A whole collection can feed a
template or role input, such as backend addresses for a load balancer.

A role declares values in `outputs:`, evaluated after the role's node tasks
succeed; the caller reads
`.node_tasks.<role_call_id>.outputs.<name>`, independent of the role's task names.

A needed output from a failed, skipped, or unstarted producer blocks the
consumer and names the cause. During dryrun, a value that cannot be known yet
leaves dependent work reported as not yet checkable, not false. `.fleet_stages`
reports status and change without requiring success, but never satisfies a
`requires` entry. Reading an output already implies the dependency, so it needs
no duplicate `requires`. The
[output recovery contract](execution.md#completed-outputs-survive-resume)
owns restoring outputs on resume.

### Save outputs to files on the controller machine

A fleet stage can write its outputs to files on the controller machine with
`save_on_controller_machine`, a list beside `outputs`. Each entry writes
files and runs no command. The `collect_hardware_report` fleet stage saves
one hardware report for each node:

```yaml
- id: collect_hardware_report
  nodes: all

  node_tasks:
    - id: hardware
      shell:
        script: ip address && lsblk
        dryrun: ip address && lsblk
        changed_when: false
  outputs:
    hardware_report: "{{ .node_tasks.hardware.stdout }}"
  save_on_controller_machine:
    - output: hardware_report
      path: "server_details/{{ .node.name }}.txt"
      permissions_mode: "0644"
```

After the run, the project holds `server_details/web01.txt`,
`server_details/db01.txt`, and so on.

| Field | Meaning |
|---|---|
| `path` | Where to write. A relative path resolves from the project root and cannot leave it. |
| `permissions_mode` | The file's permissions, required. |
| `secret_readable_by_group` | Acknowledges group read access. Required as `true` when a secret file allows group reading but not public reading. Missing acknowledgement is an error; see the [read-bit rule](../../reference/modules/file.md#files-that-hold-secrets). |
| `dangerously_let_every_account_read_secret` | Acknowledges read access for every account. Required as `true` when a secret file allows public reading, and covers group reading too. Missing acknowledgement is an error; see the [read-bit rule](../../reference/modules/file.md#files-that-hold-secrets). |
| `output` | The name of an output declared in `outputs` on the fleet stage containing `save_on_controller_machine`. MMX writes one file for each node, so `path` must contain `{{ .node.name }}`. Text and binary values are written unchanged; numbers and booleans become text. Mappings and lists require a `template` entry. |
| `template` | A template file rendered once from the fleet stage's whole output collection, read as `.outputs.<fleet stage id>`. A relative template path starts at the project root. MMX writes one file. |

An entry has exactly one of `output` and `template`. A `template` entry needs
the outputs of every selected node that the fleet stage's `when` did not skip,
so MMX rejects a `--limit` that narrows a fleet stage with a `template`
entry, as MMX does for any reader of a whole collection.

A file from an `output` entry is written when the node's fleet stage has
succeeded. A `template` entry needs every selected node not skipped by
the fleet stage's `when` to succeed before writing the whole-collection
file. A
[secret](#delivery-and-output) value follows the
[read-bit rule](../../reference/modules/file.md#files-that-hold-secrets) and goes from
the controller process's memory to the destination file on the controller
machine; the secret value never enters saved history. Dryrun reports the
destination file on the controller machine as a proposed change with the
file's difference and writes nothing. `mmx resume` keeps a file when the run
recorded a successful write and the path still exists, without comparing
the file's contents. Resume writes the files that are still missing.

## Loops and conditions remain visible

The `loop` / `as` form works on a node task, node task block, or sequence
of fleet stages. For a list, `as` names the
variable that receives each value, in list order. For a mapping, the `as`
variable receives each entry as `key` and `value`, visiting keys in sorted
order. Use a list when order matters. The `as` name cannot be a reserved
scope name such as `vars`, `node`, `node_tasks`, or `this_node_task`.

In `mmx_project_settings.yml`:

```yaml
vars:
  accounts:
    alice: {shell: /bin/bash}
    bob: {shell: /bin/zsh}
```

In `playbooks/accounts.yml`:

```yaml
fleet_stages:
  - id: accounts
    nodes: all

    node_tasks:
      - loop: "{{ .vars.accounts }}"
        as: account
        user:
          create_home_directory: true
          name: "{{ .account.key }}"
          shell: "{{ .account.value.shell }}"
          state: present
```

Looping a file task inside a node task block produces one activation after
all files.
Looping a node task block produces one node task block per entry. Looped
node task blocks and fleet stage sequences need a stable `key`, such as
`key: "{{ .account.key }}"`, to identify each iteration in results and
recovery. Duplicate keys are errors.
When an individual node task or role call loops over a list without
`key`, the iteration key is the zero-based index as text.
When an individual node task or role call loops over a non-secret mapping
without `key`, the iteration key is the mapping entry's key.

A looped node task, role call, or node task block with an outer task `id`
exposes ordered per-item results through `.node_tasks.<id>.items`, each with
an `iteration_key`.

A loop over fleet stages runs in the controller process and finishes one item's fleet stages before
the next. Each fleet stage still selects its own `nodes`; `.node` is that fleet stage's
execution node. Each iteration keeps its own variables and results, and a loop
can publish per-iteration `outputs`, keyed by its `key`. Use `loop` for a list
of things that are not nodes. With `tenants: [acme, globex]` in the project's
`vars`, the `add_tenants` loop creates each tenant's database on `db01` and
then loads the tenant's configuration on the app servers:

```yaml
- id: add_tenants
  loop: "{{ .vars.tenants }}"
  as: tenant
  key: "{{ .tenant }}"
  fleet_stages:
    - id: create_database
      nodes: db01

      node_tasks:
        - postgresql_database:
            owner: postgres
            name: "{{ .tenant }}"
            state: present

    - id: load_tenant_config
      nodes: app

      node_tasks:
        - shell:
            script: /usr/local/bin/myapp-load-tenant "$TENANT"
            env: {TENANT: "{{ .tenant }}"}
            skip_if_this_command_succeeds: /usr/local/bin/myapp-has-tenant "$TENANT"
```

Saved fleet stage paths include the iteration, such as
`add_tenants[acme]/create_database`, and
`mmx results --fleet-stage 'add_tenants[acme]'` shows the enclosed fleet stages.

### Loop over nodes with `for_each_node`

When the items of a fleet stage loop are nodes, write `for_each_node` in
place of `loop`. Use `for_each_node` when one node's share of the work
happens partly on another node. For example, take an app server out of the
load balancer, update the app server, and put the app server back.
Another example is creating a virtual machine on the machine's hypervisor
before configuring the virtual machine.

`for_each_node` takes the same selectors as `nodes`, and `as` (required)
names the variable that holds the current node's ID. The node ID is each
iteration's key, so the loop takes no `key`. For the inventory shown below:

```yaml
nodes:
  app01: {address: app01.example.net, ssh_user: deploy}
  app02: {address: app02.example.net, ssh_user: deploy}
  app03: {address: app03.example.net, ssh_user: deploy}
  lb01: {address: lb01.example.net, ssh_user: deploy}

groups:
  app:
    nodes: [app01, app02, app03]
```

`playbooks/deploy.yml` updates the app servers one at a time:

```yaml
fleet_stages:
  - id: rollout
    for_each_node: app
    as: app_server
    max_nodes_at_once: 1
    on_node_failure: stop_run
    fleet_stages:
      - id: take_out_of_load_balancer
        nodes: lb01

        node_tasks:
          - shell:
              script: /usr/local/bin/lb-disable "$SERVER"
              env: {SERVER: "{{ .app_server }}"}

      - id: update
        nodes: "{{ .app_server }}"

        node_tasks:
          - role: app_server
          - id: ready
            shell:
              script: /usr/local/bin/myapp-health
              changed_when: false
        outputs:
          healthy: {equals: ["{{ .node_tasks.ready.exit_code }}", 0]}

      - id: put_back_into_load_balancer
        nodes: lb01

        node_tasks:
          - shell:
              script: /usr/local/bin/lb-enable "$SERVER"
              env: {SERVER: "{{ .app_server }}"}
    outputs:
      healthy: "{{ .outputs.update[.app_server].healthy }}"
```

In the `app02` iteration, `.app_server` is `app02`: the first and third
enclosed fleet stages run on `lb01` with `SERVER=app02`, and the second runs
on `app02` itself. `.outputs.update[.app_server].healthy` reads the output
that the enclosed fleet stage `update` published on `app02`. Later work
reads the loop's own output as `.outputs.rollout.app02.healthy`. Saved fleet
stage paths are `rollout[app02]/update` and so on.

The loop is the unit of rollout. `max_nodes_at_once` (a count or a
percentage such as `"34%"`), `wait_for_all_in_batch`, `on_node_failure`, and
`failed_nodes_allowed` are written on the loop and count the loop's nodes.
The enclosed fleet stages reject `on_node_failure` and
`failed_nodes_allowed`.

Without `max_nodes_at_once` the loop finishes one node's fleet stages before
starting the next node. `except_nodes` and
[`skip_when_no_nodes_are_selected`](#leave-out-a-fleet-stage-that-selects-no-nodes)
on the loop work as on a fleet stage. The
[execution contract](execution.md) owns admission, batches, what a failure
stops, and resume.

`--limit` picks the loop's iterations, and the enclosed fleet stages of a
kept iteration reach the nodes they name
([limits](#limit-a-run-to-part-of-the-inventory)). So `mmx run deploy --limit
app02` takes `app02` out of `lb01`, updates it, and puts it back, and prints
`lb01` as a node outside the limit that does work for it.

The loop forms and the limit answer different questions:

| Form | Purpose of the form |
|---|---|
| `loop` on a node task or node task block | Repeat work on one node for each item of a list or mapping, such as each account. |
| `loop` on a sequence of fleet stages | Repeat work across nodes for each item that is not a node, such as each tenant. |
| `for_each_node` | Repeat a sequence of fleet stages for each node, where part of each node's work runs on other nodes. |
| `--limit` | Run a command on part of the inventory for the current invocation. The `--limit` flag never appears in a playbook. |

### Conditions

`when` takes a boolean reference or a typed predicate:

```yaml
when:
  equals: ["{{ .node.arch }}", arm64]
```

| Predicate | True when |
|---|---|
| `equals: [a, b]` | The two values are equal. |
| `contains: [text, literal]` | The text contains the literal. |
| `matches: [text, pattern]` | The text matches the Go regular expression, unanchored. |
| `all: [...]`, `any: [...]`, `not: ...` | `all` is true when every enclosed condition is true. `any` is true when at least one enclosed condition is true. `not` is true when the enclosed condition is false. `all` and `any` short-circuit left to right. |
| `number_equals`, `number_less_than`, `number_at_most`, `number_greater_than`, `number_at_least`: `[left, right]` | The left number is equal to, less than, at most, greater than, or at least the right number, respectively. An operand may be text with whitespace around a number, such as a command's output. |
| `version_equals`, `version_less_than`, `version_at_most`, `version_greater_than`, `version_at_least`: `[left, right]` | The left version is equal to, less than, at most, greater than, or at least the right version, respectively. MMX compares components numerically from left to right. Missing trailing components count as zero, so `1.2` equals `1.2.0`. A version is dotted numbers with an optional leading `v`, such as `1.30.2` or `v2.4`. Package version syntax, such as `5:27.3.1-1~debian.12`, is not covered. |
| `path_exists: /absolute/path` | The path exists on the node, as any kind of file. |

For example, with a node task `installed` that prints the installed version
of a program and a node task `replication_lag` that prints a number of
seconds:

```yaml
- id: installed
  shell:
    script: kubelet --version | cut -d' ' -f2
    changed_when: false

- when:
    version_less_than: ["{{ .node_tasks.installed.stdout }}", "1.30.2"]
  role: upgrade_kubelet

- id: replication_lag
  shell:
    script: /usr/local/bin/replication-lag-seconds
    changed_when: false

- assert:
    that:
      number_at_most: ["{{ .node_tasks.replication_lag.stdout }}", 30]
    message: "Replication lag is {{ .node_tasks.replication_lag.stdout }} seconds"
```

The predicates listed in the conditions table serve `when`,
`set_vars`, outputs, shell result rules such as
`failed_when: {contains: ["{{ .this_node_task.stderr }}", "ERROR:"]}`, and retry
conditions. `path_exists` is the exception: it inspects the node, so it is
accepted only in the `when` of a node task or node task block, and it is
evaluated when that node task is reached. In dryrun, a path that an earlier
node task in the node job evaluating `path_exists` would create is reported
as not yet checkable.
A number or version operand of another shape fails the predicate with a
message; it never counts as false. A predicate that reads truncated output fails with a clear reason
instead of guessing. Conditions have no function calls or shell evaluation.

Admission, stopping policies, and `requires` apply first. Then `when` is
evaluated. A false condition skips the work without resolving its arguments.
Without a blocking requirement, a disabled optional producer and its
disabled consumer both skip cleanly.
`when` never overrides a requirement or stopping policy.

A `when` on a fleet stage or on a `for_each_node` loop is checked by the
controller process for each node, just before that node would start. A node
whose condition is false is recorded as skipped, with the condition as the
reason. The skipped node starts no node job and takes no place in a batch.
Such a `when` reads `.vars`, `.inventory`, the node's name as `.node.name`,
a loop's variable, the outputs of earlier fleet stages, and the node's
facts, such as `.node.distribution.id`. Only the node measures its facts, so
for a condition that reads node facts, the controller process asks the node
for node facts just before deciding. A node that cannot answer fails as the
node's job would have.
A condition without facts contacts no node. This playbook updates the replicas of a
PostgreSQL cluster one at a time and leaves the leader alone:

```yaml
fleet_stages:
  - id: find_leader
    nodes: postgres_cluster

    node_tasks:
      - id: role_in_cluster
        shell:
          script: /usr/local/bin/cluster-role --format json
          changed_when: false
    outputs:
      is_leader: "{{ .node_tasks.role_in_cluster.stdout_json.is_leader }}"

  - id: update_replicas
    for_each_node: postgres_cluster
    as: database_node
    when:
      not: "{{ .outputs.find_leader[.database_node].is_leader }}"
    max_nodes_at_once: 1
    on_node_failure: stop_run
    fleet_stages:
      - id: update
        nodes: "{{ .database_node }}"

        node_tasks:
          - role: postgresql_server
```

`postgres_cluster` is an inventory group. The fleet stage `find_leader` runs
on each node in `postgres_cluster` and publishes `is_leader`, `true` or
`false`, from the JSON the script prints. In the loop, `.database_node` is
the node the iteration is for, so
`.outputs.find_leader[.database_node].is_leader` is that node's answer.
`mmx nodes` shows the loop's nodes with the note "narrowed
during the run by `when`", so every node the run may touch is known before it
starts. `mmx resume` keeps the answers recorded at the start of the run.

## Facts and results are observations, not defaults

MMX collects common node facts automatically, once per node job and before any
task, and exposes them through `.node` in tasks, roles, and templates. Dryrun
also collects node facts. `explain` marks node facts as runtime values.
Resume and a new run collect fresh facts.

| Reference | Meaning |
|---|---|
| `.node.name` | The node's inventory name. |
| `.node.hostname` | The machine's hostname. |
| `.node.fully_qualified_name` | The machine's fully qualified domain name, such as `web01.example.net`. |
| `.node.os` | Lowercase keys and string values from `os-release`, unchanged. |
| `.node.distribution.id` | The `os-release` `ID`, such as `debian`, `ubuntu`, `rocky`, `almalinux`, `arch`, or `opensuse-leap`. |
| `.node.distribution.version` | The `os-release` `VERSION_ID` as text, such as `12` or `24.04`. Unavailable on a rolling release. |
| `.node.distribution.major_version` | Integer: the digits of the version before the first dot (`12`, `24`). Unavailable on a rolling release. |
| `.node.distribution.codename` | The `os-release` `VERSION_CODENAME`, such as `bookworm` or `noble`. Unavailable where the distribution sets none. |
| `.node.distribution.family` | `debian`, `redhat`, `arch`, or `suse`, from MMX's own table of IDs; for an ID the table lacks, the first `ID_LIKE` word the table knows. On a distribution of another family, the family fact is unavailable, with the reason for unavailability. |
| `.node.package_manager` | The package manager of the node's family: `apt`, `dnf`, `pacman`, or `zypper`. On a distribution of another family, the package manager fact is unavailable, with the reason for unavailability. |
| `.node.selinux.mode` | `enforcing`, `permissive`, or `disabled`. `disabled` also when selinuxfs is mounted but no policy is loaded. |
| `.node.virtualization.type` | What `systemd-detect-virt` reports: `none` on physical hardware, otherwise the virtual machine or container technology, such as `kvm`, `vmware`, `lxc`, or `docker`. |
| `.node.arch` | `amd64` or `arm64`. |
| `.node.kernel.release` | Kernel release. |
| `.node.boot_id` | Current boot UUID. |
| `.node.cpu.count` | Online logical CPUs (a VM's vCPUs). |
| `.node.memory.total_bytes` | Usable RAM from `MemTotal`; excludes swap and is not free memory. |
| `.node.memory.total_mib` | Usable RAM from `.node.memory.total_bytes`, converted to MiB and rounded down. |
| `.node.memory.swap_total_bytes` | Configured swap capacity. |
| `.node.network.interfaces` | Map by interface name: `index`, `mtu`, `mac`, and `addresses` (`family`, `address`, `prefix_length`). Each interface's addresses keep the kernel's address order, as shown by `ip address`. Includes loopback. |
| `.node.network.default_routes` | Default routes: `family`, `interface`, optional `gateway`, `metric`, and optional `source_address`, the address the kernel sends the route's packets from (`src` in `ip route`). |
| `.node.network.default_ipv4_address` | For the IPv4 default route with the lowest metric: its `source_address` when it has one, as a DHCP client's route does; otherwise the first IPv4 address that the kernel lists on the route's interface. An address added later, such as a keepalived virtual address, comes after the interface's own. Unavailable when the node has no IPv4 default route. |
| `.node.network.default_ipv6_address` | Uses the IPv6 default route with the lowest metric. Uses the route's `source_address` when present, otherwise the first IPv6 address in kernel order on the route's interface. Never uses a link-local address. An address added later comes after the interface's own. Unavailable when the node has no IPv6 default route. |
| `.node.storage.mounts` | Mounts: `id`, `path`, `source`, `filesystem`, `read_only`; local disk filesystems also have `total_bytes` and `available_bytes`. |
| `.node.storage.block_devices` | The node's nonzero-size block devices, sorted by kernel `name`; see [block-device fields](#block-device-fields). |

Collections are sorted for stable output.
Within each address family, default routes with equal metrics are ordered
by interface name, then gateway. MMX selects the first matching route.
Mount capacity is read only for local disk filesystems, never network,
FUSE, or virtual ones. A field whose inspection
fails is unavailable with a saved reason, not zero or empty; reading it is an
error that names the reason, while tasks that do not read it continue.

The distribution facts exist so that a project with more than one
distribution can say what differs. MMX has no layer that hides the
distribution: a module that drives one tool is named for that tool, and the
playbook or role chooses with `when`:

```yaml
- when:
    equals: ["{{ .node.package_manager }}", apt]
  apt_package:
    packages:
      - {name: apache2, version: newest_when_first_installed}

- when:
    equals: ["{{ .node.package_manager }}", dnf]
  dnf_package:
    packages:
      - {name: httpd, version: newest_when_first_installed}
```

A value that differs can also be a mapping looked up by a fact, such as
`.inputs.web_server_user[.node.distribution.family]`.

Templates can size configuration from capacity with the
[`percent` function](#template-functions). Facts stay fixed within a node
job. After a resize, the next run re-renders the template, and ordinary file
comparison and service hashes decide whether anything changes.

Saved results include `facts.collected_at`, `facts.values`, and
`facts.unavailable`. For each node, `mmx results` lists fields that changed
between observations within a run as `facts_changed`.
Facts are an observation per node job, not monitoring or a node database.

Use total capacity, not current free memory, so configuration does not
change with load. The [node-facts example](../../../examples/node-facts/README.md)
shows how to size
configuration from facts and apply configuration changes after a node resize.

`.inventory.nodes.<name>` exposes each node's `name`, `address`, and `vars`:
the values that differ by node, its groups' `vars` then its own. Values every
node shares (project and inventory `vars`, command-line variables) are read
through `.vars`. Each node job carries shared values once in the node job's
variables, rather than repeating shared values in every inventory node record.
`.inventory.groups.<name>` is a group's sorted node list, so a playbook can
pass a database address to a role as an ordinary input or loop over a group.
A template can loop over inventory node records or group node lists.
For example, a template can write one firewall rule file or one `/etc/hosts`
file from the whole inventory.
Discovered data from other nodes needs an explicit collecting fleet stage and
[outputs](#outputs-and-fleet-stage-outcomes). Within a shell task's own result
conditions, `.this_node_task.exit_code`, `.this_node_task.stdout`, and `.this_node_task.stderr` refer to
that command ([result rules of `shell`](../../reference/modules/shell.md#result-rules)).

Module pages that rely on this section:
[`selinux_boolean`](../../reference/modules/selinux_boolean.md),
[`selinux_file_context`](../../reference/modules/selinux_file_context.md),
[`selinux_mode`](../../reference/modules/selinux_mode.md),
[`selinux_port`](../../reference/modules/selinux_port.md).

### Block-device fields

`.node.storage.block_devices` lists each block device in the node's
`/sys/class/block`, except devices whose size is zero. MMX reads files and
links only. MMX runs no program and probes no device to collect
`.node.storage.block_devices`.

Every entry has `name`, `path`, `kind`, `size_bytes`, and `read_only`.
Other fields are omitted when they do not apply or are not known.

| Field | Meaning and source |
|---|---|
| `name` | The kernel name from `/sys/class/block`, such as `sdb1` or `dm-0`. |
| `path` | `/dev/<name>`. |
| `kind` | `partition` when the `partition` file exists; `lvm_logical_volume` when `dm/uuid` starts with `LVM-`; `device_mapper_device` for other `dm/` devices; `loop_device` for a `loop*` name with `loop/`; `raid_array` when `md/` exists; otherwise `disk`. |
| `size_bytes` | The `size` file's sector count multiplied by 512. |
| `read_only` | Whether the `ro` file says `1`. |
| `part_of_disk` | For a partition, its disk's kernel name, from the parent of the resolved sysfs link. |
| `built_on_devices` | For a device mapper device or RAID array, the underlying kernel names from `slaves/`, sorted. |
| `device_mapper_name` | The name from `dm/name`, also reachable at `/dev/mapper/<that name>`. Applies to LVM logical volumes too. |
| `stable_paths_by_id` | Every link in `/dev/disk/by-id/` pointing to the entry's block device, as full paths, sorted. |
| `mount_points` | Mount paths from `/proc/self/mountinfo` matching the device number in `dev` (`major:minor`), sorted. |
| `serial` | Udev's `ID_SERIAL_SHORT`, or `ID_SERIAL` if the short serial is absent. |
| `model` | Udev's `ID_MODEL`. |
| `partition_table_type` | Udev's `ID_PART_TABLE_TYPE`, such as `gpt` or `dos`, for a device that holds a partition table. A partition does not have `partition_table_type`. |
| `content_type` | Udev's `ID_FS_TYPE`, such as `ext4`, `xfs`, `swap`, `LVM2_member`, `zfs_member`, or `crypto_LUKS`. |
| `content_uuid` | Udev's `ID_FS_UUID`. |
| `content_label` | Udev's `ID_FS_LABEL`. |

Udev properties come from `E:<KEY>=<value>` lines in
`/run/udev/data/b<major>:<minor>`. Missing udev files are normal, including
on containers. Block-device entries with missing udev files omit the udev
fields.
The `content_*` fields record what udev found at its last probe, not a fresh
look at the device. Storage modules probe the device themselves before they
change anything. In a container, the kernel lists the block devices of the
server that runs the container.

### Template functions

A template can call the functions of [Sprig](https://masterminds.github.io/sprig/),
the function library that Helm templates use, under Sprig's names and
argument order. MMX offers Sprig's hermetic set: the functions whose result
depends only on their arguments. Left out are the functions that read the
clock (`now`, `date`, `ago`), produce random values (`randAlphaNum`,
`uuidv4`, `shuffle`, `genPrivateKey`, `htpasswd`, `bcrypt`), read the
controller machine's environment (`env`, `expandenv`), or use the network
(`getHostByName`). A template that calls an excluded function fails
validation with the function's name. The same template therefore renders
the same bytes on every run, and an unchanged rerun reports unchanged.

MMX adds two functions. `toYaml` renders a value as YAML with sorted keys, as
in Helm. `percent` returns an integer percentage, rounded down, of a
non-negative capacity for a percentage from 0 to 100:

```text
workers = {{ .node.cpu.count }}
cache_mib = {{ percent .node.memory.total_mib .inputs.cache_percent }}
replica_parallel_workers = {{ div .node.cpu.count 2 }}
Ciphers {{ join "," .inputs.ssh_ciphers }}

scrape_configs:
{{ .inputs.scrape_configs | toYaml | indent 2 }}
```

In the capacity and scrape configuration template example, `cache_percent`
is an integer input, `ssh_ciphers` a list input, and
`scrape_configs` a list of mappings that the role call passes in. Functions
are for templates only: task arguments accept data references, not function
calls.

## Secrets

A secret has a key, such as `database_password`, and a value. Secrets come
from environment variables on the controller machine or from a shared
encrypted project file. Environment variables and the encrypted project file
use the same `.secrets.<key>` references and delivery rules.

Module pages that rely on this section:
[`shell`](../../reference/modules/shell.md).

### Environment variable inputs

Bind inputs in `mmx_project_settings.yml`:

```yaml
secrets_from_env:
  database_password: SHOP_DATABASE_PASSWORD
```

The `secrets_from_env` binding needs no vault or passphrase. Only the
environment variable's name, `SHOP_DATABASE_PASSWORD`, appears in the project.
An environment variable supplies a secret only through such a
binding. A key listed in `secrets_from_env` reads the environment
variable instead of the vault entry.

### Shared age vault

The vault is one encrypted file of keys and their YAML values, protected by a shared
passphrase and kept in version control. Teammates use a password manager to
share the passphrase. MMX selects the vault as follows:

- By default, MMX uses `secrets_vault.age` at the project root.
- `vault_file: PATH` in `mmx_project_settings.yml` selects another vault.
  A relative path starts at the project root.
- `vault_file: PATH` in the selected inventory overrides the project setting.
  A relative path starts at the directory containing the inventory file.

Each environment's inventory can therefore name a separate vault. A reference
such as `.secrets.database_password` reads the vault entry unless the project
lists that key in `secrets_from_env`.

MMX uses the `filippo.io/age` Go library in passphrase mode and its standard
file format, so no separate age installation or key setup is needed. The
[CLI workflow](workflow.md#create-and-edit-shared-secrets) owns creating and
editing the vault, unlocking it by prompt, password file, or environment
value, and the commands that list its keys and show where each key is used.
The passphrase stays on the controller machine.

### Secrets a run needs

For `run`, `dryrun`, and `resume`, the controller process loads every named
secret read by the selected fleet stages before starting any node job.
A missing environment variable or vault, a failed decryption, or a missing
secret then fails the run before any node job starts. No node changes in a
run that could not finish for want of a fleet stage's secret.

A fleet stage left out before the run starts does not require secrets for
its node tasks:

- a fleet stage that [`--only-fleet-stages`](#run-only-some-fleet-stages)
  leaves out;
- a fleet stage or `for_each_node` loop whose selection resolves before the
  run, from node and group names, `vars`, and the inventory.
  MMX omits the work when the group is empty and
  [`skip_when_no_nodes_are_selected`](#leave-out-a-fleet-stage-that-selects-no-nodes)
  is set, or when the [limit](#limit-a-run-to-part-of-the-inventory) leaves
  out every selected node. The fleet stages inside an omitted loop are
  also left out;
- a fleet stage of the playbook's top level whose `when` is false on every
  node it selects, from each node's `vars` and name, which is how the run
  decides it as each node would start. A `when` that reads a fact, a secret,
  or an earlier fleet stage's output keeps the fleet stage's secrets.
  Reason: one playbook can then hold two ways of doing a job, chosen by a
  setting, without a placeholder secret for the way not chosen; a fleet
  stage inside a loop is not checked, because its `when` may read the
  loop's variable.

Inside a loop that runs, MMX checks each enclosed fleet stage's selection
as MMX checks a fleet stage outside a loop, once for all iterations. Inside
a `for_each_node` iteration, the limit does not narrow the enclosed fleet
stage's selection, as during the run.

A team that leaves an optional group empty in its inventory, such as a
staging server it does not have, therefore needs no placeholder secret for
that group's work. The reason: a placeholder in the vault would be a value
that no node ever uses, and it would hide a real missing secret on the day
the group gets a node. Likewise, `mmx run site --limit web01` needs no secret
that only the fleet stages this limit leaves out read.

A selection that resolves during the run, from `.outputs`, `.fleet_stages`,
or a loop's item, may still select nodes. The controller process therefore
loads the fleet stage's secrets before the run, because the controller
process cannot yet rule out selecting nodes.

A fleet stage that selects no nodes still writes its
`save_on_controller_machine` templates from the fleet stage's empty output
collection. The controller process therefore still loads secrets read by
such templates, even when the empty selection leaves out the node work.

Connection secrets are the `ssh_password` and `sudo_password` that the
inventory names for a node. The controller process reads connection secrets
when connecting to the node whose inventory entry names the secrets.

A resume uses the run's saved playbook, inventory, limit, and
`--only-fleet-stages` value. The resumed run therefore needs the secrets
required by the original run's selections.
[`mmx secrets usage`](workflow.md#find-where-a-secret-is-used) shows, for an
inventory, which keys only fleet stages that select no nodes read.

### Delivery and output

The controller process resolves connection passwords only for SSH or sudo
authentication. Connection passwords are never task inputs or stored on the
node.

Pass a secret to a role through an input declared `secret: true`; such inputs
take references, not plaintext values or defaults. File and unit content,
templates, shell `env`, and module inputs can consume secrets; paths,
identifiers, URLs, and loop keys stay public. Some module fields accept only
a secret reference, never plain text, so the value cannot sit in a task file.
Examples are `user.password_hash` and `apt_repository.password`.
Each module's document identifies the fields that require a whole secret
reference. A field requiring a whole secret reference accepts two forms:

- A whole reference to a named secret, such as
  `{{ .secrets.alice_password_hash }}`.
- Inside a role, a whole reference to a role input declared `secret: true`,
  such as `{{ .inputs.password_hash }}`. The role call must supply the input
  from a secret. A role input without `secret: true` is refused in fields
  requiring a whole secret reference.

A whole reference to a named secret or secret role input keeps the secret
value out of the YAML.

A node receives only secrets consumed by the node's assigned tasks.
When the controller process knows a selector, such as
`.secrets.passwords[.node.name]`, delivery includes only the selected entry
or field. Inventory assignment decides delivery. Node-reported facts cannot
grant access.

Secret values travel through protected input channels, never command lines.
Sensitivity follows values through results, assignments, and outputs; contents,
differences, arguments, and diagnostics derived from secrets show as
`[redacted]` while status and exit codes stay visible. Third-party modules are
trusted code and must keep values sensitive themselves.

A node task that reads a secret has its whole result hidden: its value,
output, differences, and failure reason.
The hidden failure reason reads
`details suppressed for a task using secrets`.
When a node task that reads a secret fails, the node keeps the suppressed
details for root in one file per node job under `/run/mmx/private/`.
The failure reason names the private file:
`details suppressed for a task using secrets; root can read them on the node
in /run/mmx/private/<job-id>-hidden-details-of-failed-node-tasks.txt for 72
hours, or until the node reboots`.

Within a role or node task block, MMX also hides results of node tasks after
the first secret read, because later tasks may print what the reading task
wrote.
The enclosing role call's or node task block's result is also hidden.

Passing a secret into a role input does not count as reading the secret and
does not hide results by itself. Inside the role, MMX hides the result of
the node task that reads the input, and results of subsequent node tasks.
The reading node task's result is hidden regardless of how the node task
reads the secret input: a module input, a template, `when`, a loop's items,
`set_vars`, `assert`, `retry.until`, or `check_new_file`.

Passing the secret role input into another role also does not count as
reading the secret. A node task that reads the secret role input inside the
nested role hides results of subsequent node tasks in the nested role.
The secret read also hides each enclosing role call's result and results of
subsequent node tasks within each enclosing role.

A role output that reads a secret input hides the role call's result.
The role output remains a secret value.

An `assert` can still show a failure reason after an earlier secret read,
or when the assertion condition reads a secret.
MMX shows the assertion's `message`, or the condition as written when no
`message` is set, unless the displayed text reads a secret.
A message that reads a secret or a hidden node task's result stays hidden.
The role call or node task block that fails because of the assertion shows
the assertion's failure reason.

A `shell` command can promise that its diagnostics hold no secret with
`output_holds_no_secret: true`, also inside `file.check_new_file`.
A run and a dryrun then show the command's standard output, standard error,
and failure reason. The enclosing `file` task also shows the failure reason
that quotes the check's failure.

Before showing command diagnostics, the node task execution engine replaces
every exact occurrence of a secret value with `[redacted]`.
Replacement covers secrets held by the node job or read by the node task.
The replacement rules are the rules for logins handed only to a tool.
The node task execution engine also replaces each secret value without
surrounding whitespace and each nonempty line without surrounding whitespace.
Replacement also covers secret mapping keys and the JSON text MMX gives a
secret list or mapping through `env`.

When the node job starts, the node task execution engine collects secret
forms from the node job's scope.
The node task execution engine also records secret forms read by a template
or the check's environment in the node task's trace.

The `shell` or enclosing `file` task remains secret except for the permitted
diagnostics. Differences, module outputs, and outputs made
from standard output stay hidden. Showing diagnostics after replacing secret
values lets a configuration checker explain a typo without exposing the
checked file.
MMX cannot recognize other forms of a secret, such as part of a secret line
or the Base64 encoding of a secret value. The promise must therefore cover
forms MMX cannot recognize too.

`output_holds_no_secret` defaults to `false`.
Project loading refuses `output_holds_no_secret: true` with
`output_is_secret: true`.
The [`shell` page](../../reference/modules/shell.md#secret-output) and
[`file` page](../../reference/modules/file.md#check-a-file-that-holds-secrets) own
the examples.

One kind of read is the exception: a login that MMX hands only to a tool.
Four core modules give a secret to their tool as a login and do nothing else
with it: `git` (`username`, `password`, `ssh_private_key`), `docker_image` and
`podman_image` (`registry_username`, `registry_password`), and
`docker_compose_project` (`registry_username` and `registry_password` in
`registry_logins`). The login reaches the tool through a file in the node
task's
[private directory](execution.md#a-node-tasks-private-directory-lives-in-one-place-that-the-node-empties)
or through the Docker daemon's socket on the node.

- A secret read through `git.username`, `git.password`, or
  `git.ssh_private_key` does not hide the node task's result.
  A secret read through `registry_username` or `registry_password` also
  leaves the node task's result visible in `docker_image`, `podman_image`,
  or `docker_compose_project.registry_logins`.
  Every other secret read in the node task hides the node task's result,
  for example in `when` or `git`'s `ssh_known_hosts_lines`.
  An earlier node task that reads a secret also hides the current node
  task's result when both node tasks belong to the same role or node task
  block.
  A role whose node tasks hand a secret input only to a tool as a login
  hides nothing.
- MMX replaces every exact occurrence of such a secret with `[redacted]` in
  everything the node task reports: the reason, standard output and standard
  error, the value and every text inside it, a module's outputs, differences,
  each try, each item of a loop, and the check of a new file.
  A full difference, which is a stored file, is left out of the node task's
  result.
- MMX replaces secret logins in plain text and in the forms the tool
  receives, stores, or sends. For Docker and Podman, replacement covers:
  - The Base64 text of the user name, a colon, and the password.
    Docker and Podman use the Base64 text in login files and
    HTTP basic authentication.
  - The Base64 text of the header that a dryrun sends to the Docker daemon.
  - The user name or password as held in a web request's address.
    For example, a registry's token-service address contains the
    user name.

  Git and SSH receive and report logins only in plain text.
- The replacement happens on the node, where the result is made, before the
  result reaches a retry condition, a later node task, a node job record, the
  node job's error, or the controller process.
- A login that is no secret, such as a user name written in the playbook, is
  not replaced, and an empty value is never replaced.
- A later node task that reads the result, such as an image ID or a commit,
  is not secret because of the login.
- The login inputs stay private values in every other respect:
  `mmx explain`, the plan, and a dryrun name the secret and never show its
  value.

A secret can also start on a node, when one node makes a value that other
nodes need, such as a cluster's join token. Two settings mark such a value
as secret at its source: `content_is_secret: true` on a
[`read_file`](../../reference/modules/read_file.md) task, which reads a file on the node, and
`output_is_secret: true` on a `shell` task, which marks the command's
`stdout` and `stderr`. An output made from such a value is secret with no
further marking:

```yaml
fleet_stages:
  - id: start_first_server
    nodes: server01

    node_tasks:
      - role: cluster_first_server
      - id: join_token
        read_file:
          path: /var/lib/rancher/k3s/server/token
          content_is_secret: true
    outputs:
      join_token: "{{ .node_tasks.join_token.content }}"

  - id: join_other_servers
    nodes: other_servers

    node_tasks:
      - role: cluster_server
        inputs:
          join_token: "{{ .outputs.start_first_server.server01.join_token }}"
```

`server01` is a node and `other_servers` a group in the inventory; the role
`cluster_server` declares `join_token` with `secret: true`. The value travels
from `server01` through the controller process's memory to the nodes whose
node tasks consume it, and `mmx results` shows `[redacted]`.

Durable bundles saved on the controller machine, saved history, and node
records hold secret references, not values. On the node, secret inputs and
private outputs live only in root-private files under `/run`. Secret inputs
are removed when the node job ends. Private outputs are removed when the
node job's records expire
([secret rules](../implementation-rules.md#secrets)). Resume resolves named
secrets again, so a rotated value takes effect and affected files are
re-rendered. A saved output that came from a secret is resolved only when
consumed. If a saved secret
output's value is no longer available, the consumer reports that the value
is unavailable without rerunning the producer.
For a secret that `read_file` read on a node, saved history keeps the node
and the path, and `mmx resume` reads the file again from that node. A secret
from a `shell` task's output has nothing to read again, so on resume its
consumers are blocked with a message that names the producing fleet stage
and task;
`mmx run <playbook>` produces the value again.

## Tests

Configuration tests live with the configuration loader and node task
execution engine ([`internal/config`](../../../internal/config) and
[`internal/engine`](../../../internal/engine)) and the workflow suites listed in
the [acceptance checks](../acceptance.md#acceptance-checks). The configuration
tests cover the schema and YAML policy, fleet stages and requirements,
inventory choice and limits,
`except_nodes`, `--only-fleet-stages`, one-time playbook names and the
`completed/` directory, group-variable order and conflicts, command-line
variables and their reuse on resume, defaults and retry bounds, references
and value origins, asset paths that try to leave their base, source paths
with references, `set_vars`, `show`, outputs and availability, files saved on
the controller machine, loops including `for_each_node` under a limit, every
predicate, the template function set, facts on real nodes of each target
distribution, both secret sources with redaction and scoped delivery, and
secrets that start on a node.

## Design decisions

| Decision | Reason |
|---|---|
| Project settings live in `mmx_project_settings.yml`; named playbooks contain only fleet stages and command-line variable declarations. | Settings live in one place, so playbooks do not repeat them. |
| `resume` continues a saved run; `retry` repeats one node task automatically. | `stop` and `resume` describe run-level actions, leaving `retry` one meaning. |
| One inventory per invocation, chosen by `default_inventory` or `--inventory`; files never merge. | An environment is a file you can read whole. |
| Selectors are a union of nodes and groups, minus `except_nodes`; subsets are `--limit`, which only narrows. | No pattern algebra to decode; a limit cannot skip prerequisites, and a new node is included without anyone adding it to a list. |
| An empty selection is an error unless the fleet stage sets `skip_when_no_nodes_are_selected: true`. | A group emptied by mistake fails loudly, while a playbook shared by teams keeps the fleet stages of groups that some teams do not have. |
| A whole output collection leaves out a node that the fleet stage's `when` skipped, while `requires.all_nodes` still needs that node to have succeeded. | A failed node must not quietly disappear from a collection. A node skipped by the fleet stage's `when` reflects the playbook's choice, made with the same per-node facts and variables as a selection. Refusing the collection would make a playbook name its nodes again in a setting of its own. A `requires` entry says the prerequisite work happened. If `when` skips a backup, work requiring the backup must not start. |
| Inside a `for_each_node` loop a limit picks iterations, and a kept iteration reaches the helper nodes its fleet stages name. | One node's share of the work stays whole; the helper nodes are printed before any connection. |
| No tags or start-at-task. `--only-fleet-stages` keeps whole fleet stages and rejects a selection that leaves out a fleet stage a kept one needs. | Tags let selection skip prerequisites the written order promised; a fleet stage declares its prerequisites, so selecting by fleet stage cannot. `resume` handles recovery. |
| One-time work is a dated playbook in `one_time_playbooks/`, moved to `completed/` when every node has it; roles hold no removal lines for finished migrations. | A destructive line left in a role is dangerous, and the directory shows what is still outstanding. |
| A relative asset path may leave the role directory but must stay inside the project root. An absolute path names a file anywhere on the controller machine. A shared file gets its own role, and a shared value is passed as an input. | Every file has one owning role, so two roles cannot drift apart over a file both install. |
| Source and template paths accept references only to values the controller process knows before it connects. | A missing file is reported before any node is touched. |
| No layer that chooses values or tasks by distribution; facts and `when` say what differs. | The reader sees which tool runs on which node. |
| Groups may set variables; one fixed order, and two groups setting the same variable for a node is an error, even with equal values. | Shared values live in one place without order-dependent group precedence, and no second copy can drift apart from the first. |
| `--var` sets only declared, typed, scalar command-line variables, saved with the run. | CI can pass a release version while every value stays visible in `explain` and fixed for resume. |
| Command-line variables never carry secrets. | Command lines and history are public; secrets come from environment bindings or the vault. |
| No dynamic-inventory plugins. | A generator program writes YAML in MMX's inventory format. |
| No `register`: a task `id` exposes `.node_tasks.<id>`. | One naming mechanism for results. |
| `set_vars` assigns local values only; inputs, facts, results, and outputs are read-only. | Values cannot change behind another node's or role's back. |
| `show` replaces `debug`, with no modes. | One task that displays a message or value. |
| Strict YAML with no anchors, aliases, merge keys, custom tags, or duplicate keys. | Reuse comes from variables and roles, which `explain` can trace. |
| Roles live at `roles/<name>/main.yml` with typed inputs, no ambient variables, no automatic dependencies, and nested calls only when enabled; no include or import variants. | The playbook shows every role and its order. |
| Facts are collected automatically once per node job; no gather task. | Facts are observations for the node job, not monitoring. |
| Cross-node data uses a collecting fleet stage and outputs; no continuous discovery. | Every value has a visible producer. |
| Go `text/template` with Sprig's hermetic functions, `toYaml`, and `percent`; task references are data, not expressions. | Templates use names people already know from Helm and render the same bytes every run; arguments stay predictable. |
| Predicates compare numbers and versions by name (`number_at_least`, `version_less_than`); no expression language. | A condition reads as a sentence and has no evaluation rules to learn. |
| Mappings loop directly as `key` and `value`. | No conversion filter to learn. |
| Secrets come from environment bindings or one shared-passphrase age vault. | No extra encryption tool or per-user keys. |
| A duration setting (`time_limit`, `time_limit_per_try`, `retry.pause_between_tries`, `reboot` and `wait_for` `wait_at_most`, `systemd_scheduled_job` `random_delay` and `stop_a_run_after`, `restore_unless_controller_confirms_within`) is written out or is one whole reference to a var. MMX puts each node's value in when the project loads and when it prepares the node job (`config.ResolveDurations`), checks it as the setting's duration, and refuses a reference to an output, a fact, or a secret, a var that `set_vars` sets in the fleet stage, and a reference in a role. | A team sets a wait such as a reboot's in one place, per server where servers differ (a cloud server returns in seconds, a dedicated server with hardware checks in minutes), instead of editing every playbook. The controller process waits for a reboot and the node sets time limits before a node task resolves anything, so the value must be known before the run; a value known only during the run would leave a wait unchecked until it is used. |
| A value read on a node is secret only when its task says so (`content_is_secret`, `output_is_secret`); `read_file` requires the choice. | The author decides at the source, and nothing is redacted or exposed by a guess. |
| A role call that passes a secret to a role input hides nothing by itself. Inside the role, MMX hides the result of the node task reading the secret input, and of every node task reading a value made from it. When the reading node task runs something on the node, MMX also hides the node tasks that start after it and the role call's result, as in a node task block; a read by `set_vars`, `show`, or `assert` hides only the role call's result and the node tasks that read the values it made. Each hidden result says why in `secret_because`, and an identifier field that reads a hidden output names the node task that read the secret. | Roles commonly reuse deployments. Hiding every node task whenever a role receives a secret would hide most of a real project. Hiding every node task would also hide login-only results. Node tasks before the first secret read have no secret text to report. A node task that ran on the node may have left the secret there, where a later node task could print it without reading a secret value, so the later node tasks are hidden; `set_vars`, `show`, and `assert` change nothing on the node, and their values keep their secrecy, so hiding the node tasks after them protected nothing and made task order decide whether a later output could name a file or a schedule. |
| An `assert`'s failure reason, its `message` or its condition as written, shows unless that text reads a secret, also after a node task that read one; the role call or node task block that fails because of it shows the same reason. | The node tasks after a read are hidden because they may print what the reading task wrote on the node. An assert reads no file on the node: its text comes only from values, which keep their secrecy, a hidden node task's result included, so MMX knows whether the text holds a secret. An assert that refuses a run, such as one that waits for approval of a restart, must be able to say why. |
| A secret in a login that a module hands only to its tool (`git`, `docker_image`, `docker_compose_project`, `podman_image`) does not hide the node task's result; MMX replaces the login with `[redacted]` in what the task reports. Every other read of a secret hides the whole result, except command diagnostics explicitly promised with `output_holds_no_secret`. | A task that needs a login is where the reason of a failure matters most (`pull access denied`, `manifest unknown`), and a hidden result would also take away the image ID, the commit, and the services' actions. These modules only pass the secret on, so MMX knows every form in which the tool holds it. A `shell` or `file` task can print a part of a secret or change its form in ways MMX cannot know, so showing command diagnostics there requires the program's explicit promise, `output_holds_no_secret`. |
| A failed node task whose result is hidden keeps its reason, exit code, and output on the node, in `/run/mmx/private/`, readable only by root; the hidden reason names the file. The node removes the file with the node job's records, after 72 hours, and a reboot removes it too. | A failure whose reason cannot be seen costs real time in an incident, and a command's own error line, such as psql's, is where the reason is. Replacing the secret's text in the output is not safe for a `shell` task, which can print a part of the secret or change its form; psql prints the input row that it refused. The secret never leaves the node it was sent to, which already holds it, and it stays out of MMX's output and records, as the node job's secret outputs do. |

## Beyond the first release

- SOPS as an additional secret source feeding the [secret rules](#secrets).
- Hardware topology facts: physical CPU layout and block-device graphs.
