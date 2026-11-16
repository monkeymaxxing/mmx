# MMX documentation map

This map says where each kind of fact about MMX is written, and what else to
update when that fact changes. Read the documentation map before you change
MMX's behavior, a name that users read, or any document. The documentation
map is written for everyone who changes MMX, people and coding agents alike.

## The rules

- **One fact, one owner.** Each kind of fact has exactly one owner document,
  listed below. Another document may state the fact in one sentence and
  link to the owner document. A document that does not own the fact never
  holds a second full description, because two descriptions drift apart.
- **Links in both directions.** When a document relies on a section of
  another document, it links to that section, and that section links back.
  The most common case: a module page relies on a section that several
  modules share, and that section lists every module page that relies on it.
  Whoever changes a linked document or section then finds the document or
  section at the other end of the link.
- **The current build only.** Documents describe what MMX does now. Unbuilt
  design lives in the [design plan](design/README.md) and its design
  documents. The [writing rules](#writing-rules) say what to delete.
- **Generated files are never edited by hand.** The first line of each
  generated file names the source files. Edit the named source files, then
  generate again.
- **Tests enforce the rules that a program can check.** The
  [section on tests](#what-the-tests-check) lists the rules checked by tests.

## Writing rules

Write for a smart reader who can code and knows basic Linux: files, shell
commands, configuration files, and SSH. Do not assume system administration details
or knowledge of MMX. Each sentence must say what happens, which thing is
meant, and on which machine it happens. Remove ambiguity without removing
technical detail. Split long sentences and put the normal case first.
Repeat a full term when a shorter reference could mean more than one thing.

- Use the [MMX glossary](../../AGENTS.md#public-terminology-and-examples).
  Define each term in plain words at first use in each document. Say
  controller machine, controller process, fleet stage, node job, or node
  task block in prose; YAML nesting permits the short keys. Do not shorten
  a term to a fragment such as "the controller" or "the engine".
- Apply the [explicit-names rule](../../AGENTS.md#names-that-users-read) to
  every name and value. Use obvious placeholders such as `example.com`,
  `web01`, and `myapp`. To teach a mechanism, choose an obvious example;
  `person: Barbara` leaves less to guess than `name: Ada`.
- Give each document one audience and purpose, as the ownership table below
  states. Put a fact in its owning document and link to it elsewhere.
  Keep internal protocols and Go types in contributor or module-author
  guides; show users the commands and YAML they use.
- Introduce the controller machine and nodes before the work they do.
  Build concepts in the order a newcomer needs them. Prefer one example
  that grows step by step rather than several unrelated examples. Put limits
  after the normal case.
- Examples are whole working pieces, with required settings and related files.
  Explain what happens, in order, and on which machine after each example.
  Comment a line when its meaning depends on another line.
- Every sentence gives the reader a fact to use and is true exactly as
  written. Remove a sentence that only announces what follows, and praise
  or scene-setting that nobody checked. Write "will", "every", "always",
  and "never" only when no case is an exception. A number or a default must
  not read as a limit. Say what an unfamiliar name is where the name
  appears, such as "`dnf_package` for dnf on Fedora". Say what the reader
  gains before how MMX works.
- Use repository-relative links and unnumbered headings. Label each diagram
  part with its full term and where it runs.
- A revision preserves product facts. Before removing a misplaced fact from
  a document, move the fact to the document that owns the subject.
  Read the whole affected document afterwards for undefined terms,
  repetition, missing context, and stale claims.
- Describe current behavior and the technical reasons for decisions.
  Unbuilt requirements belong in design documents and remaining work in
  the design plan. Remove completed plans, fixed findings, superseded
  results, rename history, and review or approval narratives. Git keeps
  development history. Preserve measured methods, results, limitations,
  and migration information that readers need to interpret evidence or
  use a released version.

## Where each kind of fact lives

### Entry points

- [`README.md`](../../README.md) is for users who have never heard of MMX: what
  MMX does, why to use a tool instead of shell scripts, where MMX differs
  from Ansible and when Ansible or Salt fits better, one example with real
  output, how to install and start, the setup prompt and
  checklist for a coding agent, the usual first failures, and links to the
  guides. Its claims about Ansible and speed come from the
  [performance record](../reference/performance.md).
  The table under "The Monkeymaxxing tools" is the same in the README of
  every Monkeymaxxing tool. Change the table in all of them together.
- [`AGENTS.md`](../../AGENTS.md) is for contributors and coding agents: MMX's glossary, the
  compatibility rules for changes, and links to this map and the other guides.
- [`CONTRIBUTING.md`](../../CONTRIBUTING.md) is for people outside the project:
  how a bug report, an idea, or a pull request becomes a change to MMX.

### Owner documents

| Document | Owns | Reader |
|---|---|---|
| [How MMX works](../how-it-works.md) | The parts of MMX, where each one runs, how a run works, the two connections, and what MMX protects. | A user who wants to understand MMX |
| [Get started](../get-started.md), the [how-to pages](../README.md), and [When something fails](../when-something-fails.md) | One job each, from the first server to secrets, rollouts, and recovery, and the error messages with what to do. | A user |
| The [reference pages](../README.md#look-things-up): [commands](../reference/commands.md), [project settings](../reference/project-settings.md), [inventory file](../reference/inventory-file.md), [playbook file](../reference/playbook-file.md), [secrets](../reference/secrets.md), and [shared module behavior](../reference/shared-module-behavior.md) | Every command, flag, field, and the behavior that several core modules share. | A user |
| Module pages, [index](../reference/modules/README.md) | Everything about one module: what it manages, its inputs and outputs, examples, how it works, its errors, where it is tested, and why it works this way. | A user, and an agent that writes configuration |
| [Write your own module](../how-to/write-your-own-module.md), [write a package module](../how-to/write-a-package-module.md), and the [module interface reference](../reference/module-interface.md) | Modules of the project's own, as a Starlark script or an executable: the steps, and every field, primitive, and protocol rule. | A user who writes a module |
| [Move from Ansible or Salt](../how-to/move-from-ansible-or-salt.md) | How Ansible and Salt habits map to MMX. | A user who knows those tools |
| [Design plan](design/README.md) | Product principles, the release plan, the record of what is built, and the remaining work. | Maintainers and contributors |
| The design documents in [`design/`](design/README.md#design-map) | The design that spans modules: workflow and CLI, configuration, execution and recovery, the rules every module follows, the design that a group of modules shares, and what comes after the first release. | Anyone who changes MMX |
| [Code map](code-map.md) | Where the source code of each part of a run lives, and a first reading path through it. | Anyone who changes MMX's code |
| [Acceptance contracts](acceptance.md) | What MMX must get right, and the tests that prove it. | A reviewer |
| [Implementation rules](implementation-rules.md) | The rules for MMX's own code. | Anyone who changes MMX's code |
| [Adding a core module](adding-core-modules.md) and [adding an official extra module](adding-official-extra-modules.md) | Where a new module's code, tests, and page go. | Anyone who adds a module |
| [Testing guide](testing.md) | The test commands and which tests run at which level. | Anyone who runs tests |
| [Validation record](validation.md) | The latest test results, and what is not verified. | Everyone |
| [Limits on each distribution](../reference/limits-on-each-distribution.md) | For each tested distribution, what is limited there and why, in one short entry per limit that links to the module page or record that owns the fact. | A user who runs a given distribution |
| [Performance record](../reference/performance.md) | The current scale measurements. | Everyone |
| [`examples/`](../../examples/) | Whole example projects, each with its own `README.md`. | A user |

## Module pages

Every module has one page. A user reads a module's page on the web or in
the repository. The command `mmx describe-module NAME` prints the named
module's page in a terminal.
The page text is built into the controller program `mmx` only. The node
program `mmx-runner` does not carry module page text.

### Where a page comes from

- **A core module** has two sources side by side:
  - `internal/modules/<module>.go` holds the code and the module's
    description: the summary, each input with its type, default, and notes,
    one example, the result fields, the dryrun behavior, and the topic that
    groups the module in the index. The retry rule comes from the module's
    flags in the registry, `internal/modules/registry.go`.
  - `internal/modules/<module>.md` holds the page's text.

  `go generate ./docs/reference/modules` joins the two core module sources into
  `docs/reference/modules/<module>.md` and writes the index `docs/reference/modules/README.md`.
- **An official extra module** keeps its page as
  `official_extra_modules/<module>/README.md`, beside its script and its
  `module_description.yml`. An official extra module's page is not generated.
- **A module of the project's own** may keep a page as
  `external_modules/<module>/README.md`. MMX does not require a page for a
  module of the project's own.

### The sections of a module page

Every module page has the following sections, in the order shown. The table
marks required sections. Other sections appear only when the sections have
content.

| Section | Holds | Core module page | Official extra module page |
|---|---|---|---|
| `# <module>`, the name in backticks | What the module manages, through which program, and when to choose it over a related module. | Required, in the `.md` file | Required |
| `## Set up` | The node tasks that install the tool and start its service, as whole YAML. | Only when the tool must be installed first | Required |
| `## Inputs` | Every input, its type, whether it is required, its default, and what it means. | Generated from the description | Required, as a table with the columns "Input", "Meaning", and "When omitted" |
| `## Outputs` | What the result holds and what later node tasks can read. | Generated from the description | Required |
| `## What a dryrun reports` | What a dryrun says, what it does not check, and what it leaves on the node. | Generated from the description | Required |
| `## Retries and time limits` | Whether the node task tries again by default, and which time limits it takes. | Generated from the description | Not used |
| `## Examples` | Whole examples. Each example says what the example does. The prose after each example says what happens, in order, and on which machine. A core module page may give each example its own `###` heading. | Required, in the `.md` file | Required, two or three examples |
| `## How it works` | What a run does step by step, what a removal removes, and how the module works with other modules. | When needed, in the `.md` file | When needed |
| `## Errors and what they mean` | The failures a user meets, and what to do about each. | When needed, in the `.md` file | Required, as a table |
| `## Tested on` | The distributions and tool versions that the module's suite runs on, and where the tool cannot be installed. The first line is `**Distributions:** ` and the list of distributions, such as `Debian 12 and 13, Ubuntu 24.04 and 26.04.`; the [module index](../reference/modules/README.md) shows that line in its column "Tested on". Only a distribution where the module's suite passes is listed. | Required, in the `.md` file | Required, with the same first line |
| `## Why it works this way` | The reasons for the module's behavior, each in one or two sentences. | When needed, in the `.md` file | Required |

### How to write a page

- Follow the [writing rules](#writing-rules) and use the terms of MMX's glossary in
  [`AGENTS.md`](../../AGENTS.md#public-terminology-and-examples).
- Write YAML examples as whole node tasks. A YAML block on a core module page
  that starts with `- ` is a list of node tasks, and a test loads it through
  MMX's validation.
- Write links relative to the file you edit. The generator rewrites the
  links for `docs/reference/modules/`.
- When the page relies on a section that several modules share, link to that
  section, and add the page to that section's list of module pages.

### How to change a page

1. Edit `internal/modules/<module>.md`, the module's description, or both.
   The description is the `Description` value in a Go file of
   `internal/modules/`, usually `<module>.go`; the first line of the
   generated page names the file.
2. From the repository root, run `go generate ./docs/reference/modules`.
   `go generate ./docs/reference/modules` names on standard error each core module
   that has no page yet.
3. Run `go test ./docs/reference/modules/... ./internal/config/`: the page tests and
   the description tests. Run the other Go tests as the
   [testing guide](testing.md) says.

## Sections that several modules share

Behavior that several modules share has one owner section:

- the [shared module behavior reference](../reference/shared-module-behavior.md)
  and the [how-to pages](../README.md) own what a user needs to know about the
  shared behavior.
- a design document owns the design and reasons for the shared behavior.

Such a section ends with a list of the module pages that rely on it:

```markdown
Module pages that rely on this section:
[`apt_package`](../reference/modules/apt_package.md), [`dnf_package`](../reference/modules/dnf_package.md).
```

Each module page listed in a shared section links back to the shared
section that lists the page. When you change a shared section,
check every page on its list. When you change a module, check every shared
section that its page links to.

## When you change something, also update

| When you change | Also update |
|---|---|
| A core module's inputs, defaults, allowed values, result fields, dryrun behavior, or retry rule | The changed core module's description in `internal/modules/<module>.go`, or for the retry rule, the changed module's flags in `internal/modules/registry.go`. Then run `go generate ./docs/reference/modules`. The tests fail until the description matches the code. |
| A core module's behavior | The changed core module's page `internal/modules/<module>.md`. Update shared sections linked from the changed module's page if the change affects the shared sections. Update the [acceptance contracts](acceptance.md) if a contract changes. |
| Behavior that several modules share | The shared section, and every page on its list. |
| A new core module | The new core module's registry line, description, and page. Run `go generate ./docs/reference/modules`. Update the status table of the [design plan](design/README.md#status) and the [acceptance contracts](acceptance.md). After the new core module's test suite runs, update the [validation record](validation.md). |
| An official extra module | The changed official extra module's `README.md`, as [adding an official extra module](adding-official-extra-modules.md) says. |
| A command or a flag of `mmx` | The command's section in the [commands reference](../reference/commands.md), and the [workflow design](design/workflow.md). |
| A term of the glossary | The glossary in [`AGENTS.md`](../../AGENTS.md#public-terminology-and-examples), then every document and page that uses the old term. |
| A heading that other documents link to | The links to the changed heading. The link test names each broken link. |
| A test result | The [validation record](validation.md). When the result adds, changes, or removes a limit on one distribution, also [limits on each distribution](../reference/limits-on-each-distribution.md). |
| A test command or the test harness | The [testing guide](testing.md). |
| A rule for MMX's own code | The [implementation rules](implementation-rules.md). |
| How MMX's documentation is organized | This map, and the lines in `AGENTS.md` and `README.md` that point to it. |

## What the tests check

The Go tests in `docs/reference/modules/generator/` (`go test ./docs/reference/modules/...`)
fail when:

- a core module has no page source `internal/modules/<module>.md`;
- a core module's page source does not start with ``# `<module>` ``;
- a core module's page source has a section that the table above does not
  list;
- a core module's page source has a section that the generator supplies;
- a core module's page source has no `## Examples`;
- a core module's page source has sections out of order;
- a YAML example on a core module page or an official extra
  module's `README.md` fails MMX's validation.
  The test selects YAML blocks whose first nonempty, noncomment line starts
  with `- `. The test loads each selected block as the node tasks of one
  fleet stage;
- a generated file in `docs/reference/modules/` differs from what `go generate
  ./docs/reference/modules` writes now;
- a relative link in a Markdown file of the repository leads to a file or a
  heading that does not exist;
- a module page links to a section of a Markdown file outside
  `internal/modules/` and `docs/reference/modules/`, and the linked section does not
  link to the module's page in `docs/reference/modules/`.
  The linked section ends at the next heading whose level equals or is
  higher than the linked section's heading level;
- the node program `mmx-runner` links the package that holds the page text.

The [module description tests](implementation-rules.md#module-descriptions)
in `internal/config/` also check each core module's description against its
code, and that it names one of the topics in `modules.CoreModuleTopics`.
