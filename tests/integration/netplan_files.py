#!/usr/bin/env python3
"""Production Netplan file changes and autonomous recovery on two Ubuntu VMs."""
import contextlib
import copy
import json
import os
from pathlib import Path
import shlex
import shutil
import signal

import vm
from owed_restores import instant
from systemd_networkd_files import Suite as NetworkdObservations, GATE_DIRECTORY, LINK_INDEXES_AND_NAMES

MODULE = 'netplan_file'
DIRECTORY = '/etc/netplan'
FIXTURES = '/var/tmp/mmx-integration/netplan-files'
SUPPORTED = ('ubuntu2404', 'ubuntu2604')


def configuration(**devices):
    # JSON is also YAML. Netplan, not the harness, interprets the settings.
    return json.dumps({'network': {'version': 2, 'renderer': 'networkd', **devices}}, indent=2) + '\n'


class Suite(NetworkdObservations):
    # Share observations and exact-process draining, not networkd's fixtures,
    # file changes, or recovery policy. No test-only module is built or used.
    def __init__(self, distribution, instance):
        self.distribution, self.instance = distribution, instance
        self.workspace = vm.ROOT / 'netplan-files'
        shutil.rmtree(self.workspace, ignore_errors=True)
        self.workspace.mkdir(parents=True)
        vm.build()
        self.connect()
        self.binaries, self.mmx = vm.ROOT / 'bin', vm.ROOT / 'bin/mmx'
        self.processes, self.ssh_units, self.passed = [], [], []
        self.case_name, self.case_directory = 'setup', self.workspace
        self.destination, self.last_output, self.last_record = '', '', None
        self.old_records = set()
        self.added_interface, self.saved_main = False, False
        self.netplan_mode, self.netplan_path = None, ''
        self.monitor_unit = None
        self.remote(f'mkdir -p {FIXTURES}')
        routes = json.loads(self.remote('ip -j -4 route show default').stdout)
        self.require(routes, 'a main card with an IPv4 default route', routes)
        self.main = routes[0]['dev']
        self.initial_addresses = self.addresses(self.main)
        self.require(self.initial_addresses, 'the main card has an IPv4 address')
        self.main_file = DIRECTORY + '/50-cloud-init.yaml'
        self.remote(f'cp -a {self.main_file} {FIXTURES}/original-main.yaml')
        self.saved_main = True
        before = set(self.remote('ls /sys/class/net').stdout.split())
        vm.cmd(['incus', 'config', 'device', 'add', self.instance, 'second-interface',
                'nic', 'nictype=p2p', 'mtu=1500'])
        self.added_interface = True
        names = self.wait('one added second network card',
                          lambda: set(self.remote('udevadm settle --timeout=5; ls /sys/class/net').stdout.split()) - before,
                          lambda names: len(names) == 1, seconds=10)
        self.second = names.pop()
        self.remote(f'ip link set {self.second} up')
        self.require(self.remote('systemctl is-active systemd-networkd.service').stdout.strip() == 'active',
                     'systemd-networkd is active')
        self.inventory('ssh_only')

    def task(self, name, content=None, duration='20s'):
        fields = {'name': name, 'restore_unless_controller_confirms_within': duration}
        fields.update({'content': content} if content is not None else {'state': 'absent'})
        return {MODULE: fields}

    def project(self, tasks):
        self.old_records = {r['owed_restore']['owed_restore_id'] for r in self.all_records()}
        self.destination = DIRECTORY + '/' + tasks[-1][MODULE]['name']
        self.last_record = None
        vm.write_project(self.workspace, project_name='netplan_files', fleet_stages=[
            {'id': 'network', 'nodes': 'all', 'node_tasks': tasks}])

    def second_configuration(self, address='10.0.0.5/24'):
        return configuration(ethernets={self.second: {'dhcp4': False, 'optional': True, 'addresses': [address]}})

    def bridge_configuration(self):
        return configuration(ethernets={self.second: {'dhcp4': False, 'optional': True}},
                             bridges={'mmxbr0': {'interfaces': [self.second], 'addresses': ['10.0.8.5/24'],
                                                'ignore-carrier': True, 'optional': True, 'parameters': {'stp': False}}})

    def port(self, attached=True):
        self.wait('the second card ' + ('belongs to the bridge' if attached else 'has no master'),
                  lambda: self.remote(f'ip -j link show dev {self.second}').stdout,
                  lambda output: (json.loads(output)[0].get('master') == 'mmxbr0') if attached
                  else not json.loads(output)[0].get('master'), seconds=10, interval=0.2)

    def no_global_configuration(self):
        def remaining():
            return self.remote(f'ip -o address show dev {self.second} scope global\n'
                               f'ip -4 route show table all dev {self.second}\n'
                               f"ip -6 route show dev {self.second} | grep -v '^fe80::/64 ' || true").stdout
        # The kernel's own IPv6 routes of a link that is up are not the
        # file's configuration: its link-local network, and the multicast
        # route in the table `local`. So IPv6 is read from the main table.
        self.wait('the second card has no global address or route', remaining,
                  lambda output: not output.strip(), seconds=10, interval=0.2)

    def generated_contents(self):
        return self.remote('find /run/systemd/network -maxdepth 1 -type f -name "10-netplan-*" '
                           '-exec sha256sum {} + | sort').stdout

    def refuse(self, name, content, reason):
        self.destination = DIRECTORY + '/' + name
        before = self.file_state()
        generated = self.generated_contents()
        devices = self.remote(LINK_INDEXES_AND_NAMES).stdout
        records = self.all_records()
        units_command = 'find /etc/systemd/system -name "mmx-*restore-*" -o -name "mmx-activate-change-*" | sort'
        units = self.remote(units_command).stdout
        result = self.apply(name, content, expected=1, change='unchanged')
        task = result['entries'][0]['response']['results'][-1]
        text = task.get('reason', '')
        self.require(reason in text and self.destination in text, 'refusal gives the native reason and live path', task)
        self.require('/private_task_directories/' not in text, 'refusal hides the temporary tree path', text)
        self.require(before == self.file_state(), 'refusal preserves live bytes and metadata', task)
        self.require(generated == self.generated_contents(), 'refusal preserves generated file names and bytes')
        self.require(devices == self.remote(LINK_INDEXES_AND_NAMES).stdout, 'refusal preserves all devices')
        self.require(records == self.all_records() and self.record() is None, 'refusal writes no restore record')
        self.require(units == self.remote(units_command).stdout, 'refusal writes no unit')

    @contextlib.contextmanager
    def address_monitor(self):
        self.monitor_unit = 'mmx-netplan-main-address-monitor'
        output = FIXTURES + '/main-address-events'
        self.remote(f'rm -f {output}\nsystemd-run --unit={self.monitor_unit} --property=StandardOutput=file:{output} '
                    f'$(command -v ip) -4 -o monitor address dev {self.main}')
        self.wait('the main-card address monitor is running',
                  lambda: self.remote(f'systemctl is-active {self.monitor_unit}.service', check=False).stdout.strip(),
                  lambda state: state == 'active', seconds=5)
        try:
            yield output
        finally:
            self.remote(f'systemctl stop {self.monitor_unit}.service')
            self.monitor_unit = None

    def previous_main(self, exists):
        self.remote(f'cp -a {FIXTURES}/original-main.yaml {self.main_file}\nrm -f {DIRECTORY}/99-mmx-cut.yaml')
        if exists:
            self.remote(f'chown 1:1 {self.main_file}\nchmod 0640 {self.main_file}\n'
                        f'touch -d "2001-01-02 03:04:05 UTC" {self.main_file}')
        self.remote('netplan apply')
        self.main_restored()
        self.destination = self.main_file if exists else DIRECTORY + '/99-mmx-cut.yaml'
        self.before = self.file_state()

    def run(self):
        second_file = '60-mmx-second-card.yaml'
        second_tree = json.loads(self.second_configuration())
        second_tree['network']['ethernets'][self.second]['link-local'] = []
        second_tree['network']['ethernets'][self.second]['accept-ra'] = False
        second = json.dumps(second_tree)
        with self.case('second-card-dryrun-present-unchanged-changed-and-absent'):
            self.apply(second_file, second, expected=(0, 2, 3), change='would_change', command='dryrun')
            self.require(self.record() is None, 'dryrun creates no restore record')
            self.require(not self.remote('find /etc/systemd/system -name "mmx-owed-restore-*" -o -name "mmx-activate-change-*"').stdout,
                         'dryrun creates no unit')
            self.require(self.remote(f'test ! -e {DIRECTORY}/{second_file}', check=False).returncode == 0,
                         'dryrun creates no file')
            self.apply(second_file, second)
            self.address(self.second, '10.0.0.5')
            self.require(self.remote(f'cat {DIRECTORY}/{second_file}').stdout == second, 'installed bytes match the complete file')
            self.require(self.remote(f'stat -c "%a %U %G" {DIRECTORY}/{second_file}').stdout.strip() == '600 root root',
                         'fixed Netplan file mode and ownership')
            with self.address_monitor() as events:
                self.apply(second_file, second, change='unchanged')
                self.require(not self.remote(f'cat {events}').stdout, 'unchanged causes no address event on the main card')
            self.require(self.record() is None, 'unchanged creates no record')
            self.inventory('https_with_ssh_fallback')
            self.apply(second_file, second.replace('10.0.0.5', '10.0.0.6'))
            self.require(sorted(self.record()['after_change_confirmed_connection_kinds']) == ['https', 'ssh'],
                         'both new connection kinds confirm the change', self.last_record)
            self.inventory('ssh_only')
            self.address(self.second, '10.0.0.6'); self.address(self.second, '10.0.0.5', False)
            self.apply(second_file)
            self.no_global_configuration()

        for mistake, contents, reason in [
                ('unknown-key', 'network: {version: 2, definitely_unknown_key: true}', "unknown key 'definitely_unknown_key'"),
                ('bad-address', self.second_configuration('not-an-address'), "address 'not-an-address'"),
                ('wrong-format', 'network: [wrong, format]', 'expected mapping')]:
            with self.case('native-rejection-' + mistake):
                self.refuse('60-mmx-invalid.yaml', contents, reason)

        with self.case('native-rejection-of-removing-a-referenced-bridge'):
            self.apply('60-mmx-bridge.yaml', self.bridge_configuration())
            self.apply('61-mmx-dependent-vlan.yaml', configuration(vlans={
                'mmxvlan0': {'id': 101, 'link': 'mmxbr0', 'optional': True}}))
            self.refuse('60-mmx-bridge.yaml', None, "interface 'mmxbr0' is not defined")
            self.apply('61-mmx-dependent-vlan.yaml')
            self.apply('60-mmx-bridge.yaml')
            self.device('mmxvlan0', False); self.device('mmxbr0', False); self.port(False)

        with self.case('devices-dropped-device-existing-device-refusals-and-absent'):
            tree = json.loads(self.bridge_configuration())
            tree['network'].update({
                'vlans': {'mmxvlan0': {'id': 101, 'link': 'mmxbr0', 'optional': True}},
                'bonds': {'mmxbond0': {'interfaces': [], 'optional': True, 'ignore-carrier': True,
                                     'parameters': {'mode': 'active-backup'}}},
                'dummy-devices': {'mmxdummy0': {'optional': True, 'ignore-carrier': True}},
                'virtual-ethernets': {'mmxveth0': {'peer': 'mmxveth1', 'optional': True},
                                     'mmxveth1': {'peer': 'mmxveth0', 'optional': True}}})
            name = '70-mmx-devices.yaml'
            self.apply(name, json.dumps(tree))
            devices = ['mmxbr0', 'mmxvlan0', 'mmxbond0', 'mmxdummy0', 'mmxveth0', 'mmxveth1']
            for device in devices: self.device(device)
            self.address('mmxbr0', '10.0.8.5'); self.port()
            indexes = self.remote(LINK_INDEXES_AND_NAMES).stdout
            del tree['network']['dummy-devices']
            self.apply(name, json.dumps(tree)); self.device('mmxdummy0', False)
            for device in devices:
                if device == 'mmxdummy0': continue
                self.device(device)
                before_line = next(line for line in indexes.splitlines() if line.split()[1].split('@')[0] == device)
                self.require(before_line in self.remote(LINK_INDEXES_AND_NAMES).stdout.splitlines(),
                             device + ' keeps its device index when another device is dropped')
            changed = copy.deepcopy(tree); changed['network']['bridges']['mmxbr0']['parameters']['stp'] = True
            self.refuse(name, json.dumps(changed), 'systemd-networkd applies the settings only when creating the device')
            changed = copy.deepcopy(tree); changed['network']['vlans']['mmxvlan0']['id'] = 102
            self.refuse(name, json.dumps(changed), 'systemd-networkd applies the settings only when creating the device')
            self.remote('ip link add mmxoutside0 type dummy')
            try:
                self.refuse('71-mmx-outsider.yaml', configuration(**{'dummy-devices': {'mmxoutside0': {}}}),
                            'already exists and was not created by Netplan')
            finally:
                self.remote('ip link delete mmxoutside0')
            self.apply(name)
            for device in devices: self.device(device, False)
            self.port(False)

        with self.case('timer-recreates-removed-bridge-address-and-port-without-controller'):
            name = '70-mmx-recreate.yaml'
            self.apply(name, self.bridge_configuration())
            self.address('mmxbr0', '10.0.8.5'); self.port()
            gate_text = "[Service]\nExecStartPre=/bin/sh -c 'while ! test -f " + FIXTURES + "/release_change_activation; do sleep 0.05; done'\n"
            self.remote(f'mkdir -p {GATE_DIRECTORY}\nrm -f {FIXTURES}/release_change_activation\n'
                        f'printf %s {shlex.quote(gate_text)} > {GATE_DIRECTORY}/10-netplan-wait.conf\nsystemctl daemon-reload')
            self.project([self.task(name, duration='20s')]); self.before = self.file_state()
            process = self.background()
            record = self.wait('activation is held before the controller can confirm it', self.record,
                               lambda r: r and r['change_phase'] == 'activating_config', seconds=10)
            unit = 'mmx-activate-change-' + record['owed_restore']['owed_restore_id'] + '.service'
            gate = self.wait('the activation gate is running',
                             lambda: self.remote(f'systemctl show {unit} -p ControlPID --value').stdout.strip(),
                             lambda pid: pid.isdigit() and int(pid) > 0, seconds=5)
            self.require(FIXTURES + '/release_change_activation' in
                         self.remote(f'tr "\\0" " " < /proc/{gate}/cmdline').stdout,
                         'the native gate holds activation')
            self.stop_controller_and_drain_requests(process)
            saved = self.command('results')
            self.remote(f'touch {FIXTURES}/release_change_activation')
            self.after_wait(); self.device('mmxbr0', False); self.port(False)
            self.wait('the timer recreates the bridge without a controller request', self.record,
                      lambda r: r and r['owed_restore']['restore_status'] == 'succeeded', seconds=40)
            self.device('mmxbr0'); self.address('mmxbr0', '10.0.8.5'); self.port()
            # The node job worker still runs here: it reads the record at its
            # own pace, sees the timer's restore, and then ends the job.
            self.restored(self.wait('the node job worker ends the job after the timer\'s restore',
                                    lambda: self.command('status', saved['id']),
                                    lambda status: status['entries'][0]['response']['status'] != 'running', seconds=30, interval=0.5))
            self.remote(f'rm -rf {GATE_DIRECTORY}\nsystemctl daemon-reload')
            self.apply(name)

        for case, previous in [('main-card-cut-with-previous-file', True), ('main-card-cut-with-no-previous-file', False),
                               ('main-card-cut-worker-killed-timer-alone', False), ('main-card-cut-restart-before-deadline', False),
                               ('main-card-cut-failed-restore-and-administrator-repair', True)]:
            with self.case(case):
                interrupted = 'worker-killed' in case or 'restart' in case
                self.inventory('ssh_only' if interrupted else 'https_with_ssh_fallback')
                self.previous_main(previous)
                if not interrupted:
                    # Process-draining cases stop the API. SSH fallback with
                    # an unchanged runner does not set up that service again.
                    self.remote('systemctl start mmx-https-api.service')
                name = Path(self.destination).name
                cut = configuration(ethernets={self.main: {'dhcp4': False, 'addresses': ['192.0.2.5/24']}})
                self.project([self.task(name, cut, duration='2m' if 'restart' in case else
                                        ('25s' if 'worker-killed' in case else '15s'))])
                process = self.background(); record = self.after_wait()
                if not interrupted:
                    self.require(sorted(record['before_change_confirmed_connection_kinds']) == ['https', 'ssh'],
                                 'both connection kinds worked before the cut', record)
                self.address(self.main, '192.0.2.5')
                self.require(not any(address in self.addresses(self.main) for address in self.initial_addresses),
                             'the cut removed the original DHCP address')
                self.require(not self.remote('ip -4 route show default').stdout, 'the cut removed the default route')
                self.require(self.fresh_ssh().returncode != 0, 'the cut refuses new SSH connections')
                if interrupted:
                    self.stop_controller_and_drain_requests(process); saved = self.command('results')
                    if 'worker-killed' in case:
                        self.kill_worker(record)
                    else:
                        self.remote('systemd-run --on-active=1s --timer-property=AccuracySec=100ms /usr/bin/systemctl reboot')
                        vm.await_boot(record['node_boot_id_before_change'])
                        self.connect(); self.inventory('ssh_only')
                if 'failed-restore' in case:
                    self.netplan_path = self.remote('command -v netplan').stdout.strip()
                    self.netplan_mode = self.remote('stat -c %a ' + self.netplan_path).stdout.strip()
                    self.remote('chmod 000 ' + self.netplan_path)
                    failed = self.wait('the failed native apply remains owed', self.record,
                                       lambda r: r and r['change_phase'] == 'restore_failed', seconds=40)
                    saved_contents = '/var/lib/mmx/owed_restores/' + failed['owed_restore']['owed_restore_id'] + '/previous_file_contents'
                    self.require(self.remote(f'test -f {saved_contents}', check=False).returncode == 0,
                                 'the failed restore retains the previous file')
                    self.require(failed['owed_restore']['previous_file_restored'] and
                                 not failed['owed_restore']['previous_config_activated'],
                                 'the failed apply does not claim restored running configuration', failed)
                    self.remote(f'chmod {self.netplan_mode} {self.netplan_path}'); self.netplan_mode = None
                    self.remote('systemctl start ' + failed['owed_restore']['restore_service_unit'])
                restored = self.wait('restoration completes before another MMX request', self.record,
                                     lambda r: r and r['owed_restore']['restore_status'] == 'succeeded', seconds=40)
                self.require(not restored['after_change_confirmed_connection_kinds'], 'the cut received no confirmation', restored)
                if 'restart' in case:
                    self.require(self.node_time() < instant(record['restore_deadline']), 'boot restores before the deadline', record)
                self.main_restored()
                if interrupted: result = self.command('status', saved['id'])
                else:
                    result = self.collect(process, 1)
                    result = self.command('status', result['id'])
                self.restored(result, task=not interrupted)
                output = self.command('status', result['id'], human=True)
                self.require(self.destination + ' was restored to its previous state' in output,
                             'human output states the restored state', output)
                if not interrupted:
                    # Status may use an already terminal saved result. A new
                    # read-only run must actually establish another connection.
                    vm.write_project(self.workspace, project_name='netplan_files', fleet_stages=[
                        {'id': 'verify_https_after_restore', 'nodes': 'all',
                         'node_tasks': [{'read_file': {'path': '/proc/sys/kernel/hostname', 'content_is_secret': False}}]}])
                    reached = self.command('run')
                    self.require(reached['entries'][0]['connection_used'] == 'https',
                                 'a new controller process reaches the restored node over HTTPS', reached)
        print(json.dumps({'passed': self.passed, 'count': len(self.passed)}, indent=2), flush=True)

    def connect(self):
        self.node, self.ssh = vm.connection()
        # vm.connection always creates a local SSH forward, even for Incus. Its
        # one long-lived SSH connection can end during a cut of the main card,
        # and HTTPS through it then fails although the node is fine, so MMX's
        # HTTPS goes straight to the node.
        self.node['https_address'] = f'https://{self.node["address"]}:7443'

    def cleanup(self):
        try:
            for process in self.processes:
                if process.poll() is None:
                    os.killpg(process.pid, signal.SIGKILL); process.communicate(timeout=5)
            if self.netplan_mode: self.remote(f'chmod {self.netplan_mode} {self.netplan_path}')
            if self.monitor_unit: self.remote(f'systemctl stop {self.monitor_unit}.service', check=False)
            self.remote(f'rm -rf {GATE_DIRECTORY}\n'
                        'for unit in /etc/systemd/system/mmx-activate-change-*.service /etc/systemd/system/mmx-owed-restore-*.timer /etc/systemd/system/mmx-owed-restore-*.service; do '
                        'test ! -f "$unit" || systemctl disable --now "${unit##*/}"; done\n'
                        'rm -f /etc/systemd/system/mmx-activate-change-*.service /etc/systemd/system/mmx-owed-restore-*.service /etc/systemd/system/mmx-owed-restore-*.timer\n'
                        f'rm -f {DIRECTORY}/*-mmx-*.yaml\n')
            if self.saved_main: self.remote(f'cp -a {FIXTURES}/original-main.yaml {self.main_file}')
            self.remote('netplan apply\nsystemctl daemon-reload\nsystemctl reset-failed')
            for name in ['mmxbr0', 'mmxvlan0', 'mmxbond0', 'mmxdummy0', 'mmxveth0', 'mmxveth1', 'mmxoutside0']:
                self.remote(f'if test -e /sys/class/net/{name}; then networkctl delete {name}; fi')
            if self.added_interface:
                self.remote(f'ip address flush dev {self.second} scope global')
            self.main_restored()
        finally:
            if self.added_interface:
                vm.cmd(['incus', 'config', 'device', 'remove', self.instance, 'second-interface'])
        vm.reset()


def main():
    selected = vm.target()
    if not selected.get('incus_instance'):
        print('SKIP netplan_files.py: this VM suite needs Incus for a second card and observations during loss of network access', flush=True)
        return
    import incus
    distribution = next((name for name, image in incus.IMAGES.items() if image == selected.get('image')), '')
    if distribution not in SUPPORTED:
        print(f'SKIP netplan_files.py: {distribution or selected.get("image")} is not one of the Netplan guests, Ubuntu 24.04 and 26.04', flush=True)
        return
    vm.up(); vm.reset()
    suite, failure = None, None
    try:
        suite = Suite.__new__(Suite)
        suite.__init__(distribution, selected['incus_instance'])
        suite.run()
    except BaseException as error:
        failure = error
        print('FAIL netplan_files.py: ' + str(error), flush=True)
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
