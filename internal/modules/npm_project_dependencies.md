# `npm_project_dependencies`

`npm_project_dependencies` installs what a Node.js application's
`package-lock.json` lists into the application's `node_modules`, with
`npm ci`, as the application's account. The task names the directory on the
node that holds `package.json` and the lock file, usually a release
directory or a checkout. Command-line tools for the whole node are
[`npm_global_package`](npm_global_package.md); an application that uses
Yarn is [`yarn_project_dependencies`](yarn_project_dependencies.md).

The five lock file modules work the same way: the lock file is never
rewritten, and a record in the lock file directory makes a rerun unchanged.
That is in
[install what an application's lock file lists](../../docs/how-to/install-languages.md#steps)
and in the design's
[what the lock file modules share](../../docs/changing-mmx/design/modules-packages.md#what-the-lock-file-modules-share).
The rules of every language and tool installer, such as
`run_as_user_account`, `npm_program_path`, and `env`, are in
[what the installers share](../../docs/how-to/install-languages.md#what-the-installer-modules-share).

## Examples

### Install a release's packages without development dependencies

```yaml
- npm_project_dependencies:
    lock_file_dir: /srv/myapp/releases/v1.4.2
    run_as_user_account: myapp
    include_development_dependencies: false
```

`include_development_dependencies` is required. `false` leaves out the
packages that `package.json` lists under `devDependencies`. `true` installs
the `devDependencies` packages, which a build step on the node often needs.
On the node, MMX runs `npm ci` in `/srv/myapp/releases/v1.4.2` as the account
`myapp`. `npm ci` deletes `node_modules` and installs exactly what
`package-lock.json` lists. After a successful installation, MMX writes the
record `.mmx-npm-project-dependencies` in the same directory; the next run is
unchanged while the record matches and `node_modules` exists.

## How it works

`npm ci` reads `package-lock.json`, or `npm-shrinkwrap.json`, and fails when
the lock file is missing or does not fit `package.json`.

## Tested on

**Distributions:** Debian 12 and 13, Ubuntu 24.04 and 26.04, Rocky Linux 9, AlmaLinux 10.

The suite `language_installers.py` runs on Debian 12 and 13, Ubuntu 24.04 and 26.04, Rocky Linux 9, and AlmaLinux 10, with
Node.js 22 from nodejs.org unpacked under `/opt`, and installs against a
lock file that the suite makes on the node, as a repository would carry it.

## Why it works this way

- **A changed manifest reaches the installer's lock-file check.** The shared
  [installation record](../../docs/changing-mmx/design/modules-packages.md#what-the-lock-file-modules-share)
  prevents an old successful installation from hiding a manifest change.
- **A written program path is absolute.** Leaving the path out uses the
  installer's normal program selection, as the
  [shared inputs](../../docs/how-to/install-languages.md#what-the-installer-modules-share) explain.
