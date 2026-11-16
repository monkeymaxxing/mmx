# Grows one existing partition through growpart, leaving its contents alone.
# docs/changing-mmx/design/modules-official-extra.md ("Growing a partition") owns the behavior.
MODULE = "grow_partition_into_free_space_after_it"
SIZE_UNITS = {"TiB": 1024 * 1024 * 1024 * 1024, "GiB": 1024 * 1024 * 1024, "MiB": 1024 * 1024, "KiB": 1024}
LSBLK_NOT_A_BLOCK_DEVICE = 32
PROGRAM_PACKAGES = {
    "growpart": "cloud-guest-utils (Debian and Ubuntu) or cloud-utils-growpart (the Red Hat family)",
    "lsblk": "util-linux",
    "wipefs": "util-linux",
    "sfdisk": "fdisk (Debian and Ubuntu) or util-linux (the Red Hat family)",
    "blockdev": "util-linux",
}

def failed(message):
    return {"status": "failed", "reason": MODULE + ": " + message}

def invalid(input_name, rule):
    return {"status": "invalid_inputs", "reason": MODULE + "." + input_name + ": " + rule}

def missing(message):
    return {"status": "missing_prerequisite", "reason": MODULE + ": " + message}

def refused(message):
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

def read_block_device(path):
    """What lsblk says about one block device: (device, None) with path, kernel_name, kernel_name_of_its_disk, type, size_bytes, and sector_bytes; (None, None) when the path is no block device; or (None, a failed result)."""
    out = run_command(["lsblk", "--json", "--bytes", "--nodeps", "-o", "PATH,KNAME,PKNAME,TYPE,SIZE,LOG-SEC", path])
    if not out.error and out.exit_code == LSBLK_NOT_A_BLOCK_DEVICE:
        return None, None
    if out.error or out.exit_code != 0:
        return None, failed("lsblk could not read %s: %s" % (path, tool_message(out)))
    listing = json.decode(out.standard_output, default = None)
    devices = listing.get("blockdevices") if type(listing) == "dict" else None
    if type(devices) != "list" or len(devices) != 1 or type(devices[0]) != "dict":
        return None, failed("lsblk printed what MMX cannot read for %s: %s" % (path, one_line(out.standard_output)))
    found = devices[0]
    for key in ("path", "kname", "type"):
        if type(found.get(key)) != "string":
            return None, failed("lsblk printed no %s for %s: %s" % (key, path, one_line(out.standard_output)))
    for key in ("size", "log-sec"):
        if type(found.get(key)) != "int":
            return None, failed("lsblk printed no number as %s of %s: %s" % (key, path, one_line(out.standard_output)))
    return {
        "path": found["path"],
        "kernel_name": found["kname"],
        "kernel_name_of_its_disk": found.get("pkname") or "",
        "type": found["type"],
        "size_bytes": found["size"],
        "sector_bytes": found["log-sec"],
    }, None

def read_partition(disk, number, kernel_disk_path):
    out = run_command(["sfdisk", "--json", kernel_disk_path])
    if out.error or out.exit_code != 0:
        return None, failed("reading the partition table of %s: %s" % (disk, tool_message(out)))
    listing = json.decode(out.standard_output, default = None)
    table = listing.get("partitiontable") if type(listing) == "dict" else None
    if type(table) != "dict" or type(table.get("device")) != "string" or type(table.get("partitions")) != "list":
        return None, failed("sfdisk printed what MMX cannot read for %s: %s" % (disk, one_line(out.standard_output)))
    # Read through lsblk's kernel path: sfdisk gives by-id partitions
    # special names, which must not be guessed from the input link.
    device = table["device"]
    partition_path = device + ("p" if device[-1:].isdigit() else "") + str(number)
    for partition in table["partitions"]:
        if type(partition) != "dict" or type(partition.get("node")) != "string" or type(partition.get("size")) != "int" or type(partition.get("start")) != "int":
            return None, failed("sfdisk printed what MMX cannot read for %s: %s" % (disk, one_line(out.standard_output)))
        if partition["node"] == partition_path:
            return partition, None
    return None, missing("the disk %s has no partition %d; create it in an earlier node task" % (disk, number))

def read_positive_number(arguments):
    out = run_command(arguments)
    text = out.standard_output.strip()
    if out.error or out.exit_code != 0:
        return None, failed("reading with %s: %s" % (" ".join(arguments), tool_message(out)))
    if not text.isdigit() or int(text) <= 0:
        return None, failed("%s printed what MMX cannot read: %s" % (" ".join(arguments), tool_message(out)))
    return int(text), None

def change_sizes(out, prefix, number):
    lines = [line for line in out.standard_output.splitlines() if line.startswith(prefix)]
    if len(lines) != 1:
        return None

    # growpart 0.33 prints "new: size=1021919 end=1228766"; older versions
    # put a comma between the two, and some count the end one sector later.
    # The module reads the start and the two sizes, and checks them against
    # the partition table that it read itself.
    words = lines[0].replace(",", " ").split()
    if len(words) != 9 or words[1] != "partition=%d" % number or words[3] != "old:" or words[6] != "new:":
        return None
    values = []
    for index, key in [(2, "start="), (4, "size="), (5, "end="), (7, "size="), (8, "end=")]:
        if not words[index].startswith(key) or not words[index][len(key):].isdigit():
            return None
        values.append(int(words[index][len(key):]))
    start, old, _, new, _ = values
    if old <= 0 or new <= old:
        return None
    return {"start": start, "old": old, "new": new}

def run_node_task(inputs):
    disk = inputs["device"]
    number = inputs["partition_number"]
    if disk == "":
        return invalid("device", "must not be empty")
    if not disk.startswith("/"):
        return invalid("device", "expected the absolute path of the whole disk, such as /dev/sdb")
    if number < 1:
        return invalid("partition_number", "must be 1 or more")
    for program, package in PROGRAM_PACKAGES.items():
        found = find_program(program)
        if found.error:
            return failed("looking for the program %s: %s" % (program, found.error))
        if not found.found:
            return missing("the program %s is not installed; install the package %s in an earlier node task" % (program, package))
    fingerprint = json.encode({"device": disk, "partition_number": number})
    present = inspect_path(disk)
    if present.error:
        return failed("looking at %s: %s" % (disk, present.error))
    if not present.exists:
        return missing("the disk %s does not exist yet" % disk)
    device, problem = read_block_device(disk)
    if problem:
        return problem
    if device == None:
        return refused(disk + " is not a block device; give the whole disk, such as /dev/sdb")
    if device["type"] == "part":
        return refused("%s is a partition of the disk /dev/%s, not a whole disk; give the whole disk" % (disk, device["kernel_name_of_its_disk"]))
    if device["type"] == "rom":
        return refused(disk + " is a read-only medium, such as a CD drive; give a writable disk")
    if device["sector_bytes"] not in (512, 4096):
        return refused("%s has sectors of %d bytes; the module knows disks with sectors of 512 and of 4096 bytes" % (disk, device["sector_bytes"]))
    probe = run_command(["wipefs", "--no-act", "--json", "--output", "TYPE,USAGE", disk])
    if probe.error or probe.exit_code != 0:
        return failed("probing %s: %s" % (disk, tool_message(probe)))
    listing = json.decode(probe.standard_output, default = None)
    signatures = listing.get("signatures") if type(listing) == "dict" else None
    if type(signatures) != "list" or any([type(signature) != "dict" or type(signature.get("type")) != "string" or type(signature.get("usage")) != "string" for signature in signatures]):
        return failed("wipefs printed what MMX cannot read for %s: %s" % (disk, one_line(probe.standard_output)))
    if not any([signature["usage"] == "partition-table" for signature in signatures]):
        return missing("the disk %s has no partition table; create it in an earlier node task" % disk)
    before, problem = read_partition(disk, number, device["path"])
    if problem:
        return problem
    sector_bytes, problem = read_positive_number(["blockdev", "--getss", disk])
    if problem:
        return problem

    # growpart gets the kernel's own path of the disk, as lsblk gave it: the
    # partitions of a link such as /dev/disk/by-id/... have other names.
    preview = run_command(["growpart", "--dry-run", device["path"], str(number)])
    if not preview.error and preview.exit_code == 1 and any([line.startswith("NOCHANGE:") for line in preview.standard_output.splitlines()]):
        return {"fingerprint": fingerprint}
    sizes = change_sizes(preview, "CHANGE:", number)
    if preview.error or preview.exit_code != 0 or sizes == None:
        return failed("checking growth of partition %d of %s: %s" % (number, disk, tool_message(preview)))
    if sizes["old"] != before["size"] or sizes["start"] != before["start"]:
        return failed("growpart's proposed size or first sector differs from the partition table of %s: %s" % (disk, one_line(preview.standard_output)))
    growth = "partition %d of %s from %s to %s" % (number, disk, size_text(before["size"] * sector_bytes), size_text(sizes["new"] * sector_bytes))
    changed = run_command_that_changes_the_node(["growpart", device["path"], str(number)])
    if not changed.ran and not changed.error:
        return {"reason": "would grow " + growth, "fingerprint": fingerprint}
    after, problem = read_partition(disk, number, device["path"])
    if changed.error or changed.exit_code != 0 or change_sizes(changed, "CHANGED:", number) == None:
        result = failed("growing partition %d of %s: %s" % (number, disk, tool_message(changed)))
        if problem:
            result["reason"] += ". The partition size could not be read afterwards: " + problem["reason"]
            result["try_again"] = False
        elif after["size"] != before["size"] or after["start"] != before["start"]:
            result["reason"] += ". The partition now has " + size_text(after["size"] * sector_bytes)
            result["try_again"] = False
        else:
            result["reason"] += ". The partition still has " + size_text(before["size"] * sector_bytes)
        return result
    if not problem:
        kernel_bytes, problem = read_positive_number(["blockdev", "--getsize64", after["node"]])
        if not problem and (after["start"] != before["start"] or after["size"] != sizes["new"] or kernel_bytes != after["size"] * sector_bytes):
            problem = failed("growpart reported success, but the partition table or kernel size differs from its proposal for partition %d of %s; the table has %s and the kernel has %s" % (number, disk, size_text(after["size"] * sector_bytes), size_text(kernel_bytes)))
    if problem:
        problem["status"] = "failed"
        problem["try_again"] = False
        return problem
    return {"reason": "grew partition %d of %s from %s to %s" % (number, disk, size_text(before["size"] * sector_bytes), size_text(kernel_bytes)), "fingerprint": fingerprint}
