# pgbackrest_stanza manages one pgBackRest stanza in its repositories: it
# creates a missing stanza and upgrades one of an older PostgreSQL version,
# and checks after either that archiving reaches the repositories. It runs
# pgbackrest as the account postgres with the task's configuration file.
# docs/changing-mmx/design/modules-official-extra.md ("pgBackRest") owns the behavior.

MODULE = "pgbackrest_stanza"

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

def run_node_task(inputs):
    rejected = common_input_problem(inputs)
    if rejected:
        return rejected
    missing = missing_program([("runuser", "util-linux"), ("pgbackrest", "pgbackrest")])
    if missing:
        return missing
    problem = configuration_problem(inputs)
    if problem:
        return problem
    stanza = inputs["stanza"]
    fingerprint = json.encode(inputs)

    read = read_file(inputs["config_file"])
    if read.error:
        return failed("reading " + inputs["config_file"] + ": " + read.error)
    cluster = ini_value(read.content, [stanza, "global"], "pg1-path")
    if not cluster:
        return {"status": "invalid_inputs", "reason": "%s: the configuration file %s gives the stanza %s no pg1-path" % (MODULE, inputs["config_file"], stanza)}

    entry, failure = read_stanza(inputs)
    if failure:
        return failure
    codes = repository_codes(entry)
    if 3 in codes:
        return partial_stanza(inputs)
    unknown = [code for code in codes if code not in [0, 1, 2]]
    if unknown or not codes:
        return failed("pgbackrest info reports the stanza %s as %s" % (stanza, entry.get("status", {}).get("message", "unknown")))

    version_file = read_file(cluster + "/PG_VERSION")
    if version_file.error:
        # An earlier node task may create the cluster.
        return {"status": "missing_prerequisite",
                "reason": "%s: %s, the stanza's pg1-path, holds no PostgreSQL cluster (%s)" % (MODULE, cluster, version_file.error)}
    version = version_file.content.strip()

    change = ""
    if 1 in codes:
        change = "create"
    else:
        database = current_database(entry)
        if database != None and str(database.get("version")) != version:
            change = "upgrade"
    if not change:
        return {"outputs": {"created": False}, "fingerprint": fingerprint}

    for signal in ["standby.signal", "recovery.signal"]:
        found = inspect_path(cluster + "/" + signal)
        if found.error:
            return failed("inspecting " + cluster + ": " + found.error)
        if found.exists:
            # The primary runs the task; after a failover a former replica is
            # the primary.
            return {"status": "missing_prerequisite",
                    "reason": "%s: the PostgreSQL cluster in %s is in recovery (%s), and pgBackRest %ss a stanza only on the primary: run the task on the primary" %
                              (MODULE, cluster, signal, change)}

    created = change == "create"
    command = run_command_that_changes_the_node(pgbackrest(inputs, ["stanza-" + change]))
    if command.error or command.exit_code != 0:
        if not command.error and command.exit_code == 55:
            return partial_stanza(inputs)
        return failed("%sing the stanza %s: %s" % ("creat" if created else "upgrad", stanza, message_of(command)))
    if not command.ran:
        what = "the stanza %s for PostgreSQL %s in %s" % (stanza, version, cluster)
        return {"reason": did("created", "create", what) if created else did("upgraded", "upgrade", what),
                "outputs": {"created": created}, "fingerprint": fingerprint}

    # check switches the write-ahead log and waits until its segment is in
    # every repository, which proves archive_command. An unchanged run does
    # not check, because every check archives one more segment.
    checked = run_command_that_changes_the_node(pgbackrest(inputs, ["check"]))
    if checked.error or checked.exit_code != 0:
        return {"status": "failed", "try_again": False, "outputs": {"created": created},
                "reason": "%s: %s the stanza %s, and then pgbackrest check failed, so archiving does not reach the repository: %s" %
                          (MODULE, "created" if created else "upgraded", stanza, message_of(checked))}
    what = "the stanza %s for PostgreSQL %s in %s, and checked that archiving reaches its repositories" % (stanza, version, cluster)
    return {"reason": ("created " if created else "upgraded ") + what, "outputs": {"created": created}, "fingerprint": fingerprint}
