# rabbitmq_policy manages one policy of a virtual host of a RabbitMQ server,
# through RabbitMQ's own program rabbitmqctl on the node of the server.
# docs/changing-mmx/design/modules-official-extra.md owns the behavior ("RabbitMQ").

MODULE = "rabbitmq_policy"

# ---------------------------------------------------------------------------
# The same in all four rabbitmq_* scripts. A script cannot load another file,
# so each script holds its own copy; keep the copies identical.
# ---------------------------------------------------------------------------

RABBITMQ_PACKAGE = "rabbitmq-server"

def missing_program(programs):
    """Returns the result for the first program that is not on the node, or
    None when all are there. programs is a list of (program, package)."""
    for program, package in programs:
        location = find_program(program)
        if location.error:
            return {"status": "failed", "reason": MODULE + ": looking for the program " + program + ": " + location.error}
        if not location.found:
            return {
                "status": "missing_prerequisite",
                "reason": MODULE + ": the program " + program + " is not installed; install the package " + package + " in an earlier node task",
            }
    return None

def message_of(command):
    """Returns the tool's own message of a command that failed: the first
    paragraph of its standard error, without the line "Error:" that only
    announces a message. RabbitMQ prints "Validation failed" as a paragraph
    of its own before the reason, so that reason is added."""
    if command.error:
        return command.error
    paragraphs = []
    lines = []
    for line in command.standard_error.splitlines() + [""]:
        line = line.strip()
        if line == "Error:":
            continue
        if line:
            lines.append(line)
        elif lines:
            paragraphs.append(" ".join(lines))
            lines = []
    if not paragraphs:
        return "the command ended with exit code " + str(command.exit_code) + " and printed no message"
    message = paragraphs[0]
    if message == "Validation failed" and len(paragraphs) > 1:
        message += ": " + paragraphs[1]
    if len(message) > 500:
        message = message[:500] + " ..."
    return message

def name_problem(name):
    """Returns the rule that a name breaks, or an empty text. A name reaches
    RabbitMQ's programs as one argument, and they read an argument that
    starts with a hyphen as an option."""
    if name == "":
        return "must not be empty"
    if name.startswith("-"):
        return "must not start with a hyphen (-), which RabbitMQ's programs read as an option"
    return ""

def invalid(field, rule):
    """Returns the result for an input that breaks a rule."""
    return {"status": "invalid_inputs", "reason": MODULE + "." + field + ": " + rule}

def other_input_of_a_removal(inputs, state, accepted):
    """Returns the result for a task that removes its thing and also gives an
    input that only describes the thing, or None. accepted is the list of
    the inputs that the state takes."""
    for name in inputs:
        if name != "state" and name not in accepted:
            return invalid("state", state + " accepts only " + " and ".join(accepted) + "; remove " + name)
    return None

def did(past, infinitive, thing):
    """Returns one part of a reason: what the run did, or in a dryrun what a
    run would do."""
    return ("would " + infinitive if is_dryrun() else past) + " " + thing

def change_failed(what, command):
    """Returns the failed result when a changing command did not work, or
    None. In a dryrun the command did not run and nothing failed."""
    if command.error or command.exit_code != 0:
        return {"status": "failed", "reason": MODULE + ": " + what + ": " + message_of(command)}
    return None

# ---------------------------------------------------------------------------
# The same in rabbitmq_virtual_host, rabbitmq_user, and rabbitmq_policy, which
# manage something inside the server; keep the copies identical.
# ---------------------------------------------------------------------------

# The real rabbitmqctl program. The command "rabbitmqctl" of Debian and Ubuntu
# hands its arguments to a shell, which changes them and can run a command
# that an argument holds. So the scripts run the real program as the account
# rabbitmq themselves, as RabbitMQ's own wrapper does.
RABBITMQCTL_PROGRAM = "/usr/lib/rabbitmq/bin/rabbitmqctl"

PROGRAMS = [
    ("runuser", "util-linux"),
    (RABBITMQCTL_PROGRAM, RABBITMQ_PACKAGE),
    ("rabbitmq-diagnostics", RABBITMQ_PACKAGE),
]

def rabbitmqctl(arguments):
    """Returns the command line that runs the real rabbitmqctl as the account
    rabbitmq, without a shell."""
    return ["runuser", "-u", "rabbitmq", "--", RABBITMQCTL_PROGRAM] + arguments

def server_does_not_answer():
    """Returns None when the RabbitMQ server runs. Otherwise returns the
    task's result: a dryrun is not checked, because an earlier node task may
    start the server, and a run fails and is tried again. Exit code 69 of
    another command also means other things, and a server that still starts
    answers a ping; check_running is the one question with one meaning."""
    asked = run_command(["rabbitmq-diagnostics", "-q", "check_running"])
    if not asked.error and asked.exit_code == 0:
        return None
    return {
        "status": "not_checked" if is_dryrun() else "failed",
        "reason": MODULE + ": the RabbitMQ server on this node does not answer; start its service in an earlier node task " +
                  "(rabbitmq-diagnostics check_running said: " + message_of(asked) + ")",
    }

def read_list(what, arguments):
    """Runs a read command of rabbitmqctl that prints a JSON list of objects.
    Returns the list and None, or None and the failed result."""
    answer = run_command(rabbitmqctl(["-q"] + arguments + ["--formatter", "json"]))
    if answer.error or answer.exit_code != 0:
        return None, {"status": "failed", "reason": MODULE + ": reading " + what + ": " + message_of(answer)}
    rows = json.decode(answer.standard_output, default = None)
    if type(rows) != "list" or [row for row in rows if type(row) != "dict"]:
        return None, {
            "status": "failed",
            "reason": MODULE + ": reading " + what + ": rabbitmqctl printed no JSON list of objects: " + answer.standard_output[:200],
        }
    return rows, None

def the_virtual_host_is_missing(virtual_host):
    """Returns the result for a virtual host that an earlier node task may
    still create."""
    return {
        "status": "missing_prerequisite",
        "reason": MODULE + ": the virtual host " + virtual_host + " does not exist; create it with a rabbitmq_virtual_host task in an earlier node task",
    }

# ---------------------------------------------------------------------------
# This script's own part.
# ---------------------------------------------------------------------------

def check_inputs(inputs):
    """Returns the result for the first rule that the inputs break, or None."""
    for field in ["name", "virtual_host"]:
        problem = name_problem(inputs[field])
        if problem:
            return invalid(field, problem)
    if inputs.get("state", "present") == "absent":
        return other_input_of_a_removal(inputs, "absent", ["name", "virtual_host"])
    for field in ["pattern", "apply_to", "definition", "priority"]:
        if field not in inputs:
            return invalid(field, "is required for state: present")
    definition = inputs["definition"]
    if type(definition) != "dict":
        return invalid("definition", "must be a mapping of RabbitMQ's policy keys to their values, such as max-length: 100000")
    if not definition:
        return invalid("definition", "must hold at least one policy key, such as max-length: 100000; RabbitMQ accepts no policy without one")
    return None

def differences(policy, inputs, priority):
    """Returns the inputs whose value differs from the policy that RabbitMQ
    has. RabbitMQ gives the definition as a text that holds JSON, with the
    keys in its own order; the values are compared one by one."""
    differing = []
    if policy.get("pattern") != inputs["pattern"]:
        differing.append("pattern")
    if policy.get("apply-to") != inputs["apply_to"]:
        differing.append("apply_to")
    stored = policy.get("definition")
    if type(stored) == "string":
        stored = json.decode(stored, default = None)
    if stored != inputs["definition"]:
        differing.append("definition")
    if policy.get("priority") != priority:
        differing.append("priority")
    return differing

def run_node_task(inputs):
    name = inputs["name"]
    virtual_host = inputs["virtual_host"]
    state = inputs.get("state", "present")
    rejected = check_inputs(inputs)
    if rejected:
        return rejected

    missing = missing_program(PROGRAMS)
    if missing:
        return missing
    silent = server_does_not_answer()
    if silent:
        return silent

    fingerprint = json.encode(inputs)
    what = "the policy " + name + " of the virtual host " + virtual_host

    # list_policies fails for a virtual host that does not exist, so the list
    # of virtual hosts is read first.
    virtual_hosts, failure = read_list("the virtual hosts", ["list_vhosts"])
    if failure:
        return failure
    # The output created: this task created the policy, or in a dryrun a run
    # would.
    unchanged = {"outputs": {"created": False}, "fingerprint": fingerprint}
    if virtual_host not in [row.get("name") for row in virtual_hosts]:
        if state == "absent":
            return unchanged
        return the_virtual_host_is_missing(virtual_host)
    policies, failure = read_list("the policies of the virtual host " + virtual_host, ["list_policies", "-p", virtual_host])
    if failure:
        return failure
    policy = None
    for row in policies:
        if row.get("name") == name:
            policy = row

    if state == "absent":
        if policy == None:
            return unchanged
        failure = change_failed(
            "removing " + what,
            run_command_that_changes_the_node(rabbitmqctl(["-q", "clear_policy", "-p", virtual_host, name])),
        )
        if failure:
            return failure
        return {"reason": did("removed", "remove", what), "outputs": {"created": False}, "fingerprint": fingerprint}

    priority = inputs["priority"]
    differing = differences(policy, inputs, priority) if policy != None else []
    if policy != None and not differing:
        return unchanged

    # set_policy replaces a policy as a whole. "--" ends the options: a
    # pattern may start with a hyphen. A whole number reaches the script as a
    # whole number, so the JSON holds it without a fraction, which RabbitMQ
    # would refuse.
    arguments = ["-q", "set_policy", "-p", virtual_host, "--priority", str(priority), "--apply-to", inputs["apply_to"],
                 "--", name, inputs["pattern"], json.encode(inputs["definition"])]
    answer = run_command_that_changes_the_node(rabbitmqctl(arguments))
    if not answer.error and answer.exit_code != 0 and message_of(answer).startswith("Validation failed"):
        # RabbitMQ checks a policy when it is set: the value of apply_to, the
        # keys and values of the definition, and the pattern. What it takes
        # depends on its version. Another try would get the same answer, so
        # the task is tried once.
        return {"status": "invalid_inputs", "reason": MODULE + ": RabbitMQ refuses " + what + ": " + message_of(answer)}
    failure = change_failed("setting " + what, answer)
    if failure:
        return failure
    if policy == None:
        return {"reason": did("created", "create", what), "outputs": {"created": True}, "fingerprint": fingerprint}
    return {"reason": did("changed", "change", what + ": " + ", ".join(differing)), "outputs": {"created": False}, "fingerprint": fingerprint}
