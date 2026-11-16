#!/usr/bin/env python3
"""Standalone module/helper conformance; automatic module dispatch is separate."""
import argparse
import base64
import json
import os
from pathlib import Path
import signal
import subprocess
import tempfile
import time

import vm


# Keep multi-change failure behavior here, out of the teaching example.
TWO_FILES = r'''#!/usr/bin/env bash
# Test fixture: two writes let the suite observe partial failure and recovery.
set -euo pipefail

: "${MMX_MODULE_HELPER:?MMX_MODULE_HELPER must name the MMX executable}"
request=$("$MMX_MODULE_HELPER" module-json request --inputs path,second_path,text) || exit 1
mode=$("$MMX_MODULE_HELPER" module-json get --type string /mode <<< "$request") || exit 1
change=unchanged
temporary=
text_file=
trap 'if [[ -n "$temporary" ]]; then rm -f -- "$temporary"; fi; if [[ -n "$text_file" ]]; then rm -f -- "$text_file"; fi' EXIT

report() {
    "$MMX_MODULE_HELPER" module-json result --mode "$mode" --status "$1" \
        --change "$change" --reason "$2"
}
fail() {
    report failed "$1"
    exit 1
}

path=$("$MMX_MODULE_HELPER" module-json get --type string /inputs/path <<< "$request") ||
    fail 'path must be a string'
second_path=$("$MMX_MODULE_HELPER" module-json get --type string /inputs/second_path <<< "$request") ||
    fail 'second_path must be a string'
text_file=$(mktemp) || fail 'could not create text file'
"$MMX_MODULE_HELPER" module-json get --type string /inputs/text <<< "$request" > "$text_file" ||
    fail 'text must be a string'
[[ "$path" == /* && "$second_path" == /* && "$path" != "$second_path" ]] ||
    fail 'path and second_path must be different absolute paths'

for destination in "$path" "$second_path"; do
    if [[ -L "$destination" || ( -e "$destination" && ! -f "$destination" ) ]]; then
        fail 'destination must be a regular file'
    fi
done

for destination in "$path" "$second_path"; do
    parent=${destination%/*}
    [[ -n "$parent" ]] || parent=/
    if [[ ! -d "$parent" ]]; then
        if [[ "$mode" == check ]]; then
            change=unknown
            report not_checked 'create the parent directories first'
            exit 0
        fi
        fail 'create the parent directories first'
    fi

    if [[ -f "$destination" ]]; then
        if cmp -s -- "$destination" "$text_file"; then
            continue
        else
            comparison=$?
            [[ "$comparison" == 1 ]] || fail 'could not inspect destination'
        fi
    fi
    if [[ "$mode" == check ]]; then
        change=would_change
        continue
    fi

    temporary=$(mktemp "$parent/.mmx-two-files.XXXXXXXX") || fail 'could not create candidate'
    cat -- "$text_file" > "$temporary" || fail 'could not write candidate'
    mv -f -- "$temporary" "$destination" || fail 'could not install candidate'
    temporary=
    change=changed
done
report succeeded 'both files match the requested text'
'''


class ExecutionTarget:
    def __init__(self, root, helper, remote=False):
        self.root = Path(root)
        self.helper = str(helper)
        self.remote = remote

    def command(self, args, **kwargs):
        if self.remote:
            return vm.guest(*args, **kwargs)
        return vm.cmd(args, cwd=self.root, **kwargs)

    def read(self, path):
        if self.remote:
            return base64.b64decode(self.command(["base64", str(path)]).stdout)
        return path.read_bytes()

    def exists(self, path):
        return self.command(["test", "-e", str(path)], check=False).returncode == 0

    def inode(self, path):
        if self.remote:
            return self.command(["stat", "-c", "%i", str(path)]).stdout.strip()
        return path.stat().st_ino

    def module(self, executable, mode, inputs, *, status="succeeded", change=None):
        process = self.command(["env", "MMX_MODULE_HELPER=" + self.helper, str(executable)],
                               input=json.dumps({"protocol": 1, "mode": mode, "inputs": inputs}),
                               check=False)
        expected = 1 if status == "failed" else 0
        assert process.returncode == expected, (process.returncode, process.stdout, process.stderr)
        # A helper may reject malformed inputs before the module can emit a result.
        if not process.stdout and status == "failed":
            return
        result = json.loads(process.stdout)
        assert result["protocol"] == 1 and result["status"] == status, result
        if change is not None:
            assert result["change"] == change, result
        return result


def helper_cases(node):
    text = 'quote" slash\\ Unicode 日本語\r\n\n'
    request = json.dumps({"protocol": 1, "mode": "check", "inputs": {"text": text}})
    # JSON output avoids the test harness's newline normalization on raw stdout.
    result = node.command([node.helper, "module-json", "get", "/inputs/text"], input=request)
    assert json.loads(result.stdout) == text
    result = node.command([node.helper, "module-json", "result", "--mode", "check",
                           "--change", "would_change", "--secret", "--output-stdin", "greeting"], input=text)
    assert json.loads(result.stdout)["outputs"]["greeting"] == text
    assert json.loads(result.stdout)["secret"] is True
    result = node.command([node.helper, "module-json", "get", "--type", "boolean", "/inputs/text"],
                          input=request, check=False)
    assert result.returncode != 0 and not result.stdout and text not in result.stderr
    assert not node.exists(node.root / ".mmx")
    print("PASS " + ("Linux runner" if node.remote else "controller") + " helper without project or job setup", flush=True)


def two_files_cases(node, executable, label):
    directory = node.root / label
    node.command(["mkdir", "-p", str(directory)])
    path, second_path = directory / "first", directory / "second"
    text = 'quote" slash\\ Unicode 日本語\r\n\n'
    inputs = {"path": str(path), "second_path": str(second_path), "text": text}

    node.module(executable, "check", inputs, change="would_change")
    assert not node.exists(path) and not node.exists(second_path)
    node.module(executable, "apply", inputs, change="changed")
    assert node.read(path) == node.read(second_path) == text.encode()
    inode = node.inode(path)
    node.module(executable, "check", inputs, change="unchanged")
    node.module(executable, "apply", inputs, change="unchanged")
    assert node.inode(path) == inode
    print("PASS " + label + " check, apply, exact text, unchanged rerun", flush=True)

    for bad in [dict(inputs, text=False), dict(inputs, text=None), dict(inputs, text=[]),
                dict(inputs, text="a\x00b"), dict(inputs, path="relative"),
                dict(inputs, second_path=str(path)), dict(inputs, extra="private-token"),
                {"path": str(path), "second_path": str(second_path)}]:
        node.module(executable, "apply", bad, status="failed", change="unchanged")
        assert node.read(path) == node.read(second_path) == text.encode()
    target = directory / "target"
    node.command(["ln", "-s", str(path), str(target)])
    node.module(executable, "apply", dict(inputs, second_path=str(target), text="new"),
                status="failed", change="unchanged")
    assert node.read(path) == text.encode()
    print("PASS " + label + " input errors and symlink rejected before mutation", flush=True)

    for replacement in ["", "new text\n\n", "x" * (256 << 10) + "\n\n"]:
        changed = dict(inputs, text=replacement)
        node.module(executable, "apply", changed, change="changed")
        assert node.read(path) == node.read(second_path) == replacement.encode()
        node.module(executable, "apply", changed, change="unchanged")
    print("PASS " + label + " empty and long input passed through stdin", flush=True)

    partial_path = directory / "partial"
    partial_second_path = directory / "missing-parent" / "second"
    partial = {"path": str(partial_path), "second_path": str(partial_second_path), "text": text}
    node.module(executable, "check", partial, status="not_checked", change="unknown")
    assert not node.exists(partial_path) and not node.exists(partial_second_path)
    node.module(executable, "apply", partial, status="failed", change="changed")
    assert node.read(partial_path) == text.encode() and not node.exists(partial_second_path)
    inode = node.inode(partial_path)
    node.command(["mkdir", str(partial_second_path.parent)])
    node.module(executable, "apply", partial, change="changed")
    assert node.read(partial_second_path) == text.encode() and node.inode(partial_path) == inode
    node.module(executable, "apply", partial, change="unchanged")
    print("PASS " + label + " partial failure followed by successful reapply", flush=True)


def example_cases(node, executable, label):
    directory = node.root / label
    node.command(["mkdir", "-p", str(directory)])
    path = directory / "greeting.txt"
    inputs = {"path": str(path), "person": "Barbara"}
    greeting = b"Hello, Barbara!\n"
    result = node.module(executable, "check", inputs, change="would_change")
    assert result["outputs"]["greeting"].encode() == greeting and not node.exists(path)
    result = node.module(executable, "apply", inputs, change="changed")
    assert result["outputs"]["greeting"].encode() == node.read(path) == greeting
    if node.remote:
        assert node.command(["stat", "-c", "%a", str(path)]).stdout.strip() == "644"
    else:
        assert path.stat().st_mode & 0o777 == 0o644
    inode = node.inode(path)
    for mode in ("check", "apply"):
        result = node.module(executable, mode, inputs, change="unchanged")
        assert result["outputs"]["greeting"].encode() == greeting and node.inode(path) == inode
    for bad in [dict(inputs, person=False), dict(inputs, person=None), dict(inputs, person=[]),
                dict(inputs, person="a\x00b"), dict(inputs, person="Barbara\n"),
                dict(inputs, path="relative"), dict(inputs, path=str(path) + "\n"),
                dict(inputs, extra="private-token"), {"path": str(path)}]:
        node.module(executable, "apply", bad, status="failed", change="unchanged")
        assert node.read(path) == greeting and node.inode(path) == inode
    for destination, reason in [(directory / "absent" / "file", "parent directory"),
                                (directory, "regular file")]:
        for mode in ("check", "apply"):
            result = node.module(executable, mode, dict(inputs, path=str(destination)),
                                 status="failed", change="unchanged")
            assert reason in result["reason"], result
    link = directory / "link"
    node.command(["ln", "-s", str(path), str(link)])
    dangling = directory / "dangling"
    node.command(["ln", "-s", str(directory / "absent"), str(dangling)])
    fifo = directory / "fifo"
    node.command(["mkfifo", str(fifo)])
    for destination in (link, dangling, fifo):
        for mode in ("check", "apply"):
            result = node.module(executable, mode, dict(inputs, path=str(destination)),
                                 status="failed", change="unchanged")
            assert "regular file" in result["reason"]
        assert node.read(path) == greeting
    # A correct greeting with an extra newline must still need repair.
    node.command(["sh", "-c", 'printf "Hello, Barbara!\\n\\n" > "$1"', "sh", str(path)])
    node.module(executable, "check", inputs, change="would_change")
    assert node.read(path) == greeting + b"\n"
    node.module(executable, "apply", inputs, change="changed")
    assert node.read(path) == greeting
    # Different names, including empty and long strings, travel through stdin.
    for person in ["", 'quote" slash\\ Unicode 日本語', "x" * (256 << 10)]:
        changed = dict(inputs, person=person)
        before = node.read(path)
        node.module(executable, "check", changed, change="would_change")
        assert node.read(path) == before
        result = node.module(executable, "apply", changed, change="changed")
        expected = ("Hello, " + person + "!\n").encode()
        assert result["outputs"]["greeting"].encode() == node.read(path) == expected
        node.module(executable, "apply", changed, change="unchanged")
    print("PASS " + label + " greeting check/apply/output, 0644, unchanged rerun, invalid inputs and paths", flush=True)


def interrupted_reply(node, executable, label):
    # Fill stdout's pipe before launch. The module can change files, but cannot
    # finish reporting. Kill it after observing those effects, then reapply.
    directory = node.root / (label + "-interrupted")
    directory.mkdir()
    inputs = {"path": str(directory / "greeting.txt"), "person": "Barbara"}
    read_fd, write_fd = os.pipe()
    process = None
    try:
        os.set_blocking(write_fd, False)
        while True:
            try:
                os.write(write_fd, b"x" * 4096)
            except BlockingIOError:
                break
        os.set_blocking(write_fd, True)
        process = subprocess.Popen([str(executable)], cwd=node.root,
                                   env=dict(vm.ENV, MMX_MODULE_HELPER=node.helper),
                                   stdin=subprocess.PIPE, stdout=write_fd, stderr=subprocess.PIPE,
                                   start_new_session=True)
        process.stdin.write(json.dumps({"protocol": 1, "mode": "apply", "inputs": inputs}).encode())
        process.stdin.close()
        deadline = time.monotonic() + 15
        while True:
            if Path(inputs["path"]).exists() and Path(inputs["path"]).read_bytes() == b"Hello, Barbara!\n":
                break
            assert process.poll() is None, process.stderr.read()
            assert time.monotonic() < deadline, "module did not apply before deadline"
            time.sleep(0.01)
        assert process.poll() is None, "module should be waiting to report"
        os.killpg(process.pid, signal.SIGKILL)
        process.wait(timeout=5)
        node.module(executable, "apply", inputs, change="unchanged")
        print("PASS " + label + " interrupted result followed by unchanged reapply", flush=True)
    finally:
        if process is not None:
            if process.poll() is None:
                os.killpg(process.pid, signal.SIGKILL)
                process.wait(timeout=5)
            process.stderr.close()
        os.close(read_fd)
        os.close(write_fd)


def interrupted_helper(node):
    # The standalone helper must honor one interrupt even while awaiting stdin.
    # A background shell can pass SIGINT=ignored through Python. Give this
    # fixture the normal foreground disposition before exec.
    process = subprocess.Popen([node.helper, "module-json", "request"],
                               cwd=node.root, env=vm.ENV, stdin=subprocess.PIPE,
                               stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                               start_new_session=True,
                               preexec_fn=lambda: signal.signal(signal.SIGINT, signal.SIG_DFL))
    try:
        time.sleep(0.2)
        process.send_signal(signal.SIGINT)
        assert process.wait(timeout=5) != 0
        assert process.stdout.read() == b""
        print("PASS controller helper stops on one interrupt", flush=True)
    finally:
        if process.poll() is None:
            process.kill()
            process.wait(timeout=5)
        process.stdin.close()
        process.stdout.close()
        process.stderr.close()


def build_go(output, *, target=None):
    env = dict(vm.ENV, CGO_ENABLED="0")
    if target:
        env.update(GOOS="linux", GOARCH=target)
    subprocess.run(["go", "build", "-trimpath", "-o", str(output),
                    "./examples/external-modules/go/greeting"], cwd=vm.REPO, env=env, check=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--local-only", action="store_true", help="run without starting a VM")
    parser.add_argument("--helper", type=Path, help="use this existing MMX binary for local checks")
    args = parser.parse_args()
    if args.helper and not args.local_only:
        parser.error("--helper requires --local-only")
    if not args.helper:
        vm.build()
    bash = vm.REPO / "examples/external-modules/bash/greeting/module_executable"
    with tempfile.TemporaryDirectory(prefix="module-helper-", dir=None if args.local_only else vm.ROOT) as workspace:
        local = ExecutionTarget(workspace, args.helper.resolve() if args.helper else vm.ROOT / "bin/mmx")
        fixture = local.root / "two-files"
        fixture.write_text(TWO_FILES)
        fixture.chmod(0o700)
        go = local.root / "greeting-go"
        build_go(go)
        helper_cases(local)
        interrupted_helper(local)
        for executable, label in [(bash, "local-bash"), (go, "local-go")]:
            example_cases(local, executable, label)
            interrupted_reply(local, executable, label)

        two_files_cases(local, fixture, "local-two-files")
        if args.local_only:
            print("PASS local standalone helper and module examples", flush=True)
            return

        vm.up()
        remote_root = vm.guest("mktemp", "-d", "/var/tmp/mmx-module-helper.XXXXXXXX").stdout.strip()
        remote = ExecutionTarget(remote_root, remote_root + "/mmx-runner", True)
        try:
            assert vm.guest("id", "-u").stdout.strip() != "0"
            vm.copy(vm.ROOT / ("bin/mmx-runner-linux-" + vm.architecture()), remote.helper)
            vm.copy(bash, remote_root + "/greeting-bash")
            build_go(go, target=vm.architecture())
            vm.copy(go, remote_root + "/greeting-go")
            remote.command(["chmod", "700", remote.helper, remote_root + "/greeting-bash", remote_root + "/greeting-go"])
            vm.copy(fixture, remote_root + "/two-files")
            helper_cases(remote)
            two_files_cases(remote, remote.root / "two-files", "linux-two-files")
            for name in ["bash", "go"]:
                example_cases(remote, remote.root / ("greeting-" + name), "linux-" + name)
        finally:
            # This suite owns only its mktemp directory, not node MMX state.
            vm.guest("rm", "-rf", remote_root)
    print("PASS standalone helper and module examples on the controller and Linux guest", flush=True)


if __name__ == "__main__":
    # A local-only invocation has no shared VM environment to lock.
    import sys
    if "--local-only" in sys.argv:
        main()
    else:
        vm.run_suite(main)
