# libvirt_domain manages one libvirt domain, which is a virtual machine: its
# definition, whether it runs, and whether libvirt starts it when the node
# boots. It runs virsh as root. The behavior is owned by
# docs/changing-mmx/design/modules-official-extra.md, section "libvirt".

MODULE = "libvirt_domain"

# libvirt rewrites a definition when it stores it, so the stored XML never
# equals the task's file as text. The module therefore keeps the SHA-256 of
# the file that it last defined in the domain's own libvirt metadata: one XML
# element under this namespace. libvirt writes the attribute with double
# quotes, whatever the module sends.
RECORD_NAMESPACE = "urn:mmx:libvirt_domain"
RECORD_NAMESPACE_PREFIX = "mmx"
RECORD_START = '<last_defined_file sha256="'

# In a dryrun, the SHA-256 of a file that an earlier node task would change is
# not known yet. This text stands for it in the command that a run would run
# and in the fingerprint, which then differs from every real one.
SHA256_NOT_KNOWN_YET = "the SHA-256 of the changed file"

# The words of virsh for the state of a domain. Three of them are two words,
# so a state is always a whole line.
RUNNING = "running"
PAUSED = "paused"
IN_SHUTDOWN = "in shutdown"
SHUT_OFF = "shut off"
STATES = [RUNNING, PAUSED, IN_SHUTDOWN, SHUT_OFF, "no state", "idle", "crashed", "pmsuspended"]

def run_node_task(inputs):
    problem = problem_with_the_inputs(inputs)
    if problem:
        return {"status": "invalid_inputs", "reason": MODULE + "." + problem}
    missing = missing_program(inputs)
    if missing:
        return {"status": "missing_prerequisite", "reason": MODULE + ": " + missing}
    libvirt = inputs.get("libvirt_connection_uri", "qemu:///system")
    domain, stop = read_domain(libvirt, inputs["name"])
    if stop:
        return stop
    if inputs["state"] == "absent":
        return remove_definition(libvirt, inputs["name"], domain)
    return define_and_set_state(inputs, libvirt, domain)

def problem_with_the_inputs(inputs):
    """The rules that the module description cannot state. Returns "<input>: <the rule>", or None.

    Only name, a plain name, appears in such a reason with its value.
    """
    name = inputs["name"]
    if name == "":
        return "name: must not be empty"
    if name.startswith("-"):
        return "name: must not start with a hyphen (-), which virsh would read as an option"

    # virsh looks a text up as the ID of a running domain, then as the UUID of
    # a domain, and only then as a name. With such a name, a task could change
    # another domain.
    if name.isdigit():
        return "name: must not be a number, and %s is one: virsh takes a number for the ID of a running domain before it looks for a name" % name
    if has_the_form_of_a_uuid(name):
        return "name: must not have the form of a UUID, and %s has it: virsh takes such a text for the UUID of a domain before it looks for a name" % name
    if inputs["state"] == "absent":
        for other in sorted(inputs.keys()):
            if other not in ("name", "state", "libvirt_connection_uri"):
                return "state: absent accepts only name and libvirt_connection_uri; remove " + other
    path = inputs.get("domain_xml_file", "")
    if "domain_xml_file" in inputs and path == "":
        return "domain_xml_file: must not be empty"
    if path != "" and not path.startswith("/"):
        return "domain_xml_file: must be an absolute path, one that starts with /"
    wait = parse_duration(inputs.get("wait_for_shutdown_at_most", "2m"))
    if wait.error or wait.seconds < 0 or int(wait.seconds) != wait.seconds:
        return "wait_for_shutdown_at_most: must be a nonnegative duration of whole seconds, such as 30s or 2m; shutdown checks repeat each second"
    if inputs.get("libvirt_connection_uri", "qemu:///system") == "":
        return "libvirt_connection_uri: must not be empty"
    return None

def has_the_form_of_a_uuid(text):
    """Whether text is 32 hexadecimal digits in groups of 8, 4, 4, 4, and 12 with hyphens between them."""
    if len(text) != 36:
        return False
    for position, character in enumerate(text.elems()):
        if position in [8, 13, 18, 23]:
            if character != "-":
                return False
        elif character not in "0123456789abcdefABCDEF":
            return False
    return True

def missing_program(inputs):
    """What to install when a program that the task runs is not on the node, or None."""
    if not find_program("virsh").found:
        return ("the program virsh is not installed; install the package libvirt-clients (Debian and Ubuntu) or " +
                "libvirt-client (the Red Hat family) in an earlier node task")

    # The wait for a shutdown sleeps one second between two reads.
    if inputs["state"] == "stopped" and not find_program("sleep").found:
        return "the program sleep is not installed; install the package coreutils in an earlier node task"
    return None

# Commands and results.

def virsh(libvirt, arguments):
    """Runs a virsh command that only reads."""
    return run_command(["virsh", "-c", libvirt] + arguments)

def virsh_that_changes_the_node(libvirt, arguments, standard_input = ""):
    """Runs a virsh command that changes libvirt; a dryrun records it and does not run it."""
    return run_command_that_changes_the_node(["virsh", "-c", libvirt] + arguments, standard_input = standard_input)

def did_not_work(command):
    return command.error != "" or command.exit_code != 0

def what_virsh_said(command):
    """virsh's own error message, in one line.

    Only the lines that start with "error: " are the message. A newer libvirt
    also quotes the line of the XML that it could not read, and a domain's XML
    can hold a password, so no other line of standard error reaches a reason.
    """
    if command.error:
        return command.error
    lines = error_lines(command)
    if lines:
        return ": ".join(lines)
    return "virsh ended with the exit code %d and without an error message" % command.exit_code

def error_lines(command):
    """The lines of virsh's error message, each without its "error: "."""
    return [line[len("error: "):] for line in command.standard_error.split("\n") if line.startswith("error: ")]

def failed(did_what, command = None, created = None):
    """A failure that another try may fix: what the module did, on which thing, and virsh's own message.

    After the task created the domain, MMX must not try again: the next try
    would find the domain and report created: False for the domain that this
    task created.
    """
    reason = MODULE + ": " + did_what
    if command != None:
        reason += ": " + what_virsh_said(command)
    result = {"status": "failed", "reason": reason}
    if created != None:
        result["outputs"] = {"created": created}
    if created:
        result["try_again"] = False
    return result

def with_what_the_task_did(stop, reasons):
    """A failure that MMX does not try again also says what the task did before it."""
    if stop.get("try_again") == False and reasons:
        stop["reason"] += ". Before that, the task " + "; ".join(reasons)
    return stop

def missing_state(reason):
    """A state that an earlier node task may change: a dryrun cannot check the task, and a run fails once."""
    return {"status": "missing_prerequisite", "reason": MODULE + ": " + reason}

def cannot_succeed(reason):
    """A failure that another try cannot fix, because the task cannot succeed on the node as it is: MMX tries such a task once."""
    return {"status": "invalid_inputs", "reason": MODULE + ": " + reason}

def did(past_tense, would):
    """A part of the reason: what a run did, or what a dryrun says a run would do."""
    return would if is_dryrun() else past_tense

# Reading.

def read_domain(libvirt, name):
    """Returns (domain, stop). domain is None when libvirt has no domain of this name."""
    listed = virsh(libvirt, ["list", "--all", "--name"])
    if did_not_work(listed):
        if "error: failed to connect to the hypervisor" in listed.standard_error:
            # An earlier node task may start libvirt's socket unit.
            reason = ("%s: libvirt at %s does not answer: %s; start libvirt's socket unit in an earlier node task " +
                      "(libvirtd.socket on Debian and Ubuntu, virtqemud.socket on the other distributions)") % (MODULE, libvirt, what_virsh_said(listed))
            return None, {"status": "not_checked" if is_dryrun() else "failed", "reason": reason}
        return None, failed("listing the domains of libvirt at " + libvirt, listed)

    # One name in each line; the order is not fixed.
    if name not in listed.standard_output.split("\n"):
        return None, None
    return read_domain_information(libvirt, name)

def read_domain_information(libvirt, name, created = None):
    """Reads virsh dominfo: "Key: value" lines. Returns (domain, stop). created is set when the task defined the domain before."""
    answered = virsh(libvirt, ["dominfo", name])
    if did_not_work(answered):
        return None, failed("reading the domain " + name, answered, created)
    values = {}
    for line in answered.standard_output.split("\n"):
        # A line that starts with a space continues the value of the line before, as in the block "Messages:".
        if line.startswith(" ") or ":" not in line:
            continue
        key, _, value = line.partition(":")
        values[key.strip()] = value.strip()
    for key, known in [("Name", None), ("UUID", None), ("State", STATES), ("Persistent", ["yes", "no"]), ("Autostart", ["enable", "disable"])]:
        if key not in values or values[key] == "" or (known != None and values[key] not in known):
            return None, cannot_succeed('reading the domain %s: virsh dominfo printed "%s" for %s, which this module does not know' % (name, values.get(key, ""), key))

    # The rules for name keep out the texts that virsh is known to take for
    # an ID or a UUID. This check holds for any other text that virsh does not
    # look up as a name: every later command would reach that other domain.
    if values["Name"] != name:
        return None, cannot_succeed("reading the domain %s: virsh answered for the domain %s, so it does not take %s for a name; give the domain another name" % (name, values["Name"], name))
    return {
        "state": values["State"],
        "has_stored_definition": values["Persistent"] == "yes",
        "starts_at_boot": values["Autostart"] == "enable",
        "has_saved_memory_image": values.get("Managed save", "") == "yes",
        "uuid": values["UUID"],
    }, None

def read_domain_xml_file(path, name):
    """Returns (text, stop) for the file of domain_xml_file."""
    found = inspect_path(path)
    if found.error:
        return "", failed("reading the file %s of domain_xml_file: %s" % (path, found.error))
    if not found.exists:
        # An earlier node task, such as a file task, writes the file.
        return "", {"status": "missing_prerequisite", "reason": "%s: the file %s of domain_xml_file does not exist; write it in an earlier node task, for example with file" % (MODULE, path)}
    if found.type == "directory":
        return "", {"status": "invalid_inputs", "reason": "%s.domain_xml_file: %s is a directory; give the path of the file that holds the domain's libvirt XML" % (MODULE, path)}
    read = read_file(path)
    if read.error:
        # A file that is too large or no text stays so at another try.
        return "", {"status": "invalid_inputs", "reason": "%s.domain_xml_file: MMX cannot read the file as text: %s" % (MODULE, read.error)}

    # virsh defines whatever <name> a file holds, so a file of another domain
    # would make or replace that other domain.
    if "<name>" + name + "</name>" not in read.content:
        return "", {"status": "invalid_inputs", "reason": "%s.name: the file %s does not hold <name>%s</name>; name must be the <name> of the domain in the XML file" % (MODULE, path, name)}
    return read.content, None

def read_record_of_the_last_defined_file(libvirt, name):
    """Returns (sha256, stop); sha256 is "" when the domain has no record."""

    # --config reads the stored definition; without it, virsh reads the
    # running copy of a running domain, which has no record.
    answered = virsh(libvirt, ["metadata", name, "--uri", RECORD_NAMESPACE, "--config"])
    if did_not_work(answered):
        # libvirt up to 10.0.0 answers a domain without a record with this error; newer ones with empty output.
        if answered.error == "" and "metadata not found" in answered.standard_error:
            return "", None
        return "", failed("reading the record of the last defined file of the domain " + name, answered)
    record = answered.standard_output.strip()

    # Anything else than the module's own record is no record: the domain is then defined again, which writes one.
    if not record.startswith(RECORD_START):
        return "", None
    return record.split('"')[1], None

# Removing the definition.

def remove_definition(libvirt, name, domain):
    if domain == None:
        return {"outputs": {"created": False}, "fingerprint": json.encode({"state": "absent"})}

    # What stands in the way of a removal is something that an earlier node
    # task may change: it shuts the domain off, or deletes a saved memory
    # image, a snapshot, or the record of a checkpoint. So each is a missing
    # state: a dryrun, where the earlier task has changed nothing yet, does
    # not fail.
    if domain["state"] != SHUT_OFF:
        # libvirt itself would remove the definition and leave the domain running.
        return missing_state(("the domain %s is not shut off (its state is %s), and absent removes the definition only of a domain that is shut off. " +
                              "Shut the domain off in an earlier node task, with state: stopped") % (name, domain["state"]))

    # libvirt refuses to remove the definition of a domain with one of these
    # two, and a dryrun must not claim a removal that a run would not do.
    if domain["has_saved_memory_image"]:
        return missing_state(("the domain %s has a saved memory image, and libvirt does not remove the definition of such a domain. " +
                              "Delete the image in an earlier node task (virsh managedsave-remove %s), or start the domain and shut it off") % (name, name))
    snapshots, stop = names_that_virsh_lists(libvirt, "snapshot", name)
    if stop:
        return stop
    if snapshots:
        return missing_state(("the domain %s has snapshots (%s), and libvirt does not remove the definition of such a domain. " +
                              "Delete each of them in an earlier node task (virsh snapshot-delete %s NAME)") % (name, the_first_five(snapshots), name))

    # libvirt does remove the definition of a domain with checkpoints, and
    # with it, without a word, its record of each checkpoint, from which an
    # incremental backup continues. absent removes a definition and nothing
    # else, so the module refuses where libvirt would not.
    checkpoints, stop = names_that_virsh_lists(libvirt, "checkpoint", name)
    if stop:
        return stop
    if checkpoints:
        return missing_state(("the domain %s has checkpoints (%s). libvirt would remove its record of each checkpoint with the definition, " +
                              "and absent removes nothing but the definition. Remove the records in an earlier node task " +
                              "(virsh checkpoint-delete %s NAME --metadata)") % (name, the_first_five(checkpoints), name))

    # --keep-nvram keeps the variables of a UEFI firmware, and --keep-tpm the
    # state of an emulated TPM, which a plain undefine deletes.
    removed = virsh_that_changes_the_node(libvirt, ["undefine", "--keep-nvram", "--keep-tpm", name])
    if did_not_work(removed):
        return failed("removing the definition of the domain " + name, removed, created = False)
    return {
        "reason": did("removed", "would remove") + " the definition of the domain " + name,
        "outputs": {"created": False},
        "fingerprint": json.encode({"state": "absent"}),
    }

def names_that_virsh_lists(libvirt, kind, name):
    """The snapshots or the checkpoints of a domain: one name in each line. Returns (names, stop)."""
    listed = virsh(libvirt, [kind + "-list", name, "--name"])
    if did_not_work(listed):
        return [], failed("listing the %ss of the domain %s" % (kind, name), listed, created = False)
    return [line for line in listed.standard_output.split("\n") if line != ""], None

def the_first_five(names):
    """The names for a reason: the first five, and how many more there are."""
    if len(names) <= 5:
        return ", ".join(names)
    return ", ".join(names[:5]) + ", and %d more" % (len(names) - 5)

# Defining, starting, and shutting off.

def define_and_set_state(inputs, libvirt, domain):
    name = inputs["name"]
    path = inputs.get("domain_xml_file")
    existed = domain != None
    if path == None and not existed:
        return missing_state("the domain " + name + " does not exist; create it in an earlier node task with domain_xml_file, or add domain_xml_file to this task")

    was_active = existed and domain["state"] != SHUT_OFF
    created = False
    reasons = []

    # In a dryrun, the file on the node is not the file of the run while an
    # earlier node task would write it. The module then reads and compares
    # nothing of the old file, or of a file that is not there yet: a run would
    # define the domain from the changed file.
    file_changes_in_this_dryrun = path != None and earlier_node_task_would_change(path)
    if path == None:
        file_sha256 = None
        file_differs_from_definition = False
    elif file_changes_in_this_dryrun:
        file_sha256 = SHA256_NOT_KNOWN_YET
        file_differs_from_definition = True
        virsh_that_changes_the_node(libvirt, ["define", "/dev/stdin"])
        virsh_that_changes_the_node(libvirt, record_command(name, file_sha256))
    else:
        text, stop = read_domain_xml_file(path, name)
        if stop:
            return stop
        file_sha256 = sha256(text)
        recorded_sha256 = ""
        if existed and domain["has_stored_definition"]:
            recorded_sha256, stop = read_record_of_the_last_defined_file(libvirt, name)
            if stop:
                return stop
        file_differs_from_definition = recorded_sha256 != file_sha256
        if file_differs_from_definition:
            stop = define(libvirt, name, path, text, domain)
            if stop:
                return stop
    if file_differs_from_definition:
        created = not existed
        from_the_file = " from " + path + (", which an earlier node task of this dryrun would change" if file_changes_in_this_dryrun else "")
        if not existed:
            reasons.append(did("created", "would create") + " the domain " + name + from_the_file)
        elif not domain["has_stored_definition"]:
            reasons.append(did("stored", "would store") + " a definition for the domain %s, which ran without one," % name + from_the_file)
        else:
            reasons.append(did("defined", "would define") + " the domain %s again" % name + from_the_file)
        if not file_changes_in_this_dryrun:
            # A define removes the record, so the record follows every define.
            # When a run ends between the two commands, the next run finds no
            # record and defines again.
            recorded = virsh_that_changes_the_node(libvirt, record_command(name, file_sha256))
            if did_not_work(recorded):
                return with_what_the_task_did(failed("recording the defined file on the domain " + name, recorded, created), reasons)
        domain, stop = domain_after_define(libvirt, name, domain, created)
        if stop:
            return with_what_the_task_did(stop, reasons)

    start_at_boot = inputs.get("start_at_boot")
    if start_at_boot != None and start_at_boot != domain["starts_at_boot"]:
        marked = virsh_that_changes_the_node(libvirt, ["autostart"] + ([] if start_at_boot else ["--disable"]) + [name])
        if did_not_work(marked):
            return with_what_the_task_did(failed("setting whether the domain %s starts at boot" % name, marked, created), reasons)
        reasons.append(did("set", "would set") + " the domain %s %s at boot" % (name, "to start" if start_at_boot else "not to start"))

    if inputs["state"] == "running":
        reason, stop = make_running(libvirt, name, domain, created)
    else:
        reason, stop = make_stopped(inputs, libvirt, name, domain, created)
    if stop:
        return with_what_the_task_did(stop, reasons)
    if reason:
        reasons.append(reason)

    # libvirt changes only the stored definition of a domain that runs.
    if file_differs_from_definition and was_active and inputs["state"] == "running":
        reasons.append("the running domain %s the old definition until it is shut off and started again" % did("keeps", "would keep"))
    result = {
        "outputs": {"created": created},
        "fingerprint": json.encode({"domain_xml_file_sha256": file_sha256, "state": inputs["state"], "start_at_boot": start_at_boot}),
    }
    if reasons:
        result["reason"] = "; ".join(reasons)
    return result

def define(libvirt, name, path, text, domain):
    """Defines the domain from the file's text. Returns stop."""
    if domain != None and "<uuid>" not in text:
        # libvirt refuses to define an existing name again unless the XML
        # holds the domain's UUID. It accepts the element as the last one.
        end = text.rfind("</domain>")
        if end < 0:
            return {"status": "invalid_inputs", "reason": "%s.domain_xml_file: the file %s holds no </domain>, so it is not the libvirt XML of one domain" % (MODULE, path)}
        text = text[:end] + "<uuid>" + domain["uuid"] + "</uuid>\n" + text[end:]

    # The XML can hold a password, so it reaches virsh through standard input and never through a command line.
    defined = virsh_that_changes_the_node(libvirt, ["define", "/dev/stdin"], standard_input = text)
    if did_not_work(defined):
        if libvirt_refused_the_xml(defined):
            # libvirt refuses the same file at another try.
            return cannot_succeed("defining the domain %s from %s: %s" % (name, path, what_virsh_said(defined)))
        return failed("defining the domain %s from %s" % (name, path), defined, False)
    if defined.ran and not defined.standard_output.startswith("Domain '%s' defined" % name):
        return cannot_succeed("defining the domain %s from %s: libvirt answered \"%s\", so the file defined another domain; name must be the <name> of the domain in the XML file" %
                              (name, path, defined.standard_output.strip()))
    return None

# How the second line of virsh's error message starts when libvirt read the
# XML and refused it, on the libvirt versions of the test nodes. The first
# line, "Failed to define domain from ...", comes with every failed define,
# also with one that another try may fix.
REFUSALS_OF_THE_XML = [
    "at line ",  # not well-formed, libvirt 11.3.0 and newer
    "(domain_definition):",  # not well-formed, libvirt up to 10.0.0
    "XML error: ",
    "missing name information",
    "unsupported configuration: ",
    "operation failed: domain ",  # the file's <uuid> or <name> belongs to another domain
]

def libvirt_refused_the_xml(defined):
    lines = error_lines(defined)
    if defined.error != "" or len(lines) < 2 or not lines[0].startswith("Failed to define domain from "):
        return False
    return any([lines[1].startswith(start) for start in REFUSALS_OF_THE_XML])

def record_command(name, file_sha256):
    """The arguments of the virsh command that records the SHA-256 of the defined file in the domain's metadata."""
    return ["metadata", name, "--uri", RECORD_NAMESPACE, "--key", RECORD_NAMESPACE_PREFIX, "--set", "<last_defined_file sha256='%s'/>" % file_sha256, "--config"]

def domain_after_define(libvirt, name, domain, created):
    """Returns (domain, stop): the domain as libvirt has it after the define."""
    if is_dryrun():
        # The define did not run. A new domain is shut off and does not start
        # at boot; a define leaves the state and that setting of an existing
        # one as they are.
        if domain == None:
            return {"state": SHUT_OFF, "has_stored_definition": True, "starts_at_boot": False, "has_saved_memory_image": False, "uuid": ""}, None
        return domain, None
    return read_domain_information(libvirt, name, created)

def make_running(libvirt, name, domain, created):
    """Returns (reason, stop)."""
    state = domain["state"]
    if state == RUNNING:
        return "", None
    if state == SHUT_OFF:
        started = virsh_that_changes_the_node(libvirt, ["start", name])
        if did_not_work(started):
            return "", failed("starting the domain " + name, started, created)
        return did("started", "would start") + " the domain " + name, None
    if state == PAUSED:
        resumed = virsh_that_changes_the_node(libvirt, ["resume", name])
        if did_not_work(resumed):
            return "", failed("resuming the domain " + name, resumed, created)
        return did("resumed", "would resume") + " the paused domain " + name, None
    if state == IN_SHUTDOWN:
        # The domain is shut off in a moment, and another try can start it then.
        return "", failed("the domain %s shuts down at this moment (its state is \"in shutdown\"); the module starts it once it is shut off" % name, created = created)
    return "", cannot_succeed("the domain %s is in the state \"%s\", from which this module cannot make it run; look at it with virsh domstate --reason %s" % (name, state, name))

def make_stopped(inputs, libvirt, name, domain, created):
    """Returns (reason, stop)."""
    state = domain["state"]
    seconds = int(parse_duration(inputs.get("wait_for_shutdown_at_most", "2m")).seconds)
    power_off = inputs.get("power_off_when_shutdown_takes_longer", False)
    if state == SHUT_OFF:
        return "", None
    if state == PAUSED:
        return "", cannot_succeed("the domain %s is paused, and a request to shut down cannot reach a paused domain; resume it first (virsh resume %s)" % (name, name))
    if state not in [RUNNING, IN_SHUTDOWN]:
        return "", cannot_succeed("the domain %s is in the state \"%s\", from which this module cannot shut it off; look at it with virsh domstate --reason %s" % (name, state, name))
    reason = ""

    # A domain that already shuts down gets no second request.
    if state == RUNNING:
        asked = virsh_that_changes_the_node(libvirt, ["shutdown", name])
        if did_not_work(asked):
            return "", failed("asking the domain %s to shut down" % name, asked, created)
        reason = did("asked", "would ask") + " the domain %s to shut down" % name
    if is_dryrun():
        # Whether the operating system inside reacts in time shows only in a run.
        if reason:
            reason += " and wait at most %d seconds" % seconds
            if power_off:
                reason += "; would cut its power if it still runs then"
        return reason, None

    # virsh shutdown only sends the request, and an operating system can
    # ignore it for ever. So the module reads the state once in each second.
    for second in range(seconds + 1):
        read = virsh(libvirt, ["domstate", name])
        if did_not_work(read):
            return "", failed("reading the state of the domain " + name, read, created)
        state = read.standard_output.strip()
        if state not in STATES:
            return "", cannot_succeed('reading the state of the domain %s: virsh domstate printed "%s", which this module does not know' % (name, state))
        if state == SHUT_OFF:
            return (reason + "; " if reason else "") + "the domain %s was shut off after %d seconds" % (name, second), None
        if second < seconds:
            slept = run_command(["sleep", "1"])
            if did_not_work(slept):
                return "", failed("waiting for the domain %s to shut down: sleep 1: %s" % (name, slept.error or slept.standard_error.strip()), created = created)
    if not power_off:
        # MMX must not try again: another try would send a second request
        # and wait once more, and the task would wait a multiple of its seconds.
        stop = failed(("the domain %s did not shut down within %d seconds%s (its state is %s); its operating system may ignore the request. " +
                       "Set power_off_when_shutdown_takes_longer: true to cut its power then, or give it more time with wait_for_shutdown_at_most") %
                      (name, seconds, " after the task asked it to" if reason else "", state), created = created)
        stop["try_again"] = False
        return "", stop
    cut = virsh_that_changes_the_node(libvirt, ["destroy", name])
    if did_not_work(cut):
        # The domain may have shut down between the last read and this command.
        last = virsh(libvirt, ["domstate", name])
        if "domain is not running" in cut.standard_error and not did_not_work(last) and last.standard_output.strip() == SHUT_OFF:
            return (reason + "; " if reason else "") + "the domain %s was shut off after %d seconds" % (name, seconds), None
        return "", failed("cutting the power of the domain " + name, cut, created)
    return (reason + "; " if reason else "") + "cut the power of the domain %s, which still ran after %d seconds" % (name, seconds), None
