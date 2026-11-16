#!/usr/bin/env python3
"""Public CLI checks for recursive ownership and permissions on real Linux trees."""
import json
import shlex
import shutil

import vm


def main():
    vm.up()
    node, ssh = vm.connection()
    vm.reset()
    workspace = vm.ROOT / 'directory-resources'
    if workspace.exists():
        shutil.rmtree(workspace)
    workspace.mkdir()
    vm.write_inventory(workspace, {'nodes': {'app': node}}, name='example')
    vm.build()
    mmx = vm.ROOT / 'bin/mmx'
    base = '/var/tmp/mmx-directory-resources'
    tree = base + '/tree'
    passed = []

    def remote(script):
        return vm.cmd([*ssh, 'sudo -n /bin/sh -c ' + shlex.quote('set -eu\n' + script)]).stdout

    def project(tasks):
        vm.write_project(workspace, playbook_name='configure', **{
            'project_name': 'directory_resources', 'default_inventory': 'example',
            'fleet_stages': [{'id': 'configure', 'nodes': 'all', 'node_tasks': tasks}]})

    def directory(path=tree, **fields):
        return {'id': 'permissions', 'directory': {'path': path, **fields}}

    def run(command, expected=None):
        result = vm.cmd([mmx, command, *vm.playbook_args(command, "configure"), '--json'], cwd=workspace, check=False)
        assert result.returncode in (vm.success_codes(command) if expected is None else (expected,)), (command, result.stdout, result.stderr)
        return json.loads(result.stdout)

    def result(record):
        return record['entries'][0]['response']['results'][0]

    def snapshot(path=tree, fields='%n %i %y %a %u %g'):
        return remote(f"find {path} -xdev -exec stat -c {shlex.quote(fields)} {{}} + | sort")

    def mark(name):
        passed.append(name)
        print('PASS ' + name, flush=True)

    def cleanup():
        remote(f'for mount in {base}/boundaries/mount {base}/deletions/tree/data/mounted {base}/deletions/tree/bound '
               f'{base}/deletions/mounted-root; do if mountpoint -q "$mount"; then umount "$mount"; fi; done\n'
               f'if command -v chattr >/dev/null && [ -d {base}/deletions ]; then chattr -R -i {base}/deletions 2>/dev/null || true; fi\n'
               f'rm -rf {base}')

    def value(record):
        return result(record).get('value')

    cleanup()
    try:
        remote(f'mkdir -p {tree}/nested\nprintf "first bytes\\n" > {tree}/first\n'
               f'printf "second bytes\\n" > {tree}/nested/second\n'
               f'chmod 0755 {tree} {tree}/nested\nchmod 0644 {tree}/first {tree}/nested/second')
        before = snapshot()
        identity = snapshot(fields='%n %i %y')
        contents = remote(f'sha256sum {tree}/first {tree}/nested/second')
        # The group users exists on every tested distribution; Debian's nogroup does not.
        task = directory(also_apply_to_everything_inside=True, owner='nobody', group='users', permissions_mode='0750', file_permissions_mode='0640')
        project([task])
        preview = run('dryrun')
        summary = result(preview)['directory_change']
        assert (summary['directories'], summary['files'], summary['changes'], summary['applied']) == (2, 2, 4, 0), summary
        assert snapshot() == before
        summary = result(run('run'))['directory_change']
        assert summary['applied'] == 4, summary
        remote(f'test "$(find {tree} -type d ! -perm 0750 | wc -l)" -eq 0\n'
               f'test "$(find {tree} -type f ! -perm 0640 | wc -l)" -eq 0\n'
               f'test "$(find {tree} ! -user nobody -o ! -group users | wc -l)" -eq 0')
        assert snapshot(fields='%n %i %y') == identity
        assert remote(f'sha256sum {tree}/first {tree}/nested/second') == contents
        unchanged = snapshot(fields='%n %i %y %z %a %u %g')
        assert result(run('run'))['change'] == 'unchanged'
        assert snapshot(fields='%n %i %y %z %a %u %g') == unchanged
        mark('dryrun, recursive ownership and distinct modes, preserved bytes/inodes/mtimes, and mutation-free rerun')

        remote(f'chmod 1750 {tree}\nchmod 2750 {tree}/nested\nchmod 6751 {tree}/first\nchmod 0600 {tree}/nested/second')
        modes = snapshot(fields='%n %a')
        groups = snapshot(fields='%n %g')
        project([directory(also_apply_to_everything_inside=True, owner='root')])
        run('run')
        assert snapshot(fields='%n %a') == modes
        assert snapshot(fields='%n %g') == groups
        remote(f'test "$(find {tree} ! -user root | wc -l)" -eq 0')
        project([directory(also_apply_to_everything_inside=True, group='root')])
        run('run')
        assert snapshot(fields='%n %a') == modes
        remote(f'test "$(find {tree} ! -group root | wc -l)" -eq 0')
        # Ordinary file tasks use the same helper; chown must preserve these bits too.
        project([{'file': {'path': tree + '/first', 'owner': 'nobody'}}])
        run('run')
        assert snapshot(fields='%n %a') == modes
        mark('ownership-only changes preserve each entry mode, including executable and special bits')

        ownership = snapshot(fields='%n %u %g')
        project([directory(also_apply_to_everything_inside=True, permissions_mode='0750', file_permissions_mode='0640')])
        run('run')
        assert snapshot(fields='%n %u %g') == ownership
        remote(f'test "$(stat -c %a {tree}/first)" = 640\ntest "$(stat -c %a {tree}/nested)" = 750')
        mark('explicit modes change permissions while preserving mixed ownership')

        boundary = base + '/boundaries'
        remote(f'mkdir -p {boundary}/mount {boundary}/nested {base}/outside\n'
               f'touch {base}/outside/file {boundary}/nested/file\n'
               f'ln -s {base}/outside {boundary}/external\nln -s nested {boundary}/internal\n'
               f'ln -s missing {boundary}/dangling\nln -s {base}/outside/file {boundary}/file-link\n'
               f'mkfifo -m 0644 {boundary}/fifo\n'
               f'mount -t tmpfs -o size=64k,mode=0755 tmpfs {boundary}/mount\n'
               f'touch {boundary}/mount/file')
        outside = snapshot(base + '/outside')
        mounted = snapshot(boundary + '/mount')
        links = remote(f'stat -c "%n %i %a %u %g" {boundary}/external {boundary}/internal {boundary}/dangling {boundary}/file-link {boundary}/fifo')
        project([directory(boundary, also_apply_to_everything_inside=True, owner='nobody', group='users', permissions_mode='0700', file_permissions_mode='0600')])
        for command in ['dryrun', 'run']:
            summary = result(run(command))['directory_change']
            assert (summary['skipped_symlinks'], summary['skipped_special'], summary['skipped_mounts']) == (4, 1, 1), summary
            assert snapshot(base + '/outside') == outside
            assert snapshot(boundary + '/mount') == mounted
            assert remote(f'stat -c "%n %i %a %u %g" {boundary}/external {boundary}/internal {boundary}/dangling {boundary}/file-link {boundary}/fifo') == links
        mark('symlinks, special files, and a different-device mounted tree remain untouched')

        batch = base + '/batch'
        remote(f'mkdir {batch}\ni=0\nwhile [ "$i" -lt 300 ]; do touch {batch}/file-$i; i=$((i+1)); done')
        project([directory(batch, also_apply_to_everything_inside=True, file_permissions_mode='0600')])
        preview = run('dryrun')
        summary = result(preview)['directory_change']
        assert summary['files'] == 300 and summary['changes'] == 300 and len(summary['examples']) == 10, summary
        human = vm.cmd([mmx, 'results', preview['id'], '--diff'], cwd=workspace).stdout
        assert '300 entries would change' in human and '290 further' in human, human
        assert result(run('run'))['directory_change']['applied'] == 300
        assert result(run('run'))['change'] == 'unchanged'
        mark('multiple directory-read batches retain one result and ten examples, including saved human previews')

        missing = base + '/new-root'
        project([directory(missing, also_apply_to_everything_inside=True, file_permissions_mode='0600')])
        assert result(run('dryrun'))['directory_change']['changes'] == 1
        remote(f'test ! -e {missing}')
        run('run')
        remote(f'test "$(stat -c %a {missing})" = 755\ntouch {missing}/child\nchmod 0644 {missing}/child')
        project([directory(missing, permissions_mode='0700')])
        run('run')
        remote(f'test "$(stat -c %a {missing}/child)" = 644\nln -s {missing} {base}/root-link')
        project([directory(base + '/root-link/', also_apply_to_everything_inside=True, permissions_mode='0755')])
        assert 'not a directory' in result(run('run', 1))['reason']
        remote(f'test "$(stat -c %a {missing})" = 700')
        mark('missing root creation, default nonrecursive behavior, and refusal of a symlink root')

        # The group users and the set-group-ID bit of the existing ancestor
        # would pass to new directories; the created parents are root's, 0755.
        shared = base + '/shared'
        parents = [shared + '/a', shared + '/a/b']
        remote(f'mkdir {shared}\nchgrp users {shared}\nchmod 2775 {shared}')
        project([directory(parents[1] + '/data', create_missing_parent_directories=True, owner='nobody', permissions_mode='0750')])
        preview = result(run('dryrun'))
        assert preview['change'] == 'would_change' and preview['directory_change']['created_parent_directories'] == parents, preview
        remote(f'test ! -e {parents[0]}')
        created = result(run('run'))
        assert created['change'] == 'changed' and created['directory_change']['created_parent_directories'] == parents, created
        assert remote(f'stat -c "%a %U %G" {parents[0]} {parents[1]} {parents[1]}/data') == '755 root root\n' * 2 + '750 nobody root\n'
        again = result(run('run'))
        assert again['change'] == 'unchanged' and 'created_parent_directories' not in again['directory_change'], again
        mark('create_missing_parent_directories creates the missing parents owned by root with 0755, also under a '
             'set-group-ID directory, lists them in the dryrun and the run, and reruns unchanged')

        # A numeric owner or group needs no account on the node, as for a
        # directory that a container's process owns.
        numeric = base + '/numeric'
        remote(f'! getent passwd 4242 && ! getent group 4343\nmkdir -p {numeric}/inside\ntouch {numeric}/inside/file')
        project([directory(numeric, also_apply_to_everything_inside=True, owner=4242, group=4343),
                 {'file': {'path': numeric + '-config', 'content': 'x\n', 'owner': 4242, 'group': 'root', 'permissions_mode': '0640'}}])
        assert remote(f'stat -c "%u %g" {numeric}/inside/file').strip() != '4242 4343'
        run('dryrun')
        assert remote(f'stat -c "%u %g" {numeric}/inside/file').strip() != '4242 4343'
        run('run')
        assert remote(f'stat -c "%u %g" {numeric} {numeric}/inside {numeric}/inside/file {numeric}-config') == '4242 4343\n' * 3 + '4242 0\n'
        assert run('run')['entries'][0]['response']['change'] == 'unchanged'
        project([directory(numeric, owner='4242')])
        refused = vm.cmd([mmx, 'validate', 'configure'], cwd=workspace, check=False)
        assert refused.returncode != 0 and 'write it without quotes' in refused.stderr, (refused.stdout, refused.stderr)
        mark('a numeric owner and group apply with no account on the node and rerun unchanged; digits in quotes are refused')

        # Deletions. Each is checked on the node with find and stat, not
        # only through MMX's result.
        deletions = base + '/deletions'
        outside = base + '/outside-deletions'
        remote(f'mkdir -p {deletions}/empty {deletions}/full/data {outside}\n'
               f'touch {deletions}/full/data/file {outside}/keep')
        project([directory(deletions + '/empty', state='absent')])
        preview = run('dryrun')
        assert value(preview) == {'deleted_entries': 1} and result(preview)['change'] == 'would_change', result(preview)
        remote(f'test -d {deletions}/empty')
        assert value(run('run')) == {'deleted_entries': 1}
        remote(f'test ! -e {deletions}/empty')
        assert result(run('run'))['change'] == 'unchanged'
        project([directory(deletions + '/full', state='absent')])
        refused = result(run('run', 1))
        assert 'is not empty (2 entries inside); write also_delete_everything_inside: true' in refused['reason'], refused
        remote(f'test -f {deletions}/full/data/file')
        project([directory('/var', state='absent', also_delete_everything_inside=True)])
        refused = vm.cmd([mmx, 'validate', 'configure'], cwd=workspace, check=False)
        assert refused.returncode != 0 and 'at least two levels deep' in refused.stderr, (refused.stdout, refused.stderr)
        remote('test -d /var/tmp')
        mark('state: absent deletes an empty directory, refuses one with entries, and refuses a path one level deep')

        tree_path = deletions + '/tree'
        remote(f'mkdir -p {tree_path}/bin {tree_path}/data/deep/deeper\n'
               f'printf app > {tree_path}/bin/app\ntouch {tree_path}/data/deep/deeper/file {tree_path}/.hidden\n'
               f'ln -s {outside} {tree_path}/data/outside-link\nln -s {outside}/keep {tree_path}/keep-link\n'
               f'mkfifo {tree_path}/fifo')
        project([directory(tree_path, state='absent', also_delete_everything_inside=True)])
        before = snapshot(deletions)
        outside_before = snapshot(outside)
        preview = run('dryrun')
        # tree, bin, bin/app, data, deep, deeper, file, .hidden, two links, the fifo
        assert value(preview) == {'deleted_entries': 11}, value(preview)
        assert snapshot(deletions) == before
        # A mount anywhere inside, another filesystem or a bind mount of
        # this one, fails the task before anything is deleted.
        for mount, kind in [(tree_path + '/data/mounted', '-t tmpfs -o size=64k tmpfs'), (tree_path + '/bound', '--bind ' + outside)]:
            remote(f'mkdir {mount}\nmount {kind} {mount}')
            mounted_before = snapshot(deletions)
            for command in ('dryrun', 'run'):
                refused = result(run(command, 1))
                assert 'is a mount point' in refused['reason'] and mount in refused['reason'], refused
                assert snapshot(deletions) == mounted_before
            remote(f'umount {mount}\nrmdir {mount}')
        assert snapshot(outside) == outside_before
        applied = run('run')
        assert value(applied) == {'deleted_entries': 11} and result(applied)['change'] == 'changed', result(applied)
        remote(f'test ! -e {tree_path}\ntest -z "$(ls -A {deletions} | grep -v "^full$")"')
        assert snapshot(outside) == outside_before
        for command in ('dryrun', 'run'):
            assert result(run(command))['change'] == 'unchanged'
        mark('also_delete_everything_inside: dryrun counts, a mounted filesystem or bind mount inside fails before any deletion, '
             'links are deleted as links, and the rerun is unchanged')

        # A deletion that stops part of the way leaves only the staging
        # directory beside the path; the next run finishes it. An immutable
        # file makes MMX's own deletion stop after the rename.
        remote(f'mkdir -p {tree_path}/logs\ntouch {tree_path}/logs/a {tree_path}/logs/b')
        staging = deletions + '/.mmx-staging-tree'
        if remote('command -v chattr || true').strip():
            remote(f'chattr +i {tree_path}/logs/a')
            stopped = result(run('run', 1))
            assert stopped['status'] == 'failed' and stopped['change'] == 'changed' and 'operation not permitted' in stopped['reason'], stopped
            remote(f'test ! -e {tree_path}\ntest -f {staging}/logs/a\nchattr -i {staging}/logs/a')
        else:
            remote(f'mv {tree_path} {staging}')
        assert result(run('dryrun'))['change'] == 'would_change'
        remote(f'test -d {staging}')
        finished = run('run')
        assert result(finished)['change'] == 'changed', result(finished)
        remote(f'test ! -e {staging}\ntest ! -e {tree_path}')
        assert result(run('run'))['change'] == 'unchanged'
        mark('an interrupted tree deletion leaves only its staging directory, which the next run deletes')

        releases = deletions + '/releases'
        remote(f'mkdir -p {releases}/1.0/bin {releases}/1.1 {releases}/2.0 {releases}/.mmx-staging-0.9/old\n'
               f'touch {releases}/1.0/bin/app {releases}/notes {releases}/2.0/app\nln -s 2.0 {releases}/current')
        project([directory(releases, delete_everything_inside_except=['2.0', '1.1', '3.0'], owner='root', group='root', permissions_mode='0700')])
        kept = snapshot(releases + '/2.0')
        preview = run('dryrun')
        assert value(preview) == {'deleted_entries': 7, 'deleted_names': ['.mmx-staging-0.9', '1.0', 'current', 'notes']}, value(preview)
        remote(f'test -f {releases}/1.0/bin/app\ntest "$(stat -c %a {releases})" = 755')
        applied = run('run')
        assert value(applied) == value(preview) and result(applied)['change'] == 'changed'
        assert remote(f'ls -A {releases}') == '1.1\n2.0\n'
        assert snapshot(releases + '/2.0') == kept
        assert remote(f"stat -c '%U:%G %a' {releases}").strip() == 'root:root 700'
        assert result(run('run'))['change'] == 'unchanged'
        # The directory itself may be a mount point: what it deletes lives
        # on that one filesystem.
        mounted_root = deletions + '/mounted-root'
        remote(f'mkdir {mounted_root}\nmount -t tmpfs -o size=64k tmpfs {mounted_root}\nmkdir {mounted_root}/keep {mounted_root}/old')
        project([directory(mounted_root, delete_everything_inside_except=['keep'])])
        assert value(run('run'))['deleted_names'] == ['old']
        assert remote(f'ls -A {mounted_root}') == 'keep\n'
        remote(f'umount {mounted_root}\nrmdir {mounted_root}')
        project([directory(releases, delete_everything_inside_except=[])])
        assert value(run('run'))['deleted_names'] == ['1.1', '2.0']
        remote(f'test -d {releases}\ntest -z "$(ls -A {releases})"')
        mark('delete_everything_inside_except deletes every unlisted entry with its tree, also a leftover staging directory, '
             'keeps the listed ones untouched, works in a mount point, and [] empties the directory')

        # With state: present the directory exists: a missing one that
        # delete_everything_inside_except names is created with the task's
        # owner, group, and mode, and a later file task writes into it.
        created = deletions + '/created'
        project([{'id': 'shop_releases', 'directory': {'path': created, 'owner': 'nobody', 'group': 'users', 'permissions_mode': '0750',
                                                       'delete_everything_inside_except': ['1.0']}},
                 {'id': 'release_notes', 'file': {'path': created + '/1.0', 'content': 'first release\n', 'permissions_mode': '0644'}}])
        ids = remote('id -u nobody\ngetent group users | cut -d: -f3').split()

        def by_id(record):
            return {r['task'].rsplit('/', 1)[-1]: r for r in record['entries'][0]['response']['results']}

        preview = by_id(run('dryrun'))
        assert preview['shop_releases']['change'] == 'would_change' and 'value' not in preview['shop_releases'], preview['shop_releases']
        assert preview['shop_releases']['directory_change']['examples'] == [
            {'path': created, 'action': 'create', 'before': None,
             'after': {'kind': 'directory', 'permissions_mode': '0750', 'uid': int(ids[0]), 'gid': int(ids[1])}}], preview['shop_releases']
        assert preview['release_notes']['change'] == 'would_change', preview['release_notes']
        remote(f'test ! -e {created}')
        applied = by_id(run('run'))
        assert applied['shop_releases']['status'] == 'succeeded' and applied['shop_releases']['change'] == 'changed', applied['shop_releases']
        assert applied['release_notes']['change'] == 'changed', applied['release_notes']
        assert remote(f'stat -c "%F %U %G %a" {created}') == 'directory nobody users 750\n'
        assert remote(f'ls -A {created}\ncat {created}/1.0') == '1.0\nfirst release\n'
        for command in ('run', 'dryrun'):
            assert {r['change'] for r in by_id(run(command)).values()} == {'unchanged'}, command
        mark('delete_everything_inside_except on a missing directory creates it with its owner, group, and mode, '
             'a later file task writes into it, the dryrun on the missing path predicts both, and the rerun is unchanged')

        task = directory(batch, also_apply_to_everything_inside=True, file_permissions_mode='0640')
        project([task, {'shell': {'script': 'test ! -e ' + base + '/fail'}}])
        remote(f'touch {base}/fail')
        run('run', 1)
        remote(f'rm {base}/fail\nchmod 0666 {batch}/file-0')
        task['directory']['file_permissions_mode'] = '0600'
        project([task])
        resumed = result(run('resume'))
        assert resumed['directory_change']['applied'] == 1, resumed
        remote(f'test "$(stat -c %a {batch}/file-0)" = 640')
        mark('recorded resume keeps recursive settings and reconciles current metadata without an entry ledger')
    finally:
        cleanup()
    print(f'{len(passed)} directory-resource workflows passed', flush=True)


if __name__ == '__main__':
    vm.run_suite(main)
