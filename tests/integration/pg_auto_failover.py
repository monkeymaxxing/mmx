#!/usr/bin/env python3
"""pg_auto_failover_monitor, pg_auto_failover_node, and
pg_auto_failover_primary through the public CLI, on three guests: the
monitor on mmx-third and the database servers db01 on mmx-dev and db02 on
mmx-peer. The suite checks the formation with pg_autoctl, psql, and
systemctl themselves.

The whole test runs on Debian 12 and 13 and Ubuntu 24.04 and 26.04 with the
distribution's own packages: pg_auto_failover 2.0 to 2.2 with PostgreSQL 15
to 18. It first runs without pg_autoctl: a dryrun is not checked and names
the program, and a run fails once with a missing prerequisite. A container
is enough on every distribution.

Every instance listens on its guest's address alone (listen_address), and
its pg_hba.conf asks who logs in on the local socket and the loopback
addresses: the account nobody stands for any other account of a server. A
real failover stops db01's service, keeper and PostgreSQL, and db01 comes
back as a replica."""
import ipaddress
import json
import shlex
import sys
import time

import vm

# The guest of each server of the formation.
GUESTS = {'monitor': 'mmx-third', 'db01': 'mmx-dev', 'db02': 'mmx-peer'}
# The PostgreSQL of each distribution, which its pg_auto_failover packages
# are built for.
VERSIONS = {('debian', '12'): 15, ('debian', '13'): 17, ('ubuntu', '24.04'): 16, ('ubuntu', '26.04'): 18}
# Made-up passwords with what PostgreSQL's password file escapes, a colon
# and a backslash, and with quotes and a letter that is not ASCII.
AUTOCTL_PASSWORD = 'Sentinel autoctl:pass\\word "q" \'s ü-1'
REPLICATION_PASSWORD = 'Sentinel repl:ica\\tor "q" ü-2'
SECOND_AUTOCTL_PASSWORD = 'Second-autoctl-sentinel-3'
SECOND_REPLICATION_PASSWORD = 'Second-replication-sentinel-4'
SECRETS = {'secrets_from_env': {'autoctl_node_password': 'MMX_TEST_AUTOCTL_NODE_PASSWORD',
                                'pgautofailover_replicator_password': 'MMX_TEST_REPLICATION_PASSWORD'}}
MISSING_PG_AUTOCTL = ('{module}: the program pg_autoctl is not installed; install the package pg-auto-failover-cli in an earlier node task')
UNIT = '/etc/systemd/system/pgautofailover.service'
# pg_auto_failover_node's file of PostgreSQL settings.
SETTINGS_FILE = '/etc/postgresql/pg_auto_failover_node_settings.conf'
# What the packages and the suite leave on a guest, which the cleanup removes.
LEFT_BEHIND = ['/var/lib/postgresql', '/etc/postgresql', '/etc/postgresql-common', '/var/log/postgresql', UNIT, '/var/tmp/mmx-pg-auto-failover']
INSTALLED_PACKAGES = ("dpkg-query -W -f '${db:Status-Abbrev} ${Package}\\n' | awk '$1 == \"ii\" {print $2}' | sort -u")
# The states that the monitor assigns to a formation's primary.
PRIMARY_STATES = ('single', 'wait_primary', 'primary', 'join_primary', 'apply_settings')


def main():
    nodes, connections = {}, {}
    for server, guest in GUESTS.items():
        vm.up(guest)
        nodes[server], connections[server] = vm.connection(guest)
        vm.reset(guest)
    distribution, release = vm.cmd([*connections['db01'], '. /etc/os-release; echo "$ID $VERSION_ID"']).stdout.split()
    version = VERSIONS[(distribution, release)]
    data = f'/var/lib/postgresql/{version}/main'
    monitor_data = f'/var/lib/postgresql/{version}/monitor'
    binaries = f'/usr/lib/postgresql/{version}/bin'
    address = {server: vm.cmd([*connections[server], "ip -4 -o route get 10.0.0.1 | sed -n 's/.* src \\([0-9.]*\\).*/\\1/p'"]).stdout.strip()
               for server in GUESTS}
    # pg_autoctl create monitor lets autoctl_node log in only from the
    # subnet of the monitor's address, and pg_autoctl create postgres
    # retries a refused login without end. Test machines that share no
    # subnet, such as cloud servers with a public /32 address each, would
    # keep the first run waiting until the suite's time limit.
    monitor_network = next(ipaddress.ip_interface(line.split()[3]).network
                           for line in vm.cmd([*connections['monitor'], 'ip -4 -o address show']).stdout.splitlines()
                           if line.split()[3].split('/')[0] == address['monitor'])
    outside = [f'{server} ({address[server]})' for server in ('db01', 'db02') if ipaddress.ip_address(address[server]) not in monitor_network]
    if outside:
        raise SystemExit(f'pg_auto_failover.py needs test machines on one subnet: the monitor has {monitor_network}, which leaves out '
                         f'{" and ".join(outside)}, so the nodes could not log in to the monitor as autoctl_node')
    workspace = vm.ROOT / 'pg-auto-failover'

    def write_inventory():
        vm.write_inventory(workspace, {'nodes': nodes, 'groups': {'formation': {'nodes': list(GUESTS), 'vars': {'postgresql_major_version': version}}}})

    write_inventory()
    vm.build()
    mmx = vm.ROOT / 'bin/mmx'
    passed = []
    printed = []
    marked = time.monotonic()
    state = {}

    def remote(server, script, input=None, check=True):
        return vm.cmd([*connections[server], 'sudo -n /bin/bash -c ' + shlex.quote('set -eu\n' + script)], input=input, check=check, timeout=600).stdout.strip()

    def mark(message):
        nonlocal marked
        now = time.monotonic()
        passed.append(message)
        print(f'PASS {message} ({now - marked:.0f} s)', flush=True)
        marked = now

    def project(*stages):
        vm.write_project(workspace, project_name='pg_auto_failover', node_task_defaults={'retry': {'pause_between_tries': '0s'}},
                         fleet_stages=[{'id': stage_id, 'nodes': selected, 'node_tasks': tasks} for stage_id, selected, tasks in stages], **SECRETS)

    def run(command, expected=None):
        # A dryrun reads the formation once and reports not_checked while the
        # monitor still moves it, such as for a few seconds after a node's
        # restart; a run waits instead. So a dryrun is repeated until the
        # formation has settled, at most a minute.
        deadline = time.monotonic() + 60
        while True:
            result = vm.cmd([mmx, command, *vm.playbook_args(command), '--json'], cwd=workspace, check=False, timeout=1800)
            printed.append(result.stdout + result.stderr)
            if command != 'dryrun' or ' is not settled: ' not in result.stdout or time.monotonic() > deadline:
                break
            time.sleep(2)
        if result.returncode not in (vm.success_codes(command) if expected is None else (expected,)):
            raise AssertionError((command, result.returncode, result.stdout[-6000:], result.stderr[-3000:]))
        return json.loads(result.stdout)

    def results(record):
        """The results of the module tasks by task id, from every fleet stage and node."""
        return {r['task'].rsplit('/', 1)[-1]: r for entry in record['entries'] for r in (entry['response'].get('results') or [])}

    def is_result(result, status, change, reason=None, outputs=None):
        assert result['status'] == status and result['change'] == change and result.get('secret') is not True, result
        if reason is not None:
            assert result.get('reason', '') == reason, result
        if outputs is not None:
            assert {key: result['module_outputs'][key] for key in outputs} == outputs, result
        return True

    def unchanged(record):
        changed = {r['task']: (r['status'], r['change'], r.get('reason', '')) for entry in record['entries'] for r in (entry['response'].get('results') or [])
                   if r['change'] != 'unchanged'}
        assert not changed, changed
        return True

    def monitor_task(**changes):
        inputs = {'data_directory': monitor_data, 'postgresql_major_version': version, 'port': 5432, 'host_name': address['monitor'],
                  'listen_address': address['monitor'],
                  'autoctl_node_password': '{{ .secrets.autoctl_node_password }}', 'formation': 'default',
                  'node_names': ['db01', 'db02'], 'nodes_not_listed': 'remove'}
        inputs.update(changes)
        return {'id': 'monitor', 'pg_auto_failover_monitor': {key: value for key, value in inputs.items() if value is not None}}

    def node_task(server, **changes):
        inputs = {'name': server, 'data_directory': data, 'postgresql_major_version': version, 'port': 5432, 'host_name': address[server],
                  'listen_address': address[server], 'monitor_host_name': address['monitor'], 'monitor_port': 5432,
                  'autoctl_node_password': '{{ .secrets.autoctl_node_password }}',
                  'replication_password': '{{ .secrets.pgautofailover_replicator_password }}',
                  'formation': 'default', 'candidate_priority': 50, 'replication_quorum': True}
        inputs.update(changes)
        task = {'id': server, 'pg_auto_failover_node': {key: value for key, value in inputs.items() if value is not None}}
        if changes.get('state') in ('absent', 'deleted_with_all_its_data'):
            task['pg_auto_failover_node'] = {'data_directory': data, 'state': changes['state']}
        return task

    def primary_task(name):
        return {'id': 'primary', 'pg_auto_failover_primary': {'monitor_data_directory': monitor_data, 'formation': 'default', 'node_name': name}}

    def formation():
        """The formation as the monitor reports it: {name: (current state, assigned state, healthy)}."""
        rows = json.loads(remote('monitor', f'runuser -u postgres -- pg_autoctl show state --pgdata {monitor_data} --formation default --json'))
        return {row['nodename']: (row['current_group_state'], row['assigned_group_state'], row['health'] == 1) for row in rows}

    def wait_for_formation(wanted, seconds=180):
        deadline = time.monotonic() + seconds
        while True:
            found = formation()
            if found == wanted:
                return found
            if time.monotonic() > deadline:
                raise AssertionError(f'the formation is {found}, not {wanted}, after {seconds} seconds')
            time.sleep(1)

    def sql(server, query, database='postgres'):
        return remote(server, f'runuser -u postgres -- {binaries}/psql --no-psqlrc -At -p 5432 -d {database}', input=query)

    def login(server, host, user, password, database):
        """Whether host's PostgreSQL accepts a login over TLS from server."""
        found = remote(server, f'PGPASSWORD="$(cat)" {binaries}/psql --no-psqlrc -At '
                       f'"host={host} port=5432 dbname={database} user={user} sslmode=require connect_timeout=10" -c "select 1"', input=password, check=False)
        return found == '1'

    def directory_of(server):
        return monitor_data if server == 'monitor' else data

    def hba_lines(server):
        """The lines of the server's pg_hba.conf without comments, as lists of fields."""
        return [line.split() for line in remote(server, f'cat {directory_of(server)}/pg_hba.conf').splitlines() if line.split() and not line.startswith('#')]

    def initdb_lines(server):
        """initdb's six lines as the modules leave them: Debian's methods."""
        socket = ['peer', 'map=pg_autoctl'] if server == 'monitor' else ['peer']
        return [['local', 'all', 'all', *socket], ['host', 'all', 'all', '127.0.0.1/32', 'scram-sha-256'], ['host', 'all', 'all', '::1/128', 'scram-sha-256'],
                ['local', 'replication', 'all', 'peer'], ['host', 'replication', 'all', '127.0.0.1/32', 'scram-sha-256'],
                ['host', 'replication', 'all', '::1/128', 'scram-sha-256']]

    def other_account_login(server, host):
        """What psql says when the account nobody logs in as postgres without a password."""
        database = 'pg_auto_failover' if server == 'monitor' else 'postgres'
        return remote(server, f'runuser -u nobody -- {binaries}/psql --no-psqlrc -At -w "host={host} port=5432 dbname={database} user=postgres '
                              'connect_timeout=5" -c "select current_user" 2>&1 || true')

    def listening(server):
        """The addresses at which PostgreSQL listens on the port 5432."""
        found = set()
        for line in remote(server, 'ss -ltnH').splitlines():
            local_address = line.split()[3]
            if local_address.endswith(':5432'):
                found.add(local_address.rpartition(':')[0].strip('[]'))
        return sorted(found)

    def before_the_suite():
        for server in GUESTS:
            state[server] = (set(remote(server, INSTALLED_PACKAGES).split()), remote(server, f"ls -d {' '.join(LEFT_BEHIND)} 2>/dev/null || true").split())
            assert remote(server, 'command -v pg_autoctl || true') == '', f'{server} already has pg_auto_failover'

    def cleanup():
        for server in GUESTS:
            packages, paths = state[server]
            added = ' '.join(sorted(set(remote(server, INSTALLED_PACKAGES).split()) - packages))
            gone = ' '.join(path for path in LEFT_BEHIND if path not in paths)
            remote(server, f"""systemctl disable --now pgautofailover.service 2>/dev/null || true
pkill -KILL -u postgres 2>/dev/null || true
{f'DEBIAN_FRONTEND=noninteractive apt-get purge -y -q {added} >/dev/null 2>&1 || true' if added else ''}
rm -rf {gone}
userdel postgres 2>/dev/null || true; groupdel postgres 2>/dev/null || true
systemctl daemon-reload; systemctl reset-failed 2>/dev/null || true""")

    def guests_are_as_found():
        for server in GUESTS:
            packages, paths = state[server]
            left = remote(server, f"command -v pg_autoctl || true; ls -d {' '.join(path for path in LEFT_BEHIND if path not in paths)} 2>/dev/null || true")
            now = set(remote(server, INSTALLED_PACKAGES).split())
            if now != packages:
                left += f'\npackages added: {sorted(now - packages)}; gone: {sorted(packages - now)}'
            running = vm.guest('sudo', '-n', 'systemctl', 'is-system-running', name=GUESTS[server], check=False).stdout.strip()
            if running != 'running':
                left += f'\nsystemd is {running}'
            assert left.strip() == '', f'the suite left this on {server}:\n{left}'
        print('The three guests are as the suite found them: no PostgreSQL, no pg_auto_failover, and systemd is running', flush=True)

    environment = {'MMX_TEST_AUTOCTL_NODE_PASSWORD': AUTOCTL_PASSWORD, 'MMX_TEST_REPLICATION_PASSWORD': REPLICATION_PASSWORD}
    before_the_suite()
    try:
        vm.ENV.update(environment)

        # Without pg_auto_failover: each module waits for pg_autoctl.
        project(('monitor', 'monitor', [monitor_task()]), ('first', 'db01', [node_task('db01')]), ('switch', 'monitor', [primary_task('db01')]))
        previewed = results(run('dryrun'))
        # The monitor's node runs the fleet stage switch after the fleet stage monitor, whose task is not checked either and so may change
        # the node: the task primary names it. The node db01 runs one fleet stage.
        after_the_monitor = 'monitor in the fleet stage monitor'
        for task, module, earlier_task in (('monitor', 'pg_auto_failover_monitor', None), ('db01', 'pg_auto_failover_node', None),
                                           ('primary', 'pg_auto_failover_primary', after_the_monitor)):
            missing_program = MISSING_PG_AUTOCTL.format(module=module)
            is_result(previewed[task], 'not_checked', 'unknown', missing_program if earlier_task is None else vm.earlier_change_may_provide(earlier_task, missing_program))
        missing = results(run('run', 1))['monitor']
        assert is_result(missing, 'failed', 'unchanged', MISSING_PG_AUTOCTL.format(module='pg_auto_failover_monitor')) and \
            missing['failure_kind'] == 'missing_prerequisite' and len(missing.get('tries') or [missing]) == 1, missing
        mark('without pg_autoctl, a dryrun of each module is not checked and names the program and its package, and a run fails once with a missing prerequisite')

        # The page's set-up: no cluster of Debian's tools, the packages, and
        # the directory of the data directories, which belongs to postgres.
        set_up = [
            {'directory': {'path': '/etc/postgresql-common', 'permissions_mode': '0755'}},
            {'file': {'path': '/etc/postgresql-common/createcluster.conf', 'content': 'create_main_cluster = false\n', 'permissions_mode': '0644'}},
            {'apt_package': {'packages': [{'name': 'postgresql-{{ .vars.postgresql_major_version }}', 'version': 'newest_when_first_installed'},
                                          {'name': 'postgresql-{{ .vars.postgresql_major_version }}-auto-failover', 'version': 'newest_when_first_installed'},
                                          {'name': 'pg-auto-failover-cli', 'version': 'newest_when_first_installed'}]}},
            {'directory': {'path': '/var/lib/postgresql/{{ .vars.postgresql_major_version }}', 'owner': 'postgres', 'group': 'postgres', 'permissions_mode': '0755'}},
        ]
        # Later node tasks read is_primary, as a database server's role does
        # to work only on the primary.
        primary_reads = [{'id': 'name_whether_db01_is_the_primary', 'set_vars': {'is_the_primary': '{{ .node_tasks.db01.is_primary }}'}},
                         {'id': 'only_on_the_primary', 'when': '{{ .vars.is_the_primary }}', 'show': 'db01 is the primary'}]
        formation_stages = [('set_up', 'all', set_up), ('monitor', 'monitor', [monitor_task()]), ('first', 'db01', [node_task('db01'), *primary_reads]),
                            ('second', 'db02', [dict(node_task('db02'), time_limit='10m')])]

        def primary_reads_not_checked(previewed, because):
            for task in ('name_whether_db01_is_the_primary', 'only_on_the_primary'):
                assert previewed[task]['status'] == 'not_checked', previewed[task]
            assert because in previewed['name_whether_db01_is_the_primary']['reason'], previewed['name_whether_db01_is_the_primary']
            assert 'name_whether_db01_is_the_primary is not_checked' in previewed['only_on_the_primary']['reason'], previewed['only_on_the_primary']

        project(*formation_stages)
        previewed = results(run('dryrun'))
        is_result(previewed['monitor'], 'not_checked', 'unknown')
        is_result(previewed['db01'], 'not_checked', 'unknown')
        primary_reads_not_checked(previewed, 'db01 is not_checked: ')
        # With the packages, a dryrun would create the node, whose state the
        # monitor gives it only then.
        project(formation_stages[0])
        run('run')
        project(*formation_stages)
        previewed = results(run('dryrun'))
        is_result(previewed['db01'], 'succeeded', 'would_change')
        assert previewed['db01']['module_outputs'] == {'created': True}, previewed['db01']
        primary_reads_not_checked(previewed, 'db01 reports no is_primary in this dryrun')
        mark('a dryrun before a node exists: the node tasks that read its is_primary are not checked, before and after the packages are installed')
        started = time.monotonic()
        applied = results(run('run'))
        print(f'The first run created the monitor and both nodes in {time.monotonic() - started:.0f} s', flush=True)
        is_result(applied['monitor'], 'succeeded', 'changed', outputs={'created': True, 'primary_node_name': '',
                                                                        'node_monitor_uri': f'postgres://autoctl_node@{address["monitor"]}:5432/pg_auto_failover?sslmode=require'})
        loopback = 'scram-sha-256 (host all all 127.0.0.1/32, host all all ::1/128, host replication all 127.0.0.1/32, host replication all ::1/128)'
        assert applied['monitor']['reason'] == (
            f'created the pg_auto_failover monitor in {monitor_data}; wrote the unit {UNIT}; started pgautofailover.service; '
            f'wrote the ident map pg_autoctl, with which pg_autoctl logs in as autoctl_node on the local socket, into {monitor_data}/pg_ident.conf; '
            f'replaced trust in {monitor_data}/pg_hba.conf with peer map=pg_autoctl (local all all), {loopback}, and peer (local replication all); '
            f'reloaded PostgreSQL in {monitor_data}; set the password of autoctl_node'), applied['monitor']
        is_result(applied['db01'], 'succeeded', 'changed', outputs={'created': True, 'current_state': 'single', 'is_primary': True})
        assert applied['only_on_the_primary']['status'] == 'succeeded' and applied['only_on_the_primary']['value'] == 'db01 is the primary', applied['only_on_the_primary']
        assert applied['db01']['reason'].startswith(f'wrote the logins of autoctl_node and pgautofailover_replicator into the password file /var/lib/postgresql/.pgpass; '
                                                    f'created the node db01 in {data} in the formation default; wrote the unit {UNIT}; started pgautofailover.service; '
                                                    f'replaced trust in {data}/pg_hba.conf with peer (local all all, local replication all) and {loopback}; '
                                                    f'reloaded PostgreSQL in {data}; set the password of pgautofailover_replicator on the primary db01'), applied['db01']
        is_result(applied['db02'], 'succeeded', 'changed', outputs={'created': True, 'is_primary': False})
        # db02 copied db01's pg_hba.conf, which db01's task had changed.
        assert 'set the password' not in applied['db02']['reason'] and 'replaced trust' not in applied['db02']['reason'], applied['db02']
        for task in ('db01', 'db02'):
            assert applied[task]['diff'].count('/dev/stdin /var/lib/postgresql/.pgpass') == 1 and 'Sentinel' not in applied[task]['diff'], applied[task]['diff']
        # db02's task ends once the formation has settled after its join.
        assert formation() == {'db01': ('primary', 'primary', True), 'db02': ('secondary', 'secondary', True)}, formation()
        assert sql('db01', 'show synchronous_standby_names') == 'ANY 1 (pgautofailover_standby_2)'
        assert sql('db01', 'select sync_state from pg_stat_replication') == 'quorum'
        sql('db01', 'create table mmx_orders (number integer); insert into mmx_orders values (1);')
        assert sql('db02', 'select count(*) from mmx_orders') == '1' and sql('db02', 'select pg_is_in_recovery()') == 't'
        for server in ('db01', 'db02'):
            assert remote(server, 'stat -c "%U %G %a" /var/lib/postgresql/.pgpass') == 'postgres postgres 600'
            assert remote(server, 'systemctl is-active pgautofailover; systemctl is-enabled pgautofailover') == 'active\nenabled'
        assert login('db01', address['monitor'], 'autoctl_node', AUTOCTL_PASSWORD, 'pg_auto_failover')
        assert not login('db01', address['monitor'], 'autoctl_node', 'not the password', 'pg_auto_failover')
        assert login('db02', address['db01'], 'pgautofailover_replicator', REPLICATION_PASSWORD, 'postgres')
        for command in ('run', 'dryrun'):
            assert unchanged(run(command))
        outputs = results(run('run'))['monitor']['module_outputs']
        assert outputs['primary_node_name'] == 'db01' and [(n['name'], n['current_state'], n['healthy'], n['replication_quorum']) for n in outputs['nodes']] == \
            [('db01', 'primary', True, True), ('db02', 'secondary', True, True)], outputs
        mark('the first run: the packages without a cluster of Debian\'s tools, the monitor, db01 as the primary, and db02, which copies db01 and joins as a '
             'replica with synchronous replication; the passwords work over TLS; an unchanged rerun and dryrun; the monitor\'s outputs')

        # The local logins: initdb's six lines with Debian's methods; every
        # other line as pg_autoctl wrote it, its health check's trust from
        # the monitor's address included.
        for server in GUESTS:
            lines = hba_lines(server)
            assert lines[:6] == initdb_lines(server), (server, lines)
            assert [line for line in lines if 'trust' in line] == ([] if server == 'monitor' else [
                ['hostssl', 'all', '"pgautofailover_monitor"', f'{address["monitor"]}/32', 'trust', '#', 'Auto-generated', 'by', 'pg_auto_failover']]), (server, lines)
            found = other_account_login(server, '/var/run/postgresql')
            assert 'Peer authentication failed for user "postgres"' in found, (server, found)
            assert remote(server, f'runuser -u postgres -- {binaries}/psql --no-psqlrc -At -p 5432 -d postgres -c "select current_user"') == 'postgres'
        ident = [line.split() for line in remote('monitor', f'cat {monitor_data}/pg_ident.conf').splitlines() if line.split() and not line.startswith('#')]
        assert ident == [['pg_autoctl', 'postgres', 'autoctl_node'], ['pg_autoctl', '/^(.*)$', '\\1']], ident
        # Every instance listens at its guest's address alone; pg_autoctl
        # needs no loopback address.
        for server in GUESTS:
            assert listening(server) == [address[server]], (server, listening(server))
            found = other_account_login(server, '127.0.0.1')
            assert 'Connection refused' in found, (server, found)
        mark('initdb\'s trust lines in pg_hba.conf become peer on the socket (with the ident map pg_autoctl on the monitor) and scram-sha-256 on the '
             'loopback addresses, and pg_autoctl\'s own lines stay; another account cannot log in as postgres; every instance listens at its address alone')

        # A new listen_address: the monitor and the replica db02 listen on
        # every address after a restart of their service, and the primary
        # db01 waits for a switchover.
        everywhere = [('monitor', 'monitor', [monitor_task(listen_address='*')]), ('second', 'db02', [node_task('db02', listen_address='*')])]
        project(*everywhere)
        previewed = results(run('dryrun'))
        listen_reason = 'pgautofailover.service, set postgresql.listen_addresses of the instance in {directory} from {before} to {after}, and {started} the service again'
        is_result(previewed['monitor'], 'succeeded', 'would_change',
                  'would stop ' + listen_reason.format(directory=monitor_data, before=address['monitor'], after='*', started='start'))
        is_result(previewed['db02'], 'succeeded', 'would_change', 'would stop ' + listen_reason.format(directory=data, before=address['db02'], after='*', started='start'))
        changed = results(run('run'))
        is_result(changed['monitor'], 'succeeded', 'changed', 'stopped ' + listen_reason.format(directory=monitor_data, before=address['monitor'], after='*', started='started'))
        is_result(changed['db02'], 'succeeded', 'changed', 'stopped ' + listen_reason.format(directory=data, before=address['db02'], after='*', started='started'))
        # The keeper of a replica may restart its PostgreSQL once more after
        # its service started: the logins wait for a healthy formation.
        wait_for_formation({'db01': ('primary', 'primary', True), 'db02': ('secondary', 'secondary', True)})
        for server in ('monitor', 'db02'):
            assert listening(server) in (['0.0.0.0', '::'], ['0.0.0.0']), (server, listening(server))
            found = other_account_login(server, '127.0.0.1')
            assert 'no password supplied' in found, (server, found)
        assert unchanged(run('run'))
        # A trust line that comes back is replaced again: until then another
        # account logs in as postgres over 127.0.0.1.
        remote('db02', f"sed -i -E 's/^(host[[:space:]]+all[[:space:]]+all[[:space:]]+127\\.0\\.0\\.1\\/32[[:space:]]+)scram-sha-256$/\\1trust/' {data}/pg_hba.conf\n"
                       f'runuser -u postgres -- {binaries}/pg_ctl reload --pgdata {data} >/dev/null')
        found = other_account_login('db02', '127.0.0.1')
        assert found == 'postgres', found
        is_result(results(run('dryrun'))['db02'], 'succeeded', 'would_change',
                  f'would replace trust in {data}/pg_hba.conf with scram-sha-256 (host all all 127.0.0.1/32); would reload PostgreSQL in {data}')
        is_result(results(run('run'))['db02'], 'succeeded', 'changed',
                  f'replaced trust in {data}/pg_hba.conf with scram-sha-256 (host all all 127.0.0.1/32); reloaded PostgreSQL in {data}')
        found = other_account_login('db02', '127.0.0.1')
        assert 'no password supplied' in found, found
        # Back to each guest's address.
        project(*formation_stages)
        changed = results(run('run'))
        is_result(changed['monitor'], 'succeeded', 'changed', 'stopped ' + listen_reason.format(directory=monitor_data, before='*', after=address['monitor'], started='started'))
        is_result(changed['db02'], 'succeeded', 'changed', 'stopped ' + listen_reason.format(directory=data, before='*', after=address['db02'], started='started'))
        wait_for_formation({'db01': ('primary', 'primary', True), 'db02': ('secondary', 'secondary', True)})
        for server in GUESTS:
            assert listening(server) == [address[server]], (server, listening(server))
        # On the primary db01, the restart would make the monitor fail over.
        project(('first', 'db01', [node_task('db01', listen_address='*')]))
        refusal = (f'pg_auto_failover_node: the node db01 is the primary of the formation default (primary), and a new listen_address takes a restart of '
                   'its service, after which the monitor fails over to a replica: make another node the primary first with pg_auto_failover_primary')
        is_result(results(run('dryrun'))['db01'], 'not_checked', 'unknown', refusal)
        refused = results(run('run', 1))['db01']
        assert is_result(refused, 'failed', 'unchanged', refusal) and refused['failure_kind'] == 'missing_prerequisite', refused
        assert listening('db01') == [address['db01']]
        project(*formation_stages)
        assert unchanged(run('run'))
        mark('a new listen_address on the monitor and the replica: the service stops, pg_autoctl\'s setting changes, the service starts, and PostgreSQL '
             'listens on every address and back; a trust line that comes back is replaced again; on the primary the change waits for a switchover')

        # A creation that stopped before the unit: the next run finishes it.
        remote('monitor', f'systemctl disable --now pgautofailover; rm {UNIT}; systemctl daemon-reload')
        finished = results(run('run'))['monitor']
        is_result(finished, 'succeeded', 'changed', outputs={'created': False})
        assert finished['reason'] == f'finished creating the pg_auto_failover monitor in {monitor_data}; wrote the unit {UNIT}; started pgautofailover.service', finished
        wait_for_formation({'db01': ('primary', 'primary', True), 'db02': ('secondary', 'secondary', True)})
        assert unchanged(run('run'))
        mark('a monitor whose unit is missing, as after a creation that stopped partway, is finished by the next run, which reports created: false')

        # New passwords: the monitor and the primary set theirs, and every
        # node writes its password file; the formation stays healthy.
        vm.ENV.update({'MMX_TEST_AUTOCTL_NODE_PASSWORD': SECOND_AUTOCTL_PASSWORD, 'MMX_TEST_REPLICATION_PASSWORD': SECOND_REPLICATION_PASSWORD})
        project(*formation_stages)
        previewed = results(run('dryrun'))
        is_result(previewed['monitor'], 'succeeded', 'would_change', 'would set the password of autoctl_node')
        rotated = results(run('run'))
        is_result(rotated['monitor'], 'succeeded', 'changed', 'set the password of autoctl_node')
        is_result(rotated['db01'], 'succeeded', 'changed', 'wrote the logins of autoctl_node and pgautofailover_replicator into the password file '
                                                          '/var/lib/postgresql/.pgpass; set the password of pgautofailover_replicator on the primary db01')
        is_result(rotated['db02'], 'succeeded', 'changed', 'wrote the logins of autoctl_node and pgautofailover_replicator into the password file /var/lib/postgresql/.pgpass')
        assert login('db02', address['db01'], 'pgautofailover_replicator', SECOND_REPLICATION_PASSWORD, 'postgres')
        assert not login('db02', address['db01'], 'pgautofailover_replicator', REPLICATION_PASSWORD, 'postgres')
        assert login('db01', address['monitor'], 'autoctl_node', SECOND_AUTOCTL_PASSWORD, 'pg_auto_failover')
        remote('db02', 'systemctl restart pgautofailover')
        wait_for_formation({'db01': ('primary', 'primary', True), 'db02': ('secondary', 'secondary', True)})
        assert unchanged(run('run'))
        mark('new passwords: the monitor sets the password of autoctl_node after a refused login, the primary sets the replication password, '
             'each node writes its password file; db02 copies db01 again after a restart, and the next run is unchanged')

        # The failover settings of a node.
        project(('db02', 'db02', [node_task('db02', candidate_priority=70, replication_quorum=False)]))
        is_result(results(run('dryrun'))['db02'], 'succeeded', 'would_change', 'would set candidate_priority of the node db02 to 70; would set replication_quorum of the node db02 to false')
        is_result(results(run('run'))['db02'], 'succeeded', 'changed', 'set candidate_priority of the node db02 to 70; set replication_quorum of the node db02 to false')
        assert remote('db02', f'runuser -u postgres -- pg_autoctl get node candidate-priority --pgdata {data}') == '70'
        assert remote('db02', f'runuser -u postgres -- pg_autoctl get node replication-quorum --pgdata {data}') == 'false'
        project(*formation_stages)
        is_result(results(run('run'))['db02'], 'succeeded', 'changed', 'set candidate_priority of the node db02 to 50; set replication_quorum of the node db02 to true')
        assert unchanged(run('run'))
        mark('candidate_priority and replication_quorum are set with pg_autoctl set node when they differ, and read back with pg_autoctl get node')

        # A planned switchover to db02, and back.
        project(('switch', 'monitor', [primary_task('db02')]))
        is_result(results(run('dryrun'))['primary'], 'succeeded', 'would_change',
                  'would make the node db02 the primary of the formation default in place of db01', {'primary_node_name': 'db02'})
        assert formation()['db01'][0] == 'primary'
        switched = results(run('run'))['primary']
        is_result(switched, 'succeeded', 'changed', 'made the node db02 the primary of the formation default in place of db01', {'primary_node_name': 'db02'})
        # The task ends once the old primary db01 rests as a healthy replica.
        assert formation() == {'db01': ('secondary', 'secondary', True), 'db02': ('primary', 'primary', True)}, formation()
        assert sql('db02', 'select count(*) from mmx_orders') == '1'
        assert unchanged(run('run'))
        project(*formation_stages)
        assert unchanged(run('run'))
        mark('pg_auto_failover_primary makes db02 the primary with a planned switchover, which keeps every row, and ends once the formation has settled; a rerun, '
             'and the node tasks on the new roles, are unchanged')

        # Maintenance on the replica db01, and the refusal on the primary db02.
        project(('maintenance', 'db01', [node_task('db01', state='in_maintenance')]))
        is_result(results(run('dryrun'))['db01'], 'succeeded', 'would_change', 'would put the node db01 into maintenance')
        is_result(results(run('run'))['db01'], 'succeeded', 'changed', 'put the node db01 into maintenance', {'current_state': 'maintenance', 'is_primary': False})
        assert formation()['db01'][:2] == ('maintenance', 'maintenance')
        # The primary does not wait for a replica in maintenance.
        assert remote('db02', f'timeout 10 runuser -u postgres -- {binaries}/psql --no-psqlrc -At -c "insert into mmx_orders values (2)"') == 'INSERT 0 1'
        assert sql('db02', 'show synchronous_standby_names') == ''
        assert unchanged(run('run'))
        # A reboot in maintenance, as for a kernel update: pg_autoctl keeps
        # db01's PostgreSQL stopped. in_maintenance stays unchanged, and
        # present ends the maintenance, after which PostgreSQL starts.
        vm.reboot(GUESTS['db01'])
        nodes['db01'], connections['db01'] = vm.connection(GUESTS['db01'])
        write_inventory()
        assert formation()['db01'][:2] == ('maintenance', 'maintenance')
        assert remote('db01', f'if {binaries}/pg_isready --quiet --port 5432 --host {address["db01"]}; then echo answers; else echo stopped; fi') == 'stopped'
        assert unchanged(run('run'))
        project(('maintenance', 'db01', [node_task('db01')]))
        is_result(results(run('run'))['db01'], 'succeeded', 'changed', 'ended the maintenance of the node db01', {'is_primary': False})
        # The task ends once db01 has caught up and the formation has settled.
        assert formation() == {'db01': ('secondary', 'secondary', True), 'db02': ('primary', 'primary', True)}, formation()
        refusal = ('pg_auto_failover_node: the node db02 is the primary of the formation default (primary); maintenance on the primary would cause a failover '
                   'that the task does not ask for. Make another node the primary first with pg_auto_failover_primary')
        project(('maintenance', 'db02', [node_task('db02', state='in_maintenance')]))
        is_result(results(run('dryrun'))['db02'], 'not_checked', 'unknown', refusal)
        refused = results(run('run', 1))['db02']
        assert is_result(refused, 'failed', 'unchanged', refusal) and refused['failure_kind'] == 'missing_prerequisite', refused
        assert formation()['db02'][:2] == ('primary', 'primary')
        mark('in_maintenance on the replica db01 puts it into maintenance, writes on the primary go on, and after a reboot, which leaves its PostgreSQL '
             'stopped, in_maintenance is unchanged and present ends the maintenance once db01 has caught up; in_maintenance on the primary is a missing '
             'prerequisite that says to switch the primary first')

        # A restart of the primary's service, as a playbook's systemd_service
        # task would do, is a failover: the new keeper finds PostgreSQL
        # stopped, and the monitor promotes the replica. A node task that
        # starts while the failover goes on waits until the formation has
        # settled; on db01, it then counts db01 as the primary.
        remote('db02', 'systemctl restart pgautofailover')
        started = time.monotonic()
        while formation()['db02'][1] == 'primary':
            assert time.monotonic() - started < 60, f'the monitor has not begun a failover a minute after db02\'s service restarted: {formation()}'
            time.sleep(0.5)
        print(f'The monitor began a failover {time.monotonic() - started:.1f} s after db02\'s service restarted: {formation()}', flush=True)
        project(('first', 'db01', [node_task('db01')]))
        settled = {'db01': ('primary', 'primary', True), 'db02': ('secondary', 'secondary', True)}
        before = formation()
        assert before != settled, f'the failover ended before the node task started, so the step shows nothing: {before}'
        started = time.monotonic()
        during = results(run('run'))['db01']
        print(f'db01\'s node task, started on {before}, ended after {time.monotonic() - started:.0f} s', flush=True)
        assert during['status'] == 'succeeded' and during['change'] == 'unchanged' and during['module_outputs']['is_primary'] is True, during
        # It is unchanged, so only its wait at the start keeps it from ending
        # before the formation has settled.
        assert formation() == settled, formation()
        assert sql('db01', 'select pg_is_in_recovery(), count(*) from mmx_orders') == 'f|2'
        assert sql('db02', 'select pg_is_in_recovery(), count(*) from mmx_orders') == 't|2'
        mark('a restart of the primary db02\'s service makes the monitor fail over; db01\'s node task, started during the failover, waits until the formation '
             'has settled, counts db01 as the primary, and every row stays')

        # The PostgreSQL settings, with db01 the primary and db02 its
        # replica. The module's file stays on both nodes until their
        # deleted_with_all_its_data removes it; the later steps' node tasks
        # do not name postgresql_settings, so they leave it alone.
        settings = {'shared_buffers': '136MB', 'work_mem': '64MB'}
        include = f"include_if_exists = '{SETTINGS_FILE}'"

        def settings_stages(restart, servers=('db01', 'db02'), **changes):
            """The node tasks of servers, in that order, with the settings and changes."""
            stage = {'db01': 'first', 'db02': 'second'}
            return [(stage[server], server, [dict(node_task(server, postgresql_settings=dict(settings, **changes), restart_postgresql_for_new_settings=restart),
                                                  time_limit='10m')]) for server in servers]

        # Before the module's include exists, a dryrun compares by
        # PostgreSQL's own reading: 131072kB is initdb's 128MB, so nothing
        # would wait for a restart.
        project(('first', 'db01', [node_task('db01', postgresql_settings={'shared_buffers': '131072kB'}, restart_postgresql_for_new_settings='never')]))
        previewed = results(run('dryrun'))['db01']
        assert previewed['status'] == 'succeeded' and 'wait' not in previewed.get('reason', ''), previewed
        # never on both nodes: work_mem is live from the module's file after
        # a reload, and shared_buffers waits for a restart, which the reason
        # and the output name.
        project(*settings_stages('never'))
        applied = results(run('run'))
        for server, hint in (('db01', 'make another node the primary with pg_auto_failover_primary, then run this task with restart_postgresql_for_new_settings: '
                                      'on_a_replica on this node'),
                             ('db02', 'run this task with restart_postgresql_for_new_settings: on_a_replica on this node')):
            is_result(applied[server], 'succeeded', 'changed', f'set shared_buffers to 136MB and work_mem to 64MB in {SETTINGS_FILE}; added the line {include} at '
                                                               f'the end of {data}/postgresql.conf; reloaded PostgreSQL in {data}; shared_buffers waits for a restart '
                                                               f'of PostgreSQL; {hint}',
                      {'changed_postgresql_settings': ['shared_buffers', 'work_mem'], 'restarted_postgresql': False,
                       'postgresql_settings_waiting_for_a_restart': ['shared_buffers']})
            assert sql(server, "select setting, unit, sourcefile from pg_settings where name = 'work_mem'") == f'65536|kB|{SETTINGS_FILE}', server
            assert sql(server, "select pending_restart from pg_settings where name = 'shared_buffers'") == 't', server
            assert remote(server, f'stat -c "%U %G %a" {SETTINGS_FILE}; grep -v "^#" {SETTINGS_FILE}; tail -n 1 {data}/postgresql.conf') == \
                f"root postgres 644\nshared_buffers = '136MB'\nwork_mem = '64MB'\n{include}", server
        assert unchanged(run('run'))
        mark('postgresql_settings with never on both nodes: the module writes its file and the include line, PostgreSQL checks and reloads them, work_mem '
             'is live from the file, shared_buffers waits for a restart and the reason and the output say so; a rerun is unchanged')

        # db01 runs with initdb's 128MB while its file's 136MB waits for a
        # restart: a dryrun that puts 128MB back compares it with the value
        # that PostgreSQL runs with, and nothing would wait.
        project(*settings_stages('never', servers=('db01',), shared_buffers='128MB'))
        is_result(results(run('dryrun'))['db01'], 'succeeded', 'would_change',
                  f'would set shared_buffers from 136MB to 128MB in {SETTINGS_FILE}; would reload PostgreSQL in {data}')
        mark('a dryrun that puts back the value that PostgreSQL runs with, while the file\'s other value waits for a restart, says nothing would wait')

        # on_a_replica: the replica db02 restarts through maintenance, and
        # the formation settles; the primary db01 refuses, and stays as it
        # was. db02 runs first.
        primary_refusal = ('pg_auto_failover_node: the node db01 is the primary of the formation default, and shared_buffers takes a restart of PostgreSQL, '
                           'which on the primary is a failover: make another node the primary first with pg_auto_failover_primary, then run this task again; '
                           'the earlier settings stay in effect')
        project(*settings_stages('on_a_replica', servers=('db02', 'db01')))
        previewed = results(run('dryrun'))
        is_result(previewed['db02'], 'succeeded', 'would_change', 'would restart PostgreSQL on the replica db02 through maintenance for shared_buffers')
        is_result(previewed['db01'], 'not_checked', 'unknown', primary_refusal)
        restarted = results(run('run', 1))
        is_result(restarted['db02'], 'succeeded', 'changed', 'restarted PostgreSQL on the replica db02 through maintenance for shared_buffers',
                  {'restarted_postgresql': True, 'postgresql_settings_waiting_for_a_restart': [], 'current_state': 'secondary', 'is_primary': False})
        assert is_result(restarted['db01'], 'failed', 'unchanged', primary_refusal) and restarted['db01']['failure_kind'] == 'missing_prerequisite', restarted['db01']
        assert formation() == settled, formation()
        assert sql('db02', "select setting, sourcefile from pg_settings where name = 'shared_buffers'") == f'17408|{SETTINGS_FILE}'
        assert sql('db01', "select pending_restart from pg_settings where name = 'shared_buffers'") == 't'
        assert sql('db02', 'select pg_is_in_recovery(), count(*) from mmx_orders') == 't|2'
        mark('on_a_replica: the replica db02 restarts through maintenance for the setting that the earlier run left waiting, and the formation settles; '
             'the primary db01 refuses with a missing prerequisite that says to switch first')

        # A value that PostgreSQL refuses: postgres -C fails, the module puts
        # its file back, and the task fails once with PostgreSQL's words.
        project(*settings_stages('on_a_replica', servers=('db02',), shared_buffers='not a size'))
        refused = results(run('run', 1))['db02']
        assert refused['status'] == 'failed' and refused['failure_kind'] == 'config' and len(refused.get('tries') or [refused]) == 1, refused
        assert refused['reason'].startswith('pg_auto_failover_node.postgresql_settings.shared_buffers: PostgreSQL refuses it: ') and \
            'invalid value for parameter "shared_buffers": "not a size"' in refused['reason'] and refused['reason'].endswith('; the earlier settings stay in effect'), refused
        assert "shared_buffers = '136MB'" in remote('db02', f'cat {SETTINGS_FILE}') and sql('db02', 'show shared_buffers') == '136MB'
        # A setting that ALTER SYSTEM sets in postgresql.auto.conf, which
        # PostgreSQL reads last: the task's value would not be live.
        sql('db02', "alter system set work_mem = '32MB'")
        project(*settings_stages('on_a_replica', servers=('db02',)))
        refused = results(run('run', 1))['db02']
        assert refused['status'] == 'failed' and refused['failure_kind'] == 'config' and refused['change'] == 'unchanged', refused
        assert refused['reason'].startswith(f'pg_auto_failover_node.postgresql_settings.work_mem: {data}/postgresql.auto.conf sets it again at line ') and \
            refused['reason'].endswith(f', which PostgreSQL reads after {SETTINGS_FILE}, so the task\'s value would not be live: remove it there with '
                                       'ALTER SYSTEM RESET work_mem; the earlier settings stay in effect'), refused
        sql('db02', 'alter system reset work_mem')
        assert unchanged(run('run'))
        assert sql('db02', 'show work_mem') == '64MB'
        mark('a value that PostgreSQL refuses fails once with PostgreSQL\'s words and the old file back; a setting that ALTER SYSTEM sets again fails '
             'and names postgresql.auto.conf, and after ALTER SYSTEM RESET the task is unchanged')

        # A share of the server's memory, worked out from MemTotal on the
        # node: a hundredth of it, in steps of 4 MiB, as maintenance_work_mem,
        # which a reload applies. Then the task's settings again.
        share = {'percent_of_memory': 1, 'rounded_down_to_mib': 4, 'at_least_mib': 4}
        memory_mib = int(remote('db02', "awk '$1 == \"MemTotal:\" {print $2}' /proc/meminfo")) // 1024
        expected_mib = max(4, memory_mib * 1 // 100 // 4 * 4)
        project(*settings_stages('on_a_replica', servers=('db02',), maintenance_work_mem=share))
        shared = results(run('run'))['db02']
        assert shared['status'] == 'succeeded' and shared['module_outputs']['changed_postgresql_settings'] == ['maintenance_work_mem'], shared
        assert f"maintenance_work_mem = '{expected_mib}MB'" in remote('db02', f'cat {SETTINGS_FILE}'), remote('db02', f'cat {SETTINGS_FILE}')
        assert sql('db02', "select setting, unit, sourcefile from pg_settings where name = 'maintenance_work_mem'") == f'{expected_mib * 1024}|kB|{SETTINGS_FILE}'
        project(*settings_stages('on_a_replica', servers=('db02',)))
        assert results(run('run'))['db02']['module_outputs']['changed_postgresql_settings'] == ['maintenance_work_mem']
        mark('a share of the server\'s memory: the module works it out from MemTotal on the node, writes it in MB, and PostgreSQL takes it from the module\'s file')

        # A shared_buffers larger than the server's memory and swap, which
        # postgres -C takes: on the replica db02, PostgreSQL does not start
        # after the restart through maintenance, and the run fails. On Debian
        # 13 and Ubuntu 26.04, pg_autoctl then started PostgreSQL itself for
        # the crash recovery before pg_rewind, kept it as a defunct process
        # after it failed, and never started it again. The monitor assigns
        # db02 catchingup while it still reports maintenance, so db01's task
        # goes on. db02's task with the earlier value writes it and restarts
        # pgautofailover.service, after which pg_autoctl starts PostgreSQL.
        project(*[(stage, server, [dict(task, retry={'max_tries': 1}) for task in tasks])
                  for stage, server, tasks in settings_stages('on_a_replica', servers=('db02',), shared_buffers='1TB')])
        assert results(run('run', 1))['db02']['status'] == 'failed'
        started = time.monotonic()
        while formation()['db02'] != ('maintenance', 'catchingup', False):
            assert time.monotonic() - started < 90, f'the monitor does not see db02 as failed 90 s after the run: {formation()}'
            time.sleep(1)
        project(('first', 'db01', [node_task('db01')]))
        started = time.monotonic()
        is_result(results(run('run'))['db01'], 'succeeded', 'unchanged', outputs={'is_primary': True})
        assert time.monotonic() - started < 120, f'db01\'s task waited for the failed replica: {time.monotonic() - started:.0f} s'
        project(*settings_stages('on_a_replica', servers=('db02',)))
        is_result(results(run('dryrun'))['db02'], 'succeeded', 'would_change',
                  f'would set shared_buffers from 1TB to 136MB in {SETTINGS_FILE}; would restart pgautofailover.service so that PostgreSQL starts with them')
        is_result(results(run('run'))['db02'], 'succeeded', 'changed',
                  f"set shared_buffers from 1TB to 136MB in {SETTINGS_FILE}; restarted pgautofailover.service, as the node's PostgreSQL did not run, so that "
                  'PostgreSQL starts with them',
                  {'changed_postgresql_settings': ['shared_buffers'], 'restarted_postgresql': True, 'postgresql_settings_waiting_for_a_restart': [],
                   'current_state': 'secondary'})
        assert formation() == settled, formation()
        assert sql('db02', 'show shared_buffers') == '136MB' and sql('db02', 'select pg_is_in_recovery(), count(*) from mmx_orders') == 't|2'
        mark('a replica whose PostgreSQL does not start with a shared_buffers larger than its memory: db01\'s node task goes on, and db02\'s task with the '
             'earlier value writes it and restarts pgautofailover.service, after which PostgreSQL starts and the formation settles')

        # On the replica, a max_connections lower than the primary's, which
        # pg_controldata reports there, is refused before anything changes.
        primary_connections = int(sql('db01', 'show max_connections'))
        project(*settings_stages('on_a_replica', servers=('db02',), max_connections=primary_connections - 10))
        refused = results(run('run', 1))['db02']
        assert is_result(refused, 'failed', 'unchanged', f'pg_auto_failover_node.postgresql_settings.max_connections: {primary_connections - 10} is lower than '
                                                         f'{primary_connections}, the primary\'s value, which pg_controldata reports on this replica: PostgreSQL '
                                                         'does not run a replica with a lower value than its primary\'s. Keep at least '
                                                         f'{primary_connections}; a higher value goes to the replicas first, then to the primary'), refused
        assert 'max_connections' not in remote('db02', f'cat {SETTINGS_FILE}')
        project(*formation_stages)
        assert unchanged(run('run'))
        mark('on the replica, a max_connections lower than the primary\'s is refused before the file changes; the usual playbook is unchanged after')

        # A failover: with db01 the primary, its service stops, keeper and
        # PostgreSQL. The monitor promotes db02; db01 still reports its last
        # state, and the monitor has assigned it another one.
        project(('switch', 'monitor', [primary_task('db01')]))
        run('run')
        assert formation() == {'db01': ('primary', 'primary', True), 'db02': ('secondary', 'secondary', True)}, formation()
        sql('db01', 'insert into mmx_orders values (3);')
        remote('db01', 'systemctl stop pgautofailover')
        started = time.monotonic()
        while formation()['db02'][1] not in PRIMARY_STATES or formation()['db02'][0] != formation()['db02'][1]:
            assert time.monotonic() - started < 240, f'the monitor has not promoted db02 four minutes after db01 stopped: {formation()}'
            time.sleep(1)
        print(f'The monitor promoted db02 {time.monotonic() - started:.0f} s after db01 stopped: {formation()}', flush=True)
        project(('monitor', 'monitor', [monitor_task()]), ('second', 'db02', [node_task('db02')]), ('switch', 'monitor', [primary_task('db02')]))
        failed_over = results(run('run'))
        outputs = failed_over['monitor']['module_outputs']
        states = {n['name']: (n['current_state'], n['assigned_state'], n['healthy']) for n in outputs['nodes']}
        print(f'The monitor\'s output nodes after the failover: {states}', flush=True)
        assert outputs['primary_node_name'] == 'db02', outputs
        assert states['db01'][0] == 'primary' and states['db01'][1] not in PRIMARY_STATES and not states['db01'][2], states
        assert states['db02'][0] == states['db02'][1] and states['db02'][1] in PRIMARY_STATES and states['db02'][2], states
        assert failed_over['db02']['module_outputs']['is_primary'] is True, failed_over['db02']
        is_result(failed_over['primary'], 'succeeded', 'unchanged', outputs={'primary_node_name': 'db02'})
        assert sql('db02', 'select pg_is_in_recovery(), count(*) from mmx_orders') == 'f|3'
        # db01 comes back and rejoins as a replica of db02.
        remote('db01', 'systemctl start pgautofailover')
        wait_for_formation({'db01': ('secondary', 'secondary', True), 'db02': ('primary', 'primary', True)}, seconds=300)
        assert sql('db01', 'select pg_is_in_recovery(), count(*) from mmx_orders') == 't|3'
        assert hba_lines('db01')[:6] == initdb_lines('db01')
        project(*formation_stages)
        assert unchanged(run('run'))
        mark('a failover: db01\'s service stops, the monitor promotes db02, and the monitor\'s task names db02 the primary while db01 still reports '
             'primary with another assigned state; db02\'s node task and the switchover task count db02 as the primary; db01 comes back as a replica')

        # The monitor's server is lost and built again from the playbook: a
        # new, empty monitor at the same address. The nodes go on serving,
        # and their tasks register them with it: the primary db02 first,
        # then the replica db01, which waits for a primary.
        remote('monitor', f'systemctl stop pgautofailover; rm -f /etc/systemd/system/pgautofailover.service; systemctl daemon-reload; '
                          f'rm -rf {monitor_data} /var/lib/postgresql/.config/pg_autoctl /var/lib/postgresql/.local/share/pg_autoctl')
        sql('db02', 'insert into mmx_orders values (4);')
        # A fleet stage starts its nodes in the order of their names, so in
        # one that runs a node at a time the replica db01 comes first. Its
        # dryrun does not wait for the primary, and names it.
        project(('monitor', 'monitor', [monitor_task()]))
        run('run')
        project(('first', 'db01', [node_task('db01')]))
        is_result(results(run('dryrun'))['db01'], 'not_checked', 'unknown',
                  f'pg_auto_failover_node: the monitor at {address["monitor"]}:5432 has no record of the replica db01 and lists no primary of the formation '
                  'default that has reported to it: a replica registers after its primary, so that the monitor does not make it a primary in an empty '
                  f'formation. The replica receives its changes from the primary at {address["db02"]}. Register the primary first: run the playbook with '
                  '--limit and the primary\'s node, then run it again')
        project(('monitor', 'monitor', [monitor_task()]), ('second', 'db02', [dict(node_task('db02'), time_limit='10m')]),
                ('first', 'db01', [dict(node_task('db01'), time_limit='10m')]))
        registered = results(run('run'))
        is_result(registered['db02'], 'succeeded', 'changed', outputs={'is_primary': True})
        assert registered['db02']['reason'].startswith(f'registered the node db02, the primary, with the monitor at {address["monitor"]}:5432, which had no record of it'), registered['db02']
        is_result(registered['db01'], 'succeeded', 'changed', outputs={'is_primary': False})
        assert registered['db01']['reason'].startswith(f'registered the node db01, a replica, with the monitor at {address["monitor"]}:5432, which had no record of it'), registered['db01']
        wait_for_formation({'db01': ('secondary', 'secondary', True), 'db02': ('primary', 'primary', True)}, seconds=300)
        assert sql('db02', 'select pg_is_in_recovery(), count(*) from mmx_orders') == 'f|4'
        assert sql('db01', 'select pg_is_in_recovery(), count(*) from mmx_orders') == 't|4'
        project(*formation_stages)
        assert unchanged(run('run'))
        mark('the monitor\'s server is built again with an empty monitor: a dryrun of the replica db01 alone names the address of the primary db02, to register first; '
             'the primary db02 registers with it first and stays the primary, the replica db01 registers after it, every row stays, and the usual playbook '
             'is unchanged after')

        # Back to db01 as the primary; then db02's server stops, and the
        # monitor's list without it drops it.
        project(('switch', 'monitor', [primary_task('db01')]))
        run('run')
        wait_for_formation({'db01': ('primary', 'primary', True), 'db02': ('secondary', 'secondary', True)})
        remote('db02', 'systemctl stop pgautofailover')
        deadline = time.monotonic() + 120
        while formation()['db02'][2]:
            assert time.monotonic() < deadline, 'the monitor still reports db02 healthy two minutes after its service stopped'
            time.sleep(1)
        project(('monitor', 'monitor', [monitor_task(node_names=['db01'])]))
        previewed = results(run('dryrun'))['monitor']
        assert previewed['reason'].startswith('would drop the node db02 (') and previewed['module_outputs']['nodes'][0]['name'] == 'db01', previewed
        dropped = results(run('run'))['monitor']
        is_result(dropped, 'succeeded', 'changed', outputs={'primary_node_name': 'db01'})
        assert dropped['reason'].startswith('dropped the node db02 (') and [n['name'] for n in dropped['module_outputs']['nodes']] == ['db01'], dropped
        assert set(formation()) == {'db01'}
        assert unchanged(run('run'))
        # The list without the healthy primary db01: the task refuses.
        project(('monitor', 'monitor', [monitor_task(node_names=[])]))
        refusal = ('pg_auto_failover_monitor: node_names does not list the node db01, which the monitor reports healthy and as the primary (assigned state {state}): '
                   'dropping it would cause a failover, or remove the only copy of the database that runs. Make another node the primary first with '
                   'pg_auto_failover_primary, or list the node')
        db01_state = formation()['db01'][1]
        is_result(results(run('dryrun'))['monitor'], 'not_checked', 'unknown', refusal.format(state=db01_state))
        refused = results(run('run', 1))['monitor']
        assert is_result(refused, 'failed', 'unchanged', refusal.format(state=db01_state)) and refused['failure_kind'] == 'missing_prerequisite', refused
        assert set(formation()) == {'db01'}
        mark('the monitor\'s task drops the stopped node db02, which node_names no longer lists, with --force, and refuses to drop the healthy primary db01')

        # db01 is alone in its formation now: on_a_replica refuses a restart
        # and puts the file back, and on_a_replica_or_a_node_alone restarts
        # PostgreSQL in place.
        alone_refusal = ('pg_auto_failover_node: the node db01 is alone in the formation default, and shared_buffers takes a restart of PostgreSQL, which '
                         'restart_postgresql_for_new_settings: on_a_replica makes only on a replica: run this task with on_a_replica_or_a_node_alone to '
                         'restart this node in place; the earlier settings stay in effect')
        assert sql('db01', 'show shared_buffers') == '136MB'
        project(*settings_stages('on_a_replica', servers=('db01',), shared_buffers='152MB'))
        is_result(results(run('dryrun'))['db01'], 'not_checked', 'unknown', alone_refusal)
        refused = results(run('run', 1))['db01']
        assert is_result(refused, 'failed', 'changed', alone_refusal) and refused['failure_kind'] == 'missing_prerequisite', refused
        assert "shared_buffers = '136MB'" in remote('db01', f'cat {SETTINGS_FILE}') and sql('db01', 'show shared_buffers') == '136MB'
        project(*settings_stages('on_a_replica_or_a_node_alone', servers=('db01',), shared_buffers='152MB'))
        is_result(results(run('run'))['db01'], 'succeeded', 'changed',
                  f'set shared_buffers from 136MB to 152MB in {SETTINGS_FILE}; reloaded PostgreSQL in {data}; restarted PostgreSQL on the node db01, alone in '
                  'the formation default, for shared_buffers', {'restarted_postgresql': True, 'postgresql_settings_waiting_for_a_restart': [], 'is_primary': True})
        assert sql('db01', 'show shared_buffers') == '152MB' and set(formation()) == {'db01'}
        assert sql('db01', 'select pg_is_in_recovery(), count(*) from mmx_orders') == 'f|4'
        assert unchanged(run('run'))
        mark('db01 alone in its formation: on_a_replica refuses the restart with the file put back, and on_a_replica_or_a_node_alone restarts '
             'PostgreSQL in place')

        # db01 alone, broken by hand: its service stops, the module's file
        # gets a value that PostgreSQL refuses, and the service starts. db01
        # reports single, as assigned, while the monitor does not see it as
        # healthy. Its own task counts it as stopped while its PostgreSQL
        # does not answer, writes the fixed value at once, and restarts
        # pgautofailover.service, after which PostgreSQL starts with it.
        remote('db01', f"systemctl stop pgautofailover; sed -i \"s/^shared_buffers = .*/shared_buffers = 'not a size'/\" {SETTINGS_FILE}; "
                       'systemctl start pgautofailover')
        started = time.monotonic()
        while formation()['db01'] != ('single', 'single', False):
            assert time.monotonic() - started < 90, f'the monitor still sees db01 as healthy 90 s after its start: {formation()}'
            time.sleep(1)
        project(*settings_stages('on_a_replica_or_a_node_alone', servers=('db01',), shared_buffers='152MB'))
        started = time.monotonic()
        repaired = results(run('run'))['db01']
        assert time.monotonic() - started < 240, f'db01\'s task waited for its own node: {time.monotonic() - started:.0f} s'
        is_result(repaired, 'succeeded', 'changed', f"set shared_buffers from 'not a size' to 152MB in {SETTINGS_FILE}; restarted pgautofailover.service, as "
                  "the node's PostgreSQL did not run, so that PostgreSQL starts with them", {'restarted_postgresql': True, 'is_primary': True})
        assert sql('db01', 'show shared_buffers') == '152MB' and formation() == {'db01': ('single', 'single', True)}, formation()
        assert sql('db01', 'select pg_is_in_recovery(), count(*) from mmx_orders') == 'f|4'
        mark('db01 alone, with a value in the module\'s file that keeps PostgreSQL from starting: its own task writes the fixed value at once, and '
             'PostgreSQL starts with it')

        # absent and deleted_with_all_its_data on db02, whose keeper the
        # monitor dropped; then on db01, the last node.
        project(('remove', 'db02', [node_task('db02', state='absent')]))
        removed = results(run('run'))['db02']
        is_result(removed, 'succeeded', 'changed', 'stopped and removed the unit pgautofailover.service')
        assert remote('db02', f'test ! -e {UNIT} && test -e {data}/PG_VERSION && systemctl is-active pgautofailover || true') == 'inactive'
        assert unchanged(run('run'))
        project(('remove', 'db02', [node_task('db02', state='deleted_with_all_its_data')]))
        # The monitor dropped db02, so pg_autoctl's --destroy, which asks the
        # monitor first, cannot delete it: the module does, and removes its
        # file of settings, which absent kept.
        is_result(results(run('run'))['db02'], 'succeeded', 'changed', f'deleted the node db02 in {data} with all its data, which the monitor had dropped already; '
                                                                       f'removed {SETTINGS_FILE}')
        assert remote('db02', f'ls -d {data} {SETTINGS_FILE} 2>/dev/null || true') == ''
        assert unchanged(run('run'))
        project(('remove', 'db01', [node_task('db01', state='absent')]))
        is_result(results(run('dryrun'))['db01'], 'succeeded', 'would_change',
                  f'would stop and remove the unit pgautofailover.service; would remove the node db01 from its formation; its data directory {data} stays')
        is_result(results(run('run'))['db01'], 'succeeded', 'changed',
                  f'stopped and removed the unit pgautofailover.service; removed the node db01 from its formation; its data directory {data} stays')
        assert formation() == {} and remote('db01', f'test -e {data}/PG_VERSION && echo kept') == 'kept'
        assert unchanged(run('run'))
        mark('absent stops and removes the unit of a node that the monitor dropped, and drops the last node db01 from its formation, keeping each data '
             'directory; deleted_with_all_its_data deletes it; each rerun is unchanged')

        # The passwords are only in the password files of postgres.
        saved = ''.join(path.read_text(errors='replace') for path in (workspace / '.mmx').rglob('*') if path.is_file() and 'runners' not in path.parts)
        forms = [AUTOCTL_PASSWORD, REPLICATION_PASSWORD, SECOND_AUTOCTL_PASSWORD, SECOND_REPLICATION_PASSWORD]
        forms += [form.replace('\\', '\\\\').replace(':', '\\:') for form in forms]
        forms += [json.dumps(form, ensure_ascii=ascii)[1:-1] for form in list(forms) for ascii in (True, False)]
        for form in sorted(set(forms)):
            for where, text in (('what mmx printed', ''.join(printed)), ('the saved runs', saved)):
                assert form not in text, f'a password is in {where}: ...{text[max(0, text.find(form) - 300):text.find(form) + 60]}'
        for server in GUESTS:
            remote(server, 'mkdir -p /var/tmp/mmx-pg-auto-failover; cat > /var/tmp/mmx-pg-auto-failover/patterns', input='\n'.join(sorted(set(forms))) + '\n')
            found = remote(server, 'grep -rlF -f /var/tmp/mmx-pg-auto-failover/patterns /var/lib/mmx /run/mmx /root /home /tmp /var/log /etc '
                                   '/var/lib/postgresql --exclude=.pgpass --exclude=patterns 2>/dev/null || true\n'
                                   'journalctl --no-pager -q 2>/dev/null | grep -F -f /var/tmp/mmx-pg-auto-failover/patterns || true')
            assert found == '', f'a password is on {server}: {found[:600]}'
        mark('the passwords are in nothing that mmx printed or saved, and on the guests only in the password file of postgres: in no job record, '
             'journal, log, configuration, or data directory')

        # The nodes go first: a node's deletion asks the monitor whether it
        # still lists the node.
        project(('remove', 'db01', [node_task('db01', state='deleted_with_all_its_data')]),
                ('monitor', 'monitor', [{'id': 'monitor', 'pg_auto_failover_monitor': {'data_directory': monitor_data, 'state': 'deleted_with_all_its_data'}}]))
        deleted = results(run('run'))
        is_result(deleted['monitor'], 'succeeded', 'changed',
                  f'stopped and removed the unit pgautofailover.service; deleted the pg_auto_failover monitor in {monitor_data} with all its data')
        is_result(deleted['db01'], 'succeeded', 'changed', f'deleted the node db01 in {data} with all its data, which the monitor had dropped already; '
                                                           f'removed {SETTINGS_FILE}')
        assert remote('monitor', f'ls -d {monitor_data} {UNIT} 2>/dev/null || true') == ''
        assert remote('db01', f'ls -d {data} /var/lib/postgresql/.config/pg_autoctl{data}/pg_autoctl.cfg {SETTINGS_FILE} 2>/dev/null || true') == ''
        assert unchanged(run('run'))
        mark('deleted_with_all_its_data deletes the monitor and the last node with their data directories; a rerun is unchanged')
    finally:
        failing = sys.exc_info()[0] is not None
        for variable in environment:
            vm.ENV.pop(variable, None)
        cleanup()
        if not failing:
            guests_are_as_found()
    (workspace / 'summary.json').write_text(json.dumps(passed, indent=2) + '\n')
    print(f'{len(passed)} pg_auto_failover workflows passed: PostgreSQL {version} on {distribution} {release}', flush=True)


if __name__ == '__main__':
    vm.run_suite(main)
