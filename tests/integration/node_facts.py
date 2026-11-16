#!/usr/bin/env python3
"""Common Linux facts, saved observations, and capacity-based service configuration."""
import json
import shlex
import shutil
import subprocess

import vm


# The family and package manager that each tested distribution's os-release ID
# stands for; the facts under test must agree with this table.
FAMILY_AND_PACKAGE_MANAGER = {
    'debian': ('debian', 'apt'), 'ubuntu': ('debian', 'apt'),
    'rocky': ('redhat', 'dnf'), 'almalinux': ('redhat', 'dnf'), 'fedora': ('redhat', 'dnf'),
    'arch': ('arch', 'pacman'), 'opensuse-leap': ('suse', 'zypper'),
}
# Independent observations use tools already shipped in every test image.
ORACLE = r'''
import json, os, re, subprocess
from pathlib import Path

def command(*args):
    return subprocess.check_output(args, text=True).strip()

def read(path):
    return Path(path).read_text().strip()

memory = {key: int(value) * 1024 for key, value in
          re.findall(r'^(MemTotal|SwapTotal):\s+(\d+) kB$', read('/proc/meminfo'), re.M)}
interfaces = {}
for link in json.loads(command('ip', '-j', 'address')):
    mac = link.get('address', '')
    if link['link_type'] == 'loopback':
        mac = ''
    interfaces[link['ifname']] = {'index': link['ifindex'], 'mtu': link['mtu'], 'mac': mac,
        'addresses': [{'family': {'inet': 'ipv4', 'inet6': 'ipv6'}[a['family']],
                       'address': a['local'], 'prefix_length': a['prefixlen']}
                      for a in link['addr_info']]}
routes = []
for flag, family in [('-4', 'ipv4'), ('-6', 'ipv6')]:
    for route in json.loads(command('ip', '-j', flag, 'route', 'show', 'table', 'main', 'default')):
        item = {'family': family, 'interface': route['dev'], 'metric': route.get('metric', 0)}
        if 'gateway' in route:
            item['gateway'] = route['gateway']
        if 'prefsrc' in route:
            item['source_address'] = route['prefsrc']
        routes.append(item)
routes.sort(key=lambda r: (r['family'], r['metric'], r['interface'], r.get('gateway', '')))
# The address of the IPv4 default route with the lowest metric: its source
# address, or else the first IPv4 address of its interface.
first = next(r for r in routes if r['family'] == 'ipv4')
default_ipv4_address = first.get('source_address') or next(
    a['address'] for a in interfaces[first['interface']]['addresses'] if a['family'] == 'ipv4')
mounts = []
for line in read('/proc/self/mountinfo').splitlines():
    left, right = line.split(' - ')
    a, b = left.split(), right.split()
    unescape = lambda s: re.sub(r'\\([0-7]{3})', lambda m: chr(int(m[1], 8)), s)
    path = unescape(a[4])
    item = dict(id=int(a[0]), path=path, source=unescape(b[1]), filesystem=b[0],
                read_only='ro' in (a[5] + ',' + b[2]).split(','))
    if b[0] == 'ext4':
        info = os.statvfs(path)
        item.update(total_bytes=info.f_blocks * info.f_frsize, available_bytes=info.f_bavail * info.f_frsize)
    mounts.append(item)
mounts.sort(key=lambda m: (m['path'], m['id']))
print(json.dumps(dict(hostname=read('/proc/sys/kernel/hostname'), kernel=command('uname', '-r'),
    boot_id=read('/proc/sys/kernel/random/boot_id'), cpu=int(command('getconf', '_NPROCESSORS_ONLN')),
    memory=memory, interfaces=interfaces, routes=routes, default_ipv4_address=default_ipv4_address, mounts=mounts)))
'''


def main():
    vm.up()
    vm.reset()
    vm.build()
    node, ssh = vm.connection()
    work = vm.ROOT / 'node-facts'
    if work.exists():
        shutil.rmtree(work)
    shutil.copytree(vm.REPO / 'examples/node-facts', work)
    project = work / 'playbooks/sized_service.yml'
    original = project.read_text()
    settings = work / 'mmx_project_settings.yml'
    original_settings = settings.read_text()
    restore_size = None
    passed = []
    base = '/srv/mmx-facts-demo'

    def inventory():
        vm.write_inventory(work, {'nodes': {'app': node}}, name='example')

    def remote(script):
        return vm.cmd([*ssh, 'sudo -n /bin/sh -c ' + shlex.quote('set -eu\n' + script)]).stdout

    def run(command, expected=None, *args, human=False):
        p = vm.cmd([vm.ROOT / 'bin/mmx', command, *vm.playbook_args(command, "sized_service"), *args, *([] if human else ['--json'])], cwd=work, check=False)
        assert p.returncode in (vm.success_codes(command) if expected is None else (expected,)), (command, p.stdout, p.stderr)
        return p.stdout if human else json.loads(p.stdout)

    def observation(record, index=0):
        return record['entries'][index]['response']['facts']

    def tasks(items, extra_fleet_stages=None):
        vm.write_project(work, playbook_name='sized_service', **{'project_name': 'node_facts', 'default_inventory': 'example',
            'node_task_defaults': {'retry': {'max_tries': 1}},
            'fleet_stages': [{'id': 'inspect', 'nodes': 'all', 'node_tasks': items}, *(extra_fleet_stages or [])]})

    def mark(message):
        passed.append(message)
        print('PASS ' + message, flush=True)

    def cleanup():
        remote('ip link del mmx-facts0 2>/dev/null || true\n'
               'systemctl disable --now mmx-facts-demo.service 2>/dev/null || true\n'
               'rm -f /etc/systemd/system/mmx-facts-demo.service\nsystemctl daemon-reload\n'
               'rm -rf /srv/mmx-facts-demo /usr/local/bin/mmx-facts-demo-load /var/tmp/mmx-facts-fail /var/tmp/mmx-facts-running')

    inventory()
    cleanup()
    try:
        run('validate')
        preview = run('dryrun')
        cpus = int(remote('getconf _NPROCESSORS_ONLN'))
        assert observation(preview)['values']['cpu']['count'] == cpus
        remote('test ! -e ' + base + '/config')
        applied = run('run')
        snap = observation(applied)
        node_facts = snap['values']
        release = remote('. /etc/os-release; printf "%s|%s|%s" "$ID" "${VERSION_ID:-}" "${VERSION_CODENAME:-}"').split('|')
        distribution = node_facts['distribution']
        # Exactly the facts that os-release leaves empty are unavailable: a rolling
        # release has no VERSION_ID, and some distributions have no codename.
        expected_unavailable = {'/node/distribution/' + name for name, empty in (
            ('version', not release[1]), ('major_version', not release[1]), ('codename', not release[2])) if empty}
        assert set(snap.get('unavailable', {})) == expected_unavailable, (snap.get('unavailable'), expected_unavailable)
        if release[1]:
            assert distribution['major_version'] == int(release[1].split('.')[0]), distribution
        assert [distribution['id'], distribution.get('version', ''), distribution.get('codename', '')] == release, (distribution, release)
        family, package_manager = FAMILY_AND_PACKAGE_MANAGER[release[0]]
        assert distribution['family'] == family and node_facts['package_manager'] == package_manager, node_facts
        assert node_facts['selinux']['mode'] == remote('getenforce 2>/dev/null || echo Disabled').strip().lower(), node_facts['selinux']
        assert node_facts['virtualization']['type'] == remote('systemd-detect-virt || true').strip(), node_facts['virtualization']
        assert node_facts['fully_qualified_name'].split('.')[0] == node_facts['hostname'].split('.')[0], node_facts
        actual = json.loads(remote("python3 - <<'PY'\n" + ORACLE + '\nPY'))
        assert node_facts['name'] == 'app' and node_facts['arch'] == vm.architecture()
        assert node_facts['os']['id'] == release[0] and node_facts['os'].get('version_id', '') == release[1], node_facts['os']
        assert node_facts['hostname'] == actual['hostname'] and node_facts['kernel']['release'] == actual['kernel']
        assert node_facts['boot_id'] == actual['boot_id'] and node_facts['cpu']['count'] == actual['cpu']
        assert node_facts['memory'] == {'total_bytes': actual['memory']['MemTotal'],
            'total_mib': actual['memory']['MemTotal'] // (1 << 20), 'swap_total_bytes': actual['memory']['SwapTotal']}
        assert node_facts['network']['interfaces'] == actual['interfaces']
        assert node_facts['network']['default_routes'] == actual['routes']
        assert node_facts['network']['default_ipv4_address'] == actual['default_ipv4_address'], node_facts['network']
        for observed, outside in zip(node_facts['storage']['mounts'], actual['mounts'], strict=True):
            for key in ('id', 'path', 'source', 'filesystem', 'read_only'):
                assert observed[key] == outside[key], (observed, outside)
            if outside['filesystem'] == 'ext4':
                assert observed['total_bytes'] == outside['total_bytes']
                # MMX saves its result between these independent observations.
                assert abs(observed['available_bytes'] - outside['available_bytes']) < (32 << 20)
        assert remote('cat ' + base + '/config') == f"workers = {actual['cpu']}\ncache_mib = {node_facts['memory']['total_mib'] * 25 // 100}\n"
        assert remote('cat ' + base + '/actions') == 'start\n'
        assert run('run')['entries'][0]['response']['change'] == 'unchanged'
        mark('example facts match Linux tools, byte units, network/mount ordering, rendered capacity, and unchanged service')

        # The second show must use the first observation, despite this real change.
        add_link = 'ip link add mmx-facts0 type dummy\nip link set mmx-facts0 up\nip address add 192.0.2.10/24 dev mmx-facts0\nip route add default dev mmx-facts0 metric 60000'
        tasks([{'id': 'before', 'show': '{{ .node.network }}'},
               {'shell': {'script': add_link}},
               {'id': 'after', 'show': '{{ .node.network }}'}],
              [{'id': 'fresh', 'nodes': 'all', 'node_tasks': [{'show': '{{ .node.network }}'}]}])
        record = run('run')
        results = record['entries'][0]['response']['results']
        assert results[0]['value'] == results[2]['value']
        assert 'mmx-facts0' not in observation(record)['values']['network']['interfaces']
        fresh_network = observation(record, 1)['values']['network']
        assert fresh_network['interfaces']['mmx-facts0']['addresses'][0] == {'family': 'ipv4', 'address': '192.0.2.10', 'prefix_length': 24}
        assert {'family': 'ipv4', 'interface': 'mmx-facts0', 'metric': 60000} in fresh_network['default_routes']
        actual = json.loads(remote("python3 - <<'PY'\n" + ORACLE + '\nPY'))
        assert fresh_network['default_routes'] == actual['routes']
        assert '/node/network/default_routes' in record['entries'][1]['facts_changed']
        mark('facts stay fixed within a job and refresh in the next fleet stage, including multiple main-table default routes')

        remote('touch /var/tmp/mmx-facts-fail')
        tasks([{'show': '{{ .node.network.interfaces }}'},
               {'shell': {'script': 'test ! -e /var/tmp/mmx-facts-fail', 'changed_when': False}}])
        failed = run('run', 1)
        remote('ip link del mmx-facts0\nrm /var/tmp/mmx-facts-fail')
        project.write_text(original)  # Resume must still use the saved task definition.
        settings.write_text(original_settings)
        resumed = run('resume', 0, failed['id'])
        entry = resumed['entries'][0]
        assert entry['node_job_attempt'] == 2
        assert 'mmx-facts0' in entry['previous_node_job_attempts'][0]['response']['facts']['values']['network']['interfaces']
        assert 'mmx-facts0' not in observation(resumed)['values']['network']['interfaces']
        assert '/node/network/interfaces/mmx-facts0' in entry['facts_changed']
        saved_results = run('results', 0, failed['id'], human=True)
        assert 'facts collected at' in saved_results and 'facts changed since the previous reading' in saved_results
        assert observation(run('results', 0, failed['id'])) == observation(resumed)
        mark('saved resume freezes project inputs but collects fresh facts and retains the previous observation in results')

        remote(add_link)
        before = remote('stat -c "%i %y" ' + base + '/config\ncat ' + base + '/actions')
        noop = run('run')
        assert noop['entries'][0]['response']['change'] == 'unchanged'
        assert 'mmx-facts0' in observation(noop)['values']['network']['interfaces']
        assert before == remote('stat -c "%i %y" ' + base + '/config\ncat ' + base + '/actions')
        remote('ip link del mmx-facts0')
        mark('changed unused facts cause neither a file rewrite nor a service reload')

        # Persist the snapshot before the first task finishes; retain it on worker loss.
        tasks([{'shell': {'script': 'touch /var/tmp/mmx-facts-running; sleep 60', 'changed_when': False}}])
        with (work / 'worker-loss.stdout').open('w') as out, (work / 'worker-loss.stderr').open('w') as err:
            controller = subprocess.Popen([str(vm.ROOT / 'bin/mmx'), 'run', 'sized_service', '--json'],
                cwd=work, env=vm.ENV, stdout=out, stderr=err)
            try:
                remote('for n in $(seq 1 200); do test ! -f /var/tmp/mmx-facts-running || exit 0; sleep 0.1; done; exit 1')
            finally:
                controller.kill()
                controller.wait(timeout=10)
        live = run('status', 0)
        run_id = live['id']
        assert observation(live)['values']['boot_id'] == actual['boot_id']
        job_id = live['entries'][0]['job_id']
        remote("pkill -KILL -f '[m]mx-runner worker " + job_id + "'")
        interrupted = run('status', 0, run_id)
        assert interrupted['status'] == 'failed' and observation(interrupted) == observation(live)
        mark('live status and interrupted worker recovery preserve the original job facts')

        project.write_text(original)
        settings.write_text(original_settings)
        old_content = remote('cat ' + base + '/config')
        old_boot = actual['boot_id']
        restore_size = vm.enlarge()
        node, ssh = vm.connection()
        inventory()
        after_boot_actions = remote('cat ' + base + '/actions')
        preview = run('dryrun')
        fresh = observation(preview)['values']
        assert fresh['boot_id'] != old_boot
        assert fresh['cpu']['count'] == int(remote('getconf _NPROCESSORS_ONLN')) > cpus
        assert fresh['memory']['total_bytes'] > node_facts['memory']['total_bytes'] + (900 << 20)
        assert remote('cat ' + base + '/config') == old_content
        assert remote('cat ' + base + '/actions') == after_boot_actions
        changed = run('run')
        assert changed['entries'][0]['response']['change'] == 'changed'
        expected = f"workers = {fresh['cpu']['count']}\ncache_mib = {fresh['memory']['total_mib'] * 25 // 100}\n"
        assert remote('cat ' + base + '/config') == expected
        assert remote('cat ' + base + '/loaded') == expected
        assert remote('cat ' + base + '/actions') == after_boot_actions + 'reload\n'
        assert run('run')['entries'][0]['response']['change'] == 'unchanged'
        assert remote('cat ' + base + '/actions') == after_boot_actions + 'reload\n'
        mark('real VM or cloud server CPU/RAM resize and reboot refresh facts, preview without mutation, and reload changed configuration exactly once')
    finally:
        # Restore even after a failed assertion; no other project's VM is touched.
        if restore_size:
            restore_size()
            node, ssh = vm.connection()
            inventory()
        cleanup()
    print(json.dumps({'passed': passed, 'count': len(passed)}, indent=2))


if __name__ == '__main__':
    vm.run_suite(main)
