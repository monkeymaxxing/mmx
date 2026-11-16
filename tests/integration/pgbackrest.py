#!/usr/bin/env python3
"""pgbackrest_stanza and pgbackrest_restore through the public CLI, with a
pg_auto_failover node as the database, on three guests: the database server
db01 on mmx-dev, the new database server on mmx-peer, and the monitor on
mmx-third. The suite checks the stanza with pgbackrest and the restored
rows with psql themselves.

On Debian 13 and Ubuntu 26.04 (pgBackRest 2.55 and 2.58) the repository is
an S3-compatible bucket that keeps versions: Ubuntu's and Debian's
python3-moto behind nginx for TLS, on mmx-third. There the suite also
restores with read_repository_as_of after it deleted the backup from the
bucket. Debian 12 and Ubuntu 24.04 (pgBackRest 2.45 and 2.50) have no
python3-moto: the repository is a directory on mmx-dev, which the suite
copies to mmx-peer, as a shared filesystem would give it, and
read_repository_as_of is a missing prerequisite that names the version.

db01 listens at mmx-dev's address alone (listen_address), which pg_autoctl
writes into the data directory's postgresql-auto-failover.conf, and the
backup carries it. The database server is lost by hand: its service, data
directory, and pg_autoctl's files go. The monitor's task drops the
unhealthy primary. A restore to a time goes into mmx-dev's emptied data
directory, as a restore drill would, and a rerun with another target is
refused. The restore of the whole archive goes to mmx-peer, which lacks
mmx-dev's address: the module starts the cluster on its local socket
only, and pg_auto_failover_node registers it as the formation's new
primary at mmx-peer's own address, and replaces a trust line that the
restored pg_hba.conf brings back.

Then a cluster of Debian's own tools, whose configuration is in
/etc/postgresql, is backed up on mmx-dev and restored on mmx-peer into the
same cluster there, which the page's node tasks stop and empty first and
whose service the module starts. A container is enough on every
distribution."""
import json
import shlex
import sys
import time

import vm

# The guest of each server: the lost database server, the new one that
# receives its restore, and the formation's monitor. The new database server
# is not the monitor's: pg_autoctl's unit runs one instance on a server.
GUESTS = {'database': 'mmx-dev', 'new_database': 'mmx-peer', 'monitor': 'mmx-third'}
VERSIONS = {('debian', '12'): 15, ('debian', '13'): 17, ('ubuntu', '24.04'): 16, ('ubuntu', '26.04'): 18}
CONFIG_FILE = '/etc/pgbackrest/pgbackrest.conf'
# Made-up secrets: the bucket's key and its secret, and the two passwords
# of the formation.
S3_KEY = 'mmx-test-key'
S3_KEY_SECRET = 'Sentinel-s3-secret-4711'
AUTOCTL_PASSWORD = 'Sentinel-autoctl-0815'
REPLICATION_PASSWORD = 'Sentinel-replication-2718'
SECRETS = {'secrets_from_env': {'s3_key': 'MMX_TEST_S3_KEY', 's3_key_secret': 'MMX_TEST_S3_KEY_SECRET',
                                'autoctl_node_password': 'MMX_TEST_AUTOCTL_NODE_PASSWORD',
                                'pgautofailover_replicator_password': 'MMX_TEST_REPLICATION_PASSWORD'}}
ENVIRONMENT = {'MMX_TEST_S3_KEY': S3_KEY, 'MMX_TEST_S3_KEY_SECRET': S3_KEY_SECRET, 'MMX_TEST_AUTOCTL_NODE_PASSWORD': AUTOCTL_PASSWORD,
               'MMX_TEST_REPLICATION_PASSWORD': REPLICATION_PASSWORD}
STORE = '/var/lib/mmx-test-s3'
UNIT = '/etc/systemd/system/pgautofailover.service'
JOB = 'pgbackrest-full-backup'
LEFT_BEHIND = ['/var/lib/postgresql', '/etc/postgresql', '/etc/postgresql-common', '/var/log/postgresql', UNIT, '/etc/pgbackrest',
               '/etc/pgbackrest.conf', '/var/lib/pgbackrest', '/var/log/pgbackrest', '/var/spool/pgbackrest', STORE,
               '/etc/systemd/system/mmx-test-s3.service', '/etc/nginx', '/var/log/nginx', '/var/lib/nginx',
               f'/etc/systemd/system/{JOB}.service', f'/etc/systemd/system/{JOB}.timer', '/var/tmp/mmx-pgbackrest']
INSTALLED_PACKAGES = ("dpkg-query -W -f '${db:Status-Abbrev} ${Package}\\n' | awk '$1 == \"ii\" {print $2}' | sort -u")
# The S3-compatible store: moto's server on the guest's loopback, behind
# nginx, which gives it TLS on port 9000.
STORE_SET_UP = f"""export DEBIAN_FRONTEND=noninteractive
apt-get install -y -q --no-install-recommends python3-moto python3-flask-cors nginx openssl >/dev/null
mkdir -p {STORE}
openssl req -x509 -newkey rsa:2048 -nodes -days 2 -subj /CN=s3.example.net -keyout {STORE}/key.pem -out {STORE}/certificate.pem 2>/dev/null
cat > /etc/systemd/system/mmx-test-s3.service <<'UNIT'
[Unit]
Description=S3-compatible test store of pgbackrest.py
[Service]
ExecStart=/usr/bin/moto_server -H 127.0.0.1 -p 5000
UNIT
rm -f /etc/nginx/sites-enabled/default
cat > /etc/nginx/sites-enabled/mmx-test-s3 <<'CONF'
server {{
  listen 9000 ssl;
  ssl_certificate {STORE}/certificate.pem;
  ssl_certificate_key {STORE}/key.pem;
  client_max_body_size 0;
  location / {{
    proxy_pass http://127.0.0.1:5000;
    proxy_set_header Host $http_host;
    proxy_request_buffering off;
    proxy_buffering off;
  }}
}}
CONF
systemctl daemon-reload
systemctl start mmx-test-s3
systemctl restart nginx
"""
# Python on the store's guest, with boto3 against moto's own port.
S3_CLIENT = f"""import boto3, sys
s3 = boto3.client('s3', endpoint_url='http://127.0.0.1:5000', aws_access_key_id='{S3_KEY}', aws_secret_access_key='{S3_KEY_SECRET}', region_name='us-east-1')
"""


def main():
    connections, nodes = {}, {}
    for server, guest in GUESTS.items():
        vm.up(guest)
        nodes[server], connections[server] = vm.connection(guest)
        vm.reset(guest)
    distribution, release = vm.cmd([*connections['database'], '. /etc/os-release; echo "$ID $VERSION_ID"']).stdout.split()
    version = VERSIONS[(distribution, release)]
    data = f'/var/lib/postgresql/{version}/main'
    monitor_data = f'/var/lib/postgresql/{version}/monitor'
    binaries = f'/usr/lib/postgresql/{version}/bin'
    address = {server: vm.cmd([*connections[server], "ip -4 -o route get 10.0.0.1 | sed -n 's/.* src \\([0-9.]*\\).*/\\1/p'"]).stdout.strip()
               for server in connections}
    workspace = vm.ROOT / 'pgbackrest'
    vm.write_inventory(workspace, {'nodes': nodes, 'groups': {'formation': {'nodes': list(GUESTS), 'vars': {'postgresql_major_version': version}}}})
    vm.build()
    mmx = vm.ROOT / 'bin/mmx'
    passed, printed, state = [], [], {}
    marked = time.monotonic()

    def remote(server, script, input=None, check=True):
        return vm.cmd([*connections[server], 'sudo -n /bin/bash -c ' + shlex.quote('set -eu\n' + script)], input=input, check=check, timeout=900).stdout.strip()

    def mark(message):
        nonlocal marked
        now = time.monotonic()
        passed.append(message)
        print(f'PASS {message} ({now - marked:.0f} s)', flush=True)
        marked = now

    def project(*stages):
        vm.write_project(workspace, project_name='pgbackrest', node_task_defaults={'retry': {'pause_between_tries': '0s'}},
                         fleet_stages=[{'id': stage_id, 'nodes': selected, 'node_tasks': tasks} for stage_id, selected, tasks in stages], **SECRETS)

    def run(command, expected=None):
        result = vm.cmd([mmx, command, *vm.playbook_args(command), '--json'], cwd=workspace, check=False, timeout=1800)
        printed.append(result.stdout + result.stderr)
        if result.returncode not in (vm.success_codes(command) if expected is None else (expected,)):
            raise AssertionError((command, result.returncode, result.stdout[-6000:], result.stderr[-3000:]))
        return json.loads(result.stdout)

    def results(record):
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

    def sql(query, server='database'):
        # On the local socket: the restored cluster listens on nothing else
        # until pg_auto_failover_node registers it.
        return remote(server, f'runuser -u postgres -- {binaries}/psql --no-psqlrc -At -p 5432 -d postgres', input=query)

    def listening(server):
        """The addresses at which a program listens on port 5432."""
        return sorted(line.split()[3].rsplit(':', 1)[0] for line in remote(server, "ss -ltnH '( sport = :5432 )'").splitlines())

    def pgbackrest(arguments):
        return remote('database', f'runuser -u postgres -- pgbackrest --config={CONFIG_FILE} --stanza=main {arguments}')

    def formation():
        rows = json.loads(remote('monitor', f'runuser -u postgres -- pg_autoctl show state --pgdata {monitor_data} --formation default --json'))
        return {row['nodename']: (row['current_group_state'], row['health'] == 1) for row in rows}

    def node_time():
        return remote('database', 'date -u "+%Y-%m-%d %H:%M:%S+00"')

    def restore_task(task_id='restore', **changes):
        inputs = {'stanza': 'main', 'config_file': CONFIG_FILE, 'data_directory': data, 'postgresql_major_version': version, 'recover_to': 'latest'}
        inputs.update(changes)
        return {'id': task_id, 'pgbackrest_restore': inputs, 'time_limit': '20m'}

    def node_task(name, server):
        return {'id': name, 'time_limit': '10m', 'pg_auto_failover_node': {
            'name': name, 'data_directory': data, 'postgresql_major_version': version, 'port': 5432, 'host_name': address[server],
            'listen_address': address[server],
            'monitor_host_name': address['monitor'], 'monitor_port': 5432, 'autoctl_node_password': '{{ .secrets.autoctl_node_password }}',
            'replication_password': '{{ .secrets.pgautofailover_replicator_password }}', 'formation': 'default', 'candidate_priority': 50,
            'replication_quorum': True}}

    def monitor_task(node_names):
        return {'id': 'monitor', 'pg_auto_failover_monitor': {
            'data_directory': monitor_data, 'postgresql_major_version': version, 'port': 5432, 'host_name': address['monitor'],
            'listen_address': address['monitor'],
            'autoctl_node_password': '{{ .secrets.autoctl_node_password }}', 'formation': 'default', 'node_names': node_names, 'nodes_not_listed': 'remove'}}

    def cleanup():
        for server in connections:
            packages, paths = state[server]
            added = ' '.join(sorted(set(remote(server, INSTALLED_PACKAGES).split()) - packages))
            gone = ' '.join(path for path in LEFT_BEHIND if path not in paths)
            remote(server, f"""systemctl disable --now pgautofailover.service {JOB}.timer mmx-test-s3.service nginx.service 2>/dev/null || true
systemctl stop 'postgresql@*' 2>/dev/null || true
pkill -KILL -u postgres 2>/dev/null || true
{f'DEBIAN_FRONTEND=noninteractive apt-get purge -y -q {added} >/dev/null 2>&1 || true' if added else ''}
rm -rf {gone}
userdel postgres 2>/dev/null || true; groupdel postgres 2>/dev/null || true
systemctl daemon-reload; systemctl reset-failed 2>/dev/null || true""")

    def guests_are_as_found():
        for server in connections:
            packages, paths = state[server]
            left = remote(server, f"command -v pgbackrest pg_autoctl || true; ls -d {' '.join(path for path in LEFT_BEHIND if path not in paths)} 2>/dev/null || true")
            now = set(remote(server, INSTALLED_PACKAGES).split())
            if now != packages:
                left += f'\npackages added: {sorted(now - packages)}; gone: {sorted(packages - now)}'
            running = vm.guest('sudo', '-n', 'systemctl', 'is-system-running', name=GUESTS[server], check=False).stdout.strip()
            if running != 'running':
                left += f'\nsystemd is {running}'
            assert left.strip() == '', f'the suite left this on {server}:\n{left}'
        print('The guests are as the suite found them: no PostgreSQL, no pgBackRest, no store, and systemd is running', flush=True)

    for server in connections:
        state[server] = (set(remote(server, INSTALLED_PACKAGES).split()), remote(server, f"ls -d {' '.join(LEFT_BEHIND)} 2>/dev/null || true").split())
        assert remote(server, 'command -v pgbackrest || true') == '', f'{server} already has pgBackRest'
    s3 = remote('monitor', 'apt-cache show python3-moto >/dev/null 2>&1 && echo yes || echo no') == 'yes'
    try:
        vm.ENV.update(ENVIRONMENT)
        stanza = {'id': 'stanza', 'pgbackrest_stanza': {'stanza': 'main', 'config_file': CONFIG_FILE}}

        # Without pgBackRest: both modules wait for the program.
        project(('database', 'database', [stanza, restore_task()]))
        previewed = results(run('dryrun'))
        # The stanza task is not checked either, so it may change the node: the restore task names it.
        for task, module, earlier_task in (('stanza', 'pgbackrest_stanza', None), ('restore', 'pgbackrest_restore', 'stanza')):
            missing_program = f'{module}: the program pgbackrest is not installed; install the package pgbackrest in an earlier node task'
            is_result(previewed[task], 'not_checked', 'unknown', missing_program if earlier_task is None else vm.earlier_change_may_provide(earlier_task, missing_program))
        missing = results(run('run', 1))['stanza']
        assert missing['failure_kind'] == 'missing_prerequisite' and len(missing.get('tries') or [missing]) == 1, missing
        mark('without pgbackrest, a dryrun of each module is not checked and names the program and its package, and a run fails once with a missing prerequisite')

        if s3:
            remote('monitor', STORE_SET_UP)
            remote('monitor', 'for i in $(seq 100); do python3 -c "import urllib.request; urllib.request.urlopen(\'http://127.0.0.1:5000/\')" 2>/dev/null && exit 0; sleep 0.2; done; exit 1')
            remote('monitor', 'python3 -', input=S3_CLIENT + "s3.create_bucket(Bucket='mmx-backups')\n"
                   "s3.put_bucket_versioning(Bucket='mmx-backups', VersioningConfiguration={'Status': 'Enabled'})\n")
            repository = (f'repo1-type=s3\nrepo1-s3-uri-style=path\nrepo1-s3-endpoint={address["monitor"]}\nrepo1-storage-port=9000\n'
                          'repo1-s3-bucket=mmx-backups\nrepo1-s3-region=us-east-1\nrepo1-s3-key={{ .secrets.s3_key }}\n'
                          'repo1-s3-key-secret={{ .secrets.s3_key_secret }}\n'
                          # The test store's certificate is its own; a real bucket's is checked.
                          'repo1-storage-verify-tls=n\nrepo1-path=/production\n')
        else:
            repository = 'repo1-path=/var/lib/pgbackrest\n'
        configuration = f'[global]\n{repository}repo1-retention-full=2\n\n[main]\npg1-path={data}\n'
        packages = [{'name': name, 'version': 'newest_when_first_installed'} for name in (
            'postgresql-{{ .vars.postgresql_major_version }}', 'postgresql-{{ .vars.postgresql_major_version }}-auto-failover', 'pg-auto-failover-cli', 'pgbackrest')]
        set_up = [
            {'directory': {'path': '/etc/postgresql-common', 'permissions_mode': '0755'}},
            {'file': {'path': '/etc/postgresql-common/createcluster.conf', 'content': 'create_main_cluster = false\n', 'permissions_mode': '0644'}},
            {'apt_package': {'packages': packages}},
            {'directory': {'path': '/var/lib/postgresql/{{ .vars.postgresql_major_version }}', 'owner': 'postgres', 'group': 'postgres', 'permissions_mode': '0755'}},
        ]
        database_tasks = [
            node_task('db01', 'database'),
            {'directory': {'path': '/etc/pgbackrest', 'permissions_mode': '0755'}},
            {'file': {'path': CONFIG_FILE, 'content': configuration, 'owner': 'root', 'group': 'postgres', 'permissions_mode': '0640',
                      'secret_readable_by_group': True}},
            {'directory': {'path': '/var/lib/pgbackrest', 'owner': 'postgres', 'group': 'postgres', 'permissions_mode': '0750'}},
            {'task_block': {'id': 'archiving', 'changes': [
                {'line': {'path': f'{data}/postgresql.conf', 'match': '^archive_mode\\s*=', 'line': 'archive_mode = on', 'insert_at_end_of_file': True}},
                {'line': {'path': f'{data}/postgresql.conf', 'match': '^archive_command\\s*=', 'insert_at_end_of_file': True,
                          'line': f"archive_command = 'pgbackrest --config={CONFIG_FILE} --stanza=main archive-push %p'"}},
                {'line': {'path': f'{data}/postgresql.conf', 'match': '^archive_timeout\\s*=', 'line': 'archive_timeout = 60', 'insert_at_end_of_file': True}},
            ], 'activate': [{'systemd_service': {'name': 'pgautofailover', 'state': 'running', 'refresh': 'restart'}}]}},
            # PostgreSQL answers a moment after its restart.
            {'wait_for': {'command_succeeds': '/usr/lib/postgresql/"$POSTGRESQL_MAJOR_VERSION"/bin/pg_isready -q -p 5432',
                          'env': {'POSTGRESQL_MAJOR_VERSION': '{{ .vars.postgresql_major_version }}'}, 'wait_at_most': '60s'}},
            stanza,
            {'systemd_scheduled_job': {'name': JOB, 'run_as_user_account': 'postgres', 'systemd_calendar_schedule': 'Sun *-*-* 01:00:00',
                                       'script': ('if [ "$(psql --no-psqlrc -At -p 5432 -d postgres -c \'select pg_is_in_recovery()\')" = f ]; then\n'
                                                  f'  pgbackrest --config={CONFIG_FILE} --stanza=main --type=full backup\nfi\n')}},
        ]
        production = [('set_up', 'all', set_up), ('monitor', 'monitor', [monitor_task(['db01'])]), ('database', 'database', database_tasks)]
        project(*production)
        run('dryrun')
        applied = results(run('run'))
        is_result(applied['stanza'], 'succeeded', 'changed',
                  f'created the stanza main for PostgreSQL {version} in {data}, and checked that archiving reaches its repositories', {'created': True})
        for command in ('run', 'dryrun'):
            assert unchanged(run(command))
        remote('database', f'systemctl start {JOB}.service')
        info = json.loads(pgbackrest('info --output=json'))[0]
        assert info['status']['code'] == 0 and [backup['type'] for backup in info['backup']] == ['full'], info
        assert sql('show archive_mode') == 'on'
        mark(f'the stanza in {"an S3-compatible bucket that keeps versions" if s3 else "a directory"}: created and checked after archiving by line tasks and a '
             'restart, unchanged in a rerun and a dryrun, which run no check; the scheduled job backs up the primary')

        # Rows before and after the time to restore to; check archives the
        # write-ahead log with the second ones.
        sql('create table mmx_orders (number integer); insert into mmx_orders select generate_series(1, 100);')
        time.sleep(2)
        target = node_time()
        time.sleep(2)
        sql('insert into mmx_orders select generate_series(101, 200);')
        pgbackrest('check')
        time.sleep(2)
        repository_time = node_time()

        # The database server is lost.
        remote('database', f"""systemctl disable --now pgautofailover
rm -f {UNIT}; systemctl daemon-reload
rm -rf {data} /var/lib/postgresql/{version}/backup /var/lib/postgresql/.config/pg_autoctl /var/lib/postgresql/.local/share/pg_autoctl /var/lib/postgresql/.pgpass""")
        deadline = time.monotonic() + 120
        while formation()['db01'][1]:
            assert time.monotonic() < deadline, 'the monitor still reports db01 healthy two minutes after its server was lost'
            time.sleep(1)
        project(('monitor', 'monitor', [monitor_task(['db11'])]))
        dropped = results(run('run'))['monitor']
        assert dropped['reason'].startswith('dropped the node db01 (') and formation() == {}, dropped
        mark('the database server is lost; the monitor\'s task drops the unhealthy primary db01, which the list of nodes leaves out')

        # A restore to the time between the two sets of rows, into the
        # emptied data directory of the lost server, as a restore drill
        # would: the cluster listens on its local socket only.
        project(('database', 'database', [restore_task(recover_to=target)]))
        label = info['backup'][0]['label']
        is_result(results(run('dryrun'))['restore'], 'succeeded', 'would_change',
                  f'would restore the stanza main into {data} to {target} from the backup {label}; would start the restored cluster and wait until its recovery ends',
                  {'restored': True})
        assert remote('database', f'ls -d {data} 2>/dev/null || true') == ''
        restored = results(run('run'))['restore']
        is_result(restored, 'succeeded', 'changed', outputs={'restored': True})
        assert restored['reason'].startswith(f'restored the stanza main into {data} to {target} from the backup {label} in ') and \
            '; started the restored cluster on its local socket only, whose recovery ended in ' in restored['reason'], restored
        assert sql('select count(*), max(number) from mmx_orders') == '100|100' and sql('select pg_is_in_recovery()') == 'f'
        assert listening('database') == []
        # The restored cluster archives nothing into the production stanza.
        assert sql('show archive_mode') == 'off'
        assert unchanged(run('run'))
        mark('pgbackrest_restore restores to a time into the empty data directory: the rows before that time, a cluster out of recovery that archives '
             'nothing and listens on its local socket only; the dryrun names the backup; a rerun is unchanged')

        # A rerun with another target is refused: MMX's record of the
        # restore names its target.
        project(('database', 'database', [restore_task()]))
        another = (f"pgbackrest_restore: {data} holds the module's restore of the stanza main to {target}, and the task asks for the stanza main to the "
                   f'end of its archive (latest). The module never writes over a cluster: to restore again, stop the cluster, empty {data}, and run '
                   'the task again')
        for command in ('dryrun', 'run'):
            refused = results(run(command, 1))['restore']
            assert is_result(refused, 'failed', 'unchanged', another) and refused['failure_kind'] == 'config' and \
                len(refused.get('tries') or [refused]) == 1, refused
        assert sql('select count(*), max(number) from mmx_orders') == '100|100'
        mark('a rerun with another target fails once, in a dryrun as in a run, names both targets, and leaves the restored cluster as it is')

        # A directory with other files is refused, in a dryrun as in a run.
        remote('database', f'install -d -o postgres -g postgres -m 700 /var/lib/postgresql/{version}/elsewhere\ntouch /var/lib/postgresql/{version}/elsewhere/notes')
        refusal = (f'pgbackrest_restore: /var/lib/postgresql/{version}/elsewhere holds files (notes) and no PostgreSQL cluster; '
                   'the module restores only into an empty or missing directory')
        project(('database', 'database', [restore_task(data_directory=f'/var/lib/postgresql/{version}/elsewhere')]))
        for command, code in (('dryrun', 1), ('run', 1)):
            refused = results(run(command, code))['restore']
            assert is_result(refused, 'failed', 'unchanged', refusal) and refused['failure_kind'] == 'config', refused
        remote('database', f'rm -rf /var/lib/postgresql/{version}/elsewhere')
        mark('a data directory that holds other files fails the restore, in a dryrun as in a run, and stays as it was')

        # The new database server gets the configuration file, and with the
        # directory repository a copy of the repository.
        new_server_set_up = [
            {'directory': {'path': '/etc/pgbackrest', 'permissions_mode': '0755'}},
            {'file': {'path': CONFIG_FILE, 'content': configuration, 'owner': 'root', 'group': 'postgres', 'permissions_mode': '0640',
                      'secret_readable_by_group': True}},
        ]
        project(('new_database', 'new_database', new_server_set_up))
        run('run')
        if not s3:
            copied = remote('database', 'tar -C /var/lib -cz pgbackrest | base64 -w0')
            remote('new_database', 'base64 -d | tar -C /var/lib -xz', input=copied)
        if s3:
            # An attacker deletes the backup from the bucket, which keeps the
            # earlier versions of every object.
            remote('monitor', 'python3 -', input=S3_CLIENT + "listed = s3.list_objects_v2(Bucket='mmx-backups', Prefix='production/backup/').get('Contents', [])\n"
                   "for found in listed:\n    s3.delete_object(Bucket='mmx-backups', Key=found['Key'])\n")
            project(('new_database', 'new_database', [restore_task()]))
            is_result(results(run('dryrun'))['restore'], 'not_checked', 'unknown', 'pgbackrest_restore: the stanza main has no backup in its repositories')
            project(('new_database', 'new_database', [restore_task(read_repository_as_of=repository_time, repository=1)]))
            is_result(results(run('dryrun'))['restore'], 'succeeded', 'would_change',
                      f'would restore the stanza main into {data} to the end of its archive, reading the repository 1 as it stood at {repository_time}; '
                      'would start the restored cluster and wait until its recovery ends')
        else:
            project(('new_database', 'new_database', [restore_task(read_repository_as_of=repository_time, repository=1)]))
            pgbackrest_version = remote('new_database', 'pgbackrest version').split()[-1]
            is_result(results(run('dryrun'))['restore'], 'not_checked', 'unknown',
                      f'pgbackrest_restore: pgBackRest {pgbackrest_version} is installed, and read_repository_as_of needs pgBackRest 2.54 or newer; '
                      'install a newer package pgbackrest in an earlier node task')
            project(('new_database', 'new_database', [restore_task()]))
        # The restored settings of pg_autoctl name mmx-dev's address, which
        # mmx-peer lacks.
        restored = results(run('run'))['restore']
        is_result(restored, 'succeeded', 'changed', outputs={'restored': True})
        assert '; started the restored cluster on its local socket only, whose recovery ended in ' in restored['reason'], restored
        assert remote('new_database', f'grep "^listen_addresses" {data}/postgresql-auto-failover.conf') == f"listen_addresses = '{address['database']}'"
        assert sql('select count(*), max(number) from mmx_orders', 'new_database') == '200|200' and listening('new_database') == []
        assert unchanged(run('run'))
        if s3:
            mark('after the backup was deleted from the bucket, a restore without read_repository_as_of finds none, and one that reads the repository '
                 'as it stood before restores every row on another server, which lacks the address in the restored settings; a rerun is unchanged')
        else:
            mark(f'pgBackRest {pgbackrest_version}: read_repository_as_of is a missing prerequisite that names the version; a restore of the whole archive '
                 'restores every row on another server, which lacks the address in the restored settings; a rerun is unchanged')

        # The restored cluster becomes the formation's primary at mmx-peer's
        # own address. Its pg_hba.conf, from a backup taken before the
        # modules replaced initdb's trust lines, holds one again: the
        # registration replaces it.
        remote('new_database', f"sed -i -E 's/^(host[[:space:]]+all[[:space:]]+all[[:space:]]+127\\.0\\.0\\.1\\/32[[:space:]]+)scram-sha-256$/\\1trust/' {data}/pg_hba.conf\n"
                               f'grep -c -E "^host +all +all +127.0.0.1/32 +trust$" {data}/pg_hba.conf')
        project(('new_database', 'new_database', [node_task('db11', 'new_database')]))
        adopted = results(run('run'))['db11']
        is_result(adopted, 'succeeded', 'changed', outputs={'created': True, 'current_state': 'single', 'is_primary': True})
        assert f'replaced trust in {data}/pg_hba.conf with scram-sha-256 (host all all 127.0.0.1/32); reloaded PostgreSQL in {data}' in adopted['reason'], adopted
        assert 'Peer authentication failed' in remote('new_database', f'runuser -u nobody -- {binaries}/psql --no-psqlrc -At -w "host=/var/run/postgresql '
                                                                      'port=5432 dbname=postgres user=postgres" -c "select 1" 2>&1 || true')
        # The monitor reports a new node healthy after its first check of it.
        deadline = time.monotonic() + 60
        while formation() != {'db11': ('single', True)}:
            assert time.monotonic() < deadline, f'the formation is {formation()} a minute after db11 registered'
            time.sleep(1)
        assert sql('select count(*) from mmx_orders', 'new_database') == '200'
        assert remote('new_database', 'systemctl is-active pgautofailover') == 'active'
        assert listening('new_database') == [address['new_database']]
        assert unchanged(run('run'))
        mark('pg_auto_failover_node registers the restored, running cluster as the primary db11 of the formation that has no node, at the new '
             'server\'s own listen_address, and replaces the trust line that its pg_hba.conf brought back; a rerun is unchanged')

        # A cluster of Debian's own tools, as postgresql-<version> creates
        # it: the cluster shop on port 5433 of each database server.
        shop = f'/var/lib/postgresql/{version}/shop'
        shop_configuration = f'/etc/postgresql/{version}/shop/postgresql.conf'
        for server in ('database', 'new_database'):
            remote(server, f'pg_createcluster {version} shop --port 5433 --start >/dev/null')
        shop_stanza = f'\n[shop]\npg1-path={shop}\npg1-port=5433\n'
        project(('database', 'database', [
            {'file': {'path': CONFIG_FILE, 'content': configuration + shop_stanza, 'owner': 'root', 'group': 'postgres', 'permissions_mode': '0640',
                      'secret_readable_by_group': True}},
            {'task_block': {'id': 'shop_archiving', 'changes': [
                {'line': {'path': shop_configuration, 'match': '^archive_mode\\s*=', 'line': 'archive_mode = on', 'insert_at_end_of_file': True}},
                {'line': {'path': shop_configuration, 'match': '^archive_command\\s*=', 'insert_at_end_of_file': True,
                          'line': f"archive_command = 'pgbackrest --config={CONFIG_FILE} --stanza=shop archive-push %p'"}},
            ], 'activate': [{'systemd_service': {'name': f'postgresql@{version}-shop', 'state': 'running', 'refresh': 'restart'}}]}},
            {'wait_for': {'command_succeeds': f'/usr/lib/postgresql/{version}/bin/pg_isready -q -p 5433', 'wait_at_most': '60s'}},
            {'id': 'shop_stanza', 'pgbackrest_stanza': {'stanza': 'shop', 'config_file': CONFIG_FILE}},
        ]))
        is_result(results(run('run'))['shop_stanza'], 'succeeded', 'changed', outputs={'created': True})
        remote('database', f'runuser -u postgres -- pgbackrest --config={CONFIG_FILE} --stanza=shop --type=full backup\n'
                           f'runuser -u postgres -- {binaries}/psql --no-psqlrc -At -p 5433 -d postgres -c "create table shop_orders (number integer); '
                           'insert into shop_orders select generate_series(1, 50);" >/dev/null\n'
                           f'runuser -u postgres -- pgbackrest --config={CONFIG_FILE} --stanza=shop check')
        if not s3:
            copied = remote('database', 'tar -C /var/lib -cz pgbackrest | base64 -w0')
            remote('new_database', 'base64 -d | tar -C /var/lib -xz', input=copied)
        # The page's node tasks on the receiving server: stop the cluster and
        # empty its data directory once, before the restore; the marker file
        # says that the restore was done.
        marker = f'/var/lib/postgresql/{version}/shop_restored_from_backup'
        not_restored = {'not': {'path_exists': marker}}
        project(('restore_shop', 'new_database', [
            {'file': {'path': CONFIG_FILE, 'content': configuration + shop_stanza, 'owner': 'root', 'group': 'postgres', 'permissions_mode': '0640',
                      'secret_readable_by_group': True}},
        ]))
        run('run')
        restore_shop = [
            {'id': 'stop_shop', 'when': not_restored, 'systemd_service': {'name': f'postgresql@{version}-shop', 'state': 'stopped'}},
            {'id': 'empty_shop', 'when': not_restored, 'directory': {'path': shop, 'delete_everything_inside_except': []}},
            {'id': 'restore_shop', 'time_limit': '20m', 'pgbackrest_restore': {
                'stanza': 'shop', 'config_file': CONFIG_FILE, 'data_directory': shop, 'postgresql_major_version': version, 'recover_to': 'latest'}},
            {'file': {'path': marker, 'content': 'pgbackrest_restore restored the cluster shop from the stanza shop.\n', 'permissions_mode': '0644'}},
        ]
        project(('restore_shop', 'new_database', restore_shop))
        previewed = results(run('dryrun'))
        is_result(previewed['restore_shop'], 'not_checked', 'unknown',
                  f'pgbackrest_restore: an earlier node task changes {shop}; a dryrun cannot see the directory that a run restores into')
        restored = results(run('run'))
        is_result(restored['restore_shop'], 'succeeded', 'changed', outputs={'restored': True})
        assert restored['restore_shop']['reason'].startswith(f'restored the stanza shop into {shop} to the end of its archive from the backup ') and \
            f'; started the restored cluster (postgresql@{version}-shop.service), whose recovery ended in ' in restored['restore_shop']['reason'], restored['restore_shop']
        found = remote('new_database', f'runuser -u postgres -- {binaries}/psql --no-psqlrc -At -p 5433 -d postgres -c "select count(*) from shop_orders" '
                                  '-c "select pg_is_in_recovery()" -c "show archive_mode" -c "show config_file"\n'
                                  f'systemctl is-active postgresql@{version}-shop\nls {shop}')
        assert found.splitlines()[:5] == ['50', 'f', 'off', shop_configuration, 'active'] and 'recovery.signal' not in found.split(), found
        assert remote('new_database', f'grep -c "^restore_command = .*pgbackrest .*--stanza=shop " {shop}/postgresql.auto.conf') == '1'
        for command in ('run', 'dryrun'):
            assert unchanged(run(command))
        mark('a cluster of Debian\'s own tools, configuration in /etc/postgresql: backed up on mmx-dev and restored into the stopped and emptied cluster '
             'on mmx-peer, which its service postgresql@<version>-shop starts; pgBackRest\'s settings in postgresql.auto.conf mark the module\'s own restore, '
             'and a rerun and a dryrun are unchanged')

        saved = ''.join(path.read_text(errors='replace') for path in (workspace / '.mmx').rglob('*') if path.is_file() and 'runners' not in path.parts)
        forms = [S3_KEY_SECRET, AUTOCTL_PASSWORD, REPLICATION_PASSWORD]
        for form in forms:
            for where, text in (('what mmx printed', ''.join(printed)), ('the saved runs', saved)):
                assert form not in text, f'a secret is in {where}: ...{text[max(0, text.find(form) - 300):text.find(form) + 60]}'
        for server in connections:
            remote(server, 'mkdir -p /var/tmp/mmx-pgbackrest; cat > /var/tmp/mmx-pgbackrest/patterns', input='\n'.join(forms) + '\n')
            found = remote(server, 'grep -rlF -f /var/tmp/mmx-pgbackrest/patterns /var/lib/mmx /run/mmx /root /home /tmp /var/log /var/lib/postgresql '
                                   '--exclude=.pgpass 2>/dev/null || true\n'
                                   'journalctl --no-pager -q 2>/dev/null | grep -F -f /var/tmp/mmx-pgbackrest/patterns || true')
            assert found == '', f'a secret is on {server}: {found[:600]}'
        mark('the bucket\'s secret and the passwords are in nothing that mmx printed or saved, and on the guests only in the configuration file and the '
             'password file of postgres')
    finally:
        failing = sys.exc_info()[0] is not None
        for variable in ENVIRONMENT:
            vm.ENV.pop(variable, None)
        cleanup()
        if not failing:
            guests_are_as_found()
    (workspace / 'summary.json').write_text(json.dumps(passed, indent=2) + '\n')
    print(f'{len(passed)} pgBackRest workflows passed: PostgreSQL {version} on {distribution} {release}', flush=True)


if __name__ == '__main__':
    vm.run_suite(main)
