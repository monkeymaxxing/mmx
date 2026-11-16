# `composer_project_dependencies`

`composer_project_dependencies` installs what a PHP application's
`composer.lock` lists into the application's `vendor` directory, with
`composer install` and without prompts, as the application's account. The
task names the directory on the node that holds `composer.json` and
`composer.lock`, usually a release directory or a checkout.

The five lock file modules work the same way: the lock file is never
rewritten, and a record in the lock file directory makes a rerun unchanged.
That is in
[install what an application's lock file lists](../../docs/how-to/install-languages.md#steps)
and in the design's
[what the lock file modules share](../../docs/changing-mmx/design/modules-packages.md#what-the-lock-file-modules-share).
The rules of every language and tool installer, such as
`run_as_user_account`, `composer_program_path`, and `env`, are in
[what the installers share](../../docs/how-to/install-languages.md#what-the-installer-modules-share).

## Set up

PHP comes from the distribution's packages, and Composer from a pinned
download of its publisher:

```yaml
- when: {equals: ["{{ .node.package_manager }}", apt]}
  apt_package: {packages: [{name: php-cli, version: newest_when_first_installed}]}
- when: {equals: ["{{ .node.package_manager }}", dnf]}
  dnf_package: {packages: [{name: php-cli, version: newest_when_first_installed}]}

- download:
    url: https://getcomposer.org/download/2.8.4/composer.phar
    sha256: "c4c4e2e1beab0ea04e0bd042a5dbba9feda1fbf5eda0d36203958edd343c0a8a"
    path: /usr/local/bin/composer
    permissions_mode: "0755"
```

## Examples

### Install a release's packages without development dependencies

```yaml
- composer_project_dependencies:
    lock_file_dir: /srv/myapp/releases/v1.4.2
    run_as_user_account: myapp
    include_development_dependencies: false
```

`include_development_dependencies` is required: `false` leaves out what
`composer.json` lists under `require-dev`. On the node, MMX runs
`composer install` without prompts in `/srv/myapp/releases/v1.4.2`, as the
account `myapp`. It installs exactly what `composer.lock` lists into
`vendor`. After a successful installation, MMX writes the record
`.mmx-composer-project-dependencies` in the same directory.

## Tested on

**Distributions:** Debian 12 and 13, Ubuntu 24.04 and 26.04, Rocky Linux 9, AlmaLinux 10.

The suite `language_installers.py` runs on Debian 12 and 13, Ubuntu 24.04 and 26.04, Rocky Linux 9, and AlmaLinux 10, with
PHP from the distribution's packages and Composer 2.8.4 from a pinned
download. Its Composer project takes its packages from a path repository, so
it needs no download.

## Why it works this way

- **A changed manifest reaches the installer's lock-file check.** The shared
  [installation record](../../docs/changing-mmx/design/modules-packages.md#what-the-lock-file-modules-share)
  prevents an old successful installation from hiding a manifest change.
- **A written program path is absolute.** Leaving the path out uses the
  installer's normal program selection, as the
  [shared inputs](../../docs/how-to/install-languages.md#what-the-installer-modules-share) explain.
