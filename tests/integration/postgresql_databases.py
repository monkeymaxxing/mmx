#!/usr/bin/env python3
"""postgresql_role and postgresql_database through the public CLI, against the distribution's own PostgreSQL server, checked with psql itself."""
import json
import shlex
import shutil
import sys
import time

import vm

BASE = '/var/tmp/mmx-postgresql-databases'
# The wrapper that stands before psql while the suite watches how MMX calls
# it. /usr/local/bin comes first on the PATH of a node job worker process.
WRAPPER = '/usr/local/bin/psql'
# Passwords that are searched for at the end, in everything MMX printed and
# saved and everywhere on the node. Each holds the characters that a
# password file and an SQL statement must escape.
SHOP_PASSWORD = 'MmxShopDatabasePasswordSentinel 4711'
ROLE_PASSWORD = "MmxRolePasswordSentinel:4712\\with'a\"quote"
LOGIN_PASSWORD = "MmxLoginPasswordSentinel:4713\\with'a\"quote"
# A role's name with everything that SQL and psql treat specially.
ROLE = 'mmx "shop" O\'Brien \\ role; drop role postgres --'
DATABASE = 'mmx "shop"=db O\'Brien \\ ; --'
# Passwords at the edges of how PostgreSQL prepares a password before it
# hashes it (SASLprep): each with what the server and its clients hash.
PASSWORDS = [
    ('plain ASCII', 'correct horse 1'),
    ('quotes and a backslash', 'pa\'ss\\wo"rd'),
    ('colons and a backslash', 'pa:ss\\wo:rd'),
    ('letters that SASLprep leaves as they are', 'p\u00e4ssw\u00f6rd-\u00f1'),
    ('characters that SASLprep replaces and removes', '\u00aab\u2168\u00adc'),
    ('a letter and a combining mark', 'pa\u0308ss'),
    ('a control character, which SASLprep refuses', '\u00aa\u0007x'),
    ('a character newer than Unicode 3.2, which SASLprep refuses', '\u00aa\U0001f600'),
    ('a zero width space, which becomes a space', 'a\u200bb'),
    ('a newer character that normalization would turn into ASCII', 'a\U0001f100'),
    ('a character that is prohibited only before normalization', 'a\u0340'),
    ('ASCII with a tab', 'tab\there'),
    ('right-to-left letters around a digit', '\u05d01\u05d0'),
    ('right-to-left letters around a left-to-right letter', '\u05d0a\u05d0'),
    ('only a soft hyphen', '\u00ad'),
    ('three hundred letters', 'L' * 300),
]
# The first passwords stand in the playbook as they are; the others reach
# MMX as secrets from the environment, because the test harness writes a
# playbook as JSON, whose escapes cannot hold every character.
PASSWORDS_WRITTEN_IN_THE_PLAYBOOK = 4


def main():
    vm.up()
    node, ssh = vm.connection()
    vm.reset()
    workspace = vm.ROOT / 'postgresql-databases'
    if workspace.exists():
        shutil.rmtree(workspace)
    vm.write_inventory(workspace, {'nodes': {'database': node}}, name='example')
    vm.build()
    mmx = vm.ROOT / 'bin/mmx'
    passed = []
    marked = time.monotonic()
    # Everything mmx printed, to search for the passwords at the end.
    printed = []
    secrets = {'secrets_from_env': {'role_password': 'MMX_TEST_POSTGRESQL_ROLE_PASSWORD', 'login_password': 'MMX_TEST_POSTGRESQL_LOGIN_PASSWORD',
                                            'login_role': 'MMX_TEST_POSTGRESQL_LOGIN_ROLE',
                                            **{f'password_{n}': f'MMX_TEST_POSTGRESQL_PASSWORD_{n}' for n in range(PASSWORDS_WRITTEN_IN_THE_PLAYBOOK, len(PASSWORDS))}}}
    environment = {'SHOP_DATABASE_PASSWORD': SHOP_PASSWORD, 'MMX_TEST_POSTGRESQL_ROLE_PASSWORD': ROLE_PASSWORD, 'MMX_TEST_POSTGRESQL_LOGIN_PASSWORD': LOGIN_PASSWORD,
                   'MMX_TEST_POSTGRESQL_LOGIN_ROLE': 'mmx_socket_login',
                   **{f'MMX_TEST_POSTGRESQL_PASSWORD_{n}': PASSWORDS[n][1] for n in range(PASSWORDS_WRITTEN_IN_THE_PLAYBOOK, len(PASSWORDS))}}
    vm.ENV.update(environment)

    def remote(script, input=None):
        return vm.cmd([*ssh, 'sudo -n /bin/sh -c ' + shlex.quote('set -eu\n' + script)], input=input).stdout.strip()

    def project(tasks):
        vm.write_project(workspace, project_name='postgresql_databases', node_task_defaults={'retry': {'pause_between_tries': '0s'}},
                         fleet_stages=[{'id': 'configure', 'nodes': 'all', 'node_tasks': tasks}], **secrets)

    def play(directory, command, expected=None, timeout=300):
        result = vm.cmd([mmx, command, *vm.playbook_args(command), '--json'], cwd=directory, check=False, timeout=timeout)
        printed.append(result.stdout + result.stderr)
        if result.returncode not in (vm.success_codes(command) if expected is None else (expected,)):
            raise AssertionError((command, result.returncode, result.stdout, result.stderr))
        record = json.loads(result.stdout)
        for r in record['entries'][0]['response']['results']:
            if r['status'] == 'succeeded' and len(r.get('tries', [])) > 1:
                # A try that the server refused costs the suite time.
                print('NOTE a task succeeded after more than one try:', r['task'], [t.get('reason') for t in r['tries']], flush=True)
        return record

    def run(command, expected=None, timeout=300):
        return play(workspace, command, expected, timeout)

    def results(record):
        return {r['task'].rsplit('/', 1)[-1]: r for r in record['entries'][0]['response']['results']}

    def outcome(result):
        """What a task of the two modules says: its status, its change, whether
        it created its object, and the attributes that differed."""
        value = result.get('value') or {}
        return result['status'], result['change'], value.get('created'), value.get('changed_attributes')

    def outcomes(record):
        return {name: outcome(r) for name, r in results(record).items()}

    def created(command='run'):
        return ('succeeded', 'changed' if command == 'run' else 'would_change', True, [])

    def changed(*attributes, command='run'):
        return ('succeeded', 'changed' if command == 'run' else 'would_change', False, list(attributes))

    UNCHANGED = ('succeeded', 'unchanged', False, [])
    NOT_CHECKED = ('not_checked', 'unknown', None, None)

    def all_unchanged(record):
        states = outcomes(record)
        assert record['entries'][0]['response']['change'] == 'unchanged' and all(state == UNCHANGED for state in states.values()), states
        return True

    def mark(message):
        # The seconds since the last mark say where the suite's time goes.
        nonlocal marked
        now = time.monotonic()
        passed.append(message)
        print(f'PASS {message} ({now - marked:.0f} s)', flush=True)
        marked = now

    def as_set_line(name, text):
        """A psql \\set line for the suite's own SQL."""
        return '\\set ' + name + " '" + text.replace('\\', '\\\\').replace("'", "''").replace('\n', '\\n').replace('\t', '\\t') + "'\n"

    def sql(script, database='postgres', variables=None, check=True):
        """The suite's own look at the server: SQL as the server's
        administrator, with the real psql, never through MMX."""
        text = ''.join(as_set_line(name, value) for name, value in (variables or {}).items()) + script
        command = 'cd / && runuser -u postgres -- env PGCLIENTENCODING=UTF8 PGDATABASE=' + shlex.quote(database) + ' ' + psql + ' --no-psqlrc --quiet --no-align --tuples-only --set ON_ERROR_STOP=1'
        result = vm.cmd([*ssh, 'sudo -n /bin/sh -c ' + shlex.quote(command)], input=text, check=False)
        if check and result.returncode:
            raise AssertionError((script, result.stdout, result.stderr))
        return result.stdout.strip()

    def role_in_catalog(name):
        """A role as the server's catalog has it: its attributes, the stored
        form of its password, and every record of a membership."""
        return json.loads(sql("""select jsonb_build_object(
  'attributes', (select jsonb_build_object('can_log_in', rolcanlogin, 'superuser', rolsuper, 'can_create_databases', rolcreatedb, 'can_create_roles', rolcreaterole, 'can_start_replication', rolreplication)
                 from pg_roles where rolname = :'name'),
  'stored_password', (select rolpassword from pg_authid where rolname = :'name'),
  'memberships', (select coalesce(jsonb_agg(jsonb_build_object('role', g.rolname, 'granted_by', b.rolname, 'gives_privileges',
                     coalesce((to_jsonb(m) ->> 'inherit_option')::boolean or (to_jsonb(m) ->> 'set_option')::boolean, true)) order by g.rolname, b.rolname), '[]'::jsonb)
                  from pg_auth_members m join pg_roles r on r.oid = m.member join pg_roles g on g.oid = m.roleid left join pg_roles b on b.oid = m.grantor
                  where r.rolname = :'name'));
""", variables={'name': name}))

    def member_of_roles(name):
        return sorted({m['role'] for m in role_in_catalog(name)['memberships'] if m['gives_privileges']})

    def database_in_catalog(name):
        """A database as the server's catalog has it, None when it does not exist."""
        found = json.loads(sql("""select jsonb_build_object('database', (select jsonb_build_object('owner', pg_get_userbyid(datdba), 'encoding', pg_encoding_to_char(encoding),
  'collation', datcollate, 'character_type', datctype) from pg_database where datname = :'name'));
""", variables={'name': name}))['database']
        if found:
            found['extensions'] = json.loads(sql("select coalesce(jsonb_agg(extname order by extname), '[]'::jsonb) from pg_extension;\n", database=name))
        return found

    def in_password_file(text):
        return text.replace('\\', '\\\\').replace(':', '\\:')

    def logins(pairs):
        """Which of these roles can log in over TCP with these passwords, as
        an application would: psql with a password file. Each attempt has a
        password file with its one line: psql takes the first line that
        names the role."""
        lines = ''.join(f'127.0.0.1:*:*:{in_password_file(role)}:{in_password_file(password)}\n' for role, password in pairs)
        attempts = '\n'.join(f'sed -n {n + 1}p {BASE}/logins-of-all > {BASE}/logins\n'
                             f'PGPASSFILE={BASE}/logins PGCONNECT_TIMEOUT=5 {psql} --no-psqlrc --no-password --quiet --no-align --tuples-only --host=127.0.0.1 '
                             f'--username={shlex.quote(role)} --command="select \'logged in\'" postgres 2>>{BASE}/login-errors || echo refused' for n, (role, _) in enumerate(pairs))
        answers = remote(f'umask 077\ncat > {BASE}/logins-of-all\n: > {BASE}/login-errors\ncd /\n{attempts}\nrm -f {BASE}/logins {BASE}/logins-of-all', input=lines).split('\n')
        assert len(answers) == len(pairs), answers
        return [answer == 'logged in' for answer in answers]

    def assert_logins(pairs, expected):
        """Each of these roles logs in with its password, or is refused, as
        expected says; a failure shows what psql said."""
        accepted = logins(pairs)
        assert accepted == expected, (list(zip([role for role, _ in pairs], accepted)), remote(f'cat {BASE}/login-errors'))

    def packages():
        return remote("""if command -v dpkg-query >/dev/null; then dpkg-query --show --showformat '${db:Status-Abbrev} ${Package}\\n' | awk '$1 == "ii" {print $2}'
else rpm --query --all --queryformat '%{NAME}\\n'; fi | sort -u""").split('\n')

    def accounts_and_groups():
        return remote('cut -d: -f1 /etc/passwd | sed "s/^/account /"; cut -d: -f1 /etc/group | sed "s/^/group /"').split('\n')

    def watch_psql(slow=False, fail_for_database=None):
        """Put a wrapper before psql that notes each call: the account that
        runs it, the role in its environment, its command line, its settings
        for the server's certificate, the names of all variables of its
        environment, and the owner and mode of its password file and of the
        directory around it, or the path of a password file that does not
        exist. With slow, it waits before it starts psql, so that a sampler
        of process command lines sees the call. With fail_for_database, it
        fails every call for that database."""
        remote(f"""rm -f {BASE}/psql-calls {BASE}/psql-fails-for-database {BASE}/psql-is-slow
install -m 666 /dev/null {BASE}/psql-calls
{'touch ' + BASE + '/psql-is-slow' if slow else ''}
cat > {WRAPPER} <<'WRAPPER'
#!/bin/sh
{{
  printf 'account=%s role=%s certificate_check=%s authority_file=%s environment=%s' "$(id -un)" "${{PGUSER:-none}}" "${{PGSSLMODE:-none}}" "${{PGSSLROOTCERT:-none}}" "$(env | cut -d= -f1 | sort | tr '\n' ',')"
  if [ -f "${{PGPASSFILE:-}}" ]; then printf ' password_file=%s directory=%s' "$(stat -c '%U:%a' "$PGPASSFILE")" "$(stat -c '%U:%a' "${{PGPASSFILE%/*}}")"; else printf ' password_file=%s' "${{PGPASSFILE:-none}}"; fi
  printf ' arguments=%s\\n' "$*"
}} >> {BASE}/psql-calls
if [ -e {BASE}/psql-is-slow ]; then sleep 1; fi
if [ -e {BASE}/psql-fails-for-database ] && [ "$PGDATABASE" = "$(cat {BASE}/psql-fails-for-database)" ]; then echo 'psql: error: the test wrapper ends this call' >&2; exit 1; fi
exec {psql} "$@"
WRAPPER
chmod 755 {WRAPPER}""")
        if fail_for_database:
            remote(f'cat > {BASE}/psql-fails-for-database\nchmod 644 {BASE}/psql-fails-for-database', input=fail_for_database)

    def psql_calls():
        """The calls that the wrapper noted, and forget the wrapper."""
        calls = remote(f'cat {BASE}/psql-calls\nrm -f {WRAPPER} {BASE}/psql-calls {BASE}/psql-fails-for-database {BASE}/psql-is-slow').split('\n')
        return [dict(part.split('=', 1) for part in call.split(' arguments=')[0].split(' ')) | {'arguments': call.split(' arguments=')[1]} for call in calls if call]

    def only_the_environment_that_mmx_sets(call, *more):
        """psql got PATH and the variables that MMX sets for it, and nothing
        of the node job worker process's own environment, which has HOME,
        USER, and more. A shell adds PWD and, depending on the shell, SHLVL,
        OLDPWD, and _ by itself."""
        names = set(call['environment'].strip(',').split(',')) - {'PWD', 'OLDPWD', 'SHLVL', '_'}
        return names == {'PATH', 'PGCLIENTENCODING', 'PGCONNECT_TIMEOUT', 'PGDATABASE', 'PGPASSFILE', 'PGUSER', *more}

    def no_private_task_directory_left():
        remote('test -d /run/mmx/private_task_directories\ntest -z "$(ls -A /run/mmx/private_task_directories)"')

    def nowhere(what, texts):
        """None of these texts is in anything that mmx printed or saved, or
        anywhere on the node where a node task could have left it: MMX's own
        records, the journal, the home directories, and the directories of
        the PostgreSQL server's configuration. The server's own log is not
        among them: the design says where a stored form shows there."""
        saved = ''.join(path.read_text(errors='replace') for path in (workspace / '.mmx').rglob('*') if path.is_file())
        for text in texts:
            assert text not in ''.join(printed) and text not in saved, what + ': a password or its stored form is in what mmx printed or saved'
        found = remote(f'umask 077\ncat > {BASE}/texts\n'
                       f'grep -rlF -f {BASE}/texts /var/lib/mmx /run/mmx /root /home /etc/postgresql /etc/postgresql-common "$(getent passwd postgres | cut -d: -f6)"/.[a-z]* 2>/dev/null || true\n'
                       f'journalctl --no-pager -q 2>/dev/null | grep -F -f {BASE}/texts || true\n'
                       f'rm -f {BASE}/texts', input='\n'.join(texts) + '\n')
        assert found == '', what + ': ' + found
        no_private_task_directory_left()

    def stop_server():
        """End the server at once, as a power failure would. The suite never
        starts this server again, and its data goes with the cleanup. An
        ordinary stop writes every changed page to disk first, which took
        more than twelve seconds on a Debian 13 guest after the suite's
        databases were made."""
        stopped = remote("""postmaster=$(head -n 1 "$(getent passwd postgres | cut -d: -f6)"/*/main/postmaster.pid "$(getent passwd postgres | cut -d: -f6)"/data/postmaster.pid 2>/dev/null | grep -E '^[0-9]+$' | head -n 1 || true)
if [ -n "$postmaster" ]; then kill -QUIT "$postmaster" 2>/dev/null || true; fi
if ! command -v pg_isready >/dev/null; then exit 0; fi
for try in $(seq 1 100); do code=0; pg_isready >/dev/null 2>&1 || code=$?; if [ $code = 2 ]; then exit 0; fi; sleep 0.1; done
echo "pg_isready still exits with $code" """)
        assert stopped == '', 'the server did not stop: ' + stopped

    def cleanup():
        """Everything the suite installed and made goes: the server with its
        data directory, the packages that came with it, and the account and
        the groups that the packages made, so that the next suite finds the
        guest as this one found it."""
        if 'account postgres' in accounts_and_groups():
            stop_server()
        added = sorted(set(packages()) - set(packages_before))
        new_names = sorted(set(accounts_and_groups()) - set(accounts_and_groups_before))
        remove_names = '\n'.join(('userdel ' if name.startswith('account ') else 'groupdel ') + shlex.quote(name.split(' ', 1)[1]) + ' 2>/dev/null || true'
                                 for name in sorted(new_names))
        remote(f"""
for unit in $(systemctl list-units --type=service --all --no-legend --plain 'postgresql*' | cut -d' ' -f1); do systemctl stop "$unit" 2>/dev/null || true; done
systemctl disable postgresql 2>/dev/null || true
if [ -n "{' '.join(added)}" ]; then
  if command -v apt-get >/dev/null; then DEBIAN_FRONTEND=noninteractive apt-get purge -y -q {' '.join(added)} >/dev/null 2>&1 || true
  else dnf remove -y -q {' '.join(added)} >/dev/null 2>&1 || true
  fi
fi
rm -rf /var/lib/postgresql /etc/postgresql /etc/postgresql-common /var/log/postgresql /var/cache/postgresql /var/lib/pgsql /run/postgresql {BASE} {WRAPPER}
rm -f /etc/systemd/system/multi-user.target.wants/postgresql.service
{remove_names}
systemctl daemon-reload
systemctl reset-failed 2>/dev/null || true
""")

    def guest_is_as_found():
        """Nothing of PostgreSQL or of this suite is left: no package, no
        account or group, no data or configuration directory, no unit, and
        systemd is running. Everything found is in the one failure."""
        left = [f'package {name}' for name in sorted(set(packages()) - set(packages_before))]
        left += sorted(set(accounts_and_groups()) - set(accounts_and_groups_before))
        left += [line for line in remote(f"""command -v psql pg_isready postgres initdb || true
ls -d /var/lib/postgresql /etc/postgresql /etc/postgresql-common /var/log/postgresql /var/lib/pgsql /run/postgresql {BASE} {WRAPPER} /run/mmx/private_task_directories/* 2>/dev/null || true
systemctl list-unit-files 'postgresql*' --no-legend 2>/dev/null || true""").split('\n') if line]
        gone = sorted(set(packages_before) - set(packages()))
        left += [f'package {name} was removed' for name in gone]
        state = vm.guest('sudo', '-n', 'systemctl', 'is-system-running', name=vm.NAME, check=False).stdout.strip()
        if state != 'running':
            left.append(f'systemd is {state}:\n' + vm.guest('sudo', '-n', '/bin/sh', '-c', vm.FAILED_UNITS, name=vm.NAME, check=False).stdout)
        assert not left, 'the suite left this on the guest:\n' + '\n'.join(left)
        print('The guest is as the suite found it: nothing of PostgreSQL is left, and systemd is running', flush=True)

    distribution = remote('. /etc/os-release; echo "$ID"')
    package_manager = remote('for manager in apt-get dnf; do if command -v $manager >/dev/null; then echo $manager; break; fi; done').replace('apt-get', 'apt')
    packages_before, accounts_and_groups_before = packages(), accounts_and_groups()
    assert remote('command -v psql pg_isready || true') == '' and 'account postgres' not in accounts_and_groups_before, 'the guest already has PostgreSQL'
    remote(f'rm -rf {BASE}\ninstall -d -m 1777 {BASE}')
    try:
        # A node without psql: the programs are something an earlier node
        # task may still install.
        project([{'id': 'role', 'postgresql_role': {'can_log_in': False, 'name': 'shop'}}, {'id': 'database', 'postgresql_database': {'owner': 'postgres', 'name': 'shop'}}])
        assert outcomes(run('dryrun')) == {'role': NOT_CHECKED, 'database': NOT_CHECKED}
        missing = results(run('run', 1))['role']
        client_package = {'apt': 'postgresql-client (apt', 'dnf': 'postgresql (dnf'}[package_manager]
        assert missing['status'] == 'failed' and missing['failure_kind'] == 'missing_prerequisite' and len(missing.get('tries', [])) < 2, missing
        assert missing['reason'].startswith('postgresql_role: psql is not installed;') and client_package in missing['reason'], missing
        mark('a node without the psql program: a dryrun reports both tasks not checked, and a run fails once and names the program and its package')

        # The example project installs the server with the distribution's
        # own packages and creates the application's role and database.
        example = vm.ROOT / 'postgresql-example'
        if example.exists():
            shutil.rmtree(example)
        shutil.copytree(vm.REPO / 'examples/postgresql-server-for-one-application', example)
        vm.write_inventory(example, {'nodes': {'database01': node}}, name='example')

        def example_tasks(record):
            """The example's tasks have no IDs; the two database tasks are its last two."""
            listed = record['entries'][0]['response']['results']
            return {'service': next(r for r in listed if r['task'].rsplit('/', 1)[-1] == 'task_7'), 'role': listed[-2], 'database': listed[-1]}

        preview = example_tasks(play(example, 'dryrun'))
        assert outcome(preview['role']) == NOT_CHECKED and outcome(preview['database']) == NOT_CHECKED, preview
        installed = example_tasks(play(example, 'run', timeout=900))
        assert outcome(installed['role']) == created() and outcome(installed['database']) == created(), installed
        # The role's password is a secret that MMX hands only to psql, as
        # its stored form: the result is not hidden.
        assert installed['role'].get('secret') is not True, installed['role']
        psql = remote('command -v psql')
        version = int(sql('show server_version_num;\n')) // 10000
        print(f'PostgreSQL {sql("show server_version;")} on {distribution}; the package task left the server '
              f'{"stopped, and the systemd_service task started it" if installed["service"]["change"] == "changed" else "running"}', flush=True)
        shop = role_in_catalog('shop')
        assert shop['attributes'] == {'can_log_in': True, 'superuser': False, 'can_create_databases': False, 'can_create_roles': False, 'can_start_replication': False}, shop
        assert shop['stored_password'].startswith('SCRAM-SHA-256$4096:') and len(shop['stored_password']) == 133, 'the stored form is not the SCRAM-SHA-256 form'
        in_catalog = database_in_catalog('shop')
        assert in_catalog['owner'] == 'shop' and in_catalog['encoding'] == 'UTF8' and in_catalog['extensions'] == ['plpgsql'], in_catalog
        for command in ('run', 'dryrun'):
            again = play(example, command)
            assert again['entries'][0]['response']['change'] == 'unchanged', [(r['task'], r['status'], r['change'], r.get('reason')) for r in again['entries'][0]['response']['results']]
        mark('the example postgresql-server-for-one-application: a dryrun before the server is installed, the run that installs it and creates the role and the database, '
             'and an unchanged run and dryrun')

        # What the suite itself sets up in the server, not through MMX: a
        # password login over TCP and through the local socket for the roles
        # it tests with, and the server's certificate, signed by a
        # certificate authority of the suite. A second authority signed
        # nothing.
        settings = json.loads(sql("select jsonb_build_object('hba_file', current_setting('hba_file'), 'data_directory', current_setting('data_directory'), 'template', "
                                  "(select datcollate from pg_database where datname = 'template1'));\n"))
        if remote('command -v openssl || true') == '':
            # The suite makes its certificates with openssl; the cleanup
            # removes every package that the guest did not have before.
            remote({'apt': 'DEBIAN_FRONTEND=noninteractive apt-get install -y -q openssl', 'dnf': 'dnf install -y -q openssl'}[package_manager] + ' >/dev/null')
        tls = BASE + '/tls'
        remote(f"""command -v openssl >/dev/null
mkdir {tls}
cd {tls}
new_key='-newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes'
openssl req -x509 $new_key -keyout authority.key -out authority.crt -subj '/CN=MMX test certificate authority' -days 2 -addext basicConstraints=critical,CA:TRUE 2>/dev/null
openssl req $new_key -keyout server.key -out server.request -subj '/CN=localhost' 2>/dev/null
printf 'subjectAltName=DNS:localhost\\n' > server.extensions
openssl x509 -req -in server.request -CA authority.crt -CAkey authority.key -CAcreateserial -out server.crt -days 2 -extfile server.extensions 2>/dev/null
openssl req -x509 $new_key -keyout other.key -out other-authority.crt -subj '/CN=Another certificate authority' -days 2 -addext basicConstraints=critical,CA:TRUE 2>/dev/null
install -o postgres -g postgres -m 600 server.key {settings['data_directory']}/mmx-test-server.key
install -o postgres -g postgres -m 644 server.crt {settings['data_directory']}/mmx-test-server.crt
chmod 644 authority.crt other-authority.crt
cp -p {settings['hba_file']} {BASE}/pg_hba.conf.before
printf '%s\\n' 'local all mmx_socket_login scram-sha-256' 'host all all 127.0.0.1/32 scram-sha-256' 'host all all ::1/128 scram-sha-256' | cat - {BASE}/pg_hba.conf.before > {settings['hba_file']}""")
        sql(f"""alter system set ssl_cert_file = '{settings['data_directory']}/mmx-test-server.crt';
alter system set ssl_key_file = '{settings['data_directory']}/mmx-test-server.key';
alter system set ssl = on;
select pg_reload_conf();
""")
        assert_logins([('shop', SHOP_PASSWORD), ('shop', 'another password')], [True, False])
        mark('the application of the example logs in over TCP with the password of the secret, and not with another one')

        # Roles: a group, a role with every kind of attribute, and one role
        # for each password at the edges of SASLprep. In the dryrun the
        # role that is to be a member of the group is not checked: the
        # group does not exist until the task before it ran.
        def password_roles(**more):
            return [{'id': f'password_{n}', 'postgresql_role': {'name': f'mmx_password_{n}', 'can_log_in': True, **more,
                                                                 'password': password if n < PASSWORDS_WRITTEN_IN_THE_PLAYBOOK else f'{{{{ .secrets.password_{n} }}}}'}}
                    for n, (_, password) in enumerate(PASSWORDS)]

        def roles(**changes):
            role = {'name': ROLE, 'password': '{{ .secrets.role_password }}', 'can_log_in': True, 'can_create_databases': True, 'superuser': False,
                    'member_of_roles': ['pg_monitor', 'mmx_group'], 'member_of_roles_not_listed': 'keep', **changes}
            return [{'id': 'group', 'postgresql_role': {'can_log_in': False, 'name': 'mmx_group'}}, {'id': 'role', 'postgresql_role': {name: value for name, value in role.items() if value is not None}}]

        project(roles() + password_roles())
        preview = outcomes(run('dryrun'))
        assert preview.pop('role') == NOT_CHECKED and all(state == created('dryrun') for state in preview.values()), preview
        first = run('run')
        assert all(state == created() for state in outcomes(first).values()), outcomes(first)
        assert all(r.get('secret') is not True for r in results(first).values()), 'a password hid a result'
        in_catalog = role_in_catalog(ROLE)
        assert in_catalog['attributes'] == {'can_log_in': True, 'superuser': False, 'can_create_databases': True, 'can_create_roles': False, 'can_start_replication': False}, in_catalog
        assert member_of_roles(ROLE) == ['mmx_group', 'pg_monitor'] and in_catalog['stored_password'].startswith('SCRAM-SHA-256$4096:'), in_catalog
        assert role_in_catalog('mmx_group') == {'attributes': {'can_log_in': False, 'superuser': False, 'can_create_databases': False, 'can_create_roles': False, 'can_start_replication': False},
                                                'stored_password': None, 'memberships': []}
        for command in ('run', 'dryrun'):
            assert all_unchanged(run(command))
        mark(f'roles: a dryrun that changes nothing, the first run, an unchanged run and dryrun, for a role with a name full of quotes and {len(PASSWORDS)} roles with a password')

        # Every password works for a login, as the client prepares it; and
        # no other password does.
        pairs = [(ROLE, ROLE_PASSWORD)] + [(f'mmx_password_{n}', password) for n, (_, password) in enumerate(PASSWORDS)]
        assert_logins(pairs + [(role, password + 'x') for role, password in pairs], [True] * len(pairs) + [False] * len(pairs))
        # The other direction: the server itself hashes each password, with
        # a salt of its own, and MMX finds the stored form unchanged.
        sql("set password_encryption = 'scram-sha-256';\n" + ''.join(
            f"select format('alter role %I password %L', 'mmx_password_{n}', convert_from(decode('{password.encode().hex()}', 'hex'), 'UTF8')) \\gexec\n" for n, (_, password) in enumerate(PASSWORDS)))
        assert all_unchanged(run('run'))
        mark('every password at the edges of SASLprep: a role whose password MMX set logs in with it, and a password that the server itself hashed is unchanged for MMX')

        # A password in the older md5 form is replaced by the SCRAM form,
        # also when it is the same password, and the result says so.
        sql("set password_encryption = 'md5';\nalter role mmx_password_0 password 'correct horse 1';\n")
        assert role_in_catalog('mmx_password_0')['stored_password'].startswith('md5')
        project(password_roles()[:1])
        preview = results(run('dryrun'))['password_0']
        assert outcome(preview) == changed('password', command='dryrun') and 'md5 form' in preview['reason'], preview
        replaced = results(run('run'))['password_0']
        assert outcome(replaced) == changed('password') and replaced['reason'] == 'the password of the role mmx_password_0 was stored in the older md5 form and is now stored in the SCRAM-SHA-256 form', replaced
        assert role_in_catalog('mmx_password_0')['stored_password'].startswith('SCRAM-SHA-256$')
        assert_logins([('mmx_password_0', 'correct horse 1')], [True])
        assert all_unchanged(run('run'))
        if version >= 16:
            # The form has the server's own count of iterations.
            sql('alter system set scram_iterations = 5000;\nselect pg_reload_conf();\n')
            project([{'id': 'password_0', 'postgresql_role': {'can_log_in': True, 'name': 'mmx_password_0', 'password': 'a new password'}}])
            assert outcome(results(run('run'))['password_0']) == changed('password')
            assert role_in_catalog('mmx_password_0')['stored_password'].startswith('SCRAM-SHA-256$5000:')
            assert_logins([('mmx_password_0', 'a new password')], [True])
            sql('alter system reset scram_iterations;\nselect pg_reload_conf();\n')
        mark('a password that is stored in the md5 form is replaced by the SCRAM-SHA-256 form'
             + (', with the server\'s own count of iterations' if version >= 16 else ''))

        # Attributes: what the task names is changed, what it omits stays.
        project(roles(can_log_in=False, can_create_databases=None, can_create_roles=True, can_start_replication=True, password=None))
        preview = outcomes(run('dryrun'))['role']
        assert preview == changed('can_log_in', 'can_create_roles', 'can_start_replication', command='dryrun'), preview
        assert outcomes(run('run'))['role'] == changed('can_log_in', 'can_create_roles', 'can_start_replication')
        in_catalog = role_in_catalog(ROLE)
        assert in_catalog['attributes'] == {'can_log_in': False, 'superuser': False, 'can_create_databases': True, 'can_create_roles': True, 'can_start_replication': True}, in_catalog
        assert in_catalog['stored_password'].startswith('SCRAM-SHA-256$'), 'a task without password removed the password'
        assert all_unchanged(run('run'))
        project(roles(password='{{ .secrets.login_password }}', can_create_roles=False, can_start_replication=False))
        assert outcomes(run('run'))['role'] == changed('can_log_in', 'can_create_roles', 'can_start_replication', 'password')
        assert_logins([(ROLE, LOGIN_PASSWORD), (ROLE, ROLE_PASSWORD)], [True, False])
        project(roles())
        assert outcomes(run('run'))['role'] == changed('password')
        assert_logins([(ROLE, ROLE_PASSWORD)], [True])
        mark('attributes and the password of an existing role: the task changes what differs, names it, and keeps what it omits')

        # Memberships. A second role grants the same membership: from
        # PostgreSQL 16 that is a second record. A role with CREATEROLE that
        # is no superuser creates a role: from PostgreSQL 16 it gets a record
        # that only lets it administer that role.
        sql("""create role mmx_team_lead createrole;
grant mmx_group to mmx_team_lead with admin option;
create role mmx_reporting;
grant mmx_reporting to :"role";
set session authorization mmx_team_lead;
grant mmx_group to :"role";
create role mmx_made_by_team_lead;
""", variables={'role': ROLE})
        records = [(m['role'], m['granted_by']) for m in role_in_catalog(ROLE)['memberships']]
        assert records == ([('mmx_group', 'mmx_team_lead'), ('mmx_group', 'postgres'), ('mmx_reporting', 'postgres'), ('pg_monitor', 'postgres')] if version >= 16 else
                           [('mmx_group', 'postgres'), ('mmx_reporting', 'postgres'), ('pg_monitor', 'postgres')]), records
        project(roles())
        assert all_unchanged(run('run')), 'member_of_roles_not_listed: keep removed or added a membership'
        project(roles(member_of_roles=['pg_monitor'], member_of_roles_not_listed='remove'))
        assert outcomes(run('dryrun'))['role'] == changed('member_of_roles', command='dryrun') and member_of_roles(ROLE) == ['mmx_group', 'mmx_reporting', 'pg_monitor']
        assert outcomes(run('run'))['role'] == changed('member_of_roles')
        assert [(m['role'], m['granted_by']) for m in role_in_catalog(ROLE)['memberships']] == [('pg_monitor', 'postgres')]
        assert all_unchanged(run('run'))
        team_lead = [{'id': 'team_lead', 'postgresql_role': {'can_log_in': False, 'name': 'mmx_team_lead', 'member_of_roles': ['pg_monitor'], 'member_of_roles_not_listed': 'remove'}}]
        project(team_lead)
        assert outcomes(run('run'))['team_lead'] == changed('member_of_roles')
        # Removal includes the record that only granted role administration.
        records = [(m['role'], m['gives_privileges']) for m in role_in_catalog('mmx_team_lead')['memberships']]
        assert records == [('pg_monitor', True)], records
        assert all_unchanged(run('run'))
        team_lead[0]['postgresql_role'].update(member_of_roles=['mmx_made_by_team_lead'], member_of_roles_not_listed='keep')
        project(team_lead)
        granted = results(run('run'))['team_lead']
        assert outcome(granted) == changed('member_of_roles') and member_of_roles('mmx_team_lead') == ['mmx_made_by_team_lead', 'pg_monitor'], granted
        mark('memberships: keep adds; remove takes every unnamed record away, including administration-only records')

        # A role in member_of_roles that does not exist, a name that is longer
        # than the server takes, and a name that the server refuses.
        project(roles(member_of_roles=['pg_monitor', 'mmx_no_such_role', 'mmx_neither'], member_of_roles_not_listed='keep')[1:])
        assert outcomes(run('dryrun'))['role'] == NOT_CHECKED
        missing = results(run('run', 1))['role']
        assert missing['failure_kind'] == 'missing_prerequisite' and missing['change'] == 'unchanged' and len(missing.get('tries', [])) < 2 and missing['reason'].startswith(
            "postgresql_role.member_of_roles: the roles mmx_no_such_role, mmx_neither do not exist in the PostgreSQL server on this node's local socket"), missing
        project([{'id': 'predefined', 'postgresql_role': {'can_log_in': False, 'name': 'mmx_reader', 'member_of_roles': ['pg_read_all_data'], 'member_of_roles_not_listed': 'keep'}}])
        predefined = results(run('run', 1 if version < 14 else None))['predefined']
        # pg_read_all_data exists from PostgreSQL 14.
        assert (predefined['failure_kind'] == 'missing_prerequisite' and role_in_catalog('mmx_reader')['attributes'] is None) if version < 14 else (
            outcome(predefined) == created() and member_of_roles('mmx_reader') == ['pg_read_all_data']), predefined
        long_name = 'mmx_' + 'n' * 60
        project([{'id': 'long', 'postgresql_role': {'name': long_name, 'can_log_in': True}}, {'id': 'never', 'postgresql_role': {'can_log_in': False, 'name': 'mmx_never'}}])
        for command in ('dryrun', 'run'):
            refused = results(run(command, 1))['long']
            assert refused['status'] == 'failed' and refused['change'] == 'unchanged' and refused['failure_kind'] == 'config' and (
                'has 64 bytes, and this PostgreSQL server takes names of at most 63 bytes' in refused['reason']), refused
        assert role_in_catalog(long_name[:63])['attributes'] is None and role_in_catalog('mmx_never')['attributes'] is None
        # The server reserves names that start with pg_. The password of
        # this task is a secret, and the result still gives the server's
        # message, with the code of the error.
        project([{'id': 'reserved', 'retry': False, 'postgresql_role': {'can_log_in': False, 'name': 'pg_mmx_reserved', 'password': '{{ .secrets.role_password }}'}}])
        refused = results(run('run', 1))['reserved']
        # The server refused the statement, so nothing of it stays.
        assert refused['status'] == 'failed' and refused['change'] == 'unchanged' and refused.get('secret') is not True and refused['reason'].startswith(
            'postgresql_role: could not create the role pg_mmx_reserved: ') and ' 42939: ' in refused['reason'] and 'LOCATION' not in refused['reason'], refused
        mark('refusals that change nothing and say so: a role of member_of_roles that does not exist (not checked in a dryrun), a name longer than the server takes, '
             'and the server\'s own message for a reserved name in the result of a task with a secret password')

        # How a task reaches the server. The suite's superuser with a
        # password is itself made by MMX, through the local socket.
        admin = {'dangerously_skip_server_certificate_check': True, 'database_server_host': '127.0.0.1', 'login_role': 'mmx_admin', 'login_password': '{{ .secrets.login_password }}'}
        project([{'id': 'admin', 'postgresql_role': {'name': 'mmx_admin', 'password': '{{ .secrets.login_password }}', 'can_log_in': True, 'superuser': True}},
                 {'id': 'socket_login', 'postgresql_role': {'name': 'mmx_socket_login', 'password': '{{ .secrets.login_password }}', 'can_log_in': True, 'superuser': True}},
                 {'id': 'deploy', 'postgresql_role': {'name': 'mmx_deploy', 'password': '{{ .secrets.login_password }}', 'can_log_in': True, 'can_create_roles': True, 'can_create_databases': True}}])
        assert all(state == created() for state in outcomes(run('run')).values())

        # A server on another machine, reached here through 127.0.0.1: psql
        # runs as root, with the password file in the task's private
        # directory, which is root's alone.
        # The database task gives the example's database to the new role;
        # in the dryrun that role does not exist yet.
        over_tcp = [{'id': 'role', 'postgresql_role': {'name': 'mmx_over_tcp', 'password': '{{ .secrets.role_password }}', 'can_log_in': True, **admin}},
                    {'id': 'database', 'postgresql_database': {'name': 'shop', 'owner': 'mmx_over_tcp', **admin}}]
        project(over_tcp)
        watch_psql()
        assert outcomes(run('dryrun')) == {'role': created('dryrun'), 'database': NOT_CHECKED}
        first = run('run')
        calls = psql_calls()
        assert outcomes(first) == {'role': created(), 'database': changed('owner')} and all(r.get('secret') is not True for r in results(first).values()), results(first)
        # The role is in psql's environment and on no command line, and
        # psql has no other environment than the one MMX sets.
        assert calls and all(call['account'] == 'root' and call['password_file'] == 'root:600' and call['directory'] == 'root:700' and call['certificate_check'] == 'prefer'
                             and call['role'] == 'mmx_admin' and call['arguments'].endswith('--host=127.0.0.1') and 'mmx_admin' not in call['arguments']
                             and only_the_environment_that_mmx_sets(call, 'PGSSLMODE') for call in calls), calls
        assert database_in_catalog('shop')['owner'] == 'mmx_over_tcp'
        assert_logins([('mmx_over_tcp', ROLE_PASSWORD)], [True])
        for command in ('run', 'dryrun'):
            assert all_unchanged(run(command))
        no_private_task_directory_left()
        # A wrong login password: the server's message, in a run and in a
        # dryrun, and the result is not hidden. Another try cannot fix a
        # login that the server refuses, and each try would be one more
        # failed login in the server's log: the task has one try.
        vm.ENV['MMX_TEST_POSTGRESQL_LOGIN_PASSWORD'] = 'not the password'
        for command in ('dryrun', 'run'):
            refused = results(run(command, 1))['role']
            assert refused['status'] == 'failed' and refused['change'] == 'unchanged' and refused.get('failure_kind') == 'config' and len(refused.get('tries') or [refused]) == 1, refused
            assert refused.get('secret') is not True and 'password authentication failed for user "mmx_admin"' in refused['reason'] and refused['reason'].startswith(
                'postgresql_role: could not read the role mmx_over_tcp: psql got no session with the PostgreSQL server at 127.0.0.1 as the role mmx_admin: ') and refused['reason'].endswith(
                "; check login_role and login_password, and that the server's pg_hba.conf has a line that lets this role log in from this node with a password"), refused
        vm.ENV['MMX_TEST_POSTGRESQL_LOGIN_PASSWORD'] = LOGIN_PASSWORD
        no_private_task_directory_left()
        mark('database_server_host with login_role and login_password: psql runs as root with only the environment that MMX sets, the role in it, '
             'and a password file that is root\'s alone and gone afterwards; a wrong password fails a run and a dryrun with the server\'s message and advice, after one try')

        # The server's certificate. With the file of the authority that
        # signed it, psql checks the certificate and the host name and the
        # task works; with another authority's file it fails, with the
        # client's message.
        with_authority = {key: value for key, value in admin.items() if key != 'dangerously_skip_server_certificate_check'}
        with_authority.update({ 'database_server_host': 'localhost', 'database_server_certificate_authority_file': tls + '/authority.crt'})
        checked = [{'id': 'role', 'postgresql_role': {'can_log_in': False, 'name': 'mmx_checked_certificate', **with_authority}},
                   {'id': 'database', 'postgresql_database': {'name': 'shop', 'owner': 'mmx_over_tcp', **with_authority}}]
        default_checked = [
            {'id': 'role', 'postgresql_role': {'name': 'mmx_checked_certificate', 'can_log_in': False,
                'database_server_host': 'localhost', 'login_role': 'mmx_admin', 'login_password': '{{ .secrets.login_password }}'}},
            {'id': 'database', 'postgresql_database': {'name': 'shop', 'owner': 'mmx_over_tcp',
                'database_server_host': 'localhost', 'login_role': 'mmx_admin', 'login_password': '{{ .secrets.login_password }}'}},
        ]
        project(default_checked)
        for command in ('dryrun', 'run'):
            refused = results(run(command, 1))['role']
            assert refused['status'] == 'failed' and refused['failure_kind'] == 'config', refused
            assert "For a private authority or a self-signed certificate, give database_server_certificate_authority_file. dangerously_skip_server_certificate_check: true turns off the check of the server's identity." in refused['reason'], refused
        project(checked)
        watch_psql()
        assert outcomes(run('dryrun')) == {'role': created('dryrun'), 'database': UNCHANGED}
        assert outcomes(run('run')) == {'role': created(), 'database': UNCHANGED}
        calls = psql_calls()
        assert calls and all(call['certificate_check'] == 'verify-full' and call['authority_file'] == tls + '/authority.crt' and '--host=localhost' in call['arguments']
                             and only_the_environment_that_mmx_sets(call, 'PGSSLMODE', 'PGSSLROOTCERT') for call in calls), calls
        assert all_unchanged(run('run'))
        for task in checked:
            task[next(key for key in task if key != 'id')].update(name='mmx_other_authority', database_server_certificate_authority_file=tls + '/other-authority.crt')
        project(checked)
        for command in ('dryrun', 'run'):
            refused = results(run(command, 1))['role']
            assert refused['status'] == 'failed' and 'certificate verify failed' in refused['reason'] and (
                'postgresql_role: could not read the role mmx_other_authority: psql got no session with the PostgreSQL server at localhost as the role mmx_admin: ' in refused['reason']), refused
            assert "For a private authority or a self-signed certificate, give database_server_certificate_authority_file. dangerously_skip_server_certificate_check: true turns off the check of the server's identity." in refused['reason'], refused
        # The certificate names localhost, not 127.0.0.1.
        for task in checked:
            task[next(key for key in task if key != 'id')].update(database_server_host='127.0.0.1', database_server_certificate_authority_file=tls + '/authority.crt')
        project(checked)
        refused = results(run('run', 1))['role']
        assert refused['status'] == 'failed' and 'does not match host name "127.0.0.1"' in refused['reason'], refused
        # A file that is not on the node yet: an earlier node task may
        # install it.
        for task in checked:
            task[next(key for key in task if key != 'id')].update(database_server_host='localhost', database_server_certificate_authority_file=tls + '/not-installed.crt')
        project(checked)
        assert outcomes(run('dryrun')) == {'role': NOT_CHECKED, 'database': NOT_CHECKED}
        refused = results(run('run', 1))['role']
        assert refused['failure_kind'] == 'missing_prerequisite' and refused['reason'] == f'postgresql_role: database_server_certificate_authority_file {tls}/not-installed.crt does not exist on the node; install the certificate of the authority in an earlier node task', refused
        assert role_in_catalog('mmx_other_authority')['attributes'] is None and database_in_catalog('mmx_other_authority') is None
        mark('database_server_certificate_authority_file: a login works with the file of the authority that signed the server\'s certificate, '
             'and fails with another authority\'s file and for a host name that the certificate does not name, with the client\'s message')

        # A password login through the local socket. The login role is a
        # secret here: the task accepts it and its result is not hidden.
        through_the_socket = {'id': 'role', 'postgresql_role': {'can_log_in': False, 'name': 'mmx_through_the_socket', 'login_role': '{{ .secrets.login_role }}', 'login_password': '{{ .secrets.login_password }}'}}
        project([through_the_socket])
        watch_psql()
        made = results(run('run'))['role']
        calls = psql_calls()
        assert outcome(made) == created() and made.get('secret') is not True, made
        assert calls and all(call['account'] == 'postgres' and call['password_file'] == 'postgres:600' and call['directory'] == 'postgres:700'
                             and call['role'] == 'mmx_socket_login' and 'mmx_socket_login' not in call['arguments'] and '--host' not in call['arguments']
                             and only_the_environment_that_mmx_sets(call) for call in calls), calls
        # A task without login_password reads no password file of the
        # account that runs psql. The account postgres gets such a file here,
        # with which psql itself logs in as the role, also without HOME in
        # its environment. The task still gets no session; its result names
        # the role as [redacted], because the role is a secret, and says
        # what to check.
        home = remote('getent passwd postgres | cut -d: -f6')
        logged_in = remote(f'umask 077\ncat > {home}/.pgpass\nchown postgres: {home}/.pgpass\ncd /\n'
                           f'runuser -u postgres -- env -i PATH=/usr/sbin:/usr/bin:/sbin:/bin PGUSER=mmx_socket_login PGDATABASE=postgres '
                           f'{psql} --no-psqlrc --no-password --quiet --no-align --tuples-only --command="select current_user"',
                           input=f'localhost:*:*:mmx_socket_login:{in_password_file(LOGIN_PASSWORD)}\n')
        assert logged_in == 'mmx_socket_login', logged_in
        del through_the_socket['postgresql_role']['login_password']
        project([through_the_socket])
        watch_psql()
        try:
            for command in ('dryrun', 'run'):
                refused = results(run(command, 1))['role']
                assert refused['status'] == 'failed' and refused['change'] == 'unchanged' and refused.get('failure_kind') == 'config' and len(refused.get('tries') or [refused]) == 1, refused
                assert refused.get('secret') is not True and 'no password supplied' in refused['reason'] and 'mmx_socket_login' not in refused['reason'] and refused['reason'].startswith(
                    "postgresql_role: could not read the role mmx_through_the_socket: psql got no session with the PostgreSQL server on this node's local socket as the role [redacted]: "
                ) and refused['reason'].endswith('; or give the task login_role and login_password'), refused
        finally:
            remote(f'rm -f {home}/.pgpass')
        calls = psql_calls()
        assert calls and all(call['password_file'] == '/dev/null/mmx_task_without_login_password' and only_the_environment_that_mmx_sets(call) for call in calls), calls
        # A login role that is no superuser.
        deploy = {'dangerously_skip_server_certificate_check': True, 'database_server_host': '127.0.0.1', 'login_role': 'mmx_deploy', 'login_password': '{{ .secrets.login_password }}'}
        project([{'id': 'made', 'postgresql_role': {'name': 'mmx_made_by_deploy', 'can_log_in': True, **deploy}},
                 {'id': 'password', 'postgresql_role': {'can_log_in': True, 'name': 'mmx_made_by_deploy', 'password': 'x', **deploy}}])
        refused = results(run('run', 1))
        assert outcome(refused['made']) == created() and role_in_catalog('mmx_made_by_deploy')['attributes']['can_log_in'] is True
        assert refused['password']['failure_kind'] == 'config' and refused['password']['change'] == 'unchanged' and refused['password']['reason'].startswith(
            'postgresql_role: the login role mmx_deploy is not a superuser and cannot read stored passwords from pg_catalog.pg_authid'), refused['password']
        assert role_in_catalog('mmx_made_by_deploy')['stored_password'] is None
        mark('login_password without database_server_host logs in through the local socket as the account postgres, with a login role that is a secret; '
             'without login_password the task does not read the password file in the home directory of the account postgres; a login role that is no superuser '
             'creates a role, and a task with password fails for it before it changes anything')

        # No server answers on another port. A dryrun is not checked, because
        # an earlier node task may start the server.
        nowhere_to_go = [{'id': 'role', 'retry': False, 'postgresql_role': {'can_log_in': False, 'name': 'mmx_group', 'database_server_port': 5999}},
                         {'id': 'database', 'postgresql_database': {'owner': 'mmx_over_tcp', 'name': 'shop', 'database_server_port': 5999}}]
        project(nowhere_to_go)
        assert outcomes(run('dryrun')) == {'role': NOT_CHECKED, 'database': NOT_CHECKED}
        refused = results(run('run', 1))['role']
        assert refused['status'] == 'failed' and refused['change'] == 'unchanged' and refused.get('failure_kind') is None and refused['reason'].startswith(
            "postgresql_role: no PostgreSQL server answers on this node's local socket, port 5999"), refused
        project([{'id': 'role', 'postgresql_role': {'can_log_in': False, 'name': 'mmx_group', 'database_server_port': 5432}}])
        assert all_unchanged(run('run'))
        mark('database_server_port: no server answers on another port, so a dryrun is not checked and a run fails; the server\'s own port works')

        # Where a password goes. A sampler reads every process command line
        # and the environment of every psql and pg_isready while a run sets
        # a role's password with a login password; the wrapper keeps each
        # psql alive for a second so that the sampler sees it.
        project([{'id': 'role', 'postgresql_role': {'name': 'mmx_watched', 'password': '{{ .secrets.role_password }}', 'can_log_in': True, **admin}}])
        watch_psql(slow=True)
        remote(f"""cat > {BASE}/sampler <<'SAMPLER'
while test ! -e {BASE}/stop-sampler; do
  for process in /proc/[0-9]*; do
    if grep -q -a -e psql -e pg_isready $process/cmdline 2>/dev/null; then
      {{ tr '\\000' ' ' < $process/cmdline; echo; tr '\\000' '\\n' < $process/environ; }} >> {BASE}/sampled 2>/dev/null
    fi
  done
done
SAMPLER
rm -f {BASE}/stop-sampler
install -m 600 /dev/null {BASE}/sampled
nohup sh {BASE}/sampler >/dev/null 2>&1 &""")
        watched = results(run('run'))['role']
        remote(f'touch {BASE}/stop-sampler\nsleep 0.3')
        psql_calls()
        assert outcome(watched) == created() and watched.get('secret') is not True, watched
        stored = [role_in_catalog(name)['stored_password'] for name in ('mmx_watched', ROLE, 'shop', 'mmx_admin')]
        assert all(form.startswith('SCRAM-SHA-256$') for form in stored), stored
        sampled = remote(f'umask 077\ncat > {BASE}/texts\ngrep -c -e "--no-password" {BASE}/sampled || true\ngrep -c PGPASSFILE=/run/mmx/private_task_directories/postgresql_role- {BASE}/sampled || true\n'
                         f'grep -c -F -f {BASE}/texts {BASE}/sampled || true\nrm -f {BASE}/texts {BASE}/sampled {BASE}/sampler {BASE}/stop-sampler',
                         input='\n'.join([ROLE_PASSWORD, LOGIN_PASSWORD, in_password_file(LOGIN_PASSWORD)] + stored) + '\n').split('\n')
        assert int(sampled[0]) > 0 and int(sampled[1]) > 0, 'the sampler never saw psql with its password file'
        assert sampled[2] == '0', 'a password or a stored form was on a command line or in the environment of psql'
        nowhere('roles and logins', [SHOP_PASSWORD, ROLE_PASSWORD, LOGIN_PASSWORD, in_password_file(LOGIN_PASSWORD)] + stored)
        mark('no password and no stored form of one is on a command line or in an environment of psql, in what mmx printed or saved, in the node\'s records, '
             'in the journal, or in a home directory, and no private directory of a task is left')

        # Databases. The task names its locale in another spelling than the
        # template database has; the server would refuse that as text.
        template = settings['template']
        other_spelling = template.replace('.UTF-8', '.utf8') if '.UTF-8' in template else template.replace('.utf8', '.UTF-8')
        assert other_spelling != template, template
        extensions_package = [] if package_manager != 'dnf' else ['postgresql-contrib']
        shop_database = {'name': DATABASE, 'owner': ROLE, 'encoding': 'utf-8', 'locale': other_spelling, 'extensions': ['pg_trgm', 'uuid-ossp'], 'extensions_not_listed': 'keep'}
        project([{'id': 'database', 'postgresql_database': shop_database}])
        assert outcomes(run('dryrun')) == {'database': created('dryrun')} and database_in_catalog(DATABASE) is None
        assert outcomes(run('run')) == {'database': created()}
        assert database_in_catalog(DATABASE) == {'owner': ROLE, 'encoding': 'UTF8', 'collation': template, 'character_type': template, 'extensions': ['pg_trgm', 'plpgsql', 'uuid-ossp']}
        for command in ('run', 'dryrun'):
            assert all_unchanged(run(command))
        mark('a database with a name full of quotes: a dryrun that changes nothing, the first run with owner, encoding, locale, and two extensions, and an unchanged run and dryrun; '
             f'the task writes the locale as {other_spelling} and the template has {template}')

        # Another locale and encoding than the template's need template0.
        # MMX reads the template before it creates anything, so a dryrun
        # promises no database that the server would refuse: both fail, with
        # one try. A template database that does not exist is one that an
        # earlier node task may still create.
        other = {'name': 'mmx_reports', 'owner': 'postgres', 'encoding': 'LATIN1', 'locale': 'POSIX'}
        project([{'id': 'database', 'postgresql_database': other}])
        for command in ('dryrun', 'run'):
            refused = results(run(command, 1))['database']
            assert refused['status'] == 'failed' and refused['change'] == 'unchanged' and refused.get('failure_kind') == 'config' and len(refused.get('tries') or [refused]) == 1, refused
            assert refused['reason'].startswith('postgresql_database: the database mmx_reports cannot be created as a copy of the template database template1: '
                                                f'the template database encoding is UTF8, but the node task requests LATIN1; the template database locale is {template}, but the node task requests POSIX. ') and 'template_database: template0' in refused['reason'], refused
        project([{'id': 'database', 'postgresql_database': {'owner': 'postgres', 'name': 'mmx_reports', 'template_database': 'mmx_no_such_template'}}])
        assert outcomes(run('dryrun')) == {'database': NOT_CHECKED}
        missing = results(run('run', 1))['database']
        assert missing['failure_kind'] == 'missing_prerequisite' and missing['change'] == 'unchanged' and missing['reason'].startswith(
            'postgresql_database.template_database: the database mmx_no_such_template, which the new database mmx_reports would be a copy of, does not exist'), missing
        assert database_in_catalog('mmx_reports') is None
        project([{'id': 'database', 'postgresql_database': {'owner': 'postgres', **other, 'template_database': 'template0'}}])
        assert outcomes(run('dryrun')) == {'database': created('dryrun')}
        assert outcomes(run('run')) == {'database': created()}
        assert database_in_catalog('mmx_reports') == {'owner': 'postgres', 'encoding': 'LATIN1', 'collation': 'C', 'character_type': 'C', 'extensions': ['plpgsql']}
        project([{'id': 'database', 'postgresql_database': {'owner': 'postgres', 'name': 'mmx_reports', 'encoding': 'latin1', 'locale': 'C'}}])
        for command in ('run', 'dryrun'):
            assert all_unchanged(run(command))
        # An existing database with another encoding or locale is not changed.
        project([{'id': 'database', 'postgresql_database': {'name': 'mmx_reports', 'owner': ROLE, 'encoding': 'UTF8', 'locale': template}}])
        for command in ('dryrun', 'run'):
            refused = results(run(command, 1))['database']
            assert refused['failure_kind'] == 'config' and refused['change'] == 'unchanged' and (
                'the database encoding is LATIN1, but the node task requests UTF8; the database locale is C, but the node task requests ' + template in refused['reason']), refused
        assert database_in_catalog('mmx_reports')['owner'] == 'postgres'
        mark('another encoding and locale than the template\'s: a run and a dryrun fail without template0 before anything changes, and the database is made with it; '
             'a template database that does not exist (not checked in a dryrun); an existing database with another encoding or locale fails the task and is not changed')

        # The owner and the extensions of an existing database.
        project([{'id': 'database', 'postgresql_database': {**shop_database, 'owner': 'mmx_no_such_role'}}])
        assert outcomes(run('dryrun')) == {'database': NOT_CHECKED}
        missing = results(run('run', 1))['database']
        assert missing['failure_kind'] == 'missing_prerequisite' and missing['change'] == 'unchanged' and missing['reason'].startswith('postgresql_database.owner: the role mmx_no_such_role does not exist'), missing
        project([{'id': 'database', 'postgresql_database': {**shop_database, 'owner': 'mmx_group', 'extensions': ['hstore', 'pg_trgm'], 'extensions_not_listed': 'remove'}}])
        assert outcomes(run('dryrun')) == {'database': changed('owner', 'extensions', command='dryrun')}
        assert outcomes(run('run')) == {'database': changed('owner', 'extensions')}
        in_catalog = database_in_catalog(DATABASE)
        # uuid-ossp is gone; plpgsql, which the list does not name either, stays.
        assert in_catalog['owner'] == 'mmx_group' and in_catalog['extensions'] == ['hstore', 'pg_trgm', 'plpgsql'], in_catalog
        assert all_unchanged(run('run'))
        project([{'id': 'database', 'postgresql_database': {'owner': 'mmx_group', 'name': DATABASE, 'extensions': ['pg_trgm', 'mmx_no_such_extension'], 'extensions_not_listed': 'remove'}}])
        assert outcomes(run('dryrun')) == {'database': NOT_CHECKED}
        missing = results(run('run', 1))['database']
        assert missing['failure_kind'] == 'missing_prerequisite' and 'does not have the files of the extension mmx_no_such_extension' in missing['reason'] and all(
            name in missing['reason'] for name in extensions_package), missing
        assert database_in_catalog(DATABASE)['extensions'] == ['hstore', 'pg_trgm', 'plpgsql'], 'a task without the files of an extension removed another extension'
        # The server refuses to drop an extension that another one needs,
        # and nothing of the task's other statements stays.
        project([{'id': 'database', 'postgresql_database': {'owner': 'mmx_group', 'name': DATABASE, 'extensions': ['cube', 'earthdistance'], 'extensions_not_listed': 'keep'}}])
        assert outcomes(run('run')) == {'database': changed('extensions')}
        project([{'id': 'database', 'retry': False, 'postgresql_database': {'owner': 'mmx_group', 'name': DATABASE, 'extensions': ['earthdistance', 'uuid-ossp'], 'extensions_not_listed': 'remove'}}])
        refused = results(run('run', 1))['database']
        assert refused['status'] == 'failed' and refused['change'] == 'unchanged' and ' 2BP01: ' in refused['reason'] and refused['reason'].startswith(
            'postgresql_database: could not change the extensions of the database ' + DATABASE + ': '), refused
        assert database_in_catalog(DATABASE)['extensions'] == ['cube', 'earthdistance', 'hstore', 'pg_trgm', 'plpgsql']
        mark('an existing database: a new owner, extensions installed and removed while plpgsql stays, an owner and an extension that are not there '
             '(not checked in a dryrun), and the server\'s refusal to drop an extension that another one needs')

        # Privileges finer than ownership are an SQL file that a shell task
        # applies when the file changed, as docs/how-to/manage-a-database.md shows.
        def privileges(statement):
            return [{'task_block': {'id': 'privileges',
                                    'changes': [{'id': 'file', 'file': {'path': BASE + '/privileges.sql', 'owner': 'postgres', 'permissions_mode': '0600', 'content': statement + '\n'}}],
                                    'activate': [{'id': 'apply', 'shell': {'script': f'psql --no-psqlrc --set ON_ERROR_STOP=1 --dbname=mmx_reports --file={BASE}/privileges.sql',
                                                                           'run_as_user_account': 'postgres', 'only_when_tracked_inputs_changed': True}}]}}]

        may_create = "select has_database_privilege('mmx_reporting', 'mmx_reports', 'CREATE');\n"
        project(privileges('grant create on database mmx_reports to mmx_reporting;'))
        granted = results(run('run'))
        assert granted['file']['change'] == 'changed' and granted['apply']['change'] == 'changed' and sql(may_create) == 't', granted
        assert run('run')['entries'][0]['response']['change'] == 'unchanged'
        project(privileges('revoke create on database mmx_reports from mmx_reporting;'))
        assert results(run('run'))['apply']['change'] == 'changed' and sql(may_create) == 'f'
        mark('privileges inside a database: an SQL file in the changes of a node task block, applied by a shell task with psql only when the file changed')

        # A run that ends after CREATE DATABASE and before the extensions:
        # the wrapper ends every psql for the new database. The next run
        # finds the database and installs what is missing.
        interrupted = {'id': 'database', 'retry': False, 'postgresql_database': {'name': 'mmx_interrupted', 'owner': 'mmx_group', 'extensions': ['pg_trgm'], 'extensions_not_listed': 'keep'}}
        project([interrupted])
        watch_psql(fail_for_database='mmx_interrupted')
        stopped = results(run('run', 1))['database']
        psql_calls()
        assert outcome(stopped) == ('failed', 'changed', True, []) and 'could not change the extensions of the database mmx_interrupted' in stopped['reason'], stopped
        assert database_in_catalog('mmx_interrupted') == {'owner': 'mmx_group', 'encoding': 'UTF8', 'collation': template, 'character_type': template, 'extensions': ['plpgsql']}
        assert outcomes(run('dryrun')) == {'database': changed('extensions', command='dryrun')}
        assert outcomes(run('run')) == {'database': changed('extensions')} and database_in_catalog('mmx_interrupted')['extensions'] == ['pg_trgm', 'plpgsql']
        assert all_unchanged(run('run'))
        mark('a run that ended between CREATE DATABASE and the extensions: its result says that the database was created, and the next run installs the extensions')

        # Removal. A role that owns a database stays, with the server's
        # message. A database with a session stays too: the server waits
        # five seconds for the session to end and then refuses.
        project([{'id': 'role', 'retry': False, 'postgresql_role': {'name': 'mmx_group', 'state': 'absent'}}])
        assert outcomes(run('dryrun')) == {'role': changed(command='dryrun')}
        refused = results(run('run', 1))['role']
        assert refused['status'] == 'failed' and refused['change'] == 'unchanged' and ' 2BP01: ' in refused['reason'] and refused['reason'].startswith('postgresql_role: could not remove the role mmx_group: '), refused
        assert role_in_catalog('mmx_group')['attributes'] is not None
        deleted = [{'id': 'database', 'retry': False, 'postgresql_database': {'name': DATABASE, 'state': 'deleted_with_all_its_data'}}]
        project(deleted)
        assert outcomes(run('dryrun')) == {'database': changed(command='dryrun')} and database_in_catalog(DATABASE) is not None
        # A session of the suite's own in the database, which sleeps there.
        remote(f'cd /\nnohup runuser -u postgres -- env PGDATABASE={shlex.quote(DATABASE)} {psql} --no-psqlrc --command="select pg_sleep(60)" >/dev/null 2>&1 &')
        sessions = "select count(*) from pg_stat_activity where datname = :'name';\n"
        deadline = time.monotonic() + 10
        while sql(sessions, variables={'name': DATABASE}) != '1':
            assert time.monotonic() < deadline, 'the session in the database did not start'
            time.sleep(0.1)
        refused = results(run('run', 1))['database']
        assert refused['status'] == 'failed' and refused['change'] == 'unchanged' and ' 55006: ' in refused['reason'] and refused['reason'].startswith('postgresql_database: could not drop the database ' + DATABASE + ': '), refused
        assert database_in_catalog(DATABASE) is not None
        # The suite ends its own session; its psql ends with it.
        sql("select pg_terminate_backend(pid) from pg_stat_activity where datname = :'name';\n", variables={'name': DATABASE})
        while sql(sessions, variables={'name': DATABASE}) != '0':
            time.sleep(0.1)
        deleted += [{'id': name, 'postgresql_database': {'name': name, 'state': 'deleted_with_all_its_data'}} for name in ('mmx_interrupted', 'mmx_reports')]
        deleted += [{'id': 'role', 'postgresql_role': {'name': 'mmx_group', 'state': 'absent'}}]
        project(deleted)
        gone = outcomes(run('run'))
        assert gone == {name: changed() for name in ('database', 'mmx_interrupted', 'mmx_reports', 'role')}, gone
        assert all(database_in_catalog(name) is None for name in (DATABASE, 'mmx_interrupted', 'mmx_reports')) and role_in_catalog('mmx_group')['attributes'] is None
        for command in ('run', 'dryrun'):
            assert all_unchanged(run(command))
        long_name = 'mmx_' + 'd' * 60
        project([{'id': 'long', 'postgresql_database': {'name': long_name, 'state': 'deleted_with_all_its_data'}}])
        refused = results(run('run', 1))['long']
        assert refused['failure_kind'] == 'config' and 'takes names of at most 63 bytes' in refused['reason'], refused
        mark('removal: a role that owns a database and a database with a session stay, with the server\'s message; afterwards both go, '
             'a missing database and a missing role are unchanged, and a name longer than the server takes fails')
        nowhere('at the end', [SHOP_PASSWORD, ROLE_PASSWORD, LOGIN_PASSWORD])

        # The server is stopped, as before its first start: a dryrun is not
        # checked, because an earlier node task may start it, and a run
        # fails. This comes last, because the server is not started again.
        stop_server()
        for task in nowhere_to_go:
            del task[next(key for key in task if key not in ('id', 'retry'))]['database_server_port']
        project(nowhere_to_go)
        assert outcomes(run('dryrun')) == {'role': NOT_CHECKED, 'database': NOT_CHECKED}
        refused = results(run('run', 1))['role']
        assert refused['status'] == 'failed' and refused['change'] == 'unchanged' and refused.get('failure_kind') is None and refused['reason'].startswith(
            "postgresql_role: no PostgreSQL server answers on this node's local socket; install and start the server in an earlier node task"), refused
        mark('a stopped server: a dryrun reports both tasks not checked, and a run fails')
    finally:
        failing = sys.exc_info()[0] is not None
        for name in environment:
            vm.ENV.pop(name, None)
        cleanup()
        if not failing:
            guest_is_as_found()
    (workspace / 'summary.json').write_text(json.dumps(passed, indent=2) + '\n')
    print(f'{len(passed)} PostgreSQL workflows passed', flush=True)


if __name__ == '__main__':
    vm.run_suite(main)
