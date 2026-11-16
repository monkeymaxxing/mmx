# pgbackrest_restore restores a PostgreSQL cluster with pgBackRest into an
# empty or missing data directory, to the end of the archive or to a chosen
# time, starts it, and waits until its recovery ends. It runs pgbackrest,
# pg_ctl, and psql as the account postgres with the task's configuration
# file. A cluster of Debian's own tools (pg_lsclusters), whose configuration
# is in /etc/postgresql, starts through its systemd service; any other on
# its local socket only. MMX records each restore's target on the node, and
# a later run with another target fails.
# docs/changing-mmx/design/modules-official-extra.md ("pgBackRest") owns the behavior.

MODULE = "pgbackrest_restore"

# ---------------------------------------------------------------------------
# The same in pgbackrest_stanza and pgbackrest_restore. A script cannot load
# another file, so each script holds its own copy; keep the copies
# identical.
# ---------------------------------------------------------------------------

# The account that runs pgbackrest, pg_ctl, and psql, and owns the data
# directories: PostgreSQL's server package creates it.
ACCOUNT = "postgres"

def pgbackrest(inputs, arguments):
    """Returns the command line that runs pgbackrest as the account postgres
    with the task's configuration file and stanza."""
    return ["runuser", "-u", ACCOUNT, "--", "pgbackrest", "--config=" + inputs["config_file"], "--stanza=" + inputs["stanza"]] + arguments

def invalid(field, rule):
    """Returns the result for an input that breaks a rule."""
    return {"status": "invalid_inputs", "reason": MODULE + "." + field + ": " + rule}

def failed(reason):
    return {"status": "failed", "reason": MODULE + ": " + reason}

def did(past, infinitive, thing):
    """Returns one part of a reason: what the run did, or in a dryrun what a
    run would do."""
    return ("would " + infinitive if is_dryrun() else past) + " " + thing

def missing_program(programs):
    """Returns the result for the first program that is not on the node, or
    None when all are there. programs is a list of (program, package)."""
    for program, package in programs:
        location = find_program(program)
        if location.error:
            return failed("looking for the program " + program + ": " + location.error)
        if not location.found:
            return {"status": "missing_prerequisite",
                    "reason": MODULE + ": the program " + program + " is not installed; install the package " + package + " in an earlier node task"}
    return None

def message_of(command):
    """Returns pgBackRest's own message of a command that failed: its ERROR
    line with the HINT lines below it. Other programs give their last line
    of standard error."""
    if command.error:
        return command.error
    lines = [line.strip() for line in (command.standard_error + "\n" + command.standard_output).splitlines() if line.strip()]
    for position, line in enumerate(lines):
        if " ERROR: " in line:
            message = " ".join([line[line.index(" ERROR: ") + 1:]] + [text for text in lines[position + 1:] if text.startswith("HINT:")])
            return message[:700]
    if lines:
        return lines[-1][:700]
    return "the command ended with exit code " + str(command.exit_code) + " and printed no message"

def common_input_problem(inputs):
    """Returns the result for a stanza or a configuration file that breaks a
    rule, or None."""
    stanza = inputs["stanza"]
    if stanza == "" or stanza.startswith("-") or [c for c in stanza.elems() if c in " /\t\n\r"]:
        return invalid("stanza", "must be a stanza's name, such as main: not empty, without a slash or white space, and not starting with a hyphen (-)")
    if not inputs["config_file"].startswith("/"):
        return invalid("config_file", "must be an absolute path, such as /etc/pgbackrest/pgbackrest.conf")
    return None

def configuration_problem(inputs):
    """Returns the result for a configuration file that is missing, or that an
    earlier node task of this dryrun writes, or None."""
    path = inputs["config_file"]
    if is_dryrun() and earlier_node_task_would_change(path):
        return {"status": "not_checked",
                "reason": MODULE + ": an earlier node task writes the configuration file " + path + "; a dryrun cannot read the stanza through the file that a run uses"}
    found = inspect_path(path)
    if found.error:
        return failed("inspecting " + path + ": " + found.error)
    if not found.exists:
        return {"status": "missing_prerequisite",
                "reason": MODULE + ": the configuration file " + path + " does not exist; write it with a file task in an earlier node task"}
    return None

def read_stanza(inputs):
    """Reads the stanza with pgbackrest info --output=json, which ends with
    exit code 0 also when the stanza does not exist. Returns its entry and
    None, or None and the failed result."""
    shown = run_command(pgbackrest(inputs, ["info", "--output=json"]))
    if shown.error or shown.exit_code != 0:
        return None, failed("reading the stanza %s with pgbackrest info: %s" % (inputs["stanza"], message_of(shown)))
    entries = json.decode(shown.standard_output, default = None)
    if type(entries) != "list" or len(entries) != 1 or type(entries[0]) != "dict" or type(entries[0].get("repo")) != "list":
        return None, failed("pgbackrest info --output=json printed no list with the stanza: " + shown.standard_output[:300])
    return entries[0], None

def repository_codes(entry):
    """The status code of each repository, as pgbackrest info gives it: 0
    ok, 1 missing stanza path, 2 no valid backups, 3 missing stanza data."""
    return [repository.get("status", {}).get("code") for repository in entry["repo"] if type(repository) == "dict"]

def current_database(entry):
    """The stanza's current PostgreSQL cluster: the entry of db with the
    highest id, or None."""
    current = None
    for database in entry.get("db", []):
        if type(database) == "dict" and (current == None or database.get("id", 0) > current.get("id", 0)):
            current = database
    return current

def partial_stanza(inputs):
    """The result for a stanza with the archive's information and without the
    backup information, as after a stanza-create that stopped partway."""
    stanza = inputs["stanza"]
    return {"status": "invalid_inputs",
            "reason": ("%s: the stanza %s has the archive's information and no backup information in a repository (missing stanza data, pgBackRest's " +
                       "error 55), as after a stanza-create that stopped partway or after the backup information was deleted. A stanza that holds no " +
                       "backup yet can be deleted: pgbackrest --stanza=%s stop, pgbackrest --stanza=%s stanza-delete, pgbackrest --stanza=%s start; " +
                       "a repository whose backups were deleted is restored with pgbackrest_restore and read_repository_as_of") % (MODULE, stanza, stanza, stanza, stanza)}

def ini_value(text, sections, key):
    """Returns the value of key in the first of sections that has it, in a
    file of pgBackRest's INI form, or an empty text."""
    values = {}
    section = None
    for line in text.splitlines():
        line = line.strip()
        if line.startswith("[") and line.endswith("]"):
            section = line[1:-1].strip()
            continue
        name, equals, value = line.partition("=")
        if equals and section in sections and name.strip() == key and (section, key) not in values:
            values[(section, key)] = value.strip()
    for section in sections:
        if (section, key) in values:
            return values[(section, key)]
    return ""

# ---------------------------------------------------------------------------
# This script's own part.
# ---------------------------------------------------------------------------

# Where Debian and Ubuntu install the programs of each PostgreSQL version.
POSTGRESQL_PROGRAMS = "/usr/lib/postgresql/%s/bin"

# pgBackRest reads a repository as it stood at a time since 2.54.
OLDEST_PGBACKREST_FOR_READ_REPOSITORY_AS_OF = (2, 54)

# How long the module waits for the restored cluster to end its recovery, in
# seconds. The replay of a long archive takes long; a run that ends here
# fails, and the next run waits again for the same cluster.
LONGEST_RECOVERY_WAIT = 6 * 3600

# Where MMX records the target of each restore that the module did: one file
# for each data directory, named for the SHA-256 of its path. Not in the data
# directory, from which pg_basebackup and pgBackRest would copy the record
# to another server with the cluster.
RESTORE_TARGET_RECORDS = "/var/lib/mmx/pgbackrest_restore_targets"

def time_problem(text):
    """Returns the rule that a time breaks, or an empty text."""
    if seconds_since_1970(text) == None:
        return "must be a time with its time zone, such as 2026-11-03 13:01:00+00"
    return ""

def seconds_since_1970(text):
    """Reads a time "YYYY-MM-DD HH:MM[:SS[.fraction]]" followed by a time
    zone "+HH", "-HH", "+HH:MM", or "Z", as seconds since 1970 in UTC, or
    None for another text."""
    date, _, rest = text.strip().partition(" ")
    parts = date.split("-")
    if len(parts) != 3 or len(parts[0]) != 4 or len(parts[1]) != 2 or len(parts[2]) != 2 or not (parts[0] + parts[1] + parts[2]).isdigit():
        return None
    year, month, day = int(parts[0]), int(parts[1]), int(parts[2])
    zone = 0
    if rest.endswith("Z"):
        clock = rest[:-1]
    else:
        position = max(rest.rfind("+"), rest.rfind("-"))
        if position < 0:
            return None
        clock, offset = rest[:position], rest[position + 1:]
        hours, _, minutes = offset.partition(":")
        if len(hours) != 2 or not hours.isdigit() or (minutes and (len(minutes) != 2 or not minutes.isdigit())):
            return None
        zone = (int(hours) * 60 + (int(minutes) if minutes else 0)) * 60
        if rest[position] == "-":
            zone = -zone
    fields = clock.split(":")
    if len(fields) not in [2, 3]:
        return None
    if len(fields) == 3:
        fields[2] = fields[2].partition(".")[0]
    for field in fields:
        if len(field) != 2 or not field.isdigit():
            return None
    hour, minute = int(fields[0]), int(fields[1])
    second = int(fields[2]) if len(fields) == 3 else 0
    if month < 1 or month > 12 or day < 1 or day > 31 or hour > 23 or minute > 59 or second > 60:
        return None
    # Days since 1970-01-01 of a date of the proleptic Gregorian calendar.
    if month <= 2:
        year -= 1
    era = year // 400
    of_era = year - era * 400
    of_year = (153 * (month + (-3 if month > 2 else 9)) + 2) // 5 + day - 1
    days = era * 146097 + of_era * 365 + of_era // 4 - of_era // 100 + of_year - 719468
    return days * 86400 + hour * 3600 + minute * 60 + second - zone

def check_inputs(inputs):
    problem = common_input_problem(inputs)
    if problem:
        return problem
    directory = inputs["data_directory"]
    if not directory.startswith("/") or directory == "/" or directory.endswith("/"):
        return invalid("data_directory", "must be an absolute path without a slash at its end, such as /var/lib/postgresql/18/main")
    recover_to = inputs["recover_to"]
    if recover_to != "latest" and time_problem(recover_to):
        return invalid("recover_to", "must be latest or " + time_problem(recover_to)[len("must be "):])
    if "read_repository_as_of" in inputs:
        if time_problem(inputs["read_repository_as_of"]):
            return invalid("read_repository_as_of", time_problem(inputs["read_repository_as_of"]))
        if "repository" not in inputs:
            return invalid("repository", "is required with read_repository_as_of: pgBackRest reads one repository as it stood then")
    if "repository" in inputs and (inputs["repository"] < 1 or inputs["repository"] > 256):
        return invalid("repository", "must be the number of a repository in the configuration file, such as 1")
    return None

def version_numbers(text):
    """Reads "pgBackRest 2.58.0" as (2, 58), or None."""
    words = text.strip().split()
    if len(words) != 2 or words[0] != "pgBackRest":
        return None
    parts = words[1].split(".")
    if len(parts) < 2 or not parts[0].isdigit() or not parts[1].isdigit():
        return None
    return (int(parts[0]), int(parts[1]))

def entries_of(directory):
    """Returns the names in a directory and None, or None and the failed
    result."""
    listed = run_command(["find", directory, "-mindepth", "1", "-maxdepth", "1", "-printf", "%f\\n"])
    if listed.error or listed.exit_code != 0:
        return None, failed("listing " + directory + ": " + message_of(listed))
    return [name for name in listed.standard_output.split("\n") if name], None

def restored_from_the_stanza(inputs):
    """Whether the cluster in the data directory was restored by pgBackRest
    from the task's stanza: its postgresql.auto.conf holds the
    restore_command that pgBackRest's restore writes, with the stanza.
    PostgreSQL keeps the line when ALTER SYSTEM rewrites the file."""
    read = read_file(inputs["data_directory"] + "/postgresql.auto.conf")
    if read.error:
        return False
    for line in read.content.splitlines():
        line = line.strip()
        if line.startswith("restore_command") and "pgbackrest " in line and "--stanza=" + inputs["stanza"] + " " in line:
            return True
    return False

def target_of(inputs):
    """The target of the task's restore, as MMX records it."""
    return {"stanza": inputs["stanza"], "recover_to": inputs["recover_to"], "read_repository_as_of": inputs.get("read_repository_as_of")}

def described(target):
    """The target in the words of a reason: "the stanza main to ..."."""
    text = "the stanza %s to %s" % (target["stanza"], "the end of its archive (latest)" if target["recover_to"] == "latest" else target["recover_to"])
    if target["read_repository_as_of"] != None:
        text += ", reading the repository as it stood at " + target["read_repository_as_of"]
    return text

def record_path(directory):
    return RESTORE_TARGET_RECORDS + "/" + sha256(directory) + ".json"

def recorded_target(directory):
    """Returns the target that MMX recorded for the module's restore into
    the directory, or None when it has no record, and the failed result or
    None."""
    path = record_path(directory)
    found = inspect_path(path)
    if found.error:
        return None, failed("inspecting MMX's record of the restore into %s, %s: %s" % (directory, path, found.error))
    if not found.exists:
        return None, None
    read = read_file(path)
    record = json.decode(read.content, default = None) if not read.error else None
    if type(record) != "dict" or record.get("data_directory") != directory:
        return None, failed("MMX's record of the restore into %s, %s, holds no target of that directory: %s" % (directory, path, read.error or read.content[:300]))
    return {key: record.get(key) for key in ["stanza", "recover_to", "read_repository_as_of"]}, None

def record_the_target(inputs):
    """Records the target of the task's restore. Returns what failed, or an
    empty text."""
    made = make_directory(RESTORE_TARGET_RECORDS, owner = "root", group = "root", permissions_mode = "0700")
    if made.error:
        return "making the directory %s for MMX's record of the restore: %s" % (RESTORE_TARGET_RECORDS, made.error)
    record = dict(target_of(inputs), data_directory = inputs["data_directory"])
    path = record_path(inputs["data_directory"])
    written = write_file(path, json.encode(record) + "\n", owner = "root", group = "root", permissions_mode = "0600")
    if written.error:
        return "writing MMX's record of the restore, %s: %s" % (path, written.error)
    return ""

def now():
    """Seconds since 1970 on the node's clock, or None."""
    asked = run_command(["date", "+%s"])
    if asked.error or asked.exit_code != 0 or not asked.standard_output.strip().isdigit():
        return None
    return int(asked.standard_output.strip())

def took(start, end):
    if start == None or end == None:
        return ""
    return " in %d seconds" % (end - start)

def chosen_backup(entry, target):
    """The backup that pgBackRest restores for the target, as seconds since
    1970 or None for the end of the archive: the newest backup that ended
    before the target. Returns its label, or None."""
    chosen = None
    for backup in entry.get("backup", []):
        if type(backup) != "dict":
            continue
        stop = backup.get("timestamp", {}).get("stop", 0)
        if target != None and stop > target:
            continue
        if chosen == None or stop > chosen.get("timestamp", {}).get("stop", 0):
            chosen = backup
    return chosen.get("label") if chosen else None

def run_node_task(inputs):
    rejected = check_inputs(inputs)
    if rejected:
        return rejected
    version = str(inputs["postgresql_major_version"])
    binaries = POSTGRESQL_PROGRAMS % version
    missing = missing_program([("runuser", "util-linux"), ("pgbackrest", "pgbackrest"), ("find", "findutils"),
                               (binaries + "/pg_ctl", "postgresql-" + version), (binaries + "/psql", "postgresql-client-" + version)])
    if missing:
        return missing
    problem = configuration_problem(inputs)
    if problem:
        return problem
    directory = inputs["data_directory"]
    stanza = inputs["stanza"]
    fingerprint = json.encode(inputs)

    as_of = inputs.get("read_repository_as_of")
    if as_of != None:
        shown = run_command(["pgbackrest", "version"])
        found = version_numbers(shown.standard_output) if not shown.error and shown.exit_code == 0 else None
        if found == None:
            return failed("reading the version of pgBackRest with pgbackrest version: " + message_of(shown))
        if found < OLDEST_PGBACKREST_FOR_READ_REPOSITORY_AS_OF:
            return {"status": "missing_prerequisite",
                    "reason": "%s: pgBackRest %s is installed, and read_repository_as_of needs pgBackRest 2.54 or newer; install a newer package pgbackrest in an earlier node task" %
                              (MODULE, shown.standard_output.strip().split()[-1])}

    if is_dryrun() and earlier_node_task_would_change(directory):
        # Such as a directory task that empties the data directory of a
        # cluster of Debian's tools, after a task that stopped the cluster.
        return {"status": "not_checked",
                "reason": "%s: an earlier node task changes %s; a dryrun cannot see the directory that a run restores into" % (MODULE, directory)}
    found = inspect_path(directory)
    if found.error:
        return failed("inspecting " + directory + ": " + found.error)
    names = []
    if found.exists:
        if found.type != "directory":
            return {"status": "invalid_inputs", "reason": "%s: %s is a %s, not a directory" % (MODULE, directory, found.type)}
        names, failure = entries_of(directory)
        if failure:
            return failure
    if "backup.manifest" in names:
        return {"status": "invalid_inputs",
                "reason": ("%s: %s holds a restore that stopped partway (pgBackRest's backup.manifest); the module restores only into an empty or missing " +
                           "directory and never writes over files: empty the directory, and run the task again") % (MODULE, directory)}
    if "PG_VERSION" in names:
        if not restored_from_the_stanza(inputs):
            return {"status": "invalid_inputs",
                    "reason": ("%s: %s holds a PostgreSQL cluster that pgBackRest did not restore from the stanza %s; the module restores only into an empty " +
                               "or missing directory and never writes over a cluster") % (MODULE, directory, stanza)}
        recorded, failure = recorded_target(directory)
        if failure:
            return failure
        reasons = []
        if recorded == None:
            reasons.append(("pgBackRest restored the cluster in %s from the stanza %s, and MMX has no record of that restore's target in %s: the module " +
                            "cannot compare it with this task's recover_to and read_repository_as_of, and leaves the cluster as it is") %
                           (directory, stanza, RESTORE_TARGET_RECORDS))
        elif recorded != target_of(inputs):
            return {"status": "invalid_inputs",
                    "reason": ("%s: %s holds the module's restore of %s, and the task asks for %s. The module never writes over a cluster: to restore " +
                               "again, stop the cluster, empty %s, and run the task again") % (MODULE, directory, described(recorded), described(target_of(inputs)), directory)}
        if "recovery.signal" not in names:
            result = {"outputs": {"restored": False}, "fingerprint": fingerprint}
            if reasons:
                result["reason"] = reasons[0]
            return result
        # A run restored the cluster and ended before its recovery did: this
        # run starts it where needed and waits.
        return finish_the_recovery(inputs, binaries, reasons, False, None, fingerprint)
    if names:
        shown = ", ".join(sorted(names)[:5]) + (", ..." if len(names) > 5 else "")
        return {"status": "invalid_inputs",
                "reason": "%s: %s holds files (%s) and no PostgreSQL cluster; the module restores only into an empty or missing directory" % (MODULE, directory, shown)}

    target = None if inputs["recover_to"] == "latest" else seconds_since_1970(inputs["recover_to"])
    to = "the end of its archive" if target == None else inputs["recover_to"]
    source = ""
    if as_of == None:
        entry, failure = read_stanza(inputs)
        if failure:
            return failure
        database = current_database(entry)
        if database == None or not entry.get("backup"):
            return {"status": "missing_prerequisite", "reason": "%s: the stanza %s has no backup in its repositories" % (MODULE, stanza)}
        if str(database.get("version")) != version:
            return {"status": "invalid_inputs",
                    "reason": "%s: the stanza %s holds PostgreSQL %s, and postgresql_major_version is %s" % (MODULE, stanza, database.get("version"), version)}
        label = chosen_backup(entry, target)
        if label == None:
            return {"status": "invalid_inputs",
                    "reason": "%s: the stanza %s has no backup that ended before %s, so no restore reaches that time" % (MODULE, stanza, inputs["recover_to"])}
        source = " from the backup " + label
    else:
        source = ", reading the repository %d as it stood at %s" % (inputs["repository"], as_of)

    reasons = []
    if not found.exists:
        parent = directory.rpartition("/")[0] or "/"
        if not inspect_path(parent).exists:
            return {"status": "missing_prerequisite", "reason": "%s: the directory %s, which holds the data directory, does not exist; create it in an earlier node task" % (MODULE, parent)}
    made = make_directory(directory, owner = ACCOUNT, group = ACCOUNT, permissions_mode = "0700")
    if made.error:
        return failed("making the data directory " + directory + ": " + made.error)

    arguments = ["--pg1-path=" + directory, "--archive-mode=off"]
    if "repository" in inputs:
        arguments.append("--repo=%d" % inputs["repository"])
    if as_of != None:
        arguments.append("--repo-target-time=" + as_of)
    if target != None:
        arguments += ["--type=time", "--target=" + inputs["recover_to"], "--target-action=promote"]
    started = now()
    command = run_command_that_changes_the_node(pgbackrest(inputs, arguments + ["restore"]))
    what = "the stanza %s into %s to %s%s" % (stanza, directory, to, source)
    if command.error or command.exit_code != 0:
        return {"status": "failed", "try_again": False, "outputs": {"restored": False},
                "reason": ("%s: restoring %s: %s. A restore that pgBackRest began leaves files in %s, and the module restores only into an empty " +
                           "directory: empty it before the next run") % (MODULE, what, message_of(command), directory)}
    if not command.ran:
        return {"reason": "would restore %s; would start the restored cluster and wait until its recovery ends" % what,
                "outputs": {"restored": True}, "fingerprint": fingerprint}
    reasons.append("restored " + what + took(started, now()))
    problem = record_the_target(inputs)
    if problem:
        return failed_after_the_restore(True, reasons, problem)
    return finish_the_recovery(inputs, binaries, reasons, True, now(), fingerprint)

def postmaster_pid(directory):
    """Returns the lines of the cluster's postmaster.pid, or None when the
    cluster does not run: the process ID, the data directory, the start
    time, the port, the socket directory, the listen addresses, the shared
    memory key, and the status."""
    found = inspect_path(directory + "/postmaster.pid")
    if found.error or not found.exists:
        return None
    read = read_file(directory + "/postmaster.pid")
    if read.error:
        return None
    lines = read.content.split("\n")
    return lines if len(lines) >= 5 else None

def debian_cluster_of(directory):
    """Returns the cluster of Debian's own tools whose data directory is
    the directory, as pg_lsclusters --json lists it, or None, and the
    failed result or None. pg_lsclusters lists a cluster by its
    configuration in /etc/postgresql, also while its data directory is
    empty or missing."""
    if not find_program("pg_lsclusters").found:
        return None, None
    listed = run_command(["pg_lsclusters", "--json"])
    if listed.error or listed.exit_code != 0:
        return None, failed("listing the PostgreSQL clusters with pg_lsclusters --json: " + message_of(listed))
    clusters = json.decode(listed.standard_output, default = None)
    if type(clusters) != "list":
        return None, failed("pg_lsclusters --json printed no list: " + listed.standard_output[:200])
    for cluster in clusters:
        if type(cluster) == "dict" and cluster.get("pgdata") == directory:
            return cluster, None
    return None, None

def finish_the_recovery(inputs, binaries, reasons, restored, started, fingerprint):
    """Starts the restored cluster where it does not run, and waits until
    pg_is_in_recovery() is false: pg_ctl returns while the replay still
    runs, and a query then sees an older database. A cluster of Debian's
    own tools starts through its service postgresql@<version>-<cluster>:
    its postgresql.conf is in /etc/postgresql, not in the data directory,
    and pg_ctl start --pgdata failed for it on the test nodes. Any other
    cluster starts on its local socket only, and the wait asks through the
    socket: the restored configuration can name an address of the server
    that was backed up, such as listen_addresses in pg_autoctl's
    postgresql-auto-failover.conf, and PostgreSQL ended at its start on a
    server without that address. A setting on the command line wins over
    the configuration files."""
    directory = inputs["data_directory"]
    result = {"outputs": {"restored": restored}, "fingerprint": fingerprint}
    started_it = postmaster_pid(directory) == None
    logs = "%s/startup.log and the files in %s/log" % (directory, directory)
    unit = ""
    if started_it:
        cluster, failure = debian_cluster_of(directory)
        if failure:
            return failed_after_the_restore(restored, reasons, failure["reason"][len(MODULE) + 2:])
        if cluster != None:
            unit = "postgresql@%s-%s.service" % (cluster.get("version"), cluster.get("cluster"))
            logs = "journalctl -u %s and %s" % (unit, cluster.get("logfile") or "the cluster's log file")
            command = run_command_that_changes_the_node(["systemctl", "start", unit])
        else:
            # pg_ctl hands the options to a shell, which reads '' as an
            # empty value.
            command = run_command_that_changes_the_node(["runuser", "-u", ACCOUNT, "--", binaries + "/pg_ctl", "start", "--no-wait",
                                                         "--pgdata", directory, "--log", directory + "/startup.log", "--options", "-c listen_addresses=''"])
        if command.error or command.exit_code != 0:
            return failed_after_the_restore(restored, reasons, "starting the restored cluster in %s%s: %s" % (directory, " with " + unit if unit else "", message_of(command)))
        if not command.ran:
            result["reason"] = "; ".join(reasons + ["would start the restored cluster in %s%s and wait until its recovery ends" % (directory, " with " + unit if unit else "")])
            return result
    elif is_dryrun():
        # The cluster runs and replays: a run waits, and changes nothing.
        return result
    if started == None:
        started = now()
    for second in range(LONGEST_RECOVERY_WAIT):
        lines = postmaster_pid(directory)
        if lines == None:
            if second > 5:
                return failed_after_the_restore(restored, reasons, "the restored cluster in %s stopped during its recovery; read %s" % (directory, logs))
        else:
            # The socket directory and the port of postmaster.pid.
            asked = run_command(["runuser", "-u", ACCOUNT, "--", binaries + "/psql", "--no-psqlrc", "--quiet", "--no-align", "--tuples-only",
                                 "--host", lines[4].strip(), "--port", lines[3].strip(), "--dbname", "postgres", "--command", "select pg_is_in_recovery()"])
            if not asked.error and asked.exit_code == 0 and asked.standard_output.strip() == "f":
                if started_it:
                    reasons.append("started the restored cluster%s, whose recovery ended%s" % (" (" + unit + ")" if unit else " on its local socket only", took(started, now())))
                if reasons:
                    result["reason"] = "; ".join(reasons)
                return result
        run_command(["sleep", "1"])
    return failed_after_the_restore(restored, reasons, "the restored cluster in %s is still in recovery after %d hours; the next run waits again" %
                                    (directory, LONGEST_RECOVERY_WAIT // 3600))

def failed_after_the_restore(restored, reasons, what_failed):
    """A failure after the restore: another try would find the restored
    cluster and report restored: false, so the task fails once."""
    before = "; ".join(reasons)
    return {"status": "failed", "try_again": False, "outputs": {"restored": restored},
            "reason": MODULE + ": " + (before + ", and then this failed: " if before else "") + what_failed}
