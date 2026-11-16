#!/usr/bin/env python3
"""swap_file, create_filesystem_on_empty_device, and fstab_mount through the
public CLI, on loop devices made from files inside the node. The node's state
is checked with blkid, wipefs, findmnt, swapon, stat, systemctl, and
/etc/fstab itself. It needs the node's own kernel: a container cannot make
filesystems, mount them, or switch swap on."""
import json
import shlex
import shutil

import vm

DATA = '/srv/mmx-storage-data'
OTHER = '/srv/mmx-storage-other'
SWAP = '/swapfile'
NOT_SWAP = '/var/tmp/mmx-storage-not-swap'
IMAGES = ('/var/tmp/mmx-storage-a.img', '/var/tmp/mmx-storage-b.img')
SAVED_FSTAB = '/root/mmx-storage-fstab.original'
LABEL = 'mmxstoragedata'


def main():
    vm.up()
    node, ssh = vm.connection()
    vm.reset()
    workspace = vm.ROOT / 'storage'
    if workspace.exists():
        shutil.rmtree(workspace)
    vm.write_inventory(workspace, {'nodes': {'app': node}}, name='example')
    vm.build()
    mmx = vm.ROOT / 'bin/mmx'
    passed = []

    def remote(script, check=True):
        return vm.cmd([*ssh, 'sudo -n /bin/sh -c ' + shlex.quote('set -eu\nPATH=/usr/sbin:/sbin:$PATH\n' + script)], check=check).stdout

    def run(command, expected=None):
        result = vm.cmd([mmx, command, *vm.playbook_args(command), '--json'], cwd=workspace, check=False)
        codes = vm.success_codes(command) if expected is None else (expected,)
        assert result.returncode in codes, (command, result.returncode, result.stdout, result.stderr)
        return json.loads(result.stdout)

    def project(tasks):
        vm.write_project(workspace, project_name='storage', node_task_defaults={'retry': {'pause_between_tries': '0s'}},
                         fleet_stages=[{'id': 'configure', 'nodes': 'all', 'node_tasks': tasks}])

    def results(record):
        return record['entries'][0]['response']['results']

    def change(record):
        return record['entries'][0]['response']['change']

    def mark(message):
        passed.append(message)
        print('PASS ' + message, flush=True)

    def fstab():
        return remote('cat /etc/fstab')

    def live(path):
        """SOURCE FSTYPE OPTIONS of the mount at path, or ''."""
        return remote(f'findmnt -n -o SOURCE,FSTYPE,OPTIONS --mountpoint {path} || true').strip()

    def signature(device):
        return remote(f'blkid -p -o value -s TYPE {device} || true').strip()

    def device_sum(device):
        return remote(f'head -c 4194304 {device} | sha256sum').split()[0]

    def swaps():
        return remote('swapon --show=NAME,SIZE --noheadings --bytes || true')

    def swap_size(path):
        """The bytes swapon reports for path: the file without its header page."""
        sizes = [int(size) for name, size in (line.split() for line in swaps().splitlines()) if name == path]
        return sizes[0] if sizes else 0

    def cleanup():
        remote(f'systemctl stop mmx-storage-holder 2>/dev/null || true\n'
               f'for path in {DATA} {OTHER}; do while mountpoint -q $path; do umount -l $path; done; done\n'
               f'swapoff {SWAP} 2>/dev/null || true\nrm -f {SWAP} /.swapfile.mmx-new {NOT_SWAP}\n'
               f'if [ -e {SAVED_FSTAB} ]; then cp {SAVED_FSTAB} /etc/fstab; rm {SAVED_FSTAB}; systemctl daemon-reload; fi\n'
               f'for image in {" ".join(IMAGES)}; do for loop in $(losetup -j $image -O NAME -n 2>/dev/null); do losetup -d $loop; done; rm -f $image; done\n'
               f'rmdir {DATA} {OTHER} 2>/dev/null || true')

    distribution = remote('. /etc/os-release; echo "$ID"').strip()
    cleanup()
    assert remote(f'test -e {SWAP} && echo exists || echo absent').strip() == 'absent', f'{SWAP} exists on this node'
    remote(f'cp /etc/fstab {SAVED_FSTAB}')
    original = fstab()
    try:
        devices = [remote(f'truncate -s 128M {image}\nlosetup --find --show {image}').strip() for image in IMAGES]
        empty, other = devices
        assert all(d.startswith('/dev/loop') for d in devices), devices

        # Where the node lacks mkfs.ext4, a preview reports it not checked and
        # a run names the package; a package task first fixes it.
        if remote('command -v mkfs.ext4 || true').strip() == '':
            project([{'create_filesystem_on_empty_device': {'device': empty, 'filesystem_type': 'ext4'}}])
            assert results(run('dryrun', 3))[0]['status'] == 'not_checked'
            assert 'install the package e2fsprogs' in json.dumps(run('run', 1))
            project([*vm.package_tasks(['e2fsprogs'])])
            run('run')
            mark(f'a missing mkfs.ext4 names the package e2fsprogs ({distribution})')

        tasks = [
            {'create_filesystem_on_empty_device': {'device': empty, 'filesystem_type': 'ext4', 'mkfs_options': ['-L', LABEL]}},
            {'directory': {'path': DATA}},
            {'fstab_mount': {'mount_point': DATA, 'mount_source': f'LABEL={LABEL}', 'filesystem_type': 'ext4', 'options': ['noatime'],
                             'check_filesystem_at_boot': 'after_root_filesystem', 'state': 'mounted'}},
            {'swap_file': {'path': SWAP, 'size': '64MiB'}},
        ]
        project(tasks)
        preview = results(run('dryrun', 3))
        assert preview[0]['change'] == 'would_change' and f'would create ext4 on {empty}' in preview[0]['reason'], preview[0]
        assert preview[2]['status'] == 'not_checked' and 'does not exist yet' in preview[2]['reason'], preview[2]
        assert preview[3]['change'] == 'would_change' and 'would write /swapfile with 64 MiB' in preview[3]['reason'], preview[3]
        assert f'+{SWAP} none swap defaults 0 0' in preview[3]['file_change']['diff'], preview[3]
        assert signature(empty) == '' and fstab() == original and swaps().strip() == ''
        assert remote(f'test -e {SWAP} && echo exists || echo absent').strip() == 'absent'
        mark('dryrun reports the filesystem, mount, and swap file it would create and changes nothing')

        made = run('run')
        assert change(made) == 'changed', made
        assert signature(empty) == 'ext4' and remote(f'blkid -p -o value -s LABEL {empty}').strip() == LABEL
        source, fstype, options = live(DATA).split()
        assert source == empty and fstype == 'ext4' and 'noatime' in options.split(','), live(DATA)
        written = fstab()
        assert written.startswith(original.rstrip('\n') + '\n'), written
        assert f'LABEL={LABEL} {DATA} ext4 noatime 0 2\n' in written and f'{SWAP} none swap defaults 0 0\n' in written, written
        assert remote('findmnt --verify >/dev/null 2>&1 && echo accepted || echo rejected').strip() == 'accepted'
        size, blocks, block_size, metadata = remote(f'stat -c "%s %b %B %a:%U:%G" {SWAP}').split()
        assert int(size) == 64 << 20 and int(blocks) * int(block_size) >= int(size) and metadata == '600:root:root', (size, blocks, metadata)
        assert (64 << 20) - 65536 <= swap_size(SWAP) < 64 << 20, swaps()
        # systemd read the new /etc/fstab: its generated units know both lines.
        unit = remote(f'systemd-escape --path --suffix=mount {DATA}').strip()
        assert remote(f'systemctl is-active {shlex.quote(unit)} || true').strip() == 'active', remote(f'systemctl status {shlex.quote(unit)} || true')
        swap_unit = remote(f'systemd-escape --path --suffix=swap {SWAP}').strip()
        assert remote(f'systemctl show -p FragmentPath --value {shlex.quote(swap_unit)}').strip().startswith('/run/systemd/generator'), swap_unit
        # On SELinux, mkswap gives a swap file the type swapfile_t, which
        # systemd needs to switch it on at boot.
        if remote(f'if {vm.SELINUX_RUNS}; then echo on; fi').strip() == 'on':
            label = remote(f'stat -c %C {SWAP}').strip()
            assert ':swapfile_t:' in label, label
        for command in ('run', 'dryrun'):
            assert change(run(command, 0)) == 'unchanged'
        assert fstab() == written
        mark('a run makes ext4 on the empty device, mounts it by its label with noatime, writes a 64 MiB swap file '
             '(no holes, 600 root) and switches it on, adds both lines to /etc/fstab, which findmnt --verify accepts '
             'and systemd has read, and leaves every other line alone; a rerun and a dryrun change nothing')

        # Changed options remount; a mount whose flags drifted is remounted.
        tasks[2]['fstab_mount']['options'] = ['nodev', 'nosuid']
        project(tasks)
        preview = results(run('dryrun'))[2]
        assert preview['change'] == 'would_change' and f'would remount {DATA} with options nodev,nosuid' in preview['reason'], preview
        assert 'noatime' in live(DATA)
        assert change(run('run')) == 'changed'
        options = live(DATA).split()[2].split(',')
        assert {'rw', 'nodev', 'nosuid', 'relatime'} <= set(options) and 'noatime' not in options, options
        assert f'LABEL={LABEL} {DATA} ext4 nodev,nosuid 0 2\n' in fstab()
        assert change(run('run')) == 'unchanged'
        remote(f'mount -o remount,ro {DATA}')
        assert 'ro' in live(DATA).split()[2].split(',')
        assert results(run('run'))[2]['change'] == 'changed'
        assert 'rw' in live(DATA).split()[2].split(',') and change(run('run')) == 'unchanged'
        mark('changed options remount the filesystem and rewrite its line; a mount someone remounted read-only is remounted read-write')

        # Real devices that hold something are never formatted.
        for prepare, found in [
            (f'mkfs.ext2 -q -F {other}', 'ext2 filesystem'),
            (f'wipefs -q -a {other}\nmkswap {other} >/dev/null', 'swap area'),
            (f'wipefs -q -a {other}\nprintf "\\000\\040\\041\\000\\203\\052\\254\\014\\000\\010\\000\\000\\000\\040\\003\\000" | dd of={other} bs=1 seek=446 conv=notrunc 2>/dev/null\n'
             f'printf "\\125\\252" | dd of={other} bs=1 seek=510 conv=notrunc 2>/dev/null', 'MBR partition table'),
        ]:
            remote(prepare)
            before = device_sum(other)
            project([{'create_filesystem_on_empty_device': {'device': other, 'filesystem_type': 'ext4'}}])
            for command in ('dryrun', 'run'):
                refused = results(run(command, 1))[0]
                assert refused['status'] == 'failed' and refused['change'] == 'unchanged', refused
                assert f'{other} is not empty; found: {found}' in refused['reason'] and 'never destroys data' in refused['reason'], refused
            assert device_sum(other) == before
        mark('create_filesystem_on_empty_device refuses a device with another filesystem, with swap, and with a partition table, '
             'in dryrun and run, naming what it found, and leaves the device as it was')

        # A live mount of another source fails and changes nothing; so does a
        # line that findmnt --verify rejects.
        remote(f'wipefs -q -a {other}\nmkfs.ext4 -q {other}')
        project([{'fstab_mount': {'check_filesystem_at_boot': 'never', 'options': ['defaults'], 'mount_point': DATA, 'mount_source': other, 'filesystem_type': 'ext4', 'state': 'mounted'}}])
        for command in ('dryrun', 'run'):
            refused = results(run(command, 1))[0]
            assert refused['change'] == 'unchanged' and f'{DATA} has {empty} (ext4) mounted, and the task asks for {other} (ext4)' in refused['reason'], refused
        remote(f'mkdir -p {OTHER}')
        project([{'fstab_mount': {'check_filesystem_at_boot': 'never', 'options': ['defaults'], 'mount_point': OTHER, 'mount_source': 'LABEL=mmx-no-such-label', 'filesystem_type': 'ext4', 'state': 'mounted'}}])
        rejected = results(run('run', 1))[0]
        assert rejected['failure_kind'] == 'validation' and 'findmnt --verify did not accept the new /etc/fstab' in rejected['reason'], rejected
        assert fstab() == written.replace(f'{DATA} ext4 noatime 0 2', f'{DATA} ext4 nodev,nosuid 0 2') and live(DATA).split()[0] == empty and live(OTHER) == ''
        mark('fstab_mount fails without a change when another device is mounted at the path, and when findmnt --verify rejects the new /etc/fstab')

        # A run cut off between the steps: the next run does only what is missing.
        remote(f'swapoff {SWAP}\nsed -i "\\#^{SWAP} #d" /etc/fstab\nhead -c 1048576 /dev/zero > /.swapfile.mmx-new')
        project([{'swap_file': {'path': SWAP, 'size': '64MiB'}}])
        resumed = results(run('run'))[0]
        assert resumed['reason'] == f'deleted .swapfile.mmx-new, switched on {SWAP}, added the swap file\'s line to /etc/fstab', resumed
        assert remote('test -e /.swapfile.mmx-new && echo left || echo gone').strip() == 'gone' and SWAP in swaps()
        # Another size: switched off, written again, switched on.
        project([{'swap_file': {'path': SWAP, 'size': '96MiB'}}])
        resized = results(run('run'))[0]
        assert resized['reason'].startswith(f'switched off {SWAP}, deleted the old {SWAP}, wrote {SWAP} with 96 MiB'), resized
        assert (96 << 20) - 65536 <= swap_size(SWAP) < 96 << 20 and fstab().count(f'{SWAP} none swap') == 1, swaps()
        # A file without a swap signature may hold data: never replaced.
        remote(f'printf "important records\\n" > {NOT_SWAP}')
        project([{'swap_file': {'path': NOT_SWAP, 'size': '64MiB'}}])
        refused = results(run('run', 1))[0]
        assert 'has no swap signature and may hold data' in refused['reason'] and refused['change'] == 'unchanged', refused
        assert remote(f'cat {NOT_SWAP}') == 'important records\n'
        mark('swap_file finds each step after an interrupted run and does only the missing ones; a file of another size is '
             'switched off, written again, and switched on; a file without a swap signature is never replaced')

        # A missing mount point fails the run; once it exists, mmx resume mounts.
        remote(f'umount {OTHER} 2>/dev/null || true; rmdir {OTHER}')
        project([{'fstab_mount': {'check_filesystem_at_boot': 'never', 'options': ['defaults'], 'mount_point': OTHER, 'mount_source': other, 'filesystem_type': 'ext4', 'state': 'mounted'}}])
        failed = run('run', 1)
        assert 'the mount point /srv/mmx-storage-other does not exist yet' in json.dumps(failed)
        remote(f'mkdir {OTHER}')
        resumed = run('resume')
        assert resumed['id'] == failed['id'] and resumed['status'] == 'succeeded', resumed
        assert live(OTHER).split()[0] == other and f'{other} {OTHER} ext4 defaults 0 0\n' in fstab()
        mark('a run that fails on a missing mount point is finished by mmx resume once the directory exists')

        # Unmount: a busy mount fails with the system's message; then the
        # path is unmounted and its line removed, the directory kept.
        # A transient unit keeps a process in the mount, and SSH need not wait for it.
        remote(f'systemd-run --quiet --unit=mmx-storage-holder --property=WorkingDirectory={OTHER} sleep 300')
        project([{'fstab_mount': {'mount_point': OTHER, 'state': 'unmounted_and_removed_from_fstab'}}])
        busy = results(run('run', 1))[0]
        assert 'busy' in busy['reason'] and busy['change'] == 'unchanged' and live(OTHER) != '', busy
        remote('systemctl stop mmx-storage-holder')
        project([{'fstab_mount': {'mount_point': OTHER, 'state': 'unmounted_and_removed_from_fstab'}},
                 {'fstab_mount': {'mount_point': DATA, 'state': 'unmounted_and_removed_from_fstab'}},
                 {'swap_file': {'path': SWAP, 'state': 'absent'}}])
        preview = results(run('dryrun'))
        assert [r['change'] for r in preview] == ['would_change'] * 3 and live(DATA) != '', preview
        run('run')
        assert live(OTHER) == '' and live(DATA) == '' and swaps().strip() == ''
        assert remote(f'test -d {OTHER} && test -d {DATA} && test ! -e {SWAP} && echo ok').strip() == 'ok'
        assert fstab() == original, fstab()
        assert change(run('run')) == 'unchanged'
        mark('a busy mount fails to unmount with the system\'s message; unmounted_and_removed_from_fstab unmounts and removes '
             'the line, keeping the directory; swap_file absent switches the file off, removes its line, and deletes it; '
             '/etc/fstab is back to its original text')
    finally:
        cleanup()
    print(f'{len(passed)} storage workflows passed', flush=True)


if __name__ == '__main__':
    vm.run_suite(main)
