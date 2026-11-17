# MMX development

**Read the [documentation map](docs/changing-mmx/documentation-map.md) before you change
behavior, a name that users read, or a document.** The documentation map says
which document owns each kind of fact, how module pages are built, and what
else to update when something changes. Keep each fact in the document that
owns it.

The acceptance contracts live in [docs/changing-mmx/acceptance.md](docs/changing-mmx/acceptance.md), not
in the README. Use the acceptance contracts, including the typical deployment
and the criteria for a correctness blocker, for completion and review.

The [design plan](docs/changing-mmx/design/README.md) owns
the [product principles](docs/changing-mmx/design/README.md#product-principles),
priorities, the record of what is built, and the remaining work; read it
before changing code, tests, or requirements. Test the intended behavior and
failure cases; keep documentation consistent with what the implementation and
tests actually prove. Follow the [implementation rules](docs/changing-mmx/implementation-rules.md)
and check every change against them.

## Names that users read

Every command, flag, file name, configuration key, YAML field, result field, environment
variable, error, and document must name its subject explicitly. A developer
reading one line without context must know which thing each name means. Use
`ssh_port` when another kind of port exists, and distinguish requested values
from observations, such as `expected_http_status_code` and `http_status_code`.
Avoid abbreviations that need decoding; standard tool names, `ssh`, `id`,
`vars`, `path`, and `url` can stay when clear. One word has one meaning.
File names say what they hold; settings and playbooks stay in separate files.

Use `env` for environment variables and `config` for configuration in names.
Use `dir` in a label without a verb whose value is a directory path, such as
`state_dir`. Use `directory` with a verb (`create_home_directory`) or in
the plural (`created_parent_directories`). Also use `directory` as the whole
name or at the start of a name (`directory_change`). In prose, write
environment variables, configuration, and directory in full. The
[naming design](docs/changing-mmx/design/modules.md#env-config-and-dir-have-one-spelling)
gives the complete spelling rule. Check every new public name before it
reaches users.

Follow the [documentation writing rules](docs/changing-mmx/documentation-map.md#writing-rules)
for the reader, terms, examples, and current-build text.

## Public terminology and examples

The terms in the table below are MMX's glossary. Use the glossary terms
exactly in documentation. Define each glossary term in plain words at first
use in each document. In prose, always give the level (controller machine,
fleet, or node) so the reader never has to guess where something happens.
In YAML the nesting already shows the level, so
configuration keeps its short keys: `fleet_stages`, `nodes`, `node_tasks`, and
`task_block`. The
[configuration specification](docs/changing-mmx/design/configuration.md#node-tasks-and-task-blocks)
owns the exact structure and formatting.

| Term | Meaning |
|---|---|
| controller machine | The machine where you run `mmx` commands, such as a laptop or a CI machine. |
| controller process | One running `mmx` command on the controller machine. |
| controller fleet scheduler | Code inside the controller process that runs fleet stages in order and limits how many nodes work at once. |
| node | A Linux server that MMX manages. |
| project settings | `mmx_project_settings.yml`, the settings file that marks the project root; it contains no fleet stages. |
| playbook | A file at `playbooks/<name>.yml` that lists fleet stages in order and may declare command-line variables. |
| inventory | A file at `inventories/<name>.yml` that lists the nodes, how to reach them, and their groups. |
| fleet stage | One entry in `fleet_stages`: which nodes to use, and the node tasks each selected node runs. |
| node task | One entry in `node_tasks`: an action one node performs, a role call, or a node task block. |
| role | A reusable list of node tasks with declared inputs, stored in its own file. |
| node task block | A `task_block`: related node tasks grouped into phases such as `changes`, `validate`, `activate`, and `verify`. |
| fleet stage output | A named value that a fleet stage publishes for each of its nodes in `outputs`, which later fleet stages read as `{{ .outputs.<fleet stage>.<node>.<name> }}`. |
| playbook run | One recorded execution of a playbook, including attempts started by `mmx resume`. |
| node job | One attempt to execute one fleet stage on one node. |
| node runner executable | `mmx-runner`, the MMX program installed on each node. |
| node HTTPS API process | The long-running `mmx-runner serve` process (systemd unit `mmx-https-api.service`) that accepts the controller process's HTTPS connections on port 7443. |
| node SSH request process | An `mmx-runner` process started for one request over SSH; it exits when that request is done. |
| node job worker process | The `mmx-runner` process on the node that carries out one node job. The node job worker process keeps going if the connection drops. |
| reachability confirmation | A request the controller process sends over a new SSH or HTTPS connection for one change of one node job. The node records which kind of connection reached the node before or after the change. |
| owed restore | A durable record on the node that one module-owned file must return to its previous state unless the controller process confirms reachability in time. A systemd timer carries out the restore without the controller machine or node job worker process. |
| node change activation process | The `mmx-runner activate-change` process in `mmx-activate-change-<owed-restore-id>.service` activates and checks one proposed configuration change. The restore service can stop all processes in the activation service. |
| node owed restore process | The `mmx-runner run-owed-restore` process in `mmx-owed-restore-<owed-restore-id>.service` restores a module's recorded previous configuration. The node owed restore process runs without the node job worker process. |
| owed restore id | The 16 lowercase hexadecimal characters that identify one owed restore: its record and its units on the node. |
| node task execution engine | Code inside the node job worker process that runs node tasks in order and applies their conditions, retries, time limits, and phases. |
| module | The code that does the work of one node task, such as `file`, `apt_package`, or `greeting`. A node task names exactly one module and writes the module's inputs below the name. A module has a kind: core, official extra, or the project's own. |
| core module | A module compiled into MMX's node runner executable, such as `file`, `apt_package`, or `systemd_service`. |
| official extra module | A script in Starlark, a small language with Python-like syntax. The MMX project writes, tests, and ships official extra modules in `official_extra_modules/` beside `mmx`, in the same release. An official extra module is never copied into a project. |
| module of the project's own | A module the project writes in its `external_modules/<name>/` directory: a Starlark script (`module.star`) or, for special needs, any executable (`module_executable`). |
| primitive | One of the functions through which every module reads or changes a node, such as `read_file`, `write_file`, or `run_command`. A Starlark script calls the same primitives as the core modules. |
| module description | A module's inputs, outputs, and examples. A core module's is built in; every other module has the file `module_description.yml`. |
| tracked input | An input declared by a node task block's `changes` section, such as a file, a package version, or a Git checkout. MMX remembers the tracked input by its hash so MMX knows that a restart or command is still owed. |
| command record | What MMX keeps on a node for a `shell` task with `only_when_tracked_inputs_changed: true`: the tracked inputs the command last ran with. |
| helper node | A node that a `for_each_node` iteration works on for the loop's node, such as the load balancer. A `--limit` still reaches the helper node. |
| one-time playbook | A dated playbook in `one_time_playbooks/` for work that happens once on every node the playbook selects. |
| secret | A value that MMX hides in output and saved runs, such as a password: from the vault, from `secrets_from_env`, or from an output read with `content_is_secret: true`. |
| vault | The encrypted file of secrets in the project, `secrets_vault.age` unless `vault_file` names another file. |
| vault passphrase | The passphrase that encrypts and opens the vault; MMX asks for it at a terminal or reads it from `MMX_VAULT_PASSWORD_FILE`, `MMX_VAULT_PASSWORD`, or `--vault-password-file`. |
| secret key | The name of one secret in the vault or in `secrets_from_env`. |

MMX supports exactly two connection types between the controller machine and
a node: SSH and HTTPS. Name SSH or HTTPS when the difference matters; otherwise
write "the connection". Preserve standard terms such as SSH host key,
hostname, and `known_hosts`. The [How MMX works page](docs/how-it-works.md)
introduces MMX's glossary terms for users.

The node side of reachability confirmation and owed restore, and the controller
process's confirmation loop, exist. A guest suite proves the whole mechanism
with a module that exists only in test builds.

The production module
`systemd_networkd_file` uses reachability confirmation and owed restore.
The dedicated `systemd_networkd_file` guest suite covers native activation
and recovery.
The production modules `netplan_file` and
`ifupdown_interfaces_file` also use the mechanism; each has its own guest
suite, and the [validation record](docs/changing-mmx/validation.md) holds the current
guest results.
The [execution design](docs/changing-mmx/design/execution.md#reachability-confirmation-and-owed-restore)
owns the behavior and administrator commands for reachability confirmation
and owed restore.

## Compatibility between releases

**MMX 0.1.0 is released. Every change keeps the promises in
[Compatibility between releases](docs/changing-mmx/design/README.md#compatibility-between-releases).**

- A patch release, such as 0.1.1, makes no incompatible change; fixes,
  documentation changes, and additions go in one. An
  incompatible change to a project file, a command, a flag, or a result field
  makes the next release a minor release, such as 0.2.0, whose release notes
  give the edit that a project needs.
- A new node runner executable reads the command records and service records
  that the node runner executables of all earlier releases wrote.
  If command or service record formats change, keep a reader of the old
  format, with a test that reads records an earlier release's node runner
  executable wrote.
- The saved state on the controller machine, `.mmx/`, needs no migration: an
  upgrade may discard it.

Within the compatibility promises above:

- Implement the current agreed design directly. Delete superseded code and
  replace or remove tests that assert discarded behavior.
- Do not add backward-compatibility layers, legacy-state readers, migrations,
  deprecation paths, or historical-format fixtures beyond the promises above.
  Existing code and tests do not create a compatibility requirement. Add a
  compatibility requirement only for a concrete need explicitly agreed with
  the user.
- Test correctness and behavior we still intend to support: real resource
  changes, failure recovery, concurrency, validation, and the user workflows in
  the design. A regression test is useful when the test protects real resource
  changes, failure recovery, concurrency, validation, or a user workflow in
  the design. A regression test is not a reason to preserve a design we have
  replaced.
- Update implementation, tests, and owning documentation together. Remove
  superseded requirements when the design changes; do not preserve them through
  exceptions or legacy sections. Reset disposable test state when its format
  changes instead of writing migration machinery for it.

Full-disk, interrupted-write, and broader competing-controller fault testing
remain later hardening work, not gates for a release.

Documents describe the current build only. Delete completed plans, fixed
findings, superseded results, and "we found X, so now Y" narratives; git history
keeps them. Keep temporary investigation material outside the repository.

Before VM tests or test-environment maintenance, follow
[testing guide](docs/changing-mmx/testing.md). The testing guide owns environment paths,
locking, ports, and isolation from other projects' test environments.

Before adding a core module or package manager, follow the
[guide to adding a core module](docs/changing-mmx/adding-core-modules.md). The guide to
adding a core module owns the code entry points, shared execution rules, and
required verification. The guide to adding a core module also owns the division between
shared package handling and package-manager-specific code.

Before adding or changing an official extra module, follow the
[guide to adding an official extra module](docs/changing-mmx/adding-official-extra-modules.md).
The guide to adding an official extra module owns the files a module
consists of, the rules every script keeps, the sections of a module's page,
and the required tests.
