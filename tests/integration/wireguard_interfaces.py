#!/usr/bin/env python3
"""wireguard_interface on two nodes in a mesh built from one template over
the inventory, with a third member of the mesh that has only its inventory
entry: keys created on the nodes, peers added and removed by a reload while
a ping between the two nodes runs, an Address change that restarts, a peer
removed by hand, key replacement, and removal. A controller process on the
first guest also checks restoration after a bad change cuts SSH over
WireGuard, including a change on the controller machine itself. A last
interface listens on a port below 1024, which Ubuntu 26.04's AppArmor
profile for wg-quick blocks. Node state is checked with wg, ip, systemctl,
stat, and the files themselves."""
from contextlib import ExitStack
import json
import os
import re
import shlex
import subprocess
import time

import vm

NODES = ('a', 'b')
# Each node's address on the WireGuard network; c is the third member,
# which has no machine of its own. The network must be one that the test
# machines' own networks do not use: an Address in the network of a guest's
# own card routes that network, its gateway included, into wg0.
ADDRESSES = {'a': '10.231.88.1', 'b': '10.231.88.2', 'c': '10.231.88.3'}
CONF, KEY = '/etc/wireguard/wg0.conf', '/etc/wireguard/wg0.private_key'
UNIT = 'wg-quick@wg0.service'
# The page's template, with the network of this suite.
TEMPLATE = '''[Interface]
Address = {{ .vars.wireguard_address }}/24
ListenPort = 51820
{{ range $name := .inventory.groups.wireguard_mesh }}
{{- $peer := index $.inventory.nodes $name }}
{{- if and (ne $name $.node.name) (hasKey $peer.vars "wireguard_public_key") }}

[Peer]
# {{ $name }}
PublicKey = {{ $peer.vars.wireguard_public_key }}
AllowedIPs = {{ $peer.vars.wireguard_address }}/32
Endpoint = {{ $peer.address }}:51820
{{- end }}
{{- end }}
'''


def main():
    nodes, connections = {}, {}
    for node_id, name in [('a', 'mmx-dev'), ('b', 'mmx-peer')]:
        vm.up(name)
        nodes[node_id], connections[node_id] = vm.connection(name)
        vm.reset(name)
    workspace = vm.ROOT / 'wireguard-interfaces' / time.strftime('%Y%m%d-%H%M%S')
    workspace.mkdir(parents=True)
    (workspace / 'templates').mkdir()
    (workspace / 'templates/wg0.conf.tmpl').write_text(TEMPLATE)
    vm.build()
    mmx = vm.ROOT / 'bin/mmx'
    passed = []
    # Everything mmx printed, to search for the private keys at the end.
    printed = []
    public_keys = {}
    mesh = list(NODES)

    def remote(node, script, check=True):
        p = vm.cmd([*connections[node], 'sudo -n /bin/sh -c ' + shlex.quote('set -eu\n' + script)], check=check)
        return p.stdout if check else p

    def inventory():
        members = dict(nodes)
        # c is never reached: its address is in TEST-NET-1.
        members['c'] = {'address': '192.0.2.30'}
        for node_id, member in members.items():
            member = dict(member)
            member['vars'] = {'wireguard_address': ADDRESSES[node_id]}
            if node_id in public_keys:
                member['vars']['wireguard_public_key'] = public_keys[node_id]
            members[node_id] = member
        vm.write_inventory(workspace, {'nodes': members, 'groups': {'wireguard_servers': {'nodes': list(NODES)},
                                                                   'wireguard_mesh': {'nodes': mesh}}}, name='example')

    def project():
        vm.write_project(workspace, project_name='wireguard_interfaces', playbook_name='private_network',
                         node_task_defaults={'retry': {'pause_between_tries': '0s'}}, fleet_stages=[
            {'id': 'private_network', 'nodes': 'wireguard_servers', 'node_tasks': [
                *vm.package_tasks(['wireguard-tools', 'iputils-ping']),
                {'id': 'wireguard', 'wireguard_interface': {'name': 'wg0', 'template': 'templates/wg0.conf.tmpl'}}]}])
        vm.write_playbook(workspace, 'replace_key', [
            {'id': 'replace_key', 'nodes': 'a', 'node_tasks': [
                {'file': {'path': KEY, 'state': 'absent'}},
                {'id': 'wireguard', 'wireguard_interface': {'name': 'wg0', 'template': 'templates/wg0.conf.tmpl'}}]}])
        vm.write_playbook(workspace, 'remove', [
            {'id': 'remove', 'nodes': 'wireguard_servers', 'node_tasks': [
                {'id': 'wireguard', 'wireguard_interface': {'name': 'wg0', 'state': 'absent'}}]}])
        # wg1 on a alone, with a ListenPort below 1024.
        vm.write_playbook(workspace, 'low_port', [
            {'id': 'low_port', 'nodes': 'a', 'node_tasks': [
                {'id': 'wireguard', 'wireguard_interface': {'name': 'wg1', 'content': '[Interface]\nListenPort = 443\n'}}]}])
        vm.write_playbook(workspace, 'remove_low_port', [
            {'id': 'remove_low_port', 'nodes': 'a', 'node_tasks': [
                {'id': 'wireguard', 'wireguard_interface': {'name': 'wg1', 'state': 'absent'}}]}])

    def mmx_command(*args, expected=(0,)):
        p = vm.cmd([mmx, *args], cwd=workspace, check=False, timeout=600)
        printed.append(p.stdout + p.stderr)
        assert p.returncode in expected, (args, p.returncode, p.stdout, p.stderr)
        return p

    def run(command, playbook='private_network', expected=(0,)):
        return json.loads(mmx_command(command, playbook, '--json', expected=expected).stdout)

    def wireguard_result(record, node):
        entry = next(e for e in record['entries'] if e['node'] == node)
        return next(r for r in entry['response']['results'] if r['task'] == 'wireguard')

    def mark(message):
        passed.append(message)
        print('PASS ' + message, flush=True)

    def metadata(node, path):
        return remote(node, f'stat -c "%a %U %G" {path}').strip()

    def peers(node):
        return set(remote(node, 'wg show wg0 peers').split())

    def ping_works(source, destination):
        # The first packets may wait for a handshake.
        return remote(source, f'ping -c 3 -i 0.2 -W 2 {ADDRESSES[destination]} >/dev/null && echo ok', check=False).stdout.strip() == 'ok'

    def start_ping(source, destination):
        remote(source, 'rm -f /run/mmx-wireguard-ping.log; systemd-run --quiet --collect --unit=mmx-wireguard-ping '
                       f"/bin/sh -c 'exec ping -n -i 0.05 {ADDRESSES[destination]} > /run/mmx-wireguard-ping.log 2>&1'")
        # Replies arrive before the change starts.
        deadline = time.monotonic() + 10
        while 'bytes from' not in remote(source, 'cat /run/mmx-wireguard-ping.log 2>/dev/null || true'):
            assert time.monotonic() < deadline, remote(source, 'cat /run/mmx-wireguard-ping.log 2>/dev/null || true')
            time.sleep(0.2)

    def stop_ping(source):
        """Stop the ping a second after the change, so that it also covers
        the time after the reload; return the packets sent and those that
        got no reply. ping's own summary counts the last request as lost
        when its reply is still on the way at SIGINT, which between servers
        in two locations happens about every second time; every earlier
        request needs its reply."""
        time.sleep(1)
        remote(source, 'systemctl kill --signal=SIGINT mmx-wireguard-ping.service; '
                       'for i in $(seq 1 50); do systemctl is-active --quiet mmx-wireguard-ping.service || break; sleep 0.1; done')
        log = remote(source, 'cat /run/mmx-wireguard-ping.log')
        match = re.search(r'(\d+) packets transmitted', log)
        assert match, log
        sent = int(match[1])
        replied = {int(n) for n in re.findall(r'icmp_seq=(\d+) ', log)}
        return sent, len(set(range(1, sent)) - replied)

    def controller_machine_cases():
        directory = workspace / 'controller-machine'
        directory.mkdir(mode=0o700)
        guest_project = '/var/tmp/mmx-wireguard-controller'
        key_comment = 'mmx-test-wireguard-controller'
        saved_file = '/run/mmx-wireguard-controller-previous.conf'
        architecture = vm.architecture('mmx-dev')
        controller = directory / 'mmx'
        # As in controller_machine_as_node.py, the guest needs a Linux
        # controller even when the harness runs on macOS.
        subprocess.run(['go', 'build', '-trimpath', vm.release_version_flag(), '-o', str(controller), './cmd/mmx'], cwd=vm.REPO,
                       env=dict(os.environ, CGO_ENABLED='0', GOOS='linux', GOARCH=architecture), check=True)
        account = nodes['a']['ssh_user']
        assert account != 'root' and nodes['b']['ssh_user'] == account, nodes

        def as_account(script, node='a', check=True, timeout=120):
            # These inspection connections use the guests' management
            # addresses, so a broken wg0 never prevents inspection or cleanup.
            return vm.cmd([*connections[node], script], check=check, timeout=timeout)

        def logins(node):
            return as_account("sha256sum < ~/.ssh/authorized_keys; stat -c '%U %a' ~/.ssh/authorized_keys", node).stdout

        def file_hash(node):
            return remote(node, f'sha256sum {CONF}').split()[0]

        def running_peers(node):
            # Never return the dump's first line: it contains the private
            # key. Ignore handshakes, counters, and roaming endpoints.
            return remote(node, "wg show wg0 dump | awk 'NR > 1 {print $1, $4, $8}' | sort").splitlines()

        fresh_ssh = ['ssh', '-T', '-F', '/dev/null', '-o', 'BatchMode=yes', '-o', 'IdentitiesOnly=yes',
                     '-o', 'StrictHostKeyChecking=yes', '-o', 'ConnectTimeout=10',
                     '-o', 'ControlMaster=no', '-o', 'ControlPath=none', '-o', 'ControlPersist=no',
                     '-i', guest_project + '/administrator_key', '-o', 'UserKnownHostsFile=' + guest_project + '/known_hosts']

        def ssh_from_controller(node):
            answer = as_account(shlex.join([*fresh_ssh, '-p', str(nodes[node]['ssh_port']),
                                           account + '@' + ADDRESSES[node], 'cat /etc/machine-id']))
            assert answer.stdout.strip() == remote(node, 'cat /etc/machine-id').strip(), answer.stdout

        def controller_command(*arguments, expected=0):
            p = as_account('cd ' + guest_project + ' && ./mmx ' + shlex.join(arguments), check=False, timeout=180)
            printed.append(p.stdout + p.stderr)
            assert p.returncode == expected, (arguments, p.returncode, p.stdout, p.stderr)
            return p

        def controller_run(*arguments, expected=0):
            started = time.monotonic()
            p = controller_command('run', 'private_network', *arguments, expected=expected)
            elapsed = time.monotonic() - started
            record = json.loads(controller_command('results', '--json').stdout)
            assert record['status'] == ('failed' if expected else 'succeeded'), record
            assert all(e['connection_used'] == 'ssh' for e in record['entries']), record
            return record, p.stdout + p.stderr, elapsed

        def write_guest_inventory(contents):
            # The node HTTPS API is not set up on these WireGuard addresses.
            # No management-address or HTTPS fallback can confirm a change.
            vm.write_inventory(directory, {'nodes': {
                node: {'address': ADDRESSES[node], 'connection': 'ssh_only', 'ssh_user': account,
                       'ssh_port': nodes[node]['ssh_port'], 'ssh_config': '/dev/null',
                       'ssh_identity_file': guest_project + '/administrator_key',
                       'ssh_known_hosts': guest_project + '/known_hosts',
                       'vars': {'wireguard_config': contents[node]}} for node in NODES}})
            vm.copy(directory / 'inventories/example.yml', guest_project + '/inventories/example.yml', 'mmx-dev')

        def restored(record, node, before_hash, before_peers):
            entry = next(e for e in record['entries'] if e['node'] == node)
            assert entry['response']['status'] == 'failed', entry
            result = wireguard_result(record, node)
            assert result['status'] == 'failed' and result['change'] == 'changed', result
            assert 'No reachability confirmation arrived over a new SSH connection within 30s.' in result['reason'], result
            assert f'The previous file {CONF} and its running configuration are restored.' in result['reason'], result
            assert result['owed_restore']['restore_status'] == 'succeeded', result
            assert file_hash(node) == before_hash, (node, file_hash(node), before_hash)
            assert running_peers(node) == before_peers, (node, running_peers(node), before_peers)
            ssh_from_controller('b')

        logins_before = {node: logins(node) for node in NODES}
        hashes_before = {node: file_hash(node) for node in NODES}
        peers_before = {node: running_peers(node) for node in NODES}
        with ExitStack() as cleanup_controller:
            remove_key = f"sed -i '/ {key_comment}$/d' ~/.ssh/authorized_keys"
            cleanup_controller.callback(as_account, f'rm -rf {guest_project}; {remove_key}')
            cleanup_controller.callback(as_account, remove_key, 'b')
            as_account(f'mkdir -m 700 {guest_project} && cd {guest_project} && mkdir inventories playbooks && '
                       f"ssh-keygen -q -t ed25519 -N '' -C {key_comment} -f administrator_key")
            vm.copy(controller, guest_project + '/mmx', 'mmx-dev')
            runner = vm.ROOT / ('bin/mmx-runner-linux-' + architecture)
            vm.copy(runner, guest_project + '/' + runner.name, 'mmx-dev')
            public_key = as_account(f'cat {guest_project}/administrator_key.pub').stdout.strip()
            assert public_key.endswith(' ' + key_comment), public_key
            for node in NODES:
                as_account(f"printf '%s\\n' {shlex.quote(public_key)} >> ~/.ssh/authorized_keys", node)
                remote(node, f'cp -p {CONF} {saved_file}')
                # A restart, because a reload of older wireguard-tools keeps
                # a keepalive that the file no longer sets.
                cleanup_controller.callback(remote, node, f'cp -p {saved_file} {CONF}; systemctl restart {UNIT}; rm -f {saved_file}')
            host_keys = {node: ' '.join(remote(node, 'cat /etc/ssh/ssh_host_ed25519_key.pub').split()[:2]) for node in NODES}
            known_hosts = directory / 'known_hosts'
            known_hosts.write_text(''.join(f'{ADDRESSES[node]},[{ADDRESSES[node]}]:{nodes[node]["ssh_port"]} '
                                          f'{host_keys[node]}\n' for node in NODES))
            vm.copy(known_hosts, guest_project + '/known_hosts', 'mmx-dev')

            contents = {node: f'[Interface]\nAddress = {ADDRESSES[node]}/24\nListenPort = 51820\n\n'
                              f'[Peer]\nPublicKey = {public_keys[other]}\nAllowedIPs = {ADDRESSES[other]}/32\n'
                              f'Endpoint = {nodes[other]["address"]}:51820\nPersistentKeepalive = 5\n'
                        for node, other in (('a', 'b'), ('b', 'a'))}
            vm.write_project(directory, project_name='wireguard_controller', playbook_name='private_network',
                             max_nodes_at_once=1, fleet_stages=[
                {'id': 'private_network', 'nodes': 'all', 'node_tasks': [
                    {'id': 'wireguard', 'wireguard_interface': {'name': 'wg0', 'content': '{{ .vars.wireguard_config }}',
                                                             'restore_unless_controller_confirms_within': '30s'}}]}])
            for path in (directory / 'mmx_project_settings.yml', directory / 'playbooks/private_network.yml'):
                vm.copy(path, guest_project + '/' + str(path.relative_to(directory)), 'mmx-dev')
            write_guest_inventory(contents)
            for node in NODES:
                ssh_from_controller(node)
            first, _, _ = controller_run()
            assert all(wireguard_result(first, node)['change'] == 'changed' for node in NODES), first
            again, _, _ = controller_run()
            assert all(wireguard_result(again, node)['change'] == 'unchanged' for node in NODES), again
            baseline_hashes = {node: file_hash(node) for node in NODES}
            baseline_peers = {node: running_peers(node) for node in NODES}
            for node, other in (('a', 'b'), ('b', 'a')):
                assert baseline_peers[node] == [f'{public_keys[other]} {ADDRESSES[other]}/32 5'], baseline_peers
            mark('the login account on guest a runs mmx over WireGuard SSH to a and b; '
                 'the first run changes both nodes and the second run is unchanged')

            # Changing only b's allowed IP for a prevents b from accepting
            # a's WireGuard packets, while b's wg0 still matches the bad file.
            bad = dict(contents)
            bad['b'] = contents['b'].replace(f'AllowedIPs = {ADDRESSES["a"]}/32', f'AllowedIPs = {ADDRESSES["c"]}/32')
            write_guest_inventory(bad)
            failed, output, elapsed = controller_run(expected=1)
            assert 'private_network / b: failed' in output and f'{CONF} was restored to its previous state' in output, output
            restored(failed, 'b', baseline_hashes['b'], baseline_peers['b'])
            # 60 s beyond the 30 s wait budgets one 10 s collection interval,
            # one 10 s request, and 40 s for setup and native activation and
            # restore. This must finish well before the 340 s loss-of-contact
            # collection allowance; elapsed covers the run, not results.
            assert elapsed < 30 + 60, (elapsed, output)
            mark(f'a bad change on b fails for missing SSH confirmation and restores the file and running peers; '
                 f'a new WireGuard SSH connection works again ({elapsed:.1f} s, under 90 s)')

            bad = dict(contents)
            # a keeps its own wg0 address, so SSH to a's local address
            # still works. Only the required new SSH answer from b is lost.
            bad['a'] = contents['a'].split('[Peer]', 1)[0]
            write_guest_inventory(bad)
            missing_b = ('The changed node is the controller machine. After the change, the controller machine '
                         'could not reach these nodes over new SSH connections: b. MMX did not confirm the change.')
            for arguments in ((), ('--limit', 'a')):
                failed, output, _ = controller_run(*arguments, expected=1)
                assert 'private_network / a: failed' in output and missing_b in output, output
                assert missing_b in wireguard_result(failed, 'a')['reason'], failed
                restored(failed, 'a', baseline_hashes['a'], baseline_peers['a'])
                assert file_hash('b') == baseline_hashes['b'] and running_peers('b') == baseline_peers['b']
                ssh_from_controller('a')
                if arguments:
                    assert {e['node'] for e in failed['entries']} == {'a'}, failed
                mark('removing b from the controller machine a restores a and names b as unreachable over new SSH connections' +
                     (' even with --limit a' if arguments else ''))

            harmless = {node: content.replace('PersistentKeepalive = 5', 'PersistentKeepalive = 15') for node, content in contents.items()}
            write_guest_inventory(harmless)
            confirmed, _, _ = controller_run()
            for node, other in (('a', 'b'), ('b', 'a')):
                result = wireguard_result(confirmed, node)
                assert result['status'] == 'succeeded' and result['change'] == 'changed', result
                outcome = result['owed_restore']
                assert outcome['restore_status'] == 'not_needed' and not outcome['previous_file_restored'], outcome
                receipt = json.loads(remote(node, f'cat /var/lib/mmx/owed_restores/{outcome["owed_restore_id"]}/owed_restore.json'))
                assert receipt['change_phase'] == 'change_accepted' and receipt['owed_restore_cleanup_complete'], receipt
                assert receipt['after_change_confirmed_connection_kinds'] == ['ssh'], receipt
                assert file_hash(node) != baseline_hashes[node]
                assert running_peers(node) == [f'{public_keys[other]} {ADDRESSES[other]}/32 15']
                assert remote(node, f'grep -Fx "PersistentKeepalive = 15" {CONF}').strip() == 'PersistentKeepalive = 15'
                ssh_from_controller(node)
            mark('a harmless keepalive change confirms with changed on a and b, keeps the new files and peers, and restores nothing')

            without_keepalive = {node: content.replace('PersistentKeepalive = 15\n', '') for node, content in harmless.items()}
            write_guest_inventory(without_keepalive)
            removed, _, _ = controller_run()
            for node, other in (('a', 'b'), ('b', 'a')):
                result = wireguard_result(removed, node)
                assert result['status'] == 'succeeded' and result['change'] == 'changed', result
                assert (f'restarted {UNIT} because peer {public_keys[other]} has a keepalive of 15 seconds '
                        'that the file does not set') in result['reason'], result
                assert running_peers(node) == [f'{public_keys[other]} {ADDRESSES[other]}/32 off'], running_peers(node)
            mark('removing the keepalive lines restarts both interfaces, because a reload of older wireguard-tools '
                 'keeps the old keepalive, and the keepalive is then off')

        assert {node: logins(node) for node in NODES} == logins_before
        assert {node: file_hash(node) for node in NODES} == hashes_before
        assert {node: running_peers(node) for node in NODES} == peers_before
        as_account(f'test ! -e {guest_project}')
        for node in NODES:
            remote(node, f'test ! -e {saved_file}')
        assert ping_works('a', 'b') and ping_works('b', 'a')
        mark('the controller project and SSH key are removed, authorized_keys are unchanged, '
             'and the original mesh files and running peers are back for the remaining cases')

    def cleanup():
        for node in NODES:
            remote(node, f'systemctl disable --now {UNIT} 2>/dev/null || true; systemctl stop mmx-wireguard-ping.service 2>/dev/null || true; '
                         f'ip link delete wg0 2>/dev/null || true; rm -f {CONF} {KEY} /root/mmx-wireguard-keys /run/mmx-wireguard-ping.log; '
                         'systemctl disable --now wg-quick@wg1.service 2>/dev/null || true; ip link delete wg1 2>/dev/null || true; '
                         'rm -f /etc/wireguard/wg1.conf /etc/wireguard/wg1.private_key')

    cleanup()
    # Ubuntu 26.04 confines wg-quick with AppArmor. A real Ubuntu 26.04
    # server has the apparmor package, whose profiles include wg-quick's; the
    # Incus image leaves it out. Every case runs under the profile, as on a
    # real server.
    release = remote('a', '. /etc/os-release && echo "$ID $VERSION_ID"').strip()
    if release == 'ubuntu 26.04':
        for node in NODES:
            remote(node, 'DEBIAN_FRONTEND=noninteractive apt-get install -yq apparmor >/dev/null')
    try:
        inventory()
        project()
        assert mmx_command('validate').returncode == 0
        preview = run('dryrun', expected=(0, 2, 3))
        for node in NODES:
            result = wireguard_result(preview, node)
            assert 'public_key' not in result and result['status'] in ('not_checked', 'succeeded'), result
            assert remote(node, f'test -e {CONF} && echo exists || echo absent').strip() == 'absent'
        mark('a dryrun on new nodes writes nothing and has no public key')

        # A first-boot script can supply the key before MMX's first run.
        remote('b', 'command -v wg >/dev/null || { apt-get update -qq; '
                    'DEBIAN_FRONTEND=noninteractive apt-get install -y -qq wireguard-tools; }\n'
                    'install -d -m 0700 /etc/wireguard\n'
                    f'umask 077; wg genkey > {KEY}; chown root:root {KEY}; chmod 0600 {KEY}')
        supplied_public_key = remote('b', f'wg pubkey < {KEY}').strip()
        supplied_key_hash = remote('b', f'sha256sum {KEY}').strip()
        first_boot_configuration = (f'[Interface]\nAddress = {ADDRESSES["b"]}/24\nListenPort = 51820\n'
                                    'PostUp = wg set %i private-key /etc/wireguard/%i.private_key\n')
        remote('b', f'printf %s {shlex.quote(first_boot_configuration)} > {CONF}\n'
                    f'chown root:root {CONF}; chmod 0600 {CONF}\n'
                    f'systemctl enable --now {UNIT}')
        supplied_running_public_key = remote('b', 'wg show wg0 public-key').strip()
        assert supplied_running_public_key == supplied_public_key

        first = run('run')
        for node in NODES:
            result = wireguard_result(first, node)
            expected_action = 'started' if node == 'a' else 'restarted'
            assert result['change'] == 'changed' and f', {expected_action} {UNIT}' in result['reason'], result
            assert (f'created the private key {KEY}' in result['reason']) == (node == 'a'), result
            public_keys[node] = result['public_key']
            assert metadata(node, '/etc/wireguard') == '700 root root', metadata(node, '/etc/wireguard')
            assert metadata(node, KEY) == '600 root root' and metadata(node, CONF) == '600 root root'
            assert remote(node, f'wg pubkey < {KEY}').strip() == public_keys[node]
            assert remote(node, 'wg show wg0 public-key').strip() == public_keys[node]
            assert remote(node, f'sed -n 2p {CONF}').strip() == 'PrivateKey = ' + remote(node, f'cat {KEY}').strip()
            assert remote(node, f'systemctl is-enabled {UNIT}; systemctl is-active {UNIT}').split() == ['enabled', 'active']
            assert '+PrivateKey = (the node\'s private key, not shown)' in result['file_change']['diff'], result
            assert ADDRESSES[node] + '/24' in remote(node, 'ip -4 -o address show dev wg0')
        assert public_keys['b'] == supplied_public_key, public_keys
        assert remote('b', f'sha256sum {KEY}').strip() == supplied_key_hash
        assert remote('b', 'wg show wg0 public-key').strip() == supplied_running_public_key
        mark('a first-boot configuration with a PostUp key command is replaced by MMX, '
             'while the supplied root-owned 0600 key stays unchanged and the running public key stays the same')

        # The page reads the public keys from mmx results --json the same way.
        results = json.loads(mmx_command('results', '--json').stdout)
        listed = {e['node']: r['public_key'] for e in results['entries'] for r in e['response']['results'] if r['task'] == 'wireguard'}
        assert listed == public_keys, listed
        mark('the first run creates or reuses each node\'s private key (0600 in /etc/wireguard 0700), writes it into the file, '
             'activates the configuration, and reports the public key, which wg derives from the key file too')

        inventory()
        joined = run('run')
        for node, other in (('a', 'b'), ('b', 'a')):
            result = wireguard_result(joined, node)
            assert result['change'] == 'changed' and result['reason'] == f'wrote {CONF}, reloaded {UNIT}: added peer {public_keys[other]}', result
            assert peers(node) == {public_keys[other]}, peers(node)
        assert ping_works('a', 'b') and ping_works('b', 'a')
        again = run('run')
        assert all(e['response']['change'] == 'unchanged' for e in again['entries']), again
        preview = run('dryrun', expected=(0,))
        assert all(wireguard_result(preview, node)['public_key'] == public_keys[node] for node in NODES), preview
        mark('the public keys in the inventory give each node the other as its peer through a reload; a second run '
             'and a dryrun change nothing')

        # c joins the mesh: its key is in the inventory, its machine is not up yet.
        public_keys['c'] = remote('a', 'wg genkey | wg pubkey').strip()
        mesh.append('c')
        inventory()
        start_ping('a', 'b')
        joined = run('run')
        sent, lost = stop_ping('a')
        for node in NODES:
            result = wireguard_result(joined, node)
            assert result['reason'] == f'wrote {CONF}, reloaded {UNIT}: added peer {public_keys["c"]}', result
            assert public_keys['c'] in peers(node)
        assert sent >= 20 and lost == 0, (sent, lost)
        mesh.remove('c')
        inventory()
        start_ping('b', 'a')
        left = run('run')
        sent, lost = stop_ping('b')
        for node in NODES:
            result = wireguard_result(left, node)
            assert result['reason'] == f'wrote {CONF}, reloaded {UNIT}: removed peer {public_keys["c"]}', result
            assert public_keys['c'] not in peers(node)
        assert sent >= 20 and lost == 0, (sent, lost)
        del public_keys['c']
        mark('adding and removing a third peer reloads both nodes while a ping between them runs, '
             'with no lost packet')

        controller_machine_cases()

        ADDRESSES['a'] = '10.231.88.11'
        inventory()
        moved = run('run')
        result = wireguard_result(moved, 'a')
        assert result['reason'].startswith(f'wrote {CONF}, restarted {UNIT} because Address changed in the file'), result
        assert wireguard_result(moved, 'b')['reason'] == f'wrote {CONF}, reloaded {UNIT}: changed the allowed IPs of peer {public_keys["a"]}', moved
        assert '10.231.88.11/24' in remote('a', 'ip -4 -o address show dev wg0') and '10.231.88.1/24' not in remote('a', 'ip -4 -o address show dev wg0')
        # The restart ended a's sessions; b still sends with its old one,
        # which a no longer knows, until a WireGuard timer makes b start a
        # new handshake.
        started = time.monotonic()
        answered = remote('b', f'ping -n -c 1 -i 0.2 -w 30 {ADDRESSES["a"]} >/dev/null && echo ok', check=False).stdout.strip() == 'ok'
        print(f'b reached a again {time.monotonic() - started:.1f} s after the restart, without a sending first', flush=True)
        assert answered and ping_works('a', 'b')
        mark('an Address change restarts the interface of its node, and the other node reloads for the peer\'s new allowed IP')

        remote('a', f'wg set wg0 peer {public_keys["b"]} remove')
        assert peers('a') == set()
        repaired = run('run')
        assert wireguard_result(repaired, 'a')['reason'] == f'reloaded {UNIT}: added peer {public_keys["b"]}', repaired
        assert wireguard_result(repaired, 'b')['change'] == 'unchanged'
        assert peers('a') == {public_keys['b']} and ping_works('a', 'b')
        mark('a peer removed by hand with wg set is put back by the next run')

        # The keys that the search at the end looks for, kept on the nodes
        # themselves: a key on a command line would reach the journal through
        # sudo's own log.
        for node in NODES:
            remote(node, f'cat {KEY} > /root/mmx-wireguard-keys; chmod 600 /root/mmx-wireguard-keys')
        old_public_key = public_keys['a']
        replaced = run('run', playbook='replace_key')
        result = wireguard_result(replaced, 'a')
        public_keys['a'] = result['public_key']
        assert public_keys['a'] != old_public_key and result['reason'].startswith(f'created the private key {KEY}, wrote {CONF}, reloaded {UNIT}'), result
        assert '-PrivateKey = (another private key, not shown)\n+PrivateKey = (the node\'s private key, not shown)' in result['file_change']['diff'], result
        assert remote('a', 'wg show wg0 public-key').strip() == public_keys['a']
        remote('a', f'cat {KEY} >> /root/mmx-wireguard-keys')
        inventory()
        updated = run('run')
        assert wireguard_result(updated, 'a')['change'] == 'unchanged', updated
        assert wireguard_result(updated, 'b')['reason'] == f'wrote {CONF}, reloaded {UNIT}: added peer {public_keys["a"]}; removed peer {old_public_key}', updated
        assert peers('b') == {public_keys['a']} and ping_works('a', 'b') and ping_works('b', 'a')
        mark('removing the key file makes a new key on the next run; with the new public key in the inventory the other node reloads, '
             'and the two talk again')

        # The private keys never left their nodes: not in mmx's output, the
        # saved runs on the controller machine, the nodes' job records, or
        # their journals.
        saved = ''.join(p.read_text(errors='replace') for p in (workspace / '.mmx').rglob('*') if p.is_file())
        for node in NODES:
            for key in remote(node, 'cat /root/mmx-wireguard-keys').split():
                assert key not in ''.join(printed) and key not in saved, 'a private key left its node'
            found = remote(node, 'grep -rlF -f /root/mmx-wireguard-keys /var/lib/mmx /run/mmx 2>/dev/null || true\n'
                                 'journalctl --no-pager -q 2>/dev/null | grep -F -f /root/mmx-wireguard-keys || true')
            assert found.strip() == '', found
        mark('the private keys appear in no mmx output, saved run, node job record, or journal')

        removed = run('run', playbook='remove')
        for node in NODES:
            result = wireguard_result(removed, node)
            assert result['reason'] == f'stopped and disabled {UNIT}, removed {CONF}, removed {KEY}', result
            assert remote(node, f'test -e {CONF} || test -e {KEY} && echo exists || echo absent').strip() == 'absent'
            # The saved copy that wg-quick down read is gone too.
            assert remote(node, 'ls -A /etc/wireguard').strip() == '', remote(node, 'ls -A /etc/wireguard')
            assert remote(node, 'ip link show wg0 >/dev/null 2>&1 && echo up || echo gone').strip() == 'gone'
            assert remote(node, f'systemctl is-enabled {UNIT} || true').strip() == 'disabled'
        assert all(e['response']['change'] == 'unchanged' for e in run('run', playbook='remove')['entries'])
        mark('state: absent stops and disables the service, which removes the interface, then removes the file and the key; '
             'a second removal changes nothing')

        # On Ubuntu 26.04 the child profile wg-quick//ip, which brings an
        # interface up and so opens its port, lacks capability
        # net_bind_service. The other distributions load no profile for
        # wg-quick.
        confined = 'wg-quick//ip (enforce)' in remote('a', 'cat /sys/kernel/security/apparmor/profiles 2>/dev/null || true').splitlines()
        assert confined == (release == 'ubuntu 26.04'), (release, confined)
        low_files = '/etc/wireguard/wg1.conf /etc/wireguard/wg1.private_key'
        gone = 'ip link show wg1 >/dev/null 2>&1 && echo up || echo gone'
        if confined:
            # The premise, without MMX: wg-quick cannot open the port here.
            remote('a', "mkdir -p -m 700 /etc/wireguard; printf '[Interface]\\nListenPort = 443\\n' > /etc/wireguard/wg1.conf")
            by_hand = remote('a', 'wg-quick up wg1', check=False)
            remote('a', 'rm -f /etc/wireguard/wg1.conf')
            assert by_hand.returncode != 0 and remote('a', gone).strip() == 'gone', (by_hand.stdout, by_hand.stderr)
            # ip, under wg-quick//ip, is refused where it brings the
            # interface up, which opens the port. The same port works below
            # once the profile is disabled.
            assert 'up dev wg1\nRTNETLINK answers: Permission denied\n' in by_hand.stderr, (by_hand.stdout, by_hand.stderr)
            for command in ('dryrun', 'run'):
                result = wireguard_result(run(command, playbook='low_port', expected=(1,)), 'a')
                assert result['status'] == 'failed' and result['change'] == 'unchanged', result
                assert result['reason'].startswith('ListenPort = 443 is below 1024, but on this node the AppArmor profile wg-quick//ip, '
                                                   'in /etc/apparmor.d/wg-quick, lacks capability net_bind_service.'), result
                assert remote('a', f'for f in {low_files}; do test ! -e $f; done; echo absent').strip() == 'absent'
            mark('on Ubuntu 26.04 wg-quick cannot open a ListenPort below 1024 under its AppArmor profile, and MMX refuses '
                 'such a file before any change, also in a dryrun')
            # Disabling the profile, as an administrator may, lifts the refusal.
            # A server's image may have no /etc/apparmor.d/disable yet.
            remote('a', 'mkdir -p /etc/apparmor.d/disable && ln -s /etc/apparmor.d/wg-quick /etc/apparmor.d/disable/wg-quick && apparmor_parser -R /etc/apparmor.d/wg-quick')
        try:
            result = wireguard_result(run('run', playbook='low_port'), 'a')
            assert result['status'] == 'succeeded' and result['change'] == 'changed', result
            assert remote('a', 'wg show wg1 listen-port').strip() == '443'
            assert wireguard_result(run('run', playbook='remove_low_port'), 'a')['change'] == 'changed'
            assert remote('a', gone).strip() == 'gone'
        finally:
            if confined:
                remote('a', 'rm -f /etc/apparmor.d/disable/wg-quick && apparmor_parser -r /etc/apparmor.d/wg-quick')
        if confined:
            assert 'wg-quick//ip (enforce)' in remote('a', 'cat /sys/kernel/security/apparmor/profiles').splitlines()
        mark('a ListenPort below 1024 works where AppArmor lets wg-quick open it' +
             (', here with the wg-quick profile disabled, and enforced again afterwards' if confined else ''))
    finally:
        cleanup()
    print(f'{len(passed)} WireGuard interface workflows passed', flush=True)


if __name__ == '__main__':
    vm.run_suite(main)
