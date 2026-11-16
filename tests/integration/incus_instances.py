#!/usr/bin/env python3
"""incus_instance and incus_profile, the official extra modules for Incus,
through the public CLI, against an Incus server that the example project
incus-host installs inside the suite's own guest. The state is checked with
the guest's incus itself (incus list, incus profile list, incus config get)
and with an HTTP request through an instance's proxy device.

The suite first runs the test without the program incus. The whole test
then runs on all six distributions: each has an Incus package or a package
repository that the Incus project names. On Ubuntu 26.04 the suite also
installs the package incus, which adds QEMU to the example's incus-base, as
the modules' pages advise for virtual machines, and makes an instance of the
type virtual machine where the guest has /dev/kvm, which a Hetzner Cloud
server does not. On the other distributions it makes none: their
packages bring QEMU only together with the package incus-agent, or not at
all.

The guest is a virtual machine. incus.py, which makes the guests on a test
server, uses that server's Incus; this suite never touches it. It installs a
second Incus inside its guest and manages only that one. The guest has a
unit file incus-agent.service, through which the test server reaches the
guest, and the package incus-agent replaces that file. So the suite installs
that package only on Ubuntu 26.04, where its package incus requires it, and
puts the guest's own file back when it removes the package.

The instances run the harness's container test program as /sbin/init, from
an image that the suite builds itself: no test fetches a public image.
"""
import io
import json
import shlex
import shutil
import sys
import tarfile
import time

import image_registry
import vm

BASE = '/var/tmp/mmx-incus-instances'
EXAMPLE = 'incus-host'
# The local images: one static program as a container's /sbin/init, and an
# empty disk for a virtual machine, which starts and runs no system.
IMAGE = 'mmx-test'
VIRTUAL_MACHINE_IMAGE = 'mmx-test-vm'
# Where the suite makes a virtual machine instance: the distribution and
# release whose package incus brings QEMU. It also brings incus-agent there.
WITH_VIRTUAL_MACHINES = (('ubuntu', '26.04'),)
# The test program ends on SIGTERM. Incus asks a container to shut down with
# SIGPWR, which the program ignores, so the instances that must shut down
# name SIGTERM. One instance without this key proves the bounded wait.
SHUTS_DOWN = {'raw.lxc': 'lxc.signal.halt=SIGTERM'}
# A configuration value that is a secret of the project.
SECRET_VALUE = 'Sentinel-configuration-value-4711'
SECRETS = {'secrets_from_env': {'shop_token': 'MMX_TEST_SHOP_TOKEN'}}
# With this file, apt installs no recommended packages. Ubuntu 24.04's
# package incus recommends incus-agent, which must not reach the guest; and
# on every apt distribution the file proves that the example names every
# package that Incus needs.
NO_RECOMMENDED_PACKAGES = '/etc/apt/apt.conf.d/90-install-no-recommended-packages'
# The guest's own unit file, which no package of the suite may leave changed.
AGENT_UNIT_OF_THE_GUEST = '/usr/lib/systemd/system/incus-agent.service'
# The start and end of the reason when the program incus is missing. The
# distributions that the message names between them are the module's own
# text, which this suite does not repeat.
MISSING_START = 'the program incus is not installed; install the package incus-base (Debian) or incus'
MISSING_END = ' in an earlier node task'

# What Incus and its packages leave on a node, and what the example adds.
LEFT_BY_INCUS = ['/var/lib/incus', '/var/log/incus', '/var/cache/incus', '/run/incus', '/root/.config/incus', '/root/.cache/incus', '/opt/incus',
                 '/var/lib/lxcfs', '/var/lib/incus-lxcfs', '/etc/default/incus', '/etc/logrotate.d/incus', '/etc/subuid-', '/etc/subgid-']
OF_THE_EXAMPLE = ['/etc/apt/sources.list.d/bookworm-backports.sources',
                  '/etc/yum.repos.d/copr-neelc-incus.repo', '/etc/pki/rpm-gpg/RPM-GPG-KEY-copr-neelc-incus']
# The names of the installed packages; the keys that rpm keeps are no
# packages of the distribution and are compared by themselves.
INSTALLED_PACKAGES = ("if command -v dpkg-query >/dev/null 2>&1; then dpkg-query -W -f '${db:Status-Abbrev} ${Package}\\n' | awk '$1 == \"ii\" {print $2}'; "
                      "else rpm -qa --qf '%{NAME}\\n' | grep -v '^gpg-pubkey$'; fi | sort -u")
RPM_KEYS = "if command -v rpm >/dev/null 2>&1 && ! command -v dpkg-query >/dev/null 2>&1; then rpm -qa 'gpg-pubkey*' | sort; fi"
# Everything of Incus goes while its server still runs, in this order: a
# profile of any project that still names the bridge or the pool keeps both.
EMPTY_INCUS = r'''
if command -v incus >/dev/null 2>&1 && incus query /1.0 >/dev/null 2>&1; then
  # The first column of each list is the name, on every Incus version.
  projects=$(incus project list --format csv | cut -d, -f1 | sed 's/ (current)//')
  for project in $projects; do
    for instance in $(incus list --project "$project" --format csv -c n); do incus delete --force --project "$project" "$instance" </dev/null || true; done
    for image in $(incus image list --project "$project" --format csv -c f); do incus image delete --project "$project" "$image" 2>/dev/null || true; done
    for profile in $(incus profile list --project "$project" --format csv | cut -d, -f1); do
      echo '{}' | incus profile edit --project "$project" "$profile" || true
      [ "$profile" = default ] || incus profile delete --project "$project" "$profile" || true
    done
  done
  for project in $projects; do
    [ "$project" = default ] || incus project delete "$project" </dev/null || true
  done
  for network in $(incus network list --format csv | awk -F, '$3 == "YES" {print $1}'); do incus network delete "$network" || true; done
  for pool in $(incus storage list --format csv | cut -d, -f1); do incus storage delete "$pool" || true; done
fi
'''


def image_metadata(architecture, description):
    return (f'architecture: {architecture}\ncreation_date: 1760000000\nproperties:\n'
            f'  description: {description}\n  os: mmxtest\n  release: "1"\n').encode()


def tarball(entries):
    """A gzip-compressed tar file of (name, bytes or None for a directory, mode) entries."""
    data = io.BytesIO()
    with tarfile.open(fileobj=data, mode='w:gz', format=tarfile.USTAR_FORMAT) as archive:
        for name, content, mode in entries:
            entry = tarfile.TarInfo(name)
            entry.mode = mode
            if content is None:
                entry.type = tarfile.DIRTYPE
                archive.addfile(entry)
            else:
                entry.size = len(content)
                archive.addfile(entry, io.BytesIO(content))
    return data.getvalue()


def main():
    vm.up()
    node, ssh = vm.connection()
    vm.reset()
    workspace = vm.ROOT / 'incus-instances'
    if workspace.exists():
        shutil.rmtree(workspace)
    vm.write_inventory(workspace, {'nodes': {'incus': node}}, name='example')
    vm.build()
    mmx = vm.ROOT / 'bin/mmx'
    passed = []
    marked = time.monotonic()
    # Everything mmx printed, to search for the secret value at the end.
    printed = []

    def remote(script, input=None, check=True, timeout=600):
        """Run a script on the guest as root with root's own home directory, without MMX."""
        return vm.cmd([*ssh, 'sudo -n env HOME=/root /bin/sh -c ' + shlex.quote('set -eu\n' + script)], input=input or '', check=check, timeout=timeout).stdout.strip()

    def incus(*arguments, input=None, check=True):
        """Run the guest's own incus as root, without MMX. Its standard input is never left open: incus init reads it to its end."""
        result = vm.cmd([*ssh, 'sudo -n env HOME=/root incus ' + shlex.join(arguments)], input=input or '', check=False)
        assert not check or result.returncode == 0, (arguments, result.stdout, result.stderr)
        return result

    def instance(name, incus_project='default'):
        """The instance as incus list shows it, or None."""
        listed = json.loads(incus('list', '--project', incus_project, '--format', 'json').stdout)
        return next((entry for entry in listed if entry['name'] == name), None)

    def own_config(name, incus_project='default'):
        """The instance's own configuration keys without Incus's own."""
        return {key: value for key, value in instance(name, incus_project)['config'].items() if not key.startswith(('volatile.', 'image.'))}

    def incus_own_keys(name):
        """The keys that Incus keeps in an instance's configuration and that no edit may lose: where the instance came from, its identity, and its network card's address."""
        return {key: value for key, value in instance(name)['config'].items()
                if key.startswith('image.') or key in ('volatile.base_image', 'volatile.uuid', 'volatile.eth0.hwaddr')}

    def as_configured(name, incus_project='default'):
        """What of an instance must not change while another project's instance of the same name is managed. incus list also shows counters that change by themselves."""
        found = instance(name, incus_project)
        return {key: found[key] for key in ('status', 'type', 'config', 'devices', 'profiles', 'description')}

    def profile(name, incus_project='default'):
        """The profile as incus profile list shows it, or None."""
        listed = json.loads(incus('profile', 'list', '--project', incus_project, '--format', 'json').stdout)
        return next((entry for entry in listed if entry['name'] == name), None)

    def served(port):
        """What the test program of an instance answers through the instance's proxy device. A program that has just started listens a moment later."""
        return json.loads(remote(f'for attempt in 1 2 3 4 5 6 7 8 9 10; do curl -sS --max-time 5 http://127.0.0.1:{port}/ 2>/dev/null && exit 0; sleep 0.3; done; '
                                 f'curl -sS --max-time 5 http://127.0.0.1:{port}/'))

    def project(tasks, **settings):
        vm.write_project(workspace, project_name='incus_instances', node_task_defaults={'retry': {'pause_between_tries': '0s'}},
                         fleet_stages=[{'id': 'configure', 'nodes': 'all', 'node_tasks': tasks}], **settings)

    def run(command, expected=None, directory=None, playbook='configure', options=(), timeout=900):
        result = vm.cmd([mmx, command, *vm.playbook_args(command, playbook), *options, '--json'], cwd=directory or workspace, check=False, timeout=timeout)
        printed.append(result.stdout + result.stderr)
        if result.returncode not in (vm.success_codes(command) if expected is None else (expected,)):
            raise AssertionError((command, result.returncode, result.stdout, result.stderr))
        return json.loads(result.stdout)

    def all_results(record):
        """The results of every node task of a run, of each of its fleet stages."""
        return [result for entry in record['entries'] for result in entry['response']['results']]

    def results(record):
        """The results of a run's node tasks, by the last part of each task's name."""
        return {r['task'].rsplit('/', 1)[-1]: r for r in all_results(record)}

    def is_result(result, status, change, reason=None, created=None):
        """The result has this status and change and is not hidden as a secret; reason is the whole reason."""
        assert result['status'] == status and result['change'] == change and result.get('secret') is not True, result
        if reason is not None:
            assert result.get('reason', '') == reason, result
        if created is not None:
            assert result['module_outputs'] == {'created': created}, result
        return True

    def names_the_missing_program(result, module, earlier_task=None):
        """The reason names the program incus and its packages; in a dryrun after an earlier node task that would change the node, it ends with the hint that names that node task."""
        reason = result.get('reason', '')
        return reason.startswith(module + ': ' + MISSING_START) and reason.endswith(MISSING_END if earlier_task is None else vm.earlier_change_may_provide(earlier_task, MISSING_END))

    def tries(result):
        """How often the node task was tried."""
        return len(result.get('tries') or [result])

    def unchanged(record):
        """No node task of the run changed anything, failed, or stayed unchecked. A task whose when is false is skipped."""
        different = {r['task']: (r['status'], r['change'], r.get('reason')) for r in all_results(record)
                     if r['change'] != 'unchanged' or r['status'] not in ('succeeded', 'skipped')}
        assert not different, different
        return True

    def mark(message):
        # The seconds since the last mark say where the suite's time goes.
        nonlocal marked
        now = time.monotonic()
        passed.append(message)
        print(f'PASS {message} ({now - marked:.0f} s)', flush=True)
        marked = now

    def observed():
        """What of the guest must be the same after the suite, beside its packages and paths."""
        return {
            'subuid': remote('cat /etc/subuid'),
            'subgid': remote('cat /etc/subgid'),
            'agent_unit': remote(f'sha256sum {AGENT_UNIT_OF_THE_GUEST} 2>/dev/null || echo none'),
            'agent_enabled': remote('systemctl is-enabled incus-agent.service 2>/dev/null || true'),
            'accounts': remote('getent passwd incus || true; getent group incus incus-admin || true'),
            'enabled_repositories': remote("if command -v dnf >/dev/null 2>&1; then dnf -q repolist --enabled 2>/dev/null | awk 'NR > 1 {print $1}' | sort | tr '\\n' ' '; fi"),
            'firewall_tables': remote("if command -v nft >/dev/null 2>&1; then nft list tables 2>/dev/null | sort | tr '\\n' ';'; fi"),
        }

    identity = remote('. /etc/os-release; echo "$ID ${VERSION_ID:-}"').split()
    distribution = identity[0]
    apt = distribution in ('debian', 'ubuntu')
    # A virtual machine instance needs /dev/kvm, which a Hetzner Cloud
    # server does not have.
    has_kvm = remote('test -e /dev/kvm && echo yes || echo no') == 'yes'
    with_virtual_machines = tuple(identity[:2]) in WITH_VIRTUAL_MACHINES and has_kvm
    if tuple(identity[:2]) in WITH_VIRTUAL_MACHINES and not has_kvm:
        print('This server has no /dev/kvm: the suite makes no virtual machine instance here', flush=True)
    # The guest's own agent unit, put back after the package incus-agent.
    agent_unit_text = remote(f'cat {AGENT_UNIT_OF_THE_GUEST}') if with_virtual_machines else None
    assert remote('command -v incus || true') == '', 'the guest already has the program incus'
    before = {
        'packages': set(remote(INSTALLED_PACKAGES).split()),
        'rpm_keys': set(remote(RPM_KEYS).split()),
        'paths': remote(f"ls -d {' '.join(LEFT_BY_INCUS + OF_THE_EXAMPLE)} /etc/apt/keyrings 2>/dev/null || true").split(),
        **observed(),
    }

    def cleanup():
        """Leave the guest as the suite found it: everything inside Incus goes
        while its server runs, then its units, exactly the packages that the
        suite's runs added, and what no package removal takes."""
        # The repository that the example switched on is switched off first:
        # the program that does it came with the example's packages.
        if before['enabled_repositories'] and 'crb' not in before['enabled_repositories'].split():
            remote('dnf config-manager --set-disabled crb >/dev/null 2>&1 || true')
        added = ' '.join(sorted(set(remote(INSTALLED_PACKAGES).split()) - before['packages']))
        keys = ' '.join(sorted(set(remote(RPM_KEYS).split()) - before['rpm_keys']))
        paths = ' '.join(path for path in LEFT_BY_INCUS + OF_THE_EXAMPLE if path not in before['paths'])
        remove = f"""if command -v apt-get >/dev/null 2>&1; then DEBIAN_FRONTEND=noninteractive apt-get -o DPkg::Lock::Timeout=300 purge -y -q {added} >/dev/null 2>&1 || true
else dnf remove -y -q {added} >/dev/null 2>&1 || true
fi""" if added else ''
        remote(f"""
incus remote switch local >/dev/null 2>&1 || true
incus project switch default >/dev/null 2>&1 || true
systemctl start incus.socket >/dev/null 2>&1 || true
{EMPTY_INCUS}
systemctl disable --now incus-startup.service incus-user.service incus-user.socket incus.service incus.socket >/dev/null 2>&1 || true
systemctl stop lxcfs.service incus-lxcfs.service >/dev/null 2>&1 || true
for mounted in $(awk '$2 ~ "^/var/lib/(incus|lxcfs|incus-lxcfs)(/|$)" {{print $2}}' /proc/mounts | sort -r); do umount -l "$mounted" 2>/dev/null || true; done
ip link delete incusbr0 2>/dev/null || true
{remove}
{'rpm -e ' + keys + ' || true' if keys else ''}
rm -rf {paths} {BASE} {NO_RECOMMENDED_PACKAGES}
{'' if '/etc/apt/keyrings' in before['paths'] else 'rmdir /etc/apt/keyrings 2>/dev/null || true'}
if ! grep -q . <<'ACCOUNTS'
{before['accounts']}
ACCOUNTS
then
  userdel incus 2>/dev/null || true
  groupdel incus-admin 2>/dev/null || true
  groupdel incus 2>/dev/null || true
fi
if command -v nft >/dev/null 2>&1 && nft list table inet incus >/dev/null 2>&1; then nft delete table inet incus; fi
systemctl daemon-reload
systemctl reset-failed >/dev/null 2>&1 || true
""", timeout=900)
        # Removing the package incus-agent removed the guest's own unit file
        # and stopped the agent; the file and its state come back.
        if agent_unit_text is not None and remote(f'sha256sum {AGENT_UNIT_OF_THE_GUEST} 2>/dev/null || echo none') != before['agent_unit']:
            remote(f'cat > {AGENT_UNIT_OF_THE_GUEST}', input=agent_unit_text + '\n')
            remote('systemctl daemon-reload\n'
                   + ('systemctl enable incus-agent.service >/dev/null 2>&1\n' if before['agent_enabled'] == 'enabled' else '')
                   + 'systemctl restart incus-agent.service')
        # The lines for root that the packages or the example added.
        for name in ('subuid', 'subgid'):
            if remote(f'cat /etc/{name}') != before[name]:
                remote(f'cat > /etc/{name}', input=before[name] + '\n' if before[name] else '')

    def guest_is_as_found():
        """Nothing of Incus or of this suite is left, and the guest's own
        agent unit is the file it was. Everything found is in the one failure."""
        paths = ' '.join(path for path in LEFT_BY_INCUS + OF_THE_EXAMPLE if path not in before['paths'])
        left = remote(f"""
for program in incus incusd lxcfs; do command -v $program || true; done
ls -d {paths} {BASE} {NO_RECOMMENDED_PACKAGES} /run/mmx/private_task_directories/* 2>/dev/null || true
ip -o link show 2>/dev/null | grep -o -E 'incusbr[0-9]+|veth[0-9a-f]+|tap[0-9a-f]+' | sed 's/^/network device /' || true
systemctl list-unit-files 'incus*' 'lxcfs*' --no-legend 2>/dev/null | grep -v '^incus-agent' || true
pgrep -a -f '[d]nsmasq.*incus' | sed 's/^/process /' || true
""")
        now = {'packages': set(remote(INSTALLED_PACKAGES).split()), 'rpm_keys': set(remote(RPM_KEYS).split()), **observed()}
        for what in ('packages', 'rpm_keys'):
            if now[what] != before[what]:
                left += f'\n{what} added: {sorted(now[what] - before[what])}; {what} gone: {sorted(before[what] - now[what])}'
        for what in ('subuid', 'subgid', 'agent_unit', 'agent_enabled', 'accounts', 'enabled_repositories', 'firewall_tables'):
            if now[what] != before[what]:
                left += f'\n{what} was:\n{before[what]}\n{what} is:\n{now[what]}'
        state = vm.guest('sudo', '-n', 'systemctl', 'is-system-running', check=False).stdout.strip()
        if state != 'running':
            left += f'\nsystemd is {state}:\n' + vm.guest('sudo', '-n', '/bin/sh', '-c', vm.FAILED_UNITS, check=False).stdout
        assert left.strip() == '', 'the suite left this on the guest:\n' + left
        print('The guest is as the suite found it: nothing of Incus is left, its own agent unit file is unchanged, and systemd is running', flush=True)

    def example_project():
        """A copy of examples/incus-host with the guest as its node."""
        directory = vm.ROOT / ('incus-instances-' + EXAMPLE)
        if directory.exists():
            shutil.rmtree(directory)
        shutil.copytree(vm.REPO / 'examples' / EXAMPLE, directory)
        vm.write_inventory(directory, {'nodes': {'incus01': node}}, name='example')
        return directory

    def of_the_modules(record):
        """The results of the official extra modules' tasks in a run of the example: the profile's, then the instance's."""
        found = [r for r in all_results(record) if r.get('module_kind') == 'official_extra']
        assert len(found) == 2, [(r['task'], r.get('module_kind')) for r in all_results(record)]
        return found

    try:
        vm.ENV['MMX_TEST_SHOP_TOKEN'] = SECRET_VALUE
        web_profile = {'id': 'profile', 'incus_profile': {'name': 'mmx-web.limits', 'description': 'Limits of the test instances',
                                                           'config': {'limits.cpu': '1'}, 'config_keys_not_listed': 'remove',
                                                           'devices': {'data': {'type': 'disk', 'source': BASE + '/shared', 'path': '/mnt/shared', 'readonly': 'true'}},
                                                           'devices_not_listed': 'remove'}}
        shop = {'instance_type': 'container', 'name': 'mmx-shop', 'state': 'running', 'image_when_created': IMAGE, 'profiles': ['default', 'mmx-web.limits'],
                'config': {**SHUTS_DOWN, 'environment.IMAGE_VERSION': '7'}, 'config_keys_not_listed': 'remove',
                'devices': {'web': {'type': 'proxy', 'listen': 'tcp:127.0.0.1:18080', 'connect': 'tcp:127.0.0.1:8080'}}, 'devices_not_listed': 'remove'}

        # Before Incus is installed: the node has no program incus.
        project([web_profile, {'id': 'instance', 'incus_instance': shop}])
        preview = results(run('dryrun'))
        # The profile task is not checked either, so it may change the node: the instance task names it.
        for task, module, earlier_task in (('profile', 'incus_profile', None), ('instance', 'incus_instance', 'profile')):
            assert is_result(preview[task], 'not_checked', 'unknown') and names_the_missing_program(preview[task], module, earlier_task), preview[task]
        # A run ends at its first failed task, so each module has its own run.
        for task, module in ((web_profile, 'incus_profile'), ({'id': 'instance', 'incus_instance': shop}, 'incus_instance')):
            project([task])
            refused = results(run('run', 1))[task['id']]
            assert refused['failure_kind'] == 'missing_prerequisite' and names_the_missing_program(refused, module) and tries(refused) == 1, refused
        mark('without the program incus, both modules name the program and its package: a dryrun is not checked, and a run fails once as a missing prerequisite')

        # The example project installs Incus and makes it ready, with apt set
        # to install no recommended packages. Its first dryrun passes on a
        # node without Incus: the two Incus tasks are not checked.
        if apt:
            project([{'file': {'path': NO_RECOMMENDED_PACKAGES, 'permissions_mode': '0644', 'content': 'APT::Install-Recommends "false";\n'}}])
            run('run')
        example = example_project()
        first = of_the_modules(run('dryrun', directory=example, playbook='incus_host'))
        assert [r['status'] for r in first] == ['not_checked', 'not_checked'], first
        started = time.monotonic()
        run('run', directory=example, playbook='incus_host', options=['--only-fleet-stages', 'incus_server'], timeout=1800)
        seconds = time.monotonic() - started
        version = remote('incus version')
        added = sorted(set(remote(INSTALLED_PACKAGES).split()) - before['packages'])
        print(f'The example installed Incus and made it ready in {seconds:.0f} s: {" ".join(version.split())}; {len(added)} packages', flush=True)
        # The package incus-agent of Debian and Ubuntu replaces the guest's
        # unit file; the one of the Red Hat family, which its package incus
        # requires, leaves the file alone.
        assert not apt or 'incus-agent' not in added, added
        assert remote(f'sha256sum {AGENT_UNIT_OF_THE_GUEST} 2>/dev/null || echo none') == before['agent_unit'], 'the installation changed the unit file of the guest'
        assert remote('systemctl is-enabled incus.socket; systemctl is-active incus.socket incus.service').split() == ['enabled', 'active', 'active']
        assert remote("grep -c '^root:1000000:1000000000$' /etc/subuid /etc/subgid") == '/etc/subuid:1\n/etc/subgid:1'
        # Root's client settings are in root's home directory, where the
        # modules' commands read them too.
        assert remote('test -f /root/.config/incus/config.yml && echo there') == 'there'
        assert unchanged(run('run', directory=example, playbook='incus_host', options=['--only-fleet-stages', 'incus_server']))
        mark(f'the example {EXAMPLE} installs Incus from the source of this distribution' + (', with apt installing no recommended packages,' if apt else '')
             + ' and makes the server ready; its first dryrun passes on a node without Incus, and a rerun is unchanged')

        # A server that does not answer: stopped, with its socket. The
        # server has no instance yet: what a stopped server does to running
        # instances differs between the distributions' packages.
        remote('systemctl stop incus.service incus.socket')
        project([web_profile, {'id': 'instance', 'incus_instance': shop}])
        preview = results(run('dryrun'))
        assert preview['profile']['status'] == 'not_checked' and preview['profile']['reason'].startswith('incus_profile: the Incus server on this node does not answer: Error: '), preview['profile']
        assert preview['instance']['status'] == 'not_checked' and preview['instance']['reason'].startswith('incus_instance: the Incus server on this node does not answer: Error: '), preview['instance']
        silent = results(run('run', 1))['profile']
        assert silent['status'] == 'failed' and 'does not answer: Error: ' in silent['reason'] and tries(silent) == 3 and 'failure_kind' not in silent, silent
        remote('systemctl start incus.socket incus.service')
        mark('a stopped Incus server: a dryrun is not checked, and a run fails with the message of incus after three tries')

        # The suite's images: the test program as /sbin/init of a container.
        architecture = vm.architecture()
        machine = {'amd64': 'x86_64', 'arm64': 'aarch64'}[architecture]
        program = image_registry.built_test_program(architecture).read_bytes()
        image_file = vm.ROOT / 'incus-instances-test-image.tar.gz'
        image_file.write_bytes(tarball([('metadata.yaml', image_metadata(machine, 'MMX test image'), 0o644), ('rootfs', None, 0o755),
                                        ('rootfs/sbin', None, 0o755), ('rootfs/sbin/init', program, 0o755)]))
        remote(f'mkdir -p {BASE}/shared\nchmod 755 {BASE}\ninstall -d -m 777 {BASE}/incoming')
        vm.copy(image_file, f'{BASE}/incoming/image.tar.gz')
        incus('image', 'import', '-q', f'{BASE}/incoming/image.tar.gz', '--alias', IMAGE)

        # The example's profile and instance, from the local image.
        with_image = ['--var', 'instance_image=' + IMAGE]
        preview = of_the_modules(run('dryrun', directory=example, playbook='incus_host', options=with_image))
        assert is_result(preview[0], 'succeeded', 'would_change', 'would create the profile web; would set the description; would set the configuration keys limits.cpu, limits.memory', created=True)
        assert is_result(preview[1], 'succeeded', 'would_change', f'would create the container shop-test from the image {IMAGE}; would start it', created=True)
        assert profile('web') is None and instance('shop-test') is None
        applied = of_the_modules(run('run', directory=example, playbook='incus_host', options=with_image))
        assert is_result(applied[0], 'succeeded', 'changed', 'created the profile web; set the description; set the configuration keys limits.cpu, limits.memory', created=True)
        assert is_result(applied[1], 'succeeded', 'changed', f'created the container shop-test from the image {IMAGE}; started it', created=True)
        made = instance('shop-test')
        assert made['status'] == 'Running' and made['type'] == 'container' and made['profiles'] == ['default', 'web'], made
        assert own_config('shop-test') == {'boot.autostart': 'true'} and made['expanded_config']['limits.memory'] == '512MiB', made['config']
        assert made['devices'] == {'web': {'type': 'proxy', 'listen': 'tcp:0.0.0.0:8080', 'connect': 'tcp:127.0.0.1:80'}}, made['devices']
        assert profile('web')['description'] == 'Limits for web containers' and profile('web')['config'] == {'limits.cpu': '1', 'limits.memory': '512MiB'}
        for command in ('run', 'dryrun'):
            assert unchanged(run(command, directory=example, playbook='incus_host', options=with_image))
        mark(f'the example {EXAMPLE} makes its profile and its running container, from a local image, and its rerun and dryrun are unchanged')

        # The first run of both modules: a profile with a dot in its name, and
        # an instance with two profiles, three configuration keys, and a
        # proxy device, through which its program answers.
        project([web_profile, {'id': 'instance', 'incus_instance': shop}])
        preview = results(run('dryrun'))
        assert is_result(preview['profile'], 'succeeded', 'would_change',
                         'would create the profile mmx-web.limits; would set the description; would set the configuration key limits.cpu; would set the device data', created=True)
        assert is_result(preview['instance'], 'succeeded', 'would_change', f'would create the container mmx-shop from the image {IMAGE}; would start it', created=True)
        assert profile('mmx-web.limits') is None and instance('mmx-shop') is None
        applied = results(run('run'))
        assert is_result(applied['profile'], 'succeeded', 'changed', 'created the profile mmx-web.limits; set the description; set the configuration key limits.cpu; set the device data', created=True)
        assert is_result(applied['instance'], 'succeeded', 'changed', f'created the container mmx-shop from the image {IMAGE}; started it', created=True)
        assert applied['instance']['module_kind'] == 'official_extra' and 'ran: incus init -q --project default local:mmx-test local:mmx-shop' in applied['instance']['diff'], applied['instance']
        made = instance('mmx-shop')
        assert made['status'] == 'Running' and made['type'] == 'container' and made['profiles'] == ['default', 'mmx-web.limits'], made
        assert own_config('mmx-shop') == shop['config'] and made['devices'] == shop['devices'], made
        assert made['expanded_config']['limits.cpu'] == '1' and made['expanded_devices']['data']['path'] == '/mnt/shared', made['expanded_devices']
        assert profile('mmx-web.limits')['devices'] == web_profile['incus_profile']['devices']
        first_start = served(18080)
        assert first_start['image_version'] == '7', first_start
        for command in ('run', 'dryrun'):
            assert unchanged(run(command))
        mark('the first run: a profile with its description, a key, and a device, and a container with two profiles, its configuration, and a proxy device '
             'through which its program answers; a dryrun before that changes nothing, and a rerun and a dryrun are unchanged')

        # A state made by hand that the tasks must keep or remove: a
        # description, a key, and a device of the instance's own.
        incus('config', 'set', 'mmx-shop', 'user.by-hand=1')
        incus('config', 'device', 'add', 'mmx-shop', 'by-hand', 'proxy', 'listen=tcp:127.0.0.1:18089', 'connect=tcp:127.0.0.1:8080')
        remote("incus config show mmx-shop | sed 's/^description: .*/description: Set by hand/' | incus config edit mmx-shop")
        assert instance('mmx-shop')['description'] == 'Set by hand'
        kept = incus_own_keys('mmx-shop')
        assert len(kept) == 6, kept
        keep = {'instance_type': 'container', 'profiles': ['default', 'mmx-web.limits'], 'name': 'mmx-shop', 'state': 'running', 'config': {'limits.memory': '256MiB'}, 'config_keys_not_listed': 'keep',
                'devices': {'second': {'type': 'proxy', 'listen': 'tcp:127.0.0.1:18081', 'connect': 'tcp:127.0.0.1:8080'}}, 'devices_not_listed': 'keep'}
        project([{'id': 'instance', 'incus_instance': keep}])
        assert is_result(results(run('dryrun'))['instance'], 'succeeded', 'would_change', 'would set the configuration key limits.memory; would set the device second', created=False)
        assert 'limits.memory' not in own_config('mmx-shop')
        assert is_result(results(run('run'))['instance'], 'succeeded', 'changed', 'set the configuration key limits.memory; set the device second', created=False)
        changed = instance('mmx-shop')
        assert own_config('mmx-shop') == {**shop['config'], 'user.by-hand': '1', 'limits.memory': '256MiB'}, changed['config']
        assert set(changed['devices']) == {'web', 'by-hand', 'second'} and changed['profiles'] == ['default', 'mmx-web.limits'] and changed['description'] == 'Set by hand', changed
        assert incus_own_keys('mmx-shop') == kept
        # Incus applied the change to the running instance: the new device
        # answers, and the program is the one that started first.
        assert served(18081)['process_started_at'] == first_start['process_started_at']
        for command in ('run', 'dryrun'):
            assert unchanged(run(command))
        # remove: the task's lists are everything of the instance's own.
        # Incus's own keys, the description, and the profile's device stay.
        whole = dict(shop, profiles=['mmx-web.limits', 'default'])
        project([{'id': 'instance', 'incus_instance': whole}])
        removal = ('set the profiles to mmx-web.limits, default; removed the configuration keys limits.memory, user.by-hand; removed the devices by-hand, second')
        assert is_result(results(run('dryrun'))['instance'], 'succeeded', 'would_change', 'would ' + removal.replace('; removed', '; would remove'), created=False)
        assert is_result(results(run('run'))['instance'], 'succeeded', 'changed', removal, created=False)
        changed = instance('mmx-shop')
        assert own_config('mmx-shop') == shop['config'] and changed['devices'] == shop['devices'] and changed['profiles'] == ['mmx-web.limits', 'default'], changed
        assert changed['description'] == 'Set by hand' and incus_own_keys('mmx-shop') == kept and changed['expanded_devices']['data']['path'] == '/mnt/shared', changed
        assert served(18080)['process_started_at'] == first_start['process_started_at']
        assert unchanged(run('run'))
        mark('an existing instance: keep sets one key and one device beside those made by hand, remove leaves exactly the task\'s keys, devices, and profiles in '
             'their order; Incus\'s own image.* and volatile.* keys and the description stay, and the instance is never restarted')

        # A key that needs a restart: a running container stores it and
        # applies it at its next start. The module does not restart.
        waits = dict(whole, config={**SHUTS_DOWN, 'environment.IMAGE_VERSION': '8'})
        project([{'id': 'instance', 'incus_instance': waits}])
        assert is_result(results(run('run'))['instance'], 'succeeded', 'changed', 'set the configuration key environment.IMAGE_VERSION')
        assert own_config('mmx-shop')['environment.IMAGE_VERSION'] == '8' and served(18080) == first_start
        # stopped shuts the instance down; running starts it, with the key.
        project([{'id': 'instance', 'incus_instance': dict(waits, state='stopped')}])
        assert is_result(results(run('dryrun'))['instance'], 'succeeded', 'would_change', 'would shut down the instance mmx-shop')
        assert instance('mmx-shop')['status'] == 'Running'
        assert is_result(results(run('run'))['instance'], 'succeeded', 'changed', 'shut down the instance mmx-shop', created=False)
        assert instance('mmx-shop')['status'] == 'Stopped'
        for command in ('run', 'dryrun'):
            assert unchanged(run(command))
        # A stopped instance gets a new key and is started, in this order.
        again = dict(waits, config={**SHUTS_DOWN, 'environment.IMAGE_VERSION': '9'})
        project([{'id': 'instance', 'incus_instance': again}])
        assert is_result(results(run('dryrun'))['instance'], 'succeeded', 'would_change', 'would set the configuration key environment.IMAGE_VERSION; would start the instance mmx-shop')
        assert is_result(results(run('run'))['instance'], 'succeeded', 'changed', 'set the configuration key environment.IMAGE_VERSION; started the instance mmx-shop')
        second_start = served(18080)
        assert second_start['image_version'] == '9' and second_start['process_started_at'] != first_start['process_started_at'], second_start
        # A frozen instance: stopped refuses it and says to resume it, and
        # running resumes it.
        incus('pause', 'mmx-shop')
        assert instance('mmx-shop')['status'] == 'Frozen'
        project([{'id': 'instance', 'incus_instance': dict(again, state='stopped')}])
        for command in ('dryrun', 'run'):
            frozen = results(run(command, 1))['instance']
            assert is_result(frozen, 'failed', 'unchanged', 'incus_instance.state: the instance mmx-shop is frozen, and Incus does not shut down a frozen instance; '
                             'resume it first, with incus resume mmx-shop') and frozen['failure_kind'] == 'config' and tries(frozen) == 1, frozen
        assert instance('mmx-shop')['status'] == 'Frozen'
        project([{'id': 'instance', 'incus_instance': again}])
        assert is_result(results(run('run'))['instance'], 'succeeded', 'changed', 'resumed the frozen instance mmx-shop')
        assert instance('mmx-shop')['status'] == 'Running' and served(18080) == second_start
        # An ephemeral instance, which Incus deletes when it stops: stopped
        # refuses it, and it still runs afterwards.
        incus('init', '-q', IMAGE, 'mmx-ephemeral', '--ephemeral', input=json.dumps({'config': SHUTS_DOWN}))
        incus('start', 'mmx-ephemeral')
        project([{'id': 'instance', 'incus_instance': {'instance_type': 'container', 'profiles': ['default'], 'name': 'mmx-ephemeral', 'state': 'stopped'}}])
        ephemeral = results(run('run', 1))['instance']
        assert is_result(ephemeral, 'failed', 'unchanged', 'incus_instance.state: the instance mmx-ephemeral is ephemeral, and Incus deletes an ephemeral instance when it stops; '
                         'to delete it, write state: deleted_with_all_its_data') and ephemeral['failure_kind'] == 'config', ephemeral
        assert instance('mmx-ephemeral')['status'] == 'Running'
        project([{'id': 'instance', 'incus_instance': {'name': 'mmx-ephemeral', 'state': 'deleted_with_all_its_data'}}])
        assert is_result(results(run('run'))['instance'], 'succeeded', 'changed', 'deleted the instance mmx-ephemeral with all its data', created=False)
        assert instance('mmx-ephemeral') is None
        # deleted_with_all_its_data also deletes a frozen instance.
        incus('init', '-q', IMAGE, 'mmx-frozen', input=json.dumps({'config': SHUTS_DOWN}))
        incus('start', 'mmx-frozen')
        incus('pause', 'mmx-frozen')
        assert instance('mmx-frozen')['status'] == 'Frozen'
        project([{'id': 'instance', 'incus_instance': {'name': 'mmx-frozen', 'state': 'deleted_with_all_its_data'}}])
        assert is_result(results(run('run'))['instance'], 'succeeded', 'changed', 'deleted the instance mmx-frozen with all its data', created=False)
        assert instance('mmx-frozen') is None
        mark('states: a key that needs a restart is stored and applied at the next start, stopped shuts down, running starts a stopped instance after it wrote '
             'the configuration and resumes a frozen one; stopped refuses a frozen and an ephemeral instance, which stay as they are; '
             'deleted_with_all_its_data deletes a running ephemeral instance and a frozen one')

        # The bounded wait. This instance's program ignores Incus's request
        # to shut down, as a hung guest does: the task waits the 3 seconds
        # that it gives, fails, names the input, and the instance still
        # runs. These seconds are what the check tests. With the input, the
        # task stops the instance at once after the wait.
        slow = {'instance_type': 'container', 'profiles': ['default'], 'name': 'mmx-slow', 'state': 'running', 'image_when_created': IMAGE}
        project([{'id': 'instance', 'incus_instance': slow}])
        assert is_result(results(run('run'))['instance'], 'succeeded', 'changed', created=True)
        wait = {'instance_type': 'container', 'profiles': ['default'], 'name': 'mmx-slow', 'state': 'stopped', 'wait_for_shutdown_at_most': "3s"}
        project([{'id': 'instance', 'incus_instance': wait}])
        started = time.monotonic()
        waited = results(run('run', 1))['instance']
        seconds = time.monotonic() - started
        # One try with MMX's default of three: another try would wait as
        # long again.
        assert waited['status'] == 'failed' and 'failure_kind' not in waited and tries(waited) == 1, waited
        assert 'incus_instance: shutting down the instance mmx-slow, with a wait of 3 seconds: ' in waited['reason'] and waited['reason'].endswith(
            '; the status of the instance is now Running. To stop the instance at once when the wait ends, write power_off_when_shutdown_takes_longer: true'), waited
        assert seconds >= 3 and instance('mmx-slow')['status'] == 'Running', seconds
        project([{'id': 'instance', 'incus_instance': dict(wait, power_off_when_shutdown_takes_longer=True)}])
        assert is_result(results(run('run'))['instance'], 'succeeded', 'changed', 'stopped the instance mmx-slow at once, because it did not shut down within 3 seconds')
        assert instance('mmx-slow')['status'] == 'Stopped' and unchanged(run('run'))
        print(f'An instance that ignores the request to shut down failed the task after {seconds:.1f} s with a wait of 3 seconds', flush=True)
        mark('the bounded wait: an instance that does not shut down fails the task after wait_for_shutdown_at_most, with one try, and still runs; '
             'with power_off_when_shutdown_takes_longer the task stops it at once')

        # What a run cannot do. A new instance without an image, and the
        # local image of a container for a virtual machine: both fail before
        # anything is created, in a dryrun too, and are not tried again.
        project([{'id': 'instance', 'incus_instance': {'instance_type': 'container', 'profiles': ['default'], 'name': 'mmx-nothing', 'state': 'running'}}])
        for command in ('dryrun', 'run'):
            missing_image = results(run(command, 1))['instance']
            assert is_result(missing_image, 'failed', 'unchanged', 'incus_instance.image_when_created: is required to create the instance mmx-nothing') and missing_image['failure_kind'] == 'config', missing_image
        project([{'id': 'instance', 'incus_instance': {'profiles': ['default'], 'name': 'mmx-nothing', 'state': 'running', 'image_when_created': IMAGE, 'instance_type': 'virtual_machine'}}])
        for command in ('dryrun', 'run'):
            other_type = results(run(command, 1))['instance']
            assert is_result(other_type, 'failed', 'unchanged', f'incus_instance.instance_type: the task asks for a virtual machine, and the image {IMAGE} is the image of a container; '
                             'Incus creates an instance of the type of its image') and tries(other_type) == 1, other_type
        # An image that the node's Incus does not have: an earlier node task
        # may import it, so a dryrun is not checked and a run fails once.
        not_imported = ('incus_instance: the Incus server on this node has no image mmx-not-imported; import it in an earlier node task, '
                        'or name an image of another server, such as images:debian/13')
        project([{'id': 'instance', 'incus_instance': {'instance_type': 'container', 'profiles': ['default'], 'name': 'mmx-nothing', 'state': 'running', 'image_when_created': 'mmx-not-imported'}}])
        assert is_result(results(run('dryrun'))['instance'], 'not_checked', 'unknown', not_imported)
        no_image = results(run('run', 1))['instance']
        assert is_result(no_image, 'failed', 'unchanged', not_imported) and no_image['failure_kind'] == 'missing_prerequisite' and tries(no_image) == 1, no_image
        # A key that Incus does not know: incus init creates no instance, and
        # MMX tries the failed command three times.
        project([{'id': 'instance', 'incus_instance': {'instance_type': 'container', 'profiles': ['default'], 'name': 'mmx-nothing', 'state': 'running', 'image_when_created': IMAGE, 'config': {'limits.nonsense': 'lots'}, 'config_keys_not_listed': 'keep'}}])
        unknown_key = results(run('run', 1))['instance']
        assert unknown_key['status'] == 'failed' and f'incus_instance: creating the instance mmx-nothing from the image {IMAGE}: Error: ' in unknown_key['reason'] \
            and 'limits.nonsense' in unknown_key['reason'] and tries(unknown_key) == 3, unknown_key
        assert instance('mmx-nothing') is None
        # The same key for a running instance: Incus refuses the document,
        # nothing is stored, the task has one try, and the reason says which
        # status the instance has afterwards.
        project([{'id': 'instance', 'incus_instance': dict(again, config={**again['config'], 'limits.nonsense': 'lots'})}])
        unknown_key = results(run('run', 1))['instance']
        assert 'incus_instance: writing the configuration of the instance mmx-shop: Error: ' in unknown_key['reason'] \
            and unknown_key['reason'].endswith('limits.nonsense; the status of the instance is now Running'), unknown_key
        assert unknown_key['failure_kind'] == 'config' and tries(unknown_key) == 1, unknown_key
        assert own_config('mmx-shop') == again['config'] and served(18080) == second_start
        # A run that stopped between two changes: the instance is created,
        # and it does not start, because its proxy device wants a port that
        # the first instance holds. The task has one try, and its result
        # says that the instance was created: another try would find the
        # instance and say that the task created nothing.
        stuck = {'instance_type': 'container', 'profiles': ['default'], 'name': 'mmx-second', 'state': 'running', 'image_when_created': IMAGE, 'config': SHUTS_DOWN, 'config_keys_not_listed': 'remove',
                 'devices': {'web': {'type': 'proxy', 'listen': 'tcp:127.0.0.1:18080', 'connect': 'tcp:127.0.0.1:8080'}}, 'devices_not_listed': 'remove'}
        project([{'id': 'instance', 'incus_instance': stuck}])
        half_done = results(run('run', 1))['instance']
        assert is_result(half_done, 'failed', 'changed', created=True) and tries(half_done) == 1 and 'failure_kind' not in half_done, half_done
        assert 'incus_instance: created the instance mmx-second, and starting it failed: ' in half_done['reason'], half_done
        # Incus leaves such an instance stopped or, without the device,
        # running; the reason names the status. With another port, the next
        # run writes the device, and starts an instance that is stopped.
        status = instance('mmx-second')['status']
        assert status in ('Stopped', 'Running') and half_done['reason'].endswith('; the status of the instance is now ' + status), half_done
        print(f'A new instance whose proxy device wants a port in use: after the failed start its status is {status}', flush=True)
        start = ('; started the instance mmx-second', '; would start the instance mmx-second') if status == 'Stopped' else ('', '')
        stuck['devices']['web']['listen'] = 'tcp:127.0.0.1:18082'
        project([{'id': 'instance', 'incus_instance': stuck}])
        assert is_result(results(run('dryrun'))['instance'], 'succeeded', 'would_change', 'would set the device web' + start[1], created=False)
        assert is_result(results(run('run'))['instance'], 'succeeded', 'changed', 'set the device web' + start[0], created=False)
        assert instance('mmx-second')['status'] == 'Running' and served(18082)['image_version'] == ''
        # A new profile that Incus refuses is removed again, with one try.
        project([{'id': 'profile', 'incus_profile': {'name': 'mmx-refused', 'config': {'limits.nonsense': 'lots'}, 'config_keys_not_listed': 'remove'}}])
        refused = results(run('run', 1))['profile']
        assert refused['status'] == 'failed' and 'incus_profile: writing the new profile mmx-refused: Error: ' in refused['reason'] \
            and refused['reason'].endswith('; the task removed the new profile again'), refused
        assert refused['failure_kind'] == 'config' and tries(refused) == 1 and refused['change'] == 'changed', refused
        assert profile('mmx-refused') is None
        # A profile edit that Incus saves and then reports as failed: the
        # profile gets a device whose port the instance that uses the
        # profile already holds. Another try would find the saved profile
        # and report success, so the task has one try, and its reason holds
        # Incus's message. The next run finds nothing to write.
        clash = {'type': 'proxy', 'listen': 'tcp:127.0.0.1:18080', 'connect': 'tcp:127.0.0.1:8080'}
        with_clash = dict(web_profile['incus_profile'], devices={**web_profile['incus_profile']['devices'], 'clash': clash})
        project([{'id': 'profile', 'incus_profile': with_clash}])
        saved = results(run('run', 1))['profile']
        assert saved['status'] == 'failed' and tries(saved) == 1 and 'failure_kind' not in saved, saved
        assert 'incus_profile: writing the profile mmx-web.limits: Error: ' in saved['reason'] and 'Incus saved the profile all the same' in saved['reason'], saved
        assert 'clash' in profile('mmx-web.limits')['devices'] and unchanged(run('run'))
        project([web_profile])
        assert is_result(results(run('run'))['profile'], 'succeeded', 'changed', 'removed the device clash', created=False)
        assert served(18080) == second_start
        # A profile that an instance uses is not removed: Incus refuses each
        # of MMX's three tries.
        project([{'id': 'profile', 'incus_profile': {'name': 'mmx-web.limits', 'state': 'absent'}}])
        in_use = results(run('run', 1))['profile']
        assert in_use['status'] == 'failed' and 'incus_profile: removing the profile mmx-web.limits: Error: ' in in_use['reason'] and tries(in_use) == 3, in_use
        assert profile('mmx-web.limits') is not None
        mark('failures: a new instance without an image and an image of the other type fail before anything is created; a local image that Incus does not have '
             'is a missing prerequisite; a key that Incus does not know creates no instance and changes no running one; an instance that was created and did '
             'not start fails its task once and is finished by the next run; a refused new profile is removed again; a profile that Incus saves and then '
             'fails on fails its task once with Incus\'s message; a profile in use is not removed')

        # Incus projects. A missing project is a missing prerequisite. A
        # project with its own profiles holds a profile and an instance of
        # the names that the project default has too; a project that uses the
        # profiles of default refuses a profile task.
        in_shop = {'incus_project': 'mmx-shop-project'}
        root_disk = {'root': {'type': 'disk', 'path': '/', 'pool': 'default'}}
        shop_profile = {'id': 'profile', 'incus_profile': {'name': 'mmx-web.limits', **in_shop, 'devices': root_disk, 'devices_not_listed': 'remove'}}
        shop_instance = {'id': 'instance', 'incus_instance': {'instance_type': 'container', 'name': 'mmx-shop', 'state': 'stopped', 'image_when_created': IMAGE, **in_shop, 'profiles': ['mmx-web.limits'],
                                                              'config': {'user.note': 'of the project'}, 'config_keys_not_listed': 'remove'}}
        project([shop_profile, shop_instance])
        no_project = 'the Incus project mmx-shop-project does not exist; create it in an earlier node task, such as a shell task that runs incus project create'
        preview = results(run('dryrun'))
        assert is_result(preview['profile'], 'not_checked', 'unknown', 'incus_profile: ' + no_project) and is_result(preview['instance'], 'not_checked', 'unknown', vm.earlier_change_may_provide('profile', 'incus_instance: ' + no_project))
        assert results(run('run', 1))['profile']['failure_kind'] == 'missing_prerequisite'
        # An earlier node task creates the project, as the modules' pages
        # say: a guarded shell task. The project uses the images of the
        # project default, so the suite's image is in it too. With that task
        # before them, the two tasks are not checked in a dryrun.
        create_project = {'id': 'project', 'shell': {'script': 'incus project create mmx-shop-project -c features.images=false',
                                                     'skip_if_this_command_succeeds': 'incus project show mmx-shop-project'}}
        project([create_project, shop_profile, shop_instance])
        preview = results(run('dryrun'))
        assert preview['profile']['status'] == 'not_checked' and preview['instance']['status'] == 'not_checked', preview
        incus('project', 'create', 'mmx-flat-project', '-c', 'features.images=false', '-c', 'features.profiles=false')
        untouched = (as_configured('mmx-shop'), profile('mmx-web.limits'))
        applied = results(run('run'))
        assert applied['project']['change'] == 'changed', applied['project']
        assert is_result(applied['profile'], 'succeeded', 'changed', 'created the profile mmx-web.limits; set the device root', created=True)
        assert is_result(applied['instance'], 'succeeded', 'changed', f'created the container mmx-shop from the image {IMAGE}', created=True)
        in_project = instance('mmx-shop', 'mmx-shop-project')
        assert in_project['status'] == 'Stopped' and in_project['profiles'] == ['mmx-web.limits'] and own_config('mmx-shop', 'mmx-shop-project') == {'user.note': 'of the project'}, in_project
        assert profile('mmx-web.limits', 'mmx-shop-project')['devices'] == root_disk
        assert (as_configured('mmx-shop'), profile('mmx-web.limits')) == untouched
        for command in ('run', 'dryrun'):
            assert unchanged(run(command))
        project([{'id': 'profile', 'incus_profile': {'name': 'mmx-web.limits', 'incus_project': 'mmx-flat-project', 'description': 'Not of this project'}}])
        for command in ('dryrun', 'run'):
            flat = results(run(command, 1))['profile']
            assert is_result(flat, 'failed', 'unchanged', 'incus_profile.incus_project: the Incus project mmx-flat-project uses the profiles of the project default '
                             '(its features.profiles is not true), so it has no profile of its own; write this task without incus_project') and flat['failure_kind'] == 'config', flat
        assert profile('mmx-web.limits') == untouched[1]
        project([{'id': 'instance', 'incus_instance': {'name': 'mmx-shop', 'state': 'deleted_with_all_its_data', **in_shop}},
                 {'id': 'profile', 'incus_profile': {'name': 'mmx-web.limits', 'state': 'absent', **in_shop}}])
        gone = results(run('run'))
        assert is_result(gone['instance'], 'succeeded', 'changed', 'deleted the instance mmx-shop with all its data') and is_result(gone['profile'], 'succeeded', 'changed', 'removed the profile mmx-web.limits', created=False)
        assert instance('mmx-shop', 'mmx-shop-project') is None and profile('mmx-web.limits', 'mmx-shop-project') is None
        assert (as_configured('mmx-shop'), profile('mmx-web.limits')) == untouched
        mark('Incus projects: a missing project is a missing prerequisite; a project with its own profiles gets a profile and an instance of the names that '
             'the project default has, which stay untouched; a project that uses the profiles of default refuses a profile task')

        # The client settings of the account root name another default
        # project and another default server. The tasks still manage this
        # node's Incus and its project default.
        incus('project', 'switch', 'mmx-shop-project')
        incus('remote', 'switch', 'images')
        assert incus('list', '--format', 'csv', check=False).returncode != 0
        project([web_profile, {'id': 'instance', 'incus_instance': again}])
        assert unchanged(run('run'))
        project([web_profile, {'id': 'instance', 'incus_instance': dict(again, state='stopped')}])
        assert is_result(results(run('run'))['instance'], 'succeeded', 'changed', 'shut down the instance mmx-shop')
        project([web_profile, {'id': 'instance', 'incus_instance': again}])
        assert is_result(results(run('run'))['instance'], 'succeeded', 'changed', 'started the instance mmx-shop')
        incus('remote', 'switch', 'local')
        incus('project', 'switch', 'default')
        assert instance('mmx-shop')['status'] == 'Running' and instance('mmx-shop', 'mmx-shop-project') is None
        mark('with another default project and another default server in root\'s client settings, the tasks still manage this node\'s Incus and its project default')

        # A virtual machine instance, where the distribution's package incus
        # brings QEMU. Its image is an empty disk: the instance starts and
        # runs no system, so it never shuts down by itself.
        if with_virtual_machines:
            # apt installs no recommended packages here, so the task names
            # what QEMU only recommends and Incus needs: ovmf, the firmware
            # that a virtual machine starts with; qemu-utils, whose qemu-img
            # Incus uses for a virtual machine's image; and
            # qemu-system-modules-spice, for the console that Incus gives
            # every virtual machine.
            project([{'apt_package': {'packages': [{'name': 'incus', 'version': 'newest_when_first_installed'}, {'name': 'ovmf', 'version': 'newest_when_first_installed'}, {'name': 'qemu-utils', 'version': 'newest_when_first_installed'}, {'name': 'qemu-system-modules-spice', 'version': 'newest_when_first_installed'}]}}])
            run('run')
            drivers = remote("incus info | sed -n 's/^ *driver: //p'")
            if 'qemu' not in drivers:
                remote('systemctl restart incus.service')
                drivers = remote("incus info | sed -n 's/^ *driver: //p'")
            assert 'qemu' in drivers, drivers
            print(f'The package incus added QEMU; the server\'s drivers: {drivers}', flush=True)
            metadata_file = vm.ROOT / 'incus-instances-test-vm-metadata.tar.gz'
            metadata_file.write_bytes(tarball([('metadata.yaml', image_metadata(machine, 'MMX test image of a virtual machine'), 0o644)]))
            vm.copy(metadata_file, f'{BASE}/incoming/vm-metadata.tar.gz')
            remote(f'qemu-img create -q -f qcow2 {BASE}/incoming/rootfs.img 16M')
            incus('image', 'import', '-q', f'{BASE}/incoming/vm-metadata.tar.gz', f'{BASE}/incoming/rootfs.img', '--alias', VIRTUAL_MACHINE_IMAGE)
            machine_task = {'profiles': ['default'], 'name': 'mmx-machine', 'state': 'running', 'image_when_created': VIRTUAL_MACHINE_IMAGE, 'instance_type': 'virtual_machine',
                            'config': {'limits.memory': '256MiB'}, 'config_keys_not_listed': 'remove'}
            # With container requested, the virtual machine image fails before
            # Incus creates anything.
            project([{'id': 'instance', 'incus_instance': dict(machine_task, instance_type='container')}])
            assert is_result(results(run('run', 1))['instance'], 'failed', 'unchanged', f'incus_instance.instance_type: the task asks for a container, and the image '
                             f'{VIRTUAL_MACHINE_IMAGE} is the image of a virtual machine; Incus creates an instance of the type of its image')
            assert instance('mmx-machine') is None
            project([{'id': 'instance', 'incus_instance': machine_task}])
            assert is_result(results(run('dryrun'))['instance'], 'succeeded', 'would_change', f'would create the virtual machine mmx-machine from the image {VIRTUAL_MACHINE_IMAGE}; would start it', created=True)
            assert is_result(results(run('run'))['instance'], 'succeeded', 'changed', f'created the virtual machine mmx-machine from the image {VIRTUAL_MACHINE_IMAGE}; started it', created=True)
            made = instance('mmx-machine')
            assert made['type'] == 'virtual-machine' and made['status'] == 'Running' and own_config('mmx-machine') == {'limits.memory': '256MiB'}, made
            assert unchanged(run('run'))
            project([{'id': 'instance', 'incus_instance': dict(machine_task, config={'limits.memory': '256MiB', 'user.note': 'a virtual machine'})}])
            assert is_result(results(run('run'))['instance'], 'succeeded', 'changed', 'set the configuration key user.note', created=False)
            assert own_config('mmx-machine')['user.note'] == 'a virtual machine' and instance('mmx-machine')['status'] == 'Running'
            project([{'id': 'instance', 'incus_instance': {'instance_type': 'container', 'profiles': ['default'], 'name': 'mmx-machine', 'state': 'running'}}])
            other_type = results(run('run', 1))['instance']
            assert is_result(other_type, 'failed', 'unchanged', 'incus_instance.instance_type: the task asks for a container, and the instance mmx-machine is of the type virtual machine; '
                             'Incus cannot change the type of an instance') and other_type['failure_kind'] == 'config', other_type
            project([{'id': 'instance', 'incus_instance': {'name': 'mmx-machine', 'state': 'deleted_with_all_its_data'}}])
            assert is_result(results(run('run'))['instance'], 'succeeded', 'changed', 'deleted the instance mmx-machine with all its data')
            assert instance('mmx-machine') is None
            mark('with the package incus, a virtual machine instance: created with its type and started, a key written while it runs, a task for a container refused before and after, and deleted')
        else:
            print(f'No virtual machine instance on {" ".join(identity)}: the suite makes one only where the package incus brings QEMU', flush=True)

        # A configuration value that is a secret of the project. It reaches
        # incus through standard input alone; the task's result is hidden,
        # because config is no private value; and the value is in nothing
        # that mmx printed or saved and in no record of MMX on the node.
        secret = {'instance_type': 'container', 'profiles': ['default'], 'name': 'mmx-with-secret', 'state': 'stopped', 'image_when_created': IMAGE, 'config': {'environment.SHOP_TOKEN': '{{ .secrets.shop_token }}'}, 'config_keys_not_listed': 'remove'}
        project([{'id': 'instance', 'incus_instance': secret}], **SECRETS)
        hidden = results(run('run'))['instance']
        assert hidden['status'] == 'succeeded' and hidden['change'] == 'changed', hidden
        assert incus('config', 'get', 'mmx-with-secret', 'environment.SHOP_TOKEN').stdout.strip() == SECRET_VALUE
        assert unchanged(run('run'))
        project([{'id': 'instance', 'incus_instance': dict(secret, config={'environment.SHOP_TOKEN': '{{ .secrets.shop_token }}', 'user.note': 'second write'})}], **SECRETS)
        assert results(run('run'))['instance']['change'] == 'changed'
        assert incus('config', 'get', 'mmx-with-secret', 'environment.SHOP_TOKEN').stdout.strip() == SECRET_VALUE
        saved = ''.join(path.read_text(errors='replace') for directory in vm.ROOT.glob('incus-instances*') if directory.is_dir()
                        for path in (directory / '.mmx').rglob('*') if path.is_file() and 'runners' not in path.parts)
        for where, text in (('what mmx printed', ''.join(printed)), ('the saved runs', saved)):
            assert SECRET_VALUE not in text, f'the secret configuration value is in {where}: ...{text[max(0, text.find(SECRET_VALUE) - 300):text.find(SECRET_VALUE) + 60]}'
        # Incus itself keeps the value as text in its own database and in
        # the instance's files under /var/lib/incus; those are not MMX's.
        # The value reaches grep in a file: sudo writes each command line
        # into the journal, which this check searches.
        remote(f'cat > {BASE}/searched-value', input=SECRET_VALUE + '\n')
        found = remote(f'grep -rlF -f {BASE}/searched-value /var/lib/mmx /run/mmx /root /home /tmp /var/log/incus 2>/dev/null || true\n'
                       f'journalctl --no-pager -q 2>/dev/null | grep -F -f {BASE}/searched-value || true')
        assert found == '', 'the secret configuration value is on the node outside Incus: ' + found[:600]
        mark('a secret in a configuration value reaches incus through standard input: it is stored, a rerun is unchanged, and it is in nothing that mmx printed or '
             'saved, in no record or journal of the node, and in no file under /root')

        # Removal: deleted_with_all_its_data stops and deletes an instance
        # with its snapshots, absent deletes a profile that nothing uses any
        # more, and what is missing is unchanged.
        incus('snapshot', 'create', 'mmx-shop', 'before-removal')
        gone = [{'id': 'running', 'incus_instance': {'name': 'mmx-shop', 'state': 'deleted_with_all_its_data'}},
                {'id': 'second', 'incus_instance': {'name': 'mmx-second', 'state': 'deleted_with_all_its_data'}},
                {'id': 'stopped', 'incus_instance': {'name': 'mmx-slow', 'state': 'deleted_with_all_its_data'}},
                {'id': 'with_secret', 'incus_instance': {'name': 'mmx-with-secret', 'state': 'deleted_with_all_its_data'}},
                {'id': 'never_there', 'incus_instance': {'name': 'mmx-never-there', 'state': 'deleted_with_all_its_data'}},
                {'id': 'profile', 'incus_profile': {'name': 'mmx-web.limits', 'state': 'absent'}},
                {'id': 'no_profile', 'incus_profile': {'name': 'mmx-never-there', 'state': 'absent'}}]
        project(gone)
        preview = results(run('dryrun'))
        assert is_result(preview['running'], 'succeeded', 'would_change', 'would delete the instance mmx-shop with all its data', created=False)
        assert is_result(preview['stopped'], 'succeeded', 'would_change', 'would delete the instance mmx-slow with all its data', created=False)
        assert is_result(preview['profile'], 'succeeded', 'would_change', 'would remove the profile mmx-web.limits', created=False)
        assert is_result(preview['never_there'], 'succeeded', 'unchanged', '', created=False) and is_result(preview['no_profile'], 'succeeded', 'unchanged', '', created=False)
        assert instance('mmx-shop')['status'] == 'Running' and profile('mmx-web.limits') is not None
        removed = results(run('run'))
        assert is_result(removed['running'], 'succeeded', 'changed', 'deleted the instance mmx-shop with all its data', created=False)
        assert is_result(removed['stopped'], 'succeeded', 'changed', 'deleted the instance mmx-slow with all its data', created=False)
        assert is_result(removed['profile'], 'succeeded', 'changed', 'removed the profile mmx-web.limits', created=False)
        assert all(instance(name) is None for name in ('mmx-shop', 'mmx-second', 'mmx-slow', 'mmx-with-secret')) and profile('mmx-web.limits') is None
        assert [entry['name'] for entry in json.loads(incus('list', '--format', 'json').stdout)] == ['shop-test']
        for command in ('run', 'dryrun'):
            assert unchanged(run(command))
        mark('removal: a running instance with a snapshot and a stopped one are deleted with all their data, a profile that nothing uses any more is removed, '
             'a dryrun before changes nothing and says what a run would delete, and what is missing is unchanged')
    finally:
        failing = sys.exc_info()[0] is not None
        vm.ENV.pop('MMX_TEST_SHOP_TOKEN', None)
        started = time.monotonic()
        cleanup()
        print(f'The cleanup took {time.monotonic() - started:.0f} s', flush=True)
        if not failing:
            guest_is_as_found()
    (workspace / 'summary.json').write_text(json.dumps(passed, indent=2) + '\n')
    print(f'{len(passed)} Incus workflows passed: {" ".join(version.split())} on {distribution}', flush=True)


if __name__ == '__main__':
    vm.run_suite(main)
