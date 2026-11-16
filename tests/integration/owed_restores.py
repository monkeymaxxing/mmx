#!/usr/bin/env python3
"""Real systemd, connection loss, worker death, and reboot for one owed restore.

Only this suite builds with mmx_owed_restore_test. Nothing changes vm.build()
or the production binaries shared by all the other suites.
"""
import contextlib
from datetime import datetime
import json
import os
from pathlib import Path
import re
import shlex
import shutil
import signal
import subprocess
import time

import vm

MODULE = 'test_change_with_owed_restore'
TAG = 'mmx_owed_restore_test'
FIXTURES = '/var/tmp/mmx-integration/owed-restores'
# These are the six catalog targets. Unit aliases and active sockets are
# inspected on the actual guest; a distro name never substitutes for that check.
SSH_SERVICE_NAMES = {
    'debian12': 'ssh.service', 'debian13': 'ssh.service',
    'ubuntu2404': 'ssh.service', 'ubuntu2604': 'ssh.service',
    'rocky9': 'sshd.service', 'almalinux10': 'sshd.service', 'fedora45': 'sshd.service',
}


def build(directory, architecture):
    """A separate, suite-only build, shared across this run's distributions."""
    with vm.shared_build() as shared:
        binaries = (shared if shared else directory) / 'owed-restore-test-binaries'
        binaries.mkdir(parents=True, exist_ok=True)
        targets = [('mmx', './cmd/mmx', [])]
        targets += [(f'mmx-runner-linux-{architecture}', './cmd/mmx-runner',
                     ['GOOS=linux', f'GOARCH={architecture}'])]
        for filename, package, environment in targets:
            output = binaries / filename
            if shared is None or not output.exists():
                vm.cmd(['env', 'CGO_ENABLED=0', *environment, 'go', 'build',
                        '-trimpath', '-tags', TAG, '-o', output, package], cwd=vm.REPO)
    vm.ENV['MMX_RUNNER_DIR'] = str(binaries)
    return binaries


def properties(text):
    return dict(line.split('=', 1) for line in text.splitlines() if '=' in line)


def instant(text):
    return datetime.fromisoformat(text.replace('Z', '+00:00')).timestamp()


class Suite:
    def __init__(self):
        self.workspace = vm.ROOT / 'owed-restores'
        shutil.rmtree(self.workspace, ignore_errors=True)
        self.workspace.mkdir(parents=True, exist_ok=True)
        self.node, self.ssh = vm.connection()
        self.binaries = build(self.workspace, vm.architecture())
        self.mmx = self.binaries / 'mmx'
        self.processes = []
        self.case_name = 'setup'
        self.case_directory = self.workspace
        self.destination = ''
        self.last_record = None
        self.last_output = ''
        self.ssh_units = []
        self.passed = []

    def remote(self, script, check=True, timeout=15):
        result = vm.cmd([*self.ssh, 'sudo -n /bin/sh -c ' + shlex.quote('set -eu\n' + script)],
                        check=False, timeout=timeout)
        if check:
            self.require(result.returncode == 0, f'node command exits 0: {script}',
                         (result.returncode, result.stdout, result.stderr))
        return result

    def emergency(self, script):
        """Diagnostics/cleanup can still use Incus when SSH itself is broken."""
        target = vm.target()
        if target.get('incus_instance'):
            return vm.cmd(['incus', 'exec', target['incus_instance'], '--',
                           '/bin/sh', '-c', script], check=False, timeout=20)
        return self.remote(script, check=False, timeout=20)

    def require(self, condition, expected, observed=None):
        if not condition:
            raise AssertionError(f'case {self.case_name}: expected {expected}; node showed {observed!r}')

    def wait(self, expected, read, satisfied, seconds=30, interval=0.1):
        deadline = time.monotonic() + seconds
        observed = None
        while time.monotonic() < deadline:
            observed = read()
            if satisfied(observed):
                return observed
            time.sleep(interval)
        self.require(False, expected, observed)

    def record(self):
        if not self.destination:
            return None
        text = self.remote('for file in /var/lib/mmx/owed_restores/*/owed_restore.json; do\n'
                           f'  if test -f "$file" && grep -Fq {shlex.quote(self.destination)} "$file"; then cat "$file"; fi\n'
                           'done').stdout
        if text.strip():
            self.last_record = json.loads(text)
            (self.case_directory / 'last-observed-owed-restore.json').write_text(text)
            return self.last_record
        return None

    def diagnostics(self):
        # Keep the printed report inside the catalog's last-30-lines window.
        # JSON stays on one line, even when the controller pretty-printed it.
        record = self.last_record
        sections = [f'CASE {self.case_name}', 'LAST CONTROLLER OUTPUT ' + json.dumps(self.last_output[-6000:]),
                    'LAST OBSERVED RECORD ' + json.dumps(record)]
        change = record['owed_restore']['owed_restore_id'] if record else None
        units = ([f'mmx-owed-restore-{change}.timer', f'mmx-owed-restore-{change}.service',
                  f'mmx-activate-change-{change}.service'] if change else [])
        try:
            current = self.emergency('for file in /var/lib/mmx/owed_restores/*/owed_restore.json; do\n'
                                     f'if test -f "$file" && grep -Fq {shlex.quote(self.destination)} "$file"; then cat "$file"; fi\ndone')
            if current.stdout.strip():
                record = json.loads(current.stdout)
                sections.append('CURRENT RECORD ' + json.dumps(record))
                change = record['owed_restore']['owed_restore_id']
                units = [f'mmx-owed-restore-{change}.timer', f'mmx-owed-restore-{change}.service',
                         f'mmx-activate-change-{change}.service']
            else:
                sections.append('CURRENT RECORD absent; ' + json.dumps(current.stderr))
        except Exception as error:
            sections.append('CURRENT RECORD UNAVAILABLE ' + json.dumps(str(error)))
        script = ''
        for unit in [*units, *self.ssh_units, 'mmx-https-api.service', 'network.target']:
            script += f'printf "%s " {shlex.quote(unit)}; systemctl show {shlex.quote(unit)} -p LoadState -p ActiveState -p SubState -p Result -p MainPID -p ControlGroup -p NextElapseUSecRealtime -p After | tr "\\n" " "; echo\n'
        services = units[1:] if units else ['mmx-owed-restore-*', 'mmx-activate-change-*']
        for unit in services:
            script += f'echo {shlex.quote("LAST JOURNAL LINES " + unit)}; journalctl --no-pager -o short-precise -n 6 -u {shlex.quote(unit)} 2>&1 | tail -n 6\n'
        try:
            output = self.emergency(script)
            sections.append(output.stdout + output.stderr)
        except Exception as error:
            sections.append('NODE DIAGNOSTICS UNAVAILABLE ' + json.dumps(str(error)))
        text = '\n'.join(sections)
        (self.case_directory / 'failure-diagnostics.txt').write_text(text)
        print(text, flush=True)

    @contextlib.contextmanager
    def case(self, name, connection='ssh_only', existed=True):
        self.case_name = name
        self.case_directory = self.workspace / name
        self.case_directory.mkdir(parents=True, exist_ok=True)
        self.destination = f'{FIXTURES}/{name}/configuration'
        self.last_record = None
        self.last_output = ''
        try:
            self.remote(f'mkdir -p {shlex.quote(str(Path(self.destination).parent))}\n'
                        f'rm -f {shlex.quote(str(Path(self.destination).parent))}/*\n'
                        'systemctl reset-failed')
            if existed:
                self.remote(f"printf 'previous contents\\n' > {self.destination}\n"
                            f'chmod 0640 {self.destination}\n'
                            f'touch -d 2020-01-02T03:04:05Z {self.destination}')
            self.before = self.file_state()
            self.inventory(connection)
            yield
        except BaseException as error:
            print(f'FAIL case {name}: {error}', flush=True)
            self.diagnostics()
            raise
        else:
            self.passed.append(name)
            print(f'PASS case {name}', flush=True)

    def inventory(self, connection):
        vm.write_inventory(self.case_directory, {'nodes': {'web01': dict(self.node, connection=connection)}}, name='example')

    def project(self, activation='succeed', duration='8s', before=None, tasks=None):
        tasks = tasks if tasks is not None else [*(before or []), {MODULE: {
            'path': self.destination, 'content': 'proposed contents\n',
            'activation': activation, 'confirm_reachability_within': duration}}]
        vm.write_project(self.case_directory, project_name='owed_restore_' + self.case_name.replace('-', '_'),
                         fleet_stages=[{'id': 'network', 'nodes': 'all', 'node_tasks': tasks}])

    def command(self, command, *args, expected=0, human=False, environment=None):
        arguments = [self.mmx, command, *vm.playbook_args(command), *args]
        if not human:
            arguments += ['--json']
        if environment is None:
            result = vm.cmd(arguments, cwd=self.case_directory, check=False, timeout=60)
        else:
            result = subprocess.run(list(map(str, arguments)), cwd=self.case_directory, env=environment,
                                    text=True, capture_output=True, timeout=60)
        self.last_output = result.stdout + '\n' + result.stderr
        self.require(result.returncode in (expected if isinstance(expected, tuple) else (expected,)),
                     f'{command} exit {expected}', (result.returncode, self.last_output))
        return result.stdout if human else json.loads(result.stdout)

    def background(self):
        process = subprocess.Popen([str(self.mmx), 'run', 'configure', '--json'], cwd=self.case_directory,
                                   env=vm.ENV, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True,
                                   start_new_session=True)
        self.processes.append(process)
        return process

    def stop_controller_and_drain_requests(self, process):
        def group_members():
            rows = vm.cmd(['ps', '-axww', '-o', 'pid=,pgid=,stat=,args=']).stdout.splitlines()
            return [fields for line in rows if len(fields := line.split(None, 3)) == 4
                    and fields[1] == str(process.pid) and not fields[2].startswith('Z')]

        # Freeze our own group before inspecting it: the controller cannot issue
        # another request while we find its active SSH control connections.
        os.killpg(process.pid, signal.SIGSTOP)
        commands = [member[3] for member in group_members()]
        control_paths = set(re.findall(r'ControlPath=(/tmp/mmx-ssh-[^\s/]+/control)', '\n'.join(commands)))
        os.killpg(process.pid, signal.SIGKILL)
        process.communicate(timeout=5)
        self.wait('controller and local SSH request processes have exited', group_members,
                  lambda members: not members, seconds=3)
        # OpenSSH can detach its shared master from the process group. Close
        # only sockets named by this controller's stopped request processes.
        for path in control_paths:
            vm.cmd(['ssh', '-F', '/dev/null', '-S', path, '-O', 'exit', self.node['address']],
                   check=False, timeout=5)
            closed = vm.cmd(['ssh', '-F', '/dev/null', '-S', path, '-O', 'check', self.node['address']],
                            check=False, timeout=5)
            self.require(closed.returncode != 0, f'controller SSH master at {path} has stopped', closed.stderr)
        # These cases use ssh_only. Stop any API left by an earlier case so
        # neither a surviving HTTPS handler nor an SSH request can inspect the
        # dead worker. Let admitted SSH requests finish while it is still alive.
        self.remote('systemctl stop mmx-https-api.service')
        self.wait('all controller request processes on the node have drained', self.node_request_processes,
                  lambda requests: not requests, seconds=6)

    def node_request_processes(self):
        return self.remote(r'''for executable in /proc/[0-9]*/exe; do
 program=$(readlink "$executable") || continue
 case "$program" in /opt/mmx/bin/*/mmx-runner) ;; *) continue ;; esac
 command_file="${executable%/exe}/cmdline"
 command=$(tr '\0' '\n' < "$command_file" 2>/dev/null | sed -n '2p') || continue
 case "$command" in worker|activate-change) continue ;; esac
 printf '%s ' "${executable%/exe}"; tr '\0' ' ' < "$command_file" 2>/dev/null || true
 echo
done''').stdout

    def node_time(self):
        return float(self.remote('date +%s.%N').stdout)

    def collect(self, process, expected):
        out, err = process.communicate(timeout=45)
        self.last_output = out + '\n' + err
        self.require(process.returncode == expected, f'controller exit {expected}', (process.returncode, self.last_output))
        return json.loads(out)

    def file_state(self):
        return self.remote(f'if test -f {self.destination}; then sha256sum {self.destination}; '
                           f"stat -c '%a %u %g %y' {self.destination}; else echo absent; fi").stdout

    def restored(self, result, task=True):
        response = result['entries'][0]['response']
        self.require(response['status'] == 'failed', 'failed node job after restore', response)
        restores = response.get('owed_restores', [])
        self.require(len(restores) == 1 and restores[0]['restore_status'] == 'succeeded',
                     'one successful restore in the response', response)
        if task:
            outcome = response['results'][-1]
            self.require(outcome['status'] == 'failed' and outcome['change'] == 'changed',
                         'failed and changed node task', outcome)
        self.require(self.file_state() == self.before, 'previous file bytes and metadata, or its absence', self.file_state())
        self.require(self.remote(f'test -f {Path(self.destination).parent}/previous_config_activated', check=False).returncode == 0,
                     'previous configuration activation checked in a new process')
        return restores[0]

    def clean_change(self):
        record = self.wait('resolved record with completed cleanup', self.record,
                           lambda r: r and r.get('owed_restore_cleanup_complete'))
        change = record['owed_restore']['owed_restore_id']
        paths = [f'/etc/systemd/system/mmx-owed-restore-{change}.timer',
                 f'/etc/systemd/system/mmx-owed-restore-{change}.service',
                 f'/etc/systemd/system/mmx-activate-change-{change}.service',
                 f'/etc/systemd/system/timers.target.wants/mmx-owed-restore-{change}.timer',
                 f'/etc/systemd/system/multi-user.target.wants/mmx-owed-restore-{change}.service',
                 f'/var/lib/mmx/owed_restores/{change}/previous_file_contents']
        for path in paths:
            self.require(self.remote(f'test ! -e {path} && test ! -L {path}', check=False).returncode == 0,
                         f'cleanup removed {path}')
        return record

    def fresh_ssh(self):
        args = vm.unshared(self.ssh[:-1]) + ['-o', 'ControlMaster=no', '-o', 'ControlPath=none',
                                           '-o', 'ControlPersist=no', self.ssh[-1], 'true']
        return vm.cmd(args, check=False, timeout=8)

    def setup_listeners(self):
        for unit in ['ssh.socket', 'sshd.socket', 'ssh.service', 'sshd.service']:
            result = self.remote(f'systemctl show {unit} -p Id -p LoadState -p ActiveState -p KillMode', check=False)
            observed = properties(result.stdout)
            print(f'SSH listener inspection {unit}: {observed}', flush=True)
            if observed.get('ActiveState') != 'active':
                continue
            if unit.endswith('.service'):
                self.require(observed.get('KillMode') == 'process', f'{unit} preserves existing sessions with KillMode=process', observed)
            if observed['Id'] not in self.ssh_units:
                self.ssh_units.append(observed['Id'])
        self.require(self.ssh_units, 'an active SSH listener service or socket')
        distribution = ''
        target = vm.target()
        if target.get('incus_instance'):
            import incus
            distribution = next((name for name, image in incus.IMAGES.items() if image == target.get('image')), '')
        if distribution in SSH_SERVICE_NAMES:
            unit = SSH_SERVICE_NAMES[distribution]
            self.require(self.remote(f'systemctl show {unit} -p LoadState --value').stdout.strip() == 'loaded',
                         f'{distribution} supplies {unit}')

    def listener_cleanup(self):
        return '\n'.join(f'systemctl start {unit}' for unit in self.ssh_units)

    def run(self):
        self.setup_listeners()
        for mode in ('ssh_only', 'https_with_ssh_fallback'):
            with self.case('accepted-' + ('ssh' if mode == 'ssh_only' else 'https'), mode):
                self.project()
                result = self.command('run')
                response = result['entries'][0]['response']
                self.require(response['status'] == 'succeeded' and response['results'][-1]['change'] == 'changed',
                             'confirmed file change succeeded and changed', result)
                self.require(self.remote(f'cat {self.destination}').stdout == 'proposed contents\n', 'proposed file installed')
                record = self.clean_change()
                kinds = ['ssh'] if mode == 'ssh_only' else ['https', 'ssh']
                self.require(sorted(record['after_change_confirmed_connection_kinds']) == kinds,
                             f'after-change confirmation over {kinds}', record)
                # Include rounding to the next second and AccuracySec=1s so
                # the erroneous timer has had its scheduled chance to fire.
                self.wait('original restore deadline and timer accuracy allowance have passed on the node', self.node_time,
                          lambda now: now > instant(record['restore_deadline']) + 2, interval=0.5)
                record = self.record()
                self.require(record and record['change_phase'] == 'change_accepted',
                             'change remains accepted after its original restore deadline', record)
                contents = self.remote(f'cat {self.destination}').stdout
                self.require(contents == 'proposed contents\n', 'proposed contents remain after the deadline', contents)
                marker = self.remote(f'test -e {Path(self.destination).parent}/previous_config_activated', check=False)
                self.require(marker.returncode == 1, 'previous configuration was never activated after acceptance', marker.returncode)
                # The current product keeps the resolved receipt until history
                # expires. Verify its eventual deletion through that real path.
                job = result['entries'][0]['job_id']
                self.remote(f"touch -d '4 days ago' /var/lib/mmx/jobs/{job}/result.json")
                self.project(tasks=[{'show': 'expire the completed test job'}])
                self.command('run')
                self.require(self.record() is None, 'resolved record removed with expired job history', self.last_record)

        with self.case('restore-after-ssh-listener-cut-with-live-worker'):
            self.project('stop_ssh_listener')
            process = self.background()
            self.wait('after-change wait with SSH stopped', self.record, lambda r: r and r['change_phase'] == 'waiting_after_change')
            # record() arrived through the already established harness session.
            fresh = self.fresh_ssh()
            self.require(fresh.returncode != 0, 'new SSH refused while the existing inspection session survives', fresh.stdout + fresh.stderr)
            result = self.collect(process, 1)
            self.restored(result)
            record = self.clean_change()
            self.require(not record['after_change_confirmed_connection_kinds'], 'no after-change SSH confirmation', record)
            self.require(self.fresh_ssh().returncode == 0, 'SSH listener restored')
            text = self.command('status', result['id'], human=True)
            self.require(f'{self.destination} was restored to its previous state' in text, 'human restored-state sentence', text)

        with self.case('restore-after-confirmed-https-cut-with-ssh-working-and-live-worker', 'https_with_ssh_fallback', existed=False):
            self.project('stop_https_listener')
            process = self.background()
            record = self.wait('after-change wait with HTTPS stopped', self.record, lambda r: r and r['change_phase'] == 'waiting_after_change')
            self.require(sorted(record['before_change_confirmed_connection_kinds']) == ['https', 'ssh'], 'both kinds confirmed before change', record)
            self.require(self.remote('systemctl is-active mmx-https-api.service', check=False).stdout.strip() == 'inactive', 'HTTPS API stopped')
            self.require(self.fresh_ssh().returncode == 0, 'new SSH still works while HTTPS is cut')
            result = self.collect(process, 1)
            self.restored(result)
            record = self.clean_change()
            self.require('https' not in (record['after_change_confirmed_connection_kinds'] or []), 'no after-change HTTPS confirmation', record)
            self.require(self.remote('systemctl is-active mmx-https-api.service').stdout.strip() == 'active', 'HTTPS API restored')

        with self.case('activation-failed'):
            self.project('fail', '25s')
            result = self.command('run', expected=1)
            self.restored(result)
            record = self.clean_change()
            started = json.loads(self.remote(f'cat {Path(self.destination).parent}/owed_restore_started.json').stdout)
            self.require(instant(started['node_time']) < instant(record['restore_deadline']) - 5,
                         'failed activation restored immediately, at least five seconds before the deadline', (started, record))

        with self.case('activation-hung'):
            self.project('hang')
            process = self.background()
            self.wait('stubborn activation child started', lambda: self.remote(f'test -f {Path(self.destination).parent}/change_activation_child_process_id', check=False).returncode, lambda code: code == 0)
            child = int(self.remote(f'cat {Path(self.destination).parent}/change_activation_child_process_id').stdout)
            ignored = self.remote(f"awk '/^SigIgn:/ {{print $2}}' /proc/{child}/status").stdout.strip()
            self.require(int(ignored, 16) & (1 << 14), 'activation child ignores SIGTERM', ignored)
            self.restored(self.collect(process, 1))
            record = self.clean_change()
            started = json.loads(self.remote(f'cat {Path(self.destination).parent}/owed_restore_started.json').stdout)
            self.require(instant(started['node_time']) >= instant(record['restore_deadline']), 'deadline starts recovery of hanging activation', (started, record))
            # Construction of the restore adapter refused to proceed unless
            # BOTH exact PIDs were dead, before the file could be restored.
            self.require(started['network_target_state'] == 'active', 'network.target active during restore', started)

        for restart in (False, True):
            with self.case('restore-on-new-boot-before-deadline' if restart else 'timer-alone-after-worker-death'):
                self.project('wait_for_release', '2m' if restart else '15s')
                process = self.background()
                self.wait('activation gate reached', lambda: self.remote(f'test -f {Path(self.destination).parent}/change_activation_started', check=False).returncode, lambda code: code == 0)
                # Lose the controller before releasing activation, so it cannot
                # race this case by confirming the after-change phase.
                self.stop_controller_and_drain_requests(process)
                saved = self.command('results')
                self.remote(f'touch {Path(self.destination).parent}/release_change_activation')
                record = self.wait('after-change wait before interruption', self.record, lambda r: r and r['change_phase'] == 'waiting_after_change')
                service = record['owed_restore']['restore_service_unit']
                state = self.remote(f'systemctl is-active {service}', check=False).stdout.strip()
                self.require(state == 'inactive', 'restore service has not started before interruption', state)
                if restart:
                    old_boot = record['node_boot_id_before_change']
                    after = self.remote(f'systemctl show {service} -p After --value').stdout.split()
                    self.require('network.target' in after, 'boot restore ordered after network.target', after)
                    vm.reboot()
                    self.node, self.ssh = vm.connection()
                    self.inventory('ssh_only')
                    now = self.node_time()
                    self.require(now < instant(record['restore_deadline']),
                                 'reboot and observation finish before the restore deadline; reboot consumed the allowance',
                                 {'node_time': now, 'restore_deadline': record['restore_deadline']})
                    started = json.loads(self.remote(f'cat {Path(self.destination).parent}/owed_restore_started.json').stdout)
                    self.require(started['node_boot_id'] != old_boot and started['network_target_state'] == 'active', 'restore on the new boot after network.target', started)
                    self.require(instant(started['node_time']) < instant(record['restore_deadline']),
                                 'boot recovery starts before the original restore deadline', (started, record))
                else:
                    pid = int(self.remote(f'cat {Path(self.destination).parent}/node_job_worker_process_id').stdout)
                    command = self.remote(f'tr "\\0" " " < /proc/{pid}/cmdline').stdout
                    self.require('worker ' + record['node_job_id'] in command, 'exact worker PID belongs to this node job', command)
                    requests = self.node_request_processes()
                    self.require(not requests, 'no MMX request process remains before killing the worker', requests)
                    self.remote(f'kill -KILL {pid}')
                    self.wait('killed node job worker has exited', lambda: self.remote(f'test -e /proc/{pid}/exe', check=False).returncode,
                              lambda code: code == 1, seconds=3)
                    now = self.node_time()
                    self.require(now < instant(record['restore_deadline']),
                                 'worker is dead before the timer deadline',
                                 {'node_time': now, 'restore_deadline': record['restore_deadline']})
                    # Only shell inspections follow; no MMX request is sent
                    # until the timer's service has recorded completed recovery.
                    self.wait('timer restores without worker or controller', lambda: self.remote(f'test -f {Path(self.destination).parent}/previous_config_activated', check=False).returncode, lambda code: code == 0)
                    started = json.loads(self.remote(f'cat {Path(self.destination).parent}/owed_restore_started.json').stdout)
                    self.require(instant(started['node_time']) >= instant(record['restore_deadline']),
                                 'timer alone starts restoration at or after its deadline', (started, record))
                restored = self.wait('restore is complete before any MMX status request', self.record,
                                     lambda r: r and r['owed_restore']['restore_status'] == 'succeeded')
                if restart:
                    now = self.node_time()
                    self.require(now < instant(record['restore_deadline']),
                                 'boot restore completes before the original deadline; reboot consumed the allowance',
                                 {'node_time': now, 'record': restored})
                result = self.command('status', saved['id'])
                self.restored(result, task=False)
                self.require('interrupted' in result['entries'][0]['response'].get('error', ''), 'interrupted node job remains visible', result)
                self.clean_change()
                text = self.command('status', saved['id'], human=True)
                self.require('was restored to its previous state' in text, 'status renders the interrupted job restore', text)

        with self.case('restore-failed'):
            self.remote(f'touch {Path(self.destination).parent}/fail_previous_config_activation')
            self.project('fail', '25s')
            failed = self.command('run', expected=1)
            record = self.record()
            self.require(record and record['change_phase'] == 'restore_failed', 'failed restore remains owed', record)
            change = record['owed_restore']['owed_restore_id']
            service = f'mmx-owed-restore-{change}.service'
            self.require(self.remote(f'test -f /var/lib/mmx/owed_restores/{change}/previous_file_contents', check=False).returncode == 0, 'saved previous contents retained')
            self.project(tasks=[{'file': {"permissions_mode": "0644", 'path': self.destination, 'content': 'must be refused'}}])
            refused = self.command('run', expected=1)
            reason = refused['entries'][0]['response']['results'][0].get('reason', '')
            self.require(service in reason and 'Cannot change' in reason, 'same-file refusal names restore service', refused)
            alternate = self.case_directory / 'different-runner'
            alternate.mkdir(exist_ok=True)
            name = f'mmx-runner-linux-{vm.architecture()}'
            # Appended bytes change the hash without introducing another program.
            (alternate / name).write_bytes((self.binaries / name).read_bytes() + b'\nMMX owed restore installation refusal test\n')
            rejected = self.command('run', expected=1, environment=dict(vm.ENV, MMX_RUNNER_DIR=str(alternate)))
            self.require(service in json.dumps(rejected) and 'replace' in json.dumps(rejected).lower(), 'different runner executable refused and restore service named', rejected)
            self.remote(f'rm {Path(self.destination).parent}/fail_previous_config_activation\nsystemctl start {service}')
            repaired = self.command('status', failed['id'])
            self.restored(repaired)
            text = self.command('status', failed['id'], human=True)
            self.require('was restored to its previous state' in text and 'test proposed configuration activation failed' in text, 'repaired state and original task failure visible together', text)
            self.clean_change()

        with self.case('no-before-change-connection'):
            # Warm ordinary SSH/install first. The shell and the test module
            # then share one admitted job and the same ordinary connection.
            self.project(tasks=[{'show': 'prepare the ordinary SSH connection'}])
            self.command('run')
            stops = '\n'.join(f'systemctl stop {unit}' for unit in self.ssh_units)
            self.project(before=[{'shell': {'script': stops}}])
            try:
                process = self.background()
                self.wait('before-change wait', self.record, lambda r: r and r['change_phase'] == 'waiting_before_change')
                self.require(self.fresh_ssh().returncode != 0, 'fresh SSH refused before the file change')
                failed = self.collect(process, 1)
                task = failed['entries'][0]['response']['results'][-1]
                self.require(task['status'] == 'failed' and task['change'] == 'unchanged', 'task failed without installing a file', task)
                self.require(self.file_state() == self.before, 'previous file untouched', self.file_state())
                self.clean_change()
            finally:
                self.remote(self.listener_cleanup())

        with self.case('dryrun'):
            self.project('stop_ssh_listener')
            result = self.command('dryrun', expected=(0, 2, 3))
            self.require(result['entries'][0]['response']['results'][-1]['change'] == 'would_change', 'dryrun describes proposed file change', result)
            self.require(self.record() is None and self.file_state() == self.before, 'dryrun writes no record and leaves destination unchanged', self.last_record)
            units = self.remote('find /etc/systemd/system -maxdepth 1 \\( -name "mmx-owed-restore-*" -o -name "mmx-activate-change-*" \\) -print').stdout
            self.require(not units, 'dryrun writes no units', units)
        print(json.dumps({'passed': self.passed, 'count': len(self.passed)}, indent=2), flush=True)

    def cleanup(self):
        for process in self.processes:
            if process.poll() is None:
                os.killpg(process.pid, signal.SIGKILL)
                process.communicate(timeout=5)
        # Restore guest access before resetting. Stop only units created by this
        # suite, before vm.reset removes their records and runner executables.
        script = self.listener_cleanup() + '\n'
        script += 'for unit in /etc/systemd/system/mmx-activate-change-*.service /etc/systemd/system/mmx-owed-restore-*.timer /etc/systemd/system/mmx-owed-restore-*.service; do\n'
        script += ' test ! -f "$unit" || systemctl disable --now "${unit##*/}"; done\n'
        script += 'rm -f /etc/systemd/system/mmx-activate-change-*.service /etc/systemd/system/mmx-owed-restore-*.timer /etc/systemd/system/mmx-owed-restore-*.service\nsystemctl daemon-reload\nsystemctl reset-failed\n'
        output = self.emergency(script)
        self.require(output.returncode == 0, 'suite cleanup restores listeners and removes its units', output.stdout + output.stderr)
        vm.reset()


def main():
    vm.up()
    vm.reset()
    suite = Suite()
    failure = None
    try:
        suite.run()
    except BaseException as error:
        failure = error
        if suite.case_name == 'setup':
            suite.diagnostics()
    finally:
        try:
            suite.cleanup()
        except Exception as error:
            print(f'FAIL cleanup after case {suite.case_name}: {error}', flush=True)
            if failure is None:
                suite.diagnostics()
                failure = error
    if failure is not None:
        # A Python traceback would displace the useful evidence in the
        # catalog's printed tail. The case report already holds the cause.
        print(f'FAIL case {suite.case_name}: ' + json.dumps(str(failure)), flush=True)
        raise SystemExit(1)


if __name__ == '__main__':
    vm.run_suite(main)
