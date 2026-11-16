#!/usr/bin/env python3
"""snap_package through the public CLI, checked with snap list, where the distribution packages snapd."""
import json
import shlex
import shutil

import vm

SNAP = 'hello-world'
# The distributions whose own repositories have snapd. The Red Hat family
# has it in EPEL, which a project adds itself.
SNAPD = {'debian', 'ubuntu'}


def main():
    vm.up()
    node, ssh = vm.connection()
    vm.reset()
    workspace = vm.ROOT / 'snap-packages'
    if workspace.exists():
        shutil.rmtree(workspace)
    vm.write_inventory(workspace, {'nodes': {'app': node}}, name='example')
    vm.build()
    mmx = vm.ROOT / 'bin/mmx'
    passed = []

    def remote(script):
        return vm.cmd([*ssh, 'sudo -n /bin/sh -c ' + shlex.quote('set -eu\n' + script)]).stdout.strip()

    # snapd may still be busy with its own first refresh, and drops a store
    # request it cannot finish; another try a little later succeeds.
    def project(tasks):
        vm.write_project(workspace, project_name='snap_packages', node_task_defaults={'retry': {'pause_between_tries': '10s'}},
                         fleet_stages=[{'id': 'configure', 'nodes': 'all', 'node_tasks': tasks}])

    def run(command, expected=None, timeout=300):
        result = vm.cmd([mmx, command, *vm.playbook_args(command), '--json'], cwd=workspace, check=False, timeout=timeout)
        assert result.returncode in (vm.success_codes(command) if expected is None else (expected,)), (command, result.stdout, result.stderr)
        return json.loads(result.stdout)

    def task(record):
        return next(r for r in record['entries'][0]['response']['results'] if r['task'].rsplit('/', 1)[-1] == 'snap')

    def mark(message):
        passed.append(message)
        print('PASS ' + message, flush=True)

    def tracking():
        return remote(f"snap list {SNAP} 2>/dev/null | awk 'NR == 2 {{print $4}}' || true")

    distribution = remote('. /etc/os-release; echo "$ID"')
    if distribution not in SNAPD:
        # Without snapd, a dryrun cannot check the task and a run fails,
        # naming the package.
        project([{'id': 'snap', 'snap_package': {'packages': [{'name': SNAP, 'channel': 'latest/stable'}]}}])
        assert task(run('dryrun'))['status'] == 'not_checked'
        failed = task(run('run', 1))
        assert failed['failure_kind'] == 'missing_prerequisite' and 'snapd' in failed['reason'], failed
        mark(f'{distribution} does not package snapd: snap_package says to install it')
    else:
        try:
            project(vm.package_tasks(['snapd']) + [{'systemd_service': {'name': 'snapd.socket', 'start_at_boot': True, 'state': 'running'}}])
            # Installing snapd takes minutes on a busy test server.
            run('run', timeout=1200)
            remote('snap wait system seed.loaded')

            project([{'id': 'snap', 'snap_package': {'packages': [{'name': SNAP, 'channel': 'latest/stable'}]}}])
            preview = task(run('dryrun'))
            assert preview['change'] == 'would_change' and preview['value'] == [{'name': SNAP, 'from': None, 'to': 'latest/stable'}], preview
            assert tracking() == ''
            installed = task(run('run'))
            assert installed['change'] == 'changed' and installed['value'][0]['to'].startswith('latest/stable revision '), installed
            assert tracking() == 'latest/stable'
            assert task(run('run'))['change'] == 'unchanged' and task(run('dryrun'))['change'] == 'unchanged'
            mark('snap_package installs a snap that follows a channel and reruns unchanged')

            project([{'id': 'snap', 'snap_package': {'packages': [{'name': SNAP, 'channel': 'latest/edge'}]}}])
            switched = task(run('run'))
            assert switched['change'] == 'changed' and switched['value'][0]['to'].startswith('latest/edge revision '), switched
            assert tracking() == 'latest/edge'
            project([{'id': 'snap', 'snap_package': {'packages': [{'name': SNAP}], 'state': 'absent'}}])
            assert task(run('run'))['change'] == 'changed' and tracking() == ''
            assert task(run('run'))['change'] == 'unchanged'
            mark('snap_package switches a snap to another channel and removes it')
        finally:
            remote(f'snap remove {SNAP} >/dev/null 2>&1 || true')
    (workspace / 'summary.json').write_text(json.dumps(passed, indent=2) + '\n')
    print(f'{len(passed)} snap workflows passed', flush=True)


if __name__ == '__main__':
    vm.run_suite(main)
