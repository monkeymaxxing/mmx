#!/usr/bin/env python3
"""Public CLI checks that entries MMX creates carry the SELinux label of their path."""
import io
import json
import shlex
import shutil
import tarfile

import vm

KEY = 'ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIORZZg3NlYdxJ28janK/DsJ1O0O2YgF7KN2aiEPskegs mmx-selinux-test'
ACCOUNT = 'mmx-selinux'
SITE = '/var/www/mmx-selinux'
RELEASE = '/var/www/mmx-selinux-release'
CRON_DIRECTORY = '/etc/cron.d'
CRON = CRON_DIRECTORY + '/mmx-selinux'
HOME = '/home/' + ACCOUNT


def main():
    vm.up()
    node, ssh = vm.connection()
    vm.reset()

    def remote(script):
        return vm.cmd([*ssh, 'sudo -n /bin/sh -c ' + shlex.quote('set -eu\n' + script)]).stdout

    if remote(f'if {vm.SELINUX_RUNS}; then echo on; fi').strip() != 'on':
        print('PASS SELinux is not enabled on this node; nothing to check', flush=True)
        return
    workspace = vm.ROOT / 'selinux-labels'
    if workspace.exists():
        shutil.rmtree(workspace)
    (workspace / 'files').mkdir(parents=True)
    vm.write_inventory(workspace, {'nodes': {'app': node}}, name='example')
    vm.build()
    mmx = vm.ROOT / 'bin/mmx'
    with tarfile.open(workspace / 'files/site.tar.gz', 'w:gz') as archive:
        for name, body in [('site/index.html', b'release\n'), ('site/assets/app.css', b'body {}\n')]:
            entry = tarfile.TarInfo(name)
            entry.size, entry.mode = len(body), 0o644
            archive.addfile(entry, io.BytesIO(body))
        link = tarfile.TarInfo('site/latest')
        link.type, link.linkname = tarfile.SYMTYPE, 'index.html'
        archive.addfile(link)

    def cleanup():
        # /var/www and /etc/cron.d belong to this test only when no package made them.
        remote(f'rm -rf {SITE} {RELEASE} {CRON} {HOME}\n'
               f'if id {ACCOUNT} >/dev/null 2>&1; then userdel {ACCOUNT}; fi\n'
               f'for made in /var/www {CRON_DIRECTORY}; do\n'
               ' if test -d $made && ! rpm -qf $made >/dev/null 2>&1; then rmdir $made 2>/dev/null || true; fi\ndone')

    def run(command):
        result = vm.cmd([mmx, command, *vm.playbook_args(command, 'configure'), '--json'], cwd=workspace, check=False)
        assert result.returncode in vm.success_codes(command), (command, result.stdout, result.stderr)
        return json.loads(result.stdout)

    cleanup()
    try:
        tasks = [
            {'directory': {'path': '/var/www', 'permissions_mode': '0755'}},
            {'directory': {'path': SITE, 'permissions_mode': '0755'}},
            {'file': {'path': SITE + '/index.html', 'content': 'hello\n', 'permissions_mode': '0644'}},
            {'symlink': {'path': SITE + '/current', 'target': 'index.html'}},
            {'unpack_archive': {'source': 'files/site.tar.gz', 'path': RELEASE, 'unpack_without_wrapping_folder': True}},
            {'directory': {'path': CRON_DIRECTORY, 'permissions_mode': '0755'}},
            {'file': {'path': CRON, 'content': '# managed by the MMX SELinux label test\n', 'permissions_mode': '0644'}},
            {'user': {'create_home_directory': True, 'name': ACCOUNT, 'home': HOME, 'shell': '/bin/bash'}},
            {'directory': {'path': HOME, 'owner': ACCOUNT, 'group': ACCOUNT, 'permissions_mode': '0700'}},
            {'authorized_keys': {'for_user_account': ACCOUNT, 'keys': [KEY], 'keys_not_listed': 'remove'}},
        ]

        def write_project():
            vm.write_project(workspace, playbook_name='configure', **{
                'project_name': 'selinux_labels', 'default_inventory': 'example',
                'fleet_stages': [{'id': 'configure', 'nodes': 'all', 'node_tasks': tasks}]})

        write_project()
        before = remote(f'ls -d {SITE} {RELEASE} {CRON} {HOME} 2>/dev/null || true')
        assert before == '', before
        run('dryrun')
        assert remote(f'ls -d {SITE} {RELEASE} {CRON} {HOME} 2>/dev/null || true') == ''
        run('run')
        paths = ['/var/www', SITE, SITE + '/index.html', SITE + '/current', RELEASE, RELEASE + '/index.html',
                 RELEASE + '/assets', RELEASE + '/assets/app.css', RELEASE + '/latest', CRON_DIRECTORY, CRON,
                 HOME, HOME + '/.ssh', HOME + '/.ssh/authorized_keys']
        # The policy's own answer for each path, compared with the label on disk.
        report = remote('for path in ' + ' '.join(paths) + '; do\n'
                        ' printf "%s %s %s\\n" "$path" "$(stat -c %C "$path")" "$(matchpathcon -n "$path")"\ndone')
        wrong = [line for line in report.splitlines() if line.split()[1] != line.split()[2]]
        assert not wrong, 'labels differ from the policy:\n' + '\n'.join(wrong)
        labels = {line.split()[0]: line.split()[1].split(':')[2] for line in report.splitlines()}
        # These types differ from their parent directory's, so inheriting would be wrong.
        assert labels['/var/www'] == 'httpd_sys_content_t' and labels[CRON] == 'system_cron_spool_t', labels
        assert labels[HOME + '/.ssh/authorized_keys'] == 'ssh_home_t' and labels[RELEASE + '/latest'] == 'httpd_sys_content_t', labels
        assert remote('restorecon -n -v -R /var/www ' + CRON_DIRECTORY + ' ' + HOME) == ''
        print('PASS files, directories, links, an unpacked tree, and SSH keys carry the label the policy gives their path', flush=True)
        assert run('run')['entries'][0]['response']['change'] == 'unchanged'
        print('PASS an unchanged rerun changes nothing', flush=True)

        # The copy of an original, a file from node paths, a kept file, and a
        # file edit_file changes are new files too.
        tasks[2] = {'file': {'path': SITE + '/index.html', 'content': 'second\n', 'permissions_mode': '0644',
                             'backup_original_file_to': SITE + '/index.html.original'}}
        tasks[4:4] = [
            {'file': {'path': SITE + '/joined.html', 'content_from_node_paths': [SITE + '/index.html'], 'permissions_mode': '0644'}},
            {'file': {'path': SITE + '/edited.html', 'content': 'draft\n', 'permissions_mode': '0644', 'if_file_exists': 'keep_existing_content'}},
            {'edit_file': {'path': SITE + '/edited.html', 'content_filter': {'shell': {'script': 'sed s/draft/final/'}}}},
        ]
        write_project()
        run('run')
        assert remote(f'cat {SITE}/index.html.original {SITE}/joined.html {SITE}/edited.html') == 'hello\nsecond\nfinal\n'
        paths = [SITE + '/index.html', SITE + '/index.html.original', SITE + '/joined.html', SITE + '/edited.html']
        report = remote('for path in ' + ' '.join(paths) + '; do\n'
                        ' printf "%s %s %s\\n" "$path" "$(stat -c %C "$path")" "$(matchpathcon -n "$path")"\ndone')
        wrong = [line for line in report.splitlines() if line.split()[1] != line.split()[2]]
        assert not wrong, 'labels differ from the policy:\n' + '\n'.join(wrong)
        assert all(line.split()[1].split(':')[2] == 'httpd_sys_content_t' for line in report.splitlines()), report
        assert remote('restorecon -n -v -R ' + SITE) == ''
        print('PASS a kept original, a file from node paths, and a file edit_file changed carry the label of their path', flush=True)
        assert run('run')['entries'][0]['response']['change'] == 'unchanged'
        print('PASS an unchanged rerun changes nothing', flush=True)
    finally:
        cleanup()


if __name__ == '__main__':
    vm.run_suite(main)
