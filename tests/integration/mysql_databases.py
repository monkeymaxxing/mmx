#!/usr/bin/env python3
"""mysql_user and mysql_database through the public CLI, against a MariaDB server on the pair's first guest and, where the distribution packages MySQL, a MySQL server on the second; checked with the servers' own client programs."""
from concurrent.futures import ThreadPoolExecutor
import hashlib
import json
import shlex
import shutil
import sys
import time

import vm

BASE = '/var/tmp/mmx-mysql-databases'
# Every call of the client program goes through a wrapper that notes its
# command line and its environment here.
CLIENT_CALLS = BASE + '/client-calls'
# The application account's password: quotes, a backslash, and letters that
# are not ASCII. MMX never sends it to a server; it sends the stored form.
ACCOUNT_PASSWORD = 'Sentinel-pä\'ss"w\\örd-4711'
SECOND_ACCOUNT_PASSWORD = 'Second-sentinel-0815'
# The password of the administrator that tasks log in as. It reaches the
# client through MMX's login file, whose values need quoting: a double
# quote, a backslash, a #, and spaces at both ends.
ADMINISTRATOR_PASSWORD = ' Administrator "sentinel" \\pass #3 '
ADMINISTRATOR = 'mmx-administrator'
SECRETS = {'secrets_from_env': {'account_password': 'MMX_TEST_ACCOUNT_PASSWORD', 'administrator_password': 'MMX_TEST_ADMINISTRATOR_PASSWORD'}}
# A file that would send a plain call of the client to another server and
# account. The tasks must not read it.
DECOY_OPTION_FILE = '[client]\nhost=192.0.2.1\nuser=mmx-nobody\npassword=wrong\n[mysqld]\nsocket=/run/mmx-nowhere.sock\n'
EXAMPLE = 'mariadb-server-for-one-application'


def native_stored_value(password):
    """The stored form of mysql_native_password, as the servers compute it."""
    return '*' + hashlib.sha1(hashlib.sha1(password.encode()).digest()).hexdigest().upper()


def mysql_package(distribution, version):
    """The distribution's MySQL server package and its unit, None where the distribution packages no MySQL."""
    major = int(version.split('.')[0])
    if distribution == 'ubuntu':
        return 'mysql-server', 'mysql'
    if distribution in ('rocky', 'almalinux'):
        return ('mysql-server' if major == 9 else 'mysql8.4-server'), 'mysqld'
    return None


def main():
    vm.up()
    node, ssh = vm.connection()
    vm.reset()
    distribution, version = vm.cmd([*ssh, '. /etc/os-release; echo "$ID $VERSION_ID"']).stdout.split()
    # MariaDB's packages are the example project's own tasks.
    servers = {'mariadb': {'guest': vm.NAME, 'node': node, 'ssh': ssh, 'product': 'MariaDB', 'client': 'mariadb', 'unit': 'mariadb'}}
    packaged = mysql_package(distribution, version)
    if packaged:
        vm.up('mmx-peer')
        peer, peer_ssh = vm.connection('mmx-peer')
        vm.reset('mmx-peer')
        servers['mysql'] = {'guest': 'mmx-peer', 'node': peer, 'ssh': peer_ssh, 'product': 'MySQL', 'client': 'mysql', 'unit': packaged[1], 'packages': [packaged[0]]}
    workspace = vm.ROOT / 'mysql-databases'
    if workspace.exists():
        shutil.rmtree(workspace)
    vm.write_inventory(workspace, {'nodes': {name: server['node'] for name, server in servers.items()}}, name='example')
    vm.build()
    mmx = vm.ROOT / 'bin/mmx'
    passed = []
    marked = time.monotonic()
    # Everything mmx printed, to search for the passwords at the end.
    printed = []
    apt = distribution in ('debian', 'ubuntu')
    red_hat_family = not apt
    configuration_directory = {'mariadb': '/etc/mysql/mariadb.conf.d' if apt else '/etc/my.cnf.d', 'mysql': '/etc/mysql/mysql.conf.d' if apt else '/etc/my.cnf.d'}

    def remote(name, script, input=None):
        return vm.cmd([*servers[name]['ssh'], 'sudo -n /bin/sh -c ' + shlex.quote('set -eu\n' + script)], input=input).stdout.strip()

    def sql(name, statements, check=True):
        """Run statements on the node's server as root through its socket, with the real client and none of MMX; return the rows."""
        p = vm.cmd([*servers[name]['ssh'], f"sudo -n /usr/bin/{servers[name]['client']} --no-defaults{servers[name].get('socket_option', '')} --batch --skip-column-names --default-character-set=utf8mb4"],
                   input=statements, check=check)
        return [line.split('\t') for line in p.stdout.split('\n') if line] if p.returncode == 0 else p.stderr.strip()

    def login(name, user, password, statements, host=None):
        """Log in as an account with its clear password, as an application does: through the socket, or over TCP to host. Return the rows, or the client's error text."""
        options = f'[client]\nuser="{user}"\npassword="' + password.replace('\\', '\\\\').replace('"', '\\"') + '"\n'
        if host:
            options += f'host="{host}"\nprotocol=tcp\n'
        remote(name, f'install -m 600 /dev/null {BASE}/application-login.cnf\ncat > {BASE}/application-login.cnf', input=options)
        # MySQL's first login of a caching_sha2_password account over TCP
        # without TLS needs the server's public key.
        extra = ' --get-server-public-key' if servers[name]['client'] == 'mysql' else ''
        p = vm.cmd([*servers[name]['ssh'], f"sudo -n /usr/bin/{servers[name]['client']} --defaults-file={BASE}/application-login.cnf --batch --skip-column-names{extra}"], input=statements, check=False)
        return [line.split('\t') for line in p.stdout.split('\n') if line] if p.returncode == 0 else p.stderr.strip()

    def project(tasks, **settings):
        """One fleet stage that runs the same node tasks on every server's node at once."""
        vm.write_project(workspace, project_name='mysql_databases', node_task_defaults={'retry': {'pause_between_tries': '0s'}},
                         fleet_stages=[{'id': 'configure', 'nodes': 'all', 'node_tasks': tasks}], **{**SECRETS, **settings})

    def only_on(name, task):
        return {'when': {'equals': ['{{ .node.name }}', name]}, **task}

    def run(command, expected=None, directory=None, playbook='configure', timeout=600):
        result = vm.cmd([mmx, command, *vm.playbook_args(command, playbook), '--json'], cwd=directory or workspace, check=False, timeout=timeout)
        printed.append(result.stdout + result.stderr)
        if result.returncode not in (vm.success_codes(command) if expected is None else (expected,)):
            raise AssertionError((command, result.returncode, result.stdout, result.stderr))
        return json.loads(result.stdout)

    def results(record, name):
        """The results of one node's tasks, by the last part of each task's name."""
        entry = next(e for e in record['entries'] if e['node'] == name)
        return {r['task'].rsplit('/', 1)[-1]: r for r in entry['response']['results']}

    def each(record, check):
        """Apply check to each server's results; a failure names the server."""
        for name in servers:
            try:
                check(name, results(record, name))
            except AssertionError as error:
                raise AssertionError(f"{servers[name]['product']} on {distribution}: {error}") from error

    def is_result(result, status, change, created=None, changed_attributes=None, reason=None):
        """The result has this status and change, is not hidden as a secret, and has these parts of its value."""
        assert result['status'] == status and result['change'] == change and result.get('secret') is not True, result
        if created is not None:
            assert result['value'] == {'created': created, 'changed_attributes': changed_attributes or []}, result
        if reason is not None:
            assert reason in result.get('reason', ''), result
        return True

    def expect(condition, detail):
        assert condition, detail
        return True

    def tries(result):
        """How often the node task was tried."""
        return len(result.get('tries') or [result])

    def unchanged(record):
        for name in servers:
            entry = next(e for e in record['entries'] if e['node'] == name)
            assert entry['response']['change'] == 'unchanged', {r['task']: (r['status'], r['change'], r.get('reason', '')) for r in entry['response']['results']}
        return True

    def mark(message):
        # The seconds since the last mark say where the suite's time goes.
        nonlocal marked
        now = time.monotonic()
        passed.append(message)
        print(f'PASS {message} ({now - marked:.0f} s)', flush=True)
        marked = now

    def stored(name, user, host):
        """The account's authentication method and stored password, as the server keeps them; None when the server has no such account."""
        rows = sql(name, f"SELECT plugin, HEX(authentication_string) FROM mysql.user WHERE User = '{user}' AND Host = '{host}';")
        return (rows[0][0], bytes.fromhex(rows[0][1] if len(rows[0]) > 1 else '').decode('latin-1')) if rows else None

    def privileges(name, user, host):
        """What the account holds, from the server's catalog: a set of (place, privilege)."""
        grantee = f"\\'{user}\\'@\\'{host}\\'"
        rows = sql(name, f"""SELECT '*.*', PRIVILEGE_TYPE, IS_GRANTABLE FROM information_schema.USER_PRIVILEGES WHERE GRANTEE = '{grantee}';
SELECT CONCAT(TABLE_SCHEMA, '.*'), PRIVILEGE_TYPE, IS_GRANTABLE FROM information_schema.SCHEMA_PRIVILEGES WHERE GRANTEE = '{grantee}';
SELECT CONCAT(TABLE_SCHEMA, '.', TABLE_NAME), PRIVILEGE_TYPE, IS_GRANTABLE FROM information_schema.TABLE_PRIVILEGES WHERE GRANTEE = '{grantee}';
SELECT CONCAT(TABLE_SCHEMA, '.', TABLE_NAME, '(', COLUMN_NAME, ')'), PRIVILEGE_TYPE, IS_GRANTABLE FROM information_schema.COLUMN_PRIVILEGES WHERE GRANTEE = '{grantee}';
SELECT CONCAT(Routine_type, ' ', Db, '.', Routine_name), Proc_priv, 'NO' FROM mysql.procs_priv WHERE User = '{user}' AND Host = '{host}';
""")
        held = set()
        for place, privilege, grantable in rows:
            # The client prints a backslash of a value as two.
            place = place.replace('\\\\', '\\')
            if privilege != 'USAGE':
                held.add((place, privilege))
            if grantable == 'YES':
                held.add((place, 'GRANT OPTION'))
        return held

    def on_database(database, *names):
        return {(database + '.*', name) for name in names}

    # What a database server's packages leave behind when they are removed:
    # the data, the logs, the configuration, and their account.
    LEFT_BY_THE_PACKAGES = ['/var/lib/mysql', '/var/lib/mysql-files', '/var/lib/mysql-keyring', '/var/log/mysql', '/var/log/mariadb', '/var/log/mysqld.log',
                            '/run/mysqld', '/run/mariadb', '/etc/mysql', '/etc/my.cnf', '/etc/my.cnf.d']
    OF_THE_SUITE = [BASE, '/etc/mmx-test-database-tls', '/etc/mysql/mmx-test-database-tls', '/usr/local/bin/mariadb', '/usr/local/bin/mysql', '/usr/local/bin/my_print_defaults', '/root/.my.cnf',
                    '/etc/my.cnf.d/auth_socket.cnf', *(directory + '/zz-mmx-test.cnf' for directory in ('/etc/my.cnf.d', '/etc/mysql/mariadb.conf.d', '/etc/mysql/mysql.conf.d'))]
    # The names of the installed packages; the keys that dnf imports are no
    # packages of the distribution.
    INSTALLED_PACKAGES = ("if command -v dpkg-query >/dev/null; then dpkg-query -W -f '${db:Status-Abbrev} ${Package}\\n' | awk '$1 == \"ii\" {print $2}'; "
                          "else rpm -qa --qf '%{NAME}\\n' | grep -v '^gpg-pubkey$'; fi | sort -u")

    def before_the_suite(name):
        """What the guest had before the suite, which the cleanup must leave."""
        servers[name]['packages_before'] = set(remote(name, INSTALLED_PACKAGES).split())
        servers[name]['paths_before'] = remote(name, f"ls -d {' '.join(LEFT_BY_THE_PACKAGES)} 2>/dev/null || true").split()
        servers[name]['account_before'] = remote(name, 'getent passwd mysql || true') != ''

    def cleanup():
        """Remove the servers, their data, and everything of the suite, so that the next suite finds the guests as this one found them: exactly the packages that the suite's runs added go, with what they leave behind."""
        for name, server in servers.items():
            added = ' '.join(sorted(set(remote(name, INSTALLED_PACKAGES).split()) - server['packages_before']))
            paths = ' '.join(path for path in LEFT_BY_THE_PACKAGES if path not in server['paths_before'])
            account = '' if server['account_before'] else 'userdel mysql 2>/dev/null || true\ngroupdel mysql 2>/dev/null || true'
            remove = f"""if command -v apt-get >/dev/null; then DEBIAN_FRONTEND=noninteractive apt-get purge -y -q {added} >/dev/null 2>&1 || true
else dnf remove -y -q {added} >/dev/null 2>&1 || true
fi""" if added else ''
            remote(name, f"""
systemctl stop {server['unit']} 2>/dev/null || true
rm -rf {' '.join(OF_THE_SUITE)}
{remove}
rm -rf {paths}
{account}
systemctl daemon-reload
systemctl reset-failed 2>/dev/null || true
""")

    def guests_are_as_found():
        for name, server in servers.items():
            paths = ' '.join(path for path in LEFT_BY_THE_PACKAGES + OF_THE_SUITE if path not in server['paths_before'])
            left = remote(name, f"""
for program in mariadb mysql mysqld mariadbd my_print_defaults; do command -v $program || true; done
{'' if server['account_before'] else 'getent passwd mysql || true'}
ls -d {paths} /run/mmx/private_task_directories/* 2>/dev/null || true
systemctl list-unit-files 'mariadb*' 'mysql*' --no-legend 2>/dev/null || true
""")
            packages = set(remote(name, INSTALLED_PACKAGES).split())
            if packages != server['packages_before']:
                left += f"\npackages added: {sorted(packages - server['packages_before'])}; packages gone: {sorted(server['packages_before'] - packages)}"
            state = vm.guest('sudo', '-n', 'systemctl', 'is-system-running', name=server['guest'], check=False).stdout.strip()
            if state != 'running':
                left += f'\nsystemd is {state}:\n' + vm.guest('sudo', '-n', '/bin/sh', '-c', vm.FAILED_UNITS, name=server['guest'], check=False).stdout
            assert left == '', f"the suite left this on the guest of {server['product']}:\n{left}"
        print('The guests are as the suite found them: no database server, no data, and systemd is running', flush=True)

    def no_private_task_directory_left():
        """No node task left its private directory with a login file behind. The node makes the place of these directories when a task first needs one."""
        for name in servers:
            remote(name, 'if [ -d /run/mmx/private_task_directories ]; then test -z "$(ls -A /run/mmx/private_task_directories)"; fi')

    def install_mysql():
        """A MySQL server on the second guest, with the tasks of the example "The same tasks for a MySQL server" on mysql_database's page (internal/modules/mysql_database.md)."""
        server = servers['mysql']
        directory = vm.ROOT / 'mysql-databases-mysql-server'
        if directory.exists():
            shutil.rmtree(directory)
        vm.write_inventory(directory, {'nodes': {'mysql': server['node']}}, name='example')
        tasks = vm.package_tasks(server['packages'])
        if red_hat_family:
            # As installed there, root@localhost has an empty password, and
            # every local account can log in as root. The plugin and the
            # one statement make it the node's root account alone.
            tasks += [{'file': {'path': '/etc/my.cnf.d/auth_socket.cnf', 'permissions_mode': '0644', 'content': '[mysqld]\nplugin-load-add=auth_socket.so\n'}}]
        tasks += [{'systemd_service': {'name': server['unit'], 'state': 'running', 'start_at_boot': True}}]
        if red_hat_family:
            tasks += [{'id': 'root', 'shell': {
                'script': "mysql --no-defaults --execute \"ALTER USER 'root'@'localhost' IDENTIFIED WITH auth_socket\"",
                'skip_if_this_command_succeeds': "mysql --no-defaults --batch --skip-column-names --execute \"SELECT plugin FROM mysql.user WHERE user = 'root' AND host = 'localhost'\" | grep --quiet --line-regexp auth_socket"}}]
        vm.write_project(directory, project_name='mysql_server', node_task_defaults={'retry': {'pause_between_tries': '0s'}},
                         fleet_stages=[{'id': 'configure', 'nodes': 'all', 'node_tasks': tasks}])
        started = time.monotonic()
        run('run', directory=directory)
        seconds = time.monotonic() - started
        assert next(e for e in run('run', directory=directory)['entries'])['response']['change'] == 'unchanged'
        return seconds

    def install_mariadb():
        """A MariaDB server on the first guest, with the example project."""
        directory = vm.ROOT / ('mysql-databases-' + EXAMPLE)
        if directory.exists():
            shutil.rmtree(directory)
        shutil.copytree(vm.REPO / 'examples' / EXAMPLE, directory)
        vm.write_inventory(directory, {'nodes': {'database': servers['mariadb']['node']}, 'vars': {
            'application_database': 'shop', 'application_account': 'shop', 'application_connects_from': 'localhost'}}, name='example')
        started = time.monotonic()
        applied = run('run', directory=directory)
        return directory, applied, time.monotonic() - started

    for name in servers:
        has_a_client = remote(name, 'for program in mariadb mysql; do command -v $program || true; done')
        assert has_a_client == '', f"the guest of {servers[name]['product']} already has a database client: {has_a_client}"
        before_the_suite(name)
    try:
        database = {'id': 'database', 'mysql_database': {'name': 'mmx-shop', 'character_set': 'utf8mb4'}}
        account = {'id': 'account', 'mysql_user': {'name': 'mmx-shop', 'may_connect_from': '%', 'password': '{{ .secrets.account_password }}',
                                                   'privileges': [{'grant_on': 'mmx-shop.*', 'grant': ['SELECT', 'INSERT', 'UPDATE', 'DELETE', 'CREATE']}],
                                                   'privileges_not_listed': 'remove'}}
        vm.ENV['MMX_TEST_ACCOUNT_PASSWORD'] = ACCOUNT_PASSWORD
        vm.ENV['MMX_TEST_ADMINISTRATOR_PASSWORD'] = ADMINISTRATOR_PASSWORD
        vm.ENV['SHOP_DATABASE_PASSWORD'] = ACCOUNT_PASSWORD

        # Before a server is installed: the node has no client program.
        project([database, account])
        each(run('dryrun'), lambda name, r: expect(r['database']['status'] == 'not_checked', r))
        # The message names the client's package for the node's package manager.
        missing = ("neither the program mariadb nor the program mysql is installed; each comes with its database server's package, "
                   'and a server on another machine needs the client alone. mariadb is not installed; ')

        def without_client(name, r):
            assert r['database']['failure_kind'] == 'missing_prerequisite' and r['database']['reason'].startswith('mysql_database: ' + missing), r['database']
        each(run('run', 1), without_client)
        project([account])
        each(run('dryrun'), lambda name, r: is_result(r['account'], 'not_checked', 'unknown', reason='mysql_user: ' + missing))
        mark('without the client program, both modules name the client package for the node\'s package manager; a dryrun is not checked')

        # The servers, both at once: MariaDB with the example project on the
        # first guest, MySQL on the second.
        with ThreadPoolExecutor(max_workers=2) as pool:
            mariadb_installed = pool.submit(install_mariadb)
            mysql_installed = pool.submit(install_mysql) if 'mysql' in servers else None
            example, applied, seconds = mariadb_installed.result()
            if mysql_installed:
                print(f"MySQL: installed and started in {mysql_installed.result():.0f} s", flush=True)
        print(f'MariaDB: the example project installed and started it in {seconds:.0f} s', flush=True)
        for name, server in servers.items():
            server['version'] = sql(name, 'SELECT VERSION();')[0][0]
            assert ('MariaDB' in server['version']) == (name == 'mariadb'), server['version']
            print(f"{server['product']} {server['version']}, client: {remote(name, '/usr/bin/' + server['client'] + ' --version')}", flush=True)
            # The node's root, and nobody else, is root@localhost through
            # the socket.
            assert sql(name, 'SELECT CURRENT_USER();') == [['root@localhost']]
            refused = remote(name, f"runuser -u nobody -- /usr/bin/{server['client']} --no-defaults --user=root --execute 'SELECT 1' 2>&1 || true")
            assert 'ERROR 1698' in refused or 'ERROR 1045' in refused, refused
            # From here on every call of the client goes through a wrapper
            # that notes its command line and its environment, and a plain
            # call of the client would follow root's own option file.
            remote(name, f"""mkdir -p {BASE}
chmod 700 {BASE}
install -m 600 /dev/null {CLIENT_CALLS}
for program in {server['client']} my_print_defaults; do
  cat > /usr/local/bin/$program <<WRAPPER
#!/bin/sh
{{ echo "call: $program \\$*"; env; }} >> {CLIENT_CALLS}
exec /usr/bin/$program "\\$@"
WRAPPER
  chmod 755 /usr/local/bin/$program
done
""")
            remote(name, 'cat > /root/.my.cnf', input=DECOY_OPTION_FILE)
        mariadb_version = tuple(int(part) for part in servers['mariadb']['version'].split('-')[0].split('.')[:2])

        # The example: its database, its account, and what the account may do.
        example_results = {r['task'].rsplit('/', 1)[-1]: r for r in applied['entries'][0]['response']['results']}
        created = [r for r in example_results.values() if r.get('value') == {'created': True, 'changed_attributes': []}]
        assert len(created) == 2 and all(r.get('secret') is not True for r in created), example_results
        assert sql('mariadb', "SELECT DEFAULT_CHARACTER_SET_NAME FROM information_schema.SCHEMATA WHERE SCHEMA_NAME = 'shop';") == [['utf8mb4']]
        assert stored('mariadb', 'shop', 'localhost') == ('mysql_native_password', native_stored_value(ACCOUNT_PASSWORD))
        assert login('mariadb', 'shop', ACCOUNT_PASSWORD, 'CREATE TABLE shop.orders (id INT); INSERT INTO shop.orders VALUES (1); SELECT COUNT(*) FROM shop.orders; DROP TABLE shop.orders;') == [['1']]
        denied = login('mariadb', 'shop', ACCOUNT_PASSWORD, 'CREATE DATABASE `mmx-not-allowed`;')
        assert 'ERROR 1044' in denied, denied
        assert 'ERROR 1045' in login('mariadb', 'shop', 'not the password', 'SELECT 1;')
        for command in ('run', 'dryrun'):
            assert run(command, directory=example)['entries'][0]['response']['change'] == 'unchanged'
        # A new password in the environment changes the account's password
        # and nothing else.
        vm.ENV['SHOP_DATABASE_PASSWORD'] = SECOND_ACCOUNT_PASSWORD
        changed = [r for r in run('run', directory=example)['entries'][0]['response']['results'] if r['change'] != 'unchanged']
        assert len(changed) == 1 and changed[0]['value'] == {'created': False, 'changed_attributes': ['password']} and changed[0]['reason'] == 'set the password', changed
        assert login('mariadb', 'shop', SECOND_ACCOUNT_PASSWORD, 'SELECT CURRENT_USER();') == [['shop@localhost']]
        assert 'ERROR 1045' in login('mariadb', 'shop', ACCOUNT_PASSWORD, 'SELECT 1;')
        mark('the example ' + EXAMPLE + ': the server from the distribution\'s packages, the database, and the account with every privilege on its database and no other; '
             'a login with the password works, an unchanged rerun, and a new password changes only the password')

        # The first run of both modules, on both servers at once.
        # MMX puts every name into backticks itself. An on with a backtick is
        # refused when the project loads, so the database that the odd account
        # gets privileges on has none in its name; the account's own name and
        # one more database have.
        odd_database = "mmx 'odd' name; -- \"x\""
        backtick_database = 'mmx `tick` name'
        odd_account = "mmx's `odd`; --"
        first = [
            database,
            {'id': 'second_database', 'mysql_database': {'name': odd_database, 'collation': 'utf8mb4_bin'}},
            {'id': 'archive', 'mysql_database': {'name': 'mmx_archive'}},
            {'id': 'archive_x', 'mysql_database': {'name': 'mmxxarchive'}},
            {'id': 'backtick_database', 'mysql_database': {'name': backtick_database}},
            account,
            {'id': 'administrator', 'mysql_user': {'name': ADMINISTRATOR, 'may_connect_from': '%', 'password': '{{ .secrets.administrator_password }}',
                                                   'privileges': [{'grant_on': '*.*', 'grant': ['ALL', 'GRANT OPTION']}], 'privileges_not_listed': 'remove'}},
            {'id': 'without_password', 'mysql_user': {'name': 'mmx-monitor', 'may_connect_from': 'LocalHost', 'allow_creating_account_without_password': True}},
            {'id': 'odd_account', 'mysql_user': {'name': odd_account, 'may_connect_from': '10.0.%', 'password': 'written in the playbook',
                                                 'privileges': [{'grant_on': odd_database + '.*', 'grant': ['select', 'Create Temporary Tables']},
                                                                {'grant_on': 'mmx_archive.*', 'grant': ['SELECT']}],
                                                 'privileges_not_listed': 'keep'}},
        ]
        project(first)

        def first_dryrun(name, r):
            for task in ('database', 'second_database', 'archive', 'archive_x', 'backtick_database'):
                is_result(r[task], 'succeeded', 'would_change', created=True)
            # A dryrun's reason says what a run would do.
            assert r['database']['reason'] == 'would create the database mmx-shop', r['database']
            assert r['account']['reason'] == 'would create the account mmx-shop@%; would grant CREATE, DELETE, INSERT, SELECT, UPDATE on mmx-shop.*', r['account']
            is_result(r['account'], 'succeeded', 'would_change', created=True)
            is_result(r['administrator'], 'succeeded', 'would_change', created=True, reason='would grant ALL PRIVILEGES on *.*; would grant GRANT OPTION on *.*')
            is_result(r['without_password'], 'succeeded', 'would_change', created=True, reason='would create the account mmx-monitor@localhost without a password')
            assert sql(name, "SELECT COUNT(*) FROM mysql.user WHERE User LIKE 'mmx%';") == [['0']] and sql(name, "SELECT COUNT(*) FROM information_schema.SCHEMATA WHERE SCHEMA_NAME LIKE 'mmx%';") == [['0']]
        each(run('dryrun'), first_dryrun)

        def first_run(name, r):
            server = servers[name]
            for task in ('database', 'second_database', 'archive', 'archive_x', 'backtick_database'):
                is_result(r[task], 'succeeded', 'changed', created=True)
            is_result(r['account'], 'succeeded', 'changed', created=True, reason='created the account mmx-shop@%; granted CREATE, DELETE, INSERT, SELECT, UPDATE on mmx-shop.*')
            is_result(r['without_password'], 'succeeded', 'changed', created=True, reason='created the account mmx-monitor@localhost without a password')
            rows = {row[0]: row[1:] for row in sql(name, "SELECT SCHEMA_NAME, DEFAULT_CHARACTER_SET_NAME, DEFAULT_COLLATION_NAME FROM information_schema.SCHEMATA WHERE SCHEMA_NAME LIKE 'mmx%';")}
            assert set(rows) == {odd_database, backtick_database, 'mmx-shop', 'mmx_archive', 'mmxxarchive'}, rows
            assert rows[odd_database] == ['utf8mb4', 'utf8mb4_bin'] and rows['mmx-shop'][0] == 'utf8mb4', rows
            # Each product's own method for a new account, and the stored
            # value of the password: MariaDB's has no salt, so it is known;
            # MySQL's has MMX's salt of letters and digits and the server's
            # own number of rounds.
            method, value = stored(name, 'mmx-shop', '%')
            if name == 'mariadb':
                assert (method, value) == ('mysql_native_password', native_stored_value(ACCOUNT_PASSWORD)), (method, value)
            else:
                rounds = int(sql(name, 'SELECT @@caching_sha2_password_digest_rounds;')[0][0])
                assert method == 'caching_sha2_password' and len(value) == 70 and value.startswith('$A$%03X$' % (rounds // 1000)) and value[7:27].isalnum() and value.isascii(), (method, value, rounds)
                print(f"MySQL {server['version']}: a new stored value has the server's {rounds} rounds", flush=True)
            server['stored'] = value
            # A login with the clear password proves the stored value.
            assert login(name, 'mmx-shop', ACCOUNT_PASSWORD, 'SELECT CURRENT_USER();', host='127.0.0.1') == [['mmx-shop@%']]
            assert 'ERROR 1045' in login(name, 'mmx-shop', 'not the password', 'SELECT 1;', host='127.0.0.1')
            assert login(name, ADMINISTRATOR, ADMINISTRATOR_PASSWORD, 'SELECT CURRENT_USER();', host='127.0.0.1') == [[ADMINISTRATOR + '@%']]
            assert privileges(name, 'mmx-shop', '%') == on_database('mmx-shop', 'SELECT', 'INSERT', 'UPDATE', 'DELETE', 'CREATE')
            assert ('*.*', 'GRANT OPTION') in privileges(name, ADMINISTRATOR, '%') and ('*.*', 'SHUTDOWN') in privileges(name, ADMINISTRATOR, '%')
            # The host part is stored in lower case, and the account without
            # a password has none.
            assert stored(name, 'mmx-monitor', 'localhost')[1] == '' and privileges(name, 'mmx-monitor', 'localhost') == set()
            # MMX quotes names and escapes literal underscores:
            # the account may read mmx_archive and not mmxxarchive.
            held = sql(name, "SELECT HEX(TABLE_SCHEMA), PRIVILEGE_TYPE FROM information_schema.SCHEMA_PRIVILEGES WHERE HEX(GRANTEE) = '" + f"'{odd_account}'@'10.0.%'".encode().hex().upper() + "' ORDER BY 1, 2;")
            assert held == [[odd_database.encode().hex().upper(), 'CREATE TEMPORARY TABLES'], [odd_database.encode().hex().upper(), 'SELECT'], ['mmx\\_archive'.encode().hex().upper(), 'SELECT']], held
        applied = run('run')
        each(applied, first_run)
        for command in ('run', 'dryrun'):
            assert unchanged(run(command))
        no_private_task_directory_left()
        mark('the first run: databases and accounts with names that need quoting, each product\'s own method and stored value, logins with the clear passwords, '
             'privileges as the catalog shows them, a dryrun before that changes nothing, and an unchanged rerun and dryrun')

        # A wildcard grant is an explicit database-pattern choice.
        project([{'id': 'pattern', 'mysql_user': {'name': odd_account, 'may_connect_from': '10.0.%',
            'privileges': [{'grant_on_databases_matching': 'mmx_archive', 'grant': ['SELECT']}], 'privileges_not_listed': 'remove'}}])
        each(run('run'), lambda name, r: is_result(r['pattern'], 'succeeded', 'changed', created=False, changed_attributes=['privileges']))
        for name in servers:
            held = sql(name, "SELECT TABLE_SCHEMA FROM information_schema.SCHEMA_PRIVILEGES WHERE HEX(GRANTEE) = '" + f"'{odd_account}'@'10.0.%'".encode().hex().upper() + "' ORDER BY 1;")
            assert held == [['mmx_archive']], held
        assert unchanged(run('run'))
        mark('grant_on uses a literal underscore; grant_on_databases_matching keeps the database pattern explicit')

        # An account that the server itself made from the clear password, with
        # the server's own salt: MMX computes the same stored value, so the
        # task finds nothing to change. And a hand-made state that a run
        # finishes: another password, privileges at every kind of place.
        for name in servers:
            escaped = ACCOUNT_PASSWORD.replace('\\', '\\\\').replace("'", "\\'")
            sql(name, f"""CREATE USER `mmx-made-by-the-server`@`%` IDENTIFIED BY '{escaped}';
CREATE USER `mmx-by-hand`@`%` IDENTIFIED BY 'another password';
CREATE TABLE `mmx-shop`.orders (id INT, total INT);
CREATE TABLE `mmx-shop`.audit (id INT, who INT);
CREATE PROCEDURE `mmx-shop`.count_orders() SELECT COUNT(*) FROM `mmx-shop`.orders;
CREATE FUNCTION `mmx-shop`.one() RETURNS INT DETERMINISTIC RETURN 1;
GRANT PROCESS ON *.* TO `mmx-by-hand`@`%`;
GRANT SELECT, DROP ON `mmx-shop`.* TO `mmx-by-hand`@`%` WITH GRANT OPTION;
GRANT SELECT ON `mmxxarchive`.* TO `mmx-by-hand`@`%`;
GRANT INSERT ON `mmx-shop`.orders TO `mmx-by-hand`@`%`;
GRANT SELECT (id), UPDATE (who) ON `mmx-shop`.audit TO `mmx-by-hand`@`%` WITH GRANT OPTION;
GRANT EXECUTE, ALTER ROUTINE ON PROCEDURE `mmx-shop`.count_orders TO `mmx-by-hand`@`%` WITH GRANT OPTION;
GRANT EXECUTE ON FUNCTION `mmx-shop`.one TO `mmx-by-hand`@`%`;
""")
            servers[name]['made_by_the_server'] = stored(name, 'mmx-made-by-the-server', '%')
        by_hand = {'name': 'mmx-by-hand', 'may_connect_from': '%', 'password': '{{ .secrets.account_password }}',
                   'privileges': [{'grant_on': 'mmx-shop.*', 'grant': ['SELECT', 'INSERT']}, {'grant_on': 'mmx-shop.orders', 'grant': ['SELECT', 'GRANT OPTION']}]}
        project([{'id': 'made_by_the_server', 'mysql_user': {'name': 'mmx-made-by-the-server', 'may_connect_from': '%', 'password': '{{ .secrets.account_password }}'}},
                 {'id': 'by_hand', 'mysql_user': {**by_hand, 'privileges_not_listed': 'keep'}}])

        def keep(name, r, dryrun=False):
            is_result(r['made_by_the_server'], 'succeeded', 'unchanged', created=False)
            assert stored(name, 'mmx-made-by-the-server', '%') == servers[name]['made_by_the_server']
            is_result(r['by_hand'], 'succeeded', 'would_change' if dryrun else 'changed', created=False, changed_attributes=['password', 'privileges'],
                      reason=('would set the password; would grant INSERT on mmx-shop.*; would grant SELECT on mmx-shop.orders; would grant GRANT OPTION on mmx-shop.orders' if dryrun
                              else 'set the password; granted INSERT on mmx-shop.*; granted SELECT on mmx-shop.orders; granted GRANT OPTION on mmx-shop.orders'))
        hand_made = {name: privileges(name, 'mmx-by-hand', '%') for name in servers}
        each(run('dryrun'), lambda name, r: keep(name, r, dryrun=True))
        assert {name: privileges(name, 'mmx-by-hand', '%') for name in servers} == hand_made
        each(run('run'), keep)
        for name in servers:
            # keep only adds; the account keeps its method and logs in with
            # the task's password.
            assert privileges(name, 'mmx-by-hand', '%') == hand_made[name] | {('mmx-shop.*', 'INSERT'), ('mmx-shop.orders', 'SELECT'), ('mmx-shop.orders', 'GRANT OPTION')}
            assert stored(name, 'mmx-by-hand', '%')[0] == servers[name]['made_by_the_server'][0]
            assert login(name, 'mmx-by-hand', ACCOUNT_PASSWORD, 'SELECT CURRENT_USER();', host='127.0.0.1') == [['mmx-by-hand@%']]
        for command in ('run', 'dryrun'):
            assert unchanged(run(command))
        # remove: the account holds exactly the list afterwards, also at the
        # places that on cannot name.
        project([{'id': 'by_hand', 'mysql_user': {**by_hand, 'privileges_not_listed': 'remove'}}])
        each(run('dryrun'), lambda name, r: is_result(r['by_hand'], 'succeeded', 'would_change', created=False, changed_attributes=['privileges'],
                                                      reason='would revoke PROCESS on *.*; would revoke GRANT OPTION on mmx-shop.*; would revoke DROP on mmx-shop.*; would revoke SELECT on mmxxarchive.*; '
                                                             'would revoke GRANT OPTION on mmx-shop.audit; would revoke INSERT on mmx-shop.orders; would revoke SELECT on column id of mmx-shop.audit; '
                                                             'would revoke UPDATE on column who of mmx-shop.audit; would revoke GRANT OPTION on procedure mmx-shop.count_orders; '
                                                             'would revoke ALTER ROUTINE, EXECUTE on procedure mmx-shop.count_orders; would revoke EXECUTE on function mmx-shop.one'))
        each(run('run'), lambda name, r: is_result(r['by_hand'], 'succeeded', 'changed', created=False, changed_attributes=['privileges']))
        for name in servers:
            assert privileges(name, 'mmx-by-hand', '%') == on_database('mmx-shop', 'SELECT', 'INSERT') | {('mmx-shop.orders', 'SELECT'), ('mmx-shop.orders', 'GRANT OPTION')}, privileges(name, 'mmx-by-hand', '%')
        for command in ('run', 'dryrun'):
            assert unchanged(run(command))
        # ALL on a database and on a table, and then no privilege at all:
        # every privilege that each server has for the two kinds of place is
        # granted by the server's own number and revoked by its name.
        project([{'id': 'by_hand', 'mysql_user': {'name': 'mmx-by-hand', 'may_connect_from': '%', 'privileges_not_listed': 'remove',
                                                  'privileges': [{'grant_on': 'mmx-shop.*', 'grant': ['ALL']}, {'grant_on': 'mmx-shop.orders', 'grant': ['all privileges']}]}}])
        each(run('run'), lambda name, r: is_result(r['by_hand'], 'succeeded', 'changed', created=False, changed_attributes=['privileges'], reason='granted ALL PRIVILEGES on mmx-shop.*; granted ALL PRIVILEGES on mmx-shop.orders'))
        for name in servers:
            everything = sql(name, "SELECT COUNT(*) FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = 'mysql' AND TABLE_NAME = 'db' AND COLUMN_NAME LIKE '%!_priv' ESCAPE '!' AND COLUMN_NAME <> 'Grant_priv';")
            held = privileges(name, 'mmx-by-hand', '%')
            assert len([place for place, _ in held if place == 'mmx-shop.*']) == int(everything[0][0]) and len([place for place, _ in held if place == 'mmx-shop.orders']) >= 12, held
            assert not any(privilege == 'GRANT OPTION' for _, privilege in held), held
        for command in ('run', 'dryrun'):
            assert unchanged(run(command))
        project([{'id': 'by_hand', 'mysql_user': {'name': 'mmx-by-hand', 'may_connect_from': '%', 'privileges': [], 'privileges_not_listed': 'remove'}}])
        each(run('run'), lambda name, r: is_result(r['by_hand'], 'succeeded', 'changed', created=False, changed_attributes=['privileges']))
        assert all(privileges(name, 'mmx-by-hand', '%') == set() for name in servers)
        mark('an account that the server made from the clear password is unchanged, so MMX computes the server\'s own stored value; '
             'privileges_not_listed: keep only adds and remove also takes privileges on another database, a table, columns, a procedure, and a function, and each grant option; '
             'ALL by the server\'s own number; an empty list with remove leaves nothing')

        # A task that would create an account and gives no password needs
        # the written permission: without it the task fails, in a dryrun and
        # in a run, is not tried again, and creates nothing.
        project([{'id': 'forgotten', 'mysql_user': {'name': 'mmx-forgotten-password', 'may_connect_from': '%',
                                                    'privileges': [{'grant_on': 'mmx-shop.*', 'grant': ['SELECT']}], 'privileges_not_listed': 'keep'}}])
        for command in ('dryrun', 'run'):
            each(run(command, 1), lambda name, r: is_result(r['forgotten'], 'failed', 'unchanged', reason='mysql_user: creating account mmx-forgotten-password@% without a password lets any machine in may_connect_from log in with all its privileges.')
                 and expect('allow_creating_account_without_password: true' in r['forgotten']['reason'] and r['forgotten']['failure_kind'] == 'config' and tries(r['forgotten']) == 1, r['forgotten']))
        assert all(stored(name, 'mmx-forgotten-password', '%') is None for name in servers)
        # For an account that exists, a task without password changes nothing.
        project([{'id': 'exists', 'mysql_user': {'name': 'mmx-shop', 'may_connect_from': '%'}}])
        assert unchanged(run('run'))

        # What a run cannot do, and what a dryrun cannot know.
        project([{'id': 'differs', 'mysql_database': {'name': 'mmx-shop', 'character_set': 'latin1'}}])
        for command in ('dryrun', 'run'):
            each(run(command, 1), lambda name, r: is_result(r['differs'], 'failed', 'unchanged', reason='mysql_database: the database mmx-shop exists with the character set utf8mb4 and the collation ')
                 and expect(r['differs']['failure_kind'] == 'config', r))
        assert all(sql(name, "SELECT DEFAULT_CHARACTER_SET_NAME FROM information_schema.SCHEMATA WHERE SCHEMA_NAME = 'mmx-shop';") == [['utf8mb4']] for name in servers)
        # A character set that the server does not have, and a collation of
        # another character set: the server's catalog says so before anything
        # changes, in a dryrun too, and the task is not tried again.
        project([{'id': 'unknown', 'mysql_database': {'name': 'mmx-unknown', 'character_set': 'nosuch'}}])
        for command in ('dryrun', 'run'):
            each(run(command, 1), lambda name, r: is_result(r['unknown'], 'failed', 'unchanged')
                 and expect(r['unknown']['reason'] == 'mysql_database.character_set: the database server on this node has no character set nosuch; '
                            'its statement SHOW CHARACTER SET lists the names, such as utf8mb4' and r['unknown']['failure_kind'] == 'config' and tries(r['unknown']) == 1, r['unknown']))
        project([{'id': 'unknown', 'mysql_database': {'name': 'mmx-unknown', 'character_set': 'latin1', 'collation': 'utf8mb4_unicode_ci'}}])
        each(run('run', 1), lambda name, r: is_result(r['unknown'], 'failed', 'unchanged')
             and expect(r['unknown']['reason'] == "mysql_database.collation: on the database server on this node, utf8mb4_unicode_ci is no collation of the character set latin1; "
                        "its statement SHOW COLLATION WHERE Charset = 'latin1' lists the collations of that character set" and r['unknown']['failure_kind'] == 'config', r['unknown']))
        # MariaDB 10.11 and later take the short name of a collation and
        # store the name with the character set, so the second run must take
        # the two for the same. A server without the name fails the task: an
        # older MariaDB, and MySQL. Each server's own catalog says which.
        short = {name: sql(name, "SELECT COUNT(*) FROM information_schema.COLLATION_CHARACTER_SET_APPLICABILITY WHERE COLLATION_NAME = 'uca1400_ai_ci';") != [['0']] for name in servers}
        assert short['mariadb'] == (mariadb_version >= (10, 11)) and not short.get('mysql'), short
        project([{'id': 'short', 'mysql_database': {'name': 'mmx-short-collation', 'character_set': 'utf8mb4', 'collation': 'uca1400_ai_ci'}}])
        some_fail = None if all(short[name] for name in servers) else 1

        def short_collation(name, r, change):
            if short[name]:
                return is_result(r['short'], 'succeeded', change, created=change != 'unchanged')
            is_result(r['short'], 'failed', 'unchanged')
            assert r['short']['reason'] == ('mysql_database.collation: the database server on this node has no collation uca1400_ai_ci; '
                                            'its statement SHOW COLLATION lists the names, such as utf8mb4_unicode_ci') and r['short']['failure_kind'] == 'config', r['short']
        each(run('dryrun', some_fail), lambda name, r: short_collation(name, r, 'would_change'))
        each(run('run', some_fail), lambda name, r: short_collation(name, r, 'changed'))
        for command in ('run', 'dryrun'):
            each(run(command, some_fail), lambda name, r: short_collation(name, r, 'unchanged'))
        for name in servers:
            rows = sql(name, "SELECT SCHEMA_NAME, DEFAULT_CHARACTER_SET_NAME, DEFAULT_COLLATION_NAME FROM information_schema.SCHEMATA WHERE SCHEMA_NAME IN ('mmx-unknown', 'mmx-short-collation');")
            assert rows == ([['mmx-short-collation', 'utf8mb4', 'utf8mb4_uca1400_ai_ci']] if short[name] else []), rows
            print(f"{servers[name]['product']} {servers[name]['version']}: " + ('the collation uca1400_ai_ci is stored as utf8mb4_uca1400_ai_ci, and the second run is unchanged' if short[name]
                                                                                 else 'has no collation uca1400_ai_ci, and the task fails before anything changes'), flush=True)
        # root logs in through the socket with another method than a
        # password's, so a password for it is refused with the method's name.
        project([{'id': 'root', 'mysql_user': {'name': 'root', 'may_connect_from': 'localhost', 'password': '{{ .secrets.account_password }}'}}])
        for command in ('dryrun', 'run'):
            def root_is_refused(name, r):
                methods = 'logs in with one of several authentication methods (mysql_native_password or unix_socket)' if name == 'mariadb' else 'logs in with the authentication method auth_socket'
                is_result(r['root'], 'failed', 'unchanged', reason='mysql_user: the account root@localhost ' + methods)
                assert r['root']['failure_kind'] == 'config', r['root']
            each(run(command, 1), root_is_refused)
        assert all(sql(name, 'SELECT CURRENT_USER();') == [['root@localhost']] for name in servers)
        # A table that does not exist yet, and a privilege that the server
        # refuses after the account was created: the next run finishes.
        project([{'id': 'later', 'mysql_user': {'name': 'mmx-later', 'may_connect_from': '%', 'password': '{{ .secrets.account_password }}',
                                                'privileges': [{'grant_on': 'mmx-shop.not_there', 'grant': ['SELECT']}], 'privileges_not_listed': 'keep'}}])
        each(run('dryrun'), lambda name, r: is_result(r['later'], 'not_checked', 'unknown', reason='mysql_user: the table mmx-shop.not_there does not exist'))
        each(run('run', 1), lambda name, r: expect(r['later']['failure_kind'] == 'missing_prerequisite', r))
        assert all(stored(name, 'mmx-later', '%') is None for name in servers)
        later = {'name': 'mmx-later', 'may_connect_from': '%', 'password': '{{ .secrets.account_password }}', 'privileges_not_listed': 'keep'}
        project([{'id': 'later', 'mysql_user': {**later, 'privileges': [{'grant_on': 'mmx-shop.*', 'grant': ['SELECT', 'NO SUCH PRIVILEGE']}]}}])

        def half_done(name, r):
            # The account is there and the result says so; the server's own
            # message is in the reason.
            is_result(r['later'], 'failed', 'changed', created=True, reason='mysql_user: changing the privileges of mmx-later@% on the database server on this node: ')
            assert r['later']['failure_kind'] == 'config' and 'ERROR ' in r['later']['reason'] and "the server refuses a privilege's name" in r['later']['reason'], r['later']
            assert stored(name, 'mmx-later', '%') is not None and privileges(name, 'mmx-later', '%') == set()
        each(run('run', 1), half_done)
        project([{'id': 'later', 'mysql_user': {**later, 'privileges': [{'grant_on': 'mmx-shop.*', 'grant': ['SELECT']}]}}])
        each(run('dryrun'), lambda name, r: is_result(r['later'], 'succeeded', 'would_change', created=False, changed_attributes=['privileges']))
        each(run('run'), lambda name, r: is_result(r['later'], 'succeeded', 'changed', created=False, changed_attributes=['privileges'], reason='granted SELECT on mmx-shop.*'))
        assert all(privileges(name, 'mmx-later', '%') == on_database('mmx-shop', 'SELECT') for name in servers)
        # The statement that carries the stored value fails: the user name is
        # longer than any server takes. The server's message shows, the
        # stored value does not.
        too_long = 'mmx-' + 'u' * 140
        project([{'id': 'too_long', 'mysql_user': {'name': too_long, 'may_connect_from': '%', 'password': '{{ .secrets.account_password }}'}}])

        def name_is_too_long(name, r):
            is_result(r['too_long'], 'failed', 'unchanged', reason='ERROR 1470 ')
            assert 'is too long for user name' in r['too_long']['reason'] and native_stored_value(ACCOUNT_PASSWORD) not in json.dumps(r) and '$A$' not in json.dumps(r), r['too_long']
            if name == 'mariadb' and mariadb_version >= (10, 6):
                # MariaDB's client prints the statement that failed.
                assert "IDENTIFIED VIA mysql_native_password USING '[redacted]'" in r['too_long']['reason'], r['too_long']
        each(run('run', 1), name_is_too_long)
        mark('failures: a new account without a password needs allow_creating_account_without_password; a database whose character set differs is not changed; '
             'a character set that the server does not have and a collation of another character set fail before anything changes; MariaDB\'s short collation name; '
             'a password for root names root\'s other method; a missing table leaves a dryrun not checked; '
             'a privilege that the server refuses after the account was created fails with the server\'s message and the next run finishes; '
             'a failing statement with the stored value shows the server\'s message without the value')

        # A MariaDB server with a password check plugin refuses every stored
        # value while strict_password_validation is on.
        plugin = sql('mariadb', "INSTALL SONAME 'simple_password_check';", check=False)
        if plugin == []:
            project([only_on('mariadb', {'id': 'checked', 'mysql_user': {'name': 'mmx-checked', 'may_connect_from': '%', 'password': '{{ .secrets.account_password }}'}})])
            for command in ('dryrun', 'run'):
                refused = results(run(command, 1), 'mariadb')['checked']
                assert is_result(refused, 'failed', 'unchanged', reason='its setting strict_password_validation is on, so it refuses every stored password value') and refused['failure_kind'] == 'config', refused
            assert stored('mariadb', 'mmx-checked', '%') is None
            sql('mariadb', "UNINSTALL SONAME 'simple_password_check';")
            mark('MariaDB with a password check plugin: a task with a password fails and names strict_password_validation, in a dryrun and in a run')
        else:
            print('MariaDB here has no plugin simple_password_check, so the check of strict_password_validation is left out: ' + str(plugin), flush=True)

        # Connection fields: a login over TCP with a password, to the node's
        # own server. The password goes through MMX's login file.
        tcp = {'dangerously_skip_server_certificate_check': True, 'database_server_host': '127.0.0.1', 'login_user': ADMINISTRATOR, 'login_password': '{{ .secrets.administrator_password }}'}
        over_tcp = [{'id': 'database', 'mysql_database': {'name': 'mmx-over-tcp', **tcp}},
                    {'id': 'account', 'mysql_user': {'name': 'mmx-over-tcp', 'may_connect_from': '%', 'password': '{{ .secrets.account_password }}',
                                                     'privileges': [{'grant_on': 'mmx-over-tcp.*', 'grant': ['ALL']}], 'privileges_not_listed': 'remove', **tcp}}]
        project(over_tcp)
        each(run('dryrun'), lambda name, r: is_result(r['database'], 'succeeded', 'would_change', created=True) and is_result(r['account'], 'succeeded', 'would_change', created=True))
        each(run('run'), lambda name, r: is_result(r['database'], 'succeeded', 'changed', created=True) and is_result(r['account'], 'succeeded', 'changed', created=True))
        for name in servers:
            assert login(name, 'mmx-over-tcp', ACCOUNT_PASSWORD, 'CREATE TABLE `mmx-over-tcp`.t (id INT); SELECT COUNT(*) FROM `mmx-over-tcp`.t;', host='127.0.0.1') == [['0']]
        for command in ('run', 'dryrun'):
            assert unchanged(run(command))
        # The same login through the socket, without a host.
        project([{'id': 'database', 'mysql_database': {'name': 'mmx-over-tcp', 'login_user': ADMINISTRATOR, 'login_password': '{{ .secrets.administrator_password }}'}}])
        assert unchanged(run('run'))
        # A wrong password is a failure in a dryrun too, with the server's
        # message, and it has one try: another try gets the same answer, and
        # a server may lock an account after a few wrong logins. A port
        # where nothing listens leaves a dryrun not checked, and a run tries
        # again, because a server that starts answers a moment later.
        vm.ENV['MMX_TEST_ADMINISTRATOR_PASSWORD'] = 'not the password'
        project(over_tcp)
        for command in ('dryrun', 'run'):
            each(run(command, 1), lambda name, r: is_result(r['database'], 'failed', 'unchanged')
                 and expect(r['database']['reason'].startswith('mysql_database: the database server at 127.0.0.1, port 3306 refused the login: ERROR 1045 ')
                            and r['database']['failure_kind'] == 'config' and tries(r['database']) == 1, r['database']))
        vm.ENV['MMX_TEST_ADMINISTRATOR_PASSWORD'] = ADMINISTRATOR_PASSWORD
        project([{'id': 'database', 'mysql_database': {'name': 'mmx-over-tcp', **tcp, 'database_server_port': 3999}}])
        each(run('dryrun'), lambda name, r: is_result(r['database'], 'not_checked', 'unknown', reason='mysql_database: the database server at 127.0.0.1, port 3999 does not answer: ERROR 200'))
        each(run('run', 1), lambda name, r: is_result(r['database'], 'failed', 'unchanged', reason='does not answer: ERROR 200')
             and expect(tries(r['database']) == 3 and 'failure_kind' not in r['database'], r['database']))
        no_private_task_directory_left()
        mark('connection fields: a login over TCP and through the socket with a password that needs quoting in the login file; a wrong password fails also in a dryrun '
             'and has one try; a port where nothing listens leaves a dryrun not checked, and a run tries three times')

        # The server's certificate. The server is the other product's where
        # there is one, so that MariaDB's client meets a MySQL server. Each
        # client reaches it at the server node's own address, never at
        # 127.0.0.1: MariaDB's client (measured on 11.8) does not check the
        # address in the certificate for a connection to 127.0.0.1.
        tls_server = 'mysql' if 'mysql' in servers else 'mariadb'
        tls_address = servers[tls_server]['node']['address']
        # Ubuntu's AppArmor profile of mysqld lets the server read /etc/mysql
        # and no other directory under /etc; without its certificate files,
        # MySQL starts with TLS off.
        tls_directory = '/etc/mysql/mmx-test-database-tls' if apt else '/etc/mmx-test-database-tls'
        moved_socket = ('/run/mysqld' if apt else '/var/lib/mysql') + '/mmx-moved.sock'
        remote(tls_server, f"""mkdir -p {tls_directory}
cd {tls_directory}
for authority in authority other-authority; do
  openssl req -x509 -newkey rsa:2048 -nodes -keyout $authority-key.pem -out $authority.pem -days 2 -subj "/CN=MMX test $authority" 2>/dev/null
done
openssl req -newkey rsa:2048 -nodes -keyout server-key.pem -out server.csr -subj "/CN=mmx-test-database-server" 2>/dev/null
printf 'subjectAltName=IP:{tls_address}\\n' > names.cnf
openssl x509 -req -in server.csr -CA authority.pem -CAkey authority-key.pem -CAcreateserial -out server-certificate.pem -days 2 -extfile names.cnf 2>/dev/null
chown mysql server-key.pem server-certificate.pem authority.pem
chmod 600 server-key.pem
""")
        certificates = {file: remote(tls_server, f'cat {tls_directory}/{file}.pem') + '\n' for file in ('authority', 'other-authority')}
        # The test's settings of the servers: the certificate, every address
        # (Debian and Ubuntu listen on 127.0.0.1 only), and, on the MariaDB
        # server, a socket at another place than the client was built with.
        tls_settings = (f'ssl_ca={tls_directory}/authority.pem\nssl_cert={tls_directory}/server-certificate.pem\n'
                        f'ssl_key={tls_directory}/server-key.pem\nbind-address=0.0.0.0\n')
        for name, server in servers.items():
            settings = '[mysqld]\n' + (tls_settings if name == tls_server else '') + (f'socket={moved_socket}\n' if name == 'mariadb' else '')
            if settings != '[mysqld]\n':
                remote(name, f"cat > {configuration_directory[name]}/zz-mmx-test.cnf\nsystemctl restart {server['unit']}", input=settings)
        # The MariaDB server's socket moved: the client's own path is wrong
        # now, and the tasks find the socket through the server's
        # configuration.
        assert 'ERROR 2002' in remote('mariadb', '/usr/bin/mariadb --no-defaults --execute "SELECT 1" 2>&1 || true')
        servers['mariadb']['socket_option'] = ' --socket=' + moved_socket
        project([only_on('mariadb', {'id': 'database', 'mysql_database': {'name': 'mmx-through-the-moved-socket'}}),
                 only_on('mariadb', {'id': 'with_login', 'mysql_database': {'name': 'mmx-over-tcp', 'login_user': ADMINISTRATOR, 'login_password': '{{ .secrets.administrator_password }}'}})])
        moved = results(run('run'), 'mariadb')
        assert is_result(moved['database'], 'succeeded', 'changed', created=True) and is_result(moved['with_login'], 'succeeded', 'unchanged', created=False), moved
        assert sql('mariadb', "SELECT COUNT(*) FROM information_schema.SCHEMATA WHERE SCHEMA_NAME = 'mmx-through-the-moved-socket';") == [['1']]
        mark('a server whose configuration moved its socket: the tasks find it through my_print_defaults, with and without a login')

        # From each node, through the node's own client, to the server with
        # the certificate: MariaDB's client from the MariaDB node and, where
        # there is a MySQL server, MySQL's client from the MySQL node, which
        # logs in to its own server through the address in the certificate.
        # Each node's task makes a database of its own.
        with_tls = {'database_server_host': tls_address, 'login_user': ADMINISTRATOR, 'login_password': '{{ .secrets.administrator_password }}'}
        authority_files = [{'file': {'path': f'{BASE}/{file}.pem', 'permissions_mode': '0644', 'content': text}} for file, text in certificates.items()]
        clients = ' and '.join(f"{servers[name]['product']}'s client" for name in servers)

        def tls_task(authority=None):
            fields = dict(with_tls)
            if authority:
                fields['database_server_certificate_authority_file'] = f'{BASE}/{authority}.pem'
            return {'id': 'database', 'mysql_database': {'name': 'mmx-checked-from-{{ .node.name }}', **fields}}

        def refused_by_the_client(name, r):
            refused = r['database']
            assert is_result(refused, 'failed', 'unchanged') and refused['reason'].startswith('mysql_database: the client could not establish a TLS connection to the database server at ' + tls_address), refused
            # Another try does not make the client accept the certificate.
            assert 'ERROR 2026' in refused['reason'] and "For a private authority or a self-signed certificate, give database_server_certificate_authority_file. dangerously_skip_server_certificate_check: true turns off the check of the server's identity." in refused['reason'], refused
            assert refused['failure_kind'] == 'config' and tries(refused) == 1, refused
        project(authority_files + [tls_task('authority')])
        each(run('run'), lambda name, r: is_result(r['database'], 'succeeded', 'changed', created=True))
        checked = sql(tls_server, "SELECT SCHEMA_NAME FROM information_schema.SCHEMATA WHERE SCHEMA_NAME LIKE 'mmx-checked-from-%' ORDER BY 1;")
        assert checked == [['mmx-checked-from-' + name] for name in sorted(servers)], checked
        project(authority_files + [tls_task('other-authority')])
        for command in ('dryrun', 'run'):
            each(run(command, 1), refused_by_the_client)
        # An authority file that is not on the node yet.
        project([tls_task('not-installed')])
        each(run('dryrun'), lambda name, r: is_result(r['database'], 'not_checked', 'unknown', reason=f'database_server_certificate_authority_file: {BASE}/not-installed.pem is not a file on the node'))
        each(run('run', 1), lambda name, r: expect(r['database']['failure_kind'] == 'missing_prerequisite', r['database']))
        # The default checks the server; the explicit override disables it.
        project([tls_task()])
        attempt = vm.cmd([mmx, 'run', 'configure', '--json'], cwd=workspace, check=False)
        printed.append(attempt.stdout + attempt.stderr)
        without_the_field = json.loads(attempt.stdout)
        for name in servers:
            without = results(without_the_field, name)['database']
            if name == 'mysql' or tls_server == 'mysql' or mariadb_version < (11, 4):
                assert is_result(without, 'failed', 'unchanged', reason='ERROR 2026'), without
            else:
                # New MariaDB clients can authenticate a MariaDB certificate using the password.
                assert is_result(without, 'succeeded', 'unchanged'), without
        unchecked = tls_task()
        unchecked['mysql_database']['dangerously_skip_server_certificate_check'] = True
        project([unchecked])
        assert unchanged(run('run'))

        # A certificate that the server's own authority signed for another
        # address, 192.0.2.10 from the range for documentation, which no
        # node has. With the authority file each client also checks the
        # address in the certificate, and refuses the server.
        renewed = remote(tls_server, f"""cd {tls_directory}
printf 'subjectAltName=IP:192.0.2.10\\n' > names.cnf
openssl x509 -req -in server.csr -CA authority.pem -CAkey authority-key.pem -CAcreateserial -out server-certificate.pem -days 2 -extfile names.cnf 2>/dev/null
openssl verify -CAfile authority.pem server-certificate.pem
systemctl restart {servers[tls_server]['unit']}
""")
        assert renewed == 'server-certificate.pem: OK', renewed
        project(authority_files + [tls_task('authority')])
        for command in ('dryrun', 'run'):
            for_another_address = run(command, 1)
            each(for_another_address, refused_by_the_client)
        for name in servers:
            print(f"{servers[name]['product']}'s client {servers[name]['version']} to a certificate of the right authority for another address: "
                  + results(for_another_address, name)['database']['reason'], flush=True)
        mark(f"database_server_certificate_authority_file, from each node through {clients} to the {servers[tls_server]['product']} server: "
             "with the server's authority the login works, with another authority's file the client refuses the server and the task has one try, "
             "a certificate of the server's authority for another address is refused in the same way, a missing file leaves a dryrun not checked, "
             'and without the field each client keeps its own rule')

        # A server that does not answer: stopped.
        for name, server in servers.items():
            remote(name, f"systemctl stop {server['unit']}")
        project([database, account])
        each(run('dryrun'), lambda name, r: is_result(r['database'], 'not_checked', 'unknown', reason='mysql_database: the database server on this node does not answer: ERROR 2002')
             and is_result(r['account'], 'not_checked', 'unknown', reason='mysql_user: the database server on this node does not answer: ERROR 2002'))
        each(run('run', 1), lambda name, r: is_result(r['database'], 'failed', 'unchanged', reason='does not answer: ERROR 2002'))
        for name, server in servers.items():
            remote(name, f"rm -f {configuration_directory[name]}/zz-mmx-test.cnf\nsystemctl start {server['unit']}")
        servers['mariadb'].pop('socket_option')
        assert unchanged(run('run'))
        mark('a stopped server: a dryrun is not checked and a run fails with the client\'s message; after the start the tasks are unchanged')

        # In the changes of a node task block: a database task is no tracked
        # input, so the block's command follows the file and not the task.
        def block(database_name):
            return [{'task_block': {'id': 'application', 'changes': [
                {'id': 'database', 'mysql_database': {'name': database_name}},
                {'id': 'settings', 'file': {'path': f'{BASE}/application.conf', 'permissions_mode': '0644', 'content': 'database=mmx-in-a-block\n'}}],
                'activate': [{'id': 'reload', 'shell': {'script': f'echo reloaded >> {BASE}/reloads', 'only_when_tracked_inputs_changed': True}}]}}]
        project(block('mmx-in-a-block'))
        each(run('run'), lambda name, r: is_result(r['database'], 'succeeded', 'changed', created=True) and expect(r['reload']['change'] == 'changed', r))
        project(block('mmx-in-a-block-2'))
        each(run('run'), lambda name, r: is_result(r['database'], 'succeeded', 'changed', created=True) and expect(r['reload']['change'] == 'unchanged', r))
        assert all(remote(name, f'cat {BASE}/reloads') == 'reloaded' for name in servers)
        mark('in the changes of a node task block a database task runs and is no tracked input: the block\'s command follows the file beside it')

        # The passwords and their stored values, in everything of this suite.
        saved = ''.join(path.read_text(errors='replace') for directory in vm.ROOT.glob('mysql-databases*') for path in (directory / '.mmx').rglob('*') if path.is_file())
        for name, server in servers.items():
            value = server['stored']
            # The stored value whole, and for MySQL's form its salt and its
            # digest alone; MariaDB's also without its asterisk.
            forms = [ACCOUNT_PASSWORD, SECOND_ACCOUNT_PASSWORD, ADMINISTRATOR_PASSWORD.strip(), ADMINISTRATOR_PASSWORD.strip().replace('\\', '\\\\').replace('"', '\\"'),
                     value, native_stored_value(ACCOUNT_PASSWORD)[1:], native_stored_value(SECOND_ACCOUNT_PASSWORD)[1:], native_stored_value(ADMINISTRATOR_PASSWORD)[1:]]
            if name == 'mysql':
                forms += [value[7:27], value[27:]]
            for form in forms:
                for where, text in (('what mmx printed', ''.join(printed)), ('the saved runs', saved)):
                    escaped = json.dumps(form)[1:-1]
                    assert form not in text and escaped not in text, f"{server['product']}: a password or its stored value is in {where}: ...{text[max(0, text.find(form) - 300):text.find(form) + 60]}"
            remote(name, f'cat > {BASE}/patterns', input='\n'.join(forms) + '\n')
            # The node's job records, MMX's directories under /run, the home
            # directories, the journal, and every call of the client.
            found = remote(name, f'grep -rlF -f {BASE}/patterns /var/lib/mmx /run/mmx /root /home /tmp 2>/dev/null || true\n'
                                 f'journalctl --no-pager -q 2>/dev/null | grep -F -f {BASE}/patterns || true\n'
                                 f'grep -F -f {BASE}/patterns {CLIENT_CALLS} || true')
            assert found == '', f"{server['product']}: a password or its stored value is on the node: {found[:600]}"
            # The wrapper saw MMX's calls of the client: each with nothing
            # but PATH in its environment and with no option file of the
            # node. The shell of the wrapper adds PWD, SHLVL, and _ itself.
            # The server's own unit also runs my_print_defaults, on Ubuntu,
            # with the unit's environment; those calls are not MMX's.
            calls = [record.split('\n') for record in remote(name, f'cat {CLIENT_CALLS}').split('call: ')[1:]]

            def only_path(record):
                return {line.split('=', 1)[0] for line in record[1:] if '=' in line} <= {'PATH', 'PWD', 'SHLVL', '_', 'OLDPWD'} and 'PATH=/usr/sbin:/usr/bin:/sbin:/bin' in record
            of_the_client = [record for record in calls if record[0].startswith(server['client'] + ' ')]
            assert len(of_the_client) > 50, len(of_the_client)
            for record in of_the_client:
                assert record[0].startswith(f"{server['client']} --no-defaults ") or record[0].startswith(f"{server['client']} --defaults-file=/run/mmx/private_task_directories/mysql_"), record[0]
                assert ' --binary-mode' in record[0] and ' --batch' in record[0] and only_path(record), record
            groups = 'my_print_defaults mysqld server' + (' mariadb mariadbd client-server' if name == 'mariadb' else '')
            asked_for_the_socket = [record for record in calls if record[0] == groups and only_path(record)]
            assert len(asked_for_the_socket) > 50, len(asked_for_the_socket)
        no_private_task_directory_left()
        mark('the passwords, plain and as each server stores them, whole and in part, are in nothing that mmx printed or saved, in no record, journal, or home directory of the node, '
             'and on no command line or environment of the client; every call of the client had an empty environment and read no option file of the node')

        # The administrator holds every privilege of the server with the
        # grant option, among them MySQL's dynamic ones, each of which
        # carries its own option. First only the option goes: one plain
        # statement, and the account keeps every privilege. After that
        # statement MySQL's catalog keeps showing the dynamic privileges
        # with the option; MMX reads them from the grant table, so the next
        # run and dryrun find nothing to do.
        def administrator(name, statements):
            """Run statements as the administrator account, over TCP."""
            return login(name, ADMINISTRATOR, ADMINISTRATOR_PASSWORD, statements, host='127.0.0.1')

        def dynamic_privileges(name):
            """MySQL's grant table: how many dynamic privileges the administrator holds, and how many with the option."""
            held, with_option = sql(name, f"SELECT COUNT(*), IFNULL(SUM(WITH_GRANT_OPTION = 'Y'), 0) FROM mysql.global_grants WHERE USER = '{ADMINISTRATOR}';")[0]
            return int(held), int(with_option)
        everything = {name: privileges(name, ADMINISTRATOR, '%') for name in servers}
        before = dynamic_privileges('mysql') if 'mysql' in servers else None
        assert before is None or (before[0] > 30 and before[0] == before[1]), before
        project([{'id': 'administrator', 'mysql_user': {'name': ADMINISTRATOR, 'may_connect_from': '%', 'privileges': [{'grant_on': '*.*', 'grant': ['ALL']}], 'privileges_not_listed': 'remove'}}])
        each(run('run'), lambda name, r: is_result(r['administrator'], 'succeeded', 'changed', created=False, changed_attributes=['privileges'])
             and expect(r['administrator']['reason'] == 'revoked GRANT OPTION on *.*', r['administrator']))
        for command in ('run', 'dryrun'):
            assert unchanged(run(command))
        for name in servers:
            # The account still uses its privileges and can pass none on.
            assert administrator(name, "SELECT COUNT(*) FROM mysql.user WHERE User = 'root' AND Host = 'localhost';") == [['1']]
            assert 'ERROR' in administrator(name, 'GRANT PROCESS ON *.* TO `mmx-by-hand`@`%`;') and privileges(name, 'mmx-by-hand', '%') == set()
            if name == 'mysql':
                assert dynamic_privileges(name) == (before[0], 0), dynamic_privileges(name)
                stale = [place for place, privilege in privileges(name, ADMINISTRATOR, '%') if privilege == 'GRANT OPTION']
                print(f"MySQL {servers[name]['version']}: after REVOKE GRANT OPTION ON *.*, its catalog " + ('still shows the option' if stale else 'shows no option any more'), flush=True)
            else:
                assert privileges(name, ADMINISTRATOR, '%') == everything[name] - {('*.*', 'GRANT OPTION')} and len(everything[name]) > 30, privileges(name, ADMINISTRATOR, '%') ^ everything[name]
        # Then every privilege goes, each by the name that the server gives it.
        project([{'id': 'administrator', 'mysql_user': {'name': ADMINISTRATOR, 'may_connect_from': '%', 'privileges': [], 'privileges_not_listed': 'remove'}}])
        each(run('run'), lambda name, r: is_result(r['administrator'], 'succeeded', 'changed', created=False, changed_attributes=['privileges']))
        for command in ('run', 'dryrun'):
            assert unchanged(run(command))
        for name in servers:
            denied = administrator(name, "SELECT COUNT(*) FROM mysql.user WHERE User = 'root';")
            assert 'ERROR 1142' in denied, denied
            if name == 'mysql':
                # The grant table is empty; the catalog is right again once
                # the server has read its tables.
                assert dynamic_privileges(name) == (0, 0), dynamic_privileges(name)
                sql(name, 'FLUSH PRIVILEGES;')
            assert privileges(name, ADMINISTRATOR, '%') == set(), privileges(name, ADMINISTRATOR, '%')

        mark('the grant option of *.* goes with one statement and the account keeps every privilege; every privilege of the server is revoked by its name')

        # A drop must not wait without end for another session. An
        # application's session has an open transaction that read a table of
        # the database, and stays open. The server makes DROP DATABASE wait
        # for it: by its own setting for a day (MariaDB) or a year (MySQL).
        # The task waits 10 seconds, fails with the server's error 1205, an
        # ordinary failure, and has changed nothing.
        def other_sessions(name):
            return sql(name, "SELECT ID FROM information_schema.PROCESSLIST WHERE INFO LIKE 'SELECT SLEEP(300)%';")
        for name, server in servers.items():
            remote(name, f"""printf 'START TRANSACTION;\\nSELECT COUNT(*) FROM `mmx-shop`.orders;\\nSELECT SLEEP(300);\\n' > {BASE}/open-transaction.sql
nohup /usr/bin/{server['client']} --no-defaults{server.get('socket_option', '')} --batch < {BASE}/open-transaction.sql > /dev/null 2>&1 &
""")
        for name in servers:
            for _ in range(50):
                if other_sessions(name):
                    break
                time.sleep(0.1)
            assert len(other_sessions(name)) == 1, other_sessions(name)
        project([{'id': 'database', 'retry': {'max_tries': 1}, 'mysql_database': {'name': 'mmx-shop', 'state': 'deleted_with_all_its_data'}}])
        started = time.monotonic()
        waited = run('run', 1)
        seconds = time.monotonic() - started

        def drop_waited(name, r):
            is_result(r['database'], 'failed', 'unchanged')
            # The reason of a task with its own retry setting starts with the number of tries.
            assert 'mysql_database: DROP DATABASE `mmx-shop` on the database server on this node: ' in r['database']['reason'] and 'ERROR 1205 ' in r['database']['reason'], r['database']
            assert tries(r['database']) == 1, r['database']
            assert r['database']['reason'].endswith('another session of the server holds a lock that the statement needs, such as an open transaction on a table, and MMX waits 10 seconds for it'), r['database']
            assert 'failure_kind' not in r['database'], r['database']
            assert sql(name, "SELECT COUNT(*) FROM information_schema.TABLES WHERE TABLE_SCHEMA = 'mmx-shop';") == [['2']]
        each(waited, drop_waited)
        assert 10 <= seconds < 30, seconds
        print(f'A drop behind an open transaction of another session ended after {seconds:.0f} s with the error 1205', flush=True)
        # The other session ends, and the removal below drops the database.
        for name in servers:
            sql(name, ''.join(f'KILL {row[0]};' for row in other_sessions(name)))
            for _ in range(50):
                if not other_sessions(name):
                    break
                time.sleep(0.1)
            assert other_sessions(name) == [], other_sessions(name)
        mark('a drop of a database does not wait without end for another session\'s open transaction: after 10 seconds it fails with the error 1205, an ordinary failure, and the database is unchanged')

        # Removal.
        gone = [{'id': 'database', 'mysql_database': {'name': 'mmx-shop', 'state': 'deleted_with_all_its_data'}},
                {'id': 'account', 'mysql_user': {'name': 'mmx-shop', 'may_connect_from': '%', 'state': 'absent'}},
                {'id': 'odd_account', 'mysql_user': {'name': odd_account, 'may_connect_from': '10.0.%', 'state': 'absent'}},
                {'id': 'never_there', 'mysql_user': {'name': 'mmx-never-there', 'may_connect_from': '%', 'state': 'absent'}},
                {'id': 'no_database', 'mysql_database': {'name': 'mmx-never-there', 'state': 'deleted_with_all_its_data'}}]
        project(gone)

        def removal(name, r, dryrun=False):
            change = 'would_change' if dryrun else 'changed'
            for task in ('database', 'account', 'odd_account'):
                is_result(r[task], 'succeeded', change, created=False)
            for task in ('never_there', 'no_database'):
                is_result(r[task], 'succeeded', 'unchanged', created=False)
            # A dryrun says what a run would remove, and a run what it removed.
            reasons = (('would drop the database mmx-shop with all its data', 'would remove the account mmx-shop@%') if dryrun
                       else ('dropped the database mmx-shop with all its data', 'removed the account mmx-shop@%'))
            assert (r['database']['reason'], r['account']['reason']) == reasons, (r['database'], r['account'])
            there = sql(name, "SELECT COUNT(*) FROM information_schema.TABLES WHERE TABLE_SCHEMA = 'mmx-shop';") + sql(name, "SELECT COUNT(*) FROM mysql.user WHERE User = 'mmx-shop';")
            # The database had two tables with it.
            assert there == ([['2'], ['1']] if dryrun else [['0'], ['0']]), there
        each(run('dryrun'), lambda name, r: removal(name, r, dryrun=True))
        each(run('run'), removal)
        for command in ('run', 'dryrun'):
            assert unchanged(run(command))
        mark('removal: an account is removed, a database is dropped with its tables, a dryrun before changes nothing and says what a run would remove, '
             'and what is missing is unchanged')
    finally:
        failing = sys.exc_info()[0] is not None
        for variable in ('MMX_TEST_ACCOUNT_PASSWORD', 'MMX_TEST_ADMINISTRATOR_PASSWORD', 'SHOP_DATABASE_PASSWORD'):
            vm.ENV.pop(variable, None)
        cleanup()
        if not failing:
            guests_are_as_found()
    (workspace / 'summary.json').write_text(json.dumps(passed, indent=2) + '\n')
    print(f"{len(passed)} MySQL and MariaDB workflows passed: MariaDB {servers['mariadb'].get('version', '')}"
          + (f", MySQL {servers['mysql'].get('version', '')}" if 'mysql' in servers else f'; {distribution} packages no MySQL'), flush=True)


if __name__ == '__main__':
    vm.run_suite(main)
