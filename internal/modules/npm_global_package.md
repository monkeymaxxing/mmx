# `npm_global_package`

`npm_global_package` installs Node.js command-line tools for the whole node
with `npm install --global`, such as `pm2` or Yarn 1. It runs the `npm`
program on the node. An application's own packages, which its
`package-lock.json` lists, are
[`npm_project_dependencies`](npm_project_dependencies.md).

What `npm_global_package` shares with the other language and tool
installers, such as `run_as_user_account`, `npm_program_path`, `env`, and
how a change becomes a tracked input, is in
[what the installers share](../../docs/how-to/install-languages.md#what-the-installer-modules-share)
and the design's
[shared installer behavior](../../docs/changing-mmx/design/modules-packages.md#what-these-modules-share).
`run_as_user_account` is required, including when the installer runs as root.

## Examples

### Install one version of a tool

```yaml
- npm_global_package:
    packages:
      - name: pm2
        version: "5.4.3"
    run_as_user_account: root
```

Each `packages` entry states an exact version or a version keyword.
The task ensures pm2 5.4.3 is installed.

### Use the npm of a Node.js under /opt

```yaml
- npm_global_package:
    packages:
      - name: yarn
        version: "1.22.22"
    npm_program_path: /opt/node/bin/npm
    run_as_user_account: root
```

`npm_program_path` names the `npm` when it is not the one on the standard
system path, such as one from Node.js unpacked under `/opt`. Its directory
comes first in `PATH`, so `npm` finds the `node` beside it.

### Keep tools at the newest version, or uninstall them

```yaml
- npm_global_package:
    packages:
      - name: pm2
        version: newest_on_every_run
    run_as_user_account: root

- npm_global_package:
    packages:
      - name: yarn
    state: absent
    run_as_user_account: root
```

What the `version` keywords do is in [Package versions](../../docs/reference/shared-module-behavior.md#package-versions).
`state: absent` uninstalls each named package; removal entries take `name` only.

## How it works

MMX reads what is installed from `npm ls --global --json` and runs npm only
for what differs. All changed entries share one install command, each with
its own version. A dryrun lists `{name, from, to}`; for
`newest_on_every_run` it asks
`npm outdated --global`.

Tracked input: the installed versions of the named packages.

## Tested on

**Distributions:** Debian 12 and 13, Ubuntu 24.04 and 26.04, Rocky Linux 9, AlmaLinux 10.

The suite `language_installers.py` runs on Debian 12 and 13, Ubuntu 24.04 and 26.04, Rocky Linux 9, and AlmaLinux 10, with
Node.js 22 from nodejs.org unpacked under `/opt`, and covers
`npm_global_package` by installing Yarn 1 (1.22.22) for the whole node.

## Why it works this way

- **The task states each package's version choice.** Presence alone does not
  say whether future runs should upgrade. See the shared design's
  [required-settings rule](../../docs/changing-mmx/design/modules.md#a-setting-with-no-safe-guess-is-required).
- **The task names the installer's account.** Package installers can run
  package code, so running as root must be an explicit choice.
