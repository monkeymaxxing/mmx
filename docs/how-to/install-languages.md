# Install languages and application dependencies

**Who acts:** you, or your coding agent, in your MMX project on your
controller machine: the computer where you run `mmx`.

**When to use this page:** an application on your servers needs a
language, such as Node.js, Python, or Ruby, and the packages that the
application lists in its own files, such as `package-lock.json` or
`requirements.txt`.

**What this page does:** you install Node.js and a small Node.js
application, `shop-api`, with the packages that its lock file lists, and
restart the application when the installed packages change. Later parts install Python
packages into a virtual environment, and install a language version that
no distribution package offers, with a version manager such as nvm,
rbenv, or uv.

**Terms:**

- A **node** is a server that MMX manages. A **playbook** in `playbooks/`
  lists **fleet stages**. Each fleet stage picks nodes, and each of those
  nodes runs the fleet stage's **node tasks**, in order. A node task names
  one **module**, the code that does the work, such as `apt_package`.
- A **node task block** groups a change with what follows it: `changes`
  makes the change, `activate` puts the change into use, and `verify` checks it.
  A `systemd_service` node task in `activate` restarts its service when
  something in `changes` changed.
- A **lock file** lists the exact version of each package that an
  application uses, such as `package-lock.json` for npm or `Gemfile.lock`
  for Bundler. The developer creates the lock file on their own computer, and the
  lock file goes into the application's repository.
- A **version manager** is a tool that installs several versions of one
  language side by side, such as nvm for Node.js or rbenv for Ruby.

| What the application needs | Where |
| --- | --- |
| The packages that its lock file lists, with the language from the distribution's packages | [Steps](#steps) |
| The packages of a `requirements.txt`, in a Python virtual environment | [Python packages in a virtual environment](#python-packages-in-a-virtual-environment) |
| A language version that no package offers | [A language version that no package offers](#a-language-version-that-no-package-offers) |
| One named package, such as a command-line tool from pip, RubyGems, npm, or the Snap Store | [One named package](#one-named-package) |

## Before you start

- You have an MMX project, as in [Get started](../get-started.md), with
  the group `web_servers` in `inventories/example.yml`: the servers
  `web01` at `203.0.113.11`, which runs Debian 13, and `web02` at
  `203.0.113.12`, which runs Ubuntu 26.04.
- The examples install with `apt_package`, for Debian and Ubuntu. On
  Fedora, Rocky Linux, or AlmaLinux, use `dnf_package`, and check the
  package names there. [Reuse work with roles](reuse-work-with-roles.md#one-role-for-several-distributions)
  shows one role that works with apt and with dnf.
- The application's lock file is in the application's files. Without a
  lock file, the module refuses to install, as
  [If something is different](#if-something-is-different) shows.

## Steps

1. **Put the application into the project.** Create
   `files/shop-api/` in the project, with three files. `server.js`
   answers on port 3000 and says which Node.js version runs
   `server.js`:

   ```js
   // files/shop-api/server.js
   const express = require("express");

   const app = express();
   app.get("/", (request, response) => {
     response.send(`Shop API on Node.js ${process.version}\n`);
   });
   app.listen(3000, "127.0.0.1");
   ```

   `package.json` lists the one package that `server.js` uses:

   ```json
   {
     "name": "shop-api",
     "version": "1.0.0",
     "private": true,
     "main": "server.js",
     "dependencies": {
       "express": "^5.1.0"
     }
   }
   ```

   On your computer, with Node.js and npm installed, create
   `package-lock.json` beside `package.json`:

   ```sh
   cd files/shop-api
   npm install --package-lock-only
   ```

   Reason: this page copies the application from the project, so the
   example needs no Git server. An application in a Git repository
   arrives with a `git` node task instead. The example
   [`deploy-from-git-with-release-directories`](../../examples/deploy-from-git-with-release-directories/README.md)
   shows that way.

   **Check:** `ls files/shop-api` lists `package-lock.json`,
   `package.json`, and `server.js`.

2. **Install Node.js, and add an account for the application.** Create
   `playbooks/set_up_shop_api.yml`:

   ```yaml
   # playbooks/set_up_shop_api.yml
   fleet_stages:
     - id: set_up_shop_api
       nodes: web_servers
       node_tasks:
         - id: install_nodejs
           apt_package:
             packages:
               - {name: nodejs, version: newest_when_first_installed}
               - {name: npm, version: newest_when_first_installed}

         - id: create_shop_group
           group:
             name: shop
             system: true

         - id: create_shop_account
           user:
             name: shop
             primary_group: shop
             system: true
             home: /srv/shop-api
             create_home_directory: false
             shell: /usr/sbin/nologin
   ```

   Reason: npm can run code from the packages that npm installs. So the
   application's own account runs npm, not root, and the account cannot
   log in.

   **Check:** `mmx validate set_up_shop_api` prints `Valid: mmx_example`.

3. **Add the application as a node task block.** Add this node task block below
   `create_shop_account`, at the same indent:

   ```yaml
         - task_block:
             id: shop_api
             changes:
               - id: copy_application
                 directory:
                   path: /srv/shop-api
                   source: files/shop-api      # a directory in the project
                   owner: shop
                   group: shop
               - id: install_dependencies
                 npm_project_dependencies:
                   lock_file_dir: /srv/shop-api
                   run_as_user_account: shop
                   include_development_dependencies: false
               - id: write_service_unit
                 systemd_unit:
                   name: shop-api.service
                   content: |
                     [Unit]
                     Description=Shop API

                     [Service]
                     User=shop
                     WorkingDirectory=/srv/shop-api
                     ExecStart=/usr/bin/node /srv/shop-api/server.js

                     [Install]
                     WantedBy=multi-user.target
             activate:
               - id: restart_shop_api
                 systemd_service:
                   name: shop-api
                   start_at_boot: true
                   state: running
             verify:
               - id: check_shop_api
                 wait_for:
                   url_responds: http://127.0.0.1:3000/
                   response_body_contains: Shop API
   ```

   `npm_project_dependencies` runs `npm ci` in `lock_file_dir` as the
   account `shop`. `npm ci` installs exactly what `package-lock.json`
   lists into `/srv/shop-api/node_modules`, and never changes the lock
   file. `include_development_dependencies: false` leaves out the
   packages under `devDependencies` in `package.json`. Because each node
   task in `changes` counts, `restart_shop_api` restarts the application
   when the files, the installed packages, or the unit changed.

   **Check:** `mmx validate set_up_shop_api` prints `Valid: mmx_example`.
   The whole file now reads:

   ```yaml
   # playbooks/set_up_shop_api.yml
   fleet_stages:
     - id: set_up_shop_api
       nodes: web_servers
       node_tasks:
         - id: install_nodejs
           apt_package:
             packages:
               - {name: nodejs, version: newest_when_first_installed}
               - {name: npm, version: newest_when_first_installed}

         - id: create_shop_group
           group:
             name: shop
             system: true

         - id: create_shop_account
           user:
             name: shop
             primary_group: shop
             system: true
             home: /srv/shop-api
             create_home_directory: false
             shell: /usr/sbin/nologin

         - task_block:
             id: shop_api
             changes:
               - id: copy_application
                 directory:
                   path: /srv/shop-api
                   source: files/shop-api
                   owner: shop
                   group: shop
               - id: install_dependencies
                 npm_project_dependencies:
                   lock_file_dir: /srv/shop-api
                   run_as_user_account: shop
                   include_development_dependencies: false
               - id: write_service_unit
                 systemd_unit:
                   name: shop-api.service
                   content: |
                     [Unit]
                     Description=Shop API

                     [Service]
                     User=shop
                     WorkingDirectory=/srv/shop-api
                     ExecStart=/usr/bin/node /srv/shop-api/server.js

                     [Install]
                     WantedBy=multi-user.target
             activate:
               - id: restart_shop_api
                 systemd_service:
                   name: shop-api
                   start_at_boot: true
                   state: running
             verify:
               - id: check_shop_api
                 wait_for:
                   url_responds: http://127.0.0.1:3000/
                   response_body_contains: Shop API
   ```

4. **Preview the playbook.**

   ```sh
   mmx dryrun set_up_shop_api
   echo "exit code $?"
   ```

   **Check:** the exit code is 3, and the lines for the installation say
   `not_checked`, because Node.js, the account, and the application's
   directory do not exist yet. Part of the output for `web01`:

   ```text
   set_up_shop_api / web01 / create_shop_account: not_checked, unknown — group shop does not exist yet. The earlier node task install_nodejs would change this node first and may provide what is missing; if the run then succeeds, change nothing.
   ...
   set_up_shop_api / web01 / shop_api/changes/install_dependencies: not_checked, unknown — the lock file directory /srv/shop-api does not exist yet; an earlier task, such as a git checkout, may create it. The earlier node task install_nodejs would change this node first and may provide what is missing; if the run then succeeds, change nothing.
   ```

   A dryrun installs nothing, so the dryrun cannot check what depends on an
   earlier node task. [Preview and run changes](preview-and-run-changes.md#what-the-results-mean)
   explains `not_checked`.

5. **Run the playbook.**

   ```sh
   mmx run set_up_shop_api
   ```

   **Check:** each node task says `succeeded`, and the run ends with
   `Run ...: succeeded` and exit code 0. On a first run, the lines for
   the installation and the restart read:

   ```text
   set_up_shop_api / web01 / shop_api/changes/install_dependencies: succeeded, changed — installed: there is no record of an earlier installation
     {"lock_file":"package-lock.json"}
   ...
   set_up_shop_api / web01 / shop_api/activate/restart_shop_api: succeeded, changed — service is not running
   set_up_shop_api / web01 / shop_api/verify/check_shop_api: succeeded, unchanged
   ```

   `service is not running` means that MMX started the application for the
   first time. On Debian 13, installing `npm` brings several hundred
   small packages, so the first run took about twelve minutes. Ask
   each server for the page:

   ```sh
   mmx run-command --nodes web_servers -- curl -s http://127.0.0.1:3000/
   ```

   ```text
   run-command / web01 / command: succeeded, changed
     Shop API on Node.js v20.19.2
   run-command / web02 / command: succeeded, changed
     Shop API on Node.js v22.22.1
   ```

   Each server runs the Node.js of its own distribution: version 20 on
   Debian 13, and version 22 on Ubuntu 26.04.

6. **Run the playbook again.**

   ```sh
   mmx run set_up_shop_api
   ```

   **Check:** each node task says `unchanged`, and `restart_shop_api`
   restarts nothing. The installation is unchanged because nothing that
   the installation depends on changed, as
   [When a lock file module installs again](#when-a-lock-file-module-installs-again)
   explains.

7. **Change the application, and run once more.** A new release of the
   application brings a new `package.json` or `package-lock.json`. For
   this example, change `"version": "1.0.0"` to `"version": "1.0.1"` in
   `files/shop-api/package.json`, then run:

   ```sh
   mmx run set_up_shop_api
   ```

   **Check:** npm installs again, and the application restarts once:

   ```text
   set_up_shop_api / web01 / shop_api/changes/install_dependencies: succeeded, changed — installed: package.json changed since the last installation
   ...
   set_up_shop_api / web01 / shop_api/activate/restart_shop_api: succeeded, changed — installed software version changed
   ```

   If the run stops between the installation and the restart, the next
   run still restarts the application, as
   [A run that stopped before the reload](change-a-configuration-and-reload-its-service.md#a-run-that-stopped-before-the-reload)
   explains.

Module pages that rely on this section: [`bundler_project_dependencies`](../reference/modules/bundler_project_dependencies.md), [`composer_project_dependencies`](../reference/modules/composer_project_dependencies.md), [`npm_project_dependencies`](../reference/modules/npm_project_dependencies.md), [`uv_project_dependencies`](../reference/modules/uv_project_dependencies.md), [`yarn_project_dependencies`](../reference/modules/yarn_project_dependencies.md).

## What the installer modules share

Nine modules install software with a language's or a tool's own
installer. Each runs that installer program on the node, so the program
must be on the node: an earlier node task installs the program when the
distribution does not, as `install_nodejs` does above.

- **Modules that install what a lock file lists:**
  [`npm_project_dependencies`](../reference/modules/npm_project_dependencies.md)
  (`package-lock.json`),
  [`yarn_project_dependencies`](../reference/modules/yarn_project_dependencies.md)
  (`yarn.lock`),
  [`bundler_project_dependencies`](../reference/modules/bundler_project_dependencies.md)
  (`Gemfile.lock`),
  [`composer_project_dependencies`](../reference/modules/composer_project_dependencies.md)
  (`composer.lock`), and
  [`uv_project_dependencies`](../reference/modules/uv_project_dependencies.md)
  (`uv.lock`). The node task names the application's directory, and the
  lock file in that directory says what to install.
- **Modules that install named packages:**
  [`pip_package`](../reference/modules/pip_package.md),
  [`gem_package`](../reference/modules/gem_package.md),
  [`npm_global_package`](../reference/modules/npm_global_package.md), and
  [`snap_package`](../reference/modules/snap_package.md). The node task names what
  to install. `pip_package` also takes a `requirements.txt`.

They share these settings and rules:

| Setting or rule | What it does |
| --- | --- |
| `run_as_user_account: shop` | The account that runs the installer, started directly as that account. The setting is required in every installer module except `snap_package`, because installers can run code from the packages they install. Write `run_as_user_account: root` to run the installer as root, so the choice is visible in review. `snap_package` has no such setting: root installs snaps. |
| `npm_program_path: /opt/nvm/versions/node/v24.22.0/bin/npm` | The installer program, when it is not the one on the standard path, such as Node.js from nvm. The other modules name theirs `python_program_path`, `gem_program_path`, `yarn_program_path`, `bundle_program_path`, `composer_program_path`, and `uv_program_path`. The path must be absolute. Its directory comes first in `PATH`, so `npm` finds the `node` beside it. |
| `env: {NPM_CONFIG_REGISTRY: https://npm.example.com/}` | Environment variables for the installer, such as a token for a private package server. The values may be secrets. `snap_package` has no `env`. |
| Nothing to do | The node task is `unchanged` when the installer has nothing to do. The modules for named packages report each change in the result as `{name, from, to}`. |
| Restarts | In a node task block's `changes`, what a node task installed counts as a change: a `systemd_service` in `activate` restarts when the installed software changed. A lock file module counts its record, as [When a lock file module installs again](#when-a-lock-file-module-installs-again) explains. A module for named packages asks the installer what is installed, with the node task's `env` and account, so software installed under `GEM_HOME` or `NPM_CONFIG_PREFIX` counts too. |
| A missing installer | A run fails the node task without another try, and names the package that provides the program. A dryrun reports the node task `not_checked`, because an earlier node task may install the program. |

`mmx describe-module npm_project_dependencies` prints each module's own
inputs.

## When a lock file module installs again

After a successful installation, the module writes a record into the
lock file's directory, such as `/srv/shop-api/.mmx-npm-project-dependencies`.
The record holds:

- the SHA-256 of the lock file, and of the manifest beside it: the file
  that lists the application's requirements, such as `package.json`,
  `Gemfile`, `composer.json`, or `pyproject.toml`
- the node task's settings
- the versions of the installer and of the language runtime

The node task stays `unchanged` while the record matches and the
installed packages still exist, such as `node_modules`, Bundler's gems
directory, Composer's `vendor`, or uv's `.venv`. These install again:

- a changed lock file or manifest, as in step 7
- a changed setting of the node task
- a new version of the installer or the runtime, such as
  `installed: the installer or language runtime version changed since the last installation: now npm 11.19.1, v24.22.0; previously npm 9.2.0, v20.19.2`
- an installation that was interrupted

Each lock file module runs its installer in the mode that installs exactly
what the lock file lists and never rewrites the lock file. A missing lock
file fails the node task. A dryrun names which of the reinstall causes above applies. When an earlier node task of the
dryrun would change the lock file or the manifest, such as a `git`
checkout, the dryrun reports the node task `not_checked`, because the
files that a run would read are not there yet. A `git` task leaves the
record alone, because the record is not in the repository and a checkout
keeps files that Git does not track.

**Each release in its own directory.** When each release of an
application has its own directory, each release also has its own installed
packages, so installing the next release cannot break the one that runs.
`bundler_project_dependencies` can put the gems outside the lock file
directory with
[`gems_dir`](../reference/modules/bundler_project_dependencies.md), so that
releases share the gems. The installation for the next release then changes
the gems that the running release reads.

## Python packages in a virtual environment

A virtual environment is a directory with its own Python packages, so
each application's packages stay apart from the system's. `pip_package`
creates the virtual environment, as the application's account, and
installs a `requirements.txt` into the virtual environment:

```yaml
# playbooks/set_up_shop_reports.yml
fleet_stages:
  - id: set_up_shop_reports
    nodes: web_servers
    node_tasks:
      - id: install_python_venv
        apt_package:
          packages:
            - {name: python3-venv, version: newest_when_first_installed}

      - id: create_reports_group
        group:
          name: reports
          system: true

      - id: create_reports_account
        user:
          name: reports
          primary_group: reports
          system: true
          home: /srv/shop-reports
          create_home_directory: false
          shell: /usr/sbin/nologin

      - id: copy_reports
        directory:
          path: /srv/shop-reports
          source: files/shop-reports        # holds requirements.txt
          owner: reports
          group: reports

      - id: install_reports_requirements
        pip_package:
          requirements_file_on_node: /srv/shop-reports/requirements.txt
          virtual_environment: /srv/shop-reports/venv
          run_as_user_account: reports
```

On Debian and Ubuntu, Python makes a virtual environment only with the
package `python3-venv`. With `requirements.txt` holding
`requests==2.32.5`, the first run reports each package that pip installed:

```text
set_up_shop_reports / web01 / install_reports_requirements: succeeded, changed
  [{"from":null,"name":"certifi","to":"2026.10.23"},{"from":null,"name":"charset-normalizer","to":"3.5.2"},{"from":null,"name":"idna","to":"3.20"},{"from":null,"name":"requests","to":"2.32.5"},{"from":null,"name":"urllib3","to":"2.8.0"}]
```

The next run reports `unchanged` with `[]`. A program of the application
then runs with `/srv/shop-reports/venv/bin/python`. For an application
with a `uv.lock`, use
[`uv_project_dependencies`](../reference/modules/uv_project_dependencies.md)
instead.

To pin a version in the playbook, not in a `requirements.txt`, name the
package in `packages` and write the version as a pip specifier in quotes:

```yaml
      - id: install_requests
        pip_package:
          packages:
            - {name: requests, version: "==2.32.3"}
          virtual_environment: /opt/shop/venv
          run_as_user_account: root
```

Another specifier, such as `">=2.32,<3"`, works too. [`pip_package`](../reference/modules/pip_package.md)
lists the forms.

## A language version that no package offers

Install a language from the distribution's packages when the packaged
version suits the application, as in the steps. The vendor's own package
repository is the next choice, added with
[`apt_repository`](../reference/modules/apt_repository.md) or
[`dnf_repository`](../reference/modules/dnf_repository.md), and installed with a
package node task. Packages get security updates with the rest of the
server.

When the application needs a version that no package offers, use a
version manager. The usual first choice for each language, and the
version managers:

| Language | First choice | When no package fits |
| --- | --- | --- |
| Python | The distribution's `python3`; on Ubuntu, the deadsnakes PPA for other versions | uv or pyenv; conda environments with Miniforge |
| Node.js | NodeSource's package repository | nvm, fnm, or Volta |
| Ruby | The distribution's `ruby` | rbenv with ruby-build, or rvm |
| Java | The distribution's OpenJDK, or Adoptium's package repository for Eclipse Temurin | SDKMAN! |
| Go | Go's official archive, unpacked with [`unpack_archive`](../reference/modules/unpack_archive.md). A Go program is usually built elsewhere and copied as one file. | No version manager needed |
| PHP | The distribution's packages, or the package repositories of Ondřej Surý (Debian, Ubuntu) and Remi (Rocky Linux, AlmaLinux) | No common version manager |
| Rust | rustup, only on a server that builds Rust code | |
| Several languages | mise, from its package repository | asdf |

MMX has no module for a version manager. Version managers run as shell
functions, read profile files, and each keeps its versions in its own
place, so two plain node tasks show exactly what happens on the server.
Each version manager comes in one of three forms, and each form takes the same two node tasks,
whatever the language:

| Form | Examples | The two node tasks |
| --- | --- | --- |
| A Git checkout and an install command | rbenv, pyenv, nvm, asdf | A [`git`](../reference/modules/git.md) node task checks out the version manager at a fixed tag, and a [`shell`](../reference/modules/shell.md) node task installs the language version. |
| One program from a release | uv, mise, fnm, Volta | An `unpack_archive` or [`download`](../reference/modules/download.md) node task installs the program with its SHA-256, and a `shell` node task installs the language version. |
| An installer script from the vendor | rvm, Miniforge, SDKMAN!, rustup | A `download` node task fetches the installer with its SHA-256, and a `shell` node task runs the installer. |

In each form, `skip_if_this_path_exists` skips the `shell` node task once
the version's program exists, so the next run changes nothing. A new
version is a new path, so the next run installs the new version and
leaves the old version in place until a node task deletes the old version.

Pin each version manager to a tag or a release with its SHA-256. A
checkout of the default branch, or an installer fetched without its
SHA-256, changes the version manager whenever the vendor publishes. A
pin changes the version manager only when the playbook changes.

A dryrun reports each of these `shell` node tasks as `skipped, unknown —
the shell node task has no dryrun command; set dryrun to a read-only
command, or run_script_in_dryrun: true for a script that changes nothing,
to check the task`, because MMX cannot know what a command would change.
A `dryrun` setting with a read-only command, such as
`test -x /opt/nvm/versions/node/v24.22.0/bin/node`, lets a dryrun check
the node task. The run then installs the version.

### Node.js with nvm

nvm downloads the official build of Node.js and checks its SHA-256. To
move `shop-api` from the steps to Node.js 24, replace `install_nodejs`
with these three node tasks:

```yaml
      - id: install_nvm_packages
        apt_package:
          packages:
            - {name: git, version: newest_when_first_installed}
            - {name: curl, version: newest_when_first_installed}

      - id: check_out_nvm
        git:
          repo: https://github.com/nvm-sh/nvm.git
          check_out_tag: v0.40.8
          path: /opt/nvm

      - id: install_nodejs
        shell:
          script: . /opt/nvm/nvm.sh && nvm install 24.22.0
          env:
            NVM_DIR: /opt/nvm
          skip_if_this_path_exists: /opt/nvm/versions/node/v24.22.0/bin/node
```

Then name the new `npm` in `install_dependencies`, and the new `node` in
the unit, by absolute path:

```yaml
              - id: install_dependencies
                npm_project_dependencies:
                  lock_file_dir: /srv/shop-api
                  run_as_user_account: shop
                  include_development_dependencies: false
                  npm_program_path: /opt/nvm/versions/node/v24.22.0/bin/npm
```

```ini
ExecStart=/opt/nvm/versions/node/v24.22.0/bin/node /srv/shop-api/server.js
```

The next run installs Node.js 24, installs the packages again for the
new runtime, and restarts the application:

```text
set_up_shop_api / web01 / install_nodejs: succeeded, changed
...
set_up_shop_api / web01 / shop_api/changes/install_dependencies: succeeded, changed — installed: the installer or language runtime version changed since the last installation: now npm 11.19.1, v24.22.0; previously npm 9.2.0, v20.19.2
...
set_up_shop_api / web01 / shop_api/activate/restart_shop_api: succeeded, changed — service definition changed
```

`mmx run-command --nodes web_servers -- curl -s http://127.0.0.1:3000/`
then prints `Shop API on Node.js v24.22.0` for both servers. The next run
reports `install_nodejs: skipped, unchanged — node task skipped
by a path condition for: /opt/nvm/versions/node/v24.22.0/bin/node`.

### Ruby with rbenv

rbenv with its plugin ruby-build installs Ruby under `/opt/rbenv`, for
each account on the server:

```yaml
# playbooks/install_ruby_with_rbenv.yml
fleet_stages:
  - id: install_ruby
    nodes: web_servers
    node_tasks:
      # ruby-build downloads and compiles Ruby, so it needs a compiler and
      # these libraries. ruby-build's wiki lists the packages for each distribution.
      - id: install_ruby_build_packages
        apt_package:
          packages:
            - {name: git, version: newest_when_first_installed}
            - {name: curl, version: newest_when_first_installed}
            - {name: build-essential, version: newest_when_first_installed}
            - {name: libssl-dev, version: newest_when_first_installed}
            - {name: libyaml-dev, version: newest_when_first_installed}
            - {name: libreadline-dev, version: newest_when_first_installed}
            - {name: zlib1g-dev, version: newest_when_first_installed}
            - {name: libffi-dev, version: newest_when_first_installed}
            - {name: libgmp-dev, version: newest_when_first_installed}

      - id: check_out_rbenv
        git:
          repo: https://github.com/rbenv/rbenv.git
          check_out_tag: v1.3.2
          path: /opt/rbenv

      # ruby-build goes into rbenv's plugins directory, which the rbenv
      # checkout does not have.
      - id: create_rbenv_plugins_directory
        directory:
          path: /opt/rbenv/plugins

      # A newer ruby-build knows newer Ruby versions.
      - id: check_out_ruby_build
        git:
          repo: https://github.com/rbenv/ruby-build.git
          check_out_tag: v20261013
          path: /opt/rbenv/plugins/ruby-build

      - id: install_ruby
        shell:
          script: /opt/rbenv/bin/rbenv install 3.4.11
          env:
            RBENV_ROOT: /opt/rbenv
          skip_if_this_path_exists: /opt/rbenv/versions/3.4.11/bin/ruby
```

ruby-build compiles Ruby, so `install_ruby` took about three minutes on
these servers. `mmx run-command --nodes web_servers -- /opt/rbenv/versions/3.4.11/bin/ruby --version`
then prints `ruby 3.4.11 (2026-09-23 revision 592f1ffdb3) +PRISM [x86_64-linux]`.
The next run reports `install_ruby: skipped, unchanged — node task
skipped by a path condition for: /opt/rbenv/versions/3.4.11/bin/ruby`.

pyenv takes the same node tasks as rbenv: `pyenv install 3.14.8` with
`PYENV_ROOT` in `env`, skipped once `versions/3.14.8/bin/python3` exists
under `PYENV_ROOT`. Like ruby-build, pyenv compiles the version, which
takes minutes.

### Python with uv

uv installs a prebuilt Python in seconds:

```yaml
# playbooks/install_python_with_uv.yml
fleet_stages:
  - id: install_python
    nodes: web_servers
    node_tasks:
      - id: unpack_uv
        unpack_archive:
          url: https://github.com/astral-sh/uv/releases/download/0.13.0/uv-x86_64-unknown-linux-gnu.tar.gz
          sha256: 1468ebd5a5541121837c5a2817b9972ba6090fa6caa3d142620850a47fb75154
          path: /opt/uv-0.13.0
          unpack_without_wrapping_folder: true

      # --no-bin leaves root's home directory alone.
      - id: install_python
        shell:
          script: /opt/uv-0.13.0/uv python install --no-bin 3.14.8
          env:
            UV_PYTHON_INSTALL_DIR: /opt/uv-python
          skip_if_this_path_exists: /opt/uv-python/cpython-3.14.8-linux-x86_64-gnu/bin/python3
```

The controller machine downloads the archive, checks its SHA-256, and
sends the archive to each server. On an arm64 server, the archive is
`uv-aarch64-unknown-linux-gnu.tar.gz`, as the page of
[`uv_project_dependencies`](../reference/modules/uv_project_dependencies.md)
shows, and the Python's directory ends in `linux-aarch64-gnu`.

`pip_package` then makes a virtual environment with that Python, named in
its `python_program_path`:

```yaml
      - id: install_reports_requirements_with_python_3_14
        pip_package:
          requirements_file_on_node: /srv/shop-reports/requirements.txt
          virtual_environment: /srv/shop-reports/venv-3.14
          python_program_path: /opt/uv-python/cpython-3.14.8-linux-x86_64-gnu/bin/python3
          run_as_user_account: reports
```

The first run of these three node tasks took about nine seconds on each
server. `mmx run-command --nodes web_servers -- /srv/shop-reports/venv-3.14/bin/python --version`
then prints `Python 3.14.8`. `uv_project_dependencies` takes the same
`python_program_path` for an application with a `uv.lock`.

### Conda environments with Miniforge

```yaml
# playbooks/install_miniforge.yml
fleet_stages:
  - id: install_miniforge
    nodes: web_servers
    node_tasks:
      - id: download_miniforge_installer
        download:
          permissions_mode: "0644"
          url: https://github.com/conda-forge/miniforge/releases/download/26.7.2-0/Miniforge3-26.7.2-0-Linux-x86_64.sh
          sha256: 281b0ac7d550802efc81af633225a5e6116d29ae72f3ab4eae7168c3931a4c05
          path: /opt/Miniforge3-26.7.2-0-Linux-x86_64.sh

      - id: install_miniforge
        shell:
          script: bash /opt/Miniforge3-26.7.2-0-Linux-x86_64.sh -b -p /opt/miniforge3
          skip_if_this_path_exists: /opt/miniforge3/bin/conda

      - id: copy_analysis
        directory:
          path: /srv/analysis
          source: files/analysis           # holds environment.yml

      # The environment file comes with the application.
      - id: create_analysis_environment
        shell:
          script: /opt/miniforge3/bin/conda env create --prefix /srv/analysis/env --file /srv/analysis/environment.yml
          skip_if_this_path_exists: /srv/analysis/env/bin/python
```

With `environment.yml` listing `python=3.13` and `pandas` from the
`conda-forge` channel, the first run downloaded the 124 MB installer,
installed Miniforge, and created the environment in about 17 seconds on
each server. The next run reports `download_miniforge_installer:
succeeded, unchanged` and skips the other two `shell` node tasks.
`mmx run-command --nodes web_servers -- "/srv/analysis/env/bin/python -c 'import pandas; print(pandas.__version__)'"`
then prints `3.1.1` for each server.

The last node task creates the environment once. A changed
`environment.yml` changes nothing on a later run, because the
environment's `python` already exists. rvm, SDKMAN!, and rustup take the
same node tasks: the installer script with its SHA-256, then the version
manager's own command for the version, each in a `shell` node task with
`skip_if_this_path_exists`.

### Use the installed version

Name the version's program by its absolute path wherever something runs
the program:

- **An installer module:** its program path setting, such as
  `npm_program_path` above, or `bundle_program_path` for Bundler:

  ```yaml
  - id: install_shop_web_gems
    bundler_project_dependencies:
      lock_file_dir: /srv/shop-web
      run_as_user_account: shop
      without_gemfile_groups: [development, test]
      bundle_program_path: /opt/rbenv/versions/3.4.11/bin/bundle
  ```

  Here `/srv/shop-web` holds the application's `Gemfile` and
  `Gemfile.lock`, copied with a `directory` node task as in the steps.
  The second run may install the gems once more, with
  `installed: the installer or language runtime version changed since the last installation: now bundle Bundler version 2.6.7, ...; previously bundle Bundler version 2.6.9, ...`.
  That happens when `BUNDLED WITH` in `Gemfile.lock` names another
  Bundler version than the one that comes with the Ruby: Bundler installs
  and switches to that version during the first installation. The runs
  after the second run are unchanged.

- **A systemd unit:** `ExecStart=/opt/rbenv/versions/3.4.11/bin/bundle exec puma`.
- **A `shell` node task:** the command's path, because a `shell` node task
  runs without the profile files that would set up the version manager.

## One named package

To install a tool, not an application's dependencies, name the package:

```yaml
- id: install_pgcli
  pip_package:
    packages:
      - {name: pgcli, version: newest_when_first_installed}
    virtual_environment: /opt/pgcli
    run_as_user_account: root
```

`pip_package` creates the virtual environment `/opt/pgcli` and installs
pgcli with what it needs. The first run lists each package, such as
`{"from":null,"name":"pgcli","to":"4.7.1"}`, and the next run reports
`unchanged` with `[]`. `newest_when_first_installed` installs the newest
version once and then leaves the installed version alone.

Each of these modules has its own page with its inputs:
[`pip_package`](../reference/modules/pip_package.md),
[`gem_package`](../reference/modules/gem_package.md),
[`npm_global_package`](../reference/modules/npm_global_package.md), and
[`snap_package`](../reference/modules/snap_package.md).

## If something is different

| What you see | What it means and what to do |
| --- | --- |
| `run_as_user_account is required: ...`, such as `run_as_user_account is required: the installer runs code from the packages it installs, which should not run as root unless the task says root` (lock file modules), or `... name the account that runs pip, including root` (named packages) | `mmx validate` refuses a node task without `run_as_user_account`. Add the application's account, or `run_as_user_account: root`. |
| `npm_program_path): path must be absolute without null bytes` | A program path starts with `/`, such as `/opt/nvm/versions/node/v24.22.0/bin/npm`. |
| `npm_project_dependencies installs only what a lock file lists, and /tmp has no package-lock.json or npm-shrinkwrap.json; commit the lock file with the application` | `lock_file_dir` names a directory without a lock file. Check the path, and create the lock file as step 1 shows. |
| `npm_project_dependencies: npm is not installed; install the package npm in an earlier node task, or name the program in npm_program_path` | The server has no `npm`. Install it in an earlier node task, as `install_nodejs` does, or name a version manager's `npm` in `npm_program_path`. A dryrun reports the same node task `not_checked`. |
| `npm_program_path /opt/nvm/versions/node/v24.22.0/bin/npm is not an executable program; an earlier node task may install the program.` in a dryrun | The version is not installed yet. The `shell` node task before it installs the version in the run. |
| A dryrun reports `not_checked` with `an earlier node task would change the dependency file` | An earlier node task, such as the copy or a `git` checkout, changes the lock file or the manifest in this run. The run installs what the new files list. |
| `check_out_ruby_build: the parent directory /opt/rbenv/plugins does not exist; create it with an earlier directory node task` | The rbenv checkout has no `plugins` directory. Add the `directory` node task `create_rbenv_plugins_directory` before the checkout of ruby-build, as [Ruby with rbenv](#ruby-with-rbenv) shows. |
| The application still runs the old Node.js after a new version was installed | The unit's `ExecStart` names the old `node`. Name the new version's path in the unit, as [Node.js with nvm](#nodejs-with-nvm) shows. |

## Where to go next

- [Change a configuration and reload its service](change-a-configuration-and-reload-its-service.md):
  node task blocks in full.
- The example
  [`deploy-from-git-with-release-directories`](../../examples/deploy-from-git-with-release-directories/README.md):
  a Python application checked out from Git into one directory for each
  release, with its own virtual environment.
- [Reuse work with roles](reuse-work-with-roles.md): the installation of
  an application as a role that several playbooks call.
- [Find the module for a job](find-the-module-for-a-job.md): every
  module, with its inputs.
