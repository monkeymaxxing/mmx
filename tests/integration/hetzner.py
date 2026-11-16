#!/usr/bin/env python3
"""Run the VM suites on disposable Hetzner Cloud servers; always delete them.

Each job slot gets a new pair of servers (mmx-dev and mmx-peer) of one
distribution and deletes them when that distribution has no suites left.
Requires MMX_TEST_HETZNER_API_TOKEN (a Hetzner Cloud API token) and
MMX_TEST_HETZNER_SSH_KEY_NAME (a key registered in that Cloud project).
The ownership manifest supports cleanup even after this process dies.
"""
import argparse
from concurrent.futures import ThreadPoolExecutor
from contextlib import contextmanager
import importlib
import json
import os
from pathlib import Path
from queue import Empty
import signal
import subprocess
import threading
import time
import traceback
import urllib.error
import urllib.parse
import urllib.request
import uuid

import testenv
import vm

# all.py's name would hide Python's all() here.
suite_runner = importlib.import_module('all')

API = 'https://api.hetzner.cloud/v1'
PROJECT = 'mmx-testing'
# Hetzner's image of each distribution for AMD64 (x86) servers, and for
# ARM64 servers. --server-architecture chooses between them.
IMAGES = {
    'debian12': 114690387, 'debian13': 310554929,
    'ubuntu2404': 161547269, 'ubuntu2604': 387894169,
    'rocky9': 76766499, 'almalinux10': 245249013,
}
ARM64_IMAGES = {
    'debian12': 114690389, 'debian13': 310557660,
    'ubuntu2404': 161547270, 'ubuntu2604': 387894171,
    'rocky9': 103907769, 'almalinux10': 245249165,
}
IMAGES_FOR_SERVER_ARCHITECTURE = {'amd64': IMAGES, 'arm64': ARM64_IMAGES}
# Two CPUs and 4 GiB: the cheaper type first, the other when Hetzner has
# sold out of it. Hetzner's ARM64 servers have one such type.
SERVER_TYPES = {'amd64': ('cx23', 'cpx22'), 'arm64': ('cax11',)}
# node_facts.py moves a server to the first of these that its location has
# and back, for its real resize.
LARGER_SERVER_TYPES = {'amd64': ('cx33', 'cpx32'), 'arm64': ('cax21', 'cax31')}
# Try Helsinki first, then the other locations when Helsinki has no room. A
# location that has a larger type now comes before one that has none, see
# locations_for_new_servers().
LOCATIONS = ('hel1', 'fsn1', 'nbg1')
# The account the harness logs in as: not root, with sudo.
LOGIN = 'mmx-test'
# Error codes for a create request that only needs room: the project's server
# limit, and a location that has no capacity for the server type.
LIMIT_CODES = {'resource_limit_exceeded'}
# Default limit for one run; --max-servers-at-once overrides it.
DEFAULT_MAX_SERVERS_AT_ONCE = 12
UNAVAILABLE_CODES = {'resource_unavailable', 'placement_error'}


class HetznerError(RuntimeError):
    def __init__(self, method, path, status, code):
        # Never include submitted cloud-init data (host private keys) in errors.
        super().__init__(f'Hetzner {method} {path}: HTTP {status} {code}')
        self.code = code


class NoRoom(Exception):
    """Hetzner has no room for another server right now."""


def api(method, path, body=None):
    token = os.environ.get('MMX_TEST_HETZNER_API_TOKEN')
    if not token:
        raise RuntimeError('MMX_TEST_HETZNER_API_TOKEN is not set; set it to a Hetzner Cloud API token with read and write access to the test project')
    data = None if body is None else json.dumps(body).encode()
    for attempt in range(6):
        request = urllib.request.Request(API + path, data=data, method=method, headers={
            'Authorization': 'Bearer ' + token, 'Content-Type': 'application/json'})
        try:
            with urllib.request.urlopen(request, timeout=30) as response:
                return json.load(response)
        except urllib.error.HTTPError as error:
            if error.code == 404:
                return None
            try:
                code = json.load(error)['error']['code']
            except (ValueError, KeyError, TypeError):
                code = 'unknown'
            # The API allows 3600 requests an hour; a rejected request changed nothing.
            if code == 'rate_limit_exceeded' and attempt < 5:
                time.sleep(10)
                continue
            raise HetznerError(method, path, error.code, code) from None


def save(path, data):
    path.write_text(json.dumps(data, indent=2) + '\n')
    path.chmod(0o600)


def owned(manifest):
    selector = 'project=' + PROJECT + ',run=' + manifest['run']
    servers, page = [], 1
    while page:
        result = api('GET', f'/servers?label_selector={selector}&per_page=50&page={page}')
        servers += result['servers']
        page = ((result.get('meta') or {}).get('pagination') or {}).get('next_page')
    return servers


def delete(path, manifest, servers):
    """Delete servers of this run after checking that they are still its own
    and that nothing billable would stay behind."""
    for server in servers:
        if server['labels'] != {'project': PROJECT, 'run': manifest['run']}:
            raise RuntimeError('server ownership changed; refusing deletion')
        if server['volumes'] or server['backup_window']:
            raise RuntimeError('test server has unexpected billable attachments')
        for version in ('ipv4', 'ipv6'):
            address = server['public_net'][version]
            if address:
                resource = api('GET', '/primary_ips/' + str(address['id']))
                if not resource['primary_ip']['auto_delete']:
                    raise RuntimeError('primary IP must be deleted with its server')
                manifest.setdefault('primary_ips', []).append(address['id'])
        save(path, manifest)
        api('DELETE', '/servers/' + str(server['id']))


def cleanup(path):
    manifest = json.loads(path.read_text())
    if manifest['project'] != PROJECT:
        raise RuntimeError('manifest is not owned by MMX testing')
    # Query by run as well as IDs: creation may have succeeded before a response
    # or manifest write was interrupted, and such a server can appear late.
    requested = set()
    deadline = time.monotonic() + 120
    while time.monotonic() < deadline:
        servers = owned(manifest)
        late = [server for server in servers if server['id'] not in requested]
        delete(path, manifest, late)
        requested.update(server['id'] for server in late)
        if not servers and all(api('GET', '/primary_ips/' + str(i)) is None for i in manifest.get('primary_ips', [])):
            manifest['cleanup_verified'] = True
            save(path, manifest)
            # The login key opened only these servers.
            for key in ('login-key', 'login-key.pub'):
                (path.parent / key).unlink(missing_ok=True)
            print('Cleanup verified: no owned servers or primary IPs remain', flush=True)
            return
        time.sleep(2)
    raise RuntimeError('cloud cleanup incomplete; run hetzner.py --cleanup-from-manifest ' + str(path))


def registered_key():
    key_name = os.environ.get('MMX_TEST_HETZNER_SSH_KEY_NAME')
    if not key_name:
        raise RuntimeError('MMX_TEST_HETZNER_SSH_KEY_NAME is not set; set it to the name of an SSH key registered in the Hetzner Cloud test project')
    keys = api('GET', '/ssh_keys?' + urllib.parse.urlencode({'name': key_name}))['ssh_keys']
    if len(keys) != 1:
        raise RuntimeError('MMX_TEST_HETZNER_SSH_KEY_NAME must name exactly one registered SSH key in the Hetzner Cloud test project')
    # When a local public key exists, it must match the selected registered key.
    local = Path.home() / '.ssh/id_rsa.pub'
    if local.exists() and keys[0]['public_key'].split()[:2] != local.read_text().split()[:2]:
        raise RuntimeError('the SSH key selected by MMX_TEST_HETZNER_SSH_KEY_NAME must match ~/.ssh/id_rsa.pub on this machine')
    return keys[0]['id']


def finished(started):
    """Wait until a Hetzner action finishes; return its error, or None when it
    succeeded."""
    deadline = time.monotonic() + 600
    while started['status'] == 'running':
        if time.monotonic() > deadline:
            return {'code': 'timeout', 'message': f'action {started["command"]} did not finish'}
        time.sleep(2)
        started = api('GET', f'/actions/{started["id"]}')['action']
    return None if started['status'] == 'success' else (started.get('error') or {'code': started['status']})


def action(server_id, name, body=None):
    """Start a server action and wait until Hetzner reports it finished."""
    error = finished(api('POST', f'/servers/{server_id}/actions/{name}', body or {})['action'])
    if error:
        raise RuntimeError(f'Hetzner {name} of server {server_id}: {error}')


def available(server_type, location):
    """Whether Hetzner can give a server of this type in this location now."""
    types = api('GET', '/server_types?name=' + server_type)['server_types']
    return any(place['name'] == location and place['available'] for t in types for place in t['locations'])


def locations_for_new_servers(server_architecture):
    """LOCATIONS, those where Hetzner has a larger server type now first.
    A server can change its type only within its own location, so a server
    in a location without one could not run node_facts.py's resize."""
    with_larger = {place['name']
                   for name in LARGER_SERVER_TYPES[server_architecture]
                   for server_type in api('GET', '/server_types?name=' + name)['server_types']
                   for place in server_type['locations'] if place['available']}
    return sorted(LOCATIONS, key=lambda location: location not in with_larger)


def change_server_type(server_id, location, candidates):
    """Shut the server down, give it the first of the candidate types its
    location has, keeping its disk so that the change can be undone, and
    start it again, also when the change fails. Return the type it got, or
    None when the location has none of them; the server then keeps running."""
    if api('GET', f'/servers/{server_id}')['server']['labels'].get('project') != PROJECT:
        raise RuntimeError(f'server {server_id} is not an MMX test server')
    chosen = next((t for t in candidates if available(t, location)), None)
    if chosen is None:
        return None
    action(server_id, 'shutdown')
    try:
        deadline = time.monotonic() + 120
        while api('GET', f'/servers/{server_id}')['server']['status'] != 'off':
            if time.monotonic() > deadline:
                action(server_id, 'poweroff')
                break
            time.sleep(2)
        action(server_id, 'change_type', {'server_type': chosen, 'upgrade_disk': False})
    finally:
        action(server_id, 'poweron')
    return chosen


class Run:
    """One invocation: its ownership manifest and login key, and the waiting
    line its slots share when the project has no room for more servers."""

    def __init__(self, max_servers_at_once=None, server_architecture='amd64'):
        self.ssh_key_id = registered_key()
        self.server_architecture = server_architecture
        # Limit this run independently of the Cloud project's server quota.
        self.max_servers_at_once = max_servers_at_once
        self.creating = 0
        self.name = 'mmx-' + time.strftime('%Y%m%d-%H%M%S') + '-' + uuid.uuid4().hex[:6]
        self.directory = vm.ROOT / 'cloud-runs' / self.name
        self.directory.mkdir(parents=True, mode=0o700)
        self.manifest_path = self.directory / 'ownership.json'
        self.manifest = {'project': PROJECT, 'run': self.name, 'servers': []}
        save(self.manifest_path, self.manifest)
        print('Ownership manifest: ' + str(self.manifest_path), flush=True)
        existing = Path.home() / '.ssh/id_rsa'
        if existing.exists():
            # Use the existing login key. registered_key() checks its public
            # key when present. The private key never leaves this machine.
            self.login_key = existing
        else:
            # A key for this run's servers only; cleanup deletes it with them.
            self.login_key = self.directory / 'login-key'
            subprocess.run(['ssh-keygen', '-q', '-t', 'ed25519', '-N', '', '-C', LOGIN + ' ' + self.name,
                            '-f', str(self.login_key)], check=True)
        self.lock = threading.Lock()  # the manifest and the counts
        self.room = threading.Lock()  # held by the slot at the head of the waiting line
        self.released = threading.Event()  # set whenever this run deletes servers
        self.stopping = threading.Event()
        self.queues = []
        self.live = 0
        self.most = 0
        self.no_room_reported = False

    def watch(self, pending):
        with self.lock:
            if pending not in self.queues:
                self.queues.append(pending)

    def stop(self):
        """Start no further suites or servers."""
        self.stopping.set()
        for pending in self.queues:
            while True:
                try:
                    pending.get_nowait()
                except Empty:
                    break

    def no_room(self, reason):
        with self.lock:
            if self.no_room_reported:
                return
            self.no_room_reported = True
            # The run's last line gives the most servers it held at once.
            print(f'No room for more Hetzner servers ({reason}); slots wait for servers to be deleted',
                  flush=True)


def create_server(run, distribution, slot, role):
    """Create one server and record it in the manifest. Raise NoRoom when
    the project is at its limit or no location has room."""
    if run.stopping.is_set():
        raise RuntimeError('the run is stopping')
    with run.lock:
        if run.max_servers_at_once and run.live + run.creating >= run.max_servers_at_once:
            raise NoRoom(f'this run holds at most {run.max_servers_at_once} servers at once')
        run.creating += 1
    try:
        return create_counted_server(run, distribution, slot, role)
    finally:
        with run.lock:
            run.creating -= 1


def create_counted_server(run, distribution, slot, role):
    # Generate only disposable SERVER host keys, so SSH verifies each server
    # from its first connection. The API already has the private key.
    host_key = run.directory / f'{distribution}-{slot}-{role}-host-key'
    host_public = Path(str(host_key) + '.pub')
    subprocess.run(['ssh-keygen', '-q', '-t', 'ed25519', '-N', '', '-C', role, '-f', str(host_key)], check=True)
    try:
        config = {
            'ssh_keys': {'ed25519_private': host_key.read_text(), 'ed25519_public': host_public.read_text()},
            'users': [{'name': LOGIN, 'shell': '/bin/bash', 'sudo': 'ALL=(ALL) NOPASSWD:ALL',
                       'ssh_authorized_keys': [Path(str(run.login_key) + '.pub').read_text().strip()]}],
            # A package mirror that stalls must not hold up the first boot or a
            # test: dnf leaves a transfer slower than 200 kB/s after 10 seconds
            # and takes the next mirror, as in incus.py's guests. bootcmd runs
            # before package_update.
            'bootcmd': ["if [ -f /etc/dnf/dnf.conf ] && ! grep -q '^minrate=' /etc/dnf/dnf.conf; then "
                        "printf 'minrate=200k\\ntimeout=10\\n' >> /etc/dnf/dnf.conf; fi"],
            'package_update': True, 'packages': ['sudo'],
            'write_files': [{'path': '/etc/mmx-test-vm', 'content': vm.MARKER + '\n'}],
            # Hetzner's Red Hat family images run SELinux permissive, with
            # files that lack their labels; a default installation and
            # incus.py's guests enforce it on labeled files. Without the labels,
            # timedated may not write /etc/localtime ("Access denied").
            'runcmd': ['if [ "$(getenforce 2>/dev/null)" = Permissive ]; then '
                       "sed -i 's/^SELINUX=.*/SELINUX=enforcing/' /etc/selinux/config; "
                       'fixfiles -F restore >/dev/null 2>&1; setenforce 1; fi'],
        }
        public = host_public.read_text()
        server_types = SERVER_TYPES[run.server_architecture]
        image = IMAGES_FOR_SERVER_ARCHITECTURE[run.server_architecture][distribution]
        locations = locations_for_new_servers(run.server_architecture)
        choices = [(t, location) for t in server_types for location in locations]
        for server_type, location in choices:
            try:
                created = api('POST', '/servers', {
                    'name': f'{run.name}-{distribution}-{slot}-{role.removeprefix("mmx-")}',
                    'server_type': server_type, 'location': location, 'image': image,
                    # The registered key keeps Hetzner from making a root password.
                    'ssh_keys': [run.ssh_key_id], 'start_after_create': True,
                    'labels': {'project': PROJECT, 'run': run.name},
                    'user_data': '#cloud-config\n' + json.dumps(config),
                })
            except HetznerError as error:
                if error.code in LIMIT_CODES:
                    raise NoRoom(error.code) from None
                if error.code not in UNAVAILABLE_CODES:
                    raise
                continue
            # Hetzner can accept the request and still fail the creation when
            # the location runs out of room meanwhile; it then removes the
            # server and gives its address to the next one.
            failure = finished(created['action'])
            if failure is None:
                server = created['server']
                break
            leftover = api('GET', f'/servers/{created["server"]["id"]}')
            if leftover:
                with run.lock:
                    delete(run.manifest_path, run.manifest, [leftover['server']])
            if failure.get('code') not in UNAVAILABLE_CODES:
                raise RuntimeError(f'Hetzner could not create a server: {failure}')
        else:
            raise NoRoom(f'no {" or ".join(server_types)} available in ' + ', '.join(locations))
    finally:
        host_key.unlink(missing_ok=True)
        host_public.unlink(missing_ok=True)
    entry = {'id': server['id'], 'name': server['name'], 'distribution': distribution, 'slot': slot, 'role': role,
             'image': image, 'server_type': server_type, 'location': location,
             'address': server['public_net']['ipv4']['ip'],
             'created': time.time()}
    with run.lock:
        run.manifest['servers'].append(entry)
        save(run.manifest_path, run.manifest)
        run.live += 1
        run.most = max(run.most, run.live)
    return dict(entry, host_public=public)


def release(run, servers, wake_waiting_slots=True):
    """Delete one slot's servers and wait until they are gone. A slot that
    deletes the first server of a pair that found no room wakes nobody: the
    room it frees is the room it just failed to fill."""
    if not servers:
        return
    with run.lock:
        current = [found['server'] for found in (api('GET', f'/servers/{s["id"]}') for s in servers) if found]
        delete(run.manifest_path, run.manifest, current)
    deadline = time.monotonic() + 120
    while any(api('GET', f'/servers/{s["id"]}') for s in servers):
        if time.monotonic() > deadline:
            raise RuntimeError('cloud servers were not deleted: ' + ', '.join(s['name'] for s in servers))
        time.sleep(2)
    with run.lock:
        for entry in run.manifest['servers']:
            if any(entry['id'] == s['id'] for s in servers):
                entry['deleted'] = time.time()
        save(run.manifest_path, run.manifest)
        run.live -= len(servers)
    if wake_waiting_slots:
        run.released.set()


def create_pair(run, distribution, slot, servers):
    """Create the slot's mmx-dev and mmx-peer into servers. Return False,
    with servers empty again, when the project has no room for both."""
    try:
        for role in ('mmx-dev', 'mmx-peer'):
            servers.append(create_server(run, distribution, slot, role))
        return True
    except NoRoom as reason:
        release(run, servers, wake_waiting_slots=False)
        servers.clear()
        run.no_room(reason)
        return False


def new_pair(run, distribution, slot, pending, servers):
    """Create the slot's servers, waiting while the project has no room.
    Leave servers empty when the suites run out first."""
    if create_pair(run, distribution, slot, servers):
        return
    # Waiting slots form a line. Only the slot at its head asks Hetzner again:
    # whenever this run deletes servers, and at least every 30 seconds.
    while not pending.empty() and not run.stopping.is_set():
        if not run.room.acquire(timeout=10):
            continue
        try:
            while not pending.empty() and not run.stopping.is_set():
                run.released.clear()
                if create_pair(run, distribution, slot, servers):
                    return
                run.released.wait(30)
        finally:
            run.room.release()


def wait_until_ready(run, node):
    deadline = time.monotonic() + 600
    while True:
        if run.stopping.is_set():
            raise RuntimeError('the run is stopping')
        # The Red Hat family keeps cloud-init's state from other accounts, so
        # the status needs sudo, which cloud-init sets up for the login.
        result = vm.cmd([*vm.ssh_args(node), 'if sudo -n true 2>/dev/null; then sudo -n cloud-init status --wait; '
                         'else exit 255; fi'], check=False, timeout=600)
        # 2: cloud-init finished with a recoverable error, which its log has.
        if result.returncode in (0, 2):
            break
        # 255: SSH cannot connect yet, or the login account or its sudo is not
        # there yet.
        if result.returncode != 255:
            detail = vm.cmd([*vm.ssh_args(node), 'sudo -n cloud-init status --long'], check=False).stdout
            raise RuntimeError(f'cloud-init on {node["address"]} exited {result.returncode}:\n{detail.strip()[-2000:]}')
        if time.monotonic() > deadline:
            raise RuntimeError(f'cloud server did not become ready: {node["address"]}: {result.stderr.strip()[-500:]}')
        time.sleep(3)
    if vm.cmd([*vm.ssh_args(node), 'sudo -n cat /etc/mmx-test-vm']).stdout.strip() != vm.MARKER:
        raise RuntimeError('target is missing its disposable-test marker: ' + node['address'])
    # cloud-init has done its one job, as in incus.py; later boots skip it. On
    # Ubuntu 26.04 ARM64 its local stage races the network card's rename
    # from enp1s0 to eth0 on some reboots and fails, which leaves the server
    # degraded.
    vm.cmd([*vm.ssh_args(node), 'sudo -n touch /etc/cloud/cloud-init.disabled'])


def target_of(run, directory, server):
    """Wait until a server is ready; return its entry in the slot's
    MMX_TEST_TARGETS file, and its platform."""
    known = directory / (server['role'] + '.known_hosts')
    known.write_text(server['address'] + ' ' + server['host_public'])
    known.chmod(0o600)
    node = {'address': server['address'], 'ssh_port': 22, 'ssh_user': LOGIN,
            'ssh_identity_file': str(run.login_key), 'ssh_known_hosts': str(known), 'ssh_config': '/dev/null'}
    wait_until_ready(run, node)
    platform = vm.cmd([*vm.ssh_args(node), '. /etc/os-release; printf "%s, kernel %s, SELinux %s" '
                       '"$PRETTY_NAME" "$(uname -r)" "$(getenforce 2>/dev/null || echo absent)"']).stdout
    return {'node': node, 'platform': platform + ' ' + run.server_architecture.upper(),
            'architecture': run.server_architecture,
            'image': server['image'], 'hetzner_server_id': server['id'],
            'hetzner_server_type': server['server_type'], 'hetzner_location': server['location']}, platform


def ready_targets(run, distribution, slot, servers):
    """Wait until both servers are ready; write and return the slot's
    MMX_TEST_TARGETS file."""
    directory = run.directory / f'{distribution}-{slot}'
    directory.mkdir(mode=0o700, exist_ok=True)
    targets = {}
    for server in servers:
        targets[server['role']], platform = target_of(run, directory, server)
    path = directory / 'targets.json'
    save(path, targets)
    print(f'{distribution}: slot {slot} {servers[0]["server_type"]} servers ready in {servers[0]["location"]}: {platform}',
          flush=True)
    return path


@contextmanager
def third_server(run, distribution, slot, targets_path):
    """all.py's third_machine: one more server, mmx-third, in the slot's
    MMX_TEST_TARGETS file for one suite, deleted afterwards. Give False when
    the project has no room for it or it failed."""
    servers = []
    try:
        try:
            servers.append(create_server(run, distribution, slot, 'mmx-third'))
            targets = json.loads(targets_path.read_text())
            targets['mmx-third'], _ = target_of(run, targets_path.parent, servers[0])
            save(targets_path, targets)
        except Exception as error:
            with suite_runner.OUTPUT_LOCK:
                print(f'{distribution}: slot {slot} has no third server: {error}', flush=True)
            yield False
            return
        yield True
    finally:
        release(run, servers)
        if targets_path.exists():
            targets = json.loads(targets_path.read_text())
            if targets.pop('mmx-third', None) is not None:
                save(targets_path, targets)


@contextmanager
def server_pair(run, distribution, slot, pending):
    """all.py's cloud_servers: new servers for one job slot, deleted when the
    slot is done. Yield None when the suites ran out first or the servers
    failed; the other slots then run the suites."""
    run.watch(pending)
    servers = []
    try:
        targets = None
        try:
            new_pair(run, distribution, slot, pending, servers)
            if servers:
                targets = ready_targets(run, distribution, slot, servers)
        except Exception as error:
            with suite_runner.OUTPUT_LOCK:
                print(f'{distribution}: slot {slot} has no servers: {error}', flush=True)
        yield targets
    finally:
        release(run, servers)


def run_distribution(run, distribution, pairs, suites):
    try:
        return suite_runner.run_all(vm.ROOT / distribution, pairs, suites=suites, label=distribution + ': ',
                                    cloud_servers=lambda slot, pending: server_pair(run, distribution, slot, pending),
                                    third_machine=lambda slot, targets: third_server(run, distribution, slot, targets),
                                    shared_build=run.directory / 'build')
    except Exception:
        with suite_runner.OUTPUT_LOCK:
            print(f'{distribution}: runner failed\n{traceback.format_exc()}', flush=True)
        return 1


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--cleanup-from-manifest', type=Path, metavar='OWNERSHIP_JSON',
                        help="delete a run's servers again, from its ownership.json")
    parser.add_argument('--distribution', action='append', choices=sorted(IMAGES),
                        help='repeat for each distribution to test; default: all of them')
    parser.add_argument('--server-architecture', choices=sorted(SERVER_TYPES), default='amd64',
                        help="the test servers' processor architecture: amd64 (Hetzner's "
                             f"{', '.join(SERVER_TYPES['amd64'])}) or arm64 (Hetzner's "
                             f"{', '.join(SERVER_TYPES['arm64'])}); default amd64")
    parser.add_argument('--pairs-per-distribution', type=int, default=3,
                        help='job slots per distribution, each with its own two servers; default 3')
    parser.add_argument('--suite', action='append', choices=[' '.join(s) for s in suite_runner.SUITES],
                        help='repeat to run only these suites; default: all of them')
    parser.add_argument('--base-suite', action='store_true',
                        help="run the base suite (bootstrap, both connections, recovery, and the plain-named modules; "
                             "BASE_SUITE in all.py), also together with --suite")
    parser.add_argument('--build-and-use-release-archive', metavar='VERSION',
                        help="build release archives from this working tree and use this controller machine's archive")
    parser.add_argument('--max-servers-at-once', type=int, default=DEFAULT_MAX_SERVERS_AT_ONCE,
                        help=f"the most servers this run holds at once; default {DEFAULT_MAX_SERVERS_AT_ONCE}; "
                             "choose a value that fits the available Hetzner project quota")
    args = parser.parse_args()
    if args.pairs_per_distribution < 1:
        parser.error('--pairs-per-distribution must be positive')
    with testenv.locked(vm.ROOT):
        if args.cleanup_from_manifest:
            cleanup(args.cleanup_from_manifest.resolve())
            return 0
        suites = suite_runner.selected_suites(args.suite, args.base_suite)
        # Before any server: a selection that runs nowhere ends here.
        suites_of_distribution = suite_runner.suites_by_distribution(suites, args.distribution or list(IMAGES))
        distributions = list(suites_of_distribution)
        run = Run(args.max_servers_at_once, args.server_architecture)

        def interrupted(signum, frame):
            raise KeyboardInterrupt
        for sig in (signal.SIGTERM, signal.SIGINT):
            signal.signal(sig, interrupted)
        pool = ThreadPoolExecutor(max_workers=len(distributions))
        try:
            if args.build_and_use_release_archive:
                vm.build_and_use_release_archive(run.directory, args.build_and_use_release_archive)
            futures = {d: pool.submit(run_distribution, run, d, args.pairs_per_distribution, suites_of_distribution[d])
                       for d in distributions}
            statuses = {d: future.result() for d, future in futures.items()}
        except BaseException:
            run.stop()
            raise
        finally:
            # A second Ctrl-C or SIGTERM must not cut the deletion short.
            for sig in (signal.SIGTERM, signal.SIGINT):
                signal.signal(sig, signal.SIG_IGN)
            pool.shutdown(wait=True)
            cleanup(run.manifest_path)
        failed = [d for d, status in statuses.items() if status]
        print(f'Cloud run {run.name}: {len(distributions) - len(failed)} of {len(distributions)} distributions passed'
              + (', failed: ' + ', '.join(failed) if failed else '') + f'; at most {run.most} servers at once',
              flush=True)
        return int(bool(failed))


if __name__ == '__main__':
    raise SystemExit(main())
