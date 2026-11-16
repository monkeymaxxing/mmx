#!/usr/bin/env python3
"""systemd_scheduled_job and cron_job through the public CLI; node state
checked with systemctl, the unit and cron files, the jobs' own output, and
cron running the entry."""
import json
import shlex
import time

import vm

ACCOUNT = 'mmx-job'
WORK = '/var/tmp/mmx-jobs'
REPORT = 'mmx-job-report'        # a cron_schedule, an account, env with a secret
WEEKLY = 'mmx-job-weekly'        # a systemd_calendar_schedule, as root
CRON = 'mmx-cron-sample'
SECRET = 'job-secret-sentinel $HOME "x" `y`'
UNITS = '/etc/systemd/system'
ENVIRONMENT_FILE = f'/etc/mmx/systemd_scheduled_jobs/{REPORT}.env'


def main():
    vm.up()
    node, ssh = vm.connection()
    vm.reset()
    workspace = vm.ROOT / 'scheduled-jobs' / time.strftime('%Y%m%d-%H%M%S')
    workspace.mkdir(parents=True)
    vm.write_inventory(workspace, {'nodes': {'app': node}}, name='example')
    vm.build()
    mmx = vm.ROOT / 'bin/mmx'
    vm.ENV['MMX_JOB_TEST_SECRET'] = SECRET
    passed, printed = [], []

    def remote(script):
        return vm.cmd([*ssh, 'sudo -n /bin/sh -c ' + shlex.quote('set -eu\n' + script)]).stdout

    def run(command, expected=None):
        p = vm.cmd([mmx, command, *vm.playbook_args(command), '--json'], cwd=workspace, check=False)
        printed.append(p.stdout + p.stderr)
        assert p.returncode in (vm.success_codes(command) if expected is None else (expected,)), (command, p.returncode, p.stdout, p.stderr)
        return json.loads(p.stdout)

    def human(command):
        p = vm.cmd([mmx, command, *vm.playbook_args(command), '--diff'], cwd=workspace, check=False)
        printed.append(p.stdout + p.stderr)
        return p.stdout

    def project(tasks):
        vm.write_project(workspace, project_name='scheduled_jobs', node_task_defaults={'retry': {'pause_between_tries': '0s'}},
                         secrets_from_env={'report_token': 'MMX_JOB_TEST_SECRET'},
                         fleet_stages=[{'id': 'configure', 'nodes': 'all', 'node_tasks': tasks}])

    def results(record):
        return record['entries'][0]['response']['results']

    def task(record, task_id):
        return next(r for r in results(record) if r['task'].endswith(task_id))

    def change(record):
        return record['entries'][0]['response']['change']

    def mark(message):
        passed.append(message)
        print('PASS ' + message, flush=True)

    def exists(path):
        return remote(f'test -e {path} && echo yes || echo no').strip() == 'yes'

    def show(unit, prop):
        return remote(f'systemctl show --value --property={prop} {unit}').strip()

    def cleanup():
        remote(f'for timer in {REPORT} {WEEKLY}; do systemctl disable --now $timer.timer 2>/dev/null || true; done\n'
               f'rm -f {UNITS}/{REPORT}.* {UNITS}/{WEEKLY}.* /etc/cron.d/mmx-* /etc/systemd/system/mmx-job-*\n'
               'rm -rf /etc/mmx/systemd_scheduled_jobs\n'
               'systemctl daemon-reload\n'
               f'userdel {ACCOUNT} 2>/dev/null || true\nrm -rf {WORK}')

    distribution = remote('. /etc/os-release; echo "$ID ${VERSION_ID:-}"').strip()
    manager = remote('for tool in apt-get dnf; do command -v $tool >/dev/null && { echo $tool; break; }; done').strip()
    module, package, daemon = {'apt-get': ('apt_package', 'cron', 'cron'), 'dnf': ('dnf_package', 'cronie', 'crond')}[manager]
    install_cron = [{'id': 'cron_package', module: {'packages': [{'name': package, 'version': 'newest_when_first_installed'}]}},
                    {'id': 'cron_daemon', 'systemd_service': {'name': daemon, 'start_at_boot': True, "state": "running"}}]
    report = {'id': 'report', 'systemd_scheduled_job': {
        'name': REPORT, 'cron_schedule': '10 0 * * *', 'run_as_user_account': ACCOUNT, 'run_from_directory': WORK,
        'script': ('printf \'user=%s dir=%s plain=%s token=%s\\n\' "$(id -un)" "$PWD" "$PLAIN" "$TOKEN" > report.out\n'
                   'echo "100% done, \\"quoted\\" \\\\ and $((6*7))" >> report.out\n'
                   'echo "report job ran"'),
        'env': {'PLAIN': 'a b', 'TOKEN': '{{ .secrets.report_token }}'},
        'random_delay': '15m', 'stop_a_run_after': '2h', 'run_missed_job_after_downtime': True}}
    weekly = {'id': 'weekly', 'systemd_scheduled_job': {
        'run_as_user_account': 'root', 'name': WEEKLY, 'systemd_calendar_schedule': 'Mon *-*-* 04:30:00', 'script': f'date +%s > {WORK}/weekly.out'}}
    cron = {'id': 'cron', 'cron_job': {
        'name': CRON, 'cron_schedule': '* * * * *', 'run_as_user_account': ACCOUNT,
        'command': f"printf '%s\\n' 100% > {WORK}/cron.out",
        'env': {'MAILTO': '', 'GREETING': ' padded '}}}
    account = [{'user': {'name': ACCOUNT, 'system': True, 'home': WORK, 'create_home_directory': True}}]
    cleanup()
    try:
        # cron_job needs cron; a dryrun on a node without it is not checked,
        # and a package task first in the same run installs it.
        if remote('{ command -v cron || command -v crond; } >/dev/null 2>&1 && test -d /etc/cron.d && echo yes || echo no').strip() == 'no':
            project([cron])
            preview = task(run('dryrun'), 'cron')
            assert preview['status'] == 'not_checked' and 'cron (apt, zypper), cronie (dnf)' in preview['reason'], preview
            failed = run('run', 1)
            assert 'cron is not installed' in task(failed, 'cron')['reason'], failed
            mark(f'without cron, cron_job is not checked in a dryrun and fails a run with the package to install ({distribution})')
        else:
            mark(f'cron is part of this node\'s base system ({distribution})')

        missing_account = {k: v for k, v in weekly['systemd_scheduled_job'].items() if k != 'run_as_user_account'}
        project([{'systemd_scheduled_job': missing_account}])
        invalid = vm.cmd([mmx, 'validate', 'configure'], cwd=workspace, check=False)
        assert invalid.returncode != 0 and 'run_as_user_account is required' in invalid.stdout + invalid.stderr, invalid
        mark('systemd_scheduled_job requires an explicit account; root is an ordinary value')

        # cron and the jobs' account first: a dryrun cannot check a service
        # that a package task of the same run would install.
        project([*install_cron, *account])
        run('run')

        # A dryrun shows the translated schedule, the next start, and the
        # unit differences, and changes nothing.
        project([*install_cron, *account, weekly])
        preview = run('dryrun')
        planned = task(preview, 'weekly')
        assert planned['change'] == 'would_change', planned
        assert planned['value']['systemd_calendar_expression'] == 'Mon *-*-* 04:30:00' and planned['value']['next_start_time'].startswith('Mon '), planned
        assert f'{UNITS}/{WEEKLY}.service: create' in planned['diff'] and '+OnCalendar=Mon *-*-* 04:30:00' in planned['diff'], planned
        assert not exists(f'{UNITS}/{WEEKLY}.timer')
        text = human('dryrun')
        assert '+ExecStart=/bin/bash -c "date +%%s > ' in text, text
        mark('a dryrun shows the calendar expression, the next start time, and the unit differences, and writes nothing')

        # The job runs as a later fleet stage step fails: resume finishes it.
        project([*install_cron, *account, report, weekly, cron,
                 {'id': 'later', 'cron_job': dict(cron['cron_job'], name='mmx-cron-later', run_as_user_account='mmx-job-later')}])
        failed = run('run', 1)
        assert task(failed, 'later')['status'] == 'failed', failed
        assert remote(f'systemctl is-enabled {REPORT}.timer; systemctl is-active {REPORT}.timer').split() == ['enabled', 'active']
        remote('useradd --system -M mmx-job-later')
        resumed = run('resume')
        assert resumed['id'] == failed['id'] and resumed['status'] == 'succeeded', resumed
        assert exists('/etc/cron.d/mmx-cron-later')
        # cron starts the entry at the next full minute, and userdel refuses
        # an account that has a process: wait until the job has ended.
        remote("rm -f /etc/cron.d/mmx-cron-later; timeout 20 sh -c 'until userdel mmx-job-later; do sleep 0.2; done'")
        project([*install_cron, *account, report, weekly, cron])
        mark('after a later task failed for a missing account, mmx resume writes its cron file and keeps the jobs')

        # The node's own systemd: units, timer state, loaded schedule, settings.
        for name, calendar in ((REPORT, '*-*-* 00:10:00'), (WEEKLY, 'Mon *-*-* 04:30:00')):
            assert remote(f'systemctl is-enabled {name}.timer; systemctl is-active {name}.timer').split() == ['enabled', 'active'], name
            assert f'OnCalendar={calendar} ;' in show(f'{name}.timer', 'TimersCalendar'), show(f'{name}.timer', 'TimersCalendar')
            listed = remote(f'systemctl list-timers --all --no-legend {name}.timer')
            assert f'{name}.timer' in listed and f'{name}.service' in listed, listed
            for unit in ('service', 'timer'):
                assert remote(f'head -n 1 {UNITS}/{name}.{unit}').startswith('# Managed by MMX: systemd_scheduled_job.')
                assert remote(f'stat -c "%a %U %G" {UNITS}/{name}.{unit}').strip() == '644 root root'
        assert show(f'{REPORT}.timer', 'Persistent') == 'yes' and show(f'{WEEKLY}.timer', 'Persistent') == 'no'
        assert show(f'{REPORT}.timer', 'RandomizedDelayUSec') == '15min'
        assert show(f'{REPORT}.service', 'TimeoutStartUSec') == '2h' and show(f'{REPORT}.service', 'Type') == 'oneshot'
        assert show(f'{REPORT}.service', 'User') == ACCOUNT and show(f'{REPORT}.service', 'WorkingDirectory') == WORK
        mark('systemd_scheduled_job writes a oneshot service and a timer with the marker line, verified, and the timer is '
             'enabled and active with the translated schedule, random delay, missed runs, and time limit loaded')

        # The script runs exactly as written, as the account, in its
        # directory, with the env values; its output goes to the journal.
        remote(f'systemctl start {REPORT}.service; systemctl start {WEEKLY}.service')
        out = remote(f'cat {WORK}/report.out')
        assert out == f'user={ACCOUNT} dir={WORK} plain=a b token={SECRET}\n100% done, "quoted" \\ and 42\n', repr(out)
        assert remote(f'cat {WORK}/weekly.out').strip().isdigit()
        # journald may store the line a moment after the job ends.
        journal = remote(f'for i in $(seq 20); do journalctl --no-pager -q -u {REPORT}.service 2>/dev/null | grep -q "report job ran" && break; sleep 0.5; done\n'
                         f'journalctl --no-pager -q -u {REPORT}.service 2>/dev/null || true')
        assert 'report job ran' in journal, journal
        mark('the service runs the script with bash as the account, in run_from_directory, with env; %, $, quotes, and '
             'backslashes reach bash as written')

        # The secret is only in the root-only environment file.
        assert remote(f'stat -c "%a %U %G" {ENVIRONMENT_FILE}').strip() == '600 root root'
        assert remote('stat -c "%a %U %G" /etc/mmx/systemd_scheduled_jobs').strip() == '700 root root'
        assert 'sentinel' not in remote(f'cat {UNITS}/{REPORT}.service') and f'EnvironmentFile={ENVIRONMENT_FILE}' in remote(f'cat {UNITS}/{REPORT}.service')
        saved = ''.join(p.read_text(errors='replace') for p in (workspace / '.mmx').rglob('*') if p.is_file())
        assert 'sentinel' not in ''.join(printed) and 'sentinel' not in saved
        mark('a secret in env is only in a root-only environment file (0600, directory 0700), never in the unit, the output, or saved runs')

        # cron_job: the file, and cron running the entry with % reaching the command.
        cron_file = remote(f'cat /etc/cron.d/{CRON}')
        assert cron_file == (f'# Managed by MMX: cron_job. Written by the node task for {CRON}; MMX replaces a change made here on its next run.\n'
                             f'GREETING=" padded "\nMAILTO=""\n* * * * * {ACCOUNT} printf \'\\%s\\n\' 100\\% > {WORK}/cron.out\n'), repr(cron_file)
        assert remote(f'stat -c "%a %U %G" /etc/cron.d/{CRON}').strip() == '644 root root'
        deadline = time.monotonic() + 150
        while remote(f'cat {WORK}/cron.out 2>/dev/null || true') != '100%\n':
            assert time.monotonic() < deadline, remote(f'ls -la {WORK}; systemctl status {daemon} --no-pager || true')
            time.sleep(5)
        mark('cron_job writes /etc/cron.d/<name> (root, 0644, marker line, env lines), and cron runs the entry with % as written')

        again = run('run')
        assert change(again) == 'unchanged', [r for r in results(again) if r['change'] != 'unchanged']
        assert change(run('dryrun', 0)) == 'unchanged'
        mark('an unchanged rerun and dryrun change nothing')

        # In a node task block's changes, the cron file is a tracked input: a
        # command that follows the block's tracked inputs runs again only
        # after the file changed.
        def with_block(entry):
            note = {'id': 'note', 'shell': {'script': f'echo activated >> {WORK}/activations', 'only_when_tracked_inputs_changed': True}}
            return [*install_cron, *account, report, weekly, {'task_block': {'id': 'cron_block', 'changes': [entry], 'activate': [note]}}]

        def activations():
            return remote(f'cat {WORK}/activations 2>/dev/null || true').count('activated')

        project(with_block(cron))
        run('run')
        before = activations()
        run('run')
        assert activations() == before
        project(with_block({'id': 'cron', 'cron_job': dict(cron['cron_job'], env={'MAILTO': 'ops@example.com', 'GREETING': ' padded '})}))
        run('run')
        assert activations() == before + 1 and 'MAILTO=ops@example.com' in remote(f'cat /etc/cron.d/{CRON}')
        project([*install_cron, *account, report, weekly, cron])
        run('run')
        mark('in a node task block, a changed cron file runs the command that follows the tracked inputs, and an unchanged one does not')

        # A changed schedule is written, reloaded, and loaded by the timer;
        # a timer stopped and disabled by hand is enabled and started again.
        report['systemd_scheduled_job']['cron_schedule'] = '*/20 6-18 * * mon-fri'
        project([*install_cron, *account, report, weekly, cron])
        remote(f'systemctl disable --now {WEEKLY}.timer')
        preview = run('dryrun')
        assert 'would enable and start' in task(preview, 'weekly')['reason'], task(preview, 'weekly')
        assert task(preview, 'report')['change'] == 'would_change'
        assert remote(f'systemctl is-active {WEEKLY}.timer || true').strip() == 'inactive'
        run('run')
        assert 'OnCalendar=Mon..Fri *-*-* 06..18:00/20:00 ;' in show(f'{REPORT}.timer', 'TimersCalendar'), show(f'{REPORT}.timer', 'TimersCalendar')
        assert remote(f'systemctl is-enabled {WEEKLY}.timer; systemctl is-active {WEEKLY}.timer').split() == ['enabled', 'active']
        mark('a changed cron_schedule reaches the running timer; a timer stopped and disabled by hand is enabled and started again')

        # A unit edited by hand and loaded: the next run writes the file again
        # and makes systemd load the job's schedule.
        remote(f'sed -i "s/^OnCalendar=.*/OnCalendar=daily/" {UNITS}/{WEEKLY}.timer\nsystemctl daemon-reload')
        assert 'OnCalendar=*-*-* 00:00:00 ;' in show(f'{WEEKLY}.timer', 'TimersCalendar')
        fixed = task(run('run'), 'weekly')
        assert fixed['change'] == 'changed' and '-OnCalendar=daily' in fixed['diff'], fixed
        assert 'OnCalendar=Mon *-*-* 04:30:00 ;' in show(f'{WEEKLY}.timer', 'TimersCalendar')
        mark('a unit changed by hand is written again and its schedule loaded')

        # Rejected inputs fail before any connection.
        for schedule, message in [({'cron_schedule': '0 0 1 * 1'}, 'restricts the day of the month'),
                                  ({'cron_schedule': '@daily'}, 'write systemd_calendar_schedule instead'),
                                  ({}, 'requires exactly one of cron_schedule')]:
            job = {k: v for k, v in weekly['systemd_scheduled_job'].items() if k != 'systemd_calendar_schedule'}
            project([{'systemd_scheduled_job': dict(job, **schedule)}])
            p = vm.cmd([mmx, 'validate', 'configure'], cwd=workspace, check=False)
            assert p.returncode != 0 and message in p.stdout + p.stderr, (bad, p.stdout, p.stderr)
        project([{'cron_job': dict(cron['cron_job'], env={'TOKEN': '{{ .secrets.report_token }}'})}])
        p = vm.cmd([mmx, 'validate', 'configure'], cwd=workspace, check=False)
        assert p.returncode != 0 and 'a secret cannot go into a cron file' in p.stdout + p.stderr, p.stdout + p.stderr
        project([{'id': 'bad_calendar', 'systemd_scheduled_job': dict(weekly['systemd_scheduled_job'], systemd_calendar_schedule='Mon *-*-* 25:00:00')}])
        refused = task(run('run', 1), 'bad_calendar')
        assert 'systemd-analyze calendar refuses the schedule' in refused['reason'], refused
        mark('a schedule that restricts both days, an @ word, no schedule, and a secret in a cron file fail validation; '
             'systemd-analyze calendar refuses a bad calendar on the node')

        # Removal refuses files without the marker line and removes the rest.
        remote('printf "0 1 * * * root /bin/true\\n" > /etc/cron.d/mmx-unmarked')
        project([{'cron_job': {'name': 'mmx-unmarked', 'state': 'absent'}}])
        refused = results(run('run', 1))[0]
        assert 'refusing to remove /etc/cron.d/mmx-unmarked' in refused['reason'] and exists('/etc/cron.d/mmx-unmarked'), refused
        remote('rm /etc/cron.d/mmx-unmarked')
        absent = [{'systemd_scheduled_job': {'name': REPORT, 'state': 'absent', "run_as_user_account": "root"}}, {'systemd_scheduled_job': {'name': WEEKLY, 'state': 'absent', "run_as_user_account": "root"}},
                  {'cron_job': {'name': CRON, 'state': 'absent'}}]
        project(absent)
        assert change(run('dryrun')) == 'would_change' and exists(f'{UNITS}/{REPORT}.timer')
        assert remote(f'systemctl is-active {REPORT}.timer').strip() == 'active'
        run('run')
        for path in (f'{UNITS}/{REPORT}.service', f'{UNITS}/{REPORT}.timer', f'{UNITS}/{WEEKLY}.timer', ENVIRONMENT_FILE, f'/etc/cron.d/{CRON}'):
            assert not exists(path), path
        for name in (REPORT, WEEKLY):
            assert show(f'{name}.timer', 'LoadState') == 'not-found' and show(f'{name}.timer', 'ActiveState') == 'inactive', name
            assert f'{name}.timer' not in remote('systemctl list-timers --all --no-legend')
        assert change(run('run')) == 'unchanged'
        mark('state: absent stops and disables the timer and removes both units, the environment file, and the cron file; '
             'a file without the marker line is refused; a rerun is unchanged')
    finally:
        cleanup()
        vm.ENV.pop('MMX_JOB_TEST_SECRET', None)
    print(f'{len(passed)} scheduled job workflows passed', flush=True)


if __name__ == '__main__':
    vm.run_suite(main)
