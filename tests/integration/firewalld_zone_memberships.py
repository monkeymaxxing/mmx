#!/usr/bin/env python3
"""Real firewalld membership, profile ownership, and autonomous recovery.

Only Incus supplies the second card and observations during lost connectivity.
HTTPS uses the guest's address directly: the harness's SSH tunnel would bypass
its firewall and cannot prove that the firewall refuses new HTTPS connections.
"""
import contextlib
import json
import os
import select
import shlex
import shutil
import signal
import socket
import ssl
import subprocess
import time

import vm
from owed_restores import Suite as RestoreObservations, instant
from systemd_networkd_files import Suite as NetworkObservations

MODULE = 'firewalld_zone_membership'
ZONE = 'mmxmembers'
SUPPORTED = ('rocky9', 'almalinux10')
PROFILE = '/etc/NetworkManager/system-connections/mmx-zone-membership.nmconnection'
PROFILE_UUID = '49050f94-91df-4af3-90d5-f526f5e97ca2'


class Suite(NetworkObservations):
    # Reuse observation and process helpers by import, never their constructors
    # or their test adapters. This suite runs ordinary production binaries.
    def __init__(self, distribution, instance):
        self.distribution, self.instance = distribution, instance
        self.workspace = vm.ROOT / 'firewalld-zone-memberships'
        self.processes, self.ssh_units, self.passed = [], [], []
        self.case_name, self.case_directory = 'setup', self.workspace
        self.destination, self.last_output, self.last_record = '', '', None
        self.old_records = set()
        self.added_interface = False
        self.firewall_command_mode = None
        self.firewall_command_path = None
        self.original_bindings = []
        self.added_ports = []
        self.profile_created = False
        self.original_service = None
        shutil.rmtree(self.workspace, ignore_errors=True)
        self.workspace.mkdir(parents=True)
        self.original_service = self.remote(
            'if systemctl is-active --quiet firewalld; then echo active; fi; '
            'if systemctl is-enabled --quiet firewalld; then echo enabled; fi').stdout.split()
        vm.build()
        self.mmx = vm.ROOT / 'bin/mmx'
        self.connect()
        self.inventory('ssh_only')
        self.project([{'dnf_package': {'packages': [{'name': 'firewalld', 'version': 'newest_when_first_installed'}]}},
                      {'systemd_service': {'name': 'firewalld', 'start_at_boot': True, "state": "running"}}])
        self.install_firewalld()
        print('firewalld ' + self.remote('firewall-cmd --version').stdout.strip(), flush=True)
        routes = json.loads(self.remote('ip -j -4 route show default').stdout)
        self.require(routes, 'a default route on the main card', routes)
        self.main = routes[0]['dev']
        source = vm.cmd([*vm.unshared(self.ssh), "printf '%s\\n' \"$SSH_CONNECTION\""]).stdout.split()
        self.require(len(source) == 4, 'the SSH peer source address', source)
        self.controller_source = source[0]
        # Every original binding we temporarily alter is restored in cleanup.
        # The main card's zone belongs to NetworkManager; no case changes it.
        self.original_bindings.append(('source', self.controller_source, self.binding('source', self.controller_source)))
        # A default zone can differ from public. Enable new SSH and HTTPS in the
        # effective main-card zone, in both layers, before the first confirmation.
        zones = {zone for zone in self.binding('interface', self.main) if zone}
        zones.add(self.remote('firewall-cmd --get-default-zone').stdout.strip())
        for zone in sorted(zones):
            for permanent in (False, True):
                for port in (22, 7443):
                    option = '--permanent ' if permanent else ''
                    if self.remote(f'firewall-cmd {option}--zone={zone} --query-port={port}/tcp', check=False).returncode:
                        self.remote(f'firewall-cmd {option}--zone={zone} --add-port={port}/tcp')
                        self.added_ports.append((zone, permanent, port))
        before = set(self.remote('ls /sys/class/net').stdout.split())
        vm.cmd(['incus', 'config', 'device', 'add', self.instance, 'second-interface',
                'nic', 'nictype=p2p', 'mtu=1500'])
        self.added_interface = True
        names = self.wait('one added second network card',
                          lambda: set(self.remote('udevadm settle --timeout=5; ls /sys/class/net').stdout.split()) - before,
                          lambda names: len(names) == 1, seconds=10)
        self.second = names.pop()
        self.remote(f'nmcli device set {self.second} managed no')
        self.remote(f'ip link set {self.second} up')
        self.inventory('https_with_ssh_fallback')
        # The earlier SSH-only bootstrap does not install the HTTPS API.
        self.project([{'firewalld_zone': {'zone': 'public'}}])
        self.command('run')
        self.start_https()

    def install_firewalld(self):
        output = vm.cmd([self.mmx, 'run', *vm.playbook_args('run'), '--json'],
                        cwd=self.workspace, check=False, timeout=600)
        self.last_output = output.stdout + '\n' + output.stderr
        self.require(output.returncode == 0, 'firewalld package installation and service start succeed', self.last_output)

    def connect(self):
        self.node, self.ssh = vm.connection()
        # vm.connection always creates a local SSH forward, even for Incus.
        # Do not use it in either the MMX inventory or independent HTTPS checks.
        self.node['https_address'] = f'https://{self.node["address"]}:7443'

    def start_https(self):
        self.remote('systemctl start mmx-https-api.service')
        self.wait('the node HTTPS port accepts fresh TLS connections', self.fresh_https,
                  bool, seconds=10, interval=0.2)

    def fresh_https(self):
        try:
            with socket.create_connection((self.node['address'], 7443), timeout=1) as connection:
                # This probe tests reachability only. MMX itself still authenticates
                # and pins the node identity during both confirmation phases.
                with ssl._create_unverified_context().wrap_socket(connection, server_hostname=self.node['address']):
                    return True
        except (OSError, ssl.SSLError):
            return False

    def project(self, tasks):
        self.old_records = {r['owed_restore']['owed_restore_id'] for r in self.all_records()}
        self.last_record = None
        vm.write_project(self.workspace, project_name='firewalld_zone_memberships', fleet_stages=[
            {'id': 'firewall', 'nodes': 'all', 'node_tasks': tasks}])

    @contextlib.contextmanager
    def case(self, name):
        started = time.monotonic()
        try:
            with super().case(name):
                yield
        except BaseException:
            print(f'FAILED CASE DURATION {name}: {time.monotonic() - started:.1f} seconds', flush=True)
            raise

    def task(self, zone=ZONE, interfaces=None, sources=None, state='present', policy='keep', duration='15s'):
        fields = {'zone': zone, 'state': state, 'restore_unless_controller_confirms_within': duration}
        if interfaces is not None: fields['interfaces'] = interfaces
        if sources is not None: fields['source_networks'] = sources
        if state != 'absent':
            if interfaces is not None: fields['interfaces_not_listed'] = policy
            if sources is not None: fields['source_networks_not_listed'] = policy
        return {MODULE: fields}

    def apply_task(self, task, command='run', expected=None, change='changed'):
        self.project([task])
        result = self.command(command, expected=vm.success_codes(command) if expected is None else expected)
        outcome = result['entries'][0]['response']['results'][-1]
        self.require(outcome['change'] == change, f'task reports {change}', outcome)
        if MODULE in task and command == 'run' and expected is None and change == 'changed':
            record = self.clean_change()
            self.require(record['change_phase'] == 'change_accepted', 'changed membership was confirmed', record)
            self.require(set(record['before_change_confirmed_connection_kinds']) == {'ssh', 'https'} and
                         set(record['after_change_confirmed_connection_kinds']) == {'ssh', 'https'},
                         'both fresh connection kinds confirmed', record)
        elif expected is None and (command == 'dryrun' or change == 'unchanged'):
            self.require(self.record() is None, 'dryrun and unchanged tasks create no restore record')
        return outcome

    def binding(self, kind, member, layers=(0, 1)):
        script = ''
        for layer in layers:
            option = '--permanent ' if layer else ''
            # firewall-cmd prints `no zone` on standard error, with exit code 2.
            script += f'firewall-cmd {option}--get-zone-of-{kind}={shlex.quote(member)} 2>&1 || test "$?" = 2\n'
        lines = self.remote(script).stdout.splitlines()
        self.require(len(lines) == len(layers), 'one binding from each requested firewall layer', lines)
        return tuple('' if line.strip() == 'no zone' else line.strip() for line in lines)

    def bindings_are(self, kind, member, expected):
        actual = self.binding(kind, member)
        self.require(actual == expected, f'{kind} {member} has runtime and stored bindings {expected}', actual)

    def set_bindings(self, kind, member, desired, layers=(0, 1)):
        for layer in layers:
            zone = desired[layer]
            previous = self.binding(kind, member, (layer,))[0]
            if previous == zone:
                continue
            option = '--permanent ' if layer else ''
            if zone:
                self.remote(f'firewall-cmd {option}--zone={zone} --change-{kind}={shlex.quote(member)}')
            elif previous:
                self.remote(f'firewall-cmd {option}--zone={previous} --remove-{kind}={shlex.quote(member)}')

    def diagnostics(self):
        try:
            print('CASE ' + self.case_name, flush=True)
            print('LAST CONTROLLER OUTPUT ' + json.dumps(self.last_output[-6000:]), flush=True)
            print('RECORDS ' + json.dumps(self.all_records()), flush=True)
            output = self.remote('firewall-cmd --list-all-zones; firewall-cmd --permanent --list-all-zones; '
                                 'journalctl --no-pager -n 18 -u "mmx-owed-restore-*" -u "mmx-activate-change-*"', check=False)
            evidence = output.stdout + output.stderr
            print('FIREWALL AND RESTORE JOURNAL ' + json.dumps(evidence), flush=True)
            (self.case_directory / 'failure-diagnostics.txt').write_text(evidence + '\n' + self.last_output)
        except Exception as error:
            print('DIAGNOSTICS FAILED ' + str(error), flush=True)

    @contextlib.contextmanager
    def old_ssh(self):
        args = vm.unshared(self.ssh[:-1]) + ['-o', 'ControlMaster=no', '-o', 'ControlPath=none',
                                           '-o', 'ControlPersist=no', self.ssh[-1],
                                           'while read line; do printf "%s\\n" "$line"; done']
        process = subprocess.Popen(args, stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                                   stderr=subprocess.PIPE, text=True, start_new_session=True, env=vm.ENV)
        self.processes.append(process)
        def echo(text):
            process.stdin.write(text + '\n'); process.stdin.flush()
            readable, _, _ = select.select([process.stdout], [], [], 3)
            self.require(readable and process.stdout.readline().strip() == text,
                         'the established SSH connection still carries new traffic')
        try:
            echo('before-change')
            yield echo
        finally:
            if process.poll() is None:
                os.killpg(process.pid, signal.SIGKILL)
            process.communicate(timeout=5)

    def check_cutoff_connections(self, record, echo, kind, member):
        # Use the deadline recorded on the node, not elapsed controller
        # time or an assumed amount left from the configured duration.
        # SSH, HTTPS, and the old-session echo can take 8, 1, and 3 seconds.
        remaining = instant(record['restore_deadline']) - self.node_time()
        self.require(remaining > 12, 'the node deadline leaves time for fresh-connection and old-session probes', remaining)
        self.require(self.fresh_ssh().returncode != 0, 'the cut refuses a fresh SSH connection')
        self.require(not self.fresh_https(), 'the cut refuses a fresh HTTPS TLS connection')
        echo('during-cut')
        self.bindings_are(kind, member, ('drop', 'drop'))
        self.require(self.node_time() < instant(record['restore_deadline']),
                     'all cut-off probes finished while both layers remained in drop, before the node deadline')

    def cutoff(self, kind, member, previous, scenario='timer'):
        self.start_https()  # The controller-draining helper stopped it in an earlier case.
        self.set_bindings(kind, member, previous)
        self.bindings_are(kind, member, previous)
        duration = '2m' if scenario == 'restart' else ('30s' if scenario == 'worker-killed' else '22s')
        task = self.task('drop', interfaces=[member] if kind == 'interface' else None,
                         sources=[member] if kind == 'source' else None, duration=duration)
        with self.old_ssh() as echo:
            self.project([task])
            process = self.background()
            record = self.wait('membership applied and waiting for new connections', self.record,
                               lambda r: r and r['change_phase'] == 'waiting_after_change', seconds=20)
            self.bindings_are(kind, member, ('drop', 'drop'))
            self.require(set(record['before_change_confirmed_connection_kinds']) == {'ssh', 'https'},
                         'SSH and HTTPS both worked before the cut', record)
            self.check_cutoff_connections(record, echo, kind, member)
            interrupted = scenario in ('worker-killed', 'restart')
            if interrupted:
                RestoreObservations.stop_controller_and_drain_requests(self, process)
                saved = self.command('results')
                if scenario == 'worker-killed':
                    self.kill_worker(record)
                else:
                    self.remote('systemd-run --on-active=1s --timer-property=AccuracySec=100ms /usr/bin/systemctl reboot')
                    vm.await_boot(record['node_boot_id_before_change'])
                    self.connect(); self.inventory('https_with_ssh_fallback')
            if scenario == 'failed-restore':
                self.firewall_command_path = self.remote('command -v firewall-cmd').stdout.strip()
                self.firewall_command_mode = self.remote('stat -c %a ' + self.firewall_command_path).stdout.strip()
                self.remote('chmod 000 ' + self.firewall_command_path)
                failed = self.wait('failed restore remains owed', self.record,
                                   lambda r: r and r['change_phase'] == 'restore_failed', seconds=35)
                self.require(failed['owed_restore']['restore_failure_reason'] and
                             not failed['owed_restore_cleanup_complete'] and
                             not failed['owed_restore']['previous_membership_restored'],
                             'failed restoration retains its cause and obligation', failed)
                self.remote(f'chmod {self.firewall_command_mode} {self.firewall_command_path}')
                self.firewall_command_mode = None
                self.bindings_are(kind, member, ('drop', 'drop'))
                self.remote('systemctl start ' + failed['owed_restore']['restore_service_unit'])
            restored = self.wait('timer or boot service restores without a controller request', self.record,
                                 lambda r: r and r['owed_restore']['restore_status'] == 'succeeded', seconds=40)
            self.require(not restored['after_change_confirmed_connection_kinds'], 'the cut received no after-change confirmation', restored)
            if scenario == 'restart':
                self.require(self.node_time() < instant(record['restore_deadline']),
                             'boot restoration finished before the original deadline', restored)
            self.bindings_are(kind, member, previous)
            self.require(self.fresh_ssh().returncode == 0, 'new SSH connections work after restoration')
            self.start_https()
            if interrupted:
                result = self.command('status', saved['id'])
            else:
                result = self.collect(process, 1)
                result = self.command('status', result['id'])
            response = result['entries'][0]['response']
            self.require(response['status'] == 'failed' and response['owed_restores'][-1]['restore_status'] == 'succeeded',
                         'restored node job remains failed with the latest restore outcome', response)
            if not interrupted:
                outcome = response['results'][-1]
                self.require(outcome['status'] == 'failed' and outcome['change'] == 'changed',
                             'a restored membership task reports failed and changed', outcome)
                self.require(not outcome.get('value'), 'a restored task omits membership operation lists', outcome)
            output = self.command('status', result['id'], human=True)
            self.require('firewalld zone membership was restored to its previous state' in output,
                         'human output describes membership restoration', output)
            self.clean_change()
        # Return this fixture to no explicit binding before the next cut case.
        self.set_bindings(kind, member, ('', ''))

    def run(self):
        with self.case('zone-dryrun-create-reload-and-unchanged'):
            self.apply_task({'firewalld_zone': {'zone': ZONE}}, 'dryrun', change='would_change')
            for option in ('', '--permanent '):
                self.require(ZONE not in self.remote('firewall-cmd ' + option + '--get-zones').stdout.split(), 'dryrun leaves the zone absent')
            self.remote('firewall-cmd --zone=public --add-port=49123/tcp')
            result = self.apply_task({'firewalld_zone': {'zone': ZONE}})
            self.require('reloaded firewalld' in result['reason'], 'creation reports its reload', result)
            for option in ('', '--permanent '):
                self.require(ZONE in self.remote('firewall-cmd ' + option + '--get-zones').stdout.split(), 'created zone exists in both layers')
            self.require(self.remote('firewall-cmd --zone=public --query-port=49123/tcp', check=False).returncode == 1,
                         'zone creation reload drops a runtime-only port of another zone')
            self.apply_task({'firewalld_zone': {'zone': ZONE}}, change='unchanged')

        network, second_network = '198.51.100.0/24', '2001:db8:44::/64'
        with self.case('source-networks-dryrun-present-unchanged-move-remove-and-absent'):
            task = self.task(sources=[network])
            preview = self.apply_task(task, 'dryrun', change='would_change')
            self.bindings_are('source', network, ('', ''))
            result = self.apply_task(task)
            self.require(preview['value'] == result['value'], 'dryrun and apply show the same per-layer changes')
            self.bindings_are('source', network, (ZONE, ZONE))
            self.apply_task(task, change='unchanged')
            self.bindings_are('source', network, (ZONE, ZONE))
            refusal = self.apply_task({'firewalld_zone': {'zone': ZONE, 'state': 'absent'}}, expected=1, change='unchanged')
            self.require(network in refusal['reason'] and 'firewalld_zone_membership' in refusal['reason'], 'nonempty zone refusal names its member', refusal)
            self.set_bindings('source', network, ('trusted', 'trusted'))
            moved = self.apply_task(task)
            self.require(all(layer['moved'][0]['previous_zone'] == 'trusted' for layer in moved['value'].values()), 'move reports its previous zone', moved)
            self.bindings_are('source', network, (ZONE, ZONE))
            self.apply_task(self.task(sources=[second_network], policy='remove'))
            self.bindings_are('source', network, ('', ''))
            self.bindings_are('source', second_network, (ZONE, ZONE))
            self.apply_task(self.task(sources=[second_network], state='absent'))
            self.bindings_are('source', second_network, ('', ''))

        with self.case('omitted-membership-list-stays-unchanged'):
            self.set_bindings('source', network, (ZONE, ZONE))
            self.set_bindings('interface', self.second, (ZONE, ZONE))
            self.apply_task(self.task(interfaces=[], policy='remove'))
            self.bindings_are('interface', self.second, ('', ''))
            self.bindings_are('source', network, (ZONE, ZONE))
            self.set_bindings('interface', self.second, (ZONE, ZONE))
            self.apply_task(self.task(sources=[], policy='remove'))
            self.bindings_are('interface', self.second, (ZONE, ZONE))
            self.bindings_are('source', network, ('', ''))
            self.apply_task(self.task(interfaces=[self.second], state='absent'))

        with self.case('unmanaged-interface-present-move-reload-and-absent'):
            self.apply_task(self.task(interfaces=[self.second]))
            self.bindings_are('interface', self.second, (ZONE, ZONE))
            self.apply_task(self.task('internal', interfaces=[self.second]))
            self.bindings_are('interface', self.second, ('internal', 'internal'))
            self.remote('firewall-cmd --reload')
            self.bindings_are('interface', self.second, ('internal', 'internal'))
            self.apply_task(self.task('internal', interfaces=[self.second], state='absent'))
            self.bindings_are('interface', self.second, ('', ''))

        with self.case('missing-zone-refusal'):
            for command in ('dryrun', 'run'):
                outcome = self.apply_task(self.task('mmxmissing', sources=[network]), command, expected=1, change='unchanged')
                self.require('firewalld_zone' in outcome['reason'] and 'running firewall' in outcome['reason'], 'missing zone names its layer and creation module', outcome)
                self.require(self.record() is None, 'refusal creates no record')

        with self.case('networkmanager-profile-ownership-including-dryrun-and-unlisted-removal'):
            content = (f'[connection]\nid=mmx-zone-membership\nuuid={PROFILE_UUID}\ntype=ethernet\n'
                       f'interface-name={self.second}\nautoconnect=false\nzone={ZONE}\n\n'
                       '[ipv4]\nmethod=manual\naddress1=192.0.2.44/24\nnever-default=true\n\n[ipv6]\nmethod=disabled\n')
            self.profile_created = True
            self.remote(f'printf %s {shlex.quote(content)} > {PROFILE}\nchmod 600 {PROFILE}\n'
                        f'restorecon {PROFILE}\nnmcli device set {self.second} managed yes\n'
                        f'nmcli connection load {PROFILE}\nnmcli connection up uuid {PROFILE_UUID}')
            before = self.remote('sha256sum ' + PROFILE).stdout
            for command in ('dryrun', 'run'):
                for task in [self.task('internal', interfaces=[self.second]),
                             self.task(interfaces=[self.second], state='absent'),
                             self.task(interfaces=[], policy='remove')]:
                    outcome = self.apply_task(task, command, expected=1, change='unchanged')
                    self.require(all(text in outcome['reason'] for text in (self.second, PROFILE, 'zone=', 'networkmanager_connection_file')),
                                 'refusal names the card, profile, zone line, and owning module', outcome)
                    self.require(before == self.remote('sha256sum ' + PROFILE).stdout, 'refusal preserves profile bytes')
                    self.require(self.record() is None, 'ownership refusal writes no record')
            self.remove_profile()

        # No later case needs the second card, and one of them restarts the
        # node: a card that is connected to nothing makes the network's wait
        # at boot fail, which the harness reports as a degraded node.
        vm.cmd(['incus', 'config', 'device', 'remove', self.instance, 'second-interface'])
        self.added_interface = False
        for previous in (('', ''), ('trusted', 'trusted')):
            with self.case('source-cut-from-' + (previous[0] or 'no-zone')):
                self.cutoff('source', self.controller_source, previous)
        for scenario in ('worker-killed', 'restart', 'failed-restore'):
            with self.case('source-cut-' + scenario):
                # Different layers before reboot prove restoration does not just
                # reload the stored configuration into the running firewall.
                previous = ('', 'trusted') if scenario == 'restart' else ('', '')
                self.cutoff('source', self.controller_source, previous, scenario)
        with self.case('empty-zone-absent'):
            self.apply_task({'firewalld_zone': {'zone': ZONE, 'state': 'absent'}})
            for option in ('', '--permanent '):
                self.require(ZONE not in self.remote('firewall-cmd ' + option + '--get-zones').stdout.split(), 'removed zone is absent in both layers')
        print(json.dumps({'passed': self.passed, 'count': len(self.passed)}, indent=2), flush=True)

    def remove_profile(self):
        if self.profile_created:
            self.remote(f'nmcli connection down uuid {PROFILE_UUID} || true\n'
                        f'nmcli connection delete uuid {PROFILE_UUID} || true\n'
                        f'rm -f {PROFILE}\nnmcli device set {self.second} managed no')
            self.profile_created = False

    def cleanup(self):
        failures = []
        def attempt(label, action):
            try:
                return action()
            except Exception as error:
                failures.append(f'{label}: {error}')
                print(f'FAIL cleanup {label}: {error}', flush=True)
                return None

        for process in self.processes:
            if process.poll() is None:
                attempt('stop suite process', lambda: os.killpg(process.pid, signal.SIGKILL))
                attempt('collect suite process', lambda: process.communicate(timeout=5))
        if self.firewall_command_mode:
            attempt('restore firewall-cmd permissions', lambda: self.remote(
                f'chmod {self.firewall_command_mode} {self.firewall_command_path}'))
        records = attempt('read restore records', self.all_records) or []
        for record in records:
            if record['owed_restore']['module_name'] != MODULE or record.get('owed_restore_cleanup_complete'):
                continue
            job = record['node_job_id']
            attempt('stop own node job worker', lambda: self.remote(
                "for path in /proc/[0-9]*/cmdline; do "
                "args=$(tr '\\0' ' ' < \"$path\" 2>/dev/null) || continue; "
                f"case \"$args\" in '/opt/mmx/bin/'*'/mmx-runner worker {job} ') "
                "pid=${path#/proc/}; kill -KILL \"${pid%/cmdline}\";; esac; done"))
            attempt('run owed restore', lambda: self.remote(
                'systemctl start ' + record['owed_restore']['restore_service_unit'], timeout=60))
        if self.profile_created:
            for command in (f'nmcli connection down uuid {PROFILE_UUID}',
                            f'nmcli connection delete uuid {PROFILE_UUID}', f'rm -f {PROFILE}',
                            f'nmcli device set {self.second} managed no'):
                attempt('remove suite profile', lambda: self.remote(command))
        installed = attempt('check firewalld installation', lambda: self.remote('command -v firewall-cmd', check=False))
        installed = installed is not None and installed.returncode == 0
        running = attempt('check running firewall', lambda: self.remote('firewall-cmd --state', check=False)) if installed else None
        if running is not None and running.returncode == 0:
            for kind, member, bindings in self.original_bindings:
                for layer in (0, 1):
                    attempt('restore original membership', lambda: self.set_bindings(kind, member, bindings, (layer,)))
            if self.added_interface and hasattr(self, 'second'):
                for layer in (0, 1):
                    attempt('remove second-card membership', lambda: self.set_bindings('interface', self.second, ('', ''), (layer,)))
            zones = attempt('read stored zones', lambda: self.remote('firewall-cmd --permanent --get-zones'))
            if zones and ZONE in zones.stdout.split():
                attempt('remove suite zone', lambda: self.remote('firewall-cmd --permanent --delete-zone=' + ZONE))
            for zone, permanent, port in self.added_ports:
                option = '--permanent ' if permanent else ''
                attempt('remove suite port', lambda: self.remote(f'firewall-cmd {option}--zone={zone} --remove-port={port}/tcp'))
            attempt('reload firewall after fixture removal', lambda: self.remote('firewall-cmd --reload'))
            for kind, member, bindings in self.original_bindings:
                for layer in (0, 1):
                    attempt('restore original membership after reload', lambda: self.set_bindings(kind, member, bindings, (layer,)))
        if self.added_interface:
            attempt('remove second card', lambda: vm.cmd(['incus', 'config', 'device', 'remove', self.instance, 'second-interface']))
        if installed and self.original_service is not None:
            if 'active' not in self.original_service:
                attempt('restore stopped firewall service', lambda: self.remote('systemctl stop firewalld'))
            if 'enabled' not in self.original_service:
                attempt('restore disabled firewall service', lambda: self.remote('systemctl disable firewalld'))
        attempt('reset suite state', vm.reset)
        if failures:
            raise RuntimeError('; '.join(failures))


def main():
    target = vm.target()
    if not target.get('incus_instance') or not target.get('platform', '').endswith(' Incus vm'):
        print('SKIP firewalld_zone_memberships.py: this suite needs an Incus VM for a second card, real firewall isolation, and observations during connection loss', flush=True)
        return
    import incus
    distribution = next((name for name, image in incus.IMAGES.items() if image == target.get('image')), '')
    if distribution not in SUPPORTED:
        print(f'SKIP firewalld_zone_memberships.py: {distribution or target.get("image")} is not one of the firewalld guests, Rocky Linux 9 and AlmaLinux 10', flush=True)
        return
    vm.up(); vm.reset()
    suite, failure = None, None
    try:
        suite = Suite.__new__(Suite)
        suite.__init__(distribution, target['incus_instance'])
        suite.run()
    except BaseException as error:
        failure = error
        print('FAIL firewalld_zone_memberships.py: ' + str(error), flush=True)
        if suite and getattr(suite, 'case_name', '') == 'setup': suite.diagnostics()
    finally:
        if suite:
            try: suite.cleanup()
            except Exception as error:
                print('FAIL cleanup: ' + str(error), flush=True)
                failure = failure or error
    if failure is not None: raise SystemExit(1)


if __name__ == '__main__':
    vm.run_suite(main)
