# rabbitmq_user manages one user of a RabbitMQ server: its password, its tags,
# and its permissions on virtual hosts, through RabbitMQ's own program
# rabbitmqctl on the node of the server.
# docs/changing-mmx/design/modules-official-extra.md owns the behavior ("RabbitMQ").

MODULE = "rabbitmq_user"

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

PERMISSION_KEYS = ["virtual_host", "configure", "write", "read"]

# What remove and keep do, for the message about a list without its setting.
NOT_LISTED = {
    "tags": "remove takes away every tag of the user that tags does not list, and keep leaves the user's other tags alone",
    "permissions": "remove clears the user's permissions on every virtual host that permissions does not list, and keep leaves them alone",
}

def check_inputs(inputs):
    """Returns the result for the first rule that the inputs break, or None.
    No reason repeats the password."""
    problem = name_problem(inputs["name"])
    if problem:
        return invalid("name", problem)
    if inputs.get("state", "present") == "absent":
        return other_input_of_a_removal(inputs, "absent", ["name"])

    # RabbitMQ takes the first line of what it reads and cuts white space off
    # both ends, without a word. It would then store another password than
    # the task gives, and every later run would find the password changed.
    if "password" in inputs:
        password = inputs["password"]
        if password == "":
            return invalid("password", "must not be empty")
        if "\n" in password or "\r" in password:
            return invalid("password", "must not hold a line break: RabbitMQ would take only the first line as the password")
        if password != password.strip():
            return invalid("password", "must not start or end with white space: RabbitMQ would cut it off and store another password")

    for things in ["tags", "permissions"]:
        not_listed = things + "_not_listed"
        if things in inputs and not_listed not in inputs:
            return invalid(not_listed, "is required with " + things + ": " + NOT_LISTED[things])
        if not_listed in inputs and things not in inputs:
            return invalid(things, "is required with " + not_listed)

    for tag in inputs.get("tags", []):
        problem = name_problem(tag)
        if problem:
            return invalid("tags", "a tag " + problem)

    permissions = inputs.get("permissions", [])
    if type(permissions) != "list":
        return invalid("permissions", "must be a list; each entry names a virtual_host and the three regular expressions configure, write, and read")
    virtual_hosts = []
    for number, permission in enumerate(permissions, 1):
        entry = "entry " + str(number)
        if type(permission) != "dict":
            return invalid("permissions", entry + " must be a mapping with virtual_host, configure, write, and read")
        for key in PERMISSION_KEYS:
            if type(permission.get(key)) != "string":
                return invalid("permissions", entry + " needs " + key + " as a text; virtual_host, configure, write, and read are all required")
        for key in permission:
            if key not in PERMISSION_KEYS:
                return invalid("permissions", entry + " has the unknown key " + key + "; the keys are virtual_host, configure, write, and read")
        problem = name_problem(permission["virtual_host"])
        if problem:
            return invalid("permissions", entry + ": virtual_host " + problem)
        if permission["virtual_host"] in virtual_hosts:
            return invalid("permissions", "the virtual host " + permission["virtual_host"] + " is listed twice")
        virtual_hosts.append(permission["virtual_host"])
    return None

def password_is_the_one_of_the_user(name, password):
    """Asks the server whether password is the user's. Returns True or False
    and None, or None and the failed result. rabbitmqctl reads the password
    from its standard input, and only in this plain form of the command:
    with any further argument, such as -q, rabbitmqctl 4 gives its Erlang
    runtime no standard input and never answers."""
    asked = run_command(rabbitmqctl(["authenticate_user", name]), standard_input = password + "\n")
    if not asked.error and asked.exit_code == 0:
        return True, None
    if not asked.error and asked.exit_code == 65 and "failed to authenticate user" in asked.standard_error:
        return False, None
    return None, {
        "status": "failed",
        "reason": MODULE + ": asking the server whether the password is the one of the user " + name + ": " + message_of(asked),
    }

def with_created(result, created):
    """Returns the result with the output created: this task created the
    user, or in a dryrun a run would."""
    result["outputs"] = {"created": created}
    return result

def failed_after_other_steps(failure, name, created):
    """Returns the failed result of a step that follows the first one, with
    the output created. When this try created the user, MMX must not try the
    task again: another try would find the user and report created: false
    for what this task created. The reason then says first that the user
    was created. On a user that existed before, the failure is tried again:
    every step is found again from what the server holds, so the next try
    repeats exactly the step that failed."""
    if created and failure["status"] == "failed":
        failure["try_again"] = False
        failure["reason"] = MODULE + ": created the user " + name + ", and then this failed: " + failure["reason"][len(MODULE) + 2:]
    return with_created(failure, created)

def named_tags(tags):
    """Returns "the tag a" or "the tags a, b"."""
    return ("the tag " if len(tags) == 1 else "the tags ") + ", ".join(tags)

def run_node_task(inputs):
    name = inputs["name"]
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

    # Read everything first: the user, its permissions, the virtual hosts, and
    # whether the password is the user's.
    users, failure = read_list("the users", ["list_users"])
    if failure:
        return failure
    user = None
    for row in users:
        if row.get("user") == name:
            user = row
    fingerprint = json.encode({key: inputs[key] for key in inputs if key != "password"})

    if state == "absent":
        if user == None:
            return with_created({"fingerprint": fingerprint}, False)
        failure = change_failed("removing the user " + name, run_command_that_changes_the_node(rabbitmqctl(["-q", "delete_user", name])))
        if failure:
            return failure
        return with_created({"reason": did("removed", "remove", "the user " + name), "fingerprint": fingerprint}, False)

    # authenticate_user answers the same for a wrong password and for a user
    # that does not exist, so the list of users says which of the two it is.
    # A task without password cannot create the user. An earlier node task
    # of the same playbook may create it, so this is a missing prerequisite:
    # a dryrun is not checked, and a run fails once.
    if user == None and "password" not in inputs:
        return {
            "status": "missing_prerequisite",
            "reason": MODULE + ": the user " + name + " does not exist, and the task has no password to create it; " +
                      "add password, or create the user with a rabbitmq_user task in an earlier node task",
        }

    permissions_now = {}
    if user != None and "permissions" in inputs:
        rows, failure = read_list("the permissions of the user " + name, ["list_user_permissions", name])
        if failure:
            return failure
        for row in rows:
            permissions_now[row.get("vhost")] = (row.get("configure"), row.get("write"), row.get("read"))
    permissions_to_set = []
    for permission in inputs.get("permissions", []):
        wanted = (permission["configure"], permission["write"], permission["read"])
        if permissions_now.get(permission["virtual_host"]) != wanted:
            permissions_to_set.append(permission)
    permissions_to_clear = []
    if inputs.get("permissions_not_listed") == "remove":
        listed_virtual_hosts = [permission["virtual_host"] for permission in inputs["permissions"]]
        permissions_to_clear = [virtual_host for virtual_host in permissions_now if virtual_host not in listed_virtual_hosts]

    # A virtual host on which the user has a permission exists. Only a
    # virtual host that is new to the user is looked up.
    new_virtual_hosts = [permission["virtual_host"] for permission in permissions_to_set if permission["virtual_host"] not in permissions_now]
    if new_virtual_hosts:
        rows, failure = read_list("the virtual hosts", ["list_vhosts"])
        if failure:
            return failure
        existing = [row.get("name") for row in rows]
        for virtual_host in new_virtual_hosts:
            if virtual_host not in existing:
                return the_virtual_host_is_missing(virtual_host)

    set_the_password = False
    if user != None and "password" in inputs:
        same, failure = password_is_the_one_of_the_user(name, inputs["password"])
        if failure:
            return failure
        set_the_password = not same

    tags_now = user.get("tags", []) if user != None else []
    tags_to_add = []
    tags_to_remove = []
    if "tags" in inputs:
        for tag in inputs["tags"]:
            if tag not in tags_now and tag not in tags_to_add:
                tags_to_add.append(tag)
        if inputs["tags_not_listed"] == "remove":
            tags_to_remove = [tag for tag in tags_now if tag not in inputs["tags"]]

    # Change only what differs. The password reaches rabbitmqctl through its
    # standard input, in the plain form of the command.
    reasons = []
    created = user == None
    if user == None:
        failure = change_failed(
            "creating the user " + name,
            run_command_that_changes_the_node(rabbitmqctl(["add_user", name]), standard_input = inputs["password"] + "\n"),
        )
        if failure:
            return failure
        reasons.append(did("created", "create", "the user " + name))
    elif set_the_password:
        failure = change_failed(
            "setting the password of the user " + name,
            run_command_that_changes_the_node(rabbitmqctl(["change_password", name]), standard_input = inputs["password"] + "\n"),
        )
        if failure:
            return failure
        reasons.append(did("set", "set", "the password of the user " + name))

    if tags_to_add or tags_to_remove:
        # set_user_tags takes the user's whole list of tags.
        tags = [tag for tag in tags_now if tag not in tags_to_remove] + tags_to_add
        failure = change_failed(
            "setting the tags of the user " + name,
            run_command_that_changes_the_node(rabbitmqctl(["-q", "set_user_tags", name] + tags)),
        )
        if failure:
            return failed_after_other_steps(failure, name, created)
        if tags_to_add:
            reasons.append(did("added", "add", named_tags(tags_to_add) + " to the user " + name))
        if tags_to_remove:
            reasons.append(did("removed", "remove", named_tags(tags_to_remove) + " from the user " + name))

    for permission in permissions_to_set:
        what = "the permissions of the user " + name + " on the virtual host " + permission["virtual_host"]

        # "--" ends the options: a regular expression may start with a hyphen.
        arguments = ["-q", "set_permissions", "-p", permission["virtual_host"], "--", name, permission["configure"], permission["write"], permission["read"]]
        answer = run_command_that_changes_the_node(rabbitmqctl(arguments))
        if not answer.error and answer.exit_code != 0 and "invalid_regexp" in answer.standard_error:
            # RabbitMQ cannot read one of the three expressions. Another try
            # would get the same answer, so the task is tried once.
            return {"status": "invalid_inputs", "reason": MODULE + ": RabbitMQ refuses a regular expression of " + what + ": " + message_of(answer)}
        failure = change_failed("setting " + what, answer)
        if failure:
            return failed_after_other_steps(failure, name, created)
        reasons.append(did("set", "set", what))
    for virtual_host in permissions_to_clear:
        what = "the permissions of the user " + name + " on the virtual host " + virtual_host
        failure = change_failed(
            "clearing " + what,
            run_command_that_changes_the_node(rabbitmqctl(["-q", "clear_permissions", "-p", virtual_host, name])),
        )
        if failure:
            return failed_after_other_steps(failure, name, created)
        reasons.append(did("cleared", "clear", what))

    result = with_created({"fingerprint": fingerprint}, created)
    if reasons:
        result["reason"] = "; ".join(reasons)
    return result
