# Secrets reference

This page lists the rules of MMX's secrets for looking up: where a secret's
value comes from, the vault file and how MMX unlocks the vault, each
`mmx secrets` command, which secrets a run needs and which secrets each
node receives, and where a secret can appear in files, output, and saved
records. A **secret** is a value that MMX keeps out of its output and saved
records, such as a password. A **secret key** is a secret's name, such as
`database_password`, which a node task reads as
`{{ .secrets.database_password }}`. The **vault** is the project's
encrypted file of secrets, `secrets_vault.age`. To create the vault and use
a secret step by step, see [Keep secrets](../how-to/keep-secrets.md).

## Where a secret's value comes from

MMX looks up each secret key on the controller machine, the computer where
you run `mmx`:

| Source | When MMX uses the source |
| --- | --- |
| An environment variable | The secret key is listed in `secrets_from_env` in `mmx_project_settings.yml`, such as `database_password: SHOP_DATABASE_PASSWORD`. MMX then never reads the secret key from the vault, even when the vault has the secret key. |
| The vault | Every other secret key. |

- An environment variable supplies a secret only through a
  `secrets_from_env` entry. The project files hold only the variable's
  name.
- A `secrets_from_env` variable must be set and nonempty when a command
  needs the secret. Otherwise the command fails with
  `secret database_password requires a nonempty environment variable SHOP_DATABASE_PASSWORD`.
- A command whose secrets all come from environment variables does not
  open the vault, so a project with only `secrets_from_env` needs no vault
  and no passphrase.
- MMX removes `MMX_VAULT_PASSWORD`, `MMX_VAULT_PASSWORD_FILE`, and every
  variable named in `secrets_from_env` from the environment of the `ssh`
  processes that MMX starts to reach a node. An OpenSSH `SendEnv` rule therefore cannot
  send these variables to a server.

### Which vault file

| Setting | The vault | A relative path starts at |
| --- | --- | --- |
| `--vault PATH` on an `mmx secrets` command, except `usage` | `PATH` | your working directory |
| `vault_file: PATH` in the selected inventory | `PATH` | the inventory file's directory |
| `vault_file: PATH` in `mmx_project_settings.yml` | `PATH` | the project directory |
| none of these | `secrets_vault.age` | the project directory |

The first setting in the table that is present wins. A run uses the vault
of the inventory that the run uses. On the `mmx secrets` commands,
`--inventory NAME` selects the inventory whose `vault_file` applies.

## The vault file

- **Format.** The vault is a file in the standard
  [age](https://age-encryption.org/) format, encrypted with a passphrase
  (age's scrypt mode). MMX has age built in, so you install no encryption
  tool. The first line of the file is `age-encryption.org/v1`.
- **Text inside.** The decrypted text is one YAML mapping. Each top-level
  name is a secret key, and its value can be any YAML value: a string, a
  block of several lines, a list, or a mapping. A mapping, such as
  `database` with `user` and `password` below, is one secret key, which a
  node task reads as `{{ .secrets.database.password }}`. A text of only
  comments is a vault with no secret keys.
- **A new vault** opens in `mmx secrets edit` with an example line and two
  comment lines:

  ```yaml
  example_secret_key: "replace this line with your own secret key and value"
  # Easier for one secret: close this editor and run mmx secrets set KEY. It asks for the value and hides what you type.
  # To save and close: in vim, press Esc, type :wq, and press Enter. In other editors, save and close.
  ```

  Replace the example line with your own secret, or add lines below the
  example line. The example line is not a comment, so an editor that
  continues comments, as vim does, does not turn your new line into a
  comment. Saved unchanged, the example line is a secret key that nothing
  reads; `mmx secrets usage` lists it under `Defined but never referenced:`.

- **Secret keys** start with a letter or an underscore and continue with
  letters, digits, underscores, or hyphens. A secret key appears once.
  `mmx secrets add-random` and `mmx secrets set` also refuse `true`,
  `false`, and `null` in any mix of capital and small letters, because
  YAML reads these words as values.
- **Size.** The decrypted text is at most 4 MiB.
- **Saving.** MMX checks the new text, encrypts the text, and replaces the
  vault in one step, so a failed save keeps the old vault. MMX writes the
  vault with mode `0600`.
- **In Git.** You can commit the vault. Each save encrypts the text again,
  so the file's bytes change even when no value changed.
  `mmx secrets diff-keys` compares the values instead.
- **Only the vault is encrypted.** MMX's saved run records on the
  controller machine, the results it shows, and the files it writes on
  nodes are protected by file permissions, not by encryption.
- **A copy of the vault** can be attacked by guessing its passphrase.
  Changing the passphrase later does not protect a copy that someone
  already has.

## Unlocking the vault

MMX takes the vault passphrase from the first of these sources that is
set:

| Order | Source |
| --- | --- |
| 1 | `--vault-password-file PATH` on the command |
| 2 | `MMX_VAULT_PASSWORD_FILE`, the path of a file on the controller machine |
| 3 | `MMX_VAULT_PASSWORD`, the passphrase itself, such as from a CI system's secret storage |
| 4 | A hidden prompt, `Vault passphrase:` |

- **A source that is set and fails stops the command.** A file that
  cannot be read, an empty passphrase, or a wrong passphrase is an error,
  and MMX does not try the next source or ask. `MMX_VAULT_PASSWORD_FILE`
  set to an empty string is an error too.
- **A passphrase file** is a regular file of at most 16 KiB that holds one
  nonempty line. A final line ending, `\n` or `\r\n`, is allowed. MMX
  reads the file as data, never as a command. When group or others can
  read the file, MMX prints a warning on standard error that names the
  file and `chmod 600 FILE`, and goes on. Create the file with mode `0600`.
- **A passphrase** is not empty and holds no line break. MMX accepts no
  passphrase as a command-line argument, and the passphrase never goes
  into the project, the saved run records, or a node job.
- **The prompt** appears only when standard input is a terminal, and the
  command has neither `--non-interactive` nor `--json`. Otherwise a missing
  source fails the command with
  `vault passphrase required; use --vault-password-file, MMX_VAULT_PASSWORD_FILE, or MMX_VAULT_PASSWORD (prompts are disabled)`.
  A command that creates a vault from the prompt first says that you
  choose the passphrase now, and keep it, such as in a password manager,
  because MMX cannot recover a lost one. It then asks
  `New vault passphrase:` and `Confirm new vault passphrase:`.
- **Which commands unlock.** Commands that do work on nodes, such as
  `mmx run`, `mmx dryrun`, `mmx resume`, and `mmx run-command`, unlock the
  vault once for each command, and only when the work reads a secret from
  the vault. `mmx explain`, `mmx nodes`, and `mmx validate` never unlock
  the vault. The `mmx secrets` commands unlock the vault as each command's
  section below says.
- **Never sent to a node.** MMX never sends the vault file or the
  passphrase to a node. MMX sends each node only the secret values that the
  node uses, as
  [Which secrets each node receives](#which-secrets-each-node-receives)
  says.

## The mmx secrets commands

| Command | What the command does | Opens the vault |
| --- | --- | --- |
| `mmx secrets edit` | Opens the whole vault in your editor, to view or reorganize it. Creates the vault if there is none. | Yes |
| `mmx secrets passwd` | Encrypts the vault with a new passphrase. | Yes |
| `mmx secrets add-random KEY` | Adds a secret key with a random value. | Yes |
| `mmx secrets set KEY` | Adds a secret key with a value from standard input, or asks for the value at a terminal. | Yes |
| `mmx secrets list-keys` | Prints the vault's secret keys. | Yes |
| `mmx secrets diff-keys` | Compares the vault's secret keys with the last Git commit. | Yes |
| `mmx secrets usage [KEY]` | Lists where each secret key is read. | Only when the vault file exists |

No `mmx secrets` command prints a secret's value, also not in an error
message. `mmx secrets edit` shows the values in your editor.

| Option | Commands | What the option does |
| --- | --- | --- |
| `--vault PATH` | all except `usage` | Works on this vault file. |
| `--inventory NAME` | all | Works on the vault that this inventory's `vault_file` selects. |
| `--vault-password-file PATH` | all | Reads the passphrase from this file. |
| `--new-vault-password-file PATH` | `passwd` | Reads the new passphrase from this file. |
| `--length N` | `add-random` | Makes a value of `N` characters. The default is 32. |
| `--replace` | `add-random`, `set` | Changes the value of a secret key that the vault has. |
| `--non-interactive` | all | Fails instead of asking a question. |
| `--json` | all | Prints JSON, and asks no question. |

Each command names the vault file from your working directory, in its
output and messages: `secrets_vault.age` in the project directory, and
`../secrets_vault.age` one directory below. JSON output holds the whole
path. A command that fails exits with code 4, or with code 130 after
Ctrl-C. `mmx secrets usage` also exits with code 1, as
[its section](#mmx-secrets-usage) says.

### mmx secrets edit

- MMX starts the editor that `VISUAL` names, else the one that `EDITOR`
  names, else `vi`. MMX runs the editor through `/bin/sh`, so a value with
  options, such as `code --wait`, works.
- The editor gets a temporary file in a new directory that only you can
  read. MMX removes the directory when the edit ends, also after Ctrl-C,
  SIGTERM, or a closed terminal. MMX exits only after the directory is
  removed, even when interrupted again.
- MMX checks the saved text as a vault text before MMX encrypts it. Text
  that is not one YAML mapping, or that has a secret key twice, fails
  with `vault must contain one YAML mapping with unique secret names`.
  The vault then stays as it was. At a terminal, MMX prints the reason and
  asks `Edit again? [Y/n]`; Enter or `y` reopens the editor with your
  edited text, in the same temporary file. Without a terminal, the command
  fails with the reason and says that nothing was saved, and the edit is
  lost.
- An editor that exits with an error leaves the vault as it was.
- Your editor's own backup and swap files are outside MMX's control.
- `--json` prints only the vault's path and `status`.

### mmx secrets passwd

- MMX opens the vault with the current passphrase, from the passphrase
  sources, and encrypts the same text with the new passphrase.
- The new passphrase comes from `--new-vault-password-file`, which follows
  the rules of a passphrase file. Without the option, MMX asks
  `New vault passphrase:` and `Confirm new vault passphrase:` at a
  terminal, and fails with
  `new vault passphrase required; use --new-vault-password-file (prompts are disabled)`
  elsewhere.
- Without a vault, the command fails and names the commands that create a
  vault.
- The secrets inside the vault stay the same. Older copies of the vault,
  such as in Git history, still open with the old passphrase.

### mmx secrets add-random

- MMX takes each character from the operating system's secure random
  generator, with the same chance for each of the 62 letters and digits.
- The command prints the secret key, the vault, and the value's length,
  never the value:

  ```text
  Added the key session_cookie_secret to the vault secrets_vault.age, with a random value of 48 letters and digits. MMX does not show the value.
  ```

- Without `--replace`, the command fails when the vault has the secret
  key, and changes nothing. With `--replace`, an existing secret key gets
  a new value in its place, and a missing secret key is added.
- A new secret key becomes a new last line of the vault's text,
  `KEY: "value"`. The comments and the order of the other secret keys stay
  as they were.
- A project without a vault gets a new vault. The passphrase comes from
  the passphrase sources, and the prompt asks twice.
- The command changes nothing when the vault's text does not end as a list
  of `key: value` lines, such as when all secret keys are written inside
  `{ }` on one line. The message says so; use `mmx secrets edit` then.
- The command refuses a secret key that `secrets_from_env` binds to an
  environment variable, because a run would never read the value from the
  vault. The message names the variable. A vault that `--vault` names
  outside a project has no project settings, so there the command accepts
  any secret key.
- `--json` prints only the vault's path and `status`.

### mmx secrets set

- The value comes from standard input, such as
  `some_program | mmx secrets set KEY` or `mmx secrets set KEY < FILE`.
  MMX removes exactly one line ending, `\n` or `\r\n`, at the end, so
  `echo token` and `printf %s token` give the same value. To keep a final
  line ending, as some private key files need, end the input with two.
- When standard input is a terminal, the command asks `Value of KEY:` and
  hides what you type. With `--non-interactive` or `--json`, the command
  fails instead of asking.
- A value from a pipe or a file takes the terminal's place on standard
  input, so MMX cannot ask for the passphrase then: give the passphrase
  through a passphrase source.
- MMX opens the vault and checks the secret key before MMX reads the
  value, so a wrong passphrase or an existing secret key is reported
  before the prompt.
- An empty value is refused, because an empty value is almost always a
  program that printed nothing. A value that is not UTF-8 text is refused,
  because the vault is YAML text: encode such data first, such as with
  `base64`.
- A value of one line is written in double quotes, such as
  `api_token: "the value"`. A value of several lines, such as a
  certificate, is written as a YAML block (`|`), unless YAML can show the
  value only in quotes.
- With `--replace`, an existing secret key keeps its place: its lines,
  from the secret key's own line down to the next secret key, become the
  new lines. Comments above and below stay. A comment at the end of the
  secret key's own line goes with the old value. A missing secret key is
  added as with `add-random`.
- A new secret key, a new vault, a text that is not a list of `key: value`
  lines, and a secret key in `secrets_from_env` work as for `add-random`.
- The command prints nothing when it succeeds. `--json` prints only the
  vault's path and `status`.

### mmx secrets list-keys

- The command prints the vault's top-level secret keys, sorted, one on
  each line. A secret whose value is a mapping is one secret key.
- `--json` prints `vault` and the list `keys`.
- MMX keeps no list of secret keys outside the vault. A team that wants
  the list in Git commits the output of `list-keys` as a file of its own.

### mmx secrets diff-keys

- The command compares the vault file as it is now with the same file in
  the last Git commit, and prints three lines. A line without a
  secret key shows `(none)`:

  ```text
  Vault: secrets_vault.age, compared with the last commit

  Added:    mail_api_token
  Removed:  (none)
  Changed:  session_cookie_secret
  ```

- The command compares values, so a change of comments or of line order
  is no difference.
- The command exits with code 0 whether or not a secret key differs.
- MMX reads the committed vault with the `git` program, in the vault's
  directory, and opens both vaults with the passphrase of the vault on
  disk.
- The command fails, with a message that names the cause, when `git` is
  not installed, when the vault is not in a Git repository, when the
  repository has no commit, when the last commit does not hold the vault,
  or when the committed vault does not open with the passphrase.
  [Change the passphrase](../how-to/keep-secrets.md#change-the-passphrase)
  says what to commit after `mmx secrets passwd`.
- `--json` prints `vault` and the lists `added`, `removed`, and `changed`.

### mmx secrets usage

- The command reads every playbook in `playbooks/`, the roles that the
  playbooks call, every [one-time playbook](../how-to/make-a-one-time-change.md)
  that is not completed, and one inventory: the default inventory, or the
  one that `--inventory NAME` names. The inventory decides which vault
  applies. For the connection secrets of another inventory, run the
  command with that inventory.
- The command contacts no node. The command accepts no `--vault`: the
  vault is the one that a run with the same inventory uses.
- The first line of each secret key says where the value comes from:
  `vault secrets_vault.age`, `environment variable SHOP_DATABASE_PASSWORD`,
  or `not defined`. A secret key in `secrets_from_env` counts as defined,
  whether or not the variable is set.
- The lines below name each use, with the file and line, ordered by file
  and line. A use is each place where a project may write `.secrets.KEY`:
  an input of a node task, a template file, a `shell` task's `env`, a
  `when`, a `loop`, a retry condition, `set_vars`, `show`, an input of a
  role call, a fleet stage's own settings such as `when` and `outputs`
  (`fleet stage database, output token`), and a node's connection setting
  (`node web01, sudo_password`).
- A node task is named by its module, then its `path`, `name`, `image`, or
  `url` as written, then the input in parentheses:
  `file /root/.my.cnf (content)`. A node task without these inputs is
  named by its module and the input: `shell env DATABASE_PASSWORD`. A file
  whose `template` path differs for each node, such as
  `templates/{{ .node.name }}.tmpl`, is listed at its node task's line
  without an input's name.
- MMX follows a secret through role inputs, also through a role that
  passes its input on to another role. Each use inside a role is an
  indented line below the role call:

  ```text
  database_password   vault secrets_vault.age
    playbooks/deploy_platform.yml:54   shell env DATABASE_PASSWORD
    playbooks/site.yml:88              role database, input root_password
      roles/database/main.yml:31       file /root/.my.cnf (content)
  ```

- MMX does not follow values made from a secret, such as a variable that
  `set_vars` assigns, a node task's result, an output, or the items of a
  `loop` over a secret. The place that reads the secret key is listed.
- `vars` in the project, an inventory, a group, or a node are plain data.
  `{{ .secrets.KEY }}` inside a variable is plain text, and no use.
- Without `KEY`, the command ends with two lists.
  `Referenced but not defined:` holds the secret keys that a playbook
  reads and that neither the vault nor `secrets_from_env` defines, with
  a place that reads each one. `Defined but never referenced:`
  holds the secret keys that nothing reads. The command exits with code 1
  when the first list has a secret key, and with code 0 otherwise.
- A secret key that only left-out fleet stages read is not in the first
  list, because no run with this inventory needs it. A fleet stage counts
  as left out when its selection is known before the run from node and
  group names, `vars`, and the inventory, is empty, and the fleet stage
  sets `skip_when_no_nodes_are_selected`; or when its `when` is false on
  every node it selects. Fleet stages inside a `for_each_node` loop that
  selects no nodes count too. The command lists such a secret key with its
  uses, and a note:

  ```text
  staging_database_password   not defined
    (secret key not needed with the selected inventory: only fleet stages that select no nodes, or whose when is false on every node, read the key)
    playbooks/site.yml:31   shell env DATABASE_PASSWORD
  ```

  A fleet stage whose `nodes` reads an earlier fleet stage's outputs may
  select nodes during the run, so its secret keys count as needed. So do
  the secrets that `save_on_controller_machine` templates read, and a
  node's connection secrets.
- With `KEY`, the command prints only that secret key's lines. The command
  exits with code 1 only when the secret key is read, not defined, and
  needed with this inventory. A name that nothing defines and nothing
  reads fails with exit code 4.
- `--json` prints the list `keys` and the lists
  `referenced_but_not_defined` and `defined_but_never_referenced`. Each
  entry of `keys` has `key`, `source` (`vault` with the vault's path,
  `env_var` with the variable's name, or `null`), and `uses`. Each use has
  `file`, `line`, `used_by`, and, for a role call,
  `uses_inside_the_role`. A secret key that only left-out fleet stages
  read has `only_read_by_fleet_stages_left_out_before_the_run: true`.

## Which secrets a run needs

`mmx run`, `mmx dryrun`, and `mmx resume` load the secrets of the fleet
stages that they run before they start work on any node. A secret that is
missing from the vault or from its environment variable stops the command
then, such as with `vault is missing required secret mail_token`, before
any node changes. A new `mmx run` or `mmx dryrun` stopped this way contacts no
node, so MMX saves no run and gives no advice to resume it. A connection secret, `ssh_password` or `sudo_password`,
is the exception: MMX reads it when it connects to that node, as the list
below says.

A fleet stage that the run leaves out before the run starts needs none of
its secrets:

- a fleet stage that `--only-fleet-stages` leaves out;
- a fleet stage or `for_each_node` loop that selects no nodes before the
  run: its group is empty and it sets
  [`skip_when_no_nodes_are_selected`](../how-to/choose-which-servers-a-run-touches.md#keep-a-fleet-stage-for-a-group-that-may-be-empty),
  or `--limit` leaves out every node that it selects. The fleet stages
  inside such a loop are left out with the loop;
- a fleet stage of the playbook's top level whose `when` is false on every
  node that it selects, decided from values known before the run: each
  node's `vars` and name.

So a team without a staging server keeps no placeholder password for
staging in the vault. A placeholder that no node uses would hide a missing
password on the day the staging server is added. In the same way, one
playbook can hold two ways of doing one job, such as moving a database
directly or through a relay, with a `when` on a setting choosing one: the
way not chosen needs no secrets.

- A `when` that reads a fact, a secret, or an earlier fleet stage's output
  is decided only during the run, so its fleet stage needs its secrets.
- A fleet stage whose `nodes` reads an earlier fleet stage's outputs
  selects its nodes during the run, so its secrets are needed.
- Inside a loop, MMX checks each fleet stage once for all iterations. A
  fleet stage whose `nodes` reads the loop's variable counts as selecting
  nodes. Inside a `for_each_node` iteration, `--limit` does not narrow a
  fleet stage, as in the run.
- Only fleet stages of the playbook's top level are checked for a `when`
  that is false before the run, because a `when` inside a loop may read
  the loop's variable.
- A fleet stage that selects no nodes, or whose `when` is false on every
  node, still writes its `save_on_controller_machine` templates, so the
  secrets that the templates read are needed.
- An `ssh_password` or `sudo_password` from a secret is read when MMX
  connects to the node whose connection settings name the secret.
- `mmx resume` uses the run's saved playbook, inventory, limit, and
  `--only-fleet-stages` value, so `mmx resume` needs the same secrets as
  the run.

## Which secrets each node receives

The controller process, the running `mmx` command, sends a secret's value
only to the nodes whose node tasks read the secret. A selector that the
controller process can resolve for a node narrows what the node receives,
also inside a secret:

- `.secrets.passwords[.node.name]` sends each node only its own entry of
  the secret `passwords`.
- `.outputs.inspect[.node.name].token` sends each node only its own
  exported token.

- A selector that cannot resolve fails that read on the node, and the node
  receives nothing for the read.
- A selector that only the node can resolve, from facts, results, a node
  task's loop items, role inputs, or a variable that the node job sets
  again, sends the whole secret. `.secrets.passwords[.item]` sends all of
  `passwords`, because the node chooses the entry.
- A template `index` with literal or reference keys selects like the same
  path: `{{ (index .outputs.inspect .node.name).token }}` sends that
  node's token, and fails on a missing key. With another kind of key,
  `index` reads the whole collection.
- A template reads specific values. A read of the whole scope, such as
  `{{ . }}`, fails validation.
- `mmx resume` unlocks the same secrets as the run. Each node job still
  receives only what it reads.

### A secret cannot select another value

A node gets a secret only as a value to use, never as the key of a lookup:

- `.vars.ports[.secrets.which_port]` fails when MMX loads the project,
  with the file, line, and field. Read `.secrets.which_port` itself, or
  select with a value that is no secret, such as an inventory variable.
- Inside a role, the same holds for a role input declared `secret: true`:
  `.inputs.ports[.inputs.which_port]` fails when MMX loads the project if
  `which_port` is declared `secret: true`. Read the input directly, or
  select with an input that is not secret, set by the role call from a
  value that is no secret.
- A value that holds a secret only during the run cannot select either: a
  variable that `set_vars` set from a secret, a loop item from a secret,
  or a node task result that is secret. `mmx validate` does not see such
  values. During the run, the node task fails with a message that names
  the lookup and the reference used as its key.

## Secrets in files, output, and history

MMX follows secret values through node task inputs and results. MMX does
not look for text that resembles a secret: a password typed into a project
file is ordinary file content. MMX shows resource names and paths, such as a file's path, in output and
in saved records, so do not put a secret into a name or a path.

- **In a file that a node task writes,** the value is in plain text, with
  the owner and permissions that the node task sets, because writing the
  value there is the node task's purpose.
- **In a `shell` node task,** give a secret through `env`, never inside
  `script`, because a command line can be read by other accounts on the
  node.
- **In output,** MMX shows `[redacted]` in place of a secret value, such
  as `outputs: {"join_token":"[redacted]"}`, and
  `contents suppressed for a task using secrets` in place of a file's
  difference. Statuses and exit codes stay visible.
- **In JSON output,** MMX leaves out secret values, also in command output
  made from a secret, in assignments, and in role and fleet stage outputs.
- **In saved records,** MMX saves no secret value in the run records on
  the controller machine or in the node job records on a node.
- **A role that receives a secret** declares `secret: true` on that
  input. A role call that passes a secret to an input without
  `secret: true` fails with
  `role input database_password receives a secret; declare secret: true`.
- **A module that is not core** can mark its result as secret when the
  module gets private data itself. A secret that such a module writes
  somewhere else, such as into a file or a network request, is outside
  MMX's control.

### Which node tasks have a hidden result

A node task that reads a secret has a hidden result. MMX shows the node
task's status and whether it changed something, and in place of a
failure's reason shows `details suppressed for a task using secrets`,
because a node task such as `shell` may print the secret.

- **A value made from a secret is a secret too:** a variable that
  `set_vars` set from a secret or from such a variable, and an output of a
  node task whose result is hidden. A node task that reads such a value
  has a hidden result.
- **Node tasks after a secret was read are hidden in a role and in a node
  task block.** A node task that runs something on the node and reads a
  secret may leave the secret there, such as in a file, where a later node
  task could read the secret back. So the node tasks that start after such a node
  task in the same role or node task block are hidden too, and so is the
  result of the role call or the node task block itself. The node tasks
  hidden this way are those in the reading node task's role and in every
  role and node task block around that role. A node task after the
  outermost role call or node task block is not hidden.
- **`set_vars`, `show`, and `assert` hide less.** A secret that `set_vars`,
  `show`, or `assert` reads hides only the node tasks that read the values
  made from the secret, because these three modules run nothing on the
  node.
- **Passing a secret on hides nothing.** A role call that passes a secret
  to an input hides nothing by itself, and neither does a role that passes
  the input on to another role.
- **`secret_because`.** With `--json`, each hidden result says why, such
  as `node task shop/write_database_settings read a secret before it
  started and may have left it on the node; a node task that starts
  before shop/write_database_settings is not hidden`.

In this role, the `apt_package` node task shows its result, the `file`
node task is hidden because it reads the secret, and the `systemd_service`
node task is hidden because it starts after the `file` node task:

```yaml
# roles/shop/main.yml
inputs:
  database_password:
    type: string
    required: true
    secret: true

node_tasks:
  - apt_package: # shown: it runs before any node task reads the secret
      packages: [{name: shop, version: newest_when_first_installed}]
  - id: write_database_settings
    file: # hidden: it reads the secret
      path: /etc/shop/database.conf
      content: "password={{ .inputs.database_password }}\n"
      permissions_mode: "0600"
  - systemd_service: # hidden: it comes after the file task
      name: shop
      state: running
```

#### An assert shows its reason

An [`assert`](modules/assert.md) node task shows the reason of its
failure, its `message` or else its condition as written, unless that text
reads a secret. The reason shows also after a node task that read a
secret, and also when the condition reads a secret. A role call or node
task block that fails because of the assert shows the same reason. With
`--json`, each result that shows its reason this way has
`reason_reads_no_secret: true`. A `message` that reads a secret, or reads a
hidden node task's result, shows as
`details suppressed for a task using secrets`. Added at the end of the
`shop` role, this assert fails on an Ubuntu node with the reason
`The shop role supports Debian only, not ubuntu`:

```yaml
- assert: # shown: its message reads no secret
    that:
      equals: ["{{ .node.distribution.id }}", debian]
    message: "The shop role supports Debian only, not {{ .node.distribution.id }}"
```

An assert reads no file on the node: its text comes only from values, and
MMX knows which values hold a secret. So an assert that stops a run, such
as one that waits for approval of a restart, can say why.

#### A command that promises to print no secret

A `shell` node task can set `output_holds_no_secret: true`, also inside
`file.check_new_file`, when its program prints no secret. A run and a
dryrun then show the command's standard output, standard error, and
failure reason, with `[redacted]` in place of:

- each secret value that the node job holds or the node task read;
- each such value without surrounding whitespace, and each nonempty line
  of the value without surrounding whitespace;
- the keys of a secret mapping;
- the JSON text that MMX gives a secret list or mapping through `env`.

For a file check, the file node task's reason that quotes the failed check
shows too. The node task stays secret otherwise: file differences, module
outputs, and outputs made from the standard output stay hidden. MMX cannot
know other forms of a secret, such as part of a line or its Base64 text, so
the program must not print such forms either. The default is `false`. Together
with `output_is_secret: true`, the setting is refused when MMX loads the
project. See [`shell` secret output](modules/shell.md#secret-output) and
[checking a file that holds secrets](modules/file.md#check-a-file-that-holds-secrets).

#### The hidden details of a failure

When a node task with a hidden result fails, the node saves the hidden
details, and the reason names the file:

```text
details suppressed for a task using secrets; root can read them on the node in /run/mmx/private/<job-id>-hidden-details-of-failed-node-tasks.txt for 72 hours, or until the node reboots
```

- The file holds the node task's name, status, exit code, and reason, and
  what the command printed on standard error and standard output, such as
  `psql`'s own `ERROR:` line.
- `<job-id>` is the node job's ID. A node job is one attempt of one fleet
  stage on one node.
- The file may hold the secret, so only root can read the file.
  [Keep secrets](../how-to/keep-secrets.md#if-something-is-different)
  shows how to read it.
- The file is in memory. The node removes the file with the node job's
  records after 72 hours, and a reboot removes the file too.
- With `--json`, the result names the file in `hidden_details_file`. A
  role call, a node task block, or a loop that failed because of the node
  task names the same file.

### A login does not hide a node task's result

The modules in this table use a secret only as a login. Each module hands
the login to its tool through a file in the
[node task's private directory](#a-node-tasks-private-directory), through
the Docker daemon's socket on the node, or, for the role that `psql` logs
in as, in the environment of the `psql` process, and puts the login
nowhere else:

| Module | Its login inputs |
|---|---|
| `git` | `username`, `password`, `ssh_private_key` |
| `docker_image` | `registry_username`, `registry_password` |
| `docker_compose_project` | `registry_username` and `registry_password` in each entry of `registry_logins` |
| `podman_image` | `registry_username`, `registry_password` |
| `postgresql_role` | `login_role`, `login_password`, and `password`, which `psql` gets only as the stored form that MMX computes from it |
| `postgresql_database` | `login_role`, `login_password` |
| `mysql_user` | `password`, `login_user`, `login_password` |
| `mysql_database` | `login_user`, `login_password` |

A secret in one of these inputs does not hide the node task's result:

```yaml
- id: shop_image
  docker_image:
    image: registry.example.com/shop/web:2.4
    pull_image: always
    registry_username: deploy
    registry_password: "{{ .secrets.registry_password }}"
```

The result of `shop_image` shows `value.image_id` and
`value.previous_image_id`. When the registry refuses the pull, the result
shows the reason:

```text
docker_image: docker pull --quiet registry.example.com/shop/web:2.4: Error response from daemon: pull access denied for shop/web, repository does not exist or may require 'docker login'
```

A later node task that reads the result, such as
`{{ .node_tasks.shop_image.value.image_id }}`, does not become secret
because of the login. The same holds inside a role whose node tasks hand a
secret input only to a tool as a login.

A tool that fails may print the login it got. So when a login input's
value comes from a secret, MMX replaces each exact occurrence of the value
with `[redacted]` in everything the node task reports: its reason, its
value, its output, each of its tries, and each item of its loop. MMX does
the replacement on the node, before the result goes to later node tasks,
to the node job records, or to the controller machine. MMX replaces the
login as plain text and in each form in which the tool gets, keeps, or
sends the login:

- the Base64 text of the user name, a colon, and the password, which the
  login file of Docker and Podman holds and which both send to a registry;
- the header in which a dryrun sends the login to the Docker daemon;
- the user name or the password as a web address holds it, such as
  `robot%24shop` for `robot$shop`;
- a PostgreSQL task's login password as PostgreSQL's password file holds
  it, with a backslash before each colon and backslash;
- the stored form of a PostgreSQL role's password (`SCRAM-SHA-256$...`),
  both the form that MMX sends to the server and the form that the server
  already holds for the same password;
- the value that a MySQL or MariaDB server stores for the `password` of a
  `mysql_user` task, which MMX computes and sends in place of the
  password, also where a failure shows only part of that value;
- the login of a MySQL or MariaDB task as MMX's login file for the client
  writes it, with a backslash before a double quote and a backslash.

MMX replaces only a value that comes from a secret: a user name written in
the playbook stays, and an empty value is never replaced. `mmx explain` and
a dryrun name the secret of a login input and never show its value.

The result of a node task with a login is still hidden in two cases:

- the node task reads a secret somewhere else, such as in its `when` or in
  the `ssh_known_hosts_lines` of a `git` task;
- an earlier node task in the same role or node task block read a secret,
  as [Which node tasks have a hidden result](#which-node-tasks-have-a-hidden-result)
  says. A node task with a login that runs before the reading node task
  keeps a visible result.

These modules give the secret to their tool and do nothing else with the
secret, so MMX knows each form in which the tool holds it. A `shell` or
`file` node task can print part of a secret or change its form in ways
that MMX cannot know, so MMX hides that node task's whole result instead.

Module pages that rely on this section: [`git`](modules/git.md), [`mysql_user`](modules/mysql_user.md), [`podman_image`](modules/podman_image.md), [`postgresql_role`](modules/postgresql_role.md).

### Where secrets stay on a node

- **`/run/mmx/private/`** holds the secret inputs of node jobs, the secret
  values that a node job computes, a node's secret fleet stage outputs, and
  the hidden details of failures. `/run` is in memory (tmpfs), so these
  files are written to no file system on disk. MMX refuses to work when
  `/run` is not tmpfs. The directory belongs to root with mode
  `0700`, and its files have mode `0600`.
- **A secret input** reaches the node job worker process, the
  `mmx-runner` process that carries out a node job, through such a file,
  and the node removes each input file as the process reads the file.
- **Computed secret values** disappear when the node reboots or when the
  node job's records expire. No secret value goes into the node job
  records.
- **`/run/mmx/`** and `/run/mmx/private_task_directories/` belong to root
  with mode `0711`.

`mmx resume` reads named secrets again for unfinished work, and says so.
A computed secret output saved without its value is unavailable after the
controller process restarts: MMX then blocks the node task that needs the
value, names the fleet stage and node task that made the value, and does
not run that completed node task again. A node job whose outcome is
uncertain can still return its outputs through `mmx status` while its
record exists on the node.

### A node task's private directory

Some node tasks hand a tool a file that no other account may read, for as
long as the node task runs. The node task writes such a file into a
directory of its own, whose name starts with the node task's module:

```text
/run/mmx/private_task_directories/git-2814750361/password
```

| Node task | What the private directory holds |
|---|---|
| `git` with `username`, `password`, `ssh_private_key`, or `ssh_known_hosts_lines` | The files from which `git` reads these values |
| `docker_image` or `docker_compose_project` with a registry login, in a run | The login file that `docker` reads |
| `podman_image` with a registry login | The login file that `podman` reads |
| `postgresql_role` or `postgresql_database` with `login_password` | The password file that `psql` reads |
| `mysql_user` or `mysql_database` with `database_server_host`, `login_user`, or `login_password` | The login file that the MySQL or MariaDB client reads |
| `netplan_file` in a run | Copies of `/etc/netplan`, `/run/netplan`, and `/lib/netplan`, in which `netplan generate` checks the proposed file |
| `systemd_unit` with `in_systemd_of_user_account` | A copy of the new unit file, which the account checks with `systemd-analyze verify` |
| `podman_quadlet` | A copy of the new Quadlet file, which Podman's generator checks |
| `docker_compose_project` in a dryrun | The files that Docker Compose's own dry run writes |

- **In memory only.** On a node whose `/run` is on a disk, the node task
  fails and writes nothing.
- **Readable by one account.** The directory belongs to the account that
  the tool runs as: root, the `run_as_user_account` of a `git` task, the
  account of `in_image_store_of_user_account` or
  `in_systemd_of_user_account`, or `postgres` for a PostgreSQL task that
  uses the local socket. The directory's mode is `0700`. An ordinary
  account can pass through `/run/mmx/` and
  `/run/mmx/private_task_directories/` to its own directory, but cannot
  list or create anything in these two directories.
- **Removed when the node task ends,** also when the node task fails or is
  cancelled.
- **Removed after a kill too.** When the node job worker process is killed
  during a node task, such as by the kernel after the node runs out of
  memory, the node task cannot remove its directory. The node then removes
  leftover directories at the next request about a node job when no node
  job runs, such as while `mmx run` follows the node job, or at a later
  `mmx status` or `mmx resume`. At the latest, the node removes leftover directories
  before the next node job starts. The directory of a node task whose node
  job still runs is never removed.

Module pages that rely on this section: [`systemd_service`](modules/systemd_service.md), [`git`](modules/git.md), [`mysql_database`](modules/mysql_database.md), [`mysql_user`](modules/mysql_user.md), [`netplan_file`](modules/netplan_file.md), [`podman_image`](modules/podman_image.md), [`postgresql_database`](modules/postgresql_database.md), [`postgresql_role`](modules/postgresql_role.md), [`systemd_unit`](modules/systemd_unit.md).

## Secrets that start on a node

Two settings mark a value as secret on the node where the value starts:
`content_is_secret: true` on a [`read_file`](modules/read_file.md) node
task, and `output_is_secret: true` on a
[`shell`](modules/shell.md#a-command-that-prints-a-secret) node task. A
fleet stage output made from such a value is secret with no further
marking. [Keep secrets](../how-to/keep-secrets.md#a-secret-that-a-node-makes)
has an example.

- **Travel.** The output goes from the node to the controller process over
  the connection, and the controller process sends the value from memory
  to the nodes whose node tasks read the value, and to no other node.
- **Saved records.** `mmx results` shows `[redacted]`. The value is never
  written to the run records on the controller machine or to a node's
  saved results. The producing node keeps the value only in
  `/run/mmx/private/`, until the node job's records expire.
- **The origin is public.** The run records keep the output's origin: the
  fleet stage, the node, the node task, and, for `read_file`, the path.

Because MMX does not save the value, `mmx resume` cannot restore the value
from the run records as MMX restores an ordinary output:

- **A `read_file` secret is read again.** `mmx resume` connects to the
  node that ran the producing `read_file` node task, and reads the file
  again before starting work that uses the output. A token that the node
  changed in between arrives as it is now. When the node cannot be reached
  or the file is gone, the unfinished work that needs the output is
  `blocked`, with the reason.
- **A `shell` secret blocks its users.** MMX never runs a completed
  command again to get its output. On resume, the work that uses the
  output is `blocked`, with a message that names the producing fleet stage
  and node task and says to start a new `mmx run`.

[A secret that a node makes](../how-to/keep-secrets.md#a-secret-that-a-node-makes)
says how to keep a value that later fleet stages need after an
interruption.

- `mmx resume` reads a file again only when unfinished work in the resume
  uses the secret: a fleet stage, a loop iteration, or a file that
  `save_on_controller_machine` still has to write. MMX contacts no node for
  a secret that no unfinished work needs, so a server that is gone does
  not hold up the resume.
- An unfinished fleet stage counts as using every secret that the fleet
  stage reads, also in a node task whose `when` would skip the node task.
- MMX reads the needed files before starting any work, once for each node
  and file, through the node's ordinary connection, from several nodes at
  once, as `read_file` does. MMX refuses to read the file from a machine
  that replaced the original node.
- Ctrl-C or `mmx stop` ends the reads of secret files. A secret whose read was
  interrupted stays unavailable until the next `mmx resume`.
- Pass the content on whole, as in
  `"{{ .node_tasks.join_token.content }}"`, for `mmx resume` to read the
  file again. An output that adds text around the content, such as
  `"K10{{ .node_tasks.join_token.content }}"`, is computed on the node, so
  on resume its users are blocked as for a `shell` secret.
