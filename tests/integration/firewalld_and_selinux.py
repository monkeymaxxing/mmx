#!/usr/bin/env python3
"""Public CLI checks of the firewalld module and the four SELinux modules.

The firewalld checks run on the Red Hat family, in a zone of their own that
no network interface or source uses, so no rule they write can reach the
harness's SSH connection. The SELinux checks run where SELinux is enabled; on
another node every SELinux module reports skipped. The suite leaves firewalld
running or stopped, and SELinux enforcing or permissive, as it found them.
"""
import json
import shlex
import shutil

import vm

ZONE = 'mmx-test'
WEB = '/srv/mmx-selinux-web'
MISSING = '/srv/mmx-selinux-missing'
BOOLEAN = 'httpd_can_network_connect'
NEW_PORT = 18443
# websm_port_t in the base policy of every Red Hat family release.
BASE_PORT = 9090


def main():
    vm.up()
    node, ssh = vm.connection()
    vm.reset()
    workspace = vm.ROOT / 'firewalld-and-selinux'
    if workspace.exists():
        shutil.rmtree(workspace)
    vm.build()
    mmx = vm.ROOT / 'bin/mmx'
    passed = []

    def remote(script):
        return vm.cmd([*ssh, 'sudo -n /bin/sh -c ' + shlex.quote('set -eu\n' + script)]).stdout.strip()

    def inventory(app_servers):
        """The test node, and two application servers MMX never connects to
        that give their private addresses in their vars."""
        vm.write_inventory(workspace, {
            'nodes': {'node': node,
                      'app01': {'address': '192.0.2.11', 'vars': {'private_address': '10.0.1.11'}},
                      'app02': {'address': '192.0.2.12', 'vars': {'private_address': '10.0.1.12'}},
                      'cache01': {'address': '192.0.2.21'}},
            'groups': {'app': {'nodes': app_servers}, 'cache': {'nodes': ['cache01']}}}, name='example')

    def tasks(items):
        vm.write_project(workspace, project_name='firewalld_and_selinux', fleet_stages=[
            {'id': 'configure', 'nodes': 'node', 'node_tasks': items}])

    def run(command, expected=None):
        result = vm.cmd([mmx, command, *vm.playbook_args(command), '--json'], cwd=workspace, check=False)
        assert result.returncode in (vm.success_codes(command) if expected is None else (expected,)), (command, result.stdout, result.stderr)
        return json.loads(result.stdout)

    def last(command, expected=None):
        """The result of the last node task."""
        return run(command, expected)['entries'][0]['response']['results'][-1]

    def mark(name):
        passed.append(name)
        print('PASS ' + name, flush=True)

    inventory(['app01', 'app02'])
    family = remote('. /etc/os-release; echo "$ID ${ID_LIKE:-}"').split()
    red_hat = 'rhel' in family
    selinux = remote(f'if {vm.SELINUX_RUNS}; then echo on; fi') == 'on'

    if not selinux:
        tasks([{'selinux_boolean': {'name': BOOLEAN, 'enabled': True}},
               {'selinux_file_context': {'path': WEB, 'also_apply_to_everything_inside': True, 'selinux_type': 'httpd_sys_content_t'}},
               {'selinux_port': {'port': NEW_PORT, 'protocol': 'tcp', 'selinux_type': 'http_port_t'}},
               {'selinux_mode': {'mode': 'enforcing'}}])
        for command in ('dryrun', 'run'):
            results = run(command)['entries'][0]['response']['results']
            assert [(r['status'], r['reason']) for r in results] == [('skipped', 'SELinux is not enabled on this node')] * 4, results
        mark('every SELinux module reports skipped, SELinux is not enabled on this node, in dryrun and run')

    if red_hat:
        firewalld_checks(remote, tasks, inventory, run, last, mark, mmx, workspace)
    if selinux:
        selinux_checks(remote, tasks, run, last, mark)
    print(f'{len(passed)} firewalld and SELinux workflows passed', flush=True)


def firewalld_checks(remote, tasks, inventory, run, last, mark, mmx, workspace):
    found = remote('if rpm -q firewalld >/dev/null 2>&1; then echo installed; fi; '
                   'if systemctl is-active --quiet firewalld; then echo active; fi; '
                   'if systemctl is-enabled --quiet firewalld 2>/dev/null; then echo enabled; fi').split()

    def zone(permanent=False):
        """What the test zone admits, read with firewall-cmd."""
        option = '--permanent ' if permanent else ''
        listed = {kind: remote(f'firewall-cmd {option}--zone={ZONE} --list-{kind}') for kind in ('services', 'ports', 'rich-rules')}
        return {'services': sorted(listed['services'].split()), 'ports': sorted(listed['ports'].split()),
                'rich-rules': sorted(line for line in listed['rich-rules'].splitlines() if line)}

    def both_layers():
        running, stored = zone(), zone(permanent=True)
        assert running == stored, (running, stored)
        return running

    def cleanup():
        remote(f'if systemctl is-active --quiet firewalld; then\n'
               f' if firewall-cmd --permanent --get-zones | grep -qw {ZONE}; then firewall-cmd --permanent --delete-zone={ZONE} >/dev/null; firewall-cmd --reload >/dev/null; fi\n'
               'fi')

    try:
        # The design's own first tasks: install firewalld, then start it.
        tasks([{'dnf_package': {'packages': [{'name': 'firewalld', 'version': 'newest_when_first_installed'}]}}, {'systemd_service': {'name': 'firewalld', "state": "running"}}])
        run('run')
        cleanup()
        remote(f'firewall-cmd --permanent --new-zone={ZONE} >/dev/null; firewall-cmd --reload >/dev/null\n'
               f'firewall-cmd --permanent --zone={ZONE} --add-service=cockpit >/dev/null; firewall-cmd --zone={ZONE} --add-service=cockpit >/dev/null\n'
               # Lost at any reload of firewalld, so it shows that MMX never reloads.
               f'firewall-cmd --zone={ZONE} --add-masquerade >/dev/null')
        group_rule = {'action': 'allow', 'firewalld_service': 'postgresql', 'from_inventory_group': {'group': 'app', 'address_var': 'private_address'}}
        firewalld = {'zone': ZONE, 'rules_not_listed': 'remove', 'rules': [
            {'action': 'allow', 'firewalld_service': 'ssh'},
            {'action': 'allow', 'port': 7443, 'protocol': 'tcp'},
            {'action': 'allow', 'port': '8000:8010', 'protocol': 'udp', 'from': '10.0.0.0/8'},
            {'action': 'reject', 'port': 23, 'protocol': 'tcp'},
            group_rule]}
        tasks([{'firewalld': firewalld}])
        rich = ['rule family="ipv4" source address="10.0.0.0/8" port port="8000-8010" protocol="udp" accept',
                'rule port port="23" protocol="tcp" reject',
                'rule family="ipv4" source address="10.0.1.11" service name="postgresql" accept',
                'rule family="ipv4" source address="10.0.1.12" service name="postgresql" accept']
        expected = {'added': ['service ssh', 'port 7443/tcp', *rich], 'removed': ['service cockpit']}
        before = both_layers()
        preview = last('dryrun')
        assert preview['change'] == 'would_change' and preview['value'] == expected, preview
        assert both_layers() == before
        applied = last('run')
        assert applied['change'] == 'changed' and applied['value'] == expected, applied
        assert both_layers() == {'services': ['ssh'], 'ports': ['7443/tcp'], 'rich-rules': sorted(rich)}, both_layers()
        assert remote(f'firewall-cmd --zone={ZONE} --query-masquerade') == 'yes', 'firewalld was reloaded'
        assert last('run')['change'] == 'unchanged' and last('dryrun', 0)['change'] == 'unchanged'
        mark('firewalld writes services, ports, and rich rules to the running firewall and the stored configuration without a reload, one rule for each node of an inventory group; reruns and previews are unchanged')

        # A node that leaves the group loses its rule; one rule lost from the
        # stored configuration only is written there again.
        inventory(['app01'])
        remote(f"firewall-cmd --permanent --zone={ZONE} --remove-port=7443/tcp >/dev/null")
        applied = last('run')
        assert applied['value'] == {'added': ['port 7443/tcp'], 'removed': [rich[3]]}, applied
        assert both_layers() == {'services': ['ssh'], 'ports': ['7443/tcp'], 'rich-rules': sorted(rich[:3])}, both_layers()
        assert last('run')['change'] == 'unchanged'
        mark('a node that leaves the inventory group loses its rule on the next run, and a rule missing from the stored configuration is written there again')

        # A missing address is refused before MMX connects.
        tasks([{'firewalld': {'zone': ZONE, 'rules_not_listed': 'keep', 'rules': [
            {'action': 'allow', 'firewalld_service': 'postgresql', 'from_inventory_group': {'group': 'cache', 'address_var': 'private_address'}}]}}])
        refused = vm.cmd([mmx, 'dryrun', 'configure'], cwd=workspace, check=False)
        assert refused.returncode != 0 and 'node cache01 in inventory group cache has no variable private_address' in refused.stderr, (refused.stdout, refused.stderr)

        # Another action replaces the rule with the same identity; keep leaves the rest.
        tasks([{'firewalld': {'zone': ZONE, 'rules_not_listed': 'keep', 'rules': [{'action': 'drop', 'port': 7443, 'protocol': 'tcp'}]}}])
        applied = last('run')
        assert applied['value'] == {'added': ['rule port port="7443" protocol="tcp" drop'], 'removed': ['port 7443/tcp']}, applied
        assert both_layers() == {'services': ['ssh'], 'ports': [], 'rich-rules': sorted([*rich[:3], 'rule port port="7443" protocol="tcp" drop'])}
        tasks([{'firewalld': {'zone': ZONE, 'state': 'absent', 'rules': [{'port': 7443, 'protocol': 'tcp'}, {'port': '8000:8010', 'protocol': 'udp', 'from': '10.0.0.0/8'}]}}])
        removed = last('run')['value']['removed']
        assert sorted(removed) == sorted(['rule port port="7443" protocol="tcp" drop', rich[0]]), removed
        assert both_layers() == {'services': ['ssh'], 'ports': [], 'rich-rules': sorted(rich[1:3])}
        assert last('run')['change'] == 'unchanged'
        mark('firewalld replaces a rule whose action changed, keep leaves other rules, and absent removes the listed rules whatever their action')

        before = both_layers()
        ssh = {'action': 'allow', 'firewalld_service': 'ssh'}
        for refused_rules, reason in (
                ({'zone': ZONE, 'rules_not_listed': 'remove', 'rules': [{'action': 'allow', 'port': 7443, 'protocol': 'tcp'}]}, "no longer admit sshd's TCP port 22"),
                ({'zone': ZONE, 'state': 'absent', 'rules': [{'firewalld_service': 'ssh'}]}, "no longer admit sshd's TCP port 22"),
                # A rule that admits a source on every port names no port.
                ({'zone': ZONE, 'rules_not_listed': 'remove', 'rules': [{'action': 'allow', 'from': '192.0.2.50'}, {'action': 'allow', 'port': 443, 'protocol': 'tcp'}]},
                 'a rule with only from does not count'),
                # firewalld applies reject and drop rich rules before services and ports.
                ({'zone': ZONE, 'rules_not_listed': 'keep', 'rules': [ssh, {'action': 'reject', 'port': 22, 'protocol': 'tcp'}]},
                 "would reject sshd's TCP port 22 for every source"),
                ({'zone': ZONE, 'rules_not_listed': 'keep', 'rules': [{'action': 'allow', 'port': 22, 'protocol': 'tcp'}, {'action': 'drop', 'firewalld_service': 'ssh'}]},
                 "would drop sshd's TCP port 22 for every source")):
            tasks([{'firewalld': refused_rules}])
            for command in ('dryrun', 'run'):
                result = last(command, 1)
                assert result['failure_kind'] == 'config' and reason in result['reason'], result
        assert both_layers() == before
        # A reject limited to one address leaves SSH admitted.
        tasks([{'firewalld': {'zone': ZONE, 'rules_not_listed': 'keep', 'rules': [ssh, {'action': 'reject', 'port': 22, 'protocol': 'tcp', 'from': '198.51.100.7'}]}}])
        assert last('run')['value'] == {'added': ['rule family="ipv4" source address="198.51.100.7" port port="22" protocol="tcp" reject']}
        mark('the firewalld SSH safeguard refuses, in dryrun too, a change after which the zone no longer admits sshd, admits it only through a source on every port, '
             'or rejects or drops it for every source, and changes nothing; a reject limited to one address is allowed')
    finally:
        cleanup()
        # The service stays as the suite found it, whatever the package's installation did to it.
        remote(('' if 'active' in found else 'systemctl stop firewalld\n') +
               ('' if 'enabled' in found else 'systemctl disable --quiet firewalld 2>/dev/null || true\n') + 'true')


def selinux_checks(remote, tasks, run, last, mark):
    # Enforcing on the Red Hat family, as installed.
    found_mode = remote('getenforce').lower()
    other_mode = 'permissive' if found_mode == 'enforcing' else 'enforcing'

    def label(path):
        return remote(f'stat -c %C {path}').split(':')[2]

    def cleanup():
        remote(f'setsebool -P {BOOLEAN} off\n'
               f'for port in {NEW_PORT} {BASE_PORT}; do semanage port --delete --proto tcp $port 2>/dev/null || true; done\n'
               f"for pattern in '{WEB}(/.*)?' '{MISSING}'; do semanage fcontext --delete \"$pattern\" 2>/dev/null || true; done\n"
               f'rm -rf {WEB} {MISSING}\n'
               f'setenforce {found_mode}\nsed -i "s/^SELINUX=.*/SELINUX={found_mode}/" /etc/selinux/config')

    try:
        print('SELinux programs on this node before the suite: ' + remote(
            'for program in semanage setsebool restorecon matchpathcon getenforce; do\n'
            ' if path=$(command -v $program); then printf "%s from %s; " $program "$(rpm -qf $path 2>/dev/null || dpkg -S $path 2>/dev/null)"; '
            'else printf "%s missing; " $program; fi\ndone'), flush=True)
        cleanup()
        # Without semanage the task names the package to install.
        if remote('command -v semanage || true') == '':
            tasks([{'selinux_port': {'port': NEW_PORT, 'protocol': 'tcp', 'selinux_type': 'http_port_t'}}])
            assert last('dryrun')['status'] == 'not_checked'
            failed = last('run', 1)
            assert 'policycoreutils-python-utils' in failed['reason'], failed
            mark('without semanage an SELinux module fails and names the package policycoreutils-python-utils')
        install = {'dnf_package': {'packages': [{'name': 'policycoreutils-python-utils', 'version': 'newest_when_first_installed'}]}}
        tasks([install])
        run('run')

        boolean = {'selinux_boolean': {'name': BOOLEAN, 'enabled': True}}
        tasks([install, boolean])
        assert remote(f'getsebool {BOOLEAN}').endswith('off')
        assert last('dryrun')['change'] == 'would_change' and remote(f'getsebool {BOOLEAN}').endswith('off')
        assert last('run')['change'] == 'changed'
        assert remote(f'getsebool {BOOLEAN}').endswith('on')
        assert remote(f"semanage boolean --list --noheading | grep '^{BOOLEAN} '").split('(')[1].replace(' ', '').startswith('on,on'), 'not stored'
        assert last('run')['change'] == 'unchanged' and last('dryrun', 0)['change'] == 'unchanged'
        # Set by hand for the running node only: the stored value is set too.
        remote(f'setsebool {BOOLEAN} off')
        assert last('run')['change'] == 'changed' and remote(f'getsebool {BOOLEAN}').endswith('on')
        mark('selinux_boolean sets the boolean now and in the stored policy, then reruns unchanged')

        def local_ports():
            return remote('semanage port --list --noheading --locallist')

        new = {'selinux_port': {'port': NEW_PORT, 'protocol': 'tcp', 'selinux_type': 'http_port_t'}}
        base = {'selinux_port': {'port': BASE_PORT, 'protocol': 'tcp', 'selinux_type': 'http_port_t'}}
        tasks([install, {'selinux_port': {'port': 8443, 'protocol': 'tcp', 'selinux_type': 'http_port_t'}}, new, base])
        preview = run('dryrun')['entries'][0]['response']['results']
        assert [r['change'] for r in preview[1:]] == ['unchanged', 'would_change', 'would_change'], preview
        assert local_ports() == ''
        assert [r['change'] for r in run('run')['entries'][0]['response']['results'][1:]] == ['unchanged', 'changed', 'changed']
        assert f'{NEW_PORT}' in local_ports() and f'{BASE_PORT}' in local_ports() and 'http_port_t' in local_ports(), local_ports()
        assert run('run')['entries'][0]['response']['change'] == 'unchanged'
        tasks([install, {'selinux_port': {'port': NEW_PORT, 'protocol': 'tcp', 'state': 'absent'}},
               {'selinux_port': {'port': BASE_PORT, 'protocol': 'tcp', 'state': 'absent'}}])
        assert [r['change'] for r in run('run')['entries'][0]['response']['results'][1:]] == ['changed', 'changed']
        assert local_ports() == ''
        assert f'{BASE_PORT}' in remote("semanage port --list --noheading | grep '^websm_port_t'")
        assert run('run')['entries'][0]['response']['change'] == 'unchanged'
        mark('selinux_port adds a port, changes a base policy port, leaves a port that has the type, and absent removes the local definitions')

        remote(f'mkdir -p {WEB}/assets; echo hello > {WEB}/index.html; echo body > {WEB}/assets/app.css')
        paths = [WEB, WEB + '/index.html', WEB + '/assets', WEB + '/assets/app.css']
        found_label = label(WEB)
        assert found_label != 'httpd_sys_content_t'
        context = {'selinux_file_context': {'path': WEB, 'also_apply_to_everything_inside': True, 'selinux_type': 'httpd_sys_content_t'}}
        tasks([install, context])
        preview = last('dryrun')
        assert preview['change'] == 'would_change' and preview['value'] == {'rule': 'added'}, preview
        assert remote(f"semanage fcontext --list --noheading --locallist | grep -c '^{WEB}' || true") == '0'
        applied = last('run')
        assert applied['change'] == 'changed' and applied['value'] == {'rule': 'added', 'paths_relabeled': 4}, applied
        assert [label(p) for p in paths] == ['httpd_sys_content_t'] * 4
        assert remote(f'restorecon -n -v -R {WEB}') == ''
        assert last('run')['change'] == 'unchanged' and last('dryrun', 0)['change'] == 'unchanged'
        # A label changed by hand is repaired on the next run.
        remote(f'chcon -t {found_label} {WEB}/index.html')
        preview = last('dryrun')
        assert preview['change'] == 'would_change' and preview['value'] == {'paths_relabeled': 1}, preview
        assert label(WEB + '/index.html') == found_label
        assert last('run')['value'] == {'paths_relabeled': 1} and label(WEB + '/index.html') == 'httpd_sys_content_t'
        mark('selinux_file_context adds the rule and relabels the existing files, repairs a label changed by hand, and reruns unchanged')

        tasks([install, {'selinux_file_context': {'path': WEB, 'also_apply_to_everything_inside': True, 'state': 'absent'}},
               {'selinux_file_context': {'path': MISSING, 'also_apply_to_everything_inside': False, 'selinux_type': 'httpd_sys_content_t'}}])
        results = run('run')['entries'][0]['response']['results']
        assert results[1]['value'] == {'rule': 'removed', 'paths_relabeled': 4} and results[2]['value'] == {'rule': 'added'}, results
        assert [label(p) for p in paths] == [found_label] * 4
        assert run('run')['entries'][0]['response']['change'] == 'unchanged'
        mark('selinux_file_context absent removes the rule and gives the files their policy label again; a missing path is not an error')

        def mode():
            return remote('getenforce; grep "^SELINUX=" /etc/selinux/config')

        def shown(name):
            return f'{name.capitalize()}\nSELINUX={name}'

        tasks([{'selinux_mode': {'mode': found_mode}}])
        assert mode() == shown(found_mode), mode()
        assert last('run')['change'] == 'unchanged'
        tasks([{'selinux_mode': {'mode': other_mode}}])
        assert last('dryrun')['change'] == 'would_change' and mode() == shown(found_mode)
        assert last('run')['change'] == 'changed' and mode() == shown(other_mode)
        assert last('run')['change'] == 'unchanged'
        tasks([{'selinux_mode': {'mode': found_mode}}])
        assert last('run')['change'] == 'changed' and mode() == shown(found_mode)
        assert last('run')['change'] == 'unchanged' and last('dryrun', 0)['change'] == 'unchanged'
        mark('selinux_mode sets the running mode and /etc/selinux/config, then reruns unchanged')
    finally:
        cleanup()


if __name__ == '__main__':
    vm.run_suite(main)
