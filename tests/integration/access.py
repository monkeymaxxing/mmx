#!/usr/bin/env python3
"""Ordinary SSH/password setup, HTTPS authorization, and verified fallback."""
import hashlib
import json
import os
from pathlib import Path
import secrets
import shlex
import shutil
import pty
import select
import signal
import socket
import termios
import subprocess
import time

import vm


def main():
    vm.up()
    node, ssh = vm.connection()
    vm.reset()
    stamp = time.strftime('%Y%m%d-%H%M%S')
    workspace = vm.ROOT / 'access' / stamp
    workspace.mkdir(parents=True)
    mmx = vm.ROOT / 'bin/mmx'
    vm.build()
    passed = []
    user = 'mmx-pass-' + str(int(time.time()))
    other = 'mmx-key-' + str(int(time.time()))
    login, privilege = secrets.token_urlsafe(24), secrets.token_urlsafe(24)
    original_root = vm.console('getent shadow root').split(':')[1]
    credential_env = {'MMX_ACCESS_SSH': login, 'MMX_ACCESS_SUDO': privilege}
    previous_env = {name: vm.ENV.get(name) for name in credential_env}
    key_id = hashlib.sha256(f'{node["address"]}\0{node["ssh_port"]}\0{user}'.encode()).hexdigest()
    generated_key = Path.home() / '.ssh/mmx' / key_id
    assert not generated_key.exists(), 'fixture would reuse a non-fixture key'
    fixture_key = workspace / 'fixture-key'
    ssh_config = workspace / 'password-ssh.conf'
    ssh_config.write_text('Host *\n  IdentityAgent none\n  IdentitiesOnly yes\n  IdentityFile none\n')
    managed = '/var/tmp/mmx-integration/access-managed'
    processes = []
    original_identity = None  # machine ID and known_hosts, while the server is replaced

    def remote(script):
        return vm.cmd([*ssh, 'sudo -n /bin/sh -c ' + shlex.quote(script)]).stdout

    # Debian and Ubuntu name the OpenSSH server's unit ssh; the others sshd.
    sshd_unit = remote('if systemctl cat ssh.service >/dev/null 2>&1; then echo ssh; else echo sshd; fi').strip()

    def write_project(selected, tasks=None):
        vm.write_inventory(workspace, {'nodes': {'test': selected}}, name='example')
        vm.write_project(workspace, playbook_name='configure', **{'project_name': 'access', 'default_inventory': 'example',
            'secrets_from_env': {'login': 'MMX_ACCESS_SSH', 'privilege': 'MMX_ACCESS_SUDO'},
            'fleet_stages': [{'id': 'configure', 'nodes': 'all', 'node_tasks': tasks or [
                {'file': {"permissions_mode": "0644", 'path': managed, 'content': 'configured'}}]}]})

    def run(command, expected=None, *args, env=None):
        if env is None:
            result = vm.cmd([mmx, command, *vm.playbook_args(command, "configure"), '--json', *args], cwd=workspace, check=False)
        else:
            result = subprocess.run([str(mmx), command, *vm.playbook_args(command, "configure"), '--json', *map(str, args)], cwd=workspace,
                env=env, text=True, capture_output=True, timeout=120)
        for value in credential_env.values():
            assert value not in result.stdout and value not in result.stderr, 'credential appeared in output'
        assert result.returncode in (vm.success_codes(command) if expected is None else (expected,)), f'{command}: {result.returncode}\n{result.stdout}\n{result.stderr}'
        return json.loads(result.stdout)

    def mark(name):
        passed.append(name)
        print('PASS ' + name, flush=True)

    def wait(script):
        deadline = time.monotonic() + 20
        while time.monotonic() < deadline:
            if remote(script).strip() == 'yes':
                return
            time.sleep(0.1)
        raise AssertionError('fixture did not reach expected state')

    def interactive(cancel=False):
        pid, terminal = pty.fork()
        if pid == 0:
            os.chdir(workspace)
            os.execve(str(mmx), [str(mmx), 'dryrun', 'configure'], vm.ENV)
        output = b''
        prompts = [(b'SSH password for ', login), (b'sudo password for ', privilege)]
        sent, status, done = 0, None, False
        deadline = time.monotonic() + 40
        try:
            while time.monotonic() < deadline:
                if select.select([terminal], [], [], 0.1)[0]:
                    try:
                        chunk = os.read(terminal, 65536)
                    except OSError:
                        chunk = b''
                    output += chunk
                if sent < len(prompts) and prompts[sent][0] in output and not termios.tcgetattr(terminal)[3] & termios.ECHO:
                    if cancel:
                        os.write(terminal, b'\x03')
                        sent = len(prompts)
                    else:
                        os.write(terminal, prompts[sent][1].encode() + b'\n')
                        sent += 1
                done, status = os.waitpid(pid, os.WNOHANG)
                if done:
                    assert os.waitstatus_to_exitcode(status) in ((130,) if cancel else vm.success_codes("dryrun")), output.decode(errors='replace')
                    assert termios.tcgetattr(terminal)[3] & termios.ECHO, 'terminal echo was not restored'
                    assert all(value.encode() not in output for value in credential_env.values()), 'password echoed to terminal'
                    assert sent == len(prompts)
                    return output.split(b'Run ', 1)[1].split()[0].decode()
            safe = output.decode(errors='replace')
            for value in credential_env.values():
                safe = safe.replace(value, '[redacted]')
            raise AssertionError('interactive access timed out: ' + safe)
        finally:
            if status is None or not done:
                os.kill(pid, signal.SIGKILL)
                os.waitpid(pid, 0)
            os.close(terminal)

    try:
        # mmx_runner_download_url: the node downloads the runner from a mirror, and only the
        # hash the controller sent over verified SSH decides what is installed.
        runner = vm.ROOT / f'bin/mmx-runner-linux-{vm.architecture()}'
        digest = hashlib.sha256(runner.read_bytes()).hexdigest()
        mirror = '/var/tmp/mmx-integration/runner-mirror'
        url = f'http://127.0.0.1:8089/mmx/mmx-runner-{digest}'
        remote(f'mkdir -p {mirror}/mmx && chown -R {node["ssh_user"]} {mirror} && '
               f'systemd-run --unit mmx-runner-mirror python3 -m http.server --bind 127.0.0.1 --directory {mirror} 8089')
        wait('curl -fsS -o /dev/null http://127.0.0.1:8089/ && echo yes || true')
        write_project(dict(node, mmx_runner_download_url='http://127.0.0.1:8089/mmx/'))
        missing = run('dryrun', 1)
        assert url in missing['entries'][0]['response']['error'], missing
        truncated = workspace / 'truncated-runner'
        truncated.write_bytes(runner.read_bytes()[:1 << 20])
        vm.copy(truncated, f'{mirror}/mmx/mmx-runner-{digest}')
        corrupt = run('dryrun', 1)
        assert url in corrupt['entries'][0]['response']['error'], corrupt
        assert remote("find /opt/mmx/bin -name mmx-runner -o -name '.upload.*'") == ''
        vm.copy(runner, f'{mirror}/mmx/mmx-runner-{digest}')
        # A dryrun on a node without MMX's HTTPS service installs only the runner
        # and uses SSH.
        mirrored = run('dryrun')
        assert mirrored['entries'][0]['connection_used'] == 'ssh'
        assert remote(f'sha256sum /opt/mmx/bin/{digest}/mmx-runner').split()[0] == digest
        remote(f'test ! -f {managed}')
        mark('mmx_runner_download_url installs only the runner whose hash the controller sent; a missing or damaged download fails naming its URL')
        vm.reset()
        shutil.rmtree(workspace / '.mmx')

        # Damage only the upload stream. The ordinary SSH host verification and
        # the real privileged installer still run, and must reject these bytes.
        write_project(node)
        shim_dir = workspace / 'interrupted-upload'
        shim_dir.mkdir()
        shim = shim_dir / 'ssh'
        shim.write_text('''#!/usr/bin/env python3
import os, subprocess, sys
if sys.argv[-1].find("sha256sum -c -") >= 0:
    data = sys.stdin.buffer.read()
    p = subprocess.run(["/usr/bin/ssh", *sys.argv[1:]], input=data[:len(data)//2])
    sys.exit(p.returncode)
os.execv("/usr/bin/ssh", ["ssh", *sys.argv[1:]])
''')
        shim.chmod(0o700)
        env = dict(vm.ENV, PATH=str(shim_dir) + os.pathsep + vm.ENV['PATH'])
        failed = run('dryrun', 1, env=env)
        assert 'setup of the node runner executable' in failed['entries'][0]['response']['error']
        assert remote("find /opt/mmx/bin -name mmx-runner -o -name '.upload.*'") == ''
        first = run('dryrun')
        assert first['entries'][0]['connection_used'] == 'ssh'
        remote(f'test ! -f {managed}')
        mark('incomplete upload installs no executable; repeating setup installs the runner, and the dryrun applies no resources')

        for account in [user, other]:
            remote(f'useradd -m -s /bin/bash {account}')
        vm.guest('sudo', '-n', 'chpasswd', input=f'{user}:{login}\nroot:{privilege}\n')
        remote(f"printf '%s\\n' 'Defaults:{user} rootpw' '{user} ALL=(ALL) ALL' > /etc/sudoers.d/{user}; "
               f"printf '%s\\n' '{other} ALL=(ALL) NOPASSWD: ALL' > /etc/sudoers.d/{other}; "
               f"chmod 440 /etc/sudoers.d/{user} /etc/sudoers.d/{other}; "
               f"printf '%s\\n' 'Match User {user}' '  PasswordAuthentication yes' 'Match all' > /etc/ssh/sshd_config.d/10-mmx-access-test.conf; "
               '/usr/sbin/sshd -t; systemctl reload ' + sshd_unit)
        password_node = dict(node, ssh_user=user, ssh_config=str(ssh_config), ssh_password='{{ .secrets.login }}', sudo_password='{{ .secrets.privilege }}')
        del password_node['ssh_identity_file']
        write_project(password_node)
        for name in credential_env:
            vm.ENV.pop(name, None)
        missing = run('run', 1)
        assert 'MMX_ACCESS_SSH' in missing['entries'][0]['response']['error']
        assert missing['entries'][0]['response']['status'] == 'failed'
        vm.ENV['MMX_ACCESS_SSH'] = login
        missing = run('run', 1)
        assert 'MMX_ACCESS_SUDO' in missing['entries'][0]['response']['error']
        remote(f'test ! -f {managed}')
        mark('JSON commands identify missing SSH and sudo inputs without prompting or changing resources')

        prompt_node = {key: value for key, value in password_node.items() if key not in ['ssh_password', 'sudo_password']}
        prompt_node['connection'] = 'ssh_only'
        write_project(prompt_node)
        canceled_id = interactive(cancel=True)
        canceled = run('results', 0, canceled_id)['entries'][0]
        assert canceled['response']['status'] == 'stopped', canceled
        interactive()
        remote(f'test ! -f {managed}')
        assert not generated_key.exists()
        mark('terminal prompts accept distinct passwords without echo; Ctrl-C restores the terminal and returns promptly')

        write_project(password_node)
        vm.ENV.update(credential_env)
        # Before MMX's HTTPS service exists, a dryrun uses the passwords over
        # SSH and enrolls no key; the first run sets the service up and
        # enrolls one key.
        previewed = run('dryrun')
        assert previewed['entries'][0]['connection_used'] == 'ssh'
        assert not generated_key.exists()
        remote(f'test ! -e /home/{user}/.ssh/authorized_keys')
        remote(f'test ! -f {managed}')
        installed = run('run')
        assert installed['entries'][0]['connection_used'] == 'https'
        assert generated_key.exists() and generated_key.stat().st_mode & 0o777 == 0o600
        authorized = remote(f'cat /home/{user}/.ssh/authorized_keys')
        assert authorized.count('ssh-ed25519') == 1
        assert remote(f'cat {managed}') == 'configured'
        remote(f'rm {managed}')
        for name in credential_env:
            vm.ENV.pop(name, None)
        assert run('dryrun')['entries'][0]['connection_used'] == 'https'
        applied = run('run')
        assert applied['entries'][0]['connection_used'] == 'https'
        assert remote(f'cat {managed}') == 'configured'
        assert remote(f'cat /home/{user}/.ssh/authorized_keys') == authorized
        mark('password-only setup accepts distinct SSH/sudo passwords, enrolls one key in the first run, and subsequent HTTPS needs neither password')

        # Current sudo authority is checked, not merely copied during setup.
        # Removing the user's sudoers file takes the authority away, unless the
        # distribution (openSUSE) lets every account use sudo with a password;
        # then a rule that denies the user has to take its place.
        remote(f'mv /etc/sudoers.d/{user} /var/tmp/{user}.sudoers')
        if 'is not allowed to run sudo' not in remote(f'sudo -l -U {user} 2>&1 || true'):
            remote(f"printf '%s\\n' '{user} ALL=(ALL) !ALL' > /etc/sudoers.d/{user}; chmod 440 /etc/sudoers.d/{user}")
        denied = run('run', 1)
        assert 'unrestricted root sudo access' in denied['entries'][0]['response']['error']
        remote(f'mv -f /var/tmp/{user}.sudoers /etc/sudoers.d/{user}')
        assert run('run')['entries'][0]['connection_used'] == 'https'
        mark('removing administrator sudo authority denies the next HTTPS connection; restoring it restores ordinary access')

        # A second key is added to ordinary SSH authorization, with no MMX list.
        vm.cmd(['ssh-keygen', '-q', '-t', 'ed25519', '-N', '', '-f', fixture_key])
        public = fixture_key.with_suffix('.pub').read_text()
        remote(f'install -d -m 700 -o {other} -g {other} /home/{other}/.ssh')
        vm.guest('sudo', '-n', '/bin/sh', '-c',
                f'cat > /home/{other}/.ssh/authorized_keys; chown {other}:{other} /home/{other}/.ssh/authorized_keys; chmod 600 /home/{other}/.ssh/authorized_keys', input=public)
        second_node = dict(node, ssh_user=other, ssh_identity_file=str(fixture_key))
        write_project(second_node)
        assert run('run')['entries'][0]['connection_used'] == 'https'
        mark('an ordinary newly-added administrator SSH key gains HTTPS access without separate MMX enrollment')

        gate = '/var/tmp/mmx-integration/access-gate'
        write_project(second_node, [{'shell': {'script': f'touch {gate}-entered; while test ! -f {gate}-release; do sleep 0.1; done; echo completed >> {gate}-count'}}])
        output = open(workspace / 'revocation.stdout', 'w')
        error = open(workspace / 'revocation.stderr', 'w')
        process = subprocess.Popen([str(mmx), 'run', 'configure', '--json'], cwd=workspace, env=vm.ENV, stdout=output, stderr=error)
        processes.append((process, output, error))
        wait(f'test ! -f {gate}-entered || echo yes')
        # A fresh local trust record requires verified SSH setup. Setup must
        # leave both the admitted worker and the already-running endpoint alone.
        runner_pid = remote('systemctl show -p MainPID --value mmx-https-api.service').strip()
        other_id = hashlib.sha256(f'{node["address"]}\0{node["ssh_port"]}\0{other}'.encode()).hexdigest()
        other_pin = workspace / '.mmx/nodes' / (other_id + '.json')
        saved_pin = other_pin.read_bytes()
        other_pin.unlink()
        blocked_setup = run('dryrun', 1)
        assert 'node busy' in blocked_setup['entries'][0]['response']['error']
        assert remote('systemctl show -p MainPID --value mmx-https-api.service').strip() == runner_pid
        remote(f'test ! -f {gate}-count')
        other_pin.write_bytes(saved_pin)
        mark('setup during an admitted job preserves the running endpoint and worker')
        # Like an SSH session, the connected fleet stage keeps the authority it connected inputs.
        remote(f': > /home/{other}/.ssh/authorized_keys')
        remote(f'touch {gate}-release')
        assert process.wait(timeout=20) == 0
        output.flush(); error.flush()
        open_run = json.loads((workspace / 'revocation.stdout').read_text())
        assert open_run['status'] == 'succeeded' and remote(f'cat {gate}-count') == 'completed\n'
        write_project(password_node)
        assert run('run')['entries'][0]['connection_used'] == 'https'
        write_project(second_node)
        denied = run('run', 1)
        assert 'HTTPS 403' in denied['entries'][0]['response']['error']
        vm.guest('sudo', '-n', '/bin/sh', '-c', f'cat > /home/{other}/.ssh/authorized_keys', input=public)
        assert run('run')['entries'][0]['connection_used'] == 'https'
        mark('key removal refuses the next connection; the connected fleet stage finishes, as an SSH session does, and other administrators continue')

        # Team onboarding and offboarding through authorized_keys, applied by
        # the first administrator, grant and revoke the second's MMX access.
        team_key = workspace / 'team-key'
        vm.cmd(['ssh-keygen', '-q', '-t', 'ed25519', '-N', '', '-C', 'team@laptop', '-f', team_key])
        team_public = team_key.with_suffix('.pub').read_text().strip()

        def key_name(path):
            bits, fingerprint, comment = vm.cmd(['ssh-keygen', '-lf', path.with_suffix('.pub')]).stdout.split()[:3]
            return f'ssh-ed25519 {fingerprint} {comment}'

        def manage_keys(**task):
            write_project(password_node, [{'authorized_keys': dict(for_user_account=other, **task)}])
            before = keys_file()
            preview = run('dryrun')['entries'][0]['response']['results'][0]
            assert preview['change'] == 'would_change' and keys_file() == before, preview
            applied = run('run')['entries'][0]['response']['results'][0]
            assert applied['change'] == 'changed' and applied['value'] == preview['value'], (preview, applied)
            assert run('run')['entries'][0]['response']['change'] == 'unchanged'
            return applied['value']

        def keys_file():
            return remote(f"cat /home/{other}/.ssh/authorized_keys; stat -c '%a %U %G' /home/{other}/.ssh /home/{other}/.ssh/authorized_keys")

        def access(selected, expected):
            write_project(selected)
            record = run('run', expected)
            return record['entries'][0]['connection_used'] if expected == 0 else record['entries'][0]['response']['error']

        team_node = dict(node, ssh_user=other, ssh_identity_file=str(team_key))
        remote(f"echo '# added by hand' >> /home/{other}/.ssh/authorized_keys")
        assert manage_keys(keys=[team_public], keys_not_listed='keep') == {'added': [key_name(team_key)]}
        assert keys_file() == f'{public.strip()}\n# added by hand\n{team_public}\n700 {other} {other}\n600 {other} {other}\n'
        assert access(team_node, 0) == 'https' and access(second_node, 0) == 'https'
        mark('authorized_keys with keys_not_listed: keep adds a key after unmanaged lines, previews it by fingerprint, reruns unchanged, and the new key gains MMX access')

        # A plain public key keeps restrictions already written by an administrator.
        restricted = 'no-agent-forwarding,no-port-forwarding ' + team_public
        remote(f"sed -i 's|^{team_public}$|{restricted}|' /home/{other}/.ssh/authorized_keys")
        write_project(password_node, [{'authorized_keys': {'for_user_account': other, 'keys': [team_public], 'keys_not_listed': 'keep'}}])
        assert run('dryrun')['entries'][0]['response']['change'] == 'unchanged'
        assert run('run')['entries'][0]['response']['change'] == 'unchanged'
        assert restricted in keys_file()
        desired_key = 'no-agent-forwarding ' + team_public
        for policy in ('keep', 'remove'):
            write_project(password_node, [{'authorized_keys': {'for_user_account': other, 'keys': [desired_key], 'keys_not_listed': policy}}])
            before = keys_file()
            for command in ('dryrun', 'run'):
                refusal = run(command, 1)['entries'][0]['response']['results'][0]
                assert refusal['status'] == 'failed' and keys_file() == before, refusal
                for text in ('team@laptop', f'/home/{other}/.ssh/authorized_keys',
                             'options [no-agent-forwarding,no-port-forwarding] on the node',
                             'node task requests options [no-agent-forwarding]', 'replace_options_of_listed_keys: true'):
                    assert text in refusal['reason'], refusal
        assert manage_keys(keys=[desired_key], keys_not_listed='keep', replace_options_of_listed_keys=True) == {'updated': [key_name(team_key)]}
        assert desired_key in keys_file() and restricted not in keys_file()

        assert manage_keys(keys=[team_public], keys_not_listed='remove') == {'removed': [key_name(fixture_key)]}
        assert keys_file() == f'{desired_key}\n700 {other} {other}\n600 {other} {other}\n'
        assert manage_keys(keys=[team_public], keys_not_listed='remove', replace_options_of_listed_keys=True) == {'updated': [key_name(team_key)]}
        assert keys_file() == f'{team_public}\n700 {other} {other}\n600 {other} {other}\n'
        mark('plain listed keys keep restrictions under keep and remove; differing explicit options refuse dryrun and apply before changes; replacement needs replace_options_of_listed_keys')
        assert 'HTTPS 403' in access(second_node, 1)
        refused = access(dict(second_node, connection='ssh_only'), 1)
        # Where the SSH server also accepts passwords, MMX says
        # that it now needs one; elsewhere the server's refusal is the answer.
        assert 'Permission denied' in refused or 'SSH password for' in refused, refused
        # OpenSSH 9.8+ refuses new connections from an address for a few seconds
        # after failed logins (PerSourcePenalties); later cases connect afresh.
        deadline = time.monotonic() + 60
        while vm.cmd([*vm.unshared(ssh), 'true'], check=False).returncode:
            assert time.monotonic() < deadline, 'sshd kept refusing new connections'
            time.sleep(2)
        assert access(team_node, 0) == 'https'
        assert manage_keys(keys=[team_public], state='absent') == {'removed': [key_name(team_key)]}
        assert keys_file() == f'700 {other} {other}\n600 {other} {other}\n'
        assert 'HTTPS 403' in access(team_node, 1)
        assert access(password_node, 0) == 'https'
        vm.guest('sudo', '-n', '/bin/sh', '-c', f'cat > /home/{other}/.ssh/authorized_keys', input=public)
        mark("keys_not_listed: remove and state: absent revoke a key: that key's next HTTPS and SSH connections are refused while other administrators continue")

        # The saved identity only records what verified SSH reported; one that no
        # longer matches is checked again through SSH and corrected.
        write_project(password_node)
        pin_file = workspace / '.mmx/nodes' / (key_id + '.json')
        trusted = json.loads(pin_file.read_text())
        pin_file.write_text(json.dumps(dict(trusted, tls_pin='0' * 64)))
        vm.ENV['MMX_ACCESS_SUDO'] = privilege
        corrected = vm.cmd([mmx, 'run', 'configure', '--json'], cwd=workspace, check=False)
        vm.ENV.pop('MMX_ACCESS_SUDO')
        for value in credential_env.values():
            assert value not in corrected.stdout and value not in corrected.stderr, 'credential appeared in output'
        assert corrected.returncode == 0, corrected.stdout + corrected.stderr
        assert json.loads(corrected.stdout)['entries'][0]['connection_used'] == 'https'
        assert 'node HTTPS API key changed, verified through SSH' in corrected.stderr
        assert json.loads(pin_file.read_text()) == trusted
        mark('a saved HTTPS identity that no longer matches is verified again through SSH and corrected')

        # HTTPS fails while the node HTTPS API process runs: here the inventory
        # names a local port where nothing listens.
        with socket.socket() as listener:
            listener.bind(('127.0.0.1', 0))
            closed_port = listener.getsockname()[1]
        write_project(dict(password_node, https_address=f'https://127.0.0.1:{closed_port}'))
        missing = run('run', 1)
        assert 'MMX_ACCESS_SUDO' in missing['entries'][0]['response']['error']
        vm.ENV['MMX_ACCESS_SUDO'] = privilege
        fallback = run('run')
        assert fallback['entries'][0]['connection_used'] == 'ssh_fallback'
        reason = fallback['entries'][0]['connection_fallback_reason']
        assert "mmx-runner, MMX's HTTPS service on this node, is running. MMX is using SSH for this node." in reason and 'connection: ssh_only' in reason, reason
        write_project(password_node)
        vm.ENV.pop('MMX_ACCESS_SUDO')
        assert run('run')['entries'][0]['connection_used'] == 'https'
        mark('HTTPS that fails uses the enrolled SSH key and ordinary sudo password, then returns to HTTPS when available')

        # A stopped node HTTPS API process is started again through verified
        # SSH, which needs the ordinary sudo password here.
        remote('systemctl stop mmx-https-api.service')
        vm.ENV['MMX_ACCESS_SUDO'] = privilege
        assert run('run')['entries'][0]['connection_used'] == 'https'
        vm.ENV.pop('MMX_ACCESS_SUDO')
        assert remote('systemctl is-active mmx-https-api.service').strip() == 'active'
        assert run('run')['entries'][0]['connection_used'] == 'https'
        mark('a stopped node HTTPS API process is started again through verified SSH with the ordinary sudo password, and HTTPS follows')

        for path in (workspace / '.mmx').rglob('*.json'):
            data = path.read_text()
            assert all(value not in data for value in credential_env.values()), 'credential entered controller history'
            assert 'BEGIN OPENSSH PRIVATE KEY' not in data
        remote_data = remote("find /var/lib/mmx/jobs -name '*.json' -exec cat {} +")
        assert all(value not in remote_data for value in credential_env.values()), 'connection credential entered node jobs'
        mark('connection passwords and private keys remain absent from retained controller records and node requests/results')

        # Replacing a server: its OpenSSH host key is the only identity to update.
        write_project(node)
        assert run('run')['entries'][0]['connection_used'] == 'https'
        default_id = hashlib.sha256(f'{node["address"]}\0{node["ssh_port"]}\0{node["ssh_user"]}'.encode()).hexdigest()
        default_pin = workspace / '.mmx/nodes' / (default_id + '.json')
        old_node = json.loads(default_pin.read_text())['node_id']
        write_project(node, [{'shell': {'script': 'exit 3'}}])
        before = run('run', 1)
        known = Path(node['ssh_known_hosts'])
        stale_known = workspace / 'replaced-server.known_hosts'
        stale_known.write_text(known.read_text())
        machine_id = remote('cat /etc/machine-id')
        remote('rm -rf /var/tmp/mmx-host-keys; mkdir -m 700 /var/tmp/mmx-host-keys; cp -a /etc/ssh/ssh_host_* /var/tmp/mmx-host-keys/')
        original_identity = (machine_id, stale_known.read_text())
        vm.reset()
        # One command replaces the identity and reports the new host key, which
        # the administrator trusts as for any SSH tool.
        new_key = remote("head -c 16 /dev/urandom | od -An -tx1 | tr -d ' \\n' > /etc/machine-id; "
                         'rm -f /etc/ssh/ssh_host_*; ssh-keygen -A >/dev/null; systemctl restart ' + sshd_unit + '; cat /etc/ssh/ssh_host_ed25519_key.pub')
        name = node['address'] if int(node['ssh_port']) == 22 else f'[{node["address"]}]:{node["ssh_port"]}'
        known.write_text(name + ' ' + new_key)
        write_project(dict(node, ssh_known_hosts=str(stale_known)))
        untrusted = run('run', 1)
        assert 'Host key verification failed' in untrusted['entries'][0]['response']['error']
        remote('test ! -e /opt/mmx')
        write_project(node)
        refused = run('resume', 1, before['id'])
        assert 'node was replaced' in refused['entries'][0]['response']['error']
        remote('test ! -e /opt/mmx')
        rebuilt = vm.cmd([mmx, 'run', 'configure', '--json'], cwd=workspace, check=False)
        assert rebuilt.returncode == 0, rebuilt.stdout + rebuilt.stderr
        assert json.loads(rebuilt.stdout)['entries'][0]['connection_used'] == 'https'
        assert 'node address now reaches a different machine' in rebuilt.stderr
        assert json.loads(default_pin.read_text())['node_id'] != old_node
        assert remote(f'cat {managed}') == 'configured'
        mark('a replaced server needs only its new SSH host key trusted; resume of older work refuses it before installing anything')
        (workspace / 'results.json').write_text(json.dumps({'passed': passed}, indent=2))
        print(f'{len(passed)} access workflows passed; records: {workspace}', flush=True)
    finally:
        for process, output, error in processes:
            if process.poll() is None:
                process.kill(); process.wait(timeout=10)
            output.close(); error.close()
        if original_identity:
            machine_id, known_hosts = original_identity
            # Restore the keys only from a complete backup; never leave sshd without keys.
            vm.guest('sudo', '-n', '/bin/sh', '-c', 'set -e; cat > /etc/machine-id; '
                     'test -f /var/tmp/mmx-host-keys/ssh_host_ed25519_key; rm -f /etc/ssh/ssh_host_*; '
                     'cp -a /var/tmp/mmx-host-keys/ssh_host_* /etc/ssh/; rm -rf /var/tmp/mmx-host-keys; systemctl restart ' + sshd_unit, input=machine_id)
            Path(node['ssh_known_hosts']).write_text(known_hosts)
        # These accounts, passwords, keys, and SSH/sudo files belong only to this fixture.
        # usermod -p takes any recorded field, such as the lock "!*" that chpasswd -e rejects as an invalid hash.
        vm.guest('sudo', '-n', 'usermod', '-p', original_root, 'root')
        remote(f'rm -f /etc/sudoers.d/{user} /etc/sudoers.d/{other} /var/tmp/{user}.sudoers /etc/ssh/sshd_config.d/10-mmx-access-test.conf; '
               f'userdel -r {user} 2>/dev/null || true; userdel -r {other} 2>/dev/null || true; systemctl reload {sshd_unit}; '
               'systemctl stop mmx-runner-mirror 2>/dev/null || true; systemctl reset-failed mmx-runner-mirror 2>/dev/null || true')
        for suffix in ['', '.pub', '.lock']:
            Path(str(generated_key) + suffix).unlink(missing_ok=True)
        fixture_key.unlink(missing_ok=True)
        fixture_key.with_suffix('.pub').unlink(missing_ok=True)
        for name, value in previous_env.items():
            if value is None:
                vm.ENV.pop(name, None)
            else:
                vm.ENV[name] = value


if __name__ == '__main__':
    vm.run_suite(main)
