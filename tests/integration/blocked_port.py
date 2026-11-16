#!/usr/bin/env python3
"""Public CLI checks against a node whose own firewall really blocks port 7443.

The other suites reach the node HTTPS API through an SSH tunnel that ends at
the node's loopback address, which no firewall filters. Here the tunnel ends
in a second network namespace on the node, joined to the node by a virtual
cable. A connection from that namespace to port 7443 arrives on a real
network interface, so firewalld or ufw decides about it as about a connection
from the controller machine. The suite also takes down the interface that a
rule for the port names, as a WireGuard interface goes down, and stops the
node HTTPS API process, which the next run starts again.
"""
import json
import shlex
import shutil
import socket
import subprocess
import time

import vm

NAMESPACE = 'mmx-outside'
NODE_ADDRESS, OUTSIDE_ADDRESS = '10.99.74.1', '10.99.74.2'
PROXY_PORT = 17443
PROXY = 'mmx-test-outside-proxy'
MANAGED = '/var/tmp/mmx-integration/blocked-port'

# One firewall for each package manager: the one its users run.
FIREWALLS = {
    # The test makes sure that firewalld stays off after its installation,
    # so that the firewall does not start at the next boot. Its rule for the
    # port is a runtime rule for the default zone, never made permanent.
    'dnf': {'name': 'firewalld', 'program': 'firewall-cmd',
            'install': 'dnf install -y firewalld >/dev/null && systemctl disable firewalld',
            'active': 'systemctl is-active --quiet firewalld',
            # systemctl start returns before firewalld has loaded its rules;
            # --state answers "running" once it has.
            'start': 'systemctl start firewalld && for second in $(seq 30); do firewall-cmd --state >/dev/null 2>&1 && break; sleep 1; done',
            'stop': 'systemctl stop firewalld',
            'open_on_interface': 'firewall-cmd --add-port=7443/tcp'},
    'apt-get': {'name': 'ufw', 'program': 'ufw',
                'install': 'DEBIAN_FRONTEND=noninteractive apt-get install -y ufw >/dev/null',
                'active': "ufw status | grep -q '^Status: active'",
                'start': 'ufw allow 22/tcp && ufw --force enable', 'stop': 'ufw --force disable',
                'open_on_interface': 'ufw allow in on mmx-node-side to any port 7443 proto tcp'},
}

# systemd opens the listening socket on the node's loopback address and starts
# this relay in the other namespace, so its connections to port 7443 leave from
# there. (systemd-socket-proxyd would do; SELinux allows it only a few ports.)
RELAY = f'''
import socket, threading

def copy(source, destination):
    try:
        while True:
            data = source.recv(65536)
            if not data:
                break
            destination.sendall(data)
    except OSError:
        pass
    for end in (source, destination):
        try:
            end.shutdown(socket.SHUT_RDWR)
        except OSError:
            pass

def serve(client):
    try:
        node = socket.create_connection(("{NODE_ADDRESS}", 7443), timeout=10)
    except OSError:
        client.close()
        return
    node.settimeout(None)
    threading.Thread(target=copy, args=(node, client), daemon=True).start()
    copy(client, node)

listener = socket.socket(fileno=3)
while True:
    threading.Thread(target=serve, args=(listener.accept()[0],), daemon=True).start()
'''

CLEANUP = f'''
systemctl stop {PROXY}.socket {PROXY}.service 2>/dev/null || true
rm -f /run/systemd/system/{PROXY}.socket /run/systemd/system/{PROXY}.service /run/{PROXY}.py
systemctl daemon-reload
ip link delete mmx-node-side 2>/dev/null || true
ip netns delete {NAMESPACE} 2>/dev/null || true
'''


def main():
    vm.up()
    node, ssh = vm.connection()
    vm.reset()
    workspace = vm.ROOT / 'blocked-port'
    if workspace.exists():
        shutil.rmtree(workspace)
    workspace.mkdir()
    vm.build()
    mmx = vm.ROOT / 'bin/mmx'
    forward = None

    def remote(script):
        return vm.cmd([*ssh, 'sudo -n /bin/sh -c ' + shlex.quote('set -eu\n' + script)]).stdout

    def run():
        result = vm.cmd([mmx, 'run', 'configure', '--json'], cwd=workspace, check=False)
        assert result.returncode == 0, (result.stdout, result.stderr)
        return json.loads(result.stdout)['entries'][0], result.stderr

    def reachable_from_outside():
        """Whether a connection from the other namespace to port 7443 is accepted."""
        return remote(f'if ip netns exec {NAMESPACE} timeout 3 bash -c "exec 3<>/dev/tcp/{NODE_ADDRESS}/7443" 2>/dev/null; '
                      'then echo yes; else echo no; fi').strip() == 'yes'

    manager = remote('for manager in dnf apt-get; do if command -v $manager >/dev/null; then echo $manager; break; fi; done').strip()
    firewall = FIREWALLS[manager]
    remote(CLEANUP)
    was_active = False
    try:
        remote(f'command -v {firewall["program"]} >/dev/null || {{ {firewall["install"]}; }}')
        # The first check needs the firewall off; the end restores what was found.
        was_active = remote(f'if {firewall["active"]}; then echo yes; fi').strip() == 'yes'
        remote(firewall['stop'])
        remote(f'ip netns add {NAMESPACE}\n'
               f'ip link add mmx-node-side type veth peer name mmx-outside netns {NAMESPACE}\n'
               f'ip address add {NODE_ADDRESS}/30 dev mmx-node-side\nip link set mmx-node-side up\n'
               f'ip -n {NAMESPACE} address add {OUTSIDE_ADDRESS}/30 dev mmx-outside\n'
               f'ip -n {NAMESPACE} link set mmx-outside up\nip -n {NAMESPACE} link set lo up\n'
               f"cat > /run/{PROXY}.py <<'RELAY'{RELAY}RELAY\n"
               f"printf '[Socket]\\nListenStream=127.0.0.1:{PROXY_PORT}\\n' > /run/systemd/system/{PROXY}.socket\n"
               f"printf '[Service]\\nNetworkNamespacePath=/run/netns/{NAMESPACE}\\n"
               f"ExecStart=/usr/bin/python3 /run/{PROXY}.py\\n' > /run/systemd/system/{PROXY}.service\n"
               f'systemctl daemon-reload\nsystemctl start {PROXY}.socket')
        with socket.socket() as listener:
            listener.bind(('127.0.0.1', 0))
            controller_port = listener.getsockname()[1]
        forward = subprocess.Popen([str(a) for a in [*vm.unshared(ssh[:-1]), '-N', '-o', 'ExitOnForwardFailure=yes',
                                                    '-L', f'127.0.0.1:{controller_port}:127.0.0.1:{PROXY_PORT}', ssh[-1]]],
                                   env=vm.ENV, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        deadline = time.monotonic() + 5
        while True:
            assert forward.poll() is None, 'the tunnel to the other namespace failed'
            try:
                with socket.create_connection(('127.0.0.1', controller_port), timeout=0.1):
                    break
            except OSError:
                assert time.monotonic() < deadline
                time.sleep(0.05)
        vm.write_inventory(workspace, {'nodes': {'app': dict(node, https_address=f'https://127.0.0.1:{controller_port}')}}, name='example')
        vm.write_project(workspace, playbook_name='configure', **{
            'project_name': 'blocked_port', 'default_inventory': 'example',
            'fleet_stages': [{'id': 'configure', 'nodes': 'all', 'node_tasks': [
                {'file': {"permissions_mode": "0644", 'path': MANAGED, 'content': 'configured\n'}}]}]})

        # Without a firewall the path through the other namespace reaches the API.
        entry, _ = run()
        assert entry['connection_used'] == 'https', entry
        assert reachable_from_outside()
        print('PASS with no firewall running, HTTPS from outside the node reaches port 7443', flush=True)

        def fallback_reason():
            """The notice of a run that fell back to SSH, after checking that the
            run succeeded and that the notice says what MMX saw and guesses no
            cause."""
            entry, notice = run()
            assert entry['connection_used'] == 'ssh_fallback', entry
            assert entry['response']['status'] == 'succeeded', entry
            reason = entry['connection_fallback_reason']
            assert f' https://127.0.0.1:{controller_port}' in reason and f'reach 127.0.0.1 on TCP port {controller_port}:' in reason, reason
            assert reason in notice and 'connection: ssh_only' in notice, notice
            for guess in ('blocks', 'something between'):
                assert guess not in reason, reason
            return reason

        remote(firewall['start'])
        assert not reachable_from_outside(), 'the firewall does not block port 7443'
        reason = fallback_reason()
        described = ("firewalld's default zone on this node has no rule for port 7443" if firewall['name'] == 'firewalld'
                     else f"{firewall['name']} on this node is active with no rule that names port 7443")
        assert (f". mmx-runner, MMX's HTTPS service on this node, is running, and {described}. "
                'MMX is using SSH for this node.') in reason, reason
        assert remote('cat ' + MANAGED) == 'configured\n'
        # MMX reports what it found and never opens the port.
        assert not reachable_from_outside()
        print(f'PASS {firewall["name"]} without a rule for port 7443: the run succeeds over SSH, says what MMX saw and that {firewall["name"]} has no rule for the port, and leaves the port closed', flush=True)

        # A rule for the port limited to one interface, and that interface is
        # down, as when WireGuard stops: the firewall has a rule for the port, so the
        # notice says only what MMX saw and that the process runs.
        remote(firewall['open_on_interface'] + '\nip link set mmx-node-side down')
        assert not reachable_from_outside()
        reason = fallback_reason()
        assert ". mmx-runner, MMX's HTTPS service on this node, is running. MMX is using SSH for this node." in reason and firewall['name'] not in reason, reason
        print('PASS a rule for port 7443 on an interface that is down: the run succeeds over SSH, and the notice says what MMX saw and names no cause', flush=True)

        remote('ip link set mmx-node-side up')
        # The interface comes up a moment after the command returns, later on a busy machine.
        deadline = time.monotonic() + 10
        while not reachable_from_outside():
            assert time.monotonic() < deadline, 'port 7443 stayed unreachable after the interface came up'
            time.sleep(0.5)
        entry, notice = run()
        assert entry['connection_used'] == 'https' and 'connection_fallback_reason' not in entry, entry
        assert 'MMX is using SSH' not in notice, notice
        print('PASS once the interface is up, its rule admits port 7443 and the next run uses HTTPS', flush=True)

        # MMX's own process stopped: the next run starts it again through
        # verified SSH and uses HTTPS.
        remote('systemctl stop mmx-https-api.service')
        entry, notice = run()
        assert entry['connection_used'] == 'https' and 'connection_fallback_reason' not in entry, entry
        assert "mmx-runner, MMX's HTTPS service on this node, was not running (its systemd unit mmx-https-api.service was inactive); MMX started it again through verified SSH" in notice, notice
        assert remote('systemctl is-active mmx-https-api.service').strip() == 'active'
        print('PASS a stopped node HTTPS API process: the next run starts it again and uses HTTPS', flush=True)
    finally:
        if forward is not None:
            forward.terminate()
            forward.wait(timeout=5)
        # The firewalld rule was never made permanent, and vm.reset() removes ufw's rules.
        remote(CLEANUP + firewall['stop'])
        vm.reset()
        if was_active:
            remote(firewall['start'])


if __name__ == '__main__':
    vm.run_suite(main)
