"""Exercise live results and user-requested stops through the real CLI."""
import json
from pathlib import Path
import signal
import shlex
import subprocess
import time

import vm


def test(workspace, mmx, nodes, remote, mark):
    base = "/var/tmp/mmx-integration/control"
    processes = []
    service = "mmx-integration-control"

    def project(name, tasks, *, prepare=False):
        fleet_stages = []
        if prepare:
            fleet_stages.append({"id": "prepare", "nodes": "all", "node_tasks": [{"shell": {
                "script": f"echo prepared >> {base}/prepared"}}]})
        fleet_stages.append({"id": "update", "nodes": "all", "node_tasks": tasks})
        fleet_stages.append({"id": "next_fleet_stage", "nodes": "all", "node_tasks": [
            {"file": {"permissions_mode": "0644", "path": f"{base}/{name}-next-fleet-stage", "content": "done"}}]})
        vm.write_project(workspace, playbook_name='configure', **{
            "project_name": name, 'default_inventory': 'example', "max_nodes_at_once": 1,
            "fleet_stages": fleet_stages})

    def command(name, *args, expected=0):
        p = vm.cmd([mmx, name, *vm.playbook_args(name, "configure"), *args, "--json"], cwd=workspace, check=False)
        assert p.returncode == expected, (name, p.returncode, p.stdout, p.stderr)
        return json.loads(p.stdout)

    def start(name, human=False):
        out, err = workspace / f"{name}.stdout", workspace / f"{name}.stderr"
        stdout, stderr = out.open("w"), err.open("w")
        p = subprocess.Popen([str(mmx), "run", 'configure', *([] if human else ["--json"])],
                             cwd=workspace, env=vm.ENV, stdout=stdout, stderr=stderr)
        processes.append((p, stdout, stderr))
        return p, out, err

    def wait_for(check, description):
        deadline = time.monotonic() + 45
        while time.monotonic() < deadline:
            value = check()
            if value:
                return value
            time.sleep(.1)
        raise AssertionError("timed out: " + description)

    def exists(node, path):
        return remote(node, f"if test -f {path}; then echo yes; fi").strip() == "yes"

    def entered(name):
        wait_for(lambda: exists("a", f"{base}/{name}-entered"), name + " began on a")

    def release(name):
        for node in nodes:
            remote(node, f"touch {base}/{name}-release")

    def gate(name):
        return {"id": "wait", "shell": {"script": (
            f"touch {base}/{name}-entered\n"
            f"for ((i=0; i<600; i++)); do\n"
            f"  if test -f {base}/{name}-release; then echo finished >> {base}/{name}-count; exit 0; fi\n"
            "  sleep .1\ndone\nexit 1"), "changed_when": False}, "time_limit": "70s"}

    def entry(run, node="a"):
        return next(e for e in run["entries"] if e["fleet_stage"] == "update" and e["node"] == node)

    def acknowledged():
        r = command("status")
        return r if entry(r)["response"].get("stop_requested") else None

    def finished_status():
        r = command("status")
        return r if entry(r)["response"]["status"] != "running" else None

    def simple(name):
        project(name, [gate(name), {"file": {"permissions_mode": "0644", "path": f"{base}/{name}-later", "content": "done"}}])

    try:
        unit = ("[Service]\nType=oneshot\nRemainAfterExit=yes\n"
                f"ExecStart=/bin/cp {base}/config {base}/loaded\n"
                f"ExecReload=/bin/sh -c 'cat {base}/config > {base}/loaded; echo reload >> {base}/reloads'\n")
        for node in nodes:
            remote(node, f"mkdir -p {base}; printf 'old\\n' > {base}/config")
            remote(node, f"printf %s {shlex.quote(unit)} > /etc/systemd/system/{service}.service; "
                         f"systemctl daemon-reload; systemctl start {service}")

        name = "ctrl_c"
        project(name, [{
            "task_block": {
                "id": "configure",
                "changes": [{"id": "config", "file": {"permissions_mode": "0644", "path": f"{base}/config", "content": "new\n"}}, gate(name)],
                "validate": [{"shell": {"script": f'test "$(cat {base}/config)" = new', "changed_when": False}}],
                "activate": [{"systemd_service": {"name": service, "refresh": "reload", "state": "running"}}],
                "verify": [{"shell": {"script": f"cmp {base}/config {base}/loaded", "changed_when": False}}],
                "cleanup": [{"file": {"permissions_mode": "0644", "path": f"{base}/cleaned", "content": "done"}}],
            }
        },
            {"file": {"permissions_mode": "0644", "path": f"{base}/{name}-later", "content": "done"}}], prepare=True)
        p, out, err = start(name, human=True)
        entered(name)
        live = command("status")
        assert entry(live)["response"]["current_task"] == "configure/changes/wait", live
        assert entry(live)["response"]["results"][0]["change"] == "changed"
        wait_for(lambda: "configure/changes/wait" in err.read_text(), "live task is shown before completion")
        p.send_signal(signal.SIGINT)
        wait_for(lambda: "Press Ctrl-C again" in err.read_text(), "Ctrl-C instructions")
        wait_for(acknowledged, "remote stop acknowledgment")
        assert p.poll() is None, "Ctrl-C killed the task block"
        assert remote("a", f"cat {base}/loaded").strip() == "old"
        release(name)
        assert p.wait(timeout=30) == 130, out.read_text() + err.read_text()
        stopped = command("results")
        assert stopped["status"] == "stopped", stopped
        assert entry(stopped)["response"]["status"] == "stopped"
        assert not exists("a", f"{base}/{name}-later")
        assert not exists("a", f"{base}/{name}-next-fleet-stage")
        assert not exists("b", f"{base}/{name}-entered")
        assert exists("a", f"{base}/cleaned")
        assert remote("a", f"cat {base}/loaded").strip() == "new"
        assert "Resume unfinished work:" in out.read_text() and "mmx run" in out.read_text()
        command("resume")
        for node in nodes:
            assert remote(node, f"wc -l < {base}/prepared").strip() == "1"
            assert remote(node, f"wc -l < {base}/reloads").strip() == "1", "resume repeated a completed reload"
            assert exists(node, f"{base}/{name}-next-fleet-stage")
        mark("Ctrl-C explains stopping, finishes real service activation/cleanup, and resume skips completed work")

        name = "external_stop"
        simple(name)
        p, out, err = start(name)
        entered(name)
        request = command("stop")
        assert request["status"] == "stopping" and entry(request)["response"]["stop_requested"]
        assert p.poll() is None, "stop killed the standalone task"
        release(name)
        assert p.wait(timeout=30) == 1, out.read_text() + err.read_text()
        assert json.loads(out.read_text())["status"] == "stopped"
        assert not exists("a", f"{base}/{name}-later") and not exists("b", f"{base}/{name}-entered")
        mark("mmx stop defaults to the current run and stops active and queued work at task boundaries")

        name = "disconnected"
        # The admitted node may finish its entire fleet stage without a controller.
        simple(name)
        p, out, err = start(name)
        entered(name)
        p.kill()
        p.wait(timeout=10)
        release(name)
        observed = wait_for(finished_status, "status collects disconnected completion")
        assert entry(observed)["response"]["status"] == "succeeded", observed
        assert observed["status"] == "interrupted", observed
        assert exists("a", f"{base}/{name}-later") and not exists("a", f"{base}/{name}-next-fleet-stage")
        assert not exists("b", f"{base}/{name}-entered")
        command("resume")
        assert remote("a", f"wc -l < {base}/{name}-count").strip() == "1"
        mark("status collects work after controller loss without admitting nodes or fleet stages; resume skips that completed job")

        name = "detach"
        simple(name)
        p, out, err = start(name)
        entered(name)
        p.send_signal(signal.SIGINT)
        wait_for(acknowledged, "stop before detach")
        p.send_signal(signal.SIGINT)
        assert p.wait(timeout=10) == 130
        assert "Leaving the display" in err.read_text() and "mmx status" in err.read_text() and "mmx stop" in err.read_text()
        still_working = command("status")
        assert entry(still_working)["response"]["status"] == "running"
        release(name)
        observed = wait_for(finished_status, "stop completes after detach")
        assert observed["status"] == "stopped", observed
        assert not exists("a", f"{base}/{name}-later")
        mark("second Ctrl-C leaves clear follow-up commands while the acknowledged stop finishes remotely")

        name = "unconfirmed"
        project(name, [{"show": "partial result remains visible"}, gate(name),
                       {"file": {"permissions_mode": "0644", "path": f"{base}/{name}-later", "content": "done"}}])
        p, out, err = start(name)
        entered(name)
        wait_for(lambda: entry(command("results"))["response"].get("results"), "controller saves partial results")
        p.kill()
        p.wait(timeout=10)
        known = Path(nodes["a"]["ssh_known_hosts"])
        original = known.read_bytes()
        try:
            # With HTTPS stopped, fallback must still verify the SSH identity.
            remote("a", "systemctl stop mmx-https-api.service")
            known.write_text("")
            observed = command("stop", expected=3)
            assert observed["status"] == "stopping", observed
            assert entry(observed)["response"]["status"] == "unknown"
            assert entry(observed)["response"]["results"][0]["value"] == "partial result remains visible"
        finally:
            known.write_bytes(original)
        assert entry(command("stop"))["response"]["stop_requested"]
        release(name)
        observed = wait_for(finished_status, "repeated stop after contact restored")
        assert observed["status"] == "stopped", observed
        assert not exists("a", f"{base}/{name}-later")
        remote("a", "systemctl start mmx-https-api.service")
        mark("failed contact reports an unconfirmed stop, retains partial results, and accepts a repeated stop")

        do_not_manage(workspace, mmx, nodes, remote, mark, command, entry, exists)
    finally:
        for p, stdout, stderr in processes:
            if p.poll() is None:
                p.kill()
                p.wait(timeout=10)
            stdout.close()
            stderr.close()
        for node in nodes:
            remote(node, f"pkill -KILL -f '[m]mx-runner worker' || true; systemctl stop {service}.service 2>/dev/null || true; "
                         f"rm -f /etc/systemd/system/{service}.service; systemctl daemon-reload")


def do_not_manage(workspace, mmx, nodes, remote, mark, command, entry, exists):
    """/etc/mmx/do_not_manage on a refuses run, resume, and run-command jobs
    before their first node task; dryrun inspects and fails as the run would."""
    base = "/var/tmp/mmx-integration/control"
    marker = "/etc/mmx/do_not_manage"
    notice = "Barbara: kernel debugging until Friday, call +49 30 1234"
    refusal = f'this node refuses management because {marker} exists; the file says "{notice}". MMX never creates or removes this file'
    written = f"{base}/do_not_manage-written"
    vm.write_project(workspace, playbook_name="configure", **{
        "project_name": "do_not_manage", "default_inventory": "example", "max_nodes_at_once": 1,
        "fleet_stages": [
            {"id": "update", "nodes": "all", "on_node_failure": "stop_run",
             "node_tasks": [{"file": {"permissions_mode": "0644", "path": written, "content": "done\n"}}]},
            {"id": "next_fleet_stage", "nodes": "all",
             "node_tasks": [{"file": {"permissions_mode": "0644", "path": f"{base}/do_not_manage-next", "content": "done\n"}}]}]})

    def statuses(run):
        return {(e["fleet_stage"], e["node"]): e["response"]["status"] for e in run["entries"]}

    def untouched():
        for node in nodes:
            assert not exists(node, written) and not exists(node, f"{base}/do_not_manage-next"), node
        assert remote("a", f"cat {marker}").strip() == notice, "MMX changed the do_not_manage file"

    remote("a", f"mkdir -p /etc/mmx && printf '%s\\n' {shlex.quote(notice)} > {marker}")
    try:
        preview = command("dryrun", expected=1)
        refused = entry(preview, "a")["response"]
        assert refused["status"] == "failed" and refused["error"].startswith("a run would be refused: " + refusal), refused
        assert refused["results"][0]["change"] == "would_change", "dryrun did not inspect the refusing node"
        assert statuses(preview)[("update", "b")] == "stopped", preview
        untouched()
        mark("dryrun inspects a node with /etc/mmx/do_not_manage and fails it with the file's text, as the run would")

        failed = command("run", expected=1)
        refused = entry(failed, "a")["response"]
        assert refused["status"] == "failed" and refused["error"].startswith(refusal) and not refused.get("results"), refused
        assert statuses(failed) == {("update", "a"): "failed", ("update", "b"): "stopped",
                                    ("next_fleet_stage", "a"): "stopped", ("next_fleet_stage", "b"): "stopped"}, failed
        assert failed["status"] == "failed", failed
        human = vm.cmd([mmx, "results", failed["id"]], cwd=workspace, check=False).stdout
        assert "update / a: failed\n  " + refusal in human, human
        resumed = command("resume", expected=1)
        assert resumed["id"] == failed["id"] and entry(resumed, "a")["response"]["error"].startswith(refusal), resumed
        untouched()
        mark("run and resume fail a node with /etc/mmx/do_not_manage before its first task; stop_run applies as for any failure")

        adhoc = vm.cmd([mmx, "run-command", "--json", "--nodes", "all", "--", "touch", f"{base}/do_not_manage-adhoc"], cwd=workspace, check=False)
        assert adhoc.returncode == 1, adhoc.stdout + adhoc.stderr
        adhoc_entries = {e["node"]: e["response"] for e in json.loads(adhoc.stdout)["entries"]}
        assert adhoc_entries["a"]["status"] == "failed" and adhoc_entries["a"]["error"].startswith(refusal), adhoc_entries
        assert adhoc_entries["b"]["status"] == "succeeded", adhoc_entries
        assert not exists("a", f"{base}/do_not_manage-adhoc") and exists("b", f"{base}/do_not_manage-adhoc")
        mark("run-command is refused on the node with /etc/mmx/do_not_manage and runs on the others")
    finally:
        remote("a", f"rm -f {marker}")

    finished = command("resume")
    assert finished["id"] == failed["id"] and finished["status"] == "succeeded", finished
    for node in nodes:
        assert exists(node, written) and exists(node, f"{base}/do_not_manage-next"), node
    again = command("run")
    assert again["status"] == "succeeded" and all(e["response"]["change"] == "unchanged" for e in again["entries"]), again
    mark("after the do_not_manage file is removed, resume finishes the refused work and the next run succeeds")
