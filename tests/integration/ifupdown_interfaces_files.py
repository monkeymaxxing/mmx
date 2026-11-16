#!/usr/bin/env python3
"""The real ifupdown module: interfaces.d files, the main file, and autonomous restore.

Uses production binaries on Debian 12 and 13 guests that the setup switches
from systemd-networkd to ifupdown, as Debian's installer and the installers of
some server providers set a server up. Incus provides the second card and the
observations while the main card is cut off; no MMX request is used to
observe or trigger a restore. The cleanup switches the guest back.
"""
import contextlib
import json
import os
import shlex
import shutil
import signal

import vm
from owed_restores import instant
from systemd_networkd_files import Suite as NetworkdObservations, LINK_INDEXES_AND_NAMES

MODULE = 'ifupdown_interfaces_file'
MAIN_FILE = '/etc/network/interfaces'
DIRECTORY = '/etc/network/interfaces.d'
SECOND_CARD_FILE = DIRECTORY + '/60-second'
VLAN_FILE = DIRECTORY + '/61-vlan'
FAILING_UP_COMMAND_FILE = DIRECTORY + '/62-failing-up-command'
MISSPELLED_OPTION_FILE = DIRECTORY + '/63-misspelled-option'
FILE_NOT_READ_BY_THE_MAIN_FILE = DIRECTORY + '/64-not-read-by-the-main-file'
FIXTURES = '/var/tmp/mmx-integration/ifupdown-interfaces-files'
SAVED_ETC_NETWORK = FIXTURES + '/etc-network-before-ifupdown.tar'
SAVED_NETWORKD_MAIN_FILE = FIXTURES + '/networkd-main-card-file-before-ifupdown'
SAVED_MAIN_FILE_BEFORE_REFUSALS = FIXTURES + '/main-file-before-refusals'
CLOUD_INIT_NETWORK_SETTING = '/etc/cloud/cloud.cfg.d/99-mmx-test-disable-network-config.cfg'
RESOLVED_DNS_SETTING = '/etc/systemd/resolved.conf.d/99-mmx-test-dns-through-the-gateway.conf'
NETWORKD_UNITS = ('systemd-networkd.service', 'systemd-networkd.socket', 'systemd-networkd-wait-online.service')
UNITS_COMMAND = 'find /etc/systemd/system -name "mmx-owed-restore-*" -o -name "mmx-activate-change-*"'
CUT_ADDRESS = '192.0.2.5'
EXTRA_ROUTE = '198.51.100.0/24'
VLAN_ROUTE = '203.0.113.0/24'
SUPPORTED = ('debian12', 'debian13')


def stanza(interface, *options):
    return f'auto {interface}\niface {interface} inet static\n' + ''.join(f'    {option}\n' for option in options)


def file_state_command(path):
    quoted = shlex.quote(path)
    return f"if test -f {quoted}; then sha256sum {quoted}; stat -c '%a %u %g %y' {quoted}; else echo absent; fi"


class Suite(NetworkdObservations):
    # Share the networkd suite's Incus observations, records, diagnostics, and
    # exact-process draining, not its fixtures or file changes.
    def __init__(self, distribution, instance):
        self.distribution, self.instance = distribution, instance
        self.workspace = vm.ROOT / 'ifupdown-interfaces-files'
        shutil.rmtree(self.workspace, ignore_errors=True)
        self.workspace.mkdir(parents=True)
        self.case_name, self.case_directory = 'setup', self.workspace
        self.processes, self.ssh_units, self.passed = [], [], []
        self.destination, self.last_output, self.last_record = '', '', None
        self.old_records = set()
        self.added_interface, self.switched_to_ifupdown = False, False
        self.networkd_units_to_enable_again = []
        vm.build()
        self.connect()
        self.binaries, self.mmx = vm.ROOT / 'bin', vm.ROOT / 'bin/mmx'
        self.remote(f'mkdir -p {FIXTURES}')
        routes = json.loads(self.remote('ip -j -4 route show default').stdout)
        self.require(routes and routes[0].get('gateway'), 'a main card with an IPv4 default route through a gateway', routes)
        self.main, self.gateway = routes[0]['dev'], routes[0]['gateway']
        self.initial_addresses = self.addresses(self.main)
        self.require(len(self.initial_addresses) == 1, 'one IPv4 address on the main card', self.initial_addresses)
        prefix = next(entry['prefixlen'] for link in json.loads(self.remote(f'ip -j -4 address show dev {self.main}').stdout)
                      for entry in link['addr_info'])
        # The DHCP-given address and gateway, written as a static stanza.
        self.main_options = (f'address {self.initial_addresses[0]}/{prefix}', f'gateway {self.gateway}')
        self.switch_to_ifupdown()
        before = set(self.remote('ls /sys/class/net').stdout.split())
        vm.cmd(['incus', 'config', 'device', 'add', self.instance, 'second-interface',
                'nic', 'nictype=p2p', 'mtu=1500'])
        self.added_interface = True
        # udev renames a new card (eth0 becomes enp6s0); read the name after that.
        names = self.wait('one added second network card',
                          lambda: set(self.remote('udevadm settle --timeout=5; ls /sys/class/net').stdout.split()) - before,
                          lambda names: len(names) == 1, seconds=10)
        self.second = names.pop()
        self.inventory('ssh_only')

    def connect(self):
        self.node, self.ssh = vm.connection()
        # vm.connection always creates a local SSH forward, even for Incus. Its
        # one long-lived SSH connection can end while the main card is taken
        # down, and HTTPS through it then fails although the node is fine, so
        # MMX's HTTPS goes straight to the node.
        self.node['https_address'] = f'https://{self.node["address"]}:7443'

    def main_file_contents(self, *options):
        # The layout of Debian's installer and of some server providers: the
        # source line, the loopback, and the main card's static stanza.
        return 'source /etc/network/interfaces.d/*\n\nauto lo\niface lo inet loopback\n\n' + stanza(self.main, *options)

    def switch_to_ifupdown(self):
        index = self.remote(f'cat /sys/class/net/{self.main}/ifindex').stdout.strip()
        properties = self.remote(f'cat /run/systemd/netif/links/{index}').stdout
        self.require('NETWORK_FILE=' in properties, 'systemd-networkd manages the main card before the switch', properties)
        self.networkd_main_file = next(line.split('=', 1)[1] for line in properties.splitlines() if line.startswith('NETWORK_FILE='))
        installed = self.remote("dpkg-query -W -f='${Status}' ifupdown 2>/dev/null || true").stdout
        self.require(installed != 'install ok installed', 'a guest without ifupdown, so that the cleanup can remove it again', installed)
        self.networkd_units_to_enable_again = [
            unit for unit in NETWORKD_UNITS
            if self.remote(f'systemctl is-enabled {unit}', check=False).stdout.strip() == 'enabled']
        print(f'SETUP switching {self.main} from {self.networkd_main_file} to ifupdown and restarting the guest', flush=True)
        self.switched_to_ifupdown = True
        self.remote(
            f'if test -d /etc/network; then tar -C /etc -cpf {SAVED_ETC_NETWORK} network; fi\n'
            f'cp -a {shlex.quote(self.networkd_main_file)} {SAVED_NETWORKD_MAIN_FILE}\n'
            'DEBIAN_FRONTEND=noninteractive apt-get install -y -q --no-install-recommends -o DPkg::Lock::Timeout=60 ifupdown\n'
            f'printf %s {shlex.quote(self.main_file_contents(*self.main_options))} > {MAIN_FILE}\n'
            f'chown root:root {MAIN_FILE}\nchmod 0644 {MAIN_FILE}\n'
            f'mkdir -p {os.path.dirname(CLOUD_INIT_NETWORK_SETTING)} {os.path.dirname(RESOLVED_DNS_SETTING)}\n'
            # Without its own network configuration cloud-init writes no file
            # for either tool at boot. Without networkd, resolved learns no DNS
            # server from DHCP; the bridge's gateway answers DNS.
            f"printf 'network: {{config: disabled}}\\n' > {CLOUD_INIT_NETWORK_SETTING}\n"
            f"printf '[Resolve]\\nDNS={self.gateway}\\n' > {RESOLVED_DNS_SETTING}\n"
            f'rm -f {shlex.quote(self.networkd_main_file)}\n'
            + (f'systemctl disable {" ".join(self.networkd_units_to_enable_again)}\n' if self.networkd_units_to_enable_again else '')
            + 'systemctl enable networking.service', timeout=300)
        vm.reboot()
        self.connect()
        states = self.remote('systemctl is-active systemd-networkd.service networking.service', check=False).stdout.split()
        self.require(states == ['inactive', 'active'], 'after the restart ifupdown, not systemd-networkd, configures the network', states)
        self.require(self.ifupdown_lists_up(self.main), f'ifupdown lists {self.main} as up')
        self.require(self.addresses(self.main) == self.initial_addresses and
                     f'via {self.gateway} dev {self.main}' in self.remote('ip -4 route show default').stdout,
                     'the main card keeps its address and default route under ifupdown',
                     (self.addresses(self.main), self.remote('ip -4 route show default').stdout))

    def switch_back_to_systemd_networkd(self):
        units = ' '.join(self.networkd_units_to_enable_again)
        self.remote(
            # Stopping networking.service takes down every interface but lo.
            'systemctl disable --now networking.service || true\n'
            "if dpkg-query -W -f='${Status}' ifupdown 2>/dev/null | grep -q '^install ok installed$'; then\n"
            '  DEBIAN_FRONTEND=noninteractive apt-get purge -y -q -o DPkg::Lock::Timeout=60 ifupdown\n'
            'fi\n'
            'rm -rf /etc/network\n'
            f'if test -f {SAVED_ETC_NETWORK}; then tar -C /etc -xpf {SAVED_ETC_NETWORK}; fi\n'
            f'if test -f {SAVED_NETWORKD_MAIN_FILE}; then cp -a {SAVED_NETWORKD_MAIN_FILE} {shlex.quote(self.networkd_main_file)}; fi\n'
            f'rm -f {CLOUD_INIT_NETWORK_SETTING} {RESOLVED_DNS_SETTING}\n'
            + (f'systemctl enable {units}\n' if units else '')
            + 'systemctl restart systemd-resolved.service\n'
            'systemctl start systemd-networkd.socket systemd-networkd.service', timeout=300)

    def task(self, path, content=None, duration='20s', permissions_mode=None):
        fields = {'path': path, 'restore_unless_controller_confirms_within': duration}
        fields.update({'content': content} if content is not None else {'state': 'absent'})
        if permissions_mode is not None: fields['permissions_mode'] = permissions_mode
        return {MODULE: fields}

    def project(self, tasks):
        self.old_records = {r['owed_restore']['owed_restore_id'] for r in self.all_records()}
        self.destination = tasks[-1][MODULE]['path']
        self.last_record = None
        vm.write_project(self.workspace, project_name='ifupdown_interfaces_files', fleet_stages=[
            {'id': 'network', 'nodes': 'all', 'node_tasks': tasks}])

    @contextlib.contextmanager
    def case(self, name):
        with super().case(name):
            yield
            self.no_owed_restore_remains()

    def no_owed_restore_remains(self):
        unresolved = [record for record in self.all_records() if not record.get('owed_restore_cleanup_complete')]
        units = self.remote(UNITS_COMMAND).stdout
        self.require(not unresolved and not units.strip(), 'no owed restore or restore unit remains after the case',
                     {'records': unresolved, 'units': units})

    def check_cleanup_paths(self, records):
        super().check_cleanup_paths(records)
        # This module's activation also gets a saved copy of the proposed file.
        paths = [f'/var/lib/mmx/owed_restores/{record["owed_restore"]["owed_restore_id"]}/proposed_file_contents'
                 for record in records]
        output = self.remote('for path in ' + ' '.join(paths) + '; do if test -e "$path"; then '
                             'echo "cleanup left $path"; exit 1; fi; done', check=False)
        self.require(output.returncode == 0, 'cleanup removed the saved proposed file', output.stdout + output.stderr)

    def diagnostics(self):
        # The inherited diagnostics include the record, both MMX services, and
        # the addresses and routes.
        super().diagnostics()
        result = self.remote(f'cat {MAIN_FILE} || true; for file in {DIRECTORY}/*; do test ! -f "$file" || {{ echo "== $file"; cat "$file"; }}; done; '
                             'ifquery --state 2>&1 || true; systemctl show networking.service -p ActiveState -p Result; '
                             'journalctl --no-pager -u networking.service -n 12 -o short-precise', check=False)
        print('IFUPDOWN FILES, STATE, AND JOURNAL ' + json.dumps(result.stdout + result.stderr), flush=True)
        with (self.case_directory / 'failure-diagnostics.txt').open('a') as stream:
            stream.write('\n' + result.stdout + result.stderr)

    def ifupdown_lists_up(self, interface):
        return self.remote(f'ifquery --state {interface}', check=False).returncode == 0

    def installed(self, path, content, permissions_mode='644'):
        output = self.remote(f'stat -c "%a %U %G" {path}; cat {path}').stdout
        metadata, _, installed_bytes = output.partition('\n')
        self.require(metadata == f'{permissions_mode} root root' and installed_bytes == content,
                     f"{path} holds the task's bytes with mode {permissions_mode}, owner root, and group root", output)

    def absent(self, path):
        self.require(self.remote(f'test ! -e {path}', check=False).returncode == 0, f'{path} is gone')

    def no_ipv4_configuration(self, device):
        self.wait(f'{device} has no IPv4 address or route',
                  lambda: self.remote(f'ip -o -4 address show dev {device}; ip -4 route show dev {device}', check=False).stdout,
                  lambda output: not output.strip(), seconds=10, interval=0.2)

    def extra_route_present(self):
        self.wait(f'the route to {EXTRA_ROUTE} through the gateway',
                  lambda: self.remote(f'ip -4 route show {EXTRA_ROUTE}').stdout,
                  lambda output: f'via {self.gateway} dev {self.main}' in output, seconds=10, interval=0.2)

    def vlan_route_present(self, vlan):
        self.wait(f'the route to {VLAN_ROUTE} through {vlan}',
                  lambda: self.remote(f'ip -4 route show {VLAN_ROUTE}').stdout,
                  lambda output: f'dev {vlan}' in output, seconds=10, interval=0.2)

    def network_state(self, path):
        # One Incus request: the task's file, the main file, addresses,
        # devices, ifupdown's state, and the restore units.
        return self.remote('\n'.join([file_state_command(path), file_state_command(MAIN_FILE), 'ip -4 -brief address',
                                      LINK_INDEXES_AND_NAMES, 'ifquery --state 2>&1 || true', UNITS_COMMAND])).stdout

    def refused(self, path, content, reason):
        for command in ('dryrun', 'run'):
            self.project([self.task(path, content)])
            before, records = self.network_state(path), self.all_records()
            result = self.command(command, expected=1)
            task = result['entries'][0]['response']['results'][-1]
            text = task.get('reason', '')
            self.require(task['status'] == 'failed' and task['change'] == 'unchanged' and reason in text,
                         f'{command} of {path}: an unchanged refusal that names "{reason}"', task)
            self.require('/dev/stdin' not in text, f'{command}: the refusal names the file, not standard input', text)
            after = self.network_state(path)
            self.require(after == before, f'{command}: the refusal changed no file, address, device, ifupdown state, or unit',
                         {'before': before, 'after': after})
            self.require(self.all_records() == records, f'{command}: the refusal wrote no restore record')

    def cut_observed(self, new_ssh_must_fail):
        self.address(self.main, CUT_ADDRESS)
        self.require(not any(address in self.addresses(self.main) for address in self.initial_addresses),
                     'the cut-off removed the main address', self.addresses(self.main))
        self.require(not self.remote('ip -4 route show default').stdout, 'the cut-off removed the default route')
        if new_ssh_must_fail:
            self.require(self.fresh_ssh().returncode != 0, 'the cut-off refuses new SSH connections')

    def restore_started_after_networking_service(self, record):
        unit = record['owed_restore']['restore_service_unit']
        lines = self.remote('systemctl show networking.service -p ActiveEnterTimestampMonotonic --value\n'
                            f'journalctl --no-pager -b 0 -u {unit} -o json --output-fields=MESSAGE | head -n 1').stdout.splitlines()
        networking_active = int(lines[0])
        first = json.loads(lines[1]) if len(lines) > 1 else None
        self.require(first and int(first['__MONOTONIC_TIMESTAMP']) >= networking_active > 0,
                     "the restore service started on the new boot after networking.service's ifup -a had finished", lines)

    def human_restored(self, run_id):
        output = self.command('status', run_id, human=True)
        self.require(self.destination + ' was restored to its previous state' in output, 'human output states the restored state', output)

    def run(self):
        second = stanza(self.second, 'address 10.0.60.5/24')
        changed_second = stanza(self.second, 'address 10.0.61.5/24')
        vlan = self.second + '.100'
        cut = self.main_file_contents(f'address {CUT_ADDRESS}/24')

        with self.case('second-card-file-dryrun-present-and-unchanged'):
            result = self.apply(SECOND_CARD_FILE, second, expected=(0, 2, 3), change='would_change', command='dryrun')
            reason = result['entries'][0]['response']['results'][-1].get('reason', '')
            self.require(f'Would write {SECOND_CARD_FILE} and activate it.' in reason and
                         f'Would bring up: {self.second}.' in reason and 'Would take down' not in reason,
                         'dryrun names the file and the interface it would bring up', reason)
            self.require(self.record() is None and not self.remote(UNITS_COMMAND).stdout, 'dryrun creates no record or unit')
            self.absent(SECOND_CARD_FILE)
            self.require(not self.addresses(self.second) and not self.ifupdown_lists_up(self.second),
                         "dryrun's checks through standard input configure nothing", self.addresses(self.second))
            self.apply(SECOND_CARD_FILE, second)
            self.installed(SECOND_CARD_FILE, second)
            self.address(self.second, '10.0.60.5')
            self.require(self.ifupdown_lists_up(self.second), f'ifupdown lists {self.second} as up')
            result = self.apply(SECOND_CARD_FILE, second, change='unchanged')
            reason = result['entries'][0]['response']['results'][-1].get('reason', '')
            self.require('no activation or confirmation wait was needed' in reason, 'unchanged reason', reason)
            # Only the activation service activates, and it exists only with a record.
            self.require(self.record() is None and not self.remote(UNITS_COMMAND).stdout, 'unchanged creates no record and no activation')
            self.require(self.addresses(self.second) == ['10.0.60.5'], 'unchanged leaves the address', self.addresses(self.second))

        with self.case('second-card-file-changed-address'):
            self.apply(SECOND_CARD_FILE, changed_second)
            self.installed(SECOND_CARD_FILE, changed_second)
            self.address(self.second, '10.0.61.5'); self.address(self.second, '10.0.60.5', False)
            routes = self.remote(f'ip -4 route show dev {self.second}').stdout
            self.require('10.0.61.0/24' in routes and '10.0.60.0/24' not in routes, 'only the new network has a route', routes)
            self.require(self.ifupdown_lists_up(self.second), f'ifupdown lists {self.second} as up')

        with self.case('existing-private-mode-preserved-and-explicit-mode-applied'):
            self.remote(f'chmod 0600 {SECOND_CARD_FILE}')
            self.apply(SECOND_CARD_FILE, second)
            self.installed(SECOND_CARD_FILE, second, '600')
            self.project([self.task(SECOND_CARD_FILE, second, permissions_mode='0640')])
            result = self.command('run')
            self.require(result['entries'][0]['response']['results'][-1]['change'] == 'changed',
                         'an explicit permissions_mode changes existing permissions', result)
            self.clean_change()
            self.installed(SECOND_CARD_FILE, second, '640')
            self.project([self.task(SECOND_CARD_FILE, second, permissions_mode='0644')])
            self.command('run')
            self.clean_change()
            self.installed(SECOND_CARD_FILE, second)
            self.apply(SECOND_CARD_FILE, changed_second)
            self.installed(SECOND_CARD_FILE, changed_second)

        with self.case('vlan-file-present-then-its-parent-card-changed'):
            vlan_file = stanza(vlan, 'address 10.0.100.5/24', f'up ip route add {VLAN_ROUTE} dev {vlan}')
            self.apply(VLAN_FILE, vlan_file)
            self.installed(VLAN_FILE, vlan_file)
            self.device(vlan); self.address(vlan, '10.0.100.5'); self.vlan_route_present(vlan)
            self.require(self.ifupdown_lists_up(vlan), f'ifupdown lists {vlan} as up')
            # Taking the parent card down takes its VLANs down too, and the
            # kernel brings them back without the routes their stanzas add.
            self.apply(SECOND_CARD_FILE, second)
            self.address(self.second, '10.0.60.5'); self.address(vlan, '10.0.100.5'); self.vlan_route_present(vlan)
            self.require(self.ifupdown_lists_up(vlan), f'ifupdown still lists {vlan} as up')

        with self.case('vlan-and-second-card-files-absent'):
            self.apply(VLAN_FILE)
            self.device(vlan, False); self.absent(VLAN_FILE)
            # The physical card's own file goes too, so that later cases find
            # the card without configuration.
            self.apply(SECOND_CARD_FILE)
            self.no_ipv4_configuration(self.second); self.absent(SECOND_CARD_FILE)
            self.require(not self.ifupdown_lists_up(self.second), f'ifupdown no longer lists {self.second} as up')

        with self.case('main-file-added-route-confirmed-over-ssh-and-https'):
            with_route = self.main_file_contents(*self.main_options, f'up ip route add {EXTRA_ROUTE} via {self.gateway} dev {self.main}')
            self.inventory('https_with_ssh_fallback')
            self.apply(MAIN_FILE, with_route)
            record = self.last_record
            self.inventory('ssh_only')
            self.require(sorted(record['after_change_confirmed_connection_kinds']) == ['https', 'ssh'],
                         'new SSH and HTTPS connections confirm the main file change', record)
            expected = {
                'interfaces_to_take_down_with_previous_file': [self.main],
                'interfaces_to_bring_up_with_proposed_file': [{'interface': self.main, 'listed_in': 'auto'}],
                'interfaces_to_take_down_with_proposed_file_on_restore': [self.main],
                'interfaces_to_bring_up_with_previous_file_on_restore': [{'interface': self.main, 'listed_in': 'auto'}]}
            self.require(record['module_restore_data'] == expected and EXTRA_ROUTE.split('/')[0] not in json.dumps(record),
                         'the restore data holds interface names and classes only, no stanza text', record['module_restore_data'])
            self.installed(MAIN_FILE, with_route)
            self.extra_route_present()
            self.require(self.addresses(self.main) == self.initial_addresses and
                         f'via {self.gateway} dev {self.main}' in self.remote('ip -4 route show default').stdout,
                         'the main card keeps its address and default route', self.addresses(self.main))

        with self.case('main-file-cut-off-restored-by-the-node'):
            self.destination = MAIN_FILE
            # Metadata unlike the module's own prove that the restore puts back
            # the previous file's owner, group, mode, and modification time.
            self.remote(f'chown 1:1 {MAIN_FILE}\nchmod 0640 {MAIN_FILE}\ntouch -d "2001-01-02 03:04:05 UTC" {MAIN_FILE}')
            self.before = self.file_state()
            self.project([self.task(MAIN_FILE, cut, duration='12s')])
            process = self.background()
            self.after_wait()
            self.cut_observed(new_ssh_must_fail=True)
            restored = self.wait('the node restores the main file by itself', self.record,
                                 lambda r: r and r['owed_restore']['restore_status'] == 'succeeded', seconds=35)
            self.require(not restored['after_change_confirmed_connection_kinds'], 'no confirmation after the cut-off', restored)
            self.main_restored(); self.extra_route_present()
            result = self.collect(process, 1)
            result = self.command('status', result['id'])
            self.restored(result)
            reason = result['entries'][0]['response']['results'][-1].get('reason', '')
            self.require('No reachability confirmation arrived over a new SSH connection within 12s.' in reason and
                         f'The previous file {MAIN_FILE} and its running configuration are restored.' in reason,
                         'the task names the missing confirmation and the restored file', reason)
            self.human_restored(result['id'])

        with self.case('added-file-whose-ifup-fails-is-restored-at-once'):
            self.destination = FAILING_UP_COMMAND_FILE
            self.before = self.file_state()
            result = self.apply(FAILING_UP_COMMAND_FILE, stanza(self.second, 'address 10.0.62.5/24', 'up false'), expected=1)
            now = self.node_time()
            self.restored(result)
            self.require(now < instant(self.last_record['restore_deadline']) - 5,
                         'the failed ifup restored at once, not at the deadline', (now, self.last_record))
            self.require(f'ifupdown did not accept {FAILING_UP_COMMAND_FILE}' in self.last_output,
                         'the task names ifupdown and the file', self.last_output)
            # ifup leaves the address of a failed interface; only ifdown --force
            # with the proposed file removes it.
            self.no_ipv4_configuration(self.second)
            self.require(not self.ifupdown_lists_up(self.second), f'ifupdown does not list {self.second} as up')

        with self.case('refusals-before-any-change-in-dryrun-and-run'):
            self.refused(MISSPELLED_OPTION_FILE, stanza(self.second, 'adress 10.0.63.5/24'), 'missing required variable: address')
            main_text = self.remote(f'cat {MAIN_FILE}').stdout
            without_source = ''.join(line for line in main_text.splitlines(keepends=True) if not line.startswith('source '))
            self.refused(MAIN_FILE, without_source, 'the proposed file changes source, source-directory, or rename lines')
            self.remote(f'cp -a {MAIN_FILE} {SAVED_MAIN_FILE_BEFORE_REFUSALS}\n'
                        f"grep -v '^source ' {SAVED_MAIN_FILE_BEFORE_REFUSALS} > {MAIN_FILE}")
            try:
                self.refused(FILE_NOT_READ_BY_THE_MAIN_FILE, stanza(self.second, 'address 10.0.64.5/24'),
                             f'the main ifupdown file {MAIN_FILE} does not include the proposed file')
            finally:
                self.remote(f'cp -a {SAVED_MAIN_FILE_BEFORE_REFUSALS} {MAIN_FILE}')
            self.no_ipv4_configuration(self.second)

        with self.case('restart-during-the-confirmation-wait-of-a-main-file-cut-off'):
            self.destination = MAIN_FILE
            self.before = self.file_state()
            self.project([self.task(MAIN_FILE, cut, duration='2m')])
            process = self.background()
            record = self.after_wait()
            self.cut_observed(new_ssh_must_fail=False)
            self.stop_controller_and_drain_requests(process)
            saved = self.command('results')
            self.remote('systemd-run --on-active=1s --timer-property=AccuracySec=100ms /usr/bin/systemctl reboot')
            vm.await_boot(record['node_boot_id_before_change'])
            self.connect(); self.inventory('ssh_only')
            restored = self.wait('the restore on the new boot completes before another MMX request', self.record,
                                 lambda r: r and r['owed_restore']['restore_status'] == 'succeeded', seconds=35)
            self.require(not restored['after_change_confirmed_connection_kinds'], 'no confirmation after the cut-off', restored)
            self.require(self.node_time() < instant(record['restore_deadline']),
                         'the restore finished on the new boot before the deadline; the restart used up the allowance', record)
            self.restore_started_after_networking_service(record)
            self.main_restored(); self.extra_route_present()
            self.restored(self.command('status', saved['id']), task=False)
            self.human_restored(saved['id'])
        print(json.dumps({'passed': self.passed, 'count': len(self.passed)}, indent=2), flush=True)

    def cleanup(self):
        try:
            for process in self.processes:
                if process.poll() is None:
                    os.killpg(process.pid, signal.SIGKILL); process.communicate(timeout=5)
            self.remote('for unit in /etc/systemd/system/mmx-activate-change-*.service /etc/systemd/system/mmx-owed-restore-*.timer /etc/systemd/system/mmx-owed-restore-*.service; do '
                        'test ! -f "$unit" || systemctl disable --now "${unit##*/}"; done\n'
                        'rm -f /etc/systemd/system/mmx-activate-change-*.service /etc/systemd/system/mmx-owed-restore-*.service /etc/systemd/system/mmx-owed-restore-*.timer\n'
                        'systemctl daemon-reload\nsystemctl reset-failed')
            if self.switched_to_ifupdown:
                self.switch_back_to_systemd_networkd()
                self.main_restored()
        finally:
            if self.added_interface:
                vm.cmd(['incus', 'config', 'device', 'remove', self.instance, 'second-interface'])
        vm.reset()


def main():
    selected = vm.target()
    if not selected.get('incus_instance'):
        print('SKIP ifupdown_interfaces_files.py: this VM suite needs Incus for a second card and observations while the main card is cut off', flush=True)
        return
    import incus
    distribution = next((name for name, image in incus.IMAGES.items() if image == selected.get('image')), '')
    if distribution not in SUPPORTED:
        print(f'SKIP ifupdown_interfaces_files.py: {distribution or selected.get("image")} is not one of the ifupdown guests, Debian 12 and 13', flush=True)
        return
    vm.up(); vm.reset()
    suite, failure = None, None
    try:
        suite = Suite.__new__(Suite)
        suite.__init__(distribution, selected['incus_instance'])
        suite.run()
    except BaseException as error:
        failure = error
        print('FAIL ifupdown_interfaces_files.py: ' + str(error), flush=True)
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
