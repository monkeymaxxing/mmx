"""Which suites incus.py runs in containers and which in VMs; no guests are needed."""
import contextlib
import importlib.util
import io
import json
from pathlib import Path
import queue
import tempfile
import threading
import time
from types import SimpleNamespace
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location('incus', Path(__file__).with_name('incus.py'))
incus = importlib.util.module_from_spec(spec)
spec.loader.exec_module(incus)


class TargetsTests(unittest.TestCase):
    def test_the_file_of_targets_tells_a_suite_that_its_guest_is_a_virtual_machine_of_incus_py(self):
        # libvirt_domains.py passes without /dev/kvm on a Hetzner Cloud
        # server, and fails without it in a virtual machine of incus.py. It reads the kind of guest from what
        # ready_targets writes.
        spec = importlib.util.spec_from_file_location('libvirt_domains', Path(__file__).with_name('libvirt_domains.py'))
        suite = importlib.util.module_from_spec(spec)
        spec.loader.exec_module(suite)
        with tempfile.TemporaryDirectory() as directory:
            run = SimpleNamespace(directory=Path(directory), login_key=Path(directory) / 'login-key')
            with patch.object(incus, 'start_guest', side_effect=lambda run, distribution, kind, slot, role: (f'{role}-of-the-test', '10.0.0.2', 'ssh-ed25519 AAAA')), \
                    patch.object(incus.vm, 'cmd', return_value=SimpleNamespace(stdout='Debian GNU/Linux 13 (trixie), kernel 6.12')):
                for kind, virtual_machine in ((incus.VIRTUAL_MACHINE, True), (incus.CONTAINER, False)):
                    path, _ = incus.ready_targets(run, 'debian13', kind, 1, [])
                    target = json.loads(path.read_text())['mmx-dev']
                    self.assertEqual(suite.is_a_virtual_machine_of_incus_py(target), virtual_machine, target)
        # A Hetzner Cloud server's target has no Incus instance.
        self.assertFalse(suite.is_a_virtual_machine_of_incus_py({'node': {}, 'platform': 'Debian GNU/Linux 13 (trixie), kernel 6.12 AMD64', 'architecture': 'amd64'}))


class PackageServerTests(unittest.TestCase):
    """Each guest's sources name the ordered package servers; the suites and
    components stay as the image has them."""
    DEBIAN = 'mirror+file:/etc/apt/mmx-test-package-servers/debian-packages.list'
    DEBIAN_SECURITY = 'mirror+file:/etc/apt/mmx-test-package-servers/debian-security.list'

    def test_a_one_line_sources_list_names_the_lists(self):
        text, used = incus.apt_sources_with_package_server_lists(
            'deb http://deb.debian.org/debian bookworm main\n'
            'deb [arch=amd64] http://deb.debian.org/debian bookworm-updates main contrib\n'
            'deb http://security.debian.org/debian-security/ bookworm-security main\n'
            'deb http://packages.example.com/debian bookworm main\n', incus.DEBIAN_ARCHIVES)
        self.assertEqual(text, f'deb {self.DEBIAN} bookworm main\n'
                               f'deb [arch=amd64] {self.DEBIAN} bookworm-updates main contrib\n'
                               f'deb {self.DEBIAN_SECURITY} bookworm-security main\n'
                               'deb http://packages.example.com/debian bookworm main\n')
        self.assertEqual(used, {'debian-packages', 'debian-security'})

    def test_deb822_sources_name_the_lists_and_keep_suites_and_keys(self):
        sources = ('Types: deb\nURIs: http://deb.debian.org/debian\nSuites: trixie trixie-updates\nComponents: main\n'
                   'Signed-By: /usr/share/keyrings/debian-archive-keyring.gpg\n\n'
                   'Types: deb\nURIs: http://deb.debian.org/debian-security/\nSuites: trixie-security\nComponents: main\n'
                   'Signed-By: /usr/share/keyrings/debian-archive-keyring.gpg\n')
        text, used = incus.apt_sources_with_package_server_lists(sources, incus.DEBIAN_ARCHIVES)
        self.assertEqual(text, sources.replace('URIs: http://deb.debian.org/debian\n', f'URIs: {self.DEBIAN}\n')
                                      .replace('URIs: http://deb.debian.org/debian-security/\n', f'URIs: {self.DEBIAN_SECURITY}\n'))
        self.assertEqual(used, {'debian-packages', 'debian-security'})
        text, used = incus.apt_sources_with_package_server_lists(
            'Types: deb\nURIs: http://archive.ubuntu.com/ubuntu/\nSuites: noble noble-updates noble-backports\n\n'
            'Types: deb\nURIs: http://security.ubuntu.com/ubuntu/\nSuites: noble-security\n', incus.UBUNTU_ARCHIVES)
        self.assertEqual(text, 'Types: deb\nURIs: mirror+file:/etc/apt/mmx-test-package-servers/ubuntu-packages.list\n'
                               'Suites: noble noble-updates noble-backports\n\n'
                               'Types: deb\nURIs: mirror+file:/etc/apt/mmx-test-package-servers/ubuntu-security.list\nSuites: noble-security\n')
        self.assertEqual(used, {'ubuntu-packages', 'ubuntu-security'})

    def test_apt_tries_the_distributions_server_first_and_hetzner_second(self):
        self.assertEqual(incus.apt_package_server_list(incus.DEBIAN_ARCHIVES['debian-security'][1]),
                         'http://deb.debian.org/debian-security\tpriority:1\nhttp://mirror.hetzner.com/debian/security\tpriority:2\n')
        for archives in (incus.DEBIAN_ARCHIVES, incus.UBUNTU_ARCHIVES):
            for _, servers in archives.values():
                self.assertTrue(servers[1].startswith('http://mirror.hetzner.com/'), servers)
        self.assertEqual(set(incus.APT_ARCHIVES), {d for d, manager in incus.PACKAGE_MANAGERS.items() if manager == 'apt'})
        self.assertEqual(set(incus.DNF_PACKAGE_SERVERS), {d for d, manager in incus.PACKAGE_MANAGERS.items() if manager == 'dnf'})
        self.assertEqual(set(incus.PACKAGE_MANAGERS), set(incus.IMAGES))
        self.assertEqual(set(incus.PACKAGE_MANAGERS.values()), set(incus.PREPARE))

    def test_a_dnf_repository_gets_a_baseurl_in_place_of_its_mirrorlist(self):
        main_server, servers = incus.DNF_PACKAGE_SERVERS['rocky9']
        repositories = ('# If the mirrorlist does not work for you, you can try the commented out\n# baseurl line instead.\n\n'
                        '[baseos]\nname=Rocky Linux $releasever - BaseOS\n'
                        'mirrorlist=https://mirrors.rockylinux.org/mirrorlist?arch=$basearch&repo=BaseOS-$releasever$rltype\n'
                        '#baseurl=http://dl.rockylinux.org/$contentdir/$releasever/BaseOS/$basearch/os/\n'
                        'gpgcheck=1\nenabled=1\ngpgkey=file:///etc/pki/rpm-gpg/RPM-GPG-KEY-Rocky-9\n\n'
                        '[other]\nname=Another server\nmetalink=https://example.com/metalink\nenabled=0\n')
        text, changed = incus.dnf_repositories_with_package_servers(repositories, main_server, servers)
        self.assertEqual(changed, 1)
        self.assertEqual(text, repositories.replace(
            'mirrorlist=https://mirrors.rockylinux.org/mirrorlist?arch=$basearch&repo=BaseOS-$releasever$rltype\n'
            '#baseurl=http://dl.rockylinux.org/$contentdir/$releasever/BaseOS/$basearch/os/\n',
            'baseurl=https://dl.rockylinux.org/$contentdir/$releasever/BaseOS/$basearch/os/ '
            'https://mirror.hetzner.com/rockylinux/$releasever/BaseOS/$basearch/os/\n'))
        # AlmaLinux's debug repositories name vault.almalinux.org and keep their mirrorlist.
        main_server, servers = incus.DNF_PACKAGE_SERVERS['almalinux10']
        repositories = ('[baseos]\nmirrorlist=https://mirrors.almalinux.org/mirrorlist/$releasever/baseos\n'
                        '# baseurl=https://repo.almalinux.org/almalinux/$releasever/BaseOS/$basearch/os/\nenabled=1\n'
                        '[baseos-debuginfo]\nmirrorlist=https://mirrors.almalinux.org/mirrorlist/$releasever/baseos-debug\n'
                        '# baseurl=https://vault.almalinux.org/$releasever/BaseOS/debug/$basearch/\nenabled=0\n')
        text, changed = incus.dnf_repositories_with_package_servers(repositories, main_server, servers)
        self.assertEqual((changed, text), (1, repositories.replace(
            'mirrorlist=https://mirrors.almalinux.org/mirrorlist/$releasever/baseos\n'
            '# baseurl=https://repo.almalinux.org/almalinux/$releasever/BaseOS/$basearch/os/\n',
            'baseurl=https://repo.almalinux.org/almalinux/$releasever/BaseOS/$basearch/os/ '
            'https://mirror.hetzner.com/almalinux/$releasever/BaseOS/$basearch/os/\n')))

    def test_a_fedora_repository_gets_the_servers_of_fedora_and_hetzner(self):
        main_server, servers = incus.DNF_PACKAGE_SERVERS['fedora45']
        repositories = ('[updates]\nname=Fedora $releasever - $basearch - Updates\n'
                        '#baseurl=http://download.example/pub/fedora/linux/updates/$releasever/Everything/$basearch/\n'
                        'metalink=https://mirrors.fedoraproject.org/metalink?repo=updates-released-f$releasever&arch=$basearch\nenabled=1\n')
        text, changed = incus.dnf_repositories_with_package_servers(repositories, main_server, servers)
        self.assertEqual((changed, text), (1, '[updates]\nname=Fedora $releasever - $basearch - Updates\n'
                                           'baseurl=https://dl.fedoraproject.org/pub/fedora/linux/updates/$releasever/Everything/$basearch/ '
                                           'https://mirror.hetzner.com/fedora/updates/$releasever/Everything/$basearch/\nenabled=1\n'))

    def test_pacman_tries_the_distributions_server_first_and_hetzner_second(self):
        scripts = []

        def inside(name, script, check=True, timeout=900):
            scripts.append(script)
            return SimpleNamespace(stdout='', stderr='', returncode=0)
        with patch.object(incus, 'inside', side_effect=inside):
            incus.set_package_servers('guest', 'archlinux')
        self.assertIn("Server = https://geo.mirror.pkgbuild.com/$repo/os/$arch\nServer = http://mirror.hetzner.com/archlinux/$repo/os/$arch\n", scripts[0])
        self.assertIn('/etc/pacman.d/mirrorlist', scripts[0])
        self.assertIn('pacman -Sy', scripts[-1])

    def test_zypper_fetches_the_indexes_of_its_repositories(self):
        scripts = []

        def inside(name, script, check=True, timeout=900):
            scripts.append(script)
            return SimpleNamespace(stdout='', stderr='', returncode=0)
        with patch.object(incus, 'inside', side_effect=inside):
            incus.set_package_servers('guest', 'opensuseleap161')
        self.assertEqual(scripts, ['zypper --non-interactive refresh'])

    def test_a_guest_whose_sources_name_no_known_server_fails_to_start(self):
        def inside(name, script, check=True, timeout=900):
            return SimpleNamespace(stdout='/etc/apt/sources.list' if script.startswith('ls') else 'deb http://other.example/debian bookworm main\n',
                                   stderr='', returncode=0)
        with patch.object(incus, 'inside', side_effect=inside):
            with self.assertRaisesRegex(RuntimeError, 'no apt source of the image names'):
                incus.set_package_servers('guest', 'debian12')

    def test_a_guest_writes_its_sources_and_lists_and_fetches_the_indexes(self):
        scripts = []

        def inside(name, script, check=True, timeout=900):
            scripts.append(script)
            stdout = {'ls': '/etc/apt/sources.list.d/ubuntu.sources', 'cat': 'URIs: http://archive.ubuntu.com/ubuntu/\n'}.get(script.split()[0], '')
            return SimpleNamespace(stdout=stdout, stderr='', returncode=0)
        with patch.object(incus, 'inside', side_effect=inside):
            incus.set_package_servers('guest', 'ubuntu2404')
        written = next(script for script in scripts if script.startswith('printf'))
        self.assertIn("printf %s 'URIs: mirror+file:/etc/apt/mmx-test-package-servers/ubuntu-packages.list\n' > /etc/apt/sources.list.d/ubuntu.sources", written)
        self.assertIn('/etc/apt/mmx-test-package-servers/ubuntu-security.list', written)
        self.assertIn('apt-get -q update', scripts[-1])


def suite_names():
    return incus.suite_runner.SUITES


class PlacementTests(unittest.TestCase):
    def test_netplan_files_has_vm_placement(self):
        self.assertIn(('netplan_files.py',), incus.suite_runner.SUITES)
        for distribution in ('ubuntu2404', 'ubuntu2604'):
            self.assertTrue(incus.needs_own_kernel(('netplan_files.py',), distribution), distribution)

    def test_owed_restores_runs_in_a_vm_on_every_distribution(self):
        self.assertIn(('owed_restores.py',), incus.suite_runner.SUITES)
        for distribution in incus.IMAGES:
            self.assertTrue(incus.needs_own_kernel(('owed_restores.py',), distribution), distribution)

    def test_systemd_networkd_files_has_vm_placement(self):
        self.assertIn(('systemd_networkd_files.py',), incus.suite_runner.SUITES)
        for distribution in ('debian12', 'debian13'):
            self.assertTrue(incus.needs_own_kernel(('systemd_networkd_files.py',), distribution), distribution)

    def test_named_suites_and_distributions_exist(self):
        suites = {suite[0] for suite in incus.suite_runner.SUITES}
        self.assertLessEqual(set(incus.KERNEL_SUITES), suites)
        self.assertEqual(set(incus.IMAGES), {'debian12', 'debian13', 'ubuntu2404', 'ubuntu2604', 'rocky9', 'almalinux10',
                                             'fedora45', 'archlinux', 'opensuseleap161'})
        self.assertLessEqual(set(incus.SELINUX_DISTRIBUTIONS), set(incus.IMAGES))

    def test_every_suite_of_the_red_hat_family_runs_in_a_vm(self):
        # SELinux does not enforce in a container, so a container proves nothing about it.
        for distribution in ('rocky9', 'almalinux10', 'fedora45'):
            for suite in suite_names():
                self.assertTrue(incus.needs_own_kernel(suite, distribution), (suite, distribution))
        for distribution in ('archlinux', 'opensuseleap161'):
            self.assertFalse(incus.needs_own_kernel(('node_facts.py',), distribution), distribution)

    def test_kernel_suites_run_in_vms(self):
        for distribution in incus.IMAGES:
            if distribution in incus.SELINUX_DISTRIBUTIONS:
                continue
            for suite in ('account_units.py', 'directory_resources.py', 'docker_containers.py'):
                self.assertTrue(incus.needs_own_kernel((suite,), distribution), (suite, distribution))
            for suite in (('vm.py', 'test'), ('node_facts.py',)):
                self.assertFalse(incus.needs_own_kernel(suite, distribution), (suite, distribution))

    def test_blocked_port_and_podman_suites_run_in_containers(self):
        for suite in ('blocked_port.py', 'podman_containers.py'):
            self.assertIn((suite,), incus.suite_runner.SUITES)
            for distribution in set(incus.IMAGES) - set(incus.SELINUX_DISTRIBUTIONS):
                self.assertFalse(incus.needs_own_kernel((suite,), distribution), (suite, distribution))

    def test_the_wireguard_suite_runs_in_a_vm_where_apparmor_confines_wg_quick(self):
        self.assertIn((incus.WIREGUARD_SUITE,), incus.suite_runner.SUITES)
        for distribution in incus.IMAGES:
            self.assertEqual(incus.needs_own_kernel((incus.WIREGUARD_SUITE,), distribution),
                             distribution == 'ubuntu2604' or distribution in incus.SELINUX_DISTRIBUTIONS, distribution)

    def test_the_zfs_suite_runs_in_a_vm_where_it_installs_zfs(self):
        self.assertIn((incus.ZFS_SUITE,), incus.suite_runner.SUITES)
        self.assertLessEqual(set(incus.ZFS_DISTRIBUTIONS), set(incus.IMAGES))
        for distribution in ('debian13', 'ubuntu2404', 'ubuntu2604'):
            self.assertTrue(incus.needs_own_kernel((incus.ZFS_SUITE,), distribution), distribution)
        # Elsewhere the suite installs no ZFS and tests the module without
        # its programs, which a container can do.
        self.assertFalse(incus.needs_own_kernel((incus.ZFS_SUITE,), 'debian12'))
        # The suite itself installs ZFS on the same distributions: its
        # table has each one's os-release ID with its major versions.
        suite = (Path(__file__).with_name(incus.ZFS_SUITE)).read_text()
        self.assertIn("INSTALLS_ZFS = {'ubuntu': ('24', '26'), 'debian': ('13',), 'rocky': ('9',), 'almalinux': ('10',)}", suite)

    def test_the_partition_and_lvm_suite_runs_in_a_vm_everywhere(self):
        for distribution in incus.IMAGES:
            self.assertTrue(incus.needs_own_kernel(('partitions_and_lvm.py',), distribution), distribution)

    def test_containers_get_two_fixed_cpus_in_turn(self):
        state = SimpleNamespace(lock=threading.Lock(), next_cpu=0)
        with patch.object(incus.os, 'cpu_count', return_value=6):
            given = [incus.Run.cpus(state) for _ in range(4)]
        self.assertEqual(given, ['0,1', '2,3', '4,5', '0,1'])

    def test_a_slot_waits_for_a_place_and_gives_it_back(self):
        run = SimpleNamespace(pair_places=threading.Semaphore(1), stopping=threading.Event(), shared_slots=None)
        pending = queue.Queue()
        pending.put(('fleet.py',))
        with patch.object(incus, 'ready_targets', return_value=(Path('targets.json'), 'Debian')), \
                patch.object(incus, 'incus'), contextlib.redirect_stdout(io.StringIO()):
            with incus.guest_pair(run, 'debian13', incus.CONTAINER, 1, pending) as targets:
                self.assertEqual(targets, Path('targets.json'))
                # The only place is taken: once the suites run out, another
                # slot gives up without guests.
                pending.get()
                with incus.guest_pair(run, 'debian13', incus.CONTAINER, 2, pending) as second:
                    self.assertIsNone(second)
            self.assertTrue(run.pair_places.acquire(blocking=False))

if __name__ == '__main__':
    unittest.main()


class SharedGuestSlotsTests(unittest.TestCase):
    def test_a_run_alone_takes_every_slot_and_two_runs_share_them(self):
        import tempfile
        with tempfile.TemporaryDirectory() as directory:
            for number in range(1, 5):
                (Path(directory) / f'slot-{number}').touch()
            first, second = incus.SharedGuestSlots(directory), incus.SharedGuestSlots(directory)
            taken = [first.acquire(1, 'first', lambda: True) for _ in range(4)]
            self.assertTrue(all(taken))
            self.assertIsNone(second.acquire(1, 'second', lambda: True))
            self.assertEqual((Path(directory) / 'slot-1').read_text(), 'first\n')
            # While the second run waits, the first gives a slot back and may
            # not take it again: its share is two of the four.
            got = []

            def second_run():
                got.append(second.acquire(1, 'second', lambda: bool(got)))
            thread = threading.Thread(target=second_run)
            thread.start()
            # The second run counts from the moment it holds its file in runs/.
            started = time.monotonic()
            while first.runs_that_want_slots() < 2 and time.monotonic() - started < 30:
                time.sleep(0.01)
            self.assertEqual(first.runs_that_want_slots(), 2)
            first.release(taken.pop())
            self.assertIsNone(first.acquire(1, 'first', lambda: True))
            thread.join(timeout=30)
            self.assertTrue(got and got[0])
            # Once the second run holds nothing and wants nothing, the first
            # takes every slot again.
            second.release(got[0])
            self.assertTrue(first.acquire(1, 'first', lambda: True))
            for held in taken:
                first.release(held)

    def test_two_runs_that_find_a_base_missing_prepare_it_once(self):
        with tempfile.TemporaryDirectory() as directory:
            (Path(directory) / 'slot-1').touch()
            prepared = []

            def info_of(*args, check=True):
                return SimpleNamespace(returncode=0 if prepared else 1)

            def prepare(distribution, kind):
                # Long enough that the second run looks while the first prepares.
                time.sleep(0.3)
                prepared.append((distribution, kind))
            with patch.object(incus.vm, 'ROOT', Path(directory)), patch.object(incus, 'incus', side_effect=info_of), \
                    patch.object(incus, 'prepare_base', side_effect=prepare), contextlib.redirect_stdout(io.StringIO()):
                runs = [incus.Run(1, incus.SharedGuestSlots(directory)) for _ in range(2)]
                threads = [threading.Thread(target=run.base, args=('debian13', incus.CONTAINER)) for run in runs]
                for thread in threads:
                    thread.start()
                for thread in threads:
                    thread.join(timeout=30)
            self.assertEqual(prepared, [('debian13', incus.CONTAINER)])
