#!/usr/bin/env python3
"""Check a release archive's layout, version, and static Linux executables."""

import argparse
from pathlib import Path
import platform
import struct
import subprocess
import sys
import tarfile
import tempfile
import zlib

from release_archive import (CONTROLLER_PLATFORMS, RUNNERS, VERSION_FILE,
                             archive_directories, archive_files, validate_version)


def check_static_elf(program):
    """Parse ELF32/ELF64 program headers and the dynamic table, in either byte order."""
    if program[:4] != b"\x7fELF" or program[4:6] not in (b"\x01\x01", b"\x01\x02", b"\x02\x01", b"\x02\x02"):
        raise ValueError("The program has no supported ELF header.")
    byte_order = "<" if program[5] == 1 else ">"
    is_64_bit = program[4] == 2
    header_format = byte_order + ("HHIQQQIHHHHHH" if is_64_bit else "HHIIIIIHHHHHH")
    header = struct.unpack_from(header_format, program, 16)
    program_header_offset, program_header_size, program_header_count = header[4], header[8], header[9]
    entry_format = byte_order + ("IIQQQQQQ" if is_64_bit else "IIIIIIII")
    if program_header_size < struct.calcsize(entry_format) or not program_header_count:
        raise ValueError("The ELF program header table is missing or invalid.")
    for index in range(program_header_count):
        entry = struct.unpack_from(entry_format, program, program_header_offset + index * program_header_size)
        entry_type = entry[0]
        if entry_type == 3:  # PT_INTERP
            raise ValueError("The Linux program has an ELF interpreter (PT_INTERP).")
        if entry_type == 2:  # PT_DYNAMIC
            offset, size = (entry[2], entry[5]) if is_64_bit else (entry[1], entry[4])
            dynamic_format = byte_order + ("qQ" if is_64_bit else "iI")
            dynamic_size = struct.calcsize(dynamic_format)
            for position in range(offset, offset + size, dynamic_size):
                tag, _ = struct.unpack_from(dynamic_format, program, position)
                if tag == 0:  # DT_NULL
                    break
                if tag == 1:  # DT_NEEDED
                    raise ValueError("The Linux program needs a shared library (DT_NEEDED).")


def check_archive(path, version):
    failures = []
    validate_version(version)
    platform_matches = [(system, architecture) for system, architecture in CONTROLLER_PLATFORMS
                        if path.name == f"mmx-{version}-{system}-{architecture}.tar.gz"]
    if not platform_matches:
        return [f"Archive filename {path.name} does not name an expected controller platform and version."]
    system, architecture = platform_matches[0]
    top_directory = path.name[:-7]
    expected_files = set(archive_files())
    expected_entries = {top_directory} | {f"{top_directory}/{name}" for name in
                                         expected_files | archive_directories(expected_files)}
    with tarfile.open(path, "r:gz") as archive:
        members = archive.getmembers()
        actual_entries = {member.name for member in members}
        for name in sorted(expected_entries - actual_entries):
            failures.append(f"Archive is missing {name}.")
        for name in sorted(actual_entries - expected_entries):
            failures.append(f"Archive contains unexpected entry {name}.")
        if len(members) != len(actual_entries):
            failures.append("Archive contains duplicate entries.")
        payload = {}
        for member in members:
            relative = member.name.removeprefix(top_directory + "/")
            if relative in expected_files:
                if not member.isfile():
                    failures.append(f"Archive entry {member.name} must be a regular file.")
                else:
                    payload[relative] = archive.extractfile(member).read()
                    expected_mode = 0o755 if relative in ("mmx", *RUNNERS) else 0o644
                    if member.mode != expected_mode:
                        failures.append(f"Archive entry {member.name} has incorrect permissions.")
            elif member.name in expected_entries and not member.isdir():
                failures.append(f"Archive entry {member.name} must be a directory.")
    if payload.get(VERSION_FILE) != (version + "\n").encode():
        failures.append(f"Official extra modules version file must contain {version} followed by a newline.")
    for name in (*RUNNERS, *(("mmx",) if system == "linux" else ())):
        if name in payload:
            try:
                check_static_elf(payload[name])
            except (ValueError, struct.error) as error:
                failures.append(f"Linux program {name} failed its static ELF check: {error}")
    host_system = {"Darwin": "macos", "Linux": "linux"}.get(platform.system())
    host_architecture = {"arm64": "arm64", "aarch64": "arm64", "x86_64": "amd64", "AMD64": "amd64"}.get(platform.machine())
    if (system, architecture) == (host_system, host_architecture) and "mmx" in payload and not failures:
        with tempfile.TemporaryDirectory(prefix="mmx-release-check-") as directory:
            executable = Path(directory) / "mmx"
            executable.write_bytes(payload["mmx"])
            executable.chmod(0o755)
            result = subprocess.run([str(executable), "version"], capture_output=True, text=True)
            # The protocol number is independent of the release version.
            if result.returncode or not result.stdout.startswith(f"mmx {version} (protocol ") or not result.stdout.endswith(")\n"):
                failures.append(f"Running mmx version did not report MMX {version}: {result.stdout.strip()} {result.stderr.strip()}")
    return failures


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("archive", type=Path)
    parser.add_argument("--version", required=True)
    arguments = parser.parse_args()
    try:
        failures = check_archive(arguments.archive, arguments.version)
    except (OSError, EOFError, ValueError, tarfile.TarError, struct.error, zlib.error) as error:
        failures = [f"Archive check failed: {error}"]
    for failure in failures:
        print(failure, file=sys.stderr)
    if not failures:
        print(f"Release archive {arguments.archive.name} passed all checks.")
    return int(bool(failures))


if __name__ == "__main__":
    sys.exit(main())
