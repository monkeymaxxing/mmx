#!/usr/bin/env python3
"""Release directories: archives and Git checkouts switched by a tracked symlink."""
import hashlib
import io
import json
import shlex
import shutil
import subprocess
import tarfile
import zipfile

import vm

BASE = '/srv/mmx-release'
SERVICE = 'mmx-release-demo.service'
FIXTURES = '/var/tmp/mmx-release-git'
RUN_SCRIPT = b'#!/bin/sh\ncat "$(dirname "$0")/VERSION" > /run/mmx-release-demo.loaded\nexec sleep infinity\n'


def tarball(path, version, members=None, mode='w:gz'):
    with tarfile.open(path, mode) as tar:
        def add(name, kind=tarfile.REGTYPE, mode=0o644, data=b'', link=''):
            info = tarfile.TarInfo(name)
            info.type, info.mode, info.linkname = kind, mode, link
            info.size = len(data)
            tar.addfile(info, io.BytesIO(data))
        top = f'app-{version}'
        for member in members or [
                (top, tarfile.DIRTYPE, 0o755), (top + '/run', tarfile.REGTYPE, 0o755, RUN_SCRIPT),
                (top + '/VERSION', tarfile.REGTYPE, 0o644, version.encode() + b'\n'),
                (top + '/lib', tarfile.DIRTYPE, 0o750), (top + '/lib/data', tarfile.REGTYPE, 0o640, b'data\n'),
                (top + '/start', tarfile.SYMTYPE, 0o777, b'', 'run')]:
            add(*member)


def main():
    vm.up()
    node, ssh = vm.connection()
    vm.reset()
    vm.build()
    workspace = vm.ROOT / 'release-directories'
    if workspace.exists():
        shutil.rmtree(workspace)
    (workspace / 'files').mkdir(parents=True)
    vm.write_inventory(workspace, {'nodes': {'app': node}}, name='example')
    mmx = vm.ROOT / 'bin/mmx'
    passed = []

    def remote(script):
        return vm.cmd([*ssh, 'sudo -n /bin/sh -c ' + shlex.quote('set -eu\n' + script)]).stdout.strip()

    def project(tasks, secrets_from_env=None):
        vm.write_project(workspace, playbook_name='configure', **{
            'project_name': 'release_directories', 'default_inventory': 'example', 'secrets_from_env': secrets_from_env or {},
            'node_task_defaults': {'retry': {'pause_between_tries': '0s'}},
            'fleet_stages': [{'id': 'deploy', 'nodes': 'all', 'node_tasks': tasks}]})

    def run(command, expected=None, *args):
        proc = vm.cmd([mmx, command, *vm.playbook_args(command, "configure"), '--json', *args], cwd=workspace, check=False)
        assert proc.returncode in (vm.success_codes(command) if expected is None else (expected,)), (command, proc.stdout, proc.stderr)
        return json.loads(proc.stdout)

    def result(record, task):
        return next(r for r in record['entries'][0]['response']['results'] if r['task'].rsplit('/', 1)[-1] == task)

    def changes(record):
        return {r['task'].rsplit('/', 1)[-1]: r['change'] for r in record['entries'][0]['response']['results']}

    def mark(name):
        passed.append(name)
        print('PASS ' + name, flush=True)

    def pid():
        return remote(f'systemctl show -p MainPID --value {SERVICE}')

    def cleanup():
        remote(f'systemctl disable --now {SERVICE} 2>/dev/null || true\n'
               f'systemctl reset-failed {SERVICE} 2>/dev/null || true\n'
               f'rm -f /etc/systemd/system/{SERVICE} /run/mmx-release-demo.loaded\nsystemctl daemon-reload\n'
               f'rm -rf {BASE} {FIXTURES} /var/lib/mmx/command_records\n'
               'userdel -r mmxgit 2>/dev/null || true\nuserdel mmxapp 2>/dev/null || true\ngroupdel mmxapp 2>/dev/null || true')

    base = [
        {'group': {'name': 'mmxapp', 'system': True}},
        {'user': {'create_home_directory': False, 'name': 'mmxapp', 'primary_group': 'mmxapp', 'system': True, 'home': BASE, 'shell': '/usr/sbin/nologin'}},
        {'directory': {'path': BASE}},
        {'directory': {'path': BASE + '/releases'}},
    ]

    def release(version, source, extra=(), more_changes=()):
        return base + [{
            "task_block": {
                "id": "app",
                "changes": [
                    {
                        "id": "release",
                        "unpack_archive": {
                            "source": source,
                            "path": f"{BASE}/releases/{version}",
                            "owner": "mmxapp",
                            "group": "mmxapp",
                            "unpack_without_wrapping_folder": True,
                        },
                    },
                    {"id": "switch", "symlink": {"path": BASE + "/current", "target": "releases/" + version}},
                    {"systemd_unit": {"name": SERVICE, "content": f"[Service]\nExecStart=/bin/sh {BASE}/current/run\n\n[Install]\nWantedBy=multi-user.target\n"}},
                    *more_changes,
                ],
                "activate": [{"systemd_service": {"name": SERVICE, "start_at_boot": True, "state": "running"}}],
            }
        }, *extra]

    vm.install_packages(['git'])
    cleanup()
    try:
        for version in ('1.0', '2.0'):
            tarball(workspace / f'files/app-{version}.tar.gz', version)
        project(base)
        run('run')
        project(release('1.0', 'files/app-1.0.tar.gz'))
        assert run('validate')['valid']
        preview = run('dryrun')
        assert result(preview, 'release')['change'] == 'would_change'
        assert result(preview, 'release')['value'] == {'entries': 5}, result(preview, 'release')
        remote(f'test ! -e {BASE}/releases/1.0\ntest -z "$(ls -A {BASE}/releases)"\ntest ! -e {BASE}/current')
        applied = run('run')
        assert result(applied, 'release')['value'] == {'entries': 5}
        assert remote(f'cat /run/mmx-release-demo.loaded') == '1.0'
        assert remote(f'readlink {BASE}/current') == 'releases/1.0'
        assert remote(f"stat -c '%U:%G %a' {BASE}/releases/1.0 {BASE}/releases/1.0/run {BASE}/releases/1.0/lib "
                      f"{BASE}/releases/1.0/lib/data {BASE}/releases/1.0/VERSION").split('\n') == [
            'mmxapp:mmxapp 755', 'mmxapp:mmxapp 755', 'mmxapp:mmxapp 750', 'mmxapp:mmxapp 640', 'mmxapp:mmxapp 644']
        assert remote(f'readlink {BASE}/releases/1.0/start') == 'run'
        record = json.loads(remote(f'cat {BASE}/releases/1.0/.mmx-archive'))
        digest = hashlib.sha256((workspace / 'files/app-1.0.tar.gz').read_bytes()).hexdigest()
        assert record == {'sha256': digest, 'unpack_without_wrapping_folder': True, 'owner': 'mmxapp', 'group': 'mmxapp'}, record
        first = pid()
        inode = remote(f'stat -c %i {BASE}/releases/1.0/VERSION')
        unchanged = run('run')
        assert set(changes(unchanged).values()) == {'unchanged'}, changes(unchanged)
        assert pid() == first and remote(f'stat -c %i {BASE}/releases/1.0/VERSION') == inode
        mark('tar.gz release previewed, extracted as its account, switched by a symlink, and started; unchanged rerun keeps the process and files')

        project(release('2.0', 'files/app-2.0.tar.gz', [{'unpack_archive': {"also_delete_everything_inside": True, 'path': f'{BASE}/releases/1.0', 'state': 'absent'}}]))
        preview = run('dryrun')
        assert changes(preview)['release'] == 'would_change' and changes(preview)['switch'] == 'would_change'
        assert remote(f'readlink {BASE}/current') == 'releases/1.0' and pid() == first
        switched = run('run')
        assert changes(switched)['release'] == 'changed' and changes(switched)['switch'] == 'changed'
        assert remote('cat /run/mmx-release-demo.loaded') == '2.0' and pid() != first
        assert remote(f'readlink {BASE}/current') == 'releases/2.0'
        remote(f'test ! -e {BASE}/releases/1.0\ntest -z "$(ls -A {BASE}/releases | grep -v "^2.0$")"')
        second = pid()
        assert set(changes(run('run')).values()) == {'unchanged'} and pid() == second
        mark('next version extracted beside the running one, switched with one restart, and the old release removed')

        # A deletion in the node task block's changes is a tracked input: it
        # alone restarts the service, once. First the block tracks the
        # releases directory while it keeps both releases.
        remote(f'mkdir -p {BASE}/releases/0.9/lib\ntouch {BASE}/releases/0.9/lib/old')
        prune = {'id': 'prune', 'directory': {'path': BASE + '/releases', 'delete_everything_inside_except': ['2.0', '0.9']}}
        project(release('2.0', 'files/app-2.0.tar.gz', more_changes=[prune]))
        assert changes(run('run'))['prune'] == 'unchanged'
        assert set(changes(run('run')).values()) == {'unchanged'}
        second = pid()
        prune['directory']['delete_everything_inside_except'] = ['2.0']
        project(release('2.0', 'files/app-2.0.tar.gz', more_changes=[prune]))
        preview = run('dryrun')
        assert result(preview, 'prune')['value'] == {'deleted_entries': 3, 'deleted_names': ['0.9']}, result(preview, 'prune')
        assert changes(preview)['release'] == 'unchanged' and changes(preview)['switch'] == 'unchanged'
        assert preview['entries'][0]['response']['change'] == 'would_change'
        remote(f'test -f {BASE}/releases/0.9/lib/old')
        assert pid() == second
        pruned = run('run')
        assert changes(pruned)['prune'] == 'changed' and changes(pruned)['switch'] == 'unchanged'
        remote(f'test ! -e {BASE}/releases/0.9\ntest "$(ls -A {BASE}/releases)" = 2.0')
        third = pid()
        assert third != second and remote('cat /run/mmx-release-demo.loaded') == '2.0'
        assert set(changes(run('run')).values()) == {'unchanged'} and pid() == third
        mark('deleting an old release inside the node task block restarts the service once; the rerun is unchanged')

        # A hostile archive fails in dryrun and run, leaving nothing behind.
        tarball(workspace / 'files/hostile.tar.gz', '3.0', [
            ('app/ok', tarfile.REGTYPE, 0o644, b'fine\n'),
            ('app/etc', tarfile.SYMTYPE, 0o777, b'', '/etc'),
            ('app/../../../../tmp/mmx-escape', tarfile.REGTYPE, 0o644, b'escaped\n')])
        project(base + [{'unpack_archive': {'source': 'files/hostile.tar.gz', 'path': BASE + '/releases/3.0', 'unpack_without_wrapping_folder': True}}])
        assert 'leads out of the directory' in json.dumps(run('dryrun', 1))
        assert 'leads out of the directory' in json.dumps(run('run', 1))
        remote(f'test -z "$(ls -A {BASE}/releases | grep -v "^2.0$")"\ntest ! -e /tmp/mmx-escape')
        mark('an archive with an escaping link or path fails preview and run without leaving files')

        # Unwrapping refuses an incomplete layout instead of dropping entries.
        for outside in ['NOTICE', 'other/README']:
            tarball(workspace / 'files/outside.tar.gz', '3.0', [
                ('app/bin/tool', tarfile.REGTYPE, 0o755, b'tool'),
                (outside, tarfile.REGTYPE, 0o644, b'outside')])
            project(base + [{'retry': False, 'unpack_archive': {'source': 'files/outside.tar.gz',
                    'path': BASE + '/releases/3.0', 'unpack_without_wrapping_folder': True}}])
            for command in ('dryrun', 'run'):
                assert 'outside the one wrapping folder' in json.dumps(run(command, 1))
            remote(f'test ! -e {BASE}/releases/3.0')
        mark('unwrapping refuses top-level files and a second folder without installing a partial release')

        with zipfile.ZipFile(workspace / 'files/tools.zip', 'w', zipfile.ZIP_DEFLATED) as archive:
            for name, mode, data, unix in [('bin/', 0o40750, b'', True), ('bin/tool', 0o100755, b'#!/bin/sh\necho tool\n', True),
                                           ('docs/README.txt', 0, b'no unix mode\n', False)]:
                info = zipfile.ZipInfo(name)
                info.create_system = 3 if unix else 0
                info.external_attr = mode << 16
                archive.writestr(info, data)
        project(base + [{'id': 'tools', 'unpack_archive': {'source': 'files/tools.zip', 'path': BASE + '/tools', 'owner': 'mmxapp'}}])
        assert result(run('run'), 'tools')['value'] == {'entries': 3}
        assert remote(f"stat -c '%U:%G %a' {BASE}/tools/bin {BASE}/tools/bin/tool {BASE}/tools/docs {BASE}/tools/docs/README.txt").split('\n') == [
            'mmxapp:root 750', 'mmxapp:root 755', 'mmxapp:root 755', 'mmxapp:root 644']
        assert remote(f'{BASE}/tools/bin/tool') == 'tool'
        assert result(run('run'), 'tools')['change'] == 'unchanged'
        mark('zip extracted with its Unix modes, default modes for entries without them, and an unchanged rerun')

        # The format comes from the file's contents, never its name: plain
        # tar and tar compressed with xz, bzip2, or zstd. A numeric owner and
        # group need no account on the node.
        members = [('tool/bin', tarfile.DIRTYPE, 0o755), ('tool/bin/run', tarfile.REGTYPE, 0o755, b'#!/bin/sh\necho formats\n')]
        tarball(workspace / 'files/plain.archive', '', members, 'w')
        tarball(workspace / 'files/xz.archive', '', members, 'w:xz')
        tarball(workspace / 'files/bzip2.archive', '', members, 'w:bz2')
        vm.cmd(['zstd', '-q', '-f', '-o', workspace / 'files/zstd.archive', workspace / 'files/plain.archive'])
        kinds = ['plain', 'xz', 'bzip2', 'zstd']
        project(base + [{'directory': {'path': BASE + '/formats'}}] + [
            {'id': kind, 'unpack_archive': {'source': f'files/{kind}.archive', 'path': f'{BASE}/formats/{kind}',
                                            'owner': 4242, 'group': 4343, 'unpack_without_wrapping_folder': True}} for kind in kinds])
        remote('! getent passwd 4242 && ! getent group 4343')
        preview = run('dryrun')
        assert all(result(preview, kind)['value'] == {'entries': 2} for kind in kinds), preview
        remote(f'test ! -e {BASE}/formats/plain')
        applied = run('run')
        for kind in kinds:
            assert result(applied, kind)['change'] == 'changed'
            assert remote(f'{BASE}/formats/{kind}/bin/run') == 'formats'
            assert remote(f"stat -c '%u %g %a' {BASE}/formats/{kind}/bin/run") == '4242 4343 755'
        assert all(result(run('run'), kind)['change'] == 'unchanged' for kind in kinds)
        mark('plain, xz, bzip2, and zstd tars recognized by their contents, extracted with a numeric owner and group, and unchanged on rerun')

        # Removing an extracted release never deletes through a mount point
        # inside it.
        mounted = f'{BASE}/formats/plain/bin/mounted'
        remote(f'mkdir {mounted}\nmount -t tmpfs -o size=64k tmpfs {mounted}\ntouch {mounted}/data')
        project(base + [{'id': 'remove', 'unpack_archive': {"also_delete_everything_inside": True, 'path': f'{BASE}/formats/plain', 'state': 'absent'}}])
        try:
            for command in ('dryrun', 'run'):
                assert 'is a mount point' in result(run(command, 1), 'remove')['reason']
            remote(f'test -f {mounted}/data\ntest -f {BASE}/formats/plain/bin/run')
        finally:
            remote(f'umount {mounted}')
        assert result(run('run'), 'remove')['change'] == 'changed'
        remote(f'test ! -e {BASE}/formats/plain\ntest ! -e {BASE}/formats/.mmx-staging-plain')
        mark('unpack_archive state: absent refuses a release with a mount point inside and deletes it once unmounted')

        # A local repository owned by the application account, with two tags.
        remote(f'rm -rf {FIXTURES}\nmkdir -p {FIXTURES}/src\ncd {FIXTURES}/src\n'
               'git init -q -b main\ngit config user.name MMX\ngit config user.email mmx@example.com\n'
               "printf 'one\\n' > app.txt\ngit add app.txt\ngit commit -q -m one\ngit tag -a v1 -m 'release one'\n"
               "printf 'two\\n' > app.txt\ngit commit -q -am two\ngit tag v2\n"
               f'git clone -q --bare {FIXTURES}/src {FIXTURES}/app.git\nchown -R mmxapp:mmxapp {FIXTURES}/src')
        commits = {tag: remote(f'git -C {FIXTURES}/app.git rev-parse {tag}^{{commit}}') for tag in ('v1', 'v2')}
        checkout = BASE + '/src'

        def clone(ref):
            project(base + [{'id': 'checkout', 'git': {'repo': FIXTURES + '/src', 'check_out_tag': ref, 'path': checkout, 'run_as_user_account': 'mmxapp'}}])

        clone('v1')
        preview = result(run('dryrun'), 'checkout')
        assert preview['change'] == 'would_change' and preview['value'] == {'commit': commits['v1'], 'previous_commit': None}, preview
        remote(f'test ! -e {checkout}')
        cloned = result(run('run'), 'checkout')
        assert cloned['change'] == 'changed' and cloned['value'] == {'commit': commits['v1'], 'previous_commit': None}
        assert remote(f'cat {checkout}/app.txt') == 'one'
        assert remote(f"find {checkout} ! -user mmxapp | wc -l") == '0'
        assert remote(f'runuser -u mmxapp -- git -C {checkout} rev-parse --is-shallow-repository') == 'true'
        unchanged = result(run('run'), 'checkout')
        assert unchanged['change'] == 'unchanged' and unchanged['value'] == {'commit': commits['v1'], 'previous_commit': commits['v1']}
        project(base + [{'id': 'checkout', 'git': {'repo': FIXTURES + '/src', 'check_out_tag': 'v1',
                'path': checkout, 'run_as_user_account': 'mmxapp', 'full_history': True}}])
        assert result(run('dryrun'), 'checkout')['change'] == 'would_change'
        assert remote(f'runuser -u mmxapp -- git -C {checkout} rev-parse --is-shallow-repository') == 'true'
        deepened = result(run('run'), 'checkout')
        assert deepened['change'] == 'changed' and deepened['value']['commit'] == commits['v1']
        assert remote(f'runuser -u mmxapp -- git -C {checkout} rev-parse --is-shallow-repository') == 'false'
        assert result(run('run'), 'checkout')['change'] == 'unchanged'
        # A hook in the checkout runs as the account, never as root.
        remote(f"printf '#!/bin/sh\\nid -un > {checkout}/hook-user\\n' > {checkout}/.git/hooks/post-checkout\n"
               f'chmod 755 {checkout}/.git/hooks/post-checkout\nchown mmxapp:mmxapp {checkout}/.git/hooks/post-checkout')
        clone('v2')
        updated = result(run('run'), 'checkout')
        assert updated['change'] == 'changed' and updated['value'] == {'commit': commits['v2'], 'previous_commit': commits['v1']}
        assert remote(f'cat {checkout}/app.txt') == 'two' and remote(f'cat {checkout}/hook-user') == 'mmxapp'
        assert remote(f"find {checkout} ! -user mmxapp | wc -l") == '0'
        remote(f"printf 'edited\\n' > {checkout}/app.txt")
        clone('v1')
        failed = run('run', 1)
        assert 'have local changes on the node: app.txt;' in json.dumps(failed) and remote(f'cat {checkout}/app.txt') == 'edited'
        # discard_the_changes restores the edited file, lists it, and moves on;
        # an untracked file stays. A dryrun lists the file and restores nothing.
        remote(f"printf 'built\\n' > {checkout}/build.out\nchown mmxapp:mmxapp {checkout}/build.out")
        project(base + [{'id': 'checkout', 'git': {'repo': FIXTURES + '/src', 'check_out_tag': 'v1', 'path': checkout, 'run_as_user_account': 'mmxapp',
                                                   'files_changed_in_checkout_on_node': 'discard_the_changes'}}])
        preview = result(run('dryrun'), 'checkout')
        assert preview['change'] == 'would_change' and preview['value']['restored_files'] == ['app.txt'], preview
        assert remote(f'cat {checkout}/app.txt') == 'edited'
        discarded = result(run('run'), 'checkout')
        assert discarded['change'] == 'changed' and discarded['value'] == {
            'commit': commits['v1'], 'previous_commit': commits['v2'], 'restored_files': ['app.txt']}, discarded
        assert remote(f'cat {checkout}/app.txt') == 'one' and remote(f'cat {checkout}/build.out') == 'built'
        assert result(run('run'), 'checkout')['change'] == 'unchanged'
        mark('Git tag cloned as the application account, unchanged rerun, update to another tag with hooks as that account, '
             'local edits refused, or discarded and listed with discard_the_changes')

        # A checkout is a tracked input of its task block: a new commit runs
        # the command that follows it, also after a run that stopped early.
        code = BASE + '/code'

        def deploy(ref, approved=True, fail=False):
            project(base + [{
                "task_block": {
                    "id": "shop_code",
                    "changes": [{"id": "code", "git": {"repo": FIXTURES + "/src", "check_out_tag": ref, "path": code, "run_as_user_account": "mmxapp"}}],
                    "checks_before_restart_or_reload": [{"assert": {"that": approved, "message": "deployments are frozen"}}],
                    "validate": [{"shell": {"script": "false" if fail else "true"}}],
                    "activate": [{
                        "id": "tell_app_to_restart",
                        "shell": {"script": f"echo restart >> {code}/restarts.log", "run_as_user_account": "mmxapp",
                                  "only_when_tracked_inputs_changed": True},
                    }],
                }
            }])

        def restarts():
            return remote(f'cat {code}/restarts.log 2>/dev/null | wc -l')

        deploy('v1')
        preview = result(run('dryrun'), 'tell_app_to_restart')
        assert preview['change'] == 'would_change' and preview['reason'] == f'would run: tracked input checkout {code} would change', preview
        assert changes(run('run'))['tell_app_to_restart'] == 'changed' and restarts() == '1'
        assert run('run')['entries'][0]['response']['change'] == 'unchanged' and restarts() == '1'
        deploy('v2', approved=False)
        assert 'deployments are frozen' in json.dumps(run('run', 1))
        assert remote(f'cat {code}/app.txt') == 'one' and restarts() == '1'
        deploy('v2', fail=True)
        run('run', 1)
        assert remote(f'cat {code}/app.txt') == 'two' and restarts() == '1'
        deploy('v2')
        recovered = run('run')
        assert changes(recovered)['code'] == 'unchanged' and changes(recovered)['tell_app_to_restart'] == 'changed' and restarts() == '2'
        assert run('run')['entries'][0]['response']['change'] == 'unchanged' and restarts() == '2'
        mark('a Git checkout is a tracked input: a new commit runs the command that follows it once, after an interrupted run too, '
             'and the checks before it can refuse the deployment')

        # A private repository over SSH, with its key from a secret.
        vm.cmd(['ssh-keygen', '-q', '-t', 'ed25519', '-N', '', '-C', 'mmx-deploy', '-f', workspace / 'deploy'])
        key = (workspace / 'deploy').read_text()
        public = (workspace / 'deploy.pub').read_text().strip()
        remote(f'useradd --create-home --shell /bin/sh mmxgit\ninstall -d -m 700 -o mmxgit -g mmxgit /home/mmxgit/.ssh\n'
               f'printf "%s\\n" {shlex.quote(public)} > /home/mmxgit/.ssh/authorized_keys\n'
               'chown mmxgit:mmxgit /home/mmxgit/.ssh/authorized_keys\nchmod 600 /home/mmxgit/.ssh/authorized_keys\n'
               f'mv {FIXTURES}/app.git /home/mmxgit/app.git\nchown -R mmxgit:mmxgit /home/mmxgit/app.git')
        known = '127.0.0.1 ' + remote('cat /etc/ssh/ssh_host_ed25519_key.pub')
        vm.ENV['MMX_TEST_DEPLOY_KEY'] = key
        project(base + [{'id': 'private', 'git': {'repo': 'mmxgit@127.0.0.1:app.git', 'check_out_tag': 'v2', 'path': BASE + '/private',
                                                  'run_as_user_account': 'mmxapp', 'ssh_private_key': '{{ .secrets.deploy_key }}', 'ssh_known_hosts_lines': known}}],
                {'deploy_key': 'MMX_TEST_DEPLOY_KEY'})
        # MMX hands the key only to ssh, so the secret does not hide the
        # task's result: it names the commit.
        private = run('run')
        checked_out = result(private, 'private')
        assert checked_out['change'] == 'changed' and checked_out.get('secret') is not True, checked_out
        assert checked_out['value'] == {'commit': commits['v2'], 'previous_commit': None}, checked_out
        assert remote(f'cat {BASE}/private/app.txt') == 'two'
        assert result(run('run'), 'private')['change'] == 'unchanged'
        # A key that the server does not know: the result gives the reason
        # of ssh and git, and the key is not in it.
        vm.cmd(['ssh-keygen', '-q', '-t', 'ed25519', '-N', '', '-C', 'mmx-unknown', '-f', workspace / 'unknown'])
        unknown = (workspace / 'unknown').read_text()
        vm.ENV['MMX_TEST_DEPLOY_KEY'] = unknown
        refused = run('run', 1)
        vm.ENV['MMX_TEST_DEPLOY_KEY'] = key
        reason = result(refused, 'private')['reason']
        assert result(refused, 'private').get('secret') is not True, refused
        assert ': git ls-remote mmxgit@127.0.0.1:app.git ' in reason and 'mmxgit@127.0.0.1: Permission denied (publickey' in reason, refused
        assert unknown.splitlines()[1] not in json.dumps(refused)
        body = key.splitlines()[1]
        # The key was a file in the node task's private directory, which the
        # account mmxapp reached through /run/mmx. The place where each such
        # directory is made is empty again, and the key is nowhere else.
        assert remote('stat -c "%a %U" /run/mmx /run/mmx/private_task_directories') == '711 root\n711 root'
        remote(f'test -z "$(ls -A /run/mmx/private_task_directories)"\n! grep -rqF {shlex.quote(body)} /var/lib/mmx /run/mmx {BASE}/private/.git')
        # A node job worker process that is killed cannot remove its task's
        # directory. The node removes what it left before the next node job
        # starts.
        remote('install -d -m 700 -o mmxapp /run/mmx/private_task_directories/git-left-by-a-killed-worker\n'
               'echo s3cret > /run/mmx/private_task_directories/git-left-by-a-killed-worker/password')
        assert result(run('run'), 'private')['change'] == 'unchanged'
        remote('test -z "$(ls -A /run/mmx/private_task_directories)"')
        assert body not in json.dumps(private)
        for saved in (workspace / '.mmx').rglob('*'):
            if saved.is_file():
                assert body.encode() not in saved.read_bytes() and unknown.splitlines()[1].encode() not in saved.read_bytes(), saved
        mark('private SSH repository cloned with a secret key and pinned host key; the result names the commit, a key that the server '
             'refuses gives the reason of ssh, and the key stays out of records and /run')
    finally:
        vm.ENV.pop('MMX_TEST_DEPLOY_KEY', None)
        cleanup()
    (workspace / 'summary.json').write_text(json.dumps(passed, indent=2) + '\n')
    print(f'{len(passed)} release-directory workflows passed', flush=True)


if __name__ == '__main__':
    vm.run_suite(main)
