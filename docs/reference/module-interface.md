# Module interface reference

This page lists what a module of the project's own may use and must do, for
looking up while you write one: the files in the module's directory, the
fields of `module_description.yml`, what a Starlark script returns, every
primitive and what it returns, the protocol of an executable module, the
dryrun rules, secrets, and limits. A **module** is the code that does the
work of one node task. A **module of the project's own** is a directory in
the project's `external_modules/`, named after the module, that holds a
[Starlark](https://github.com/google/starlark-go/blob/master/doc/spec.md)
script or an executable. A **primitive** is a function through which a
script reads or changes a node, the server that runs the module. To write a
module step by step, see [Write your own module](../how-to/write-your-own-module.md).
The core modules and the official extra modules have their own pages:
[module pages](modules/README.md).

## The module's name

| Rule | Detail |
| --- | --- |
| Characters | Lower-case letters, digits, and underscores. The first character is a letter. At most 64 characters, such as `env_file_setting`. |
| Taken names | The name of a core module, of an official extra module, or of a node task setting, such as `when`, `loop`, `retry`, `role`, or `task_block`. `mmx validate` refuses such a directory in `external_modules/`, also when no node task calls the module. |
| Lookup | MMX looks a node task's module up among the core modules, then the official extra modules, then `external_modules/`. A call inside a role is looked up the same way. |
| In a playbook | The module's name, with the inputs below it, as for a core module. The kind of the module is not written. |

## The module's directory

| File | Required | What it holds |
| --- | --- | --- |
| `module_description.yml` | Yes | The module's summary, inputs, outputs, and examples: [The module description](#the-module-description). |
| `module.star` | One of the code files | A Starlark script: [A Starlark script](#a-starlark-script). |
| `module_executable` | One of the code files | Any program that runs on the node, such as a Bash script: [The executable protocol](#the-executable-protocol). |
| `module_executable_linux_amd64` and `module_executable_linux_arm64` | One of the code files | A compiled program, as `module_executable_linux_amd64`, `module_executable_linux_arm64`, or both. MMX sends each node the build for its processor. A node whose processor has no build fails the node task. |
| `README.md` | No | The module's page. `mmx describe-module NAME` prints the page after the reference that MMX builds from the description. |
| Other files and directories | No | Supporting files, which an executable reads from its working directory. |

- A directory holds `module.star` or an executable, not both, and
  `module_executable` or the builds, not both.
- An executable file must have the execute permission for its owner, such
  as from `chmod +x`.
- Each file must be a regular file without the setuid, setgid, or sticky
  permission bits. The module's directory and `external_modules/` must be
  directories, not symbolic links.
- `module_description.yml` holds at most 256 KiB.
- A run sends the node the files of the modules that the run uses. The
  node keeps each file by its SHA-256 in `/var/lib/mmx/kept_files/`, so an
  unchanged file is not sent again.
- MMX saves a run's module files with the run, so `mmx resume` uses the
  same files, also after you change the module.

## The module description

```yaml
# external_modules/env_file_setting/module_description.yml
summary: Makes sure an environment file sets one setting to one value, and keeps the file's other lines.
inputs:
  path:
    type: string
    required: true
    description: The absolute path of the environment file, such as /etc/shop/shop.env.
outputs:
  previous_value:
    type: string
    description: The value the file had before the node task.
examples:
  - description: Set the shop's port to 8080 in /etc/shop/shop.env.
    config: |
      env_file_setting:
        path: /etc/shop/shop.env
```

| Field | Required | Meaning |
| --- | --- | --- |
| `summary` | Yes | One sentence that says what the module does. `mmx describe-module` shows the sentence in its list. |
| `inputs` | No | The module's inputs by name. Each input has the fields below. |
| `outputs` | No | The module's named outputs by name. Each output has a `type` and a `description`, both required. A later node task reads an output as `{{ .node_tasks.<id>.<name> }}`. |
| `examples` | Yes, at least one | Each example has a `description` and a `config`: the YAML of one call, the module's name with its inputs. |

| Input field | Required | Meaning |
| --- | --- | --- |
| `type` | Yes | One of the types below. |
| `required` | Yes | `true` when every call must give the input, `false` when a call may leave the input out. |
| `description` | Yes | What the input holds, with an example value. |
| `allowed_values` | No | A list of the values the input may have. Only for the types `string` and `integer`. |
| `private_value` | No | `true` for a password or another value that the module hands only to a program: [Secrets in a module](#secrets-in-a-module). Only for the type `string`. |

An input has no default value. A script reads an input that a call may
leave out with `inputs.get("name", default)`, and an executable checks
whether the request has the input.

| Type | A value of the type |
| --- | --- |
| `string` | Text, such as `"8080"`. A YAML number such as `8080` is no string. |
| `integer` | A whole number. |
| `number` | Any number. |
| `boolean` | `true` or `false`. |
| `list_of_strings` | A list of texts. |
| `mapping_of_strings` | A mapping whose values are texts. |
| `any` | Any value, such as a list of mappings. The module checks the value's form itself. |

- The names of inputs and outputs follow the same rule as a module's name.
  No output may be named `status`, `change`, `reason`, `items`, or
  `outputs`, because every result already has those names.
- An unknown field, or a key written twice, is an error that names the
  line of `module_description.yml`.
- MMX checks each example against the description when MMX reads the
  description.
- `mmx validate` checks each call against the description: an unknown
  input, a missing required input, a value of the wrong type, and a value
  that `allowed_values` does not list. A value that comes from a reference,
  such as `{{ .vars.shop_port }}`, is checked on the node, after MMX fills
  in the reference and before the module runs.
- A result must keep to the description. An output that the description
  does not list, or an output of the wrong type, fails the node task.

## A Starlark script

The script defines `run_node_task(inputs)`. The node job worker process,
MMX's program that carries out a fleet stage on one node, calls the
function once for each try of the node task, and once for each item of a
loop. `inputs` is a dictionary of the call's inputs, with references and
secrets already filled in. The script runs as root, in a dryrun as in a
run.

`run_node_task` returns `None` for a success without outputs, or a
dictionary with these keys:

| Key | Meaning |
| --- | --- |
| `status` | One of the statuses below. Without `status`, the result is `succeeded`. |
| `reason` | Text that explains the result. Required for every status except `succeeded`. |
| `outputs` | A dictionary of the named outputs that the description lists. |
| `fingerprint` | Text that MMX adds to the fingerprint it computes, for state that no changing primitive keeps, such as an instance that a command created. |
| `try_again` | `False` beside `status: failed`: MMX does not try the node task again. |

Another key fails the node task.

| `status` | Use it when | What MMX does |
| --- | --- | --- |
| `succeeded` | The node is as the inputs ask. | The node task succeeds. |
| `failed` | Something failed that another try may fix, such as a command that failed. | MMX tries the node task again, as the node task's `retry` setting allows. |
| `failed` with `try_again: False` | The script changed the node and then failed, and another try would find the changed node and report success. | The node task fails after this try and keeps its outputs and its change. Write the script so that the next run of the playbook finishes the work. |
| `missing_prerequisite` | Something that an earlier node task may still provide is missing, such as the tool's program, or the node is in a state that an earlier node task may still change. | A dryrun reports the node task `not_checked`, and names the earlier node task that would change the node first, when there is one. A run fails the node task without another try. |
| `invalid_inputs` | The node task cannot succeed as written: its inputs break a rule that the description cannot state, such as two inputs that exclude each other, or cannot work on this node, such as a login that the tool's server refuses. | The node task fails without another try, in a dryrun as in a run. |
| `not_checked` | Only in a dryrun: the script cannot tell what a run would do, such as while the tool's server does not answer yet. | The dryrun reports `not_checked, unknown`. In a run, `not_checked` fails the node task. |

**MMX decides the change.** The script never reports its change:

| What the changing primitives did | The node task's change |
| --- | --- |
| A changing primitive changed the node, or in a dryrun would have | `changed`, or `would_change` in a dryrun |
| No changing primitive changed the node | `unchanged` |
| A changing command or request started and then ended with an error, such as at a time limit, and no other call changed the node | `unknown` |

- A normal exit of `run_command_that_changes_the_node` counts as a
  change, also with a nonzero exit code, because MMX cannot see whether
  the program changed something before it refused.
- **MMX builds the difference** that a dryrun and `--diff` show: each
  file's content difference, a directory, a link, a removal, or the
  command or request that would run. A file that other accounts cannot
  read shows no content difference.
- **MMX computes the fingerprint** from every file, directory, and link
  that the changing primitives keep, changed or not, and from the
  script's own `fingerprint` text.
- `print(...)` adds a line to the node task's result. MMX keeps the
  first 8 KiB. The output shows the lines below a failed node task, and
  `--json` gives them in the node task's `stderr`.
- An error in the script fails the node task with one line that names the
  script, the line, and the column, such as
  `the script of the module env_file_setting ended with an error: module/env_file_setting/module.star:7:21: key "settings" not in dict`.
  The node task then tries again as for `failed`. A script that does not
  parse is the exception: its node task fails at once, with the failure
  kind `config`, because every try would end the same way.

### The Starlark language

- The script has the primitives below, `json.encode(value)`,
  `json.decode(text)`, `json.indent(text)`, `print(...)`, and Starlark's
  built-in functions, such as `len` and `sorted`.
- The script cannot load another file of code, and cannot reach a file, a
  program, or the network except through a primitive.
- Starlark has no `while` and no recursion. An `if` or `for` statement
  must be inside a function.
- Values that the script returns are `None`, booleans, texts, numbers,
  lists, tuples, and dictionaries with text keys. A whole number must fit
  in 64 bits.

## Primitives

Each primitive returns a result whose fields the script reads as
`result.name`, such as `found.exists`. Every result has an `error` text,
which is empty when the call worked. A failed call does not stop the
script, so the script decides what the failure means. `is_dryrun`,
`sha256`, and `earlier_node_task_would_change` return a plain value
instead.

Primitives that read the node, which also run in a dryrun:

| Primitive | What it does | What it returns |
| --- | --- | --- |
| `read_file(path)` | Reads a UTF-8 text file of at most 1 MiB. | `content` |
| `inspect_path(path)` | Looks at a path without following a final link. A missing path is no error. | `exists`, `type` (`file`, `directory`, `link`, or `other`), `owner`, `group`, `permissions_mode`, `link_target`, and for a file `sha256` |
| `look_up_account(user_account)` | Reads a local account and its groups. | `exists`, `user_id`, `group_id`, `home`, `shell`, `groups` |
| `find_program(program_name)` | Looks a program up as the command primitives find programs. | `found`, `path` |
| `run_command(arguments, standard_input = "")` | Runs a program that only reads, such as `["incus", "list", "--format", "json"]`. | `ran`, `exit_code`, `standard_output`, `standard_error` |
| `http_request(method, url, headers = {}, body = "", certificate_authority_file = "")` | Sends one HTTP or HTTPS request. `GET` and `HEAD` read. Any other method is a change: see the next table. | `sent`, `http_status_code`, `headers` (names in lower case), `body` (at most 1 MiB) |
| `parse_duration(duration)` | Parses a duration such as `30s`, `2m`, or `1m30s`. | `seconds` (a number). A text that is no duration gives zero seconds and an `error`. |
| `sha256(text)` | Hashes text. | The SHA-256 in hexadecimal |
| `is_dryrun()` | Says whether the node task runs in a dryrun. | `True` or `False` |
| `earlier_node_task_would_change(path)` | In a dryrun, says whether an earlier node task of the dryrun would create or change the path, a directory above it, or a path inside it. The answer covers the paths of core modules, such as a `file` node task's path, not what a script writes. | `True` or `False`; always `False` in a run |

Primitives that change the node, which change nothing in a dryrun and say
what the call would change:

| Primitive | What it does | What it returns |
| --- | --- | --- |
| `write_file(path, content, owner = "root", group = "root", permissions_mode = "0644")` | Makes the file hold `content` with that owner, group, and permissions. Writes a new file beside the old one and renames it into place, so a reader never sees half a file, and gives the file the SELinux label of its path. The directory must exist. | `changed` |
| `make_directory(path, owner = "root", group = "root", permissions_mode = "0755")` | Makes the directory exist with that owner, group, and permissions. The parent must exist. | `changed` |
| `make_link(path, target)` | Makes `path` a symbolic link to `target`. | `changed` |
| `remove_path(path)` | Removes a file, a link, or an empty directory. A missing path is no change. | `changed` |
| `run_command_that_changes_the_node(arguments, standard_input = "")` | Runs a program that changes the node. In a dryrun, the program does not run. | `ran` (`False` in a dryrun), `exit_code`, `standard_output`, `standard_error` |
| `run_command_that_refreshes_a_package_index(arguments)` | Runs a package manager's index refresh, such as `["pacman", "-Sy"]`. The index is a cache, so the refresh is no change of the node task. In a dryrun, the program does not run. | `ran` (`False` in a dryrun), `exit_code`, `standard_output`, `standard_error` |
| `http_request` with a method other than `GET` and `HEAD` | Sends the request. In a dryrun, the request is not sent. | As above, with `sent` `False` in a dryrun |

- **Paths** are absolute and have no `..` part. A relative path, or a
  path such as `/srv/app/../etc`, is an error, so a path built from an
  input stays below the directory the module names.
- **Commands** run as root, in `/`, with no shell: `arguments` is a list.
  The environment is the one in
  [The environment of a command](shared-module-behavior.md#the-environment-of-a-command).
- **A nonzero exit code is no error.** `error` is set when the program
  could not start, ended by a signal, or its output could not be read. The
  script checks `exit_code` itself.
- **When the program ends, MMX ends every program it left in its process
  group,** so a command cannot leave a program running in the background.
  Start a program that must keep running through systemd.
- **`run_command` runs also in a dryrun,** because MMX cannot see what a
  program does. Use `run_command` only for programs that read, and
  `run_command_that_changes_the_node` for every other program.
- **Ask `find_program` before a tool's first command,** and return
  `missing_prerequisite` when the tool is missing, so that a dryrun of a
  playbook that installs the tool first reports `not_checked`, not a
  failure. When a script
  returns `failed` with the `error` text of a command whose program is not
  on the node, MMX turns the status into `missing_prerequisite`.
- **`http_request`** follows no redirect and verifies every HTTPS
  certificate, with the authorities in `certificate_authority_file` when
  that file is given. Its `error` names only the host, never the whole URL,
  because a URL can hold a secret.
- **In a dryrun, a changing primitive sees the node as it is,** not as the
  script's earlier calls would leave it. A `write_file` into a directory
  that the script's own `make_directory` would create, or a `remove_path`
  of a directory that the script would empty first, fails in a dryrun.
  Guard such a call with `is_dryrun()` and report what would happen.
- **A file that another node task writes:** in a dryrun, the file on the
  node is still the old one. Ask `earlier_node_task_would_change(path)`,
  and when the answer is `True`, report what follows from a changed file
  instead of comparing the old one.

## The executable protocol

MMX starts an executable once for each try of its node task, as root, as a
child of the node job worker process. The working directory is a private
copy of the module's directory on the node. When the executable ends, or
the node task's time limit ends, MMX ends the processes left in the
executable's process group. A program that must keep running is started
through systemd.

The environment is the one in
[The environment of a command](shared-module-behavior.md#the-environment-of-a-command),
and also `MMX_MODULE_HELPER`, the path of `mmx-runner` on the node.

### The request

MMX writes one JSON object to the executable's standard input:

```json
{"protocol":1,"mode":"check","inputs":{"path":"/etc/shop/shop.env","setting":"SHOP_PORT","value":"8081"}}
```

| Field | Meaning |
| --- | --- |
| `protocol` | The number `1`. |
| `mode` | `check` in a dryrun, `apply` in a run. |
| `inputs` | The call's inputs, with references and secrets filled in. An input that the call leaves out is not in the object. |

| Mode | What the executable must do |
| --- | --- |
| `check` | Inspect without changing what the module manages. Report `unchanged`, `would_change`, or `not_checked` with a reason. |
| `apply` | Inspect, make the changes that are needed, and report what happened. Work without an earlier `check`: a previous try may have changed something and then stopped before its result. |

### The result

The executable writes exactly one JSON object to its standard output, and
diagnostics to its standard error:

```json
{"protocol":1,"status":"succeeded","change":"would_change","outputs":{"previous_value":"8080"},"diff":"--- before\n+++ after\n@@ -1 +1 @@\n-SHOP_PORT=8080\n+SHOP_PORT=8081","fingerprint":"c5b0503eeda95d30e4b5e4627d0466ff7ffb3163e8bae2bf5567d9c5e46126cc"}
```

| Field | Required | Meaning |
| --- | --- | --- |
| `protocol` | Yes | The number `1`. |
| `status` | Yes | `succeeded`, `failed`, or, in `check` mode only, `not_checked`. |
| `change` | Yes | `unchanged`, `changed`, `would_change`, or `unknown`. |
| `reason` | For `failed` and `not_checked` | Text that explains the result. |
| `outputs` | No | An object of the named outputs, with the names and types that the description lists. |
| `diff` | No | Text that shows what changed or would change. A dryrun and `--diff` show the text. |
| `fingerprint` | In a node task block's `changes` | Text that changes when the managed state changes, such as a hash of the managed state. MMX keeps the text's SHA-256, not the text. |
| `secret` | No | `true` makes the whole result private. `false` cannot clear a privacy that MMX gave the result. |

| Mode and outcome | `status` | `change` |
| --- | --- | --- |
| `check` finds the requested state | `succeeded` | `unchanged` |
| `check` finds work to do | `succeeded` | `would_change` |
| `check` cannot inspect, because something is missing | `not_checked` | `unknown` |
| `apply` finds the requested state | `succeeded` | `unchanged` |
| `apply` makes a change | `succeeded` | `changed` |
| Either mode fails | `failed` | What happened before the failure: `changed` after a change in `apply` mode, otherwise `unchanged`, or `unknown` when the executable cannot tell |

- `check` never reports `changed`, `apply` never reports `would_change`,
  and `succeeded` never goes with `unknown`.
- Exit code 0 means that the executable wrote a result. A nonzero exit
  code is never a success: a result that says `succeeded` becomes
  `failed` with the reason `module exited with status 1 after reporting
  succeeded`. A `failed` result with a change still counts after a
  nonzero exit, so the change is not lost.
- The node task fails when the output is missing or is not one valid JSON
  object, or has an unknown field, a key written twice, another protocol
  number, or a status and change that do not fit the mode.
- A result holds only the fields in the table above. A field for the node
  task's identity or failure kind, for example, fails the node task: MMX
  sets those itself.
- An executable that could not start fails the node task with the change
  `unchanged`.
- Report `changed` exactly when the executable changed the node. In a
  node task block, an executable that changes the node and reports
  `unchanged` leaves a service running with what it loaded before.

### The module-json helper

`$MMX_MODULE_HELPER module-json` reads requests and writes results, so a
Bash executable needs no `jq` and builds no JSON by hand. `mmx module-json`
on the controller machine is the same helper, for trying an executable by
hand.

```text
module-json request [--inputs NAME,...]
module-json get [--type string|integer|number|boolean|array|object|null|json] POINTER
module-json result --mode check|apply --change CHANGE [--status STATUS] [--secret]
  [--reason TEXT] [--output NAME TEXT]... [--output-json NAME JSON]...
  [--fingerprint TEXT] [--reason-stdin | --diff-stdin | --output-stdin NAME]
```

| Command | What it does |
| --- | --- |
| `request` | Reads the request from standard input, checks it, and prints the request unchanged. `--inputs path,setting` refuses an input whose name is not in the list. |
| `get` | Reads a request from standard input and prints the value at a JSON Pointer, such as `/mode` or `/inputs/path`. In a pointer, `~1` stands for `/` and `~0` for `~`. |
| `result` | Checks a result and prints the result as JSON. `--status` is `succeeded` unless given. Quotes, backslashes, and line breaks in values need no escaping. |

- `get --type string` prints the exact text, with no added newline. The
  shell's `$(...)` drops trailing newlines, so send exact text straight to
  a file, or count its lines, instead of keeping the text in a variable.
- `get --type integer` accepts a whole number in the signed 64-bit range,
  also written as `2.0` or `2e3`, and prints plain decimal.
  `--type number` keeps the number as the request wrote it. Other types
  print JSON.
- A missing value is an error. `null`, `false`, `0`, and `""` stay
  distinct: `--type null` and `--type json` accept `null`.
- `--output NAME TEXT` adds a text output, and `--output-json NAME JSON` a
  structured output.
- One `result` call reads at most one value from standard input:
  `--reason-stdin`, `--diff-stdin`, or `--output-stdin NAME`. Use standard
  input for long text, a difference, and a secret, because command
  arguments show in the node's process list.
- The helper exits with code 4 for wrong arguments and 1 for invalid data.

## The node task that calls the module

- **Retries.** A node task that calls a module of the project's own tries
  again after `failed` and after an error in a script (not after a script
  that does not parse, and not after a failure that names a missing
  program, which is a `missing_prerequisite`), with the project's
  `node_task_defaults.retry`: 3 tries, 2 seconds apart, unless the project
  or the node task sets other values. `retry: false` turns the tries off.
  `mmx describe-module task` lists the setting.
- **Time limits.** The node task's `time_limit` and `time_limit_per_try`
  apply. At the limit, MMX ends the script or the executable, and the
  programs that the script or the executable started.
- **Calls** work in loops and in every node task block phase except
  `validate`, with the node task's conditions, saved results, stopping,
  and resuming.
- **Tracked input.** In a node task block's `changes`, the module's node
  task is a tracked input by its fingerprint. A `systemd_service` node task
  in the block's `activate` refreshes its service, and a `shell` node task
  with `only_when_tracked_inputs_changed: true` runs its command, when the
  fingerprint differs from the one the service or command last acted on.
- **No earlier fingerprint.** On the first run, and for a service also
  after a reboot or after the service started by other means, MMX has no
  fingerprint to compare with. Then the module's node task decides. When
  the node task changes nothing, nothing is refreshed, and its fingerprint
  becomes the one to compare with. When the node task changes something,
  the service is refreshed.
- **Another kind, another fingerprint.** A script and an executable give
  different fingerprints for the same state. The first run after you
  switch a module between the two kinds refreshes the service once.
- **The kind in results.** `--json` results name the kind of the module
  as `module_kind`: `project_starlark` or `project_executable`.

## Dryrun rules

| | Starlark script | Executable |
| --- | --- | --- |
| Who keeps the dryrun from changing the node | MMX: changing primitives change nothing in a dryrun. | The executable: `check` mode must change nothing. |
| Who decides the change | MMX, from the changing primitives. | The executable. |
| Who builds the difference | MMX. | The executable, in `diff`. |
| Who computes the fingerprint | MMX, from what the changing primitives keep, and the script's `fingerprint` text. | The executable, in `fingerprint`. |
| What runs in a dryrun anyway | The reading primitives, including `run_command`. | The whole executable, in `check` mode. |

`mmx module-usage` and `mmx describe-module NAME` say, for each executable
module, that MMX cannot see what the executable changes, so the module
must keep its own dryrun promise.

## Secrets in a module

A secret is a value that MMX hides in output and saved records, such as a
password from the vault. How a secret travels and where MMX hides it is in
the [secrets reference](secrets.md#secrets-in-files-output-and-history).
For a module of the project's own:

- **A secret in an ordinary input hides the whole result** of the node
  task: its reason, its difference, and its outputs. A failure then says
  only `details suppressed for a task using secrets`. The reasons of
  `invalid_inputs` and `missing_prerequisite` are hidden too, because the
  script wrote them and they can hold the input's value.
- **An input marked `private_value: true` hides less.** A secret in such
  an input leaves the result visible, with `[redacted]` wherever the
  result's text holds the secret's value. Mark an input this way when the
  module hands the value only to a program, such as a password:

  ```yaml
  inputs:
    password:
      type: string
      required: true
      private_value: true
      description: The user's password.
  ```

- **Hand a private value to a program through standard input,** never in
  the program's arguments, because arguments show in the node's process
  list. MMX replaces the value as the call gave it, and cannot replace a
  form that the module computed from the value, such as its hash.
- **An executable can mark its result `secret: true`** when the executable
  read private data itself.
- **Outputs cannot use MMX's own record keys** `$mmx_secret`,
  `$mmx_truncated`, or `$mmx_binary`. Such an output fails the node task.
- **MMX does not follow what the module writes elsewhere.** A secret that
  the module writes into a file or a request is in that file or request.

## Limits

| Limit | Value |
| --- | --- |
| A module's name | 64 characters |
| `module_description.yml` | 256 KiB |
| Steps of a Starlark script | 10 million for one try of the node task, far more than a module needs |
| Text that `print` keeps | The first 8 KiB |
| A file that `read_file` reads, and an `http_request` answer | 1 MiB |
| `standard_output` and `standard_error` of a command primitive | The first 1 MiB of each; longer output ends with `[MMX output truncated at 1 MiB]` and sets no `error` |
| A request to an executable | 16 MiB of UTF-8 JSON |
| A result of an executable | 1 MiB |
| JSON nesting in a request or result | 64 levels |

Module files travel separately from the request and do not count toward
its size. A module is trusted code: it runs as root, and MMX limits its
steps and time but does not keep the module away from the rest of the
node. [What MMX protects](../how-it-works.md#what-mmx-protects) in How MMX works
says what this means for your servers.
