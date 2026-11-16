#!/usr/bin/env python3
"""file_access_control_list and linux_capability_on_program_file through the public CLI, checked with getfacl and getcap."""
import json
import shlex
import shutil

import vm

BASE = '/srv/mmx-acl'
PROGRAM = '/usr/local/bin/mmx-capability-test'
ACCOUNT = 'mmx-acl-reader'
GROUP = 'mmx-acl-team'


def main():
    vm.up()
    node, ssh = vm.connection()
    vm.reset()
    workspace = vm.ROOT / 'access-control'
    if workspace.exists():
        shutil.rmtree(workspace)
    vm.write_inventory(workspace, {'nodes': {'app': node}}, name='example')
    vm.build()
    mmx = vm.ROOT / 'bin/mmx'
    passed = []

    def remote(script):
        return vm.cmd([*ssh, 'sudo -n /bin/sh -c ' + shlex.quote('set -eu\n' + script)]).stdout

    def run(command, expected=None):
        result = vm.cmd([mmx, command, *vm.playbook_args(command), '--json'], cwd=workspace, check=False)
        assert result.returncode in (vm.success_codes(command) if expected is None else (expected,)), (command, result.stdout, result.stderr)
        return json.loads(result.stdout)

    def project(tasks):
        vm.write_project(workspace, project_name='access_control', node_task_defaults={'retry': {'pause_between_tries': '0s'}},
                         fleet_stages=[{'id': 'configure', 'nodes': 'all', 'node_tasks': tasks}])

    def results(record):
        return record['entries'][0]['response']['results']

    def change(record):
        return record['entries'][0]['response']['change']

    def mark(message):
        passed.append(message)
        print('PASS ' + message, flush=True)

    def acl(path, default=False):
        """The named entries getfacl prints for path."""
        lines = remote(f'getfacl --omit-header --absolute-names --skip-base --no-effective {"--default " if default else ""}{shlex.quote(path)} 2>/dev/null || true')
        return sorted(line for line in lines.splitlines() if line.count(':') == 2 and line.split(':')[1] and not line.startswith('#'))

    def cleanup():
        remote(f'rm -rf {BASE} {PROGRAM}\nuserdel {ACCOUNT} 2>/dev/null || true\ngroupdel {GROUP} 2>/dev/null || true')

    distribution = remote('. /etc/os-release; echo "$ID"').strip()
    debian_family = remote('command -v dpkg >/dev/null && echo yes || true').strip() == 'yes'
    # The package module and setcap's package of this node's package manager.
    package_manager = remote('for tool in apt-get dnf; do command -v $tool >/dev/null && { echo $tool; break; }; done').strip()
    package_module, capability_package = {'apt-get': ('apt_package', 'libcap2-bin'), 'dnf': ('dnf_package', 'libcap')}[package_manager]
    cleanup()
    try:
        # Where the node lets the packages go, a missing tool names the package
        # to install; a package task first in the same run fixes it.
        if debian_family:
            # dpkg refuses to remove a package that another installed package needs.
            remote('for package in acl libcap2-bin; do dpkg --remove $package >/dev/null 2>&1 || true; done')
        missing = {tool: remote(f'command -v {tool} || true').strip() == '' for tool in ('setfacl', 'setcap')}
        remote(f'mkdir -p {BASE}/sub\nprintf data > {BASE}/file\nprintf data > {BASE}/sub/nested\nln -s /etc/hostname {BASE}/link\n'
               f'useradd --system -M {ACCOUNT}\ngroupadd --system {GROUP}\ncp /usr/bin/true {PROGRAM}')
        tasks = [{'file_access_control_list': {'path': BASE, 'entries': [{'user_account': ACCOUNT, 'permissions': 'r-x'}, {'group': GROUP, 'permissions': 'rwx'}],
                                               'entries_not_listed': 'remove', 'also_apply_to_everything_inside': True, 'also_use_for_new_files_inside': True}},
                 {'linux_capability_on_program_file': {'capabilities_not_listed': 'remove', 'path': PROGRAM, 'capabilities': ['cap_net_raw']}}]
        if missing['setfacl'] or missing['setcap']:
            project(tasks)
            preview = results(run('dryrun'))
            for index, tool, package in [(0, 'setfacl', 'the package acl'), (1, 'setcap', capability_package)]:
                if missing[tool]:
                    assert preview[index]['status'] == 'not_checked' and package in preview[index]['reason'], preview[index]
            failed = run('run', 1)
            assert 'install the package' in json.dumps(failed), failed
            project([{package_module: {'packages': [{'name': 'acl', 'version': 'newest_when_first_installed'}, {'name': capability_package, 'version': 'newest_when_first_installed'}]}}] + tasks)
            run('run')
            mark('a missing setfacl or setcap is not checked in a preview, fails a run with the package to install, and a '
                 'package task first in the same run fixes it (%s)' % distribution)
        else:
            mark('getfacl, setfacl, getcap, and setcap are part of this node\'s base system (%s)' % distribution)

        # Entries for the directory, everything inside it, and new files in
        # it. A symbolic link inside is never followed.
        project(tasks[:1])
        remote(f'setfacl -b -R {BASE}')
        hostname_acl = remote('getfacl --absolute-names /etc/hostname')
        before = remote(f'getfacl -R --absolute-names {BASE}')
        preview = run('dryrun')
        assert results(preview)[0]['value'] == {
            'added': [f'user_account {ACCOUNT}: r-x', f'group {GROUP}: rwx', f'default user_account {ACCOUNT}: r-x', f'default group {GROUP}: rwx'],
            'number_of_paths_changed': 4}, results(preview)[0]
        assert remote(f'getfacl -R --absolute-names {BASE}') == before
        run('run')
        expected = [f'group:{GROUP}:rwx', f'user:{ACCOUNT}:r-x']
        for path in (BASE, BASE + '/sub', BASE + '/file', BASE + '/sub/nested'):
            assert acl(path) == expected, (path, acl(path))
        assert acl(BASE, default=True) == expected and acl(BASE + '/sub', default=True) == expected
        assert acl(BASE + '/file', default=True) == []
        assert remote('getfacl --absolute-names /etc/hostname') == hostname_acl
        remote(f'touch {BASE}/sub/new')
        assert acl(BASE + '/sub/new') == expected
        assert change(run('run')) == 'unchanged' and change(run('dryrun', 0)) == 'unchanged'
        mark('file_access_control_list sets the entries on a directory, everything inside, and as default entries that '
             'new files inherit; a symbolic link inside is not followed; the preview changes nothing; reruns unchanged')

        # remove takes away an entry the task does not list; keep leaves it.
        remote(f'setfacl -m u:daemon:rw- {BASE}/file')
        project([{'file_access_control_list': dict(tasks[0]['file_access_control_list'], entries_not_listed='keep')}])
        assert change(run('run')) == 'unchanged' and 'user:daemon:rw-' in acl(BASE + '/file')
        project(tasks[:1])
        removed = results(run('run'))[0]
        assert removed['value'] == {'removed': ['user_account daemon: rw-'], 'number_of_paths_changed': 1}, removed
        assert acl(BASE + '/file') == expected
        # A changed permission is corrected in place.
        project([{'file_access_control_list': {'path': BASE + '/file', 'entries': [{'user_account': ACCOUNT, 'permissions': 'rw-'}],
                                               'entries_not_listed': 'keep'}}])
        assert results(run('run'))[0]['value'] == {'updated': [f'user_account {ACCOUNT}: rw-']}
        assert f'user:{ACCOUNT}:rw-' in acl(BASE + '/file') and acl(BASE + '/sub/nested') == expected
        mark('entries_not_listed: remove takes away unlisted entries and keep leaves them; a changed permission is updated')

        # state: absent removes every named entry, and the default list.
        project([{'file_access_control_list': {'path': BASE, 'state': 'absent', 'also_apply_to_everything_inside': True,
                                               'also_use_for_new_files_inside': True}}])
        run('run')
        assert remote(f'getfacl -R --skip-base --absolute-names {BASE}').strip() == ''
        assert '+' not in remote(f'ls -ld {BASE} {BASE}/file {BASE}/sub/nested | cut -c1-11')
        assert change(run('run')) == 'unchanged'
        mark('state: absent removes every named entry and the default entries, leaving plain permissions; reruns unchanged')

        # Missing things: an account an earlier task may create, a path, and
        # the settings that are only for directories.
        project([{'file_access_control_list': {'path': BASE, 'entries': [{'user_account': 'mmx-acl-missing', 'permissions': 'r--'}], 'entries_not_listed': 'keep'}}])
        assert results(run('dryrun'))[0]['status'] == 'not_checked'
        run('run', 1)
        project([{'file_access_control_list': {'path': BASE + '/file', 'entries': [], 'entries_not_listed': 'remove', 'also_use_for_new_files_inside': True}}])
        assert 'require a directory' in json.dumps(run('run', 1))
        project([{'file_access_control_list': {'path': BASE + '/link', 'entries': [], 'entries_not_listed': 'remove'}}])
        assert 'symbolic link' in json.dumps(run('run', 1))
        mark('a missing account is not checked in a preview and fails a run; the also_ settings refuse a file; a symbolic link at path fails')

        # Capabilities: the whole set, permitted and effective.
        project(tasks[1:])
        remote(f'setcap -r {PROGRAM} 2>/dev/null || true')
        preview = results(run('dryrun'))[0]
        assert preview['value'] == {'capabilities_before': '', 'capabilities_after': 'cap_net_raw=ep'}, preview
        assert remote(f'getcap {PROGRAM}').strip() == ''
        run('run')
        assert remote(f'getcap {PROGRAM}').split()[-1].replace('+', '=') in ('cap_net_raw=ep',)
        assert change(run('run')) == 'unchanged' and change(run('dryrun', 0)) == 'unchanged'
        project([{'linux_capability_on_program_file': {'capabilities_not_listed': 'remove', 'path': PROGRAM, 'capabilities': ['cap_net_raw', 'cap_net_bind_service']}}])
        run('run')
        assert remote(f'getcap {PROGRAM}').split()[-1].replace('+', '=') == 'cap_net_bind_service,cap_net_raw=ep'
        project([{'linux_capability_on_program_file': {'path': PROGRAM, 'capabilities': ['cap_chown'], 'capabilities_not_listed': 'keep'}}])
        run('run')
        assert remote(f'getcap {PROGRAM}').split()[-1].replace('+', '=') == 'cap_chown,cap_net_bind_service,cap_net_raw=ep'
        assert change(run('run')) == 'unchanged'
        project([{'linux_capability_on_program_file': {'path': PROGRAM, 'capabilities': ['cap_net_raw', 'cap_net_bind_service'], 'capabilities_not_listed': 'remove'}}])
        run('run')
        assert remote(f'getcap {PROGRAM}').split()[-1].replace('+', '=') == 'cap_net_bind_service,cap_net_raw=ep'
        mark('capabilities_not_listed keep preserves unlisted capabilities; remove discards them')
        # A package upgrade replaces the file and drops its capabilities; the next run sets them again.
        remote(f'cp /usr/bin/true {PROGRAM}.new && mv {PROGRAM}.new {PROGRAM}')
        assert remote(f'getcap {PROGRAM}').strip() == ''
        assert change(run('run')) == 'changed' and 'cap_net_raw' in remote(f'getcap {PROGRAM}')
        project([{'linux_capability_on_program_file': {'path': PROGRAM, 'state': 'absent'}}])
        run('run')
        assert remote(f'getcap {PROGRAM}').strip() == ''
        assert change(run('run')) == 'unchanged'
        project([{'linux_capability_on_program_file': {'capabilities_not_listed': 'remove', 'path': PROGRAM + '-missing', 'capabilities': ['cap_net_raw']}},
                 {'linux_capability_on_program_file': {'path': PROGRAM + '-missing', 'state': 'absent'}}])
        assert [r['status'] for r in results(run('dryrun'))] == ['not_checked', 'succeeded']
        run('run', 1)
        mark('linux_capability_on_program_file sets the whole set, permitted and effective; reruns unchanged; a replaced '
             'program file gets its capabilities again; absent removes them; a missing file is not checked in a preview and fails a run')
    finally:
        cleanup()
    print(f'{len(passed)} access control list and capability workflows passed', flush=True)


if __name__ == '__main__':
    vm.run_suite(main)
