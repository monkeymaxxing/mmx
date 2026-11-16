#!/usr/bin/env python3
"""Run public-CLI acceptance scenarios on the test machines that incus.py or
hetzner.py creates.

Python standard library only. The file of test machines that MMX_TEST_TARGETS
names supplies each machine's address, login, and SSH host key; product
requests always verify the host key through SSH.
"""
import argparse
import atexit
import contextlib
import fcntl
import hashlib
import json
import os
from pathlib import Path
import platform
import shlex
import shutil
import subprocess
import socket
import sys
import tarfile
import time

import resource_bundle
import testenv

# A shell test that holds where the guest's kernel runs SELinux with a loaded
# policy, by libselinux's rule: selinuxfs is mounted and the context is not
# "kernel". Some images mount selinuxfs without a policy and without the
# SELinux programs.
SELINUX_RUNS = 'test -e /sys/fs/selinux/enforce && test "$(tr -d \'\\000\' < /proc/self/attr/current)" != kernel'


REPO = Path(__file__).resolve().parents[2]
ROOT = testenv.root()
NAME = "mmx-dev"
ENV = testenv.environment(MMX_RUNNER_DIR=str(ROOT / "bin"))
MARKER = "mmx-disposable-integration-v1"
# What a suite says when it is started without MMX_TEST_TARGETS, the file of
# test machines that incus.py and hetzner.py write for each job slot.
NO_TEST_MACHINES_MESSAGE = (
    "MMX_TEST_TARGETS is not set. The VM suites run only on the test machines that "
    "tests/integration/incus.py or tests/integration/hetzner.py creates; those set "
    "MMX_TEST_TARGETS to the file that lists the machines. Start the suites through "
    "incus.py or hetzner.py.")
# The shared SSH connections' sockets. A Unix socket path holds about 100
# characters, which a test root's own path can use up, so the directory is a
# short one named for the root.
SSH_CONTROL = Path("/tmp") / ("mmx-test-ssh-" + hashlib.sha256(str(ROOT).encode()).hexdigest()[:12])
FORWARDS = {}


@atexit.register
def remove_shared_connection_sockets():
    # Each run on a test server has a new root, so a directory left behind
    # stays in /tmp for good. A master that still persists exits on its own.
    shutil.rmtree(SSH_CONTROL, ignore_errors=True)


@atexit.register
def close_forwards():
    for process, _ in FORWARDS.values():
        process.terminate()
        process.wait(timeout=5)
    FORWARDS.clear()


def run_suite(main):
    if not os.environ.get("MMX_TEST_TARGETS"):
        sys.exit(NO_TEST_MACHINES_MESSAGE)
    with testenv.locked(ROOT):
        try:
            main()
        finally:
            close_forwards()


def cmd(args, *, cwd=None, check=True, input=None, timeout=300, inherit_lock=False):
    env = dict(ENV)
    env.pop(testenv.LOCK_FD, None)
    # Only nested build wrappers inherit the lock. SSH may spawn long-lived
    # helpers, which must not keep the environment leased.
    fd = testenv.inherited_fd(ROOT) if inherit_lock else None
    if fd is not None:
        env[testenv.LOCK_FD] = str(fd)
    p = subprocess.run([str(a) for a in args], cwd=cwd, env=env, input=input,
                       pass_fds=() if fd is None else (fd,),
                       text=True, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                       timeout=timeout)
    if check and p.returncode:
        raise RuntimeError(f"{shlex.join(str(a) for a in args)}\n{p.stdout}\n{p.stderr}")
    return p


def write_settings(root, settings):
    """Write project settings and the project-root marker."""
    root = Path(root)
    root.mkdir(parents=True, exist_ok=True)
    (root / "mmx_project_settings.yml").write_text(json.dumps(settings, indent=2) + "\n")


def write_playbook(root, name, fleet_stages, command_line_vars=None):
    """Write a named playbook containing only its stages and variable declarations."""
    directory = Path(root) / "playbooks"
    directory.mkdir(parents=True, exist_ok=True)
    data = {"fleet_stages": fleet_stages}
    if command_line_vars is not None:
        data["command_line_vars"] = command_line_vars
    (directory / (name + ".yml")).write_text(json.dumps(data, indent=2) + "\n")


def write_inventory(root, inventory, name="example"):
    """Write a named inventory with its node connections and groups."""
    directory = Path(root) / "inventories"
    directory.mkdir(parents=True, exist_ok=True)
    (directory / (name + ".yml")).write_text(json.dumps(inventory, indent=2) + "\n")


def write_project(root, *, project_name, fleet_stages, playbook_name="configure",
                  default_inventory="example", command_line_vars=None, **settings):
    """Write the separate settings and playbook files for one suite scenario."""
    write_settings(root, dict(project_name=project_name,
                             default_inventory=default_inventory, **settings))
    write_playbook(root, playbook_name, fleet_stages, command_line_vars)


def playbook_args(command, name="configure"):
    """Select a suite's named playbook only for commands that inspect or start it."""
    return [name] if command in ("validate", "explain", "nodes", "dryrun", "run") else []


def success_codes(command):
    """Resource suites inspect preview details; diagnostics.py checks exact codes."""
    return (0, 2, 3) if command == 'dryrun' else (0,)


def build_and_use_release_archive(directory, version):
    """Build checked archives and put this controller machine's archive in the shared build."""
    system = {'Darwin': 'macos', 'Linux': 'linux'}[platform.system()]
    architecture = {'arm64': 'arm64', 'aarch64': 'arm64',
                    'x86_64': 'amd64', 'AMD64': 'amd64'}[platform.machine()]
    archives = directory / 'release-archives'
    subprocess.run([sys.executable, str(REPO / 'scripts/build-release-archives.py'),
                    '--version', version, '--output-dir', str(archives)],
                   cwd=REPO, env=testenv.environment(), check=True)
    archive = archives / f'mmx-{version}-{system}-{architecture}.tar.gz'
    checksum = hashlib.sha256(archive.read_bytes()).hexdigest()
    print(f'Using release archive: {checksum}  {archive.name}', flush=True)
    shared = directory / 'build'
    shared.mkdir()
    # The builder has checked the locally built archive's exact contents.
    with tarfile.open(archive, 'r:gz') as payload:
        payload.extractall(shared, filter='data')
    (shared / archive.name.removesuffix('.tar.gz')).rename(shared / 'bin')


@contextlib.contextmanager
def shared_build():
    """Yield the run's shared build directory while holding its build lock, or
    None for a suite started on its own. The first suite of a run that needs
    a build makes it there; the others wait for it and use it."""
    shared = os.environ.get(testenv.SHARED_BUILD)
    if not shared:
        yield None
        return
    directory = Path(shared)
    directory.mkdir(parents=True, exist_ok=True)
    with (directory / "build.lock").open("a") as lock:
        fcntl.flock(lock, fcntl.LOCK_EX)
        yield directory


def release_version_flag():
    """The go build flag that gives an mmx a suite builds itself the release
    version of this run's node runner executables in bin/. mmx refuses node
    runner executables of another release, and a release-archive run's runners
    carry the archive's version. Call it after build()."""
    version = (ROOT / 'bin/official_extra_modules/shipped_with_mmx_version').read_text().strip()
    return '-ldflags=-X mmx/internal/release.Version=' + version


def build():
    with shared_build() as shared:
        if shared is None:
            cmd(["sh", REPO / "scripts/build.sh", ROOT / "bin"], cwd=REPO,
                inherit_lock=True)
            return
        built = shared / "bin"
        if not (built / "mmx").exists():
            cmd(["sh", REPO / "scripts/build.sh", built], cwd=REPO)
    # Links, not copies: every slot uses the same files of this run's build,
    # and its own copy of the official extra modules directory beside them.
    (ROOT / "bin").mkdir(parents=True, exist_ok=True)
    for source in built.iterdir():
        target = ROOT / "bin" / source.name
        if source.is_dir():
            shutil.rmtree(target, ignore_errors=True)
            shutil.copytree(source, target)
            continue
        target.unlink(missing_ok=True)
        os.link(source, target)


def target(name=NAME):
    """Return the named test machine from the file that MMX_TEST_TARGETS names."""
    inventory = os.environ.get("MMX_TEST_TARGETS")
    if not inventory:
        raise RuntimeError(NO_TEST_MACHINES_MESSAGE)
    return json.loads(Path(inventory).read_text())[name]


def ssh_args(node):
    # Harness checks reuse one connection per target; a fresh handshake costs
    # several round trips, which dominates suites against cloud targets.
    SSH_CONTROL.mkdir(mode=0o700, exist_ok=True)
    return ["ssh", "-T", "-F", "/dev/null", "-o", "BatchMode=yes", "-o",
            "StrictHostKeyChecking=yes", "-o", "UserKnownHostsFile=" + node["ssh_known_hosts"],
            "-i", node["ssh_identity_file"], "-o", "IdentitiesOnly=yes", "-o", "ConnectTimeout=5",
            # %C covers address, port, and account, and a cloud provider gives
            # a deleted server's address to the next one; the known_hosts file
            # names the server itself.
            "-o", "ControlMaster=auto", "-o", "ControlPath=" + str(SSH_CONTROL / (hashlib.sha256(
                node["ssh_known_hosts"].encode()).hexdigest()[:8] + "-%C")), "-o", "ControlPersist=60",
            "-o", "ServerAliveInterval=5", "-o", "ServerAliveCountMax=3",
            "-p", str(node.get("ssh_port", 22)), node["ssh_user"] + "@" + node["address"]]


def unshared(args):
    """Forwards and agent-forwarding sessions need their own connection: a shared
    master carries only the options it was opened inputs."""
    out, i = [], 0
    while i < len(args):
        if args[i] == "-o" and str(args[i + 1]).startswith("Control"):
            i += 2
            continue
        out.append(args[i])
        i += 1
    return out


def forget_shared():
    # A shared connection does not survive its target stopping or rebooting.
    # Without its socket the next check connects again; the stale master exits
    # on its keepalive or persist timeout.
    for path in SSH_CONTROL.glob("*"):
        path.unlink(missing_ok=True)


def guest(*args, name=NAME, **kwargs):
    return cmd([*ssh_args(target(name)["node"]), shlex.join(map(str, args))], **kwargs)


def copy(source, destination, name=NAME):
    """Copy a file from the controller machine to the target with its
    permission bits, as scp does: an SSH identity stays 0600, a program
    stays executable."""
    _, ssh = connection(name)
    mode = format(Path(source).stat().st_mode & 0o777, "o")
    quoted = shlex.quote(destination)
    with Path(source).open("rb") as data:
        subprocess.run([str(a) for a in [*ssh, f"cat > {quoted} && chmod {mode} {quoted}"]],
                       stdin=data, stdout=subprocess.DEVNULL, env=ENV, check=True, timeout=120)


def fetch(source, destination, name=NAME):
    """Copy a file from the target to the controller machine."""
    _, ssh = connection(name)
    with Path(destination).open("wb") as data:
        subprocess.run([str(a) for a in [*ssh, "cat " + shlex.quote(source)]],
                       stdout=data, env=ENV, check=True, timeout=120)


def console(script, name=NAME):
    return guest("sudo", "-n", "/bin/sh", "-c", script, name=name).stdout


def architecture(name=NAME):
    """Return the test machine's architecture as Go names it."""
    return {"aarch64": "arm64", "x86_64": "amd64"}[console("uname -m", name).strip()]


def close_forward(name):
    if name in FORWARDS:
        process, _ = FORWARDS.pop(name)
        process.terminate()
        process.wait(timeout=5)


def reboot(name=NAME):
    close_forward(name)
    old = console("cat /proc/sys/kernel/random/boot_id", name).strip()
    # systemd runs a timer up to a minute late unless the timer says how
    # exact it must be: without AccuracySec, the reboot started 1 to 50
    # seconds after this command.
    guest("sudo", "-n", "systemd-run", "--on-active=1s", "--timer-property=AccuracySec=100ms",
          "/usr/bin/systemctl", "reboot", name=name)
    await_boot(old, name)


# The failed units, each with its status: its result and exit code stay there
# also when it failed before the journal started.
FAILED_UNITS = ("for unit in $(systemctl --failed --no-legend --plain | cut -d' ' -f1); "
                "do systemctl status --no-pager --lines=8 \"$unit\"; done")


def await_boot(old, name=NAME):
    """Wait until the target runs on a boot other than old, then connect again:
    its shared SSH connection and HTTPS forward do not survive a reboot."""
    close_forward(name)
    forget_shared()
    deadline = time.monotonic() + 180
    while time.monotonic() < deadline:
        result = guest("cat", "/proc/sys/kernel/random/boot_id", name=name, check=False)
        if result.returncode == 0 and result.stdout.strip() != old:
            state = guest("sudo", "-n", "systemctl", "is-system-running", "--wait", name=name, check=False)
            if state.returncode:
                raise RuntimeError(f"{name} is {state.stdout.strip()} after the reboot; failed units:\n"
                                   + guest("sudo", "-n", "/bin/sh", "-c", FAILED_UNITS, name=name, check=False).stdout)
            return connection(name)
        time.sleep(1)
    raise RuntimeError("test target did not reboot: " + name)


def enlarge(name=NAME):
    """Stop the target, give it more CPUs and memory, and start it again: one
    more CPU and 1 GiB for an Incus guest, a larger server type for a cloud
    server. Return the function that gives it back its original size."""
    selected = target(name)
    if "incus_instance" in selected:
        import incus
        return incus.enlarge(name, selected)
    import hetzner
    server, location = selected["hetzner_server_id"], selected["hetzner_location"]

    def resize(candidates):
        old = console("cat /proc/sys/kernel/random/boot_id", name).strip()
        close_forward(name)
        try:
            chosen = hetzner.change_server_type(server, location, candidates)
        except BaseException:
            # A change that failed after the shutdown starts the server
            # again; one that failed before it left the server running.
            now = guest("cat", "/proc/sys/kernel/random/boot_id", name=name, check=False)
            if now.returncode != 0 or now.stdout.strip() != old:
                await_boot(old, name)
            raise
        if chosen:
            await_boot(old, name)
        return chosen

    larger_server_types = hetzner.LARGER_SERVER_TYPES[selected["architecture"]]
    if not resize(larger_server_types):
        raise RuntimeError(f"{location} has none of {', '.join(larger_server_types)} now")
    original = selected["hetzner_server_type"]

    def restore():
        if not resize((original,)):
            print(f"{original} is sold out in {location}; the server keeps its larger type", flush=True)
    return restore


def up(name=NAME):
    """Check that the test machine is a disposable one of the test run, has
    the architecture its file of test machines names, and runs without
    failed units."""
    selected = target(name)
    if console("cat /etc/mmx-test-vm", name).strip() != MARKER:
        raise RuntimeError("target is missing its disposable-test marker: " + name)
    assert architecture(name) == selected["architecture"]
    state = guest("sudo", "-n", "systemctl", "is-system-running", "--wait", name=name, check=False)
    if state.returncode:
        raise RuntimeError(f"{name} is {state.stdout.strip()}; failed units:\n"
                           + guest("sudo", "-n", "/bin/sh", "-c", FAILED_UNITS, name=name, check=False).stdout)
    print("VM ready: " + selected["platform"], flush=True)


def connection(name=NAME):
    node = dict(target(name)["node"])
    node.setdefault("ssh_port", 22)
    ssh = ssh_args(node)
    # A test-owned TCP forward over SSH carries the suites' HTTPS connections
    # to the node's listener on port 7443; MMX still uses and verifies TLS
    # independently of this tunnel.
    if name not in FORWARDS or FORWARDS[name][0].poll() is not None:
        with socket.socket() as listener:
            listener.bind(("127.0.0.1", 0))
            endpoint_port = listener.getsockname()[1]
        forward = subprocess.Popen([str(a) for a in [*unshared(ssh[:-1]), "-N", "-o", "ExitOnForwardFailure=yes", "-L", f"127.0.0.1:{endpoint_port}:127.0.0.1:7443", ssh[-1]]],
                                   env=ENV, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        FORWARDS[name] = (forward, endpoint_port)
        # Wide, since the loop ends as soon as ssh exits: a new SSH login to a
        # guest just after its reboot took over 5 s on a busy server.
        deadline = time.monotonic() + 60
        while True:
            if forward.poll() is not None:
                raise RuntimeError("test HTTPS forward failed")
            try:
                with socket.create_connection(("127.0.0.1", endpoint_port), timeout=0.1):
                    break
            except OSError:
                if time.monotonic() >= deadline:
                    raise RuntimeError(f"the test HTTPS forward to {name} did not listen on 127.0.0.1:{endpoint_port} within 60 s")
                time.sleep(0.05)
    node["https_address"] = "https://127.0.0.1:" + str(FORWARDS[name][1])
    return node, ssh


def earlier_change_may_provide(earlier_task, missing):
    """The reason of a node task that a dryrun cannot check because something is missing, after an earlier node task that would change the node first: the module's own reason (missing), then the earlier node task, named as the playbook names it ("monitor in the fleet stage monitor" for one of an earlier fleet stage)."""
    return (missing[:-1] if missing.endswith('.') else missing) + '. The earlier node task ' + earlier_task + \
        ' would change this node first and may provide what is missing; if the run then succeeds, change nothing.'


def package_tasks(names):
    """Node tasks that install names with the node's own package module."""
    return [{"when": {"equals": ["{{ .node.package_manager }}", manager]}, manager + "_package": {"packages": [{"name": name, "version": "newest_when_first_installed"} for name in names]}}
            for manager in ("apt", "dnf")]


# Harness setup that installs packages with the guest's own package manager,
# the way a person prepares a server before MMX manages it. A suite in the
# base set (all.py's BASE_SUITE) installs what it needs this way, so that it
# depends on no package module and runs on every tested distribution.
INSTALL_PACKAGES = (
    # A fresh server may have empty apt package lists, so apt refreshes them
    # when the first attempt cannot find a package.
    "if command -v apt-get >/dev/null; then export DEBIAN_FRONTEND=noninteractive; "
    "apt-get install -yq {packages} || {{ apt-get update -q && apt-get install -yq {packages}; }}; "
    "elif command -v dnf >/dev/null; then dnf install -yq {packages}; "
    "elif command -v pacman >/dev/null; then pacman -S --noconfirm --needed {packages}; "
    "elif command -v zypper >/dev/null; then zypper --non-interactive install {packages}; "
    "else echo 'the guest has no package manager that the harness knows' >&2; exit 1; fi")


def install_packages(names, name=NAME):
    """Install packages on a test machine with its own package manager."""
    console(INSTALL_PACKAGES.format(packages=" ".join(shlex.quote(n) for n in names)), name)


# ufw left on by an interrupted server_resources.py must not reach later suites.
UFW_RESET = "if command -v ufw >/dev/null; then ufw --force reset >/dev/null; rm -f /etc/ufw/*.rules.[0-9]*; fi"


def reset(name=NAME):
    if console("cat /etc/mmx-test-vm", name).strip() != MARKER:
        raise RuntimeError("target is missing its disposable-test marker: " + name)
    console("pkill -KILL -f '[m]mx-runner worker' || true", name)
    console("systemctl disable --now mmx-https-api.service 2>/dev/null || true; "
            "systemctl disable mmx-test-slow-boot.service 2>/dev/null || true; "
            "rm -f /etc/systemd/system/mmx-https-api.service /etc/systemd/system/mmx-test-slow-boot.service; systemctl daemon-reload", name)
    # A unit an earlier suite left failed would keep the system "degraded".
    console("systemctl reset-failed", name)
    # An interrupted job_control.py must not leave a node refusing management.
    console("rm -rf /var/lib/mmx /run/mmx /opt/mmx /var/tmp/mmx-integration /etc/mmx/do_not_manage; "
            "rm -f /etc/mmx-example; mkdir -p /var/tmp/mmx-integration", name)
    console(UFW_RESET, name)


def tests(name=NAME):
    up(name)
    node, ssh = connection(name)
    reset(name)
    workspace = ROOT / "workflows" / time.strftime("%Y%m%d-%H%M%S")
    workspace.mkdir(parents=True)
    write_inventory(workspace, {"nodes": {"test": node}}, name='example')
    mmx = ROOT / "bin/mmx"
    build()
    passed = []

    def remote(script):
        return cmd([*ssh, "sudo -n /bin/sh -c " + shlex.quote(script)]).stdout

    def project(tasks, name="acceptance"):
        p = {"project_name": name, 'default_inventory': 'example', "fleet_stages": [
            {"id": "configure", "nodes": "all", "node_tasks": tasks}]}
        write_project(workspace, playbook_name='configure', **p)

    def run(command, expected=None, *args):
        p = cmd([mmx, command, *playbook_args(command, "configure"), "--json", *args], cwd=workspace, check=False)
        assert p.returncode in (success_codes(command) if expected is None else (expected,)), f"{command}: exit {p.returncode}\n{p.stdout}\n{p.stderr}"
        return json.loads(p.stdout)

    def mark(name):
        passed.append(name)
        print("PASS " + name, flush=True)

    file = {"id": "welcome", "file": {"path": "/etc/mmx-example",
            "content": "Managed by MMX.\n", "owner": "root", "group": "root", "permissions_mode": "0644"}}
    project([file])
    assert run("validate")["valid"]
    dry = run("dryrun")
    assert dry["entries"][0]["response"]["results"][0]["change"] == "would_change"
    assert remote("test ! -e /etc/mmx-example; test -d /opt/mmx/bin") == ""
    mark("fresh-node dryrun installs runner without changing managed file")
    applied = run("run")
    assert applied["entries"][0]["response"]["results"][0]["change"] == "changed"
    assert remote("cat /etc/mmx-example") == "Managed by MMX.\n"
    assert remote("stat -c '%a %U %G' /etc/mmx-example").strip() == "644 root root"
    assert run("run")["entries"][0]["response"]["results"][0]["change"] == "unchanged"
    mark("apply and unchanged rerun match independent file and metadata checks")
    assert run("results")["status"] == "succeeded"
    mark("saved results available through public results command")

    # Reject candidates before replacement, preserving live contents and mode.
    rejected = json.loads(json.dumps(file))
    rejected["file"]["content"] = "bad candidate\n"
    rejected["file"]["permissions_mode"] = "0600"
    rejected["file"]["check_new_file"] = {"shell": {"script": "grep -q '^Managed' \"$NEW_FILE\"",
                                             "env": {"NEW_FILE": "{{ .new_file.path }}"}}}
    project([rejected])
    run("run", 1)
    assert remote("cat /etc/mmx-example") == "Managed by MMX.\n"
    assert remote("stat -c '%a' /etc/mmx-example").strip() == "644"
    mark("invalid candidate preserves existing live content and metadata")

    # A real packaged service role with explicit inputs and loaded-config checks.
    role_dir = workspace / "roles/web_server"
    shutil.copytree(REPO / "examples/nginx-role/roles/web_server", role_dir)
    template_path = role_dir / "files/nginx.conf.tmpl"
    original_template = template_path.read_text()
    # Start without the drop-in an earlier run of this test left.
    remote("rm -rf /etc/systemd/system/nginx.service.d; systemctl daemon-reload")
    # On a node that uses apt, the roles install nginx with apt_package. On any
    # other node the harness installs it with the guest's own package manager
    # and starts it with the distribution's own configuration, as a person does
    # before MMX takes over the service; MMX's first run then reloads it.
    uses_apt = cmd([*ssh, "command -v apt-get"], check=False).returncode == 0
    if not uses_apt:
        install_packages(["nginx"], name)
        remote("systemctl enable --now nginx")
    if uses_apt:
        project([{"role": "web_server", "inputs": {"port": 8008, "message": "checked-in example"}}], "example_acceptance")
        run("validate")
        run("dryrun")
        run("run")
        assert remote("curl --fail --silent http://127.0.0.1:8008/") == "checked-in example\n"
        example_noop = run("run")
        assert all(r["change"] == "unchanged" for r in example_noop["entries"][0]["response"]["results"])
        mark("checked-in nginx role runs directly and its second run is unchanged")
    role = {
        "inputs": {"port": {"type": "integer", "required": True},
                   "message": {"type": "string", "required": True}},
        "node_tasks": [{
            "task_block": {
                "id": "nginx",
                "changes": [
                    *(package_tasks(["nginx"]) if uses_apt else []),
                    {
                        "file": {
                            "path": "/etc/nginx/nginx.conf",
                            "template": "files/nginx.conf.tmpl",
                            "permissions_mode": "0644",
                            "owner": "root",
                            "group": "root",
                            "check_new_file": {"shell": {"script": 'nginx -t -c "$NEW_FILE"', "env": {"NEW_FILE": "{{ .new_file.path }}"}}},
                        }
                    },
                ],
                "activate": [{"id": "service", "systemd_service": {"name": "nginx", "start_at_boot": True, "refresh": "reload", "state": "running"}}],
                "verify": [
                    {
                        "wait_for": {'url_responds': "http://127.0.0.1:{{ .inputs.port }}/",
                                     "expected_http_status_codes": [200],
                                     "response_body_contains": "{{ .inputs.message }}\n",
                                     "wait_at_most": "10s"},
                    }
                ],
            }
        }]}
    def write_role():
        (role_dir / "main.yml").write_text(json.dumps(role, indent=2))
    def web_project(message):
        project([{"role": "web_server", "inputs": {"port": 8008, "message": message}}], "web_acceptance")
    def body():
        return remote("curl --fail --silent http://127.0.0.1:8008/")
    def activations():
        return remote("wc -l < /var/lib/mmx-integration-activations").strip()
    # Observe actual service reloads independently of MMX result records. The
    # count lives in /var/lib: a unit such as nginx's may have a private /tmp
    # and /var/tmp, and a reboot below empties /run.
    remote("mkdir -p /etc/systemd/system/nginx.service.d; "
           "cat > /etc/systemd/system/nginx.service.d/mmx-test.conf <<'UNIT'\n"
           "[Service]\nExecReload=\n"
           "ExecReload=/bin/sh -c '/usr/sbin/nginx -s reload && echo reloaded >> /var/lib/mmx-integration-activations'\nUNIT\n"
           "systemctl daemon-reload; : > /var/lib/mmx-integration-activations")
    write_role()
    web_project("first")
    run("dryrun")
    run("run")
    assert body() == "first\n"
    assert remote("systemctl is-active nginx").strip() == "active"
    assert remote("systemctl is-enabled nginx").strip() == "enabled"
    assert activations() == "1"
    run("run")
    assert activations() == "1"
    mark("packaged service role applies, verifies loaded config, and does not reactivate on no-op")

    block = role["node_tasks"][0]["task_block"]
    block["validate"] = [{"shell": {"script": "test -f /var/tmp/mmx-integration/validation-gate", "changed_when": False}}]
    write_role()
    web_project("second")
    failed_run = run("run", 1)
    assert body() == "first\n"
    assert "second" in remote("cat /etc/nginx/nginx.conf")
    assert activations() == "1"
    remote("touch /var/tmp/mmx-integration/validation-gate")
    template_path.write_text(original_template.replace(".inputs.message", '"SHOULD_NOT_BE_USED"'))
    recovered = run("resume")
    assert recovered["id"] == failed_run["id"]
    assert recovered["entries"][0]["previous_node_job_attempts"][0]["response"]["status"] == "failed"
    assert body() == "second\n"
    assert activations() == "2"
    template_path.write_text(original_template)
    mark("frozen resume completes pending activation despite unchanged file and edited local template")

    block["verify"].append({"shell": {"script": "test -f /var/tmp/mmx-integration/health-gate", "changed_when": False}})
    write_role()
    web_project("third")
    run("run", 1)
    assert body() == "third\n"
    assert activations() == "3"
    remote("touch /var/tmp/mmx-integration/health-gate")
    run("resume")
    assert activations() == "3"
    run("run")
    assert activations() == "3"
    mark("health failure retries verification without repeating acknowledged activation")

    # Failed cleanup is reported for that attempt, without holding new work hostage.
    cleanup_block = {
        "task_block": {
            "id": "scratch",
            "changes": [{"file": {"permissions_mode": "0644", "path": "/var/tmp/mmx-integration/original", "content": "old"}}],
            "cleanup": [{"shell": {"script": "test -f /var/tmp/mmx-integration/cleanup-gate && rm -f /var/tmp/mmx-integration/original"}}],
        }
    }
    project([cleanup_block], "cleanup_acceptance")
    cleanup_failure = run("run", 1)
    project([{"file": {"permissions_mode": "0644", "path": "/var/tmp/mmx-integration/new", "content": "new"}}], "cleanup_acceptance")
    run("run")
    remote("test -f /var/tmp/mmx-integration/new; test -f /var/tmp/mmx-integration/original; touch /var/tmp/mmx-integration/cleanup-gate")
    run("resume", 0, cleanup_failure["id"])
    remote("test ! -e /var/tmp/mmx-integration/original; test -f /var/tmp/mmx-integration/new")
    mark("failed cleanup permits new work; explicit old-run resume uses its recorded tasks")

    def wait_for(script, timeout=15):
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            p = cmd([*ssh, "sudo -n /bin/sh -c " + shlex.quote(script)], check=False)
            if p.returncode == 0:
                return
            time.sleep(0.1)
        raise AssertionError("timed out: " + script)

    def background():
        return subprocess.Popen([str(mmx), "run", 'configure', "--json"], cwd=workspace, env=ENV,
                                stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)

    project([{"shell": {"script": "touch /var/tmp/mmx-integration/disconnect-started; sleep 3; echo completed >> /var/tmp/mmx-integration/disconnect-completed"}}], "disconnect_acceptance")
    process = background()
    wait_for("test -f /var/tmp/mmx-integration/disconnect-started")
    process.kill()
    process.communicate(timeout=10)
    wait_for("test -f /var/tmp/mmx-integration/disconnect-completed")
    restored = run("resume")
    assert restored["entries"][0]["node_job_attempt"] == 1
    assert remote("wc -l < /var/tmp/mmx-integration/disconnect-completed").strip() == "1"
    mark("admitted job survives killed controller; resume reattaches without replay")

    project([{"shell": {"script": "touch /var/tmp/mmx-integration/restart-started; while ! test -f /var/tmp/mmx-integration/restart-gate; do sleep 0.1; done; echo completed >> /var/tmp/mmx-integration/restart-completed"}}], "endpoint_restart_acceptance")
    process = background()
    wait_for("test -f /var/tmp/mmx-integration/restart-started")
    remote("systemctl restart mmx-https-api.service")
    remote("touch /var/tmp/mmx-integration/restart-gate")
    out, err = process.communicate(timeout=30)
    assert process.returncode == 0 and json.loads(out)["status"] == "succeeded", (out, err)
    assert remote("wc -l < /var/tmp/mmx-integration/restart-completed").strip() == "1"
    mark("an admitted job survives an endpoint restart; the running controller follows it to completion")

    block["changes"].append({"shell": {"script": "touch /var/tmp/mmx-integration/worker-started; while ! test -f /var/tmp/mmx-integration/worker-gate; do sleep 0.1; done", "changed_when": False}})
    write_role()
    web_project("after interruption")
    process = background()
    wait_for("test -f /var/tmp/mmx-integration/worker-started")
    saved = run("results")
    job_id = saved["entries"][0]["job_id"]
    # Only the worker dies; the task's shell it started keeps running.
    remote("pkill -KILL -f '[m]mx-runner worker " + job_id + "'")
    out, err = process.communicate(timeout=20)
    assert process.returncode == 1, (out, err)
    assert "interrupted" in json.loads(out)["entries"][0]["response"]["error"]
    assert any(r["change"] == "changed" for r in json.loads(out)["entries"][0]["response"]["results"])
    assert body() == "third\n"
    assert activations() == "3"
    remote("touch /var/tmp/mmx-integration/worker-gate")
    run("resume")
    assert body() == "after interruption\n"
    assert activations() == "4"
    block["changes"].pop()
    write_role()
    mark("killed worker retains the service baseline; resume loads already-written configuration")

    # Competing controllers share one node lock, even across projects.
    project([{"shell": {"script": "touch /var/tmp/mmx-integration/busy-started; while ! test -f /var/tmp/mmx-integration/busy-gate; do sleep 0.1; done; echo completed >> /var/tmp/mmx-integration/busy-completed"}, "time_limit": "20s"}], "busy_first")
    process = background()
    wait_for("test -f /var/tmp/mmx-integration/busy-started")
    first_run = run("results")
    project([{"file": {"permissions_mode": "0644", "path": "/var/tmp/mmx-integration/busy-second", "content": "second"}}], "busy_second")
    busy = run("run", 1)
    assert "node busy" in busy["entries"][0]["response"]["error"]
    remote("test ! -e /var/tmp/mmx-integration/busy-second; touch /var/tmp/mmx-integration/busy-gate")
    out, err = process.communicate(timeout=25)
    assert process.returncode == 0, (out, err)
    run("resume")
    assert remote("cat /var/tmp/mmx-integration/busy-second") == "second"
    assert remote("wc -l < /var/tmp/mmx-integration/busy-completed").strip() == "1"
    mark("competing projects cannot overlap node work; rejected admission remains resumable")

    # Exercise request replay directly at the shared node-admission boundary.
    entry = first_run["entries"][0]
    request = remote("cat /var/lib/mmx/jobs/" + entry["job_id"] + "/request.json")
    runner = "/opt/mmx/bin/" + entry["runner_hash"] + "/mmx-runner"
    replay = cmd([*ssh, "sudo -n " + runner + " submit"], input=request)
    assert json.loads(replay.stdout)["status"] == "succeeded"
    altered = json.loads(request)
    altered["dryrun"] = True
    collision = cmd([*ssh, "sudo -n " + runner + " submit"], input=json.dumps(altered), check=False)
    assert collision.returncode == 1 and "different request" in collision.stderr
    assert remote("wc -l < /var/tmp/mmx-integration/busy-completed").strip() == "1"
    mark("identical request replay returns saved result; conflicting request identity is rejected")

    # Default resume selects the latest normal run; older failures stay historical.
    recovery_file = {"id": "config", "file": {"permissions_mode": "0644", "path": "/var/tmp/mmx-integration/current-run", "content": "first",
                     "check_new_file": {"shell": {"script": "test -f /var/tmp/mmx-integration/current-run-gate"}}}}
    project([recovery_file], "current_run_acceptance")
    original_failure = run("run", 1)
    remote("touch /var/tmp/mmx-integration/current-run-gate")
    recovery_file["file"]["content"] = "new desired value"
    project([recovery_file], "current_run_acceptance")
    successful = run("run")
    assert run("resume")["status"] == "nothing_to_resume"
    historical = run("results", 0, original_failure["id"])
    assert historical["status"] == "failed"
    assert "resolved_by" not in historical["entries"][0]
    assert remote("cat /var/tmp/mmx-integration/current-run") == "new desired value"
    mark("latest successful run yields nothing to resume; historical failures remain unchanged")

    # An unknown server key is an authentication failure, never a fallback path.
    bad_known = workspace / "empty_known_hosts"
    bad_known.write_text("")
    bad_node = dict(node, ssh_known_hosts=str(bad_known), connection='ssh_only')
    write_inventory(workspace, {"nodes": {"test": bad_node}}, name='example')
    project([{"file": {"permissions_mode": "0644", "path": "/var/tmp/mmx-integration/should-not-exist", "content": "bad"}}], "identity_acceptance")
    rejected = run("run", 1)
    assert "Host key verification failed" in rejected["entries"][0]["response"]["error"]
    remote("test ! -e /var/tmp/mmx-integration/should-not-exist")
    write_inventory(workspace, {"nodes": {"test": node}}, name='example')
    mark("unknown SSH node identity fails before managed work")

    web_project("after interruption")
    run("run")
    assert activations() == "4"
    old_boot = remote("cat /proc/sys/kernel/random/boot_id").strip()
    reboot(name)
    node, ssh = connection(name)
    write_inventory(workspace, {"nodes": {"test": node}}, name='example')
    assert remote("cat /proc/sys/kernel/random/boot_id").strip() != old_boot
    remote("test ! -e /run/mmx/services")
    assert body() == "after interruption\n"
    run("run")
    assert activations() == "4"
    mark("real reboot clears service records; an unchanged run does not reload again")
    remote("rm -f /etc/systemd/system/nginx.service.d/mmx-test.conf; systemctl daemon-reload")

    # The reboot task patches, reboots when the guard asks, and continues on
    # the new boot. A reboot clears /run, as it does /var/run/reboot-required.
    required = "/run/mmx-test-reboot-required"
    write_project(workspace, playbook_name='configure', **{"project_name": "reboot_acceptance", 'default_inventory': 'example', "fleet_stages": [
        {"id": "patch", "nodes": "all", "node_tasks": [
            {"shell": {"script": "echo patched >> /var/tmp/mmx-integration/patched"}},
            {"id": "restart", "reboot": {"run_only_if_this_command_succeeds": "test -f " + required, "wait_at_most": "3m"}}]},
        {"id": "after", "nodes": "all", "requires": {"same_node": ["patch"]}, "node_tasks": [
            {"shell": {"script": "cat /proc/sys/kernel/random/boot_id >> /var/tmp/mmx-integration/after-boot"}}]}]})

    def boot_id():
        return remote("cat /proc/sys/kernel/random/boot_id").strip()

    def fleet_stages(result):
        return {e["fleet_stage"]: e["response"] for e in result["entries"]}

    def rebooted(old):
        nonlocal node, ssh
        node, ssh = await_boot(old, name)
        write_inventory(workspace, {"nodes": {"test": node}}, name='example')
        return boot_id()

    remote("touch " + required)
    first_boot = boot_id()
    preview = fleet_stages(run("dryrun"))
    assert preview["patch"]["results"][-1]["change"] == "would_change" and boot_id() == first_boot, preview
    applied = fleet_stages(run("run"))
    second_boot = rebooted(first_boot)
    restart = applied["patch"]["results"][-1]
    assert second_boot != first_boot and restart["status"] == "succeeded" and restart["change"] == "changed", applied
    assert restart["value"] == {"boot_id_before": first_boot, "boot_id_after": second_boot}, restart
    assert applied["after"]["status"] == "succeeded"
    assert remote("cat /var/tmp/mmx-integration/after-boot").strip() == second_boot
    mark("reboot task previews, reboots when its guard allows, and the next fleet stage runs on the new boot")

    rerun = fleet_stages(run("run"))
    assert rerun["patch"]["results"][-1]["status"] == "skipped" and rerun["after"]["status"] == "succeeded"
    assert boot_id() == second_boot
    mark("a rerun whose guard no longer asks for a reboot skips it without rebooting")

    # Hold the next boot for 20 seconds before SSH and the endpoint start, so
    # the run is certainly in its reboot wait when the controller is lost;
    # some guests reboot faster than one status poll.
    remote("cat > /etc/systemd/system/mmx-test-slow-boot.service <<'UNIT'\n"
           "[Unit]\nDefaultDependencies=no\nBefore=sshd.service ssh.service mmx-https-api.service\n"
           "[Service]\nType=oneshot\nExecStart=/bin/sleep 20\n[Install]\nWantedBy=multi-user.target\nUNIT\n"
           "systemctl daemon-reload; systemctl enable mmx-test-slow-boot.service")
    remote("touch " + required)
    process = background()
    deadline = time.monotonic() + 60
    while fleet_stages(run("results")).get("patch", {}).get("status") != "rebooting":
        if time.monotonic() >= deadline or process.poll() is not None:
            process.kill()
            raise AssertionError("run did not reach its reboot wait: " + repr(process.communicate(timeout=10))[-3000:])
        time.sleep(0.2)
    process.kill()
    process.communicate(timeout=10)
    third_boot = rebooted(second_boot)
    collected = fleet_stages(run("status"))
    assert collected["patch"]["status"] == "succeeded" and collected["patch"]["results"][-1]["value"]["boot_id_after"] == third_boot, collected
    resumed = run("resume")
    assert resumed["status"] == "succeeded" and fleet_stages(resumed)["after"]["status"] == "succeeded"
    assert boot_id() == third_boot
    assert remote("wc -l < /var/tmp/mmx-integration/patched").strip() == "3"
    assert remote("tail -n 1 /var/tmp/mmx-integration/after-boot").strip() == third_boot
    mark("controller lost during the reboot wait: status records the new boot and resume continues without rebooting again")
    remote("systemctl disable mmx-test-slow-boot.service; rm -f /etc/systemd/system/mmx-test-slow-boot.service; systemctl daemon-reload")

    resource_bundle.test(workspace, run, project, remote, mark)

    (workspace / "results.json").write_text(json.dumps({"passed": passed}, indent=2))
    print(f"{len(passed)} workflows passed; records: {workspace}", flush=True)


def stop():
    """End the test command that holds this test environment or one of its job
    slots, then delete the cloud servers of its hetzner.py runs and the
    guests of its incus.py runs. Nothing outside this environment is
    touched."""
    # A job slot of hetzner.py has its root at <root>/<distribution>/slots/<n>,
    # one of incus.py at <root>/<distribution>/<kind of guest>/slots/<n>.
    # Ending the run's own command ends its slots' commands with it; a slot
    # command whose run command is already gone holds its slot's lock alone.
    slots = [d for pattern in ("*/slots/*", "*/*/slots/*") for d in ROOT.glob(pattern) if d.is_dir()]
    for directory in [ROOT, *sorted(slots)]:
        ended = testenv.stop_run(directory)
        if ended:
            print(f"Ended the test command of {directory}: {ended}", flush=True)
    failures = 0
    # An ended hetzner.py run may not have deleted its servers; its manifest
    # says whether it did.
    for manifest in sorted((ROOT / "cloud-runs").glob("*/ownership.json")):
        if not json.loads(manifest.read_text()).get("cleanup_verified"):
            code = subprocess.run([sys.executable, "-u", str(Path(__file__).with_name("hetzner.py")),
                                   "--cleanup-from-manifest", str(manifest)], env=testenv.environment()).returncode
            print(f"Cloud servers of {manifest.parent.name}: " + ("deleted" if code == 0 else
                  f"not deleted; set MMX_TEST_HETZNER_API_TOKEN to a Hetzner Cloud API token with read and write access, then run hetzner.py --cleanup-from-manifest {manifest}"), flush=True)
            failures += code != 0
    for manifest in sorted((ROOT / "incus-runs").glob("*/ownership.json")):
        if not json.loads(manifest.read_text()).get("cleanup_verified"):
            code = subprocess.run([sys.executable, "-u", str(Path(__file__).with_name("incus.py")),
                                   "--cleanup-from-manifest", str(manifest)], env=testenv.environment()).returncode
            print(f"Incus guests of {manifest.parent.name}: " + ("deleted" if code == 0 else
                  f"not deleted; run incus.py --cleanup-from-manifest {manifest}"), flush=True)
            failures += code != 0
    return 1 if failures else 0


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("action", choices=["up", "test", "reset"])
    parser.add_argument("--vm", choices=["mmx-dev", "mmx-peer"], default=NAME)
    args = parser.parse_args()
    action = args.action
    ROOT.mkdir(parents=True, exist_ok=True)
    if action == "up": up(args.vm)
    elif action == "test": tests(args.vm)
    elif action == "reset": reset(args.vm)


if __name__ == "__main__":
    if sys.argv[1:] == ["stop"]:
        # Before the lock: the command to end is the one that holds it.
        sys.exit(stop())
    run_suite(main)
