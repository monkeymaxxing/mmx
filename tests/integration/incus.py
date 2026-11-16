#!/usr/bin/env python3
"""Run the VM suites on Incus system containers and VMs on this machine.

A system container boots a distribution's own systemd, accounts, network,
and packages, and shares this machine's kernel. Each distribution's suites
run in containers, except those that need a guest's own kernel
(needs_own_kernel), which run in Incus VMs; with --kernel-suites-on-hetzner
they run on Hetzner Cloud servers instead.

Every guest is a copy of a prepared base of its distribution and kind, made
on first use from an image copied once into Incus's local store; a run
never asks the image server. Each job slot gets two new copies, mmx-dev and
mmx-peer, and deletes them when the slot is done; a suite of all.py's
SUITES_WITH_A_THIRD_TEST_MACHINE also gets mmx-third while it runs. The
ownership manifest supports cleanup even after this process dies.
"""
import argparse
import datetime
from concurrent.futures import ThreadPoolExecutor
from contextlib import contextmanager
import fcntl
import importlib
import json
import os
from pathlib import Path
import random
import re
import shlex
import signal
import subprocess
import threading
import time
import traceback
import uuid

import hetzner
import testenv
import vm

# all.py's name would hide Python's all() here.
suite_runner = importlib.import_module('all')

# The Incus image of each distribution: the cloud variant, which, like a
# cloud server, has cloud-init. Arch Linux is the current rolling release;
# openSUSE Leap is the newest release that Incus's image server has.
IMAGES = {
    'debian12': 'debian/12/cloud', 'debian13': 'debian/13/cloud',
    'ubuntu2404': 'ubuntu/noble/cloud', 'ubuntu2604': 'ubuntu/resolute/cloud',
    'rocky9': 'rockylinux/9/cloud', 'almalinux10': 'almalinux/10/cloud',
    'archlinux': 'archlinux/cloud', 'opensuseleap161': 'opensuse/16.1/cloud',
    'fedora45': 'fedora/45/cloud',
}
CONTAINER, VIRTUAL_MACHINE = 'container', 'vm'
# An image whose VM Incus refuses to start with Secure Boot on; its base is
# made with Secure Boot off, as every copy is (see start_guest).
IMAGES_INCOMPATIBLE_WITH_SECURE_BOOT = ('archlinux',)

# The package manager of each distribution's image; it picks the guest's
# preparation and package servers.
PACKAGE_MANAGERS = {
    'debian12': 'apt', 'debian13': 'apt', 'ubuntu2404': 'apt', 'ubuntu2604': 'apt',
    'rocky9': 'dnf', 'almalinux10': 'dnf', 'fedora45': 'dnf', 'archlinux': 'pacman', 'opensuseleap161': 'zypper',
}

# Suites that need the guest's own kernel, and why. A container shares this
# machine's kernel.
KERNEL_SUITES = {
    'subscription_web_service_example.py': "the example sets kernel settings, a swap file, and time synchronization, "
                                           "which belong to the guest's own kernel and clock",
    'networkmanager_connection_files.py': 'real NetworkManager changes need a second NIC, independent Incus observations, a reboot, and SELinux',
    'netplan_files.py': 'real Netplan changes need a second card, independent Incus observations, and a reboot',
    'firewalld_zone_memberships.py': 'firewalld recovery needs a second card, real firewall isolation, a node restart, and independent Incus observations',
    'systemd_networkd_files.py': 'real networkd changes need a second NIC, independent Incus observations, and a reboot',
    'ifupdown_interfaces_files.py': 'real ifupdown changes need a guest switched to ifupdown, a second card, independent Incus observations, and a reboot',
    'owed_restores.py': 'owed restore recovery needs a real node reboot and systemd process groups',
    'account_units.py': 'its automount unit needs autofs, which a container cannot use',
    'directory_resources.py': 'chattr +i needs a capability that an unprivileged container lacks',
    'kernel_settings.py': 'kernel settings and kernel modules belong to the kernel, which a container shares with this machine',
    'storage.py': 'swap, mkfs, and mounts need loop devices and the kernel of the guest itself',
    'partitions_and_lvm.py': 'loop devices with partitions and LVM\'s logical volumes need the kernel of the guest itself',
    'docker_containers.py': "in a Debian or Ubuntu container Docker's daemon cannot read AppArmor's profiles and starts no container",
    'incus_instances.py': 'a server that runs Incus has its own kernel, and a virtual machine instance needs /dev/kvm; '
                          'in a container guest Incus starts an instance only with changes that no real server needs',
    'libvirt_domains.py': "its domains are virtual machines, which need the guest's own /dev/kvm; a container has none",
}
# SELinux does not enforce in an Incus container, which has none of its own.
# A real server of the Red Hat family enforces it, so every guest of these
# distributions is a VM, and every suite runs with the labels and the policy
# of such a server.
SELINUX_DISTRIBUTIONS = ('rocky9', 'almalinux10', 'fedora45')

# snapd mounts each snap as a squashfs image and confines it with AppArmor or
# its own seccomp rules, which an unprivileged container cannot do. It runs
# where the distribution packages it; elsewhere the suite passes in a
# container without snapd.
SNAP_SUITE = 'snap_packages.py'
SNAPD_DISTRIBUTIONS = ('debian12', 'debian13', 'ubuntu2404', 'ubuntu2604')

# Ubuntu 26.04 confines wg-quick with an AppArmor profile, which an
# unprivileged container cannot enforce, as for snapd above. The WireGuard
# suite checks that profile, so on Ubuntu 26.04 it runs in a VM.
WIREGUARD_SUITE = 'wireguard_interfaces.py'
WIREGUARD_APPARMOR_DISTRIBUTIONS = ('ubuntu2604',)

# ZFS is a kernel module, which a container cannot load. The ZFS suite
# installs ZFS where that takes under two minutes, and on Debian 13 with its
# build of the module; there it runs in a VM. Elsewhere it installs no ZFS,
# tests the module without its programs, and passes in a container.
ZFS_SUITE = 'zfs_datasets.py'
ZFS_DISTRIBUTIONS = ('debian13', 'ubuntu2404', 'ubuntu2604', 'rocky9', 'almalinux10')

# Each guest's package servers, tried in order: the distribution's own
# server first, then Hetzner's mirror when it fails. The base gets the lists
# and the package indexes fetched from them, so a copy starts without that work.
#
# apt (Debian and Ubuntu): an archive of the image's sources is named by its
# URI. Each such URI becomes apt's mirror method, mirror+file:LIST, whose
# list names the servers with a priority, lowest first; apt takes a file
# from the next server when one fails. The suites and components stay as
# the image has them, in a one-line sources.list and in deb822 .sources
# files alike, because only the URI changes. The security archive has its
# own list: its servers are others. Plain HTTP, as the distributions' own
# entries have it: apt checks the signed Release files, and a minimal image
# may have no certificate authorities for HTTPS.
APT_PACKAGE_SERVER_LISTS = '/etc/apt/mmx-test-package-servers'
DEBIAN_ARCHIVES = {
    # The archive's name, with the URIs that an image may give it, without
    # their scheme, and its servers in the order apt tries them.
    'debian-packages': (('deb.debian.org/debian', 'ftp.debian.org/debian'),
                        ('http://deb.debian.org/debian', 'http://mirror.hetzner.com/debian/packages')),
    'debian-security': (('deb.debian.org/debian-security', 'security.debian.org/debian-security', 'security.debian.org'),
                        ('http://deb.debian.org/debian-security', 'http://mirror.hetzner.com/debian/security')),
}
UBUNTU_ARCHIVES = {
    'ubuntu-packages': (('archive.ubuntu.com/ubuntu',),
                        ('http://archive.ubuntu.com/ubuntu', 'http://mirror.hetzner.com/ubuntu/packages')),
    'ubuntu-security': (('security.ubuntu.com/ubuntu',),
                        ('http://security.ubuntu.com/ubuntu', 'http://mirror.hetzner.com/ubuntu/security')),
}
APT_ARCHIVES = {'debian12': DEBIAN_ARCHIVES, 'debian13': DEBIAN_ARCHIVES,
                'ubuntu2404': UBUNTU_ARCHIVES, 'ubuntu2604': UBUNTU_ARCHIVES}
# dnf (Rocky Linux and AlmaLinux): each repository of the distribution's
# files names its servers with a mirrorlist, and its main server in a
# commented-out baseurl. Such a repository gets a baseurl with the main
# server and then Hetzner's copy, in place of the mirrorlist; dnf takes a
# file from the next URL when one fails. Everything else, such as enabled,
# gpgcheck, and gpgkey, stays. Hetzner's copies carry the advisories
# (updateinfo) that only_security_updates needs. A repository whose
# commented-out baseurl names another server, such as AlmaLinux's debug and
# source repositories on vault.almalinux.org, keeps its mirrorlist.
DNF_PACKAGE_SERVERS = {
    # The start of the commented-out baseurl, and what replaces it, in order.
    'rocky9': ('http://dl.rockylinux.org/$contentdir/',
               ('https://dl.rockylinux.org/$contentdir/', 'https://mirror.hetzner.com/rockylinux/')),
    'almalinux10': ('https://repo.almalinux.org/almalinux/',
                    ('https://repo.almalinux.org/almalinux/', 'https://mirror.hetzner.com/almalinux/')),
    # Fedora's repositories name their servers with a metalink, and their
    # main server in a commented-out baseurl on the placeholder host
    # download.example. Hetzner's copy has no "linux" directory.
    'fedora45': ('http://download.example/pub/fedora/linux/',
                 ('https://dl.fedoraproject.org/pub/fedora/linux/', 'https://mirror.hetzner.com/fedora/')),
}

# pacman (Arch Linux): /etc/pacman.d/mirrorlist names the servers in the
# order pacman tries them, and pacman takes a file from the next server when
# one fails. Arch Linux is a rolling release: the guest's package databases
# and packages must come from one moment, so a base upgrades all of its
# packages (pacman -Syu) and never installs from a database alone.
PACMAN_PACKAGE_SERVERS = ('https://geo.mirror.pkgbuild.com/$repo/os/$arch',
                          'http://mirror.hetzner.com/archlinux/$repo/os/$arch')

# zypper (openSUSE Leap): the image's repositories name download.opensuse.org,
# whose redirector picks a current mirror for each file. Hetzner's mirror
# has no copy of the newest Leap release, so the guest has no second server.

# The images of the Red Hat family's releases 9 and 10 carry a udev rule
# file that no package owns: it makes NetworkManager manage every veth
# device, against the rule of the distribution's own NetworkManager package,
# which leaves them alone. NetworkManager then took the veth device of a new
# Podman container for an Ethernet device of its own, started DHCP on it,
# and took it off the container bridge. A guest's own device keeps its
# address without the file, also after a reboot.
IMAGE_UDEV_RULE_FOR_VETH_DEVICES = '/etc/udev/rules.d/86-nm-unmanaged.rules'
REMOVE_IMAGE_UDEV_RULE_FOR_VETH_DEVICES = (
    f'if [ -e {IMAGE_UDEV_RULE_FOR_VETH_DEVICES} ]; then rm -f {IMAGE_UDEV_RULE_FOR_VETH_DEVICES}; udevadm control --reload; fi')

# The account the harness logs in as: not root, with sudo.
LOGIN = 'mmx-test'
# A container like a Hetzner cx23: two CPUs and 4 GiB. A VM gets two CPUs
# and 2 GiB, because its memory stays taken.
CONTAINER_SETTINGS = ['-c', 'limits.cpu=2', '-c', 'limits.memory=4GiB', '-c', 'security.nesting=true']
VM_SETTINGS = ['--vm', '-c', 'limits.cpu=2', '-c', 'limits.memory=2GiB']
VM_ROOT_DISK_SIZE = '12GiB'

PREPARE = {
    'apt': 'export DEBIAN_FRONTEND=noninteractive; apt-get update -q && apt-get install -yq openssh-server sudo',
    # dnf leaves a package mirror that stalls after 10 seconds, so that the
    # mirror does not hold up the guest's preparation. The Red Hat
    # family images replace sshd_config with one line, which drops its
    # Include of sshd_config.d and with it the distribution's UsePAM yes; a
    # reinstall restores the package's file.
    'dnf': "grep -q '^minrate=' /etc/dnf/dnf.conf || printf 'minrate=200k\\ntimeout=10\\n' >> /etc/dnf/dnf.conf; "
           'dnf install -yq openssh-server sudo && systemctl enable sshd && '
           'if rpm -V openssh-server | grep -q "/etc/ssh/sshd_config$"; then '
           'rm -f /etc/ssh/sshd_config && dnf reinstall -yq openssh-server; fi',
    # The whole system is upgraded in the same transaction: see PACMAN_PACKAGE_SERVERS.
    'pacman': 'pacman -Syu --noconfirm --needed openssh sudo && systemctl enable sshd',
    'zypper': 'zypper --non-interactive refresh && zypper --non-interactive install openssh sudo && systemctl enable sshd',
}
# With SELinux enforcing, the Incus agent in a VM, which the harness uses to
# prepare each copy, runs as init_t from its configuration disk and may not
# open its vsock socket or make its socket in /dev/incus.
INCUS_AGENT_POLICY = ('(allow init_t self (vsock_socket (create bind listen accept connect getattr setattr '
                      'getopt setopt read write shutdown)))\n'
                      '(allow init_t device_t (dir (search write add_name remove_name)))\n'
                      '(allow init_t device_t (sock_file (create getattr setattr write unlink)))\n')


def needs_own_kernel(suite, distribution):
    """Whether a suite (all.py's tuple) needs a VM on this distribution."""
    return (distribution in SELINUX_DISTRIBUTIONS or suite[0] in KERNEL_SUITES
            or (suite[0] == SNAP_SUITE and distribution in SNAPD_DISTRIBUTIONS)
            or (suite[0] == ZFS_SUITE and distribution in ZFS_DISTRIBUTIONS)
            or (suite[0] == WIREGUARD_SUITE and distribution in WIREGUARD_APPARMOR_DISTRIBUTIONS))


def apt_sources_with_package_server_lists(text, archives):
    """The text of an apt sources file, with each URI of an archive replaced
    by its list of package servers; and the names of the archives that it
    uses."""
    used = set()
    for archive, (uris, _) in archives.items():
        # The URI as a whole word, with or without its trailing slash: the
        # URI of Debian's packages must not match that of its security
        # archive, which starts with it.
        pattern = r'(?<!\S)https?://(?:' + '|'.join(re.escape(uri) for uri in sorted(uris, key=len, reverse=True)) + r')/?(?!\S)'
        text, count = re.subn(pattern, f'mirror+file:{APT_PACKAGE_SERVER_LISTS}/{archive}.list', text)
        if count:
            used.add(archive)
    return text, used


def apt_package_server_list(servers):
    """apt's mirror list: one server a line, with its priority after a tab."""
    return ''.join(f'{server}\tpriority:{number}\n' for number, server in enumerate(servers, 1))


def dnf_repositories_with_package_servers(text, main_server, servers):
    """The text of a dnf .repo file in which each repository whose
    commented-out baseurl starts with main_server gets a baseurl with the
    servers in order, in place of its mirrorlist or metalink; and how many
    repositories changed."""
    sections = re.split(r'(?m)^(?=\[)', text)
    changed = 0
    for index, section in enumerate(sections):
        lines = section.splitlines(keepends=True)
        commented = [line for line in lines if re.match(r'#\s*baseurl=' + re.escape(main_server), line)]
        if len(commented) != 1:
            continue
        path = re.sub(r'#\s*baseurl=' + re.escape(main_server), '', commented[0]).strip()
        baseurl = 'baseurl=' + ' '.join(server + path for server in servers) + '\n'
        sections[index] = ''.join(baseurl if line is commented[0] else line for line in lines
                                  if not line.startswith(('mirrorlist=', 'metalink=')))
        changed += 1
    return ''.join(sections), changed


def write_files(name, files):
    """Write each path's text into the guest, in one command."""
    inside(name, '\n'.join(f'printf %s {shlex.quote(text)} > {shlex.quote(path)}' for path, text in files.items()))


def set_package_servers(name, distribution):
    """Give a new base its ordered package servers, and fetch the package
    indexes from them. The new sources name other files of package indexes,
    which the base does not have yet, and a dryrun of a package module reads
    the indexes that the node has; the fetch also shows at once that the
    servers answer."""
    manager = PACKAGE_MANAGERS[distribution]
    if manager == 'apt':
        archives = APT_ARCHIVES[distribution]
        paths = inside(name, 'ls -1 /etc/apt/sources.list /etc/apt/sources.list.d/*.list /etc/apt/sources.list.d/*.sources 2>/dev/null',
                       check=False).stdout.split()
        files, used = {}, set()
        for path in paths:
            text, archives_of_the_file = apt_sources_with_package_server_lists(inside(name, 'cat ' + shlex.quote(path)).stdout, archives)
            if archives_of_the_file:
                files[path] = text
                used |= archives_of_the_file
        if not used:
            raise RuntimeError(f'{name}: no apt source of the image names an archive of {sorted(archives)}: {paths}')
        inside(name, 'mkdir -p ' + APT_PACKAGE_SERVER_LISTS)
        write_files(name, {**files, **{f'{APT_PACKAGE_SERVER_LISTS}/{archive}.list': apt_package_server_list(servers)
                                       for archive, (_, servers) in archives.items()}})
        # apt's daily timer and cloud-init may hold the lock on the lists
        # just after the start; apt-get update fails at once then.
        fetched = inside(name, 'for attempt in $(seq 1 30); do '
                         'if apt-get -q update > /tmp/mmx-test-apt-update 2>&1; then exit 0; fi; '
                         "grep -q 'Could not get lock' /tmp/mmx-test-apt-update || break; sleep 2; done; "
                         'cat /tmp/mmx-test-apt-update; exit 1', check=False, timeout=600)
    elif manager == 'dnf':
        main_server, servers = DNF_PACKAGE_SERVERS[distribution]
        files, changed = {}, 0
        # Fedora (dnf 5) keeps its repository files in /usr/share/dnf5/repos.d;
        # a copy in /etc/yum.repos.d replaces the one of the same name.
        inside(name, 'if [ -d /usr/share/dnf5/repos.d ]; then cp -n /usr/share/dnf5/repos.d/*.repo /etc/yum.repos.d/; fi')
        for path in inside(name, 'ls -1 /etc/yum.repos.d/*.repo').stdout.split():
            text, changed_in_the_file = dnf_repositories_with_package_servers(inside(name, 'cat ' + shlex.quote(path)).stdout, main_server, servers)
            if changed_in_the_file:
                files[path] = text
                changed += changed_in_the_file
        if not changed:
            raise RuntimeError(f'{name}: no repository of the image names {main_server} in a commented-out baseurl')
        write_files(name, files)
        fetched = inside(name, 'dnf -q makecache', check=False, timeout=600)
    elif manager == 'pacman':
        write_files(name, {'/etc/pacman.d/mirrorlist': ''.join(f'Server = {server}\n' for server in PACMAN_PACKAGE_SERVERS)})
        # The databases are fetched by the base's pacman -Syu, which also
        # shows that the servers answer.
        fetched = inside(name, 'pacman -Sy --noconfirm', check=False, timeout=600)
    else:
        fetched = inside(name, 'zypper --non-interactive refresh', check=False, timeout=600)
    if fetched.returncode:
        raise RuntimeError(f'{name}: the package indexes could not be fetched: {(fetched.stdout + fetched.stderr).strip()}')


def selinux_enforcing(name):
    """The Red Hat family's VM images have SELinux off and unlabeled files;
    a default installation enforces it."""
    inside(name, 'dnf install -yq selinux-policy-targeted policycoreutils >/dev/null; '
           f"printf '%s' '{INCUS_AGENT_POLICY}' > /root/mmx_test_policy.cil && semodule -i /root/mmx_test_policy.cil; "
           "sed -i 's/^SELINUX=.*/SELINUX=permissive/' /etc/selinux/config; touch /.autorelabel")
    # One boot in permissive mode labels the files and boots again; booting
    # enforcing on unlabeled files leaves systemd in kernel_t.
    incus('restart', name)
    wait_running(name, seconds=600)
    inside(name, "sed -i 's/^SELINUX=.*/SELINUX=enforcing/' /etc/selinux/config")
    incus('restart', name)
    wait_running(name)
    if inside(name, 'getenforce').stdout.strip() != 'Enforcing':
        raise RuntimeError(name + ': SELinux is not enforcing')


def incus(*args, check=True, timeout=600):
    # incus launch and incus init read an instance configuration from a
    # standard input that is not a terminal, and wait for its end: from a
    # pipe that stays open, such as an SSH session's, they waited forever.
    p = subprocess.run(['incus', *map(str, args)], text=True, timeout=timeout, stdin=subprocess.DEVNULL,
                       stdout=subprocess.PIPE, stderr=subprocess.PIPE)
    if check and p.returncode:
        raise RuntimeError(f'incus {" ".join(map(str, args))}: {(p.stderr or p.stdout).strip()}')
    return p


def inside(name, script, check=True, timeout=900):
    return incus('exec', name, '--', '/bin/sh', '-c', script, check=check, timeout=timeout)


def wait_running(name, seconds=180):
    """Wait until systemd in the guest has finished starting; fail unless every
    unit started."""
    deadline = time.monotonic() + seconds
    while True:
        # Right after the start, the guest (or a VM's agent) does not answer
        # yet, or systemd reports "offline".
        state = inside(name, 'systemctl is-system-running --wait', check=False).stdout.strip()
        if state not in ('', 'offline') or time.monotonic() > deadline:
            break
        time.sleep(0.5)
    if state != 'running':
        failed = inside(name, vm.FAILED_UNITS, check=False).stdout.strip()
        raise RuntimeError(f'{name}: systemd is {state or "not answering"}: {failed}')


def image_alias(distribution, kind):
    return f'mmx-test-image-{kind}-{distribution}'


def base_name(distribution, kind):
    return f'mmx-test-base-{kind}-{distribution}'


# A base older than a day is made again. Its package indexes come from the
# day it was made, and a security update makes the mirror drop the package
# file that an older index names, and a suite that installs that package
# gets 404 Not Found. A day keeps the indexes within
# what the mirrors serve; making the bases again takes a few minutes, once
# a day per test server.
BASE_MAX_AGE = datetime.timedelta(days=1)


def base_is_older_than_a_day(name):
    created = json.loads(incus('query', f'/1.0/instances/{name}').stdout)['created_at']
    return datetime.datetime.now(datetime.timezone.utc) - datetime.datetime.fromisoformat(created) > BASE_MAX_AGE


def local_image(distribution, kind):
    """Copy the distribution's image into Incus's local store once, so that
    making a base never depends on the image server."""
    alias = image_alias(distribution, kind)
    if incus('image', 'info', alias, check=False).returncode != 0:
        incus('image', 'copy', 'images:' + IMAGES[distribution], 'local:', '--alias', alias,
              *(['--vm'] if kind == VIRTUAL_MACHINE else []), timeout=1800)
    return alias


def prepare_base(distribution, kind):
    """Make the stopped base each job slot's guests are copied from."""
    name = base_name(distribution, kind)
    started = time.monotonic()
    new = name + '-new'
    incus('delete', '--force', new, check=False)
    alias = local_image(distribution, kind)
    if kind == VIRTUAL_MACHINE:
        incus('init', alias, new, *VM_SETTINGS, '-d', 'root,size=' + VM_ROOT_DISK_SIZE)
        if distribution in IMAGES_INCOMPATIBLE_WITH_SECURE_BOOT:
            incus('config', 'set', new, 'security.secureboot=false')
        # The VM images take the agent's configuration from this disk.
        incus('config', 'device', 'add', new, 'agent', 'disk', 'source=agent:config')
        incus('start', new)
    else:
        incus('launch', alias, new, *CONTAINER_SETTINGS)
    try:
        # A first boot does more, and the bases are often made together.
        wait_running(new, seconds=600)
        set_package_servers(new, distribution)
        manager = PACKAGE_MANAGERS[distribution]
        inside(new, PREPARE[manager], timeout=1800)
        inside(new, REMOVE_IMAGE_UDEV_RULE_FOR_VETH_DEVICES)
        inside(new, f'id {LOGIN} >/dev/null 2>&1 || useradd -m -s /bin/bash {LOGIN}; '
               f'printf "%s\\n" "{LOGIN} ALL=(ALL) NOPASSWD:ALL" > /etc/sudoers.d/{LOGIN}; '
               f'chmod 0440 /etc/sudoers.d/{LOGIN}; printf "%s\\n" {vm.MARKER} > /etc/mmx-test-vm; '
               # cloud-init has done its one job; later boots skip it.
               'if [ -d /etc/cloud ]; then touch /etc/cloud/cloud-init.disabled; fi')
        if kind == VIRTUAL_MACHINE:
            if manager == 'dnf':
                selinux_enforcing(new)
            # A copy's agent would rewrite hostname, hosts, network files, and
            # cloud-init's seed from the image's templates on its first boot,
            # which SELinux forbids it; each copy sets its hostname instead.
            metadata = json.loads(incus('query', f'/1.0/instances/{new}/metadata').stdout)
            metadata['templates'] = {}
            incus('query', '-X', 'PUT', f'/1.0/instances/{new}/metadata', '--data', json.dumps(metadata))
        # Each copy starts without the timers' stamps of the base's boots, as
        # a server from a cloud image does. With them, a copy that booted
        # on a later day caught up on the daily apt upgrade that it "missed",
        # within an hour of its boot: unattended-upgrade upgraded systemd in
        # the middle of a suite, and an MMX command failed while systemd
        # re-executed ("Transport endpoint is not connected").
        inside(new, 'rm -f /var/lib/systemd/timers/stamp-*')
        # Each copy makes its own machine ID on its first boot, as a server
        # from a cloud image does; Debian's DHCP client ID, and so the
        # address, comes from it. Last, because every boot writes one.
        inside(new, ': > /etc/machine-id')
        incus('stop', new)
        incus('delete', '--force', name, check=False)
        incus('move', new, name)
    except BaseException:
        incus('delete', '--force', new, check=False)
        raise
    print(f'{distribution}: {kind} base prepared in {time.monotonic() - started:.0f}s', flush=True)
    return name



class SharedGuestSlots:
    """Lock files that every test run on this machine shares, in a directory
    that the machine's administrator fills with files named slot-*. A guest
    pair holds some of them while it runs, so that the runs of several
    projects together never start more guests than the machine has room for.
    The kernel gives a lock back when its process ends, even by a kill.

    While several runs want slots, each takes no more than its fair share,
    the slots divided by those runs; a run alone takes them all. Without the
    share, the run that came first kept every slot that it gave back, and a
    run that came later waited until the first had ended. A run that holds
    or wants slots holds a lock on its own file in runs/, which is how the
    others count it."""

    def __init__(self, directory):
        self.directory = Path(directory)
        self.files = sorted(Path(directory).glob('slot-*'))
        if not self.files:
            raise RuntimeError(f'{directory} has no files named slot-*')
        self.runs = Path(directory) / 'runs'
        self.runs.mkdir(exist_ok=True)
        self.own_marker_path = self.runs / f'{os.getpid()}-{uuid.uuid4().hex[:8]}'
        self.own_marker = None
        self.lock = threading.Lock()
        self.held = 0
        self.wanting = 0

    def runs_that_want_slots(self):
        """This run and the others that hold or want slots now."""
        count = 1
        for path in self.runs.iterdir():
            if path == self.own_marker_path:
                continue
            try:
                with path.open() as marker:
                    fcntl.flock(marker, fcntl.LOCK_SH | fcntl.LOCK_NB)
            except BlockingIOError:
                count += 1
            except FileNotFoundError:
                pass
        return count

    def mark_or_unmark(self):
        """Hold this run's file in runs/ exactly while it holds or wants slots."""
        if (self.held or self.wanting) and self.own_marker is None:
            self.own_marker = self.own_marker_path.open('a')
            fcntl.flock(self.own_marker, fcntl.LOCK_EX)
        elif not (self.held or self.wanting) and self.own_marker is not None:
            self.own_marker.close()
            self.own_marker = None
            self.own_marker_path.unlink(missing_ok=True)

    def acquire(self, count, holder, give_up):
        """Hold count slots; return their open files, or None once give_up()
        is true. A pair that cannot get all of its slots holds none, so that
        two runs never wait for each other's half."""
        with self.lock:
            self.wanting += 1
            self.mark_or_unmark()
        try:
            while True:
                share = len(self.files) // self.runs_that_want_slots()
                with self.lock:
                    if self.held + count <= max(share, count):
                        held = self.take(count, holder)
                        if held:
                            self.held += count
                            return held
                if give_up():
                    return None
                # Runs that wait try again at random times, so that no run
                # keeps every slot that another run gives back.
                time.sleep(random.uniform(1, 3))
        finally:
            with self.lock:
                self.wanting -= 1
                self.mark_or_unmark()

    def take(self, count, holder):
        held = []
        for path in random.sample(self.files, len(self.files)):
            handle = path.open('r+')
            try:
                fcntl.flock(handle, fcntl.LOCK_EX | fcntl.LOCK_NB)
            except BlockingIOError:
                handle.close()
                continue
            held.append(handle)
            if len(held) == count:
                for handle in held:
                    handle.truncate(0)
                    handle.write(holder + '\n')
                    handle.flush()
                return held
        for handle in held:
            handle.close()
        return None

    def release(self, held):
        for handle in held:
            handle.truncate(0)
            handle.close()
        with self.lock:
            self.held -= len(held)
            self.mark_or_unmark()


class Run:
    """One invocation: its ownership manifest, login key, bases, and the
    guest pairs it runs at once on this machine."""

    def __init__(self, pairs_at_once, shared_slots=None):
        self.name = 'mmx-' + time.strftime('%Y%m%d-%H%M%S') + '-' + uuid.uuid4().hex[:6]
        self.directory = vm.ROOT / 'incus-runs' / self.name
        self.directory.mkdir(parents=True, mode=0o700)
        self.manifest_path = self.directory / 'ownership.json'
        save(self.manifest_path, {'run': self.name})
        print('Ownership manifest: ' + str(self.manifest_path), flush=True)
        # A key for this run's guests only; cleanup deletes it with them.
        self.login_key = self.directory / 'login-key'
        subprocess.run(['ssh-keygen', '-q', '-t', 'ed25519', '-N', '', '-C', LOGIN + ' ' + self.name,
                        '-f', str(self.login_key)], check=True)
        self.lock = threading.Lock()
        self.base_locks = {}
        self.base_failures = {}
        self.stopping = threading.Event()
        self.next_cpu = 0
        # Slots wait for a free place, so that the machine's CPUs are busy
        # but not crowded.
        self.pair_places = threading.Semaphore(pairs_at_once)
        self.shared_slots = shared_slots

    def cpus(self):
        """Two of this machine's CPUs for a new container, in turn."""
        with self.lock:
            first = self.next_cpu
            self.next_cpu = (first + 2) % (os.cpu_count() or 2)
        return f'{first},{(first + 1) % (os.cpu_count() or 2)}'

    def base(self, distribution, kind):
        """The base of this distribution and kind, prepared once also when
        several slots ask at the same time."""
        with self.lock:
            guard = self.base_locks.setdefault((distribution, kind), threading.Lock())
        with guard:
            # A base that failed once fails the run's other slots with the
            # same error, instead of each trying again.
            if (distribution, kind) in self.base_failures:
                raise RuntimeError(f'the base could not be prepared: {self.base_failures[distribution, kind]}')
            name = base_name(distribution, kind)
            with lock_of_base(self.shared_slots, name):
                if incus('info', name, check=False).returncode != 0 or base_is_older_than_a_day(name):
                    try:
                        prepare_base(distribution, kind)
                    except Exception as error:
                        self.base_failures[distribution, kind] = error
                        raise
            return name


@contextmanager
def lock_of_base(shared_slots, name):
    """The lock that every run on this machine holds while it looks for a
    base and prepares a missing one. Two runs at the same time can both find
    a base missing; without the lock both would prepare it under the same
    temporary name, and one would delete the guest or the base that the other
    made. The lock file is in the directory of the shared guest slots, which
    every run on a test server uses, or else in the test environment."""
    directory = (shared_slots.directory if shared_slots else vm.ROOT) / 'bases'
    directory.mkdir(parents=True, exist_ok=True)
    with (directory / (name + '.lock')).open('a') as handle:
        fcntl.flock(handle, fcntl.LOCK_EX)
        yield


def save(path, data):
    path.write_text(json.dumps(data, indent=2) + '\n')
    path.chmod(0o600)


def owned(run_name):
    listing = incus('list', 'user.mmx-test-run=' + run_name, '--format', 'json').stdout
    return [instance['name'] for instance in json.loads(listing)]


def cleanup(path):
    """Delete a run's guests, check that none is left, and delete its login key."""
    manifest = json.loads(path.read_text())
    for name in owned(manifest['run']):
        incus('delete', '--force', name, check=False)
    left = owned(manifest['run'])
    if left:
        raise RuntimeError(f'Incus guests left: {", ".join(left)}; run incus.py --cleanup-from-manifest {path}')
    manifest['cleanup_verified'] = True
    save(path, manifest)
    for key in ('login-key', 'login-key.pub'):
        (path.parent / key).unlink(missing_ok=True)
    print('Cleanup verified: no guests of this run remain', flush=True)


def start_guest(run, distribution, kind, slot, role):
    """Copy the base into a new guest of this run, start it, and give it its
    own hostname, SSH host key, and the run's login key. Return its name,
    address, and SSH host key."""
    name = f'{run.name}-{distribution}-{kind}-{slot}-{role.removeprefix("mmx-")}'
    # A container gets two fixed CPUs. With a count instead, Incus picks the
    # CPUs after the start, and until then the container sees all of this
    # machine's: MMX's facts read a CPU count that a reboot then changed.
    cpus = ['-c', 'limits.cpu=' + run.cpus()] if kind == CONTAINER else []
    incus('copy', run.base(distribution, kind), name, '-c', 'user.mmx-test-run=' + run.name, *cpus)
    try:
        if kind == VIRTUAL_MACHINE:
            # Every VM runs with Secure Boot off. With it on, the kernel loads
            # only modules that the distribution signed, and the ZFS suite's
            # module, which the OpenZFS project or a build on the guest
            # provides, is refused ("Key was rejected by service"). A suite
            # cannot change this from inside its guest. The setting is
            # changed on the copy, not given to incus copy: only a change
            # makes Incus replace the firmware variables that the base's
            # Secure Boot firmware wrote, and a start with the old ones fails
            # ("Unable to locate matching firmware").
            incus('config', 'set', name, 'security.secureboot=false')
        return name, *set_up_guest(run, name)
    except BaseException:
        incus('delete', '--force', name, check=False)
        raise


def set_up_guest(run, name):
    """Start a new copy; return its address and SSH host key."""
    incus('start', name)
    wait_running(name)
    key = Path(str(run.login_key) + '.pub').read_text().strip()
    # A base made before its preparation removed the rule still has it.
    inside(name, REMOVE_IMAGE_UDEV_RULE_FOR_VETH_DEVICES)
    inside(name, f'hostnamectl set-hostname {name}; '
           'rm -f /etc/ssh/ssh_host_*; ssh-keygen -A >/dev/null; '
           f'install -d -m 700 -o {LOGIN} -g {LOGIN} /home/{LOGIN}/.ssh; '
           f'printf "%s\\n" "{key}" > /home/{LOGIN}/.ssh/authorized_keys; '
           f'chown {LOGIN}:{LOGIN} /home/{LOGIN}/.ssh/authorized_keys; chmod 600 /home/{LOGIN}/.ssh/authorized_keys; '
           '(systemctl restart ssh 2>/dev/null || systemctl restart sshd)')
    deadline = time.monotonic() + 60
    while True:
        state = json.loads(incus('query', f'/1.0/instances/{name}/state').stdout)
        # A container's interface is eth0, a VM's is named for its slot, such as enp5s0.
        address = next((a['address'] for interface, details in (state.get('network') or {}).items()
                        if interface != 'lo' for a in details['addresses'] if a['family'] == 'inet'), None)
        if address:
            break
        if time.monotonic() > deadline:
            raise RuntimeError(name + ' has no IPv4 address')
        time.sleep(0.5)
    host_key = ' '.join(inside(name, 'cat /etc/ssh/ssh_host_ed25519_key.pub').stdout.split()[:2])
    return address, host_key


def target_of(run, directory, role, address, host_key, kind, distribution, name):
    """The entry of a started guest in its slot's MMX_TEST_TARGETS file, and
    its platform."""
    known = directory / (role + '.known_hosts')
    known.write_text(f'{address} {host_key}\n')
    known.chmod(0o600)
    node = {'address': address, 'ssh_port': 22, 'ssh_user': LOGIN, 'ssh_identity_file': str(run.login_key),
            'ssh_known_hosts': str(known), 'ssh_config': '/dev/null'}
    platform = vm.cmd([*vm.ssh_args(node), '. /etc/os-release; printf "%s, kernel %s, SELinux %s" '
                       '"$PRETTY_NAME" "$(uname -r)" "$(getenforce 2>/dev/null || echo absent)"']).stdout
    return {'node': node, 'platform': f'{platform} AMD64 Incus {kind}', 'architecture': 'amd64',
            'image': IMAGES[distribution], 'incus_instance': name}, platform


def ready_targets(run, distribution, kind, slot, names):
    """Start the slot's two guests into names; write and return its
    MMX_TEST_TARGETS file."""
    directory = run.directory / f'{distribution}-{kind}-{slot}'
    directory.mkdir(mode=0o700, exist_ok=True)
    targets = {}
    for role in ('mmx-dev', 'mmx-peer'):
        name, address, host_key = start_guest(run, distribution, kind, slot, role)
        names.append(name)
        targets[role], platform = target_of(run, directory, role, address, host_key, kind, distribution, name)
    path = directory / 'targets.json'
    save(path, targets)
    return path, platform


@contextmanager
def third_guest(run, distribution, kind, slot, targets_path):
    """all.py's third_machine: one more guest, mmx-third, in the slot's
    MMX_TEST_TARGETS file for one suite, deleted afterwards. Give False when
    it could not start."""
    names = []
    try:
        try:
            name, address, host_key = start_guest(run, distribution, kind, slot, 'mmx-third')
            names.append(name)
            targets = json.loads(targets_path.read_text())
            targets['mmx-third'], _ = target_of(run, targets_path.parent, 'mmx-third', address, host_key, kind, distribution, name)
            save(targets_path, targets)
        except Exception as error:
            with suite_runner.OUTPUT_LOCK:
                print(f'{distribution}: {kind} slot {slot} has no third guest: {error}', flush=True)
            yield False
            return
        yield True
    finally:
        for name in names:
            incus('delete', '--force', name, check=False)
        if targets_path.exists():
            targets = json.loads(targets_path.read_text())
            if targets.pop('mmx-third', None) is not None:
                save(targets_path, targets)


@contextmanager
def guest_pair(run, distribution, kind, slot, pending):
    """all.py's cloud_servers: new guests for one job slot, deleted when the
    slot is done. Yield None when the guests could not start; the other
    slots then run the suites."""
    names = []
    placed = False
    shared = None
    try:
        targets = None

        def give_up():
            return pending.empty() or run.stopping.is_set()
        while not (placed := run.pair_places.acquire(timeout=5)):
            if give_up():
                break
        if placed and run.shared_slots:
            shared = run.shared_slots.acquire(1, f'{run.name} {distribution} {kind} slot {slot} PID {os.getpid()}', give_up)
        if not placed or (run.shared_slots and shared is None):
            yield None
            return
        started = time.monotonic()
        try:
            if run.stopping.is_set():
                raise RuntimeError('the run is stopping')
            targets, platform = ready_targets(run, distribution, kind, slot, names)
            with suite_runner.OUTPUT_LOCK:
                print(f'{distribution}: {kind} slot {slot} ready in {time.monotonic() - started:.1f}s: {platform}',
                      flush=True)
        except Exception as error:
            with suite_runner.OUTPUT_LOCK:
                print(f'{distribution}: {kind} slot {slot} has no guests: {error}', flush=True)
        yield targets
    finally:
        for name in names:
            incus('delete', '--force', name, check=False)
        if shared:
            run.shared_slots.release(shared)
        if placed:
            run.pair_places.release()


def enlarge(name, selected):
    """vm.enlarge for an Incus guest: stop it, give it one more CPU and 1 GiB
    more memory, and start it again, as a cloud server's type change does.
    Return the function that gives it back its original size."""
    instance = selected['incus_instance']
    cpus = incus('config', 'get', instance, 'limits.cpu').stdout.strip()
    memory = incus('config', 'get', instance, 'limits.memory').stdout.strip()
    larger = str(int(memory.removesuffix('GiB')) + 1) + 'GiB'
    if ',' in cpus:
        # A container's fixed CPUs and the one after its last.
        more = f'{cpus},{(int(cpus.split(",")[-1]) + 1) % (os.cpu_count() or 2)}'
    else:
        more = str(int(cpus) + 1)

    def resize(cpu_limit, memory_limit):
        old = vm.console('cat /proc/sys/kernel/random/boot_id', name).strip()
        vm.close_forward(name)
        incus('stop', instance)
        incus('config', 'set', instance, f'limits.cpu={cpu_limit}', f'limits.memory={memory_limit}')
        incus('start', instance)
        vm.await_boot(old, name)

    resize(more, larger)
    return lambda: resize(cpus, memory)


def run_queue(run, distribution, kind, pairs, suites, hetzner_run):
    """Run one distribution's container or kernel suites; return all.py's status."""
    root = vm.ROOT / distribution / ('containers' if kind == CONTAINER else 'kernel')
    label = f'{distribution} {kind}: '
    shared_build = run.directory / 'build'
    try:
        if kind == VIRTUAL_MACHINE and hetzner_run and distribution in hetzner.IMAGES:
            return suite_runner.run_all(root, pairs, suites=suites, label=label, shared_build=shared_build,
                                        cloud_servers=lambda slot, pending: hetzner.server_pair(
                                            hetzner_run, distribution, slot, pending),
                                        third_machine=lambda slot, targets: hetzner.third_server(
                                            hetzner_run, distribution, slot, targets))
        return suite_runner.run_all(root, pairs, suites=suites, label=label, shared_build=shared_build,
                                    cloud_servers=lambda slot, pending: guest_pair(run, distribution, kind,
                                                                                   slot, pending),
                                    third_machine=lambda slot, targets: third_guest(run, distribution, kind,
                                                                                    slot, targets))
    except Exception:
        with suite_runner.OUTPUT_LOCK:
            print(f'{label}runner failed\n{traceback.format_exc()}', flush=True)
        return 1


def main():
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument('--distribution', action='append', choices=sorted(IMAGES),
                        help='repeat for each distribution to test; default: all of them')
    parser.add_argument('--suite', action='append', choices=[' '.join(s) for s in suite_runner.SUITES],
                        help='repeat to run only these suites; default: all of them')
    parser.add_argument('--base-suite', action='store_true',
                        help="run the base suite (bootstrap, both connections, recovery, and the plain-named modules; "
                             "BASE_SUITE in all.py), also together with --suite")
    parser.add_argument('--build-and-use-release-archive', metavar='VERSION',
                        help="build release archives from this working tree and use this controller machine's archive")
    parser.add_argument('--container-pairs-per-distribution', type=int, default=3,
                        help='job slots per distribution for the container suites, each with two containers; default 3')
    parser.add_argument('--vm-pairs-per-distribution', type=int, default=2,
                        help='job slots per distribution for the suites that need their own kernel; default 2')
    parser.add_argument('--max-guest-pairs-at-once', type=int, default=(os.cpu_count() or 2) * 3 // 2,
                        help='the most job slots that have their two guests on this machine at once; '
                             "default: one and a half times the machine's CPU count")
    parser.add_argument('--shared-guest-slots', type=Path, metavar='DIRECTORY',
                        help='a directory of lock files named slot-* that every test run on this machine shares; '
                             'each guest pair holds one of them while it runs')
    parser.add_argument('--kernel-suites-on-hetzner', action='store_true',
                        help='run the suites that need their own kernel on Hetzner Cloud servers, '
                             'for the distributions Hetzner has; needs MMX_TEST_HETZNER_API_TOKEN and MMX_TEST_HETZNER_SSH_KEY_NAME')
    parser.add_argument('--max-hetzner-servers-at-once', type=int, default=hetzner.DEFAULT_MAX_SERVERS_AT_ONCE,
                        help=f'with --kernel-suites-on-hetzner, the most servers this run holds at once; '
                             f'default {hetzner.DEFAULT_MAX_SERVERS_AT_ONCE}, as for hetzner.py')
    parser.add_argument('--prepare-bases', action='store_true',
                        help='make the bases of the selected distributions again, from the newest images, and stop')
    parser.add_argument('--cleanup-from-manifest', type=Path, metavar='OWNERSHIP_JSON',
                        help="delete a run's guests again, from its ownership.json")
    args = parser.parse_args()
    if min(args.container_pairs_per_distribution, args.vm_pairs_per_distribution, args.max_guest_pairs_at_once) < 1:
        parser.error('pairs per distribution and pairs at once must be positive')
    distributions = args.distribution or list(IMAGES)
    with testenv.locked(vm.ROOT):
        if args.cleanup_from_manifest:
            cleanup(args.cleanup_from_manifest.resolve())
            return 0
        if args.prepare_bases:
            kinds = [(d, k) for d in distributions for k in (CONTAINER, VIRTUAL_MACHINE)]
            for distribution, kind in kinds:
                incus('image', 'delete', image_alias(distribution, kind), check=False)
            with ThreadPoolExecutor(max_workers=len(kinds)) as pool:
                failures = [f for f in pool.map(lambda dk: _failure(prepare_base, *dk), kinds) if f]
            for failure in failures:
                print(failure, flush=True)
            return int(bool(failures))
        suites = suite_runner.selected_suites(args.suite, args.base_suite)
        # Before any guest or server: a selection that runs nowhere ends here.
        suites_of_distribution = suite_runner.suites_by_distribution(suites, distributions)
        distributions = list(suites_of_distribution)
        hetzner_run = None
        if args.kernel_suites_on_hetzner:
            hetzner_run = hetzner.Run(args.max_hetzner_servers_at_once)
        run = Run(args.max_guest_pairs_at_once, SharedGuestSlots(args.shared_guest_slots) if args.shared_guest_slots else None)
        queues = []
        for distribution, its_suites in suites_of_distribution.items():
            for kind, pairs in ((CONTAINER, args.container_pairs_per_distribution),
                                (VIRTUAL_MACHINE, args.vm_pairs_per_distribution)):
                selected = tuple(s for s in its_suites if needs_own_kernel(s, distribution) == (kind == VIRTUAL_MACHINE))
                if selected:
                    queues.append((distribution, kind, pairs, selected))

        def interrupted(signum, frame):
            raise KeyboardInterrupt
        for sig in (signal.SIGTERM, signal.SIGINT):
            signal.signal(sig, interrupted)
        started = time.monotonic()
        pool = ThreadPoolExecutor(max_workers=len(queues))
        try:
            if args.build_and_use_release_archive:
                vm.build_and_use_release_archive(run.directory, args.build_and_use_release_archive)
            futures = [(d, pool.submit(run_queue, run, d, k, p, s, hetzner_run)) for d, k, p, s in queues]
            failed = sorted({d for d, future in futures if future.result()})
        except BaseException:
            run.stopping.set()
            if hetzner_run:
                hetzner_run.stop()
            raise
        finally:
            # A second Ctrl-C or SIGTERM must not cut the deletion short.
            for sig in (signal.SIGTERM, signal.SIGINT):
                signal.signal(sig, signal.SIG_IGN)
            pool.shutdown(wait=True)
            cleanup(run.manifest_path)
            if hetzner_run:
                hetzner.cleanup(hetzner_run.manifest_path)
        print(f'Incus run {run.name}: {len(distributions) - len(failed)} of {len(distributions)} distributions passed'
              + (', failed: ' + ', '.join(failed) if failed else '') + f'; {time.monotonic() - started:.0f}s',
              flush=True)
        return int(bool(failed))


def _failure(function, *args):
    try:
        function(*args)
        return None
    except Exception as error:
        return f'{" ".join(args)}: FAILED: {error}'


if __name__ == '__main__':
    raise SystemExit(main())
