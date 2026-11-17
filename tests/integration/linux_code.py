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
        # internal/controller alone takes about 130 s in an idle 2-CPU guest.
        # The full catalog runs it beside the installer suites, which load the
        # server's disk: it took 376 to 630 s in the Rocky Linux 9 and
        # AlmaLinux 10 VMs, mostly in fsync. The limit only catches a hang.
        result = vm.guest("sudo", "-n", target, "-test.timeout=1800s", timeout=1815, name=name)
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
