#!/usr/bin/env python3
"""http requests that write to an application's API through the public CLI, checked against what a small API server on the node received."""
import json
import shlex
import shutil

import vm

BASE = '/var/tmp/mmx-http-requests'
API = 'mmx-http-requests-api.service'
TLS_API = 'mmx-http-requests-tls.service'
TOKEN = 'mmx-http-token-sentinel'
PASSWORD = 'mmx-http-password-sentinel'

# A small account API: GET /api/users/<name> answers 200 or 404; POST
# /api/users creates an account with basic authentication; PUT and DELETE
# /api/users/<name> change and remove one. Every request is logged.
SERVER = r'''
import base64, json, ssl, sys, time
from http.server import BaseHTTPRequestHandler, HTTPServer

base, port = sys.argv[1], int(sys.argv[2])
users = {}

class API(BaseHTTPRequestHandler):
    def answer(self, status, document=None):
        body = json.dumps(document).encode() if document is not None else b''
        self.send_response(status)
        self.send_header('Content-Type', 'application/json')
        self.send_header('Content-Length', str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def received(self):
        length = int(self.headers.get('Content-Length') or 0)
        body = self.rfile.read(length).decode()
        with open(base + '/requests.log', 'a') as log:
            log.write(json.dumps({'method': self.command, 'path': self.path, 'body': body,
                                  'content_type': self.headers.get('Content-Type'),
                                  'token': self.headers.get('X-Token'),
                                  'authorization': self.headers.get('Authorization')}) + '\n')
        return body

    def do_GET(self):
        self.received()
        if self.path == '/slow':
            time.sleep(11)
            return self.answer(200, {'ready': True})
        name = self.path.rsplit('/', 1)[-1]
        if self.path.startswith('/api/users/') and name in users:
            self.answer(200, users[name])
        else:
            self.answer(404, {'message': 'not found'})

    def do_POST(self):
        body = self.received()
        expected = 'Basic ' + base64.b64encode(b'root:' + open(base + '/password').read().strip().encode()).decode()
        if self.headers.get('Authorization') != expected:
            return self.answer(401, {'message': 'unauthorized'})
        account = json.loads(body)
        users[account['username']] = account
        self.answer(201, account)

    def do_PUT(self):
        body = self.received()
        name = self.path.rsplit('/', 1)[-1]
        changed = users.get(name) != json.loads(body)
        users[name] = json.loads(body)
        self.answer(200, {'changed': changed})

    def do_DELETE(self):
        self.received()
        self.answer(204 if users.pop(self.path.rsplit('/', 1)[-1], None) else 404)

    def log_message(self, *args):
        pass

server = HTTPServer(('127.0.0.1', port), API)
if len(sys.argv) > 3:
    context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
    context.load_cert_chain(sys.argv[3], sys.argv[4])
    server.socket = context.wrap_socket(server.socket, server_side=True)
server.serve_forever()
'''


def main():
    vm.up()
    node, ssh = vm.connection()
    vm.reset()
    workspace = vm.ROOT / 'http-requests'
    if workspace.exists():
        shutil.rmtree(workspace)
    vm.write_inventory(workspace, {'nodes': {'app': node}}, name='example')
    vm.build()
    mmx = vm.ROOT / 'bin/mmx'
    passed = []

    def remote(script):
        return vm.cmd([*ssh, 'sudo -n /bin/sh -c ' + shlex.quote('set -eu\n' + script)]).stdout.strip()

    def project(tasks, account_name='alice'):
        vm.write_project(workspace, project_name='http_requests', secrets_from_env={'api_token': 'MMX_TEST_API_TOKEN', 'root_password': 'MMX_TEST_ROOT_PASSWORD'},
                         vars={'account_name': account_name}, fleet_stages=[{'id': 'configure', 'nodes': 'all', 'node_tasks': tasks}])

    def run(command, expected=None, *args):
        result = vm.cmd([mmx, command, *vm.playbook_args(command), '--json', *args], cwd=workspace, check=False)
        assert result.returncode in (vm.success_codes(command) if expected is None else (expected,)), (command, result.stdout, result.stderr)
        return json.loads(result.stdout)

    def results(record):
        return {r['task'].rsplit('/', 1)[-1]: r for r in record['entries'][0]['response']['results']}

    def requests():
        text = remote(f'cat {BASE}/requests.log 2>/dev/null || true')
        return [json.loads(line) for line in text.splitlines()]

    def mark(message):
        passed.append(message)
        print('PASS ' + message, flush=True)

    def cleanup():
        remote(f'systemctl stop {API} {TLS_API} 2>/dev/null || true\n'
               f'systemctl reset-failed {API} {TLS_API} 2>/dev/null || true\nrm -rf {BASE}')

    cleanup()
    vm.ENV['MMX_TEST_API_TOKEN'] = TOKEN
    vm.ENV['MMX_TEST_ROOT_PASSWORD'] = PASSWORD
    try:
        remote(f'mkdir -p {BASE}\ncat > {BASE}/api.py <<\'EOF\'\n{SERVER}\nEOF\n'
               f'printf %s {shlex.quote(PASSWORD)} > {BASE}/password\n'
               f'systemd-run --unit {API} python3 {BASE}/api.py {BASE} 18093\n'
               'for i in $(seq 50); do python3 -c "import socket; socket.create_connection((\'127.0.0.1\', 18093))" 2>/dev/null && break; sleep 0.2; done')

        # The design's example: ask first, and create the account only when
        # the application does not have it.
        account = [
            {'id': 'existing_account', 'http': {'url': 'http://127.0.0.1:18093/api/users/{{ .vars.account_name }}',
                                                'expected_http_status_codes': [200, 404]}},
            {'id': 'create_account', 'when': {'equals': ['{{ .node_tasks.existing_account.http_status_code }}', 404]},
             'http': {'method': 'POST', 'url': 'http://127.0.0.1:18093/api/users',
                      'basic_auth_username': 'root', 'basic_auth_password': '{{ .secrets.root_password }}',
                      'json_body': {'username': '{{ .vars.account_name }}', 'must_change_password': False},
                      'expected_http_status_codes': [201]}},
        ]
        project(account)
        preview = results(run('dryrun'))
        assert preview['existing_account']['http_status_code'] == 404 and preview['create_account']['change'] == 'would_change', preview
        assert [r['method'] for r in requests()] == ['GET'], requests()
        created = results(run('run'))
        # The request reads a secret password, so its result is suppressed.
        assert created['create_account']['change'] == 'changed' and created['create_account']['secret'], created
        assert 'http_status_code' not in created['create_account'], created
        post = requests()[-1]
        assert post['method'] == 'POST' and post['content_type'] == 'application/json', post
        assert json.loads(post['body']) == {'username': 'alice', 'must_change_password': False}, post
        again = results(run('run'))
        assert again['create_account']['status'] == 'skipped' and again['existing_account']['http_status_code'] == 200, again
        assert [r['method'] for r in requests()].count('POST') == 1
        mark('http asks first and creates an account only when the application does not have it; a dryrun sends only the GET')

        # A wrong password: the API answers 401, which the task does not
        # accept; the password stays out of the results.
        vm.ENV['MMX_TEST_ROOT_PASSWORD'] = 'wrong'
        project(account, 'bob')
        refused = run('run', 1)
        assert results(refused)['create_account']['status'] == 'failed' and results(refused)['create_account']['secret'], refused
        vm.ENV['MMX_TEST_ROOT_PASSWORD'] = PASSWORD

        # A PUT with a secret token in a header; changed_when reads the answer.
        update = [{'id': 'update_account', 'http': {
            'method': 'PUT', 'url': 'http://127.0.0.1:18093/api/users/alice',
            'headers': {'X-Token': '{{ .secrets.api_token }}'},
            'json_body': {'username': 'alice', 'shell': '/bin/sh'},
            'changed_when': {'equals': ['{{ .this_node_task.response_body_json.changed }}', True]}}}]
        project(update)
        assert results(run('dryrun'))['update_account']['status'] == 'not_checked'
        assert requests()[-1]['method'] != 'PUT'
        first = run('run')
        assert results(first)['update_account']['change'] == 'changed' and results(first)['update_account']['secret'], first
        assert requests()[-1]['token'] == TOKEN
        assert results(run('run'))['update_account']['change'] == 'unchanged'
        for saved in [json.dumps(first), *[p.read_bytes().decode(errors='replace') for p in (workspace / '.mmx').rglob('*') if p.is_file()]]:
            assert TOKEN not in saved and PASSWORD not in saved
        mark('http PUT with a secret header: changed_when reads the answer, the result is secret, and no token or password is saved')

        # DELETE: changed by default; another run gets 404, which it accepts.
        project([{'id': 'remove_account', 'http': {'method': 'DELETE', 'url': 'http://127.0.0.1:18093/api/users/alice',
                                                   'expected_http_status_codes': [204, 404],
                                                   'changed_when': {'equals': ['{{ .this_node_task.http_status_code }}', 204]}}}])
        assert results(run('run'))['remove_account']['change'] == 'changed'
        assert results(run('run'))['remove_account']['change'] == 'unchanged'
        mark('http DELETE accepts the answer of an account that is already gone and reports it unchanged')

        # HTTPS with the application's own self-signed certificate.
        remote(f'openssl req -x509 -newkey rsa:2048 -nodes -days 1 -subj /CN=127.0.0.1 -addext subjectAltName=IP:127.0.0.1 '
               f'-keyout {BASE}/key.pem -out {BASE}/certificate.pem 2>/dev/null\n'
               f'systemd-run --unit {TLS_API} python3 {BASE}/api.py {BASE} 18094 {BASE}/certificate.pem {BASE}/key.pem\n'
               'for i in $(seq 50); do python3 -c "import socket; socket.create_connection((\'127.0.0.1\', 18094))" 2>/dev/null && break; sleep 0.2; done')
        project([{'id': 'tls', 'http': {'url': 'https://127.0.0.1:18094/api/users/alice', 'expected_http_status_codes': [404]}}])
        untrusted = run('run', 1)
        assert 'certificate' in results(untrusted)['tls']['reason'], untrusted
        project([{'id': 'tls', 'http': {'url': 'https://127.0.0.1:18094/api/users/alice', 'expected_http_status_codes': [404],
                                        'trusted_certificate_authority_file': f'{BASE}/certificate.pem'}}])
        assert results(run('run'))['tls']['http_status_code'] == 404
        mark('http over HTTPS fails on a self-signed certificate and succeeds with it in trusted_certificate_authority_file')

        project([{'id': 'tls', 'http': {'url': 'https://127.0.0.1:18094/api/users/alice',
                                        'expected_http_status_codes': [404],
                                        'dangerously_skip_server_certificate_check': True}},
                 {'id': 'ready', 'wait_for': {'url_responds': 'https://127.0.0.1:18094/api/users/alice',
                                             'expected_http_status_codes': [404],
                                             'trusted_certificate_authority_file': f'{BASE}/certificate.pem'}},
                 {'id': 'unchecked_ready', 'wait_for': {'url_responds': 'https://127.0.0.1:18094/api/users/alice',
                                                       'expected_http_status_codes': [404],
                                                       'dangerously_skip_server_certificate_check': True}}])
        checked = results(run('run'))
        assert all(checked[key]['http_status_code'] == 404 for key in ['tls', 'ready', 'unchecked_ready']), checked
        mark('http and wait_for support an explicit certificate-check override; wait_for also trusts a named private CA')

        project([{'id': 'slow_request', 'time_limit_per_try': '15s', 'time_limit': '20s',
                  'http': {'url': 'http://127.0.0.1:18093/slow'}}])
        assert results(run('run'))['slow_request']['http_status_code'] == 200
        mark('http waits beyond ten seconds when the task time limits allow it')

        # A write in checks_before_restart_or_reload is refused when the
        # project loads.
        project([{'task_block': {'id': 'app', 'changes': [{'file': {"permissions_mode": "0644", 'path': f'{BASE}/config', 'content': 'x'}}],
                                 'checks_before_restart_or_reload': [{'http': {'method': 'POST', 'url': 'http://127.0.0.1:18093/api/users'}}],
                                 'activate': [{'id': 'reload', 'shell': {'script': 'true', 'only_when_tracked_inputs_changed': True}}]}}])
        refused = vm.cmd([mmx, 'validate', 'configure', '--json'], cwd=workspace, check=False)
        assert refused.returncode != 0 and 'must read: method GET or HEAD' in refused.stdout + refused.stderr, refused
        mark('validate refuses an http write in checks_before_restart_or_reload')
    finally:
        vm.ENV.pop('MMX_TEST_API_TOKEN', None)
        vm.ENV.pop('MMX_TEST_ROOT_PASSWORD', None)
        cleanup()
    (workspace / 'summary.json').write_text(json.dumps(passed, indent=2) + '\n')
    print(f'{len(passed)} http request workflows passed', flush=True)


if __name__ == '__main__':
    vm.run_suite(main)
