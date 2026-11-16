#!/usr/bin/env python3
"""Production NetworkManager files, native acceptance, and autonomous recovery."""
import json
import os
from pathlib import Path
import shlex
import shutil
import signal
import uuid

import vm
from systemd_networkd_files import Suite as NetworkObservations
from owed_restores import instant

MODULE = 'networkmanager_connection_file'
DIRECTORY = '/etc/NetworkManager/system-connections'
FIXTURES = '/var/tmp/mmx-integration/networkmanager-connection-files'
GATE_DIRECTORY = '/etc/systemd/system/mmx-activate-change-.service.d'
SUPPORTED = ('rocky9', 'almalinux10')


def profile(name, interface, kind='ethernet', address='10.0.0.5/24', extra='', identifier=None):
    identity = str(identifier or uuid.uuid5(uuid.NAMESPACE_DNS, 'mmx-suite-' + name))
    return (f'[connection]\nid={name}\nuuid={identity}\ntype={kind}\ninterface-name={interface}\n'
            'autoconnect=true\nautoconnect-priority=100\n' + extra +
            f'\n[ipv4]\nmethod={"manual" if address else "disabled"}\n' +
            (f'address1={address}\n' if address else '') + '[ipv6]\nmethod=disabled\n')


class Suite(NetworkObservations):
    def __init__(self, distribution, instance):
        self.distribution, self.instance = distribution, instance
        self.workspace = vm.ROOT / 'networkmanager-connection-files'
        shutil.rmtree(self.workspace, ignore_errors=True); self.workspace.mkdir(parents=True)
        self.case_name, self.case_directory = 'setup', self.workspace
        self.processes, self.ssh_units, self.passed = [], [], []
        self.destination, self.last_output, self.last_record = '', '', None
        self.old_records = set(); self.added_interface = False; self.nmcli_mode = None
        vm.build(); self.node, self.ssh = vm.connection()
        self.binaries, self.mmx = vm.ROOT / 'bin', vm.ROOT / 'bin/mmx'
        self.remote(f'mkdir -p {FIXTURES}')
        self.main = json.loads(self.remote('ip -j -4 route show default').stdout)[0]['dev']
        self.initial_addresses = self.addresses(self.main)
        # Guests of one run share a bridge, and NetworkManager refuses an address
        # that another guest answers for. So each guest cuts itself off with an
        # address of its own, in the range reserved for benchmarks.
        self.cut_address = '198.18.' + '.'.join(self.initial_addresses[0].split('/')[0].split('.')[2:])
        print('NetworkManager ' + self.remote('nmcli --version').stdout.strip(), flush=True)
        self.main_uuid = self.remote(f'nmcli -g GENERAL.CON-UUID device show {shlex.quote(self.main)}').stdout.strip()
        self.main_file = self.profiles()[self.main_uuid]
        self.baseline_main = self.main_file
        self.original_main = self.remote('cat ' + shlex.quote(self.main_file)).stdout
        before = set(self.remote('ls /sys/class/net').stdout.split())
        vm.cmd(['incus', 'config', 'device', 'add', instance, 'second-interface', 'nic', 'nictype=p2p', 'mtu=1500'])
        self.added_interface = True
        names = self.wait('second physical card appears', lambda: set(self.remote('udevadm settle --timeout=5; ls /sys/class/net').stdout.split()) - before,
                          lambda names: len(names) == 1, seconds=10)
        self.second = names.pop()
        self.second_baseline = profile('mmx-second-baseline', self.second, address='10.0.9.5/24').replace('priority=100', 'priority=1000')
        self.plain_file('mmx-second-baseline.nmconnection', self.second_baseline)
        self.inventory('ssh_only')

    def profiles(self):
        text = self.remote('nmcli -t --escape no -f UUID,FILENAME connection show').stdout
        return dict(line.split(':', 1) for line in text.splitlines() if line)

    def plain_file(self, name, contents):
        path = DIRECTORY + '/' + name
        self.remote(f'printf %s {shlex.quote(contents)} > {shlex.quote(path)}\nchmod 0600 {shlex.quote(path)}\n'
                    f'nmcli connection load {shlex.quote(path)}')
        identity = next(identity for identity, filename in self.profiles().items() if filename == path)
        self.remote('nmcli connection up uuid ' + identity)
        return identity

    def remove_plain(self, name):
        path = shlex.quote(DIRECTORY + '/' + name)
        self.remote(f'rm -f {path}\nnmcli connection load {path}')

    def task(self, name, content=None, duration='20s'):
        fields = {'name': name, 'restore_unless_controller_confirms_within': duration}
        fields.update({'content': content} if content is not None else {'state': 'absent'})
        return {MODULE: fields}

    def project(self, tasks):
        self.old_records = {r['owed_restore']['owed_restore_id'] for r in self.all_records()}
        self.destination = DIRECTORY + '/' + tasks[-1][MODULE]['name']; self.last_record = None
        vm.write_project(self.workspace, project_name='networkmanager_connection_files', fleet_stages=[
            {'id': 'network', 'nodes': 'all', 'node_tasks': tasks}])

    def apply_many(self, tasks):
        self.project(tasks); result = self.command('run')
        results = result['entries'][0]['response']['results']
        self.require(len(results) == len(tasks) and all(r['status'] == 'succeeded' and r['change'] == 'changed' for r in results),
                     'every grouped NetworkManager task succeeded and changed', results)
        records = self.wait('every grouped file accepted and cleaned',
                            lambda: [r for r in self.all_records() if r['owed_restore']['owed_restore_id'] not in self.old_records],
                            lambda records: len(records) == len(tasks) and all(r['change_phase'] == 'change_accepted' and r.get('owed_restore_cleanup_complete') for r in records))
        self.require({r['owed_restore']['destination_file'] for r in records} ==
                     {DIRECTORY + '/' + task[MODULE]['name'] for task in tasks}, 'one accepted record per grouped file', records)
        self.check_cleanup_paths(records)
        return result

    def file_state(self):
        path = shlex.quote(self.destination)
        return self.remote(f'if test -f {path}; then sha256sum {path}; stat -c "%a %u %g %y %C" {path}; else echo absent; fi').stdout

    def service_entries(self, cursor):
        text = self.remote('journalctl --no-pager -u NetworkManager.service --cursor ' + shlex.quote(cursor) +
                           ' -o json --output-fields=__CURSOR,MESSAGE').stdout
        return [entry for line in text.splitlines() if (entry := json.loads(line))['__CURSOR'] != cursor]

    def refused(self, name, contents, reason):
        for command in ('dryrun', 'run'):
            self.project([self.task(name, contents)])
            before, profiles, records = self.file_state(), self.profiles(), self.all_records()
            units = self.remote('find /etc/systemd/system -name "mmx-*restore-*" -o -name "mmx-activate-change-*"').stdout
            cursor = self.journal_cursor()
            result = self.command(command, expected=1)
            task = result['entries'][0]['response']['results'][-1]
            self.require(task['status'] == 'failed' and task['change'] == 'unchanged' and reason in task.get('reason', ''),
                         f'{command}: unchanged refusal naming {reason}', task)
            self.require(self.file_state() == before and self.profiles() == profiles and self.all_records() == records,
                         'refusal changed no file, profile, or restore record', {'before': before, 'after': self.file_state(), 'profiles': self.profiles()})
            self.require(not self.service_entries(cursor), 'refusal made no NetworkManager journal entry', self.service_entries(cursor))
            self.require(units == self.remote('find /etc/systemd/system -name "mmx-*restore-*" -o -name "mmx-activate-change-*"').stdout,
                         'refusal wrote no unit')

    def diagnostics(self):
        # The inherited diagnostics include both MMX services and their record.
        super().diagnostics()
        result = self.remote('nmcli -t --escape no -f UUID,FILENAME connection show; '
                             'nmcli -t -f GENERAL.DEVICE,GENERAL.STATE,GENERAL.CON-UUID device show; '
                             'journalctl --no-pager -u NetworkManager.service -n 20 -o short-precise', check=False)
        print('NETWORKMANAGER STATE AND JOURNAL ' + json.dumps(result.stdout + result.stderr), flush=True)
        with (self.case_directory / 'failure-diagnostics.txt').open('a') as stream:
            stream.write('\n' + result.stdout + result.stderr)

    def main_restored(self):
        super().main_restored()
        actual = self.remote(f'nmcli -g GENERAL.CON-UUID device show {shlex.quote(self.main)}').stdout.strip()
        self.require(actual == self.main_uuid, 'the original main-card profile is active again', actual)

    def run(self):
        second = profile('mmx-second', self.second)
        with self.case('second-card-dryrun-present-unchanged-changed-and-absent'):
            self.apply('mmx-second.nmconnection', second, expected=(0, 2, 3), change='would_change', command='dryrun')
            self.require(self.record() is None and self.destination not in self.profiles().values() and self.file_state().strip() == 'absent', 'dryrun created no file, record, or profile')
            self.require(not self.remote('find /etc/systemd/system -name "mmx-*restore-*" -o -name "mmx-activate-change-*"').stdout, 'dryrun created no unit')
            self.apply('mmx-second.nmconnection', second); self.address(self.second, '10.0.0.5')
            self.require(self.remote(f'stat -c "%a %U %G" {shlex.quote(self.destination)}').stdout.strip() == '600 root root', 'fixed keyfile metadata')
            labels = self.remote(f'ls -Z {shlex.quote(self.destination)}\nmatchpathcon {shlex.quote(self.destination)}').stdout.splitlines()
            self.require(labels[0].split()[0].split(':')[2] == labels[1].split()[-1].split(':')[2], 'installed keyfile has its policy SELinux label', labels)
            cursor = self.journal_cursor(); self.apply('mmx-second.nmconnection', second, change='unchanged')
            self.require(self.record() is None and not self.service_entries(cursor), 'unchanged file caused no activation or record', self.service_entries(cursor))
            self.inventory('https_with_ssh_fallback')
            self.apply('mmx-second.nmconnection', second.replace('10.0.0.5', '10.0.0.6')); self.address(self.second, '10.0.0.6')
            self.require(sorted(self.record()['after_change_confirmed_connection_kinds']) == ['https', 'ssh'], 'both new connection kinds confirmed', self.last_record)
            self.inventory('ssh_only'); self.apply('mmx-second.nmconnection'); self.address(self.second, '10.0.0.6', False)
            self.require(self.destination not in self.profiles().values(), 'removed file has no loaded profile', self.profiles())

        with self.case('native-rejection-and-unknown-key-limit'):
            self.refused('mmx-invalid.nmconnection', second.replace('10.0.0.5/24', 'not-an-address'), "ipv4.method: method 'manual' requires at least an address")
            self.refused('mmx-invalid.nmconnection', 'this is not a keyfile\n', 'which is not a key-value pair, group, or comment')
            for label, contents, native_reason in [
                    ('unknown-section', second + '[mmx-unknown-section]\nprobe=true\n', "invalid setting name 'mmx-unknown-section'"),
                    ('missing-card', second.replace(self.second, 'mmxmissing0'), 'No suitable device found for this connection')]:
                self.case_name = 'native-rejection-' + label
                self.destination = DIRECTORY + '/mmx-invalid.nmconnection'; self.before = self.file_state()
                result = self.apply('mmx-invalid.nmconnection', contents, expected=1)
                outcome = result['entries'][0]['response']['results'][-1]
                self.require(native_reason in outcome.get('reason', ''), f'{label}: native rejection names {native_reason}', outcome)
                self.restored(result); self.require(self.destination not in self.profiles().values(), 'rejected new profile was unloaded', self.profiles())
            self.apply_many([self.task('mmx-second.nmconnection', second + 'definitely-unknown-key=true\n'), self.task('mmx-second.nmconnection')])

        with self.case('old-format-refusal-and-selected-profile-migration'):
            if self.main_file.startswith('/etc/sysconfig/network-scripts/'):
                candidate = profile('mmx-competing-main', self.main).replace('method=manual', 'method=auto')
                self.refused('mmx-main.nmconnection', candidate, 'nmcli connection migrate --plugin keyfile uuid ' + self.main_uuid)
                self.remote('nmcli connection migrate --plugin keyfile uuid ' + self.main_uuid)
                self.main_file = self.profiles()[self.main_uuid]
                self.require(Path(self.main_file).name == 'System ' + self.main + '.nmconnection', 'migration preserves the spaced filename', self.main_file)
                contents = self.remote('cat ' + shlex.quote(self.main_file)).stdout
                self.apply(Path(self.main_file).name, contents, change='unchanged')
                self.apply(Path(self.main_file).name, contents + '\n# Managed by the MMX guest suite.\n')
                self.main_restored()
            else:
                print('SKIP old-format migration: the main profile is already a keyfile', flush=True)
            self.original_main = self.remote('cat ' + shlex.quote(self.main_file)).stdout
            self.remote(f'cp -a {shlex.quote(self.main_file)} {FIXTURES}/main-before-tests')

        with self.case('duplicate-uuid-and-omitted-uuid'):
            self.refused('mmx-duplicate.nmconnection', profile('mmx-duplicate', self.second, identifier=self.main_uuid), self.main_file)
            without = '\n'.join(line for line in second.splitlines() if not line.startswith('uuid=')) + '\n'
            self.apply('mmx-no-uuid.nmconnection', without)
            self.require(self.destination in self.profiles().values(), 'a UUID-less file is loaded by its path', self.profiles())
            self.apply('mmx-no-uuid.nmconnection')

        with self.case('devices-bridge-port-vlan-bond-dummy-and-rename'):
            bridge = profile('mmx-bridge', 'mmxbr0', 'bridge', extra='[bridge]\nstp=false\n')
            bridge_uuid = str(uuid.uuid5(uuid.NAMESPACE_DNS, 'mmx-suite-mmx-bridge'))
            port = profile('mmx-port', self.second, address='', extra=f'controller={bridge_uuid}\nport-type=bridge\n')
            self.apply_many([self.task('mmx-bridge.nmconnection', bridge), self.task('mmx-port.nmconnection', port)])
            self.device('mmxbr0'); self.address('mmxbr0', '10.0.0.5'); self.port_member()
            self.apply_many([self.task('mmx-port.nmconnection'), self.task('mmx-bridge.nmconnection')]); self.device('mmxbr0', False)
            definitions = [('mmxvlan77', 'vlan', f'[vlan]\nparent={self.second}\nid=77\n'),
                           ('mmxbond0', 'bond', '[bond]\nmode=balance-rr\n'), ('mmxdummy0', 'dummy', '')]
            self.apply_many([self.task(f'mmx-{kind}.nmconnection', profile('mmx-' + kind, device, kind, extra=extra)) for device, kind, extra in definitions])
            for device, _, _ in definitions: self.device(device)
            self.apply('mmx-dummy.nmconnection', profile('mmx-dummy', 'mmxrenamed0', 'dummy'))
            self.device('mmxdummy0', False); self.device('mmxrenamed0')
            self.apply_many([self.task(f'mmx-{kind}.nmconnection') for _, kind, _ in reversed(definitions)])
            for device in ('mmxvlan77', 'mmxbond0', 'mmxrenamed0'): self.device(device, False)

        with self.case('timer-recreates-bridge-address-and-previously-active-port'):
            self.remove_plain('mmx-second-baseline.nmconnection')
            try:
                self.apply_many([self.task('mmx-bridge.nmconnection', bridge), self.task('mmx-port.nmconnection', port)])
                self.start_gate()
                self.project([self.task('mmx-bridge.nmconnection', duration='15s')]); self.before = self.file_state()
                process = self.background(); record = self.wait('activation service reaches gate', self.record, lambda r: r and r['change_phase'] == 'activating_config')
                self.wait('native activation gate has started', lambda: self.remote('systemctl show -p ControlPID --value mmx-activate-change-' + record['owed_restore']['owed_restore_id'] + '.service').stdout.strip(), lambda pid: pid not in ('', '0'))
                self.stop_controller_and_drain_requests(process); saved = self.command('results')
                self.remote(f'touch {FIXTURES}/release_change_activation')
                record = self.after_wait(); self.device('mmxbr0', False); self.kill_worker(record)
                self.wait('timer alone restores the bridge', self.record, lambda r: r and r['owed_restore']['restore_status'] == 'succeeded', seconds=35)
                self.device('mmxbr0'); self.address('mmxbr0', '10.0.0.5'); self.port_member()
                self.restored(self.command('status', saved['id']), task=False)
                self.remote(f'rm -rf {GATE_DIRECTORY}\nsystemctl daemon-reload')
                self.apply_many([self.task('mmx-port.nmconnection'), self.task('mmx-bridge.nmconnection')])
            finally:
                self.plain_file('mmx-second-baseline.nmconnection', self.second_baseline)

        for case in ('cut-with-previous-file', 'cut-with-no-previous-file', 'cut-worker-killed-timer-alone',
                     'cut-restart-before-deadline', 'cut-failed-restore-and-administrator-repair'):
            with self.case(case):
                added = 'no-previous' in case
                name = 'mmx-main-added.nmconnection' if added else Path(self.main_file).name
                if not added: self.remote(f'touch -d "2001-01-02 03:04:05 UTC" {shlex.quote(self.main_file)}')
                contents = profile('mmx-main-added' if added else 'mmx-main-cut', self.main, address=self.cut_address + '/16', identifier=None if added else self.main_uuid).replace('priority=100', 'priority=999')
                self.project([self.task(name, contents, duration='2m' if 'restart' in case else ('20s' if 'worker-killed' in case else '12s'))])
                self.before = self.file_state(); process = self.background(); record = self.after_wait()
                self.address(self.main, self.cut_address)
                self.require(not any(address in self.addresses(self.main) for address in self.initial_addresses), 'cut removed original addresses', self.addresses(self.main))
                self.require(not self.remote('ip -4 route show default').stdout and self.fresh_ssh().returncode != 0, 'cut removed route and refuses new SSH')
                interrupted = 'worker-killed' in case or 'restart' in case
                if interrupted:
                    self.stop_controller_and_drain_requests(process); saved = self.command('results')
                    if 'worker-killed' in case: self.kill_worker(record)
                    else:
                        self.remote('systemd-run --on-active=1s --timer-property=AccuracySec=100ms /usr/bin/systemctl reboot')
                        vm.await_boot(record['node_boot_id_before_change']); self.node, self.ssh = vm.connection(); self.inventory('ssh_only')
                if 'failed-restore' in case:
                    self.nmcli_path = self.remote('command -v nmcli').stdout.strip()
                    self.nmcli_mode = self.remote('stat -c %a ' + self.nmcli_path).stdout.strip(); self.remote('chmod 000 ' + self.nmcli_path)
                    failed = self.wait('native restore failed and remains owed', self.record, lambda r: r and r['change_phase'] == 'restore_failed', seconds=35)
                    self.remote(f'chmod {self.nmcli_mode} {self.nmcli_path}'); self.nmcli_mode = None
                    self.remote('systemctl start ' + failed['owed_restore']['restore_service_unit'])
                restored = self.wait('restore completes before another MMX request', self.record, lambda r: r and r['owed_restore']['restore_status'] == 'succeeded', seconds=35)
                self.require(not restored['after_change_confirmed_connection_kinds'], 'cut had no after-change confirmation', restored)
                if 'restart' in case: self.require(self.node_time() < instant(record['restore_deadline']), 'restore finished before the deadline; reboot consumed the allowance', record)
                self.main_restored()
                result = self.command('status', saved['id']) if interrupted else self.collect(process, 1)
                result = self.command('status', result['id']); self.restored(result, task=not interrupted)
                human = self.command('status', result['id'], human=True)
                self.require(self.destination + ' was restored to its previous state' in human, 'human output reports restored file', human)
                if added: self.require(self.destination not in self.profiles().values(), 'added profile was unloaded', self.profiles())
        print(json.dumps({'passed': self.passed, 'count': len(self.passed)}, indent=2), flush=True)

    def port_member(self):
        self.wait('the physical port is back in the bridge', lambda: self.remote(f'ip -o link show dev {self.second}').stdout,
                  lambda text: 'master mmxbr0' in text, seconds=10, interval=0.2)

    def start_gate(self):
        contents = ("[Service]\nExecStartPre=/bin/sh -c 'while ! test -e " + FIXTURES +
                    "/release_change_activation; do sleep 0.05; done'\n")
        self.remote(f'mkdir -p {GATE_DIRECTORY}\nrm -f {FIXTURES}/release_change_activation\n'
                    f'printf %s {shlex.quote(contents)} > {GATE_DIRECTORY}/wait-for-test.conf\n'
                    'systemctl daemon-reload')

    def cleanup(self):
        try:
            for process in self.processes:
                if process.poll() is None: os.killpg(process.pid, signal.SIGKILL); process.communicate(timeout=5)
            if self.nmcli_mode: self.remote(f'chmod {self.nmcli_mode} {self.nmcli_path}')
            self.remote(f'rm -rf {GATE_DIRECTORY}\n'
                        'for unit in /etc/systemd/system/mmx-owed-restore-*.timer /etc/systemd/system/mmx-owed-restore-*.service /etc/systemd/system/mmx-activate-change-*.service; do '
                        'test ! -f "$unit" || systemctl disable --now "${unit##*/}"; done\n'
                        'rm -f /etc/systemd/system/mmx-owed-restore-*.timer /etc/systemd/system/mmx-owed-restore-*.service /etc/systemd/system/mmx-activate-change-*.service\n'
                        f'for file in {DIRECTORY}/mmx-*.nmconnection; do test ! -f "$file" || {{ rm -f "$file"; nmcli connection load "$file"; }}; done\n'
                        'systemctl daemon-reload\nsystemctl reset-failed')
            if hasattr(self, 'main_file'):
                self.remote(f'if test -f {FIXTURES}/main-before-tests; then cp -a {FIXTURES}/main-before-tests {shlex.quote(self.main_file)}; nmcli connection load {shlex.quote(self.main_file)}; fi\n'
                            'nmcli connection up uuid ' + self.main_uuid)
        finally:
            if self.added_interface: vm.cmd(['incus', 'config', 'device', 'remove', self.instance, 'second-interface'])
        vm.reset()


def main():
    selected = vm.target()
    if not selected.get('incus_instance'):
        print('SKIP networkmanager_connection_files.py: this VM suite needs Incus for a second card and independent observations', flush=True); return
    import incus
    distribution = next((name for name, image in incus.IMAGES.items() if image == selected.get('image')), '')
    if distribution not in SUPPORTED:
        print(f'SKIP networkmanager_connection_files.py: {distribution or selected.get("image")} is not one of the NetworkManager guests, Rocky Linux 9 and AlmaLinux 10', flush=True); return
    vm.up(); vm.reset(); suite = None; failure = None
    try:
        suite = Suite.__new__(Suite); suite.__init__(distribution, selected['incus_instance']); suite.run()
    except BaseException as error:
        failure = error; print('FAIL networkmanager_connection_files.py: ' + str(error), flush=True)
        if suite and getattr(suite, 'case_name', '') == 'setup': suite.diagnostics()
    finally:
        if suite:
            try: suite.cleanup()
            except Exception as error: print('FAIL cleanup: ' + str(error), flush=True); failure = failure or error
    if failure is not None: raise SystemExit(1)


if __name__ == '__main__':
    vm.run_suite(main)
