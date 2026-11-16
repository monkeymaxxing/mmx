# Contributing to MMX

Bug reports, ideas, and pull requests are welcome. `CONTRIBUTING.md` explains
how bug reports, ideas, and pull requests become changes to MMX.

## Reporting a bug or asking for a feature

Open an issue on GitHub. For a bug, include:

- the output of `mmx version` on the controller machine, the machine where
  you run `mmx` commands;
- the controller machine's operating system, and each node's Linux
  distribution and version;
- the smallest playbook, inventory, and role files that show the problem;
- what you expected, and what MMX did, with the output of the command and
  `--json` added to it.

For a feature, start with the problem: what you are trying to do, and how you
do it today. The [design plan](docs/changing-mmx/design/README.md) shows what MMX intends
to do and what remains. A proposal is compared with the design plan's
[product principles](docs/changing-mmx/design/README.md#product-principles).

## Reporting a security problem

Do not open an issue for a security problem: everyone can read issues. MMX
runs as root on your servers. Report the problem privately with **Report a
vulnerability** on the repository's **Security** tab on GitHub. Only MMX's
maintainers see the report and reply in the private vulnerability report.

## A pull request is a proposal

MMX's maintainers write every change that lands in MMX. Maintainers do not
merge code from outside the project. MMX runs as root on your servers, so the
origin of every line is known. No code arrives from a source whose
license, authorship, or tools the project cannot check.

So a pull request is read as a proposal: a precise description of a change,
with code that shows it. When a maintainer accepts a proposal, the maintainer
writes the change, with its tests and documentation, and credits you in the
commit message:

```text
Proposed by @your-github-name in #123
```

The pull request is then closed with a link to the maintainer's commit.
A proposal that is not accepted is closed with the reason.

A proposal is easiest to accept when it:

- changes one thing, and says why;
- includes a test that fails without the change, where one is possible;
- uses the names and terms of the [glossary](AGENTS.md#public-terminology-and-examples)
  and follows the [rules for names that users read](AGENTS.md#names-that-users-read).

## Where the rules for changes live

| Document | What the document gives you |
|---|---|
| [`AGENTS.md`](AGENTS.md) | MMX's glossary and the rules for names, for people and coding agents alike |
| [Documentation map](docs/changing-mmx/documentation-map.md) | Which document owns each fact, and what else to update when something changes |
| [Implementation rules](docs/changing-mmx/implementation-rules.md) | The rules for MMX's own code |
| [Testing guide](docs/changing-mmx/testing.md) | How to run the tests, and which tests a change needs |

## License

MMX is licensed under the [Apache License 2.0](LICENSE). Submissions in an
issue or a pull request are offered under the Apache License 2.0, as
section 5 of the license says.
