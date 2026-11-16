#!/usr/bin/env python3
"""rabbitmq_virtual_host, rabbitmq_user, rabbitmq_policy, and rabbitmq_plugin
through the public CLI, against a real RabbitMQ server on the guest, checked
with RabbitMQ's own programs and with the password hashes that the server
stores.

On every distribution the suite first runs the test without RabbitMQ's
programs. The whole test then runs on all six: on Debian 12 and 13 and
Ubuntu 24.04 and 26.04 with the distribution's own package, and on Rocky
Linux 9 and AlmaLinux 10 with the packages of the RabbitMQ project's
repositories. The example project rabbitmq-server-for-one-application
installs the server there. On ARM64, Rocky Linux and AlmaLinux run only the
test without RabbitMQ: the RabbitMQ project publishes Erlang for x86_64
only. A container is enough on every distribution."""
import base64
import hashlib
import json
import shlex
import shutil
import sys
import time

import vm

BASE = '/var/tmp/mmx-rabbitmq'
EXAMPLE = 'rabbitmq-server-for-one-application'
# The real rabbitmqctl program, run as the account rabbitmq without a shell,
# so that every argument of a check arrives as written.
RABBITMQCTL = 'runuser -u rabbitmq -- /usr/lib/rabbitmq/bin/rabbitmqctl'
# The passwords are made up. The first holds what a shell would change:
# quotes, a backslash, a dollar name, a command in backquotes, and a letter
# that is not ASCII.
PASSWORD = 'Sentinel-p4ss \'word\' "with" $HOME `id` \\ ü'
SECOND_PASSWORD = 'Second-sentinel-0815'
# A password that RabbitMQ would store without its two outer spaces.
CUT_PASSWORD = ' Sentinel with white space '
SECRETS = {'secrets_from_env': {'user_password': 'MMX_TEST_RABBITMQ_PASSWORD', 'cut_password': 'MMX_TEST_RABBITMQ_CUT_PASSWORD'}}
ENVIRONMENT = {'MMX_TEST_RABBITMQ_PASSWORD': PASSWORD, 'MMX_TEST_RABBITMQ_CUT_PASSWORD': CUT_PASSWORD, 'SHOP_RABBITMQ_PASSWORD': PASSWORD}
# Names that the rabbitmqctl command of Debian and Ubuntu would change,
# because it hands its arguments to a shell.
VIRTUAL_HOST = 'mmx a$HOME-b`id -un`-c\\d-e"f-g h'
USER = 'mmx\'s "odd" $USER `id`'
POLICY = 'mmx-limit $HOME'
# The reason when a program of RabbitMQ is missing, after the module and the
# program: it names the package rabbitmq-server, and may say where.
PROGRAM_IS_MISSING = ' is not installed; install the package rabbitmq-server'
PROGRAM_IS_MISSING_END = ' in an earlier node task'
SERVER_IS_SILENT = ': the RabbitMQ server on this node does not answer; start its service in an earlier node task (rabbitmq-diagnostics check_running said: '
STARTS_WITH_THE_SERVER = '; the RabbitMQ server on this node does not run, so the change holds from its next start'
# What RabbitMQ's packages leave behind when they are removed, and what the
# example project's two repositories put on a node.
LEFT_BY_THE_PACKAGES = ['/var/lib/rabbitmq', '/etc/rabbitmq', '/var/log/rabbitmq', '/usr/lib/rabbitmq', '/run/rabbitmq', '/usr/lib/erlang', '/usr/lib64/erlang',
                        '/etc/yum.repos.d/rabbitmq-erlang.repo', '/etc/yum.repos.d/rabbitmq-server.repo',
                        '/etc/pki/rpm-gpg/RPM-GPG-KEY-rabbitmq-erlang', '/etc/pki/rpm-gpg/RPM-GPG-KEY-rabbitmq-server', '/root/.erlang.cookie']
ACCOUNTS = ['rabbitmq', 'epmd']
# The IDs of the suite's rabbitmq_plugin tasks, whose module has no output.
PLUGIN_TASKS = {'plugin', 'shovel_management', 'agent', 'management', 'mqtt', 'shovel'}
# The names of the installed packages, and the keys that rpm trusts.
INSTALLED_PACKAGES = ("if command -v dpkg-query >/dev/null; then dpkg-query -W -f '${db:Status-Abbrev} ${Package}\\n' | awk '$1 == \"ii\" {print $2}'; "
                      "else rpm -qa --qf '%{NAME}\\n' | grep -v '^gpg-pubkey$'; fi | sort -u")
TRUSTED_RPM_KEYS = 'if command -v rpm >/dev/null; then rpm -q gpg-pubkey 2>/dev/null | grep "^gpg-pubkey-" || true; fi'


def main():
    vm.up()
    node, ssh = vm.connection()
    vm.reset()
    distribution = vm.cmd([*ssh, '. /etc/os-release; echo "$ID"']).stdout.strip()
    workspace = vm.ROOT / 'rabbitmq'
    example = vm.ROOT / ('rabbitmq-' + EXAMPLE)
    for directory in (workspace, example):
        if directory.exists():
            shutil.rmtree(directory)
    vm.write_inventory(workspace, {'nodes': {'message_broker': node}}, name='example')
    vm.build()
    mmx = vm.ROOT / 'bin/mmx'
    passed = []
    marked = time.monotonic()
    # Everything mmx printed, to search for the passwords at the end.
    printed = []

    def remote(script, input=None):
        # The checks run RabbitMQ's programs as MMX's commands run them: in
        # /, because the programs change to the account rabbitmq, which
        # cannot read the login account's home directory, and with a UTF-8
        # locale.
        return vm.cmd([*ssh, 'sudo -n /bin/sh -c ' + shlex.quote('set -eu\nexport LANG=C.UTF-8\ncd /\n' + script)], input=input).stdout.strip()

    def project(tasks, **settings):
        vm.write_project(workspace, project_name='rabbitmq', node_task_defaults={'retry': {'pause_between_tries': '0s'}},
                         fleet_stages=[{'id': 'configure', 'nodes': 'all', 'node_tasks': tasks}], **{**SECRETS, **settings})

    def run(command, expected=None, directory=None):
        result = vm.cmd([mmx, command, *vm.playbook_args(command), '--json'], cwd=directory or workspace, check=False, timeout=900)
        printed.append(result.stdout + result.stderr)
        if result.returncode not in (vm.success_codes(command) if expected is None else (expected,)):
            raise AssertionError((command, result.returncode, result.stdout, result.stderr))
        return json.loads(result.stdout)

    def results(record):
        """The node's task results, by the last part of each task's name."""
        return {r['task'].rsplit('/', 1)[-1]: r for r in record['entries'][0]['response']['results']}

    def of_the_modules(record):
        """The results of the RabbitMQ tasks of the example project, in their order: plugin, virtual host, user, policy."""
        found = [r for r in record['entries'][0]['response']['results'] if r.get('module_kind') == 'official_extra']
        assert len(found) == 4, record['entries'][0]['response']['results']
        return found

    def names_the_missing_program(result, start, earlier_task=None):
        """The whole reason names the module, the missing program, and the package rabbitmq-server; in a dryrun after an earlier node task that would change the node, it also names that node task."""
        expected = start + PROGRAM_IS_MISSING_END
        return result.get('reason', '') == (expected if earlier_task is None else vm.earlier_change_may_provide(earlier_task, expected))

    def is_result(result, status, change, reason=None, failure_kind=None, tries=None):
        """The result has this status, change, and whole reason, and is not hidden as a secret. A task of the three modules that can create their thing
        says in its output created whether it did, or in a dryrun would; a plugin task has no output."""
        assert result['status'] == status and result['change'] == change and result.get('secret') is not True, result
        assert reason is None or result.get('reason', '') == reason, (result.get('reason', ''), reason)
        if status == 'succeeded':
            of_a_plugin = result['task'].rsplit('/', 1)[-1] in PLUGIN_TASKS or 'the plugin ' in result.get('reason', '')
            created = result.get('reason', '').startswith(('created ', 'would create '))
            assert result.get('module_outputs') == (None if of_a_plugin else {'created': created}), result
        assert result.get('failure_kind') == failure_kind, result
        assert tries is None or len(result.get('tries') or [result]) == tries, result
        return True

    def unchanged(record):
        response = record['entries'][0]['response']
        assert response['change'] == 'unchanged', {r['task']: (r['status'], r['change'], r.get('reason', '')) for r in response['results']}
        return True

    def would(reason):
        """A run's reason as a dryrun gives it: "created x; set y" becomes "would create x; would set y"."""
        infinitives = {'created': 'create', 'deleted': 'delete', 'set': 'set', 'added': 'add', 'removed': 'remove', 'cleared': 'clear', 'changed': 'change',
                       'enabled': 'enable', 'disabled': 'disable'}
        return '; '.join('would ' + infinitives[part.split(' ', 1)[0]] + ' ' + part.split(' ', 1)[1] for part in reason.split('; '))

    def mark(message):
        # The seconds since the last mark say where the suite's time goes.
        nonlocal marked
        now = time.monotonic()
        passed.append(message)
        print(f'PASS {message} ({now - marked:.0f} s)', flush=True)
        marked = now

    # What the server holds, read with RabbitMQ's own programs and never
    # through MMX. Each call of rabbitmqctl starts an Erlang runtime and takes
    # about half a second, so one call reads everything.
    class Server:
        """The virtual hosts, users, permissions, and policies of the server, as rabbitmqctl export_definitions gives them at this moment."""

        def __init__(self):
            self.exported = json.loads(remote(RABBITMQCTL + ' -q export_definitions -'))
            self.virtual_hosts = {row['name'] for row in self.exported['vhosts']}
            self.tags = {row['name']: set(row['tags']) for row in self.exported['users']}

        def permissions(self, user):
            """The user's (configure, write, read) by virtual host."""
            return {row['vhost']: (row['configure'], row['write'], row['read']) for row in self.exported['permissions'] if row['user'] == user}

        def policies(self, virtual_host):
            """Each policy's (pattern, apply-to, definition, priority) in the virtual host."""
            return {row['name']: (row['pattern'], row['apply-to'], row['definition'], row['priority']) for row in self.exported['policies'] if row['vhost'] == virtual_host}

        def password_is(self, user, password):
            """Whether the hash that the server stores for the user is the hash of exactly this password: Base64 of four salt bytes and of the SHA-256 of the salt and the password."""
            entry = next(row for row in self.exported['users'] if row['name'] == user)
            assert entry['hashing_algorithm'] == 'rabbit_password_hashing_sha256', entry['hashing_algorithm']
            raw = base64.b64decode(entry['password_hash'])
            return hashlib.sha256(raw[:4] + password.encode()).digest() == raw[4:]

        def everything(self):
            """All of it in one text that is the same as long as nothing changes, with each user's password hash."""
            return json.dumps({part: sorted(json.dumps(row, sort_keys=True) for row in self.exported[part]) for part in ('vhosts', 'users', 'permissions', 'policies')})

    def plugins():
        """Each plugin of RabbitMQ: (how it is enabled, whether it runs)."""
        return {row['name']: (row['enabled'], row['running']) for row in json.loads(remote('rabbitmq-plugins -q list --formatter json'))['plugins']}

    def enabled_plugins_file():
        return remote('cat /etc/rabbitmq/enabled_plugins 2>/dev/null || true')

    def state_of_the_server():
        return Server().everything(), enabled_plugins_file()

    before = {}

    def before_the_suite():
        """What the guest had before the suite, which the cleanup must leave."""
        before['packages'] = set(remote(INSTALLED_PACKAGES).split())
        before['keys'] = set(remote(TRUSTED_RPM_KEYS).split())
        before['paths'] = remote(f"ls -d {' '.join(LEFT_BY_THE_PACKAGES)} 2>/dev/null || true").split()
        before['accounts'] = [account for account in ACCOUNTS if remote(f'getent passwd {account} || true')]

    def cleanup():
        """Remove RabbitMQ, its data, and everything of the suite, so that the next suite finds the guest as this one found it: exactly the packages that the suite's runs added go, with what they leave behind."""
        added = ' '.join(sorted(set(remote(INSTALLED_PACKAGES).split()) - before['packages']))
        keys = ' '.join(sorted(set(remote(TRUSTED_RPM_KEYS).split()) - before['keys']))
        paths = ' '.join(path for path in LEFT_BY_THE_PACKAGES if path not in before['paths'])
        accounts = ' '.join(account for account in ACCOUNTS if account not in before['accounts'])
        remove = f"""if command -v apt-get >/dev/null; then DEBIAN_FRONTEND=noninteractive apt-get purge -y -q {added} >/dev/null 2>&1 || true
else dnf remove -y -q {added} >/dev/null 2>&1 || true
fi""" if added else ''
        # epmd is Erlang's daemon that tells RabbitMQ's programs the server's
        # port. Where no unit starts it, it stays after the server stops; its
        # own command ends it, while the package that holds it is installed.
        remote(f"""
systemctl stop rabbitmq-server.service rabbitmq.service epmd.socket epmd.service 2>/dev/null || true
for epmd in /usr/lib64/erlang/erts-*/bin/epmd /usr/lib/erlang/erts-*/bin/epmd /usr/bin/epmd; do
  if [ -x "$epmd" ]; then "$epmd" -kill >/dev/null 2>&1 || true; break; fi
done
{remove}
rm -rf {BASE} {paths}
{f'rpm -e {keys}' if keys else ''}
for account in {accounts}; do userdel "$account" 2>/dev/null || true; groupdel "$account" 2>/dev/null || true; done
systemctl daemon-reload
systemctl reset-failed 2>/dev/null || true
""")

    def guest_is_as_found():
        paths = ' '.join(path for path in LEFT_BY_THE_PACKAGES + [BASE] if path not in before['paths'])
        left = remote(f"""
for program in rabbitmqctl rabbitmq-plugins rabbitmq-diagnostics erl epmd; do command -v $program || true; done
for account in {' '.join(account for account in ACCOUNTS if account not in before['accounts'])}; do getent passwd "$account" || true; getent group "$account" || true; done
ls -d {paths} /run/mmx/private_task_directories/* 2>/dev/null || true
systemctl list-unit-files 'rabbitmq*' 'epmd*' --no-legend 2>/dev/null || true
ps -e -o user=,comm= | grep -E '^(rabbitmq|epmd) | (epmd|beam[.]smp)$' || true
""")
        packages = set(remote(INSTALLED_PACKAGES).split())
        if packages != before['packages']:
            left += f"\npackages added: {sorted(packages - before['packages'])}; packages gone: {sorted(before['packages'] - packages)}"
        keys = set(remote(TRUSTED_RPM_KEYS).split())
        if keys != before['keys']:
            left += f"\nkeys that rpm trusts, added: {sorted(keys - before['keys'])}; gone: {sorted(before['keys'] - keys)}"
        state = vm.guest('sudo', '-n', 'systemctl', 'is-system-running', check=False).stdout.strip()
        if state != 'running':
            left += f'\nsystemd is {state}:\n' + vm.guest('sudo', '-n', '/bin/sh', '-c', vm.FAILED_UNITS, check=False).stdout
        assert left == '', f'the suite left this on the guest:\n{left}'
        print('The guest is as the suite found it: no RabbitMQ, no Erlang, no account, repository, or key of theirs, and systemd is running', flush=True)

    # The tasks of the suite's own project. Every name and the password hold
    # what a shell would change.
    virtual_host = {'id': 'virtual_host', 'rabbitmq_virtual_host': {'name': VIRTUAL_HOST}}
    user_inputs = {'name': USER, 'password': '{{ .secrets.user_password }}', 'tags': ['monitoring', 'management'], 'tags_not_listed': 'remove',
                   'permissions': [{'virtual_host': VIRTUAL_HOST, 'configure': '^(amq\\.gen.*|mmx\\.[a-z]+)$', 'write': '', 'read': '-.*'},
                                   {'virtual_host': '/', 'configure': '^$', 'write': '^$', 'read': '.*'}],
                   'permissions_not_listed': 'remove'}
    user = {'id': 'user', 'rabbitmq_user': user_inputs}
    definition = {'max-length': 100000, 'message-ttl': 60000, 'overflow': 'reject-publish', 'dead-letter-exchange': 'mmx a\\b "q" $HOME `id` ü'}
    policy_inputs = {'name': POLICY, 'virtual_host': VIRTUAL_HOST, 'pattern': '-mmx\\..*', 'apply_to': 'queues', 'priority': -3, 'definition': definition}
    policy = {'id': 'policy', 'rabbitmq_policy': policy_inputs}

    def plugin(name, state, task='plugin'):
        return {'id': task, 'rabbitmq_plugin': {'name': name, 'state': state}}
    user_called = f'the user {USER}'
    on_the_virtual_host = f' on the virtual host {VIRTUAL_HOST}'
    policy_called = f'the policy {POLICY} of the virtual host {VIRTUAL_HOST}'

    def server_does_not_answer():
        """With the server stopped: rabbitmq_plugin works, and the change holds from the server's next start; the three other modules are not checked in a dryrun and fail in a run."""
        check_running = remote('rabbitmq-diagnostics -q check_running 2>&1 || true')
        assert check_running.startswith('Error: unable to perform an operation on node'), check_running
        said = check_running.split('\n')[0]
        file_before = enabled_plugins_file()
        project([virtual_host, user, policy, plugin('rabbitmq_mqtt', 'enabled')])
        previewed = results(run('dryrun', 3))
        for task, module in (('virtual_host', 'rabbitmq_virtual_host'), ('user', 'rabbitmq_user'), ('policy', 'rabbitmq_policy')):
            is_result(previewed[task], 'not_checked', 'unknown', reason=module + SERVER_IS_SILENT + said + ')')
        is_result(previewed['plugin'], 'succeeded', 'would_change', reason='would enable the plugin rabbitmq_mqtt' + STARTS_WITH_THE_SERVER)
        assert enabled_plugins_file() == file_before
        # The plugin task works; the task after it fails as an ordinary
        # failure, which MMX tries three times.
        project([plugin('rabbitmq_mqtt', 'enabled'), virtual_host])
        applied = results(run('run', 1))
        is_result(applied['plugin'], 'succeeded', 'changed', reason='enabled the plugin rabbitmq_mqtt' + STARTS_WITH_THE_SERVER)
        is_result(applied['virtual_host'], 'failed', 'unchanged', tries=3)
        assert applied['virtual_host']['reason'].endswith('rabbitmq_virtual_host' + SERVER_IS_SILENT + said + ')'), applied['virtual_host']
        for task, module in ((user, 'rabbitmq_user'), (policy, 'rabbitmq_policy')):
            project([{**task, 'retry': {'max_tries': 1}}])
            failed = results(run('run', 1))[task['id']]
            is_result(failed, 'failed', 'unchanged', tries=1)
            assert failed['reason'].endswith(module + SERVER_IS_SILENT + said + ')'), failed
        assert plugins()['rabbitmq_mqtt'] == ('enabled', False) and 'rabbitmq_mqtt' in enabled_plugins_file(), enabled_plugins_file()
        # Still stopped: disabling a plugin also disables the plugins that
        # need it, and a plugin that another one needs is enabled by name.
        # RabbitMQ answers both from its files alone. rabbitmq_web_mqtt needs
        # rabbitmq_mqtt in every version, and no other plugin needs either.
        project([plugin('rabbitmq_web_mqtt', 'enabled', 'web_mqtt')])
        is_result(results(run('run'))['web_mqtt'], 'succeeded', 'changed', reason='enabled the plugin rabbitmq_web_mqtt' + STARTS_WITH_THE_SERVER)
        project([plugin('rabbitmq_mqtt', 'disabled_with_plugins_that_depend_on_it', 'mqtt')])
        also = '; RabbitMQ {} the plugins that need it: rabbitmq_web_mqtt' + STARTS_WITH_THE_SERVER
        is_result(results(run('dryrun'))['mqtt'], 'succeeded', 'would_change', reason='would disable the plugin rabbitmq_mqtt' + also.format('would also disable'))
        is_result(results(run('run'))['mqtt'], 'succeeded', 'changed', reason='disabled the plugin rabbitmq_mqtt' + also.format('also disabled'))
        assert enabled_plugins_file() == '[rabbitmq_management_agent].', enabled_plugins_file()
        project([plugin('rabbitmq_web_mqtt', 'enabled', 'web_mqtt'), plugin('rabbitmq_mqtt', 'enabled', 'mqtt')])
        applied = results(run('run'))
        is_result(applied['web_mqtt'], 'succeeded', 'changed', reason='enabled the plugin rabbitmq_web_mqtt' + STARTS_WITH_THE_SERVER)
        is_result(applied['mqtt'], 'succeeded', 'changed',
                  reason='enabled the plugin rabbitmq_mqtt by name, which was on only because another plugin needs it' + STARTS_WITH_THE_SERVER)
        assert unchanged(run('run'))
        project([plugin('rabbitmq_web_mqtt', 'disabled_with_plugins_that_depend_on_it', 'web_mqtt')])
        is_result(results(run('run'))['web_mqtt'], 'succeeded', 'changed', reason='disabled the plugin rabbitmq_web_mqtt' + STARTS_WITH_THE_SERVER)
        assert enabled_plugins_file() == '[rabbitmq_management_agent,rabbitmq_mqtt].', enabled_plugins_file()

    def whole_test():
        """Against a running server: the example project, then each state, change, failure, and removal."""
        # The example project: its first dryrun passes although nothing is
        # installed, and its run installs the server and configures it.
        shutil.copytree(vm.REPO / 'examples' / EXAMPLE, example)
        vm.write_inventory(example, {'nodes': {'message_broker': node}, 'vars': {'application_virtual_host': 'shop', 'application_user': 'shop'}}, name='example')
        for result in of_the_modules(run('dryrun', 3, directory=example)):
            assert result['status'] == 'not_checked' and PROGRAM_IS_MISSING in result['reason'], result
        started = time.monotonic()
        applied = of_the_modules(run('run', directory=example))
        seconds = time.monotonic() - started
        version = remote(RABBITMQCTL + ' version')
        number = tuple(int(part) for part in version.split('.')[:2])
        print(f"RabbitMQ {version}, {remote('rabbitmq-diagnostics -q erlang_version')}: the example project installed, started, and configured it in {seconds:.0f} s", flush=True)
        for result, reason in zip(applied, ('enabled the plugin rabbitmq_management', 'created the virtual host shop',
                                            'created the user shop; set the permissions of the user shop on the virtual host shop',
                                            'created the policy limit-queue-length of the virtual host shop')):
            is_result(result, 'succeeded', 'changed', reason=reason)
        assert remote('systemctl is-active rabbitmq-server; systemctl is-enabled rabbitmq-server') == 'active\nenabled'
        assert plugins()['rabbitmq_management'] == ('enabled', True) and enabled_plugins_file() == '[rabbitmq_management].', enabled_plugins_file()
        # The server listens for applications, and the management plugin's
        # web interface listens too.
        assert remote("for port in 5672 15672; do ss -Hltn \"sport = :$port\" | head -n 1 | wc -l; done") == '1\n1', remote('ss -Hltn')
        server = Server()
        assert server.virtual_hosts == {'/', 'shop'} and server.tags == {'guest': {'administrator'}, 'shop': set()}, server.exported
        # What the example's page says about the user that every new server has.
        assert server.password_is('guest', 'guest') and server.permissions('guest') == {'/': ('.*', '.*', '.*')}, server.permissions('guest')
        assert server.permissions('shop') == {'shop': ('.*', '.*', '.*')} and server.password_is('shop', PASSWORD), server.permissions('shop')
        assert server.policies('shop') == {'limit-queue-length': ('.*', 'queues', {'max-length': 100000}, 0)}, server.policies('shop')
        assert unchanged(run('run', directory=example))
        # A new password in the environment: a dryrun says so, finds every
        # other task unchanged, and changes nothing; a run sets the password
        # and nothing else.
        vm.ENV['SHOP_RABBITMQ_PASSWORD'] = SECOND_PASSWORD
        for command, change, reason in (('dryrun', 'would_change', 'would set the password of the user shop'), ('run', 'changed', 'set the password of the user shop')):
            differing = [r for r in run(command, directory=example)['entries'][0]['response']['results'] if r['change'] != 'unchanged']
            assert len(differing) == 1 and is_result(differing[0], 'succeeded', change, reason=reason), differing
            assert Server().password_is('shop', PASSWORD if command == 'dryrun' else SECOND_PASSWORD)
        mark('the example ' + EXAMPLE + ': its first dryrun leaves the four RabbitMQ tasks not checked, its run installs and starts the server, enables the '
             'management plugin, and creates the virtual host, the user with its permissions, and the policy; an unchanged rerun; '
             'a new password is all that a dryrun announces and all that a run changes, and the server stores exactly that password')

        # The first run of the suite's own tasks. Every name holds what a
        # shell would change, so each must arrive at RabbitMQ as written.
        first = [virtual_host, user, policy, plugin('rabbitmq_shovel_management', 'enabled', 'shovel_management'), plugin('rabbitmq_management_agent', 'enabled', 'agent')]
        project(first)
        untouched = state_of_the_server()
        previewed = results(run('dryrun'))
        is_result(previewed['virtual_host'], 'succeeded', 'would_change', reason=f'would create the virtual host {VIRTUAL_HOST}')
        # The virtual host does not exist yet, so the two tasks that need it
        # cannot be checked.
        # The task virtual_host would change the node first and may provide the host.
        is_result(previewed['user'], 'not_checked', 'unknown', reason=vm.earlier_change_may_provide('virtual_host', f'rabbitmq_user: the virtual host {VIRTUAL_HOST} does not exist; create it with a rabbitmq_virtual_host task in an earlier node task'))
        is_result(previewed['policy'], 'not_checked', 'unknown', reason=vm.earlier_change_may_provide('virtual_host', f'rabbitmq_policy: the virtual host {VIRTUAL_HOST} does not exist; create it with a rabbitmq_virtual_host task in an earlier node task'))
        is_result(previewed['shovel_management'], 'succeeded', 'would_change', reason='would enable the plugin rabbitmq_shovel_management')
        is_result(previewed['agent'], 'succeeded', 'would_change', reason='would enable the plugin rabbitmq_management_agent by name, which was on only because another plugin needs it')
        assert state_of_the_server() == untouched
        applied = results(run('run'))
        assert all(result['module_kind'] == 'official_extra' for result in applied.values()), applied
        is_result(applied['virtual_host'], 'succeeded', 'changed', reason=f'created the virtual host {VIRTUAL_HOST}')
        is_result(applied['user'], 'succeeded', 'changed', reason=f'created {user_called}; added the tags monitoring, management to {user_called}; '
                  f'set the permissions of {user_called}{on_the_virtual_host}; set the permissions of {user_called} on the virtual host /')
        assert f'ran: {RABBITMQCTL} add_user {USER}\n' in applied['user']['diff'], applied['user']
        is_result(applied['policy'], 'succeeded', 'changed', reason=f'created {policy_called}')
        is_result(applied['shovel_management'], 'succeeded', 'changed', reason='enabled the plugin rabbitmq_shovel_management')
        is_result(applied['agent'], 'succeeded', 'changed', reason='enabled the plugin rabbitmq_management_agent by name, which was on only because another plugin needs it')
        server = Server()
        assert server.virtual_hosts == {'/', 'shop', VIRTUAL_HOST} and server.tags[USER] == {'monitoring', 'management'}, (server.virtual_hosts, server.tags)
        assert server.permissions(USER) == {VIRTUAL_HOST: ('^(amq\\.gen.*|mmx\\.[a-z]+)$', '', '-.*'), '/': ('^$', '^$', '.*')}, server.permissions(USER)
        assert server.password_is(USER, PASSWORD)
        assert server.policies(VIRTUAL_HOST) == {POLICY: ('-mmx\\..*', 'queues', definition, -3)}, server.policies(VIRTUAL_HOST)
        assert enabled_plugins_file() == '[rabbitmq_management,rabbitmq_management_agent,rabbitmq_shovel_management].', enabled_plugins_file()
        running = plugins()
        assert running['rabbitmq_shovel_management'] == ('enabled', True) and running['rabbitmq_shovel'] == ('implicit', True) and running['rabbitmq_management_agent'] == ('enabled', True), running
        assert unchanged(run('run'))
        mark(f'the first run on RabbitMQ {version}: a virtual host, a user, and a policy whose names, expressions, password, and definition hold what a shell would change '
             'arrive as written; a plugin that was off and a plugin that another one needed are enabled by name; a dryrun before changes nothing and leaves the tasks '
             'that need the new virtual host not checked; an unchanged rerun')

        # Each kind of change, and a state made by hand that a run finishes.
        # A second user, made by hand with another password, a tag, and a
        # permission: keep adds to what it has and takes nothing away, and a
        # task without password leaves the password alone.
        remote(f"printf 'made by hand\\n' | {RABBITMQCTL} add_user mmx-by-hand >/dev/null\n{RABBITMQCTL} -q set_user_tags mmx-by-hand administrator\n"
               f"{RABBITMQCTL} -q set_permissions -p / mmx-by-hand a b c")
        vm.ENV['MMX_TEST_RABBITMQ_PASSWORD'] = SECOND_PASSWORD
        changed_user = {**user_inputs, 'tags': ['management', 'policymaker'],
                        'permissions': [{'virtual_host': VIRTUAL_HOST, 'configure': '^(amq\\.gen.*|mmx\\.[a-z]+)$', 'write': '', 'read': '^mmx'}]}
        changed_policy = {**policy_inputs, 'pattern': '^mmx\\.', 'apply_to': 'all', 'priority': 7, 'definition': {**definition, 'max-length': 5}}
        by_hand = {'id': 'by_hand', 'rabbitmq_user': {'name': 'mmx-by-hand', 'tags': ['monitoring'], 'tags_not_listed': 'keep',
                                                     'permissions': [{'virtual_host': VIRTUAL_HOST, 'configure': '', 'write': '', 'read': '.*'}], 'permissions_not_listed': 'keep'}}
        project([{'id': 'user', 'rabbitmq_user': changed_user}, {'id': 'policy', 'rabbitmq_policy': changed_policy}, by_hand])
        reasons = {'user': f'set the password of {user_called}; added the tag policymaker to {user_called}; removed the tag monitoring from {user_called}; '
                           f'set the permissions of {user_called}{on_the_virtual_host}; cleared the permissions of {user_called} on the virtual host /',
                   'policy': f'changed {policy_called}: pattern, apply_to, definition, priority',
                   'by_hand': f'added the tag monitoring to the user mmx-by-hand; set the permissions of the user mmx-by-hand{on_the_virtual_host}'}
        untouched = state_of_the_server()
        previewed = results(run('dryrun'))
        for task, reason in reasons.items():
            is_result(previewed[task], 'succeeded', 'would_change', reason=would(reason))
        assert state_of_the_server() == untouched
        applied = results(run('run'))
        for task, reason in reasons.items():
            is_result(applied[task], 'succeeded', 'changed', reason=reason)
        server = Server()
        assert server.password_is(USER, SECOND_PASSWORD) and server.tags[USER] == {'management', 'policymaker'}, server.tags
        assert server.permissions(USER) == {VIRTUAL_HOST: ('^(amq\\.gen.*|mmx\\.[a-z]+)$', '', '^mmx')}, server.permissions(USER)
        assert server.policies(VIRTUAL_HOST) == {POLICY: ('^mmx\\.', 'all', {**definition, 'max-length': 5}, 7)}, server.policies(VIRTUAL_HOST)
        assert server.tags['mmx-by-hand'] == {'administrator', 'monitoring'} and server.password_is('mmx-by-hand', 'made by hand')
        assert server.permissions('mmx-by-hand') == {'/': ('a', 'b', 'c'), VIRTUAL_HOST: ('', '', '.*')}, server.permissions('mmx-by-hand')
        assert unchanged(run('run'))
        mark('changes: another password, tags and permissions with remove and with keep, a policy whose every part differs; '
             'a user made by hand keeps its password, its other tag, and its other permission; a dryrun before changes nothing; an unchanged rerun')

        # A run that stops between two changes: RabbitMQ refuses a regular
        # expression after the user was created. Another try would get the
        # same answer, so MMX tries once. The next run does only what is
        # missing.
        later = {'name': 'mmx-later', 'password': '{{ .secrets.user_password }}', 'permissions_not_listed': 'keep'}
        project([{'id': 'later', 'rabbitmq_user': {**later, 'permissions': [{'virtual_host': VIRTUAL_HOST, 'configure': '*[', 'write': '.*', 'read': '.*'}]}}])
        # A dryrun cannot know whether RabbitMQ accepts an expression.
        is_result(results(run('dryrun'))['later'], 'succeeded', 'would_change', reason=f'would create the user mmx-later; would set the permissions of the user mmx-later{on_the_virtual_host}')
        stopped = results(run('run', 1))['later']
        is_result(stopped, 'failed', 'changed', failure_kind='config', tries=1)
        assert stopped['reason'].startswith(f'rabbitmq_user: RabbitMQ refuses a regular expression of the permissions of the user mmx-later{on_the_virtual_host}: ') and (
            'invalid_regexp' in stopped['reason']), stopped
        server = Server()
        assert server.permissions('mmx-later') == {} and server.password_is('mmx-later', SECOND_PASSWORD)
        project([{'id': 'later', 'rabbitmq_user': {**later, 'permissions': [{'virtual_host': VIRTUAL_HOST, 'configure': '.*', 'write': '.*', 'read': '.*'}]}}])
        is_result(results(run('run'))['later'], 'succeeded', 'changed', reason=f'set the permissions of the user mmx-later{on_the_virtual_host}')
        assert Server().permissions('mmx-later') == {VIRTUAL_HOST: ('.*', '.*', '.*')}
        mark('a regular expression that RabbitMQ refuses after the user was created fails the run once, with RabbitMQ\'s message, as a configuration error that reports '
             'the change; the next run sets only the permissions')

        # What a task cannot do. A dryrun reports a thing that an earlier
        # node task may still provide as not checked: a virtual host, a
        # plugin's file, and a user for a task that has no password to create
        # it. A broken rule is a failure in a dryrun too.
        no_virtual_host = ' the virtual host mmx-not-there does not exist; create it with a rabbitmq_virtual_host task in an earlier node task'
        no_plugin = ('rabbitmq_plugin: RabbitMQ on this node has no plugin mmx_made_up_plugin; the command rabbitmq-plugins list shows the names. '
                     'A plugin that is not part of RabbitMQ needs its file in RabbitMQ\'s plugins directory, from an earlier node task')
        failing = {
            'no_virtual_host_for_user': ({'rabbitmq_user': {**later, 'permissions': [{'virtual_host': 'mmx-not-there', 'configure': '', 'write': '', 'read': ''}]}},
                                         'rabbitmq_user:' + no_virtual_host, 'missing_prerequisite'),
            'no_virtual_host_for_policy': ({'rabbitmq_policy': {**policy_inputs, 'virtual_host': 'mmx-not-there'}}, 'rabbitmq_policy:' + no_virtual_host, 'missing_prerequisite'),
            'no_plugin': ({'rabbitmq_plugin': {'name': 'mmx_made_up_plugin', 'state': 'enabled'}}, no_plugin, 'missing_prerequisite'),
            'no_password': ({'rabbitmq_user': {'name': 'mmx-forgotten-password'}},
                            'rabbitmq_user: the user mmx-forgotten-password does not exist, and the task has no password to create it; '
                            'add password, or create the user with a rabbitmq_user task in an earlier node task', 'missing_prerequisite'),
            'cut_password': ({'rabbitmq_user': {'name': 'mmx-forgotten-password', 'password': '{{ .secrets.cut_password }}'}},
                             'rabbitmq_user.password: must not start or end with white space: RabbitMQ would cut it off and store another password', 'config'),
        }
        untouched = state_of_the_server()
        # One dryrun for the four that are not checked; a failure ends a
        # node job, so each failure has its own command.
        project([{'id': task, **inputs} for task, (inputs, _, kind) in failing.items() if kind == 'missing_prerequisite'])
        previewed = results(run('dryrun', 3))
        first_task = next(iter(failing))
        for task, (inputs, reason, kind) in failing.items():
            project([{'id': task, **inputs}])
            if kind == 'missing_prerequisite':
                # In the dryrun of the four, the first task is not checked and so may change the node: each later one names it.
                is_result(previewed[task], 'not_checked', 'unknown', reason=reason if task == first_task else vm.earlier_change_may_provide(first_task, reason))
            else:
                is_result(results(run('dryrun', 1))[task], 'failed', 'unchanged', reason=reason, failure_kind='config', tries=1)
            if task != 'cut_password':
                is_result(results(run('run', 1))[task], 'failed', 'unchanged', reason=reason, failure_kind=kind, tries=1)
        assert state_of_the_server() == untouched and 'mmx_made_up_plugin' not in enabled_plugins_file()
        # A value of apply_to that RabbitMQ takes from 3.12 on. The module
        # has no list of versions: RabbitMQ's own answer decides.
        classic = {'id': 'classic', 'rabbitmq_policy': {'priority': 0, 'name': 'mmx-classic', 'virtual_host': VIRTUAL_HOST, 'pattern': '.*', 'apply_to': 'classic_queues', 'definition': {'max-length': 5}}}
        project([classic])
        if number >= (3, 12):
            is_result(results(run('run'))['classic'], 'succeeded', 'changed', reason=f'created the policy mmx-classic of the virtual host {VIRTUAL_HOST}')
            assert Server().policies(VIRTUAL_HOST)['mmx-classic'] == ('.*', 'classic_queues', {'max-length': 5}, 0)
        else:
            is_result(results(run('run', 1))['classic'], 'failed', 'changed', failure_kind='config', tries=1,
                      reason=f"rabbitmq_policy: RabbitMQ refuses the policy mmx-classic of the virtual host {VIRTUAL_HOST}: "
                             "Validation failed: apply-to 'classic_queues' unrecognised; should be 'queues', 'exchanges' or 'all'")
            assert 'mmx-classic' not in Server().policies(VIRTUAL_HOST)
        print(f"RabbitMQ {version} {'takes' if number >= (3, 12) else 'refuses'} apply_to: classic_queues", flush=True)
        # A key that no RabbitMQ knows: the policy that was there before stays.
        project([{'id': 'policy', 'rabbitmq_policy': {**changed_policy, 'definition': {'mmx-made-up-key': 1}}}])
        is_result(results(run('run', 1))['policy'], 'failed', 'changed', failure_kind='config', tries=1,
                  reason=f'rabbitmq_policy: RabbitMQ refuses {policy_called}: Validation failed: [{{<<"mmx-made-up-key">>,1}}] are not recognised policy settings')
        assert Server().policies(VIRTUAL_HOST)[POLICY] == ('^mmx\\.', 'all', {**definition, 'max-length': 5}, 7)
        mark('failures: a permission and a policy for a virtual host that does not exist, a plugin that RabbitMQ does not have, and a task without a password '
             'for a user that does not exist are missing prerequisites (dryrun: not checked); a password that RabbitMQ would cut is a configuration error in a dryrun; '
             'apply_to: classic_queues by what this RabbitMQ answers, and a policy key that no RabbitMQ knows, each refused once with RabbitMQ\'s own reason; '
             'nothing changed on the server')

        # Disabling a plugin: RabbitMQ also disables the enabled plugins that
        # need it, and the reason names them. The plugin that the task above
        # enabled by name stays on.
        project([plugin('rabbitmq_management', 'disabled_with_plugins_that_depend_on_it', 'management'), plugin('rabbitmq_mqtt', 'disabled_with_plugins_that_depend_on_it', 'mqtt')])
        also = '; RabbitMQ {} the plugins that need it: rabbitmq_shovel_management'
        previewed = results(run('dryrun'))
        is_result(previewed['management'], 'succeeded', 'would_change', reason='would disable the plugin rabbitmq_management' + also.format('would also disable'))
        assert plugins()['rabbitmq_management'] == ('enabled', True)
        applied = results(run('run'))
        is_result(applied['management'], 'succeeded', 'changed', reason='disabled the plugin rabbitmq_management' + also.format('also disabled'))
        is_result(applied['mqtt'], 'succeeded', 'unchanged', reason='')
        running = plugins()
        assert enabled_plugins_file() == '[rabbitmq_management_agent].' and running['rabbitmq_management_agent'] == ('enabled', True), (enabled_plugins_file(), running)
        assert running['rabbitmq_management'] == running['rabbitmq_shovel_management'] == running['rabbitmq_shovel'] == ('not_enabled', False), running
        assert unchanged(run('run'))
        mark('disabling a plugin also disables the enabled plugin that needs it, which a dryrun and a run name; '
             'the plugin that was enabled by name stays on and runs; a plugin that is off is unchanged')

        # The server stopped, and then started again: the plugin that was
        # enabled meanwhile runs.
        remote('systemctl stop rabbitmq-server')
        server_does_not_answer()
        remote('systemctl start rabbitmq-server')
        assert plugins()['rabbitmq_mqtt'] == ('enabled', True), plugins()['rabbitmq_mqtt']
        project([virtual_host, plugin('rabbitmq_mqtt', 'enabled')])
        assert unchanged(run('run'))
        mark('a stopped server: the three modules that need it are not checked in a dryrun and fail in a run with what rabbitmq-diagnostics says, tried three times; '
             'rabbitmq_plugin enables a plugin, which runs after the next start, enables by name a plugin that another one needs, and disables a plugin '
             'together with the plugins that need it; after the start the tasks are unchanged')

        # The passwords, in everything of this suite.
        saved = ''.join(path.read_text(errors='replace') for directory in vm.ROOT.glob('rabbitmq*') for path in (directory / '.mmx').rglob('*') if path.is_file())
        for password in (PASSWORD, SECOND_PASSWORD, CUT_PASSWORD.strip()):
            for where, text in (('what mmx printed', ''.join(printed)), ('the saved runs', saved)):
                assert password not in text and json.dumps(password)[1:-1] not in text, f'a password is in {where}: ...{text[max(0, text.find(password) - 300):text.find(password) + 60]}'
        remote(f'cat > {BASE}/patterns', input='\n'.join((PASSWORD, SECOND_PASSWORD, CUT_PASSWORD.strip())) + '\n')
        # The node's job records, MMX's directories under /run, the home
        # directories, the journal, and the server's own logs.
        found = remote(f'grep -rlF -f {BASE}/patterns /var/lib/mmx /run/mmx /root /home /tmp /var/log/rabbitmq /var/lib/rabbitmq/.erlang.cookie 2>/dev/null || true\n'
                       f'journalctl --no-pager -q 2>/dev/null | grep -F -f {BASE}/patterns || true')
        assert found == '', f'a password is on the node: {found[:600]}'
        assert remote('ls -A /root | grep -i erlang || true') == '', 'a program of RabbitMQ wrote into /root'
        mark('the passwords are in nothing that mmx printed or saved, in no job record, journal, or home directory of the node, and in no log of the server')

        # Removal.
        gone = [{'id': 'policy', 'rabbitmq_policy': {'name': POLICY, 'virtual_host': VIRTUAL_HOST, 'state': 'absent'}},
                {'id': 'user', 'rabbitmq_user': {'name': USER, 'state': 'absent'}},
                {'id': 'virtual_host', 'rabbitmq_virtual_host': {'name': VIRTUAL_HOST, 'state': 'deleted_with_all_its_data'}},
                plugin('rabbitmq_mqtt', 'disabled_with_plugins_that_depend_on_it'),
                # A policy that a virtual host never had.
                {'id': 'no_policy', 'rabbitmq_policy': {'name': 'mmx-never-there', 'virtual_host': 'shop', 'state': 'absent'}}]
        project(gone)
        reasons = {'policy': f'removed {policy_called}', 'user': f'removed {user_called}', 'virtual_host': f'deleted the virtual host {VIRTUAL_HOST} with all its data',
                   'plugin': 'disabled the plugin rabbitmq_mqtt'}
        untouched = state_of_the_server()
        previewed = results(run('dryrun'))
        for task, reason in reasons.items():
            is_result(previewed[task], 'succeeded', 'would_change', reason=would(reason))
        assert state_of_the_server() == untouched
        applied = results(run('run'))
        for task, reason in reasons.items():
            is_result(applied[task], 'succeeded', 'changed', reason=reason)
        is_result(applied['no_policy'], 'succeeded', 'unchanged', reason='')
        # The virtual host took its policies and every user's permissions on
        # it along.
        server = Server()
        assert server.virtual_hosts == {'/', 'shop'} and USER not in server.tags and server.permissions('mmx-by-hand') == {'/': ('a', 'b', 'c')}, server.exported
        assert server.policies(VIRTUAL_HOST) == {} and server.permissions('mmx-later') == {}
        assert enabled_plugins_file() == '[rabbitmq_management_agent].' and plugins()['rabbitmq_mqtt'] == ('not_enabled', False), enabled_plugins_file()
        # The second run finds the policy's virtual host, the user, and the
        # virtual host gone, and the plugin off.
        assert unchanged(run('run'))
        mark('removal: a policy and a user are removed, a virtual host is deleted with its policies and the permissions on it, a plugin is disabled; '
             'a dryrun before changes nothing and says what a run would remove; a second run finds everything gone and is unchanged')

    assert remote('for program in rabbitmqctl rabbitmq-plugins rabbitmq-diagnostics; do command -v $program || true; done') == '', 'the guest already has RabbitMQ'
    before_the_suite()
    try:
        vm.ENV.update(ENVIRONMENT)
        remote(f'mkdir -p {BASE}\nchmod 700 {BASE}')

        # Before RabbitMQ is installed: each module names the program that it
        # misses and the package. A dryrun is not checked, because an earlier
        # node task may install the package; a run fails without another try.
        missing = {'virtual_host': 'rabbitmq_virtual_host: the program /usr/lib/rabbitmq/bin/rabbitmqctl' + PROGRAM_IS_MISSING,
                   'user': 'rabbitmq_user: the program /usr/lib/rabbitmq/bin/rabbitmqctl' + PROGRAM_IS_MISSING,
                   'policy': 'rabbitmq_policy: the program /usr/lib/rabbitmq/bin/rabbitmqctl' + PROGRAM_IS_MISSING,
                   'plugin': 'rabbitmq_plugin: the program rabbitmq-plugins' + PROGRAM_IS_MISSING}
        tasks = [virtual_host, user, policy, plugin('rabbitmq_management', 'enabled')]
        project(tasks)
        previewed = results(run('dryrun', 3))
        for task in tasks:
            is_result(previewed[task['id']], 'not_checked', 'unknown')
            # The first task has nothing before it; each later one names the first, which is not checked either and so may change the node.
            assert names_the_missing_program(previewed[task['id']], missing[task['id']], None if task is tasks[0] else 'virtual_host'), previewed[task['id']]
            project([task])
            refused = results(run('run', 1))[task['id']]
            is_result(refused, 'failed', 'unchanged', failure_kind='missing_prerequisite', tries=1)
            assert names_the_missing_program(refused, missing[task['id']]), refused
        mark('without RabbitMQ, each of the four modules names the missing program and its package; a dryrun is not checked, and a run fails once as a missing prerequisite')

        # The RabbitMQ project's Erlang repository holds packages for x86_64
        # only.
        if distribution in ('rocky', 'almalinux') and vm.architecture() == 'arm64':
            print(f'{distribution}: the suite installs no RabbitMQ here: the RabbitMQ project publishes Erlang for x86_64 only', flush=True)
        else:
            whole_test()
    finally:
        failing = sys.exc_info()[0] is not None
        for variable in ENVIRONMENT:
            vm.ENV.pop(variable, None)
        cleanup()
        if not failing:
            guest_is_as_found()
            mark('the cleanup: RabbitMQ, Erlang, their data, accounts, repositories, and keys are gone')
    (workspace / 'summary.json').write_text(json.dumps(passed, indent=2) + '\n')
    print(f'{len(passed)} RabbitMQ workflows passed on {distribution}', flush=True)


if __name__ == '__main__':
    vm.run_suite(main)
