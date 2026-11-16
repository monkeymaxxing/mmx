# zypper_repository adds or removes one software repository on an openSUSE
# node: its file in /etc/zypp/repos.d and its signing key, which the task
# gives explicitly. docs/changing-mmx/design/modules-packages.md owns the behavior
# ("zypper's own rules"). A zypper_package task after this one refreshes
# the repositories when it has work, so it sees the new repository.

MODULE = "zypper_repository"

REPOSITORIES_DIRECTORY = "/etc/zypp/repos.d"

# The signing key of each repository, as the file that the repository file
# names, and the home of the gpg that reads the keys. zypper_repository
# imports a key into rpm's keyring by itself, so zypper never has to ask
# whether to trust a key, and a non-interactive zypper would refuse it.
KEY_DIRECTORY = "/etc/zypp/mmx_repository_keys"
GNUPG_HOME = KEY_DIRECTORY + "/gnupg_home"

# The ids of the keys that the module imported into rpm for a repository, one
# on a line, in KEY_DIRECTORY/NAME.imported_key_ids. Only these leave rpm
# again, and only when no other repository's key file holds them.
RECORD_SUFFIX = ".imported_key_ids"

# The owner of the files and directories that the module writes.
FILE_OWNER = "root"
FILE_GROUP = "root"

NAME_CHARACTERS = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_-."
ARMOR_BEGIN = "-----BEGIN PGP PUBLIC KEY BLOCK-----"

def invalid(field, rule):
    """Returns the result for an input that breaks a rule."""
    return {"status": "invalid_inputs", "reason": MODULE + "." + field + ": " + rule}

def check_inputs(inputs):
    """Returns the result for inputs that break a rule that the description
    cannot state, or None."""
    name = inputs["name"]
    if name == "":
        return invalid("name", "must not be empty")
    if name.startswith("-") or name.startswith("."):
        return invalid("name", "must not start with a hyphen or a dot")
    for character in name.elems():
        if character not in NAME_CHARACTERS:
            return invalid("name", "may hold only letters, digits, and _ - .")
    if inputs.get("state", "present") == "absent":
        for other in inputs:
            if other not in ["name", "state"]:
                return invalid("state", "absent accepts only name and state; remove " + other)
        return None
    for required in ["url", "repository_metadata_is_signed"]:
        if required not in inputs:
            return invalid(required, "is required with state present")
    url = inputs["url"]
    if not (url.startswith("https://") or url.startswith("http://")):
        return invalid("url", "must start with https:// or http://")
    for character in url.elems():
        if character in " \t\r\n\\":
            return invalid("url", "must not hold a space, a line break, or a backslash")
    # With metadata that is not signed, nothing in the repository ties the
    # list of packages to the vendor: someone on the network who can change an
    # http:// answer could serve an old list, with old, correctly signed
    # packages, and keep the node from security updates. https:// stops that.
    if url.startswith("http://") and not inputs["repository_metadata_is_signed"]:
        return invalid("url", "must start with https:// when repository_metadata_is_signed is false: nothing then stops a change of the list of packages on the way, such as an old list that keeps back updates")
    sources = [key for key in ["signing_key_text", "signing_key_url"] if key in inputs]
    if len(sources) != 1:
        return invalid("signing_key_text", "exactly one of signing_key_text or signing_key_url is required with state present")
    if "signing_key_url" in inputs:
        if not inputs["signing_key_url"].startswith("https://"):
            return invalid("signing_key_url", "must start with https://")
        if ("signing_key_sha256" in inputs) == bool(inputs.get("dangerously_skip_checksum", False)):
            return invalid("signing_key_sha256", "exactly one of signing_key_sha256 or dangerously_skip_checksum: true is required with signing_key_url")
    else:
        for other in ["signing_key_sha256", "dangerously_skip_checksum"]:
            if other in inputs:
                return invalid(other, "is for signing_key_url; remove it, or use signing_key_url")
    if "signing_key_sha256" in inputs and len(inputs["signing_key_sha256"]) != 64:
        return invalid("signing_key_sha256", "must be 64 hexadecimal characters")
    return None

def signing_key_text(inputs):
    """Returns the key as text, and None; or None and the failed result. A key
    from a web address is read on the node, and its SHA-256 must be the one
    in the task."""
    if "signing_key_text" in inputs:
        text = inputs["signing_key_text"]
    else:
        answer = http_request("GET", inputs["signing_key_url"])
        if answer.error:
            return None, {"status": "failed", "reason": MODULE + ": fetching the signing key: " + answer.error}
        if answer.http_status_code != 200:
            return None, {"status": "failed", "reason": MODULE + ": fetching the signing key: the web address answered with HTTP status " + str(answer.http_status_code)}
        text = answer.body
        if "signing_key_sha256" in inputs and sha256(text) != inputs["signing_key_sha256"].lower():
            return None, invalid("signing_key_sha256", "is not the SHA-256 of the key at signing_key_url, which is " + sha256(text))
    if ARMOR_BEGIN not in text:
        return None, invalid("signing_key_text" if "signing_key_text" in inputs else "signing_key_url", "is not an ASCII-armored OpenPGP public key, which starts with " + ARMOR_BEGIN)
    return text if text.endswith("\n") else text + "\n", None

def repository_file_text(inputs):
    """Returns the repository file. Every setting is written out, so that
    zypper's defaults decide nothing: package signatures are always checked;
    the signature of the repository's metadata is checked when the task
    says that the metadata is signed."""
    name = inputs["name"]
    return "\n".join([
        "# Written by MMX (zypper_repository). MMX overwrites changes.",
        "[" + name + "]",
        "name=" + name,
        "enabled=1",
        "autorefresh=1",
        "baseurl=" + inputs["url"],
        "type=rpm-md",
        "gpgcheck=1",
        "repo_gpgcheck=" + ("1" if inputs["repository_metadata_is_signed"] else "0"),
        "pkg_gpgcheck=1",
        "gpgkey=file://" + KEY_DIRECTORY + "/" + name + ".asc",
        "",
    ])

def key_ids(text):
    """Returns the ids of the keys in text, as rpm names them (the last eight
    hexadecimal digits of the fingerprint, in lower case), and an empty text;
    or None and what went wrong. gpg works in its own home, in
    KEY_DIRECTORY."""
    answer = run_command(["gpg", "--batch", "--no-tty", "--homedir", GNUPG_HOME, "--show-keys", "--with-colons"], standard_input = text)
    if answer.error or answer.exit_code != 0:
        return None, answer.error or answer.standard_error.strip()[:300]
    ids = []
    after_public_key = False
    for line in answer.standard_output.splitlines():
        fields = line.split(":")
        if fields[0] == "pub":
            after_public_key = True
        elif fields[0] == "fpr" and after_public_key and len(fields) > 9:
            ids.append(fields[9][-8:].lower())
            after_public_key = False
        else:
            after_public_key = False
    if not ids:
        return None, "it holds no OpenPGP public key"
    return ids, ""

def key_package(key):
    """Returns the name of the package in which rpm holds the key with this
    short id, or an empty text. Before rpm 6 the package's version is the short
    id. rpm 6 makes it the key's whole fingerprint, which holds the short id (at
    its end for a version 4 key, inside it for a version 6 key)."""
    answer = run_command(["rpm", "-q", "gpg-pubkey-" + key])
    if not answer.error and answer.exit_code == 0:
        return "gpg-pubkey-" + key
    listed = run_command(["rpm", "-q", "--qf", "%{VERSION}\\n", "gpg-pubkey"])
    if listed.error or listed.exit_code != 0:
        return ""
    for version in listed.standard_output.lower().split():
        if key in version:
            return "gpg-pubkey-" + version
    return ""

def key_is_imported(ids):
    """Returns whether rpm's keyring holds every key."""
    for key in ids:
        if key_package(key) == "":
            return False
    return True

def failed(doing, result):
    return {"status": "failed", "reason": MODULE + ": " + doing + ": " + result.error}

def files_of_other_repositories(name, suffix):
    """Returns the paths of the files in KEY_DIRECTORY with this suffix, other
    than the one of the repository NAME, and an empty text; or None and what
    went wrong. find prints one path on a line."""
    if not inspect_path(KEY_DIRECTORY).exists:
        return [], ""
    answer = run_command(["find", KEY_DIRECTORY, "-maxdepth", "1", "-type", "f", "-name", "*" + suffix])
    if answer.error or answer.exit_code != 0:
        return None, answer.error or answer.standard_error.strip()[:300]
    own = KEY_DIRECTORY + "/" + name + suffix
    return [line for line in answer.standard_output.splitlines() if line and line != own], ""

def ids_held_by_other_repositories(name):
    """Returns the key ids in the key files of the other repositories, and the
    ids that they recorded as imported, and an empty text; or None, None and
    what went wrong. A key that another repository's file holds stays in rpm
    when this repository goes."""
    held, recorded = [], []
    if not inspect_path(GNUPG_HOME).exists:
        return held, recorded, ""
    paths, problem = files_of_other_repositories(name, ".asc")
    if problem:
        return None, None, problem
    for path in paths:
        read = read_file(path)
        if read.error:
            return None, None, read.error
        ids, problem = key_ids(read.content)
        if problem:
            return None, None, path + ": " + problem
        held += ids
    paths, problem = files_of_other_repositories(name, RECORD_SUFFIX)
    if problem:
        return None, None, problem
    for path in paths:
        read = read_file(path)
        if read.error:
            return None, None, read.error
        recorded += read.content.split()
    return held, recorded, ""

def read_record(name):
    """Returns the key ids that this module imported for the repository NAME,
    and an empty text; or None and what went wrong."""
    path = KEY_DIRECTORY + "/" + name + RECORD_SUFFIX
    if not inspect_path(path).exists:
        return [], ""
    read = read_file(path)
    if read.error:
        return None, read.error
    return read.content.split(), ""

def remove_keys_from_rpm(key_ids_to_remove):
    """Removes the keys from rpm's keyring. Returns the parts of the reason,
    and None; or None and the failed result."""
    parts = []
    for key_id in key_ids_to_remove:
        package = key_package(key_id)
        if package == "":
            continue
        removed = run_command_that_changes_the_node(["rpm", "-e", package])
        if removed.error or removed.exit_code != 0:
            return None, {"status": "failed", "reason": MODULE + ": removing the signing key " + key_id + " from rpm: " + (removed.error or removed.standard_error.strip()[:300])}
        parts.append(("would remove" if is_dryrun() else "removed") + " the signing key " + key_id)
    return parts, None

def present(inputs):
    name = inputs["name"]
    key_path = KEY_DIRECTORY + "/" + name + ".asc"
    record_path = KEY_DIRECTORY + "/" + name + RECORD_SUFFIX
    repository_path = REPOSITORIES_DIRECTORY + "/" + name + ".repo"
    text, failure = signing_key_text(inputs)
    if failure:
        return failure
    wanted = repository_file_text(inputs)
    existing = inspect_path(repository_path)
    if existing.error:
        return failed("looking at " + repository_path, existing)
    created = not existing.exists
    parts = []
    # The directories first: gpg needs its home to read the key's ids.
    for path, mode in [(KEY_DIRECTORY, "0755"), (GNUPG_HOME, "0700")]:
        if is_dryrun() and not inspect_path(path.rsplit("/", 1)[0]).exists:
            # A dryrun creates no parent directory, so it cannot make the
            # directory inside; a run makes both.
            continue
        made = make_directory(path, owner = FILE_OWNER, group = FILE_GROUP, permissions_mode = mode)
        if made.error:
            return failed("making the directory " + path, made)
    ids = None
    recorded, problem = read_record(name)
    if problem:
        return {"status": "failed", "reason": MODULE + ": reading the record " + record_path + ": " + problem}
    if not is_dryrun() or inspect_path(GNUPG_HOME).exists:
        ids, problem = key_ids(text)
        if problem:
            return invalid("signing_key_text" if "signing_key_text" in inputs else "signing_key_url", "is not usable: " + problem)
    # A dryrun cannot write into a directory that it would only create.
    can_write = not (is_dryrun() and not inspect_path(KEY_DIRECTORY).exists)
    if can_write:
        written = write_file(key_path, text, owner = FILE_OWNER, group = FILE_GROUP)
        if written.error:
            return failed("writing the signing key " + key_path, written)
    # The ids this module imported for the repository: those that rpm did not
    # hold before, those it imported earlier, and those that another
    # repository of this module also recorded, so that the last of them to go
    # removes the key. A key that rpm held before this module (the
    # distribution's own) is never recorded and never removed.
    new_record = recorded
    rotated_out = []
    if ids != None:
        others_held, others_recorded, problem = ids_held_by_other_repositories(name)
        if problem:
            return {"status": "failed", "reason": MODULE + ": reading the keys of the other repositories: " + problem}
        new_record = [key_id for key_id in ids if key_id in recorded or key_id in others_recorded or not key_is_imported([key_id])]
        rotated_out = [key_id for key_id in recorded if key_id not in ids]
    if ids == None or not key_is_imported(ids):
        imported = run_command_that_changes_the_node(["rpm", "--import", key_path])
        if imported.error or imported.exit_code != 0:
            return {"status": "failed", "reason": MODULE + ": importing the signing key into rpm: " + (imported.error or imported.standard_error.strip()[:300])}
        parts.append(("would import" if is_dryrun() else "imported") + " the signing key " + (", ".join(ids) if ids else "for the repository " + name))
    written = write_file(repository_path, wanted, owner = FILE_OWNER, group = FILE_GROUP)
    if written.error:
        return failed("writing " + repository_path, written)
    if written.changed:
        parts.append(("would " + ("add" if created else "change") if is_dryrun() else ("added" if created else "changed")) + " the repository " + name + " at " + inputs["url"])
    if ids != None and can_write:
        # The record keeps the old ids until they are removed, so that a run
        # that stopped between the two steps removes them at the next run.
        record = write_file(record_path, "".join([key_id + "\n" for key_id in new_record + rotated_out]), owner = FILE_OWNER, group = FILE_GROUP)
        if record.error:
            return failed("writing " + record_path, record)
        # A key of the old file that the task no longer gives goes from rpm,
        # unless another repository's file holds it.
        removable = [key_id for key_id in rotated_out if key_id not in others_held and key_is_imported([key_id])]
        removed_parts, failure = remove_keys_from_rpm(removable)
        if failure:
            return failure
        parts += [part + " (the key that the task replaced)" for part in removed_parts]
        if rotated_out:
            record = write_file(record_path, "".join([key_id + "\n" for key_id in new_record]), owner = FILE_OWNER, group = FILE_GROUP)
            if record.error:
                return failed("writing " + record_path, record)
    result = {
        "outputs": {"created": created and bool(written.changed)},
        "fingerprint": json.encode({
            "name": name,
            "url": inputs["url"],
            "repository_metadata_is_signed": inputs["repository_metadata_is_signed"],
            "signing_key_sha256": sha256(text),
        }),
    }
    if parts:
        result["reason"] = "; ".join(parts)
    return result

def absent(inputs):
    name = inputs["name"]
    key_path = KEY_DIRECTORY + "/" + name + ".asc"
    record_path = KEY_DIRECTORY + "/" + name + RECORD_SUFFIX
    repository_path = REPOSITORIES_DIRECTORY + "/" + name + ".repo"
    repository = inspect_path(repository_path)
    key = inspect_path(key_path)
    record = inspect_path(record_path)
    if repository.error or key.error or record.error:
        return failed("looking at the files of the repository " + name, repository if repository.error else key if key.error else record)
    # Only the keys that this module imported for the repository go, and not
    # a key that another repository's file holds.
    recorded, problem = read_record(name)
    if problem:
        return {"status": "failed", "reason": MODULE + ": reading the record " + record_path + ": " + problem}
    others_held, _, problem = ids_held_by_other_repositories(name)
    if problem:
        return {"status": "failed", "reason": MODULE + ": reading the keys of the other repositories: " + problem}
    removable = [key_id for key_id in recorded if key_id not in others_held and key_is_imported([key_id])]
    parts = []
    # The repository goes first, so that no package comes from it while its
    # key is gone. zypper removerepo also removes the repository's cache.
    if repository.exists:
        removed = run_command_that_changes_the_node(["env", "ZYPP_LOCK_TIMEOUT=60", find_program("zypper").path, "--non-interactive", "removerepo", name])
        if removed.error or removed.exit_code != 0:
            return {"status": "failed", "reason": MODULE + ": removing the repository " + name + ": " + (removed.error or (removed.standard_output + removed.standard_error).strip()[-300:])}
        parts.append(("would remove" if is_dryrun() else "removed") + " the repository " + name)
    removed_parts, failure = remove_keys_from_rpm(removable)
    if failure:
        return failure
    parts += removed_parts
    for path, status in [(key_path, key), (record_path, record)]:
        if status.exists:
            removed = remove_path(path)
            if removed.error:
                return failed("removing " + path, removed)
    result = {"outputs": {"created": False}, "fingerprint": json.encode({"name": name, "state": "absent"})}
    if parts:
        result["reason"] = "; ".join(parts)
    return result

def run_node_task(inputs):
    failure = check_inputs(inputs)
    if failure:
        return failure
    state = inputs.get("state", "present")
    for program, package in [("zypper", "zypper"), ("rpm", "rpm"), ("gpg", "gpg2"), ("find", "findutils")]:
        location = find_program(program)
        if location.error:
            return {"status": "failed", "reason": MODULE + ": looking for the program " + program + ": " + location.error}
        if not location.found:
            if program == "zypper":
                return {
                    "status": "missing_prerequisite",
                    "reason": MODULE + ": the program zypper is not installed; zypper_repository works only on a node whose package manager is zypper, " +
                              "such as openSUSE; give the node task a condition on the node fact package_manager",
                }
            return {"status": "missing_prerequisite", "reason": MODULE + ": the program " + program + " is not installed; install the package " + package + " in an earlier node task"}
    if state == "absent":
        return absent(inputs)
    return present(inputs)
