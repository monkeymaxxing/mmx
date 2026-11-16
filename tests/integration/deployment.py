#!/usr/bin/env python3
"""Execute the documented rolling deployment on two disposable Debian VMs."""
import json
from pathlib import Path
import shlex
import shutil
import socket
import subprocess
import time

import vm

BASE = '/var/tmp/mmx-demo'
DEMO = f'python3 {BASE}/demo.py'


def main():
    nodes, connections = {}, {}
    for node, name in [('app01', 'mmx-dev'), ('app02', 'mmx-peer')]:
        vm.up(name)
        nodes[node], connections[node] = vm.connection(name)
        vm.reset(name)
    nodes['loadbalancer'] = dict(nodes['app02'])
    connections['loadbalancer'] = connections['app02']
    workspace = vm.ROOT / 'deployment'
    workspace.mkdir(parents=True, exist_ok=True)
    # The suite resets node identity, so its disposable controller state resets too.
    if (workspace / '.mmx').exists():
        shutil.rmtree(workspace / '.mmx')
    example = vm.REPO / 'examples/rolling-deployment'
    shutil.copytree(example, workspace, dirs_exist_ok=True, ignore=shutil.ignore_patterns('__pycache__'))
    vm.write_inventory(workspace, {'nodes': nodes, 'groups': {'app_servers': {'nodes': ['app01', 'app02']}}}, name='example')
    original_settings = (example / 'mmx_project_settings.yml').read_text()
    vm.build()
    mmx = vm.ROOT / 'bin/mmx'
    processes = []
    passed = []

    def remote(node, script):
        return vm.cmd([*connections[node], 'sudo -n /bin/sh -c ' + shlex.quote(script)]).stdout

    def wait_for(check, description):
        deadline = time.monotonic() + 40
        while time.monotonic() < deadline:
            result = check()
            if result:
                return result
            time.sleep(.1)
        raise AssertionError('timed out: ' + description)

    def project(release, schema=2):
        (workspace / 'mmx_project_settings.yml').write_text(original_settings.replace('release: v2', 'release: ' + release).replace('schema: 2', 'schema: ' + str(schema)))

    def run(command, expected=None, *args):
        result = vm.cmd([mmx, command, *vm.playbook_args(command, "deploy"), '--json', *args], cwd=workspace, check=False)
        assert result.returncode in (vm.success_codes(command) if expected is None else (expected,)), (command, result.returncode, result.stdout, result.stderr)
        return json.loads(result.stdout)

    def state(node):
        return json.loads(remote(node, DEMO + ' state'))

    def events(node, action=None):
        lines = remote(node, f'cat {BASE}/events 2>/dev/null || true').splitlines()
        return [line for line in lines if action is None or line.startswith(action + ' ')]

    def members():
        return json.loads(remote('loadbalancer', f'cat {BASE}/members.json'))

    def traffic(expected):
        responses = remote('loadbalancer', f'for i in 1 2 3 4; do {DEMO} request; done')
        got = {item['name']: item['release'] for item in map(json.loads, responses.splitlines())}
        assert got == expected, got

    def entry(record, fleet_stage, node):
        return next(e for e in record['entries'] if e['fleet_stage'] == fleet_stage and e['node'] == node)

    def mark(name):
        passed.append(name)
        print('PASS ' + name, flush=True)

    def tunnel(node, option, mapping):
        args = [str(x) for x in [*vm.unshared(connections[node][:-1]), '-N', '-o', 'ExitOnForwardFailure=yes', option, mapping, connections[node][-1]]]
        process = subprocess.Popen(args, env=vm.ENV, stdout=subprocess.DEVNULL, stderr=subprocess.PIPE)
        processes.append(process)
        return process

    try:
        for node in ['app01', 'app02']:
            remote(node, f"systemctl stop mmx-demo-app mmx-demo-lb 2>/dev/null || true; rm -rf {BASE}; mkdir -p {BASE}")
            source = (example / 'files/demo.py').read_text()
            remote(node, f'printf %s {shlex.quote(source)} > {BASE}/demo.py; '
                         f'printf %s {node} > {BASE}/name; printf v1 > {BASE}/release; printf old > {BASE}/site')
            unit = ('[Service]\n' + f'ExecStart=/usr/bin/python3 {BASE}/demo.py serve-app\n'
                    + f'ExecReload=/usr/bin/python3 {BASE}/demo.py reload\n')
            remote(node, f'printf %s {shlex.quote(unit)} > /etc/systemd/system/mmx-demo-app.service; systemctl daemon-reload; systemctl start mmx-demo-app')
            wait_for(lambda: remote(node, f'if test -f {BASE}/loaded.json; then echo yes; fi').strip(), 'app ready')

        # The VM NAT networks are isolated. Test-owned SSH forwards connect the
        # real app HTTP listener to the peer's real load balancer.
        with socket.socket() as listener:
            listener.bind(('127.0.0.1', 0))
            port = listener.getsockname()[1]
        tunnel('app01', '-L', f'127.0.0.1:{port}:127.0.0.1:18990')
        tunnel('loadbalancer', '-R', f'127.0.0.1:18992:127.0.0.1:{port}')
        backends = {'app01': 'http://127.0.0.1:18992', 'app02': 'http://127.0.0.1:18990'}
        remote('loadbalancer', f'printf %s {shlex.quote(json.dumps(backends))} > {BASE}/backends.json; '
                              f'printf %s \'["app01","app02"]\' > {BASE}/members.json')
        unit = '[Service]\n' + f'ExecStart=/usr/bin/python3 {BASE}/demo.py serve-lb\n'
        remote('loadbalancer', f'printf %s {shlex.quote(unit)} > /etc/systemd/system/mmx-demo-lb.service; systemctl daemon-reload; systemctl start mmx-demo-lb')
        # app02 answers at once; app01 only when both tunnels are up.
        def both_backends_answer():
            responses = remote('loadbalancer', f'for i in 1 2 3 4; do {DEMO} request 2>/dev/null; done; true')
            return {json.loads(line)['name'] for line in responses.splitlines() if line.startswith('{')} == {'app01', 'app02'}
        wait_for(both_backends_answer, 'proxy can reach both backends')
        traffic({'app01': 'v1', 'app02': 'v1'})

        project('v2')
        run('validate')
        run('dryrun')
        assert state('app01')['release'] == state('app02')['release'] == 'v1'
        assert events('loadbalancer', 'drain') == []
        remote('app01', f'test ! -e {BASE}/app.db')
        mark('documented playbook validates and previews without changing traffic, configuration, or schema')

        completed = run('run')
        traffic({'app01': 'v2', 'app02': 'v2'})
        for node in ['app01', 'app02']:
            assert state(node)['site'] == 'example' and state(node)['reloads'] == 1
        assert completed['selections']['rollout']['iterations']['app01']['outputs']['release'] == 'v2'
        assert completed['entries'][-1]['fleet_stage'] == 'report'
        assert completed['selections']['rollout']['iterations']['app01']['change'] == 'changed'
        assert events('app01', 'migrate') == ['migrate 2']
        remote('app02', f'test ! -e {BASE}/app.db')
        mark('ordered deployment serves the new release; two looped files cause one reload per app; migration runs on one node')

        drains = events('loadbalancer', 'drain')
        run('run')
        assert events('loadbalancer', 'drain') == drains
        assert state('app01')['reloads'] == state('app02')['reloads'] == 1
        assert events('app01', 'migrate') == ['migrate 2']
        mark('healthy unchanged run leaves traffic membership and service reload counts unchanged')

        project('v3')
        remote('app01', f'touch {BASE}/fail-before-reload')
        failed = run('run', 1)
        assert state('app01')['release'] == 'v2'
        assert remote('app01', f'cat {BASE}/release') == 'v3'
        assert members() == ['app02'] and state('app02')['release'] == 'v2'
        traffic({'app02': 'v2'})
        remote('app01', f'rm {BASE}/fail-before-reload')
        resume = run('resume')
        assert resume['id'] == failed['id']
        assert state('app01')['reloads'] == state('app02')['reloads'] == 2
        traffic({'app01': 'v3', 'app02': 'v3'})
        mark('failure before reload leaves the next app alone; resume activates already-written looped configuration')

        project('v4')
        remote('app01', f'touch {BASE}/fail-health')
        run('run', 1)
        assert state('app01')['release'] == 'v4' and state('app01')['reloads'] == 3
        assert state('app02')['release'] == 'v3' and members() == ['app02']
        traffic({'app02': 'v3'})
        remote('app01', f'rm {BASE}/fail-health')
        run('resume')
        assert state('app01')['reloads'] == state('app02')['reloads'] == 3
        traffic({'app01': 'v4', 'app02': 'v4'})
        mark('failed health keeps the app drained; resume checks health without repeating its confirmed reload')

        project('v5')
        remote('loadbalancer', f'touch {BASE}/fail-rejoin-app01')
        failed = run('run', 1)
        before = events('app01', 'inspect')
        assert members() == ['app02'] and state('app02')['release'] == 'v4'
        remote('loadbalancer', f'rm {BASE}/fail-rejoin-app01')
        resumed = run('resume')
        assert events('app01', 'inspect') == before
        for fleet_stage in ['inspect_app', 'drain', 'update', 'verify']:
            node = 'loadbalancer' if fleet_stage == 'drain' else 'app01'
            path = 'rollout[app01]/' + fleet_stage
            assert entry(failed, path, node)['job_id'] == entry(resumed, path, node)['job_id']
        traffic({'app01': 'v5', 'app02': 'v5'})
        saved_results = run('results', 0, '--fleet-stage', 'rollout[app01]', '--node', 'app01')
        assert all(e['fleet_stage'].startswith('rollout[app01]/') and e['node'] == 'app01' for e in saved_results['entries'])
        mark('resume restores completed outputs without repeating inspection, update, or verification; results retain loop context')

        project('v6')
        remote('loadbalancer', f'touch {BASE}/fail-rejoin-app01')
        failed = run('run', 1)
        # Record the baseline only after the intended failure: app01 updated, then rejoin failed.
        statuses = {e['fleet_stage']: e['response']['status'] for e in failed['entries'] if e['fleet_stage'].startswith('rollout[app01]/')}
        assert statuses.get('rollout[app01]/update') == 'succeeded' and statuses.get('rollout[app01]/rejoin') == 'failed', failed
        assert state('app01')['release'] == 'v6' and members() == ['app02'], (state('app01'), members())
        drains = events('loadbalancer', 'drain')
        reloads = state('app01')['reloads']
        remote('loadbalancer', f'rm {BASE}/fail-rejoin-app01')
        fresh = run('run')
        assert state('app01')['reloads'] == reloads, (reloads, state('app01'), fresh)
        assert events('loadbalancer', 'drain').count('drain app01') == drains.count('drain app01')
        traffic({'app01': 'v6', 'app02': 'v6'})
        mark('a fresh run observes a current but drained app and rejoins it without another drain or reload')

        project('v7')
        remote('loadbalancer', f'touch {BASE}/hold-drain-app01')
        with (workspace / 'disconnected.stdout').open('w') as out, (workspace / 'disconnected.stderr').open('w') as err:
            process = subprocess.Popen([str(mmx), 'run', 'deploy', '--json'], cwd=workspace, env=vm.ENV, stdout=out, stderr=err)
            processes.append(process)
            wait_for(lambda: members() == ['app02'], 'app01 drained')
            process.kill()
            process.wait(timeout=5)
        before = events('app01', 'inspect')
        remote('loadbalancer', f'rm {BASE}/hold-drain-app01')
        def collected():
            status = run('status')
            selected = [e for e in status['entries'] if e['fleet_stage'] == 'rollout[app01]/drain']
            return status if selected and selected[0]['response']['status'] == 'succeeded' else None
        status = wait_for(collected, 'admitted drain collected after disconnection')
        assert status['status'] != 'succeeded'
        run('resume')
        assert len(events('app01', 'inspect')) == len(before) + 1  # only the later verify task
        traffic({'app01': 'v7', 'app02': 'v7'})
        mark('controller loss preserves the admitted drain and saved selections; resume finishes remaining servers')

        project('v8', schema=3)
        remote('app01', f'touch {BASE}/fail-migration')
        drains = events('loadbalancer', 'drain')
        run('run', 1)
        assert events('loadbalancer', 'drain') == drains
        remote('app01', f'rm {BASE}/fail-migration')
        run('resume')
        assert events('app01', 'migrate') == ['migrate 2', 'migrate 3']
        traffic({'app01': 'v8', 'app02': 'v8'})
        mark('singleton migration failure stops rollout; its state check makes explicit resume safe')

        # --limit app02 picks app02's iteration of the for_each_node loop. Its
        # drain and rejoin still run on the load balancer, a helper node outside
        # the limit, which dryrun and run list before they connect.
        project('v9', schema=3)
        drains = events('loadbalancer', 'drain')
        preview = vm.cmd([mmx, 'dryrun', 'deploy', '--limit', 'app02'], cwd=workspace, check=False)
        assert preview.returncode in vm.success_codes('dryrun'), (preview.stdout, preview.stderr)
        helper = 'Nodes outside --limit that help selected nodes\n  loadbalancer   rollout[app02]/inspect_membership, rollout[app02]/drain, rollout[app02]/rejoin\n'
        assert helper in preview.stderr, preview.stderr
        assert events('loadbalancer', 'drain') == drains and state('app02')['release'] == 'v8'
        limited = vm.cmd([mmx, 'run', 'deploy', '--limit', 'app02'], cwd=workspace, check=False)
        assert limited.returncode == 0 and helper in limited.stderr, (limited.stdout, limited.stderr)
        assert events('loadbalancer', 'drain')[len(drains):] == ['drain app02'], events('loadbalancer', 'drain')
        assert state('app02')['release'] == 'v9' and state('app01')['release'] == 'v8'
        assert events('app01', 'migrate') == ['migrate 2', 'migrate 3']
        traffic({'app01': 'v8', 'app02': 'v9'})
        run('run')
        traffic({'app01': 'v9', 'app02': 'v9'})
        mark('a limit runs one app server\'s iteration with its drain and rejoin on the load balancer, shown before connecting')

        collection_tests(workspace, run, remote, mark)
        (workspace / 'result.json').write_text(json.dumps({'passed': passed}, indent=2) + '\n')
        print(f'{len(passed)} deployment scenarios passed', flush=True)
    finally:
        for process in processes:
            if process.poll() is None:
                process.terminate()
                process.wait(timeout=5)
        for node in ['app01', 'app02']:
            remote(node, 'systemctl stop mmx-demo-app mmx-demo-lb 2>/dev/null || true; '
                         'rm -f /etc/systemd/system/mmx-demo-app.service /etc/systemd/system/mmx-demo-lb.service; systemctl daemon-reload')


def collection_tests(workspace, run, remote, mark):
    role = workspace / 'roles/backends'
    role.mkdir(exist_ok=True)
    (role / 'files').mkdir(exist_ok=True)
    (role / 'files/backends.tmpl').write_text('{{ range $name, $backend := .inputs.backends }}{{ $name }} {{ $backend.port }}\n{{ end }}')
    (role / 'main.yml').write_text('inputs: {backends: {type: mapping, required: true}}\nnode_tasks:\n- file: {"permissions_mode": "0644", path: /var/tmp/mmx-demo/backend-list, template: files/backends.tmpl}\n')
    definition = {'project_name': 'collection_demo', 'default_inventory': 'example', 'fleet_stages': [
        {'id': 'inspect', 'nodes': ['app01', 'app02'], 'node_tasks': [{'shell': {'script': 'test ! -f /var/tmp/mmx-demo/fail-inspect', 'changed_when': False}}], 'outputs': {'port': 18990}},
        {'id': 'render', 'nodes': 'loadbalancer', 'node_tasks': [{'role': 'backends', 'inputs': {'backends': '{{ .outputs.inspect }}'}}]},
        {'id': 'one', 'nodes': 'loadbalancer', 'node_tasks': [{'show': '{{ .outputs.inspect.app01.port }}'}]},
        {'id': 'facts', 'nodes': 'loadbalancer', 'outputs': {'name': '{{ .node.name }}'}},
    ]}
    vm.write_project(workspace, playbook_name='deploy', **definition)
    remote('app02', f'touch {BASE}/fail-inspect')
    failed = run('run', 1)
    statuses = {e['fleet_stage']: e['response']['status'] for e in failed['entries'] if e['node'] == 'loadbalancer'}
    assert statuses == {'render': 'blocked', 'one': 'succeeded', 'facts': 'succeeded'}, statuses
    assert next(e for e in failed['entries'] if e['fleet_stage'] == 'facts')['response']['outputs']['name'] == 'loadbalancer'
    remote('loadbalancer', f'test ! -f {BASE}/backend-list')
    remote('app02', f'rm {BASE}/fail-inspect')
    run('resume')
    assert remote('loadbalancer', f'cat {BASE}/backend-list') == 'app01 18990\napp02 18990\n'
    mark('whole backend collections reach a role template only when complete; a good individual node remains readable')


if __name__ == '__main__':
    vm.run_suite(main)
