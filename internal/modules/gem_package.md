# `gem_package`

`gem_package` installs Ruby gems on a node with the `gem` program of the
Ruby on the node. Use it for a gem that the node needs by itself, such as
Bundler. An application's own gems, which its `Gemfile.lock` lists, are
[`bundler_project_dependencies`](bundler_project_dependencies.md).

What `gem_package` shares with the other language and tool installers,
such as `run_as_user_account`, `gem_program_path`, `env`, and how a change
becomes a tracked input, is in
[what the installers share](../../docs/how-to/install-languages.md#what-the-installer-modules-share)
and in the design's
[what these modules share](../../docs/changing-mmx/design/modules-packages.md#what-these-modules-share).
`run_as_user_account` is required, including when the installer runs as root.

## Set up

Ruby and `gem` come from the distribution's packages:

```yaml
- when: {equals: ["{{ .node.package_manager }}", apt]}
  apt_package: {packages: [{name: ruby, version: newest_when_first_installed}]}
- when: {equals: ["{{ .node.package_manager }}", dnf]}
  dnf_package: {packages: [{name: ruby, version: newest_when_first_installed}, {name: rubygems, version: newest_when_first_installed}]}
```

## Examples

### Install one version of a gem

```yaml
- gem_package:
    packages:
      - name: bundler
        version: "2.5.23"
    run_as_user_account: root
```

Each `packages` entry states an exact version or a version keyword.
RubyGems, the package manager that provides `gem` on the node, keeps
several versions of a gem side by side. The example's `gem_package` node
task ensures Bundler 2.5.23 is installed without removing other installed
Bundler versions.

### Keep gems at the newest version, or remove them

```yaml
- gem_package:
    packages:
      - name: rake
        version: newest_on_every_run
    run_as_user_account: root

- gem_package:
    packages:
      - name: bundler
        version: "2.5.23"
    state: absent
    run_as_user_account: root
```

What the `version` keywords do is in [Package versions](../../docs/reference/shared-module-behavior.md#package-versions).
`state: absent` removes every installed version of each named gem, or only
the exact version written in the gem's `packages` entry.

## How it works

MMX asks `gem list` what is installed and runs `gem install` or
`gem uninstall` only for what differs. A dryrun lists the gems MMX would
install or remove; for `newest_on_every_run` MMX asks `gem outdated`.
MMX passes every gem needing installation or an upgrade to a single
`gem install` command, attaching each exact version to the corresponding
gem name.

Tracked input: the installed versions of the named gems.

## Tested on

**Distributions:** Debian 12 and 13, Ubuntu 24.04 and 26.04, Rocky Linux 9, AlmaLinux 10.

The suite `language_installers.py` runs on Debian 12 and 13, Ubuntu 24.04 and 26.04, Rocky Linux 9, and AlmaLinux 10, with
Ruby from the distribution's packages, and covers `gem_package` with an
exact version.

## Why it works this way

- **The task states each package's version choice.** Presence alone does not
  say whether future runs should upgrade. See the shared design's
  [required-settings rule](../../docs/changing-mmx/design/modules.md#a-setting-with-no-safe-guess-is-required).
- **The task names the installer's account.** Package installers can run
  package code, so running as root must be an explicit choice.
