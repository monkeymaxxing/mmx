#!/usr/bin/env python3
"""The real networkd module: native rejection, devices, and autonomous restore.

Uses production binaries. Incus provides observations during network loss;
no MMX request is used to observe or trigger the timer's recovery.
"""
import contextlib
import json
import os
from pathlib import Path
import shlex
import shutil
import signal
import subprocess
import time

import vm
from owed_restores import Suite as RestoreObservations, instant

MODULE = 'systemd_networkd_file'
DIRECTORY = '/etc/systemd/network'
LINK_INDEXES_AND_NAMES = "ip -o link show | awk -F': ' '{print $1, $2}'"
FIXTURES = '/var/tmp/mmx-integration/systemd-networkd-files'
GATE_DIRECTORY = '/etc/systemd/system/mmx-activate-change-.service.d'
SUPPORTED = ('debian12', 'debian13')


def network(interface, settings):
    return f'[Match]\nName={interface}\n[Network]\n{settings}\n'


def netdev(name, kind, settings=''):
    return f'[NetDev]\nName={name}\nKind={kind}\n{settings}'


class Suite(RestoreObservations):
    # Reuse the foundation suite's bounded observations, diagnostics, fresh SSH,
    # and exact-process draining. Its constructor/build and adapter never run.
    def __init__(self, distribution, instance):
        self.distribution, self.instance = distribution, instance
        self.workspace = vm.ROOT / 'systemd-networkd-files'
        shutil.rmtree(self.workspace, ignore_errors=True)
        self.workspace.mkdir(parents=True)
        vm.build()
        self.node, self.ssh = vm.connection()
        self.binaries, self.mmx = vm.ROOT / 'bin', vm.ROOT / 'bin/mmx'
        self.processes, self.ssh_units, self.passed = [], [], []
        self.case_name, self.case_directory = 'setup', self.workspace
        self.destination, self.last_output, self.last_record = '', '', None
        self.old_records = set()
        self.added_interface = False
        self.networkctl_mode = None
        self.networkctl_path = '/usr/bin/networkctl'
        self.remote(f'mkdir -p {FIXTURES}')
        routes = json.loads(self.remote('ip -j -4 route show default').stdout)
        self.require(routes, 'a main card with an IPv4 default route', routes)
        self.main = routes[0]['dev']
        self.initial_addresses = self.addresses(self.main)
        self.require(self.initial_addresses, 'a main card IPv4 address', self.initial_addresses)
        main_index = self.remote(f'cat /sys/class/net/{self.main}/ifindex').stdout.strip()
        properties = self.remote(f'cat /run/systemd/netif/links/{main_index}').stdout
        self.require('NETWORK_FILE=' in properties, 'networkd manages the main card', properties)
        self.main_file = next(line.split('=', 1)[1] for line in properties.splitlines() if line.startswith('NETWORK_FILE='))
        self.require('00-mmx-main.network' < Path(self.main_file).name,
                     'the suite main-card file sorts before the distribution file', self.main_file)
        before = set(self.remote('ls /sys/class/net').stdout.split())
        vm.cmd(['incus', 'config', 'device', 'add', self.instance, 'second-interface',
                'nic', 'nictype=p2p', 'mtu=1500'])
        self.added_interface = True
        # udev renames a new card (eth0 becomes enp6s0); read the name after that.
        names = self.wait('one added second network card', lambda: set(self.remote('udevadm settle --timeout=5; ls /sys/class/net').stdout.split()) - before,
                          lambda names: len(names) == 1, seconds=10)
        self.second = names.pop()
        self.remote(f'ip link set {self.second} up')
        self.require(self.remote('systemctl is-active systemd-networkd.service').stdout.strip() == 'active',
                     'systemd-networkd is active')
        self.inventory('ssh_only')

    def remote(self, script, check=True, timeout=20):
        result = vm.cmd(['incus', 'exec', self.instance, '--', '/bin/sh', '-c', 'set -eu\n' + script],
                        check=False, timeout=timeout)
        if check:
            self.require(result.returncode == 0, f'node command exits 0: {script}',
                         (result.returncode, result.stdout, result.stderr))
        return result

    def all_records(self):
        output = self.remote('for file in /var/lib/mmx/owed_restores/*/owed_restore.json; do '
                             'test ! -f "$file" || { cat "$file"; echo; }; done').stdout
        # Records are durable, single JSON objects; parse even pretty-printed ones.
        decoder, records = json.JSONDecoder(), []
        while output.strip():
            output = output.lstrip()
            record, length = decoder.raw_decode(output)
            records.append(record)
            output = output[length:]
        return records

    def record(self):
        records = [r for r in self.all_records() if r['owed_restore'].get('destination_file', '') == self.destination
                   and r['owed_restore']['owed_restore_id'] not in self.old_records]
        self.require(len(records) <= 1, 'at most one new record for the current change', records)
        if records:
            self.last_record = records[0]
            (self.case_directory / 'last-observed-owed-restore.json').write_text(json.dumps(records[0], indent=2))
            return records[0]
        return None

    def diagnostics(self):
        try:
            record = self.record() or self.last_record
        except Exception as error:
            record = self.last_record
            print('RECORD READ FAILED ' + str(error), flush=True)
        print('CASE ' + self.case_name, flush=True)
        print('LAST CONTROLLER OUTPUT ' + json.dumps(self.last_output[-6000:]), flush=True)
        # A grouped run can stop at an earlier file than the last task's file.
        try:
            records = [r for r in self.all_records() if r['owed_restore']['owed_restore_id'] not in self.old_records]
            if not record and records:
                record = next((r for r in records if r['change_phase'] != 'change_accepted'), records[-1])
            print('RECORDS OF THIS RUN ' + json.dumps(records), flush=True)
        except Exception as error:
            print('RECORD ENUMERATION FAILED ' + str(error), flush=True)
        print('CURRENT RECORD ' + json.dumps(record), flush=True)
        units = ([record['owed_restore']['restore_service_unit'],
                  'mmx-activate-change-' + record['owed_restore']['owed_restore_id'] + '.service',
                  'mmx-owed-restore-' + record['owed_restore']['owed_restore_id'] + '.timer'] if record else [])
        script = ''
        for unit in units:
            script += f'echo {unit}; systemctl show {unit} -p ActiveState -p SubState -p Result -p ControlPID -p MainPID -p After -p NextElapseUSecRealtime; '
        for unit in units[:2]:
            script += f'journalctl --no-pager -n 8 -u {unit} -o short-precise; '
        script += 'ip -brief address; ip route; networkctl --no-pager list; journalctl --no-pager -u systemd-networkd.service -n 12 -o short-precise'
        try:
            output = self.remote(script, check=False)
            evidence = output.stdout + output.stderr
        except Exception as error:
            evidence = str(error)
        print('UNITS, SERVICE JOURNALS, AND NETWORK ' + json.dumps(evidence), flush=True)
        (self.case_directory / 'failure-diagnostics.txt').write_text(json.dumps({'record': record, 'controller': self.last_output, 'node': evidence}, indent=2))

    def inventory(self, connection):
        vm.write_inventory(self.workspace, {'nodes': {'web01': dict(self.node, connection=connection)}}, name='example')

    def background(self):
        process = subprocess.Popen([str(self.mmx), 'run', 'configure', '--json'], cwd=self.workspace,
                                   env=vm.ENV, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True,
                                   start_new_session=True)
        self.processes.append(process)
        return process

    @contextlib.contextmanager
    def case(self, name):
        self.case_name = name
        self.case_directory = self.workspace / name
        self.case_directory.mkdir(parents=True, exist_ok=True)
        self.last_output, self.last_record = '', None
        started = time.monotonic()
        try:
            yield
        except BaseException as error:
            print(f'FAIL {name}: {error}', flush=True)
            self.diagnostics()
            raise
        else:
            self.passed.append(name)
            print(f'PASS {name}: {time.monotonic() - started:.1f} seconds', flush=True)

    def task(self, name, content=None, duration='20s'):
        fields = {'name': name, 'restore_unless_controller_confirms_within': duration}
        fields.update({'content': content} if content is not None else {'state': 'absent'})
        return {MODULE: fields}

    def project(self, tasks):
        self.old_records = {r['owed_restore']['owed_restore_id'] for r in self.all_records()}
        self.destination = DIRECTORY + '/' + tasks[-1][MODULE]['name']
        self.last_record = None
        vm.write_project(self.workspace, project_name='systemd_networkd_files', fleet_stages=[
            {'id': 'network', 'nodes': 'all', 'node_tasks': tasks}])

    def apply(self, name, content=None, expected=0, change='changed', command='run'):
        self.project([self.task(name, content)])
        result = self.command(command, expected=expected)
        task = result['entries'][0]['response']['results'][-1]
        self.require(task['change'] == change, f'{name}: task reports {change}', task)
        if command == 'run' and expected == 0 and change == 'changed':
            record = self.clean_change()
            self.require(record['change_phase'] == 'change_accepted', f'{name}: accepted change', record)
        return result

    def apply_many(self, tasks):
        # One node job still confirms and records every file separately.
        self.project(tasks)
        result = self.command('run')
        results = result['entries'][0]['response']['results']
        self.require(len(results) == len(tasks), 'one result for every grouped file task', results)
        for task, outcome in zip(tasks, results):
            self.require(outcome['status'] == 'succeeded' and outcome['change'] == 'changed',
                         task[MODULE]['name'] + ': grouped task succeeded and changed', outcome)
        destinations = {DIRECTORY + '/' + task[MODULE]['name'] for task in tasks}
        records = self.wait('every grouped file is accepted and cleaned up',
                            lambda: [record for record in self.all_records()
                                     if record['owed_restore']['owed_restore_id'] not in self.old_records],
                            lambda records: len(records) == len(tasks) and all(
                                record['change_phase'] == 'change_accepted' and
                                record.get('owed_restore_cleanup_complete') for record in records))
        self.require({record['owed_restore']['destination_file'] for record in records} == destinations,
                     'accepted records name exactly the grouped files', records)
        self.check_cleanup_paths(records)
        return result

    def check_cleanup_paths(self, records):
        paths = []
        for record in records:
            change = record['owed_restore']['owed_restore_id']
            paths.extend([
                f'/etc/systemd/system/mmx-owed-restore-{change}.timer',
                f'/etc/systemd/system/mmx-owed-restore-{change}.service',
                f'/etc/systemd/system/mmx-activate-change-{change}.service',
                f'/etc/systemd/system/timers.target.wants/mmx-owed-restore-{change}.timer',
                f'/etc/systemd/system/multi-user.target.wants/mmx-owed-restore-{change}.service',
                f'/var/lib/mmx/owed_restores/{change}/previous_file_contents'])
        # One Incus request checks every path; a failure still names that path.
        output = self.remote('for path in ' + ' '.join(shlex.quote(path) for path in paths) +
                             '; do if test -e "$path" || test -L "$path"; then '
                             'echo "cleanup left $path"; exit 1; fi; done', check=False)
        self.require(output.returncode == 0, 'cleanup removed all units and saved contents', output.stdout + output.stderr)

    def clean_change(self):
        record = self.wait('resolved record with completed cleanup', self.record,
                           lambda record: record and record.get('owed_restore_cleanup_complete'))
        self.check_cleanup_paths([record])
        return record

    def command(self, command, run_id=None, expected=0, human=False):
        args = [self.mmx, command, *(vm.playbook_args(command) if command in ('run', 'dryrun') else [])]
        if run_id:
            args.append(run_id)
        if not human:
            args.append('--json')
        output = vm.cmd(args, cwd=self.workspace, check=False, timeout=100)
        self.last_output = output.stdout + '\n' + output.stderr
        allowed = expected if isinstance(expected, tuple) else (expected,)
        self.require(output.returncode in allowed, f'controller exits {allowed}', (output.returncode, self.last_output))
        return output.stdout if human else json.loads(output.stdout)

    def addresses(self, device):
        result = self.remote(f'ip -j -4 address show dev {device}', check=False)
        if result.returncode:
            return []
        return sorted(entry['local'] for link in json.loads(result.stdout) for entry in link.get('addr_info', []))

    def address(self, device, address, present=True):
        self.wait(f'{device}: {address} is ' + ('present' if present else 'absent'),
                  lambda: self.addresses(device), lambda addresses: (address in addresses) == present, seconds=10, interval=0.2)

    def device(self, name, present=True):
        self.wait(f'device {name} is ' + ('present' if present else 'absent'),
                  lambda: self.remote(f'test -e /sys/class/net/{name}', check=False).returncode,
                  lambda code: (code == 0) == present, seconds=10, interval=0.2)

    def journal_cursor(self):
        output = self.remote('journalctl --no-pager -n 1 -o cat --show-cursor').stdout
        return next(line.removeprefix('-- cursor: ') for line in output.splitlines() if line.startswith('-- cursor: '))

    def refuse(self, name, content, reason):
        destination = DIRECTORY + '/' + name
        before = self.remote(f'if test -f {destination}; then sha256sum {destination}; stat -c "%u %g %a %y" {destination}; fi').stdout
        # Index and name only: the detailed listing of a bridge holds running timers.
        devices = self.remote(LINK_INDEXES_AND_NAMES).stdout
        records = self.all_records()
        units = self.remote('find /etc/systemd/system -name "mmx-*restore-*" -o -name "mmx-activate-change-*"').stdout
        cursor = self.journal_cursor()
        result = self.apply(name, content, expected=1, change='unchanged')
        task = result['entries'][0]['response']['results'][-1]
        self.require(reason in task.get('reason', ''), f'refusal names {reason}', task)
        after = self.remote(f'if test -f {destination}; then sha256sum {destination}; stat -c "%u %g %a %y" {destination}; fi').stdout
        devices_after = self.remote(LINK_INDEXES_AND_NAMES).stdout
        self.require(before == after and devices == devices_after, 'refused task changes neither file nor devices',
                     {'before': before, 'after': after, 'devices_before': devices, 'devices_after': devices_after})
        self.require(records == self.all_records() and self.record() is None,
                     'refused task writes no record', self.last_record)
        self.require(units == self.remote('find /etc/systemd/system -name "mmx-*restore-*" -o -name "mmx-activate-change-*"').stdout,
                     'refused task writes no unit')
        journal = self.remote('journalctl --no-pager -u systemd-networkd.service --cursor ' + shlex.quote(cursor) +
                              ' -o json --output-fields=__CURSOR,MESSAGE').stdout
        entries = [json.loads(line) for line in journal.splitlines()]
        entries = [entry for entry in entries if entry['__CURSOR'] != cursor]
        self.require(not entries, 'refused task does not make networkd log a change', entries)

    def restored(self, result, task=True):
        response = result['entries'][0]['response']
        self.require(response['status'] == 'failed', 'restored node job remains failed', response)
        outcomes = response.get('owed_restores', [])
        self.require(outcomes and outcomes[-1]['restore_status'] == 'succeeded', 'successful restore in node response', response)
        if task:
            outcome = response['results'][-1]
            self.require(outcome['status'] == 'failed' and outcome['change'] == 'changed', 'failed and changed task', outcome)
        self.require(self.file_state() == self.before, 'previous file bytes, owner, group, mode and modification time (or absence)',
                     {'before': self.before, 'after': self.file_state()})
        self.clean_change()
        return outcomes[-1]

    def after_wait(self):
        return self.wait('after-change confirmation wait', self.record,
                         lambda r: r and r['change_phase'] == 'waiting_after_change', seconds=10, interval=0.05)

    def main_restored(self):
        self.wait('original main-card addresses and default route return',
                  lambda: (self.addresses(self.main), self.remote('ip -4 route show default').stdout),
                  lambda state: state[0] == self.initial_addresses and f'dev {self.main}' in state[1], seconds=15, interval=0.2)
        self.require(self.fresh_ssh().returncode == 0, 'new SSH works after the restore')

    def previous_main(self, exists):
        path = DIRECTORY + '/00-mmx-main.network'
        if exists:
            contents = network(self.main, 'DHCP=ipv4')
            self.remote(f'printf %s {shlex.quote(contents)} > {path}\nchown 1:systemd-network {path}\n'
                        f'chmod 0640 {path}\ntouch -d "2001-01-02 03:04:05 UTC" {path}\nnetworkctl reload')
        else:
            self.remote(f'rm -f {path}\nnetworkctl reload')
        self.main_restored()
        self.destination = path
        self.before = self.file_state()

    @contextlib.contextmanager
    def second_card_for_restore(self):
        properties_command = (f'index=$(cat /sys/class/net/{self.second}/ifindex)\n'
                              'if test -f /run/systemd/netif/links/$index; then '
                              'cat /run/systemd/netif/links/$index; fi')
        properties = self.remote(properties_command).stdout
        selected_file = next((line.split('=', 1)[1] for line in properties.splitlines()
                              if line.startswith('NETWORK_FILE=')), '')
        if selected_file != self.main_file:
            yield
            return
        # The disconnected test card cannot obtain DHCP from the main card's
        # distribution file. Keep it out of the restore's online wait, at boot too.
        path = DIRECTORY + '/00-mmx-restore-second.network'
        self.require(Path(path).name < Path(selected_file).name,
                     'the restore second-card file sorts before the distribution file', selected_file)
        contents = (f'[Match]\nName={self.second}\n[Link]\nRequiredForOnline=no\n'
                    '[Network]\nDHCP=no\nAddress=10.0.9.5/24\nConfigureWithoutCarrier=yes\n')
        print(f'SETUP restore: {selected_file} manages {self.second}; using {path} during this case', flush=True)
        try:
            self.remote(f'printf %s {shlex.quote(contents)} > {path}\nchmod 0644 {path}\nnetworkctl reload')
            self.wait('the static second-card restore fixture finishes configuring',
                      lambda: self.remote(properties_command).stdout,
                      lambda value: f'NETWORK_FILE={path}' in value.splitlines()
                      and 'ADMIN_STATE=configured' in value.splitlines(), seconds=10, interval=0.2)
            yield
        finally:
            self.remote(f'rm -f {path}\nnetworkctl reload')

    def run(self):
        second_file = '00-mmx-second.network'
        second = network(self.second, 'DHCP=no\nAddress=10.0.0.5/24')
        with self.case('second-card-dryrun-present-unchanged-changed-and-absent'):
            self.apply(second_file, second, expected=(0, 2, 3), change='would_change', command='dryrun')
            self.require(self.record() is None, 'dryrun creates no restore record')
            units = self.remote('find /etc/systemd/system -name "mmx-owed-restore-*" -o -name "mmx-activate-change-*"').stdout
            self.require(not units, 'dryrun creates no unit', units)
            self.require(self.remote(f'test ! -e {DIRECTORY}/{second_file}', check=False).returncode == 0, 'dryrun creates no file')
            self.apply(second_file, second)
            metadata = self.remote(f'stat -c "%a %U %G" {DIRECTORY}/{second_file}').stdout.strip()
            self.require(metadata == '644 root root', 'fixed .network mode and ownership', metadata)
            self.require(self.remote(f'cat {DIRECTORY}/{second_file}').stdout == second, 'installed network file bytes')
            self.address(self.second, '10.0.0.5')
            self.apply(second_file, second, change='unchanged')
            self.require(self.record() is None, 'unchanged creates no record')
            self.inventory('https_with_ssh_fallback')
            self.apply(second_file, second.replace('10.0.0.5', '10.0.0.6'))
            self.require(sorted(self.record()['after_change_confirmed_connection_kinds']) == ['https', 'ssh'], 'both kinds confirm the real module change', self.last_record)
            self.inventory('ssh_only')
            self.address(self.second, '10.0.0.6'); self.address(self.second, '10.0.0.5', False)
            self.apply(second_file)
            self.address(self.second, '10.0.0.6', False)

        with self.case('native-rejection-of-every-measured-mistake'), self.second_card_for_restore():
            bad_network = [
                ('unknown-key', second + 'DefinitelyUnknownKey=yes\n'),
                ('bad-address', second.replace('10.0.0.5/24', 'not-an-address')),
                ('assignment-outside-section', 'Address=10.0.0.5/24\n' + second),
                ('unknown-section', second + '[DefinitelyUnknownSection]\n'),
                ('empty-match', '[Match]\n[Network]\nDHCP=no\n')]
            bad_netdev = [
                ('unknown-key', netdev('mmxbad0', 'dummy') + 'DefinitelyUnknownKey=yes\n'),
                ('unknown-kind', netdev('mmxbad0', 'definitely_unknown_kind')),
                ('missing-name', '[NetDev]\nKind=dummy\n'),
                ('long-name', netdev('mmx_name_longer_than_15', 'dummy')),
                ('bad-vlan-id', netdev('mmxbad0', 'vlan', '[VLAN]\nId=99999\n'))]
            for extension, examples in [('network', bad_network), ('netdev', bad_netdev)]:
                for mistake, contents in examples:
                    self.case_name = f'native-rejection-{extension}-{mistake}'
                    name = f'99-mmx-invalid-{mistake}.{extension}'
                    self.destination = DIRECTORY + '/' + name
                    self.before = self.file_state()
                    failed = self.apply(name, contents, expected=1)
                    self.restored(failed)
                    self.require('systemd-networkd did not accept ' + self.destination in self.last_output,
                                 f'{extension} {mistake}: native rejection names this file', self.last_output)
                    self.device('mmxbad0', False)

        with self.case('devices-order-refusals-and-reverse-removal'):
            bridge = netdev('mmxbr0', 'bridge')
            self.apply('20-mmx-bridge.netdev', bridge); self.device('mmxbr0')
            metadata = self.remote(f'stat -c "%a %U %G" {DIRECTORY}/20-mmx-bridge.netdev').stdout.strip()
            self.require(metadata == '640 root systemd-network', 'fixed .netdev mode and ownership', metadata)
            self.remote(f'chmod 0644 {DIRECTORY}/20-mmx-bridge.netdev')
            bridge = '# A comment changes no device setting.\n' + bridge
            self.apply('20-mmx-bridge.netdev', bridge)
            self.device('mmxbr0')
            self.apply('21-mmx-bridge.network', network('mmxbr0', 'Address=10.0.8.5/24\nConfigureWithoutCarrier=yes'))
            self.address('mmxbr0', '10.0.8.5')
            self.refuse('20-mmx-bridge.netdev', bridge + '[Bridge]\nSTP=yes\n', 'Cannot change')
            self.refuse('22-mmx-existing.netdev', bridge, 'already exists and was not created by the .netdev file')
            self.apply(second_file, network(self.second, 'Bridge=mmxbr0'))
            self.wait('second card is a bridge port', lambda: self.remote(f'ip -o link show {self.second}').stdout,
                      lambda output: 'master mmxbr0' in output, seconds=10)
            self.refuse('20-mmx-bridge.netdev', None, 'still refers to the device (Bridge=mmxbr0)')
            self.apply_many([self.task(second_file), self.task('21-mmx-bridge.network'), self.task('20-mmx-bridge.netdev')])
            self.device('mmxbr0', False)
            self.apply_many([
                self.task('20-mmx-vlan.netdev', netdev('mmxvlan0', 'vlan', '[VLAN]\nId=71\n')),
                self.task(second_file, network(self.second, 'VLAN=mmxvlan0')),
                self.task('21-mmx-vlan.network', network('mmxvlan0', 'Address=10.0.71.5/24'))])
            self.device('mmxvlan0'); self.address('mmxvlan0', '10.0.71.5')
            self.apply_many([self.task('21-mmx-vlan.network'), self.task(second_file), self.task('20-mmx-vlan.netdev')])
            self.device('mmxvlan0', False)
            devices = [('bond', 'mmxbond0'), ('dummy', 'mmxdummy0'), ('veth', 'mmxveth0')]
            self.apply_many([self.task('20-mmx-' + kind + '.netdev', netdev(device, kind,
                                      '[Peer]\nName=mmxveth1\n' if kind == 'veth' else ''))
                             for kind, device in devices])
            for _, device in devices: self.device(device)
            self.device('mmxveth1')
            self.apply_many([self.task('20-mmx-' + kind + '.netdev') for kind, _ in reversed(devices)])
            for _, device in devices: self.device(device, False)
            self.device('mmxveth1', False)
            self.apply('20-mmx-rename.netdev', netdev('mmxbr0', 'bridge')); self.device('mmxbr0')
            self.apply('20-mmx-rename.netdev', netdev('mmxbr1', 'bridge'))
            self.device('mmxbr1'); self.device('mmxbr0', False)
            self.apply('20-mmx-rename.netdev'); self.device('mmxbr1', False)

        with self.case('timer-recreates-removed-bridge-and-address-without-controller-or-worker'), self.second_card_for_restore():
            self.apply_many([
                self.task('20-mmx-recreate.netdev', netdev('mmxbr0', 'bridge')),
                self.task('21-mmx-recreate.network', network('mmxbr0', 'Address=10.0.8.5/24\nConfigureWithoutCarrier=yes'))])
            self.address('mmxbr0', '10.0.8.5')
            # A native systemd ExecStartPre gate holds activation until all
            # controller request processes have drained. No product hook exists.
            gate_text = "[Service]\nExecStartPre=/bin/sh -c 'while ! test -f " + FIXTURES + "/release_change_activation; do sleep 0.05; done'\n"
            self.remote(f'mkdir -p {GATE_DIRECTORY}\nrm -f {FIXTURES}/release_change_activation\n'
                        f'printf %s {shlex.quote(gate_text)} > {GATE_DIRECTORY}/10-wait-for-controller-requests-to-end.conf\nsystemctl daemon-reload')
            self.project([self.task('20-mmx-recreate.netdev', duration='15s')])
            self.before = self.file_state()
            process = self.background()
            record = self.wait('activation service is held at the native systemd gate', self.record,
                               lambda r: r and r['change_phase'] == 'activating_config', seconds=10)
            activation_unit = 'mmx-activate-change-' + record['owed_restore']['owed_restore_id'] + '.service'
            gate = self.wait('systemd starts the activation gate',
                             lambda: self.remote(f'systemctl show {activation_unit} -p ControlPID --value').stdout.strip(),
                             lambda pid: pid.isdigit() and int(pid) > 0, seconds=5, interval=0.05)
            command = self.remote(f'tr \"\\0\" \" \" < /proc/{gate}/cmdline').stdout
            self.require(FIXTURES + '/release_change_activation' in command, 'native systemd gate holds activation before confirmation', command)
            self.stop_controller_and_drain_requests(process)
            saved = self.command('results')
            self.remote(f'touch {FIXTURES}/release_change_activation')
            record = self.after_wait()
            self.device('mmxbr0', False)
            self.kill_worker(record)
            self.wait('timer restores the bridge without any MMX request', self.record,
                      lambda r: r and r['owed_restore']['restore_status'] == 'succeeded', seconds=30)
            self.device('mmxbr0'); self.address('mmxbr0', '10.0.8.5')
            self.restored(self.command('status', saved['id']), task=False)
            self.remote(f'rm -rf {GATE_DIRECTORY}\nsystemctl daemon-reload')
            self.apply_many([self.task('21-mmx-recreate.network'), self.task('20-mmx-recreate.netdev')])

        for case, previous in [('cut-with-previous-file', True), ('cut-with-no-previous-file', False),
                               ('cut-worker-killed-timer-alone', True), ('cut-restart-before-deadline', True),
                               ('cut-failed-restore-and-administrator-repair', True)]:
            with self.case(case), self.second_card_for_restore():
                self.previous_main(previous)
                self.project([self.task('00-mmx-main.network', network(self.main, 'DHCP=no\nAddress=192.0.2.5/24'),
                                        duration='2m' if 'restart' in case else ('20s' if 'worker-killed' in case else '12s'))])
                process = self.background()
                record = self.after_wait()
                self.address(self.main, '192.0.2.5')
                self.require(not any(address in self.addresses(self.main) for address in self.initial_addresses),
                             'cut removed the original DHCP address', self.addresses(self.main))
                self.require(not self.remote('ip -4 route show default').stdout, 'cut removed the default route')
                self.require(self.fresh_ssh().returncode != 0, 'cut refuses new SSH connections')
                interrupted = 'worker-killed' in case or 'restart' in case
                if interrupted:
                    self.stop_controller_and_drain_requests(process)
                    saved = self.command('results')
                    if 'worker-killed' in case:
                        self.kill_worker(record)
                    else:
                        self.remote('systemd-run --on-active=1s --timer-property=AccuracySec=100ms /usr/bin/systemctl reboot')
                        vm.await_boot(record['node_boot_id_before_change'])
                        self.node, self.ssh = vm.connection(); self.inventory('ssh_only')
                if 'failed-restore' in case:
                    self.networkctl_path = self.remote('command -v networkctl').stdout.strip()
                    self.networkctl_mode = self.remote('stat -c %a ' + self.networkctl_path).stdout.strip()
                    self.remote('chmod 000 ' + self.networkctl_path)
                    failed_record = self.wait('native reload failure stays owed', self.record,
                                             lambda r: r and r['change_phase'] == 'restore_failed', seconds=35)
                    saved_contents = '/var/lib/mmx/owed_restores/' + failed_record['owed_restore']['owed_restore_id'] + '/previous_file_contents'
                    self.require(self.remote(f'test -f {saved_contents}', check=False).returncode == 0, 'failed restore retains the saved previous contents', failed_record)
                    self.require(failed_record['owed_restore']['previous_file_restored'] and
                                 not failed_record['owed_restore']['previous_config_activated'],
                                 'failed reload does not claim the previous configuration was activated', failed_record)
                    self.remote(f'chmod {self.networkctl_mode} {self.networkctl_path}')
                    self.networkctl_mode = None
                    self.remote('systemctl start ' + failed_record['owed_restore']['restore_service_unit'])
                restored = self.wait('restore completes before another MMX request', self.record,
                                     lambda r: r and r['owed_restore']['restore_status'] == 'succeeded', seconds=35)
                self.require(not restored['after_change_confirmed_connection_kinds'], 'no after-change confirmation during cut', restored)
                if 'restart' in case:
                    now = self.node_time()
                    self.require(now < instant(record['restore_deadline']),
                                 'restore completed on the new boot before the deadline; reboot consumed the allowance',
                                 {'node_time': now, 'deadline': record['restore_deadline']})
                self.main_restored()
                if interrupted:
                    result = self.command('status', saved['id'])
                else:
                    result = self.collect(process, 1)
                    # A repaired failed task retains its original failure; status
                    # refreshes the restore outcome that may have arrived later.
                    result = self.command('status', result['id'])
                self.restored(result, task=not interrupted)
                output = self.command('status', result['id'], human=True)
                self.require(self.destination + ' was restored to its previous state' in output,
                             'human output states the restored state', output)
                # The next fixture or final cleanup replaces this restored file.
        print(json.dumps({'passed': self.passed, 'count': len(self.passed)}, indent=2), flush=True)

    def kill_worker(self, record):
        # Match a complete runner argv, never a name pattern used to end processes.
        job = record['node_job_id']
        output = self.remote("for path in /proc/[0-9]*/cmdline; do "
                             "args=$(tr '\\0' ' ' < \"$path\" 2>/dev/null) || continue; "
                             f"case \"$args\" in '/opt/mmx/bin/'*'/mmx-runner worker {job} ') "
                             "echo \"${path%/cmdline}\";; esac; done").stdout.splitlines()
        self.require(len(output) == 1, 'one exact node job worker process', output)
        pid = Path(output[0]).name
        self.remote(f'kill -KILL {pid}')
        self.wait('worker is dead before the timer deadline', lambda: self.remote(f'test -e /proc/{pid}/exe', check=False).returncode,
                  lambda code: code == 1, seconds=3)
        self.require(self.node_time() < instant(record['restore_deadline']), 'worker dies before the restore deadline', record)
        self.require(not self.node_request_processes(), 'controller request processes are drained before timer recovery')

    def cleanup(self):
        try:
            for process in self.processes:
                if process.poll() is None:
                    os.killpg(process.pid, signal.SIGKILL); process.communicate(timeout=5)
            if self.networkctl_mode:
                self.remote(f'chmod {self.networkctl_mode} {self.networkctl_path}')
            self.remote(f'rm -rf {GATE_DIRECTORY}\n'
                        'for unit in /etc/systemd/system/mmx-activate-change-*.service /etc/systemd/system/mmx-owed-restore-*.timer /etc/systemd/system/mmx-owed-restore-*.service; do '
                        'test ! -f "$unit" || systemctl disable --now "${unit##*/}"; done\n'
                        f'rm -f {DIRECTORY}/*mmx-*.network {DIRECTORY}/*mmx-*.netdev\n'
                        'rm -f /etc/systemd/system/mmx-activate-change-*.service /etc/systemd/system/mmx-owed-restore-*.service /etc/systemd/system/mmx-owed-restore-*.timer\n'
                        'networkctl reload\nsystemctl daemon-reload\nsystemctl reset-failed')
            for name in ['mmxbr0', 'mmxbr1', 'mmxbond0', 'mmxdummy0', 'mmxveth0', 'mmxveth1', 'mmxvlan0', 'mmxbad0']:
                self.remote(f'if test -e /sys/class/net/{name}; then networkctl delete {name}; fi')
            self.main_restored()
        finally:
            if self.added_interface:
                vm.cmd(['incus', 'config', 'device', 'remove', self.instance, 'second-interface'])
        vm.reset()


def main():
    selected = vm.target()
    if not selected.get('incus_instance'):
        print('SKIP systemd_networkd_files.py: this VM suite needs Incus for a second card and observations while the main card is cut off', flush=True)
        return
    import incus
    distribution = next((name for name, image in incus.IMAGES.items() if image == selected.get('image')), '')
    if distribution not in SUPPORTED:
        print(f'SKIP systemd_networkd_files.py: {distribution or selected.get("image")} is not one of the systemd-networkd guests, Debian 12 and 13', flush=True)
        return
    vm.up(); vm.reset()
    suite = None
    failure = None
    try:
        suite = Suite.__new__(Suite)
        suite.__init__(distribution, selected['incus_instance'])
        suite.run()
    except BaseException as error:
        failure = error
        print('FAIL systemd_networkd_files.py: ' + str(error), flush=True)
        if suite and getattr(suite, 'case_name', '') == 'setup': suite.diagnostics()
    finally:
        if suite:
            try: suite.cleanup()
            except Exception as error:
                print('FAIL cleanup: ' + str(error), flush=True)
                failure = failure or error
    if failure is not None:
        raise SystemExit(1)


if __name__ == '__main__':
    vm.run_suite(main)
