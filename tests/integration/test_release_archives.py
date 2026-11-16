"""Fast release checks; the full cross-build is a separate artifact proof."""

import importlib.util
from pathlib import Path
import struct
import subprocess
import sys
import tarfile
import tempfile
import unittest
from unittest.mock import patch

SCRIPTS = Path(__file__).resolve().parents[2] / "scripts"
sys.path.insert(0, str(SCRIPTS))
import release_archive


def load_script(name):
    specification = importlib.util.spec_from_file_location(name, SCRIPTS / (name + ".py"))
    module = importlib.util.module_from_spec(specification)
    specification.loader.exec_module(module)
    return module


checker = load_script("check-release-archive")
builder = load_script("build-release-archives")


def elf_fixture(entry_type=1, dynamic_tag=0, bits=64, byte_order="<"):
    is_64_bit = bits == 64
    header_format = byte_order + ("HHIQQQIHHHHHH" if is_64_bit else "HHIIIIIHHHHHH")
    program_format = byte_order + ("IIQQQQQQ" if is_64_bit else "IIIIIIII")
    header_size = 16 + struct.calcsize(header_format)
    program_size = struct.calcsize(program_format)
    dynamic_format = byte_order + ("qQ" if is_64_bit else "iI")
    dynamic = struct.pack(dynamic_format, dynamic_tag, 0)
    offset = header_size + program_size
    identity = b"\x7fELF" + bytes([2 if is_64_bit else 1, 1 if byte_order == "<" else 2, 1]) + bytes(9)
    header = struct.pack(header_format, 2, 62, 1, 0, header_size, 0, 0, header_size, program_size, 1, 0, 0, 0)
    fields = (entry_type, 0, offset, 0, 0, len(dynamic), len(dynamic), 8) if is_64_bit else (
        entry_type, offset, 0, 0, len(dynamic), len(dynamic), 0, 4)
    return identity + header + struct.pack(program_format, *fields) + dynamic


class StaticELFTests(unittest.TestCase):
    def test_static_program_passes_and_dynamic_programs_fail(self):
        for bits in (32, 64):
            for byte_order in ("<", ">"):
                with self.subTest(bits=bits, byte_order=byte_order):
                    checker.check_static_elf(elf_fixture(bits=bits, byte_order=byte_order))
                    checker.check_static_elf(elf_fixture(2, 0, bits, byte_order))
                    with self.assertRaisesRegex(ValueError, "PT_INTERP"):
                        checker.check_static_elf(elf_fixture(3, bits=bits, byte_order=byte_order))
                    with self.assertRaisesRegex(ValueError, "DT_NEEDED"):
                        checker.check_static_elf(elf_fixture(2, 1, bits, byte_order))

    def test_non_elf_and_truncated_programs_fail(self):
        for program in (b"not ELF", elf_fixture()[:20]):
            with self.assertRaises((ValueError, struct.error)):
                checker.check_static_elf(program)


class ArchiveTests(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory()
        self.addCleanup(self.temporary.cleanup)
        self.directory = Path(self.temporary.name)
        self.top_directory = "mmx-0.1.0-linux-amd64"
        self.archive = self.directory / (self.top_directory + ".tar.gz")
        self.payload = {name: b"example\n" for name in release_archive.archive_files()}
        self.payload[release_archive.VERSION_FILE] = b"0.1.0\n"
        for name in ("mmx", *release_archive.RUNNERS):
            self.payload[name] = elf_fixture()
        self.host = patch.object(checker.platform, "system", return_value="Other")
        self.host.start()
        self.addCleanup(self.host.stop)

    def write(self):
        release_archive.write_archive(self.archive, self.top_directory, self.payload, 1234567890)

    def test_expected_archive_passes(self):
        self.write()
        self.assertEqual(checker.check_archive(self.archive, "0.1.0"), [])

    def test_extra_and_missing_files_are_rejected(self):
        self.payload["surprise.txt"] = b"unexpected"
        del self.payload["LICENSE"]
        self.write()
        failures = checker.check_archive(self.archive, "0.1.0")
        self.assertTrue(any("unexpected entry" in failure and "surprise.txt" in failure for failure in failures))
        self.assertTrue(any("missing" in failure and "LICENSE" in failure for failure in failures))

    def test_invalid_expected_version_exits_one_with_a_plain_reason(self):
        result = subprocess.run([sys.executable, str(SCRIPTS / "check-release-archive.py"),
                                 str(self.archive), "--version", "invalid"],
                                text=True, capture_output=True)
        self.assertEqual(result.returncode, 1)
        self.assertIn("must be a semantic version", result.stderr)
        self.assertNotIn("Traceback", result.stderr)

    def test_wrong_version_and_dynamic_runner_are_rejected(self):
        self.payload[release_archive.VERSION_FILE] = b"0.1.0-dev\n"
        self.payload[release_archive.RUNNERS[0]] = elf_fixture(3)
        self.write()
        failures = checker.check_archive(self.archive, "0.1.0")
        self.assertTrue(any("version file" in failure for failure in failures))
        self.assertTrue(any("PT_INTERP" in failure for failure in failures))

    def test_archive_bytes_ignore_payload_order_and_destination(self):
        self.write()
        second = self.directory / "second.tar.gz"
        release_archive.write_archive(second, self.top_directory, dict(reversed(list(self.payload.items()))), 1234567890)
        self.assertEqual(self.archive.read_bytes(), second.read_bytes())
        with tarfile.open(self.archive) as archive:
            names = archive.getnames()
            self.assertEqual(names, sorted(names))
            for entry in archive.getmembers():
                self.assertEqual((entry.mtime, entry.uid, entry.gid, entry.uname, entry.gname),
                                 (1234567890, 0, 0, "", ""))

    def test_same_version_output_is_refused_before_building(self):
        self.write()
        with self.assertRaisesRegex(ValueError, "already holds archives"):
            builder.build_archives("0.1.0", self.directory)

    def test_dependency_notices_include_nested_copies(self):
        repository = self.directory
        nested = repository / "third_party/example.org/library/internal/LICENSE.txt"
        nested.parent.mkdir(parents=True)
        nested.write_text("copyright")
        (nested.parent / "COPYING.extra").write_text("copyright")
        (nested.parent / "NOTICE").write_text("notice")
        files = release_archive.archive_files(repository)
        for name in ("LICENSE.txt", "COPYING.extra", "NOTICE"):
            self.assertIn(f"dependency_license_notices/example.org/library/internal/{name}", files)
        self.assertIn("dependency_license_notices/go/LICENSE", files)


class VersionTests(unittest.TestCase):
    def test_semantic_versions(self):
        for version in ("0.1.0", "0.1.0-dev", "0.1.0-rc.1", "1.0.0+build.3", "1.0.0-1a"):
            self.assertEqual(release_archive.validate_version(version), version)
        for version in ("v0.1.0", "0.1", "01.1.0", "0.1.0-01", "0.1.0-", "../0.1.0", "0.1.0\n"):
            with self.assertRaises(ValueError):
                release_archive.validate_version(version)


if __name__ == "__main__":
    unittest.main()
