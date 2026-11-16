#!/usr/bin/env python3
"""Grouped human output and locally retained full differences over both transports."""
import hashlib
import json
import shlex
import shutil
import subprocess
import time

import vm


def main():
    nodes, connections = {}, {}
    for key, name in [('app01', 'mmx-dev'), ('app02', 'mmx-peer')]:
        vm.up(name)
        vm.reset(name)
        nodes[key], connections[key] = vm.connection(name)
    nodes['app01']['connection'] = 'ssh_only'
    nodes['app01']['vars'] = {'instance': 'blue'}
    nodes['app02']['vars'] = {'instance': 'green'}
    vm.build()
    root = vm.ROOT / 'inspection'
    if root.exists():
        shutil.rmtree(root)
    shutil.copytree(vm.REPO / 'examples/inspection', root)
    vm.write_inventory(root, {'nodes': nodes}, name='example')
    mmx = vm.ROOT / 'bin/mmx'
    base = '/var/tmp/mmx-inspection'
    passed = []
    processes = []

    def remote(script, node='app01'):
        return vm.cmd([*connections[node], 'sudo -n /bin/sh -c ' + shlex.quote('set -eu\n' + script)]).stdout

    def project(tasks, selection='app01', retention='24h'):
        vm.write_project(root, playbook_name='configure', **{'project_name': 'inspection', 'default_inventory': 'example',
            'run_history_retention': retention, 'fleet_stages': [{'id': 'configure', 'nodes': selection, 'node_tasks': tasks}]})

    def invoke(command, *args, expected=None, structured=True):
        p = vm.cmd([mmx, command, *vm.playbook_args(command, "configure"), *args, *(['--json'] if structured else [])], cwd=root, check=False)
        assert p.returncode in (vm.success_codes(command) if expected is None else (expected,)), (p.returncode, p.stdout, p.stderr)
        return json.loads(p.stdout) if structured else p.stdout

    def results(record):
        return record['entries'][0]['response']['results']

    def diff_refs(record):
        refs = {}
        def visit(value):
            if isinstance(value, dict):
                if 'full_diff' in value:
                    ref = value['full_diff']
                    refs[ref['sha256']] = ref['size']
                for child in value.values():
                    visit(child)
            elif isinstance(value, list):
                for child in value:
                    visit(child)
        visit(record['entries'])
        return refs

    def mark(name):
        passed.append(name)
        print('PASS ' + name, flush=True)

    for key in nodes:
        remote('rm -rf ' + base + '; mkdir ' + base + '; rm -f /var/tmp/mmx-inspection-example', key)
    try:
        invoke('validate')
        preview = invoke('dryrun')
        for key in nodes:
            remote('test ! -e /var/tmp/mmx-inspection-example', key)
        human = invoke('results', preview['id'], '--output=summary', '--diff', structured=False)
        assert '+instance = blue' in human and '+instance = green' in human
        assert 'Configured blue' in human and 'Configured green' in human
        applied = invoke('run')
        assert {e['connection_used'] for e in applied['entries']} == {'ssh', 'https'}
        assert 'full_differences_unavailable' not in invoke('results', applied['id'], '--diff=all')
        assert all(e['response']['change'] == 'unchanged' for e in invoke('run')['entries'])
        mark('shipped example keeps node-rendered differences and show values distinct through SSH and HTTPS')

        data = ''.join(f'line {i:04d} = retained configuration\n' for i in range(1500))
        (root / 'large.conf').write_text(data)
        project([{'id': 'shared', 'file': {'path': base + '/shared', 'source': 'large.conf', 'permissions_mode': '0644'}}], 'all')
        shared = invoke('run')
        human = invoke('results', shared['id'], '--output=summary', '--diff', structured=False)
        assert '2 nodes [app01, app02]' in human and 'line 1499' not in human
        full = invoke('results', shared['id'], '--output=summary', '--diff=all', structured=False)
        assert 'line 1499' in full and full.count('full difference for') == 1
        expanded = invoke('results', shared['id'], '--output=nodes', '--diff=all', structured=False)
        assert expanded.count('full difference for') == 2
        saved = invoke('results', shared['id'], '--diff=all')
        for digest, size in diff_refs(saved).items():
            text = saved['full_differences'][digest].encode()
            assert len(text) == size and hashlib.sha256(text).hexdigest() == digest
        assert len(saved['entries']) == 2
        mark('matching changes group once with exact node membership; full human and lossless JSON inspection outlive bounded previews')

        # Diagnostic limits do not constrain installation. Unique 1 MiB texts
        # exhaust one node-job budget; the 18 MiB binary remains an ordinary asset.
        tasks = []
        for i in range(10):
            name = 'budget' + str(i)
            (root / name).write_text(str(i) * (1024 * 1024 - 1) + '\n')
            tasks.append({'id': name, 'file': {'path': base + '/' + name, 'source': name, 'permissions_mode': '0644'}})
        (root / 'over').write_text('o' * (1024 * 1024 + 1))
        (root / 'binary').write_bytes(b'\0' * (18 * 1024 * 1024))
        for name in ['over', 'binary']:
            tasks.append({'id': name, 'file': {'path': base + '/' + name, 'source': name, 'permissions_mode': '0644'}})
        project(tasks)
        budget = invoke('run')
        changes = [r['file_change'] for r in results(budget)]
        assert any('8 MiB limit' in c.get('full_diff_omitted', '') for c in changes)
        assert '1 MiB' in changes[-2]['full_diff_omitted']
        assert sum(diff_refs(budget).values()) <= 8 * 1024 * 1024
        for name in ['budget0', 'budget9', 'over', 'binary']:
            assert remote('sha256sum ' + base + '/' + name).split()[0] == hashlib.sha256((root / name).read_bytes()).hexdigest()
        mark('1 MiB and 8 MiB diagnostic limits give explicit omissions while all files, including an 18 MiB binary, install correctly')

        remote("printf 'deleted-sentinel' > " + base + "/deleted; chmod 644 " + base + "/deleted")
        (root / 'small-binary').write_bytes(b'binary-sentinel\x00')
        secret = 'secret-artifact-sentinel'
        vm.ENV['MMX_INSPECTION_SECRET'] = secret
        tasks = [
            {'id': 'private', 'file': {'path': base + '/private', 'content': 'private-artifact-sentinel', 'permissions_mode': '0600'}},
            {'id': 'binary', 'file': {'path': base + '/small-binary', 'source': 'small-binary', 'permissions_mode': '0644'}},
            {'id': 'deleted', 'file': {'path': base + '/deleted', 'state': 'absent'}},
            {'id': 'secret', 'file': {'path': base + '/secret', 'content': '{{ .secrets.token }}', 'permissions_mode': '0600'}},
            {'id': 'validator', 'file': {'path': base + '/validator', 'content': 'validator-sentinel', 'permissions_mode': '0644',
                'check_new_file': {'shell': {'script': 'test -n "$TOKEN"', 'env': {'TOKEN': '{{ .secrets.token }}'}}}}},
        ]
        project(tasks)
        config = json.loads((root / 'mmx_project_settings.yml').read_text())
        config['secrets_from_env'] = {'token': 'MMX_INSPECTION_SECRET'}
        vm.write_settings(root, config)
        suppressed = invoke('run')
        assert not diff_refs(suppressed)
        job = suppressed['entries'][0]['job_id']
        remote('test ! -d /var/lib/mmx/jobs/' + job + '/diffs')
        assert not (root / '.mmx/runs' / suppressed['id'] / 'diffs').exists()
        mark('private, binary, deleted, secret-derived, and secret-validator contents never become stored artifacts')

        project([{'id': 'announce', 'show': 'checking configuration'}, {'id': 'fail', 'retry': False, 'shell': {'script': 'echo common-error >&2; exit 1'}}], 'all')
        failed = invoke('run', expected=1)
        human = invoke('results', failed['id'], '--output=summary', structured=False)
        assert '2 nodes [app01, app02] / fail: failed' in human and human.count('common-error') == 1
        assert 'checking configuration' in human
        mark('matching failures group without hiding the error or explicit show output')

        # nodes and explain show except_nodes and the fleet stages
        # --only-fleet-stages leaves out, locally, and reject what run rejects.
        vm.write_project(root, playbook_name='configure', project_name='inspection', default_inventory='example', fleet_stages=[
            {'id': 'network', 'nodes': 'all', 'except_nodes': ['app02'], 'node_tasks': [{'file': {"permissions_mode": "0644", 'path': base + '/network', 'content': 'x'}}]},
            {'id': 'ruby', 'nodes': 'all', 'node_tasks': [{'file': {"permissions_mode": "0644", 'path': base + '/ruby', 'content': 'x'}}]},
            {'id': 'sites', 'nodes': 'app01', 'requires': {'same_node': ['ruby']}, 'node_tasks': [{'show': 'sites'}]}])
        shown = invoke('nodes', '--only-fleet-stages', 'network', structured=False)
        assert 'Only fleet stages: network' in shown and 'network   all except app02   → app01' in shown, shown
        assert 'ruby      all                → none; left out by --only-fleet-stages' in shown, shown
        explained = invoke('explain', '--only-fleet-stages', 'ruby,sites', '--limit', 'app01')
        stages = {s['path']: s for s in explained['fleet_stages']}
        assert explained['only_fleet_stages'] == ['ruby', 'sites'] and stages['network']['left_out_by_only_fleet_stages'], explained
        assert stages['ruby']['nodes'] == ['app01'] and stages['ruby']['left_out'] == ['app02'] and stages['sites']['nodes'] == ['app01']
        assert invoke('explain')['fleet_stages'][0]['except_nodes'] == ['app02']
        for command in ('nodes', 'explain'):
            p = vm.cmd([mmx, command, 'configure', '--only-fleet-stages', 'sites'], cwd=root, check=False)
            assert p.returncode == 4 and 'use --only-fleet-stages ruby,sites' in p.stderr, p.stderr
        preview = invoke('dryrun', '--only-fleet-stages', 'network')
        assert {(e['fleet_stage'], e['node']) for e in preview['entries']} == {('network', 'app01')}
        for key in nodes:
            remote('test ! -e ' + base + '/network; test ! -e ' + base + '/ruby', key)
        mark('nodes and explain show except_nodes and the fleet stages --only-fleet-stages leaves out; dryrun follows the same selection')

        # A node finishes without the controller; status saves the result and
        # downloads its artifacts while the node still has them.
        gate, entered = base + '/release', base + '/entered'
        project([{'id': 'detached_file', 'file': {'path': base + '/detached', 'source': 'large.conf', 'permissions_mode': '0644'}},
                 {'shell': {'script': 'touch ' + entered + '; while ! test -f ' + gate + '; do sleep .1; done'}}])
        with (root / 'detached.stdout').open('w') as out, (root / 'detached.stderr').open('w') as err:
            process = subprocess.Popen([str(mmx), 'run', 'configure', '--json'], cwd=root, env=vm.ENV, stdout=out, stderr=err)
            processes.append(process)
            remote('for n in $(seq 1 200); do if test -f ' + entered + '; then exit 0; fi; sleep .1; done; exit 1')
            process.kill()
            process.wait(timeout=5)
        detached = invoke('results')
        job = detached['entries'][0]['job_id']
        # The running job's saved partial result names artifacts not local yet.
        pending = invoke('results', detached['id'], '--diff=all')
        assert not pending['full_differences'] and set(pending.get('full_differences_unavailable', [])) == set(diff_refs(pending))
        remote('touch ' + gate + '; for n in $(seq 1 200); do if test -f /var/lib/mmx/jobs/' + job + '/result.json; then exit 0; fi; sleep .1; done; exit 1')
        finished = invoke('status', detached['id'])
        assert finished['entries'][0]['response']['status'] == 'succeeded'
        full = invoke('results', detached['id'], '--diff=all')
        assert full['full_differences'] and 'full_differences_unavailable' not in full
        # Nodes keep a finished job for 72 hours from its result.
        remote("touch -d '4 days ago' /var/lib/mmx/jobs/" + job + '/result.json')
        project([{'show': 'cleanup traffic'}])
        invoke('run')
        remote('test ! -e /var/lib/mmx/jobs/' + job)
        assert invoke('results', detached['id'], '--diff=all')['full_differences'] == full['full_differences']
        mark('unsaved artifacts are noted as unavailable; status downloads a disconnected result\'s artifacts; local full differences survive node expiry')

        # Disable node access for log inspection. Each invocation is a fresh
        # controller process; the old project file is no longer needed either.
        vm.close_forwards()
        blocked = root / 'blocked-bin'
        blocked.mkdir()
        marker = root / 'unexpected-ssh'
        (blocked / 'ssh').write_text('#!/bin/sh\ntouch ' + shlex.quote(str(marker)) + '\nexit 99\n')
        (blocked / 'ssh').chmod(0o700)
        previous_path = vm.ENV['PATH']
        vm.ENV['PATH'] = str(blocked) + ':' + previous_path
        try:
            (root / 'playbooks/configure.yml').write_text('invalid current project')
            assert invoke('results', detached['id'], '--diff=all')['full_differences'] == full['full_differences']
            assert not marker.exists()
            digest = next(iter(diff_refs(full)))
            artifact = root / '.mmx/runs' / detached['id'] / 'diffs' / digest
            original = artifact.read_bytes()
            artifact.write_bytes(b'!' * len(original))
            assert 'is not available locally' in invoke('results', detached['id'], '--diff=all', structured=False)
            artifact.unlink()
            assert digest in invoke('results', detached['id'], '--diff=all')['full_differences_unavailable']
            assert invoke('results', detached['id'])['status'] == 'succeeded'
            assert not marker.exists()
        finally:
            vm.ENV['PATH'] = previous_path
        mark('fresh controllers inspect offline; missing or corrupt artifacts show as unavailable without executing work')

        # Controller expiry removes its own artifacts with the run directory.
        project([{'show': 'cleanup local history'}], retention='1s')
        time.sleep(1.1)
        invoke('results', structured=False)
        assert not (root / '.mmx/runs' / detached['id']).exists()
        mark('controller retention removes full differences with their owning run')
    finally:
        vm.ENV.pop('MMX_INSPECTION_SECRET', None)
        for process in processes:
            if process.poll() is None:
                process.kill()
                process.wait(timeout=5)
        for key in nodes:
            remote('rm -rf ' + base + '; rm -f /var/tmp/mmx-inspection-example', key)
    print(json.dumps({'passed': passed, 'count': len(passed)}, indent=2))


if __name__ == '__main__':
    vm.run_suite(main)
