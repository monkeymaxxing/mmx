# `uv_project_dependencies`

`uv_project_dependencies` installs what a Python application's `uv.lock`
lists into the application's virtual environment, `.venv`, with
`uv sync --locked`, as the application's account. The task names the
directory on the node that holds `pyproject.toml` and the lock file, usually
a release directory or a checkout. An application that lists its packages in
a `requirements.txt` is [`pip_package`](pip_package.md)'s, with
`requirements_file_on_node` and a virtual environment.

The five lock file modules work the same way: the lock file is never
rewritten, and a record in the lock file directory makes a rerun unchanged.
That is in
[install what an application's lock file lists](../../docs/how-to/install-languages.md#steps)
and in the design's
[what the lock file modules share](../../docs/changing-mmx/design/modules-packages.md#what-the-lock-file-modules-share).
The shared rules for MMX modules that install language packages and tools,
including the inputs
`run_as_user_account`, `uv_program_path`, and `env`, are in
[what the installers share](../../docs/how-to/install-languages.md#what-the-installer-modules-share).

## Set up

uv is one program. Its release archive for each processor architecture
holds it; unpack the archive of a pinned release:

```yaml
- when: {equals: ["{{ .node.arch }}", amd64]}
  unpack_archive:
    url: https://github.com/astral-sh/uv/releases/download/0.13.0/uv-x86_64-unknown-linux-gnu.tar.gz
    sha256: 1468ebd5a5541121837c5a2817b9972ba6090fa6caa3d142620850a47fb75154
    path: /opt/uv-0.13.0
    unpack_without_wrapping_folder: true

- when: {equals: ["{{ .node.arch }}", arm64]}
  unpack_archive:
    url: https://github.com/astral-sh/uv/releases/download/0.13.0/uv-aarch64-unknown-linux-gnu.tar.gz
    sha256: 3ccfb6af6e242433eb552f7d9676abd5412c6595c497e990d9c8cb7b5bd4d2c3
    path: /opt/uv-0.13.0
    unpack_without_wrapping_folder: true
```

The node gets `/opt/uv-0.13.0/uv`. uv also needs a Python that the project
accepts: the distribution's `python3`, or one that uv installs, as the
[recipe for version managers](../../docs/how-to/install-languages.md#a-language-version-that-no-package-offers)
shows.

## Examples

### Install a release's packages without development dependencies

```yaml
- uv_project_dependencies:
    lock_file_dir: /srv/myapp/releases/v1.4.2
    run_as_user_account: myapp
    include_development_dependencies: false
    uv_program_path: /opt/uv-0.13.0/uv
```

`include_development_dependencies` is required: `false` leaves out the
`dev` group with `--no-group dev`, and `true` installs the `dev` group with
`--group dev`. Other groups follow the application's uv settings.

On the node, MMX runs
`uv sync --locked --no-python-downloads --no-group dev` in
`/srv/myapp/releases/v1.4.2` as the account `myapp`. uv creates `.venv` in
that directory with a Python it finds on the node, installs exactly what
`uv.lock` lists, and removes from `.venv` what the lock file does not list.
After a successful installation, MMX writes the record
`.mmx-uv-project-dependencies` in the same directory; the next run is
unchanged while the record matches and `.venv` exists.

### Use a Python that uv installed

```yaml
- uv_project_dependencies:
    lock_file_dir: /srv/myapp/releases/v1.4.2
    run_as_user_account: myapp
    include_development_dependencies: false
    uv_program_path: /opt/uv-0.13.0/uv
    python_program_path: /opt/uv-python/cpython-3.14.8-linux-x86_64-gnu/bin/python3
```

MMX passes the Python to uv with `--python`, and the record holds its
version, so a new Python installs the packages again. An earlier `shell`
task installs that Python, as the
[recipe for version managers](../../docs/how-to/install-languages.md#a-language-version-that-no-package-offers)
shows. A systemd unit then starts the application from
`/srv/myapp/releases/v1.4.2/.venv/bin/`.

## How it works

`uv sync --locked` reads `uv.lock` and fails when the lock file is missing
or no longer fits `pyproject.toml`; it never writes the lock file. MMX runs
uv only when the installation record does not match, as for every lock file
module. A changed `pyproject.toml` in `lock_file_dir` makes MMX run uv again
even when `uv.lock` did not change, so uv checks whether the lock file still
fits. Without `python_program_path`, uv chooses an installed Python that
the project's `requires-python` and `.python-version` accept.
Without `python_program_path`, the record holds the version of `python3`
on the installer's `PATH`.
uv keeps downloaded packages in a cache in the home directory of the account
named in `run_as_user_account`.

## Errors and what they mean

- **`uv.lock` is missing:** the task fails and names the lock file. Commit
  `uv.lock` with the application.
- **The lock file no longer fits `pyproject.toml`:** uv's own message says
  that the lock file needs to be updated. Run `uv lock` where the
  application is developed, and commit the new `uv.lock`.
- **No Python that the project accepts:** uv's message says that no
  interpreter fits the project's `requires-python`. Install a fitting Python
  in an earlier task, and name the installed Python executable in
  `python_program_path`.

## Tested on

**Distributions:** Debian 12 and 13, Ubuntu 24.04 and 26.04, Rocky Linux 9, AlmaLinux 10.

The suite `language_installers.py` runs on Debian 12 and 13, Ubuntu 24.04 and 26.04, Rocky Linux 9, and AlmaLinux 10, with
uv 0.13.0 from its release archive, and installs against a lock file that the
suite makes on the node, as a repository would carry it: first with the
distribution's `python3` (3.9 on Rocky Linux 9), then with Python 3.14.8
from `uv python install`, named in `python_program_path`.

## Why it works this way

- **uv never downloads a Python here.** uv would otherwise fetch one into the
  account's home directory when none fits, an installation that no node
  task shows and a dryrun cannot preview. Installing Python is a task of its
  own.
- **`--locked`, not `--frozen`.** Both install what `uv.lock` lists, but
  `--frozen` also installs from a lock file that no longer fits
  `pyproject.toml`; `--locked` fails, as `npm ci` does.

- **The development group is explicit.** Passing `--group dev` or
  `--no-group dev` makes the task's choice apply even when uv's configured
  default groups leave `dev` out.

- **A changed manifest reaches the installer's lock-file check.** The shared
  [installation record](../../docs/changing-mmx/design/modules-packages.md#what-the-lock-file-modules-share)
  prevents an old successful installation from hiding a manifest change.
- **A written program path is absolute.** Leaving the path out uses the
  installer's normal program selection, as the
  [shared inputs](../../docs/how-to/install-languages.md#what-the-installer-modules-share) explain.
