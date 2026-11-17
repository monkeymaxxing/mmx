# Write a package module for your distribution

**Who acts:** you, or your coding agent, in your MMX project on your
controller machine: the computer where you run `mmx`. The checks run on a
test server that runs your distribution.

**When to use this page:** your servers run a distribution whose package
manager has no MMX module, and you want node tasks that install and remove
packages there. MMX's package modules are `apt_package` for apt on Debian
and Ubuntu, `dnf_package` for dnf on Fedora, Rocky Linux, and AlmaLinux,
and the official extra modules `pacman_package` for pacman on Arch Linux
and `zypper_package` for zypper on openSUSE. If one of these modules fits your
servers, use that module instead: [Find the module for a job](find-the-module-for-a-job.md).

**What this page does:** you copy `pacman_package` into your project as
`mytool_package`, for a package manager called `mytool`. You replace
pacman's commands with your package manager's, one part of the script at
a time. Then you run the checks that show the new module behaves like
MMX's own package modules.

**Terms:**

- A **node** is a server that MMX manages. A **playbook** lists **fleet
  stages**. Each fleet stage picks nodes, and each of those nodes runs the
  fleet stage's **node tasks**, in order. A node task names one
  **module**, the code that does the work.
- A **module of the project's own** is a directory in your project's
  `external_modules/`. This page uses a script in
  [Starlark](https://github.com/google/starlark-go/blob/master/doc/spec.md),
  a small language that reads like Python. The script reads and changes
  the node only through **primitives**, functions that MMX gives the
  script, such as `run_command`.
- An **official extra module** is a Starlark module that ships with MMX,
  such as `pacman_package`. Its files are in `official_extra_modules/`
  beside the `mmx` program.
- The **package index** is the package manager's local copy of the list of
  packages that the package servers offer. pacman calls the package index
  the package databases.
- A **transaction** is one run of the package manager that installs,
  upgrades, or removes a set of packages together.
- A **node task block** groups a change with what follows it, in phases
  such as `changes` and `activate`. A `systemd_service` node task in
  `activate` restarts its service when something in `changes` changed.

## Before you start

- You know how a module of the project's own works: the script, the
  module description, and the statuses a script returns.
  [Write your own module](write-your-own-module.md) teaches them with a
  small example, and the
  [module interface reference](../reference/module-interface.md) lists
  every primitive.
- You have a test node that runs your distribution with systemd, and that
  nothing else depends on. The checks install and remove packages on the test node,
  and hold the package manager's lock. The examples call the test node
  `test01`, at `203.0.113.31`.
- You test each step on a server of the distribution that the module is
  for, which has its package manager. `mytool` does not exist, so this
  page cannot show what your server prints. The outputs on this page come
  from a Debian `test01` without `mytool`, where they show the program
  missing (steps 4 and 5). Where a step's result needs the real package
  manager, the step describes what you see, and shows no output. The
  outputs of pacman, in step 11 and in the checks, are from an Arch Linux
  node.
- You know your package manager's commands for each job in this table.
  pacman's commands are the ones that the copied script uses:

  | Job in the script | pacman's command | Where in `module.star` |
  | --- | --- | --- |
  | List the installed packages, one line with name and version each | `pacman -Q` | `read_installed` |
  | Show what an installation would do, without doing it | `pacman -Sup --needed <names> --print-format "%n %v"` | `preview`, `preview_installation` |
  | Show what a removal would do, without doing it | `pacman -Rcp <names> --print-format "%n %v"` | `remove` |
  | Refresh the package index | `pacman -Sy` | `refresh_the_package_databases` |
  | Install packages | `pacman -Su --needed <names>` | `install` |
  | Remove packages | `pacman -R <names>` | `remove` |
  | Upgrade every package | `pacman -Su`, after the refresh | `upgrade_all` |
  | Answer every question with the default | `--noconfirm` | every command that changes packages |
  | The lock that the package manager holds while it works | the file `/var/lib/pacman/db.lck` | `LOCK_FILE`, `wait_for_the_lock` |

## Steps

1. **Copy `pacman_package` into the project.** In the project directory:

   ```sh
   mkdir -p external_modules
   cp -r ~/.local/lib/mmx/official_extra_modules/pacman_package external_modules/mytool_package
   ```

   `~/.local/lib/mmx` is where [Get started](../get-started.md) installs
   `mmx` and its official extra modules. The directory name,
   `mytool_package`, becomes the module's name. Reason: a module of your
   own cannot keep the name `pacman_package`, because an official extra
   module has that name.

   **Check:** `ls external_modules/mytool_package` lists `README.md`,
   `module.star`, and `module_description.yml`.

2. **Give the copy its new name.** Two places still say
   `pacman_package`: `MODULE` in the script, which starts every failure
   reason, and the examples in the module description.

   ```sh
   sed -i 's/^MODULE = "pacman_package"/MODULE = "mytool_package"/' external_modules/mytool_package/module.star
   sed -i 's/pacman_package:/mytool_package:/' external_modules/mytool_package/module_description.yml
   ```

   On macOS, write `sed -i ''` instead of `sed -i`.

   **Check:** `mmx describe-module mytool_package` starts with:

   ```text
   mytool_package: the project's own module, Starlark, in external_modules/mytool_package/
   ```

   If the command prints `examples.0.config: the example does not name the module
   mytool_package`, an example in `module_description.yml` still names
   `pacman_package`.

3. **Add the test node and a test playbook.** Add `test01` to
   `inventories/example.yml`:

   ```yaml
   # inventories/example.yml, below nodes:
     test01:
       address: 203.0.113.31
       ssh_user: root
   ```

   Create `playbooks/test_mytool_package.yml`. The test playbook installs
   nginx and starts nginx, so the checks see both the package and a service that
   depends on the package:

   ```yaml
   # playbooks/test_mytool_package.yml
   fleet_stages:
     - id: install_nginx
       nodes: test01
       node_tasks:
         - task_block:
             id: nginx
             changes:
               - id: install_nginx_package
                 mytool_package:
                   packages: [nginx]
             activate:
               - id: start_nginx
                 systemd_service:
                   name: nginx
                   state: running
                   start_at_boot: true
   ```

   If your distribution names the package differently, write that name.

   **Check:** `mmx validate test_mytool_package` prints
   `Valid: mmx_example`. `mmx validate` checks the playbook and the module
   description. `mmx validate` does not run the script, so a mistake in
   `module.star` shows only in a dryrun or a run.

4. **Replace the rules for the inputs.** `check_inputs` returns
   `invalid_inputs` for each rule that the module description cannot
   state, before the script reads anything on the node. pacman's rules are
   the characters of a package name, a name listed twice, and inputs that
   exclude each other. Put your package manager's characters into
   `NAME_CHARACTERS`, and its rules into `name_problem`.

   `packages` in `pacman_package` is a list of names, because pacman's
   package servers offer one version of each package. If your package
   manager can install a chosen version of a package, each entry needs a
   `name` and a `version`, as in `apt_package`, `dnf_package`, and
   `zypper_package`. A module
   description has no type for a list of mappings, so give `packages` the
   type `any`, and check each entry in `check_inputs`.

   **Check:** put a name that breaks your rule into the test playbook, and
   run `mmx dryrun test_mytool_package`. The node task fails in the
   dryrun with your rule, and the exit code is 1. The reason is the text
   of your rule after `failed, unchanged — `. For pacman, the name
   `extra/nginx` gives this reason:

   ```text
   mytool_package.packages: extra/nginx may hold only letters, digits, and @ . _ + -; a repository in front of the name, such as extra/nginx, is not accepted
   ```

   Put `nginx` back afterwards.

5. **Replace the program and its message.** In `run_node_task`, change
   `find_program("pacman")` to `find_program("mytool")`, and rewrite the
   `missing_prerequisite` reason below it. On a node without the program,
   a run then fails without another try, and a dryrun reports the node
   task `not_checked`, because an earlier node task may install the
   program.

   pacman's reason tells the user to add a condition on the node fact
   `package_manager`. A **node fact** is a value that MMX reads on the
   node. On a distribution outside the families that MMX knows,
   `.node.package_manager` is unavailable, and a node task that reads it
   fails. So your reason names the node fact `distribution.id`, the `ID`
   in the node's `/etc/os-release`. A playbook that runs on several
   distributions then reads:

   ```yaml
   - when:
       equals: ["{{ .node.distribution.id }}", mydistro]
     mytool_package:
       packages: [nginx]
   ```

   For the checks below, the reason reads:

   ```python
   "reason": MODULE + ": the program mytool is not installed; mytool_package works only on a node whose distribution is mydistro; " +
             "give the node task a condition on the node fact distribution.id",
   ```

   **Check:** run `mmx dryrun test_mytool_package` on `test01`, which has
   no `mytool`. The dryrun no longer says `pacman_package works only on a
   node whose package manager is pacman`. It reports the node task
   `not_checked` with your reason, and the exit code is 3:

   ```text
   install_nginx / test01: succeeded
     install_nginx / test01 / nginx/changes/install_nginx_package: not_checked, unknown — mytool_package: the program mytool is not installed; mytool_package works only on a node whose distribution is mydistro; give the node task a condition on the node fact distribution.id
     install_nginx / test01 / nginx/activate/start_nginx: skipped, unknown — service state can be checked after the node task block's proposed changes are applied
     install_nginx / test01 / nginx: succeeded, unknown
   Some node tasks are not_checked, usually because what they check is not installed or created yet: an earlier node task does that during the run. This is normal on a new server. mmx run checks them for real; change the playbook only if the run fails.
   Run 55abc510: succeeded
   1 finished
   ```

   Then run `mmx run test_mytool_package`. The run fails at once, with the
   failure kind `missing_prerequisite` and without another try, and the
   exit code is 1:

   ```text
   install_nginx / test01: failed
     nginx: nginx/changes/install_nginx_package: mytool_package: the program mytool is not installed; mytool_package works only on a node whose distribution is mydistro; give the node task a condition on the node fact distribution.id
     install_nginx / test01 / nginx/changes/install_nginx_package: failed, unchanged — mytool_package: the program mytool is not installed; mytool_package works only on a node whose distribution is mydistro; give the node task a condition on the node fact distribution.id
     install_nginx / test01 / nginx: failed, unchanged — nginx/changes/install_nginx_package: mytool_package: the program mytool is not installed; mytool_package works only on a node whose distribution is mydistro; give the node task a condition on the node fact distribution.id
   Run afb21223: failed
   1 failed
   ```

   `mmx results afb21223 --json` shows `failure_kind: missing_prerequisite`
   and `node_job_attempt: 1`. These outputs come from a Debian `test01`,
   which has no `mytool`. On a node that has `mytool`, the node task goes
   on to the next part of the script.

6. **Replace the list of installed packages.** `read_installed` runs
   `pacman -Q` and returns a dictionary of versions by name:

   ```python
   def read_installed():
       answer = run_command(["pacman", "-Q"])
       if answer.error or answer.exit_code != 0:
           return None, message_of(answer)
       return read_lines_of_names_and_versions(answer.standard_output)
   ```

   Put in your package manager's command that prints one line for each
   installed package, with its name and version. If the lines have
   another form than `name version`, change
   `read_lines_of_names_and_versions` too. `run_command` runs a command
   that only reads, also in a dryrun. Every later part compares with this
   dictionary.

   Also change `message_of`, which picks pacman's error lines out of a
   failed command. Your package manager's errors start with other words.

   **Check:** on your test server, which has your package manager, run
   `mmx dryrun test_mytool_package`. If the dryrun fails with a reason
   that starts `mytool_package: reading the installed packages:`, the
   command or the form of its lines is wrong; the reason ends with your
   package manager's own message. If that reason is gone, the node task
   reaches the next part that still calls pacman, and that part fails
   until you replace it in step 7.

7. **Replace the preview.** Before any change, the script asks the
   package manager what a transaction would do, without doing it. pacman's
   dry run is `--print`, written `-p`, in `preview`:

   ```python
   answer = run_command(["pacman"] + arguments + ["--noconfirm"] + PRINT_FORMAT)
   ```

   Change `preview` to your package manager's dry run, and the arguments
   that `preview_installation` and `remove` pass to it. The preview
   answers three questions:

   - **Which packages would change, with their versions.** A dryrun
     reports them, and a removal refuses packages outside `packages`
     unless `also_remove_packages_that_depend_on_these` is true.
   - **Which names the package index does not have.** pacman prints
     `error: target not found: <name>` for each, and `preview` collects
     them. Change that text to your package manager's.
   - **Which names the package manager installs under other names,** such
     as a group of packages or a name that another package provides, such
     as `sh`. The copied script refuses such a name, because the package
     manager installs and removes the name under other names, so the task would
     find the name missing on every run.

   A removal needs the same check. `installed_under_other_names` asks
   pacman, for each name that is not installed under its own name,
   whether the name is a group with installed members or a name that an
   installed package provides. Write down the meaning of each exit code
   that the script relies on, as the copied script does:

   ```python
   elif provided.exit_code != 127:
       # pacman -T exits with 127 for a name that nothing installed
       # provides.
       return None, message_of(provided)
   ```

   **Check:** on your test server, without nginx, run
   `mmx dryrun test_mytool_package --diff`. The package node task reports
   `would_change` with nginx in `changed_packages`, and the reason says
   `would install nginx` with the version that your package manager's
   preview gave. The node task for `start_nginx` is `skipped`, because
   the service can be checked only after the package is installed, and the
   dryrun's exit code is 3. The `would run:` line still names pacman
   until step 10. A name that your package index lacks is `not_checked`,
   not a failure, because a run refreshes the index first.

8. **Replace the lock.** A package manager holds a lock while it changes
   packages or refreshes its index. Before each command that takes the
   lock, `wait_for_the_lock` waits for up to `LOCK_WAIT_SECONDS`, which is
   60, and then fails with a reason that says what to do. Starlark has no
   `while`, so the wait is a loop with a fixed number of rounds:

   ```python
   for _ in range(LOCK_WAIT_SECONDS):
       lock = inspect_path(LOCK_FILE)
       if lock.error:
           return {"status": "failed", "reason": MODULE + ": looking for pacman's lock " + LOCK_FILE + ": " + lock.error}
       if not lock.exists:
           return None
       run_command(["sleep", "1"])
   ```

   pacman's lock is a file that exists only while pacman runs. If your
   package manager also works this way, change `LOCK_FILE` and the
   messages. Some package managers, such as dpkg, keep a lock file that
   always exists and lock the file while they run, so the file's
   existence says nothing. For such a package manager, use its own option
   to wait for the lock, if it has one. Reading the installed packages
   and the preview take no lock.

   **Check:** none yet. The lock is checked in step 13, in a run.

9. **Replace the refresh of the package index.** The refresh goes through
   the primitive `run_command_that_refreshes_a_package_index`:

   ```python
   refreshed = run_command_that_refreshes_a_package_index(["pacman", "-Sy", "--noconfirm"])
   ```

   Keep this primitive, and change only the command. The package index is
   a cache, so MMX does not count the refresh as a change of the node
   task. A whole-system upgrade with nothing newer then reports
   `unchanged`. A dryrun refreshes nothing:
   `refresh_the_package_databases` returns at once when `is_dryrun()` is
   true, before it waits for the lock.

   Keep the order that the copied `install` uses: preview against the
   index that the node has, then refresh, then preview again. A name with
   a character that breaks your rules fails in step 4, before any
   refresh. A name that the node's index lacks may be in a newer index,
   so a run refreshes the index first, and fails with `invalid_inputs`
   only when the refreshed index still lacks the name.

   pacman has a rule of its own here: Arch Linux supports no partial
   upgrade, so each installation also upgrades the whole system, as
   `pacman -Syu <names>` does, in two steps. Most package managers allow
   an installation without a whole-system upgrade. If yours does, the
   installation refreshes the index and installs only the named packages
   and their dependencies, and `install` can drop the `-u` part.

   **Check:** none yet. The refresh is checked in step 13.

10. **Replace the transaction.** `run_transaction` runs the command that
    changes packages:

    ```python
    # In a dryrun MMX does not run the command. The call is still what makes
    # the dryrun report would_change and show "would run: pacman ..." in
    # the difference; a script never reports its change itself.
    command = run_command_that_changes_the_node(["pacman"] + command_arguments + ["--noconfirm"])
    ```

    Change `pacman` and the arguments that `install`, `remove`, and
    `upgrade_all` pass. Run the package manager without questions, as
    `--noconfirm` does for pacman, because no terminal answers them.

    Call `run_command_that_changes_the_node` in a dryrun too. Do not wrap
    the call in `if not is_dryrun()`. In a dryrun, MMX runs nothing, and
    the call is how the node task reports `would_change` and how `--diff`
    shows the command.

    **Check:** on your test server, without nginx, run
    `mmx dryrun test_mytool_package --diff`. The `would run:` line under
    the package node task now names your package manager's command, with
    the arguments you set, and no longer names pacman.

11. **Run the test playbook.**

    ```sh
    mmx run test_mytool_package
    ```

    **Check:** the package node task says `succeeded, changed` and lists
    what the transaction changed, and nginx starts. With pacman, on an
    Arch Linux node:

    ```text
    install_nginx / test01: succeeded
      install_nginx / test01 / nginx/changes/install_nginx_package: succeeded, changed — installed mailcap 2.1.54-3, nginx 1.30.6-3; upgraded dbus from 1.16.2-2 to 1.16.4-1, libp11-kit from 0.26.5-1 to 0.26.6-1, p11-kit from 0.26.5-1 to 0.26.6-1, python-jsonpatch from 1.45-1 to 1.46-1, python-jsonpointer from 3.8-1 to 3.12-1
        changed_packages: [{"from":"1.16.2-2","name":"dbus","to":"1.16.4-1"},{"from":"0.26.5-1","name":"libp11-kit","to":"0.26.6-1"},{"name":"mailcap","to":"2.1.54-3"},{"name":"nginx","to":"1.30.6-3"},{"from":"0.26.5-1","name":"p11-kit","to":"0.26.6-1"},{"from":"1.45-1","name":"python-jsonpatch","to":"1.46-1"},{"from":"3.8-1","name":"python-jsonpointer","to":"3.12-1"}]
      install_nginx / test01 / nginx/activate/start_nginx: succeeded, changed — service is not running
      install_nginx / test01 / nginx: succeeded, changed
    Run 95aa8ded: succeeded
    ```

    `changed_packages` lists every package that the transaction changed,
    also dependencies and upgrades outside `packages`. The upgrades here
    come from pacman's rule of step 9. On the node, list the installed
    packages to see that the result is true:

    ```sh
    mmx run-command --nodes test01 -- pacman -Q nginx
    ```

    Use your package manager's listing command in place of `pacman -Q`.
    If the run fails, its reason names the part of the script and the
    package manager's own message.

12. **Write the module's page.** Replace `external_modules/mytool_package/README.md`,
    which still describes pacman. Keep its sections, and write in each
    what is true for your package manager: what the module does, its
    inputs, and its examples. Write down where your package manager
    differs from the
    [rules that every package module follows](../changing-mmx/design/modules-packages.md#what-every-package-manager-module-does),
    as `pacman_package`'s page does with
    [pacman's own rules](../changing-mmx/design/modules-packages.md#pacmans-own-rules).
    Under "What a dryrun reports", write what your package manager's dry
    run does not check. pacman's dry run checks no conflict between
    packages, so a conflict shows only in a run.

    Also rewrite the descriptions in `module_description.yml`, which
    still name pacman. `mmx describe-module mytool_package` prints them.

    **Check:** `mmx describe-module mytool_package` names your package
    manager, and `grep -n pacman external_modules/mytool_package/*` finds
    nothing that is still about pacman.

13. **Run the checks on the test node.** Each check in
    [The checks](#the-checks) shows one rule of MMX's package modules.
    Run each one before you use the module on other servers.

    **Check:** each check gives the result that its row describes.

## The checks

Each check runs on `test01`. The output shown is from `mytool_package` as
copied from `pacman_package`, on Arch Linux. Your module prints its own
package manager's names and messages in the same places.

| Check | What to do | The result you must see |
| --- | --- | --- |
| A second run changes nothing | Run `mmx run test_mytool_package` twice. Before and after the second run, look at the modification times of the package index's files, such as with `stat`. | The second run reports each node task `unchanged`, with `changed_packages: []`. The index's files keep their modification times: a node task with nothing to install refreshes nothing. |
| A dryrun changes nothing | Remove nginx by hand. List the installed packages, run `mmx dryrun test_mytool_package --diff`, and list them again. | The dryrun reports `would_change` with nginx in `changed_packages`, and `would run:` with your command. Both lists of installed packages are the same, and the index's files keep their modification times. |
| A new version restarts the service | Install another version of nginx by hand, such as an older one. On Arch Linux: `pacman -U https://archive.archlinux.org/packages/n/nginx/nginx-1.30.6-2-x86_64.pkg.tar.zst`. Run the playbook twice. | The first run reports `start_nginx: succeeded, changed — tracked input module mytool_package changed after the service last started or reloaded`. The second run reports `start_nginx: succeeded, unchanged`. |
| A misspelled name fails without another try | Write `nginxx` in the test playbook, and run it. | The dryrun reports `not_checked`, because a run refreshes the index first. The run fails once with `invalid_inputs`, after the refresh: `mytool_package.packages: pacman's package databases have no package nginxx; ...`. |
| A name that breaks a rule fails before the refresh | Write a name with a character that your rules refuse, such as `extra/nginx` for pacman, and run the playbook. | The run fails with your rule, as in step 4, and the index's files keep their modification times. |
| A name of a group or a provided name is refused | Write a group's name, or a name that another package provides, such as `sh`, with `state: absent` too. | `mytool_package.packages: sh is a group or a name that another package provides, not a package of its own; name each package itself`. If your module handles such names on purpose, its page says how. |
| A node without the program reports a missing prerequisite | Point the test playbook at a node of another distribution, such as `nodes: web01` on Debian. | The dryrun reports `not_checked` with your reason. The run fails once, without another try, with the same reason. |
| The lock makes the node task wait | Hold the package manager's lock on the node, remove nginx by hand, and run the playbook. For pacman: `touch /var/lib/pacman/db.lck`. Then release the lock and run again. | The run fails after the wait: `another pacman has held its lock /var/lib/pacman/db.lck for 60 seconds; ...`. MMX tries a failed node task three times by default, so the run took about three minutes. After the lock is gone, the next run installs nginx. |
| What the dry run does not check fails in a run | Do what your package manager's dry run does not check. For pacman, install a package that conflicts with an installed one, such as `nginx-mainline` beside `nginx`. | The dryrun reports `would_change`. The run fails with the package manager's own message, and reports the node task `failed, changed`: `mytool_package: installing nginx-mainline: nginx-mainline-1.31.8-2 and nginx-1.30.6-2 are in conflict; unresolvable package conflicts detected; failed to prepare transaction (conflicting dependencies)`. |
| The exit codes mean what the comments say | Run each command that the script tests for an exit code, with a name that exists and a name that does not. | The exit codes match the comments in `module.star`, such as `pacman -T exits with 127 for a name that nothing installed provides`. |

The last row's `changed` comes from a rule of MMX: each finished call of
`run_command_that_changes_the_node` counts as a change, also one that ends
with a nonzero exit code. The [module interface reference](../reference/module-interface.md#a-starlark-script)
gives the reason.

Reinstalling the same version by hand restarts nothing: the module
records each package's version, and the version is the same as before.

## What the copied script already does

These parts of the copied script work for any package manager, so most
modules keep them as they are:

- **Reading again after the transaction.** A run reads the installed
  packages again and compares them with the first read. `changes_between`
  returns one `{name, from, to}` entry for each package whose version
  differs. The output `changed_packages` holds the entries, and the
  reason says the same in words, with at most 10 packages of each kind,
  set in `LISTED_IN_A_REASON`. A dryrun has no second read, so it uses
  the versions that the preview predicted.
- **A failure after the transaction stays a failure.** If the second read
  fails, the result has `try_again: False`. Another try would find the
  packages as the node task wants them, report success, and hide the
  failure.
- **The fingerprint.** A package module keeps no file through a changing
  primitive, so the script's own fingerprint is the whole fingerprint.
  The [tracked-input rule](../reference/module-interface.md#the-node-task-that-calls-the-module)
  decides what a changed fingerprint restarts. The fingerprint holds the
  version of each named package, or `absent`:

  ```python
  state = {}
  for name in sorted(names):
      state[name] = versions.get(name, "absent")
  result = {"outputs": {"changed_packages": changes}, "fingerprint": json.encode(state)}
  ```

  Each successful result carries the fingerprint, also an unchanged one.
  A dryrun's fingerprint uses the predicted versions, so the dryrun
  predicts the restart. A whole-system upgrade names no package and gives
  no versions to the fingerprint.

In a dryrun, the same `run_node_task` runs. The reads and the preview
run. The lock wait and the refresh do not. The transaction's call records
`would run: ...` and changes nothing. A name to install that the node's
package index lacks is `not_checked` in a dryrun, because a run refreshes
the index first, and an earlier node task may add the package server that
has the package. A missing program is `not_checked` too. Every other
failure, such as an input that breaks a rule or a removal that would take
other packages, fails in a dryrun as in a run.

## What MMX's own package modules have that yours does not

A package module in Starlark gets what every script gets, and passes the
same checks. It lacks two checks that `apt_package` and `dnf_package`
have:

- **The check across a fleet stage.** `apt_package` and `dnf_package`
  check on the controller machine that two node tasks of one fleet stage
  do not ask for conflicting versions of one package. Only modules built
  into MMX can make this check.
- **The compiler's checks.** A mistake in a script shows only when that
  part of the script runs. So the checks above must reach each part of
  the script.

## If something is different

| What you see | What it means and what to do |
| --- | --- |
| `mmx: external_modules/pacman_package: pacman_package is an official extra module of MMX; give the module another name` | The copy kept the name `pacman_package`. Rename the directory, as in step 1. |
| `examples.0.config: the example does not name the module mytool_package` | An example in `module_description.yml` still names `pacman_package`. Do step 2. |
| `the script of the module mytool_package ended with an error: module/mytool_package/module.star:29:25: got newline, want primary expression` | A mistake in `module.star`, at line 29, column 25. `mmx validate` does not run the script, so only a dryrun or a run finds the mistake. |
| `the program pacman is not installed` on your distribution | `find_program` still looks for pacman. Do step 5. |
| `would run:` still names pacman | The transaction still runs pacman. Do step 10. |
| A second run reports `changed` again | The listing of step 6 and the names in `packages` do not match, such as a name that the package manager installs under another name. Compare the listing with `packages`, and refuse such names, as step 7 says. |
| A whole-system upgrade reports `changed` on each run | The refresh goes through `run_command_that_changes_the_node`. Use `run_command_that_refreshes_a_package_index`, as step 9 says. |
| The service does not restart after a package changed | The package's node task is not in the same node task block's `changes` as the service's `activate` node task. |

## Where to go next

- [Write your own module](write-your-own-module.md): how a module of the
  project's own works, with a smaller example, and the general
  [tests for any module](write-your-own-module.md#test-the-module), such
  as a repeat after a partial failure.
- [Module interface reference](../reference/module-interface.md): every
  primitive, status, and field that a script uses.
- [`pacman_package`'s page](../../official_extra_modules/pacman_package/README.md)
  and [its script](../../official_extra_modules/pacman_package/module.star):
  the module that this page copies.
- [`zypper_package`'s script](../../official_extra_modules/zypper_package/module.star):
  another package module in Starlark, whose `packages` entries each take
  a `name` and a `version`.
- [Reuse work with roles](reuse-work-with-roles.md#one-role-for-several-distributions):
  one role that picks the package module for each distribution.
