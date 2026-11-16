# grow_filesystem_to_fill_device grows a mounted ext4 or XFS filesystem.
# docs/changing-mmx/design/modules-official-extra.md owns the behavior
# ("Growing a filesystem to fill its device").

MODULE = "grow_filesystem_to_fill_device"
SIZE_UNITS = {"TiB": 1024 * 1024 * 1024 * 1024, "GiB": 1024 * 1024 * 1024, "MiB": 1024 * 1024, "KiB": 1024}
PROGRAM_PACKAGES = {"blockdev": "util-linux", "findmnt": "util-linux", "lsblk": "util-linux", "wipefs": "util-linux"}
FILESYSTEM_PROGRAM_PACKAGES = {"ext4": {"dumpe2fs": "e2fsprogs", "resize2fs": "e2fsprogs"}, "xfs": {"xfs_info": "xfsprogs", "xfs_growfs": "xfsprogs"}}

def failed(message, try_again = True):
    return {"status": "failed", "reason": MODULE + ": " + message, "try_again": try_again}

def prerequisite(message):
    return {"status": "missing_prerequisite", "reason": MODULE + ": " + message}

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

def read_command(arguments):
    out = run_command(arguments)
    if out.error or out.exit_code != 0:
        return None, failed("reading with %s: %s" % (" ".join(arguments), tool_message(out)))
    return out.standard_output, None

def unreadable(program, device, text):
    return failed("%s printed what MMX cannot read for %s: %s" % (program, device, one_line(text)))

def mounted_filesystem(device, filesystem_type):
    text, problem = read_command(["lsblk", "--json", "--nodeps", "-o", "PATH,MAJ:MIN", device])
    if problem:
        return None, problem
    listing = json.decode(text, default = None)
    devices = listing.get("blockdevices") if type(listing) == "dict" else None
    if type(devices) != "list" or len(devices) != 1 or type(devices[0]) != "dict" or type(devices[0].get("maj:min")) != "string":
        return None, unreadable("lsblk", device, text)
    numbers = devices[0]["maj:min"]
    text, problem = read_command(["wipefs", "--no-act", "--json", device])
    if problem:
        return None, problem
    listing = json.decode(text, default = None)
    signatures = listing.get("signatures") if type(listing) == "dict" else None
    if type(signatures) != "list" or any([type(row) != "dict" or type(row.get("type")) != "string" for row in signatures]):
        return None, unreadable("wipefs", device, text)
    found = sorted({row["type"]: True for row in signatures}.keys())
    if found != [filesystem_type]:
        return None, prerequisite("the device %s holds %s, not the expected %s filesystem; create the expected filesystem in an earlier node task with create_filesystem_on_empty_device on an empty device" % (device, ", ".join(found) or "no filesystem signature", filesystem_type))
    text, problem = read_command(["findmnt", "--json", "--list", "--output", "TARGET,FSTYPE,OPTIONS,MAJ:MIN"])
    if problem:
        return None, problem
    listing = json.decode(text, default = None)
    mounts = listing.get("filesystems") if type(listing) == "dict" else None
    if type(mounts) != "list":
        return None, unreadable("findmnt", device, text)
    matching = []
    for mount in mounts:
        if type(mount) != "dict" or type(mount.get("maj:min")) != "string":
            return None, unreadable("findmnt", device, text)
        if mount["maj:min"] != numbers:
            continue
        if any([type(mount.get(key)) != "string" for key in ("target", "fstype", "options")]) or not mount["target"].startswith("/"):
            return None, unreadable("findmnt", device, text)
        if mount["fstype"] != filesystem_type:
            return None, prerequisite("the mount %s of %s has type %s, not %s; mount the expected filesystem first with fstab_mount" % (mount["target"], device, mount["fstype"], filesystem_type))
        matching.append(mount)
    for mount in matching:
        options = mount["options"].split(",")
        if "rw" in options and "ro" not in options:
            return mount["target"], None
    if matching:
        return None, prerequisite("the %s filesystem on %s is mounted read-only; mount it for writing first with fstab_mount" % (filesystem_type, device))
    return None, prerequisite("the %s filesystem on %s is not mounted; mount it for writing first with fstab_mount" % (filesystem_type, device))

def filesystem_geometry(device, filesystem_type, mount_point):
    program = "dumpe2fs" if filesystem_type == "ext4" else "xfs_info"
    arguments = [program, "-h", device] if filesystem_type == "ext4" else [program, mount_point]
    text, problem = read_command(arguments)
    if problem:
        return None, problem
    counts = []
    sizes = []
    for line in text.splitlines():
        if filesystem_type == "ext4":
            if line.startswith("Block count:"):
                counts.append(line.split(":", 1)[1].strip())
            if line.startswith("Block size:"):
                sizes.append(line.split(":", 1)[1].strip())
        elif line.strip().startswith("data"):
            words = line.replace("=", " = ", 1).split()
            if len(words) < 4 or words[:2] != ["data", "="]:
                return None, unreadable(program, device, text)
            for word in words[2:]:
                if word.startswith("bsize="):
                    sizes.append(word[6:].rstrip(","))
                elif word.startswith("blocks="):
                    counts.append(word[7:].rstrip(","))
    if len(counts) != 1 or len(sizes) != 1 or not counts[0].isdigit() or not sizes[0].isdigit() or int(counts[0]) == 0 or int(sizes[0]) == 0:
        return None, unreadable(program, device, text)
    return {"blocks": int(counts[0]), "block_bytes": int(sizes[0])}, None

def run_node_task(inputs):
    device = inputs["device"]
    filesystem_type = inputs["filesystem_type"]
    if device == "":
        return {"status": "invalid_inputs", "reason": MODULE + ".device: must not be empty"}
    if not device.startswith("/"):
        return {"status": "invalid_inputs", "reason": MODULE + ".device: expected the absolute path of a block device"}
    programs = dict(PROGRAM_PACKAGES)
    programs.update(FILESYSTEM_PROGRAM_PACKAGES[filesystem_type])
    for program, package in programs.items():
        found = find_program(program)
        if found.error:
            return failed("looking for the program %s: %s" % (program, found.error))
        if not found.found:
            return prerequisite("the program %s is not installed; install the package %s in an earlier node task" % (program, package))
    present = inspect_path(device)
    if present.error:
        return failed("reading the device %s: %s" % (device, present.error))
    if not present.exists:
        return prerequisite("the device %s does not exist yet" % device)
    mount_point, problem = mounted_filesystem(device, filesystem_type)
    if problem:
        return problem
    text, problem = read_command(["blockdev", "--getsize64", device])
    if problem:
        return problem
    if not text.strip().isdigit() or int(text.strip()) == 0:
        return unreadable("blockdev", device, text)
    geometry, problem = filesystem_geometry(device, filesystem_type, mount_point)
    if problem:
        return problem
    old_bytes = geometry["blocks"] * geometry["block_bytes"]
    wanted_blocks = int(text.strip()) // geometry["block_bytes"]
    wanted_bytes = wanted_blocks * geometry["block_bytes"]
    result = {"fingerprint": json.encode({"device": device, "type": filesystem_type, "size_bytes": wanted_bytes})}
    if wanted_blocks <= geometry["blocks"]:
        return result
    what = "the %s filesystem on %s from %s to %s" % (filesystem_type, device, size_text(old_bytes), size_text(wanted_bytes))
    arguments = ["resize2fs", device] if filesystem_type == "ext4" else ["xfs_growfs", mount_point]
    out = run_command_that_changes_the_node(arguments)
    if not out.error and not out.ran:
        result["reason"] = "would grow " + what
        return result
    after, problem = filesystem_geometry(device, filesystem_type, mount_point)
    if problem:
        # Even a failed grow can have changed the filesystem. Preserve the
        # command's words when the read cannot determine what happened.
        if out.error or out.exit_code != 0:
            return failed("growing %s: %s; %s" % (what, tool_message(out), problem["reason"]), False)
        problem["try_again"] = False
        return problem
    new_bytes = after["blocks"] * after["block_bytes"]
    if out.error or out.exit_code != 0:
        return failed("growing %s: %s. The filesystem now has %s" % (what, tool_message(out), size_text(new_bytes)), new_bytes == old_bytes)
    if new_bytes <= old_bytes:
        return failed("growing %s: %s. The filesystem is no larger (%s)" % (what, (one_line(out.standard_error + "\n" + out.standard_output) or tool_message(out)), size_text(new_bytes)))
    result["reason"] = "grew the %s filesystem on %s from %s to %s" % (filesystem_type, device, size_text(old_bytes), size_text(new_bytes))
    return result
