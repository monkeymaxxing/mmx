#!/usr/bin/env python3
"""External executable modules through ordinary public MMX task workflows."""
import hashlib
import json
import shlex
import shutil
import subprocess
import time

import vm
from module_helper import TWO_FILES


def main():
    vm.up()
    node, ssh = vm.connection()
    vm.reset()
    architecture = vm.architecture()
    workspace = vm.ROOT / "external-modules"
    if workspace.exists():
        shutil.rmtree(workspace)
    workspace.mkdir()
    vm.build()
    mmx = vm.ROOT / "bin/mmx"
    base = "/var/tmp/mmx-external-modules"
    passed = []

    def remote(script):
        return vm.cmd([*ssh, "sudo -n /bin/sh -c " + shlex.quote(script)]).stdout

    def project(tasks, **extra):
        data = {"project_name": "external_modules", 'default_inventory': 'example',
                "node_task_defaults": {"retry": {"pause_between_tries": "0s"}},
                "fleet_stages": [{"id": "configure", "nodes": "all", "node_tasks": tasks}], **extra}
        vm.write_project(workspace, playbook_name='configure', **data)

    def run(command, expected=None, *args):
        result = vm.cmd([mmx, command, *vm.playbook_args(command, "configure"), "--json", *args], cwd=workspace, check=False)
        assert result.returncode in (vm.success_codes(command) if expected is None else (expected,)), (command, result.stdout, result.stderr)
        return json.loads(result.stdout)

    def results(record):
        return record["entries"][0]["response"]["results"]

    def submitted_job(record):
        job_id = record["entries"][0]["job_id"]
        return json.loads(remote(f"cat /var/lib/mmx/jobs/{job_id}/request.json"))

    def mark(name):
        passed.append(name)
        print("PASS " + name, flush=True)

    def describe(name, inputs=("data", "kind", "path", "person", "second_path", "text")):
        # Every module has a description; these fixtures take any of the
        # suite's inputs and return the outputs the suite reads.
        directory = workspace / "external_modules" / name
        directory.mkdir(parents=True, exist_ok=True)
        (directory / "module_description.yml").write_text(json.dumps({
            "summary": "A fixture of the suite for modules of the project's own.",
            "inputs": {i: {"type": "any", "required": False, "description": "Any value."} for i in inputs},
            "outputs": {"greeting": {"type": "string", "description": "The greeting."},
                        "data": {"type": "any", "description": "The data."}},
            "examples": [{"description": "A call.", "config": name + ": {}\n"}]}))

    def install(name, text):
        describe(name)
        path = workspace / "external_modules" / name / "module_executable"
        path.write_text(text)
        path.chmod(0o700)
        return path

    remote(f"rm -rf {base}; mkdir -p {base}")
    try:
        bash = install("greeting_bash", (vm.REPO / "examples/external-modules/bash/greeting/module_executable").read_text())
        describe("greeting_go")
        go_dir = workspace / "external_modules/greeting_go"
        for arch in ("amd64", "arm64"):
            output = go_dir / ("module_executable_linux_" + arch)
            subprocess.run(["go", "build", "-trimpath", "-o", str(output),
                            "./examples/external-modules/go/greeting"], cwd=vm.REPO,
                           env=dict(vm.ENV, CGO_ENABLED="0", GOOS="linux", GOARCH=arch), check=True)
        # The actual executable is larger than the instruction limit; trailing
        # padding leaves ELF execution intact and exercises the ordinary path.
        large = go_dir / ("module_executable_linux_" + architecture)
        with large.open("ab") as output:
            output.truncate(18 << 20)
        text = 'exact "quote" \\ Unicode 日本語\n\n'
        greeting = "Hello, Barbara!\n"
        for connection in ("ssh_only", "https_with_ssh_fallback"):
            vm.write_inventory(workspace, {"nodes": {"test": dict(node, connection=connection)}}, name='example')
            for name in ("greeting_bash", "greeting_go"):
                destination = base + "/" + connection + "-" + name
                task = {"id": "greeting", name: {
                    "path": destination, "person": "Barbara"}}
                project([task, {"id": "returned_greeting", "show": "{{ .node_tasks.greeting.greeting }}"}])
                assert run("validate")["valid"]
                preview = run("dryrun")
                assert results(preview)[0]["change"] == "would_change"
                remote(f"test ! -e {destination}")
                applied = run("run")
                assert results(applied)[0]["change"] == "changed"
                assert applied["entries"][0]["connection_used"] == ("ssh" if connection == "ssh_only" else "https")
                assert applied["entries"][0]["architecture"] == architecture
                assert results(applied)[0]["module_outputs"]["greeting"] == greeting
                assert results(applied)[0]["module_kind"] == "project_executable"
                assert results(applied)[1]["value"] == greeting
                assert remote("sha256sum " + destination).split()[0] == hashlib.sha256(greeting.encode()).hexdigest()
                assert remote("stat -c %a " + destination).strip() == "644"
                inode = remote("stat -c %i " + destination)
                assert results(run("run"))[0]["change"] == "unchanged"
                assert remote("stat -c %i " + destination) == inode
                bundle = submitted_job(applied)
                assert list(bundle["modules"]) == [name]
                selected = bundle["modules"][name]
                assert selected["executable"] == ("module_executable" if name == "greeting_bash" else "module_executable_linux_" + architecture)
                assert all(f["path"] != "module_executable_linux_" + ("amd64" if architecture == "arm64" else "arm64") for f in selected["files"])
                assert len(json.dumps(bundle)) < 16384
                job_id = applied["entries"][0]["job_id"]
                remote(f"test ! -e /var/lib/mmx/uploads/{job_id}")
                # The node keeps the module's files by SHA-256 for later runs.
                remote("cd /var/lib/mmx/kept_files && test -f " + " -a -f ".join(f["asset"]["sha256"] for f in selected["files"]))
                saved_results = run("results", 0, applied["id"])
                assert results(saved_results)[0]["change"] == "changed"
            mark(connection + " automatically delivers both examples, executes the 18 MiB Go binary, keeps module files by SHA-256, and cleans node assets")

        # Saved resume must deliver both script and compiled example bytes even
        # when their original module directories are no longer available.
        for name in ("greeting_bash", "greeting_go"):
            destination = base + "/resume-" + name + "/greeting.txt"
            project([{"id": "greeting", name: {
                "path": destination, "person": "Barbara"}}])
            failed = run("run", 1)
            assert results(failed)[0]["change"] == "unchanged"
            assert "parent directory" in results(failed)[0]["reason"]
            remote("test ! -e " + destination)
            local = workspace / "external_modules" / name
            backup = workspace / ("restore-" + name)
            local.rename(backup)
            try:
                remote("mkdir " + base + "/resume-" + name)
                resumed = run("resume", 0, failed["id"])
                assert results(resumed)[0]["change"] == "changed"
                assert results(resumed)[0]["module_outputs"]["greeting"] == greeting
                assert remote("sha256sum " + destination).split()[0] == hashlib.sha256(greeting.encode()).hexdigest()
            finally:
                backup.rename(local)
        mark("both greeting examples resume from saved executable bytes after local module removal")

        # A two-write fixture proves partial failure without complicating greeting.
        install("two_files", TWO_FILES)
        name = "two_files"
        destination = base + "/partial-" + name
        task = {"id": "partial", name: {
            "path": destination, "second_path": destination + "-parent/second", "text": text}}
        project([task])
        assert results(run("dryrun"))[0]["status"] == "not_checked"
        remote("test ! -e " + destination)
        failed = run("run", 1)
        partial = results(failed)[0]
        assert partial["change"] == "changed" and len(partial["tries"]) == 3
        assert partial["tries"][0]["change"] == "changed"
        assert partial["tries"][1]["change"] == "unchanged"
        # Remove this local fixture; resume must use its saved executable.
        local = workspace / "external_modules" / name
        backup = workspace / ("restore-" + name)
        local.rename(backup)
        try:
            remote(f"mkdir {destination}-parent")
            resumed = run("resume", 0, failed["id"])
            assert results(resumed)[0]["change"] == "changed"
            assert remote(f"sha256sum {destination}-parent/second").split()[0] == hashlib.sha256(text.encode()).hexdigest()
        finally:
            backup.rename(local)
        mark("two-write fixture preserves partial changes and recorded resume works after local module removal")

        echo = install("inspect", '''#!/usr/bin/env bash
set -euo pipefail
h=$MMX_MODULE_HELPER
request=$("$h" module-json request --inputs data,kind)
mode=$(printf '%s' "$request" | "$h" module-json get --type string /mode)
kind=$(printf '%s' "$request" | "$h" module-json get --type string /inputs/kind)
case "$kind" in
  support) "$h" module-json result --mode "$mode" --change unchanged --output-json data "$(cat support/value.json)" ;;
  fail)
    printf '%s' "$request" | "$h" module-json get --type string /inputs/data >&2
    printf '%s' "$request" | "$h" module-json get --type string /inputs/data |
      "$h" module-json result --mode "$mode" --change unchanged --status failed --reason-stdin
    exit 1 ;;
  *) "$h" module-json result --mode "$mode" --change unchanged \
       --output-json data "$(printf '%s' "$request" | "$h" module-json get --type json /inputs/data)" ;;
esac
''')
        support = echo.parent / "support/value.json"
        support.parent.mkdir()
        literal = "{{ .secrets.not_an_input }}"
        support.write_text(json.dumps(literal))
        role = workspace / "roles/demo"
        role.mkdir(parents=True)
        (role / "main.yml").write_text(json.dumps({"inputs": {"data": {"type": "mapping", "required": True}},
            "node_tasks": [{"id": "echo", "inspect": {"data": "{{ .inputs.data }}", "kind": "echo"}}],
            "outputs": {"data": "{{ .node_tasks.echo.data }}"}}))
        data = {"none": None, "off": False, "zero": 0, "empty": "", "big": 9007199254740993,
                "nested": [False, None, text]}
        project([{"id": "role", "role": "demo", "inputs": {"data": data}},
                 {"id": "loop", "loop": [None, False, 0, ""], "as": "item",
                  "inspect": {"kind": "echo", "data": "{{ .item }}"}},
                 {"id": "literal", "inspect": {"kind": "support"}},
                 {
                     "task_block": {
                         "id": "custom",
                         "changes": [{"id": "nested", "inspect": {"kind": "echo", "data": "{{ .node_tasks.role.outputs.data }}"}}],
                         "cleanup": [{"id": "cleanup", "inspect": {"kind": "echo", "data": "done"}}],
                     },
                     "id": "grouped",
                 },
                 {"id": "skipped", "when": False,
                  "inspect": {"kind": "echo", "data": "{{ .vars.mapping.missing }}"}}], vars={"mapping": {}})
        record = run("run")
        by_id = {r["task"]: r for r in results(record)}
        assert by_id["role/echo"]["module_outputs"]["data"] == data and by_id["grouped/changes/nested"]["module_outputs"]["data"] == data
        assert [item["module_outputs"]["data"] for item in by_id["loop"]["items"]] == [None, False, 0, ""]
        assert by_id["literal"]["module_outputs"]["data"] == literal and by_id["skipped"]["status"] == "skipped"
        assert by_id["grouped/cleanup/cleanup"]["module_outputs"]["data"] == "done"
        assert len(submitted_job(record)["modules"]) == 1
        assert "not_an_input" in vm.cmd([mmx, "results", record["id"]], cwd=workspace).stdout
        mark("roles, loops, task block phases, typed results, literal supporting files, and false conditions use ordinary task behavior")

        # Freeze support files and selected executable; a fresh run sees edits.
        gate = base + "/gate"
        project([{"id": "support", "inspect": {"kind": "support"}},
                 {"shell": {"script": "test -f " + gate}}])
        failed = run("run", 1)
        original = echo.read_text()
        echo.write_text("#!/bin/sh\nexit 77\n")
        support.write_text(json.dumps("edited support"))
        remote("touch " + gate)
        resumed = run("resume", 0, failed["id"])
        assert results(resumed)[0]["module_outputs"]["data"] == literal
        fresh = run("run", 1)
        assert results(fresh)[0]["status"] == "failed"
        echo.write_text(original)
        assert results(run("run"))[0]["module_outputs"]["data"] == "edited support"
        mark("saved resume retains executable and supporting-file bytes; a new run uses current files")

        private = "module-private-input-unique"
        vm.ENV["MMX_MODULE_TEST_SECRET"] = private
        for connection in ("ssh_only", "https_with_ssh_fallback"):
            vm.write_inventory(workspace, {"nodes": {"test": dict(node, connection=connection)}}, name='example')
            project([{"id": "private", "inspect": {"kind": "echo", "data": "{{ .secrets.token }}"}},
                     {"file": {"path": base + "/private", "content": "{{ .node_tasks.private.data }}", "permissions_mode": "0600"}},
                     {"show": "{{ .node_tasks.private.data }}"}], secrets_from_env={"token": "MMX_MODULE_TEST_SECRET"})
            record = run("run")
            assert remote("cat " + base + "/private") == private
            assert private not in json.dumps(record) and results(record)[0]["secret"]
            project([{"id": "private", "retry": False, "inspect": {
                "kind": "fail", "data": "{{ .secrets.token }}"}}], secrets_from_env={"token": "MMX_MODULE_TEST_SECRET"})
            failure = run("run", 1)
            assert private not in json.dumps(failure)
            for command in ("results", "status"):
                assert private not in vm.cmd([mmx, command, *vm.playbook_args(command, "configure"), failure["id"]], cwd=workspace).stdout
        for path in (workspace / ".mmx").rglob("*"):
            if path.is_file() and "runners" not in path.parts:
                assert private.encode() not in path.read_bytes(), path
        assert private not in remote("find /var/lib/mmx/jobs -type f -exec cat {} +")
        mark("secret inputs reach modules over both transports; results propagate privately and failures/results retain no plaintext")

        remote("rm -f " + gate)
        project([{"id": "private", "inspect": {"kind": "echo", "data": "{{ .secrets.token }}"}},
                 {"file": {"path": base + "/private", "content": "{{ .node_tasks.private.data }}", "permissions_mode": "0600"}},
                 {"shell": {"script": "test -f " + gate}}], secrets_from_env={"token": "MMX_MODULE_TEST_SECRET"})
        failed = run("run", 1)
        rotated = private + "-rotated"
        vm.ENV["MMX_MODULE_TEST_SECRET"] = rotated
        original = echo.read_text()
        echo.write_text("#!/bin/sh\nexit 77\n")
        try:
            remote("touch " + gate)
            record = run("resume", 0, failed["id"])
            assert record["secret_inputs_refreshed"]
            assert remote("cat " + base + "/private") == rotated
            assert private not in json.dumps(record)
            for path in (workspace / ".mmx").rglob("*"):
                if path.is_file() and "runners" not in path.parts:
                    assert private.encode() not in path.read_bytes(), path
        finally:
            echo.write_text(original)
            del vm.ENV["MMX_MODULE_TEST_SECRET"]
        mark("recorded module resume refreshes secret inputs while preserving its executable and task definition")

        own_secret = "module-declared-private-unique"
        remote("printf %s " + shlex.quote(own_secret) + " > " + base + "/discovered-private")
        install("private", f'''#!/bin/sh
cat >/dev/null
cat {base}/discovered-private >&2
cat {base}/discovered-private | "$MMX_MODULE_HELPER" module-json result --mode apply --status failed --change unchanged --secret --reason-stdin
exit 1
''')
        project([{"private": {}, "retry": False}])
        record = run("run", 1)
        assert own_secret not in json.dumps(record) and results(record)[0]["secret"]
        assert own_secret not in vm.cmd([mmx, "results", record["id"]], cwd=workspace).stdout
        mark("module-declared private diagnostics are suppressed even in the job failure summary")

        install("timeout", f'''#!/bin/sh
cat >/dev/null
(sleep 2; touch {base}/late-child) &
sleep 20
''')
        project([{"id": "timeout", "timeout": {}, "time_limit_per_try": "200ms", "retry": False}])
        record = run("run", 1)
        assert results(record)[0]["change"] == "unknown" and "deadline" in results(record)[0]["reason"]
        time.sleep(2)
        remote("test ! -e " + base + "/late-child")
        mark("module timeout kills child processes and reports unknown effects without a valid result")

        install("wait", f'''#!/bin/sh
cat >/dev/null
touch {base}/started
while test ! -e {base}/release; do sleep 0.1; done
printf applied > {base}/finished
"$MMX_MODULE_HELPER" module-json result --mode apply --change changed
''')
        project([{"id": "wait", "wait": {}}, {"file": {"permissions_mode": "0644", "path": base + "/after-stop", "content": "after"}}])
        process = subprocess.Popen([str(mmx), "run", 'configure', "--json"], cwd=workspace, env=vm.ENV,
                                   stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
        try:
            deadline = time.monotonic() + 40
            while remote("if test -f " + base + "/started; then printf ready; fi") != "ready":
                assert process.poll() is None, process.communicate()
                assert time.monotonic() < deadline
                time.sleep(0.1)
            process.kill()
            process.communicate(timeout=5)
            run("stop")
            remote("touch " + base + "/release")
            deadline = time.monotonic() + 30
            while True:
                status = run("status")
                if status["status"] == "stopped":
                    break
                assert time.monotonic() < deadline, status
                time.sleep(0.1)
            assert remote("cat " + base + "/finished") == "applied"
            remote("test ! -e " + base + "/after-stop")
            assert run("resume")["status"] == "succeeded"
        finally:
            if process.poll() is None:
                process.kill()
                process.communicate(timeout=5)
        mark("controller loss leaves the module running; stop finishes its task and resume completes remaining work")

        # A Starlark module: MMX decides its change and its fingerprint, and a
        # service in its node task block restarts exactly when the
        # fingerprint changes.
        writer = workspace / "external_modules/config_writer"
        writer.mkdir(parents=True, exist_ok=True)
        (writer / "module.star").write_text('''def run_node_task(inputs):
    written = write_file(inputs["path"], inputs["text"])
    if written.error:
        return {"status": "failed", "reason": written.error}
    return None
''')
        (writer / "module_description.yml").write_text(json.dumps({
            "summary": "Writes a configuration file.",
            "inputs": {"path": {"type": "string", "required": True, "description": "The file."},
                       "text": {"type": "string", "required": True, "description": "Its text."}},
            "examples": [{"description": "Write a file.", "config": "config_writer: {path: /tmp/x, text: x}\n"}]}))
        # The checks below restart the unit more often than systemd's start
        # limit of five in ten seconds allows.
        unit = ("[Unit]\nStartLimitIntervalSec=0\n[Service]\nExecStart=/bin/sleep infinity\n"
                "[Install]\nWantedBy=multi-user.target\n")
        configured = base + "/module-follow.conf"

        noted = base + "/module-follow.ran"

        def follow(text, command=False, other=None):
            """Write the project: a node task block that writes text with the
            module and names the service. With command, a command follows the
            module in that block too. With other, a second node task block
            writes other with the same module and names the same service."""
            service = {"systemd_service": {"name": "mmx-module-follow", "start_at_boot": True, "refresh": "restart", "state": "running"}}
            activate = [dict(service, id="service")]
            if command:
                activate.append({"id": "note", "shell": {
                    "script": "echo ran >> " + noted, "only_when_tracked_inputs_changed": True}})
            blocks = [{"task_block": {"id": "follow",
                                      "changes": [{"id": "write", "config_writer": {"path": configured, "text": text}}],
                                      "activate": activate}}]
            if other is not None:
                blocks.append({"task_block": {"id": "follow_other",
                                              "changes": [{"id": "write_other", "config_writer": {
                                                  "path": configured + ".other", "text": other}}],
                                              "activate": [dict(service, id="service_other")]}})
            project([{"systemd_unit": {"name": "mmx-module-follow.service", "content": unit}}, *blocks])

        def followed():
            """Run the project. Return the change of each node task by its
            ID, and the service's instance afterwards."""
            changes = {r["task"].rsplit("/", 1)[-1]: r["change"] for r in results(run("run"))}
            return changes, remote("systemctl show -p InvocationID --value mmx-module-follow")

        follow("first\n")
        preview = run("dryrun")
        written = [r for r in results(preview) if r["task"].endswith("/write")][0]
        assert written["change"] == "would_change" and written["module_kind"] == "project_starlark" and "first" in written["diff"], written
        remote("test ! -e " + configured)
        applied = run("run")
        assert [r for r in results(applied) if r["task"].endswith("/write")][0]["change"] == "changed"
        assert remote("cat " + configured) == "first\n"
        started = remote("systemctl show -p InvocationID --value mmx-module-follow")
        unchanged = run("run")
        assert [r for r in results(unchanged) if r["task"].endswith("/write")][0]["change"] == "unchanged"
        assert remote("systemctl show -p InvocationID --value mmx-module-follow") == started
        follow("second\n")
        run("run")
        restarted = remote("systemctl show -p InvocationID --value mmx-module-follow")
        assert restarted != started and remote("cat " + configured) == "second\n"
        run("run")
        assert remote("systemctl show -p InvocationID --value mmx-module-follow") == restarted
        mark("a Starlark module previews without writing, and its fingerprint restarts the node task block's service exactly when it changes")

        # A reboot removes the service's record, and the service that the
        # boot started has loaded what the module keeps: an unchanged run
        # restarts nothing. Only the module's own run reports its
        # fingerprint, so MMX has nothing on the node to compare with before.
        vm.reboot()
        node, ssh = vm.connection()
        vm.write_inventory(workspace, {"nodes": {"test": dict(node, connection="https_with_ssh_fallback")}}, name='example')
        remote("test ! -e /run/mmx/services")
        booted = remote("systemctl show -p InvocationID --value mmx-module-follow")
        assert booted.strip() and booted != restarted, (booted, restarted)
        changes, instance = followed()
        assert (changes["write"], changes["service"], instance) == ("unchanged", "unchanged", booted), (changes, instance, booted)
        follow("third\n")
        changes, changed = followed()
        assert (changes["write"], changes["service"]) == ("changed", "changed") and changed != booted, (changes, changed, booted)
        assert remote("cat " + configured) == "third\n"
        changes, instance = followed()
        assert (changes["write"], changes["service"], instance) == ("unchanged", "unchanged", changed), (changes, instance, changed)
        # A restart by hand gives the service a new instance, for which MMX
        # has no record either. An unchanged module restarts nothing after
        # it; a module that changes what it keeps restarts the service.
        remote("systemctl restart mmx-module-follow")
        by_hand = remote("systemctl show -p InvocationID --value mmx-module-follow")
        changes, instance = followed()
        assert (changes["write"], changes["service"], instance) == ("unchanged", "unchanged", by_hand), (changes, instance, by_hand)
        remote("systemctl restart mmx-module-follow")
        by_hand = remote("systemctl show -p InvocationID --value mmx-module-follow")
        follow("fourth\n")
        changes, changed = followed()
        assert (changes["write"], changes["service"]) == ("changed", "changed") and changed != by_hand, (changes, changed, by_hand)
        changes, instance = followed()
        assert (changes["write"], changes["service"], instance) == ("unchanged", "unchanged", changed), (changes, instance, changed)
        mark("after a reboot, and after a restart by hand, an unchanged Starlark module restarts nothing, and a changed one restarts the service once")

        # A command that follows the module is new: its record has no
        # fingerprint, and the unchanged module does not run it.
        follow("fourth\n", command=True)
        changes, instance = followed()
        assert set(changes.values()) == {"unchanged"} and instance == changed, (changes, instance, changed)
        remote("test ! -e " + noted)
        follow("fifth\n", command=True)
        changes, fifth = followed()
        assert changes == dict(changes, write="changed", service="changed", note="changed") and fifth != changed, (changes, fifth)
        changes, instance = followed()
        assert set(changes.values()) == {"unchanged"} and instance == fifth, (changes, instance, fifth)
        assert remote("cat " + noted) == "ran\n"
        mark("a command that is new does not run for an unchanged Starlark module, and runs once when the module changes what it keeps")

        # A second node task block calls the same module for the same
        # service, as a second call of one role does. Each block follows its
        # own calls: the new block restarts the service once for what it
        # wrote, an unchanged run restarts nothing, and a change in the
        # first block restarts once.
        follow("fifth\n", command=True, other="one\n")
        changes, other_block = followed()
        assert changes == dict(changes, write="unchanged", service="unchanged", note="unchanged",
                               write_other="changed", service_other="changed") and other_block != fifth, (changes, other_block)
        changes, instance = followed()
        assert set(changes.values()) == {"unchanged"} and instance == other_block, (changes, instance, other_block)
        follow("sixth\n", command=True, other="one\n")
        changes, sixth = followed()
        assert changes == dict(changes, write="changed", service="changed", note="changed",
                               write_other="unchanged", service_other="unchanged") and sixth != other_block, (changes, sixth)
        changes, instance = followed()
        assert set(changes.values()) == {"unchanged"} and instance == sixth, (changes, instance, sixth)
        mark("two node task blocks that call one Starlark module for one service each follow their own calls")

        # What a module that drives a tool needs from MMX: find_program, the
        # statuses missing_prerequisite and not_checked, a fixed environment
        # for its commands, and a private value that does not hide the result.
        tool_user = workspace / "external_modules" / "tool_user"
        tool_user.mkdir(parents=True)
        (tool_user / "module.star").write_text('''def run_node_task(inputs):
    tool = find_program(inputs["program"])
    if not tool.found:
        return {"status": "missing_prerequisite", "reason": "tool_user: the program " + inputs["program"] + " is not installed; install it in an earlier node task"}
    if not inputs["server_answers"]:
        return {"status": "not_checked" if is_dryrun() else "failed", "reason": "tool_user: the server does not answer"}
    environment = run_command(["env"])
    # The tool reads the password from its standard input and prints it,
    # as a tool's error message can.
    printed = run_command([tool.path], standard_input = inputs["password"])
    return {"status": "failed", "reason": "tool_user: the tool refused the password " + printed.standard_output,
            "outputs": {"environment": environment.standard_output}}
''')
        (tool_user / "module_description.yml").write_text('''summary: A fixture of the suite that drives a tool.
inputs:
  program: {type: string, required: true, description: The tool's program.}
  server_answers: {type: boolean, required: true, description: Whether the tool's server answers.}
  password: {type: string, required: true, private_value: true, description: The password the tool gets.}
outputs:
  environment: {type: string, description: The environment of a command.}
examples:
  - description: A call.
    config: |
      tool_user: {program: cat, server_answers: true, password: p}
''')
        password = "tool-password-unique"
        vm.ENV["MMX_MODULE_TEST_SECRET"] = password

        def tool(program, server_answers=True):
            project([{"id": "tool", "tool_user": {"program": program, "server_answers": server_answers, "password": "{{ .secrets.token }}"}}],
                    secrets_from_env={"token": "MMX_MODULE_TEST_SECRET"})

        for connection in ("ssh_only", "https_with_ssh_fallback"):
            vm.write_inventory(workspace, {"nodes": {"test": dict(node, connection=connection)}}, name='example')
            tool("mmx-no-such-tool")
            previewed = results(run("dryrun", 3))[0]
            assert previewed["status"] == "not_checked" and previewed["change"] == "unknown" and \
                previewed["reason"] == "tool_user: the program mmx-no-such-tool is not installed; install it in an earlier node task", previewed
            missing = results(run("run", 1))[0]
            assert missing["status"] == "failed" and missing["failure_kind"] == "missing_prerequisite" and missing["change"] == "unchanged" and \
                len(missing["tries"]) == 1 and missing["reason"] == previewed["reason"], missing
            tool("cat", server_answers=False)
            assert results(run("dryrun", 3))[0]["status"] == "not_checked"
            silent = results(run("run", 1))[0]
            assert silent["status"] == "failed" and silent["reason"].endswith("tool_user: the server does not answer") and len(silent["tries"]) == 3, silent
            tool("cat")
            refused = run("run", 1)
            result = results(refused)[0]
            assert result["reason"].endswith("tool_user: the tool refused the password [redacted]") and not result.get("secret"), result
            environment = dict(line.split("=", 1) for line in result["module_outputs"]["environment"].splitlines() if "=" in line)
            assert environment["HOME"] == "/root" and environment["USER"] == "root" and environment["LOGNAME"] == "root" and \
                environment["LANG"] == "C.UTF-8", (connection, environment)
            assert password not in json.dumps(refused)
        for path in (workspace / ".mmx").rglob("*"):
            if path.is_file() and "runners" not in path.parts:
                assert password.encode() not in path.read_bytes(), path
        assert password not in remote("find /var/lib/mmx/jobs -type f -exec cat {} +")
        mark("a Starlark module that drives a tool: a missing program is not checked in a dryrun and fails a run once as a missing prerequisite; "
             "a server that does not answer is not checked in a dryrun and fails a run with retries; a command gets HOME=/root and LOGNAME=root "
             "over SSH and over HTTPS; a secret in a private value leaves the reason readable with [redacted] and reaches no record")

        (workspace / "summary.json").write_text(json.dumps({"passed": passed}, indent=2) + "\n")
        print(f"{len(passed)} external-module workflows passed", flush=True)
    finally:
        vm.ENV.pop("MMX_MODULE_TEST_SECRET", None)
        remote("systemctl disable --now mmx-module-follow 2>/dev/null; rm -f /etc/systemd/system/mmx-module-follow.service; systemctl daemon-reload")
        remote(f"rm -rf {base}")
        # Large fixtures and duplicated run assets are disposable after checking.
        shutil.rmtree(workspace / "external_modules", ignore_errors=True)
        shutil.rmtree(workspace / ".mmx", ignore_errors=True)


if __name__ == "__main__":
    vm.run_suite(main)
