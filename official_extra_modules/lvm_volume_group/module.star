# lvm_volume_group manages one LVM volume group and its physical volumes,
# through the programs of the package lvm2. It creates a missing volume
# group, adds listed devices, takes out devices that the task does not list
# when the task says so, grows listed physical volumes with their devices,
# and removes a volume group that has no logical
# volume. It never wipes a device: a device that holds anything but an
# unused physical volume fails the task.
# docs/changing-mmx/design/modules-official-extra.md owns the behavior ("LVM").

MODULE = "lvm_volume_group"

# The programs the module runs, each with its package.
PROGRAM_PACKAGES = {
    "vgs": "lvm2",
    "pvs": "lvm2",
    "lvs": "lvm2",
    "vgcreate": "lvm2",
    "vgextend": "lvm2",
    "vgreduce": "lvm2",
    "vgremove": "lvm2",
    "pvremove": "lvm2",
    "pvresize": "lvm2",
    "lsblk": "util-linux",
    "wipefs": "util-linux",
}

# LVM's own characters for the name of a volume group.
LVM_NAME_CHARACTERS = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789+_.-"
LONGEST_LVM_NAME = 127
LVM_NAME_RULE = "1 to %d letters, digits, +, _, ., and -, not starting with - and not . or .." % LONGEST_LVM_NAME

PHYSICAL_VOLUME_COLUMNS = ["pv_name", "vg_name", "pv_major", "pv_minor", "pv_used", "pv_size", "dev_size", "pe_start", "pv_pe_count", "vg_extent_size", "pv_mda_count", "pv_mda_size"]

SIZE_UNITS = {"TiB": 1024 * 1024 * 1024 * 1024, "GiB": 1024 * 1024 * 1024, "MiB": 1024 * 1024, "KiB": 1024}

# lsblk's exit code for a path that is no block device or does not exist.
LSBLK_NOT_A_BLOCK_DEVICE = 32

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

def read_device_numbers(device):
    """The kernel's path of a block device and its major and minor numbers as major:minor, which is how the module finds the device in LVM's report whatever path the task wrote: (device, None); (None, None) when the path is no block device; or (None, a failed result)."""
    out = run_command(["lsblk", "--json", "--nodeps", "-o", "PATH,MAJ:MIN", device])
    if not out.error and out.exit_code == LSBLK_NOT_A_BLOCK_DEVICE:
        return None, None
    if out.error or out.exit_code != 0:
        return None, failed("lsblk could not read %s: %s" % (device, tool_message(out)))
    listing = json.decode(out.standard_output, default = None)
    devices = listing.get("blockdevices") if type(listing) == "dict" else None
    if type(devices) != "list" or len(devices) != 1 or type(devices[0]) != "dict" or type(devices[0].get("path")) != "string" or type(devices[0].get("maj:min")) != "string":
        return None, failed("lsblk printed what MMX cannot read for %s: %s" % (device, one_line(out.standard_output)))
    return {"path": devices[0]["path"], "numbers": devices[0]["maj:min"].strip()}, None

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
    if inputs["name"] == "":
        return invalid("name", "must not be empty")
    if not is_lvm_name(inputs["name"]):
        return invalid("name", "expected the volume group's name: " + LVM_NAME_RULE)
    if inputs.get("state", "present") == "absent":
        for other in ("physical_volumes", "physical_volumes_not_listed", "grow_physical_volumes_with_their_devices"):
            if other in inputs:
                return invalid("state", "absent accepts only name; remove " + other)
        return None
    devices = inputs.get("physical_volumes")
    if devices == None and "physical_volumes_not_listed" in inputs:
        return invalid("physical_volumes", "is required with physical_volumes_not_listed")
    if devices == None or len(devices) == 0:
        return invalid("physical_volumes", "is required for state present: the devices of the volume group, at least one, such as [/dev/disk/by-partlabel/lvm]")
    if "physical_volumes_not_listed" not in inputs:
        return invalid("physical_volumes_not_listed", "is required with physical_volumes: remove takes a device that the task does not list out of the volume group, and keep leaves it there")
    for index, device in enumerate(devices):
        if device == "":
            return invalid("physical_volumes[%d]" % index, "must not be empty")
        if not device.startswith("/"):
            return invalid("physical_volumes[%d]" % index, "expected the absolute path of a device, such as /dev/sdb1 or /dev/disk/by-partlabel/lvm")
        if device in devices[:index]:
            return invalid("physical_volumes[%d]" % index, "the same path as physical_volumes[%d]; list each device once" % devices.index(device))
    if "grow_physical_volumes_with_their_devices" not in inputs:
        return invalid("grow_physical_volumes_with_their_devices", "is required for state: present; write true to grow physical volumes with their devices or false to leave physical volumes unchanged")
    return None

def change_the_node(arguments, what, another_try_would_hide_a_failure = False):
    """Run one command that changes the node: None, or a failed result with LVM's message. In a dryrun MMX records the command and does not run it. A command whose failure another try would hide fails the task without another try."""
    out = run_command_that_changes_the_node(arguments)
    if out.error or (out.ran and out.exit_code != 0):
        fails = failed_once if another_try_would_hide_a_failure else failed
        return fails("%s: %s" % (what, tool_message(out)))
    return None

def run_node_task(inputs):
    name = inputs["name"]
    state = inputs.get("state", "present")
    problem = inputs_problem(inputs)
    if problem:
        return problem
    for program, package in PROGRAM_PACKAGES.items():
        found = find_program(program)
        if found.error:
            return failed("looking for the program %s: %s" % (program, found.error))
        if not found.found:
            return {"status": "missing_prerequisite", "reason": "%s: the program %s is not installed; install the package %s in an earlier node task" % (MODULE, program, package)}

    volume_groups, problem = read_lvm_report("vgs", ["vg_name", "lv_count"])
    if problem:
        return problem
    physical_volumes, problem = read_lvm_report("pvs", PHYSICAL_VOLUME_COLUMNS)
    if problem:
        return problem
    volume_group = None
    for row in volume_groups:
        if row["vg_name"] == name:
            volume_group = row
    in_the_group = [row for row in physical_volumes if row["vg_name"] == name]
    if state == "absent":
        return make_absent(name, volume_group, in_the_group)
    return make_present(inputs, volume_group, physical_volumes, in_the_group)

def make_absent(name, volume_group, in_the_group):
    result = {"outputs": {"created": False}, "fingerprint": json.encode({"name": name, "state": "absent"})}
    if volume_group == None:
        return result
    if volume_group["lv_count"] != "0":
        logical_volumes, problem = read_lvm_report("lvs", ["lv_name", "vg_name"])
        if problem:
            return problem
        names = [row["lv_name"] for row in logical_volumes if row["vg_name"] == name]

        # An earlier node task of the same playbook may remove the logical
        # volumes, which a dryrun cannot see: so this is a missing
        # prerequisite and no invalid input.
        return waits_for_an_earlier_node_task("the volume group %s still has the logical volumes %s, so it stays. Remove each of them first with lvm_logical_volume and state: deleted_with_all_its_data, which destroys its data" % (name, ", ".join(names)))
    devices = [row["pv_name"] for row in in_the_group]
    problem = change_the_node(["vgremove", name], "removing the volume group %s" % name)
    if problem:
        return problem

    # LVM leaves its label on a device that leaves a volume group, and other
    # node tasks would then refuse the device as an LVM physical volume. The
    # next try would find no volume group and report success, so a label
    # that stays fails the task without another try.
    for device in devices:
        problem = change_the_node(["pvremove", device], "the volume group %s was removed, and LVM's label stays on %s; remove it by hand with pvremove %s" % (name, device, device), True)
        if problem:
            return problem
    result["reason"] = "%s the volume group %s and LVM's label from %s" % ("would remove" if is_dryrun() else "removed", name, ", ".join(devices))
    return result

def make_present(inputs, volume_group, physical_volumes, in_the_group):
    name = inputs["name"]
    wanted = inputs["physical_volumes"]
    not_listed = inputs["physical_volumes_not_listed"]
    fingerprint = json.encode({"name": name, "state": "present", "physical_volumes": sorted(wanted), "physical_volumes_not_listed": not_listed, "grow_physical_volumes_with_their_devices": inputs["grow_physical_volumes_with_their_devices"]})
    physical_volume_with_numbers = {row["pv_major"] + ":" + row["pv_minor"]: row for row in physical_volumes}

    # Which listed devices the volume group lacks. The module probes each of
    # them itself, so that a dryrun knows what LVM would refuse.
    to_add = []
    to_grow = []
    listed_numbers = {}
    for index, device in enumerate(wanted):
        present = inspect_path(device)
        if present.error:
            return failed("looking at %s: %s" % (device, present.error))
        if not present.exists:
            return {"status": "missing_prerequisite", "reason": "%s: the device %s does not exist yet" % (MODULE, device)}
        found, problem = read_device_numbers(device)
        if problem:
            return problem
        if found == None:
            return refused(device + " is not a block device; physical_volumes lists disks and partitions, such as /dev/sdb1")
        if found["numbers"] in listed_numbers:
            return invalid("physical_volumes", "physical_volumes[%d] and physical_volumes[%d] lead to the same device, %s; list each device once" % (listed_numbers[found["numbers"]], index, found["path"]))
        listed_numbers[found["numbers"]] = index
        physical_volume = physical_volume_with_numbers.get(found["numbers"])
        if physical_volume != None and physical_volume["vg_name"] == name:
            if inputs["grow_physical_volumes_with_their_devices"]:
                sizes, problem = physical_volume_sizes(physical_volume)
                if problem:
                    return problem
                if sizes["device_extent_count"] > sizes["extent_count"]:
                    to_grow.append((device, physical_volume, sizes))
            continue
        if physical_volume != None and physical_volume["vg_name"] != "":
            # An earlier node task may take the device out of the other
            # volume group, or remove that volume group.
            return waits_for_an_earlier_node_task("the device %s belongs to the volume group %s; a device is in one volume group only" % (device, physical_volume["vg_name"]))
        if physical_volume == None:
            # An unused physical volume holds no data and needs no probe;
            # everything else must be empty.
            signatures, problem = read_signatures(device)
            if problem:
                return problem
            if signatures:
                return refused("the device %s is not empty; found: %s. %s adds only a device that holds nothing or an unused LVM physical volume, and never wipes a device. To use this device, first check that nothing on it is needed, then wipe it by hand, such as with wipefs --all %s" % (
                    device,
                    describe_signatures(signatures),
                    MODULE,
                    device,
                ))
        to_add.append(device)

    # Which devices leave the volume group. One that still holds data of a
    # logical volume stays, and LVM would refuse it too.
    to_remove = []
    if not_listed == "remove":
        for row in in_the_group:
            if row["pv_major"] + ":" + row["pv_minor"] in listed_numbers:
                continue
            if row["pv_used"] != "0":
                # An earlier node task may remove the logical volumes that
                # use the device, or move their data away.
                return waits_for_an_earlier_node_task("the device %s of the volume group %s still holds %s of logical volumes, so it cannot leave the volume group. Move that data to the other devices first, such as with pvmove %s, or list the device in physical_volumes" % (
                    row["pv_name"],
                    name,
                    size_text(int(row["pv_used"])) if row["pv_used"].isdigit() else row["pv_used"] + " bytes",
                    row["pv_name"],
                ))
            to_remove.append(row["pv_name"])

    # vgcreate and vgextend get no --yes and no --force: LVM refuses a device
    # that holds a signature, which is the second guard behind the probe.
    reasons = []
    would = is_dryrun()
    if volume_group == None:
        problem = change_the_node(["vgcreate", name] + to_add, "creating the volume group %s on %s" % (name, ", ".join(to_add)))
        if problem:
            return problem
        reasons.append("%s the volume group %s on %s" % ("would create" if would else "created", name, ", ".join(to_add)))
    elif to_add:
        problem = change_the_node(["vgextend", name] + to_add, "adding %s to the volume group %s" % (", ".join(to_add), name))
        if problem:
            return problem
        reasons.append("%s %s to the volume group %s" % ("would add" if would else "added", ", ".join(to_add), name))
    for device in to_remove:
        problem = change_the_node(["vgreduce", name, device], "taking %s out of the volume group %s" % (device, name))
        if problem:
            return problem

        # LVM leaves its label on the device, and other node tasks would
        # then refuse the device as an LVM physical volume. The next try
        # would find the device outside the volume group and report
        # success, so a label that stays fails the task without another try.
        problem = change_the_node(["pvremove", device], "%s left the volume group %s, and LVM's label stays on it; remove it by hand with pvremove %s" % (device, name, device), True)
        if problem:
            return problem
        reasons.append("%s %s out of the volume group %s and %s LVM's label from it" % ("would take" if would else "took", device, name, "remove" if would else "removed"))
    for device, physical_volume, sizes in to_grow:
        reason, problem = grow_physical_volume(device, name, physical_volume, sizes)
        if problem:
            if reasons and not would:
                problem["reason"] += ". Before this: " + "; ".join(reasons)
            return problem
        reasons.append(reason)
    result = {"outputs": {"created": volume_group == None}, "fingerprint": fingerprint}
    if reasons:
        result["reason"] = "; ".join(reasons)
    return result


def physical_volume_sizes(row):
    """Read whole byte sizes and extent counts from one pvs row, without trusting progress text."""
    for column in ("pv_size", "dev_size", "pe_start", "pv_pe_count", "vg_extent_size", "pv_mda_count", "pv_mda_size"):
        if not row[column].strip().isdigit():
            return None, failed("pvs printed what MMX cannot read for %s of the physical volume %s: %s" % (column, row["pv_name"], row[column]))
    extent_bytes = int(row["vg_extent_size"])
    if extent_bytes == 0:
        return None, failed("pvs printed a zero vg_extent_size for the physical volume %s" % row["pv_name"])

    # A physical volume with a second metadata area keeps it at the end of
    # the device, where it takes room from the extents (measured with LVM
    # 2.03.31: a device with room for exactly 76 extents after pe_start
    # holds 75). The module leaves that area's size and one more MiB for
    # its alignment out of the count, so that it never promises growth
    # that pvresize does not give.
    room_bytes = int(row["dev_size"]) - int(row["pe_start"])
    if int(row["pv_mda_count"]) > 1:
        room_bytes -= int(row["pv_mda_size"]) + 1024 * 1024
    return {
        "size_bytes": int(row["pv_size"]),
        "extent_count": int(row["pv_pe_count"]),
        "extent_bytes": extent_bytes,
        "device_extent_count": max(room_bytes, 0) // extent_bytes,
    }, None

def grow_physical_volume(device, name, before_row, before):
    wanted_bytes = before["device_extent_count"] * before["extent_bytes"]
    described = "the physical volume %s of the volume group %s from %s to %s" % (device, name, size_text(before["size_bytes"]), size_text(wanted_bytes))
    out = run_command_that_changes_the_node(["pvresize", device])
    if is_dryrun():
        if out.error:
            return None, failed("growing %s: %s" % (described, tool_message(out)))
        return "would grow " + described, None

    # Even a failed pvresize may have written metadata. A read failure must
    # not be retried: another try could find the grown volume and hide it.
    rows, problem = read_lvm_report("pvs", PHYSICAL_VOLUME_COLUMNS)
    after = None
    if not problem:
        for row in rows:
            if row["pv_major"] == before_row["pv_major"] and row["pv_minor"] == before_row["pv_minor"] and row["vg_name"] == name:
                after, problem = physical_volume_sizes(row)
                break
    if problem:
        problem["try_again"] = False
        problem["reason"] += ". This was after pvresize tried growing " + described + ": " + tool_message(out)
        return None, problem
    if after == None:
        return None, failed_once("pvresize tried growing %s, but the physical volume has no row in LVM's list: %s" % (described, tool_message(out)))
    actual = "the physical volume %s of the volume group %s from %s to %s" % (device, name, size_text(before["size_bytes"]), size_text(after["size_bytes"]))
    if out.error or out.exit_code != 0:
        fails = failed_once if after["extent_count"] > before["extent_count"] else failed
        return None, fails("growing %s: %s. The physical volume went from %s to %s" % (described, tool_message(out), size_text(before["size_bytes"]), size_text(after["size_bytes"])))
    if after["extent_count"] <= before["extent_count"]:
        return None, failed("pvresize reported success but gained no extents while growing %s: %s. The physical volume went from %s to %s; check pvs on the node, or set grow_physical_volumes_with_their_devices: false" % (described, tool_message(out), size_text(before["size_bytes"]), size_text(after["size_bytes"])))
    return "grew " + actual, None
