#!/usr/bin/env python3
"""The catalog of VM workflow suites and the job slots that run them, for
incus.py and hetzner.py. Each slot runs suites on the test machines that its
caller creates for it."""
from collections import deque
from concurrent.futures import ThreadPoolExecutor
from pathlib import Path
from queue import Empty, Queue
import shutil
import subprocess
import sys
import threading
import time

import testenv

DIRECTORY = Path(__file__).resolve().parent
# Longest expected first; short suites fill slots as they become available.
SUITES = (
    ('subscription_web_service_example.py',),
    ('vm.py', 'test'), ('linux_code.py',), ('fleet.py',), ('node_facts.py',),
    ('owed_restores.py',), ('systemd_networkd_files.py',),
    ('networkmanager_connection_files.py',),
    ('netplan_files.py',),
    ('firewalld_zone_memberships.py',),
    ('ifupdown_interfaces_files.py',),
    ('server_resources.py',), ('access.py',), ('limited_control.py',),
    ('task_secrets.py',), ('external_modules.py',), ('deployment.py',),
    ('account_units.py',), ('file_resources.py',), ('streamed_assets.py',),
    ('directory_resources.py',), ('directory_copy.py',), ('downloads.py',),
    ('release_directories.py',), ('diagnostics.py',), ('node_history.py',),
    ('inspection.py',), ('module_helper.py',), ('linux_controller.py',),
    ('controller_machine_as_node.py',),
    ('selinux_labels.py',), ('blocked_port.py',),
    ('access_control_lists_and_capabilities.py',),
    ('firewalld_and_selinux.py',),
    ('ssh_key_pairs.py',),
    ('wireguard_interfaces.py',),
    ('node_settings.py',),
    ('scheduled_jobs.py',),
    ('kernel_settings.py',),
    ('storage.py',),
    ('partitions_and_lvm.py',),
    ('zfs_datasets.py',),
    ('http_requests.py',),
    ('dns_tls_and_account_lookups.py',),
    ('language_installers.py',),
    ('snap_packages.py',),
    ('docker_containers.py',),
    ('podman_containers.py',),
    ('postgresql_databases.py',),
    ('mysql_databases.py',),
    ('mongodb_users.py',),
    ('pg_auto_failover.py',),
    ('pgbackrest.py',),
    ('rabbitmq.py',),
    ('certbot_certificates.py',),
    ('pacman_packages.py',),
    ('zypper_packages.py',),
    ('one_time_playbooks.py',),
    ('routine_fixes.py',),
    ('incus_instances.py',),
    ('libvirt_domains.py',),
)
# The base suite: bootstrap, both connections (SSH and HTTPS), recovery
# (owed restores and resume), and the plain-named modules, whose mechanism
# is the same wherever they run. A suite in it
# installs nothing with a package module: it prepares its node with the
# harness (vm.install_packages) or needs no package at all. Before a release
# the base suite runs on every tested distribution (--base-suite). It
# replaces the former platform subset: one set, one job.
BASE_SUITE = (
    ('vm.py', 'test'), ('node_facts.py',), ('fleet.py',), ('owed_restores.py',),
    ('access.py',), ('limited_control.py',), ('task_secrets.py',), ('external_modules.py',),
    ('linux_code.py',), ('linux_controller.py',), ('controller_machine_as_node.py',),
    ('file_resources.py',), ('directory_resources.py',), ('directory_copy.py',),
    ('streamed_assets.py',), ('account_units.py',), ('downloads.py',),
    ('node_history.py',), ('inspection.py',), ('one_time_playbooks.py',),
    ('module_helper.py',), ('http_requests.py',), ('dns_tls_and_account_lookups.py',),
    ('deployment.py',), ('release_directories.py',),
)


def selected_suites(suite_names, base_suite):
    """The suites of SUITES that a command line selects: those named with
    --suite, and the base suite with --base-suite; every suite when neither
    is given."""
    wanted = set(suite_names or ())
    if base_suite:
        wanted |= {' '.join(suite) for suite in BASE_SUITE}
    return tuple(suite for suite in SUITES if not wanted or ' '.join(suite) in wanted)


# The distributions of the Debian family, where the harness's tool-module
# suites run unless SUITE_DISTRIBUTIONS says otherwise.
DEBIAN_FAMILY_DISTRIBUTIONS = ('debian12', 'debian13', 'ubuntu2404', 'ubuntu2604')
# Suites that run only on some distributions, and why. A suite of the base
# suite runs on every distribution of a run. Any other suite is a tool
# module's suite: by default it runs on the distributions that its module's page
# lists under "Tested on". Those are the Debian family's unless an entry here
# names others; a family's own suites, such as those for
# dnf_package, name that family's distributions here.
RED_HAT_FAMILY_DISTRIBUTIONS = ('rocky9', 'almalinux10')
DEBIAN_AND_RED_HAT_FAMILY_DISTRIBUTIONS = DEBIAN_FAMILY_DISTRIBUTIONS + RED_HAT_FAMILY_DISTRIBUTIONS
SUITE_DISTRIBUTIONS = {
    # The example is written and tested for Ubuntu 24.04. Its roles use
    # Ubuntu's package names, such as mysql-server and libmysqlclient-dev,
    # which Debian and the Red Hat family do not have, and outside apt
    # repositories: Phusion's for Passenger, whose suite is the release's
    # code name, and NodeSource's for Node.js.
    'subscription_web_service_example.py': ('ubuntu2404',),
    # The Red Hat family's own modules: dnf_package and dnf_repository are
    # tested by server_resources.py, and the SELinux, firewalld, and
    # NetworkManager modules by their own suites.
    'networkmanager_connection_files.py': RED_HAT_FAMILY_DISTRIBUTIONS,
    'firewalld_zone_memberships.py': RED_HAT_FAMILY_DISTRIBUTIONS,
    'selinux_labels.py': RED_HAT_FAMILY_DISTRIBUTIONS,
    'firewalld_and_selinux.py': DEBIAN_AND_RED_HAT_FAMILY_DISTRIBUTIONS,
    # pacman_package's suite: pacman is the package manager of Arch Linux.
    'pacman_packages.py': ('archlinux',),
    # zypper_package's and zypper_repository's suite: zypper is the package
    # manager of openSUSE.
    'zypper_packages.py': ('opensuseleap161',),
}
# Tool-module suites that test their modules on the Debian family and on the
# Red Hat family alike.
SUITE_DISTRIBUTIONS.update({name: DEBIAN_AND_RED_HAT_FAMILY_DISTRIBUTIONS for name in (
    'server_resources.py', 'blocked_port.py', 'access_control_lists_and_capabilities.py', 'ssh_key_pairs.py',
    'node_settings.py', 'scheduled_jobs.py', 'kernel_settings.py', 'storage.py', 'partitions_and_lvm.py',
    'zfs_datasets.py', 'language_installers.py', 'snap_packages.py', 'docker_containers.py',
    'podman_containers.py', 'postgresql_databases.py', 'mysql_databases.py', 'mongodb_users.py',
    'rabbitmq.py', 'incus_instances.py', 'libvirt_domains.py')})
# Fedora runs the suites of the Red Hat family that passed on it with dnf 5;
# see "Fedora" in docs/changing-mmx/validation.md. Not the two NetworkManager suites
# (networkmanager_connection_files.py, firewalld_zone_memberships.py): Fedora's
# cloud image has no NetworkManager, and they need one that manages the guest's cards.
FEDORA_DISTRIBUTIONS = ('fedora45',)
for _suite in ('server_resources.py', 'selinux_labels.py', 'firewalld_and_selinux.py', 'blocked_port.py'):
    SUITE_DISTRIBUTIONS[_suite] += FEDORA_DISTRIBUTIONS
# certbot comes from EPEL on the Red Hat family. EPEL 10's certbot does not
# install on AlmaLinux 10: its python3-pyOpenSSL needs a newer cryptography
# than the AlmaLinux 10 repositories have. The suite passes on Rocky Linux 9.
SUITE_DISTRIBUTIONS['certbot_certificates.py'] = DEBIAN_FAMILY_DISTRIBUTIONS + ('rocky9',)
# Suites that leave their test machines unfit for the suites after them,
# and why. The slot deletes its machines after such a suite and takes new
# ones for its next suite.
SUITES_AFTER_WHICH_A_SLOT_TAKES_NEW_TEST_MACHINES = {
    'subscription_web_service_example.py': "the example's firewall drops every forwarded packet, which containers "
                                           "and virtual machines need, and its MySQL, Solr, ClickHouse, and Redis "
                                           "servers keep most of the memory",
}

# Suites that need a third test machine, mmx-third, beside the slot's
# mmx-dev and mmx-peer, and why. The slot creates it before the suite and
# deletes it after, so other suites hold no idle machine.
SUITES_WITH_A_THIRD_TEST_MACHINE = {
    'pg_auto_failover.py': "a pg_auto_failover monitor and two database servers each need a server of their own: "
                           "pg_autoctl's unit runs one instance on a server",
    'pgbackrest.py': "a database server is restored onto another server than the one that was lost, and both and the "
                     "pg_auto_failover monitor each need a server of their own: pg_autoctl's unit runs one instance on a server",
}


def distributions_of(suite):
    """The distributions that a suite (a tuple of SUITES) is limited to, or
    None for a suite of the base suite that has no limit."""
    if suite[0] in SUITE_DISTRIBUTIONS:
        return SUITE_DISTRIBUTIONS[suite[0]]
    return None if suite in BASE_SUITE else DEBIAN_FAMILY_DISTRIBUTIONS


def runs_on_distribution(suite, distribution):
    """Whether a suite (a tuple of SUITES) runs on a distribution."""
    limit = distributions_of(suite)
    return limit is None or distribution in limit


def suites_by_distribution(suites, distributions):
    """The suites that run on each distribution, for the distributions that
    have any. Print each selected suite that some of the distributions
    leave out; end the program with a message when none of the suites runs
    on any of them."""
    for suite in suites:
        left_out = [d for d in distributions if not runs_on_distribution(suite, d)]
        if left_out:
            print(f'{suite[0]} runs only on {", ".join(distributions_of(suite))} (BASE_SUITE and SUITE_DISTRIBUTIONS in '
                  f'tests/integration/all.py); this run leaves it out on {", ".join(left_out)}', flush=True)
    selected = {}
    for distribution in distributions:
        if chosen := tuple(s for s in suites if runs_on_distribution(s, distribution)):
            selected[distribution] = chosen
    if not selected:
        raise SystemExit(f'Nothing to run: none of the selected suites runs on {", ".join(distributions)}. '
                         'Each line above names the distributions where its suite runs.')
    return selected


# One lock for every runner in this process, so that the failure output of
# one suite is never interleaved with another's.
OUTPUT_LOCK = threading.Lock()


def run_command(arguments, environment, log):
    return subprocess.run([sys.executable, '-u', str(DIRECTORY / arguments[0]),
                           *arguments[1:]], env=environment, stdout=log,
                          stderr=subprocess.STDOUT).returncode


def show_failure(log_path):
    with log_path.open(errors='replace') as log:
        tail = ''.join(deque(log, maxlen=30))
    print(f'Log: {log_path}\n{tail}', flush=True)


def run_all(root, jobs, *, cloud_servers, third_machine=None, suites=SUITES, label='', shared_build=None):
    """Run suites in job slots. cloud_servers(slot, pending) is a context
    manager that gives the slot the path of its test machines'
    MMX_TEST_TARGETS file, or None when the suites ran out while it waited
    for machines, and deletes the machines when the slot is done.
    third_machine(slot, targets) is a context manager that adds mmx-third to
    that file for one suite of SUITES_WITH_A_THIRD_TEST_MACHINE, gives True,
    or False when it has no machine, and deletes the machine afterwards.
    label starts each printed line.

    The slots build MMX and the Go test programs once, in shared_build: a
    directory the caller made empty for this run, or else a new root/build."""
    if shared_build is None:
        shared_build = root / 'build'
        shutil.rmtree(shared_build, ignore_errors=True)
    pending = Queue()
    for suite in suites:
        pending.put(suite)
    started = time.monotonic()

    def worker(slot):
        slot_root = root / 'slots' / str(slot)
        logs = slot_root / 'logs'
        logs.mkdir(parents=True, exist_ok=True)
        slot_environment = testenv.environment(MMX_TEST_ROOT=str(slot_root))
        slot_environment[testenv.SHARED_BUILD] = str(shared_build)
        slot_environment.pop(testenv.LOCK_FD, None)
        results = []
        while True:
            # A slot takes its first suite only once its test machines are ready.
            with cloud_servers(slot, pending) as targets:
                if not targets:
                    return results
                slot_environment['MMX_TEST_TARGETS'] = str(targets)
                suite = next_suite()
                while suite:
                    passed = run_suite(slot, suite, slot_environment, logs)
                    results.append(passed)
                    # A slot whose test machines no longer answer would fail
                    # every later suite; the other slots take them instead.
                    if not passed and not servers_answer(slot, slot_environment, logs):
                        return results
                    if suite[0] in SUITES_AFTER_WHICH_A_SLOT_TAKES_NEW_TEST_MACHINES:
                        break
                    suite = next_suite()
            # A suite that left the machines unfit ended them; a suite still
            # in the queue gets new ones.
            if not suite or pending.empty():
                return results

    def next_suite():
        try:
            return pending.get_nowait()
        except Empty:
            return None

    def run_suite(slot, suite, environment, logs):
        name = ' '.join(suite)
        log_path = logs / (Path(suite[0]).stem + '.log')
        suite_started = time.monotonic()
        with log_path.open('w') as log:
            try:
                if suite[0] in SUITES_WITH_A_THIRD_TEST_MACHINE:
                    if third_machine is None:
                        print(f'{suite[0]} needs a third test machine, which this harness does not make', file=log)
                        passed = False
                    else:
                        with third_machine(slot, Path(environment['MMX_TEST_TARGETS'])) as made:
                            if not made:
                                print(f'{suite[0]} got no third test machine; the harness printed why', file=log)
                            passed = made and run_command(suite, environment, log) == 0
                else:
                    passed = run_command(suite, environment, log) == 0
            except OSError as error:
                print(error, file=log)
                passed = False
        elapsed = time.monotonic() - suite_started
        with OUTPUT_LOCK:
            print(f'{label}{name}: slot {slot} {"PASS" if passed else "FAIL"} {elapsed:.1f}s', flush=True)
            if not passed:
                show_failure(log_path)
        return passed

    def servers_answer(slot, environment, logs):
        with (logs / 'server-check.log').open('w') as log:
            for name in ('mmx-dev', 'mmx-peer'):
                if run_command(('vm.py', 'up', '--vm', name), environment, log) != 0:
                    with OUTPUT_LOCK:
                        print(f'{label}slot {slot}: {name} does not answer; the slot stops', flush=True)
                    return False
        return True

    slots = range(1, min(jobs, len(suites)) + 1)
    with ThreadPoolExecutor(max_workers=jobs) as workers:
        slot_results = list(workers.map(worker, slots))
    results = [passed for suites_passed in slot_results for passed in suites_passed]
    failed = results.count(False)
    # Suites are left over only when no slot got test machines to run them.
    not_run = len(suites) - len(results)
    print(f'{label}Summary: {results.count(True)} passed, {failed} failed; total wall time {time.monotonic()-started:.1f}s; '
          f'{not_run} not run', flush=True)
    return int(failed != 0 or not_run != 0)


if __name__ == '__main__':
    sys.exit('all.py makes no test machines of its own. Run its catalog of VM suites with '
             'tests/integration/incus.py or tests/integration/hetzner.py, which create the test machines.')
