# `yarn_project_dependencies`

`yarn_project_dependencies` installs what a Node.js application's
`yarn.lock` lists, with the Yarn install that refuses to change `yarn.lock`,
as the application's account. The task names the directory on the node that
holds `package.json` and `yarn.lock`, usually a release directory or a
checkout. An application that uses npm is
[`npm_project_dependencies`](npm_project_dependencies.md).

The five lock file modules work the same way: the lock file is never
rewritten, and a record in the lock file directory makes a rerun unchanged.
That is in
[install what an application's lock file lists](../../docs/how-to/install-languages.md#steps)
and in the design's
[what the lock file modules share](../../docs/changing-mmx/design/modules-packages.md#what-the-lock-file-modules-share).
The rules of every language and tool installer, such as
`run_as_user_account`, `yarn_program_path`, and `env`, are in
[what the installers share](../../docs/how-to/install-languages.md#what-the-installer-modules-share).

## Examples

### Install a release's packages without development dependencies

```yaml
- yarn_project_dependencies:
    lock_file_dir: /srv/myapp/releases/v1.4.2
    run_as_user_account: myapp
    include_development_dependencies: false
```

`include_development_dependencies` is required, as for npm: `false` leaves
out `devDependencies`, and `true` installs them. On the node, MMX reads the
Yarn version and runs the install that refuses to change `yarn.lock`, in
`/srv/myapp/releases/v1.4.2` as the account `myapp`. After a successful
installation, MMX writes the record `.mmx-yarn-project-dependencies` in the
directory `/srv/myapp/releases/v1.4.2`.

## How it works

| Yarn on the node | What MMX runs |
|---|---|
| Yarn 1 | `yarn install --frozen-lockfile` |
| Yarn 2 and newer | `yarn install --immutable` |
| Yarn 2 and newer, with `include_development_dependencies: false` | `yarn workspaces focus --all --production`, with `YARN_ENABLE_IMMUTABLE_INSTALLS` set so that it cannot change `yarn.lock` either |

## Tested on

**Distributions:** Debian 12 and 13, Ubuntu 24.04 and 26.04, Rocky Linux 9, AlmaLinux 10.

The suite `language_installers.py` runs on Debian 12 and 13, Ubuntu 24.04 and 26.04, Rocky Linux 9, and AlmaLinux 10 with
Yarn 1 (1.22.22), which `npm_global_package` installs into Node.js 22 from
nodejs.org under `/opt`. Not verified on this build: Yarn 2 and newer
(`--immutable`, `workspaces focus`); Go tests cover the command choice.

## Why it works this way

- **A changed manifest reaches the installer's lock-file check.** The shared
  [installation record](../../docs/changing-mmx/design/modules-packages.md#what-the-lock-file-modules-share)
  prevents an old successful installation from hiding a manifest change.
- **A written program path is absolute.** Leaving the path out uses the
  installer's normal program selection, as the
  [shared inputs](../../docs/how-to/install-languages.md#what-the-installer-modules-share) explain.
