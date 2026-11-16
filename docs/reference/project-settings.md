# Project settings reference

This page lists every field of `mmx_project_settings.yml`. The page also
lists the limits on the output that MMX saves, and what the project's
`.mmx/` directory holds. The **project settings** file, `mmx_project_settings.yml`,
marks the root of an MMX project and holds the settings that every playbook
of the project shares. The **controller machine** is the computer where you
run `mmx`. A **node** is a Linux server that MMX manages. To create a
project step by step, see [Get started](../get-started.md).

## The settings file

`mmx init` writes the smallest valid settings file:

```yaml
# mmx_project_settings.yml
project_name: mmx_example      # a project name you choose
default_inventory: example     # selects inventories/example.yml
```

A settings file that sets every field:

```yaml
# mmx_project_settings.yml
project_name: mmx_example
default_inventory: example
vars:                                 # project variables, for every node
  site_name: shop                     # a variable name you choose
vault_file: secrets_vault.age
secrets_from_env:
  deploy_sudo_password: MMX_DEPLOY_SUDO_PASSWORD   # a secret key and an environment variable you choose
max_nodes_at_once: 20
run_history_retention: 7d
node_task_defaults:
  time_limit: 10m
  time_limit_per_try: 2m
  retry:
    max_tries: 5
    pause_between_tries: 10s
allow_role_calls_within_roles: false
```

| Field | Type | Required | Default |
| --- | --- | --- | --- |
| [`project_name`](#project_name) | name | Yes | none |
| [`default_inventory`](#default_inventory) | inventory name | Yes | none |
| [`vars`](#vars) | mapping | No | no project variables |
| [`vault_file`](#vault_file) | path | No | `secrets_vault.age` |
| [`secrets_from_env`](#secrets_from_env) | mapping of secret key to environment variable | No | none |
| [`max_nodes_at_once`](#max_nodes_at_once) | integer | No | `100` |
| [`run_history_retention`](#run_history_retention) | duration or `forever` | No | `24h` |
| [`node_task_defaults`](#node_task_defaults) | mapping | No | no time limits, 3 tries, 2 s pause |
| [`allow_role_calls_within_roles`](#allow_role_calls_within_roles) | boolean | No | `false` |

These rules hold for the whole file, and for every other YAML file of the
project:

- **No other field.** A misspelled or unknown field stops every command that reads the project,
  such as `mmx: mmx_project_settings.yml: line 3 column 1 (runner_url): unknown field`.
  The settings for reaching each node, such as the SSH port, are on the
  node in the inventory: see the [inventory file reference](inventory-file.md).
- **Booleans are `true` or `false`.** MMX refuses `yes`, `no`, `on`, and
  `off`, so a value never changes its type by accident.
- **A value has the type that the field takes.** `max_nodes_at_once: "20"`
  fails with `expected int, got !!str`, and `run_history_retention: 0`
  fails with `expected string, got !!int`.
- **One plain YAML document.** MMX refuses a second document after `---`,
  duplicate keys, anchors and aliases (`&name`, `*name`), merge keys
  (`<<`), and explicit tags such as `!!str`.

**Where the file is.** MMX looks for `mmx_project_settings.yml` in the
current directory, then in each parent directory, the way `git` finds a
repository. So you can run `mmx` from any directory inside the project.
Outside a project, a command fails with
`no mmx_project_settings.yml found in the current directory or its parents`.

**When MMX checks the file.** Every command that reads the project checks
the settings file first. `mmx validate` checks the settings file, every
inventory, and every playbook without contacting a server. An error names
the file, the line, the column, and the field, such as
`mmx_project_settings.yml:3:20 (max_nodes_at_once)`.

## project_name

`project_name` names the project in the output of each run and in the
records that MMX keeps on each node.

```yaml
project_name: mmx_example
```

- **Type:** a name that starts with a letter or underscore and contains
  only letters, digits, underscores, or hyphens. The name need not match
  the project directory's name.
- **Required.** Without the field, every command that reads the project fails with
  `project_name is required, such as project_name: shop`.
- **On the nodes:** MMX files its records on each node under the name.
  After you rename the project, MMX treats the node as new: it doesn't
  find the old records. [When a playbook last succeeded on each server](../how-to/stop-resume-and-read-a-run.md#when-a-playbook-last-succeeded-on-each-server)
  shows them.
- **Give two projects that manage the same node different names,** so
  that the records of the two projects on the node stay apart.

## default_inventory

`default_inventory` names the inventory that a command uses when the
command has no `--inventory` option.

```yaml
default_inventory: example     # inventories/example.yml
```

- **Type:** an inventory's name: the file name in `inventories/` without
  `.yml`. A path fails with
  `default_inventory must be an inventory name, not a path`.
- **Required.** Without the field, every command that reads the project fails with
  `default_inventory is required: the inventory used when --inventory is omitted, such as default_inventory: production for inventories/production.yml`.
- **A missing file** fails with
  `there is no inventory production: inventories/production.yml does not exist; the project's inventories are: example`.
- **Choose a default that is safe to use by mistake,** such as your staging
  inventory. Then a command without `--inventory` reaches staging, and
  production needs `--inventory production`.
- [Choosing the inventory on the command line](inventory-file.md#choosing-the-inventory-on-the-command-line)
  lists the rules of `--inventory`.

## vars

`vars` sets project variables: values that every node of every inventory
gets, which node tasks read as `{{ .vars.NAME }}`.

```yaml
vars:
  site_name: shop              # a variable name you choose
  log_level: warn
```

- **Type:** a mapping. A value can be text, a number, a boolean, a list, or
  a mapping.
- **Default:** no project variables.
- **Order:** project variables come first; the inventory, the node's
  groups, and the node replace them. [`vars` in the inventory file](inventory-file.md#vars)
  owns the order and shows an example.
- **A command-line variable** that a playbook declares in
  `command_line_vars` must not also be in `vars`. Otherwise the project
  fails to load with
  `command-line variable release must not also be set in vars (mmx_project_settings.yml:4:12 (vars.release))`.
- [Use variables and templates](../how-to/use-variables-and-templates.md#where-a-value-comes-from)
  lists every place that a node task's value can come from.

## vault_file

`vault_file` names the project's vault: the encrypted file of secrets, such
as passwords, that node tasks read as `{{ .secrets.NAME }}`.

```yaml
vault_file: secrets/production.age
```

- **Type:** a path. A relative path starts at the project directory.
- **Default:** `secrets_vault.age` in the project directory. A project
  without a vault file needs no `vault_file`.
- **An inventory's own `vault_file` wins** for the commands that use that
  inventory. So a staging inventory can use a vault without production's
  secrets.
- The [secrets reference](secrets.md#which-vault-file) lists the order in
  which MMX chooses the vault, and [Keep secrets](../how-to/keep-secrets.md)
  creates a vault step by step.

## secrets_from_env

`secrets_from_env` reads secrets from environment variables on the
controller machine instead of from the vault. Each entry maps a secret key,
the name that node tasks and the inventory use, to an environment variable.

```yaml
secrets_from_env:
  deploy_sudo_password: MMX_DEPLOY_SUDO_PASSWORD   # secret key: environment variable
```

A node task or the inventory then reads the secret as
`{{ .secrets.deploy_sudo_password }}`, and MMX takes the value from
`MMX_DEPLOY_SUDO_PASSWORD`.

- **Type:** a mapping. A secret key starts with a letter or underscore and
  contains only letters, digits, underscores, or hyphens. An environment
  variable's name starts with a letter or underscore and contains only
  letters, digits, or underscores.
- **Default:** none. Every secret comes from the vault.
- **A wrong name** fails with
  `secret key "login" must start with a letter or underscore and contain only letters, digits, underscores, or hyphens; the secrets_from_env value must name an environment variable using letters, digits, or underscores and starting with a letter or underscore`.
- **Use:** passwords for a CI job or a coding agent, which has no terminal
  to type the vault passphrase into.
  [Passwords for automation](../how-to/connect-to-your-servers.md#passwords-for-automation)
  shows SSH and `sudo` passwords from environment variables.
- The [secrets reference](secrets.md#where-a-secrets-value-comes-from)
  owns the rules: when MMX reads the variable, the error for an empty
  variable, and how MMX keeps the variable away from SSH.

## max_nodes_at_once

`max_nodes_at_once` sets how many nodes work at the same time in each fleet
stage that sets no `max_nodes_at_once` of its own. A **fleet stage** is one
entry in a playbook's `fleet_stages`: which nodes to use, and the work that
each selected node does.

```yaml
max_nodes_at_once: 20
```

- **Type:** a positive integer, such as `20`. The project setting takes no
  percentage: `max_nodes_at_once: "50%"` fails with
  `expected int, got !!str`, and `0` fails with
  `max_nodes_at_once must be a positive integer`.
- **Default:** `100`. A fleet stage with more nodes starts the next node
  as soon as a node finishes, so a run works on every selected node.
- **A fleet stage's own `max_nodes_at_once`** replaces the project setting
  for that fleet stage, and can also be a percentage, such as `"50%"`.
- **`mmx run-command`** uses the project setting too.
- [Roll out a change gradually](../how-to/roll-out-a-change-gradually.md#change-several-servers-at-a-time)
  sets the number for one fleet stage.

## run_history_retention

`run_history_retention` sets how long the controller machine keeps each
finished run in the project's `.mmx/runs/` directory.

```yaml
run_history_retention: 7d
```

- **Type:** text. A duration above zero, written as
  [Go writes durations](https://pkg.go.dev/time#ParseDuration), such as
  `90m` or `24h`, or a whole number of days, such as `7d` or `30d`, or the
  word `forever`.
- **Default:** `24h`, counted from the end of the run's latest attempt.
- **A wrong value** fails with
  `run_history_retention must be a positive duration (24h, 7d) or forever`.
  A bare number, such as `0`, fails with `expected string, got !!int`.
- **`forever`** keeps every run until you delete the run's directory.
- [Where runs are kept, and for how long](../how-to/stop-resume-and-read-a-run.md#where-runs-are-kept-and-for-how-long)
  says which runs MMX keeps longer, which commands delete old runs, and
  how long each node keeps its own copy of a result.

## node_task_defaults

`node_task_defaults` sets the time limits and retry settings that each node
task uses when the node task sets none of its own. A **node task** is one
step that a node does, listed under a fleet stage's `node_tasks`.

```yaml
node_task_defaults:
  time_limit: 10m              # the whole node task, all tries together
  time_limit_per_try: 2m       # one try
  retry:
    max_tries: 5
    pause_between_tries: 10s
```

| Field | What it limits | Allowed values | Default |
| --- | --- | --- | --- |
| `time_limit` | The whole node task, including every try | a duration above zero, such as `10m`, or `null` | `null`: no limit |
| `time_limit_per_try` | One try of the node task | a duration above zero, or `null` | `null`: no limit |
| `retry.max_tries` | How many times a node task that retries runs at most, the first try included | a positive integer, or `unlimited` | `3` |
| `retry.pause_between_tries` | The wait after a failed try, before the next try | a duration of zero or more, such as `0s` or `10s` | `2s` |

- **Each field applies on its own.** A node task's own setting wins, then
  `node_task_defaults`, then MMX's default. `null` on a node task's time
  limit removes a limit that `node_task_defaults` sets.
- **The retry settings apply only to a node task that retries.**
  `max_tries` does not make a node task retry. Which modules retry by
  default, the rule for `max_tries: unlimited`, which modules ignore
  `node_task_defaults`, and what a result says when a time limit ends are
  in the playbook file reference:
  [`retry`](playbook-file.md#retry),
  [`time_limit` and `time_limit_per_try`](playbook-file.md#time_limit-and-time_limit_per_try),
  and [Modules that take no retry or time limit](playbook-file.md#modules-that-take-no-retry-or-time-limit).
- **`mmx run-command`** uses the time limits of `node_task_defaults`, and
  its command does not retry.
- **A wrong time limit** fails with
  `time limit must be a positive duration or null`, and names the fleet
  stage that would use the time limit and the line in
  `mmx_project_settings.yml`.

## allow_role_calls_within_roles

`allow_role_calls_within_roles: true` lets a role call another role. A
**role** is a reusable list of node tasks with declared inputs, in its own
file `roles/<name>/main.yml`.

```yaml
allow_role_calls_within_roles: true
```

- **Type:** boolean.
- **Default:** `false`. A role that calls a role then fails to load with
  `role calls within roles require allow_role_calls_within_roles: true`.
  Reason: the playbook then shows every role in order, so a reader sees
  which roles run without opening each role file.
- **A role that calls itself,** directly or through other roles, always
  fails with `recursive role call: NAME`.
- [Role calls within roles](../how-to/reuse-work-with-roles.md#role-calls-within-roles)
  shows the setting with an example.

## Saved output sizes

These limits keep a run small when a command prints a lot, or when a node
task tries many times. The limits are fixed, so no setting changes them:

| What | Limit | What MMX keeps beyond the limit |
| --- | --- | --- |
| The output of a command, an HTTP response body, or a module's result | 1 MiB for each output stream of each try | The first 1 MiB, followed by the line `[MMX output truncated at 1 MiB]` |
| All the node task records of one node job | 16 MiB | For each further node task: the status, the change, the exit code, the reason, and the summary of a file or directory. The record says what MMX left out, in the field `omitted`. |
| The reason of a node task, in a record beyond the 16 MiB | 4 KiB | The first 4 KiB of the reason |
| The tries of a node task that retries | The first try and the latest nine tries | The number of the other tries, in the result field `tries_omitted` |

A **node job** is one attempt to run a fleet stage on one node.

- **The 1 MiB limit applies to the result itself.** A later node task that
  reads the output, such as through `.node_tasks.ID`, gets the cut text.
  A condition, such as `when` or `until`, refuses a cut value instead of
  reading the cut text.
- **The other limits apply only to what MMX saves and shows.** A condition
  of a node task that retries reads the result of each try in full.

## The .mmx directory

MMX keeps what it saves on the controller machine in the directory `.mmx/`
inside the project directory. `mmx init` adds the line `/.mmx/` to the
project's `.gitignore`, so Git leaves `.mmx/` out. The first
command that saves or reads runs, such as `mmx dryrun`, creates `.mmx/`. Only your
account can read the directories that MMX creates in `.mmx/`.

```text
shop/                                   the project directory
  mmx_project_settings.yml
  .mmx/
    runs/
      20261115T035229.429039873-fbe759f2091942e3/   one saved run, named by the run's full ID
    nodes/                              what MMX learned about each node
    runners/                            copies of mmx-runner that saved runs need
    history.lock                        a lock file for saved runs
```

| Path | What the path holds | How long MMX keeps the path | Delete it? |
| --- | --- | --- | --- |
| `.mmx/runs/<run-id>/` | One playbook run, dryrun, or `mmx run-command`: the project's files as the run read them, the selected nodes, each node job's progress and result, and the text differences of changed files. The directory `assets/` inside holds copies of the files and templates that the run sends to nodes, so `mmx resume` sends the same files. | [`run_history_retention`](#run_history_retention) | Yes, when no `mmx` command runs in the project. `mmx results`, `mmx status`, and `mmx resume` then find no such run. Before you delete a run whose work MMX has not collected from a node, run `mmx status` on the run. |
| `.mmx/nodes/` | One small file for each node address, SSH port, and account, named by a hash of the three. The file holds what MMX learned about the node at the first contact over SSH, such as the fingerprint of the key of MMX's HTTPS service on the node. | Until `mmx remove-mmx-from-node` removes the node's file | Yes. The next run on each node sets up MMX again through SSH, and needs the SSH login, and the `sudo` password if the account's `sudo` asks for one. |
| `.mmx/runners/` | Copies of `mmx-runner`, MMX's program for the nodes, named by SHA-256 hash. `mmx resume` sends the same build of `mmx-runner` that the run started with. | Until no saved run needs the copy | Yes, with the runs. A resume of a run whose copy is gone fails. |
| `.mmx/history.lock` | An empty lock file. Commands hold the lock while they save or read runs, and MMX deletes old runs only while no other command holds the lock. | Kept | No need. Never while an `mmx` command runs in the project. |

**Safe to delete:** the whole `.mmx/` directory, when no `mmx` command
runs in the project. You lose the saved runs, so `mmx results`,
`mmx status`, and `mmx resume` find none, and what MMX learned about each
node, so the next run sets up each node again through SSH.

**What `.mmx/` does not hold:** MMX's own SSH keys, in `~/.ssh/mmx/` on
the controller machine, which MMX makes for
[a server that takes only a password](../how-to/connect-to-your-servers.md#a-server-that-takes-only-a-password),
and the trusted SSH host keys, in `~/.ssh/known_hosts`. MMX also keeps
records on each node, in `/var/lib/mmx/`.

**Secrets:** MMX saves no secret value from the vault or from
`secrets_from_env` in `.mmx/`. A file of the project that holds a password
as ordinary text, such as a template, is copied into `assets/` as the file
is. The
[secrets reference](secrets.md#secrets-in-files-output-and-history) has the
rules.

**Sharing and moving:** keep `.mmx/` out of Git. Each controller machine
keeps its own `.mmx/`, and a teammate's first run sets up the teammate's
`.mmx/`. To move a project to a new controller machine, copy `.mmx/` with
the project, so that saved runs and node records come along.
[Move or share a project](../how-to/move-or-share-a-project.md#what-mmx-keeps-on-a-controller-machine)
lists what each controller machine keeps, and what a controller machine
without each part sees.
