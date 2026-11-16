#!/usr/bin/env python3
"""Pinned HTTPS artifacts using a Linux controller and two real node jobs."""
from collections import Counter
import hashlib
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import bz2
import gzip
import json
import lzma
import os
from pathlib import Path
import shlex
import shutil
import ssl
import subprocess
import tempfile
import threading
import datetime
import time

import vm


def main():
    for name in ('mmx-dev', 'mmx-peer'):
        vm.up(name)
        vm.reset(name)
    nodes = {name: vm.connection(name) for name in ('mmx-dev', 'mmx-peer')}
    homes = {name: vm.guest('sh', '-c', 'printf %s "$HOME"', name=name).stdout for name in nodes}
    vm.build()
    architecture = vm.architecture()
    binary = vm.ROOT / f'bin/mmx-linux-{architecture}'
    subprocess.run(['go', 'build', '-trimpath', vm.release_version_flag(), '-o', binary, './cmd/mmx'], cwd=vm.REPO,
                   env=dict(os.environ, CGO_ENABLED='0', GOOS='linux', GOARCH=architecture), check=True)
    base = '/var/tmp/mmx-downloads'
    service = 'mmx-download-demo.service'
    destination = '/usr/local/bin/mmx-download-demo'
    passed = []
    forward = None
    server = None
    key_line = None

    def remote(name, script, check=True):
        return vm.cmd([*nodes[name][1], 'sudo -n /bin/sh -c ' + shlex.quote('set -eu\n' + script)], check=check)

    def mark(name):
        passed.append(name)
        print('PASS ' + name, flush=True)

    def cleanup_nodes():
        for name in nodes:
            script = (f'systemctl disable --now {service} 2>/dev/null || true\n'
                      f'systemctl reset-failed {service} 2>/dev/null || true\n'
                      f'rm -f /etc/systemd/system/{service} {destination}\nsystemctl daemon-reload\n'
                      f'rm -rf {base}\n')
            script += 'rm -f /var/tmp/mmx-download-block\n'
            if key_line:
                auth = homes[name] + '/.ssh/authorized_keys'
                # Only remove this suite's ephemeral public key.
                script += f"sed -i '\\|{key_line}|d' {shlex.quote(auth)}\n"
            remote(name, script)

    cleanup_nodes()
    with tempfile.TemporaryDirectory(prefix='downloads-', dir=vm.ROOT) as tmp:
        work = Path(tmp)
        try:
            vm.cmd(['ssh-keygen', '-q', '-t', 'ed25519', '-N', '', '-f', work / 'identity'])
            key_line = (work / 'identity.pub').read_text().split()[1]
            for name in nodes:
                auth = shlex.quote(homes[name] + '/.ssh/authorized_keys')
                remote(name, f'printf "%s\\n" {shlex.quote((work / "identity.pub").read_text().strip())} >> {auth}')
            # The certificate starts a day ago, so that a node whose clock is
            # behind, such as a VM, does not call it not yet valid.
            now = datetime.datetime.now(datetime.timezone.utc)
            fmt = '%Y%m%d%H%M%SZ'
            vm.cmd(['openssl', 'req', '-x509', '-newkey', 'rsa:2048', '-nodes',
                    '-not_before', (now - datetime.timedelta(days=1)).strftime(fmt),
                    '-not_after', (now + datetime.timedelta(days=2)).strftime(fmt),
                    '-subj', '/CN=MMX download fixture', '-addext', 'subjectAltName=IP:127.0.0.1',
                    '-keyout', work / 'tls.key', '-out', work / 'tls.crt'])
            artifact = work / 'artifact'
            # The program that the tasks download and run as a service: a
            # sleep of the suite's own. A distribution's /usr/bin/sleep may
            # be a program that picks its tool by the name it was started
            # with, as Rust coreutils' on Ubuntu 26.04 does; under the
            # service's name it would not sleep.
            sleep_source = work / 'sleep'
            sleep_source.mkdir()
            (sleep_source / 'go.mod').write_text('module sleep\n\ngo 1.21\n')
            (sleep_source / 'main.go').write_text('package main\n\nimport "time"\n\nfunc main() {\n\tfor {\n\t\ttime.Sleep(time.Hour)\n\t}\n}\n')
            subprocess.run(['go', 'build', '-trimpath', '-o', artifact, '.'], cwd=sleep_source,
                           env=dict(os.environ, CGO_ENABLED='0', GOOS='linux', GOARCH=architecture), check=True)
            with artifact.open('r+b') as output:
                output.truncate(18 << 20)
            counts = Counter()
            online = True
            started = threading.Event()
            # The artifact as a project publishes it compressed, made when a
            # request asks for it and kept while the artifact is unchanged.
            compressed_cache = {}

            def compressed(extension):
                data = artifact.read_bytes()
                key = (extension, hashlib.sha256(data).hexdigest())
                if key not in compressed_cache:
                    if extension == 'zst':
                        compressed_cache[key] = subprocess.run(['zstd', '-q', '-c'], input=data, stdout=subprocess.PIPE, check=True).stdout
                    else:
                        compressed_cache[key] = {'gz': gzip.compress, 'bz2': bz2.compress, 'xz': lzma.compress}[extension](data)
                return compressed_cache[key]

            class Handler(BaseHTTPRequestHandler):
                def log_message(self, *args):
                    pass

                def do_GET(self):
                    counts[self.path] += 1
                    if not online:
                        self.send_error(503)
                        return
                    if self.path in ('/redirect', '/loop', '/downgrade'):
                        self.send_response(302)
                        self.send_header('Location', {'/redirect': '/artifact', '/loop': '/loop', '/downgrade': 'http://127.0.0.1:1/artifact'}[self.path])
                        self.end_headers()
                        return
                    if self.path == '/stall':
                        self.send_response(200)
                        self.end_headers()
                        self.wfile.write(b'partial')
                        self.wfile.flush()
                        started.set()
                        time.sleep(35)
                        return
                    if self.path == '/partial':
                        self.send_response(200)
                        self.send_header('Content-Length', '1000')
                        self.end_headers()
                        self.wfile.write(b'partial')
                        return
                    if self.path.startswith('/artifact.'):
                        body = compressed(self.path.rsplit('.', 1)[1])
                        self.send_response(200)
                        self.send_header('Content-Length', str(len(body)))
                        self.end_headers()
                        self.wfile.write(body)
                        return
                    if self.path != '/artifact':
                        self.send_error(404)
                        return
                    self.send_response(200)
                    self.send_header('Content-Length', str(artifact.stat().st_size))
                    self.end_headers()
                    with artifact.open('rb') as source:
                        shutil.copyfileobj(source, self.wfile, 65536)

            server = ThreadingHTTPServer(('127.0.0.1', 0), Handler)
            context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
            context.load_cert_chain(work / 'tls.crt', work / 'tls.key')
            server.socket = context.wrap_socket(server.socket, server_side=True)
            threading.Thread(target=server.serve_forever, daemon=True).start()

            # The Linux controller uses normal SSL_CERT_FILE trust. No product
            # insecure-TLS switch or change to the controller's trust store is needed.
            ports = json.loads(vm.guest('python3', '-c',
                'import socket,json; ss=[socket.socket() for _ in range(3)]; '
                '[s.bind(("127.0.0.1",0)) for s in ss]; print(json.dumps([s.getsockname()[1] for s in ss]))').stdout)
            source_port, peer_ssh, peer_https = ports
            args = [*vm.unshared(nodes['mmx-dev'][1][:-1]), '-N', '-o', 'ExitOnForwardFailure=yes']
            # The peer's SSH is the peer's own address; its HTTPS is the
            # controller machine's forward to the peer.
            peer = nodes['mmx-peer'][0]
            for guest_port, address, port in [(source_port, '127.0.0.1', server.server_port),
                                              (peer_ssh, peer['address'], peer['ssh_port']),
                                              (peer_https, '127.0.0.1', int(peer['https_address'].rsplit(':', 1)[1]))]:
                args += ['-R', f'127.0.0.1:{guest_port}:{address}:{port}']
            forward = subprocess.Popen([str(a) for a in [*args, nodes['mmx-dev'][1][-1]]],
                                       stdout=subprocess.DEVNULL, stderr=subprocess.PIPE, env=vm.ENV)
            deadline = time.monotonic() + 5
            probe = shlex.quote(f'import socket; socket.create_connection(("127.0.0.1",{source_port}),1).close()')
            while remote('mmx-dev', 'python3 -c ' + probe, check=False).returncode:
                if forward.poll() is not None or time.monotonic() > deadline:
                    raise RuntimeError('download fixture tunnel did not start')
                time.sleep(.05)
            guest_user = nodes['mmx-dev'][0]['ssh_user']
            remote('mmx-dev', f'mkdir -p {base}/bin\nchown -R {guest_user} {base}')
            for local, name in [(binary, 'bin/mmx'), (vm.ROOT / f'bin/mmx-runner-linux-{architecture}', f'bin/mmx-runner-linux-{architecture}'),
                                (work / 'identity', 'identity'), (work / 'tls.crt', 'tls.crt')]:
                vm.copy(local, base + '/' + name)
            known = []
            inventory = {'nodes': {}}
            for name, port, endpoint in [('mmx-dev', 22, 7443), ('mmx-peer', peer_ssh, peer_https)]:
                key = vm.console('cat /etc/ssh/ssh_host_ed25519_key.pub', name).strip()
                known.append(('127.0.0.1' if port == 22 else f'[127.0.0.1]:{port}') + ' ' + key)
                inventory['nodes'][name] = {'address': '127.0.0.1', 'ssh_port': port, 'ssh_user': nodes[name][0]['ssh_user'],
                    'ssh_identity_file': base + '/identity', 'ssh_known_hosts': base + '/known_hosts',
                    'ssh_config': '/dev/null', 'https_address': f'https://127.0.0.1:{endpoint}', 'connection': 'ssh_only'}

            def put(name, content):
                (work / name).parent.mkdir(parents=True, exist_ok=True)
                vm.guest('mkdir', '-p', str(Path(base) / Path(name).parent))
                (work / name).write_text(content)
                vm.copy(work / name, base + '/' + name)

            put('known_hosts', '\n'.join(known) + '\n')
            put('inventories/example.yml', json.dumps(inventory))
            put('mmx_project_settings.yml', (vm.REPO / 'examples/download/mmx_project_settings.yml').read_text())
            original = (vm.REPO / 'examples/download/playbooks/install.yml').read_text()
            url = f'https://127.0.0.1:{source_port}'

            def digest():
                with artifact.open('rb') as source:
                    return hashlib.file_digest(source, 'sha256').hexdigest()

            def project(pin=None, path='/artifact', text=None):
                text = text or original
                text = text.replace('https://downloads.example.com/sleep-linux-arm64', url + path)
                text = text.replace('0' * 64, pin or digest())
                put('playbooks/install.yml', text)

            def command(action, *args, trust=True):
                prefix = f'cd {base} && '
                if trust:
                    prefix += f'SSL_CERT_FILE={base}/tls.crt '
                return prefix + shlex.join([base + '/bin/mmx', action, *vm.playbook_args(action, 'install'), '--json', *args])

            def run(action, expected=None, *args, trust=True):
                result = vm.cmd([*nodes['mmx-dev'][1], command(action, *args, trust=trust)], check=False)
                assert result.returncode in (vm.success_codes(action) if expected is None else (expected,)), (action, result.stdout, result.stderr)
                return json.loads(result.stdout) if result.stdout else result

            def state():
                result = {}
                for name in nodes:
                    pid = remote(name, f'systemctl show -p MainPID --value {service}').stdout.strip()
                    installed = remote(name, f'sha256sum {destination}').stdout.split()[0]
                    loaded = remote(name, f'sha256sum /proc/{pid}/exe').stdout.split()[0]
                    inode = remote(name, f'stat -c %i:%a:%U:%G {destination}').stdout.strip()
                    result[name] = (pid, installed, loaded, inode)
                return result

            def assert_fetched(before, times):
                assert counts['/artifact'] == before + times, counts

            def runs_with_the_kept_copy(text):
                """Runs the playbook text with a node task that prints the
                inodes of the node's kept copy and of the job's file: equal
                when the node kept the file and the job uses it, not a copy
                that the controller sent."""
                kept = f'/var/lib/mmx/kept_files/{digest()}'
                put('playbooks/install.yml', text + f"""      - id: kept_inodes
        shell:
          script: stat -c %i {kept} /var/lib/mmx/uploads/*/{digest()}
          dryrun: stat -c %i {kept} /var/lib/mmx/uploads/*/{digest()}
          changed_when: false
""")
                for entry in run('run')['entries']:
                    inodes = [r for r in entry['response']['results'] if r['task'].endswith('kept_inodes')][0]['stdout'].split()
                    assert len(inodes) == 2 and inodes[0] == inodes[1], (entry['node'], inodes)

            def forget_saved_downloads():
                # A pinned file that an earlier run saved is reused; a case
                # that tests the fetch itself starts without it.
                remote('mmx-dev', f'rm -f {base}/.mmx/runs/*/assets/{digest()}')

            project()
            before = counts['/artifact']
            run('validate')
            run('explain')
            assert counts['/artifact'] == before
            for connection in ('ssh_only', 'https_with_ssh_fallback'):
                for h in inventory['nodes'].values():
                    h['connection'] = connection
                put('inventories/example.yml', json.dumps(inventory))
                before = counts['/artifact']
                run('dryrun')
                assert_fetched(before, 1 if connection == 'ssh_only' else 0)
                if connection == 'ssh_only':
                    for name in nodes:
                        remote(name, f'test ! -e {destination}')
                before = counts['/artifact']
                applied = run('run')
                assert_fetched(before, 0)
                assert len(applied['entries']) == 2
                assert all(e['connection_used'] == ('ssh' if connection == 'ssh_only' else 'https') for e in applied['entries'])
                actual = state()
                for observed in actual.values():
                    assert observed[1:3] == (digest(), digest()), observed
                    assert observed[3].endswith(':755:root:root'), observed
                before = counts['/artifact']
                unchanged = run('run')
                assert_fetched(before, 0)
                assert all(e['response']['change'] == 'unchanged' for e in unchanged['entries'])
                assert state() == actual
                # Each node kept the file from the first run, and a later run
                # uses the kept copy.
                project()
                runs_with_the_kept_copy((work / 'playbooks/install.yml').read_text())
                project()
                folder = base + '/.mmx/runs/' + applied['id']
                remote('mmx-dev', f'test "$(find {folder}/assets -type f | wc -l)" = 1\n'
                                  f'test "$(wc -c < {folder}/run.json)" -lt 32768')
                mark(connection + ': one 18 MiB fetch for two nodes, reused by later runs without a fetch, dryrun, candidate validation, installed/running hashes, permissions, unchanged inode/process; '
                     'each node keeps the file, and a later run sends it no bytes')

            actual = state()
            # The example already has a candidate validator; reject a changed
            # artifact there and prove live bytes/processes stay untouched.
            with artifact.open('r+b') as output:
                output.seek(-1, 2)
                output.write(b'B')
            project(text=original.replace('script: test -s "$NEW_FILE"', 'script: exit 1'))
            run('run', 1)
            assert state() == actual
            mark('rejected candidate preserves installed and running executable')

            # Install B, then fail before restart. Resume must finish the original
            # playbook and frozen bytes after both local declarations and source change.
            for name in nodes:
                remote(name, 'touch /var/tmp/mmx-download-block')
            failing = original.replace('          activate:\n',
                '          validate:\n            - shell:\n                script: test ! -e /var/tmp/mmx-download-block\n          activate:\n')
            project(text=failing)
            failed = run('run', 1)
            installed = state()
            for name, observed in installed.items():
                assert observed[1] == digest() and observed[2] == actual[name][2]
                assert observed[0] == actual[name][0]
                remote(name, 'rm /var/tmp/mmx-download-block')
            frozen = digest()
            with artifact.open('r+b') as output:
                output.seek(-1, 2)
                output.write(b'C')
            project()
            online = False
            before = counts.copy()
            run('resume', 0, failed['id'])
            recovered = state()
            assert counts == before
            for name, observed in recovered.items():
                assert observed[1:3] == (frozen, frozen) and observed[0] != actual[name][0], observed
            run('run', 4)
            assert state() == recovered
            online = True
            before = counts['/artifact']
            run('run')
            assert_fetched(before, 1)
            current = state()
            assert all(s[1:3] == (digest(), digest()) for s in current.values())
            run('run')
            assert state() == current
            mark('failure before activation, offline frozen resume, and new run loading the current release exactly once')

            # Preparation errors cannot create a node job or retain partial assets.
            jobs = {name: remote(name, 'find /var/lib/mmx/jobs -mindepth 1 -maxdepth 1 | sort').stdout for name in nodes}
            runs = remote('mmx-dev', f'find {base}/.mmx/runs -mindepth 1 -maxdepth 1 | sort').stdout
            for path, pin, trust in [('/artifact', '0' * 64, True), ('/partial', None, True),
                                     ('/downgrade', None, True), ('/loop', None, True), ('/artifact', None, False)]:
                project(pin=pin, path=path)
                forget_saved_downloads()
                run('run', 4, trust=trust)
            assert remote('mmx-dev', f'find {base}/.mmx/runs -mindepth 1 -maxdepth 1 | sort').stdout == runs
            for name in nodes:
                assert remote(name, 'find /var/lib/mmx/jobs -mindepth 1 -maxdepth 1 | sort').stdout == jobs[name]
            assert state() == current
            project(path='/redirect')
            forget_saved_downloads()
            before = counts['/artifact']
            run('run')
            assert_fetched(before, 1)
            assert state() == current
            # Refresh saved run list after the successful redirect case.
            runs = remote('mmx-dev', f'find {base}/.mmx/runs -mindepth 1 -maxdepth 1 | sort').stdout
            jobs = {name: remote(name, 'find /var/lib/mmx/jobs -mindepth 1 -maxdepth 1 | sort').stdout for name in nodes}
            project(path='/stall')
            forget_saved_downloads()
            started.clear()
            proc = subprocess.Popen([str(a) for a in [*nodes['mmx-dev'][1], command('run')]],
                                    stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True, env=vm.ENV)
            assert started.wait(10)
            # Signal the controller itself, not its SSH display process.
            remote('mmx-dev', f'pkill -INT -f "^{base}/bin/mmx run install --json$"')
            out, err = proc.communicate(timeout=10)
            assert proc.returncode != 0 and 'context canceled' in err, (out, err)
            project(path='/stall')
            forget_saved_downloads()
            start = time.monotonic()
            stalled = run('run', 4)
            assert 'stalled' in stalled.stderr and time.monotonic() - start < 40, stalled
            assert remote('mmx-dev', f'find {base}/.mmx/runs -mindepth 1 -maxdepth 1 | sort').stdout == runs
            for name in nodes:
                assert remote(name, 'find /var/lib/mmx/jobs -mindepth 1 -maxdepth 1 | sort').stdout == jobs[name]
            assert state() == current
            mark('HTTPS redirects, bad pins/TLS/partial streams/redirects, Ctrl-C and idle timeout; no jobs, live changes, or partial assets on preparation failure')

            # dangerously_skip_checksum: true takes the place of the SHA-256: the
            # file is fetched and installed as it arrives.
            pinned = 'sha256: "' + '0' * 64 + '"'
            assert pinned in original
            project(text=original.replace(pinned, 'dangerously_skip_checksum: true'))
            before = counts['/artifact']
            run('run')
            assert_fetched(before, 1)
            assert state() == current
            project(text=original.replace(pinned, pinned + '\n                dangerously_skip_checksum: true'))
            refused = run('run', 4)
            assert 'takes sha256 or dangerously_skip_checksum: true, not both' in refused.stderr, refused
            assert '                ' + pinned + '\n' in original
            project(text=original.replace('                ' + pinned + '\n', ''))
            refused = run('run', 4)
            assert 'download.sha256 is required' in refused.stderr and 'dangerously_skip_checksum: true' in refused.stderr, refused
            assert_fetched(before, 1)
            mark('dangerously_skip_checksum: true installs the file as it arrives; a download with both or with neither is refused before any fetch')

            # The web address and the SHA-256 may read values the controller
            # machine knows before the run, such as a version in the project's vars.
            settings = (vm.REPO / 'examples/download/mmx_project_settings.yml').read_text()
            assert 'vars:' not in settings
            variable = original.replace('https://downloads.example.com/sleep-linux-arm64', url + '/{{ .vars.artifact_name }}').replace('0' * 64, '{{ .vars.artifact_sha256 }}')
            assert variable != original
            put('mmx_project_settings.yml', settings + f'vars:\n  artifact_name: artifact\n  artifact_sha256: "{digest()}"\n')
            put('playbooks/install.yml', variable)
            forget_saved_downloads()
            before = counts['/artifact']
            run('run')
            assert_fetched(before, 1)
            assert state() == current
            # The node keeps a file whose web address a template gives too,
            # which the job that the node receives does not carry: without
            # its earlier copy, a run sends it, and the node keeps it.
            for name in nodes:
                remote(name, f'rm -f /var/lib/mmx/kept_files/{digest()}')
            runs_with_the_kept_copy(variable)
            runs_with_the_kept_copy(variable)
            put('mmx_project_settings.yml', settings + 'vars:\n  artifact_name: artifact\n  artifact_sha256: "' + 'f' * 64 + '"\n')
            refused = run('run', 4)
            assert 'SHA-256' in refused.stderr or 'sha256' in refused.stderr.lower(), refused
            assert state() == current
            put('mmx_project_settings.yml', settings)
            mark('a web address and SHA-256 that read vars fetch and verify the resolved file, which the node keeps; a wrong resolved SHA-256 is refused')

            # A tool published as one compressed file: sha256 is the digest
            # of the compressed file, and the node gets the decompressed one.
            def decompressing(extension, decompress, pin=None):
                text = original.replace('https://downloads.example.com/sleep-linux-arm64', f'{url}/artifact.{extension}')
                text = text.replace('0' * 64, pin or hashlib.sha256(compressed(extension)).hexdigest())
                text = text.replace('                path: /usr/local/bin/mmx-download-demo\n',
                                    f'                decompress: {decompress}\n                path: /usr/local/bin/mmx-download-demo\n')
                put('playbooks/install.yml', text)

            formats = [('gz', 'gzip'), ('bz2', 'bzip2'), ('xz', 'xz'), ('zst', 'zstd')]
            for extension, decompress in formats:
                decompressing(extension, decompress)
                before = counts[f'/artifact.{extension}']
                unchanged = run('run')
                assert counts[f'/artifact.{extension}'] == before + 1, counts
                assert all(e['response']['change'] == 'unchanged' for e in unchanged['entries']), unchanged
                assert state() == current
                folder = base + '/.mmx/runs/' + unchanged['id']
                remote('mmx-dev', f'test "$(ls {folder}/assets)" = {digest()}')
            with artifact.open('r+b') as output:
                output.seek(-1, 2)
                output.write(b'D')
            decompressing('xz', 'xz')
            run('dryrun')
            assert state() == current
            run('run')
            changed = state()
            for name, observed in changed.items():
                assert observed[1:3] == (digest(), digest()) and observed[0] != current[name][0], observed
            current = changed
            assert all(e['response']['change'] == 'unchanged' for e in run('run')['entries'])
            assert state() == current
            mark('decompress: gzip, bzip2, xz, and zstd install the decompressed file, unchanged when it matches; '
                 'a new compressed release installs and restarts once; only the decompressed file is kept with the run')

            runs = remote('mmx-dev', f'find {base}/.mmx/runs -mindepth 1 -maxdepth 1 | sort').stdout
            for pin, decompress, message in [(digest(), 'xz', 'SHA-256 mismatch'), (None, 'gzip', 'decompress gzip')]:
                decompressing('xz', decompress, pin)
                refused = run('run', 4)
                assert message in refused.stderr, refused
            assert remote('mmx-dev', f'find {base}/.mmx/runs -mindepth 1 -maxdepth 1 | sort').stdout == runs
            assert state() == current
            mark('decompress refuses the digest of the decompressed file and the wrong compression before any node job')
        finally:
            if forward:
                forward.terminate()
                forward.communicate(timeout=5)
            if server:
                server.shutdown()
                server.server_close()
            cleanup_nodes()
    report = vm.ROOT / 'downloads-result.json'
    report.write_text(json.dumps({'passed': passed}, indent=2) + '\n')
    print(f'{len(passed)} download workflows passed; {report}', flush=True)


if __name__ == '__main__':
    vm.run_suite(main)
