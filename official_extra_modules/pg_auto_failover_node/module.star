# pg_auto_failover_node manages one PostgreSQL node of a pg_auto_failover
# formation on its database server: the node's pg_autoctl instance, its
# systemd unit, the two logins in the password file of the account postgres,
# the node's failover settings, maintenance, and its PostgreSQL settings. It
# runs pg_autoctl and psql as the account postgres.
# docs/changing-mmx/design/modules-official-extra.md ("pg_auto_failover") owns the
# behavior.

MODULE = "pg_auto_failover_node"

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

def monitor_refuses_the_node(binaries, inputs):
    """Logs in to the monitor once as autoctl_node, as pg_autoctl will, before
    pg_autoctl create postgres runs. pg_autoctl lets autoctl_node log in
    only from the subnet of the monitor's address; for a node outside it,
    the monitor answers "no pg_hba.conf entry", and pg_autoctl create
    postgres tries that login again without end. psql takes the password
    from ~postgres/.pgpass, as pg_autoctl does, and never asks for one.
    Only that refusal gives a result: a monitor that does not answer, or
    another failure, is left to pg_autoctl's creation. Returns the failed
    result, or None."""
    login = run_command(["runuser", "-u", ACCOUNT, "--", binaries + "/psql", "--no-psqlrc", "--quiet", "--no-align", "--tuples-only", "--no-password",
                         "--command", "select 1",
                         "host=%s port=%d dbname=pg_auto_failover user=autoctl_node sslmode=require connect_timeout=5" % (inputs["monitor_host_name"], inputs["monitor_port"])])
    if login.error or login.exit_code == 0 or "no pg_hba.conf entry" not in login.standard_error:
        return None
    return {"status": "failed", "try_again": False,
            "reason": ("%s: the monitor at %s, port %d refused the login of autoctl_node from this node: its pg_hba.conf has no entry for the node's address. " +
                       "The monitor lets nodes in only from the subnet of its own address, so the monitor and the nodes must share a subnet. Put the monitor's " +
                       "host_name and the nodes on one network that they share, such as a private network or a WireGuard network, and run the task again. psql said: %s") %
                      (MODULE, inputs["monitor_host_name"], inputs["monitor_port"], first_error_line(login.standard_error))}

def monitor_has_a_record_of(data_directory, formation, name, monitor):
    """Whether the monitor's events of the formation name the node. A
    monitor records an event for each registration and state change of a
    node, and keeps the events of a node it dropped; a monitor built again
    in place of a lost one has none of the nodes it never knew. Returns it
    and an empty text, or False and pg_autoctl's message."""
    shown = run_command(pg_autoctl(["show", "events"] + monitor_of(data_directory, monitor) + ["--formation", formation, "--count", str(EVENTS_READ), "--json"]))
    if shown.error or shown.exit_code != 0:
        return False, message_of(shown)
    text = shown.standard_output.strip()
    events = json.decode(text, default = None) if text not in ["", "null"] else []
    if type(events) != "list":
        return False, "pg_autoctl show events --json printed no list of events: " + text[:300]
    return len([event for event in events if type(event) == "dict" and event.get("nodename") == name]) > 0, ""

def is_in_recovery(binaries, port):
    """Whether the node's PostgreSQL is a replica, as it says itself: True
    or False and an empty text, or None and psql's message."""
    asked = run_command(psql(binaries, port, "postgres") + ["--command", "select pg_is_in_recovery()"])
    if asked.error or asked.exit_code != 0 or asked.standard_output.strip() not in ["t", "f"]:
        return None, message_of(asked)
    return asked.standard_output.strip() == "t", ""

def primary_address_of_the_replica(binaries, port):
    """The address of the primary from which the replica's PostgreSQL
    receives its changes, or an empty text when it receives none."""
    asked = run_command(psql(binaries, port, "postgres") + ["--command", "select sender_host from pg_stat_wal_receiver"])
    return asked.standard_output.strip() if not asked.error and asked.exit_code == 0 else ""

def register_with_a_monitor_without_its_record(binaries, data_directory, inputs, rows, monitor_disabled, reasons):
    """Registers a node that the monitor has no record of, as after the
    monitor's server was built again with an empty monitor, with the
    commands of pg_auto_failover's monitor replacement: pg_autoctl disable
    monitor --force, then pg_autoctl enable monitor. The primary registers
    first, and stays the primary; a replica waits until the monitor lists
    a primary, since in an empty formation the monitor would make the first
    node it registers a primary. A node whose monitor an earlier run
    disabled, before pg_autoctl enable monitor failed, only enables it.
    Returns None, or the result that ends the task."""
    name, formation = inputs["name"], inputs["formation"]
    monitor = "%s:%d" % (inputs["monitor_host_name"], inputs["monitor_port"])
    if not wait_until_postgresql_answers([binaries + "/pg_isready"], "", inputs["port"]):
        return failed("the monitor at %s has no record of the node %s, and the node's PostgreSQL in %s does not accept connections on port %d, so MMX cannot tell whether it is the primary; read journalctl -u %s" %
                      (monitor, name, data_directory, inputs["port"], UNIT_NAME))
    replica, problem = is_in_recovery(binaries, inputs["port"])
    if replica == None:
        return failed("asking the node's PostgreSQL whether it is a replica: " + problem)
    primaries = [row["nodename"] for row in rows if is_primary(row)]
    reporting = [row["nodename"] for row in rows if is_primary(row) and has_reported(row)]
    if not replica and primaries:
        return failed(("the node %s is a primary, and the monitor at %s, which has no record of it, lists %s as the primary of the formation %s: two nodes " +
                       "serve as primaries. Decide which one keeps the data; remove the other with state: deleted_with_all_its_data") %
                      (name, monitor, and_joined(primaries), formation))
    if replica:
        # The primary must also report to the monitor: a primary that does
        # not, the monitor takes for failed, and it would promote this
        # replica once it registers.
        for _ in range(REPLICA_WAITS_FOR_THE_PRIMARY_SECONDS // 2):
            if reporting or is_dryrun():
                break
            run_command(["sleep", "2"])
            rows, problem = read_formation(data_directory, formation, node_monitor_uri(inputs["monitor_host_name"], inputs["monitor_port"]) if monitor_disabled else "")
            if problem:
                return failed("reading the formation %s from the monitor at %s: %s" % (formation, monitor, problem))
            reporting = [row["nodename"] for row in rows if is_primary(row) and has_reported(row)]
        if not reporting and not (is_dryrun() and primaries):
            # The task fails rather than succeeds: a success would report
            # the formation complete while the replica has no failover.
            address = primary_address_of_the_replica(binaries, inputs["port"])
            return {"status": "not_checked" if is_dryrun() else "missing_prerequisite",
                    "reason": ("%s: the monitor at %s has no record of the replica %s and lists no primary of the formation %s that has reported to it: " +
                               "a replica registers after its primary, so that the monitor does not make it a primary in an empty formation.%s Register " +
                               "the primary first: run the playbook with --limit and the primary's node, then run it again") %
                              (MODULE, monitor, name, formation, " The replica receives its changes from the primary at %s." % address if address else "")}
    # Each pg_autoctl command below signals the running keeper to read its
    # configuration again. On the test servers the second of two signals
    # close together was lost: the keeper went on without a monitor, the
    # monitor had no report from it, and pg_autoctl enable monitor waited
    # for ever on a replica. So the task waits between the two, bounds
    # enable monitor, signals the keeper once more, and waits for its report.
    if not monitor_disabled:
        disabled = run_command_that_changes_the_node(pg_autoctl(["disable", "monitor", "--force", "--pgdata", data_directory]))
        if disabled.error or disabled.exit_code != 0:
            return failed("registering the node %s with the monitor at %s: pg_autoctl disable monitor: %s" % (name, monitor, message_of(disabled)))
        if disabled.ran:
            run_command(["sleep", str(KEEPER_READS_ITS_CONFIGURATION_SECONDS)])
    enabled = run_command_that_changes_the_node(["timeout", str(ENABLE_MONITOR_SECONDS)] +
                                                pg_autoctl(["enable", "monitor", "--pgdata", data_directory, node_monitor_uri(inputs["monitor_host_name"], inputs["monitor_port"])]))
    if enabled.error or enabled.exit_code not in [0, 124]:
        return failed("registering the node %s with the monitor at %s: pg_autoctl enable monitor: %s" % (name, monitor, message_of(enabled)))
    reasons.append(did("registered", "register", "the node %s, %s, with the monitor at %s, which had no record of it" %
                                                 (name, "a replica" if replica else "the primary", monitor)))
    if not enabled.ran:
        return None
    for attempt in range(REPORT_WAIT_SECONDS // 2):
        if attempt % 10 == 0:
            reloaded = run_command_that_changes_the_node(pg_autoctl(["reload", "--pgdata", data_directory]))
            if reloaded.error or reloaded.exit_code != 0:
                return failed("asking the keeper of the node %s to read its configuration again: pg_autoctl reload: %s" % (name, message_of(reloaded)))
        rows, problem = read_formation(data_directory, "")
        row = node_of(rows, name) if not problem else None
        if row != None and has_reported(row):
            return None
        run_command(["sleep", "2"])
    return failed(("registered the node %s with the monitor at %s, but its keeper has sent the monitor no report for %d seconds; read journalctl -u %s " +
                   "on the node. Until it reports, the monitor takes the node for failed") % (name, monitor, REPORT_WAIT_SECONDS, UNIT_NAME))

def has_reported(row):
    """Whether the node's keeper has sent the monitor a report since it
    registered: a registration alone leaves its reported position 0/0."""
    return row.get("reported_lsn", "0/0") not in ["", "0/0"]

# ---------------------------------------------------------------------------

# Every input that state present and in_maintenance use; absent and
# deleted_with_all_its_data take only data_directory.
PRESENT_INPUTS = ["name", "postgresql_major_version", "port", "host_name", "listen_address", "monitor_host_name", "monitor_port", "autoctl_node_password",
                  "replication_password", "formation", "candidate_priority", "replication_quorum"]

# What pg_autoctl disable monitor writes as a node's monitor.
MONITOR_DISABLED = "PG_AUTOCTL_DISABLED"

# How many of the formation's latest events the module reads to find
# whether the monitor ever knew a node. A monitor records a few events for
# each registration, failover, or maintenance, so this reaches back over
# years of a formation's history; a node whose last event is older is
# taken as one the monitor never knew, and registers again.
EVENTS_READ = 100000

# How long a replica waits for its primary to register with a monitor that
# has no record of either: the primary's own task, run at the same time,
# registers it in seconds.
REPLICA_WAITS_FOR_THE_PRIMARY_SECONDS = 300

# How long the module waits after pg_autoctl disable monitor, so that the
# running keeper reads it before enable monitor signals it again; how long
# enable monitor may wait for the node's state, which a replica reaches
# after the primary takes it; and how long the module then waits for the
# keeper's first report to the monitor.
KEEPER_READS_ITS_CONFIGURATION_SECONDS = 5
ENABLE_MONITOR_SECONDS = 300
REPORT_WAIT_SECONDS = 120

# The two logins of the password file ~/.pgpass that the module keeps, by
# their database and role: autoctl_node reaches the monitor, and
# pgautofailover_replicator copies from the primary. The replicator's line
# names every database (*): a replica's connection names none.
# A file beside ~/.pgpass that says the file holds a replication password
# that the primary may not have set yet: a try that wrote the new password
# into the file and failed before setting it leaves the file, so the next try
# still sets it.
REPLICATION_PASSWORD_NOT_SET_YET = "/.pgpass.mmx-replication-password-not-set-yet"

PGPASS_LOGINS = [("pg_auto_failover", "autoctl_node", "autoctl_node_password"), ("*", "pgautofailover_replicator", "replication_password")]

def check_inputs(inputs):
    """Returns the result for the first rule that the inputs break, or None.
    No reason repeats a password."""
    problem = path_problem(inputs["data_directory"])
    if problem:
        return invalid("data_directory", problem)
    state = inputs.get("state", "present")
    if state in ["absent", "deleted_with_all_its_data"]:
        for name in inputs:
            if name not in ["data_directory", "state"]:
                return invalid("state", state + " accepts only data_directory; remove " + name)
        return None
    for name in PRESENT_INPUTS:
        if name not in inputs:
            return invalid(name, "is required for state: " + state)
    for name in ["port", "monitor_port"]:
        if inputs[name] < 1 or inputs[name] > 65535:
            return invalid(name, "must be a port number from 1 to 65535")
    for name in ["host_name", "monitor_host_name"]:
        if host_name_problem(inputs[name]):
            return invalid(name, host_name_problem(inputs[name]))
    if listen_address_problem(inputs["listen_address"]):
        return invalid("listen_address", listen_address_problem(inputs["listen_address"]))
    for name in ["name", "formation"]:
        if name_problem(inputs[name]):
            return invalid(name, name_problem(inputs[name]))
    if inputs["candidate_priority"] < 0 or inputs["candidate_priority"] > 100:
        return invalid("candidate_priority", "must be a whole number from 0 to 100")
    return (password_problem("autoctl_node_password", inputs["autoctl_node_password"]) or password_problem("replication_password", inputs["replication_password"]) or
            settings_problem(inputs))

def pgpass_escaped(text):
    """A field of PostgreSQL's password file, with a backslash before each
    colon and backslash."""
    return text.replace("\\", "\\\\").replace(":", "\\:")

def pgpass_fields(line):
    """The fields of one line of the password file, without their escapes."""
    fields = [""]
    escaped = False
    for character in line.elems():
        if escaped:
            fields[-1] += character
            escaped = False
        elif character == "\\":
            escaped = True
        elif character == ":":
            fields.append("")
        else:
            fields[-1] += character
    return fields

def wanted_password_file(content, inputs):
    """Returns the password file's content with the module's two lines, the
    file's other lines as they are, and whether the replicator's line held
    another password before. A line of the module's is one for every host
    and port with one of its two databases and roles."""
    wanted = {}
    for database, role, field in PGPASS_LOGINS:
        wanted[(database, role)] = "*:*:%s:%s:%s" % (database, role, pgpass_escaped(inputs[field]))
    lines = []
    written = []
    replicator_differed = True
    for line in content.splitlines():
        fields = pgpass_fields(line)
        key = (fields[2], fields[3]) if len(fields) == 5 and fields[0] == "*" and fields[1] == "*" else None
        if key not in wanted:
            lines.append(line)
            continue
        if key == ("*", "pgautofailover_replicator") and fields[4] == inputs["replication_password"]:
            replicator_differed = False
        if key not in written:
            lines.append(wanted[key])
            written.append(key)
    for database, role, _ in PGPASS_LOGINS:
        if (database, role) not in written:
            lines.append(wanted[(database, role)])
    return "\n".join(lines) + "\n", replicator_differed

def keep_the_password_file(home, inputs, reasons):
    """Writes the module's two lines into the account postgres's ~/.pgpass,
    which belongs to postgres with mode 0600. The file goes to install
    through its standard input, so the passwords and the file's other lines
    stay out of the result, into a new file beside it, which mv renames into
    place: a reader never sees half a file, and install cannot write over an
    existing file from its standard input. Returns whether the replicator's
    line held another password before, and the failed result or None."""
    path = home + "/.pgpass"
    found = inspect_path(path)
    if found.error:
        return True, failed("inspecting " + path + ": " + found.error)
    content = ""
    if found.exists:
        read = read_file(path)
        if read.error:
            return True, failed("reading " + path + ": " + read.error)
        content = read.content
    wanted, replicator_differed = wanted_password_file(content, inputs)
    marker = home + REPLICATION_PASSWORD_NOT_SET_YET
    pending = inspect_path(marker)
    if pending.error:
        return True, failed("inspecting " + marker + ": " + pending.error)
    if pending.exists:
        replicator_differed = True
    elif replicator_differed and ":pgautofailover_replicator:" in content:
        marked = write_file(marker, "", owner = "root", group = "root", permissions_mode = "0600")
        if marked.error:
            return True, failed("writing " + marker + ": " + marked.error)
    if found.exists and content == wanted and found.owner == ACCOUNT and found.group == ACCOUNT and found.permissions_mode == "0600":
        return replicator_differed, None
    new = path + ".mmx-new"
    left = remove_path(new)
    if left.error:
        return True, failed("removing " + new + ": " + left.error)
    written = run_command_that_changes_the_node(["install", "-o", ACCOUNT, "-g", ACCOUNT, "-m", "0600", "/dev/stdin", new], standard_input = wanted)
    if written.error or written.exit_code != 0:
        return True, failed("writing the password file " + new + ": " + message_of(written))
    moved = run_command_that_changes_the_node(["mv", "-f", new, path])
    if moved.error or moved.exit_code != 0:
        return True, failed("renaming " + new + " to " + path + ": " + message_of(moved))
    reasons.append(did("wrote", "write", "the logins of autoctl_node and pgautofailover_replicator into the password file " + path))
    return replicator_differed, None

def run_node_task(inputs):
    rejected = check_inputs(inputs)
    if rejected:
        return rejected
    data_directory = inputs["data_directory"]
    state = inputs.get("state", "present")

    programs = [("runuser", "util-linux"), ("pg_autoctl", "pg-auto-failover-cli"), ("install", "coreutils"), ("mv", "coreutils"), ("rm", "coreutils"),
                ("timeout", "coreutils")]
    binaries = ""
    if state in ["present", "in_maintenance"]:
        version = str(inputs["postgresql_major_version"])
        binaries = POSTGRESQL_PROGRAMS % version
        programs += [(binaries + "/pg_ctl", "postgresql-" + version), (binaries + "/psql", "postgresql-client-" + version),
                     (binaries + "/pg_isready", "postgresql-client-" + version), ("ss", "iproute2")]
        if "postgresql_settings" in inputs:
            programs += [(binaries + "/postgres", "postgresql-" + version), (binaries + "/pg_controldata", "postgresql-" + version)]
    missing = missing_program(programs)
    if missing:
        return missing
    home, failure = postgres_home()
    if failure:
        return failure
    configuration, failure = read_ini(configuration_path(home, data_directory))
    if failure:
        return failure
    if configuration != None and setting(configuration, "pg_autoctl", "role") != "keeper":
        return {"status": "invalid_inputs",
                "reason": "%s: %s holds a pg_autoctl instance of the role %s, not a node" % (MODULE, data_directory, setting(configuration, "pg_autoctl", "role") or "(none)")}
    unit, failure = read_unit()
    if failure:
        return failure
    unit_directory = data_directory_of_unit(unit) if unit != None else ""
    if unit != None and unit_directory != data_directory:
        return {"status": "invalid_inputs",
                "reason": "%s: %s runs the pg_autoctl instance in %s; one instance of pg_autoctl runs on a server, so this node needs a server of its own" %
                          (MODULE, UNIT_FILE, unit_directory or "(no data directory)")}
    fingerprint = json.encode({key: inputs[key] for key in inputs if key not in ["autoctl_node_password", "replication_password"]})
    if state in ["absent", "deleted_with_all_its_data"]:
        return remove_the_node(state, data_directory, configuration, unit, fingerprint, home)

    # A node whose monitor is disabled is one that an earlier run moved to a
    # monitor without its record, before pg_autoctl enable monitor failed.
    monitor_disabled = configuration != None and setting(configuration, "pg_autoctl", "monitor") == MONITOR_DISABLED
    if configuration != None:
        for section, key, wanted, field in [
            ("pg_autoctl", "name", inputs["name"], "name"),
            ("pg_autoctl", "formation", inputs["formation"], "formation"),
            ("pg_autoctl", "hostname", inputs["host_name"], "host_name"),
            ("pg_autoctl", "monitor", node_monitor_uri(inputs["monitor_host_name"], inputs["monitor_port"]), "monitor_host_name"),
            ("postgresql", "port", str(inputs["port"]), "port"),
        ]:
            if setting(configuration, section, key) != wanted and not (key == "monitor" and monitor_disabled):
                return invalid(field, "the node in %s has the %s %s, which pg_autoctl does not change: remove the node with state: absent first, and create it again under a new name" %
                                      (data_directory, key, setting(configuration, section, key)))

    reasons = []
    replicator_differed, failure = keep_the_password_file(home, inputs, reasons)
    if failure:
        return failure
    created = False
    # The unit is written after pg_autoctl created the node, so a node
    # without its unit is a creation that stopped partway: pg_autoctl's
    # create command finishes it.
    if configuration == None or unit == None:
        if configuration == None:
            for problem in [parent_problem(data_directory), database_server_problem(inputs["port"]), restored_cluster_problem(data_directory, inputs)]:
                if problem:
                    return problem
        if configuration == None and not is_dryrun():
            failure = monitor_refuses_the_node(binaries, inputs)
            if failure:
                return failure
        created = configuration == None
        if created:
            failure = stop_an_unregistered_cluster(binaries, data_directory, reasons)
            if failure:
                return failure
        create = pg_autoctl([
            "create", "postgres", "--pgctl", binaries + "/pg_ctl", "--pgdata", data_directory, "--pgport", str(inputs["port"]),
            "--name", inputs["name"], "--hostname", inputs["host_name"], "--listen", inputs["listen_address"], "--formation", inputs["formation"],
            "--monitor", node_monitor_uri(inputs["monitor_host_name"], inputs["monitor_port"]),
            "--candidate-priority", str(inputs["candidate_priority"]), "--replication-quorum", "true" if inputs["replication_quorum"] else "false",
            "--auth", "scram-sha-256", "--ssl-self-signed"])
        command = run_command_that_changes_the_node(create)
        if (command.error or command.exit_code != 0) and not command.error and inspect_path(configuration_path(home, data_directory)).exists:
            # pg_autoctl wrote the node's configuration and stopped partway.
            # For a data directory that pg_autoctl managed before, such as a
            # restored one, its first run refused with "PostgreSQL is already
            # running" on the test servers, and the same command, run again,
            # finished the creation: as pg_autoctl allows, it runs once more.
            command = run_command_that_changes_the_node(create)
        if command.error or command.exit_code != 0:
            return creation_failed(home, inputs, created, "creating the node %s in %s: %s" % (inputs["name"], data_directory, message_of(command)))
        what = "the node %s in %s in the formation %s" % (inputs["name"], data_directory, inputs["formation"])
        reasons.append(did("created", "create", what) if created else did("finished creating", "finish creating", what))
        if not command.ran:
            # The monitor gives the node its state once it is created, so a
            # dryrun reports neither current_state nor is_primary.
            reasons.append("would write the unit " + UNIT_FILE + " and start it")
            return {"reason": "; ".join(reasons), "outputs": {"created": created}, "fingerprint": fingerprint}

    failure = write_the_unit(data_directory, reasons)
    if failure:
        return failed_after_creation(failure, created)
    running, failure = start_the_unit(reasons)
    if failure:
        return failed_after_creation(failure, created)
    if not running and is_dryrun():
        reasons.append("would compare the node's settings once it runs")
        return {"reason": "; ".join(reasons), "outputs": {"created": created}, "fingerprint": fingerprint}
    monitor = node_monitor_uri(inputs["monitor_host_name"], inputs["monitor_port"]) if monitor_disabled else ""
    # The task starts on a settled formation: a restart of another node,
    # such as a playbook's, may have started a failover that still goes on,
    # and what the task does depends on the states that the monitor assigns.
    # Its own node counts as stopped while its PostgreSQL does not answer:
    # the task's later steps handle that, and a node alone in its formation
    # with a setting that keeps PostgreSQL from starting would otherwise
    # never settle, and never get its fixed file.
    rows, problem, unsettled = wait_until_the_formation_settles(lambda: read_formation(data_directory, inputs["formation"] if monitor_disabled else "", monitor),
                                                                [], lambda: [] if postgresql_answers(binaries, inputs["port"]) else [inputs["name"]])
    if problem:
        # An earlier node task may start the monitor.
        if is_dryrun():
            return {"status": "not_checked", "reason": MODULE + ": the monitor does not answer: " + problem}
        return failed_after_creation(failed("reading the node %s from the monitor: %s" % (inputs["name"], problem)), created)
    if unsettled:
        return failed_after_creation(not_settled(inputs["formation"], unsettled), created)
    row = node_of(rows, inputs["name"])
    if row == None:
        known, problem = monitor_has_a_record_of(data_directory, inputs["formation"], inputs["name"], monitor)
        if problem:
            return failed_after_creation(failed("reading the events of the formation %s from the monitor: %s" % (inputs["formation"], problem)), created)
        if known:
            return {"status": "invalid_inputs",
                    "reason": ("%s: the monitor does not list the node %s of %s in the formation %s: it was dropped from the monitor. pg_autoctl cannot add " +
                               "a dropped node again: remove it with state: deleted_with_all_its_data, and create it again under a new name") %
                              (MODULE, inputs["name"], data_directory, inputs["formation"])}
        ended = register_with_a_monitor_without_its_record(binaries, data_directory, inputs, rows, monitor_disabled, reasons)
        if ended:
            return ended
        if is_dryrun():
            # The monitor gives the node its state once it registers.
            return {"reason": "; ".join(reasons), "outputs": {"created": False}, "fingerprint": fingerprint}
        rows, problem = read_formation(data_directory, "")
        if problem:
            return failed("reading the node %s from the monitor: %s" % (inputs["name"], problem))
        row = node_of(rows, inputs["name"])
        if row == None:
            return failed("the monitor does not list the node %s after pg_autoctl enable monitor registered it" % inputs["name"])

    task_settings, failure = wanted_settings(inputs)
    if failure:
        return failed_after_creation(failure, created)
    settings_outcome = {"changed": [], "restarted": False, "waiting": [], "waiting_reason": "", "started_with_the_file": False, "in_maintenance": False}
    if task_settings != None:
        refusal = settings_refused_by_the_formation(binaries, inputs, task_settings, rows, row)
        if refusal:
            return failed_after_creation(refusal, created)

    # After a reboot in maintenance, pg_autoctl keeps the node's PostgreSQL
    # stopped. present ends the maintenance first, after which pg_autoctl
    # starts it; a node that stays in maintenance needs none that runs.
    node_in_maintenance = row.get("assigned_group_state", "") in MAINTENANCE_STATES
    stays_in_maintenance = state == "in_maintenance" and node_in_maintenance
    postgresql_answered = task_settings == None or postgresql_answers(binaries, inputs["port"])
    if not postgresql_answered:
        # PostgreSQL starts with the settings, so they go into the file
        # before anything waits for it, and a node whose PostgreSQL does not
        # run gets its service restarted.
        failure = write_the_settings_while_postgresql_does_not_run(binaries, inputs, task_settings, stays_in_maintenance, settings_outcome, reasons)
        if failure:
            return failed_after_creation(failure, created)
    if state == "present" and node_in_maintenance:
        command = run_command_that_changes_the_node(pg_autoctl(["disable", "maintenance", "--pgdata", data_directory]))
        if command.error or command.exit_code != 0:
            return failed_after_creation(failed("ending the maintenance of the node %s: %s" % (inputs["name"], message_of(command))), created)
        reasons.append(did("ended", "end", "the maintenance of the node %s" % inputs["name"]))
    if not is_dryrun() and not stays_in_maintenance and not wait_until_postgresql_answers([binaries + "/pg_isready"], "", inputs["port"]):
        restarted = ("restarted %s, as the node's PostgreSQL did not run, and a minute later " % UNIT_NAME) if settings_outcome["restarted"] else ""
        failure = failed("%sthe node's PostgreSQL in %s does not accept connections on port %d; read journalctl -u %s" % (restarted, data_directory, inputs["port"], UNIT_NAME))
        return failed_after_creation(failure, created)

    replaced, failure = replace_trust_of_initdb(data_directory, "peer", reasons)
    if not failure and replaced:
        failure = reload_postgresql(binaries, data_directory, reasons)
    if failure:
        return failed_after_creation(failure, created)

    the_primary = is_primary(row)
    if configuration != None and setting(configuration, "postgresql", "listen_addresses") != inputs["listen_address"]:
        if the_primary and len(rows) > 1:
            # An earlier node task, pg_auto_failover_primary, may make another
            # node the primary.
            return {"status": "missing_prerequisite",
                    "reason": ("%s: the node %s is the primary of the formation %s (%s), and a new listen_address takes a restart of its service, " +
                               "after which the monitor fails over to a replica: make another node the primary first with pg_auto_failover_primary") %
                              (MODULE, inputs["name"], inputs["formation"], row.get("assigned_group_state"))}
        failure = keep_the_listen_address(binaries, data_directory, configuration, inputs, reasons)
        if failure:
            return failed_after_creation(failure, created)

    if the_primary:
        failure = make_the_replication_password_work(binaries, inputs, created or replicator_differed, reasons)
        if failure:
            return failed_after_creation(failure, created)
    # Only the primary sets the password; PostgreSQL copies it to the others.
    failure = forget_that_the_replication_password_is_not_set(home)
    if failure:
        return failed_after_creation(failure, created)

    for setting_name, key, wanted, text in [
        ("candidate-priority", "candidate_priority", inputs["candidate_priority"], str(inputs["candidate_priority"])),
        ("replication-quorum", "replication_quorum", inputs["replication_quorum"], "true" if inputs["replication_quorum"] else "false"),
    ]:
        if row.get(key) == wanted:
            continue
        command = run_command_that_changes_the_node(pg_autoctl(["set", "node", setting_name, "--pgdata", data_directory, text]))
        if command.error or command.exit_code != 0:
            return failed_after_creation(failed("setting %s of the node %s to %s: %s" % (key, inputs["name"], text, message_of(command))), created)
        reasons.append(did("set", "set", "%s of the node %s to %s" % (key, inputs["name"], text)))

    # The settings need a PostgreSQL that runs: always in a run out of
    # maintenance, which waited for it above.
    if task_settings != None and (postgresql_answered or not (is_dryrun() or stays_in_maintenance)):
        ended = keep_the_postgresql_settings(binaries, inputs, task_settings, rows, row, created, stays_in_maintenance, settings_outcome, reasons)
        if ended:
            return failed_after_creation(ended, created)
        if settings_outcome["in_maintenance"]:
            # The restart for the settings put the node into maintenance,
            # where the task keeps it.
            node_in_maintenance, stays_in_maintenance = True, True

    current = row.get("current_group_state", "")
    if state == "in_maintenance" and not node_in_maintenance:
        if the_primary:
            # An earlier node task, pg_auto_failover_primary, may make another
            # node the primary.
            return {"status": "missing_prerequisite",
                    "reason": ("%s: the node %s is the primary of the formation %s (%s); maintenance on the primary would cause a failover that the task " +
                               "does not ask for. Make another node the primary first with pg_auto_failover_primary") % (MODULE, inputs["name"], inputs["formation"], row.get("assigned_group_state"))}
        command = run_command_that_changes_the_node(pg_autoctl(["enable", "maintenance", "--pgdata", data_directory]))
        if command.error or command.exit_code != 0:
            return failed_after_creation(failed("putting the node %s into maintenance: %s" % (inputs["name"], message_of(command))), created)
        reasons.append(did("put", "put", "the node %s into maintenance" % inputs["name"]))

    if reasons and not is_dryrun():
        # The task ends on a settled formation in which this node rests,
        # healthy, so that the next node of a fleet stage that runs one node
        # at a time starts on a settled formation too. A node that stays in
        # maintenance may have its PostgreSQL stopped, so it need not be
        # healthy.
        rows, problem, unsettled = wait_until_the_formation_settles(lambda: read_formation(data_directory, ""), [] if stays_in_maintenance else [inputs["name"]])
        if problem:
            return failed_after_creation(failed("reading the node %s from the monitor: %s" % (inputs["name"], problem)), created)
        row = node_of(rows, inputs["name"])
        if row == None:
            return failed_after_creation(failed("the monitor no longer lists the node " + inputs["name"]), created)
        if unsettled:
            return failed_after_creation(not_settled(inputs["formation"], unsettled), created)
        current = row.get("current_group_state", "")
    result = {"outputs": {"created": created, "current_state": current, "is_primary": is_primary(row)}, "fingerprint": fingerprint}
    if task_settings != None:
        # Whether a run restarts and what stays pending, only a run knows.
        result["outputs"]["changed_postgresql_settings"] = settings_outcome["changed"]
        if not is_dryrun():
            result["outputs"]["restarted_postgresql"] = settings_outcome["restarted"]
            result["outputs"]["postgresql_settings_waiting_for_a_restart"] = settings_outcome["waiting"]
        # A setting that waits for a restart is told also when nothing
        # changed, and is no change to settle after.
        if settings_outcome["waiting_reason"]:
            reasons.append(settings_outcome["waiting_reason"])
    if reasons:
        result["reason"] = "; ".join(reasons)
    return result

def forget_that_the_replication_password_is_not_set(home):
    """Removes the file REPLICATION_PASSWORD_NOT_SET_YET once the primary has
    set the replication password, or the node is not the primary. Returns
    the failed result or None."""
    marker = home + REPLICATION_PASSWORD_NOT_SET_YET
    found = inspect_path(marker)
    if found.error:
        return failed("inspecting " + marker + ": " + found.error)
    if found.exists:
        removed = remove_path(marker)
        if removed.error:
            return failed("removing " + marker + ": " + removed.error)
    return None

def make_the_replication_password_work(binaries, inputs, unknown, reasons):
    """Sets the password of pgautofailover_replicator on the primary, so that
    a node that joins later can copy, when the role has none, or when the
    task created the node or its password file held another replication
    password before (unknown). A login cannot compare it here:
    pg_autoctl lets pgautofailover_replicator in only from the other nodes'
    addresses, and the loopback addresses, where the module's pg_hba.conf
    lines ask for a password, need not be open (listen_address). Returns
    the failed result or None."""
    if not unknown:
        query = "select rolpassword is null from pg_authid where rolname = 'pgautofailover_replicator';\n"
        asked = run_command(psql(binaries, inputs["port"], "postgres"), standard_input = query)
        if asked.error or asked.exit_code != 0:
            return failed("reading whether pgautofailover_replicator has a password: " + (asked.error or first_error_line(asked.standard_error)))
        if asked.standard_output.strip() == "f":
            return None
    command = set_password(binaries, inputs["port"], "postgres", "pgautofailover_replicator", inputs["replication_password"])
    if command.error or command.exit_code != 0:
        return failed("setting the password of pgautofailover_replicator: " + (command.error or first_error_line(command.standard_error)))
    reasons.append(did("set", "set", "the password of pgautofailover_replicator on the primary " + inputs["name"]))
    return None

def restored_cluster_problem(data_directory, inputs):
    """Returns the result for a data directory that holds a PostgreSQL
    cluster while the formation has nodes, or None. pg_autoctl registers
    such a cluster as the primary of a formation without nodes; in a
    formation with a primary it would replace the cluster with a copy of
    the primary."""
    found = inspect_path(data_directory + "/PG_VERSION")
    if found.error:
        return failed("inspecting " + data_directory + ": " + found.error)
    if not found.exists:
        return None
    shown = run_command(pg_autoctl(["show", "state", "--monitor", node_monitor_uri(inputs["monitor_host_name"], inputs["monitor_port"]),
                                    "--formation", inputs["formation"], "--json"]))
    rows = json.decode(shown.standard_output, default = None) if not shown.error and shown.exit_code == 0 else None
    if type(rows) != "list":
        return {"status": "not_checked" if is_dryrun() else "failed",
                "reason": MODULE + ": reading the nodes of the formation " + inputs["formation"] + " from the monitor: " + message_of(shown)}
    if rows:
        names = ", ".join([row.get("nodename", "") for row in rows if type(row) == "dict"])
        # An earlier pg_auto_failover_monitor task may drop the nodes.
        return {"status": "missing_prerequisite",
                "reason": ("%s: %s holds a PostgreSQL cluster, and the formation %s has the nodes %s: pg_autoctl would replace the cluster with a copy " +
                           "of the primary. Drop the formation's nodes first with pg_auto_failover_monitor, or empty the directory") %
                          (MODULE, data_directory, inputs["formation"], names)}
    return None

def stop_an_unregistered_cluster(binaries, data_directory, reasons):
    """Stops a PostgreSQL cluster that runs in the data directory before
    pg_autoctl registers it, such as one that pgbackrest_restore started:
    pg_autoctl create postgres refuses a cluster that runs, and the node's
    service starts it again. Returns the failed result or None."""
    found = inspect_path(data_directory + "/postmaster.pid")
    if found.error:
        return failed("inspecting " + data_directory + ": " + found.error)
    if not found.exists:
        return None
    # pg_ctl status: 0 while the cluster runs, 3 when it does not.
    status = run_command(["runuser", "-u", ACCOUNT, "--", binaries + "/pg_ctl", "status", "--pgdata", data_directory])
    if status.error or status.exit_code not in [0, 3]:
        return failed("asking pg_ctl whether the cluster in " + data_directory + " runs: " + message_of(status))
    if status.exit_code == 3:
        return None
    stopped = run_command_that_changes_the_node(["runuser", "-u", ACCOUNT, "--", binaries + "/pg_ctl", "stop", "--pgdata", data_directory,
                                                 "--mode", "fast", "--wait"])
    if stopped.error or stopped.exit_code != 0:
        return failed("stopping the cluster in " + data_directory + " before pg_autoctl registers it: " + message_of(stopped))
    reasons.append(did("stopped", "stop", "the PostgreSQL cluster that runs in " + data_directory + ", which pg_autoctl registers only while it does not run"))
    return None

def creation_failed(home, inputs, created, reason):
    """Returns the failed result of pg_autoctl create postgres. When the
    command wrote the node's configuration, the node is registered: another
    try would find it and report created: false, so the task fails once,
    and the next run finishes the creation."""
    if not created:
        return failed(reason)
    written = inspect_path(configuration_path(home, inputs["data_directory"]))
    if not written.error and not written.exists:
        return dict(failed(reason), outputs = {"created": False, "current_state": "", "is_primary": False})
    return {"status": "failed", "try_again": False, "outputs": {"created": True, "current_state": "", "is_primary": False},
            "reason": MODULE + ": " + reason + ". pg_autoctl wrote the node's configuration; the next run of the playbook runs the creation again, which finishes it"}

def failed_after_creation(failure, created):
    """Returns a failure after this try created the node, with the output
    created, and without another try: the next try would find the node and
    report created: false for what this task created."""
    if created and failure["status"] == "failed":
        failure["try_again"] = False
        failure["reason"] = MODULE + ": created the node, and then this failed: " + failure["reason"][len(MODULE) + 2:]
    failure["outputs"] = {"created": created, "current_state": "", "is_primary": False}
    return failure

def remove_the_node(state, data_directory, configuration, unit, fingerprint, home):
    """absent: drops the node from its formation, stops its service, and
    removes its unit; the data directory and the configuration stay.
    deleted_with_all_its_data: also deletes them (pg_autoctl drop node
    --destroy). The service stops first: pg_autoctl's drop ends the running
    keeper, and systemd would start it again."""
    result = {"fingerprint": fingerprint}
    reasons = []
    if configuration == None:
        if unit != None:
            failure = remove_the_unit(reasons)
            if failure:
                return failure
        if state == "deleted_with_all_its_data":
            failure = remove_the_settings_file(reasons)
            if failure:
                return failure
        if reasons:
            result["reason"] = "; ".join(reasons)
        return result
    name = setting(configuration, "pg_autoctl", "name")
    rows, problem = read_formation(data_directory, "")
    if problem:
        if is_dryrun():
            return {"status": "not_checked", "reason": MODULE + ": the monitor does not answer: " + problem}
        return failed("reading the node %s from the monitor: %s" % (name, problem))
    listed = node_of(rows, name) != None
    if unit != None:
        failure = remove_the_unit(reasons)
        if failure:
            return failure
    if state == "deleted_with_all_its_data" and listed:
        command = run_command_that_changes_the_node(pg_autoctl(["drop", "node", "--pgdata", data_directory, "--destroy"]))
        if command.error or command.exit_code != 0:
            return failed("deleting the node %s in %s: %s" % (name, data_directory, message_of(command)))
        reasons.append(did("deleted", "delete", "the node %s in %s with all its data" % (name, data_directory)))
    elif state == "deleted_with_all_its_data":
        failure = delete_a_dropped_node(configuration, data_directory, home, name, reasons)
        if failure:
            return failure
    elif listed:
        command = run_command_that_changes_the_node(pg_autoctl(["drop", "node", "--pgdata", data_directory]))
        if command.error or command.exit_code != 0:
            return failed("dropping the node %s from its formation: %s" % (name, message_of(command)))
        reasons.append(did("removed", "remove", "the node %s from its formation; its data directory %s stays" % (name, data_directory)))
    if state == "deleted_with_all_its_data":
        failure = remove_the_settings_file(reasons)
        if failure:
            return failure
    if reasons:
        result["reason"] = "; ".join(reasons)
    return result

def delete_a_dropped_node(configuration, data_directory, home, name, reasons):
    """Deletes a node that the monitor no longer lists: pg_autoctl drop node
    --destroy asks the monitor first and fails for it, also with --force,
    while its keeper has not run since the monitor dropped it. The module
    stops the cluster with the pg_ctl of pg_autoctl's configuration, and
    removes the data directory and pg_autoctl's configuration and state, as
    --destroy does. Returns the failed result or None."""
    pg_ctl = setting(configuration, "postgresql", "pg_ctl")
    status = run_command(["runuser", "-u", ACCOUNT, "--", pg_ctl, "status", "--pgdata", data_directory]) if pg_ctl else None
    if status != None and not status.error and status.exit_code == 0:
        stopped = run_command_that_changes_the_node(["runuser", "-u", ACCOUNT, "--", pg_ctl, "stop", "--pgdata", data_directory, "--mode", "fast", "--wait"])
        if stopped.error or stopped.exit_code != 0:
            return failed("stopping the cluster in %s before deleting it: %s" % (data_directory, message_of(stopped)))
    found = inspect_path(data_directory)
    if found.error:
        return failed("inspecting " + data_directory + ": " + found.error)
    if found.exists:
        removed = run_command_that_changes_the_node(["rm", "-r", "--one-file-system", "--preserve-root=all", "--", data_directory])
        if removed.error or removed.exit_code != 0:
            return failed("deleting the data directory " + data_directory + ": " + message_of(removed))
    for path in [configuration_path(home, data_directory)] + [home + "/.local/share/pg_autoctl" + data_directory + "/pg_autoctl." + kind for kind in ["state", "init"]]:
        removed = remove_path(path)
        if removed.error:
            return failed("removing " + path + ": " + removed.error)
    reasons.append(did("deleted", "delete", "the node %s in %s with all its data, which the monitor had dropped already" % (name, data_directory)))
    return None

# ---------------------------------------------------------------------------
# The node's PostgreSQL settings: postgresql_settings and
# restart_postgresql_for_new_settings.

# The module's file of PostgreSQL settings. It lies outside the data
# directory, which pg_basebackup and pg_rewind copy from the primary, so each
# node keeps values sized from its own memory. The data directory's
# postgresql.conf, which those copies bring along, includes it as its last
# setting line: pg_autoctl includes its own files at the top of
# postgresql.conf, so the module's file wins over them and over initdb's
# lines.
SETTINGS_FILE = "/etc/postgresql/pg_auto_failover_node_settings.conf"
INCLUDE_LINE = "include_if_exists = '" + SETTINGS_FILE + "'"
SETTINGS_FILE_HEADER = ("# The PostgreSQL settings of this node, from the input postgresql_settings of MMX's module\n" +
                        "# pg_auto_failover_node. Each run of the task writes this file again: change the task, not the file.\n")

# The queries of the settings: each line of every configuration file that
# PostgreSQL reads, in the order in which it reads them; and each setting
# with whether it changes only with a restart (context postmaster), whether
# a new value waits for one, and the file that its value comes from.
FILE_SETTINGS_QUERY = "select name, sourcefile, sourceline from pg_file_settings order by seqno"
SETTINGS_QUERY = "select name, context, pending_restart, sourcefile, setting from pg_settings order by name"

# The characters of a setting's name in lower case. PostgreSQL ignores the
# case of a name, and pg_settings lists every name in lower case, so one
# setting has one spelling.
NAME_CHARACTERS = "abcdefghijklmnopqrstuvwxyz0123456789_"

# The settings that pg_autoctl and the module keep, which postgresql_settings
# refuses, with the reason.
STANDBY_SETTING_REASON = ("pg_autoctl writes it into postgresql-auto-failover-standby.conf to follow the current primary, and a value from the " +
                          "module's file would win over it")
SETTINGS_THE_MODULE_KEEPS = {
    "listen_addresses": "is the module's input listen_address: pg_autoctl keeps the address in its configuration and starts PostgreSQL with it",
    "port": "is the module's input port: pg_autoctl's configuration and the monitor's registration of the node hold it",
    "primary_conninfo": STANDBY_SETTING_REASON,
    "primary_slot_name": STANDBY_SETTING_REASON,
    "synchronous_standby_names": "pg_autoctl sets it on the primary with ALTER SYSTEM, as the monitor decides",
}

# The settings that pg_auto_failover needs at one value, with the values
# that the module accepts, in lower case, and the reason. A boolean is
# accepted in the words that mean on.
ON_WORDS = ["on", "true", "yes", "1"]
REQUIRED_SETTING_VALUES = {
    "hot_standby": (ON_WORDS, "must be on: the keeper and the monitor's health checks connect to a replica's PostgreSQL"),
    "wal_level": (["replica", "logical"], "must be replica or logical: with minimal, PostgreSQL writes too little for streaming replication and for pg_rewind"),
    "wal_log_hints": (ON_WORDS, "must be on: pg_rewind, which pg_autoctl runs when an old primary rejoins, needs it"),
    "ssl": (ON_WORDS, "must be on: the replicas and the monitor connect with sslmode=require"),
    "password_encryption": (["scram-sha-256"], "must be scram-sha-256: the module and pg_autoctl create scram-sha-256 logins"),
}

# The values of synchronous_commit with which a commit is acknowledged
# before a replica has it.
UNSAFE_SYNCHRONOUS_COMMIT = ["off", "local", "false", "no", "0"]

# The settings that a replica's PostgreSQL needs at least as high as its
# primary's, with the line of pg_controldata that holds the value: on a
# replica, the primary's; on the primary, its own. A replica receives the
# primary's values with its changes, and does not run with lower ones.
HOT_STANDBY_MINIMUMS = [("max_connections", "max_connections setting:"), ("max_worker_processes", "max_worker_processes setting:"),
                        ("max_wal_senders", "max_wal_senders setting:"), ("max_prepared_transactions", "max_prepared_xacts setting:"),
                        ("max_locks_per_transaction", "max_locks_per_xact setting:")]

# The levels of PostgreSQL's messages on standard error.
POSTGRESQL_MESSAGE_LEVELS = ["FATAL:", "PANIC:", "ERROR:", "WARNING:", "LOG:", "DETAIL:", "HINT:"]

def setting_name_problem(name):
    """Returns the rule that a setting's name breaks, or an empty text."""
    parts = name.split(".")
    if len(parts) > 2 or [part for part in parts if part == "" or part[0].isdigit() or [c for c in part.elems() if c not in NAME_CHARACTERS]]:
        return "the name %s is no PostgreSQL setting's name in lower case, such as work_mem or pg_stat_statements.max" % repr(name)
    return ""

def setting_text(value):
    """The text of a setting's value: a boolean as on or off, a number in
    digits."""
    if type(value) == "bool":
        return "on" if value else "off"
    if type(value) in ["int", "float"]:
        return str(value)
    return value

# The keys of a value that is a share of the server's memory.
MEMORY_SHARE_KEYS = ["at_least_mib", "percent_of_memory", "rounded_down_to_mib"]

# The server's memory, as MMX's fact memory.total_mib reads it.
MEMORY_FILE = "/proc/meminfo"

def memory_share_problem(value):
    """Returns the rule that a share of the server's memory breaks, or an
    empty text."""
    if sorted(value.keys()) != MEMORY_SHARE_KEYS or [key for key in MEMORY_SHARE_KEYS if type(value[key]) != "int"]:
        return ("must be a text, a number, true or false, or a share of the server's memory with the whole numbers percent_of_memory, " +
                "rounded_down_to_mib, and at_least_mib, such as {percent_of_memory: 25, rounded_down_to_mib: 128, at_least_mib: 128}")
    if value["percent_of_memory"] < 1 or value["percent_of_memory"] > 100:
        return "percent_of_memory must be from 1 to 100"
    if value["rounded_down_to_mib"] < 1 or value["at_least_mib"] < 0:
        return "rounded_down_to_mib must be at least 1, and at_least_mib at least 0"
    return ""

def memory_mib():
    """The server's memory in MiB: MemTotal of /proc/meminfo, rounded down,
    as MMX's fact memory.total_mib gives it. Returns it and None, or None
    and the failed result."""
    read = read_file(MEMORY_FILE)
    if read.error:
        return None, failed("reading " + MEMORY_FILE + ": " + read.error)
    for line in read.content.splitlines():
        fields = line.split()
        if len(fields) == 3 and fields[0] == "MemTotal:" and fields[1].isdigit() and fields[2] == "kB":
            return int(fields[1]) // 1024, None
    return None, failed(MEMORY_FILE + " holds no line MemTotal in kB")

def share_of_memory(value, total_mib):
    """A share of the server's memory as PostgreSQL's text, such as 2048MB:
    percent_of_memory of it, rounded down to a multiple of
    rounded_down_to_mib, and at least at_least_mib. PostgreSQL's MB is a
    MiB."""
    step = value["rounded_down_to_mib"]
    return "%dMB" % max(value["at_least_mib"], total_mib * value["percent_of_memory"] // 100 // step * step)

def setting_refused(name, text):
    """Returns why the module refuses a setting, or an empty text."""
    if name in SETTINGS_THE_MODULE_KEEPS:
        return SETTINGS_THE_MODULE_KEEPS[name]
    if name.startswith("recovery_target"):
        return STANDBY_SETTING_REASON
    if name in REQUIRED_SETTING_VALUES and text.lower() not in REQUIRED_SETTING_VALUES[name][0]:
        return REQUIRED_SETTING_VALUES[name][1]
    if name == "synchronous_commit" and text.lower() in UNSAFE_SYNCHRONOUS_COMMIT:
        return "must not be off or local: a commit is then acknowledged before a replica has it, and a failover loses writes that were acknowledged"
    return ""

def settings_problem(inputs):
    """Returns the result for the first rule that postgresql_settings or
    restart_postgresql_for_new_settings breaks, or None."""
    settings = inputs.get("postgresql_settings")
    if settings == None:
        if "restart_postgresql_for_new_settings" in inputs:
            return invalid("postgresql_settings", "is required with restart_postgresql_for_new_settings")
        return None
    if "restart_postgresql_for_new_settings" not in inputs:
        return invalid("restart_postgresql_for_new_settings", "is required with postgresql_settings: write never, on_a_replica, or on_a_replica_or_a_node_alone")
    if type(settings) != "dict":
        return invalid("postgresql_settings", "must be a mapping of PostgreSQL's setting names to their values, such as work_mem: 64MB")
    for name in sorted(settings.keys()):
        problem = setting_name_problem(name)
        if problem:
            return invalid("postgresql_settings", problem)
        value = settings[name]
        if type(value) == "dict":
            problem = memory_share_problem(value)
            if problem:
                return invalid("postgresql_settings." + name, problem)
            refused = setting_refused(name, "")
            if refused:
                return invalid("postgresql_settings." + name, refused)
            continue
        if type(value) not in ["string", "int", "float", "bool"]:
            return invalid("postgresql_settings." + name, memory_share_problem({}))
        text = setting_text(value)
        if "\n" in text or "\r" in text:
            return invalid("postgresql_settings." + name, "must not hold a line break, which a line of PostgreSQL's configuration file cannot hold")
        refused = setting_refused(name, text)
        if refused:
            return invalid("postgresql_settings." + name, refused)
    return None

def wanted_settings(inputs):
    """The task's settings as name: text, or None without
    postgresql_settings, with a share of the server's memory worked out;
    and None, or the failed result when the memory cannot be read."""
    settings = inputs.get("postgresql_settings")
    if settings == None:
        return None, None
    total_mib = 0
    if [name for name in settings if type(settings[name]) == "dict"]:
        total_mib, failure = memory_mib()
        if failure:
            return None, failure
    return {name: share_of_memory(settings[name], total_mib) if type(settings[name]) == "dict" else setting_text(settings[name]) for name in settings}, None

def whole_number(text):
    """The whole number that a text of digits gives, or None."""
    return int(text.lstrip("0") or "0") if text.isdigit() else None

def merged(names, more):
    """The names of both lists, sorted, each once."""
    return sorted({name: True for name in names + more}.keys())

def and_takes(names, singular, plural):
    """"a takes" or "a and b take"."""
    return and_joined(names) + " " + (singular if len(names) == 1 else plural)

def shown(text):
    """A value in a reason: in quotes when it is empty or holds a space."""
    return "'" + text + "'" if text == "" or " " in text else text

def settings_refused_by_the_formation(binaries, inputs, wanted, rows, row):
    """Returns the result for a setting that the formation cannot take, or
    None. Each replica takes one WAL sender and one replication slot on the
    primary. A replica's PostgreSQL needs the settings of
    HOT_STANDBY_MINIMUMS at least as high as its primary's, and stops with
    lower ones; so a replica takes no lower value than its primary's, and a
    primary with replicas lowers none, since a lower value would take a
    restart of the primary before its replicas, which on the primary is a
    failover."""
    nodes = len(rows)
    for name in ["max_wal_senders", "max_replication_slots"]:
        number = whole_number(wanted.get(name, ""))
        if number != None and number < nodes:
            return invalid("postgresql_settings." + name, "must be at least %d, the number of nodes in the formation %s: each replica takes one of each on the primary" %
                                                          (nodes, inputs["formation"]))
    guarded = [(name, label) for name, label in HOT_STANDBY_MINIMUMS if whole_number(wanted.get(name, "")) != None]
    the_primary = is_primary(row)
    if not guarded or (the_primary and nodes == 1):
        return None
    values, failure = values_of_the_control_file(binaries, inputs["data_directory"], [label for _, label in guarded])
    if failure:
        return failure
    for name, label in guarded:
        number, highest = whole_number(wanted[name]), values[label]
        if number >= highest:
            continue
        if the_primary:
            return invalid("postgresql_settings." + name, ("%d is lower than %d, with which the primary %s runs: a replica needs at least its primary's value, so a lower " +
                                                           "value takes a restart of the primary before its replicas, which on the primary is a failover; keep at least %d") %
                                                          (number, highest, inputs["name"], highest))
        return invalid("postgresql_settings." + name, ("%d is lower than %d, the primary's value, which pg_controldata reports on this replica: PostgreSQL does not run a " +
                                                       "replica with a lower value than its primary's. Keep at least %d; a higher value goes to the replicas first, then to the primary") %
                                                      (number, highest, highest))
    return None

def values_of_the_control_file(binaries, data_directory, labels):
    """Returns the numbers that pg_controldata prints on the lines of labels,
    as label: number, and None; or None and the failed result."""
    printed = run_command(["runuser", "-u", ACCOUNT, "--", binaries + "/pg_controldata", "-D", data_directory])
    if printed.error or printed.exit_code != 0:
        return None, failed("reading the control file of the cluster in %s with pg_controldata: %s" % (data_directory, message_of(printed)))
    values = {}
    for line in printed.standard_output.splitlines():
        label, colon, value = line.partition(":")
        if colon and label.strip() + ":" in labels and whole_number(value.strip()) != None:
            values[label.strip() + ":"] = whole_number(value.strip())
    for label in labels:
        if label not in values:
            return None, failed("pg_controldata printed no line %s for the cluster in %s" % (repr(label), data_directory))
    return values, None

def postgresql_answers(binaries, port):
    """Whether the node's PostgreSQL accepts connections now."""
    asked = run_command([binaries + "/pg_isready", "--quiet", "--port", str(port)])
    return not asked.error and asked.exit_code == 0

def postgresql_runs(binaries, port):
    """Whether a PostgreSQL server runs on the port, also one that refuses
    connections for now, such as while it starts or recovers: pg_isready
    exits with 2 when no server answers at all."""
    asked = run_command([binaries + "/pg_isready", "--quiet", "--port", str(port)])
    return not asked.error and asked.exit_code in [0, 1]

def settings_file_content(wanted):
    """The module's file for the settings: its header, then one line for each
    setting, sorted by name. PostgreSQL reads a backslash in a quoted value as
    the start of an escape, and two quotes as one, so both are doubled."""
    lines = [SETTINGS_FILE_HEADER]
    for name in sorted(wanted.keys()):
        lines.append("%s = '%s'\n" % (name, wanted[name].replace("\\", "\\\\").replace("'", "''")))
    return "".join(lines)

def unquoted(text):
    """The value of a configuration file's line, after its equals sign: a
    quoted value without its quotes and escapes, or a bare value without a
    comment."""
    if not text.startswith("'"):
        return text.partition("#")[0].strip()
    value = ""
    escaped = False
    quote = False
    for character in text[1:].elems():
        if escaped:
            value += character
            escaped = False
        elif quote:
            if character != "'":
                break
            value += "'"
            quote = False
        elif character == "\\":
            escaped = True
        elif character == "'":
            quote = True
        else:
            value += character
    return value

def settings_of_the_file(content):
    """The settings of a configuration file as name: value."""
    found = {}
    for line in content.splitlines():
        line = line.strip()
        if not line or line.startswith("#"):
            continue
        name, equals, value = line.partition("=")
        if equals:
            found[name.strip().lower()] = unquoted(value.strip())
    return found

def is_the_include(line):
    """Whether a line of postgresql.conf includes the module's file."""
    return " ".join(line.partition("#")[0].replace("=", " ").split()) == "include_if_exists '" + SETTINGS_FILE + "'"

def with_the_include_at_the_end(content):
    """Returns postgresql.conf with the module's include as its last setting
    line, and what the module did to the line: "", "added", or "moved"."""
    lines = content.split("\n")
    setting_lines = [line for line in lines if line.strip() and not line.strip().startswith("#")]
    if setting_lines and is_the_include(setting_lines[-1]):
        return content, ""
    kept = [line for line in lines if not is_the_include(line)]
    text = "\n".join(kept)
    if text and not text.endswith("\n"):
        text += "\n"
    return text + INCLUDE_LINE + "\n", "moved" if len(kept) < len(lines) else "added"

def plan_the_settings(data_directory, wanted):
    """Reads the module's file and the data directory's postgresql.conf, and
    returns what the task writes into them, with their old contents, so that
    a refused change can be put back, and None; or None and the failed
    result."""
    found = inspect_path(SETTINGS_FILE)
    if found.error:
        return None, failed("inspecting " + SETTINGS_FILE + ": " + found.error)
    old = None
    if found.exists:
        read = read_file(SETTINGS_FILE)
        if read.error:
            return None, failed("reading " + SETTINGS_FILE + ": " + read.error)
        old = read.content
    path = data_directory + "/postgresql.conf"
    configuration = inspect_path(path)
    if configuration.error or not configuration.exists:
        return None, failed("inspecting " + path + ": " + (configuration.error or "it does not exist"))
    read = read_file(path)
    if read.error:
        return None, failed("reading " + path + ": " + read.error)
    new_configuration, include = with_the_include_at_the_end(read.content)
    before = settings_of_the_file(old or "")
    changed = [name for name in merged(list(before.keys()), list(wanted.keys())) if before.get(name) != wanted.get(name)]
    return {"old": old, "new": settings_file_content(wanted), "before": before, "wanted": wanted, "changed": changed, "configuration_file": path,
            "configuration": configuration, "old_configuration": read.content, "new_configuration": new_configuration, "include": include}, None

def write_the_settings(plan, reasons):
    """Writes the module's file and the include line of postgresql.conf where
    they differ, and says what changed. Returns whether a file changed, and
    the failed result or None."""
    written = write_file(SETTINGS_FILE, plan["new"], owner = "root", group = ACCOUNT, permissions_mode = "0644")
    if written.error:
        return False, failed("writing " + SETTINGS_FILE + ": " + written.error)
    changed = written.changed
    before, wanted = plan["before"], plan["wanted"]
    values = [name + (" from " + shown(before[name]) if name in before else "") + " to " + shown(wanted[name]) for name in plan["changed"] if name in wanted]
    removed = [name for name in plan["changed"] if name not in wanted]
    if values:
        reasons.append(did("set", "set", and_joined(values) + " in " + SETTINGS_FILE))
    if removed:
        reasons.append(did("removed", "remove", and_joined(removed) + " from " + SETTINGS_FILE))
    if changed and not values and not removed:
        reasons.append(did("wrote", "write", SETTINGS_FILE))
    if plan["include"]:
        found = plan["configuration"]
        written = write_file(plan["configuration_file"], plan["new_configuration"], owner = found.owner, group = found.group, permissions_mode = found.permissions_mode)
        if written.error:
            return changed, failed("writing " + plan["configuration_file"] + ": " + written.error)
        changed = True
        if plan["include"] == "added":
            reasons.append(did("added", "add", "the line " + INCLUDE_LINE + " at the end of " + plan["configuration_file"]))
        else:
            reasons.append(did("moved", "move", "the line " + INCLUDE_LINE + " to the end of " + plan["configuration_file"]))
    return changed, None

def put_back_the_settings(binaries, data_directory, plan, refusal, reload):
    """Puts the module's file and postgresql.conf back as the plan read them,
    and reloads PostgreSQL when it read the new ones, so that nothing
    changed. Returns refusal, or the failure to put them back."""
    if plan["old"] == None:
        restored = remove_path(SETTINGS_FILE)
    else:
        restored = write_file(SETTINGS_FILE, plan["old"], owner = "root", group = ACCOUNT, permissions_mode = "0644")
    failure = failed("putting back " + SETTINGS_FILE + ": " + restored.error) if restored.error else None
    if not failure and plan["include"]:
        found = plan["configuration"]
        restored = write_file(plan["configuration_file"], plan["old_configuration"], owner = found.owner, group = found.group, permissions_mode = found.permissions_mode)
        failure = failed("putting back " + plan["configuration_file"] + ": " + restored.error) if restored.error else None
    if not failure and reload:
        failure = reload_postgresql(binaries, data_directory, [])
    if failure:
        return dict(failure, reason = failure["reason"] + ", after this: " + refusal["reason"], try_again = False)
    return refusal

def postgresql_words(text):
    """PostgreSQL's messages on standard error, without their time and
    process: "invalid value for parameter ...; HINT: ...". """
    words = []
    for line in text.splitlines():
        line = line.strip()
        for level in POSTGRESQL_MESSAGE_LEVELS:
            at = line.find(level + " ")
            if at >= 0:
                line = line[at + len(level):].strip()
                if level in ["DETAIL:", "HINT:"]:
                    line = level + " " + line
                break
        if line and line not in words:
            words.append(line)
    message = "; ".join(words)
    if len(message) > 600:
        message = message[:600] + " ..."
    return message

def refused_by_postgresql(binaries, data_directory, wanted):
    """Has PostgreSQL read its configuration with postgres -C port: it reads
    postgresql.conf with every include and postgresql.auto.conf, also while
    the server runs, and ends with an error for a value, a name, or a line
    that it refuses. pg_file_settings cannot tell: it says "setting could not
    be applied" for a valid value that waits for a restart as for an invalid
    one. Returns the result for a refusal, with PostgreSQL's words, or None."""
    checked = run_command(["runuser", "-u", ACCOUNT, "--", binaries + "/postgres", "-D", data_directory, "-C", "port"])
    if checked.error:
        return failed("having PostgreSQL read its configuration with postgres -C port: " + checked.error)
    if checked.exit_code == 0:
        return None
    words = postgresql_words(checked.standard_error) or message_of(checked)
    for name in sorted(wanted.keys()):
        if "parameter \"" + name + "\"" in words:
            return invalid("postgresql_settings." + name, "PostgreSQL refuses it: " + words + "; the earlier settings stay in effect")
    return invalid("postgresql_settings", "PostgreSQL refuses its configuration with these settings: " + words + "; the earlier settings stay in effect")

def setting_set_again(binaries, port, wanted):
    """Returns the result for a setting of the task that a later line of
    another file sets again, or None. pg_file_settings lists the lines of
    every configuration file in the order in which PostgreSQL reads them,
    and the last line of a name gives its value. ALTER SYSTEM writes
    postgresql.auto.conf, which PostgreSQL reads last; there the module's
    line shows as not applied, without an error."""
    asked = run_command(psql(binaries, port, "postgres") + ["--command", FILE_SETTINGS_QUERY])
    if asked.error or asked.exit_code != 0:
        return failed("reading pg_file_settings: " + (asked.error or first_error_line(asked.standard_error)))
    last = {}
    for line in asked.standard_output.splitlines():
        fields = line.split("|")
        if len(fields) == 3:
            last[fields[0].lower()] = fields
    for name in sorted(wanted.keys()):
        entry = last.get(name)
        if entry == None:
            return invalid("postgresql_settings." + name, "PostgreSQL reads no line of %s for it (pg_file_settings), so it does not read the file; the earlier settings stay in effect" % SETTINGS_FILE)
        if entry[1] == SETTINGS_FILE:
            continue
        remove = "remove it there with ALTER SYSTEM RESET " + name if entry[1].endswith("/postgresql.auto.conf") else "remove the line there"
        return invalid("postgresql_settings." + name, "%s sets it again at line %s, which PostgreSQL reads after %s, so the task's value would not be live: %s; the earlier settings stay in effect" %
                                                      (entry[1], entry[2], SETTINGS_FILE, remove))
    return None

def read_pg_settings(binaries, port):
    """Returns pg_settings as name: {context, pending_restart, sourcefile,
    setting}, and None; or None and the failed result. setting is the value
    that PostgreSQL runs with, in the setting's own unit."""
    asked = run_command(psql(binaries, port, "postgres") + ["--command", SETTINGS_QUERY])
    if asked.error or asked.exit_code != 0:
        return None, failed("reading pg_settings: " + (asked.error or first_error_line(asked.standard_error)))
    found = {}
    for line in asked.standard_output.splitlines():
        fields = line.split("|", 4)
        if len(fields) == 5:
            found[fields[0]] = {"context": fields[1], "pending_restart": fields[2] == "t", "sourcefile": fields[3], "setting": fields[4]}
    return found, None

def waiting_for_a_restart(found, names):
    """The settings of names that PostgreSQL reports pending_restart for.
    Only the module's own: after a promotion, pg_autoctl removes the
    standby's recovery_target_timeline, which PostgreSQL then reports
    pending until a restart that nothing needs."""
    return sorted([name for name in names if name in found and found[name]["pending_restart"]])

def runs_with(binaries, data_directory, name, text, running):
    """Whether PostgreSQL makes of the task's text for a setting the value
    that it runs with, running, from pg_settings.setting. postgres -C prints
    the value of the text in the setting's own unit, as pg_settings does, so
    that 2GB and 2048MB are the same. A setting that the task removes,
    without a text, may change."""
    if text == None:
        return False
    asked = run_command(["runuser", "-u", ACCOUNT, "--", binaries + "/postgres", "-D", data_directory, "-C", name, "-c", name + "=" + text])
    return not asked.error and asked.exit_code == 0 and asked.standard_output.strip() == running

def restart_decision(inputs, row, rows, created, pending):
    """How the task restarts PostgreSQL for the settings that wait for a
    restart: "in_place", "through_maintenance", "wait", or "refuse", with
    the node as a reason names it, or the text of the wait or the refusal.
    The primary of a formation with replicas never restarts in place, which
    would be a failover. A node that this task created serves nobody yet."""
    choice, name, formation = inputs["restart_postgresql_for_new_settings"], inputs["name"], inputs["formation"]
    waits = and_takes(pending, "would wait" if is_dryrun() else "waits", "would wait" if is_dryrun() else "wait") + " for a restart of PostgreSQL; "
    if created:
        return "in_place", "on the node %s, which this task created," % name
    if len(rows) == 1:
        if choice == "on_a_replica_or_a_node_alone":
            return "in_place", "on the node %s, alone in the formation %s," % (name, formation)
        if choice == "never":
            return "wait", waits + "run this task with restart_postgresql_for_new_settings: on_a_replica_or_a_node_alone on this node"
        return "refuse", ("the node %s is alone in the formation %s, and %s a restart of PostgreSQL, which restart_postgresql_for_new_settings: on_a_replica makes " +
                          "only on a replica: run this task with on_a_replica_or_a_node_alone to restart this node in place; the earlier settings stay in effect") % (name, formation, and_takes(pending, "takes", "take"))
    if is_primary(row):
        if choice == "never":
            return "wait", waits + "make another node the primary with pg_auto_failover_primary, then run this task with restart_postgresql_for_new_settings: on_a_replica on this node"
        return "refuse", ("the node %s is the primary of the formation %s, and %s a restart of PostgreSQL, which on the primary is a failover: make another node the " +
                          "primary first with pg_auto_failover_primary, then run this task again; the earlier settings stay in effect") % (name, formation, and_takes(pending, "takes", "take"))
    if choice == "never":
        return "wait", waits + "run this task with restart_postgresql_for_new_settings: on_a_replica on this node"
    return "through_maintenance", "on the replica " + name

def write_the_settings_while_postgresql_does_not_run(binaries, inputs, wanted, stays_in_maintenance, settings, reasons):
    """Writes the settings while the node's PostgreSQL does not answer, before
    the task waits for it, and restarts pgautofailover.service while no
    PostgreSQL runs, so that pg_autoctl starts one with the file: a fixed
    value repairs a node whose PostgreSQL did not start with a setting. A
    node whose PostgreSQL does not run serves nobody, a replica or a
    primary, so the restart takes nothing from anyone. In maintenance, the
    restart leaves PostgreSQL stopped, and present ends the maintenance
    after it; a node that stays in maintenance only gets the file, with which
    PostgreSQL starts when present ends the maintenance. Returns the result
    that ends the task, or None."""
    plan, failure = plan_the_settings(inputs["data_directory"], wanted)
    if failure:
        return failure
    written, failure = write_the_settings(plan, reasons)
    if failure:
        return failure
    settings["changed"] = plan["changed"]
    if written and not is_dryrun():
        refusal = refused_by_postgresql(binaries, inputs["data_directory"], wanted)
        if refusal:
            return put_back_the_settings(binaries, inputs["data_directory"], plan, refusal, False)
    if stays_in_maintenance:
        if written:
            settings["started_with_the_file"] = True
            reasons.append("PostgreSQL, which does not run in maintenance, %s with them when present ends the maintenance" % ("would start" if is_dryrun() else "starts"))
        return None
    if postgresql_runs(binaries, inputs["port"]):
        # It is starting, such as after a reboot; the module reloads it once
        # it answers.
        if written:
            settings["started_with_the_file"] = True
        return None
    # After PostgreSQL failed to start, pg_autoctl may never start it again:
    # on the test servers, the process that started PostgreSQL for the crash
    # recovery before pg_rewind kept it as a defunct process, and logged
    # "Failed to get Postgres pid" every 30 seconds. After a restart of the
    # service, PostgreSQL started within 4 seconds.
    restarted = run_command_that_changes_the_node(["systemctl", "restart", UNIT_NAME])
    if restarted.error or restarted.exit_code != 0:
        return failed("restarting %s, as the node's PostgreSQL does not run: %s" % (UNIT_NAME, message_of(restarted)))
    settings["started_with_the_file"], settings["restarted"] = True, True
    them = "them" if written else "the settings of " + SETTINGS_FILE
    if is_dryrun():
        reasons.append("would restart %s so that PostgreSQL starts with %s" % (UNIT_NAME, them))
    else:
        reasons.append("restarted %s, as the node's PostgreSQL did not run, so that PostgreSQL starts with %s" % (UNIT_NAME, them))
    return None

def keep_the_postgresql_settings(binaries, inputs, wanted, rows, row, created, in_maintenance, settings, reasons):
    """Brings the module's file and the running PostgreSQL to the task's
    settings: writes the file, has PostgreSQL check it, makes sure that no
    later file sets a setting again, reloads, and restarts PostgreSQL where
    restart_postgresql_for_new_settings allows it, for every setting that
    waits for a restart: also one that an earlier run or a person left. A
    refused change is put back. Returns the result that ends the task, or
    None; settings takes what the outputs report."""
    data_directory, port = inputs["data_directory"], inputs["port"]
    if settings["started_with_the_file"] and not is_dryrun():
        # pg_autoctl may restart the PostgreSQL that just started with the
        # file, as it sets the node up again as a replica: on the test
        # servers a query a moment after PostgreSQL answered found it
        # shutting down. Once the formation has settled with this node
        # healthy, pg_autoctl is done with it.
        rows, problem, unsettled = wait_until_the_formation_settles(lambda: read_formation(data_directory, ""), [inputs["name"]])
        if problem:
            return failed("reading the node %s from the monitor: %s" % (inputs["name"], problem))
        if unsettled:
            return not_settled(inputs["formation"], unsettled)
        row = node_of(rows, inputs["name"]) or row
    plan, failure = plan_the_settings(data_directory, wanted)
    if failure:
        return failure
    written, failure = write_the_settings(plan, reasons)
    if failure:
        return failure
    settings["changed"] = merged(settings["changed"], plan["changed"])
    if not is_dryrun():
        refusal = refused_by_postgresql(binaries, data_directory, wanted) if written else None
        refusal = refusal or setting_set_again(binaries, port, wanted)
        if refusal:
            return put_back_the_settings(binaries, data_directory, plan, refusal, False) if written else refusal
    # A file written while PostgreSQL did not answer may have come after
    # PostgreSQL read its configuration, as it started.
    if written or settings["started_with_the_file"]:
        failure = reload_postgresql(binaries, data_directory, reasons if written else [])
        if failure:
            return failure
    found, failure = read_pg_settings(binaries, port)
    if failure:
        return failure
    # The settings that the file held before the task count too: one that
    # the task removes reverts with the restart.
    pending = waiting_for_a_restart(found, merged(list(wanted.keys()), list(plan["before"].keys())))
    if is_dryrun():
        # A dryrun changes nothing, so pg_settings tells what waits with the
        # files as they are. A setting that the task changes, and that
        # changes only with a restart, would wait unless PostgreSQL runs with
        # the value of the task's text already: such as the value that it
        # runs with while the file's other one waits, or another file's equal
        # line before the module's include exists.
        changes = merged(list(wanted.keys()), list(plan["before"].keys())) if plan["include"] else plan["changed"]
        candidates = [name for name in changes if found.get(name, {}).get("context") == "postmaster"]
        pending = merged([name for name in pending if name not in changes],
                         [name for name in candidates if not runs_with(binaries, data_directory, name, wanted.get(name), found[name]["setting"])])
    settings["waiting"] = pending
    if not pending:
        return None
    decision, text = restart_decision(inputs, row, rows, created, pending)
    if decision == "refuse":
        refusal = {"status": "missing_prerequisite", "reason": MODULE + ": " + text}
        return put_back_the_settings(binaries, data_directory, plan, refusal, True) if written and not is_dryrun() else refusal
    if decision == "wait":
        settings["waiting_reason"] = text
        return None
    return restart_for_the_settings(binaries, inputs, decision, text, in_maintenance, pending, wanted, settings, reasons)

def restart_for_the_settings(binaries, inputs, decision, node, in_maintenance, pending, wanted, settings, reasons):
    """Restarts PostgreSQL for the settings that wait for it. A replica goes
    into maintenance first, so that the primary stops waiting for it: in
    maintenance, a restart leaves PostgreSQL stopped, and pg_autoctl starts
    it when the maintenance ends. A replica that the task keeps in
    maintenance only restarts. Then the module checks that each of the
    task's settings comes from its file. Returns the result that ends the
    task, or None."""
    name, data_directory, port = inputs["name"], inputs["data_directory"], inputs["port"]
    keeps_maintenance = inputs.get("state", "present") == "in_maintenance"
    what = "PostgreSQL %s for %s" % (node, and_joined(pending))
    if decision == "through_maintenance":
        what = "PostgreSQL %s %s for %s" % (node, "in maintenance" if keeps_maintenance else "through maintenance", and_joined(pending))
    if is_dryrun():
        # The restart does not run in a dryrun; MMX reports what would change.
        run_command_that_changes_the_node(["systemctl", "restart", UNIT_NAME])
        reasons.append("would restart " + what)
        return None
    if decision == "through_maintenance" and not in_maintenance:
        failure = put_into_maintenance_for_a_restart(inputs, pending)
        if failure:
            return failure
        if keeps_maintenance:
            reasons.append("put the node %s into maintenance" % name)
    restarted = run_command_that_changes_the_node(["systemctl", "restart", UNIT_NAME])
    if restarted.error or restarted.exit_code != 0:
        return failed("restarting %s for %s: %s" % (UNIT_NAME, and_joined(pending), message_of(restarted)))
    settings["restarted"] = True
    if decision == "through_maintenance" and keeps_maintenance:
        reasons.append("restarted " + what + "; PostgreSQL starts with the new values when present ends the maintenance")
        settings["waiting"], settings["in_maintenance"] = [], True
        return None
    reasons.append("restarted " + what)
    if decision == "through_maintenance":
        ended = run_command_that_changes_the_node(pg_autoctl(["disable", "maintenance", "--pgdata", data_directory]))
        if ended.error or ended.exit_code != 0:
            return failed("restarted PostgreSQL on the replica %s in maintenance, and then ending the maintenance failed: %s" % (name, message_of(ended)))
    if not wait_until_postgresql_answers([binaries + "/pg_isready"], "", port):
        return dict(failed("restarted %s, and a minute later PostgreSQL does not accept connections; read journalctl -u %s" % (what, UNIT_NAME)), try_again = False)
    found, failure = read_pg_settings(binaries, port)
    if failure:
        return failure
    settings["waiting"] = waiting_for_a_restart(found, wanted.keys())
    for setting in sorted(wanted.keys()):
        row = found.get(setting)
        if row != None and not row["pending_restart"] and row["sourcefile"] != SETTINGS_FILE:
            # Another try would find nothing that waits, and not look again.
            return dict(failed("restarted %s, and PostgreSQL takes %s from %s, not from %s; read pg_settings and pg_file_settings for it" %
                               (what, setting, row["sourcefile"] or "no configuration file", SETTINGS_FILE)), try_again = False)
    return None

def put_into_maintenance_for_a_restart(inputs, pending):
    """Puts a replica into maintenance once the formation has settled with
    it healthy, and waits until the monitor reports it in maintenance: the
    monitor refuses maintenance on a replica while the primary waits for a
    replica (wait_primary), such as one that has just ended its maintenance.
    Returns the failed result or None."""
    name, data_directory, formation = inputs["name"], inputs["data_directory"], inputs["formation"]
    rows, problem, unsettled = wait_until_the_formation_settles(lambda: read_formation(data_directory, ""), [name])
    if problem:
        return failed("reading the node %s from the monitor: %s" % (name, problem))
    if unsettled:
        return not_settled(formation, unsettled)
    command = run_command_that_changes_the_node(pg_autoctl(["enable", "maintenance", "--pgdata", data_directory]))
    if command.error or command.exit_code != 0:
        return failed("putting the node %s into maintenance to restart PostgreSQL for %s: %s" % (name, and_joined(pending), message_of(command)))
    found = ""
    for _ in range(FORMATION_SETTLES_SECONDS // 2):
        rows, problem = read_formation(data_directory, "")
        if problem:
            return failed("reading the node %s from the monitor: %s" % (name, problem))
        row = node_of(rows, name)
        found = row.get("current_group_state", "") if row != None else "not listed"
        if found == "maintenance":
            return None
        run_command(["sleep", "2"])
    return failed("put the node %s into maintenance to restart PostgreSQL for %s, and %d seconds later the monitor reports it %s" %
                  (name, and_joined(pending), FORMATION_SETTLES_SECONDS, found))

def remove_the_settings_file(reasons):
    """Removes the module's file of settings. Returns the failed result or
    None."""
    removed = remove_path(SETTINGS_FILE)
    if removed.error:
        return failed("removing " + SETTINGS_FILE + ": " + removed.error)
    if removed.changed:
        reasons.append(did("removed", "remove", SETTINGS_FILE))
    return None
