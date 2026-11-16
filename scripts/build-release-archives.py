#!/usr/bin/env python3
"""Build and check the four reproducible MMX controller release archives."""

import argparse
import hashlib
import os
from pathlib import Path
import subprocess
import sys
import tempfile

from release_archive import (CONTROLLER_PLATFORMS, LICENSE_DIR, REPOSITORY,
                             RUNNERS, VERSION_FILE, archive_files,
                             validate_version, write_archive)


def build_archives(version, output_dir):
    validate_version(version)
    output_dir = output_dir.resolve()
    output_dir.mkdir(parents=True, exist_ok=True)
    if any((output_dir / f"mmx-{version}-{system}-{architecture}.tar.gz").exists()
           for system, architecture in CONTROLLER_PLATFORMS):
        raise ValueError(f"Output directory {output_dir} already holds archives for MMX {version}.")
    toolchain = next(line.split()[1] for line in (REPOSITORY / "go.mod").read_text().splitlines()
                     if line.startswith("toolchain "))
    env = dict(os.environ, CGO_ENABLED="0", GOTOOLCHAIN=toolchain, GOFLAGS="")
    goroot = Path(subprocess.check_output(["go", "env", "GOROOT"], cwd=REPOSITORY, env=env, text=True).strip())
    timestamp = int(os.environ["SOURCE_DATE_EPOCH"]) if "SOURCE_DATE_EPOCH" in os.environ else int(
        subprocess.check_output(["git", "log", "-1", "--format=%ct", "HEAD"], cwd=REPOSITORY, text=True).strip())
    with tempfile.TemporaryDirectory(prefix="mmx-release-build-") as directory:
        build_dir = Path(directory)

        def build(package, system, architecture, destination, strip=False):
            flags = ("-s -w " if strip else "") + f"-X mmx/internal/release.Version={version}"
            subprocess.run(["go", "build", "-trimpath", "-buildvcs=false", "-ldflags", flags,
                            "-o", str(destination), package], cwd=REPOSITORY,
                           env=dict(env, GOOS=system, GOARCH=architecture), check=True)
            return destination.read_bytes()

        payload = {name: source.read_bytes() for name, source in archive_files().items() if source is not None}
        payload[VERSION_FILE] = (version + "\n").encode()
        payload[f"{LICENSE_DIR}/go/LICENSE"] = (goroot / "LICENSE").read_bytes()
        for runner in RUNNERS:
            architecture = runner.rsplit("-", 1)[1]
            payload[runner] = build("./cmd/mmx-runner", "linux", architecture, build_dir / runner, strip=True)
        checksums = []
        for system, architecture in CONTROLLER_PLATFORMS:
            payload["mmx"] = build("./cmd/mmx", "darwin" if system == "macos" else system,
                                   architecture, build_dir / "mmx")
            top_directory = f"mmx-{version}-{system}-{architecture}"
            destination = output_dir / (top_directory + ".tar.gz")
            write_archive(destination, top_directory, payload, timestamp)
            subprocess.run([sys.executable, str(REPOSITORY / "scripts/check-release-archive.py"),
                            str(destination), "--version", version], check=True)
            checksums.append(f"{hashlib.sha256(destination.read_bytes()).hexdigest()}  {destination.name}\n")
            print(f"Built {destination.name}: {destination.stat().st_size} bytes.", flush=True)
        (output_dir / "SHA256SUMS").write_text("".join(sorted(checksums)))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--version", required=True)
    parser.add_argument("--output-dir", required=True, type=Path)
    arguments = parser.parse_args()
    try:
        build_archives(arguments.version, arguments.output_dir)
    except (OSError, ValueError, subprocess.CalledProcessError) as error:
        print(f"Release build failed: {error}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
