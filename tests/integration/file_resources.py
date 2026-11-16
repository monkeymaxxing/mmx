#!/usr/bin/env python3
"""Public CLI coverage for exact copies, file removal, links, previews, and activation."""
import hashlib
import json
import shlex
import shutil

import vm


def main():
    vm.up()
    node, ssh = vm.connection()
    vm.reset()
    workspace = vm.ROOT / 'file-resources'
    if workspace.exists():
        shutil.rmtree(workspace)
    workspace.mkdir()
    vm.write_inventory(workspace, {'nodes': {'app': node}}, name='example')
    vm.build()
    mmx = vm.ROOT / 'bin/mmx'
    base = '/var/tmp/mmx-file-resources'
    passed = []

    def remote(script):
        return vm.cmd([*ssh, 'sudo -n /bin/sh -c ' + shlex.quote(script)]).stdout

    def project(tasks, **extra):
        data = {'project_name': 'file_resources', 'default_inventory': 'example',
                'fleet_stages': [{'id': 'configure', 'nodes': 'all', 'node_tasks': tasks}], **extra}
        vm.write_project(workspace, playbook_name='configure', **data)

    def run(command, expected=None, *args):
        result = vm.cmd([mmx, command, *vm.playbook_args(command, "configure"), '--json', *args], cwd=workspace, check=False)
        assert result.returncode in (vm.success_codes(command) if expected is None else (expected,)), (command, result.stdout, result.stderr)
        return json.loads(result.stdout)

    def results(record):
        return record['entries'][0]['response']['results']

    def mark(name):
        passed.append(name)
        print('PASS ' + name, flush=True)

    def cleanup():
        for unit in ['mmx-files.service', 'mmx-lines.service']:
            remote(f'systemctl disable --now {unit} 2>/dev/null || true; '
                   f'systemctl reset-failed {unit} 2>/dev/null || true; '
                   f'rm -f /etc/systemd/system/{unit}')
        remote(f'systemctl daemon-reload; rm -rf {base}')

    cleanup()
    remote(f'mkdir {base}')
    try:
        role = workspace / 'roles/assets'
        (role / 'files').mkdir(parents=True)
        samples = {'literal': b'{{ .secrets.not_a_reference }}\n',
                   'empty': b'', 'binary': bytes(range(256))}
        tasks = []
        for name, data in samples.items():
            (role / 'files' / name).write_bytes(data)
            tasks.append({'id': name, 'file': {'path': base + '/' + name,
                          'source': 'files/' + name, 'permissions_mode': '0644'}})
        (role / 'main.yml').write_text(json.dumps({'node_tasks': tasks}))
        project([{'role': 'assets'}])
        preview = run('dryrun')
        remote(f'test ! -e {base}/literal; test ! -e {base}/binary; test ! -e {base}/empty')
        assert any(r.get('file_change', {}).get('diff_omitted') == 'binary contents omitted' for r in results(preview))
        run('run')
        for name, data in samples.items():
            assert remote(f'sha256sum {base}/{name}').split()[0] == hashlib.sha256(data).hexdigest()
        assert run('run')['entries'][0]['response']['change'] == 'unchanged'
        mark('role sources copy literal delimiters, empty files, and every byte value exactly; preview and rerun are unchanged on disk')

        for alteration in ['edit', 'remove']:
            source = workspace / 'snapshot'
            source.write_bytes(b'frozen bytes\xff\n')
            project([{'file': {"permissions_mode": "0644", 'path': base + '/frozen', 'source': 'snapshot'}},
                     {'shell': {'script': 'test ! -e ' + base + '/fail'}}])
            remote(f'touch {base}/fail')
            run('run', 1)
            if alteration == 'edit':
                source.write_bytes(b'changed locally')
            else:
                source.unlink()
            remote(f'rm {base}/fail {base}/frozen')
            run('resume')
            assert remote(f'sha256sum {base}/frozen').split()[0] == hashlib.sha256(b'frozen bytes\xff\n').hexdigest()
        mark('recorded resume copies frozen source bytes after the local source is edited or deleted')

        source = workspace / 'candidate'
        source.write_text('first\n')
        file = {'file': {'path': base + '/preview', 'source': 'candidate', 'permissions_mode': '0644'}}
        project([file])
        run('run')
        source.write_text('second\n')
        preview = run('dryrun')
        assert '-first\n+second\n' in results(preview)[0]['file_change']['diff']
        assert remote(f'cat {base}/preview') == 'first\n'
        human = vm.cmd([mmx, 'results', preview['id'], '--diff'], cwd=workspace).stdout
        assert '-first' in human and '+second' in human and base + '/preview' in human
        file['file']['check_new_file'] = {'shell': {'script': 'exit 1'}}
        project([file])
        rejected = run('run', 1)
        assert results(rejected)[0]['failure_kind'] == 'validation'
        assert remote(f'cat {base}/preview') == 'first\n'
        mark('source validation preserves live bytes and retained text previews redisplay through results --diff')

        source.write_text('secret-old\n')
        file['file'].pop('check_new_file')
        file['file']['permissions_mode'] = '0600'
        project([file])
        run('run')
        source.write_text('secret-new\n')
        record = run('dryrun')
        encoded = json.dumps(record)
        assert 'secret-old' not in encoded and 'secret-new' not in encoded
        assert 'private file' in results(record)[0]['file_change']['diff_omitted']
        file['file']['permissions_mode'] = '0644'
        source.write_text('public\n')
        project([file])
        run('run')
        project([{'file': {'path': base + '/preview', 'content': '{{ .secrets.token }}', 'permissions_mode': '0600'}}],
                secrets_from_env={'token': 'MMX_FILE_TEST_SECRET'})
        vm.ENV['MMX_FILE_TEST_SECRET'] = 'secret-preview-sentinel'
        record = run('dryrun')
        assert 'secret-preview-sentinel' not in json.dumps(record)
        assert 'secrets' in results(record)[0]['file_change']['diff_omitted']
        del vm.ENV['MMX_FILE_TEST_SECRET']
        mark('private files and secret-consuming tasks suppress text before it reaches saved previews')

        project([{'file': {'path': base + '/preview', 'state': 'absent'}}])
        preview = run('dryrun')
        assert results(preview)[0]['file_change']['action'] == 'remove'
        assert 'diff' not in results(preview)[0]['file_change']
        remote(f'test -f {base}/preview')
        run('run')
        remote(f'test ! -e {base}/preview')
        assert results(run('run'))[0]['change'] == 'unchanged'
        project([{'retry': False, 'file': {'path': base, 'state': 'absent'}}])
        run('run', 1)
        remote(f'test -d {base}')
        mark('regular-file removal previews without contents, removes once, and refuses directories')

        link = {'symlink': {'path': base + '/link', 'target': 'literal'}}
        project([link])
        run('dryrun')
        remote(f'test ! -L {base}/link')
        run('run')
        assert remote(f'readlink {base}/link').strip() == 'literal'
        assert results(run('run'))[0]['change'] == 'unchanged'
        for target in ['missing', base + '/binary']:
            link['symlink']['target'] = target
            project([link])
            run('run')
            assert remote(f'readlink {base}/link').strip() == target
        project([{'retry': False, 'file': {'path': base + '/link/', 'state': 'absent'}}])
        run('run', 1)
        remote(f'test -L {base}/link')
        project([{'symlink': {'path': base + '/link', 'state': 'absent'}}])
        run('dryrun')
        remote(f'test -L {base}/link')
        run('run')
        assert results(run('run'))[0]['change'] == 'unchanged'
        assert remote(f'sha256sum {base}/binary').split()[0] == hashlib.sha256(samples['binary']).hexdigest()
        mark('relative, absolute, dangling, and removed links leave their targets untouched')

        loader = workspace / 'load'
        loader.write_text(f'''#!/bin/sh
set -eu
{{ cat {base}/config; if test -f {base}/extra; then cat {base}/extra; fi; if test -L {base}/active; then cat {base}/active; fi; }} > {base}/loaded
printf 'loaded\\n' >> {base}/actions
''')
        (workspace / 'config').write_text('config-A\n')
        unit = f'''[Unit]
Description=MMX file workflow fixture
[Service]
ExecStartPre=/bin/sh {base}/load
ExecStart=/bin/sleep infinity
ExecReload=/bin/sh {base}/load
[Install]
WantedBy=multi-user.target
'''
        changes = [
            {'file': {'path': base + '/load', 'source': 'load', 'permissions_mode': '0755'}},
            {'file': {'path': base + '/config', 'source': 'config', 'permissions_mode': '0644'}},
            {'file': {"permissions_mode": "0644", 'path': base + '/extra', 'content': 'extra\n'}},
            {'file': {"permissions_mode": "0644", 'path': base + '/a', 'content': 'link-A\n'}},
            {'file': {"permissions_mode": "0644", 'path': base + '/b', 'content': 'link-B\n'}},
            {'symlink': {'path': base + '/active', 'target': 'a'}},
            {'systemd_unit': {'name': 'mmx-files.service', 'content': unit}},
        ]
        task_block = {
            "task_block": {
                "id": "files",
                "changes": changes,
                "activate": [{"systemd_service": {"name": "mmx-files", "start_at_boot": True, "refresh": "reload", "state": "running"}}],
            }
        }
        project([task_block])
        run('run')
        assert remote(f'cat {base}/loaded') == 'config-A\nextra\nlink-A\n'
        invocation = remote('systemctl show -p InvocationID --value mmx-files')
        count = len(remote(f'cat {base}/actions').splitlines())
        for kind in ['copy', 'remove', 'link', 'unlink']:
            if kind == 'copy':
                (workspace / 'config').write_text('config-B\n')
            elif kind == 'remove':
                changes[2] = {'file': {'path': base + '/extra', 'state': 'absent'}}
            elif kind == 'link':
                changes[5]['symlink']['target'] = 'b'
            else:
                changes[5] = {'symlink': {'path': base + '/active', 'state': 'absent'}}
            previous = remote(f'cat {base}/loaded')
            task_block['task_block']['validate'] = [{'shell': {'script': 'test ! -e ' + base + '/fail'}}]
            project([task_block])
            remote(f'touch {base}/fail')
            run('run', 1)
            assert remote(f'cat {base}/loaded') == previous
            assert len(remote(f'cat {base}/actions').splitlines()) == count
            remote(f'rm {base}/fail')
            preview = run('dryrun')
            assert any(r['change'] == 'would_change' and '/activate/' in r['task'] for r in results(preview))
            run('resume')
            count += 1
            expected = 'config-B\n' + ('extra\n' if kind == 'copy' else '') + ('' if kind == 'unlink' else ('link-B\n' if kind == 'link' else 'link-A\n'))
            assert remote(f'cat {base}/loaded') == expected
            assert len(remote(f'cat {base}/actions').splitlines()) == count
            assert remote('systemctl show -p InvocationID --value mmx-files') == invocation
            run('run')
            assert len(remote(f'cat {base}/actions').splitlines()) == count
            mark(kind + ' recovery reloads once after failure before activation, even with unchanged resource tasks')

        conf = base + '/app.conf'
        remote(f"printf '# app\\nport=80\\nmode=slow\\n' > {conf}; chown nobody:users {conf}; chmod 0644 {conf}")
        project([{'line': {"insert_at_end_of_file": True, 'path': conf, 'match': '^port=', 'line': 'port=8080'}}])
        preview = run('dryrun')
        assert '-port=80\n+port=8080\n' in results(preview)[0]['file_change']['diff']
        assert remote(f'cat {conf}') == '# app\nport=80\nmode=slow\n'
        run('run')
        assert remote(f'cat {conf}') == '# app\nport=8080\nmode=slow\n'
        assert remote(f"stat -c '%U:%G %a' {conf}").strip() == 'nobody:users 644'
        assert results(run('run'))[0]['change'] == 'unchanged'
        mark('dryrun previews a line edit as a diff; run replaces the matching line, keeping the other lines, owner, group, and mode; rerun is unchanged')

        project([{'line': {"insert_at_end_of_file": True, 'path': conf, 'match': '^workers=', 'line': 'workers=4'}},
                 {'line': {'path': conf, 'match': '^mode=', 'state': 'absent'}}])
        run('run')
        assert remote(f'cat {conf}') == '# app\nport=8080\nworkers=4\n'
        assert run('run')['entries'][0]['response']['change'] == 'unchanged'
        project([{'line': {"insert_at_end_of_file": True, 'path': conf, 'line': 'extra=1', 'check_new_file': {'shell': {'script': 'exit 1'}}}}])
        assert results(run('run', 1))[0]['failure_kind'] == 'validation'
        project([{'retry': False, 'line': {"insert_at_end_of_file": True, 'path': base + '/missing.conf', 'line': 'x=1'}}])
        run('run', 1)
        remote(f'test ! -e {base}/missing.conf')
        assert remote(f'cat {conf}') == '# app\nport=8080\nworkers=4\n'
        mark('line appends when nothing matches and removes with state: absent; a rejected candidate or missing file changes nothing')

        lines = base + '/lines.conf'
        remote(f"printf 'a=1\\nb=1\\n' > {lines}")
        remote(f"printf '#!/bin/sh\\ncat {lines} > {base}/lines-loaded\\necho loaded >> {base}/lines-actions\\n' > {base}/lines-load; chmod 0755 {base}/lines-load")

        def lines_task_block(a, b):
            return {
                "task_block": {
                    "id": "lines",
                    "changes": [
                        {
                            "systemd_unit": {
                                "name": "mmx-lines.service",
                                "content": f"[Service]\nExecStartPre=/bin/sh {base}/lines-load\nExecStart=/bin/sleep infinity\n",
                            }
                        },
                        {"line": {"insert_at_end_of_file": True, "path": lines, "match": "^a=", "line": "a=" + a}},
                        {"line": {"insert_at_end_of_file": True, "path": lines, "match": "^b=", "line": "b=" + b}},
                    ],
                    "validate": [{"shell": {"script": "test ! -e " + base + "/fail"}}],
                    "activate": [{"systemd_service": {"name": "mmx-lines", "state": "running"}}],
                }
            }

        def restarts():
            return len(remote(f'cat {base}/lines-actions').splitlines())

        project([lines_task_block('1', '1')])
        run('run')
        assert remote(f'cat {base}/lines-loaded') == 'a=1\nb=1\n' and restarts() == 1
        project([lines_task_block('2', '1')])
        preview = run('dryrun')
        assert any(r['change'] == 'would_change' and '/activate/' in r['task'] for r in results(preview))
        project([lines_task_block('2', '2')])
        run('run')
        assert remote(f'cat {base}/lines-loaded') == 'a=2\nb=2\n' and restarts() == 2
        assert run('run')['entries'][0]['response']['change'] == 'unchanged' and restarts() == 2
        project([lines_task_block('3', '2')])
        remote(f'touch {base}/fail')
        run('run', 1)
        assert remote(f'cat {lines}') == 'a=3\nb=2\n' and restarts() == 2
        remote(f'rm {base}/fail')
        run('resume')
        assert remote(f'cat {base}/lines-loaded') == 'a=3\nb=2\n' and restarts() == 3
        mark('two line edits of one file in a task block restart its service once, predicted by dryrun, and once after a failure before activation')

        hba = base + '/pg_hba.conf'
        remote(f"printf 'local all all peer\\nhost all all 0.0.0.0/0 reject\\n' > {hba}")
        rule = 'host app app 10.0.0.0/8 scram-sha-256'
        project([{'line': {'path': hba, 'line': rule, 'insert_before_first_line_matching': '^host\\s+all\\s+all\\s'}}])
        preview = run('dryrun')
        assert ' local all all peer\n+' + rule + '\n host all all' in results(preview)[0]['file_change']['diff']
        assert remote(f'cat {hba}') == 'local all all peer\nhost all all 0.0.0.0/0 reject\n'
        run('run')
        assert remote(f'cat {hba}') == f'local all all peer\n{rule}\nhost all all 0.0.0.0/0 reject\n'
        assert results(run('run'))[0]['change'] == 'unchanged'
        project([{'line': {'path': hba, 'line': '# first', 'insert_at_start_of_file': True}},
                 {'line': {'path': hba, 'line': 'local app app peer', 'insert_after_last_line_matching': '^local\\s'}}])
        run('run')
        assert remote(f'cat {hba}') == f'# first\nlocal all all peer\nlocal app app peer\n{rule}\nhost all all 0.0.0.0/0 reject\n'
        assert run('run')['entries'][0]['response']['change'] == 'unchanged'
        project([{'retry': False, 'line': {'path': hba, 'line': 'x=1', 'insert_after_first_line_matching': '^\\[missing\\]'}}])
        assert 'no line matches' in results(run('run', 1))[0]['reason']
        assert remote(f'cat {hba}') == f'# first\nlocal all all peer\nlocal app app peer\n{rule}\nhost all all 0.0.0.0/0 reject\n'
        mark('line places a new line before or after the first or last matching line or at the start; a placing pattern that matches nothing changes nothing')

        fstab = base + '/fstab'
        remote(f"printf 'UUID=1 / ext4 defaults 0 1\\n/swap.img none swap sw 0 0\\n' > {fstab}; chown nobody:users {fstab}; chmod 0644 {fstab}")
        swap_off = 'sed -E "s/^([^#].*[[:space:]]$KIND[[:space:]].*)$/#\\1/"'
        project([{'edit_file': {'path': fstab, 'content_filter': {'shell': {'script': swap_off, 'env': {'KIND': 'swap'}}}}}])
        preview = run('dryrun')
        assert '-/swap.img none swap sw 0 0\n+#/swap.img none swap sw 0 0\n' in results(preview)[0]['file_change']['diff']
        assert remote(f'cat {fstab}') == 'UUID=1 / ext4 defaults 0 1\n/swap.img none swap sw 0 0\n'
        run('run')
        assert remote(f'cat {fstab}') == 'UUID=1 / ext4 defaults 0 1\n#/swap.img none swap sw 0 0\n'
        assert remote(f"stat -c '%U:%G %a' {fstab}").strip() == 'nobody:users 644'
        assert results(run('run'))[0]['change'] == 'unchanged'
        assert results(run('dryrun'))[0]['change'] == 'unchanged'
        project([{'retry': False, 'edit_file': {'path': fstab, 'content_filter': {'shell': {'script': "sed 's/$/ x/'"}}}}])
        assert 'edit_file.content_filter would change the file on every run' in results(run('run', 1))[0]['reason']
        project([{'retry': False, 'edit_file': {'path': fstab, 'content_filter': {'shell': {'script': 'exit 4'}}}}])
        assert 'exit code 4' in results(run('run', 1))[0]['reason']
        project([{'retry': False, 'edit_file': {'path': base + '/missing.conf', 'content_filter': {'shell': {'script': 'cat'}}}}])
        assert results(run('dryrun'))[0]['status'] == 'not_checked'
        run('run', 1)
        assert remote(f'cat {fstab}') == 'UUID=1 / ext4 defaults 0 1\n#/swap.img none swap sw 0 0\n'
        mark('edit_file previews and installs a filtered file keeping owner, group, and mode; an unstable, failing, or missing-file edit changes nothing')

        kept = base + '/sentinel.conf'
        keep = {'file': {'path': kept, 'content': 'first\n', 'permissions_mode': '0640', 'if_file_exists': 'keep_existing_content'}}
        project([keep])
        assert results(run('dryrun'))[0]['file_change']['action'] == 'create'
        remote(f'test ! -e {kept}')
        run('run')
        assert remote(f'cat {kept}') == 'first\n'
        remote(f"printf 'rewritten by the program\\n' > {kept}")
        keep['file'].update(content='second\n', permissions_mode='0600')
        project([keep])
        change = results(run('dryrun'))[0]['file_change']
        assert 'diff' not in change and change['after']['permissions_mode'] == '0600', change
        run('run')
        assert remote(f'cat {kept}') == 'rewritten by the program\n'
        assert remote(f'stat -c %a {kept}').strip() == '600'
        assert results(run('run'))[0]['change'] == 'unchanged'
        mark('if_file_exists: keep_existing_content creates a missing file once, then applies only metadata')

        remote(f"printf 'CERTIFICATE-SENTINEL\\n' > {base}/fullchain.pem; printf 'PRIVATE-KEY-SENTINEL\\n' > {base}/privkey.pem; "
               f'chmod 0600 {base}/privkey.pem; ln -sfn privkey.pem {base}/key-link.pem')
        joined = hashlib.sha256(b'CERTIFICATE-SENTINEL\nPRIVATE-KEY-SENTINEL\n').hexdigest()
        pem = {'file': {'path': base + '/site.pem', 'permissions_mode': '0600',
                        'content_from_node_paths': [base + '/fullchain.pem', base + '/key-link.pem']}}
        project([pem])
        preview = run('dryrun')
        change = results(preview)[0]['file_change']
        assert change['after']['sha256'] == joined and 'diff' not in change, change
        assert 'SENTINEL' not in json.dumps(preview)
        remote(f'test ! -e {base}/site.pem')
        run('run')
        assert remote(f'sha256sum {base}/site.pem').split()[0] == joined
        assert results(run('run'))[0]['change'] == 'unchanged'
        remote(f"printf 'RENEWED-SENTINEL\\n' > {base}/fullchain.pem")
        change = results(run('dryrun'))[0]['file_change']
        assert change['before']['sha256'] == joined and change['after']['sha256'] != joined, change
        record = run('run')
        assert 'SENTINEL' not in json.dumps(record)
        assert remote(f'cat {base}/site.pem') == 'RENEWED-SENTINEL\nPRIVATE-KEY-SENTINEL\n'
        project([{'retry': False, 'file': {"permissions_mode": "0600", 'path': base + '/site.pem', 'content_from_node_paths': [base + '/missing.pem']}}])
        assert results(run('dryrun'))[0]['status'] == 'not_checked'
        run('run', 1)
        assert remote(f'cat {base}/site.pem') == 'RENEWED-SENTINEL\nPRIVATE-KEY-SENTINEL\n'
        mark('content_from_node_paths joins node files through a link, previews SHA-256 values without text, and reports a missing path as not checked')

        original = base + '/sshd.conf.original'
        remote(f"printf 'distribution\\n' > {base}/sshd.conf; chown nobody:users {base}/sshd.conf; chmod 0640 {base}/sshd.conf")
        managed = {'file': {"permissions_mode": "0644", 'path': base + '/sshd.conf', 'content': 'managed 1\n', 'backup_original_file_to': original}}
        project([managed])
        assert results(run('dryrun'))[0]['file_change']['original_file_copied_to'] == original
        remote(f'test ! -e {original}')
        run('run')
        assert remote(f'cat {original}') == 'distribution\n' and remote(f'cat {base}/sshd.conf') == 'managed 1\n'
        assert remote(f"stat -c '%U:%G %a' {original}").strip() == 'nobody:users 640'
        managed['file']['content'] = 'managed 2\n'
        project([managed])
        assert 'original_file_copied_to' not in results(run('run'))[0]['file_change']
        assert remote(f'cat {original}') == 'distribution\n' and remote(f'cat {base}/sshd.conf') == 'managed 2\n'
        assert results(run('run'))[0]['change'] == 'unchanged'
        mark('backup_original_file_to copies the original once with its owner, group, and mode, and never again')

        remote(f"printf 'left by a package\\n' > {base}/current; mkdir {base}/directory")
        link = {'symlink': {'path': base + '/current', 'target': 'binary'}}
        project([{'retry': False, **link}])
        run('run', 1)
        remote(f'test -f {base}/current && test ! -L {base}/current')
        link['symlink']['replace_existing_file'] = True
        project([link])
        assert results(run('dryrun'))[0]['file_change']['action'] == 'replace'
        remote(f'test ! -L {base}/current')
        run('run')
        assert remote(f'readlink {base}/current').strip() == 'binary'
        assert results(run('run'))[0]['change'] == 'unchanged'
        project([{'retry': False, 'symlink': {'path': base + '/directory', 'target': 'binary', 'replace_existing_file': True}}])
        run('run', 1)
        remote(f'test -d {base}/directory && test ! -L {base}/directory')
        mark('replace_existing_file replaces a regular file with the link, previewed in dryrun; a directory is never replaced')
    finally:
        vm.ENV.pop('MMX_FILE_TEST_SECRET', None)
        cleanup()
    print(f'{len(passed)} file-resource workflows passed', flush=True)


if __name__ == '__main__':
    vm.run_suite(main)
