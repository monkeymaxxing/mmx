# `locale`

A locale is the set of language settings that programs use for messages,
dates, and sorting, such as `en_US.UTF-8`. `locale` makes one locale
available on the node and says whether it is the system default. Write one
node task for each locale the node needs.

## Set up

On the Red Hat family, locales come in packages, one for each language:
`glibc-langpack-<language>`, such as `glibc-langpack-de` for `de_DE.UTF-8`.
Install the package in an earlier node task:

```yaml
- dnf_package:
    packages:
      - {name: glibc-langpack-de, version: newest_when_first_installed}

- locale:
    name: de_DE.UTF-8
    use_as_system_default: true
```

On Debian and Ubuntu, `locale-gen` and `update-locale` come from the package
`locales`, which server images usually have. A missing program fails the
node task, and the reason names its package.

## Examples

### Make a locale the system default

```yaml
- locale:
    name: en_US.UTF-8
    use_as_system_default: true
```

On the node, this node task makes the locale `en_US.UTF-8` available and
makes it the system default. MMX inspects the node first and reports
unchanged when the node already matches.

## How it works

- `name` is a locale name, such as `en_US.UTF-8` or `de_DE.UTF-8`.
  `use_as_system_default` is required. With `true`, MMX sets `LANG` of the
  system locale to the locale named by `name`.
  With `false`, MMX leaves the system default alone.
- **Debian and Ubuntu** make locales from the list in `/etc/locale.gen`.
  When the locale is missing, MMX enables its line there and runs
  `locale-gen`. When the file has no line for it, MMX adds the line from
  `/usr/share/i18n/SUPPORTED`. A name that neither file lists fails before
  any change, so a misspelled name never reaches the node.
- **The Red Hat family** installs locales in packages. When the locale is
  missing, the node task fails and names the package to install first with
  [`dnf_package`](dnf_package.md); see [Set up](#set-up).
- With `use_as_system_default: true`, MMX sets `LANG` with `update-locale`
  on Debian and Ubuntu, which writes `/etc/default/locale`, and with
  `localectl` on the Red Hat family. The other settings of the system
  locale, such as `LC_TIME`, stay as they are.
- Results and dryrun show what changes in `.this_node_task.value`:
  `made_available: true` when the locale was made, and
  `system_default_before` and `system_default_after` when `LANG` changed. A
  run that fails lists only what it did before the failure, so a node task in
  `on_failure` can read what happened.
- Running services read the locale when they start. A change does not
  restart them; restart a service that must follow the change on purpose,
  for example from a restart playbook.

## Errors and what they mean

| The reason holds | What the error means, and what to do |
|---|---|
| `locale de_DE.UTF-8 is not installed; install the package glibc-langpack-de first with dnf_package` | On the Red Hat family, the locale's package is missing. Install it in an earlier node task; see [Set up](#set-up). A dryrun reports the node task not checked, because an earlier node task may install the package. |
| `this node cannot make locale ...: neither /etc/locale.gen nor /usr/share/i18n/SUPPORTED lists it; check the spelling, such as en_US.UTF-8` | On Debian and Ubuntu, the name is not a locale that the node can make. Check the spelling. Nothing was changed. |
| `locale-gen is not installed; install the package ... first` | A program that the module runs is missing: `localectl`, `locale-gen`, or `update-locale`. The reason names the package. Install it in an earlier node task. A dryrun reports the node task not checked. |

## Tested on

**Distributions:** Debian 12 and 13, Ubuntu 24.04 and 26.04, Rocky Linux 9, AlmaLinux 10.

The suite `tests/integration/node_settings.py` runs on Debian 12 and 13, Ubuntu 24.04 and 26.04, Rocky Linux 9, and AlmaLinux 10, in containers on the first four and in virtual machines on the last two. The
[validation record](../../docs/changing-mmx/validation.md)
has the latest results.

## Why it works this way

- **`update-locale` on Debian and Ubuntu.** Debian 13 does not let
  `localectl` change the system locale, not even as root: Debian keeps it
  in `/etc/default/locale`, which `update-locale` from the `locales` package
  writes. Debian 13's restriction on `localectl` is why MMX uses
  `update-locale` on every Debian and Ubuntu node.
