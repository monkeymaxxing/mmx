#!/usr/bin/env python3
"""Exercise accounts, directories, service units, packages, and package sources through the public CLI."""
import base64
import hashlib
import ipaddress
import json
import re
import shlex
import shutil
import tempfile

import vm

# Two versions of matching test packages, one with a conffile, served by a
# local trusted flat apt source. The guest has dpkg-deb but no archive tools.
PIN_FIXTURE = r'''set -eu
fixture=/var/tmp/mmx-pin-fixture
mkdir -p "$fixture/repo"
for version in 1.0 2.0; do
  build="$fixture/build-$version"
  mkdir -p "$build/DEBIAN" "$build/etc"
  printf 'Package: mmx-pin-fixture\nVersion: %s\nArchitecture: all\nMaintainer: MMX tests <tests@example.invalid>\nDescription: Disposable MMX pinned-version fixture\n' "$version" > "$build/DEBIAN/control"
  printf '/etc/mmx-pin-fixture.conf\n' > "$build/DEBIAN/conffiles"
  printf 'packaged %s\n' "$version" > "$build/etc/mmx-pin-fixture.conf"
  dpkg-deb --build "$build" "$fixture/repo/mmx-pin-fixture_${version}_all.deb" > /dev/null
  dependent="$fixture/dependent-$version"
  mkdir -p "$dependent/DEBIAN"
  printf 'Package: mmx-pin-dependent\nVersion: %s\nArchitecture: all\nMaintainer: MMX tests <tests@example.invalid>\nDepends: mmx-pin-fixture (= %s)\nDescription: Matching dependency fixture\n' "$version" "$version" > "$dependent/DEBIAN/control"
  dpkg-deb --build "$dependent" "$fixture/repo/mmx-pin-dependent_${version}_all.deb" > /dev/null
done
for deb in "$fixture"/repo/*.deb; do
  dpkg-deb -f "$deb"
  printf 'Filename: ./%s\nSize: %s\nSHA256: %s\n\n' "${deb##*/}" "$(stat -c %s "$deb")" "$(sha256sum "$deb" | cut -d' ' -f1)"
done > "$fixture/repo/Packages"
printf 'deb [trusted=yes] file:%s/repo ./\n' "$fixture" > /etc/apt/sources.list.d/mmx-pin-fixture.list
'''

# Two versions of another test package in a flat apt repository that apt
# trusts only through its signing key; the controller signs its Release file
# (see sign_source_fixture). The guest's other sources are set aside until
# cleanup, so these cases never use the internet or upgrade the guest's own
# packages. The keyrings directory is removed so a repository creates it.
SOURCE_FIXTURE = r'''set -eu
fixture=/var/tmp/mmx-source-fixture
mkdir -p "$fixture/repo" "$fixture/hidden/sources.list.d"
for version in 1.0 2.0; do
  build="$fixture/build-$version"
  mkdir -p "$build/DEBIAN"
  printf 'Package: mmx-source-fixture\nVersion: %s\nArchitecture: all\nMaintainer: MMX tests <tests@example.invalid>\nDescription: Disposable MMX package-source fixture\n' "$version" > "$build/DEBIAN/control"
  dpkg-deb --build "$build" "$fixture/repo/mmx-source-fixture_${version}_all.deb" > /dev/null
done
cd "$fixture/repo"
for deb in *.deb; do
  dpkg-deb -f "$deb"
  printf 'Filename: ./%s\nSize: %s\nSHA256: %s\n\n' "$deb" "$(stat -c %s "$deb")" "$(sha256sum "$deb" | cut -d' ' -f1)"
done > Packages
printf 'Date: %s\nSHA256:\n %s %s Packages\n' "$(date -Ru)" "$(sha256sum Packages | cut -d' ' -f1)" "$(stat -c %s Packages)" > Release
[ ! -e /etc/apt/sources.list ] || mv /etc/apt/sources.list "$fixture/hidden/"
for source in /etc/apt/sources.list.d/*; do
  case "${source##*/}" in mmx-*) ;; *) mv "$source" "$fixture/hidden/sources.list.d/";; esac
done
rmdir /etc/apt/keyrings 2>/dev/null || true
'''

# Restores the guest's own sources before removing the fixture.
SOURCE_CLEANUP = r'''fixture=/var/tmp/mmx-source-fixture
if [ -d "$fixture/hidden" ]; then
  [ ! -e "$fixture/hidden/sources.list" ] || mv "$fixture/hidden/sources.list" /etc/apt/
  for source in "$fixture"/hidden/sources.list.d/*; do
    [ ! -e "$source" ] || mv "$source" /etc/apt/sources.list.d/
  done
fi
dpkg --purge mmx-source-fixture >/dev/null 2>&1 || true
rm -rf "$fixture" /etc/apt/sources.list.d/mmx-source-fixture.sources /etc/apt/keyrings/mmx-source-fixture.* /var/lib/apt/lists/*mmx-source-fixture*
[ ! -d /etc/apt ] || mkdir -p /etc/apt/keyrings
'''


def main():
    vm.up()
    node, ssh = vm.connection()
    vm.reset()
    workspace = vm.ROOT / 'server-resources'
    if workspace.exists():
        shutil.rmtree(workspace)
    example = vm.REPO / 'examples/service-app'
    shutil.copytree(example, workspace)
    vm.write_inventory(workspace, {'nodes': {'app': node}}, name='example')
    vm.build()
    mmx = vm.ROOT / 'bin/mmx'
    passed = []
    stamp = hashlib.sha256(b'mmx-worker.service').hexdigest()
    project = workspace / 'playbooks/worker.yml'
    original = project.read_text()
    role = workspace / 'roles/worker/main.yml'
    original_role = role.read_text()
    unit = workspace / 'roles/worker/files/worker.service'
    original_unit = unit.read_text()

    def remote(script):
        return vm.cmd([*ssh, 'sudo -n /bin/sh -c ' + shlex.quote(script)]).stdout.strip()

    def run(command, expected=None, connection=None):
        arguments = [mmx, command, *vm.playbook_args(command, "worker"), '--json']
        if connection:
            arguments += ['--connection', connection]
        result = vm.cmd(arguments, cwd=workspace, check=False)
        assert result.returncode in (vm.success_codes(command) if expected is None else (expected,)), (command, result.stdout, result.stderr)
        return json.loads(result.stdout)

    def tasks(items):
        vm.write_project(workspace, playbook_name='worker', **{'project_name': 'server_resources', 'default_inventory': 'example',
            'fleet_stages': [{'id': 'configure', 'nodes': 'all', 'node_tasks': items}]})

    def results(record):
        return record['entries'][0]['response']['results']

    def invocation():
        return remote('systemctl show -p InvocationID --value mmx-worker.service')

    def actions():
        return remote('cat /srv/mmx-worker/actions')

    def mark(name):
        passed.append(name)
        print('PASS ' + name, flush=True)

    def sign_source_fixture():
        """Clearsign the fixture's Release with a throwaway key; return that key."""
        release = vm.cmd([*ssh, 'cat /var/tmp/mmx-source-fixture/repo/Release']).stdout
        with tempfile.TemporaryDirectory(dir=vm.ROOT) as home:
            gpg = ['gpg', '--homedir', home, '--batch', '--pinentry-mode', 'loopback', '--passphrase', '']
            try:
                vm.cmd([*gpg, '--quick-gen-key', 'MMX test repository', 'ed25519', 'sign', 'never'])
                signed = vm.cmd([*gpg, '--clearsign'], input=release).stdout
                key = vm.cmd([*gpg, '--armor', '--export']).stdout
            finally:
                vm.cmd(['gpgconf', '--homedir', home, '--kill', 'gpg-agent'], check=False)
        vm.cmd([*ssh, 'sudo -n /bin/sh -c ' + shlex.quote('cat > /var/tmp/mmx-source-fixture/repo/InRelease')], input=signed)
        return key

    def cleanup():
        remote("systemctl disable --now mmx-worker.service 2>/dev/null || true; "
               "systemctl reset-failed mmx-worker.service 2>/dev/null || true; "
               "systemctl disable --now mmx-demo@1.service mmx-demo-main.service 2>/dev/null || true; "
               "rm -rf /etc/systemd/system/mmx-demo* /var/tmp/mmx-demo; "
               "rm -f /etc/systemd/system/mmx-worker.service /var/tmp/mmx-worker-fail; systemctl daemon-reload; "
               "userdel mmx-worker 2>/dev/null || true; userdel mmx-worker-tmp 2>/dev/null || true; "
               "for group in mmx-worker mmx-worker-extra mmx-worker-keep; do groupdel \"$group\" 2>/dev/null || true; done; "
               "rm -rf /srv/mmx-worker; "
               "dpkg --purge mmx-pin-dependent mmx-pin-fixture nginx nginx-common >/dev/null 2>&1 || true; "
               "rm -f /etc/apt/preferences.d/mmx-* /etc/apt/auth.conf.d/mmx-* /etc/apt/sources.list.d/mmx-login-fixture.*; "
               "rm -rf /etc/apt/sources.list.d/mmx-pin-fixture.list /var/lib/apt/lists/*mmx-pin-fixture* /var/tmp/mmx-pin-fixture; "
               + vm.UFW_RESET)
        remote(SOURCE_CLEANUP)

    cleanup()
    try:
        run('validate')
        preview = run('dryrun')
        remote('! getent passwd mmx-worker; ! getent group mmx-worker; test ! -d /srv/mmx-worker; test ! -e /etc/systemd/system/mmx-worker.service')
        assert any(r['status'] == 'not_checked' for r in results(preview))
        mark('fresh preview leaves accounts, directory, and unit absent and reports pending prerequisites')

        run('run')
        assert remote('cat /srv/mmx-worker/loaded') == 'first: Hello from MMX'
        assert remote("stat -c '%a %U %G' /srv/mmx-worker") == '750 mmx-worker mmx-worker'
        assert remote("getent passwd mmx-worker | cut -d: -f6,7") == '/srv/mmx-worker:/usr/sbin/nologin'
        assert remote('systemctl is-enabled mmx-worker.service') == 'enabled'
        assert remote('systemctl show -p User --value mmx-worker.service') == 'mmx-worker'
        assert remote('systemctl show -p NeedDaemonReload --value mmx-worker.service') == 'no'
        mark('application example creates its account and directory, installs the unit, starts as that account, and verifies loaded configuration')

        before = invocation(), actions(), remote("journalctl -b _PID=1 --grep '^Reloading' --no-pager -o cat | wc -l")
        for forget_record in (False, True):
            if forget_record:
                remote(f'rm /run/mmx/services/{stamp}.json')
            unchanged = run('run')
            assert unchanged['entries'][0]['response']['change'] == 'unchanged', unchanged
            assert (invocation(), actions(), remote("journalctl -b _PID=1 --grep '^Reloading' --no-pager -o cat | wc -l")) == before
        mark('unchanged rerun neither reloads the systemd manager nor refreshes the application')

        project.write_text(original.replace('Hello from MMX', 'Updated message'))
        run('run')
        assert invocation() == before[0]
        assert len(actions().splitlines()) == len(before[1].splitlines()) + 1
        assert remote('cat /srv/mmx-worker/loaded') == 'first: Updated message'
        mark('configuration content reloads once without restarting the worker')

        project.write_text(original.replace('label: first', 'label: second'))
        previous = invocation()
        run('run')
        assert invocation() != previous
        assert remote('cat /srv/mmx-worker/loaded') == 'second: Hello from MMX'
        mark('unit environment change reloads the manager and restarts despite refresh: reload')

        project.write_text(original.replace('label: first', 'label: third'))
        role.write_text(original_role.replace('      activate:', '        - shell:\n            script: test ! -e /var/tmp/mmx-worker-fail\n            changed_when: false\n      activate:'))
        remote('touch /var/tmp/mmx-worker-fail')
        previous = invocation()
        run('run', 1)
        assert invocation() == previous
        assert 'WORKER_LABEL=third' in remote('cat /etc/systemd/system/mmx-worker.service')
        remote('rm /var/tmp/mmx-worker-fail')
        run('resume')
        assert invocation() != previous
        assert remote('cat /srv/mmx-worker/loaded') == 'third: Hello from MMX'
        role.write_text(original_role)
        mark('resume restarts after a failure between unit installation and activation even though the unit file is now unchanged')

        previous, installed = invocation(), remote('sha256sum /etc/systemd/system/mmx-worker.service')
        unit.write_text(original_unit.replace('/bin/bash /srv/mmx-worker/worker.sh', '/mmx-missing-executable'))
        run('run', 1)
        assert invocation() == previous and remote('sha256sum /etc/systemd/system/mmx-worker.service') == installed
        unit.write_text(original_unit)
        mark('invalid unit candidate preserves the installed definition and running worker')

        remote('systemctl reset-failed mmx-worker.service')
        # The unit file already has its desired contents but systemd has not read them.
        project.write_text(original.replace('label: first', 'label: fifth'))
        content = original_unit.replace('{{ .inputs.label }}', 'fifth')
        remote('printf %s ' + shlex.quote(content) + ' > /etc/systemd/system/.mmx-worker-test; mv /etc/systemd/system/.mmx-worker-test /etc/systemd/system/mmx-worker.service')
        assert remote('systemctl show -p NeedDaemonReload --value mmx-worker.service') == 'yes'
        run('run')
        assert remote('systemctl show -p NeedDaemonReload --value mmx-worker.service') == 'no'
        assert remote('cat /srv/mmx-worker/loaded') == 'fifth: Hello from MMX'
        mark('a pending unit definition gets its manager reload and restart on the next run')

        def two_fleet_stages(extra):
            project.write_text(original.replace('label: first', 'label: fifth') + (
                '  - id: extra\n    nodes: all\n    node_tasks:\n      - task_block:\n          id: extra\n          changes:\n'
                f'            - file: {{permissions_mode: "0644", path: /srv/mmx-worker/extra, content: "{extra}\\n"}}\n'
                '          activate:\n            - systemd_service: {name: mmx-worker, refresh: reload, state: running}\n'))

        def reloads():
            return len(actions().splitlines())

        two_fleet_stages('one')
        previous, count = invocation(), reloads()
        first = run('run')
        assert [e['response']['change'] for e in first['entries']] == ['unchanged', 'changed'], first
        assert (invocation(), reloads()) == (previous, count + 1)
        for forget_record in (False, True):
            if forget_record:
                remote(f'rm /run/mmx/services/{stamp}.json')
            unchanged = run('run')
            assert [e['response']['change'] for e in unchanged['entries']] == ['unchanged', 'unchanged'], unchanged
            assert (invocation(), reloads()) == (previous, count + 1)
        two_fleet_stages('two')
        run('run')
        assert (invocation(), reloads()) == (previous, count + 2)
        assert remote('cat /srv/mmx-worker/loaded') == 'fifth: Hello from MMX'
        mark('task blocks in two fleet stages keep separate inputs for one service: each reloads once for its own change and reruns reload nothing')

        # Each start of a demo service appends the MMX_DEMO value its process received.
        def demo(service, units):
            tasks([{
                "task_block": {"id": "demo", "changes": [{"systemd_unit": u} for u in units], "activate": [{"systemd_service": {"name": service, "start_at_boot": True, "state": "running"}}]}
            }])

        def demo_base(name, log, install=''):
            return {'name': name, 'content': '[Service]\nEnvironment=MMX_DEMO=base\n'
                    f"ExecStart=/bin/sh -c 'echo $${{MMX_DEMO}} >> /var/tmp/mmx-demo/{log}; exec sleep infinity'\n"
                    f'[Install]\nWantedBy=multi-user.target\n{install}'}

        def demo_drop_in(name, value):
            return {'name': name, 'drop_in': '10-x.conf', 'content': f'[Service]\nEnvironment=MMX_DEMO={value}\n'}

        def demo_state(unit, log):
            return remote(f'systemctl show -p InvocationID --value {unit}'), remote(f'cat /var/tmp/mmx-demo/{log}')

        remote('mkdir -p /var/tmp/mmx-demo')
        template = demo_base('mmx-demo@.service', 'instance')
        demo('mmx-demo@1', [template, demo_drop_in('mmx-demo@.service', 'one')])
        run('run')
        started = demo_state('mmx-demo@1.service', 'instance')
        assert started[1] == 'one'
        assert '/etc/systemd/system/mmx-demo@.service.d/10-x.conf' in remote('systemctl show -p DropInPaths --value mmx-demo@1.service').split()
        demo('mmx-demo@1', [template, demo_drop_in('mmx-demo@.service', 'two')])
        run('run')
        restarted = demo_state('mmx-demo@1.service', 'instance')
        assert restarted[0] != started[0] and restarted[1] == 'one\ntwo'
        assert remote('systemctl show -p NeedDaemonReload --value mmx-demo@1.service') == 'no'
        assert run('run')['entries'][0]['response']['change'] == 'unchanged'
        assert run('dryrun', 0)['entries'][0]['response']['change'] == 'unchanged'
        assert demo_state('mmx-demo@1.service', 'instance') == restarted
        mark('template drop-in restarts its running instance once when changed, then reruns and previews unchanged')

        # Enabling links the Alias= name; systemd then also reads drop-ins under it.
        main = demo_base('mmx-demo-main.service', 'main', 'Alias=mmx-demo-alias.service\n')
        demo('mmx-demo-main', [main])
        run('run')
        assert remote('systemctl show -p Names --value mmx-demo-main.service') == 'mmx-demo-main.service mmx-demo-alias.service'
        started = demo_state('mmx-demo-main.service', 'main')
        assert started[1] == 'base'
        demo('mmx-demo-main', [main, demo_drop_in('mmx-demo-alias.service', 'one')])
        run('run')
        assert '/etc/systemd/system/mmx-demo-alias.service.d/10-x.conf' in remote('systemctl show -p DropInPaths --value mmx-demo-main.service').split()
        restarted = demo_state('mmx-demo-main.service', 'main')
        assert restarted[0] != started[0] and restarted[1] == 'base\none'
        demo('mmx-demo-alias', [main, demo_drop_in('mmx-demo-alias.service', 'one')])
        assert run('run')['entries'][0]['response']['change'] == 'unchanged'
        assert demo_state('mmx-demo-main.service', 'main') == restarted
        demo('mmx-demo-alias', [main, demo_drop_in('mmx-demo-alias.service', 'two')])
        run('run')
        changed = demo_state('mmx-demo-main.service', 'main')
        assert changed[0] != restarted[0] and changed[1] == 'base\none\ntwo'
        assert run('run')['entries'][0]['response']['change'] == 'unchanged'
        assert demo_state('mmx-demo-main.service', 'main') == changed
        mark('drop-in under an enabled alias belongs to the canonical service: one restart per change, unchanged through either name')

        remote('groupadd --system mmx-worker-keep; usermod -aG mmx-worker-keep mmx-worker')
        tasks([{'loop': ['mmx-worker-extra'], 'as': 'group_name', 'group': {'name': '{{ .group_name }}', 'system': True}},
               {'loop': ['mmx-worker'], 'as': 'account_name', 'user': {'create_home_directory': False, 'name': '{{ .account_name }}', 'supplementary_groups': ['mmx-worker-extra'], 'supplementary_groups_not_listed': 'keep'}}])
        run('run')
        groups = remote('id -nG mmx-worker').split()
        assert 'mmx-worker-keep' in groups and 'mmx-worker-extra' in groups
        assert run('run')['entries'][0]['response']['change'] == 'unchanged'
        mark('looped account tasks preserve existing memberships during supplementary_groups_not_listed: keep updates and become unchanged')

        # mmx-worker's login shell is nologin: the switch uses neither it nor sudo.
        tasks([{'shell': {'run_as_user_account': 'mmx-worker', 'run_from_directory': '/srv/mmx-worker', 'env': {'APP_MODE': 'production'},
                          'run_only_if_this_command_succeeds': 'test "$(id -un)" = mmx-worker', 'dryrun': 'id -un', 'changed_when': False,
                          'script': 'id -un; id -Gn; echo "$HOME $USER $LOGNAME $APP_MODE"; pwd; touch created-by-app'}}])
        assert results(run('dryrun'))[0]['stdout'] == 'mmx-worker\n'
        remote('test ! -e /srv/mmx-worker/created-by-app')
        lines = results(run('run'))[0]['stdout'].splitlines()
        assert lines[0] == 'mmx-worker' and {'mmx-worker', 'mmx-worker-extra', 'mmx-worker-keep'} <= set(lines[1].split()), lines
        assert lines[2:] == ['/srv/mmx-worker mmx-worker mmx-worker production', '/srv/mmx-worker'], lines
        assert remote("stat -c '%U %G' /srv/mmx-worker/created-by-app") == 'mmx-worker mmx-worker'
        adhoc = vm.cmd([mmx, 'run-command', '--json', '--nodes', 'app', '--run-as-node-user-account', 'mmx-worker', '--', 'id -un; echo $HOME'], cwd=workspace, check=False)
        assert adhoc.returncode == 0, adhoc.stderr
        assert results(json.loads(adhoc.stdout))[0]['stdout'] == 'mmx-worker\n/srv/mmx-worker\n'
        mark('shell.run_as_user_account runs the guard, dryrun command, and script as the account with its groups, HOME, and run_from_directory; mmx run-command --run-as-node-user-account does the same')

        tasks([
            {'shell': {'run_from_directory': '/srv/mmx-worker',
                       'skip_if_this_path_exists': 'created-by-app', 'script': 'exit 7'}},
            {'shell': {'run_from_directory': '/srv/mmx-worker',
                       'run_only_if_this_path_exists': 'created-by-app', 'script': 'printf guard-passed',
                       'dryrun': 'printf guard-passed'}},
            {'shell': {'run_from_directory': '/srv/mmx-worker',
                       'run_only_if_this_path_exists': 'missing-path', 'script': 'exit 7'}},
        ])
        for command in ['dryrun', 'run']:
            guarded = results(run(command))
            assert [r['status'] for r in guarded] == ['skipped', 'succeeded', 'skipped'], guarded
            assert guarded[1]['stdout'] == 'guard-passed', guarded
        mark('relative shell path guards inspect run_from_directory in runs and dryruns')

        tasks([{'user': {'create_home_directory': False, 'name': 'mmx-worker-tmp', 'system': True}},
               {'shell': {'run_as_user_account': 'mmx-worker-tmp', 'script': 'id -un', 'dryrun': 'id -un'}}])
        assert results(run('dryrun'))[1]['status'] == 'not_checked'
        assert results(run('run'))[1]['stdout'] == 'mmx-worker-tmp\n'
        tasks([{'shell': {'run_as_user_account': 'mmx-missing-account', 'script': 'touch /var/tmp/mmx-missing-account'}}])
        missing = results(run('run', 1))[0]
        assert missing['status'] == 'failed' and 'shell.run_as_user_account' in missing['reason'], missing
        remote('test ! -e /var/tmp/mmx-missing-account')
        mark('shell.run_as_user_account finds an account created earlier in the job; dryrun reports it not checked, and a missing account fails')

        tasks([{'user': {'create_home_directory': False, 'name': 'mmx-worker', 'supplementary_groups': ['mmx-worker-extra'], 'supplementary_groups_not_listed': 'remove'}}])
        run('run')
        groups = remote('id -nG mmx-worker').split()
        assert 'mmx-worker-extra' in groups and 'mmx-worker-keep' not in groups
        tasks([{'user': {'create_home_directory': False, 'name': 'mmx-worker', 'supplementary_groups': [], 'supplementary_groups_not_listed': 'remove'}},
               {'shell': {'script': 'test ! -e /var/tmp/mmx-worker-fail'}}])
        remote('touch /var/tmp/mmx-worker-fail')
        run('run', 1)
        assert remote('id -nG mmx-worker') == 'mmx-worker'
        remote('rm /var/tmp/mmx-worker-fail')
        resumed = run('resume')
        assert results(resumed)[0]['change'] == 'unchanged'
        mark('explicit replacement and empty groups survive saved resume without restoring removed memberships')

        remote('printf keep > /srv/mmx-worker/keep')
        before = remote("stat -c '%i %s %Y' /srv/mmx-worker/keep; cat /srv/mmx-worker/keep")
        tasks([{'directory': {'path': '/srv/mmx-worker', 'permissions_mode': '0700'}}])
        run('dryrun')
        assert remote('stat -c %a /srv/mmx-worker') == '750'
        run('run')
        assert remote('stat -c %a /srv/mmx-worker') == '700'
        assert remote("stat -c '%i %s %Y' /srv/mmx-worker/keep; cat /srv/mmx-worker/keep") == before
        mark('directory metadata changes preserve children and dryrun leaves permissions alone')

        def apt_and_ufw_workflows():
            # Run the repository's project with only its inventory replaced.
            with tempfile.TemporaryDirectory(prefix='security-updates-', dir=vm.ROOT) as temporary:
                security_example = shutil.copytree(vm.REPO / 'examples/security-updates-on-debian-and-ubuntu',
                                                   temporary, dirs_exist_ok=True)
                vm.write_inventory(security_example, {'nodes': {'server01': node}}, name='example')

                def security_updates(command, playbook):
                    # A run applies every security update that the test
                    # machine's image lacks, so its time grows with the
                    # image's age: the whole suite took 28 minutes on a
                    # two-core ARM64 server with Ubuntu 26.04, nearly all of
                    # it in this run.
                    result = vm.cmd([mmx, command, playbook, '--json'], cwd=security_example, check=False, timeout=3600)
                    assert result.returncode in vm.success_codes(command), (command, playbook, result.stdout, result.stderr)
                    return json.loads(result.stdout)

                security_updates('run', 'set_up_automatic_security_updates')
                assert remote("dpkg-query --show --showformat='${db:Status-Status}' unattended-upgrades") == 'installed'
                configuration = remote('apt-config dump')
                for setting in ['APT::Periodic::Update-Package-Lists "1";',
                                'APT::Periodic::Unattended-Upgrade "1";',
                                'Unattended-Upgrade::Automatic-Reboot "false";',
                                'Unattended-Upgrade::Remove-Unused-Dependencies "true";']:
                    assert setting in configuration, configuration
                for timer in ('apt-daily.timer', 'apt-daily-upgrade.timer'):
                    assert remote('systemctl is-enabled ' + timer) == 'enabled'
                    assert remote('systemctl is-active ' + timer) == 'active'
                allowed_origins_before = remote('sha256sum /etc/apt/apt.conf.d/50unattended-upgrades')
                rerun = security_updates('run', 'set_up_automatic_security_updates')
                assert all(result['change'] == 'unchanged' for result in results(rerun)), rerun
                assert remote('sha256sum /etc/apt/apt.conf.d/50unattended-upgrades') == allowed_origins_before
                security_updates('dryrun', 'apply_security_updates_now')
                security_updates('run', 'apply_security_updates_now')
            mark('the security-updates-on-debian-and-ubuntu example configures daily security updates and both timers, reruns unchanged, and applies updates in dryrun and run')

            # The harness reaches the guest over SSH and its endpoint through an SSH
            # tunnel to loopback, so every enabled firewall here allows port 22 and
            # cleanup resets ufw. ufw is installed by the playbook where missing.
            def firewall(ufw):
                tasks([{'apt_package': {'packages': [{'name': 'ufw', 'version': 'newest_when_first_installed'}]}}, {'ufw': ufw}])

            def ufw_rules():
                return remote('ufw show added').splitlines()[1:]

            def ufw_result(command, expected=None, connection=None):
                return results(run(command, expected, connection))[1]

            ssh_rule = {'action': 'allow', 'port': 22, 'protocol': 'tcp'}
            firewall({'default_incoming': 'deny', 'enabled': True, 'incoming_rules': [
                ssh_rule, {'action': 'limit', 'port': 2222, 'protocol': 'tcp'},
                {'action': 'allow', 'port': 5432, 'protocol': 'tcp', 'from': '10.0.0.0/8'},
                {'action': 'deny', 'port': '6000:6010', 'protocol': 'udp'}], 'incoming_rules_not_listed': 'keep'})
            run('run')
            assert ufw_rules() == ['ufw allow 22/tcp', 'ufw limit 2222/tcp', 'ufw allow from 10.0.0.0/8 to any port 5432 proto tcp', 'ufw deny 6000:6010/udp']
            status = remote('ufw status verbose')
            assert 'Status: active' in status and 'Default: deny (incoming), allow (outgoing)' in status, status
            assert remote('grep ^ENABLED= /etc/ufw/ufw.conf; systemctl is-enabled ufw') == 'ENABLED=yes\nenabled'
            assert ufw_result('run')['change'] == 'unchanged'
            assert ufw_result('dryrun', 0)['change'] == 'unchanged'
            mark('ufw applies incoming rules in order, the default policy, and enables the firewall now and at boot; reruns and previews are unchanged')

            firewall({'logging_level': 'low'})
            run('run')
            assert ufw_result('run')['change'] == 'unchanged'
            firewall({'logging_level': 'medium'})
            assert ufw_result('run')['value'] == {'logging_level': 'medium'}
            assert 'Logging: on (medium)' in remote('ufw status verbose')
            mark('ufw logging_level sets the level, reruns unchanged, and changes it')

            remote('ufw allow 9999/tcp')
            firewall({'incoming_rules': [{'action': 'allow', 'port': 2222, 'protocol': 'tcp'}], 'incoming_rules_not_listed': 'keep'})
            assert ufw_result('dryrun')['value'] == {'updated': ['allow 2222/tcp']}
            assert ufw_result('run')['value'] == {'updated': ['allow 2222/tcp']}
            assert ufw_rules() == ['ufw allow 22/tcp', 'ufw allow 2222/tcp', 'ufw allow from 10.0.0.0/8 to any port 5432 proto tcp', 'ufw deny 6000:6010/udp', 'ufw allow 9999/tcp']
            firewall({'incoming_rules_not_listed': 'remove', 'incoming_rules': [ssh_rule, {'action': 'allow', 'port': 443, 'protocol': 'tcp'}]})
            preview = ufw_result('dryrun')
            assert preview['value'] == {'added': ['allow 443/tcp'], 'removed': [
                'allow 2222/tcp', 'allow from 10.0.0.0/8 to any port 5432 proto tcp', 'deny 6000:6010/udp', 'allow 9999/tcp']}, preview
            assert len(ufw_rules()) == 5
            assert ufw_result('run')['value'] == preview['value']
            assert ufw_rules() == ['ufw allow 22/tcp', 'ufw allow 443/tcp']
            assert ufw_result('run')['change'] == 'unchanged'
            firewall({'state': 'absent', 'incoming_rules': [{'port': 443, 'protocol': 'tcp'}]})
            assert ufw_result('run')['value'] == {'removed': ['allow 443/tcp']}
            assert ufw_rules() == ['ufw allow 22/tcp']
            mark('ufw replaces a rule action in place, incoming_rules_not_listed: keep keeps unlisted rules, remove removes them, and absent removes listed rules')

            firewall({'incoming_rules_not_listed': 'remove', 'incoming_rules': [
                {'action': 'allow', 'port': 443, 'protocol': 'tcp'}, ssh_rule,
                {'action': 'allow', 'port': '8000:8010', 'protocol': 'tcp_and_udp'}]})
            preview = ufw_result('dryrun')
            assert preview['change'] == 'would_change'
            assert ufw_result('run')['value'] == preview['value']
            assert ufw_rules() == ['ufw allow 443/tcp', 'ufw allow 22/tcp',
                                   'ufw allow 8000:8010/tcp', 'ufw allow 8000:8010/udp']
            assert ufw_result('run')['change'] == 'unchanged'
            firewall({'incoming_rules_not_listed': 'remove', 'incoming_rules': [ssh_rule]})
            ufw_result('run')
            mark('an owned incoming list follows page order, including moving SSH without losing access, and tcp_and_udp expands a range to both protocols')

            firewall({'incoming_rules_not_listed': 'remove', 'incoming_rules': [
                {'action': 'deny', 'from': '2001:db8::/32', 'port': 443, 'protocol': 'tcp'},
                ssh_rule, {'action': 'allow', 'port': 443, 'protocol': 'tcp'}]})
            ufw_result('run')
            ipv6_rules = remote('ip6tables -S ufw6-user-input')
            assert ipv6_rules.index('-s 2001:db8::/32') < ipv6_rules.index('--dport 443 -j ACCEPT'), ipv6_rules
            assert ufw_result('run')['change'] == 'unchanged'
            firewall({'incoming_rules_not_listed': 'remove', 'incoming_rules': [ssh_rule]})
            ufw_result('run')
            mark('an IPv6-only rule follows page order relative to dual-family rules, although ufw show added groups it last')

            for refused in [{'incoming_rules_not_listed': 'remove', 'incoming_rules': [{'action': 'allow', 'port': 443, 'protocol': 'tcp'}]},
                            {'state': 'absent', 'incoming_rules': [{'port': 22, 'protocol': 'tcp'}]},
                            # A rule that admits a source on every port names no port.
                            {'incoming_rules_not_listed': 'remove', 'incoming_rules': [{'action': 'allow', 'from': '192.0.2.50', 'protocol': 'tcp'}, {'action': 'allow', 'port': 443, 'protocol': 'tcp'}]},
                            # ufw changes the rule in place, so the deny would come first.
                            {'incoming_rules_not_listed': 'keep', 'incoming_rules': [{'action': 'deny', 'port': 22, 'protocol': 'tcp'}]}]:
                firewall(refused)
                for command in ['dryrun', 'run']:
                    result = ufw_result(command, 1)
                    assert result['failure_kind'] == 'config' and "sshd's TCP port 22" in result['reason'], result
                assert ufw_rules() == ['ufw allow 22/tcp']
            firewall({'enabled': False})
            run('run')
            firewall({'incoming_rules': [], 'incoming_rules_not_listed': 'remove', 'enabled': True})
            assert "sshd's TCP port 22" in ufw_result('run', 1)['reason']
            assert remote('ufw status') == 'Status: inactive' and ufw_rules() == ['ufw allow 22/tcp']
            mark('the ufw SSH safeguard refuses, in dryrun too, any change leaving the enabled firewall without a rule that names the SSH port, or with a deny for it first, and changes nothing')

            # HTTPS reaches 127.0.0.1 through an SSH port forward, not the
            # guest's inventory address. Use --connection ssh_only so the
            # safeguard knows the controller machine's SSH source address.
            ssh_connection = vm.cmd([*ssh, 'printf "%s\\n" "$SSH_CONNECTION"']).stdout.split()
            assert len(ssh_connection) == 4, ssh_connection
            controller_address = str(ipaddress.ip_address(ssh_connection[0]))
            inventory_address = ipaddress.ip_address(node['address'])
            interfaces = json.loads(remote('ip -json address show'))
            main_interface = next(interface['ifname'] for interface in interfaces
                                  if any(ipaddress.ip_address(address['local']) == inventory_address
                                         for address in interface['addr_info']))
            other_interface = next(interface['ifname'] for interface in interfaces
                                   if interface['ifname'] != main_interface and interface['ifname'] == 'lo')
            other_address = '192.0.2.99' if ipaddress.ip_address(controller_address).version == 4 else '2001:db8::99'
            if other_address == controller_address:
                other_address = '192.0.2.98' if ipaddress.ip_address(controller_address).version == 4 else '2001:db8::98'
            restricted_ssh = dict(ssh_rule, incoming_interface=main_interface, **{'from': controller_address})
            firewall({'default_incoming': 'deny', 'enabled': True, 'incoming_rules_not_listed': 'remove',
                      'incoming_rules': [restricted_ssh]})
            assert ufw_result('dryrun', connection='ssh_only')['change'] == 'would_change'
            assert ufw_result('run', connection='ssh_only')['change'] == 'changed'
            assert ufw_rules() == [f'ufw allow in on {main_interface} from {controller_address} to any port 22 proto tcp']
            assert ufw_result('run', connection='ssh_only')['change'] == 'unchanged'
            try:
                for rule, error in [
                    (dict(restricted_ssh, incoming_interface=other_interface), 'which does not hold'),
                    (dict(restricted_ssh, **{'from': other_address}), 'only from addresses that do not include'),
                ]:
                    firewall({'default_incoming': 'deny', 'enabled': True, 'incoming_rules_not_listed': 'remove',
                              'incoming_rules': [rule]})
                    before = ufw_rules()
                    for command in ['dryrun', 'run']:
                        result = ufw_result(command, 1, connection='ssh_only')
                        assert result['failure_kind'] == 'config' and error in result['reason'], result
                        assert f'from: {controller_address}' in result['reason'], result
                        assert '{action: allow, port: 22, protocol: tcp' in result['reason'], result
                        assert ufw_rules() == before
            finally:
                # Restore the disabled firewall and broad SSH rule that the
                # following existing ufw cases expect.
                remote('ufw --force disable')
                firewall({'enabled': False, 'incoming_rules_not_listed': 'remove', 'incoming_rules': [ssh_rule]})
                ufw_result('run', connection='ssh_only')
            mark('the ufw SSH safeguard admits the controller source on the inventory interface, refuses another interface or source in run and dryrun, and changes nothing on refusal')

            # Two application servers MMX never connects to give their private
            # addresses in their vars; only the test node runs the tasks.
            def group_firewall(ufw, app_servers, in_role=False):
                vm.write_inventory(workspace, {'nodes': {
                    'app': node, 'app01': {'address': '192.0.2.11', 'vars': {'private_address': '10.0.1.11'}},
                    'app02': {'address': '192.0.2.12', 'vars': {'private_address': '10.0.1.12'}}, 'cache01': {'address': '192.0.2.21'}},
                    'groups': {'app_servers': {'nodes': app_servers}, 'cache': {'nodes': ['cache01']}}}, name='example')
                task = {'ufw': ufw}
                if in_role:
                    (workspace / 'roles/firewall').mkdir(exist_ok=True)
                    (workspace / 'roles/firewall/main.yml').write_text(json.dumps({'inputs': {}, 'node_tasks': [task]}))
                    task = {'role': 'firewall'}
                vm.write_project(workspace, playbook_name='worker', project_name='server_resources', default_inventory='example',
                                 fleet_stages=[{'id': 'configure', 'nodes': 'app', 'node_tasks': [{'apt_package': {'packages': [{'name': 'ufw', 'version': 'newest_when_first_installed'}]}}, task]}])

            firewall({'incoming_rules_not_listed': 'keep', 'incoming_rules': [{'action': 'allow', 'from': '192.0.2.0/24', 'protocol': 'tcp'},
                                                            {'action': 'deny', 'from': '198.51.100.7', 'protocol': 'udp'}]})
            assert ufw_result('run')['value'] == {'added': ['allow from 192.0.2.0/24 proto tcp', 'deny from 198.51.100.7 proto udp']}
            assert ufw_rules() == ['ufw allow 22/tcp', 'ufw allow from 192.0.2.0/24 proto tcp', 'ufw deny from 198.51.100.7 proto udp']
            assert ufw_result('run')['change'] == 'unchanged' and ufw_result('dryrun', 0)['change'] == 'unchanged'
            mark('a ufw rule with from and no port admits the selected protocol on every port from that address or network, and reruns unchanged')

            # A server whose containers sit behind a bridge: everything they send
            # to the server, and their traffic routed through it. The interface
            # need not exist; ufw matches it by name.
            def routed():
                return [rule for rule in ufw_rules() if rule.startswith('ufw route ')]

            def incoming():
                return [rule for rule in ufw_rules() if not rule.startswith('ufw route ')]

            remote('ufw route allow 8080/tcp')
            bridge = {'default_routed': 'reject',
                      'incoming_rules_not_listed': 'keep', 'incoming_rules': [{'action': 'allow', 'incoming_interface': 'mmxtest0', 'protocol': 'tcp'}],
                      'routed_rules_not_listed': 'remove', 'routed_rules': [
                          {'action': 'allow', 'incoming_interface': 'mmxtest0', 'protocol': 'tcp'}, {'action': 'allow', 'outgoing_interface': 'mmxtest0', 'protocol': 'tcp'},
                          {'action': 'deny', 'incoming_interface': 'mmxtest0', 'outgoing_interface': 'eth0', 'to': '192.0.2.0/24', 'port': 25, 'protocol': 'tcp'}]}
            firewall(bridge)
            routed_rules = ['route allow in on mmxtest0 proto tcp', 'route allow out on mmxtest0 proto tcp', 'route deny in on mmxtest0 out on eth0 to 192.0.2.0/24 port 25 proto tcp']
            preview = ufw_result('dryrun')
            assert preview['value'] == {'added': ['allow in on mmxtest0 proto tcp', *routed_rules], 'removed': ['route allow 8080/tcp'], 'default_routed': 'reject'}, preview
            assert ufw_result('run')['value'] == preview['value']
            assert incoming() == ['ufw allow 22/tcp', 'ufw allow from 192.0.2.0/24 proto tcp', 'ufw deny from 198.51.100.7 proto udp', 'ufw allow in on mmxtest0 proto tcp']
            assert routed() == ['ufw ' + rule for rule in routed_rules]
            assert remote("grep ^DEFAULT_FORWARD_POLICY= /etc/default/ufw") == 'DEFAULT_FORWARD_POLICY="REJECT"'
            assert ufw_result('run')['change'] == 'unchanged' and ufw_result('dryrun', 0)['change'] == 'unchanged'
            # Each kind is managed only when its list is given.
            firewall({'incoming_rules_not_listed': 'remove', 'incoming_rules': [ssh_rule, {'action': 'allow', 'from': '192.0.2.0/24', 'protocol': 'tcp'},
                                                                                {'action': 'deny', 'from': '198.51.100.7', 'protocol': 'udp'}]})
            assert ufw_result('run')['value'] == {'removed': ['allow in on mmxtest0 proto tcp']}
            assert routed() == ['ufw ' + rule for rule in routed_rules]
            firewall({'state': 'absent', 'routed_rules': [{'incoming_interface': 'mmxtest0', 'protocol': 'tcp'}, {'outgoing_interface': 'mmxtest0', 'protocol': 'tcp'}]})
            assert ufw_result('run')['value'] == {'removed': routed_rules[:2]}
            firewall({'routed_rules_not_listed': 'remove', 'routed_rules': [], 'default_routed': 'deny'})
            assert ufw_result('run')['value'] == {'removed': routed_rules[2:], 'default_routed': 'deny'}
            assert routed() == [] and incoming() == ['ufw allow 22/tcp', 'ufw allow from 192.0.2.0/24 proto tcp', 'ufw deny from 198.51.100.7 proto udp']
            mark('ufw interface and routed rules: incoming_interface, both routed interfaces, to, the routed default; each kind removes only its own unlisted rules; absent; reruns and previews unchanged')

            database = {'enabled': True, 'incoming_rules_not_listed': 'remove', 'incoming_rules': [
                ssh_rule, {'action': 'allow', 'port': 5432, 'protocol': 'tcp',
                           'from_inventory_group': {'group': 'app_servers', 'address_var': 'private_address'}}]}
            group_firewall(database, ['app01', 'app02'])
            each = ['allow from 10.0.1.11 to any port 5432 proto tcp', 'allow from 10.0.1.12 to any port 5432 proto tcp']
            preview = ufw_result('dryrun')
            assert preview['value'] == {'added': each, 'removed': ['allow from 192.0.2.0/24 proto tcp', 'deny from 198.51.100.7 proto udp'], 'enabled': True}, preview
            assert remote('ufw status') == 'Status: inactive'
            assert ufw_result('run')['change'] == 'changed'
            assert ufw_rules() == ['ufw allow 22/tcp'] + ['ufw ' + rule for rule in each]
            assert remote('ufw status').startswith('Status: active')
            assert ufw_result('run')['change'] == 'unchanged' and ufw_result('dryrun', 0)['change'] == 'unchanged'
            group_firewall(database, ['app01'])
            assert ufw_result('run')['value'] == {'removed': [each[1]]}
            assert ufw_rules() == ['ufw allow 22/tcp', 'ufw ' + each[0]]
            # The inventory address itself, without a copy in vars, from a role,
            # which reads the inventory its node job carries.
            database['incoming_rules'][1]['from_inventory_group'] = {'group': 'app_servers', 'use_node_address': True}
            group_firewall(database, ['app01'], in_role=True)
            assert ufw_result('run')['value'] == {'added': ['allow from 192.0.2.11 to any port 5432 proto tcp'], 'removed': [each[0]]}
            assert ufw_rules() == ['ufw allow 22/tcp', 'ufw allow from 192.0.2.11 to any port 5432 proto tcp']
            database['incoming_rules'][1]['from_inventory_group'] = {'group': 'app_servers', 'address_var': 'private_address'}
            database['incoming_rules'][1]['from_inventory_group']['group'] = 'cache'
            group_firewall(database, ['app01'])
            refused = vm.cmd([mmx, 'dryrun', 'worker'], cwd=workspace, check=False)
            assert refused.returncode != 0 and 'node cache01 in inventory group cache has no variable private_address' in refused.stderr, (refused.stdout, refused.stderr)
            vm.write_inventory(workspace, {'nodes': {'app': node}}, name='example')
            firewall({'enabled': False})
            run('run')
            mark('from_inventory_group gives one ufw rule for each node of the group, from a variable or the inventory address, also inside a role; a node that leaves the group loses its rule; a node without the address is refused before MMX connects')

            incoming_before = ufw_rules()
            outgoing = [{'action': 'allow', 'port': 53, 'protocol': 'udp'},
                        {'action': 'allow', 'port': 443, 'protocol': 'tcp'},
                        {'action': 'allow', 'to': '10.1.0.0/16', 'protocol': 'tcp'},
                        {'action': 'reject', 'outgoing_interface': 'mmxtest0', 'protocol': 'tcp'}]
            outgoing_rules = ['allow out 53/udp', 'allow out 443/tcp',
                              'allow out to 10.1.0.0/16 proto tcp', 'reject out on mmxtest0 proto tcp']
            firewall({'outgoing_rules': outgoing, 'outgoing_rules_not_listed': 'remove'})
            preview = ufw_result('dryrun')
            assert preview['value'] == {'added': outgoing_rules}, preview
            assert ufw_result('run')['value'] == preview['value']
            assert ufw_rules() == incoming_before + ['ufw ' + rule for rule in outgoing_rules]
            assert ufw_result('run')['change'] == 'unchanged'
            remote('ufw allow out 9999/tcp')
            assert 'ufw allow out 9999/tcp' in ufw_rules()
            assert ufw_result('run')['value'] == {'removed': ['allow out 9999/tcp']}
            assert ufw_rules() == incoming_before + ['ufw ' + rule for rule in outgoing_rules]
            firewall({'state': 'absent', 'outgoing_rules': [{'port': 53, 'protocol': 'udp'}]})
            assert ufw_result('run')['value'] == {'removed': [outgoing_rules[0]]}
            assert ufw_rules() == incoming_before + ['ufw ' + rule for rule in outgoing_rules[1:]]
            mark('ufw outgoing rules: added and rerun unchanged; remove deletes a hand-added outgoing rule and keeps incoming rules; absent removes a listed outgoing rule')

            remote(PIN_FIXTURE)

            def pinned(version, *more, downgrade=False):
                task = {'packages': [{'name': 'mmx-pin-fixture', 'version': version}]}
                if downgrade:
                    task['packages'][0]['allow_downgrade'] = True
                tasks([{'apt_package': task}, *more])

            def package():
                return remote("dpkg-query -W -f='${Version} ${db:Status-Status}' mmx-pin-fixture; echo; cat /etc/mmx-pin-fixture.conf")

            pinned('1.0')
            # The fixture's source is new and its index not yet read: a dryrun,
            # which refreshes nothing, cannot see the version yet.
            assert results(run('dryrun'))[0]['status'] == 'not_checked'
            remote('! dpkg-query -W mmx-pin-fixture')
            run('run')
            assert package() == '1.0 installed\npackaged 1.0'
            pinned('2.0')
            assert results(run('dryrun'))[0]['change'] == 'would_change'
            assert package() == '1.0 installed\npackaged 1.0'
            run('run')
            assert package() == '2.0 installed\npackaged 2.0'
            pinned('1.0')
            assert 'add allow_downgrade: true' in results(run('run', 1))[0]['reason']
            assert package() == '2.0 installed\npackaged 2.0'
            pinned('1.0', downgrade=True)
            run('run')
            assert package() == '1.0 installed\npackaged 1.0'
            assert run('run')['entries'][0]['response']['change'] == 'unchanged'
            assert run('dryrun', 0)['entries'][0]['response']['change'] == 'unchanged'
            mark('pinned package versions preview, install, upgrade, and downgrade only with allow_downgrade from a local apt source, then rerun unchanged')

            managed = {'file': {"permissions_mode": "0644", 'path': '/etc/mmx-pin-fixture.conf', 'content': 'managed by MMX\n'}}
            pinned('1.0', managed, downgrade=True)
            run('run')
            pinned('2.0', managed)
            upgraded = results(run('run'))
            assert [r['change'] for r in upgraded] == ['changed', 'unchanged'], upgraded
            assert package() == '2.0 installed\nmanaged by MMX'
            # Without a conffile policy, dpkg stops at its prompt and leaves the package unpacked.
            remote('! dpkg -i /var/tmp/mmx-pin-fixture/repo/mmx-pin-fixture_1.0_all.deb </dev/null >/dev/null 2>&1')
            assert package() == '1.0 unpacked\nmanaged by MMX'
            pinned('1.0', managed, downgrade=True)
            repaired = results(run('run'))
            assert [r['change'] for r in repaired] == ['changed', 'unchanged'], repaired
            assert package() == '1.0 installed\nmanaged by MMX'
            assert run('run')['entries'][0]['response']['change'] == 'unchanged'
            mark('pinned upgrade and interrupted configuration keep the MMX-managed conffile without prompting, then rerun unchanged')

            rule = '/etc/apt/preferences.d/mmx-mmx-pin-fixture.pref'
            pinned('2.*')
            applied = results(run('run'))[0]
            assert applied['change'] == 'changed' and applied['value'] == [{'name': 'mmx-pin-fixture', 'from': '1.0', 'to': '2.0'}], applied
            assert remote(f'cat {rule}') == ('Explanation: Written by MMX for an apt_package task with a version.\nPackage: mmx-pin-fixture\n'
                                            'Pin: version 2.*\nPin-Priority: 999\n\nExplanation: Versions outside the pattern are never installed.\n'
                                            'Package: mmx-pin-fixture\nPin: version *\nPin-Priority: -1')
            policy = remote('apt-cache policy mmx-pin-fixture')
            assert 'Candidate: 2.0' in policy and '     1.0 -1' in policy, policy
            assert run('run')['entries'][0]['response']['change'] == 'unchanged'
            pinned('1.*')
            assert 'add allow_downgrade: true' in results(run('run', 1))[0]['reason']
            assert 'Pin: version 2.*' in remote(f'cat {rule}') and package().startswith('2.0 installed')
            pinned('3.*')
            assert 'available: ' in results(run('run', 1))[0]['reason']
            assert 'Pin: version 2.*' in remote(f'cat {rule}')
            tasks([{'apt_package': {'packages': [{'name': 'mmx-pin-fixture', 'version': 'newest_when_first_installed'}]}}])
            assert results(run('run'))[0]['change'] == 'changed'
            remote(f'test ! -e {rule}')
            assert package().startswith('2.0 installed')
            mark('a version pattern installs the newest matching version and apt keeps it there; refusals write nothing; a version keyword deletes only the MMX rule')

            pinned('1.0', downgrade=True)
            run('run')
            tasks([{'apt_package': {'packages': [{'name': 'mmx-pin-fixture', 'version': 'newest_on_every_run'}]}}])
            pending = [{'name': 'mmx-pin-fixture', 'from': '1.0', 'to': '2.0'}]
            preview = results(run('dryrun'))[0]
            assert preview['change'] == 'would_change' and preview['value'] == pending, preview
            assert package().startswith('1.0 installed')
            applied = results(run('run'))[0]
            assert applied['change'] == 'changed' and applied['value'] == pending, applied
            assert package().startswith('2.0 installed')
            remote(f'test ! -e {rule}')
            assert run('run')['entries'][0]['response']['change'] == 'unchanged'
            mark('newest_on_every_run previews, installs the newest version, deletes the version rule, then reruns unchanged')

            deb = workspace / 'mmx-pin-fixture_1.0_all.deb'
            deb.write_bytes(base64.b64decode(remote('base64 -w0 /var/tmp/mmx-pin-fixture/repo/mmx-pin-fixture_1.0_all.deb')))
            from_file = {'packages': [{'name': 'mmx-pin-fixture', 'version': '1.0', 'package_file_source': deb.name}]}
            tasks([{'apt_package': from_file}])
            assert 'add allow_downgrade: true' in results(run('run', 1))[0]['reason']
            tasks([{'apt_package': {'packages': [{**from_file['packages'][0], 'allow_downgrade': True}]}}])
            assert results(run('dryrun'))[0]['change'] == 'would_change' and package().startswith('2.0 installed')
            assert results(run('run'))[0]['change'] == 'changed'
            assert package().startswith('1.0 installed') and 'Pin: version 1.0\n' in remote(f'cat {rule}')
            assert run('run')['entries'][0]['response']['change'] == 'unchanged'
            tasks([{'apt_package': {'packages': [{'name': 'mmx-other', 'version': '1.0', 'package_file_source': deb.name}]}}])
            assert 'holds the package mmx-pin-fixture, not mmx-other' in results(run('run', 1))[0]['reason']
            mark('a package file installs its version, checks the name it holds, keeps the version with a rule, and needs allow_downgrade to go back')

            tasks([{'apt_package': {'packages': [{'name': 'mmx-pin-fixture'}], 'state': 'absent'}}])
            assert results(run('run'))[0]['value'] == [{'name': 'mmx-pin-fixture', 'from': '1.0'}]
            assert remote("dpkg-query -W -f='${db:Status-Status}' mmx-pin-fixture") == 'config-files'
            remote(f'test -e /etc/mmx-pin-fixture.conf; test ! -e {rule}')
            assert run('run')['entries'][0]['response']['change'] == 'unchanged'
            tasks([{'apt_package': {'packages': [{'name': 'mmx-pin-fixture'}], 'state': 'absent', 'also_delete_its_config_files': True, 'also_remove_packages_no_longer_needed': True, 'also_remove_packages_that_depend_on_these': True}}])
            assert results(run('run'))[0]['change'] == 'changed'
            remote("test ! -e /etc/mmx-pin-fixture.conf; ! dpkg-query -W -f='${db:Status-Status}' mmx-pin-fixture | grep -q config-files")
            assert run('run')['entries'][0]['response']['change'] == 'unchanged'
            mark('removal keeps configuration files; also_delete_its_config_files purges them, also from a removed package, then reruns unchanged')

            # Both pins must exist before apt solves this exact-version dependency.
            paired = [{'name': 'mmx-pin-dependent', 'version': '2.0'},
                      {'name': 'mmx-pin-fixture', 'version': '2.0'}]
            tasks([{'apt_package': {'packages': paired}}])
            assert results(run('run'))[0]['change'] == 'changed'
            assert remote("dpkg-query -W -f='${Version}\n' mmx-pin-fixture mmx-pin-dependent").split() == ['2.0', '2.0']
            tasks([{'apt_package': {'packages': list(reversed(paired))}}])
            assert results(run('run'))[0]['change'] == 'unchanged'
            original_rule = remote(f'cat {rule}')
            tasks([{'apt_package': {'packages': [{'name': 'mmx-pin-dependent', 'version': '1.0', 'allow_downgrade': True},
                                               {'name': 'mmx-pin-fixture', 'version': '1.0'}]}}])
            assert 'add allow_downgrade: true' in results(run('run', 1))[0]['reason']
            assert remote(f'cat {rule}') == original_rule
            assert 'Pin: version 2.0' in remote('cat /etc/apt/preferences.d/mmx-mmx-pin-dependent.pref')
            tasks([{'apt_package': {'packages': [{'name': 'mmx-pin-fixture'}], 'state': 'absent'}}])
            for command in ['dryrun', 'run']:
                refused = results(run(command, 1))[0]
                assert 'mmx-pin-dependent' in refused['reason'] and 'also_remove_packages_that_depend_on_these: true' in refused['reason'], refused
                assert package().startswith('2.0 installed') and remote(f'cat {rule}') == original_rule
            tasks([{'apt_package': {'packages': [{'name': 'mmx-pin-fixture'}], 'state': 'absent',
                                    'also_remove_packages_that_depend_on_these': True}}])
            assert {change['name'] for change in results(run('run'))[0]['value']} == {'mmx-pin-fixture', 'mmx-pin-dependent'}
            remote('dpkg --purge mmx-pin-fixture mmx-pin-dependent >/dev/null')
            tasks([{'apt_package': {'packages': [{'name': 'mmx-pin-dependent'}], 'state': 'absent'}}])
            run('run')
            mark('one task pins matching dependencies together in either order; a later refusal writes no rule; unnamed removals require consent in previews and runs')

            # The newest dependency is also apt's candidate, outside the task's list.
            tasks([{'apt_package': {'packages': [{'name': 'mmx-pin-dependent', 'version': '2.0'}]}}])
            dependency_installation = results(run('run'))[0]
            assert {'name': 'mmx-pin-dependent', 'to': '2.0'} in dependency_installation['value'], dependency_installation
            assert {'name': 'mmx-pin-fixture', 'to': '2.0'} in dependency_installation['value'], dependency_installation
            assert remote("dpkg-query -W -f='${Version}\n' mmx-pin-fixture mmx-pin-dependent").split() == ['2.0', '2.0']
            tasks([{'apt_package': {'packages': [{'name': 'mmx-pin-dependent'}, {'name': 'mmx-pin-fixture'}], 'state': 'absent'}}])
            run('run')
            remote('dpkg --purge mmx-pin-fixture mmx-pin-dependent >/dev/null')
            mark('apt_package value includes an unlisted dependency installed by the native transaction')

            remote('apt-get purge -y nginx nginx-common >/dev/null 2>&1 || true')
            tasks([{'apt_package': {'packages': [{'name': 'nginx', 'version': 'newest_when_first_installed'}]}}])
            run('run')
            assert remote('systemctl is-active nginx || true') == 'inactive'
            remote('test ! -e /usr/sbin/policy-rc.d')
            remote('apt-get purge -y nginx nginx-common >/dev/null')
            tasks([{'apt_package': {'packages': [{'name': 'nginx', 'version': 'newest_when_first_installed'}], 'allow_package_to_start_its_services': True}}])
            run('run')
            assert remote('systemctl is-active nginx') == 'active'
            remote('apt-get purge -y nginx nginx-common >/dev/null')
            mark('an installed package does not start its service unless allow_package_to_start_its_services says so; the policy program is gone afterwards')

            vm.ENV['MMX_TEST_REPOSITORY_PASSWORD'] = 'repository-p4ss'
            login_file = '/etc/apt/auth.conf.d/mmx-login-fixture.conf'
            private = {'apt_repository': {'name': 'mmx-login-fixture', 'uris': ['https://pkgs.example.invalid/debian'], 'suites': ['stable'],
                                          'components': ['main'], 'signing_key_source': 'source-fixture.asc', 'username': 'customer',
                                          'password': '{{ .secrets.repository_password }}'}}
            (workspace / 'source-fixture.asc').write_text('-----BEGIN PGP PUBLIC KEY BLOCK-----\nnot a real key\n-----END PGP PUBLIC KEY BLOCK-----\n')
            vm.write_project(workspace, playbook_name='worker', **{'project_name': 'server_resources', 'default_inventory': 'example',
                'secrets_from_env': {'repository_password': 'MMX_TEST_REPOSITORY_PASSWORD'},
                'fleet_stages': [{'id': 'configure', 'nodes': 'all', 'node_tasks': [private]}]})
            preview = run('dryrun')
            applied = run('run')
            assert 'repository-p4ss' not in json.dumps(preview) + json.dumps(applied)
            assert results(applied)[0]['change'] == 'changed'
            assert remote(f"stat -c '%a %U %G' {login_file}") == '600 root root'
            assert remote(f'cat {login_file}') == 'machine pkgs.example.invalid/debian login customer password repository-p4ss'
            assert 'customer' not in remote('cat /etc/apt/sources.list.d/mmx-login-fixture.sources')
            assert run('run')['entries'][0]['response']['change'] == 'unchanged'
            tasks([{'apt_repository': {'name': 'mmx-login-fixture', 'state': 'absent'}}])
            run('run')
            remote(f'test ! -e {login_file}; test ! -e /etc/apt/sources.list.d/mmx-login-fixture.sources')
            vm.ENV.pop('MMX_TEST_REPOSITORY_PASSWORD', None)
            mark('a private repository login goes to a root-only file, never into the source file or results; absent removes it')

            remote(SOURCE_FIXTURE)
            key = sign_source_fixture()
            (workspace / 'source-fixture.asc').write_text(key)
            repository = {'apt_repository': {'name': 'mmx-source-fixture', 'uris': ['file:/var/tmp/mmx-source-fixture/repo'],
                                         'suites': ['./'], 'signing_key_source': 'source-fixture.asc'}}
            source_file = '/etc/apt/sources.list.d/mmx-source-fixture.sources'
            key_file = '/etc/apt/keyrings/mmx-source-fixture.asc'

            def source_package():
                return remote("dpkg-query -W -f='${Version} ${db:Status-Status}' mmx-source-fixture")

            # The first installation refreshes the job's indexes; the new source
            # makes the next one refresh again, or it could not find its package.
            tasks([{'apt_package': {'packages': [{'name': 'mmx-pin-fixture', 'version': '2.0'}]}}, repository,
                   {'apt_package': {'packages': [{'name': 'mmx-source-fixture', 'version': '1.0'}]}}])
            # The new source is not indexed yet, so dryrun cannot check the last task.
            preview = results(run('dryrun'))
            assert [r['change'] for r in preview] == ['would_change', 'would_change', 'unknown'], preview
            assert preview[2]['status'] == 'not_checked', preview
            remote(f'test ! -e {source_file}; test ! -e {key_file}')
            applied = results(run('run'))
            assert [r['change'] for r in applied] == ['changed'] * 3, applied
            assert remote(f'cat {source_file}') == ('Types: deb\nURIs: file:/var/tmp/mmx-source-fixture/repo\nSuites: ./\n'
                                                  f'Signed-By: {key_file}')
            assert remote(f"cat {key_file}") == key.strip()
            assert remote(f"stat -c '%a %U %G' /etc/apt/keyrings {key_file} {source_file}") == '755 root root\n644 root root\n644 root root'
            assert source_package() == '1.0 installed'
            assert run('run')['entries'][0]['response']['change'] == 'unchanged'
            assert run('dryrun', 0)['entries'][0]['response']['change'] == 'unchanged'
            mark('a repository installs its signing key and source, the next installation refreshes and installs from it, then reruns unchanged')

            # The version task is gone, but its rule stays and holds the package.
            tasks([repository, {'apt_package': {'upgrade_all_packages_within_their_version_rules': True}}])
            assert results(run('run'))[1]['change'] == 'unchanged'
            assert source_package() == '1.0 installed'
            mark('upgrade_all_packages_within_their_version_rules: true stays inside the version rule that a deleted version task left')

            tasks([repository, {'apt_package': {'packages': [{'name': 'mmx-source-fixture', 'version': 'newest_when_first_installed'}]}}])
            run('run')
            remote('test ! -e /etc/apt/preferences.d/mmx-mmx-source-fixture.pref')
            tasks([repository, {'apt_package': {'upgrade_all_packages_within_their_version_rules': True}}])
            pending = [{'name': 'mmx-source-fixture', 'from': '1.0', 'to': '2.0'}]
            preview = results(run('dryrun'))
            assert [r['change'] for r in preview] == ['unchanged', 'would_change'] and preview[1]['value'] == pending, preview
            assert source_package() == '1.0 installed'
            applied = results(run('run'))
            assert applied[1]['change'] == 'changed' and applied[1]['value'] == pending, applied
            assert source_package() == '2.0 installed'
            assert run('run')['entries'][0]['response']['change'] == 'unchanged'
            assert run('dryrun', 0)['entries'][0]['response']['change'] == 'unchanged'
            mark('upgrade_all_packages_within_their_version_rules: true previews pending upgrades from current indexes, applies them, then reruns unchanged')

            tasks([{'apt_repository': {'name': 'mmx-source-fixture', 'state': 'absent'}}])
            assert results(run('dryrun'))[0]['change'] == 'would_change'
            remote(f'test -e {source_file}; test -e {key_file}')
            assert results(run('run'))[0]['change'] == 'changed'
            remote(f'test ! -e {source_file}; test ! -e {key_file}')
            assert source_package() == '2.0 installed'
            assert run('run')['entries'][0]['response']['change'] == 'unchanged'
            mark('removing a repository removes its source file and key, keeps its packages, then reruns unchanged')
        def throwaway_key():
            """An armored RSA public key nothing signs with."""
            with tempfile.TemporaryDirectory(dir=vm.ROOT) as home:
                gpg = ['gpg', '--homedir', home, '--batch', '--pinentry-mode', 'loopback', '--passphrase', '']
                try:
                    vm.cmd([*gpg, '--quick-gen-key', 'MMX test dnf repository', 'rsa2048', 'sign', 'never'])
                    return vm.cmd([*gpg, '--armor', '--export']).stdout
                finally:
                    vm.cmd(['gpgconf', '--homedir', home, '--kill', 'gpg-agent'], check=False)

        def dnf_workflows():
            print('dnf ' + remote('dnf --version | head -1'), flush=True)
            # dnf 5 (Fedora) keeps its version lock in /etc/dnf/versionlock.toml, built in; dnf 4
            # (Rocky Linux, AlmaLinux) in the plugin's /etc/dnf/plugins/versionlock.list.
            five = 'dnf5' in remote('readlink -f "$(command -v dnf)"')
            lock_file = '/etc/dnf/versionlock.toml' if five else '/etc/dnf/plugins/versionlock.list'
            mmx_mark = 'comment = "Written by MMX for package ' if five else '# Written by MMX for package '
            # Start without the packages, the version lock plugin, or lock entries.
            remote('systemctl disable --now nginx >/dev/null 2>&1; '
                   'dnf -y -q remove nginx nginx-core tree python3-dnf-plugin-versionlock >/dev/null 2>&1 || true; '
                   f'rm -f /etc/yum.repos.d/mmx-* /etc/pki/rpm-gpg/RPM-GPG-KEY-mmx-* {lock_file if five else ""}')

            def version(name):
                # rpm prints "package ... is not installed" on standard output.
                return remote(f"if rpm -q {name} >/dev/null 2>&1; then rpm -q --qf '%{{EPOCH}}:%{{VERSION}}-%{{RELEASE}}' {name}; fi").replace('(none):', '')

            def first(record):
                return results(record)[0]

            tasks([{'apt_package': {'packages': [{'name': 'nginx', 'version': 'newest_when_first_installed'}]}}])
            assert "this node's package manager is dnf (the node fact package_manager)" in first(run('run', 1))['reason']
            tasks([{'dnf_package': {'packages': [{'name': 'nginx', 'version': 'newest_when_first_installed'}]}}])
            assert first(run('dryrun'))['change'] == 'would_change'
            assert first(run('run'))['change'] == 'changed'
            assert version('nginx') and remote('systemctl is-active nginx || true') == 'inactive'
            assert first(run('run'))['change'] == 'unchanged' and first(run('dryrun', 0))['change'] == 'unchanged'
            mark('dnf_package installs a package without starting it and reruns unchanged; apt_package refuses and names the node fact package_manager')

            # sort -V orders these versions as rpm does.
            available = [v[2:] if v.startswith('0:') else v for v in
                         remote("dnf -q repoquery --qf '%{epoch}:%{version}-%{release}\\n' nginx | grep . | sort -uV").split()]
            oldest, newest = available[0], available[-1]
            lock = {'dnf_package': {'packages': [{'name': 'nginx', 'version': oldest, 'allow_downgrade': True}]}}
            tasks([lock])
            if five:
                plugin = []  # dnf 5's version lock is built in: no plugin to install
            else:
                assert 'python3-dnf-plugin-versionlock' in first(run('run', 1))['reason']
                plugin = [{'dnf_package': {'packages': [{'name': 'python3-dnf-plugin-versionlock', 'version': 'newest_when_first_installed'}]}}]
            tasks([*plugin, lock])
            run('run')
            assert version('nginx') == oldest, (version('nginx'), oldest)
            listed = remote(f'cat {lock_file}')
            assert oldest.split(':')[-1] in listed and mmx_mark + 'nginx' in listed, listed
            remote('dnf -y -q upgrade nginx >/dev/null 2>&1 || true')
            assert version('nginx') == oldest, 'dnf moved a locked package'
            assert results(run('run'))[-1]['change'] == 'unchanged'
            if len(available) > 1:
                tasks([*plugin, {'dnf_package': {'packages': [{'name': 'nginx', 'version': newest}]}}])
                applied = results(run('run'))[-1]
                assert {'name': 'nginx', 'from': oldest, 'to': newest} in applied['value'], applied
                tasks([*plugin, {'dnf_package': {'packages': [{'name': 'nginx', 'version': oldest}]}}])
                assert 'add allow_downgrade: true' in results(run('run', 1))[-1]['reason']
                assert version('nginx') == newest
            if five:
                # dnf 5's lock has no wildcard: a rule of numbers that end in .* becomes a range of versions.
                pattern = newest.split('-')[0].rsplit('.', 1)[0] + '.*'
                lower_bound = pattern.removesuffix('.*') if ':' in pattern else '0:' + pattern.removesuffix('.*')
                tasks([*plugin, {'dnf_package': {'packages': [{'name': 'nginx', 'version': newest.rsplit('.', 1)[0] + '*'}]}}])
                refused = results(run('run', 1))[-1]
                assert 'cannot be kept on this node' in refused['reason'] and version('nginx') == newest, refused
            else:
                pattern = newest.rsplit('.', 1)[0] + '*'
                lower_bound = pattern
            tasks([*plugin, {'dnf_package': {'packages': [{'name': 'nginx', 'version': pattern}]}}])
            assert results(run('run'))[-1]['status'] == 'succeeded'
            assert lower_bound in remote(f'cat {lock_file}')
            mark("dnf_package version installs the version and keeps it with dnf's version lock; going back needs allow_downgrade")

            tasks([*plugin, {'dnf_package': {'packages': [{'name': 'nginx', 'version': oldest, 'allow_downgrade': True}]}}])
            run('run')
            tasks([{'dnf_package': {'packages': [{'name': 'nginx', 'version': 'newest_on_every_run'}]}}])
            if len(available) > 1:
                preview = first(run('dryrun'))
                assert preview['change'] == 'would_change' and {'name': 'nginx', 'from': oldest, 'to': newest} in preview['value'], preview
                applied = first(run('run'))
                assert applied['change'] == 'changed' and version('nginx') == newest, applied
            else:
                run('run')
            listed = remote(f'cat {lock_file}')
            assert 'nginx' not in listed, listed
            assert first(run('run'))['change'] == 'unchanged'
            mark('newest_on_every_run deletes the version lock entry and brings the package to the newest version')

            if five:
                # dnf 5: an administrator's table stays in force, and a rule change that it forbids writes nothing.
                header = 'version = "1.0"\n\n'
                def administrator_table(name, comparator, value):
                    return (f'[[packages]]\nname = "{name}"\ncomment = "administrator fixture"\n\n[[packages.conditions]]\n'
                            f'key = "evr"\ncomparator = "{comparator}"\nvalue = "{value}"\n')
                def write_lock(text):
                    (workspace / 'versionlock.toml').write_text(text)
                    remote(f"cat > {lock_file} <<'MMX_LOCK_END'\n{text}MMX_LOCK_END")
                newest_evr = newest if ':' in newest else '0:' + newest
                if len(available) > 1:
                    tasks([{'dnf_package': {'packages': [{'name': 'nginx', 'version': oldest, 'allow_downgrade': True}]}}])
                    run('run')
                    tasks([{'dnf_package': {'packages': [{'name': 'nginx', 'version': 'newest_when_first_installed'}]}}])
                    run('run')
                    # The table allows only versions below the newest: newest_on_every_run must not try the newest.
                    write_lock(header + administrator_table('nginx', '<', newest_evr))
                    tasks([{'dnf_package': {'packages': [{'name': 'nginx', 'version': 'newest_on_every_run'}]}}])
                    assert first(run('run'))['change'] == 'unchanged' and version('nginx') == oldest
                    assert first(run('dryrun', 0))['change'] == 'unchanged'
                    # A rule change to a version the table forbids fails before anything is written.
                    before = remote(f'cat {lock_file}')
                    tasks([{'dnf_package': {'packages': [{'name': 'nginx', 'version': newest}]}}])
                    for command in ('dryrun', 'run'):
                        refused = first(run(command, 1))
                        assert 'outside a version lock that an administrator wrote' in refused['reason'], refused
                        assert remote(f'cat {lock_file}') == before and version('nginx') == oldest
                    write_lock(header)
                    tasks([{'dnf_package': {'packages': [{'name': 'nginx', 'version': 'newest_on_every_run'}]}}])
                    assert first(run('run'))['change'] == 'changed' and version('nginx') == newest
                    mark("on dnf 5, newest_on_every_run and a version rule stay inside an administrator's lock table, and a refused rule change writes nothing")
                # dnf versionlock add writes its table beside MMX's; MMX's rerun is unchanged and keeps one table.
                tasks([{'dnf_package': {'packages': [{'name': 'nginx', 'version': pattern}]}}])
                run('run')
                remote('dnf -y -q install tree >/dev/null 2>&1 || true; dnf -q versionlock add tree >/dev/null 2>&1')
                assert 'Added by' in remote(f'cat {lock_file}')
                assert first(run('run'))['change'] == 'unchanged'
                listed = remote(f'cat {lock_file}')
                assert listed.count('Written by MMX for package nginx') == 1 and 'name = "tree"' in listed, listed
                tasks([{'dnf_package': {'packages': [{'name': 'nginx', 'version': 'newest_when_first_installed'}]}}])
                run('run')
                remote('dnf -q versionlock delete tree >/dev/null 2>&1 || true; dnf -y -q remove tree >/dev/null 2>&1 || true')
                mark("dnf 5's own versionlock add keeps MMX's table, and MMX's rerun is unchanged without a second table")

                # The range of a rule that ends in .*, checked by dnf 5 itself with a package that has versions on both sides.
                remote('dnf -y -q install rpm-build createrepo_c >/dev/null 2>&1; rm -rf /var/tmp/mmx-range && mkdir -p /var/tmp/mmx-range/repo')
                versions = ['15.9', '16.0', '16.9', '16^1', '16a', '17.0~rc1', '17.0', '160.0']
                for v in versions:
                    remote(f"""cd /var/tmp/mmx-range && cat > fixture.spec <<'MMX_SPEC_END'
Name: mmx-range-fixture
Version: {v}
Release: 1
Summary: MMX test package
License: MIT
BuildArch: noarch
%description
MMX test package.
%files
MMX_SPEC_END
rpmbuild -bb --define "_rpmdir /var/tmp/mmx-range/repo" --define "_topdir /var/tmp/mmx-range/top" fixture.spec >/dev/null 2>&1""")
                remote("createrepo_c -q /var/tmp/mmx-range/repo; printf '[mmx-range]\nname=range\nbaseurl=file:///var/tmp/mmx-range/repo/noarch\nenabled=1\ngpgcheck=0\n' > /etc/yum.repos.d/mmx-range.repo; "
                       'rm -rf /var/tmp/mmx-range/repo/noarch/repodata; createrepo_c -q /var/tmp/mmx-range/repo/noarch; dnf -q makecache --refresh >/dev/null')
                tasks([{'dnf_package': {'packages': [{'name': 'mmx-range-fixture', 'version': '16.*'}]}}])
                assert first(run('run'))['status'] == 'succeeded' and version('mmx-range-fixture') == '16.9-1'
                def excluded(v):
                    out = remote(f'dnf --assumeno install mmx-range-fixture-{v}-1 2>&1 || true')
                    return 'excluded by versionlock' in out
                inside = ['16.0', '16.9', '16^1', '16a']
                outside = ['15.9', '17.0~rc1', '17.0', '160.0']
                assert [v for v in inside if excluded(v)] == [], 'dnf 5 refused a version inside the range'
                assert [v for v in outside if not excluded(v)] == [], 'dnf 5 admitted a version outside the range'
                tasks([{'dnf_package': {'packages': [{'name': 'mmx-range-fixture', 'version': 'newest_when_first_installed'}]}}])
                run('run')
                remote('dnf -y -q remove mmx-range-fixture >/dev/null 2>&1; rm -rf /var/tmp/mmx-range /etc/yum.repos.d/mmx-range.repo')
                mark('the range that MMX writes for 16.* admits 16.0, 16.9, 16^1, and 16a and makes dnf 5 refuse 15.9, 17.0~rc1, 17.0, and 160.0')

            rpm_file = remote('cd /var/tmp && rm -f tree-*.rpm && dnf -q download tree >/dev/null && ls /var/tmp/tree-*.rpm')
            (workspace / 'tree.rpm').write_bytes(base64.b64decode(remote(f'base64 -w0 {rpm_file}')))
            tree_file_version = remote("rpm -qp --qf '%{EPOCH}:%{VERSION}-%{RELEASE}' " + rpm_file)
            tree_file_version = tree_file_version.removeprefix("(none):").removeprefix("0:")
            tasks([*plugin, {'dnf_package': {'packages': [{'name': 'tree', 'version': tree_file_version, 'package_file_source': 'tree.rpm'}]}}])
            assert results(run('run'))[-1]['change'] == 'changed'
            tree_version = version('tree')
            assert tree_version and tree_version.split(':')[-1] in remote(f'cat {lock_file}')
            rerun = results(run('run'))[-1]
            assert rerun['change'] == 'unchanged', rerun
            tasks([*plugin, {'dnf_package': {'packages': [{'name': 'nginx', 'version': tree_file_version, 'package_file_source': 'tree.rpm'}]}}])
            assert 'holds the package tree, not nginx' in results(run('run', 1))[-1]['reason']
            mark('an .rpm package file installs its version, checks the name it holds, and is locked at that version')

            if five:
                administrator_tree_rule = f'comment = "administrator fixture rule"'
                remote("printf '\n[[packages]]\nname = \"tree\"\n%s\n\n[[packages.conditions]]\nkey = \"evr\"\ncomparator = \"=\"\nvalue = \"%s\"\n' "
                       + shlex.quote(administrator_tree_rule) + ' ' + shlex.quote(tree_version if ':' in tree_version else '0:' + tree_version) + f' >> {lock_file}')
            else:
                administrator_tree_rule = f'tree-{tree_version}.*'
                remote("printf '%s\n' '# administrator fixture rule' " + shlex.quote(administrator_tree_rule) + " >> /etc/dnf/plugins/versionlock.list")
            tasks([{'dnf_package': {'packages': [{'name': 'tree', 'version': 'newest_when_first_installed'}]}}])
            assert first(run('run'))['change'] == 'changed'
            listed = remote(f'cat {lock_file}')
            assert administrator_tree_rule in listed and mmx_mark + 'tree' not in listed, listed
            assert first(run('run'))['change'] == 'unchanged'
            mark('a version keyword deletes only the MMX-marked lock and keeps an administrator rule for the same package')

            tasks([{'dnf_package': {'packages': [{'name': 'tree'}, {'name': 'nginx'}], 'state': 'absent'}}])
            removed = first(run('run'))
            assert {r['name'] for r in removed['value']} == {'tree', 'nginx'}, removed
            assert not version('tree') and not version('nginx') and version('nginx-core'), 'removal took more than the named packages'
            assert administrator_tree_rule in remote(f'cat {lock_file}')
            assert first(run('run'))['change'] == 'unchanged'
            mark('dnf_package absent removes exactly the named packages and their version lock entries, then reruns unchanged')
            # Removing nginx-core also removes nginx, which depends on it.
            tasks([{'dnf_package': {'packages': [{'name': 'nginx', 'version': 'newest_when_first_installed'}]}}])
            run('run')
            tasks([{'dnf_package': {'packages': [{'name': 'nginx-core'}], 'state': 'absent'}}])
            for command in ['dryrun', 'run']:
                refused = first(run(command, 1))
                assert 'nginx' in refused['reason'] and 'also_remove_packages_that_depend_on_these: true' in refused['reason'], refused
                assert version('nginx') and version('nginx-core')
            tasks([{'dnf_package': {'packages': [{'name': 'nginx-core'}], 'state': 'absent',
                                    'also_remove_packages_that_depend_on_these': True}}])
            assert first(run('run'))['change'] == 'changed'
            assert not version('nginx') and not version('nginx-core')
            mark('dnf unnamed removal fails before changing packages in dryrun and run, then succeeds with the explicit scope setting')


            key = throwaway_key()
            (workspace / 'mmx-dnf-key.asc').write_text(key)
            repository = {'dnf_repository': {'name': 'mmx-fixture', 'description': 'MMX test repository', 'enabled': False,
                                             'baseurl': ['file:///var/tmp/mmx-dnf-repo'], 'signing_key_source': 'mmx-dnf-key.asc'}}
            tasks([repository])
            assert first(run('dryrun'))['change'] == 'would_change'
            remote('test ! -e /etc/yum.repos.d/mmx-fixture.repo')
            assert first(run('run'))['change'] == 'changed'
            assert remote('cat /etc/yum.repos.d/mmx-fixture.repo') == (
                '[mmx-fixture]\nname=MMX test repository\nbaseurl=file:///var/tmp/mmx-dnf-repo\nenabled=0\ngpgcheck=1\n'
                'gpgkey=file:///etc/pki/rpm-gpg/RPM-GPG-KEY-mmx-fixture')
            assert remote("stat -c '%a %U' /etc/yum.repos.d/mmx-fixture.repo /etc/pki/rpm-gpg/RPM-GPG-KEY-mmx-fixture") == '644 root\n644 root'
            assert 'MMX test dnf repository' in remote("rpm -q --qf '%{SUMMARY}\\n' gpg-pubkey")
            assert 'mmx-fixture' in remote('dnf -q repolist --disabled')
            assert first(run('run'))['change'] == 'unchanged'
            # A second repository with the same key keeps it imported while the first goes.
            second = {'dnf_repository': {**repository['dnf_repository'], 'name': 'mmx-fixture-two'}}
            tasks([repository, second])
            run('run')
            remote('test -e /etc/yum.repos.d/mmx-fixture-two.repo')
            tasks([{'dnf_repository': {'name': 'mmx-fixture', 'state': 'absent'}}])
            assert first(run('run'))['change'] == 'changed'
            remote('test ! -e /etc/yum.repos.d/mmx-fixture.repo; test ! -e /etc/pki/rpm-gpg/RPM-GPG-KEY-mmx-fixture')
            assert 'MMX test dnf repository' in remote("rpm -q --qf '%{SUMMARY}\\n' gpg-pubkey"), 'the key another repository needs was removed'
            tasks([{'dnf_repository': {'name': 'mmx-fixture-two', 'state': 'absent'}}])
            assert first(run('run'))['change'] == 'changed'
            remote('test ! -e /etc/yum.repos.d/mmx-fixture-two.repo; test ! -e /etc/pki/rpm-gpg/RPM-GPG-KEY-mmx-fixture-two')
            assert 'MMX test dnf repository' not in remote("rpm -q --qf '%{SUMMARY}\\n' gpg-pubkey || true"), 'the key stayed in rpm after its last repository went'
            tasks([{'dnf_repository': {'name': 'mmx-fixture-two', 'state': 'absent'}}])
            assert first(run('run'))['change'] == 'unchanged'
            mark('dnf_repository writes the key, imports it into rpm, writes the repository file with gpgcheck=1, and absent removes both and the imported key unless another key file holds it')

            def held_keys():
                return sorted(remote('rpm -qa gpg-pubkey || true').split())

            # A key rpm held before the task, the distribution's own, is not the module's to remove.
            distribution_key = remote('ls /etc/pki/rpm-gpg | grep -E "Rocky|AlmaLinux" | grep -v Testing | head -n 1').strip()
            if distribution_key:
                distribution_key_text = remote(f'cat /etc/pki/rpm-gpg/{distribution_key}')
            else:
                # Fedora keeps no key files in /etc/pki/rpm-gpg; rpm holds the keys, and the first one is a key rpm held.
                distribution_key_text = remote("rpm -q --qf '%{DESCRIPTION}\\n' gpg-pubkey | awk '/END PGP/ {print; exit} {print}'")
            (workspace / 'mmx-dnf-distribution-key.asc').write_text(distribution_key_text + '\n')
            before = held_keys()
            held_before = {'dnf_repository': {**repository['dnf_repository'], 'name': 'mmx-fixture-held', 'signing_key_source': 'mmx-dnf-distribution-key.asc'}}
            tasks([held_before])
            assert first(run('run'))['change'] == 'changed'
            tasks([{'dnf_repository': {'name': 'mmx-fixture-held', 'state': 'absent'}}])
            assert first(run('run'))['change'] == 'changed'
            assert held_keys() == before, 'removing the repository removed a key rpm held before the task'
            # A key that changes: the old key goes after the new one is imported.
            tasks([repository])
            run('run')
            first_key = [k for k in held_keys() if k not in before]
            assert len(first_key) == 1, first_key
            (workspace / 'mmx-dnf-key.asc').write_text(throwaway_key())
            rotation = first(run('run', None))
            assert rotation['change'] == 'changed' and rotation['status'] == 'succeeded', rotation
            rotated = [k for k in held_keys() if k not in before]
            assert len(rotated) == 1 and rotated != first_key, (first_key, rotated)
            tasks([{'dnf_repository': {'name': 'mmx-fixture', 'state': 'absent'}}])
            run('run')
            assert held_keys() == before
            mark('dnf_repository removes only keys it imported: a key rpm held before the task stays, and a changed key replaces the old one')

            disabled = remote("dnf -q repolist --disabled | awk 'NR>1 {print $1}' | head -n 1")
            switch = {'dnf_repository': {'existing_repository': disabled, 'enabled': True}}
            tasks([switch])
            switched = first(run('run'))
            assert switched['change'] == 'changed', (disabled, switched)
            assert disabled in remote("dnf -q repolist --enabled | awk 'NR>1 {print $1}'"), remote('dnf -q repolist --all')
            assert first(run('run'))['change'] == 'unchanged'
            switch['dnf_repository']['enabled'] = False
            tasks([switch])
            assert first(run('run'))['change'] == 'changed'
            assert disabled in remote("dnf -q repolist --disabled | awk 'NR>1 {print $1}'")
            tasks([{'dnf_repository': {'existing_repository': 'mmx-no-such-repository', 'enabled': True}}])
            assert 'defines no repository mmx-no-such-repository' in first(run('run', 1))['reason']
            mark('dnf_repository existing_repository switches a repository already on the node on and off through dnf')

            postgresql = ['postgresql-server', 'postgresql', 'postgresql-private-libs']
            # The package postgresql-server makes the account postgres, and
            # its removal leaves the account. postgresql_databases.py refuses
            # a guest that has it, so this suite removes what it brought.
            had_the_account_postgres = remote('getent passwd postgres || true').strip() != ''

            def without_postgresql():
                # MMX's lock entries outlive a removal by hand; each is its
                # marker line and the entry line after it.
                remote(f"dnf -y -q remove {' '.join(postgresql)} >/dev/null 2>&1 || true; "
                       "dnf -y -q module reset postgresql >/dev/null 2>&1 || true; "
                       "sed -i '/^# Written by MMX for package postgresql/{N;d}' /etc/dnf/plugins/versionlock.list 2>/dev/null || true")
                if not had_the_account_postgres:
                    remote('userdel postgres 2>/dev/null || true; groupdel postgres 2>/dev/null || true; rm -rf /var/lib/pgsql')

            without_postgresql()
            record = '/etc/dnf/modules.d/postgresql.module'
            streams = remote("dnf -q module list postgresql 2>/dev/null | awk '$1 == \"postgresql\" {print $2}'").split()
            stream_task = {'dnf_package': {'packages': [{'name': 'postgresql-server', 'version': '16.*'}], 'enable_dnf_module_stream': 'postgresql:16'}}
            tasks([stream_task])
            if '16' not in streams:
                assert 'other versions are separate package names, such as postgresql16-server' in first(run('run', 1))['reason']
                mark('enable_dnf_module_stream fails on a node without module streams and names the separate package names')
            else:
                for policy in ['16.*', 'newest_on_every_run']:
                    without_postgresql()
                    stream_task['dnf_package']['packages'][0]['version'] = policy
                    tasks([stream_task])
                    dryrun = first(run('dryrun'))
                    assert dryrun['status'] == 'not_checked' and dryrun['change'] == 'unknown', dryrun
                    assert 'state=enabled' not in remote(f'cat {record} 2>/dev/null || true')
                    assert first(run('run'))['change'] == 'changed'
                    assert version('postgresql-server').startswith('16.'), version('postgresql-server')
                    assert 'stream=16' in remote(f'cat {record}') and 'state=enabled' in remote(f'cat {record}')
                    assert first(run('run'))['change'] == 'unchanged'
                without_postgresql()
                # Refusing a version keeps only the requested stream enabled.
                remote("sed -i '/^# Written by MMX for package postgresql-server$/{N;d;}' /etc/dnf/plugins/versionlock.list")
                stream_task['dnf_package']['packages'][0]['version'] = '99.*'
                tasks([stream_task])
                refused = first(run('run', 1))
                assert refused['change'] == 'changed' and 'no version of postgresql-server' in refused['reason'], refused
                assert 'stream=16' in remote(f'cat {record}') and 'state=enabled' in remote(f'cat {record}')
                assert all(version(name) == '' for name in postgresql)
                assert mmx_mark + 'postgresql-server' not in remote(f'cat {lock_file}')
                stream_task['dnf_package']['packages'][0]['version'] = '16.*'
                tasks([stream_task])
                assert first(run('run'))['change'] == 'changed'
                assert version('postgresql-server').startswith('16.')
                tasks([{'dnf_package': {'packages': [{'name': 'postgresql-contrib', 'version': 'newest_when_first_installed'}], 'enable_dnf_module_stream': 'postgresql:15'}}])
                assert 'has the stream 16 enabled, not 15' in first(run('run', 1))['reason']
                assert 'stream=16' in remote(f'cat {record}')
                without_postgresql()
                remote('dnf -y -q install postgresql-server >/dev/null')
                outside = version('postgresql-server')
                tasks([stream_task])
                assert 'is already installed from outside the stream postgresql:16' in first(run('run', 1))['reason']
                assert version('postgresql-server') == outside and 'state=enabled' not in remote(f'cat {record}')
                without_postgresql()
                mark('enable_dnf_module_stream reports an inactive stream not_checked in dryrun, enables it before installing a pattern or newest_on_every_run, and keeps the stream enabled after a refused version; another stream or outside package fails without changes')

            tasks([{'dnf_package': {'upgrade_all_packages_within_their_version_rules': True}}])
            preview = first(run('dryrun'))
            assert preview['status'] == 'succeeded' and all({'name', 'to'} <= set(c) for c in preview.get('value', [])), preview
            mark('dnf_package upgrade_all_packages_within_their_version_rules: true previews the whole-node upgrade as name, from, and to without changing anything')

            # Refresh before capturing the selection so preview and apply use
            # the same metadata. No general upgrade runs before this case.
            remote('dnf -q makecache --refresh')
            advisories = remote('LC_ALL=C dnf -q --cacheonly updateinfo list --all')
            has_advisories = False
            for line in advisories.splitlines():
                fields = line.split()
                if fields and fields[0] == 'i':
                    fields = fields[1:]
                if len(fields) >= 3 and (fields[1] in ('security', 'bugfix', 'enhancement', 'newpackage', 'unknown', 'other')
                                         or fields[1].endswith('/Sec.')):
                    has_advisories = True
            security_check = vm.cmd([*ssh, 'sudo -n env LC_ALL=C dnf -q check-update --security'], check=False)
            assert security_check.returncode in (0, 100), (security_check.stdout, security_check.stderr)
            security_packages = set()
            for line in security_check.stdout.splitlines():
                fields = line.split()
                if fields and re.match(r'^[A-Za-z0-9_+][A-Za-z0-9_.+-]*\.(?:x86_64|aarch64|noarch|i[3-6]86)$', fields[0]):
                    security_packages.add(fields[0].rsplit('.', 1)[0])
                elif len(fields) == 4 and re.match(r'^(?:[0-9]+:)?[0-9]', fields[2]):
                    security_packages.add(fields[0])
            assert bool(security_packages) == (security_check.returncode == 100), security_check.stdout
            installed_before = remote("rpm -qa --qf '%{NAME} %{EPOCH}:%{VERSION}-%{RELEASE}.%{ARCH}\\n' | sort")
            tasks([{'dnf_package': {'upgrade_all_packages_within_their_version_rules': True, 'only_security_updates': True}}])
            if has_advisories:
                preview = first(run('dryrun'))
                assert preview['status'] == 'succeeded', preview
                assert remote("rpm -qa --qf '%{NAME} %{EPOCH}:%{VERSION}-%{RELEASE}.%{ARCH}\\n' | sort") == installed_before
                applied = first(run('run'))
                assert applied['status'] == 'succeeded', applied
                assert security_packages <= {change['name'] for change in applied.get('value', [])}, (security_check.stdout, applied)
                assert applied['change'] == ('changed' if security_packages else 'unchanged'), applied
                assert preview.get('value', []) == applied.get('value', []), (preview, applied)
                after = vm.cmd([*ssh, 'sudo -n env LC_ALL=C dnf -q check-update --security'], check=False)
                assert after.returncode == 0, (after.stdout, after.stderr)
                mark('dnf_package security upgrade previews and applies exactly the security selection, or stays unchanged')
            else:
                reason = "the node's package sources list no advisories for its packages, so dnf cannot select security updates"
                for command in ('dryrun', 'run'):
                    failed = first(run(command, 1))
                    assert failed['status'] == 'failed' and reason in failed['reason'], failed
                    assert remote("rpm -qa --qf '%{NAME} %{EPOCH}:%{VERSION}-%{RELEASE}.%{ARCH}\\n' | sort") == installed_before
                mark('dnf_package security upgrade fails without advisories in dryrun and run, leaving every package unchanged')

        manager = remote('if command -v apt-get >/dev/null; then echo apt; else echo dnf; fi')
        {'apt': apt_and_ufw_workflows, 'dnf': dnf_workflows}[manager]()
    finally:
        cleanup()
    print(f'{len(passed)} server-resource workflows passed', flush=True)


if __name__ == '__main__':
    vm.run_suite(main)
