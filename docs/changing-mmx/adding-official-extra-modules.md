# Adding an official extra module to MMX

This guide is for contributors who add or change a module in
`official_extra_modules/`. An **official extra module** is a Starlark script
for a less common tool, such as Incus or RabbitMQ, that the MMX project
writes, tests, and ships beside `mmx` in every release. Adding an official
extra module changes no Go code and needs no new build of the node runner
executable.

- To write a module for your own project, read
  [Write your own module](../how-to/write-your-own-module.md). The page
  `docs/how-to/write-your-own-module.md` explains scripts and the module
  description, and the [module interface reference](../reference/module-interface.md)
  lists the primitives. The guide `docs/changing-mmx/adding-official-extra-modules.md` builds on
  both.
- To add a module that is compiled into MMX, read
  [Adding a core module](adding-core-modules.md). Common work goes in core;
  [Where a module belongs](design/modules.md#where-a-module-belongs) says
  how to decide.
- [The official extra modules](design/modules-official-extra.md) owns the
  inputs and the behavior of each module, and the rules they share. Write
  the design in `docs/changing-mmx/design/modules-official-extra.md` first.

## What a module consists of

A directory `official_extra_modules/<module>/` with three files:

| File | Holds |
|---|---|
| `module.star` | The script. |
| `module_description.yml` | The summary, every input and output with a description a user can act on, and at least two examples. |
| `README.md` | The module's page, with the sections below. |

Also add Go tests of the script's logic in
`internal/modules/official_extra_<area>_test.go`. Add an integration suite
that proves the module on real nodes. Add a row in the table of
[Find the module for a job](../how-to/find-the-module-for-a-job.md#official-extra-modules).

When a module needs something that no primitive gives, add the primitive to
MMX's Go code as its own change, with its tests and its documentation, so
that every script can use it. Do not work around a missing primitive in a
script.

## Rules for every script

**The order of work in `run_node_task(inputs)`:**

1. Check the inputs that the description cannot check, such as a rule
   between two inputs. When inputs break a rule, `run_node_task(inputs)`
   returns `invalid_inputs` before anything else happens. Use the reason
   `<module>.<input>: <the rule>`.
   Never put the value of an input into such a reason, unless the input is
   a plain name.
2. Ask `find_program` for each program the task will run. A missing program
   returns `missing_prerequisite` with the reason `<module>: the program
   <program> is not installed; install the package <package> in an earlier
   node task`. Name the package for each family of distributions where the
   names differ.
3. Read the present state with the tool's read commands.
4. Compare, and change only what differs, with
   `run_command_that_changes_the_node`.
5. Return the reason, the outputs, and the fingerprint.

**Reading.**

- Never read a tool's progress text. Read the tool's structured output,
  such as JSON, before a change, and where it matters, after it. Where a
  tool prints only text, read the text with a closed list of words. When
  parsing text-only output, make anything the script does not know a
  failure that quotes the text.
- A read command that fails does not mean "the thing does not exist". Use
  the tool's exit code or message to distinguish a missing resource from a
  failed read. Fail with the tool's own message for every read
  failure that does not report a missing resource.
- When the tool's server does not answer, a dryrun returns `not_checked`,
  because an earlier node task may start the server. A run returns
  `failed` with the tool's message, and MMX tries again as for every
  module.
- **A file that another node task writes is not yet the run's file in a
  dryrun.** A module that reads a file written by another node task asks
  `earlier_node_task_would_change(path)`. When
  `earlier_node_task_would_change(path)` answers `True`, the reading module
  reports what a run would do with a changed file and skips comparison with
  the old file.
- **A state that an earlier node task may change returns
  `missing_prerequisite`.** A playbook often changes one thing and then
  uses the result: the playbook unmounts a filesystem and then deletes its
  volume, deletes the logical volumes and then their volume group, shuts a
  machine down and then removes its definition. In a dryrun the earlier
  node task has changed nothing yet, so the later node task finds the old
  state. With `missing_prerequisite` the dryrun reports the later node task
  `not_checked`, and a run, where the state is real, fails once with the
  reason. A state that no node task changes, such as a volume that would
  have to shrink, stays `invalid_inputs` and fails a dryrun too.
- **A failure that another try cannot fix returns `invalid_inputs`,** not
  `failed`, also when the cause is the node's state and not a mistake in
  the task: a login that the tool's server refuses, a volume that would
  have to shrink, a disk that holds data, a machine in a state that the
  task cannot change. MMX then tries once, in a dryrun as in a run.
  `failed` is for what another try may fix: a server that does not answer,
  a command that failed.
- **A failure that another try would hide returns `failed` with
  `try_again: False`.** MMX tries a failed task again, and the script then
  reads the node again. When the first try changed the node before it
  failed, the second try can find the changed state and report success:
  a dataset that was created and then could not be shared, a volume that
  grew while its filesystem did not. For such a failure the task must fail
  after one try, with a reason that says what was done and what failed.

  Every failed result with `created: true` must also return
  `try_again: False` and explain what was done and what failed.
  Another try would find the resource created by the first try and report
  `created: false` for the resource.

**Changing.**

- Every change goes through a changing primitive. `run_command` is for
  commands that only read; it runs in a dryrun too.
- **A dryrun claims only what a run would do.** In a dryrun a changing
  command does not run. Compute what the run would do from what the script
  read, and say so in the reason. Where the script cannot know before the
  run, return `not_checked` with the reason in plain words. Leave out an
  output that a dryrun cannot know, such as the state that a tool gives a
  thing the run would create. A later node task that reads the omitted
  output is then `not_checked` in the dryrun, never handed a guess.
- Check each changing command's `error` and `exit_code`. A failure returns
  `failed` with `<module>: <what the module tried, on which thing>: <the
  tool's own message>`. MMX records every changing command that ran, so a
  failure after a change still reports `changed`.
- Inspect the real state on every run. A previous run may have stopped
  between two changes; the next run finishes what is missing.
- Never pass an option that forces, wipes, or answers "yes" to a question
  about data, unless the design names it.
- Run no shell (`sh -c`).
- A wait is a bounded loop with `run_command(["sleep", "1"])` between
  reads.

**Private values.** A password reaches a program only through
`standard_input`. A password never goes into `arguments`, a file, an
output, or a reason. Mark the module's password input `private_value: true`
in the description. MMX replaces the value's own text in the result; it
cannot replace a form that the script computes, so compute none.

**The result.**

- `reason` in a run says what the task did, in the past tense, and names
  the thing: `created the logical volume data/postgresql with 50 GiB`. In
  a dryrun, `reason` says what a run would do: `would create ...`. Several
  changes are joined with `; `. An unchanged task gives no reason.
- `outputs` holds only what the design names.
- `fingerprint` is `json.encode` of the state the task asks for, without
  any private value. A node task block's service then acts when the wanted
  state changes.
- A failure's reason starts with the module's name and a colon. The reason
  says what is wrong, on which resource, and what to do.

**Words that every module uses.** A user who knows one module then reads
every other one:

| Case | The text |
|---|---|
| A missing program | `<module>: the program <program> is not installed; install the package <package> in an earlier node task`. |
| An input that is empty | `<module>.<input>: must not be empty` |
| A list without its setting | `<module>.<things>_not_listed: is required with <things>: remove <what remove does>, and keep <what keep does>` |
| The setting without its list | `<module>.<things>: is required with <things>_not_listed` |
| A removal state with another input | `<module>.state: <state> accepts only <the inputs it accepts>; remove <input>`. Every module refuses inputs outside the removal state's allowed inputs. No module ignores disallowed inputs. |
| A command that failed | `<module>: <what the module did, as "creating the virtual host shop">: <the tool's own message>` |
| What a run created | `created the <thing> <name> ...`; a dryrun says `would create ...` |
| `state: absent` | `removed the <thing> <name>` |
| `state: deleted_with_all_its_data` | `deleted the <thing> <name> with all its data` |

- A module whose task can create its thing returns the output `created`:
  true when this task created it, and in a dryrun when a run would.
- The values of `state` that say whether something runs are `running` and
  `stopped`, for both Incus instances and libvirt domains (virtual
  machines).
- MMX reports `changed` for every changing command that ran, also for one
  that the tool refused: it cannot see whether a failed command changed
  something. A page says so where a refused change is likely.

**Style.** Small functions with explicit names. A comment explains the
reason when the code does not show the reason. The comment at the top of
the script says what the module manages and names its design section. A
script has no `load`, so each script stands alone: two scripts for one tool
may hold the same helper function, and such copies stay identical.

## The module's page

`README.md` is the module's page. Every module page, core or official extra,
has the sections that the
[documentation map](documentation-map.md#the-sections-of-a-module-page)
lists, in that order; its column for official extra modules says which
sections this page requires. For an official extra module:

- `## Set up` gives the node tasks for each family of distributions the
  module is tested on.
- `## Outputs` names the outputs, or says that the module has none.
- `## What a dryrun reports` is a list: what a dryrun says for each kind of
  change, when it reports `not_checked`, when it fails as a run would, and
  what its read commands leave on the node, if anything.
- `## Errors and what they mean` is a table with the two columns "The reason
  holds" and "What it means, and what to do".
- `## Tested on` also names the distributions where the tool cannot be
  installed, with the reason.

## Tests

**Go tests.** A script's tool is not on the machine that runs the Go tests.
`internal/modules/official_extra_modules_test.go` has the helpers:
`stubTools` gives a script stand-in programs that print what the real tool
printed on a test node, and `runOfficialExtraModule` runs one node task and
returns its result. Cover for each module:

- every `invalid_inputs` rule with its message, and the missing program;
- unchanged state: no changing command runs (assert the list of calls);
- each change: the exact commands, in order, and the standard input that a
  private value went through;
- a dryrun that runs no changing command and reports what a run would do;
- a read that fails, and a change that fails after an earlier change;
- the tool's output on its oldest and its newest tested version, where
  they differ.

`TestOfficialExtraModulesAreComplete` checks every module without further
code: the three files exist, the description and its examples are valid,
the script compiles and defines `run_node_task(inputs)`, and the page names
every input and output. The tests in `docs/reference/modules/generator/` also load
every node task list on the page through MMX's validation, as the
[documentation map](documentation-map.md#what-the-tests-check) says.

**An integration suite** proves the module through the public CLI on real
nodes: a first run, an unchanged rerun, a dryrun that changes nothing, each
state and each removal, the failures the design names, and a run that
stopped between two changes and ran again. The integration suite checks the
node's state with the tool's own commands, independently of what MMX
reports.

For a private value, the integration suite uses a sentinel password. The
integration suite proves that the password is in no output of MMX, no saved
run, no job record on the node, no journal, and no file under `/root`.

- The suite installs the tool with MMX's own modules, through the node
  tasks that the page shows under "Set up", so the page is tested.
- On a distribution where the tool cannot be installed, the suite proves
  the behavior without the tool: a dryrun reports `not_checked` and names
  the program, and a run fails once with `failure_kind:
  missing_prerequisite`.
- The suite leaves its node as it found it, and names every package it
  needs.

[The testing guide](testing.md) says how to run a suite and which suites
need a virtual machine. Add a new suite to `SUITES` in
`tests/integration/all.py`, to the command list under
[Tests](acceptance.md#tests) in the acceptance contracts, and to the row
of its contract in [the acceptance checks](acceptance.md#acceptance-checks).
