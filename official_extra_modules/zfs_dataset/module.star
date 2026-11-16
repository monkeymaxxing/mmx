# zfs_dataset manages one ZFS filesystem dataset and its properties, through
# the zfs program. It creates a missing dataset with its properties, sets
# each listed property whose value in effect differs, and destroys a dataset
# only for the state deleted_with_all_its_data. Pools are not its business:
# a missing pool is a missing prerequisite.
# docs/changing-mmx/design/modules-official-extra.md owns the behavior ("ZFS").

MODULE = "zfs_dataset"

# The package of the two programs, for each family of distributions.
ZFS_PACKAGE = "zfsutils-linux (Debian and Ubuntu) or zfs (the Red Hat family)"
PROGRAM_PACKAGES = {"zfs": ZFS_PACKAGE, "zpool": ZFS_PACKAGE}

# The fixed words of ZFS's messages that the module reads. Every other
# failure is passed on with ZFS's own message.
DATASET_DOES_NOT_EXIST = "dataset does not exist"
NO_SUCH_POOL = "no such pool"
INVALID_PROPERTY = "bad property list: invalid property '"
# What ZFS says while its kernel module is not loaded and cannot be loaded:
# ZFS 2.1.5 the first words, ZFS 2.2.2 the second, ZFS 2.2.11 and newer the
# third.
KERNEL_MODULE_NOT_LOADED = ("The ZFS modules are not loaded", "Failed to initialize the libzfs library", "The ZFS modules cannot be auto-loaded")

# How many child datasets and snapshots a reason names; it counts the rest.
MOST_NAMED_BELOW = 5

# K, M, G, and T as ZFS reads them in a size, in either case.
ZFS_SIZE_MULTIPLIERS = {"k": 1024, "m": 1024 * 1024, "g": 1024 * 1024 * 1024, "t": 1024 * 1024 * 1024 * 1024}

def invalid(input_name, rule):
    return {"status": "invalid_inputs", "reason": MODULE + "." + input_name + ": " + rule}

def failed(message):
    return {"status": "failed", "reason": MODULE + ": " + message}

def failed_once(message):
    """The result for a failure after the task changed the node. MMX must not try the task again: the next try would find the changed node and could report success, and nobody would read what failed."""
    return {"status": "failed", "reason": MODULE + ": " + message, "try_again": False}

def waits_for_an_earlier_node_task(message):
    """The result for a state of the node that an earlier node task of the same playbook may change, such as something in the way that it deletes first. A dryrun, in which the earlier task has changed nothing yet, reports the task as not checked; a run fails once with the reason."""
    return {"status": "missing_prerequisite", "reason": MODULE + ": " + message}

def refused(message):
    """The result for a task that cannot succeed on the node as it is, such as on a disk that holds data: another try would end the same way, so MMX tries once, in a dryrun as in a run."""
    return {"status": "invalid_inputs", "reason": MODULE + ": " + message}

def one_line(text):
    """A tool's message on one line."""
    return " ".join([line.strip() for line in text.strip().splitlines() if line.strip()])

def tool_message(out):
    """What a command that failed said: MMX's own error, else what the tool printed."""
    if out.error:
        return out.error
    return one_line(out.standard_error) or one_line(out.standard_output) or "exit code %d without a message" % out.exit_code

def zfs_failed(what, out):
    """The result for a zfs or zpool command that failed. While ZFS's kernel module is not loaded, a dryrun cannot check the task, because an earlier node task may load the module; a run fails."""
    if not out.error and len([words for words in KERNEL_MODULE_NOT_LOADED if words in out.standard_error]) > 0:
        return {
            "status": "not_checked" if is_dryrun() else "failed",
            "reason": "%s: ZFS's kernel module is not loaded on this node, and the zfs program could not load it: %s" % (MODULE, tool_message(out)),
        }
    return failed("%s: %s" % (what, tool_message(out)))

def zfs_size_in_bytes(text):
    """The bytes of a size as ZFS reads one: a number with an optional fraction and an optional K, M, G, or T in either case, with an optional B or iB behind the letter. None for a text that is no such size."""
    lowered = text.lower()
    for ending in ("ib", "b"):
        if lowered.endswith(ending) and lowered[:-len(ending)][-1:] in ZFS_SIZE_MULTIPLIERS:
            lowered = lowered[:-len(ending)]
            break
    multiplier = 1
    if lowered[-1:] in ZFS_SIZE_MULTIPLIERS:
        multiplier = ZFS_SIZE_MULTIPLIERS[lowered[-1:]]
        lowered = lowered[:-1]
    whole, point, fraction = lowered.partition(".")
    if not whole.isdigit() or (point != "" and not fraction.isdigit()):
        return None
    scale = 1
    for _ in fraction.elems():
        scale *= 10
    return (int(whole) * scale + int(fraction or "0")) * multiplier // scale

def is_in_effect(name, wanted, exact, shown):
    """Whether the property has the task's value: the value equals what zfs get prints with -p (exact numbers) or without it (as a person writes it), or both are sizes with the same number of bytes. A user property, which has a colon in its name, is text and nothing else."""
    if wanted == exact or wanted == shown:
        return True
    if ":" in name:
        return False
    wanted_bytes = zfs_size_in_bytes(wanted)
    return wanted_bytes != None and wanted_bytes == zfs_size_in_bytes(exact)

def inputs_problem(inputs):
    """An invalid_inputs result for inputs that break a rule, or None."""
    name = inputs["name"]
    if name == "":
        return invalid("name", "must not be empty")
    if name.startswith("-") or name.startswith("/") or name.endswith("/") or "//" in name or "\t" in name or "\n" in name:
        return invalid("name", "expected a dataset as ZFS names it: the pool and the datasets below it with / between them, such as tank/clickhouse")
    for delimiter, kind in (("@", "snapshot"), ("#", "bookmark")):
        if delimiter in name:
            return invalid("name", "%s names a %s; the module manages filesystem datasets, such as tank/clickhouse" % (name, kind))
    if inputs.get("state", "present") == "deleted_with_all_its_data":
        if "properties" in inputs:
            return invalid("state", "deleted_with_all_its_data accepts only name; remove properties")
        if "/" not in name:
            return invalid("name", "%s is a pool's own dataset, which goes only with its pool; the module deletes datasets below a pool, such as %s/clickhouse" % (name, name))
        return None
    for property_name, value in inputs.get("properties", {}).items():
        if property_name == "" or property_name.startswith("-") or len([character for character in property_name.elems() if character in "=, \t\n"]) > 0:
            return invalid("properties", "%s is no name of a ZFS property; a name has no space, comma, or equals sign, such as compression or com.example:owner" % repr(property_name))
        if "\t" in value or "\n" in value:
            return invalid("properties." + property_name, "the value holds a tab or a line break, which the module cannot read back from zfs get; write the value without one")
    return None

def read_dataset_type(name):
    """The type that zfs list gives for a dataset, such as filesystem or volume, and "" for a dataset that does not exist: (type, None), or (None, a result)."""
    out = run_command(["zfs", "list", "-H", "-p", "-o", "name,type", name])
    if not out.error and out.exit_code != 0 and DATASET_DOES_NOT_EXIST in out.standard_error:
        return "", None
    if out.error or out.exit_code != 0:
        return None, zfs_failed("zfs list could not read the dataset %s" % name, out)
    fields = out.standard_output.rstrip("\n").split("\t")
    if len(fields) != 2 or fields[0] != name or fields[1] == "":
        return None, failed("zfs list printed what MMX cannot read for the dataset %s: %s" % (name, one_line(out.standard_output)))
    return fields[1], None

def read_properties(dataset, names, exact):
    """The value of each named property as zfs get prints it, with -p (exact numbers) or without it (as a person writes it): ({name: value}, None), or (None, a result). ZFS prints no line at all when it does not know one of the names; that name is an invalid input, and so is a second name of a property, for which ZFS prints the property's long name."""
    out = run_command(["zfs", "get", "-H"] + (["-p"] if exact else []) + ["-o", "property,value,source", ",".join(names), dataset])
    if out.error or out.exit_code != 0:
        first_line = one_line(out.standard_error.strip().split("\n")[0]) if not out.error else ""
        if first_line.startswith(INVALID_PROPERTY):
            unknown = first_line[len(INVALID_PROPERTY):].rstrip("'")
            return None, invalid("properties", "ZFS on this node knows no property %s; its command zfs get all %s lists the properties of the dataset, and a property of your own needs a colon in its name, such as com.example:owner" % (unknown, dataset))
        return None, zfs_failed("zfs get could not read the properties of the dataset %s" % dataset, out)
    lines = out.standard_output.rstrip("\n").split("\n")
    if len(lines) != len(names):
        return None, refused("zfs get printed %d lines for the %d properties %s of the dataset %s. A value that holds a line break cannot be read: set that property by hand to a value without one" % (len(lines), len(names), ", ".join(names), dataset))
    values = {}
    for index, line in enumerate(lines):
        # property, then the value up to the last tab, then the source: a
        # value that someone else set can hold a tab.
        first_tab = line.find("\t")
        last_tab = line.rfind("\t")
        if first_tab < 0 or last_tab == first_tab:
            return None, failed("zfs get printed what MMX cannot read for the property %s of the dataset %s: %s" % (names[index], dataset, line))
        printed_name = line[:first_tab]
        if printed_name != names[index]:
            # ZFS takes some properties by a second, shorter name, such as
            # compress for compression, and zfs get prints the long one. The
            # task must write the name that ZFS prints: a second task with
            # the long name would manage the same property.
            return None, invalid("properties", "%s is a second name of the ZFS property %s; write %s in the task, which is the name that zfs get prints" % (names[index], printed_name, printed_name))
        values[names[index]] = line[first_tab + 1:last_tab]
    return values, None

def stored_differently(dataset, wanted):
    """After a change: a result that names the first property whose value in effect is not the task's although ZFS took the value, or None. ZFS stores some values in another form, and the form differs between ZFS versions; with such a value the task would set the property on every run."""
    names = sorted(wanted.keys())
    exact, problem = read_properties(dataset, names, True)
    if problem:
        return problem
    shown, problem = read_properties(dataset, names, False)
    if problem:
        return problem
    for name in names:
        if not is_in_effect(name, wanted[name], exact[name], shown[name]):
            # The task has changed the node by now, and the next try would
            # set the property again: so this failure is not tried again.
            return failed_once("ZFS took the value %s for the property %s of the dataset %s and stores it as %s. Write %s: %s in the task; as written, the task would set the property again on every run" % (
                wanted[name],
                name,
                dataset,
                shown[name],
                name,
                json.encode(shown[name]),
            ))
    return None

def assignments(wanted, names):
    return ["%s=%s" % (name, wanted[name]) for name in names]

def with_warning(reason, out):
    """The reason, with what ZFS printed on standard error although it succeeded, such as that it stored a value and could not share the filesystem."""
    warning = one_line(out.standard_error)
    return reason + ("; ZFS warned: " + warning if warning else "")

def run_node_task(inputs):
    name = inputs["name"]
    state = inputs.get("state", "present")
    wanted = inputs.get("properties", {})
    problem = inputs_problem(inputs)
    if problem:
        return problem
    for program, package in PROGRAM_PACKAGES.items():
        found = find_program(program)
        if found.error:
            return failed("looking for the program %s: %s" % (program, found.error))
        if not found.found:
            return {"status": "missing_prerequisite", "reason": "%s: the program %s is not installed; install the package %s in an earlier node task" % (MODULE, program, package)}
    dataset_type, problem = read_dataset_type(name)
    if problem:
        return problem
    if dataset_type not in ("", "filesystem"):
        return refused("%s is a %s; the module manages filesystem datasets and leaves this one alone" % (name, dataset_type))
    if state == "deleted_with_all_its_data":
        return destroy_with_all_its_data(name, dataset_type)
    fingerprint = json.encode({"name": name, "state": "present", "properties": wanted})
    if dataset_type == "":
        return create(name, wanted, fingerprint)
    return set_properties(name, wanted, fingerprint)

def create(name, wanted, fingerprint):
    names = sorted(wanted.keys())
    pool = name.split("/")[0]
    listed = run_command(["zpool", "list", "-H", "-p", "-o", "name", pool])
    if not listed.error and listed.exit_code != 0 and NO_SUCH_POOL in listed.standard_error:
        return {"status": "missing_prerequisite", "reason": "%s: the ZFS pool %s does not exist; create it in an earlier node task, such as a shell task that runs zpool create behind a guard" % (MODULE, pool)}
    if listed.error or listed.exit_code != 0:
        return zfs_failed("zpool list could not read the pool %s" % pool, listed)
    if "/" not in name:
        return failed("the pool %s exists, and zfs list finds no dataset %s, which every pool has" % (pool, name))
    parent = name.rsplit("/", 1)[0]
    parent_type, problem = read_dataset_type(parent)
    if problem:
        return problem
    if parent_type == "":
        return {"status": "missing_prerequisite", "reason": "%s: the parent dataset %s does not exist; create it in an earlier node task with zfs_dataset" % (MODULE, parent)}
    if parent_type != "filesystem":
        return refused("the parent %s is a %s; a dataset is created below a filesystem" % (parent, parent_type))
    if names:
        # Asked of the parent, so that a name ZFS does not know fails the
        # task before anything is created, in a dryrun too.
        _, problem = read_properties(parent, names, True)
        if problem:
            return problem
    options = []
    for assignment in assignments(wanted, names):
        options += ["-o", assignment]
    created = run_command_that_changes_the_node(["zfs", "create"] + options + [name])
    with_properties = " with " + ", ".join(assignments(wanted, names)) if names else ""
    if created.error:
        return failed("creating the dataset %s: %s" % (name, created.error))
    if not created.ran:
        return {"reason": "would create the dataset %s%s" % (name, with_properties), "outputs": {"created": True}, "fingerprint": fingerprint}
    if created.exit_code != 0:
        # zfs create can fail after it created the dataset, for example when
        # it cannot share or mount the new filesystem. Another try would
        # find the dataset with its properties and report success, so this
        # failure is not tried again.
        now, problem = read_dataset_type(name)
        if problem:
            return problem
        if now == "":
            return zfs_failed("creating the dataset %s%s" % (name, with_properties), created)
        result = failed_once("ZFS created the dataset %s and then failed: %s. The dataset exists now, and the next run compares its properties" % (name, tool_message(created).rstrip(".")))
        result["outputs"] = {"created": True}
        return result
    if names:
        problem = stored_differently(name, wanted)
        if problem:
            problem["reason"] += ". The dataset %s was created" % name
            if problem["status"] == "failed":
                # Another try would find the dataset and report
                # created: false, so this failure is not tried again.
                problem["try_again"] = False
                problem["outputs"] = {"created": True}
            return problem
    return {"reason": with_warning("created the dataset %s%s" % (name, with_properties), created), "outputs": {"created": True}, "fingerprint": fingerprint}

def set_properties(name, wanted, fingerprint):
    result = {"outputs": {"created": False}, "fingerprint": fingerprint}
    names = sorted(wanted.keys())
    if not names:
        return result
    exact, problem = read_properties(name, names, True)
    if problem:
        return problem
    shown, problem = read_properties(name, names, False)
    if problem:
        return problem
    differing = [property_name for property_name in names if not is_in_effect(property_name, wanted[property_name], exact[property_name], shown[property_name])]
    if not differing:
        return result
    changed = run_command_that_changes_the_node(["zfs", "set"] + assignments(wanted, differing) + [name])
    if changed.error or (changed.ran and changed.exit_code != 0):
        return zfs_failed("setting %s on the dataset %s" % (", ".join(assignments(wanted, differing)), name), changed)
    reasons = []
    for property_name in differing:
        # ZFS prints - for a property of the task's own that was never set.
        before = "" if shown[property_name] == "-" and ":" in property_name else " from %s" % shown[property_name]
        reasons.append("%s %s of the dataset %s%s to %s" % ("set" if changed.ran else "would set", property_name, name, before, wanted[property_name]))
    result["reason"] = "; ".join(reasons)
    if changed.ran:
        problem = stored_differently(name, {property_name: wanted[property_name] for property_name in differing})
        if problem:
            if problem["status"] == "failed":
                problem["outputs"] = {"created": False}
            return problem
        result["reason"] = with_warning(result["reason"], changed)
    return result

def destroy_with_all_its_data(name, dataset_type):
    result = {"outputs": {"created": False}, "fingerprint": json.encode({"name": name, "state": "deleted_with_all_its_data"})}
    if dataset_type == "":
        return result

    # ZFS destroys a dataset only without child datasets and snapshots. The
    # module never passes -r, which destroys them too, and names them.
    listed = run_command(["zfs", "list", "-H", "-p", "-o", "name,type", "-r", "-t", "filesystem,volume,snapshot", name])
    if listed.error or listed.exit_code != 0:
        return zfs_failed("zfs list could not read what is below the dataset %s" % name, listed)
    below = []
    for line in listed.standard_output.rstrip("\n").split("\n"):
        fields = line.split("\t")
        if len(fields) != 2:
            return failed("zfs list printed what MMX cannot read below the dataset %s: %s" % (name, line))
        if fields[0] != name:
            below.append("%s (%s)" % (fields[0], fields[1]))
    if below:
        # An earlier node task of the same playbook may destroy them, which
        # a dryrun cannot see: so this is a missing prerequisite.
        named = ", ".join(below[:MOST_NAMED_BELOW])
        if len(below) > MOST_NAMED_BELOW:
            named += ", and %d more" % (len(below) - MOST_NAMED_BELOW)
        return waits_for_an_earlier_node_task("the dataset %s has child datasets or snapshots, so it stays: %s. ZFS destroys a dataset only without them: delete each of them first, a child dataset with its own zfs_dataset task and a snapshot with zfs destroy" % (name, named))
    destroyed = run_command_that_changes_the_node(["zfs", "destroy", name])
    if destroyed.error or (destroyed.ran and destroyed.exit_code != 0):
        return zfs_failed("deleting the dataset %s" % name, destroyed)
    result["reason"] = "%s the dataset %s with all its data" % ("deleted" if destroyed.ran else "would delete", name)
    return result
