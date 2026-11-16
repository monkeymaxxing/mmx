#!/usr/bin/env python3
"""Directory source copies through public CLI, saved assets, and real services."""
import hashlib
import json
import shlex
import shutil

import vm


def main():
    vm.up()
    node, ssh = vm.connection()
    vm.reset()
    vm.build()
    workspace = vm.ROOT / 'directory-copy'
    if workspace.exists():
        shutil.rmtree(workspace)
    workspace.mkdir()
    mmx = vm.ROOT / 'bin/mmx'
    base = '/var/tmp/mmx-directory-copy'
    passed = []

    def remote(script):
        return vm.cmd([*ssh, 'sudo -n /bin/sh -c ' + shlex.quote('set -eu\n' + script)]).stdout

    def project(tasks):
        vm.write_project(workspace, playbook_name='configure', **{
            'project_name': 'directory_copy', 'default_inventory': 'example',
            'node_task_defaults': {'retry': {'pause_between_tries': '0s'}},
            'fleet_stages': [{'id': 'configure', 'nodes': 'all', 'node_tasks': tasks}]})

    def run(command, expected=None, *args):
        proc = vm.cmd([mmx, command, *vm.playbook_args(command, "configure"), '--json', *args], cwd=workspace, check=False)
        assert proc.returncode in (vm.success_codes(command) if expected is None else (expected,)), (command, proc.stdout, proc.stderr)
        return json.loads(proc.stdout)

    def results(record):
        return record['entries'][0]['response']['results']

    def mark(name):
        passed.append(name)
        print('PASS ' + name, flush=True)

    def cleanup():
        remote('systemctl disable --now mmx-config-demo.service 2>/dev/null || true\n'
               'systemctl reset-failed mmx-config-demo.service 2>/dev/null || true\n'
               'rm -f /etc/systemd/system/mmx-config-demo.service\nsystemctl daemon-reload\n'
               f'rm -rf {base} /etc/mmx-config-demo /srv/mmx-config-demo /usr/local/bin/mmx-config-demo-load')

    cleanup()
    remote(f'mkdir {base}')
    try:
        role = workspace / 'roles/bundle'
        source = role / 'files'
        (source / 'nested').mkdir(parents=True)
        (source / 'empty').mkdir()
        literal = '{{ .secrets.not_an_input }}'
        samples = {'.hidden': b'hidden\n', 'empty-file': b'', 'nested/message': b'configuration\n',
                   'duplicate': b'configuration\n', literal: b'{{ .vars.literal }}\n',
                   'run': b'#!/bin/sh\nprintf "works\\n"\n'}
        for name, data in samples.items():
            (source / name).write_bytes(data)
            (source / name).chmod(0o755 if name == 'run' else 0o644)
        large = source / 'binary'
        with large.open('wb') as output:
            output.write(bytes(range(256)))
            output.truncate(18 << 20)
        with large.open('rb') as content:
            large_hash = hashlib.file_digest(content, 'sha256').hexdigest()
        (role / 'main.yml').write_text(json.dumps({'inputs': {'destination': {'type': 'string', 'required': True}},
            'node_tasks': [{'id': 'tree', 'directory': {'path': '{{ .inputs.destination }}', 'source': 'files/'}}]}))
        for connection in ('ssh_only', 'https_with_ssh_fallback'):
            vm.write_inventory(workspace, {'nodes': {'app': dict(node, connection=connection)}}, name='example')
            destination = base + '/' + connection
            project([{'role': 'bundle', 'inputs': {'destination': destination + '/'}}])
            assert run('validate')['valid']
            preview = run('dryrun')
            assert results(preview)[0]['directory_change']['changes'] == 10
            remote(f'test ! -e {destination}')
            applied = run('run')
            assert applied['entries'][0]['connection_used'] == ('ssh' if connection == 'ssh_only' else 'https')
            assert results(applied)[0]['directory_change']['applied'] == 10
            for name, data in samples.items():
                assert remote('sha256sum ' + shlex.quote(destination + '/' + name)).split()[0] == hashlib.sha256(data).hexdigest()
            assert remote(f'sha256sum {destination}/binary').split()[0] == large_hash
            assert remote(f'{destination}/run') == 'works\n'
            remote(f'test -d {destination}/empty\nprintf keep > {destination}/unlisted\nchmod 0600 {destination}/unlisted')
            before = remote(f'find {destination} -exec stat -c "%n %i %y %z %a %u %g" {{}} + | sort')
            assert results(run('run'))[0]['change'] == 'unchanged'
            assert remote(f'find {destination} -exec stat -c "%n %i %y %z %a %u %g" {{}} + | sort') == before
            bundle = json.loads(remote(f"cat /var/lib/mmx/jobs/{applied['entries'][0]['job_id']}/request.json"))
            entries = bundle['node_tasks'][0]['role_node_tasks'][0]['module_inputs']['entries']
            refs = [e['asset']['sha256'] for e in entries if 'asset' in e]
            assert len(refs) == 7 and len(set(refs)) == 6
            assert len(json.dumps(bundle)) < 16384
            remote('test ! -e /var/lib/mmx/uploads/' + applied['entries'][0]['job_id'])
            assert results(run('results', 0, applied['id']))[0]['directory_change']['applied'] == 10
            mark(connection + ' role source, empty directories, dotfiles, literal names, executable and 18 MiB binary, deduplicated assets, unchanged rerun')

        # The controller machine can choose SSH without changing the shared inventory.
        inventory_node = dict(node)
        inventory_node.pop('connection', None)
        vm.write_inventory(workspace, {'nodes': {'app': inventory_node}}, name='example')
        applied = run('run', 0, '--connection', 'ssh_only')
        saved = run('results', 0, applied['id'])
        assert saved['entries'] and all(e['connection_used'] == 'ssh' for e in saved['entries']), saved
        mark('--connection ssh_only uses SSH with the default inventory connection and saves connection_used in results')

        # Reduce retained assets for later scenarios; old saved runs still contain them.
        large.unlink()
        destination = base + '/https_with_ssh_fallback'
        direct = {'id': 'tree', 'directory': {'path': destination, 'source': 'roles/bundle/files',
                  'owner': 'nobody', 'group': 'users', 'permissions_mode': '0750', 'file_permissions_mode': '0640'}}
        project([direct])
        before = remote(f'stat -c "%i %y" {destination}/nested/message')
        run('dryrun')
        run('run')
        assert remote(f'stat -c "%i %y" {destination}/nested/message') == before
        remote(f'test "$(stat -c %a {destination}/run)" = 640\n'
               f'test "$(stat -c %U:%G {destination}/nested/message)" = nobody:users\n'
               f'test "$(stat -c %a:%U {destination}/unlisted)" = 600:root\n'
               f'test -f {destination}/binary')
        (source / '.hidden').unlink()
        assert results(run('run'))[0]['change'] == 'unchanged'
        remote(f'test -f {destination}/.hidden')
        mark('explicit permissions and ownership preserve file identity and unlisted or removed-source destination entries')

        # Exact copies remove stale source entries and entries nobody copied.
        exact_source = workspace / 'exact-source'
        (exact_source / 'kept').mkdir(parents=True)
        (exact_source / 'kept/file').write_text('kept bytes\n')
        for delete_extra in (False, True):
            (exact_source / 'removed-file').write_text('old bytes\n')
            (exact_source / 'removed-directory').mkdir()
            (exact_source / 'removed-directory/file').write_text('old nested bytes\n')
            target = base + ('/exact-copy' if delete_extra else '/copy-with-extras')
            task = {'id': 'exact', 'directory': {'path': target, 'source': 'exact-source',
                    'also_delete_entries_not_in_source': delete_extra}}
            project([task])
            assert results(run('run'))[0]['change'] == 'changed'
            remote(f'printf extra > {target}/node-only-file')
            kept = remote(f'stat -c "%i %y %a" {target}/kept/file')
            (exact_source / 'removed-file').unlink()
            shutil.rmtree(exact_source / 'removed-directory')
            before = remote(f'find {target} -exec stat -c "%n %i %y %a" {{}} + | sort')
            preview = run('dryrun')
            expected_change = 'would_change' if delete_extra else 'unchanged'
            assert results(preview)[0]['change'] == expected_change, results(preview)
            if delete_extra:
                result = results(preview)[0]
                assert result['value'] == {'deleted_entries': 4}, result
                summary = result['directory_change']
                assert summary['changes'] == 4 and summary['applied'] == 0, summary
                assert {item['path'] for item in summary['examples'] if item['action'] == 'delete'} == {
                    target + '/node-only-file', target + '/removed-file',
                    target + '/removed-directory', target + '/removed-directory/file'}, summary
            assert remote(f'find {target} -exec stat -c "%n %i %y %a" {{}} + | sort') == before
            if delete_extra:
                # Both a filesystem and a bind mount must fail global preflight.
                for kind in ('-t tmpfs -o size=64k tmpfs', '--bind ' + target + '/kept'):
                    mount = target + '/removed-directory/mounted'
                    remote(f'mkdir {mount}\nmount {kind} {mount}')
                    try:
                        mounted_before = remote(f'find {target} -xdev -exec stat -c "%n %i %y %a" {{}} + | sort')
                        for command in ('dryrun', 'run'):
                            refused = results(run(command, 1))[0]
                            assert 'is a mount point' in refused['reason'] and mount in refused['reason'], refused
                            assert remote(f'find {target} -xdev -exec stat -c "%n %i %y %a" {{}} + | sort') == mounted_before
                    finally:
                        remote(f'umount {mount}\nrmdir {mount}')
                # A source-owned directory must not allow deletion across its mount.
                remote(f'touch {target}/kept/node-only\nmount --bind {target}/kept {target}/kept')
                try:
                    for command in ('dryrun', 'run'):
                        refused = results(run(command, 1))[0]
                        assert 'is a mount point' in refused['reason'] and target + '/kept' in refused['reason'], refused
                        remote(f'test -f {target}/node-only-file\ntest -f {target}/kept/node-only')
                finally:
                    remote(f'umount {target}/kept\nrm {target}/kept/node-only')
            applied = results(run('run'))[0]
            assert applied['change'] == ('changed' if delete_extra else 'unchanged'), applied
            if delete_extra:
                assert applied['value'] == {'deleted_entries': 4}, applied
                assert applied['directory_change']['applied'] == 4, applied
                remote(f'test ! -e {target}/node-only-file\ntest ! -e {target}/removed-file\n'
                       f'test ! -e {target}/removed-directory')
            else:
                remote(f'test -f {target}/node-only-file\ntest -f {target}/removed-file\n'
                       f'test -f {target}/removed-directory/file')
            assert remote(f'stat -c "%i %y %a" {target}/kept/file') == kept
            assert results(run('run'))[0]['change'] == 'unchanged'
        mark('source copies optionally delete removed files, whole directories, and node-only entries; '
             'dryrun changes nothing, mounts fail before deletion, and the third run is unchanged')

        # Resume must still deliver the old tree after the whole local source disappears.
        project([direct, {'shell': {'script': 'test ! -e ' + base + '/fail'}}])
        remote(f'touch {base}/fail')
        failed = run('run', 1)
        shutil.move(source, role / 'saved-source')
        remote(f'rm {base}/fail {destination}/nested/message')
        resumed = run('resume', 0, failed['id'])
        assert results(resumed)[0]['directory_change']['applied'] == 1
        assert remote(f'cat {destination}/nested/message') == 'configuration\n'
        shutil.move(role / 'saved-source', source)
        (source / 'nested/message').write_text('current source\n')
        project([direct])
        run('run')
        assert remote(f'cat {destination}/nested/message') == 'current source\n'
        mark('recorded resume restores frozen paths/bytes after source deletion; fresh runs use current sources')

        # Type conflicts are discovered before installing another source entry.
        remote(f'mkdir {base}/conflict\nln -s {base}/https_with_ssh_fallback/nested {base}/conflict/nested')
        direct['directory']['path'] = base + '/conflict'
        project([direct])
        bad = run('run', 1)
        assert 'conflicting type' in json.dumps(bad)
        remote(f'test ! -e {base}/conflict/run\ntest ! -e {base}/conflict/empty')
        (source / 'bad-link').symlink_to('nested/message')
        assert not vm.cmd([mmx, 'validate', 'configure', '--json'], cwd=workspace, check=False).returncode == 0
        (source / 'bad-link').unlink()
        mark('source links and conflicting destination links fail without installing another entry or changing link targets')

        # Run the shipped example, inspecting loaded bytes and actual service actions.
        example = workspace / 'example'
        shutil.copytree(vm.REPO / 'examples/directory-copy', example)
        (example / 'inventories/example.yml').write_text((workspace / 'inventories/example.yml').read_text())
        example_file = example / 'playbooks/copy_config.yml'
        original = example_file.read_text()
        config_file = example / 'conf.d/10-main.conf'
        initial = config_file.read_text()
        extra = (example / 'conf.d/20-options.conf').read_text()

        def example_run(command, expected=None, *args):
            proc = vm.cmd([mmx, command, *vm.playbook_args(command, "copy_config"), '--json', *args], cwd=example, check=False)
            assert proc.returncode in (vm.success_codes(command) if expected is None else (expected,)), (command, proc.stdout, proc.stderr)
            return json.loads(proc.stdout)

        def loaded():
            return remote('cat /srv/mmx-config-demo/loaded')

        def actions():
            return int(remote('wc -l < /srv/mmx-config-demo/actions'))

        assert example_run('validate')['valid']
        example_run('dryrun')
        remote('test ! -e /etc/mmx-config-demo')
        example_run('run')
        assert loaded() == initial + extra
        count = actions()
        example_run('run')
        assert actions() == count
        config_file.write_text('message=B\n')
        example_run('dryrun')
        assert loaded() == initial + extra and actions() == count
        remote(f'touch {base}/fail')
        example_file.write_text(original.replace('script: |\n',
            'script: |\n                  test ! -e ' + base + '/fail || exit 1\n', 1))
        failed = example_run('run', 1)
        assert remote('cat /etc/mmx-config-demo/10-main.conf') == 'message=B\n'
        assert loaded() == initial + extra and actions() == count
        config_file.write_text('message=C\n')
        remote(f'rm {base}/fail')
        recovered = example_run('resume', 0, failed['id'])
        assert next(r for r in results(recovered) if r['task'].endswith('/tree'))['change'] == 'unchanged'
        assert loaded() == 'message=B\n' + extra and actions() == count + 1
        example_run('run')
        assert loaded() == 'message=C\n' + extra
        count = actions()
        example_file.write_text(original.replace('file_permissions_mode: "0644"', 'file_permissions_mode: "0640"'))
        example_run('run')
        example_run('run')
        assert actions() == count
        mark('shipped example loads changed tree once after failed validation; saved resume and metadata-only runs preserve reload semantics')

        # A project without the source directory copies nothing when the task says so.
        empty = base + '/project-modules'
        missing = {'id': 'modules', 'directory': {'path': empty, 'source': 'project_modules', 'permissions_mode': '0750'}}
        project([missing])
        refused = vm.cmd([mmx, 'validate', 'configure', '--json'], cwd=workspace, check=False)
        assert refused.returncode != 0 and 'if_source_directory_is_missing: copy_nothing' in refused.stdout + refused.stderr
        missing['directory']['if_source_directory_is_missing'] = 'copy_nothing'
        project([missing])
        assert run('validate')['valid']
        run('dryrun')
        remote(f'test ! -e {empty}')
        assert results(run('run'))[0]['change'] == 'changed'
        assert remote(f'stat -c %a {empty}\nls -A {empty}') == '750\n'
        remote(f'printf kept > {empty}/earlier.sh')
        assert results(run('run'))[0]['change'] == 'unchanged'
        assert remote(f'cat {empty}/earlier.sh') == 'kept'
        (workspace / 'project_modules').mkdir()
        (workspace / 'project_modules/check.sh').write_text('#!/bin/sh\n')
        run('run')
        assert remote(f'ls -A {empty} | sort') == 'check.sh\nearlier.sh\n'
        mark('a missing source directory refuses the playbook, or with copy_nothing leaves the target existing and empty, then copies once the source exists')

        # Aggregate previews stay bounded, while private contents stay out of records.
        many = workspace / 'many'
        many.mkdir()
        for i in range(40):
            (many / f'file-{i:02d}').write_text('private-tree-sentinel\n')
            (many / f'file-{i:02d}').chmod(0o600)
        project([{'id': 'many', 'directory': {'path': base + '/many', 'source': 'many'}}])
        preview = run('dryrun')
        change = results(preview)[0]['directory_change']
        assert change['changes'] == 41 and len(change['examples']) == 10
        assert 'private-tree-sentinel' not in json.dumps(preview)
        human = vm.cmd([mmx, 'results', preview['id'], '--diff'], cwd=workspace).stdout
        assert '41 entries would change' in human and '31 further' in human
        assert 'private-tree-sentinel' not in human
        run('run')
        assert results(run('run'))[0]['change'] == 'unchanged'
        for record in (workspace / '.mmx').rglob('*.json'):
            assert 'private-tree-sentinel' not in record.read_text()
        mark('one bounded summary, saved human/JSON previews, and private-file content suppression')
    finally:
        cleanup()
        shutil.rmtree(workspace / '.mmx', ignore_errors=True)
        shutil.rmtree(workspace / 'example/.mmx', ignore_errors=True)
    (workspace / 'summary.json').write_text(json.dumps(passed, indent=2) + '\n')
    print(f'{len(passed)} directory-copy workflows passed', flush=True)


if __name__ == '__main__':
    vm.run_suite(main)
