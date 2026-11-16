#!/usr/bin/env python3
"""ssh_key_pair on one node, its public key given to authorized_keys on the
other node through a fleet stage output; node state checked with stat,
ssh-keygen, and the files themselves."""
import json
import shlex
import shutil
import time

import vm

BACKUP = 'mmx-key-backup'      # owns the key pair on backup01
RECEIVER = 'mmx-key-receiver'  # accepts the key on database01
PRIVATE = f'/home/{BACKUP}/.ssh/id_ed25519'
LEGACY = f'/home/{BACKUP}/.ssh/id_rsa'


def main():
    nodes, connections = {}, {}
    for node_id, name in [('backup01', 'mmx-dev'), ('database01', 'mmx-peer')]:
        vm.up(name)
        nodes[node_id], connections[node_id] = vm.connection(name)
        vm.reset(name)
    workspace = vm.ROOT / 'ssh-key-pairs' / time.strftime('%Y%m%d-%H%M%S')
    workspace.mkdir(parents=True)
    vm.write_inventory(workspace, {'nodes': nodes, 'groups': {'database': {'nodes': ['database01']}}}, name='example')
    vm.build()
    mmx = vm.ROOT / 'bin/mmx'
    passed = []
    # Everything mmx printed, to search for the private key at the end.
    printed = []

    def remote(node, script):
        return vm.cmd([*connections[node], 'sudo -n /bin/sh -c ' + shlex.quote('set -eu\n' + script)]).stdout

    def mmx_command(*args, expected):
        p = vm.cmd([mmx, *args], cwd=workspace, check=False)
        printed.append(p.stdout + p.stderr)
        assert p.returncode in expected, (args, p.returncode, p.stdout, p.stderr)
        return p

    def run(command, *expected):
        p = mmx_command(command, *vm.playbook_args(command, 'backup_access'), '--json', expected=expected or (0,))
        return json.loads(p.stdout)

    def project(key_type='ed25519'):
        vm.write_project(workspace, project_name='ssh_key_pairs', playbook_name='backup_access',
                         node_task_defaults={'retry': {'pause_between_tries': '0s'}}, fleet_stages=[
            {'id': 'create_backup_key', 'nodes': 'backup01', 'node_tasks': [
                {'id': 'backup_key', 'ssh_key_pair': {'for_user_account': BACKUP, 'private_key_path': PRIVATE, 'key_type': key_type}},
                {'id': 'legacy_key', 'ssh_key_pair': {'for_user_account': BACKUP, 'private_key_path': LEGACY, 'key_type': 'rsa',
                                                      'key_comment_when_created': 'legacy backup key'}}],
             'outputs': {'backup_public_key': '{{ .node_tasks.backup_key.public_key }}'}},
            {'id': 'accept_backup_key', 'nodes': 'database', 'requires': {'all_nodes': ['create_backup_key']}, 'node_tasks': [
                {'authorized_keys': {'for_user_account': RECEIVER, 'keys': ['{{ .outputs.create_backup_key["backup01"].backup_public_key }}'],
                                     'keys_not_listed': 'keep'}}]}])

    def entry(record, fleet_stage):
        return next(e['response'] for e in record['entries'] if e['fleet_stage'] == fleet_stage)

    def key_result(record, task='backup_key'):
        return next(r for r in entry(record, 'create_backup_key')['results'] if r['task'] == task)

    def mark(message):
        passed.append(message)
        print('PASS ' + message, flush=True)

    def metadata(path):
        return remote('backup01', f'stat -c "%a %U %G" {path}').strip()

    def identity(line):
        """A key's type and base64 text, without its comment."""
        return ' '.join(line.split()[:2])

    def private_sha():
        return remote('backup01', f'sha256sum {PRIVATE} {LEGACY}')

    def accepted():
        return remote('database01', f'cat /home/{RECEIVER}/.ssh/authorized_keys 2>/dev/null || true')

    def cleanup():
        remote('backup01', f'userdel -r {BACKUP} 2>/dev/null || true; rm -rf /home/{BACKUP}')
        remote('database01', f'userdel -r {RECEIVER} 2>/dev/null || true; rm -rf /home/{RECEIVER}')

    cleanup()
    try:
        remote('backup01', f'useradd -m -s /bin/sh {BACKUP}')
        project()
        preview = run('dryrun', 3)
        made = key_result(preview)
        assert made['change'] == 'would_change' and 'public_key' not in made, made
        assert entry(preview, 'create_backup_key')['status'] == 'not_checked', preview
        assert entry(preview, 'accept_backup_key')['status'] == 'not_checked', preview
        assert remote('backup01', f'test -e /home/{BACKUP}/.ssh && echo exists || echo absent').strip() == 'absent'
        mark('dryrun reports that the key pairs would be created; the output and the other node\'s task that read public_key are not checked')

        # The receiving account does not exist yet: the key is made, the
        # second fleet stage fails, and resume gives the saved output to it.
        failed = run('run', 1)
        public = key_result(failed)['public_key']
        assert entry(failed, 'create_backup_key')['status'] == 'succeeded' and entry(failed, 'accept_backup_key')['status'] == 'failed', failed
        assert public.startswith('ssh-ed25519 ') and public.endswith(f' {BACKUP}@backup01'), public
        assert entry(failed, 'create_backup_key')['outputs']['backup_public_key'] == public
        legacy = key_result(failed, 'legacy_key')['public_key']
        assert legacy.startswith('ssh-rsa ') and legacy.endswith(' legacy backup key'), legacy
        assert metadata(f'/home/{BACKUP}/.ssh') == f'700 {BACKUP} {BACKUP}'
        for path in (PRIVATE, LEGACY):
            assert metadata(path) == f'600 {BACKUP} {BACKUP}' and metadata(path + '.pub') == f'644 {BACKUP} {BACKUP}', path
        assert remote('backup01', f'cat {PRIVATE}.pub').strip() == public
        assert remote('backup01', f'ssh-keygen -l -f {PRIVATE}').strip().endswith('(ED25519)')
        assert remote('backup01', f'ssh-keygen -l -f {LEGACY}').strip().endswith('(RSA)')
        # The node's own ssh-keygen derives the same public key from the private key.
        assert identity(remote('backup01', f'ssh-keygen -y -f {PRIVATE}')) == identity(public)
        sums = private_sha()
        remote('database01', f'useradd -m -s /bin/sh {RECEIVER}')
        resumed = run('resume')
        assert resumed['id'] == failed['id'] and resumed['status'] == 'succeeded', resumed
        assert public in accepted().splitlines(), accepted()
        assert private_sha() == sums
        mark('ssh_key_pair makes ed25519 and rsa pairs owned by the account (.ssh 700, private key 600, public key 644) and returns '
             'public_key; after a failure on the other node, resume gives that key to authorized_keys through the fleet stage output')

        again = run('run')
        assert key_result(again)['change'] == 'unchanged' and key_result(again)['public_key'] == public, again
        assert key_result(again, 'legacy_key')['public_key'] == legacy
        assert all(e['response']['change'] == 'unchanged' for e in again['entries']), again
        assert entry(run('dryrun', 0), 'create_backup_key')['outputs']['backup_public_key'] == public
        human = mmx_command('run', 'backup_access', expected=(0,)).stdout
        assert f'public_key: {public}' in human, human
        assert accepted().count(identity(public)) == 1 and private_sha() == sums
        mark('an unchanged rerun and dryrun return the same public_key and change nothing on either node')

        remote('backup01', f'rm {PRIVATE}.pub')
        preview = run('dryrun', 2)
        assert key_result(preview)['change'] == 'would_change' and identity(key_result(preview)['public_key']) == identity(public), preview
        assert remote('backup01', f'test -e {PRIVATE}.pub && echo exists || echo absent').strip() == 'absent'
        rewritten = run('run')
        assert key_result(rewritten)['change'] == 'changed', rewritten
        assert identity(remote('backup01', f'cat {PRIVATE}.pub')) == identity(public) and metadata(f'{PRIVATE}.pub') == f'644 {BACKUP} {BACKUP}'
        assert private_sha() == sums and identity(public) in accepted()
        assert key_result(run('run'))['change'] == 'unchanged'
        mark('a missing .pub file is written again from the private key, which stays as it was')

        project(key_type='rsa')
        for command in ('dryrun', 'run'):
            refused = key_result(run(command, 1))
            assert refused['status'] == 'failed' and f'the private key {PRIVATE} is a ssh-ed25519 key, not rsa as key_type says' in refused['reason'], refused
        assert private_sha() == sums
        mark('an existing private key of another type than key_type fails dryrun and run and is never replaced')

        # The private keys never left backup01: not in mmx's output, the saved
        # runs on the controller machine, the node's job records, or its journal.
        for path in (PRIVATE, LEGACY):
            text = remote('backup01', f'cat {path}')
            # The first base64 line is the same header in every OpenSSH key of
            # one type; the full lines after it hold this key's own bytes.
            lines = [line for line in text.splitlines()[2:] if len(line) >= 64 and not line.startswith('-----')]
            assert len(lines) >= 2, text
            saved = ''.join(p.read_text(errors='replace') for p in (workspace / '.mmx').rglob('*') if p.is_file())
            for line in lines:
                assert line not in ''.join(printed) and line not in saved, 'a private key line left the node'
            # The node searches with the key's own lines, read from the key
            # file there: a line on a command line would reach the journal
            # through sudo's own log.
            found = remote('backup01', f'sed -e 1,2d -e "/^-----/d" {path} | awk "length(\\$0) >= 64" > /root/mmx-key-lines\n'
                                       'grep -rlF -f /root/mmx-key-lines /var/lib/mmx /run/mmx 2>/dev/null || true\n'
                                       'test -s /root/mmx-key-lines\n'
                                       'journalctl --no-pager -q 2>/dev/null | grep -F -f /root/mmx-key-lines || true\n'
                                       'rm -f /root/mmx-key-lines')
            assert found.strip() == '', found
        mark('the private keys appear in no mmx output, saved run, node job record, or journal')
    finally:
        cleanup()
    print(f'{len(passed)} SSH key pair workflows passed', flush=True)


if __name__ == '__main__':
    vm.run_suite(main)
