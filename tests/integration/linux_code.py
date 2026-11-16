#!/usr/bin/env python3
"""Run the Go code tests as native Linux executables without installing Go in the VM."""
from concurrent.futures import ThreadPoolExecutor
import os
import subprocess

import vm

# Test programs running at once on the node, which has two CPUs: one program
# at a time made this the longest suite, mostly waiting.
PROGRAMS_AT_ONCE = 3


def compile_tests(output, architecture):
    output.mkdir(parents=True, exist_ok=True)
    env = dict(os.environ, CGO_ENABLED="0", GOOS="linux", GOARCH=architecture)
    subprocess.run(["go", "test", "-c", "-o", str(output) + "/", "./internal/...", "./cmd/mmx"],
                   cwd=vm.REPO, env=env, check=True)
    (output / "complete").touch()


def main(name="mmx-peer"):
    vm.up(name)
    architecture = vm.architecture(name)
    # The job slots of one run compile the test programs once and share them.
    with vm.shared_build() as shared:
        output = (shared or vm.ROOT) / ("linux-tests-" + architecture)
        if shared is None or not (output / "complete").exists():
            compile_tests(output, architecture)
    vm.guest("mkdir", "-p", "/var/tmp/mmx-code-tests", name=name)

    def run(binary):
        target = "/var/tmp/mmx-code-tests/" + binary.name
        vm.copy(binary, target, name)
        # internal/controller alone takes up to 210 s in a 2-CPU guest, and
        # the full catalog runs it beside the installer suites.
        result = vm.guest("sudo", "-n", target, "-test.timeout=600s", timeout=615, name=name)
        assert result.stdout.strip().endswith("PASS"), result.stdout
        return "PASS Linux " + binary.stem

    try:
        with ThreadPoolExecutor(max_workers=PROGRAMS_AT_ONCE) as pool:
            for line in pool.map(run, sorted(output.glob("*.test"))):
                print(line, flush=True)
    finally:
        vm.guest("rm", "-rf", "/var/tmp/mmx-code-tests", name=name)


if __name__ == "__main__":
    vm.run_suite(main)
