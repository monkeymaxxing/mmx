# rabbitmq_plugin manages whether one plugin of RabbitMQ is enabled, through
# RabbitMQ's own program rabbitmq-plugins on the node of the server. It works
# while the server runs and while it is stopped.
# docs/changing-mmx/design/modules-official-extra.md owns the behavior ("RabbitMQ").

MODULE = "rabbitmq_plugin"

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
# This script's own part.
# ---------------------------------------------------------------------------

# What rabbitmq-plugins says about a plugin: named in the file of enabled
# plugins, on only because an enabled plugin needs it, or off.
ENABLED_BY_NAME = "enabled"
ENABLED_BECAUSE_ANOTHER_PLUGIN_NEEDS_IT = "implicit"
NOT_ENABLED = "not_enabled"

def read_plugins():
    """Reads every plugin that RabbitMQ has, with the plugins that each one
    needs. Returns a dictionary and an empty text, or None and what went
    wrong. The dictionary holds "plugins", the rows by plugin name, and
    "server_runs". The command reads the file of enabled plugins, so it also
    answers while the server is stopped."""
    answer = run_command(["rabbitmq-plugins", "-q", "list", "-v", "--formatter", "json"])
    if answer.error or answer.exit_code != 0:
        return None, message_of(answer)
    decoded = json.decode(answer.standard_output, default = None)
    rows = decoded.get("plugins") if type(decoded) == "dict" else None
    if type(rows) != "list" or [row for row in rows if type(row) != "dict"]:
        return None, "rabbitmq-plugins printed no JSON object with a list of plugins: " + answer.standard_output[:200]
    plugins = {}
    for row in rows:
        if row.get("enabled") not in [ENABLED_BY_NAME, ENABLED_BECAUSE_ANOTHER_PLUGIN_NEEDS_IT, NOT_ENABLED]:
            return None, ("rabbitmq-plugins says about the plugin " + str(row.get("name")) + " that it is " + str(row.get("enabled")) +
                          ", which this module does not know")
        plugins[row.get("name")] = row
    return {"plugins": plugins, "server_runs": decoded.get("status") == "running"}, ""

def read_after_the_change(done):
    """Reads the plugins again after a changing command that reported no
    error. Returns the dictionary of read_plugins and None, or None and the
    failed result. MMX must not try such a failure again: the next try would
    find the plugin as the task wants it and report an unchanged task, and
    nobody would read that this read failed. done says what the command
    did."""
    after, problem = read_plugins()
    if problem:
        return None, {"status": "failed", "reason": MODULE + ": " + done + ", and then reading the plugins failed: " + problem, "try_again": False}
    return after, None

def enabled_by_name(plugins):
    """Returns the names of the plugins that the file of enabled plugins
    names, sorted."""
    return sorted([name for name, row in plugins.items() if row["enabled"] == ENABLED_BY_NAME])

def plugins_that_need(plugins, name):
    """Returns the names of the plugins that need the plugin name, directly or
    through other plugins."""
    needing = {name: True}
    for _ in plugins:
        # Each round adds the plugins that need one that was found before.
        found_one = False
        for other, row in plugins.items():
            if other in needing:
                continue
            if [dependency for dependency in row.get("dependencies") or [] if dependency in needing]:
                needing[other] = True
                found_one = True
        if not found_one:
            break
    return [other for other in needing if other != name]

def when_it_takes_effect(server_runs):
    """Returns the part of a reason for a change that waits for the server."""
    if server_runs:
        return ""
    return "; the RabbitMQ server on this node does not run, so the change holds from its next start"

def run_node_task(inputs):
    name = inputs["name"]
    state = inputs["state"]
    problem = name_problem(name)
    if problem:
        return invalid("name", problem)
    missing = missing_program([("rabbitmq-plugins", RABBITMQ_PACKAGE)])
    if missing:
        return missing

    before, problem = read_plugins()
    if problem:
        return {"status": "failed", "reason": MODULE + ": reading the plugins: " + problem}
    plugins = before["plugins"]
    fingerprint = json.encode({"name": name, "state": state})

    # A newer RabbitMQ writes any name into its file of enabled plugins, so
    # the module asks for the name itself.
    if name not in plugins:
        return {
            "status": "missing_prerequisite",
            "reason": MODULE + ": RabbitMQ on this node has no plugin " + name + "; the command rabbitmq-plugins list shows the names. " +
                      "A plugin that is not part of RabbitMQ needs its file in RabbitMQ's plugins directory, from an earlier node task",
        }
    how = plugins[name]["enabled"]
    what = "the plugin " + name

    if state == "enabled":
        if how == ENABLED_BY_NAME:
            return {"fingerprint": fingerprint}
        if how == NOT_ENABLED:
            command = ["rabbitmq-plugins", "-q", "enable", name]
            reason = did("enabled", "enable", what)
        else:
            # "enable" changes nothing for a plugin that is on because another
            # needs it. "set" writes the whole list of the enabled plugins,
            # and turns off every plugin that the list does not name or need:
            # so the list is the one just read, and this plugin.
            command = ["rabbitmq-plugins", "-q", "set"] + sorted(enabled_by_name(plugins) + [name])
            reason = did("enabled", "enable", what + " by name, which was on only because another plugin needs it")
        failure = change_failed("enabling " + what, run_command_that_changes_the_node(command))
        if failure:
            return failure
        if not is_dryrun():
            after, failure = read_after_the_change("enabled " + what)
            if failure:
                return failure
            row = after["plugins"].get(name)
            if row == None or row["enabled"] != ENABLED_BY_NAME:
                # The next try finds the plugin as it was and runs the
                # command again, so this failure may be tried again.
                return {"status": "failed", "reason": MODULE + ": enabling " + what + ": rabbitmq-plugins reported no error, and the plugin is still not enabled by name"}
        return {"reason": reason + when_it_takes_effect(before["server_runs"]), "fingerprint": fingerprint}

    if how == NOT_ENABLED:
        return {"fingerprint": fingerprint}

    # RabbitMQ also turns off every enabled plugin that needs this one. A
    # dryrun names them from what each plugin needs, a run from what
    # RabbitMQ did.
    needing = plugins_that_need(plugins, name)
    others = [other for other in enabled_by_name(plugins) if other in needing]
    failure = change_failed("disabling " + what, run_command_that_changes_the_node(["rabbitmq-plugins", "-q", "disable", name]))
    if failure:
        return failure
    if not is_dryrun():
        after, failure = read_after_the_change("disabled " + what)
        if failure:
            return failure
        row = after["plugins"].get(name)
        if row != None and row["enabled"] != NOT_ENABLED:
            # The next try finds the plugin as it was and runs the command
            # again, so this failure may be tried again.
            return {"status": "failed", "reason": MODULE + ": disabling " + what + ": rabbitmq-plugins reported no error, and the plugin is still enabled"}
        still_enabled = enabled_by_name(after["plugins"])
        others = [other for other in enabled_by_name(plugins) if other != name and other not in still_enabled]
    reason = did("disabled", "disable", what)
    if others:
        reason += "; RabbitMQ " + ("would also disable" if is_dryrun() else "also disabled") + " the plugins that need it: " + ", ".join(others)
    return {"reason": reason + when_it_takes_effect(before["server_runs"]), "fingerprint": fingerprint}
