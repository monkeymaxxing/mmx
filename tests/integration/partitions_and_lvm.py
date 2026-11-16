#!/usr/bin/env python3
"""create_partitions_on_empty_disk, grow_partition_into_free_space_after_it, lvm_volume_group, lvm_logical_volume, and grow_filesystem_to_fill_device through the public CLI, on loop devices made from files inside the node.

The suite runs the whole test on every target distribution: sfdisk is on each of them, and the suite installs lvm2 with the node tasks that the modules' pages show under "Set up". It needs the node's own kernel, so it runs in a virtual machine: a container cannot make loop devices with partitions or logical volumes. The node's state is checked with sfdisk, wipefs, blkid, readlink, vgs, pvs, lvs, findmnt, and df, never through MMX."""
import json
import shlex
import shutil
import sys
import time

import vm

BASE = '/var/tmp/mmx-partitions-and-lvm'
MOUNT_POINT = '/srv/mmx-data-disk-with-lvm'
SAVED_FSTAB = '/root/mmx-partitions-and-lvm-fstab.original'
EXAMPLE = 'data-disk-with-lvm'
MIB = 1024 * 1024
TYPES = {'linux_filesystem': '0FC63DAF-8483-4772-8E79-3D69D8477DE4', 'linux_lvm': 'E6D6D379-F507-44C2-A23C-238F2A3DF928',
         'linux_swap': '0657FD6D-A4AB-43C4-84E5-0933C84B4F4F', 'linux_raid': 'A19D880F-05FC-4D3B-A006-743F0F84911E',
         'efi_system': 'C12A7328-F81F-11D2-BA4B-00A0C93EC93B'}
# The names of the installed packages; the keys that dnf imports are no
# packages of the distribution.
INSTALLED_PACKAGES = ("if command -v dpkg-query >/dev/null; then dpkg-query -W -f '${db:Status-Abbrev} ${Package}\\n' | awk '$1 == \"ii\" {print $2}'; "
                      "else rpm -qa --qf '%{NAME}\\n' | grep -v '^gpg-pubkey$'; fi | sort -u")
# What LVM writes outside its package: a backup and an archive of each volume
# group's metadata, and on the Red Hat family its devices file.
LEFT_BY_LVM = ['/etc/lvm/backup', '/etc/lvm/archive', '/etc/lvm/devices', '/etc/lvm', '/run/lvm', '/run/lock/lvm']


def set_up_playbook(*modules):
    """A playbook of the node tasks that each module's page shows under "Set up", so that the pages are what the suite runs."""
    tasks = ''
    for module in modules:
        page = (vm.REPO / 'official_extra_modules' / module / 'README.md').read_text()
        section = page.split('\n## Set up\n', 1)[1].split('\n## ', 1)[0]
        blocks = [block.split('```', 1)[0] for block in section.split('```yaml\n')[1:]]
        assert blocks and all(block.startswith('- ') for block in blocks), (module, blocks)
        tasks += ''.join('      ' + line if line.strip() else line for block in blocks for line in block.splitlines(keepends=True))
    return 'fleet_stages:\n  - id: set_up\n    nodes: all\n    node_tasks:\n' + tasks


def main():
    vm.up()
    node, ssh = vm.connection()
    vm.reset()
    workspace = vm.ROOT / 'partitions-and-lvm'
    if workspace.exists():
        shutil.rmtree(workspace)
    vm.write_inventory(workspace, {'nodes': {'storage': node}}, name='example')
    vm.build()
    mmx = vm.ROOT / 'bin/mmx'
    passed = []
    marked = time.monotonic()

    def remote(script, check=True):
        return vm.cmd([*ssh, 'sudo -n /bin/sh -c ' + shlex.quote('set -eu\nPATH=/usr/sbin:/sbin:$PATH\n' + script)], check=check).stdout.strip()

    def run(command, expected=None, directory=None, playbook='configure', extra=()):
        result = vm.cmd([mmx, command, *vm.playbook_args(command, playbook), '--json', *extra], cwd=directory or workspace, check=False, timeout=600)
        codes = vm.success_codes(command) if expected is None else (expected,)
        assert result.returncode in codes, (command, result.returncode, result.stdout, result.stderr)
        return json.loads(result.stdout)

    def project(tasks):
        vm.write_project(workspace, project_name='partitions_and_lvm', node_task_defaults={'retry': {'pause_between_tries': '0s'}},
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

    def loop_device(name, mib, options='--partscan', more_bytes=0):
        """A loop device on a new sparse file of the suite, of mib MiB and more_bytes bytes."""
        return remote(f'truncate -s {mib * MIB + more_bytes} {BASE}/{name}.img\nlosetup --find --show {options} {BASE}/{name}.img')

    def detach(device):
        remote(f'losetup -d {device}\nudevadm settle')

    def table(device):
        """The partitions of the device as sfdisk reads them: (name, first sector, sectors, type GUID), or None without a partition table."""
        printed = remote(f'sfdisk --json {device} 2>/dev/null || true')
        if not printed:
            return None
        read = json.loads(printed)['partitiontable']
        assert read['label'] == 'gpt', read
        return [(p.get('name'), p['start'], p['size'], p['type']) for p in read.get('partitions', [])]

    def signatures(device):
        return [s['type'] for s in json.loads(remote(f'wipefs --no-act --json {device}'))['signatures']]

    def first_mib_sum(device):
        return remote(f'head -c 1048576 {device} | sha256sum').split()[0]

    def link(name):
        """Where /dev/disk/by-partlabel/<name> leads, or ''."""
        return remote(f'readlink -e /dev/disk/by-partlabel/{name} 2>/dev/null || true')

    def partitions(device, listed):
        return {'create_partitions_on_empty_disk': {'device': device, 'partitions': listed}}

    def volume_groups():
        """Each volume group's name with its free bytes, as vgs lists them."""
        return {name: int(free) for name, free in (line.split() for line in remote('vgs --noheadings --units b --nosuffix -o vg_name,vg_free').splitlines())}

    def physical_volumes():
        """Each physical volume's device with its volume group ('' for none) and the bytes of logical volumes it holds."""
        rows = (line.split('|') for line in remote("pvs --noheadings --units b --nosuffix --separator '|' -o pv_name,vg_name,pv_used").splitlines())
        return {device.strip(): (group.strip(), int(used)) for device, group, used in rows}

    def logical_volumes(group):
        """Each logical volume of the group with its bytes."""
        return {name: int(size) for name, size in (line.split() for line in remote(f'lvs --noheadings --units b --nosuffix -o lv_name,lv_size {group}').splitlines())}

    def filesystem_bytes(mount_point):
        return int(remote(f'df --block-size=1 --output=size {mount_point} | tail -n 1'))

    def volume_group(name, devices, not_listed, **more):
        return {'id': 'group', 'lvm_volume_group': {'grow_physical_volumes_with_their_devices': True, 'name': name, 'physical_volumes': devices, 'physical_volumes_not_listed': not_listed, **more}}

    def volume(name, size=None, group='mmxdata', **more):
        return {'id': 'volume', 'lvm_logical_volume': {'name': name, 'volume_group': group,
                **({'grow_filesystem_with_volume': True} if more.get('state', 'present') == 'present' else {}),
                **({'size': size} if size else {}), **more}}

    def cleanup():
        remote(f'''
for path in {MOUNT_POINT} {BASE}/grown-partition {BASE}/mounted {BASE}/read-only {BASE}/xfs {BASE}/held {BASE}/grow-ext4 {BASE}/grow-xfs; do while mountpoint -q $path; do umount -l $path; done; done
if command -v dmsetup >/dev/null; then for holder in mmx-holder mmx-lvm-holder; do dmsetup remove $holder 2>/dev/null || true; done; fi
if [ -e {SAVED_FSTAB} ]; then cp {SAVED_FSTAB} /etc/fstab; rm {SAVED_FSTAB}; systemctl daemon-reload; fi
if command -v vgs >/dev/null; then
  for group in $(vgs --noheadings -o vg_name 2>/dev/null | grep mmx || true); do vgremove --yes --force $group >/dev/null 2>&1 || true; done
fi
for image in {BASE}/*.img; do
  [ -e "$image" ] || continue
  for loop in $(losetup -j $image -O NAME -n 2>/dev/null); do partx --delete $loop 2>/dev/null || true; losetup -d $loop; done
done
udevadm settle
rm -rf {BASE}
rmdir {MOUNT_POINT} 2>/dev/null || true
''')

    def remove_what_the_suite_installed():
        """Remove exactly the packages that the suite's runs added, and what LVM wrote outside its package, so that the next suite finds the guest as this one found it. Returns the last lines that the package manager printed, for the message about a package that stayed."""
        added = ' '.join(sorted(set(remote(INSTALLED_PACKAGES).split()) - packages_before))
        said = ''
        if added:
            # apt waits for another apt's lock, such as the daily update's
            # after a guest's start, and does not fail at once.
            said = remote(f"""if command -v apt-get >/dev/null; then DEBIAN_FRONTEND=noninteractive apt-get -o DPkg::Lock::Timeout=120 purge -y -q {added} 2>&1 | tail -n 12 || true
else dnf remove -y -q {added} 2>&1 | tail -n 12 || true
fi""")
        remote('rm -rf /etc/lvm/backup/mmx* /etc/lvm/archive/mmx*\n'
               + ''.join(f'rm -rf {path}\n' for path in LEFT_BY_LVM if path not in paths_before)
               + 'systemctl daemon-reload\nsystemctl reset-failed 2>/dev/null || true')
        return said

    distribution = remote('. /etc/os-release; echo "$ID"')
    cleanup()
    remote(f'mkdir -p {BASE}\ncp /etc/fstab {SAVED_FSTAB}')
    packages_before = set(remote(INSTALLED_PACKAGES).split())
    paths_before = remote(f"ls -d {' '.join(LEFT_BY_LVM)} 2>/dev/null || true").split()
    print(f"{distribution}: {remote('sfdisk --version')}", flush=True)
    try:
        # --- create_partitions_on_empty_disk ---
        disk = loop_device('disk', 256)
        two = [{'name': 'mmx-database', 'size': '64MiB'}, {'name': 'mmx-backups', 'size': 'rest_of_disk', 'type': 'linux_lvm'}]
        expected = [('mmx-database', 2048, 131072, TYPES['linux_filesystem']), ('mmx-backups', 133120, 389120, TYPES['linux_lvm'])]
        described = f'a GPT partition table on {disk} with the partitions: mmx-database (64 MiB), mmx-backups (190 MiB, the rest of the disk)'

        # The page's "Set up" tasks: sfdisk's package where the node lacks it.
        vm.write_settings(workspace, {'project_name': 'partitions_and_lvm', 'default_inventory': 'example'})
        (workspace / 'playbooks').mkdir(parents=True, exist_ok=True)
        (workspace / 'playbooks/set_up_partitions.yml').write_text(set_up_playbook('create_partitions_on_empty_disk', 'grow_partition_into_free_space_after_it'))
        run('run', playbook='set_up_partitions')
        assert run('run', playbook='set_up_partitions')['entries'][0]['response']['change'] == 'unchanged'

        before = first_mib_sum(disk)
        previewed = is_result(one(partitions(disk, two), 'dryrun')[0], 'succeeded', 'would_change')
        assert previewed['reason'] == 'would create ' + described, previewed
        assert table(disk) is None and signatures(disk) == [] and first_mib_sum(disk) == before
        created = is_result(one(partitions(disk, two), 'run')[0], 'succeeded', 'changed')
        assert created['reason'] == 'created ' + described and created['module_outputs'] == {'created': True} and previewed['module_outputs'] == {'created': True}, (previewed, created)
        assert table(disk) == expected, table(disk)
        assert link('mmx-database') == disk + 'p1' and link('mmx-backups') == disk + 'p2', (link('mmx-database'), link('mmx-backups'))
        for command in ('run', 'dryrun'):
            assert is_result(one(partitions(disk, two), command, 0)[0], 'succeeded', 'unchanged')['module_outputs'] == {'created': False}
        mark('a dryrun says which partitions it would create and writes nothing; a run writes the GPT partition table with every sector as computed, '
             'and the links in /dev/disk/by-partlabel lead to the partitions; a rerun and a dryrun change nothing')

        # Grow the last partition of a disk that its provider enlarged, while
        # the partition holds a mounted ext4 filesystem with a file on it.
        growth_disk = loop_device('growth-disk', 300)
        growth_layout = [{'name': 'mmx-growth-first', 'size': '100MiB'},
                         {'name': 'mmx-growth-last', 'size': 'rest_of_disk'}]
        is_result(one(partitions(growth_disk, growth_layout), 'run')[0], 'succeeded', 'changed')
        growth_mount = f'{BASE}/grown-partition'
        remote(f'mkfs.ext4 -q {growth_disk}p2\nmkdir -p {growth_mount}\nmount {growth_disk}p2 {growth_mount}\n'
               f'head -c {MIB} /dev/urandom > {growth_mount}/kept-file\nsync')
        kept_file_sum = f'sha256sum {growth_mount}/kept-file'
        kept_before = remote(kept_file_sum).split()[0]
        filesystem_before_growth = filesystem_bytes(growth_mount)
        before_growth = table(growth_disk)
        old_bytes = int(remote(f'blockdev --getsize64 {growth_disk}p2'))
        remote(f'truncate -s {600 * MIB} {BASE}/growth-disk.img\nlosetup -c {growth_disk}')
        growth_task = {'grow_partition_into_free_space_after_it': {'device': growth_disk, 'partition_number': 2}}
        previewed = is_result(one(growth_task, 'dryrun')[0], 'succeeded', 'would_change',
                              reason=f'would grow partition 2 of {growth_disk} from ')
        assert table(growth_disk) == before_growth and int(remote(f'blockdev --getsize64 {growth_disk}p2')) == old_bytes
        applied = is_result(one(growth_task, 'run')[0], 'succeeded', 'changed',
                            reason=f'grew partition 2 of {growth_disk} from ')
        grown_table = table(growth_disk)
        new_bytes = int(remote(f'blockdev --getsize64 {growth_disk}p2'))
        sector_bytes = int(remote(f'blockdev --getss {growth_disk}'))
        assert grown_table[0] == before_growth[0] and grown_table[1][:2] == before_growth[1][:2]
        assert grown_table[1][3] == before_growth[1][3] and new_bytes == grown_table[1][2] * sector_bytes > old_bytes
        assert applied['reason'] == previewed['reason'].replace('would grow ', 'grew ', 1), (previewed, applied)
        # sfdisk --verify exits with 0 also for a table with errors (measured
        # on a disk that grew and whose backup header was not moved), so the
        # check reads its words.
        verified = remote(f'sfdisk --verify {growth_disk} 2>&1')
        assert 'No errors detected' in verified, verified
        # The filesystem stays mounted from the grown partition, keeps its
        # size (growing it is another node task's work), and its file reads
        # the same from the disk once the page cache is dropped.
        mounted_from = remote(f'findmnt --noheadings --output SOURCE --mountpoint {growth_mount}')
        assert mounted_from == growth_disk + 'p2', mounted_from
        assert filesystem_bytes(growth_mount) == filesystem_before_growth, (filesystem_bytes(growth_mount), filesystem_before_growth)
        kept_after = remote(f'echo 3 > /proc/sys/vm/drop_caches\n{kept_file_sum}').split()[0]
        assert kept_after == kept_before, (kept_after, kept_before)
        for command in ('run', 'dryrun'):
            is_result(one(growth_task, command, 0)[0], 'succeeded', 'unchanged')
            first_task = {'grow_partition_into_free_space_after_it': {'device': growth_disk, 'partition_number': 1}}
            is_result(one(first_task, command, 0)[0], 'succeeded', 'unchanged')
        assert table(growth_disk) == grown_table
        remote(f'umount {growth_mount}')
        detach(growth_disk)
        mark('partition growth: a dryrun changes nothing; the last partition grows after disk enlargement while its ext4 filesystem is mounted, '
             'the table and kernel sizes agree, the table verifies, the filesystem stays mounted with its size and its file unchanged, '
             'and reruns and the first partition are unchanged')

        # A disk with sectors of 4096 bytes, and every type.
        large_sectors = loop_device('large-sectors', 256, '--partscan --sector-size 4096')
        every_type = [{'name': 'mmx-efi', 'size': '16MiB', 'type': 'efi_system'}, {'name': 'mmx-swap', 'size': '16MiB', 'type': 'linux_swap'},
                      {'name': 'mmx-raid', 'size': '16MiB', 'type': 'linux_raid'}, {'name': 'mmx-lvm', 'size': '16MiB', 'type': 'linux_lvm'},
                      {'name': 'mmx.root_1', 'size': 'rest_of_disk', 'type': 'linux_filesystem'}]
        is_result(one(partitions(large_sectors, every_type), 'run')[0], 'succeeded', 'changed')
        assert table(large_sectors) == [('mmx-efi', 256, 4096, TYPES['efi_system']), ('mmx-swap', 4352, 4096, TYPES['linux_swap']),
                                        ('mmx-raid', 8448, 4096, TYPES['linux_raid']), ('mmx-lvm', 12544, 4096, TYPES['linux_lvm']),
                                        ('mmx.root_1', 16640, 48640, TYPES['linux_filesystem'])], table(large_sectors)
        assert link('mmx.root_1') == large_sectors + 'p5'
        is_result(one(partitions(large_sectors, every_type), 'run', 0)[0], 'succeeded', 'unchanged')
        detach(large_sectors)
        mark('a disk with sectors of 4096 bytes gets the same partitions, counted in its own sectors, with each of the five types')

        # A table that this node's own sfdisk wrote with a last partition
        # without a size: older versions end it at the last usable sector,
        # newer ones at the last whole MiB. The task finds its partitions.
        native = loop_device('native', 256)
        remote(f"printf 'label: gpt\\nsize=64MiB, name=mmx-native-one\\nname=mmx-native-rest\\n' | sfdisk -q {native}\nudevadm settle")
        native_rest = table(native)[1][2]
        assert native_rest in (389120, 391135), table(native)
        native_task = partitions(native, [{'name': 'mmx-native-one', 'size': '64MiB'}, {'name': 'mmx-native-rest', 'size': 'rest_of_disk'}])
        for command in ('run', 'dryrun'):
            is_result(one(native_task, command, 0)[0], 'succeeded', 'unchanged')
        print(f"{distribution}: this sfdisk ends a last partition without a size after {native_rest} sectors, "
              + ('at the last usable sector' if native_rest == 391135 else 'at the last whole MiB'), flush=True)
        # The same disk with other partitions than the task lists is never changed.
        other = partitions(native, [{'name': 'mmx-native-one', 'size': '32MiB'}, {'name': 'mmx-native-rest', 'size': 'rest_of_disk'}])
        written = table(native)
        refused(other, f'create_partitions_on_empty_disk: {native} already has a GPT partition table with other partitions than the task lists. '
                       'Found: mmx-native-one (64 MiB, linux_filesystem, from sector 2048), mmx-native-rest (')
        assert table(native) == written
        mark('a table that the node\'s own sfdisk wrote with a last partition without a size is unchanged for rest_of_disk; '
             'a disk with other partitions than the task lists fails the task and stays as it was')

        # A disk that holds anything is never written, in a dryrun and in a run.
        full = loop_device('full', 128)
        one_partition = [{'name': 'mmx-never', 'size': '64MiB'}]
        for prepare, found in [
            (f'mkfs.ext4 -q {full}', 'ext4 filesystem (ext4 signature at offset 0x438)'),
            (f'wipefs -q --all {full}\nmkswap {full} >/dev/null', 'swap area (swap signature at offset 0xff6)'),
            (f"wipefs -q --all {full}\nprintf 'label: dos\\nsize=16MiB\\n' | sfdisk -q {full}", 'MBR partition table (dos signature at offset 0x1fe)'),
        ]:
            remote(prepare)
            before = first_mib_sum(full)
            refused(partitions(full, one_partition), f'create_partitions_on_empty_disk: {full} is not empty; found: {found}. '
                    'create_partitions_on_empty_disk writes a partition table only on a disk that holds nothing, and never destroys data.')
            assert first_mib_sum(full) == before and link('mmx-never') == ''
        # A partition is no whole disk; sfdisk itself would write a table into it.
        refused(partitions(disk + 'p1', one_partition), f'create_partitions_on_empty_disk: {disk}p1 is a partition of the disk /dev/{disk.rsplit("/", 1)[1]}, not a whole disk')
        assert signatures(disk + 'p1') == []
        refused(partitions(f'{BASE}/full.img', one_partition), f'create_partitions_on_empty_disk: {BASE}/full.img is not a block device')
        remote(f'wipefs -q --all {full}')
        mark('a disk with a filesystem, with swap, and with an MBR partition table, a partition, and a file are refused in a dryrun and in a run, '
             'named with what was found, and left as they were')

        # Partitions that do not fit, and a name that another disk already has.
        refused(partitions(full, [{'name': 'mmx-never', 'size': '64MiB'}, {'name': 'mmx-too-large', 'size': '1GiB'}]),
                f'create_partitions_on_empty_disk: the partitions do not fit on {full} (128 MiB): the partition mmx-too-large (1 GiB) does not fit: '
                'the disk has room for 126 MiB of partitions, and the partitions up to this one need 1088 MiB. Nothing was written')
        refused(partitions(full, [{'name': 'mmx-never', 'size': '126MiB'}, {'name': 'mmx-nothing-left', 'size': 'rest_of_disk'}]),
                'the partition mmx-nothing-left with the size rest_of_disk needs at least 1 MiB')
        refused(partitions(full, [{'name': 'mmx-never', 'size': '32MiB'}, {'name': 'mmx-database', 'size': '32MiB'}]),
                f'create_partitions_on_empty_disk: the name mmx-database is already the name of the partition {disk}p1 on the disk /dev/{disk.rsplit("/", 1)[1]}: '
                '/dev/disk/by-partlabel/mmx-database leads there')
        assert table(full) is None and signatures(full) == [] and link('mmx-database') == disk + 'p1'
        # The largest table that fits: the last partition ends at the last usable sector's MiB.
        is_result(one(partitions(full, [{'name': 'mmx-whole', 'size': '126MiB'}]), 'run')[0], 'succeeded', 'changed')
        assert table(full) == [('mmx-whole', 2048, 258048, TYPES['linux_filesystem'])]
        remote(f'wipefs -q --all {full}\nudevadm settle')
        mark('partitions that do not fit and a name that another disk\'s partition has fail before anything is written, in a dryrun and in a run; '
             'partitions that fill the disk to its last MiB are written')

        # A disk that ends between two MiB. A GPT partition table keeps the
        # last 33 sectors of 512 bytes, or the last 5 of 4096 bytes, so on
        # these disks the last usable sector is the last one of a whole MiB:
        # the rest of the disk goes up to it, and a partition with a size
        # that ends there fits.
        for name, options, sector_bytes, kept in (('odd', '--partscan', 512, 33), ('odd-large-sectors', '--partscan --sector-size 4096', 4096, 5)):
            in_one_mib = MIB // sector_bytes
            odd = loop_device(name, 64, options, more_bytes=kept * sector_bytes)
            rest = partitions(odd, [{'name': 'mmx-odd-one', 'size': '1MiB'}, {'name': 'mmx-odd-rest', 'size': 'rest_of_disk'}])
            wrote = is_result(one(rest, 'run')[0], 'succeeded', 'changed')
            assert wrote['reason'].endswith('mmx-odd-one (1 MiB), mmx-odd-rest (62 MiB, the rest of the disk)'), wrote
            assert table(odd) == [('mmx-odd-one', in_one_mib, in_one_mib, TYPES['linux_filesystem']), ('mmx-odd-rest', 2 * in_one_mib, 62 * in_one_mib, TYPES['linux_filesystem'])], table(odd)
            # The disk has 64 MiB and the kept sectors; sfdisk gives the last sector of the 64 MiB as the last usable one.
            assert json.loads(remote(f'sfdisk --json {odd}'))['partitiontable']['lastlba'] == 64 * in_one_mib - 1
            is_result(one(rest, 'run', 0)[0], 'succeeded', 'unchanged')
            remote(f'wipefs -q --all {odd}\nudevadm settle')
            whole = partitions(odd, [{'name': 'mmx-odd-whole', 'size': '63MiB'}])
            is_result(one(whole, 'run')[0], 'succeeded', 'changed')
            assert table(odd) == [('mmx-odd-whole', in_one_mib, 63 * in_one_mib, TYPES['linux_filesystem'])], table(odd)
            remote(f'wipefs -q --all {odd}\nudevadm settle')
            refused(partitions(odd, [{'name': 'mmx-odd-whole', 'size': '64MiB'}]), 'the disk has room for 63 MiB of partitions, and the partitions up to this one need 64 MiB. Nothing was written')
            detach(odd)
        mark('on a disk that ends 33 sectors of 512 bytes, or 5 sectors of 4096 bytes, behind a whole MiB, the rest of the disk and a partition of 63 MiB end at the last usable sector, '
             'and one MiB more does not fit')

        # A device that does not exist yet, as before an earlier node task attaches the disk.
        missing = partitions('/dev/mmx-no-such-disk', one_partition)
        is_result(one(missing, 'dryrun', 3)[0], 'not_checked', 'unknown', reason='create_partitions_on_empty_disk: the device /dev/mmx-no-such-disk does not exist yet')
        absent = is_result(one(missing, 'run', 1)[0], 'failed', 'unchanged', reason='create_partitions_on_empty_disk: the device /dev/mmx-no-such-disk does not exist yet', tries=1)
        assert absent['failure_kind'] == 'missing_prerequisite', absent
        # Inputs that break a rule fail when the task runs, once, and before any command.
        wrong = is_result(one(partitions(full, [{'name': 'mmx-one', 'size': 'rest_of_disk'}, {'name': 'mmx-two', 'size': '8MiB'}]), 'run', 1)[0], 'failed', 'unchanged',
                          reason='create_partitions_on_empty_disk.partitions[0].size: only the last partition can have the size rest_of_disk', tries=1)
        assert wrong['failure_kind'] == 'config', wrong
        mark('a device that does not exist yet leaves a dryrun not checked and fails a run once as a missing prerequisite; inputs that break a rule fail once')

        # The kernel does not read the new table of a loop device that was
        # attached without --partscan: the links do not appear, and the task
        # says so after its write. It is not tried again, because the next
        # try would find the table; it stays an ordinary failure. The next
        # run and dryrun find the table and still no link, which no try
        # changes, so they fail too. Once the kernel has read the table, the
        # next run finds the partitions and changes nothing.
        detach(full)
        remote(f'rm {BASE}/full.img')
        unscanned = loop_device('unscanned', 128, '')
        missing_link = 'but /dev/disk/by-partlabel/mmx-never does not exist, so a later node task cannot find the partition'
        no_link = is_result(one(partitions(unscanned, one_partition), 'run', 1)[0], 'failed', 'changed',
                            reason=f'create_partitions_on_empty_disk: the partition table was written on {unscanned}, {missing_link}', tries=1)
        assert 'failure_kind' not in no_link and 'losetup --partscan' in no_link['reason'], no_link
        assert table(unscanned) == [('mmx-never', 2048, 131072, TYPES['linux_filesystem'])]
        refused(partitions(unscanned, one_partition), f'create_partitions_on_empty_disk: {unscanned} has the partition table that the task lists, {missing_link}')
        remote(f'partx --add {unscanned}\nudevadm settle')
        is_result(one(partitions(unscanned, one_partition), 'run', 0)[0], 'succeeded', 'unchanged')
        assert link('mmx-never') == unscanned + 'p1'
        remote(f'partx --delete {unscanned}')
        detach(unscanned)
        mark('a table that the kernel did not read fails the task after its write, without another try, and in the runs after it, naming the missing link; '
             'once the kernel has read it, the next run is unchanged')

        # A disk whose partition table someone wiped still holds the old
        # filesystem where the new partition begins. The module wipes
        # nothing, names it, and a following filesystem task finds it.
        remote(f'mkfs.ext4 -q -L mmxoldfiles {disk}p1\nwipefs -q --all {disk}\nudevadm settle')
        assert signatures(disk) == [] and table(disk) is None
        again = is_result(one(partitions(disk, two), 'run')[0], 'succeeded', 'changed')
        assert again['reason'] == ('created ' + described + '; the new partition mmx-database holds what was on the disk before: '
                                   'ext4 filesystem (ext4 signature at offset 0x438); the module wiped nothing'), again
        assert remote(f'blkid -p -o value -s LABEL {disk}p1') == 'mmxoldfiles' and table(disk) == expected
        kept = one({'create_filesystem_on_empty_device': {'device': '/dev/disk/by-partlabel/mmx-database', 'filesystem_type': 'ext4'}}, 'run', 0)[0]
        assert kept['change'] == 'unchanged', kept
        remote(f'wipefs -q --all {disk}p1')
        mark('an old filesystem behind a wiped partition table stays: the task names it in its reason and wipes nothing')

        # --- lvm_volume_group and lvm_logical_volume ---
        first, second, third = (loop_device(name, mib, '') for name, mib in (('first', 512), ('second', 256), ('third', 128)))
        group_on = ['/dev/disk/by-partlabel/mmx-backups', first]
        lvm2_was_installed = remote('command -v vgs || true') != ''
        if not lvm2_was_installed:
            # Before lvm2 is installed: a dryrun cannot check the tasks, and
            # a run fails once and names the program and its package.
            for task, program, module in ((volume_group('mmxdata', group_on, 'keep'), 'vgs', 'lvm_volume_group'), (volume('postgresql', '50MiB'), 'vgs', 'lvm_logical_volume')):
                reason = f'{module}: the program {program} is not installed; install the package lvm2 in an earlier node task'
                is_result(one(task, 'dryrun', 3)[0], 'not_checked', 'unknown', reason=reason)
                without = is_result(one(task, 'run', 1)[0], 'failed', 'unchanged', reason=reason, tries=1)
                assert without['failure_kind'] == 'missing_prerequisite', without
            mark('without lvm2, both LVM modules leave a dryrun not checked and fail a run once as a missing prerequisite that names the program and the package')
        else:
            print(f'{distribution}: lvm2 is installed in the guest as it comes, so the check without the programs is left out', flush=True)

        # lvm2 from the pages' "Set up" tasks, and the programs that make and
        # grow the filesystems of the suite.
        (workspace / 'playbooks/set_up_lvm.yml').write_text(set_up_playbook('lvm_volume_group', 'lvm_logical_volume', 'grow_filesystem_to_fill_device'))
        started = time.monotonic()
        run('run', playbook='set_up_lvm')
        assert run('run', playbook='set_up_lvm')['entries'][0]['response']['change'] == 'unchanged'
        print(f"{distribution}: {' '.join(remote('lvm version | head -n 1').split())}", flush=True)
        mark('the node tasks of the pages\' "Set up" install lvm2' + (', which this guest had,' if lvm2_was_installed else '') + f' and run unchanged the second time; with e2fsprogs and xfsprogs in {time.monotonic() - started:.0f} s')
        has_fsmode = '--fsmode' in remote('lvextend --help')

        # A provider enlarged the device, leaving its mounted filesystem small.
        # Read the filesystem tools' own geometry, independently of MMX.
        for filesystem_type, small_mib, large_mib in (('ext4', 300, 600), ('xfs', 400, 800)):
            name = 'grow-' + filesystem_type
            device = loop_device(name, small_mib, '')
            mount_point = f'{BASE}/{name}'
            remote(f'mkfs.{filesystem_type} {device} >/dev/null\nmkdir -p {mount_point}\nmount {device} {mount_point}\necho kept > {mount_point}/file')

            def geometry_bytes():
                if filesystem_type == 'ext4':
                    lines = remote(f'dumpe2fs -h {device} 2>/dev/null').splitlines()
                    count = int(next(line.split(':', 1)[1] for line in lines if line.startswith('Block count:')))
                    block_bytes = int(next(line.split(':', 1)[1] for line in lines if line.startswith('Block size:')))
                else:
                    words = next(line for line in remote(f'xfs_info {mount_point}').splitlines() if line.startswith('data')).split()
                    count = int(next(word.split('=', 1)[1].rstrip(',') for word in words if word.startswith('blocks=')))
                    block_bytes = int(next(word.split('=', 1)[1] for word in words if word.startswith('bsize=')))
                return count * block_bytes

            before = geometry_bytes()
            assert before == small_mib * MIB, before
            remote(f'truncate -s {large_mib * MIB} {BASE}/{name}.img\nlosetup -c {device}')
            task = {'grow_filesystem_to_fill_device': {'device': device, 'filesystem_type': filesystem_type}}
            what = f'the {filesystem_type} filesystem on {device} from {small_mib} MiB to {large_mib} MiB'
            is_result(one(task, 'dryrun')[0], 'succeeded', 'would_change', reason='would grow ' + what)
            assert geometry_bytes() == before and remote(f'cat {mount_point}/file') == 'kept'
            is_result(one(task, 'run')[0], 'succeeded', 'changed', reason='grew ' + what)
            assert geometry_bytes() == large_mib * MIB and remote(f'cat {mount_point}/file') == 'kept'
            for command in ('run', 'dryrun'):
                is_result(one(task, command, 0)[0], 'succeeded', 'unchanged')
            remote(f'umount {mount_point}')
            waits_for_an_earlier_task(task, f'grow_filesystem_to_fill_device: the {filesystem_type} filesystem on {device} is not mounted; mount it for writing first with fstab_mount')
            detach(device)
            mark(f'{filesystem_type}: a dryrun leaves a mounted filesystem small; a run grows it to the enlarged device, preserves a file, and reruns unchanged; an unmounted filesystem is a missing prerequisite')

        # A disk in use that holds no signature: device-mapper holds it, as
        # for an encrypted volume without a header. The real sfdisk refuses
        # such a disk and sfdisk --no-act does not, so the module looks
        # itself, and a dryrun fails as a run does. (dmsetup came with lvm2.)
        held = loop_device('held', 128)
        held_partition = partitions(held, [{'name': 'mmx-held', 'size': '64MiB'}])
        remote(f'dmsetup create mmx-holder --table "0 $(blockdev --getsz {held}) linear {held} 0"')
        before = first_mib_sum(held)
        assert signatures(held) == []
        refused(held_partition, f'create_partitions_on_empty_disk: {held} is in use, although it holds no signature: the device /dev/mapper/mmx-holder (dm) is on it. '
                'The module writes a partition table only on a disk that nothing uses')
        assert first_mib_sum(held) == before and table(held) is None and link('mmx-held') == ''
        remote('dmsetup remove mmx-holder\nudevadm settle')
        # A partition that the kernel still has: someone wiped the partition
        # table while a filesystem of the disk was mounted, so the kernel
        # could not read the disk again. The task names the partition, and
        # after blockdev --rereadpt it writes its table.
        remote(f"printf 'label: gpt\\nsize=32MiB, name=mmx-held-old\\n' | sfdisk -q {held}\nudevadm settle\nmkfs.ext4 -q {held}p1\nmkdir -p {BASE}/held\nmount {held}p1 {BASE}/held\n"
               f'dd if=/dev/zero of={held} bs=1M count=1 conv=notrunc status=none\ndd if=/dev/zero of={held} bs=1M seek=127 count=1 conv=notrunc status=none\n'
               f'udevadm settle\numount {BASE}/held\nudevadm settle')
        assert signatures(held) == [] and remote(f'test -b {held}p1 && echo block') == 'block'
        refused(held_partition, f'create_partitions_on_empty_disk: {held} is in use, although it holds no signature: the kernel still has the partition {held}p1 of an earlier partition table. '
                f'The module writes a partition table only on a disk that nothing uses; when nothing uses a partition of an earlier partition table any more, blockdev --rereadpt {held} makes the kernel forget it')
        remote(f'blockdev --rereadpt {held}\nudevadm settle')
        is_result(one(held_partition, 'run')[0], 'succeeded', 'changed')
        assert table(held) == [('mmx-held', 2048, 131072, TYPES['linux_filesystem'])] and link('mmx-held') == held + 'p1', table(held)
        remote(f'wipefs -q --all {held}p1 {held}\nudevadm settle')
        detach(held)
        mark('a disk that device-mapper holds, and a disk of which the kernel still has a partition, are refused in a dryrun and in a run although they hold no signature, '
             'and stay as they were; after blockdev --rereadpt the task writes its table')

        # The volume group: a partition by its link, and a whole device.
        on = f'the volume group mmxdata on {group_on[0]}, {first}'
        previewed = is_result(one(volume_group('mmxdata', group_on, 'remove'), 'dryrun')[0], 'succeeded', 'would_change', reason='would create ' + on)
        assert 'mmxdata' not in volume_groups() and signatures(first) == [] and previewed['module_outputs'] == {'created': True}, previewed
        created = is_result(one(volume_group('mmxdata', group_on, 'remove'), 'run')[0], 'succeeded', 'changed', reason='created ' + on)
        assert physical_volumes() == {disk + 'p2': ('mmxdata', 0), first: ('mmxdata', 0)} and created['module_outputs'] == {'created': True}, (physical_volumes(), created)
        for command in ('run', 'dryrun'):
            assert is_result(one(volume_group('mmxdata', group_on, 'remove'), command, 0)[0], 'succeeded', 'unchanged')['module_outputs'] == {'created': False}
        mark('a dryrun says on which devices it would create the volume group and changes nothing; a run creates it on a partition named by its link and on a whole device; '
             'a rerun and a dryrun change nothing')

        # A provider grows a physical volume's device. Leave the main
        # group's sizes alone for the logical-volume cases below.
        growing = loop_device('growing', 200, '')
        growing_task = volume_group('mmxgrowing', [growing], 'keep')
        is_result(one(growing_task, 'run')[0], 'succeeded', 'changed')

        def physical_volume_bytes():
            return int(remote(f'pvs --noheadings --units b --nosuffix -o pv_size {growing}'))

        old_size = physical_volume_bytes()
        old_free = volume_groups()['mmxgrowing']
        remote(f'truncate -s {500 * MIB} {BASE}/growing.img\nlosetup -c {growing}')
        disabled = volume_group('mmxgrowing', [growing], 'keep', grow_physical_volumes_with_their_devices=False)
        is_result(one(disabled, 'run', 0)[0], 'succeeded', 'unchanged')
        assert physical_volume_bytes() == old_size and volume_groups()['mmxgrowing'] == old_free
        previewed = is_result(one(growing_task, 'dryrun')[0], 'succeeded', 'would_change',
                              reason=f'would grow the physical volume {growing} of the volume group mmxgrowing from ')
        assert physical_volume_bytes() == old_size and volume_groups()['mmxgrowing'] == old_free
        grown = is_result(one(growing_task, 'run')[0], 'succeeded', 'changed',
                          reason=f'grew the physical volume {growing} of the volume group mmxgrowing from ')
        new_size = physical_volume_bytes()
        assert new_size > old_size and volume_groups()['mmxgrowing'] - old_free == new_size - old_size
        assert f'from {old_size // MIB} MiB to {new_size // MIB} MiB' in grown['reason'], grown
        assert previewed['reason'] == grown['reason'].replace('grew ', 'would grow ', 1), (previewed, grown)
        is_result(one(growing_task, 'run', 0)[0], 'succeeded', 'unchanged')
        is_result(one({'lvm_volume_group': {'name': 'mmxgrowing', 'state': 'absent'}}, 'run')[0], 'succeeded', 'changed')
        detach(growing)
        mark('a grown device leaves its physical volume alone with growth disabled; a dryrun predicts growth and changes nothing; '
             'a run grows the physical volume and the volume group free space, and a rerun is unchanged')

        # A device that holds anything is never added, and LVM is not asked.
        remote(f'mkfs.ext4 -q {second}')
        before = first_mib_sum(second)
        refused(volume_group('mmxdata', group_on + [second], 'keep'),
                f'lvm_volume_group: the device {second} is not empty; found: ext4 filesystem (ext4 signature at offset 0x438). '
                'lvm_volume_group adds only a device that holds nothing or an unused LVM physical volume, and never wipes a device.')
        assert first_mib_sum(second) == before and second not in physical_volumes()
        remote(f'wipefs -q --all {second}\nvgcreate -q mmxother {third} >/dev/null')
        # An earlier node task may take the device out of the other volume group, so a dryrun does not check this task.
        waits_for_an_earlier_task(volume_group('mmxdata', group_on + [third], 'keep'), f'lvm_volume_group: the device {third} belongs to the volume group mmxother; a device is in one volume group only')
        assert physical_volumes()[third] == ('mmxother', 0)
        refused(volume_group('mmxdata', [group_on[0], disk + 'p2'], 'keep'), f'lvm_volume_group.physical_volumes: physical_volumes[0] and physical_volumes[1] lead to the same device, {disk}p2')
        missing = volume_group('mmxdata', group_on + ['/dev/disk/by-partlabel/mmx-no-such-partition'], 'keep')
        is_result(one(missing, 'dryrun', 3)[0], 'not_checked', 'unknown', reason='lvm_volume_group: the device /dev/disk/by-partlabel/mmx-no-such-partition does not exist yet')
        assert is_result(one(missing, 'run', 1)[0], 'failed', 'unchanged', tries=1)['failure_kind'] == 'missing_prerequisite'
        # A device that device-mapper holds and that holds no signature: LVM
        # itself refuses it, because it opens the device for itself alone.
        # The task fails with LVM's words, and the device stays as it was.
        held = loop_device('held-for-lvm', 64, '')
        remote(f'dmsetup create mmx-lvm-holder --table "0 $(blockdev --getsz {held}) linear {held} 0"')
        before = first_mib_sum(held)
        by_lvm = is_result(one(volume_group('mmxdata', group_on + [held], 'keep'), 'run', 1)[0], 'failed', 'changed', reason=f'lvm_volume_group: adding {held} to the volume group mmxdata: ')
        assert f"Can't open {held} exclusively" in by_lvm['reason'] and 'failure_kind' not in by_lvm, by_lvm
        assert held not in physical_volumes() and first_mib_sum(held) == before and signatures(held) == []
        remote('dmsetup remove mmx-lvm-holder')
        detach(held)
        mark('a device with a filesystem and one device under two paths are refused in a dryrun and in a run and stay as they were; a device of another volume group '
             'and a device that does not exist yet leave a dryrun not checked and fail a run once; a device that device-mapper holds is refused by LVM itself and stays as it was')

        # A logical volume: LVM rounds 50 MiB up to whole extents of 4 MiB.
        described = 'the logical volume mmxdata/postgresql with 52 MiB, which is 50 MiB rounded up to whole extents of 4 MiB'
        previewed = is_result(one(volume('postgresql', '50MiB'), 'dryrun')[0], 'succeeded', 'would_change', reason='would create ' + described)
        assert previewed['module_outputs'] == {'created': True, 'device': '/dev/mmxdata/postgresql'} and logical_volumes('mmxdata') == {}, previewed
        created = is_result(one(volume('postgresql', '50MiB'), 'run')[0], 'succeeded', 'changed', reason='created ' + described)
        assert created['module_outputs'] == {'created': True, 'device': '/dev/mmxdata/postgresql'}, created
        assert logical_volumes('mmxdata') == {'postgresql': 52 * MIB} and remote('test -b /dev/mmxdata/postgresql && echo block') == 'block'
        for command in ('run', 'dryrun'):
            again = is_result(one(volume('postgresql', '50MiB'), command, 0)[0], 'succeeded', 'unchanged')
            assert again['module_outputs'] == {'created': False, 'device': '/dev/mmxdata/postgresql'}, again
        # The same size written in whole extents is the same volume.
        is_result(one(volume('postgresql', '52MiB'), 'run', 0)[0], 'succeeded', 'unchanged')
        mark('a dryrun says with which size it would create the logical volume; a run creates it with the size rounded up to whole extents and gives its device; '
             'a rerun and a dryrun change nothing')

        # Devices join and leave the volume group. The volume's data is on
        # one device, which cannot leave; the others can, and lose LVM's label.
        added = is_result(one(volume_group('mmxdata', group_on + [second], 'keep'), 'run')[0], 'succeeded', 'changed')
        assert added['reason'] == f'added {second} to the volume group mmxdata', added
        holding = [device for device, (group, used) in physical_volumes().items() if group == 'mmxdata' and used]
        assert len(holding) == 1 and physical_volumes()[second] == ('mmxdata', 0), physical_volumes()
        # An earlier node task may delete the volume or move its data, so a dryrun does not check this task.
        waits_for_an_earlier_task(volume_group('mmxdata', [second], 'remove'), f'lvm_volume_group: the device {holding[0]} of the volume group mmxdata still holds 52 MiB of logical volumes, so it cannot leave the volume group')
        leaving = [device for device in (disk + 'p2', first) if device != holding[0]][0]
        staying = [path for path in group_on if remote(f'readlink -e {path}') == holding[0]]
        took = f'{leaving} out of the volume group mmxdata and'
        is_result(one(volume_group('mmxdata', staying + [second], 'remove'), 'dryrun')[0], 'succeeded', 'would_change', reason=f'would take {took} remove LVM\'s label from it')
        assert physical_volumes()[leaving] == ('mmxdata', 0)
        is_result(one(volume_group('mmxdata', staying + [second], 'remove'), 'run')[0], 'succeeded', 'changed', reason=f'took {took} removed LVM\'s label from it')
        assert leaving not in physical_volumes() and signatures(leaving) == [], (physical_volumes(), signatures(leaving))
        is_result(one(volume_group('mmxdata', staying + [second], 'remove'), 'run', 0)[0], 'succeeded', 'unchanged')
        # A run that ended between its two commands: the device left the
        # volume group and kept LVM's label. The module takes such a device
        # back without a question, as LVM does.
        remote(f'vgreduce -q mmxdata {second} >/dev/null')
        assert physical_volumes()[second] == ('', 0) and signatures(second) == ['LVM2_member']
        is_result(one(volume_group('mmxdata', staying + [second, leaving], 'keep'), 'run')[0], 'succeeded', 'changed', reason=f'added {second}, {leaving} to the volume group mmxdata')
        assert {device: group for device, (group, used) in physical_volumes().items()} == {disk + 'p2': 'mmxdata', first: 'mmxdata', second: 'mmxdata', third: 'mmxother'}
        mark('a listed device is added; with remove, a device that the task does not list leaves the volume group and loses LVM\'s label, and a device that holds data stays: '
             'a dryrun does not check that task, and a run fails once; a device that an interrupted run left with LVM\'s label is added again')

        # A new volume in space where an old volume had a filesystem: lvcreate
        # would ask about the old signature, answer no by itself, and create
        # nothing. The module has it wipe that signature, so the new volume
        # is empty. (Ubuntu's LVM discards a removed volume's space, which
        # also removes the signature; without the discard it stays.)
        remote('lvcreate -q --yes --wipesignatures y --name mmxold --size 64MiB mmxdata >/dev/null\nmkfs.ext4 -q /dev/mmxdata/mmxold\n'
               'lvremove -q --yes --config devices/issue_discards=0 mmxdata/mmxold >/dev/null')
        is_result(one(volume('logs', '64MiB'), 'run')[0], 'succeeded', 'changed', reason='created the logical volume mmxdata/logs with 64 MiB')
        assert signatures('/dev/mmxdata/logs') == [], signatures('/dev/mmxdata/logs')
        mark('a new logical volume in space where a removed volume had a filesystem is created and holds no signature')

        # Growing. An ext4 filesystem grows with its volume while it is
        # mounted; an xfs filesystem while it is not mounted, which LVM
        # 2.03.31 and newer do only with --fsmode manage.
        remote(f'mkfs.ext4 -q /dev/mmxdata/postgresql\nmkdir -p {BASE}/mounted\nmount /dev/mmxdata/postgresql {BASE}/mounted\necho kept > {BASE}/mounted/file')
        small = filesystem_bytes(f'{BASE}/mounted')
        grown = 'the logical volume mmxdata/postgresql from 52 MiB to 100 MiB, together with the filesystem on it'
        is_result(one(volume('postgresql', '100MiB'), 'dryrun')[0], 'succeeded', 'would_change', reason='would grow ' + grown)
        assert logical_volumes('mmxdata')['postgresql'] == 52 * MIB and filesystem_bytes(f'{BASE}/mounted') == small
        result = is_result(one(volume('postgresql', '100MiB'), 'run')[0], 'succeeded', 'changed', reason='grew ' + grown)
        assert result['module_outputs'] == {'created': False, 'device': '/dev/mmxdata/postgresql'}, result
        assert logical_volumes('mmxdata')['postgresql'] == 100 * MIB and filesystem_bytes(f'{BASE}/mounted') > small * 1.7, (small, filesystem_bytes(f'{BASE}/mounted'))
        assert remote(f'cat {BASE}/mounted/file') == 'kept'
        for command in ('run', 'dryrun'):
            is_result(one(volume('postgresql', '100MiB'), command, 0)[0], 'succeeded', 'unchanged')
        is_result(one(volume('archive', '320MiB'), 'run')[0], 'succeeded', 'changed')
        def xfs_bytes():
            """The size of the xfs filesystem, which is mounted only to read it."""
            remote(f'mkdir -p {BASE}/xfs\nmount /dev/mmxdata/archive {BASE}/xfs')
            size = filesystem_bytes(f'{BASE}/xfs')
            remote(f'umount {BASE}/xfs')
            return size
        remote('mkfs.xfs -q /dev/mmxdata/archive')
        xfs_before = xfs_bytes()
        is_result(one(volume('archive', '400MiB'), 'run')[0], 'succeeded', 'changed',
                  reason='grew the logical volume mmxdata/archive from 320 MiB to 400 MiB, together with the filesystem on it')
        assert logical_volumes('mmxdata')['archive'] == 400 * MIB and xfs_bytes() >= xfs_before + 64 * MIB, (xfs_before, xfs_bytes())
        print(f"{distribution}: this LVM {'has' if has_fsmode else 'does not have'} lvextend --fsmode; an xfs filesystem that was not mounted grew with its volume", flush=True)
        mark('a smaller logical volume grows to the task\'s size: with its mounted ext4 filesystem, whose files stay, and with an xfs filesystem that is not mounted; '
             'a dryrun before says so and changes nothing, and a rerun is unchanged')

        # A volume that holds nothing has no filesystem that could be left
        # small, so it grows alone: the volume of a first run that stopped
        # before its filesystem task, when the size was raised before the
        # next run. A volume that holds something other than a filesystem
        # that LVM grows, such as swap, does not grow together with a
        # filesystem: LVM would refuse and grow nothing, so the module looks
        # at the volume first, and a dryrun fails as a run does. With
        # grow_filesystem_with_volume: false that volume grows.
        is_result(one(volume('swap', '32MiB'), 'run')[0], 'succeeded', 'changed')
        empty = 'the logical volume mmxdata/swap from 32 MiB to 48 MiB, and no filesystem, because the volume holds none yet'
        is_result(one(volume('swap', '48MiB'), 'dryrun')[0], 'succeeded', 'would_change', reason='would grow ' + empty)
        assert logical_volumes('mmxdata')['swap'] == 32 * MIB
        is_result(one(volume('swap', '48MiB'), 'run')[0], 'succeeded', 'changed', reason='grew ' + empty)
        assert logical_volumes('mmxdata')['swap'] == 48 * MIB and signatures('/dev/mmxdata/swap') == []
        is_result(one(volume('swap', '48MiB'), 'run', 0)[0], 'succeeded', 'unchanged')
        remote('mkswap /dev/mmxdata/swap >/dev/null')
        refused(volume('swap', '64MiB'), 'lvm_logical_volume: the logical volume mmxdata/swap holds no ext2, ext3, ext4, or xfs filesystem (found: swap area (swap signature at offset 0xff6)), '
                'and grow_filesystem_with_volume is true: LVM grows a volume together with its filesystem only for these four. '
                'To grow a volume without such a filesystem, such as swap, set grow_filesystem_with_volume: false')
        assert logical_volumes('mmxdata')['swap'] == 48 * MIB
        is_result(one(volume('swap', '64MiB', grow_filesystem_with_volume=False), 'run')[0], 'succeeded', 'changed',
                  reason='grew the logical volume mmxdata/swap from 48 MiB to 64 MiB, and not a filesystem on it, because grow_filesystem_with_volume is false')
        assert logical_volumes('mmxdata')['swap'] == 64 * MIB and signatures('/dev/mmxdata/swap') == ['swap']
        # A filesystem that is mounted read-only: LVM grows the volume, and
        # the filesystem's own program then fails. The task fails and says
        # that the filesystem is still smaller. It is not tried again,
        # because another try would find the volume at its size and report
        # success; it stays an ordinary failure with its change.
        remote(f'mkfs.ext4 -q /dev/mmxdata/logs\nmkdir -p {BASE}/read-only\nmount -o ro /dev/mmxdata/logs {BASE}/read-only')
        small = filesystem_bytes(f'{BASE}/read-only')
        left_small = is_result(one(volume('logs', '96MiB'), 'run', 1)[0], 'failed', 'changed', tries=1,
                               reason='The logical volume now has 96 MiB, and its filesystem is still smaller: once the cause is gone, grow the filesystem by hand')
        assert 'failure_kind' not in left_small and 'Read-only file system' in left_small['reason'], left_small
        assert left_small['module_outputs'] == {'created': False, 'device': '/dev/mmxdata/logs'}, left_small
        assert logical_volumes('mmxdata')['logs'] == 96 * MIB and filesystem_bytes(f'{BASE}/read-only') == small
        is_result(one(volume('logs', '96MiB'), 'run', 0)[0], 'succeeded', 'unchanged')
        remote(f'umount {BASE}/read-only')
        mark('with grow_filesystem_with_volume: true, a volume that holds nothing grows alone, and a volume that holds swap is refused in a dryrun and in a run, '
             'named with what it holds, and not grown; with false that volume grows; a volume whose filesystem is mounted read-only grows, '
             'and the task fails without another try and says that the filesystem is still smaller')

        # An ext3 filesystem grows with its volume as an ext4 one does.
        is_result(one(volume('old-format', '32MiB'), 'run')[0], 'succeeded', 'changed')
        remote('mkfs.ext3 -q /dev/mmxdata/old-format')
        is_result(one(volume('old-format', '48MiB'), 'run')[0], 'succeeded', 'changed',
                  reason='grew the logical volume mmxdata/old-format from 32 MiB to 48 MiB, together with the filesystem on it')
        blocks, block_bytes = (int(remote(f"dumpe2fs -h /dev/mmxdata/old-format 2>/dev/null | sed -n 's/^{field}: *//p'")) for field in ('Block count', 'Block size'))
        assert logical_volumes('mmxdata')['old-format'] == 48 * MIB == blocks * block_bytes and signatures('/dev/mmxdata/old-format') == ['ext3'], (blocks, block_bytes)
        mark('an ext3 filesystem grows together with its volume')

        # A volume with a snapshot. lvremove --yes on the volume would delete
        # the snapshot too, which the task does not name: the module runs no
        # lvremove while a volume depends on the one to delete. A playbook
        # deletes the snapshot first; in its dryrun the snapshot is still
        # there, so the dryrun does not check the second task.
        remote('lvcreate -q --yes --snapshot --name mmxsnap --size 8MiB mmxdata/old-format >/dev/null')
        origin_gone = volume('old-format', state='deleted_with_all_its_data')
        waits_for_an_earlier_task(origin_gone, 'lvm_logical_volume: other logical volumes depend on the logical volume mmxdata/old-format, and LVM deletes them together with it: '
                                  'mmxsnap (a snapshot of it). Delete each of them first with its own lvm_logical_volume task and state: deleted_with_all_its_data; nothing was deleted')
        assert {'old-format', 'mmxsnap'} <= set(logical_volumes('mmxdata'))
        snapshot_first = [{'id': 'snapshot', 'lvm_logical_volume': {'name': 'mmxsnap', 'volume_group': 'mmxdata', 'state': 'deleted_with_all_its_data'}}, origin_gone]
        previewed = one(snapshot_first, 'dryrun', 3)
        assert previewed[0]['change'] == 'would_change' and previewed[1]['status'] == 'not_checked', previewed
        assert {'old-format', 'mmxsnap'} <= set(logical_volumes('mmxdata'))
        deleted = one(snapshot_first, 'run')
        assert [r['change'] for r in deleted] == ['changed', 'changed'] and deleted[1]['reason'] == 'deleted the logical volume mmxdata/old-format with all its data (48 MiB)', deleted
        assert not {'old-format', 'mmxsnap'} & set(logical_volumes('mmxdata'))
        mark('a volume with a snapshot is not deleted, and the snapshot stays: a dryrun does not check the task, and a run fails once and names the snapshot; '
             'a playbook that deletes the snapshot first deletes both, and its dryrun leaves the second task not checked')

        # The module never shrinks, and it checks the free space itself.
        refused(volume('postgresql', '64MiB'), 'lvm_logical_volume: the logical volume mmxdata/postgresql has 100 MiB, and the task asks for 64 MiB. The module never shrinks a logical volume')
        free = volume_groups()['mmxdata']
        # An earlier node task may add a device to the volume group, so a dryrun does not check these tasks.
        waits_for_an_earlier_task(volume('postgresql', '10GiB'), f'and the volume group mmxdata has {free // MIB} MiB free')
        waits_for_an_earlier_task(volume('too-large', '10GiB'), f'lvm_logical_volume: the volume group mmxdata has {free // MIB} MiB free, and the new logical volume mmxdata/too-large needs 10 GiB')
        assert logical_volumes('mmxdata') == {'postgresql': 100 * MIB, 'logs': 96 * MIB, 'archive': 400 * MIB, 'swap': 64 * MIB}
        # The rest of the volume group goes to a new volume; an existing
        # volume is unchanged whatever its size.
        rest = is_result(one(volume('everything', 'all_free_space_when_created'), 'run')[0], 'succeeded', 'changed')
        assert rest['reason'] == f'created the logical volume mmxdata/everything with {free // MIB} MiB, the rest of the volume group', rest
        assert volume_groups()['mmxdata'] == 0 and logical_volumes('mmxdata')['everything'] == free
        is_result(one(volume('everything', 'all_free_space_when_created'), 'run', 0)[0], 'succeeded', 'unchanged')
        is_result(one(volume('postgresql', 'all_free_space_when_created'), 'dryrun', 0)[0], 'succeeded', 'unchanged')
        waits_for_an_earlier_task(volume('nothing-left', 'all_free_space_when_created'), 'lvm_logical_volume: the volume group mmxdata has no free space')
        # A volume group that does not exist yet, as before an earlier node task creates it.
        no_group = volume('postgresql', '50MiB', group='mmx-no-such-group')
        is_result(one(no_group, 'dryrun', 3)[0], 'not_checked', 'unknown', reason='lvm_logical_volume: the volume group mmx-no-such-group does not exist; create it in an earlier node task with lvm_volume_group')
        assert is_result(one(no_group, 'run', 1)[0], 'failed', 'unchanged', tries=1)['failure_kind'] == 'missing_prerequisite'
        mark('a larger volume is never shrunk, in a dryrun and in a run; a size beyond the free space fails a run before LVM is called, for a new and for an existing volume, '
             'and leaves a dryrun not checked; all_free_space_when_created gives a new volume all free space and leaves an existing one; a missing volume group leaves a dryrun not checked')

        # Removal. A volume in use stays; a volume group with volumes stays.
        gone = volume('postgresql', state='deleted_with_all_its_data')
        waits_for_an_earlier_task(gone, 'lvm_logical_volume: the logical volume mmxdata/postgresql is in use: a filesystem on it is mounted, or a program holds the device open')
        assert remote(f'cat {BASE}/mounted/file') == 'kept'
        remote(f'umount {BASE}/mounted')
        waits_for_an_earlier_task({'lvm_volume_group': {'name': 'mmxdata', 'state': 'absent'}}, 'lvm_volume_group: the volume group mmxdata still has the logical volumes ')
        is_result(one(gone, 'dryrun')[0], 'succeeded', 'would_change', reason='would delete the logical volume mmxdata/postgresql with all its data (100 MiB)')
        assert 'postgresql' in logical_volumes('mmxdata')
        removed = is_result(one(gone, 'run')[0], 'succeeded', 'changed', reason='deleted the logical volume mmxdata/postgresql with all its data (100 MiB)')
        assert removed['module_outputs'] == {'created': False} and 'postgresql' not in logical_volumes('mmxdata'), removed
        for command in ('run', 'dryrun'):
            is_result(one(gone, command, 0)[0], 'succeeded', 'unchanged')
        project([{'id': name, 'lvm_logical_volume': {'name': name, 'volume_group': 'mmxdata', 'state': 'deleted_with_all_its_data'}} for name in ('logs', 'archive', 'swap', 'everything')]
                + [{'id': 'group', 'lvm_volume_group': {'name': 'mmxdata', 'state': 'absent'}}])
        # This is the playbook of the volume group's page. In its dryrun the
        # volume group still has its volumes, which only the run deletes, so
        # the dryrun does not check the last task.
        previewed = results(run('dryrun', 3))
        assert [r['change'] for r in previewed[:4]] == ['would_change'] * 4 and previewed[4]['status'] == 'not_checked', previewed
        emptied = results(run('run'))
        assert [r['change'] for r in emptied] == ['changed'] * 5, emptied
        devices = [disk + 'p2', first, second]
        assert emptied[4]['reason'].startswith('removed the volume group mmxdata and LVM\'s label from ') and all(device in emptied[4]['reason'] for device in devices), emptied[4]
        assert volume_groups() == {'mmxother': volume_groups()['mmxother']} and physical_volumes() == {third: ('mmxother', 0)}
        assert all(signatures(device) == [] for device in devices)
        assert [r['change'] for r in results(run('run', 0))] == ['unchanged'] * 5
        remote(f'vgremove -q mmxother >/dev/null\npvremove -q {third} >/dev/null')
        mark('a mounted volume and a volume group with volumes stay: a dryrun does not check those tasks, and a run fails once; deleted_with_all_its_data deletes a volume, '
             'absent removes the empty volume group, and each of its devices loses LVM\'s label; a dryrun of the playbook that does both leaves the volume group\'s task not checked; '
             'what is missing is unchanged')

        # The example project: an empty disk becomes one LVM partition, a
        # volume group, a logical volume, an ext4 filesystem, and a mount.
        for device in (first, second, third):
            detach(device)
        data_disk = loop_device('data-disk', 1024)
        example = vm.ROOT / ('partitions-and-lvm-' + EXAMPLE)
        if example.exists():
            shutil.rmtree(example)
        shutil.copytree(vm.REPO / 'examples' / EXAMPLE, example)

        def example_inventory(size):
            vm.write_inventory(example, {'nodes': {'database': node}, 'vars': {
                'data_volume_group': 'mmxexample', 'data_logical_volume': 'postgresql', 'data_logical_volume_size': size, 'data_mount_point': MOUNT_POINT}}, name='example')

        def example_run(command, expected=None):
            return results(run(command, expected, directory=example, extra=('--var', 'data_disk=' + data_disk)))

        # The example's node tasks in their order; the first two install
        # the packages, one for each package manager.
        PARTITIONS, GROUP, VOLUME, FILESYSTEM, DIRECTORY, MOUNT = range(2, 8)
        example_inventory('256MiB')
        previewed = example_run('dryrun', 3)
        assert len(previewed) == 8 and previewed[PARTITIONS]['change'] == 'would_change' and 'mmxexample-lvm (1022 MiB, the rest of the disk)' in previewed[PARTITIONS]['reason'], previewed
        assert all(previewed[task]['status'] == 'not_checked' for task in (GROUP, VOLUME, FILESYSTEM, MOUNT)), previewed
        assert table(data_disk) is None
        started = time.monotonic()
        applied = example_run('run')
        assert all(applied[task]['change'] == 'changed' for task in (PARTITIONS, GROUP, VOLUME, FILESYSTEM, DIRECTORY, MOUNT)), applied
        assert table(data_disk) == [('mmxexample-lvm', 2048, 1022 * 2048, TYPES['linux_lvm'])], table(data_disk)
        assert physical_volumes() == {data_disk + 'p1': ('mmxexample', 256 * MIB)} and logical_volumes('mmxexample') == {'postgresql': 256 * MIB}
        source, fstype = remote(f'findmnt -n -o SOURCE,FSTYPE --mountpoint {MOUNT_POINT}').split()
        assert remote(f'readlink -e {source}') == remote('readlink -e /dev/mmxexample/postgresql') and fstype == 'ext4', (source, fstype)
        assert f'/dev/mmxexample/postgresql {MOUNT_POINT} ext4 defaults 0 0' in remote('cat /etc/fstab')
        seconds = time.monotonic() - started
        for command in ('run', 'dryrun'):
            assert all(r['change'] == 'unchanged' for r in example_run(command, 0))
        # A larger size in the inventory grows the volume and its filesystem
        # while the filesystem stays mounted.
        remote(f'echo kept > {MOUNT_POINT}/file')
        small = filesystem_bytes(MOUNT_POINT)
        example_inventory('512MiB')
        changed = [r for r in example_run('run') if r['change'] != 'unchanged']
        assert len(changed) == 1 and changed[0]['reason'] == 'grew the logical volume mmxexample/postgresql from 256 MiB to 512 MiB, together with the filesystem on it', changed
        assert filesystem_bytes(MOUNT_POINT) > small * 1.8 and remote(f'cat {MOUNT_POINT}/file') == 'kept'
        assert all(r['change'] == 'unchanged' for r in example_run('run', 0))
        mark('the example ' + EXAMPLE + f': its first dryrun reports the partition table and leaves the later tasks not checked; its run, in {seconds:.0f} s, '
             'makes the partition, the volume group, the volume, the filesystem, and the mount; a rerun and a dryrun change nothing; a larger size in the inventory grows the mounted filesystem')

        # The playbook of the logical volume's page: unmount the filesystem,
        # then delete the volume. In its dryrun the filesystem is still
        # mounted, so the dryrun does not check the second task.
        unmount_first = [{'id': 'unmount', 'fstab_mount': {'mount_point': MOUNT_POINT, 'state': 'unmounted_and_removed_from_fstab'}},
                         {'id': 'volume', 'lvm_logical_volume': {'name': 'postgresql', 'volume_group': 'mmxexample', 'state': 'deleted_with_all_its_data'}}]
        previewed = one(unmount_first, 'dryrun', 3)
        assert previewed[0]['change'] == 'would_change' and previewed[1]['status'] == 'not_checked' and 'is in use' in previewed[1]['reason'], previewed
        assert remote(f'cat {MOUNT_POINT}/file') == 'kept'
        deleted = one(unmount_first, 'run')
        assert [r['change'] for r in deleted] == ['changed', 'changed'] and logical_volumes('mmxexample') == {}, deleted
        mark('the playbook that unmounts a filesystem and then deletes its logical volume: its dryrun leaves the volume\'s task not checked, and its run does both')
    finally:
        failing = sys.exc_info()[0] is not None
        cleanup()
        package_manager_said = remove_what_the_suite_installed()
        if not failing:
            left = remote(f'losetup --list --noheadings -O NAME,BACK-FILE | grep {BASE} || true\nls -d {BASE} {MOUNT_POINT} /dev/disk/by-partlabel/mmx* 2>/dev/null || true')
            packages = set(remote(INSTALLED_PACKAGES).split())
            if packages != packages_before:
                left += f'\npackages added: {sorted(packages - packages_before)}; packages gone: {sorted(packages_before - packages)}; the package manager said at the removal:\n{package_manager_said}'
            paths = [path for path in remote(f"ls -d {' '.join(LEFT_BY_LVM)} 2>/dev/null || true").split() if path not in paths_before]
            if paths:
                left += f'\nleft by LVM: {paths}'
            state = vm.guest('sudo', '-n', 'systemctl', 'is-system-running', check=False).stdout.strip()
            if state != 'running':
                left += f'\nsystemd is {state}:\n' + vm.guest('sudo', '-n', '/bin/sh', '-c', vm.FAILED_UNITS, check=False).stdout
            assert left.strip() == '', f'the suite left this on the guest:\n{left}'
            print('The guest is as the suite found it: no loop device, no volume group, no package of the suite, and systemd is running', flush=True)
    (workspace / 'summary.json').write_text(json.dumps(passed, indent=2) + '\n')
    print(f'{len(passed)} partition and LVM workflows passed on {distribution}', flush=True)


if __name__ == '__main__':
    vm.run_suite(main)
