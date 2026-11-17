#!/usr/bin/env python3
"""pacman_package through the public CLI on an Arch Linux node; checked with pacman -Q, systemctl, and the files of pacman's databases, never through MMX.

The suite installs nginx with a full system upgrade, keeps it, restarts its service from a node task block when its installed version differs from the one the service loaded, removes it with the package that depends on it, upgrades the whole system, waits for pacman's lock, and refuses a name that no repository has and a name that another package provides. The node's packages are upgraded by the suite, as every installation on Arch Linux upgrades them; the suite removes the packages that it added."""
import json
import shlex
import shutil
import time

import vm

LOCK_FILE = '/var/lib/pacman/db.lck'
# A package database file: its time of change says whether a task refreshed
# the databases.
CORE_DATABASE = '/var/lib/pacman/sync/core.db'


def main():
    vm.up()
    node, ssh = vm.connection()
    vm.reset()
    workspace = vm.ROOT / 'pacman-packages'
    if workspace.exists():
        shutil.rmtree(workspace)
    vm.write_inventory(workspace, {'nodes': {'arch': node}}, name='example')
    vm.build()
    mmx = vm.ROOT / 'bin/mmx'
    passed = []
    marked = time.monotonic()

    def remote(script, check=True):
        return vm.cmd([*ssh, 'sudo -n /bin/sh -c ' + shlex.quote('set -eu\n' + script)], check=check, timeout=900).stdout.strip()

    def run(command, expected=None, timeout=900):
        result = vm.cmd([mmx, command, *vm.playbook_args(command), '--json'], cwd=workspace, check=False, timeout=timeout)
        codes = vm.success_codes(command) if expected is None else (expected,)
        assert result.returncode in codes, (command, result.returncode, result.stdout, result.stderr)
        return json.loads(result.stdout)

    def project(tasks):
        vm.write_project(workspace, project_name='pacman_packages', node_task_defaults={'retry': {'pause_between_tries': '0s'}},
                         fleet_stages=[{'id': 'configure', 'nodes': 'all', 'node_tasks': tasks}])

    def results(record):
        """The results of the node tasks by id, the last part of each task's name."""
        return {r['task'].rsplit('/', 1)[-1]: r for r in record['entries'][0]['response']['results']}

    def one(task, command, expected=None):
        """Run one pacman_package node task and return its result."""
        project([{'id': 'packages', 'pacman_package': task}])
        return results(run(command, expected))['packages']

    def is_result(result, status, change, reason=None, tries=None):
        assert result['status'] == status and result['change'] == change and result.get('module_kind') == 'official_extra', result
        if reason is not None:
            assert reason in result.get('reason', ''), result
        if tries is not None:
            assert len(result.get('tries') or [result]) == tries, result
        return result

    def refused(task, reason):
        """The task cannot succeed: it fails in a dryrun and in a run with the same reason, after one try, and changes no package."""
        for command in ('dryrun', 'run'):
            before = installed()
            result = is_result(one(task, command, 1), 'failed', 'unchanged', reason=reason, tries=1)
            assert result['failure_kind'] == 'config' and installed() == before, result

    def mark(message):
        nonlocal marked
        now = time.monotonic()
        passed.append(message)
        print(f'PASS {message} ({now - marked:.0f} s)', flush=True)
        marked = now

    def installed():
        """The installed packages and their versions, from pacman -Q."""
        return dict(line.split(' ') for line in remote('pacman -Q').splitlines())

    def changes(before, after):
        """What changed between two pacman -Q readings, in the form of the output changed_packages."""
        found = []
        for name in sorted(set(before) | set(after)):
            if before.get(name) != after.get(name):
                found.append({'name': name, **({'from': before[name]} if name in before else {}), **({'to': after[name]} if name in after else {})})
        return found

    def database_changed_at():
        return remote(f'stat -c %Y.%y {CORE_DATABASE}')

    def main_pid():
        return remote('systemctl show -p MainPID --value nginx.service')

    # The real version of each package whose record fake_installed_version
    # changed, so that the clean-up can put the record back.
    faked = {}

    def fake_installed_version(package, version):
        """Make pacman's local database say that an older version is installed, as if a whole-system upgrade had not yet happened: the files stay, only the record changes."""
        faked.setdefault(package, installed()[package])
        remote(f'''d=$(ls -d /var/lib/pacman/local/{package}-[0-9]*)
mv "$d" /var/lib/pacman/local/{package}-{version}
sed -i '/^%VERSION%/{{n;s/.*/{version}/}}' /var/lib/pacman/local/{package}-{version}/desc''')
        assert installed()[package] == version

    assert remote('command -v pacman') == '/usr/bin/pacman'
    # An earlier suite on this guest may have left some of the suite's
    # packages: vm.py's test installs nginx on a node without apt.
    suite_packages = {'nginx', 'nginx-mod-stream', 'mailcap', 'tree'}
    left_by_earlier_suites = sorted(suite_packages & set(installed()))
    if left_by_earlier_suites:
        remote('systemctl disable --now nginx 2>/dev/null || true\npacman -Rns --noconfirm ' + ' '.join(left_by_earlier_suites))
    packages_before = set(installed())
    assert not suite_packages & packages_before, 'the guest still has a package of the suite'
    try:
        # A missing package: the dryrun previews the transaction and changes
        # nothing; the run installs it with a full system upgrade. The
        # record of an older which makes sure that the upgrade has something
        # to do, whatever the package servers published since the guest's
        # base.
        which_version = installed()['which']
        fake_installed_version('which', '0.1-1')
        which_upgrade = {'name': 'which', 'from': '0.1-1', 'to': which_version}
        before = installed()
        refreshed = database_changed_at()
        preview = is_result(one({'packages': ['nginx']}, 'dryrun'), 'succeeded', 'would_change', reason='would install ')
        names = [change['name'] for change in preview['module_outputs']['changed_packages']]
        assert {'nginx', 'mailcap'} <= set(names) and 'mailcap 2.' in preview['reason'] and 'nginx 1.' in preview['reason'], preview
        assert which_upgrade in preview['module_outputs']['changed_packages'], preview
        assert installed() == before and database_changed_at() == refreshed
        applied = is_result(one({'packages': ['nginx']}, 'run'), 'succeeded', 'changed', reason='installed ')
        after = installed()
        assert 'nginx' in after and applied['module_outputs']['changed_packages'] == changes(before, after), (applied, changes(before, after))
        assert which_upgrade in applied['module_outputs']['changed_packages'] and after['which'] == which_version, applied
        upgraded = [change for change in applied['module_outputs']['changed_packages'] if 'from' in change]
        print(f"pacman {remote('pacman -Q pacman').split()[1]}; nginx {after['nginx']}; the installation also upgraded {len(upgraded)} packages", flush=True)
        mark('a dryrun names what pacman would install, judged against the databases the node has, and changes nothing; a run installs nginx with mailcap, '
             'upgrades the older which in the same transaction, and reports exactly what pacman -Q shows changed')

        # Installed: unchanged, and no package server is asked.
        refreshed = database_changed_at()
        for command in ('run', 'dryrun'):
            again = is_result(one({'packages': ['nginx', 'mailcap']}, command, 0), 'succeeded', 'unchanged')
            assert again['module_outputs']['changed_packages'] == [] and 'reason' not in again, again
        assert database_changed_at() == refreshed and installed() == after
        mark('a rerun and a dryrun of installed packages are unchanged and do not refresh the package databases')

        # A node task block restarts nginx when the version that pacman -Q
        # shows differs from the one nginx last loaded.
        block = {'task_block': {'id': 'web', 'changes': [{'id': 'nginx_package', 'pacman_package': {'packages': ['nginx']}}],
                                'activate': [{'id': 'nginx_service', 'systemd_service': {'name': 'nginx', 'state': 'running'}}]}}
        project([block])
        run('run')
        pid = main_pid()
        assert pid != '0' and remote('systemctl is-active nginx') == 'active'
        assert run('run')['entries'][0]['response']['change'] == 'unchanged' and main_pid() == pid
        real_version = installed()['nginx']
        fake_installed_version('nginx', '1.0.0-1')
        previewed = results(run('dryrun'))
        assert previewed['nginx_package']['change'] == 'unchanged' and previewed['nginx_service']['change'] == 'would_change', previewed
        restarted = results(run('run'))
        assert restarted['nginx_package']['change'] == 'unchanged' and restarted['nginx_service']['change'] == 'changed', restarted
        assert main_pid() not in ('0', pid)
        pid = main_pid()
        assert run('run')['entries'][0]['response']['change'] == 'unchanged' and main_pid() == pid
        mark('in a node task block, nginx starts once; when the installed version differs from the one nginx loaded, a dryrun predicts the restart '
             'and a run restarts nginx once, with the package task unchanged; the next run restarts nothing')

        # The whole-system upgrade brings the package to the repository's
        # version; the block then restarts nginx again. A second upgrade
        # with nothing newer is unchanged, although it refreshed the
        # databases.
        before = installed()
        nginx_upgrade = {'name': 'nginx', 'from': '1.0.0-1', 'to': real_version}
        previewed = is_result(one({'upgrade_all_packages': True}, 'dryrun'), 'succeeded', 'would_change', reason='would upgrade ')
        assert nginx_upgrade in previewed['module_outputs']['changed_packages'] and installed() == before, previewed
        upgraded = is_result(one({'upgrade_all_packages': True}, 'run'), 'succeeded', 'changed', reason='upgraded ')
        assert installed()['nginx'] == real_version and upgraded['module_outputs']['changed_packages'] == changes(before, installed())
        assert nginx_upgrade in upgraded['module_outputs']['changed_packages'], upgraded
        for _ in range(3):
            # A package server may publish a package between two runs; the
            # third run at the latest finds nothing newer.
            refreshed = database_changed_at()
            again = one({'upgrade_all_packages': True}, 'run')
            if again['change'] == 'unchanged':
                break
        assert is_result(again, 'succeeded', 'unchanged')['module_outputs']['changed_packages'] == [], again
        project([block])
        restarted = results(run('run'))
        assert restarted['nginx_service']['change'] == 'changed' and main_pid() not in ('0', pid)
        mark('a dryrun of the whole-system upgrade names the upgrade and changes nothing; the run upgrades nginx back to the repository\'s version; '
             'an upgrade with nothing newer is unchanged although it refreshed the databases; the block then restarts nginx')

        # A conflict: --print checks none, so a dryrun says what it would
        # install; the transaction finds the conflict and changes no package.
        before = installed()
        is_result(one({'packages': ['nginx-mainline']}, 'dryrun'), 'succeeded', 'would_change', reason='would install nginx-mainline ')
        project([{'id': 'packages', 'retry': False, 'pacman_package': {'packages': ['nginx-mainline']}}])
        conflict = is_result(results(run('run', 1))['packages'], 'failed', 'changed', reason='pacman_package: installing nginx-mainline: nginx-mainline-')
        assert f"and nginx-{before['nginx']} are in conflict; unresolvable package conflicts detected" in conflict['reason'] and installed() == before, conflict
        mark('a dryrun of a package that conflicts with an installed one says it would install it, because --print checks no conflict; '
             'the run fails with the packages in conflict named and changes no package')

        # Removal: a package that another needs is refused unless the task
        # says so.
        is_result(one({'packages': ['nginx-mod-stream']}, 'run'), 'succeeded', 'changed', reason='installed nginx-mod-stream ')
        refused({'packages': ['nginx'], 'state': 'absent'},
                'pacman_package: removing nginx would also remove nginx-mod-stream, which depend on them; '
                'to remove these packages too, add also_remove_packages_that_depend_on_these: true')
        remote('systemctl stop nginx')
        removal = {'packages': ['nginx', 'mailcap'], 'state': 'absent', 'also_remove_packages_that_depend_on_these': True}
        before = installed()
        previewed = is_result(one(removal, 'dryrun'), 'succeeded', 'would_change')
        assert previewed['reason'] == (f"would remove mailcap {before['mailcap']}, nginx {before['nginx']}, nginx-mod-stream {before['nginx-mod-stream']}"), previewed
        assert installed() == before
        removed = is_result(one(removal, 'run'), 'succeeded', 'changed')
        assert removed['reason'] == 'removed' + previewed['reason'].removeprefix('would remove'), removed
        assert not {'nginx', 'nginx-mod-stream', 'mailcap'} & set(installed())
        assert removed['module_outputs']['changed_packages'] == changes(before, installed())
        for command in ('run', 'dryrun'):
            is_result(one(removal, command, 0), 'succeeded', 'unchanged')
        mark('removing a package that another needs fails in a dryrun and a run and changes nothing; with also_remove_packages_that_depend_on_these, '
             'a dryrun names all three packages and a run removes them; then the removal is unchanged')

        # Names that the task cannot install.
        missing = 'mmx-no-such-package'
        is_result(one({'packages': [missing]}, 'dryrun', 3), 'not_checked', 'unknown',
                  reason=f"pacman_package: the node's package databases have no package {missing}; a run refreshes them first")
        failed = is_result(one({'packages': [missing]}, 'run', 1), 'failed', 'unchanged',
                           reason=f"pacman_package.packages: pacman's package databases have no package {missing}", tries=1)
        assert failed['failure_kind'] == 'config', failed
        not_its_own = 'is a group or a name that another package provides, not a package of its own; name each package itself'
        refreshed = database_changed_at()
        refused({'packages': ['sh']}, 'pacman_package.packages: sh ' + not_its_own)
        assert database_changed_at() == refreshed
        refused({'packages': ['extra/tree']}, 'pacman_package.packages: extra/tree may hold only')
        # Removing a group, or a name that an installed package provides,
        # would remove nothing; pacquery is the one member of the group
        # archlinux-tools.
        is_result(one({'packages': ['pacquery']}, 'run'), 'succeeded', 'changed', reason='installed pacquery ')
        refused({'packages': ['archlinux-tools'], 'state': 'absent'}, 'pacman_package.packages: archlinux-tools ' + not_its_own)
        refused({'packages': ['sh'], 'state': 'absent'}, 'pacman_package.packages: sh ' + not_its_own)
        assert 'pacquery' in installed() and 'bash' in installed()
        is_result(one({'packages': ['pacquery'], 'state': 'absent'}, 'run'), 'succeeded', 'changed', reason='removed pacquery ')
        mark('a name that no repository has is not checked in a dryrun and fails a run once after the refresh; '
             'a name that another package provides fails before a refresh, and a repository in front of the name fails too; '
             'removing the group archlinux-tools while pacquery is installed, or the provided name sh, fails and removes nothing')

        # pacman's lock: the task waits while another pacman holds it.
        remote(f'touch {LOCK_FILE}\nsystemd-run --quiet --on-active=5 rm -f {LOCK_FILE}')
        started = time.monotonic()
        is_result(one({'packages': ['tree']}, 'run'), 'succeeded', 'changed', reason='installed tree ', tries=1)
        waited = time.monotonic() - started
        assert waited >= 4 and 'tree' in installed(), waited
        # A lock that stays, as a pacman that was stopped leaves it.
        remote(f'touch {LOCK_FILE}')
        project([{'id': 'packages', 'retry': False, 'pacman_package': {'packages': ['tree'], 'state': 'absent'}}])
        stale = is_result(results(run('run', 1))['packages'], 'failed', 'unchanged',
                          reason=f'pacman_package: another pacman has held its lock {LOCK_FILE} for 60 seconds')
        assert 'tree' in installed(), stale
        remote(f'rm {LOCK_FILE}')
        is_result(one({'packages': ['tree'], 'state': 'absent'}, 'run'), 'succeeded', 'changed', reason='removed tree ')
        mark(f'a task waits while pacman\'s lock is held and installs when it goes ({waited:.0f} s); a lock that stays fails the task after 60 seconds, '
             'and once the file is removed the task removes the package')
    finally:
        remote(f'rm -f {LOCK_FILE}\nsystemctl stop nginx 2>/dev/null || true\nsystemctl reset-failed 2>/dev/null || true')
        now = installed()
        for package, version in faked.items():
            if package in now and now[package] != version:
                fake_installed_version(package, version)
        added = sorted(set(installed()) - packages_before)
        if added:
            remote('pacman -R --noconfirm ' + ' '.join(added))
    assert set(installed()) == packages_before
    (workspace / 'summary.json').write_text(json.dumps(passed, indent=2) + '\n')
    print(f'{len(passed)} pacman_package workflows passed; the guest has the packages it had before, upgraded', flush=True)


if __name__ == '__main__':
    vm.run_suite(main)
