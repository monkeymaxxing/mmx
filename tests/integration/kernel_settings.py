#!/usr/bin/env python3
"""sysctl_setting and kernel_module through the public CLI, checked in /proc/sys, /proc/modules, /sys/module, and
with systemd-sysctl and systemd-modules-load as the next boot runs them. It needs the guest's own kernel."""
import json
import shlex
import shutil

import vm

# Files the suite adds beside MMX's own; each name says it is the suite's.
LATER_FILE = '/usr/lib/sysctl.d/95-mmx-test-later.conf'
LATER_MASK = '/etc/sysctl.d/95-mmx-test-later.conf'
EARLIER_FILE = '/usr/lib/sysctl.d/10-mmx-test-earlier.conf'
SYSCTL_CONF_BACKUP = '/var/tmp/mmx-test-sysctl.conf'
# A bridge whose name has a dot, as a VLAN interface's has. The bridge module
# is there once br_netfilter is loaded.
DOTTED_INTERFACE = 'mmx0.10'
# The design's example, as written there.
EXAMPLE = [
    {'kernel_module': {'name': 'br_netfilter'}},
    {'id': 'kernel_settings', 'loop': {'net.ipv4.ip_forward': '1', 'net.bridge.bridge-nf-call-iptables': '1',
                                        'fs.inotify.max_user_instances': '1048576'},
     'as': 'setting', 'sysctl_setting': {'name': '{{ .setting.key }}', 'value': '{{ .setting.value }}'}},
]
SETTINGS = ('net.ipv4.ip_forward', 'fs.inotify.max_user_instances', 'net.ipv4.ip_local_port_range')


def main():
    vm.up()
    node, ssh = vm.connection()
    vm.reset()
    workspace = vm.ROOT / 'kernel-settings'
    if workspace.exists():
        shutil.rmtree(workspace)
    vm.write_inventory(workspace, {'nodes': {'app': node}}, name='example')
    vm.build()
    mmx = vm.ROOT / 'bin/mmx'
    passed = []

    def remote(script, check=True):
        result = vm.cmd([*ssh, 'sudo -n /bin/sh -c ' + shlex.quote('set -eu\n' + script)], check=check)
        return result.stdout if check else result

    def run(command, expected=None):
        result = vm.cmd([mmx, command, *vm.playbook_args(command), '--json'], cwd=workspace, check=False)
        assert result.returncode in (vm.success_codes(command) if expected is None else (expected,)), (command, result.stdout, result.stderr)
        return json.loads(result.stdout)

    def project(tasks):
        vm.write_project(workspace, project_name='kernel_settings', node_task_defaults={'retry': {'pause_between_tries': '0s'}},
                         fleet_stages=[{'id': 'configure', 'nodes': 'all', 'node_tasks': tasks}])

    def results(record):
        return record['entries'][0]['response']['results']

    def change(record):
        return record['entries'][0]['response']['change']

    def mark(message):
        passed.append(message)
        print('PASS ' + message, flush=True)

    def live(name):
        return remote(f'cat /proc/sys/{name.replace(".", "/")}').strip()

    def loaded(module):
        return any(line.split()[0] == module for line in remote('cat /proc/modules').splitlines())

    def text(path):
        return remote(f'cat {path} 2>/dev/null || true')

    def boot():
        """Apply the configuration files as the next boot does: modules first, then settings. The suite
        restarts them more often than systemd's start limit allows, so it forgets the earlier starts."""
        remote('systemctl reset-failed systemd-modules-load.service systemd-sysctl.service\n'
               'systemctl restart systemd-modules-load.service systemd-sysctl.service')

    distribution = remote('. /etc/os-release; echo "$ID ${VERSION_ID:-}"').strip()
    original = {name: live(name) for name in SETTINGS}
    sysctl_conf_existed = remote('test -e /etc/sysctl.conf && echo yes || true').strip() == 'yes'
    if sysctl_conf_existed:
        remote(f'cp -p /etc/sysctl.conf {SYSCTL_CONF_BACKUP}')
    builtin = remote('basename "$(head -n 1 /lib/modules/$(uname -r)/modules.builtin)" .ko').strip().replace('-', '_')

    def cleanup():
        remote(f'rm -f /etc/sysctl.d/90-mmx-* /etc/modules-load.d/mmx-* /etc/modprobe.d/mmx-* {LATER_FILE} {LATER_MASK} {EARLIER_FILE}\n'
               f'ip link del {DOTTED_INTERFACE} 2>/dev/null || true\n'
               'modprobe -r vxlan br_netfilter 2>/dev/null || true\n'
               + (f'cp -p {SYSCTL_CONF_BACKUP} /etc/sysctl.conf\n' if sysctl_conf_existed else 'rm -f /etc/sysctl.conf\n')
               + ''.join(f'sysctl -q -w {shlex.quote(name + "=" + value)} || true\n' for name, value in original.items()))

    installed_modules_package = False
    try:
        cleanup()
        # The Red Hat family 10 ships br_netfilter in kernel-modules-extra,
        # which must match the running kernel. The suite removes the package
        # at its end: a later suite on this guest must not find kernel
        # modules that the guest did not have.
        installed_modules_package = 'the suite installed the package' in remote(
            'modprobe --dry-run br_netfilter 2>/dev/null || '
            '{ dnf -y -q install "kernel-modules-extra-$(uname -r)" && echo the suite installed the package; }')
        assert not loaded('br_netfilter'), 'br_netfilter is in use on this node; the suite needs it unloaded'
        remote('sysctl -q -w net.ipv4.ip_forward=0 fs.inotify.max_user_instances=128')

        # The design's example: the second setting exists only once
        # br_netfilter is loaded, so the dryrun cannot check it.
        project(EXAMPLE)
        preview = results(run('dryrun'))
        assert preview[0]['change'] == 'would_change' and preview[0]['value']['loaded_after'] is True, preview[0]
        # Each iteration of the loop has its own result.
        items = {r['task'][len('kernel_settings['):-1]: r for r in preview if r['task'].startswith('kernel_settings[')}
        assert len(items) == 3, preview
        assert items['net.ipv4.ip_forward']['change'] == 'would_change', items
        assert items['net.ipv4.ip_forward']['value'] == {'live_value_before': '0', 'live_value_after': '1'}, items
        assert items['fs.inotify.max_user_instances']['change'] == 'would_change', items
        assert items['net.bridge.bridge-nf-call-iptables']['status'] == 'not_checked', items
        assert 'kernel_module' in items['net.bridge.bridge-nf-call-iptables']['reason'], items
        assert not loaded('br_netfilter') and live('net.ipv4.ip_forward') == '0'
        assert remote('ls /etc/sysctl.d /etc/modules-load.d').count('mmx') == 0
        mark('dryrun of the design\'s example reports the module to load and the old and new values; the setting that '
             'br_netfilter adds is not checked, and nothing changes (%s)' % distribution)

        record = run('run')
        assert change(record) == 'changed', record
        assert loaded('br_netfilter')
        assert live('net.ipv4.ip_forward') == '1' and live('net.bridge.bridge-nf-call-iptables') == '1'
        assert live('fs.inotify.max_user_instances') == '1048576'
        assert text('/etc/modules-load.d/mmx-br_netfilter.conf') == 'br_netfilter\n'
        assert text('/etc/sysctl.d/90-mmx-net.ipv4.ip_forward.conf') == 'net.ipv4.ip_forward = 1\n'
        assert text('/etc/sysctl.d/90-mmx-net.bridge.bridge-nf-call-iptables.conf') == 'net.bridge.bridge-nf-call-iptables = 1\n'
        assert remote('stat -c %a /etc/sysctl.d/90-mmx-net.ipv4.ip_forward.conf').strip() == '644'
        assert change(run('run')) == 'unchanged' and change(run('dryrun', 0)) == 'unchanged'
        # Only the named settings were applied: another file's setting stays
        # as it was set by hand.
        remote(f'printf "fs.inotify.max_user_watches = 4096\\n" > {EARLIER_FILE}\nsysctl -q -w net.ipv4.ip_forward=0')
        watches = remote('cat /proc/sys/fs/inotify/max_user_watches').strip()
        assert watches != '4096'
        assert change(run('run')) == 'changed' and live('net.ipv4.ip_forward') == '1'
        assert remote('cat /proc/sys/fs/inotify/max_user_watches').strip() == watches
        remote(f'rm -f {EARLIER_FILE}')
        # The next boot: systemd-modules-load loads the module and systemd-sysctl
        # applies MMX's files, the setting that needs the module included.
        remote('sysctl -q -w net.ipv4.ip_forward=0 fs.inotify.max_user_instances=128 net.bridge.bridge-nf-call-iptables=0\n'
               'modprobe -r br_netfilter')
        assert not loaded('br_netfilter')
        boot()
        assert loaded('br_netfilter') and live('net.bridge.bridge-nf-call-iptables') == '1'
        assert live('net.ipv4.ip_forward') == '1' and live('fs.inotify.max_user_instances') == '1048576'
        mark('the run loads br_netfilter and sets the three settings now; reruns are unchanged; only the named settings are '
             'applied; systemd-modules-load and systemd-sysctl apply the same at the next boot')

        # A part with a dot in it: the name is written with slashes.
        remote(f'ip link add {DOTTED_INTERFACE} type bridge')
        dotted = f'net/ipv4/conf/{DOTTED_INTERFACE}/rp_filter'
        project([{'sysctl_setting': {'name': dotted, 'value': '2'}}])
        run('run')
        assert remote(f'cat /proc/sys/{dotted}').strip() == '2'
        assert text(f'/etc/sysctl.d/90-mmx-net.ipv4.conf.{DOTTED_INTERFACE}.rp_filter.conf') == f'{dotted} = 2\n'
        assert change(run('run')) == 'unchanged'
        remote(f'echo 0 > /proc/sys/{dotted}')
        boot()
        assert remote(f'cat /proc/sys/{dotted}').strip() == '2'
        mark('a name with a dotted part, net/ipv4/conf/mmx0.10/rp_filter, sets that interface\'s setting now and at boot')

        # Values compare word by word: the kernel separates these with a tab.
        ports = live('net.ipv4.ip_local_port_range')
        assert '\t' in remote('cat /proc/sys/net/ipv4/ip_local_port_range'), ports
        project([{'sysctl_setting': {'name': 'net.ipv4.ip_local_port_range', 'value': ' '.join(ports.split())}}])
        first = results(run('run'))[0]
        assert first['change'] == 'changed' and 'value' not in first, first
        assert change(run('run')) == 'unchanged'
        mark('a value is compared word by word, so "32768 60999" matches the kernel\'s tab and only the file is written')

        # A name the kernel lacks fails before any change; a value it refuses
        # fails with its message.
        project([{'sysctl_setting': {'name': 'net.ipv4.mmx_no_such_setting', 'value': '1'}}])
        assert results(run('dryrun'))[0]['status'] == 'not_checked'
        failed = results(run('run', 1))[0]
        assert 'check the setting name' in failed['reason'], failed
        assert text('/etc/sysctl.d/90-mmx-net.ipv4.mmx_no_such_setting.conf') == ''
        project([{'sysctl_setting': {'name': 'net.ipv4.ip_forward', 'value': 'yes'}}])
        refused = results(run('run', 1))[0]
        assert 'the kernel refused net.ipv4.ip_forward = yes' in refused['reason'] and 'invalid argument' in refused['reason'], refused
        assert live('net.ipv4.ip_forward') == '1' and text('/etc/sysctl.d/90-mmx-net.ipv4.ip_forward.conf') == 'net.ipv4.ip_forward = 1\n'
        mark('a name the kernel lacks is not checked in a dryrun and fails a run without a file; a value the kernel refuses '
             'fails with the kernel\'s message and changes nothing')

        # The file applied last wins at boot, in systemd-sysctl's order.
        forward = [{'sysctl_setting': {'name': 'net.ipv4.ip_forward', 'value': '1'}}]
        project(forward)
        remote(f'printf "net.ipv4.ip_forward = 0\\n" > {LATER_FILE}')
        order = remote('systemd-analyze cat-config sysctl.d | grep "^# /"').split()
        assert order.index(LATER_FILE) > order.index('/etc/sysctl.d/90-mmx-net.ipv4.ip_forward.conf'), order
        later = results(run('run', 1))[0]
        assert LATER_FILE + ' sets the managed kernel setting to "0"' in later['reason'], later
        boot()
        assert live('net.ipv4.ip_forward') == '0'
        # A link to /dev/null of the same name in /etc/sysctl.d masks the file.
        remote(f'ln -s /dev/null {LATER_MASK}')
        run('run')
        boot()
        assert live('net.ipv4.ip_forward') == '1'
        remote(f'rm -f {LATER_MASK} {LATER_FILE}\nprintf "net.ipv4.ip_forward = 0\\n" > {EARLIER_FILE}')
        run('run')
        boot()
        assert live('net.ipv4.ip_forward') == '1'
        remote(f'rm -f {EARLIER_FILE}')
        # /etc/sysctl.conf comes last: through 99-sysctl.conf where the
        # distribution links it, and always for sysctl --system.
        linked = remote('readlink -f /etc/sysctl.d/99-sysctl.conf || true').strip() == '/etc/sysctl.conf'
        remote('printf "net.ipv4.ip_forward = 0\\n" >> /etc/sysctl.conf')
        conf = results(run('run', 1))[0]
        named = '/etc/sysctl.d/99-sysctl.conf' if linked else '/etc/sysctl.conf'
        assert (named + ' sets the managed kernel setting to "0"') in conf['reason'], conf
        boot()
        assert live('net.ipv4.ip_forward') == ('0' if linked else '1'), linked
        remote('sysctl -q -w net.ipv4.ip_forward=1\nsysctl -q --system')
        assert live('net.ipv4.ip_forward') == '0'
        remote(f'cp -p {SYSCTL_CONF_BACKUP} /etc/sysctl.conf' if sysctl_conf_existed else 'rm -f /etc/sysctl.conf')
        run('run')
        mark('a later file that sets another value fails the task and names it, in systemd-sysctl\'s order, which '
             'systemd-sysctl itself confirms; a masked or earlier file does not; /etc/sysctl.conf counts as the last file '
             '(%s through 99-sysctl.conf: %s)' % (distribution, linked))

        # Removing the boot setting keeps the live value.
        project([{'sysctl_setting': {'name': 'net.ipv4.ip_forward', 'state': 'boot_setting_removed_live_value_kept'}}])
        assert change(run('dryrun')) == 'would_change' and text('/etc/sysctl.d/90-mmx-net.ipv4.ip_forward.conf') != ''
        run('run')
        assert text('/etc/sysctl.d/90-mmx-net.ipv4.ip_forward.conf') == '' and live('net.ipv4.ip_forward') == '1'
        assert change(run('run')) == 'unchanged'
        mark('boot_setting_removed_live_value_kept deletes MMX\'s file and leaves the live value')

        # Options: a module loaded with other values keeps them. The task writes
        # the files and fails; once the module is unloaded, resume loads it again
        # with the options.
        remote('modprobe vxlan')
        assert remote('cat /sys/module/vxlan/parameters/udp_port').strip() == '8472'
        project([{'kernel_module': {'name': 'vxlan', 'options': {'udp_port': '4789'}}}])
        preview = results(run('dryrun', 1))[0]
        assert preview['status'] == 'failed' and 'udp_port is 8472, not 4789' in preview['reason'], preview
        assert 'MMX would write the requested options to /etc/modprobe.d/mmx-vxlan.conf' in preview['reason'], preview
        assert text('/etc/modprobe.d/mmx-vxlan.conf') == ''
        failed = run('run', 1)
        reason = results(failed)[0]['reason']
        assert 'unload the module with modprobe -r vxlan' in reason and 'reboot the node' in reason, reason
        assert text('/etc/modprobe.d/mmx-vxlan.conf') == 'options vxlan udp_port=4789\n'
        assert text('/etc/modules-load.d/mmx-vxlan.conf') == 'vxlan\n'
        assert loaded('vxlan') and remote('cat /sys/module/vxlan/parameters/udp_port').strip() == '8472'
        remote('modprobe -r vxlan')
        resumed = run('resume')
        assert resumed['id'] == failed['id'] and resumed['status'] == 'succeeded', resumed
        assert loaded('vxlan') and remote('cat /sys/module/vxlan/parameters/udp_port').strip() == '4789'
        assert change(run('run')) == 'unchanged' and change(run('dryrun', 0)) == 'unchanged'
        mark('a module loaded with other options keeps them: the task writes both files and fails, saying to unload or '
             'reboot; after modprobe -r, resume loads it with udp_port=4789; reruns are unchanged')

        project([{'kernel_module': {'name': 'vxlan'}}])
        assert change(run('dryrun')) == 'unchanged' and change(run('run')) == 'unchanged'
        assert text('/etc/modprobe.d/mmx-vxlan.conf') == 'options vxlan udp_port=4789\n'
        project([{'kernel_module': {'name': 'vxlan', 'options': {}}}])
        assert change(run('dryrun')) == 'would_change'
        assert text('/etc/modprobe.d/mmx-vxlan.conf') == 'options vxlan udp_port=4789\n'
        assert change(run('run')) == 'changed' and text('/etc/modprobe.d/mmx-vxlan.conf') == ''
        assert loaded('vxlan') and remote('cat /sys/module/vxlan/parameters/udp_port').strip() == '4789'
        mark('omitted options keep the saved options file; an empty mapping deletes only that file and keeps the loaded module')

        # unloaded deletes both files and unloads; a module in use fails with
        # modprobe's message.
        project([{'kernel_module': {'name': 'vxlan', 'state': 'unloaded'}}])
        preview = results(run('dryrun'))[0]
        assert preview['change'] == 'would_change' and preview['value']['loaded_after'] is False and loaded('vxlan'), preview
        run('run')
        assert not loaded('vxlan') and text('/etc/modprobe.d/mmx-vxlan.conf') == '' and text('/etc/modules-load.d/mmx-vxlan.conf') == ''
        assert change(run('run')) == 'unchanged'
        project([{'kernel_module': {'name': 'bridge', 'state': 'unloaded'}}])
        busy = results(run('run', 1))[0]
        assert 'in use' in busy['reason'] and 'modprobe -r bridge failed: ' in busy['reason'], busy
        assert loaded('bridge')
        mark('state: unloaded deletes both files and unloads the module; a module in use (bridge, used by br_netfilter) '
             'fails with modprobe\'s message')

        # A module that does not exist, and one built into the kernel.
        project([{'kernel_module': {'name': 'mmx_no_such_module'}}])
        assert results(run('dryrun'))[0]['status'] == 'not_checked'
        missing = results(run('run', 1))[0]
        assert 'not found' in missing['reason'], missing
        assert text('/etc/modules-load.d/mmx-mmx_no_such_module.conf') == ''
        project([{'kernel_module': {'name': builtin}}])
        first = results(run('run'))[0]
        assert first['status'] == 'succeeded' and 'loaded_after' not in first.get('value', {}), first
        assert change(run('run')) == 'unchanged'
        project([{'kernel_module': {'name': builtin, 'options': {'mmx_option': '1'}}}])
        assert 'kernel command line' in results(run('run', 1))[0]['reason']
        project([{'kernel_module': {'name': builtin, 'state': 'unloaded'}}])
        assert 'cannot be unloaded' in results(run('run', 1))[0]['reason']
        mark('a module that does not exist fails with modprobe\'s message and leaves no file; a built-in module (%s) counts '
             'as loaded, refuses options, and cannot be unloaded' % builtin)
    finally:
        cleanup()
        if installed_modules_package:
            remote('dnf -y -q remove "kernel-modules-extra-$(uname -r)"')
    print(f'{len(passed)} kernel setting and kernel module workflows passed', flush=True)


if __name__ == '__main__':
    vm.run_suite(main)
