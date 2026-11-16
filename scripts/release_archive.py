"""The release archive layout, version scheme, and deterministic tar writer."""

import gzip
import io
from pathlib import Path
import re
import tarfile

REPOSITORY = Path(__file__).resolve().parents[1]
CONTROLLER_PLATFORMS = (("macos", "arm64"), ("macos", "amd64"),
                        ("linux", "amd64"), ("linux", "arm64"))
RUNNERS = ("mmx-runner-linux-amd64", "mmx-runner-linux-arm64")
VERSION_FILE = "official_extra_modules/shipped_with_mmx_version"
LICENSE_DIR = "dependency_license_notices"


def validate_version(version):
    """Accept SemVer, including prereleases and build metadata, without a v prefix."""
    number = r"(?:0|[1-9][0-9]*)"
    identifier = r"(?:0|[1-9][0-9]*|[0-9]*[A-Za-z-][0-9A-Za-z-]*)"
    pattern = (rf"{number}\.{number}\.{number}"
               rf"(?:-{identifier}(?:\.{identifier})*)?"
               r"(?:\+[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?")
    if not re.fullmatch(pattern, version):
        raise ValueError(f"Version {version!r} must be a semantic version without a v prefix.")
    return version


def development_version():
    source = (REPOSITORY / "internal/release/release.go").read_text()
    return re.search(r'const DefaultVersion = "([^"]+)"', source).group(1)


def archive_files(repository=REPOSITORY):
    """Map each payload path to its source; generated files have no source."""
    files = {"mmx": None, **{name: None for name in RUNNERS},
             "LICENSE": repository / "LICENSE",
             f"{LICENSE_DIR}/go/LICENSE": None}
    for path in sorted((repository / "official_extra_modules").rglob("*")):
        if path.is_file():
            files[path.relative_to(repository).as_posix()] = path
    for path in sorted((repository / "third_party").rglob("*")):
        if path.is_file() and path.name.startswith(("LICENSE", "NOTICE", "COPYING")):
            relative = path.relative_to(repository / "third_party").as_posix()
            files[f"{LICENSE_DIR}/{relative}"] = path
    return files


def archive_directories(files):
    return {parent.as_posix() for name in files for parent in Path(name).parents
            if parent != Path(".")}


def write_archive(destination, top_directory, payload, timestamp):
    """Write identical bytes independently of source modes, owners, and times."""
    entries = {top_directory: None}
    entries.update({f"{top_directory}/{name}": None
                    for name in archive_directories(payload)})
    entries.update({f"{top_directory}/{name}": content for name, content in payload.items()})
    with destination.open("wb") as output:
        # An empty gzip filename avoids recording the output path in the header.
        with gzip.GzipFile(filename="", mode="wb", fileobj=output, mtime=timestamp) as compressed:
            with tarfile.open(fileobj=compressed, mode="w", format=tarfile.USTAR_FORMAT) as archive:
                for name, content in sorted(entries.items()):
                    entry = tarfile.TarInfo(name)
                    entry.mtime = timestamp
                    entry.uid = entry.gid = 0
                    entry.uname = entry.gname = ""
                    if content is None:
                        entry.type = tarfile.DIRTYPE
                        entry.mode = 0o755
                    else:
                        entry.mode = 0o755 if name.rsplit("/", 1)[-1] in ("mmx", *RUNNERS) else 0o644
                        entry.size = len(content)
                    archive.addfile(entry, io.BytesIO(content) if content is not None else None)
