# incus_instance manages one Incus container or virtual machine through the
# program incus: its profiles, its configuration, its devices, and whether it
# runs. docs/changing-mmx/design/modules-official-extra.md ("Incus") owns the behavior.
#
# Three rules shape every command of this script:
#
# - Each name has Incus's prefix "local:" and each command names its project,
#   so the client settings of the node's root account cannot send a command to
#   another server or to another project.
# - Each read is "incus query --raw". It prints JSON and says in error_code
#   whether the thing exists. Incus's error texts change between its
#   versions, so nothing is decided from one.
# - "incus config edit" takes a whole document and removes what the document
#   leaves out. So the script reads the instance, changes three parts of that
#   document, and writes all of it back through standard input, where a
#   configuration value is on no command line.

MODULE = "incus_instance"

INCUS_IS_NOT_INSTALLED = ("the program incus is not installed; install the package incus-base (Debian) or incus " +
                          "(Ubuntu and the Red Hat family) in an earlier node task")

# The two types as the task writes them and as Incus's API names them, and
# the status codes of an instance in Incus's API.
INCUS_NAME_OF_TYPE = {"container": "container", "virtual_machine": "virtual-machine"}
WORDS_FOR_TYPE = {"container": "container", "virtual-machine": "virtual machine"}
STATUS_STOPPED, STATUS_RUNNING, STATUS_FROZEN = 102, 103, 110

# The inputs that say how the task gets to its state, not what the state is.
INPUTS_ABOUT_THE_WAY = ["wait_for_shutdown_at_most", "power_off_when_shutdown_takes_longer"]

# The characters of a name that an API path holds as they are.
PLAIN_CHARACTERS = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
HEXADECIMAL_DIGITS = "0123456789ABCDEF"

def run_node_task(inputs):
    problem = problem_with_the_inputs(inputs)
    if problem:
        return {"status": "invalid_inputs", "reason": MODULE + "." + problem}
    without_the_program = result_without_the_program()
    if without_the_program:
        return without_the_program

    name = inputs["name"]
    project = inputs.get("incus_project", "default")
    if "incus_project" in inputs:
        read = read_from_incus("/1.0/projects/" + percent_encoded(project))
        if read["outcome"] == "missing":
            return {"status": "missing_prerequisite", "reason": MODULE + ": " + project_is_missing(project)}
        if read["outcome"] != "found":
            return reading_failed(read, "the Incus project " + project)
    read = read_instance(name, project)
    if read["outcome"] not in ("found", "missing"):
        return reading_failed(read, "the instance " + name)
    instance = read["value"]

    if inputs["state"] == "deleted_with_all_its_data":
        return delete_instance(inputs, project, instance)
    if instance == None:
        return create_instance(inputs, project)
    return bring_instance_to_the_task(inputs, project, instance)

def problem_with_the_inputs(inputs):
    """The first rule that the inputs break and that the description cannot
    state, as "<input>: <the rule>", or None."""
    for text_input in ("name", "image_when_created", "incus_project"):
        if inputs.get(text_input) == "":
            return text_input + ": must not be empty"
    for option_like in ("image_when_created", "incus_project"):
        if inputs.get(option_like, "").startswith("-"):
            return option_like + ": must not start with a hyphen, which the program incus would read as one of its options"
    if inputs["state"] == "deleted_with_all_its_data":
        for other in sorted(inputs.keys()):
            if other not in ("name", "state", "incus_project"):
                return "state: deleted_with_all_its_data accepts only name and incus_project; remove " + other
        return None
    for required in ("instance_type", "profiles"):
        if required not in inputs:
            return required + ": is required for state: running or stopped"
    wait = parse_duration(inputs.get("wait_for_shutdown_at_most", "2m"))
    if wait.error or wait.seconds < 1 or int(wait.seconds) != wait.seconds:
        return "wait_for_shutdown_at_most: must be a positive duration of whole seconds, such as 30s or 2m; incus stop takes whole seconds"
    return problem_with_config_and_devices(inputs)

def problem_with_config_and_devices(inputs):
    """The rules of config and devices: each comes with its *_not_listed, a
    device is a mapping of options, and every value is text that is not
    empty."""
    for listed, not_listed, one in (("config", "config_keys_not_listed", "key"), ("devices", "devices_not_listed", "device")):
        if listed in inputs and not_listed not in inputs:
            return "%s: is required with %s: remove removes every %s that %s does not list, and keep leaves those %ss" % (not_listed, listed, one, listed, one)
        if not_listed in inputs and listed not in inputs:
            return "%s: is required with %s" % (listed, not_listed)

    # Incus stores no key and no device option with an empty value, so a
    # task with one would report a change on every run.
    for key in sorted(inputs.get("config", {}).keys()):
        if inputs["config"][key] == "":
            return ("config: the key %s has an empty value, and Incus stores no key without a value; " % key +
                    "leave the key out, and write config_keys_not_listed: remove to remove a key that the task does not list")
    if "devices" not in inputs:
        return None
    if type(inputs["devices"]) != "dict":
        return "devices: expected a mapping of device names, each with a mapping of Incus device options"
    for device in sorted(inputs["devices"].keys()):
        options = inputs["devices"][device]
        if type(options) != "dict":
            return "devices: the device %s must be a mapping of Incus device options, such as type: disk" % device
        for option in sorted(options.keys()):
            if type(options[option]) != "string":
                return "devices: the option %s of the device %s must be text; write a number or true and false in quotes, such as \"true\"" % (option, device)
            if options[option] == "":
                return "devices: the option %s of the device %s has an empty value, and Incus stores no option without a value; leave the option out" % (option, device)
    return None

def project_is_missing(project):
    return ("the Incus project %s does not exist; create it in an earlier node task, " % project +
            "such as a shell task that runs incus project create")

def result_without_the_program():
    """The result of a task on a node without the program incus, or None."""
    located = find_program("incus")
    if located.error:
        return {"status": "failed", "reason": MODULE + ": looking for the program incus: " + located.error}
    if not located.found:
        return {"status": "missing_prerequisite", "reason": MODULE + ": " + INCUS_IS_NOT_INSTALLED}
    return None

# Reading.

def percent_encoded(text):
    """A name as one part of an API path or of its query: every byte that is
    not an ASCII letter, a digit, a hyphen, or an underscore is written as %
    and two hexadecimal digits, so a name cannot change the path that the
    server reads."""
    parts = []
    for byte in text.elem_ords():
        if byte < 128 and chr(byte) in PLAIN_CHARACTERS:
            parts.append(chr(byte))
        else:
            parts.append("%" + HEXADECIMAL_DIGITS[byte // 16] + HEXADECIMAL_DIGITS[byte % 16])
    return "".join(parts)

def message_of(result):
    """The program's own words about a command that failed, on one line: its
    standard error from the line that starts with "Error:" on. Before that
    line, the first incus command of an account prints hints for a new
    user. From Incus 6.23 on, a failed command also prints its usage on
    standard output, which says nothing about the failure."""
    if result.error:
        return result.error
    lines = [line.strip() for line in result.standard_error.splitlines() if line.strip()]
    for index, line in enumerate(lines):
        if line.startswith("Error:"):
            lines = lines[index:]
            break
    if lines:
        return " ".join(lines)
    return "the command ended with the exit code %d and printed no message" % result.exit_code

def read_from_incus(path):
    """Ask the Incus server of this node for one thing by its API path.
    Returns a dictionary whose outcome is "found" (value holds the thing),
    "missing", "no_answer" (the server did not answer), or "refused" (the
    server answered with an error); message holds Incus's own words."""
    answered = run_command(["incus", "query", "--raw", "local:" + path])
    if answered.error or answered.exit_code != 0:
        return {"outcome": "no_answer", "value": None, "message": message_of(answered)}
    answer = json.decode(answered.standard_output, default = None)
    if type(answer) != "dict" or type(answer.get("error_code")) != "int":
        return {"outcome": "refused", "value": None,
                "message": "incus query printed no answer in JSON: " + answered.standard_output[:200].strip()}
    if answer["error_code"] == 404:
        return {"outcome": "missing", "value": None, "message": str(answer.get("error"))}
    if answer["error_code"] != 0 or type(answer.get("metadata")) != "dict":
        return {"outcome": "refused", "value": None,
                "message": "%s (error code %d)" % (answer.get("error"), answer["error_code"])}
    return {"outcome": "found", "value": answer["metadata"], "message": ""}

def reading_failed(read, what):
    """The result of a task after a read that did not say whether the thing
    exists. While the server does not answer, a dryrun is not checked: an
    earlier node task may start the server."""
    if read["outcome"] == "no_answer":
        return {"status": "not_checked" if is_dryrun() else "failed",
                "reason": MODULE + ": the Incus server on this node does not answer: " + read["message"]}
    return {"status": "failed",
            "reason": "%s: reading %s from the Incus server on this node: %s" % (MODULE, what, read["message"])}

def read_instance(name, project):
    return read_from_incus("/1.0/instances/" + percent_encoded(name) + "?project=" + percent_encoded(project))

# Changing.

def change(arguments, standard_input = ""):
    return run_command_that_changes_the_node(arguments, standard_input = standard_input)

def failed(result):
    """Whether a changing command failed. In a dryrun it did not run."""
    return result.error != "" or (result.ran and result.exit_code != 0)

def said(done, do, what):
    """The words of a reason: what a run did, or what a dryrun says that a
    run would do."""
    return "would " + do + " " + what if is_dryrun() else done + " " + what

def document_for_the_task(inputs, present):
    """The whole document that the task asks for: the present one with the
    task's profiles, description, configuration, and devices in it.
    Everything else stays as it is, because the edit commands take a whole
    document. Incus's own volatile.* and image.* keys are never removed."""
    document = dict(present)
    for whole in ("profiles", "description"):
        if whole in inputs:
            document[whole] = inputs[whole]
    if "config" in inputs:
        config = {}
        for key, value in (present.get("config") or {}).items():
            if inputs["config_keys_not_listed"] == "keep" or key.startswith("volatile.") or key.startswith("image."):
                config[key] = value
        config.update(inputs["config"])
        document["config"] = config
    if "devices" in inputs:
        devices = {}
        if inputs["devices_not_listed"] == "keep":
            devices.update(present.get("devices") or {})
        devices.update(inputs["devices"])
        document["devices"] = devices
    return document

def differences_between(present, wanted):
    """What differs between a thing as Incus has it and the document that
    the task asks for, as the words of a reason. They name keys and devices
    and never a value: a configuration value can be private."""
    steps = []
    profiles = wanted.get("profiles") or []
    if profiles != (present.get("profiles") or []):
        if profiles:
            steps.append(said("set", "set", "the profiles to " + ", ".join(profiles)))
        else:
            steps.append(said("removed", "remove", "every profile"))
    if (wanted.get("description") or "") != (present.get("description") or ""):
        steps.append(said("set", "set", "the description"))
    steps += differences_of_a_mapping(present.get("config") or {}, wanted.get("config") or {}, "the configuration key", "the configuration keys")
    steps += differences_of_a_mapping(present.get("devices") or {}, wanted.get("devices") or {}, "the device", "the devices")
    return steps

def differences_of_a_mapping(present, wanted, one, several):
    steps = []
    to_set = sorted([key for key in wanted if key not in present or present[key] != wanted[key]])
    to_remove = sorted([key for key in present if key not in wanted])
    if to_set:
        steps.append(said("set", "set", (one if len(to_set) == 1 else several) + " " + ", ".join(to_set)))
    if to_remove:
        steps.append(said("removed", "remove", (one if len(to_remove) == 1 else several) + " " + ", ".join(to_remove)))
    return steps

def after_a_failed_command(tried, result, name, project):
    """What a failed changing command left: the reason, with the status that
    the instance has now, and the instance as Incus has it now, or None. A
    change that Incus refuses can still leave the instance in another state
    than before."""
    reason = "%s: %s: %s" % (MODULE, tried, message_of(result))
    read = read_instance(name, project)
    if read["outcome"] != "found":
        return reason, None
    return reason + "; the status of the instance is now " + str(read["value"].get("status")), read["value"]

def succeeded(inputs, steps, created):
    wanted = {key: value for key, value in inputs.items() if key not in INPUTS_ABOUT_THE_WAY}
    result = {"outputs": {"created": created}, "fingerprint": json.encode(wanted)}
    if steps:
        result["reason"] = "; ".join(steps)
    return result

# The three things a task can ask for.

def delete_instance(inputs, project, instance):
    name = inputs["name"]
    if instance == None:
        return succeeded(inputs, [], False)

    # --force stops a running instance at once; the state's name says that
    # the data goes.
    deleted = change(["incus", "delete", "--force", "--project", project, "local:" + name])
    if failed(deleted):
        reason, _ = after_a_failed_command("deleting the instance " + name, deleted, name, project)
        return {"status": "failed", "reason": reason}
    return succeeded(inputs, [said("deleted", "delete", "the instance %s with all its data" % name)], False)

def type_of_a_local_image(image, project):
    """What the Incus server of this node says about one of its own images,
    by its alias or by its fingerprint. Returns the image's type and None;
    or None and the result of a task that cannot go on."""
    in_project = "?project=" + percent_encoded(project)
    for path in ("/1.0/images/aliases/", "/1.0/images/"):
        read = read_from_incus(path + percent_encoded(image) + in_project)
        if read["outcome"] == "found":
            return read["value"].get("type"), None
        if read["outcome"] != "missing":
            return None, reading_failed(read, "the image " + image)

    # An earlier node task may import the image.
    return None, {"status": "missing_prerequisite",
                  "reason": "%s: the Incus server on this node has no image %s; import it in an earlier node task, or name an image of another server, such as images:debian/13" %
                            (MODULE, image)}

def create_instance(inputs, project):
    name = inputs["name"]
    if "image_when_created" not in inputs:
        return {"status": "invalid_inputs", "reason": "%s.image_when_created: is required to create the instance %s" % (MODULE, name)}
    image = inputs["image_when_created"]
    wanted_type = INCUS_NAME_OF_TYPE[inputs["instance_type"]]

    # An image of this server says its type before the instance is created.
    # Without --vm, Incus creates an instance of the image's own type.
    if ":" not in image or image.startswith("local:"):
        image = image.removeprefix("local:")
        image_type, stopped = type_of_a_local_image(image, project)
        if stopped:
            return stopped
        if image_type in WORDS_FOR_TYPE and image_type != wanted_type:
            return {"status": "invalid_inputs",
                    "reason": "%s.instance_type: the task asks for a %s, and the image %s is the image of a %s; Incus creates an instance of the type of its image" %
                              (MODULE, WORDS_FOR_TYPE[wanted_type], image, WORDS_FOR_TYPE[image_type])}
        image = "local:" + image

    arguments = ["incus", "init", "-q", "--project", project, image, "local:" + name]
    if wanted_type == "virtual-machine":
        arguments.append("--vm")
    document = {}
    if inputs["profiles"]:
        document["profiles"] = inputs["profiles"]
    else:
        # An empty list in the document gives the profile default; only
        # this option creates an instance without any profile.
        arguments.append("--no-profiles")
    for part in ("config", "devices"):
        if part in inputs:
            document[part] = inputs[part]
    created = change(arguments, json.encode(document))
    if failed(created):
        return {"status": "failed", "reason": "%s: creating the instance %s from the image %s: %s" % (MODULE, name, inputs["image_when_created"], message_of(created))}
    steps = [said("created", "create", "the %s %s from the image %s" % (WORDS_FOR_TYPE[wanted_type], name, inputs["image_when_created"]))]

    # From here on the instance exists. MMX's next try of a failed task
    # would find it and report that the task created nothing: every failure
    # below asks for no other try and says that the task created it.
    if created.ran:
        # An image of another server says its type only now.
        read = read_instance(name, project)
        if read["outcome"] != "found":
            return {"status": "failed", "try_again": False, "outputs": {"created": True},
                    "reason": "%s: reading the new instance %s from the Incus server on this node: %s" % (MODULE, name, read["message"] or "it is not there")}
        created_type = read["value"].get("type")
        if created_type != wanted_type:
            reason = "%s.instance_type: the task asks for a %s, and the image %s created an instance of the type %s; " % (
                MODULE,
                WORDS_FOR_TYPE[wanted_type],
                inputs["image_when_created"],
                WORDS_FOR_TYPE.get(created_type, str(created_type)),
            )
            removed = change(["incus", "delete", "--project", project, "local:" + name])
            if failed(removed):
                return {"status": "failed", "try_again": False, "outputs": {"created": True},
                        "reason": reason + "deleting the new instance again failed: " + message_of(removed)}
            return {"status": "invalid_inputs", "reason": reason + "the task deleted the new instance again, which never ran"}

    if inputs["state"] == "running":
        started = change(["incus", "start", "--project", project, "local:" + name])
        if failed(started):
            reason, _ = after_a_failed_command("created the instance %s, and starting it failed" % name, started, name, project)
            return {"status": "failed", "try_again": False, "outputs": {"created": True}, "reason": reason}
        steps.append(said("started", "start", "it"))
    return succeeded(inputs, steps, True)

def bring_instance_to_the_task(inputs, project, instance):
    name = inputs["name"]
    state = inputs["state"]
    wanted_type = INCUS_NAME_OF_TYPE[inputs["instance_type"]]
    if instance.get("type") != wanted_type:
        return {"status": "invalid_inputs",
                "reason": "%s.instance_type: the task asks for a %s, and the instance %s is of the type %s; Incus cannot change the type of an instance" %
                          (MODULE, WORDS_FOR_TYPE[wanted_type], name, WORDS_FOR_TYPE.get(instance.get("type"), str(instance.get("type"))))}
    status = instance.get("status_code")
    if status not in (STATUS_STOPPED, STATUS_RUNNING, STATUS_FROZEN):
        return {"status": "failed",
                "reason": "%s: the instance %s has the status %s (code %s), and this module acts only on the statuses Stopped, Running, and Frozen" %
                          (MODULE, name, instance.get("status"), status)}
    if state == "stopped" and status == STATUS_FROZEN:
        return {"status": "invalid_inputs",
                "reason": "%s.state: the instance %s is frozen, and Incus does not shut down a frozen instance; resume it first, with incus resume %s" %
                          (MODULE, name, name)}
    if state == "stopped" and status == STATUS_RUNNING and instance.get("ephemeral") == True:
        return {"status": "invalid_inputs",
                "reason": "%s.state: the instance %s is ephemeral, and Incus deletes an ephemeral instance when it stops; to delete it, write state: deleted_with_all_its_data" %
                          (MODULE, name)}

    steps = []
    if state == "stopped" and status == STATUS_RUNNING:
        step, failure = shut_down(inputs, project)
        if failure:
            return failure
        steps.append(step)

    differences = differences_between(instance, document_for_the_task(inputs, instance))
    if differences:
        failure = write_instance(inputs, project)
        if failure:
            return failure
        steps += differences

    if state == "running" and status != STATUS_RUNNING:
        started = change(["incus", "start", "--project", project, "local:" + name])
        if failed(started):
            reason, now = after_a_failed_command("starting the instance " + name, started, name, project)
            if now != None and now.get("status_code") == STATUS_RUNNING:
                # The instance runs all the same. MMX's next try would
                # find it running and report success.
                return {"status": "failed", "try_again": False, "reason": reason}
            return {"status": "failed", "reason": reason}
        if status == STATUS_FROZEN:
            steps.append(said("resumed", "resume", "the frozen instance " + name))
        else:
            steps.append(said("started", "start", "the instance " + name))
    return succeeded(inputs, steps, False)

def shut_down(inputs, project):
    """Ask the instance to shut down and wait. Returns the words for the
    reason and None, or None and the result of a failed task."""
    name = inputs["name"]
    seconds = int(parse_duration(inputs.get("wait_for_shutdown_at_most", "2m")).seconds)
    instance_name = "local:" + name

    # Without --timeout, incus stop waits without end for an instance that
    # ignores the request.
    asked = change(["incus", "stop", "--project", project, instance_name, "--timeout", str(seconds)])
    if not failed(asked):
        return said("shut down", "shut down", "the instance " + name), None

    # The command failed. Its text is not read: the instance says whether
    # it still runs.
    reason, now = after_a_failed_command("shutting down the instance %s, with a wait of %d seconds" % (name, seconds), asked, name, project)
    if now != None and now.get("status_code") == STATUS_STOPPED:
        return said("shut down", "shut down", "the instance " + name), None

    # The wait is the task's own limit: MMX's next try would wait as long
    # again, so a failure here asks for no other try.
    if not inputs.get("power_off_when_shutdown_takes_longer", False):
        return None, {"status": "failed", "try_again": False,
                      "reason": reason + ". To stop the instance at once when the wait ends, write power_off_when_shutdown_takes_longer: true"}
    forced = change(["incus", "stop", "--force", "--project", project, instance_name])
    if failed(forced):
        reason, now = after_a_failed_command("stopping the instance %s at once" % name, forced, name, project)
        if now == None or now.get("status_code") != STATUS_STOPPED:
            return None, {"status": "failed", "try_again": False, "reason": reason}
    return "stopped the instance %s at once, because it did not shut down within %d seconds" % (name, seconds), None

def write_instance(inputs, project):
    """Write the instance's whole configuration with the task's parts in it.
    Returns the result of a failed task, or None."""
    name = inputs["name"]

    # Read again right before the write: Incus changes its own volatile.*
    # keys when an instance starts or stops, and the document must hold them
    # as they are now.
    read = read_instance(name, project)
    if read["outcome"] == "missing":
        return {"status": "failed", "reason": "%s: the instance %s is gone since this task read it" % (MODULE, name)}
    if read["outcome"] != "found":
        return reading_failed(read, "the instance " + name)
    document = document_for_the_task(inputs, read["value"])
    written = change(["incus", "config", "edit", "--project", project, "local:" + name], json.encode(document))
    if not failed(written):
        return None
    reason, now = after_a_failed_command("writing the configuration of the instance " + name, written, name, project)
    if now == None:
        return {"status": "failed", "reason": reason}

    # Incus refused the document and stored none of it: a key or a value
    # that it does not accept, or a device that cannot start. Another try
    # sends the same document and gets the same answer.
    return {"status": "invalid_inputs", "reason": reason}
