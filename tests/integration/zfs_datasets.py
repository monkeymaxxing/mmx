#!/usr/bin/env python3
"""zfs_dataset through the public CLI, on a ZFS pool that is a file inside the node; checked with zfs get, zfs list, and findmnt, never through MMX.

The suite runs the whole test where ZFS installs in under two minutes, and on Debian 13 with its build of the kernel module: Ubuntu 24.04 and 26.04 (the module comes with the kernel), Rocky Linux 9 and AlmaLinux 10 (the OpenZFS project's prebuilt module, or, where it has no build for the node's kernel, built on the node as the page says), and Debian 13 (built on the node, about five minutes). There it needs a virtual machine, because a container cannot load a kernel module, and it installs ZFS with the node tasks that the module's page shows under "Set up". On Debian 12 it runs the test without the tool, in a container: a dryrun reports the task not checked, and a run fails once and names the program and its package. A build of ZFS there takes four to seven minutes and breaks whenever the distribution's kernel is newer than ZFS supports."""
import json
import re
import shlex
import shutil
import sys
import time

import vm

BASE = '/var/tmp/mmx-zfs-datasets'
POOL = 'mmxtest'
# Where the suite installs ZFS and runs the whole test: the distribution's ID
# with the major versions. incus.py's ZFS_DISTRIBUTIONS names the same
# distributions, which run this suite in a VM.
INSTALLS_ZFS = {'ubuntu': ('24', '26'), 'debian': ('13',), 'rocky': ('9',), 'almalinux': ('10',)}
# The start of the reason without the program zfs, up to the first package
# that it names; which distributions it lists after that is the module's own
# text.
WITHOUT_ZFS = 'zfs_dataset: the program zfs is not installed; install the package zfsutils-linux (Debian and Ubuntu)'
# The names of the installed packages; the keys that dnf imports are no
# packages of the distribution.
INSTALLED_PACKAGES = ("if command -v dpkg-query >/dev/null; then dpkg-query -W -f '${db:Status-Abbrev} ${Package}\\n' | awk '$1 == \"ii\" {print $2}'; "
                      "else rpm -qa --qf '%{NAME}\\n' | grep -v '^gpg-pubkey$'; fi | sort -u")
# The keys of package repositories that rpm knows.
IMPORTED_KEYS = 'if command -v rpm >/dev/null && ! command -v dpkg-query >/dev/null; then rpm -q gpg-pubkey 2>/dev/null | grep -v "not installed" | sort; fi'
# What ZFS and the tasks that install it write outside their packages.
LEFT_BY_ZFS = ['/etc/zfs', '/etc/exports.d', '/var/lib/dkms', '/etc/yum.repos.d/zfs.repo', '/etc/dnf/plugins/versionlock.list', '/etc/dnf/plugins/versionlock.list.rpmsave',
               '/etc/modprobe.d/mmx-test-no-zfs.conf', '/etc/default/zfs', '/etc/sudoers.d/zfs']


# The page's heading over the tasks that replace the last three of the
# Red Hat family's tasks when OpenZFS's repository zfs-kmod has no build for
# the node's kernel, and the error that says so.
WITHOUT_KMOD_BUILD = "\n### When zfs-kmod has no build for the node's kernel\n"
# The heading over the last task, which loads the kernel module everywhere.
EVERY_DISTRIBUTION = "\n### Notes for every distribution\n"
NO_KMOD_BUILD_ERROR = re.compile(r'nothing provides kernel\([^)]*\) = \w+ needed by kmod-zfs-')


def set_up_playbook(build_on_the_node=False):
    """A playbook of the node tasks that the zfs_dataset page shows under "Set up", so that the page is what the suite runs.
    build_on_the_node: the page's tasks for a node whose kernel zfs-kmod has no build for, in place of the last three.
    Both end with the task under "Notes for every distribution" that loads the kernel module."""
    page = (vm.REPO / 'official_extra_modules/zfs_dataset/README.md').read_text()
    section = page.split('\n## Set up\n', 1)[1].split('\n## ', 1)[0]
    usual, alternative = section.split(WITHOUT_KMOD_BUILD, 1)
    alternative, notes = alternative.split(EVERY_DISTRIBUTION, 1)

    def tasks(text):
        """Each node task of the text's YAML blocks, each ending in a blank line."""
        blocks = [block.split('```', 1)[0] for block in text.split('```yaml\n')[1:]]
        assert blocks and all(block.startswith('- ') for block in blocks), blocks
        return ['- ' + task.rstrip('\n') + '\n\n' for block in blocks for task in ('\n' + block).split('\n- ')[1:]]
    chosen = (tasks(usual)[:-3] + tasks(alternative) if build_on_the_node else tasks(usual)) + tasks(notes)
    indented = ''.join('      ' + line if line.strip() else line for task in chosen for line in task.splitlines(keepends=True))
    return 'fleet_stages:\n  - id: set_up\n    nodes: all\n    node_tasks:\n' + indented


def main():
    vm.up()
    node, ssh = vm.connection()
    vm.reset()
    workspace = vm.ROOT / 'zfs-datasets'
    if workspace.exists():
        shutil.rmtree(workspace)
    vm.write_inventory(workspace, {'nodes': {'storage': node}}, name='example')
    vm.build()
    mmx = vm.ROOT / 'bin/mmx'
    passed = []
    marked = time.monotonic()

    def remote(script, check=True):
        return vm.cmd([*ssh, 'sudo -n /bin/sh -c ' + shlex.quote('set -eu\nPATH=/usr/sbin:/sbin:$PATH\n' + script)], check=check, timeout=900).stdout.strip()

    def run(command, expected=None, playbook='configure', timeout=600):
        result = vm.cmd([mmx, command, *vm.playbook_args(command, playbook), '--json'], cwd=workspace, check=False, timeout=timeout)
        codes = vm.success_codes(command) if expected is None else (expected,)
        assert result.returncode in codes, (command, result.returncode, result.stdout, result.stderr)
        return json.loads(result.stdout)

    def project(tasks):
        vm.write_project(workspace, project_name='zfs_datasets', node_task_defaults={'retry': {'pause_between_tries': '0s'}},
                         fleet_stages=[{'id': 'configure', 'nodes': 'all', 'node_tasks': tasks}])

    def results(record):
        return record['entries'][0]['response']['results']

    def one(tasks, command, expected=None):
        """Run one node task list and return the results of its tasks."""
        project(tasks if isinstance(tasks, list) else [tasks])
        return results(run(command, expected))

    def is_result(result, status, change, reason=None, tries=None):
        assert result['status'] == status and result['change'] == change and result.get('module_kind') == 'official_extra', result
        if reason is not None:
            assert reason in result.get('reason', ''), result
        if tries is not None:
            assert len(result.get('tries') or [result]) == tries, result
        return result

    def refused(task, reason):
        """The task cannot succeed on the node as it is: it fails in a dryrun and in a run with the same reason, after one try, and has changed nothing."""
        for command in ('dryrun', 'run'):
            result = is_result(one(task, command, 1)[0], 'failed', 'unchanged', reason=reason, tries=1)
            assert result['failure_kind'] == 'config', result

    def waits_for_an_earlier_task(task, reason):
        """The node is in a state that an earlier node task of the same playbook may change: a dryrun does not check the task, and a run fails once as a missing prerequisite and changes nothing."""
        is_result(one(task, 'dryrun', 3)[0], 'not_checked', 'unknown', reason=reason)
        result = is_result(one(task, 'run', 1)[0], 'failed', 'unchanged', reason=reason, tries=1)
        assert result['failure_kind'] == 'missing_prerequisite', result

    def mark(message):
        # The seconds since the last mark say where the suite's time goes.
        nonlocal marked
        now = time.monotonic()
        passed.append(message)
        print(f'PASS {message} ({now - marked:.0f} s)', flush=True)
        marked = now

    def dataset(name, properties=None, **more):
        return {'id': 'dataset', 'zfs_dataset': {'name': f'{POOL}/{name}', **({'properties': properties} if properties is not None else {}), **more}}

    def exists(name):
        return remote(f'zfs list -H -o name {POOL}/{name} 2>/dev/null || true') == f'{POOL}/{name}'

    def value(name, zfs_property, exact=True):
        """A property's value as zfs get prints it, with exact numbers or as a person writes it."""
        return remote(f"zfs get -H {'-p ' if exact else ''}-o value {zfs_property} {POOL}/{name}")

    def cleanup():
        remote(f'''
systemctl stop mmx-zfs-holder 2>/dev/null || true
rm -f /etc/modprobe.d/mmx-test-no-zfs.conf
if command -v zpool >/dev/null; then zpool destroy {POOL} 2>/dev/null || true; fi
rm -rf {BASE}
''')

    def remove_what_the_suite_installed():
        """Remove exactly the packages and repository keys that the suite's runs added, unload the kernel module, and remove what ZFS wrote outside its packages, so that the next suite finds the guest as this one found it."""
        added = ' '.join(sorted(set(remote(INSTALLED_PACKAGES).split()) - packages_before))
        keys = ' '.join(sorted(set(remote(IMPORTED_KEYS).split()) - keys_before))
        remote('systemctl stop zfs-zed.service 2>/dev/null || true\nif lsmod | grep -q "^zfs "; then modprobe --remove zfs || true; fi')
        if added:
            remote(f"""if command -v apt-get >/dev/null; then DEBIAN_FRONTEND=noninteractive apt-get purge -y -q {added} >/dev/null 2>&1 || true
else dnf remove -y -q {added} >/dev/null 2>&1 || true
fi""")
        if keys:
            remote(f'rpm --erase {keys}')
        remote(''.join(f'rm -rf {path}\n' for path in LEFT_BY_ZFS if path not in paths_before) + 'systemctl daemon-reload\nsystemctl reset-failed 2>/dev/null || true')

    def with_zfs():
        """The whole test, on a distribution where the suite installs ZFS."""
        # ZFS from the page's "Set up" tasks.
        vm.write_settings(workspace, {'project_name': 'zfs_datasets', 'default_inventory': 'example'})
        (workspace / 'playbooks').mkdir(parents=True, exist_ok=True)
        (workspace / 'playbooks/set_up_zfs.yml').write_text(set_up_playbook())
        started = time.monotonic()
        how = 'install'
        first = vm.cmd([mmx, 'run', 'set_up_zfs', '--json'], cwd=workspace, check=False, timeout=1500)
        if first.returncode == 1 and distribution in ('rocky', 'almalinux') and NO_KMOD_BUILD_ERROR.search(first.stdout):
            # As the page says: zfs-kmod has no build for this kernel, so the
            # page's tasks for that case build the module on the node.
            print(f"{label}: zfs-kmod has no build for kernel {remote('uname -r')}; the page's tasks for that case build the module on the node", flush=True)
            how = "for a kernel that zfs-kmod has no build for, which build the module on the node, install"
            (workspace / 'playbooks/set_up_zfs.yml').write_text(set_up_playbook(build_on_the_node=True))
            run('run', playbook='set_up_zfs', timeout=1500)
        else:
            assert first.returncode == 0, (first.returncode, first.stdout, first.stderr)
        seconds = time.monotonic() - started
        assert run('run', playbook='set_up_zfs')['entries'][0]['response']['change'] == 'unchanged'
        zfs_version = remote('zfs version').splitlines()
        release = tuple(int(part) for part in zfs_version[0].removeprefix('zfs-').split('-')[0].split('.')[:2])
        print(f"{label}: {' with '.join(zfs_version)}; Secure Boot: {remote('mokutil --sb-state 2>&1 | head -n 1 || true')}", flush=True)
        mark(f'the node tasks of the page\'s "Set up" {how} ZFS {release[0]}.{release[1]} with its kernel module in {seconds:.0f} s and run unchanged the second time')

        # No pool yet: an earlier node task may create it.
        no_pool = f'zfs_dataset: the ZFS pool {POOL} does not exist; create it in an earlier node task'
        is_result(one(dataset('clickhouse'), 'dryrun', 3)[0], 'not_checked', 'unknown', reason=no_pool)
        assert is_result(one(dataset('clickhouse'), 'run', 1)[0], 'failed', 'unchanged', reason=no_pool, tries=1)['failure_kind'] == 'missing_prerequisite'
        # The pool, from a guarded shell task as the page shows it; here it
        # is a file, where a server gives whole disks.
        remote(f'mkdir -p {BASE}\ntruncate -s 256M {BASE}/pool.img')
        pool = {'shell': {'script': f'zpool create -o cachefile=none -m {BASE}/pool {POOL} {BASE}/pool.img', 'skip_if_this_command_succeeds': f'zpool list {POOL}'}}
        first = {'compression': 'lz4', 'recordsize': '128K', 'quota': '64M', 'com.example:owner': 'team storage'}
        described = f'the dataset {POOL}/clickhouse with com.example:owner=team storage, compression=lz4, quota=64M, recordsize=128K'
        project([pool, dataset('clickhouse', first)])
        previewed = results(run('dryrun', 3))
        assert previewed[1]['status'] == 'not_checked' and no_pool in previewed[1]['reason'], previewed
        created = results(run('run'))
        assert created[0]['change'] == 'changed' and is_result(created[1], 'succeeded', 'changed')['reason'] == 'created ' + described, created
        assert created[1]['module_outputs'] == {'created': True}, created[1]
        assert [value('clickhouse', name) for name in ('compression', 'recordsize', 'quota', 'com.example:owner')] == ['lz4', '131072', '67108864', 'team storage']
        assert remote(f'findmnt -n -o FSTYPE --mountpoint {BASE}/pool/clickhouse') == 'zfs'
        for command in ('run', 'dryrun'):
            again = results(run(command, 0))
            assert [r['change'] for r in again] == ['unchanged', 'unchanged'] and again[1]['module_outputs'] == {'created': False}, again
        # The same sizes in other forms are the same values.
        is_result(one(dataset('clickhouse', {'recordsize': '131072', 'quota': '64MiB'}), 'run', 0)[0], 'succeeded', 'unchanged')
        is_result(one(dataset('clickhouse', {'recordsize': '128k', 'quota': '0.0625G'}), 'dryrun', 0)[0], 'succeeded', 'unchanged')
        mark('without the pool a dryrun is not checked and a run fails once; a guarded shell task creates the pool; a run creates the dataset with its properties, '
             'which zfs get shows, and mounts it; a rerun and a dryrun change nothing, also with the sizes written in other forms')

        # A dataset below another one needs its parent.
        no_parent = f'zfs_dataset: the parent dataset {POOL}/missing does not exist; create it in an earlier node task with zfs_dataset'
        is_result(one(dataset('missing/child'), 'dryrun', 3)[0], 'not_checked', 'unknown', reason=no_parent)
        assert is_result(one(dataset('missing/child'), 'run', 1)[0], 'failed', 'unchanged', reason=no_parent, tries=1)['failure_kind'] == 'missing_prerequisite'
        previewed = is_result(one(dataset('clickhouse/logs', {'compression': 'zstd'}), 'dryrun')[0], 'succeeded', 'would_change', reason=f'would create the dataset {POOL}/clickhouse/logs with compression=zstd')
        assert previewed['module_outputs'] == {'created': True} and not exists('clickhouse/logs'), previewed
        is_result(one(dataset('clickhouse/logs', {'compression': 'zstd'}), 'run')[0], 'succeeded', 'changed', reason=f'created the dataset {POOL}/clickhouse/logs with compression=zstd')
        assert value('clickhouse/logs', 'compression') == 'zstd'
        # The playbook of the page: the parent's task comes first. In its
        # dryrun the parent does not exist yet, so the dryrun does not check
        # the child's task.
        parent_first = [{'id': 'parent', 'zfs_dataset': {'name': f'{POOL}/tree'}}, {'id': 'child', 'zfs_dataset': {'name': f'{POOL}/tree/leaf', 'properties': {'compression': 'zstd'}}}]
        previewed = one(parent_first, 'dryrun', 3)
        assert previewed[0]['change'] == 'would_change' and previewed[1]['status'] == 'not_checked' and not exists('tree'), previewed
        assert [r['change'] for r in one(parent_first, 'run')] == ['changed', 'changed'] and exists('tree/leaf')
        mark('a dataset below a missing parent leaves a dryrun not checked and fails a run once; below its parent, a dryrun says what it would create and a run creates it; '
             'a playbook with the parent\'s task first creates both, and its dryrun leaves the child\'s task not checked')

        # Only what differs is set, and a property that the task does not list stays.
        second = {'compression': 'zstd', 'quota': '128M', 'atime': 'off', 'com.example:owner': 'storage team', 'recordsize': '128K'}
        changes = (f'set atime of the dataset {POOL}/clickhouse from on to off; %s com.example:owner of the dataset {POOL}/clickhouse from team storage to storage team; '
                   f'%s compression of the dataset {POOL}/clickhouse from lz4 to zstd; %s quota of the dataset {POOL}/clickhouse from 64M to 128M')
        previewed = is_result(one(dataset('clickhouse', second), 'dryrun')[0], 'succeeded', 'would_change')
        assert previewed['reason'] == 'would ' + changes % (('would set',) * 3), previewed
        assert value('clickhouse', 'compression') == 'lz4' and value('clickhouse', 'quota') == '67108864'
        changed = is_result(one(dataset('clickhouse', second), 'run')[0], 'succeeded', 'changed')
        assert changed['reason'] == changes % (('set',) * 3) and changed['module_outputs'] == {'created': False}, changed
        assert [value('clickhouse', name) for name in ('compression', 'recordsize', 'quota', 'atime', 'com.example:owner')] == ['zstd', '131072', '134217728', 'off', 'storage team']
        for command in ('run', 'dryrun'):
            is_result(one(dataset('clickhouse', second), command, 0)[0], 'succeeded', 'unchanged')
        # none is how a person writes "no quota"; zfs get -p prints 0 for it.
        is_result(one(dataset('clickhouse', {'quota': 'none'}), 'run')[0], 'succeeded', 'changed', reason=f'set quota of the dataset {POOL}/clickhouse from 128M to none')
        assert value('clickhouse', 'quota') == '0' and value('clickhouse', 'atime') == 'off'
        is_result(one(dataset('clickhouse', {'quota': 'none'}), 'run', 0)[0], 'succeeded', 'unchanged')
        mark('each listed property whose value differs is set, with its old and its new value in the reason, and the others stay; a dryrun before changes nothing; '
             'a value that ZFS shows only in its written form, none, is unchanged on the next run')

        # ZFS stores gzip-6 as gzip, which the module learns only after the
        # change: the task fails without another try, because the next try
        # would set the value again, and names the form. The result stays an
        # ordinary failure with its change and its output.
        stored = is_result(one(dataset('clickhouse', {'compression': 'gzip-6'}), 'run', 1)[0], 'failed', 'changed', tries=1,
                           reason=f'zfs_dataset: ZFS took the value gzip-6 for the property compression of the dataset {POOL}/clickhouse and stores it as gzip. Write compression: "gzip" in the task')
        assert 'failure_kind' not in stored and stored['module_outputs'] == {'created': False} and value('clickhouse', 'compression') == 'gzip', stored
        is_result(one(dataset('clickhouse', {'compression': 'gzip'}), 'run', 0)[0], 'succeeded', 'unchanged')
        # The same for a new dataset: it exists, and the result says that the task created it.
        stored = is_result(one(dataset('stored', {'compression': 'gzip-6'}), 'run', 1)[0], 'failed', 'changed', tries=1,
                           reason=f'stores it as gzip. Write compression: "gzip" in the task; as written, the task would set the property again on every run. The dataset {POOL}/stored was created')
        assert 'failure_kind' not in stored and stored['module_outputs'] == {'created': True} and exists('stored'), stored
        is_result(one(dataset('stored', {'compression': 'gzip'}), 'run', 0)[0], 'succeeded', 'unchanged')
        # A property's second name, such as compress: zfs get prints the
        # long name, and the task must write that one.
        refused(dataset('clickhouse', {'atime': 'off', 'compress': 'lz4'}),
                'zfs_dataset.properties: compress is a second name of the ZFS property compression; write compression in the task, which is the name that zfs get prints')
        refused(dataset('new', {'recsize': '16K'}), 'zfs_dataset.properties: recsize is a second name of the ZFS property recordsize; write recordsize in the task')
        assert value('clickhouse', 'compression') == 'gzip' and not exists('new')
        # A name that ZFS does not know, for an existing and for a new dataset.
        refused(dataset('clickhouse', {'atime': 'on', 'nosuchproperty': '1'}), f'zfs_dataset.properties: ZFS on this node knows no property nosuchproperty; its command zfs get all {POOL}/clickhouse lists')
        refused(dataset('new', {'compression': 'lz4', 'nosuchproperty': '1'}), f'zfs_dataset.properties: ZFS on this node knows no property nosuchproperty; its command zfs get all {POOL} lists')
        assert value('clickhouse', 'atime') == 'off' and not exists('new')
        # A property that is set only at creation, and a value that ZFS
        # refuses: ZFS's own message, and no property of the task is set.
        read_only = is_result(one(dataset('clickhouse', {'casesensitivity': 'insensitive', 'atime': 'on'}), 'run', 1)[0], 'failed', 'changed',
                              reason=f"zfs_dataset: setting atime=on, casesensitivity=insensitive on the dataset {POOL}/clickhouse: cannot set property for '{POOL}/clickhouse': 'casesensitivity' is readonly")
        assert 'failure_kind' not in read_only and value('clickhouse', 'atime') == 'off', read_only
        wrong = is_result(one(dataset('clickhouse', {'recordsize': '3K'}), 'run', 1)[0], 'failed', 'changed', reason=f"cannot set property for '{POOL}/clickhouse': 'recordsize' must be power of 2 from 512B to ")
        assert value('clickhouse', 'recordsize') == '131072', wrong
        mark('a value that ZFS stores in another form fails the task without another try and names that form, after a zfs set and after a zfs create, which reports created: true, '
             'and the form itself is unchanged; an unknown property name and a property\'s second name fail a dryrun and a run before anything changes, '
             'for an existing and for a new dataset; a property that is set only at creation and a refused value fail with ZFS\'s message')

        # sharenfs on a node without an NFS server: ZFS stores the value and
        # cannot share. ZFS 2.2 and 2.3's zfs create exit with an error after
        # they created the dataset; ZFS 2.4's prints the same message and
        # succeeds. zfs set succeeds with a warning.
        assert remote('command -v exportfs || true') == '', 'the guest has an NFS server, which this check needs to be missing'
        if release < (2, 4):
            shared = one(dataset('shared', {'sharenfs': 'on'}), 'run', 1)[0]
            # Not tried again, because another try would find the dataset
            # and report success; an ordinary failure that keeps its output.
            is_result(shared, 'failed', 'changed', tries=1, reason=f'zfs_dataset: ZFS created the dataset {POOL}/shared and then failed: ')
            assert 'failure_kind' not in shared and shared['module_outputs'] == {'created': True}, shared
            assert 'filesystem successfully created, but not shared. The dataset exists now, and the next run compares its properties' in shared['reason'], shared
        else:
            shared = one(dataset('shared', {'sharenfs': 'on'}), 'run')[0]
            is_result(shared, 'succeeded', 'changed', reason=f'created the dataset {POOL}/shared with sharenfs=on; ZFS warned: ')
            assert shared['module_outputs'] == {'created': True} and 'filesystem successfully created, but not shared' in shared['reason'], shared
        print(f'{label}: for sharenfs without an NFS server, ZFS {release[0]}.{release[1]}\'s zfs create '
              + ('exits with an error after it created the dataset' if release < (2, 4) else 'succeeds with a warning'), flush=True)
        assert exists('shared') and value('shared', 'sharenfs') == 'on'
        is_result(one(dataset('shared', {'sharenfs': 'on'}), 'run', 0)[0], 'succeeded', 'unchanged')
        warned = is_result(one(dataset('clickhouse', {'sharenfs': 'on'}), 'run')[0], 'succeeded', 'changed', reason=f'set sharenfs of the dataset {POOL}/clickhouse from off to on')
        assert '; ZFS warned: cannot share ' in warned['reason'] and warned['reason'].endswith('property may be set but unable to reshare filesystem'), warned
        assert value('clickhouse', 'sharenfs') == 'on'
        mark('sharenfs without an NFS server: the dataset is created and the value stored; where zfs create exits with an error after it created the dataset, '
             'the task fails without another try, says that the dataset exists, and reports created: true; where it succeeds, its warning is in the reason; '
             'the next run is unchanged; a warning of zfs set is in the reason')

        # The module manages filesystems: a volume is left alone.
        remote(f'zfs create -V 16M {POOL}/volume')
        refused(dataset('volume', {'compression': 'lz4'}), f'zfs_dataset: {POOL}/volume is a volume; the module manages filesystem datasets and leaves this one alone')
        refused(dataset('volume', state='deleted_with_all_its_data'), f'zfs_dataset: {POOL}/volume is a volume')
        refused(dataset('clickhouse@monday'), f'zfs_dataset.name: {POOL}/clickhouse@monday names a snapshot')
        assert remote(f'zfs list -H -o type {POOL}/volume') == 'volume'
        remote(f'zfs destroy {POOL}/volume')

        # Removal: never with what is below the dataset, and not while in use.
        remote(f'zfs snapshot {POOL}/clickhouse@monday\necho kept > {BASE}/pool/clickhouse/file')
        gone = dataset('clickhouse', state='deleted_with_all_its_data')
        # An earlier node task may delete what is below the dataset, so a
        # dryrun does not check this task, and a run fails once.
        waits_for_an_earlier_task(gone, f'zfs_dataset: the dataset {POOL}/clickhouse has child datasets or snapshots, so it stays: ')
        below = one(gone, 'dryrun', 3)[0]['reason']
        assert f'{POOL}/clickhouse/logs (filesystem)' in below and f'{POOL}/clickhouse@monday (snapshot)' in below, below
        assert exists('clickhouse/logs') and remote(f'cat {BASE}/pool/clickhouse/file') == 'kept'
        remote(f'zfs destroy {POOL}/clickhouse@monday')
        # A playbook deletes the child dataset first. In its dryrun the child
        # is still there, so the dryrun does not check the parent's task.
        child_gone = {'id': 'child', 'zfs_dataset': {'name': f'{POOL}/clickhouse/logs', 'state': 'deleted_with_all_its_data'}}
        previewed = one([child_gone, gone], 'dryrun', 3)
        assert previewed[0]['change'] == 'would_change' and previewed[1]['status'] == 'not_checked' and exists('clickhouse/logs'), previewed
        assert one(child_gone, 'run')[0]['reason'] == f'deleted the dataset {POOL}/clickhouse/logs with all its data'
        # A transient unit keeps a process in the dataset's directory.
        remote(f'systemd-run --quiet --unit=mmx-zfs-holder --property=WorkingDirectory={BASE}/pool/clickhouse sleep 300')
        busy = is_result(one(gone, 'run', 1)[0], 'failed', 'changed', reason=f'zfs_dataset: deleting the dataset {POOL}/clickhouse: ')
        assert 'busy' in busy['reason'] and 'failure_kind' not in busy and exists('clickhouse') and remote(f'cat {BASE}/pool/clickhouse/file') == 'kept', busy
        remote('systemctl stop mmx-zfs-holder')
        is_result(one(gone, 'dryrun')[0], 'succeeded', 'would_change', reason=f'would delete the dataset {POOL}/clickhouse with all its data')
        assert exists('clickhouse')
        deleted = is_result(one(gone, 'run')[0], 'succeeded', 'changed', reason=f'deleted the dataset {POOL}/clickhouse with all its data')
        assert deleted['module_outputs'] == {'created': False} and not exists('clickhouse'), deleted
        for command in ('run', 'dryrun'):
            is_result(one(gone, command, 0)[0], 'succeeded', 'unchanged')
        mark('a volume and a snapshot name are left alone; a dataset with a child dataset and a snapshot stays, and the task names both: a dryrun does not check it, '
             'and a run fails once; the dryrun of a playbook that deletes the child first leaves the parent\'s task not checked; '
             'a dataset in use fails with ZFS\'s message and keeps its files; then the dataset is deleted, and what is missing is unchanged')

        # ZFS's programs without its kernel module, as after a kernel update
        # for which the module was not built: the kernel is told to refuse
        # the module. A dryrun cannot check the task, and a run fails with
        # ZFS's message.
        remote(f'zpool destroy {POOL}\nsystemctl stop zfs-zed.service 2>/dev/null || true\nmodprobe --remove zfs\n'
               'echo "install zfs /bin/false" > /etc/modprobe.d/mmx-test-no-zfs.conf')
        not_loaded = "zfs_dataset: ZFS's kernel module is not loaded on this node, and the zfs program could not load it: "
        is_result(one(dataset('clickhouse'), 'dryrun', 3)[0], 'not_checked', 'unknown', reason=not_loaded)
        unloaded = is_result(one(dataset('clickhouse'), 'run', 1)[0], 'failed', 'unchanged', reason=not_loaded)
        assert 'failure_kind' not in unloaded and remote('lsmod | grep -c "^zfs " || true') == '0', unloaded
        remote('rm /etc/modprobe.d/mmx-test-no-zfs.conf')
        mark('while the kernel cannot load ZFS\'s module, a dryrun reports the task not checked and a run fails with ZFS\'s message')

    distribution, version = remote('. /etc/os-release; echo "$ID $VERSION_ID"').split()
    label = f'{distribution} {version}'
    in_a_container = remote('systemd-detect-virt --container || true') not in ('', 'none')
    # OpenZFS publishes its Enterprise Linux packages only for x86_64.
    no_packages_for_arm64 = distribution in ('rocky', 'almalinux') and vm.architecture() == 'arm64'
    whole_test = version.split('.')[0] in INSTALLS_ZFS.get(distribution, ()) and not in_a_container and not no_packages_for_arm64
    assert remote('command -v zfs zpool || true') == '', 'the guest already has ZFS'
    cleanup()
    packages_before = set(remote(INSTALLED_PACKAGES).split())
    keys_before = set(remote(IMPORTED_KEYS).split())
    paths_before = remote(f"ls -d {' '.join(LEFT_BY_ZFS)} 2>/dev/null || true").split()
    try:
        # Without ZFS's programs: a dryrun cannot check the task, and a run
        # fails once and names the program and its package.
        for task in (dataset('clickhouse', {'compression': 'lz4'}), dataset('clickhouse', state='deleted_with_all_its_data')):
            is_result(one(task, 'dryrun', 3)[0], 'not_checked', 'unknown', reason=WITHOUT_ZFS)
            failed = is_result(one(task, 'run', 1)[0], 'failed', 'unchanged', reason=WITHOUT_ZFS, tries=1)
            assert failed['failure_kind'] == 'missing_prerequisite', failed
        mark('without ZFS\'s programs, a dryrun reports the task not checked, and a run fails once as a missing prerequisite that names the program and the package of each family of distributions')
        if whole_test:
            with_zfs()
        else:
            print(f"{label}: the suite installs no ZFS here"
                  + (', in a container, which cannot load a kernel module' if in_a_container else '')
                  + (' on arm64, for which OpenZFS publishes no Enterprise Linux packages' if no_packages_for_arm64 else '') + '; the whole test runs on '
                  + ', '.join(f"{name} {' and '.join(versions)}" for name, versions in INSTALLS_ZFS.items()), flush=True)
    finally:
        failing = sys.exc_info()[0] is not None
        cleanup()
        remove_what_the_suite_installed()
        if not failing:
            left = remote(f"command -v zfs zpool || true\nlsmod | grep '^zfs ' || true\nls -d {BASE} {' '.join(path for path in LEFT_BY_ZFS if path not in paths_before)} 2>/dev/null || true")
            packages = set(remote(INSTALLED_PACKAGES).split())
            if packages != packages_before:
                left += f'\npackages added: {sorted(packages - packages_before)}; packages gone: {sorted(packages_before - packages)}'
            keys = set(remote(IMPORTED_KEYS).split())
            if keys != keys_before:
                left += f'\nrepository keys added: {sorted(keys - keys_before)}'
            state = vm.guest('sudo', '-n', 'systemctl', 'is-system-running', check=False).stdout.strip()
            if state != 'running':
                left += f'\nsystemd is {state}:\n' + vm.guest('sudo', '-n', '/bin/sh', '-c', vm.FAILED_UNITS, check=False).stdout
            assert left.strip() == '', f'the suite left this on the guest:\n{left}'
            print('The guest is as the suite found it: no ZFS program, kernel module, pool, or package of the suite, and systemd is running', flush=True)
    (workspace / 'summary.json').write_text(json.dumps(passed, indent=2) + '\n')
    print(f"{len(passed)} ZFS workflows passed on {label}" + ('' if whole_test else ', which is the test without the tool'), flush=True)


if __name__ == '__main__':
    vm.run_suite(main)
