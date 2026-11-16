# pg_auto_failover_monitor manages the pg_auto_failover monitor of a
# formation on the monitor's server: the monitor's PostgreSQL instance, its
# systemd unit, the password of its login autoctl_node, the formation, and
# which nodes belong to the formation. It runs pg_autoctl and psql as the
# account postgres.
# docs/changing-mmx/design/modules-official-extra.md ("pg_auto_failover") owns the
# behavior.
#
# Every pg_autoctl command names the monitor's data directory, so pg_autoctl
# reaches the monitor through its local socket and needs no password.

MODULE = "pg_auto_failover_monitor"

# ---------------------------------------------------------------------------
# The same in pg_auto_failover_monitor, pg_auto_failover_node, and
# pg_auto_failover_primary. A script cannot load another file, so each script
# holds its own copy; keep the copies identical.
# ---------------------------------------------------------------------------

# pg_autoctl's systemd unit, as pg_autoctl show systemd prints it. One
# instance of pg_autoctl runs on a server.
UNIT_NAME = "pgautofailover.service"
UNIT_FILE = "/etc/systemd/system/pgautofailover.service"

# The account that runs pg_autoctl and psql, and owns the data directories:
# PostgreSQL's server package creates it.
ACCOUNT = "postgres"

# Where Debian and Ubuntu install the programs and the extensions of each
# PostgreSQL version.
POSTGRESQL_PROGRAMS = "/usr/lib/postgresql/%s/bin"
POSTGRESQL_EXTENSIONS = "/usr/share/postgresql/%s/extension"

# The states that the monitor assigns to its formation's primary: alone
# (single), while it waits for a replica to catch up (wait_primary), with
# one (primary), while a new replica joins (join_primary), and while it
# applies new replication settings (apply_settings). They are the states in
# which pg_auto_failover's monitor lets a node take writes
# (CanTakeWritesInState in its source, the same in 2.0 to 2.2). The
# assigned state decides: after a failover, the old primary that died still
# reports its last state, such as primary, while the monitor has assigned
# it another one, such as demoted.
PRIMARY_STATES = ["single", "wait_primary", "primary", "join_primary", "apply_settings"]

# The lines that initdb writes into pg_hba.conf for the local socket and
# the loopback addresses, which pg_autoctl creates with the method trust,
# and the method that each one gets instead: Debian's own defaults. peer
# lets an account of the server in only as the role of its own name, and
# scram-sha-256 asks for a password. The first line's method is the
# script's: plain peer on a node, and peer with an ident map on the
# monitor. Every other line stays as it is, pg_autoctl's own hostssl lines
# included.
INITDB_LINES = [
    (["local", "all", "all"], None),
    (["host", "all", "all", "127.0.0.1/32"], "scram-sha-256"),
    (["host", "all", "all", "::1/128"], "scram-sha-256"),
    (["local", "replication", "all"], "peer"),
    (["host", "replication", "all", "127.0.0.1/32"], "scram-sha-256"),
    (["host", "replication", "all", "::1/128"], "scram-sha-256"),
]

def pg_autoctl(arguments):
    """Returns the command line that runs pg_autoctl as the account
    postgres. runuser gives it the account's home directory, where
    pg_autoctl keeps its configuration and libpq finds ~/.pgpass."""
    return ["runuser", "-u", ACCOUNT, "--", "pg_autoctl"] + arguments

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
    """Returns the tool's own message of a command that failed. pg_autoctl
    writes log lines "HH:MM:SS PID LEVEL text": the message is the text of
    its ERROR and FATAL lines. Other programs give their last line of
    standard error."""
    if command.error:
        return command.error
    texts = []
    last = ""
    for line in command.standard_error.splitlines():
        line = line.strip()
        if not line:
            continue
        last = line
        parts = line.split(None, 3)
        # pg_autoctl's services repeat a failure's lines; each text counts once.
        if len(parts) == 4 and parts[2] in ["ERROR", "FATAL"] and parts[3] not in texts:
            texts.append(parts[3])
    message = "; ".join(texts) or last
    if not message:
        return "the command ended with exit code " + str(command.exit_code) + " and printed no message"
    if len(message) > 600:
        message = message[:600] + " ..."
    return message

def path_problem(path):
    """Returns the rule that a data directory's path breaks, or an empty
    text. pg_autoctl keeps its files under the path as it is written, so
    one directory has one spelling."""
    if not path.startswith("/") or path == "/":
        return "must be an absolute path, such as /var/lib/postgresql/18/main"
    if path.endswith("/") or "//" in path:
        return "must not end with a slash or hold two slashes in a row"
    for part in path.split("/")[1:]:
        if part in [".", ".."]:
            return "must not hold . or .. as a part"
    return ""

def parent_of(path):
    return path.rpartition("/")[0] or "/"

def postgres_home():
    """Returns the home directory of the account postgres and None, or None
    and the result for an account that is missing."""
    account = look_up_account(ACCOUNT)
    if account.error:
        return None, failed("looking up the account postgres: " + account.error)
    if not account.exists:
        return None, {"status": "missing_prerequisite",
                      "reason": MODULE + ": the account postgres does not exist; install the package postgresql-<version> in an earlier node task"}
    return account.home, None

def read_ini(path):
    """Reads an INI file, as pg_autoctl writes its configuration. Returns a
    dictionary of sections, each a dictionary of keys, and None; None and
    None when the file does not exist; or None and the failed result."""
    found = inspect_path(path)
    if found.error:
        return None, failed("inspecting " + path + ": " + found.error)
    if not found.exists:
        return None, None
    read = read_file(path)
    if read.error:
        return None, failed("reading " + path + ": " + read.error)
    sections = {}
    section = None
    for line in read.content.splitlines():
        line = line.strip()
        if not line or line.startswith("#") or line.startswith(";"):
            continue
        if line.startswith("[") and line.endswith("]"):
            section = line[1:-1].strip()
            sections.setdefault(section, {})
            continue
        key, equals, value = line.partition("=")
        if not equals or section == None:
            return None, failed("reading " + path + ": the line " + repr(line) + " is no key = value of a section")
        sections[section][key.strip()] = value.strip()
    return sections, None

def configuration_path(home, data_directory):
    """pg_autoctl's configuration of the instance in data_directory."""
    return home + "/.config/pg_autoctl" + data_directory + "/pg_autoctl.cfg"

def setting(configuration, section, key):
    return configuration.get(section, {}).get(key, "")

def read_unit():
    """Returns the text of pg_autoctl's unit file, or None when there is
    none, and the failed result or None."""
    found = inspect_path(UNIT_FILE)
    if found.error:
        return None, failed("inspecting " + UNIT_FILE + ": " + found.error)
    if not found.exists:
        return None, None
    read = read_file(UNIT_FILE)
    if read.error:
        return None, failed("reading " + UNIT_FILE + ": " + read.error)
    return read.content, None

def data_directory_of_unit(text):
    """Returns the data directory that pg_autoctl's unit runs, from its line
    Environment = 'PGDATA=...', or an empty text."""
    for line in text.splitlines():
        key, equals, value = line.partition("=")
        if equals and key.strip() == "Environment":
            value = value.strip().strip("'\"")
            if value.startswith("PGDATA="):
                return value[len("PGDATA="):]
    return ""

def unit_state():
    """Returns systemd's state of pg_autoctl's unit as a dictionary with
    ActiveState and UnitFileState, and None, or None and the failed
    result."""
    shown = run_command(["systemctl", "show", UNIT_NAME, "--property=ActiveState", "--property=UnitFileState"])
    if shown.error or shown.exit_code != 0:
        return None, failed("asking systemd about " + UNIT_NAME + ": " + message_of(shown))
    state = {}
    for line in shown.standard_output.splitlines():
        key, _, value = line.partition("=")
        state[key.strip()] = value.strip()
    return state, None

def write_the_unit(data_directory, reasons):
    """Writes pg_autoctl's unit as pg_autoctl show systemd prints it for the
    instance in data_directory, and reloads systemd when the file changed.
    Returns the failed result or None. The instance must exist: for a data
    directory without one, pg_autoctl prints nothing, and an empty unit file
    reads as masked."""
    shown = run_command(pg_autoctl(["-q", "show", "systemd", "--pgdata", data_directory]))
    if shown.error or shown.exit_code != 0 or "ExecStart" not in shown.standard_output:
        return failed("reading the unit of the instance in " + data_directory + " with pg_autoctl show systemd: " + message_of(shown))
    written = write_file(UNIT_FILE, shown.standard_output, owner = "root", group = "root", permissions_mode = "0644")
    if written.error:
        return failed("writing " + UNIT_FILE + ": " + written.error)
    if written.changed:
        reloaded = run_command_that_changes_the_node(["systemctl", "daemon-reload"])
        if reloaded.error or reloaded.exit_code != 0:
            return failed("reloading systemd after writing " + UNIT_FILE + ": " + message_of(reloaded))
        reasons.append(did("wrote", "write", "the unit " + UNIT_FILE))
    return None

def start_the_unit(reasons):
    """Enables and starts pg_autoctl's unit where it is not enabled or does
    not run. Returns whether the unit ran before, and the failed result or
    None."""
    state, failure = unit_state()
    if failure:
        return False, failure
    running = state.get("ActiveState") == "active"
    if running and state.get("UnitFileState") == "enabled":
        return True, None
    started = run_command_that_changes_the_node(["systemctl", "enable", "--now", UNIT_NAME])
    if started.error or started.exit_code != 0:
        return running, failed("enabling and starting " + UNIT_NAME + ": " + message_of(started))
    if running:
        reasons.append(did("enabled", "enable", UNIT_NAME + " at boot"))
    else:
        reasons.append(did("started", "start", UNIT_NAME))
    return running, None

def remove_the_unit(reasons):
    """Stops and disables pg_autoctl's unit and removes its file. Returns the
    failed result or None."""
    stopped = run_command_that_changes_the_node(["systemctl", "disable", "--now", UNIT_NAME])
    if stopped.error or stopped.exit_code != 0:
        return failed("stopping and disabling " + UNIT_NAME + ": " + message_of(stopped))
    removed = remove_path(UNIT_FILE)
    if removed.error:
        return failed("removing " + UNIT_FILE + ": " + removed.error)
    reloaded = run_command_that_changes_the_node(["systemctl", "daemon-reload"])
    if reloaded.error or reloaded.exit_code != 0:
        return failed("reloading systemd after removing " + UNIT_FILE + ": " + message_of(reloaded))
    reasons.append(did("stopped and removed", "stop and remove", "the unit " + UNIT_NAME))
    return None

def monitor_of(data_directory, monitor):
    """The arguments with which pg_autoctl reaches the monitor: through the
    instance in data_directory, or at the monitor URI given for an instance
    whose monitor is disabled."""
    return ["--monitor", monitor] if monitor else ["--pgdata", data_directory]

def read_formation(data_directory, formation, monitor = ""):
    """Returns the nodes that the monitor lists for the formation, through
    the pg_autoctl instance in data_directory, or at the URI monitor, as
    pg_autoctl show state --json prints them, and an empty text; or None and
    pg_autoctl's message. An empty formation is an empty list. Without a
    formation, pg_autoctl takes the one in the instance's configuration."""
    arguments = ["show", "state"] + monitor_of(data_directory, monitor) + ["--json"]
    if formation:
        arguments += ["--formation", formation]
    shown = run_command(pg_autoctl(arguments))
    if shown.error or shown.exit_code != 0:
        return None, message_of(shown)
    rows = json.decode(shown.standard_output, default = None)
    if type(rows) != "list" or [row for row in rows if type(row) != "dict" or type(row.get("nodename")) != "string"]:
        return None, "pg_autoctl show state --json printed no list of nodes: " + shown.standard_output[:300]
    return rows, ""

def node_of(rows, name):
    for row in rows:
        if row["nodename"] == name:
            return row
    return None

def is_healthy(row):
    return row.get("health") == 1

def is_primary(row):
    """Whether the monitor has made the node its formation's primary."""
    return row.get("assigned_group_state") in PRIMARY_STATES

def primary_name(rows):
    """The name of the formation's primary, or the empty text when no node
    or more than one has a primary's assigned state."""
    names = [row["nodename"] for row in rows if is_primary(row)]
    return names[0] if len(names) == 1 else ""

def database_server_problem(port):
    """Returns the result for a PostgreSQL cluster of Debian's own tools on
    the task's port, or None. Installing postgresql-<version> creates the
    cluster main on 5432 unless createcluster.conf says otherwise."""
    if not find_program("pg_lsclusters").found:
        return None
    listed = run_command(["pg_lsclusters", "--json"])
    if listed.error or listed.exit_code != 0:
        return failed("listing the PostgreSQL clusters with pg_lsclusters --json: " + message_of(listed))
    clusters = json.decode(listed.standard_output, default = None)
    if type(clusters) != "list":
        return failed("pg_lsclusters --json printed no list: " + listed.standard_output[:200])
    for cluster in clusters:
        if type(cluster) == "dict" and str(cluster.get("port")) == str(port):
            name = "%s %s" % (cluster.get("version"), cluster.get("cluster"))
            return {"status": "missing_prerequisite",
                    "reason": ("%s: the PostgreSQL cluster %s of Debian's tools uses the port %s; write create_main_cluster = false into " +
                               "/etc/postgresql-common/createcluster.conf before the task that installs postgresql-<version>, or remove the cluster " +
                               "with pg_dropcluster --stop %s in an earlier node task") % (MODULE, name, port, name)}
    return None

def parent_problem(data_directory):
    """Returns the result for a data directory whose parent does not belong
    to postgres, or None. pg_autoctl makes a directory backup beside the
    data directory."""
    parent = parent_of(data_directory)
    found = inspect_path(parent)
    if found.error:
        return failed("inspecting " + parent + ": " + found.error)
    if not found.exists or found.type != "directory" or found.owner != ACCOUNT:
        what = "does not exist" if not found.exists else ("is no directory" if found.type != "directory" else "belongs to " + found.owner)
        return {"status": "missing_prerequisite",
                "reason": "%s: the directory %s, which holds the data directory, %s; pg_autoctl needs it as a directory of the account postgres, because it makes a directory backup beside the data directory: create it with a file task in an earlier node task" %
                          (MODULE, parent, what)}
    return None

def wait_until_postgresql_answers(pg_isready, host, port):
    """Waits until PostgreSQL accepts connections, at most a minute. Returns
    True when it does."""
    for _ in range(60):
        asked = run_command(pg_isready + ["--quiet", "--port", str(port)] + (["--host", host] if host else []))
        if not asked.error and asked.exit_code == 0:
            return True
        run_command(["sleep", "1"])
    return False

def psql(binaries, port, database):
    """Returns the command line of psql as the account postgres, on the local
    socket, where pg_hba.conf lets the account postgres in as the role
    postgres: with peer, or with initdb's trust before the module replaces
    it."""
    return ["runuser", "-u", ACCOUNT, "--", binaries + "/psql", "--no-psqlrc", "--quiet", "--no-align", "--tuples-only",
            "--set", "ON_ERROR_STOP=1", "--port", str(port), "--dbname", database]

def set_password(binaries, port, database, role, password):
    """Sets a role's password with psql's \\password, which reads the
    password twice from its standard input and sends the server only the
    SCRAM-SHA-256 form. Returns the command."""
    return run_command_that_changes_the_node(psql(binaries, port, database), standard_input = "\\password " + role + "\n" + password + "\n" + password + "\n")

def first_error_line(text):
    """The first line of psql's standard error that is not its prompt."""
    for line in text.splitlines():
        line = line.strip()
        if line and not line.startswith("Password:") and not line.startswith("Enter "):
            return line
    return ""

def host_name_problem(host_name):
    """Returns the rule that a host name breaks, or an empty text."""
    if host_name == "" or host_name.startswith("-") or [c for c in host_name.elems() if c in " /\t\n\r@?#"]:
        return "must be a host name or an IP address, such as 10.20.0.31"
    return ""

def name_problem(name):
    """Returns the rule that the name of a node or a formation breaks, or an
    empty text."""
    if name == "":
        return "must not be empty"
    if "\n" in name or "\r" in name or "'" in name:
        return "must not hold a line break or a quote (')"
    return ""

def node_monitor_uri(host_name, port):
    """The connection URI with which nodes reach the monitor, without a
    password, as pg_autoctl writes it into a node's configuration."""
    host = "[" + host_name + "]" if ":" in host_name else host_name
    return "postgres://autoctl_node@%s:%d/pg_auto_failover?sslmode=require" % (host, port)

def password_problem(field, password):
    if password == "":
        return invalid(field, "must not be empty")
    if "\n" in password or "\r" in password:
        return invalid(field, "must not hold a line break, which psql and PostgreSQL's password file cannot take")
    return None

def listen_address_problem(text):
    """Returns the rule that a listen address breaks, or an empty text. The
    module compares the listening sockets with the address after a change,
    so a host name, which has no address until it is resolved, is not
    one."""
    if text == "*":
        return ""
    parts = text.split(".")
    if len(parts) == 4 and not [part for part in parts if not part.isdigit() or len(part) > 3 or int(part) > 255]:
        return ""
    if ":" in text and not [c for c in text.elems() if c not in "0123456789abcdefABCDEF:."]:
        return ""
    return "must be an IP address of the server, such as 10.20.0.21, or * for every address"

def and_joined(items):
    """"a", "a and b", or "a, b, and c"."""
    if len(items) < 3:
        return " and ".join(items)
    return ", ".join(items[:-1]) + ", and " + items[-1]

def replace_trust_of_initdb(data_directory, socket_method, reasons):
    """Gives the lines of INITDB_LINES in the instance's pg_hba.conf their
    method in place of trust: socket_method for initdb's first line. Every
    run checks the file, so a trust line that comes back is replaced again.
    The caller reloads PostgreSQL. Returns whether the file changed, and
    the failed result or None."""
    path = data_directory + "/pg_hba.conf"
    found = inspect_path(path)
    if found.error:
        return False, failed("inspecting " + path + ": " + found.error)
    if not found.exists:
        return False, None
    read = read_file(path)
    if read.error:
        return False, failed("reading " + path + ": " + read.error)
    lines = []
    replaced = {}
    for line in read.content.split("\n"):
        fields = line.split()
        for start, method in INITDB_LINES:
            if fields == start + ["trust"]:
                method = method or socket_method
                kept = line.rstrip()
                line = kept[:len(kept) - len("trust")] + method
                replaced[method] = replaced.get(method, []) + [" ".join(start)]
        lines.append(line)
    if not replaced:
        return False, None
    written = write_file(path, "\n".join(lines), owner = found.owner, group = found.group, permissions_mode = found.permissions_mode)
    if written.error:
        return False, failed("writing " + path + ": " + written.error)
    parts = ["%s (%s)" % (method, ", ".join(starts)) for method, starts in replaced.items()]
    reasons.append(did("replaced", "replace", "trust in " + path + " with " + and_joined(parts)))
    return True, None

def reload_postgresql(binaries, data_directory, reasons):
    """Reloads the configuration of the PostgreSQL that runs in the data
    directory. Returns the failed result or None."""
    reloaded = run_command_that_changes_the_node(["runuser", "-u", ACCOUNT, "--", binaries + "/pg_ctl", "reload", "--pgdata", data_directory])
    if reloaded.error or reloaded.exit_code != 0:
        return failed("reloading PostgreSQL in " + data_directory + ": " + message_of(reloaded))
    reasons.append(did("reloaded", "reload", "PostgreSQL in " + data_directory))
    return None

def listening_addresses(port):
    """Returns the addresses at which a program listens for TCP connections
    on the port, as ss prints them, and None; or None and the failed
    result."""
    shown = run_command(["ss", "-ltnH"])
    if shown.error or shown.exit_code != 0:
        return None, failed("listing the listening sockets with ss -ltnH: " + message_of(shown))
    addresses = []
    for line in shown.standard_output.splitlines():
        fields = line.split()
        if len(fields) < 4:
            continue
        # Such as 10.20.0.21:5432, [::]:5432, or 127.0.0.53%lo:53.
        address, _, found_port = fields[3].rpartition(":")
        address = address.partition("%")[0].strip("[]")
        if found_port == str(port) and address not in addresses:
            addresses.append(address)
    return sorted(addresses), None

def listens_as_asked(addresses, wanted):
    if wanted == "*":
        return addresses != [] and not [address for address in addresses if address not in ["0.0.0.0", "::", "*"]]
    return addresses == [wanted]

def keep_the_listen_address(binaries, data_directory, configuration, inputs, reasons):
    """Sets pg_autoctl's postgresql.listen_addresses where the instance has
    another value, while its service is stopped: pg_autoctl gives PostgreSQL
    the value when it starts it (postgres -h), and pg_autoctl config set
    while the service ran did not keep the value on the test nodes, because
    the running pg_autoctl wrote its own value back. Then waits, at most a
    minute, until PostgreSQL listens on the task's port at the address and
    nowhere else, and accepts connections. Returns the failed result or
    None."""
    wanted = inputs["listen_address"]
    current = setting(configuration, "postgresql", "listen_addresses")
    if current == wanted:
        return None
    what = "%s, set postgresql.listen_addresses of the instance in %s from %s to %s, and %s the service again" % (
        UNIT_NAME, data_directory, current or "(none)", wanted, "start" if is_dryrun() else "started")
    stopped = run_command_that_changes_the_node(["systemctl", "stop", UNIT_NAME])
    if stopped.error or stopped.exit_code != 0:
        return failed("stopping %s to set postgresql.listen_addresses: %s" % (UNIT_NAME, message_of(stopped)))
    command = run_command_that_changes_the_node(pg_autoctl(["config", "set", "--pgdata", data_directory, "postgresql.listen_addresses", wanted]))
    started = run_command_that_changes_the_node(["systemctl", "start", UNIT_NAME])
    if command.error or command.exit_code != 0:
        return failed("setting postgresql.listen_addresses of the instance in %s to %s: %s" % (data_directory, wanted, message_of(command)))
    if started.error or started.exit_code != 0:
        # The next try would find the setting as the task asks and start
        # nothing.
        return dict(failed("set postgresql.listen_addresses to %s, and then starting %s failed: %s" % (wanted, UNIT_NAME, message_of(started))), try_again = False)
    reasons.append(("would stop " if is_dryrun() else "stopped ") + what)
    if not started.ran:
        return None
    addresses = []
    for _ in range(60):
        addresses, failure = listening_addresses(inputs["port"])
        if failure:
            return failure
        if listens_as_asked(addresses, wanted):
            break
        run_command(["sleep", "1"])
    listening = listens_as_asked(addresses, wanted)
    if listening and wait_until_postgresql_answers([binaries + "/pg_isready"], "", inputs["port"]):
        return None
    found = "does not accept connections" if listening else "listens on port %d at %s" % (inputs["port"], and_joined(addresses) or "no address")
    return dict(failed("set postgresql.listen_addresses to %s and started %s, and a minute later PostgreSQL %s; read journalctl -u %s" %
                       (wanted, UNIT_NAME, found, UNIT_NAME)), try_again = False)

# The states of a node on its way into maintenance and in it.
MAINTENANCE_STATES = ["wait_maintenance", "prepare_maintenance", "maintenance"]

# The states in which a node rests: the monitor assigns it no other one
# until something changes, such as a node that fails or a person's
# switchover. wait_primary rests only while no other healthy node is out of
# maintenance: with one, the monitor goes on to primary once the replica has
# caught up.
RESTING_STATES = ["single", "primary", "secondary", "maintenance", "wait_primary"]

# How long a task waits for its formation to settle. A failover, a
# switchover, and a replica's catching up after maintenance took about a
# minute on the test servers; a formation that changes for longer needs a
# person to look at it.
FORMATION_SETTLES_SECONDS = 300

def unsettled_nodes(rows, must_rest, stopped_nodes = []):
    """The nodes that keep the formation from being settled, each as a text
    with its states: a node that does not rest healthy in the state that
    the monitor assigned it. A node that the monitor does not see as healthy
    and that reports a resting state other than its assigned one has
    stopped, such as one whose server is down: it keeps the state it last
    reported and cannot change it. So does a node in maintenance, whose
    PostgreSQL may be stopped, and one that stopped_nodes names, such as the
    task's own node while its PostgreSQL does not answer. None of them
    counts unless must_rest names it. Any other node that is not healthy is
    on its way between states: the monitor did not see a node as healthy for
    a few seconds while its PostgreSQL restarted to rejoin as a replica."""
    found = []
    for row in rows:
        name, current, assigned = row["nodename"], row.get("current_group_state", ""), row.get("assigned_group_state", "")
        healthy = is_healthy(row)
        stopped = name in stopped_nodes or (not healthy and current in RESTING_STATES and (current != assigned or current == "maintenance"))
        if stopped and name not in must_rest:
            continue
        others_out_of_maintenance = [other for other in rows if other["nodename"] != name and is_healthy(other) and
                                     other.get("assigned_group_state") not in MAINTENANCE_STATES]
        if healthy and current == assigned and current in RESTING_STATES and not (current == "wait_primary" and others_out_of_maintenance):
            continue
        found.append("%s is %s (assigned %s, %s)" % (name, current or "(no state)", assigned or "(no state)", "healthy" if healthy else "not healthy"))
    return found

def wait_until_the_formation_settles(read, must_rest, stopped_nodes = None):
    """Reads the formation with read() until it is settled, at most
    FORMATION_SETTLES_SECONDS; a dryrun reads it once. stopped_nodes(),
    asked once the formation does not settle at once, names nodes that count
    as stopped. Returns its rows, the problem that read() returned, and the
    nodes that kept it from settling: an empty text and an empty list once
    it is settled."""
    stopped = []
    for attempt in range(FORMATION_SETTLES_SECONDS // 2 + 1):
        rows, problem = read()
        if problem:
            return None, problem, []
        unsettled = unsettled_nodes(rows, must_rest, stopped)
        if unsettled and stopped_nodes != None and attempt == 0:
            stopped = stopped_nodes()
            unsettled = unsettled_nodes(rows, must_rest, stopped)
        if not unsettled or is_dryrun() or attempt == FORMATION_SETTLES_SECONDS // 2:
            return rows, "", unsettled
        run_command(["sleep", "2"])

def not_settled(formation, unsettled):
    """The result of a task whose formation did not settle. Another try
    would wait as long again."""
    nodes = "; ".join(unsettled)
    if is_dryrun():
        return {"status": "not_checked",
                "reason": "%s: the formation %s is not settled: %s; a run waits for it, at most %d seconds" % (MODULE, formation, nodes, FORMATION_SETTLES_SECONDS)}
    return dict(failed(("the formation %s has not settled in %d seconds: %s. A failover or a switchover may still go on, or a node needs a person; " +
                        "read pg_autoctl show state and pg_autoctl show events on the monitor's server") % (formation, FORMATION_SETTLES_SECONDS, nodes)),
                try_again = False)

# ---------------------------------------------------------------------------
# This script's own part.
# ---------------------------------------------------------------------------

# The monitor's ident map in its pg_ident.conf, which initdb's first line of
# pg_hba.conf names in place of trust. pg_autoctl, which runs as the account
# postgres, logs in to the monitor's database on the local socket as
# autoctl_node, the user name of its configuration; plain peer refused it,
# and pg_autoctl show state failed. Every account also logs in as the role
# of its own name, as plain peer allows.
IDENT_MAP = "pg_autoctl"
IDENT_MAP_LINES = [[IDENT_MAP, "postgres", "autoctl_node"], [IDENT_MAP, "/^(.*)$", "\\1"]]

def keep_the_ident_map(data_directory, reasons):
    """Adds the lines of IDENT_MAP_LINES that the monitor's pg_ident.conf
    lacks; the file's other lines stay. Returns whether the file changed,
    and the failed result or None."""
    path = data_directory + "/pg_ident.conf"
    found = inspect_path(path)
    if found.error:
        return False, failed("inspecting " + path + ": " + found.error)
    content = ""
    if found.exists:
        read = read_file(path)
        if read.error:
            return False, failed("reading " + path + ": " + read.error)
        content = read.content
    present = [line.split() for line in content.split("\n")]
    missing = [fields for fields in IDENT_MAP_LINES if fields not in present]
    if not missing:
        return False, None
    if content and not content.endswith("\n"):
        content += "\n"
    content += "".join(["    ".join(fields) + "\n" for fields in missing])
    written = write_file(path, content, owner = found.owner if found.exists else ACCOUNT, group = found.group if found.exists else ACCOUNT,
                         permissions_mode = found.permissions_mode if found.exists else "0600")
    if written.error:
        return False, failed("writing " + path + ": " + written.error)
    reasons.append(did("wrote", "write", "the ident map %s, with which pg_autoctl logs in as autoctl_node on the local socket, into %s" % (IDENT_MAP, path)))
    return True, None

def secure_the_local_logins(binaries, data_directory, reasons):
    """Replaces initdb's trust lines, with the ident map on the socket, and
    reloads PostgreSQL after a change. Returns the failed result or None."""
    mapped, failure = keep_the_ident_map(data_directory, reasons)
    if failure:
        return failure
    replaced, failure = replace_trust_of_initdb(data_directory, "peer map=" + IDENT_MAP, reasons)
    if failure:
        return failure
    if mapped or replaced:
        return reload_postgresql(binaries, data_directory, reasons)
    return None

# Every input that state: present uses; deleted_with_all_its_data takes only
# data_directory.
PRESENT_INPUTS = ["postgresql_major_version", "port", "host_name", "listen_address", "autoctl_node_password", "formation", "node_names", "nodes_not_listed"]

def check_inputs(inputs):
    """Returns the result for the first rule that the inputs break, or None.
    No reason repeats the password."""
    problem = path_problem(inputs["data_directory"])
    if problem:
        return invalid("data_directory", problem)
    state = inputs.get("state", "present")
    if state == "deleted_with_all_its_data":
        for name in inputs:
            if name not in ["data_directory", "state"]:
                return invalid("state", "deleted_with_all_its_data accepts only data_directory; remove " + name)
        return None
    for name in PRESENT_INPUTS:
        if name not in inputs:
            return invalid(name, "is required for state: present")
    if inputs["port"] < 1 or inputs["port"] > 65535:
        return invalid("port", "must be a port number from 1 to 65535")
    problem = host_name_problem(inputs["host_name"]) or name_problem(inputs["formation"])
    if problem:
        return invalid("host_name" if host_name_problem(inputs["host_name"]) else "formation", problem)
    if listen_address_problem(inputs["listen_address"]):
        return invalid("listen_address", listen_address_problem(inputs["listen_address"]))
    for name in inputs["node_names"]:
        if name_problem(name):
            return invalid("node_names", "a name " + name_problem(name))
    return password_problem("autoctl_node_password", inputs["autoctl_node_password"])

def described_nodes(rows):
    """The output nodes: one entry for each registered node."""
    return [{
        "name": row["nodename"],
        "host_name": row.get("nodehost", ""),
        "port": row.get("nodeport", 0),
        "current_state": row.get("current_group_state", ""),
        "assigned_state": row.get("assigned_group_state", ""),
        "healthy": is_healthy(row),
        "candidate_priority": row.get("candidate_priority", 0),
        "replication_quorum": row.get("replication_quorum", False),
    } for row in rows]

def outputs_of(created, rows, inputs):
    return {"created": created, "primary_node_name": primary_name(rows), "nodes": described_nodes(rows),
            "node_monitor_uri": node_monitor_uri(inputs["host_name"], inputs["port"])}

def run_node_task(inputs):
    rejected = check_inputs(inputs)
    if rejected:
        return rejected
    data_directory = inputs["data_directory"]
    state = inputs.get("state", "present")

    programs = [("runuser", "util-linux"), ("pg_autoctl", "pg-auto-failover-cli")]
    if state == "present":
        programs.append(("ss", "iproute2"))
        version = str(inputs["postgresql_major_version"])
        binaries = POSTGRESQL_PROGRAMS % version
        programs += [(binaries + "/pg_ctl", "postgresql-" + version), (binaries + "/psql", "postgresql-client-" + version),
                     (binaries + "/pg_isready", "postgresql-client-" + version)]
    missing = missing_program(programs)
    if missing:
        return missing
    home, failure = postgres_home()
    if failure:
        return failure
    configuration, failure = read_ini(configuration_path(home, data_directory))
    if failure:
        return failure
    if configuration != None and setting(configuration, "pg_autoctl", "role") != "monitor":
        return {"status": "invalid_inputs",
                "reason": "%s: %s holds a pg_autoctl instance of the role %s, not a monitor" % (MODULE, data_directory, setting(configuration, "pg_autoctl", "role") or "(none)")}
    unit, failure = read_unit()
    if failure:
        return failure
    unit_directory = data_directory_of_unit(unit) if unit != None else ""
    if unit != None and unit_directory != data_directory:
        return {"status": "invalid_inputs",
                "reason": "%s: %s runs the pg_autoctl instance in %s; one instance of pg_autoctl runs on a server, so the monitor needs a server of its own" %
                          (MODULE, UNIT_FILE, unit_directory or "(no data directory)")}
    fingerprint = json.encode({key: inputs[key] for key in inputs if key != "autoctl_node_password"})

    if state == "deleted_with_all_its_data":
        return delete_the_monitor(data_directory, configuration, unit, fingerprint)

    extension = POSTGRESQL_EXTENSIONS % version + "/pgautofailover.control"
    found = inspect_path(extension)
    if found.error:
        return failed("inspecting " + extension + ": " + found.error)
    if not found.exists:
        return {"status": "missing_prerequisite",
                "reason": MODULE + ": the PostgreSQL extension pgautofailover is not installed; install the package postgresql-" + version + "-auto-failover in an earlier node task"}

    if configuration != None:
        for section, key, wanted, field in [("pg_autoctl", "hostname", inputs["host_name"], "host_name"), ("postgresql", "port", str(inputs["port"]), "port")]:
            if setting(configuration, section, key) != wanted:
                return invalid(field, "the monitor in %s has the %s %s; pg_autoctl keeps it in the configuration of every node, so it does not change here" %
                                      (data_directory, key, setting(configuration, section, key)))

    reasons = []
    created = False
    # The unit is written after pg_autoctl created the monitor, so a monitor
    # without its unit is a creation that stopped partway: pg_autoctl's
    # create command finishes it.
    if configuration == None or unit == None:
        if configuration == None:
            for problem in [parent_problem(data_directory), database_server_problem(inputs["port"])]:
                if problem:
                    return problem
        created = configuration == None
        command = run_command_that_changes_the_node(pg_autoctl([
            "create", "monitor", "--pgctl", binaries + "/pg_ctl", "--pgdata", data_directory, "--pgport", str(inputs["port"]),
            "--hostname", inputs["host_name"], "--listen", inputs["listen_address"], "--auth", "scram-sha-256", "--ssl-self-signed"]))
        if command.error or command.exit_code != 0:
            return failed("creating the monitor in " + data_directory + ": " + message_of(command))
        if created:
            reasons.append(did("created", "create", "the pg_auto_failover monitor in " + data_directory))
        else:
            reasons.append(did("finished creating", "finish creating", "the pg_auto_failover monitor in " + data_directory))
        if not command.ran:
            reasons.append("would write the unit " + UNIT_FILE + " and start it")
            reasons.append("would set the password of autoctl_node")
            if inputs["formation"] != "default":
                reasons.append("would create the formation " + inputs["formation"])
            return {"reason": "; ".join(reasons), "outputs": outputs_of(created, [], inputs), "fingerprint": fingerprint}

    failure = write_the_unit(data_directory, reasons)
    if failure:
        return failed_after_creation(failure, created, inputs)
    running, failure = start_the_unit(reasons)
    if failure:
        return failed_after_creation(failure, created, inputs)
    if not running and is_dryrun():
        # The monitor does not run: its password and its nodes are read once
        # it runs.
        reasons.append("would compare the password of autoctl_node and the formation's nodes once the monitor runs")
        return {"reason": "; ".join(reasons), "outputs": {"created": created, "node_monitor_uri": node_monitor_uri(inputs["host_name"], inputs["port"])}, "fingerprint": fingerprint}
    if not wait_until_postgresql_answers([binaries + "/pg_isready"], inputs["host_name"], inputs["port"]):
        failure = failed("the monitor's PostgreSQL in %s does not accept connections at %s, port %d; read journalctl -u %s" %
                         (data_directory, inputs["host_name"], inputs["port"], UNIT_NAME))
        return failed_after_creation(failure, created, inputs)

    failure = (secure_the_local_logins(binaries, data_directory, reasons) or
               (keep_the_listen_address(binaries, data_directory, configuration, inputs, reasons) if configuration != None else None) or
               make_the_password_work(binaries, inputs, reasons) or make_the_formation_exist(binaries, data_directory, inputs, reasons))
    if failure:
        return failed_after_creation(failure, created, inputs)

    rows, problem = read_formation(data_directory, inputs["formation"])
    if problem:
        return failed_after_creation(failed("reading the nodes of the formation %s: %s" % (inputs["formation"], problem)), created, inputs)
    to_drop = []
    if inputs["nodes_not_listed"] == "remove":
        to_drop = [row for row in rows if row["nodename"] not in inputs["node_names"]]
    refused = [row for row in to_drop if is_healthy(row) and is_primary(row)]
    if refused:
        # An earlier node task, such as pg_auto_failover_primary, may make
        # another node the primary.
        return {"status": "missing_prerequisite",
                "reason": ("%s: node_names does not list the node %s, which the monitor reports healthy and as the primary (assigned state %s): " +
                           "dropping it would cause a failover, or remove the only copy of the database that runs. Make another node the primary " +
                           "first with pg_auto_failover_primary, or list the node") % (MODULE, refused[0]["nodename"], refused[0].get("assigned_group_state"))}
    for row in to_drop:
        # --force drops a node that does not answer, such as a server that is
        # gone; without it pg_autoctl waits for the node and fails after a
        # minute.
        command = run_command_that_changes_the_node(pg_autoctl(["drop", "node", "--pgdata", data_directory, "--formation", inputs["formation"],
                                                                "--name", row["nodename"], "--force"]))
        if command.error or command.exit_code != 0:
            failure = failed("dropping the node %s from the formation %s: %s" % (row["nodename"], inputs["formation"], message_of(command)))
            return failed_after_creation(failure, created, inputs)
        reasons.append(did("dropped", "drop", "the node %s (%s) from the formation %s" % (row["nodename"], row.get("current_group_state"), inputs["formation"])))
    if to_drop and not is_dryrun():
        rows, problem = read_formation(data_directory, inputs["formation"])
        if problem:
            return failed_after_creation(failed("reading the nodes of the formation %s: %s" % (inputs["formation"], problem)), created, inputs)
    rows = [row for row in rows if row not in to_drop]

    result = {"outputs": outputs_of(created, rows, inputs), "fingerprint": fingerprint}
    if reasons:
        result["reason"] = "; ".join(reasons)
    return result

def failed_after_creation(failure, created, inputs):
    """Returns a failure after this try created the monitor with the output
    created, and without another try: the next try would find the monitor
    and report created: false for what this task created."""
    if created and failure["status"] == "failed":
        failure["try_again"] = False
        failure["reason"] = MODULE + ": created the monitor, and then this failed: " + failure["reason"][len(MODULE) + 2:]
    failure["outputs"] = outputs_of(created, [], inputs)
    return failure

def make_the_password_work(binaries, inputs, reasons):
    """Compares the password of autoctl_node by one login as autoctl_node
    over TCP with TLS, as a node logs in, and sets it with psql's \\password
    when the monitor refuses that login. Returns the failed result or
    None."""
    accepted, _, failure = log_in_as_autoctl_node(binaries, inputs)
    if failure:
        return failure
    if accepted:
        return None
    command = set_password(binaries, inputs["port"], "pg_auto_failover", "autoctl_node", inputs["autoctl_node_password"])
    if command.error or command.exit_code != 0:
        return failed("setting the password of autoctl_node: " + (command.error or first_error_line(command.standard_error)))
    if command.ran:
        accepted, refusal, failure = log_in_as_autoctl_node(binaries, inputs)
        if failure:
            return failure
        if not accepted:
            # The login fails for another reason than the password, which
            # another try does not change.
            return {"status": "failed", "try_again": False,
                    "reason": "%s: set the password of autoctl_node, and the monitor still refuses the login as autoctl_node at %s, port %d: %s" %
                              (MODULE, inputs["host_name"], inputs["port"], refusal)}
    reasons.append(did("set", "set", "the password of autoctl_node"))
    return None

def login_command(binaries, inputs):
    return ["runuser", "-u", ACCOUNT, "--", binaries + "/psql", "--no-psqlrc", "--quiet", "--no-align", "--tuples-only", "--password",
            "host=%s port=%d dbname=pg_auto_failover user=autoctl_node sslmode=require connect_timeout=10" % (inputs["host_name"], inputs["port"])]

def log_in_as_autoctl_node(binaries, inputs):
    """Logs in once as autoctl_node with the task's password. psql reads the
    password from its standard input, where it has no terminal. Returns
    whether the monitor accepted it, psql's message, and the failed result
    or None. The module asked before that the monitor answers, so a session
    that psql does not get (its exit code 2) is a refused login."""
    login = run_command(login_command(binaries, inputs), standard_input = inputs["autoctl_node_password"] + "\nselect 1;\n")
    if login.error:
        return None, "", failed("logging in as autoctl_node: " + login.error)
    if login.exit_code == 0 and login.standard_output.strip() == "1":
        return True, "", None
    if login.exit_code == 2:
        return False, first_error_line(login.standard_error), None
    return None, "", failed("logging in as autoctl_node: " + first_error_line(login.standard_error))

def make_the_formation_exist(binaries, data_directory, inputs, reasons):
    """Creates the formation on the monitor where it is missing. Returns the
    failed result or None."""
    formation = inputs["formation"]
    query = "\\set formation '" + formation.replace("\\", "\\\\").replace("'", "\\'") + "'\nselect count(*) from pgautofailover.formation where formationid = :'formation';\n"
    asked = run_command(psql(binaries, inputs["port"], "pg_auto_failover"), standard_input = query)
    if asked.error or asked.exit_code != 0:
        return failed("reading the formations of the monitor: " + (asked.error or first_error_line(asked.standard_error)))
    if asked.standard_output.strip() == "1":
        return None
    command = run_command_that_changes_the_node(pg_autoctl(["create", "formation", "--pgdata", data_directory, "--formation", formation, "--kind", "pgsql"]))
    if command.error or command.exit_code != 0:
        return failed("creating the formation " + formation + ": " + message_of(command))
    reasons.append(did("created", "create", "the formation " + formation))
    return None

def delete_the_monitor(data_directory, configuration, unit, fingerprint):
    """Stops the monitor, removes its unit, and deletes its data directory
    with pg_autoctl drop monitor --destroy."""
    result = {"fingerprint": fingerprint}
    if configuration == None and unit == None:
        return result
    reasons = []
    if unit != None:
        failure = remove_the_unit(reasons)
        if failure:
            return failure
    if configuration != None:
        command = run_command_that_changes_the_node(pg_autoctl(["drop", "monitor", "--pgdata", data_directory, "--destroy"]))
        if command.error or command.exit_code != 0:
            return failed("deleting the monitor in " + data_directory + ": " + message_of(command))
        reasons.append(did("deleted", "delete", "the pg_auto_failover monitor in " + data_directory + " with all its data"))
    result["reason"] = "; ".join(reasons)
    return result
