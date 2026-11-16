#!/usr/bin/env python3
"""certbot_certificate through the public CLI, against a Pebble on the guest
itself (the ACME test server of Let's Encrypt), checked with openssl,
certbot's own files, and the distribution's packaged renewal service.

No request of this suite may reach a public certificate authority, also not
Let's Encrypt's test server. Each task explicitly chooses its authority,
and certbot's own "renew --dry-run" asks Let's Encrypt's test
server whatever a certificate's settings say. So:

- Before certbot is installed, the suite maps both Let's Encrypt names to
  the guest itself in /etc/hosts and checks that they resolve nowhere else.
- Every task names certificate_authority_url, a Pebble on the guest. One case
  explicitly chooses Let's Encrypt: with the names mapped to the guest,
  it fails with a connection error that names Let's Encrypt's address.
- At its end the suite lists every host that certbot contacted, from
  certbot's log, and fails when one is not the guest itself.
"""
import base64
import hashlib
import io
import itertools
import json
import os
import re
import shlex
import shutil
import subprocess
import sys
import tarfile
import threading
import time
import urllib.request

import vm

LETS_ENCRYPT_NAMES = ('acme-v02.api.letsencrypt.org', 'acme-staging-v02.api.letsencrypt.org')
LETS_ENCRYPT = 'https://acme-v02.api.letsencrypt.org/directory'
# Pebble's release archives, each with one statically linked program.
PEBBLE_RELEASE = 'https://github.com/letsencrypt/pebble/releases/download/v2.10.1/'
PEBBLE_ARCHIVES = {
    'amd64': ('pebble-linux-amd64.tar.gz', '4f2fcb5bca8c85c9cf73ad140fccfc0d2be40bd81ab99879c79b7b8a0b4f70ed'),
    'arm64': ('pebble-linux-arm64.tar.gz', 'b53fd072a69eb7692451de4e8b0667e0bdf5cccd7e36fc51b8eaf2fcc135ed9f'),
}
# The certificate authority that the suite makes for Pebble's HTTPS address,
# and Pebble's own certificate from it. Each has the extensions that a
# certificate of its kind must have: Python 3.13 and newer, as on Debian 13,
# refuse an authority without a key usage extension.
AUTHORITY_CONFIGURATION = """[req]
distinguished_name = name
x509_extensions = authority
prompt = no
[name]
CN = MMX test certificate authority for Pebble
[authority]
basicConstraints = critical, CA:TRUE
keyUsage = critical, keyCertSign, cRLSign
subjectKeyIdentifier = hash
[server]
basicConstraints = critical, CA:FALSE
keyUsage = critical, digitalSignature, keyEncipherment
extendedKeyUsage = serverAuth
subjectAltName = DNS:localhost, IP:127.0.0.1
subjectKeyIdentifier = hash
authorityKeyIdentifier = keyid"""
# Everything of the suite's own on the guest is below these two. Neither is
# below /tmp or /var/tmp: the packaged renewal service of Debian and Ubuntu
# has PrivateTmp=true, so a script that certbot runs there sees its own
# empty /tmp and /var/tmp.
PEBBLE = '/opt/mmx-test-pebble'
BASE = '/var/lib/mmx-test-certbot-certificates'
# Two authorities: A issues certificates for 90 days, B for half a minute, so
# that certbot's packaged service finds a certificate of B due: certbot up
# to 4.0 at once, certbot 5.8 after half of the certificate's life.
AUTHORITY_A = 'https://localhost:14000/dir'
AUTHORITY_B = 'https://localhost:14100/dir'
VALIDITY_OF_B_IN_SECONDS = 30
# The directories that the suite's web server serves in turn, the second of
# them only after MMX wrote it into certbot's renewal settings.
WEBROOT = '/var/www/certbot-challenges'
OTHER_WEBROOT = '/var/www/other-certbot-challenges'
NOT_SERVED = BASE + '/a-directory-that-no-web-server-serves'
# A name that both Pebbles refuse by policy.
BLOCKED = 'blocked-domain.example'
# The scripts that certbot runs after a certificate; each appends a line to
# its own file. The second has a space and a comma in its path, which
# certbot's shell and its renewal file each need quoted.
SCRIPT = '/usr/local/bin/mmx-test-after-certificate-renewal'
SCRIPT_RUNS = BASE + '/script-runs'
QUOTED_SCRIPT = '/usr/local/lib/mmx test scripts/after renewal, second'
QUOTED_SCRIPT_RUNS = BASE + '/quoted-script-runs'
EXAMPLE_SCRIPT = '/usr/local/bin/reload-web-server-after-certificate-renewal'
WEB_SERVER, WEB_SERVER_RELOADS = 'mmx-test-web-server', BASE + '/web-server-reloads'
UNITS = ('mmx-test-pebble-a', 'mmx-test-pebble-b', 'mmx-test-dns-server', WEB_SERVER, 'mmx-test-other-certbot', 'mmx-test-lock-watcher')
DNS_KEY_NAME = 'mmx-test-key'
# The suite's DNS server listens on the DNS port itself, at 127.0.0.1 only.
# SELinux lets BIND listen for TCP only on the ports that its policy names
# for DNS. On another port, such as 8054, BIND still starts and answers
# UDP, and refuses every TCP connection, which is how certbot's rfc2136
# plugin sends its updates.
DNS_PORT = 53
CREDENTIALS = '/etc/letsencrypt/mmx-test-rfc2136.ini'
CERTBOT_LOCK = '/etc/letsencrypt/.certbot.lock'
ADMIN = 'admin@example.com'
INSTALL = 'certbot is not installed; install the package that provides it in an earlier node task: '


# A program for the guest that waits until a process opens a file. It writes
# one marker when its watch is in place and another when the file was
# opened. A task that waits for another certbot opens certbot's lock file to
# ask the kernel about the lock, so the second marker says that the task has
# looked at the lock; a stat of the file, as "test -e" makes it, is no open.
WATCH_FOR_OPEN = """import ctypes, os, select, sys
watched, ready, opened, limit = sys.argv[1], sys.argv[2], sys.argv[3], float(sys.argv[4])
libc = ctypes.CDLL(None, use_errno=True)
descriptor = libc.inotify_init()
if descriptor < 0 or libc.inotify_add_watch(descriptor, watched.encode(), 0x20) < 0:  # IN_OPEN
    sys.exit('inotify: ' + os.strerror(ctypes.get_errno()))
open(ready, 'w').close()
if not select.select([descriptor], [], [], limit)[0]:
    sys.exit('nobody opened ' + watched)
open(opened, 'w').close()"""


def pebble_program(architecture):
    """Fetch Pebble's release archive for the architecture once for a test
    run, check its SHA-256, and return the path of the program in it."""
    archive, sha256 = PEBBLE_ARCHIVES[architecture]
    with vm.shared_build() as shared:
        program = (shared or vm.ROOT) / ('pebble-linux-' + architecture)
        if not program.exists():
            program.parent.mkdir(parents=True, exist_ok=True)
            with urllib.request.urlopen(PEBBLE_RELEASE + archive, timeout=120) as answer:
                data = answer.read()
            found = hashlib.sha256(data).hexdigest()
            assert found == sha256, f'{archive} has SHA-256 {found}, not {sha256}'
            with tarfile.open(fileobj=io.BytesIO(data)) as files:
                member = files.extractfile(f'{archive.removesuffix(".tar.gz")}/linux/{architecture}/pebble')
                building = program.with_name(program.name + '.building')
                building.write_bytes(member.read())
            building.chmod(0o755)
            os.replace(building, program)
    return program


def pebble_configuration(port, validity_in_seconds):
    return json.dumps({'pebble': {
        'listenAddress': f'127.0.0.1:{port}', 'managementListenAddress': f'127.0.0.1:{port + 1000}',
        'certificate': PEBBLE + '/cert.pem', 'privateKey': PEBBLE + '/key.pem',
        # The port on which Pebble fetches a challenge file, and the one
        # where certbot's standalone server answers.
        'httpPort': 80, 'tlsPort': 443, 'ocspResponderURL': '', 'externalAccountBindingRequired': False,
        'domainBlocklist': [BLOCKED], 'retryAfter': {'authz': 3, 'order': 5}, 'keyAlgorithm': 'ecdsa',
        'profiles': {'default': {'description': 'the profile of the test', 'validityPeriod': validity_in_seconds}}}})


def main():
    vm.up()
    node, ssh = vm.connection()
    vm.reset()
    workspace = vm.ROOT / 'certbot-certificates'
    if workspace.exists():
        shutil.rmtree(workspace)
    vm.write_inventory(workspace, {'nodes': {'web': node}}, name='example')
    vm.build()
    mmx = vm.ROOT / 'bin/mmx'
    passed, printed = [], []
    marked = time.monotonic()
    # The secret of the DNS server's update key: a sentinel that must stay
    # out of everything that mmx prints and saves.
    dns_secret = base64.b64encode(os.urandom(32)).decode()
    vm.ENV['MMX_TEST_DNS_UPDATE_KEY'] = dns_secret
    secrets = {'secrets_from_env': {'dns_update_key': 'MMX_TEST_DNS_UPDATE_KEY'}}

    def remote(script, check=True):
        result = vm.cmd([*ssh, 'sudo -n /bin/sh -c ' + shlex.quote('set -eu\n' + script)], check=check)
        return result.stdout.strip()

    def project(tasks, **settings):
        vm.write_project(workspace, project_name='certbot_certificates', fleet_stages=[{'id': 'configure', 'nodes': 'all', 'node_tasks': tasks}], **settings)

    def mmx_run(command, arguments, directory, expected, timeout=300):
        result = vm.cmd([mmx, command, *arguments, '--json'], cwd=directory, check=False, timeout=timeout)
        printed.append(result.stdout + result.stderr)
        if result.returncode not in (vm.success_codes(command) if expected is None else (expected,)):
            raise AssertionError((command, result.stdout, result.stderr))
        return json.loads(result.stdout)

    def run(command, expected=None):
        return mmx_run(command, vm.playbook_args(command), workspace, expected)

    def results(record):
        return {r['task'].rsplit('/', 1)[-1]: r for r in record['entries'][0]['response']['results']}

    def changes(record):
        return {name: r['change'] for name, r in results(record).items()}

    def unchanged(record):
        """The record's overall change is unchanged; a failure shows what the record's tasks say."""
        overall = record['entries'][0]['response']['change']
        assert overall == 'unchanged', (overall, {name: (r['status'], r['change'], r.get('reason', '')) for name, r in results(record).items()})
        return True

    def mark(message):
        # The seconds since the last mark say where the suite's time goes.
        nonlocal marked
        now = time.monotonic()
        passed.append(message)
        print(f'PASS {message} ({now - marked:.0f} s)', flush=True)
        marked = now

    def task(name='shop.example.com', domains=('shop.example.com', 'www.shop.example.com'), id='certificate', **fields):
        """A certbot_certificate task for authority A through the web
        server's directory. A field given as None is left out."""
        inputs = {'certificate_name': name, 'domains': list(domains), 'account_email_when_registered': ADMIN, 'challenge': 'webroot',
                  'webroot_path': WEBROOT, 'certificate_authority_url': AUTHORITY_A, **fields}
        return {'id': id, 'certbot_certificate': {field: written for field, written in inputs.items() if written is not None}}

    def certificate(name):
        """The certificate that certbot has under name, as openssl reads it:
        its names, its serial number, and its issuer; None when there is no
        certificate file."""
        text = remote(f'if [ -e /etc/letsencrypt/live/{name}/cert.pem ]; then openssl x509 -in /etc/letsencrypt/live/{name}/cert.pem -noout -serial -issuer -ext subjectAltName; fi')
        if not text:
            return None
        return {'serial': re.search(r'^serial=(\S+)$', text, re.M).group(1), 'issuer': re.search(r'^issuer=(.*)$', text, re.M).group(1),
                'names': sorted(re.findall(r'DNS:([^,\s]+)', text))}

    def value_of(result):
        value = result['value']
        return value['certificate_dir'], sorted(value['domains'])

    def has(result, name, names, change):
        """The result has the change and names the certificate's directory and its names."""
        assert result['status'] == 'succeeded' and result['change'] == change and value_of(result) == (f'/etc/letsencrypt/live/{name}', sorted(names)), result
        return True

    def certbot_log():
        """Every file of certbot's log directory with its size and time: the
        same listing before and after means that no certbot started."""
        return remote("find /var/log/letsencrypt -type f -printf '%p %s %T@\\n' 2>/dev/null | sort || true")

    def certbot_starts():
        """How many certbot commands have started, by certbot's log: each writes its version first."""
        return int(remote("cat /var/log/letsencrypt/* 2>/dev/null | grep -c 'certbot._internal.main:certbot version:' || true") or 0)

    def requests_sent():
        """How many requests certbot has sent to a certificate authority, by its log."""
        return int(remote("cat /var/log/letsencrypt/* 2>/dev/null | grep -c -E 'acme.client:Sending (GET|POST|HEAD) request to' || true") or 0)

    def renewal_file(name='shop.example.com'):
        return remote(f'cat /etc/letsencrypt/renewal/{name}.conf')

    def lines(path):
        return int(remote(f'cat {shlex.quote(path)} 2>/dev/null | wc -l'))

    def serve(directory):
        """The suite's web server for the files that an authority fetches,
        serving directory. Its reload only notes itself, for the example's
        script."""
        remote(f"""systemctl stop {WEB_SERVER} 2>/dev/null || true
systemctl reset-failed {WEB_SERVER} 2>/dev/null || true
systemd-run --quiet --unit {WEB_SERVER} -p ExecReload='/bin/sh -c "echo reload >> {WEB_SERVER_RELOADS}"' python3 -m http.server 80 --bind 127.0.0.1 --directory {directory}
python3 {PEBBLE}/wait_for_port.py 80""")

    def certbot_renews_now(name):
        """certbot renews the certificate with the settings that its renewal
        file holds, as its timer will, and the certificate is another one
        afterwards. --no-random-sleep-on-renew: without a terminal certbot
        otherwise sleeps up to eight minutes first."""
        before = certificate(name)
        remote(f'certbot renew --cert-name {name} --force-renewal --no-random-sleep-on-renew --non-interactive >{BASE}/renewal.log 2>&1 || {{ cat {BASE}/renewal.log; exit 1; }}')
        after = certificate(name)
        assert after['serial'] != before['serial'] and after['names'] == before['names'], (before, after)
        return after

    def lets_encrypt_names_lead_to_this_guest():
        """Both Let's Encrypt names resolve to this guest and nowhere else."""
        for name in LETS_ENCRYPT_NAMES:
            addresses = set(remote(f"getent ahosts {name} | awk '{{print $1}}'").split())
            assert addresses and addresses <= {'127.0.0.1', '::1'}, f'{name} resolves to {addresses}: the guard against requests to Let\'s Encrypt is broken'

    def hosts_that_certbot_contacted():
        """Every host that a certbot of this guest sent a request to or
        opened a connection to, from certbot's log files, with the number of
        lines; and the failure when one of them is not this guest."""
        found = remote(r"""cat /var/log/letsencrypt/* 2>/dev/null | grep -o -E 'Sending (GET|POST|HEAD) request to https?://[^/ ]+|Starting new HTTPS? connection \([0-9]+\): [^ ]+' | sed -E 's#^Sending [A-Z]+ request to https?://##; s#^Starting new HTTPS? connection \([0-9]+\): ##' | sort | uniq -c || true""")
        hosts = {}
        for line in found.splitlines():
            count, host = line.split()
            hosts[host] = int(count)
        for host in hosts:
            name = host.rsplit(':', 1)[0] if host.count(':') == 1 else host
            addresses = set(remote(f"getent ahosts {shlex.quote(name)} | awk '{{print $1}}'").split())
            assert addresses and addresses <= {'127.0.0.1', '::1'}, f'certbot contacted {host}, which resolves to {addresses}: a request left this guest'
        return hosts

    def selinux_denials():
        """What SELinux denied on the guest since the suite started, each
        denial once. Nothing on a guest without SELinux. A denial is in the
        audit log while the audit service runs, as on a cloud server, and in
        the kernel's log otherwise, as on a test guest without that service.
        ausearch reads its standard input in place of the audit log unless
        --input-logs says otherwise, and in the C locale it takes a date
        with a two-digit year."""
        found = remote(f"""{vm.SELINUX_RUNS} || exit 0
if command -v ausearch >/dev/null 2>&1; then
  found=$(LC_ALL=C ausearch --input-logs -m avc -ts {started_for_ausearch} 2>&1) || case "$found" in *'<no matches>'*) found= ;; *) echo "ausearch failed: $found" >&2; exit 1 ;; esac
  printf '%s\n' "$found" | grep 'avc:' || true
fi
journalctl -k --no-pager -q --since '{started}' 2>/dev/null | grep 'avc:' || true""")
        return sorted({re.sub(r' (pid|ino)=\d+', '', line.split('avc:', 1)[1]).strip() for line in found.splitlines()})

    def cleanup(first=False):
        """Everything of the suite and of certbot, so that the next suite
        finds the guest as this one found it. The two lines in /etc/hosts
        stay while certbot is installed: they go last."""
        remove = {'apt': 'DEBIAN_FRONTEND=noninteractive apt-get purge -y -q', 'dnf': 'dnf remove -y -q'}[manager]
        remote(f"""for unit in {' '.join(UNITS)}; do systemctl stop $unit 2>/dev/null || true; done
systemctl disable --now certbot.timer certbot-renew.timer >/dev/null 2>&1 || true
""" + (f'{remove} {"--autoremove" if manager == "apt" else ""} {" ".join(removable)} >/dev/null 2>&1 || true' if removable else ''))
        # What came with these packages and stayed after their removal:
        # python3-openssl on Debian 13, dbus-libs and python3-dbus on Rocky
        # Linux 9. The guest did not have them, so they go too.
        stayed = sorted(set(remote(installed).split()) - had)
        if stayed:
            remote(f'{remove} {" ".join(stayed)} >/dev/null 2>&1 || true')
        accounts = '\n'.join(f'userdel {account} 2>/dev/null || true\ngroupdel {account} 2>/dev/null || true' for account in removable_accounts)
        remote(f"""{accounts}
rm -rf /etc/letsencrypt /var/lib/letsencrypt /var/log/letsencrypt {PEBBLE} {BASE} {WEBROOT} {OTHER_WEBROOT} {SCRIPT} {EXAMPLE_SCRIPT} "{os.path.dirname(QUOTED_SCRIPT)}"
rm -rf {dns['configuration']} {dns['directory']}
rm -f {trust['file']}
{trust['update']} >/dev/null 2>&1 || true
{'rmdir /var/www 2>/dev/null || true' if not had_var_www else ''}
systemctl reset-failed 2>/dev/null || true
""")
        if not first:
            remote("sed -i '/letsencrypt\\.org$/d' /etc/hosts\nsed -i -e :a -e '/^\\n*$/{$d;N;ba' -e '}' /etc/hosts")

    def guest_is_as_found():
        """Nothing of certbot or of this suite is left, and systemd runs."""
        programs = ' '.join(program for program, package in (('certbot', 'certbot'), ('named', dns['package'])) if package in removable)
        left = remote(f"""command -v {programs} || true
ls -d /etc/letsencrypt /var/lib/letsencrypt /var/log/letsencrypt {PEBBLE} {BASE} {WEBROOT} {OTHER_WEBROOT} {SCRIPT} {EXAMPLE_SCRIPT} {dns['configuration']} {dns['directory']} {trust['file']} /run/mmx/private_task_directories/* 2>/dev/null || true
{'ls -d /var/www 2>/dev/null || true' if not had_var_www else ''}
grep letsencrypt /etc/hosts || true
systemctl list-unit-files 'certbot*' 'mmx-test-*' --no-legend 2>/dev/null || true
systemctl list-units 'mmx-test-*' --all --no-legend 2>/dev/null || true
for account in {' '.join(removable_accounts) or '""'}; do [ -z "$account" ] || getent passwd "$account" || true; done
""")
        assert remote('cat /etc/hosts') == hosts_file_before, 'the suite changed /etc/hosts:\n' + remote('cat /etc/hosts')
        # Every package that came with certbot, its plugin, and the DNS
        # server went with them, and no package that the guest had.
        now = set(remote(installed).split())
        if now != had:
            left += f'\npackages that the guest did not have before: {" ".join(sorted(now - had))}; packages that it had and lost: {" ".join(sorted(had - now))}'
        state = vm.guest('sudo', '-n', 'systemctl', 'is-system-running', check=False).stdout.strip()
        if state != 'running':
            left += f'\nsystemd is {state}:\n' + vm.guest('sudo', '-n', '/bin/sh', '-c', vm.FAILED_UNITS, check=False).stdout
        assert left == '', 'the suite left this on the guest:\n' + left
        print('The guest is as the suite found it: no certbot, no file of it, and systemd is running', flush=True)

    distribution = remote('. /etc/os-release; echo "$ID"')
    # When the suite started, by the guest's clock, as journalctl and as
    # ausearch read a time; and the guest's SELinux mode, '' without SELinux.
    started, started_for_ausearch = remote("date '+%Y-%m-%d %H:%M:%S'; LC_ALL=C date '+%x %H:%M:%S'").split('\n')
    selinux = remote(f'if {vm.SELINUX_RUNS}; then getenforce; fi')
    manager = remote('for manager in apt-get dnf; do if command -v $manager >/dev/null; then echo $manager; break; fi; done').removesuffix('-get')
    # Where each family keeps the certificate of an authority that the node
    # trusts, the DNS server's package, account, and files, and the names of
    # certbot's packages and of its timer.
    trust = {'apt': {'file': '/usr/local/share/ca-certificates/mmx-test-pebble.crt', 'update': 'update-ca-certificates'},
             'dnf': {'file': '/etc/pki/ca-trust/source/anchors/mmx-test-pebble.pem', 'update': 'update-ca-trust'}}[manager]
    # BIND writes into its zone's directory when it takes an update. On the
    # Red Hat family the zone is below /var/named/dynamic, the directory of
    # the package for zones with updates: SELinux lets BIND write there
    # always, and into another directory below /var/named only while the
    # policy's switch named_write_master_zones is on.
    dns = {'apt': {'package': 'bind9', 'account': 'bind', 'configuration': '/etc/bind/mmx-test-named.conf', 'directory': '/var/cache/bind/mmx-test'},
           'dnf': {'package': 'bind', 'account': 'named', 'configuration': '/etc/mmx-test-named.conf', 'directory': '/var/named/dynamic/mmx-test'}}[manager]
    plugin_package = 'python3-certbot-dns-rfc2136'
    timer = 'certbot.timer' if manager == 'apt' else 'certbot-renew.timer'
    renewal_service = timer.removesuffix('.timer') + '.service'
    # Rocky Linux and AlmaLinux have certbot in EPEL.
    with_epel = manager == 'dnf'
    # What the guest had before the suite stays when the suite ends.
    installed = {'apt': "dpkg-query --show --showformat '${db:Status-Abbrev} ${Package}\\n' 2>/dev/null | awk '$1 == \"ii\" {print $2}'",
                 'dnf': "rpm -qa --qf '%{NAME}\\n'"}[manager]
    had = set(remote(installed).split())
    removable = [name for name in ('certbot', plugin_package, dns['package'], 'openssl', *(['epel-release'] if with_epel else [])) if name not in had]
    removable_accounts = [dns['account']] if remote(f'getent passwd {dns["account"]} || true') == '' else []
    had_var_www = remote('test -d /var/www && echo yes || true') == 'yes'
    hosts_file_before = remote("sed '/letsencrypt\\.org$/d' /etc/hosts | sed -e :a -e '/^\\n*$/{$d;N;ba' -e '}'")
    try:
        # The guard comes first, before certbot is on the guest. A newline
        # goes before the lines, for a file that does not end with one.
        remote("sed -i '/letsencrypt\\.org$/d' /etc/hosts\n"
               f"printf '\\n127.0.0.1 {' '.join(LETS_ENCRYPT_NAMES)}\\n::1 {' '.join(LETS_ENCRYPT_NAMES)}\\n' >> /etc/hosts")
        lets_encrypt_names_lead_to_this_guest()
        assert remote('command -v certbot || true\nls -d /etc/letsencrypt 2>/dev/null || true') == '', 'certbot or its directory is on the guest before the suite installed it'
        cleanup(first=True)
        lets_encrypt_names_lead_to_this_guest()
        mark("both names of Let's Encrypt lead to this guest and nowhere else, before certbot is installed")

        # Without certbot: a run names what to install and is not tried
        # again, a dryrun is not checked, and a certificate that the node
        # does not have needs no certbot to be absent.
        project([task(), {'id': 'none', 'certbot_certificate': {'certificate_name': 'none.example.com', 'state': 'absent'}}])
        assert results(run('dryrun'))['certificate']['status'] == 'not_checked'
        without = results(run('run', 1))['certificate']
        assert without['failure_kind'] == 'missing_prerequisite' and without['reason'].startswith(INSTALL) and 'tries' not in without, without
        project([{'id': 'none', 'certbot_certificate': {'certificate_name': 'none.example.com', 'state': 'absent'}}])
        assert unchanged(run('run')) and unchanged(run('dryrun'))
        mark('without certbot, a certificate task says to install it with a package task and a dryrun is not checked; a certificate that is not there is absent without certbot')

        # The suite's own stand-ins on the guest: a certificate authority of
        # its own for Pebble's HTTPS address, which the node trusts as it
        # trusts any authority (certbot gets no option for it); two Pebbles;
        # a DNS server that answers every name below example.com with the
        # guest and takes signed updates; and a web server.
        program = pebble_program(vm.architecture())
        remote(f'install -d -m 755 {PEBBLE} {BASE} {WEBROOT} {OTHER_WEBROOT} {NOT_SERVED}')
        with program.open('rb') as data:
            subprocess.run([*ssh, 'sudo -n /bin/sh -c ' + shlex.quote(f'cat > {PEBBLE}/pebble && chmod 755 {PEBBLE}/pebble')],
                           stdin=data, stdout=subprocess.DEVNULL, env=vm.ENV, check=True, timeout=120)
        project(vm.package_tasks([dns['package'], 'openssl']))
        run('run')
        remote(f"""cd {PEBBLE}
cat > authority.cnf <<'CONFIGURATION'
{AUTHORITY_CONFIGURATION}
CONFIGURATION
openssl req -x509 -config authority.cnf -newkey rsa:2048 -nodes -keyout ca-key.pem -out ca.pem -days 2 >/dev/null 2>&1
openssl req -new -config authority.cnf -newkey rsa:2048 -nodes -keyout key.pem -out request.pem -subj '/CN=localhost' >/dev/null 2>&1
openssl x509 -req -in request.pem -CA ca.pem -CAkey ca-key.pem -CAcreateserial -out cert.pem -days 2 -extfile authority.cnf -extensions server >/dev/null 2>&1
cp ca.pem {trust['file']}
{trust['update']} >/dev/null 2>&1
cat > a.json <<'CONFIGURATION'
{pebble_configuration(14000, 7776000)}
CONFIGURATION
cat > b.json <<'CONFIGURATION'
{pebble_configuration(14100, VALIDITY_OF_B_IN_SECONDS)}
CONFIGURATION
cat > watch_for_open.py <<'PROGRAM'
{WATCH_FOR_OPEN}
PROGRAM
cat > wait_for_port.py <<'PROGRAM'
import socket, sys, time
deadline = time.monotonic() + 30
while True:
    try:
        socket.create_connection(('127.0.0.1', int(sys.argv[1])), timeout=1).close()
        break
    except OSError as error:
        if time.monotonic() > deadline:
            sys.exit(f'nothing answers TCP on 127.0.0.1:{{sys.argv[1]}} after 30 seconds: {{error}}')
        time.sleep(0.1)
PROGRAM
# A stand-in that does not listen fails the suite here, with its name, its
# port, and its own last lines.
listens() {{
  python3 wait_for_port.py "$2" && return
  echo "the stand-in $1 does not listen on 127.0.0.1:$2; its last lines:"
  journalctl --no-pager -q -u "$1" | grep -v 'built with' | tail -n 40
  exit 1
}}
python3 -c "import socket, sys; sys.exit(0 if socket.socket().connect_ex(('127.0.0.1', {DNS_PORT})) else 'a program of the guest already listens on 127.0.0.1:{DNS_PORT}, where the suite starts its DNS server')"
install -d -o {dns['account']} -g {dns['account']} -m 750 {dns['directory']}
cat > {dns['configuration']} <<'CONFIGURATION'
options {{ directory "{dns['directory']}"; listen-on port {DNS_PORT} {{ 127.0.0.1; }}; listen-on-v6 {{ none; }}; recursion no; allow-query {{ any; }};
  pid-file none; session-keyfile "{dns['directory']}/session.key"; dnssec-validation no; }};
key "{DNS_KEY_NAME}" {{ algorithm hmac-sha256; secret "{dns_secret}"; }};
zone "example.com" {{ type master; file "example.com.zone"; allow-update {{ key "{DNS_KEY_NAME}"; }}; }};
CONFIGURATION
chgrp {dns['account']} {dns['configuration']}
chmod 640 {dns['configuration']}
cat > {dns['directory']}/example.com.zone <<'ZONE'
$TTL 1
@ IN SOA ns.example.com. admin.example.com. 1 60 60 60 1
@ IN NS ns.example.com.
ns IN A 127.0.0.1
@ IN A 127.0.0.1
* IN A 127.0.0.1
ZONE
chown {dns['account']}:{dns['account']} {dns['directory']}/example.com.zone
systemd-run --quiet --unit mmx-test-dns-server "$(command -v named)" -g -c {dns['configuration']} -u {dns['account']}
listens mmx-test-dns-server {DNS_PORT}
for authority in a b; do
  systemd-run --quiet --unit mmx-test-pebble-$authority -E PEBBLE_VA_NOSLEEP=1 -E PEBBLE_WFE_NONCEREJECT=0 -E PEBBLE_AUTHZREUSE=0 {PEBBLE}/pebble -config {PEBBLE}/$authority.json -dnsserver 127.0.0.1:{DNS_PORT}
done
listens mmx-test-pebble-a 14000
listens mmx-test-pebble-b 14100
cat > {SCRIPT} <<'SCRIPT'
#!/bin/sh
echo "$RENEWED_LINEAGE [$RENEWED_DOMAINS]" >> {SCRIPT_RUNS}
SCRIPT
chmod 755 {SCRIPT}
install -d -m 755 "{os.path.dirname(QUOTED_SCRIPT)}"
cat > "{QUOTED_SCRIPT}" <<'SCRIPT'
#!/bin/sh
echo "$RENEWED_LINEAGE [$RENEWED_DOMAINS]" >> {QUOTED_SCRIPT_RUNS}
SCRIPT
chmod 755 "{QUOTED_SCRIPT}"
""")
        serve(WEBROOT)
        lets_encrypt_names_lead_to_this_guest()
        mark('the stand-ins on the guest: two Pebbles behind a certificate authority that the node trusts, a DNS server, and a web server')

        # The example project on a node without certbot: it picks the
        # package source and the timer's name by the node's facts, gets the
        # certificate through the web server, and certbot runs its script,
        # which reloads the web server.
        example = vm.ROOT / 'certbot-example'
        if example.exists():
            shutil.rmtree(example)
        shutil.copytree(vm.REPO / 'examples/certificate-with-certbot', example)
        vm.write_inventory(example, {'nodes': {'web': node}, 'vars': {
            'site_name': 'shop.example.com', 'certificate_account_email': ADMIN, 'web_server_service': WEB_SERVER, 'certificate_authority_url': AUTHORITY_A}}, name='example')
        # The dryrun cannot check the certificate while certbot is missing:
        # the package task before it would install certbot.
        preview = results(mmx_run('dryrun', ['configure'], example, None))['certificate']
        assert preview['status'] == 'not_checked' and preview['reason'].startswith(INSTALL), preview
        assert certificate('shop.example.com') is None and certbot_starts() == 0
        applied = results(mmx_run('run', ['configure'], example, None, timeout=900))['certificate']
        first = certificate('shop.example.com')
        assert has(applied, 'shop.example.com', ['shop.example.com', 'www.shop.example.com'], 'changed') and first['names'] == ['shop.example.com', 'www.shop.example.com'], (applied, first)
        assert applied['reason'] == (f'got a certificate for shop.example.com, www.shop.example.com from the certificate authority {AUTHORITY_A}: '
                                     'the node has no certificate named shop.example.com.'), applied
        version = tuple(int(part) for part in remote('certbot --version').split()[-1].split('.')[:2])
        print(f'certbot {version[0]}.{version[1]} on {distribution}', flush=True)
        assert remote(f'systemctl is-enabled {timer}; systemctl is-active {timer}') == 'enabled\nactive'
        assert lines(WEB_SERVER_RELOADS) == 1, 'certbot did not run the script after the first certificate'
        # certbot's own files, and the key only for root.
        assert remote("stat -c '%a %U' /etc/letsencrypt/live /etc/letsencrypt/archive/shop.example.com/privkey1.pem; readlink /etc/letsencrypt/live/shop.example.com/cert.pem") == \
            '700 root\n600 root\n../../archive/shop.example.com/cert1.pem'
        starts, log = certbot_starts(), certbot_log()
        assert starts == 1, starts
        for command in ('run', 'dryrun'):
            again = mmx_run(command, ['configure'], example, None)
            assert unchanged(again) and has(results(again)['certificate'], 'shop.example.com', ['shop.example.com', 'www.shop.example.com'], 'unchanged')
        assert certbot_log() == log and certificate('shop.example.com') == first and lines(WEB_SERVER_RELOADS) == 1, 'a rerun started certbot'
        # The timer stays off while the suite works: a renewal of its own
        # between two checks would hold certbot's lock.
        remote(f'systemctl stop {timer}')
        mark('the example certificate-with-certbot on a node without certbot: packages, script, certificate, and timer; certbot ran the script; '
             'a rerun and a dryrun start no certbot')

        # A dryrun goes on when an earlier node task of the same dryrun would
        # install the script that is missing, and says that a certificate
        # would be requested; without that node task it is not checked. The
        # dryrun writes no script and starts no certbot.
        new_script = '/usr/local/bin/mmx-test-script-of-a-dryrun'
        with_script = task('preview.example.com', ['preview.example.com'], run_after_issue_and_renewal=new_script)
        project([{'id': 'script', 'file': {'path': new_script, 'permissions_mode': '0755', 'content': '#!/bin/sh\n'}}, with_script])
        preview = results(run('dryrun'))
        assert preview['script']['change'] == 'would_change' and preview['certificate']['change'] == 'would_change' and preview['certificate']['reason'] == (
            f'would ask the certificate authority {AUTHORITY_A} for a certificate for preview.example.com: the node has no certificate named preview.example.com'), preview
        project([with_script])
        alone = results(run('dryrun'))['certificate']
        assert alone['status'] == 'not_checked' and alone['reason'] == (
            f'certbot_certificate: the script {new_script} of run_after_issue_and_renewal does not exist; install it in an earlier node task, '
            'for example with file and permissions_mode 0755'), alone
        assert remote(f'ls {new_script} 2>/dev/null || true') == '' and certbot_log() == log and certificate('preview.example.com') is None
        mark('a dryrun goes on when an earlier node task of the same dryrun would install the missing script, and is not checked without that node task')

        # The suite's own tasks from here on. The DNS plugin's package, and
        # the same certificate with the suite's script: only certbot's
        # renewal settings change, and no certbot starts for that.
        project(vm.package_tasks([plugin_package]) + [task(run_after_issue_and_renewal=SCRIPT)])
        settings = renewal_file()
        preview = results(run('dryrun'))['certificate']
        assert preview['change'] == 'would_change' and preview['reason'] == (
            'would write the task\'s settings into certbot\'s renewal settings of the certificate shop.example.com, for its next renewal: '
            f'run_after_issue_and_renewal: certbot has {EXAMPLE_SCRIPT}, and the task says {SCRIPT}. '
            'No new certificate is requested, and nobody tests these settings until certbot renews'), preview
        assert preview['file_change']['path'] == '/etc/letsencrypt/renewal/shop.example.com.conf' and f'+renew_hook = {SCRIPT}' in preview['file_change']['diff'], preview
        assert renewal_file() == settings
        written = results(run('run'))['certificate']
        assert has(written, 'shop.example.com', first['names'], 'changed') and written['reason'].startswith('wrote the task\'s settings into certbot\'s renewal settings'), written
        assert renewal_file() == settings.replace(f'renew_hook = {EXAMPLE_SCRIPT}', f'renew_hook = {SCRIPT}'), renewal_file()
        assert certbot_log() == log and certificate('shop.example.com') == first, 'changed settings started certbot or changed the certificate'
        for command in ('run', 'dryrun'):
            assert unchanged(run(command))
        # The next renewal uses what MMX wrote: the suite's script runs and
        # the example's does not.
        renewed = certbot_renews_now('shop.example.com')
        assert lines(SCRIPT_RUNS) == 1 and lines(WEB_SERVER_RELOADS) == 1
        # MMX never renews, and a certificate that certbot renewed is still
        # the task's certificate.
        assert unchanged(run('run')) and certificate('shop.example.com') == renewed
        mark('another script is written into certbot\'s renewal file, key by key, without a certbot; a dryrun shows the difference of the file; '
             'the next renewal runs the new script')

        # Another list of names: one certificate for exactly the new list,
        # in the same directory, also for a shorter list.
        for names in (['shop.example.com', 'www.shop.example.com', 'api.shop.example.com'], ['shop.example.com', 'api.shop.example.com']):
            before = certificate('shop.example.com')
            project([task(domains=names, run_after_issue_and_renewal=SCRIPT)])
            preview = results(run('dryrun'))['certificate']
            assert preview['change'] == 'would_change' and sorted(preview['value']['domains']) == sorted(names) and \
                ': the certificate shop.example.com covers ' in preview['reason'] and preview['reason'].endswith(f', and the task says {", ".join(names)}'), preview
            assert certificate('shop.example.com') == before
            changed = results(run('run'))['certificate']
            now = certificate('shop.example.com')
            assert has(changed, 'shop.example.com', names, 'changed') and now['names'] == sorted(names) and now['serial'] != before['serial'], (changed, now)
        assert remote('ls /etc/letsencrypt/live') == 'README\nshop.example.com', remote('ls /etc/letsencrypt/live')
        assert lines(SCRIPT_RUNS) == 3
        log = certbot_log()
        for command in ('run', 'dryrun'):
            assert unchanged(run(command))
        assert certbot_log() == log
        mark('another list of names gets a new certificate for exactly that list under the same name, for a longer and for a shorter list')

        # Changed settings of the challenge. The web server moves to the
        # other directory, so a renewal passes only with what MMX wrote; the
        # script's path needs quotes for certbot's shell and for its file.
        names = ['shop.example.com', 'api.shop.example.com']
        remote("sed -i '1i # a note of the administrator' /etc/letsencrypt/renewal/shop.example.com.conf")
        kept = [line for line in renewal_file().splitlines() if line.startswith(('#', 'version', 'archive_dir', 'cert', 'privkey', 'chain', 'fullchain', 'account', 'server', 'key_type'))]
        project([task(domains=names, webroot_path=OTHER_WEBROOT, run_after_issue_and_renewal=QUOTED_SCRIPT)])
        log, before = certbot_log(), certificate('shop.example.com')
        preview = results(run('dryrun'))['certificate']
        assert preview['change'] == 'would_change' and f'webroot_path: certbot has {WEBROOT}, and the task says {OTHER_WEBROOT}' in preview['reason'] and \
            f"run_after_issue_and_renewal: certbot has {SCRIPT}, and the task says '{QUOTED_SCRIPT}'" in preview['reason'], preview
        assert has(results(run('run'))['certificate'], 'shop.example.com', names, 'changed')
        settings = renewal_file()
        assert f'renew_hook = "\'{QUOTED_SCRIPT}\'"' in settings and f'webroot_path = {OTHER_WEBROOT},' in settings and WEBROOT not in settings, settings
        # certbot maps only the names whose challenge it answered. Pebble
        # reuses a valid authorization one time in a hundred even with
        # PEBBLE_AUTHZREUSE=0, as a real authority may, and a name without an
        # entry renews through webroot_path.
        mapped = settings.split('[[webroot_map]]\n', 1)[1].splitlines() if '[[webroot_map]]\n' in settings else []
        mapped = [line for line in itertools.takewhile(lambda line: not line.startswith('['), mapped) if ' = ' in line]
        assert all(line in [f'{name} = {OTHER_WEBROOT}' for name in names] for line in mapped), settings
        # Every line that the task does not name is as it was, among them
        # the authority, the account, and the administrator's note.
        assert [line for line in settings.splitlines() if line in kept] == kept, settings
        assert certbot_log() == log and certificate('shop.example.com') == before
        for command in ('run', 'dryrun'):
            assert unchanged(run(command))
        serve(OTHER_WEBROOT)
        certbot_renews_now('shop.example.com')
        assert lines(QUOTED_SCRIPT_RUNS) == 1 and lines(SCRIPT_RUNS) == 3
        assert unchanged(run('run')), 'certbot wrote its renewal file in a form that MMX reads as different'
        serve(WEBROOT)
        mark('another webroot directory and a script whose path needs quotes are written into the renewal file, every other line stays, '
             'and the next renewal passes with them')

        # standalone: certbot answers the authority itself on port 80, so
        # the web server is down meanwhile. The existing certificate changes
        # its challenge without a new certificate, and a renewal uses it.
        project([task(domains=names, challenge='standalone', webroot_path=None, run_after_issue_and_renewal=QUOTED_SCRIPT),
                 task('standalone.example.com', ['standalone.example.com'], id='standalone', challenge='standalone', webroot_path=None)])
        remote(f'systemctl stop {WEB_SERVER}')
        applied = results(run('run'))
        assert has(applied['certificate'], 'shop.example.com', names, 'changed') and 'challenge: certbot has webroot, and the task says standalone' in applied['certificate']['reason'], applied
        assert has(applied['standalone'], 'standalone.example.com', ['standalone.example.com'], 'changed') and certificate('standalone.example.com')['names'] == ['standalone.example.com']
        settings = renewal_file()
        assert 'authenticator = standalone' in settings and 'webroot' not in settings, settings
        certbot_renews_now('shop.example.com')
        assert unchanged(run('run'))
        serve(WEBROOT)
        mark('standalone: a certificate through certbot\'s own server on port 80, and an existing certificate changes its challenge without a new certificate')

        # dns. A plugin that certbot cannot load and a credentials file that
        # is not there are named before any request; a wildcard name needs
        # dns, which validation says when the project loads.
        project([task('wildcard.example.com', ['example.com', '*.example.com'], challenge='dns', webroot_path=None, dns_plugin='cloudflare',
                      dns_credentials_file='/etc/letsencrypt/cloudflare.ini')])
        starts = certbot_starts()
        # The message names the package; what may follow it says where
        # another distribution names it differently.
        missing = 'certbot_certificate: certbot\'s DNS plugin cloudflare is not installed on this node; install its package in an earlier node task: ' \
                  'python3-certbot-dns-cloudflare'
        refused = results(run('dryrun'))['certificate']
        assert refused['status'] == 'not_checked' and refused['reason'].startswith(missing), refused
        refused = results(run('run', 1))['certificate']
        assert refused['failure_kind'] == 'missing_prerequisite' and refused['reason'].startswith(missing), refused
        dns_task = task('wildcard.example.com', ['example.com', '*.example.com'], challenge='dns', webroot_path=None, dns_plugin='rfc2136',
                        dns_credentials_file=CREDENTIALS, wait_after_creating_dns_record='1s')
        project([dns_task])
        missing = f'certbot_certificate: the credentials file {CREDENTIALS} of dns_credentials_file does not exist; write it in an earlier node task, ' \
                  'for example with file and permissions_mode 0600'
        refused = results(run('dryrun'))['certificate']
        assert refused['status'] == 'not_checked' and refused['reason'] == missing, refused
        refused = results(run('run', 1))['certificate']
        assert refused['failure_kind'] == 'missing_prerequisite' and refused['reason'] == missing and certbot_starts() == starts, refused
        project([task('wildcard.example.com', ['example.com', '*.example.com'])])
        refused = vm.cmd([mmx, 'validate', 'configure'], cwd=workspace, check=False)
        assert refused.returncode != 0 and 'the wildcard name *.example.com needs challenge: dns, because a certificate authority checks a wildcard name only through DNS' in refused.stderr, refused
        credentials = {'id': 'credentials', 'file': {'path': CREDENTIALS, 'permissions_mode': '0600', 'content': (
            f'dns_rfc2136_server = 127.0.0.1\ndns_rfc2136_port = {DNS_PORT}\ndns_rfc2136_name = {DNS_KEY_NAME}\n'
            'dns_rfc2136_secret = {{ .secrets.dns_update_key }}\ndns_rfc2136_algorithm = HMAC-SHA256\n')}}
        project([credentials, dns_task], **secrets)
        applied = results(run('run'))['certificate']
        wildcard = certificate('wildcard.example.com')
        # The credentials file's task reads a secret; the certificate's task
        # gets only the file's path, so its result is not hidden.
        assert has(applied, 'wildcard.example.com', ['example.com', '*.example.com'], 'changed') and applied.get('secret') is not True and wildcard['names'] == ['*.example.com', 'example.com'], (applied, wildcard)
        settings = renewal_file('wildcard.example.com')
        assert 'authenticator = dns-rfc2136' in settings and f'dns_rfc2136_credentials = {CREDENTIALS}' in settings and 'dns_rfc2136_propagation_seconds = 1' in settings, settings
        for command in ('run', 'dryrun'):
            assert unchanged(run(command))
        # Another wait is a setting. Omitting it preserves the saved wait;
        # explicit removal restores the plugin's default.
        dns_task['certbot_certificate']['wait_after_creating_dns_record'] = '2s'
        project([credentials, dns_task], **secrets)
        changed = results(run('run'))['certificate']
        assert changed['change'] == 'changed' and 'wait_after_creating_dns_record: certbot has 1, and the task says 2' in changed['reason'] and \
            'dns_rfc2136_propagation_seconds = 2' in renewal_file('wildcard.example.com') and certificate('wildcard.example.com') == wildcard, changed
        certbot_renews_now('wildcard.example.com')
        del dns_task['certbot_certificate']['wait_after_creating_dns_record']
        project([credentials, dns_task], **secrets)
        assert unchanged(run('run'))
        assert 'dns_rfc2136_propagation_seconds = 2' in renewal_file('wildcard.example.com')
        dns_task['certbot_certificate']['renewal_settings_to_remove'] = ['wait_after_creating_dns_record']
        project([credentials, dns_task], **secrets)
        changed = results(run('run'))['certificate']
        assert changed['change'] == 'changed' and 'wait_after_creating_dns_record: certbot has 2, and renewal_settings_to_remove deletes it' in changed['reason'] and \
            'propagation_seconds' not in renewal_file('wildcard.example.com'), changed
        assert unchanged(run('run'))
        # The update key is in nothing that mmx printed or saved, and in no
        # log of certbot.
        saved = ''.join(path.read_text(errors='replace') for path in (workspace / '.mmx').rglob('*') if path.is_file())
        assert dns_secret not in ''.join(printed) and dns_secret not in saved, 'the DNS update key left its file task'
        vm.cmd([*ssh, 'sudo -n /bin/sh -c ' + shlex.quote(f'cat > {BASE}/dns-update-key')], input=dns_secret + '\n')
        found = remote(f'grep -rlF -f {BASE}/dns-update-key /var/lib/mmx /run/mmx /var/log/letsencrypt /etc/letsencrypt/renewal 2>/dev/null || true\n'
                       f'journalctl --no-pager -q -u mmx-https-api 2>/dev/null | grep -F -f {BASE}/dns-update-key || true')
        assert found == '', found
        mark('dns with the rfc2136 plugin: a wildcard certificate through a DNS record, the wait as a setting, a missing plugin and a missing '
             'credentials file named before any request; the DNS update key stays in its file')

        # Another certificate authority: a new certificate from it, which
        # certbot gives only when it is forced. certbot's settings then name
        # the new authority and its account.
        project([task(domains=names, challenge='standalone', webroot_path=None, run_after_issue_and_renewal=QUOTED_SCRIPT, certificate_authority_url=AUTHORITY_B)])
        before = certificate('shop.example.com')
        why = f'the certificate shop.example.com comes from the certificate authority {AUTHORITY_A}, and the task says {AUTHORITY_B}'
        preview = results(run('dryrun'))['certificate']
        assert preview['change'] == 'would_change' and preview['reason'] == f'would ask the certificate authority {AUTHORITY_B} for a certificate for {", ".join(names)}: {why}', preview
        assert certificate('shop.example.com') == before and f'server = {AUTHORITY_A}' in renewal_file()
        remote(f'systemctl stop {WEB_SERVER}')
        moved = results(run('run'))['certificate']
        after = certificate('shop.example.com')
        assert has(moved, 'shop.example.com', names, 'changed') and moved['reason'].endswith(why + '.') and after['issuer'] != before['issuer'] and after['names'] == before['names'], (moved, before, after)
        assert f'server = {AUTHORITY_B}' in renewal_file() and AUTHORITY_A not in renewal_file()
        for command in ('run', 'dryrun'):
            assert unchanged(run(command))
        mark('another certificate_authority_url gets a new certificate from that authority, and certbot\'s settings then name it')

        # The distribution's own renewal service renews the certificate with
        # what MMX wrote, and runs the script. The certificate of authority B
        # lives half a minute; the service is started until the certificate
        # is another one, because each certbot decides differently when a
        # certificate is due.
        # Without a terminal and without this option, certbot sleeps up to
        # eight minutes before it renews, with its lock held.
        assert '--no-random-sleep-on-renew' in remote(f'systemctl cat {renewal_service}'), f'the packaged {renewal_service} lets certbot sleep before it renews'
        runs = lines(QUOTED_SCRIPT_RUNS)
        started = time.monotonic()
        while certificate('shop.example.com')['serial'] == after['serial']:
            assert time.monotonic() - started < 150, 'the packaged renewal service did not renew a certificate that lives half a minute:\n' + \
                remote(f'journalctl --no-pager -q -u {renewal_service} | tail -n 30')
            remote(f'systemctl start {renewal_service}')
            time.sleep(2)
        how = f'the packaged {renewal_service}, {time.monotonic() - started:.0f} s after the first start'
        assert lines(QUOTED_SCRIPT_RUNS) == runs + 1 and f'server = {AUTHORITY_B}' in renewal_file()
        assert unchanged(run('run'))
        serve(WEBROOT)
        mark(f'a renewal by certbot itself uses the settings that MMX wrote and runs the script ({how})')

        # Refusals. A name that the authority refuses by policy: the task's
        # reason holds the authority's own answer from certbot's log, with
        # its kind, on every version of certbot. The task tries once, and
        # as often as its retry says.
        starts = certbot_starts()
        project([task('blocked', [BLOCKED])])
        refused = results(run('run', 1))['certificate']
        assert refused['status'] == 'failed' and refused['reason'] == (
            f'certbot_certificate: the certificate authority {AUTHORITY_A} refused the request for the certificate blocked: '
            f'Order included an identifier for which issuance is forbidden by policy: "{BLOCKED}" '
            '(error type urn:ietf:params:acme:error:rejectedIdentifier, HTTP status 400)'), refused
        assert 'tries' not in refused and certbot_starts() == starts + 1 and certificate('blocked') is None
        refused_task = task('blocked', [BLOCKED])
        refused_task['retry'] = {'max_tries': 2, 'pause_between_tries': '0s'}
        project([refused_task])
        refused = results(run('run', 1))['certificate']
        assert len(refused['tries']) == 2 and certbot_starts() == starts + 3, refused
        # A challenge that fails, here through a directory that no web
        # server serves: certbot's own words. Nothing is left half done, and
        # the next run with the right directory gets the certificate.
        project([task('later.example.com', ['later.example.com'], webroot_path=NOT_SERVED)])
        failed = results(run('run', 1))['certificate']
        assert failed['status'] == 'failed' and failed['reason'].startswith(f'certbot_certificate: certbot did not get the certificate later.example.com from {AUTHORITY_A}:') and \
            'Some challenges have failed.' in failed['reason'], failed
        assert certificate('later.example.com') is None and remote('ls /etc/letsencrypt/renewal /etc/letsencrypt/live | grep later || true') == ''
        project([task('later.example.com', ['later.example.com'])])
        assert has(results(run('run'))['certificate'], 'later.example.com', ['later.example.com'], 'changed')
        # The task explicitly chooses Let's Encrypt. The guard holds the
        # request on this guest, and the failure names the address.
        lets_encrypt_names_lead_to_this_guest()
        project([task('default.example.com', ['default.example.com'], certificate_authority_url=LETS_ENCRYPT)])
        preview = results(run('dryrun'))['certificate']
        assert preview['reason'].startswith(f'would ask the certificate authority {LETS_ENCRYPT} for a certificate for default.example.com'), preview
        held = results(run('run', 1))['certificate']
        assert held['status'] == 'failed' and held['reason'].startswith(f'certbot_certificate: certbot did not get the certificate default.example.com from {LETS_ENCRYPT}:') and \
            'acme-v02.api.letsencrypt.org' in held['reason'].split('\n', 1)[1], held
        assert certificate('default.example.com') is None
        mark('a refusal by the authority fails the task with the authority\'s own answer and its kind; a failed challenge leaves nothing and the next run '
             'succeeds; the task tries once unless it sets retry; with certificate_authority_url explicitly set to Let\'s Encrypt the task asks that authority, which this guest holds back')

        # A renewal file that certbot could not read: the task fails before
        # any request. certbot itself would get a certificate, put it into
        # later.example.com-0001, and report success.
        good = renewal_file('later.example.com')
        remote("sed -i '2i this line is no setting' /etc/letsencrypt/renewal/later.example.com.conf")
        project([task('later.example.com', ['later.example.com', 'www.later.example.com'])])
        log = certbot_log()
        for command, code in (('dryrun', 1), ('run', 1)):
            damaged = results(run(command, code))['certificate']
            assert damaged['status'] == 'failed' and damaged['reason'].startswith(
                'certbot_certificate: MMX cannot read the renewal file /etc/letsencrypt/renewal/later.example.com.conf for certificate '
                'later.example.com: line 2 (this line is no setting): the line is neither a setting (key = value), a section, nor a comment.'), damaged
        assert certbot_log() == log and remote('ls /etc/letsencrypt/live | grep 0001 || true') == ''
        remote("sed -i '2d' /etc/letsencrypt/renewal/later.example.com.conf")
        assert renewal_file('later.example.com') == good
        mark('a renewal file that certbot could not read fails the task before any request, with the file and its line')

        # Another certbot holds certbot's lock, as a renewal service that
        # sleeps before it renews does: here a real certbot whose script
        # waits until the suite lets it go. A task waits for it, never
        # longer than its own time limit, and then makes its one try.
        remote(f"""systemd-run --quiet --unit mmx-test-other-certbot certbot certonly --non-interactive --agree-tos --email {ADMIN} --no-eff-email --server {AUTHORITY_A} \\
  --cert-name holder.example.com --webroot --webroot-path {WEBROOT} -d holder.example.com --disable-hook-validation \\
  --deploy-hook 'touch {BASE}/lock-is-held; while [ -e {BASE}/lock-is-held ]; do sleep 0.2; done'
for wait in $(seq 1 150); do [ -e {BASE}/lock-is-held ] && break; sleep 0.2; done
test -e {BASE}/lock-is-held && test -e {CERTBOT_LOCK}""")
        starts = certbot_starts()
        waiting = task('waits.example.com', ['waits.example.com'])
        waiting['time_limit'] = '3s'
        project([waiting])
        gave_up = results(run('run', 1))['certificate']
        assert gave_up['status'] == 'failed' and gave_up['change'] == 'unchanged' and re.match(
            r"time_limit exceeded: certbot_certificate: the node task reached its time limit after waiting \ds for another certbot process \(lock file "
            + re.escape(CERTBOT_LOCK) + r'\), so the certbot_certificate node task did not start certbot\.', gave_up['reason']), gave_up
        assert certbot_starts() == starts and certificate('waits.example.com') is None
        project([task('waits.example.com', ['waits.example.com'])])
        # The suite lets the other certbot go only when the task has looked
        # at certbot's lock, which the watcher sees as an open of the lock
        # file. The task found the lock held then, so it waits, however fast
        # or slow the server is; its next look is a second later.
        watcher_failed = '{ journalctl --no-pager -q -u mmx-test-lock-watcher | tail -n 5; exit 1; }'
        remote(f"""rm -f {BASE}/lock-watch-is-ready {BASE}/lock-was-looked-at
systemd-run --quiet --unit mmx-test-lock-watcher python3 {PEBBLE}/watch_for_open.py {CERTBOT_LOCK} {BASE}/lock-watch-is-ready {BASE}/lock-was-looked-at 120
for wait in $(seq 1 100); do [ -e {BASE}/lock-watch-is-ready ] && break; sleep 0.1; done
test -e {BASE}/lock-watch-is-ready || {watcher_failed}""")
        waited = {}
        runner = threading.Thread(target=lambda: waited.update(record=run('run')))
        runner.start()
        remote(f"""for wait in $(seq 1 600); do [ -e {BASE}/lock-was-looked-at ] && break; sleep 0.2; done
test -e {BASE}/lock-was-looked-at || {watcher_failed}""")
        assert runner.is_alive() and certificate('waits.example.com') is None, 'the task did not wait for the other certbot'
        remote(f'rm -f {BASE}/lock-is-held')
        runner.join(timeout=120)
        got = results(waited['record'])['certificate']
        seconds = re.search(r' The task waited (\d+)s until another certbot had ended\.$', got['reason'])
        assert has(got, 'waits.example.com', ['waits.example.com'], 'changed') and seconds and int(seconds.group(1)) >= 1, got
        mark('while another certbot holds certbot\'s lock, a task waits and then gets its certificate, and says how long it waited; '
             'the task\'s own time limit ends the wait')

        # state: absent deletes the certificate, its key, and its renewal
        # settings, and nothing of another certificate. A certbot older than
        # 2.6 keeps a copy of each key outside the certificate's own
        # directories, which certbot delete leaves; MMX removes the copies
        # of the deleted certificate's keys.
        def keys(pattern):
            """The public keys of the private key files that match pattern, one line each."""
            return set(remote(f'for key in {pattern}; do [ -e "$key" ] && openssl pkey -in "$key" -pubout -outform DER | sha256sum | cut -d" " -f1; done || true').split())

        old_certbot = remote('test -d /etc/letsencrypt/keys && echo yes || true') == 'yes'
        assert old_certbot == (version < (2, 6)), 'certbot keeps copies of keys in /etc/letsencrypt/keys only before 2.6'
        of_shop, of_others = keys('/etc/letsencrypt/archive/shop.example.com/privkey*.pem'), keys('/etc/letsencrypt/archive/later.example.com/privkey*.pem')
        assert len(of_shop) > 1 and of_others
        if old_certbot:
            assert of_shop <= keys('/etc/letsencrypt/keys/*') and of_others <= keys('/etc/letsencrypt/keys/*')
        absent = [{'id': 'certificate', 'certbot_certificate': {'certificate_name': 'shop.example.com', 'state': 'absent'}}]
        project(absent)
        log, sent = certbot_log(), requests_sent()
        preview = results(run('dryrun'))['certificate']
        assert preview['change'] == 'would_change' and preview['reason'] == 'would delete the certificate shop.example.com, its key, and its renewal settings', preview
        assert certbot_log() == log and certificate('shop.example.com')
        deleted = results(run('run'))['certificate']
        assert deleted['change'] == 'changed' and deleted['reason'].startswith('deleted the certificate shop.example.com, its key, and its renewal settings.'), deleted
        assert remote('ls -d /etc/letsencrypt/live/shop.example.com /etc/letsencrypt/archive/shop.example.com /etc/letsencrypt/renewal/shop.example.com.conf 2>/dev/null || true') == ''
        # certbot delete asks no authority.
        assert requests_sent() == sent
        if old_certbot:
            left = keys('/etc/letsencrypt/keys/*')
            assert not (of_shop & left) and of_others <= left, 'the copies of the deleted certificate\'s keys are still there, or another certificate lost its own'
            # Each key copy has its certificate request beside it, and both went.
            assert remote('ls /etc/letsencrypt/keys | wc -l') == remote('ls /etc/letsencrypt/csr | wc -l') == str(len(left))
            assert 'copies of its keys and certificate requests that this older certbot keeps' in deleted['reason'], deleted
        assert certificate('later.example.com') and keys('/etc/letsencrypt/archive/later.example.com/privkey*.pem') == of_others
        for command in ('run', 'dryrun'):
            assert unchanged(run(command))
        mark('state: absent deletes the certificate, its key, and its renewal settings, and no other certificate'
             + (', and the copies of its keys that this older certbot keeps' if old_certbot else '; this certbot keeps no copies of keys'))

        # A removal that stopped part of the way: certbot removes the renewal
        # file first, so the certificate's two directories, with its key,
        # are left without it. A task that wants the certificate fails then
        # and names them, before any request; state: absent removes them
        # itself, with the key copies of an older certbot.
        left_behind = '/etc/letsencrypt/live/later.example.com, /etc/letsencrypt/archive/later.example.com'
        remote('rm /etc/letsencrypt/renewal/later.example.com.conf')
        project([task('later.example.com', ['later.example.com'])])
        sent = requests_sent()
        unfinished = results(run('run', 1))['certificate']
        assert unfinished['status'] == 'failed' and unfinished['reason'].startswith(
            'certbot_certificate: /etc/letsencrypt/live/later.example.com holds files, but the renewal file '
            '/etc/letsencrypt/renewal/later.example.com.conf is missing after an unfinished removal or certificate request.'), unfinished
        assert requests_sent() == sent
        project([{'id': 'certificate', 'certbot_certificate': {'certificate_name': 'later.example.com', 'state': 'absent'}}])
        preview = results(run('dryrun'))['certificate']
        assert preview['change'] == 'would_change' and preview['reason'] == (
            'would remove what an unfinished removal or request of the certificate later.example.com left on the node: ' + left_behind), preview
        assert remote('ls -d /etc/letsencrypt/live/later.example.com /etc/letsencrypt/archive/later.example.com | wc -l') == '2'
        log = certbot_log()
        finished = results(run('run'))['certificate']
        assert finished['change'] == 'changed' and finished['reason'].startswith(
            'removed what an unfinished removal or request of the certificate later.example.com left on the node: ' + left_behind + '.'), finished
        assert remote('ls -d /etc/letsencrypt/live/later.example.com /etc/letsencrypt/archive/later.example.com /etc/letsencrypt/live/.mmx-staging-* '
                      '/etc/letsencrypt/archive/.mmx-staging-* 2>/dev/null || true') == ''
        # No certbot was needed for it.
        assert certbot_log() == log
        if old_certbot:
            assert not (of_others & keys('/etc/letsencrypt/keys/*')), 'the copies of the keys of the unfinished removal are still there'
        for command in ('run', 'dryrun'):
            assert unchanged(run(command))
        mark('state: absent finishes a removal that stopped after certbot removed the renewal file; a task that wants the certificate fails in that state and names the directories')

        # Every host that certbot contacted in this suite is the guest.
        hosts = hosts_that_certbot_contacted()
        assert {host.rsplit(':', 1)[0] if host.count(':') == 1 else host for host in hosts} == {'localhost', 'acme-v02.api.letsencrypt.org'} and \
            {'localhost:14000', 'localhost:14100'} <= set(hosts), hosts
        print('Hosts that certbot contacted, with the lines of its log: ' + ', '.join(f'{host} ({count})' for host, count in sorted(hosts.items()))
              + "; each resolves only to this guest, also Let's Encrypt's name, which /etc/hosts maps to it", flush=True)
        mark('every host that certbot contacted is this guest')

        # On a guest with SELinux: nothing of the suite ran into a denial.
        # BIND is the one program here that SELinux confines; certbot and a
        # certificate's script run unconfined, from a task and from the
        # packaged renewal service.
        if selinux:
            denied = selinux_denials()
            print(f'SELinux is {selinux} on this guest and denied this during the suite: ' + (json.dumps(denied, indent=1) if denied else 'nothing'), flush=True)
            assert not [line for line in denied if 'named_t' in line or 'certbot' in line or 'pebble' in line], denied
            mark('SELinux denied nothing to the DNS server, to certbot, or to Pebble')
    finally:
        failing = sys.exc_info()[0] is not None
        vm.ENV.pop('MMX_TEST_DNS_UPDATE_KEY', None)
        if failing:
            # What certbot contacted is known also when a case failed, and
            # what SELinux denied can be the cause.
            try:
                print('Hosts that certbot contacted before the failure: ' + json.dumps(hosts_that_certbot_contacted()), flush=True)
            except Exception as error:
                print(f'The list of hosts that certbot contacted could not be read: {error}', flush=True)
            if selinux:
                try:
                    print(f'SELinux is {selinux} on this guest and denied this before the failure: ' + json.dumps(selinux_denials(), indent=1), flush=True)
                except Exception as error:
                    print(f'What SELinux denied could not be read: {error}', flush=True)
        cleanup()
        if not failing:
            guest_is_as_found()
    (workspace / 'summary.json').write_text(json.dumps(passed, indent=2) + '\n')
    print(f'{len(passed)} certbot workflows passed', flush=True)


if __name__ == '__main__':
    vm.run_suite(main)
