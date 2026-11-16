#!/usr/bin/env python3
"""Exercise task secrets, age editing, password files, the commands that work
on secret keys, and resume on real nodes."""
import hashlib
import json
import os
import pty
import re
import select
import signal
import termios
from pathlib import Path
import shlex
import shutil
import subprocess
import threading
import time

import vm

BASE = '/var/tmp/mmx-integration'
SERVICE = 'mmx-secret-test.service'
PORT = 18763

APP = r'''import hashlib, http.server, pathlib, signal
base = pathlib.Path('/var/tmp/mmx-integration')
loaded = ''
def reload(*_):
    global loaded
    loaded = hashlib.sha256((base / 'private.conf').read_bytes()).hexdigest()
    (base / 'loaded.sha256').write_text(loaded)
    with (base / 'reloads').open('a') as f: f.write('loaded\n')
class Handler(http.server.BaseHTTPRequestHandler):
    def log_message(self, *_): pass
    def do_GET(self):
        self.send_response(200); self.end_headers(); self.wfile.write(loaded.encode())
reload()
signal.signal(signal.SIGHUP, reload)
http.server.HTTPServer(('127.0.0.1', 18763), Handler).serve_forever()
'''
UNIT = f'''[Unit]
Description=MMX disposable secret test
[Service]
ExecStart=/usr/bin/python3 {BASE}/secret-app.py
ExecReload=/bin/kill -HUP $MAINPID
Restart=no
'''


def main():
    nodes, connections = {}, {}
    for node, name in [('a', 'mmx-dev'), ('b', 'mmx-peer')]:
        vm.up(name)
        nodes[node], connections[node] = vm.connection(name)
        vm.console(f'systemctl stop {SERVICE} 2>/dev/null || true', name)
        vm.reset(name)
    nodes['b']['connection'] = 'ssh_only'
    workspace = vm.ROOT / 'secrets'
    if workspace.exists():
        shutil.rmtree(workspace)
    workspace.mkdir(mode=0o700)
    vm.write_inventory(workspace, {'nodes': nodes}, name='example')
    role = workspace / 'roles/app'
    role.mkdir(parents=True)
    mmx = vm.ROOT / 'bin/mmx'
    vm.build()
    env = dict(vm.ENV)
    for name in ['MMX_VAULT_PASSWORD_FILE', 'MMX_VAULT_PASSWORD']:
        env.pop(name, None)
    env['MMX_SECRET_A'] = 'fixture-secret-a-initial'
    env['MMX_SECRET_B'] = 'fixture-secret-b-initial'
    secret_values = [env['MMX_SECRET_A'], env['MMX_SECRET_B']]
    passed = []
    marked = time.monotonic()
    process = None

    def remote(node, script):
        return vm.cmd([*connections[node], 'sudo -n /bin/sh -c ' + shlex.quote(script)]).stdout

    def run(command, expected=None, *args, selected_env=None):
        result = subprocess.run([str(mmx), command, *vm.playbook_args(command, "configure"), *map(str, args), '--json'], cwd=workspace,
                                env=env if selected_env is None else selected_env,
                                capture_output=True, text=True, timeout=120)
        for value in secret_values:
            assert value not in result.stdout + result.stderr, 'secret appeared in CLI output'
        assert result.returncode in (vm.success_codes(command) if expected is None else (expected,)), (command, result.returncode, result.stdout, result.stderr)
        return json.loads(result.stdout) if result.stdout.strip() else {}, result.stderr

    def interactive(cancel=False, editing=False):
        selected = dict(env)
        if editing:
            selected['MMX_TEST_EDITOR_INTERACTIVE'] = '1'
            selected['MMX_TEST_EDITOR_PATH'] = str(workspace / 'editor-path')
        else:
            selected.pop('MMX_VAULT_PASSWORD_FILE', None)
            selected.pop('MMX_VAULT_PASSWORD', None)
        prompt = b'Editor input: ' if editing else b'Vault passphrase: '
        pid, terminal = pty.fork()
        if pid == 0:
            os.chdir(workspace)
            command = ['secrets', 'edit'] if editing else ['dryrun', 'configure']
            os.execve(str(mmx), [str(mmx), *command], selected)
        output, sent, done = b'', False, False
        deadline = time.monotonic() + 45
        try:
            while time.monotonic() < deadline:
                if select.select([terminal], [], [], .1)[0]:
                    try:
                        output += os.read(terminal, 65536)
                    except OSError:
                        pass
                if not sent and prompt in output and (editing or not termios.tcgetattr(terminal)[3] & termios.ECHO):
                    if cancel:
                        os.kill(pid, signal.SIGINT)
                    else:
                        os.write(terminal, b'save\n' if editing else b'fixture-vault-passphrase\n')
                    sent = True
                child, status = os.waitpid(pid, os.WNOHANG)
                if child:
                    done = True
                    assert os.waitstatus_to_exitcode(status) in ((130,) if cancel else ((0,) if editing else vm.success_codes("dryrun"))), output.decode()
                    assert sent and output.count(prompt) == 1
                    assert b'fixture-vault-passphrase' not in output
                    assert termios.tcgetattr(terminal)[3] & termios.ECHO, 'terminal echo was not restored'
                    return
            raise AssertionError('interactive vault command did not finish')
        finally:
            if not done:
                os.kill(pid, signal.SIGKILL)
                os.waitpid(pid, 0)
            os.close(terminal)

    def mark(name):
        # The seconds since the last mark say where the suite's time goes.
        nonlocal marked
        now = time.monotonic()
        passed.append(name)
        print(f'PASS {name} ({now - marked:.0f} s)', flush=True)
        marked = now

    def project(vault=False, exports=True):
        p = {'project_name': 'secrets', 'default_inventory': 'example', 'fleet_stages': []}
        if not vault:
            p['secrets_from_env'] = {'password_a': 'MMX_SECRET_A', 'password_b': 'MMX_SECRET_B'}
        for node in ['a', 'b']:
            p['fleet_stages'].append({'id': 'configure_' + node, 'nodes': node, 'on_node_failure': 'stop_run',
                'node_tasks': [{'id': 'app', 'role': 'app', 'inputs': {'password': '{{ .secrets.password_' + node + ' }}'}}],
                'outputs': {'derived': '{{ .node_tasks.app.outputs.derived }}'}})
        if exports:
            p['fleet_stages'].append({'id': 'consume', 'nodes': 'b', 'node_tasks': [
                {'file': {'path': BASE + '/exported-secret', 'content': '{{ .outputs.configure_a.a.derived }}', 'permissions_mode': '0600'}}]})
        vm.write_project(workspace, playbook_name='configure', **p)

    role_definition = {'inputs': {'password': {'type': 'string', 'required': True, 'secret': True}}, 'node_tasks': [
        {
            "task_block": {
                "id": "configure",
                "changes": [
                    {"file": {"path": BASE + "/private.conf", "template": "private.conf.tmpl", "permissions_mode": "0600"}},
                    {"shell": {"script": f"test ! -e {BASE}/fail-before-activate", "dryrun": "true", "changed_when": False}},
                    {
                        "shell": {
                            "script": f"if test -e {BASE}/pause; then touch {BASE}/admitted; sleep 3; fi",
                            "dryrun": "true",
                            "changed_when": False,
                        }
                    },
                ],
                "activate": [{"systemd_service": {"name": SERVICE, "state": "running", "refresh": "reload"}}],
                "verify": [{"wait_for": {"url_responds": f"http://127.0.0.1:{PORT}/", "wait_at_most": "30s"}}],
            }
        },
        {'id': 'derived', 'shell': {'script': 'printf "%s-derived" "$TOKEN"', 'env': {'TOKEN': '{{ .inputs.password }}'}, 'changed_when': False}},
        {'show': {'label': 'configured', 'secret': '{{ .node_tasks.derived.stdout }}'}},
    ], 'outputs': {'derived': '{{ .node_tasks.derived.stdout }}'}}
    template = role / 'private.conf.tmpl'
    template.write_text('{{ .inputs.password }}')
    (role / 'main.yml').write_text(json.dumps(role_definition))

    def loaded(node, expected):
        digest = hashlib.sha256(expected.encode()).hexdigest()
        deadline = time.monotonic() + 15
        while time.monotonic() < deadline:
            if remote(node, f'cat {BASE}/loaded.sha256 2>/dev/null || true').strip() == digest:
                return
            time.sleep(.1)
        raise AssertionError('service did not load the expected configuration')

    def reloads(node):
        return int(remote(node, f'wc -l < {BASE}/reloads').strip())

    def scan():
        for path in (workspace / '.mmx').rglob('*.json'):
            raw = path.read_text()
            assert all(value not in raw for value in secret_values), 'secret in controller history'
        for path in (workspace / '.mmx/runs').glob('*/assets/*'):
            assert all(value.encode() not in path.read_bytes() for value in secret_values), 'runtime secret in saved asset'
        for node in ['a', 'b']:
            raw = remote(node, "find /var/lib/mmx -type f \\( -name '*.json' -o -name worker.log \\) -exec cat {} +")
            # This suite's entries only: the long-lived test guests keep every
            # earlier run's journal, which makes a full scan slow.
            raw += remote(node, f"journalctl -q --no-pager --since @{started[node]} -u mmx-https-api.service")
            assert all(value not in raw for value in secret_values), 'secret in node history or journal'
            assert not remote(node, "find /run/mmx/private -name '*-input.json' -print 2>/dev/null || true").strip(), 'private handoff not removed'

    try:
        started = {node: remote(node, 'date +%s').strip() for node in ['a', 'b']}
        for node in ['a', 'b']:
            remote(node, f"cat > {BASE}/secret-app.py <<'APP'\n{APP}\nAPP\ncat > /etc/systemd/system/{SERVICE} <<'UNIT'\n{UNIT}\nUNIT\nsystemctl daemon-reload")
        project()
        env['MMX_VAULT_PASSWORD_FILE'] = str(workspace / 'does-not-exist')
        run('validate')
        run('explain')
        run('dryrun')
        for node in ['a', 'b']:
            assert remote(node, f'test ! -e {BASE}/private.conf && echo unchanged').strip() == 'unchanged'
        mark('environment-only validation, explanation, and preview need no vault or password file')

        run('run')
        loaded('a', env['MMX_SECRET_A'])
        loaded('b', env['MMX_SECRET_B'])
        actual = remote('b', f'cat {BASE}/exported-secret')
        assert actual == env['MMX_SECRET_A'] + '-derived'
        assert remote('a', f'stat -c %a {BASE}/private.conf').strip() == '600'
        scan()
        mark('HTTPS and SSH deliver private inputs and computed fleet stage outputs without retained plaintext')

        before = [reloads('a'), reloads('b')]
        run('run')
        assert before == [reloads('a'), reloads('b')]
        mark('unchanged secret configuration causes no extra reload')

        remote('a', f'touch {BASE}/fail-before-activate')
        env['MMX_SECRET_A'] = 'fixture-secret-written-before-failure'
        secret_values.append(env['MMX_SECRET_A'])
        run('run', 1)
        assert reloads('a') == before[0]
        env['MMX_SECRET_A'] = 'fixture-secret-rotated-before-resume'
        secret_values.append(env['MMX_SECRET_A'])
        remote('a', f'rm {BASE}/fail-before-activate')
        template.unlink()
        record, _ = run('resume')
        template.write_text('{{ .inputs.password }}')
        assert record['secret_inputs_refreshed']
        loaded('a', env['MMX_SECRET_A'])
        assert reloads('a') == before[0] + 1
        scan()
        mark('resume uses the frozen template after deletion, refreshes a rotated secret, and activates once')

        env['MMX_SECRET_A'] = 'fixture-secret-controller-disconnected'
        secret_values.append(env['MMX_SECRET_A'])
        remote('a', f'touch {BASE}/pause; rm -f {BASE}/admitted')
        process = subprocess.Popen([str(mmx), 'run', 'configure', '--json'], cwd=workspace, env=env, stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
        deadline = time.monotonic() + 25
        while time.monotonic() < deadline:
            if remote('a', f'test -e {BASE}/admitted && echo ready || true').strip() == 'ready':
                break
            assert process.poll() is None, process.communicate()
            time.sleep(.1)
        else:
            raise AssertionError('job was not admitted')
        process.kill(); process.communicate(); process = None
        loaded('a', env['MMX_SECRET_A'])
        remote('a', f'rm {BASE}/pause')
        # Status recovers the admitted producer and its private runtime exports.
        # The job does not run again merely because its controller disconnected.
        after_disconnect = reloads('a')
        run('resume')
        assert reloads('a') == after_disconnect
        scan()
        mark('an admitted secret-bearing job finishes after controller loss and is not replayed')

        password = workspace / 'vault-password'
        password.write_text('fixture-vault-passphrase\n'); password.chmod(0o600)
        secret_values.append('fixture-vault-passphrase')
        env['MMX_VAULT_PASSWORD_FILE'] = str(password)
        env['MMX_TEST_VAULT_TEXT'] = 'password_a: fixture-vault-a\npassword_b: fixture-vault-b\n'
        secret_values.extend(['fixture-vault-a', 'fixture-vault-b'])
        editor = workspace / 'editor.py'
        editor.write_text('''#!/usr/bin/env python3
import os,pathlib,sys
if os.environ.get('MMX_TEST_EDITOR_INTERACTIVE'):
    path = pathlib.Path(sys.argv[1])
    assert path.stat().st_mode & 0o777 == 0o600
    assert path.parent.stat().st_mode & 0o777 == 0o700
    pathlib.Path(os.environ['MMX_TEST_EDITOR_PATH']).write_text(str(path))
    assert input('Editor input: ') == 'save'
pathlib.Path(sys.argv[1]).write_text(os.environ['MMX_TEST_VAULT_TEXT'])
''')
        editor.chmod(0o700); env['EDITOR'] = str(editor); env.pop('VISUAL', None)
        run('secrets', 0, 'edit')
        vault = workspace / 'secrets_vault.age'
        assert vault.read_bytes().startswith(b'age-encryption.org/v1\n')
        project(vault=True)
        run('run')
        loaded('a', 'fixture-vault-a'); loaded('b', 'fixture-vault-b')
        run('run', 0, selected_env=dict(env, MMX_STATE_DIR=str(workspace / 'controller-two')))
        scan()
        mark('the age vault and password file work from two independent controller states')
        interactive()
        interactive(cancel=True)
        mark('one hidden vault prompt serves the whole invocation; cancellation restores the terminal')
        interactive(editing=True)
        temporary = Path((workspace / 'editor-path').read_text())
        assert not temporary.parent.exists(), 'successful edit retained plaintext'
        before = vault.read_bytes()
        interactive(editing=True, cancel=True)
        temporary = Path((workspace / 'editor-path').read_text())
        assert not temporary.parent.exists(), 'interrupted edit retained plaintext'
        assert vault.read_bytes() == before, 'interrupted edit changed encrypted vault'
        mark('the editor accepts terminal input; success and cancellation remove its private plaintext')

        selected = dict(env)
        selected.pop('MMX_VAULT_PASSWORD_FILE')
        selected['MMX_VAULT_PASSWORD'] = 'fixture-vault-passphrase'
        run('dryrun', selected_env=selected)
        selected.pop('MMX_VAULT_PASSWORD')
        _, error = run('run', 4, selected_env=selected)
        assert '--vault-password-file' in error
        run('explain', selected_env=selected)
        mark('CI accepts an environment passphrase and missing credentials fail without prompting')

        before = vault.read_bytes()
        env['MMX_TEST_VAULT_TEXT'] = 'password_a: [fixture-private-invalid-yaml'
        secret_values.append('fixture-private-invalid-yaml')
        run('secrets', 4, 'edit')
        assert vault.read_bytes() == before
        mark('an invalid edit preserves the usable encrypted vault and suppresses YAML contents')

        replacement = workspace / 'new-vault-password'
        replacement.write_text('fixture-new-vault-passphrase\n'); replacement.chmod(0o600)
        secret_values.append('fixture-new-vault-passphrase')
        run('secrets', 0, 'passwd', '--new-vault-password-file', replacement)
        run('run', 4)
        env['MMX_VAULT_PASSWORD_FILE'] = str(replacement)
        run('run')
        scan()
        mark('passphrase changes preserve named values and reject the old passphrase')

        project(vault=True, exports=False)
        current = json.loads((workspace / 'mmx_project_settings.yml').read_text())
        current['secrets_from_env'] = {'password_b': 'MMX_SECRET_B'}
        vm.write_settings(workspace, current)
        env['MMX_SECRET_A'] = 'fixture-ambient-must-not-override-vault'
        secret_values.append(env['MMX_SECRET_A'])
        run('run')
        loaded('a', 'fixture-vault-a'); loaded('b', env['MMX_SECRET_B'])
        scan()
        mark('explicit environment bindings can mix with the vault; ambient values never override it')

        secret_key_commands(workspace, mmx, env, remote, scan, mark, secret_values)
        secrets_that_start_on_a_node(workspace, mmx, env, connections, remote, scan, mark, secret_values)
    finally:
        if process is not None:
            process.kill(); process.communicate()
        for node in ['a', 'b']:
            remote(node, f'systemctl stop {SERVICE} 2>/dev/null || true; rm -f /etc/systemd/system/{SERVICE}; systemctl daemon-reload')
        for name in ['vault-password', 'new-vault-password', 'editor.py', 'editor-path']:
            (workspace / name).unlink(missing_ok=True)
    print(f'{len(passed)} secret scenarios passed', flush=True)


COOKIE_PLAYBOOK = '''fleet_stages:
  - id: cookie
    nodes: a
    node_tasks:
      - role: cookie
        inputs:
          cookie_secret: "{{ .secrets.session_cookie_secret }}"
      - id: length
        shell:
          script: printf %s "$COOKIE_SECRET" | wc -c
          env:
            COOKIE_SECRET: "{{ .secrets.session_cookie_secret }}"
          changed_when: false
'''
COOKIE_ROLE = f'''inputs:
  cookie_secret: {{type: string, required: true, secret: true}}
node_tasks:
  - file:
      path: {BASE}/cookie-secret
      content: "{{{{ .inputs.cookie_secret }}}}"
      permissions_mode: "0600"
'''
LATER_PLAYBOOK = '''fleet_stages:
  - id: later
    nodes: a
    node_tasks:
      - shell:
          script: "true"
          env:
            TOKEN: "{{ .secrets.not_yet_defined }}"
'''


def secret_key_commands(workspace, mmx, env, remote, scan, mark, secret_values):
    """mmx secrets add-random, set, list-keys, diff-keys, and usage on a real
    project. A key that add-random made reaches a node task, whose result is
    hidden, set replaces it, and no output of the five commands holds a value."""
    assert shutil.which('git'), 'mmx secrets diff-keys needs the git program on the controller machine'
    shown = []

    def secrets(*args, expected=0, value=None):
        # value goes to standard input, as some_command | mmx secrets set KEY gives it.
        result = subprocess.run([str(mmx), 'secrets', *args], cwd=workspace, env=env, capture_output=True, text=True, timeout=60, input=value)
        shown.append(result.stdout + result.stderr)
        assert result.returncode == expected, (args, result.returncode, result.stdout, result.stderr)
        return result.stdout

    def git(*args):
        # The user's own Git settings, such as commit signing, play no part.
        subprocess.run(['git', *args], cwd=workspace, check=True, capture_output=True, timeout=60,
                       env=dict(os.environ, GIT_CONFIG_GLOBAL=os.devnull, GIT_CONFIG_SYSTEM=os.devnull, GIT_AUTHOR_NAME='MMX',
                                GIT_AUTHOR_EMAIL='mmx@example.com', GIT_COMMITTER_NAME='MMX', GIT_COMMITTER_EMAIL='mmx@example.com'))

    def shape(text):
        """Each line's indentation and words, whatever the width of the columns."""
        return [(len(line) - len(line.lstrip()), ' '.join(line.split())) for line in text.splitlines()]

    vault = workspace / 'secrets_vault.age'
    # The last commit holds the vault as it is: password_a and password_b.
    # The edit changes one value, removes one key, and adds one.
    git('init', '--quiet')
    git('add', 'secrets_vault.age')
    git('commit', '--quiet', '--message', 'The vault')
    env['MMX_TEST_VAULT_TEXT'] = '# Shop secrets\npassword_a: fixture-vault-a-rotated\nspare_key: fixture-spare-value\n'
    secret_values.extend(['fixture-vault-a-rotated', 'fixture-spare-value'])
    secrets('edit')

    assert secrets('add-random', 'session_cookie_secret', '--length', '48') == (
        'Added the key session_cookie_secret to the vault secrets_vault.age, with a random value of 48 letters and digits. '
        'MMX does not show the value.\n'), shown[-1]
    written = vault.read_bytes()
    # The project reads password_b from an environment variable: a value in the vault would never be read.
    assert secrets('add-random', 'password_b', expected=4) == '' and 'secret key password_b is bound to environment variable MMX_SECRET_B' in shown[-1], shown[-1]
    assert secrets('list-keys') == 'password_a\nsession_cookie_secret\nspare_key\n'
    assert secrets('diff-keys') == ('Vault: secrets_vault.age, compared with the last commit\n\n'
                                    'Added:    session_cookie_secret, spare_key\nRemoved:  password_b\nChanged:  password_a\n')

    (workspace / 'roles/cookie').mkdir(parents=True, exist_ok=True)
    (workspace / 'roles/cookie/main.yml').write_text(COOKIE_ROLE)
    (workspace / 'playbooks/cookie.yml').write_text(COOKIE_PLAYBOOK)
    (workspace / 'playbooks/later.yml').write_text(LATER_PLAYBOOK)
    assert shape(secrets('usage', 'session_cookie_secret')) == [
        (0, 'session_cookie_secret vault secrets_vault.age'),
        (2, 'playbooks/cookie.yml:7 role cookie, input cookie_secret'),
        (4, f'roles/cookie/main.yml:6 file {BASE}/cookie-secret (content)'),
        (2, 'playbooks/cookie.yml:12 shell env COOKIE_SECRET')], shown[-1]
    # A referenced key that is not defined makes the exit code 1, for CI.
    everything = shape(secrets('usage', expected=1))
    for line in [(0, 'password_a vault secrets_vault.age'), (4, f'roles/app/main.yml:1 file {BASE}/private.conf (template)'),
                 (4, 'roles/app/main.yml:1 shell env TOKEN'), (0, 'password_b environment variable MMX_SECRET_B'),
                 (0, 'spare_key vault secrets_vault.age'), (2, '(never referenced)'), (0, 'not_yet_defined not defined')]:
        assert line in everything, (line, shown[-1])
    assert everything[-2:] == [(0, 'Referenced but not defined: not_yet_defined (playbooks/later.yml:8)'),
                               (0, 'Defined but never referenced: spare_key')], shown[-1]
    usage = json.loads(secrets('usage', '--json', expected=1))
    by_key = {entry['key']: entry for entry in usage['keys']}
    assert usage['schema_version'] == 1 and sorted(by_key) == ['not_yet_defined', 'password_a', 'password_b', 'session_cookie_secret', 'spare_key'], usage
    assert usage['referenced_but_not_defined'] == ['not_yet_defined'] and usage['defined_but_never_referenced'] == ['spare_key'], usage
    assert by_key['password_b']['source'] == {'env_var': 'MMX_SECRET_B'} and by_key['not_yet_defined']['source'] is None, usage
    cookie = by_key['session_cookie_secret']
    assert Path(cookie['source']['vault']).samefile(vault) and [use['used_by'] for use in cookie['uses']] == [
        'role cookie, input cookie_secret', 'shell env COOKIE_SECRET'], cookie
    assert cookie['uses'][0]['uses_inside_the_role'] == [
        {'file': 'roles/cookie/main.yml', 'line': 6, 'used_by': f'file {BASE}/cookie-secret (content)'}], cookie
    (workspace / 'playbooks/later.yml').unlink()
    assert vault.read_bytes() == written, 'a command that only reads wrote the vault'

    # The generated value reaches the node tasks. The suite learns it only there.
    result = subprocess.run([str(mmx), 'run', 'cookie', '--json'], cwd=workspace, env=env, capture_output=True, text=True, timeout=180)
    shown.append(result.stdout + result.stderr)
    assert result.returncode == 0, (result.returncode, result.stdout, result.stderr)
    generated = remote('a', f'cat {BASE}/cookie-secret')
    assert re.fullmatch('[A-Za-z0-9]{48}', generated), 'the generated value is not 48 letters and digits'
    assert remote('a', f'stat -c %a {BASE}/cookie-secret').strip() == '600'
    secret_values.append(generated)
    results = {r['task']: r for r in json.loads(result.stdout)['entries'][0]['response']['results']}
    written_file, length = results['cookie/task_1'], results['length']
    assert written_file['change'] == 'changed' and written_file['secret'] and not written_file.get('file_change', {}).get('diff'), written_file
    assert length['secret'] and length['stdout'] == '[redacted]', length
    for text in shown:
        assert all(value not in text for value in secret_values), 'a secret value appeared in the output of a command'
    scan()
    shutil.rmtree(workspace / '.git')
    mark('add-random makes a key that reaches a node task with a hidden result; list-keys, diff-keys against the last '
         'Git commit, and usage name keys and places, never a value')

    # mmx secrets set takes a value from a pipe, without its final line ending, and replaces a key only with
    # --replace; the next run delivers the new value. It prints nothing.
    secret_values.extend(['fixture-set-from-a-pipe', 'fixture-set-at-the-prompt'])
    assert secrets('set', 'session_cookie_secret', value='fixture-set-from-a-pipe\n', expected=4) == '', shown[-1]
    assert 'the vault already has the key session_cookie_secret; to replace its value, add --replace' in shown[-1], shown[-1]
    assert secrets('set', 'session_cookie_secret', '--replace', value='fixture-set-from-a-pipe\n') == '' and shown[-1] == '', shown[-1]
    result = subprocess.run([str(mmx), 'run', 'cookie', '--json'], cwd=workspace, env=env, capture_output=True, text=True, timeout=180)
    shown.append(result.stdout + result.stderr)
    assert result.returncode == 0, (result.returncode, result.stdout, result.stderr)
    assert remote('a', f'cat {BASE}/cookie-secret') == 'fixture-set-from-a-pipe'
    # On a terminal, set asks for the value with a hidden prompt; --non-interactive refuses to ask.
    for args, typed, code in [(['set', 'typed_token'], b'fixture-set-at-the-prompt\n', 0), (['set', 'other_token', '--non-interactive'], None, 4)]:
        pid, terminal = pty.fork()
        if pid == 0:
            os.chdir(workspace)
            os.execve(str(mmx), [str(mmx), 'secrets', *args], env)
        output, sent, status = b'', False, None
        deadline = time.monotonic() + 45
        try:
            while status is None and time.monotonic() < deadline:
                if select.select([terminal], [], [], .1)[0]:
                    try:
                        output += os.read(terminal, 65536)
                    except OSError:
                        pass
                if typed and not sent and b'Value of typed_token: ' in output and not termios.tcgetattr(terminal)[3] & termios.ECHO:
                    os.write(terminal, typed)
                    sent = True
                child, waited = os.waitpid(pid, os.WNOHANG)
                if child:
                    status = os.waitstatus_to_exitcode(waited)
            # What the command wrote just before it exited.
            while select.select([terminal], [], [], .1)[0]:
                try:
                    chunk = os.read(terminal, 65536)
                except OSError:
                    break
                if not chunk:
                    break
                output += chunk
        finally:
            if status is None:
                os.kill(pid, signal.SIGKILL)
                os.waitpid(pid, 0)
            os.close(terminal)
        shown.append(output.decode(errors='replace'))
        assert status == code and (sent or not typed), (args, status, shown[-1])
    assert 'standard input is a terminal, and --non-interactive turns off the prompt for the value' in shown[-1], shown[-1]
    assert secrets('list-keys') == 'password_a\nsession_cookie_secret\nspare_key\ntyped_token\n', shown[-1]
    for text in shown:
        assert all(value not in text for value in secret_values), 'a secret value appeared in the output of a command'
    scan()
    mark('set puts a value from a pipe or a hidden prompt into the vault, replaces one only with --replace, and the '
         'next run delivers it')


TOKEN = BASE + '/server-token'


def secrets_that_start_on_a_node(workspace, mmx, env, connections, remote, scan, mark, secret_values):
    """A join token read with read_file on one node reaches a role input with
    secret: true on the other, through the controller process's memory only.
    mmx resume reads the file again; a shell task's secret blocks its consumers."""
    (workspace / 'roles/cluster_server').mkdir(parents=True, exist_ok=True)
    (workspace / 'roles/cluster_server/main.yml').write_text(json.dumps({
        'inputs': {'join_token': {'type': 'string', 'required': True, 'secret': True}},
        'node_tasks': [
            {'shell': {'script': f'test ! -e {BASE}/fail-join', 'dryrun': 'true', 'changed_when': False}},
            {'file': {'path': BASE + '/joined-token', 'content': '{{ .inputs.join_token }}', 'permissions_mode': '0600'}}]}))

    def mmx_command(command, playbook, expected):
        result = subprocess.run([str(mmx), command, *vm.playbook_args(command, playbook), '--json'], cwd=workspace, env=env,
                                capture_output=True, text=True, timeout=180)
        for value in secret_values:
            assert value not in result.stdout + result.stderr, 'secret appeared in CLI output'
        assert result.returncode == expected, (command, result.returncode, result.stdout, result.stderr)
        return json.loads(result.stdout)

    def entries(record):
        return {(e['fleet_stage'], e['node']): e['response'] for e in record['entries']}

    def sample(node, patterns):
        """Record every process command line on node that holds a pattern."""
        vm.cmd([*connections[node], 'sudo -n /bin/sh -c ' + shlex.quote(f'cat > {BASE}/patterns')], input='\n'.join(patterns) + '\n')
        remote(node, f"""rm -f {BASE}/stop-sampler; : > {BASE}/command-lines; : > {BASE}/sampled
cat > {BASE}/sampler <<'SAMPLER'
while test ! -e {BASE}/stop-sampler; do
  cat /proc/[0-9]*/cmdline 2>/dev/null | tr '\\000' '\\n' > {BASE}/all-command-lines
  grep -a -c mmx-runner {BASE}/all-command-lines >> {BASE}/sampled
  grep -a -F -f {BASE}/patterns {BASE}/all-command-lines >> {BASE}/command-lines
done
SAMPLER
nohup sh {BASE}/sampler >/dev/null 2>&1 &""")

    def sampled(node):
        remote(node, f'touch {BASE}/stop-sampler; sleep 1')
        assert remote(node, f'grep -c -v "^0$" {BASE}/sampled || true').strip() not in ('', '0'), 'the sampler never saw MMX running'
        assert remote(node, f'cat {BASE}/command-lines') == '', 'a secret appeared on a process command line'

    class ControllerSampler(threading.Thread):
        """Every process command line on the controller machine during a run."""
        def __init__(self, patterns):
            super().__init__(daemon=True)
            self.patterns, self.found, self.done = patterns, [], threading.Event()

        def run(self):
            while not self.done.is_set():
                for path in Path('/proc').glob('[0-9]*/cmdline'):
                    try:
                        text = path.read_bytes().decode(errors='replace')
                    except OSError:
                        continue
                    self.found += [p for p in self.patterns if p in text]
                time.sleep(.05)

    def cluster(first, other):
        """The design's example: server01 is first, the other servers join it."""
        vm.write_playbook(workspace, 'cluster', [
            {'id': 'start_first_server', 'nodes': first, 'node_tasks': [
                {'shell': {'script': f'echo started >> {BASE}/first-server-runs', 'dryrun': 'true'}},
                {'id': 'join_token', 'read_file': {'path': TOKEN, 'content_is_secret': True}}],
             'outputs': {'join_token': '{{ .node_tasks.join_token.content }}'},
             'save_on_controller_machine': [{'output': 'join_token', 'path': 'credentials/{{ .node.name }}.token', 'permissions_mode': '0600'},
                 {'output': 'join_token', 'path': 'credentials/{{ .node.name }}.group-token', 'permissions_mode': '0640', 'secret_readable_by_group': True},
                 {'output': 'join_token', 'path': 'credentials/{{ .node.name }}.public-token', 'permissions_mode': '0644', 'dangerously_let_every_account_read_secret': True}]},
            {'id': 'join_other_servers', 'nodes': other, 'node_tasks': [
                {'role': 'cluster_server', 'inputs': {'join_token': '{{ .outputs.start_first_server.' + first + '.join_token }}'}}]}])

    # The first server is reached over HTTPS, then over SSH (node b is ssh_only).
    for first, other in [('a', 'b'), ('b', 'a')]:
        # The token appears on the node, as a program there would make it.
        remote(first, f'head -c 24 /dev/urandom | base64 > {TOKEN}; head -c 24 /dev/urandom | base64 > {TOKEN}.next; chmod 600 {TOKEN} {TOKEN}.next; rm -f {BASE}/first-server-runs')
        token, rotated = remote(first, f'cat {TOKEN}'), remote(first, f'cat {TOKEN}.next')
        secret_values.extend([token.strip(), rotated.strip()])
        remote(other, f'touch {BASE}/fail-join; rm -f {BASE}/joined-token')
        cluster(first, other)
        patterns = [token.strip(), rotated.strip()]
        for node in ['a', 'b']:
            sample(node, patterns)
        controller = ControllerSampler(patterns)
        controller.start()
        failed = mmx_command('run', 'cluster', 1)
        status = entries(failed)
        assert status['start_first_server', first]['status'] == 'succeeded' and status['join_other_servers', other]['status'] == 'failed', status
        saved = workspace / f'credentials/{first}.token'
        assert saved.read_text() == token and saved.stat().st_mode & 0o777 == 0o600
        for suffix, mode in [('group-token', 0o640), ('public-token', 0o644)]:
            acknowledged = workspace / f'credentials/{first}.{suffix}'
            assert acknowledged.read_text() == token and acknowledged.stat().st_mode & 0o777 == mode
        # The node generated a new token since: resume reads the file again.
        remote(first, f'mv {TOKEN}.next {TOKEN}')
        remote(other, f'rm {BASE}/fail-join')
        resumed = mmx_command('resume', 'cluster', 0)
        controller.done.set(); controller.join()
        for node in ['a', 'b']:
            sampled(node)
        assert not controller.found, 'a secret appeared on a process command line on the controller machine'
        assert resumed['id'] == failed['id'] and resumed['status'] == 'succeeded', resumed['status']
        assert remote(other, f'cat {BASE}/joined-token') == rotated
        assert remote(other, f'stat -c %a {BASE}/joined-token').strip() == '600'
        assert remote(first, f'wc -l < {BASE}/first-server-runs').strip() == '1', 'resume ran the producer again'
        results = subprocess.run([str(mmx), 'results'], cwd=workspace, env=env, capture_output=True, text=True, timeout=60)
        assert results.returncode == 0 and '"join_token":"[redacted]"' in results.stdout, results.stdout
        assert all(value not in results.stdout for value in patterns)
        scan()
        mark(f'a read_file secret from node {first} reaches a secret role input on node {other} and a 0600 file on the '
             'controller machine, plus explicitly acknowledged group and public files; never output, history, or a command line; resume reads it again')

    # The node cannot give the file again: its consumers are blocked with the reason.
    cluster('a', 'b')
    remote('b', f'touch {BASE}/fail-join')
    failed = mmx_command('run', 'cluster', 1)
    remote('a', f'rm {TOKEN}')
    remote('b', f'rm {BASE}/fail-join')
    blocked = entries(mmx_command('resume', 'cluster', 1))['join_other_servers', 'b']
    assert blocked['status'] == 'blocked' and f'could not read {TOKEN} on node a again' in blocked['error'], blocked
    mark('resume blocks the consumers of a read_file secret whose file is gone, naming the file and node')

    # A shell task's secret output cannot be read again.
    remote('a', f'head -c 24 /dev/urandom | base64 > {BASE}/seed')
    made = hashlib.sha256(remote('a', f'cat {BASE}/seed').encode()).hexdigest()[:32]
    secret_values.append(made)
    vm.write_playbook(workspace, 'token', [
        {'id': 'create_token', 'nodes': 'a', 'node_tasks': [
            {'id': 'new_token', 'shell': {'script': f'sha256sum {BASE}/seed | cut -c1-32', 'output_is_secret': True, 'changed_when': False}}],
         'outputs': {'token': '{{ .node_tasks.new_token.stdout }}'}},
        {'id': 'use_token', 'nodes': 'b', 'node_tasks': [
            {'shell': {'script': f'test ! -e {BASE}/fail-join', 'changed_when': False}},
            {'file': {'path': BASE + '/made-token', 'content': '{{ .outputs.create_token.a.token }}', 'permissions_mode': '0600'}}]}])
    remote('b', f'touch {BASE}/fail-join; rm -f {BASE}/made-token')
    failed = mmx_command('run', 'token', 1)
    result = failed['entries'][0]['response']['results'][0]
    assert result['task'] == 'new_token' and result['stdout'] == '[redacted]', result
    remote('b', f'rm {BASE}/fail-join')
    blocked = entries(mmx_command('resume', 'token', 1))['use_token', 'b']
    assert blocked['status'] == 'blocked' and 'node task new_token of fleet stage create_token on node a' in blocked['error'] and 'mmx run' in blocked['error'], blocked
    assert remote('b', f'test -e {BASE}/made-token && echo present || echo absent').strip() == 'absent'
    mmx_command('run', 'token', 0)
    assert remote('b', f'cat {BASE}/made-token') == made + '\n'
    scan()
    mark('a shell secret from output_is_secret reaches another node; on resume its consumers are blocked, naming the '
         'producing fleet stage and task, until a new mmx run')

    # A node task that reads the secret and fails, as a command that loads
    # it into a database can: MMX reports none of what the command printed,
    # and the reason names the file in which root reads it on the node.
    vm.write_playbook(workspace, 'load_token', [
        {'id': 'create_token', 'nodes': 'a', 'node_tasks': [
            {'id': 'new_token', 'shell': {'script': f'sha256sum {BASE}/seed | cut -c1-32', 'output_is_secret': True, 'changed_when': False}}],
         'outputs': {'token': '{{ .node_tasks.new_token.stdout }}'}},
        {'id': 'load_token', 'nodes': 'b', 'node_tasks': [
            {'id': 'insert_rows', 'retry': {'max_tries': 1}, 'shell': {
                'script': 'echo "ERROR:  invalid input syntax: $TOKEN" >&2; echo "COPY 0"; exit 1', 'env': {'TOKEN': '{{ .outputs.create_token.a.token }}'}}}]}])
    failed = entries(mmx_command('run', 'load_token', 1))['load_token', 'b']
    result = failed['results'][0]
    kept = result.get('hidden_details_file', '')
    assert re.fullmatch(r'/run/mmx/private/[0-9a-f]+-hidden-details-of-failed-node-tasks\.txt', kept) and result['stderr'] == '[redacted]', result
    assert result['reason'] == f'details suppressed for a task using secrets; root can read them on the node in {kept} for 72 hours, or until the node reboots', result
    assert kept in failed['error'], failed
    assert remote('b', f'stat -c "%U %a" {kept}; stat -c "%U %a" /run/mmx/private') == 'root 600\nroot 700\n'
    details = remote('b', f'cat {kept}')
    lines = details.split('\n')
    # The token is the producing task's standard output, with its line
    # ending, so the error line is followed by an empty line.
    assert lines[:3] == ['Node task: insert_rows', 'Status: failed', 'Exit code: 1'] and lines[3].startswith('Reason: ') and \
        lines[3].endswith('exit status 1') and \
        '\n'.join(lines[4:]) == f'Standard error:\nERROR:  invalid input syntax: {made}\n\nStandard output:\nCOPY 0\n\n', details
    scan()
    mark('a failed node task that reads a secret reports none of its output; its reason names the file, readable only by root, '
         'in which the node keeps the reason and the output')


    # The checker promises to print no secrets: useful errors stay visible,
    # with an exact secret value replaced even when the template read it.
    (workspace / 'files').mkdir(exist_ok=True)
    (workspace / 'files/checked-settings.tmpl').write_text('{{ .secrets.password_b }}')
    check = 'printf "settings typo: " >&2; cat "$NEW_FILE" >&2; exit 1'
    vm.write_playbook(workspace, 'check_secret_settings', [
        {'id': 'settings', 'nodes': 'a', 'node_tasks': [
            {'id': 'checked_settings', 'file': {'path': BASE + '/checked-settings',
                'template': 'files/checked-settings.tmpl', 'permissions_mode': '0600',
                'check_new_file': {'shell': {'script': check, 'dryrun': check, 'env': {'NEW_FILE': '{{ .new_file.path }}'}, 'output_holds_no_secret': True}}}}]}])
    for command in ['run', 'dryrun']:
        record = mmx_command(command, 'check_secret_settings', 1)
        response = record['entries'][0]['response']
        result = response['results'][0]
        assert result['secret'] and 'check_new_file failed: ' in result['reason'], result
        assert 'settings typo: [redacted]' in result['reason'] and 'settings typo: [redacted]' in response['error'], response
        assert result['check_new_file']['stderr'] == 'settings typo: [redacted]', result
        assert not result['file_change'].get('diff') and not result['file_change'].get('full_diff'), result
    scan()
    mark('a run and a dryrun show a promised file check error with the template secret replaced; the file difference stays hidden')


if __name__ == '__main__':
    vm.run_suite(main)
