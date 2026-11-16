# zypper_package manages packages on an openSUSE node through zypper:
# packages present with a version rule or absent, and the whole system
# updated. docs/changing-mmx/design/modules-packages.md owns the behavior ("What every
# package manager module does" and "zypper's own rules").
#
# The shape is the one of pacman_package: read the installed packages, ask
# the package manager what a transaction would do without doing it, refresh
# its index, run the transaction, and read the installed packages again.
# The parts marked "zypper's own rule" are where zypper differs.

MODULE = "zypper_package"

# The version values that are no version. Any other value is a version that
# the installed package must have.
NEWEST_WHEN_FIRST_INSTALLED = "newest_when_first_installed"
NEWEST_ON_EVERY_RUN = "newest_on_every_run"

# zypper's exit codes, as zypper(8) lists them and as they were observed on
# openSUSE Leap 16.1 (zypper 1.14). Only the codes below are tested.
EXIT_OK = 0
# ZYPPER_EXIT_ERR_ZYPP: libzypp failed. Seen for a solver problem (a package
# that conflicts with an installed one), a repository that did not answer
# during a refresh, and a repository whose signing key is not imported.
EXIT_PROBLEM = 4
# ZYPPER_EXIT_ZYPP_LOCKED: another program holds /run/zypp.pid. Nothing
# changed.
EXIT_LOCKED = 7
# ZYPPER_EXIT_ERR_COMMIT: the transaction failed. Seen for a file conflict
# between two packages (the package is not installed), and for a package
# whose signature is missing or wrong (nothing is installed from it).
EXIT_COMMIT_FAILED = 8
# ZYPPER_EXIT_INF_REBOOT_NEEDED and ZYPPER_EXIT_INF_RESTART_NEEDED: the
# transaction worked; a reboot, or a new start of zypper, is advised. These
# two are in zypper(8) and not observed on the test node.
EXIT_REBOOT_NEEDED = 102
EXIT_ZYPPER_ITSELF_WAS_UPDATED = 103
# ZYPPER_EXIT_INF_CAP_NOT_FOUND: no package or provider of the name exists.
# Seen for an install and for a remove.
EXIT_NAME_NOT_FOUND = 104
# ZYPPER_EXIT_INF_RPM_SCRIPT_FAILED: the transaction worked and a script of
# a package failed. The package stays installed.
EXIT_SCRIPT_FAILED = 107
# The codes 100 and 101 (updates are available) belong to commands that
# list updates; 105 (zypper was stopped by a signal) and 106 (a repository
# was skipped) are not tested.
CODES_OF_A_TRANSACTION_THAT_WORKED = [EXIT_OK, EXIT_REBOOT_NEEDED, EXIT_ZYPPER_ITSELF_WAS_UPDATED]

# zypper takes this file while it changes packages or reads them; a second
# zypper then ends with exit code 7 and names the pid of the first. The file
# stays after zypper ends and is then empty, so a script cannot tell a held
# lock from its existence: it runs zypper and tests the exit code.
LOCK_FILE = "/run/zypp.pid"

# How long a task waits for another zypper, as apt_package waits for dpkg's
# lock. zypper itself waits when the environment variable ZYPP_LOCK_TIMEOUT
# names a number of seconds, for reads, for a refresh, and for a transaction
# alike (observed on openSUSE Leap 16.1), so no script loop is needed. The
# lock is released when its holder ends, even when it was killed, so a lock
# that is still held after this time has a zypper that runs.
LOCK_WAIT_SECONDS = 60

# The characters of an RPM package name. Anything else is a form that
# zypper reads as another request: a pattern (php*), a capability (/usr/bin/php,
# libz.so.1()(64bit)), a version (name=1.2), or a repository (repo:name).
# zypper removes whatever provides such a capability, so a task never passes
# one.
NAME_CHARACTERS = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789._+-"
VERSION_CHARACTERS = NAME_CHARACTERS + "~^:"

# A reason lists at most this many packages of each kind of change; the
# output changed_packages lists every one.
LISTED_IN_A_REASON = 10

# zypper's options for every command: the wait for the lock, no question,
# no refresh of the repositories (the module refreshes them itself, in a
# run), and XML output, in which a problem is a message of type error or
# warning, or the description of a prompt.
def zypper_command(options = ["--non-interactive", "--no-refresh", "--xmlout"]):
    """Returns the start of every zypper command. env sets the wait for the
    lock, and env finds only the programs in its own PATH, so the command names
    zypper by its path."""
    return ["env", "ZYPP_LOCK_TIMEOUT=" + str(LOCK_WAIT_SECONDS), find_program("zypper").path] + options

def invalid(field, rule):
    """Returns the result for an input that breaks a rule."""
    return {"status": "invalid_inputs", "reason": MODULE + "." + field + ": " + rule}

def verb_for_this_run(past, infinitive):
    """Returns the verb of a reason: what the run did, or in a dryrun what a
    run would do."""
    return "would " + infinitive if is_dryrun() else past

def unescaped(text):
    """Returns text from zypper's XML output with its entities replaced."""
    return text.replace("&apos;", "'").replace("&quot;", '"').replace("&lt;", "<").replace("&gt;", ">").replace("&amp;", "&")

def attribute(line, key):
    """Returns the value of an attribute of an XML tag on one line, or an
    empty text."""
    marker = " " + key + '="'
    start = line.find(marker)
    if start < 0:
        return ""
    rest = line[start + len(marker):]
    return unescaped(rest[:rest.find('"')])

def problems_in(output):
    """Returns the texts of zypper's XML output that name a problem: the
    messages of type error and warning, and the descriptions of prompts, such
    as the list of files that two packages share. A text can span lines."""
    found = []
    text = None
    closing = ""
    in_prompt = False
    shared_file = ""
    first_owner = ""
    for line in output.splitlines():
        if text == None:
            # A file that two packages of the transaction own: the lhs line
            # holds the file and the first package, the rhs line the second.
            if line.startswith("<lhs><file>") or line.startswith("<rhs><file>"):
                owner = line.split("<name>")[1].split("</name>")[0] if "<name>" in line else "?"
                if line.startswith("<lhs>"):
                    shared_file, first_owner = unescaped(line.split("<file>")[1].split("</file>")[0]), owner
                else:
                    found.append("the file " + shared_file + " is in both " + first_owner + " and " + owner + ", with different content")
                continue
            if line.startswith("<prompt"):
                in_prompt = True
            elif line.startswith("</prompt>"):
                in_prompt = False
            for marker in ['<message type="error">', '<message type="warning">']:
                if line.startswith(marker):
                    text, closing, line = [], "</message>", line.removeprefix(marker)
            if text == None and in_prompt and line.startswith("<description>"):
                text, closing, line = [], "</description>", line.removeprefix("<description>")
            if text == None:
                continue
        if line.endswith(closing):
            text.append(line.removesuffix(closing))
            found.append(" / ".join([l.strip() for l in unescaped("\n".join(text)).splitlines() if l.strip()]))
            text = None
        else:
            text.append(line)
    # zypper ends its error messages with a sentence about an aborted
    # installation that says nothing more.
    return [f for f in found if not f.startswith("Problem occurred during or after")] or found

def message_of(command):
    """Returns zypper's own message of a command that failed."""
    if command.error:
        return command.error
    text = " / ".join(problems_in(command.standard_output))
    if not text:
        text = command.standard_error.strip()
    if len(text) > 700:
        text = text[:700] + "..."
    if not text:
        text = "zypper ended with exit code " + str(command.exit_code) + " and printed no message"
    if command.exit_code == EXIT_LOCKED:
        text = "another zypper still holds its lock " + LOCK_FILE + " after " + str(LOCK_WAIT_SECONDS) + " seconds: " + text
    return text

def name_problem(name):
    """Returns the rule that a package name breaks, or an empty text."""
    if name == "":
        return "must not be empty"
    if name.startswith("-") or name.startswith("."):
        return "must not start with a hyphen or a dot"
    for character in name.elems():
        if character not in NAME_CHARACTERS:
            return "may hold only letters, digits, and . _ + -; a version goes in version, and a pattern, a capability, and a repository in front of the name are not accepted"
    return ""

def version_problem(version):
    """Returns the rule that a version breaks, or an empty text."""
    if version == "":
        return "must not be empty"
    if version in [NEWEST_WHEN_FIRST_INSTALLED, NEWEST_ON_EVERY_RUN]:
        return ""
    if version.startswith("-"):
        return "must not start with a hyphen"
    for character in version.elems():
        if character not in VERSION_CHARACTERS:
            return "may hold only letters, digits, and . _ + ~ ^ : -; zypper's operators such as >= are not accepted"
    return ""

def check_inputs(inputs):
    """Returns the result for inputs that break a rule that the description
    cannot state, or None."""
    entries = inputs.get("packages")
    upgrade_all = inputs.get("upgrade_all_packages", False)
    state = inputs.get("state", "present")
    if upgrade_all:
        for other in ["packages", "state", "also_remove_packages_that_depend_on_these"]:
            if other in inputs:
                return invalid("upgrade_all_packages", "takes no other input; remove " + other)
        return None
    if entries == None:
        return invalid("packages", "is required, or upgrade_all_packages: true")
    if type(entries) != "list" or not entries:
        return invalid("packages", "must be a list of entries" if entries else "must not be empty")
    seen = {}
    for entry in entries:
        if type(entry) != "dict" or "name" not in entry or type(entry["name"]) != "string":
            return invalid("packages", "each entry is a mapping with a name, such as {name: nginx, version: newest_when_first_installed}")
        name = entry["name"]
        problem = name_problem(name)
        if problem:
            return invalid("packages", name + " " + problem if name else problem)
        if name in seen:
            return invalid("packages", name + " is listed twice")
        seen[name] = True
        for key in entry:
            if key not in ["name", "version"]:
                return invalid("packages", name + " has the unknown key " + key + "; an entry holds name and version")
        if state == "absent":
            if "version" in entry:
                return invalid("packages", name + ": state: absent accepts only name; remove version")
        elif type(entry.get("version")) != "string":
            return invalid("packages", name + ": version is required: " + NEWEST_WHEN_FIRST_INSTALLED + ", " + NEWEST_ON_EVERY_RUN + ", or a version")
        else:
            problem = version_problem(entry["version"])
            if problem:
                return invalid("packages", name + ": version " + problem)
    if inputs.get("also_remove_packages_that_depend_on_these") and state != "absent":
        return invalid("also_remove_packages_that_depend_on_these", "requires state: absent")
    return None

def joined_versions(versions):
    """Returns the versions of one name as one text: a package can be
    installed in several versions (kernels) or for several architectures."""
    return ", ".join(sorted({version: True for version in versions}))

def read_installed():
    """Returns the installed packages, as a dictionary of versions by name,
    and an empty text; or None and what went wrong. rpm's database is the one
    that zypper changes. A version is rpm's [epoch:]version-release, which is
    zypper's edition."""
    answer = run_command(["rpm", "-qa", "--queryformat", "%{NAME}\\t%{EVR}\\n"])
    if answer.error or answer.exit_code != 0:
        return None, message_of(answer)
    found = {}
    for line in answer.standard_output.splitlines():
        fields = line.split("\t")
        if len(fields) != 2 or not fields[0] or not fields[1]:
            return None, "rpm printed a line that is not a package name and a version: " + line[:200]
        found.setdefault(fields[0], []).append(fields[1])
    return {name: joined_versions(versions) for name, versions in found.items()}, ""

def preview(command_name, names = []):
    """Asks zypper what a transaction would do, without a change
    (--dry-run). Returns four values: the summary, which holds the packages
    to install, upgrade, downgrade, and remove, each with its versions; the
    names that zypper does not know; what else went wrong; and the summary's
    text. The dry run resolves names and dependencies and a conflict between
    packages' declarations. It downloads nothing, so it checks neither the
    files that two packages share nor the signature of a package: zypper finds
    these when the transaction runs."""
    command = run_command(zypper_command() + [command_name, "--dry-run"] + names)
    if command.error:
        return None, [], command.error
    if command.exit_code == EXIT_NAME_NOT_FOUND:
        not_found = []
        for line in command.standard_output.splitlines():
            marker = " not found in package names"
            if line.startswith('<message type="info">') and marker in line:
                not_found.append(unescaped(line.removeprefix('<message type="info">').split(marker)[0]).strip("'"))
        if not_found:
            return None, not_found, ""
        return None, [], message_of(command)
    if command.exit_code != EXIT_OK:
        return None, [], message_of(command)
    # The parser expects zypper's plain section tags on lines of their own,
    # such as <to-install> and </to-install>, and one <solvable ...> tag on
    # one line, as zypper 1.14 prints them. It ignores every other section.
    # notes holds zypper's remark that a newer version exists only from
    # another vendor, which zypper update and zypper install never take.
    summary = {"to-install": {}, "to-upgrade": {}, "to-downgrade": {}, "to-remove": {}, "notes": []}
    section = ""
    for line in command.standard_output.splitlines():
        if line.startswith('<message type="info">There is an update candidate'):
            summary["notes"].append(unescaped(line.removeprefix('<message type="info">').removesuffix("</message>")))
        if line.startswith("</to-"):
            section = ""
        elif line.startswith("<to-"):
            section = line.removeprefix("<").removesuffix(">")
        elif line.startswith("<solvable ") and section in summary and section != "notes":
            name, edition = attribute(line, "name"), attribute(line, "edition")
            if not name or not edition:
                return None, [], "zypper printed a package that has no name or no edition: " + line[:200]
            summary[section].setdefault(name, []).append(edition)
    return summary, [], ""

def predicted_after(installed, summary):
    """Returns the installed packages as the previewed transaction would leave
    them."""
    after = dict(installed)
    for name in summary["to-remove"]:
        after.pop(name, None)
    for name, editions in summary["to-install"].items():
        after[name] = joined_versions(editions + ([after[name]] if name in after else []))
    for name, editions in list(summary["to-upgrade"].items()) + list(summary["to-downgrade"].items()):
        after[name] = joined_versions(editions)
    return after

def not_packages_of_their_own(names):
    """Returns the result for names that zypper resolves to other packages or
    reads as a package's name with a version or an architecture. zypper
    installs and removes such a name under other names, so a task that named
    it would find it missing on every run, or a removal would report
    unchanged while the package stays."""
    return invalid("packages", ", ".join(names) + " is a name that another package provides, or a package's name with a version or an architecture, not a package of its own; name each package itself")

def named_in_other_ways(names):
    """Returns the names, among those not installed under their own name, that
    zypper still resolves to an installed package: rpm -q finds a package's
    name with its version or architecture (tree-2.2.1, tree.x86_64), and rpm
    -q --whatprovides finds a name that an installed package provides.
    Both exit with 1 for a name that nothing installed matches. Returns the
    names and an empty text, or None and what went wrong."""
    found = []
    for name in names:
        for question in [["rpm", "-q", name], ["rpm", "-q", "--whatprovides", name]]:
            answer = run_command(question)
            if answer.error:
                return None, answer.error
            if answer.exit_code == 0:
                found.append(name)
                break
            if answer.exit_code != 1:
                return None, message_of(answer)
    return found, ""

def refresh_the_package_index():
    """Refreshes the repositories' metadata, in a run; a dryrun refreshes
    nothing. Returns None, or the failed result."""
    if is_dryrun():
        return None
    refreshed = run_command_that_refreshes_a_package_index(zypper_command(["--non-interactive", "--xmlout"]) + ["refresh"])
    if refreshed.error or refreshed.exit_code != EXIT_OK:
        return {"status": "failed", "reason": MODULE + ": refreshing the repositories: " + message_of(refreshed)}
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
    "installed nginx 1.28.1-1.1; upgraded glibc from 2.42-1.1 to 2.42-2.1"."""
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

def succeeded_with(names, versions, changes, advice = "", notes = []):
    """Returns the result of a task that succeeded. The fingerprint holds
    the version of each package of the task, or absent, so that a node task
    block's service restarts when one of them changed."""
    state = {}
    for name in sorted(names):
        state[name] = versions.get(name, "absent")
    result = {"outputs": {"changed_packages": changes}, "fingerprint": json.encode(state)}
    if changes:
        result["reason"] = describe(changes) + advice
    if notes:
        result["reason"] = result.get("reason", "") + ("; " if changes else "") + "not updated: " + " ".join(notes)
    return result

def has_version(installed_versions, wanted):
    """Returns whether one of the installed versions of a package is the
    wanted one, given with or without its release."""
    for version in installed_versions.split(", "):
        if version == wanted or version.rsplit("-", 1)[0] == wanted:
            return True
    return False

def run_transaction(command_arguments, description, names, installed_before, predicted):
    """Runs the zypper command that changes packages, and returns the failure
    or None, the result or None, and the installed packages after it. A
    dryrun reports the versions that the preview predicted; a run reads the
    installed packages again and reports what zypper did."""
    # In a dryrun MMX does not run the command. The call is still what makes
    # the dryrun report would_change and show "would run: zypper ..." in
    # the difference; a script never reports its change itself.
    command = run_command_that_changes_the_node(zypper_command() + command_arguments)
    if command.error or (command.exit_code not in CODES_OF_A_TRANSACTION_THAT_WORKED and command.exit_code != EXIT_SCRIPT_FAILED):
        return {"status": "failed", "reason": MODULE + ": " + description + ": " + message_of(command)}, None, None
    installed_after = predicted
    if not is_dryrun():
        installed_after, problem = read_installed()
        if problem:
            # Another try would find the packages as the task wants them and
            # report an unchanged task, so this failure stays.
            return {"status": "failed", "reason": MODULE + ": " + description + " ended, and then reading the installed packages failed: " + problem, "try_again": False}, None, None
    if command.exit_code == EXIT_SCRIPT_FAILED:
        # The packages are installed, so another try reports an unchanged
        # task and hides the failure of the script.
        return {
            "status": "failed",
            "try_again": False,
            "reason": MODULE + ": " + description + ": the packages changed, and a script of a package failed (exit code " + str(EXIT_SCRIPT_FAILED) + "): " + message_of(command) +
                      "; " + describe(changes_between(installed_before, installed_after)),
        }, None, None
    advice = ""
    if command.exit_code == EXIT_REBOOT_NEEDED:
        advice = "; zypper advises a reboot"
    return None, succeeded_with(names, installed_after, changes_between(installed_before, installed_after), advice), installed_after

def install(entries, installed):
    """Installs the packages that are missing, the packages that must be at an
    exact version, and the packages that must be the newest."""
    requests = []
    must_change = {}
    for entry in entries:
        name, version = entry["name"], entry["version"]
        if name not in installed:
            requests.append(name if version in [NEWEST_WHEN_FIRST_INSTALLED, NEWEST_ON_EVERY_RUN] else name + "=" + version)
        elif version == NEWEST_ON_EVERY_RUN:
            requests.append(name)
        elif version != NEWEST_WHEN_FIRST_INSTALLED and not has_version(installed[name], version):
            requests.append(name + "=" + version)
            must_change[name] = version
    names = [entry["name"] for entry in entries]
    if not requests:
        return succeeded_with(names, installed, [])
    # Names that an installed package provides are refused before anything
    # runs: zypper would install nothing for them and the task would try
    # again on every run.
    missing = [entry["name"] for entry in entries if entry["name"] not in installed]
    provided, problem = named_in_other_ways(missing)
    if problem:
        return {"status": "failed", "reason": MODULE + ": looking for the packages that provide " + ", ".join(missing) + ": " + problem}
    if provided:
        return not_packages_of_their_own(provided)
    failure = refresh_the_package_index()
    if failure:
        return failure
    summary, not_found, problem = preview("install", requests)
    if not_found and is_dryrun():
        return {
            "status": "not_checked",
            "reason": MODULE + ": the repositories the node has now offer no " + ", ".join(not_found) +
                      "; a run refreshes them first, and an earlier node task may add the repository that has it",
        }
    if not_found:
        return invalid("packages", "the repositories offer no " + ", ".join(not_found) +
                       "; zypper search finds packages, and a repository of your own comes from a zypper_repository task before this one")
    if problem:
        return {"status": "failed", "reason": MODULE + ": previewing the installation of " + ", ".join(requests) + ": " + problem}
    # A missing name that zypper does not install is a name that other
    # packages provide, such as a capability that a repository's package
    # provides.
    not_their_own = [name for name in missing if name not in summary["to-install"]]
    if not_their_own:
        return not_packages_of_their_own(not_their_own)
    # An exact version that is older than the installed one is no upgrade:
    # zypper does nothing for it, with exit code 0, and downgrades only with
    # --oldpackage, which this module never passes.
    older = [name for name in must_change if name not in summary["to-upgrade"] and name not in summary["to-install"]]
    if older:
        return invalid("packages", ", ".join(older) + " is installed at " + "; ".join([installed[name] for name in older]) +
                       ", a version that zypper does not replace by the version asked for; this module never downgrades")
    if not summary["to-install"] and not summary["to-upgrade"] and not summary["to-downgrade"] and not summary["to-remove"]:
        return succeeded_with(names, installed, [], "", summary["notes"])
    failure, result, after = run_transaction(["install"] + requests, "installing " + ", ".join(requests), names, installed, predicted_after(installed, summary))
    if failure:
        return failure
    if not is_dryrun():
        # zypper ended without an error; the packages must be as the task wants them.
        not_as_wanted = [entry["name"] for entry in entries if entry["name"] not in after or (entry["version"] not in [NEWEST_WHEN_FIRST_INSTALLED, NEWEST_ON_EVERY_RUN] and not has_version(after[entry["name"]], entry["version"]))]
        if not_as_wanted:
            return {"status": "failed", "reason": MODULE + ": installing " + ", ".join(requests) + " ended without an error, and " + ", ".join(not_as_wanted) + " is not installed at the wanted version; " + describe(result["outputs"]["changed_packages"])}
    return result

def remove(entries, installed, also_dependents):
    """Removes the packages that are installed, and with also_dependents the
    packages that depend on them. zypper removes the packages that depend on
    the named ones by itself, so the task previews the removal first."""
    names = [entry["name"] for entry in entries]
    provided, problem = named_in_other_ways([name for name in names if name not in installed])
    if problem:
        return {"status": "failed", "reason": MODULE + ": looking for the packages that provide the names: " + problem}
    if provided:
        return not_packages_of_their_own(provided)
    present = [name for name in names if name in installed]
    if not present:
        return succeeded_with(names, installed, [])
    summary, _, problem = preview("remove", present)
    if problem:
        return {"status": "failed", "reason": MODULE + ": previewing the removal of " + ", ".join(present) + ": " + problem}
    dependents = sorted([name for name in summary["to-remove"] if name not in names])
    if dependents and not also_dependents:
        return {
            "status": "invalid_inputs",
            "reason": MODULE + ": removing " + ", ".join(present) + " would also remove " + ", ".join(dependents) +
                      ", which depend on them; to remove these packages too, add also_remove_packages_that_depend_on_these: true",
        }
    failure, result, _ = run_transaction(["remove"] + present, "removing " + ", ".join(present), names, installed, predicted_after(installed, summary))
    if failure:
        return failure
    return result

def upgrade_all(installed):
    """Updates every package that has a newer version, as zypper update does:
    zypper keeps each package's vendor and architecture, and the locks that
    zypper addlock made."""
    failure = refresh_the_package_index()
    if failure:
        return failure
    summary, _, problem = preview("update")
    if problem:
        return {"status": "failed", "reason": MODULE + ": previewing the update of all packages: " + problem}
    if not summary["to-install"] and not summary["to-upgrade"] and not summary["to-downgrade"] and not summary["to-remove"]:
        return succeeded_with([], installed, [], "", summary["notes"])
    failure, result, _ = run_transaction(["update"], "updating all packages", [], installed, predicted_after(installed, summary))
    if failure:
        return failure
    return result

def run_node_task(inputs):
    failure = check_inputs(inputs)
    if failure:
        return failure
    location = find_program("zypper")
    if location.error:
        return {"status": "failed", "reason": MODULE + ": looking for the program zypper: " + location.error}
    if not location.found:
        return {
            "status": "missing_prerequisite",
            "reason": MODULE + ": the program zypper is not installed; zypper_package works only on a node whose package manager is zypper, " +
                      "such as openSUSE; give the node task a condition on the node fact package_manager",
        }
    installed, problem = read_installed()
    if problem:
        return {"status": "failed", "reason": MODULE + ": reading the installed packages: " + problem}
    if inputs.get("upgrade_all_packages", False):
        return upgrade_all(installed)
    if inputs.get("state", "present") == "absent":
        return remove(inputs["packages"], installed, inputs.get("also_remove_packages_that_depend_on_these", False))
    return install(inputs["packages"], installed)
