#!/usr/bin/env python3
"""hostname, time_zone, locale, and update_alternatives through the public CLI, checked with hostnamectl, timedatectl, locale, and update-alternatives."""
import json
import shlex
import shutil

import vm

# Copies of what the suite changes, so that it can put the node back for the
# suites that run after it.
SAVED = '/var/tmp/mmx-node-settings'
HOSTNAME = 'mmx-node-settings.example.com'
LOCALE = 'de_DE.UTF-8'
ALTERNATIVE = 'mmx_node_settings_tool'
LINK = '/usr/local/bin/mmx-node-settings-tool'


def main():
    vm.up()
    node, ssh = vm.connection()
    vm.reset()
    workspace = vm.ROOT / 'node-settings'
    if workspace.exists():
        shutil.rmtree(workspace)
    vm.write_inventory(workspace, {'nodes': {'app': node}}, name='example')
    vm.build()
    mmx = vm.ROOT / 'bin/mmx'
    passed = []

    def remote(script):
        # The suite changes the system locale, which sudo passes on; the
        # checks read the programs' English output.
        return vm.cmd([*ssh, 'sudo -n /bin/sh -c ' + shlex.quote('set -eu\nexport LC_ALL=C\n' + script)]).stdout

    def run(command, expected=None):
        result = vm.cmd([mmx, command, *vm.playbook_args(command), '--json'], cwd=workspace, check=False)
        assert result.returncode in (vm.success_codes(command) if expected is None else (expected,)), (command, result.stdout, result.stderr)
        return json.loads(result.stdout)

    def project(tasks):
        vm.write_project(workspace, project_name='node_settings', node_task_defaults={'retry': {'pause_between_tries': '0s'}},
                         fleet_stages=[{'id': 'configure', 'nodes': 'all', 'node_tasks': tasks}])

    def results(record):
        return record['entries'][0]['response']['results']

    def change(record):
        return record['entries'][0]['response']['change']

    def unchanged_twice():
        assert change(run('run')) == 'unchanged' and change(run('dryrun', 0)) == 'unchanged'

    def mark(message):
        passed.append(message)
        print('PASS ' + message, flush=True)

    distribution = remote('. /etc/os-release; echo "$ID"').strip()
    package_manager = remote('for tool in apt-get dnf; do command -v $tool >/dev/null && { echo $tool; break; }; done').strip()
    package_manager = {'apt-get': 'apt'}.get(package_manager, package_manager)
    uses_locale_gen = package_manager == 'apt'
    original_hostname = remote('hostnamectl --static').strip()
    remote(f'rm -rf {SAVED}\nmkdir -p {SAVED}\n'
           f'for f in /etc/localtime /etc/timezone /etc/hosts /etc/locale.gen /etc/default/locale /etc/locale.conf; do '
           f'if [ -e $f ] || [ -L $f ]; then cp -a $f {SAVED}/$(echo $f | tr / _); fi; done')
    langpack_installed = False

    def cleanup():
        remote(f'hostnamectl set-hostname {shlex.quote(original_hostname)}\n'
               f'for f in /etc/localtime /etc/timezone /etc/hosts /etc/locale.gen /etc/default/locale /etc/locale.conf; do '
               f'saved={SAVED}/$(echo $f | tr / _); if [ -e $saved ] || [ -L $saved ]; then rm -f $f; cp -a $saved $f; fi; done\n'
               f'if command -v update-alternatives >/dev/null; then update-alternatives --remove-all {ALTERNATIVE} >/dev/null 2>&1 || true; fi\n'
               f'rm -f {LINK}\nrm -rf {SAVED}')
        if langpack_installed:
            remote('dnf -y -q remove glibc-langpack-de >/dev/null')

    try:
        # The host name, stored and running; /etc/hosts stays as it is.
        hosts = remote('cat /etc/hosts')
        project([{'hostname': {'name': HOSTNAME}}])
        preview = results(run('dryrun'))[0]
        assert preview['change'] == 'would_change' and preview['value'] == {'hostname_before': original_hostname, 'hostname_after': HOSTNAME}, preview
        assert remote('hostnamectl --static').strip() == original_hostname
        assert results(run('run'))[0]['change'] == 'changed'
        assert remote('hostnamectl --static; cat /proc/sys/kernel/hostname').split() == [HOSTNAME, HOSTNAME]
        assert remote('cat /etc/hosts') == hosts
        unchanged_twice()
        mark(f'hostname sets the stored and the running host name with hostnamectl and leaves /etc/hosts alone; the preview '
             f'changes nothing; reruns unchanged ({distribution})')

        # The time zone; an unknown name fails before any change.
        zone_before = remote('timedatectl show --property=Timezone --value').strip()
        zone = 'America/New_York' if zone_before == 'Europe/Berlin' else 'Europe/Berlin'
        project([{'time_zone': {'name': 'Europe/Berlinn'}}])
        for command in ('dryrun', 'run'):
            failed = results(run(command, 1))[0]
            assert failed['status'] == 'failed' and 'is not a time zone' in failed['reason'], failed
        project([{'time_zone': {'name': zone}}])
        preview = results(run('dryrun'))[0]
        assert preview['value'] == {'time_zone_before': zone_before, 'time_zone_after': zone}, preview
        assert remote('timedatectl show --property=Timezone --value').strip() == zone_before
        applied = results(run('run'))[0]
        link = remote('readlink /etc/localtime; ls -l /etc/localtime; timedatectl show --property=Timezone --value').strip()
        assert link.splitlines()[0].endswith('zoneinfo/' + zone), (link, applied)
        assert remote('date +%Z').strip() in {'Europe/Berlin': ('CET', 'CEST'), 'America/New_York': ('EST', 'EDT')}[zone]
        unchanged_twice()
        mark('time_zone sets the zone with timedatectl (/etc/localtime and date agree); an unknown name fails in a dryrun '
             'and a run before any change; reruns unchanged')

        # A locale and the system default. A second setting of the default
        # stays.
        if uses_locale_gen:
            remote(f"sed -i 's/^{LOCALE} UTF-8/# {LOCALE} UTF-8/' /etc/locale.gen\nlocale-gen >/dev/null")
        if package_manager == 'apt':
            remote('update-locale LANG=en_US.UTF-8 LC_TIME=C.UTF-8')
        else:
            remote('localectl set-locale LANG=en_US.UTF-8 LC_TIME=C.UTF-8')

        def available():
            return 'de_DE.utf8' in remote('locale -a').split()

        def default_locale():
            if package_manager == 'apt':
                text = remote('cat /etc/default/locale')
            else:
                text = remote('localectl status')
            return sorted(word.strip('"') for word in text.split() if word.startswith(('LANG=', 'LC_')))

        tasks = [{'locale': {'name': LOCALE, 'use_as_system_default': True}}]
        # Debian's package locales-all, which Hetzner's Debian images have,
        # holds every locale ready, whatever locale.gen says: the task then
        # only sets the default.
        ready_made = uses_locale_gen and available()
        if package_manager == 'dnf' and not available():
            # The Red Hat family gets a locale from its language's package,
            # which the task names.
            project(tasks)
            preview = results(run('dryrun'))[0]
            assert preview['status'] == 'not_checked' and 'glibc-langpack-de' in preview['reason'], preview
            failed = results(run('run', 1))[0]
            assert 'install the package glibc-langpack-de first with dnf_package' in failed['reason'], failed
            langpack_installed = True
            project([{'dnf_package': {'packages': [{'name': 'glibc-langpack-de', 'version': 'newest_when_first_installed'}]}}] + tasks)
            assert [r['change'] for r in results(run('run'))] == ['changed', 'changed']
            project(tasks)
            mark(f'locale names the missing package glibc-langpack-de: not checked in a preview, failed in a run; a dnf_package '
                 f'task first fixes it ({distribution})')
        else:
            assert ready_made or uses_locale_gen != available(), 'the locale was there before the test'
            project(tasks)
            preview = results(run('dryrun'))[0]
            want = {'system_default_before': 'en_US.UTF-8', 'system_default_after': LOCALE}
            if uses_locale_gen and not ready_made:
                want['made_available'] = True
            assert preview['change'] == 'would_change' and preview['value'] == want, preview
            assert (ready_made or available() != uses_locale_gen) and default_locale() == ['LANG=en_US.UTF-8', 'LC_TIME=C.UTF-8']
            run('run')
        assert available(), remote('locale -a')
        assert default_locale() == [f'LANG={LOCALE}', 'LC_TIME=C.UTF-8'], default_locale()
        if uses_locale_gen and not ready_made:
            assert remote(f"grep -c '^{LOCALE} UTF-8$' /etc/locale.gen").strip() == '1'
        unchanged_twice()
        project([{'locale': {'name': 'en_US.UTF-8', 'use_as_system_default': False}}])
        assert change(run('run')) == 'unchanged' and default_locale()[0] == f'LANG={LOCALE}'
        mark(f'locale makes the locale available ({"ready-made with locales-all" if ready_made else "locale.gen and locale-gen" if uses_locale_gen else "its language pack"}) and sets '
             f'LANG as the system default ({"update-locale" if package_manager == "apt" else "localectl"}), keeping LC_TIME; '
             f'use_as_system_default: false leaves the default alone; reruns unchanged')

        # The alternatives system.
        def choice():
            """The mode and the program, as update-alternatives shows them, and where the link points."""
            display = remote(f'update-alternatives --display {ALTERNATIVE}')
            mode = 'manual' if ('manual mode' in display or 'status is manual' in display) else 'automatic'
            return mode, remote(f'readlink /etc/alternatives/{ALTERNATIVE}').strip(), remote(f'readlink {LINK}').strip()

        def register(path, priority):
            return {'update_alternatives': {'name': ALTERNATIVE, 'program_path': path, 'generic_command_path': LINK, 'priority_when_registered': priority}}

        project([register('/usr/bin/true', 10)])
        preview = results(run('dryrun'))[0]
        assert preview['change'] == 'would_change' and preview['value']['program_path_after'] == '/usr/bin/true', preview
        assert remote(f'test -e /etc/alternatives/{ALTERNATIVE} && echo yes || echo no').strip() == 'no'
        run('run')
        assert choice() == ('manual', '/usr/bin/true', f'/etc/alternatives/{ALTERNATIVE}'), choice()
        unchanged_twice()
        project([register('/usr/bin/false', 20)])
        assert results(run('run'))[0]['value'] == {'program_path_before': '/usr/bin/true', 'automatic_before': False,
                                                    'program_path_after': '/usr/bin/false', 'automatic_after': False}
        assert choice()[:2] == ('manual', '/usr/bin/false')
        project([{'update_alternatives': {'name': ALTERNATIVE, 'program_path': '/usr/bin/true'}}])
        run('run')
        assert choice()[:2] == ('manual', '/usr/bin/true')
        # Automatic mode: the highest priority wins.
        project([{'update_alternatives': {'name': ALTERNATIVE, 'automatic': True}}])
        assert results(run('dryrun'))[0]['value']['program_path_after'] == '/usr/bin/false'
        run('run')
        assert choice()[:2] == ('automatic', '/usr/bin/false')
        unchanged_twice()
        mark('update_alternatives registers a choice with generic_command_path and priority_when_registered, sets it in manual mode, switches between '
             'registered choices, and returns to automatic mode, where the highest priority wins; reruns unchanged')

        # A path that is not registered: not checked in a preview, and a
        # failed run lists the choices. Once a package registers it,
        # resume finishes the run.
        project([{'hostname': {'name': HOSTNAME}}, {'update_alternatives': {'name': ALTERNATIVE, 'program_path': '/usr/bin/env'}}])
        assert [r['status'] for r in results(run('dryrun'))] == ['succeeded', 'not_checked']
        failed = run('run', 1)
        reason = results(failed)[1]['reason']
        assert '/usr/bin/env is not a registered choice' in reason and '/usr/bin/false' in reason and '/usr/bin/true' in reason, reason
        assert choice()[:2] == ('automatic', '/usr/bin/false')
        remote(f'update-alternatives --install {LINK} {ALTERNATIVE} /usr/bin/env 5 >/dev/null')
        resumed = run('resume')
        assert resumed['id'] == failed['id'] and resumed['status'] == 'succeeded', resumed
        assert choice()[:2] == ('manual', '/usr/bin/env')
        mark('a path that is not a registered choice is not checked in a preview and fails a run, listing the choices; '
             'after the choice is registered, mmx resume sets it')
    finally:
        cleanup()
    print(f'{len(passed)} node settings workflows passed', flush=True)


if __name__ == '__main__':
    vm.run_suite(main)
