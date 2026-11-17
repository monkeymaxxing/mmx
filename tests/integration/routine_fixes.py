#!/usr/bin/env python3
"""The modules of routine fixes through the public CLI: systemd_service with restart_now in a one-time playbook,
which mmx check-one-time-playbooks finds still needed before its run and done after, systemd_journal_vacuum,
apt_downloaded_package_files, and delete_old_files. Node state is checked with systemctl, stat, find, and ls."""
import functools
import json
import shlex
import shutil
import time

import vm

UNIT = 'mmx-test-hanging-worker.service'
MARKER = '/var/lib/mmx/commands_run_once/mmx_test_restart_hanging_worker'
BASE = '/var/tmp/mmx-test-routine-fixes'
LOGS = BASE + '/logs'
OUTSIDE = BASE + '/outside'
APT_ARCHIVES = '/var/cache/apt/archives'
# Package files the suite puts into apt's archives directory, beside those
# that the guest's own installations left there.
TEST_PACKAGE_FILES = ('mmx-test-one_1.0_all.deb', 'mmx-test-two_2.0_amd64.deb', 'partial/mmx-test-three_3.0_all.deb')
MEBIBYTE = 1024 * 1024


def main():
    vm.up()
    node, ssh = vm.connection()
    vm.reset()
    workspace = vm.ROOT / 'routine-fixes'
    if workspace.exists():
        shutil.rmtree(workspace)
    vm.write_inventory(workspace, {'nodes': {'app': node}}, name='example')
    vm.build()
    mmx = vm.ROOT / 'bin/mmx'
    passed = []
    marked = time.monotonic()

    def remote(script):
        return vm.cmd([*ssh, 'sudo -n /bin/sh -c ' + shlex.quote('set -eu\n' + script)]).stdout.strip()

    def project(tasks):
        vm.write_project(workspace, project_name='routine_fixes', node_task_defaults={'retry': {'pause_between_tries': '0s'}},
                         fleet_stages=[{'id': 'fix', 'nodes': 'all', 'node_tasks': tasks}])

    def command(*args, expected):
        p = vm.cmd([mmx, *args], cwd=workspace, check=False, timeout=600)
        assert p.returncode in (expected if isinstance(expected, tuple) else (expected,)), (args, p.returncode, p.stdout, p.stderr)
        return p

    def run(name='run', playbook='configure', expected=None):
        p = command(name, playbook, '--json', expected=vm.success_codes(name) if expected is None else expected)
        return json.loads(p.stdout)

    def result(record, task='fixed'):
        return {r['task'].rsplit('/', 1)[-1]: r for r in record['entries'][0]['response']['results']}[task]

    def mark(message):
        nonlocal marked
        now = time.monotonic()
        passed.append(message)
        print(f'PASS {message} ({now - marked:.0f} s)', flush=True)
        marked = now

    def cleanup():
        remote(f'systemctl stop {UNIT} 2>/dev/null || true\nrm -f /etc/systemd/system/{UNIT} {MARKER}\nsystemctl daemon-reload\n'
               f'for mount in {LOGS}/mounted {LOGS}/bound; do if mountpoint -q "$mount"; then umount "$mount"; fi; done\n'
               f'rm -rf {BASE} /var/tmp/mmx-test-routine-fixes-link\n' + ''.join(f'rm -f {APT_ARCHIVES}/{name}\n' for name in TEST_PACKAGE_FILES))

    distribution = remote('. /etc/os-release; echo "$ID $VERSION_ID"')
    cleanup()
    try:
        restart_now(remote, command, run, result, mark, workspace)
        journal_vacuum(remote, project, run, result, mark)
        downloaded_package_files(remote, project, run, result, mark)
        old_files(remote, project, run, result, mark, command)
    finally:
        cleanup()
    print(f'{len(passed)} routine fix workflows passed on {distribution}', flush=True)


def restart_now(remote, command, run, result, mark, workspace):
    """A one-time playbook restarts a running unit once: still needed before its run, done after, and its rerun
    leaves the unit alone. The unit's invocation ID, which systemd gives each start, tells a restart."""
    remote(f'printf "[Service]\\nExecStart=/bin/sleep infinity\\n" > /etc/systemd/system/{UNIT}\n'
           f'systemctl daemon-reload\nsystemctl start {UNIT}')

    def invocation():
        return remote(f'systemctl show --property=InvocationID --value {UNIT}')

    name = time.strftime('%Y-%m-%d') + '_restart_hanging_worker'
    directory = workspace / 'one_time_playbooks'
    directory.mkdir()
    vm.write_settings(workspace, {'project_name': 'routine_fixes', 'default_inventory': 'example'})
    (directory / (name + '.yml')).write_text(json.dumps({'fleet_stages': [{'id': 'restart_hanging_worker', 'nodes': 'all', 'node_tasks': [
        {'id': 'fixed', 'systemd_service': {'name': UNIT, 'state': 'running', 'restart_now': True, 'run_only_once_marker_file': MARKER}}]}]}, indent=2) + '\n')
    before = invocation()
    checked = command('check-one-time-playbooks', expected=2)
    assert checked.stdout == f'Inventory: example (inventories/example.yml)\n\n{name}\n  still needed on 1 server: app\n', checked.stdout
    preview = result(run('dryrun', name))
    assert preview['change'] == 'would_change' and preview['reason'] == f'would restart {UNIT} once: the marker file {MARKER} does not exist', preview
    assert invocation() == before and remote(f'test -e {MARKER} && echo yes || true') == ''
    ran = result(run('run', name))
    assert ran['change'] == 'changed' and ran['reason'] == f'restarted {UNIT} once and wrote the marker file {MARKER}', ran
    after = invocation()
    assert after != before and remote(f'systemctl is-active {UNIT}') == 'active', (before, after)
    assert remote(f'cat {MARKER}').startswith(f'The restart of {UNIT} succeeded at '), remote(f'cat {MARKER}')
    checked = command('check-one-time-playbooks', expected=0)
    assert checked.stdout == f'Inventory: example (inventories/example.yml)\n\n{name}\n  done on the 1 server\n', checked.stdout
    again = result(run('run', name))
    assert again['change'] == 'unchanged' and invocation() == after, again
    mark('a one-time playbook with restart_now is still needed before its run; the dryrun leaves the unit as it is; the run restarts the '
         'running unit once and writes the marker file; then the playbook is done, and a rerun leaves the unit alone')

    # With the marker file there, the task keeps the unit running as any
    # task with state: running does.
    remote(f'systemctl stop {UNIT}')
    command('check-one-time-playbooks', expected=2)
    started = result(run('run', name))
    assert started['change'] == 'changed' and started['reason'] == 'service was not running; started', started
    assert remote(f'systemctl is-active {UNIT}') == 'active'
    command('check-one-time-playbooks', expected=0)
    (directory / (name + '.yml')).unlink()
    directory.rmdir()
    mark('with its marker file there, the restart_now task starts the unit when it stopped, as state: running does, and restarts nothing')


def journal_vacuum(remote, project, run, result, mark):
    """systemd_journal_vacuum on the node's own journal: archived files made with journalctl --rotate. The dryrun's
    prediction is compared with what journalctl deletes, and both with the files on the node."""
    machine = remote('cat /etc/machine-id')
    directory = f'/var/log/journal/{machine}'
    if remote(f'test -d {directory} && echo yes || true') != 'yes':
        directory = f'/run/log/journal/{machine}'
    # Three archived files: lines, then a rotation, three times.
    for n in range(3):
        remote(f'for i in $(seq 200); do echo "mmx-test routine fixes line $i of {n}"; done | systemd-cat -t mmx-test-routine-fixes\n'
               'journalctl --rotate')

    def archived():
        """The archived files of the directory, with the bytes each takes on disk, in journald's order of deletion:
        the files of one sequence by sequence number, others by the time in their names. The two orders differ when
        the clock went back during a boot, as on a new cloud server."""
        lines = remote(f"find {directory} -maxdepth 1 -type f -name '*@*.journal' -printf '%f %b\\n'").splitlines()
        files = sorted((line.split()[0], int(line.split()[1]) * 512) for line in lines)

        def journald_order(a, b):
            (sequence_a, number_a, time_a), (sequence_b, number_b, time_b) = (
                f[0].removesuffix('.journal').rsplit('@', 1)[1].split('-') for f in (a, b))
            if sequence_a == sequence_b:
                return int(number_a, 16) - int(number_b, 16)
            return int(time_a, 16) - int(time_b, 16) or (sequence_a > sequence_b) - (sequence_a < sequence_b)
        return sorted(files, key=functools.cmp_to_key(journald_order))

    def settled():
        """journald takes an archived file offline in the background and can make it smaller there, and a filesystem
        such as btrfs allocates the blocks of new data only when it writes the data out: wait until the archived files
        keep their blocks for a second."""
        remote('journalctl --sync\nsync')
        before, deadline = archived(), time.monotonic() + 30
        while True:
            time.sleep(1)
            remote('sync')
            now = archived()
            if now == before:
                return
            assert time.monotonic() < deadline, ('the archived journal files keep changing', before, now)
            before = now

    def names(files):
        return [name for name, _ in files]

    def total():
        return sum(int(b) * 512 for b in remote(f"find {directory} -maxdepth 1 -type f -name '*.journal*' -printf '%b\\n'").split())

    settled()
    files = archived()
    assert len(files) >= 3, files
    # The oldest archived file looks 30 days old; journald takes the
    # earliest of the times in its name and on the file.
    oldest, oldest_bytes = files[0]
    remote(f"touch -d '30 days ago' {directory}/{oldest}")
    project([{'id': 'fixed', 'systemd_journal_vacuum': {'keep_newer_than': '14d'}}])
    preview = result(run('dryrun'))
    assert preview['change'] == 'would_change' and preview['value'] == {'freed_bytes': oldest_bytes, 'deleted_archived_journal_files': 1}, (preview, files)
    assert names(archived()) == names(files), (archived(), files)
    ran = result(run('run'))
    assert ran['change'] == 'changed' and ran['value'] == preview['value'], (ran, preview)
    assert ran['reason'].startswith('deleted 1 archived journal files and freed '), ran
    assert names(archived()) == names(files[1:]), (archived(), files)
    for name in ('dryrun', 'run'):
        again = result(run(name))
        assert again['change'] == 'unchanged' and again['value'] == {'freed_bytes': 0, 'deleted_archived_journal_files': 0}, again
    mark('systemd_journal_vacuum with keep_newer_than: the dryrun predicts exactly the archived file that journalctl --vacuum-time then '
         'deletes, by its modification time, and the bytes it took on disk; the rerun is unchanged')

    # A size that the journal's files exceed by less than the oldest
    # archived file: journald deletes the oldest file, and the dryrun says
    # which and how many bytes before the run.
    settled()
    files = archived()
    limit = (total() - 1) // MEBIBYTE
    project([{'id': 'fixed', 'systemd_journal_vacuum': {'keep_at_most_size': f'{limit}MiB'}}])
    preview = result(run('dryrun'))
    assert preview['change'] == 'would_change' and preview['value']['deleted_archived_journal_files'] >= 1, (preview, files, limit)
    ran = result(run('run'))
    assert ran['change'] == 'changed' and ran['value'] == preview['value'], (ran, preview)
    deleted = preview['value']['deleted_archived_journal_files']
    assert names(archived()) == names(files[deleted:]) and ran['value']['freed_bytes'] == sum(b for _, b in files[:deleted]), (archived(), files, ran)
    assert total() <= limit * MEBIBYTE, (total(), limit)
    assert result(run('run'))['change'] == 'unchanged'
    mark('systemd_journal_vacuum with keep_at_most_size: the dryrun predicts the oldest archived files that journalctl --vacuum-size '
         'deletes until the directory fits, and the run deletes exactly those and reports their bytes')


def downloaded_package_files(remote, project, run, result, mark):
    """apt_downloaded_package_files: the package files in apt's archives directory, the guest's own and three of the
    suite's, one in partial/; once with another apt holding the lock of the archives directory for a few seconds."""
    def package_files():
        lines = remote(f"find {APT_ARCHIVES} {APT_ARCHIVES}/partial -maxdepth 1 ! -type d ! -name lock -printf '%p %s\\n' | sort -u").splitlines()
        return {line.split()[0]: int(line.split()[1]) for line in lines}

    def put_package_files():
        remote(''.join(f'head -c {100000 * (n + 1)} /dev/urandom > {APT_ARCHIVES}/{name}\n' for n, name in enumerate(TEST_PACKAGE_FILES)))

    put_package_files()
    files = package_files()
    assert len(files) >= 3, files
    project([{'id': 'fixed', 'apt_downloaded_package_files': {'state': 'absent'}}])
    preview = result(run('dryrun'))
    assert preview['change'] == 'would_change' and preview['value'] == {'deleted_package_files': len(files), 'deleted_bytes': sum(files.values())}, (preview, files)
    assert package_files() == files
    ran = result(run('run'))
    assert ran['change'] == 'changed' and ran['value'] == preview['value'], (ran, preview)
    assert ran['reason'].startswith(f'deleted {len(files)} package files totaling ') and ran['reason'].endswith(f' from {APT_ARCHIVES}/'), ran
    assert package_files() == {} and remote(f'test -e {APT_ARCHIVES}/lock && test -d {APT_ARCHIVES}/partial && echo yes') == 'yes'
    for name in ('dryrun', 'run'):
        again = result(run(name))
        assert again['change'] == 'unchanged' and again['reason'] == f"apt's archives directory {APT_ARCHIVES}/ holds no downloaded package file", again
    mark('apt_downloaded_package_files: the dryrun counts the package files and their bytes, apt-get clean deletes them all, apt\'s lock '
         'and partial/ stay, and the rerun runs nothing')

    # Another apt holds the lock of the archives directory for six seconds,
    # as apt's timer does while it downloads: the task waits for it.
    put_package_files()
    remote("systemd-run --unit=mmx-test-apt-lock --collect python3 -c "
           "'import fcntl, time; f = open(\"/var/cache/apt/archives/lock\", \"w\"); fcntl.lockf(f, fcntl.LOCK_EX); time.sleep(6)'\n"
           "for i in $(seq 50); do systemctl is-active --quiet mmx-test-apt-lock && break; sleep 0.1; done")
    started = time.monotonic()
    ran = result(run('run'))
    waited = time.monotonic() - started
    assert ran['change'] == 'changed' and ran['value']['deleted_package_files'] == 3 and package_files() == {}, ran
    assert waited >= 3, f'the task did not wait for the lock: it took {waited:.1f} s'
    mark('apt_downloaded_package_files waits while another apt holds the lock of the archives directory, then deletes')


def old_files(remote, project, run, result, mark, command):
    """delete_old_files below one directory: matching names, ages by touch, a symlink to a file and one to a
    directory outside, a tmpfs and a bind mount of the same filesystem below the directory."""
    remote(f'mkdir -p {LOGS}/workers {LOGS}/mounted {LOGS}/bound {OUTSIDE}/linked {OUTSIDE}/bound\n'
           f'for f in app.log app.log.1 app.log.2.gz app.log.3.gz workers/worker.log.1.gz; do printf "%s\\n" "$f" > {LOGS}/$f; done\n'
           f'echo target > {OUTSIDE}/target.gz\necho linked > {OUTSIDE}/linked/old.gz\necho bound > {OUTSIDE}/bound/old.gz\n'
           f'ln -s {OUTSIDE}/target.gz {LOGS}/link.gz\nln -s {OUTSIDE}/linked {LOGS}/linked\n'
           f'mount -t tmpfs -o size=64k tmpfs {LOGS}/mounted\necho mounted > {LOGS}/mounted/old.gz\n'
           f'mount --bind {OUTSIDE}/bound {LOGS}/bound\n'
           f"find {LOGS}/ {OUTSIDE} -type f ! -name app.log.3.gz -exec touch -d '30 days ago' {{}} +")

    def files():
        return sorted(remote(f'cd {BASE} && find . \\( -type f -o -type l \\) | sort').splitlines())

    before = files()
    project([{'id': 'fixed', 'delete_old_files': {'directory': LOGS, 'name_patterns': ['*.gz', '*.[0-9]'], 'older_than': '7d', 'include_subdirectories': True}}])
    deleted = [f'{LOGS}/app.log.1', f'{LOGS}/app.log.2.gz', f'{LOGS}/workers/worker.log.1.gz']
    sizes = sum(len(path.rsplit('/logs/', 1)[1]) + 1 for path in deleted)
    preview = result(run('dryrun'))
    assert preview['change'] == 'would_change' and preview['value'] == {'deleted_files': 3, 'deleted_bytes': sizes, 'deleted_file_examples': deleted}, preview
    assert preview['reason'] == (f'would delete 3 files totaling {sizes} bytes in {LOGS}; left alone 1 symlinks whose names match and 2 mount points, '
                                 'which delete_old_files never follows'), preview
    assert files() == before
    ran = result(run('run'))
    assert ran['change'] == 'changed' and ran['value'] == preview['value'], ran
    left = [f for f in before if BASE + f[1:] not in deleted]
    assert files() == left, (files(), left)
    assert remote(f'cat {OUTSIDE}/target.gz {OUTSIDE}/linked/old.gz {LOGS}/mounted/old.gz {LOGS}/bound/old.gz') == 'target\nlinked\nmounted\nbound'
    assert remote(f'test -L {LOGS}/link.gz && test -L {LOGS}/linked && echo yes') == 'yes'
    for name in ('dryrun', 'run'):
        again = result(run(name))
        assert again['change'] == 'unchanged' and again['value']['deleted_files'] == 0, again
    mark('delete_old_files deletes the old files whose names match, also in a directory below; it follows no symlink to a file or a '
         'directory and enters neither a tmpfs nor a bind mount; the dryrun lists the same files; the rerun is unchanged')

    # Only directly in the directory; a directory that is a symlink fails;
    # a directory one level below / is refused before any connection.
    remote(f"for f in top.gz workers/below.gz; do echo old > {LOGS}/$f; touch -d '30 days ago' {LOGS}/$f; done")
    project([{'id': 'fixed', 'delete_old_files': {'directory': LOGS, 'name_patterns': ['*.gz'], 'older_than': '7d', 'include_subdirectories': False}}])
    ran = result(run('run'))
    assert ran['value']['deleted_file_examples'] == [f'{LOGS}/top.gz'], ran
    assert remote(f'test -e {LOGS}/workers/below.gz && echo yes') == 'yes'
    # A link as the directory, and a link in a middle component of its path:
    # each fails, names the link, and deletes nothing where it points.
    remote(f'ln -s {LOGS} {BASE}/logs-link\nln -s {BASE} /var/tmp/mmx-test-routine-fixes-link')
    for directory, link in ((BASE + '/logs-link', BASE + '/logs-link'),
                            ('/var/tmp/mmx-test-routine-fixes-link/logs', '/var/tmp/mmx-test-routine-fixes-link')):
        project([{'id': 'fixed', 'delete_old_files': {'directory': directory, 'name_patterns': ['*.gz'], 'older_than': '7d', 'include_subdirectories': True}}])
        for name in ('dryrun', 'run'):
            failed = result(run(name, expected=1))
            assert failed['status'] == 'failed' and failed['change'] == 'unchanged' and failed['reason'].startswith(
                f'{link} is a symlink; delete_old_files deletes only below the directory as written and follows no link on its path'), failed
        assert remote(f'test -e {LOGS}/workers/below.gz && echo yes') == 'yes'
    project([{'id': 'fixed', 'delete_old_files': {'directory': '/var', 'name_patterns': ['*.gz'], 'older_than': '7d', 'include_subdirectories': False}}])
    refused = command('validate', expected=1)
    assert 'a directory that MMX deletes from or deletes must be an absolute path at least two levels deep' in refused.stderr, refused.stderr
    mark('delete_old_files with include_subdirectories: false deletes only directly in the directory; a directory that is a symlink, and '
         'one with a symlink in a middle component of its path, fail the dryrun and the run, naming the link; a directory one level below / '
         'is refused by validate')


if __name__ == '__main__':
    vm.run_suite(main)
