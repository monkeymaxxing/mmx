#!/usr/bin/env python3
"""Default DNS, HTTPS trust, named accounts, and matching controller/node versions."""
import json
import shlex
import shutil

import vm


# A released Go module's go.mod is immutable. Measured with curl: HTTP 200.
URL = 'https://proxy.golang.org/golang.org/x/text/@v/v0.42.0.mod'
SHA256 = '4756c4b8eeeae2a20cb43c198c10701d20c53493bd90b91c6632153c9cbac240'
CONTENT = ('module golang.org/x/text\n\ngo 1.26.0\n\n'
           'require golang.org/x/tools v0.49.0 // tagx:ignore\n\nrequire (\n'
           '\tgolang.org/x/mod v0.41.0 // indirect; tagx:ignore\n'
           '\tgolang.org/x/sync v0.23.0 // indirect\n)\n')
CREATED_ACCOUNT = 'mmx-account-lookup'
DOWNLOADED_FILE = '/var/tmp/mmx-dns-tls-download.mod'
CREATED_ACCOUNT_FILE = '/var/tmp/mmx-created-account-lookup'


def main():
    vm.up()
    vm.reset()
    vm.build()
    node, ssh = vm.connection()
    workspace = vm.ROOT / 'dns-tls-and-account-lookups'
    shutil.rmtree(workspace, ignore_errors=True)
    vm.write_inventory(workspace, {'nodes': {'server': node}})
    mmx = vm.ROOT / 'bin/mmx'

    def remote(script):
        return vm.cmd([*ssh, 'sudo -n /bin/sh -c ' + shlex.quote('set -eu\n' + script)]).stdout

    def cleanup():
        remote(f'rm -f {DOWNLOADED_FILE} {CREATED_ACCOUNT_FILE}\n'
               f'if getent passwd {CREATED_ACCOUNT} >/dev/null; then userdel {CREATED_ACCOUNT}; fi')

    cleanup()
    try:
        vm.write_project(workspace, project_name='dns_tls_and_account_lookups', fleet_stages=[{
            'id': 'check_clean_system', 'nodes': 'all', 'node_tasks': [
                {'id': 'create_account', 'user': {'name': CREATED_ACCOUNT, 'system': True,
                                                'primary_group': 'root', 'create_home_directory': False}},
                {'id': 'node_https', 'http': {'url': URL, 'expected_http_status_codes': [200]}},
                {'id': 'controller_https', 'download': {'url': URL, 'sha256': SHA256,
                                                        'path': DOWNLOADED_FILE, 'owner': 'nobody',
                                                        'group': 'root', 'permissions_mode': '0644'}},
                {'id': 'created_account_owner', 'file': {'path': CREATED_ACCOUNT_FILE,
                                                        'content': 'created account lookup\n',
                                                        'owner': CREATED_ACCOUNT, 'group': 'root',
                                                        'permissions_mode': '0644'}},
            ]}])
        record = json.loads(vm.cmd([mmx, 'run', 'configure', '--json'], cwd=workspace).stdout)
        entry = record['entries'][0]
        results = {result['task'].rsplit('/', 1)[-1]: result for result in entry['response']['results']}
        assert results['node_https']['http_status_code'] == 200, record
        assert results['node_https']['response_body'] == CONTENT, record
        assert remote(f'cat {DOWNLOADED_FILE}') == CONTENT
        print('PASS node http and controller download resolve a public host and use default HTTPS trust', flush=True)

        root_group_id = remote("getent group root | cut -d: -f3").strip()
        for path, account in ((DOWNLOADED_FILE, 'nobody'), (CREATED_ACCOUNT_FILE, CREATED_ACCOUNT)):
            account_id = remote(f'id -u {account}').strip()
            observed = remote(f"stat -c '%u:%g' {path}").strip()
            assert observed == f'{account_id}:{root_group_id}', (path, observed, account_id, root_group_id)
        assert results['create_account']['value']['account_created'], record
        print('PASS file ownership resolves an existing account and group and an account created in this run', flush=True)

        controller_version = vm.cmd([mmx, 'version']).stdout.split()[1]
        runner = '/opt/mmx/bin/' + entry['runner_hash'] + '/mmx-runner'
        node_version = json.loads(remote(shlex.quote(runner) + ' version'))['version']
        assert node_version == controller_version, (node_version, controller_version)
        print(f'PASS installed node runner and controller both report MMX {controller_version}', flush=True)
    finally:
        cleanup()


if __name__ == '__main__':
    vm.run_suite(main)
