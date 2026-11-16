# create_partitions_on_empty_disk writes a GPT partition table with named
# partitions on a disk that holds nothing yet, through sfdisk. Its one job is
# to never destroy data: it writes only when its own probe finds no partition
# table and no signature of any kind, is unchanged when the disk already has
# exactly these partitions, and fails for everything else. It has no force
# option, no removal, and no resize.
# docs/changing-mmx/design/modules-official-extra.md owns the behavior ("Disk partitions").

MODULE = "create_partitions_on_empty_disk"

MIB = 1024 * 1024
SIZE_UNITS = {"TiB": 1024 * 1024 * 1024 * 1024, "GiB": 1024 * 1024 * 1024, "MiB": 1024 * 1024, "KiB": 1024}
SIZE_UNITS_OF_AN_INPUT = ("MiB", "GiB", "TiB")
REST_OF_DISK = "rest_of_disk"

# sfdisk --json prints a partition's type as this GUID, in capital letters.
PARTITION_TYPE_GUIDS = {
    "linux_filesystem": "0FC63DAF-8483-4772-8E79-3D69D8477DE4",
    "linux_lvm": "E6D6D379-F507-44C2-A23C-238F2A3DF928",
    "linux_swap": "0657FD6D-A4AB-43C4-84E5-0933C84B4F4F",
    "linux_raid": "A19D880F-05FC-4D3B-A006-743F0F84911E",
    "efi_system": "C12A7328-F81F-11D2-BA4B-00A0C93EC93B",
}

# Each of these characters gives the plain link /dev/disk/by-partlabel/<name>
# and needs no quotes in sfdisk's script.
PARTITION_NAME_CHARACTERS = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-_."
LONGEST_PARTITION_NAME = 36

# A GPT partition table has room for this many partitions.
MOST_PARTITIONS = 128

# The sectors that a GPT partition table keeps for its second copy at the end
# of the disk, by the disk's sector size: 128 entries of 128 bytes and one
# sector for the header. The last usable sector is the one before them, which
# sfdisk --json prints as lastlba: the number of sectors minus 34, or minus 6.
SECTORS_AFTER_THE_LAST_USABLE_ONE = {512: 33, 4096: 5}

PARTITION_LINKS = "/dev/disk/by-partlabel/"

# The package of each program, for each family of distributions.
PROGRAM_PACKAGES = {
    "sfdisk": "fdisk (Debian and Ubuntu) or util-linux (the Red Hat family)",
    "wipefs": "util-linux",
    "lsblk": "util-linux",
    "udevadm": "udev (Debian and Ubuntu) or systemd-udev (the Red Hat family)",
}

# lsblk's exit code for a path that is no block device or does not exist.
LSBLK_NOT_A_BLOCK_DEVICE = 32

# The first words of what sfdisk says when it refuses a disk that is in use.
SFDISK_DISK_IN_USE = "This disk is currently in use"

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

def is_partition_name(name):
    # . and .. are no names of a link below /dev/disk/by-partlabel.
    if type(name) != "string" or len(name) == 0 or len(name) > LONGEST_PARTITION_NAME or name in (".", ".."):
        return False
    for character in name.elems():
        if character not in PARTITION_NAME_CHARACTERS:
            return False
    return True

def wanted_partitions(inputs):
    """The partitions the task asks for, each with name, size_bytes (None for rest_of_disk), type, and type_guid: (partitions, None), or (None, an invalid_inputs result)."""
    if inputs["device"] == "":
        return None, invalid("device", "must not be empty")
    if not inputs["device"].startswith("/"):
        return None, invalid("device", "expected the absolute path of the whole disk, such as /dev/sdb")
    listed = inputs["partitions"]
    if type(listed) != "list" or len(listed) == 0:
        return None, invalid("partitions", "expected a list of at least one partition, each with a name and a size")
    if len(listed) > MOST_PARTITIONS:
        return None, invalid("partitions", "a GPT partition table holds at most %d partitions, and the task lists %d" % (MOST_PARTITIONS, len(listed)))
    wanted = []
    place_of_name = {}
    for index, partition in enumerate(listed):
        place = "partitions[%d]" % index
        if type(partition) != "dict":
            return None, invalid(place, "expected a partition with name, size, and optionally type, such as {name: database, size: 200GiB}")
        for key in partition:
            if key not in ("name", "size", "type"):
                return None, invalid(place, "unknown key %s; a partition has name, size, and type" % key)
        name = partition.get("name")
        if name == "":
            return None, invalid(place + ".name", "must not be empty")
        if not is_partition_name(name):
            return None, invalid(place + ".name", "expected the partition's name: 1 to %d letters, digits, -, _, and ., and not . or .." % LONGEST_PARTITION_NAME)
        if name in place_of_name:
            return None, invalid(place + ".name", "the name %s is also the name of %s; each partition needs its own name, because later node tasks find it as %s%s" % (name, place_of_name[name], PARTITION_LINKS, name))
        place_of_name[name] = place
        size = partition.get("size")
        size_bytes = None
        if size == REST_OF_DISK:
            if index != len(listed) - 1:
                return None, invalid(place + ".size", "only the last partition can have the size " + REST_OF_DISK)
        else:
            size_bytes = size_in_bytes(size) if type(size) == "string" else None
            if size_bytes == None:
                return None, invalid(place + ".size", "expected a whole number and MiB, GiB, or TiB, such as 200GiB, or %s for the last partition" % REST_OF_DISK)
        partition_type = partition.get("type", "linux_filesystem")
        if type(partition_type) != "string" or partition_type not in PARTITION_TYPE_GUIDS:
            return None, invalid(place + ".type", "expected one of " + ", ".join(PARTITION_TYPE_GUIDS.keys()))
        wanted.append({"name": name, "size_bytes": size_bytes, "type": partition_type, "type_guid": PARTITION_TYPE_GUIDS[partition_type]})
    return wanted, None

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

def read_what_is_on_the_disk(device):
    """What the kernel has on the disk: (texts, None) with one text for a mount point of the disk and one for each other device on it, or (None, a failed result). lsblk lists the disk first and then every device on it: a partition that the kernel still has from an earlier partition table, or a device-mapper device, such as an encrypted volume without a header. Such a disk is in use although it holds no signature."""
    out = run_command(["lsblk", "--json", "-o", "PATH,KNAME,TYPE,MOUNTPOINT", device])
    if out.error or out.exit_code != 0:
        return None, failed("lsblk could not read what is on %s: %s" % (device, tool_message(out)))
    listing = json.decode(out.standard_output, default = None)
    devices = listing.get("blockdevices") if type(listing) == "dict" else None
    if type(devices) != "list" or len(devices) == 0:
        return None, failed("lsblk printed what MMX cannot read for what is on %s: %s" % (device, one_line(out.standard_output)))
    found = []
    for index, listed in enumerate(devices):
        if type(listed) != "dict" or type(listed.get("path")) != "string" or type(listed.get("type")) != "string" or "children" in listed:
            return None, failed("lsblk printed what MMX cannot read for what is on %s: %s" % (device, one_line(out.standard_output)))
        if index == 0:
            if listed.get("mountpoint"):
                found.append("it is mounted at %s" % listed["mountpoint"])
        elif listed["type"] == "part":
            found.append("the kernel still has the partition %s of an earlier partition table" % listed["path"])
        else:
            found.append("the device %s (%s) is on it" % (listed["path"], listed["type"]))
    return found, None

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

def read_partition_table(device):
    """The partition table that sfdisk --json reads from the device: (table, None), or (None, a failed result)."""
    out = run_command(["sfdisk", "--json", device])
    if out.error or out.exit_code != 0:
        return None, failed("sfdisk --json could not read the partition table of %s: %s" % (device, tool_message(out)))
    listing = json.decode(out.standard_output, default = None)
    table = listing.get("partitiontable") if type(listing) == "dict" else None
    if type(table) != "dict" or type(table.get("label")) != "string" or type(table.get("partitions", [])) != "list":
        return None, failed("sfdisk --json printed what MMX cannot read for %s: %s" % (device, one_line(out.standard_output)))
    if table["label"] == "gpt" and (type(table.get("sectorsize")) != "int" or type(table.get("lastlba")) != "int"):
        return None, failed("sfdisk --json printed no sector size and last usable sector for %s: %s" % (device, one_line(out.standard_output)))
    for partition in table.get("partitions", []):
        if type(partition) != "dict" or type(partition.get("start")) != "int" or type(partition.get("size")) != "int" or type(partition.get("type")) != "string":
            return None, failed("sfdisk --json printed what MMX cannot read for %s: %s" % (device, one_line(out.standard_output)))
    return table, None

def planned_layout(wanted, sector_bytes, last_usable_sector):
    """Where each wanted partition goes, in sectors: the first one starts at 1 MiB, and each next one follows directly. A rest_of_disk partition ends at the last whole MiB of the usable space. Returns (layout, None), or (None, why the partitions do not fit)."""
    sectors_in_one_mib = MIB // sector_bytes
    usable_bytes = max((last_usable_sector + 1) * sector_bytes - MIB, 0)
    layout = []
    start = sectors_in_one_mib
    for partition in wanted:
        if partition["size_bytes"] == None:
            end_after_whole_mib = ((last_usable_sector + 1) // sectors_in_one_mib) * sectors_in_one_mib
            sectors = end_after_whole_mib - start
            if sectors < sectors_in_one_mib:
                return None, "the partition %s with the size %s needs at least 1 MiB, and the partitions before it leave less: the disk has room for %d MiB of partitions" % (partition["name"], REST_OF_DISK, usable_bytes // MIB)
        else:
            sectors = partition["size_bytes"] // sector_bytes
        if start + sectors - 1 > last_usable_sector:
            return None, "the partition %s (%s) does not fit: the disk has room for %d MiB of partitions, and the partitions up to this one need %d MiB" % (
                partition["name"],
                size_text(partition["size_bytes"]),
                usable_bytes // MIB,
                ((start + sectors) * sector_bytes - MIB) // MIB,
            )
        layout.append({"name": partition["name"], "type": partition["type"], "type_guid": partition["type_guid"], "start": start, "sectors": sectors, "rest_of_disk": partition["size_bytes"] == None})
        start += sectors
    return layout, None

def type_name(type_guid):
    for name, guid in PARTITION_TYPE_GUIDS.items():
        if guid == type_guid.upper():
            return name
    return "type " + type_guid

def describe_partition(name, sectors, sector_bytes, type_guid, start):
    return "%s (%s, %s, from sector %d)" % (name or "a partition without a name", size_text(sectors * sector_bytes), type_name(type_guid), start)

def table_matches(table, layout):
    """Whether the table holds exactly the planned partitions: the same names, first sectors, sizes, and types, in the same order. A rest_of_disk partition may end anywhere in the last MiB before the table's last usable sector, because older and newer versions of sfdisk end such a partition at different sectors."""
    found = table.get("partitions", [])
    if len(found) != len(layout):
        return False
    sectors_in_one_mib = MIB // table["sectorsize"]
    for index, planned in enumerate(layout):
        partition = found[index]
        if partition.get("name", "") != planned["name"] or partition["type"].upper() != planned["type_guid"] or partition["start"] != planned["start"]:
            return False
        if planned["rest_of_disk"]:
            last_sector = partition["start"] + partition["size"] - 1
            if last_sector > table["lastlba"] or table["lastlba"] - last_sector >= sectors_in_one_mib:
                return False
        elif partition["size"] != planned["sectors"]:
            return False
    return True

def sfdisk_script(layout):
    """sfdisk's script with every sector written out, so that every version of sfdisk writes the same table and refuses one that does not fit."""
    lines = ["label: gpt"]
    for planned in layout:
        lines.append("start=%d, size=%d, name=%s, type=%s" % (planned["start"], planned["sectors"], planned["name"], planned["type_guid"]))
    return "\n".join(lines) + "\n"

def describe_layout(layout, sector_bytes):
    parts = []
    for planned in layout:
        size = size_text(planned["sectors"] * sector_bytes)
        parts.append("%s (%s%s)" % (planned["name"], size, ", the rest of the disk" if planned["rest_of_disk"] else ""))
    return ", ".join(parts)

def after_the_write(device, problem):
    """The result for a read that failed after the task wrote the partition table. It is not tried again: the next try would find the table and report a disk that needs nothing, without the note about an old filesystem inside a new partition."""
    if problem["status"] == "failed" and "try_again" not in problem:
        problem["reason"] += ". This was after the partition table was written on %s; the next run checks the disk again" % device
        problem["try_again"] = False
    problem["outputs"] = {"created": True}
    return problem

def partition_links_problem(device, disk, layout, wrote_the_table):
    """A result that fails the task when the link of a partition does not exist or leads to another disk, else None. The links appear only after the node's device manager has worked, so the module waits for it first; the next node task finds a partition by its link. After the task's own write the failure is not tried again; on a disk that the task found with its table, the node is in a state that no try changes."""
    found_the_table = "the partition table was written on %s" % device if wrote_the_table else "%s has the partition table that the task lists" % device
    fails = failed_once if wrote_the_table else refused
    settled = run_command(["udevadm", "settle"])
    for planned in layout:
        link = PARTITION_LINKS + planned["name"]
        partition, problem = read_block_device(link)
        if problem:
            return problem
        if partition == None:
            waited = "" if not settled.error and settled.exit_code == 0 else " (udevadm settle: %s)" % tool_message(settled)
            return fails("%s, but %s does not exist%s, so a later node task cannot find the partition. The kernel has not read the partition table: attach a loop device with losetup --partscan, and for another disk run partx --add %s" % (found_the_table, link, waited, device))
        if partition["kernel_name_of_its_disk"] != disk["kernel_name"]:
            return fails("%s, but %s leads to %s on the disk /dev/%s: two partitions of this node have the name %s, and a later node task would work on the wrong one. Give one of the two another name by hand" % (
                found_the_table,
                link,
                partition["path"],
                partition["kernel_name_of_its_disk"],
                planned["name"],
            ))
    return None

def run_node_task(inputs):
    device = inputs["device"]
    wanted, problem = wanted_partitions(inputs)
    if problem:
        return problem
    for program, package in PROGRAM_PACKAGES.items():
        found = find_program(program)
        if found.error:
            return failed("looking for the program %s: %s" % (program, found.error))
        if not found.found:
            return {"status": "missing_prerequisite", "reason": "%s: the program %s is not installed; install the package %s in an earlier node task" % (MODULE, program, package)}
    fingerprint = json.encode({"device": device, "partitions": [[partition["name"], partition["size_bytes"], partition["type"]] for partition in wanted]})

    # The module's own probe is the whole guard: sfdisk itself replaces a
    # partition table and writes over a filesystem that is not mounted.
    present = inspect_path(device)
    if present.error:
        return failed("looking at %s: %s" % (device, present.error))
    if not present.exists:
        return {"status": "missing_prerequisite", "reason": "%s: the device %s does not exist yet" % (MODULE, device)}
    disk, problem = read_block_device(device)
    if problem:
        return problem
    if disk == None:
        return refused(device + " is not a block device; give the whole disk, such as /dev/sdb")
    if disk["type"] == "part":
        return refused("%s is a partition of the disk /dev/%s, not a whole disk; the module writes a partition table only on a whole disk" % (device, disk["kernel_name_of_its_disk"]))
    if disk["type"] == "rom":
        return refused(device + " is a read-only medium, such as a CD drive; the module writes a partition table only on a disk")
    if disk["sector_bytes"] not in SECTORS_AFTER_THE_LAST_USABLE_ONE:
        return refused("%s has sectors of %d bytes; the module knows disks with sectors of 512 and of 4096 bytes" % (device, disk["sector_bytes"]))
    signatures, problem = read_signatures(device)
    if problem:
        return problem
    sector_bytes = disk["sector_bytes"]
    last_usable_sector = disk["size_bytes"] // sector_bytes - SECTORS_AFTER_THE_LAST_USABLE_ONE[sector_bytes] - 1

    if signatures:
        if [signature for signature in signatures if signature["type"] not in ("gpt", "PMBR")]:
            return refused("%s is not empty; found: %s. %s writes a partition table only on a disk that holds nothing, and never destroys data. To use this disk, first check that nothing on it is needed, then wipe it by hand, such as with wipefs --all %s" % (
                device,
                describe_signatures(signatures),
                MODULE,
                device,
            ))

        # A GPT partition table: unchanged when it holds exactly these
        # partitions, and a failure for every other table.
        table, problem = read_partition_table(device)
        if problem:
            return problem
        if table["label"] != "gpt":
            return refused("%s holds a partition table of the kind %s; the module never changes a disk that holds anything" % (device, table["label"]))
        layout, does_not_fit = planned_layout(wanted, table["sectorsize"], table["lastlba"])
        if does_not_fit == None and table_matches(table, layout):
            # A run that wrote the table and then found no link is followed
            # by a run that finds the table: the links are checked here too.
            problem = partition_links_problem(device, disk, layout, False)
            if problem:
                return problem
            return {"outputs": {"created": False}, "fingerprint": fingerprint}
        found = [describe_partition(partition.get("name", ""), partition["size"], table["sectorsize"], partition["type"], partition["start"]) for partition in table.get("partitions", [])]
        asked = does_not_fit or ", ".join([describe_partition(planned["name"], planned["sectors"], table["sectorsize"], planned["type_guid"], planned["start"]) for planned in layout])
        return refused("%s already has a GPT partition table with other partitions than the task lists. Found: %s. The task lists: %s. The module writes a partition table only on a disk that holds nothing, and never changes an existing partition table" % (
            device,
            ", ".join(found) or "no partition",
            asked,
        ))

    # A disk can be in use although it holds no signature: an encrypted
    # volume without a header, or any other device-mapper device on it. The
    # real sfdisk refuses such a disk, and sfdisk --no-act does not, so the
    # module looks itself and a dryrun fails as a run does.
    on_the_disk, problem = read_what_is_on_the_disk(device)
    if problem:
        return problem
    if on_the_disk:
        return refused("%s is in use, although it holds no signature: %s. The module writes a partition table only on a disk that nothing uses; when nothing uses a partition of an earlier partition table any more, blockdev --rereadpt %s makes the kernel forget it. Nothing was written" % (device, "; ".join(on_the_disk), device))

    layout, does_not_fit = planned_layout(wanted, sector_bytes, last_usable_sector)
    if does_not_fit:
        return refused("the partitions do not fit on %s (%d MiB): %s. Nothing was written" % (device, disk["size_bytes"] // MIB, does_not_fit))

    # Two partitions of a node with one name would share one link, and a
    # later node task would then work on the wrong partition.
    for planned in layout:
        other, problem = read_block_device(PARTITION_LINKS + planned["name"])
        if problem:
            return problem
        if other != None:
            return refused("the name %s is already the name of the partition %s on the disk /dev/%s: %s%s leads there, and two partitions with one name would share that link. Give the new partition another name. Nothing was written" % (
                planned["name"],
                other["path"],
                other["kernel_name_of_its_disk"],
                PARTITION_LINKS,
                planned["name"],
            ))

    # --wipe never and --wipe-partitions never: sfdisk wipes no signature.
    # --lock: sfdisk waits for a lock that another process holds, usually the
    # node's device manager.
    script = sfdisk_script(layout)
    sfdisk = ["sfdisk", "--wipe", "never", "--wipe-partitions", "never", "--lock"]
    if is_dryrun():
        # sfdisk --no-act goes through the same steps without writing, so a
        # table that sfdisk would refuse fails the dryrun as it fails a run.
        checked = run_command(sfdisk[:1] + ["--no-act"] + sfdisk[1:] + [device], standard_input = script)
        if checked.error or checked.exit_code != 0:
            return failed("sfdisk would not write the partition table on %s: %s" % (device, tool_message(checked)))
    written = run_command_that_changes_the_node(sfdisk + [device], standard_input = script)
    if written.error:
        return failed("writing the partition table on %s: %s" % (device, written.error))
    if not written.ran:
        return {"reason": "would create a GPT partition table on %s with the partitions: %s" % (device, describe_layout(layout, sector_bytes)), "outputs": {"created": True}, "fingerprint": fingerprint}
    if written.exit_code != 0:
        if SFDISK_DISK_IN_USE in written.standard_error:
            # sfdisk's own message goes on to name its force option, which
            # the module never passes.
            return failed("%s is in use, and sfdisk wrote nothing: the kernel has a filesystem of the disk mounted, uses it as swap, or another device holds it" % device)
        return failed("sfdisk did not write the partition table on %s: %s" % (device, tool_message(written)))

    table, problem = read_partition_table(device)
    if problem:
        return after_the_write(device, problem)
    if table["label"] != "gpt" or not table_matches(table, layout):
        return after_the_write(device, failed_once("sfdisk reported success, but the partition table of %s is not the one the task lists: %s" % (
            device,
            ", ".join([describe_partition(partition.get("name", ""), partition["size"], table.get("sectorsize", sector_bytes), partition["type"], partition["start"]) for partition in table.get("partitions", [])]) or "no partition",
        )))

    problem = partition_links_problem(device, disk, layout, True)
    if problem:
        return after_the_write(device, problem)

    # A disk whose partition table someone wiped can still hold an old
    # filesystem where a new partition begins. The module wipes nothing and
    # says what it found.
    notes = []
    for planned in layout:
        old, problem = read_signatures(PARTITION_LINKS + planned["name"])
        if problem:
            return after_the_write(device, problem)
        if old:
            notes.append("the new partition %s holds what was on the disk before: %s; the module wiped nothing" % (planned["name"], describe_signatures(old)))
    reason = "created a GPT partition table on %s with the partitions: %s" % (device, describe_layout(layout, sector_bytes))
    return {"reason": "; ".join([reason] + notes), "outputs": {"created": True}, "fingerprint": fingerprint}
