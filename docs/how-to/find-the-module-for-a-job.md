# Find the module for a job

**Who acts:** you, or your coding agent, in your MMX project on your
controller machine: the computer where you run `mmx`.

**When to use this page:** you know what a server should do, such as "use
the UTC time zone" or "have a test certificate", and you need the node task
that does it.

**What this page does:** you list MMX's modules, pick the module for the
job, read the module's inputs, and write and check a node task that calls
the module. The later parts say what is different about an official extra
module, what to do when no module fits, and how to see which modules a
project uses.

**Terms:**

- A **node** is a server that MMX manages. A **playbook** in `playbooks/`
  lists **fleet stages**. Each fleet stage picks nodes, and each of those
  nodes runs the fleet stage's **node tasks**, in order.
- A **module** is the code that does the work of one node task. A node task
  names one module, with the module's inputs below the name, such as
  `time_zone:` with `name: Etc/UTC` below the module's name.
- A module has one of three kinds:

  | Kind | What it is | Where it is |
  | --- | --- | --- |
  | **Core module** | Built into MMX, such as `file`, `apt_package`, or `systemd_service`. | Inside MMX's node program. |
  | **Official extra module** | A script in [Starlark](https://github.com/google/starlark-go/blob/master/doc/spec.md), a small language that reads like Python, for a less common tool, such as Incus, ZFS, or RabbitMQ. The MMX project writes, tests, and ships each official extra module with MMX. | `official_extra_modules/` beside the `mmx` program on your computer. |
  | **A module of the project's own** | A module that you or your coding agent write for the project. | The project's `external_modules/<name>/` directory. |

| What you need | Where |
| --- | --- |
| A module for a job | [Steps](#steps) |
| The inputs of a module whose name you know | [Step 3](#steps) |
| A module for a less common tool, such as Incus or RabbitMQ | [Official extra modules](#official-extra-modules) |
| No module does the job | [When no module fits](#when-no-module-fits) |
| The modules a project uses, and where | [See which modules a project uses](#see-which-modules-a-project-uses) |

## Before you start

- You have an MMX project, as in [Get started](../get-started.md). Run
  `mmx` from a directory inside the project. `mmx describe-module` and
  `mmx module-usage` read files on your computer and contact no server.
- The examples use the group `web_servers` with the servers `web01` at
  `203.0.113.11` and `web02` at `203.0.113.12`, both Debian 13.

## Steps

1. **List every module.**

   ```sh
   mmx describe-module
   ```

   The list gives each module with a one-line summary: the core modules by
   topic, then the official extra modules, then the project's own modules
   in `external_modules/`, then the node task forms that are not modules,
   such as `role` and `task_block`. A part of the list:

   ```text
   Core modules (built into MMX), by topic

   Files and links
     file        one regular file from content, template, source, or files already on the node; metadata of an existing file; or removal
     line        one line of an existing file, selected by a regular expression or its exact text; prefer a drop-in or whole file where the program has one
   ...
   Host name, time zone, locale, and alternatives
     hostname              the node's host name, stored and running, set through hostnamectl; /etc/hosts is not changed
     time_zone             the node's time zone, set through timedatectl; running services read it when they start
   ...
   Official extra modules (Starlark, shipped with MMX 0.1.0)
     create_partitions_on_empty_disk           Writes a GPT partition table with named partitions on a disk that holds nothing yet, with sfdisk; a disk that holds anything else fails the task, and nothing ever forces it.
   ...
   The project's own modules
     env_file_setting   Makes sure an environment file sets one setting to one value, and keeps the file's other lines.
   ```

   The same list, with a page for each module, is in the
   [module pages](../reference/modules/README.md). Each line there also says on which
   distributions the module is tested.

   **Check:** the list starts with `Core modules (built into MMX), by
   topic`. A coding agent can search the output, such as with
   `mmx describe-module | grep -i "time zone"`, or read it as JSON with
   `mmx describe-module --json`.

2. **Pick the module whose summary names the job.** Look in the core
   modules first, because the core modules cover files, packages, services, accounts,
   firewalls, and the other common work. For the job "use the UTC time
   zone", the topic `Host name, time zone, locale, and alternatives` holds
   `time_zone`.

   For a file, several modules fit. The [`edit_file`](../reference/modules/edit_file.md)
   page gives the order to choose in: a drop-in file, a whole `file`, one
   `line`, then `edit_file`.

   **Check:** the module's summary says what you want the server to have.
   If no summary fits, read [Official extra modules](#official-extra-modules),
   then [When no module fits](#when-no-module-fits).

3. **Read the module's inputs.**

   ```sh
   mmx describe-module time_zone --reference-only
   ```

   ```text
   time_zone: core module, built into MMX
   the node's time zone, set through timedatectl; running services read it when they start

   Fields
     name   string   required; a name from the time zone database, such as Europe/Berlin or UTC; a name the node's /usr/share/zoneinfo does not have fails before any change

   Example
     - time_zone:
         name: Europe/Berlin

   Result: status, change, value.time_zone_before, value.time_zone_after
   Dryrun: supported; reads the time zone and reports the one that would be set; an unknown name fails
   Retry: bounded automatic retries by default; retry: false turns them off
   ```

   The first line gives the module's kind. `Result:` lists what a later
   node task can read from this node task's result. `Dryrun:` says what
   `mmx dryrun` checks.

   Without `--reference-only`, `mmx describe-module time_zone` prints the
   module's whole page: its inputs, examples, how it works, its errors,
   and the distributions it is tested on. In a terminal, the page opens in
   the program that `PAGER` names, or in `less -FRX`. When the output is no
   terminal, such as for a coding agent, the page is printed as it is.
   `--json` gives the same facts as JSON.

   **Check:** the command prints the module's kind on the first line. If
   it prints `mmx: time_zones is not a node task setting or a known
   module; ...` with exit code 4, the name is misspelled: find the right
   name in the list from step 1.

4. **Write the node task.** Start from the example of step 3. This
   playbook gives every web server the UTC time zone:

   ```yaml
   # playbooks/set_time_zone.yml
   fleet_stages:
     - id: set_time_zone
       nodes: web_servers
       node_tasks:
         - id: use_utc
           time_zone:            # the module's name
             name: Etc/UTC       # the module's input
   ```

   **Check:** `mmx validate set_time_zone` prints `Valid:` and the
   project's name. A misspelled input fails with the file, the line, and
   the input's name, such as
   `playbooks/set_time_zone.yml: line 8 column 11 (fleet_stages[0].node_tasks[0].time_zone.nam): unknown field`.

5. **Preview the node task.**

   ```sh
   mmx dryrun set_time_zone --output=summary
   echo "exit code $?"
   ```

   ```text
   set_time_zone / 2 nodes [web01, web02]: succeeded
     set_time_zone / 2 nodes [web01, web02] / use_utc: succeeded, unchanged
   Run 5687c476: succeeded
   2 finished
   ...
   exit code 0
   ```

   **Check:** the node task says `succeeded`. `unchanged` means that the
   servers already use that time zone. `would_change` with exit code 2
   means that `mmx run set_time_zone` would change the servers.
   [Preview and run changes](preview-and-run-changes.md#what-the-results-mean)
   explains every result.

## Official extra modules

An official extra module is a module for a less common tool, such as
`incus_instance` for Incus, `zfs_dataset` for ZFS, `rabbitmq_user` for
RabbitMQ, or `pacman_package` for Arch Linux. You find, read, and call an
official extra module as steps 1 to 4 show for a core module. The first
line of an official extra module's reference names the module's kind and
the directory of the module's files:

```text
incus_instance: official extra module, shipped with MMX 0.1.0, in /home/you/.local/lib/mmx/official_extra_modules/incus_instance/
```

Four things are different from a core module:

- **The tool must be on the server first.** An official extra module does
  not install the tool that it works with. Install the tool with a package
  node task before the module's node task. While the tool's program is
  missing, a dryrun reports the module's node task `not_checked`, and a
  run fails with a reason that names the missing program and, for most
  modules, the package that provides it.
- **The module comes with `mmx`.** The module's files are in
  `official_extra_modules/` beside the `mmx` program, in the same release
  archive. A run sends the module's files to each server that needs the
  module. Install
  `mmx` with the whole archive, as [Get started](../get-started.md) step 1
  does.
- **The module and `mmx` have one version.** When the files in
  `official_extra_modules/` come from another release than the `mmx`
  program, `mmx` refuses to read the project. Install `mmx` and
  `official_extra_modules/` again from one release archive.
- **A password does not hide the result.** An input that is a private
  value, such as `password`, takes a secret without making the node task's
  result private. MMX shows `[redacted]` wherever a text of the result
  would hold the value.
- **You do not edit an official extra module.** To make such a module
  work differently, write a module of the project's own under another name:
  [Write your own module](write-your-own-module.md).

Each official extra module's page is the `README.md` in its directory,
such as [`incus_instance`](../../official_extra_modules/incus_instance/README.md).
`mmx describe-module incus_instance` prints the same page.

## When no module fits

Choose the smallest tool that does the job:

| The job | Use |
| --- | --- |
| Several existing modules together, used in several places, such as "install the app, write its settings, start it" | A role: [Reuse work with roles](reuse-work-with-roles.md). |
| A command that must run once, such as creating a certificate or loading a database schema | A `shell` node task with a guard that skips the command when its work is done. See [the example below](#a-command-with-a-guard). |
| A command that must run after a file changed, such as `postmap` after a Postfix table changed | A `shell` node task with `only_when_tracked_inputs_changed: true`: [Run a command after a change](../reference/modules/shell.md#run-a-command-after-a-change). |
| One command now, on some servers, with no playbook | `mmx run-command`: [Run a command on servers now](run-a-command-on-servers-now.md). |
| A thing with its own state that a command must inspect, change only when the state differs, and report on, used in several playbooks | A module of the project's own: [Write your own module](write-your-own-module.md). |
| Packages on a distribution that has no package module | A module of the project's own, built from `pacman_package`: [A package module for your distribution](write-a-package-module.md). |

A module of your own is worth its extra files when a `shell` node task
would need several guards, would parse a program's output, or must say
`unchanged` and `would_change` correctly. A `shell` node task that runs
says `changed` unless you write a `changed_when` test on its output. A
dryrun checks the node task only through a read-only `dryrun` command, or
by running a read-only script with `run_script_in_dryrun: true`.

### A command with a guard

No module creates a self-signed certificate for a test server. A `shell`
node task does, and its guard `skip_if_this_path_exists` skips the command
when the certificate is already there:

```yaml
# playbooks/make_test_certificate.yml
fleet_stages:
  - id: make_test_certificate
    nodes: web_servers
    node_tasks:
      - id: install_openssl
        apt_package:
          packages:
            - {name: openssl, version: newest_when_first_installed}

      - id: create_self_signed_certificate
        shell:
          script: >-
            openssl req -quiet -x509 -newkey rsa:2048 -noenc -days 365
            -subj "/CN=$SERVER_NAME"
            -keyout /etc/ssl/private/shop-test.key
            -out /etc/ssl/certs/shop-test.pem
          env:
            SERVER_NAME: "{{ .node.name }}"     # MMX replaces references in env, never in script
          skip_if_this_path_exists: /etc/ssl/certs/shop-test.pem
```

A dryrun cannot tell what the command would do, because the node task has
no `dryrun` command. So the dryrun reports the node task `skipped,
unknown` and exits with code 3:

```text
make_test_certificate / 2 nodes [web01, web02] / create_self_signed_certificate: skipped, unknown — the shell node task has no dryrun command; set dryrun to a read-only command, or run_script_in_dryrun: true for a script that changes nothing, to check the task
```

The first `mmx run make_test_certificate` says `succeeded, changed` for
the certificate. Every later run skips the command:

```text
make_test_certificate / 2 nodes [web01, web02] / create_self_signed_certificate: skipped, unchanged — node task skipped by a path condition for: /etc/ssl/certs/shop-test.pem
```

The [`shell` page](../reference/modules/shell.md#guards) lists every guard, such as
`run_only_if_this_command_succeeds` and `run_only_once_marker_file`.

## See which modules a project uses

```sh
mmx module-usage
```

`mmx module-usage` lists every module that the project's playbooks, and
the roles they call, use, by kind, with each file and line that calls the
module:

```text
Core modules (built into MMX)
  apt_package       playbooks/make_test_certificate.yml:8
  directory         playbooks/configure_shop.yml:8
  shell             playbooks/make_test_certificate.yml:13
  systemd_service   playbooks/configure_shop.yml:41
  systemd_unit      playbooks/configure_shop.yml:15
  time_zone         playbooks/set_time_zone.yml:8

The project's own modules
  env_file_setting   external_modules/env_file_setting/   Starlark
                     playbooks/configure_shop.yml:29, playbooks/configure_shop.yml:35
```

- `mmx module-usage set_time_zone` covers one playbook.
- `mmx module-usage --module env_file_setting` shows one module.
- For an executable module of the project's own, the list also names the
  program that runs the executable, such as `executable (Bash)`. The
  [module interface reference](../reference/module-interface.md#dryrun-rules)
  says what that list says about the executable's dryrun.
- A module of the project's own that no node task calls is listed last.
- `--json` names each module's `kind`: `core`, `official_extra`,
  `project_starlark`, or `project_executable`.

## If something is different

| What you see | What it means and what to do |
| --- | --- |
| `mmx: fleet stage set_time_zone: playbooks/set_time_zone.yml:8:11 (fleet_stages[0].node_tasks[0].time_zones): time_zones is not a node task setting or a known module; MMX checked the core modules (listed by mmx describe-module), the official extra modules in /home/you/.local/lib/mmx/official_extra_modules, and the project's own modules in external_modules/` | No module has that name. MMX looks a name up among the core modules, then the official extra modules, then `external_modules/`. Check the spelling with `mmx describe-module`. |
| The same message says `the official extra modules (... does not exist)` | `mmx` was installed without the rest of the release archive. Install `mmx` again as [Get started](../get-started.md) step 1 says. |
| `the official extra modules in ... were shipped with MMX 0.1.0, but the mmx program is version 0.2.0; reinstall the mmx program and official_extra_modules/ from a single MMX release` | The `mmx` program and `official_extra_modules/` come from different releases. Install both from one release archive. |
| `mmx: external_modules/line: line is a core module of MMX; give the module another name` | A directory in `external_modules/` has the name of a core module. Rename the directory and the node tasks that call it. [The module's name](../reference/module-interface.md#the-modules-name) lists the rules. |
| A dryrun says `not_checked` for an official extra module's node task, and names a program | The tool is not installed yet. If an earlier node task installs it, run the playbook: the run checks the node task for real. Otherwise add a package node task for the tool. |
| The server runs a distribution that no package module covers | Write a package module of the project's own: [A package module for your distribution](write-a-package-module.md). [Limits on each distribution](../reference/limits-on-each-distribution.md) says what is limited where. |

## Where to go next

- [Write your own module](write-your-own-module.md): a module of the
  project's own, as a Starlark script or as an executable.
- [The module pages](../reference/modules/README.md): every core module and official
  extra module, with its inputs, examples, and errors.
- [Write a playbook](write-a-playbook.md): fleet stages, node tasks, and a
  node task block.
- [Module interface reference](../reference/module-interface.md): what a
  module of the project's own may call and must return.
