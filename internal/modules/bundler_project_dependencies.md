# `bundler_project_dependencies`

`bundler_project_dependencies` installs what a Ruby application's
`Gemfile.lock` lists, with `bundle install` in Bundler's deployment mode, as
the application's account. The task names the directory on the node that
holds the `Gemfile` and `Gemfile.lock`, usually a release directory or a
checkout. A single gem that the node needs by itself, such as Bundler, is
[`gem_package`](gem_package.md).

The five lock file modules work the same way: the lock file is never
rewritten, and a record in the lock file directory makes a rerun unchanged.
That is in
[install what an application's lock file lists](../../docs/how-to/install-languages.md#steps)
and in the design's
[what the lock file modules share](../../docs/changing-mmx/design/modules-packages.md#what-the-lock-file-modules-share).
The rules of every language and tool installer, such as
`run_as_user_account`, `bundle_program_path`, and `env`, are in
[what the installers share](../../docs/how-to/install-languages.md#what-the-installer-modules-share).

## Set up

Ruby and Bundler come from the distribution's packages:

```yaml
- when: {equals: ["{{ .node.package_manager }}", apt]}
  apt_package: {packages: [{name: ruby, version: newest_when_first_installed}, {name: ruby-bundler, version: newest_when_first_installed}]}
- when: {equals: ["{{ .node.package_manager }}", dnf]}
  dnf_package: {packages: [{name: ruby, version: newest_when_first_installed}, {name: rubygems, version: newest_when_first_installed}, {name: rubygem-bundler, version: newest_when_first_installed}]}
```

## Examples

### Install a release's gems without the development and test groups

Gemfile groups name sets of gems for purposes such as development and
testing.

```yaml
- bundler_project_dependencies:
    lock_file_dir: /srv/myapp/releases/v1.4.2
    run_as_user_account: myapp
    without_gemfile_groups: [development, test]
```

`without_gemfile_groups` is required: the `Gemfile` groups to leave out,
and `[]` installs every group. On the node, MMX runs `bundle install` in
Bundler's deployment mode in `/srv/myapp/releases/v1.4.2`, as the account
`myapp`. It installs exactly what `Gemfile.lock` lists into `vendor/bundle`
inside the lock file directory, and fails when `Gemfile.lock` does not fit
the `Gemfile`. After a successful installation, MMX writes the record
`.mmx-bundler-project-dependencies` in the same directory.

The application must run with the same settings; the task's result lists
them in `value.bundler_settings`.

### Share one set of gems between releases

`gems_dir` changes where the gems go, by default `vendor/bundle` in
the lock file directory. A `gems_dir` outside the lock file directory
lets several releases share one set of gems, at the price that installing
for the next release changes a directory the running release reads.

## How it works

The settings reach Bundler through its environment variables
(`BUNDLE_DEPLOYMENT`, `BUNDLE_PATH`, `BUNDLE_WITHOUT`), so MMX writes no
Bundler configuration file. Bundler before 2.3, such as Rocky Linux 9's 2.2,
writes `BUNDLE_WITHOUT` into `.bundle/config` in the lock file directory
itself; the environment variables still take precedence over that file.

## Tested on

**Distributions:** Debian 12 and 13, Ubuntu 24.04 and 26.04, Rocky Linux 9, AlmaLinux 10.

The suite `language_installers.py` runs on Debian 12 and 13, Ubuntu 24.04 and 26.04, Rocky Linux 9, and AlmaLinux 10, with
Ruby and Bundler from the distribution's packages, and installs against a
lock file that the suite makes on the node, as a repository would carry it.

## Why it works this way

- **A changed manifest reaches the installer's lock-file check.** The shared
  [installation record](../../docs/changing-mmx/design/modules-packages.md#what-the-lock-file-modules-share)
  prevents an old successful installation from hiding a manifest change.
- **A written program path is absolute.** Leaving the path out uses the
  installer's normal program selection, as the
  [shared inputs](../../docs/how-to/install-languages.md#what-the-installer-modules-share) explain.
