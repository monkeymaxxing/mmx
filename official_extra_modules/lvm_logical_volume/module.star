# lvm_logical_volume manages one LVM logical volume, through the programs of
# the package lvm2. It creates a missing volume, grows a smaller one to the
# task's size together with its filesystem, and removes a volume only for
# the state deleted_with_all_its_data. It never shrinks a volume.
# docs/changing-mmx/design/modules-official-extra.md owns the behavior ("LVM").

MODULE = "lvm_logical_volume"

# The programs the module runs, each with its package.
PROGRAM_PACKAGES = {
    "vgs": "lvm2",
    "lvs": "lvm2",
    "lvcreate": "lvm2",
    "lvextend": "lvm2",
    "lvremove": "lvm2",
    "wipefs": "util-linux",
}

# LVM's own characters for the name of a volume group or a logical volume.
LVM_NAME_CHARACTERS = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789+_.-"
LONGEST_LVM_NAME = 127
LVM_NAME_RULE = "1 to %d letters, digits, +, _, ., and -, not starting with - and not . or .." % LONGEST_LVM_NAME

SIZE_UNITS = {"TiB": 1024 * 1024 * 1024 * 1024, "GiB": 1024 * 1024 * 1024, "MiB": 1024 * 1024, "KiB": 1024}
SIZE_UNITS_OF_AN_INPUT = ("MiB", "GiB", "TiB")
ALL_FREE_SPACE_WHEN_CREATED = "all_free_space_when_created"

# The options of lvcreate that the module writes itself, from the task's
# name and size and for the signatures in the new volume's space.
LVCREATE_OPTIONS_OF_THE_MODULE = ("-n", "--name", "-L", "--size", "-l", "--extents", "-y", "--yes", "-W", "--wipesignatures")

# The filesystems that LVM grows together with their volume, as wipefs names
# their signatures.
FILESYSTEMS_THAT_GROW_WITH_THEIR_VOLUME = ("ext2", "ext3", "ext4", "xfs")

# What the module reads of every logical volume of the node. origin, pool_lv,
# and lv_role say which other volumes LVM removes together with one.
LVS_COLUMNS = ["lv_name", "vg_name", "lv_size", "lv_path", "lv_device_open", "origin", "pool_lv", "lv_role"]

# How many depending volumes a reason names; it counts the rest.
MOST_NAMED_DEPENDING_VOLUMES = 5

# What each signature type of wipefs is, in plain words; every other type is
# a filesystem.
SIGNATURE_KINDS = {
    "swap": "swap area",
    "LVM2_member": "LVM physical volume",
    "linux_raid_member": "Linux RAID member",
    "crypto_LUKS": "LUKS encrypted volume",
    "zfs_member": "member of a ZFS pool",
    "gpt": "GPT partition table",
    "dos": "MBR partition table",
    "PMBR": "protective MBR of a GPT partition table",
}

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

def size_in_bytes(text):
    """The bytes of a size such as 50GiB, or None for a text that is no such size."""
    for unit in SIZE_UNITS_OF_AN_INPUT:
        if text.endswith(unit):
            number = text[:-len(unit)]
            if number.isdigit() and not number.startswith("0"):
                return int(number) * SIZE_UNITS[unit]
    return None

def size_text(size_bytes):
    """A number of bytes in the largest binary unit that gives a whole number."""
    for unit, unit_bytes in SIZE_UNITS.items():
        if size_bytes % unit_bytes == 0:
            return "%d %s" % (size_bytes // unit_bytes, unit)
    return "%d bytes" % size_bytes

def is_lvm_name(name):
    if len(name) == 0 or len(name) > LONGEST_LVM_NAME or name.startswith("-") or name in (".", ".."):
        return False
    for character in name.elems():
        if character not in LVM_NAME_CHARACTERS:
            return False
    return True

def read_lvm_report(program, columns):
    """The rows that vgs, pvs, or lvs reports for every volume group, physical volume, or logical volume of the node, each a dictionary of texts: (rows, None), or (None, a failed result). The module asks for all of them and picks its own, so a thing that does not exist is a missing row and never a failed command."""
    out = run_command([program, "--reportformat", "json", "--units", "b", "--nosuffix", "-o", ",".join(columns)])
    listing = None if out.error else json.decode(out.standard_output, default = None)
    if out.error or out.exit_code != 0:
        # Newer LVM writes the message of a failed report into the report's
        # own log, as entries of the type error, and nothing on standard
        # error; older LVM writes it on standard error.
        messages = []
        if type(listing) == "dict" and type(listing.get("log")) == "list":
            messages = [entry.get("log_message", "") for entry in listing["log"] if type(entry) == "dict" and entry.get("log_type") == "error"]
        return None, failed("%s could not read LVM's state: %s" % (program, one_line("\n".join(messages)) or tool_message(out)))
    reports = listing.get("report") if type(listing) == "dict" else None
    rows = reports[0].get(program[:2]) if type(reports) == "list" and len(reports) == 1 and type(reports[0]) == "dict" else None
    if type(rows) != "list":
        return None, failed("%s printed what MMX cannot read: %s" % (program, one_line(out.standard_output)))
    for row in rows:
        for column in columns:
            if type(row) != "dict" or type(row.get(column)) != "string":
                return None, failed("%s printed no %s: %s" % (program, column, one_line(out.standard_output)))
    return rows, None

def read_signatures(device):
    """Every signature that wipefs --no-act finds on the device: (signatures, None), or (None, a failed result). It changes nothing."""
    out = run_command(["wipefs", "--no-act", "--json", device])
    if out.error or out.exit_code != 0:
        return None, failed("wipefs --no-act could not probe %s: %s" % (device, tool_message(out)))
    if out.standard_output.strip() == "":
        return [], None
    listing = json.decode(out.standard_output, default = None)
    signatures = listing.get("signatures") if type(listing) == "dict" else None
    if type(signatures) != "list":
        return None, failed("wipefs --no-act printed what MMX cannot read for %s: %s" % (device, one_line(out.standard_output)))
    for signature in signatures:
        if type(signature) != "dict" or type(signature.get("type")) != "string":
            return None, failed("wipefs --no-act printed what MMX cannot read for %s: %s" % (device, one_line(out.standard_output)))
    return signatures, None

def describe_signatures(signatures):
    """What was found, in plain words, each kind once with the type wipefs gives and where it sits."""
    parts = []
    described = {}
    for signature in signatures:
        signature_type = signature["type"]
        if signature_type in described:
            continue
        described[signature_type] = True
        kind = SIGNATURE_KINDS.get(signature_type, signature_type + " filesystem")
        parts.append("%s (%s signature at offset %s)" % (kind, signature_type, signature.get("offset")))
    return ", ".join(parts)

def inputs_problem(inputs):
    """An invalid_inputs result for inputs that break a rule, or None."""
    for input_name, thing in (("name", "logical volume"), ("volume_group", "volume group")):
        if inputs[input_name] == "":
            return invalid(input_name, "must not be empty")
        if not is_lvm_name(inputs[input_name]):
            return invalid(input_name, "expected the %s's name: %s" % (thing, LVM_NAME_RULE))
    if inputs.get("state", "present") != "present":
        for other in ("size", "grow_filesystem_with_volume", "lvcreate_options"):
            if other in inputs:
                return invalid("state", "deleted_with_all_its_data accepts only name and volume_group; remove " + other)
        return None
    size = inputs.get("size")
    if size == None:
        return invalid("size", "is required for state present: a whole number and MiB, GiB, or TiB, such as 50GiB, or " + ALL_FREE_SPACE_WHEN_CREATED)
    if size != ALL_FREE_SPACE_WHEN_CREATED and size_in_bytes(size) == None:
        return invalid("size", "expected a whole number and MiB, GiB, or TiB, such as 50GiB, or " + ALL_FREE_SPACE_WHEN_CREATED)
    for index, option in enumerate(inputs.get("lvcreate_options", [])):
        if option == "":
            return invalid("lvcreate_options[%d]" % index, "must not be empty")
        for own in LVCREATE_OPTIONS_OF_THE_MODULE:
            # A long option can carry its value behind =, and a short one
            # directly behind its letter.
            if option == own or (own.startswith("--") and option.startswith(own + "=")) or (not own.startswith("--") and option.startswith(own) and not option.startswith("--")):
                return invalid("lvcreate_options[%d]" % index, "the module writes lvcreate's option %s itself, from the task's name and size; leave it out" % own)
    if "grow_filesystem_with_volume" not in inputs:
        return invalid("grow_filesystem_with_volume", "is required for state: present; write true to grow the filesystem with the volume or false to leave the filesystem unchanged")
    return None

def read_logical_volume(volume_group, name):
    """LVM's row for one logical volume: (volume, None), with volume None for a volume that does not exist, or (None, a failed result). The volume names the logical volumes that depend on it, which LVM removes together with it: its snapshots, and for a pool the volumes in the pool. A snapshot of a thin volume is a volume of its own and stays."""
    rows, problem = read_lvm_report("lvs", LVS_COLUMNS)
    if problem:
        return None, problem
    volume = None
    depending = []
    for row in rows:
        if row["vg_name"] != volume_group:
            continue
        if row["lv_name"] == name:
            if not row["lv_size"].isdigit():
                return None, failed("lvs printed the size %s for the logical volume %s/%s, which is no number of bytes" % (row["lv_size"], volume_group, name))

            # LVM prints open for a volume in use, nothing for one that is
            # not, and unknown for a volume that is not active.
            volume = {"size_bytes": int(row["lv_size"]), "device": row["lv_path"], "is_open": row["lv_device_open"] == "open"}
        elif row["pool_lv"] == name:
            depending.append(row["lv_name"] + " (a volume in this pool)")
        elif row["origin"] == name and "thicksnapshot" in row["lv_role"].split(","):
            depending.append(row["lv_name"] + " (a snapshot of it)")
    if volume != None:
        volume["depending"] = depending
    return volume, None

def run_node_task(inputs):
    name = inputs["name"]
    volume_group_name = inputs["volume_group"]
    state = inputs.get("state", "present")
    full_name = volume_group_name + "/" + name
    problem = inputs_problem(inputs)
    if problem:
        return problem
    for program, package in PROGRAM_PACKAGES.items():
        found = find_program(program)
        if found.error:
            return failed("looking for the program %s: %s" % (program, found.error))
        if not found.found:
            return {"status": "missing_prerequisite", "reason": "%s: the program %s is not installed; install the package %s in an earlier node task" % (MODULE, program, package)}

    volume_groups, problem = read_lvm_report("vgs", ["vg_name", "vg_extent_size", "vg_free"])
    if problem:
        return problem
    volume_group = None
    for row in volume_groups:
        if row["vg_name"] == volume_group_name:
            if not row["vg_extent_size"].isdigit() or not row["vg_free"].isdigit() or int(row["vg_extent_size"]) == 0:
                return failed("vgs printed the extent size %s and the free space %s for the volume group %s, which are no numbers of bytes" % (row["vg_extent_size"], row["vg_free"], volume_group_name))
            volume_group = {"extent_bytes": int(row["vg_extent_size"]), "free_bytes": int(row["vg_free"])}
    volume = None
    if volume_group != None:
        volume, problem = read_logical_volume(volume_group_name, name)
        if problem:
            return problem
    if state == "deleted_with_all_its_data":
        return delete_with_all_its_data(full_name, volume)
    if volume_group == None:
        return {"status": "missing_prerequisite", "reason": "%s: the volume group %s does not exist; create it in an earlier node task with lvm_volume_group" % (MODULE, volume_group_name)}
    fingerprint = json.encode({
        "name": name,
        "volume_group": volume_group_name,
        "state": "present",
        "size": inputs["size"],
        "grow_filesystem_with_volume": inputs["grow_filesystem_with_volume"],
        "lvcreate_options": inputs.get("lvcreate_options", []),
    })
    if volume == None:
        return create(inputs, full_name, volume_group, fingerprint)
    outputs = {"created": False, "device": volume["device"]}
    if inputs["size"] == ALL_FREE_SPACE_WHEN_CREATED:
        # The rest of the volume group is what a new volume gets; an existing
        # one has its size.
        return {"outputs": outputs, "fingerprint": fingerprint}

    # LVM rounds a size up to whole extents of the volume group.
    extent_bytes = volume_group["extent_bytes"]
    wanted_bytes = (size_in_bytes(inputs["size"]) + extent_bytes - 1) // extent_bytes * extent_bytes
    if volume["size_bytes"] == wanted_bytes:
        return {"outputs": outputs, "fingerprint": fingerprint}
    if volume["size_bytes"] > wanted_bytes:
        return refused("the logical volume %s has %s, and the task asks for %s. The module never shrinks a logical volume, because that destroys the data behind the new end. Write the size that the volume has, or shrink the filesystem and the volume by hand" % (full_name, size_text(volume["size_bytes"]), size_text(wanted_bytes)))
    result = grow(inputs, full_name, volume, volume_group, wanted_bytes)
    if result.get("status", "succeeded") == "succeeded":
        result["outputs"] = outputs
        result["fingerprint"] = fingerprint
    return result

def delete_with_all_its_data(full_name, volume):
    result = {"outputs": {"created": False}, "fingerprint": json.encode({"logical_volume": full_name, "state": "deleted_with_all_its_data"})}
    if volume == None:
        return result
    if volume["depending"]:
        # LVM asks before it removes a volume with its snapshots, or a pool
        # with its volumes, and --yes would answer that question too: LVM
        # would then delete volumes that the task does not name. An earlier
        # node task of the same playbook may delete them, so this is a
        # missing prerequisite.
        named = ", ".join(volume["depending"][:MOST_NAMED_DEPENDING_VOLUMES])
        if len(volume["depending"]) > MOST_NAMED_DEPENDING_VOLUMES:
            named += ", and %d more" % (len(volume["depending"]) - MOST_NAMED_DEPENDING_VOLUMES)
        return waits_for_an_earlier_node_task("other logical volumes depend on the logical volume %s, and LVM deletes them together with it: %s. Delete each of them first with its own lvm_logical_volume task and state: deleted_with_all_its_data; nothing was deleted" % (full_name, named))
    if volume["is_open"]:
        # LVM refuses such a volume too. An earlier node task of the same
        # playbook may unmount it, which a dryrun cannot see.
        return waits_for_an_earlier_node_task("the logical volume %s is in use: a filesystem on it is mounted, or a program holds the device open. Unmount it or stop the program first; nothing was deleted" % full_name)

    # --yes answers LVM's question whether to remove the volume; the state's
    # name says that the data is destroyed. No volume depends on this one,
    # so the answer covers this volume only.
    removed = run_command_that_changes_the_node(["lvremove", "--yes", full_name])
    if removed.error or (removed.ran and removed.exit_code != 0):
        return failed("deleting the logical volume %s: %s" % (full_name, tool_message(removed)))
    result["reason"] = "%s the logical volume %s with all its data (%s)" % ("deleted" if removed.ran else "would delete", full_name, size_text(volume["size_bytes"]))
    return result

def create(inputs, full_name, volume_group, fingerprint):
    name = inputs["name"]
    volume_group_name = inputs["volume_group"]
    extent_bytes = volume_group["extent_bytes"]
    if inputs["size"] == ALL_FREE_SPACE_WHEN_CREATED:
        if volume_group["free_bytes"] < extent_bytes:
            return waits_for_an_earlier_node_task("the volume group %s has no free space, so the new logical volume %s cannot take the rest of it" % (volume_group_name, full_name))
        wanted_bytes = volume_group["free_bytes"]
        size_arguments = ["--extents", "100%FREE"]
        size_words = "%s, the rest of the volume group" % size_text(wanted_bytes)
    else:
        asked_bytes = size_in_bytes(inputs["size"])
        wanted_bytes = (asked_bytes + extent_bytes - 1) // extent_bytes * extent_bytes
        size_arguments = ["--size", inputs["size"]]
        size_words = size_text(wanted_bytes)
        if wanted_bytes != asked_bytes:
            size_words += ", which is %s rounded up to whole extents of %s" % (size_text(asked_bytes), size_text(extent_bytes))
        if wanted_bytes > volume_group["free_bytes"]:
            return waits_for_an_earlier_node_task("the volume group %s has %s free, and the new logical volume %s needs %s" % (volume_group_name, size_text(volume_group["free_bytes"]), full_name, size_text(wanted_bytes)))

    # The space of a new volume belongs to no volume, so nothing that exists
    # is lost when lvcreate wipes an old signature there. Without these two
    # options lvcreate asks about such a signature, answers no itself when
    # it has no terminal, and creates nothing.
    created = run_command_that_changes_the_node(["lvcreate", "--yes", "--wipesignatures", "y", "--name", name] + size_arguments + inputs.get("lvcreate_options", []) + [volume_group_name])
    if created.error or (created.ran and created.exit_code != 0):
        return failed("creating the logical volume %s: %s" % (full_name, tool_message(created)))
    if not created.ran:
        return {"reason": "would create the logical volume %s with %s" % (full_name, size_words), "outputs": {"created": True, "device": "/dev/" + full_name}, "fingerprint": fingerprint}
    volume, problem = read_logical_volume(volume_group_name, name)
    if problem:
        # The volume exists. Another try would find it and report
        # created: false, so this failure is not tried again.
        problem["reason"] += ". This was after lvcreate created the logical volume %s" % full_name
        problem["try_again"] = False
        problem["outputs"] = {"created": True, "device": "/dev/" + full_name}
        return problem
    if volume == None:
        return failed("lvcreate reported success, but LVM lists no logical volume %s" % full_name)
    if volume["size_bytes"] != wanted_bytes:
        # lvcreate's own options, such as a mirror, can change what fits.
        size_words = size_text(volume["size_bytes"])
    return {"reason": "created the logical volume %s with %s" % (full_name, size_words), "outputs": {"created": True, "device": volume["device"]}, "fingerprint": fingerprint}

def grow(inputs, full_name, volume, volume_group, wanted_bytes):
    name = inputs["name"]
    volume_group_name = inputs["volume_group"]
    with_filesystem = inputs["grow_filesystem_with_volume"]
    if wanted_bytes - volume["size_bytes"] > volume_group["free_bytes"]:
        # An earlier node task may give the volume group another device.
        return waits_for_an_earlier_node_task("the logical volume %s has %s and needs %s more for %s, and the volume group %s has %s free" % (
            full_name,
            size_text(volume["size_bytes"]),
            size_text(wanted_bytes - volume["size_bytes"]),
            size_text(wanted_bytes),
            volume_group_name,
            size_text(volume_group["free_bytes"]),
        ))
    arguments = ["lvextend"]
    grows_filesystem = False
    filesystem_words = "and not a filesystem on it, because grow_filesystem_with_volume is false"
    if with_filesystem:
        # LVM grows a volume together with its filesystem only when it finds
        # a filesystem that it can grow, and no reading command of LVM says
        # so beforehand. The module looks itself, so that a dryrun promises
        # no growth that the run refuses.
        signatures, problem = read_signatures(volume["device"])
        if problem:
            return problem
        if not signatures:
            # A volume that holds nothing has no filesystem that could be
            # left small, so it grows alone and loses nothing. That is the
            # volume of a first run that stopped before its filesystem
            # task, when the size was raised before the next run.
            filesystem_words = "and no filesystem, because the volume holds none yet"
        elif len(signatures) != 1 or signatures[0]["type"] not in FILESYSTEMS_THAT_GROW_WITH_THEIR_VOLUME:
            return refused("the logical volume %s holds no ext2, ext3, ext4, or xfs filesystem (found: %s), and grow_filesystem_with_volume is true: LVM grows a volume together with its filesystem only for these four. To grow a volume without such a filesystem, such as swap, set grow_filesystem_with_volume: false. Nothing was changed" % (
                full_name,
                describe_signatures(signatures),
            ))
        else:
            grows_filesystem = True
            filesystem_words = "together with the filesystem on it"
            arguments.append("--resizefs")

            # Newer LVM grows an xfs filesystem that is not mounted only with
            # --fsmode manage, and then mounts it for a moment by itself;
            # older LVM does the same without the option and does not know
            # it.
            help_text = run_command(["lvextend", "--help"])
            if help_text.error or help_text.exit_code != 0:
                return failed("lvextend --help, to learn whether this LVM has the option --fsmode: %s" % tool_message(help_text))
            if "--fsmode" in help_text.standard_output:
                arguments += ["--fsmode", "manage"]
    grown = run_command_that_changes_the_node(arguments + ["--size", inputs["size"], full_name])
    change = "from %s to %s" % (size_text(volume["size_bytes"]), size_text(wanted_bytes))
    if grown.error:
        return failed("growing the logical volume %s %s: %s" % (full_name, change, grown.error))
    if not grown.ran:
        return {"reason": "would grow the logical volume %s %s, %s" % (full_name, change, filesystem_words)}
    after, problem = read_logical_volume(volume_group_name, name)
    if problem:
        return problem
    if grown.exit_code != 0:
        # When LVM itself refuses, nothing has grown. When the filesystem's
        # own program fails, such as for a filesystem that is mounted
        # read-only, the volume has grown and the filesystem has not.
        tried = "growing the logical volume %s %s, %s: %s. " % (full_name, change, filesystem_words, tool_message(grown).rstrip("."))
        if after == None:
            return failed(tried + "LVM lists no such logical volume now")
        if after["size_bytes"] == volume["size_bytes"]:
            return failed(tried + "The logical volume still has %s" % size_text(after["size_bytes"]))

        # Another try would find the volume at its size and report success,
        # with the filesystem still small: so this failure is not tried again.
        now = "The logical volume now has %s" % size_text(after["size_bytes"])
        if grows_filesystem:
            now += ", and its filesystem is still smaller: once the cause is gone, grow the filesystem by hand, with resize2fs for ext2, ext3, and ext4 or xfs_growfs for xfs. The next run finds the volume at its size and grows nothing"
        result = failed_once(tried + now)
        result["outputs"] = {"created": False, "device": after["device"]}
        return result
    if after == None or after["size_bytes"] != wanted_bytes:
        return failed("lvextend reported success, but the logical volume %s has %s and not %s" % (full_name, "no row in LVM's list" if after == None else size_text(after["size_bytes"]), size_text(wanted_bytes)))
    return {"reason": "grew the logical volume %s %s, %s" % (full_name, change, filesystem_words)}
