#!/usr/bin/env python3
"""The controller machine as a node: a controller process on a guest manages
that guest as an ordinary node, and never reboots it.

The guest gets the mmx program, a project, and an SSH key of its own for its
administrative account, as a Linux controller machine has them. Every mmx
command runs there, as that account and not as root. The second guest is
another machine, which the same fleet stage reboots.
"""
from contextlib import ExitStack
import json
import os
import shlex
import shutil
import subprocess
import time

import vm

# The project directory on the controller machine; it also holds mmx itself.
PROJECT = '/var/tmp/mmx-controller-machine-as-node'
# The comment of the SSH key that the suite makes on the controller machine.
# The suite removes the key's lines from authorized_keys by this comment.
KEY_COMMENT = 'mmx-test-controller-machine-as-node'
MARKERS = '/var/tmp/mmx-integration'
REFUSAL = ('MMX does not reboot the controller machine, the machine where this mmx command runs. '
           'Run the playbook from another machine, or reboot this machine by hand after MMX finishes.')
BOOT_ID = 'cat /proc/sys/kernel/random/boot_id'


def main():
    machine, other = vm.NAME, 'mmx-peer'
    started = marked = time.monotonic()
    passed = []

    def mark(message):
        # The seconds since the last mark say where the suite's time goes.
        nonlocal marked
        now = time.monotonic()
        passed.append(message)
        print(f'PASS {message} ({now - marked:.0f} s)', flush=True)
        marked = now

    for name in (machine, other):
        vm.up(name)
        vm.reset(name)
    architecture = vm.architecture(machine)
    directory = vm.ROOT / 'controller-machine-as-node'
    shutil.rmtree(directory, ignore_errors=True)
    directory.mkdir(mode=0o700)
    vm.build()
    controller = directory / 'mmx'
    subprocess.run(['go', 'build', '-trimpath', vm.release_version_flag(), '-o', str(controller), './cmd/mmx'], cwd=vm.REPO,
                   env=dict(os.environ, CGO_ENABLED='0', GOOS='linux', GOARCH=architecture), check=True)
    nodes, sessions = {}, {}
    for name in (machine, other):
        nodes[name], sessions[name] = vm.connection(name)
    account = nodes[machine]['ssh_user']
    assert account != 'root' and nodes[other]['ssh_user'] == account, (account, nodes[other]['ssh_user'])

    def as_account(script, name=machine, check=True, timeout=120):
        """Run a shell script on a guest as its administrative account, not as root."""
        return vm.cmd([*sessions[name], script], check=check, timeout=timeout)

    def as_root(script, name=machine):
        return vm.console(script, name)

    def mmx(command, *arguments, expected=0, timeout=120, said=None):
        """Run an mmx command on the controller machine, in the project;
        return its JSON. said, when set, is a line the command must print."""
        p = as_account('cd ' + PROJECT + ' && ./mmx ' + shlex.join([command, *arguments, '--json']), check=False, timeout=timeout)
        assert p.returncode == expected, f'mmx {command} {arguments}: exit code {p.returncode}, expected {expected}\n{p.stdout}\n{p.stderr}'
        assert said is None or said in p.stderr, (said, p.stderr)
        return json.loads(p.stdout)

    def entries(record):
        return {e['node']: e for e in record['entries']}

    def wait_until(script, name=machine, seconds=30):
        """Wait until a check by root on a guest succeeds."""
        deadline = time.monotonic() + seconds
        while vm.guest('sudo', '-n', '/bin/sh', '-c', script, name=name, check=False).returncode != 0:
            assert time.monotonic() < deadline, 'timed out: ' + script
            time.sleep(0.1)

    def logins(name):
        """What tells whether the suite left the account's SSH logins as they were."""
        return as_account("sha256sum < ~/.ssh/authorized_keys; stat -c '%U %a' ~/.ssh/authorized_keys", name).stdout

    logins_before = {name: logins(name) for name in (machine, other)}
    with ExitStack() as cleanup:
        # The guests are as before when the suite ends, also after a failure.
        remove_key = f"sed -i '/ {KEY_COMMENT}$/d' ~/.ssh/authorized_keys"
        cleanup.callback(as_account, f'rm -rf {PROJECT} /tmp/mmx-ssh-*; {remove_key}', check=False)
        cleanup.callback(lambda: as_account(remove_key, other, check=False))
        as_account(f'rm -rf {PROJECT} && mkdir -m 700 {PROJECT} && cd {PROJECT} && mkdir inventories playbooks && '
                   f"ssh-keygen -q -t ed25519 -N '' -C {KEY_COMMENT} -f administrator_key")
        vm.copy(controller, PROJECT + '/mmx', machine)
        runner = vm.ROOT / ('bin/mmx-runner-linux-' + architecture)
        vm.copy(runner, PROJECT + '/' + runner.name, machine)
        public_key = as_account(f'cat {PROJECT}/administrator_key.pub').stdout.strip()
        assert public_key.endswith(' ' + KEY_COMMENT), public_key
        for name in (machine, other):
            as_account(f"printf '%s\\n' {shlex.quote(public_key)} >> ~/.ssh/authorized_keys", name)

        # The three ways to this machine: 127.0.0.1, its host name, and the
        # address of its network interface.
        host_name = as_account('hostname').stdout.strip()
        other_address = nodes[machine]['address']
        resolved = as_account('getent ahostsv4 ' + shlex.quote(host_name)).stdout.split()[0]
        assert len({'127.0.0.1', host_name, other_address}) == 3, (host_name, other_address)
        host_keys = {name: ' '.join(as_root('cat /etc/ssh/ssh_host_ed25519_key.pub', name).split()[:2]) for name in (machine, other)}
        known_hosts = directory / 'known_hosts'
        known_hosts.write_text(f'127.0.0.1,{host_name},{other_address} {host_keys[machine]}\n'
                               f'{nodes[other]["address"]} {host_keys[other]}\n')
        vm.copy(known_hosts, PROJECT + '/known_hosts', machine)

        def node(address, **fields):
            return {'address': address, 'ssh_user': account, 'ssh_identity_file': PROJECT + '/administrator_key',
                    'ssh_known_hosts': PROJECT + '/known_hosts', 'ssh_config': '/dev/null', **fields}

        inventories = {
            'through_ssh': {'controller_machine': node('127.0.0.1', connection='ssh_only')},
            'through_https': {'controller_machine': node('127.0.0.1')},
            'both_servers': {'controller_machine': node('127.0.0.1'), 'other_server': node(nodes[other]['address'])},
            'by_host_name': {'controller_machine': node(host_name, connection='ssh_only')},
            'by_other_address': {'controller_machine': node(other_address)},
        }
        patch_and_reboot = {'id': 'patch_and_reboot', 'nodes': 'all', 'node_tasks': [
            {'id': 'patch', 'file': {"permissions_mode": "0644", 'path': MARKERS + '/patched', 'content': 'patched\n'}},
            {'id': 'restart', 'reboot': {'wait_at_most': '3m'}}]}
        playbooks = {
            'configure': [{'id': 'configure', 'nodes': 'controller_machine', 'node_tasks': [
                {'id': 'write', 'file': {"permissions_mode": "0644", 'path': MARKERS + '/managed', 'content': 'managed by the controller process on this machine\n'}}]}],
            # The node job waits until the suite lets it go on.
            'wait_for_release': [{'id': 'wait_for_release', 'nodes': 'controller_machine', 'node_tasks': [
                {'id': 'wait', 'shell': {'script': f'echo started >> {MARKERS}/started; '
                                                   f'while ! test -e {MARKERS}/release; do sleep 0.1; done; '
                                                   f'echo completed >> {MARKERS}/completed'}}]}],
            'patch': [patch_and_reboot],
            'patch_except_the_controller_machine': [dict(patch_and_reboot, except_nodes='controller_machine')],
        }
        project = directory / 'project'
        vm.write_settings(project, {'project_name': 'controller_machine_as_node', 'default_inventory': 'through_https'})
        for name, fleet_nodes in inventories.items():
            vm.write_inventory(project, {'nodes': fleet_nodes}, name=name)
        for name, fleet_stages in playbooks.items():
            vm.write_playbook(project, name, fleet_stages)
        for path in sorted(p for p in project.rglob('*') if p.is_file()):
            vm.copy(path, PROJECT + '/' + str(path.relative_to(project)), machine)
        mark('the controller machine has mmx, a project, and an SSH key of its own for its administrative account')

        for inventory, connection_used in (('through_ssh', 'ssh'), ('through_https', 'https')):
            as_root(f'rm -f {MARKERS}/managed {MARKERS}/started {MARKERS}/release {MARKERS}/completed')
            # Until a run sets up MMX's HTTPS service, a dryrun uses SSH.
            preview = mmx('dryrun', 'configure', '--inventory', inventory, expected=2)['entries'][0]
            assert preview['connection_used'] == 'ssh' and preview['response']['change'] == 'would_change', preview
            as_root(f'test ! -e {MARKERS}/managed')
            notice = vm.https_setup_notice('controller_machine', inventory) if connection_used == 'https' else None
            applied = mmx('run', 'configure', '--inventory', inventory, said=notice)['entries'][0]
            assert applied['connection_used'] == connection_used and applied['response']['change'] == 'changed', applied
            # The node runner executable wrote the file as root, and the
            # controller process ran as the administrative account.
            assert as_root(f"cat {MARKERS}/managed; stat -c '%U' {MARKERS}/managed {PROJECT}/.mmx") == \
                f'managed by the controller process on this machine\nroot\n{account}\n'
            again = mmx('run', 'configure', '--inventory', inventory)
            assert again['status'] == 'succeeded' and again['entries'][0]['response']['change'] == 'unchanged', again
            assert again['entries'][0]['connection_used'] == connection_used
            preview = mmx('dryrun', 'configure', '--inventory', inventory)['entries'][0]
            assert preview['connection_used'] == connection_used and preview['response']['change'] == 'unchanged', preview

            # The controller process is killed while the node job runs. The
            # node job is another process of the same machine and goes on.
            as_account(f'cd {PROJECT} && (nohup ./mmx run wait_for_release --inventory {inventory} --json '
                       '> interrupted.stdout 2> interrupted.stderr < /dev/null & echo $! > interrupted.pid)')
            try:
                wait_until(f'test -s {MARKERS}/started')
            except AssertionError:
                raise AssertionError(as_account(f'cd {PROJECT} && cat interrupted.stdout interrupted.stderr', check=False).stdout)
            as_account(f'cd {PROJECT} && kill -KILL "$(cat interrupted.pid)" && '
                       'while kill -0 "$(cat interrupted.pid)" 2>/dev/null; do sleep 0.05; done')
            as_root(f'test ! -e {MARKERS}/completed && touch {MARKERS}/release')
            wait_until(f'test -s {MARKERS}/completed')
            resumed = mmx('resume', '--inventory', inventory)
            entry = resumed['entries'][0]
            assert resumed['status'] == 'succeeded' and entry['node_job_attempt'] == 1 and entry['response']['status'] == 'succeeded', resumed
            assert as_root(f'cat {MARKERS}/started {MARKERS}/completed') == 'started\ncompleted\n'
            mark(f'through {connection_used} to 127.0.0.1: a dryrun over SSH, a first run, an unchanged second run and dryrun; '
                 'after the controller process is killed, the node job finishes and mmx resume takes its result without running it again')

        # The controller's state and the node's state are apart on the one machine.
        state = as_root(f"stat -c '%U %a' /var/lib/mmx; systemctl is-active mmx-https-api.service; "
                        f"ls {PROJECT}/.mmx/runs | wc -l; ls /var/lib/mmx/jobs | wc -l").split('\n')
        assert state[0] == 'root 700' and state[1] == 'active' and int(state[2]) >= 8 and int(state[3]) >= 8, state
        mark("the run history is in the project's .mmx, which the administrative account owns; "
             "the node's state is in /var/lib/mmx, which root owns; the node HTTPS API runs on the machine")

        # The guard. The node task before the reboot task must never run.
        boot = {name: as_root(BOOT_ID, name).strip() for name in (machine, other)}

        def refused(record, attempt=1):
            """The controller machine's node job failed with the refusal,
            before anything of it reached the node."""
            entry = entries(record)['controller_machine']
            response = entry['response']
            assert response['status'] == 'failed' and response['error'] == REFUSAL and not response.get('results'), entry
            assert entry['node_job_attempt'] == attempt and len(entry.get('previous_node_job_attempts', [])) == attempt - 1, entry
            assert as_root(f"test ! -e {MARKERS}/patched && test ! -e /var/lib/mmx/jobs/{entry['job_id']} && {BOOT_ID}").strip() == boot[machine]
            return entry

        for inventory, route, connection_used in (('by_host_name', f'its host name ({host_name}, which is {resolved})', 'ssh'),
                                                  ('by_other_address', f"its network interface's address ({other_address})", 'https')):
            for command in ('dryrun', 'run'):
                record = mmx(command, 'patch', '--inventory', inventory, expected=1)
                assert refused(record)['connection_used'] == connection_used and record['status'] == 'failed', record
            mark(f'reached by {route} through {connection_used}, the controller machine refuses the fleet stage that ends with a reboot, '
                 'in a dryrun and in a run, and no node task ran')

        preview = mmx('dryrun', 'patch', '--inventory', 'both_servers', expected=1)
        refused(preview)
        would = entries(preview)['other_server']['response']
        assert would['status'] == 'succeeded' and [r['change'] for r in would['results']] == ['would_change', 'would_change'], would
        assert as_root(f'test ! -e {MARKERS}/patched && {BOOT_ID}', other).strip() == boot[other]
        mark('reached as 127.0.0.1, the controller machine refuses in a dryrun, and the dryrun of the other server goes on')

        # Without --json: what a person reads.
        said = as_account(f'cd {PROJECT} && ./mmx run patch --inventory both_servers', check=False, timeout=300)
        assert said.returncode == 1 and 'patch_and_reboot / controller_machine: failed\n  ' + REFUSAL + '\n' in said.stdout and \
            'patch_and_reboot / other_server: succeeded\n' in said.stdout, (said.returncode, said.stdout, said.stderr)
        nodes[other], sessions[other] = vm.await_boot(boot[other], other)
        record = mmx('results', '--inventory', 'both_servers')
        refused(record)
        rebooted = entries(record)['other_server']['response']
        new_boot = as_root(BOOT_ID, other).strip()
        assert new_boot != boot[other] and rebooted['status'] == 'succeeded', rebooted
        assert rebooted['results'][-1]['value'] == {'boot_id_before': boot[other], 'boot_id_after': new_boot}, rebooted
        assert as_root(f'cat {MARKERS}/patched', other) == 'patched\n'
        mark('in a run of the same fleet stage the controller machine refuses and stays up, and the other server, another machine, '
             'is patched and rebooted')

        resumed = mmx('resume', '--inventory', 'both_servers', expected=1)
        refused(resumed, attempt=2)
        assert entries(resumed)['other_server']['node_job_attempt'] == 1 and as_root(BOOT_ID, other).strip() == new_boot, resumed
        mark('mmx resume meets the refusal again and does not reboot the other server again')

        # The rule on a real node, with the node's own readings: the node
        # runner executable of the other server is asked directly, as the
        # controller process asks it over SSH.
        def asked(boot_id, machine_id_hash):
            question = json.dumps({'controller_machine': {'boot_id': boot_id, 'machine_id_hash': machine_id_hash}})
            return json.loads(as_root(f"printf '%s' {shlex.quote(question)} | /opt/mmx/bin/*/mmx-runner reboot-refusal", other))['refusal']
        machine_id_hash = as_root("tr -d '\\n' < /etc/machine-id | sha256sum | cut -d' ' -f1", other).strip()
        is_container = vm.guest('systemd-detect-virt', '--container', '--quiet', name=other, check=False).returncode == 0
        assert asked(new_boot, machine_id_hash) == REFUSAL
        assert asked(boot[other], machine_id_hash) == ''
        # A controller machine with this boot ID and another machine ID, or
        # none: a node that is a container may reboot, a server refuses.
        expected = '' if is_container else REFUSAL
        assert asked(new_boot, 'f' * 64) == expected and asked(new_boot, '') == expected, (is_container, asked(new_boot, ''))
        mark('asked directly, the node refuses for its own boot ID and machine ID, not for another boot ID, and for its own boot ID '
             'with another machine ID ' + ('it may reboot, because it is a container' if is_container else 'it refuses, because it is no container'))

        excepted = mmx('dryrun', 'patch_except_the_controller_machine', '--inventory', 'both_servers', expected=2)
        assert list(entries(excepted)) == ['other_server'] and excepted['status'] == 'succeeded', excepted
        mark('with except_nodes the fleet stage leaves the controller machine out, and nothing fails')

    assert {name: logins(name) for name in (machine, other)} == logins_before
    as_account(f'test ! -e {PROJECT} && ! ls -d /tmp/mmx-ssh-* 2>/dev/null')
    mark("the suite's SSH key, its lines in authorized_keys on both guests, and the project are gone")

    (directory / 'results.json').write_text(json.dumps({'passed': passed}, indent=2))
    print(f'{len(passed)} checks of the controller machine as a node passed in {time.monotonic() - started:.0f} s; records: {directory}', flush=True)


if __name__ == '__main__':
    vm.run_suite(main)
