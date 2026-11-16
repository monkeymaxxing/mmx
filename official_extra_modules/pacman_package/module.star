# pacman_package manages packages on an Arch Linux node through pacman:
# packages present or absent, and the whole system upgraded.
# docs/changing-mmx/design/modules-packages.md owns the behavior ("What every package
# manager module does" and "pacman's own rules").
#
# The shape fits any package manager: read the installed packages, ask the
# package manager what a transaction would do without doing it, refresh its
# index, run the transaction, and read the installed packages again. The
# parts marked "pacman's own rule" are where another package manager
# differs.
#
# pacman's own rule: pacman supports no partial upgrade. The package
# databases and the installed packages must come from one moment, so every
# installation is also a full system upgrade, as pacman -Syu <package> is,
# and a package cannot be held at a version or brought to the newest
# version alone.

MODULE = "pacman_package"

# pacman takes this file while it changes packages or downloads its
# databases, and refuses to start while the file exists. A pacman that was
# stopped leaves the file behind.
LOCK_FILE = "/var/lib/pacman/db.lck"

# How long a task waits for another pacman to give up its lock, as
# apt_package waits for dpkg's: long enough for an upgrade that a timer or a
# person started, and short enough that a lock that a stopped pacman left
# fails the run with the reason.
LOCK_WAIT_SECONDS = 60

# The characters of a package name, which must not start with a hyphen or a
# dot: letters, digits, and @ . _ + - (PKGBUILD(5)).
NAME_CHARACTERS = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789@._+-"

# A reason lists at most this many packages of each kind of change; the
# output changed_packages lists every one.
LISTED_IN_A_REASON = 10

# What pacman prints, one line per package, for a list of packages
# (--print-format) and for the installed packages (pacman -Q).
PRINT_FORMAT = ["--print-format", "%n %v"]

def invalid(field, rule):
    """Returns the result for an input that breaks a rule."""
    return {"status": "invalid_inputs", "reason": MODULE + "." + field + ": " + rule}

def verb_for_this_run(past, infinitive):
    """Returns the verb of a reason: what the run did, or in a dryrun what a
    run would do."""
    return "would " + infinitive if is_dryrun() else past

def message_of(command):
    """Returns pacman's own message of a command that failed: the packages
    that are in conflict and the error lines, or else its last question,
    such as whether to remove a package that pacman.conf's HoldPkg
    protects, which --noconfirm answers no."""
    if command.error:
        return command.error
    conflicts = []
    for line in command.standard_output.splitlines():
        if line.startswith(":: ") and " are in conflict" in line:
            conflict = line.removeprefix(":: ").split(" are in conflict")[0] + " are in conflict"
            if conflict not in conflicts:
                conflicts.append(conflict)
    errors = [line.removeprefix("error: ") for line in command.standard_error.splitlines() if line.startswith("error: ")]
    if conflicts or errors:
        return "; ".join(conflicts + errors)
    questions = [line.removeprefix(":: ") for line in command.standard_output.splitlines() if line.startswith(":: ")]
    if questions:
        return questions[-1]
    return "pacman ended with exit code " + str(command.exit_code) + " and printed no message"

def name_problem(name):
    """Returns the rule that a package name breaks, or an empty text."""
    if name == "":
        return "must not be empty"
    if name.startswith("-") or name.startswith("."):
        return "must not start with a hyphen or a dot"
    for character in name.elems():
        if character not in NAME_CHARACTERS:
            return "may hold only letters, digits, and @ . _ + -; a repository in front of the name, such as extra/nginx, is not accepted"
    return ""

def check_inputs(inputs):
    """Returns the result for inputs that break a rule that the description
    cannot state, or None."""
    names = inputs.get("packages")
    upgrade_all = inputs.get("upgrade_all_packages", False)
    state = inputs.get("state", "present")
    if upgrade_all:
        for other in ["packages", "state", "also_remove_packages_that_depend_on_these"]:
            if other in inputs:
                return invalid("upgrade_all_packages", "takes no other input; remove " + other)
        return None
    if names == None:
        return invalid("packages", "is required, or upgrade_all_packages: true")
    if not names:
        return invalid("packages", "must not be empty")
    seen = {}
    for name in names:
        problem = name_problem(name)
        if problem:
            return invalid("packages", name + " " + problem if name else problem)
        if name in seen:
            return invalid("packages", name + " is listed twice")
        seen[name] = True
    if inputs.get("also_remove_packages_that_depend_on_these") and state != "absent":
        return invalid("also_remove_packages_that_depend_on_these", "requires state: absent")
    return None

def read_lines_of_names_and_versions(text):
    """Reads lines of a package name and its version, as pacman -Q and
    --print-format "%n %v" print them. Returns a dictionary of versions by
    name and an empty text, or None and the line that has another form."""
    versions = {}
    for line in text.splitlines():
        fields = line.split(" ")
        if len(fields) != 2 or not fields[0] or not fields[1]:
            return None, "pacman printed a line that is not a package name and a version: " + line[:200]
        versions[fields[0]] = fields[1]
    return versions, ""

def read_installed():
    """Returns the installed packages, as a dictionary of versions by name,
    and an empty text, or None and what went wrong."""
    answer = run_command(["pacman", "-Q"])
    if answer.error or answer.exit_code != 0:
        return None, message_of(answer)
    return read_lines_of_names_and_versions(answer.standard_output)

def preview(arguments):
    """Asks pacman which packages a transaction would install or remove,
    without a change (--print). Returns three values: the packages with
    their versions; the names that pacman's databases do not have; and
    what else went wrong. --print resolves names and dependencies, and
    checks no conflict between packages: pacman reports a conflict only
    when the transaction runs."""
    answer = run_command(["pacman"] + arguments + ["--noconfirm"] + PRINT_FORMAT)
    if answer.error:
        return None, [], answer.error
    if answer.exit_code != 0:
        not_found = [line.removeprefix("error: target not found: ") for line in answer.standard_error.splitlines()
                     if line.startswith("error: target not found: ")]
        return None, not_found, message_of(answer)
    targets, problem = read_lines_of_names_and_versions(answer.standard_output)
    return targets, [], problem

def not_packages_of_their_own(names):
    """Returns the result for names that pacman knows as a group or as a
    name that another package provides. pacman installs and removes such a
    name under other names, so a task that named it would find it missing
    on every run."""
    return invalid("packages", ", ".join(names) + " is a group or a name that another package provides, not a package of its own; name each package itself")

def wait_for_the_lock():
    """Waits while another pacman holds its lock. Returns None when the lock
    is free, or the failed result."""
    for _ in range(LOCK_WAIT_SECONDS):
        lock = inspect_path(LOCK_FILE)
        if lock.error:
            return {"status": "failed", "reason": MODULE + ": looking for pacman's lock " + LOCK_FILE + ": " + lock.error}
        if not lock.exists:
            return None
        run_command(["sleep", "1"])
    return {
        "status": "failed",
        "reason": MODULE + ": another pacman has held its lock " + LOCK_FILE + " for " + str(LOCK_WAIT_SECONDS) + " seconds; " +
                  "when no pacman runs on the node, a pacman that was stopped left the file, and removing it lets pacman run again",
    }

def refresh_the_package_databases():
    """Downloads pacman's package databases, in a run; a dryrun refreshes
    nothing. Returns None, or the failed result."""
    if is_dryrun():
        return None
    failure = wait_for_the_lock()
    if failure:
        return failure
    refreshed = run_command_that_refreshes_a_package_index(["pacman", "-Sy", "--noconfirm"])
    if refreshed.error or refreshed.exit_code != 0:
        return {"status": "failed", "reason": MODULE + ": refreshing the package databases: " + message_of(refreshed)}
    return None

def changes_between(before, after):
    """Returns each package whose version differs, by name: {name, from, to},
    without from for a package that was not installed and without to for
    one that is gone."""
    changes = []
    for name in sorted({name: True for name in list(before) + list(after)}):
        old, new = before.get(name), after.get(name)
        if old == new:
            continue
        change = {"name": name}
        if old != None:
            change["from"] = old
        if new != None:
            change["to"] = new
        changes.append(change)
    return changes

def joined_with_a_limit(parts):
    """Returns parts joined for a reason, with at most LISTED_IN_A_REASON of
    them."""
    if len(parts) > LISTED_IN_A_REASON:
        return ", ".join(parts[:LISTED_IN_A_REASON]) + ", and " + str(len(parts) - LISTED_IN_A_REASON) + " more"
    return ", ".join(parts)

def describe(changes):
    """Returns the reason of a task that changed packages, such as
    "installed nginx 1.30.6-3; upgraded glibc from 2.43-1 to 2.44-1"."""
    installed = [c["name"] + " " + c["to"] for c in changes if "from" not in c]
    upgraded = [c["name"] + " from " + c["from"] + " to " + c["to"] for c in changes if "from" in c and "to" in c]
    removed = [c["name"] + " " + c["from"] for c in changes if "to" not in c]
    parts = []
    if installed:
        parts.append(verb_for_this_run("installed", "install") + " " + joined_with_a_limit(installed))
    if upgraded:
        parts.append(verb_for_this_run("upgraded", "upgrade") + " " + joined_with_a_limit(upgraded))
    if removed:
        parts.append(verb_for_this_run("removed", "remove") + " " + joined_with_a_limit(removed))
    return "; ".join(parts)

def succeeded_with(names, versions, changes):
    """Returns the result of a task that succeeded. The fingerprint holds
    the version of each package of the task, or absent, so that a node task
    block's service restarts when one of them changed."""
    state = {}
    for name in sorted(names):
        state[name] = versions.get(name, "absent")
    result = {"outputs": {"changed_packages": changes}, "fingerprint": json.encode(state)}
    if changes:
        result["reason"] = describe(changes)
    return result

def run_transaction(command_arguments, description, names, installed_before, predicted_after):
    """Runs the pacman command that changes packages, and returns the
    result. A dryrun reports the versions that the preview predicted; a run
    reads the installed packages again and reports what pacman did."""
    if not is_dryrun():
        failure = wait_for_the_lock()
        if failure:
            return failure
    # In a dryrun MMX does not run the command. The call is still what makes
    # the dryrun report would_change and show "would run: pacman ..." in
    # the difference; a script never reports its change itself.
    command = run_command_that_changes_the_node(["pacman"] + command_arguments + ["--noconfirm"])
    if command.error or command.exit_code != 0:
        return {"status": "failed", "reason": MODULE + ": " + description + ": " + message_of(command)}
    installed_after = predicted_after
    if not is_dryrun():
        installed_after, problem = read_installed()
        if problem:
            # Another try would find the packages as the task wants them and
            # report an unchanged task, so this failure stays.
            return {"status": "failed", "reason": MODULE + ": " + description + " ended, and then reading the installed packages failed: " + problem, "try_again": False}
    return succeeded_with(names, installed_after, changes_between(installed_before, installed_after))

def preview_installation(missing, after_refresh):
    """Asks pacman what installing the missing packages would do. Returns
    the packages that pacman would install or upgrade with their versions,
    and None; or None and the result that ends the task. Before a refresh in
    a run, a name that the databases lack gives None and None: the run
    refreshes and asks again."""
    targets, not_found, problem = preview(["-Sup", "--needed"] + missing)
    if not_found and is_dryrun():
        return None, {
            "status": "not_checked",
            "reason": MODULE + ": the node's package databases have no package " + ", ".join(not_found) +
                      "; a run refreshes them first, and an earlier node task may add the repository that has it",
        }
    if not_found and after_refresh:
        return None, invalid("packages", "pacman's package databases have no package " + ", ".join(not_found) +
                             "; pacman -Ss searches them, and a repository of your own goes into /etc/pacman.conf in an earlier node task")
    if not_found:
        return None, None
    if problem:
        return None, {"status": "failed", "reason": MODULE + ": previewing the installation of " + ", ".join(missing) + ": " + problem}
    # A missing name that pacman resolves to other packages is a group or
    # a provided name. A name in IgnorePkg still shows: --noconfirm answers
    # pacman's "install anyway?" with yes.
    not_their_own = [name for name in missing if name not in targets]
    if not_their_own:
        return None, not_packages_of_their_own(not_their_own)
    return targets, None

def install(names, installed):
    """Installs the packages that are missing, with a full system upgrade."""
    missing = [name for name in names if name not in installed]
    if not missing:
        return succeeded_with(names, installed, [])
    # pacman's own rule: install only after a refresh, with a full upgrade.
    # The names are checked against the databases the node has first, so a
    # wrong name fails before a refresh would leave the databases newer than
    # the installed packages.
    targets, ending = preview_installation(missing, False)
    if ending:
        return ending
    if not is_dryrun():
        failure = refresh_the_package_databases()
        if failure:
            return failure
        targets, ending = preview_installation(missing, True)
        if ending:
            return ending
    return run_transaction(["-Su", "--needed"] + missing, "installing " + ", ".join(missing), names, installed, installed | targets)

def installed_under_other_names(names, installed):
    """Returns the names, among those not installed under their own name,
    that are a group with installed members (pacman -Qg) or a name that an
    installed package provides (pacman -T), and an empty text; or None and
    what went wrong."""
    found = []
    for name in [name for name in names if name not in installed]:
        group = run_command(["pacman", "-Qg", name])
        provided = run_command(["pacman", "-T", name])
        if group.error or provided.error:
            return None, group.error or provided.error
        if group.exit_code == 0 or provided.exit_code == 0:
            found.append(name)
        elif provided.exit_code != 127:
            # pacman -T exits with 127 for a name that nothing installed
            # provides.
            return None, message_of(provided)
    return found, ""

def remove(names, installed, also_dependents):
    """Removes the packages that are installed, and with also_dependents
    the packages that depend on them."""
    not_their_own, problem = installed_under_other_names(names, installed)
    if problem:
        return {"status": "failed", "reason": MODULE + ": reading the installed groups and provided names: " + problem}
    if not_their_own:
        return not_packages_of_their_own(not_their_own)
    present = [name for name in names if name in installed]
    if not present:
        return succeeded_with(names, installed, [])
    # -c lists the packages that depend on these too, which pacman removes
    # only with -c and otherwise refuses to leave without what they need.
    targets, _, problem = preview(["-Rcp"] + present)
    if problem:
        return {"status": "failed", "reason": MODULE + ": previewing the removal of " + ", ".join(present) + ": " + problem}
    dependents = sorted([name for name in targets if name not in names])
    if dependents and not also_dependents:
        return {
            "status": "invalid_inputs",
            "reason": MODULE + ": removing " + ", ".join(present) + " would also remove " + ", ".join(dependents) +
                      ", which depend on them; to remove these packages too, add also_remove_packages_that_depend_on_these: true",
        }
    predicted_after = {name: version for name, version in installed.items() if name not in targets}
    return run_transaction(["-R"] + (["-c"] if dependents else []) + present, "removing " + ", ".join(present), names, installed, predicted_after)

def upgrade_all(installed):
    """Upgrades every package that has a newer version, as pacman -Syu."""
    failure = refresh_the_package_databases()
    if failure:
        return failure
    targets, _, problem = preview(["-Sup"])
    if problem:
        return {"status": "failed", "reason": MODULE + ": previewing the upgrade of all packages: " + problem}
    if not targets:
        return succeeded_with([], installed, [])
    return run_transaction(["-Su"], "upgrading all packages", [], installed, installed | targets)

def run_node_task(inputs):
    failure = check_inputs(inputs)
    if failure:
        return failure
    location = find_program("pacman")
    if location.error:
        return {"status": "failed", "reason": MODULE + ": looking for the program pacman: " + location.error}
    if not location.found:
        return {
            "status": "missing_prerequisite",
            "reason": MODULE + ": the program pacman is not installed; pacman_package works only on a node whose package manager is pacman, " +
                      "such as Arch Linux; give the node task a condition on the node fact package_manager",
        }
    installed, problem = read_installed()
    if problem:
        return {"status": "failed", "reason": MODULE + ": reading the installed packages: " + problem}
    if inputs.get("upgrade_all_packages", False):
        return upgrade_all(installed)
    if inputs.get("state", "present") == "absent":
        return remove(inputs["packages"], installed, inputs.get("also_remove_packages_that_depend_on_these", False))
    return install(inputs["packages"], installed)
