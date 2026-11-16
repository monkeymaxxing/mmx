#!/usr/bin/env python3
"""mongodb_user through the public CLI, against a MongoDB server from MongoDB's
own package repository, checked with mongosh itself and none of MMX.

The whole test runs on all six distributions, which MongoDB serves a
package repository for. Rocky Linux 9 installs MongoDB 8.0, the other five
MongoDB 9.0. Each first runs the test without mongosh: a dryrun is not
checked and names the program, and a run fails once with a missing
prerequisite. A container is enough on every distribution."""
import json
import re
import shlex
import shutil
import sys
import time

import vm

BASE = '/var/tmp/mmx-mongodb-users'
EXAMPLE = 'mongodb-server-for-one-application'
# Every call of mongosh by MMX goes through a wrapper that notes its command
# line and its environment here.
MONGOSH_CALLS = BASE + '/mongosh-calls'
# While this file exists, the wrapper lets the next changing run of mongosh do
# its work and then ends it with exit code 1 and without its result, as a
# mongosh that fails after it sent its commands.
END_THE_NEXT_CHANGE_WITHOUT_A_RESULT = BASE + '/end-the-next-change-without-a-result'
# The files of a second member of the replica set, which the suite starts by
# hand, and the key file that the members of a replica set log in to each
# other with.
SECOND_MEMBER = '/var/lib/mmx-test-mongod-second'
KEY_FILE = '/etc/mmx-test-mongod-key'
CONNECTION = 'mongodb://127.0.0.1:27017/?directConnection=true'
# Made-up passwords with what a password may hold: quotes, a backslash,
# letters that are not ASCII, and the characters of a connection string.
ADMINISTRATOR_PASSWORD = 'Administrator "sentinel" \\pass:/@?#%&+ ü-3'
USER_PASSWORD = "Sentinel-pä'ss\"w\\örd-4711"
SECOND_USER_PASSWORD = 'Second-sentinel-0815'
LIMITED_PASSWORD = 'Limited-sentinel-2718'
PASSWORD_IN_THE_PLAYBOOK = 'Written-in-the-playbook-3141'
SECRETS = {'secrets_from_env': {'administrator_password': 'MMX_TEST_MONGODB_ADMINISTRATOR_PASSWORD', 'user_password': 'MMX_TEST_MONGODB_USER_PASSWORD',
                                        'second_user_password': 'MMX_TEST_MONGODB_SECOND_USER_PASSWORD', 'limited_password': 'MMX_TEST_MONGODB_LIMITED_PASSWORD',
                                        'operator_name': 'MMX_TEST_MONGODB_OPERATOR_NAME'}}
ENVIRONMENT = {'MMX_TEST_MONGODB_ADMINISTRATOR_PASSWORD': ADMINISTRATOR_PASSWORD, 'MMX_TEST_MONGODB_USER_PASSWORD': USER_PASSWORD,
               'MMX_TEST_MONGODB_SECOND_USER_PASSWORD': SECOND_USER_PASSWORD, 'MMX_TEST_MONGODB_LIMITED_PASSWORD': LIMITED_PASSWORD,
               'MMX_TEST_MONGODB_OPERATOR_NAME': 'mmx-operator',
               'MONGODB_ADMIN_PASSWORD': ADMINISTRATOR_PASSWORD, 'SHOP_MONGODB_PASSWORD': USER_PASSWORD}
# The key that signs the packages of each series, with its SHA-256. The
# example project's inventory holds the values of series 9.0.
SIGNING_KEYS = {
    '8.0': ('https://pgp.mongodb.com/server-8.0.asc', '8c467ea138207ee8d0cbbce3e005c82c73c81b3263e7909bdc36b9a1feff14f6'),
    '9.0': ('https://pgp.mongodb.com/server-9.asc', 'a8ec953b159e65ee2e327dde5506008d7693c3c6c3841beed421ec429bd10162'),
}
MONGOSH_COMMAND = 'env HOME=/dev/null mongosh --quiet --norc --nodb --file /dev/stdin'
MISSING_MONGOSH = ("mongodb_user: the program mongosh is not installed; install the package mongodb-mongosh, from MongoDB's own package repository, "
                   'in an earlier node task')
NO_ADMINISTRATOR_YET = ('mongodb_user: this MongoDB server has access control on and no administrator yet; '
                        'create the first user in the database admin, with the role root, in an earlier mongodb_user task')
# What a MongoDB server's packages leave behind when they are removed, and
# what the suite itself puts on the guest.
LEFT_BY_THE_PACKAGES = ['/var/lib/mongodb', '/var/lib/mongo', '/var/log/mongodb', '/etc/mongod.conf', '/etc/mongod.conf.rpmsave', '/tmp/mongodb-27017.sock', '/tmp/mongodb-27018.sock']
OF_THE_SUITE = [BASE, SECOND_MEMBER, KEY_FILE, '/usr/local/bin/mongosh', '/root/.mongodb',
                '/etc/apt/sources.list.d/mongodb-org.sources', '/etc/apt/keyrings/mongodb-org.asc', '/etc/yum.repos.d/mongodb-org.repo', '/etc/pki/rpm-gpg/RPM-GPG-KEY-mongodb-org']
# The names of the installed packages, and the keys that dnf's rpm trusts.
INSTALLED_PACKAGES = ("if command -v dpkg-query >/dev/null; then dpkg-query -W -f '${db:Status-Abbrev} ${Package}\\n' | awk '$1 == \"ii\" {print $2}'; "
                      "else rpm -qa --qf '%{NAME}\\n' | grep -v '^gpg-pubkey$'; fi | sort -u")
TRUSTED_RPM_KEYS = "if command -v rpm >/dev/null && ! command -v dpkg-query >/dev/null; then rpm -q gpg-pubkey 2>/dev/null | grep '^gpg-pubkey-' | sort; fi"


def series_for(distribution, version):
    """The MongoDB series that the suite installs: an older one on Rocky Linux 9, so that the suite meets a server before 8.2."""
    major = version.split('.')[0]
    return {('rocky', '9'): '8.0'}.get((distribution, major), '9.0')


def main():
    vm.up()
    node, ssh = vm.connection()
    vm.reset()
    distribution, version = vm.cmd([*ssh, '. /etc/os-release; echo "$ID $VERSION_ID"']).stdout.split()
    workspace = vm.ROOT / 'mongodb-users'
    example = vm.ROOT / ('mongodb-users-' + EXAMPLE)
    for directory in (workspace, example):
        if directory.exists():
            shutil.rmtree(directory)
    vm.write_inventory(workspace, {'nodes': {'database': node}}, name='example')
    vm.build()
    mmx = vm.ROOT / 'bin/mmx'
    passed = []
    marked = time.monotonic()
    # Everything mmx printed, to search for the passwords at the end.
    printed = []
    apt = distribution in ('debian', 'ubuntu')
    data_dir = '/var/lib/mongodb' if apt else '/var/lib/mongo'
    server_account = 'mongodb' if apt else 'mongod'
    state = {}

    def remote(script, input=None, check=True):
        return vm.cmd([*ssh, 'sudo -n /bin/bash -c ' + shlex.quote('set -eu\n' + script)], input=input, check=check).stdout.strip()

    def mongo(script, login=None, connection=CONNECTION, **arguments):
        """Run JavaScript on the node's server with mongosh itself and none of MMX, and return what the script returns.

        The script has connection, logged in as login (a name, its database, and its password) when one is given,
        its arguments as given.arguments, and code_of(action), which gives the error code of an action, or 0."""
        given = json.dumps({'connection': connection, 'login': login, 'arguments': arguments})
        program = f'''const given = {given};
const connection = new Mongo(given.connection);
if (given.login) {{ connection.getDB(given.login[1]).auth(given.login[0], given.login[2]); }}
function code_of(action) {{ try {{ action(); return 0; }} catch (error) {{ return error.code === undefined ? String(error.name) : error.code; }} }}
const answer = (function () {{ {script} }})();
print(JSON.stringify(answer === undefined ? null : answer));
'''
        # With this HOME, mongosh writes no log of the suite's own programs.
        p = vm.cmd([*ssh, 'sudo -n env HOME=/dev/null /usr/bin/mongosh --quiet --norc --nodb --file /dev/stdin'], input=program, check=False)
        if p.returncode:
            raise AssertionError(f'mongosh ended with exit code {p.returncode}: {p.stderr.strip().splitlines()[:1]} for: {script}')
        return json.loads(p.stdout.strip().splitlines()[-1])

    administrator = ('admin', 'admin', ADMINISTRATOR_PASSWORD)

    def users(login=administrator, connection=CONNECTION):
        """Every user of the server with its roles, as the server gives them to login: {'shop@shop': ['readWrite@shop']}."""
        return mongo('''return Object.fromEntries(connection.getDB("admin").runCommand({usersInfo: {forAllDBs: true}}).users.map(
  (user) => [user.user + "@" + user.db, user.roles.map((role) => role.role + "@" + role.db).sort()]));''', login=login, connection=connection)

    def suite_users(login=administrator):
        """The users that the suite's own tasks manage: those whose name starts with mmx."""
        return {name: roles for name, roles in users(login).items() if name.startswith('mmx')}

    def logins(*attempts):
        """Whether the server accepts each login, a name, its database, and a password, from a new connection each: a list of True and False."""
        codes = mongo('''return given.arguments.attempts.map(function (attempt) {
  const fresh = new Mongo(given.connection);
  try { return code_of(() => fresh.getDB(attempt[1]).auth(attempt[0], attempt[2])); } finally { fresh.close(); }
});''', attempts=[list(attempt) for attempt in attempts])
        assert all(code in (0, 18) for code in codes), codes
        return [code == 0 for code in codes]

    def project(tasks, **settings):
        vm.write_project(workspace, project_name='mongodb_users', node_task_defaults={'retry': {'pause_between_tries': '0s'}},
                         fleet_stages=[{'id': 'configure', 'nodes': 'all', 'node_tasks': tasks}], **{**SECRETS, **settings})

    def run(command, expected=None, directory=None):
        result = vm.cmd([mmx, command, *vm.playbook_args(command), '--json'], cwd=directory or workspace, check=False, timeout=900)
        printed.append(result.stdout + result.stderr)
        if result.returncode not in (vm.success_codes(command) if expected is None else (expected,)):
            raise AssertionError((command, result.returncode, result.stdout, result.stderr))
        return json.loads(result.stdout)

    def all_results(record):
        return record['entries'][0]['response']['results']

    def results(record):
        """The results of the node's tasks, by the last part of each task's name."""
        return {r['task'].rsplit('/', 1)[-1]: r for r in all_results(record)}

    def is_result(result, status, change, created=None, reason=None, reason_of_each_try=None):
        """The result has this status and change and is not hidden as a secret; its output created and its whole reason are these. A task that failed in a way that another try may fix was tried three times, in a dryrun too, and each try has reason_of_each_try."""
        assert result['status'] == status and result['change'] == change and result.get('secret') is not True, result
        if created is not None:
            assert result['module_outputs'] == {'created': created}, result
        if reason is not None:
            assert result.get('reason', '') == reason, result
        if reason_of_each_try is not None:
            assert [each['reason'] for each in result['tries']] == [reason_of_each_try] * 3 and result['reason'].endswith(reason_of_each_try) and 'failure_kind' not in result, result
        # The one command that a changing task runs holds nothing of the task.
        if change in ('changed', 'would_change'):
            assert result['diff'] == ('ran: ' if change == 'changed' else 'would run: ') + MONGOSH_COMMAND + '\n', result
        return True

    def tries(result):
        """How often the node task was tried."""
        return len(result.get('tries') or [result])

    def unchanged(record):
        response = record['entries'][0]['response']
        assert response['change'] == 'unchanged', {r['task']: (r['status'], r['change'], r.get('reason', '')) for r in response['results']}
        return True

    def mark(message):
        # The seconds since the last mark say where the suite's time goes.
        nonlocal marked
        now = time.monotonic()
        passed.append(message)
        print(f'PASS {message} ({now - marked:.0f} s)', flush=True)
        marked = now

    def user(task_id, name, database='mmx_orders', login=None, **fields):
        """One mongodb_user node task that logs in as the administrator, or with the inputs of login; an empty login gives the task none."""
        if login is None:
            login = {'login_user': 'admin', 'login_password': '{{ .secrets.administrator_password }}'}
        return {'id': task_id, 'mongodb_user': {'name': name, 'database': database, **fields, **login}}

    def role(name, database):
        return {'role': name, 'database': database}

    def in_database(name, database='mmx_orders'):
        return f'the user {name} in the database {database}'

    def before_the_suite():
        """What the guest had before the suite, which the cleanup must leave."""
        state['packages_before'] = set(remote(INSTALLED_PACKAGES).split())
        state['keys_before'] = set(remote(TRUSTED_RPM_KEYS).split())
        state['paths_before'] = remote(f"ls -d {' '.join(LEFT_BY_THE_PACKAGES + OF_THE_SUITE)} 2>/dev/null || true").split()
        state['accounts_before'] = remote('getent passwd mongodb mongod || true; getent group mongodb mongod || true')

    def server_configuration(access_control, replica_set):
        """The server's settings file for a part of the suite: the example's file with other last lines."""
        text = (f'storage:\n  dbPath: {data_dir}\nsystemLog:\n  destination: file\n  logAppend: true\n  path: /var/log/mongodb/mongod.log\n'
                'net:\n  port: 27017\n  bindIp: 127.0.0.1\nprocessManagement:\n  timeZoneInfo: /usr/share/zoneinfo\n')
        if replica_set:
            text += 'replication:\n  replSetName: rs0\n'
        if access_control:
            # The members of a replica set with access control log in to each other with a key file.
            text += 'security:\n  authorization: enabled\n' + (f'  keyFile: {KEY_FILE}\n' if replica_set else '')
        return text

    def start_server_with(configuration, empty=False):
        """Stop the server, write its settings, start it, and wait for its port. systemd refuses more than five starts in ten seconds, so its count is reset first."""
        remote(f"""systemctl stop mongod
{f'find {data_dir} -mindepth 1 -delete' if empty else ''}
cat > /etc/mongod.conf
systemctl reset-failed mongod 2>/dev/null || true
systemctl start mongod
for i in $(seq 1 300); do if (exec 3<>/dev/tcp/127.0.0.1/27017) 2>/dev/null; then exit 0; fi; sleep 0.1; done
echo "the MongoDB server does not answer on port 27017" >&2; journalctl -u mongod -n 20 --no-pager >&2; exit 1
""", input=configuration)

    def wait_until_primary(login=None):
        assert mongo('''for (let i = 0; i < 600; i++) { if (connection.getDB("admin").runCommand({hello: 1}).isWritablePrimary) { return true; } sleep(50); }
return false;''', login=login), 'the MongoDB server did not become the primary of its replica set in 30 seconds'

    def cleanup():
        """Remove the server, its data, MongoDB's repository, and everything of the suite, so that the next suite finds the guest as this one found it: exactly the packages that the suite's runs added go, with what they leave behind."""
        added = ' '.join(sorted(set(remote(INSTALLED_PACKAGES).split()) - state['packages_before']))
        keys = ' '.join(sorted(set(remote(TRUSTED_RPM_KEYS).split()) - state['keys_before']))
        paths = ' '.join(path for path in LEFT_BY_THE_PACKAGES + OF_THE_SUITE if path not in state['paths_before'])
        accounts = '' if state['accounts_before'] else 'for account in mongodb mongod; do userdel $account 2>/dev/null || true; groupdel $account 2>/dev/null || true; done'
        remove = f"""if command -v apt-get >/dev/null; then DEBIAN_FRONTEND=noninteractive apt-get purge -y -q {added} >/dev/null 2>&1 || true
elif command -v dnf >/dev/null; then dnf remove -y -q {added} >/dev/null 2>&1 || true
fi""" if added else ''
        remote(f"""
# The second member of the replica set is ended by its process ID, at once.
if [ -e {SECOND_MEMBER}/pid ]; then kill -KILL "$(cat {SECOND_MEMBER}/pid)" 2>/dev/null || true; fi
systemctl stop mongod 2>/dev/null || true
{remove}
{f'rpm -e {keys}' if keys else ''}
rm -rf {paths}
rm -rf /var/lib/apt/lists/repo.mongodb.org_* /var/cache/dnf/mongodb-org*
{accounts}
systemctl daemon-reload
systemctl reset-failed 2>/dev/null || true
""")

    def guest_is_as_found():
        paths = ' '.join(path for path in LEFT_BY_THE_PACKAGES + OF_THE_SUITE if path not in state['paths_before'])
        left = remote(f"""
for program in mongosh mongod; do command -v $program || true; done
ls -d {paths} /var/lib/apt/lists/repo.mongodb.org_* 2>/dev/null || true
systemctl list-unit-files 'mongod*' --no-legend 2>/dev/null || true
""")
        packages = set(remote(INSTALLED_PACKAGES).split())
        if packages != state['packages_before']:
            left += f"\npackages added: {sorted(packages - state['packages_before'])}; packages gone: {sorted(state['packages_before'] - packages)}"
        keys = set(remote(TRUSTED_RPM_KEYS).split())
        if keys != state['keys_before']:
            left += f"\nkeys of rpm added: {sorted(keys - state['keys_before'])}; keys gone: {sorted(state['keys_before'] - keys)}"
        accounts = remote('getent passwd mongodb mongod || true; getent group mongodb mongod || true')
        if accounts != state['accounts_before']:
            left += '\naccounts and groups: ' + accounts
        running = vm.guest('sudo', '-n', 'systemctl', 'is-system-running', check=False).stdout.strip()
        if running != 'running':
            left += f'\nsystemd is {running}:\n' + vm.guest('sudo', '-n', '/bin/sh', '-c', vm.FAILED_UNITS, check=False).stdout
        assert left == '', f'the suite left this on the guest:\n{left}'
        print('The guest is as the suite found it: no MongoDB, no repository of MongoDB, no account of the server, and systemd is running', flush=True)

    assert remote('for program in mongosh mongod; do command -v $program || true; done') == '', 'the guest already has MongoDB'
    before_the_suite()
    try:
        vm.ENV.update(ENVIRONMENT)
        shop = user('shop', 'mmx-shop', password='{{ .secrets.user_password }}', roles=[role('readWrite', 'mmx_orders')], roles_not_listed='remove')

        # Before MongoDB is installed: the node has no mongosh.
        project([shop])
        is_result(results(run('dryrun'))['shop'], 'not_checked', 'unknown', reason=MISSING_MONGOSH)
        missing = results(run('run', 1))['shop']
        assert is_result(missing, 'failed', 'unchanged', reason=MISSING_MONGOSH) and missing['failure_kind'] == 'missing_prerequisite' and tries(missing) == 1, missing
        mark('without mongosh, a dryrun is not checked and names the program and its package, and a run fails once with a missing prerequisite')
        # MongoDB publishes its Debian packages only for amd64, and its server
        # refuses to start on Linux 6.19.0 through 7.0.13
        # (https://jira.mongodb.org/browse/SERVER-121912), such as Ubuntu
        # 26.04's first kernels. A container shares its server's kernel.
        kernel = tuple(int(part) for part in re.match(r'(\d+)\.(\d+)\.(\d+)', remote('uname -r')).groups())
        without_mongodb = ('MongoDB publishes its Debian packages only for amd64' if distribution == 'debian' and vm.architecture() == 'arm64'
                           else 'MongoDB refuses to start on Linux 6.19.0 through 7.0.13' if (6, 19, 0) <= kernel <= (7, 0, 13) else None)
        if without_mongodb:
            print(f'{distribution} {version}: the suite installs no MongoDB here: {without_mongodb}', flush=True)
            (workspace / 'summary.json').write_text(json.dumps(passed, indent=2) + '\n')
            return

        # The example project: MongoDB's repository, the server and mongosh,
        # the settings with access control, the first administrator, and the
        # application's user. Its playbook is the module page's "Set up".
        shutil.copytree(vm.REPO / 'examples' / EXAMPLE, example)
        # The example's own inventory holds the values of series 9.0 that the
        # suite uses. The suite's inventory names the guest, in the example's
        # group, with the series of this distribution.
        shipped = (example / 'inventories/example.yml').read_text()
        assert all(line in shipped for line in ('mongodb_series: "9.0"\n', 'mongodb_signing_key_url: ' + SIGNING_KEYS['9.0'][0] + '\n',
                                                'mongodb_signing_key_sha256: ' + SIGNING_KEYS['9.0'][1] + '\n')), shipped
        series = series_for(distribution, version)
        vm.write_inventory(example, {'nodes': {'database01': node}, 'groups': {'mongodb_servers': {'nodes': ['database01'], 'vars': {
            'mongodb_series': series, 'mongodb_signing_key_url': SIGNING_KEYS[series][0], 'mongodb_signing_key_sha256': SIGNING_KEYS[series][1]}}}}, name='example')
        # The first dryrun, on a node without MongoDB, passes: the two user
        # tasks wait for mongosh.
        # Both name the first node task of the playbook that would change the
        # node: the repository task of the distribution, task_1 for Debian,
        # task_2 for Ubuntu, and task_3 for the Red Hat family.
        first_change = {'debian': 'task_1', 'ubuntu': 'task_2'}.get(distribution, 'task_3')
        first_dryrun = [r for r in all_results(run('dryrun', directory=example)) if r.get('module_kind') == 'official_extra']
        assert len(first_dryrun) == 2 and all(is_result(r, 'not_checked', 'unknown', reason=vm.earlier_change_may_provide(first_change, MISSING_MONGOSH)) for r in first_dryrun), first_dryrun
        started = time.monotonic()
        applied = run('run', directory=example)
        print(f'The example project installed MongoDB {series}, started it, and created both users in {time.monotonic() - started:.0f} s', flush=True)
        first_administrator, application_user = [r for r in all_results(applied) if r.get('module_kind') == 'official_extra']
        is_result(first_administrator, 'succeeded', 'changed', created=True,
                  reason='created the user admin in the database admin with the role root on the database admin, as the first user of this MongoDB server')
        is_result(application_user, 'succeeded', 'changed', created=True, reason='created the user shop in the database shop with the role readWrite on the database shop')
        server_version = mongo('return connection.getDB("admin").runCommand({buildInfo: 1}).version;', login=administrator)
        assert server_version.startswith(series + '.'), (server_version, series)
        print(f"MongoDB {server_version}, mongosh {remote('/usr/bin/mongosh --version')}", flush=True)
        # MongoDB 8.2 lets the first administrator in without a login also
        # while users exist in other databases; the series before it and
        # MongoDB 9.0 ask for a login that nobody has.
        first_administrator_beside_other_users = tuple(int(part) for part in server_version.split('.')[:2]) == (8, 2)
        assert remote('systemctl is-active mongod; systemctl is-enabled mongod') == 'active\nenabled'
        assert users() == {'admin@admin': ['root@admin'], 'shop@shop': ['readWrite@shop']}, users()
        # Without a login the server tells nothing, the application's user
        # reads and writes its own database, and it manages no user.
        assert mongo('return code_of(() => connection.getDB("admin").runCommand({listDatabases: 1}));') == 13
        assert logins(('shop', 'shop', USER_PASSWORD), ('shop', 'shop', 'not the password'), ('admin', 'admin', ADMINISTRATOR_PASSWORD), ('admin', 'admin', USER_PASSWORD)) == [True, False, True, False]
        assert mongo('''connection.getDB("shop").orders.insertOne({number: 1});
return [connection.getDB("shop").orders.countDocuments({}), code_of(() => connection.getDB("shop").createUser({user: "mmx-not-allowed", pwd: "x", roles: []})),
        code_of(() => connection.getDB("mmx_orders").orders.insertOne({number: 1}))];''', login=('shop', 'shop', USER_PASSWORD)) == [1, 13, 13]
        for command in ('run', 'dryrun'):
            assert unchanged(run(command, directory=example))
        # From here on every call of mongosh by MMX goes through a wrapper
        # that notes its command line and its environment.
        remote(f'mkdir -p {BASE}\nchmod 700 {BASE}\ninstall -m 600 /dev/null {MONGOSH_CALLS}\ncat > /usr/local/bin/mongosh\nchmod 755 /usr/local/bin/mongosh', input=f"""#!/bin/sh
{{ echo "call: mongosh $*"; env; }} >> {MONGOSH_CALLS}
if [ -e {END_THE_NEXT_CHANGE_WITHOUT_A_RESULT} ] && [ "$1" != --version ]; then
  # The program stays in a variable of this shell, on no command line.
  program=$(cat; echo .)
  program=${{program%.}}
  case "$program" in *'"mode":"change"'*)
    rm {END_THE_NEXT_CHANGE_WITHOUT_A_RESULT}
    printf '%s' "$program" | /usr/bin/mongosh "$@" > /dev/null 2>&1
    echo "the wrapper of the suite ended mongosh without its result" >&2
    exit 1 ;;
  esac
  printf '%s' "$program" | /usr/bin/mongosh "$@"
  exit $?
fi
exec /usr/bin/mongosh "$@"
""")
        # A new password in the environment changes the user's password and
        # nothing else.
        vm.ENV['SHOP_MONGODB_PASSWORD'] = SECOND_USER_PASSWORD
        changed = [r for r in all_results(run('run', directory=example)) if r['change'] != 'unchanged']
        assert len(changed) == 1 and is_result(changed[0], 'succeeded', 'changed', created=False, reason='set the password of the user shop in the database shop'), changed
        assert logins(('shop', 'shop', SECOND_USER_PASSWORD), ('shop', 'shop', USER_PASSWORD)) == [True, False]
        assert mongo('return connection.getDB("shop").orders.countDocuments({});', login=('shop', 'shop', SECOND_USER_PASSWORD)) == 1
        mark('the example ' + EXAMPLE + ': a first dryrun that waits for mongosh, the server and mongosh from MongoDB\'s repository, access control on, the first administrator created '
             'without a login, the application\'s user with the right to read and write its database and no other; an unchanged rerun and dryrun; a new password changes only the password')

        # The first run of the suite's own users.
        odd = 'mmx "odd" user\'s \\ name ü'
        # The login user's name is a secret too: it must not hide a result.
        operator_login = {'login_user': '{{ .secrets.operator_name }}', 'login_database': 'mmx_operators', 'login_password': '{{ .secrets.second_user_password }}'}
        first = [
            shop,
            user('reports', 'mmx-reports', 'mmx_reports', password='{{ .secrets.user_password }}', roles=[role('readWrite', 'mmx_reports'), role('read', 'mmx_orders')], roles_not_listed='remove'),
            user('odd', odd, password=PASSWORD_IN_THE_PLAYBOOK, roles=[role('read', 'mmx_orders')], roles_not_listed='keep'),
            user('without_roles', 'mmx-without-roles', password='{{ .secrets.user_password }}'),
            user('operator', 'mmx-operator', 'mmx_operators', password='{{ .secrets.second_user_password }}', roles=[role('userAdminAnyDatabase', 'admin')], roles_not_listed='remove'),
            # Two tasks for one user: the first creates it, and the second,
            # which has no password, grants one more role.
            user('two_tasks', 'mmx-two-tasks', password='{{ .secrets.user_password }}', roles=[role('read', 'mmx_orders')], roles_not_listed='keep'),
            user('second_task', 'mmx-two-tasks', roles=[role('read', 'mmx_reports')], roles_not_listed='keep'),
        ]
        reasons = {
            'shop': f"create {in_database('mmx-shop')} with the role readWrite on the database mmx_orders",
            'reports': f"create {in_database('mmx-reports', 'mmx_reports')} with the roles readWrite on the database mmx_reports, read on the database mmx_orders",
            'odd': f'create {in_database(odd)} with the role read on the database mmx_orders',
            'without_roles': f"create {in_database('mmx-without-roles')} without a role",
            'operator': f"create {in_database('mmx-operator', 'mmx_operators')} with the role userAdminAnyDatabase on the database admin",
            'two_tasks': f"create {in_database('mmx-two-tasks')} with the role read on the database mmx_orders",
        }
        no_password = (f"mongodb_user: {in_database('mmx-two-tasks')} does not exist, and the task has no password to create it; "
                       'add password, or create the user in an earlier mongodb_user task')
        project(first)
        previewed = results(run('dryrun'))
        assert all(is_result(previewed[task], 'succeeded', 'would_change', created=True, reason='would ' + reason) for task, reason in reasons.items())
        # In the dryrun the first of the two tasks has created nothing yet,
        # so the second one is not checked; the first task of the playbook,
        # shop, would change the node first.
        assert is_result(previewed['second_task'], 'not_checked', 'unknown', reason=vm.earlier_change_may_provide('shop', no_password)) and suite_users() == {}, suite_users()
        applied = results(run('run'))
        assert all(is_result(applied[task], 'succeeded', 'changed', created=True, reason='created ' + reason[len('create '):]) for task, reason in reasons.items())
        is_result(applied['second_task'], 'succeeded', 'changed', created=False, reason=f"granted the role read on the database mmx_reports to {in_database('mmx-two-tasks')}")
        assert suite_users() == {'mmx-shop@mmx_orders': ['readWrite@mmx_orders'], 'mmx-reports@mmx_reports': ['read@mmx_orders', 'readWrite@mmx_reports'],
                                 odd + '@mmx_orders': ['read@mmx_orders'], 'mmx-without-roles@mmx_orders': [], 'mmx-operator@mmx_operators': ['userAdminAnyDatabase@admin'],
                                 'mmx-two-tasks@mmx_orders': ['read@mmx_orders', 'read@mmx_reports']}, suite_users()
        assert logins(('mmx-shop', 'mmx_orders', USER_PASSWORD), ('mmx-shop', 'mmx_orders', SECOND_USER_PASSWORD), (odd, 'mmx_orders', PASSWORD_IN_THE_PLAYBOOK),
                      ('mmx-shop', 'mmx_reports', USER_PASSWORD), ('mmx-operator', 'mmx_operators', SECOND_USER_PASSWORD)) == [True, False, True, False, True]
        assert unchanged(run('run'))
        mark('the first run: users in three databases, one with a name full of quotes, one without roles, one with a role on another database; a dryrun before that changes nothing '
             'and says what a run would create; two tasks for one user, of which the second has no password and is not checked in the dryrun; logins with the passwords work; '
             'an unchanged rerun')

        # A hand-made state that a run changes: only what differs.
        mongo('''const orders = connection.getDB("mmx_orders");
orders.createUser({user: "mmx-keep", pwd: "another password", roles: [{role: "read", db: "mmx_orders"}, {role: "dbAdmin", db: "mmx_orders"}]});
orders.createUser({user: "mmx-remove", pwd: given.arguments.password, roles: [{role: "dbAdmin", db: "mmx_orders"}, {role: "read", db: "mmx_reports"}, {role: "read", db: "mmx_orders"}]});
orders.createUser({user: "mmx-empty", pwd: "another password", roles: [{role: "read", db: "mmx_orders"}]});''', login=administrator, password=USER_PASSWORD)
        by_hand = [
            # keep only adds, and the password is set because the server refuses a login with it.
            user('keep', 'mmx-keep', password='{{ .secrets.user_password }}', roles=[role('read', 'mmx_orders'), role('readWrite', 'mmx_reports')], roles_not_listed='keep'),
            # remove writes the whole list; the password is the user's already.
            user('remove', 'mmx-remove', password='{{ .secrets.user_password }}', roles=[role('read', 'mmx_orders'), role('readWrite', 'mmx_orders')], roles_not_listed='remove'),
            # An empty list with remove leaves no role; a task without password does not touch the password.
            user('empty', 'mmx-empty', roles=[], roles_not_listed='remove'),
            # A login in another database than admin.
            user('by_operator', 'mmx-by-operator', password='{{ .secrets.user_password }}', roles=[role('read', 'mmx_orders')], roles_not_listed='remove', login=operator_login),
            # A task without password and roles for a user that exists has nothing to do.
            user('exists', 'mmx-shop'),
        ]
        reasons = {
            'keep': (False, f"set the password of {in_database('mmx-keep')}; grant the role readWrite on the database mmx_reports to {in_database('mmx-keep')}"),
            'remove': (False, f"grant the role readWrite on the database mmx_orders to {in_database('mmx-remove')}; "
                              f"revoke the roles dbAdmin on the database mmx_orders, read on the database mmx_reports from {in_database('mmx-remove')}"),
            'empty': (False, f"revoke the role read on the database mmx_orders from {in_database('mmx-empty')}"),
            'by_operator': (True, f"create {in_database('mmx-by-operator')} with the role read on the database mmx_orders"),
        }
        hand_made = suite_users()
        project(by_hand)
        previewed = results(run('dryrun'))
        assert all(is_result(previewed[task], 'succeeded', 'would_change', created=created, reason='; '.join('would ' + part for part in reason.split('; ')))
                   for task, (created, reason) in reasons.items())
        assert is_result(previewed['exists'], 'succeeded', 'unchanged', created=False, reason='') and suite_users() == hand_made
        applied = results(run('run'))
        past = {'set': 'set', 'grant': 'granted', 'revoke': 'revoked', 'create': 'created'}
        assert all(is_result(applied[task], 'succeeded', 'changed', created=created,
                             reason='; '.join(past[part.split(' ', 1)[0]] + ' ' + part.split(' ', 1)[1] for part in reason.split('; ')))
                   for task, (created, reason) in reasons.items())
        assert suite_users() == {**hand_made, 'mmx-keep@mmx_orders': ['dbAdmin@mmx_orders', 'read@mmx_orders', 'readWrite@mmx_reports'],
                                 'mmx-remove@mmx_orders': ['read@mmx_orders', 'readWrite@mmx_orders'], 'mmx-empty@mmx_orders': [],
                                 'mmx-by-operator@mmx_orders': ['read@mmx_orders']}, suite_users()
        assert logins(('mmx-keep', 'mmx_orders', USER_PASSWORD), ('mmx-keep', 'mmx_orders', 'another password'), ('mmx-empty', 'mmx_orders', 'another password')) == [True, False, True]
        assert unchanged(run('dryrun'))
        mark('a hand-made state: roles_not_listed keep only grants, remove also revokes, and an empty list with remove leaves no role; the password is set only where the server '
             'refuses a login with it, and a task without password leaves it; a login user in another database than admin; a dryrun before changes nothing, and one afterwards is unchanged')

        # What a dryrun cannot check: a role that the server does not have,
        # and a server that does not answer. Both wait for an earlier node
        # task, and neither stops the dryrun.
        missing_role = user('missing_role', 'mmx-missing-role', password='{{ .secrets.user_password }}', roles=[role('read', 'mmx_orders'), role('mmxNoSuchRole', 'mmx_orders')], roles_not_listed='remove')
        no_such_role = 'mongodb_user: the role mmxNoSuchRole on the database mmx_orders does not exist on this MongoDB server; create it in an earlier node task'
        closed_port = user('closed_port', 'mmx-shop', mongodb_connection_string='mongodb://127.0.0.1:27999/?directConnection=true')
        no_answer = 'mongodb_user: the MongoDB server does not answer: connect ECONNREFUSED 127.0.0.1:27999'
        project([missing_role, closed_port])
        previewed = results(run('dryrun'))
        assert is_result(previewed['missing_role'], 'not_checked', 'unknown', reason=no_such_role) and is_result(previewed['closed_port'], 'not_checked', 'unknown', reason=no_answer)
        project([missing_role])
        failed = results(run('run', 1))['missing_role']
        assert is_result(failed, 'failed', 'unchanged', reason=no_such_role) and failed['failure_kind'] == 'missing_prerequisite' and tries(failed) == 1, failed
        # A user that the server does not have, for a task without password:
        # no earlier node task created it, so the run fails once.
        project([user('new', 'mmx-new')])
        failed = results(run('run', 1))['new']
        assert is_result(failed, 'failed', 'unchanged', reason=no_password.replace('mmx-two-tasks', 'mmx-new')) and failed['failure_kind'] == 'missing_prerequisite' and tries(failed) == 1, failed
        # A server that does not answer may answer a moment later: a run tries three times.
        project([closed_port])
        is_result(results(run('run', 1))['closed_port'], 'failed', 'unchanged', reason_of_each_try=no_answer)

        # A failure that another try cannot fix fails a dryrun and a run
        # alike and is not tried again: inputs that break a rule, a refused
        # login, a command that the server refuses to the login user, and a
        # server that is not the primary.
        def rejected(command, task, reason=None, change='unchanged'):
            project([task])
            result = results(run(command, 1))[task['id']]
            assert is_result(result, 'failed', change, reason=reason) and result['failure_kind'] == 'config' and tries(result) == 1, result
            return result
        rejected('run', user('no_login', 'mmx-shop', login={}), 'mongodb_user.login_user: this MongoDB server needs a login; give login_user and login_password')
        rejected('dryrun', user('unreadable', 'mmx-shop', mongodb_connection_string='mongodb://127.0.0.1:27017/?directConnection=true&noSuchOption=1'),
                 'mongodb_user.mongodb_connection_string: mongosh cannot read this connection string: option nosuchoption is not supported')
        rejected('run', user('with_login', 'mmx-shop', mongodb_connection_string='mongodb://admin:not-a-real-password@127.0.0.1:27017/?directConnection=true'),
                 'mongodb_user.mongodb_connection_string: must hold no user name and no password; give them as login_user and login_password')
        # A name that MongoDB cannot take for a database is refused for the
        # input that holds it: mongosh refuses a dot, a space, and a slash,
        # and the server a name of more than 63 bytes.
        cannot_take = 'MongoDB cannot take this as the name of a database: [COMMON-10001] Invalid database name: '
        rejected('dryrun', user('dot', 'mmx-shop', 'mmx.orders'), 'mongodb_user.database: ' + cannot_take + 'mmx.orders')
        rejected('run', user('space', 'mmx-shop', login={'login_user': 'admin', 'login_database': 'mmx operators', 'login_password': '{{ .secrets.administrator_password }}'}),
                 'mongodb_user.login_database: ' + cannot_take + 'mmx operators')
        rejected('dryrun', user('slash', 'mmx-shop', roles=[role('readWrite', 'mmx_orders'), role('read', 'mmx/reports')], roles_not_listed='keep'),
                 'mongodb_user.roles: MongoDB cannot take the database of the role read as the name of a database: [COMMON-10001] Invalid database name: mmx/reports')
        too_long = rejected('run', user('too_long', 'mmx-shop', 'd' * 64))
        assert too_long['reason'].startswith('mongodb_user.database: MongoDB cannot take this as the name of a database: ') and \
            too_long['reason'].endswith("(MongoDB's error 73, InvalidNamespace)"), too_long
        # A removal refuses the inputs that it does not use.
        rejected('dryrun', user('removal', 'mmx-shop', state='absent', password='{{ .secrets.user_password }}'),
                 'mongodb_user.state: absent accepts only name, database, login_user, login_password, login_database, and mongodb_connection_string; remove password')

        # A wrong login password fails the task in a dryrun too, once, with
        # a reason that names the login and never the password.
        refused = 'mongodb_user: the MongoDB server refused the login of the user admin in the database admin; check login_user, login_database, and login_password'
        vm.ENV['MMX_TEST_MONGODB_ADMINISTRATOR_PASSWORD'] = 'Wrong-sentinel-1618'
        for command in ('dryrun', 'run'):
            rejected(command, shop, refused)
        vm.ENV['MMX_TEST_MONGODB_ADMINISTRATOR_PASSWORD'] = ADMINISTRATOR_PASSWORD
        # A login user whose name is a secret, with a wrong password: the
        # result is not hidden, and the reason shows [redacted] for the name.
        rejected('run', user('by_operator', 'mmx-by-operator', login={**operator_login, 'login_password': '{{ .secrets.limited_password }}'}),
                 'mongodb_user: the MongoDB server refused the login of the user [redacted] in the database mmx_operators; check login_user, login_database, and login_password')
        # A login user that may not read users.
        failed = rejected('dryrun', user('by_reports', 'mmx-shop', login={'login_user': 'mmx-reports', 'login_database': 'mmx_reports', 'login_password': '{{ .secrets.user_password }}'}))
        assert failed['reason'].startswith(f"mongodb_user: reading {in_database('mmx-shop')}: not authorized on mmx_orders to execute command ") \
            and failed['reason'].endswith("(MongoDB's error 13, Unauthorized); login_user must be a user that is allowed to manage users"), failed
        assert not {'mmx-missing-role@mmx_orders', 'mmx-new@mmx_orders'} & set(suite_users()), suite_users()
        mark('failures: a role that does not exist and a server that does not answer leave a dryrun not checked, the first fails a run once and the second three times; '
             'a missing user for a task without password fails a run once; '
             'a server that needs a login and gets none, and a connection string that mongosh cannot read or that holds a login are refused '
             'in a dryrun and a run; a name that MongoDB cannot take for a database is refused for the input that holds it; a removal refuses a password; '
             'a wrong login password fails a dryrun and a run once; a login user whose name is a secret shows as [redacted] in a readable reason; '
             'a login user that may not read users gets the server\'s message, once')

        # A run that stops between two changes. The login user may set
        # passwords and may not grant a role on another database: the task
        # sets the password, and the server refuses the grant. The result
        # says both, and the task has one try, because the login user may
        # not grant at the next try either. The next run, as the
        # administrator, only grants.
        mongo('''connection.getDB("admin").createRole({role: "mmxSetsPasswordsOnly", roles: [], privileges: [
  {resource: {db: "mmx_orders", collection: ""}, actions: ["changePassword", "viewUser", "viewRole"]}, {resource: {db: "mmx_reports", collection: ""}, actions: ["viewRole"]}]});
connection.getDB("admin").createUser({user: "mmx-limited", pwd: given.arguments.password, roles: [{role: "mmxSetsPasswordsOnly", db: "admin"}]});''', login=administrator, password=LIMITED_PASSWORD)
        half = {'password': '{{ .secrets.second_user_password }}', 'roles': [role('dbAdmin', 'mmx_reports')], 'roles_not_listed': 'keep'}
        grant = f"the role dbAdmin on the database mmx_reports to {in_database('mmx-keep')}"
        stopped = rejected('run', user('half', 'mmx-keep', login={'login_user': 'mmx-limited', 'login_password': '{{ .secrets.limited_password }}'}, **half), change='changed')
        assert stopped['reason'].startswith(f'mongodb_user: granting {grant}: not authorized on mmx_orders to execute command ') and \
            stopped['reason'].endswith("(MongoDB's error 13, Unauthorized); login_user must be a user that is allowed to manage users. "
                                       f"Before this failure, the task set the password of {in_database('mmx-keep')}"), stopped
        assert logins(('mmx-keep', 'mmx_orders', SECOND_USER_PASSWORD), ('mmx-keep', 'mmx_orders', USER_PASSWORD)) == [True, False]
        assert suite_users()['mmx-keep@mmx_orders'] == ['dbAdmin@mmx_orders', 'read@mmx_orders', 'readWrite@mmx_reports']
        project([user('half', 'mmx-keep', **half)])
        is_result(results(run('run'))['half'], 'succeeded', 'changed', created=False, reason='granted ' + grant)
        assert suite_users()['mmx-keep@mmx_orders'] == ['dbAdmin@mmx_orders', 'dbAdmin@mmx_reports', 'read@mmx_orders', 'readWrite@mmx_reports']
        # A run that creates the user and does not learn it: the wrapper lets
        # mongosh create the user and ends it without its result. Another
        # try would find the user and report created: false. The task reads
        # the server once more, fails without another try, and says that the
        # user is there.
        ended = user('ended', 'mmx-ended', password='{{ .secrets.user_password }}', roles=[role('read', 'mmx_orders')], roles_not_listed='remove')
        project([ended])
        remote(f'touch {END_THE_NEXT_CHANGE_WITHOUT_A_RESULT}')
        failed = results(run('run', 1))['ended']
        assert is_result(failed, 'failed', 'changed', created=True) and tries(failed) == 1 and 'failure_kind' not in failed, failed
        assert failed['reason'] == (f"mongodb_user: creating {in_database('mmx-ended')} with the role read on the database mmx_orders: mongosh ended with exit code 1 and without a result "
                                    'that the module can read: the wrapper of the suite ended mongosh without its result. '
                                    f"The server has {in_database('mmx-ended')} now: it created the user before this failure, and the next run compares the user with the task"), failed
        assert suite_users()['mmx-ended@mmx_orders'] == ['read@mmx_orders'] and remote(f'ls {END_THE_NEXT_CHANGE_WITHOUT_A_RESULT} 2>/dev/null || true') == ''
        assert unchanged(run('run'))
        mark('a run that stopped between two changes: with a login user that may set passwords and not grant, the task sets the password, fails once at the grant with the '
             'server\'s message, and says what it did before; the next run only grants the role. A run whose mongosh ends after the creation without its result fails once, '
             'says that the server has the user, and reports created: true; the next run is unchanged')

        # Removal.
        gone = [user('reports', 'mmx-reports', 'mmx_reports', state='absent'),
                user('odd', odd, state='absent'),
                user('never_there', 'mmx-never-there', state='absent')]
        before_the_removal = suite_users()
        project(gone)
        previewed = results(run('dryrun'))
        assert is_result(previewed['reports'], 'succeeded', 'would_change', created=False, reason='would remove ' + in_database('mmx-reports', 'mmx_reports')) and \
            is_result(previewed['odd'], 'succeeded', 'would_change', created=False, reason='would remove ' + in_database(odd)) and \
            is_result(previewed['never_there'], 'succeeded', 'unchanged', created=False, reason='') and suite_users() == before_the_removal
        applied = results(run('run'))
        assert is_result(applied['reports'], 'succeeded', 'changed', created=False, reason='removed ' + in_database('mmx-reports', 'mmx_reports')) and \
            is_result(applied['odd'], 'succeeded', 'changed', created=False, reason='removed ' + in_database(odd)) and is_result(applied['never_there'], 'succeeded', 'unchanged', created=False)
        assert suite_users() == {name: roles for name, roles in before_the_removal.items() if name not in ('mmx-reports@mmx_reports', odd + '@mmx_orders')}
        assert unchanged(run('run'))
        # The administrator's own password, as the example's README says:
        # the task logs in with the old password and sets the new one.
        project([user('admin', 'admin', 'admin', password='{{ .secrets.second_user_password }}', roles=[role('root', 'admin')], roles_not_listed='remove')])
        is_result(results(run('run'))['admin'], 'succeeded', 'changed', created=False, reason='set the password of the user admin in the database admin')
        assert logins(('admin', 'admin', SECOND_USER_PASSWORD), ('admin', 'admin', ADMINISTRATOR_PASSWORD)) == [True, False]
        mark('removal: a user is removed, a dryrun before changes nothing and says what a run would remove, a user that the server does not have is unchanged, and so is a rerun; '
             'the administrator\'s own password changes with the old one as the login password')

        # A replica set with access control, as a production server has it,
        # from an empty data directory. A member that was never initiated is
        # no primary: the task says that the replica set has none.
        remote(f'head -c 600 /dev/urandom | base64 -w 0 > {KEY_FILE}\nchown {server_account} {KEY_FILE}\nchmod 400 {KEY_FILE}')
        start_server_with(server_configuration(access_control=True, replica_set=True), empty=True)
        admin = user('admin', 'admin', 'admin', password='{{ .secrets.administrator_password }}', roles=[role('root', 'admin')], roles_not_listed='remove')
        no_primary = ('mongodb_user: this MongoDB server is a member of a replica set and not its primary, and only the primary changes users. '
                      'The replica set has no primary now (Does not have a valid replica set config)')
        project([admin])
        # A replica set chooses its primary a few seconds after it was
        # initiated: a dryrun cannot check the task, and a run tries again.
        is_result(results(run('dryrun'))['admin'], 'not_checked', 'unknown', reason=no_primary)
        is_result(results(run('run', 1))['admin'], 'failed', 'unchanged', reason_of_each_try=no_primary)
        project([admin, shop])
        mongo('connection.getDB("admin").runCommand({replSetInitiate: {_id: "rs0", members: [{_id: 0, host: "127.0.0.1:27017"}]}});')
        wait_until_primary()
        # The server has no user: the first administrator would be created,
        # and every other task waits for it.
        previewed = results(run('dryrun'))
        first_user = 'the user admin in the database admin with the role root on the database admin, as the first user of this MongoDB server'
        assert is_result(previewed['admin'], 'succeeded', 'would_change', created=True, reason='would create ' + first_user) and \
            is_result(previewed['shop'], 'not_checked', 'unknown', reason=vm.earlier_change_may_provide('admin', NO_ADMINISTRATOR_YET))
        project([shop])
        waiting = results(run('run', 1))['shop']
        assert is_result(waiting, 'failed', 'unchanged', reason=NO_ADMINISTRATOR_YET) and waiting['failure_kind'] == 'missing_prerequisite' and tries(waiting) == 1, waiting
        # MongoDB would accept a first user that cannot manage users, and
        # nobody could create a user afterwards: the module refuses it.
        rejected('run', user('admin', 'admin', 'admin', password='{{ .secrets.administrator_password }}', roles=[role('readWrite', 'admin')], roles_not_listed='remove'),
                 'mongodb_user.roles: this MongoDB server has access control on and no user yet, so this task creates its first user. The first user must be allowed to manage users: '
                 'give it the role root, userAdminAnyDatabase, or userAdmin on the database admin. MongoDB would accept a first user without such a role, and nobody could create a user afterwards')
        assert mongo('return code_of(() => connection.getDB("admin").runCommand({usersInfo: 1}));') == 13
        project([admin, shop])
        applied = results(run('run'))
        assert is_result(applied['admin'], 'succeeded', 'changed', created=True, reason='created ' + first_user) and is_result(applied['shop'], 'succeeded', 'changed', created=True)
        assert users() == {'admin@admin': ['root@admin'], 'mmx-shop@mmx_orders': ['readWrite@mmx_orders']}, users()

        # A second member, which the suite starts by hand. It has no vote, so
        # that the primary stays the primary when the member ends. A task
        # that reaches the secondary fails and names the primary; with the
        # replica set's name in the connection string and without
        # directConnection, mongosh goes to the primary by itself.
        remote(f"""install -d -o {server_account} {SECOND_MEMBER}
runuser -u {server_account} -- /usr/bin/mongod --replSet rs0 --port 27018 --dbpath {SECOND_MEMBER} --bind_ip 127.0.0.1 --keyFile {KEY_FILE} --fork --logpath {SECOND_MEMBER}/log --pidfilepath {SECOND_MEMBER}/pid > /dev/null
for i in $(seq 1 300); do if (exec 3<>/dev/tcp/127.0.0.1/27018) 2>/dev/null; then exit 0; fi; sleep 0.1; done
echo "the second member does not answer on port 27018" >&2; exit 1""")
        assert mongo('''const admin = connection.getDB("admin");
const configuration = admin.runCommand({replSetGetConfig: 1}).config;
configuration.version += 1;
configuration.members.push({_id: 1, host: "127.0.0.1:27018", priority: 0, votes: 0});
admin.runCommand({replSetReconfig: configuration});
const second = new Mongo("mongodb://127.0.0.1:27018/?directConnection=true");
for (let i = 0; i < 600; i++) { if (second.getDB("admin").runCommand({hello: 1}).secondary) { return true; } sleep(50); }
return false;''', login=administrator), 'the second member did not become a secondary in 30 seconds'
        through_the_secondary = dict(password='{{ .secrets.user_password }}', roles=[role('readWrite', 'mmx_orders'), role('read', 'mmx_reports')], roles_not_listed='remove')
        rejected('dryrun', user('shop', 'mmx-shop', mongodb_connection_string='mongodb://127.0.0.1:27018/?directConnection=true', **through_the_secondary),
                 'mongodb_user: this MongoDB server is a member of the replica set rs0 and not its primary, and only the primary changes users. '
                 'The primary is 127.0.0.1:27017: run this task on that node, or put replicaSet=rs0 into mongodb_connection_string '
                 'and leave directConnection=true out, so that mongosh goes to the primary by itself')
        project([user('shop', 'mmx-shop', mongodb_connection_string='mongodb://127.0.0.1:27018/?replicaSet=rs0', **through_the_secondary)])
        is_result(results(run('run'))['shop'], 'succeeded', 'changed', created=False, reason=f"granted the role read on the database mmx_reports to {in_database('mmx-shop')}")
        assert users()['mmx-shop@mmx_orders'] == ['read@mmx_reports', 'readWrite@mmx_orders']
        mongo('''const admin = connection.getDB("admin");
const configuration = admin.runCommand({replSetGetConfig: 1}).config;
configuration.version += 1;
configuration.members = configuration.members.filter((member) => member._id === 0);
admin.runCommand({replSetReconfig: configuration});''', login=administrator)
        remote(f'kill -KILL "$(cat {SECOND_MEMBER}/pid)"\nrm -rf {SECOND_MEMBER} /tmp/mongodb-27018.sock')
        mark('a replica set with access control and a key file: on a member that was never initiated a dryrun is not checked and a run is tried three times; on the empty primary the first administrator '
             'would be created and every other task waits for it; a first user that cannot manage users is refused; a task that reaches a secondary names the primary, '
             'and with replicaSet= in the connection string it reaches the primary')

        # Without access control no login is needed: a task without one
        # works, and a task with one does not use it. The administrator is
        # removed, so that the server has users only outside admin.
        start_server_with(server_configuration(access_control=False, replica_set=True))
        wait_until_primary()
        without_login = [user('reports', 'mmx-reports', 'mmx_reports', password='{{ .secrets.user_password }}', roles=[role('read', 'mmx_reports')], roles_not_listed='remove', login={}),
                         shop,
                         user('admin', 'admin', 'admin', state='absent', login={})]
        project(without_login)
        applied = results(run('run'))
        assert is_result(applied['reports'], 'succeeded', 'changed', created=True) and is_result(applied['admin'], 'succeeded', 'changed', created=False, reason='removed the user admin in the database admin')
        is_result(applied['shop'], 'succeeded', 'changed', created=False, reason=f"revoked the role read on the database mmx_reports from {in_database('mmx-shop')}")
        assert users(login=None) == {'mmx-shop@mmx_orders': ['readWrite@mmx_orders'], 'mmx-reports@mmx_reports': ['read@mmx_reports']}, users(login=None)
        assert unchanged(run('dryrun'))
        mark('without access control: a task without a login creates a user and removes the administrator, a task with a login does not use it, and a dryrun afterwards is unchanged')

        # Access control goes on again while users exist only outside admin.
        # MongoDB 8.2 still lets the first administrator in without a login.
        # Other series, before 8.2 and 9.0, ask for a login that nobody has: the
        # module's page says to create the administrator first.
        start_server_with(server_configuration(access_control=True, replica_set=True))
        wait_until_primary()
        project([admin])
        if first_administrator_beside_other_users:
            is_result(results(run('dryrun'))['admin'], 'succeeded', 'would_change', created=True, reason='would create ' + first_user)
            is_result(results(run('run'))['admin'], 'succeeded', 'changed', created=True, reason='created ' + first_user)
            assert users() == {'admin@admin': ['root@admin'], 'mmx-shop@mmx_orders': ['readWrite@mmx_orders'], 'mmx-reports@mmx_reports': ['read@mmx_reports']}, users()
            assert unchanged(run('run'))
            mark(f'MongoDB {server_version} with users only outside admin and access control on: the first administrator is created without a login')
        else:
            for command in ('dryrun', 'run'):
                rejected(command, admin, refused)
            assert logins(('admin', 'admin', ADMINISTRATOR_PASSWORD)) == [False] and mongo('return code_of(() => connection.getDB("admin").runCommand({listDatabases: 1}));') == 13
            mark(f'MongoDB {server_version} with users only outside admin and access control on: the server asks for a login that nobody has, and the task fails with the refused login')

        # The passwords, in everything of this suite.
        saved = ''.join(path.read_text(errors='replace') for directory in (workspace, example) for path in (directory / '.mmx').rglob('*') if path.is_file() and 'runners' not in path.parts)
        forms = [ADMINISTRATOR_PASSWORD, USER_PASSWORD, SECOND_USER_PASSWORD, LIMITED_PASSWORD, 'Wrong-sentinel-1618']
        forms += [json.dumps(form, ensure_ascii=ascii)[1:-1] for form in list(forms) for ascii in (True, False)]
        for form in sorted(set(forms)):
            for where, text in (('what mmx printed', ''.join(printed)), ('the saved runs', saved)):
                assert form not in text, f'a password is in {where}: ...{text[max(0, text.find(form) - 300):text.find(form) + 60]}'
        remote(f'cat > {BASE}/patterns', input='\n'.join(sorted(set(forms))) + '\n')
        # The node's job records, MMX's directories under /run, the home
        # directories, /tmp, the server's log, the journal, and every call of
        # mongosh. mongosh itself wrote nothing: root has no .mongodb.
        found = remote(f'grep -rlF -f {BASE}/patterns /var/lib/mmx /run/mmx /root /home /tmp /var/log/mongodb 2>/dev/null || true\n'
                       f'journalctl --no-pager -q 2>/dev/null | grep -F -f {BASE}/patterns || true\n'
                       f'grep -F -f {BASE}/patterns {MONGOSH_CALLS} || true\n'
                       'ls -d /root/.mongodb 2>/dev/null || true')
        assert found == '', f'a password, or a file of mongosh, is on the node: {found[:600]}'
        # The wrapper saw MMX's calls of mongosh: the version question, and
        # the one fixed command line with HOME=/dev/null and nothing else of
        # the node job worker process's environment.
        calls = [record.split('\n') for record in remote(f'cat {MONGOSH_CALLS}').split('call: ')[1:]]
        programs = [record for record in calls if record[0] == 'mongosh --quiet --norc --nodb --file /dev/stdin']
        assert len(programs) > 40 and len(programs) + len([record for record in calls if record[0] == 'mongosh --version']) == len(calls), sorted({record[0] for record in calls})
        for record in programs:
            environment = dict(line.split('=', 1) for line in record[1:] if '=' in line)
            assert environment['HOME'] == '/dev/null' and environment['LANG'] == 'C.UTF-8' and environment['USER'] == 'root' and \
                set(environment) <= {'HOME', 'LANG', 'USER', 'LOGNAME', 'PATH', 'PWD', 'SHLVL', '_', 'OLDPWD'}, record
        mark('the passwords are in nothing that mmx printed or saved, in no record, journal, log of the server, or home directory of the node; mongosh wrote no file under /root; '
             'every call of mongosh had the one fixed command line and HOME=/dev/null')
    finally:
        failing = sys.exc_info()[0] is not None
        for variable in ENVIRONMENT:
            vm.ENV.pop(variable, None)
        cleanup()
        if not failing:
            guest_is_as_found()
    (workspace / 'summary.json').write_text(json.dumps(passed, indent=2) + '\n')
    print(f'{len(passed)} MongoDB workflows passed: MongoDB {server_version} on {distribution} {version}', flush=True)


if __name__ == '__main__':
    vm.run_suite(main)
