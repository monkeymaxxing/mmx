#!/usr/bin/env python3
"""Large ordinary files through both transports, frozen resume, and asset lifecycle."""
import hashlib
import json
from pathlib import Path
import shlex
import tempfile
from datetime import datetime, timedelta, timezone

import vm


def main():
    vm.up()
    node, ssh = vm.connection()
    vm.reset()
    vm.build()
    mmx = vm.ROOT / 'bin/mmx'
    base = '/var/tmp/mmx-streamed-assets'
    passed = []

    def remote(script):
        return vm.cmd([*ssh, 'sudo -n /bin/sh -c ' + shlex.quote(script)]).stdout

    def mark(name):
        passed.append(name)
        print('PASS ' + name, flush=True)

    try:
        for transport in ['ssh', 'https']:
            with tempfile.TemporaryDirectory(prefix='streamed-assets-', dir=vm.ROOT) as tmp:
                workspace = Path(tmp)
                target = dict(node)
                target['connection'] = 'ssh_only' if transport == 'ssh' else 'https_with_ssh_fallback'
                vm.write_inventory(workspace, {'nodes': {'app': target}}, name='example')
                remote(f'rm -rf {base}; mkdir {base}')
                samples = {'binary18': 18, 'binary64': 64, 'small1': 6, 'small2': 6, 'small3': 6}
                # The role's files live in the role: a role's path never leaves it.
                role = workspace / 'roles/copy'
                role.mkdir(parents=True)
                digests = {}
                for index, (name, size) in enumerate(samples.items()):
                    chunk = bytes([index + 128]) * (1024 * 1024)
                    digest = hashlib.sha256()
                    with (role / name).open('wb') as file:
                        for _ in range(size):
                            file.write(chunk)
                            digest.update(chunk)
                    digests[name] = digest.hexdigest()
                (role / 'config.tmpl').write_text('value={{ .inputs.setting }}\n')
                tasks = [{'id': name, 'file': {'path': base + '/' + name, 'source': name, 'permissions_mode': '0644'}}
                         for name in samples]
                tasks += [{'id': 'loop', 'loop': ['one', 'two'], 'as': 'item',
                           'file': {"permissions_mode": "0644", 'path': base + '/{{ .item }}', 'source': 'small1'}},
                          {'id': 'template', 'file': {"permissions_mode": "0644", 'path': base + '/config', 'template': 'config.tmpl'}}]
                # A role and task block exercise asset discovery through nested task lists.
                (role / 'main.yml').write_text(json.dumps({'inputs': {'setting': {'type': 'string', 'required': True}}, 'node_tasks': tasks}))
                data = {'project_name': 'streamed_assets', 'default_inventory': 'example', 'vars': {'setting': 'frozen'},
                        'fleet_stages': [{'id': 'copy', 'nodes': 'all', 'node_tasks': [
                            {'role': 'copy', 'inputs': {'setting': '{{ .vars.setting }}'}}, {"task_block": {"id": "copy_assets", "changes": [{"file": {"permissions_mode": "0644", "path": base + "/task-block", "source": "roles/copy/small2"}}]}}]}]}

                def project():
                    vm.write_project(workspace, playbook_name='configure', **data)

                def run(command, expected=None, *args):
                    result = vm.cmd([mmx, command, *vm.playbook_args(command, "configure"), '--json', *args], cwd=workspace, check=False)
                    assert result.returncode in (vm.success_codes(command) if expected is None else (expected,)), (command, result.stdout, result.stderr)
                    record = json.loads(result.stdout)
                    for entry in record.get('entries', []):
                        assert entry['connection_used'] == transport, entry
                    return record

                def verify():
                    for name, digest in digests.items():
                        assert remote(f'sha256sum {base}/{name}').split()[0] == digest
                    assert remote(f'cat {base}/config') == 'value=frozen\n'

                def inspect_saved(record):
                    folder = workspace / '.mmx/runs' / record['id']
                    definitions = folder / 'run.json'
                    assert definitions.stat().st_size < 32768
                    assert len(list((folder / 'assets').iterdir())) == len(samples) + 1
                    assert sum(p.stat().st_size for p in (folder / 'assets').iterdir()) < 101 * 1024 * 1024
                    for path in folder.rglob('*.json'):
                        assert path.stat().st_size < 128 * 1024
                        assert 'source_data' not in path.read_text() and 'template_text' not in path.read_text()
                    # The node records a job's result before it deletes the
                    # job's uploads, so a run can return a moment before that.
                    for entry in record['entries']:
                        remote('for i in $(seq 60); do test -e /var/lib/mmx/uploads/' + entry['job_id']
                               + ' || exit 0; sleep 0.5; done; exit 1')
                    return folder

                if transport == 'https':
                    # A dryrun uses SSH until a run has set up MMX's HTTPS
                    # service, so a first run of another playbook sets it up and
                    # every command below goes over HTTPS.
                    vm.write_playbook(workspace, 'set_up_https', [{'id': 'set_up_https', 'nodes': 'all', 'node_tasks': [
                        {'file': {'permissions_mode': '0644', 'path': base + '/https', 'content': 'set up'}}]}])
                    vm.write_settings(workspace, {'project_name': 'streamed_assets', 'default_inventory': 'example'})
                    set_up = vm.cmd([mmx, 'run', 'set_up_https', '--json'], cwd=workspace)
                    assert vm.https_setup_notice('app', 'example') in set_up.stderr, set_up.stderr
                    assert json.loads(set_up.stdout)['entries'][0]['connection_used'] == 'https', set_up.stdout
                project()
                preview = run('dryrun')
                assert 'file exceeds 64 KiB' in json.dumps(preview)
                remote(f'test ! -e {base}/binary64')
                inspect_saved(preview)
                applied = run('run')
                verify()
                inspect_saved(applied)
                inode = remote(f'stat -c %i {base}/binary64')
                unchanged = run('run')
                assert unchanged['entries'][0]['response']['change'] == 'unchanged'
                assert remote(f'stat -c %i {base}/binary64') == inode
                inspect_saved(unchanged)
                mark(f'{transport}: 18 MiB, 64 MiB, and three 6 MiB files; dryrun, byte hashes, no-op/inode, compact JSON, nested deduplication, node cleanup')

                # A later failure leaves a resumable run. Delete the local sources
                # and destination files; resume must use only the saved disk assets.
                data['fleet_stages'][0]['node_tasks'].append({'shell': {'script': f'test ! -e {base}/fail'}})
                project()
                remote(f'touch {base}/fail')
                failed = run('run', 1)
                for name in samples:
                    (role / name).unlink()
                (role / 'config.tmpl').write_text('changed={{ .vars.setting }}\n')
                (role / 'config.tmpl').unlink()
                remote(f'rm {base}/fail {base}/binary* {base}/small* {base}/config')
                resumed = run('resume', 0, failed['id'])
                verify()
                inspect_saved(resumed)
                assert len(resumed['entries'][0]['previous_node_job_attempts']) == 1
                mark(f'{transport}: recorded resume restores large sources and rendered template after originals are deleted')

                # Ordinary history retention removes assets with their owning run.
                old = (datetime.now(timezone.utc) - timedelta(days=2)).isoformat().replace('+00:00', 'Z')
                for record in [preview, applied, unchanged]:
                    path = workspace / '.mmx/runs' / record['id'] / 'status.json'
                    status = json.loads(path.read_text())
                    status['finished'] = old
                    path.write_text(json.dumps(status))
                run('results', 0, failed['id'])
                for record in [preview, applied, unchanged]:
                    assert not (workspace / '.mmx/runs' / record['id']).exists()
                assert (workspace / '.mmx/runs' / failed['id'] / 'assets').exists()
                mark(f'{transport}: expiry deletes old run assets and retains the latest normal run')
    finally:
        remote(f'rm -rf {base}')
    report = vm.ROOT / 'streamed-assets-result.json'
    report.write_text(json.dumps({'passed': passed}, indent=2) + '\n')
    print(f'{len(passed)} streamed asset workflows passed; {report}', flush=True)


if __name__ == '__main__':
    vm.run_suite(main)
