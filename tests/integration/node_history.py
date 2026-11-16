#!/usr/bin/env python3
"""Node job history through public commands and independently inspected files."""
import hashlib
import json
import shlex
import shutil
import subprocess
import time

import vm


def main():
    vm.up()
    vm.reset()
    vm.build()
    node, ssh = vm.connection()
    root = vm.ROOT / 'node-history'
    if root.exists():
        shutil.rmtree(root)
    a, b = root / 'controller-a', root / 'controller-b'
    shutil.copytree(vm.REPO / 'examples/history', a)
    b.mkdir(parents=True)
    mmx = vm.ROOT / 'bin/mmx'
    passed = []
    processes = []

    def remote(script):
        return vm.cmd([*ssh, 'sudo -n /bin/sh -c ' + shlex.quote('set -eu\n' + script)]).stdout

    def inventory(work, connection='https_with_ssh_fallback'):
        vm.write_inventory(work, {'nodes': {'app': dict(node, connection=connection)}}, name='example')

    def project(work, tasks, retention='2s'):
        vm.write_project(work, playbook_name='configure', **{'project_name': 'history_' + work.name.replace('-', '_'),
            'default_inventory': 'example', 'run_history_retention': retention,
            'fleet_stages': [{'id': 'configure', 'nodes': 'all', 'node_tasks': tasks}]})

    def run(work, command, expected=0, *args):
        p = vm.cmd([mmx, command, *vm.playbook_args(command, "configure"), *args, '--json'], cwd=work, check=False)
        assert p.returncode == expected, (command, p.returncode, p.stdout, p.stderr)
        return json.loads(p.stdout)

    def entry(record):
        return record['entries'][0]

    def directory(record):
        return '/var/lib/mmx/jobs/' + entry(record)['job_id']

    def wait_for(script):
        remote('for n in $(seq 1 200); do if ' + script + '; then exit 0; fi; sleep 0.1; done; exit 1')

    def age():
        time.sleep(3)  # past the 2s retention with margin for guest clock steps

    def expire(record):
        # Nodes keep a finished job for 72 hours from its result.
        remote("touch -d '4 days ago' " + directory(record) + '/result.json')

    def mark(message):
        passed.append(message)
        print('PASS ' + message, flush=True)

    def last_successful_run(project_name):
        return json.loads(remote('cat /var/lib/mmx/last_successful_run/' + project_name + '/configure'))

    inventory(a, 'ssh_only')
    inventory(b)
    project(b, [{'show': 'ordinary traffic'}], 'forever')
    try:
        run(a, 'validate')
        initial = run(a, 'run')
        response = entry(initial)['response']
        assert entry(initial)['connection_used'] == 'ssh' and response['finished']
        raw = json.loads(remote('cat ' + directory(initial) + '/result.json'))
        assert raw['finished'] == response['finished']
        request = json.loads(remote('cat ' + directory(initial) + '/request.json'))
        assert 'run_history_retention' not in request
        native = run(b, 'run')
        assert entry(native)['connection_used'] == 'https' and entry(native)['response']['finished']
        # Each run also records itself as its playbook's last successful run.
        for record, transport in ((initial, 'SSH'), (native, 'HTTPS')):
            assert last_successful_run(record['project'])['run_id'] == record['id'], transport
        mark('shipped example keeps the finished result on the node through SSH and HTTPS, and records the last successful run over both')

        inventory(a)
        fail = '/var/tmp/mmx-history-fail'
        counter = '/var/tmp/mmx-history-count'
        remote('touch ' + fail + '\nrm -f ' + counter)
        project(a, [{'shell': {'script': 'echo attempt >> ' + counter + '; test ! -f ' + fail}}])
        failed = run(a, 'run', 1)
        old_id = entry(failed)['job_id']
        expire(failed)
        # Inspecting the current job must not sweep that job before returning it.
        runner = '/opt/mmx/bin/' + entry(failed)['runner_hash'] + '/mmx-runner'
        remote(runner + ' status ' + old_id + ' >/dev/null\ntest -f ' + directory(failed) + '/result.json')
        run(b, 'run')
        remote('test ! -e ' + directory(failed))
        assert run(a, 'results', 0, failed['id'])['status'] == 'failed'
        assert run(a, 'status', 0, failed['id'])['status'] == 'failed'
        remote('rm ' + fail)
        (a / 'playbooks/configure.yml').write_text('invalid current project')
        resumed = run(a, 'resume', 0, failed['id'])
        assert entry(resumed)['job_id'] != old_id and entry(resumed)['node_job_attempt'] == 2
        assert remote('cat ' + counter) == 'attempt\nattempt\n'
        mark('expired node results remain inspectable locally and saved resume runs a new attempt with frozen definitions')

        # The node completes after the controller goes away; ordinary traffic
        # keeps the result until it ages out, so status can still save it.
        gate = '/var/tmp/mmx-history-release'
        entered = '/var/tmp/mmx-history-entered'
        remote('rm -f ' + gate + ' ' + entered)
        project(a, [{'shell': {'script': 'touch ' + entered + '; while ! test -f ' + gate + '; do sleep .1; done', 'changed_when': False}}])
        with (root / 'detached.stdout').open('w') as out, (root / 'detached.stderr').open('w') as err:
            p = subprocess.Popen([str(mmx), 'run', 'configure', '--json'], cwd=a, env=vm.ENV, stdout=out, stderr=err)
            processes.append(p)
            wait_for('test -f ' + entered)
            p.kill()
            p.wait(timeout=5)
        detached = run(a, 'results')
        remote('touch ' + gate)
        wait_for('test -f ' + directory(detached) + '/result.json')
        age()
        run(b, 'run')
        remote('test -f ' + directory(detached) + '/result.json')
        recovered = run(a, 'status', 0, detached['id'])
        assert recovered['status'] == 'succeeded' and entry(recovered)['response']['finished']
        expire(detached)
        run(b, 'run')
        remote('test ! -e ' + directory(detached))
        assert run(a, 'results', 0, detached['id'])['status'] == 'succeeded'
        mark('a disconnected run keeps its finished node result until it ages out; status saves it and results keep it')

        # Node expiry follows the node's fixed window, not a project's retention.
        project(a, [{'show': 'retained'}], 'forever')
        forever = run(a, 'run')
        project(b, [{'show': 'short retention'}])
        short = run(b, 'run')
        age()
        expire(forever)
        run(a, 'run')
        remote('test ! -e ' + directory(forever) + '\ntest -f ' + directory(short) + '/result.json')
        mark('nodes expire finished jobs by age alone, whatever retention each project sets')

        remote('rm -f ' + counter)
        project(a, [{'shell': {'script': 'echo once >> ' + counter}}])
        missing = run(a, 'run')
        # The controller names each saved entry by its fleet stage and node.
        saved_entry = entry(missing)
        local = a / '.mmx/runs' / missing['id'] / 'entries' / (hashlib.sha256((saved_entry['fleet_stage'] + '\0' + saved_entry['node']).encode()).hexdigest() + '.json')
        saved = json.loads(local.read_text())
        saved['response'] = {'id': entry(missing)['job_id'], 'status': 'unknown'}
        local.write_text(json.dumps(saved))
        remote('rm -rf ' + directory(missing))
        unknown = run(a, 'status', 3, missing['id'])
        assert entry(unknown)['response']['status'] == 'not_found'
        assert entry(missing)['job_id'] in entry(unknown)['response']['error']
        assert remote('cat ' + counter) == 'once\n'
        resume = run(a, 'resume', 0, missing['id'])
        assert entry(resume)['job_id'] != entry(missing)['job_id']
        assert remote('cat ' + counter) == 'once\nonce\n'
        mark('missing node and controller results report the job identity without replay; explicit resume uses a new job')

        # The node records a playbook's last successful run for monitoring:
        # one root-only JSON line that a dryrun, a run-command run, and a failed
        # run leave alone, and that a resume which succeeds replaces.
        path = '/var/lib/mmx/last_successful_run/history_controller_a/configure'
        remote('rm -rf /var/lib/mmx/last_successful_run')
        project(a, [{'shell': {'script': 'test ! -f ' + fail, 'dryrun': 'test ! -f ' + fail, 'changed_when': False}}])
        run(a, 'dryrun')
        command = vm.cmd([mmx, 'run-command', '--json', '--nodes', 'all', '--', 'true'], cwd=a)
        assert json.loads(command.stdout)['status'] == 'succeeded'
        remote('test ! -e /var/lib/mmx/last_successful_run')
        succeeded = run(a, 'run')
        assert entry(succeeded)['response']['change'] == 'unchanged' and 'housekeeping' not in entry(succeeded)
        line = remote('cat ' + path)
        written = json.loads(line)
        assert line.count('\n') == 1 and list(written) == ['run_id', 'inventory', 'limit', 'only_fleet_stages', 'finished_at'], line
        assert written['run_id'] == succeeded['id'] and written['inventory'] == 'example' and written['limit'] == written['only_fleet_stages'] == '', written
        assert written['finished_at'] == succeeded['finished'][:19] + 'Z', (written, succeeded['finished'])
        assert remote("stat -c '%U %a' " + path) == 'root 600\n'
        rerun = run(a, 'run')
        assert last_successful_run('history_controller_a')['run_id'] == rerun['id']
        run(a, 'dryrun')
        vm.cmd([mmx, 'run-command', '--json', '--nodes', 'all', '--', 'true'], cwd=a)
        remote('touch ' + fail)
        failed = run(a, 'run', 1)
        assert last_successful_run('history_controller_a')['run_id'] == rerun['id']
        remote('rm ' + fail)
        resumed = run(a, 'resume')
        assert resumed['id'] == failed['id'] and last_successful_run('history_controller_a')['run_id'] == failed['id']
        mark('the node records the last successful run; dryrun, run-command, and a failed run keep the earlier record; resume replaces it')
    finally:
        for process in processes:
            if process.poll() is None:
                process.kill()
                process.wait(timeout=5)
        remote('rm -f /var/tmp/mmx-history-example /var/tmp/mmx-history-fail /var/tmp/mmx-history-count /var/tmp/mmx-history-release /var/tmp/mmx-history-entered')
    print(json.dumps({'passed': passed, 'count': len(passed)}, indent=2))


if __name__ == '__main__':
    vm.run_suite(main)
