# `update_alternatives`

Debian, Ubuntu, and the Red Hat family let several installed programs
provide one generic command, such as `editor` or `java`. The alternatives
system chooses which one the command runs.
`update_alternatives` sets that choice, through the node's
`update-alternatives` command; on the Red Hat family it is the same program
as `alternatives`.

## Examples

### Choose the editor

```yaml
- update_alternatives:
    name: editor
    program_path: /usr/bin/vim.basic
```

After this node task, the command `editor` runs `/usr/bin/vim.basic` on the
node. MMX reads the name's current choice and mode, and sets `program_path` in
manual mode (`update-alternatives --set`), so a later package upgrade does
not switch it back. The node task reports the old and the new choice.

### Register a program that no package registers

A JDK unpacked under `/opt` is not a registered choice of `java`. `generic_command_path` and
`priority_when_registered` register it first:

```yaml
- update_alternatives:
    name: java
    program_path: /opt/jdk-21/bin/java
    generic_command_path: /usr/bin/java
    priority_when_registered: 2100
```

`generic_command_path` is the path of the generic command that runs the
selected program, such as `/usr/bin/java`. `priority_when_registered` is a
whole number assigned to the program during registration. In automatic
mode, the alternatives system selects the registered program with the
highest numeric priority.

With `generic_command_path` and `priority_when_registered`, MMX first
registers `program_path` as a choice for `name` when the program is not
registered (`update-alternatives --install`). Then MMX selects `program_path`
for the generic command specified by `name`.

### Return to automatic mode

```yaml
- update_alternatives:
    name: editor
    automatic: true
```

When `automatic: true`, omit `program_path`. MMX returns the generic command
specified by `name` to automatic mode, which selects the registered program
with the highest priority. The `automatic` setting defaults to `false`.

## How it works

- `program_path` must be one of the name's registered choices; each package that
  provides the command registers its program as a choice. `program_path` is
  required unless `automatic: true`.
- Results and dryrun show `program_path_before` and `program_path_after`, the program the
  name runs before and after, and `automatic_before` and `automatic_after`,
  `true` in automatic mode and `false` in manual mode, in
  `.this_node_task.value`. Unchanged runs show no value. A run where
  `update-alternatives --set` or `--auto` fails shows no value, so a node
  task in `on_failure` can read what happened.

## Errors and what they mean

| The reason holds | What the error means, and what to do |
|---|---|
| `... is not a registered choice for editor; the registered choices are: ...` | A `program_path` that is not a registered choice, without `generic_command_path`, fails the node task and lists the registered choices. Install the package that registers it in an earlier node task, or give `generic_command_path` and `priority_when_registered`. A dryrun reports the node task not checked, because an earlier `apt_package` or `dnf_package` task may install the package that registers it. |
| `the alternatives system has no choices for ... yet; ...` | The name has no choices at all, here with `automatic: true`. The packages that provide the command register them. It is treated as above: not checked in a dryrun. |
| `update_alternatives supports nodes with apt or dnf; the node's package manager is ...` | The node is not one of the distributions above. A [`symlink`](symlink.md) node task can make a generic command run a program there. The node task fails in a dryrun too. |

## Tested on

**Distributions:** Debian 12 and 13, Ubuntu 24.04 and 26.04, Rocky Linux 9, AlmaLinux 10.

The suite `tests/integration/node_settings.py` runs on Debian 12 and 13, Ubuntu 24.04 and 26.04, Rocky Linux 9, and AlmaLinux 10, in containers on the first four and in virtual machines on the last two. The
[validation record](../../docs/changing-mmx/validation.md)
has the latest results.

## Why it works this way

- **A module named for its tool.** 20 independent authors set the
  alternatives system in the Ansible projects MMX was checked against, with a
  `shell` recipe.
- **`priority_when_registered` names when MMX uses the priority.** MMX uses
  `priority_when_registered` only when registering the program, following the
  [creation-name rule](../../docs/changing-mmx/design/modules.md#a-setting-used-only-at-creation-says-so-in-its-name).
- **Manual mode for a chosen `program_path`.** In automatic mode, a later package
  upgrade can switch the command to another program.
