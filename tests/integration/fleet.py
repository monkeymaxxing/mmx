#!/usr/bin/env python3
"""Public CLI workflows on two independent Debian VMs."""
import json
import shlex
import time
from concurrent.futures import ThreadPoolExecutor

import vm
import job_control


def main():
    nodes, connections = {}, {}
    for node_id, name in [("a", "mmx-dev"), ("b", "mmx-peer")]:
        vm.up(name)
        nodes[node_id], connections[node_id] = vm.connection(name)
        vm.reset(name)
    workspace = vm.ROOT / "fleet" / time.strftime("%Y%m%d-%H%M%S")
    workspace.mkdir(parents=True)
    vm.write_inventory(workspace, {"nodes": nodes}, name='example')
    vm.build()
    mmx = vm.ROOT / "bin/mmx"
    passed = []

    def remote(node, script):
        return vm.cmd([*connections[node], "sudo -n /bin/sh -c " + shlex.quote(script)]).stdout

    def project(fleet_stages, limit=2, name="fleet_acceptance", **fields):
        vm.write_project(workspace, playbook_name='configure', **{
            "project_name": name, 'default_inventory': 'example', "max_nodes_at_once": limit,
            "fleet_stages": fleet_stages, **fields})

    def run(command, expected=None, *args):
        p = vm.cmd([mmx, command, *vm.playbook_args(command, "configure"), "--json", *args], cwd=workspace, check=False)
        assert p.returncode in (vm.success_codes(command) if expected is None else (expected,)), f"{command}: {p.returncode}\n{p.stdout}\n{p.stderr}"
        return json.loads(p.stdout)

    def mark(name):
        passed.append(name)
        print("PASS " + name, flush=True)

    # Each node job waits until the test releases it, and the test releases
    # both only after both have started. So the run passes only when MMX runs
    # the two node jobs at once, however long each fresh node's setup takes.
    waiting_for_release = {"id": "concurrent", "nodes": "all", "node_tasks": [{"shell": {
        "script": "touch /var/tmp/mmx-integration/started; waited=0; "
                  "until test -e /var/tmp/mmx-integration/release; do "
                  "test $waited -lt 480 || exit 1; sleep 0.5; waited=$((waited + 1)); done",
        "changed_when": False}}]}
    project([waiting_for_release])
    started = set()
    with ThreadPoolExecutor(1) as executor:
        running = executor.submit(run, "run")
        deadline = time.monotonic() + 240
        while started != set(nodes) and not running.done() and time.monotonic() < deadline:
            time.sleep(1)
            started = {h for h in nodes if remote(h, "test -e /var/tmp/mmx-integration/started && echo yes || true").strip()}
        for h in nodes:
            remote(h, "touch /var/tmp/mmx-integration/release")
        running.result()
    assert started == set(nodes), f"only {sorted(started)} started while the other node job waited for its release"
    mark("direct run bootstraps fresh nodes and two independent VMs execute concurrently")
    timing = {"id": "timed", "nodes": "all", "node_tasks": [{"shell": {
        "script": "date +%s%N > /var/tmp/mmx-integration/start; sleep 2; date +%s%N > /var/tmp/mmx-integration/end",
        "changed_when": False}}]}
    project([timing], limit=1)
    run("run")
    # The VM clocks can differ by tens of milliseconds, more than the gap between
    # serial jobs. Overlapping 2-second jobs would start b about 2 seconds early.
    assert int(remote("a", "cat /var/tmp/mmx-integration/end")) - 1_000_000_000 <= int(remote("b", "cat /var/tmp/mmx-integration/start"))
    mark("max_nodes_at_once=1 serializes real node jobs")

    remote("a", "touch /var/tmp/mmx-integration/fail-setup")
    fleet_stages = [
        {"id": "setup", "nodes": "all", "node_tasks": [{"id": "check", "shell": {
            "script": "echo attempt >> /var/tmp/mmx-integration/setup-count; test ! -f /var/tmp/mmx-integration/fail-setup"}}]},
        {"id": "local", "nodes": "all", "requires": {"same_node": ["setup"]},
         "node_tasks": [{"file": {"permissions_mode": "0644", "path": "/var/tmp/mmx-integration/local", "content": "ready"}}]},
        {"id": "shared", "nodes": "all", "requires": {"all_nodes": ["setup"]},
         "node_tasks": [{"file": {"permissions_mode": "0644", "path": "/var/tmp/mmx-integration/shared", "content": "ready"}}]},
        {"id": "independent", "nodes": "all", "node_tasks": [{"shell": {
            "script": "echo attempt >> /var/tmp/mmx-integration/independent-count"}}]},
    ]
    project(fleet_stages)
    failed = run("run", 1)
    status = {(e["fleet_stage"], e["node"]): e["response"]["status"] for e in failed["entries"]}
    assert status["setup", "a"] == "failed" and status["setup", "b"] == "succeeded"
    assert status["local", "a"] == "blocked" and status["local", "b"] == "succeeded"
    assert status["shared", "a"] == status["shared", "b"] == "blocked"
    assert status["independent", "a"] == "stopped" and status["independent", "b"] == "succeeded"
    remote("a", "test ! -e /var/tmp/mmx-integration/local; test ! -e /var/tmp/mmx-integration/shared; test ! -e /var/tmp/mmx-integration/independent-count")
    remote("b", "test -f /var/tmp/mmx-integration/local; test ! -e /var/tmp/mmx-integration/shared")
    remote("a", "rm /var/tmp/mmx-integration/fail-setup")
    recovered = run("resume")
    assert recovered["id"] == failed["id"]
    assert remote("a", "wc -l < /var/tmp/mmx-integration/setup-count").strip() == "2"
    assert remote("b", "wc -l < /var/tmp/mmx-integration/setup-count").strip() == "1"
    for h in nodes:
        remote(h, "test -f /var/tmp/mmx-integration/local; test -f /var/tmp/mmx-integration/shared")
        assert remote(h, "wc -l < /var/tmp/mmx-integration/independent-count").strip() == "1"
    mark("node and fleet prerequisites block correctly; resume skips completed nodes and fleet stages")
    inspected = run("results", 0, "--node", "a", "--fleet-stage", "setup", "--task", "check")
    assert len(inspected["entries"]) == 1
    assert inspected["entries"][0]["response"]["results"][0]["task"] == "check"
    assert "bundle" not in inspected["entries"][0]
    mark("results filters expose task results without dumping the frozen execution bundle")

    # A stopped admission policy does not interrupt work already admitted.
    remote("a", "touch /var/tmp/mmx-integration/fail-setup")
    fleet_stages[0]["on_node_failure"] = "stop_run"
    project(fleet_stages, limit=1, name="stop_acceptance")
    stopped = run("run", 1)
    unstarted = {(e["fleet_stage"], e["node"]): e for e in stopped["entries"]}
    for key in [("setup", "b"), ("independent", "a")]:
        assert unstarted[key]["node_job_attempt"] == 0 and unstarted[key]["response"]["status"] == "stopped", unstarted[key]
        assert unstarted[key]["response"]["error"] == "not started: on_node_failure stop_run after setup failed on a"
    remote("a", "rm /var/tmp/mmx-integration/fail-setup")
    run("resume")
    mark("stop_run prevents later admission; recorded resume resumes unfinished work")

    # failed_nodes_allowed: within the allowance the fleet stage continues; one more
    # failed node stops the run exactly as stop_run does without one.
    allowance = [
        {"id": "setup", "nodes": "all", "on_node_failure": "stop_run", "failed_nodes_allowed": 1, "node_tasks": [{"shell": {
            "script": "test ! -f /var/tmp/mmx-integration/fail-setup"}}]},
        {"id": "after", "nodes": "all", "node_tasks": [{"shell": {"script": "touch /var/tmp/mmx-integration/after"}}]},
    ]
    remote("a", "touch /var/tmp/mmx-integration/fail-setup")
    project(allowance, limit=1, name="allowance_acceptance")
    under = run("run", 1)
    status = {(e["fleet_stage"], e["node"]): e["response"]["status"] for e in under["entries"]}
    assert status == {("setup", "a"): "failed", ("setup", "b"): "succeeded", ("after", "a"): "stopped", ("after", "b"): "succeeded"}, status
    remote("a", "test ! -e /var/tmp/mmx-integration/after")
    remote("b", "test -f /var/tmp/mmx-integration/after && rm /var/tmp/mmx-integration/after")
    allowance[0]["failed_nodes_allowed"] = "50%"
    project(allowance, limit=1, name="allowance_acceptance")
    assert run("explain")["fleet_stages"][0]["allowed_failures"] == 1
    remote("b", "touch /var/tmp/mmx-integration/fail-setup")
    over = run("run", 1)
    entries = {(e["fleet_stage"], e["node"]): e for e in over["entries"]}
    assert [entries["setup", h]["response"]["status"] for h in ("a", "b")] == ["failed", "failed"]
    for h in ("a", "b"):
        assert entries["after", h]["node_job_attempt"] == 0 and entries["after", h]["response"]["error"] == \
            "not started: on_node_failure stop_run after setup failed on b (2 failed nodes; failed_nodes_allowed 50% allows 1)", entries["after", h]
        remote(h, "test ! -e /var/tmp/mmx-integration/after; rm /var/tmp/mmx-integration/fail-setup")
    run("resume")
    for h in ("a", "b"):
        remote(h, "test -f /var/tmp/mmx-integration/after")
    mark("failed_nodes_allowed continues the fleet stage within its count and stops the run past its percentage; resume finishes")

    controller_machine_files(workspace, nodes, remote, project, run, mark)
    targeting(workspace, mmx, nodes, remote, project, run, mark)
    selection(workspace, mmx, nodes, remote, project, run, mark)
    variables(workspace, mmx, nodes, remote, project, run, mark)
    ad_hoc(workspace, mmx, nodes, remote, run, mark)
    job_control.test(workspace, mmx, nodes, remote, mark)
    variable_sources(workspace, mmx, nodes, remote, project, run, mark)
    rolling_reboot(workspace, nodes, connections, remote, project, run, mark)
    helper_loop(workspace, mmx, nodes, remote, project, run, mark)
    fact_conditions(workspace, nodes, remote, project, run, mark)

    (workspace / "results.json").write_text(json.dumps({"passed": passed}, indent=2))
    print(f"{len(passed)} fleet workflows passed; records: {workspace}", flush=True)


def controller_machine_files(workspace, nodes, remote, project, run, mark):
    """save_on_controller_machine writes outputs to files on the controller machine."""
    fail = "/var/tmp/mmx-integration/fail-report"
    (workspace / "summary.tmpl").write_text('{{ range $node, $o := .outputs.collect_hardware_report }}{{ $node }}: {{ $o.hardware_report }}{{ end }}')
    project([{"id": "collect_hardware_report", "nodes": "all", "node_tasks": [
        {"shell": {"script": f"test ! -e {fail}", "dryrun": "true", "changed_when": False}},
        {"id": "hardware", "shell": {"script": "printf 'machine %s\\n' \"$(cat /etc/machine-id)\"",
                                     "dryrun": "printf 'machine %s\\n' \"$(cat /etc/machine-id)\"", "changed_when": False}}],
        "outputs": {"hardware_report": "{{ .node_tasks.hardware.stdout }}"},
        "save_on_controller_machine": [
            {"output": "hardware_report", "path": "server_details/{{ .node.name }}.txt", "permissions_mode": "0644"},
            {"template": "summary.tmpl", "path": "server_details/summary.txt", "permissions_mode": "0640"}]}],
        name="controller_machine_files")
    details = workspace / "server_details"
    expected = {h: f"machine {remote(h, 'cat /etc/machine-id').strip()}\n" for h in nodes}

    def saved(record):
        files = record["selections"]["collect_hardware_report"].get("controller_machine_files", [])
        return {(f["save_on_controller_machine_index"], f.get("node", "")): f["result"] for f in files}

    def check_files():
        for h in nodes:
            assert (details / f"{h}.txt").read_text() == expected[h]
            assert (details / f"{h}.txt").stat().st_mode & 0o777 == 0o644
        assert (details / "summary.txt").read_text() == "".join(f"{h}: {expected[h]}" for h in sorted(nodes))
        assert (details / "summary.txt").stat().st_mode & 0o777 == 0o640

    first = saved(run("run"))
    check_files()
    assert {k: (r["status"], r["change"]) for k, r in first.items()} == {
        (0, "a"): ("succeeded", "changed"), (0, "b"): ("succeeded", "changed"), (1, ""): ("succeeded", "changed")}, first
    again = saved(run("run"))
    assert all(r["change"] == "unchanged" for r in again.values()), again
    mark("save_on_controller_machine writes one file per node and one from a template; an unchanged rerun changes nothing")

    (details / "a.txt").write_text("edited by hand\n")
    (details / "summary.txt").unlink()
    preview = saved(run("dryrun"))
    assert preview[0, "a"]["change"] == "would_change" and "+" + expected["a"].strip() in preview[0, "a"]["file_change"]["diff"], preview
    assert preview[0, "b"]["change"] == "unchanged" and preview[1, ""]["file_change"]["action"] == "create", preview
    assert (details / "a.txt").read_text() == "edited by hand\n" and not (details / "summary.txt").exists()
    mark("dryrun reports each controller machine file with its difference and writes nothing")

    for f in details.iterdir():
        f.unlink()
    remote("b", f"touch {fail}")
    failed = run("run", 1)
    files = saved(failed)
    assert (0, "b") not in files and files[1, ""]["status"] == "blocked", files
    assert (details / "a.txt").read_text() == expected["a"] and not (details / "b.txt").exists()
    remote("b", f"rm {fail}")
    resumed = run("resume")
    assert resumed["id"] == failed["id"] and resumed["status"] == "succeeded", resumed["status"]
    check_files()
    mark("mmx resume writes the controller machine files that are still missing")


def variable_sources(workspace, mmx, nodes, remote, project, run, mark):
    """One task installs a different file for each node and for each loop item."""
    sites = {"a": "frankfurt", "b": "helsinki"}
    vm.write_inventory(workspace, {"nodes": {h: dict(nodes[h], vars={"site": sites[h]}) for h in nodes}}, name='example')
    base = "/var/tmp/mmx-integration/variable"
    for directory in ("haproxy", "files"):
        (workspace / directory).mkdir(exist_ok=True)
    for site in sites.values():
        (workspace / f"haproxy/{site}.cfg.tmpl").write_text(f"site {site} on {{{{ .node.name }}}} with {{{{ .node.cpu.count }}}} CPUs\n")
        (workspace / "trees" / site).mkdir(parents=True, exist_ok=True)
        (workspace / "trees" / site / (site + ".txt")).write_text(site + "\n")
    for h in nodes:
        (workspace / f"files/{h}.motd").write_text(f"this is {h}\n")
    for person in ("alice", "bob"):
        (workspace / f"files/{person}.pub").write_text(person + " key\n")
    project([{"id": "files", "nodes": "all", "node_tasks": [
        {"directory": {"path": base}},
        {"file": {"permissions_mode": "0644", "path": base + "/haproxy.cfg", "template": "haproxy/{{ .vars.site }}.cfg.tmpl"}},
        {"file": {"permissions_mode": "0644", "path": base + "/motd", "source": "files/{{ .node.name }}.motd"}},
        {"loop": ["alice", "bob"], "as": "person",
         "file": {"permissions_mode": "0644", "path": base + "/{{ .person }}.pub", "source": "files/{{ .person }}.pub"}},
        {"directory": {"path": base + "/tree", "source": "trees/{{ .vars.site }}"}}]}], name="variable_sources")
    try:
        run("dryrun")
        for h in nodes:
            remote(h, "test ! -e " + base)
        applied = run("run")
        for h, other in (("a", "b"), ("b", "a")):
            cpus = remote(h, "nproc").strip()
            assert remote(h, f"cat {base}/haproxy.cfg") == f"site {sites[h]} on {h} with {cpus} CPUs\n"
            assert remote(h, f"cat {base}/motd {base}/alice.pub {base}/bob.pub") == f"this is {h}\nalice key\nbob key\n"
            assert remote(h, f"ls {base}/tree").split() == [sites[h] + ".txt"]
            # The node's job named its own site's template and never the other's.
            job = next(e["job_id"] for e in applied["entries"] if e["node"] == h)
            request = remote(h, f"cat /var/lib/mmx/jobs/{job}/request.json")
            assert f"haproxy/{sites[h]}.cfg.tmpl" in request and f"haproxy/{sites[other]}.cfg.tmpl" not in request
            assert f"files/{h}.motd" in request and f"files/{other}.motd" not in request
        assert all(e["response"]["change"] == "unchanged" for e in run("run")["entries"])
        # A node whose file is missing stops the run before any node is reached.
        (workspace / "files/b.motd").unlink()
        (workspace / "files/a.motd").write_text("changed\n")
        refused = vm.cmd([mmx, "run", "configure", "--json"], cwd=workspace, check=False)
        assert refused.returncode == 4 and "node b" in refused.stderr and "file.source" in refused.stderr and "b.motd" in refused.stderr, refused.stderr
        assert remote("a", f"cat {base}/motd") == "this is a\n"
        mark("a variable template, source, and directory path installs each node's and each loop item's own file; a missing file stops the run before any node is reached")
    finally:
        vm.write_inventory(workspace, {"nodes": nodes}, name='example')


def rolling_reboot(workspace, nodes, connections, remote, project, run, mark):
    """Patch and reboot one node at a time; the next fleet stage runs on the new boots."""
    boot = "cat /proc/sys/kernel/random/boot_id"
    before = {h: remote(h, boot).strip() for h in nodes}
    project([
        {"id": "patch", "nodes": "all", "max_nodes_at_once": 1, "on_node_failure": "stop_fleet_stage", "node_tasks": [
            {"shell": {"script": "date +%s%N > /var/tmp/mmx-integration/patch-started"}},
            {"id": "restart", "reboot": {}}]},
        {"id": "verify", "nodes": "all", "requires": {"same_node": ["patch"]}, "node_tasks": [
            {"shell": {"script": "cat /proc/sys/kernel/random/boot_id > /var/tmp/mmx-integration/verified-boot"}}]},
    ], name="rolling_reboot")
    result = {(e["fleet_stage"], e["node"]): e["response"] for e in run("run")["entries"]}
    for h, name in (("a", "mmx-dev"), ("b", "mmx-peer")):
        nodes[h], connections[h] = vm.await_boot(before[h], name)
    vm.write_inventory(workspace, {"nodes": nodes}, name='example')
    for h in nodes:
        after = remote(h, boot).strip()
        restart = result["patch", h]["results"][-1]
        assert restart["status"] == "succeeded" and restart["value"] == {"boot_id_before": before[h], "boot_id_after": after}, restart
        assert result["verify", h]["status"] == "succeeded" and remote(h, "cat /var/tmp/mmx-integration/verified-boot").strip() == after
    # b's patch started only after a answered on its new boot.
    booted_a = int(remote("a", "awk '/^btime/ {print $2}' /proc/stat"))
    started_b = int(remote("b", "cat /var/tmp/mmx-integration/patch-started")) // 1_000_000_000
    assert started_b > booted_a, (booted_a, started_b)
    mark("max_nodes_at_once=1 reboots one node at a time; the next fleet stage runs on each new boot")


def helper_loop(workspace, mmx, nodes, remote, project, run, mark):
    """A for_each_node loop whose iteration for each node works on the other one."""
    base = "/var/tmp/mmx-integration/loop"
    vm.write_inventory(workspace, {"nodes": nodes, "groups": {"servers": {"nodes": ["a", "b"]}}}, name="example")
    for h in nodes:
        remote(h, f"rm -rf {base}; mkdir -p {base}")
    helper = "{{ .vars.helper_of[.server] }}"
    env = {"S": "{{ .server }}", "D": base, "R": "{{ .vars.release }}"}
    needed = "{{ .outputs.inspect[.server].update_needed }}"

    # Each fleet stage prints a marker that becomes its output, and the next
    # fleet stage logs the marker it received. A marker reaches the next fleet
    # stage only after the fleet stage that printed it succeeded, so the logs
    # prove the order without comparing two servers' clocks.
    # A dryrun prints the same marker and changes nothing.
    def stage(id, nodes, script, previous=None, **fields):
        stage_env = dict(env, P="{{ " + previous + " }}") if previous else env
        marker = 'printf %s-%s "' + {"drain": "drained", "update": "updated", "rejoin": "rejoined"}[id] + '" "$S"'
        return {"id": id, "nodes": nodes, **fields, "node_tasks": [{"id": "step", "shell": {"script": script + "; " + marker, "dryrun": marker, "env": stage_env}}],
                "outputs": {"marker": "{{ .node_tasks.step.stdout }}"}}

    def loop(**settings):
        return {"id": "roll", "for_each_node": "servers", "as": "server", "on_node_failure": "stop_run", **settings, "fleet_stages": [
            {"id": "inspect", "nodes": "{{ .server }}", "node_tasks": [{"id": "installed", "shell": {
                "script": f"cat {base}/release 2>/dev/null || true", "dryrun": f"cat {base}/release 2>/dev/null || true", "changed_when": False}}],
             "outputs": {"update_needed": {"not": {"equals": ["{{ .node_tasks.installed.stdout }}", "{{ .vars.release }}"]}}}},
            stage("drain", helper, 'echo "$(date +%s%N) drain $S" >> $D/log; touch $D/drained-$S', when=needed),
            stage("update", "{{ .server }}", 'test ! -f $D/fail-update || exit 1; echo "$(date +%s%N) update $S after $P" >> $D/log; printf %s "$R" > $D/release',
                  ".outputs.drain[.vars.helper_of[.server]].marker", when=needed),
            stage("rejoin", helper, 'echo "$(date +%s%N) rejoin $S after $P" >> $D/log; rm $D/drained-$S',
                  ".outputs.update[.server].marker", when=needed)]}

    def write(**settings):
        project([loop(**settings),
                 {"id": "report", "nodes": "servers", "when": {"not": {"equals": ["{{ .node.name }}", "a"]}},
                  "node_tasks": [{"file": {"permissions_mode": "0644", "path": f"{base}/report", "content": "reported\n"}}]}],
                name="helper_loop", vars={"helper_of": {"a": "b", "b": "a"}}, command_line_vars={"release": {"type": "string"}})

    def log(node):
        return [line.split(" ", 1) for line in remote(node, f"cat {base}/log 2>/dev/null || true").splitlines()]

    def at(node, action):
        """The times, on node's own clock, of the log lines that begin with action."""
        return [int(t) for t, what in log(node) if what == action or what.startswith(action + " ")]

    def release(node):
        return remote(node, f"cat {base}/release 2>/dev/null || echo absent").strip()

    def raw(*args):
        return vm.cmd([mmx, args[0], "configure", *args[1:]], cwd=workspace, check=False)

    write()
    run("dryrun", None, "--var", "release=v1")
    for h in nodes:
        assert log(h) == [] and release(h) == "absent", (h, log(h))
    mark("for_each_node dryrun previews every iteration and changes nothing on the loop nodes or their helper nodes")

    first = run("run", 0, "--var", "release=v1")
    assert release("a") == release("b") == "v1"
    for server, helper_node in (("a", "b"), ("b", "a")):
        # update received drain's marker and rejoin update's: drain, update, rejoin.
        drain, update = at(helper_node, "drain " + server), at(server, f"update {server} after drained-{server}")
        rejoin = at(helper_node, f"rejoin {server} after updated-{server}")
        assert len(drain) == len(update) == len(rejoin) == 1, (server, log("a"), log("b"))
        remote(helper_node, f"test ! -e {base}/drained-{server}")
    # One node at a time: b's iteration starts after a's ended. Each check
    # compares two lines written on the same server, so on one clock.
    assert at("a", "update a")[0] < at("a", "drain b")[0], log("a")
    assert at("b", "rejoin a")[0] < at("b", "update b")[0], log("b")
    entries = {(e["fleet_stage"], e["node"]): e["response"] for e in first["entries"]}
    assert entries["report", "a"]["status"] == "skipped" and entries["report", "a"]["error"].startswith("when is false: "), entries["report", "a"]
    remote("a", f"test ! -e {base}/report")
    remote("b", f"test -f {base}/report")
    mark("for_each_node runs each node's drain, update, and rejoin in order, on its helper node and itself; a fleet stage's when skips a node without a node job")

    again = run("run", 0, "--var", "release=v1")
    statuses = {(e["fleet_stage"], e["node"]): e["response"]["status"] for e in again["entries"]}
    assert all(statuses[f"roll[{s}]/{id}", n] == "skipped" for s, h in (("a", "b"), ("b", "a")) for id, n in (("drain", h), ("update", s), ("rejoin", h))), statuses
    assert len(log("a")) == len(log("b")) == 3
    mark("an unchanged rerun checks each node and skips its helper-node work through the controller's per-node when")

    limited = raw("run", "--limit", "b", "--var", "release=v2")
    assert limited.returncode == 0, (limited.stdout, limited.stderr)
    assert "Nodes outside --limit that help selected nodes\n  a   roll[b]/drain, roll[b]/rejoin\n" in limited.stderr, limited.stderr
    assert release("a") == "v1" and release("b") == "v2"
    assert len(at("a", "drain b")) == len(at("a", "rejoin b")) == 2 and len(at("b", "drain a")) == 1, (log("a"), log("b"))
    remote("a", f"test ! -e {base}/drained-b")
    mark("--limit b runs only b's iteration and still drains and rejoins b on the helper node a, listed before connecting")

    remote("a", f"touch {base}/fail-update")
    failed = run("run", 1, "--var", "release=v3")
    entries = {(e["fleet_stage"], e["node"]): e for e in failed["entries"]}
    assert entries["roll[a]/update", "a"]["response"]["status"] == "failed"
    assert entries["roll[a]/rejoin", "b"]["node_job_attempt"] == 0 and entries["roll[a]/rejoin", "b"]["response"]["status"] == "stopped"
    iteration = failed["selections"]["roll"]["iterations"]["b"]
    assert iteration["status"] == "stopped" and iteration["error"] == "not started: on_node_failure stop_run after roll failed on a", iteration
    remote("b", f"test -f {base}/drained-a")  # the failed node stays out of the helper's pool
    assert release("b") == "v2" and len(at("a", "drain b")) == 2
    remote("a", f"rm {base}/fail-update")
    resumed = run("resume")
    assert resumed["id"] == failed["id"] and resumed["status"] == "succeeded"
    entries_after = {(e["fleet_stage"], e["node"]): e for e in resumed["entries"]}
    assert entries_after["roll[a]/drain", "b"]["job_id"] == entries["roll[a]/drain", "b"]["job_id"]
    assert len(at("b", "drain a")) == 2 and release("a") == release("b") == "v3"
    remote("b", f"test ! -e {base}/drained-a")
    mark("a failed update ends that loop node's iteration and stop_run keeps the next from starting; resume continues from the failed fleet stage")

    write(max_nodes_at_once="100%")
    together = run("run", 0, "--var", "release=v4")
    assert together["status"] == "succeeded" and release("a") == release("b") == "v4"
    for h in nodes:
        remote(h, f"test ! -e {base}/drained-a; test ! -e {base}/drained-b")
    mark("two loop nodes in their sequences at once take turns on each other as helper nodes")
    vm.write_inventory(workspace, {"nodes": nodes}, name="example")


def fact_conditions(workspace, nodes, remote, project, run, mark):
    """A fleet stage's when that reads node facts, checked before each node starts."""
    base = "/var/tmp/mmx-integration/facts"
    for h in nodes:
        remote(h, f"rm -rf {base}; mkdir -p {base}")
    # The hostname differs between the two servers; the distribution does not.
    host_a = remote("a", "cat /proc/sys/kernel/hostname").strip()
    distribution = remote("a", ". /etc/os-release; echo $ID").strip()
    assert remote("b", "cat /proc/sys/kernel/hostname").strip() != host_a

    def marker(name):
        return [{"file": {"permissions_mode": "0644", "path": f"{base}/{name}", "content": "x\n"}}]

    project([
        {"id": "only_a", "nodes": "all", "when": {"equals": ["{{ .node.hostname }}", host_a]}, "node_tasks": marker("only_a")},
        {"id": "this_distribution", "nodes": "all", "when": {"equals": ["{{ .node.distribution.id }}", distribution]}, "node_tasks": marker("distribution")},
        {"id": "other_distribution", "nodes": "all", "when": {"equals": ["{{ .node.distribution.id }}", "no-such-distribution"]}, "node_tasks": marker("other")},
        {"id": "roll", "for_each_node": "all", "as": "server", "when": {"not": {"equals": ["{{ .node.hostname }}", host_a]}},
         "fleet_stages": [{"id": "mark", "nodes": "{{ .server }}", "node_tasks": marker("loop")}]},
    ], name="fact_conditions")

    def present(node):
        return set(remote(node, f"ls {base}").split())

    preview = run("dryrun")
    statuses = {(e["fleet_stage"], e["node"]): e["response"]["status"] for e in preview["entries"]}
    assert statuses[("only_a", "b")] == statuses[("other_distribution", "a")] == "skipped", statuses
    for h in nodes:
        assert present(h) == set(), (h, present(h))
    mark("dryrun checks a when that reads node facts on each node and changes nothing")

    applied = run("run")
    entries = {(e["fleet_stage"], e["node"]): e["response"] for e in applied["entries"]}
    for key in [("only_a", "b"), ("other_distribution", "a"), ("other_distribution", "b")]:
        assert entries[key]["status"] == "skipped" and entries[key]["error"].startswith("when is false: "), (key, entries[key])
        assert entries[key]["facts"]["values"]["distribution"]["id"] == distribution, entries[key]
        block_devices = entries[key]["facts"]["values"]["storage"]["block_devices"]
        assert isinstance(block_devices, list), block_devices
        for device in block_devices:
            assert all(field in device for field in ("name", "path", "kind", "size_bytes")), device
            assert device["size_bytes"] > 0, device
    assert applied["selections"]["roll"]["iterations"]["a"]["status"] == "skipped", applied["selections"]["roll"]
    assert present("a") == {"only_a", "distribution"} and present("b") == {"distribution", "loop"}, (present("a"), present("b"))
    unchanged = run("run")
    assert all(e["response"].get("change", "unchanged") == "unchanged" for e in unchanged["entries"]), unchanged["entries"]
    mark("a when on a fleet stage or for_each_node loop reads each node's facts just before it starts; a false one starts no node job")

    project([
        {"id": "only_a", "nodes": "all", "when": {"equals": ["{{ .node.hostname }}", host_a]}, "outputs": {"name": "{{ .node.name }}"}},
        {"id": "roll", "for_each_node": "all", "as": "server", "when": {"not": {"equals": ["{{ .node.hostname }}", host_a]}},
         "fleet_stages": [{"id": "mark", "nodes": "{{ .server }}", "node_tasks": [{"show": "{{ .server }}"}]}], "outputs": {"server": "{{ .server }}"}},
        {"id": "read_collections", "nodes": "all", "node_tasks": [{"show": "{{ .outputs.only_a }}"}, {"show": "{{ .outputs.roll }}"}]},
        {"id": "read_the_skipped_entry", "nodes": "a", "node_tasks": [{"show": "{{ .outputs.only_a.b.name }}"}]},
    ], name="skipped_outputs")
    collected = run("run", 1)
    entries = {(e["fleet_stage"], e["node"]): e["response"] for e in collected["entries"]}
    for h in nodes:
        read = entries[("read_collections", h)]
        assert read["status"] == "succeeded", (h, read)
        assert [r["value"] for r in read["results"]] == [{"a": {"name": "a"}}, {"b": {"server": "b"}}], (h, read["results"])
    skipped_entry = entries[("read_the_skipped_entry", "a")]
    assert skipped_entry["status"] == "blocked" and "only_a on b is skipped: when is false: " in skipped_entry["error"], skipped_entry
    mark("a whole output collection leaves out the node and the for_each_node iteration that a when skipped; a read of the skipped node's entry is blocked")


def targeting(workspace, mmx, nodes, remote, project, run, mark):
    """--inventory, --limit, mmx nodes, per-inventory defaults, and short IDs."""
    vm.write_inventory(workspace, {"nodes": nodes, "groups": {"canary": {"nodes": ["a"]}, "rest": {"nodes": ["b"]}}}, name='targets')
    vm.write_inventory(workspace, {"nodes": {"b": nodes["b"]}, "groups": {"rest": {"nodes": ["b"]}}}, name='staging')
    target = "/var/tmp/mmx-integration/target"
    runs = workspace / ".mmx" / "runs"

    def raw(*args):
        return vm.cmd([mmx, args[0], *vm.playbook_args(args[0]), *args[1:]], cwd=workspace, check=False)

    def deployed(node):
        return remote(node, f"cat {target} 2>/dev/null || echo absent").strip()

    project([
        {"id": "prepare", "nodes": "rest", "node_tasks": [{"file": {"permissions_mode": "0644", "path": "/var/tmp/mmx-integration/prepared", "content": "x"}}]},
        {"id": "deploy", "nodes": "all", "requires": {"all_nodes": ["prepare"]},
         "node_tasks": [{"file": {"permissions_mode": "0644", "path": target, "content": "v1"}}]}], name="targeting")
    before = len(list(runs.iterdir()))
    for command in ("nodes", "run"):
        p = raw(command, "--json", "--inventory", "targets", "--limit", "canary")
        assert p.returncode == 4 and "--limit excludes fleet stage prepare, which fleet stage deploy requires" in p.stderr, (command, p.returncode, p.stderr)
    assert len(list(runs.iterdir())) == before
    for h in nodes:
        assert deployed(h) == "absent"
        remote(h, "test ! -e /var/tmp/mmx-integration/prepared")
    mark("nodes and run reject a limit that leaves out a required fleet stage, naming both, before any remote work")

    remote("a", "touch /var/tmp/mmx-integration/fail-deploy")
    project([
        {"id": "deploy", "nodes": "all", "node_tasks": [
            {"shell": {"script": "test ! -f /var/tmp/mmx-integration/fail-deploy",
                       "dryrun": "test ! -f /var/tmp/mmx-integration/fail-deploy", "changed_when": False}},
            {"file": {"permissions_mode": "0644", "path": target, "content": "v1"}}]},
        {"id": "report", "nodes": "rest", "node_tasks": [{"show": "not for the canary"}]}], name="targeting")
    shown = json.loads(raw("nodes", "--json", "--inventory", "targets", "--limit", "canary").stdout)
    fleet_stages = {s["fleet_stage"]: s for s in shown["fleet_stages"]}
    assert shown["limit"] == ["canary"] and [h["name"] for h in shown["nodes"]] == ["a", "b"]
    assert fleet_stages["deploy"]["nodes"] == ["a"] and fleet_stages["deploy"]["left_out"] == ["b"]
    assert "nodes" not in fleet_stages["report"] and fleet_stages["report"]["left_out"] == ["b"]
    failed = run("run", 1, "--inventory", "targets", "--limit", "canary")
    assert failed["limit"] == ["canary"] and {e["node"] for e in failed["entries"]} == {"a"}
    assert failed["selections"]["report"]["left_out"] == ["b"] and "nodes" not in failed["selections"]["report"]
    rejected = raw("resume", "--json", "--inventory", "targets", "--limit", "canary")
    assert rejected.returncode == 4 and "does not accept --limit" in rejected.stderr, rejected.stderr
    remote("a", "rm /var/tmp/mmx-integration/fail-deploy")
    resumed = run("resume", 0, "--inventory", "targets")
    assert resumed["id"] == failed["id"] and resumed["status"] == "succeeded" and resumed["limit"] == ["canary"]
    assert {e["node"] for e in resumed["entries"]} == {"a"}
    assert deployed("a") == "v1" and deployed("b") == "absent"
    preview = run("dryrun", 0, "--inventory", "targets", "--limit", "canary")  # exit 0: unchanged
    assert {e["node"] for e in preview["entries"]} == {"a"}
    mark("run --limit changes only the limited nodes, reports the left-out fleet stage, and resume keeps the recorded limit")

    human = raw("run", "--inventory", "staging", "--output", "nodes")
    assert human.returncode == 0, human.stderr
    assert "Inventory: staging (1 node)   Selected: 1 node" in human.stderr, human.stderr
    short = next(line.split()[1].rstrip(":") for line in human.stdout.splitlines() if line.startswith("Run "))
    assert deployed("b") == "v1"
    staging = run("results", 0, short)
    assert staging["short_id"] == short and staging["inventory"] == "staging"
    assert run("results", 0, "--inventory", "staging")["id"] == staging["id"]
    assert run("results", 0, "--inventory", "targets")["id"] == preview["id"]
    assert raw("results", "--json").returncode == 4  # no run of this project used its own inventory
    mark("each inventory keeps its own default run, the selection line precedes remote work, and short IDs select runs")


def selection(workspace, mmx, nodes, remote, project, run, mark):
    """except_nodes, --only-fleet-stages with --limit and resume, and each node's record of its last successful run."""
    base, fail = "/var/tmp/mmx-integration/selection", "/var/tmp/mmx-integration/fail-sites"
    runs = workspace / ".mmx" / "runs"
    for h in nodes:
        remote(h, f"rm -rf {base} {fail}; mkdir -p {base}")

    def raw(*args):
        return vm.cmd([mmx, args[0], *vm.playbook_args(args[0]), *args[1:]], cwd=workspace, check=False)

    def created(node):
        return sorted(remote(node, f"ls {base}").split())

    def record(node):
        return json.loads(remote(node, "cat /var/lib/mmx/last_successful_run/selection/configure"))

    def stage(name, **fields):
        return {"id": name, "nodes": "all", **fields, "node_tasks": [
            {"shell": {"script": f"test {name} != sites || test ! -f {fail}", "changed_when": False}},
            {"file": {"permissions_mode": "0644", "path": f"{base}/{name}", "content": name}}]}

    project([stage("network", except_nodes=["rest"]), stage("ruby"), stage("sites", requires={"same_node": ["ruby"]})], name="selection")
    only_network = ("--inventory", "targets", "--only-fleet-stages", "network")
    run("dryrun", None, *only_network)
    assert created("a") == created("b") == []
    applied = run("run", 0, *only_network)
    assert applied["only_fleet_stages"] == ["network"] and {(e["fleet_stage"], e["node"]) for e in applied["entries"]} == {("network", "a")}
    assert created("a") == ["network"] and created("b") == []
    human = raw("run", *only_network)
    assert human.returncode == 0 and "Only fleet stages: network" in human.stderr, human.stderr
    assert "  ruby: not selected — left out by --only-fleet-stages" in human.stdout and "  sites: not selected" in human.stdout, human.stdout
    rerun = run("run", 0, *only_network)
    assert all(e["response"]["change"] == "unchanged" for e in rerun["entries"])
    assert created("a") == ["network"] and created("b") == []
    mark("except_nodes leaves a node untouched and --only-fleet-stages runs only the named fleet stage; dryrun changes nothing and a rerun is unchanged")

    before = len(list(runs.iterdir()))
    for command in ("nodes", "explain", "dryrun", "run"):
        p = raw(command, "--json", "--inventory", "targets", "--only-fleet-stages", "sites")
        assert p.returncode == 4 and "--only-fleet-stages sites leaves out fleet stage ruby, which fleet stage sites requires; use --only-fleet-stages ruby,sites" in p.stderr, (command, p.stderr)
    p = raw("run", "--json", "--inventory", "targets", "--only-fleet-stages", "nope")
    assert p.returncode == 4 and "its fleet stages are network, ruby, sites" in p.stderr, p.stderr
    assert len(list(runs.iterdir())) == before and created("a") == ["network"] and created("b") == []
    mark("--only-fleet-stages that leaves out a required fleet stage is rejected before any remote work, printing the value that includes it")

    remote("a", f"touch {fail}")
    kept = ("--inventory", "targets", "--only-fleet-stages", "ruby,sites", "--limit", "canary")
    failed = run("run", 1, *kept)
    assert {(e["fleet_stage"], e["node"], e["response"]["status"]) for e in failed["entries"]} == {("ruby", "a", "succeeded"), ("sites", "a", "failed")}
    assert created("a") == ["network", "ruby"] and created("b") == []
    assert record("a")["run_id"] == rerun["id"]
    for option in (["--only-fleet-stages", "ruby"], ["--limit", "canary"]):
        p = raw("resume", "--json", "--inventory", "targets", *option)
        assert p.returncode == 4 and "start a new mmx run <playbook>" in p.stderr, p.stderr
    remote("a", f"rm {fail}")
    resumed = run("resume", 0, "--inventory", "targets")
    assert resumed["id"] == failed["id"] and resumed["status"] == "succeeded", resumed["status"]
    assert resumed["only_fleet_stages"] == ["ruby", "sites"] and resumed["limit"] == ["canary"]
    assert created("a") == ["network", "ruby", "sites"] and created("b") == []
    assert record("a") == dict(record("a"), run_id=failed["id"], inventory="targets", limit="canary", only_fleet_stages="ruby,sites")
    remote("b", "test ! -e /var/lib/mmx/last_successful_run/selection")
    mark("resume repeats the recorded fleet stages and limit and rejects both options; the node records the run's limit and fleet stages")

    remote("a", f"touch {fail}")
    mixed = run("run", 1, "--inventory", "targets")
    assert record("b") == dict(record("b"), run_id=mixed["id"], limit="", only_fleet_stages="")
    assert record("a")["run_id"] == failed["id"] and created("b") == ["ruby", "sites"]
    remote("a", f"rm {fail}")
    mark("a node whose fleet stages all succeeded records the run; a failed node keeps its earlier record")


def variables(workspace, mmx, nodes, remote, project, run, mark):
    """Group vars and declared command-line variables (--var), through resume."""
    vm.write_inventory(workspace, {
        "vars": {"port": 80},
        "nodes": {"a": nodes["a"], "b": dict(nodes["b"], vars={"port": 8081})},
        "groups": {"web": {"nodes": ["a", "b"], "vars": {"port": 9090, "tier": "web"}}}}, name='vars')
    vm.write_inventory(workspace, {
        "nodes": nodes,
        "groups": {"web": {"nodes": ["a", "b"], "vars": {"port": 9090}},
                   "canary": {"nodes": ["a"], "vars": {"port": 9090}}}}, name='overlap')
    target, migrated = "/var/tmp/mmx-integration/variables", "/var/tmp/mmx-integration/migrated"
    fail = "/var/tmp/mmx-integration/fail-variables"
    runs = workspace / ".mmx" / "runs"

    def raw(*args):
        return vm.cmd([mmx, args[0], *vm.playbook_args(args[0]), *args[1:]], cwd=workspace, check=False)

    def deployed(node):
        return remote(node, f"cat {target} 2>/dev/null || echo absent").strip()

    project([{"id": "deploy", "nodes": "web", "node_tasks": [
        {"shell": {"script": f"test ! -f {fail}", "changed_when": False}},
        {"file": {"permissions_mode": "0644", "path": target, "content": "release={{ .vars.release }} port={{ .vars.port }} tier={{ .vars.tier }}"}},
        {"when": "{{ .vars.migrate }}", "file": {"permissions_mode": "0644", "path": migrated, "content": "x"}}]}],
        name="variables", command_line_vars={"release": {"type": "string"}, "migrate": {"type": "boolean", "default": True}})
    before = len(list(runs.iterdir()))
    for args, message in [
        (["--var", "version=2"], "version is not declared in command_line_vars (declared: migrate, release)"),
        (["--var", "release=1.4.3", "--var", "migrate=no"], "migrate must be true or false"),
        ([], "set release with --var NAME=VALUE"),
    ]:
        p = raw("run", "--json", "--inventory", "vars", *args)
        assert p.returncode == 4 and message in p.stderr, (args, p.returncode, p.stderr)
    p = raw("nodes", "--inventory", "overlap")
    assert p.returncode == 4 and "node a belongs to groups canary and web, which set the same variable port" in p.stderr, p.stderr
    assert len(list(runs.iterdir())) == before
    for h in nodes:
        assert deployed(h) == "absent"
    mark("undeclared, mistyped, or missing --var values and overlapping group vars are rejected before any remote work")

    shown = json.loads(raw("explain", "--json", "--inventory", "vars", "--limit", "a", "--var", "release=1.4.3").stdout)
    assert {v["field"]: v["source"]["field"] for v in shown["command_line_vars"]} == {
        "release": "--var release", "migrate": "command_line_vars.migrate.default"}, shown["command_line_vars"]
    content = next(a for t in shown["fleet_stages"][0]["node_tasks"] for a in t["arguments"] if a["field"] == "file.content")
    assert content["value"] == "release=1.4.3 port=9090 tier=web", content
    assert {o["field"] for o in content["origins"]} == {"--var release", "groups.web.vars.port", "groups.web.vars.tier"}, content
    remote("b", f"touch {fail}")
    failed = run("run", 1, "--inventory", "vars", "--var", "release=1.4.3", "--var", "migrate=false")
    assert {(e["node"], e["response"]["status"]) for e in failed["entries"]} == {("a", "succeeded"), ("b", "failed")}
    assert deployed("a") == "release=1.4.3 port=9090 tier=web" and deployed("b") == "absent"
    rejected = raw("resume", "--json", "--inventory", "vars", "--var", "release=9")
    assert rejected.returncode == 4 and "accepts no --var" in rejected.stderr, rejected.stderr
    remote("b", f"rm {fail}")
    resumed = run("resume", 0, "--inventory", "vars")
    assert resumed["id"] == failed["id"] and resumed["status"] == "succeeded"
    assert deployed("b") == "release=1.4.3 port=8081 tier=web"
    for h in nodes:
        remote(h, f"test ! -e {migrated}")
    mark("group vars reach their nodes under node overrides, explain names each origin, and resume reuses the recorded --var values")



def ad_hoc(workspace, mmx, nodes, remote, run, mark):
    """mmx run-command: the connection check and one-off commands as recorded runs."""
    base = "/var/tmp/mmx-integration"

    def raw(*args):
        return vm.cmd([mmx, args[0], *vm.playbook_args(args[0]), *args[1:]], cwd=workspace, check=False)

    checked = raw("run-command", "--nodes", "all", "--", "true")
    assert checked.returncode == 0, (checked.stdout, checked.stderr)
    assert "Selected: 2 nodes" in checked.stderr, checked.stderr
    assert "run-command / a: succeeded" in checked.stdout and "run-command / b: succeeded" in checked.stdout, checked.stdout
    (workspace / "empty_known_hosts").write_text("")
    untrusted = dict(nodes["b"], ssh_known_hosts=str(workspace / "empty_known_hosts"), connection='ssh_only')
    vm.write_inventory(workspace, {"nodes": {"a": nodes["a"], "b": untrusted}}, name='untrusted')
    refused = run("run-command", 1, "--inventory", "untrusted", "--nodes", "all", "--", "true")
    outcome = {e["node"]: e["response"] for e in refused["entries"]}
    assert outcome["a"]["status"] == "succeeded", outcome
    assert outcome["b"]["status"] == "failed" and "Host key verification failed" in outcome["b"]["error"], outcome
    assert "known_hosts" in outcome["b"]["error"], outcome
    mark("the connection check succeeds on reachable nodes and names an untrusted host key with its remedy")

    for h in nodes:
        remote(h, f"echo {h} > {base}/adhoc-name")
    human = raw("run-command", "--inventory", "targets", "--nodes", "canary,rest", "--output", "summary", "--", "echo", "same", "output")
    assert human.returncode == 0, human.stderr
    assert "run-command / 2 nodes [a, b] / command: succeeded, changed\n    same output\n" in human.stdout, human.stdout
    distinct = run("run-command", 0, "--nodes", "all", "--", f"cat {base}/adhoc-name")
    assert {e["node"]: e["response"]["results"][0]["stdout"] for e in distinct["entries"]} == {"a": "a\n", "b": "b\n"}
    mark("mmx run-command runs one command across a node and group union, grouping identical output and keeping distinct output")

    remote("a", f"touch {base}/adhoc-fail")
    failed = run("run-command", 1, "--nodes", "all", "--", f"echo attempt >> {base}/adhoc-count; test ! -f {base}/adhoc-fail")
    outcome = {e["node"]: e["response"] for e in failed["entries"]}
    assert outcome["a"]["status"] == "failed" and outcome["a"]["results"][0]["exit_code"] == 1, outcome
    assert outcome["b"]["status"] == "succeeded", outcome
    unselected = raw("resume", "--json")  # defaults keep selecting playbook runs, and this project has none
    assert unselected.returncode == 4 and "no matching saved run" in unselected.stderr, unselected.stderr
    assert raw("results", "--json").returncode == 4
    assert run("results", 0, failed["short_id"])["id"] == failed["id"]
    remote("a", f"rm {base}/adhoc-fail")
    resumed = run("resume", 0, failed["short_id"])
    assert resumed["id"] == failed["id"] and resumed["status"] == "succeeded"
    assert [remote(h, f"wc -l < {base}/adhoc-count").strip() for h in ("a", "b")] == ["2", "1"]
    mark("a failing node fails only its entry; the run-command run is selected by ID for results and resume, which reruns only that node")


if __name__ == "__main__":
    vm.run_suite(main)
