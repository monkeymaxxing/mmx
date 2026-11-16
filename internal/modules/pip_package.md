# `pip_package`

`pip_package` installs Python packages on a node with pip, into a Python
virtual environment or into the system's Python. A task names the packages,
names packages to keep at the newest version, or points at a pip
requirements file on the node, usually inside the application's checkout.
It runs the pip of the Python on the node and links no library into MMX.

What `pip_package` shares with the other language and tool installers,
such as `run_as_user_account`, `python_program_path`, `env`, and how a
change becomes a tracked input, is in
[what the installers share](../../docs/how-to/install-languages.md#what-the-installer-modules-share)
and the design's
[shared installer behavior](../../docs/changing-mmx/design/modules-packages.md#what-these-modules-share).
`run_as_user_account` is required, including when pip runs as root.

## Set up

Python comes from the distribution's packages. On Debian and Ubuntu, a
virtual environment needs the package `python3-venv`:

```yaml
- when: {equals: ["{{ .node.package_manager }}", apt]}
  apt_package: {packages: [{name: python3-venv, version: newest_when_first_installed}]}
- when: {equals: ["{{ .node.package_manager }}", dnf]}
  dnf_package: {packages: [{name: python3, version: newest_when_first_installed}]}
```

## Examples

### Install packages into a virtual environment

```yaml
- apt_package:
    packages:
      - {name: python3-venv, version: newest_when_first_installed}

- pip_package:
    packages:
      - name: gunicorn
        version: "==23.0.0"
      - name: requests
        version: newest_when_first_installed
    virtual_environment: /srv/myapp/venv
    run_as_user_account: myapp
```

Each `packages` entry has a `name` and a `version`. An explicit version is
a pip version specifier, such as `"==23.0.0"` or `">=2.32,<3"`.
An environment marker is a condition after `;`, such as
`"==23.0.0; python_version >= '3.9'"`. MMX refuses environment markers in
`version`. Use the node task's
[`when`](../../docs/reference/shared-module-behavior.md#conditions) to set a condition.
The keywords choose newest-on-first-install or newest-on-every-run behavior.
On the node, MMX creates the virtual environment as the task's account
when the directory does not exist, then runs the pip inside it.

### Install what a requirements file lists, and restart the service

A typical deployment checks out the code, installs what it needs, and
restarts the service once when either changed:

```yaml
- task_block:
    id: myapp
    changes:
      - git:
          repo: https://github.com/example/myapp.git
          check_out_tag: v1.4.2
          path: /srv/myapp/app
          run_as_user_account: myapp
      - pip_package:
          requirements_file_on_node: /srv/myapp/app/requirements.txt
          virtual_environment: /srv/myapp/venv
          run_as_user_account: myapp
    activate:
      - systemd_service:
          name: myapp
          state: running
```

On the node, the [`git`](git.md) task checks out the release, and the
`pip_package` task installs what `requirements.txt` lists into the virtual
environment, as the account `myapp`. A new commit, or a changed set of
installed Python packages, restarts `myapp.service` once.

### Keep packages at the newest version, or uninstall them

```yaml
- pip_package:
    packages:
      - name: certifi
        version: newest_on_every_run
    virtual_environment: /srv/myapp/venv
    run_as_user_account: myapp

- pip_package:
    packages:
      - name: requests
    state: absent
    virtual_environment: /srv/myapp/venv
    run_as_user_account: myapp
```

`newest_on_every_run` installs a missing package and asks pip for the newest
version on every run. `newest_when_first_installed` leaves an installed
package at its version. Removal entries take `name` only.

### Install into the system's Python

```yaml
- pip_package:
    packages:
      - name: requests
        version: newest_when_first_installed
    install_system_wide: true
    run_as_user_account: root
```

`install_system_wide: true` runs `python -m pip` for the system's Python.
Debian 12, Ubuntu 24.04, and newer releases refuse pip installations into
the system's Python by default, to protect the distribution's Python
packages. The task then fails with pip's message.
Use a virtual environment, or the distribution's own
`python3-<name>` package. A project that accepts the risk sets
`env: {PIP_BREAK_SYSTEM_PACKAGES: "1"}`.

## How it works

A task has exactly one of `packages` or `requirements_file_on_node`,
and exactly one of `virtual_environment` or `install_system_wide: true`.
There is no default place.

MMX reads the environment's list of installed packages and versions
(`pip freeze --all`) before and after it runs pip. The task reports changed
only when the list differs, and its result's `value` lists the differences
as `{name, from, to}`. pip itself decides whether a requirement is
satisfied; MMX compares no versions. For a package installed from a URL,
MMX reads Python's installed-package metadata to find the version to keep.

A dryrun asks pip what it would install (`pip install --dry-run`, which pip
has from version 22.2) and lists that. With an older pip, such as Rocky
Linux 9's, a dryrun reports the task as not checked and says why. A
requirements file that does not exist yet also reports not checked, and so
does one that an earlier node task of the dryrun would change, such as the
`git` checkout that holds it: what the file says now is not what the run
reads. A dryrun then predicts that the service or command that follows the
task's node task block restarts or runs.

Tracked input: the environment's list of installed packages and versions.

## Tested on

**Distributions:** Debian 12 and 13, Ubuntu 24.04 and 26.04, Rocky Linux 9, AlmaLinux 10.

The suite `language_installers.py` runs on Debian 12 and 13, Ubuntu 24.04 and 26.04, Rocky Linux 9, and AlmaLinux 10, with
Python from the distribution's packages. It covers `pip_package` in a
virtual environment that it creates as the application's account, with a
requirements file, and as a tracked input that runs a command once for each
new version.

## Why it works this way

- **MMX compares `pip freeze --all` before and after pip.** pip alone
  decides whether a requirement is met, so MMX compares no versions itself.
- **A dryrun that cannot ask pip predicts the restart.** A predicted restart
  that does not come is safer than a restart the dryrun missed.

- **The task states each package's version choice.** Presence alone does not
  say whether future runs should upgrade. See the shared design's
  [required-settings rule](../../docs/changing-mmx/design/modules.md#a-setting-with-no-safe-guess-is-required).
- **The task names the installer's account.** Package installers can run
  package code, so running as root must be an explicit choice.

- **Conditions use the node task's `when`.** Refusing environment markers
  in `version` gives a task one way to state a condition.
- **A URL is not a version.** Installed-package metadata supplies the version
  to keep, while `pip freeze` still records the package's source.
