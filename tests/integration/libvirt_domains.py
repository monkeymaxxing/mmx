#!/usr/bin/env python3
"""libvirt_domain through the public CLI, against libvirt and QEMU from the
distribution's own packages, which the example project libvirt-host installs.
The node's state is checked with virsh, ps, and the files of libvirt,
independently of what MMX reports.

The domains are virtual machines inside the guest, so the suite needs the
guest's own kernel with /dev/kvm: it runs in a virtual machine on every
distribution, and it runs the whole test on all six. No domain needs a
download: the smallest one has no disk and no operating system, one boots
the guest's own kernel without a root filesystem and ends by itself, and the
example's has an empty disk.

A guest without /dev/kvm, such as a Hetzner Cloud server, cannot run a
domain. There the suite runs only the test without virsh, and says so. In a virtual machine of incus.py a missing /dev/kvm fails the
suite: that guest is there to have it.
"""
import hashlib
import json
import re
import shlex
import shutil
import sys
import time

import vm

EXAMPLE = 'libvirt-host'
EXAMPLE_DOMAIN = 'shop-db'
EXAMPLE_XML = '/etc/mmx-libvirt/shop-db.xml'
EXAMPLE_IMAGE = '/var/lib/libvirt/images/shop-db.qcow2'
LIBVIRT = 'virsh -c qemu:///system'
# The suite's own XML files, which it writes itself as root: a file task
# would show a file's text in mmx's output, and one file holds a password.
XML_DIRECTORY = '/etc/mmx-libvirt-test'
KERNEL_COPY = '/var/lib/libvirt/images/mmx-test-vmlinuz'
DISK_IMAGE = '/var/lib/libvirt/images/mmx-test-disk.qcow2'
SERIAL_LOG = '/var/log/libvirt/qemu/mmx-test-ends-serial.log'
# VNC takes at most eight characters.
PASSWORD = 'Mx4711pw'
RECORD = 'urn:mmx:libvirt_domain'
# The reason without virsh names the program and the package of each family;
# which distributions it lists is the module's own text.
MISSING_VIRSH_START = 'libvirt_domain: the program virsh is not installed; install the package libvirt-clients (Debian and Ubuntu)'
MISSING_VIRSH_NAMES = ('libvirt-client (the Red Hat family', ' in an earlier node task')
# What the packages of libvirt and QEMU leave behind when they are removed.
LEFT_BY_THE_PACKAGES = ['/etc/libvirt', '/var/lib/libvirt', '/var/log/libvirt', '/var/cache/libvirt', '/run/libvirt',
                        '/var/log/swtpm', '/var/lib/swtpm-localca', '/etc/qemu', '/etc/qemu-kvm', '/var/lib/dnsmasq', '/var/lib/qemu']
OF_THE_SUITE = [XML_DIRECTORY, '/etc/mmx-libvirt', '/etc/apt/apt.conf.d/90-no-recommended-packages']
# The names of the installed packages; the keys that dnf imports are no
# packages of the distribution.
INSTALLED_PACKAGES = ("if command -v dpkg-query >/dev/null; then dpkg-query -W -f '${db:Status-Abbrev} ${Package}\\n' | awk '$1 == \"ii\" {print $2}'; "
                      "else rpm -qa --qf '%{NAME}\\n' | grep -v '^gpg-pubkey$'; fi | sort -u")


def tasks_of_the_page(heading):
    """The text of the first YAML block below a heading of the module's page: the node tasks that the page tells its reader to write."""
    page = (vm.REPO / 'official_extra_modules/libvirt_domain/README.md').read_text()
    return page.split(heading + '\n', 1)[1].split('```yaml\n', 1)[1].split('```', 1)[0]


def is_a_virtual_machine_of_incus_py(target):
    """Whether incus.py made the suite's guest as a virtual machine. Its file
    of targets says so: the guest has an Incus instance, and its platform
    text ends with the kind of guest, "Incus vm" or "Incus container"."""
    return 'incus_instance' in target and target.get('platform', '').endswith(' Incus vm')


def smallest_domain(name, memory_mib=64, devices=''):
    """A domain without a disk, a network card, or an operating system: it starts, runs, and never shuts down by itself."""
    return (f"<domain type='kvm'>\n  <name>{name}</name>\n  <memory unit='MiB'>{memory_mib}</memory>\n"
            f"  <os>\n    <type arch='x86_64'>hvm</type>\n  </os>\n{devices}</domain>\n")


def domain_that_ends_by_itself(name, seconds):
    """The guest's own kernel without a root filesystem: it stops, restarts after the seconds, and the restart shuts the domain off."""
    return (f"<domain type='kvm'>\n  <name>{name}</name>\n  <memory unit='MiB'>128</memory>\n"
            f"  <os>\n    <type arch='x86_64'>hvm</type>\n    <kernel>{KERNEL_COPY}</kernel>\n    <cmdline>console=ttyS0 panic={seconds}</cmdline>\n  </os>\n"
            f"  <on_reboot>destroy</on_reboot>\n  <devices>\n    <serial type='file'><source path='{SERIAL_LOG}'/></serial>\n  </devices>\n</domain>\n")


def main():
    vm.up()
    node, ssh = vm.connection()
    vm.reset()
    workspace = vm.ROOT / 'libvirt-domains'
    example = vm.ROOT / ('libvirt-domains-' + EXAMPLE)
    for directory in (workspace, example):
        if directory.exists():
            shutil.rmtree(directory)
    vm.build()
    mmx = vm.ROOT / 'bin/mmx'
    passed = []
    marked = time.monotonic()
    # Everything mmx printed, to search for the password at the end.
    printed = []

    def remote(script, input=None, check=True):
        return vm.cmd([*ssh, 'sudo -n /bin/sh -c ' + shlex.quote('set -eu\nPATH=/usr/sbin:/sbin:$PATH\n' + script)], input=input, check=check).stdout.strip()

    def virsh(arguments, check=True):
        """What virsh prints, with the real libvirt and none of MMX."""
        return remote(f'{LIBVIRT} {arguments}' + ('' if check else ' 2>&1 || true'))

    # A domain of the guest's size: the guest itself has 2 GiB.
    example_vars = {'shop_db_memory_gib': 1, 'shop_db_cpu_count': 1, 'shop_db_disk_size_gib': 1}

    def write_inventories():
        vm.write_inventory(workspace, {'nodes': {'host': node}}, name='example')
        vm.write_inventory(example, {'nodes': {'virtualization-host': node}, 'vars': example_vars}, name='example')

    def project(tasks):
        vm.write_project(workspace, project_name='libvirt_domains', node_task_defaults={'retry': {'pause_between_tries': '0s'}},
                         fleet_stages=[{'id': 'configure', 'nodes': 'all', 'node_tasks': tasks}])

    def run(command, expected=None, directory=None, timeout=900):
        result = vm.cmd([mmx, command, *vm.playbook_args(command), '--json'], cwd=directory or workspace, check=False, timeout=timeout)
        printed.append(result.stdout + result.stderr)
        if result.returncode not in (vm.success_codes(command) if expected is None else (expected,)):
            raise AssertionError(f'mmx {command} ended with the exit code {result.returncode}:\n{outcome(result.stdout)}\n{result.stderr}')
        return json.loads(result.stdout)

    def outcome(output):
        """Each node task's result in one line, for a failure's message."""
        try:
            entries = json.loads(output)['entries']
        except (ValueError, KeyError):
            return output
        return '\n'.join(f"{r['task']}: {r['status']}, {r['change']}: {r.get('reason', '')} {r.get('stderr', '')}".strip()
                         for entry in entries for r in entry['response']['results'])

    def results(record):
        """The results of the node's tasks, by the last part of each task's name."""
        return {r['task'].rsplit('/', 1)[-1]: r for r in record['entries'][0]['response']['results']}

    def change(record):
        return record['entries'][0]['response']['change']

    def is_result(result, status, change, reason=None, created=None):
        assert result['status'] == status and result['change'] == change, result
        if reason is not None:
            assert result.get('reason', '') == reason, (result.get('reason'), reason)
        if created is not None:
            assert result['module_outputs'] == {'created': created}, result
        return result

    def names_missing_virsh(result, earlier_task=None):
        """The reason names the program and the packages; in a dryrun after an earlier node task that would change the node, it ends with the hint that names that node task."""
        reason = result.get('reason', '')
        assert reason.startswith(MISSING_VIRSH_START) and all(name in reason for name in MISSING_VIRSH_NAMES), result
        if earlier_task is not None:
            assert reason.endswith(vm.earlier_change_may_provide(earlier_task, '')[1:]), result
        return result

    def tries(result):
        return len(result.get('tries') or [result])

    def cannot_succeed(result, change, reason):
        """The task failed in a way that another try cannot fix, and MMX tried it once."""
        assert (result['status'], result.get('failure_kind'), result['change'], tries(result)) == ('failed', 'config', change, 1), result
        assert result['reason'] == reason, (result['reason'], reason)
        return result

    def missing_state(result, reason):
        """The task met a state that an earlier node task may change: a run fails once as a missing prerequisite."""
        assert (result['status'], result.get('failure_kind'), result['change'], tries(result)) == ('failed', 'missing_prerequisite', 'unchanged', 1), result
        assert result['reason'] == reason, (result['reason'], reason)
        return result

    def mark(message):
        # The seconds since the last mark say where the suite's time goes.
        nonlocal marked
        now = time.monotonic()
        passed.append(message)
        print(f'PASS {message} ({now - marked:.0f} s)', flush=True)
        marked = now

    def write_xml(name, text):
        """Write a domain's XML file as root, without MMX; return its path."""
        path = f'{XML_DIRECTORY}/{name}.xml'
        remote(f'install -d -m 700 {XML_DIRECTORY}\ninstall -m 600 /dev/null {path}\ncat > {path}', input=text)
        return path

    def domain_task(name, state, path=None, **inputs):
        fields = {'name': name, 'state': state, **inputs}
        if path:
            fields['domain_xml_file'] = path
        return {'id': name.replace('-', '_'), 'libvirt_domain': fields}

    def domains():
        return set(virsh('list --all --name').split())

    def information(name):
        """virsh dominfo of a domain as a mapping."""
        return {key.strip(): value.strip() for key, _, value in (line.partition(':') for line in virsh(f'dominfo {name}').splitlines() if not line.startswith(' '))}

    def state(name):
        return virsh(f'domstate --reason {name}')

    def record(name):
        """The SHA-256 that the domain's metadata holds, or the text that virsh printed."""
        answer = virsh(f'metadata {name} --uri {RECORD} --config', check=False)
        found = re.fullmatch(r'<last_defined_file sha256="([0-9a-f]{64})"/>', answer)
        return found.group(1) if found else answer

    def sha256_of(path):
        return remote(f'sha256sum {path}').split()[0]

    def stored_xml(name):
        return virsh(f'dumpxml --inactive {name}')

    def qemu_processes(name):
        """The QEMU processes of a domain, read from the process list and not from libvirt."""
        return remote(f"ps -eo args | grep -c '[g]uest={name},' || true")

    # The programs and the processes of libvirt and QEMU that the guest has;
    # sort -u, because the process list changes its order.
    PROGRAMS_AND_PROCESSES = """
{ for program in virsh qemu-img qemu-system-x86_64 dnsmasq; do command -v $program || true; done
  ps -eo comm | grep -E '^(libvirtd|virt[a-z]+d|qemu-|dnsmasq)' || true; } | sort -u
"""

    def before_the_suite():
        """What the guest had before the suite, which the cleanup must leave."""
        return {'packages': set(remote(INSTALLED_PACKAGES).split()),
                'paths': remote(f"ls -d {' '.join(LEFT_BY_THE_PACKAGES)} 2>/dev/null || true").split(),
                'accounts': set(remote('cut -d: -f1 /etc/passwd').split()), 'groups': set(remote('cut -d: -f1 /etc/group').split()),
                'ip_forward': remote('cat /proc/sys/net/ipv4/ip_forward'),
                'polkit': remote('systemctl cat polkit.service >/dev/null 2>&1 && echo yes || true') == 'yes',
                'units': set(remote("systemctl list-unit-files --no-legend 'libvirt*' 'virt*' 'dnsmasq*' 2>/dev/null | cut -d' ' -f1 || true").split()),
                # A cloud provider's image brings some of these itself: a
                # Hetzner Cloud server runs QEMU's guest agent, qemu-ga, and
                # its Debian image has qemu-img.
                'programs_and_processes': set(remote(PROGRAMS_AND_PROCESSES).splitlines())}

    def cleanup(found):
        """Remove the domains, libvirt, QEMU, and everything of the suite, so that the next suite finds the guest as this one found it."""
        added = ' '.join(sorted(set(remote(INSTALLED_PACKAGES).split()) - found['packages']))
        paths = ' '.join(path for path in LEFT_BY_THE_PACKAGES if path not in found['paths'])
        accounts = ' '.join(sorted(set(remote('cut -d: -f1 /etc/passwd').split()) - found['accounts']))
        groups = ' '.join(sorted(set(remote('cut -d: -f1 /etc/group').split()) - found['groups']))
        # polkit comes with libvirt on some distributions; where the guest had it before, it stays as it is.
        polkit = '' if found['polkit'] else ' polkit'
        remove = f"""if command -v apt-get >/dev/null; then DEBIAN_FRONTEND=noninteractive apt-get purge -y -q --autoremove {added} >/dev/null 2>&1 || true
else dnf remove -y -q {added} >/dev/null 2>&1 || true
fi""" if added else ''
        remote(f"""
if command -v virsh >/dev/null; then
  for domain in $({LIBVIRT} list --all --name 2>/dev/null); do
    {LIBVIRT} destroy "$domain" >/dev/null 2>&1 || true
    {LIBVIRT} undefine --nvram --managed-save --snapshots-metadata "$domain" >/dev/null 2>&1 || true
  done
  {LIBVIRT} net-destroy default >/dev/null 2>&1 || true
fi
# Each unit by itself: one that does not exist must not keep systemctl from
# stopping the others.
for unit in libvirtd virtqemud virtnetworkd virtlogd virtlockd virtstoraged virtnodedevd virtsecretd virtnwfilterd virtinterfaced virtproxyd libvirt-guests{polkit}; do
  for kind in .socket -ro.socket -admin.socket .service; do systemctl stop $unit$kind 2>/dev/null || true; done
done
{remove}
rm -rf {paths} {' '.join(OF_THE_SUITE)}
for account in {accounts}; do userdel $account 2>/dev/null || true; done
for group in {groups}; do groupdel $group 2>/dev/null || true; done
echo {found['ip_forward']} > /proc/sys/net/ipv4/ip_forward
systemctl daemon-reload
systemctl reset-failed 2>/dev/null || true
""")

    def guest_is_as_found(found):
        paths = ' '.join(path for path in LEFT_BY_THE_PACKAGES + OF_THE_SUITE if path not in found['paths'])
        new = [line for line in remote(PROGRAMS_AND_PROCESSES).splitlines() if line not in found['programs_and_processes']]
        left = '\n'.join(new + remote(f"""
ls -d {paths} /usr/libexec/qemu-kvm 2>/dev/null || true
ip -o link show virbr0 2>/dev/null || true
""").splitlines())
        packages = set(remote(INSTALLED_PACKAGES).split())
        if packages != found['packages']:
            left += f"\npackages added: {sorted(packages - found['packages'])}; packages gone: {sorted(found['packages'] - packages)}"
        for kind, file in (('accounts', '/etc/passwd'), ('groups', '/etc/group')):
            now = set(remote(f'cut -d: -f1 {file}').split())
            if now != found[kind]:
                left += f'\n{kind} added: {sorted(now - found[kind])}; {kind} gone: {sorted(found[kind] - now)}'
        units = set(remote("systemctl list-unit-files --no-legend 'libvirt*' 'virt*' 'dnsmasq*' 2>/dev/null | cut -d' ' -f1 || true").split())
        if units != found['units']:
            left += f"\nunit files added: {sorted(units - found['units'])}"
        if remote('cat /proc/sys/net/ipv4/ip_forward') != found['ip_forward']:
            left += '\nnet.ipv4.ip_forward is not as it was'
        system = vm.guest('sudo', '-n', 'systemctl', 'is-system-running', check=False).stdout.strip()
        if system != 'running':
            left += f'\nsystemd is {system}:\n' + vm.guest('sudo', '-n', '/bin/sh', '-c', vm.FAILED_UNITS, check=False).stdout
        assert left == '', f'the suite left this on the guest:\n{left}'
        print('The guest is as the suite found it: no libvirt, no QEMU, no domain, and systemd is running', flush=True)

    identity = remote('. /etc/os-release; echo "$ID ${VERSION_ID:-}"').split()
    distribution = identity[0] + (' ' + identity[1] if len(identity) > 1 else '')
    apt = identity[0] in ('debian', 'ubuntu')
    assert remote('command -v virsh || true') == '', 'the guest already has virsh'
    has_kvm = remote('test -e /dev/kvm && echo yes || echo no') == 'yes'
    # incus.py runs this suite in a virtual machine because it needs
    # /dev/kvm. Without the device there, the server gives its guests no
    # nested virtualization, and a pass of the test without virsh alone
    # would hide that.
    assert has_kvm or not is_a_virtual_machine_of_incus_py(vm.target(vm.NAME)), \
        'this guest is a virtual machine of incus.py and has no /dev/kvm; the suite needs nested virtualization on the server that runs the guests'
    found = before_the_suite()
    shutil.copytree(vm.REPO / 'examples' / EXAMPLE, example)
    write_inventories()
    try:
        # Before libvirt is installed: the node has no virsh.
        project([domain_task('mmx-test', 'running', XML_DIRECTORY + '/mmx-test.xml'), domain_task('mmx-test-gone', 'absent')])
        # The first task is not checked either, so it may change the node: the second names it.
        for result, earlier_task in zip(results(run('dryrun', 3)).values(), (None, 'mmx_test')):
            names_missing_virsh(is_result(result, 'not_checked', 'unknown'), earlier_task)
        refused = results(run('run', 1))['mmx_test']
        assert names_missing_virsh(is_result(refused, 'failed', 'unchanged'))['failure_kind'] == 'missing_prerequisite' and tries(refused) == 1, refused
        mark('without virsh, a dryrun is not checked and a run fails once as a missing prerequisite; both name the package of each family')
        if not has_kvm:
            print(f'{distribution}: this guest has no /dev/kvm, so it cannot run a domain: only the test without virsh ran. '
                  'The whole test runs in a virtual machine of incus.py, which has nested virtualization', flush=True)
            return

        # The example project: the packages, the socket unit, the XML file
        # from a template, an empty disk image, and the domain. Its first
        # tasks are the tasks under "Set up" on the module's page, which a
        # Go test checks. Its first dryrun passes although nothing is
        # installed yet.
        first = results(run('dryrun', directory=example))
        names_missing_virsh(is_result(next(r for r in first.values() if r.get('module_kind') == 'official_extra'), 'not_checked', 'unknown'))
        assert remote('command -v virsh || true') == '' and remote(f'test -e {EXAMPLE_XML} && echo there || true') == ''
        started = time.monotonic()
        applied = results(run('run', directory=example))
        installed = time.monotonic() - started
        defined = next(r for r in applied.values() if r.get('module_kind') == 'official_extra')
        is_result(defined, 'succeeded', 'changed', created=True, reason=f'created the domain {EXAMPLE_DOMAIN} from {EXAMPLE_XML}; '
                  f'set the domain {EXAMPLE_DOMAIN} to start at boot; started the domain {EXAMPLE_DOMAIN}')
        assert defined['diff'] == (f'ran: {LIBVIRT} define /dev/stdin\n'
                                   f"ran: {LIBVIRT} metadata {EXAMPLE_DOMAIN} --uri {RECORD} --key mmx --set <last_defined_file sha256='{sha256_of(EXAMPLE_XML)}'/> --config\n"
                                   f'ran: {LIBVIRT} autostart {EXAMPLE_DOMAIN}\nran: {LIBVIRT} start {EXAMPLE_DOMAIN}\n'), defined['diff']
        version = virsh('version')
        print(f'{distribution}: the example project installed libvirt and QEMU and started its domain in {installed:.0f} s; '
              + ', '.join(line.strip() for line in version.splitlines() if line.startswith(('Using library', 'Running hypervisor'))), flush=True)
        about = information(EXAMPLE_DOMAIN)
        assert (about['State'], about['Persistent'], about['Autostart']) == ('running', 'yes', 'enable'), about
        assert state(EXAMPLE_DOMAIN) == 'running (booted)' and qemu_processes(EXAMPLE_DOMAIN) == '1'
        assert record(EXAMPLE_DOMAIN) == sha256_of(EXAMPLE_XML)
        assert remote(f'stat -c %a {EXAMPLE_XML}') == '600' and "<memory unit='KiB'>1048576</memory>" in stored_xml(EXAMPLE_DOMAIN)
        for command in ('run', 'dryrun'):
            assert change(run(command, 0, directory=example)) == 'unchanged'
        mark('the example ' + EXAMPLE + ": a first dryrun before anything is installed, then libvirt and QEMU from the distribution's packages without recommended "
             'packages, the socket unit, the XML file from a template, and a domain that runs and is set to start at boot; an unchanged rerun and dryrun')

        # The template changes: the file task would write the XML file, so
        # the file on the node is not the run's file. The dryrun reports
        # both tasks, and it reads nothing of the old file.
        example_vars['shop_db_cpu_count'] = 2
        write_inventories()
        recorded = record(EXAMPLE_DOMAIN)
        # Two tasks would change something: the file task, and after it the domain's.
        would_write, would_define = [r for r in results(run('dryrun', 2, directory=example)).values() if r['change'] != 'unchanged']
        assert is_result(would_write, 'succeeded', 'would_change').get('module_kind') is None, would_write
        again = (f'the domain {EXAMPLE_DOMAIN} again from {EXAMPLE_XML}')
        is_result(would_define, 'succeeded', 'would_change', created=False,
                  reason=f'would define {again}, which an earlier node task of this dryrun would change; '
                  'the running domain would keep the old definition until it is shut off and started again')
        assert record(EXAMPLE_DOMAIN) == recorded == sha256_of(EXAMPLE_XML) and '<vcpu>1</vcpu>' in remote(f'cat {EXAMPLE_XML}')
        wrote, defined = [r for r in results(run('run', directory=example)).values() if r['change'] != 'unchanged']
        assert is_result(wrote, 'succeeded', 'changed').get('module_kind') is None, wrote
        is_result(defined, 'succeeded', 'changed', created=False,
                  reason=f'defined {again}; the running domain keeps the old definition until it is shut off and started again')
        assert record(EXAMPLE_DOMAIN) == sha256_of(EXAMPLE_XML) != recorded and re.search(r'<vcpu[^>]*>2</vcpu>', stored_xml(EXAMPLE_DOMAIN))
        for command in ('run', 'dryrun'):
            assert change(run(command, 0, directory=example)) == 'unchanged'
        mark('a changed template: the dryrun reports that the file task would write the XML file and that the domain would be defined again from it; '
             'the run does both, and a rerun changes nothing')

        # libvirt's default network, with the tasks of the module's page as
        # the page writes them, and a domain with a card on that network.
        project([])
        (workspace / 'playbooks/configure.yml').write_text('fleet_stages:\n  - id: configure\n    nodes: all\n    node_tasks:\n'
                                                           + ''.join('      ' + line + '\n' for line in tasks_of_the_page("### libvirt's `default` network").splitlines()))
        started = time.monotonic()
        run('run')
        assert 'default' in virsh('net-list --name').split() and 'default' in virsh('net-list --all --autostart --name').split()
        assert change(run('run', 0)) == 'unchanged' and remote('ip -o link show virbr0') != ''
        print(f'{distribution}: the tasks for the default network took {time.monotonic() - started:.0f} s', flush=True)
        on_network = write_xml('mmx-test-network', smallest_domain('mmx-test-network', devices=(
            "  <devices>\n    <interface type='network'>\n      <source network='default'/>\n      <mac address='52:54:00:47:11:02'/>\n"
            "      <model type='virtio'/>\n    </interface>\n  </devices>\n")))
        project([domain_task('mmx-test-network', 'running', on_network, start_at_boot=True)])
        is_result(results(run('run'))['mmx_test_network'], 'succeeded', 'changed', created=True)
        assert state('mmx-test-network') == 'running (booted)'
        mark("the page's tasks for libvirt's default network start it and set it to start at boot, change nothing on a rerun, and a domain with a card on it starts")

        # The domains start at boot: after a restart of the guest they run
        # before anything asked libvirt, which a virsh call would wake up.
        # The one with the network card needs the network at that moment.
        # Both are shut off before the restart, so that only libvirt's
        # setting can start them, and the guest does not wait for domains
        # without an operating system while it shuts down.
        virsh(f'destroy {EXAMPLE_DOMAIN}')
        virsh('destroy mmx-test-network')
        started = time.monotonic()
        vm.reboot()
        node, ssh = vm.connection()
        write_inventories()
        restarted = time.monotonic()
        deadline = restarted + 60
        while (qemu_processes(EXAMPLE_DOMAIN), qemu_processes('mmx-test-network')) != ('1', '1') and time.monotonic() < deadline:
            time.sleep(0.5)
        print(f'{distribution}: the guest restarted in {restarted - started:.0f} s, and both domains ran {time.monotonic() - restarted:.0f} s later', flush=True)
        assert (qemu_processes(EXAMPLE_DOMAIN), qemu_processes('mmx-test-network')) == ('1', '1'), remote('systemctl list-units --no-pager "libvirt*" "virt*" || true')
        assert state(EXAMPLE_DOMAIN) == 'running (booted)' and state('mmx-test-network') == 'running (booted)'
        for command in ('run', 'dryrun'):
            assert change(run(command, 0, directory=example)) == 'unchanged' and change(run(command, 0)) == 'unchanged'
        # One run shuts the domain off and removes its definition.
        project([{**domain_task('mmx-test-network', 'stopped', on_network, wait_for_shutdown_at_most="0s", power_off_when_shutdown_takes_longer=True), 'id': 'stopped'},
                 {**domain_task('mmx-test-network', 'absent'), 'id': 'absent'}])
        # A dryrun of the two tasks cannot know that the first one shuts
        # the domain off: the second one is not checked, and nothing fails.
        preview = results(run('dryrun', 3))
        is_result(preview['stopped'], 'succeeded', 'would_change',
                  reason='would ask the domain mmx-test-network to shut down and wait at most 0 seconds; would cut its power if it still runs then')
        is_result(preview['absent'], 'not_checked', 'unknown')
        assert state('mmx-test-network') == 'running (booted)'
        taken_away = results(run('run'))
        is_result(taken_away['stopped'], 'succeeded', 'changed', reason='asked the domain mmx-test-network to shut down; '
                  'cut the power of the domain mmx-test-network, which still ran after 0 seconds')
        is_result(taken_away['absent'], 'succeeded', 'changed', reason='removed the definition of the domain mmx-test-network')
        assert domains() == {EXAMPLE_DOMAIN}
        mark('after a restart of the guest the example\'s domain and the domain on the default network, both shut off before it, run before any virsh call, '
             'and a run changes nothing; one run then shuts a domain off and removes its definition, and its dryrun fails nothing')

        # A first run of the module's own tasks. The XML file of one domain
        # does not exist yet: a file task would write it.
        small = write_xml('mmx-test', smallest_domain('mmx-test'))
        # The first task would create the domain, but dryrun leaves it absent
        # for the later task, which deliberately keeps the stored definition.
        project([{**domain_task('mmx-test', 'stopped', small), 'id': 'define_stopped_domain'},
                 domain_task('mmx-test', 'running')])
        two_tasks = results(run('dryrun'))
        is_result(two_tasks['define_stopped_domain'], 'succeeded', 'would_change', created=True,
                  reason=f'would create the domain mmx-test from {small}')
        is_result(two_tasks['mmx_test'], 'not_checked', 'unknown',
                  reason=vm.earlier_change_may_provide('define_stopped_domain', 'libvirt_domain: the domain mmx-test does not exist; '
                                                       'create it in an earlier node task with domain_xml_file, or add domain_xml_file to this task'))
        assert domains() == {EXAMPLE_DOMAIN}
        mark('a dryrun can define a stopped domain and then start it without XML: the later task is not checked because its prerequisite is not created yet')
        tasks = [domain_task('mmx-test', 'running', small, start_at_boot=True),
                 domain_task('mmx-test-later', 'stopped', XML_DIRECTORY + '/mmx-test-later.xml')]
        project(tasks)
        preview = results(run('dryrun'))
        is_result(preview['mmx_test'], 'succeeded', 'would_change', created=True,
                  reason=f'would create the domain mmx-test from {small}; would set the domain mmx-test to start at boot; would start the domain mmx-test')
        assert preview['mmx_test']['diff'].startswith(f'would run: {LIBVIRT} define /dev/stdin\n'), preview['mmx_test']
        no_file = f'libvirt_domain: the file {XML_DIRECTORY}/mmx-test-later.xml of domain_xml_file does not exist; write it in an earlier node task, for example with file'
        # The task mmx_test would change the node first.
        is_result(preview['mmx_test_later'], 'not_checked', 'unknown', reason=vm.earlier_change_may_provide('mmx_test', no_file))
        assert domains() == {EXAMPLE_DOMAIN}
        failed = results(run('run', 1))
        is_result(failed['mmx_test'], 'succeeded', 'changed', created=True,
                  reason=f'created the domain mmx-test from {small}; set the domain mmx-test to start at boot; started the domain mmx-test')
        assert is_result(failed['mmx_test_later'], 'failed', 'unchanged', reason=no_file)['failure_kind'] == 'missing_prerequisite' and tries(failed['mmx_test_later']) == 1
        about = information('mmx-test')
        uuid = about['UUID']
        assert (about['State'], about['Persistent'], about['Autostart']) == ('running', 'yes', 'enable') and record('mmx-test') == sha256_of(small), about
        project(tasks[:1])
        for command in ('run', 'dryrun'):
            assert is_result(results(run(command, 0))['mmx_test'], 'succeeded', 'unchanged', created=False).get('reason', '') == ''
        mark('a first run defines, records, sets the start at boot, and starts; a dryrun before it changes nothing and says what a run would do; '
             'a missing XML file is not checked in a dryrun and a missing prerequisite in a run; an unchanged rerun and dryrun')

        # The file changes while the domain runs: libvirt refuses a second
        # definition without the domain's UUID, so the module adds it. The
        # running domain keeps its old definition.
        write_xml('mmx-test', smallest_domain('mmx-test', 96))
        again = f'the domain mmx-test again from {small}'
        is_result(results(run('dryrun'))['mmx_test'], 'succeeded', 'would_change', created=False,
                  reason=f'would define {again}; the running domain would keep the old definition until it is shut off and started again')
        assert "<memory unit='KiB'>65536</memory>" in stored_xml('mmx-test')
        is_result(results(run('run'))['mmx_test'], 'succeeded', 'changed', created=False,
                  reason=f'defined {again}; the running domain keeps the old definition until it is shut off and started again')
        about = information('mmx-test')
        assert (about['UUID'], about['State'], about['Autostart']) == (uuid, 'running', 'enable') and record('mmx-test') == sha256_of(small), about
        assert "<memory unit='KiB'>98304</memory>" in stored_xml('mmx-test') and "<memory unit='KiB'>65536</memory>" in virsh('dumpxml mmx-test')
        assert change(run('run', 0)) == 'unchanged'
        # A run that ended between the define and the record: the next run
        # finds no record and defines again.
        virsh(f'metadata mmx-test --uri {RECORD} --key mmx --remove --config')
        no_record = record('mmx-test')
        assert 'last_defined_file' not in no_record, no_record
        is_result(results(run('run'))['mmx_test'], 'succeeded', 'changed', created=False,
                  reason=f'defined {again}; the running domain keeps the old definition until it is shut off and started again')
        assert record('mmx-test') == sha256_of(small) and information('mmx-test')['UUID'] == uuid and change(run('run', 0)) == 'unchanged'
        print(f"{distribution}: virsh answers a domain without a record with: {no_record!r}", flush=True)
        mark('a changed file defines the domain again with its own UUID, the running domain keeps its old definition and the reason says so; '
             'a run that ended before the record defines again')

        # A network card without a MAC address gets a new one at every
        # define; a card with its address in the file keeps it.
        card = "  <devices>\n    <interface type='user'>\n{mac}      <model type='virtio'/>\n    </interface>\n  </devices>\n"
        with_card = write_xml('mmx-test-card', smallest_domain('mmx-test-card', devices=card.format(mac='')))
        project([domain_task('mmx-test-card', 'stopped', with_card)])

        def mac_address():
            return re.search(r"<mac address='([0-9a-f:]+)'/>", stored_xml('mmx-test-card')).group(1)
        is_result(results(run('run'))['mmx_test_card'], 'succeeded', 'changed', created=True, reason=f'created the domain mmx-test-card from {with_card}')
        card_uuid, first_address = information('mmx-test-card')['UUID'], mac_address()
        write_xml('mmx-test-card', smallest_domain('mmx-test-card', 96, devices=card.format(mac='')))
        is_result(results(run('run'))['mmx_test_card'], 'succeeded', 'changed', created=False, reason=f'defined the domain mmx-test-card again from {with_card}')
        second_address = mac_address()
        assert information('mmx-test-card')['UUID'] == card_uuid and second_address != first_address, (first_address, second_address)
        for memory in (64, 96):
            write_xml('mmx-test-card', smallest_domain('mmx-test-card', memory, devices=card.format(mac="      <mac address='52:54:00:47:11:01'/>\n")))
            assert change(run('run')) == 'changed' and mac_address() == '52:54:00:47:11:01' and information('mmx-test-card')['UUID'] == card_uuid
        assert information('mmx-test-card')['State'] == 'shut off' and change(run('run', 0)) == 'unchanged'
        mark('a domain keeps its UUID through every define; a network card without a MAC address in the file gets a new address at each define, and one with its address keeps it')

        # stopped for a domain whose operating system never shuts down: the
        # task waits its 3 seconds and fails, or cuts the power when it says so.
        # MMX tries a failed task three times by default; this failure is
        # not tried again, so the task waits once.
        waits = domain_task('mmx-test', 'stopped', wait_for_shutdown_at_most="3s")
        project([waits])
        is_result(results(run('dryrun'))['mmx_test'], 'succeeded', 'would_change', reason='would ask the domain mmx-test to shut down and wait at most 3 seconds')
        started = time.monotonic()
        stayed = results(run('run', 1))['mmx_test']
        waited = time.monotonic() - started
        is_result(stayed, 'failed', 'changed', created=False, reason=(
            'libvirt_domain: the domain mmx-test did not shut down within 3 seconds after the task asked it to (its state is running); its operating system may ignore the request. '
            'Set power_off_when_shutdown_takes_longer: true to cut its power then, or give it more time with wait_for_shutdown_at_most'))
        assert 'failure_kind' not in stayed and tries(stayed) == 1 and stayed['diff'] == f'ran: {LIBVIRT} shutdown mmx-test\n', stayed
        assert state('mmx-test') == 'running (booted)' and 3 <= waited < 9, waited
        waits['libvirt_domain']['power_off_when_shutdown_takes_longer'] = True
        project([waits])
        is_result(results(run('dryrun'))['mmx_test'], 'succeeded', 'would_change',
                  reason='would ask the domain mmx-test to shut down and wait at most 3 seconds; would cut its power if it still runs then')
        cut = is_result(results(run('run'))['mmx_test'], 'succeeded', 'changed', created=False,
                        reason='asked the domain mmx-test to shut down; cut the power of the domain mmx-test, which still ran after 3 seconds')
        assert cut['diff'] == f'ran: {LIBVIRT} shutdown mmx-test\nran: {LIBVIRT} destroy mmx-test\n' and state('mmx-test') == 'shut off (destroyed)', cut
        assert qemu_processes('mmx-test') == '0' and information('mmx-test')['Autostart'] == 'enable' and change(run('run', 0)) == 'unchanged'
        print(f'{distribution}: a shutdown that the domain ignores failed the task after {waited:.0f} s', flush=True)
        mark('stopped waits its seconds once for a domain that ignores the request: the task fails without another try and the domain runs on, or with '
             'power_off_when_shutdown_takes_longer the module cuts the power')

        # The good path of the wait: a domain that ends by itself is running
        # when the task asks and shut off within the wait. One run starts it
        # and shuts it off, so the second task meets it while it runs.
        remote(f'cp /boot/vmlinuz-$(uname -r) {KERNEL_COPY} 2>/dev/null || cp /lib/modules/$(uname -r)/vmlinuz {KERNEL_COPY}')
        ends = write_xml('mmx-test-ends', domain_that_ends_by_itself('mmx-test-ends', 2))
        project([{**domain_task('mmx-test-ends', 'running', ends), 'id': 'start'}, {**domain_task('mmx-test-ends', 'stopped', ends, wait_for_shutdown_at_most="60s"), 'id': 'stopped'}])
        ended = results(run('run'))
        is_result(ended['start'], 'succeeded', 'changed', created=True, reason=f'created the domain mmx-test-ends from {ends}; started the domain mmx-test-ends')
        seconds = re.fullmatch(r'asked the domain mmx-test-ends to shut down; the domain mmx-test-ends was shut off after (\d+) seconds', ended['stopped']['reason'])
        assert seconds and 1 <= int(seconds.group(1)) < 60 and ended['stopped']['diff'] == f'ran: {LIBVIRT} shutdown mmx-test-ends\n', ended['stopped']
        assert state('mmx-test-ends') == 'shut off (shutdown)' and 'Rebooting in 2 seconds' in remote(f'cat {SERIAL_LOG}'), state('mmx-test-ends')
        print(f"{distribution}: the domain that ends by itself was shut off {seconds.group(1)} s after the request", flush=True)
        mark('stopped asks a running domain once and finds it shut off within the wait, without cutting its power')

        # running starts a domain that is shut off, with the definition of
        # the changed file, and resumes a paused one. stopped refuses a
        # paused domain, also with the permission to cut the power.
        definition_before = stored_xml('mmx-test')
        project([domain_task('mmx-test', 'running', start_at_boot=False)])
        is_result(results(run('dryrun'))['mmx_test'], 'succeeded', 'would_change', reason='would set the domain mmx-test not to start at boot; would start the domain mmx-test')
        is_result(results(run('run'))['mmx_test'], 'succeeded', 'changed', created=False, reason='set the domain mmx-test not to start at boot; started the domain mmx-test')
        about = information('mmx-test')
        assert (about['State'], about['Autostart'], about['UUID']) == ('running', 'disable', uuid) and "<memory unit='KiB'>98304</memory>" in virsh('dumpxml mmx-test'), about
        assert stored_xml('mmx-test') == definition_before
        virsh('suspend mmx-test')
        assert state('mmx-test') == 'paused (user)'
        project([domain_task('mmx-test', 'stopped', small, wait_for_shutdown_at_most="3s", power_off_when_shutdown_takes_longer=True)])
        paused = 'libvirt_domain: the domain mmx-test is paused, and a request to shut down cannot reach a paused domain; resume it first (virsh resume mmx-test)'
        for command in ('dryrun', 'run'):
            cannot_succeed(results(run(command, 1))['mmx_test'], 'unchanged', paused)
            assert state('mmx-test') == 'paused (user)'
        project([domain_task('mmx-test', 'running')])
        is_result(results(run('dryrun'))['mmx_test'], 'succeeded', 'would_change', reason='would resume the paused domain mmx-test')
        is_result(results(run('run'))['mmx_test'], 'succeeded', 'changed', reason='resumed the paused domain mmx-test')
        assert state('mmx-test') == 'running (unpaused)' and change(run('run', 0)) == 'unchanged'
        mark('running starts a domain that is shut off, which then has the new definition, and resumes a paused one; stopped fails once for a paused domain; '
             'start_at_boot: false takes the setting away; without domain_xml_file, the stored definition stays')

        # A domain that runs without a stored definition: the module stores
        # one, and the domain was not created.
        transient = write_xml('mmx-test-transient', smallest_domain('mmx-test-transient'))
        virsh(f'create {transient}')
        assert information('mmx-test-transient')['Persistent'] == 'no'
        project([domain_task('mmx-test-transient', 'running', transient, start_at_boot=True)])
        is_result(results(run('run'))['mmx_test_transient'], 'succeeded', 'changed', created=False,
                  reason=f'stored a definition for the domain mmx-test-transient, which ran without one, from {transient}; set the domain mmx-test-transient to start at boot; '
                  'the running domain keeps the old definition until it is shut off and started again')
        about = information('mmx-test-transient')
        assert (about['State'], about['Persistent'], about['Autostart']) == ('running', 'yes', 'enable') and record('mmx-test-transient') == sha256_of(transient), about
        assert change(run('run', 0)) == 'unchanged'
        mark('a domain that runs without a stored definition gets one, with its record and its start at boot, and is not reported as created')

        # The inputs and files that the module refuses before it changes
        # anything, and a file that libvirt refuses.
        other = write_xml('mmx-test-other', smallest_domain('mmx-test-not-this'))
        project([domain_task('mmx-test-other', 'running', other)])
        for command in ('dryrun', 'run'):
            cannot_succeed(results(run(command, 1))['mmx_test_other'], 'unchanged',
                           f'libvirt_domain.name: the file {other} does not hold <name>mmx-test-other</name>; name must be the <name> of the domain in the XML file')
        project([{**domain_task('12345', 'absent'), 'id': 'number'}])
        assert results(run('run', 1))['number']['reason'].startswith('libvirt_domain.name: must not be a number, and 12345 is one')
        # Not well-formed, with a password in the line that a newer libvirt
        # quotes: the line reaches no output of MMX. And a well-formed file
        # without <os>.
        broken = write_xml('mmx-test-broken', f"<domain type='kvm'>\n  <name>mmx-test-broken</name>\n  <graphics type='vnc' passwd='{PASSWORD}'>\n</domain>\n")
        project([domain_task('mmx-test-broken', 'running', broken)])
        refused = results(run('run', 1))['mmx_test_broken']
        said = refused['reason'].split('/dev/stdin: ', 1)[-1]
        # libvirt refuses the same file at every try, so MMX tries once.
        cannot_succeed(refused, 'changed', f'libvirt_domain: defining the domain mmx-test-broken from {broken}: Failed to define domain from /dev/stdin: {said}')
        assert 'Opening and ending tag mismatch: graphics line 3 and domain' in said and PASSWORD not in json.dumps(refused), refused
        print(f'{distribution}: libvirt refused a file that is not well-formed with: {said}', flush=True)
        write_xml('mmx-test-broken', "<domain type='kvm'>\n  <name>mmx-test-broken</name>\n  <memory unit='MiB'>64</memory>\n</domain>\n")
        cannot_succeed(results(run('run', 1))['mmx_test_broken'], 'changed', f'libvirt_domain: defining the domain mmx-test-broken from {broken}: '
                       'Failed to define domain from /dev/stdin: XML error: an os <type> must be specified')
        assert domains() == {EXAMPLE_DOMAIN, 'mmx-test', 'mmx-test-card', 'mmx-test-ends', 'mmx-test-transient'}, domains()
        mark('a file of another domain and a name that virsh would take for an ID fail before anything changes; a file that libvirt refuses fails once with '
             "libvirt's message and without a line of the file")

        for wanted_state in ('running', 'stopped'):
            project([domain_task('mmx-test-missing-definition', wanted_state)])
            missing = ('libvirt_domain: the domain mmx-test-missing-definition does not exist; create it in an earlier node task with domain_xml_file, '
                       'or add domain_xml_file to this task')
            is_result(results(run('dryrun'))['mmx_test_missing_definition'], 'not_checked', 'unknown', reason=missing)
            missing_state(results(run('run', 1))['mmx_test_missing_definition'], missing)
            assert 'mmx-test-missing-definition' not in domains()
        mark('a missing domain without domain_xml_file is not checked in a dryrun and fails once as a missing prerequisite in a run')

        # A start that libvirt refuses after the define: the result is
        # failed and changed and says that the domain was created. The next
        # run does only what is missing.
        disk = ("  <devices>\n    <disk type='file' device='disk'>\n      <driver name='qemu' type='qcow2'/>\n"
                f"      <source file='{DISK_IMAGE}'/>\n      <target dev='vda' bus='virtio'/>\n    </disk>\n"
                f"    <graphics type='vnc' port='-1' passwd='{PASSWORD}'/>\n  </devices>\n")
        with_disk = write_xml('mmx-test-disk', smallest_domain('mmx-test-disk', devices=disk))
        project([domain_task('mmx-test-disk', 'running', with_disk)])
        no_image = results(run('run', 1))['mmx_test_disk']
        # Another try would find the domain and report created: false, so
        # MMX tries once, also with its default of three tries.
        is_result(no_image, 'failed', 'changed', created=True)
        assert tries(no_image) == 1 and 'failure_kind' not in no_image, no_image
        assert no_image['reason'].startswith("libvirt_domain: starting the domain mmx-test-disk: Failed to start domain 'mmx-test-disk': ") and DISK_IMAGE in no_image['reason'], no_image
        assert no_image['reason'].endswith(f'. Before that, the task created the domain mmx-test-disk from {with_disk}'), no_image
        assert information('mmx-test-disk')['State'] == 'shut off' and record('mmx-test-disk') == sha256_of(with_disk)
        remote(f'qemu-img create -q -f qcow2 {DISK_IMAGE} 16M')
        is_result(results(run('run'))['mmx_test_disk'], 'succeeded', 'changed', created=False, reason='started the domain mmx-test-disk')
        assert state('mmx-test-disk') == 'running (booted)'
        mark('a start that libvirt refuses after the task created the domain fails without another try and reports changed and created with libvirt\'s message; '
             'the next run only starts the domain')

        # absent. A domain that runs keeps its definition: libvirt itself
        # would remove it and leave the domain running.
        gone = [domain_task('mmx-test-disk', 'absent'), domain_task('mmx-test-never-there', 'absent')]
        project(gone)
        runs = ('libvirt_domain: the domain mmx-test-disk is not shut off (its state is running), and absent removes the definition only of a domain that is shut off. '
                'Shut the domain off in an earlier node task, with state: stopped')
        preview = results(run('dryrun'))
        is_result(preview['mmx_test_disk'], 'not_checked', 'unknown', reason=runs)
        is_result(preview['mmx_test_never_there'], 'succeeded', 'unchanged', created=False)
        project(gone[:1])
        missing_state(results(run('run', 1))['mmx_test_disk'], runs)
        assert information('mmx-test-disk')['Persistent'] == 'yes' and state('mmx-test-disk') == 'running (booted)'

        def blocked_by(reason, libvirt_says):
            """The task keeps the definition and says what an earlier node task can do: not checked in a dryrun, one try in a run. With libvirt_says, libvirt itself refuses too, in these words."""
            is_result(results(run('dryrun', 3))['mmx_test_disk'], 'not_checked', 'unknown', reason=reason)
            missing_state(results(run('run', 1))['mmx_test_disk'], reason)
            if libvirt_says:
                answer = virsh('undefine --keep-nvram mmx-test-disk', check=False)
                assert libvirt_says in answer, answer
            assert 'mmx-test-disk' in domains()
        # A saved memory image.
        virsh('managedsave mmx-test-disk')
        assert information('mmx-test-disk')['Managed save'] == 'yes'
        blocked_by('libvirt_domain: the domain mmx-test-disk has a saved memory image, and libvirt does not remove the definition of such a domain. '
                   'Delete the image in an earlier node task (virsh managedsave-remove mmx-test-disk), or start the domain and shut it off',
                   'Refusing to undefine while domain managed save image exists')
        virsh('managedsave-remove mmx-test-disk')
        # A snapshot.
        virsh('snapshot-create-as mmx-test-disk before-upgrade')
        blocked_by('libvirt_domain: the domain mmx-test-disk has snapshots (before-upgrade), and libvirt does not remove the definition of such a domain. '
                   'Delete each of them in an earlier node task (virsh snapshot-delete mmx-test-disk NAME)',
                   'cannot delete inactive domain with 1 snapshots')
        virsh('snapshot-delete mmx-test-disk before-upgrade')
        # Checkpoints, which libvirt makes of a domain that runs. libvirt
        # itself would remove the definition and, without a word, its record
        # of each checkpoint. The module refuses, and names the command that
        # removes the records and leaves the checkpoints' bitmaps in the image.
        assert remote('virsh help checkpoint-list >/dev/null 2>&1 && echo yes || echo no') == 'yes', 'this virsh has no checkpoint-list'
        virsh('start mmx-test-disk')
        for checkpoint in ('before-backup', 'before-the-next-backup'):
            virsh(f'checkpoint-create-as mmx-test-disk {checkpoint}')
        virsh('destroy mmx-test-disk')
        checkpoints = virsh('checkpoint-list mmx-test-disk --name').split()
        records = '/var/lib/libvirt/qemu/checkpoint/mmx-test-disk'
        assert sorted(checkpoints) == ['before-backup', 'before-the-next-backup'] and remote(f'ls {records}') != '', (checkpoints, remote(f'ls -la {records} 2>&1 || true'))
        blocked_by(f"libvirt_domain: the domain mmx-test-disk has checkpoints ({', '.join(checkpoints)}). libvirt would remove its record of each checkpoint with the definition, "
                   'and absent removes nothing but the definition. Remove the records in an earlier node task (virsh checkpoint-delete mmx-test-disk NAME --metadata)', None)
        assert virsh('checkpoint-list mmx-test-disk --name').split() == checkpoints
        for checkpoint in ('before-the-next-backup', 'before-backup'):
            virsh(f'checkpoint-delete mmx-test-disk {checkpoint} --metadata')
        bitmaps = remote(f"qemu-img info {DISK_IMAGE} | grep -c 'name: before-' || true")
        assert virsh('checkpoint-list mmx-test-disk --name') == '' and remote(f'ls {records} 2>/dev/null || true') == '' and bitmaps == '2', (bitmaps, remote(f'qemu-img info {DISK_IMAGE}'))
        project(gone)
        is_result(results(run('dryrun'))['mmx_test_disk'], 'succeeded', 'would_change', created=False, reason='would remove the definition of the domain mmx-test-disk')
        assert 'mmx-test-disk' in domains()
        removed = results(run('run'))
        is_result(removed['mmx_test_disk'], 'succeeded', 'changed', created=False, reason='removed the definition of the domain mmx-test-disk')
        is_result(removed['mmx_test_never_there'], 'succeeded', 'unchanged', created=False)
        assert removed['mmx_test_disk']['diff'] == f"ran: {LIBVIRT} undefine --keep-nvram --keep-tpm mmx-test-disk\n", removed['mmx_test_disk']
        assert 'mmx-test-disk' not in domains() and remote(f'test -s {DISK_IMAGE} && echo kept') == 'kept'
        for command in ('run', 'dryrun'):
            assert change(run(command, 0)) == 'unchanged'
        mark('absent removes the definition of a domain that is shut off and keeps its disk image file; a domain that runs, or has a saved memory image, a snapshot, or checkpoints, keeps its definition: '
             'a dryrun is not checked, a run fails once and names what an earlier node task can do; with the records of the checkpoints removed as the reason says, '
             'the task removes the definition; a domain that is not there is unchanged')

        # The packages that the module's page names for UEFI firmware and for
        # an emulated TPM. On Debian and Ubuntu they are
        # packages of their own; elsewhere the packages of the example
        # brought some of them already.
        started = time.monotonic()
        project([{'when': {'equals': ['{{ .node.package_manager }}', 'apt']}, 'apt_package': {'packages': [{'name': 'ovmf', 'version': 'newest_when_first_installed'}, {'name': 'swtpm', 'version': 'newest_when_first_installed'}, {'name': 'swtpm-tools', 'version': 'newest_when_first_installed'}]}},
                 {'when': {'equals': ['{{ .node.package_manager }}', 'dnf']}, 'dnf_package': {'packages': [{'name': 'edk2-ovmf', 'version': 'newest_when_first_installed'}, {'name': 'swtpm', 'version': 'newest_when_first_installed'}, {'name': 'swtpm-tools', 'version': 'newest_when_first_installed'}]}}])
        came_with_the_example = change(run('run')) == 'unchanged'
        print(f'{distribution}: the packages for UEFI firmware and an emulated TPM '
              + ('came with the packages of the example' if came_with_the_example else f'installed in {time.monotonic() - started:.0f} s'), flush=True)

        # A UEFI domain that was started once has a file of UEFI variables,
        # with which a plain virsh undefine fails. The module keeps the file.
        uefi = write_xml('mmx-test-uefi', "<domain type='kvm'>\n  <name>mmx-test-uefi</name>\n  <memory unit='MiB'>256</memory>\n"
                         "  <os firmware='efi'>\n    <type arch='x86_64' machine='q35'>hvm</type>\n  </os>\n"
                         "  <features>\n    <acpi/>\n  </features>\n</domain>\n")
        project([domain_task('mmx-test-uefi', 'running', uefi)])
        run('run')
        variables = re.search(r"<nvram[^>]*>([^<]+)</nvram>", virsh('dumpxml mmx-test-uefi')).group(1)
        virsh('destroy mmx-test-uefi')
        assert remote(f'test -s {variables} && echo there') == 'there'
        print(f"{distribution}: a plain virsh undefine of the UEFI domain answers: {virsh('undefine mmx-test-uefi', check=False)!r}", flush=True)
        assert 'mmx-test-uefi' in domains()
        project([domain_task('mmx-test-uefi', 'absent')])
        is_result(results(run('run'))['mmx_test_uefi'], 'succeeded', 'changed', reason='removed the definition of the domain mmx-test-uefi')
        assert 'mmx-test-uefi' not in domains() and remote(f'test -s {variables} && echo kept') == 'kept'
        remote(f'rm -f {variables}')
        mark('absent removes the definition of a UEFI domain that was started once, which a plain virsh undefine refuses, and the file of its UEFI variables stays')

        # An emulated TPM: its state can hold a disk's key, and a plain
        # virsh undefine deletes it. The task keeps it.
        tpm = write_xml('mmx-test-tpm', "<domain type='kvm'>\n  <name>mmx-test-tpm</name>\n  <memory unit='MiB'>128</memory>\n"
                        "  <os>\n    <type arch='x86_64' machine='q35'>hvm</type>\n  </os>\n"
                        "  <devices>\n    <tpm model='tpm-crb'><backend type='emulator' version='2.0'/></tpm>\n  </devices>\n</domain>\n")
        project([domain_task('mmx-test-tpm', 'running', tpm)])
        run('run')
        tpm_uuid = information('mmx-test-tpm')['UUID']
        tpm_state = '/var/lib/libvirt/swtpm/' + tpm_uuid
        virsh('destroy mmx-test-tpm')
        assert remote(f'test -d {tpm_state} && echo there') == 'there'
        project([domain_task('mmx-test-tpm', 'absent')])
        is_result(results(run('dryrun'))['mmx_test_tpm'], 'succeeded', 'would_change', reason='would remove the definition of the domain mmx-test-tpm')
        is_result(results(run('run'))['mmx_test_tpm'], 'succeeded', 'changed', reason='removed the definition of the domain mmx-test-tpm')
        assert 'mmx-test-tpm' not in domains()
        assert remote(f'test -d {tpm_state} && echo kept') == 'kept'
        mark('absent keeps the state of an emulated TPM')

        # While libvirt does not answer: not checked in a dryrun, because an
        # earlier node task may start the socket unit, and a failure with
        # virsh's message in a run, which is tried again.
        daemon = 'libvirtd' if apt else 'virtqemud'
        units = f'{daemon}.service {daemon}.socket {daemon}-ro.socket {daemon}-admin.socket'
        remote(f'systemctl stop {units}')
        project([domain_task('mmx-test', 'running', small), domain_task('mmx-test-gone', 'absent')])
        silent = 'libvirt_domain: libvirt at qemu:///system does not answer: failed to connect to the hypervisor: Failed to connect socket to '
        advice = ("; start libvirt's socket unit in an earlier node task (libvirtd.socket on Debian and Ubuntu, virtqemud.socket on the other distributions)")
        for result in results(run('dryrun', 3)).values():
            assert is_result(result, 'not_checked', 'unknown')['reason'].startswith(silent) and result['reason'].endswith(advice), result
        stopped = results(run('run', 1))['mmx_test']
        assert stopped['status'] == 'failed' and silent[len('libvirt_domain: '):] in stopped['reason'] and tries(stopped) == 3 and 'failure_kind' not in stopped, stopped
        remote(f'systemctl start {daemon}.socket {daemon}-ro.socket {daemon}-admin.socket')
        # The domains ran on without the daemon.
        assert change(run('run', 0)) == 'unchanged' and state('mmx-test') == 'running (unpaused)'
        mark('while libvirt does not answer, a dryrun is not checked and a run fails three times with virsh\'s message; the domains run on')

        # The password of the domain's XML is in nothing that MMX printed or
        # saved, on the controller machine or on the node.
        assert PASSWORD in remote(f'cat {with_disk}')
        assert PASSWORD not in ''.join(printed)
        for path in list(workspace.rglob('*')) + list(example.rglob('*')):
            if path.is_file() and 'runners' not in path.parts:
                assert PASSWORD.encode() not in path.read_bytes(), path
        # The pattern comes from a file: sudo writes each command line of
        # the suite into the journal.
        remote(f'install -m 600 /dev/null {XML_DIRECTORY}/pattern\ncat > {XML_DIRECTORY}/pattern', input=PASSWORD + '\n')
        assert remote(f'grep -rlF -f {XML_DIRECTORY}/pattern /var/lib/mmx /run/mmx 2>/dev/null || true') == ''
        logged = remote(f'journalctl --no-pager -q 2>/dev/null | grep -F -f {XML_DIRECTORY}/pattern || true')
        assert logged == '', logged
        mark("the password in a domain's XML file is in no output of mmx, no saved run, no record of MMX on the node, and not in the journal")
    finally:
        failing = sys.exc_info()[0] is not None
        started = time.monotonic()
        cleanup(found)
        print(f'{distribution}: the removal of the domains, the packages, and what they leave behind took {time.monotonic() - started:.0f} s', flush=True)
        if not failing:
            guest_is_as_found(found)
            (workspace / 'summary.json').write_text(json.dumps(passed, indent=2) + '\n')
            print(f'{len(passed)} libvirt workflows passed on {distribution}', flush=True)


if __name__ == '__main__':
    vm.run_suite(main)
