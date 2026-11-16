#!/usr/bin/env python3
"""Explicit accounts and unit drop-ins/removal through MMX and native state checks."""
import json
import shlex
import shutil
import time

import vm

# Accounts the account workflows create, and the group they share.
PEOPLE = ['mmx-account-person', 'mmx-account-second', 'mmx-account-service', 'mmx-account-primary']
TEAM = 'mmx-account-team'


def main():
    vm.up()
    node, ssh = vm.connection()
    vm.reset()
    vm.build()
    workspace = vm.ROOT / 'account-units'
    if workspace.exists():
        shutil.rmtree(workspace)
    shutil.copytree(vm.REPO / 'examples/account-and-unit', workspace)
    vm.write_inventory(workspace, {'nodes': {'app': node}}, name='example')
    mmx = vm.ROOT / 'bin/mmx'
    base = '/var/tmp/mmx-account-units'
    name = 'mmx-account-demo'
    unit = name + '.service'
    installed = '/etc/systemd/system/' + unit
    drop = installed + '.d/20-limits.conf'
    source = workspace / 'files/limits.conf.tmpl'
    original = (workspace / 'playbooks/account_service.yml').read_text()
    settings_file = workspace / 'mmx_project_settings.yml'
    original_settings = settings_file.read_text()
    passed = []
    # Password hashes this suite gives to MMX; no output may contain one.
    secret_values = []

    def remote(script):
        return vm.cmd([*ssh, 'sudo -n /bin/sh -c ' + shlex.quote('set -eu\n' + script)]).stdout

    def run(command, expected=None, *args, playbook='account_service'):
        proc = vm.cmd([mmx, command, *vm.playbook_args(command, playbook), '--json', *args], cwd=workspace, check=False)
        for secret in secret_values:
            assert secret not in proc.stdout + proc.stderr, 'a password hash appeared in mmx output'
        assert proc.returncode in (vm.success_codes(command) if expected is None else (expected,)), (command, proc.stdout, proc.stderr)
        return json.loads(proc.stdout)

    def project(tasks, **settings):
        vm.write_project(workspace, playbook_name='account_service', **{'project_name': 'account_and_unit', 'default_inventory': 'example',
            'node_task_defaults': {'retry': {'pause_between_tries': '0s'}},
            'fleet_stages': [{'id': 'configure', 'nodes': 'all', 'node_tasks': tasks}]}, **settings)

    def mark(message):
        passed.append(message)
        print('PASS ' + message, flush=True)

    def prop(key, service=unit):
        return remote(f'systemctl show -p {key} --value {service}').strip()

    def environment(pid):
        return remote(f'tr "\\000" "\\n" < /proc/{pid}/environ')

    def eventually(check, seconds=15):
        deadline = time.monotonic() + seconds
        while not check():
            assert time.monotonic() < deadline, 'condition not reached'
            time.sleep(.2)

    # Timer, socket, and mount fixtures. A mount unit is named for its escaped
    # mount point, so the shell needs its backslashes quoted.
    tick, echo = 'mmx-units-tick', 'mmx-units-echo'
    where = '/srv/mmx-units-data'
    mount = 'srv-mmx\\x2dunits\\x2ddata.mount'
    quoted_mount = shlex.quote(mount)

    # A target with one member, a slice, an automount for the mount above, and a
    # swap file. Swap and automount units are named for their escaped paths.
    workers, worker, batch = 'mmx-units-workers.target', 'mmx-units-worker.service', 'mmxunitsbatch.slice'
    automount = shlex.quote('srv-mmx\\x2dunits\\x2ddata.automount')
    swap_file = '/var/tmp/mmx-units-swap'
    swap = 'var-tmp-mmx\\x2dunits\\x2dswap.swap'
    quoted_swap = shlex.quote(swap)
    marker = '/var/lib/mmx/commands_run_once/mmx_account_units_suite'
    cache = 'mmx-units-cache.service'
    reports = 'mmx-units-reports'  # an account with units in its own systemd

    # One unit at a time, so that a unit that does not exist cannot keep the
    # others from being stopped after a failed run.
    def stop_and_disable(*units):
        return ''.join(f'systemctl disable --now {u} 2>/dev/null || systemctl stop {u} 2>/dev/null || true\n' for u in units)

    reference, skeleton_marker = 'mmx-account-reference', '.mmx-skeleton-marker'

    def cleanup():
        # Some distributions (openSUSE) ship no mail spool directory.
        remote(f'mkdir -p /var/mail\nuserdel -r {reference} 2>/dev/null || true\nrm -f /etc/skel/{skeleton_marker}')
        remote(''.join(f'userdel {a} 2>/dev/null || true\n' for a in PEOPLE) +
               f'groupdel {TEAM} 2>/dev/null || true\n'
               # /nonexistent must never exist; a home a task wrongly made there would hide the next mistake.
               f'rm -rf {" ".join("/home/" + a for a in PEOPLE)} /var/mail/{PEOPLE[0]} /usr/local/sbin/chpasswd /nonexistent\n')
        remote(f'loginctl disable-linger {reports} 2>/dev/null || true\n'
               f'if id -u {reports} >/dev/null 2>&1; then systemctl stop "user@$(id -u {reports}).service" 2>/dev/null || true; fi\n'
               f'userdel {reports} 2>/dev/null || true\ngroupdel {reports} 2>/dev/null || true\nrm -rf /home/{reports}\n')
        remote(stop_and_disable(automount, quoted_swap, workers, worker, cache) +
               f'rm -rf /etc/systemd/system/{cache} /var/lib/mmx/command_records\n'
               f'swapoff {swap_file} 2>/dev/null || true\n'
               f'systemctl unmask mmx-vendor-demo.service 2>/dev/null || true\n'
               f'rm -f /etc/systemd/system/{automount} /etc/systemd/system/{quoted_swap} /etc/systemd/system/{workers} '
               f'/etc/systemd/system/{worker} /etc/systemd/system/{batch} {swap_file} {marker}\n')
        remote(stop_and_disable(f'{tick}.timer', f'{echo}.socket', quoted_mount) +
               f'systemctl stop mmx-units-busy.service {tick}.service 2>/dev/null || true\n'
               f'rm -f /etc/systemd/system/{tick}.service /etc/systemd/system/{tick}.timer /etc/systemd/system/{echo}@.service '
               f'/etc/systemd/system/{echo}.socket /etc/systemd/system/{quoted_mount}\n'
               f'umount {where} 2>/dev/null || true\nrmdir {where} 2>/dev/null || true\n'
               f'systemctl reset-failed {tick}.service mmx-units-busy.service 2>/dev/null || true\n')
        remote(stop_and_disable('mmx-account-demo.service', 'mmx-account-busy.service', 'mmx-vendor-demo.service', 'mmx-account-repair.service') +
               'systemctl reset-failed mmx-account-demo.service mmx-account-busy.service mmx-vendor-demo.service mmx-account-repair.service 2>/dev/null || true\n'
               'rm -rf /etc/systemd/system/mmx-account-demo.service.d /etc/systemd/system/mmx-account-link.service.d /etc/systemd/system/mmx-account-repair.service.d\n'
               'rm -f /etc/systemd/system/mmx-account-demo.service /etc/systemd/system/mmx-account-link.service /etc/systemd/system/mmx-vendor-demo.service /usr/lib/systemd/system/mmx-vendor-demo.service /etc/systemd/system/mmx-account-repair.service\n'
               'systemctl daemon-reload\n'
               'userdel mmx-account-demo 2>/dev/null || true\nuserdel mmx-account-retired 2>/dev/null || true\n'
               'groupdel mmx-account-demo 2>/dev/null || true\ngroupdel mmx-account-retired 2>/dev/null || true\n'
               f'rm -rf {base} /srv/mmx-account-demo\nrm -f /var/mail/mmx-account-retired')

    cleanup()
    remote(f'mkdir {base}\n! getent passwd 42020\n! getent group 42020')
    try:
        assert run('validate')['valid']
        run('dryrun')
        remote(f'! getent passwd {name}\n! getent group {name}\ntest ! -e {installed}\ntest ! -e {drop}')
        remote(f'mkdir -p {installed}.d\nprintf "[Service]\\nEnvironment=UNMANAGED=kept\\n" > {installed}.d/90-unmanaged.conf')
        run('run')
        assert remote(f'id -u {name}').strip() == '42020'
        assert remote(f'id -g {name}').strip() == '42020'
        assert prop('User') == name and prop('MemoryMax') == str(64 << 20)
        pid = prop('MainPID')
        assert 'MMX_PROFILE=demo\n' in environment(pid) and 'UNMANAGED=kept\n' in environment(pid)
        before = remote(f'stat -c "%i %y %z" {installed} {drop}')
        assert run('run')['entries'][0]['response']['change'] == 'unchanged'
        assert prop('MainPID') == pid and remote(f'stat -c "%i %y %z" {installed} {drop}') == before
        # An unchanged preview writes no unit candidates and is complete.
        directories = remote(f'stat -c "%y %z" /etc/systemd/system {installed}.d')
        preview = run('dryrun', 0)
        assert preview['entries'][0]['response']['change'] == 'unchanged'
        assert remote(f'stat -c "%y %z" /etc/systemd/system {installed}.d') == directories
        mark('shipped example: typed IDs, non-root service, combined drop-in, preserved other drop-in, unchanged rerun and complete dryrun')

        for task in [
            {'user': {'create_home_directory': True, 'name': name, 'uid': 42021, 'home': base + '/must-not-change'}},
            {'group': {'name': name, 'gid': 42021}},
            {'user': {'create_home_directory': True, 'name': 'mmx-account-collision', 'uid': 42020}},
            {'group': {'name': 'mmx-account-collision', 'gid': 42020}},
        ]:
            project([task])
            record = run('run', 1)
            assert 'renumbering' in json.dumps(record) or 'already used' in json.dumps(record)
        remote(f'test "$(getent passwd {name} | cut -d: -f6)" = /nonexistent\n! getent passwd mmx-account-collision\n! getent group mmx-account-collision')
        project([{'user': {'create_home_directory': False, 'name': name}}, {'group': {'name': name}}])
        assert run('run')['entries'][0]['response']['change'] == 'unchanged'
        mark('ID mismatches/collisions fail before other account changes; omitted IDs preserve existing accounts')

        # Native refusal must leave the account/group and live process intact.
        project([{'group': {'name': name, 'state': 'absent'}}])
        run('run', 1)
        remote(f'getent group {name}')
        retired = 'mmx-account-retired'
        project([{'user': {'create_home_directory': False, 'name': retired, 'home': base + '/home', 'system': True}}])
        run('run')
        remote(f'mkdir {base}/home\nprintf kept > {base}/home/file\nprintf mail > /var/mail/{retired}\n'
               f'chown -R {retired}:{retired} {base}/home /var/mail/{retired}\n'
               f'systemd-run --unit=mmx-account-busy --uid={retired} /bin/sleep infinity')
        project([{'user': {'name': retired, 'state': 'absent'}}])
        run('dryrun')
        remote(f'getent passwd {retired}')
        run('run', 1)
        remote(f'getent passwd {retired}\nsystemctl is-active mmx-account-busy.service\nsystemctl stop mmx-account-busy.service')
        run('run')
        remote(f'! getent passwd {retired}\n! getent group {retired}\n'
               f'test "$(cat {base}/home/file)" = kept\ntest "$(cat /var/mail/{retired})" = mail')
        assert run('run')['entries'][0]['response']['change'] == 'unchanged'
        mark('native in-use refusal, read-only removal preview, preserved home/mail, native private-group removal, already-absent no-op')

        # Accounts for people: a full name, a home directory from the node's
        # skeleton, a password hash from a secret, a password lock, groups
        # that list their members, keys read from files, and removal with the
        # home directory. Each state is read from the node's own databases.
        person, second, service, primary = PEOPLE
        hashes = {name: vm.cmd(['openssl', 'passwd', '-6', '-salt', 'mmx' + name, name + '-password']).stdout.strip()
                  for name in ('first', 'chosen', 'second')}
        secret_values.extend(hashes.values())
        vm.ENV['MMX_TEST_PASSWORD_HASH'] = hashes['first']
        with_secret = {'secrets_from_env': {'person_password_hash': 'MMX_TEST_PASSWORD_HASH'}}
        started = remote('date +%s').strip()

        def person_task(**fields):
            task = {'create_home_directory': True, 'name': person, 'full_name': 'Alice Example', 'shell': '/bin/bash',
                    'password_hash': '{{ .secrets.person_password_hash }}', 'apply_password_hash': 'only_when_account_has_no_password'}
            task.update(fields)
            return {'user': {k: v for k, v in task.items() if v is not None}}

        def shadow(account):
            return remote(f'getent shadow {account} | cut -d: -f2').strip()

        def passwd(account):
            return remote(f'getent passwd {account}').strip().split(':')

        def first_result(record):
            return record['entries'][0]['response']['results'][0]

        # The oracle for a home directory is the distribution's own useradd: an
        # account it makes with -m on this node has the mode and the files that
        # MMX must give a home directory it makes. A file in /etc/skel makes the
        # skeleton differ from the distribution's own, which openSUSE merges
        # with the one in /usr/etc/skel.
        remote(f'userdel -r {reference} 2>/dev/null || true\nprintf marker > /etc/skel/{skeleton_marker}\nuseradd -m {reference}')
        reference_mode = remote(f'stat -c "%a" /home/{reference}').strip()
        reference_files = remote(f'cd /home/{reference} && find . -mindepth 1 | sort')
        assert skeleton_marker in reference_files, reference_files
        selinux = remote(f'if {vm.SELINUX_RUNS}; then echo on; fi').strip() == 'on'

        def check_home(account):
            home = '/home/' + account
            assert remote(f'stat -c "%U %G %a" {home}').split() == [account, account, reference_mode], remote(f'stat -c "%U %G %a" {home}')
            assert remote(f'cd {home} && find . -mindepth 1 | sort') == reference_files
            assert remote(f'find {home} -mindepth 1 ! -user {account} -print').strip() == ''
            if selinux:
                assert remote(f'restorecon -n -v -R {home}').strip() == ''

        project([person_task()], **with_secret)
        preview = run('dryrun')
        result = first_result(preview)
        assert result['change'] == 'would_change' and result.get('secret') is True, result
        remote(f'! getent passwd {person}\ntest ! -e /home/{person}')
        run('run')
        fields = passwd(person)
        assert fields[4] == 'Alice Example' and fields[5] == '/home/' + person and fields[6] == '/bin/bash', fields
        assert shadow(person) == hashes['first']
        check_home(person)
        assert run('run')['entries'][0]['response']['change'] == 'unchanged'
        assert run('dryrun', 0)['entries'][0]['response']['change'] == 'unchanged'
        mark('a new account gets its full name, shell, a home directory from the skeleton with useradd\'s permissions, '
             'and the password hash from a secret; the preview is suppressed and changes nothing; reruns unchanged')

        # only_when_account_has_no_password keeps a password the person set later.
        remote(f'printf "%s\\n" {shlex.quote(person + ":" + hashes["chosen"])} | chpasswd -e')
        assert run('run')['entries'][0]['response']['change'] == 'unchanged' and shadow(person) == hashes['chosen']
        # on_every_run sets the given hash, through chpasswd's standard input:
        # a wrapper records chpasswd's arguments, and a sampler reads every
        # process command line while the run lasts.
        real = remote('command -v chpasswd').strip()
        patterns = '\n'.join([*hashes.values(), '--encrypted'])
        # The patterns go through standard input: on a command line, the
        # sampler would find them there.
        vm.cmd([*ssh, 'sudo -n /bin/sh -c ' + shlex.quote(f'mkdir -p {base} && cat > {base}/patterns')], input=patterns + '\n')
        remote(f"""cat > /usr/local/sbin/chpasswd <<'WRAPPER'
#!/bin/sh
printf '%s\\n' "$*" >> {base}/chpasswd-arguments
sleep 1
exec {real} "$@"
WRAPPER
chmod 755 /usr/local/sbin/chpasswd
cat > {base}/sampler <<'SAMPLER'
while test -e {base}/patterns && test ! -e {base}/stop-sampler; do
  cat /proc/[0-9]*/cmdline 2>/dev/null | tr '\\000' '\\n' | grep -a -F -f {base}/patterns >> {base}/command-lines
done
SAMPLER
touch {base}/command-lines
nohup sh {base}/sampler >/dev/null 2>&1 &""")
        vm.ENV['MMX_TEST_PASSWORD_HASH'] = hashes['second']
        project([person_task(apply_password_hash='on_every_run')], **with_secret)
        changed = first_result(run('run'))
        assert changed['change'] == 'changed' and changed.get('secret') is True, changed
        remote(f'touch {base}/stop-sampler\nsleep 1\nrm -f /usr/local/sbin/chpasswd')
        assert shadow(person) == hashes['second']
        assert remote(f'cat {base}/chpasswd-arguments').strip() == '--encrypted'
        lines = remote(f'cat {base}/command-lines')
        assert '--encrypted' in lines, 'the sampler never saw chpasswd running'
        assert not any(h in lines for h in hashes.values()), 'a password hash appeared on a process command line'
        assert run('run')['entries'][0]['response']['change'] == 'unchanged'
        mark('only_when_account_has_no_password keeps the password the person set; on_every_run sets the given hash, which goes '
             'to chpasswd on standard input and never on a command line')

        project([person_task(apply_password_hash='on_every_run', password_locked=True)], **with_secret)
        run('run')
        assert shadow(person) == '!' + hashes['second']
        assert run('run')['entries'][0]['response']['change'] == 'unchanged'
        # A new hash for a locked account is stored locked, in one write.
        vm.ENV['MMX_TEST_PASSWORD_HASH'] = hashes['first']
        run('run')
        assert shadow(person) == '!' + hashes['first']
        assert run('run')['entries'][0]['response']['change'] == 'unchanged'
        vm.ENV['MMX_TEST_PASSWORD_HASH'] = hashes['second']
        project([person_task(apply_password_hash='on_every_run', password_locked=False)], **with_secret)
        run('run')
        assert shadow(person) == hashes['second']
        assert run('run')['entries'][0]['response']['change'] == 'unchanged'
        project([person_task(apply_password_hash='on_every_run', password_locked=True)], **with_secret)
        run('run')
        project([person_task(apply_password_hash='on_every_run', password_locked=False)], **with_secret)
        run('run')
        assert shadow(person) == hashes['second']
        mark('password_locked locks and unlocks password login and keeps the stored hash; a new hash for a locked '
             'account stays locked')

        # A missing home directory of an existing account is made again; an
        # existing one is never changed.
        remote(f'rm -rf /home/{person}')
        project([person_task(password_hash=None, apply_password_hash=None, create_home_directory=False)])
        assert run('run')['entries'][0]['response']['change'] == 'unchanged'
        remote(f'test ! -e /home/{person}')
        project([person_task(password_hash=None, apply_password_hash=None, create_home_directory=True)])
        assert 'home_directory_created' in json.dumps(run('dryrun'))
        remote(f'test ! -e /home/{person}')
        run('run')
        check_home(person)
        remote(f'chmod 711 /home/{person}\ntouch /home/{person}/kept')
        assert run('run')['entries'][0]['response']['change'] == 'unchanged'
        assert remote(f'stat -c %a /home/{person}').strip() == '711'
        mark("an existing account's missing home directory is made from the skeleton with useradd's permissions only "
             'when the task asks for it; an existing home directory is left alone')

        project([{'user': {'name': second, 'create_home_directory': False}},
                 {'user': {'create_home_directory': False, 'name': service, 'system': True}}])
        run('run')
        assert passwd(second)[5] == '/home/' + second and passwd(service)[5:] == ['/nonexistent', '/usr/sbin/nologin']
        remote(f'test ! -e /home/{second}\ntest ! -e /nonexistent\ntest -x /usr/sbin/nologin')
        assert shadow(second) in ('!', '!!', '*') and shadow(service) in ('!', '!!', '*')
        project([{'user': {'create_home_directory': False, 'name': service, 'password_locked': False}}])
        assert 'no password hash' in json.dumps(run('run', 1))
        assert shadow(service) in ('!', '!!', '*')
        for task, message in [({'name': second, 'password_hash': hashes['first'], 'apply_password_hash': 'on_every_run'}, 'one whole secret reference'),
                              ({'name': second, 'password_hash': '{{ .secrets.person_password_hash }}'}, 'apply_password_hash is required'),
                              ({'name': second, 'full_name': 'A: B'}, 'full_name must not contain'),
                              ({'name': second, 'also_delete_home_directory_and_mail': True}, 'only for state: absent')]:
            task['create_home_directory'] = False
            project([{'user': task}], **with_secret)
            invalid = vm.cmd([mmx, 'validate', '--json'], cwd=workspace, check=False)
            assert invalid.returncode != 0 and message in invalid.stdout + invalid.stderr, (task, invalid.stdout, invalid.stderr)
            assert hashes['first'] not in invalid.stdout + invalid.stderr
        mark('create_home_directory: false makes no home; a system account gets /usr/sbin/nologin; '
             'unlocking an account without a hash fails; a hash in the YAML, a missing apply_password_hash, and a full name '
             'with a colon are validation errors')

        # A group's supplementary members. An account whose primary group it
        # is stays, even with supplementary_members_not_listed: remove.
        remote(f'groupadd {TEAM}\nuseradd -M -g {TEAM} {primary}\ngpasswd -a {primary} {TEAM} >/dev/null\ngpasswd -a {service} {TEAM} >/dev/null')

        def members():
            return sorted(m for m in remote(f'getent group {TEAM} | cut -d: -f4').strip().split(',') if m)

        project([{'group': {'name': TEAM, 'supplementary_members': [person, second], 'supplementary_members_not_listed': 'remove'}}])
        preview = first_result(run('dryrun'))
        assert preview['value'] == {'members_added': [person, second], 'members_removed': [service]}, preview
        assert members() == sorted([primary, service])
        run('run')
        assert members() == sorted([person, second, primary])
        assert remote(f'id -nG {person}').split().count(TEAM) == 1
        assert run('run')['entries'][0]['response']['change'] == 'unchanged'
        project([{'group': {'name': TEAM, 'supplementary_members': [service], 'supplementary_members_not_listed': 'keep'}}])
        run('run')
        assert members() == sorted([person, second, primary, service])
        project([{'group': {'name': TEAM, 'supplementary_members': [], 'supplementary_members_not_listed': 'remove'}}])
        run('run')
        assert members() == [primary] and remote(f'id -gn {primary}').strip() == TEAM
        project([{'group': {'name': TEAM, 'supplementary_members': ['mmx-account-nobody'], 'supplementary_members_not_listed': 'keep'}}])
        assert first_result(run('dryrun'))['status'] == 'not_checked'
        run('run', 1)
        mark('group members: remove makes the listed accounts the whole supplementary set, keep only adds, an account with '
             'the group as its primary group is not touched, and a missing account is not checked in a preview and fails a run')

        # Keys read from files on the controller machine. A resume uses the
        # keys saved with the run; a fresh run reads the files again.
        keys = {name: vm.cmd(['sh', '-c', f'rm -f {workspace}/{name}-key*; ssh-keygen -q -t ed25519 -N "" -C {name}@laptop -f {workspace}/{name}-key '
                              f'&& cat {workspace}/{name}-key.pub']).stdout.strip() for name in ('alice', 'bob', 'carol', 'ci')}
        team_keys = workspace / 'files/team_keys'
        if team_keys.exists():
            shutil.rmtree(team_keys)
        team_keys.mkdir(parents=True)
        (team_keys / 'alice.pub').write_text(keys['alice'] + '\n')
        (team_keys / 'bob.pub').write_text('# Bob\n\n' + keys['bob'] + '\n')
        (team_keys / 'README').write_text('not a key\n')
        (workspace / 'files/ci.keys').write_text('# the CI machine\n' + keys['ci'] + '\n')

        def keys_tasks(*extra):
            return [{'authorized_keys': {'for_user_account': person, 'key_files_on_controller_machine': ['files/ci.keys'], 'key_directories_on_controller_machine': ['files/team_keys'],
                                         'keys_not_listed': 'remove'}}, *extra]

        def installed_keys():
            return remote(f'cat /home/{person}/.ssh/authorized_keys')

        project(keys_tasks({'shell': {'script': f'test ! -e {base}/fail', 'changed_when': False}}))
        assert len(first_result(run('dryrun'))['value']['added']) == 3
        remote(f'touch {base}/fail')
        failed = run('run', 1)
        assert installed_keys() == keys['ci'] + '\n' + keys['alice'] + '\n' + keys['bob'] + '\n'
        (team_keys / 'bob.pub').write_text(keys['carol'] + '\n')
        remote(f'rm {base}/fail')
        run('resume', 0, failed['id'])
        assert keys['bob'] in installed_keys() and keys['carol'] not in installed_keys()
        run('run')
        assert installed_keys() == keys['ci'] + '\n' + keys['alice'] + '\n' + keys['carol'] + '\n'
        (team_keys / 'alice.pub').unlink()
        run('run')
        assert installed_keys() == keys['ci'] + '\n' + keys['carol'] + '\n'
        assert run('run')['entries'][0]['response']['change'] == 'unchanged'
        for key_file, body, message in [('broken.pub', keys['alice'] + '\nnot a key\n', 'files/team_keys/broken.pub line 2'),
                                    ('empty.pub', '# nobody yet\n', 'key file files/team_keys/empty.pub holds no key line'),
                                    ('again.pub', keys['carol'] + '\n', 'in key_directories_on_controller_machine[0] files/team_keys/again.pub line 1 and in key_directories_on_controller_machine[0] files/team_keys/carol.pub line 1')]:
            (team_keys / key_file).write_text(body)
            if key_file == 'again.pub':
                (team_keys / 'carol.pub').write_text(keys['carol'] + '\n')
                (team_keys / 'bob.pub').unlink()
            invalid = vm.cmd([mmx, 'validate', '--json'], cwd=workspace, check=False)
            assert invalid.returncode != 0 and message in invalid.stdout + invalid.stderr, (key_file, invalid.stdout, invalid.stderr)
            (team_keys / key_file).unlink()
        # A directory of keys for each environment, chosen by a var.
        for deployment_environment, person_key in [('production', 'alice'), ('staging', 'bob')]:
            (workspace / 'files/environment_keys' / deployment_environment).mkdir(parents=True, exist_ok=True)
            (workspace / 'files/environment_keys' / deployment_environment / (person_key + '.pub')).write_text(keys[person_key] + '\n')
        for deployment_environment, person_key in [('production', 'alice'), ('staging', 'bob'), ('production', 'alice')]:
            project([{'authorized_keys': {'for_user_account': person, 'key_directories_on_controller_machine': ['files/environment_keys/{{ .vars.environment }}'],
                                          'keys_not_listed': 'remove'}}], vars={'environment': deployment_environment})
            run('run')
            assert installed_keys() == keys[person_key] + '\n', (deployment_environment, installed_keys())
            assert run('run')['entries'][0]['response']['change'] == 'unchanged'
        mark('authorized_keys reads key_files_on_controller_machine and the .pub files of key_directories_on_controller_machine in name order; a resume uses the keys '
             'saved with the run; deleting a file removes that key; a malformed line, an empty file, and a repeated key '
             'fail validation and name the file and line; a key directory chosen by a var gives that directory\'s keys')

        # Removal: the account, its home directory, and its mail spool. Tasks
        # that take keys away from an account that is gone change nothing.
        # A distribution whose login.defs sets no MAIL_DIR (openSUSE) has no mail
        # spool that userdel -r removes.
        has_mail_spool = remote('grep -qs "^MAIL_DIR" /usr/etc/login.defs /etc/login.defs && echo yes || echo no').strip() == 'yes'
        if has_mail_spool:
            remote(f'printf mail > /var/mail/{person}\nchown {person} /var/mail/{person}')
        project([{'user': {'name': person, 'state': 'absent', 'also_delete_home_directory_and_mail': True}}])
        run('dryrun')
        remote(f'getent passwd {person}\ntest -d /home/{person}')
        run('run')
        remote(f'! getent passwd {person}\ntest ! -e /home/{person}\ntest ! -e /var/mail/{person}')
        assert run('run')['entries'][0]['response']['change'] == 'unchanged'
        project([{'authorized_keys': {'for_user_account': person, 'keys': [keys['alice']], 'state': 'absent'}},
                 {'authorized_keys': {'for_user_account': person, 'keys': [], 'keys_not_listed': 'remove'}}])
        record = run('run')
        assert [r['change'] for r in record['entries'][0]['response']['results']] == ['unchanged', 'unchanged'], record
        mark('state: absent with also_delete_home_directory_and_mail removes the account, its home directory, and its mail spool; '
             'removing keys of an account that is gone is unchanged')

        # No hash in the controller's saved runs, or on the node.
        for path in (workspace / '.mmx').rglob('*.json'):
            assert not any(h in path.read_text() for h in hashes.values()), 'a password hash in ' + str(path)
        saved = remote("find /var/lib/mmx -type f \\( -name '*.json' -o -name worker.log \\) -exec cat {} +")
        saved += remote(f'journalctl -q --no-pager --since @{started} -u mmx-https-api.service')
        assert not any(h in saved for h in hashes.values()), 'a password hash in node history or the journal'
        mark('no password hash in mmx output, saved results on the controller machine or the node, or the journal')

        # Changed unit bytes must restart once even if resume finds them unchanged.
        settings_file.write_text(original_settings.replace('profile: demo', 'profile: blue').replace('memory_max: 64M', 'memory_max: 80M'))
        (workspace / 'playbooks/account_service.yml').write_text(original.replace(
            '          activate:', '          validate:\n            - shell: {script: "test ! -e ' + base + '/fail"}\n          activate:'))
        remote(f'touch {base}/fail')
        failed = run('run', 1)
        assert prop('MainPID') == pid and 'MMX_PROFILE=demo\n' in environment(pid)
        assert 'MMX_PROFILE=blue' in remote(f'cat {drop}')
        source.write_text('[Service]\nMemoryMax=96M\nEnvironment=MMX_PROFILE=current-source\n')
        remote(f'rm {base}/fail')
        recovered = run('resume', 0, failed['id'])
        next_pid = prop('MainPID')
        assert next_pid != pid and 'MMX_PROFILE=blue\n' in environment(next_pid)
        assert prop('MemoryMax') == str(80 << 20)
        assert run('results', 0, recovered['id'])['status'] == 'succeeded'
        run('run')
        pid = prop('MainPID')
        assert 'MMX_PROFILE=current-source\n' in environment(pid)
        assert prop('MemoryMax') == str(96 << 20)
        assert run('run')['entries'][0]['response']['change'] == 'unchanged' and prop('MainPID') == pid
        mark('drop-in restart recovery uses frozen templates; fresh run uses current bytes, then remains unchanged')

        live = remote(f'cat {drop}')
        for bad in ['[Service]\nMemoryMax=invalid-value\n', '[Service]\nExecStart=/bin/true\n']:
            source.write_text(bad)
            record = run('run', 1)
            assert 'validation' in json.dumps(record)
            # systemd_unit has no check_new_file field; its failure names its own check.
            assert "systemd_unit: systemd's check of the new unit file failed: " in json.dumps(record) and 'check_new_file failed' not in json.dumps(record), record
            assert remote(f'cat {drop}') == live and prop('MainPID') == pid
        remote(f'test -f {installed}.d/90-unmanaged.conf')
        mark('invalid values and incompatible combined ExecStart candidates preserve live files and the running process')

        base_content = '[Service]\nUser=' + name + '\nGroup=' + name + '\nExecStart=/bin/sleep infinity\nEnvironment=MMX_PROFILE=base\n[Install]\nWantedBy=multi-user.target\n'
        task_block = {
            "task_block": {
                "id": "remove_drop_in",
                "changes": [
                    {"systemd_unit": {"name": unit, "content": base_content}},
                    {"systemd_unit": {"name": unit, "drop_in": "20-limits.conf", "state": "absent"}},
                ],
                "validate": [{"shell": {"script": "test ! -e " + base + "/fail"}}],
                "activate": [{"systemd_service": {"name": name, "start_at_boot": True, "state": "running"}}],
            }
        }
        project([task_block])
        remote(f'touch {base}/fail')
        failed = run('run', 1)
        remote(f'test ! -e {drop}\ntest -f {installed}.d/90-unmanaged.conf')
        assert prop('MainPID') == pid
        remote(f'rm {base}/fail')
        run('resume', 0, failed['id'])
        pid = prop('MainPID')
        assert 'MMX_PROFILE=base\n' in environment(pid) and 'UNMANAGED=kept\n' in environment(pid)
        assert run('run')['entries'][0]['response']['change'] == 'unchanged' and prop('MainPID') == pid
        mark('drop-in removal preserves other files and restarts once after failure before activation')

        project([{'systemd_unit': {'name': 'mmx-account-missing.service', 'drop_in': '20-limits.conf', 'content': '[Service]\nMemoryMax=64M\n'}}])
        preview = run('dryrun')
        assert 'not checked' in json.dumps(preview)
        run('run', 1)
        remote('test ! -e /etc/systemd/system/mmx-account-missing.service.d')
        remote("printf '[Service]\\n' > /etc/systemd/system/mmx-account-repair.service\nsystemctl daemon-reload")
        assert prop('LoadState', 'mmx-account-repair.service') == 'bad-setting'
        project([{'systemd_unit': {'name': 'mmx-account-repair.service', 'drop_in': '20-exec.conf',
                          'content': '[Service]\nExecStart=/bin/sleep infinity\n'}}])
        run('run')
        assert prop('LoadState', 'mmx-account-repair.service') == 'loaded'
        # The drop-in directory MMX made is readable like /etc/systemd/system.
        assert remote('stat -c %a /etc/systemd/system/mmx-account-repair.service.d').strip() == '755'
        mark('missing base previews as not checked and fails apply; a drop-in can repair an incomplete installed base')

        remote(f'printf untouched > {base}/outside\nln -s {base}/outside {drop}')
        project([{'systemd_unit': {'name': unit, 'drop_in': '20-limits.conf', 'state': 'absent'}}])
        run('run', 1)
        remote(f'test "$(cat {base}/outside)" = untouched\nrm {drop}\n'
               f'ln -s {base} /etc/systemd/system/mmx-account-link.service.d')
        project([{'systemd_unit': {'name': 'mmx-account-link.service', 'drop_in': 'outside.conf', 'content': '[Service]\nMemoryMax=64M\n'}}])
        run('run', 1)
        remote(f'test ! -e {base}/outside.conf\nrm /etc/systemd/system/mmx-account-link.service.d\n'
               f'ln -s {base}/outside /etc/systemd/system/mmx-account-link.service')
        project([{'systemd_unit': {'name': 'mmx-account-link.service', 'state': 'absent'}}])
        run('run', 1)
        remote(f'test "$(cat {base}/outside)" = untouched\nrm /etc/systemd/system/mmx-account-link.service')
        mark('managed unit/drop-in links and linked parent directories never redirect writes or removal')

        vendor = 'mmx-vendor-demo.service'
        remote("printf '[Service]\\nExecStart=/bin/sleep infinity\\nEnvironment=MMX_PROFILE=vendor\\n[Install]\\nWantedBy=multi-user.target\\n' > /usr/lib/systemd/system/" + vendor)
        vendor_content = base_content.replace('User=' + name + '\n', '').replace('Group=' + name + '\n', '')
        project([{'systemd_unit': {'name': vendor, 'content': vendor_content.replace('MMX_PROFILE=base', 'MMX_PROFILE=override')}},
                 {'systemd_service': {'name': vendor, 'start_at_boot': True, "state": "running"}}])
        run('run')
        vendor_pid = prop('MainPID', vendor)
        enabled_link = remote('readlink /etc/systemd/system/multi-user.target.wants/' + vendor)
        project([{'systemd_unit': {'name': vendor, 'state': 'absent'}}, {'systemd_unit': {'name': unit, 'state': 'absent'}}])
        run('run')
        assert prop('MainPID', vendor) == vendor_pid and prop('MainPID') == pid
        # Debian 12 reports the path through its /lib link.
        assert remote('realpath ' + prop('FragmentPath', vendor)).strip() == '/usr/lib/systemd/system/' + vendor
        assert 'MMX_PROFILE=override\n' in environment(vendor_pid)
        assert remote('readlink /etc/systemd/system/multi-user.target.wants/' + vendor) == enabled_link
        assert run('run')['entries'][0]['response']['change'] == 'unchanged'
        mark('whole-unit removal leaves running services and enable links alone; vendor definition reappears when present')

        # A timer replaces cron: activate names the timer, and the oneshot
        # service it fires needs no service task.
        def timer_task_block(version, interval):
            return {
                "task_block": {
                    "id": "tick",
                    "changes": [
                        {
                            "systemd_unit": {
                                "name": tick + ".service",
                                "content": f"[Service]\nType=oneshot\nExecStart=/bin/sh -c 'echo {version} >> {base}/ticks'\n",
                            }
                        },
                        {
                            "systemd_unit": {
                                "name": tick + ".timer",
                                "content": f"[Timer]\nOnActiveSec=1s\nOnUnitActiveSec={interval}\nAccuracySec=100ms\n\n[Install]\nWantedBy=timers.target\n",
                            }
                        },
                    ],
                    "activate": [{"systemd_service": {"name": tick + ".timer", "start_at_boot": True, "state": "running"}}],
                }
            }

        def last_tick():
            return remote(f'cat {base}/ticks 2>/dev/null || true').split()[-1:]

        project([timer_task_block('v1', '2s')])
        run('run')
        eventually(lambda: last_tick() == ['v1'])
        timer = prop('InvocationID', tick + '.timer')
        assert prop('UnitFileState', tick + '.timer') == 'enabled'
        assert run('run')['entries'][0]['response']['change'] == 'unchanged'
        assert prop('InvocationID', tick + '.timer') == timer
        project([timer_task_block('v2', '2s')])
        run('run')
        eventually(lambda: last_tick() == ['v2'])
        assert prop('InvocationID', tick + '.timer') == timer
        project([timer_task_block('v2', '3s')])
        run('run')
        assert prop('InvocationID', tick + '.timer') != timer and 'OnUnitActiveUSec=3s' in prop('TimersMonotonic', tick + '.timer')
        timer = prop('InvocationID', tick + '.timer')
        assert run('run')['entries'][0]['response']['change'] == 'unchanged'
        assert prop('InvocationID', tick + '.timer') == timer
        mark('enabled timer fires its oneshot service; a changed service runs at the next firing without restarting the timer; '
             'a changed timer restarts once; reruns unchanged')

        # Socket activation: a changed service template serves the next
        # connection; a changed socket reopens on its new address.
        def socket_task_block(reply, port):
            return {
                "task_block": {
                    "id": "echo",
                    "changes": [
                        {
                            "systemd_unit": {
                                "name": echo + "@.service",
                                "content": f"[Service]\nExecStart=/bin/echo {reply}\nStandardInput=socket\nStandardOutput=socket\n",
                            }
                        },
                        {
                            "systemd_unit": {
                                "name": echo + ".socket",
                                "content": f"[Socket]\nListenStream=127.0.0.1:{port}\nAccept=yes\n\n[Install]\nWantedBy=sockets.target\n",
                            }
                        },
                    ],
                    "activate": [{"systemd_service": {"name": echo + ".socket", "start_at_boot": True, "state": "running"}}],
                }
            }

        def ask(port):
            return remote(f"bash -c 'exec 3<>/dev/tcp/127.0.0.1/{port}; cat <&3'").strip()

        project([socket_task_block('one', 18191)])
        run('run')
        assert ask(18191) == 'one' and prop('UnitFileState', echo + '.socket') == 'enabled'
        listener = prop('InvocationID', echo + '.socket')
        assert run('run')['entries'][0]['response']['change'] == 'unchanged'
        project([socket_task_block('two', 18191)])
        run('run')
        assert ask(18191) == 'two' and prop('InvocationID', echo + '.socket') == listener
        project([socket_task_block('two', 18192)])
        run('run')
        assert ask(18192) == 'two' and prop('InvocationID', echo + '.socket') != listener
        remote("! bash -c 'exec 3<>/dev/tcp/127.0.0.1/18191' 2>/dev/null")
        assert run('run')['entries'][0]['response']['change'] == 'unchanged'
        mark('enabled socket activates its service template; a changed template serves the next connection without '
             'reopening the socket; a changed socket reopens on its new address; reruns unchanged')

        def mount_task_block(size, mount_point=where):
            return {
                "task_block": {
                    "id": "data",
                    "changes": [
                        {
                            "systemd_unit": {
                                "name": mount,
                                "content": f"[Mount]\nWhat=tmpfs\nWhere={mount_point}\nType=tmpfs\nOptions=size={size},mode=0755\n\n[Install]\nWantedBy=multi-user.target\n",
                            }
                        }
                    ],
                    "activate": [{"systemd_service": {"name": mount, "start_at_boot": True, "state": "running"}}],
                }
            }

        def mounted():
            return remote(f'findmnt -no FSTYPE,OPTIONS {where} || true')

        project([mount_task_block('1m', '/srv/mmx-units-elsewhere')])
        assert 'validation' in json.dumps(run('run', 1))
        remote(f'test ! -e /etc/systemd/system/{quoted_mount}\n! mountpoint -q {where}')
        project([mount_task_block('1m')])
        run('run')
        assert 'tmpfs' in mounted() and 'size=1024k' in mounted()
        assert prop('UnitFileState', quoted_mount) == 'enabled'
        instance = prop('InvocationID', quoted_mount)
        assert run('run')['entries'][0]['response']['change'] == 'unchanged'
        assert prop('InvocationID', quoted_mount) == instance
        # A process outside the mount's dependencies keeps it busy, so systemd
        # refuses the restart; the next run restarts it once the mount is free.
        remote(f"systemd-run --unit=mmx-units-busy /bin/sh -c 'cd {where} && exec sleep infinity'")
        eventually(lambda: remote('readlink /proc/"$(systemctl show -p MainPID --value mmx-units-busy.service)"/cwd').strip() == where)
        project([mount_task_block('2m')])
        run('run', 1)
        assert 'size=1024k' in mounted()
        remote('systemctl stop mmx-units-busy.service')
        run('run')
        assert 'size=2048k' in mounted() and prop('InvocationID', quoted_mount) != instance
        assert run('run')['entries'][0]['response']['change'] == 'unchanged'
        mark('mount unit named for its escaped path mounts and is enabled; a misnamed mount fails verification; '
             'a busy mount fails its restart and the next run remounts it')

        project([{'systemd_service': {'name': name, 'start_at_boot': True}}])
        invalid = vm.cmd([mmx, 'validate', 'account_service'], cwd=workspace, check=False)
        assert invalid.returncode != 0 and 'systemd_service.state is required' in invalid.stdout + invalid.stderr, invalid
        mark('systemd_service requires state even when the task only sets start_at_boot')

        # A target restarts its members as one. The member is enabled with
        # do_not_start_or_stop, which never starts it: the target pulls it in.
        def workers_task_block(description, sleep='infinity'):
            return {
                "task_block": {
                    "id": "workers",
                    "changes": [
                        {
                            "systemd_unit": {
                                "name": worker,
                                "content": f"[Unit]\nPartOf={workers}\n\n[Service]\nSlice={batch}\nExecStart=/bin/sleep {sleep}\n\n[Install]\nWantedBy={workers}\n",
                            }
                        },
                        {"systemd_unit": {"name": batch, "content": "[Slice]\nMemoryMax=64M\n"}},
                        {
                            "systemd_unit": {
                                "name": workers,
                                "content": f"[Unit]\nDescription={description}\n\n[Install]\nWantedBy=multi-user.target\n",
                            }
                        },
                    ],
                    "activate": [
                        {"systemd_service": {"name": worker, "state": "do_not_start_or_stop", "start_at_boot": True}},
                        {"systemd_service": {"name": workers, "start_at_boot": True, "state": "running"}},
                    ],
                }
            }

        project([workers_task_block('workers v1')])
        run('run')
        assert prop('ActiveState', workers) == 'active' and prop('ActiveState', worker) == 'active'
        assert prop('UnitFileState', worker) == 'enabled' and prop('MemoryMax', batch) == str(64 << 20)
        assert prop('Slice', worker) == batch
        member, group = prop('MainPID', worker), prop('InvocationID', workers)
        assert run('run')['entries'][0]['response']['change'] == 'unchanged'
        assert prop('MainPID', worker) == member and prop('InvocationID', workers) == group
        project([workers_task_block('workers v2')])
        run('run')
        assert prop('InvocationID', workers) != group and prop('MainPID', worker) != member
        # do_not_start_or_stop refreshes the running member after its own change and
        # leaves a stopped member stopped, with nothing owed when it starts.
        member, group = prop('MainPID', worker), prop('InvocationID', workers)
        task_block = workers_task_block('workers v2', '86400')
        task_block['task_block']['activate'].pop()
        project([task_block])
        run('run')
        assert prop('MainPID', worker) != member and prop('InvocationID', workers) == group
        remote(f'systemctl stop {worker}')
        task_block = workers_task_block('workers v2', '86401')
        task_block['task_block']['activate'].pop()
        project([task_block])
        run('run')
        assert prop('ActiveState', worker) == 'inactive'
        remote(f'systemctl start {worker}')
        member = prop('MainPID', worker)
        assert run('run')['entries'][0]['response']['change'] == 'unchanged' and prop('MainPID', worker) == member
        mark('target, slice, and member service: the target starts and restarts its member; do_not_start_or_stop enables without '
             'starting, refreshes a running unit, and leaves a stopped unit stopped')

        # An automount arms the mount point; the first access mounts it.
        project([{'systemd_service': {'name': mount, 'state': 'stopped', 'start_at_boot': False}},
                 {'task_block': {'id': 'data_on_demand',
                                 'changes': [{'systemd_unit': {'name': 'srv-mmx\\x2dunits\\x2ddata.automount',
                                                               'content': f'[Automount]\nWhere={where}\n\n[Install]\nWantedBy=multi-user.target\n'}}],
                                 'activate': [{'systemd_service': {'name': 'srv-mmx\\x2dunits\\x2ddata.automount', 'start_at_boot': True, "state": "running"}}]}}])
        run('run')
        assert prop('ActiveState', automount) == 'active' and prop('ActiveState', quoted_mount) == 'inactive'
        remote(f'ls {where} >/dev/null')
        assert 'tmpfs' in mounted() and prop('ActiveState', quoted_mount) == 'active'
        remote(f'systemctl disable --now {automount}\nsystemctl stop {quoted_mount}')
        mark('automount unit named for its escaped path is armed and mounts on first access')

        # A swap unit turns its swap file on. chattr +C is for Btrfs, which
        # refuses a swap file that is copied on write.
        remote(f'rm -f {swap_file}\ntouch {swap_file}\nchattr +C {swap_file} 2>/dev/null || true\n'
               f'dd if=/dev/zero of={swap_file} bs=1M count=16 status=none\nchmod 600 {swap_file}\nmkswap {swap_file} >/dev/null')
        project([{'task_block': {'id': 'swap',
                                 'changes': [{'systemd_unit': {'name': swap, 'content': f'[Swap]\nWhat={swap_file}\n\n[Install]\nWantedBy=swap.target\n'}}],
                                 'activate': [{'systemd_service': {'name': swap, 'start_at_boot': True, "state": "running"}}]}}])
        run('run')
        assert swap_file in remote('swapon --show=NAME --noheadings') and prop('UnitFileState', quoted_swap) == 'enabled'
        assert run('run')['entries'][0]['response']['change'] == 'unchanged'
        project([{'systemd_service': {'name': swap, 'state': 'stopped', 'start_at_boot': False}}])
        run('run')
        assert swap_file not in remote('swapon --show=NAME --noheadings')
        mark('swap unit turns its swap file on and off')

        # Masking: nothing can start a masked unit until a task unmasks it.
        vendor = 'mmx-vendor-demo.service'
        project([{'systemd_service': {'name': vendor, 'state': 'stopped', 'start_at_boot': False, 'masked': True}}])
        assert run('dryrun')['entries'][0]['response']['change'] == 'would_change'
        assert prop('ActiveState', vendor) == 'active'
        run('run')
        assert prop('ActiveState', vendor) == 'inactive' and prop('UnitFileState', vendor) == 'masked'
        remote(f'! systemctl start {vendor} 2>/dev/null')
        assert run('run')['entries'][0]['response']['change'] == 'unchanged'
        project([{'systemd_service': {'name': vendor, "state": "running"}}])
        assert 'write masked: false' in json.dumps(run('run', 1))
        project([{'systemd_service': {'name': vendor, 'masked': False, 'start_at_boot': True, "state": "running"}}])
        run('run')
        assert prop('ActiveState', vendor) == 'active' and prop('UnitFileState', vendor) == 'enabled'
        assert run('run')['entries'][0]['response']['change'] == 'unchanged'
        # systemd masks with a link in /etc/systemd/system and refuses when a
        # unit file is there; MMX says what to do.
        project([{'systemd_service': {'name': tick + '.service', 'state': 'stopped', 'masked': True}}])
        assert 'remove that file first' in json.dumps(run('run', 1))
        project([{'systemd_service': {'name': tick + '.service', "state": "running"}}])
        assert 'runs once and then ends' in json.dumps(run('run', 1))
        mark('masked: true stops and masks, masked: false unmasks before starting; a masked unit, a unit file in /etc, '
             'and a oneshot unit asked to be started each fail with what to do')

        # A command that runs once: the marker file is written after success.
        project([{'shell': {'script': f'test ! -e {base}/fail && echo imported >> {base}/imports',
                            'run_only_once_marker_file': marker}}])
        remote(f'touch {base}/fail')
        assert run('dryrun')['entries'][0]['response']['change'] == 'would_change'
        run('run', 1)
        remote(f'test ! -e {marker}\nrm {base}/fail')
        run('run')
        remote(f'test -f {marker}\ntest "$(cat {base}/imports)" = imported')
        again = run('run')
        assert again['entries'][0]['response']['change'] == 'unchanged' and 'already run; delete the marker file' in json.dumps(again)
        assert run('dryrun', 0)['entries'][0]['response']['change'] == 'unchanged'
        remote(f'rm {marker}')
        run('run')
        remote(f'test -f {marker}\ntest "$(wc -l < {base}/imports)" = 2')
        mark('run_only_once_marker_file: a failed command runs again, a successful one writes the marker and is skipped '
             'until the marker is deleted; dryrun reports from the file')

        # wait_for with a script: only the program can say it is ready.
        project([{'wait_for': {'command_succeeds': 'test "$(cat "$READY_FILE")" = "$(id -un)"', 'run_as_user_account': 'nobody',
                               'run_from_directory': '/tmp', 'env': {'READY_FILE': base + '/ready'}, 'wait_at_most': '30s'}}])
        assert 'not_checked' in json.dumps(run('dryrun'))
        remote(f'chmod 755 {base}\nsystemd-run --on-active=3 /bin/sh -c "echo nobody > {base}/ready"')
        run('run')
        project([{'wait_for': {'command_succeeds': 'echo still starting; exit 3', 'wait_at_most': '3s'}}])
        failed = json.dumps(run('run', 1))
        assert 'the wait_for.command_succeeds script exited with code 3' in failed and 'still starting' in failed
        mark('wait_for script waits until the command exits with 0, runs as the named account with env, and reports '
             'the last exit code and output when the wait ends')

        # A command that follows a change, and checks before a restart. The
        # checks run only when the block would restart the running service or
        # run the command, and before anything is written.
        def cache_task_block(setting, approved):
            return {
                "task_block": {
                    "id": "cache",
                    "changes": [
                        {"systemd_unit": {"name": cache, "content": "[Service]\nExecStart=/bin/sleep infinity\n"}},
                        {"file": {"permissions_mode": "0644", "path": base + "/cache.conf", "content": setting + "\n"}},
                    ],
                    "checks_before_restart_or_reload": [
                        {"assert": {"that": approved, "message": "a restart empties the cache; run again off-peak"}}
                    ],
                    "validate": [{"shell": {"script": f"test ! -e {base}/fail"}}],
                    "activate": [
                        {"systemd_service": {"name": cache, "state": "running"}},
                        {"id": "rebuild_index", "shell": {"script": f"echo rebuilt >> {base}/rebuilds", "only_when_tracked_inputs_changed": True}},
                    ],
                }
            }

        def rebuilds():
            return remote(f'cat {base}/rebuilds 2>/dev/null | wc -l').strip()

        def check_result(record):
            return next(r for r in record['entries'][0]['response']['results'] if '/checks_before_restart_or_reload/' in r['task'])

        project([cache_task_block('v1', True)])
        preview = run('dryrun')
        assert 'would run: tracked input' in json.dumps(preview) and rebuilds() == '0'
        run('run')
        cache_pid = prop('MainPID', cache)
        assert prop('ActiveState', cache) == 'active' and rebuilds() == '1'
        remote('test -n "$(ls /var/lib/mmx/command_records)"')
        project([cache_task_block('v1', False)])
        unchanged = run('run')
        assert unchanged['entries'][0]['response']['change'] == 'unchanged' and check_result(unchanged)['status'] == 'skipped'
        assert prop('MainPID', cache) == cache_pid and rebuilds() == '1'
        project([cache_task_block('v2', False)])
        for command in ('dryrun', 'run'):
            assert 'a restart empties the cache; run again off-peak' in json.dumps(run(command, 1))
            assert remote(f'cat {base}/cache.conf') == 'v1\n' and prop('MainPID', cache) == cache_pid and rebuilds() == '1'
        # A run that wrote the file and stopped before activation still owes
        # the restart and the command, and the checks guard them again.
        project([cache_task_block('v2', True)])
        remote(f'touch {base}/fail')
        run('run', 1)
        assert remote(f'cat {base}/cache.conf') == 'v2\n' and prop('MainPID', cache) == cache_pid and rebuilds() == '1'
        remote(f'rm {base}/fail')
        project([cache_task_block('v2', False)])
        run('run', 1)
        assert prop('MainPID', cache) == cache_pid and rebuilds() == '1'
        project([cache_task_block('v2', True)])
        approved = run('run')
        assert check_result(approved)['status'] == 'succeeded'
        assert prop('MainPID', cache) != cache_pid and rebuilds() == '2'
        cache_pid = prop('MainPID', cache)
        assert run('run')['entries'][0]['response']['change'] == 'unchanged'
        assert prop('MainPID', cache) == cache_pid and rebuilds() == '2'
        mark('a command with only_when_tracked_inputs_changed runs once per change and after an interrupted run; '
             'checks_before_restart_or_reload run only when something would restart, before any write')

        # Units in an account's own systemd: the unit file belongs to the
        # account, its service runs as the account, and MMX turns lingering on.
        home = '/home/' + reports
        worker_unit = 'report-worker.service'
        user_units = home + '/.config/systemd/user'

        def as_account(command):
            return remote(f'runuser -u {reports} -- env XDG_RUNTIME_DIR=/run/user/$(id -u {reports}) {command}').strip()

        def reports_tasks(version, extra_changes=()):
            return [
                {"user": {'create_home_directory': True, "name": reports, "home": home, "shell": "/bin/sh"}},
                {"directory": {"path": home, "owner": reports, "group": reports, "permissions_mode": "0750"}},
                {
                    "task_block": {
                        "id": "report_worker",
                        "changes": [
                            {
                                "systemd_unit": {
                                    "name": worker_unit,
                                    "in_systemd_of_user_account": reports,
                                    "content": f"[Service]\nExecStart=/bin/sleep infinity\nEnvironment=VERSION={version}\n\n[Install]\nWantedBy=default.target\n",
                                }
                            },
                            *extra_changes,
                        ],
                        "activate": [
                            {"systemd_service": {"name": worker_unit, "in_systemd_of_user_account": reports, "start_at_boot": True, "state": "running"}}
                        ],
                    }
                },
            ]

        def worker_pid():
            return as_account(f'systemctl --user show -p MainPID --value {worker_unit}')

        project(reports_tasks('1'))
        run('dryrun')
        remote(f'! id -u {reports} 2>/dev/null')
        first = run('run')
        assert 'Enabled lingering for account ' + reports in json.dumps(first)
        assert remote(f'loginctl show-user {reports} -p Linger --value').strip() == 'yes'
        assert remote(f'stat -c "%U %G %a" {user_units}/{worker_unit}').strip() == f'{reports} {reports} 644'
        assert remote(f'stat -c "%U %a" {home}/.config {user_units}').split() == [reports, '700', reports, '700']
        assert as_account(f'systemctl --user is-active {worker_unit}') == 'active'
        assert as_account(f'systemctl --user is-enabled {worker_unit}') == 'enabled'
        user_pid = worker_pid()
        assert remote(f'ps -o user= -p {user_pid}').strip() in (reports, remote(f'id -u {reports}').strip())
        assert f'VERSION=1\n' in environment(user_pid)
        assert run('run')['entries'][0]['response']['change'] == 'unchanged' and worker_pid() == user_pid
        assert run('dryrun', 0)['entries'][0]['response']['change'] == 'unchanged'
        project(reports_tasks('2'))
        assert 'would_change' in json.dumps(run('dryrun'))
        run('run')
        assert worker_pid() != user_pid and 'VERSION=2\n' in environment(worker_pid())
        user_pid = worker_pid()
        assert run('run')['entries'][0]['response']['change'] == 'unchanged' and worker_pid() == user_pid
        drop_in = {"systemd_unit": {"name": worker_unit, "drop_in": "10-extra.conf", "in_systemd_of_user_account": reports,
                                    "content": "[Service]\nEnvironment=EXTRA=yes\n"}}
        project(reports_tasks('2', [drop_in]))
        run('run')
        assert worker_pid() != user_pid and 'EXTRA=yes\n' in environment(worker_pid())
        owners = remote(f'stat -c "%U %a" {user_units}/{worker_unit}.d {user_units}/{worker_unit}.d/10-extra.conf').split()
        assert owners == [reports, '700', reports, '644'], owners
        user_pid = worker_pid()
        # A unit systemd rejects is not installed, also in an account's systemd.
        # The account runs systemd's check of its own unit. A link in its
        # unit directory to a file that only root can read gives nothing of
        # that file away: systemd names an unknown key in its message, so the
        # file holds one.
        remote(f"printf '[Service]\\nMmxRootOnlySentinel=1\\n' > {base}/root-only.conf\nchmod 600 {base}/root-only.conf\n"
               f'ln -s {base}/root-only.conf {user_units}/{worker_unit}.d/05-linked.conf\nchown -h {reports}:{reports} {user_units}/{worker_unit}.d/05-linked.conf')
        bad = dict(drop_in['systemd_unit'], content='[Service]\nMemoryMax=invalid-value\n')
        project(reports_tasks('2', [{'systemd_unit': bad}]))
        rejected = json.dumps(run('run', 1))
        assert 'validation' in rejected and "systemd_unit: systemd's check of the new unit file failed: " in rejected, rejected
        assert 'MmxRootOnlySentinel' not in rejected and 'private_task_directories' not in rejected, rejected
        # The check's copy was in the node task's private directory, and
        # the place where each one is made is empty again.
        remote(f'rm {user_units}/{worker_unit}.d/05-linked.conf {base}/root-only.conf\n'
               'test -d /run/mmx/private_task_directories\ntest -z "$(ls -A /run/mmx/private_task_directories)"')
        # The link came and went as a drop-in of the unit, so the account's
        # systemd reads the unit's files again before the next run looks.
        as_account('systemctl --user daemon-reload')
        assert 'EXTRA=yes' in remote(f'cat {user_units}/{worker_unit}.d/10-extra.conf') and worker_pid() == user_pid
        project(reports_tasks('2', [drop_in]))
        assert run('run')['entries'][0]['response']['change'] == 'unchanged' and worker_pid() == user_pid
        # Removal: the service task stops and disables, then the files go.
        # The account's systemd is not running here (lingering off, nobody
        # logged in), and the unit is still enabled. MMX reads and changes
        # the account's unit files offline and starts nothing: the account's
        # systemd stays inactive throughout, so the unit never runs.
        uid = remote(f'id -u {reports}').strip()
        wanted_by = f'{user_units}/default.target.wants/{worker_unit}'
        remote(f'loginctl disable-linger {reports}\nsystemctl stop user@{uid}.service\ntest -L {wanted_by}\nsleep 1')
        since = remote("date '+%Y-%m-%d %H:%M:%S'").strip()

        def account_systemd_stayed_stopped():
            # The journal has no line of the account and none about its
            # systemd since it stopped, so neither that systemd nor any of
            # the account's units started in between.
            assert remote(f'systemctl is-active user@{uid}.service || true').strip() == 'inactive'
            lines = remote(f'journalctl --since {shlex.quote(since)} -q -o cat _UID={uid} + UNIT=user@{uid}.service')
            assert lines.strip() == '', lines

        project([{'systemd_service': {'name': worker_unit, 'in_systemd_of_user_account': reports, 'state': 'stopped', 'start_at_boot': False}},
                 {'systemd_unit': {'name': worker_unit, 'drop_in': '10-extra.conf', 'in_systemd_of_user_account': reports, 'state': 'absent'}},
                 {'systemd_unit': {'name': worker_unit, 'in_systemd_of_user_account': reports, 'state': 'absent'}}])
        preview = run('dryrun')['entries'][0]['response']['results']
        assert preview[0]['status'] == 'succeeded' and preview[0]['change'] == 'would_change', preview
        remote(f'test -L {wanted_by}')
        account_systemd_stayed_stopped()
        run('run')
        remote(f'test ! -e {user_units}/{worker_unit}\ntest ! -e {user_units}/{worker_unit}.d/10-extra.conf\n'
               f'test ! -L {wanted_by}\ntest ! -e /var/lib/systemd/linger/{reports}')
        account_systemd_stayed_stopped()
        assert run('run')['entries'][0]['response']['change'] == 'unchanged'
        assert run('dryrun', 0)['entries'][0]['response']['change'] == 'unchanged'
        account_systemd_stayed_stopped()
        # A decommission removes the account last. The removal still reruns
        # unchanged once the account is gone: such an account has no unit.
        remote(f'loginctl disable-linger {reports}\nsystemctl stop "user@$(id -u {reports}).service"\n'
               f'userdel {reports}\ngroupdel {reports} 2>/dev/null || true\nrm -rf {home}')
        assert run('dryrun', 0)['entries'][0]['response']['change'] == 'unchanged'
        assert run('run')['entries'][0]['response']['change'] == 'unchanged'
        # The account and its home come back for the cases below.
        project(reports_tasks('2')[:2])
        run('run')
        project([{'systemd_service': {'name': 'x', 'in_systemd_of_user_account': 'mmx-units-nobody-here', "state": "running"}}])
        assert 'in_systemd_of_user_account' in json.dumps(run('run', 1))
        mark("units in an account's own systemd: file owned by the account, lingering turned on and reported, service as "
             'the account, restart after a changed unit or drop-in, rejected unit kept out by a check that the account runs, removal that disables '
             "while the account's systemd is not running without starting it, unchanged reruns, also after the account is gone")

        # A start that fails says why: the reason of the failed node task
        # holds the lines that the unit wrote to the journal during that
        # start, so that nobody has to log in to the node to read them.
        broken, shop_pod, shop, report_broken = 'mmx-units-broken.service', 'mmx-units-shop-pod.service', 'mmx-units-shop.service', 'report-broken.service'
        journal = 'Last lines of the systemd journal for '

        def unit_that_does_not_start(line, more='', printed_by='echo {} >&2'):
            return f"[Service]\nExecStartPre=/bin/sh -c '{printed_by.format(line)}; exit 3'\nExecStart=/bin/sleep infinity\n{more}"

        def failed_start(units, service, account=None, **settings_of_the_service_task):
            in_account = {'in_systemd_of_user_account': account} if account else {}
            project([{'task_block': {'id': 'does_not_start',
                                     'changes': [{'systemd_unit': {'name': name, 'content': content, **in_account}} for name, content in units.items()],
                                     'activate': [{'systemd_service': {'name': service, **in_account, "state": "running"}, **settings_of_the_service_task}]}}])
            return next(r for r in run('run', 1)['entries'][0]['response']['results'] if r['status'] == 'failed')

        def times_printed(line, reason):
            return (reason + '\n').count(f'\n  {line}\n')

        try:
            # A service task tries once unless it sets retry: a start that
            # failed fails again. With retry: true it tries three times.
            # Each try reports systemd's own message and after it the
            # unit's lines of that try alone: the unit prints its line at
            # every try, and each reason holds it once. This unit starts
            # itself again after a failure, so systemd reports it as
            # activating, not as failed, and the second and third try
            # restart it.
            failed = failed_start({broken: unit_that_does_not_start('mmx-units-broken: no settings file', 'Restart=on-failure\nRestartSec=10min\n')}, broken, retry=True)
            tries = [t['reason'] for t in failed['tries']]
            assert len(tries) == 3 and failed['reason'] == 'node task did not finish after 3 tries, because each try failed or left retry.until false: ' + tries[2], failed
            assert tries[0].startswith(f'exit status 1\n{journal}{broken} during this start:\n'), tries[0]
            assert tries[2].startswith(f'exit status 1\n{journal}{broken} during this restart:\n'), tries[2]
            assert all(times_printed('mmx-units-broken: no settings file', t) == 1 and 'status=3' in t for t in tries), tries
            assert f'Job for {broken} failed' in failed['stderr'], failed
            assert prop('ActiveState', broken) == 'activating'
            # A unit that systemd does not start because a unit it needs
            # failed: the reason names that unit and holds its lines.
            # This task sets no retry, so the unit that it needs ran once.
            failed = failed_start({shop_pod: unit_that_does_not_start('mmx-units-shop-pod: catatonit is not installed',
                                                                      printed_by='echo started >> /run/mmx-units-shop-pod-starts; echo {} >&2'),
                                   shop: f'[Unit]\nRequires={shop_pod}\nAfter={shop_pod}\n\n[Service]\nExecStart=/bin/sleep infinity\n'}, shop)
            reason = failed['reason']
            assert not failed.get('tries') and remote('wc -l < /run/mmx-units-shop-pod-starts').strip() == '1', failed
            assert reason.startswith(f'exit status 1\nsystemd did not start {shop}, because {shop_pod}, which it needs, failed.\n'
                                     f'{journal}{shop_pod} during this start:\n'), reason
            assert times_printed('mmx-units-shop-pod: catatonit is not installed', reason) == 1, reason
            # A unit in an account's own systemd. A program that the unit's
            # command starts prints the line and ends, as the program of a
            # wrapper script does. The journal often records no unit for
            # such a line; the line is then under the heading for the
            # account's lines without a unit, and it is there in either case.
            reason = failed_start({report_broken: unit_that_does_not_start('report-broken: no settings file', printed_by='/bin/sh -c "echo {} >&2"')},
                                  report_broken, reports)['reason']
            assert times_printed('report-broken: no settings file', reason) == 1 and 'status=3' in reason, reason
            headings = [line for line in reason.splitlines()[1:] if not line.startswith('  ')]
            of_unit = f'{journal}{report_broken} of account {reports} during this start:'
            without_unit = (f'Lines that programs of account {reports} printed during this start, '
                            'for which the systemd journal recorded no unit because the program ended at once:')
            assert headings in ([of_unit], [of_unit, without_unit]), reason
        finally:
            remote(stop_and_disable(broken, shop_pod, shop) +
                   f'systemctl reset-failed {broken} {shop_pod} {shop} 2>/dev/null || true\n'
                   f'rm -f /etc/systemd/system/{broken} /etc/systemd/system/{shop_pod} /etc/systemd/system/{shop} {user_units}/{report_broken} /run/mmx-units-shop-pod-starts\n'
                   'systemctl daemon-reload\n'
                   f'runuser -u {reports} -- env XDG_RUNTIME_DIR=/run/user/$(id -u {reports}) /bin/sh -c '
                   f'"systemctl --user reset-failed {report_broken}; systemctl --user daemon-reload" 2>/dev/null || true\n')
        mark('a start that fails is tried once and reports the journal lines of that try alone, also of each of three tries with '
             "retry: of a system unit that systemd reports as activating, of the unit that the unit needs, and of a unit in an account's own systemd")

        # The example's explicit service task owns stopping/disabling before removal.
        (workspace / 'playbooks/account_service.yml').write_text(original)
        settings_file.write_text(original_settings)
        source.write_text((vm.REPO / 'examples/account-and-unit/files/limits.conf.tmpl').read_text())
        run('run')
        remote('printf kept > /srv/mmx-account-demo/file')
        run('dryrun', None, playbook='remove')
        assert prop('ActiveState') == 'active'
        run('run', 0, playbook='remove')
        remote(f'! getent passwd {name}\n! getent group {name}\ntest ! -e {installed}\ntest ! -e {drop}\n'
               f'test -f {installed}.d/90-unmanaged.conf\ntest "$(cat /srv/mmx-account-demo/file)" = kept\n'
               f'test ! -L /etc/systemd/system/multi-user.target.wants/{unit}')
        assert prop('ActiveState') == 'inactive'
        assert run('run', 0, playbook='remove')['entries'][0]['response']['change'] == 'unchanged'
        mark('shipped removal playbook stops/disables explicitly, removes named resources, preserves data, and reruns unchanged')
    finally:
        cleanup()
    (workspace / 'summary.json').write_text(json.dumps(passed, indent=2) + '\n')
    print(f'{len(passed)} account/unit workflows passed', flush=True)


if __name__ == '__main__':
    vm.run_suite(main)
